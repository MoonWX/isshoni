package media

// Probe is what Engine.Probe reports: the binding's decode of 04's im_caps; the fake fills the same struct.
type Probe struct {
	OS OSInfo
	// Encoders lists every probed encoder in 04's order (NVENC, QSV, AMF, MF hardware, MF software, OpenH264),
	// unavailable ones included.
	Encoders []EncoderInfo
	Capture  CaptureCaps
	Audio    AudioCaps
}

// OSInfo describes the operating system.
type OSInfo struct {
	Name    string // "windows"
	Version string // "10.0.<os_build>.<os_ubr>", e.g. "10.0.26200.6584"
	Build   int    // os_build: 26200
	// Supported is IM_OS_SUPPORTED: the OS meets isshoni's minimum (Win10 22H2); false → 03's "Needs Windows 10
	// 22H2 or newer".
	Supported bool
}

// EncoderInfo is one probed encoder.
type EncoderInfo struct {
	Name string // kind token: "nvenc" | "qsv" | "amf" | "mf-hw" | "mf-sw" | "openh264" | "fake"
	// Adapter is the adapter_index into the probe's adapters; −1 for software encoders (one entry per adapter is
	// possible).
	Adapter int
	HW      bool // IM_ENCF_HARDWARE
	// Tested is IM_ENCF_AVAILABLE: the encoder opened and passed the one-frame test encode (plan "im_probe").
	Tested      bool
	LiveBitrate bool     // IM_ENCF_LIVE_BITRATE: SetBitrate applies without a re-init (01 D10)
	Profiles    []string // "6400" from IM_ENCF_HIGH, "42e0" from IM_ENCF_CB
	ZeroCopy    bool     // IM_ENCF_ZERO_COPY
	// Status is "" when Tested; else why the encoder is unavailable (no_encoder, dependency, …): 03 tells "update
	// driver" from "turn on OpenH264".
	Status ErrorCode
	Reason string // 04's reason token for Status (driver_too_old, missing, blocked_by_policy, …)
}

// CaptureCaps is what screen capture can do.
type CaptureCaps struct {
	Picker     bool // IM_OS_WGC_PICKER: GraphicsCapturePicker available
	Borderless bool // IM_OS_WGC_BORDERLESS: the capture border can be turned off
	// DesktopDuplication is IM_OS_DESKTOP_DUPLICATION: a picked Win10 monitor is captured through DD (04 §7.5).
	DesktopDuplication bool
	ExcludeFromCapture bool // IM_OS_EXCLUDE_FROM_CAPTURE: WDA_EXCLUDEFROMCAPTURE works (04 §11)
	MaxFPS             int  // 240 with IM_OS_WGC_MIN_INTERVAL, else 60
}

// AudioCaps is derived only from im_caps.audio_flags: the four IM_AUDIO_* bits 04's header declares and 05 §11.1
// defines. There is no other audio struct in im_caps. 05 owns the semantics. Live banners come from 05's AudioReport
// through its Service, never from the probe (the probe plays no sound). Endpoint counts are in 05's report
// (05 §12.1), and the OS build is Probe.OS.Build. There is no timer bit (05 §11.1).
//
// Backend is the value 03's ShareCaps.audioBackend shows before a share (the share sheet's "everything on this PC"
// line for "endpoint"). It is computed here, in one place, so the binding and the fake agree. There is no Mode field,
// no Degraded field and no "exclude-one" value.
type AudioCaps struct {
	// ProcessLoopback is IM_AUDIO_PROCESS_LOOPBACK (0x1): an include stream on the DLL's own pid activates (the
	// include-set mixer can run).
	ProcessLoopback bool
	// EndpointLoopback is IM_AUDIO_ENDPOINT_LOOPBACK (0x2): endpoint loopback of the default render endpoint
	// initializes.
	EndpointLoopback bool
	// SessionEvents is IM_AUDIO_SESSION_EVENTS (0x4): session notifications register on every active render
	// endpoint.
	SessionEvents bool
	// MicDetection is IM_AUDIO_MIC_DETECTION (0x8): at least one active capture endpoint is enumerated.
	MicDetection bool
	// Backend is "process-loopback" when ProcessLoopback; else "endpoint" when EndpointLoopback; else "none".
	Backend string
}

// The im_caps.audio_flags bits (04 §5.2, 05 §11.1) and the AudioCaps.Backend values.
const (
	audioFlagProcessLoopback  = 0x1
	audioFlagEndpointLoopback = 0x2
	audioFlagSessionEvents    = 0x4
	audioFlagMicDetection     = 0x8

	audioBackendProcessLoopback = "process-loopback"
	audioBackendEndpoint        = "endpoint"
	audioBackendNone            = "none"
)

// AudioCapsFromFlags decodes im_caps.audio_flags (bits above 0x8 are ignored). 04's binding fills Probe.Audio with
// it; the fake's default probe uses it too (01 §6.2).
func AudioCapsFromFlags(flags uint32) AudioCaps {
	c := AudioCaps{
		ProcessLoopback:  flags&audioFlagProcessLoopback != 0,
		EndpointLoopback: flags&audioFlagEndpointLoopback != 0,
		SessionEvents:    flags&audioFlagSessionEvents != 0,
		MicDetection:     flags&audioFlagMicDetection != 0,
	}
	switch {
	case c.ProcessLoopback:
		c.Backend = audioBackendProcessLoopback
	case c.EndpointLoopback:
		c.Backend = audioBackendEndpoint
	default:
		c.Backend = audioBackendNone
	}
	return c
}
