//go:build windows

package winos

import (
	"errors"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// The winos thread and WDA (03 §4.3, §11.5, §22.2; README WA68).

var procSendMessageW = modUser32.NewProc("SendMessageW")

// send delivers a message to the thread's hidden window as the system does, and returns its result.
func send(t *testing.T, h *Hooks, message uint32, wParam uintptr) uintptr {
	t.Helper()
	r, _, _ := procSendMessageW.Call(h.t.hwnd, uintptr(message), wParam, 0)
	return r
}

func nextEvent(t *testing.T, h *Hooks) app.SystemEvent {
	t.Helper()
	select {
	case ev := <-h.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no system event")
		return ""
	}
}

// TestThreadEvents: the hidden window turns the session and power messages into the app's events: lock, unlock,
// suspend, resume (PBT_APMRESUMEAUTOMATIC only), the display change; the hidden window is a top-level window, not
// a message-only one, so broadcasts reach it.
func TestThreadEvents(t *testing.T) {
	h := New()
	defer func() { _ = h.Close() }()
	if h.t == nil {
		t.Fatal("the winos thread didn't start")
	}
	parent, _, _ := modUser32.NewProc("GetAncestor").Call(h.t.hwnd, 1) // GA_PARENT
	desktop, _, _ := modUser32.NewProc("GetDesktopWindow").Call()
	if parent != desktop {
		t.Errorf("the hidden window's parent is %#x, not the desktop %#x (HWND_MESSAGE gets no broadcasts)", parent,
			desktop)
	}
	cases := []struct {
		message uint32
		wParam  uintptr
		want    app.SystemEvent
	}{
		{wmWTSSessionChange, wtsSessionLock, app.EventLock},
		{wmWTSSessionChange, wtsSessionUnlock, app.EventUnlock},
		{wmPowerBroadcast, pbtAPMSuspend, app.EventSuspend},
		{wmPowerBroadcast, pbtAPMResumeAuto, app.EventResume},
		{wmDisplayChange, 32, app.EventDisplayChange},
	}
	for _, c := range cases {
		send(t, h, c.message, c.wParam)
		if got := nextEvent(t, h); got != c.want {
			t.Errorf("message %#x/%#x: %q, want %q", c.message, c.wParam, got, c.want)
		}
	}
	// PBT_APMRESUMESUSPEND (7) and other session changes give nothing.
	send(t, h, wmPowerBroadcast, 7)
	send(t, h, wmWTSSessionChange, 1)
	select {
	case ev := <-h.Events():
		t.Errorf("unexpected event %q", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestEndSessionWaits: WM_QUERYENDSESSION delivers end-session and answers TRUE once the app has handled it, or
// after EndSessionWait.
func TestEndSessionWaits(t *testing.T) {
	h := New()
	defer func() { _ = h.Close() }()
	go func() {
		if ev := <-h.Events(); ev == app.EventEndSession {
			time.Sleep(200 * time.Millisecond) // the share's stop
			h.EndSessionHandled()
		}
	}()
	start := time.Now()
	if r := send(t, h, wmQueryEndSession, 0); r != 1 {
		t.Fatalf("WM_QUERYENDSESSION answered %d", r)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > EndSessionWait {
		t.Errorf("WM_QUERYENDSESSION returned after %v", d)
	}

	// Nobody answers: it allows the end after EndSessionWait.
	start = time.Now()
	if r := send(t, h, wmQueryEndSession, 0); r != 1 {
		t.Fatalf("WM_QUERYENDSESSION answered %d", r)
	}
	if d := time.Since(start); d < EndSessionWait-100*time.Millisecond || d > EndSessionWait+time.Second {
		t.Errorf("WM_QUERYENDSESSION without an answer returned after %v", d)
	}
	if ev := nextEvent(t, h); ev != app.EventEndSession {
		t.Errorf("event %q", ev)
	}
}

// TestEndSessionConfirmed: WM_ENDSESSION with wParam TRUE delivers end-session-confirmed and returns once the app
// has quit (EndSessionHandled), or after EndSessionQuitWait; with FALSE (the end was cancelled) it delivers nothing
// and returns at once.
func TestEndSessionConfirmed(t *testing.T) {
	h := New()
	defer func() { _ = h.Close() }()
	go func() {
		if ev := <-h.Events(); ev == app.EventEndSessionConfirmed {
			time.Sleep(200 * time.Millisecond) // the quit
			h.EndSessionHandled()
		}
	}()
	start := time.Now()
	send(t, h, wmEndSession, 1)
	if d := time.Since(start); d < 150*time.Millisecond || d > EndSessionQuitWait {
		t.Errorf("WM_ENDSESSION returned after %v", d)
	}

	start = time.Now()
	send(t, h, wmEndSession, 0)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("a cancelled end's WM_ENDSESSION returned after %v", d)
	}
	select {
	case ev := <-h.Events():
		t.Errorf("a cancelled end delivered %q", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// Nobody answers: it returns after EndSessionQuitWait.
	start = time.Now()
	send(t, h, wmEndSession, 1)
	if d := time.Since(start); d < EndSessionQuitWait-100*time.Millisecond || d > EndSessionQuitWait+time.Second {
		t.Errorf("WM_ENDSESSION without an answer returned after %v", d)
	}
	if ev := nextEvent(t, h); ev != app.EventEndSessionConfirmed {
		t.Errorf("event %q", ev)
	}
}

// TestHotkeyRegistration (03 §22.2): a hotkey registers and unregisters; the same combination registered again by
// another window is ErrHotkeyTaken; a WM_HOTKEY of its id is delivered.
func TestHotkeyRegistration(t *testing.T) {
	a := New()
	defer func() { _ = a.Close() }()
	b := New()
	defer func() { _ = b.Close() }()
	combo := hotkey.MustParse("Ctrl+Alt+Shift+F23") // a key nobody presses on a CI runner
	if err := a.RegisterHotkey(app.HotkeyPause, combo); err != nil {
		t.Fatal(err)
	}
	if err := b.RegisterHotkey(app.HotkeyPause, combo); !errors.Is(err, app.ErrHotkeyTaken) {
		t.Fatalf("a second registration: %v", err)
	}
	send(t, a, wmHotkey, 1)
	select {
	case id := <-a.Hotkeys():
		if id != app.HotkeyPause {
			t.Errorf("hotkey %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WM_HOTKEY wasn't delivered")
	}
	a.UnregisterHotkey(app.HotkeyPause)
	if err := b.RegisterHotkey(app.HotkeyPause, combo); err != nil {
		t.Fatalf("after the unregistration: %v", err)
	}
	b.UnregisterHotkey(app.HotkeyPause)
	b.UnregisterHotkey(app.HotkeyStop) // not registered: a no-op
	if err := a.RegisterHotkey(app.HotkeyStop, hotkey.Combo{Key: "F23"}); !errors.Is(err, app.ErrInvalidArgument) {
		t.Errorf("an F-key without a modifier: %v", err)
	}
}

// TestKeepAwake: the execution state is set on the winos thread without an error, on and off.
func TestKeepAwake(t *testing.T) {
	h := New()
	defer func() { _ = h.Close() }()
	if err := h.t.keepAwake(true); err != nil {
		t.Fatal(err)
	}
	if err := h.t.keepAwake(false); err != nil {
		t.Fatal(err)
	}
	if err := h.t.call(func() {}); err != nil { // both ran
		t.Fatal(err)
	}
	_ = h.Close()
	if err := h.t.keepAwake(true); !errors.Is(err, errThreadGone) {
		t.Errorf("after Close: %v", err)
	}
	h.KeepAwake(true) // logs, never blocks or panics
}

// testWindow creates a visible top-level window on a thread of its own, as Wails' UI thread owns the main window,
// and returns it with a function that destroys it.
func testWindow(t *testing.T) (uintptr, func()) {
	t.Helper()
	type result struct {
		hwnd uintptr
		err  error
	}
	created := make(chan result, 1)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(done)
		cls, _ := windows.UTF16PtrFromString("Static")
		title, _ := windows.UTF16PtrFromString("isshoni test window")
		const wsOverlappedWindow, wsVisible = 0x00CF0000, 0x10000000
		hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
			wsOverlappedWindow|wsVisible, 0, 0, 200, 100, 0, 0, 0, 0)
		if hwnd == 0 {
			created <- result{err: err}
			return
		}
		created <- result{hwnd: hwnd}
		<-quit
		_, _, _ = procDestroyWindow.Call(hwnd)
	}()
	r := <-created
	if r.err != nil {
		t.Skipf("no window could be created here (no interactive desktop?): %v", r.err)
	}
	return r.hwnd, func() { close(quit); <-done }
}

// TestExcludeFromCapture (03 §11.5, §22.2): the main window, owned by another thread, reads WDA_EXCLUDEFROMCAPTURE
// after ExcludeFromCapture, and WDA_NONE after IncludeInCapture; no window is an error.
func TestExcludeFromCapture(t *testing.T) {
	h := New()
	defer func() { _ = h.Close() }()
	hwnd, destroy := testWindow(t)
	defer destroy()
	if err := h.ExcludeFromCapture(hwnd); err != nil {
		t.Fatal(err)
	}
	if a, err := displayAffinity(hwnd); err != nil || a != wdaExcludeFromCapture {
		t.Fatalf("GetWindowDisplayAffinity = %#x, %v; want WDA_EXCLUDEFROMCAPTURE", a, err)
	}
	if err := h.IncludeInCapture(hwnd); err != nil {
		t.Fatal(err)
	}
	if a, err := displayAffinity(hwnd); err != nil || a != wdaNone {
		t.Fatalf("GetWindowDisplayAffinity = %#x, %v; want WDA_NONE", a, err)
	}
	if err := h.ExcludeFromCapture(0); err == nil {
		t.Error("no window succeeded")
	}
}

// TestCloseEndsTheThread: after Close the window is gone and calls fail fast.
func TestCloseEndsTheThread(t *testing.T) {
	h := New()
	hwnd := h.t.hwnd
	_ = h.Close()
	if _, ok := threads.Load(hwnd); ok {
		t.Error("the window is still registered")
	}
	if err := h.RegisterHotkey(app.HotkeyPause, hotkey.DefaultPause); !errors.Is(err, errThreadGone) {
		t.Errorf("RegisterHotkey after Close: %v", err)
	}
}
