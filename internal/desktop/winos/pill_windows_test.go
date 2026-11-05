//go:build windows

package winos

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// The Sharing pill on Windows (03 §11.4, §22.2; README WA80): created hidden with WDA_EXCLUDEFROMCAPTURE before it
// shows, topmost and never activated, sized by the DPI, placed per monitor inside the work area, its buttons named
// for Narrator and reporting their clicks.

var (
	procGetWindowLongPtrW = modUser32.NewProc("GetWindowLongPtrW")
	procIsWindowVisible   = modUser32.NewProc("IsWindowVisible")
	procIsWindowEnabled   = modUser32.NewProc("IsWindowEnabled")
	procGetWindowTextW    = modUser32.NewProc("GetWindowTextW")
)

const (
	gwlExStyle = ^uintptr(19) // GWL_EXSTYLE, -20
)

var pillText = app.PillText{Line: "Sharing · 3 watching", Pause: "Pause", Resume: "Resume", Stop: "Stop",
	PauseUnavailable: "Pause needs a newer server", Questions: "1 question waiting", Collapsed: "Sharing"}

// pillHooks starts the winos thread with its pill, or skips where no window can be created.
func pillHooks(t *testing.T) *Hooks {
	t.Helper()
	h := New()
	t.Cleanup(func() { _ = h.Close() })
	if h.pill == nil {
		t.Skip("the winos thread didn't start here (no interactive desktop?)")
	}
	return h
}

// onThread runs f on the winos thread.
func onThread(t *testing.T, h *Hooks, f func()) {
	t.Helper()
	if err := h.t.call(f); err != nil {
		t.Fatal(err)
	}
}

// setPill gives the pill a state and waits until the thread applied it.
func setPill(t *testing.T, h *Hooks, s app.PillState) (hwnd, first, stop uintptr) {
	t.Helper()
	h.Pill().Set(s)
	onThread(t, h, func() { hwnd, first, stop = h.pill.hwnd, h.pill.first, h.pill.stop })
	return hwnd, first, stop
}

func windowText(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

func windowRect(t *testing.T, hwnd uintptr) rect {
	t.Helper()
	var wr winRect
	if r, _, err := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr))); r == 0 {
		t.Fatal(err)
	}
	return wr.rect()
}

// click is what a button sends its parent when it is clicked: WM_COMMAND with BN_CLICKED and its id (a
// simulated click that needs no foreground window, which a CI runner may not give).
func click(parent, id, button uintptr) {
	_, _, _ = procSendMessageW.Call(parent, wmCommand, bnClicked<<16|id, button)
}

func visible(hwnd uintptr) bool { r, _, _ := procIsWindowVisible.Call(hwnd); return r != 0 }

func nextAction(t *testing.T, h *Hooks) app.PillAction {
	t.Helper()
	select {
	case a := <-h.Pill().Actions():
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("no pill action")
		return app.PillAction{}
	}
}

