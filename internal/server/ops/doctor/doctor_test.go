package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with a file in testdata, or writes the file with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil { //nolint:gosec // G301: a directory of the repository
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: a test fixture in the repository
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run the test with -update after checking the change):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// The ids of 04 §13.2, in table order: what --list-checks prints, and what the site's troubleshooting anchors are
// named after (06 §10.4).
func TestCheckIDs(t *testing.T) {
	want := []string{
		"config", "data_dir", "secrets", "schema", "public_ip", "dns", "tls", "clock", "udp_buffers", "ports",
		"firewall_hint", "container", "lan_privacy", "transfer", "release", "nofile", "bandwidth",
	}
	if got := CheckIDs(); !slices.Equal(got, want) {
		t.Errorf("CheckIDs:\n got %v\nwant %v", got, want)
	}
	for _, id := range CheckIDs() {
		if len(id) > idWidth {
			t.Errorf("check id %q is longer than the text output's column (%d)", id, idWidth)
		}
	}
}

// vps is the healthy server of 04 §13.2's example: everything fine except the socket buffer limits, which
// install.sh has not raised.
func vps(t *testing.T) *world {
	w := newWorld(t, "--tls.mode", "ip")
	w.files["proc/sys/net/core/rmem_max"].Data = []byte("212992\n")
	w.files["proc/sys/net/core/wmem_max"].Data = []byte("212992\n")
	w.live.UDPRcvBufBytes, w.live.UDPSndBufBytes = 212992, 212992
	return w
}

// A whole report of a running server: every check, in order, with the summary of 04 §13.5's example (12 ok, 1 warn,
// 0 fail, 0 skip, 4 info) and the JSON of that section.
func TestRunServerReport(t *testing.T) {
	w := vps(t)
	rep := Run(t.Context(), w.build(), api.BandwidthInput{}, nil)

	if rep.Schema != api.DoctorReportSchema || rep.Mode != api.DoctorModeServer || rep.Version != "0.3.0" || !rep.RanAt.Equal(testNow) {
		t.Errorf("header: schema %d, mode %s, version %s, ranAt %s", rep.Schema, rep.Mode, rep.Version, rep.RanAt)
	}
	wantEnv := api.DoctorEnv{
		OS: "linux", Arch: rep.Env.Arch, Kernel: "6.8.0-45-generic", Container: api.ContainerKindNone, Systemd: true,
		Provider: api.CloudProviderHetzner, User: "isshoni",
	}
	if rep.Env != wantEnv {
		t.Errorf("env %+v, want %+v", rep.Env, wantEnv)
	}
	var ids []string
	for _, c := range rep.Checks {
		ids = append(ids, c.ID)
		if c.LocalOnly != (c.ID == "ports") {
			t.Errorf("%s: localOnly %v", c.ID, c.LocalOnly)
		}
		if c.Message == "" || c.Message == c.Code {
			t.Errorf("%s: code %s has no message", c.ID, c.Code)
		}
	}
	if !slices.Equal(ids, CheckIDs()) {
		t.Errorf("checks %v, want every id in order", ids)
	}
	if want := (api.DoctorCounts{OK: 12, Warn: 1, Fail: 0, Skip: 0, Info: 4}); rep.Summary != want {
		t.Errorf("summary %+v, want %+v", rep.Summary, want)
	}
	if rep.Bandwidth == nil || rep.Bandwidth.Input != DefaultBandwidthInput() || rep.Bandwidth.EgressMediaMbps != 41.54 {
		t.Errorf("bandwidth %+v, want the estimate of the default session", rep.Bandwidth)
	}

	rep.Env.Arch = "amd64" // the one field that follows the machine the test runs on
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The data directory is a temporary one.
	data = bytes.ReplaceAll(data, []byte(filepath.ToSlash(w.dataDir)), []byte("/var/lib/isshoni"))
	golden(t, "report.server.json", append(data, '\n'))

	var text bytes.Buffer
	RenderText(&text, rep, false)
	golden(t, "report.server.txt", bytes.ReplaceAll(text.Bytes(), []byte(filepath.ToSlash(w.dataDir)), []byte("/var/lib/isshoni")))
}

