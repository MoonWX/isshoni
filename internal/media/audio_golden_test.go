package media_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/media"
)

// The golden reports (05 §13.1) are im_list_audio_apps' JSON v1 (05 §12.1) for seven situations. This package
// parses them, and the C++ report writer's tests (native/common/audiomix) write the same files byte for byte from
// fixed inputs, so the two sides can't drift. Each file is exactly what the DLL returns, in this form:
//   - compact JSON, no whitespace, and no line break at all (not even at the end, so a Windows checkout that converts
//     line ends leaves the file unchanged);
//   - the keys of every object in 05 §12.1's order, all of them always present; empty lists are [];
//   - integers in decimal, sessions[].created as a decimal string; levelDb and audioCpuMsPerS in the shortest form
//     that reads back as the same double (Go's encoding/json; C++17 std::to_chars without a format writes the same
//     for these values: -60.1, -120, 6.2);
//   - strings as raw UTF-8 (names with é and CJK in report-many-apps.json), and no character in them needs escaping.
//
// goldenChecks names every golden file and what it shows beyond the common checks.
var goldenChecks = map[string]func(t *testing.T, r media.AudioReport){
	// A live share on the include set: 05 §12.1's example.
	"report-normal.json": func(t *testing.T, r media.AudioReport) {
		want(t, r.Live && r.Engine == "include-set" && r.State == "ok" && r.Probe.Result == "ok", "a live include set")
		want(t, appIs(r, "exe:spotify.exe", "heard", "") && app(r, "exe:spotify.exe").Streams == 1, "Spotify heard")
		want(t, appIs(r, "discord", "kept_out", "builtin"), "Discord kept out by the built-in rule")
		want(t, appIs(r, "exe:steam.exe", "kept_out", "mic") && !app(r, "exe:steam.exe").Playing, "Steam kept out")
		want(t, appIs(r, "exe:launcherx.exe", "not_captured", "tree") &&
			app(r, "exe:launcherx.exe").Detail == "Discord", "LauncherX not captured (tree, Discord)")
	},
	// The probe failed: the endpoint engine runs and everything is heard, Discord included (05 §6.1, §11.2).
	"report-degraded.json": func(t *testing.T, r media.AudioReport) {
		want(t, r.Engine == "endpoint" && r.State == "degraded" && r.Probe.Result == "failed", "a degraded endpoint")
		for _, a := range r.Apps {
			want(t, a.Verdict == "heard" && a.Reason == "", a.ID+" heard")
		}
		want(t, appIs(r, "discord", "heard", ""), "Discord heard")
	},
	// Watching with no endpoint at all: only a running built-in voice app has a row (05 §12.1's row rule).
	"report-no-device.json": func(t *testing.T, r media.AudioReport) {
		want(t, r.State == "no-device" && r.Engine == "none" && !r.Live, "no device, watching")
		want(t, r.Device == media.AudioDeviceState{} && r.Probe.Result == "skipped" && r.Probe.LevelDB == -120,
			"no endpoint, the probe skipped")
		want(t, len(r.Apps) == 1 && appIs(r, "discord", "kept_out", "builtin") && app(r, "discord").Sessions == 0,
			"one row: Discord without a session")
		want(t, len(r.Sessions) == 0, "no session")
	},
	// No voice app: the fast path's one EXCLUDE stream (05 §6.4).
	"report-fast-path.json": func(t *testing.T, r media.AudioReport) {
		want(t, r.Live && r.Engine == "exclude-self" && r.Counters.Streams == 1, "the fast path, one stream")
		for _, a := range r.Apps {
			want(t, a.Verdict == "heard" && a.Streams == 0, a.ID+" heard through the EXCLUDE stream")
		}
	},
	// The 128-row cap, 48 include streams, the other playing heard apps over the limit (05 §6.2 step 4, §12.1).
	"report-many-apps.json": func(t *testing.T, r media.AudioReport) {
		want(t, len(r.Apps) == 128, "128 rows")
		streams, limit := 0, 0
		for _, a := range r.Apps {
			streams += a.Streams
			if a.Verdict == "not_captured" && a.Reason == "limit" {
				limit++
			}
		}
		want(t, streams == 48 && r.Counters.Streams == 48 && limit > 0, "48 streams, the rest over the limit")
		want(t, app(r, "exe:app007.exe").Name == "Café Radio" && app(r, "exe:app008.exe").Name == "音乐播放器",
			"the UTF-8 names")
	},
	// The sheet before a share; Chrome holds the mic, so the whole browser is kept out (05 §5.5).
	"report-browser-mic.json": func(t *testing.T, r media.AudioReport) {
		want(t, !r.Live && r.Engine == "include-set", "watching")
		c := app(r, "exe:chrome.exe")
		want(t, appIs(r, "exe:chrome.exe", "kept_out", "mic") && c.Browser && c.UsesMic, "Chrome kept out (mic)")
		want(t, appIs(r, "exe:msedge.exe", "heard", "") && app(r, "exe:msedge.exe").Browser, "Edge heard")
	},
	// The shared game holds the mic and is heard anyway; KOOK has a saved "Include" (05 §5.8, §14.6).
	"report-shared-mic.json": func(t *testing.T, r media.AudioReport) {
		v := app(r, "exe:valorant.exe")
		want(t, appIs(r, "exe:valorant.exe", "heard", "") && v.Shared && v.UsesMic &&
			slices.Contains(v.Warnings, "shared_app_uses_mic"), "the shared app heard with shared_app_uses_mic")
		want(t, slices.Contains(app(r, "exe:kook.exe").Warnings, "included_uses_mic"), "KOOK included_uses_mic")
		want(t, len(r.Warnings) == 1 && r.Warnings[0].Code == "multi_process_session", "the report's warning")
	},
}

