package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// doctor is tested against a faked machine: a Linux VPS at Hetzner whose address is 203.0.113.7. The fake server
// of the client tests examines it from inside (fakeMachine.serverEnv), and the offline runs of the in-process
// tests get the same fakes through invocation.doctorEnv. No test here asks a resolver, a STUN server or a web
// server, or binds a service port; the test scripts, which run the real binary without the hook, only run checks
// that look at this machine.

// fakeNow is the clock of the faked server: a while after fakeStatus's start, and well before its certificate ends.
var fakeNow = time.Date(2026, 9, 29, 13, 12, 0, 0, time.UTC)

// fakeNet is the network of the faked machine: every STUN server sees it as 203.0.113.7, and the resolver knows
// the names in hosts.
type fakeNet struct {
	hosts map[string][]string
}

func (f fakeNet) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = netip.MustParseAddr(a)
	}
	return out, nil
}

func (fakeNet) Mapped(context.Context, net.PacketConn, string) (netip.AddrPort, error) {
	return netip.MustParseAddrPort("203.0.113.7:40000"), nil
}

func (fakeNet) Interfaces() ([]netx.Interface, error) {
	return []netx.Interface{{Name: "eth0", Flags: net.FlagUp, Addrs: []netx.InterfaceAddr{
		{Addr: netip.MustParseAddr("203.0.113.7")}, {Addr: netip.MustParseAddr("2001:db8::7")},
	}}}, nil
}

func (fakeNet) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.MustParseAddr("203.0.113.7"), nil
}

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeProbes puts fakes in place of everything of env that would ask the network or depend on the machine the
// tests run on. root is an empty directory that stands for the machine's file system in the container checks;
// rmemMax is the socket buffer limit the machine has.
func fakeProbes(env *doctor.Env, root, rmemMax string) {
	network := fakeNet{}
	now := env.Now
	if now == nil {
		now = time.Now
	}
	env.FS = fstest.MapFS{
		"proc/sys/kernel/osrelease":   {Data: []byte("6.8.0-45-generic\n")},
		"proc/sys/net/core/rmem_max":  {Data: []byte(rmemMax + "\n")},
		"proc/sys/net/core/wmem_max":  {Data: []byte("8388608\n")},
		"run/systemd/system/.keep":    {},
		"sys/class/dmi/id/sys_vendor": {Data: []byte("Hetzner\n")},
		"sys/class/net/eth0/speed":    {Data: []byte("1000\n")},
	}
	env.Resolver, env.STUN, env.Interfaces = network, network, network
	env.HTTP = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Date", now().UTC().Format(http.TimeFormat))
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})}
	env.Adjtimex = func() (bool, error) { return false, nil }
	env.GOOS, env.User = "linux", "isshoni"
	env.Host = config.Host{Environ: []string{}, Root: root}
	env.Bind = func(context.Context, string, string) error { return nil }
	env.CAA = func(context.Context, string) ([]doctor.CAARecord, error) { return nil, nil }
	env.DiskFree = func(string) (uint64, error) { return 37_200_000_000, nil }
	env.NoFile = func() (uint64, error) { return 65536, nil }
	// The faked machine has no user named isshoni, whatever users the machine of the tests has: an offline run
	// takes the caller for the service's user.
	env.LookupUser = func(string) (int, bool) { return 0, false }
}

// fakeMachine is the machine of a fake server: a data directory with its secrets, and what doctor finds there.
type fakeMachine struct {
	dir string // holds the data directory and the empty root
	cfg *config.Config

	mu    sync.Mutex
	state string // "ok", "warn" or "fail"
}

func newFakeMachine() (*fakeMachine, error) {
	dir, err := os.MkdirTemp("", "isshoni-doctor")
	if err != nil {
		return nil, err
	}
	m := &fakeMachine{dir: dir, state: "ok"}
	dataDir := filepath.Join(dir, "data")
	if err := errors.Join(os.Mkdir(dataDir, 0o700), os.Mkdir(filepath.Join(dir, "root"), 0o700)); err != nil {
		m.remove()
		return nil, err
	}
	if _, err := config.OpenSecrets(filepath.Join(dataDir, "secrets.json"), nil); err != nil {
		m.remove()
		return nil, err
	}
	m.cfg, err = config.LoadFlags(flag.NewFlagSet("fake", flag.ContinueOnError), []string{"--tls.mode", "ip", "--data-dir", dataDir})
	if err != nil {
		m.remove()
		return nil, err
	}
	return m, nil
}