// Without a server the same checks run on the machine itself: what only a running server knows is skipped, the
// rest is found out (04 §13.1).
func TestRunOfflineReport(t *testing.T) {
	w := newWorld(t, "--tls.mode", "ip", "--network.stun-servers", "stun.example.net:3478")
	w.live = nil
	stun := &fakeSTUN{answers: map[string]string{"stun.example.net:3478": testIP + ":40000"}}
	w.env.STUN = stun
	var bound []string
	w.env.Bind = func(_ context.Context, network, addr string) error {
		bound = append(bound, network+" "+addr)
		return nil
	}
	w.env.DB.Inspect = func(context.Context, string) (DBInfo, error) {
		t.Error("the schema check inspected a database that does not exist")
		return DBInfo{}, nil
	}
	rep := Run(t.Context(), w.build(), api.BandwidthInput{}, nil)

	if rep.Mode != api.DoctorModeCLIOffline {
		t.Errorf("mode %s, want cli-offline", rep.Mode)
	}
	status := map[string]api.DoctorCheck{}
	for _, c := range rep.Checks {
		status[c.ID] = c
	}
	for _, id := range []string{"tls", "transfer", "release"} {
		want(t, status[id], api.DoctorStatusSkip, codeSkipNeedsServer, "")
	}
	want(t, status["public_ip"], api.DoctorStatusOK, codePublicIPOK, "")
	contains(t, status["public_ip"].Message, testIP+" (on interface eth0, confirmed by STUN)")
	want(t, status["schema"], api.DoctorStatusInfo, codeSchemaNoDB, "")
	want(t, status["ports"], api.DoctorStatusInfo, codePortsFree, "")
	if stun.calls == 0 {
		t.Error("the offline public_ip check asked no STUN server")
	}
	if want := []string{"tcp :443", "tcp :80", "udp :7882", "tcp :7882"}; !slices.Equal(bound, want) {
		t.Errorf("ports tried to bind %v, want %v", bound, want)
	}
	if rep.Summary.Fail != 0 || rep.Summary.Skip != 3 {
		t.Errorf("summary %+v, want no failure and 3 skipped", rep.Summary)
	}

	var text bytes.Buffer
	RenderText(&text, rep, false)
	out := strings.ReplaceAll(text.String(), filepath.ToSlash(w.dataDir), "/var/lib/isshoni")
	rest, _ := strings.CutPrefix(out, "isshoni doctor · ")
	_, rest, _ = strings.Cut(rest, " · ") // the version of the test binary
	golden(t, "report.offline.txt", []byte(rest))
}

// --only runs the named checks, in table order whatever the order of the names; an unknown id is ignored. The
// estimate is part of the report only with the bandwidth check.
func TestRunOnly(t *testing.T) {
	w := vps(t)
	rep := Run(t.Context(), w.build(), api.BandwidthInput{}, []string{"clock", "nope", "dns", "public_ip"})
	var ids []string
	for _, c := range rep.Checks {
		ids = append(ids, c.ID)
	}
	if want := []string{"public_ip", "dns", "clock"}; !slices.Equal(ids, want) {
		t.Errorf("checks %v, want %v", ids, want)
	}
	if rep.Summary != (api.DoctorCounts{OK: 3}) || rep.Bandwidth != nil {
		t.Errorf("summary %+v, bandwidth %v", rep.Summary, rep.Bandwidth)
	}

	in := api.BandwidthInput{People: 10, Sharing: 10, Thumbnails: 8, Quality: api.BandwidthQuality1080p60, Preset: api.BandwidthPresetAuto, Hours: 2}
	rep = Run(t.Context(), w.build(), in, []string{"bandwidth"})
	if len(rep.Checks) != 1 || rep.Bandwidth == nil || rep.Bandwidth.Input != in || rep.Bandwidth.EgressMediaMbps != 105.28 {
		t.Errorf("--only bandwidth: %d checks, bandwidth %+v", len(rep.Checks), rep.Bandwidth)
	}
	contains(t, rep.Checks[0].Message, "10 people, 10 sharing: ~105.3 Mbps egress (~111 on the wire), ~99 GB per 2-hour session")
}

// A run that is interrupted skips what it has not checked yet, and still returns a report.
func TestRunInterrupted(t *testing.T) {
	w := vps(t)
	ctx, cancel := context.WithCancel(t.Context())
	env := w.build()
	env.NoFile = func() (uint64, error) { // the check before the last
		cancel()
		return 65536, nil
	}
	rep := Run(ctx, env, api.BandwidthInput{}, []string{"release", "nofile", "bandwidth"})
	if len(rep.Checks) != 3 {
		t.Fatalf("%d checks, want 3", len(rep.Checks))
	}
	want(t, rep.Checks[0], api.DoctorStatusOK, codeReleaseOK, "")
	want(t, rep.Checks[1], api.DoctorStatusInfo, codeNofileOK, "")
	want(t, rep.Checks[2], api.DoctorStatusSkip, codeSkipInterrupted, "")
	if rep.Summary.Skip != 1 {
		t.Errorf("summary %+v", rep.Summary)
	}
}

