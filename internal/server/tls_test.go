package server_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
	"github.com/MoonWX/isshoni/internal/version"
)

// The TLS modes in the server (04 §8, §17; README S44). servertest.Options.TLS is manual mode with a private CA;
// the modes that talk to a CA run against a closed port here, and against Pebble in acme_test.go.

// wsRecord is what the stub /ws handler saw.
type wsRecord struct {
	mu    sync.Mutex
	proto string
	alpn  string
	tls   bool
}

// wsStub stands in for 01's hub at /ws (Deps.WS): it accepts the WebSocket and echoes one message, so the test sees
// the protocol and the frames of the connection itself. TestWiringOverTLS runs the real hub over the same path.
func wsStub(t *testing.T, rec *wsRecord) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.proto = r.Proto
		if r.TLS != nil {
			rec.tls, rec.alpn = true, r.TLS.NegotiatedProtocol
		}
		rec.mu.Unlock()
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("stub /ws: %v", err)
			return
		}
		defer func() { _ = ws.CloseNow() }()
		kind, msg, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		_ = ws.Write(r.Context(), kind, append([]byte("echo:"), msg...))
		_ = ws.Close(websocket.StatusNormalClosure, "")
	})
}

// stunBindingFrame is an ICE-TCP client's first bytes: a STUN Binding request in RFC 4571 framing (a 16-bit length,
// then the message).
func stunBindingFrame() []byte {
	const headerLen = 20 // a STUN message without attributes
	frame := make([]byte, 2+headerLen)
	binary.BigEndian.PutUint16(frame[0:], headerLen)  // RFC 4571: the length of what follows
	binary.BigEndian.PutUint16(frame[2:], 0x0001)     // Binding request
	binary.BigEndian.PutUint16(frame[4:], 0)          // no attributes
	binary.BigEndian.PutUint32(frame[6:], 0x2112A442) // magic cookie
	copy(frame[10:], "isshoni-test")                  // transaction ID
	return frame
}

