package netx

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
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/stun/v4"
)

// TestIPKey is the IPKey table of 04 §17; the wiring test pins 03's limiter key to the same results.
func TestIPKey(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"203.0.113.7", "203.0.113.7/32"},
		{"::ffff:203.0.113.7", "203.0.113.7/32"}, // IPv4-mapped IPv6 is unmapped first
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"}, // same /64
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},                  // the neighbouring /64
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
	} {
		if got := IPKey(netip.MustParseAddr(tc.addr)); got != netip.MustParsePrefix(tc.want) {
			t.Errorf("IPKey(%s) = %v, want %s", tc.addr, got, tc.want)
		}
	}
	if got := IPKey(netip.Addr{}); got.IsValid() {
		t.Errorf("IPKey(zero) = %v, want the zero Prefix", got)
	}
}

// --- helpers ---

// dialMux opens a client connection to m and closes it when the test ends.
func dialMux(t *testing.T, m *PortMux) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(context.Background(), "tcp4", m.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dialMuxWith dials m and sends first.
func dialMuxWith(t *testing.T, m *PortMux, first ...byte) net.Conn {
	t.Helper()
	c := dialMux(t, m)
	if _, err := c.Write(first); err != nil {
		t.Fatal(err)
	}
	return c
}

// acceptOne accepts one connection from l within 5 s and closes it when the test ends.
func acceptOne(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1) // an Accept that outlives a failed test ends when the mux closes
	go func() {
		c, err := l.Accept()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Accept: %v", r.err)
		}
		t.Cleanup(func() { _ = r.c.Close() })
		return r.c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection to accept")
		return nil
	}
}

// expectClosedByServer checks that the server closed c: reading ends with EOF (or a reset) within 5 s.
func expectClosedByServer(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, c); isTimeout(err) {
		t.Errorf("connection from %v is still open", c.LocalAddr())
	}
}

// expectOpen checks that c is still open: a short read times out.
func expectOpen(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !isTimeout(err) {
		t.Errorf("connection from %v: read %v, want a timeout (still open)", c.LocalAddr(), err)
	}
	_ = c.SetReadDeadline(time.Time{})
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pendingRemotes lists the remote addresses of m's pending connections, oldest first.
func pendingRemotes(m *PortMux) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for e := m.pending.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*prefixConn).RemoteAddr().String())
	}
	return out
}

func pendingLen(m *PortMux) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending.Len()
}

func liveLen(m *PortMux) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

var loopbackKey = IPKey(netip.MustParseAddr("127.0.0.1"))

// stunFrame is an RFC 4571 frame holding a STUN Binding request, as an ICE-TCP client sends first.
func stunFrame(t *testing.T) []byte {
	t.Helper()
	msg, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername("ufrag:peer"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(msg.Raw); n >= 0 && n <= 0x0200 { // pion reads the first frame into 512 bytes
		return append(binary.BigEndian.AppendUint16(nil, uint16(n)), msg.Raw...)
	}
	t.Fatalf("STUN request of %d bytes", len(msg.Raw))
	return nil
}

// testCertificate is a self-signed certificate for 127.0.0.1 and the pool that trusts it.
func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "isshoni portmux test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func httpProtocols(h1, h2 bool) *http.Protocols {
	var p http.Protocols
	p.SetHTTP1(h1)
	p.SetHTTP2(h2)
	return &p
}

// --- routes ---

