package media

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// This file is 05's (docs/m2/05-windows-audio.md §13.1): the OS-neutral audio types and the AudioController
// interface. The Opus bitrate is set with Session.SetBitrate(LayerAudio, bps); this file declares no layer constant.

// AudioMode is how an audio session picks what friends hear.
type AudioMode string

// The audio modes. M2 exposes no sound-mode choice: AudioExcludeApps is the mode of every share, and "Share sound"
// off is SetAudioMode(AudioOff) (docs/m2/README.md §3 item 15).
const (
	// AudioExcludeApps is IM_AUDIO_MODE_EXCLUDE_APPS: the M2 mode.
	AudioExcludeApps AudioMode = "exclude-apps"
	// AudioOff is IM_AUDIO_MODE_OFF: a SetAudioMode value only ("Share sound" off).
	AudioOff AudioMode = "off"
	// AudioEverything is IM_AUDIO_MODE_EVERYTHING: reserved for later; rejected in M2 with CodeInvalid, both in
	// Config.Audio at start and in SetAudioMode, without a DLL call.
	AudioEverything AudioMode = "everything"
	// AudioTestTone is IM_AUDIO_MODE_TEST_TONE (100, 04 §10.3): an AudioConfig.Mode at start only, for tests,
	// mediatest.Conformance and enginetest; no capture. It is the one carrier of the test tone.
	AudioTestTone AudioMode = "test-tone"
)

// RuleKind is a Rule's kind. Its values are the header's IM_RULE_*.
type RuleKind uint32

// The rule kinds.
const (
	RuleAppExclude RuleKind = iota
	RuleAppAllow
	RuleInstance
	RuleStop
	RuleSharedRuntime
	RuleBrowser
	RuleWebViewDataDir
)

// RuleSource is where a Rule comes from. Its values are the header's IM_RULE_SRC_*.
type RuleSource uint32

// The rule sources.
const (
	SourceBuiltin RuleSource = iota
	SourceUser
	SourceSelf
	SourceViewer
	SourceData
)

// Rule is one exclusion rule (05 §5.3). Rules are local data: never logged, never sent.
type Rule struct {
	Kind   RuleKind
	Source RuleSource
	Value  string // exe name, pid or path
	AppID  string // "" = "exe:" + Value
	Name   string // "" = from the exe
}

// Exclusions is the rule set an audio session or the watcher applies (05 §5, §6).
type Exclusions struct {
	KeepOutMicUsers bool
	FastPath        bool
	Rules           []Rule // at most 512
	SharedPID       uint32 // 0 = none
}

// AudioConfig is used as Config.Audio: nil = no audio (IM_AUDIO_MODE_OFF at im_start). One type, declared here; 01
// declares no AudioConfig of its own.
type AudioConfig struct {
	BitrateBPS int        // Opus target in bit/s; the core fills it from ShareParams.AudioBitrate (0 = 128000)
	Exclusions Exclusions // from audio.Service.AudioConfig() (05 §14.2)
	// Mode "" is AudioExcludeApps; AudioTestTone is 04's tone source (the one carrier of the test tone). AudioOff,
	// and AudioEverything (reserved), are rejected in M2 (CodeInvalid).
	Mode      AudioMode
	TapPCM    bool   // audiotest only: IM_AUDIOF_TAP_PCM (Session.Next then also returns KindPCM packets)
	TestFlags uint32 // audiotest only: IM_AUDIOF_TEST_FAIL_PROBE
}

// AudioReport is "What friends hear": im_list_audio_apps' JSON v1 (05 §12.1), with the field names the DLL writes.
// Parse it with ParseAudioReport, which rejects v != 1 and ignores unknown fields (additive changes only). Seq
// increases whenever anything except levels, Counters, QPCNanos and Events changes, so a caller that wants to know
// whether the report changed compares Seq.
type AudioReport struct {
	V        int       `json:"v"`
	Seq      uint32    `json:"seq"`
	QPCNanos int64     `json:"qpcNs"`  // engine clock of the snapshot
	Mode     AudioMode `json:"mode"`   // "exclude-apps" | "off"; "everything" is reserved
	Engine   string    `json:"engine"` // "include-set" | "exclude-self" | "endpoint" | "none"
	// State is "starting" (probe running), "ok", "degraded" (probe failed or unsupported), "no-device" or "stopped"
	// (watcher not running: a one-shot snapshot).
	State      string           `json:"state"`
	Muted      bool             `json:"muted"`  // the im_audio_set_muted flag (05 §9.6)
	Paused     bool             `json:"paused"` // the pause flag (im_set_paused, 05 §9.6)
	Live       bool             `json:"live"`
	Rebuilding bool             `json:"rebuilding"`
	Probe      AudioProbeResult `json:"probe"`
	Device     AudioDeviceState `json:"device"`
	Apps       []AudioApp       `json:"apps"`     // at most 128 rows
	Sessions   []AudioSession   `json:"sessions"` // at most 256
	Warnings   []AudioWarning   `json:"warnings"`
	Counters   AudioCounters    `json:"counters"` // the mixer's; drops after the mixer are in EngineStats
	Events     []AudioEvent     `json:"events"`   // the last 300
}

