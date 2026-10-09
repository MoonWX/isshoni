package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Principals the fake auth service returns for the session cookie values "user" and "admin".
var (
	userPrincipal = auth.Principal{UserID: "u1u1u1u1u1u1", Username: "Sam", Role: store.RoleUser,
		Method: auth.MethodSession, SessionID: "s1s1s1s1s1s1", ClientKind: "web"}
	adminPrincipal = auth.Principal{UserID: "a1a1a1a1a1a1", Username: "Alex", Role: store.RoleAdmin,
		Method: auth.MethodSession, SessionID: "s2s2s2s2s2s2", ClientKind: "web"}
)

// fakeAuth stands in for *auth.Service in the /api/v1 chain. CSRF is auth's real CSRFGuard trusting testOrigin (what
// Service.CSRF wraps); Authenticate reads a cookie named "session": "user" and "admin" are principals, "broken" is a
// non-API error, anything else (or no cookie) is 401 unauthenticated. Every call is recorded.
type fakeAuth struct {
	guard *auth.CSRFGuard

	mu    sync.Mutex
	calls []string
}

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	g, err := auth.NewCSRFGuard([]string{testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAuth{guard: g}
}

func (f *fakeAuth) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// take returns the recorded calls and clears them.
func (f *fakeAuth) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeAuth) CSRF(next http.Handler) http.Handler {
	guarded := f.guard.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("csrf")
		guarded.ServeHTTP(w, r)
	})
}

func (f *fakeAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	f.record("authenticate")
	c, err := r.Cookie("session")
	if err != nil {
		return auth.Principal{}, api.NewError(api.CodeUnauthenticated)
	}
	switch c.Value {
	case "user":
		return userPrincipal, nil
	case "admin":
		return adminPrincipal, nil
	case "broken":
		return auth.Principal{}, io.ErrUnexpectedEOF
	}
	return auth.Principal{}, api.NewError(api.CodeUnauthenticated)
}

func (f *fakeAuth) MaybeRotate(w http.ResponseWriter, p auth.Principal) {
	f.record("rotate")
	w.Header().Add("Set-Cookie", "session=rotated-"+string(p.UserID)+"; Path=/; Secure; HttpOnly; SameSite=Lax")
}

// fakeInfo is a fixed InfoSource.
type fakeInfo struct {
	version          string
	current, minimum int
}

func (f fakeInfo) ServerVersion() string            { return f.version }
func (f fakeInfo) Protocol() (current, minimum int) { return f.current, f.minimum }

// testVAPIDKey is a VAPID public key in the shape 04 produces (87 base64url characters of an uncompressed point).
const testVAPIDKey = "BEl62iUYgUivxIkv69yViEuiBIa-Ib9-SkvMeAtA3LFgDzkrxZJjSgSnfckjBJuBkr3qBUYIHBQFLXYp5Nksh8U"

// fakePush is a Push with a fixed key. It accepts every endpoint and queues every test unless validate or sendErr
// say otherwise, and records what it was asked.
type fakePush struct {
	key string
	// validate decides ValidateEndpoint; nil accepts every endpoint.
	validate func(endpoint string) error
	// sendErr is SendTest's answer.
	sendErr error

	mu        sync.Mutex
	validated []string                   // the endpoints ValidateEndpoint saw
	tests     [][]store.PushSubscription // the subscriptions of each SendTest call
}

func (f *fakePush) VAPIDPublicKey() string { return f.key }

func (f *fakePush) ValidateEndpoint(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validated = append(f.validated, endpoint)
	if f.validate != nil {
		return f.validate(endpoint)
	}
	return nil
}

func (f *fakePush) SendTest(_ context.Context, subs []store.PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tests = append(f.tests, subs)
	return f.sendErr
}

// takeValidated returns the endpoints ValidateEndpoint saw so far and clears them.
func (f *fakePush) takeValidated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.validated
	f.validated = nil
	return out
}

// takeTests returns the subscriptions of the SendTest calls so far and clears them.
func (f *fakePush) takeTests() [][]store.PushSubscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.tests
	f.tests = nil
	return out
}

