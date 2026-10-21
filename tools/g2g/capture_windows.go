//go:build windows

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GDI, through lazy procs from System32 (no cgo, 01 §12.2).
var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procCreateCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection              = gdi32.NewProc("CreateDIBSection")
	procSelectObject                  = gdi32.NewProc("SelectObject")
	procBitBlt                        = gdi32.NewProc("BitBlt")
	procGdiFlush                      = gdi32.NewProc("GdiFlush")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procDeleteDC                      = gdi32.NewProc("DeleteDC")
	procGetDeviceCaps                 = gdi32.NewProc("GetDeviceCaps")
)

const (
	smCxScreen   = 0
	smCyScreen   = 1
	vRefresh     = 116 // GetDeviceCaps: the display's vertical refresh rate, in Hz
	srcCopy      = 0x00CC0020
	captureBlt   = 0x40000000 // include layered windows
	biRGB        = 0
	dibRGBColors = 0
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, (DPI_AWARENESS_CONTEXT)-4: the screenshot is in physical pixels,
	// whatever the display scaling, so the clock's strip is 48 px a cell in it.
	dpiPerMonitorAwareV2 = ^uintptr(3)
)

type bitmapInfoHeader struct {
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
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

// gdiCapturer copies the primary monitor into a 32-bit top-down DIB section with BitBlt (SRCCOPY | CAPTUREBLT).
// The screen DC is released by the thread that got it, so all GDI work runs on one locked OS thread, which serves
// grab requests.
type gdiCapturer struct {
	req  chan struct{}
	resp chan error
	done chan struct{}
	hz   int
	f    Frame
}

func openGDI() (capturer, error) {
	c := &gdiCapturer{req: make(chan struct{}), resp: make(chan error), done: make(chan struct{})}
	ready := make(chan error, 1)
	go c.loop(ready)
	if err := <-ready; err != nil {
		<-c.done
		return nil, err
	}
	return c, nil
}

func (c *gdiCapturer) grab() (*Frame, error) {
	c.req <- struct{}{}
	if err := <-c.resp; err != nil {
		return nil, err
	}
	return &c.f, nil
}

func (c *gdiCapturer) refreshHz() int { return c.hz }

func (c *gdiCapturer) close() error {
	close(c.req)
	<-c.done
	return nil
}

func (c *gdiCapturer) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.done)

	// Without DPI awareness, Windows scales the screenshot to the logical resolution.
	if procSetProcessDpiAwarenessContext.Find() == nil {
		_, _, _ = procSetProcessDpiAwarenessContext.Call(dpiPerMonitorAwareV2)
	} else if procSetProcessDPIAware.Find() == nil {
		_, _, _ = procSetProcessDPIAware.Call()
	}
	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if w == 0 || h == 0 || w > 1<<15 || h > 1<<15 {
		ready <- fmt.Errorf("the primary monitor's size is %d × %d", w, h)
		return
	}
	screen, _, err := procGetDC.Call(0)
	if screen == 0 {
		ready <- fmt.Errorf("GetDC: %w", err)
		return
	}
	defer func() { _, _, _ = procReleaseDC.Call(0, screen) }()
	mem, _, err := procCreateCompatibleDC.Call(screen)
	if mem == 0 {
		ready <- fmt.Errorf("CreateCompatibleDC: %w", err)
		return
	}
	defer func() { _, _, _ = procDeleteDC.Call(mem) }()
	bi := bitmapInfo{Header: bitmapInfoHeader{
		Width:       int32(w),  //nolint:gosec // G115: checked above, at most 32768
		Height:      -int32(h), //nolint:gosec // G115: as Width; negative is top-down
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	var bits unsafe.Pointer
	bmp, _, err := procCreateDIBSection.Call(screen, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		ready <- fmt.Errorf("CreateDIBSection: %w", err)
		return
	}
	defer func() { _, _, _ = procDeleteObject.Call(bmp) }()
	old, _, _ := procSelectObject.Call(mem, bmp)
	defer func() { _, _, _ = procSelectObject.Call(mem, old) }()
	hz, _, _ := procGetDeviceCaps.Call(screen, vRefresh)
	if hz > 1 && hz < 1000 { // 0 and 1 mean the hardware's default
		c.hz = int(hz)
	}
	wi, hi := int(w), int(h)
	c.f = Frame{W: wi, H: hi, Stride: 4 * wi, Pix: unsafe.Slice((*byte)(bits), 4*wi*hi)}
	ready <- nil

	for range c.req {
		r, _, err := procBitBlt.Call(mem, 0, 0, w, h, screen, 0, 0, srcCopy|captureBlt)
		if r == 0 {
			c.resp <- fmt.Errorf("BitBlt: %w", err)
			continue
		}
		_, _, _ = procGdiFlush.Call() // GDI may batch drawing; the DIB's bits are read directly
		c.resp <- nil
	}
}

// osVersion is the Windows release and build, like "Windows 11 10.0.26100" (Windows 11 still reports 10.0; its
// builds start at 22000).
func osVersion() string {
	v := windows.RtlGetVersion()
	name := "Windows"
	if v.MajorVersion == 10 && v.MinorVersion == 0 {
		name = "Windows 10"
		if v.BuildNumber >= 22000 {
			name = "Windows 11"
		}
	}
	return fmt.Sprintf("%s %d.%d.%d", name, v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
