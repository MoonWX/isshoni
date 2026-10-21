// Package native binds the Windows media engine, isshoni-media.dll, to the media contract
// (docs/m2/04-windows-engine.md §12, docs/m2/01-client-core.md §7): Open returns an *Engine that is a media.Engine
// and 05's media.AudioController, its sessions are media.Session values, and every error is a *media.Error.
//
// There is no cgo. On Windows the DLL is loaded by full path with LoadLibraryEx and called through syscall.SyscallN
// (call_windows.go); everything else goes through the small caller interface (bridge.go), so the binding's logic
// builds and is tested on every OS against a fake caller. Off Windows, Open returns an error wrapping
// media.ErrUnsupportedOS (engine_other.go).
//
// This file mirrors native/include/isshoni_media.h, the C ABI v1 (04 §5): its constants and its struct layouts.
// The header is the one source: abi_test.go checks every #define against these constants and layout_test.go every
// struct against native/common/tests/testdata/abi_layout.json (04 §5.4). Window titles, source names and executable
// names that cross the ABI are local data: the binding never logs them.
package native

// The C ABI version this binding speaks (04 §5.5): the DLL's major must equal imABIMajor (media.ABIVersion) and its
// minor must be at least goMinMinor, the newest minor whose functions or fields Go uses unconditionally.
const (
	imABIMajor   = 1
	imABIMinor   = 0
	imABIVersion = imABIMajor<<16 | imABIMinor

	goMinMinor = 0
)

// im_status values (04 §5.7). Positive values are outcomes of im_next_packet, negative ones errors.
const (
	imOK                    = 0
	imTimeout               = 1
	imEnded                 = 2
	imErrInvalidArg         = -1
	imErrBadState           = -2
	imErrABI                = -3
	imErrUnsupportedOS      = -4
	imErrPermissionDenied   = -5
	imErrAudioDeviceChanged = -6
	imErrSourceGone         = -7
	imErrEncoderLost        = -8
	imErrNoEncoder          = -9
	imErrCancelled          = -10
	imErrLimit              = -11
	imErrDependency         = -12
	imErrAudioUnavailable   = -13
	imErrNotImplemented     = -14
	imErrTimeout            = -15
	imErrInternal           = -99
)

// Init, log levels.
const (
	imLogError = 0
	imLogWarn  = 1
	imLogInfo  = 2
	imLogDebug = 3

	imInitNoAudio = 0x1
)

// Probe.
const (
	imMaxAdapters = 4
	imMaxEncoders = 8

	imEncAuto         = 0
	imEncNVENC        = 1
	imEncQSV          = 2
	imEncAMF          = 3
	imEncMFHW         = 4
	imEncMFSW         = 5
	imEncOpenH264     = 6
	imEncVideoToolbox = 7
	imEncGStreamer    = 8
	imEncFake         = 100

	imEncfAvailable   = 0x01
	imEncfHardware    = 0x02
	imEncfLiveBitrate = 0x04
	imEncfZeroCopy    = 0x08
	imEncfHigh        = 0x10
	imEncfCB          = 0x20
	imEncfForcedIDR   = 0x40

	imAdapterSoftware   = 0x1
	imAdapterHasOutputs = 0x2

	imOSSupported           = 0x00000001
	imOSWGC                 = 0x00000002
	imOSWGCPicker           = 0x00000004
	imOSWGCBorderless       = 0x00000008
	imOSWGCMinInterval      = 0x00000010
	imOSWGCSecondaryWindows = 0x00000020
	imOSWGCCursorToggle     = 0x00000040
	imOSDesktopDuplication  = 0x00000080
	imOSExcludeFromCapture  = 0x00000100
	imOSMediaFoundation     = 0x00000200
	imOSHDRDisplay          = 0x00000400

	// im_caps.audio_flags; media.AudioCapsFromFlags decodes them.
	imAudioProcessLoopback  = 0x1
	imAudioEndpointLoopback = 0x2
	imAudioSessionEvents    = 0x4
	imAudioMicDetection     = 0x8

	imProbeQuick = 0x0
	imProbeFull  = 0x1
)

// Sources.
const (
	imSourceMonitor     = 1
	imSourceWindow      = 2
	imSourceTestPattern = 3

	imCaptureWGC    = 1
	imCaptureDD     = 2
	imCaptureTest   = 3
	imCaptureSCK    = 4
	imCapturePortal = 5
	imCaptureX11    = 6

	imSrcfOSBorder = 0x1
	imSrcfSelf     = 0x2
	imSrcfMapped   = 0x4
)

