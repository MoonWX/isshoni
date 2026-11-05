//go:build windows

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect-check's noise window (protect.go): a topmost popup over the middle quarter of the primary monitor that never
// takes the focus and paints new random blocks on every 16 ms timer tick. It lives on a locked OS thread of its own,
// which runs its message loop; the other methods may be called from any goroutine (the affinity calls work from any
// thread of the process, ShowWindow sends to the owning thread).

var (
	pwUser32                   = windows.NewLazySystemDLL("user32.dll")
	pwGdi32                    = windows.NewLazySystemDLL("gdi32.dll")
	pwKernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procPWGetModuleHandleW     = pwKernel32.NewProc("GetModuleHandleW")
	procPWRegisterClassExW     = pwUser32.NewProc("RegisterClassExW")
	procPWCreateWindowExW      = pwUser32.NewProc("CreateWindowExW")
	procPWDefWindowProcW       = pwUser32.NewProc("DefWindowProcW")
	procPWShowWindow           = pwUser32.NewProc("ShowWindow")
	procPWSetTimer             = pwUser32.NewProc("SetTimer")
	procPWGetMessageW          = pwUser32.NewProc("GetMessageW")
	procPWTranslateMessage     = pwUser32.NewProc("TranslateMessage")
	procPWDispatchMessageW     = pwUser32.NewProc("DispatchMessageW")
	procPWPostMessageW         = pwUser32.NewProc("PostMessageW")
	procPWPostQuitMessage      = pwUser32.NewProc("PostQuitMessage")
	procPWInvalidateRect       = pwUser32.NewProc("InvalidateRect")
	procPWBeginPaint           = pwUser32.NewProc("BeginPaint")
	procPWEndPaint             = pwUser32.NewProc("EndPaint")
	procPWGetClientRect        = pwUser32.NewProc("GetClientRect")
	procPWMonitorFromPoint     = pwUser32.NewProc("MonitorFromPoint")
	procPWMonitorFromWindow    = pwUser32.NewProc("MonitorFromWindow")
	procPWGetMonitorInfoW      = pwUser32.NewProc("GetMonitorInfoW")
	procPWSetWindowDispAff     = pwUser32.NewProc("SetWindowDisplayAffinity")
	procPWGetWindowDispAff     = pwUser32.NewProc("GetWindowDisplayAffinity")
	procPWStretchDIBits        = pwGdi32.NewProc("StretchDIBits")
	patternClassOnce           sync.Once
	patternClassErr            error
	patternClassName, _        = windows.UTF16PtrFromString("isshoni-enginetest-protect")
	patternTitle, _            = windows.UTF16PtrFromString("isshoni-enginetest protect-check")
	patternWndProcCallback     uintptr
	patternBits                []byte // the noise, painted by the window's thread only
	patternBitmapInfo          pwBitmapInfo
	errPatternWindowNotCreated = errors.New("CreateWindowExW failed")
)

// The Win32 constants used here (winuser.h, wingdi.h).
const (
	pwWSPopup            = 0x80000000
	pwWSExTopmost        = 0x00000008
	pwWSExToolWindow     = 0x00000080
	pwWSExNoActivate     = 0x08000000
	pwSWHide             = 0
	pwSWShowNoActivate   = 4
	pwWMDestroy          = 0x0002
	pwWMClose            = 0x0010
	pwWMPaint            = 0x000F
	pwWMEraseBkgnd       = 0x0014
	pwWMMouseActivate    = 0x0021
	pwWMTimer            = 0x0113
	pwMANoActivate       = 3
	pwMonitorPrimary     = 1 // MONITOR_DEFAULTTOPRIMARY
	pwMonitorNearest     = 2 // MONITOR_DEFAULTTONEAREST
	pwDIBRGBColors       = 0
	pwSrcCopy            = 0x00CC0020
	pwWDANone            = 0
	pwTimerEvery         = 16 // ms
	patternW, patternH   = 160, 90
	patternCloseDeadline = 2 * time.Second
)

type pwRect struct{ Left, Top, Right, Bottom int32 }

type pwMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
	Private uint32
}

type pwWndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type pwPaintStruct struct {
	HDC        uintptr
	Erase      int32
	Paint      pwRect
	Restore    int32
	IncUpdate  int32
	RGBReserve [32]byte
}

type pwMonitorInfoEx struct {
	Size    uint32
	Monitor pwRect
	Work    pwRect
	Flags   uint32
	Device  [32]uint16
}

type pwBitmapInfo struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
	Colors        [1]uint32
}

type noiseWindow struct {
	hwnd uintptr
	done chan struct{}
	once sync.Once
}

// createPatternWindow creates and shows the noise window; it is painting when this returns.
func createPatternWindow() (patternWindow, error) {
	ready := make(chan error, 1)
	w := &noiseWindow{done: make(chan struct{})}
	go w.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}

