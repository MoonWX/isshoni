package update

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests of update.go, fetch.go and state.go run in testing/synctest bubbles, where time moves only when every
// goroutine is durably blocked. A real socket never is, so the fake GitHub is an httptest TLS server on an in-memory
// network: memPipe connections handed over a channel. The client's Transport dials that network whatever the host,
// and checks the server's certificate, made here for github.com, api.github.com and *.githubusercontent.com, against
// a pool holding only it; the server tells the hosts apart by the Host header. So the updater runs with its real URLs
// and its real redirect rule.

// testEpoch is the wall clock of the tests (Options.Now) at the start of a bubble: between the test manifests'
// publishedAt (2026-12-01) and expiresAt (2027-05-30).
var testEpoch = time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)

const (
	testCurrent = "0.2.0"
	repoPath    = "/MoonWX/isshoni/releases"
	assetHost   = "objects.githubusercontent.com"
	assetPrefix = "/github-production-release-asset/"
	assetQuery  = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=0badc0ffee&response-content-type=application%2Foctet-stream"
)

// memConn is one end of an in-memory connection. Unlike net.Pipe's, its writes never block: two TLS ends that both
// write, such as a client's close_notify while the server still writes a body the client gave up on, would
// otherwise wait for each other until TLS's 5 s write deadline. Its reads block durably (on channels made in the
// bubble) and honour read deadlines, which http.Server uses to stop its background read.
type memConn struct {
	in, out *memBuf
	rd      memDeadline
	done    chan struct{}
	once    sync.Once
}

// memBuf is one direction of a memConn.
type memBuf struct {
	mu     sync.Mutex
	data   []byte
	closed bool
	ready  chan struct{} // a wake-up for the reader, capacity 1
}

func newMemBuf() *memBuf { return &memBuf{ready: make(chan struct{}, 1)} }

func (b *memBuf) wake() {
	select {
	case b.ready <- struct{}{}:
	default:
	}
}

func (b *memBuf) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.wake()
}

// memPipe returns the two ends of an in-memory connection.
func memPipe() (a, b *memConn) {
	ab, ba := newMemBuf(), newMemBuf()
	a = &memConn{in: ba, out: ab, rd: newMemDeadline(), done: make(chan struct{})}
	b = &memConn{in: ab, out: ba, rd: newMemDeadline(), done: make(chan struct{})}
	return a, b
}

func (c *memConn) Read(p []byte) (int, error) {
	for {
		c.in.mu.Lock()
		if len(c.in.data) > 0 {
			n := copy(p, c.in.data)
			c.in.data = c.in.data[n:]
			c.in.mu.Unlock()
			return n, nil
		}
		closed := c.in.closed
		c.in.mu.Unlock()
		select {
		case <-c.done:
			return 0, net.ErrClosed
		default:
		}
		if closed {
			return 0, io.EOF
		}
		select {
		case <-c.in.ready:
		case <-c.rd.wait():
			return 0, os.ErrDeadlineExceeded
		case <-c.done:
			return 0, net.ErrClosed
		}
	}
}

func (c *memConn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	c.out.mu.Lock()
	if c.out.closed {
		c.out.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.out.data = append(c.out.data, p...)
	c.out.mu.Unlock()
	c.out.wake()
	return len(p), nil
}

// Close ends both directions: the peer reads what was written and then EOF, and its writes fail.
func (c *memConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.out.close()
		c.in.close()
	})
	return nil
}

func (c *memConn) LocalAddr() net.Addr               { return pipeAddr("192.0.2.1:443") }
func (c *memConn) RemoteAddr() net.Addr              { return pipeAddr("192.0.2.2:50000") }
func (c *memConn) SetDeadline(t time.Time) error     { c.rd.set(t); return nil }
func (c *memConn) SetReadDeadline(t time.Time) error { c.rd.set(t); return nil }
func (c *memConn) SetWriteDeadline(time.Time) error  { return nil } // writes never block