// Audio (05 §4.1).
const (
	imAudioSampleRate = 48000
	imAudioChannels   = 2
	imMaxRules        = 512
	imMaxRuleValue    = 520
	imAppIDLen        = 64
	imMaxLevelApps    = 64

	imAudioModeOff         = 0
	imAudioModeExcludeApps = 1
	imAudioModeEverything  = 2 // reserved; IM_ERR_INVALID_ARG in M2, and Go never passes it (README §3 item 15)
	imAudioModeTestTone    = 100

	imRuleAppExclude     = 0
	imRuleAppAllow       = 1
	imRuleInstance       = 2
	imRuleStop           = 3
	imRuleSharedRuntime  = 4
	imRuleBrowser        = 5
	imRuleWebViewDataDir = 6

	imRuleSrcBuiltin = 0
	imRuleSrcUser    = 1
	imRuleSrcSelf    = 2
	imRuleSrcViewer  = 3
	imRuleSrcData    = 4

	imExclfKeepOutMicUsers = 0x1
	imExclfFastPath        = 0x2

	imAudiofTapPCM        = 0x1
	imAudiofTestFailProbe = 0x80000000
)

// Sessions.
const (
	imMaxLayers = 3
	imAllLayers = 0xFFFFFFFE
	imNoLayer   = 0xFFFFFFFF

	imProfileHigh = 0x6400
	imProfileCB   = 0x42E0

	imContentAuto   = 0
	imContentMotion = 1
	imContentDetail = 2

	imLayerInactive = 0x1

	imStartHideCursor        = 0x1
	imStartPaused            = 0x2
	imStartAllowListedSource = 0x4
	imStartKeepBorder        = 0x8

	imSessNoOSIndicator = 0x1
	imSessAudio         = 0x2

	imAudioStateOff      = 0
	imAudioStateStarting = 1
	imAudioStateOK       = 2
	imAudioStateDegraded = 3
	imAudioStateNoDevice = 4
)

// Packets and events.
const (
	imPacketVideo = 1
	imPacketAudio = 2
	imPacketEvent = 3
	imPacketPCM   = 4
	imPacketDebug = 100

	imPktKeyframe      = 0x1
	imPktReinit        = 0x2
	imPktRefresh       = 0x4
	imPktDiscontinuity = 0x8

	imEventEnded              = 1
	imEventSourceResized      = 2
	imEventSourceMinimized    = 3
	imEventSourceRestored     = 4
	imEventCaptureInterrupted = 5
	imEventCaptureResumed     = 6
	imEventEncoderChanged     = 7
	imEventDeviceReset        = 8
	imEventLimitChanged       = 9
	imEventRingOverflow       = 10
)

// Own windows.
const imProtectOff = 0x1

// The C structs, field for field (natural alignment, amd64 and arm64; 04 §5.4). Pointers into Go memory that the
// engine reads during a call are typed Go pointers, so the collector sees them (the engine copies everything before
// the call returns, 04 §5.1); handles and pointers into native memory are uintptr, so it never does. The c tags name
// the C fields; layout_test.go compares every offset with the golden file.

type imInitParams struct {
	structSize   uint32 `c:"struct_size"`
	abiVersion   uint32 `c:"abi_version"`
	flags        uint32 `c:"flags"`
	logLevel     uint32 `c:"log_level"`
	ffmpegDir    *byte  `c:"ffmpeg_dir"`    // NUL-terminated UTF-8; nil = the DLL's own directory
	openh264Path *byte  `c:"openh264_path"` // nil = none
}

type imClockSample struct {
	structSize    uint32 `c:"struct_size"`
	reserved      uint32 `c:"reserved"`
	monoNS        int64  `c:"mono_ns"`
	unixNS        int64  `c:"unix_ns"`
	uncertaintyNS int64  `c:"uncertainty_ns"`
}

type imAdapterInfo struct {
	luid          uint64    `c:"luid"`
	vendorID      uint32    `c:"vendor_id"`
	deviceID      uint32    `c:"device_id"`
	driverVersion uint64    `c:"driver_version"`
	name          [128]byte `c:"name"`
	flags         uint32    `c:"flags"`
	reserved      uint32    `c:"reserved"`
}

