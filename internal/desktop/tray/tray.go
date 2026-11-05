// Package tray is the desktop app's tray view model (docs/m2/03-desktop-app.md, "03" below, §11.1): what the tray
// icon shows for the share's state, as a pure function of State. It knows nothing of Wails or Windows:
// internal/desktop/app keeps the State and calls Render, and cmd/isshoni-desktop's shell draws the View with the
// Wails tray (W18), whose menu items report their Action back to the app.
package tray

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/desktop/i18n"
)

// Mode is the share's state as the tray shows it (03 §11.1's rows).
type Mode string

// The modes.
const (
	Idle         Mode = "idle"         // no share (also while one is picked or stopping)
	Live         Mode = "live"         // a share is starting or live
	Paused       Mode = "paused"       // the share is paused
	Reconnecting Mode = "reconnecting" // 01 is reconnecting (also unreachable)
)

// IconID names one of the tray icons of cmd/isshoni-desktop/build/tray (tray-idle.ico, …).
type IconID string

// The icons.
const (
	IconIdle   IconID = "tray-idle"
	IconLive   IconID = "tray-live"   // red
	IconPaused IconID = "tray-paused" // amber
	IconWarn   IconID = "tray-warn"   // reconnecting, or friends hear voice apps
)

// Icons lists every icon, for the shell's embed check.
var Icons = []IconID{IconIdle, IconLive, IconPaused, IconWarn}

// HotkeyID names one of the global hotkeys (the same ids as internal/desktop/app's HotkeyID).
type HotkeyID string

// The hotkeys shown in the menu.
const (
	HotkeyPause HotkeyID = "pause"
	HotkeyStop  HotkeyID = "stop"
)

// Action is what a menu item does when it is chosen; "" for a disabled line and a separator.
type Action string

// The actions (internal/desktop/app's TrayAction carries them out).
const (
	ActionNone        Action = ""
	ActionOpen        Action = "open"          // open and focus the window
	ActionPause       Action = "pause"         // Share.SetPaused(true)
	ActionResume      Action = "resume"        // Share.SetPaused(false); ends a lock pause
	ActionStop        Action = "stop"          // stop the share (end reason tray)
	ActionStopAndQuit Action = "stop-and-quit" // stop the share, then quit (03 §11.6)
	ActionQuit        Action = "quit"          // quit (Idle)
	ActionRestart     Action = "restart"       // Restart and update (Updates.Install; Idle only)
	ActionQuestions   Action = "questions"     // open the window on the mic prompt card (05 §14.7)
)

// State is what the tray shows (03 §11.1). Source, the pick's label, stays on this PC and is never logged.
type State struct {
	Mode        Mode
	Source      string              // the pick's label (local only)
	Watching    int                 // len(ShareSnapshot.Watchers)
	Hotkeys     map[HotkeyID]string // the registered combinations, for the menu ("Ctrl+Alt+Shift+F9")
	UpdateReady string              // a version in update state ready, or ""
	// LockPrompt: a lock pause awaits an answer (an unlock later than 5 min after the lock, 03 §11.6): the Paused
	// tooltip asks "Resume sharing?" and Resume sharing comes first, in bold.
	LockPrompt bool
	// PauseUnavailable: the server has no share.pause (03 §11.3): the Pause item is disabled and says so.
	PauseUnavailable bool
	VoiceAppsHeard   bool // 05's FriendsHear.alert.voiceAppsHeard (live only)
	Questions        int  // 05's FriendsHear.alert.questions: pending mic prompts
}

// MenuItem is one line of the tray menu. A Separator has no label.
type MenuItem struct {
	Label     string `json:"label,omitempty"`
	Action    Action `json:"action,omitempty"`
	Enabled   bool   `json:"enabled"`
	Bold      bool   `json:"bold,omitempty"` // the default item (Windows draws it bold)
	Separator bool   `json:"separator,omitempty"`
}

// View is the tray as the shell draws it.
type View struct {
	Icon    IconID     `json:"icon"`
	Tooltip string     `json:"tooltip"`
	Menu    []MenuItem `json:"menu"`
}

// Equal reports whether v and w draw the same tray.
func (v View) Equal(w View) bool {
	if v.Icon != w.Icon || v.Tooltip != w.Tooltip || len(v.Menu) != len(w.Menu) {
		return false
	}
	for i := range v.Menu {
		if v.Menu[i] != w.Menu[i] {
			return false
		}
	}
	return true
}

// The limits of 03 §11.1.
const (
	// MaxTooltip is the tooltip's length in UTF-16 code units: NOTIFYICONDATAW.szTip holds 128 with its NUL.
	MaxTooltip = 127
	// MaxSource is how many characters of the pick's label the tooltip and the menu show; a longer one is cut to
	// MaxSource-1 and "…".
	MaxSource = 40
)

const ellipsis = "…"