func (m *fakeMachine) remove() { _ = os.RemoveAll(m.dir) }

func (m *fakeMachine) set(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = state
}

// serverEnv is ops.DoctorOptions.Env of the fake server: fakeStatus's server, seen from inside.
func (m *fakeMachine) serverEnv(context.Context) doctor.Env {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()

	live := fakeStatus()
	live.LocalIPv4, live.PublicIPv4Method = "203.0.113.7", "interface"
	live.Listeners = append(live.Listeners, api.ListenerInfo{Key: "listen.http", Network: "tcp", Addr: "[::]:80"})
	live.Advertised = append(live.Advertised, api.AdvertisedAddr{Proto: "tcp", Addr: "203.0.113.7:7882", Via: api.TransportTCP7882})
	live.UDPRcvBufBytes, live.UDPSndBufBytes = 8388608, 8388608
	live.Update = &api.UpdateInfo{Latest: live.Version}
	rmem := "8388608"
	switch state {
	case "warn":
		rmem = "212992"
	case "fail":
		// Three hours up and still no certificate: Let's Encrypt can't get through.
		live.TLS = api.TLSInfo{Mode: api.TLSModeIP, Names: []string{"203.0.113.7"}, LastErrorCode: "tls.acme_unreachable"}
	}
	env := doctor.Env{
		Config: m.cfg,
		Live:   &live,
		DB:     doctor.DBFiles{LatestSchemaVersion: func() int { return live.SchemaVersion }},
		Now:    func() time.Time { return fakeNow },
		UID:    os.Getuid(),
	}
	if state == "fail" {
		// The server's own sentence for that error (tlsmgr.Status.LastError), which the wiring passes along.
		env.TLSLastError = "Let's Encrypt couldn't connect to 203.0.113.7 on port 80 or 443. Open TCP 80 and 443 in your cloud firewall."
	}
	fakeProbes(&env, filepath.Join(m.dir, "root"), rmem)
	return env
}

// doctorRun is one run of `isshoni doctor` inside the test process.
type doctorRun struct {
	code           int
	stdout, stderr string
	offline        int // how often the checks ran in this process
}

