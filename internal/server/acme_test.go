package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// The ACME test of the whole server (04 §17): `serve` in auto mode against Pebble. Like the TLS manager's own ACME
// tests (tlsmgr/acme_test.go, which says how to run Pebble) it runs when both variables are set, as in the acme job
// of the CI workflow, and it listens on the two ports of tlsmgr/testdata/pebble-config.json: run the two packages
// one after the other (go test -p 1).
const (
	envACMECA     = "ISSHONI_TLS_ACME_CA"
	envACMECARoot = "ISSHONI_TLS_ACME_CA_ROOT"
)

// acmeTestCA returns the ACME test server that the environment names.
func acmeTestCA() (directory, rootFile string, ok bool) {
	directory, rootFile = os.Getenv(envACMECA), os.Getenv(envACMECARoot)
	return directory, rootFile, directory != "" && rootFile != ""
}

// pebbleSetup reads what the test needs from Pebble's configuration and from Pebble itself: the two ports it
// validates on, and the root it issues from in this run (a new one at every start, served on its management port).
func pebbleSetup(t *testing.T, directory, rootFile string) (httpAddr, tlsAddr string, issuing *x509.CertPool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("tlsmgr", "testdata", "pebble-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Pebble struct {
			ManagementListenAddress string `json:"managementListenAddress"`
			HTTPPort                int    `json:"httpPort"`
			TLSPort                 int    `json:"tlsPort"`
		} `json:"pebble"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	httpAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Pebble.HTTPPort))
	tlsAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Pebble.TLSPort))

	dir, err := url.Parse(directory)
	if err != nil {
		t.Fatalf("%s: %v", envACMECA, err)
	}
	_, mgmtPort, err := net.SplitHostPort(cfg.Pebble.ManagementListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, err := os.ReadFile(rootFile)
	if err != nil {
		t.Fatalf("%s: %v", envACMECARoot, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("%s holds no PEM certificate", envACMECARoot)
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
	issuingPEM, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("Pebble's issuing root: %d, %v", res.StatusCode, err)
	}
	issuing = x509.NewCertPool()
	if !issuing.AppendCertsFromPEM(issuingPEM) {
		t.Fatalf("Pebble's issuing root is not PEM: %q", issuingPEM)
	}
	return httpAddr, tlsAddr, issuing
}

// TestACMEServe: a server in auto mode orders its certificate from an ACME CA through its own listeners (the 443
// multiplexer for tls-alpn-01, the plain port for http-01), becomes ready, serves HTTPS with it and keeps it in its
// data directory.
func TestACMEServe(t *testing.T) {
	directory, rootFile, ok := acmeTestCA()
	if !ok {
		t.Skipf("needs Pebble: set %s and %s (see tlsmgr/acme_test.go)", envACMECA, envACMECARoot)
	}
	httpAddr, tlsAddr, issuing := pebbleSetup(t, directory, rootFile)
	const domain = "serve.isshoni.test"

	begin := time.Now()
	srv := servertest.Start(t, servertest.Options{
		Roots: issuing,
		Flags: []string{
			"--tls.mode=auto", "--domain=" + domain,
			"--tls.acme-ca=" + directory, "--tls.acme-ca-root=" + rootFile,
			"--listen.https=" + tlsAddr, "--listen.http=" + httpAddr,
		},
		Deps: server.Deps{SPA: testSPA()},
	})
	t.Logf("ready with a certificate after %v", time.Since(begin).Round(time.Millisecond))

	_, port, _ := net.SplitHostPort(tlsAddr)
	if site := srv.Srv.Site(); site.Origin != "https://"+domain+":"+port || site.TLSMode != config.TLSAuto {
		t.Errorf("site = %+v", site)
	}
	if srv.URL != "https://"+domain+":"+port {
		t.Errorf("URL = %q", srv.URL)
	}

	// The web app over HTTP/2, with a certificate that the CA issued for the domain.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != indexHTML || res.ProtoMajor != 2 {
		t.Errorf("GET / = %d over %s, %q", res.StatusCode, res.Proto, body)
	}
	leaf := res.TLS.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain || !strings.HasPrefix(leaf.Issuer.CommonName, "Pebble") {
		t.Errorf("certificate for %v from %q, want one for %s from Pebble", leaf.DNSNames, leaf.Issuer.CommonName, domain)
	}

	if res := get(t, srv, "/readyz"); res.status != 200 || !strings.Contains(res.body, `"tls":"ok"`) {
		t.Errorf("/readyz = %d %s", res.status, res.body)
	}
	st := srv.Srv.TLSManager().Status()
	if st.Mode != config.TLSAuto || !st.Ready || len(st.Names) != 1 || st.Names[0] != domain ||
		!strings.HasPrefix(st.Issuer, "Pebble") || !st.NotAfter.Equal(leaf.NotAfter) || st.NextRenewal.IsZero() || st.LastErrorCode != "" {
		t.Errorf("TLS status = %+v", st)
	}
	// With the certificate there, the plain port redirects.
	if res := plainGet(t, srv.Srv, "/r/lounge", domain); res.status != http.StatusPermanentRedirect || res.header.Get("Location") != srv.URL+"/r/lounge" {
		t.Errorf("plain-HTTP GET = %d → %q", res.status, res.header.Get("Location"))
	}
	// The certificate and the ACME account are in the data directory, where a backup finds them (04 §5.1).
	certs, _ := filepath.Glob(filepath.Join(srv.DataDir, "certmagic", "certificates", "*", domain, domain+".crt"))
	accounts, _ := filepath.Glob(filepath.Join(srv.DataDir, "certmagic", "acme", "*", "users", "*", "*.key"))
	if len(certs) != 1 || len(accounts) != 1 {
		t.Errorf("certmagic storage: certificates %v, account keys %v", certs, accounts)
	}
	// certmagic answered the challenge through the server's own listeners, and logged it as the server's.
	var served bool
	for _, rec := range logRecords(t, srv.Logs()) {
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "served key authentication") {
			served = rec["component"] == "tls"
		}
	}
	if !served {
		t.Errorf("no challenge was served through the server's listeners:\n%s", srv.Logs())
	}

	// A restart finds the certificate in storage and is ready again.
	srv.Restart(t)
	if res := get(t, srv, "/readyz"); res.status != 200 {
		t.Errorf("/readyz after a restart = %d %s", res.status, res.body)
	}
}
