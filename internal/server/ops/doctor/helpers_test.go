package doctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// TestMain fails the run when a goroutine outlives the tests: the CAA test's resolver and the detection's STUN
// queries must all have stopped.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// Every test here runs doctor against fakes: no test asks a real resolver, STUN server or web server, binds a real
// service port or reads the machine's /proc. What stays real are a temporary data directory and, for the offline
// address detection, one unused UDP socket on an ephemeral port.

// testNow is the clock of the tests.
var testNow = time.Date(2026, 9, 29, 20, 15, 0, 0, time.UTC)

const (
	testIP     = "203.0.113.7"
	testIPv6   = "2001:db8::7"
	testDomain = "watch.example.com"
)

// loadConfig builds a config from flags alone, as `isshoni config init` does: no file, no environment. A config
// with errors is returned too, as config.Load returns it to doctor.
func loadConfig(t *testing.T, args ...string) *config.Config {
	t.Helper()
	cfg, err := config.LoadFlags(flag.NewFlagSet("test", flag.ContinueOnError), args)
	var ve *config.ValidationError
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("config %v: %v", args, err)
	}
	return cfg
}

// fakeResolver answers LookupNetIP from a table; a host that is not in it does not exist.
type fakeResolver struct {
	hosts map[string][]string
	err   error // returned for every lookup when set
	calls []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.calls = append(f.calls, host)
	if f.err != nil {
		return nil, f.err
	}
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

// fakeSTUN answers Mapped from a table by server; a server that is not in it does not answer.
type fakeSTUN struct {
	answers map[string]string
	calls   int
}

func (f *fakeSTUN) Mapped(_ context.Context, _ net.PacketConn, server string) (netip.AddrPort, error) {
	f.calls++
	a, ok := f.answers[server]
	if !ok {
		return netip.AddrPort{}, errors.New("no answer")
	}
	return netip.MustParseAddrPort(a), nil
}

// fakeIfaces is the host's interfaces.
type fakeIfaces struct {
	ifs   []netx.Interface
	route string // the source address of the default route; "" = no route
}

func (f *fakeIfaces) Interfaces() ([]netx.Interface, error) { return f.ifs, nil }

func (f *fakeIfaces) RouteSource(_ context.Context, dst netip.Addr) (netip.Addr, error) {
	if f.route == "" || !dst.Is4() {
		return netip.Addr{}, errors.New("no route")
	}
	return netip.MustParseAddr(f.route), nil
}

// iface builds an up interface with these addresses.
func iface(name string, addrs ...string) netx.Interface {
	it := netx.Interface{Name: name, Flags: net.FlagUp}
	for _, a := range addrs {
		it.Addrs = append(it.Addrs, netx.InterfaceAddr{Addr: netip.MustParseAddr(a)})
	}
	return it
}

// roundTrip is an http.RoundTripper from a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// dateServer is an HTTP client whose every answer carries this Date header: the ACME directory of the clock check.
// requests counts what was asked.
func dateServer(date time.Time, requests *[]string) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if requests != nil {
			*requests = append(*requests, r.Method+" "+r.URL.String())
		}
		h := http.Header{}
		h.Set("Date", date.UTC().Format(http.TimeFormat))
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})}
}

// failingHTTP is an HTTP client whose every request fails with err. requests counts what was asked.
func failingHTTP(err error, requests *[]string) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if requests != nil {
			*requests = append(*requests, r.Method+" "+r.URL.String())
		}
		return nil, err
	})}
}

// fsFile is a file of the fake machine.
type fsFile = fstest.MapFile

// world is a machine for doctor to examine: a healthy Linux VPS at Hetzner with systemd, whose isshoni runs in ip
// mode and is ready. Tests bend it before they call env.
type world struct {
	t       *testing.T
	args    []string // config flags; data_dir is added
	dataDir string
	files   fstest.MapFS
	live    *api.ServerStatus // nil = offline
	uid     int               // the uid doctor runs as: the owner of the data directory unless a test changes it
	env     Env               // what is set here wins over the defaults of newWorld
}

