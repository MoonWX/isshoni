package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// WA80: the hotkeys while a share is live, Pause from every control, the pill's state and actions, the pause toast,
// the audio alert outside the window and the close-to-tray notice in the window (03 §11.2–§11.4, §11.6, §15; 05
// §14.7).

// safetyOS is recOS with recorded hotkeys, a recording pill and a keyboard layout check.
type safetyOS struct {
	*recOS
	mu        sync.Mutex
	regs      []string // "+pause Ctrl+Alt+Shift+F9", "-pause"
	taken     map[HotkeyID]bool
	keys      chan HotkeyID
	pill      *recPill
	typesChar bool
}

func (o *safetyOS) RegisterHotkey(id HotkeyID, k hotkey.Combo) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.taken[id] {
		return ErrHotkeyTaken
	}
	o.regs = append(o.regs, "+"+string(id)+" "+k.String())
	return nil
}

func (o *safetyOS) UnregisterHotkey(id HotkeyID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.regs = append(o.regs, "-"+string(id))
}

func (o *safetyOS) Hotkeys() <-chan HotkeyID    { return o.keys }
func (o *safetyOS) Pill() Pill                  { return o.pill }
func (o *safetyOS) TypesChar(hotkey.Combo) bool { return o.typesChar }

func (o *safetyOS) registrations() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.regs)
}

// recPill records every state the app gives the pill.
type recPill struct {
	mu      sync.Mutex
	states  []PillState
	actions chan PillAction
}

func (p *recPill) Set(s PillState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = append(p.states, s)
}

func (p *recPill) Actions() <-chan PillAction { return p.actions }

func (p *recPill) last() (PillState, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.states) == 0 {
		return PillState{}, 0
	}
	return p.states[len(p.states)-1], len(p.states)
}

type safetyEnv struct {
	*sysEnv
	sos *safetyOS
}

// newSafetyEnv is newSysEnv with safetyOS as the OS hooks and its pill as the app's.
func newSafetyEnv(t *testing.T, features ...protocol.Feature) *safetyEnv {
	t.Helper()
	s := newSysEnv(t, features...)
	o := &safetyOS{recOS: s.os, taken: map[HotkeyID]bool{}, keys: make(chan HotkeyID, 4),
		pill: &recPill{actions: make(chan PillAction, 4)}}
	s.app.d.OS = o
	s.app.pill = o.pill
	return &safetyEnv{sysEnv: s, sos: o}
}

func (s *safetyEnv) waitPill(t *testing.T, ok func(PillState) bool, what string) PillState {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		s.app.flushTray()
		p, n := s.sos.pill.last()
		if n > 0 && ok(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("pill %+v: %s", p, what)
		}
	}
}

func (s *safetyEnv) waitRegs(t *testing.T, want ...string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		got := s.sos.registrations()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hotkey registrations %q, want %q", got, want)
		}
	}
}

func statesOf(hs []HotkeyStatus) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(string(h.ID) + "=" + h.Combo + ":" + string(h.State) + " ")
	}
	return strings.TrimSpace(b.String())
}

// TestHotkeysOnlyWhileLive (03 D9, §11.2): nothing is registered before a share is live; at live both defaults are
// registered and the tray's menu names them; the end unregisters them.
func TestHotkeysOnlyWhileLive(t *testing.T) {
	s := newSafetyEnv(t)
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:idle stop=Ctrl+Alt+Shift+F10:idle" {
		t.Fatalf("before a share: %s", got)
	}
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:registered stop=Ctrl+Alt+Shift+F10:registered" {
		t.Fatalf("live: %s", got)
	}
	s.waitTray(t, func(v tray.View) bool {
		return len(v.Menu) > 2 && v.Menu[1].Label == "Pause sharing (Ctrl+Alt+Shift+F9)" &&
			v.Menu[2].Label == "Stop sharing (Ctrl+Alt+Shift+F10)"
	}, "the registered combinations in the menu")

	s.app.TrayAction(tray.ActionStop)
	s.waitState(t, share.StateEnded)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10", "-pause", "-stop")
	if got := statesOf(s.app.Hotkeys()); !strings.HasSuffix(got, ":idle") || strings.Contains(got, "registered") {
		t.Fatalf("after the end: %s", got)
	}
	// A press after the end does nothing (the hotkeys aren't registered).
	s.app.onHotkey(HotkeyStop)
	if s.state() != share.StateEnded {
		t.Fatal("a press after the end acted")
	}
}