// TestPillWindow (03 §22.2): a pill that isn't visible makes no window; shown, it reads WDA_EXCLUDEFROMCAPTURE, its
// extended styles include WS_EX_TOPMOST | WS_EX_NOACTIVATE (and TOOLWINDOW, LAYERED), its size follows the window's
// DPI, the window and buttons carry their names, and Visible false hides it.
func TestPillWindow(t *testing.T) {
	h := pillHooks(t)
	if hwnd, _, _ := setPill(t, h, app.PillState{Mode: app.PillLive, Text: pillText}); hwnd != 0 {
		t.Fatal("a window for a pill that isn't visible")
	}
	hwnd, first, stop := setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Watching: 3, Text: pillText,
		Questions: 1})
	if hwnd == 0 {
		t.Fatal("no pill window")
	}
	if a, err := displayAffinity(hwnd); err != nil || a != wdaExcludeFromCapture {
		t.Fatalf("GetWindowDisplayAffinity = %#x, %v; want WDA_EXCLUDEFROMCAPTURE", a, err)
	}
	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	for name, bit := range map[string]uintptr{"WS_EX_TOPMOST": wsExTopmost, "WS_EX_NOACTIVATE": wsExNoActivate,
		"WS_EX_TOOLWINDOW": wsExToolWindow, "WS_EX_LAYERED": wsExLayered} {
		if ex&bit == 0 {
			t.Errorf("extended styles %#x lack %s", ex, name)
		}
	}
	if !visible(hwnd) {
		t.Fatal("the pill isn't visible")
	}
	if !h.pill.Shown() {
		t.Error("Shown is false with the pill on screen")
	}
	dpi, _, _ := procGetDpiForWindow.Call(hwnd)
	w, h0 := pillSize(int(dpi), false)
	if r := windowRect(t, hwnd); r.width() != w || r.height() != h0 {
		t.Errorf("size %d × %d at %d DPI, want %d × %d", r.width(), r.height(), dpi, w, h0)
	}
	if got := windowText(hwnd); got != "Sharing · 3 watching · 1 question waiting" {
		t.Errorf("the pill's name %q", got)
	}
	if got := windowText(first); got != "Pause" {
		t.Errorf("the first button's name %q", got)
	}
	if got := windowText(stop); got != "Stop" {
		t.Errorf("the second button's name %q", got)
	}

	setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText, PauseUnavailable: true})
	if got := windowText(first); got != "Pause needs a newer server" {
		t.Errorf("the disabled Pause's name %q", got)
	}
	if r, _, _ := procIsWindowEnabled.Call(first); r != 0 {
		t.Error("Pause is enabled on a server without share.pause")
	}

	setPill(t, h, app.PillState{Visible: true, Mode: app.PillPaused, Text: pillText})
	if got := windowText(first); got != "Resume" {
		t.Errorf("paused: the first button's name %q", got)
	}
	setPill(t, h, app.PillState{Mode: app.PillPaused, Text: pillText})
	if visible(hwnd) {
		t.Fatal("Visible false didn't hide the pill")
	}
	if h.pill.Shown() {
		t.Error("Shown is true with the pill hidden")
	}
}

// TestPillButtonClicks (03 §22.2): simulated clicks on the buttons report pause, resume (while paused) and stop; a
// double click on the drag handle collapses and expands it.
func TestPillButtonClicks(t *testing.T) {
	h := pillHooks(t)
	hwnd, first, stop := setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText})
	if hwnd == 0 {
		t.Fatal("no pill window")
	}
	click(hwnd, idFirst, first)
	if a := nextAction(t, h); a.Kind != app.PillPause {
		t.Errorf("first button live: %+v", a)
	}
	setPill(t, h, app.PillState{Visible: true, Mode: app.PillLockPrompt, Text: pillText})
	click(hwnd, idFirst, first)
	if a := nextAction(t, h); a.Kind != app.PillResume {
		t.Errorf("first button paused: %+v", a)
	}
	click(hwnd, idStop, stop)
	if a := nextAction(t, h); a.Kind != app.PillStop {
		t.Errorf("Stop: %+v", a)
	}
	_, _, _ = procSendMessageW.Call(hwnd, wmNCLButtonDblClk, htCaption, 0)
	if a := nextAction(t, h); a.Kind != app.PillCollapse {
		t.Errorf("double click: %+v", a)
	}
	dpi, _, _ := procGetDpiForWindow.Call(hwnd)
	if r := windowRect(t, hwnd); r.width() != scale(pillCollapsed, int(dpi)) || visible(first) {
		t.Errorf("collapsed: %d px wide, buttons visible %v", r.width(), visible(first))
	}
	if got := windowText(hwnd); got != "Sharing" {
		t.Errorf("collapsed: the pill's name %q, want the dot's", got)
	}
	_, _, _ = procSendMessageW.Call(hwnd, wmNCLButtonDblClk, htCaption, 0)
	if a := nextAction(t, h); a.Kind != app.PillExpand {
		t.Errorf("second double click: %+v", a)
	}
	if !visible(first) || !visible(stop) {
		t.Error("the buttons didn't come back")
	}
	if got := windowText(hwnd); got != "Sharing · 3 watching · 1 question waiting" {
		t.Errorf("expanded: the pill's name %q", got)
	}
	if r, _, _ := procSendMessageW.Call(hwnd, wmMouseActivate, hwnd, 0); r != maNoActivate {
		t.Errorf("WM_MOUSEACTIVATE = %d, want MA_NOACTIVATE", r)
	}
}

