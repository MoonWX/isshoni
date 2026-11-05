package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/web"
)

// WA68: the tray, the close policy, toasts with nonces, the system events of 03 §11.6, keep-awake, WDA on the main
// window, the heartbeat watchdog and the quit hooks (W7, W14, W17, W18).

// sysShell is the shell with what WA68 adds: toasts, the tray and the window's visibility, recorded.
type sysShell struct {
	*fakeShell
	mu      sync.Mutex
	toasts  []Notification
	trays   []tray.View
	hidden  bool
	hwnd    uintptr
	onQuit  func() // Wails' OnShutdown, as the real shell calls it from its Quit
	visible func() bool
}

func (s *sysShell) Notify(n Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toasts = append(s.toasts, n)
	return nil
}

func (s *sysShell) SetTray(v tray.View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trays = append(s.trays, v)
}

func (s *sysShell) HideWindow() {
	s.mu.Lock()
	s.hidden = true
	s.mu.Unlock()
	s.fakeShell.HideWindow()
}

func (s *sysShell) ShowWindow(focus bool) {
	s.mu.Lock()
	s.hidden = false
	s.mu.Unlock()
	s.fakeShell.ShowWindow(focus)
}

func (s *sysShell) WindowVisible() bool {
	if s.visible != nil {
		return s.visible()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.hidden
}

func (s *sysShell) MainWindowHandle() uintptr { return s.hwnd }

func (s *sysShell) Quit() {
	s.fakeShell.Quit()
	if s.onQuit != nil {
		s.onQuit()
	}
}

func (s *sysShell) toastsOf(kind string) []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Notification
	for _, n := range s.toasts {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func (s *sysShell) lastTray() (tray.View, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.trays) == 0 {
		return tray.View{}, 0
	}
	return s.trays[len(s.trays)-1], len(s.trays)
}

// recOS is OSHooks that records keep-awake, WDA and the end-of-session handshake.
type recOS struct {
	fakeOS
	mu       sync.Mutex
	awake    []bool
	excluded []uintptr
	included []uintptr
	acks     int
	events   chan SystemEvent
}

func (o *recOS) KeepAwake(on bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.awake = append(o.awake, on)
}

func (o *recOS) ExcludeFromCapture(h uintptr) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.excluded = append(o.excluded, h)
	return nil
}

func (o *recOS) IncludeInCapture(h uintptr) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.included = append(o.included, h)
	return nil
}

func (o *recOS) EndSessionHandled() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.acks++
}

func (o *recOS) Events() <-chan SystemEvent { return o.events }

func (s *sysEnv) acks() int {
	s.os.mu.Lock()
	defer s.os.mu.Unlock()
	return s.os.acks
}

func (o *recOS) awakeCalls() []bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.awake)
}

// fakeTimers are the lock pause's timers, fired by the test.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }

func (ft *fakeTimers) after(d time.Duration, f func()) stopper {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return t
}

// fire runs the last timer that isn't stopped; false when there is none.
func (ft *fakeTimers) fire() bool {
	ft.mu.Lock()
	var t *fakeTimer
	for i := len(ft.timers) - 1; i >= 0; i-- {
		if !ft.timers[i].stopped {
			t = ft.timers[i]
			break
		}
	}
	ft.mu.Unlock()
	if t == nil {
		return false
	}
	t.stopped = true
	t.f()
	return true
}

func (ft *fakeTimers) pending() []time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []time.Duration
	for _, t := range ft.timers {
		if !t.stopped {
			out = append(out, t.d)
		}
	}
	return out
}

// pauseClient is C2 with share.pause: its share's SetPaused sets the snapshot's Paused.
type pauseClient struct {
	features []protocol.Feature
	mu       sync.Mutex
	sh       *pauseShare
}

func (c *pauseClient) Status() core.Status {
	return core.Status{Conn: core.ConnOnline, ConnectionID: "c_k3v9q2m7xw4pa8d1", Features: c.features}
}
func (c *pauseClient) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}
func (c *pauseClient) StartShare(_ context.Context, r core.ShareRequest) (share.Share, error) {
	s := &pauseShare{done: make(chan struct{}), snap: core.ShareSnapshot{ID: "s_q7m2k9x4w3bd8c1a", RoomID: r.RoomID,
		State: core.StateLive, Watchers: []protocol.Watcher{{UserID: "u2"}}}}
	c.mu.Lock()
	c.sh = s
	c.mu.Unlock()
	return s, nil
}
func (c *pauseClient) StopOther(context.Context, string, string) error { return nil }
func (c *pauseClient) WatchServerStats(bool)                           {}

type pauseShare struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	snap core.ShareSnapshot
	sets []bool
}

func (s *pauseShare) Snapshot() core.ShareSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}
func (s *pauseShare) SetPreset(context.Context, protocol.Preset) error { return nil }
func (s *pauseShare) SetPaused(_ context.Context, p bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Paused = p
	s.sets = append(s.sets, p)
	return nil
}
func (s *pauseShare) Stop(ctx context.Context) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.snap.State, s.snap.End = core.StateEnded, &core.EndInfo{Reason: core.EndUser, ByThisApp: true}
		s.mu.Unlock()
		close(s.done)
	})
	return ctx.Err()
}
func (s *pauseShare) Done() <-chan struct{} { return s.done }

func (s *pauseShare) pauses() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sets)
}

type pausePublisher struct{ c *pauseClient }