// TestAudioGoldens: every golden report parses, is in the form above (encoding/json writes it back byte for byte),
// stays inside 05 §12.1's caps and value sets, and shows its situation.
func TestAudioGoldens(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "audio", "report-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	var wantNames []string
	for n := range goldenChecks {
		wantNames = append(wantNames, n)
	}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Fatalf("golden files %v, want %v", names, wantNames)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "audio", name))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.ContainsAny(b, "\r\n") {
				t.Fatal("the file has a line break; the DLL's JSON has none")
			}
			strict := strictDecode(t, b) // every field is one AudioReport knows
			r, err := media.ParseAudioReport(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r, strict) {
				t.Errorf("ParseAudioReport =\n%+v\nwant\n%+v", r, strict)
			}
			if got := encodeReport(t, r); !bytes.Equal(got, b) {
				t.Errorf("not in the golden form: encoding/json writes\n%s", got)
			}
			checkReportValues(t, r)
			goldenChecks[name](t, r)
		})
	}
}

// encodeReport writes r as the golden files hold it: encoding/json without HTML escaping or a final newline.
func encodeReport(t *testing.T, r media.AudioReport) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// The value sets of 05 §12.1.
var (
	reportModes     = []string{"exclude-apps", "off"}
	reportEngines   = []string{"include-set", "exclude-self", "endpoint", "none"}
	reportStates    = []string{"starting", "ok", "degraded", "no-device", "stopped"}
	probeResults    = []string{"pending", "ok", "failed", "unsupported", "skipped"}
	appVerdicts     = []string{"heard", "kept_out", "not_captured", "pending"}
	keptOutReasons  = []string{"builtin", "user", "mic", "viewer", "isshoni"}
	notCaptReasons  = []string{"launcher", "tree", "pid_reuse", "limit", "capture_failed"}
	appWarnings     = []string{"shared_app_uses_mic", "shared_app_kept_out", "included_uses_mic", "multi_process_session"}
	sessionStreams  = []string{"none", "activating", "preroll", "running", "fading"}
	sessionNotes    = []string{"", "covered", "no_handle"}
	reportEventCode = []string{"session_created", "session_expired", "stream_opened", "stream_running",
		"stream_removed", "stream_failed", "activation_failed", "fastpath_enter", "fastpath_exit", "fastpath_late_cut",
		"default_device_changed", "device_added", "device_removed", "no_device", "audiosrv_restart", "probe_ok",
		"probe_failed", "mixer_resync", "underrun", "trim", "silent_reopen", "rules_changed", "muted", "unmuted",
		"mode_changed", "reanchor"}
)

