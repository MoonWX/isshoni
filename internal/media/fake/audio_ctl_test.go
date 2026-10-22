package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// scenarioNames are 05 §13.3's scenarios.
var scenarioNames = []string{"normal", "degraded", "degrade-live", "old-windows", "no-device", "fast-path",
	"shared-mic", "many"}

// desktopExclusions is the kind of rule set the desktop app's service builds (05 §14.3), for the scripted apps: the
// built-in voice apps, launchers, the shared runtime, the browsers, isshoni itself, mic users kept out and the fast
// path allowed.
func desktopExclusions() media.Exclusions {
	return media.Exclusions{KeepOutMicUsers: true, FastPath: true, Rules: []media.Rule{
		{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: "discord.exe", AppID: "discord", Name: "Discord"},
		{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: "Teamspeak.exe", AppID: "teamspeak",
			Name: "TeamSpeak"},
		{Kind: media.RuleStop, Source: media.SourceData, Value: "steam.exe"},
		{Kind: media.RuleStop, Source: media.SourceData, Value: "explorer.exe"},
		{Kind: media.RuleSharedRuntime, Source: media.SourceData, Value: "msedgewebview2.exe"},
		{Kind: media.RuleBrowser, Source: media.SourceData, Value: "chrome.exe"},
		{Kind: media.RuleBrowser, Source: media.SourceData, Value: "msedge.exe"},
		{Kind: media.RuleInstance, Source: media.SourceSelf, Value: "4", AppID: "isshoni", Name: "isshoni"},
		{Kind: media.RuleWebViewDataDir, Source: media.SourceSelf, Value: `C:\isshoni-test\WebView2`},
	}}
}

// with returns ex plus rules.
func with(ex media.Exclusions, rules ...media.Rule) media.Exclusions {
	ex.Rules = append(slices.Clone(ex.Rules), rules...)
	return ex
}

// audioConfig starts the listed monitor with audio under ex.
func audioConfig(ex media.Exclusions) media.Config {
	cfg := testConfig()
	cfg.Audio = &media.AudioConfig{Exclusions: ex}
	return cfg
}

// mustWatch starts the watcher.
func mustWatch(t *testing.T, e *Engine, ex media.Exclusions) {
	t.Helper()
	if err := e.WatchAudio(t.Context(), ex); err != nil {
		t.Fatalf("WatchAudio: %v", err)
	}
}

// mustReport returns the report and checks that it is a valid v1 report: encoding/json writes it in a form that
// media.ParseAudioReport reads back unchanged, and its sessions agree with its rows.
func mustReport(t *testing.T, e *Engine) media.AudioReport {
	t.Helper()
	r, err := e.AudioReport(t.Context())
	if err != nil {
		t.Fatalf("AudioReport: %v", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := media.ParseAudioReport(b)
	if err != nil || !reflect.DeepEqual(back, r) {
		t.Fatalf("the report does not read back: %v\n%s", err, b)
	}
	if bytes.Contains(b, []byte("null")) {
		t.Errorf("a list is null: %s", b)
	}
	for _, s := range r.Sessions {
		a := row(r, s.AppID)
		if a.ID == "" || a.Verdict != s.Verdict || a.Reason != s.Reason || a.Exe != s.Exe {
			t.Errorf("session of pid %d disagrees with its row %+v", s.PID, a)
		}
	}
	return r
}

// mustLevels returns the levels.
func mustLevels(t *testing.T, e *Engine) media.AudioLevels {
	t.Helper()
	lv, err := e.AudioLevels(t.Context())
	if err != nil {
		t.Fatalf("AudioLevels: %v", err)
	}
	return lv
}

// row returns r's row with id, or a zero row.
func row(r media.AudioReport, id string) media.AudioApp {
	for _, a := range r.Apps {
		if a.ID == id {
			return a
		}
	}
	return media.AudioApp{}
}

// verdict is a row's "verdict/reason", or "-" without the row.
func verdict(r media.AudioReport, id string) string {
	a := row(r, id)
	if a.ID == "" {
		return "-"
	}
	return a.Verdict + "/" + a.Reason
}

// ids lists r's row ids.
func ids(r media.AudioReport) []string {
	var out []string
	for _, a := range r.Apps {
		out = append(out, a.ID)
	}
	return out
}

// level returns lv's entry for id; ok is false without one.
func level(lv media.AudioLevels, id string) (media.AudioLevel, bool) {
	for _, l := range lv.Apps {
		if l.AppID == id {
			return l, true
		}
	}
	return media.AudioLevel{}, false
}

// near reports whether a dB value is within 0.001 dB of want.
func near(got float32, want float64) bool {
	return math.Abs(float64(got)-want) < 1e-3
}

// events returns the codes of r's events with their app, in order, from index from.
func eventsOf(r media.AudioReport) []string {
	var out []string
	for _, ev := range r.Events {
		s := ev.Code
		if ev.App != "" {
			s += " " + ev.App
		}
		if ev.Detail != "" {
			s += " (" + ev.Detail + ")"
		}
		out = append(out, s)
	}
	return out
}

// eventAt returns the engine-clock time of r's last event with code (−1 without one).
func eventAt(r media.AudioReport, code string) int64 {
	for i := len(r.Events) - 1; i >= 0; i-- {
		if r.Events[i].Code == code {
			return r.Events[i].TNanos
		}
	}
	return -1
}

// TestAudioScenarioNames: every scenario of 05 §13.3 exists, "" is "normal", and an unknown name makes every
// AudioController call invalid.
func TestAudioScenarioNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, name := range scenarioNames {
			e := NewEngine(EngineConfig{AudioScenario: name})
			mustWatch(t, e, desktopExclusions())
			if r := mustReport(t, e); r.State == stateStopped || len(r.Apps) == 0 {
				t.Errorf("%s: %+v", name, r)
			}
			_ = e.Close()
		}
		def, normal := NewEngine(EngineConfig{}), NewEngine(EngineConfig{AudioScenario: "normal"})
		defer def.Close()
		defer normal.Close()
		mustWatch(t, def, desktopExclusions())
		mustWatch(t, normal, desktopExclusions())
		time.Sleep(6 * time.Second)
		if a, b := mustReport(t, def), mustReport(t, normal); !reflect.DeepEqual(a, b) {
			t.Errorf("the default scenario\n%+v\nis not normal's\n%+v", a, b)
		}

		bad := NewEngine(EngineConfig{AudioScenario: "Normal"})
		defer bad.Close()
		ctx := t.Context()
		_, err1 := bad.AudioReport(ctx)
		_, err2 := bad.AudioLevels(ctx)
		_, err3 := bad.SoundCheck(ctx, 0, 3*time.Second)
		for _, err := range []error{
			bad.WatchAudio(ctx, media.Exclusions{}), bad.UnwatchAudio(ctx), bad.SetExclusions(ctx, media.Exclusions{}),
			bad.SetAudioMode(ctx, media.AudioOff), bad.SetAudioMuted(ctx, true), err1, err2, err3,
		} {
			if !errors.Is(err, media.ErrInvalid) || !strings.Contains(err.Error(), `"Normal"`) {
				t.Errorf("with an unknown scenario: %v", err)
			}
		}
		// Sessions still run with audio.
		s := mustStart(t, bad, testConfig())
		if n := len(ofKind(pullFor(t, s, 100*time.Millisecond), media.KindAudio)); n == 0 {
			t.Error("no audio")
		}
	})
}

