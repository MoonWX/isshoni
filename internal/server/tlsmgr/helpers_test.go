package tlsmgr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// testCA is a private certificate authority for the tests, as an operator's own CA would be in manual mode.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

// newTestCA makes a CA that is valid for ten years around now (the fake clock's now inside a synctest bubble).
func newTestCA(t testing.TB, now time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"isshoni tests"}, CommonName: "Test Root"},
		NotBefore:             now.Add(-5 * 365 * 24 * time.Hour),
		NotAfter:              now.Add(5 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

var testSerial struct {
	sync.Mutex
	n int64
}

// pair is a leaf certificate with its key, as PEM.
type pair struct {
	certPEM, keyPEM []byte
	serial          *big.Int
}

// issue makes a leaf for names (DNS names and IP addresses), valid from notBefore to notAfter.
func (ca *testCA) issue(t testing.TB, notBefore, notAfter time.Time, names ...string) pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	testSerial.Lock()
	testSerial.n++
	serial := big.NewInt(1000 + testSerial.n)
	testSerial.Unlock()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pair{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		serial:  serial,
	}
}

// certFiles are the two files of a manual-mode server.
type certFiles struct {
	cert, key string
	mod       time.Time // the modification time of the last write
}

func newCertFiles(t testing.TB) *certFiles {
	t.Helper()
	dir := t.TempDir()
	return &certFiles{cert: filepath.Join(dir, "fullchain.pem"), key: filepath.Join(dir, "privkey.pem"), mod: time.Unix(1_700_000_000, 0)}
}

// write replaces a file and moves its modification time on by a second, so that a change shows whatever the file
// system's timestamp resolution is, and whatever the fake clock says.
func (f *certFiles) write(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.mod = f.mod.Add(time.Second)
	if err := os.Chtimes(path, f.mod, f.mod); err != nil {
		t.Fatal(err)
	}
}

func (f *certFiles) writePair(t testing.TB, p pair) {
	t.Helper()
	f.write(t, f.cert, p.certPEM)
	f.write(t, f.key, p.keyPEM)
}

// logSink collects the manager's log as JSON lines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// records returns the log lines whose message contains msg.
func (l *logSink) records(t testing.TB, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(l.String()))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", sc.Text(), err)
		}
		if m, _ := rec["msg"].(string); strings.Contains(m, msg) {
			out = append(out, rec)
		}
	}
	return out
}

// exact returns the log lines whose message is msg.
func (l *logSink) exact(t testing.TB, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range l.records(t, msg) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// testSite is the site of a domain install.
func testSite(mode config.TLSMode, host string) config.Site {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = strings.Trim(h, "[]")
	}
	return config.Site{Origin: "https://" + host, Host: host, Hostname: hostname, TLSMode: mode}
}

// newManager builds a manager that logs into the returned sink, and stops it when the test ends.
func newManager(t testing.TB, opts Options) (*Manager, *logSink) {
	t.Helper()
	sink := &logSink{}
	opts.Logger = sink.logger()
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if t.Failed() {
			t.Logf("manager log:\n%s", sink.String())
		}
	})
	return m, sink
}

// memConn is one end of an in-memory connection with unbounded buffers. net.Pipe has none: a client that rejects
// the server's certificate writes its alert while the server still writes its flight, and both would block for
// good. Reads block on a sync.Cond, which a synctest bubble counts as idle.
type memConn struct {
	in, out *memBuffer
}

type memBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   bytes.Buffer
	closed bool
}

func newMemBuffer() *memBuffer {
	b := &memBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// memPipe returns the two ends of an in-memory connection.
func memPipe() (a, b net.Conn) {
	x, y := newMemBuffer(), newMemBuffer()
	return &memConn{in: x, out: y}, &memConn{in: y, out: x}
}

func (c *memConn) Read(p []byte) (int, error) {
	b := c.in
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.data.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.data.Len() == 0 {
		return 0, io.EOF
	}
	return b.data.Read(p)
}

func (c *memConn) Write(p []byte) (int, error) {
	b := c.out
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.cond.Broadcast()
	return b.data.Write(p)
}

// Close ends both directions: the peer reads what is buffered and then EOF, and its writes fail.
func (c *memConn) Close() error {
	for _, b := range []*memBuffer{c.in, c.out} {
		b.mu.Lock()
		b.closed = true
		b.cond.Broadcast()
		b.mu.Unlock()
	}
	return nil
}

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }

func (*memConn) LocalAddr() net.Addr              { return memAddr{} }
func (*memConn) RemoteAddr() net.Addr             { return memAddr{} }
func (*memConn) SetDeadline(time.Time) error      { return nil }
func (*memConn) SetReadDeadline(time.Time) error  { return nil }
func (*memConn) SetWriteDeadline(time.Time) error { return nil }

// handshake completes a TLS handshake against the manager's configuration over an in-memory connection and
// returns the client's view of it. serverName "" sends no SNI and skips the name check.
func handshake(t testing.TB, m *Manager, roots *x509.CertPool, serverName string, protos ...string) (tls.ConnectionState, error) {
	t.Helper()
	clientSide, serverSide := memPipe()
	defer func() { _ = clientSide.Close() }()
	srv := tls.Server(serverSide, m.TLSConfig())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = serverSide.Close() }()
		_ = srv.HandshakeContext(context.Background())
	}()
	cfg := &tls.Config{RootCAs: roots, ServerName: serverName, NextProtos: protos, MinVersion: tls.VersionTLS12}
	if serverName == "" {
		// No SNI, as a client that connects to an IP address: check the chain, not the name.
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: the chain is verified below
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
			return err
		}
	}
	client := tls.Client(clientSide, cfg)
	err := client.HandshakeContext(context.Background())
	state := client.ConnectionState()
	_ = clientSide.Close()
	<-done
	return state, err
}

// servedSerial returns the serial number of the certificate the manager serves.
func servedSerial(t testing.TB, m *Manager, roots *x509.CertPool, serverName string) *big.Int {
	t.Helper()
	state, err := handshake(t, m, roots, serverName)
	if err != nil {
		t.Fatalf("handshake for %q: %v", serverName, err)
	}
	return state.PeerCertificates[0].SerialNumber
}