// memDeadline is a read deadline, as net.Pipe's pipeDeadline: a channel closed when it passes.
type memDeadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func newMemDeadline() memDeadline { return memDeadline{cancel: make(chan struct{})} }

func (d *memDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired: wait until its callback closed cancel
	}
	d.timer = nil
	closed := false
	select {
	case <-d.cancel:
		closed = true
	default:
	}
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *memDeadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// pipeListener is a net.Listener over memPipe connections.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr("192.0.2.1:443") }

// DialContext is the client Transport's dialer: a new pipe whatever the address.
func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := memPipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
	}
	_ = client.Close()
	_ = server.Close()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: net.ErrClosed}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "tcp" }
func (a pipeAddr) String() string  { return string(a) }

// testCert is the fake GitHub's certificate and the pool that trusts it.
var testCert = sync.OnceValues(func() (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake GitHub"},
		NotBefore:             time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), // a bubble's clock starts in 2000
		NotAfter:              time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC),
		DNSNames:              []string{"github.com", "api.github.com", "*.githubusercontent.com", "*.example"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
})

// seen is one request the fake GitHub got.
type seen struct {
	host, path, query string
	header            http.Header
}

func (s seen) String() string { return "https://" + s.host + s.path }

// fakeGitHub serves releases the way GitHub does: releases/latest/download/<asset> redirects to
// releases/download/v<latest>/<asset>, which redirects to a signed URL on objects.githubusercontent.com, and
// api.github.com answers the release listing. A test may answer a request itself (hook).
type fakeGitHub struct {
	t    *testing.T
	ln   *pipeListener
	srv  *httptest.Server
	tr   *http.Transport
	quit chan struct{} // closed when the test ends: blocked handlers return

	mu      sync.Mutex
	assets  map[string][]byte // "v<version>/<name>" → bytes
	latest  string            // the version releases/latest points to; "" is 404
	listing []byte            // the API's release listing; nil is 404
	hook    func(w http.ResponseWriter, r *http.Request) bool
	reqs    []seen
}

// newFakeGitHub starts the fake GitHub; call it inside the bubble.
func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	cert, pool := testCert()
	fg := &fakeGitHub{t: t, ln: newPipeListener(), quit: make(chan struct{}), assets: make(map[string][]byte)}
	fg.srv = httptest.NewUnstartedServer(fg)
	_ = fg.srv.Listener.Close()
	fg.srv.Listener = fg.ln
	fg.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	fg.srv.StartTLS()
	fg.tr = &http.Transport{DialContext: fg.ln.DialContext, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(func() {
		close(fg.quit)
		fg.tr.CloseIdleConnections()
		fg.srv.Close()
	})
	return fg
}

func (fg *fakeGitHub) with(f func(fg *fakeGitHub)) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	f(fg)
}

// setHook makes h answer requests first; h returns false to leave a request to the fake's default answers.
func (fg *fakeGitHub) setHook(h func(w http.ResponseWriter, r *http.Request) bool) {
	fg.with(func(fg *fakeGitHub) { fg.hook = h })
}

func (fg *fakeGitHub) requests() []seen {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return slices.Clone(fg.reqs)
}

// count returns the number of requests whose URL (https://host/path) contains sub.
func (fg *fakeGitHub) count(sub string) int {
	n := 0
	for _, r := range fg.requests() {
		if strings.Contains(r.String(), sub) {
			n++
		}
	}
	return n
}

