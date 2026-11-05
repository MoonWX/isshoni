package winos

import "github.com/MoonWX/isshoni/internal/desktop/app"

// The Sharing pill's geometry (03 §11.4), in logical pixels at 100 % (96 DPI); the Windows pill (pill_windows.go)
// scales them by the window's DPI. Pure, so it is tested on every OS.
const (
	pillWidth     = 260 // about 260 × 36 at 100 %
	pillHeight    = 36
	pillCollapsed = 24 // the collapsed pill: a red dot
	pillTop       = 8  // the default position: top center of the monitor, this far below its top
	pillPad       = 8  // the padding inside the pill
	pillDot       = 10 // the red dot's diameter
	pillMarker    = 8  // the amber marker's diameter (a mic prompt waits)
	pillButtonW   = 64 // each of the two buttons
	pillButtonH   = 24
	baseDPI       = 96
)

// rect is a screen rectangle: left, top, right and bottom, right and bottom exclusive (RECT's meaning).
type rect struct{ left, top, right, bottom int }

func (r rect) width() int  { return r.right - r.left }
func (r rect) height() int { return r.bottom - r.top }

// contains reports whether r holds all of o.
func (r rect) contains(o rect) bool {
	return o.left >= r.left && o.top >= r.top && o.right <= r.right && o.bottom <= r.bottom
}

// scale is v logical pixels at dpi (rounded half away from zero, as MulDiv does).
func scale(v, dpi int) int {
	if dpi <= 0 {
		dpi = baseDPI
	}
	n := v * dpi
	if n >= 0 {
		return (n + baseDPI/2) / baseDPI
	}
	return (n - baseDPI/2) / baseDPI
}

// pillSize is the pill's size in physical pixels at dpi: about 260 × 36 at 100 %, or the collapsed 24 px dot.
func pillSize(dpi int, collapsed bool) (w, h int) {
	if collapsed {
		return scale(pillCollapsed, dpi), scale(pillCollapsed, dpi)
	}
	return scale(pillWidth, dpi), scale(pillHeight, dpi)
}

// clampTo moves a w × h rectangle at (x, y) into work as little as it can; a rectangle larger than work keeps its
// top left corner in it.
func clampTo(x, y, w, h int, work rect) (int, int) {
	if x+w > work.right {
		x = work.right - w
	}
	if y+h > work.bottom {
		y = work.bottom - h
	}
	if x < work.left {
		x = work.left
	}
	if y < work.top {
		y = work.top
	}
	return x, y
}

// pillPlace is where a w × h pill goes on a monitor whose device name is device (03 §11.4): the last position kept
// for that name in saved (settings' pill.positions), else the top center of the monitor, pillTop below its top;
// either is clamped to the monitor's work area (the taskbar's side excluded).
func pillPlace(saved map[string]app.PillPoint, device string, monitor, work rect, w, h, dpi int) (int, int) {
	if p, ok := saved[device]; ok && device != "" {
		return clampTo(p.X, p.Y, w, h, work)
	}
	x := monitor.left + (monitor.width()-w)/2
	y := monitor.top + scale(pillTop, dpi)
	return clampTo(x, y, w, h, work)
}

// pillLayout is where the parts of an expanded pill of width w and height h go at dpi: the dot, the marker (beside
// the dot), the label, and the two buttons at the right end.
type pillLayout struct {
	dot, marker, label, pause, stop rect
}

func layoutPill(w, h, dpi int) pillLayout {
	pad, dot, marker := scale(pillPad, dpi), scale(pillDot, dpi), scale(pillMarker, dpi)
	bw, bh := scale(pillButtonW, dpi), scale(pillButtonH, dpi)
	gap := scale(4, dpi)
	var l pillLayout
	l.dot = rect{pad, (h - dot) / 2, pad + dot, (h-dot)/2 + dot}
	l.marker = rect{l.dot.right + gap, (h - marker) / 2, l.dot.right + gap + marker, (h-marker)/2 + marker}
	l.stop = rect{w - pad - bw, (h - bh) / 2, w - pad, (h-bh)/2 + bh}
	l.pause = rect{l.stop.left - gap - bw, l.stop.top, l.stop.left - gap, l.stop.bottom}
	l.label = rect{l.marker.right + gap, 0, l.pause.left - gap, h}
	return l
}

// pillHit is what a point in an expanded pill's client area is: the marker (a click opens the window on the mic
// prompt card) while questions wait, else the drag handle (03 §11.4: the label area is a drag handle).
type pillHit int

const (
	hitDrag pillHit = iota
	hitMarker
)

func hitPill(l pillLayout, x, y int, questions bool) pillHit {
	if questions && x >= l.marker.left && x < l.marker.right && y >= l.marker.top && y < l.marker.bottom {
		return hitMarker
	}
	return hitDrag
}

// pillButtons is what the two buttons say and do for s: Pause or Resume (Resume while paused or asking), then Stop.
// pauseEnabled is false on a server without share.pause.
func pillButtons(s app.PillState) (first string, firstAct app.PillActionKind, pauseEnabled bool) {
	switch s.Mode {
	case app.PillPaused, app.PillLockPrompt:
		return s.Text.Resume, app.PillResume, true
	}
	return s.Text.Pause, app.PillPause, !s.PauseUnavailable
}

// pillName is the pill window's name, which Narrator reads: the line and the marker's name ("Sharing · 3 watching ·
// 1 question waiting"), or the collapsed dot's name while collapsed (its buttons are hidden then).
func pillName(t app.PillText, collapsed bool) string {
	if collapsed {
		return t.Collapsed
	}
	if t.Questions != "" {
		return t.Line + " · " + t.Questions
	}
	return t.Line
}
