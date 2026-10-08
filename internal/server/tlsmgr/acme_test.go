package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// The ACME tests of 04 §17: the manager against a real ACME server, Pebble, with pebble-challtestsrv as its DNS.
// They run when both variables below are set, which is what the acme job of the CI workflow does; every other run
// skips them. The names are the env names of the config keys tls.acme_ca and tls.acme_ca_root, the two keys that
// point a real server at Pebble (06 §11.2, scenario V5).
//
// Pebble must run with testdata/pebble-config.json, which this file reads too: Pebble validates http-01 on port
// 5002 and tls-alpn-01 on port 5001, and the tests listen there on 127.0.0.1. So Pebble, challtestsrv and the tests
// have to share one loopback interface. On Linux, host networking does that:
//
//	docker run -d --name challtestsrv --network host ghcr.io/letsencrypt/pebble-challtestsrv \
//	    -defaultIPv4 127.0.0.1 -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh ""
//	docker run -d --name pebble --network host -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
//	    -v "$PWD/internal/server/tlsmgr/testdata/pebble-config.json:/test/config/isshoni.json:ro" \
//	    ghcr.io/letsencrypt/pebble -config /test/config/isshoni.json -dnsserver 127.0.0.1:8053
//	docker cp pebble:/test/certs/pebble.minica.pem /tmp/pebble.minica.pem
//	ISSHONI_TLS_ACME_CA=https://127.0.0.1:14000/dir ISSHONI_TLS_ACME_CA_ROOT=/tmp/pebble.minica.pem \
//	    go test -count=1 -run 'TestACME' -v ./internal/server/tlsmgr
//
// Where containers have no host networking (Docker Desktop), run the tests in a container too and put all three in
// one network namespace: start challtestsrv without --network, then pebble and a golang container with
// --network container:challtestsrv.
const (
	envACMECA     = "ISSHONI_TLS_ACME_CA"
	envACMECARoot = "ISSHONI_TLS_ACME_CA_ROOT"
)

// acmeReadyTimeout bounds an order, or a renewal, against Pebble. Either takes a second or two.
const acmeReadyTimeout = 90 * time.Second

// pebbleConfig is what the tests read from testdata/pebble-config.json.
type pebbleConfig struct {
	Pebble struct {
		ManagementListenAddress string `json:"managementListenAddress"`
		HTTPPort                int    `json:"httpPort"`
		TLSPort                 int    `json:"tlsPort"`
		Profiles                map[string]struct {
			ValidityPeriod int `json:"validityPeriod"` // seconds
		} `json:"profiles"`
	} `json:"pebble"`
}

// pebble is the ACME test server as the tests use it.
type pebble struct {
	directory string         // the ACME directory URL
	rootFile  string         // the PEM file trusted for Pebble's own HTTPS
	httpAddr  string         // where the tests listen for http-01
	tlsAddr   string         // where the tests listen for tls-alpn-01, and serve HTTPS
	issuing   *x509.CertPool // the root that Pebble issues from in this run
	lifetime  map[string]time.Duration
}

// requirePebble returns the ACME test server, or skips the test when the environment names none.
func requirePebble(t *testing.T) *pebble {
	t.Helper()
	directory, rootFile, ok := acmeTestCA()
	if !ok {
		t.Skipf("needs Pebble: set %s and %s (see acme_test.go)", envACMECA, envACMECARoot)
	}
	raw, err := os.ReadFile("testdata/pebble-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg pebbleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("testdata/pebble-config.json: %v", err)
	}
	p := &pebble{
		directory: directory,
		rootFile:  rootFile,
		httpAddr:  net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Pebble.HTTPPort)),
		tlsAddr:   net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Pebble.TLSPort)),
		lifetime:  map[string]time.Duration{},
	}
	for name, profile := range cfg.Pebble.Profiles {
		p.lifetime[name] = time.Duration(profile.ValidityPeriod) * time.Second
	}

	// Pebble makes a new issuing root at every start and serves it on its management port.
	dir, err := url.Parse(directory)
	if err != nil {
		t.Fatalf("%s: %v", envACMECA, err)
	}
	_, mgmtPort, err := net.SplitHostPort(cfg.Pebble.ManagementListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := loadRoots(rootFile)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(dir.Hostname(), mgmtPort)+"/roots/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("Pebble's management port: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	pemBytes, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("Pebble's issuing root: %d, %v", res.StatusCode, err)
	}
	p.issuing = x509.NewCertPool()
	if !p.issuing.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("Pebble's issuing root is not PEM: %q", pemBytes)
	}
	return p
}

