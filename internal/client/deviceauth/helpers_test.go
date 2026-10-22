package deviceauth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The tests run in testing/synctest bubbles, where time only moves when every goroutine is durably blocked. A real
// socket never is, so the httptest servers listen on an in-memory network instead: net.Pipe connections handed
// over a channel, which block durably inside the bubble. The client's Transport dials that network whatever the
// host, so every origin reaches the one fake server, which can tell them apart by the Host header.

const (
	testOrigin  = "http://127.0.0.1:8443"
	testVersion = "0.2.0"
	testUserID  = "k3m9p2qxw7ht"
	testDevice  = "m4q8x2c6v0bn"
)

var testApp = deviceauth.AppInfo{
	Version:    testVersion,
	ClientKind: protocol.ClientKindDesktop,
	OS:         protocol.ClientOSWindows,
	DeviceName: "DESKTOP-7F3K2LQ",
}

// pipeAddr is the in-memory listener's address.
type pipeAddr string

func (a pipeAddr) Network() string { return "tcp" }
func (a pipeAddr) String() string  { return string(a) }

// pipeListener is a net.Listener over net.Pipe connections.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	down  atomic.Bool  // dials fail: the server is unreachable
	dials atomic.Int64 // dial attempts, failed ones included
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

func (l *pipeListener) Addr() net.Addr { return pipeAddr("127.0.0.1:8443") }

// DialContext is the client Transport's dialer: a new pipe whatever the address.
func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	l.dials.Add(1)
	if l.down.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused (test)")}
	}
	client, server := net.Pipe()
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

// request is what the fake server saw.
type request struct {
	Method, Path, Host string
	Header             http.Header
	Body               []byte
	At                 time.Time
}

func (r request) tokenRequest(t *testing.T) api.DeviceTokenRequest {
	t.Helper()
	var req api.DeviceTokenRequest
	if err := json.Unmarshal(r.Body, &req); err != nil {
		t.Fatalf("token request body: %v", err)
	}
	return req
}

// reply is a scripted answer: an HTTP status and a body to encode as JSON (nil = no body).
type reply struct {
	status int
	body   any
	header http.Header
}

func ok(body any) reply { return reply{status: http.StatusOK, body: body} }

func apiErr(code string, params map[string]any) reply {
	return reply{status: api.StatusOf(code), body: api.ErrorResponse{Error: api.Error{Code: code, Params: params}}}
}

// fakeServer is a scripted isshoni server: /api/v1/info, the device routes and whatever other answers a test sets.
// Its scripts are read under mu, so a test may change them with with().
type fakeServer struct {
	t      *testing.T
	ln     *pipeListener
	srv    *httptest.Server
	tr     *http.Transport
	origin string

	mu       sync.Mutex
	info     api.Info
	codeURIs func(code string) (uri, complete string) // nil = the server's own origin
	poll     func(n int) reply                        // the device_code grant's n-th answer (from 0)
	refresh  func(req api.DeviceTokenRequest, n int) reply
	password func(req api.DevicePasswordRequest) reply
	revoke   func(r *http.Request) reply
	hang     func(r *http.Request) bool // block until the client gives up
	before   func(r *http.Request)      // runs first, without mu, holding the request back until it returns
	override map[string]reply           // path → a fixed answer
	requests []request
	pollN    int
	refreshN int
	issued   int
}

// newFakeServer starts a fake server on the in-memory network; call it inside the bubble.
func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := newScriptedServer(t, testOrigin)
	fs.ln = newPipeListener()
	fs.srv = httptest.NewUnstartedServer(http.HandlerFunc(fs.serve))
	_ = fs.srv.Listener.Close()
	fs.srv.Listener = fs.ln
	fs.srv.Start()
	fs.tr = &http.Transport{DialContext: fs.ln.DialContext, DisableKeepAlives: true}
	t.Cleanup(func() {
		fs.tr.CloseIdleConnections()
		fs.srv.Close()
	})
	return fs
}

// newTCPFakeServer starts the fake server on a real loopback port, outside any bubble: for the tests of what the
// package's own keep-alive Transport leaves behind. Its origin is the port's; fs.ln and fs.tr are nil.
func newTCPFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := newScriptedServer(t, "")
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.serve))
	fs.with(func(fs *fakeServer) {
		fs.origin = fs.srv.URL
		fs.info.Server.PublicURL = fs.srv.URL
	})
	t.Cleanup(fs.srv.Close)
	return fs
}