// TestTLSOnePort is the check of 04 §17 for the TLS path: with servertest.Options{TLS: true}, WSS signaling over
// HTTP/1.1, the web app over HTTP/2 and ICE-TCP share the one port behind the 443 multiplexer. ICE-TCP is a raw
// STUN frame here, to see where the multiplexer sends it (media over this port: internal/server/itest); /ws is a
// stub that records how the connection arrived.
func TestTLSOnePort(t *testing.T) {
	rec := &wsRecord{}
	srv := servertest.Start(t, servertest.Options{TLS: true, Deps: server.Deps{SPA: testSPA(), WS: wsStub(t, rec)}})
	addrs := srv.Srv.Addrs()
	if addrs.HTTPS == nil || addrs.HTTP == nil || addrs.HTTPS.String() == addrs.HTTP.String() {
		t.Fatalf("Addrs = %+v, want the 443 multiplexer and the plain-HTTP port", addrs)
	}
	_, port, _ := net.SplitHostPort(addrs.HTTPS.String())
	site := srv.Srv.Site()
	wantHost := servertest.TLSDomain + ":" + port
	if site.Origin != "https://"+wantHost || site.Host != wantHost || site.TLSMode != config.TLSManual || site.Dev {
		t.Errorf("site = %+v, want https://%s in manual mode", site, wantHost)
	}
	if srv.URL != site.Origin || srv.WSURL != "wss://"+wantHost+"/ws" {
		t.Errorf("URL %q, WSURL %q", srv.URL, srv.WSURL)
	}

	// The web app over HTTP/2.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/r/lounge", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET /r/lounge: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != indexHTML {
		t.Errorf("GET /r/lounge = %d %q, want the web app", res.StatusCode, body)
	}
	if res.ProtoMajor != 2 || res.TLS == nil || res.TLS.NegotiatedProtocol != "h2" {
		t.Errorf("GET /r/lounge came over %s (ALPN %q), want HTTP/2", res.Proto, alpnOf(res.TLS))
	}
	if res.TLS != nil && res.TLS.Version < tls.VersionTLS12 {
		t.Errorf("TLS version %#x", res.TLS.Version)
	}
	// A domain over TLS: HSTS, and the WebSocket origin in the CSP.
	if got := res.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security = %q", got)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' wss://"+wantHost) {
		t.Errorf("CSP = %q, want connect-src with wss://%s", csp, wantHost)
	}

	// WSS signaling on the same port, over HTTP/1.1 Upgrade: Go's HTTP/2 server offers no extended CONNECT, so a
	// browser opens an HTTP/1.1 connection for it (04 §7.2), and so does the client here.
	ws, wsRes, err := websocket.Dial(ctx, srv.WSURL, &websocket.DialOptions{HTTPClient: srv.Client})
	if wsRes != nil && wsRes.Body != nil { // nil after a successful Dial
		defer func() { _ = wsRes.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("WSS: %v", err)
	}
	if wsRes.StatusCode != http.StatusSwitchingProtocols || wsRes.ProtoMajor != 1 {
		t.Errorf("WSS handshake = %d over %s, want 101 over HTTP/1.1", wsRes.StatusCode, wsRes.Proto)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := ws.Read(ctx); err != nil || string(msg) != "echo:hello" {
		t.Errorf("WSS echo = %q, %v", msg, err)
	}
	_ = ws.CloseNow()
	rec.mu.Lock()
	if rec.proto != "HTTP/1.1" || !rec.tls || rec.alpn == "h2" {
		t.Errorf("the /ws handler saw %s, TLS %v, ALPN %q; want HTTP/1.1 over TLS", rec.proto, rec.tls, rec.alpn)
	}
	rec.mu.Unlock()

	// ICE-TCP on the same port: a first byte of 0x00 is no TLS record, so the multiplexer routes the connection to
	// its ICE side, which the SFU's Transport reads (its ICE-TCP mux; media over this port is the test of
	// internal/server/itest). This Binding request names no ICE session, so the mux has no PeerConnection to hand it
	// to and closes the connection. What must not come back is a TLS alert or the plain-HTTP hint: nothing does.
	mux := srv.Srv.PortMux()
	before := mux.Stats()
	if before.TLS < 2 || before.ICE != 0 {
		t.Errorf("multiplexer before the ICE connection: %+v, want two or more TLS connections and no ICE one", before)
	}
	ice := dialRaw(t, addrs.HTTPS)
	if _, err := ice.Write(stunBindingFrame()); err != nil {
		t.Fatal(err)
	}
	_ = ice.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := ice.Read(make([]byte, 64)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("the ICE connection read %d bytes, %v; want the ICE-TCP mux to close it without a byte", n, err)
	}
	after := mux.Stats()
	if after.ICE != 1 || after.TLS != before.TLS || after.PlainHTTP != 0 || after.Garbage != 0 {
		t.Errorf("multiplexer after the ICE connection: %+v (before: %+v)", after, before)
	}

	// HTTPS still works next to it.
	if res := get(t, srv, "/healthz"); res.status != 200 {
		t.Errorf("/healthz = %d", res.status)
	}

	// Someone who types http://host:443 gets the hint, not a TLS error.
	plain := dialRaw(t, addrs.HTTPS)
	if _, err := io.WriteString(plain, "GET / HTTP/1.1\r\nHost: "+wantHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	hint, err := http.ReadResponse(bufio.NewReader(plain), nil)
	if err != nil {
		t.Fatalf("plain HTTP on the TLS port: %v", err)
	}
	hintBody, _ := io.ReadAll(hint.Body)
	_ = hint.Body.Close()
	if hint.StatusCode != http.StatusBadRequest || !strings.Contains(string(hintBody), "This port speaks HTTPS. Use https://") {
		t.Errorf("plain HTTP on the TLS port = %d %q", hint.StatusCode, hintBody)
	}

	// The ready line names the site, both listeners and the media ports: ICE-TCP is on the HTTPS port too.
	want := fmt.Sprintf("isshoni %s ready: %s (tls=manual) media udp/%d ice-tcp %s,%d", version.Version(), srv.URL,
		addrs.ICEUDP.(*net.UDPAddr).Port, port, addrs.ICETCP.(*net.TCPAddr).Port)
	var found bool
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["msg"] == want {
			found = rec["listen"] == addrs.HTTPS.String() && rec["listen_http"] == addrs.HTTP.String()
		}
	}
	if !found {
		t.Errorf("no ready line %q with both listeners:\n%s", want, srv.Logs())
	}
}

func alpnOf(cs *tls.ConnectionState) string {
	if cs == nil {
		return ""
	}
	return cs.NegotiatedProtocol
}

// tryDial connects to addr, or fails within a second.
func tryDial(addr net.Addr) (net.Conn, error) {
	d := net.Dialer{Timeout: time.Second}
	return d.DialContext(context.Background(), "tcp", addr.String())
}

// dialRaw opens a TCP connection to addr.
func dialRaw(t *testing.T, addr net.Addr) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(context.Background(), "tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	return c
}

// plainGet sends one request to the server's plain-HTTP port (listen.http) without following its redirect.
func plainGet(t *testing.T, s *server.Server, target, host string) response {
	t.Helper()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Addrs().HTTP.String()+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s on the plain-HTTP port: %v", target, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: string(b)}
}

// TestTLSPlainPortRedirects: in a TLS mode, listen.http redirects to the site's origin, whatever Host the request
// names, and never serves the app (04 §8.3).
func TestTLSPlainPortRedirects(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true, Deps: server.Deps{SPA: testSPA()}})
	for _, host := range []string{"", servertest.TLSDomain, "evil.example"} {
		res := plainGet(t, srv.Srv, "/invite/abc?x=1", host)
		if res.status != http.StatusPermanentRedirect || res.header.Get("Location") != srv.URL+"/invite/abc?x=1" {
			t.Errorf("Host %q: plain-HTTP GET = %d → %q, want 308 → %s/invite/abc?x=1", host, res.status, res.header.Get("Location"), srv.URL)
		}
		if strings.Contains(res.body, "<div id=root>") || res.header.Get("Strict-Transport-Security") != "" {
			t.Errorf("Host %q: the plain-HTTP port served the app or sent HSTS: %q", host, res.body)
		}
	}
	// Health is the HTTPS port's business: the plain port redirects that too.
	if res := plainGet(t, srv.Srv, "/readyz", ""); res.status != http.StatusPermanentRedirect {
		t.Errorf("plain-HTTP /readyz = %d, want 308", res.status)
	}

	hs := srv.Srv.PlainServer()
	if hs.ReadHeaderTimeout != 5*time.Second || hs.IdleTimeout != 30*time.Second || hs.MaxHeaderBytes != 16<<10 {
		t.Errorf("port 80 server: ReadHeaderTimeout %v, IdleTimeout %v, MaxHeaderBytes %d (04 §7.8)", hs.ReadHeaderTimeout, hs.IdleTimeout, hs.MaxHeaderBytes)
	}
	if hs.ReadTimeout == 0 || hs.WriteTimeout == 0 {
		t.Error("port 80 server: an exchange there has no reason to last, yet nothing bounds it")
	}
	// The main server keeps its limits behind TLS: ReadHeaderTimeout is the handshake's limit too.
	if main := srv.Srv.MainServer(); main.ReadHeaderTimeout != 10*time.Second || main.ReadTimeout != 0 || main.WriteTimeout != 0 || main.TLSConfig == nil {
		t.Errorf("main server: ReadHeaderTimeout %v, ReadTimeout %v, WriteTimeout %v, TLSConfig %v", main.ReadHeaderTimeout, main.ReadTimeout, main.WriteTimeout, main.TLSConfig != nil)
	}
}