func (p pausePublisher) Ready(context.Context) (share.Client, string, error) {
	return p.c, "https://a.example.com", nil
}

// sysEnv is an app whose share controller shares through pauseClient, with sysShell, recOS and fake timers.
type sysEnv struct {
	*env
	shell  *sysShell
	os     *recOS
	timers *fakeTimers
	client *pauseClient
}

func newSysEnv(t *testing.T, features ...protocol.Feature) *sysEnv {
	t.Helper()
	if features == nil {
		features = []protocol.Feature{protocol.FeatureSharePause}
	}
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	cat, err := i18n.Load(web.DesktopCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &sysEnv{env: e, shell: &sysShell{fakeShell: e.shell, hwnd: 0x1234}, os: &recOS{},
		timers: &fakeTimers{}, client: &pauseClient{features: features}}
	eng := fake.NewEngine(fake.EngineConfig{})
	t.Cleanup(func() { _ = eng.Close() })
	a := e.app
	a.d.Shell, a.d.OS, a.d.I18n, a.d.Engine = s.shell, s.os, cat, eng
	a.after = s.timers.after
	a.trayEvery = time.Millisecond
	a.clients = watchClients(pausePublisher{c: s.client})
	ctl := newShares(a.d, a.clients)
	t.Cleanup(ctl.Close)
	a.shares, a.sharing = ctl, ctl.Live
	unsubscribe := ctl.Subscribe(a.onShareEvent)
	t.Cleanup(unsubscribe)
	return s
}

// goLive picks and starts a share and waits until the view is live and keep-awake is on.
func (s *sysEnv) goLive(t *testing.T) {
	t.Helper()
	p, err := s.app.shares.Pick(context.Background())
	if err != nil || p == nil {
		t.Fatalf("Pick = %+v, %v", p, err)
	}
	if _, err := s.app.shares.Start(context.Background(), share.StartRequest{PickID: p.PickID,
		RoomID: "lounge"}); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StateLive)
	s.waitAwake(t, true)
}

func (s *sysEnv) state() share.State {
	if v := s.app.shares.Current(); v != nil {
		return v.State
	}
	return ""
}

func (s *sysEnv) waitState(t *testing.T, want share.State) *share.View {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if v := s.app.shares.Current(); v != nil && v.State == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("the share is %q, want %q", s.state(), want)
		}
	}
}

// waitAwake waits until the last KeepAwake call is want (the share's events reach the app asynchronously).
func (s *sysEnv) waitAwake(t *testing.T, want bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		calls := s.os.awakeCalls()
		if len(calls) > 0 && calls[len(calls)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("keep-awake calls %v, want the last to be %v", calls, want)
		}
	}
}

func (s *sysEnv) waitTray(t *testing.T, ok func(tray.View) bool, what string) tray.View {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		v, n := s.shell.lastTray()
		if n > 0 && ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("tray %+v: %s", v, what)
		}
	}
}

// TestLockPausesUnlockResumes: a lock pauses a live share through the controller (01's SetPaused) and releases
// keep-awake; an unlock at 4 min resumes it and takes keep-awake again (03 §11.6, §22.1's share row).
func TestLockPausesUnlockResumes(t *testing.T) {
	s := newSysEnv(t)
	s.goLive(t)

	s.app.handleSystemEvent(EventLock)
	s.waitState(t, share.StatePaused)
	s.waitAwake(t, false)
	if got := s.timers.pending(); !slices.Equal(got, []time.Duration{LockStop}) {
		t.Fatalf("the lock pause's timers %v, want [%v]", got, LockStop)
	}
	s.waitTray(t, func(v tray.View) bool { return v.Icon == tray.IconPaused }, "paused after the lock")

	s.clock.Advance(4 * time.Minute)
	s.app.handleSystemEvent(EventUnlock)
	s.waitState(t, share.StateLive)
	s.waitAwake(t, true)
	if got := s.client.sh.pauses(); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("SetPaused calls %v", got)
	}
	if len(s.timers.pending()) != 0 {
		t.Error("the 30 min timer outlived the resume")
	}
	if n := len(s.shell.toastsOf(ToastLockPaused)); n != 0 {
		t.Errorf("%d \"Resume sharing?\" toasts after a short lock", n)
	}
}

