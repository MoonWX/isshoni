package app

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// Share safety (03 §11.2–§11.4, §11.6, §15; 05 §14.7 "Outside the window"; README WA80): the global hotkeys,
// registered only while a share is live (03 D9), Pause from every control (the hotkey, the tray, the pill, a toast;
// the panel's button goes through the bound Share.SetPaused), the Sharing pill's state on every change, the pause
// toast, the audio alert outside the window (the tray's warning, the pill's text and marker, one toast) and the
// close-to-tray notice in the window that sets tips.trayNoticeShown once it was shown.

// HotkeyState is a hotkey's state (03 §6.2 Settings.Hotkeys).
type HotkeyState string

// The hotkey states.
const (
	HotkeyRegistered HotkeyState = "registered" // registered for the running share
	HotkeyIdle       HotkeyState = "idle"       // valid, not registered now (no share, or Pause on a server without it)
	HotkeyTaken      HotkeyState = "taken"      // another app has the combination (ERROR_HOTKEY_ALREADY_REGISTERED)
	HotkeyInvalid    HotkeyState = "invalid"    // settings.json's text doesn't parse
)

// HotkeyStatus is one hotkey's state: its id, the combination in hotkey's text form and its state. The bound
// Settings.Hotkeys (README WA84) answers with these.
type HotkeyStatus struct {
	ID    HotkeyID
	Combo string
	State HotkeyState
}

// charTyper is the OS hooks' check of a combination against the current keyboard layout (winos: VkKeyScanExW and
// ToUnicodeEx): true when typing it produces a character (03 §11.2). Found by a type assertion; without it nothing
// is checked.
type charTyper interface {
	TypesChar(k hotkey.Combo) bool
}

// The toasts this file adds (Notification.Kind, 03 §15).
const (
	ToastVoiceAppsHeard = "voice-apps-heard" // 05's audio.alert during a live share: Open
	ToastPaused         = "paused"           // a hotkey pause with no pill and the window not in front
)

// Window notices (03 §11.6): a line at the top of the window, shown the next time it opens.
const (
	// NoticeStillSharing is the close-to-tray notice: "isshoni is still sharing. Stop it from the tray icon (^)."
	NoticeStillSharing = "still-sharing"
	// EventWindowNotice is emitted (with a WindowNotice) when the window opens with a notice due; App.WindowNotice
	// is its pull.
	EventWindowNotice = "isshoni:window.notice"
)

// WindowNotice is the window.notice event's payload: the notice's code and its text from the catalog.
type WindowNotice struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// ErrNoNotice is WindowNoticeShown's error for a code that isn't due, or a window that isn't showing.
var ErrNoNotice = errors.New("app: no such window notice is showing")

// safetyActionTimeout bounds what a hotkey or the pill asks of the share (a pause, a resume or a stop).
const safetyActionTimeout = 10 * time.Second

// safety is this file's state. mu orders the hotkeys' registrations and guards the hotkey fields; pillMu guards
// the pill's. Neither is held while App.mu or tray.mu is taken.
type safety struct {
	mu         sync.Mutex
	registered bool                      // the hotkeys are registered for the running share
	status     map[HotkeyID]HotkeyStatus // the last registration's result, by id
	combos     map[HotkeyID]string       // the registered combinations
	pauseToast bool                      // this pause's toast went out (once per pause)

	pillMu   sync.Mutex
	lastPill PillState // what the pill was last given
	pillSet  bool      // lastPill was given
}

// hotkeyOrder is the order of the hotkeys everywhere (the menu, the status).
var hotkeyOrder = []HotkeyID{HotkeyPause, HotkeyStop}

// hotkeysWanted reports a share whose hotkeys are registered: from live to its end (03 D9; the same span as
// keep-awake).
func hotkeysWanted(v *share.View) bool { return awakeWanted(v) }