// TestHotkeyPresses: the Pause hotkey toggles pause and resume through the controller (01's SetPaused); the Stop
// hotkey stops without asking, with the end reason hotkey; Run reads the presses from the OS hooks.
func TestHotkeyPresses(t *testing.T) {
	s := newSafetyEnv(t)
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")
	done := make(chan error, 1)
	go func() { done <- s.app.Run(context.Background()) }()

	s.sos.keys <- HotkeyPause
	s.waitState(t, share.StatePaused)
	s.sos.keys <- HotkeyPause
	s.waitState(t, share.StateLive)
	if got := s.client.sh.pauses(); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("SetPaused calls %v", got)
	}
	s.sos.keys <- HotkeyStop
	if v := s.waitState(t, share.StateEnded); v.EndReason != share.EndHotkey {
		t.Fatalf("end reason %q", v.EndReason)
	}
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10", "-pause", "-stop")
	s.waitAwake(t, false)
	s.app.Quit(false)
	select {
	case <-done:
	case <-time.After(2 * QuitTimeout):
		t.Fatal("Run didn't return")
	}
}

// TestHotkeyTaken: a combination another app has is reported as taken and isn't in the menu; the other hotkey and
// the tray's Pause still work.
func TestHotkeyTaken(t *testing.T) {
	s := newSafetyEnv(t)
	s.sos.taken[HotkeyPause] = true
	s.sos.typesChar = true // logged at info, by combination only
	s.goLive(t)
	s.waitRegs(t, "+stop Ctrl+Alt+Shift+F10")
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:taken stop=Ctrl+Alt+Shift+F10:registered" {
		t.Fatalf("states %s", got)
	}
	s.waitTray(t, func(v tray.View) bool {
		return len(v.Menu) > 2 && v.Menu[1].Label == "Pause sharing" &&
			v.Menu[2].Label == "Stop sharing (Ctrl+Alt+Shift+F10)"
	}, "only the registered combination in the menu")
	if !strings.Contains(s.logs.String(), "types a character") {
		t.Error("a combination that types a character wasn't logged")
	}
	s.app.onHotkey(HotkeyPause) // not registered: nothing
	if s.state() != share.StateLive {
		t.Fatal("a press of the taken hotkey paused")
	}
	s.app.TrayAction(tray.ActionPause)
	s.waitState(t, share.StatePaused)
}

// TestInvalidHotkeySetting: a combination in settings.json that doesn't parse is invalid and registers nothing.
func TestInvalidHotkeySetting(t *testing.T) {
	s := newSafetyEnv(t)
	// Only a file can hold it (Update refuses it): a newer build's combination, or a hand edit.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"schema":1,"hotkeys":{"pause":"Ctrl+Alt+Shift+F9","stop":"Win+Q"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(settings.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.app.d.Settings = st
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:idle stop=Win+Q:invalid" {
		t.Fatalf("idle: %s", got)
	}
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9")
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:registered stop=Win+Q:invalid" {
		t.Fatalf("live: %s", got)
	}
}

// TestPauseHotkeyNeedsSharePause (03 §11.3): on a server without share.pause the Pause hotkey isn't registered (Stop
// is), and the pill's Pause is disabled with "Pause needs a newer server".
func TestPauseHotkeyNeedsSharePause(t *testing.T) {
	s := newSafetyEnv(t, protocol.Feature("share.other"))
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Pill.Mode = settings.PillAlways }); err != nil {
		t.Fatal(err)
	}
	s.goLive(t)
	s.waitRegs(t, "+stop Ctrl+Alt+Shift+F10")
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Alt+Shift+F9:idle stop=Ctrl+Alt+Shift+F10:registered" {
		t.Fatalf("states %s", got)
	}
	p := s.waitPill(t, func(p PillState) bool { return p.Visible }, "shown")
	if !p.PauseUnavailable || p.Text.PauseUnavailable != "Pause needs a newer server" {
		t.Fatalf("pill %+v", p)
	}
}