// TestLateUnlockAsks: an unlock at 6 min leaves the share paused with "Resume sharing?" (the tray's lock prompt, a
// toast with Resume and Stop and a nonce), keep-awake stays off; the toast's Resume makes it live and takes
// keep-awake again, and the same nonce is refused afterwards.
func TestLateUnlockAsks(t *testing.T) {
	s := newSysEnv(t)
	s.goLive(t)
	s.app.handleSystemEvent(EventLock)
	s.waitState(t, share.StatePaused)
	s.clock.Advance(6 * time.Minute)
	s.app.handleSystemEvent(EventUnlock)

	toasts := s.shell.toastsOf(ToastLockPaused)
	if len(toasts) != 1 {
		t.Fatalf("%d lock-paused toasts", len(toasts))
	}
	n := toasts[0]
	if n.Body != "Sharing is still paused since you locked your PC. Resume sharing?" || len(n.Nonce) != 32 ||
		len(n.Actions) != 2 || n.Actions[0].ID != ToastResume || n.Actions[1].ID != ToastStop ||
		n.Actions[0].Title != "Resume" || n.Actions[1].Title != "Stop" {
		t.Fatalf("toast %+v", n)
	}
	if s.state() != share.StatePaused {
		t.Fatalf("the share is %q after a late unlock", s.state())
	}
	s.waitTray(t, func(v tray.View) bool {
		return v.Icon == tray.IconPaused && strings.Contains(v.Tooltip, "Resume sharing?") &&
			len(v.Menu) > 1 && v.Menu[1].Action == tray.ActionResume && v.Menu[1].Bold
	}, "the lock prompt in the tray")
	if calls := s.os.awakeCalls(); calls[len(calls)-1] {
		t.Fatal("keep-awake on while the lock pause waits for an answer")
	}

	if s.app.ToastAction("0123456789abcdef0123456789abcdef", ToastResume) {
		t.Fatal("a made-up nonce resumed the share")
	}
	if s.app.ToastAction(n.Nonce, ToastKeepOut) {
		t.Fatal("an action the toast doesn't have ran")
	}
	if !s.app.ToastAction(n.Nonce, ToastResume) {
		t.Fatal("the toast's Resume didn't run")
	}
	s.waitState(t, share.StateLive)
	s.waitAwake(t, true)
	if s.app.ToastAction(n.Nonce, ToastResume) || s.app.ToastAction(n.Nonce, ToastStop) {
		t.Fatal("a settled toast's button ran again")
	}
	if len(s.timers.pending()) != 0 {
		t.Error("the 30 min timer outlived the resume")
	}
	if s.state() != share.StateLive {
		t.Error("the settled Stop stopped the share")
	}
}

// TestLockStop: a lock pause stops the share at 30 min with the end reason system and the toast "Sharing stopped
// while your PC was locked", also after an unlock at 6 min with no Resume; an unlock at 31 min finds it stopped.
func TestLockStop(t *testing.T) {
	for _, unlockFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "locked", true: "unlocked at 6 min"}[unlockFirst], func(t *testing.T) {
			s := newSysEnv(t)
			s.goLive(t)
			s.app.handleSystemEvent(EventLock)
			s.waitState(t, share.StatePaused)
			if unlockFirst {
				s.clock.Advance(6 * time.Minute)
				s.app.handleSystemEvent(EventUnlock)
				s.app.handleSystemEvent(EventLock) // locked again: the same lock pause, no new timer
				if got := s.timers.pending(); len(got) != 1 {
					t.Fatalf("timers %v after a second lock", got)
				}
			}
			s.clock.Advance(24 * time.Minute)
			if !s.timers.fire() {
				t.Fatal("no 30 min timer")
			}
			v := s.waitState(t, share.StateEnded)
			if v.EndReason != share.EndSystem {
				t.Fatalf("end reason %q", v.EndReason)
			}
			toasts := s.shell.toastsOf(ToastStoppedLock)
			if len(toasts) != 1 || toasts[0].Body != "Sharing stopped while your PC was locked" ||
				len(toasts[0].Actions) != 1 || toasts[0].Actions[0].ID != ToastOpen {
				t.Fatalf("toasts %+v", toasts)
			}
			s.waitAwake(t, false)
			s.clock.Advance(time.Minute)
			s.app.handleSystemEvent(EventUnlock) // 31 min
			if got := s.client.sh.pauses(); slices.Contains(got[1:], false) {
				t.Errorf("an unlock after the stop resumed: %v", got)
			}
			s.waitAwake(t, false)
		})
	}
}

// TestUserPauseIsNoLockPause: a share the user paused before the lock stays paused and never resumes by itself.
func TestUserPauseIsNoLockPause(t *testing.T) {
	s := newSysEnv(t)
	s.goLive(t)
	if err := s.app.shares.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StatePaused)
	s.app.handleSystemEvent(EventLock)
	s.waitAwake(t, false)
	if len(s.timers.pending()) != 0 {
		t.Fatal("a user's pause became a lock pause")
	}
	s.clock.Advance(time.Minute)
	s.app.handleSystemEvent(EventUnlock)
	s.waitAwake(t, true)
	if s.state() != share.StatePaused || !slices.Equal(s.client.sh.pauses(), []bool{true}) {
		t.Fatalf("state %q, SetPaused %v", s.state(), s.client.sh.pauses())
	}
}

// TestLockWithoutSharePause: a server without share.pause: the lock doesn't pause (the share goes on), keep-awake
// is released while locked and taken again at the unlock.
func TestLockWithoutSharePause(t *testing.T) {
	s := newSysEnv(t, protocol.Feature("share.other"))
	s.goLive(t)
	s.app.handleSystemEvent(EventLock)
	s.waitAwake(t, false)
	if s.state() != share.StateLive || len(s.timers.pending()) != 0 {
		t.Fatalf("state %q, timers %v", s.state(), s.timers.pending())
	}
	s.app.handleSystemEvent(EventUnlock)
	s.waitAwake(t, true)
}

// TestSuspendStops: a suspend stops the share (end reason system) and releases keep-awake; the resume shows "Sharing
// stopped because your PC went to sleep"; a suspend without a share shows nothing.
func TestSuspendStops(t *testing.T) {
	s := newSysEnv(t)
	s.app.handleSystemEvent(EventSuspend)
	s.app.handleSystemEvent(EventResume)
	if n := len(s.shell.toastsOf(ToastStoppedSleep)); n != 0 {
		t.Fatalf("%d sleep toasts without a share", n)
	}
	s.goLive(t)
	s.app.handleSystemEvent(EventSuspend)
	v := s.waitState(t, share.StateEnded)
	if v.EndReason != share.EndSystem {
		t.Fatalf("end reason %q", v.EndReason)
	}
	s.waitAwake(t, false)
	if n := len(s.shell.toastsOf(ToastStoppedSleep)); n != 0 {
		t.Fatal("the sleep toast came before the resume")
	}
	s.app.handleSystemEvent(EventResume)
	toasts := s.shell.toastsOf(ToastStoppedSleep)
	if len(toasts) != 1 || toasts[0].Body != "Sharing stopped because your PC went to sleep" {
		t.Fatalf("toasts %+v", toasts)
	}
	s.app.handleSystemEvent(EventResume)
	if n := len(s.shell.toastsOf(ToastStoppedSleep)); n != 1 {
		t.Fatalf("%d sleep toasts after a second resume", n)
	}
}

