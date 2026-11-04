package app

import (
	"errors"
	"fmt"

	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// The main window (03 §4.4, §11.5, §11.6): the close policy, showing and hiding, and WDA_EXCLUDEFROMCAPTURE.

// CloseAction is what closing the window does.
type CloseAction int

// The close actions.
const (
	CloseQuit CloseAction = iota // quit (with the share stopped first)
	CloseHide                    // hide to the tray; the page keeps running
)

// ClosePolicy is 03 §11.6's first row: while a share is live (or paused, reconnecting, unreachable, or still
// starting) closing the window hides it to the tray; otherwise it quits, or hides when Settings → App → "Keep
// isshoni running in the tray when I close the window" (closeToTray, off by default) is on.
func ClosePolicy(live, closeToTray bool) CloseAction {
	if live || closeToTray {
		return CloseHide
	}
	return CloseQuit
}

// visibility is 05's audio.Service.SetVisible (05 §14.2), found on Deps.Audio: the window's visibility, which 05
// only records in M2.
type visibility interface {
	SetVisible(visible bool)
}

// WindowClosing is the shell's close hook (W7): the user closed the window (X, Alt+F4). The shell has cancelled the
// close; the app hides the window or quits by ClosePolicy. The first close while live shows the toast "isshoni is
// still sharing. Stop it from the tray icon (^)." while the window notice of the same line hasn't been shown
// (tips.trayNoticeShown, which README WA80's notice sets), at most once a run. The shell calls it off its UI
// thread: a quit waits for the share's stop.
func (a *App) WindowClosing() {
	_, live := a.Live()
	st := a.d.Settings.Get()
	if ClosePolicy(live, st.CloseToTray) == CloseQuit {
		a.quit(true, share.CauseUser, true)
		return
	}
	a.HideWindow()
	if !live || st.Tips.TrayNoticeShown {
		return
	}
	a.mu.Lock()
	first := !a.sys.closeToastSent
	a.sys.closeToastSent = true
	a.mu.Unlock()
	if first {
		a.notify(ToastStillSharing, a.d.I18n.T("desktop.native.toast.stillSharing"))
	}
}

// ShowWindow shows the main window and gives it the focus (the tray's Open, a toast's Open, a click on the icon).
func (a *App) ShowWindow() {
	a.d.Shell.ShowWindow(true)
	a.windowVisible(true)
}

// HideWindow hides the main window; the page keeps running (03 §4.4).
func (a *App) HideWindow() {
	a.d.Shell.HideWindow()
	a.windowVisible(false)
}

func (a *App) windowVisible(on bool) {
	if v, ok := a.d.Audio.(visibility); ok {
		v.SetVisible(on)
	}
}

// captureIncluder is the OS hooks' way back from WDA_EXCLUDEFROMCAPTURE (SetWindowDisplayAffinity(WDA_NONE)),
// when "Hide isshoni windows from screen capture" is off (winos on Windows).
type captureIncluder interface {
	IncludeInCapture(hwnd uintptr) error
}

// WindowCreated is the shell's report that the main window's native handle exists (W10): the app sets
// WDA_EXCLUDEFROMCAPTURE on it (03 §11.5). Run does the same at start for a window that already exists; calling it
// again is harmless.
func (a *App) WindowCreated() { a.applyCaptureExclusion() }

// ApplyHideFromCapture follows a change of Settings → App → "Hide isshoni windows from screen capture
// (recommended)" (hideFromCapture, on by default) on the main window; the pill keeps its exclusion regardless (03
// §11.5). README WA84's Settings.Update calls it.
func (a *App) ApplyHideFromCapture() { a.applyCaptureExclusion() }

// applyCaptureExclusion sets the main window's display affinity by hideFromCapture.
func (a *App) applyCaptureExclusion() {
	hwnd := a.d.Shell.MainWindowHandle()
	if !a.d.Settings.Get().HideFromCapture {
		if inc, ok := a.d.OS.(captureIncluder); ok && hwnd != 0 {
			if err := inc.IncludeInCapture(hwnd); err != nil {
				a.log.Warn("app: the main window's capture affinity wasn't reset", "err", err)
			}
		}
		return
	}
	if err := a.d.OS.ExcludeFromCapture(hwnd); err != nil {
		if hwnd == 0 {
			a.log.Debug("app: no main window to keep out of captures yet")
			return
		}
		a.log.Warn("app: the main window isn't kept out of captures", "err", err)
	}
}

// pathOpener opens a folder or a file with the OS's shell (winos on Windows: ShellExecuteW "open").
type pathOpener interface {
	OpenPath(path string) error
}

// ErrIO is a local file or folder the app couldn't open (the bridge's io_error).
var ErrIO = errors.New("app: io error")

// OpenLogsFolder opens Explorer at the logs folder (Deps.LogsDir, 03 §6.2, §17).
func (a *App) OpenLogsFolder() error { return a.openPath(a.d.LogsDir) }

// OpenNotices opens <exe dir>\licenses\THIRD_PARTY_NOTICES.txt (Deps.NoticesFile, 03 §6.2, 06 §2.2).
func (a *App) OpenNotices() error { return a.openPath(a.d.NoticesFile) }

func (a *App) openPath(path string) error {
	o, ok := a.d.OS.(pathOpener)
	if path == "" || !ok {
		return ErrNotImplemented
	}
	if err := o.OpenPath(path); err != nil {
		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	return nil
}