// TestTLSReadinessAndHostCheck: the readiness check "tls" is the TLS manager's, and the router treats the TLS
// modes as 04 §8.5 says: the Host must be the site's, and forwarding headers count for nothing.
func TestTLSReadinessAndHostCheck(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true, Deps: server.Deps{SPA: testSPA()}})

	ready := get(t, srv, "/readyz")
	if ready.status != 200 || !strings.Contains(ready.body, `"tls":"ok"`) || strings.Contains(ready.body, "public_ip") {
		t.Errorf("/readyz = %d %s, want ready with the tls check (and no public_ip check: the site is a domain)", ready.status, ready.body)
	}
	st := srv.Srv.TLSManager().Status()
	if st.Mode != config.TLSManual || !st.Ready || st.Issuer != "isshoni servertest Test Root" || st.LastErrorCode != "" {
		t.Errorf("TLS status = %+v", st)
	}
	if len(st.Names) != 4 || st.Names[0] != servertest.TLSDomain {
		t.Errorf("certificate names = %v", st.Names)
	}

	if res := get(t, srv, "/", "Host", "evil.example"); res.status != http.StatusMisdirectedRequest {
		t.Errorf("another Host = %d, want 421", res.status)
	}
	// A client's X-Forwarded-For is not believed: it still counts as this machine, and /readyz shows it the checks
	// only when it sends none (ops.Health hides them from a request with a forwarding header).
	if res := get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor, "X-Forwarded-Proto", "http"); res.status != 200 || strings.Contains(res.body, "checks") {
		t.Errorf("/readyz with forwarding headers = %d %s", res.status, res.body)
	}
	// The servertest log has no warning of the TLS manager's: the test certificate covers the site and has time left.
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["component"] == "tls" && rec["level"] != "INFO" && rec["level"] != "DEBUG" {
			t.Errorf("unexpected line of the TLS manager: %v", rec)
		}
	}
}