// TestEndSession: WM_QUERYENDSESSION stops the share within 2 s and lets the session end, and the app goes on (the
// end may be cancelled); WM_ENDSESSION's confirmed end quits, and the OS hooks are told once the quit ran.
func TestEndSession(t *testing.T) {
	s := newSysEnv(t)
	s.goLive(t)
	s.app.handleSystemEvent(EventEndSession)
	v := s.waitState(t, share.StateEnded)
	if v.EndReason != share.EndSystem {
		t.Fatalf("end reason %q", v.EndReason)
	}
	if n := s.acks(); n != 1 {
		t.Fatalf("%d end-session acks", n)
	}
	time.Sleep(50 * time.Millisecond)
	if s.shell.count("quit") != 0 || s.app.quitting.Load() {
		t.Fatal("the app quit at WM_QUERYENDSESSION: a cancelled end would leave it gone")
	}

	s.app.handleSystemEvent(EventEndSessionConfirmed)
	for deadline := time.Now().Add(3 * time.Second); s.acks() != 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the confirmed end of the session wasn't acknowledged")
		}
	}
	if s.shell.count("quit") != 1 || !s.app.quitting.Load() {
		t.Fatal("the app hadn't quit when it acknowledged the end of the session")
	}
}

// TestSystemEventsLoop: Run reads the OS's events.
func TestSystemEventsLoop(t *testing.T) {
	s := newSysEnv(t)
	s.os.events = make(chan SystemEvent, 1)
	s.goLive(t)
	done := make(chan error, 1)
	go func() { done <- s.app.Run(context.Background()) }()
	s.os.events <- EventLock
	s.waitState(t, share.StatePaused)
	s.app.Quit(true)
	select {
	case <-done:
	case <-time.After(2 * QuitTimeout):
		t.Fatal("Run didn't return")
	}
}

// TestClosePolicy is 03 §11.6's close row.
func TestClosePolicy(t *testing.T) {
	cases := []struct {
		live, closeToTray bool
		want              CloseAction
	}{
		{true, false, CloseHide},
		{true, true, CloseHide},
		{false, false, CloseQuit},
		{false, true, CloseHide},
	}
	for _, c := range cases {
		if got := ClosePolicy(c.live, c.closeToTray); got != c.want {
			t.Errorf("ClosePolicy(live %v, closeToTray %v) = %v, want %v", c.live, c.closeToTray, got, c.want)
		}
	}
}

// TestWindowClosing: closing while live hides the window with the one-time toast (once a run, and never once the
// window notice was shown); closing while idle quits, or hides with closeToTray.
func TestWindowClosing(t *testing.T) {
	s := newSysEnv(t)
	s.goLive(t)
	s.app.WindowClosing()
	if s.shell.WindowVisible() || s.shell.count("quit") != 0 {
		t.Fatal("closing while live didn't hide the window")
	}
	toasts := s.shell.toastsOf(ToastStillSharing)
	if len(toasts) != 1 || toasts[0].Body != "isshoni is still sharing. Stop it from the tray icon (^)." ||
		toasts[0].Nonce != "" || len(toasts[0].Actions) != 0 {
		t.Fatalf("toasts %+v", toasts)
	}
	s.app.TrayClicked()
	if !s.shell.WindowVisible() || s.shell.count("show(true)") < 2 { // the pick's show, then the click's
		t.Fatal("a click on the tray icon didn't open the window")
	}
	s.app.WindowClosing()
	if n := len(s.shell.toastsOf(ToastStillSharing)); n != 1 {
		t.Fatalf("%d close toasts after a second close", n)
	}

	// Idle with closeToTray: hide.
	s.app.TrayAction(tray.ActionStop)
	if v := s.waitState(t, share.StateEnded); v.EndReason != share.EndTray {
		t.Fatalf("the tray's stop: end reason %q", v.EndReason)
	}
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.CloseToTray = true }); err != nil {
		t.Fatal(err)
	}
	s.app.ShowWindow()
	s.app.WindowClosing()
	if s.shell.WindowVisible() || s.shell.count("quit") != 0 {
		t.Fatal("closeToTray didn't keep the app in the tray")
	}

	// Idle: quit.
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.CloseToTray = false }); err != nil {
		t.Fatal(err)
	}
	s.app.WindowClosing()
	if s.shell.count("quit") != 1 {
		t.Fatal("closing while idle didn't quit")
	}
}

// TestWindowClosingAfterNotice: the window notice was shown (tips.trayNoticeShown): no toast.
func TestWindowClosingAfterNotice(t *testing.T) {
	s := newSysEnv(t)
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Tips.TrayNoticeShown = true }); err != nil {
		t.Fatal(err)
	}
	s.goLive(t)
	s.app.WindowClosing()
	if s.shell.WindowVisible() || len(s.shell.toastsOf(ToastStillSharing)) != 0 {
		t.Fatal("hidden without the toast expected")
	}
}