// TestAudioScenarioApps: in every scenario a process belongs to one app (no two apps' pids overlap), an app's
// sessions are among its processes, and exes are unique, since the events key rows by their exe.
func TestAudioScenarioApps(t *testing.T) {
	for _, name := range scenarioNames {
		apps := audioScenarios[name]().apps
		exes := map[string]bool{}
		for i, a := range apps {
			if exe := strings.ToLower(a.exe); exes[exe] {
				t.Errorf("%s: exe %s twice", name, a.exe)
			} else {
				exes[exe] = true
			}
			if a.sessions > max(a.pids, 1) {
				t.Errorf("%s: %s has %d sessions in %d processes", name, a.exe, a.sessions, a.pids)
			}
			for k := range max(a.pids, 1) {
				pid := uint64(a.pid) + 4*uint64(k)
				if !a.hasPID(pid) {
					t.Errorf("%s: %s does not own its pid %d", name, a.exe, pid)
				}
				for _, b := range apps[i+1:] {
					if b.hasPID(pid) {
						t.Errorf("%s: pid %d is %s's and %s's", name, pid, a.exe, b.exe)
					}
				}
			}
		}
	}
}

// TestAudioBeforeStart: the AudioController works before any Start, as the share sheet needs (05 §13.3); without a
// watcher the report is a one-shot snapshot and the levels are ErrAudioNotRunning; the mode and the mute need a
// session (the mode is checked first).
func TestAudioBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()

		// No watcher: a one-shot snapshot of the script's start under the stored rules.
		if err := e.SetExclusions(ctx, desktopExclusions()); err != nil {
			t.Fatal(err)
		}
		r := mustReport(t, e)
		if r.State != stateStopped || r.Engine != engineNone || r.Live || verdict(r, "discord") != "kept_out/builtin" {
			t.Errorf("the snapshot: %+v", r)
		}
		if _, err := e.AudioLevels(ctx); !errors.Is(err, media.ErrAudioNotRunning) || opOf(err) != "audio_levels_get" {
			t.Errorf("AudioLevels without a watcher: %v", err)
		}

		mustWatch(t, e, desktopExclusions())
		r = mustReport(t, e)
		if r.State != "ok" || r.Engine != engineIncludeSet || r.Live || r.Mode != media.AudioExcludeApps ||
			r.Counters.Streams != 0 || r.Probe.Result != "ok" || !r.Device.Present {
			t.Errorf("watching: %+v", r)
		}
		lv := mustLevels(t, e)
		if l, ok := level(lv, "exe:spotify.exe"); !ok || l.Captured || !near(l.PeakDB, -18) || lv.MixPeakDB != silenceDB {
			t.Errorf("levels while watching: %+v", lv)
		}
		if lv.ReportSeq != r.Seq {
			t.Errorf("ReportSeq %d, report Seq %d", lv.ReportSeq, r.Seq)
		}

		for m, want := range map[media.AudioMode]media.ErrorCode{
			media.AudioOff: media.CodeAudioNotRunning, media.AudioExcludeApps: media.CodeAudioNotRunning,
			media.AudioEverything: media.CodeInvalid, media.AudioTestTone: media.CodeInvalid, "": media.CodeInvalid,
			"exclude": media.CodeInvalid,
		} {
			if err := e.SetAudioMode(ctx, m); codeOf(err) != want || opOf(err) != "audio_set_mode" {
				t.Errorf("SetAudioMode(%q) without a session = %v, want %q", m, err, want)
			}
		}
		if err := e.SetAudioMuted(ctx, true); !errors.Is(err, media.ErrAudioNotRunning) ||
			opOf(err) != "audio_set_muted" {
			t.Errorf("SetAudioMuted without a session: %v", err)
		}
		// A session without audio is no audio session either.
		cfg := testConfig()
		cfg.Audio = nil
		s := mustStart(t, e, cfg)
		if err := e.SetAudioMuted(ctx, true); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMuted in a session without audio: %v", err)
		}
		if err := e.SetAudioMode(ctx, media.AudioOff); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMode in a session without audio: %v", err)
		}
		if r := mustReport(t, e); r.Live {
			t.Error("a session without audio is live audio")
		}
		_ = s.Stop()

		res, err := e.SoundCheck(ctx, 0, 3*time.Second)
		if err != nil || !res.Captured {
			t.Errorf("SoundCheck before Start: %+v, %v", res, err)
		}
		if err := e.UnwatchAudio(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.UnwatchAudio(ctx); err != nil { // idempotent
			t.Fatal(err)
		}
		if r := mustReport(t, e); r.State != stateStopped || r.Engine != engineNone {
			t.Errorf("after UnwatchAudio: %+v", r)
		}
		if _, err := e.AudioLevels(ctx); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("AudioLevels after UnwatchAudio: %v", err)
		}
	})
}

