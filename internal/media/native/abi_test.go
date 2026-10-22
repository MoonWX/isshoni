package native

import (
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// The C sources this package mirrors.
const (
	headerFile   = "../../../native/include/isshoni_media.h"
	statusHeader = "../../../native/common/include/im/abi/status.h"
)

// abiConstants maps every #define of the header to its Go mirror in abi.go.
var abiConstants = map[string]int64{
	"IM_ABI_MAJOR": imABIMajor, "IM_ABI_MINOR": imABIMinor, "IM_ABI_VERSION": imABIVersion,

	"IM_OK": imOK, "IM_TIMEOUT": imTimeout, "IM_ENDED": imEnded,
	"IM_ERR_INVALID_ARG": imErrInvalidArg, "IM_ERR_BAD_STATE": imErrBadState, "IM_ERR_ABI": imErrABI,
	"IM_ERR_UNSUPPORTED_OS": imErrUnsupportedOS, "IM_ERR_PERMISSION_DENIED": imErrPermissionDenied,
	"IM_ERR_AUDIO_DEVICE_CHANGED": imErrAudioDeviceChanged, "IM_ERR_SOURCE_GONE": imErrSourceGone,
	"IM_ERR_ENCODER_LOST": imErrEncoderLost, "IM_ERR_NO_ENCODER": imErrNoEncoder, "IM_ERR_CANCELLED": imErrCancelled,
	"IM_ERR_LIMIT": imErrLimit, "IM_ERR_DEPENDENCY": imErrDependency, "IM_ERR_AUDIO_UNAVAILABLE": imErrAudioUnavailable,
	"IM_ERR_NOT_IMPLEMENTED": imErrNotImplemented, "IM_ERR_TIMEOUT": imErrTimeout, "IM_ERR_INTERNAL": imErrInternal,

	"IM_LOG_ERROR": imLogError, "IM_LOG_WARN": imLogWarn, "IM_LOG_INFO": imLogInfo, "IM_LOG_DEBUG": imLogDebug,
	"IM_INIT_NO_AUDIO": imInitNoAudio,

	"IM_MAX_ADAPTERS": imMaxAdapters, "IM_MAX_ENCODERS": imMaxEncoders,
	"IM_ENC_AUTO": imEncAuto, "IM_ENC_NVENC": imEncNVENC, "IM_ENC_QSV": imEncQSV, "IM_ENC_AMF": imEncAMF,
	"IM_ENC_MF_HW": imEncMFHW, "IM_ENC_MF_SW": imEncMFSW, "IM_ENC_OPENH264": imEncOpenH264,
	"IM_ENC_VIDEOTOOLBOX": imEncVideoToolbox, "IM_ENC_GSTREAMER": imEncGStreamer, "IM_ENC_FAKE": imEncFake,
	"IM_ENCF_AVAILABLE": imEncfAvailable, "IM_ENCF_HARDWARE": imEncfHardware, "IM_ENCF_LIVE_BITRATE": imEncfLiveBitrate,
	"IM_ENCF_ZERO_COPY": imEncfZeroCopy, "IM_ENCF_HIGH": imEncfHigh, "IM_ENCF_CB": imEncfCB,
	"IM_ENCF_FORCED_IDR":  imEncfForcedIDR,
	"IM_ADAPTER_SOFTWARE": imAdapterSoftware, "IM_ADAPTER_HAS_OUTPUTS": imAdapterHasOutputs,
	"IM_OS_SUPPORTED": imOSSupported, "IM_OS_WGC": imOSWGC, "IM_OS_WGC_PICKER": imOSWGCPicker,
	"IM_OS_WGC_BORDERLESS": imOSWGCBorderless, "IM_OS_WGC_MIN_INTERVAL": imOSWGCMinInterval,
	"IM_OS_WGC_SECONDARY_WINDOWS": imOSWGCSecondaryWindows, "IM_OS_WGC_CURSOR_TOGGLE": imOSWGCCursorToggle,
	"IM_OS_DESKTOP_DUPLICATION": imOSDesktopDuplication, "IM_OS_EXCLUDE_FROM_CAPTURE": imOSExcludeFromCapture,
	"IM_OS_MEDIA_FOUNDATION": imOSMediaFoundation, "IM_OS_HDR_DISPLAY": imOSHDRDisplay,
	"IM_AUDIO_PROCESS_LOOPBACK": imAudioProcessLoopback, "IM_AUDIO_ENDPOINT_LOOPBACK": imAudioEndpointLoopback,
	"IM_AUDIO_SESSION_EVENTS": imAudioSessionEvents, "IM_AUDIO_MIC_DETECTION": imAudioMicDetection,
	"IM_PROBE_QUICK": imProbeQuick, "IM_PROBE_FULL": imProbeFull,

	"IM_SOURCE_MONITOR": imSourceMonitor, "IM_SOURCE_WINDOW": imSourceWindow,
	"IM_SOURCE_TEST_PATTERN": imSourceTestPattern,
	"IM_CAPTURE_WGC":         imCaptureWGC, "IM_CAPTURE_DD": imCaptureDD, "IM_CAPTURE_TEST": imCaptureTest,
	"IM_CAPTURE_SCK": imCaptureSCK, "IM_CAPTURE_PORTAL": imCapturePortal, "IM_CAPTURE_X11": imCaptureX11,
	"IM_SRCF_OS_BORDER": imSrcfOSBorder, "IM_SRCF_SELF": imSrcfSelf, "IM_SRCF_MAPPED": imSrcfMapped,

	"IM_AUDIO_SAMPLE_RATE": imAudioSampleRate, "IM_AUDIO_CHANNELS": imAudioChannels, "IM_MAX_RULES": imMaxRules,
	"IM_MAX_RULE_VALUE": imMaxRuleValue, "IM_APP_ID_LEN": imAppIDLen, "IM_MAX_LEVEL_APPS": imMaxLevelApps,
	"IM_AUDIO_MODE_OFF": imAudioModeOff, "IM_AUDIO_MODE_EXCLUDE_APPS": imAudioModeExcludeApps,
	"IM_AUDIO_MODE_EVERYTHING": imAudioModeEverything, "IM_AUDIO_MODE_TEST_TONE": imAudioModeTestTone,
	"IM_RULE_APP_EXCLUDE": imRuleAppExclude, "IM_RULE_APP_ALLOW": imRuleAppAllow, "IM_RULE_INSTANCE": imRuleInstance,
	"IM_RULE_STOP": imRuleStop, "IM_RULE_SHARED_RUNTIME": imRuleSharedRuntime, "IM_RULE_BROWSER": imRuleBrowser,
	"IM_RULE_WEBVIEW_DATA_DIR": imRuleWebViewDataDir,
	"IM_RULE_SRC_BUILTIN":      imRuleSrcBuiltin, "IM_RULE_SRC_USER": imRuleSrcUser, "IM_RULE_SRC_SELF": imRuleSrcSelf,
	"IM_RULE_SRC_VIEWER": imRuleSrcViewer, "IM_RULE_SRC_DATA": imRuleSrcData,
	"IM_EXCLF_KEEP_OUT_MIC_USERS": imExclfKeepOutMicUsers, "IM_EXCLF_FAST_PATH": imExclfFastPath,
	"IM_AUDIOF_TAP_PCM": imAudiofTapPCM, "IM_AUDIOF_TEST_FAIL_PROBE": imAudiofTestFailProbe,

	"IM_MAX_LAYERS": imMaxLayers, "IM_ALL_LAYERS": imAllLayers, "IM_NO_LAYER": imNoLayer,
	"IM_PROFILE_HIGH": imProfileHigh, "IM_PROFILE_CB": imProfileCB,
	"IM_CONTENT_AUTO": imContentAuto, "IM_CONTENT_MOTION": imContentMotion, "IM_CONTENT_DETAIL": imContentDetail,
	"IM_LAYER_INACTIVE":    imLayerInactive,
	"IM_START_HIDE_CURSOR": imStartHideCursor, "IM_START_PAUSED": imStartPaused,
	"IM_START_ALLOW_LISTED_SOURCE": imStartAllowListedSource, "IM_START_KEEP_BORDER": imStartKeepBorder,
	"IM_SESS_NO_OS_INDICATOR": imSessNoOSIndicator, "IM_SESS_AUDIO": imSessAudio,
	"IM_AUDIO_STATE_OFF": imAudioStateOff, "IM_AUDIO_STATE_STARTING": imAudioStateStarting,
	"IM_AUDIO_STATE_OK": imAudioStateOK, "IM_AUDIO_STATE_DEGRADED": imAudioStateDegraded,
	"IM_AUDIO_STATE_NO_DEVICE": imAudioStateNoDevice,

	"IM_PACKET_VIDEO": imPacketVideo, "IM_PACKET_AUDIO": imPacketAudio, "IM_PACKET_EVENT": imPacketEvent,
	"IM_PACKET_PCM": imPacketPCM, "IM_PACKET_DEBUG": imPacketDebug,
	"IM_PKT_KEYFRAME": imPktKeyframe, "IM_PKT_REINIT": imPktReinit, "IM_PKT_REFRESH": imPktRefresh,
	"IM_PKT_DISCONTINUITY": imPktDiscontinuity,
	"IM_EVENT_ENDED":       imEventEnded, "IM_EVENT_SOURCE_RESIZED": imEventSourceResized,
	"IM_EVENT_SOURCE_MINIMIZED": imEventSourceMinimized, "IM_EVENT_SOURCE_RESTORED": imEventSourceRestored,
	"IM_EVENT_CAPTURE_INTERRUPTED": imEventCaptureInterrupted, "IM_EVENT_CAPTURE_RESUMED": imEventCaptureResumed,
	"IM_EVENT_ENCODER_CHANGED": imEventEncoderChanged, "IM_EVENT_DEVICE_RESET": imEventDeviceReset,
	"IM_EVENT_LIMIT_CHANGED": imEventLimitChanged, "IM_EVENT_RING_OVERFLOW": imEventRingOverflow,

	"IM_PROTECT_OFF": imProtectOff,
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHeaderConstants: abi.go mirrors every #define of isshoni_media.h with its value, and nothing else.
func TestHeaderConstants(t *testing.T) {
	define := regexp.MustCompile(`(?m)^#define\s+(IM_\w+)\s+(.+?)\s*(?:/\*.*)?$`)
	seen := map[string]bool{}
	for _, m := range define.FindAllStringSubmatch(readSource(t, headerFile), -1) {
		name, value := m[1], m[2]
		if name == "IM_API" || name == "IM_CALL" {
			continue
		}
		seen[name] = true
		goValue, ok := abiConstants[name]
		if !ok {
			t.Errorf("%s has no Go mirror in abi.go", name)
			continue
		}
		if name == "IM_ABI_VERSION" { // ((IM_ABI_MAJOR << 16) | IM_ABI_MINOR)
			if goValue != abiConstants["IM_ABI_MAJOR"]<<16|abiConstants["IM_ABI_MINOR"] {
				t.Errorf("IM_ABI_VERSION = %#x in Go", goValue)
			}
			continue
		}
		want, err := strconv.ParseInt(strings.TrimSuffix(value, "u"), 0, 64)
		if err != nil {
			t.Errorf("%s: can't read the header's value %q: %v", name, value, err)
			continue
		}
		if goValue != want {
			t.Errorf("%s = %d in Go, %d in the header", name, goValue, want)
		}
	}
	for name := range abiConstants {
		if !seen[name] {
			t.Errorf("abi.go mirrors %s, which the header does not define", name)
		}
	}
	if len(seen) < 143 { // v1.0's count

		t.Errorf("only %d defines found in %s: is the pattern still right?", len(seen), headerFile)
	}
	if media.ABIVersion != imABIMajor {
		t.Errorf("media.ABIVersion %d, the header's major %d", media.ABIVersion, imABIMajor)
	}
}

// TestProcsAreTheHeaderExports: the export table is exactly the header's IM_API functions, in header order (the
// list isshoni-media.def must match too, WA11's exports_test).
func TestProcsAreTheHeaderExports(t *testing.T) {
	export := regexp.MustCompile(`IM_API\s+[\w\s\*]+?\bIM_CALL\s+(im_\w+)\s*\(`)
	var header []string
	for _, m := range export.FindAllStringSubmatch(readSource(t, headerFile), -1) {
		header = append(header, m[1])
	}
	if !slices.Equal(header, procNames[:]) {
		t.Errorf("procNames differ from the header's IM_API functions\n go:     %v\n header: %v", procNames, header)
	}
	if len(header) != 39 {
		t.Errorf("%d exports in the header; v1.0 has 39", len(header))
	}
	for p := range procCount {
		if p.String() != procNames[p] {
			t.Errorf("proc %d prints %q", p, p.String())
		}
	}
	if s := procCount.String(); s != "proc(39)" {
		t.Errorf("an unknown proc prints %q", s)
	}
}

// TestDetailTokensMatchNative: the Go token list is native/common's (04 §5.7), in the same order.
func TestDetailTokensMatchNative(t *testing.T) {
	block := readSource(t, statusHeader)
	start := strings.Index(block, "namespace token {")
	end := strings.Index(block, "} // namespace token")
	if start < 0 || end < start {
		t.Fatalf("%s: no token namespace", statusHeader)
	}
	tok := regexp.MustCompile(`std::string_view k\w+ = "([a-z_]+)";`)
	var native []string
	for _, m := range tok.FindAllStringSubmatch(block[start:end], -1) {
		native = append(native, m[1])
	}
	if !slices.Equal(native, detailTokens[:]) {
		t.Errorf("tokens differ\n go:     %v\n native: %v", detailTokens, native)
	}
}

// TestEncoderTokens: 04 §5.6's table, both ways.
func TestEncoderTokens(t *testing.T) {
	want := map[uint32]string{
		imEncNVENC: "nvenc", imEncQSV: "qsv", imEncAMF: "amf", imEncMFHW: "mf-hw", imEncMFSW: "mf-sw",
		imEncOpenH264: "openh264", imEncFake: "fake", imEncVideoToolbox: "videotoolbox", imEncGStreamer: "gstreamer",
	}
	for kind, token := range want {
		if got, ok := encoderToken(kind); !ok || got != token {
			t.Errorf("encoderToken(%d) = %q, %v; want %q", kind, got, ok, token)
		}
		if got, ok := encoderKind(token); !ok || got != kind {
			t.Errorf("encoderKind(%q) = %d, %v; want %d", token, got, ok, kind)
		}
	}
	if len(encoderTokens) != len(want) {
		t.Errorf("%d encoder tokens, want %d", len(encoderTokens), len(want))
	}
	if got, ok := encoderKind(""); !ok || got != imEncAuto {
		t.Errorf(`encoderKind("") = %d, %v; want IM_ENC_AUTO`, got, ok)
	}
	if _, ok := encoderToken(imEncAuto); ok {
		t.Error("IM_ENC_AUTO has a token")
	}
	if _, ok := encoderToken(42); ok {
		t.Error("an unknown kind has a token")
	}
	if _, ok := encoderKind("h264_nvenc"); ok {
		t.Error("an FFmpeg codec name is a token (it is diagnostic only, 04 §5.6)")
	}
}

func TestCBytes(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
	}{
		{nil, ""},
		{[]byte{0, 'x'}, ""},
		{[]byte("f\x00\x00\x00"), "f"},
		{[]byte("nvenc"), "nvenc"}, // no NUL: the whole field
	} {
		if got := cBytes(c.in); got != c.want {
			t.Errorf("cBytes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQPCToNS: 04 §12.3's formula, also where q × 1e9 would overflow.
func TestQPCToNS(t *testing.T) {
	for _, c := range []struct{ q, f, want int64 }{
		{0, 10_000_000, 0},
		{1, 10_000_000, 100},
		{12_345_678, 10_000_000, 1_234_567_800},
		{3_579_545, 3_579_545, 1_000_000_000},
		// The ACPI PM timer's frequency: truncated, as the engine does.
		{3_579_546, 3_579_545, 1_000_000_279},
		{24 * 3600 * 10_000_000 * 365, 10_000_000, 24 * 3600 * 1e9 * 365}, // a year of uptime
		// q × 1e9 is far beyond int64 here; the result, 285 years of uptime, is not.
		{90_000_000_000_000_123, 10_000_000, 9_000_000_000_000_012_300},
	} {
		if got := qpcToNS(c.q, c.f); got != c.want {
			t.Errorf("qpcToNS(%d, %d) = %d, want %d", c.q, c.f, got, c.want)
		}
	}
	if q := int64(90_000_000_000_000_123); q <= math.MaxInt64/1_000_000_000 {
		t.Error("the large case no longer overflows q × 1e9")
	}
}