// syncHotkeys registers the hotkeys when a share has gone live and unregisters them when it ends (03 §11.2): the
// combinations from settings.json, the Pause hotkey only when the share's server has share.pause (03 §11.3). A
// combination another app has is reported as taken; the buttons, the tray and the pill still work.
func (a *App) syncHotkeys(v *share.View) {
	want := hotkeysWanted(v)
	a.safe.mu.Lock()
	if want == a.safe.registered {
		a.safe.mu.Unlock()
		return
	}
	if want {
		a.registerHotkeysLocked()
	} else {
		for _, id := range hotkeyOrder {
			if _, ok := a.safe.combos[id]; ok {
				a.d.OS.UnregisterHotkey(id)
			}
		}
		a.safe.combos, a.safe.status = nil, nil
		a.safe.registered = false
	}
	combos := maps.Clone(a.safe.combos)
	a.safe.mu.Unlock()
	a.SetTrayHotkeys(combos)
}

// registerHotkeysLocked registers both hotkeys; a.safe.mu is held.
func (a *App) registerHotkeysLocked() {
	st := a.d.Settings.Get().Hotkeys
	noPause := a.clients.pauseUnavailable()
	a.safe.registered = true
	a.safe.combos = map[HotkeyID]string{}
	a.safe.status = map[HotkeyID]HotkeyStatus{}
	for _, id := range hotkeyOrder {
		text := st.Pause
		if id == HotkeyStop {
			text = st.Stop
		}
		c, err := hotkey.Parse(text)
		if err != nil {
			a.log.Info("app: a hotkey in the settings isn't valid; it isn't registered", "hotkey", string(id))
			a.safe.status[id] = HotkeyStatus{ID: id, Combo: text, State: HotkeyInvalid}
			continue
		}
		combo := c.String()
		if id == HotkeyPause && noPause {
			// A server without share.pause: the Pause hotkey isn't registered, Stop still is (03 §11.3).
			a.safe.status[id] = HotkeyStatus{ID: id, Combo: combo, State: HotkeyIdle}
			continue
		}
		if ct, ok := a.d.OS.(charTyper); ok && ct.TypesChar(c) {
			// The combination, never the character (03 §11.2).
			a.log.Info("app: a hotkey types a character on this keyboard layout", "hotkey", string(id),
				"combo", combo)
		}
		switch err := a.d.OS.RegisterHotkey(id, c); {
		case err == nil:
			a.safe.combos[id] = combo
			a.safe.status[id] = HotkeyStatus{ID: id, Combo: combo, State: HotkeyRegistered}
		case errors.Is(err, ErrHotkeyTaken):
			a.log.Info("app: another app has a hotkey's combination", "hotkey", string(id), "combo", combo)
			a.safe.status[id] = HotkeyStatus{ID: id, Combo: combo, State: HotkeyTaken}
		default:
			a.log.Warn("app: a hotkey wasn't registered", "hotkey", string(id), "err", err)
			a.safe.status[id] = HotkeyStatus{ID: id, Combo: combo, State: HotkeyIdle}
		}
	}
}

// Hotkeys returns the state of both hotkeys (03 §6.2 Settings.Hotkeys; the bound method is README WA84's): while a
// share is live, what its registration found; otherwise idle for a combination that parses and invalid for one that
// doesn't.
func (a *App) Hotkeys() []HotkeyStatus {
	a.safe.mu.Lock()
	if a.safe.registered {
		out := make([]HotkeyStatus, 0, len(hotkeyOrder))
		for _, id := range hotkeyOrder {
			out = append(out, a.safe.status[id])
		}
		a.safe.mu.Unlock()
		return out
	}
	a.safe.mu.Unlock()
	st := a.d.Settings.Get().Hotkeys
	out := make([]HotkeyStatus, 0, len(hotkeyOrder))
	for _, id := range hotkeyOrder {
		text := st.Pause
		if id == HotkeyStop {
			text = st.Stop
		}
		if c, err := hotkey.Parse(text); err == nil {
			out = append(out, HotkeyStatus{ID: id, Combo: c.String(), State: HotkeyIdle})
		} else {
			out = append(out, HotkeyStatus{ID: id, Combo: text, State: HotkeyInvalid})
		}
	}
	return out
}