// TestPauseToast (03 §11.3, §15): a hotkey pause with the window hidden and no pill shows "Sharing paused.
// Ctrl+Alt+Shift+F9 resumes." once per pause; not with the window in front, a pill on screen, or "Show
// notifications" off.
func TestPauseToast(t *testing.T) {
	s := newSafetyEnv(t)
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")

	s.app.onHotkey(HotkeyPause) // the window is in front
	s.waitState(t, share.StatePaused)
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StateLive)
	if n := len(s.shell.toastsOf(ToastPaused)); n != 0 {
		t.Fatalf("%d pause toasts with the window in front", n)
	}

	s.app.HideWindow()
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StatePaused)
	toasts := s.shell.toastsOf(ToastPaused)
	if len(toasts) != 1 || toasts[0].Body != "Sharing paused. Ctrl+Alt+Shift+F9 resumes." || len(toasts[0].Actions) != 0 {
		t.Fatalf("toasts %+v", toasts)
	}
	s.app.togglePause(true) // a second pause request while paused resumes
	s.waitState(t, share.StateLive)
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StatePaused)
	if n := len(s.shell.toastsOf(ToastPaused)); n != 2 {
		t.Fatalf("%d pause toasts after the second pause, want 2 (one per pause)", n)
	}
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StateLive)

	// A pill on screen: no toast.
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Pill.Mode = settings.PillAlways }); err != nil {
		t.Fatal(err)
	}
	s.waitPill(t, func(p PillState) bool { return p.Visible }, "shown")
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StatePaused)
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StateLive)
	if n := len(s.shell.toastsOf(ToastPaused)); n != 2 {
		t.Fatalf("%d pause toasts with the pill shown", n)
	}

	// "Show notifications" off: no toast.
	if err := s.app.d.Settings.Update(func(st *settings.Settings) {
		st.Pill.Mode, st.Notifications = settings.PillAuto, false
	}); err != nil {
		t.Fatal(err)
	}
	s.waitPill(t, func(p PillState) bool { return !p.Visible }, "hidden")
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StatePaused)
	if n := len(s.shell.toastsOf(ToastPaused)); n != 2 {
		t.Fatalf("%d pause toasts with notifications off", n)
	}
}

// TestPauseToastPillNotShown (03 §15): a pill that was asked to show but isn't on screen (Windows refused its window
// or WDA_EXCLUDEFROMCAPTURE) doesn't stop the pause toast.
func TestPauseToastPillNotShown(t *testing.T) {
	s := newSafetyEnv(t)
	np := &notShownPill{recPill: s.sos.pill}
	s.app.pill = np
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Pill.Mode = settings.PillAlways }); err != nil {
		t.Fatal(err)
	}
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")
	s.waitPill(t, func(p PillState) bool { return p.Visible }, "asked to show")
	s.app.HideWindow()
	s.app.onHotkey(HotkeyPause)
	s.waitState(t, share.StatePaused)
	if toasts := s.shell.toastsOf(ToastPaused); len(toasts) != 1 {
		t.Fatalf("%d pause toasts with no pill on screen, want 1", len(toasts))
	}
	if np.calls.Load() == 0 {
		t.Fatal("Shown wasn't asked")
	}
}

// notShownPill is a recPill that reports no pill on screen.
type notShownPill struct {
	*recPill
	calls atomic.Int32
}

func (p *notShownPill) Shown() bool { p.calls.Add(1); return false }