// runDoctorCLI runs the CLI in-process like runCLITTY, with the faked machine for an offline run: hook may bend
// the Env after the fakes are in place. The data directory is a fresh one unless environ names another.
func runDoctorCLI(t *testing.T, tty bool, environ []string, hook func(*doctor.Env), args ...string) doctorRun {
	t.Helper()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{"ISSHONI_CONFIG=" + empty, "ISSHONI_LISTEN_ADMIN_SOCKET=" + socketPath(t)}
	if !slices.ContainsFunc(environ, func(kv string) bool { return strings.HasPrefix(kv, "ISSHONI_DATA_DIR=") }) {
		base = append(base, "ISSHONI_DATA_DIR="+filepath.Join(dir, "data"))
	}
	var out, errOut bytes.Buffer
	var res doctorRun
	inv := &invocation{
		stdout:  &out,
		stderr:  &errOut,
		environ: append(base, environ...),
		isTTY:   func(w io.Writer) bool { return tty && w == io.Writer(&out) },
		doctorEnv: func(env *doctor.Env) {
			res.offline++
			fakeProbes(env, root, "8388608")
			if hook != nil {
				hook(env)
			}
		},
	}
	if slices.Contains(environ, strangerEnv+"=1") {
		inv.dialAdmin = dialAsStranger
	}
	res.code = run(t.Context(), inv, args)
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

// --list-checks prints the ids of 04 §13.2 in table order, one per line, or as a JSON array; it needs neither a
// server nor a config that works.
func TestDoctorListChecks(t *testing.T) {
	want := []string{
		"config", "data_dir", "secrets", "schema", "public_ip", "dns", "tls", "clock", "udp_buffers", "ports",
		"firewall_hint", "container", "lan_privacy", "transfer", "release", "nofile", "bandwidth",
	}
	res := runDoctorCLI(t, false, nil, nil, "doctor", "--list-checks")
	if res.code != exitOK || res.stderr != "" || res.stdout != strings.Join(want, "\n")+"\n" {
		t.Errorf("--list-checks: exit %d, stderr %q, stdout:\n%s", res.code, res.stderr, res.stdout)
	}
	res = runDoctorCLI(t, false, nil, nil, "doctor", "--list-checks", "--json")
	var ids []string
	if err := json.Unmarshal([]byte(res.stdout), &ids); err != nil || !slices.Equal(ids, want) || res.code != exitOK ||
		strings.Count(res.stdout, "\n") != 1 || !strings.HasPrefix(res.stdout, `["config","data_dir",`) {
		t.Errorf("--list-checks --json: exit %d, stdout %q (%v)", res.code, res.stdout, err)
	}
	// With a config that has errors, and without running anything.
	res = runDoctorCLI(t, true, nil, nil, "doctor", "--tls.mode", "manual", "--list-checks")
	if res.code != exitOK || res.stdout != strings.Join(want, "\n")+"\n" || res.offline != 0 {
		t.Errorf("--list-checks with a broken config: exit %d, %d runs, stdout:\n%s", res.code, res.offline, res.stdout)
	}
}

func TestDoctorUsageErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--only", "nope"}, `isshoni doctor: --only: no check "nope"; --list-checks prints the ids`},
		{[]string{"--only", "dns,public-ip"}, `isshoni doctor: --only: no check "public-ip" (did you mean "public_ip"?); --list-checks prints the ids`},
		{[]string{"--only", " , "}, `isshoni doctor: --only " , " names no check; --list-checks prints the ids`},
		{[]string{"--people", "0"}, "isshoni doctor: --people 0: want 1 to 1000"},
		{[]string{"--sharing", "6"}, "isshoni doctor: --sharing 6: want 0 to --people (5)"},
		{[]string{"--thumbnails", "-1"}, "isshoni doctor: --thumbnails -1: want 0 to 100"},
		{[]string{"--hours", "0"}, "isshoni doctor: --hours 0: want more than 0, up to 8784"},
		{[]string{"--quality", "720p30"}, `isshoni doctor: --quality "720p30": want 1080p60, 1440p60 or 2160p60`},
		{[]string{"--preset", "game"}, `isshoni doctor: --preset "game": want auto or movie`},
		{[]string{"extra"}, `isshoni doctor: unexpected argument "extra"`},
	}
	for _, tt := range tests {
		res := runDoctorCLI(t, false, nil, nil, append([]string{"doctor"}, tt.args...)...)
		first, _, _ := strings.Cut(res.stderr, "\n")
		if res.code != exitUsage || first != tt.want || res.stdout != "" || res.offline != 0 {
			t.Errorf("doctor %v: exit %d, %d runs, stdout %q, stderr %q; want exit 2 and %q", tt.args, res.code, res.offline, res.stdout, first, tt.want)
		}
	}
}

