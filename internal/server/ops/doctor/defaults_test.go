package doctor

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// An Env with nothing but a config gets the real machine: its clock, its interfaces, its limits. The checks run
// here ask the machine only; none of them reaches for the network.
func TestRunDefaults(t *testing.T) {
	cfg := loadConfig(t, "--tls.mode", "off", "--data-dir", filepath.Join(t.TempDir(), "never-started"))
	before := time.Now()
	rep := Run(t.Context(), Env{Config: cfg, UID: os.Getuid()}, api.BandwidthInput{},
		[]string{"config", "data_dir", "secrets", "lan_privacy", "nofile", "bandwidth"})

	if rep.Mode != api.DoctorModeCLIOffline || rep.Version != version.Version() || rep.RanAt.Before(before.Add(-time.Second)) {
		t.Errorf("header: mode %s, version %q, ranAt %s", rep.Mode, rep.Version, rep.RanAt)
	}
	if rep.Env.OS != runtime.GOOS || rep.Env.Arch != runtime.GOARCH || rep.Env.User == "" || rep.Env.Provider == "" || rep.Env.Container == "" {
		t.Errorf("env %+v", rep.Env)
	}
	if unixPerms && rep.Env.Kernel == "" {
		t.Errorf("env.kernel is empty on %s", runtime.GOOS)
	}
	if len(rep.Checks) != 6 || rep.Bandwidth == nil || rep.Summary.Fail != 0 {
		t.Fatalf("%d checks, summary %+v", len(rep.Checks), rep.Summary)
	}
	for _, c := range rep.Checks {
		if c.Message == "" || c.Message == c.Code || c.DurationMs < 0 {
			t.Errorf("%s: %+v", c.ID, c)
		}
	}
	if c := rep.Checks[4]; unixPerms && (c.Code != codeNofileOK && c.Code != codeNofileLow) {
		t.Errorf("nofile on this machine: %+v", c)
	}
}

// Run needs a config: without one it is a programming error of the caller.
func TestRunWithoutConfigPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Run with a nil Config did not panic")
		}
	}()
	Run(t.Context(), Env{}, api.BandwidthInput{}, nil)
}

// The clock check's own HTTP client: one GET with isshoni's User-Agent, and trust in tls.acme_ca_root for an ACME
// server with a private CA. The server is a loopback one of the test.
func TestClockOwnHTTPClient(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Date", time.Now().Add(-10*time.Minute).UTC().Format(http.TimeFormat))
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	root := filepath.Join(t.TempDir(), "root.pem")
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(root, rootPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	w := newWorld(t, "--tls.mode", "ip", "--tls.acme-ca", srv.URL+"/directory", "--tls.acme-ca-root", root)
	env := w.build()
	env.HTTP, env.Now = nil, nil // doctor's own client and the real clock
	c := Run(t.Context(), env, api.BandwidthInput{}, []string{"clock"}).Checks[0]
	// The fake directory says that it is ten minutes earlier than it is.
	want(t, c, api.DoctorStatusFail, codeClockSkew, fixClockNTP)
	contains(t, c.Message, "the clock is 10m", " ahead (compared with 127.0.0.1:")
	mu.Lock()
	if len(got) != 1 || got[0] != "GET /directory "+version.UserAgent() {
		t.Errorf("requests %q, want one GET /directory with isshoni's User-Agent", got)
	}
	mu.Unlock()

	// Without the root the server's certificate is not trusted: no skew, and with the sync flag fine the check
	// passes on that alone.
	w = newWorld(t, "--tls.mode", "ip", "--tls.acme-ca", srv.URL+"/directory")
	env = w.build()
	env.HTTP = nil
	c = Run(t.Context(), env, api.BandwidthInput{}, []string{"clock"}).Checks[0]
	want(t, c, api.DoctorStatusOK, codeClockOK, "")
	if _, ok := c.Params["skewS"]; ok || c.Params["error"] == nil {
		t.Errorf("params %v, want an error and no skew", c.Params)
	}

	// A root file that is missing, or holds no certificate, is the reason the check gives.
	for _, file := range []string{filepath.Join(t.TempDir(), "missing.pem"), filepath.Join(w.dataDir, "secrets.json")} {
		w = newWorld(t, "--tls.mode", "ip", "--tls.acme-ca", srv.URL+"/directory", "--tls.acme-ca-root", file)
		env = w.build()
		env.HTTP = nil
		env.Adjtimex = func() (bool, error) { return false, errors.New("EPERM") }
		c = Run(t.Context(), env, api.BandwidthInput{}, []string{"clock"}).Checks[0]
		want(t, c, api.DoctorStatusInfo, codeClockUnknown, "")
		contains(t, c.Message, "tls.acme_ca_root")
	}

	// An answer without a Date header gives no skew.
	w = newWorld(t, "--tls.mode", "ip")
	w.env.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})}
	c = w.check("clock")
	want(t, c, api.DoctorStatusOK, codeClockOK, "")
	contains(t, c.Params["error"].(string), "no usable Date header")
}