func (fg *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fg.mu.Lock()
	fg.reqs = append(fg.reqs, seen{host: r.Host, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
	hook := fg.hook
	fg.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	fg.serveDefault(w, r)
}

func (fg *fakeGitHub) serveDefault(w http.ResponseWriter, r *http.Request) {
	fg.mu.Lock()
	latest, listing := fg.latest, fg.listing
	fg.mu.Unlock()
	switch r.Host {
	case "github.com":
		// A cookie, which the updater must never send back.
		http.SetCookie(w, &http.Cookie{Name: "_gh_sess", Value: "fake", Path: "/", Secure: true, HttpOnly: true,
			SameSite: http.SameSiteLaxMode})
		if name, ok := strings.CutPrefix(r.URL.Path, repoPath+"/latest/download/"); ok && latest != "" {
			redirect(w, "https://github.com"+repoPath+"/download/v"+latest+"/"+name)
			return
		}
		if key, ok := strings.CutPrefix(r.URL.Path, repoPath+"/download/"); ok && fg.asset(key) != nil {
			redirect(w, "https://"+assetHost+assetPrefix+key+"?"+assetQuery)
			return
		}
	case assetHost:
		if key, ok := strings.CutPrefix(r.URL.Path, assetPrefix); ok && r.URL.RawQuery == assetQuery {
			if data := fg.asset(key); data != nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data)
				return
			}
		}
	case "api.github.com":
		if r.URL.Path == "/repos/MoonWX/isshoni/releases" && listing != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write(listing)
			return
		}
	}
	http.NotFound(w, r)
}

func redirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusFound)
}

func (fg *fakeGitHub) asset(key string) []byte {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return fg.assets[key]
}

// release is what publish put on the fake GitHub.
type release struct {
	version   string
	name      string // the installer's asset name
	installer []byte
	manifest  []byte
	sig       []byte
}

// publish puts a release of version v with installer on the fake GitHub, its manifest signed by s; edit may change
// the manifest's fields first. A version without a prerelease part becomes releases/latest.
func (fg *fakeGitHub) publish(s testSigner, v string, installer []byte, edit ...func(fields map[string]any)) release {
	fg.t.Helper()
	sum := sha256.Sum256(installer)
	fields := withArtifact(manifestFields(v), "size", len(installer), "sha256", hex.EncodeToString(sum[:]))
	for _, e := range edit {
		e(fields)
	}
	data := encode(fg.t, fields)
	rel := release{version: v, name: "isshoni-desktop_" + v + "_windows_amd64_setup.exe", installer: installer,
		manifest: data, sig: s.sign(data)}
	fg.with(func(fg *fakeGitHub) {
		fg.assets["v"+v+"/"+ManifestName] = rel.manifest
		fg.assets["v"+v+"/"+SignatureName] = rel.sig
		fg.assets["v"+v+"/"+rel.name] = installer
		if !strings.Contains(v, "-") {
			fg.latest = v
		}
	})
	return rel
}

// listingEntry is one release of a fake API listing.
type listingEntry struct {
	tag    string
	draft  bool
	assets []string // asset names, each at its exact browser_download_url
	urls   map[string]string
}

// setListing sets the API's release listing.
func (fg *fakeGitHub) setListing(entries ...listingEntry) {
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	type rel struct {
		TagName    string  `json:"tag_name"`
		Draft      bool    `json:"draft"`
		Prerelease bool    `json:"prerelease"`
		Assets     []asset `json:"assets"`
	}
	list := make([]rel, 0, len(entries))
	for _, e := range entries {
		r := rel{TagName: e.tag, Draft: e.draft, Prerelease: strings.Contains(e.tag, "-"), Assets: []asset{}}
		for _, name := range e.assets {
			u := "https://github.com" + repoPath + "/download/" + e.tag + "/" + name
			if o, ok := e.urls[name]; ok {
				u = o
			}
			r.Assets = append(r.Assets, asset{Name: name, URL: u})
		}
		list = append(list, r)
	}
	data, err := json.Marshal(list)
	if err != nil {
		fg.t.Fatal(err)
	}
	fg.with(func(fg *fakeGitHub) { fg.listing = data })
}

// installerBytes returns n bytes of a fake installer; seed tells two installers apart.
func installerBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

// logRecorder is a slog.Handler that keeps every record.
type logRecorder struct {
	mu   sync.Mutex
	recs []logLine
}