type imEncoderInfo struct {
	kind         uint32    `c:"kind"`
	adapterIndex uint32    `c:"adapter_index"`
	flags        uint32    `c:"flags"`
	status       int32     `c:"status"`
	maxWidth     uint32    `c:"max_width"`
	maxHeight    uint32    `c:"max_height"`
	testEncodeUS uint32    `c:"test_encode_us"`
	reserved     [3]uint32 `c:"reserved"`
	codec        [32]byte  `c:"codec"`
	reason       [32]byte  `c:"reason"`
}

type imCaps struct {
	structSize   uint32                       `c:"struct_size"`
	osBuild      uint32                       `c:"os_build"`
	osUBR        uint32                       `c:"os_ubr"`
	osFlags      uint32                       `c:"os_flags"`
	audioFlags   uint32                       `c:"audio_flags"`
	adapterCount uint32                       `c:"adapter_count"`
	adapters     [imMaxAdapters]imAdapterInfo `c:"adapters"`
	encoderCount uint32                       `c:"encoder_count"`
	reserved0    uint32                       `c:"reserved0"`
	encoders     [imMaxEncoders]imEncoderInfo `c:"encoders"`
	probeMS      uint32                       `c:"probe_ms"`
	reserved     [15]uint32                   `c:"reserved"`
}

type imPickParams struct {
	structSize   uint32  `c:"struct_size"`
	flags        uint32  `c:"flags"`
	parentWindow uintptr `c:"parent_window"` // an HWND: never Go memory
	timeoutMS    uint32  `c:"timeout_ms"`
	reserved     uint32  `c:"reserved"`
}

type imSourceInfo struct {
	structSize  uint32    `c:"struct_size"`
	kind        uint32    `c:"kind"`
	id          [64]byte  `c:"id"`
	name        [256]byte `c:"name"`
	monitorID   [64]byte  `c:"monitor_id"`
	width       uint32    `c:"width"`
	height      uint32    `c:"height"`
	capturePath uint32    `c:"capture_path"`
	flags       uint32    `c:"flags"`
	pid         uint32    `c:"pid"`
	reserved0   uint32    `c:"reserved0"`
	window      uint64    `c:"window"`
	reserved    [4]uint32 `c:"reserved"`
}

type imRule struct {
	kind   uint32 `c:"kind"`
	source uint32 `c:"source"`
	value  *byte  `c:"value"`
	appID  *byte  `c:"app_id"` // nil = "exe:<value>"
	name   *byte  `c:"name"`   // nil = the exe's FileDescription
}

type imExclusions struct {
	structSize uint32  `c:"struct_size"`
	flags      uint32  `c:"flags"`
	rules      *imRule `c:"rules"` // the first of ruleCount elements
	ruleCount  uint32  `c:"rule_count"`
	sharedPID  uint32  `c:"shared_pid"`
}

type imAudioConfig struct {
	mode       uint32    `c:"mode"`
	bitrateBPS uint32    `c:"bitrate_bps"`
	frameMS    uint32    `c:"frame_ms"`
	flags      uint32    `c:"flags"`
	reserved   [4]uint32 `c:"reserved"`
}

type imAudioLevel struct {
	appID    [imAppIDLen]byte `c:"app_id"`
	peakDB   float32          `c:"peak_db"`
	rmsDB    float32          `c:"rms_db"`
	source   uint32           `c:"source"`
	reserved uint32           `c:"reserved"`
}

type imAudioLevels struct {
	structSize uint32                       `c:"struct_size"`
	reportSeq  uint32                       `c:"report_seq"`
	qpcNS      int64                        `c:"qpc_ns"`
	mixPeakDB  float32                      `c:"mix_peak_db"`
	mixRMSDB   float32                      `c:"mix_rms_db"`
	count      uint32                       `c:"count"`
	reserved   uint32                       `c:"reserved"`
	apps       [imMaxLevelApps]imAudioLevel `c:"apps"`
}

type imLayerConfig struct {
	rid        [4]byte   `c:"rid"`
	width      uint32    `c:"width"`
	height     uint32    `c:"height"`
	maxPixels  uint32    `c:"max_pixels"`
	fps        uint32    `c:"fps"`
	bitrateBPS uint32    `c:"bitrate_bps"`
	gopMS      uint32    `c:"gop_ms"`
	flags      uint32    `c:"flags"`
	reserved   [2]uint32 `c:"reserved"`
}