// acmeServer is a manager behind real listeners, wired as internal/server wires it: the port 80 handler on a plain
// listener, and the TLS configuration on an http.Server behind the 443 multiplexer.
type acmeServer struct {
	m      *Manager
	sink   *logSink
	pebble *pebble
	stop   func()
}

// startACMEServer binds Pebble's two validation ports and starts a manager on them. tweak sets test seams before
// Start.
func (p *pebble) startACMEServer(t *testing.T, opts Options, tweak func(*Manager)) *acmeServer {
	t.Helper()
	var lc net.ListenConfig
	httpLn, err := lc.Listen(context.Background(), "tcp", p.httpAddr)
	if err != nil {
		t.Fatalf("the http-01 port: %v", err)
	}
	mux, err := netx.ListenPortMux(netx.PortMuxOptions{Addr: p.tlsAddr})
	if err != nil {
		_ = httpLn.Close()
		t.Fatalf("the tls-alpn-01 port: %v", err)
	}
	opts.CA, opts.CARootFile = p.directory, p.rootFile
	opts.HTTPAddr, opts.HTTPSAddr = httpLn.Addr().String(), mux.Addr().String()
	sink := &logSink{}
	opts.Logger = sink.logger()
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if tweak != nil {
		tweak(m)
	}
	plain := &http.Server{Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 5 * time.Second}
	main := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "hello over %s", r.Proto) //nolint:gosec // G705: a test handler; plain text
		}),
		TLSConfig:         m.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		// net/http's own lines (a failed handshake) go to the sink too.
		ErrorLog: slog.NewLogLogger(sink.logger().Handler(), slog.LevelDebug),
	}
	var serving sync.WaitGroup
	serving.Go(func() { _ = plain.Serve(httpLn) })
	serving.Go(func() { _ = main.ServeTLS(mux.TLS(), "", "") })
	// The 443 multiplexer's other half: nothing speaks ICE here.
	serving.Go(func() {
		for {
			c, err := mux.ICE().Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	})

	s := &acmeServer{m: m, sink: sink, pebble: p}
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := m.Shutdown(ctx); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			_ = plain.Close()
			_ = main.Close()
			_ = mux.Close()
			serving.Wait()
		})
	}
	t.Cleanup(func() {
		s.stop()
		if t.Failed() {
			t.Logf("manager log:\n%s", sink.String())
		}
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// waitReady waits for the certificate.
func (s *acmeServer) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(acmeReadyTimeout)
	for {
		ok, detail := s.m.Ready()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no certificate after %v: %s", acmeReadyTimeout, detail)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dial completes a TLS handshake with the server on its real port and returns the leaf it presented. An empty
// serverName sends no SNI and checks the chain alone.
func (s *acmeServer) dial(t *testing.T, serverName string, verifyName bool) *x509.Certificate {
	t.Helper()
	cfg := &tls.Config{RootCAs: s.pebble.issuing, ServerName: serverName, MinVersion: tls.VersionTLS12}
	if !verifyName {
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: the chain is verified below; the name is the test's subject
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: s.pebble.issuing, Intermediates: inter})
			return err
		}
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", s.pebble.tlsAddr)
	if err != nil {
		t.Fatalf("TLS handshake (server name %q): %v", serverName, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
}

// challengesServed returns how many challenge responses of each type certmagic logged.
func (s *acmeServer) challengesServed(t *testing.T) (http01, tlsALPN01 int) {
	t.Helper()
	for _, rec := range s.sink.records(t, "served key authentication") {
		switch rec["challenge"] {
		case "http-01":
			http01++
		case "tls-alpn-01":
			tlsALPN01++
		}
	}
	return http01, tlsALPN01
}

func near(a, b time.Time, slack time.Duration) bool {
	d := a.Sub(b)
	return d >= -slack && d <= slack
}

// TestACMEDomain: auto mode gets a certificate for a domain, through each of the two challenge types, and serves
// HTTPS with it on the port the challenge came in on.
func TestACMEDomain(t *testing.T) {
	p := requirePebble(t)
	// Auto mode asks for no profile, and gets the CA's default one. Pebble's idea of a default is a random profile,
	// so the tests ask for it by name.
	tests := []struct {
		challenge string
		domain    string
		tweak     func(*Manager)
	}{
		{"http-01", "http01.isshoni.test", func(m *Manager) { m.profile, m.noTLSALPN01 = "default", true }},
		{"tls-alpn-01", "alpn.isshoni.test", func(m *Manager) { m.profile, m.noHTTP01 = "default", true }},
	}
	for _, tc := range tests {
		t.Run(tc.challenge, func(t *testing.T) {
			storage := t.TempDir()
			// The domain exists in challtestsrv only, so the manager's own DNS check gets its answer from here.
			resolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
			opts := Options{
				Mode: config.TLSAuto, Domain: tc.domain, Email: "admin@isshoni.test", StorageDir: storage,
				PublicIP: netip.MustParseAddr("127.0.0.1"), Resolver: resolver,
				Site: testSite(config.TLSAuto, tc.domain+":"+portOf(p.tlsAddr)),
			}
			begin := time.Now()
			s := p.startACMEServer(t, opts, tc.tweak)

			// Until the certificate is there, port 80 says so.
			if code, _, _ := s.plainGet(t, "/", tc.domain); code != http.StatusServiceUnavailable && code != http.StatusPermanentRedirect {
				t.Errorf("port 80 during the order = %d, want the waiting page (or the redirect, if the order was that quick)", code)
			}
			s.waitReady(t)
			t.Logf("certificate for %s after %v", tc.domain, time.Since(begin).Round(time.Millisecond))

			leaf := s.dial(t, tc.domain, true)
			if !slices.Equal(leaf.DNSNames, []string{tc.domain}) || len(leaf.IPAddresses) != 0 {
				t.Errorf("certificate names %v %v, want %s alone", leaf.DNSNames, leaf.IPAddresses, tc.domain)
			}
			http01, tlsALPN01 := s.challengesServed(t)
			if tc.challenge == "http-01" && (http01 == 0 || tlsALPN01 != 0) || tc.challenge == "tls-alpn-01" && (tlsALPN01 == 0 || http01 != 0) {
				t.Errorf("challenges served: %d http-01, %d tls-alpn-01; want %s only", http01, tlsALPN01, tc.challenge)
			}

			st := s.m.Status()
			if !st.Ready || !slices.Equal(st.Names, []string{tc.domain}) || !strings.HasPrefix(st.Issuer, "Pebble") ||
				st.LastError != "" || st.LastErrorCode != "" {
				t.Errorf("Status = %+v", st)
			}
			if !st.NotAfter.Equal(leaf.NotAfter) || !st.NotBefore.Equal(leaf.NotBefore) {
				t.Errorf("Status validity %v to %v, the served certificate's is %v to %v", st.NotBefore, st.NotAfter, leaf.NotBefore, leaf.NotAfter)
			}
			lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
			if want := p.lifetime["default"]; lifetime < want-time.Minute || lifetime > want+time.Minute {
				t.Errorf("certificate lifetime %v, want the default profile's %v", lifetime, want)
			}
			if want := leaf.NotAfter.Add(-lifetime / 3); !near(st.NextRenewal, want, time.Second) {
				t.Errorf("NextRenewal = %v, want %v (the last third of the lifetime)", st.NextRenewal, want)
			}
			if len(s.sink.exact(t, "certificate obtained")) != 1 {
				t.Errorf("want one \"certificate obtained\" line")
			}
			// The DNS check ran before the order and found the domain pointing here.
			if calls := resolver.called(); len(calls) != 1 || calls[0] != "ip "+tc.domain {
				t.Errorf("DNS lookups = %v, want one for the domain", calls)
			}
			if n := len(s.sink.records(t, "the domain does not")); n != 0 {
				t.Errorf("%d DNS warnings for a domain that points here", n)
			}

			// HTTPS over HTTP/2 and over HTTP/1.1 on the multiplexed port.
			for _, proto := range []string{"HTTP/2.0", "HTTP/1.1"} {
				if got := s.httpsGet(t, tc.domain, proto == "HTTP/2.0"); got != "hello over "+proto {
					t.Errorf("GET https://%s/ = %q, want %q", tc.domain, got, "hello over "+proto)
				}
			}
			// Port 80 now sends people to HTTPS, and still knows no challenge of its own making.
			code, location, _ := s.plainGet(t, "/r/lounge?x=1", "whatever.example")
			if code != http.StatusPermanentRedirect || location != opts.Site.Origin+"/r/lounge?x=1" {
				t.Errorf("port 80 with a certificate = %d → %q", code, location)
			}
			if code, _, _ := s.plainGet(t, acmeChallengePrefix+"nope", tc.domain); code != http.StatusNotFound {
				t.Errorf("an unknown challenge = %d, want 404", code)
			}

			// A restart finds the certificate in its storage: ready at once, and no new order.
			s.stop()
			again := p.startACMEServer(t, opts, tc.tweak)
			again.waitReady(t)
			if got := again.dial(t, tc.domain, true); got.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
				t.Errorf("after a restart the server has certificate %v, want the stored %v", got.SerialNumber, leaf.SerialNumber)
			}
			if n := len(again.sink.exact(t, "certificate obtained")); n != 0 {
				t.Errorf("the restarted manager ordered %d new certificates", n)
			}
		})
	}
}

// TestACMEIP: ip mode gets a short-lived certificate for an IP address identifier (RFC 8738), through each of the
// two challenge types, and a client that sends no SNI gets it.
func TestACMEIP(t *testing.T) {
	p := requirePebble(t)
	ip := netip.MustParseAddr("127.0.0.1")
	tests := []struct {
		challenge string
		tweak     func(*Manager)
	}{
		{"http-01", func(m *Manager) { m.noTLSALPN01 = true }},
		{"tls-alpn-01", func(m *Manager) { m.noHTTP01 = true }},
	}
	for _, tc := range tests {
		t.Run(tc.challenge, func(t *testing.T) {
			s := p.startACMEServer(t, Options{
				Mode: config.TLSIP, PublicIP: ip, StorageDir: t.TempDir(),
				Site: testSite(config.TLSIP, p.tlsAddr),
			}, tc.tweak)
			s.waitReady(t)

			// No SNI: Go's client sends none for an IP literal, and checks the IP SAN.
			leaf := s.dial(t, "127.0.0.1", true)
			if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.IPv4(127, 0, 0, 1)) {
				t.Errorf("certificate names %v %v, want the IP address alone", leaf.DNSNames, leaf.IPAddresses)
			}
			if got := s.dial(t, "", false); got.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
				t.Errorf("a handshake without SNI got certificate %v, want %v", got.SerialNumber, leaf.SerialNumber)
			}
			// A client that asks for some other name gets the same certificate, and with it a clear name mismatch
			// instead of a failed handshake.
			if got := s.dial(t, "other.example", false); got.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
				t.Errorf("a handshake for another name got certificate %v, want %v", got.SerialNumber, leaf.SerialNumber)
			}
			http01, tlsALPN01 := s.challengesServed(t)
			if tc.challenge == "http-01" && (http01 == 0 || tlsALPN01 != 0) || tc.challenge == "tls-alpn-01" && (tlsALPN01 == 0 || http01 != 0) {
				t.Errorf("challenges served: %d http-01, %d tls-alpn-01; want %s only", http01, tlsALPN01, tc.challenge)
			}

			// The shortlived profile was asked for.
			lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
			if want := p.lifetime[shortlivedProfile]; lifetime < want-2*time.Second || lifetime > want+2*time.Second {
				t.Errorf("certificate lifetime %v, want the shortlived profile's %v", lifetime, want)
			}
			st := s.m.Status()
			if !st.Ready || !slices.Equal(st.Names, []string{"127.0.0.1"}) || st.LastErrorCode != "" {
				t.Errorf("Status = %+v", st)
			}
			if want := leaf.NotAfter.Add(-lifetime / 2); !near(st.NextRenewal, want, time.Second) {
				t.Errorf("NextRenewal = %v, want %v (half the lifetime)", st.NextRenewal, want)
			}
			if got := s.httpsGet(t, "127.0.0.1", true); got != "hello over HTTP/2.0" {
				t.Errorf("GET https://127.0.0.1/ = %q", got)
			}
		})
	}
}