// Without a server doctor runs its checks here (04 §13.1): everything this machine can answer, with the checks
// that need a live server skipped.
func TestDoctorOffline(t *testing.T) {
	res := runDoctorCLI(t, false, nil, nil, "doctor", "--tls.mode", "ip")
	if res.code != exitOK || res.stderr != "" || res.offline != 1 {
		t.Fatalf("exit %d, %d offline runs, stderr %q, stdout:\n%s", res.code, res.offline, res.stderr, res.stdout)
	}
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "isshoni doctor · "+version.Version()+" · server not running · ") || !strings.HasSuffix(lines[0], " UTC") {
		t.Errorf("header %q", lines[0])
	}
	for _, want := range []string{
		"[ ok ] config        ",
		"[info] data_dir      ", // not created yet: this server never started
		"[ ok ] public_ip     203.0.113.7 (on interface eth0, confirmed by STUN)",
		"[ ok ] dns           no domain: the site is this server's IP address",
		"[skip] tls           not checked: isshoni is not running",
		"[ ok ] clock         synchronized, within 1 second of acme-v02.api.letsencrypt.org",
		"[info] ports         443/tcp, 80/tcp, 7882/udp and 7882/tcp are free (local check only)",
		"       This only checks this machine. The browser connection test checks from outside.",
		"[info] firewall_hint Hetzner: Cloud Console → Firewalls → allow TCP 80, 443, 7882 and UDP 7882",
		"       https://moonwx.github.io/isshoni/install/vps#hetzner",
		"[skip] transfer      not checked: isshoni is not running",
		"[skip] release       not checked: isshoni is not running",
		"[info] bandwidth     5 people, 2 sharing: ~41.5 Mbps egress (~44 on the wire), ~39 GB per 2-hour session",
	} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("no line starts with %q in:\n%s", want, res.stdout)
		}
	}
	if last := lines[len(lines)-1]; last != "7 ok · 0 warn · 0 fail · 3 skipped" {
		t.Errorf("summary %q", last)
	}
	if strings.Contains(res.stdout, "\x1b[") {
		t.Error("colors off a terminal")
	}

	// --json: the one document of 04 §13.5 on stdout, and nothing else anywhere.
	res = runDoctorCLI(t, false, nil, nil, "doctor", "--tls.mode", "ip", "--json")
	var rep api.DoctorReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("--json: %v\n%s", err, res.stdout)
	}
	if res.code != exitOK || res.stderr != "" || strings.Count(res.stdout, "\n") != 1 {
		t.Errorf("--json: exit %d, stderr %q, %d lines", res.code, res.stderr, strings.Count(res.stdout, "\n"))
	}
	if rep.Schema != 1 || rep.Mode != api.DoctorModeCLIOffline || rep.Version != version.Version() || len(rep.Checks) != 17 ||
		rep.Summary != (api.DoctorCounts{OK: 7, Skip: 3, Info: 7}) || rep.Bandwidth == nil || rep.Env.Provider != api.CloudProviderHetzner {
		t.Errorf("--json report: mode %s, %d checks, summary %+v, env %+v", rep.Mode, len(rep.Checks), rep.Summary, rep.Env)
	}
	if !strings.HasPrefix(res.stdout, `{"schema":1,"version":"`) || !strings.Contains(res.stdout, `"mode":"cli-offline"`) {
		t.Errorf("--json document: %.80s…", res.stdout)
	}

	// Colors on a terminal, unless NO_COLOR says no.
	res = runDoctorCLI(t, true, nil, nil, "doctor", "--tls.mode", "ip", "--only", "dns")
	if !strings.Contains(res.stdout, "\x1b[32m[ ok ]\x1b[0m dns") {
		t.Errorf("no colors on a terminal: %q", res.stdout)
	}
	for _, env := range []string{"NO_COLOR=1", "NO_COLOR=true"} {
		if res = runDoctorCLI(t, true, []string{env}, nil, "doctor", "--tls.mode", "ip", "--only", "dns"); strings.Contains(res.stdout, "\x1b[") {
			t.Errorf("%s: colors in %q", env, res.stdout)
		}
	}
	if res = runDoctorCLI(t, true, []string{"NO_COLOR="}, nil, "doctor", "--tls.mode", "ip", "--only", "dns"); !strings.Contains(res.stdout, "\x1b[32m") {
		t.Errorf("an empty NO_COLOR switched the colors off: %q", res.stdout)
	}
}

// install.sh's pre-check before the first start (06 §4.8): three checks and the firewall note, offline. A domain
// that points elsewhere fails it with exit 5, before Let's Encrypt is asked for anything.
func TestDoctorPreCheck(t *testing.T) {
	const only = "dns,public_ip,clock,firewall_hint"
	res := runDoctorCLI(t, false, nil, nil, "doctor", "--tls.mode", "ip", "--public-ip", "203.0.113.7", "--only", only)
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if res.code != exitOK || len(lines) != 7 || !strings.HasPrefix(lines[1], "[ ok ] public_ip     203.0.113.7 (set in the config)") ||
		!strings.HasPrefix(lines[2], "[ ok ] dns ") || !strings.HasPrefix(lines[3], "[ ok ] clock ") ||
		!strings.HasPrefix(lines[4], "[info] firewall_hint ") || lines[6] != "3 ok · 0 warn · 0 fail" {
		t.Errorf("pre-check in ip mode: exit %d\n%s", res.code, res.stdout)
	}

	dns := func(addrs ...string) func(*doctor.Env) {
		return func(env *doctor.Env) { env.Resolver = fakeNet{hosts: map[string][]string{"share.example.com": addrs}} }
	}
	res = runDoctorCLI(t, false, nil, dns("203.0.113.7"), "doctor", "--domain", "share.example.com", "--only", only)
	if res.code != exitOK || !strings.Contains(res.stdout, "[ ok ] dns           share.example.com → 203.0.113.7 (this server)") {
		t.Errorf("pre-check with a domain that points here: exit %d\n%s", res.code, res.stdout)
	}
	res = runDoctorCLI(t, false, nil, dns("198.51.100.4"), "doctor", "--domain", "share.example.com", "--only", only)
	want := `[fail] dns           share.example.com points to 198.51.100.4, but this server is 203.0.113.7
       fix: point the A record of share.example.com to 203.0.113.7 only, then run doctor again (a changed record can take a while to show)
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-dns
`
	if res.code != exitDoctorFail || !strings.Contains(res.stdout, want) || !strings.HasSuffix(res.stdout, "\n2 ok · 0 warn · 1 fail\n") || res.stderr != "" {
		t.Errorf("pre-check with a domain that points elsewhere: exit %d, stderr %q\n%s", res.code, res.stderr, res.stdout)
	}
}