type logLine struct {
	level slog.Level
	text  string // message and attributes
}

type logHandler struct {
	r   *logRecorder
	pre string
}

func (h logHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h logHandler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Message)
	b.WriteString(h.pre)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.r.mu.Lock()
	h.r.recs = append(h.r.recs, logLine{level: rec.Level, text: b.String()})
	h.r.mu.Unlock()
	return nil
}

func (h logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.pre)
	for _, a := range as {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	return logHandler{r: h.r, pre: b.String()}
}

func (h logHandler) WithGroup(string) slog.Handler { return h }

func (r *logRecorder) logger() *slog.Logger { return slog.New(logHandler{r: r}) }

func (r *logRecorder) lines() []logLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs)
}

// count returns the number of records at level that contain every one of subs.
func (r *logRecorder) count(level slog.Level, subs ...string) int {
	n := 0
	for _, l := range r.lines() {
		if l.level == level && !slices.ContainsFunc(subs, func(s string) bool { return !strings.Contains(l.text, s) }) {
			n++
		}
	}
	return n
}

// launch is one call of the system's launch seam.
type launch struct {
	path string
	args []string
	// attempts is pending.attempts in state.json on disk at the time of the launch.
	attempts int
}

// harness is an Updater wired to a fake GitHub and fake OS seams.
type harness struct {
	t       *testing.T
	gh      *fakeGitHub
	logs    *logRecorder
	data    string // DataDir
	dir     string // DataDir\updates
	active  testSigner
	backup  testSigner
	start   time.Time // the bubble's clock when the harness was made
	current string

	installed atomic.Bool
	enabled   atomic.Bool
	wizard    atomic.Bool
	policy    atomic.Bool
	free      atomic.Uint64

	mu        sync.Mutex
	channel   Channel
	rands     []float64 // Options.Rand's answers, the last one repeated
	launches  []launch
	launchErr error
	rename    func(oldpath, newpath string) error

	u *Updater
}

// newHarness makes a fake GitHub and an Updater of version testCurrent that is enabled, installed, past the wizard,
// on the stable channel, with plenty of free space; call it inside the bubble.
func newHarness(t *testing.T) *harness {
	t.Helper()
	data := t.TempDir()
	h := &harness{
		t: t, gh: newFakeGitHub(t), logs: &logRecorder{}, data: data, dir: filepath.Join(data, "updates"),
		active: newTestSigner(t, "isshoni-update-1"), backup: newTestSigner(t, "isshoni-update-backup-1"),
		start: time.Now(), current: testCurrent, channel: ChannelStable, rands: []float64{0.5}, rename: os.Rename,
	}
	h.installed.Store(true)
	h.enabled.Store(true)
	h.wizard.Store(true)
	h.free.Store(1 << 40)
	h.u = h.newUpdater()
	return h
}

// now is Options.Now: testEpoch plus the bubble's elapsed time.
func (h *harness) now() time.Time { return testEpoch.Add(time.Since(h.start)) }

// newUpdater makes another Updater on the same data folder, as a new start of the app would; h.current is its
// version.
func (h *harness) newUpdater() *Updater {
	h.t.Helper()
	o := Options{
		CurrentVersion: h.current,
		Channel: func() Channel {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.channel
		},
		Enabled:    h.enabled.Load,
		WizardDone: h.wizard.Load,
		DataDir:    h.data,
		HTTP:       &http.Client{Transport: h.gh.tr},
		Now:        h.now,
		Rand: func() float64 {
			h.mu.Lock()
			defer h.mu.Unlock()
			r := h.rands[0]
			if len(h.rands) > 1 {
				h.rands = h.rands[1:]
			}
			return r
		},
		Logger: h.logs.logger(),
	}
	e := env{
		signers: func() ([]TrustedKey, error) { return keys(h.active, h.backup), nil },
		ep:      defaultEndpoints(),
		sys: system{
			goos: "windows", goarch: "amd64",
			installed: func() (bool, error) { return h.installed.Load(), nil },
			freeSpace: func(string) (uint64, error) { return h.free.Load(), nil },
			launch:    h.launch,
			policy:    h.policy.Load,
			rename: func(oldpath, newpath string) error {
				h.mu.Lock()
				rename := h.rename
				h.mu.Unlock()
				return rename(oldpath, newpath)
			},
		},
	}
	u, err := newUpdater(o, e)
	if err != nil {
		h.t.Fatalf("newUpdater: %v", err)
	}
	return u
}