// TestToastOpen: a toast's body (no nonce, or any) opens the window; an unknown action never runs.
func TestToastOpen(t *testing.T) {
	s := newSysEnv(t)
	s.app.HideWindow()
	if !s.app.ToastAction("", "") || !s.shell.WindowVisible() {
		t.Fatal("a toast's body didn't open the window")
	}
	s.app.HideWindow()
	if !s.app.ToastAction("ffffffffffffffffffffffffffffffff", ToastOpen) || !s.shell.WindowVisible() {
		t.Fatal("Open didn't open the window")
	}
	if s.app.ToastAction("", ToastStop) || s.app.ToastAction("", "rm -rf") {
		t.Fatal("an action without a nonce ran")
	}
}

// TestToastBookBound: only the newest maxToasts toasts keep their buttons.
func TestToastBookBound(t *testing.T) {
	s := newSysEnv(t)
	for range maxToasts + 1 {
		s.app.notify(ToastLockPaused, "x", NotificationAction{ID: ToastStop, Title: "Stop"})
	}
	all := s.shell.toastsOf(ToastLockPaused)
	if len(all) != maxToasts+1 {
		t.Fatalf("%d toasts", len(all))
	}
	ids := map[string]bool{}
	for _, n := range all {
		if ids[n.ID] || ids[n.Nonce] {
			t.Fatal("an id or nonce repeats")
		}
		ids[n.ID], ids[n.Nonce] = true, true
	}
	if _, ok := s.app.toasts.settle(all[0].Nonce, ToastStop); ok {
		t.Error("the oldest toast kept its buttons")
	}
	if _, ok := s.app.toasts.settle(all[len(all)-1].Nonce, ToastStop); !ok {
		t.Error("the newest toast lost its buttons")
	}
}

// TestTrayFollowsTheShare (W18's view model in the app): idle at the start, live with the source and the watchers,
// one redraw for a burst, and the tray's Pause and Quit.
func TestTrayFollowsTheShare(t *testing.T) {
	s := newSysEnv(t)
	s.app.flushTray()
	if v, n := s.shell.lastTray(); n != 1 || v.Icon != tray.IconIdle || v.Tooltip != "isshoni" {
		t.Fatalf("first tray %+v (%d)", v, n)
	}
	s.app.flushTray()
	if _, n := s.shell.lastTray(); n != 1 {
		t.Fatal("the same view was drawn twice")
	}
	s.goLive(t)
	s.waitTray(t, func(v tray.View) bool {
		return v.Icon == tray.IconLive && v.Tooltip == "Sharing Fake window · 1 watching"
	}, "live with the fake pick's label")

	s.app.tray.mu.Lock()
	s.app.trayEvery = time.Hour // a burst within the debounce: one redraw at most
	s.app.tray.mu.Unlock()
	_, before := s.shell.lastTray()
	for range 10 {
		s.app.scheduleTray()
	}
	if _, after := s.shell.lastTray(); after != before {
		t.Fatalf("%d redraws within the debounce", after-before)
	}
	s.app.stopTimers()
	s.app.tray.mu.Lock()
	s.app.tray.stopped = false
	s.app.trayEvery = time.Millisecond
	s.app.tray.mu.Unlock()

	s.app.SetAudioAlert(true, 1)
	s.waitTray(t, func(v tray.View) bool {
		return v.Icon == tray.IconWarn && len(v.Menu) > 1 && v.Menu[1].Label == "1 question waiting"
	}, "the audio alert")
	s.app.SetAudioAlert(false, 0)

	s.app.TrayAction(tray.ActionPause)
	s.waitState(t, share.StatePaused)
	s.app.TrayAction(tray.ActionResume)
	s.waitState(t, share.StateLive)

	s.app.TrayAction(tray.ActionStopAndQuit)
	if v := s.waitState(t, share.StateEnded); v.EndReason != share.EndTray {
		t.Fatalf("end reason %q", v.EndReason)
	}
	if s.shell.count("quit") != 1 {
		t.Fatal("Stop sharing and quit didn't quit")
	}
	_, n := s.shell.lastTray()
	s.app.scheduleTray()
	s.app.flushTray()
	if _, m := s.shell.lastTray(); m != n {
		t.Fatal("the tray was drawn after the quit")
	}
}

// TestUpdateReadyOnlyIdle: "Restart and update" shows in Idle only.
func TestUpdateReadyOnlyIdle(t *testing.T) {
	s := newSysEnv(t)
	s.app.SetUpdateReady("0.3.0")
	v := s.waitTray(t, func(v tray.View) bool { return v.Icon == tray.IconIdle }, "idle")
	if len(v.Menu) != 3 || v.Menu[1].Action != tray.ActionRestart {
		t.Fatalf("menu %+v", v.Menu)
	}
	s.goLive(t)
	v = s.waitTray(t, func(v tray.View) bool { return v.Icon == tray.IconLive }, "live")
	for _, m := range v.Menu {
		if m.Action == tray.ActionRestart {
			t.Fatal("Restart and update while live")
		}
	}
}

