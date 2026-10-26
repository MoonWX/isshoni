package bridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// fakeCore is the app as the services see it.
type fakeCore struct {
	mu        sync.Mutex
	dev       bool
	live      bool
	quits     []bool
	opened    []string
	pages     []app.OSPage
	logs      []string
	ownConns  []string
	pending   *app.PendingLink
	handled   int
	probe     media.Probe
	probeErr  error
	openErr   error
	heartbeat int
	shares    *fakeShares
}

var _ Core = (*fakeCore)(nil)

func (c *fakeCore) Info() app.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	return app.Info{Version: "0.0.0-test", OS: "windows", OSBuild: 26100, Arch: "amd64", Dev: c.dev}
}
func (c *fakeCore) Heartbeat() { c.mu.Lock(); c.heartbeat++; c.mu.Unlock() }
func (c *fakeCore) OpenExternal(url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !app.ValidExternalURL(url, c.dev) {
		return app.ErrInvalidArgument
	}
	c.opened = append(c.opened, url)
	return c.openErr
}
func (c *fakeCore) OpenOSSettings(p app.OSPage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages = append(c.pages, p)
	return nil
}
func (c *fakeCore) Live() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "lounge", c.live
}
func (c *fakeCore) Quit(stopShare bool) {
	c.mu.Lock()
	c.quits = append(c.quits, stopShare)
	c.mu.Unlock()
}
func (c *fakeCore) Log(level, msg string, _ map[string]string) {
	c.mu.Lock()
	c.logs = append(c.logs, level+" "+msg)
	c.mu.Unlock()
}
func (c *fakeCore) OpenLogsFolder() error { return app.ErrNotImplemented }
func (c *fakeCore) OpenNotices() error    { return app.ErrNotImplemented }
func (c *fakeCore) PendingLink() *app.PendingLink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
}
func (c *fakeCore) LinkHandled() { c.mu.Lock(); c.handled++; c.mu.Unlock() }
func (c *fakeCore) OwnConnections(ids []string) error {
	if len(ids) > app.MaxOwnConnections {
		return app.ErrInvalidArgument
	}
	c.mu.Lock()
	c.ownConns = ids
	c.mu.Unlock()
	return nil
}
func (c *fakeCore) Probe(context.Context) (media.Probe, error) { return c.probe, c.probeErr }
func (c *fakeCore) Shares() app.ShareController {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shares == nil {
		c.shares = &fakeShares{}
	}
	return c.shares
}

// fakeShares is the share controller: it records each call, answers with the configured results and keeps the one
// subscriber the services register.
type fakeShares struct {
	mu        sync.Mutex
	calls     []string
	picked    *share.Picked
	view      share.View
	current   *share.View
	stats     *share.Stats
	err       error         // every error-returning call's
	startCtx  time.Duration // the time left on Start's ctx
	startReq  share.StartRequest
	stopCause share.Cause
	watch     []bool
	sub       func(share.Event)
}

var _ app.ShareController = (*fakeShares)(nil)

func (f *fakeShares) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeShares) Pick(context.Context) (*share.Picked, error) {
	f.record("Pick")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.picked, f.err
}
func (f *fakeShares) Release(pickID string) { f.record("Release " + pickID) }
func (f *fakeShares) Start(ctx context.Context, r share.StartRequest) (share.View, error) {
	f.record("Start")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startReq = r
	if d, ok := ctx.Deadline(); ok {
		f.startCtx = time.Until(d)
	}
	return f.view, f.err
}
func (f *fakeShares) Current() *share.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}
func (f *fakeShares) Stop(_ context.Context, cause share.Cause) {
	f.record("Stop")
	f.mu.Lock()
	f.stopCause = cause
	f.mu.Unlock()
}
func (f *fakeShares) StopOther(_ context.Context, roomID, shareID string) error {
	f.record("StopOther " + roomID + " " + shareID)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func (f *fakeShares) SetPaused(_ context.Context, paused bool) error {
	f.record(fmt.Sprintf("SetPaused %v", paused))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func (f *fakeShares) SetPreset(_ context.Context, p protocol.Preset) error {
	f.record("SetPreset " + string(p))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func (f *fakeShares) SetAudioEnabled(on bool) error {
	f.record(fmt.Sprintf("SetAudioEnabled %v", on))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func (f *fakeShares) Stats() *share.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}
func (f *fakeShares) WatchServerStats(on bool) {
	f.mu.Lock()
	f.watch = append(f.watch, on)
	f.mu.Unlock()
}
func (f *fakeShares) Subscribe(fn func(share.Event)) func() {
	f.mu.Lock()
	f.sub = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.sub = nil
		f.mu.Unlock()
	}
}

// send delivers ev to the subscriber, as the controller does.
func (f *fakeShares) send(ev share.Event) {
	f.mu.Lock()
	fn := f.sub
	f.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

func (f *fakeShares) set(fn func(f *fakeShares)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeShares) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeServers is 02's server list: a list and the active manager, which a test may switch at any time.
type fakeServers struct {
	mu      sync.Mutex
	entries []deviceauth.ServerEntry
	active  *fakeManager
}

func (s *fakeServers) List() []deviceauth.ServerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries
}

func (s *fakeServers) Active() Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil
	}
	return s.active
}

func (s *fakeServers) setActive(m *fakeManager) {
	s.mu.Lock()
	s.active = m
	s.mu.Unlock()
}