func (h *harness) launch(_ context.Context, path string, args []string, _ *slog.Logger) error {
	st := h.diskState()
	l := launch{path: path, args: slices.Clone(args), attempts: -1}
	if st.Pending != nil {
		l.attempts = st.Pending.Attempts
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.launches = append(h.launches, l)
	return h.launchErr
}

func (h *harness) launched() []launch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.launches)
}

func (h *harness) setRands(r ...float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rands = r
}

func (h *harness) setChannel(c Channel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.channel = c
}

// diskState reads state.json from disk (the empty state when there is none).
func (h *harness) diskState() *stateFile {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, stateName))
	if errors.Is(err, os.ErrNotExist) {
		return emptyState()
	}
	if err != nil {
		h.t.Fatal(err)
	}
	st, dropped, err := parseState(data)
	if err != nil || dropped != "" {
		h.t.Fatalf("state.json %s: %v (dropped %q)", data, err, dropped)
	}
	return st
}

// files returns the names in the updates folder.
func (h *harness) files() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// wantFiles fails the test unless the updates folder holds exactly names.
func (h *harness) wantFiles(names ...string) {
	h.t.Helper()
	got := h.files()
	slices.Sort(got)
	slices.Sort(names)
	if !slices.Equal(got, names) {
		h.t.Errorf("updates folder holds %q, want %q", got, names)
	}
}

// stage writes rel's installer into the updates folder as the pending update with attempts, as a download would
// have left it.
func (h *harness) stage(rel release, attempts int) {
	h.t.Helper()
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, rel.name), rel.installer, 0o600); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(rel.installer)
	st := h.diskState()
	st.Pending = &pendingRec{Version: rel.version, File: rel.name, Size: int64(len(rel.installer)),
		SHA256: hex.EncodeToString(sum[:]), ExpiresAt: wireTime{time.Date(2027, 5, 30, 10, 0, 0, 0, time.UTC)},
		Attempts: attempts, NotesURL: ReleasesURL + "/tag/v" + rel.version}
	data, err := marshalState(st)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, stateName), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// checkNow runs CheckNow and fails the test unless it ends in state want with the error code wantCode ("" for no
// error).
func (h *harness) checkNow(want State, wantCode string) Status {
	h.t.Helper()
	st, err := h.u.CheckNow(h.t.Context())
	if st.State != want || CodeOf(err) != wantCode || st.ErrorCode != wantCode && want == StateError {
		h.t.Fatalf("CheckNow = %+v, %v; want state %s, code %q", st, err, want, wantCode)
	}
	if wantCode == "" && err != nil {
		h.t.Fatalf("CheckNow = %+v, %v; want no error", st, err)
	}
	return st
}

// run starts Run in a goroutine and returns a stop function that ends it and checks that it returned nil.
func (h *harness) run() (stop func()) {
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)
	go func() { done <- h.u.Run(ctx) }()
	return func() {
		h.t.Helper()
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Run = %v, want nil", err)
		}
	}
}

// exactly returns the number of requests to exactly https://host/path.
func (fg *fakeGitHub) exactly(url string) int {
	n := 0
	for _, r := range fg.requests() {
		if r.String() == url {
			n++
		}
	}
	return n
}

// checks returns how many times the fake GitHub was asked for the stable manifest.
func (h *harness) checks() int {
	return h.gh.exactly("https://github.com" + repoPath + "/latest/download/" + ManifestName)
}
