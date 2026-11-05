//go:build windows

package winos

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// The Sharing pill (03 §11.4; README WA80): a small always-on-top window on the winos thread, which owns its
// message loop. CreateWindowExW(WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE | WS_EX_LAYERED, …, WS_POPUP),
// rounded corners on Windows 11 (DWMWA_WINDOW_CORNER_PREFERENCE), per-monitor DPI (GetDpiForMonitor,
// WM_DPICHANGED), and WDA_EXCLUDEFROMCAPTURE before it is first shown: when that fails (an older build), the pill is
// destroyed and never shown, with a WARN, because a pill that would be captured must not show.
//
// It draws a red dot (amber while paused), the amber marker while a mic prompt waits (a click on it opens the
// window on the prompt card), the app's line and two owner-drawn BUTTON children, Pause/Resume and Stop, whose
// window texts are their names for Narrator (the disabled Pause's name is "Pause needs a newer server"); colors
// follow high contrast (SPI_GETHIGHCONTRAST: the system colors). The rest of the pill is a drag handle
// (WM_NCHITTEST → HTCAPTION); a double click collapses it to a 24 px dot and back. It goes to the top center of the
// captured monitor (the primary one for "") 8 px below its top, or where it was last moved on that monitor
// (PillState.Positions), clamped to the work area; WM_DISPLAYCHANGE clamps it again.

// The pill's window messages, styles and constants (WinUser.h, dwmapi.h, ShellScalingApi.h).
const (
	wmPaint            = 0x000F
	wmEraseBkgnd       = 0x0014
	wmMouseActivate    = 0x0021
	wmDrawItem         = 0x002B
	wmNCHitTest        = 0x0084
	wmNCLButtonDblClk  = 0x00A3
	wmCommand          = 0x0111
	wmLButtonUp        = 0x0202
	wmExitSizeMove     = 0x0232
	wmDPIChanged       = 0x02E0
	htClient           = 1
	htCaption          = 2
	maNoActivate       = 3
	bnClicked          = 0
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsTabStop          = 0x00010000
	bsOwnerDraw        = 0x0000000B
	wsExTopmost        = 0x00000008
	wsExNoActivate     = 0x08000000
	wsExLayered        = 0x00080000
	swHide             = 0
	swShowNoActivate   = 4
	swpNoSize          = 0x0001
	swpNoMove          = 0x0002
	swpNoZOrder        = 0x0004
	swpNoActivate      = 0x0010
	monitorPrimary     = 1 // MONITOR_DEFAULTTOPRIMARY
	monitorNearest     = 2 // MONITOR_DEFAULTTONEAREST
	lwaAlpha           = 0x2
	dwmwaCornerPref    = 33 // DWMWA_WINDOW_CORNER_PREFERENCE (Windows 11)
	dwmwcpRound        = 2
	mdtEffectiveDPI    = 0
	spiGetHighContrast = 0x0042
	hcfHighContrastOn  = 0x1
	odsSelected        = 0x0001
	odsDisabled        = 0x0004
	odsFocus           = 0x0010
	dtCenter           = 0x0001
	dtVCenter          = 0x0004
	dtSingleLine       = 0x0020
	dtNoPrefix         = 0x0800
	dtEndEllipsis      = 0x8000
	bkTransparent      = 1
	nullPen            = 8 // GetStockObject(NULL_PEN)
	fwNormal           = 400
	fwSemiBold         = 600
	defaultCharset     = 1
	cleartypeQuality   = 5
	colorWindow        = 5
	colorWindowText    = 8
	colorHighlight     = 13
	colorBtnFace       = 15
	colorGrayText      = 17
	colorBtnText       = 18
	// The buttons' control ids.
	idFirst = 1 // Pause or Resume
	idStop  = 2
)

// hwndTopmost is HWND_TOPMOST, (HWND)-1.
var hwndTopmost = ^uintptr(0)