// The exit codes of 04 §3.2: 0 without failures (warnings allowed), 5 with one, and with --strict also for a
// warning. A config with errors is a failed check, not an exit 78.
func TestDoctorExitCodes(t *testing.T) {
	lowBuffers := func(env *doctor.Env) {
		env.FS.(fstest.MapFS)["proc/sys/net/core/rmem_max"] = &fstest.MapFile{Data: []byte("212992\n")}
	}
	portTaken := func(env *doctor.Env) {
		env.Bind = func(_ context.Context, network, addr string) error {
			if network == "tcp" && addr == ":443" {
				return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
			}
			return nil
		}
	}
	tests := []struct {
		name string
		hook func(*doctor.Env)
		args []string
		code int
		want string
	}{
		{"nothing wrong", nil, nil, exitOK, " · 0 warn · 0 fail"},
		{"nothing wrong, strict", nil, []string{"--strict"}, exitOK, " · 0 warn · 0 fail"},
		{"a warning", lowBuffers, nil, exitOK, "[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608"},
		{"a warning, strict", lowBuffers, []string{"--strict"}, exitDoctorFail, " · 1 warn · 0 fail"},
		{"a failure", portTaken, nil, exitDoctorFail, "[fail] ports         443/tcp is in use by another program (local check only)"},
		{"a failure, strict", portTaken, []string{"--strict"}, exitDoctorFail, " · 0 warn · 1 fail"},
		{"a failure outside --only", portTaken, []string{"--only", "dns,clock"}, exitOK, "2 ok · 0 warn · 0 fail"},
		{"a config with errors", nil, []string{"--tls.mode", "manual", "--only", "config"}, exitDoctorFail, "[fail] config        1 error: tls.mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"doctor", "--tls.mode", "ip"}, tt.args...)
			res := runDoctorCLI(t, false, nil, tt.hook, args...)
			if res.code != tt.code || !strings.Contains(res.stdout, tt.want) || res.stderr != "" {
				t.Errorf("exit %d, want %d; stderr %q; want %q in:\n%s", res.code, tt.code, res.stderr, tt.want, res.stdout)
			}
			// --json exits the same way, with the document on stdout and nothing on stderr.
			res = runDoctorCLI(t, false, nil, tt.hook, append(args, "--json")...)
			var rep api.DoctorReport
			if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil || res.code != tt.code || res.stderr != "" {
				t.Errorf("--json: exit %d, want %d; stderr %q; %v", res.code, tt.code, res.stderr, err)
			}
		})
	}

	// The failure comes with its fix and the link to its troubleshooting section.
	res := runDoctorCLI(t, false, nil, portTaken, "doctor", "--tls.mode", "ip", "--only", "ports")
	want := `[fail] ports         443/tcp is in use by another program (local check only)
       This only checks this machine. The browser connection test checks from outside.
       fix: stop the other program (another web server?), or run isshoni behind it with tls.mode = "off"
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-ports
0 ok · 0 warn · 1 fail
`
	if _, body, _ := strings.Cut(res.stdout, "\n"); body != want {
		t.Errorf("text:\n%s\nwant:\n%s", body, want)
	}
}

// The bandwidth flags describe the session of the estimate (04 §13.4).
func TestDoctorBandwidthFlags(t *testing.T) {
	res := runDoctorCLI(t, false, nil, nil, "doctor", "--only", "bandwidth",
		"--people", "10", "--sharing", "10", "--thumbnails", "8", "--quality", "1080p60", "--preset", "auto", "--hours", "2")
	if res.code != exitOK || !strings.Contains(res.stdout, "[info] bandwidth     10 people, 10 sharing: ~105.3 Mbps egress (~111 on the wire), ~99 GB per 2-hour session") {
		t.Errorf("the plan's example: exit %d\n%s", res.code, res.stdout)
	}
	res = runDoctorCLI(t, false, nil, nil, "doctor", "--only", "bandwidth", "--json", "--quality", "2160p60", "--preset", "movie", "--hours", "0.5")
	var rep api.DoctorReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	want := api.BandwidthInput{People: 5, Sharing: 2, Thumbnails: 8, Quality: api.BandwidthQuality2160p60, Preset: api.BandwidthPresetMovie, Hours: 0.5}
	if rep.Bandwidth == nil || rep.Bandwidth.Input != want || rep.Bandwidth.PerViewerMbps.Viewer != 32.56 || len(rep.Checks) != 1 {
		t.Errorf("--json bandwidth: %+v", rep.Bandwidth)
	}
	if !strings.Contains(res.stdout, `"bandwidth":{"input":{"people":5,"sharing":2,"thumbnails":8,"quality":"2160p60","preset":"movie","hours":0.5},"perViewerMbps":{`) {
		t.Errorf("--json bandwidth document: %s", res.stdout)
	}
}