// TestPillOnTheSharedMonitor (03 §11.4): a monitor share's pill goes on that monitor: PillState.Monitor follows the
// view's (04's MonitorID without "mon:").
func TestPillOnTheSharedMonitor(t *testing.T) {
	s := newSafetyEnv(t)
	eng := fake.NewEngine(fake.EngineConfig{PickResult: &media.Source{Kind: media.SourceScreen, Name: "Display 2",
		Width: 1920, Height: 1080, MonitorID: `mon:\\.\DISPLAY2`, Capture: "dd"}})
	t.Cleanup(func() { _ = eng.Close() })
	a := s.app
	a.d.Engine = eng
	ctl := newShares(a.d, a.clients)
	t.Cleanup(ctl.Close)
	a.shares, a.sharing = ctl, ctl.Live
	t.Cleanup(ctl.Subscribe(a.onShareEvent))
	s.goLive(t)
	if v := a.shares.Current(); v == nil || v.Monitor != `\\.\DISPLAY2` {
		t.Fatalf("view %+v", v)
	}
	p := s.waitPill(t, func(p PillState) bool { return p.Mode == PillLive }, "live")
	if p.Monitor != `\\.\DISPLAY2` {
		t.Fatalf("pill monitor %q", p.Monitor)
	}
}

// TestPillFollowsTheShare (03 §11.4): hidden without a share; with the OS border and the default mode the state is
// kept but not shown; with "Always" it shows "Sharing · 1 watching", "Paused", the alert's text in place of the
// count, the marker, the lock prompt; the end hides it.
func TestPillFollowsTheShare(t *testing.T) {
	s := newSafetyEnv(t)
	s.app.flushTray()
	if p, n := s.sos.pill.last(); n != 1 || p.Visible || p.Mode != "" {
		t.Fatalf("pill before a share %+v (%d)", p, n)
	}
	s.goLive(t)
	// The fake engine's pick has the OS border: the pill isn't due, but its state follows.
	p := s.waitPill(t, func(p PillState) bool { return p.Mode == PillLive }, "live")
	if p.Visible || p.Watching != 1 || p.Text.Line != "Sharing · 1 watching" || p.Monitor != "" {
		t.Fatalf("live with the border %+v", p)
	}
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Pill.Mode = settings.PillAlways }); err != nil {
		t.Fatal(err)
	}
	p = s.waitPill(t, func(p PillState) bool { return p.Visible }, "Always shows it")
	if p.Text.Pause != "Pause" || p.Text.Resume != "Resume" || p.Text.Stop != "Stop" || p.Text.Questions != "" ||
		p.PauseUnavailable {
		t.Fatalf("pill %+v", p)
	}

	s.app.SetAudioAlert(true, 1)
	p = s.waitPill(t, func(p PillState) bool { return p.VoiceAppsHeard }, "the alert")
	if p.Questions != 1 || p.Text.Line != "Sharing · Friends hear voice apps" ||
		p.Text.Questions != "1 question waiting" {
		t.Fatalf("pill with the alert %+v", p)
	}
	s.app.SetAudioAlert(false, 0)

	s.app.TrayAction(tray.ActionPause)
	p = s.waitPill(t, func(p PillState) bool { return p.Mode == PillPaused }, "paused")
	if p.Text.Line != "Paused" {
		t.Fatalf("paused pill %+v", p)
	}
	s.app.mu.Lock()
	s.app.sys.prompt = true
	s.app.mu.Unlock()
	p = s.waitPill(t, func(p PillState) bool { return p.Mode == PillLockPrompt }, "the lock prompt")
	if p.Text.Line != "Paused · Resume sharing?" {
		t.Fatalf("lock prompt pill %+v", p)
	}
	s.app.mu.Lock()
	s.app.sys.prompt = false
	s.app.mu.Unlock()
	s.waitPill(t, func(p PillState) bool { return p.Mode == PillPaused }, "paused again")

	_, before := s.sos.pill.last()
	s.app.flushTray()
	if _, after := s.sos.pill.last(); after != before {
		t.Fatal("the same state was given twice")
	}
	s.app.handleSystemEvent(EventDisplayChange) // given again, to re-clamp
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if _, n := s.sos.pill.last(); n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a display change didn't give the pill its state again")
		}
	}

	s.app.TrayAction(tray.ActionStop)
	s.waitPill(t, func(p PillState) bool { return !p.Visible && p.Mode == "" }, "hidden after the end")
}