// registeredCombo is the registered combination of id, "" when it isn't registered.
func (a *App) registeredCombo(id HotkeyID) string {
	a.safe.mu.Lock()
	defer a.safe.mu.Unlock()
	return a.safe.combos[id]
}

// hotkeyLoop handles the presses of the registered hotkeys until the app ends.
func (a *App) hotkeyLoop(ch <-chan HotkeyID) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			a.onHotkey(id)
		}
	}
}

// onHotkey is one press: Pause toggles pause and resume (a resume ends a lock pause), Stop stops the share without
// asking, with the end reason hotkey (03 §11.2). A press of a hotkey that isn't registered does nothing.
func (a *App) onHotkey(id HotkeyID) {
	if a.registeredCombo(id) == "" {
		a.log.Debug("app: a press of a hotkey that isn't registered", "hotkey", string(id))
		return
	}
	switch id {
	case HotkeyPause:
		a.togglePause(true)
	case HotkeyStop:
		ctx, cancel := context.WithTimeout(a.ctx, safetyActionTimeout)
		defer cancel()
		a.shares.Stop(ctx, share.CauseHotkey)
	}
}

// togglePause resumes a paused share and pauses a running one. fromHotkey: the pause shows the pause toast where
// the user can't see the window or a pill (03 §11.3, §15).
func (a *App) togglePause(fromHotkey bool) {
	v := a.shares.Current()
	if v == nil {
		return
	}
	if v.State == share.StatePaused {
		a.resume()
		return
	}
	if !pausable(v) {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, safetyActionTimeout)
	err := a.shares.SetPaused(ctx, true)
	cancel()
	if err != nil {
		a.log.Info("app: the pause didn't pause", errAttr(err))
		return
	}
	if fromHotkey {
		a.pauseToast()
	}
}

// pauseToast is "Sharing paused. Ctrl+Alt+Shift+F9 resumes." with the registered combination, once per pause, when
// no pill is shown and the window isn't in the foreground; Settings → App → "Show notifications" turns it off (03
// §15).
func (a *App) pauseToast() {
	if !a.d.Settings.Get().Notifications || a.windowInFront() || a.pillShown() {
		return
	}
	combo := a.registeredCombo(HotkeyPause)
	a.safe.mu.Lock()
	sent := a.safe.pauseToast
	a.safe.pauseToast = true
	a.safe.mu.Unlock()
	if sent || combo == "" {
		return
	}
	a.notify(ToastPaused, a.d.I18n.T("desktop.native.toast.paused", "hotkey", combo))
}

// followPauseToast forgets the pause toast once the share isn't paused: the next pause may show one again.
func (a *App) followPauseToast(v *share.View) {
	if v != nil && v.State == share.StatePaused {
		return
	}
	a.safe.mu.Lock()
	a.safe.pauseToast = false
	a.safe.mu.Unlock()
}

// pillShown reports a pill on screen: the pill's own answer when it has one (PillShowing), else the Visible it was
// last given.
func (a *App) pillShown() bool {
	if ps, ok := a.pill.(PillShowing); ok {
		return ps.Shown()
	}
	a.safe.pillMu.Lock()
	defer a.safe.pillMu.Unlock()
	return a.safe.pillSet && a.safe.lastPill.Visible
}

