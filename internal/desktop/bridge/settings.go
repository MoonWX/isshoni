package bridge

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Settings is the bound Settings service (03 §6.2, §14.2, §14.4): the user-facing part of settings.json and its
// changes (their side effects through the app, and the settings.changed event), the hotkeys' state and the hotkey
// field's test, and the Cisco OpenH264 opt-in (04 §9.9).
type Settings struct {
	store     SettingsSource // nil: not_implemented
	policyOff func() bool    // 06's update.PolicyDisabled; nil: false
	core      Core           // the app: autostart, the side effects, the hotkeys (each found by a type assertion)
	servers   ServerList     // serverPrefs' origins must be listed servers
	h264      OpenH264Codec  // nil: the opt-in is unavailable
	emit      func(name string, data any)
	log       *slog.Logger

	mu sync.Mutex // one Update at a time: each reads the settings it changes
}

// SettingsView is the user-facing part of settings.json (03 §14.2): Settings.Get() and the settings.changed event.
// pill.mode is auto or always; updates.channel stable or beta (policyOff: 06's policy turned checks off); log.level
// info or debug. The audio section is 05's, read through Audio.
type SettingsView struct {
	Schema          int                       `json:"schema"`
	ServerPrefs     map[string]ServerPrefView `json:"serverPrefs"`
	WizardDone      bool                      `json:"wizardDone"`
	Window          WindowBounds              `json:"window"`
	CloseToTray     bool                      `json:"closeToTray"`
	Autostart       bool                      `json:"autostart"`
	HideFromCapture bool                      `json:"hideFromCapture"`
	Notifications   bool                      `json:"notifications"`
	Hotkeys         HotkeyCombos              `json:"hotkeys"`
	Pill            PillView                  `json:"pill"`
	Updates         UpdatesView               `json:"updates"`
	Tips            TipsView                  `json:"tips"`
	Log             LogView                   `json:"log"`
}

// ServerPrefView is one server's preferences: its default preset (auto, game, movie, text).
type ServerPrefView struct {
	Preset string `json:"preset"`
}

// WindowBounds is the main window's last placement; all zero before the first.
type WindowBounds struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	W         int  `json:"w"`
	H         int  `json:"h"`
	Maximized bool `json:"maximized"`
}

// HotkeyCombos are the Pause and Stop combinations in hotkey's text form ("Ctrl+Alt+Shift+F9").
type HotkeyCombos struct {
	Pause string `json:"pause"`
	Stop  string `json:"stop"`
}

// PillView is the Sharing pill's settings: mode and the last position per monitor.
type PillView struct {
	Mode      string               `json:"mode"`
	Positions map[string]PointView `json:"positions"`
}

// PointView is a screen position.
type PointView struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// UpdatesView is the update settings plus whether 06's policy turned checks off.
type UpdatesView struct {
	Check     bool   `json:"check"`
	Channel   string `json:"channel"`
	PolicyOff bool   `json:"policyOff"`
}

// TipsView records the one-time sheets and notices that were shown.
type TipsView struct {
	FirstShareShown bool `json:"firstShareShown"`
	DNDShown        bool `json:"dndShown"`
	TrayNoticeShown bool `json:"trayNoticeShown"`
}

// LogView is the log settings: level info or debug.
type LogView struct {
	Level string `json:"level"`
}

// SettingsPatch is Settings.Update's argument (03 §14.2): every field optional; only these can be changed from the
// page. WizardDone can only be set (true): the wizard's sound and voice steps were seen once (03 §8.2).
type SettingsPatch struct {
	WizardDone      *bool                           `json:"wizardDone,omitempty"`
	CloseToTray     *bool                           `json:"closeToTray,omitempty"`
	Autostart       *bool                           `json:"autostart,omitempty"`
	HideFromCapture *bool                           `json:"hideFromCapture,omitempty"`
	Notifications   *bool                           `json:"notifications,omitempty"`
	Hotkeys         *HotkeysPatch                   `json:"hotkeys,omitempty"`
	Pill            *PillPatch                      `json:"pill,omitempty"`
	Updates         *UpdatesPatch                   `json:"updates,omitempty"`
	Tips            *TipsPatch                      `json:"tips,omitempty"`
	Log             *LogPatch                       `json:"log,omitempty"`
	ServerPrefs     map[string]ServerPrefsPatchItem `json:"serverPrefs,omitempty"`
}

// HotkeysPatch changes one or both hotkeys.
type HotkeysPatch struct {
	Pause *string `json:"pause,omitempty"`
	Stop  *string `json:"stop,omitempty"`
}

// PillPatch changes the pill's mode.
type PillPatch struct {
	Mode *string `json:"mode,omitempty"`
}

// UpdatesPatch changes the update settings.
type UpdatesPatch struct {
	Check   *bool   `json:"check,omitempty"`
	Channel *string `json:"channel,omitempty"`
}