// AudioStatus is 01's name for "What friends hear" (Session.AudioStatus, core events).
type AudioStatus = AudioReport

// AudioProbeResult is the report's startup probe (05 §11.2).
type AudioProbeResult struct {
	Result  string  `json:"result"` // "pending" | "ok" | "failed" | "unsupported" | "skipped"
	OSBuild int     `json:"osBuild"`
	Format  string  `json:"format"`
	LevelDB float64 `json:"levelDb"`
	Millis  int     `json:"ms"`
}

// AudioDeviceState is the report's endpoint summary.
type AudioDeviceState struct {
	Present          bool `json:"present"`
	RenderEndpoints  int  `json:"renderEndpoints"`
	CaptureEndpoints int  `json:"captureEndpoints"`
}

// AudioApp is one apps[] row of 05 §12.1. 01 relies on ID, Name and UsesMic. The fields are in the order the DLL
// writes them (05 §12.1), so encoding/json writes a report in the order of the golden files.
type AudioApp struct {
	ID      string `json:"id"`   // the app id of the rule that matched ("discord"), else "exe:" + the app root's exe
	Name    string `json:"name"` // local only: never logged, never sent
	Exe     string `json:"exe"`
	Verdict string `json:"verdict"` // "heard" | "kept_out" | "not_captured" | "pending"
	// Reason is, when kept out: "builtin", "user", "mic", "viewer", "isshoni"; when not captured: "launcher",
	// "tree", "pid_reuse", "limit", "capture_failed"; when heard: "".
	Reason   string `json:"reason"`
	Playing  bool   `json:"playing"` // a session active, or its meter above −70 dBFS, within the last 2 s
	UsesMic  bool   `json:"usesMic"`
	Browser  bool   `json:"browser"`
	Shared   bool   `json:"shared"`
	PIDs     int    `json:"pids"`
	Sessions int    `json:"sessions"`
	Streams  int    `json:"streams"`
	Detail   string `json:"detail"` // for "tree" and "pid_reuse": the kept-out app's name; else ""
	// Warnings are "shared_app_uses_mic", "shared_app_kept_out", "included_uses_mic" and "multi_process_session".
	Warnings []string `json:"warnings"`
}

// AudioSession is one sessions[] row of 05 §12.1: an audio session of a process on an endpoint.
type AudioSession struct {
	PID          uint32 `json:"pid"`
	Created      string `json:"created"` // FILETIME as a decimal string (JSON numbers lose precision above 2^53)
	Exe          string `json:"exe"`
	AppID        string `json:"appId"`
	Flow         string `json:"flow"`
	State        string `json:"state"`
	MultiProcess bool   `json:"multiProcess"`
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason"`
	Stream       string `json:"stream"` // "none" | "activating" | "preroll" | "running" | "fading"
	Format       string `json:"format"`
	Note         string `json:"note"` // "" | "covered" | "no_handle"
}

// AudioWarning is one warnings[] entry of 05 §12.1.
type AudioWarning struct {
	Code string `json:"code"`
	App  string `json:"app"`
}

// AudioCounters are the mixer's counters of 05 §12.1.
type AudioCounters struct {
	Streams            int     `json:"streams"`
	Activations        int     `json:"activations"`
	ActivationFailures int     `json:"activationFailures"`
	Underruns          int     `json:"underruns"`
	Trims              int     `json:"trims"`
	Overflows          int     `json:"overflows"`
	Discontinuities    int     `json:"discontinuities"`
	MixerResyncs       int     `json:"mixerResyncs"`
	FastpathExits      int     `json:"fastpathExits"`
	FastpathLateCuts   int     `json:"fastpathLateCuts"`
	SilentReopens      int     `json:"silentReopens"`
	DeviceChanges      int     `json:"deviceChanges"`
	PIDReuseBlocks     int     `json:"pidReuseBlocks"`
	MaxResamplePPM     int     `json:"maxResamplePpm"`
	AudioCPUMsPerS     float64 `json:"audioCpuMsPerS"`
}

// AudioEvent is one events[] entry of 05 §12.1.
type AudioEvent struct {
	TNanos int64  `json:"tNs"` // engine clock
	Code   string `json:"code"`
	App    string `json:"app"`
	Detail string `json:"detail"`
}

// reportVersion is the version of im_list_audio_apps' JSON that this package reads (05 §12.1: "v": 1).
const reportVersion = 1

// ParseAudioReport parses im_list_audio_apps' JSON (05 §12.1). It ignores unknown fields (the report changes only
// additively) and rejects every version but 1. Its errors are *Error with Op "parse_audio_report": a document that is
// not a report is internal; a report of another version is dependency with Reason "abi", like every other mismatch
// between the binding and the DLL (an installation that mixes two releases gets 03's reinstall text). An error never
// quotes the document, which names apps (docs/m2/README.md §4, "Logging").
//
// The golden reports in testdata/audio (05 §13.1) are parsed by this package's tests and written byte for byte by
// the C++ report writer's tests, so the two sides can't drift.
func ParseAudioReport(b []byte) (AudioReport, error) {
	const op = "parse_audio_report"
	var r AudioReport
	if err := json.Unmarshal(b, &r); err != nil {
		return AudioReport{}, &Error{Code: CodeInternal, Op: op, Detail: "the report is not JSON: " + err.Error()}
	}
	if r.V != reportVersion {
		return AudioReport{}, &Error{Code: CodeDependency, Op: op, Reason: "abi",
			Detail: fmt.Sprintf("report version %d, want %d", r.V, reportVersion)}
	}
	return r, nil
}