// TestAudioNormal: 05 §13.3's normal script, seconds since the watch started: t0 Spotify heard (−18 dB ± 6 dB at
// 0.5 Hz), Discord kept out (built-in, activity −24 dB), Steam kept out (mic, silent); t5 Chrome starts using the
// mic and is kept out (mic, browser).
func TestAudioNormal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		t0 := time.Now()
		mustWatch(t, e, desktopExclusions())
		r := mustReport(t, e)
		if got := ids(r); !slices.Equal(got, []string{"exe:spotify.exe", "discord", "exe:steam.exe"}) {
			t.Fatalf("rows at t0: %v", got)
		}
		if verdict(r, "exe:spotify.exe") != "heard/" || verdict(r, "discord") != "kept_out/builtin" ||
			verdict(r, "exe:steam.exe") != "kept_out/mic" {
			t.Errorf("verdicts at t0: %+v", r.Apps)
		}
		if d := row(r, "discord"); d.Name != "Discord" || !d.Playing || !d.UsesMic || d.Sessions != 2 {
			t.Errorf("Discord: %+v", d)
		}
		if s := row(r, "exe:steam.exe"); s.Playing || !s.UsesMic || s.Browser {
			t.Errorf("Steam: %+v", s)
		}
		for _, tc := range []struct {
			at      time.Duration
			spotify float64
		}{{0, -18}, {500 * time.Millisecond, -12}, {time.Second, -18}, {1500 * time.Millisecond, -24}} {
			time.Sleep(time.Until(t0.Add(tc.at)))
			lv := mustLevels(t, e)
			s, _ := level(lv, "exe:spotify.exe")
			d, _ := level(lv, "discord")
			st, _ := level(lv, "exe:steam.exe")
			if !near(s.PeakDB, tc.spotify) || s.RMSDB != s.PeakDB || s.Captured || !near(d.PeakDB, -24) ||
				st.PeakDB != silenceDB || len(lv.Apps) != 3 || lv.Apps[2].AppID != "exe:steam.exe" {
				t.Errorf("levels at %v: %+v", tc.at, lv)
			}
		}

		time.Sleep(time.Until(t0.Add(5*time.Second - time.Millisecond)))
		before := mustReport(t, e)
		if verdict(before, "exe:chrome.exe") != "-" {
			t.Fatal("Chrome before 5 s")
		}
		time.Sleep(time.Millisecond)
		r = mustReport(t, e)
		c := row(r, "exe:chrome.exe")
		if verdict(r, "exe:chrome.exe") != "kept_out/mic" || !c.Browser || !c.UsesMic || !c.Playing || c.Name != "Google Chrome" {
			t.Errorf("Chrome at 5 s: %+v", c)
		}
		if r.Seq <= before.Seq {
			t.Errorf("Seq %d after Chrome came, %d before", r.Seq, before.Seq)
		}
		if at := eventAt(r, "session_created"); at != int64(5*time.Second) || r.Events[len(r.Events)-1].App != "exe:chrome.exe" {
			t.Errorf("events %v (last at %d)", eventsOf(r), at)
		}
		if l, ok := level(mustLevels(t, e), "exe:chrome.exe"); !ok || !near(l.PeakDB, -30) {
			t.Errorf("Chrome's level %+v", l)
		}
		// A new watch starts the script again.
		_ = e.UnwatchAudio(t.Context())
		mustWatch(t, e, desktopExclusions())
		if r := mustReport(t, e); verdict(r, "exe:chrome.exe") != "-" {
			t.Error("Chrome right after a new watch")
		}
	})
}

// TestAudioVerdicts: the verdicts follow the rule set with 05 §5.3's precedence, for WatchAudio, SetExclusions and a
// Start's Exclusions alike; ids and names come from the matched rule.
func TestAudioVerdicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		mustWatch(t, e, media.Exclusions{})
		time.Sleep(5 * time.Second)
		r := mustReport(t, e)
		// No rules at all: everything heard, ids from the exe; mic users heard with included_uses_mic.
		for _, id := range []string{"exe:spotify.exe", "exe:discord.exe", "exe:steam.exe", "exe:chrome.exe"} {
			if verdict(r, id) != "heard/" {
				t.Errorf("without rules: %s %s", id, verdict(r, id))
			}
		}
		if d := row(r, "exe:discord.exe"); !slices.Equal(d.Warnings, []string{"included_uses_mic"}) || d.Browser {
			t.Errorf("Discord without rules: %+v", d)
		}

		viewer := media.Rule{Kind: media.RuleAppExclude, Source: media.SourceViewer, Value: "chrome.exe"}
		allowChrome := media.Rule{Kind: media.RuleAppAllow, Source: media.SourceUser, Value: "chrome.exe"}
		for _, tc := range []struct {
			name string
			ex   media.Exclusions
			want map[string]string
		}{
			{"the desktop set", desktopExclusions(), map[string]string{"exe:spotify.exe": "heard/",
				"discord": "kept_out/builtin", "exe:steam.exe": "kept_out/mic", "exe:chrome.exe": "kept_out/mic"}},
			{"mic users heard", func() media.Exclusions { ex := desktopExclusions(); ex.KeepOutMicUsers = false; return ex }(),
				map[string]string{"exe:steam.exe": "not_captured/launcher", "exe:chrome.exe": "heard/"}},
			{"a user keep-out", with(desktopExclusions(),
				media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser, Value: "SPOTIFY.EXE"}),
				map[string]string{"exe:spotify.exe": "kept_out/user"}},
			{"an include", with(desktopExclusions(), allowChrome), map[string]string{"exe:chrome.exe": "heard/"}},
			{"the viewer beats an include", with(desktopExclusions(), allowChrome, viewer),
				map[string]string{"exe:chrome.exe": "kept_out/viewer"}},
			{"an include of a launcher", with(desktopExclusions(),
				media.Rule{Kind: media.RuleAppAllow, Source: media.SourceUser, Value: "steam.exe"}),
				map[string]string{"exe:steam.exe": "not_captured/launcher"}},
			{"a built-in beats an include", with(desktopExclusions(),
				media.Rule{Kind: media.RuleAppAllow, Source: media.SourceUser, Value: "discord.exe"}),
				map[string]string{"discord": "kept_out/builtin"}},
			{"an isshoni instance", with(desktopExclusions(),
				media.Rule{Kind: media.RuleInstance, Source: media.SourceSelf, Value: "4124"}),
				map[string]string{"exe:spotify.exe": "kept_out/isshoni"}},
			{"isshoni.exe in the walk", with(media.Exclusions{},
				media.Rule{Kind: media.RuleAppExclude, Source: media.SourceSelf, Value: "spotify.exe", AppID: "isshoni",
					Name: "isshoni"}),
				map[string]string{"isshoni": "kept_out/isshoni"}},
		} {
			if err := e.SetExclusions(ctx, tc.ex); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			r := mustReport(t, e)
			for id, want := range tc.want {
				if got := verdict(r, id); got != want {
					t.Errorf("%s: %s is %s, want %s", tc.name, id, got, want)
				}
			}
		}

		// Include on Chrome: heard, with included_uses_mic; Chrome shared through SharedPID: heard with
		// shared_app_uses_mic, and with a user keep-out kept out with shared_app_kept_out.
		_ = e.SetExclusions(ctx, with(desktopExclusions(), allowChrome))
		if c := row(mustReport(t, e), "exe:chrome.exe"); !slices.Equal(c.Warnings, []string{"included_uses_mic"}) {
			t.Errorf("included Chrome: %+v", c)
		}
		shared := desktopExclusions()
		shared.SharedPID = 11240 + 4*13 // one of Chrome's 14 processes
		_ = e.SetExclusions(ctx, shared)
		if c := row(mustReport(t, e), "exe:chrome.exe"); c.Verdict != "heard" || !c.Shared ||
			!slices.Equal(c.Warnings, []string{"shared_app_uses_mic"}) {
			t.Errorf("shared Chrome: %+v", c)
		}
		_ = e.SetExclusions(ctx, with(shared, media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser,
			Value: "chrome.exe"}))
		if c := row(mustReport(t, e), "exe:chrome.exe"); c.Reason != "user" ||
			!slices.Equal(c.Warnings, []string{"shared_app_kept_out"}) {
			t.Errorf("shared Chrome kept out: %+v", c)
		}
		shared.SharedPID = 11240 + 4*14 // not Chrome's
		_ = e.SetExclusions(ctx, shared)
		if c := row(mustReport(t, e), "exe:chrome.exe"); c.Shared {
			t.Errorf("Chrome shared through another pid: %+v", c)
		}

		// Each change of the rules is a rules_changed event; the same rules again are none.
		_ = e.SetExclusions(ctx, desktopExclusions())
		n := len(mustReport(t, e).Events)
		_ = e.SetExclusions(ctx, desktopExclusions())
		_ = e.WatchAudio(ctx, desktopExclusions())
		if r := mustReport(t, e); len(r.Events) != n {
			t.Errorf("events after the same rules: %v", eventsOf(r)[n:])
		}

		// A Start's Exclusions replace the watcher's rules (05 §4.2).
		s := mustStart(t, e, audioConfig(media.Exclusions{}))
		if r := mustReport(t, e); verdict(r, "exe:discord.exe") != "heard/" || eventAt(r, "rules_changed") != eventAt(r, "stream_running") {
			t.Errorf("after a Start without rules: %v, %v", verdict(r, "exe:discord.exe"), eventsOf(r))
		}
		_ = s.Stop()
	})
}