// TestShutdownHook is W14: a quit Wails began (its OnShutdown) stops the share first and closes the audio service
// without asking the shell to quit again; the shell's quit that Quit asks for may call it (no deadlock).
func TestShutdownHook(t *testing.T) {
	s := newSysEnv(t)
	s.app.d.Audio = appAudio{shell: s.env.shell}
	s.goLive(t)
	s.env.shell.reset()
	s.app.Shutdown()
	if v := s.waitState(t, share.StateEnded); v.EndReason != share.EndUser {
		t.Fatalf("end reason %q", v.EndReason)
	}
	calls, _ := s.env.shell.snapshot()
	if !slices.Equal(calls, []string{"audio.close"}) {
		t.Fatalf("calls %q", calls)
	}
	s.app.Quit(true)
	if s.env.shell.count("quit") != 0 {
		t.Fatal("Quit after Shutdown asked the shell to quit")
	}

	// The real shell's Quit runs Wails' OnShutdown, which calls Shutdown: it returns at once.
	r := newSysEnv(t)
	r.shell.onQuit = r.app.Shutdown
	done := make(chan struct{})
	go func() { r.app.Quit(true); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * QuitTimeout):
		t.Fatal("Quit deadlocked on the shell's shutdown hook")
	}
	if r.env.shell.count("quit") != 1 {
		t.Fatal("the shell didn't quit")
	}
}

// TestCaptureExclusion: Run sets WDA_EXCLUDEFROMCAPTURE on the main window (03 §11.5); with hideFromCapture off it
// resets the affinity instead.
func TestCaptureExclusion(t *testing.T) {
	s := newSysEnv(t)
	done := make(chan error, 1)
	go func() { done <- s.app.Run(context.Background()) }()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		s.os.mu.Lock()
		n := len(s.os.excluded)
		s.os.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run didn't exclude the main window from captures")
		}
	}
	s.app.Quit(false)
	<-done
	s.os.mu.Lock()
	if !slices.Equal(s.os.excluded, []uintptr{0x1234}) {
		t.Errorf("excluded %v", s.os.excluded)
	}
	s.os.mu.Unlock()

	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.HideFromCapture = false }); err != nil {
		t.Fatal(err)
	}
	s.app.ApplyHideFromCapture()
	s.app.WindowCreated()
	s.os.mu.Lock()
	defer s.os.mu.Unlock()
	if len(s.os.excluded) != 1 || !slices.Equal(s.os.included, []uintptr{0x1234, 0x1234}) {
		t.Errorf("excluded %v, included %v", s.os.excluded, s.os.included)
	}
}

// TestWatchdog is W17's fallback: no reload before the page's first beat; a visible window without a beat for
// more than 15 s reloads once per 15 s; a hidden window isn't watched, and one just shown gets a fresh 15 s.
func TestWatchdog(t *testing.T) {
	s := newSysEnv(t)
	visible := true
	s.shell.visible = func() bool { return visible }
	reloads := func() int { return s.env.shell.count("reload") }

	s.clock.Advance(time.Hour)
	s.app.watchdogTick()
	if reloads() != 0 {
		t.Fatal("a page that never beat was reloaded")
	}
	s.app.Heartbeat()
	s.clock.Advance(HeartbeatTimeout)
	s.app.watchdogTick()
	if reloads() != 0 {
		t.Fatal("reloaded at exactly 15 s")
	}
	s.clock.Advance(time.Second)
	s.app.watchdogTick()
	if reloads() != 1 {
		t.Fatalf("%d reloads after 16 s without a beat", reloads())
	}
	s.clock.Advance(5 * time.Second)
	s.app.watchdogTick()
	if reloads() != 1 {
		t.Fatal("reloaded again within 15 s")
	}

	visible = false
	s.clock.Advance(time.Hour)
	s.app.watchdogTick()
	if reloads() != 1 {
		t.Fatal("a hidden window was reloaded")
	}
	visible = true
	s.app.watchdogTick() // just shown: a fresh grace
	s.clock.Advance(10 * time.Second)
	s.app.watchdogTick()
	if reloads() != 1 {
		t.Fatal("a window just shown was reloaded")
	}
	s.app.Heartbeat()
	s.clock.Advance(10 * time.Second)
	s.app.watchdogTick()
	if reloads() != 1 {
		t.Fatal("reloaded with a beat 10 s ago")
	}
}

// homeRecShell is a shell that can load the start page (the app's homeShell), as the Wails shell does.
type homeRecShell struct{ *sysShell }

func (s *homeRecShell) ReturnHome() { s.record("home") }

// TestWatchdogReturnsHome: a reload that brings no beat back is followed by the start page, through a shell that can
// load it (W5's and W17's fallbacks: Wails' Reload runs in the page); a beat starts over with a reload.
func TestWatchdogReturnsHome(t *testing.T) {
	s := newSysEnv(t)
	s.app.d.Shell = &homeRecShell{sysShell: s.shell}
	count := s.env.shell.count
	s.app.watchdogTick() // shown
	s.app.Heartbeat()
	s.clock.Advance(HeartbeatTimeout + time.Second)
	s.app.watchdogTick()
	if count("reload") != 1 || count("home") != 0 {
		t.Fatalf("first miss: %d reloads, %d homes", count("reload"), count("home"))
	}
	s.clock.Advance(HeartbeatTimeout + time.Second)
	s.app.watchdogTick()
	if count("reload") != 1 || count("home") != 1 {
		t.Fatalf("second miss: %d reloads, %d homes", count("reload"), count("home"))
	}
	s.clock.Advance(HeartbeatTimeout + time.Second)
	s.app.watchdogTick()
	if count("home") != 2 {
		t.Fatalf("third miss: %d homes", count("home"))
	}
	s.app.Heartbeat()
	s.clock.Advance(HeartbeatTimeout + time.Second)
	s.app.watchdogTick()
	if count("reload") != 2 || count("home") != 2 {
		t.Fatalf("a miss after a beat: %d reloads, %d homes", count("reload"), count("home"))
	}
}