// checkReportValues checks r against 05 §12.1's caps and value sets, and that its rows agree with each other.
func checkReportValues(t *testing.T, r media.AudioReport) {
	t.Helper()
	in := func(what, v string, set []string) {
		t.Helper()
		if !slices.Contains(set, v) {
			t.Errorf("%s %q not in %v", what, v, set)
		}
	}
	in("mode", string(r.Mode), reportModes)
	in("engine", r.Engine, reportEngines)
	in("state", r.State, reportStates)
	in("probe.result", r.Probe.Result, probeResults)
	if len(r.Apps) > 128 || len(r.Sessions) > 256 || len(r.Events) > 300 {
		t.Errorf("%d apps, %d sessions, %d events: over the caps", len(r.Apps), len(r.Sessions), len(r.Events))
	}
	ids := map[string]media.AudioApp{}
	for _, a := range r.Apps {
		if _, dup := ids[a.ID]; dup {
			t.Errorf("app %s twice", a.ID)
		}
		ids[a.ID] = a
		in("verdict", a.Verdict, appVerdicts)
		switch a.Verdict {
		case "kept_out":
			in("kept-out reason", a.Reason, keptOutReasons)
		case "not_captured":
			in("not-captured reason", a.Reason, notCaptReasons)
		default:
			in("reason", a.Reason, []string{""})
		}
		if (a.Detail != "") != (a.Reason == "tree" || a.Reason == "pid_reuse") {
			t.Errorf("app %s: detail %q with reason %q", a.ID, a.Detail, a.Reason)
		}
		for _, w := range a.Warnings {
			in("app warning", w, appWarnings)
		}
		for _, s := range []string{a.ID, a.Name, a.Exe, a.Detail} {
			if len(s) > 260 || !utf8.ValidString(s) {
				t.Errorf("app %s: string %q is over 260 bytes or not UTF-8", a.ID, s)
			}
		}
		if a.Streams > 0 && (a.Verdict != "heard" || !r.Live) {
			t.Errorf("app %s: %d streams while %s, live %v", a.ID, a.Streams, a.Verdict, r.Live)
		}
	}
	for _, s := range r.Sessions {
		a, ok := ids[s.AppID]
		if !ok {
			t.Errorf("session of pid %d: no app %s", s.PID, s.AppID)
			continue
		}
		if s.Verdict != a.Verdict || s.Reason != a.Reason || s.Exe != a.Exe {
			t.Errorf("session of pid %d disagrees with app %s", s.PID, a.ID)
		}
		in("session stream", s.Stream, sessionStreams)
		in("session note", s.Note, sessionNotes)
		in("session flow", s.Flow, []string{"render"})
		if s.Created == "" || s.Created[0] == '-' {
			t.Errorf("session of pid %d: created %q", s.PID, s.Created)
		}
	}
	for _, e := range r.Events {
		in("event code", e.Code, reportEventCode)
		if e.TNanos > r.QPCNanos {
			t.Errorf("event %s after the snapshot", e.Code)
		}
	}
}

// app returns r's row with id (a zero row when there is none).
func app(r media.AudioReport, id string) media.AudioApp {
	for _, a := range r.Apps {
		if a.ID == id {
			return a
		}
	}
	return media.AudioApp{}
}

// appIs reports whether r has a row with id, verdict and reason.
func appIs(r media.AudioReport, id, verdict, reason string) bool {
	a := app(r, id)
	return a.ID == id && a.Verdict == verdict && a.Reason == reason
}

func want(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Errorf("want %s", what)
	}
}

// FuzzParseAudioReport: ParseAudioReport never panics; it returns a v1 report or one of its two errors with a zero
// report; and a report it returns reads back the same after encoding/json writes it (05 §19.1: fuzzed, 60 s).
func FuzzParseAudioReport(f *testing.F) {
	paths, _ := filepath.Glob(filepath.Join("testdata", "audio", "report-*.json"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(reportV1))
	f.Add([]byte(`{"v":1}`))
	f.Add([]byte(`{"v":2,"apps":[]}`))
	f.Add([]byte(`{"v":1,"apps":[null,{"warnings":null}],"sessions":[{"created":"\ud800"}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := media.ParseAudioReport(b)
		if err != nil {
			me, ok := errors.AsType[*media.Error](err)
			if !ok || me.Op != "parse_audio_report" ||
				(me.Code != media.CodeInternal && (me.Code != media.CodeDependency || me.Reason != "abi")) {
				t.Fatalf("error %v", err)
			}
			if !reflect.DeepEqual(r, media.AudioReport{}) {
				t.Fatalf("a report with the error: %+v", r)
			}
			return
		}
		if r.V != 1 {
			t.Fatalf("accepted version %d", r.V)
		}
		again, err := media.ParseAudioReport(encodeReport(t, r))
		if err != nil {
			t.Fatalf("the written report does not parse: %v", err)
		}
		if !reflect.DeepEqual(again, r) {
			t.Fatalf("round trip:\n%+v\nwant\n%+v", again, r)
		}
	})
}
