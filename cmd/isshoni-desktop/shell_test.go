//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
)

// TestTrayIcons: every icon of internal/desktop/tray is embedded as an ICO with PNG images of 16, 20, 24 and 32 px
// (03 §4.5's build/tray), and an unknown id falls back to the idle icon.
func TestTrayIcons(t *testing.T) {
	for _, id := range tray.Icons {
		data := trayIcon(id)
		if len(data) < 6 || binary.LittleEndian.Uint16(data[2:]) != 1 {
			t.Fatalf("%s: not an ICO", id)
		}
		n := int(binary.LittleEndian.Uint16(data[4:]))
		var sizes []int
		for i := range n {
			e := data[6+16*i:]
			size, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
			img, err := png.Decode(bytes.NewReader(data[off : off+size]))
			if err != nil {
				t.Fatalf("%s image %d: %v", id, i, err)
			}
			if w := img.Bounds().Dx(); w != int(e[0]) || img.Bounds().Dy() != int(e[1]) {
				t.Errorf("%s image %d is %v, its entry says %d", id, i, img.Bounds(), e[0])
			}
			sizes = append(sizes, int(e[0]))
		}
		if !slices.Equal(sizes, []int{16, 20, 24, 32}) {
			t.Errorf("%s: sizes %v", id, sizes)
		}
	}
	if !bytes.Equal(trayIcon("tray-none"), trayIcon(tray.IconIdle)) {
		t.Error("an unknown icon isn't the idle one")
	}
}

// TestTrayMenu (W18's Go half): the view's items in order, their labels and whether they are enabled, separators
// included; a share label's "&" stays an ampersand in a Windows menu.
func TestTrayMenu(t *testing.T) {
	v := tray.Render(tray.State{Mode: tray.Live, Source: "Tom & Jerry", Watching: 2}, nil)
	m := trayMenu(v, func(tray.Action) {})
	items := 0
	for i, want := range v.Menu {
		mi := m.ItemAt(i)
		if mi == nil {
			t.Fatalf("item %d missing", i)
		}
		items++
		if want.Separator {
			continue
		}
		if mi.Enabled() != want.Enabled {
			t.Errorf("item %d (%q): enabled %v", i, want.Label, mi.Enabled())
		}
		if mi.Label() != menuLabel(want.Label) {
			t.Errorf("item %d: label %q, want %q", i, mi.Label(), menuLabel(want.Label))
		}
	}
	if m.ItemAt(items) != nil {
		t.Error("more items than the view has")
	}
	if want := "Tom && Jerry"; runtime.GOOS == "windows" && menuLabel("Tom & Jerry") != want {
		t.Errorf("menuLabel = %q", menuLabel("Tom & Jerry"))
	}
	if runtime.GOOS != "windows" && menuLabel("Tom & Jerry") != "Tom & Jerry" {
		t.Error("menuLabel changed a label outside Windows")
	}
}

// TestActivation: Wails' response → the app's nonce and action; the body is open.
func TestActivation(t *testing.T) {
	for _, c := range []struct {
		id     string
		info   map[string]any
		nonce  string
		action string
	}{
		{"DEFAULT_ACTION", map[string]any{"nonce": "ab"}, "ab", app.ToastOpen},
		{"", nil, "", app.ToastOpen},
		{app.ToastResume, map[string]any{"nonce": "cd"}, "cd", app.ToastResume},
		{app.ToastStop, map[string]any{"nonce": 7}, "", app.ToastStop},
	} {
		n, a := activation(c.id, c.info)
		if n != c.nonce || a != c.action {
			t.Errorf("activation(%q, %v) = %q %q", c.id, c.info, n, a)
		}
	}
	if got := toastCategory(app.ToastLockPaused); got != "io.isshoni.desktop.lock-paused" {
		t.Errorf("category %q", got)
	}
}

// TestEmbeddingOpen (03 §13.4 step 1): -Embedding whose activation is open continues as a normal start without the
// flag; any other activation, or none, exits 0 before anything else.
func TestEmbeddingOpen(t *testing.T) {
	for _, open := range []bool{false, true} {
		r := &startRun{}
		s := newStarter(r)
		waited := false
		s.embedding = func() bool { waited = true; return open }
		var args []string
		s.runApp = func(p app.Args, a []string, _ app.Pending, _ *forwarded, _ instance) int {
			r.rec = append(r.rec, "app")
			args = a
			return 0
		}
		if code := s.start([]string{"-Embedding"}); code != exitOK || !waited {
			t.Fatalf("open %v: exit %d, waited %v", open, code, waited)
		}
		ran := slices.Contains(r.rec, "app")
		if ran != open {
			t.Fatalf("open %v: the app ran %v (%v)", open, ran, r.rec)
		}
		if open && slices.ContainsFunc(args, func(a string) bool { return strings.EqualFold(a, "-embedding") }) {
			t.Errorf("the app got -Embedding: %v", args)
		}
	}
}