// TestPillPositions (03 §22.2): on every monitor the pill goes to the top center by default and to a kept position
// otherwise, always inside that monitor's work area, even for a position far outside it; WM_DISPLAYCHANGE keeps it
// inside.
func TestPillPositions(t *testing.T) {
	h := pillHooks(t)
	mons := monitors()
	if len(mons) == 0 {
		t.Skip("no monitor here")
	}
	for _, m := range mons {
		hwnd, _, _ := setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText,
			Monitor: m.device})
		r := windowRect(t, hwnd)
		if !m.wrk.contains(r) {
			t.Errorf("%s: default %+v outside the work area %+v", m.device, r, m.wrk)
		}
		if mid := (r.left + r.right) / 2; abs(mid-(m.bounds.left+m.bounds.right)/2) > 1 &&
			m.wrk.width() == m.bounds.width() {
			t.Errorf("%s: default %+v isn't centered on %+v", m.device, r, m.bounds)
		}
		setPill(t, h, app.PillState{Mode: app.PillLive, Text: pillText, Monitor: m.device}) // hide: placed again
		far := map[string]app.PillPoint{m.device: {X: m.bounds.right + 5000, Y: m.bounds.bottom + 5000}}
		setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText, Monitor: m.device,
			Positions: far})
		r = windowRect(t, hwnd)
		if !m.wrk.contains(r) || r.right != m.wrk.right || r.bottom != m.wrk.bottom {
			t.Errorf("%s: a far position at %+v, want the work area's bottom right corner %+v", m.device, r, m.wrk)
		}
		_, _, _ = procSendMessageW.Call(hwnd, wmDisplayChange, 32, 0)
		if r := windowRect(t, hwnd); !m.wrk.contains(r) {
			t.Errorf("%s: after WM_DISPLAYCHANGE %+v", m.device, r)
		}
		setPill(t, h, app.PillState{Mode: app.PillLive, Text: pillText, Monitor: m.device})
	}
	// A device name no monitor has: the primary monitor.
	hwnd, _, _ := setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText,
		Monitor: `\\.\NOSUCHDISPLAY`})
	primary, ok := targetMonitor("")
	if !ok {
		t.Fatal("no primary monitor")
	}
	if r := windowRect(t, hwnd); !primary.wrk.contains(r) {
		t.Errorf("an unknown monitor: %+v outside the primary's work area %+v", r, primary.wrk)
	}
}

// TestPillMovedReportsItsMonitor: the end of a drag reports the position and the monitor's device name.
func TestPillMovedReportsItsMonitor(t *testing.T) {
	h := pillHooks(t)
	hwnd, _, _ := setPill(t, h, app.PillState{Visible: true, Mode: app.PillLive, Text: pillText})
	_, _, _ = procSendMessageW.Call(hwnd, wmExitSizeMove, 0, 0)
	a := nextAction(t, h)
	r := windowRect(t, hwnd)
	if a.Kind != app.PillMoved || a.Monitor == "" || a.X != r.left || a.Y != r.top {
		t.Fatalf("moved %+v, window %+v", a, r)
	}
}

// TestTypesChar (03 §11.2): the default hotkeys, F-keys with Ctrl, Alt and Shift, type nothing on any layout, and
// neither does Ctrl+Shift+A, whose U+0001 is a control character; an invalid combination is never checked.
func TestTypesChar(t *testing.T) {
	h := pillHooks(t)
	if h.TypesChar(hotkey.DefaultPause) || h.TypesChar(hotkey.DefaultStop) {
		t.Error("the default hotkeys type a character")
	}
	if h.TypesChar(hotkey.Combo{Ctrl: true, Shift: true, Key: "A"}) {
		t.Error("Ctrl+Shift+A counts as a character")
	}
	if h.TypesChar(hotkey.Combo{Key: "A"}) {
		t.Error("an invalid combination was checked")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