// Render is the tray for s, with t's texts (a nil catalog gives the keys, as i18n does).
func Render(s State, t *i18n.Catalog) View {
	if s.Mode != Live && s.Mode != Paused && s.Mode != Reconnecting {
		return idle(s, t)
	}
	var v View
	var status string // the mode's own text: the tooltip, and the menu's first (disabled) line
	switch s.Mode {
	case Live:
		v.Icon = IconLive
		status = t.T("desktop.native.tray.tooltip.live", "source", Cut(s.Source, MaxSource), "count", s.Watching)
	case Paused:
		v.Icon = IconPaused
		if s.LockPrompt {
			status = t.T("desktop.native.tray.tooltip.lockPrompt")
		} else {
			status = t.T("desktop.native.tray.tooltip.paused", "count", s.Watching)
		}
	default:
		v.Icon = IconWarn
		status = t.T("desktop.native.tray.tooltip.reconnecting")
	}

	// 05 §14.7 "Outside the window": the alert comes before the mode's own text, and the icon warns.
	var lines []MenuItem
	tooltip := status
	if s.VoiceAppsHeard {
		v.Icon = IconWarn
		alert := t.T("desktop.native.audio.voiceAppsHeard")
		tooltip = alert + " · " + status
		lines = append(lines, MenuItem{Label: alert})
	}
	if s.Questions > 0 {
		q := t.T("desktop.native.audio.questionsWaiting", "count", s.Questions)
		if len(lines) == 0 {
			lines = append(lines, MenuItem{Label: status})
			lines = append(lines, MenuItem{Label: q, Action: ActionQuestions, Enabled: true})
		} else {
			lines = append(lines, MenuItem{Label: q, Action: ActionQuestions, Enabled: true},
				MenuItem{Label: status})
		}
	} else {
		lines = append(lines, MenuItem{Label: status})
	}
	v.Tooltip = cutUTF16(tooltip, MaxTooltip)
	v.Menu = append(v.Menu, lines...)

	stop := MenuItem{Label: withHotkey(t, t.T("desktop.native.tray.stop"), s.Hotkeys[HotkeyStop]),
		Action: ActionStop, Enabled: true}
	if s.Mode == Paused {
		v.Menu = append(v.Menu, MenuItem{Label: withHotkey(t, t.T("desktop.native.tray.resume"),
			s.Hotkeys[HotkeyPause]), Action: ActionResume, Enabled: true, Bold: s.LockPrompt}, stop)
	} else {
		pause := MenuItem{Label: withHotkey(t, t.T("desktop.native.tray.pause"), s.Hotkeys[HotkeyPause]),
			Action: ActionPause, Enabled: true}
		if s.PauseUnavailable {
			pause = MenuItem{Label: t.T("desktop.native.tray.pauseNeedsNewerServer")}
		}
		v.Menu = append(v.Menu, pause, stop)
	}
	v.Menu = append(v.Menu,
		MenuItem{Separator: true},
		MenuItem{Label: t.T("desktop.native.tray.open"), Action: ActionOpen, Enabled: true},
		MenuItem{Label: t.T("desktop.native.tray.stopAndQuit"), Action: ActionStopAndQuit, Enabled: true})
	return v
}

// idle is the Idle row: Open isshoni · (Restart and update to 0.3.0) · Quit. Restart and update is never offered
// while a share is live (03 §11.1, §16.2).
func idle(s State, t *i18n.Catalog) View {
	v := View{Icon: IconIdle, Tooltip: cutUTF16(t.T("desktop.native.tray.tooltip.idle"), MaxTooltip)}
	v.Menu = append(v.Menu, MenuItem{Label: t.T("desktop.native.tray.open"), Action: ActionOpen, Enabled: true,
		Bold: true})
	if s.UpdateReady != "" {
		v.Menu = append(v.Menu, MenuItem{Label: t.T("desktop.native.tray.restartAndUpdate", "version",
			s.UpdateReady), Action: ActionRestart, Enabled: true})
	}
	v.Menu = append(v.Menu, MenuItem{Label: t.T("desktop.native.tray.quit"), Action: ActionQuit, Enabled: true})
	return v
}

// withHotkey is "Pause sharing (Ctrl+Alt+Shift+F9)" while the hotkey is registered, else the action alone.
func withHotkey(t *i18n.Catalog, action, combo string) string {
	if combo == "" {
		return action
	}
	return t.T("desktop.native.tray.withHotkey", "action", action, "hotkey", combo)
}

// Cut shortens s to at most n characters (runes): a longer s keeps n-1 of them and ends with "…". Invalid UTF-8 is
// replaced, so the result is always valid.
func Cut(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + ellipsis
}

// cutUTF16 shortens s to at most n UTF-16 code units (szTip's unit): a longer s ends with "…", never in the middle
// of a surrogate pair.
func cutUTF16(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(utf16.Encode([]rune(s))) <= n {
		return s
	}
	units := 0
	var b strings.Builder
	for _, r := range s {
		w := utf16.RuneLen(r)
		if units+w > n-1 { // room for the ellipsis, one unit
			break
		}
		units += w
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}