// TestPortMuxTLS: a TLS ClientHello goes to TLS() with its first byte replayed, and http.Server.ServeTLS on it
// completes the handshake and serves HTTP/1.1 and HTTP/2 (ALPN); the handler sees a *net.TCPAddr as local address.
func TestPortMuxTLS(t *testing.T) {
	var counter TransferCounter
	m := newTestPortMux(t, PortMuxOptions{Counter: &counter})
	cert, roots := testCertificate(t)
	seen := make(chan string, 2) // what the handler saw: protocol and local address type
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- fmt.Sprintf("%s %T", r.Proto, r.Context().Value(http.LocalAddrContextKey))
			w.WriteHeader(http.StatusNoContent)
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         httpProtocols(true, true),
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(m.TLS(), "", "") }()
	defer func() {
		_ = srv.Close()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("ServeTLS: %v", err)
		}
	}()

	for _, tc := range []struct {
		h1, h2 bool
		proto  string
	}{
		{h1: true, proto: "HTTP/1.1"},
		{h2: true, proto: "HTTP/2.0"},
	} {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			Protocols:       httpProtocols(tc.h1, tc.h2),
		}
		client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+m.Addr().String()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.proto, err)
		}
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		if resp.StatusCode != http.StatusNoContent || resp.Proto != tc.proto {
			t.Errorf("response %d %s, want 204 %s", resp.StatusCode, resp.Proto, tc.proto)
		}
		if got, want := <-seen, tc.proto+" *net.TCPAddr"; got != want {
			t.Errorf("handler saw %q, want %q", got, want)
		}
	}
	if st := m.Stats(); st != (PortMuxStats{TLS: 2}) {
		t.Errorf("Stats = %+v, want 2 TLS connections", st)
	}
	totals := counter.Totals()
	if w := totals[PathWeb]; w.Egress == 0 || w.Ingress == 0 {
		t.Errorf("web = %+v, want bytes both ways", w)
	}
	if c := totals[PathMediaTCP]; c.Egress != 0 || c.Ingress != 0 {
		t.Errorf("media_tcp = %+v, want nothing on TLS connections", c)
	}
}

// TestPortMuxICE: an RFC 4571 STUN frame goes to ICE() with the first byte replayed; the addresses are the TCP
// ones (pion needs a *net.TCPAddr), bytes are counted on media_tcp, and the connection holds one open and one ICE
// slot of its IPKey until it is closed.
func TestPortMuxICE(t *testing.T) {
	var counter TransferCounter
	m := newTestPortMux(t, PortMuxOptions{Counter: &counter})
	if a, ok := m.ICE().Addr().(*net.TCPAddr); !ok || a.String() != m.Addr().String() {
		t.Errorf("ICE().Addr() = %#v, want the *net.TCPAddr %v", m.ICE().Addr(), m.Addr())
	}
	frame := stunFrame(t)
	client := dialMuxWith(t, m, frame...)
	server := acceptOne(t, m.ICE())

	if _, ok := server.LocalAddr().(*net.TCPAddr); !ok {
		t.Errorf("LocalAddr is %T, want *net.TCPAddr", server.LocalAddr())
	}
	if ra, ok := server.RemoteAddr().(*net.TCPAddr); !ok || ra.String() != client.LocalAddr().String() {
		t.Errorf("RemoteAddr = %#v, want the client's %v", server.RemoteAddr(), client.LocalAddr())
	}
	if n, err := server.Read(nil); n != 0 || err != nil { // a zero-length read keeps the peeked byte
		t.Errorf("Read(nil) = %d, %v", n, err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(frame) {
		t.Errorf("read % x, want % x", got, frame)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}

	if st := m.Stats(); st != (PortMuxStats{ICE: 1}) {
		t.Errorf("Stats = %+v, want 1 ICE connection", st)
	}
	totals := counter.Totals()
	if c := totals[PathMediaTCP]; c.Egress != 4 || c.Ingress != uint64(len(frame)) {
		t.Errorf("media_tcp = %+v, want {4 %d}", c, len(frame))
	}
	if w := totals[PathWeb]; w.Egress != 0 || w.Ingress != 0 {
		t.Errorf("web = %+v, want nothing on an ICE connection", w)
	}
	if o, i := m.open.open(loopbackKey), m.iceConns.open(loopbackKey); o != 1 || i != 1 {
		t.Errorf("open %d, ICE %d; want 1 and 1", o, i)
	}
	_ = server.Close()
	_ = server.Close() // releases once
	if o, i, l := m.open.open(loopbackKey), m.iceConns.open(loopbackKey), liveLen(m); o != 0 || i != 0 || l != 0 {
		t.Errorf("after Close: open %d, ICE %d, live %d; want 0", o, i, l)
	}
	expectClosedByServer(t, client)
}

// TestPortMuxFirstByte is the classification table of 04 §7.2.
func TestPortMuxFirstByte(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{PublicHost: "example.com"})
	for _, tc := range []struct {
		first byte
		want  string
	}{
		{0x16, "tls"},
		{0x00, "ice"}, {0x01, "ice"}, {0x02, "ice"},
		{'A', "hint"}, {'G', "hint"}, {'P', "hint"}, {'Z', "hint"},
		{0x03, "garbage"}, {0x15, "garbage"}, {0x17, "garbage"}, {'@', "garbage"}, {'[', "garbage"}, {'a', "garbage"},
		{'g', "garbage"}, {0x80, "garbage"}, {0xFF, "garbage"},
	} {
		t.Run(fmt.Sprintf("%#02x", tc.first), func(t *testing.T) {
			before := m.Stats()
			c := dialMuxWith(t, m, tc.first)
			want := before
			expectFirst := func(l net.Listener) {
				s := acceptOne(t, l)
				b := make([]byte, 1)
				if _, err := io.ReadFull(s, b); err != nil || b[0] != tc.first {
					t.Errorf("read %#02x, %v; want %#02x", b[0], err, tc.first)
				}
				_ = s.Close()
			}
			switch tc.want {
			case "tls":
				want.TLS++
				expectFirst(m.TLS())
			case "ice":
				want.ICE++
				expectFirst(m.ICE())
			case "hint":
				want.PlainHTTP++
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				resp, err := io.ReadAll(c)
				if err != nil || !strings.HasPrefix(string(resp), "HTTP/1.1 400 Bad Request\r\n") {
					t.Errorf("got %q, %v; want the 400 hint", resp, err)
				}
			case "garbage":
				want.Garbage++
				expectClosedByServer(t, c)
			}
			eventually(t, fmt.Sprintf("Stats is %+v", want), func() bool { return m.Stats() == want })
		})
	}
	eventually(t, "every connection is released", func() bool { return liveLen(m) == 0 })
}

// TestPortMuxNoFirstByte: a client that closes without sending anything is Garbage.
func TestPortMuxNoFirstByte(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{})
	c := dialMux(t, m)
	eventually(t, "the connection is pending", func() bool { return pendingLen(m) == 1 })
	_ = c.Close()
	eventually(t, "it is counted as garbage", func() bool { return m.Stats() == PortMuxStats{Garbage: 1} })
	eventually(t, "it is released", func() bool { return liveLen(m) == 0 && m.open.open(loopbackKey) == 0 })
}

