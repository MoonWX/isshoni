package winos

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// TestPillSize: about 260 × 36 at 100 %, scaled by the DPI; the collapsed dot is 24 px.
func TestPillSize(t *testing.T) {
	cases := []struct {
		dpi       int
		collapsed bool
		w, h      int
	}{
		{96, false, 260, 36},
		{120, false, 325, 45},
		{144, false, 390, 54},
		{192, false, 520, 72},
		{0, false, 260, 36}, // unknown: 100 %
		{96, true, 24, 24},
		{192, true, 48, 48},
	}
	for _, c := range cases {
		if w, h := pillSize(c.dpi, c.collapsed); w != c.w || h != c.h {
			t.Errorf("pillSize(%d, %v) = %d × %d, want %d × %d", c.dpi, c.collapsed, w, h, c.w, c.h)
		}
	}
	if got := scale(-3, 144); got != -5 {
		t.Errorf("scale(-3, 144) = %d", got)
	}
}

// TestPillPlace: the top center of the monitor, 8 px (scaled) below its top, unless a position was kept for the
// monitor; either is clamped to the work area, on monitors left of and above the primary too.
func TestPillPlace(t *testing.T) {
	primary := rect{0, 0, 1920, 1080}
	primaryWork := rect{0, 0, 1920, 1032} // the taskbar at the bottom
	left := rect{-2560, -360, 0, 1080}
	leftWork := rect{-2560, -312, 0, 1080} // a taskbar at the top
	saved := map[string]app.PillPoint{
		`\\.\DISPLAY2`: {X: -100, Y: 500},  // partly off its monitor: clamped
		`\\.\DISPLAY3`: {X: 700, Y: 20},    // kept as is
		`\\.\DISPLAY4`: {X: 5000, Y: 5000}, // far away: the bottom right corner
	}
	cases := []struct {
		name          string
		device        string
		mon, work     rect
		dpi           int
		wantX, wantY  int
		wantW, wantH  int
		collapsedPill bool
	}{
		{"default on the primary", `\\.\DISPLAY1`, primary, primaryWork, 96, 830, 8, 260, 36, false},
		{"default at 150 %", `\\.\DISPLAY1`, primary, primaryWork, 144, 765, 12, 390, 54, false},
		{"default under a top taskbar", `\\.\DISPLAY9`, left, leftWork, 96, -1410, -312, 260, 36, false},
		{"saved, clamped into its monitor", `\\.\DISPLAY2`, left, leftWork, 96, -260, 500, 260, 36, false},
		{"saved", `\\.\DISPLAY3`, primary, primaryWork, 96, 700, 20, 260, 36, false},
		{"saved far away", `\\.\DISPLAY4`, primary, primaryWork, 96, 1660, 996, 260, 36, false},
		{"collapsed", `\\.\DISPLAY4`, primary, primaryWork, 96, 1896, 1008, 24, 24, true},
		{"no device name: default", "", primary, primaryWork, 96, 830, 8, 260, 36, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := pillSize(c.dpi, c.collapsedPill)
			if w != c.wantW || h != c.wantH {
				t.Fatalf("size %d × %d", w, h)
			}
			x, y := pillPlace(saved, c.device, c.mon, c.work, w, h, c.dpi)
			if x != c.wantX || y != c.wantY {
				t.Fatalf("at (%d, %d), want (%d, %d)", x, y, c.wantX, c.wantY)
			}
			if !c.work.contains(rect{x, y, x + w, y + h}) {
				t.Fatalf("(%d, %d) %d × %d is outside the work area %+v", x, y, w, h, c.work)
			}
		})
	}
	// Larger than the work area: the top left corner stays in it.
	if x, y := clampTo(-50, -50, 400, 400, rect{0, 0, 300, 300}); x != 0 || y != 0 {
		t.Errorf("clampTo of an oversized rectangle = (%d, %d)", x, y)
	}
}

// TestPillLayout: the dot, the marker beside it, the label, then Pause and Stop at the right end, inside the pill at
// every DPI; a click hits the marker only while a question waits.
func TestPillLayout(t *testing.T) {
	for _, dpi := range []int{96, 120, 144, 192} {
		w, h := pillSize(dpi, false)
		l := layoutPill(w, h, dpi)
		whole := rect{0, 0, w, h}
		for name, r := range map[string]rect{"dot": l.dot, "marker": l.marker, "label": l.label, "pause": l.pause,
			"stop": l.stop} {
			if !whole.contains(r) || r.width() <= 0 || r.height() <= 0 {
				t.Errorf("dpi %d: %s %+v outside %d × %d", dpi, name, r, w, h)
			}
		}
		if l.dot.right > l.marker.left || l.marker.right > l.label.left || l.label.right > l.pause.left ||
			l.pause.right > l.stop.left {
			t.Errorf("dpi %d: overlapping parts %+v", dpi, l)
		}
		mx, my := (l.marker.left+l.marker.right)/2, (l.marker.top+l.marker.bottom)/2
		if hitPill(l, mx, my, true) != hitMarker || hitPill(l, mx, my, false) != hitDrag ||
			hitPill(l, (l.label.left+l.label.right)/2, my, true) != hitDrag {
			t.Errorf("dpi %d: hit test", dpi)
		}
	}
}

// TestPillButtons: Pause (disabled on a server without share.pause) or Resume while paused or asking, by the
// state's texts.
func TestPillButtons(t *testing.T) {
	text := app.PillText{Pause: "Pause", Resume: "Resume", Stop: "Stop"}
	cases := []struct {
		s       app.PillState
		label   string
		act     app.PillActionKind
		enabled bool
	}{
		{app.PillState{Mode: app.PillLive, Text: text}, "Pause", app.PillPause, true},
		{app.PillState{Mode: app.PillReconnecting, Text: text}, "Pause", app.PillPause, true},
		{app.PillState{Mode: app.PillLive, Text: text, PauseUnavailable: true}, "Pause", app.PillPause, false},
		{app.PillState{Mode: app.PillPaused, Text: text}, "Resume", app.PillResume, true},
		{app.PillState{Mode: app.PillLockPrompt, Text: text}, "Resume", app.PillResume, true},
	}
	for _, c := range cases {
		label, act, enabled := pillButtons(c.s)
		if label != c.label || act != c.act || enabled != c.enabled {
			t.Errorf("%s: %q %q %v", c.s.Mode, label, act, enabled)
		}
	}
}
