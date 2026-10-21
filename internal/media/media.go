// Package media is the media contract between Go and an OS media engine (docs/m2/01-client-core.md §5): the Engine
// and Session interfaces, Packet and Event, the errors, the config and probe types (probe.go, errors.go), and the
// audio types and AudioController of docs/m2/05-windows-audio.md §13.1 (audio.go).
//
// The package has no engine of its own. internal/media/native binds the Windows engine DLL
// (docs/m2/04-windows-engine.md §12) and internal/media/fake is the contract's test double (the engine of every test
// that needs no hardware, and of the app on macOS and Linux in M2). Both implement Engine, Session and
// AudioController.
//
// The contract is standard library only, with no build-tagged files and no cgo (01 D1), so that internal/client may
// import it and still build for android and ios with CGO_ENABLED=0 (01 D2, the gomobile check). Source names, window
// titles and executable names are local data: never logged, never sent to the server.
package media

import (
	"context"
	"fmt"
	"time"
)

// ABIVersion is the isshoni_media.h ABI major this build speaks (04 §5.5: IM_ABI_MAJOR). The native binding also
// requires the DLL's minor to be at least the minor it was built against (0 in M2).
const ABIVersion = 1

// Kind is a packet's kind. The zero Kind is invalid.
type Kind uint8

