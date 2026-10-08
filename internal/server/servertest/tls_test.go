package servertest_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// TestStartTLS: Options.TLS gives a server in manual mode behind the 443 multiplexer, with a certificate from a CA
// that only this test knows, and a Client that trusts it (04 §8.6, §17, §18).
func TestStartTLS(t *testing.T) {
	begin := time.Now()
	srv := servertest.Start(t, servertest.Options{TLS: true})
	t.Logf("ready after %v", time.Since(begin))

	addrs := srv.Srv.Addrs()
	if addrs.HTTPS == nil {
		t.Fatal("no HTTPS listener")
	}
	_, port, _ := net.SplitHostPort(addrs.HTTPS.String())
	wantHost := servertest.TLSDomain + ":" + port
	if srv.URL != "https://"+wantHost || srv.WSURL != "wss://"+wantHost+"/ws" {
		t.Errorf("URL %q, WSURL %q; want https://%s and wss://%s/ws", srv.URL, srv.WSURL, wantHost, wantHost)
	}
	if site := srv.Srv.Site(); site.Origin != srv.URL || site.TLSMode != config.TLSManual || site.Dev {
		t.Errorf("site = %+v", site)
	}
	if srv.Roots == nil {
		t.Fatal("Roots is nil with Options.TLS")
	}

	// The config is the TLS test setup of 04 §17: manual mode on ephemeral loopback ports, and no STUN servers.
	c := srv.Cfg
	if c.EffectiveTLSMode() != config.TLSManual || c.Domain != servertest.TLSDomain || c.Listen.HTTPS != "127.0.0.1:0" ||
		c.Listen.HTTP != "127.0.0.1:0" || len(c.Network.STUNServers) != 0 {
		t.Errorf("config: tls %q, domain %q, listen %+v, stun %v", c.EffectiveTLSMode(), c.Domain, c.Listen, c.Network.STUNServers)
	}
	for _, f := range []string{c.TLS.CertFile, c.TLS.KeyFile} {
		if fi, err := os.Stat(f); err != nil || fi.Size() == 0 {
			t.Errorf("certificate file %q: %v", f, err)
		}
	}
	for _, k := range config.Keys() {
		if k.Policy && c.IsSet(k.Path) {
			t.Errorf("the harness sets the policy key %s", k.Path)
		}
	}

	// Client speaks HTTPS to the server, over HTTP/2, and checks its certificate against the test CA.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ready"`) || !strings.Contains(string(body), `"tls":"ok"`) {
		t.Errorf("/readyz = %d %s", res.StatusCode, body)
	}
	if res.ProtoMajor != 2 || res.TLS == nil || len(res.TLS.VerifiedChains) == 0 {
		t.Errorf("/readyz came over %s, verified chains: %v", res.Proto, res.TLS != nil && len(res.TLS.VerifiedChains) > 0)
	}

	// A client of the test's own verifies the server with Roots, under each name the certificate has. An IP
	// address sends no SNI; the server presents the same certificate.
	for _, name := range []string{servertest.TLSDomain, "localhost", "127.0.0.1"} {
		d := &tls.Dialer{Config: &tls.Config{RootCAs: srv.Roots, ServerName: name, MinVersion: tls.VersionTLS12}}
		conn, err := d.DialContext(ctx, "tcp", addrs.HTTPS.String())
		if err != nil {
			t.Errorf("TLS handshake as %q: %v", name, err)
			continue
		}
		_ = conn.Close()
	}
	// Nobody else trusts that CA.
	d := &tls.Dialer{Config: &tls.Config{ServerName: servertest.TLSDomain, MinVersion: tls.VersionTLS12}}
	if conn, err := d.DialContext(ctx, "tcp", addrs.HTTPS.String()); err == nil {
		_ = conn.Close()
		t.Error("a client with the system's roots trusts the test certificate")
	}

	// Client reaches the server whatever host the URL names, as in off mode: the Host check answers.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET / as localhost: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("GET / as localhost = %d, want the server's 421", res.StatusCode)
	}

	if !strings.Contains(srv.Logs(), " ready: "+srv.URL+" (tls=manual)") {
		t.Errorf("Logs() has no ready line:\n%s", srv.Logs())
	}
	if time.Since(begin) > 5*time.Second {
		t.Errorf("the TLS server took %v", time.Since(begin))
	}
}

// TestTLSRestartAndStop: Restart brings a TLS server back on both ports with the same certificate, and Stop leaves
// neither port listening.
func TestTLSRestartAndStop(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	before := srv.Srv.Addrs()
	url := srv.URL
	srv.Restart(t)
	after := srv.Srv.Addrs()
	if after.HTTPS.String() != before.HTTPS.String() || after.HTTP.String() != before.HTTP.String() || srv.URL != url {
		t.Errorf("after Restart: %v, %v, %s; before: %v, %v, %s", after.HTTPS, after.HTTP, srv.URL, before.HTTPS, before.HTTP, url)
	}
	if code, _ := status(t, srv, "/readyz"); code != 200 {
		t.Errorf("/readyz after Restart = %d", code)
	}
	srv.Stop(t)
	for _, a := range []net.Addr{after.HTTPS, after.HTTP} {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(context.Background(), "tcp", a.String()); err == nil {
			_ = c.Close()
			t.Errorf("%s still accepts after Stop", a)
		}
	}
}

// TestOffModeHasNoRoots: without Options.TLS nothing about the harness speaks TLS.
func TestOffModeHasNoRoots(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	if srv.Roots != nil || srv.Srv.Addrs().HTTPS != nil || !strings.HasPrefix(srv.URL, "http://") || !strings.HasPrefix(srv.WSURL, "ws://") {
		t.Errorf("off mode: Roots %v, HTTPS %v, URL %q, WSURL %q", srv.Roots != nil, srv.Srv.Addrs().HTTPS, srv.URL, srv.WSURL)
	}
	// No test asks the public STUN servers for this machine's address.
	if len(srv.Cfg.Network.STUNServers) != 0 {
		t.Errorf("STUN servers = %v", srv.Cfg.Network.STUNServers)
	}
}