// The small text helpers.
func TestTextHelpers(t *testing.T) {
	if got := and(nil) + "|" + and([]string{"a"}) + "|" + and([]string{"a", "b"}) + "|" + and([]string{"a", "b", "c"}); got != "|a|a and b|a, b and c" {
		t.Errorf("and: %q", got)
	}
	if got := sizeText(1_500_000_000) + "|" + sizeText(999_999_999) + "|" + sizeText(0); got != "1.5 GB|1000 MB|0 MB" {
		t.Errorf("sizeText: %q", got)
	}
	if got := timeText("2026-10-05T12:00:00.000+02:00") + "|" + dateText("2026-10-05T23:30:00.000-02:00") + "|" + timeText("soon") + "|" + dateText("soon"); got != "2026-10-05 10:00 UTC|2026-10-06|soon|soon" {
		t.Errorf("timeText, dateText: %q", got)
	}
	if got := plural(1, "day") + "|" + plural(0, "day") + "|" + number(2) + "|" + number(2.5); got != "1 day|0 days|2|2.5" {
		t.Errorf("plural, number: %q", got)
	}
	if got := dnsErrText(&net.DNSError{Err: "server misbehaving"}) + "|" + dnsErrText(context.DeadlineExceeded) + "|" + dnsErrText(errors.New("boom")); got != "server misbehaving|the resolver did not answer in time|boom" {
		t.Errorf("dnsErrText: %q", got)
	}
	if got := httpErrText(context.DeadlineExceeded); got != "no answer in time" {
		t.Errorf("httpErrText: %q", got)
	}
	p := params{"n": 3, "f": 2.5, "s": "x", "b": true, "l": []string{"a"}}
	if p.str("n") != "3" || p.str("none") != "" || p.int("f") != 2 || p.num("s") != 0 || !p.bool("b") || p.bool("s") || p.list("s") != nil || !p.has("l") {
		t.Errorf("params accessors: %v", p)
	}
	// The service's user by name, by number where the report has no name, and by number on a container's host.
	sys, numbered, docker := api.DoctorEnv{User: "isshoni"}, api.DoctorEnv{User: "uid 998"}, api.DoctorEnv{User: "isshoni", Container: api.ContainerKindDocker}
	uid := params{"uid": 998, "path": "/var/lib/isshoni"}
	if got := ownerFix(uid, sys, true) + "|" + ownerFix(uid, numbered, false); got != "sudo chown -R isshoni /var/lib/isshoni|sudo chown 998 /var/lib/isshoni" {
		t.Errorf("ownerFix: %q", got)
	}
	if got := ownerFix(uid, docker, true); got != "on the container's host, run: sudo chown -R 998:998 <the host path of /var/lib/isshoni>" {
		t.Errorf("ownerFix in a container: %q", got)
	}
	if got := ownerFix(params{"path": "/data"}, sys, true); !strings.Contains(got, "let the user that isshoni runs as read and write /data") {
		t.Errorf("ownerFix without a uid: %q", got)
	}
	if restart(api.DoctorEnv{}) != "" || restart(api.DoctorEnv{Systemd: true}) != "sudo systemctl restart isshoni" {
		t.Error("restart names a command where doctor knows none, or the wrong one")
	}
}
