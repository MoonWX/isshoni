//go:build windows

package winos

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// errNotImplemented is returned by the OSHooks methods whose slices haven't landed yet.
var errNotImplemented = errors.New("winos: not implemented yet")

// Hooks is app.OSHooks on Windows. The winos thread (thread_windows.go) carries the hotkeys, the session and power
// events, keep-awake and the Sharing pill (pill_windows.go, README WA80); WDA is SetWindowDisplayAffinity; the
// settings pages and OpenPath are ShellExecuteW. Autostart (WA84) answers errNotImplemented until its slice.
type Hooks struct {
	log   *slog.Logger
	t     *thread // nil when the thread didn't start: the methods that need it degrade
	pill  *pill   // on the thread; nil without it
	build int
}

var (
	_ interface{ IncludeInCapture(uintptr) error } = (*Hooks)(nil)
	_ interface{ EndSessionHandled() }             = (*Hooks)(nil)
	_ interface{ OpenPath(string) error }          = (*Hooks)(nil)
)

// New starts the winos thread and returns the Windows hooks. A thread that can't start is logged at WARN: the app
// then runs without hotkeys, system events and keep-awake.
func New() *Hooks {
	h := &Hooks{log: slog.Default().With("component", "winos"), build: int(windows.RtlGetVersion().BuildNumber)}
	t, err := startThread(h.log)
	if err != nil {
		h.log.Warn("winos: the winos thread didn't start", "err", err)
	} else {
		h.t = t
		h.pill = newPill(t, h.log)
	}
	return h
}

// Close ends the winos thread (tests; the app's thread ends with the process).
func (h *Hooks) Close() error {
	if h.t != nil {
		if h.pill != nil {
			_ = h.t.call(h.pill.destroy)
		}
		h.t.close()
	}
	return nil
}

// OSBuild is the Windows build number, such as 26100 (RtlGetVersion, which no compatibility shim changes).
func (h *Hooks) OSBuild() int { return h.build }

// RegisterHotkey registers a global hotkey on the winos thread's window (RegisterHotKey with MOD_NOREPEAT, 03
// §11.2): app.ErrHotkeyTaken when another app has the combination.
func (h *Hooks) RegisterHotkey(id app.HotkeyID, k hotkey.Combo) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("%w: %w", app.ErrInvalidArgument, err)
	}
	if h.t == nil {
		return errThreadGone
	}
	return h.t.registerHotkey(id, k.Modifiers(), k.VirtualKey())
}

// UnregisterHotkey removes a registered hotkey; an unknown id is a no-op.
func (h *Hooks) UnregisterHotkey(id app.HotkeyID) {
	if h.t != nil {
		h.t.unregisterHotkey(id)
	}
}

// Hotkeys delivers each press of a registered hotkey (WM_HOTKEY).
func (h *Hooks) Hotkeys() <-chan app.HotkeyID {
	if h.t == nil {
		return nil
	}
	return h.t.keys
}

// Pill is the Sharing pill (03 §11.4): a window of the winos thread; a no-op when the thread didn't start.
func (h *Hooks) Pill() app.Pill {
	if h.pill == nil {
		return nopPill{}
	}
	return h.pill
}

// ExcludeFromCapture sets WDA_EXCLUDEFROMCAPTURE on a window of this process (03 §11.5).
func (*Hooks) ExcludeFromCapture(hwnd uintptr) error {
	return setDisplayAffinity(hwnd, wdaExcludeFromCapture)
}

// IncludeInCapture resets a window's affinity to WDA_NONE (Settings → App → "Hide isshoni windows from screen
// capture" off; the main window only).
func (*Hooks) IncludeInCapture(hwnd uintptr) error { return setDisplayAffinity(hwnd, wdaNone) }

func (*Hooks) SetAutostart(bool, string) error { return errNotImplemented }
func (*Hooks) Autostart() (bool, error)        { return false, errNotImplemented }

// OpenSettingsPage opens a Windows settings page by the OS build's URI (settingsTarget).
func (h *Hooks) OpenSettingsPage(p app.OSPage) error {
	file, params, err := settingsTarget(p, h.build)
	if err != nil {
		return err
	}
	return shellOpen(file, params)
}

// OpenPath opens a folder in Explorer, or a file with its default app (App.OpenLogsFolder, App.OpenNotices).
func (*Hooks) OpenPath(path string) error { return shellOpen(path, "") }

// shellOpen is ShellExecuteW(nil, "open", file, params, nil, SW_SHOWNORMAL).
func shellOpen(file, params string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	f, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return err
	}
	var p *uint16
	if params != "" {
		if p, err = windows.UTF16PtrFromString(params); err != nil {
			return err
		}
	}
	if err := windows.ShellExecute(0, verb, f, p, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("winos: ShellExecuteW: %w", err)
	}
	return nil
}

// KeepAwake keeps the PC and the display awake while on (SetThreadExecutionState(ES_CONTINUOUS |
// ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED) on the winos thread; off is ES_CONTINUOUS alone). It doesn't wait.
func (h *Hooks) KeepAwake(on bool) {
	if h.t == nil {
		return
	}
	if err := h.t.keepAwake(on); err != nil {
		h.log.Warn("winos: keep-awake not changed", "err", err)
	}
}

// Events delivers the session and power events of 03 §11.6.
func (h *Hooks) Events() <-chan app.SystemEvent {
	if h.t == nil {
		return nil
	}
	return h.t.events
}

// EndSessionHandled lets a waiting WM_QUERYENDSESSION (the app stopped its share) or WM_ENDSESSION (it quit) return.
func (h *Hooks) EndSessionHandled() {
	if h.t == nil {
		return
	}
	select {
	case h.t.endAck <- struct{}{}:
	default:
	}
}