var (
	modGdi32  = windows.NewLazySystemDLL("gdi32.dll")
	modDwmapi = windows.NewLazySystemDLL("dwmapi.dll")
	modShcore = windows.NewLazySystemDLL("shcore.dll")

	procShowWindow                 = modUser32.NewProc("ShowWindow")
	procSetWindowPos               = modUser32.NewProc("SetWindowPos")
	procGetWindowRect              = modUser32.NewProc("GetWindowRect")
	procGetClientRect              = modUser32.NewProc("GetClientRect")
	procSetWindowTextW             = modUser32.NewProc("SetWindowTextW")
	procEnableWindow               = modUser32.NewProc("EnableWindow")
	procInvalidateRect             = modUser32.NewProc("InvalidateRect")
	procBeginPaint                 = modUser32.NewProc("BeginPaint")
	procEndPaint                   = modUser32.NewProc("EndPaint")
	procFillRect                   = modUser32.NewProc("FillRect")
	procDrawTextW                  = modUser32.NewProc("DrawTextW")
	procGetSysColor                = modUser32.NewProc("GetSysColor")
	procSystemParametersInfoW      = modUser32.NewProc("SystemParametersInfoW")
	procSetLayeredWindowAttributes = modUser32.NewProc("SetLayeredWindowAttributes")
	procMonitorFromPoint           = modUser32.NewProc("MonitorFromPoint")
	procMonitorFromWindow          = modUser32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW            = modUser32.NewProc("GetMonitorInfoW")
	procEnumDisplayMonitors        = modUser32.NewProc("EnumDisplayMonitors")
	procGetDpiForWindow            = modUser32.NewProc("GetDpiForWindow")
	procCreateSolidBrush           = modGdi32.NewProc("CreateSolidBrush")
	procDeleteObject               = modGdi32.NewProc("DeleteObject")
	procSelectObject               = modGdi32.NewProc("SelectObject")
	procGetStockObject             = modGdi32.NewProc("GetStockObject")
	procEllipse                    = modGdi32.NewProc("Ellipse")
	procSetBkMode                  = modGdi32.NewProc("SetBkMode")
	procSetTextColor               = modGdi32.NewProc("SetTextColor")
	procCreateFontW                = modGdi32.NewProc("CreateFontW")
	procDwmSetWindowAttribute      = modDwmapi.NewProc("DwmSetWindowAttribute")
	procGetDpiForMonitor           = modShcore.NewProc("GetDpiForMonitor")
)

// winRect is RECT.
type winRect struct{ left, top, right, bottom int32 }

func (r winRect) rect() rect {
	return rect{int(r.left), int(r.top), int(r.right), int(r.bottom)}
}

// paintStruct is PAINTSTRUCT.
type paintStruct struct {
	hdc         uintptr
	erase       int32
	paint       winRect
	restore     int32
	incUpdate   int32
	rgbReserved [32]byte
}

// monitorInfoEx is MONITORINFOEXW.
type monitorInfoEx struct {
	size    uint32
	monitor winRect
	work    winRect
	flags   uint32
	device  [32]uint16
}

// drawItemStruct is DRAWITEMSTRUCT.
type drawItemStruct struct {
	_         uint32 // CtlType
	ctlID     uint32
	_         uint32 // itemID
	_         uint32 // itemAction
	itemState uint32
	_         uintptr // hwndItem
	hdc       uintptr
	item      winRect
	_         uintptr // itemData
}

// highContrast is HIGHCONTRASTW.
type highContrast struct {
	size   uint32
	flags  uint32
	scheme *uint16
}

// pillClassName is the pill's window class, registered once per process.
const pillClassName = "isshoni.pill"

var (
	pillClassOnce sync.Once
	pillClassErr  error
	pills         sync.Map // hwnd → *pill: pillWndProc's way to its pill
)