// TestPillActions: the pill's Pause, Resume and Stop go through the controller (Stop with the end reason tray), the
// marker opens the window, and a move is kept per monitor in settings and comes back in the state.
func TestPillActions(t *testing.T) {
	s := newSafetyEnv(t)
	s.goLive(t)
	done := make(chan error, 1)
	go func() { done <- s.app.Run(context.Background()) }()
	acts := s.sos.pill.actions

	acts <- PillAction{Kind: PillPause}
	s.waitState(t, share.StatePaused)
	acts <- PillAction{Kind: PillResume}
	s.waitState(t, share.StateLive)

	s.app.HideWindow()
	acts <- PillAction{Kind: PillOpen}
	for deadline := time.Now().Add(3 * time.Second); !s.shell.WindowVisible(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the marker didn't open the window")
		}
	}

	acts <- PillAction{Kind: PillMoved, Monitor: `\\.\DISPLAY2`, X: -1500, Y: 40}
	acts <- PillAction{Kind: PillCollapse}
	p := s.waitPill(t, func(p PillState) bool { return len(p.Positions) == 1 }, "the saved position")
	if got := p.Positions[`\\.\DISPLAY2`]; got != (PillPoint{X: -1500, Y: 40}) {
		t.Fatalf("positions %+v", p.Positions)
	}
	if got := s.app.d.Settings.Get().Pill.Positions[`\\.\DISPLAY2`]; got != (settings.Point{X: -1500, Y: 40}) {
		t.Fatalf("settings' positions %+v", s.app.d.Settings.Get().Pill.Positions)
	}

	acts <- PillAction{Kind: PillStop}
	if v := s.waitState(t, share.StateEnded); v.EndReason != share.EndTray {
		t.Fatalf("the pill's Stop: end reason %q", v.EndReason)
	}
	s.app.Quit(false)
	select {
	case <-done:
	case <-time.After(2 * QuitTimeout):
		t.Fatal("Run didn't return")
	}
}

// TestAudioAlertOutsideWindow (05 §14.7 "Outside the window"): 05's report during a live share gives the tray its
// warning icon, "Friends hear voice apps" first and "1 question waiting", and the pill the alert and the marker;
// 05's audio.alert becomes exactly one toast with Open, whether or not the window is in front; none without a
// live share.
func TestAudioAlertOutsideWindow(t *testing.T) {
	s := newSafetyEnv(t)
	s.app.onAudioEvent(audio.Event{Name: audio.EventAlert, Payload: audio.Banner{Code: "exclusion-unavailable",
		Level: "error"}})
	if n := len(s.shell.toastsOf(ToastVoiceAppsHeard)); n != 0 {
		t.Fatalf("%d alert toasts without a live share", n)
	}
	s.goLive(t)
	s.app.onAudioEvent(audio.Event{Name: audio.EventReport, Payload: audio.FriendsHear{Live: true,
		Alert: audio.Alert{VoiceAppsHeard: true, Questions: 1}}})
	s.app.onAudioEvent(audio.Event{Name: audio.EventAlert, Payload: audio.Banner{Code: "exclusion-unavailable",
		Level: "error"}})
	v := s.waitTray(t, func(v tray.View) bool { return v.Icon == tray.IconWarn }, "the warning icon")
	if len(v.Menu) < 3 || v.Menu[0].Label != "Friends hear voice apps" || v.Menu[0].Enabled ||
		v.Menu[1].Label != "1 question waiting" || v.Menu[1].Action != tray.ActionQuestions ||
		!strings.HasPrefix(v.Tooltip, "Friends hear voice apps · ") {
		t.Fatalf("tray %+v", v)
	}
	p := s.waitPill(t, func(p PillState) bool { return p.VoiceAppsHeard }, "the alert")
	if p.Questions != 1 || p.Text.Questions != "1 question waiting" {
		t.Fatalf("pill %+v", p)
	}
	toasts := s.shell.toastsOf(ToastVoiceAppsHeard)
	if len(toasts) != 1 || toasts[0].Body != "Voice apps can't be kept out right now, so friends hear everything, "+
		"including your calls. Open isshoni to turn sound off." || len(toasts[0].Actions) != 1 ||
		toasts[0].Actions[0].ID != ToastOpen {
		t.Fatalf("toasts %+v", toasts)
	}
	s.app.HideWindow()
	if !s.app.ToastAction(toasts[0].Nonce, ToastOpen) || !s.shell.WindowVisible() {
		t.Fatal("the alert's Open didn't open the window")
	}
	s.app.TrayAction(tray.ActionQuestions)
	if !s.shell.WindowVisible() {
		t.Fatal("the questions line didn't open the window")
	}
}

