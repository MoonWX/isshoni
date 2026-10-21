package native

// The exported Windows-only types (04 §12.2): not in the media contract; 03 and the tools reach them by type-asserting
// *native.Engine or the session. The test tone is media.AudioTestTone (05 §13.1): this package declares no mode
// constant of its own.

// BuildInfo is im_build_info's JSON (04 §5.6): what the DLL was built from and what it has loaded. 06's --self-check
// reads FFmpeg.Loaded, FFmpeg.License and TestHooks; logs, About and diagnostics read the rest.
type BuildInfo struct {
	Engine    string            `json:"engine"`    // the release SemVer (06 sets it from the tag); Info().Build
	ABI       uint32            `json:"abi"`       // the DLL's IM_ABI_VERSION, major << 16 | minor
	Commit    string            `json:"commit"`    // the source commit, short
	TestHooks bool              `json:"testHooks"` // IM_TEST_HOOKS: a win-x64-ci build, never shipped
	OS        string            `json:"os"`        // "windows"
	Arch      string            `json:"arch"`      // "amd64"
	Compiler  string            `json:"compiler"`  // e.g. "MSVC 19.44"
	FFmpeg    FFmpegBuildInfo   `json:"ffmpeg"`
	Opus      string            `json:"opus"` // the linked libopus version string
	OpenH264  OpenH264BuildInfo `json:"openh264"`
}

// FFmpegBuildInfo is im_build_info's ffmpeg object. FFmpeg loads lazily (the first probe or start, 04 §5.3), so
// Loaded and the library versions change after Open: Engine.BuildInfo reads them again on every call.
type FFmpegBuildInfo struct {
	Loaded bool `json:"loaded"`
	// Dir is the FFmpeg directory relative to the engine DLL's ("." when installed next to it), never absolute.
	Dir     string `json:"dir"`
	Tag     string `json:"tag"`     // native/ffmpeg/lock.json's tag, compiled in
	AVCodec string `json:"avcodec"` // the loaded avcodec version, "" when not loaded
	AVUtil  string `json:"avutil"`
	License string `json:"license"` // avcodec_license(): "LGPL version 2.1 or later"
}

// OpenH264BuildInfo is im_build_info's openh264 object.
type OpenH264BuildInfo struct {
	Loaded  bool   `json:"loaded"`
	Version string `json:"version"` // "" while not loaded (JSON null)
}

// ClockSample is one im_clock reading: the engine clock and the wall clock at the same instant (04 §5.2). MonoNS is
// on the QPC nanosecond clock that packets' capture times use before the binding subtracts the session's epoch
// (Session.Now and Packet.CaptureNS share that origin; 01 anchors sender reports from them, not from this sample).
type ClockSample struct {
	MonoNS        int64 // QPC in nanoseconds (04 §12.3's formula)
	UnixNS        int64 // wall clock, nanoseconds since the Unix epoch
	UncertaintyNS int64 // half the QPC bracket around the wall-clock read
}

// SourceDetail is what im_source_info says about a source beyond media.Source (04 §12.2). Local data, never logged.
type SourceDetail struct {
	MonitorID   string  // monitor sources: the im_list_sources id (03 places the pill on it); "" otherwise
	CapturePath string  // "wgc" | "dd" | "test"
	OSBorder    bool    // IM_SRCF_OS_BORDER: Windows draws its yellow border
	Mapped      bool    // IM_SRCF_MAPPED: a Win10 monitor pick mapped to a DD output
	Window      uintptr // the HWND when identified (04 §7.6), else 0
}

// PCMChunk is one IM_PACKET_PCM chunk of a session started with media.AudioConfig.TapPCM: 10 ms of the mix fed to
// Opus, for 05's audiotest (04 §12.2, PCMTap).
type PCMChunk struct {
	CaptureNS int64     // the chunk's first sample, on the clock of Session.Now
	Frames    []float32 // 960 values: 480 interleaved stereo frames
}