// TestOpenPaths: OpenLogsFolder and OpenNotices need their path and the OS's opener; failures are ErrIO.
func TestOpenPaths(t *testing.T) {
	s := newSysEnv(t)
	if err := s.app.OpenLogsFolder(); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("OpenLogsFolder without a folder: %v", err)
	}
	o := &openerOS{recOS: s.os}
	s.app.d.OS = o
	s.app.d.LogsDir, s.app.d.NoticesFile = "/logs", "/licenses/THIRD_PARTY_NOTICES.txt"
	if err := s.app.OpenLogsFolder(); err != nil {
		t.Fatal(err)
	}
	if err := s.app.OpenNotices(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.opened, []string{"/logs", "/licenses/THIRD_PARTY_NOTICES.txt"}) {
		t.Errorf("opened %v", o.opened)
	}
	o.fail = true
	if err := s.app.OpenLogsFolder(); !errors.Is(err, ErrIO) {
		t.Errorf("a failing open: %v", err)
	}
}

type openerOS struct {
	*recOS
	opened []string
	fail   bool
}

func (o *openerOS) OpenPath(p string) error {
	if o.fail {
		return context.DeadlineExceeded
	}
	o.opened = append(o.opened, p)
	return nil
}

// lockPrompt reports the app's lock pause and its question.
func (s *sysEnv) lockPrompt() (lockPaused, prompt bool) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	return s.app.sys.lockPaused, s.app.sys.prompt
}

// lockLate goes live, waits past share.StateEvery (so the lock's paused event goes out inside SetPaused, before the
// app records the lock pause), locks, and unlocks later than LockResume: "Resume sharing?" waits. It returns the
// lock-paused toast.
func (s *sysEnv) lockLate(t *testing.T) Notification {
	t.Helper()
	s.goLive(t)
	time.Sleep(3 * share.StateEvery)
	s.app.handleSystemEvent(EventLock)
	s.waitState(t, share.StatePaused)
	s.clock.Advance(6 * time.Minute)
	s.app.handleSystemEvent(EventUnlock)
	toasts := s.shell.toastsOf(ToastLockPaused)
	if len(toasts) != 1 {
		t.Fatalf("%d lock-paused toasts", len(toasts))
	}
	if lp, prompt := s.lockPrompt(); !lp || !prompt {
		t.Fatalf("lockPaused %v, prompt %v after a late unlock", lp, prompt)
	}
	return toasts[0]
}

// waitLockCleared waits until a resume from outside the app ended the lock pause: no question, no 30 min timer,
// keep-awake on and the tray out of its lock prompt.
func (s *sysEnv) waitLockCleared(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if lp, prompt := s.lockPrompt(); !lp && !prompt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resume didn't end the lock pause")
		}
	}
	if got := s.timers.pending(); len(got) != 0 {
		t.Fatalf("timers %v after the resume", got)
	}
	s.waitAwake(t, true)
	s.waitTray(t, func(v tray.View) bool {
		return v.Icon == tray.IconLive && !strings.Contains(v.Tooltip, "Resume sharing?")
	}, "live, without the lock prompt")
}

// TestOutsideResumeEndsLockPause (WA68-R1): a lock pause resumed through the controller from outside the app (the
// panel's Resume through the bridge; WA80's hotkey and pill) ends, also when the lock's own paused event went out
// before the app recorded the lock pause; the 30 min stop then never stops the live share.
func TestOutsideResumeEndsLockPause(t *testing.T) {
	s := newSysEnv(t)
	s.lockLate(t)
	if err := s.app.shares.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StateLive)
	s.waitLockCleared(t)
	if s.timers.fire() {
		t.Fatal("a 30 min timer was still armed")
	}
	if n := len(s.shell.toastsOf(ToastStoppedLock)); n != 0 || s.state() != share.StateLive {
		t.Fatalf("state %q, %d stopped-locked toasts", s.state(), n)
	}
}