// registerPillClass registers the pill's class with its window procedure.
func registerPillClass() error {
	pillClassOnce.Do(func() {
		name, err := windows.UTF16PtrFromString(pillClassName)
		if err != nil {
			pillClassErr = err
			return
		}
		inst, _, _ := procGetModuleHandleW.Call(0)
		wc := wndClassEx{wndProc: windows.NewCallback(pillWndProc), instance: windows.Handle(inst), className: name}
		wc.size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			pillClassErr = fmt.Errorf("winos: RegisterClassExW (pill): %w", err)
		}
	})
	return pillClassErr
}

// pill is app.Pill on Windows. Set and Actions are safe for concurrent use; every other method runs on the winos
// thread.
type pill struct {
	t       *thread
	log     *slog.Logger
	actions chan app.PillAction
	// onScreen is shown for other goroutines (Shown): set by ShowWindow(SW_SHOWNOACTIVATE), cleared when the pill
	// is hidden or destroyed. It stays false when the window or its WDA_EXCLUDEFROMCAPTURE failed.
	onScreen atomic.Bool

	// Only the winos thread touches these.
	hwnd, first, stop uintptr
	state             app.PillState
	firstAct          app.PillActionKind
	shown             bool   // on screen
	device            string // the monitor it was placed on
	collapsed         bool
	failed            bool // WDA_EXCLUDEFROMCAPTURE failed: never shown
	dpi               int
	font, bold        uintptr
}

var (
	_ app.Pill        = (*pill)(nil)
	_ app.PillShowing = (*pill)(nil)
)

func newPill(t *thread, log *slog.Logger) *pill {
	return &pill{t: t, log: log, actions: make(chan app.PillAction, 16), dpi: baseDPI}
}

// Set shows, updates or hides the pill (idempotent); it doesn't wait for the winos thread.
func (p *pill) Set(s app.PillState) {
	if err := p.t.post(func() { p.apply(s) }); err != nil {
		p.log.Debug("winos: the pill's state didn't reach the thread", "err", err)
	}
}

// Shown reports a pill on screen (app.PillShowing): false while hidden, and when Windows refused to create the
// window or to exclude it from capture, so the app sends the toasts meant for no pill (03 §15).
func (p *pill) Shown() bool { return p.onScreen.Load() }

// Actions delivers what the user did on the pill.
func (p *pill) Actions() <-chan app.PillAction { return p.actions }

// act delivers an action; a full channel drops it with a WARN (nobody is reading).
func (p *pill) act(a app.PillAction) {
	select {
	case p.actions <- a:
	default:
		p.log.Warn("winos: a pill action was dropped", "action", string(a.Kind))
	}
}

