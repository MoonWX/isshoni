package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/version"
)

// TestNew: the options are checked per mode.
func TestNew(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"off", Options{Mode: config.TLSOff}, ""},
		{"auto", Options{Mode: config.TLSAuto, Domain: testDomain, StorageDir: "certmagic"}, ""},
		{"auto without a domain", Options{Mode: config.TLSAuto, StorageDir: "certmagic"}, `"auto" needs a domain`},
		{"auto without storage", Options{Mode: config.TLSAuto, Domain: testDomain}, "needs a storage directory"},
		{"ip", Options{Mode: config.TLSIP, PublicIP: ip, StorageDir: "certmagic"}, ""},
		{"ip without an address", Options{Mode: config.TLSIP, StorageDir: "certmagic"}, ""}, // runs, and is not ready
		{"ip without storage", Options{Mode: config.TLSIP, PublicIP: ip}, "needs a storage directory"},
		{"manual", Options{Mode: config.TLSManual, CertFile: "c.pem", KeyFile: "k.pem"}, ""},
		{"manual without a key", Options{Mode: config.TLSManual, CertFile: "c.pem"}, "needs tls.cert_file and tls.key_file"},
		{"manual without a certificate", Options{Mode: config.TLSManual, KeyFile: "k.pem"}, "needs tls.cert_file and tls.key_file"},
		{"no mode", Options{}, `unknown tls.mode ""`},
		{"unknown mode", Options{Mode: "selfsigned"}, `unknown tls.mode "selfsigned"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(tc.opts)
			if tc.wantErr == "" {
				if err != nil || m == nil {
					t.Fatalf("New = %v, %v", m, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestNames: the certificate's name is the domain in lower case, or the address as text.
func TestNames(t *testing.T) {
	tests := []struct {
		opts Options
		want string
	}{
		{Options{Mode: config.TLSAuto, Domain: "Watch.Example.COM.", StorageDir: "s"}, "watch.example.com"},
		{Options{Mode: config.TLSIP, PublicIP: netip.MustParseAddr("203.0.113.7"), StorageDir: "s"}, "203.0.113.7"},
		{Options{Mode: config.TLSIP, PublicIP: netip.MustParseAddr("::ffff:203.0.113.7"), StorageDir: "s"}, "203.0.113.7"},
		{Options{Mode: config.TLSIP, PublicIP: netip.MustParseAddr("2001:db8::7"), StorageDir: "s"}, "2001:db8::7"},
		{Options{Mode: config.TLSManual, Domain: testDomain, CertFile: "c", KeyFile: "k"}, ""},
	}
	for _, tc := range tests {
		m, err := New(tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if m.name != tc.want {
			t.Errorf("%+v: name %q, want %q", tc.opts, m.name, tc.want)
		}
	}
}

// TestOffMode: a proxy has the certificate; the manager is ready and has nothing to do.
func TestOffMode(t *testing.T) {
	m, _ := newManager(t, Options{Mode: config.TLSOff})
	if m.TLSConfig() != nil {
		t.Error("TLSConfig is not nil in off mode")
	}
	if ok, detail := m.Ready(); !ok || detail != "" {
		t.Errorf("Ready = %v, %q", ok, detail)
	}
	start(t, m)
	start(t, m) // again: nothing
	if err := m.Reload(); err != nil {
		t.Errorf("Reload = %v", err)
	}
	st := m.Status()
	if st.Mode != config.TLSOff || !st.Ready || st.Names == nil || len(st.Names) != 0 || st.Issuer != "" {
		t.Errorf("Status = %+v", st)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

// TestTLSConfig: TLS 1.2 or newer and the protocols of 04 §7.2; each call returns its own copy.
func TestTLSConfig(t *testing.T) {
	m, err := New(Options{Mode: config.TLSAuto, Domain: testDomain, StorageDir: "s"})
	if err != nil {
		t.Fatal(err)
	}
	c := m.TLSConfig()
	if c.MinVersion != tls.VersionTLS12 || !slices.Equal(c.NextProtos, []string{"h2", "http/1.1", "acme-tls/1"}) || c.GetCertificate == nil {
		t.Errorf("TLSConfig: MinVersion %#x, NextProtos %v", c.MinVersion, c.NextProtos)
	}
	c.NextProtos[0] = "changed"
	c.MinVersion = 0
	if again := m.TLSConfig(); again.NextProtos[0] != "h2" || again.MinVersion != tls.VersionTLS12 {
		t.Errorf("a caller's change reached the manager's configuration: %v", again.NextProtos)
	}
	// TLS 1.1 is refused before any certificate is looked at.
	clientSide, serverSide := memPipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tls.Server(serverSide, m.TLSConfig()).HandshakeContext(context.Background())
		_ = serverSide.Close()
	}()
	old := &tls.Config{ServerName: testDomain, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11, InsecureSkipVerify: true} //nolint:gosec // G402: the test offers an old version on purpose
	err = tls.Client(clientSide, old).HandshakeContext(context.Background())
	_ = clientSide.Close()
	<-done
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("a TLS 1.1 handshake = %v, want a protocol version alert", err)
	}
}

// TestStatusJSON: the field names of 04 §8.2, zero times and empty strings left out.
func TestStatusJSON(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	full, err := json.Marshal(Status{
		Mode: config.TLSIP, Names: []string{"203.0.113.7"}, Ready: true, Issuer: "Let's Encrypt E7",
		NotBefore: at, NotAfter: at.Add(160 * time.Hour), NextRenewal: at.Add(80 * time.Hour),
		LastError: "fix", LastErrorCode: CodeRateLimited, LastErrorAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"mode":"ip","names":["203.0.113.7"],"ready":true,"issuer":"Let's Encrypt E7",` +
		`"not_before":"2026-10-01T10:00:00Z","not_after":"2026-10-08T02:00:00Z","next_renewal":"2026-10-04T18:00:00Z",` +
		`"last_error":"fix","last_error_code":"tls.rate_limited","last_error_at":"2026-10-01T10:00:00Z"}`
	if string(full) != want {
		t.Errorf("Status JSON = %s\n         want %s", full, want)
	}
	m, _ := newManager(t, Options{Mode: config.TLSAuto, Domain: testDomain, StorageDir: t.TempDir()})
	empty, err := json.Marshal(m.Status())
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"mode":"auto","names":["watch.example.com"],"ready":false}` {
		t.Errorf("Status JSON without a certificate = %s", empty)
	}
}

// TestIssuerName: how a certificate's issuer is shown.
func TestIssuerName(t *testing.T) {
	tests := []struct {
		issuer pkix.Name
		want   string
	}{
		{pkix.Name{Organization: []string{"Let's Encrypt"}, CommonName: "E7"}, "Let's Encrypt E7"},
		{pkix.Name{CommonName: "Pebble Intermediate CA 5a3c1f"}, "Pebble Intermediate CA 5a3c1f"},
		{pkix.Name{Organization: []string{"Example Trust"}, CommonName: "Example Trust TLS CA 1"}, "Example Trust TLS CA 1"},
		{pkix.Name{Organization: []string{"Example Trust"}}, "Example Trust"},
		{pkix.Name{Country: []string{"XX"}}, "C=XX"},
	}
	for _, tc := range tests {
		if got := issuerName(&x509.Certificate{Issuer: tc.issuer}); got != tc.want {
			t.Errorf("issuerName(%v) = %q, want %q", tc.issuer, got, tc.want)
		}
	}
}

// TestChallengeListeners: the bound addresses in certmagic's terms.
func TestChallengeListeners(t *testing.T) {
	tests := []struct {
		http, https string
		host        string
		httpPort    int
		tlsPort     int
	}{
		{"", "", "", 0, 0}, // certmagic's defaults: 80 and 443 on every address
		{"[::]:80", "[::]:443", "::", 80, 443},
		{"0.0.0.0:80", "0.0.0.0:443", "0.0.0.0", 80, 443},
		{"127.0.0.1:5002", "127.0.0.1:5001", "127.0.0.1", 5002, 5001},
		{"127.0.0.1:8080", "[::]:443", "", 8080, 443}, // two hosts: certmagic gets none
		{"203.0.113.7:80", "[2001:db8::7]:443", "", 80, 443},
		{"not an address", "127.0.0.1:443", "", 0, 443},
		{"127.0.0.1:http", "127.0.0.1:70000", "", 0, 0},
	}
	for _, tc := range tests {
		host, httpPort, tlsPort := challengeListeners(tc.http, tc.https)
		if host != tc.host || httpPort != tc.httpPort || tlsPort != tc.tlsPort {
			t.Errorf("challengeListeners(%q, %q) = %q, %d, %d; want %q, %d, %d", tc.http, tc.https, host, httpPort, tlsPort, tc.host, tc.httpPort, tc.tlsPort)
		}
	}
}

// TestDirectory: the ACME directory from tls.acme_ca and tls.acme_staging.
func TestDirectory(t *testing.T) {
	const prod, staging = "https://acme-v02.api.letsencrypt.org/directory", "https://acme-staging-v02.api.letsencrypt.org/directory"
	tests := []struct {
		ca      string
		staging bool
		want    string
	}{
		{"", false, prod},
		{prod, false, prod},
		{"", true, staging},
		{prod, true, staging},
		{"https://127.0.0.1:14000/dir", false, "https://127.0.0.1:14000/dir"},
		{"https://127.0.0.1:14000/dir", true, "https://127.0.0.1:14000/dir"}, // staging is Let's Encrypt's
	}
	for _, tc := range tests {
		m, err := New(Options{Mode: config.TLSAuto, Domain: testDomain, StorageDir: "s", CA: tc.ca, Staging: tc.staging})
		if err != nil {
			t.Fatal(err)
		}
		if got := m.directory(); got != tc.want {
			t.Errorf("directory(ca %q, staging %v) = %q, want %q", tc.ca, tc.staging, got, tc.want)
		}
	}
}

// TestACMESetup pins the certmagic configuration of 04 §8.2 for both modes, without a CA.
func TestACMESetup(t *testing.T) {
	ca := newTestCA(t, time.Now())
	rootFile := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(rootFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("auto", func(t *testing.T) {
		m, _ := acmeManager(t, Options{
			Mode: config.TLSAuto, Domain: "Watch.Example.com", Email: "admin@example.com",
			HTTPAddr: "127.0.0.1:5002", HTTPSAddr: "127.0.0.1:5001",
		})
		start(t, m)
		st := m.acme.Load()
		cfg, iss := st.cfg, st.issuer
		if cfg.DefaultServerName != testDomain || cfg.FallbackServerName != testDomain {
			t.Errorf("server names %q / %q, want the domain", cfg.DefaultServerName, cfg.FallbackServerName)
		}
		if cfg.RenewalWindowRatio != certmagic.DefaultRenewalWindowRatio {
			t.Errorf("RenewalWindowRatio = %v, want certmagic's default", cfg.RenewalWindowRatio)
		}
		if !cfg.OCSP.DisableStapling {
			t.Error("OCSP stapling is on: the server would call OCSP responders (04 §16)")
		}
		if cfg.OnDemand != nil {
			t.Error("on-demand issuance is on")
		}
		if store, ok := cfg.Storage.(*storage); !ok || store.Path != m.opts.StorageDir {
			t.Errorf("Storage = %#v, want the storage directory", cfg.Storage)
		}
		if len(cfg.Issuers) != 1 || cfg.Issuers[0] != certmagic.Issuer(iss) {
			t.Errorf("Issuers = %v, want the one ACME issuer", cfg.Issuers)
		}
		if iss.CA != "https://127.0.0.1:1/dir" || iss.TestCA != "" || iss.Email != "admin@example.com" || !iss.Agreed || iss.Profile != "" {
			t.Errorf("issuer: CA %q, TestCA %q, Email %q, Agreed %v, Profile %q", iss.CA, iss.TestCA, iss.Email, iss.Agreed, iss.Profile)
		}
		if iss.ListenHost != "127.0.0.1" || iss.AltHTTPPort != 5002 || iss.AltTLSALPNPort != 5001 {
			t.Errorf("issuer listens on %q, %d and %d; want our own listeners", iss.ListenHost, iss.AltHTTPPort, iss.AltTLSALPNPort)
		}
		if iss.DisableHTTPChallenge || iss.DisableTLSALPNChallenge || iss.DNS01Solver != nil {
			t.Error("both http-01 and tls-alpn-01 stay enabled, and nothing else (04 §8.1)")
		}
		if iss.TrustedRoots != nil {
			t.Error("TrustedRoots is set without tls.acme_ca_root")
		}
		if certmagic.UserAgent != version.UserAgent() {
			t.Errorf("certmagic.UserAgent = %q, want %q", certmagic.UserAgent, version.UserAgent())
		}
		if fi, err := os.Stat(m.opts.StorageDir); err != nil || !fi.IsDir() {
			t.Errorf("storage directory: %v", err)
		}
	})

	t.Run("ip", func(t *testing.T) {
		m, _ := acmeManager(t, Options{
			Mode: config.TLSIP, PublicIP: netip.MustParseAddr("203.0.113.7"), CARootFile: rootFile,
			Site: testSite(config.TLSIP, "203.0.113.7"),
		})
		start(t, m)
		st := m.acme.Load()
		cfg, iss := st.cfg, st.issuer
		if cfg.DefaultServerName != "203.0.113.7" || cfg.FallbackServerName != "203.0.113.7" {
			t.Errorf("server names %q / %q, want the address (IP clients send no SNI)", cfg.DefaultServerName, cfg.FallbackServerName)
		}
		if cfg.RenewalWindowRatio != 0.5 {
			t.Errorf("RenewalWindowRatio = %v, want 0.5", cfg.RenewalWindowRatio)
		}
		if iss.Profile != "shortlived" {
			t.Errorf("Profile = %q, want shortlived", iss.Profile)
		}
		if iss.TrustedRoots == nil || iss.TrustedRoots.Equal(x509.NewCertPool()) {
			t.Error("TrustedRoots does not hold tls.acme_ca_root")
		}
		if iss.ListenHost != "" || iss.AltHTTPPort != 0 || iss.AltTLSALPNPort != 0 {
			t.Errorf("issuer listens on %q, %d and %d; want certmagic's defaults without bound addresses", iss.ListenHost, iss.AltHTTPPort, iss.AltTLSALPNPort)
		}
		if got := m.Status().Names; !slices.Equal(got, []string{"203.0.113.7"}) {
			t.Errorf("Status.Names = %v", got)
		}
	})

	t.Run("Let's Encrypt", func(t *testing.T) {
		// Built by hand, and never started: this one would talk to the real CA.
		m, err := New(Options{Mode: config.TLSAuto, Domain: testDomain, StorageDir: "s"})
		if err != nil {
			t.Fatal(err)
		}
		if got := m.directory(); got != certmagic.LetsEncryptProductionCA {
			t.Errorf("directory = %q", got)
		}
	})
}

// TestCARootFile: a tls.acme_ca_root that can't be used fails Start, and the manager can be started once it is fixed.
func TestCARootFile(t *testing.T) {
	dir := t.TempDir()
	rootFile := filepath.Join(dir, "root.pem")
	m, _ := acmeManager(t, Options{CARootFile: rootFile})

	err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tls.acme_ca_root") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Start without the file = %v", err)
	}
	if m.acme.Load() != nil {
		t.Error("a failed Start left certmagic running")
	}
	if err := os.WriteFile(rootFile, []byte("no PEM here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Fatalf("Start with a file without PEM = %v", err)
	}
	ca := newTestCA(t, time.Now())
	if err := os.WriteFile(rootFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, m)
}

// TestACMEFailureShowsInStatus: a failed order is retried by certmagic; meanwhile the manager is not ready and
// Status, the readiness detail and the log carry the hint code.
func TestACMEFailureShowsInStatus(t *testing.T) {
	for _, mode := range []config.TLSMode{config.TLSAuto, config.TLSIP} {
		t.Run(string(mode), func(t *testing.T) {
			opts, name := Options{Mode: config.TLSAuto, Domain: testDomain}, testDomain
			if mode == config.TLSIP {
				opts, name = Options{Mode: config.TLSIP, PublicIP: netip.MustParseAddr("203.0.113.7"), Site: testSite(mode, "203.0.113.7")}, "203.0.113.7"
			}
			m, sink := acmeManager(t, opts)
			wantNotReady(t, m, "getting a certificate for "+name)
			begin := time.Now()
			start(t, m)
			if d := time.Since(begin); d > 2*time.Second {
				t.Errorf("Start took %v: the order runs in the background", d)
			}
			waitFor(t, "the failed order", func() bool { return m.Status().LastErrorCode != "" })

			st := m.Status()
			if st.LastErrorCode != CodeACMEFailed || !strings.Contains(st.LastError, "127.0.0.1:1") || st.LastErrorAt.Before(begin) {
				t.Errorf("Status = %+v", st)
			}
			if st.Ready || !slices.Equal(st.Names, []string{name}) || !st.NotAfter.IsZero() || !st.NextRenewal.IsZero() {
				t.Errorf("Status = %+v", st)
			}
			wantNotReady(t, m, "getting a certificate for "+name+" ("+CodeACMEFailed+")")
			if _, err := handshake(t, m, nil, name); err == nil {
				t.Error("a handshake succeeded without a certificate")
			}
			recs := sink.records(t, "could not get a certificate")
			if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["code"] != CodeACMEFailed || recs[0]["name"] != name ||
				recs[0]["component"] != "tls" || recs[0]["err"] == nil {
				t.Errorf("want one warning with the code, got %v", recs)
			}
			// certmagic's own lines arrive through the zap bridge, under the same component.
			if recs := sink.records(t, "obtaining certificate"); len(recs) == 0 || recs[0]["component"] != "tls" {
				t.Errorf("certmagic's log lines are missing or lack component=tls:\n%s", sink.String())
			}
		})
	}
}

// fakeResolver answers LookupNetIP from a table.
type fakeResolver struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
	calls []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, network+" "+host)
	return f.addrs, f.err
}

func (f *fakeResolver) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// TestDNSCheck: auto mode's look at its own domain (04 §8.7).
func TestDNSCheck(t *testing.T) {
	v4, v6 := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")
	notFound := &net.DNSError{Err: "no such host", Name: testDomain, IsNotFound: true}
	tests := []struct {
		name     string
		own4     netip.Addr
		own6     netip.Addr
		resolver *fakeResolver
		wantCode string
		wantFix  string
		noLookup bool
	}{
		{"points here (IPv4)", v4, netip.Addr{}, &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.7")}}, "", "", false},
		{"points here (IPv4-mapped answer)", v4, netip.Addr{}, &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("::ffff:203.0.113.7")}}, "", "", false},
		{"points here (IPv6 only)", v4, v6, &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("2001:db8::7")}}, "", "", false},
		{"one of several records", v4, netip.Addr{}, &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("198.51.100.4"), v4}}, "", "", false},
		{"points elsewhere", v4, v6, &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("198.51.100.4")}},
			CodeDNSWrong, "watch.example.com points to 198.51.100.4, but this server is 203.0.113.7.", false},
		{"does not resolve", v4, netip.Addr{}, &fakeResolver{err: notFound},
			CodeDNSMissing, "watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7.", false},
		{"no records", netip.Addr{}, v6, &fakeResolver{},
			CodeDNSWrong, "watch.example.com points to no address, but this server is 2001:db8::7.", false},
		{"the resolver fails", v4, netip.Addr{}, &fakeResolver{err: errors.New("server misbehaving")}, "", "", false},
		{"own address unknown", netip.Addr{}, netip.Addr{}, &fakeResolver{addrs: []netip.Addr{v4}}, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sink := newManager(t, Options{
				Mode: config.TLSAuto, Domain: testDomain, StorageDir: t.TempDir(),
				PublicIP: tc.own4, PublicIPv6: tc.own6, Resolver: tc.resolver,
			})
			m.checkDNS(context.Background())

			calls := tc.resolver.called()
			if tc.noLookup != (len(calls) == 0) || (len(calls) > 0 && calls[0] != "ip "+testDomain) {
				t.Errorf("lookups = %v", calls)
			}
			var warnings []map[string]any
			for _, rec := range sink.records(t, "the domain") {
				if rec["level"] == "WARN" {
					warnings = append(warnings, rec)
				}
			}
			if tc.wantCode == "" {
				if len(warnings) != 0 {
					t.Errorf("unexpected warning: %v", warnings)
				}
				return
			}
			if len(warnings) != 1 || warnings[0]["code"] != tc.wantCode || warnings[0]["fix"] != tc.wantFix {
				t.Errorf("warnings = %v\n want code %s, fix %q", warnings, tc.wantCode, tc.wantFix)
			}
		})
	}
}

// TestDNSCheckRunsBeforeTheFirstOrder: certmagic's "obtaining" event starts the check, once, and a renewal does not.
func TestDNSCheckRunsBeforeTheFirstOrder(t *testing.T) {
	resolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("198.51.100.4")}}
	m, sink := acmeManager(t, Options{
		Mode: config.TLSAuto, Domain: testDomain, PublicIP: netip.MustParseAddr("203.0.113.7"), Resolver: resolver,
	})
	// A renewal says nothing about DNS: the name worked before.
	if err := m.onEvent(context.Background(), eventObtaining, map[string]any{"renewal": true, "identifier": testDomain}); err != nil {
		t.Fatal(err)
	}
	start(t, m) // the real order, against a CA that is not there
	waitFor(t, "the DNS warning", func() bool { return len(sink.records(t, "does not point to this server")) > 0 })
	waitFor(t, "the failed order", func() bool { return m.Status().LastErrorCode != "" })
	// Another attempt of the same order.
	if err := m.onEvent(context.Background(), eventObtaining, map[string]any{"identifier": testDomain}); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := resolver.called(); len(calls) != 1 {
		t.Errorf("lookups = %v, want one", calls)
	}
	recs := sink.records(t, "does not point to this server")
	if len(recs) != 1 || recs[0]["code"] != CodeDNSWrong {
		t.Errorf("warnings = %v", recs)
	}
	// The hint is a log line only: Status keeps what the CA said.
	if st := m.Status(); st.LastErrorCode != CodeACMEFailed {
		t.Errorf("Status.LastErrorCode = %q", st.LastErrorCode)
	}
}

// TestOnEvent: what certmagic's events do to Status.
func TestOnEvent(t *testing.T) {
	m, sink := newManager(t, Options{
		Mode: config.TLSIP, PublicIP: netip.MustParseAddr("203.0.113.7"), StorageDir: t.TempDir(),
	})
	ctx := context.Background()
	failed := map[string]any{"renewal": true, "identifier": "203.0.113.7", "error": problem("connection", "203.0.113.7: Timeout during connect")}
	if err := m.onEvent(ctx, eventFailed, failed); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.LastErrorCode != CodeACMEUnreachable || !strings.HasPrefix(st.LastError, "Let's Encrypt couldn't connect to 203.0.113.7") || st.LastErrorAt.IsZero() {
		t.Errorf("Status after cert_failed = %+v", st)
	}
	if recs := sink.records(t, "could not renew the certificate"); len(recs) != 1 || recs[0]["code"] != CodeACMEUnreachable {
		t.Errorf("want the renewal warning, got:\n%s", sink.String())
	}

	if err := m.onEvent(ctx, eventObtained, map[string]any{"renewal": true, "identifier": "203.0.113.7"}); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.LastErrorCode != "" || st.LastError != "" || !st.LastErrorAt.IsZero() {
		t.Errorf("Status after cert_obtained = %+v", st)
	}
	if recs := sink.exact(t, "certificate obtained"); len(recs) != 1 || recs[0]["renewal"] != true {
		t.Errorf("want the info line, got:\n%s", sink.String())
	}

	// An event without an error value, and one the manager does not know.
	if err := m.onEvent(ctx, eventFailed, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.LastErrorCode != CodeACMEFailed {
		t.Errorf("Status = %+v", st)
	}
	if err := m.onEvent(ctx, "cert_ocsp_revoked", nil); err != nil {
		t.Fatal(err)
	}
}

// TestLifecycle: Start and Shutdown in every order.
func TestLifecycle(t *testing.T) {
	t.Run("shutdown before start", func(t *testing.T) {
		m, _ := acmeManager(t, Options{})
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := m.Start(context.Background()); !errors.Is(err, errStopped) {
			t.Errorf("Start after Shutdown = %v", err)
		}
	})
	t.Run("start twice, shutdown twice", func(t *testing.T) {
		m, _ := acmeManager(t, Options{})
		start(t, m)
		first := m.acme.Load()
		start(t, m)
		if m.acme.Load() != first {
			t.Error("the second Start built certmagic again")
		}
		for range 2 {
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		// The parts stay usable for the handshakes and requests still in flight.
		wantWaitingPage(t, "after Shutdown", serve(m.HTTPHandler(nil), "GET", "/", testDomain))
		_ = m.Status()
	})
	t.Run("the start context may end", func(t *testing.T) {
		m, _ := acmeManager(t, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		cancel() // it bounds the startup, not the manager's life
		waitFor(t, "the failed order", func() bool { return m.Status().LastErrorCode != "" })
		if m.ctx.Err() != nil {
			t.Error("the manager's own context ended with Start's")
		}
	})
	t.Run("shutdown honors its context", func(t *testing.T) {
		m, _ := acmeManager(t, Options{})
		start(t, m)
		// Something of the manager's that does not end by itself.
		release := make(chan struct{})
		m.mu.Lock()
		m.wg.Go(func() { <-release })
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown = %v, want the context's error", err)
		}
		close(release)
	})
}