// TestLockPromptInShareView (03 §11.6, README WF17): the late unlock's "Resume sharing?" reaches the share's view
// (ShareView.lockPrompt, the panel's bar) with a share.state event of its own, while the share stays paused; a resume
// from anywhere (here the panel's, through the controller) ends it, and the view says live without it. A lock that
// the unlock resumes in time never shows it, nor does a pause the user chose.
func TestLockPromptInShareView(t *testing.T) {
	s := newSysEnv(t)
	var mu sync.Mutex
	var prompts []bool // the state events' LockPrompt, while paused
	unsubscribe := s.app.shares.Subscribe(func(ev share.Event) {
		if ev.Kind == share.EventState && ev.View != nil && ev.View.State == share.StatePaused {
			mu.Lock()
			prompts = append(prompts, ev.View.LockPrompt)
			mu.Unlock()
		}
	})
	t.Cleanup(unsubscribe)
	sawPrompt := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(prompts, true)
	}
	waitView := func(ok func(*share.View) bool, what string) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			if v := s.app.shares.Current(); v != nil && ok(v) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("view %+v: %s", s.app.shares.Current(), what)
			}
		}
	}

	// A short lock: paused without the prompt, then live again.
	s.goLive(t)
	s.app.handleSystemEvent(EventLock)
	s.waitState(t, share.StatePaused)
	s.app.handleSystemEvent(EventUnlock)
	s.waitState(t, share.StateLive)
	if sawPrompt() || s.app.shares.Current().LockPrompt {
		t.Fatal("a short lock showed the lock prompt")
	}

	// A late unlock: the prompt is in the view and in an event of its own, the share still paused.
	time.Sleep(3 * share.StateEvery)
	s.app.handleSystemEvent(EventLock)
	s.waitState(t, share.StatePaused)
	s.clock.Advance(6 * time.Minute)
	s.app.handleSystemEvent(EventUnlock)
	waitView(func(v *share.View) bool { return v.State == share.StatePaused && v.LockPrompt }, "the lock prompt")
	for deadline := time.Now().Add(3 * time.Second); !sawPrompt(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no share.state event with the lock prompt")
		}
	}

	// The panel's Resume: live, no prompt, the lock pause over.
	if err := s.app.shares.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StateLive)
	s.waitLockCleared(t)
	waitView(func(v *share.View) bool { return v.State == share.StateLive && !v.LockPrompt }, "live without it")

	// Paused again by the user: no prompt (the question was answered, and this pause isn't the lock's).
	if err := s.app.shares.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StatePaused)
	time.Sleep(20 * time.Millisecond) // the tray's debounce, which syncs the prompt, is 1 ms here
	if s.app.shares.Current().LockPrompt {
		t.Fatal("a pause the user chose shows the lock prompt")
	}
}

// TestLockStopFindsLiveShare: a 30 min timer that comes due for a share that is live again, before the app has its
// live event, only forgets the lock pause.
func TestLockStopFindsLiveShare(t *testing.T) {
	s := newSysEnv(t)
	s.lockLate(t)
	if err := s.app.shares.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StateLive)
	s.waitLockCleared(t)
	// The live event not handled yet: the lock pause as it was before it.
	s.app.mu.Lock()
	s.app.sys.lockPaused, s.app.sys.prompt = true, true
	gen := s.app.sys.gen
	s.app.mu.Unlock()
	s.app.lockStopReached(gen)
	if lp, prompt := s.lockPrompt(); lp || prompt || s.state() != share.StateLive {
		t.Fatalf("lockPaused %v, prompt %v, state %q", lp, prompt, s.state())
	}
	if n := len(s.shell.toastsOf(ToastStoppedLock)); n != 0 {
		t.Fatalf("%d stopped-locked toasts for a live share", n)
	}
}

// TestStaleLockToast (WA68-R3): a "Resume sharing?" toast answered another way (the tray's Resume, the share's end)
// no longer acts: its Stop doesn't stop the live share, its Resume doesn't resume a pause the user chose.
func TestStaleLockToast(t *testing.T) {
	s := newSysEnv(t)
	n := s.lockLate(t)
	s.app.TrayAction(tray.ActionResume)
	s.waitState(t, share.StateLive)
	if s.app.ToastAction(n.Nonce, ToastStop) {
		t.Fatal("the stale toast's Stop ran")
	}
	if err := s.app.shares.SetPaused(context.Background(), true); err != nil { // the user's own pause
		t.Fatal(err)
	}
	s.waitState(t, share.StatePaused)
	if s.app.ToastAction(n.Nonce, ToastResume) {
		t.Fatal("the stale toast's Resume ran")
	}
	if s.state() != share.StatePaused {
		t.Fatalf("the share is %q", s.state())
	}
	if !s.app.ToastAction(n.Nonce, ToastOpen) {
		t.Fatal("a stale toast's body doesn't open the window")
	}

	// The share's end settles it too.
	s2 := newSysEnv(t)
	n2 := s2.lockLate(t)
	s2.app.TrayAction(tray.ActionStop)
	s2.waitState(t, share.StateEnded)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) { // the ended event
		if lp, _ := s2.lockPrompt(); !lp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the share's end didn't end the lock pause")
		}
	}
	if s2.app.ToastAction(n2.Nonce, ToastResume) || s2.app.ToastAction(n2.Nonce, ToastStop) {
		t.Fatal("a toast of an ended share's lock pause acted")
	}
}

// TestTrayPauseNeedsNewerServer (WA68-R4, 03 §11.1, §11.3): a share on a server without share.pause gets the
// disabled "Pause needs a newer server" line instead of Pause sharing.
func TestTrayPauseNeedsNewerServer(t *testing.T) {
	for _, withPause := range []bool{true, false} {
		var s *sysEnv
		if withPause {
			s = newSysEnv(t)
		} else {
			s = newSysEnv(t, protocol.Feature("share.other"))
		}
		s.goLive(t)
		v := s.waitTray(t, func(v tray.View) bool { return v.Icon == tray.IconLive }, "live")
		var pause *tray.MenuItem
		for i := range v.Menu {
			if v.Menu[i].Action == tray.ActionPause || v.Menu[i].Label == "Pause needs a newer server" {
				pause = &v.Menu[i]
			}
		}
		switch {
		case pause == nil:
			t.Fatalf("share.pause %v: no Pause line in %+v", withPause, v.Menu)
		case withPause && (pause.Action != tray.ActionPause || !pause.Enabled):
			t.Errorf("with share.pause: %+v", *pause)
		case !withPause && (pause.Label != "Pause needs a newer server" || pause.Enabled || pause.Action != ""):
			t.Errorf("without share.pause: %+v", *pause)
		}
	}
}