// TipsPatch marks one-time sheets as shown.
type TipsPatch struct {
	FirstShareShown *bool `json:"firstShareShown,omitempty"`
	DNDShown        *bool `json:"dndShown,omitempty"`
	TrayNoticeShown *bool `json:"trayNoticeShown,omitempty"`
}

// LogPatch changes the log level.
type LogPatch struct {
	Level *string `json:"level,omitempty"`
}

// ServerPrefsPatchItem changes one server's preferences.
type ServerPrefsPatchItem struct {
	Preset *string `json:"preset,omitempty"`
}

// HotkeyStatus is one hotkey's state (03 §6.2): id pause or stop, the combination, state registered, idle, taken or
// invalid, and typesChar when the combination types a character in the current keyboard layout (03 §11.2).
type HotkeyStatus struct {
	ID        string `json:"id"`
	Combo     string `json:"combo"`
	State     string `json:"state"`
	TypesChar bool   `json:"typesChar,omitempty"`
}

// Get returns the user-facing settings.
func (s *Settings) Get() (SettingsView, error) {
	if s.store == nil {
		return SettingsView{}, notImplemented("Settings.Get")
	}
	return settingsView(s.store.Get(), s.policyOff != nil && s.policyOff()), nil
}

// settingsWriter is the store's write (*settings.Store's Update), found on Deps.Settings.
type settingsWriter interface {
	Update(fn func(*settings.Settings)) error
}

var _ settingsWriter = (*settings.Store)(nil)

// What Settings calls of the app (*app.App), each found on Deps.App by a type assertion: the autostart Run value,
// the side effects of a saved change, and the hotkeys' state and test.
type (
	autostarter interface {
		SetAutostart(on bool) error
	}
	settingsApplier interface {
		SettingsChanged(prev settings.Settings)
	}
	hotkeyReader interface {
		Hotkeys() []app.HotkeyStatus
	}
	hotkeyTester interface {
		TestHotkey(combo string) (app.HotkeyStatus, error)
	}
)

var (
	_ autostarter     = (*app.App)(nil)
	_ settingsApplier = (*app.App)(nil)
	_ hotkeyReader    = (*app.App)(nil)
	_ hotkeyTester    = (*app.App)(nil)
)

