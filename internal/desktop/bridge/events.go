package bridge

import "github.com/MoonWX/isshoni/internal/desktop/app"

// EventPrefix starts every event name the app sends the page, so none collides with Wails' own (03 §6.3).
const EventPrefix = "isshoni:"

// The events (Go → SPA, 03 §6.3). Every stateful one has a pull method; the SPA subscribes first and then pulls, so
// it misses nothing. The SPA's NativeBridge.on takes the name without the prefix.
const (
	EventShareState      = EventPrefix + "share.state"      // ShareView; coalesced to ≤ 10 per second; Share.Current()
	EventShareStats      = EventPrefix + "share.stats"      // ShareStatsSample every 2 s while live; Share.Stats()
	EventShareNotice     = EventPrefix + "share.notice"     // ShareNotice {code, params}; transient
	EventAudioReport     = EventPrefix + "audio.report"     // 05's FriendsHear, ≤ 4 per second; Audio.FriendsHear()
	EventAudioLevels     = EventPrefix + "audio.levels"     // 05's Levels at 10 Hz while Audio.WatchLevels counts > 0
	EventAudioPrompt     = EventPrefix + "audio.prompt"     // 05's Prompt; also in FriendsHear.prompts
	EventAudioAlert      = EventPrefix + "audio.alert"      // 05's error-level Banner during a live share
	EventAuthState       = EventPrefix + "auth.state"       // AuthState of the active server; Auth.State()
	EventDeepLinkOpen    = app.EventDeepLinkOpen            // app.PendingLink; App.PendingLink()
	EventWindowState     = EventPrefix + "window.state"     // WindowState
	EventSettingsChanged = EventPrefix + "settings.changed" // SettingsView; Settings.Get()
	EventUpdateState     = EventPrefix + "update.state"     // UpdateStatus; Updates.Status()
)

// Events lists every event name, for the shell and the harness.
var Events = []string{
	EventShareState, EventShareStats, EventShareNotice,
	EventAudioReport, EventAudioLevels, EventAudioPrompt, EventAudioAlert,
	EventAuthState, EventDeepLinkOpen, EventWindowState, EventSettingsChanged, EventUpdateState,
}

// WindowState is the window.state event's payload (03 §6.3).
type WindowState struct {
	Visible    bool `json:"visible"`
	Focused    bool `json:"focused"`
	Fullscreen bool `json:"fullscreen"`
}

// ShareNotice is the share.notice event's payload: 01's EventNotice, e.g. encoder_fallback {encoder} (03 §10.5).
type ShareNotice struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}