// fakeManager is one server's 02 Manager. It records every call that would touch the server or a token.
type fakeManager struct {
	origin string

	mu         sync.Mutex
	state      deviceauth.State
	token      protocol.Secret
	expires    time.Time
	tokenErr   error
	tokenCalls []bool // the force argument of each AccessTokenWithExpiry
	sessions   []protocol.ErrorCode
	hellos     []*protocol.Error
	linkErr    error
	linkCalls  int
	linkBlock  bool       // Link waits for its ctx to end and returns its error
	linkCtxErr chan error // with linkBlock: the ctx error Link saw
	pwErr      error
	pwCalls    []string // the usernames
	recheckErr error
	cancels    int
}

func newFakeManager(origin string, status deviceauth.Status) *fakeManager {
	return &fakeManager{origin: origin, state: deviceauth.State{Origin: origin, ServerName: "Friends", Status: status},
		token: protocol.Secret("isa_" + origin), expires: time.UnixMilli(1762110845123)}
}

func (m *fakeManager) Origin() string { return m.origin }

func (m *fakeManager) State() deviceauth.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *fakeManager) Link(ctx context.Context) error {
	m.mu.Lock()
	m.linkCalls++
	block, ch := m.linkBlock, m.linkCtxErr
	m.mu.Unlock()
	if block {
		<-ctx.Done()
		if ch != nil {
			ch <- ctx.Err()
		}
		return ctx.Err()
	}
	return m.linkErr
}

func (m *fakeManager) CancelLink() { m.mu.Lock(); m.cancels++; m.mu.Unlock() }

func (m *fakeManager) LinkWithPassword(_ context.Context, username string, _ protocol.Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pwCalls = append(m.pwCalls, username)
	if m.pwErr == nil {
		m.state.Status = deviceauth.StatusLinked
	}
	return m.pwErr
}

func (m *fakeManager) AccessTokenWithExpiry(_ context.Context, force bool) (protocol.Secret, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenCalls = append(m.tokenCalls, force)
	if m.tokenErr != nil {
		return "", time.Time{}, m.tokenErr
	}
	return m.token, m.expires, nil
}

func (m *fakeManager) SessionEnded(_ context.Context, code protocol.ErrorCode) deviceauth.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, code)
	return m.state
}

func (m *fakeManager) HelloRefused(e *protocol.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hellos = append(m.hellos, e)
}

func (m *fakeManager) Recheck(context.Context) (deviceauth.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.recheckErr
}

func (m *fakeManager) Unlink(context.Context) error { return nil }

// tokenCallCount is the number of AccessTokenWithExpiry calls so far.
func (m *fakeManager) tokenCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tokenCalls)
}

// fakeAudio is 05's service.
type fakeAudio struct {
	mu          sync.Mutex
	watches     []bool
	levels      []bool
	watchErr    error
	decided     []string
	dismissed   []string
	decideErr   error
	appErr      error
	tipErr      error
	soundChecks int
	subscribed  func(audio.Event)
	cancelled   bool
}

var _ AudioPolicy = (*fakeAudio)(nil)

func (a *fakeAudio) FriendsHear() audio.FriendsHear {
	return audio.FriendsHear{Seq: 7, Engine: "include-set"}
}
func (a *fakeAudio) AppRules() []audio.AppRule { return nil }
func (a *fakeAudio) Watch(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watches = append(a.watches, on)
	return a.watchErr
}
func (a *fakeAudio) WatchLevels(on bool) { a.mu.Lock(); a.levels = append(a.levels, on); a.mu.Unlock() }
func (a *fakeAudio) Decide(promptID string, d audio.Decision) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decided = append(a.decided, promptID+" "+string(d))
	return a.decideErr
}
func (a *fakeAudio) DismissPrompt(promptID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dismissed = append(a.dismissed, promptID)
	return a.decideErr
}
func (a *fakeAudio) SetAppDecision(string, audio.Decision) error { return a.appErr }
func (a *fakeAudio) SetKeepOutMicUsers(bool) error               { return nil }
func (a *fakeAudio) DismissTip(string) error                     { return a.tipErr }
func (a *fakeAudio) SoundCheck(context.Context) (media.SoundCheckResult, error) {
	a.mu.Lock()
	a.soundChecks++
	a.mu.Unlock()
	return media.SoundCheckResult{Captured: true, Backend: "process-loopback"}, nil
}
func (a *fakeAudio) Subscribe(fn func(audio.Event)) func() {
	a.mu.Lock()
	a.subscribed = fn
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.cancelled = true
		a.subscribed = nil
		a.mu.Unlock()
	}
}

// fakeShell records SetFullscreen.
type fakeShell struct{ fullscreen []bool }

func (s *fakeShell) SetFullscreen(on bool) { s.fullscreen = append(s.fullscreen, on) }

// testLog is a logger that writes to buf at debug level.
func testLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTestServices2 builds the services over any Core.
func newTestServices2(t *testing.T, core Core, buf *bytes.Buffer) *Services {
	t.Helper()
	s, err := New(Deps{App: core, Servers: &fakeServers{}, Log: testLog(buf), Shell: &fakeShell{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newTestServices builds the services over fakes; the returned values are the fakes.
func newTestServices(core *fakeCore, srv *fakeServers, au AudioPolicy, buf *bytes.Buffer) *Services {
	d := Deps{App: core, Servers: srv, Log: testLog(buf), Shell: &fakeShell{}}
	if au != nil {
		d.Audio = au
	}
	s, err := New(d)
	if err != nil {
		panic(err)
	}
	return s
}