// newScriptedServer is a fake server's scripts with their defaults: /info names origin, polls are pending, and
// refreshes and passwords get a new pair.
func newScriptedServer(t *testing.T, origin string) *fakeServer {
	fs := &fakeServer{t: t, origin: origin}
	fs.info = api.Info{
		Server:       api.InfoServer{Name: "Alex's server", Version: "0.2.0", PublicURL: origin},
		Protocol:     api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion},
		Registration: api.RegistrationModeInvite,
		Features:     []string{api.InfoFeaturePasswordReset, api.InfoFeatureDeviceFlow},
	}
	fs.poll = func(int) reply { return apiErr(api.CodeAuthorizationPending, nil) }
	fs.refresh = func(api.DeviceTokenRequest, int) reply { return ok(fs.pairLocked()) }
	fs.password = func(api.DevicePasswordRequest) reply { return ok(fs.pairLocked()) }
	fs.revoke = func(*http.Request) reply { return reply{status: http.StatusNoContent} }
	return fs
}

// httpClient is DefaultHTTPClient on the in-memory network.
func (fs *fakeServer) httpClient() *http.Client {
	hc := deviceauth.DefaultHTTPClient()
	hc.Transport = fs.tr
	return hc
}

// client returns a Client for origin (testOrigin when "").
func (fs *fakeServer) client(origin string) *deviceauth.Client {
	fs.t.Helper()
	if origin == "" {
		origin = fs.origin
	}
	c, err := deviceauth.NewClient(origin, testApp,
		deviceauth.ClientOptions{HTTP: fs.httpClient(), AllowInsecureLoopback: true})
	if err != nil {
		fs.t.Fatalf("NewClient: %v", err)
	}
	return c
}

func (fs *fakeServer) with(f func(fs *fakeServer)) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f(fs)
}

// pairLocked issues a new token pair.
func (fs *fakeServer) pairLocked() api.DeviceTokenResponse {
	fs.issued++
	return api.DeviceTokenResponse{
		AccessToken:      accessToken(fs.issued),
		TokenType:        "Bearer",
		ExpiresIn:        900,
		RefreshToken:     refreshToken(fs.issued),
		RefreshExpiresIn: 7776000,
		Device:           api.DeviceRef{ID: testDevice, Name: "Test PC"},
		User:             api.User{ID: testUserID, Username: "Alex", Role: api.RoleAdmin},
	}
}

// codeResponseLocked is a valid /device/code answer for the server's origin.
func (fs *fakeServer) codeResponseLocked() api.DeviceCodeResponse {
	uri, complete := fs.origin+"/link", fs.origin+"/link?code=WDJB-MJHT"
	if fs.codeURIs != nil {
		uri, complete = fs.codeURIs("WDJB-MJHT")
	}
	return api.DeviceCodeResponse{
		DeviceCode:              b64("device-code"),
		UserCode:                "WDJB-MJHT",
		VerificationURI:         uri,
		VerificationURIComplete: complete,
		ExpiresIn:               600,
		Interval:                5,
	}
}

func (fs *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fs.mu.Lock()
	fs.requests = append(fs.requests, request{
		Method: r.Method, Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone(), Body: body, At: time.Now(),
	})
	before := fs.before
	fs.mu.Unlock()
	if before != nil {
		before(r)
	}
	fs.mu.Lock()
	hang := fs.hang != nil && fs.hang(r)
	fs.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	fs.mu.Lock()
	rep, fixed := fs.override[r.URL.Path]
	if !fixed {
		rep = fs.answerLocked(r, body)
	}
	fs.mu.Unlock()
	for k, v := range rep.header {
		w.Header()[k] = v
	}
	if rep.body == nil {
		w.WriteHeader(rep.status)
		return
	}
	if raw, isRaw := rep.body.([]byte); isRaw {
		w.WriteHeader(rep.status)
		_, _ = w.Write(raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_ = json.NewEncoder(w).Encode(rep.body)
}

func (fs *fakeServer) answerLocked(r *http.Request, body []byte) reply {
	switch r.URL.Path {
	case "/api/v1/info":
		return ok(fs.info)
	case "/api/v1/device/code":
		return ok(fs.codeResponseLocked())
	case "/api/v1/device/token":
		var req api.DeviceTokenRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return apiErr(api.CodeBadRequest, nil)
		}
		switch req.GrantType {
		case api.GrantTypeDeviceCode:
			n := fs.pollN
			fs.pollN++
			return fs.poll(n)
		case api.GrantTypeRefreshToken:
			n := fs.refreshN
			fs.refreshN++
			return fs.refresh(req, n)
		}
		return apiErr(api.CodeValidationFailed, nil)
	case "/api/v1/device/password":
		var req api.DevicePasswordRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return apiErr(api.CodeBadRequest, nil)
		}
		return fs.password(req)
	case "/api/v1/device/revoke":
		return fs.revoke(r)
	}
	return apiErr(api.CodeNotFound, nil)
}