// TestAudioExclusionsChecked: rules the DLL refuses are CodeInvalid in WatchAudio, SetExclusions and Start (05 §4.3),
// with a Detail that names the rule's index, never its value; nothing is applied.
func TestAudioExclusionsChecked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		mustWatch(t, e, desktopExclusions())
		good := media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser, Value: "secretapp.exe"}
		bad := func(mod func(r *media.Rule)) media.Exclusions {
			r := good
			mod(&r)
			return with(desktopExclusions(), r)
		}
		for _, tc := range []struct {
			name string
			ex   media.Exclusions
		}{
			{"513 rules", media.Exclusions{Rules: slices.Repeat([]media.Rule{good}, 513)}},
			{"an unknown kind", bad(func(r *media.Rule) { r.Kind = media.RuleWebViewDataDir + 1 })},
			{"an unknown source", bad(func(r *media.Rule) { r.Source = media.SourceData + 1 })},
			{"an empty value", bad(func(r *media.Rule) { r.Value = "" })},
			{"a 520-byte value", bad(func(r *media.Rule) { r.Value = strings.Repeat("s", 520) })},
			{"a NUL in the value", bad(func(r *media.Rule) { r.Value = "secretapp.exe\x00" })},
			{"a NUL in the name", bad(func(r *media.Rule) { r.Name = "Secret\x00App" })},
			{"a pid with a letter", bad(func(r *media.Rule) { r.Kind, r.Value = media.RuleInstance, "12a" })},
			{"a negative pid", bad(func(r *media.Rule) { r.Kind, r.Value = media.RuleInstance, "-1" })},
			{"a pid over 32 bits", bad(func(r *media.Rule) { r.Kind, r.Value = media.RuleInstance, "4294967296" })},
			{"an upper-case app id", bad(func(r *media.Rule) { r.AppID = "SecretApp" })},
			{"a 64-byte app id", bad(func(r *media.Rule) { r.AppID = strings.Repeat("s", 64) })},
			{"a space in the app id", bad(func(r *media.Rule) { r.AppID = "secret app" })},
		} {
			before := mustReport(t, e)
			for op, err := range map[string]error{
				"audio_watch":    e.WatchAudio(ctx, tc.ex),
				"set_exclusions": e.SetExclusions(ctx, tc.ex),
			} {
				if !errors.Is(err, media.ErrInvalid) || opOf(err) != op {
					t.Errorf("%s: %s = %v, want invalid", tc.name, op, err)
				} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "Secret") {
					t.Errorf("%s: the error names the rule: %v", tc.name, err)
				}
			}
			if _, err := e.Start(ctx, audioConfig(tc.ex)); !errors.Is(err, media.ErrInvalid) || opOf(err) != "start" {
				t.Errorf("%s: Start = %v, want invalid", tc.name, err)
			}
			if after := mustReport(t, e); !reflect.DeepEqual(after.Apps, before.Apps) || len(after.Events) != len(before.Events) {
				t.Errorf("%s: a refused rule set changed the report", tc.name)
			}
		}
		if err := e.SetExclusions(ctx, media.Exclusions{Rules: slices.Repeat([]media.Rule{good}, 512)}); err != nil {
			t.Errorf("512 rules: %v", err)
		}
		ok := with(media.Exclusions{}, media.Rule{Kind: media.RuleInstance, Value: "4294967295", AppID: "a-b_c.d:e9"},
			media.Rule{Kind: media.RuleAppAllow, Source: media.SourceData, Value: strings.Repeat("v", 519),
				AppID: strings.Repeat("a", 63)})
		if err := e.SetExclusions(ctx, ok); err != nil {
			t.Errorf("rules at the limits: %v", err)
		}
	})
}