// apply is Set on the thread.
func (p *pill) apply(s app.PillState) {
	monitorChanged := s.Monitor != p.state.Monitor
	p.state = s
	if !s.Visible {
		if p.hwnd != 0 && p.shown {
			_, _, _ = procShowWindow.Call(p.hwnd, swHide)
		}
		p.shown = false
		p.onScreen.Store(false)
		return
	}
	if p.failed {
		return
	}
	if p.hwnd == 0 {
		if err := p.create(); err != nil {
			p.log.Warn("winos: the Sharing pill isn't shown", "err", err)
			return
		}
	}
	p.updateTexts()
	if !p.shown || monitorChanged {
		p.place()
	}
	_, _, _ = procSetWindowPos.Call(p.hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	if !p.shown {
		_, _, _ = procShowWindow.Call(p.hwnd, swShowNoActivate)
		p.shown = true
		p.onScreen.Store(true)
	}
	_, _, _ = procInvalidateRect.Call(p.hwnd, 0, 1)
}

// create makes the window and its buttons, hidden, with WDA_EXCLUDEFROMCAPTURE set before anything shows it.
func (p *pill) create() error {
	if err := registerPillClass(); err != nil {
		return err
	}
	cls, _ := windows.UTF16PtrFromString(pillClassName)
	title, _ := windows.UTF16PtrFromString("isshoni")
	inst, _, _ := procGetModuleHandleW.Call(0)
	w, h := pillSize(baseDPI, false)
	hwnd, _, err := procCreateWindowExW.Call(wsExTopmost|wsExToolWindow|wsExNoActivate|wsExLayered,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsPopup, 0, 0, uintptr(w), uintptr(h), 0, 0,
		inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("winos: CreateWindowExW (pill): %w", err)
	}
	if err := setDisplayAffinity(hwnd, wdaExcludeFromCapture); err != nil {
		_, _, _ = procDestroyWindow.Call(hwnd)
		p.failed = true // an older build: a pill that would be captured is never shown (03 §11.4)
		return err
	}
	p.hwnd = hwnd
	pills.Store(hwnd, p)
	if r, _, err := procSetLayeredWindowAttributes.Call(hwnd, 0, 255, lwaAlpha); r == 0 {
		p.log.Debug("winos: SetLayeredWindowAttributes failed", "err", err)
	}
	if procDwmSetWindowAttribute.Find() == nil { // rounded corners; Windows 10 refuses the attribute
		pref := uint32(dwmwcpRound)
		_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaCornerPref, uintptr(unsafe.Pointer(&pref)), 4)
	}
	button, _ := windows.UTF16PtrFromString("BUTTON")
	empty, _ := windows.UTF16PtrFromString("")
	for _, b := range []struct {
		id  uintptr
		out *uintptr
	}{{idFirst, &p.first}, {idStop, &p.stop}} {
		c, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(button)), uintptr(unsafe.Pointer(empty)),
			wsChild|wsVisible|wsTabStop|bsOwnerDraw, 0, 0, 1, 1, hwnd, b.id, inst, 0)
		if c == 0 {
			p.destroy()
			return fmt.Errorf("winos: CreateWindowExW (pill button): %w", err)
		}
		*b.out = c
	}
	p.dpi = 0 // place sets it
	return nil
}

// destroy removes the window (the app's end).
func (p *pill) destroy() {
	if p.hwnd != 0 {
		_, _, _ = procDestroyWindow.Call(p.hwnd)
		pills.Delete(p.hwnd)
	}
	p.hwnd, p.first, p.stop, p.shown = 0, 0, 0, false
	p.onScreen.Store(false)
	p.deleteFonts()
}

// updateTexts gives the window and the buttons their names (Narrator reads them; collapsed, the window's is the
// dot's) and enables Pause by the state.
func (p *pill) updateTexts() {
	s := p.state
	setText(p.hwnd, pillName(s.Text, p.collapsed))
	label, act, enabled := pillButtons(s)
	p.firstAct = act
	if !enabled && s.Text.PauseUnavailable != "" {
		label = s.Text.PauseUnavailable // the disabled button's name says why; it still draws "Pause"
	}
	setText(p.first, label)
	setText(p.stop, s.Text.Stop)
	on := uintptr(0)
	if enabled {
		on = 1
	}
	_, _, _ = procEnableWindow.Call(p.first, on)
	_, _, _ = procInvalidateRect.Call(p.first, 0, 1)
	_, _, _ = procInvalidateRect.Call(p.stop, 0, 1)
}

func setText(hwnd uintptr, s string) {
	if hwnd == 0 {
		return
	}
	u, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	_, _, _ = procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(u)))
}

// monitor is one display: its device name, bounds and work area, and its DPI.
type monitor struct {
	device      string
	bounds, wrk rect
	dpi         int
}

