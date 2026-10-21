package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// TestParseAudioReport: 05 §12.1's example parses into the strict decode's value; unknown fields are ignored (the
// report changes only additively).
func TestParseAudioReport(t *testing.T) {
	want := strictDecode(t, []byte(reportV1))
	r, err := media.ParseAudioReport([]byte(reportV1))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("ParseAudioReport =\n%+v\nwant\n%+v", r, want)
	}
	extended := strings.Replace(reportV1, `"v": 1,`, `"v": 1, "later": {"x": [1, "a"]}, "flags": 3,`, 1)
	extended = strings.Replace(extended, `"pids": 6,`, `"pids": 6, "cpu": 0.5,`, 1)
	if extended == reportV1 {
		t.Fatal("the test document did not change")
	}
	if r, err := media.ParseAudioReport([]byte(extended)); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("with unknown fields: %v, %+v", err, r)
	}
}

// TestParseAudioReportErrors: a document that is not a report is internal, a report of another version is
// dependency with Reason "abi" (05 §13.1: a DLL of another release), and neither error quotes the document.
func TestParseAudioReportErrors(t *testing.T) {
	const secret = "Secret Voice App"
	for _, tc := range []struct {
		name   string
		doc    string
		code   media.ErrorCode
		reason string
	}{
		{"empty", ``, media.CodeInternal, ""},
		{"not JSON", `Secret Voice App`, media.CodeInternal, ""},
		{"truncated", reportV1[:len(reportV1)/2], media.CodeInternal, ""},
		{"trailing data", reportV1 + `{"v": 1}`, media.CodeInternal, ""},
		{"a list", `[{"v": 1}]`, media.CodeInternal, ""},
		{"a wrong type", `{"v": 1, "apps": [{"name": "Secret Voice App", "pids": "Secret Voice App"}]}`,
			media.CodeInternal, ""},
		{"a negative seq", `{"v": 1, "seq": -1}`, media.CodeInternal, ""},
		{"null", `null`, media.CodeDependency, "abi"},
		{"no version", `{"seq": 1, "apps": [{"name": "Secret Voice App"}]}`, media.CodeDependency, "abi"},
		{"version 0", `{"v": 0}`, media.CodeDependency, "abi"},
		{"version 2", strings.Replace(reportV1, `"v": 1,`, `"v": 2,`, 1), media.CodeDependency, "abi"},
	} {
		r, err := media.ParseAudioReport([]byte(tc.doc))
		me, ok := errors.AsType[*media.Error](err)
		if !ok || me.Code != tc.code || me.Reason != tc.reason || me.Op != "parse_audio_report" {
			t.Errorf("%s: ParseAudioReport = %v, want %s (%q) with Op parse_audio_report", tc.name, err, tc.code,
				tc.reason)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Spotify") {
			t.Errorf("%s: the error quotes the document: %v", tc.name, err)
		}
		if !reflect.DeepEqual(r, media.AudioReport{}) {
			t.Errorf("%s: a report %+v with the error", tc.name, r)
		}
	}
	if _, err := media.ParseAudioReport([]byte(`{"v": 2}`)); !errors.Is(err, media.ErrDependency) {
		t.Errorf("version 2: %v, want ErrDependency", err)
	}
}

// TestAudioControllerErrors pins the Go side of 05 §13.1's status → error table: what errors.Is finds for each
// row's error, so every implementation's errors are handled alike.
func TestAudioControllerErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   *media.Error
		is    []error
		isNot []error
	}{
		{"IM_ERR_BAD_STATE", &media.Error{Code: media.CodeAudioNotRunning, Op: "audio_set_muted",
			Detail: "im_audio_set_muted: IM_ERR_BAD_STATE"},
			[]error{media.ErrAudioNotRunning}, []error{media.ErrAudioUnavailable, media.ErrStopped, media.ErrInvalid}},
		{"IM_ERR_INVALID_ARG", &media.Error{Code: media.CodeInvalid, Op: "set_exclusions", Detail: "rule 3: kind"},
			[]error{media.ErrInvalid}, []error{media.ErrAudioNotRunning}},
		{"IM_ERR_TIMEOUT", &media.Error{Code: media.CodeTimeout, Op: "list_audio_apps"},
			[]error{context.DeadlineExceeded}, []error{context.Canceled, media.ErrCancelled}},
		{"IM_ERR_ABI", &media.Error{Code: media.CodeDependency, Reason: "abi", Op: "audio_watch", Detail: "struct_size"},
			[]error{media.ErrDependency}, []error{media.ErrInvalid}},
		{"IM_ERR_AUDIO_UNAVAILABLE", &media.Error{Code: media.CodeAudioUnavailable, Op: "audio_sound_check"},
			[]error{media.ErrAudioUnavailable}, []error{media.ErrAudioNotRunning}},
	} {
		wrapped := fmt.Errorf("audio service: %w", tc.err)
		for _, target := range tc.is {
			if !errors.Is(tc.err, target) || !errors.Is(wrapped, target) {
				t.Errorf("%s: %v is not %v", tc.name, tc.err, target)
			}
		}
		for _, target := range tc.isNot {
			if errors.Is(tc.err, target) {
				t.Errorf("%s: %v is %v", tc.name, tc.err, target)
			}
		}
	}
}

// strictDecode decodes doc as AudioReport with unknown fields disallowed.
func strictDecode(t *testing.T, doc []byte) media.AudioReport {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var r media.AudioReport
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAudioReportFields decodes 05 §12.1's example strictly: every field the DLL writes has a field with its name in
// AudioReport (the json tags are the DLL's camelCase names), and every field of AudioReport and of the first element
// of each list is filled from it.
func TestAudioReportFields(t *testing.T) {
	r := strictDecode(t, []byte(reportV1))
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
