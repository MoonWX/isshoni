package app

import (
	"errors"
	"maps"

	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// OSHooks is the Windows layer (internal/desktop/winos); darwin and linux get no-op stubs in M2 (03 §6.1). Every
// method is safe for concurrent use. The Windows implementation runs its window work on the winos thread (03 §4.3).
type OSHooks interface {
	// OSBuild is the Windows build number (26100), 0 elsewhere.
	OSBuild() int
	// RegisterHotkey registers a global hotkey (RegisterHotKey with MOD_NOREPEAT); ErrHotkeyTaken when another app
	// has the combination. The app registers them only while a share is live (03 D9, §11.2).
	RegisterHotkey(id HotkeyID, k hotkey.Combo) error
	// UnregisterHotkey removes a registered hotkey; unknown ids are a no-op.
	UnregisterHotkey(id HotkeyID)
	// Hotkeys delivers each press of a registered hotkey (WM_HOTKEY).
	Hotkeys() <-chan HotkeyID
	// Pill is the Sharing pill (03 §11.4).
	Pill() Pill
	// ExcludeFromCapture sets WDA_EXCLUDEFROMCAPTURE on a window (SetWindowDisplayAffinity, 03 §11.5).
	ExcludeFromCapture(hwnd uintptr) error
	// SetAutostart writes or removes HKCU\...\Run\isshoni = "<exe>" --autostart (03 §16.1).
	SetAutostart(on bool, exe string) error
	// Autostart reports whether the Run value is there.
	Autostart() (bool, error)
	// OpenSettingsPage opens a Windows settings page by the OS build's URI (03 §6.2 App.OpenOSSettings).
	OpenSettingsPage(p OSPage) error
	// KeepAwake keeps the PC and the display awake while on (SetThreadExecutionState).
	KeepAwake(on bool)
	// Events delivers the session and power events of 03 §11.6.
	Events() <-chan SystemEvent
}

// ErrHotkeyTaken is RegisterHotkey's error for a combination another app registered
// (ERROR_HOTKEY_ALREADY_REGISTERED): reported as "taken" (03 §11.2).
var ErrHotkeyTaken = errors.New("app: the hotkey is taken by another app")

// ErrNotSupported is returned by the OSHooks stubs of the OSes the app doesn't run on in M2 for what they can't do.
var ErrNotSupported = errors.New("app: not supported on this OS")

// HotkeyID names one of the app's global hotkeys.
type HotkeyID string

const (
	HotkeyPause HotkeyID = "pause" // Pause/Resume toggle; default Ctrl+Alt+Shift+F9
	HotkeyStop  HotkeyID = "stop"  // Stop, no confirmation; default Ctrl+Alt+Shift+F10
)

// OSPage is a Windows settings page App.OpenOSSettings may open (03 §6.2).
type OSPage string

const (
	PageNotifications       OSPage = "notifications"
	PageDND                 OSPage = "dnd" // Do not disturb (Windows 11) or Focus assist (Windows 10), §11.7
	PageGraphicsCapture     OSPage = "graphics-capture"
	PageSound               OSPage = "sound"
	PageSoundCommunications OSPage = "sound-communications" // control.exe mmsys.cpl,,3 (05's ducking tip)
	PageTaskbar             OSPage = "taskbar"
)

// Valid reports whether p is one of the pages.
func (p OSPage) Valid() bool {
	switch p {
	case PageNotifications, PageDND, PageGraphicsCapture, PageSound, PageSoundCommunications, PageTaskbar:
		return true
	}
	return false
}

// SystemEvent is a session or power event (03 §4.3, §11.6).
type SystemEvent string

const (
	EventLock          SystemEvent = "lock"           // WTS_SESSION_LOCK
	EventUnlock        SystemEvent = "unlock"         // WTS_SESSION_UNLOCK
	EventSuspend       SystemEvent = "suspend"        // PBT_APMSUSPEND
	EventResume        SystemEvent = "resume"         // PBT_APMRESUMEAUTOMATIC
	EventEndSession    SystemEvent = "end-session"    // WM_QUERYENDSESSION
	EventDisplayChange SystemEvent = "display-change" // WM_DISPLAYCHANGE
	// EventEndSessionConfirmed: the session ends (WM_ENDSESSION with wParam TRUE); a cancelled end (FALSE) sends
	// nothing.
	EventEndSessionConfirmed SystemEvent = "end-session-confirmed"
)

// Pill is the Sharing pill, a native Win32 window drawn by Go (03 D8, §11.4).
type Pill interface {
	// Set shows, updates or hides the pill; idempotent.
	Set(s PillState)
	// Actions delivers what the user did on it.
	Actions() <-chan PillAction
}

// PillMode is what the pill says.
type PillMode string

const (
	PillLive         PillMode = "live"         // "Sharing · 3 watching"
	PillPaused       PillMode = "paused"       // "Paused"
	PillLockPrompt   PillMode = "lockPrompt"   // "Paused · Resume sharing?"
	PillReconnecting PillMode = "reconnecting" // "Reconnecting…"
)

// PillState is the pill's content (03 §11.4). The fields after Monitor are what the app adds as built (README WA80),
// so the Windows pill needs neither the settings nor the catalog: the saved positions, whether the share's server
// has share.pause, and the texts in the catalog's words.
type PillState struct {
	Visible        bool
	Mode           PillMode
	Watching       int
	VoiceAppsHeard bool   // 05's FriendsHear.alert.voiceAppsHeard: "Friends hear voice apps"
	Questions      int    // 05's FriendsHear.alert.questions: the amber marker
	Monitor        string // the captured monitor's device name; "" = the primary monitor
	// Positions are settings.json's pill.positions: the last position per monitor device name, which the pill clamps
	// to that monitor's work area (03 §11.4); nil when none was saved.
	Positions map[string]PillPoint
	// PauseUnavailable: the server has no share.pause; the Pause button is disabled with Text.PauseUnavailable as its
	// tooltip (03 §11.3).
	PauseUnavailable bool
	Text             PillText
}

// PillPoint is a pill position in screen coordinates (physical pixels): its top left corner.
type PillPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// PillText is the pill's words, from the catalog (desktop.native.pill.*): its line ("Sharing · 3 watching",
// "Paused", "Paused · Resume sharing?", "Reconnecting…", or "Sharing · Friends hear voice apps"), the buttons'
// names, the disabled Pause button's tooltip, the marker's name ("1 question waiting"; "" without a question) and
// the collapsed dot's name.
type PillText struct {
	Line             string
	Pause            string
	Resume           string
	Stop             string
	PauseUnavailable string
	Questions        string
	Collapsed        string
}

// Equal reports whether s and o draw the same pill.
func (s PillState) Equal(o PillState) bool {
	return s.Visible == o.Visible && s.Mode == o.Mode && s.Watching == o.Watching &&
		s.VoiceAppsHeard == o.VoiceAppsHeard && s.Questions == o.Questions && s.Monitor == o.Monitor &&
		s.PauseUnavailable == o.PauseUnavailable && s.Text == o.Text && maps.Equal(s.Positions, o.Positions)
}

// PillActionKind is what the user did on the pill.
type PillActionKind string

const (
	PillPause    PillActionKind = "pause"
	PillResume   PillActionKind = "resume"
	PillStop     PillActionKind = "stop"
	PillCollapse PillActionKind = "collapse"
	PillExpand   PillActionKind = "expand"
	PillMoved    PillActionKind = "moved" // Monitor, X, Y: the new position, kept in settings (pill.positions)
	// PillOpen: a click on the amber marker opens the window on the mic prompt card (03 §11.4; as built, README WA80).
	PillOpen PillActionKind = "open"
)

// PillAction is one action on the pill.
type PillAction struct {
	Kind    PillActionKind
	Monitor string
	X, Y    int
}
