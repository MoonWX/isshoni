//go:build windows

package winos

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// The winos thread (03 §4.3): one goroutine locked to its OS thread runs a GetMessageW loop for a hidden top-level
// window (WS_POPUP, never shown; not HWND_MESSAGE, because message-only windows get no broadcasts). It receives
// WM_HOTKEY, WM_WTSSESSION_CHANGE (WTSRegisterSessionNotification), WM_POWERBROADCAST
// (RegisterSuspendResumeNotification), WM_QUERYENDSESSION/WM_ENDSESSION, WM_DISPLAYCHANGE and WM_SETTINGCHANGE.
// Requests reach it by PostMessageW(WM_APP+1); results come back on Go channels. Nothing on it blocks on the network
// or the engine; WM_QUERYENDSESSION alone waits, at most EndSessionWait, for the app to stop its share, and
// WM_ENDSESSION, at most EndSessionQuitWait, for it to quit.

// EndSessionWait bounds WM_QUERYENDSESSION's wait for the app's EndSessionHandled (03 §11.6: stop within 2 s, then
// allow).
const EndSessionWait = 2 * time.Second

// EndSessionQuitWait bounds WM_ENDSESSION's wait for the app's quit (03 §11.6's "close sockets with 1000"): Windows
// may end the process as soon as WM_ENDSESSION returns.
const EndSessionQuitWait = 3 * time.Second

// Window messages and their parameters (WinUser.h, WtsApi32.h).
const (
	wmDestroy           = 0x0002
	wmClose             = 0x0010
	wmQueryEndSession   = 0x0011
	wmEndSession        = 0x0016
	wmSettingChange     = 0x001A
	wmDisplayChange     = 0x007E
	wmPowerBroadcast    = 0x0218
	wmWTSSessionChange  = 0x02B1
	wmHotkey            = 0x0312
	wmApp               = 0x8000
	wmAppCall           = wmApp + 1 // run the queued calls
	wtsSessionLock      = 0x7
	wtsSessionUnlock    = 0x8
	pbtAPMSuspend       = 0x4
	pbtAPMResumeAuto    = 0x12
	notifyForThisSess   = 0 // NOTIFY_FOR_THIS_SESSION
	deviceNotifyHWND    = 0 // DEVICE_NOTIFY_WINDOW_HANDLE
	wsPopup             = 0x80000000
	wsExToolWindow      = 0x00000080
	modNoRepeat         = 0x4000
	errHotkeyRegistered = windows.Errno(1409) // ERROR_HOTKEY_ALREADY_REGISTERED
	// SetThreadExecutionState's flags.
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

var (
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	modWtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	procRegisterClassExW                    = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW                     = modUser32.NewProc("CreateWindowExW")
	procDestroyWindow                       = modUser32.NewProc("DestroyWindow")
	procDefWindowProcW                      = modUser32.NewProc("DefWindowProcW")
	procGetMessageW                         = modUser32.NewProc("GetMessageW")
	procTranslateMessage                    = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW                    = modUser32.NewProc("DispatchMessageW")
	procPostMessageW                        = modUser32.NewProc("PostMessageW")
	procPostQuitMessage                     = modUser32.NewProc("PostQuitMessage")
	procRegisterHotKey                      = modUser32.NewProc("RegisterHotKey")
	procUnregisterHotKey                    = modUser32.NewProc("UnregisterHotKey")
	procRegisterSuspendResumeNotification   = modUser32.NewProc("RegisterSuspendResumeNotification")
	procUnregisterSuspendResumeNotification = modUser32.NewProc("UnregisterSuspendResumeNotification")
	procSetWindowDisplayAffinity            = modUser32.NewProc("SetWindowDisplayAffinity")
	procGetWindowDisplayAffinity            = modUser32.NewProc("GetWindowDisplayAffinity")
	procGetModuleHandleW                    = modKernel32.NewProc("GetModuleHandleW")
	procSetThreadExecutionState             = modKernel32.NewProc("SetThreadExecutionState")
	procWTSRegisterSessionNotification      = modWtsapi32.NewProc("WTSRegisterSessionNotification")
	procWTSUnRegisterSessionNotification    = modWtsapi32.NewProc("WTSUnRegisterSessionNotification")
)

// errThreadGone is a request after the winos thread ended (or that never started).
var errThreadGone = errors.New("winos: the winos thread isn't running")

// wndClassEx is WNDCLASSEXW.
type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

// msg is MSG.
type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// className is the hidden window's class, registered once per process.
const className = "isshoni.winos"

var (
	classOnce sync.Once
	classErr  error
	threads   sync.Map // hwnd → *thread: wndProc's way to the thread of a window
)

// registerClass registers the hidden window's class with the process's one window procedure.
func registerClass() error {
	classOnce.Do(func() {
		name, err := windows.UTF16PtrFromString(className)
		if err != nil {
			classErr = err
			return
		}
		inst, _, _ := procGetModuleHandleW.Call(0)
		wc := wndClassEx{wndProc: windows.NewCallback(wndProc), instance: windows.Handle(inst), className: name}
		wc.size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = fmt.Errorf("winos: RegisterClassExW: %w", err)
		}
	})
	return classErr
}