// TestTLSRestartKeepsPorts: a restarted TLS server is back on both ports.
func TestTLSRestartKeepsPorts(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	before := srv.Srv.Addrs()
	url := srv.URL
	srv.Restart(t)
	after := srv.Srv.Addrs()
	if after.HTTPS.String() != before.HTTPS.String() || after.HTTP.String() != before.HTTP.String() || srv.URL != url {
		t.Errorf("after the restart: %v and %v (%s), before: %v and %v (%s)", after.HTTPS, after.HTTP, srv.URL, before.HTTPS, before.HTTP, url)
	}
	if res := get(t, srv, "/healthz"); res.status != 200 {
		t.Errorf("/healthz after the restart = %d", res.status)
	}
	if res := plainGet(t, srv.Srv, "/", ""); res.status != http.StatusPermanentRedirect {
		t.Errorf("plain-HTTP port after the restart = %d", res.status)
	}
}

// testCert writes a certificate for names, from a CA of its own, into dir and returns that CA.
func testCert(t *testing.T, certFile, keyFile string, names ...string) *x509.CertPool {
	t.Helper()
	fail := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	fail(err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Another Test Root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	fail(err)
	ca, err := x509.ParseCertificate(caDER)
	fail(err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	fail(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	fail(err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	fail(err)
	fail(os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	fail(os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

// handshakeWith completes a TLS handshake with the server's HTTPS port, trusting roots.
func handshakeWith(s *server.Server, roots *x509.CertPool, serverName string) (*x509.Certificate, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", s.Addrs().HTTPS.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0], nil
}

// TestTLSManualReload: the running server serves the certificate that the TLS manager has loaded last, and keeps
// the old one when the new files are no pair (04 §8.4). The manager's own tests cover the 60 s poll; this one
// reloads as SIGHUP will (04 §6.5; README S90 wires the signal).
func TestTLSManualReload(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	first, err := handshakeWith(srv.Srv, srv.Roots, servertest.TLSDomain)
	if err != nil {
		t.Fatalf("handshake with the test certificate: %v", err)
	}
	certFile, keyFile := srv.Cfg.TLS.CertFile, srv.Cfg.TLS.KeyFile
	oldKey, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	// A renewed certificate, from another CA.
	newRoots := testCert(t, certFile, keyFile, servertest.TLSDomain)
	if err := srv.Srv.TLSManager().Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	second, err := handshakeWith(srv.Srv, newRoots, servertest.TLSDomain)
	if err != nil {
		t.Fatalf("handshake after the reload, trusting the new CA: %v", err)
	}
	if second.Issuer.CommonName != "Another Test Root" || second.Equal(first) {
		t.Errorf("after the reload the server presents %q", second.Issuer.CommonName)
	}
	if _, err := handshakeWith(srv.Srv, srv.Roots, servertest.TLSDomain); err == nil {
		t.Error("a handshake that trusts only the old CA still succeeds: the old certificate is served")
	}

	// A key that does not belong to the certificate: rejected, and the server stays ready on what it has.
	if err := os.WriteFile(keyFile, oldKey, 0o600); err != nil { //nolint:gosec // G703: the harness's own file, under the test's temp directory
		t.Fatal(err)
	}
	if err := srv.Srv.TLSManager().Reload(); err == nil {
		t.Error("Reload accepted a certificate with another certificate's key")
	}
	if got, err := handshakeWith(srv.Srv, newRoots, servertest.TLSDomain); err != nil || !got.Equal(second) {
		t.Errorf("after a rejected reload: %v, want the certificate loaded before", err)
	}
	if ok, checks := srv.Srv.Health().Ready(); !ok {
		t.Errorf("not ready after a rejected reload: %v", checks)
	}
	if st := srv.Srv.TLSManager().Status(); st.LastErrorCode != tlsmgr.CodeCertInvalid {
		t.Errorf("TLS status = %+v, want the load error", st)
	}
}

// logCapture collects a server's log as JSON lines at debug level, as servertest does.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logCapture) logger() *slog.Logger {
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	return logx.New(logx.Options{Level: level, Format: "json", Out: l})
}

// startDirect starts a server from cfg without the harness, for the servers that are not ready after Start. pub is
// what the public-address detection finds.
func startDirect(t *testing.T, cfg *config.Config, deps server.Deps, pub netx.PublicAddrs) (*server.Server, *logCapture) {
	t.Helper()
	logs := &logCapture{}
	s, err := server.New(cfg, logs.logger(), deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetPublicAddrs(pub)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx, server.ShutdownStop); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	return s, logs
}

// tlsFlags are the listeners of a TLS-mode server on ephemeral loopback ports, without STUN servers.
func tlsFlags(flags ...string) []string {
	return append([]string{"--listen.https=127.0.0.1:0", "--listen.http=127.0.0.1:0", "--network.stun-servers="}, flags...)
}

// closedCA is an ACME directory on a closed port of this machine: every order fails at once and no test talks to a
// real certificate authority.
const closedCA = "--tls.acme-ca=https://127.0.0.1:1/dir"

// TestManualModeWithoutCertificate: files that can't be loaded don't stop the server. It runs, not ready, and
// becomes ready when the files are there (04 §8.4).
func TestManualModeWithoutCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cfg := testConfig(t, tlsFlags("--tls.mode=manual", "--domain=watch.example.com", "--tls.cert-file="+certFile, "--tls.key-file="+keyFile)...)
	s, logs := startDirect(t, cfg, testDeps(), netx.PublicAddrs{})

	ok, checks := s.Health().Ready()
	if ok || !strings.Contains(checks["tls"], tlsmgr.CodeCertUnreadable) {
		t.Fatalf("Ready = %v %v, want the tls check to fail on the missing files", ok, checks)
	}
	if _, err := handshakeWith(s, nil, "watch.example.com"); err == nil {
		t.Error("a TLS handshake succeeded without a certificate")
	}
	// The plain port still knows where the site is.
	_, port, _ := net.SplitHostPort(s.Addrs().HTTPS.String())
	if res := plainGet(t, s, "/x", ""); res.status != http.StatusPermanentRedirect || res.header.Get("Location") != "https://watch.example.com:"+port+"/x" {
		t.Errorf("plain-HTTP GET = %d → %q", res.status, res.header.Get("Location"))
	}
	if !strings.Contains(logs.String(), "no certificate loaded") {
		t.Errorf("no log line about the missing certificate:\n%s", logs.String())
	}

	roots := testCert(t, certFile, keyFile, "watch.example.com")
	if err := s.TLSManager().Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if ok, checks := s.Health().Ready(); !ok {
		t.Fatalf("not ready with a certificate: %v", checks)
	}
	if _, err := handshakeWith(s, roots, "watch.example.com"); err != nil {
		t.Errorf("handshake: %v", err)
	}
}

// TestIPMode: the site and the certificate's name are the public address. Until the certificate is there the
// server runs, not ready, and its plain port shows the waiting page; the ACME path belongs to certmagic.
func TestIPMode(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	s, logs := startDirect(t, cfg, testDeps(), netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodInterface})

	_, port, _ := net.SplitHostPort(s.Addrs().HTTPS.String())
	site := s.Site()
	if site.Origin != "https://203.0.113.7:"+port || site.Hostname != "203.0.113.7" || site.TLSMode != config.TLSIP {
		t.Errorf("site = %+v", site)
	}
	ok, checks := s.Health().Ready()
	if ok || checks["public_ip"] != "ok" || !strings.HasPrefix(checks["tls"], "getting a certificate for 203.0.113.7") {
		t.Errorf("Ready = %v %v, want public_ip ok and tls waiting for the certificate", ok, checks)
	}
	res := plainGet(t, s, "/", "203.0.113.7")
	if res.status != http.StatusServiceUnavailable || res.header.Get("Retry-After") != "30" || !strings.Contains(res.body, "isshoni is getting its certificate") {
		t.Errorf("plain-HTTP GET during the order = %d %q", res.status, res.body)
	}
	if res := plainGet(t, s, "/.well-known/acme-challenge/unknown", "203.0.113.7"); res.status != http.StatusNotFound {
		t.Errorf("an unknown ACME challenge = %d, want 404", res.status)
	}
	if _, err := handshakeWith(s, nil, "203.0.113.7"); err == nil {
		t.Error("a TLS handshake succeeded without a certificate")
	}

	// The order fails (the CA is a closed port); the status says so and the server keeps running.
	waitFor(t, "the failed order", func() bool { return s.TLSManager().Status().LastErrorCode != "" })
	st := s.TLSManager().Status()
	if st.Mode != config.TLSIP || st.Ready || len(st.Names) != 1 || st.Names[0] != "203.0.113.7" || st.LastErrorCode != tlsmgr.CodeACMEFailed {
		t.Errorf("TLS status = %+v", st)
	}
	if _, checks := s.Health().Ready(); !strings.Contains(checks["tls"], tlsmgr.CodeACMEFailed) {
		t.Errorf("the tls check does not carry the error code: %v", checks)
	}
	// The certificates live in the data directory.
	if fi, err := os.Stat(cfg.Paths().CertMagic); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("certmagic storage: %v, %v", fi, err)
	}
	if !strings.Contains(logs.String(), "ready: https://203.0.113.7:"+port+" (tls=ip)") {
		t.Errorf("no ready line for the ip site:\n%s", logs.String())
	}
}

// TestIPModeWithoutPublicAddress: when the detection finds nothing, an ip-mode server has no site. It starts all
// the same and says what is missing: the readiness checks, the log and the waiting page (04 §6.2, §7.4).
func TestIPModeWithoutPublicAddress(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	s, logs := startDirect(t, cfg, testDeps(), netx.PublicAddrs{})

	if site := s.Site(); site.Origin != "" || site.Host != "" || site.TLSMode != config.TLSIP {
		t.Errorf("site = %+v, want none", site)
	}
	ok, checks := s.Health().Ready()
	if ok || !strings.HasPrefix(checks["public_ip"], "no public IP address found") || !strings.HasPrefix(checks["tls"], "no public IP address") {
		t.Errorf("Ready = %v %v", ok, checks)
	}
	if res := plainGet(t, s, "/", ""); res.status != http.StatusServiceUnavailable || res.header.Get("Location") != "" {
		t.Errorf("plain-HTTP GET = %d → %q, want the waiting page", res.status, res.header.Get("Location"))
	}
	var warned bool
	for _, rec := range logRecords(t, logs.String()) {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "started without a public IP address (tls=ip)") {
			warned = rec["level"] == "WARN"
		}
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "isshoni "+version.Version()+" ready: ") {
			t.Errorf("a ready line without a site: %v", rec)
		}
	}
	if !warned {
		t.Errorf("no warning about the missing address:\n%s", logs.String())
	}
	// No order was started for nothing.
	if st := s.TLSManager().Status(); len(st.Names) != 0 || st.LastErrorCode != "" {
		t.Errorf("TLS status = %+v", st)
	}
}

// resolverFunc is a netx.Resolver.
type resolverFunc func(host string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return f(host)
}

// TestAutoMode: the site is the domain; the server waits for its certificate, and looks at the domain's DNS records
// before the first order (04 §8.7).
func TestAutoMode(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=auto", "--domain=Watch.Example.com", "--tls.acme-email=admin@example.com", closedCA)...)
	deps := testDeps()
	var asked sync.Map
	deps.Resolver = resolverFunc(func(host string) ([]netip.Addr, error) {
		asked.Store(host, true)
		return []netip.Addr{netip.MustParseAddr("198.51.100.4")}, nil
	})
	s, logs := startDirect(t, cfg, deps, netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodInterface})

	_, port, _ := net.SplitHostPort(s.Addrs().HTTPS.String())
	if site := s.Site(); site.Origin != "https://watch.example.com:"+port || site.TLSMode != config.TLSAuto {
		t.Errorf("site = %+v", site)
	}
	ok, checks := s.Health().Ready()
	if _, has := checks["public_ip"]; ok || has || !strings.HasPrefix(checks["tls"], "getting a certificate for watch.example.com") {
		t.Errorf("Ready = %v %v, want only the tls check to fail", ok, checks)
	}
	if res := plainGet(t, s, "/r/lounge", "watch.example.com"); res.status != http.StatusServiceUnavailable {
		t.Errorf("plain-HTTP GET during the order = %d, want the waiting page", res.status)
	}
	waitFor(t, "the failed order", func() bool { return s.TLSManager().Status().LastErrorCode != "" })
	waitFor(t, "the DNS check", func() bool { return strings.Contains(logs.String(), tlsmgr.CodeDNSWrong) })
	if _, ok := asked.Load("watch.example.com"); !ok {
		t.Error("the server's resolver was not asked for the domain")
	}
	var hint map[string]any
	for _, rec := range logRecords(t, logs.String()) {
		if rec["code"] == tlsmgr.CodeDNSWrong {
			hint = rec
		}
	}
	if hint == nil || hint["fix"] != "watch.example.com points to 198.51.100.4, but this server is 203.0.113.7." || hint["component"] != "tls" {
		t.Errorf("DNS hint = %v", hint)
	}
}