// TestAudioLive: a session with audio runs the watcher (also without WatchAudio), captures the heard playing apps,
// and its pause, mute and sound-off show in the report, the levels and the Calls.
func TestAudioLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		time.Sleep(time.Second)
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		r := mustReport(t, e)
		if !r.Live || r.State != "ok" || r.Engine != engineIncludeSet || r.Counters.Streams != 1 ||
			row(r, "exe:spotify.exe").Streams != 1 || row(r, "discord").Streams != 0 {
			t.Fatalf("live: %+v", r)
		}
		if got := eventsOf(r); !slices.Contains(got, "stream_running exe:spotify.exe") {
			t.Errorf("events %v", got)
		}
		if i := slices.IndexFunc(r.Sessions, func(s media.AudioSession) bool { return s.AppID == "exe:spotify.exe" }); i < 0 ||
			r.Sessions[i].Stream != "running" || r.Sessions[i].State != "active" {
			t.Errorf("sessions %+v", r.Sessions)
		}
		time.Sleep(500 * time.Millisecond) // Spotify at its peak, −12 dB
		lv := mustLevels(t, e)
		sp, _ := level(lv, "exe:spotify.exe")
		d, _ := level(lv, "discord")
		if !sp.Captured || !near(sp.PeakDB, -12) || !near(sp.RMSDB, -15) || d.Captured || !near(d.PeakDB, -24) ||
			!near(lv.MixPeakDB, -12) || !near(lv.MixRMSDB, -15) || lv.QPCNanos != int64(1500*time.Millisecond) {
			t.Errorf("live levels: %+v", lv)
		}

		// The pause mute.
		if err := e.SetAudioMuted(ctx, true); err != nil {
			t.Fatal(err)
		}
		muted := mustReport(t, e)
		lv = mustLevels(t, e)
		sp, _ = level(lv, "exe:spotify.exe")
		if !muted.Muted || muted.Seq <= r.Seq || sp.PeakDB != silenceDB || lv.MixPeakDB != silenceDB ||
			lv.ReportSeq != muted.Seq || !slices.Contains(eventsOf(muted), "muted") {
			t.Errorf("muted: %+v, %+v", muted, lv)
		}
		_ = e.SetAudioMuted(ctx, false)
		// The session's pause.
		_ = s.SetPaused(true)
		if r := mustReport(t, e); !r.Paused || mustLevels(t, e).MixPeakDB != silenceDB {
			t.Errorf("paused: %+v", r)
		}
		_ = s.SetPaused(false)
		// Share sound off: mode off, engine none, nothing captured; on again.
		if err := e.SetAudioMode(ctx, media.AudioOff); err != nil {
			t.Fatal(err)
		}
		off := mustReport(t, e)
		lv = mustLevels(t, e)
		sp, _ = level(lv, "exe:spotify.exe")
		if off.Mode != media.AudioOff || off.Engine != engineNone || off.Counters.Streams != 0 || sp.Captured ||
			lv.MixPeakDB != silenceDB || !slices.Contains(eventsOf(off), "mode_changed (off)") ||
			!slices.Contains(eventsOf(off), "stream_removed exe:spotify.exe") {
			t.Errorf("sound off: %+v, %+v", off, lv)
		}
		_ = e.SetAudioMode(ctx, media.AudioExcludeApps)
		if on := mustReport(t, e); on.Mode != media.AudioExcludeApps || row(on, "exe:spotify.exe").Streams != 1 {
			t.Errorf("sound on: %+v", on)
		}
		var audioCalls []Call
		for _, c := range s.Calls() {
			if strings.HasPrefix(c.Op, "audio_") {
				c.At = time.Time{}
				audioCalls = append(audioCalls, c)
			}
		}
		if want := []Call{{Op: "audio_muted", On: true, Muted: true}, {Op: "audio_muted"},
			{Op: "audio_mode", Mode: media.AudioOff}, {Op: "audio_mode", Mode: media.AudioExcludeApps}}; !reflect.DeepEqual(audioCalls, want) {
			t.Errorf("audio calls %+v, want %+v", audioCalls, want)
		}

		// A rule change that renames a row is no session or stream event: Spotify keeps its session and stream
		// under the allow rule's id, and gets its exe's id back.
		n := len(mustReport(t, e).Events)
		_ = e.SetExclusions(ctx, with(desktopExclusions(),
			media.Rule{Kind: media.RuleAppAllow, Source: media.SourceUser, Value: "spotify.exe", AppID: "spotify"}))
		renamed := mustReport(t, e)
		_ = e.SetExclusions(ctx, desktopExclusions())
		back := mustReport(t, e)
		if got := eventsOf(back)[n:]; !slices.Equal(got, []string{"rules_changed", "rules_changed"}) ||
			row(renamed, "spotify").Streams != 1 || row(back, "exe:spotify.exe").Streams != 1 {
			t.Errorf("renamed: %+v, back: %+v, events %v", renamed.Apps, back.Apps, got)
		}

		// The session ends: without a watch, the watcher stops; its streams are removed.
		time.Sleep(time.Second)
		_ = s.Stop()
		time.Sleep(time.Second)
		r = mustReport(t, e)
		if r.Live || r.State != stateStopped || r.Muted {
			t.Errorf("after Stop: %+v", r)
		}
		if _, err := e.AudioLevels(ctx); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("AudioLevels after Stop: %v", err)
		}

		// Watching: UnwatchAudio during the share keeps the watcher until the session ends.
		mustWatch(t, e, desktopExclusions())
		s = mustStart(t, e, audioConfig(desktopExclusions()))
		_ = e.UnwatchAudio(ctx)
		if r := mustReport(t, e); r.State != "ok" || !r.Live {
			t.Errorf("unwatched during the share: %+v", r)
		}
		time.Sleep(time.Second)
		end := time.Now()
		_ = s.Stop()
		time.Sleep(2 * time.Second)
		r = mustReport(t, e)
		if r.State != stateStopped || eventAt(r, "stream_removed") != end.Sub(e.audio.created).Nanoseconds() {
			t.Errorf("after the share: %+v", r)
		}
	})
}

// TestAudioModeSilence: sound off makes the session's audio Opus silence, as the native mix is silent while the
// mode is off (05 §4.3); sound on brings the tone back.
func TestAudioModeSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		skipEvents(t, s, 2)
		silence := silenceOpus().frames[0]
		isSilent := func(pkts []media.Packet) bool {
			for _, p := range pkts {
				if op, err := parseOpus(p.Data); err != nil || !bytes.Equal(op.frames[0], silence) {
					return false
				}
			}
			return len(pkts) > 0
		}
		if isSilent(ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)) {
			t.Fatal("silent before sound off")
		}
		_ = e.SetAudioMode(t.Context(), media.AudioOff)
		if !isSilent(ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)) {
			t.Error("not silent with sound off")
		}
		// The pause unmute doesn't turn the sound back on: each flag is cleared only by its own call.
		_ = e.SetAudioMuted(t.Context(), true)
		_ = e.SetAudioMuted(t.Context(), false)
		if !isSilent(ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)) {
			t.Error("the unmute turned the sound on")
		}
		_ = e.SetAudioMode(t.Context(), media.AudioExcludeApps)
		if isSilent(ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)) {
			t.Error("still silent with sound on")
		}
	})
}

// TestAudioDegraded: the probe failed: engine endpoint, everything heard including Discord; the sound check says
// endpoint.
func TestAudioDegraded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "degraded"})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		r := mustReport(t, e)
		if r.Engine != engineEndpoint || r.State != "degraded" || r.Probe.Result != "failed" {
			t.Errorf("degraded: %+v", r)
		}
		for _, a := range r.Apps {
			if a.Verdict != "heard" || a.Reason != "" || slices.Contains(a.Warnings, "included_uses_mic") {
				t.Errorf("row %+v", a)
			}
		}
		if row(r, "discord").Name != "Discord" {
			t.Errorf("Discord's row %+v keeps the rule's id and name", row(r, "discord"))
		}
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		r = mustReport(t, e)
		lv := mustLevels(t, e)
		// The mix holds Spotify (−18 dB) and Discord (−24 dB): everything is heard.
		want := 10 * math.Log10(math.Pow(10, -1.8)+math.Pow(10, -2.4))
		if r.Counters.Streams != 1 || row(r, "exe:spotify.exe").Streams != 0 || !near(lv.MixPeakDB, want) {
			t.Errorf("live degraded: %+v, %+v", r, lv)
		}
		_ = s.Stop()
		if res, err := e.SoundCheck(t.Context(), 0, 3*time.Second); err != nil || res != soundCheckEndpoint {
			t.Errorf("SoundCheck: %+v, %v", res, err)
		}
	})
}