// thread is one winos thread with its hidden window.
type thread struct {
	log    *slog.Logger
	hwnd   uintptr
	events chan app.SystemEvent
	keys   chan app.HotkeyID
	endAck chan struct{} // EndSessionHandled
	done   chan struct{} // closed when the loop has ended

	mu     sync.Mutex
	queue  []func()
	closed bool

	// Only the thread itself touches these.
	hotkeys   map[app.HotkeyID]uintptr // registered: id → RegisterHotKey's id
	byNumber  map[uintptr]app.HotkeyID
	nextKey   uintptr
	powerNote uintptr // RegisterSuspendResumeNotification's handle
}

// startThread starts the winos thread and waits until its window exists.
func startThread(log *slog.Logger) (*thread, error) {
	if err := registerClass(); err != nil {
		return nil, err
	}
	t := &thread{
		log:      log,
		events:   make(chan app.SystemEvent, 16),
		keys:     make(chan app.HotkeyID, 16),
		endAck:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		hotkeys:  map[app.HotkeyID]uintptr{},
		byNumber: map[uintptr]app.HotkeyID{},
		nextKey:  3,
	}
	ready := make(chan error, 1)
	go t.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return t, nil
}

// run is the thread: the window, its registrations, and the message loop until WM_QUIT.
func (t *thread) run(ready chan<- error) {
	runtime.LockOSThread() // never unlocked: the thread ends with the goroutine
	defer close(t.done)
	name, _ := windows.UTF16PtrFromString(className)
	inst, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, err := procCreateWindowExW.Call(wsExToolWindow, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(name)), wsPopup, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("winos: CreateWindowExW: %w", err)
		return
	}
	t.hwnd = hwnd
	threads.Store(hwnd, t)
	defer threads.Delete(hwnd)
	if r, _, err := procWTSRegisterSessionNotification.Call(hwnd, notifyForThisSess); r == 0 {
		t.log.Warn("winos: no lock and unlock events", "err", err)
	}
	if procRegisterSuspendResumeNotification.Find() == nil {
		t.powerNote, _, _ = procRegisterSuspendResumeNotification.Call(hwnd, deviceNotifyHWND)
	}
	ready <- nil

	var m msg
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) == -1 { //nolint:gosec // G115: GetMessageW's BOOL is -1, 0 or positive
			t.log.Error("winos: GetMessageW failed", "err", err)
			break
		}
		if r == 0 { // WM_QUIT
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	t.mu.Lock()
	t.closed = true
	queue := t.queue
	t.queue = nil
	t.mu.Unlock()
	for _, f := range queue { // run what was asked before the end, so no caller waits for good
		f()
	}
}

// wndProc is the hidden windows' procedure.
func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if v, ok := threads.Load(hwnd); ok {
		if r, handled := v.(*thread).handle(uint32(message), wParam, lParam); handled { //nolint:gosec // G115: a message id is 32 bits
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// handle is the thread's part of wndProc: true when it handled the message.
func (t *thread) handle(message uint32, wParam, _ uintptr) (uintptr, bool) {
	switch message {
	case wmAppCall:
		t.mu.Lock()
		queue := t.queue
		t.queue = nil
		t.mu.Unlock()
		for _, f := range queue {
			f()
		}
		return 0, true
	case wmWTSSessionChange:
		switch wParam {
		case wtsSessionLock:
			t.emit(app.EventLock)
		case wtsSessionUnlock:
			t.emit(app.EventUnlock)
		}
		return 0, true
	case wmPowerBroadcast:
		switch wParam {
		case pbtAPMSuspend:
			t.emit(app.EventSuspend)
		case pbtAPMResumeAuto: // also sent before PBT_APMRESUMESUSPEND; one resume event
			t.emit(app.EventResume)
		}
		return 1, true // TRUE
	case wmQueryEndSession:
		t.emitAndWait(app.EventEndSession, EndSessionWait, "the share wasn't stopped before the end of the session")
		return 1, true // TRUE: the session may end
	case wmEndSession:
		if wParam != 0 { // the session ends; FALSE: the end was cancelled and the app goes on
			t.emitAndWait(app.EventEndSessionConfirmed, EndSessionQuitWait,
				"the app didn't quit before the end of the session")
		}
		return 0, true
	case wmDisplayChange:
		t.emit(app.EventDisplayChange)
		return 0, false
	case wmHotkey:
		if id, ok := t.byNumber[wParam]; ok {
			select {
			case t.keys <- id:
			default:
				t.log.Warn("winos: a hotkey press was dropped")
			}
		}
		return 0, true
	case wmClose:
		_, _, _ = procDestroyWindow.Call(t.hwnd)
		return 0, true
	case wmDestroy:
		t.unregisterAll()
		_, _, _ = procPostQuitMessage.Call(0)
		return 0, true
	case wmSettingChange:
		return 0, false
	}
	return 0, false
}

// emitAndWait delivers ev and waits at most d for the app's EndSessionHandled; what is logged when it doesn't come.
func (t *thread) emitAndWait(ev app.SystemEvent, d time.Duration, what string) {
	select { // an acknowledgement of an earlier message whose wait was over
	case <-t.endAck:
	default:
	}
	t.emit(ev)
	select {
	case <-t.endAck:
	case <-time.After(d):
		t.log.Warn("winos: " + what)
	}
}

// emit delivers a system event; a full channel drops it with a WARN (nobody is reading).
func (t *thread) emit(ev app.SystemEvent) {
	select {
	case t.events <- ev:
	default:
		t.log.Warn("winos: a system event was dropped", "event", string(ev))
	}
}

// unregisterAll undoes the window's registrations before it is destroyed.
func (t *thread) unregisterAll() {
	for _, n := range t.hotkeys {
		_, _, _ = procUnregisterHotKey.Call(t.hwnd, n)
	}
	clear(t.hotkeys)
	clear(t.byNumber)
	_, _, _ = procWTSUnRegisterSessionNotification.Call(t.hwnd)
	if t.powerNote != 0 {
		_, _, _ = procUnregisterSuspendResumeNotification.Call(t.powerNote)
		t.powerNote = 0
	}
}

// post queues f to run on the thread and returns at once.
func (t *thread) post(f func()) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errThreadGone
	}
	t.queue = append(t.queue, f)
	t.mu.Unlock()
	if r, _, err := procPostMessageW.Call(t.hwnd, wmAppCall, 0, 0); r == 0 {
		return fmt.Errorf("winos: PostMessageW: %w", err)
	}
	return nil
}