type imStartConfig struct {
	structSize  uint32                     `c:"struct_size"`
	flags       uint32                     `c:"flags"`
	sourceID    *byte                      `c:"source_id"` // nil = audio only
	profile     uint32                     `c:"profile"`
	contentHint uint32                     `c:"content_hint"`
	encoder     uint32                     `c:"encoder"`
	layerCount  uint32                     `c:"layer_count"`
	layers      [imMaxLayers]imLayerConfig `c:"layers"`
	audio       imAudioConfig              `c:"audio"`
	exclusions  *imExclusions              `c:"exclusions"` // nil = the last im_set_exclusions/im_audio_watch set
	reserved    [8]uint32                  `c:"reserved"`
}

type imLayerInfo struct {
	rid        [4]byte   `c:"rid"`
	encoder    uint32    `c:"encoder"`
	width      uint32    `c:"width"`
	height     uint32    `c:"height"`
	fps        uint32    `c:"fps"`
	bitrateBPS uint32    `c:"bitrate_bps"`
	profile    uint32    `c:"profile"`
	flags      uint32    `c:"flags"`
	reserved   [2]uint32 `c:"reserved"`
}

type imSessionInfo struct {
	structSize  uint32                   `c:"struct_size"`
	flags       uint32                   `c:"flags"`
	capturePath uint32                   `c:"capture_path"`
	layerCount  uint32                   `c:"layer_count"`
	epochNS     int64                    `c:"epoch_ns"`
	audioState  uint32                   `c:"audio_state"`
	reserved0   uint32                   `c:"reserved0"`
	layers      [imMaxLayers]imLayerInfo `c:"layers"`
	reserved    [8]uint32                `c:"reserved"`
}

type imPacket struct {
	structSize  uint32 `c:"struct_size"`
	kind        uint32 `c:"kind"`
	layer       uint32 `c:"layer"`
	flags       uint32 `c:"flags"`
	captureTSNS int64  `c:"capture_ts_ns"`
	// data points into engine memory, valid until the next im_next_packet or im_stop of the session (04 §5.1): a
	// uintptr, so the collector never sees it. The binding copies [data, data+size) before its next call.
	data       uintptr   `c:"data"`
	size       uint32    `c:"size"`
	durationUS uint32    `c:"duration_us"`
	width      uint16    `c:"width"`
	height     uint16    `c:"height"`
	profile    uint32    `c:"profile"`
	event      int32     `c:"event"`
	status     int32     `c:"status"`
	reserved   [4]uint32 `c:"reserved"`
}

// encoderTokens are the encoder tokens of 04 §5.6: one per im_encoder_info.kind names an encoder everywhere (the
// stats JSON, ENCODER_CHANGED, media.EncoderInfo.Name, media.Event.Encoder, media.Config.Encoder, enginetest's
// -encoder flag). IM_ENC_AUTO has none: media.Config.Encoder "" means it.
var encoderTokens = [...]struct {
	kind  uint32
	token string
}{
	{imEncNVENC, "nvenc"},
	{imEncQSV, "qsv"},
	{imEncAMF, "amf"},
	{imEncMFHW, "mf-hw"},
	{imEncMFSW, "mf-sw"},
	{imEncOpenH264, "openh264"},
	{imEncVideoToolbox, "videotoolbox"},
	{imEncGStreamer, "gstreamer"},
	{imEncFake, "fake"},
}

// encoderToken returns the token of an IM_ENC_* kind; ok is false for IM_ENC_AUTO and unknown kinds.
func encoderToken(kind uint32) (token string, ok bool) {
	for _, e := range encoderTokens {
		if e.kind == kind {
			return e.token, true
		}
	}
	return "", false
}

// encoderKind returns the IM_ENC_* kind of a token; "" is IM_ENC_AUTO. ok is false for an unknown token.
func encoderKind(token string) (kind uint32, ok bool) {
	if token == "" {
		return imEncAuto, true
	}
	for _, e := range encoderTokens {
		if e.token == token {
			return e.kind, true
		}
	}
	return 0, false
}

// cBytes returns the Go string of a fixed char[N] field: the bytes before the first NUL (all N when none).
func cBytes(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
