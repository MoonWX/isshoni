//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// avsync's Windows half: the helper processes as children of explorer.exe, and the flash window (a borderless
// flip-model swap chain driven through D3D11 and DXGI's COM vtables; no cgo).

const (
	avsyncFlashW      = 1280
	avsyncFlashH      = 720
	avsyncFlashLen    = 100 * time.Millisecond
	avsyncHelperReady = 20 * time.Second
	avsyncHelperStop  = 5 * time.Second
	avsyncStopPoll    = 15 // the flash window looks for the stop file every this many frames
)

var (
	d3d11                     = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice     = d3d11.NewProc("D3D11CreateDevice")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procUnregisterClassW      = user32.NewProc("UnregisterClassW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procPeekMessageW          = user32.NewProc("PeekMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procQueryPerformanceCount = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFreq  = kernel32.NewProc("QueryPerformanceFrequency")
)

// ------------------------------------------------------------------------------------------- the helper processes

// winHelpers starts copies of this program with -role flash or click as children of explorer.exe. Their results
// and logs go into a temporary folder that Close removes.
type winHelpers struct {
	exe     string
	dll     string
	verbose bool
	dir     string
	stderr  io.Writer
}

func newAVSyncHelpers(g *globals) (avsyncHelpers, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dll := g.dll
	if dll == "" {
		dll = filepath.Dir(exe)
	}
	// The helpers start in explorer's working folder: a relative -dll (the run book's "-dll ci") must be made
	// absolute here.
	if dll, err = filepath.Abs(dll); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "isshoni-avsync-")
	if err != nil {
		return nil, err
	}
	return &winHelpers{exe: exe, dll: dll, verbose: g.verbose, dir: dir, stderr: g.stderr}, nil
}

func (h *winHelpers) Close() { _ = os.RemoveAll(h.dir) }

func (h *winHelpers) path(name string) string { return filepath.Join(h.dir, name) }