// monitorInfo reads a monitor handle's MONITORINFOEXW and its effective DPI.
func monitorInfo(hmon uintptr) (monitor, bool) {
	var mi monitorInfoEx
	mi.size = uint32(unsafe.Sizeof(mi))
	if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return monitor{}, false
	}
	m := monitor{device: windows.UTF16ToString(mi.device[:]), bounds: mi.monitor.rect(), wrk: mi.work.rect(),
		dpi: baseDPI}
	if procGetDpiForMonitor.Find() == nil {
		var x, y uint32
		if r, _, _ := procGetDpiForMonitor.Call(hmon, mdtEffectiveDPI, uintptr(unsafe.Pointer(&x)),
			uintptr(unsafe.Pointer(&y))); r == 0 && x > 0 {
			m.dpi = int(x)
		}
	}
	return m, true
}

// monitors lists the displays (EnumDisplayMonitors).
func monitors() []monitor {
	var out []monitor
	cb := enumMonitorsCallback(func(hmon uintptr) {
		if m, ok := monitorInfo(hmon); ok {
			out = append(out, m)
		}
	})
	_, _, _ = procEnumDisplayMonitors.Call(0, 0, cb.ptr, 0)
	cb.done()
	return out
}

// enumMonitors is the one EnumDisplayMonitors callback of the process (NewCallback's are never freed); calls are
// serialized by enumMu, and fn is the current call's.
var (
	enumMu       sync.Mutex
	enumFn       func(uintptr)
	enumCallback = sync.OnceValue(func() uintptr {
		return windows.NewCallback(func(hmon, _, _, _ uintptr) uintptr {
			if enumFn != nil {
				enumFn(hmon)
			}
			return 1 // go on
		})
	})
)

type enumCall struct{ ptr uintptr }

func enumMonitorsCallback(fn func(uintptr)) enumCall {
	enumMu.Lock()
	enumFn = fn
	return enumCall{ptr: enumCallback()}
}

func (enumCall) done() {
	enumFn = nil
	enumMu.Unlock()
}

// targetMonitor is the monitor the pill goes on: the state's by device name, else the primary one.
func targetMonitor(device string) (monitor, bool) {
	if device != "" {
		for _, m := range monitors() {
			if m.device == device {
				return m, true
			}
		}
	}
	hmon, _, _ := procMonitorFromPoint.Call(0, monitorPrimary) // POINT{0, 0} packed: the primary monitor
	return monitorInfo(hmon)
}

// place sizes the pill for its monitor's DPI and puts it where it goes there (pillPlace).
func (p *pill) place() {
	m, ok := targetMonitor(p.state.Monitor)
	if !ok {
		return
	}
	p.device = m.device
	p.setDPI(m.dpi)
	w, h := pillSize(p.dpi, p.collapsed)
	x, y := pillPlace(p.state.Positions, m.device, m.bounds, m.wrk, w, h, p.dpi)
	_, _, _ = procSetWindowPos.Call(p.hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoActivate)
	// Moving to a monitor of another DPI sends WM_DPICHANGED, which sizes it again; the window's own DPI decides.
	if r, _, _ := procGetDpiForWindow.Call(p.hwnd); r != 0 && int(r) != p.dpi {
		p.setDPI(int(r))
		w, h = pillSize(p.dpi, p.collapsed)
		_, _, _ = procSetWindowPos.Call(p.hwnd, 0, 0, 0, uintptr(w), uintptr(h), swpNoMove|swpNoZOrder|swpNoActivate)
	}
	p.layout()
}