// run is the window's thread: the class, the window, then the message loop until WM_QUIT. The OS thread stays
// locked to the end, so it exits with the goroutine.
func (w *noiseWindow) run(ready chan<- error) {
	runtime.LockOSThread()
	defer close(w.done)
	if err := registerPatternClass(); err != nil {
		ready <- err
		return
	}
	var mi pwMonitorInfoEx
	mi.Size = uint32(unsafe.Sizeof(mi))
	mon, _, _ := procPWMonitorFromPoint.Call(0, pwMonitorPrimary) // POINT{0, 0} by value
	if r, _, err := procPWGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		ready <- fmt.Errorf("GetMonitorInfoW: %w", err)
		return
	}
	mw, mh := mi.Monitor.Right-mi.Monitor.Left, mi.Monitor.Bottom-mi.Monitor.Top
	x, y := mi.Monitor.Left+mw/4, mi.Monitor.Top+mh/4
	instance, _, _ := procPWGetModuleHandleW.Call(0)
	hwnd, _, err := procPWCreateWindowExW.Call(pwWSExTopmost|pwWSExToolWindow|pwWSExNoActivate,
		uintptr(unsafe.Pointer(patternClassName)), uintptr(unsafe.Pointer(patternTitle)), pwWSPopup,
		uintptr(x), uintptr(y), uintptr(mw/2), uintptr(mh/2), 0, 0, instance, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("%w: %w", errPatternWindowNotCreated, err)
		return
	}
	w.hwnd = hwnd
	_, _, _ = procPWShowWindow.Call(hwnd, pwSWShowNoActivate)
	if r, _, err := procPWSetTimer.Call(hwnd, 1, pwTimerEvery, 0); r == 0 {
		ready <- fmt.Errorf("SetTimer: %w", err)
		_, _, _ = procPWPostMessageW.Call(hwnd, pwWMClose, 0, 0)
	} else {
		ready <- nil
	}
	var msg pwMsg
	for {
		r, _, _ := procPWGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { //nolint:gosec // G115: GetMessageW returns a BOOL (0, nonzero or -1)
			return
		}
		_, _, _ = procPWTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procPWDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func registerPatternClass() error {
	patternClassOnce.Do(func() {
		patternWndProcCallback = windows.NewCallback(patternWndProc)
		patternBits = make([]byte, patternW*patternH*4)
		patternBitmapInfo = pwBitmapInfo{Width: patternW, Height: -patternH, Planes: 1, BitCount: 32}
		patternBitmapInfo.Size = uint32(unsafe.Offsetof(patternBitmapInfo.Colors))
		instance, _, _ := procPWGetModuleHandleW.Call(0)
		wc := pwWndClassEx{WndProc: patternWndProcCallback, Instance: instance, ClassName: patternClassName}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procPWRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			patternClassErr = fmt.Errorf("RegisterClassExW: %w", err)
		}
	})
	return patternClassErr
}

func patternWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case pwWMTimer:
		_, _, _ = procPWInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case pwWMEraseBkgnd:
		return 1 // WM_PAINT covers the whole client area
	case pwWMMouseActivate:
		return pwMANoActivate
	case pwWMPaint:
		paintNoise(hwnd)
		return 0
	case pwWMDestroy:
		_, _, _ = procPWPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procPWDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// paintNoise fills the bitmap with new random bytes and stretches it over the client area (nearest neighbour, so the
// blocks keep hard edges, which no encoder predicts from the last frame).
func paintNoise(hwnd uintptr) {
	for i := 0; i+8 <= len(patternBits); i += 8 {
		v := rand.Uint64() //nolint:gosec // G404: a test pattern, not a secret
		for j := range 8 {
			patternBits[i+j] = byte(v >> (8 * j))
		}
	}
	var ps pwPaintStruct
	hdc, _, _ := procPWBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	var rc pwRect
	_, _, _ = procPWGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	_, _, _ = procPWStretchDIBits.Call(hdc, 0, 0, uintptr(rc.Right), uintptr(rc.Bottom), 0, 0, patternW, patternH,
		uintptr(unsafe.Pointer(&patternBits[0])), uintptr(unsafe.Pointer(&patternBitmapInfo)), pwDIBRGBColors,
		pwSrcCopy)
	_, _, _ = procPWEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

func (w *noiseWindow) Handle() uintptr { return w.hwnd }

func (w *noiseWindow) MonitorID() (string, error) {
	mon, _, _ := procPWMonitorFromWindow.Call(w.hwnd, pwMonitorNearest)
	var mi pwMonitorInfoEx
	mi.Size = uint32(unsafe.Sizeof(mi))
	if r, _, err := procPWGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return "", fmt.Errorf("GetMonitorInfoW: %w", err)
	}
	return "mon:" + windows.UTF16ToString(mi.Device[:]), nil
}

func (w *noiseWindow) Affinity() (uint32, error) {
	var a uint32
	if r, _, err := procPWGetWindowDispAff.Call(w.hwnd, uintptr(unsafe.Pointer(&a))); r == 0 {
		return 0, err
	}
	return a, nil
}

func (w *noiseWindow) Unprotect() error {
	if r, _, err := procPWSetWindowDispAff.Call(w.hwnd, pwWDANone); r == 0 {
		return err
	}
	return nil
}

func (w *noiseWindow) SetVisible(visible bool) {
	cmd := uintptr(pwSWHide)
	if visible {
		cmd = pwSWShowNoActivate
	}
	_, _, _ = procPWShowWindow.Call(w.hwnd, cmd)
}

// Close destroys the window (WM_CLOSE: DefWindowProcW destroys it, WM_DESTROY ends the loop) and waits up to 2 s for
// its thread.
func (w *noiseWindow) Close() {
	w.once.Do(func() {
		_, _, _ = procPWPostMessageW.Call(w.hwnd, pwWMClose, 0, 0)
		t := time.NewTimer(patternCloseDeadline)
		defer t.Stop()
		select {
		case <-w.done:
		case <-t.C:
		}
	})
}