// call runs f on the thread and returns once it ran. It must not be called from the thread itself.
func (t *thread) call(f func()) error {
	done := make(chan struct{})
	if err := t.post(func() { f(); close(done) }); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-t.done:
		select {
		case <-done:
			return nil
		default:
			return errThreadGone
		}
	}
}

// close ends the thread: its window is destroyed and the loop returns.
func (t *thread) close() {
	_, _, _ = procPostMessageW.Call(t.hwnd, wmClose, 0, 0)
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		t.log.Warn("winos: the winos thread didn't end")
	}
}

// registerHotkey is RegisterHotKey on the thread: ids pause and stop are 1 and 2, others get their own.
func (t *thread) registerHotkey(id app.HotkeyID, mods uint32, vk uint16) error {
	var result error
	err := t.call(func() {
		if n, ok := t.hotkeys[id]; ok {
			_, _, _ = procUnregisterHotKey.Call(t.hwnd, n)
			delete(t.hotkeys, id)
			delete(t.byNumber, n)
		}
		var n uintptr
		switch id {
		case app.HotkeyPause:
			n = 1
		case app.HotkeyStop:
			n = 2
		default:
			n = t.nextKey
			t.nextKey++
		}
		if r, _, err := procRegisterHotKey.Call(t.hwnd, n, uintptr(mods|modNoRepeat), uintptr(vk)); r == 0 {
			if errors.Is(err, errHotkeyRegistered) {
				result = app.ErrHotkeyTaken
			} else {
				result = fmt.Errorf("winos: RegisterHotKey: %w", err)
			}
			return
		}
		t.hotkeys[id], t.byNumber[n] = n, id
	})
	if err != nil {
		return err
	}
	return result
}

// unregisterHotkey is UnregisterHotKey on the thread; an id that isn't registered does nothing.
func (t *thread) unregisterHotkey(id app.HotkeyID) {
	_ = t.call(func() {
		if n, ok := t.hotkeys[id]; ok {
			_, _, _ = procUnregisterHotKey.Call(t.hwnd, n)
			delete(t.hotkeys, id)
			delete(t.byNumber, n)
		}
	})
}

// keepAwake is SetThreadExecutionState on the thread, which stays alive for the app's life: the state belongs to the
// calling thread, so a goroutine's passing thread would lose it.
func (t *thread) keepAwake(on bool) error {
	flags := uintptr(esContinuous)
	if on {
		flags |= esSystemRequired | esDisplayRequired
	}
	return t.post(func() {
		if r, _, err := procSetThreadExecutionState.Call(flags); r == 0 {
			t.log.Warn("winos: SetThreadExecutionState failed", "err", err)
		}
	})
}

// The display affinities (SetWindowDisplayAffinity).
const (
	wdaNone               = 0x00
	wdaExcludeFromCapture = 0x11
)

// setDisplayAffinity is SetWindowDisplayAffinity on a window of this process.
func setDisplayAffinity(hwnd uintptr, affinity uint32) error {
	if hwnd == 0 {
		return errors.New("winos: no window")
	}
	if r, _, err := procSetWindowDisplayAffinity.Call(hwnd, uintptr(affinity)); r == 0 {
		return fmt.Errorf("winos: SetWindowDisplayAffinity: %w", err)
	}
	return nil
}

// displayAffinity is GetWindowDisplayAffinity (the tests' check).
func displayAffinity(hwnd uintptr) (uint32, error) {
	var a uint32
	if r, _, err := procGetWindowDisplayAffinity.Call(hwnd, uintptr(unsafe.Pointer(&a))); r == 0 {
		return 0, fmt.Errorf("winos: GetWindowDisplayAffinity: %w", err)
	}
	return a, nil
}