// Every code constant has an English text, every text belongs to a constant, and the two families keep to their
// naming: a message code is "<check id>.<what>" (or a skip code), a fix code "<check id>.fix_<what>" or one of the
// ten certificate codes of 04 §8.7.
func TestCodesHaveTexts(t *testing.T) {
	fset := token.NewFileSet()
	consts := map[string]string{} // constant name → value
	for _, file := range []string{"messages_en.go", "checks_net.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
				return true
			}
			lit, ok := spec.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			consts[spec.Names[0].Name] = value
			return true
		})
	}
	ids := CheckIDs()
	tlsCodes := []string{
		tlsACMEUnreachable, tlsDNSMissing, tlsDNSWrong, tlsRateLimited, tlsIPRejected, tlsCAAForbids, tlsACMEFailed,
		tlsCertUnreadable, tlsCertInvalid, tlsCertExpired,
	}
	var codes, fixes []string
	for name, value := range consts {
		id, what, _ := strings.Cut(value, ".")
		switch {
		case strings.HasPrefix(name, "code"):
			codes = append(codes, value)
			if id != "skip" && !slices.Contains(ids, id) || what == "" || strings.HasPrefix(what, "fix_") {
				t.Errorf("%s = %q is not <check id>.<what>", name, value)
			}
		case strings.HasPrefix(name, "fix"):
			fixes = append(fixes, value)
			if !slices.Contains(ids, id) || !strings.HasPrefix(what, "fix_") {
				t.Errorf("%s = %q is not <check id>.fix_<what>", name, value)
			}
		case strings.HasPrefix(name, "tls") && strings.HasPrefix(value, "tls."):
			fixes = append(fixes, value)
			if !slices.Contains(tlsCodes, value) {
				t.Errorf("%s = %q is not in this test's list of certificate codes", name, value)
			}
		}
	}
	slices.Sort(codes)
	slices.Sort(fixes)
	if got := Codes(); !slices.Equal(got, codes) {
		t.Errorf("message texts and code constants differ:\n texts     %v\n constants %v", got, codes)
	}
	if got := FixCodes(); !slices.Equal(got, fixes) {
		t.Errorf("fix texts and fix constants differ:\n texts     %v\n constants %v", got, fixes)
	}
	if len(codes) != len(slices.Compact(slices.Clone(codes))) || len(fixes) != len(slices.Compact(slices.Clone(fixes))) {
		t.Error("two constants share one code")
	}
	// A code without a text prints as itself, so that a missing text shows instead of an empty line.
	if got := messageEN("nope.code", nil, api.DoctorEnv{}); got != "nope.code" {
		t.Errorf("unknown code renders as %q", got)
	}
	if got := fixEN("nope.fix_code", nil, api.DoctorEnv{}); got != "" {
		t.Errorf("unknown fix code renders as %q", got)
	}
}

// Every text renders from empty params without a panic: a report that went through JSON, or a code the web
// client's catalog renders, never has more than the params say.
func TestTextsTakeAnyParams(t *testing.T) {
	envs := []api.DoctorEnv{{}, {Container: api.ContainerKindDocker}, {Systemd: true, User: "isshoni"}}
	// What JSON makes of the params: numbers are float64 and lists are []any.
	viaJSON := params{"want": float64(8388608), "others": []any{"198.51.100.4"}, "tcpPorts": []any{"80", "443"}, "uid": float64(998)}
	for _, e := range envs {
		for _, p := range []params{nil, {}, viaJSON} {
			for code, f := range messagesEN {
				if f(p, e) == "" && code != codeClockOK {
					t.Errorf("message %s is empty for params %v", code, p)
				}
			}
			for code, f := range fixesEN {
				if f(p, e) == "" && code != fixPortsFree {
					t.Errorf("fix %s is empty for params %v", code, p)
				}
			}
		}
	}
	if got := viaJSON.list("tcpPorts"); !slices.Equal(got, []string{"80", "443"}) {
		t.Errorf("list from JSON: %v", got)
	}
	if got := viaJSON.int("want"); got != 8388608 {
		t.Errorf("int from JSON: %d", got)
	}
}