// Update applies a patch and returns the new settings (03 §14.2); settings.changed carries them to the page too.
// invalid_argument with params.field for a value that isn't valid, and then nothing is saved. wizardDone can only be
// set (false is invalid: the wizard's steps can't be unseen); serverPrefs only for a listed server; Pause and Stop
// must differ. Autostart is written to Windows first (the Run value, 03 §16.1), so a value Windows refused (io_error)
// is never saved; the app then makes the rest take effect (the capture exclusion, a running share's hotkeys, the
// pill, the log level).
func (s *Settings) Update(patch SettingsPatch) (SettingsView, error) {
	w, ok := s.store.(settingsWriter)
	if !ok {
		return SettingsView{}, notImplemented("Settings.Update")
	}
	if err := s.checkPatch(patch); err != nil {
		return SettingsView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.store.Get()
	if err := checkHotkeys(prev.Hotkeys, patch.Hotkeys); err != nil {
		return SettingsView{}, err
	}
	var as autostarter
	autostart := patch.Autostart != nil && *patch.Autostart != prev.Autostart
	if autostart {
		if as, ok = s.core.(autostarter); !ok {
			return SettingsView{}, notImplemented("Settings.Update")
		}
		if err := as.SetAutostart(*patch.Autostart); err != nil {
			return SettingsView{}, mapError(err, s.log)
		}
	}
	if err := w.Update(func(st *settings.Settings) { applyPatch(st, patch) }); err != nil {
		if autostart {
			if rerr := as.SetAutostart(prev.Autostart); rerr != nil {
				s.log.Warn("bridge: autostart not set back after a refused change", "err", rerr)
			}
		}
		return SettingsView{}, mapError(err, s.log)
	}
	if ap, ok := s.core.(settingsApplier); ok {
		ap.SettingsChanged(prev)
	}
	v, err := s.Get()
	if err != nil {
		return SettingsView{}, err
	}
	if s.emit != nil {
		s.emit(EventSettingsChanged, v)
	}
	return v, nil
}

// checkPatch refuses what settings.Store doesn't check itself: wizardDone false, and an origin or a preset of
// serverPrefs that isn't one (an origin must be a listed server's: settings.json drops the others at load).
func (s *Settings) checkPatch(p SettingsPatch) error {
	if p.WizardDone != nil && !*p.WizardDone {
		return invalidArgument("wizardDone")
	}
	if len(p.ServerPrefs) == 0 {
		return nil
	}
	listed := map[string]bool{}
	if s.servers != nil {
		for _, e := range s.servers.List() {
			listed[e.Origin] = true
		}
	}
	for origin, item := range p.ServerPrefs {
		if !listed[origin] {
			return invalidArgument("serverPrefs")
		}
		if item.Preset != nil && !protocol.Preset(*item.Preset).Valid() {
			return invalidArgument("serverPrefs.preset")
		}
	}
	return nil
}

// checkHotkeys refuses a patch after which Pause and Stop would be the same combination (03 §11.2). A combination
// that doesn't parse is settings.Store's to refuse (hotkeys.pause, hotkeys.stop).
func checkHotkeys(cur settings.Hotkeys, p *HotkeysPatch) error {
	if p == nil || (p.Pause == nil && p.Stop == nil) {
		return nil
	}
	next := cur
	if p.Pause != nil {
		next.Pause = *p.Pause
	}
	if p.Stop != nil {
		next.Stop = *p.Stop
	}
	pause, perr := hotkey.Parse(next.Pause)
	stop, serr := hotkey.Parse(next.Stop)
	if perr == nil && serr == nil && pause == stop {
		if p.Stop != nil {
			return invalidArgument("hotkeys.stop")
		}
		return invalidArgument("hotkeys.pause")
	}
	return nil
}

// applyPatch sets the patch's fields on st; settings.Store validates what changed.
func applyPatch(st *settings.Settings, p SettingsPatch) {
	setBool := func(dst, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	setString := func(dst, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	setBool(&st.WizardDone, p.WizardDone)
	setBool(&st.CloseToTray, p.CloseToTray)
	setBool(&st.Autostart, p.Autostart)
	setBool(&st.HideFromCapture, p.HideFromCapture)
	setBool(&st.Notifications, p.Notifications)
	if h := p.Hotkeys; h != nil {
		setString(&st.Hotkeys.Pause, canonicalCombo(h.Pause))
		setString(&st.Hotkeys.Stop, canonicalCombo(h.Stop))
	}
	if p.Pill != nil {
		setString(&st.Pill.Mode, p.Pill.Mode)
	}
	if u := p.Updates; u != nil {
		setBool(&st.Updates.Check, u.Check)
		setString(&st.Updates.Channel, u.Channel)
	}
	if t := p.Tips; t != nil {
		setBool(&st.Tips.FirstShareShown, t.FirstShareShown)
		setBool(&st.Tips.DNDShown, t.DNDShown)
		setBool(&st.Tips.TrayNoticeShown, t.TrayNoticeShown)
	}
	if p.Log != nil {
		setString(&st.Log.Level, p.Log.Level)
	}
	for origin, item := range p.ServerPrefs {
		if item.Preset == nil {
			continue
		}
		if st.ServerPrefs == nil {
			st.ServerPrefs = map[string]settings.ServerPrefs{}
		}
		pref := st.ServerPrefs[origin]
		pref.Preset = protocol.Preset(*item.Preset)
		st.ServerPrefs[origin] = pref
	}
}

// canonicalCombo is a combination in hotkey's text form ("ctrl+alt+shift+f9" is saved as Ctrl+Alt+Shift+F9); one
// that doesn't parse is kept as it came, for settings.Store to refuse.
func canonicalCombo(v *string) *string {
	if v == nil {
		return nil
	}
	c, err := hotkey.Parse(*v)
	if err != nil {
		return v
	}
	text := c.String()
	return &text
}

// Hotkeys returns the state of both hotkeys (03 §6.2): registered while a share has them, taken when another app
// has the combination, idle otherwise, invalid for a saved text this build can't read.
func (s *Settings) Hotkeys() ([]HotkeyStatus, error) {
	r, ok := s.core.(hotkeyReader)
	if !ok {
		return nil, notImplemented("Settings.Hotkeys")
	}
	hs := r.Hotkeys()
	out := make([]HotkeyStatus, 0, len(hs))
	for _, h := range hs {
		out = append(out, HotkeyStatus{ID: string(h.ID), Combo: h.Combo, State: string(h.State)})
	}
	return out, nil
}

// TestHotkey registers and at once unregisters combo, and reports its state (taken, registered when the running
// share has it, else idle) and typesChar (03 §11.2): invalid_argument for a combination that doesn't parse.
func (s *Settings) TestHotkey(combo string) (HotkeyStatus, error) {
	t, ok := s.core.(hotkeyTester)
	if !ok {
		return HotkeyStatus{}, notImplemented("Settings.TestHotkey")
	}
	h, err := t.TestHotkey(combo)
	switch {
	case errors.Is(err, app.ErrInvalidArgument):
		return HotkeyStatus{}, invalidArgument("combo")
	case err != nil:
		return HotkeyStatus{}, mapError(err, s.log)
	}
	return HotkeyStatus{ID: string(h.ID), Combo: h.Combo, State: string(h.State), TypesChar: h.TypesChar}, nil
}

// OpenH264Codec is the Cisco OpenH264 opt-in (03 §14.4, 04 §9.9): main implements it over 04's helpers
// (native.InstallOpenH264, VerifyOpenH264, RemoveOpenH264) and the engine's SetOpenH264Path, in the codecs folder.
type OpenH264Codec interface {
	// Version is the pinned release (native.OpenH264.Version).
	Version() string
	// Installed reports the pinned file in the codecs folder, verified (size and SHA-256).
	Installed() bool
	// Enable downloads and verifies Cisco's binary and hands it to the engine; on any failure nothing is left.
	Enable(ctx context.Context) error
	// Disable turns it off in the engine and deletes the file.
	Disable() error
}

// The opt-in's states (OpenH264Status.State).
const (
	OpenH264Unavailable = "unavailable" // 04 doesn't offer it here: no engine that takes it
	OpenH264Off         = "off"
	OpenH264On          = "on" // installed and verified
)

// openH264Timeout bounds EnableOpenH264: the download's own 60 s (04 §9.9), then the verify and the engine's check.
const openH264Timeout = 90 * time.Second

// OpenH264Status is the opt-in's state (Settings.OpenH264()): state unavailable, off or on; version the pinned
// release ("" when unavailable).
type OpenH264Status struct {
	State   string `json:"state"`
	Version string `json:"version"`
}

// OpenH264 returns the opt-in's state.
func (s *Settings) OpenH264() (OpenH264Status, error) {
	if s.h264 == nil {
		return OpenH264Status{State: OpenH264Unavailable}, nil
	}
	st := OpenH264Status{State: OpenH264Off, Version: s.h264.Version()}
	if s.h264.Installed() {
		st.State = OpenH264On
	}
	return st, nil
}

// EnableOpenH264 downloads Cisco's pinned binary, verifies it and turns it on (04 §9.9): engine.dependency with
// params.reason download (the network; trying again may help) or corrupt (not the pinned file), or the engine's
// missing, version or blocked_by_policy; engine.timeout. A failure leaves nothing behind.
func (s *Settings) EnableOpenH264(ctx context.Context) (OpenH264Status, error) {
	if s.h264 == nil {
		return OpenH264Status{}, notImplemented("Settings.EnableOpenH264")
	}
	ctx, cancel := withTimeout(ctx, openH264Timeout)
	defer cancel()
	if err := s.h264.Enable(ctx); err != nil {
		return OpenH264Status{}, mapError(err, s.log)
	}
	return s.OpenH264()
}

// DisableOpenH264 turns OpenH264 off and deletes the file: busy while a share is live (its encoder may hold the
// DLL, which Windows then can't delete).
func (s *Settings) DisableOpenH264() (OpenH264Status, error) {
	if s.h264 == nil {
		return OpenH264Status{}, notImplemented("Settings.DisableOpenH264")
	}
	if s.core != nil {
		if _, live := s.core.Live(); live {
			return OpenH264Status{}, newError(codes.CodeBusy, nil)
		}
	}
	if err := s.h264.Disable(); err != nil {
		return OpenH264Status{}, mapError(err, s.log)
	}
	return s.OpenH264()
}

// settingsView maps settings.json to the view.
func settingsView(st settings.Settings, policyOff bool) SettingsView {
	v := SettingsView{
		Schema:          st.Schema,
		ServerPrefs:     make(map[string]ServerPrefView, len(st.ServerPrefs)),
		WizardDone:      st.WizardDone,
		CloseToTray:     st.CloseToTray,
		Autostart:       st.Autostart,
		HideFromCapture: st.HideFromCapture,
		Notifications:   st.Notifications,
		Hotkeys:         HotkeyCombos{Pause: st.Hotkeys.Pause, Stop: st.Hotkeys.Stop},
		Pill:            PillView{Mode: st.Pill.Mode, Positions: make(map[string]PointView, len(st.Pill.Positions))},
		Updates:         UpdatesView{Check: st.Updates.Check, Channel: st.Updates.Channel, PolicyOff: policyOff},
		Tips: TipsView{FirstShareShown: st.Tips.FirstShareShown, DNDShown: st.Tips.DNDShown,
			TrayNoticeShown: st.Tips.TrayNoticeShown},
		Log: LogView{Level: st.Log.Level},
	}
	for origin, p := range maps.All(st.ServerPrefs) {
		v.ServerPrefs[origin] = ServerPrefView{Preset: string(p.Preset)}
	}
	for monitor, p := range maps.All(st.Pill.Positions) {
		v.Pill.Positions[monitor] = PointView{X: p.X, Y: p.Y}
	}
	if w := st.Window; w != nil {
		v.Window = WindowBounds{X: w.X, Y: w.Y, W: w.W, H: w.H, Maximized: w.Maximized}
	}
	return v
}