// reclamp keeps the pill inside the work area of the monitor it is on (WM_DISPLAYCHANGE).
func (p *pill) reclamp() {
	if p.hwnd == 0 || !p.shown {
		return
	}
	var wr winRect
	if r, _, _ := procGetWindowRect.Call(p.hwnd, uintptr(unsafe.Pointer(&wr))); r == 0 {
		return
	}
	hmon, _, _ := procMonitorFromWindow.Call(p.hwnd, monitorNearest)
	m, ok := monitorInfo(hmon)
	if !ok {
		return
	}
	r := wr.rect()
	x, y := clampTo(r.left, r.top, r.width(), r.height(), m.wrk)
	_, _, _ = procSetWindowPos.Call(p.hwnd, 0, uintptr(x), uintptr(y), 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
}

// setDPI takes a DPI and makes the fonts for it.
func (p *pill) setDPI(dpi int) {
	if dpi <= 0 {
		dpi = baseDPI
	}
	if dpi == p.dpi && p.font != 0 {
		return
	}
	p.dpi = dpi
	p.deleteFonts()
	face, _ := windows.UTF16PtrFromString("Segoe UI")
	mk := func(weight int) uintptr {
		f, _, _ := procCreateFontW.Call(uintptr(-scale(12, dpi)), 0, 0, 0, uintptr(weight), 0, 0, 0,
			defaultCharset, 0, 0, cleartypeQuality, 0, uintptr(unsafe.Pointer(face)))
		return f
	}
	p.font, p.bold = mk(fwNormal), mk(fwSemiBold)
}

func (p *pill) deleteFonts() {
	for _, f := range []*uintptr{&p.font, &p.bold} {
		if *f != 0 {
			_, _, _ = procDeleteObject.Call(*f)
			*f = 0
		}
	}
}

// layout moves the buttons to their places, or hides them while collapsed.
func (p *pill) layout() {
	if p.collapsed {
		_, _, _ = procShowWindow.Call(p.first, swHide)
		_, _, _ = procShowWindow.Call(p.stop, swHide)
		return
	}
	w, h := pillSize(p.dpi, false)
	l := layoutPill(w, h, p.dpi)
	for _, b := range []struct {
		hwnd uintptr
		r    rect
	}{{p.first, l.pause}, {p.stop, l.stop}} {
		_, _, _ = procSetWindowPos.Call(b.hwnd, 0, uintptr(b.r.left), uintptr(b.r.top), uintptr(b.r.width()),
			uintptr(b.r.height()), swpNoZOrder|swpNoActivate)
		_, _, _ = procShowWindow.Call(b.hwnd, swShowNoActivate)
	}
}

// toggleCollapsed collapses the pill to its dot or expands it, keeping its top left corner, and reports it.
func (p *pill) toggleCollapsed() {
	p.collapsed = !p.collapsed
	setText(p.hwnd, pillName(p.state.Text, p.collapsed))
	w, h := pillSize(p.dpi, p.collapsed)
	_, _, _ = procSetWindowPos.Call(p.hwnd, 0, 0, 0, uintptr(w), uintptr(h), swpNoMove|swpNoZOrder|swpNoActivate)
	p.layout()
	p.reclamp()
	_, _, _ = procInvalidateRect.Call(p.hwnd, 0, 1)
	kind := app.PillExpand
	if p.collapsed {
		kind = app.PillCollapse
	}
	p.act(app.PillAction{Kind: kind})
}

// moved reports the pill's new position on its monitor (WM_EXITSIZEMOVE): the app keeps it in pill.positions.
func (p *pill) moved() {
	var wr winRect
	if r, _, _ := procGetWindowRect.Call(p.hwnd, uintptr(unsafe.Pointer(&wr))); r == 0 {
		return
	}
	hmon, _, _ := procMonitorFromWindow.Call(p.hwnd, monitorNearest)
	m, ok := monitorInfo(hmon)
	if !ok {
		return
	}
	p.device = m.device
	p.act(app.PillAction{Kind: app.PillMoved, Monitor: m.device, X: int(wr.left), Y: int(wr.top)})
}

// pillColors are the pill's colors: its own, or the system colors under high contrast.
type pillColors struct {
	back, text, button, buttonPressed, buttonText, disabled, dot, paused, marker uint32
}

func rgb(r, g, b uint8) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

func currentColors() pillColors {
	hc := highContrast{}
	hc.size = uint32(unsafe.Sizeof(hc))
	if r, _, _ := procSystemParametersInfoW.Call(spiGetHighContrast, uintptr(hc.size), uintptr(unsafe.Pointer(&hc)),
		0); r != 0 && hc.flags&hcfHighContrastOn != 0 {
		sys := func(i int) uint32 {
			c, _, _ := procGetSysColor.Call(uintptr(i))
			return uint32(c) //nolint:gosec // G115: a COLORREF
		}
		return pillColors{back: sys(colorWindow), text: sys(colorWindowText), button: sys(colorBtnFace),
			buttonPressed: sys(colorHighlight), buttonText: sys(colorBtnText), disabled: sys(colorGrayText),
			dot: sys(colorHighlight), paused: sys(colorHighlight), marker: sys(colorHighlight)}
	}
	return pillColors{back: rgb(32, 32, 32), text: rgb(255, 255, 255), button: rgb(58, 58, 58),
		buttonPressed: rgb(80, 80, 80), buttonText: rgb(255, 255, 255), disabled: rgb(140, 140, 140),
		dot: rgb(232, 17, 35), paused: rgb(255, 185, 0), marker: rgb(255, 185, 0)}
}

// fill paints r with a solid color.
func fill(hdc uintptr, r rect, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	if br == 0 {
		return
	}
	wr := winRect{int32(r.left), int32(r.top), int32(r.right), int32(r.bottom)} //nolint:gosec // G115: pixels
	_, _, _ = procFillRect.Call(hdc, uintptr(unsafe.Pointer(&wr)), br)
	_, _, _ = procDeleteObject.Call(br)
}

// ellipse paints a filled circle in r.
func ellipse(hdc uintptr, r rect, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	if br == 0 {
		return
	}
	pen, _, _ := procGetStockObject.Call(nullPen)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	oldBrush, _, _ := procSelectObject.Call(hdc, br)
	_, _, _ = procEllipse.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom))
	_, _, _ = procSelectObject.Call(hdc, oldBrush)
	_, _, _ = procSelectObject.Call(hdc, oldPen)
	_, _, _ = procDeleteObject.Call(br)
}