// TestAudioDegradeLive: as normal until 5 s after a session with audio starts; then process loopback fails twice
// (05 §6.5): engine endpoint, state degraded, probe failed, events stream_failed ×2 and probe_failed, everything
// heard including Discord. It stays so after the share, and the next share starts normal again.
func TestAudioDegradeLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "degrade-live"})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		time.Sleep(2 * time.Second)
		if r := mustReport(t, e); r.State != "ok" || r.Engine != engineIncludeSet {
			t.Fatalf("before the share: %+v", r)
		}
		start := time.Now()
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		time.Sleep(5*time.Second - time.Millisecond)
		before := mustReport(t, e)
		if before.State != "ok" || verdict(before, "discord") != "kept_out/builtin" || row(before, "exe:spotify.exe").Streams != 1 {
			t.Fatalf("4.999 s into the share: %+v", before)
		}
		time.Sleep(time.Millisecond)
		r := mustReport(t, e)
		if r.Engine != engineEndpoint || r.State != "degraded" || r.Probe.Result != "failed" || r.Seq <= before.Seq ||
			verdict(r, "discord") != "heard/" || verdict(r, "exe:steam.exe") != "heard/" || verdict(r, "exe:chrome.exe") != "heard/" {
			t.Errorf("5 s into the share: %+v", r)
		}
		failAt := start.Add(5 * time.Second).Sub(e.audio.created).Nanoseconds()
		var got []string
		for _, ev := range r.Events {
			if ev.TNanos == failAt {
				got = append(got, ev.Code+" "+ev.App)
			}
		}
		want := []string{"stream_failed exe:spotify.exe", "stream_failed exe:spotify.exe", "probe_failed ",
			"stream_removed exe:spotify.exe"}
		if !slices.Equal(got, want) {
			t.Errorf("events at the failure %v, want %v", got, want)
		}
		if lv := mustLevels(t, e); !near(lv.MixPeakDB, 10*math.Log10(
			math.Pow(10, spotifyLevel(7*time.Second)/10)+math.Pow(10, -2.4)+math.Pow(10, -3))) {
			t.Errorf("the mix holds Discord and Chrome now: %+v", lv)
		}
		_ = s.Stop()
		time.Sleep(time.Second)
		if r := mustReport(t, e); r.State != "degraded" || r.Live {
			t.Errorf("after the share: %+v", r)
		}
		s = mustStart(t, e, audioConfig(desktopExclusions()))
		if r := mustReport(t, e); r.State != "ok" || verdict(r, "discord") != "kept_out/builtin" {
			t.Errorf("the next share: %+v", r)
		}
		// A share that ends within 5 s never fails.
		time.Sleep(4 * time.Second)
		_ = s.Stop()
		time.Sleep(10 * time.Second)
		if r := mustReport(t, e); r.State != "ok" {
			t.Errorf("after a 4 s share: %+v", r)
		}
	})
}

// TestAudioOldWindows: probe unsupported on build 18363: engine endpoint, everything heard.
func TestAudioOldWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "old-windows"})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		r := mustReport(t, e)
		if r.Probe.Result != "unsupported" || r.Probe.OSBuild != 18363 || r.Engine != engineEndpoint ||
			r.State != "degraded" || verdict(r, "discord") != "heard/" {
			t.Errorf("old-windows: %+v", r)
		}
		if res, err := e.SoundCheck(t.Context(), 0, 3*time.Second); err != nil || res.Backend != "endpoint" || !res.Captured {
			t.Errorf("SoundCheck: %+v, %v", res, err)
		}
	})
}

// TestAudioNoDevice: no endpoint: state no-device, nothing plays, only the running built-in voice app has a row; the
// sound check finds no device.
func TestAudioNoDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "no-device"})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		time.Sleep(10 * time.Second)
		r := mustReport(t, e)
		if r.State != "no-device" || r.Engine != engineNone || r.Device.Present || r.Device.RenderEndpoints != 0 ||
			r.Probe.Result != "skipped" || len(r.Sessions) != 0 {
			t.Errorf("no-device: %+v", r)
		}
		if got := ids(r); !slices.Equal(got, []string{"discord"}) || row(r, "discord").Playing ||
			verdict(r, "discord") != "kept_out/builtin" {
			t.Errorf("rows %v: %+v", got, r.Apps)
		}
		if lv := mustLevels(t, e); len(lv.Apps) != 0 || lv.MixPeakDB != silenceDB {
			t.Errorf("levels %+v", lv)
		}
		// Discord's row has no session: its appearing and going are no session events.
		if got := eventsOf(r); !slices.Equal(got, []string{"rules_changed"}) {
			t.Errorf("events after WatchAudio: %v", got)
		}
		_ = e.SetExclusions(t.Context(), media.Exclusions{})
		if r := mustReport(t, e); len(r.Apps) != 0 ||
			!slices.Equal(eventsOf(r), []string{"rules_changed", "rules_changed"}) {
			t.Errorf("without the built-in rule: rows %+v, events %v", r.Apps, eventsOf(r))
		}
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		if r := mustReport(t, e); !r.Live || r.Engine != engineNone || r.Counters.Streams != 0 {
			t.Errorf("live without a device: %+v", r)
		}
		_ = s.Stop()
		if res, err := e.SoundCheck(t.Context(), 0, 3*time.Second); err != nil || res != soundCheckNoDevice {
			t.Errorf("SoundCheck: %+v, %v", res, err)
		}
	})
}

// TestAudioFastPath: only Spotify and a game: engine exclude-self; at 8 s Discord starts and the include set takes
// over (fastpath_exit). Without the fast-path flag, the include set runs from the start.
func TestAudioFastPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "fast-path"})
		defer e.Close()
		t0 := time.Now()
		mustWatch(t, e, desktopExclusions())
		s := mustStart(t, e, audioConfig(desktopExclusions()))
		r := mustReport(t, e)
		if r.Engine != engineExcludeSelf || !slices.Equal(ids(r), []string{"exe:spotify.exe", "exe:eldenring.exe"}) ||
			r.Counters.Streams != 1 || row(r, "exe:spotify.exe").Streams != 0 || verdict(r, "exe:eldenring.exe") != "heard/" {
			t.Errorf("before 8 s: %+v", r)
		}
		lv := mustLevels(t, e)
		want := 10 * math.Log10(math.Pow(10, -1.8)+math.Pow(10, -2))
		if g, _ := level(lv, "exe:eldenring.exe"); g.Captured || !near(lv.MixPeakDB, want) {
			t.Errorf("levels on the fast path: %+v", lv)
		}
		time.Sleep(10 * time.Second)
		r = mustReport(t, e)
		if r.Engine != engineIncludeSet || verdict(r, "discord") != "kept_out/builtin" || r.Counters.Streams != 2 ||
			row(r, "exe:spotify.exe").Streams != 1 {
			t.Errorf("after 8 s: %+v", r)
		}
		if at := eventAt(r, "fastpath_exit"); at != t0.Add(8*time.Second).Sub(e.audio.created).Nanoseconds() {
			t.Errorf("fastpath_exit at %d: %v", at, eventsOf(r))
		}
		_ = s.Stop()

		noFast := desktopExclusions()
		noFast.FastPath = false
		_ = e.UnwatchAudio(t.Context())
		mustWatch(t, e, noFast)
		if r := mustReport(t, e); r.Engine != engineIncludeSet {
			t.Errorf("without the fast-path flag: %+v", r)
		}
	})
}