// TestTLSPortInUse: a busy port is a runtime error that names the port and the ways out (04 §6.1 step 6).
func TestTLSPortInUse(t *testing.T) {
	busy := listenLoopback(t)
	_, port, _ := net.SplitHostPort(busy.Addr().String())
	dir := t.TempDir()
	manual := []string{"--tls.mode=manual", "--domain=watch.example.com", "--tls.cert-file=" + filepath.Join(dir, "c.pem"), "--tls.key-file=" + filepath.Join(dir, "k.pem"), "--network.stun-servers="}

	t.Run("listen.https", func(t *testing.T) {
		cfg := testConfig(t, append(manual, "--listen.https="+busy.Addr().String(), "--listen.http=127.0.0.1:0")...)
		s, err := server.New(cfg, discardLog(), testDeps())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Start(context.Background())
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || server.NeedsOperator(err) {
			t.Fatalf("Start = %v, want a busy-port error that a restart may fix", err)
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "port "+port+" is in use (another web server?)") || !strings.Contains(msg, `tls.mode = "off"`) {
			t.Errorf("error = %q", msg)
		}
		// Nothing stays open: the data directory can be locked again.
		lock, err := config.LockDataDir(context.Background(), cfg.Paths())
		if err != nil {
			t.Fatalf("the data directory is still locked: %v", err)
		}
		_ = lock.Close()
	})

	t.Run("listen.http", func(t *testing.T) {
		cfg := testConfig(t, append(manual, "--listen.https=127.0.0.1:0", "--listen.http="+busy.Addr().String())...)
		s, err := server.New(cfg, discardLog(), testDeps())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Start(context.Background())
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || server.NeedsOperator(err) {
			t.Fatalf("Start = %v, want a busy-port error", err)
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "port "+port+" is in use") || !strings.Contains(msg, "certificate challenges") {
			t.Errorf("error = %q", msg)
		}
		// The multiplexer that was bound first is closed again.
		if a := s.Addrs().HTTPS; a != nil {
			if c, err := tryDial(a); err == nil {
				_ = c.Close()
				t.Errorf("the HTTPS port %s still accepts after a failed Start", a)
			}
		}
	})
}