// explorerProcess opens the shell's process (the owner of the shell window) for use as a parent: creating a process
// under it and duplicating the child's standard handles into it.
func explorerProcess() (windows.Handle, error) {
	shell := windows.GetShellWindow()
	if shell == 0 {
		return 0, errors.New("no shell window: explorer.exe isn't running")
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(shell, &pid); err != nil || pid == 0 {
		return 0, fmt.Errorf("the shell window's process: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE, false, pid)
	if err != nil {
		return 0, fmt.Errorf("opening the shell's process: %w", err)
	}
	return h, nil
}

// start runs this program with args (after the global flags) as a child of explorer.exe, its stdout and stderr in
// the log file logName. It returns the command and a channel with Wait's result.
func (h *winHelpers) start(ctx context.Context, args []string, logName string) (*exec.Cmd, <-chan error, error) {
	parent, err := explorerProcess()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	full := []string{"-dll", h.dll}
	if h.verbose {
		full = append(full, "-v")
	}
	log, err := os.Create(h.path(logName))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.CommandContext(ctx, h.exe, append(full, args...)...) //nolint:gosec // G204: this program itself
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{ParentProcess: syscall.Handle(parent)}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting a helper under explorer.exe: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done, nil
}

// logOf returns a helper's log, for an error message.
func (h *winHelpers) logOf(name string) string {
	b, err := os.ReadFile(h.path(name)) //nolint:gosec // G304: our own temporary folder
	if err != nil || len(b) == 0 {
		return ""
	}
	const keep = 2000
	if len(b) > keep {
		b = b[len(b)-keep:]
	}
	return "; its log: " + string(b)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: our own temporary folder
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type winFlash struct {
	h    *winHelpers
	cmd  *exec.Cmd
	done <-chan error
}

func (h *winHelpers) StartFlash(ctx context.Context) (flashProc, error) {
	args := []string{"avsync", "-role", "flash", "-result", h.path("flash.json"), "-stop", h.path("flash.stop"),
		"-ready", h.path("flash.ready"), "-life", avsyncFlashLifetime.String()}
	cmd, done, err := h.start(ctx, args, "flash.log")
	if err != nil {
		return nil, err
	}
	f := &winFlash{h: h, cmd: cmd, done: done}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	deadline := time.NewTimer(avsyncHelperReady)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(h.path("flash.ready")); err == nil {
			return f, nil
		}
		select {
		case err := <-done:
			return nil, fmt.Errorf("the flash window exited before its first frame (%w)%s", err, h.logOf("flash.log"))
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("the flash window showed no frame within %v%s", avsyncHelperReady, h.logOf("flash.log"))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (f *winFlash) Stop() (flashResult, error) {
	var res flashResult
	if err := os.WriteFile(f.h.path("flash.stop"), nil, 0o600); err != nil {
		_ = f.cmd.Process.Kill()
	}
	select {
	case <-f.done:
	case <-time.After(avsyncHelperStop):
		_ = f.cmd.Process.Kill()
		<-f.done
		return res, fmt.Errorf("the flash window didn't stop within %v", avsyncHelperStop)
	}
	if err := readJSONFile(f.h.path("flash.json"), &res); err != nil {
		return res, fmt.Errorf("its result: %w%s", err, f.h.logOf("flash.log"))
	}
	if res.Error != "" {
		return res, errors.New(res.Error + f.h.logOf("flash.log"))
	}
	return res, nil
}

type winClicks struct {
	h    *winHelpers
	cmd  *exec.Cmd
	done <-chan error
}

func (h *winHelpers) StartClicks(ctx context.Context, n int) (clickProc, error) {
	args := []string{"avsync", "-role", "click", "-clicks", strconv.Itoa(n), "-result", h.path("click.json")}
	cmd, done, err := h.start(ctx, args, "click.log")
	if err != nil {
		return nil, err
	}
	return &winClicks{h: h, cmd: cmd, done: done}, nil
}

func (c *winClicks) Wait(ctx context.Context) (clickResult, error) {
	var res clickResult
	select {
	case err := <-c.done:
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				return res, err
			}
		}
	case <-ctx.Done():
		_ = c.cmd.Process.Kill()
		<-c.done
		return res, fmt.Errorf("the click process didn't finish: %w", ctx.Err())
	}
	if err := readJSONFile(c.h.path("click.json"), &res); err != nil {
		return res, fmt.Errorf("its result: %w%s", err, c.h.logOf("click.log"))
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// --------------------------------------------------------------------------------------------- the flash window

// wndClassEx is WNDCLASSEXW.
type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// winMsg is MSG.
type winMsg struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       [2]int32
	LPrivate uint32
}

// swapChainDesc1 is DXGI_SWAP_CHAIN_DESC1.
type swapChainDesc1 struct {
	Width       uint32
	Height      uint32
	Format      uint32
	Stereo      int32
	SampleCount uint32
	SampleQual  uint32
	BufferUsage uint32
	BufferCount uint32
	Scaling     uint32
	SwapEffect  uint32
	AlphaMode   uint32
	Flags       uint32
}

// dxgiFrameStatistics is DXGI_FRAME_STATISTICS.
type dxgiFrameStatistics struct {
	PresentCount        uint32
	PresentRefreshCount uint32
	SyncRefreshCount    uint32
	SyncQPCTime         int64
	SyncGPUTime         int64
}

const (
	wsPopup         = 0x80000000
	wsVisible       = 0x10000000
	wsExTopmost     = 0x00000008
	wmClose         = 0x0010
	pmRemove        = 0x0001
	smCxScreen      = 0
	smCyScreen      = 1
	idcArrow        = 32512
	d3dDriverHW     = 1
	d3d11SDKVersion = 7
	d3d11BGRA       = 0x20
	dxgiFormatBGRA  = 87
	dxgiUsageRT     = 0x20
	dxgiFlipDiscard = 4
	dxgiMWANoAltEnt = 0x2

	// vtable slots.
	vtQueryInterface         = 0
	vtRelease                = 2
	vtDXGIObjectGetParent    = 6
	vtDXGIDeviceGetAdapter   = 7
	vtFactoryMakeWindowAssoc = 8
	vtFactory2CreateForHwnd  = 15
	vtDeviceCreateRTV        = 9
	vtContextClearRTV        = 50
	vtSwapPresent            = 8
	vtSwapGetBuffer          = 9
	vtSwapGetFrameStats      = 16
	vtSwapGetLastPresentCnt  = 17
)

var (
	iidIDXGIDevice     = windows.GUID{Data1: 0x54ec77fa, Data2: 0x1377, Data3: 0x44e6, Data4: [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidIDXGIFactory2   = windows.GUID{Data1: 0x50c83a1c, Data2: 0xe072, Data3: 0x4c48, Data4: [8]byte{0x87, 0xb0, 0x36, 0x30, 0xfa, 0x36, 0xa6, 0xd0}}
	iidID3D11Texture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// ptrAt reinterprets an address as a pointer (memory the Go heap doesn't own: a COM object or its vtable), as
// internal/media/native's nativeBytes does, so vet's unsafeptr check has no conversion to flag.
func ptrAt(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

// method is the address of a COM object's vtable slot i.
func method(obj uintptr, i int) uintptr {
	vtbl := *(*uintptr)(ptrAt(obj))
	return *(*uintptr)(ptrAt(vtbl + uintptr(i)*unsafe.Sizeof(uintptr(0))))
}

func release(obj uintptr) {
	if obj != 0 {
		_, _, _ = syscall.SyscallN(method(obj, vtRelease), obj)
	}
}

func failedHR(r uintptr) bool { return int32(r) < 0 } //nolint:gosec // G115: an HRESULT in a register

func hrError(what string, r uintptr) error {
	return fmt.Errorf("%s: HRESULT 0x%08x", what, uint32(r)) //nolint:gosec // G115: an HRESULT in a register
}

func qpcNow() int64 {
	var q int64
	_, _, _ = procQueryPerformanceCount.Call(uintptr(unsafe.Pointer(&q)))
	return q
}

func qpcFreq() int64 {
	var f int64
	if r, _, _ := procQueryPerformanceFreq.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
		return 0
	}
	return f
}

var flashClosed atomic.Bool

func flashWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	if msg == wmClose {
		flashClosed.Store(true)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// flashWindow is the window and its swap chain.
type flashWindow struct {
	class    *uint16
	instance windows.Handle
	hwnd     uintptr
	dev      uintptr
	ctx      uintptr
	sc       uintptr
	rtv      uintptr
}

func (w *flashWindow) close() {
	release(w.rtv)
	release(w.sc)
	release(w.ctx)
	release(w.dev)
	if w.hwnd != 0 {
		_, _, _ = procDestroyWindow.Call(w.hwnd)
	}
	if w.class != nil {
		_, _, _ = procUnregisterClassW.Call(uintptr(unsafe.Pointer(w.class)), uintptr(w.instance))
	}
}

// openFlashWindow creates the borderless, topmost 1280×720 window centered on the primary monitor and its
// flip-model swap chain (FLIP_DISCARD, two BGRA buffers) on a hardware D3D11 device.
func openFlashWindow() (*flashWindow, error) {
	w := &flashWindow{}
	if err := windows.GetModuleHandleEx(0, nil, &w.instance); err != nil {
		return nil, fmt.Errorf("GetModuleHandleEx: %w", err)
	}
	class, err := windows.UTF16PtrFromString("isshoni-enginetest-avsync-flash")
	if err != nil {
		return nil, err
	}
	title, err := windows.UTF16PtrFromString(avsyncFlashName)
	if err != nil {
		return nil, err
	}
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{WndProc: windows.NewCallback(flashWndProc), Instance: w.instance, Cursor: cursor, ClassName: class}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return nil, fmt.Errorf("RegisterClassExW: %w", e)
	}
	w.class = class
	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
	x := max(0, (int32(sw)-avsyncFlashW)/2) //nolint:gosec // G115: a screen size
	y := max(0, (int32(sh)-avsyncFlashH)/2) //nolint:gosec // G115: a screen size
	hwnd, _, e := procCreateWindowExW.Call(wsExTopmost, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)),
		wsPopup|wsVisible, uintptr(x), uintptr(y), avsyncFlashW, avsyncFlashH, 0, 0, uintptr(w.instance), 0)
	if hwnd == 0 {
		w.close()
		return nil, fmt.Errorf("CreateWindowExW: %w", e)
	}
	w.hwnd = hwnd
	if err := w.openSwapChain(); err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *flashWindow) openSwapChain() error {
	var level uint32
	r, _, _ := syscall.SyscallN(procD3D11CreateDevice.Addr(), 0, d3dDriverHW, 0, d3d11BGRA, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&w.dev)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&w.ctx)))
	if failedHR(r) {
		return hrError("D3D11CreateDevice", r)
	}
	var dxgiDev, adapter, factory uintptr
	defer func() {
		release(factory)
		release(adapter)
		release(dxgiDev)
	}()
	r, _, _ = syscall.SyscallN(method(w.dev, vtQueryInterface), w.dev, uintptr(unsafe.Pointer(&iidIDXGIDevice)),
		uintptr(unsafe.Pointer(&dxgiDev)))
	if failedHR(r) {
		return hrError("IDXGIDevice", r)
	}
	r, _, _ = syscall.SyscallN(method(dxgiDev, vtDXGIDeviceGetAdapter), dxgiDev, uintptr(unsafe.Pointer(&adapter)))
	if failedHR(r) {
		return hrError("IDXGIDevice::GetAdapter", r)
	}
	r, _, _ = syscall.SyscallN(method(adapter, vtDXGIObjectGetParent), adapter,
		uintptr(unsafe.Pointer(&iidIDXGIFactory2)), uintptr(unsafe.Pointer(&factory)))
	if failedHR(r) {
		return hrError("IDXGIFactory2", r)
	}
	desc := swapChainDesc1{Width: avsyncFlashW, Height: avsyncFlashH, Format: dxgiFormatBGRA, SampleCount: 1,
		BufferUsage: dxgiUsageRT, BufferCount: 2, SwapEffect: dxgiFlipDiscard}
	r, _, _ = syscall.SyscallN(method(factory, vtFactory2CreateForHwnd), factory, w.dev, w.hwnd,
		uintptr(unsafe.Pointer(&desc)), 0, 0, uintptr(unsafe.Pointer(&w.sc)))
	if failedHR(r) {
		return hrError("IDXGIFactory2::CreateSwapChainForHwnd", r)
	}
	_, _, _ = syscall.SyscallN(method(factory, vtFactoryMakeWindowAssoc), factory, w.hwnd, dxgiMWANoAltEnt)
	var tex uintptr
	r, _, _ = syscall.SyscallN(method(w.sc, vtSwapGetBuffer), w.sc, 0, uintptr(unsafe.Pointer(&iidID3D11Texture2D)),
		uintptr(unsafe.Pointer(&tex)))
	if failedHR(r) {
		return hrError("IDXGISwapChain::GetBuffer", r)
	}
	defer release(tex)
	// D3D11's flip model: buffer 0 is always the current back buffer, so one view serves every frame.
	r, _, _ = syscall.SyscallN(method(w.dev, vtDeviceCreateRTV), w.dev, tex, 0, uintptr(unsafe.Pointer(&w.rtv)))
	if failedHR(r) {
		return hrError("ID3D11Device::CreateRenderTargetView", r)
	}
	return nil
}

// pump handles the window's messages.
func (w *flashWindow) pump() {
	var m winMsg
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// frame clears the back buffer to white or black, presents it with sync interval 1 and returns its present count
// and the frame statistics (ok false when DXGI has none yet).
func (w *flashWindow) frame(white bool) (n uint32, st dxgiFrameStatistics, ok bool, err error) {
	color := [4]float32{0, 0, 0, 1}
	if white {
		color = [4]float32{1, 1, 1, 1}
	}
	_, _, _ = syscall.SyscallN(method(w.ctx, vtContextClearRTV), w.ctx, w.rtv, uintptr(unsafe.Pointer(&color)))
	r, _, _ := syscall.SyscallN(method(w.sc, vtSwapPresent), w.sc, 1, 0)
	if failedHR(r) {
		return 0, st, false, hrError("IDXGISwapChain::Present", r)
	}
	r, _, _ = syscall.SyscallN(method(w.sc, vtSwapGetLastPresentCnt), w.sc, uintptr(unsafe.Pointer(&n)))
	if failedHR(r) {
		return 0, st, false, hrError("IDXGISwapChain::GetLastPresentCount", r)
	}
	r, _, _ = syscall.SyscallN(method(w.sc, vtSwapGetFrameStats), w.sc, uintptr(unsafe.Pointer(&st)))
	return n, st, !failedHR(r), nil // DXGI_ERROR_FRAME_STATISTICS_DISJOINT and friends: no reading this frame
}

// runFlashRole is the flash window: black, white for avsyncFlashLen at the start of every second (on QPC), until
// the stop file exists, the window is closed or -life passes; then the first white present of each flash resolved
// to its display time, into -result.
func runFlashRole(ctx context.Context, o avsyncOptions, stderr io.Writer) int {
	runtime.LockOSThread() // the window, its messages and the D3D context stay on this thread
	defer runtime.UnlockOSThread()
	res := flashResult{FlashesNS: []int64{}}
	code := exitPass
	freq := qpcFreq()
	w, err := openFlashWindow()
	switch {
	case freq <= 0:
		res.Error, code = "QueryPerformanceFrequency failed", exitEnv
	case err != nil:
		res.Error, code = err.Error(), exitEnv
	default:
		defer w.close()
		var firstWhite []uint32
		var stats []frameStat
		prevWhite := false
		ready := false
		q0 := qpcNow()
		flashTicks := freq * int64(avsyncFlashLen) / int64(time.Second)
		end := time.Now().Add(o.life)
		for i := 0; ; i++ {
			w.pump()
			if flashClosed.Load() || ctx.Err() != nil || time.Now().After(end) {
				break
			}
			if i%avsyncStopPoll == 0 {
				if _, err := os.Stat(o.stop); err == nil {
					break
				}
			}
			white := (qpcNow()-q0)%freq < flashTicks
			n, st, ok, err := w.frame(white)
			if err != nil {
				res.Error, code = err.Error(), exitFail
				break
			}
			res.Presents++
			if white && !prevWhite {
				firstWhite = append(firstWhite, n)
			}
			prevWhite = white
			if ok {
				fs := frameStat{PresentCount: st.PresentCount, PresentRefreshCount: st.PresentRefreshCount,
					SyncRefreshCount: st.SyncRefreshCount, SyncQPC: st.SyncQPCTime}
				if len(stats) == 0 || stats[len(stats)-1] != fs {
					stats = append(stats, fs)
				}
			}
			if !ready {
				if err := os.WriteFile(o.ready, nil, 0o600); err != nil {
					res.Error, code = err.Error(), exitEnv
					break
				}
				ready = true
			}
		}
		resolved := flashTimes(firstWhite, stats, freq)
		resolved.Presents, resolved.Error = res.Presents, res.Error
		res = resolved
	}
	if err := writeJSONFile(o.result, res); err != nil {
		fmt.Fprintln(stderr, "avsync flash:", err)
		return exitEnv
	}
	if res.Error != "" {
		fmt.Fprintln(stderr, "avsync flash:", res.Error)
	}
	return code
}