// TestWindowNotice (03 §11.6): the first close to the tray while live shows the toast and makes the window notice
// due; it is offered only while the window shows, emitted when it opens, and tips.trayNoticeShown is set only once
// the SPA reports it shown; after that neither comes again.
func TestWindowNotice(t *testing.T) {
	s := newSafetyEnv(t)
	if s.app.WindowNotice() != nil || !errors.Is(s.app.WindowNoticeShown(NoticeStillSharing), ErrNoNotice) {
		t.Fatal("a notice before any close")
	}
	s.goLive(t)
	s.app.WindowClosing()
	if n := len(s.shell.toastsOf(ToastStillSharing)); n != 1 {
		t.Fatalf("%d close toasts", n)
	}
	if s.app.WindowNotice() != nil {
		t.Fatal("the notice is offered while the window is hidden")
	}
	if !errors.Is(s.app.WindowNoticeShown(NoticeStillSharing), ErrNoNotice) {
		t.Fatal("the notice was taken as shown while the window is hidden")
	}
	if s.app.d.Settings.Get().Tips.TrayNoticeShown {
		t.Fatal("tips.trayNoticeShown set before the window notice was shown")
	}

	s.app.TrayClicked()
	calls, events := s.shell.snapshot()
	if !slices.Contains(calls, "emit "+EventWindowNotice) {
		t.Fatalf("no window.notice event: %q", calls)
	}
	var emitted WindowNotice
	for _, e := range events {
		if n, ok := e.(WindowNotice); ok {
			emitted = n
		}
	}
	want := WindowNotice{Code: NoticeStillSharing, Text: "isshoni is still sharing. Stop it from the tray icon (^)."}
	if emitted != want {
		t.Fatalf("event %+v", emitted)
	}
	if n := s.app.WindowNotice(); n == nil || *n != want {
		t.Fatalf("WindowNotice %+v", n)
	}
	if s.app.d.Settings.Get().Tips.TrayNoticeShown {
		t.Fatal("tips.trayNoticeShown set before the SPA showed the notice")
	}
	if !errors.Is(s.app.WindowNoticeShown("other"), ErrNoNotice) {
		t.Fatal("an unknown notice was taken")
	}
	if err := s.app.WindowNoticeShown(NoticeStillSharing); err != nil {
		t.Fatal(err)
	}
	if !s.app.d.Settings.Get().Tips.TrayNoticeShown {
		t.Fatal("tips.trayNoticeShown not set once the notice was shown")
	}
	if s.app.WindowNotice() != nil {
		t.Fatal("the notice is offered again")
	}
	s.app.WindowClosing()
	s.app.ShowWindow()
	if n := len(s.shell.toastsOf(ToastStillSharing)); n != 1 {
		t.Fatalf("%d close toasts after the notice", n)
	}
	if s.shell.count("emit "+EventWindowNotice) != 1 {
		t.Fatal("the notice was emitted again")
	}
}

// TestQuitUnregistersAndHides: the app's end unregisters the hotkeys and hides a pill on screen.
func TestQuitUnregistersAndHides(t *testing.T) {
	s := newSafetyEnv(t)
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Pill.Mode = settings.PillAlways }); err != nil {
		t.Fatal(err)
	}
	s.goLive(t)
	s.waitPill(t, func(p PillState) bool { return p.Visible }, "shown")
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")
	s.app.stopSafety()
	if p, _ := s.sos.pill.last(); p.Visible {
		t.Fatal("the pill shows after the app's end")
	}
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10", "-pause", "-stop")
}