// fakeSignal is a Signal that records its hooks; it shows that the interface can be implemented in full.
type fakeSignal struct {
	mu    sync.Mutex
	calls []string
	// notes are the Notify calls again, each with its target: "admins [admin.invites]", "user <id> [devices]",
	// "all [me]" (takeNotes).
	notes []string
	// presence is what RoomPresence returns (setPresence); nil means two participants and one share in the Lounge.
	presence map[store.RoomID]RoomPresence
}

func (f *fakeSignal) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeSignal) RoomPresence() map[store.RoomID]RoomPresence {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.presence == nil {
		return map[store.RoomID]RoomPresence{store.DefaultRoomID: {Participants: 2, Shares: 1}}
	}
	return maps.Clone(f.presence)
}

// setPresence replaces the hub snapshot that RoomPresence returns.
func (f *fakeSignal) setPresence(p map[store.RoomID]RoomPresence) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = p
}

func (f *fakeSignal) OnlineUserIDs() map[store.UserID]struct{} {
	return map[store.UserID]struct{}{userPrincipal.UserID: {}}
}

func (f *fakeSignal) RoomDeleted(id store.RoomID) { f.record("room_deleted " + string(id)) }

func (f *fakeSignal) UserChanged(id store.UserID, _ string, _ bool) {
	f.record("user_changed " + string(id))
}

func (f *fakeSignal) Notify(t NotifyTarget, topics ...protocol.Topic) {
	f.record(fmt.Sprint("notify ", topics))
	var target string
	switch t {
	case NotifyTarget{}:
		target = "nobody" // no handler means that
	case NotifyTarget{Admins: true}:
		target = "admins"
	case NotifyTarget{All: true}:
		target = "all"
	case NotifyTarget{UserID: t.UserID}:
		target = "user " + string(t.UserID)
	default:
		target = fmt.Sprintf("%+v", t) // more than one of the three: no handler means that either
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, fmt.Sprint(target, " ", topics))
}

// takeNotes returns the Notify calls recorded so far, each with its target, and clears them (and the hook list).
func (f *fakeSignal) takeNotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.notes
	f.notes, f.calls = nil, nil
	return out
}

var (
	_ Signal     = (*fakeSignal)(nil)
	_ Push       = (*fakePush)(nil)
	_ InfoSource = fakeInfo{}
)

// openTestDB opens a fresh store in a temporary directory, closed when the test ends.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:       filepath.Join(dir, "isshoni.db"),
		AppVersion: "0.1.0",
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// apiFixture is the whole stack: 04's router with the API mounted at /api/v1/, a real store, the fake auth service
// and fake Info and Push sources.
type apiFixture struct {
	*fixture
	api  *API
	db   *store.DB
	auth *fakeAuth
}

func newAPIFixture(t *testing.T, mod func(*Deps)) *apiFixture {
	t.Helper()
	db := openTestDB(t)
	fa := newFakeAuth(t)
	d := Deps{
		DB:     db,
		Signal: &fakeSignal{},
		Push:   &fakePush{key: testVAPIDKey},
		Info:   fakeInfo{version: "1.2.3", current: 3, minimum: 2},
		Site:   domainSite(),
		Logger: slog.New(slog.DiscardHandler),
	}
	if mod != nil {
		mod(&d)
	}
	a := newAPI(d, fa)
	f := newFixture(t, func(o *RouterOptions) { o.API = a })
	return &apiFixture{fixture: f, api: a, db: db, auth: fa}
}

// do serves one request with an optional body and header pairs. A nil body sends none.
func (f *apiFixture) do(method, path string, body io.Reader, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, testOrigin+path, body)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	return f.serve(req)
}

// sameOriginJSON are the headers of a same-origin fetch from the SPA with a JSON body.
var sameOriginJSON = []string{"Sec-Fetch-Site", "same-origin", "Origin", testOrigin, "Content-Type", "application/json"}

// withCookie appends a session cookie to header pairs.
func withCookie(value string, header ...string) []string {
	return append(slices.Clone(header), "Cookie", "session="+value)
}