// TestAudioSharedMic: the shared game uses the mic and is heard with shared_app_uses_mic (05 §5.8).
func TestAudioSharedMic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "shared-mic"})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		r := mustReport(t, e)
		v := row(r, "exe:valorant.exe")
		if v.Verdict != "heard" || !v.Shared || !v.UsesMic || !slices.Equal(v.Warnings, []string{"shared_app_uses_mic"}) ||
			verdict(r, "discord") != "kept_out/builtin" {
			t.Errorf("shared-mic: %+v", r.Apps)
		}
		_ = e.SetExclusions(t.Context(), with(desktopExclusions(),
			media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser, Value: "valorant.exe"}))
		if v := row(mustReport(t, e), "exe:valorant.exe"); v.Reason != "user" ||
			!slices.Equal(v.Warnings, []string{"shared_app_kept_out"}) {
			t.Errorf("the shared app kept out: %+v", v)
		}
	})
}

// TestAudioMany: 60 apps; live, at most 48 include streams and the rest over the limit (05 §6.2 step 4); at most 64
// level entries, playing apps first.
func TestAudioMany(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "many"})
		defer e.Close()
		mustWatch(t, e, media.Exclusions{FastPath: true})
		r := mustReport(t, e)
		if len(r.Apps) != 60 || r.Engine != engineExcludeSelf {
			t.Fatalf("%d rows, engine %s", len(r.Apps), r.Engine)
		}
		lv := mustLevels(t, e)
		if len(lv.Apps) != 60 {
			t.Errorf("%d level entries", len(lv.Apps))
		}
		seenQuiet := false
		for _, l := range lv.Apps {
			quiet := !row(r, l.AppID).Playing
			if seenQuiet && !quiet {
				t.Errorf("playing %s after a quiet app", l.AppID)
			}
			seenQuiet = seenQuiet || quiet
		}
		// A pid is one app's: the shared pid and an instance rule each name exactly one row, also where an app with 4
		// processes is followed by another app.
		for _, a := range audioScenarios["many"]().apps[3:] {
			id := "exe:" + a.exe
			for _, pid := range []uint32{a.pid, a.pid + 4*uint32(max(a.pids, 1)-1)} { // its first and last process
				_ = e.SetExclusions(t.Context(), media.Exclusions{FastPath: true, SharedPID: pid})
				var shared []string
				for _, row := range mustReport(t, e).Apps {
					if row.Shared {
						shared = append(shared, row.ID)
					}
				}
				if !slices.Equal(shared, []string{id}) {
					t.Errorf("SharedPID %d: shared rows %v, want %s", pid, shared, id)
				}
			}
			_ = e.SetExclusions(t.Context(), media.Exclusions{FastPath: true, Rules: []media.Rule{{
				Kind: media.RuleInstance, Source: media.SourceSelf, Value: strconv.FormatUint(uint64(a.pid), 10),
			}}})
			var out []string
			for _, row := range mustReport(t, e).Apps {
				if row.Verdict == verdictKeptOut {
					out = append(out, row.ID+"/"+row.Reason)
				}
			}
			if !slices.Equal(out, []string{id + "/isshoni"}) {
				t.Errorf("instance %d: kept out %v, want %s", a.pid, out, id)
			}
		}
		_ = e.SetExclusions(t.Context(), media.Exclusions{FastPath: true})
		// Many apps playing on the include set: 48 streams, the rest not captured (limit).
		s := mustStart(t, e, audioConfig(media.Exclusions{}))
		r = mustReport(t, e)
		heard, limit := 0, 0
		for _, a := range r.Apps {
			heard += a.Streams
			if a.Reason == "limit" {
				limit++
				if !a.Playing || a.Streams != 0 {
					t.Errorf("over the limit: %+v", a)
				}
			}
		}
		if r.Engine != engineIncludeSet || heard != 48 || r.Counters.Streams != 48 || limit == 0 {
			t.Errorf("engine %s, %d streams (counters %d), %d over the limit", r.Engine, heard, r.Counters.Streams, limit)
		}
		_ = s.Stop()
	})
}

// TestAudioSeq: Seq changes only when the report does (not for levels, counters, qpcNs or events), and the levels'
// ReportSeq is the report's Seq.
func TestAudioSeq(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		r1 := mustReport(t, e)
		time.Sleep(time.Second)
		lv := mustLevels(t, e)
		r2 := mustReport(t, e)
		if r2.Seq != r1.Seq || lv.ReportSeq != r1.Seq || r2.QPCNanos != r1.QPCNanos+int64(time.Second) {
			t.Errorf("Seq %d → %d (levels %d), qpcNs %d → %d", r1.Seq, r2.Seq, lv.ReportSeq, r1.QPCNanos, r2.QPCNanos)
		}
		_ = e.SetExclusions(t.Context(), with(desktopExclusions(),
			media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser, Value: "spotify.exe"}))
		r3 := mustReport(t, e)
		if r3.Seq != r2.Seq+1 || mustLevels(t, e).ReportSeq != r3.Seq {
			t.Errorf("Seq %d after a change, %d before", r3.Seq, r2.Seq)
		}
		// Rules that change no verdict change no Seq, though they are an event.
		_ = e.SetExclusions(t.Context(), with(desktopExclusions(),
			media.Rule{Kind: media.RuleAppExclude, Source: media.SourceUser, Value: "spotify.exe"},
			media.Rule{Kind: media.RuleStop, Source: media.SourceData, Value: "cmd.exe"}))
		if r4 := mustReport(t, e); r4.Seq != r3.Seq || len(r4.Events) != len(r3.Events)+1 {
			t.Errorf("Seq %d → %d, events %d → %d", r3.Seq, r4.Seq, len(r3.Events), len(r4.Events))
		}
	})
}

// TestAudioEventsCapped: the report keeps the last 300 events.
func TestAudioEventsCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		mustStart(t, e, audioConfig(desktopExclusions()))
		for i := range 400 {
			_ = e.SetAudioMuted(t.Context(), i%2 == 0)
		}
		r := mustReport(t, e)
		if len(r.Events) != 300 || r.Events[299].Code != "unmuted" || r.Events[298].Code != "muted" {
			t.Errorf("%d events, the last %v", len(r.Events), eventsOf(r)[len(r.Events)-2:])
		}
	})
}