// TestACMERenewal: a certificate with a shortened lifetime is renewed in the background and the new one serves
// without a restart.
func TestACMERenewal(t *testing.T) {
	p := requirePebble(t)
	s := p.startACMEServer(t, Options{
		Mode: config.TLSIP, PublicIP: netip.MustParseAddr("127.0.0.1"), StorageDir: t.TempDir(),
		Site: testSite(config.TLSIP, p.tlsAddr),
	}, func(m *Manager) {
		m.renewCheckInterval = time.Second // certmagic looks every 10 minutes otherwise
	})
	s.waitReady(t)
	first := s.dial(t, "127.0.0.1", true)
	begin := time.Now()

	var renewed *x509.Certificate
	deadline := begin.Add(p.lifetime[shortlivedProfile] + 30*time.Second)
	for renewed == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no renewal within %v of a certificate that lives %v", time.Since(begin).Round(time.Second), p.lifetime[shortlivedProfile])
		}
		time.Sleep(250 * time.Millisecond)
		if leaf := s.dial(t, "127.0.0.1", true); leaf.SerialNumber.Cmp(first.SerialNumber) != 0 {
			renewed = leaf
		}
	}
	t.Logf("renewed after %v", time.Since(begin).Round(time.Millisecond))
	if !renewed.NotAfter.After(first.NotAfter) {
		t.Errorf("the renewed certificate ends %v, not after the first one's %v", renewed.NotAfter, first.NotAfter)
	}
	if ok, detail := s.m.Ready(); !ok {
		t.Errorf("not ready after the renewal: %s", detail)
	}
	st := s.m.Status()
	if !st.Ready || st.NotAfter.Before(renewed.NotAfter) || st.LastErrorCode != "" {
		t.Errorf("Status = %+v, want the renewed certificate (ends %v)", st, renewed.NotAfter)
	}
	var renewals int
	for _, rec := range s.sink.exact(t, "certificate obtained") {
		if rec["renewal"] == true {
			renewals++
		}
	}
	if renewals == 0 {
		t.Errorf("no \"certificate obtained\" line with renewal=true")
	}
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// plainGet sends a plain-HTTP request to the port 80 listener.
func (s *acmeServer) plainGet(t *testing.T, target, host string) (status int, location, body string) {
	t.Helper()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.pebble.httpAddr+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s on port 80: %v", target, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Location"), string(b)
}

