package media_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestAudioConstants pins the values that cross the C ABI (05 §4.1: IM_RULE_*, IM_RULE_SRC_*) and the mode strings
// of 05 §13.1.
func TestAudioConstants(t *testing.T) {
	for kind, want := range map[media.RuleKind]uint32{
		media.RuleAppExclude: 0, media.RuleAppAllow: 1, media.RuleInstance: 2, media.RuleStop: 3,
		media.RuleSharedRuntime: 4, media.RuleBrowser: 5, media.RuleWebViewDataDir: 6,
	} {
		if uint32(kind) != want {
			t.Errorf("rule kind %d, want %d", kind, want)
		}
	}
	for src, want := range map[media.RuleSource]uint32{
		media.SourceBuiltin: 0, media.SourceUser: 1, media.SourceSelf: 2, media.SourceViewer: 3, media.SourceData: 4,
	} {
		if uint32(src) != want {
			t.Errorf("rule source %d, want %d", src, want)
		}
	}
	for mode, want := range map[media.AudioMode]string{
		media.AudioExcludeApps: "exclude-apps", media.AudioOff: "off", media.AudioEverything: "everything",
		media.AudioTestTone: "test-tone",
	} {
		if string(mode) != want {
			t.Errorf("mode %q, want %q", mode, want)
		}
	}
}

// TestParseAudioReportNotImplemented: until WA26, ParseAudioReport says so with an internal *media.Error.
func TestParseAudioReportNotImplemented(t *testing.T) {
	r, err := media.ParseAudioReport([]byte(reportV1))
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeInternal || me.Reason != "not_implemented" ||
		me.Op != "parse_audio_report" {
		t.Fatalf("ParseAudioReport = %v, want an internal not_implemented *media.Error", err)
	}
	if !reflect.DeepEqual(r, media.AudioReport{}) {
		t.Errorf("ParseAudioReport returned %+v with its error", r)
	}
	if errors.Is(err, media.ErrInvalid) {
		t.Error("not implemented is not invalid input")
	}
}

// TestAudioReportFields decodes 05 §12.1's example strictly: every field the DLL writes has a field with its name in
// AudioReport (the json tags are the DLL's camelCase names), and every field of AudioReport and of the first element
// of each list is filled from it.
func TestAudioReportFields(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader([]byte(reportV1)))
	dec.DisallowUnknownFields()
	var r media.AudioReport
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	checkFilled(t, "AudioReport", reflect.ValueOf(r))
	if r.Seq != 118 || r.QPCNanos != 81234567890123 || r.Mode != media.AudioExcludeApps || r.Probe.LevelDB != -60.1 ||
		r.Sessions[0].Created != "133725183442731234" || r.Counters.AudioCPUMsPerS != 6.2 || len(r.Apps) != 2 ||
		r.Apps[1].ID != "discord" || r.Events[0].TNanos != 81234560000000 {
		t.Errorf("report %+v", r)
	}
	if reflect.TypeFor[media.AudioStatus]() != reflect.TypeFor[media.AudioReport]() {
		t.Error("AudioStatus, 01's name for the report, is not AudioReport")
	}
}

// checkFilled fails for every zero field of v, a struct, looking into structs and the first element of slices.
func checkFilled(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			checkFilled(t, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty", path)
			return
		}
		checkFilled(t, path+"[0]", v.Index(0))
	default:
		if v.IsZero() {
			t.Errorf("%s is not filled", path)
		}
	}
}

// reportV1 is 05 §12.1's example, shortened to two apps, one session and one event, with values where the example
// has zeros, false or "" so that TestAudioReportFields sees every field arrive.
const reportV1 = `{
  "v": 1, "seq": 118, "qpcNs": 81234567890123, "mode": "exclude-apps", "engine": "include-set", "state": "ok",
  "muted": true, "paused": true, "live": true, "rebuilding": true,
  "probe": {"result": "ok", "osBuild": 26100, "format": "float32", "levelDb": -60.1, "ms": 231},
  "device": {"present": true, "renderEndpoints": 2, "captureEndpoints": 1},
  "apps": [
    {"id": "exe:launcherx.exe", "name": "LauncherX", "exe": "launcherx.exe", "verdict": "not_captured",
     "reason": "tree", "playing": true, "usesMic": true, "browser": true, "shared": true, "pids": 1, "sessions": 1,
     "streams": 1, "detail": "Discord", "warnings": ["shared_app_uses_mic"]},
    {"id": "discord", "name": "Discord", "exe": "discord.exe", "verdict": "kept_out", "reason": "builtin",
     "playing": true, "usesMic": true, "browser": false, "shared": false, "pids": 6, "sessions": 2,
     "streams": 0, "detail": "", "warnings": []}
  ],
  "sessions": [
    {"pid": 4120, "created": "133725183442731234", "exe": "spotify.exe", "appId": "exe:spotify.exe",
     "flow": "render", "state": "active", "multiProcess": true, "verdict": "kept_out", "reason": "user",
     "stream": "running", "format": "float32", "note": "covered"}
  ],
  "warnings": [{"code": "multi_process_session", "app": "exe:foo.exe"}],
  "counters": {"streams": 7, "activations": 9, "activationFailures": 1, "underruns": 2, "trims": 3,
    "overflows": 4, "discontinuities": 5, "mixerResyncs": 6, "fastpathExits": 7,
    "fastpathLateCuts": 8, "silentReopens": 10, "deviceChanges": 11, "pidReuseBlocks": 13, "maxResamplePpm": 41,
    "audioCpuMsPerS": 6.2},
  "events": [{"tNs": 81234560000000, "code": "stream_running", "app": "exe:spotify.exe", "detail": "d"}]
}`

// TestSoundCheckResultJSON pins SoundCheckResult's JSON names (05 §13.1), which the app's bridge passes on.
func TestSoundCheckResultJSON(t *testing.T) {
	b, err := json.Marshal(media.SoundCheckResult{Captured: true, LevelDB: -14, Millis: 1500, Backend: "endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"captured":true,"levelDb":-14,"ms":1500,"backend":"endpoint"}`; string(b) != want {
		t.Errorf("JSON %s, want %s", b, want)
	}
}