// pillState is the pill's state now (03 §11.4): the share's view, the lock prompt, 05's alert, the settings' mode
// and positions, and the texts in the catalog's words. No share, or one that is starting or ending: the zero state
// (hidden).
func (a *App) pillState() PillState {
	v := a.shares.Current()
	if v == nil {
		return PillState{}
	}
	var mode PillMode
	switch v.State {
	case share.StateLive:
		mode = PillLive
	case share.StatePaused:
		mode = PillPaused
	case share.StateReconnecting, share.StateUnreachable:
		mode = PillReconnecting
	default:
		return PillState{}
	}
	st := a.d.Settings.Get()
	noPause := a.clients.pauseUnavailable()
	a.mu.Lock()
	prompt := a.sys.prompt
	a.mu.Unlock()
	if mode == PillPaused && prompt {
		mode = PillLockPrompt
	}
	a.tray.mu.Lock()
	voice, questions := a.tray.voiceHeard, a.tray.questions
	a.tray.mu.Unlock()
	s := PillState{
		Visible:          v.Pill || st.Pill.Mode == settings.PillAlways,
		Mode:             mode,
		Watching:         v.Watching,
		Monitor:          v.Monitor,
		VoiceAppsHeard:   voice,
		Questions:        questions,
		PauseUnavailable: noPause,
	}
	if len(st.Pill.Positions) > 0 {
		s.Positions = make(map[string]PillPoint, len(st.Pill.Positions))
		for name, p := range st.Pill.Positions {
			s.Positions[name] = PillPoint(p)
		}
	}
	s.Text = pillText(s, a)
	return s
}

// pillText is the pill's words for s: its line, the buttons' names (which Narrator reads), the Pause button's
// tooltip on a server without share.pause, the marker's name and the collapsed dot's name.
func pillText(s PillState, a *App) PillText {
	t := a.d.I18n
	var line string
	switch s.Mode {
	case PillLive:
		if s.VoiceAppsHeard {
			// "Friends hear voice apps" replaces the watcher count (03 §11.4).
			line = t.T("desktop.native.pill.liveAlert", "alert", t.T("desktop.native.audio.voiceAppsHeard"))
		} else {
			line = t.T("desktop.native.pill.live", "count", s.Watching)
		}
	case PillPaused:
		line = t.T("desktop.native.pill.paused")
	case PillLockPrompt:
		line = t.T("desktop.native.pill.lockPrompt")
	case PillReconnecting:
		line = t.T("desktop.native.pill.reconnecting")
	}
	out := PillText{
		Line:      line,
		Pause:     t.T("desktop.native.pill.pause"),
		Resume:    t.T("desktop.native.pill.resume"),
		Stop:      t.T("desktop.native.pill.stop"),
		Collapsed: t.T("desktop.native.pill.collapsed"),
	}
	if s.PauseUnavailable {
		out.PauseUnavailable = t.T("desktop.native.tray.pauseNeedsNewerServer")
	}
	if s.Questions > 0 {
		out.Questions = t.T("desktop.native.audio.questionsWaiting", "count", s.Questions)
	}
	return out
}

// flushPill gives the pill its state when it changed (Pill.Set is idempotent; this only saves the winos thread a
// call). It runs with the tray's redraw, so a burst of changes reaches it once.
func (a *App) flushPill() {
	p := a.pill
	if p == nil {
		return
	}
	s := a.pillState()
	a.safe.pillMu.Lock()
	if a.safe.pillSet && a.safe.lastPill.Equal(s) {
		a.safe.pillMu.Unlock()
		return
	}
	a.safe.lastPill, a.safe.pillSet = s, true
	a.safe.pillMu.Unlock()
	p.Set(s)
}

// pillLoop carries out what the user does on the pill until the app ends.
func (a *App) pillLoop(ch <-chan PillAction) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case act, ok := <-ch:
			if !ok {
				return
			}
			a.onPillAction(act)
		}
	}
}