// text draws s in r with flags.
func text(hdc uintptr, s string, r rect, font uintptr, color uint32, flags uintptr) {
	u, err := windows.UTF16FromString(s)
	if err != nil || len(u) <= 1 {
		return
	}
	old, _, _ := procSelectObject.Call(hdc, font)
	_, _, _ = procSetBkMode.Call(hdc, bkTransparent)
	_, _, _ = procSetTextColor.Call(hdc, uintptr(color))
	wr := winRect{int32(r.left), int32(r.top), int32(r.right), int32(r.bottom)} //nolint:gosec // G115: pixels
	_, _, _ = procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1),
		uintptr(unsafe.Pointer(&wr)), flags|dtNoPrefix)
	_, _, _ = procSelectObject.Call(hdc, old)
}

// paint draws the pill: the background, the dot (amber while paused), the marker and the line; collapsed, the dot
// alone.
func (p *pill) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(p.hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer func() { _, _, _ = procEndPaint.Call(p.hwnd, uintptr(unsafe.Pointer(&ps))) }()
	var cr winRect
	_, _, _ = procGetClientRect.Call(p.hwnd, uintptr(unsafe.Pointer(&cr)))
	c := currentColors()
	whole := cr.rect()
	fill(hdc, whole, c.back)
	dot := c.dot
	if p.state.Mode == app.PillPaused || p.state.Mode == app.PillLockPrompt {
		dot = c.paused
	}
	if p.collapsed {
		in := scale(5, p.dpi)
		ellipse(hdc, rect{whole.left + in, whole.top + in, whole.right - in, whole.bottom - in}, dot)
		return
	}
	l := layoutPill(whole.width(), whole.height(), p.dpi)
	ellipse(hdc, l.dot, dot)
	if p.state.Questions > 0 {
		ellipse(hdc, l.marker, c.marker)
	}
	text(hdc, p.state.Text.Line, l.label, p.font, c.text, dtSingleLine|dtVCenter|dtEndEllipsis)
}