// TestSoundCheck: the answer comes after 1.5 s; a timeout outside 1–10 s is invalid, one under 1.5 s ends without a
// capture; ctx and Close end a check that waits.
func TestSoundCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		t0 := time.Now()
		res, err := e.SoundCheck(ctx, 4321, 3*time.Second)
		if err != nil || res != soundCheckLoopback || time.Since(t0) != 1500*time.Millisecond {
			t.Errorf("SoundCheck = %+v, %v after %v", res, err, time.Since(t0))
		}
		want := `{"captured":true,"levelDb":-14,"ms":1500,"backend":"process-loopback"}`
		if b, _ := json.Marshal(res); string(b) != want {
			t.Errorf("JSON %s, want %s", b, want)
		}
		for _, d := range []time.Duration{0, 999 * time.Millisecond, 10*time.Second + 1, -time.Second} {
			if _, err := e.SoundCheck(ctx, 0, d); !errors.Is(err, media.ErrInvalid) || opOf(err) != "audio_sound_check" {
				t.Errorf("timeout %v: %v", d, err)
			}
		}
		t0 = time.Now()
		res, err = e.SoundCheck(ctx, 0, 1200*time.Millisecond)
		if err != nil || res.Captured || res.Reason != "timeout" || res.Backend != "process-loopback" ||
			time.Since(t0) != 1200*time.Millisecond {
			t.Errorf("a short timeout: %+v, %v after %v", res, err, time.Since(t0))
		}

		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(time.Second, cancel)
		if _, err := e.SoundCheck(cctx, 0, 3*time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled check: %v", err)
		}
		time.AfterFunc(time.Second, func() { _ = e.Close() })
		t0 = time.Now()
		if _, err := e.SoundCheck(ctx, 0, 3*time.Second); !errors.Is(err, media.ErrStopped) || time.Since(t0) != time.Second {
			t.Errorf("a check during Close: %v after %v", err, time.Since(t0))
		}
	})
}

// TestAudioClosedAndCancelled: after Close every call is stopped (Reason "closed"); a done ctx is its error.
func TestAudioClosedAndCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		done, cancel := context.WithCancel(t.Context())
		cancel()
		_, err1 := e.AudioReport(done)
		_, err2 := e.AudioLevels(done)
		_, err3 := e.SoundCheck(done, 0, 3*time.Second)
		all := func(ctx context.Context) []error {
			return []error{e.WatchAudio(ctx, media.Exclusions{}), e.UnwatchAudio(ctx),
				e.SetExclusions(ctx, media.Exclusions{}), e.SetAudioMode(ctx, media.AudioOff), e.SetAudioMuted(ctx, true)}
		}
		for _, err := range append(all(done), err1, err2, err3) {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("with a done ctx: %v", err)
			}
		}
		_ = e.Close()
		ctx := t.Context()
		_, err1 = e.AudioReport(ctx)
		_, err2 = e.AudioLevels(ctx)
		_, err3 = e.SoundCheck(ctx, 0, 3*time.Second)
		for _, err := range append(all(ctx), err1, err2, err3) {
			if !errors.Is(err, media.ErrStopped) || reasonOf(err) != "closed" {
				t.Errorf("after Close: %v", err)
			}
		}
	})
}

// TestSetAudioStatus: a test's report replaces the script's (a copy each time) until nil.
func TestSetAudioStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		mustWatch(t, e, desktopExclusions())
		mine := media.AudioStatus{V: 1, Seq: 77, State: "ok", Engine: engineIncludeSet, Mode: media.AudioExcludeApps,
			Apps: []media.AudioApp{{ID: "exe:x.exe", Name: "X", Verdict: "not_captured", Reason: "pid_reuse",
				Detail: "Discord", Warnings: []string{}}}}
		e.SetAudioStatus(&mine)
		mine.Apps[0].ID = "changed"
		r, err := e.AudioReport(t.Context())
		if err != nil || r.Seq != 77 || len(r.Apps) != 1 || r.Apps[0].ID != "exe:x.exe" ||
			mustLevels(t, e).ReportSeq != 77 {
			t.Errorf("the test's report: %+v, %v", r, err)
		}
		r.Apps[0].ID = "changed"
		if r, _ := e.AudioReport(t.Context()); r.Apps[0].ID != "exe:x.exe" {
			t.Error("AudioReport returned the stored report")
		}
		e.SetAudioStatus(nil)
		if r := mustReport(t, e); r.Seq == 77 || row(r, "discord").ID == "" {
			t.Errorf("back to the script: %+v", r)
		}
	})
}

// TestAudioConcurrent: the AudioController is safe for concurrent use with sessions starting and stopping (run with
// -race).
func TestAudioConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{AudioScenario: "degrade-live"})
		defer e.Close()
		ctx := t.Context()
		var wg sync.WaitGroup
		for g := range 4 {
			wg.Go(func() {
				for i := range 50 {
					switch (g + i) % 6 {
					case 0:
						_ = e.WatchAudio(ctx, desktopExclusions())
					case 1:
						_, _ = e.AudioReport(ctx)
					case 2:
						_, _ = e.AudioLevels(ctx)
					case 3:
						_ = e.SetExclusions(ctx, media.Exclusions{KeepOutMicUsers: i%2 == 0})
					case 4:
						_ = e.SetAudioMuted(ctx, i%2 == 0)
						_ = e.SetAudioMode(ctx, media.AudioOff)
					case 5:
						_ = e.UnwatchAudio(ctx)
					}
					time.Sleep(100 * time.Millisecond)
				}
			})
		}
		wg.Go(func() {
			for range 10 {
				s, err := e.Start(ctx, audioConfig(desktopExclusions()))
				if err != nil {
					t.Error(err)
					return
				}
				time.Sleep(700 * time.Millisecond)
				_ = s.Stop()
			}
		})
		wg.Wait()
		mustReport(t, e)
	})
}

// TestAudioStartMuteRace: a SetAudioMuted that races a Start with audio and finds its session is in the report: a
// live, muted report with a "muted" event, never a mute the report's events missed because the session ran before
// the AudioController knew it. Real goroutines, no synctest, so the scheduler can hit the window.
func TestAudioStartMuteRace(t *testing.T) {
	ctx := t.Context()
	for i := range 300 {
		e := NewEngine(EngineConfig{})
		started := make(chan media.Session, 1)
		go func() {
			s, err := e.Start(ctx, audioConfig(desktopExclusions()))
			if err != nil {
				t.Error(err)
			}
			started <- s
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := e.SetAudioMuted(ctx, true)
			if err == nil {
				break
			}
			if !errors.Is(err, media.ErrAudioNotRunning) || time.Now().After(deadline) {
				t.Fatalf("SetAudioMuted: %v", err)
			}
			runtime.Gosched()
		}
		if r := mustReport(t, e); !r.Live || !r.Muted || !slices.Contains(eventsOf(r), "muted") {
			t.Errorf("run %d: live %v, muted %v, events %v", i, r.Live, r.Muted, eventsOf(r))
		}
		if s := <-started; s != nil {
			_ = s.Stop()
		}
		_ = e.Close()
	}
}

// opOf returns the Op of err's media.Error, or "".
func opOf(err error) string {
	if me, ok := errors.AsType[*media.Error](err); ok {
		return me.Op
	}
	return ""
}