// onPillAction is one action on the pill (03 §11.4): Pause and Resume as every control (a resume ends a lock pause),
// Stop with the end reason tray (the pill's Stop, 03 §10.1), the marker's click opens the window on the mic prompt
// card, and a move keeps the position for its monitor in settings (pill.positions).
func (a *App) onPillAction(act PillAction) {
	switch act.Kind {
	case PillPause:
		a.togglePause(false)
	case PillResume:
		a.resume()
	case PillStop:
		ctx, cancel := context.WithTimeout(a.ctx, safetyActionTimeout)
		defer cancel()
		a.shares.Stop(ctx, share.CauseTray)
	case PillOpen:
		a.ShowWindow()
	case PillMoved:
		if act.Monitor == "" {
			return
		}
		err := a.d.Settings.Update(func(s *settings.Settings) {
			if s.Pill.Positions == nil {
				s.Pill.Positions = map[string]settings.Point{}
			}
			s.Pill.Positions[act.Monitor] = settings.Point{X: act.X, Y: act.Y}
		})
		if err != nil {
			a.log.Warn("app: the pill's position wasn't saved", "err", err)
		}
		a.scheduleTray()
	case PillCollapse, PillExpand:
		// The pill draws it; nothing else changes.
	default:
		a.log.Debug("app: unknown pill action", "action", string(act.Kind))
	}
}

// onAudioAlert is 05's audio.alert (05 §14.4, §14.7): during a live share, the toast "Voice apps can't be kept out
// right now…" with Open, whether or not the window is in front (a game's Do-not-disturb may swallow it; the tray and
// the pill say it too). 05 sends it at most once per banner code per share, and again only after 60 s.
func (a *App) onAudioAlert(b audio.Banner) {
	if _, live := a.Live(); !live {
		return
	}
	a.log.Info("app: friends hear voice apps", "banner", b.Code)
	t := a.d.I18n
	a.notify(ToastVoiceAppsHeard, t.T("desktop.native.toast.voiceAppsHeard"),
		NotificationAction{ID: ToastOpen, Title: t.T("desktop.native.toast.open")})
}

// WindowNotice returns the notice due at the top of the window, while the window shows (03 §11.6); nil when none is.
// The SPA pulls it after subscribing to EventWindowNotice and calls WindowNoticeShown once it showed it.
func (a *App) WindowNotice() *WindowNotice {
	if !a.d.Shell.WindowVisible() {
		return nil
	}
	a.mu.Lock()
	due := a.sys.noticeDue
	a.mu.Unlock()
	if !due || a.d.Settings.Get().Tips.TrayNoticeShown {
		return nil
	}
	return &WindowNotice{Code: NoticeStillSharing, Text: a.d.I18n.T("desktop.native.toast.stillSharing")}
}

// WindowNoticeShown records that the SPA showed the notice code in the visible window: for the close-to-tray
// notice, settings.json's tips.trayNoticeShown, so neither the toast nor the notice comes again (03 §11.6).
// ErrNoNotice when that notice isn't due or the window isn't showing.
func (a *App) WindowNoticeShown(code string) error {
	if code != NoticeStillSharing || a.WindowNotice() == nil {
		return ErrNoNotice
	}
	if err := a.d.Settings.Update(func(s *settings.Settings) { s.Tips.TrayNoticeShown = true }); err != nil {
		return err
	}
	a.mu.Lock()
	a.sys.noticeDue = false
	a.mu.Unlock()
	return nil
}

// emitWindowNotice tells the SPA of a due notice when the window has just been shown.
func (a *App) emitWindowNotice() {
	if n := a.WindowNotice(); n != nil {
		a.d.Shell.Emit(EventWindowNotice, *n)
	}
}

// startSafety starts the loops of the hotkeys and the pill's actions. Run calls it.
func (a *App) startSafety() {
	if ch := a.d.OS.Hotkeys(); ch != nil {
		a.wg.Go(func() { a.hotkeyLoop(ch) })
	}
	if a.pill != nil {
		if ch := a.pill.Actions(); ch != nil {
			a.wg.Go(func() { a.pillLoop(ch) })
		}
	}
}

// stopSafety unregisters the hotkeys and hides the pill: the app quits.
func (a *App) stopSafety() {
	a.syncHotkeys(nil)
	if a.pill != nil {
		a.safe.pillMu.Lock()
		shown := a.safe.pillSet && a.safe.lastPill.Visible
		a.safe.lastPill, a.safe.pillSet = PillState{}, true
		a.safe.pillMu.Unlock()
		if shown {
			a.pill.Set(PillState{})
		}
	}
}
