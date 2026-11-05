package tray

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/desktop/i18n"
	"github.com/MoonWX/isshoni/web"
)

func catalog(t *testing.T) *i18n.Catalog {
	t.Helper()
	c, err := i18n.Load(web.DesktopCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// labels is the menu as text: "-" for a separator, "(x)" for a disabled line, "*x" for the bold one.
func labels(v View) []string {
	var out []string
	for _, m := range v.Menu {
		switch {
		case m.Separator:
			out = append(out, "-")
		case !m.Enabled:
			out = append(out, "("+m.Label+")")
		case m.Bold:
			out = append(out, "*"+m.Label)
		default:
			out = append(out, m.Label)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var registered = map[HotkeyID]string{HotkeyPause: "Ctrl+Alt+Shift+F9", HotkeyStop: "Ctrl+Alt+Shift+F10"}

// TestRender is 03 §11.1's table, one row per mode, with the hotkeys as registered.
func TestRender(t *testing.T) {
	c := catalog(t)
	cases := []struct {
		name    string
		s       State
		icon    IconID
		tooltip string
		menu    []string
	}{
		{"idle", State{Mode: Idle}, IconIdle, "isshoni", []string{"*Open isshoni", "Quit"}},
		{"idle with an update", State{Mode: Idle, UpdateReady: "0.3.0"}, IconIdle, "isshoni",
			[]string{"*Open isshoni", "Restart and update to 0.3.0", "Quit"}},
		{"zero mode is idle", State{}, IconIdle, "isshoni", []string{"*Open isshoni", "Quit"}},
		{"live", State{Mode: Live, Source: "Minecraft", Watching: 3, Hotkeys: registered}, IconLive,
			"Sharing Minecraft · 3 watching", []string{"(Sharing Minecraft · 3 watching)",
				"Pause sharing (Ctrl+Alt+Shift+F9)", "Stop sharing (Ctrl+Alt+Shift+F10)", "-", "Open isshoni",
				"Stop sharing and quit"}},
		{"live without hotkeys", State{Mode: Live, Source: "Minecraft", Watching: 1}, IconLive,
			"Sharing Minecraft · 1 watching", []string{"(Sharing Minecraft · 1 watching)", "Pause sharing",
				"Stop sharing", "-", "Open isshoni", "Stop sharing and quit"}},
		{"live, update ready: no restart", State{Mode: Live, Source: "x", UpdateReady: "0.3.0"}, IconLive,
			"Sharing x · 0 watching", []string{"(Sharing x · 0 watching)", "Pause sharing", "Stop sharing", "-",
				"Open isshoni", "Stop sharing and quit"}},
		{"live, no share.pause", State{Mode: Live, Source: "x", Watching: 2, PauseUnavailable: true}, IconLive,
			"Sharing x · 2 watching", []string{"(Sharing x · 2 watching)", "(Pause needs a newer server)",
				"Stop sharing", "-", "Open isshoni", "Stop sharing and quit"}},
		{"paused", State{Mode: Paused, Source: "Minecraft", Watching: 3, Hotkeys: registered}, IconPaused,
			"Sharing paused · 3 watching", []string{"(Sharing paused · 3 watching)",
				"Resume sharing (Ctrl+Alt+Shift+F9)", "Stop sharing (Ctrl+Alt+Shift+F10)", "-", "Open isshoni",
				"Stop sharing and quit"}},
		{"lock prompt", State{Mode: Paused, Watching: 3, LockPrompt: true}, IconPaused,
			"Sharing paused since you locked your PC · Resume sharing?",
			[]string{"(Sharing paused since you locked your PC · Resume sharing?)", "*Resume sharing",
				"Stop sharing", "-", "Open isshoni", "Stop sharing and quit"}},
		{"reconnecting", State{Mode: Reconnecting, Source: "x", Watching: 3}, IconWarn,
			"Sharing · reconnecting…", []string{"(Sharing · reconnecting…)", "Pause sharing", "Stop sharing", "-",
				"Open isshoni", "Stop sharing and quit"}},
		{"voice apps heard", State{Mode: Live, Source: "x", Watching: 1, VoiceAppsHeard: true}, IconWarn,
			"Friends hear voice apps · Sharing x · 1 watching", []string{"(Friends hear voice apps)",
				"(Sharing x · 1 watching)", "Pause sharing", "Stop sharing", "-", "Open isshoni",
				"Stop sharing and quit"}},
		{"voice apps heard, one question", State{Mode: Paused, Watching: 1, VoiceAppsHeard: true, Questions: 1},
			IconWarn, "Friends hear voice apps · Sharing paused · 1 watching", []string{"(Friends hear voice apps)",
				"1 question waiting", "(Sharing paused · 1 watching)", "Resume sharing", "Stop sharing", "-",
				"Open isshoni", "Stop sharing and quit"}},
		{"two questions", State{Mode: Live, Source: "x", Questions: 2}, IconLive, "Sharing x · 0 watching",
			[]string{"(Sharing x · 0 watching)", "2 questions waiting", "Pause sharing", "Stop sharing", "-",
				"Open isshoni", "Stop sharing and quit"}},
		{"idle ignores the alert", State{Mode: Idle, VoiceAppsHeard: true, Questions: 2}, IconIdle, "isshoni",
			[]string{"*Open isshoni", "Quit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Render(tc.s, c)
			if v.Icon != tc.icon {
				t.Errorf("icon %q, want %q", v.Icon, tc.icon)
			}
			if v.Tooltip != tc.tooltip {
				t.Errorf("tooltip %q, want %q", v.Tooltip, tc.tooltip)
			}
			if got := labels(v); !equal(got, tc.menu) {
				t.Errorf("menu\n%q\nwant\n%q", got, tc.menu)
			}
		})
	}
}

// TestActions: each enabled item has its action, disabled lines and separators none, and "Restart and update"
// appears only in Idle.
func TestActions(t *testing.T) {
	c := catalog(t)
	want := map[Mode][]Action{
		Idle:         {ActionOpen, ActionRestart, ActionQuit},
		Live:         {ActionNone, ActionPause, ActionStop, ActionNone, ActionOpen, ActionStopAndQuit},
		Paused:       {ActionNone, ActionResume, ActionStop, ActionNone, ActionOpen, ActionStopAndQuit},
		Reconnecting: {ActionNone, ActionPause, ActionStop, ActionNone, ActionOpen, ActionStopAndQuit},
	}
	for mode, actions := range want {
		v := Render(State{Mode: mode, UpdateReady: "0.3.0"}, c)
		if len(v.Menu) != len(actions) {
			t.Fatalf("%s: %d items, want %d", mode, len(v.Menu), len(actions))
		}
		for i, m := range v.Menu {
			if m.Action != actions[i] {
				t.Errorf("%s item %d (%q): action %q, want %q", mode, i, m.Label, m.Action, actions[i])
			}
			if (m.Action != ActionNone) != m.Enabled {
				t.Errorf("%s item %d (%q): enabled %v with action %q", mode, i, m.Label, m.Enabled, m.Action)
			}
			if m.Action == ActionRestart && mode != Idle {
				t.Errorf("%s offers Restart and update", mode)
			}
		}
	}
}

// TestTooltipTruncation: the source is cut to 40 characters with "…", and the tooltip to 127 UTF-16 units, never
// in a surrogate pair; the menu line shows the same cut source.
func TestTooltipTruncation(t *testing.T) {
	c := catalog(t)
	long := strings.Repeat("Minecraft ", 10) // 100 characters
	v := Render(State{Mode: Live, Source: long, Watching: 3}, c)
	cut := Cut(long, MaxSource)
	if utf8.RuneCountInString(cut) != MaxSource || !strings.HasSuffix(cut, "…") {
		t.Fatalf("Cut = %q", cut)
	}
	if want := "Sharing " + cut + " · 3 watching"; v.Tooltip != want || v.Menu[0].Label != want {
		t.Errorf("tooltip %q, first line %q, want %q", v.Tooltip, v.Menu[0].Label, want)
	}
	if Cut("Minecraft", MaxSource) != "Minecraft" || Cut("", MaxSource) != "" {
		t.Error("a short source changed")
	}
	if got := Cut("abcdef", 0); got != "" {
		t.Errorf("Cut(_, 0) = %q", got)
	}
	if got := Cut("\xff\xfeab", 40); !utf8.ValidString(got) {
		t.Errorf("Cut kept invalid UTF-8: %q", got)
	}

	// The tooltip's own bound: with the alert in front, a long source and a big count it still fits.
	v = Render(State{Mode: Live, Source: long, Watching: 1234567, VoiceAppsHeard: true}, c)
	if n := len(utf16.Encode([]rune(v.Tooltip))); n > MaxTooltip {
		t.Errorf("tooltip is %d UTF-16 units: %q", n, v.Tooltip)
	}
	for _, s := range []string{strings.Repeat("a", 200), strings.Repeat("😀", 100), strings.Repeat("é😀", 70)} {
		got := cutUTF16(s, MaxTooltip)
		n := len(utf16.Encode([]rune(got)))
		if n > MaxTooltip || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
			t.Errorf("cutUTF16(%d runes) = %d units, %q", utf8.RuneCountInString(s), n, got)
		}
	}
	if got := cutUTF16("isshoni", MaxTooltip); got != "isshoni" {
		t.Errorf("cutUTF16 changed a short text: %q", got)
	}
}

// TestEqual: two renders of the same state are equal; another state isn't.
func TestEqual(t *testing.T) {
	c := catalog(t)
	a := Render(State{Mode: Live, Source: "x", Watching: 1}, c)
	b := Render(State{Mode: Live, Source: "x", Watching: 1}, c)
	if !a.Equal(b) {
		t.Error("equal states render differently")
	}
	if a.Equal(Render(State{Mode: Live, Source: "x", Watching: 2}, c)) {
		t.Error("a watcher more is the same view")
	}
	if a.Equal(Render(State{Mode: Idle}, c)) {
		t.Error("idle is the same view as live")
	}
}

// TestNilCatalog: without a catalog the keys stand in, and nothing panics.
func TestNilCatalog(t *testing.T) {
	v := Render(State{Mode: Paused, LockPrompt: true}, nil)
	if v.Tooltip != "desktop.native.tray.tooltip.lockPrompt" {
		t.Errorf("tooltip %q", v.Tooltip)
	}
}

func FuzzRender(f *testing.F) {
	c, err := i18n.Load(web.DesktopCatalog(), nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Add("live", "Minecraft", 3, false, 0)
	f.Add("paused", strings.Repeat("😀", 80), 99999, true, 5)
	f.Fuzz(func(t *testing.T, mode, source string, watching int, alert bool, questions int) {
		v := Render(State{Mode: Mode(mode), Source: source, Watching: watching, VoiceAppsHeard: alert,
			Questions: questions}, c)
		if n := len(utf16.Encode([]rune(v.Tooltip))); n > MaxTooltip || !utf8.ValidString(v.Tooltip) {
			t.Fatalf("tooltip of %d units: %q", n, v.Tooltip)
		}
		if len(v.Menu) == 0 || v.Icon == "" {
			t.Fatal("empty view")
		}
		for _, m := range v.Menu {
			if !utf8.ValidString(m.Label) {
				t.Fatalf("invalid label %q", m.Label)
			}
		}
	})
}