// AudioLevel is one app's level (05 §12.2).
type AudioLevel struct {
	AppID         string
	PeakDB, RMSDB float32
	Captured      bool // source 0: captured, post-fade, what friends hear; else the session's activity meter
}

// AudioLevels are the levels of the mix and of each app (im_audio_levels, 05 §12.2).
type AudioLevels struct {
	ReportSeq           uint32
	QPCNanos            int64 // engine clock of the sample (im_audio_levels.qpc_ns)
	MixPeakDB, MixRMSDB float32
	Apps                []AudioLevel // at most 64, most recently active first
}

// SoundCheckResult is the outcome of AudioController.SoundCheck (05 §11.3).
type SoundCheckResult struct {
	Captured bool    `json:"captured"`
	LevelDB  float64 `json:"levelDb"`
	Millis   int     `json:"ms"`
	Backend  string  `json:"backend"` // "process-loopback" | "endpoint" | "none"
	Reason   string  `json:"reason,omitempty"`
}

// AudioController is the audio surface of an engine. The Windows engine (the value native.Open returns) and
// *fake.Engine implement it (M2); the macOS (M3) and Linux (M4) engines will. Callers get it with a type assertion,
// eng.(media.AudioController). Every method is safe for concurrent use. WatchAudio, UnwatchAudio, SetExclusions,
// AudioReport, AudioLevels and SoundCheck work without a Session; SetAudioMode and SetAudioMuted need a running
// Session with audio (else ErrAudioNotRunning).
//
// Its errors are 05 §13.1's status → error table, the same from every implementation (the Windows binding maps the
// DLL's statuses, the fake makes them itself), so callers handle one set of errors whatever the engine. Each is an
// *Error whose Op is the C export's name without "im_" ("audio_watch", "audio_unwatch", "set_exclusions",
// "audio_set_mode", "audio_set_muted", "list_audio_apps", "audio_levels_get", "audio_sound_check"), and errors.Is
// matches it by its code:
//
//	Native status, from                          Go error                                   errors.Is target
//	IM_ERR_BAD_STATE: im_audio_levels_get,       {CodeAudioNotRunning, Op, Detail}          ErrAudioNotRunning
//	  im_audio_set_mode, im_audio_set_muted,
//	  any audio call after IM_INIT_NO_AUDIO
//	IM_ERR_INVALID_ARG: im_set_exclusions,       {CodeInvalid, Op, Detail: im_last_error}   ErrInvalid
//	  im_audio_watch, im_audio_set_mode
//	IM_ERR_TIMEOUT (−15): the COM wait of        {CodeTimeout, Op}; never retried           context.DeadlineExceeded
//	  im_list_audio_apps, im_audio_sound_check
//	IM_ERR_ABI: a struct the DLL refuses         {CodeDependency, Reason "abi", Op, Detail} ErrDependency
//	others                                       01 §5.2's mapping (audio_unavailable, internal, …)
//
// IM_ERR_BAD_STATE from every other call of the engine is internal (04 §12.2); the first row is the audio exception.
// Mode validation needs no call and comes first: AudioConfig.Mode at start accepts "", AudioExcludeApps and
// AudioTestTone, and SetAudioMode accepts AudioOff and AudioExcludeApps; any other mode (AudioEverything included,
// docs/m2/README.md §3 item 15) is CodeInvalid, also when no session runs. Exclusions that the DLL refuses (more
// than 512 rules, an unknown kind or source, an empty value, a malformed pid, an app id outside [a-z0-9:._-]{1,63},
// a NUL byte) are CodeInvalid, with a Detail that names the rule's index, never its value. A report that the binding
// can't read is ParseAudioReport's error.
type AudioController interface {
	WatchAudio(ctx context.Context, ex Exclusions) error
	UnwatchAudio(ctx context.Context) error
	SetExclusions(ctx context.Context, ex Exclusions) error
	SetAudioMode(ctx context.Context, m AudioMode) error // M2: AudioOff <-> AudioExcludeApps only
	SetAudioMuted(ctx context.Context, muted bool) error
	AudioReport(ctx context.Context) (AudioReport, error)
	AudioLevels(ctx context.Context) (AudioLevels, error)
	SoundCheck(ctx context.Context, helperPID uint32, timeout time.Duration) (SoundCheckResult, error)
}

// ErrAudioNotRunning is the sentinel of CodeAudioNotRunning: neither watching nor live (IM_ERR_BAD_STATE from an
// audio call, 05 §13.1).
var ErrAudioNotRunning = &Error{Code: CodeAudioNotRunning}