// drawButton draws an owner-drawn button: its label (Pause, Resume or Stop), greyed while disabled.
func (p *pill) drawButton(di *drawItemStruct) {
	c := currentColors()
	r := di.item.rect()
	back, fore := c.button, c.buttonText
	if di.itemState&odsSelected != 0 {
		back = c.buttonPressed
	}
	if di.itemState&odsDisabled != 0 {
		fore = c.disabled
	}
	fill(di.hdc, r, back)
	label := p.state.Text.Stop
	if di.ctlID == idFirst {
		label, _, _ = pillButtons(p.state)
	}
	text(di.hdc, label, r, p.bold, fore, dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis)
	if di.itemState&odsFocus != 0 {
		in := scale(2, p.dpi)
		edge := rect{r.left + in, r.bottom - in*2, r.right - in, r.bottom - in}
		fill(di.hdc, edge, fore)
	}
}

// pillWndProc is the pill windows' procedure.
func pillWndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if v, ok := pills.Load(hwnd); ok {
		if r, handled := v.(*pill).handle(uint32(message), wParam, lParam); handled { //nolint:gosec // G115: a message id is 32 bits
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// handle is the pill's part of its window procedure: true when it handled the message.
func (p *pill) handle(message uint32, wParam, lParam uintptr) (uintptr, bool) {
	switch message {
	case wmNCHitTest:
		if p.collapsed {
			return htCaption, true
		}
		var wr winRect
		_, _, _ = procGetWindowRect.Call(p.hwnd, uintptr(unsafe.Pointer(&wr)))
		x := int(int16(lParam&0xFFFF)) - int(wr.left)      //nolint:gosec // G115: GET_X_LPARAM
		y := int(int16((lParam>>16)&0xFFFF)) - int(wr.top) //nolint:gosec // G115: GET_Y_LPARAM
		w, h := pillSize(p.dpi, false)
		if hitPill(layoutPill(w, h, p.dpi), x, y, p.state.Questions > 0) == hitMarker {
			return htClient, true
		}
		return htCaption, true // the drag handle
	case wmNCLButtonDblClk:
		p.toggleCollapsed()
		return 0, true
	case wmLButtonUp: // only the marker is client area
		p.act(app.PillAction{Kind: app.PillOpen})
		return 0, true
	case wmMouseActivate:
		return maNoActivate, true
	case wmExitSizeMove:
		p.moved()
		return 0, true
	case wmCommand:
		if (wParam>>16)&0xFFFF != bnClicked {
			return 0, false
		}
		switch wParam & 0xFFFF {
		case idFirst:
			p.act(app.PillAction{Kind: p.firstAct})
		case idStop:
			p.act(app.PillAction{Kind: app.PillStop})
		}
		return 0, true
	case wmDrawItem:
		p.drawButton(*(**drawItemStruct)(unsafe.Pointer(&lParam))) // the system's DRAWITEMSTRUCT
		return 1, true
	case wmPaint:
		p.paint()
		return 0, true
	case wmEraseBkgnd:
		return 1, true
	case wmDPIChanged:
		p.setDPI(int((wParam >> 16) & 0xFFFF))
		r := *(**winRect)(unsafe.Pointer(&lParam)) // the system's suggested RECT
		w, h := pillSize(p.dpi, p.collapsed)
		x, y := int(r.left), int(r.top)
		_, _, _ = procSetWindowPos.Call(p.hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
			swpNoZOrder|swpNoActivate)
		p.layout()
		_, _, _ = procInvalidateRect.Call(p.hwnd, 0, 1)
		return 0, true
	case wmDisplayChange:
		p.reclamp()
		return 0, false
	case wmSettingChange: // high contrast on or off
		_, _, _ = procInvalidateRect.Call(p.hwnd, 0, 1)
		return 0, false
	case wmClose: // it can be collapsed, not closed (03 §11.4)
		return 0, true
	}
	return 0, false
}