// seen returns the requests for path ("" = all).
func (fs *fakeServer) seen(path string) []request {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []request
	for _, r := range fs.requests {
		if path == "" || r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// refreshes returns the refresh_token grant requests.
func (fs *fakeServer) refreshes() []api.DeviceTokenRequest {
	fs.t.Helper()
	var out []api.DeviceTokenRequest
	for _, r := range fs.seen("/api/v1/device/token") {
		if req := r.tokenRequest(fs.t); req.GrantType == api.GrantTypeRefreshToken {
			out = append(out, req)
		}
	}
	return out
}

func (fs *fakeServer) polls() []request {
	var out []request
	for _, r := range fs.seen("/api/v1/device/token") {
		if r.tokenRequest(fs.t).GrantType == api.GrantTypeDeviceCode {
			out = append(out, r)
		}
	}
	return out
}

// b64 is a canonical 43-character base64url value of 32 bytes derived from seed.
func b64(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func accessToken(n int) string  { return "isa_" + b64("access-"+strconv.Itoa(n)) }
func refreshToken(n int) string { return "isr_" + b64("refresh-"+strconv.Itoa(n)) }

// seedToken is the refresh token of a credential a test stores before the manager starts.
var seedToken = "isr_" + b64("seed")

// eventLog records OnState calls and a test's own events in one order.
type eventLog struct {
	mu     sync.Mutex
	events []string
	states []deviceauth.State
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) onState(s deviceauth.State) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, "state:"+string(s.Status))
	l.states = append(l.states, s)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// index returns the position of the first event e, or -1.
func (l *eventLog) index(e string) int { return slices.Index(l.list(), e) }

// orderStore is a Store that logs its writes into an eventLog and can be made to fail them.
type orderStore struct {
	credstore.Store
	log      *eventLog
	failPuts atomic.Int64 // the next this many Puts fail
	failAll  atomic.Bool  // every Put fails
	puts     atomic.Int64
	onPut    func(c credstore.Credential)
}

var errStoreDown = errors.New("store down (test)")

func (s *orderStore) Put(origin string, c credstore.Credential) error {
	s.puts.Add(1)
	if s.onPut != nil {
		s.onPut(c)
	}
	if s.failAll.Load() || s.failPuts.Add(-1) >= 0 {
		s.log.add("put-failed")
		return errStoreDown
	}
	s.log.add("put")
	return s.Store.Put(origin, c)
}

func (s *orderStore) Delete(origin string) error {
	s.log.add("delete")
	return s.Store.Delete(origin)
}

// env is a manager on a fake server with a memory store.
type env struct {
	t     *testing.T
	fs    *fakeServer
	store *orderStore
	log   *eventLog
	logs  *logBuffer
	m     *deviceauth.Manager
}

// newEnv makes an env inside the bubble; linked stores a credential first. The manager is closed by t.Cleanup.
func newEnv(t *testing.T, linked bool) *env {
	t.Helper()
	fs := newFakeServer(t)
	log := &eventLog{}
	store := &orderStore{Store: credstore.NewMemory(), log: log}
	if linked {
		seedCredential(t, store.Store, testOrigin)
	}
	logs := &logBuffer{}
	m := deviceauth.NewManager(deviceauth.ManagerOptions{
		Client:  fs.client(""),
		Store:   store,
		OnState: log.onState,
		Logger:  logs.logger(),
	})
	t.Cleanup(func() { _ = m.Close() })
	return &env{t: t, fs: fs, store: store, log: log, logs: logs, m: m}
}

// logBuffer collects a manager's log lines at every level.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newMemoryStore() credstore.Store { return credstore.NewMemory() }

// quiet drops the log lines of managers and servers whose logs a test doesn't read.
var quiet = slog.New(slog.DiscardHandler)

func seedCredential(t *testing.T, s credstore.Store, origin string) {
	t.Helper()
	err := s.Put(origin, credstore.Credential{RefreshToken: seedToken, DeviceID: testDevice, UserName: "Alex",
		SavedAt: time.Now()})
	if err != nil {
		t.Fatalf("seed the credential: %v", err)
	}
}

// storedToken returns the refresh token the store holds for origin, or "".
func storedToken(t *testing.T, s credstore.Store, origin string) string {
	t.Helper()
	c, err := s.Get(origin)
	if errors.Is(err, credstore.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	return c.RefreshToken
}

func wantStatus(t *testing.T, m *deviceauth.Manager, status deviceauth.Status, notice deviceauth.Notice) {
	t.Helper()
	s := m.State()
	if s.Status != status || s.Notice != notice {
		t.Fatalf("state = %s/%q, want %s/%q", s.Status, s.Notice, status, notice)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// relinker and updater are the methods 01-client-core reads from an Auth error's chain.
type relinker interface{ RelinkRequired() bool }
type updater interface{ UpdateRequired() bool }

func relinkRequired(err error) bool {
	var r relinker
	return errors.As(err, &r) && r.RelinkRequired()
}

func updateRequired(err error) bool {
	var u updater
	return errors.As(err, &u) && u.UpdateRequired()
}

func describe(err error) string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v (%T)", err, err)
}