// Offline doctor reads the database through 03's file-level functions and leaves the data directory as it found
// it: no -wal or -shm file, nothing created (04 §13.1, §17). A schema newer than this binary is explained with
// 03's message and the command that restores the backup from before the upgrade.
func TestDoctorOfflineSchema(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFlags(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--data-dir", dataDir, "--tls.mode", "off"})
	if err != nil {
		t.Fatal(err)
	}
	paths := cfg.Paths()
	db, err := store.Open(t.Context(), store.Options{Path: paths.DB, BackupDir: paths.Backups, AppVersion: "0.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := config.OpenSecrets(paths.Secrets, nil); err != nil {
		t.Fatal(err)
	}
	listing := func() []string {
		t.Helper()
		var names []string
		err := filepath.WalkDir(dataDir, func(path string, _ os.DirEntry, err error) error {
			names = append(names, strings.TrimPrefix(path, dataDir))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	before := listing()
	env := []string{"ISSHONI_DATA_DIR=" + dataDir}
	latest := store.LatestSchemaVersion()

	res := runDoctorCLI(t, false, env, nil, "doctor", "--tls.mode", "off", "--only", "data_dir,secrets,schema")
	want := "[ ok ] schema        schema " + strconv.Itoa(latest) + " (" + paths.DB + ")\n"
	if res.code != exitOK || !strings.Contains(res.stdout, want) || !strings.Contains(res.stdout, "[ ok ] secrets ") ||
		!strings.Contains(res.stdout, "[ ok ] data_dir ") {
		t.Errorf("exit %d\n%s\nwant a line %q", res.code, res.stdout, want)
	}
	if after := listing(); !slices.Equal(before, after) {
		t.Errorf("doctor changed the data directory:\n got %v\nwant %v", after, before)
	}

	// A newer isshoni has used the database, and its pre-migration backup is there.
	alter, err := sql.Open("sqlite", paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = alter.ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, 'from_the_future', 0, '9.9.9')`, latest+1)
	if err := errors.Join(err, alter.Close()); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(paths.Backups, "pre-"+strconv.Itoa(latest)+"-20261014T021500Z.db")
	if err := os.MkdirAll(paths.Backups, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("a database from before the upgrade"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = listing()
	info, err := store.InspectFile(t.Context(), paths.DB, paths.Backups)
	if err != nil || info.TooNew == nil {
		t.Fatalf("InspectFile: %+v, %v", info, err)
	}

	res = runDoctorCLI(t, false, env, nil, "doctor", "--tls.mode", "off", "--only", "schema")
	wantText := "[fail] schema        database schema " + strconv.Itoa(latest+1) + " is newer than this isshoni build supports (" +
		strconv.Itoa(latest) + "): isshoni refuses to start\n" +
		"       fix: " + info.TooNew.Error() + "\n" +
		"            sudo -u isshoni isshoni admin restore --offline " + backup + " && sudo systemctl start isshoni\n" +
		"       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-schema\n"
	if res.code != exitDoctorFail || !strings.Contains(res.stdout, wantText) {
		t.Errorf("exit %d\n%s\nwant:\n%s", res.code, res.stdout, wantText)
	}
	if !strings.Contains(info.TooNew.Error(), backup) {
		t.Errorf("03's message does not name the backup: %s", info.TooNew.Error())
	}
	if after := listing(); !slices.Equal(before, after) {
		t.Errorf("doctor changed the data directory:\n got %v\nwant %v", after, before)
	}
}

// `sudo isshoni doctor` while the server is stopped, on a host that the installer set up (04 §6.3, 06 §4.8): the
// owners of the data directory and of secrets.json are compared with the service's user and not with root, so a
// file that makes the next start exit 78 fails its check, and the fix gives it to the service's user.
func TestDoctorOfflineOwners(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file owners")
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dataDir, "secrets.json")
	if _, err := config.OpenSecrets(secrets, nil); err != nil {
		t.Fatal(err)
	}
	environ := []string{"ISSHONI_DATA_DIR=" + dataDir}
	asRoot := func(serviceUID int) func(*doctor.Env) {
		return func(env *doctor.Env) {
			env.UID, env.User = 0, "root"
			env.LookupUser = func(name string) (int, bool) { return serviceUID, name == doctor.ServiceUser }
		}
	}

	// The files belong to the service's user.
	res := runDoctorCLI(t, false, environ, asRoot(os.Getuid()), "doctor", "--tls.mode", "off", "--only", "data_dir,secrets")
	if res.code != exitOK || !strings.Contains(res.stdout, "[ ok ] data_dir ") || !strings.Contains(res.stdout, "[ ok ] secrets ") {
		t.Errorf("the service's own files: exit %d\n%s", res.code, res.stdout)
	}

	// They belong to someone else: exit 5, and they go to isshoni, not to root.
	other := os.Getuid() + 1000
	res = runDoctorCLI(t, false, environ, asRoot(other), "doctor", "--tls.mode", "off", "--only", "data_dir,secrets")
	for _, want := range []string{
		"[fail] data_dir      " + dataDir + " belongs to uid " + strconv.Itoa(os.Getuid()) + ", but isshoni runs as the user isshoni (uid " + strconv.Itoa(other) + ")\n" +
			"       fix: sudo chown -R isshoni " + dataDir + "\n",
		"[fail] secrets       " + secrets + " belongs to uid " + strconv.Itoa(os.Getuid()) + ", but isshoni runs as the user isshoni (uid " + strconv.Itoa(other) + ")\n" +
			"       fix: sudo chown isshoni " + secrets + "\n",
		"\n0 ok · 0 warn · 2 fail\n",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("no %q in:\n%s", want, res.stdout)
		}
	}
	if res.code != exitDoctorFail || res.stderr != "" || strings.Contains(res.stdout, "chown -R root") {
		t.Errorf("someone else's files: exit %d, stderr %q\n%s", res.code, res.stderr, res.stdout)
	}
}

// With a running server the checks run inside it (04 §13.1): the CLI prints the server's report and runs nothing
// itself.
func TestDoctorWithServer(t *testing.T) {
	f, env := startTestServer(t, false)

	res := runDoctorCLI(t, false, env, nil, "doctor")
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if res.code != exitOK || res.stderr != "" || res.offline != 0 {
		t.Fatalf("exit %d, %d offline runs, stderr %q\n%s", res.code, res.offline, res.stderr, res.stdout)
	}
	if lines[0] != "isshoni doctor · 0.3.0 · server running · 2026-09-29 13:12 UTC" {
		t.Errorf("header %q", lines[0])
	}
	for _, want := range []string{
		"[ ok ] tls           ip certificate for 203.0.113.7, valid until 2026-10-06 00:00 UTC",
		"[info] ports         listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (local check only)",
		"[ ok ] transfer      12.3 GB out in 2026-09; no alert limit set",
		"[ ok ] release       0.3.0 is the latest release",
		"12 ok · 0 warn · 0 fail",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("no line %q in:\n%s", want, res.stdout)
		}
	}

	res = runDoctorCLI(t, false, env, nil, "doctor", "--json")
	var rep api.DoctorReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("--json: %v\n%s", err, res.stdout)
	}
	if res.code != exitOK || res.stderr != "" || rep.Mode != api.DoctorModeCLIWithServer || rep.Version != "0.3.0" ||
		len(rep.Checks) != 17 || rep.Summary != (api.DoctorCounts{OK: 12, Info: 5}) || !rep.RanAt.Equal(fakeNow) {
		t.Errorf("--json: exit %d, mode %s, summary %+v", res.code, rep.Mode, rep.Summary)
	}

	// --only and the bandwidth flags travel to the server.
	res = runDoctorCLI(t, false, env, nil, "doctor", "--only", "bandwidth,tls", "--people", "10", "--sharing", "10")
	if lines := strings.Split(res.stdout, "\n"); res.code != exitOK || len(lines) != 5 || !strings.HasPrefix(lines[1], "[ ok ] tls ") ||
		lines[2] != "[info] bandwidth     10 people, 10 sharing: ~105.3 Mbps egress (~111 on the wire), ~99 GB per 2-hour session" {
		t.Errorf("--only with bandwidth flags: exit %d\n%s", res.code, res.stdout)
	}

	// A warning: exit 0, with --strict 5.
	f.machine.set("warn")
	res = runDoctorCLI(t, false, env, nil, "doctor")
	if res.code != exitOK || !strings.Contains(res.stdout, "[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608\n") ||
		!strings.HasSuffix(res.stdout, "\n11 ok · 1 warn · 0 fail\n") {
		t.Errorf("a warning: exit %d\n%s", res.code, res.stdout)
	}
	if res = runDoctorCLI(t, false, env, nil, "doctor", "--strict"); res.code != exitDoctorFail || res.stderr != "" {
		t.Errorf("a warning with --strict: exit %d, stderr %q", res.code, res.stderr)
	}

	// A failure: exit 5, with the cause the server knows.
	f.machine.set("fail")
	res = runDoctorCLI(t, false, env, nil, "doctor")
	want := `[fail] tls           still no ip certificate for 203.0.113.7, 192 minutes after the start
       fix: Let's Encrypt couldn't connect to 203.0.113.7 on port 80 or 443. Open TCP 80 and 443 in your cloud firewall
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-tls
`
	if res.code != exitDoctorFail || !strings.Contains(res.stdout, want) || res.stderr != "" || res.offline != 0 {
		t.Errorf("a failure: exit %d, stderr %q\n%s", res.code, res.stderr, res.stdout)
	}
	res = runDoctorCLI(t, false, env, nil, "doctor", "--json", "--only", "tls")
	if res.code != exitDoctorFail || !strings.Contains(res.stdout, `"fixCode":"tls.acme_unreachable"`) || res.stderr != "" {
		t.Errorf("a failure with --json: exit %d, stderr %q, stdout %s", res.code, res.stderr, res.stdout)
	}

	// A usage error never reaches the server.
	if res = runDoctorCLI(t, false, env, nil, "doctor", "--only", "nope"); res.code != exitUsage {
		t.Errorf("--only nope with a server: exit %d", res.code)
	}

	// A server that shuts down answers 503: exit 1, and no offline run behind its back.
	f.health.SetShuttingDown()
	res = runDoctorCLI(t, false, env, nil, "doctor")
	if res.code != exitRuntime || res.stdout != "" || res.offline != 0 || !strings.Contains(res.stderr, "isshoni doctor: the server is shutting down") {
		t.Errorf("a server that shuts down: exit %d, %d offline runs, stdout %q, stderr %q", res.code, res.offline, res.stdout, res.stderr)
	}
}

// A user the admin socket refuses gets the sudo message and exit 4, and no check runs: as an ordinary user the
// offline checks would only be false failures (04 §12.1, §13.1).
func TestDoctorPermissionDenied(t *testing.T) {
	if !peerCreds {
		t.Skip("this platform does not check peer credentials on the admin socket")
	}
	f, env := startTestServer(t, true)
	for _, args := range [][]string{{"doctor"}, {"doctor", "--json", "--only", "dns"}} {
		res := runDoctorCLI(t, false, env, nil, args...)
		want := "Permission denied on " + f.path + ": run it with sudo (sudo isshoni " + strings.Join(args, " ") + ")\n"
		if res.code != exitUnreachable || res.stdout != "" || res.stderr != want || res.offline != 0 {
			t.Errorf("%v: exit %d, %d offline runs, stdout %q, stderr %q; want exit 4 and %q", args, res.code, res.offline, res.stdout, res.stderr, want)
		}
	}
}

// Ctrl-C during an offline run: exit 130, and no report with half its checks skipped.
func TestDoctorInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	empty := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	inv := &invocation{
		stdout: &out, stderr: &errOut,
		environ: []string{"ISSHONI_CONFIG=" + empty, "ISSHONI_LISTEN_ADMIN_SOCKET=" + socketPath(t), "ISSHONI_DATA_DIR=" + t.TempDir()},
		doctorEnv: func(env *doctor.Env) {
			fakeProbes(env, t.TempDir(), "8388608")
			env.NoFile = func() (uint64, error) {
				cancel()
				return 65536, nil
			}
		},
	}
	code := run(ctx, inv, []string{"doctor", "--tls.mode", "off"})
	if code != exitInterrupted || out.Len() != 0 || !strings.Contains(errOut.String(), "isshoni doctor: context canceled") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 130 and no report", code, out.String(), errOut.String())
	}
}
