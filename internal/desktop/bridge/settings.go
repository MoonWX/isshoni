package bridge

import (
	"log/slog"
	"maps"

	"github.com/MoonWX/isshoni/internal/desktop/settings"
)

// Settings is the bound Settings service (03 §6.2, §14.2): the user-facing part of settings.json, the hotkeys'
// state and the hotkey field's test. Get works now; Update, Hotkeys and TestHotkey are README WA84's.
type Settings struct {
	store     SettingsSource // nil: not_implemented
	policyOff func() bool    // 06's update.PolicyDisabled; nil: false
	log       *slog.Logger
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
// page.
type SettingsPatch struct {
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

// Update applies a patch and returns the new settings (03 §14.2): invalid_argument with the field. README WA84.
func (s *Settings) Update(patch SettingsPatch) (SettingsView, error) {
	return SettingsView{}, notImplemented("Settings.Update")
}

// Hotkeys returns the state of both hotkeys. README WA84.
func (s *Settings) Hotkeys() ([]HotkeyStatus, error) {
	return nil, notImplemented("Settings.Hotkeys")
}

// TestHotkey registers and at once unregisters combo, and reports its state and typesChar (03 §11.2):
// invalid_argument for a combination that doesn't parse. README WA84.
func (s *Settings) TestHotkey(combo string) (HotkeyStatus, error) {
	return HotkeyStatus{}, notImplemented("Settings.TestHotkey")
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