// TestPortMuxPlainHTTPHint: plain HTTP on the TLS port gets a 400 that points to https://PublicHost/, also when
// the request has a body the answer doesn't wait for, and then the connection is closed.
func TestPortMuxPlainHTTPHint(t *testing.T) {
	body := strings.Repeat("x", 16<<10)
	post := "POST /api/v1/auth/login HTTP/1.1\r\nHost: example.com\r\nContent-Type: text/plain\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body
	get := "GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: curl\r\n\r\n"
	for _, tc := range []struct {
		name, publicHost, req, want string
	}{
		{"domain", "example.com", get, "https://example.com/"},
		{"IPv6 literal", "2001:db8::7", get, "https://[2001:db8::7]/"},
		{"with port", "example.com:8443", get, "https://example.com:8443/"},
		{"no public host", "", get, "https://127.0.0.1:%d/"},
		{"request body", "example.com", post, "https://example.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var counter TransferCounter
			m := newTestPortMux(t, PortMuxOptions{PublicHost: tc.publicHost, Counter: &counter})
			want := tc.want
			if strings.Contains(want, "%d") {
				want = fmt.Sprintf(want, tcpPortOf(m.TLS()))
			}
			c := dialMux(t, m)
			if _, err := io.WriteString(c, tc.req); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest || !resp.Close ||
				resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("response %d, close %v, headers %v", resp.StatusCode, resp.Close, resp.Header)
			}
			if w := "This port speaks HTTPS. Use " + want + "\n"; string(got) != w {
				t.Errorf("body %q, want %q", got, w)
			}
			expectClosedByServer(t, c)
			if st := m.Stats(); st != (PortMuxStats{PlainHTTP: 1}) {
				t.Errorf("Stats = %+v, want 1 plain HTTP", st)
			}
			if w := counter.Totals()[PathWeb]; w.Egress == 0 || w.Ingress == 0 {
				t.Errorf("web = %+v, want the hint's bytes both ways", w)
			}
		})
	}
}

func TestHintHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"example.com", "example.com"},
		{"example.com:8443", "example.com:8443"},
		{"203.0.113.7", "203.0.113.7"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		{"2001:db8::7", "[2001:db8::7]"},
		{"fe80::1%eth0", "[fe80::1]"},
		{"[2001:db8::7]:8443", "[2001:db8::7]:8443"},
	} {
		if got, err := hintHostOf(tc.in); err != nil || got != tc.want {
			t.Errorf("hintHostOf(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"example.com/x", "a b", "example.com\r\nX-Evil: 1", "user@example.com", `a"b`} {
		if _, err := hintHostOf(bad); err == nil {
			t.Errorf("hintHostOf(%q) accepted", bad)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:443", "127.0.0.1"},
		{"127.0.0.1:8443", "127.0.0.1:8443"},
		{"[::1]:443", "[::1]"},
		{"[::ffff:192.0.2.1]:443", "192.0.2.1"},
		{"[2001:db8::1]:8443", "[2001:db8::1]:8443"},
		{"0.0.0.0:443", ""},
	} {
		if got := urlHost(netip.MustParseAddrPort(tc.in)); got != tc.want {
			t.Errorf("urlHost(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	m := &PortMux{}
	if got := string(m.hintResponse(&net.UnixAddr{Name: "x", Net: "unix"})); !strings.HasSuffix(got,
		"\r\n\r\nThis port speaks HTTPS. Use an https:// address.\n") {
		t.Errorf("hint without any host: %q", got)
	}
}

func TestListenPortMuxErrors(t *testing.T) {
	if m, err := ListenPortMux(PortMuxOptions{}); m != nil || err == nil {
		t.Errorf("no address: %v, %v; want an error", m, err)
	}
	if m, err := ListenPortMux(PortMuxOptions{Addr: "127.0.0.1:0", PublicHost: "example.com/x"}); m != nil || err == nil {
		t.Errorf("bad PublicHost: %v, %v; want an error", m, err)
	}
	var lc net.ListenConfig
	busy, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	m, err := ListenPortMux(PortMuxOptions{Addr: busy.Addr().String()})
	var le *ListenError
	if m != nil || !errors.As(err, &le) || le.Proto != "tcp" || !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("busy port: %v, %v; want a tcp *ListenError with EADDRINUSE", m, err)
	}
}

// --- timeouts ---

// TestPortMuxSlowClient: a client that sends nothing is closed at ClassifyTimeout (shortened here) and counted as
// Timeout; meanwhile the accept loop keeps classifying other clients, including one that takes a moment.
func TestPortMuxSlowClient(t *testing.T) {
	const timeout = time.Second
	m := newTestPortMux(t, PortMuxOptions{ClassifyTimeout: timeout})
	start := time.Now()
	silent := dialMux(t, m)
	eventually(t, "the silent client is pending", func() bool { return pendingLen(m) == 1 })

	dialMuxWith(t, m, 0x16)
	acceptOne(t, m.TLS())
	slow := dialMux(t, m)
	time.Sleep(timeout / 5)
	if _, err := slow.Write([]byte{0x00}); err != nil {
		t.Fatal(err)
	}
	acceptOne(t, m.ICE())
	if pendingLen(m) != 1 {
		t.Errorf("pending = %v, want only the silent client", pendingRemotes(m))
	}

	expectClosedByServer(t, silent)
	if d := time.Since(start); d < timeout || d > timeout+4*time.Second {
		t.Errorf("the silent client was closed after %v, want about %v", d, timeout)
	}
	eventually(t, "the timeout is counted", func() bool { return m.Stats() == PortMuxStats{TLS: 1, ICE: 1, Timeout: 1} })
}

// TestPortMuxClassifyTimeout checks the default first-byte timeout of 10 s on the fake clock.
func TestPortMuxClassifyTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fl := newFakeListener()
		m := newPortMux(fl, PortMuxOptions{})
		defer func() { _ = m.Close() }()
		s, c := pipeFrom(t, "192.0.2.1:1000")
		fl.conns <- s
		synctest.Wait()
		if pendingLen(m) != 1 {
			t.Fatal("the connection is not pending")
		}
		time.Sleep(10*time.Second - time.Nanosecond)
		synctest.Wait()
		if st := m.Stats(); st.Timeout != 0 || pendingLen(m) != 1 {
			t.Fatalf("closed before 10 s: %+v", st)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if st := m.Stats(); st != (PortMuxStats{Timeout: 1}) || pendingLen(m) != 0 || liveLen(m) != 0 {
			t.Errorf("after 10 s: %+v, pending %d, live %d; want one timeout and nothing left", st, pendingLen(m), liveLen(m))
		}
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("client read %v, want EOF", err)
		}
	})
}

// TestPortMuxQueueFull: a classified connection waits 1 s for room in a full sub-listener queue, then it is dropped
// (Limited); the queued one is still served.
func TestPortMuxQueueFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fl := newFakeListener()
		m := newPortMux(fl, PortMuxOptions{QueueLen: 1})
		defer func() { _ = m.Close() }()
		s1, c1 := pipeFrom(t, "192.0.2.1:1000")
		s2, c2 := pipeFrom(t, "192.0.2.2:1000")
		fl.conns <- s1
		fl.conns <- s2
		for _, c := range []net.Conn{c1, c2} { // c1 is queued before c2 arrives
			if _, err := c.Write([]byte{0x16}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		if st := m.Stats(); st.Limited != 0 {
			t.Fatalf("dropped before 1 s: %+v", st)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if st := m.Stats(); st != (PortMuxStats{Limited: 1}) {
			t.Errorf("after 1 s: %+v, want one Limited", st)
		}
		if _, err := c2.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("the dropped client read %v, want EOF", err)
		}
		sc, err := m.TLS().Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sc.Close() }()
		if sc.RemoteAddr().String() != "192.0.2.1:1000" {
			t.Errorf("accepted %v, want the first client", sc.RemoteAddr())
		}
		if st := m.Stats(); st != (PortMuxStats{TLS: 1, Limited: 1}) {
			t.Errorf("Stats = %+v", st)
		}
	})
}

// TestPortMuxAcceptRetries: transient errors of the raw listener are retried with a pause (5 ms doubling) and
// never reach the sub-listeners; Close during a pause ends the accept loop.
func TestPortMuxAcceptRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fl := newFakeListener()
		for range 3 {
			fl.errs <- errors.New("accept: too many open files")
		}
		start := time.Now()
		m := newPortMux(fl, PortMuxOptions{})
		defer func() { _ = m.Close() }()
		s, c := pipeFrom(t, "192.0.2.1:1000")
		fl.conns <- s
		if _, err := c.Write([]byte{0x16}); err != nil {
			t.Fatal(err)
		}
		sc, err := m.TLS().Accept()
		if err != nil {
			t.Fatalf("TLS().Accept = %v, want the connection after the transient errors", err)
		}
		if d := time.Since(start); d != 35*time.Millisecond {
			t.Errorf("accepted after %v, want 5+10+20 ms of pauses", d)
		}
		_ = sc.Close()

		fl.errs <- errors.New("accept: transient")
		synctest.Wait() // the loop pauses
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 35*time.Millisecond {
			t.Errorf("Close waited for the pause (%v)", d)
		}
	})
}

// --- limits ---

// TestPortMuxPendingPerIP: the 33rd connection of one IP that waits for its first byte is closed, and the waiting
// ones are served when they speak.
func TestPortMuxPendingPerIP(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{})
	var clients []net.Conn
	for i := range defaultMaxPendingPerIP + 1 {
		clients = append(clients, dialMux(t, m))
		wantPending, wantLimited := min(i+1, defaultMaxPendingPerIP), uint64(max(0, i+1-defaultMaxPendingPerIP))
		eventually(t, fmt.Sprintf("connection %d is admitted or refused", i+1), func() bool {
			return pendingLen(m) == wantPending && m.Stats().Limited == wantLimited
		})
	}
	expectClosedByServer(t, clients[defaultMaxPendingPerIP])
	expectOpen(t, clients[0])
	if _, err := clients[0].Write([]byte{0x16}); err != nil {
		t.Fatal(err)
	}
	acceptOne(t, m.TLS())
	dialMux(t, m) // room again for one waiting connection
	eventually(t, "a new connection is pending", func() bool { return pendingLen(m) == defaultMaxPendingPerIP })
	if st := m.Stats(); st != (PortMuxStats{TLS: 1, Limited: 1}) {
		t.Errorf("Stats = %+v", st)
	}
}

// TestPortMuxPendingPerSlash64: the per-IP pending count is per IPKey, so 33 connections from different addresses
// of one IPv6 /64 hit it, and the neighbouring /64 does not.
func TestPortMuxPendingPerSlash64(t *testing.T) {
	fl := newFakeListener()
	m := newPortMux(fl, PortMuxOptions{})
	defer func() { _ = m.Close() }()
	var clients []net.Conn
	for i := range defaultMaxPendingPerIP + 1 {
		s, c := pipeFrom(t, fmt.Sprintf("[2001:db8:1:2::%x]:1000", i+1))
		fl.conns <- s
		clients = append(clients, c)
	}
	s, _ := pipeFrom(t, "[2001:db8:1:3::1]:1000")
	fl.conns <- s
	eventually(t, "every connection is admitted or refused", func() bool {
		return pendingLen(m) == defaultMaxPendingPerIP+1 && m.Stats().Limited == 1
	})
	last := clients[defaultMaxPendingPerIP]
	_ = last.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := last.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the 33rd client of the /64 read %v, want EOF", err)
	}
	m.mu.Lock()
	n := m.pendingPerIP[netip.MustParsePrefix("2001:db8:1:2::/64")]
	m.mu.Unlock()
	if n != defaultMaxPendingPerIP {
		t.Errorf("pending in the /64 = %d, want %d", n, defaultMaxPendingPerIP)
	}
}

// TestPortMuxMaxPending: with the pending pool full, a new connection is admitted and the oldest pending one is
// closed (Limited), unless the new one's IPKey is at its own pending limit: then the new one is closed.
func TestPortMuxMaxPending(t *testing.T) {
	fl := newFakeListener()
	m := newPortMux(fl, PortMuxOptions{MaxPending: 3, MaxPendingPerIP: 2})
	defer func() { _ = m.Close() }()
	clients := map[string]net.Conn{}
	arrive := func(name, remote string, wantPending []string, wantLimited uint64) {
		t.Helper()
		s, c := pipeFrom(t, remote)
		clients[name] = c
		fl.conns <- s
		eventually(t, name+" is admitted or refused", func() bool {
			return strings.Join(pendingRemotes(m), " ") == strings.Join(wantPending, " ") &&
				m.Stats().Limited == wantLimited
		})
	}
	arrive("a", "192.0.2.1:1", []string{"192.0.2.1:1"}, 0)
	arrive("b", "192.0.2.2:1", []string{"192.0.2.1:1", "192.0.2.2:1"}, 0)
	arrive("c", "192.0.2.3:1", []string{"192.0.2.1:1", "192.0.2.2:1", "192.0.2.3:1"}, 0)
	arrive("d", "192.0.2.4:1", []string{"192.0.2.2:1", "192.0.2.3:1", "192.0.2.4:1"}, 1) // a is pushed out
	arrive("e", "192.0.2.4:2", []string{"192.0.2.3:1", "192.0.2.4:1", "192.0.2.4:2"}, 2) // b is pushed out
	arrive("f", "192.0.2.4:3", []string{"192.0.2.3:1", "192.0.2.4:1", "192.0.2.4:2"}, 3) // f's IP has 2 pending
	for _, name := range []string{"a", "b", "f"} {
		c := clients[name]
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("client %s read %v, want EOF", name, err)
		}
	}
	// The pushed-out ones are gone for good: their classifiers counted nothing else.
	eventually(t, "the closed connections are released", func() bool { return liveLen(m) == 3 })
	if st := m.Stats(); st != (PortMuxStats{Limited: 3}) {
		t.Errorf("Stats = %+v, want 3 Limited", st)
	}
}

// TestPortMuxRefusesUnknownRemote: a connection without an IP remote address can't be counted per IP, so it is
// closed (Limited).
func TestPortMuxRefusesUnknownRemote(t *testing.T) {
	fl := newFakeListener()
	m := newPortMux(fl, PortMuxOptions{})
	defer func() { _ = m.Close() }()
	s, c := net.Pipe() // RemoteAddr is "pipe"
	defer func() { _ = c.Close() }()
	fl.conns <- s
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("client read %v, want EOF", err)
	}
	eventually(t, "the refusal is counted", func() bool { return m.Stats() == PortMuxStats{Limited: 1} })
	if l := liveLen(m); l != 0 {
		t.Errorf("live = %d, want 0", l)
	}
}

// TestPortMuxOpenPerIP: MaxConnsPerIP counts every connection of an IPKey from accept to close (waiting, TLS and
// ICE); the next one is closed at once, and closing one gives its slot back.
func TestPortMuxOpenPerIP(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{MaxConnsPerIP: 2})
	dialMuxWith(t, m, 0x16)
	handed := acceptOne(t, m.TLS())
	dialMux(t, m) // waits for its first byte
	eventually(t, "the second connection is pending", func() bool { return pendingLen(m) == 1 })
	third := dialMuxWith(t, m, 0x16)
	expectClosedByServer(t, third)
	if st := m.Stats(); st != (PortMuxStats{TLS: 1, Limited: 1}) {
		t.Errorf("Stats = %+v, want the third connection Limited", st)
	}
	_ = handed.Close()
	if n := m.open.open(loopbackKey); n != 1 {
		t.Errorf("open = %d after closing a handed-out connection, want 1", n)
	}
	dialMuxWith(t, m, stunFrame(t)...)
	acceptOne(t, m.ICE())
	if st := m.Stats(); st != (PortMuxStats{TLS: 1, ICE: 1, Limited: 1}) {
		t.Errorf("Stats = %+v", st)
	}
}

// TestPortMuxICELimit: the per-IP ICE limit closes the next ICE connection and counts it once as Limited; TLS
// connections of the same IP are not affected.
func TestPortMuxICELimit(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{MaxICEConnsPerIP: 1})
	frame := stunFrame(t)
	dialMuxWith(t, m, frame...)
	first := acceptOne(t, m.ICE())
	refused := dialMuxWith(t, m, frame...)
	expectClosedByServer(t, refused)
	dialMuxWith(t, m, 0x16)
	acceptOne(t, m.TLS())
	eventually(t, "the refusal is counted", func() bool { return m.Stats() == PortMuxStats{TLS: 1, ICE: 1, Limited: 1} })
	if got := m.limited.Load(); got != 0 {
		t.Errorf("the mux counted the ICE refusal itself too (%d)", got)
	}
	_ = first.Close()
	if n := m.iceConns.open(loopbackKey); n != 0 {
		t.Errorf("ICE open = %d after Close, want 0", n)
	}
	dialMuxWith(t, m, frame...)
	acceptOne(t, m.ICE())
}

// TestPortMuxICELimitSharedWith7882: with the real mux, one ICE count covers 443 and 7882/tcp (04 §7.3): a 443
// connection handed to pion blocks a 7882 one, and the other way round.
func TestPortMuxICELimitSharedWith7882(t *testing.T) {
	pm := newTestPortMux(t, PortMuxOptions{MaxICEConnsPerIP: 1})
	tr := newTestTransport(t, TransportOptions{
		TCPAddr: "127.0.0.1:0", PortMux: pm, IncludeLoopback: true, Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	var d net.Dialer
	dial7882 := func() net.Conn {
		c, err := d.DialContext(context.Background(), "tcp4", tr.tcpLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	on443 := dialMuxWith(t, pm, 0x00) // pion waits for the rest of the frame
	eventually(t, "pion holds the 443 connection", func() bool { return tr.ln443.handedOut() == 1 })
	expectClosedByServer(t, dial7882())
	if st := pm.Stats(); st != (PortMuxStats{ICE: 1, Limited: 1}) {
		t.Errorf("Stats = %+v, want the 7882 refusal Limited", st)
	}

	_ = on443.Close() // pion's first read fails and it closes the connection, giving the slot back
	eventually(t, "the 443 slot is released", func() bool { return pm.iceConns.open(loopbackKey) == 0 })
	dial7882()
	eventually(t, "pion holds the 7882 connection", func() bool { return tr.tcpLn.handedOut() == 1 })
	expectClosedByServer(t, dialMuxWith(t, pm, 0x00))
	eventually(t, "the 443 refusal is counted", func() bool { return pm.Stats() == PortMuxStats{ICE: 1, Limited: 2} })
}

// --- closing ---

// TestPortMuxCloseUnblocksAccepts: Close ends Accept on both sub-listeners with net.ErrClosed, closes every
// connection (waiting, handed out as TLS or ICE), waits for its goroutines and frees the port.
func TestPortMuxCloseUnblocksAccepts(t *testing.T) {
	m, err := ListenPortMux(PortMuxOptions{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tlsClient := dialMuxWith(t, m, 0x16)
	acceptOne(t, m.TLS())
	iceClient := dialMuxWith(t, m, stunFrame(t)...)
	acceptOne(t, m.ICE())
	silent := dialMux(t, m)
	eventually(t, "the silent client is pending", func() bool { return pendingLen(m) == 1 })

	accepted := make(chan error, 2)
	for _, l := range []net.Listener{m.TLS(), m.ICE()} {
		go func() {
			c, err := l.Accept()
			if c != nil {
				_ = c.Close()
			}
			accepted <- err
		}()
	}
	time.Sleep(20 * time.Millisecond) // let both block

	start := time.Now()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
	for range 2 {
		select {
		case err := <-accepted:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("Accept returned %v, want net.ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an Accept did not return after Close")
		}
	}
	for _, c := range []net.Conn{tlsClient, iceClient, silent} {
		expectClosedByServer(t, c)
	}
	for _, l := range []net.Listener{m.TLS(), m.ICE()} {
		if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
		}
		if err := l.Close(); err != nil {
			t.Errorf("sub-listener Close after Close = %v", err)
		}
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if l, o, i := liveLen(m), m.open.open(loopbackKey), m.iceConns.open(loopbackKey); l != 0 || o != 0 || i != 0 {
		t.Errorf("after Close: live %d, open %d, ICE %d; want 0", l, o, i)
	}
	if st := m.Stats(); st != (PortMuxStats{TLS: 1, ICE: 1}) {
		t.Errorf("Stats = %+v: closing must not count outcomes", st)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp4", m.Addr().String())
	if err != nil {
		t.Fatalf("the port is still bound after Close: %v", err)
	}
	_ = ln.Close()
}

// TestPortMuxSubListenerClose: TLS().Close closes the queued TLS connections but leaves the handed-out ones to
// their server (graceful Shutdown); ICE().Close also closes the ICE connections it handed out, as pion's
// TCPMuxDefault.Close needs. Closing one sub-listener leaves the other working, and later connections for the
// closed one are closed.
func TestPortMuxSubListenerClose(t *testing.T) {
	m := newTestPortMux(t, PortMuxOptions{})
	handedTLS := dialMuxWith(t, m, 0x16)
	serverTLS := acceptOne(t, m.TLS())
	queuedTLS := dialMuxWith(t, m, 0x16)
	eventually(t, "a TLS connection is queued", func() bool { return len(m.tls.queue) == 1 })
	if err := m.TLS().Close(); err != nil {
		t.Fatal(err)
	}
	expectClosedByServer(t, queuedTLS)
	expectOpen(t, handedTLS)
	if _, err := serverTLS.Write([]byte("x")); err != nil {
		t.Errorf("the handed-out TLS connection was closed: %v", err)
	}
	if _, err := m.TLS().Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("TLS().Accept after Close = %v", err)
	}
	expectClosedByServer(t, dialMuxWith(t, m, 0x16))

	frame := stunFrame(t)
	handedICE := dialMuxWith(t, m, frame...)
	acceptOne(t, m.ICE())
	queuedICE := dialMuxWith(t, m, frame...)
	eventually(t, "an ICE connection is queued", func() bool { return len(m.ice.queue) == 1 })
	for range 2 {
		if err := m.ICE().Close(); err != nil {
			t.Fatal(err)
		}
	}
	expectClosedByServer(t, handedICE)
	expectClosedByServer(t, queuedICE)
	if st := m.Stats(); st != (PortMuxStats{TLS: 1, ICE: 1}) {
		t.Errorf("Stats = %+v", st)
	}
	if n := m.iceConns.open(loopbackKey); n != 0 {
		t.Errorf("ICE open = %d, want 0", n)
	}
}