// httpsGet fetches / over HTTPS for host from the multiplexed port, over HTTP/2 or HTTP/1.1.
func (s *acmeServer) httpsGet(t *testing.T, host string, h2 bool) string {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: s.pebble.issuing, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", s.pebble.tlsAddr)
		},
		ForceAttemptHTTP2: h2,
	}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // HTTP/1.1 only
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("GET https://%s/: %v", host, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET https://%s/ = %d, %v", host, res.StatusCode, err)
	}
	return string(b)
}

// TestACMEHelpers keeps the helpers above honest without Pebble.
func TestACMEHelpers(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	if !near(at, at.Add(time.Second), time.Second) || near(at, at.Add(2*time.Second), time.Second) || !near(at.Add(time.Second), at, time.Second) {
		t.Error("near is off")
	}
	if portOf("127.0.0.1:5001") != "5001" || portOf("nothing") != "" {
		t.Error("portOf is off")
	}
	var cfg pebbleConfig
	raw, err := os.ReadFile("testdata/pebble-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// What acme_test.go and the CI job rely on: the two validation ports, and the two profiles the manager asks for
	// ("" in auto mode is Pebble's "default").
	if cfg.Pebble.HTTPPort == 0 || cfg.Pebble.TLSPort == 0 || cfg.Pebble.ManagementListenAddress == "" {
		t.Errorf("testdata/pebble-config.json lacks a port: %+v", cfg.Pebble)
	}
	for _, profile := range []string{"default", shortlivedProfile} {
		if cfg.Pebble.Profiles[profile].ValidityPeriod <= 0 {
			t.Errorf("testdata/pebble-config.json has no %q profile", profile)
		}
	}
}