// newWorld returns the healthy VPS. Its data directory exists with a secrets.json, both private.
func newWorld(t *testing.T, args ...string) *world {
	t.Helper()
	w := &world{t: t, args: args, dataDir: filepath.Join(t.TempDir(), "isshoni"), uid: os.Getuid()}
	if err := os.Mkdir(w.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.OpenSecrets(filepath.Join(w.dataDir, "secrets.json"), nil); err != nil {
		t.Fatal(err)
	}
	w.files = fstest.MapFS{
		"proc/sys/kernel/osrelease":       {Data: []byte("6.8.0-45-generic\n")},
		"proc/sys/net/core/rmem_max":      {Data: []byte("8388608\n")},
		"proc/sys/net/core/wmem_max":      {Data: []byte("8388608\n")},
		"run/systemd/system/.keep":        {},
		"sys/class/dmi/id/sys_vendor":     {Data: []byte("Hetzner\n")},
		"sys/class/dmi/id/product_name":   {Data: []byte("vServer\n")},
		"sys/class/net/eth0/speed":        {Data: []byte("1000\n")},
		"sys/class/net/eth0/ifindex":      {Data: []byte("2\n")},
		"sys/class/net/eth0/iflink":       {Data: []byte("2\n")},
		"etc/resolv.conf":                 {Data: []byte("nameserver 192.0.2.53\n")},
		"sys/class/dmi/id/bios_vendor":    {Data: []byte("Hetzner\n")},
		"sys/class/dmi/id/chassis_vendor": {Data: []byte("QEMU\n")},
	}
	w.live = healthyStatus()
	return w
}

// healthyStatus is the status of a ready server in ip mode, one hour after its start.
func healthyStatus() *api.ServerStatus {
	return &api.ServerStatus{
		Version:   "0.3.0",
		StartedAt: testNow.Add(-time.Hour),
		UptimeS:   3600,
		Origin:    "https://" + testIP,
		TLS: api.TLSInfo{
			Mode: api.TLSModeIP, Names: []string{testIP}, Ready: true, Issuer: "Let's Encrypt",
			NotBefore:   time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC),
			NotAfter:    time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
			NextRenewal: time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC),
		},
		PublicIPv4: testIP, PublicIPv4Method: "interface",
		PublicIPv6: testIPv6, PublicIPv6Method: "interface",
		LocalIPv4: testIP,
		NAT:       api.NATKindNone,
		Advertised: []api.AdvertisedAddr{
			{Proto: "udp", Addr: testIP + ":7882", Via: api.TransportUDP},
			{Proto: "tcp", Addr: testIP + ":443", Via: api.TransportTCP443},
			{Proto: "tcp", Addr: testIP + ":7882", Via: api.TransportTCP7882},
		},
		Listeners: []api.ListenerInfo{
			{Key: "listen.https", Network: "tcp", Addr: "[::]:443"},
			{Key: "listen.http", Network: "tcp", Addr: "[::]:80"},
			{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"},
		},
		UDPRcvBufBytes: 8388608, UDPSndBufBytes: 8388608,
		Rooms: 1, Participants: 3, Shares: 2,
		SchemaVersion: 7,
		Transfer:      &api.TransferInfo{Month: "2026-09", EgressBytes: 412_000_000_000, IngressBytes: 98_000_000_000, AlertGB: 1000},
		Update:        &api.UpdateInfo{Latest: "0.3.0", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0"},
	}
}

// build returns the Env of the world. Fields that the test set in w.env are kept.
func (w *world) build() Env {
	w.t.Helper()
	env := w.env
	if env.Config == nil {
		args := append([]string{"--data-dir", w.dataDir}, w.args...)
		env.Config = loadConfig(w.t, args...)
	}
	env.Live = w.live
	if env.FS == nil {
		env.FS = w.files
	}
	if env.Resolver == nil {
		env.Resolver = &fakeResolver{}
	}
	if env.STUN == nil {
		env.STUN = &fakeSTUN{}
	}
	if env.HTTP == nil {
		env.HTTP = dateServer(testNow, nil)
	}
	if env.Adjtimex == nil {
		env.Adjtimex = func() (bool, error) { return false, nil }
	}
	if env.DB.LatestSchemaVersion == nil {
		env.DB.LatestSchemaVersion = func() int { return 7 }
	}
	if env.Now == nil {
		env.Now = func() time.Time { return testNow }
	}
	if env.GOOS == "" {
		env.GOOS = "linux"
	}
	env.UID = w.uid
	if env.User == "" {
		env.User = "isshoni"
	}
	if env.Host.Environ == nil {
		env.Host = config.Host{Environ: []string{}, Root: w.t.TempDir()}
	}
	if env.Interfaces == nil {
		env.Interfaces = &fakeIfaces{
			ifs:   []netx.Interface{iface("lo", "127.0.0.1"), iface("eth0", testIP, testIPv6)},
			route: testIP,
		}
	}
	if env.Bind == nil {
		env.Bind = func(context.Context, string, string) error { return nil }
	}
	if env.CAA == nil {
		env.CAA = func(context.Context, string) ([]CAARecord, error) { return nil, nil }
	}
	if env.DiskFree == nil {
		env.DiskFree = func(string) (uint64, error) { return 37_200_000_000, nil }
	}
	if env.NoFile == nil {
		env.NoFile = func() (uint64, error) { return 65536, nil }
	}
	return env
}

// check runs one check of the world and returns its result.
func (w *world) check(id string) api.DoctorCheck {
	w.t.Helper()
	rep := Run(w.t.Context(), w.build(), api.BandwidthInput{}, []string{id})
	if len(rep.Checks) != 1 || rep.Checks[0].ID != id {
		w.t.Fatalf("Run(only %s) returned %d checks: %+v", id, len(rep.Checks), rep.Checks)
	}
	return rep.Checks[0]
}

// want fails the test unless c has this status and code. fixCode "" means no fix.
func want(t *testing.T, c api.DoctorCheck, status api.DoctorStatus, code, fixCode string) {
	t.Helper()
	if c.Status != status || c.Code != code || c.FixCode != fixCode {
		t.Errorf("%s: %s %s (fix %q), want %s %s (fix %q)\n  message: %s\n  fix: %s",
			c.ID, c.Status, c.Code, c.FixCode, status, code, fixCode, c.Message, c.Fix)
	}
	if c.Message == "" || c.Message == c.Code {
		t.Errorf("%s: code %s has no message", c.ID, c.Code)
	}
	if (c.FixCode == "") != (c.Fix == "") {
		t.Errorf("%s: fix code %q with fix text %q", c.ID, c.FixCode, c.Fix)
	}
}

// contains fails the test unless text has every part.
func contains(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("%q is not in:\n%s", part, text)
		}
	}
}

// container turns the world's host into a container of this kind (the env variable Podman and systemd-nspawn
// set), with or without a volume mounted at the data directory.
func (w *world) container(kind string, mounted bool) {
	w.t.Helper()
	root := w.t.TempDir()
	mountinfo := "22 1 0:21 / / rw,relatime - overlay overlay rw\n"
	if mounted {
		mountinfo += fmt.Sprintf("40 22 8:1 /vol %s rw,relatime - ext4 /dev/sda1 rw\n", w.dataDir)
	}
	if err := os.MkdirAll(filepath.Join(root, "proc", "self"), 0o750); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "self", "mountinfo"), []byte(mountinfo), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.env.Host = config.Host{Environ: []string{"container=" + kind}, Root: root}
}

// errInUse and errDenied are what a bind answers for a taken port and for a privileged one.
var (
	errInUse  = &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	errDenied = &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)}
)