// TestTLSShutdown: stopping a TLS server takes no longer with connections in every state on its ports: one that
// never sent a byte, one that stopped inside the TLS handshake, an idle HTTP/2 one, an ICE one and an idle one on
// the plain port.
func TestTLSShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	addrs := srv.Srv.Addrs()

	silent := dialRaw(t, addrs.HTTPS)
	halfHello := dialRaw(t, addrs.HTTPS)
	if _, err := halfHello.Write([]byte{0x16, 0x03, 0x01, 0x02, 0x00}); err != nil { // a TLS record header, and nothing after it
		t.Fatal(err)
	}
	ice := dialRaw(t, addrs.HTTPS)
	if _, err := ice.Write(stunBindingFrame()); err != nil {
		t.Fatal(err)
	}
	plain := dialRaw(t, addrs.HTTP)
	if res := get(t, srv, "/healthz"); res.status != 200 { // leaves an idle HTTP/2 connection in the client's pool
		t.Fatalf("/healthz = %d", res.status)
	}
	// The half ClientHello and the client's connection are on the TLS side. The STUN frame waits in the queue of
	// the ICE side, which nothing reads yet, and the silent connection for its first byte.
	waitFor(t, "the multiplexer to see the connections", func() bool { return srv.Srv.PortMux().Stats().TLS >= 2 })

	begin := time.Now()
	srv.Stop(t)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("Stop took %v with idle connections on the TLS ports", d)
	}
	for name, c := range map[string]net.Conn{"silent": silent, "half a ClientHello": halfHello, "ICE": ice, "plain": plain} {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 16)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the %s connection is still open after the shutdown (%v)", name, err)
		}
	}
	for _, a := range []net.Addr{addrs.HTTPS, addrs.HTTP} {
		if c, err := tryDial(a); err == nil {
			_ = c.Close()
			t.Errorf("%s still accepts after the shutdown", a)
		}
	}
}

// TestPlainPortConnectionLimit: limits.conns_per_ip holds on listen.http as on the 443 multiplexer (04 §7.8).
func TestPlainPortConnectionLimit(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true, Flags: []string{"--limits.conns-per-ip=3"}})
	addr := srv.Srv.Addrs().HTTP

	// Three connections sit there; the server keeps them until they send a request.
	held := make([]net.Conn, 3)
	for i := range held {
		held[i] = dialRaw(t, addr)
	}
	waitFor(t, "the server to accept the three connections", func() bool { return srv.Srv.PendingConns() >= 3 })
	// The fourth from the same address is closed at once.
	extra := dialRaw(t, addr)
	if _, err := io.WriteString(extra, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err == nil {
		_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := extra.Read(make([]byte, 16)); n != 0 || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the connection over the limit got an answer or stayed open (%d bytes, %v)", n, err)
		}
	}
	// A place that is given back can be taken again.
	_ = held[0].Close()
	waitFor(t, "a request to get through after a connection closed", func() bool {
		c, err := tryDial(addr)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
			return false
		}
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusPermanentRedirect
	})
}