// The packet kinds.
const (
	KindVideo Kind = 1
	KindAudio Kind = 2
	KindEvent Kind = 3 // a non-fatal engine event; Packet.Event is set, Data is nil
	// KindPCM is for the audiotest and the sound check only: 10 ms of 48 kHz stereo float32 (3840 bytes), only with
	// AudioConfig.TapPCM. The core never asks for it, so the pump and the publisher never see one.
	KindPCM Kind = 4
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindEvent:
		return "event"
	case KindPCM:
		return "pcm"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// LayerAudio names the Opus encoder in Session.SetBitrate. Video layers are named by their rid ("f", "q"). Declared
// once, here: audio.go does not redeclare it.
const LayerAudio = "audio"

// Packet is one access unit, one Opus packet, one PCM chunk or one event, as the engine hands it out (04 §12.3).
type Packet struct {
	Kind  Kind
	Layer string // video: the rid of a Config.Layers entry; "" otherwise
	// Keyframe marks a video IDR access unit with SPS and PPS in front (in-band, plan "Keyframes"); always false
	// for audio.
	Keyframe bool
	// Reinit marks, for video, the first access unit after an encoder (re)initialization, always a keyframe; for
	// audio, the first packet after Start or after a timestamp-grid discontinuity (05 §10.4).
	Reinit bool
	// CaptureNS is the capture time on the session's clock (Session.Now): the frame's, or the first sample's.
	CaptureNS int64
	// Data is, for video, one Annex B access unit; for audio, one 20 ms Opus packet; for PCM, 3840 bytes. It is owned
	// by the receiver: the engine never touches it again.
	Data  []byte
	Event *Event // KindEvent only
}

// Engine is one OS media engine: the native one (internal/media/native) or the fake (internal/media/fake). Its
// methods are safe for concurrent use. An engine may run several sessions (native: up to 4, at most one with audio;
// beyond that Start returns ErrBusy, from IM_ERR_LIMIT); one share at a time is the core's rule (01 D12). The engines
// also implement AudioController (audio.go), which callers get with a type assertion.
type Engine interface {
	// Info describes the engine. It does no I/O.
	Info() Info
	// Probe reports what the PC can do. Native: IM_PROBE_FULL on the first call or with refresh; cached otherwise.
	Probe(ctx context.Context, refresh bool) (Probe, error)
	// Sources lists monitors, for pill placement, diagnostics, tests and tools (mediatest, TestCaptureToSend). The
	// app never shares a listed source (plan: OS pickers only): starting one needs Config.Listed. May be empty.
	Sources(ctx context.Context) ([]Source, error)
	// Pick runs the OS picker. It returns ErrCancelled when the user closes it or it times out; ctx ends it. A pick
	// of an isshoni window is released and returns *Error{Code: CodeInvalid, Reason: "self_window"}.
	Pick(ctx context.Context, req PickRequest) (Source, error)
	// Release forgets an unused pick (native: im_release_source). Other ids are a no-op.
	Release(id string)
	// Start starts capturing and encoding cfg.Source.
	Start(ctx context.Context, cfg Config) (Session, error)
	// Close stops every running Session; the engine is unusable afterwards.
	Close() error
}

// Session is a running capture and encode. Next is for exactly one goroutine; every other method may be called from
// any goroutine, also while Next blocks.
type Session interface {
	// Next blocks until the next packet or event, in capture order per stream (kind + layer); there is no order
	// across streams (04 §6.2). It returns ErrStopped after Stop, a fatal *Error when the session ended (it is then
	// stopped), or ctx's error within 50 ms of ctx's end.
	Next(ctx context.Context) (Packet, error)
	// Now returns the current time on the clock of Packet.CaptureNS.
	Now() int64
	// RequestKeyframe makes a coming frame of the layer an IDR (also when the screen is static: the engine encodes
	// its last frame again, plan "Keyframes"). Unknown layers are ignored. Never blocks.
	RequestKeyframe(layer string)
	// SetBitrate sets a video layer's target in bit/s (04 §6.4: layer "f" 100 kbit/s–50 Mbit/s, other layers
	// 50 kbit/s–2 Mbit/s; 0 or out of range is ErrInvalid) or, for LayerAudio, the Opus target (32 000–510 000,
	// live, 05 §4.3). A video encoder without live bitrate re-initializes (Reinit packets), at most every 10 s
	// (04 §6.4).
	SetBitrate(layer string, bps int) error
	// SetLayerActive pauses or resumes a video layer's encoder (native: im_set_layer_active). An inactive layer
	// sends nothing; a resumed one starts with a keyframe that is not a Reinit (04 §8.5).
	SetLayerActive(layer string, active bool) error
	// SetPaused freezes the session's video (native: im_set_paused): from the next frame interval on, the engine
	// encodes only the frozen frame (04 §8.5). Audio is muted separately through AudioController.SetAudioMuted
	// (01 D14).
	SetPaused(paused bool) error
	// Reconfigure changes what Start set, without restarting capture or audio: the profile, the preset tuning, a
	// layer's frame rate or pixel budget. Zero fields keep their value. Affected encoders re-initialize with an IDR
	// (Reinit packets). It is how publish.ProfileSource.SetProfile reaches the engine. The Opus target changes only
	// through SetBitrate(LayerAudio, bps).
	Reconfigure(r Reconfig) error
	// Stats returns the engine's own per-layer numbers (native: im_get_stats, 04 §5.6). Cheap; polled at 1 Hz.
	Stats() (EngineStats, error)
	// Stop ends the session and unblocks Next within 200 ms. Idempotent.
	Stop() error
}

// Info describes an engine.
type Info struct {
	Name  string // "windows" | "fake" | "darwin" (M3) | "linux" (M4)
	ABI   int    // the ABI major: native int(im_version() >> 16); fake: ABIVersion
	Build string // native: im_build_info().engine (the release SemVer, 04 §5.5); "" for the fake
}

// SourceKind is what a Source shows.
type SourceKind string

// The source kinds: the same strings as protocol.ShareKind.
const (
	SourceScreen SourceKind = "screen"
	SourceWindow SourceKind = "window"
)

// Source is something the user picked, or a listed monitor. ID, Name, Exe and MonitorID are local only: never
// logged, never sent to the server (plan "Privacy and trust model").
//
// ID: a picked source ("pick:7") is single use: one successful Start consumes it; it expires 5 min after the pick,
// and the engine keeps at most 4 unused picks (04 §7.2); Release forgets one. Listed sources ("mon:\\.\DISPLAY1",
// "test:pattern") can be started again, and only with Config.Listed.
//
// There is no Self field: a pick of an isshoni window (04 IM_SRCF_SELF) never returns a Source. Pick releases it and
// returns *Error{Code: CodeInvalid, Reason: "self_window"} (04 §12.3, docs/m2/README.md §3 item 17), and the app
// reopens the picker.
type Source struct {
	ID   string
	Kind SourceKind // native: IM_SOURCE_MONITOR and IM_SOURCE_TEST_PATTERN → screen; IM_SOURCE_WINDOW → window
	Name string     // window title or monitor name, for the local UI
	// Width and Height are the size in pixels at pick time.
	Width, Height int
	// PID is, for window sources, the owning process when the engine identified it (04 §7.6), else 0 (05: the shared
	// app is never auto-excluded).
	PID int
	// Exe is, for window sources, the base name of PID's image, derived by the binding (OpenProcess with
	// PROCESS_QUERY_LIMITED_INFORMATION and QueryFullProcessImageNameW), or ""; for the local UI and 05.
	Exe       string
	MonitorID string // monitor sources: the Sources id of that monitor (pill placement, 03); else ""
	Capture   string // "wgc" | "dd" | "test": the capture path Start will use (04 §7.1, §7.5)
	OSBorder  bool   // Windows will draw its capture border (04 IM_SRCF_OS_BORDER)
}

// PickRequest configures Engine.Pick.
type PickRequest struct {
	// OwnerWindow is the app window (HWND) for IInitializeWithWindow (04 §7.2); 0 = an engine-owned owner window.
	OwnerWindow uintptr
	// Kinds is ignored on Windows (its picker shows both, 04 §7.2) and used on macOS (M3): nil = window first (plan).
	Kinds   []SourceKind
	Timeout time.Duration // 0 = 120 s; at most 600 s (native: im_pick_params.timeout_ms)
}

// Config configures Engine.Start.
//
// There is no GOP field: the engine's defaults apply (f 4 s, q 2 s, no periodic IDR while the content is static,
// 04 §9.4); the fake uses the same defaults.
type Config struct {
	// Source is a Source.ID. "" only for an audio-only session: Layers empty and Audio set (05's audiotest and sound
	// check; the core never starts one).
	Source string
	// Listed is for tests and tools only: accept a listed source (native: IM_START_ALLOW_LISTED_SOURCE). The app
	// never sets it.
	Listed bool
	// Encoder "" is automatic (native: IM_ENC_AUTO, 04's order with per-layer fallback). A probe token (nvenc, qsv,
	// amf, mf-hw, mf-sw, openh264, fake) forces that kind without fallback: enginetest, 03's hidden support override,
	// and "fake" (IM_ENC_FAKE, the win-x64-ci build) in CI.
	Encoder string
	// Profile is the engine profile key: "6400" | "42e0" (others: ErrInvalid; "640c" is only an SDP alias, 01 §8.6).
	Profile string
	Preset  string        // "auto" | "game" | "movie" | "text": encoder tuning (native content hint, 04 §20)
	Layers  []LayerConfig // 0–2, "f" first; 0 only for an audio-only session
	// Audio is 05's AudioConfig (05 §13.1). nil = no audio track. Mode is AudioExcludeApps ("" means it) or, in
	// tests and tools, AudioTestTone; AudioEverything is rejected in M2 (CodeInvalid, docs/m2/README.md §3
	// item 15); AudioOff is only a mid-share AudioController.SetAudioMode value.
	Audio *AudioConfig
	// Borderless asks for no capture border where Probe.Capture.Borderless; false maps to IM_START_KEEP_BORDER
	// (tests). The core always sets it to Probe.Capture.Borderless (04 asks for borderless by default). Whether a
	// border really shows is the session's NoOSIndicator (EventLayer), not this request.
	Borderless bool
}

// LayerConfig is one video layer of a Config.
type LayerConfig struct {
	RID string // "f" | "q"
	// MaxPixels is the pixel budget (ShareParams.Encodings[i].MaxPixels): the engine scales the source to fit,
	// keeping its aspect ratio, even sizes, never upscaling.
	MaxPixels int
	MaxFPS    int  // ShareParams.Encodings[i].MaxFramerate
	Bitrate   int  // initial target in bit/s (ratectl, 01 §9)
	Inactive  bool // start with this layer paused (native: IM_LAYER_INACTIVE; ShareParams.Encodings[i].Active false)
}

// Reconfig is what Session.Reconfigure changes. Zero fields keep their value.
type Reconfig struct {
	Profile string
	Preset  string
	Layers  []LayerConfig // matched by RID; zero MaxPixels, MaxFPS and Bitrate keep the value
}

// EngineStats is the engine's own view of a session (native: im_get_stats, 04 §5.6; fake: its own numbers).
type EngineStats struct {
	Layers []EngineLayerStats
}

// EngineLayerStats is one layer of EngineStats.
type EngineLayerStats struct {
	RID                    string
	Encoder                string // probe token of the encoder in use
	HW                     bool
	FPSIn, FPSOut          float64 // captured and encoded frames per second over the last 2 s
	BitrateBps, TargetBps  int
	Limit                  string // "none" | "cpu" | "gpu" | "source"
	EncodeP95              time.Duration
	LatencyP50, LatencyP95 time.Duration // capture → packet in the engine's ring (04's latencyMs)
	Drops                  int           // pacer + ring + stale drops since Start
}

// EventCode is Go-only: the binding maps 04's IM_EVENT_* to it (01 §5.2's table); the numbers are not the header's.
type EventCode uint8

// The event codes. 2–5 are reserved: audio is reported through 05's AudioReport, never through Next.
const (
	// EventLayer: a layer's encoder (re)started: Layer, Width, Height, FPS, Encoder, HW, Profile, LiveBitrate,
	// NoOSIndicator. The engine sends one per layer at start and one before every Reinit packet of the layer.
	EventLayer EventCode = 1
	// EventLimit: the layer's encoder can't keep up (Limit "cpu" | "gpu") or can again ("none"); 04 applies the
	// hysteresis.
	EventLimit EventCode = 6
	// EventSourceResized: the source size changed (Width, Height); a Reinit follows only if the output size changes
	// (after 1 s of stability, 04 §7.7).
	EventSourceResized EventCode = 7
	// EventSourceMinimized: the shared window is minimized; viewers see the last frame.
	EventSourceMinimized EventCode = 8
	// EventSourceRestored: the shared window is shown again.
	EventSourceRestored EventCode = 9
	// EventCaptureInterrupted: secure desktop or a mode change; the last frame repeats (log only).
	EventCaptureInterrupted EventCode = 10
	// EventCaptureResumed ends an EventCaptureInterrupted.
	EventCaptureResumed EventCode = 11
	// EventEncoderChanged: the engine fell back to another encoder for Layer (Encoder); an EventLayer follows with
	// the re-init (01 D15).
	EventEncoderChanged EventCode = 12
	// EventDeviceReset: the GPU was reset or removed and the pipeline rebuilt (log only).
	EventDeviceReset EventCode = 13
)

// Event is a non-fatal engine event, the Event of a KindEvent Packet.
type Event struct {
	Code               EventCode
	Layer              string // the rid, for layer events
	Width, Height, FPS int    // EventLayer: the encoded size and rate; EventSourceResized: the source size
	Encoder            string // EventLayer, EventEncoderChanged: probe token of the encoder now in use
	HW                 bool
	Profile            string // EventLayer: the SPS's profile key ("6400" | "42e0")
	LiveBitrate        bool   // EventLayer: this layer's encoder changes bitrate live (01 D10)
	// NoOSIndicator is, for EventLayer, whether the session has no OS capture border (IM_SESS_NO_OS_INDICATOR:
	// borderless WGC or DD).
	NoOSIndicator bool
	Limit         string // EventLimit: "cpu" | "gpu" | "none"
}
