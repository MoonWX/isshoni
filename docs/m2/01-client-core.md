# M2 design 01: the Go client core (`internal/client`, `internal/media`)

M2 is "Windows app". This spec covers the Go code that the desktop app runs below its window: the media contract
between Go and a native engine (`internal/media`), the fake engine that stands in for it in tests and on macOS and
Linux, what the Go binding of the Windows engine DLL must do (04 implements it), the publish path from encoded packets
to RTP, uplink rate control, the connection and share lifecycle of the app's Go connection, the same-user relay
payloads, and how the M2 exit criterion "glass-to-glass latency under 200 ms at 1080p60" is measured. It also
specifies the server pieces that exist only to serve this client (§10), because no other M2 spec owns them.

The other M2 specs, written at the same time, are referenced by file name:
[02-device-auth.md](02-device-auth.md) (device flow, tokens, the server's bearer path),
[03-desktop-app.md](03-desktop-app.md) (Wails shell, SPA `DesktopPlatform`, share sheet, wizard, share safety),
[04-windows-engine.md](04-windows-engine.md) (the C++ DLL, `native/include/isshoni_media.h` and the Go binding
`internal/media/native`),
[05-windows-audio.md](05-windows-audio.md) (the include-set mixer, exclusion rules, "What friends hear"),
[06-windows-release.md](06-windows-release.md) (CI, installer, updater, release). `docs/PLAN.md` stays the source of
truth for product decisions; the M1 specs are the record of what exists.

**How this spec cites**: `§x` alone is a section of this spec. `01 §…` is always M1 (`01 §8.10` is
`docs/m1/01-protocol.md` §8.10; "M1 01 §3.2" only stresses it). `02 §…` to `06 §…` and bare numbers
are the **M2** specs (`04 §12` is `docs/m2/04-windows-engine.md` §12, `03` is `docs/m2/03-desktop-app.md`). The M1
specs 02–06 carry an **M1** prefix (`M1 02 §9.1` is `docs/m1/02-sfu.md` §9.1, `M1 05 §13.7` is
`docs/m1/05-web-client.md` §13.7). `README §3` is `docs/m1/README.md` §3 and `S88` an M1 slice.

Every item is marked **M2**, or **later (M3/M4/M5)** when M2 only leaves room for it.

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | `internal/media` (root package only) is the **media contract**: the `Engine` and `Session` interfaces, `Packet`, `Event`, errors and the config and probe types. Stdlib only, no build-tagged files, no cgo | `internal/client` may then import it under the gomobile rule (D2). M1 README §3 left "move the packet type out of `fake` or add a depguard exception" to M2: the packet type moves here, and `fake.Packet` becomes an alias |
| D2 | The depguard rule of M1 06 §8.2 goes live: non-test files under `internal/client/` import from this repository only `internal/client/...` (sibling packages), `internal/protocol`, `internal/protocol/api`, `internal/protocol/relay`, `internal/media` (root), `internal/version` and `internal/logx`. M1 06 §8.2's third-party clause ("only Pion, x/net and internal/protocol") is **replaced**: depguard restricts in-repo imports only, and third-party modules are governed by the license gate. CI also compiles `internal/client/...` and `internal/media` for `android/arm64` and `ios/arm64` with `CGO_ENABLED=0` | Keeps the core reusable by the pending gomobile apps; `logx` and `version` were placed outside `internal/server` for this (M1 04 §1). Siblings: the core imports `signal`, `publish` and `ratectl`; 02's `deviceauth` imports `credstore`, `compat` and `deeplink`. The third-party clause never matched what exists: `signal` already uses `coder/websocket`, 02's `credstore` needs `golang.org/x/sys/windows` and `deeplink` needs `golang.org/x/net/idna` |
| D3 | The OS engines live in **`internal/media/native`** (04 §12.1's files for Windows, M2; `engine_darwin.go` M3; `engine_linux.go` M4), not in the `internal/media` root as the plan's tree shows | A root package with cgo (darwin) or `x/sys/windows` files would drag them into every importer of the contract, the gomobile builds included (`GOOS=ios` also matches `_darwin.go`) |
| D4 | The Windows binding uses **`golang.org/x/sys/windows`** (`LoadLibraryEx`, `SyscallN`), not purego (04 §12.4) | `x/sys` is already a module of the repo (v0.49.0) and S1's loader used it; purego adds a dependency and on Windows wraps the same `SyscallN` |
| D5 | **Pull-based, one drain goroutine**: the core's media pump is the only caller of `Session.Next` (`im_next_packet`) and hands packets to the current Publisher through bounded queues | The engine must outlive PeerConnections (30 s hold, rebuilds); native rings never overflow because nobody pulls; backpressure drops whole frames and asks for a keyframe instead of leaking latency |
| D6 | RTP timestamps come from capture times and **the SR anchor uses the engine's clock** (`Session.Now`, QPC on Windows), not "now" | M1's Publisher assumed a packet is due when the fake hands it out; a real engine hands out a frame 10–30 ms after capture. `NTP = wall(now) − (engine.Now − capture)` keeps "NTP = wall clock at capture" true (plan "A/V sync") |
| D7 | The native publisher negotiates **no RTX**: NACKs are answered with plain retransmissions by Pion's NACK responder, sized **4096 packets** per SSRC | The SFU caches and forwards plain retransmissions like RTX ones (M1 02 §9.1) and bounds its requests (≤ 10 per packet, M1 02 §8.1); no second SSRC per layer. 4096 covers ≥ 4 s of an 8 Mbit/s layer and 1 s at 40 Mbit/s (M3 4K) |
| D8 | The core's publisher **paces video** (an opt-in `publish` option, so M1's users are unchanged) with a token bucket at `max(3 × Σ active layer targets, 6 Mbit/s)`, depth 40 ms of that rate; audio is never paced | A 1080p screen IDR is 150–400 KB; written at once it hits the user's router as a line-rate burst, which is loss or bufferbloat. Same reasoning as the SFU's pacer (M1 02 §9.8) and Chrome's 2.5× pacing factor |
| D9 | **Rate control is server-estimated, client-applied** (plan "Rate control"): the SFU's native estimator (§10.1) sends `quality.hint{reason: congestion, maxBitrate}` with ≥ 15 % / ≥ 2 s hysteresis; the client applies a hint at most every 2 s (live encoders), or only for ≥ 30 % changes at most every 10 s (encoders that need a re-init + IDR). The client also **ramps** the full layer from 4 Mbit/s by +15 % per 2 s up to its ceiling while no cap binds | Pion GCC stays off until `pion/bwe` ships (plan). The plan's Auto preset "starts at ~4 Mbps and ramps to 8 Mbps, never above ~80 % of measured uplink": the client ramps, the server's estimator caps at 80 % of what arrived when loss shows |
| D10 | Whether an encoder changes bitrate live comes **only from the engine**: `EncoderInfo.LiveBitrate` in the probe and, per layer, `Event.LiveBitrate` of the layer's `EventLayer` (the encoder really running it), never from encoder names in Go | The plan lists AMF and MF via FFmpeg as re-init; the M2 task text lists MF as live. 04 decides per encoder (FFmpeg's `h264_mf` has no runtime reconfigure; a direct ICodecAPI path would be live); the layers of one session may run on different encoders (04 §9.7), so the controller follows each layer's flag |
| D11 | Resolution and frame rate stay fixed per share in M2; only bitrates change. When the server's cap binds below what the target size needs, the sharer sees "Your upload allows about 720p" (`UploadHint`) | The plan's ABI has `im_set_bitrate` only; resolution or frame-rate adaptation per preset needs encoder reconfiguration with IDRs and tuning: later (M5, "preset tuning") |
| D12 | The app's Go connection uses role **`publisher`**, joins a room **only while it shares there** (and briefly to stop another share of the user, `StopOther`, §11.2) and leaves when its last share ends; the in-app SPA's `viewer` connection follows the UI's room (03). Both authenticate as the same device, so they are one Participant (01 §4.1) | Presence stays exactly what the user sees. The relay works without a room (01 §8.14) |
| D13 | **No preset push from the server**: the publishing client reconciles its share with its own `ShareInfo` in every `room.state` (preset, `paused`), and fetches new `ShareParams` with an idempotent `share.update{preset}` | `room.state` already reaches the publishing connection; one code path covers resume, a change made from a web tab, and pause. `HintReasonPreset` stays reserved (01 §8.10) |
| D14 | **Pause** = the core stops forwarding video and audio **at the pump** at once (frozen frame for current viewers, silence), then tells the engine: `Session.SetPaused(true)` (04: only the frozen frame is encoded from the next frame interval) and `media.AudioController.SetAudioMuted(true)` (05: 10 ms fade, then silent frames), then sends `share.update{paused: true}` (feature `share.pause`, §10.3). On resume it unmutes the engine first, un-pauses it, requests a keyframe per active layer and forwards again (video from those keyframes). Keyframe requests are ignored while paused; viewers who join a paused share see a "Paused" tile, not the old frame | Pause is a safety control (plan "Share safety": "frozen frame plus muted audio"): no frame captured while paused may leave the PC, so a keyframe of the current screen can't be sent to a late joiner. The pump drop covers the up to one frame interval before the engine applies the pause; the engine pause stops new content at the source |
| D15 | **Encoder fallback is the engine's** (04 §9.7, §9.8): the engine switches a layer to the next encoder mid-session and reports `EventEncoderChanged`; the core only shows it. `encoder_lost` from `Next` means no candidate is left and ends the share. The core does not retry `Engine.Start` with one layer after `no_encoder` in M2: the share is refused (04 §9.7's one-layer option stays unused until M3) | A Go-side restart can't work against 04: picked sources are single use (04 §7.2), a new session has a new `epoch_ns` so `CaptureNS` restarts at 0 and a retained Publisher's RTP timestamps would jump back, and naming an encoder forces it and disables the per-layer fallback (04 §9.1) |
| D16 | In M2 the Windows app's Go connection **serves no relay request kind**: it only consumes `playback.state` from web tabs (05 §5.9), and the desktop SPA (C1) sends `playback.stop` (03 §12.2). `share.request`/`share.status` (web-initiated desktop shares) are later (M4), as M1 01 §8.14 reserves them and the plan puts the agent relay in M4. `exclusions.get/set` are reserved kinds; their payloads are declared by M4's agent spec | Nothing in M2 starts a desktop share from a web tab (03 §2). The privacy model says app lists, the exclusion list and "What friends hear" never leave the device; on Windows nothing needs them to |
| D17 | Glass-to-glass is measured on the owner's PC by **screen-capture comparison** (§12.2): a Gray-coded clock strip on the shared monitor and its copy in a Chrome viewer on the same monitor, both decoded from the same desktop screenshot. The gating run is a normal share: **Share sound on** with a continuous tone, the viewer's audio subscribed, and full-screen content cuts every 5 s. Pass: **p95 < 200 ms** over 60 s at 1080p60, server RTT ≤ 50 ms, on Win11 and Win10. Sound-off and no-cut runs are recorded for diagnosis only | Both copies go through the same display pipeline, so the difference is photon-to-photon without a photodiode, accurate to one display frame. With sound, Chrome lip-syncs video to the slower audio path (M1 02 §9.6), and content cuts are screen sharing's main latency spike (§12.1). A sound-off, steady-motion run would pass while normal shares miss. The statistic, network condition and content are this spec's reading of the plan's "under 200 ms" (§18 asks the owner to confirm) |
| D18 | The core has **no Wails dependency**. 03 binds the core's Go API to the window; 03 pins the Wails version | Lets the core build and test on every OS, and keeps M3's macOS app on the same core |

## 2. Scope

**In this spec (M2)**
- `internal/media`: the contract (§5).
- `internal/media/fake`: `fake.Engine`, the contract's test double (§6), on top of M1's `fake.Source`.
- `internal/media/mediatest`: a conformance suite every engine must pass (§6.4).
- `internal/media/native`: what 04's binding of `isshoni-media.dll` must do to implement the contract (§7); 04 §12
  owns and implements the package.
- `internal/client/publish`: the M2 changes to M1's Publisher (§8).
- `internal/client/ratectl`: uplink rate control (§9).
- `internal/client/core`: the desktop client core: the Go connection, shares, media pump, recovery, hints, pause,
  stats, `playback.state`, events for the app (§11).
- `internal/protocol` additions and the relay payloads in `internal/protocol/relay` (§10.3, §10.4, §11.11).
- Server counterparts: the native uplink estimator (SFU), `share.pause` (protocol, hub, SFU), `user.connections`
  (hub) (§10).
- Latency measurement: `tools/g2g` and its run book, plus two CI proxies (§12).

**Not in this spec**: the DLL, the header and the Go binding's implementation (04), audio exclusion semantics, the
audio types' fields and the audio control surface (05), device linking, token storage and the server's bearer
authentication (02), the Wails app, its windows, tray, hotkeys, share sheet, wizard and SPA changes (03), CI jobs,
installer, updater and release (06). The macOS and Linux engines (M3, M4). Mobile apps (pending).

## 3. What M1 built, and what M2 changes

### 3.1 `internal/client/signal` (01 §15.3, README S39)

One reconnecting WebSocket client: `Dial`, `Request`, `Send`, `Events` (must be drained, 256 deep), `States` (never
blocks, 64 deep), `Probe`, `RetryNow`, `Close`; the state machine and backoff of 01 §10.1–§10.2; resume tokens in
memory; ping every `limits.pingIntervalMs` with a 10 s pong timeout. Bearer auth exists already: `Options.Token` is
called before every `hello`, resuming ones included, and its value goes into `hello.auth`. An `unauthenticated`
answer stops the client; "refresh once and retry" is the caller's (01 §3.2). Pure Go on `coder/websocket`.

Users: the itest harness (`internal/server/itest/client_test.go`, `harness_test.go`), `isshoni-loadtest`
(`cmd/isshoni-loadtest/client.go`, cookie auth, `kind: tool`).

**M2: no API change.** The core (§11) is built on it as it is.

### 3.2 `internal/client/publish` (M1 02 §15.1, README S63, S69, S88)

One Pion PeerConnection that the client offers: one video transceiver with one encoding per rid (`f`, `q`) and an
own `TrackLocal` that stamps `sdes:mid` and `rtp-stream-id`; H.264 packetized with `H264Payloader` (MTU 1200), Opus
as is; `rtp_ts = base + (capture − t0) × clock` with one `t0` for all tracks; its own SRs every 1 s per SSRC (no
`report.SenderInterceptor`); PLI/FIR → `Source.RequestKeyframe(layer)`; Pion's NACK responder; the TWCC
header-extension interceptor; re-offers (`Offer`, `OfferICERestart`) with `sendOnlySimulcast`; several H.264
profiles with `SetProfile` and the switch at the next keyframe (`ProfileSource`); `SetActive(layer)`; test options
`RedundantRTX` and `Padding`. No rate control (REMBs are counted only).

Users: the itests, `isshoni-loadtest`, `sfutest` (`ApplyHint`, `ProfileSource`), the SFU's tests.

**M2 changes** (§8): `Source.Next` returns `media.Packet`; an optional `Clock` for the SR anchor; an opt-in video
pacer (`Options.Pacing`); the responder size; `Reinit` packets; per-layer bitrate statistics, capture-to-send latency and RTT from receiver
reports. All M1 behavior and options stay, so every M1 user compiles and passes unchanged.

### 3.3 `internal/media/fake` (M1 02 §15.1)

`fake.Source`: pull-based like `im_next_packet`; Synthetic (codec-shaped H.264 for load) and Decodable (pure-Go I_PCM
/ P_Skip Constrained Baseline) video, the committed Opus tone, flash/beep markers in every payload, `SetBitrate`,
`RequestKeyframe`, a 200 ms lag cap. Test code: no release artifact contains it.

Users: `isshoni-loadtest`, `sfutest`, the SFU, server and itest tests.

**M2 changes** (§6): `fake.Packet`, `fake.Kind`, `fake.Video` and `fake.Audio` become aliases of the `internal/media`
types; the `Flash` and `Beep` fields leave the packet (nothing outside `fake` reads them; `sfutest` and the load test
read the Markers from the payloads); `fake.Engine` implements `media.Engine` on top of `fake.Source`, and 05's
`media.AudioController` (05 §13.3).

### 3.4 `cmd/isshoni-loadtest` and the itests

The load test drives `signal` + `publish` + `fake` per simulated sharer and an `sfutest.Viewer` per viewer, with no
recovery beyond what a healthy run needs. The itests carry a **test-only** implementation of the client's recovery
rules (01 §10.4–§10.6) around `publish.Publisher` (`client_test.go`: `share.drive`, `rebuild`, `restartICE`,
`republish`, `hinted`).

**M2**: the load test is unchanged (it compiles through the aliases). The core (§11.6–§11.7) is the production
version of the itest's recovery rules; the M1 itest cases keep their own client, and the new desktop cases (§14) use
the core.

### 3.5 Summary

| Package | M1 | M2 |
|---|---|---|
| `internal/media` | – | new: the contract (§5) |
| `internal/media/fake` | `Source` | + `Engine` (and 05's `AudioController`); packet types are aliases |
| `internal/media/mediatest` | – | new: engine conformance suite |
| `internal/media/native` | – | new: 04's Windows binding (04 §12), implementing §5 as §7 requires |
| `internal/client/signal` | complete | unchanged |
| `internal/client/publish` | M1 subset | + `media.Packet`, clock, pacer, stats, reinit |
| `internal/client/ratectl` | – | new |
| `internal/client/core` | – | new |
| `internal/protocol` | v1 | additive: `ShareUpdate.Paused`, `ShareStatusPaused`, `OwnConnection.DeviceName` |
| `internal/protocol/relay` | – | new: relay payloads `playback.stop`, `playback.state` (tygo → TS) |
| `internal/server/sfu` | `m1Uplink` | + `nativeUplink`, `RawLossPct`, sharer pause |
| `internal/server/signal` | relay transport | + `share.pause`, `user.connections` |

## 4. Package layout and import rules

```
internal/media/                 media.go probe.go errors.go                    (D1; this spec)
                                audio.go                                       (05 §13.1: AudioConfig, AudioController, …)
internal/media/fake/            fake.go h264.go decodable.go opus.go marker.go (M1) + engine.go session.go (M2)
                                + audio_ctl.go                                 (05 §13.3)
internal/media/mediatest/       conformance.go                                 (test helper, imported by _test files)
internal/media/native/          04 §12.1's files (abi.go, types.go, errors.go, engine_windows.go, session_windows.go,
                                call_windows.go, engine_other.go, …) + audio_windows.go (05's AudioController binding)
internal/client/publish/        publish.go track.go repair.go (M1) + pacer.go stats.go clock.go
internal/client/ratectl/        ratectl.go split.go upload.go
internal/client/core/           core.go conn.go share.go pump.go pubdriver.go hints.go relay.go stats.go events.go
internal/protocol/relay/        relay.go                                         (stdlib + internal/protocol)
tools/g2g/                      main.go clock.go capture_windows.go decode.go report.go web/clock.html
```

Import rules (additions to M1 04 §2's table; depguard encodes them):

| Package | May import (this repo) | Never imports |
|---|---|---|
| `internal/media` | nothing (stdlib only) | any package of this repo |
| `internal/media/fake` | `internal/media` | `internal/client/...`, `internal/server/...` |
| `internal/media/native` | `internal/media`, `internal/logx` | `internal/client/...`, `internal/server/...` |
| `internal/media/mediatest` | `internal/media` | – (test helper) |
| `internal/media/audiorules` (05) | nothing (stdlib only) | any package of this repo |
| `internal/desktop/audio` (05, non-test files) | `internal/media`, `internal/media/audiorules`, `internal/logx` | `internal/client/...`, `internal/server/...`, `internal/media/native`, `internal/media/fake` |
| `internal/protocol/relay` | `internal/protocol` | everything else |
| `internal/client/...` (non-test files) | `internal/client/...` (sibling packages), `internal/protocol`, `internal/protocol/api`, `internal/protocol/relay`, `internal/media`, `internal/version`, `internal/logx` | `internal/media/*` subpackages, `internal/server/...`, `internal/desktop/...` |

- depguard restricts in-repo imports only (D2); third-party modules of `internal/client/...` are governed by the
  license gate, not by M1 06 §8.2's "only Pion, x/net and internal/protocol", which this rule replaces.
- Test files of `internal/client/...` may import `internal/media/fake`, `mediatest`, `servertest` and `sfutest`.
- `cmd/isshoni` keeps its rule against `internal/media/fake` (M1 02 §15.1). `cmd/isshoni-desktop` links
  `internal/media/fake` only in files tagged `dev || e2e` (03 §5.3, §22.4); `internal/desktop/e2ehost` links it
  always. 06's no-fake artifact check runs on the `-tags production` build.
- **gomobile check** (lint-go job, M2): `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./internal/client/...
  ./internal/media` and the same with `GOOS=ios`. Package builds only (no linking), so no mobile toolchain is
  needed. It covers all of `./internal/client/...`, 02's packages included (they pass: `credstore`'s file store is
  `!windows`). A gomobile-*bindable* API (no channels, no generics) is the pending mobile wrapper's job, not the
  core's.

## 5. The media contract (`internal/media`, M2)

### 5.1 Types

```go
package media

import (
	"context"
	"time"
)

// ABIVersion is the isshoni_media.h ABI major this build speaks (04 §5.5: IM_ABI_MAJOR). The native binding also
// requires the DLL's minor to be at least goMinMinor (0 in M2).
const ABIVersion = 1

// Kind is a packet's kind. The zero Kind is invalid.
type Kind uint8

const (
	KindVideo Kind = 1
	KindAudio Kind = 2
	KindEvent Kind = 3 // a non-fatal engine event; Packet.Event is set, Data is nil
	KindPCM   Kind = 4 // audiotest and sound check only: 10 ms of 48 kHz stereo float32 (3840 bytes), only with AudioConfig.TapPCM
)

// LayerAudio names the Opus encoder in SetBitrate. Video layers are named by their rid ("f", "q"). Declared once,
// here: 05's audio.go does not redeclare it.
const LayerAudio = "audio"

// Packet is one access unit, one Opus packet, one PCM chunk or one event, as the engine hands it out (04 §12.3).
type Packet struct {
	Kind      Kind
	Layer     string // video: the rid of a Config.Layers entry; "" otherwise
	Keyframe  bool   // video: an IDR access unit with SPS and PPS in front (in-band, plan "Keyframes"); always false for audio
	Reinit    bool   // video: the first access unit after an encoder (re)initialization, always a keyframe; audio: the first
	                 // packet after Start or after a timestamp-grid discontinuity (05 §10.4)
	CaptureNS int64  // capture time on the session's clock (Session.Now): the frame's, or the first sample's
	Data      []byte // video: one Annex B access unit; audio: one 20 ms Opus packet; PCM: 3840 bytes. Owned by the receiver
	Event     *Event // KindEvent only
}

// Engine is one OS media engine: the native one (internal/media/native) or the fake (internal/media/fake). Its
// methods are safe for concurrent use. An engine may run several sessions (native: up to 4, at most one with audio;
// beyond that Start returns ErrBusy, from IM_ERR_LIMIT); one share at a time is the core's rule (D12).
type Engine interface {
	Info() Info                                                // no I/O
	Probe(ctx context.Context, refresh bool) (Probe, error)    // native: IM_PROBE_FULL on the first call or with refresh; cached otherwise
	// Sources lists monitors, for pill placement, diagnostics, tests and tools (mediatest, TestCaptureToSend). The app
	// never shares a listed source (PLAN: OS pickers only): starting one needs Config.Listed. May be empty.
	Sources(ctx context.Context) ([]Source, error)
	Pick(ctx context.Context, req PickRequest) (Source, error) // the OS picker; ErrCancelled when the user closes it or it times out; ctx ends it
	Release(id string)                                         // forget an unused pick (native: im_release_source; fake: forgets it); other ids: no-op
	Start(ctx context.Context, cfg Config) (Session, error)
	Close() error                                              // stops every running Session; the engine is unusable afterwards
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
	// RequestKeyframe makes a coming frame of the layer an IDR (also when the screen is static: the engine encodes its
	// last frame again, plan "Keyframes"). Unknown layers are ignored. Never blocks.
	RequestKeyframe(layer string)
	// SetBitrate sets a video layer's target in bit/s (04 §6.4: layer "f" 100 kbit/s–50 Mbit/s, other layers
	// 50 kbit/s–2 Mbit/s; 0 or out of range is ErrInvalid) or, for LayerAudio, the Opus target (32 000–510 000, live,
	// 05 §4.3). A video encoder without live bitrate re-initializes (Reinit packets), at most every 10 s (04 §6.4).
	SetBitrate(layer string, bps int) error
	// SetLayerActive pauses or resumes a video layer's encoder (native: im_set_layer_active). An inactive layer sends
	// nothing; a resumed one starts with a keyframe that is not a Reinit (04 §8.5).
	SetLayerActive(layer string, active bool) error
	// SetPaused freezes the session's video (native: im_set_paused): from the next frame interval on, the engine
	// encodes only the frozen frame (04 §8.5). Audio is muted separately through AudioController.SetAudioMuted (D14).
	SetPaused(paused bool) error
	// Reconfigure changes what Start set, without restarting capture or audio: the profile, the preset tuning, a
	// layer's frame rate or pixel budget. Zero fields keep their value. Affected encoders re-initialize with an IDR
	// (Reinit packets). It is how publish.ProfileSource.SetProfile reaches the engine. The Opus target changes only
	// through SetBitrate(LayerAudio, bps).
	Reconfigure(r Reconfig) error
	// Stats returns the engine's own per-layer numbers (native: im_get_stats, 04 §5.6). Cheap; polled at 1 Hz.
	Stats() (EngineStats, error)
	// Stop ends the session and unblocks Next. Idempotent.
	Stop() error
}

type Info struct {
	Name  string // "windows" | "fake" | "darwin" (M3) | "linux" (M4)
	ABI   int    // the ABI major: native int(im_version() >> 16); fake: ABIVersion
	Build string // native: im_build_info().engine (the release SemVer, 04 §5.5); "" for the fake
}

type SourceKind string

const (
	SourceScreen SourceKind = "screen" // same strings as protocol.ShareKind
	SourceWindow SourceKind = "window"
)

// Source is something the user picked, or a listed monitor. ID, Name, Exe and MonitorID are local only: never
// logged, never sent to the server (plan "Privacy and trust model").
//
// ID: a picked source ("pick:7") is single use: one successful Start consumes it; it expires 5 min after the pick,
// and the engine keeps at most 4 unused picks (04 §7.2); Release forgets one. Listed sources ("mon:\\.\DISPLAY1",
// "test:pattern") can be started again, and only with Config.Listed.
type Source struct {
	ID            string
	Kind          SourceKind // native: IM_SOURCE_MONITOR and IM_SOURCE_TEST_PATTERN → screen; IM_SOURCE_WINDOW → window
	Name          string     // window title or monitor name, for the local UI
	Width, Height int        // size in pixels at pick time
	PID           int        // window sources: the owning process when 04 identified it (04 §7.6), else 0 (05: the shared app is never auto-excluded)
	Exe           string     // window sources: derived by the binding from PID (OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) +
	                         // QueryFullProcessImageNameW, base name), or ""; for the local UI and 05
	MonitorID     string     // monitor sources: the Sources id of that monitor (pill placement, 03); else ""
	Capture       string     // "wgc" | "dd" | "test": the capture path Start will use (04 §7.1, §7.5)
	OSBorder      bool       // Windows will draw its capture border (04 IM_SRCF_OS_BORDER)
	// No Self field: a pick of an isshoni window (04 IM_SRCF_SELF) never returns a Source. Pick releases it and returns
	// *Error{Code: CodeInvalid, Reason: "self_window"} (04 §12.3, README §3 item 17); 03 reopens the picker.
}

type PickRequest struct {
	OwnerWindow uintptr       // the app window (HWND) for IInitializeWithWindow (04 §7.2); 0 = an engine-owned owner window
	Kinds       []SourceKind  // ignored on Windows (its picker shows both, 04 §7.2); used on macOS (M3): nil = window first (plan)
	Timeout     time.Duration // 0 = 120 s; at most 600 s (native: im_pick_params.timeout_ms)
}

type Config struct {
	// Source is a Source.ID. "" only for an audio-only session: Layers empty and Audio set (05's audiotest and sound
	// check; the core never starts one).
	Source string
	Listed bool // tests and tools only: accept a listed source (native: IM_START_ALLOW_LISTED_SOURCE); the app never sets it
	// Encoder "" = automatic (native: IM_ENC_AUTO, 04's order with per-layer fallback). A probe token (nvenc, qsv, amf,
	// mf-hw, mf-sw, openh264, fake) forces that kind without fallback: enginetest, 03's hidden support override, and
	// "fake" (IM_ENC_FAKE, the win-x64-ci build) in CI.
	Encoder string
	Profile string        // engine profile key: "6400" | "42e0" (others: ErrInvalid; "640c" is only an SDP alias, §8.6)
	Preset  string        // "auto" | "game" | "movie" | "text": encoder tuning (native content hint, 04 §20)
	Layers  []LayerConfig // 0–2, "f" first; 0 only for an audio-only session
	// Audio is 05's AudioConfig (05 §13.1). nil = no audio track. Mode is AudioExcludeApps ("" means it) or, in tests
	// and tools, AudioTestTone; AudioEverything is rejected in M2 (CodeInvalid, README §3 item 15); AudioOff is only a
	// mid-share AudioController.SetAudioMode value.
	Audio *AudioConfig
	// Borderless asks for no capture border where Probe.Capture.Borderless; false maps to IM_START_KEEP_BORDER (tests).
	// The core always sets it to Probe.Capture.Borderless (04 asks for borderless by default). Whether a border really
	// shows is the session's NoOSIndicator (EventLayer), not this request.
	Borderless bool
}

// GOP: there is no GOP field. The engine's defaults apply (f 4 s, q 2 s, no periodic IDR while the content is
// static, 04 §9.4); the fake uses the same defaults.

type LayerConfig struct {
	RID       string // "f" | "q"
	MaxPixels int    // pixel budget (ShareParams.Encodings[i].MaxPixels): the engine scales the source to fit, keeping its aspect ratio, even sizes, never upscaling
	MaxFPS    int    // ShareParams.Encodings[i].MaxFramerate
	Bitrate   int    // initial target in bit/s (ratectl, §9)
	Inactive  bool   // start with this layer paused (native: IM_LAYER_INACTIVE; ShareParams.Encodings[i].Active false)
}

type Reconfig struct {
	Profile string
	Preset  string
	Layers  []LayerConfig // matched by RID; zero MaxPixels/MaxFPS/Bitrate keep the value
}

// EngineStats is the engine's own view of a session (native: im_get_stats, 04 §5.6; fake: its own numbers).
type EngineStats struct {
	Layers []EngineLayerStats
}

type EngineLayerStats struct {
	RID                    string
	Encoder                string        // probe token of the encoder in use
	HW                     bool
	FPSIn, FPSOut          float64       // captured and encoded frames per second over the last 2 s
	BitrateBps, TargetBps  int
	Limit                  string        // "none" | "cpu" | "gpu" | "source"
	EncodeP95              time.Duration
	LatencyP50, LatencyP95 time.Duration // capture → packet in the engine's ring (04's latencyMs)
	Drops                  int           // pacer + ring + stale drops since Start
}
```

`probe.go` (the binding's decode of 04's `im_caps`; the fake fills the same struct):

```go
type Probe struct {
	OS       OSInfo
	Encoders []EncoderInfo // every probed encoder in 04's order (NVENC, QSV, AMF, MF hardware, MF software, OpenH264), unavailable ones included
	Capture  CaptureCaps
	Audio    AudioCaps
}

type OSInfo struct {
	Name      string // "windows"
	Version   string // "10.0.<os_build>.<os_ubr>", e.g. "10.0.26200.6584"
	Build     int    // os_build: 26200
	Supported bool   // IM_OS_SUPPORTED: meets isshoni's minimum (Win10 22H2); false → 03's "Needs Windows 10 22H2 or newer"
}

type EncoderInfo struct {
	Name        string    // kind token: "nvenc" | "qsv" | "amf" | "mf-hw" | "mf-sw" | "openh264" | "fake"
	Adapter     int       // adapter_index into the probe's adapters; −1 for software encoders (one entry per adapter is possible)
	HW          bool      // IM_ENCF_HARDWARE
	Tested      bool      // IM_ENCF_AVAILABLE: opened and passed the one-frame test encode (plan "im_probe")
	LiveBitrate bool      // IM_ENCF_LIVE_BITRATE: SetBitrate applies without a re-init (D10)
	Profiles    []string  // "6400" from IM_ENCF_HIGH, "42e0" from IM_ENCF_CB
	ZeroCopy    bool      // IM_ENCF_ZERO_COPY
	Status      ErrorCode // "" when Tested; else why it is unavailable (no_encoder, dependency, …): 03 tells "update driver" from "turn on OpenH264"
	Reason      string    // 04's reason token for Status (driver_too_old, missing, blocked_by_policy, …)
}

type CaptureCaps struct {
	Picker             bool // IM_OS_WGC_PICKER: GraphicsCapturePicker available
	Borderless         bool // IM_OS_WGC_BORDERLESS: the capture border can be turned off
	DesktopDuplication bool // IM_OS_DESKTOP_DUPLICATION: a picked Win10 monitor is captured through DD (04 §7.5)
	ExcludeFromCapture bool // IM_OS_EXCLUDE_FROM_CAPTURE: WDA_EXCLUDEFROMCAPTURE works (04 §11)
	MaxFPS             int  // 240 with IM_OS_WGC_MIN_INTERVAL, else 60
}

// AudioCaps is derived only from im_caps.audio_flags: the four IM_AUDIO_* bits 04's header declares and 05 §11.1
// defines. There is no other audio struct in im_caps. 05 owns the semantics. Live banners come from 05's AudioReport
// through its Service, never from the probe (the probe plays no sound). Endpoint counts are in 05's report
// (05 §12.1), and the OS build is Probe.OS.Build. There is no timer bit (05 §11.1).
type AudioCaps struct {
	ProcessLoopback  bool   // IM_AUDIO_PROCESS_LOOPBACK (0x1): an include stream on the DLL's own pid activates (the include-set mixer can run)
	EndpointLoopback bool   // IM_AUDIO_ENDPOINT_LOOPBACK (0x2): endpoint loopback of the default render endpoint initializes
	SessionEvents    bool   // IM_AUDIO_SESSION_EVENTS (0x4): session notifications register on every active render endpoint
	MicDetection     bool   // IM_AUDIO_MIC_DETECTION (0x8): at least one active capture endpoint is enumerated
	Backend          string // "process-loopback" when ProcessLoopback; else "endpoint" when EndpointLoopback; else "none"
}

// AudioCapsFromFlags decodes im_caps.audio_flags (bits above 0x8 are ignored). 04's binding fills Probe.Audio with
// it; the fake's default probe uses it too (§6.2).
func AudioCapsFromFlags(flags uint32) AudioCaps
```

`Backend` is the value 03's `ShareCaps.audioBackend` shows before a share (the share sheet's "everything on this PC"
line for `endpoint`). It is computed in Go from the bits, in one place, so the binding and the fake agree. There is no
`Mode` field, no `Degraded` field and no `exclude-one` value; 04 §12.2 and 03 §6.3 use this shape.

`audio.go` is **05's** (05 §13.1): `AudioMode`, `AudioConfig`, `Rule`, `Exclusions`, `AudioReport`, `AudioLevels`,
`SoundCheckResult`, `AudioController` and its errors. The core reads only `AudioConfig.BitrateBPS` (it overwrites it
with `ShareParams.AudioBitrate`, §11.4) and forwards `AudioConfig` values from `Options.AudioConfig` to
`Engine.Start` unread. **The engines returned by `native.Open` and `fake.NewEngine` also implement 05's
`media.AudioController`**; 03 and 05's `internal/desktop/audio.Service` get it with `eng.(media.AudioController)`,
and the core uses only `SetAudioMuted` from it (pause, D14). "What friends hear", levels, prompts and exclusions flow
only DLL → 05's Service → 03; none of it passes through the core.

### 5.2 Errors and events

```go
type ErrorCode string

const (
	CodePermissionDenied ErrorCode = "permission_denied"
	CodeEncoderLost      ErrorCode = "encoder_lost"
	CodeSourceGone       ErrorCode = "source_gone"
	CodeUnsupportedOS    ErrorCode = "unsupported_os"
	CodeNoEncoder        ErrorCode = "no_encoder"
	CodeDependency       ErrorCode = "dependency"        // FFmpeg or OpenH264 missing, wrong version or blocked; an ABI mismatch (Reason "abi")
	CodeAudioUnavailable ErrorCode = "audio_unavailable" // no audio capture path works (AudioController calls)
	CodeAudioNotRunning  ErrorCode = "audio_not_running" // an audio call that needs a watcher or a live share (the header's not-started status)
	CodeCancelled        ErrorCode = "cancelled"
	CodeBusy             ErrorCode = "busy"
	CodeStopped          ErrorCode = "stopped"
	CodeTimeout          ErrorCode = "timeout"           // a blocking native call hit its limit (never from Next: Next loops on the timeout status)
	CodeInvalid          ErrorCode = "invalid"
	CodeInternal         ErrorCode = "internal"
)

// Error is an engine error. Reason is a short machine token from 04 (missing, version, blocked_by_policy,
// driver_too_old, dpi_unaware, slow_open, abi) that 03 may map to a text; "" when none. Detail is the native message
// (im_last_error), for logs only: never shown, never sent.
type Error struct {
	Code   ErrorCode
	Op     string // "start", "next", "pick", "probe", …
	Reason string
	Detail string
}

func (e *Error) Error() string        // "media: <op>: <code>" (+ " (<reason>)") (+ ": detail")
func (e *Error) Is(target error) bool // true for a target *Error with the same Code and empty Op, Reason and Detail
                                      // (every sentinel below, and 05's ErrAudioNotRunning, is such a target)
func (e *Error) Unwrap() error        // context.DeadlineExceeded for CodeTimeout (04 §12.2), else nil

var (
	ErrPermissionDenied = &Error{Code: CodePermissionDenied}
	ErrEncoderLost      = &Error{Code: CodeEncoderLost}
	ErrSourceGone       = &Error{Code: CodeSourceGone}
	ErrUnsupportedOS    = &Error{Code: CodeUnsupportedOS}
	ErrNoEncoder        = &Error{Code: CodeNoEncoder}
	ErrDependency       = &Error{Code: CodeDependency}
	ErrAudioUnavailable = &Error{Code: CodeAudioUnavailable}
	ErrCancelled        = &Error{Code: CodeCancelled}
	ErrBusy             = &Error{Code: CodeBusy}
	ErrStopped          = &Error{Code: CodeStopped}
	ErrInvalid          = &Error{Code: CodeInvalid}
)
// 05's audio.go declares ErrAudioNotRunning = &Error{Code: CodeAudioNotRunning} (05 §13.1); invalid audio input is
// CodeInvalid. There is no ErrInvalidAudioConfig and no plain errors.New sentinel.

// EventCode is Go-only: the binding maps 04's IM_EVENT_* to it (table below); the numbers are not the header's.
type EventCode uint8

const (
	EventLayer              EventCode = 1  // a layer's encoder (re)started: Layer, Width, Height, FPS, Encoder, HW, Profile, LiveBitrate, NoOSIndicator
	// 2–5: reserved. Audio is reported through 05's AudioReport, never through Next.
	EventLimit              EventCode = 6  // the layer's encoder can't keep up (Limit "cpu" | "gpu") or can again ("none"); 04 applies the hysteresis
	EventSourceResized      EventCode = 7  // the source size changed (Width, Height); a Reinit follows only if the output size changes (after 1 s of stability, 04 §7.7)
	EventSourceMinimized    EventCode = 8  // the shared window is minimized: viewers see the last frame
	EventSourceRestored     EventCode = 9
	EventCaptureInterrupted EventCode = 10 // secure desktop, mode change: the last frame repeats (log only)
	EventCaptureResumed     EventCode = 11
	EventEncoderChanged     EventCode = 12 // the engine fell back to another encoder for Layer (Encoder); an EventLayer follows with the re-init
	EventDeviceReset        EventCode = 13 // the GPU was reset or removed and the pipeline rebuilt (log only)
)

type Event struct {
	Code               EventCode
	Layer              string // the rid, for layer events
	Width, Height, FPS int    // EventLayer: the encoded size and rate; EventSourceResized: the source size
	Encoder            string // EventLayer, EventEncoderChanged: probe token of the encoder now in use
	HW                 bool
	Profile            string // EventLayer: the SPS's profile key ("6400" | "42e0")
	LiveBitrate        bool   // EventLayer: this layer's encoder changes bitrate live (D10)
	NoOSIndicator      bool   // EventLayer: the session has no OS capture border (IM_SESS_NO_OS_INDICATOR: borderless WGC or DD)
	Limit              string // EventLimit: "cpu" | "gpu" | "none"
}
```

Status mapping (the binding implements it; 04 §5.2's names):

| Native status | `media.ErrorCode` |
|---|---|
| `IM_ERR_INVALID_ARG` | `invalid` |
| (Go only) a pick with `IM_SRCF_SELF` | `invalid`, Reason `self_window`: the binding releases the token before `Pick` returns (04 §12.3) |
| `IM_ERR_BAD_STATE` | `internal` |
| `IM_ERR_ABI` | `dependency`, Reason `abi` |
| `IM_ERR_UNSUPPORTED_OS` | `unsupported_os` |
| `IM_ERR_PERMISSION_DENIED` | `permission_denied` |
| `IM_ERR_SOURCE_GONE` | `source_gone` |
| `IM_ERR_ENCODER_LOST` | `encoder_lost` |
| `IM_ERR_NO_ENCODER` | `no_encoder` |
| `IM_ERR_CANCELLED` | `cancelled` |
| `IM_ERR_LIMIT` | `busy` |
| `IM_ERR_DEPENDENCY` | `dependency` (Reason from the detail: `missing`, `version`, `blocked_by_policy`) |
| `IM_ERR_AUDIO_UNAVAILABLE` | `audio_unavailable` |
| the header's not-started status (05 `IM_ERR_NOT_STARTED`) | `audio_not_running` |
| the timeout status (`IM_TIMEOUT` / 05 `IM_ERR_TIMEOUT`) | looped inside `Next`; `timeout` from every other call |
| `IM_ERR_AUDIO_DEVICE_CHANGED` | never returned by `Next` (05 §4.3: a report event, not a return code); `internal` if another call returns it |
| `IM_ERR_NOT_IMPLEMENTED`, `IM_ERR_INTERNAL`, others | `internal` |
| `IM_ENDED` from `im_next_packet` | the `ENDED` event's status, mapped by this table; `IM_OK` → `ErrStopped` |

Event mapping (the binding implements it):

| 04 event | `media` | Note |
|---|---|---|
| (session start, and every `REINIT` video packet) | `EventLayer` from `im_session_info` / `im_get_session_info`, before that layer's packet | carries `Profile`, `LiveBitrate` (the layer's `IM_ENCF_LIVE_BITRATE`), `NoOSIndicator` (`IM_SESS_NO_OS_INDICATOR`) |
| `ENDED` | not an event: `Next` returns the error of its status | |
| `SOURCE_RESIZED` | `EventSourceResized{Width, Height}` | |
| `SOURCE_MINIMIZED`, `SOURCE_RESTORED` | `EventSourceMinimized`, `EventSourceRestored` | snapshot `Minimized` |
| `CAPTURE_INTERRUPTED`, `CAPTURE_RESUMED` | `EventCaptureInterrupted`, `EventCaptureResumed` | log |
| `ENCODER_CHANGED` | `EventEncoderChanged{Layer, Encoder}` | an `EventLayer` follows |
| `DEVICE_RESET` | `EventDeviceReset` | log |
| `LIMIT_CHANGED` | `EventLimit{Layer, Limit}` | edge-triggered; `ratectl.SetLimit` |
| `RING_OVERFLOW` | none: logged by the binding | |
| 32–63 (audio) | none: dropped by the binding (debug log) | 05's Service reads audio state from the report |

What each fatal error makes the core do, and the state the app shows (03 renders the texts):

| Error | Where | Core (§11) | App state (plan "Errors") |
|---|---|---|---|
| `permission_denied` | `Pick`, `Start`, `Next` | `Start`/`Next`: end the share (`error`, code); `Pick` is 03's | guided fix |
| `encoder_lost` | `Next` | end the share (`error`, `encoder_lost`): the engine already tried every candidate (D15) | stop with a notice: "Video encoding stopped" with "Update your graphics driver" or "Turn on OpenH264" |
| `source_gone` | `Next` | end the share (`source-gone`) | notice "The shared window was closed" |
| `unsupported_os`, `no_encoder` | `Probe`, `Start` | refuse the share (no one-layer retry in M2, D15) | notice / guided fix |
| `dependency` | `Open`, `Probe` (`EncoderInfo.Status`), `Start` | refuse the share | "Part of isshoni is missing or blocked: reinstall"; Reason `blocked_by_policy` gets the Smart App Control text; Reason `abi`: "Reinstall isshoni", sharing off (viewing works) |
| `audio_unavailable` | `AudioController` calls (05); never `Start` or `Next` (05 §4.3) | nothing | 05's banner |
| `busy` | `Start`, `Pick` | refuse the share | "Stop your other share first" |
| `cancelled` | `Pick` | nothing | nothing |
| `internal` | anywhere | end the share (`error`, `internal`); the detail is logged | error with "Copy diagnostics" (M5) |

A fatal `Next` error is **never audio-caused**: audio never ends a session (05 §4.3), and the binding maps no audio
status to a fatal `Next` error. Audio health banners (`reconnecting-sound`, `no-output-device`, `capture-trouble`)
come from 05's Service.

### 5.3 Semantics every engine follows

- **Order**: `CaptureNS` never decreases within a stream (kind + layer); there is no order across streams (04 §6.2).
  Video and audio use one clock (QPC on Windows, `mach_absolute_time` M3, `CLOCK_MONOTONIC` M4; plan "A/V sync").
- **First packet**: every video layer starts with a keyframe (`Reinit` set) within 500 ms of `Start`, also on a
  static screen, so a share goes live without waiting for screen changes (01 §4.4's 30 s start timeout). The rule
  applies per video layer only (an audio-only session has none).
- **Keyframes**: after `RequestKeyframe`, the layer's next or second-next frame is a keyframe, unless the layer had a
  forced IDR in the last 250 ms; then at the 250 ms mark (04 §6.4). The core's 300 ms throttle (§8.3) fits.
- **Ownership**: `Packet.Data` belongs to the receiver; the engine never touches it again (§7.1 for how native
  memory becomes Go memory).
- **Stop**: `Stop` makes a blocked `Next` return `ErrStopped` within 200 ms; `Next` after `Stop` returns `ErrStopped`.
- **Concurrency**: `Next` from one goroutine; every other `Session` method concurrently with it.
- **Sources**: a picked source is single use and expires after 5 min; a listed source needs `Config.Listed`.
- **Sessions**: several may run (native: up to 4, at most one with audio); a second `Start` with audio while a
  session with audio runs is `ErrBusy`. The fake runs one session at a time.

`internal/media/mediatest.Conformance(t *testing.T, newEngine func(t *testing.T) media.Engine, src media.Source,
o mediatest.Options)` asserts each rule (§6.4).

## 6. The fake engine (`internal/media/fake`, M2)

### 6.1 Changes to M1's package

- `type Packet = media.Packet`, `type Kind = media.Kind`, `const Video = media.KindVideo`, `Audio = media.KindAudio`.
- `Packet.Flash` and `Packet.Beep` are removed. The Markers in the payloads are unchanged, and fake's own tests read
  the flags with `ParseVideoMarker` and `ParseAudioMarker`, as `sfutest` and the load test already do.
- `Source` gains `Now() int64` (its clock: nanoseconds since its first `Next`, minus the time the 200 ms lag cap
  skipped) and `Origin() time.Time` (the wall instant of `CaptureNS` 0, moved by the same skips), for §12.4.

### 6.2 `fake.Engine`

```go
package fake

type EngineConfig struct {
	Mode          Mode           // Synthetic (default) or Decodable
	SourceSize    [2]int         // the fake screen, default 1920×1080; layers are scaled from it by MaxPixels
	Probe         media.Probe    // default: OS {runtime.GOOS, Supported}; one encoder {Name "fake", Adapter −1, Tested, LiveBitrate true,
	                             // Profiles ["6400","42e0"] (Decodable: ["42e0"])}; Capture {Picker, Borderless, MaxFPS 60};
	                             // Audio = media.AudioCapsFromFlags(0xF) (all four bits, Backend "process-loopback")
	Sources       []media.Source // default: one listed monitor "mon:fake" (Capture "test")
	PickResult    *media.Source  // what Pick returns, with a fresh "pick:<n>" ID each time; nil = a window "Fake window" with OSBorder
	Cancel        bool           // Pick returns ErrCancelled
	AudioScenario string         // 05 §13.3's scenarios for the AudioController ("" = "normal")
	Seed          uint64
	FlashEvery    time.Duration  // as Config.FlashEvery
}

func NewEngine(cfg EngineConfig) *Engine // implements media.Engine and media.AudioController (05 §13.3)

// Test controls (any goroutine):
func (e *Engine) Session() *Session                 // the running session, or nil
func (s *Session) Fail(err *media.Error)            // the next Next returns err and the session stops
func (s *Session) Emit(ev media.Event)              // the next Next returns this event before any media
func (s *Session) Calls() []Call                    // every control call with its time, for rate-control and pause assertions
func (s *Session) Origin() time.Time

type Call struct {
	At     time.Time
	Op     string // "keyframe" | "bitrate" | "active" | "paused" | "reconfigure" | "audio_muted" | "exclusions" | "stop"
	Layer  string
	Bps    int
	On     bool   // "active": active; "paused": paused; "audio_muted": muted
	Reconf media.Reconfig
}
```

Behavior:
- The fake enforces the native engine's source rules, so core tests take the app's real path: `Pick` returns
  `PickResult` under a fresh single-use `pick:<n>` ID (or `ErrCancelled` with `Cancel`; it honors ctx and
  `PickRequest.Timeout`); `Start` consumes it; `Release` forgets an unused one; a listed ID needs `Config.Listed`
  (else `invalid`).
- `Start` builds a `fake.Source` with one `VideoLayer` per `LayerConfig`: size = `SourceSize` scaled to `MaxPixels`
  (aspect kept, even sizes), `FPS = MaxFPS`, `Bitrate` = the initial target, GOP f 4 s / q 2 s; audio when
  `Config.Audio` is set; profile = `Config.Profile` ("6400" or "42e0"; other keys are `invalid`; Decodable is always
  "42e0" and refuses "6400" with `CodeInvalid`). It emits one `EventLayer` per layer (with `Profile`, `LiveBitrate`
  from its encoder, `NoOSIndicator = Config.Borderless`), then media. `Source ""` with no layers and `Audio` set
  runs audio only; with `AudioConfig.TapPCM` it also emits `KindPCM` chunks of its tone (3840 bytes per 10 ms). A
  second `Start` while a session runs is `ErrBusy`.
- `SetBitrate(layer, 0)` is `invalid`. With `Probe.Encoders[0].LiveBitrate == false` every `SetBitrate` forces a
  `Reinit` keyframe, so tests see what a re-init encoder costs.
- `SetLayerActive(layer, false)`: no packets on that layer; `true`: it resumes with a keyframe whose `Reinit` is false.
- `SetPaused(true)`: no video packets (audio goes on unless muted); `SetPaused(false)`: video resumes (the core asks
  for keyframes).
- `Reconfigure{Profile}` swaps the `fake.Source` for one in the new profile on the same timeline (like
  `sfutest.ProfileSource`), every layer starting with a `Reinit` keyframe and a new `EventLayer`.
- `AudioController` follows 05 §13.3 (scripted scenarios). `SetAudioMuted(true)` makes the audio packets carry a
  constant 20 ms Opus silence frame instead of the tone until `SetAudioMuted(false)`. Calls made while a session runs
  are recorded in its `Calls` (`"audio_muted"`, `"exclusions"`).
- `Stats` fills `EngineStats` from the fake source's own counters (`Limit` "none", latencies 0).

### 6.3 Use outside tests

`fake.NewEngine` is what the app runs with on macOS and Linux in M2 and in CI e2e runs: `cmd/isshoni-desktop` links
`internal/media/fake` only in files tagged `dev || e2e` (03 §5.3, §22.4); `internal/desktop/e2ehost` links it always.
It is also the engine of every core test. 06's no-fake artifact check runs on the `-tags production` build.

### 6.4 `mediatest.Conformance`

Runs against any `media.Engine` with a `Source` that needs no picker:

```go
type Options struct {
	Listed   bool   // src is a listed source (mon:…, test:pattern): Config.Listed is set
	TestTone bool   // audio from 04's IM_AUDIO_MODE_TEST_TONE (no audio device needed), requested as
	                // Config.Audio = &media.AudioConfig{Mode: media.AudioTestTone} (05 §13.1, the one carrier)
	Encoder  string // Config.Encoder: "" = automatic; "fake" = IM_ENC_FAKE (win-x64-ci build)
}
```

1. `Info().ABI == media.ABIVersion`; `Probe` returns at least one encoder with `Tested`.
2. `Start` with two layers (1080p60 at 4 Mbit/s and 360p15 at 300 kbit/s) and audio: each layer's first packet is a
   `Reinit` keyframe within 500 ms; one `EventLayer` per layer with sizes within the budgets and `Profile` set.
3. 5 s of media: `CaptureNS` monotonic per stream; video frame intervals within ±50 % of `1/MaxFPS` on
   average (a static desktop may legitimately send fewer frames: the check is skipped below 5 frames per second);
   audio packets every 20 ms ± 5 ms on average; every video `Data` starts with an Annex B start code.
4. Wait until ≥ 250 ms have passed since `f`'s previous keyframe, then `RequestKeyframe("f")` → a keyframe on `f`
   within 2 frames.
5. `SetBitrate("f", 1_000_000)` → when `f`'s `EventLayer.LiveBitrate` is false, wait for its `Reinit` keyframe
   (≤ 11 s, 04 §9.5 defers re-inits up to 10 s); then `f`'s bitrate over the next 4 s within 50–150 % of the target
   (skipped for the fake in Decodable mode).
6. `SetLayerActive("f", false)` → no `f` packets for 1 s while `q` and audio go on; `SetLayerActive("f", true)` →
   `f` resumes with a keyframe within 500 ms.
7. `Reconfigure{Profile: "42e0"}` → the next `f` and `q` packets are `Reinit` keyframes whose SPS has
   `profile_idc` 66 with `constraint_set1`, and the following `EventLayer`s report `Profile` "42e0".
8. `Stop` while `Next` blocks → `ErrStopped` within 200 ms; with `o.Listed`, `Start` again with the same `Source`
   works (a picked source would be consumed).
9. A second `Start` with `Audio` while a session with audio runs → `errors.Is(err, media.ErrBusy)`.
10. `Session.Now()` is within 50 ms of the latest **video** packet's `CaptureNS` plus the time since that packet
    arrived. For audio (stamped `tick − L − look-ahead`, 05 §10.3, about 55–80 ms old when emitted, §12.1), only
    `CaptureNS ≤ Now()` and the 20 ms spacing of check 3 are asserted.

With `Encoder: "fake"` (IM_ENC_FAKE), checks 5 and 7 run only once 04 specifies IM_ENC_FAKE's output (a bitrate
that follows the target, a CB profile); until then they are skipped with a message.

The fake runs it in CI on every OS (`src` = `mon:fake`, `Listed`). The Windows engine runs it on windows-2025 (06's
`windows-desktop`) with the `win-x64-ci` DLL, `test:pattern`, `{Listed, TestTone, Encoder: "fake"}`, and on the
owner's PC runner with a real monitor (`Listed`) per probed encoder (§14).

## 7. The Windows binding (`internal/media/native`, M2)

04 §12 owns and implements `internal/media/native` against this contract; 04 §18 tests it. This section is what
the core needs from it.

### 7.1 Opening the engine

```go
package native

// Open loads the engine. On windows it loads isshoni-media.dll, checks its ABI and calls im_init; elsewhere (until
// M3/M4) it returns an error that wraps media.ErrUnsupportedOS. The returned *Engine implements media.Engine and 05's
// media.AudioController. (04 §12.2 owns it; the shape is what 03 and the core rely on.)
func Open(o Options) (*Engine, error)

type Options struct { // 04 §12.2's fields
	Dir          string       // directory of isshoni-media.dll; "" = the executable's
	FFmpegDir    string       // "" = Dir, where the FFmpeg DLLs are installed (im_init_params.ffmpeg_dir = NULL)
	OpenH264Path string       // "" = none; must have passed VerifyOpenH264
	Logger       *slog.Logger // engine logs with component=engine
	LogLevel     slog.Level   // mapped to IM_LOG_*
	NoAudio      bool         // IM_INIT_NO_AUDIO
}
```

Requirements the binding meets (04 §12 implements them; 04 §18 asserts them):

1. **Loading**: `windows.LoadLibraryEx(path, 0, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32)` with an
   absolute path, so the DLL's own dependencies (the FFmpeg DLLs) resolve from its directory and System32 only, never
   from `PATH` or the working directory. The DLL is never unloaded.
2. **ABI check** (04 §5.5): `im_version()` major == `media.ABIVersion` (1) and minor ≥ `goMinMinor` (0 in M2); every
   required export resolves: the header's video and session exports and 05's non-test audio exports
   (`im_audio_watch`, `im_audio_unwatch`, `im_set_exclusions`, `im_audio_set_mode`, `im_audio_set_muted`,
   `im_list_audio_apps`, `im_audio_levels_get`, `im_audio_sound_check`, as 04's header carries them, bound by 05's
   `internal/media/native/audio_windows.go`). Exports of newer minors resolve as optional and gate their feature;
   `im_test_*` resolve only through `native.AudioTestHooks`. A failure is `CodeDependency` with Reason `abi` and a
   detail naming the export or both versions. `Info().ABI = int(im_version() >> 16)`, `Info().Build =
   im_build_info().engine`.
3. **Thread affinity**: every call that can fail runs under `runtime.LockOSThread` together with its `im_last_error`
   read (thread-local), as S1's `engine.do` did.
4. **Serialization**: one mutex per Session covers only the session control calls (keyframe, bitrate, layer active,
   profile and layer updates, paused, stop); `im_next_packet` is never under it. 05's audio exports are called
   without it (some block: a one-shot `im_list_audio_apps` takes ≤ 500 ms with a 3 s COM limit, `im_audio_sound_check`
   1.5–3 s, and 05's Service calls `im_audio_levels_get` at 10 Hz), so they never delay `im_request_keyframe` or
   `im_set_bitrate` mid-share.
5. **Structs, not JSON**: input structs and their strings are heap-allocated (`new`, `windows.BytePtrFromString`) and
   kept alive with `runtime.KeepAlive` until the call returns; the engine copies everything during the call
   (04 §12.4, 05 §4.3, §13.2). The mapping:

   | `media` | Native |
   |---|---|
   | `Config.Source` | `source_id` (NULL when "") |
   | `Config.Listed` | flag `IM_START_ALLOW_LISTED_SOURCE` |
   | `Config.Borderless == false` | flag `IM_START_KEEP_BORDER` |
   | `Config.Profile` "6400" / "42e0" | `IM_PROFILE_HIGH` / `IM_PROFILE_CB` |
   | `Config.Preset` | content hint: auto → `IM_CONTENT_AUTO`; game, movie → `IM_CONTENT_MOTION`; text → `IM_CONTENT_DETAIL` |
   | `Config.Encoder` | "" → `IM_ENC_AUTO`; nvenc, qsv, amf, mf-hw, mf-sw, openh264 → `IM_ENC_NVENC` … `IM_ENC_OPENH264`; fake → `IM_ENC_FAKE` |
   | `Config.Layers[i]` | `layers[i]`: `rid`, `max_pixels`, `fps` = MaxFPS, `bitrate_bps` = Bitrate, `gop_ms` 0 (engine default), `IM_LAYER_INACTIVE` when Inactive; `layer_count` |
   | `Config.Audio == nil` | audio mode off (no audio packets) |
   | `Config.Audio` (05's `AudioConfig`) | 05's `im_audio_config`: `mode` through 05's single mode mapping (05 §13.1: "" and exclude-apps → 1, test-tone → 100 `IM_AUDIO_MODE_TEST_TONE`; everything and off → `CodeInvalid` without a call in M2), `bitrate_bps` = BitrateBPS, `frame_ms` 20, `flags` (`IM_AUDIOF_TAP_PCM` when TapPCM, plus TestFlags, which carries only `IM_AUDIOF_TEST_FAIL_PROBE`), and `im_exclusions` (flags from KeepOutMicUsers/FastPath, `rules`, `shared_pid` = SharedPID) |

6. **Next**: a loop of `im_next_packet(s, 50, &pkt)`; the timeout status re-checks ctx and loops, so a cancelled ctx
   returns within 50 ms. The timeout status is looped only here; other calls return it as `CodeTimeout`. 04's event
   queue is merged into `Next`: events are returned as `KindEvent` packets (events first, as 04 orders them), mapped
   by §5.2's table, with an `EventLayer` at start and before every `REINIT` video packet; `ENDED` becomes `Next`'s
   fatal error. `CaptureNS = capture_ts_ns − epoch_ns`; `Session.Now()` returns the engine clock (QPC in ns with the
   header's formula) minus the same `epoch_ns`.
7. **Copy**: before the next call on the session, `Data` is copied into a **fresh** `[]byte` (the native slot is valid
   only until then, plan "packet memory stays owned by native code until the next call, and Go copies it"). No
   pooling in M2: an 8 Mbit/s layer allocates about 1 MB/s, which the GC handles; a pool would have to prove that no
   payload slice is still referenced by the payloader or Pion's NACK responder.
8. **Validation**: a video packet's `layer` must be an index below the session's layer count; audio and PCM packets
   carry the header's audio-layer value (05's `IM_LAYER_AUDIO` 255, 04's `IM_NO_LAYER`; the header settles one) and
   become `Packet.Layer` ""; only the reinit bit is valid in an audio packet's flags (keyframe 0, 05 §4.2); a PCM chunk
   is exactly 3840 bytes; sizes are bounded by 04 §6.2's ring arenas. An invalid packet is dropped and logged at warn
   with its size, and a video layer gets a keyframe request.
9. **Pick** runs `im_pick_source` on its own goroutine (never on Wails's UI thread); ctx's end calls `im_cancel_pick`.
   `Source.Exe` is derived from the pick's `pid` (§5.1).
10. **Tests without Windows**: the binding's logic runs against a fake caller on the Mac and CI ubuntu (04 §18.3);
    only the DLL tests need Windows (04 §18.2).

## 8. The publish path (`internal/client/publish`, M2)

### 8.1 Packetization

Unchanged from M1 (M1 02 §15.1): one video transceiver with an encoding per rid (`f`, then `q`; one layer goes out
without a rid), `H264Payloader` at MTU 1200 with SPS and PPS aggregated into a STAP-A, the marker bit on the last
packet of a frame, one RTP packet per Opus packet, `sdes:mid` and `rtp-stream-id` stamped by the track, TWCC sequence
numbers by the interceptor.

```go
// Source is what a Publisher sends. Packets come in capture order per stream (kind + layer); there is no order
// across streams (04 §6.2). M1's anchoring handles a packet older than one already sent on another track (rtpTicks
// takes negative offsets).
type Source interface {
	Next(ctx context.Context) (media.Packet, error)
	RequestKeyframe(layer string)
}

// Clock is implemented by a Source whose CaptureNS is not "now" when Next returns it (a real engine, §8.2).
type Clock interface {
	Now() int64 // the current time on the clock of Packet.CaptureNS
}
```

`ProfileSource` keeps M1's shape (`SetProfile(profile string) error`). A packet of `KindEvent` or `KindPCM` reaching
the Publisher is dropped (the core's pump never passes one).

### 8.2 Timestamps and sender reports

- `rtp_ts = base + (capture − t0) × clock` with one `t0` per Publisher for all tracks (M1). `base` is random per track
  (RFC 3550 §5.1).
- **Anchor** (D6): at the first packet a Publisher sends, `cap0 = pkt.CaptureNS` and
  `wall0 = time.Now() − (src.Now() − pkt.CaptureNS)` when the Source is a `Clock`, else `wall0 = time.Now()` (M1's
  rule for the fake). Every SR then carries `NTP = wall0 + (capture − cap0)` of the last frame sent and that frame's
  RTP timestamp. The anchor is fixed for the Publisher's life: a wall-clock step changes nothing, since receivers use
  SRs only to align audio with video and to estimate capture times.
- A new Publisher (rebuild, republish) anchors anew; its tracks have new SSRCs and bases.

### 8.3 Keyframes

PLI and FIR call `Source.RequestKeyframe(layer)` (M1). The core's pump throttles what reaches the engine to one
request per layer per 300 ms and drops requests while the share is paused (D14), remembering that a keyframe is owed
at resume.

### 8.4 NACK and RTX (D7)

- No RTX payload types are offered (unchanged default; `RedundantRTX` stays a test option).
- `nack.NewResponderInterceptor(nack.ResponderSize(4096))` on every Publisher (M1 used Pion's default of 1024).
- What the SFU does with it (M1 02 §8.1, §9.1, §9.5): its NACK generator asks for a missing sequence number every 50 ms,
  at most 10 times, and its filter stops asking once the packet is in its cache; a plain retransmission arrives on the
  media SSRC with its original sequence number and fills the cache slot. Audio is repaired the same way.

### 8.5 Pacing (D8)

- Video packets of all layers go through one token bucket per Publisher: rate
  `max(3 × Σ targets of the active video layers, 6 Mbit/s)`, depth 40 ms at that rate, starting full. The targets are
  the ones the core last applied (`SetPacingTargets(map[string]int)`, called by the core after every rate change).
- The send loop packetizes and hands video packets to a writer goroutine (`pacer.go`) through a queue; audio packets
  are written by the send loop directly, never behind video.
- **Backpressure, never drops**: when the queue holds more than 300 ms of data at the pacing rate, the send loop
  blocks before taking the next video packet from its Source. Dropping is the pump's job (§11.5), which drops whole
  frames and asks for a keyframe; a packet dropped here would cost a NACK round trip or a PLI.
- Numbers: an 8 Mbit/s full layer paces at 24 Mbit/s (25.2 with `q`); from a full bucket a 300 KB IDR leaves in
  about 60 ms after the bucket's 120 KB, which a GOP of 4 s makes rare (§12.1).
- **Opt-in**: `Options.Pacing` (default off) turns the pacer on, so the load test, `sfutest` and every M1 test keep
  M1's behavior and numbers. The core always sets it and calls `SetPacingTargets` with the initial targets before
  media flows; a pacing Publisher without targets uses the 6 Mbit/s floor.

### 8.6 Profiles and the codec switch

- **Engine profile keys** are `6400` and `42e0` only (04: `IM_PROFILE_HIGH`, `IM_PROFILE_CB`); the binding rejects
  others with `invalid`. `640c` is an **SDP alias**: when the session runs High, the Publisher advertises `640c` next
  to `6400` for the same bitstream (plan: native senders use High, "advertised as `64001f` and `640c1f`"); WebRTC
  encoders never emit B-frames, so it decodes as Constrained High (M1 02 §8.2). No constraint flags are set in the SPS.
- **Offered profiles**: `Profile` = the profile the session really runs, from its first `EventLayer.Profile` (04 may
  run Constrained Baseline when only a CB encoder opened, 04 §9.6), not the request; `Profiles` = the others in the
  order `6400`, `640c`, `42e0` (`640c` only next to `6400`; `42e0` when an available encoder lists it).
- A `quality.hint` with `codec` (M1 02 §8.3: High → Constrained Baseline when a Firefox viewer joins, and back after
  60 s) runs M1's path: `Publisher.SetProfile`, a re-offer (same `gen`, `neg + 1`), and on the answer the send loop
  calls `ProfileSource.SetProfile`, which the pump forwards as `Session.Reconfigure{Profile}`. Each layer switches
  payload type at its first `Reinit` keyframe. A hint naming the profile already sent needs nothing (01 §8.10). If
  the `EventLayer` after that re-init reports another profile than requested (a CB-only encoder stays CB and returns
  OK, 04 §9.6), the core re-offers with the actual profile.

### 8.7 Statistics

`TrackStats` (M1) gains, per track:

```go
type TrackStats struct {
	// … M1 fields …
	Bitrate       int           // bit/s over the last 2 s (payload + RTP headers)
	FPS           float64       // frames over the last 2 s
	CaptureToSend LatencyStats  // video: from CaptureNS to the moment the frame's last packet left the pacer
	RTT           time.Duration // from the SFU's receiver reports (LSR/DLSR against this track's SRs); 0 until known
}

type LatencyStats struct {
	P50, P95, Max time.Duration // over the last 10 s
	Samples       int
}
```

`CaptureToSend` needs a `Clock` Source; without one it stays zero.

## 9. Uplink rate control (`internal/client/ratectl`, M2)

Pure logic, owned by one goroutine (the share's actor, §11.4); no Pion, no I/O. Time comes in as arguments, so tests
drive it with fixed instants.

### 9.1 API

```go
package ratectl

type Config struct {
	Live         map[string]bool      // per layer: its encoder changes bitrate live (each layer's first EventLayer.LiveBitrate, D10)
	Params       protocol.ShareParams // the share.start reply
	StartBps     int                  // the full layer's start; 0 = DefaultStartBps
	SourceAspect float64              // width/height of the source, for the upload hint; 0 = 16/9
	TargetHeight int                  // f's encoded height (from the engine's EventLayer); updated with SetTargetHeight
}

type Controller struct{ /* not safe for concurrent use */ }

func New(cfg Config) *Controller
func (c *Controller) Initial() []Action                                  // the targets for media.Config.Layers and Audio
func (c *Controller) Live(now time.Time)                                 // the share went live: the ramp starts 2 s later
func (c *Controller) Params(p protocol.ShareParams, now time.Time) []Action // a share.update reply (preset change)
func (c *Controller) Hint(h protocol.QualityHint, now time.Time) []Action
func (c *Controller) SetLive(layer string, live bool, now time.Time)     // a later EventLayer (encoder fallback, re-init)
func (c *Controller) SetLimit(limit string, now time.Time)               // media.EventLimit: "cpu" | "gpu" | "none"
func (c *Controller) SetTargetHeight(h int)
func (c *Controller) Tick(now time.Time) []Action                        // every 500 ms
func (c *Controller) State() State
func (c *Controller) Upload(now time.Time) UploadHint
func (c *Controller) CPU(now time.Time) bool                             // the CPU hint (§9.5)

type Op uint8

const (
	OpBitrate Op = iota + 1 // Session.SetBitrate(Layer, Bps)
	OpActive                // Publisher.SetActive(Layer, Active) and Session.SetLayerActive(Layer, Active); a resume carries the layer's target in Bps, applied with SetBitrate in the same step
	OpAudioBitrate          // Session.SetBitrate(media.LayerAudio, Bps)
)

type Action struct {
	Op     Op
	Layer  string
	Bps    int
	Active bool
	Reinit bool // the change costs an encoder re-init and an IDR (logs and stats)
}

type State struct {
	Cap       int            // the server's total video cap in bit/s; 0 = none
	Ceiling   map[string]int // per layer: min(encodings' maxBitrate) after the admin cap
	Applied   map[string]int // what the engine was last told
	Wanted    map[string]int // where the controller is heading (applied after the guards below)
	Active    map[string]bool
	Ramping   bool
	Limit     string         // the last SetLimit value ("" = none reported)
}

type UploadHint struct {
	Limited      bool
	ApproxHeight int // 360 | 480 | 540 | 720 | 900 | 1080; 0 when not limited
}
```

### 9.2 Ceilings, cap and split

- **Ceilings** come from the latest complete encodings list: `ShareParams.Encodings` (start, `share.update`) or a
  hint's `Encodings`; each layer's `MaxBitrate` already includes the admin cap (M1 02 §8.6). `Active` flags come from
  the same list (layer pausing, M1 02 §11).
- **Cap**: `QualityHint.MaxBitrate`, the server's total for the share's video layers (audio excluded); 0 or absent
  means none. M1 servers send it with `admin` and `viewers` hints (the admin's cap); M2 servers also with
  `congestion` (§10.1).
- **Split** of a total `T` (the cap when set, else the sum of ceilings) over the active layers:
  - `q` gets `min(q.ceiling, max(150 kbit/s, 0.3 × T))`; `f` gets `min(f.ceiling, T − q)`, at least 250 kbit/s;
  - a single active layer gets `min(ceiling, T)`;
  - so a cap of 8 Mbit/s leaves `f` 7.7 Mbit/s; a cap of 600 kbit/s gives `q` 180 kbit/s and `f` 420 kbit/s; a cap
    below 400 kbit/s sends the floors (150 + 250) and the upload hint says 360p.
- **Wanted** for `f` = `min(split f, ramp)` while ramping, else `split f`. `q` and audio have no ramp.
- **Engine ranges**: every output is clamped to 04 §6.4's ranges: `f` 100 kbit/s–50 Mbit/s, other layers
  50 kbit/s–2 Mbit/s, Opus 32–510 kbit/s. `FloorF` (250 kbit/s) and `FloorQ` (150 kbit/s) already fit.

### 9.3 Ramp

- Start: `f` at `min(DefaultStartBps (4 Mbit/s), f.ceiling, split f)` (plan: Auto "starts at ~4 Mbps and ramps to
  8 Mbps"); every preset uses the same start in M2.
- From 2 s after `Live`, every 2 s while no cap binds below the ramp and the encoder is not limited (`SetLimit` cpu or
  gpu) now or in the last 10 s: `ramp = ramp × 1.15`, until it reaches `f.ceiling` (4 → 8 Mbit/s in 5 steps, 10 s).
- A congestion hint that lowers `f` holds the ramp for 10 s; after that it grows again toward the cap (or the ceiling
  once the server lifts the cap).
- When `f`'s encoder has no live bitrate (`Live["f"]` false), it ramps in **one step**: 10 s after `Live`, straight
  to `min(f.ceiling, split f)` when that is ≥ 30 % above the start. On an uplink slower than that step, the
  backoff can apply only 10 s later (§10.1's known limit).
- The ramp runs while the server sets no cap. The server's queueing-delay rule (§10.1) is what stops it at a slow
  uplink before the router's buffer fills: the first backoff sets a cap, and the ramp is then held by it.

### 9.4 Guards (plan: changes of at least 15 %, at most every 2 s)

For each layer, `Wanted` becomes `Applied` (an `OpBitrate`) when (chosen **per layer** from `Live[layer]`, updated by
`SetLive`):

| The layer's encoder | Applies when |
|---|---|
| live (`Live[layer]`) | `wanted` differs from `applied` by ≥ 15 % of `applied` (either way), and ≥ 2 s since the layer's last change; otherwise the change waits and the latest `wanted` is applied at the next `Tick` that passes |
| re-init | `wanted` differs from `applied` by ≥ 30 % of `applied` (either way), and ≥ 10 s since the layer's last change (plan); smaller changes are not applied at all until they grow to 30 % |

- **Exceptions**: a layer turned active or inactive (`OpActive`) applies at once, with the resumed layer's target in
  the same step; the first targets (`Initial`) and a `share.update` that changes the preset (new ceilings) apply at
  once.
- Audio: `OpAudioBitrate` at once when `ShareParams.AudioBitrate` changes (Opus changes bitrate live).
- The server already spaces its congestion hints (§10.1); the client's guards matter for the ramp, for re-init
  encoders and for hints of different reasons arriving close together.

### 9.5 The upload hint (D11)

- Required bitrate for height `h` at the layer's frame rate: `req(h) = 0.05 × (h × aspect) × h × fps` bit/s
  (1080p60: 6.2 Mbit/s; 720p60: 2.8; 540p60: 1.6; 360p60: 0.7; half of each at 30 fps).
- `approx = max{h ∈ 360, 480, 540, 720, 900, 1080 : req(h) ≤ applied f}` (360 if none).
- **Limited** when the server's cap binds (the split for `f` from the cap is below `f.ceiling`), and
  `approx < 0.95 × TargetHeight`, for 3 consecutive `Tick`s 2 s apart (6 s), the web sharer's rule (M1 05 §13.7).
  It clears after 10 s without that condition. The ramp alone never shows it.
- The app shows "Your upload allows about {approx}p" (03); the same `ShareHint {kind: 'upload-limited', approxHeight}`
  shape as the web sharer.
- **CPU hint**: `CPU(now)` is true while the last `SetLimit` was `cpu` or `gpu`. 04 already applies the hysteresis
  (limited after 3 s below 90 % of the target rate, clear after 10 s above 95 %), so the controller adds none. The
  share snapshot's `Overloaded` (§11.9) shows it, with the web's CPU hint text (M1 05 §13.7). A limit also holds the
  ramp while it lasts and for 10 s after (§9.3).

### 9.6 Constants

| Name | Value |
|---|---|
| `DefaultStartBps` | 4 000 000 |
| `RampFactor`, `RampEvery`, `RampDelay` | 1.15, 2 s, 2 s after live |
| `HoldAfterCongestion`, `HoldAfterLimit` | 10 s, 10 s |
| `MinChange`, `MinInterval` (live) | 15 %, 2 s |
| `ReinitMinChange`, `ReinitMinInterval` | 30 %, 10 s |
| `FloorF`, `FloorQ`, `QShare` | 250 kbit/s, 150 kbit/s, 0.3 |
| Engine ranges (04 §6.4) | `f` 100 kbit/s–50 Mbit/s; other layers 50 kbit/s–2 Mbit/s; Opus 32–510 kbit/s |
| `TickEvery` | 500 ms |
| Upload hint on / off | 6 s (3 samples 2 s apart) / 10 s |
| `BitsPerPixel` | 0.05 |

## 10. Server counterparts (M2)

These live in `internal/server/{sfu,signal}` and `internal/protocol`. They follow the M1 conventions (README §4) and
the M1 specs that own those packages (`docs/m1/01-protocol.md`, `docs/m1/02-sfu.md`); this section is the M2 delta.

### 10.1 The native uplink estimator (`internal/server/sfu/uplink_native.go`)

M1 02 §11's "Later (M2), native publishers" item.
- **Selection**: at `StartShare`, a share whose publishing Conn's `ClientKind` is `desktop` or `mobile` gets
  `nativeUplink`; every other kind keeps `m1Uplink`. `nativeUplink` embeds `m1Uplink`, so the admin's cap and layer
  pausing behave exactly as in M1, and adds an estimate.
- **Input**: `ShareUplinkStats` once a second (the existing ticker), plus two new fields per layer:
  - `LayerUplinkStats.RawLossPct float64`: the share of sequence numbers missing when a later one arrived, over the
    last 2 s, **before** any retransmission (the Layer's RTP loop counts each gap at the new highest sequence number;
    padding counts as received). M1's `LossPct` is after repair and hides congestion that NACKs fix.
  - `LayerUplinkStats.QueueDelay time.Duration`: the layer's queueing-delay trend (next item); −1 when unknown.
- **Queueing delay** (the delay-trend signal). A native publisher's RTP timestamps come from capture times (§8.2), so
  arrival time minus RTP time changes only with the path's queue, plus clock drift between the two machines:
  - **Sample**: the first packet of each video frame, which is the packet `sampleJitter` already takes (a new RTP
    timestamp; M1 02 §9.1). Retransmissions, padding and a timestamp older than the last sample's (reordering) never
    sample. `owd = arrival − ext_ts × 10⁹ / 90 000` in ns. `arrival` is the RTP loop's monotonic receive time.
    `ext_ts` is the timestamp unwrapped to 64 bits (`ext += int64(int32(ts − last_ts))`). The constant offset (random
    RTP base, unrelated clocks) cancels against the baseline.
  - **Window**: `owdMin` is the minimum of the samples of the last 500 ms (`nativeDelayWindow`), kept in a monotonic
    min-deque of at most 256 entries (240 fps × 0.5 s, with room).
  - **Baseline**: the minimum since the Layer started, allowed to rise slowly. On each tick,
    `baseline = min(baseline + 0.5 ms × seconds since the last tick, minimum of the samples since the last tick)`
    (`nativeBaselineRise`: 0.5 ms per second, 500 ppm). It follows a sharer clock that runs slower than the server's
    (crystal drift, an NTP slew) and absorbs a standing queue only slowly (a 40 ms queue after ≥ 80 s). A queue that
    drains brings it back down at once. A plain 10 s minimum would turn a standing queue into the baseline after
    10 s, and the growth row would then let the queue grow by another step.
  - `QueueDelay = owdMin − baseline`, computed on the tick. It is −1 (unknown) when the window has fewer than 3
    samples (a static screen sends few frames) and during the Layer's first 2 s.
  - A new Layer (republish, new SSRC) starts a new baseline. A layer pause keeps it, because the clocks don't change.
    An encoder re-init or a profile switch keeps the Publisher's RTP timeline (§8.2), so the baseline stays too.
- **Per tick**, over the active video layers: `rate = Σ Bitrate`, `loss = Σ(RawLossPct × Bitrate) / rate`,
  `delay` = the `f` layer's `QueueDelay` (else `q`'s; −1 = unknown). The rows are checked top-down and the first
  that matches decides:

| Condition | Estimate |
|---|---|
| `rate < 250 kbit/s` (too few packets for a verdict), a sharer-paused share (§10.3), or within 3 s of a layer resume | unchanged |
| `loss ≥ 10 %` on this tick, or `loss ≥ 3 %` on 2 consecutive ticks | `max(500 kbit/s, 0.8 × rate × (1 − loss/100))`; growth held for 10 s |
| `delay ≥ 40 ms` on 2 consecutive ticks | `max(500 kbit/s, 0.85 × rate)`; growth held for 10 s |
| estimate set, `loss < 1 %` on 2 consecutive ticks, `delay < 15 ms` (or unknown) on 2 consecutive ticks, `rate ≥ 0.85 × estimate`, ≥ 2 s since the last growth, not held | `estimate × 1.10`; when it reaches the sum of the preset ceilings × 1.1, the estimate is cleared (none) |

  A backoff row never raises a set estimate: the new estimate is the smaller of the row's value and the current one.
  "Never above ~80 % of measured uplink" (plan) is the 0.8 factor: the estimate after loss is 80 % of what got
  through.
- **Why a delay rule** (the plan's "loss and receive-rate estimator" doesn't exclude one). A home router's uplink
  buffer often holds 0.5–2 s, and loss starts only when it overflows. A loss-only estimator therefore lets latency
  grow to seconds before it reacts, and then growth (this estimator's +10 % per 2 s, or the client's 4 → 8 Mbit/s
  ramp, §9.3) refills the buffer in a sawtooth. Any friend whose uplink is below about 8–10 Mbit/s would see it, and the exit run
  on a good link would not. The delay rule backs off at a 40 ms queue, before any loss. Growth waits until the queue
  is below 15 ms. Two consecutive ticks over the 500 ms minimum keep a single IDR burst from counting: under 04
  §9.3's CBR buffer the burst is followed by smaller frames, and its queue drains within about a second. M1's jitter
  rule is gone, because it couldn't fire: M1's jitter takes one sample per frame against the previous frame's last
  packet (M1 02 §9.1, `sampleJitter`). On a saturated link the next frame's first packet arrives right behind, so
  |D| ≈ the frame interval minus one packet's serialization, about 15 ms at 60 fps and never 30 ms.
  `LayerUplinkStats.Jitter` stays for `m1Uplink` and the dashboard.
- **Timing**: with the worst +15 % ramp step over capacity, the queue grows about 150 ms per second. The rule fires
  on the second qualifying tick, about 2–3 s after the step. A live encoder applies the hint at its next allowed
  change, at most 2 s after its last one (§9.4), and 0.85 × the arrival rate then drains the queue at 15 % of the
  link's rate. D19 (§14) asserts the outcome. **Known limit**: a re-init encoder (AMF, MF through FFmpeg) may change
  only every 10 s (plan). Its one-step ramp (§9.3) can overshoot a slow uplink until then. The delay rule has the
  backoff ready at that moment, but cannot shorten the 10 s.
- **Decision**: `MaxBitrate = min(admin cap, estimate)` (0 = none), `Reason = "congestion"` when the estimate is the
  binding one, else as M1. `UplinkDecision` gains `Reason string` (additive; `m1Uplink` leaves it empty and M1's
  rule chooses).
- **Hints**: a congestion change goes out as `quality.hint{reason: "congestion", maxBitrate, encodings}` only when it
  differs from what the sharer was told by ≥ 15 % (or between none and some) **and** ≥ 2 s after the share's last
  hint (plan hysteresis). Admin and pause changes keep M1's timing (at once). Every hint carries the full encodings
  under the admin cap with the pause state (M1 rule); `maxBitrate` carries the dynamic total.
- REMB: M1's one-REMB-per-pub-PC logic is unchanged and uses the decision's cap; a Pion publisher only counts it.

```json
{"type": "quality.hint", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "reason": "congestion", "maxBitrate": 2400000,
  "encodings": [
    {"rid": "f", "layer": "high", "active": true, "maxBitrate": 8000000, "maxFramerate": 60, "maxPixels": 2073600},
    {"rid": "q", "layer": "low", "active": true, "maxBitrate": 300000, "maxFramerate": 15, "maxPixels": 230400}]}}
```

- Constants live in `uplink_native.go` (`nativeLossHigh` 10, `nativeLossMid` 3, `nativeLossLow` 1, `nativeBackoff`
  0.8, `nativeDelayHigh` 40 ms, `nativeDelayLow` 15 ms, `nativeDelayBackoff` 0.85, `nativeDelayWindow` 500 ms,
  `nativeBaselineRise` 0.5 ms per s, `nativeDelayMinSamples` 3, `nativeGrowth` 1.10, `nativeHold` 10 s,
  `nativeMinCap` 500 kbit/s, `nativeMinRate` 250 kbit/s, `nativeHintMinChange` 0.15, `nativeHintMinGap` 2 s). They
  are tuned only with §14's integration cases D9, D10 and D19 passing.
- Metrics: `isshoni_sfu_uplink_hints_total{reason}` gains the `congestion` value; the dashboard's per-share row (M1 04
  §11.4) shows the cap.
- `sfutest.FaultConn` gains `SetInboundLink(peer net.Addr, l *Link)`: a bottleneck for datagrams **from** a client,
  the uplink twin of M1's `SetLink` (nil removes it). It takes either kind of `Link`:
  - M1's **policer**, `NewLink(capacity)`: a token bucket with a 100 ms burst (`LinkBuffer`) and no queue. What comes
    faster is lost. This is what D9 uses.
  - A new **queueing** link, `NewQueueLink(capacity int64, limit time.Duration) *Link`: a FIFO delay line, like a home
    router's uplink buffer. Each datagram departs at `max(now, previous departure) + size × 8 / capacity`. An RTP
    datagram whose wait would exceed `limit` is lost (drop-tail in time). STUN, DTLS and RTCP queue like the rest but
    are never lost, as in M1's Link. `(*Link).Queued() time.Duration` returns the current wait, and `Counts()` works
    as in M1. While a queueing link is set, `FaultConn` reads the socket on its own goroutine, and `ReadFrom` returns
    each datagram at its departure time (other peers' datagrams pass at once). `Close` stops the goroutine. M1's
    policer can't build a deep queue, so bufferbloat can't be tested on `link.go` as it stands.
  - `SetLink` (outgoing) keeps M1's policer only. A queueing Link passed to it panics.

### 10.2 What the server does not do in M2

- No `quality.hint{reason: "preset"}` push (D13): the publishing client reconciles from `room.state`.
- No GCC or TWCC-based estimation on the publish side: the SFU keeps generating TWCC feedback, which the native
  client ignores.

### 10.3 `share.pause` (protocol, hub, SFU)

Wire, additive (01 §14.1):

```go
type ShareUpdate struct {
	ShareID string  `json:"shareId"`
	Label   *string `json:"label,omitempty"`
	Preset  Preset  `json:"preset,omitempty"`
	Paused  *bool   `json:"paused,omitempty"` // M2, feature share.pause: ignored without it
}

const ShareStatusPaused ShareStatus = "paused" // M2, feature share.pause; old clients treat it as live (01 §4.4)
```

```json
{"type": "share.update", "id": "14", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "paused": true}}
{"type": "room.state", "data": {"roomId": "lounge", "rev": 42,
  "participants": [{"userId": "k3m9p2qxw7ht", "name": "Alex", "status": "present",
                    "joinedAt": "2026-11-20T19:03:58.004Z",
                    "connections": [{"id": "c_k3v9q2m7xw4pa8d1", "kind": "desktop", "role": "publisher",
                                     "status": "online"}]}],
  "shares": [{"id": "s_q7m2x9c4v8b1n5k3", "userId": "k3m9p2qxw7ht", "connectionId": "c_k3v9q2m7xw4pa8d1",
              "kind": "window", "preset": "auto", "audio": true, "status": "paused", "layers": ["high", "low"],
              "codec": "h264/6400", "startedAt": "2026-11-20T19:04:05.123Z", "watchers": []}]}}
```

- **Hub**: the welcome lists `share.pause` when the server supports it (M2 servers do). `paused` is read only from a
  connection that has the feature active; otherwise the field is ignored (no error), so old and new clients
  interoperate. Any connection of the user may set it (01 §4.1 ownership). The hub keeps a `paused` bit per share:
  status is `stalled` while the pub PC is down (stalled wins), else `paused` while the bit is set, else
  `live`/`starting` as in M1. Pausing a `starting` share stores the bit; the share shows `paused` once it would be
  live. No timeout applies to `paused`. No `room.event`; `room.state` carries the change. The reply is the share's
  `ShareParams`, as for every `share.update`.
- **SFU** (`MediaPeer.UpdateShare` already receives the `ShareUpdate`): `Share.sharerPaused` (an atomic, named apart
  from the layer-pausing bits `Share.paused`) makes keyframe requests for the share do nothing (no PLIs at a share
  that sends nothing, and no throttle spent) and makes `nativeUplink` skip its ticks. On unpause the SFU requests one
  keyframe per video layer that has viewers, like a layer resume (M1 02 §11).
- **Clients**: the SPA shows a paused share's tile and stage with a "Paused" overlay and keeps the last frame for
  viewers who had video; it must not show "connection unstable" for a paused share (03 owns the SPA's M2 changes).

### 10.4 `user.connections` (hub)

The M1 registry entry (feature `user.connections`, 01 §8.14) gets its server behavior:
- To every **online** connection of a user that has the feature active, the hub sends the user's full connection
  list: after its `welcome`, and whenever any of the user's connections opens, closes, detaches, re-attaches or
  changes room, and after a `Revalidate` that changed a connection's `DeviceName`. Coalesced to at most one per
  200 ms per user; the list is sorted by `connectionId`. A device rename therefore reaches the list up to 5 min late
  (the hub's revalidation interval, M1 01 §3.2).
- Content: `OwnConnection` per connection of the user (all of them, the recipient included, detached ones with
  status `reconnecting`), with one additive field:

```go
type OwnConnection struct {
	ConnectionInfo `tstype:",extends"`
	OS             ClientOS `json:"os"`
	Version        string   `json:"version"`
	RoomID         string   `json:"roomId,omitempty"`
	DeviceName     string   `json:"deviceName,omitempty"` // M2: the device's name for a bearer connection (02's Identity.DeviceName); may lag a rename by ≤ 5 min
}
```

```json
{"type": "user.connections", "data": {"connections": [
  {"id": "c_4hq8m2v7kx9p3w1d", "kind": "web", "role": "full", "status": "online", "os": "windows",
   "version": "0.2.0", "roomId": "lounge"},
  {"id": "c_k3v9q2m7xw4pa8d1", "kind": "desktop", "role": "publisher", "status": "online", "os": "windows",
   "version": "0.2.0", "deviceName": "Gaming PC"}]}}
```

- It is how a web tab learns that the user's desktop app is online, for 03 §12.3's "Open in the isshoni app", and
  which connection id (the `desktop` connection with role `publisher`) to address with `playback.state` (§11.11).
  Only the user's own connections ever see it (01 §17: OS and version stay private).

## 11. The client core (`internal/client/core`, M2)

### 11.1 Connection model

The desktop app runs two signaling connections to its linked server, both authenticated with the same device's
bearer access token (02), so the server sees one user and, in a room, one Participant (01 §4.1):

| | Go connection (this package) | SPA connection (03) |
|---|---|---|
| Role | `publisher` | `viewer` |
| `client.kind` | `desktop` | `desktop` |
| Features | `agent.relay` (to receive `playback.state`, §11.11), `share.pause` | as the SPA asks (`user.connections`, `agent.relay`, …) |
| In a room | only while it shares there, or briefly for `StopOther` (D12) | the room the window shows |
| PeerConnections | pub only | sub only |
| `caps` | `decode: []`, `encode`: `h264/6400` and `h264/640c` when an available encoder encodes High, `h264/42e0` when one encodes CB, `opus`; `simulcast: true`, `displayCapture: true` | the WebView2 decoder's caps |

```json
{"type": "hello", "id": "1", "data": {"protocol": 1, "minProtocol": 1,
  "features": ["agent.relay", "share.pause"],
  "client": {"kind": "desktop", "version": "0.2.0", "os": "windows"},
  "role": "publisher",
  "auth": {"scheme": "bearer", "token": "…"},
  "caps": {"decode": [], "encode": ["h264/6400", "h264/640c", "h264/42e0", "opus"],
           "simulcast": true, "displayCapture": true}}}
```

- Share limits count per user (4 across rooms, 01 §13). A share started by the Go connection belongs to the user, so
  a web tab of the same user may `share.stop` it (01 §4.1); the in-app SPA, role `viewer`, can't (01 §6.3) and goes
  through the core's `StopOther` instead (§11.2; 03's bindings).
- Watchers exclude the owner (01 §4.1): the user's own viewer connection watching the share is not counted.

### 11.2 API

```go
package core

type Options struct {
	ServerURL   string              // the linked server's origin: "https://watch.example.com" (02); http only for 02's loopback dev origins
	Auth        Auth                // 02's *deviceauth.Manager; nil only with Header
	Header      http.Header         // tests and tools only: a Cookie instead of a bearer token
	Client      protocol.ClientInfo // {Kind: desktop, Version: version.Version(), OS: windows}
	Engine      media.Engine
	// AudioConfig returns the audio start config (05's media.AudioConfig: mode, rules, SharedPID); 03 wires it to
	// 05's Service.AudioConfig(). The core calls it at each share's Engine.Start and overwrites BitrateBPS with the
	// share's ShareParams.AudioBitrate. nil = {Mode: media.AudioExcludeApps} (tests).
	AudioConfig func() media.AudioConfig
	Encoder     string                // "" = automatic; a probe token forces that kind (03's hidden support override, 04 §20)
	Settings    *webrtc.SettingEngine // nil = DefaultSettings(logger)
	HTTPClient  *http.Client
	Logger      *slog.Logger
	allowListed bool                  // in-package tests only (TestCaptureToSend): Engine.Start gets Config.Listed
}

// Auth is exactly what 02's *deviceauth.Manager provides.
type Auth interface {
	// AccessToken returns an access token, normally with at least 120 s left (02 §11.4); while a refresh is being
	// retried, possibly as little as 10 s, which is enough for one hello, since the token is checked once (M1 01
	// §3.2). forceRefresh drops the token the server refused; 02 does at most one forced refresh per 10 s and returns
	// its result to later forced calls. An error whose chain has a RelinkRequired() bool method returning true means
	// the device is no longer linked; one whose chain has UpdateRequired() bool returning true means the app and the
	// server are incompatible (02 §13); any other error is transient.
	AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)
	// SessionEnded reports a stop in scope session (session_revoked, account_disabled) or a second unauthenticated
	// right after a forced refresh. The manager refreshes once and, on invalid_grant, deletes the credential and sets
	// its notice (signed_out, account_disabled); it owns the credential and the link state.
	SessionEnded(ctx context.Context, code protocol.ErrorCode)
}

// DefaultSettings returns the Pion settings of §11.6: UDP and TCP (ICE-TCP to the server's 443/7882), IPv4 and
// IPv6, no mDNS, no loopback candidates, ICE timeouts disconnected 3 s / failed 25 s / keepalive 1 s, and logx's
// Pion logger (it redacts ICE passwords).
func DefaultSettings(log *slog.Logger) *webrtc.SettingEngine

func New(o Options) (*Client, error) // starts nothing
// Run connects and keeps the connection until ctx ends or a terminal stop (Status.Stop set); then it ends the share
// and returns. The error is nil when ctx ended, else a *StopError. A Client runs once: Run after it has returned, or
// after Close, returns ErrClosed. The app makes a new Client per (server, link) and calls Close before switching
// (server switch, relink, sign-out). Cancelling Run's ctx runs the same steps as Close, and Run returns when they
// are done (the same 3 s bound).
func (c *Client) Run(ctx context.Context) error
func (c *Client) Status() Status
// Subscribe returns a channel of events (64 deep; when a subscriber falls behind, its oldest events are dropped
// and EventResync tells it to read Status again) and a function that ends the subscription.
func (c *Client) Subscribe() (<-chan Event, func())
// StartShare joins r.RoomID, starts the share on the server and then on the engine (§11.4 steps 1–3), and returns
// once Engine.Start succeeded; the pub PC and the live state follow asynchronously (Snapshot().State). It returns
// ErrNotReady at once when Status().Conn != "online" (03 waits up to 10 s for EventStatus online itself), ErrBusy
// while a share exists, a *protocol.Error for room and share errors, and a *media.Error for engine start errors.
// Cancelling ctx before it returns aborts the start: share.stop if share.start succeeded, engine Stop, room.leave,
// and no share remains. 03 gives it a 10 s ctx after its own 10 s wait (03 §6.2), so both fit its 20 s cap.
func (c *Client) StartShare(ctx context.Context, r ShareRequest) (*Share, error)
// StopOther stops another share of this user (the SPA's role viewer may not, 01 §6.3): (1) if the Go connection is
// in roomID, it sends share.stop; (2) if it is in no room, it joins roomID (the same Participant as the SPA, so
// presence doesn't change), sends share.stop, then leaves; (3) if it is in another room, ErrBusy. It returns nil
// only once room.state no longer lists shareID: share.stop answers ok for unknown ids, and M1's server looks the
// share up only in the caller's current room (internal/server/signal/share.go, onShareStop with requireRoom).
func (c *Client) StopOther(ctx context.Context, roomID, shareID string) error
func (c *Client) ActiveShare() *Share // nil when none
// WatchServerStats turns stats.watch on or off (the app's debug overlay); the server's view of this connection's
// layers then arrives in ShareSnapshot.Server.
func (c *Client) WatchServerStats(on bool)
// Close is idempotent: it sends share.stop for a live share and stops the engine session (capture ends within one
// frame interval, 04 §6.3), waits at most 3 s for the share.stop reply, then closes with 1000 (the server ends
// anything left at once, 01 §4.2). It returns within 3 s plus the close handshake; 03's Quit waits at most 3 s.
func (c *Client) Close() error

type ShareRequest struct {
	RoomID string
	Source media.Source    // a picked source (03: Engine.Pick); Engine.Start consumes it
	Preset protocol.Preset
	Audio  bool            // share.start{audio}; false: Config.Audio = nil
	Label  string          // user-typed only; "" from M2's UI (plan: labels default to Screen or Window)
}

type Share struct{ /* … */ }

func (s *Share) Snapshot() ShareSnapshot
func (s *Share) SetPreset(ctx context.Context, p protocol.Preset) error
func (s *Share) SetPaused(ctx context.Context, paused bool) error // D14 ("Share sound" off is 05's Service.SetShareSound(false))
func (s *Share) Stop(ctx context.Context) error
func (s *Share) Done() <-chan struct{}

var (
	ErrBusy     = errors.New("core: a share is active or starting")
	ErrNotReady = errors.New("core: not connected")
	ErrClosed   = errors.New("core: closed")
)

type StopError struct {
	Reason StopReason
	Code   protocol.ErrorCode // the server's code when it said one
	Err    *protocol.Error    // the server's error as signal.StateChange.Err reported it (params included); nil for local stops
}

func (e *StopError) Error() string

type StopReason string

const (
	StopUnlinked       StopReason = "unlinked"        // session_revoked, a second unauthenticated right after the forced refresh, or an Auth error with RelinkRequired
	StopDisabled       StopReason = "account_disabled"
	StopUpdateRequired StopReason = "update_required" // protocol_unsupported, client_outdated, or an Auth error with UpdateRequired
	StopTooMany        StopReason = "too_many_connections"
	StopReplaced       StopReason = "replaced"        // another socket resumed this connection (a second app instance)
	StopProtocol       StopReason = "bad_message"
)
```

For `update_required` caused by a server error, 03 hands `StopError.Err` (with `params.serverMax` and
`serverVersion`) to 02's `compat.FromError` and `Manager.SetIncompatible` (02 §13.1) to choose between "Update the
app" and "Ask your admin".

### 11.3 Connection lifecycle and authentication

- `Run` dials `signal.Dial` with `URL = ServerURL + "/ws"` unchanged (signal accepts https and http; coder/websocket
  maps them to wss and ws; http only for 02's loopback dev origins, `AllowInsecureLoopback`),
  `Token = func(ctx) { Auth.AccessToken(ctx, false) }`, the hello values of §11.1, and `Header` for tests. Signaling
  states map to `Status.Conn`: `connecting`/`handshaking` → `connecting`; `ready` → `online`; `backoff` →
  `reconnecting`; `stopped` → see below.
- **Stops** (01 §10.1):

| Signal stop | Core |
|---|---|
| `unauthenticated` | `Auth.AccessToken(ctx, true)` once; on success `signal.Dial` again (a new connection: the share is republished, §11.7); if that hello is answered `unauthenticated` again, `Auth.SessionEnded(ctx, unauthenticated)`, then stop `unlinked`; on a `RelinkRequired` error stop `unlinked`; on an `UpdateRequired` error stop `update_required`; on a transient error retry with 01 §10.2's backoff (0.5–10 s), the share's 30 s hold running |
| `session_revoked` (device revoked, password reset) | `Auth.SessionEnded(ctx, session_revoked)`, then stop `unlinked` (no refresh by the core) |
| `account_disabled` | `Auth.SessionEnded(ctx, account_disabled)`, then stop `account_disabled` |
| an `Auth.AccessToken` error whose chain has `UpdateRequired()` true (at any dial) | stop `update_required` (`Err` nil) |
| `protocol_unsupported`, `client_outdated` | stop `update_required`; `StopError.Err` carries the server's params (03 shows "Update the app" or "Ask your admin", 02 §13.3) |
| `too_many_connections` | stop `too_many_connections` |
| `replaced` | stop `replaced` (03's single-instance lock should make it impossible) |
| `bad_message` | stop `bad_message` |

  `Auth.SessionEnded` is called synchronously, with its own ctx bounded at 15 s, before the stop is reported and
  `Run` returns. With `Options.Header` (no `Auth`) the same stops happen without the calls.
  A stop ends the share (`EndReason` local: `unlinked`, `server`, …), stops the engine session, and returns from
  `Run`. **The manager (02) owns the credential**: the core never stores or deletes tokens, and 03 reads the final
  link state from the manager (`State`, `OnState`), not from the `StopReason` alone; the app decides what to show and
  whether to relink (02, 03).
- `Events()` of the signal client is drained by one goroutine (the connection actor), which also owns the room
  state, the share's server-side view and the relay. Long work never runs on it: negotiation and engine calls run on
  the share's actor.

### 11.4 Share lifecycle

One share at a time (D12). Each share has an actor goroutine that owns its Publisher, the `ratectl.Controller`, the
engine session and the pub PC driver; the connection actor posts server events to it.

```
StartShare ─► joining ─► starting ──first room.state with status live──► live ◄────────► reconnecting
                │          │                                              │  ▲              │ 30 s without ready
                │          │ engine or server error                       │  │ paused flag  ▼
                └──────────┴──────────────────────────────────────────────┴──┴─────────► ended(reason)
```

Start, in order (the whole start is bounded by 30 s, the server's start timeout, 01 §4.4). `StartShare` returns
after step 3 (§11.2): with `ErrNotReady` at once when the connection isn't online, with the error of a failed step,
or with the share.
1. `room.join{roomId}` unless the connection is in that room (10 s request timeout). Errors end the share with the
   server's code (`room_not_found`, `room_full`, `forbidden`), returned as `*protocol.Error`.
2. `share.start{kind: Source.Kind, preset, audio, ref}` → `ShareParams` (`ref`: 8 random base32 characters; retried
   with the same `ref` after `connection_lost`, 01 §10.5). `share_limit` ends the share.
3. `Engine.Start(Config)` from the params: `Profile` = `params.Codec`'s profile (`6400` or `42e0`) when the first
   available probed encoder lists it, else that encoder's first profile (the SFU then answers with what the offer
   has, M1 02 §8.4); layers from `params.Encodings` (`MaxPixels`, `MaxFramerate`, `Inactive` = !`Active`,
   `ratectl.Initial` bitrates); `Audio` = `Options.AudioConfig()` with `BitrateBPS = params.AudioBitrate` when
   `r.Audio`, else nil; `Encoder` "" unless `Options.Encoder` (the support override) is set; `Borderless =
   Probe.Capture.Borderless`. On error: `share.stop`, `room.leave`, and `StartShare` returns the `*media.Error`
   (`no_encoder` refuses the share; no one-layer retry in M2, D15). There is no mid-share engine restart in M2, so
   `Options.AudioConfig` is read once per share; later rule changes reach the engine through 05's Service.
4. The media pump attaches (§11.5); a Publisher is created (`Layers` = the active rids of `params.Encodings`,
   `Profile` = the session's first `EventLayer.Profile` and `Profiles` per §8.6, `Audio`), `ratectl.New` gets `Live`
   from the first `EventLayer` of each layer, and the pub PC is negotiated (§11.6) with `tracks` naming the share.
5. The share is `live` when `room.state` shows it `live` (the SFU saw its first keyframe). `ratectl.Live` starts the
   ramp.

Stop (`Share.Stop`, or the end of `Run`): `share.stop{shareId}` → `pc.close{pub, gen}` → Publisher closed →
engine `Stop` → `room.leave` when the connection has no other share. When signaling is not ready the local part runs
at once, and the server side is reconciled on the next `welcome` (§11.7).

The server ends the share (it vanishes from `room.state` or a `room.event share.stopped` names it, while the socket is
ready; an `error` in scope `share` or `room`): the share ends with `server` and the server's `EndReason`, the engine
stops, nothing is republished (01 §10.5).

### 11.5 The media pump (D5)

```
Session.Next ──► pump goroutine ──► per-kind queues ──► consumer (the current Publisher's Source)
                     │ events ──► share actor (ratectl, snapshots, app events)
                     └ fatal error ──► share actor (end)
```

- One goroutine per engine session calls `Next` in a loop; it lives exactly as long as the session.
- Queues: video 6 frames per layer (100 ms at 60 fps), audio 10 packets (200 ms). `KindPCM` packets are dropped (the
  core never asks for them).
- **No consumer attached** (no Publisher yet, a rebuild in progress, the 30 s hold): media is dropped; when a consumer
  attaches, every active layer gets a keyframe request and its frames are dropped until that keyframe arrives.
- **Consumer behind** (a video queue full): the layer's oldest frame is dropped, then its frames until the next
  keyframe, which is requested (throttled 300 ms per layer). Audio drops its oldest packet.
- **Paused** (D14): video and audio are dropped at the pump from the moment of the pause, keyframe requests from the
  Publisher are swallowed; the engine is paused (`Session.SetPaused`) and muted (`AudioController.SetAudioMuted`).
  On resume the engine is unmuted first, then un-paused; audio is forwarded at once (it fades in from the engine's
  silence), every active layer gets a keyframe request, and video frames are dropped until it arrives.
- The consumer endpoint implements `publish.Source`, `publish.Clock` (`Session.Now`) and `publish.ProfileSource`
  (`SetProfile` → `Session.Reconfigure{Profile}`).
- Events go to the share actor:
  - `EventLayer`: the layer's snapshot (size, FPS, encoder, HW, profile), `ShareSnapshot.NoOSIndicator`,
    `ratectl.SetTargetHeight` (for `f`) and `ratectl.SetLive`; the first one per layer sets the Publisher's profile
    (§11.4 step 4); one after a `SetProfile` re-init that reports another profile than requested makes the core
    re-offer with the actual profile (§8.6).
  - `EventLimit`: `ratectl.SetLimit` and the layer's `Limit`.
  - `EventEncoderChanged`: the layer's encoder in the snapshot and `EventNotice{"encoder_fallback", {encoder}}`. The
    share id, the Publisher and the pub PC stay; 04 re-inits the layer with a `Reinit` keyframe, and decoders handle
    the new SPS at the IDR.
  - `EventSourceMinimized` / `EventSourceRestored`: `ShareSnapshot.Minimized` (03: "Your shared window is
    minimized…", 04 §5.7).
  - `EventSourceResized`, `EventCaptureInterrupted` / `EventCaptureResumed`, `EventDeviceReset`: logged (new sizes
    reach the snapshot with the `EventLayer` of a re-init).
  - Reserved codes (2–5) and unknown codes: dropped (debug log). Audio events never come through `Next` (05).
- A fatal error from `Next` ends the share with its code, `encoder_lost` included (the engine already tried every
  candidate, D15). It is never audio-caused (§5.2).

### 11.6 Negotiation and PeerConnection recovery

The pub PC driver (`pubdriver.go`) is the production version of the itest's share driver, implementing 01 §9 and the
pub column of 01 §10.4:

| Condition | Driver |
|---|---|
| First offer, or a new share on the PC | `Offer` with `tracks`, `gen` 1 (or current), `neg + 1`; one outstanding offer, changes folded into one follow-up (01 §9 rule 3) |
| ICE `disconnected` for 3 s | `signal.Probe()` at once; then `OfferICERestart` (same `gen`, `neg + 1`) |
| ICE restart not `connected` within 15 s, PC `failed` or closed under it, or `pc.restart{pub, rebuild}` | rebuild: close the Publisher, new Publisher, `gen + 1`, same share id; the pump re-attaches (keyframes) |
| `sdp_invalid` or `bad_request` in scope `pc` naming the outstanding offer | one rebuild; a second within 60 s ends the share (`error`, `sdp_invalid`) (01 §9 rule 8) |
| `stale_negotiation` | ignored |
| `rate_limited` in scope `pc` | the offer goes out again after `retryAfterMs` (1 s when absent) |
| 5 rebuilds without reaching `connected` | snapshot state `unreachable` (03: "Can't reach the server's media port" with the connection test); rebuilds go on every 30 s |
| Signaling not ready | state changes are recorded; acting waits for `ready` |

Limits: at most one ICE restart per 5 s and one rebuild per 10 s (01 §10.4). Remote candidates: the server sends all
of its own in the answer (01 §9 rule 5); local ones are trickled with `pc.ice` (at most 64 kept while not ready).

`DefaultSettings`: network types `udp4`, `udp6`, `tcp4`, `tcp6` (ICE-TCP active candidates reach the server's TCP 443
and 7882 passive ones, M1 02 §7.4; M1's `TestICETCPVia443` proves it for Go clients); `SetICEMulticastDNSMode(disabled)`;
`SetIncludeLoopbackCandidate(false)`; `SetICETimeouts(3 s, 25 s, 1 s)` (the publisher receives RTCP every second,
so 3 s without inbound traffic is a real outage); `LoggerFactory = logx.NewPionLoggerFactory(log)`.

### 11.7 Reconnect, resync and the 30 s hold

- **Hold**: when signaling leaves `ready`, a 30 s timer starts (plan: "the desktop keeps its encoder running for
  30 s"). The engine keeps capturing and the pump keeps draining. If the pub PC stays connected meanwhile, media keeps
  flowing (a WebSocket drop is not a media drop, 01 §4.2). On `ready` within 30 s the timer stops; after 30 s the
  share ends with `server-unreachable` ("Sharing stopped: the server was unreachable", as the web's 60 s, 01 §10.6).
- **Resumed `welcome`** (01 §10.5): re-send a pending pub offer (same `neg`), ICE-restart a pub PC that isn't
  `connected`, retry requests that failed with `connection_lost` (`share.start` with the same `ref`, `share.stop`),
  then reconcile with the first `room.state`: a local share missing on the server is republished (below); a server
  share with this `connectionId` that the core doesn't have is stopped with `share.stop`.
- **Not resumed** (server restart, grace over, or the new connection after a forced token refresh): drop the pub PC
  without a message, `gen` back to 1; `room.join{the share's room}`; `share.start{replaces: oldShareId, ref: new}`; a
  new Publisher at `gen` 1; the engine session continues and the pump asks for keyframes. The share keeps its local
  identity; its `ID` changes and the app gets a snapshot (viewers move focus by `replaces`, 01 §10.6). The
  connection's id changes too (`Status.ConnectionID`, `EventStatus`).
- **Reconcile with `room.state`** (D13), for the share's own `ShareInfo` in every snapshot:
  - `preset` differs from the applied one (changed from a web tab): `share.update{shareId, preset}` (idempotent) →
    the reply's `ShareParams` go to `ratectl.Params` and `Session.Reconfigure` (preset tuning, frame rate; Text runs
    `f` at 30 fps, M1 02 §8.6); a changed `AudioBitrate` becomes `OpAudioBitrate` → `Session.SetBitrate(LayerAudio)`;
  - `status` is `paused` while the local share isn't: pause locally (D14: pump drops, `Session.SetPaused`,
    `AudioController.SetAudioMuted`) without another `share.update`; `live` while the local share is paused by a
    remote change: resume locally the same way;
  - `watchers` → the snapshot (tray "Sharing <source> · N watching", plan "Share safety").

### 11.8 Hints and pause

- `quality.hint` for the share's current id: `codec` → §8.6; `encodings` and `maxBitrate` → `ratectl.Hint`; the
  resulting actions are applied in order: `OpActive` → `Publisher.SetActive` and `Session.SetLayerActive(layer,
  active)`, plus `Session.SetBitrate(layer, target)` in the same step when resuming; `OpBitrate` →
  `Session.SetBitrate`; `OpAudioBitrate` → `Session.SetBitrate(LayerAudio)`; then `Publisher.SetPacingTargets`.
  Hints for other share ids are ignored. Hints are re-sent after a resume (01 §8.10); applying the same values again
  changes nothing.
- `ratectl.Tick` runs every 500 ms on the share actor while the share is live.
- `Share.SetPaused(true)`: the pump drops video and audio from this moment (it covers the up to one frame interval
  before the engine applies), then `Session.SetPaused(true)` and `eng.(media.AudioController).SetAudioMuted(ctx,
  true)`, then `share.update{paused: true}` (when the server lacks the feature, only the local part happens: viewers
  see a frozen frame without the "Paused" label). Keyframe requests stay swallowed. `SetPaused(false)`:
  `SetAudioMuted(ctx, false)` (10 ms fade-in), `Session.SetPaused(false)`, a keyframe request per active layer, the
  pump forwards (video from those keyframes), then `share.update{paused: false}`. The engine keeps capturing, so a
  resume is instant. The same transitions run for a pause reconciled from `room.state` (§11.7) and for 03's global
  hotkey. An engine without `AudioController` gets only the pump drop for audio.

### 11.9 Snapshots and stats

```go
type ShareState string // "joining" | "starting" | "live" | "reconnecting" | "unreachable" | "ended"

type ShareSnapshot struct {
	ID            string // the current shareId (changes on republish)
	RoomID        string
	Kind          protocol.ShareKind
	Preset        protocol.Preset
	State         ShareState
	Paused        bool
	Audio         bool             // the share has audio
	AudioSentBps  int              // live counter
	NoOSIndicator bool             // no OS border (borderless WGC or DD, from EventLayer): 03 must show the Sharing pill
	Minimized     bool             // the shared window is minimized (EventSourceMinimized)
	Watchers      []protocol.Watcher
	Encoder       string           // f's encoder ("nvenc", …)
	HW            bool             // f's
	Layers        []LayerSnapshot
	Upload        ratectl.UploadHint
	Overloaded    bool             // ratectl.CPU
	Server        []protocol.ServerLayerStats // with WatchServerStats(true)
	End           *EndInfo
}

type LayerSnapshot struct {
	RID            string
	Width, Height  int
	FPS            float64              // live counter (Publisher)
	Active         bool
	Encoder        string               // this layer's encoder (layers may differ, 04 §9.7)
	HW             bool
	Profile        string
	Limit          string               // "none" | "cpu" | "gpu" | "source"
	TargetBps      int
	SentBps        int                  // live counter
	CaptureToSend  publish.LatencyStats // live counter
	EngineLatency  publish.LatencyStats // live counter: capture → engine ring (Session.Stats LatencyP50/P95; Max 0)
	FPSIn          float64              // live counter: captured frames per second (Session.Stats)
}

type EndInfo struct {
	Reason    string               // "user" | "source-gone" | "server" | "server-unreachable" | "unlinked" | "error"
	Media     media.ErrorCode      // engine failures
	Protocol  protocol.ErrorCode   // server error replies
	Server    protocol.EndReason   // server-ended shares
	ByThisApp bool                 // true when this core sent the share.stop that ended it
}
```

| `EndInfo.Reason` | Set fields |
|---|---|
| `user` | `ByThisApp` true (`Share.Stop`, `Close`, the Stop hotkey); nothing else |
| `source-gone` | `Media` = `source_gone` |
| `error` | `Media` (an engine failure: `encoder_lost`, `permission_denied`, `internal`, …) **or** `Protocol` (a server error reply that ended it: `share_limit` at republish, a second `sdp_invalid`, …) |
| `server` | `Server` = the server's `EndReason`; `ByThisApp` false (for example a `share.stop` from a web tab of the user, which 03 shows as "stopped elsewhere") |
| `server-unreachable` | nothing |
| `unlinked` | `Protocol` = the stop's code (`session_revoked`, `account_disabled`, `unauthenticated`) when there is one |

- **Engine stats**: while a share is live the share actor calls `Session.Stats()` every 1 s and copies each layer's
  `Encoder`, `HW`, `Limit`, `FPSIn` and `LatencyP50`/`P95` (as `EngineLatency`) into its `LayerSnapshot`, next to the
  core's own `CaptureToSend` (04 reports the engine's latency next to the exit measurement).
- **`stats` to the server** every 10 s while the pub PC exists (01 §8.11): `pcs[{pc: pub, gen, state, rttMs,
  candidateType, transport}]` from the Publisher (RTT from receiver reports, the selected pair's local candidate
  type and protocol from `GetStats`), and `outbound` per layer and for audio: `bitrate`, `fps`, `width`, `height`,
  `encoder`, `hwEncoder`, `qualityLimitation` (`bandwidth` while the upload hint's condition holds; `cpu` while the
  layer's limit is `cpu`; `other` while it is `gpu`, since the protocol enum is `none|cpu|bandwidth|other`; else
  `none`). No source names, no app names.

### 11.10 Events for the app

```go
type EventKind uint8

const (
	EventStatus        EventKind = iota + 1 // Status changed (connection state, connection id, stop)
	EventShare                              // a ShareSnapshot field other than a live counter changed (coalesced to ≤ 4 per second)
	EventNotice                             // a toast: Code "encoder_fallback" {encoder}, …
	EventPlaybackState                      // a web tab reported isshoni playback (relay playback.state, §11.11)
	EventResync                             // the subscriber missed events: read Status()
)

type Event struct {
	Kind     EventKind
	Status   *Status
	Share    *ShareSnapshot
	Notice   *Notice
	Playback *PlaybackState
}

type Status struct {
	Conn          string     // "connecting" | "online" | "reconnecting" | "stopped"
	ConnectionID  string     // the Go connection's id: "" until the first welcome; changes after a reconnect that isn't resumed
	Stop          *StopError // when stopped
	ServerVersion string
	Features      []protocol.Feature
	Share         *ShareSnapshot
}

type PlaybackState struct {
	From    string             // the sender's connectionId (agent.recv.from)
	Playing bool
	Browser string             // "chrome" | "edge" | "firefox" | "other" (05 §5.4 families)
	OS      protocol.ClientOS
}

type Notice struct {
	Code   string
	Params map[string]string
}
```

- `EventStatus` fires on every change of `Status`, `ConnectionID` included (03 uses it for
  `App.Info().publisherConnectionId`, `ShareSnapshot.connectionId` and `SharingProvider.localConnectionIds()`).
- `EventShare` fires when any `ShareSnapshot` field changes **except the live counters** (`Layers[].SentBps`,
  `Layers[].FPS`, `Layers[].CaptureToSend`, `Layers[].EngineLatency`, `Layers[].FPSIn`, `Server`, `AudioSentBps`),
  coalesced to ≤ 4 per second; callers read counters with `Share.Snapshot()` (03 polls every 2 s for its stats and
  caps `share.state` at ≤ 4 per second, 03 §6.3, §20).
- 03 maps events to Wails events; the core never formats text. `EventPlaybackState` goes to 05's
  `Service.SetViewerBrowser(From, Browser, OS, Playing)`. Audio state ("What friends hear", levels, prompts, banners)
  never passes through the core: 05's Service reads it from the engine (05 §14).

### 11.11 The same-user relay (`internal/protocol/relay`)

Payload types for `agent.send`/`agent.recv` (01 §8.14), stdlib + `internal/protocol`, generated to
`web/src/protocol/relay.gen.ts` by tygo (a new `tygo.yaml` entry, integrator-owned). `relay.gen.ts` is the one TS
source that 03's `playback.stop` sender and `rooms/handoff.ts` import.

```go
package relay

// Kinds ([a-z.]{1,32}, 01 §8.14).
const (
	KindPlaybackStop  = "playback.stop"  // M2: desktop SPA (C1) → the user's full connections (toRole "full"), 03 §12.2
	KindPlaybackState = "playback.state" // M2: web tab → desktop app (to: the app's publisher connectionId from user.connections), 05 §5.9
	KindShareRequest  = "share.request"  // later (M4): web-initiated desktop or agent shares; no payload declared in M2
	KindShareStatus   = "share.status"   // later (M4)
	KindExclusionsGet = "exclusions.get" // reserved (M4): payload declared by M4's agent spec (D16)
	KindExclusionsSet = "exclusions.set" // reserved (M4)
)

// PlaybackStop: "Now watching in the isshoni app" (03 §12.2). V = 1. Sender: the desktop SPA (C1), with
// toRole "full", once each time the user brings the app into a room. Receiver rules (rooms/handoff.ts): v === 1,
// roomId equals the session's room, from is a desktop connection of the user in room.state; anything else ignored.
type PlaybackStop struct {
	V      int    `json:"v"`
	RoomID string `json:"roomId"`
}

// PlaybackState: a web tab reports whether it plays isshoni audio, so 05 can keep that browser out (05 §5.9).
type PlaybackState struct {
	Playing bool              `json:"playing"`
	Browser string            `json:"browser"` // "chrome" | "edge" | "firefox" | "other" (05 §5.4 families)
	OS      protocol.ClientOS `json:"os"`
}
```

```json
{"type": "agent.send", "id": "41", "data": {"toRole": "full", "kind": "playback.stop",
  "payload": {"v": 1, "roomId": "lounge"}}}
{"type": "agent.send", "id": "52", "data": {"to": "c_k3v9q2m7xw4pa8d1", "kind": "playback.state",
  "payload": {"playing": true, "browser": "chrome", "os": "windows"}}}
{"type": "agent.recv", "data": {"from": "c_4hq8m2v7kx9p3w1d", "kind": "playback.state",
  "payload": {"playing": true, "browser": "chrome", "os": "windows"}}}
```

Core rules:
1. `playback.state`: decode strictly (unknown fields ignored, size already bounded at 16 KiB by the server); a
   payload that doesn't decode, or a `browser` outside the four families, is ignored (debug log with the kind only).
   Otherwise emit `EventPlaybackState{From, Playing, Browser, OS}`; 03 forwards it to 05's
   `Service.SetViewerBrowser(From, Browser, OS, Playing)` (05 §14.2). The server delivers only messages of the same
   user (01 §8.14).
2. `playback.stop` is never sent or handled by the core (03 owns sender and receiver); the type lives here only for
   `relay.gen.ts`.
3. Every other kind (`share.request`, `share.status`, `exclusions.*`, unknown) is ignored (debug log with the kind
   only); the Windows app answers nothing over the relay in M2 (D16).

### 11.12 Logging and privacy

- `log/slog` only, attributes per README §4 plus `share_id`, `gen`, `encoder`. The app chooses the handler (03).
- **Never logged**: tokens, SDP, ICE candidates and passwords, window titles, monitor names, process names and paths,
  exclusion lists, audio app lists, relay payloads, usernames. Audio apps appear as counts.
- Never sent to the server: the same list (plan "What never leaves the device"); `stats` carries encoder names and
  numbers only.

## 12. Glass-to-glass latency (exit criterion)

### 12.1 Budget at 1080p60

A real share has an audio track. A share's audio and video DownTracks have the same msid stream (the ShareID, M1 02
§9.6), so Chrome lip-syncs them: it plays a frame no earlier than the audio captured at the same time. The audio path
is slower than the video path, so with sound on Chrome holds video back by the difference. The budget therefore has
both paths, and the gate is measured with sound on (§12.2). RTT is 25–50 ms in both tables.

**Video path**

| Stage | Expected (NVENC/QSV/AMF) |
|---|---|
| Capture (WGC/DD frame after composition) | 8–17 ms |
| GPU convert and downscale (D3D11 VideoProcessor, 04) | 1–2 ms |
| Encode | 3–8 ms |
| Pump, packetize, pace (P-frames; IDRs add up to ~60 ms, about 1 frame in 240 at a 4 s GOP) | < 2 ms |
| Sharer → server → viewer | ≈ 1 RTT when both are on one PC (≤ 50 ms) |
| SFU forward and DownTrack pacing (M1 02 §9.8) | 1–3 ms |
| Chrome jitter buffer + decode | 15–45 ms |
| Render and composition | 17–33 ms |
| **Video total** | **≈ 70–160 ms** |

**Audio path** (from the Windows audio engine's mix on the sharer, which is what 05 §10.3 stamps, as video stamps
DWM's composition, to the audible time on the viewer)

| Stage | Expected |
|---|---|
| Mix → mixer: the mixer's buffer fill (40 ms running target, 05 §9.3) plus one 10 ms loopback packet (05 §10.3's L, ≈ 50 ms) | 40–60 ms |
| Opus: the wait for the frame's second 10 ms chunk, plus 6.5 ms lookahead (05 §10.1) | ≈ 17 ms |
| (So an audio packet's stamp is ≈ 55–80 ms old when the packet is emitted, §6.4 check 10, against ≤ 25 ms p95 for video, 04 §15) | |
| Send (audio is never paced, D8) and SFU forward | ≈ 2 ms |
| Sharer → server → viewer | ≈ 1 RTT (≤ 50 ms), as for video |
| NetEq (Chrome's audio jitter buffer, 20 ms packets) | 20–60 ms |
| Audio output on the viewer (Chrome's renderer, WASAPI shared mode) | 10–30 ms |
| **Audio total** | **≈ 115–220 ms** |

**With sound**, glass-to-glass ≈ max(video total, audio total). The **lip-sync hold** on video is ≈ 45–60 ms, and the
range is ≈ 115–220 ms. The upper end is over 200 ms, which is why a sound-off number can't gate. If the gating run
misses (§12.2), the levers, in this order:
1. **05 §10.3's stamps must be right**: chunk time = the time Windows mixed the audio, without the sharer's
   output-device latency. An earlier draft added the stream latency with the wrong sign, which made audio look about
   2 × that latency older than it is. Chrome would then hold video by that much more, on top of the real
   difference. The rc's audiotest gate checks this (05 case 20 `timestamps`), so §12.2's run follows the audio run
   (exit run book A).
2. **Lower 05 §9.3's running fill target** from 40 ms to 20–25 ms, now that the PI resampler holds it. This saves
   15–20 ms on the audio path, so on the lip-sync hold. Do it only if the sound-on run misses, and only with 05 A4's
   drift simulation still passing (the underrun and trim margins shrink).
3. NetEq and the viewer's output are Chrome's and are not tuned in M2.

**Content cuts**: a full-screen content change (a window or tab switch, a page jump, a game's scene cut) makes one
large P frame. Layer 0 is CBR with a 0.5 s buffer (04 §9.3), so at 8 Mbit/s that frame can approach 500 KB. It drains
through the sharer's pacer (24 Mbit/s, §8.5) and the SFU's DownTrack pacer (20 Mbit/s, M1 02 §9.8). The two are
pipelined, so the cost is close to the slower one's, about 100–200 ms (M1 02 §9.8: "a 300 KB frame about 100 ms"),
rather than their sum. Chrome's jitter buffer then grows for seconds after it. The budget rows above assume steady
content. The gate therefore includes cuts (§12.2), and 04's E7 compares layer-0 buffers of 0.5 s and 0.1–0.2 s.

The component the client core owns, capture → last packet sent, is measured on every share
(`LayerSnapshot.CaptureToSend`, next to the engine's own `EngineLatency`) and asserted on the owner's PC (§12.3).

### 12.2 The measurement on the owner's PC (`tools/g2g`, D17)

**The tool** (`tools/g2g`, Go, Windows-only capture, in the `tools/` module like `tools/exittest`):
- `g2g serve` serves `clock.html` on `127.0.0.1:<port>`. The page draws, at every `requestAnimationFrame`, the value
  `v = floor(performance.now()) mod 65536` as a 16-bit **Gray code** in a strip of 18 cells of 48 × 48 px (a
  black-white guard at each end, 16 data cells, black = 1) on a mid-gray frame at the top-left, a human-readable
  millisecond counter, and moving content over the rest of the window (scrolling text and a moving gradient) so the
  encoder works as for a real share. Two more things make it a normal share, both on by default:
  - **Content cuts**: behind the moving content, the background swaps every 5 s between two very different
    full-window images, both generated by the page from a fixed seed (no image files). One is a dense page of small
    black text on white, the other coarse colored noise. Each swap is a full-screen change, like a window or tab
    switch. Neither image uses the strip's mid-gray or 48 px black-white cells, so calibration can't lock onto them.
    `?cuts=0` turns the swaps off (the diagnostic run).
  - **A continuous quiet tone**: a 440 Hz sine at −30 dBFS through Web Audio, so the share carries real audio. It
    starts with a click on the page's **Start** button (browsers allow audio only after a user gesture), and the page
    shows "tone on".
  The counter line also shows `cuts on|off` and `tone on|off`. `?freeze=<ms>` draws one frame as at that time and
  stops (the decoder's fixtures are recorded so), and the page has a read-only debug hook, `window.g2gClock`.
- `g2g measure [-d 60s] [-run gate|nosound|nocuts] [-o result.json] [-server https://…] [-dump DIR]` grabs the
  **primary monitor** (`(0,0)` at `SM_CXSCREEN` × `SM_CYSCREEN`) 20 times a second with GDI (`GetDC(0)`,
  `CreateDIBSection`, `BitBlt` with `SRCCOPY|CAPTUREBLT`, through `x/sys/windows` lazy procs, per-monitor DPI aware
  so the pixels are physical; no cgo). With the run book's one monitor, or the primary one of a larger setup (step
  2), that is the shared screen; it avoids slow multi-monitor BitBlts, and the clock's known position is relative to
  that monitor. A 2 s calibration finds the clock's own strip (scale 1, normally at the top-left of the full-screen
  page) and the **largest other copy** of the strip at a scale of 0.25–1.0 that is in at least 30 % of the
  calibration screenshots. Strips are found by scanning rows for the guard pattern (grey, black half, white, then
  white, black half, grey): the guards' black halves give each copy's scale, and the guards' ends place every cell to
  a fraction of a pixel, so every scale in the range decodes, not only steps of 0.05 (whose far cells would be off by
  almost a cell at, say, 0.43). Then it reads both strips in each screenshot, follows the copy when the viewer moves
  (`copyMoves`: a move counts only when an edge shifts by more than half a cell), and records `Δ = v_clock − v_copy`
  (mod 65536) per screenshot.
- **A strip caught mid-transition** has cells between two frames. With millisecond values a 17 ms step changes
  several Gray bits, so reading those cells either way could be off by up to 31 ms; the decoder takes the closest
  complementary resolution of the ambiguous cells and its midpoint, which is within **half a frame** (checked
  exhaustively). Such samples count in `transitions`.
- **Plausibility filter**: a strip that decodes can still be wrong (a cell flipped by the codec, and one wrong high
  bit moves the value by seconds), so these screenshots count as decode failures of cause `implausible`, not as
  samples: the clock's value disagrees with the capture time (anchored during calibration) by more than 150 ms; Δ
  is over 30 s; or Δ is more than 3 s from the median of the last 21 accepted samples (once there are 5). After 20
  implausible readings in a row that agree with each other, the tool takes them as the new normal (a reloaded page,
  a latency step) and follows them, with a warning.
- With `-server` it also measures the TCP connect time to the server (443 for https) five times after one DNS lookup,
  records the median as the RTT (a warning above 50 ms), and reads the server's version from `/api/v1/info`.
  `-dump DIR` saves PNG crops of the two strips at calibration and for the first screenshots that fail to decode, for
  diagnosis (look at them before sharing them).
- `-run gate|nosound|nocuts` (default `gate`) names the run in the JSON. The operator states what the run is; the
  tool doesn't check it. `g2g version` prints the tool's, Go's and Windows' versions.
- Output (to `-o`, or stdout): the run name, `startedAt`, the samples, `decodedPct`, p50, p95, max, the decode
  failures by cause (`capture`, `clock`, `copy`, `implausible`), `transitions`, `copyMoves`, the monitor's refresh
  rate, the screen size, the copy's scale and position, the RTT and its five samples, the versions (g2g, Go, Windows,
  the server), the `criterion`, the `result`, the `exitCode` and every screenshot's Δ (`series`); no host, user,
  machine or path. Exit 0 when p95 < 200 ms with ≥ 90 % of screenshots decoded, 1 when the latency fails, 2 when too
  few decoded or the run was interrupted (inconclusive, S1's convention); there are only these three codes, so a usage
  error or a measurement that couldn't run (not on Windows, no clock or copy on the screen) is 2 too. Every run
  computes this, and only the `gate` run decides the criterion. `-o` is written through a temporary file next to it,
  created before the calibration (an unwritable path fails at once), and replaces `-o` only when a whole result has
  been written; an earlier file is never truncated by a failed run.
- The decoder tests' recorded fixtures are headless-Chrome renders of the real clock page (`?freeze=`): Chrome
  scales the viewer copy, and a JPEG round trip stands in for codec artefacts. They are not Windows GDI screenshots,
  which only the owner's-PC run gives; re-recording them is reproducible byte for byte (`G2G_CHROME=<chrome> go -C
  tools test ./g2g -run TestRecordFixtures -args -record`).

**The run** (manual, owner's PC, Win11 and Win10 22H2, with the release candidate):
1. An isshoni server with RTT ≤ 50 ms from the PC (the M1 exit-test VPS, or a LAN server); note the RTT.
2. One 1080p monitor (or the primary monitor of a larger setup, whose share the full layer scales to ≤ 1080p).
3. `g2g serve`, open the clock page in Edge on the shared monitor, click **Start** (the tone plays), press F11
   (full screen).
4. In the isshoni app: Share → the OS picker → that monitor (Win11: borderless WGC; Win10: Desktop Duplication),
   preset Auto, **Share sound on** (the default, Exclude apps). Wait until the app shows the full layer at 8 Mbit/s
   (≈ 10 s, the ramp).
5. In Chrome (not the isshoni app: its windows are excluded from capture, plan "Own windows"), signed in as **the same
   user**, open the room. Click the share to focus it, then its speaker button: an own share is never made audible
   by focus (M1 05 §12.2), and its audio must be subscribed for Chrome to lip-sync. If Chrome shows "Now watching in
   the isshoni app", click **Watch here instead** (03 §12.2). Place the Chrome window over the bottom-right half of
   the monitor at 50 % size, leaving the strip visible. The viewer shows the monitor, which contains the viewer: the
   nested copies are expected, and the tool takes the largest. If the copy can't be found (the video sits on a
   hardware overlay that GDI doesn't see), start Chrome with `--disable-direct-composition-video-overlays`.
6. Check the app's What friends hear: **Edge heard, Chrome kept out** as your viewer (05 §5.9, from Chrome's
   `playback.state`). If Chrome is heard, stop: the share would feed its own sound back.
7. **Gate run**: `g2g measure -d 60s -run gate -server https://<server> -o g2g-win11-gate.json` (sound on, cuts on).
8. **Diagnostic runs** (not gating; they tell a miss's cause):
   - `nocuts`: reload the clock page with `?cuts=0`, click Start, F11, and run `g2g measure -d 60s -run nocuts …`.
     This is the p95 without cuts.
   - `nosound`: stop the share and start a new one with **Share sound unchecked** in the share sheet (no audio track,
     `ShareInfo.audio` false). Turning sound off mid-share is not the same: 05's mixer keeps sending silent Opus frames
     (03 §10.3), and Chrome keeps syncing to them. Click the new share in Chrome to focus it (own shares aren't
     focused automatically), use the default page (cuts on), wait for 8 Mbit/s, and run `g2g measure -d 60s -run
     nosound …`.
9. Pass: **p95 < 200 ms in the `gate` run** (D17). Repeat on Win10 22H2. Record the three JSON files per OS, the
   encoder (the app's debug overlay), the RTT and the versions in the M2 exit-test record (06). On a miss, the
   diagnostic runs show whether the lip-sync hold (`gate` − `nosound`) or the cuts (`gate` − `nocuts`) cost it, and
   §12.1's levers apply.
10. Run this after the audio run (exit run book A) on the same rc. Its gating case 20 `timestamps` checks the audio
    stamps that the lip-sync hold depends on (§12.1 lever 1).

### 12.3 Component checks on the owner's PC (automatable on the self-hosted runner)

- `TestCaptureToSend` (`internal/client/core`, in-package, build tag `isshoni_hw`, windows): the real engine shares a
  monitor from `Engine.Sources()` (or 04's `test:pattern`) at 1080p60 through the core (the test sets the core's
  unexported `allowListed`, so `Engine.Start` gets `Config.Listed`) to an in-process server with an
  `sfutest.Viewer`, while a helper window animates; asserts `CaptureToSend` p95 ≤ 35 ms and p50 ≤ 20 ms over 30 s,
  per encoder the probe lists as `Tested` (forced with `Options.Encoder`).
- `mediatest.Conformance` with the real DLL (§6.4).

### 12.4 CI proxies (every PR)

1. **Go pipeline latency** (`internal/server/itest/desktop_latency_test.go`, ubuntu, macos, windows): `fake.Engine`
   (Synthetic, 1080p60 at 8 Mbit/s, the default 4 s GOP) → core → in-process server → `sfutest.Viewer`. For each `f`
   frame, `latency = arrival of its last packet − (Origin + Marker.CaptureNS)`. Asserts p95 < 60 ms and max < 200 ms
   over 20 s (loopback; under `-race` except on windows). This catches a pump, pacer or SFU change that holds frames
   back.
2. **Viewer path** (`web/e2e/latency.spec.ts`, Playwright on Chrome, the e2e job): `isshoni-loadtest -scenario smoke
   -decodable -publishers 1 -subscribers 0` publishes (as `decodable.spec` does, `LOADTEST_BIN`); the Chrome viewer
   records `requestVideoFrameCallback` metadata for 20 s. When `captureTime` is present (Chrome estimates it from our
   SRs; sender and viewer share the machine's clock), asserts median `expectedDisplayTime − captureTime` < 150 ms and
   p95 < 250 ms; otherwise asserts median `expectedDisplayTime − receiveTime` < 100 ms and annotates the test that the
   capture leg was not measured. This catches timestamp or SR mistakes that make Chrome's jitter buffer grow.

## 13. Limits and timeouts

| Item | Value | Where |
|---|---|---|
| Engine `im_next_packet` timeout per call | 50 ms (04 §12.3) | §7.1 |
| `Next` honors ctx | ≤ 50 ms | §5.1, §7.1 |
| `Stop` unblocks `Next` | ≤ 200 ms | §5.3 |
| First keyframe after `Start` | ≤ 500 ms | §5.3 |
| Forced IDR spacing in the engine | ≤ 1 per layer per 250 ms (04 §6.4) | §5.3 |
| Picks | single use; expire after 5 min; ≤ 4 unused kept; picker 120 s default, ≤ 600 s (04) | §5.1 |
| Sessions per engine | native ≤ 4, ≤ 1 with audio; fake 1 | §5.3 |
| Pump queues | video 6 frames per layer, audio 10 packets | §11.5 |
| Keyframe requests to the engine | ≤ 1 per layer per 300 ms | §8.3, §11.5 |
| Pacer | max(3 × targets, 6 Mbit/s), depth 40 ms, backpressure at 300 ms queued | §8.5 |
| NACK responder | 4096 packets per SSRC | §8.4 |
| Packet checks | per 04 §6.2's rings; PCM exactly 3840 B | §7.1 |
| Share start | ≤ 30 s overall (server start timeout); `StartShare` returns after `Engine.Start` (03 gives it 10 s) | §11.4 |
| `Auth.SessionEnded` call | ≤ 15 s, before `Run` returns | §11.3 |
| `Close` | ≤ 3 s for the `share.stop` reply, then close 1000 | §11.2 |
| Hold without signaling | 30 s | §11.7 |
| ICE restart / rebuild / spacing | 3 s / 15 s / 5 s and 10 s | §11.6 |
| Unreachable after | 5 rebuilds without connect; then every 30 s | §11.6 |
| Rate control | §9.6 (outputs clamped to 04 §6.4's ranges) | §9 |
| Native estimator | §10.1 constants (loss 10 / 3 / 1 %; queueing delay: back off at ≥ 40 ms on 2 ticks, grow below 15 ms, 500 ms window, baseline rise ≤ 0.5 ms per s); hints ≥ 15 %, ≥ 2 s apart | §10.1 |
| Test queue link | `NewQueueLink(capacity, limit)`: FIFO at the capacity, RTP lost beyond `limit` | §10.1 |
| `stats` to the server | every 10 s | §11.9 |
| Engine `Stats` poll | 1 s while live | §11.9 |
| Snapshot events | ≤ 4 per second; live counters don't fire them | §11.10 |
| Subscriber channel | 64 events | §11.2 |
| `user.connections` coalescing | ≤ 1 per 200 ms per user; `deviceName` may lag a rename by ≤ 5 min | §10.4 |

## 14. Test plan

Everything below runs with `go test -race` unless marked. **Windows rows**: CI windows-2025 runs the Go tests in 06's
`windows-go` (`-race` with MinGW, then `CGO_ENABLED=0`, the shipped mode) and, with the DLL, in `windows-desktop`; the
owner's PC runs `CGO_ENABLED=0` without `-race` (race needs cgo, and the plan says no cgo on Windows). "Mac" =
the developer's machine (`task test`); "CI" jobs per 06; "owner's PC" = the Windows PC, by hand or as the
self-hosted runner (06 names its labels).

**Unit**

| Package | What is asserted | Where |
|---|---|---|
| `internal/media` | `Error.Is` by code (also with a Reason), `errors.Is(wrapped, media.ErrSourceGone)`, `Is(media.ErrAudioNotRunning)` for `CodeAudioNotRunning`; `Error()` formats; `AudioCapsFromFlags` for all 16 values of the four bits (`Backend` "process-loopback" whenever 0x1 is set, "endpoint" with only 0x2 of the loopback bits, else "none"; unknown higher bits ignored) | Mac, CI ubuntu |
| `internal/media/fake` | `fake.Engine` passes `mediatest.Conformance` (Synthetic; Decodable minus check 5); single-use picks, `Release`, `Listed` enforcement; `SetBitrate(0)` is `invalid`; `SetLayerActive` pause and resume (keyframe, no `Reinit`); `SetPaused` stops video only; re-init mode marks `Reinit`; `Reconfigure{Profile}` swaps the SPS and emits `EventLayer`; audio-only and `TapPCM` sessions; `Pick` cancel, timeout and ctx; `Fail`, `Emit`, `Calls` (incl. `audio_muted`); the `AudioController` scenarios are 05's tests; M1's tests unchanged except reading flags from Markers | Mac, CI ubuntu, macos; CI windows (06's `windows-go`) |
| `internal/media/native` | 04 §12 and §18.3: the binding's fake-caller tests (04 owns them); among their assertions, §7.1's requirements: ABI major/minor and export checks (05's audio exports required, `im_test_*` optional), the status and event tables of §5.2, `Next` honors ctx within 50 ms, the copy rule, packet validation (audio layer value, PCM size), `EventLayer` before every `REINIT` | Mac, CI ubuntu (logic); CI windows (`windows-go`; with the DLL, `windows-desktop`, 04 §18.2) |
| `internal/client/publish` | M1 suite unchanged (pacing off by default); SR NTP equals `wall(now) − (Now − capture)` within 1 ms with a `Clock` Source whose packets are 25 ms old; pacer at 24 Mbit/s: a 300 KB frame's last packet leaves after 55–70 ms from a full bucket and 95–110 ms from an empty one, and audio written meanwhile is not delayed > 2 ms; backpressure blocks `Next` and never drops; responder size 4096 (a NACK for a packet 3000 back is answered); RTT from a crafted RR; `CaptureToSend`; `KindPCM` dropped | Mac, CI |
| `internal/client/ratectl` | table tests at fixed instants: initial targets per preset; split examples of §9.2; clamps to 04 §6.4's ranges; ramp 4 → 8 Mbit/s in 5 steps and 10 s; congestion hold; live guards (15 %, 2 s, latest wins) and re-init guards (30 %, 10 s, one-step ramp) chosen per layer, `SetLive` switching a layer mid-share; `OpActive` at once with the resume target; admin cap; upload hint on after 6 s with a binding cap, never during the ramp, off after 10 s; `SetLimit` cpu/gpu → `CPU` true and the ramp held for 10 s after `none` | Mac, CI |
| `internal/protocol` | fixtures for `share.update{paused}`, `room.state` with `paused`, `user.connections` with `deviceName`; compat replay of v1 fixtures; validation ignores nothing new | Mac, CI |
| `internal/protocol/relay` | round-trip fixtures for `playback.stop` (`{v: 1, roomId}`) and `playback.state`; tygo drift check covers `relay.gen.ts` | Mac, CI |
| `internal/server/sfu` | `nativeUplink` table tests per §10.1 row (fixed stats sequences → decisions), including the delay rows: `delay ≥ 40 ms` on one tick doesn't back off, on two it does (to 0.85 × rate, never above the current estimate); growth blocked at `delay ≥ 15 ms`, allowed when unknown; `RawLossPct` counts gaps before repair; queueing delay at fixed instants: samples only at a frame's first packet (not retransmissions, padding or an older timestamp), unwrap across the 32-bit timestamp wrap, 500 ms window minimum, baseline rising ≤ 0.5 ms per s under a simulated +300 ppm sender drift (`QueueDelay` stays < 5 ms) and dropping at once when a queue drains, −1 with < 3 samples and in the first 2 s; hint spacing 15 %/2 s; `sharerPaused` swallows keyframe requests and requests one per watched layer on unpause; web and tool kinds keep `m1Uplink` | Mac, CI |
| `internal/server/sfu/sfutest` | `NewQueueLink`: departures at the capacity in FIFO order, `Queued()` grows by `size × 8 / capacity` per datagram, RTP lost only beyond `limit`, STUN/DTLS/RTCP never lost; `ReadFrom` delivers at the departure time; other peers pass at once; `Close` ends the reader goroutine (goleak); `SetLink` with a queueing Link panics; M1's `Link` tests unchanged | Mac, CI |
| `internal/server/signal` | `paused` ignored without the feature; status precedence stalled > paused > live; pause of a starting share; any same-user connection may pause; `user.connections` on welcome, open, close, detach, room change, and after a `Revalidate` that changed `DeviceName`; coalesced ≤ 1/200 ms, only to feature holders of the same user | Mac, CI |
| `internal/client/core` (unit, with `signaltest`-style fake server where cheap) | stop table of §11.3: forced refresh once; `Auth.SessionEnded` called (bounded 15 s) before `Run` returns on `session_revoked`, `account_disabled` and a second `unauthenticated`; `RelinkRequired` → `unlinked`; `UpdateRequired` → `update_required`; `StopError.Err` carries the server error's params; `Run` after return or `Close` → `ErrClosed`; `Close` waits ≤ 3 s; `StartShare` → `ErrNotReady` when not online, ctx cancel aborts cleanly; `Status.ConnectionID` and its `EventStatus`; `playback.state` rules 1–3; `EventShare` not fired by live counters; subscriber overflow → `EventResync` | Mac, CI |

**Integration** (`internal/server/itest/desktop_*_test.go`: an in-process server, the core on `fake.Engine`, M1's
viewer clients and `sfutest.Viewer`, fault injection; cookie auth via `Options.Header` until 02's bearer path is
merged, then a bearer device from 02's `linkedManager` helper in package itest
(`internal/server/itest/device_helpers_test.go`, slice DA11). Every share starts from `fake.Engine.Pick`, as in the
app.)

| # | Case | Asserted |
|---|---|---|
| D1 | Share and watch | `StartShare` → live within 3 s; viewer gets both layers and audio; `CheckContinuous`, `CheckStartsOnSPS`; A/V offset ≤ 10 ms in-process (plan target 45 ms end to end) |
| D2 | One Participant | the core's publisher connection plus a viewer connection of the same user: `room.state` has one participant with two connections; the own viewer is no watcher; the core is in no room before and after the share |
| D3 | Socket killed mid-share | resume; media never stops (viewer gap < 500 ms); no republish |
| D4 | UDP black-holed 5 s | ICE restart, then rebuild if needed; share back to live; viewer resumes on a keyframe |
| D5 | Server restart | `welcome{resumed: false}` → republish with `replaces` within 5 s; viewer focus follows by `replaces`; `Status.ConnectionID` changed and `EventStatus` fired |
| D6 | Hold expiry | server stopped for 35 s → share ends `server-unreachable` at 30 s ± 1 s; engine stopped |
| D7 | Codec switch | a CB-only viewer joins → `quality.hint{codec: h264/42e0}` → `Reconfigure{Profile: 42e0}` and re-offer; viewer gets CB from a `Reinit` keyframe; back to High 60 s after it leaves |
| D8 | Layer pausing | no high request for 10 s → `f` inactive: `SetLayerActive(f, false)` in `Calls`, no `f` packets; a high request resumes with a keyframe ≤ 1 s (`SetLayerActive(f, true)` and `SetBitrate(f, target)` in one step) |
| D9 | Uplink congestion | `SetInboundLink` with M1's policer (`NewLink`) at 3 Mbit/s on the publisher: congestion hint ≤ 6 s; applied `f` settles ≤ 2.3 Mbit/s within 15 s; unrecovered loss < 2 % after settling; upload hint "540p" or "720p" event; capacity to 20 Mbit/s → ramps back to 8 Mbit/s ≤ 60 s, hint clears |
| D10 | Re-init encoder | probe `LiveBitrate: false`: during D9's script no two bitrate changes < 10 s apart, none < 30 %; every change is followed by a `Reinit` keyframe |
| D11 | Pause | `SetPaused(true)`: no media within 50 ms (viewer recorder); `Calls` show `paused true` and `audio_muted true`; `room.state` status `paused`; viewer PLIs swallowed; a viewer joining while paused gets no frame; resume → `audio_muted false` then `paused false` in `Calls`, keyframe ≤ 500 ms, status `live`; a pause set from a web-role connection is reconciled with the same calls |
| D12 | Preset from another connection | a web-role connection of the user sends `share.update{preset: movie}` → the core reconciles: `share.update` with the same preset, Opus `SetBitrate(audio, 256000)` |
| D13 | Encoder changed, then lost | `Emit(EventEncoderChanged{f, "mf-sw"})` and an `EventLayer` → `EventNotice{"encoder_fallback"}`, the snapshot's `f` encoder changes, same share id, viewer media continuous; then `Fail(encoder_lost)` ends the share `error`/`encoder_lost` |
| D14 | Source gone | `Fail(source_gone)` → `share.stop`, ended `source-gone`, room left |
| D15 | StopOther | a full-role connection of the same user shares in `lounge`; the core (in no room) calls `StopOther(lounge, id)` → nil once `room.state` drops the share; the core is in no room afterwards and presence never showed a second participant; with the core sharing in another room → `ErrBusy` |
| D16 | Latency proxy | §12.4 (1) |
| D17 | No leaks | goleak after every case; the engine's session stopped and the pump ended after `Close` |
| D18 | Playback state | a full-role connection of the same user sends `agent.send{to: Status.ConnectionID, kind: playback.state, payload}` → `EventPlaybackState` with `From` = that connection; an invalid payload is ignored |
| D19 | Bufferbloat (delay rule) | `SetInboundLink(NewQueueLink(6 Mbit/s, 1 s))` on the publisher; the core ramps from 4 Mbit/s (live encoder). "Crossing" is the first applied ramp step whose `f` + `q` + audio targets exceed 6 Mbit/s (from `fake.Session.Calls`). `Queued()`, sampled every 100 ms, is < 150 ms within 6 s of the crossing; a `congestion` hint arrives; no RTP datagram is lost at the queue from the start to 60 s after the crossing (`Counts()`), so the delay rule, not loss, did the backoff |

**End-to-end (browser)**: `web/e2e/latency.spec.ts` (§12.4 (2)) on CI ubuntu (headful Chrome under xvfb) and the Mac.

**Compatibility (nightly, 06's `compat-servers` job)**: `TestCompatServer` (`internal/client/core/compat_test.go`).
Skipped unless `ISSHONI_COMPAT_SERVER_URL` is set. `ISSHONI_COMPAT_ADMIN_FILE` names a JSON file
`{"username": "…", "password": "…"}` that 06's job writes after creating the admin. The test:
1. Signs in with `POST /api/v1/auth/login` and uses the session cookie as `Options.Header`.
2. Joins `lounge` (M1's default room), publishes `fake.Engine` (picked source), and checks with an `sfutest.Viewer` on
   a second cookie connection that both layers and audio arrive (`CheckContinuous` over 5 s).
3. When `/info` lists `deviceFlow`: links a `deviceauth.Manager` (memory store), approving the code with the admin
   cookie (02 §16.2 case 1's lookup and decide), runs the same share with bearer auth (`Options.Auth` = the manager),
   forces one refresh (`AccessToken(ctx, true)`), drops the socket through its `HTTPClient` dialer and checks that
   the connection comes back with the refreshed token and the share stays live.
4. Against a 0.1.x server (no `deviceFlow`): asserts that `compat.FromInfo` returns `update_server` with reason
   `no_device_flow`.

**Windows**
- CI windows-2025 (06): `windows-go` runs `go test -race` (MinGW), then `CGO_ENABLED=0 go test` (the shipped mode),
  of `./internal/client/...` with the fake engine; `windows-desktop` runs 04's binding tests (04 §18.2) with the DLL
  built in the same workflow, and `mediatest.Conformance` on the `win-x64-ci` DLL with
  `test:pattern` and `{Listed, TestTone, Encoder: "fake"}` (checks 5 and 7 skipped with a message until 04 specifies
  IM_ENC_FAKE's output).
- Owner's PC, self-hosted runner, tag `isshoni_hw` (`CGO_ENABLED=0`, no `-race`): Conformance with a real monitor
  (`Listed`) per probed encoder; `TestCaptureToSend` (§12.3).
- Owner's PC, manual: g2g on Win11 and Win10 (§12.2, the exit gate: the `gate` run with sound on and cuts, plus the
  `nocuts` and `nosound` diagnostic runs); one real-network share with a friend at 1080p60
  for 10 minutes with an upload limiter on the PC (for example clumsy's bandwidth module at 3 Mbit/s for 2 minutes):
  the upload hint appears and clears, no freeze longer than 1 s on the friend's side; the audiotest is 05's.

**Cross-compile** (CI ubuntu, every PR): `GOOS=windows CGO_ENABLED=0 go build ./internal/... ./cmd/...`; `GOOS=darwin
go vet ./...` (M1); the gomobile check of §4.

## 15. Interfaces other docs rely on

- **`internal/media`** (§5): `Engine` (with `Release`), `Session` (with `SetLayerActive`, `SetPaused`, `Stats`),
  `Packet`, `Kind` (incl. `KindPCM`), `Event`/`EventCode` and the event mapping, `Error`/`ErrorCode` (with `Reason`),
  sentinels and the status mapping, `Config` (`Listed`, `Encoder` "" = automatic, `Borderless`), `LayerConfig`
  (`Inactive`), `Reconfig`, `EngineStats`, `Probe` and its parts (`AudioCaps` derived only from `audio_flags`, with
  `media.AudioCapsFromFlags`; 03's `ShareCaps.audioBackend` is `Probe.Audio.Backend`), `Source` (`OSBorder`,
  `MonitorID`, `Capture`, `Exe`; no `Self`: a self pick is `invalid`/`self_window`), `PickRequest` (`Timeout`),
  `LayerAudio`, `ABIVersion` (the major). 03 calls `Probe`, `Pick` and `Release` (and `Sources` only to place the pill,
  never to share); 04 §12 implements `internal/media/native` against it; 05 owns `audio.go` (`AudioConfig`, `AudioMode`,
  `Exclusions`, `AudioController`, `AudioReport`, …). The engines from `native.Open` and `fake.NewEngine` implement
  `media.AudioController`.
- **`internal/media/native.Open(Options) (*Engine, error)`** with 04's `Options`: what 03's `main` calls on Windows;
  elsewhere it returns `ErrUnsupportedOS`. §7.1's requirements are what 04's binding meets.
- **`internal/media/fake.NewEngine(EngineConfig)`**: the engine of every core test, of 03's `dev` and `e2e` builds
  and of `internal/desktop/e2ehost`; `mediatest.Conformance` with `mediatest.Options` for 04's Windows CI and the
  owner's PC runner.
- **`internal/client/core`** (§11.2): `New`, `Run` (runs once), `Status` (with `ConnectionID`), `Subscribe` and the
  `Event` types (`EventPlaybackState`), `StartShare` (semantics of §11.2), `StopOther`, `ActiveShare`,
  `WatchServerStats`, `Close` (≤ 3 s); `Share` methods; `ShareSnapshot` (`NoOSIndicator`, `Minimized`, per-layer
  `Encoder`/`HW`/`Limit`/`EngineLatency`) and `EndInfo` (typed fields, `ByThisApp`); `StopError` (with `Err`) and
  `StopReason`; `Auth` (implemented by 02's `*deviceauth.Manager`); `Options.AudioConfig` (wired to 05's Service)
  and `Options.Encoder`. 03 binds these to Wails and maps events to the SPA.
- **`internal/client/ratectl.UploadHint`** and the CPU flag: what 03's share panel shows ("Your upload allows about
  720p").
- **`internal/protocol/relay`** (§11.11): `KindPlaybackStop` and `PlaybackStop{V, RoomID}` (03's definition: C1 sends
  it, `rooms/handoff.ts` receives it, both through `relay.gen.ts`); `KindPlaybackState` and `PlaybackState` (web tabs
  send it to the app's publisher connection; the core emits `EventPlaybackState`); `share.request`/`share.status`
  reserved for M4; `exclusions.*` reserved, M4's agent spec adds their payloads.
- **Protocol additions** (§10.3, §10.4): `ShareUpdate.Paused`, `ShareStatusPaused`, feature `share.pause` served;
  `user.connections` served, `OwnConnection.DeviceName`. 03's SPA renders `paused` and uses `user.connections`.
- **Server**: `nativeUplink` for `desktop`/`mobile` kinds; `quality.hint{reason: congestion, maxBitrate}`;
  `LayerUplinkStats.RawLossPct` and `QueueDelay` (§10.1's delay rule); `sfutest.FaultConn.SetInboundLink` with M1's
  policer or `sfutest.NewQueueLink(capacity, limit)`.
- **Measurement**: `tools/g2g` and the run book of §12.2 for 06's exit-test record (three JSON files per OS: `gate`,
  `nocuts`, `nosound`; only `gate` decides); `LayerSnapshot.CaptureToSend` and `EngineLatency` for 03's debug
  overlay.
- **`TestCompatServer`** (§14) for 06's nightly `compat-servers` job, with `ISSHONI_COMPAT_SERVER_URL` and
  `ISSHONI_COMPAT_ADMIN_FILE` (`{"username", "password"}`).
- **Logging rule** of §11.12 for 03's log file and 05's audio code.

## 16. Depends on

- **02-device-auth.md**: `*deviceauth.Manager` as `core.Auth`: `AccessToken(ctx, forceRefresh bool)` (≥ 120 s left
  normally, ≥ 10 s during refresh retries, one forced refresh per 10 s) and `SessionEnded(ctx, code)` (refresh once,
  then the credential deleted and the notice set on `invalid_grant`); errors with `RelinkRequired() bool` and
  `UpdateRequired() bool`; `compat.FromError` and `Manager.SetIncompatible` for `StopError.Err` (02 §13.1); the
  server's bearer path in `hello.auth` for `kind: desktop` connections (01 §3.2), so both app connections
  authenticate as the same device and resume as it; `signal.Identity.DeviceName` for bearer identities (DA6, §10.4);
  the `linkedManager` helper in package itest (`internal/server/itest/device_helpers_test.go`, DA11) for the
  integration cases; `AllowInsecureLoopback` origins for dev builds. 02's depguard needs (§18) are covered by D2.
- **03-desktop-app.md**: calls `native.Open` once per process and passes the engine to the core, to its share sheet
  and to 05's Service; calls `Pick` (and `Release` when the user backs out; it holds a pick up to 120 s) and
  `StartShare` with its 10 s waits; `StopOther(roomId, shareId)` behind `Share.StopOther(roomId, shareId)`,
  `NativeBridge.share.stopOther(roomId, shareId)` and `SharingProvider.stopOther?(roomId, shareId)`; one `Client` per
  (server, link), `Close` before switching; wires `Options.AudioConfig` to 05's `Service.AudioConfig()`; maps core
  events to the SPA (`EventPlaybackState` → 05's `SetViewerBrowser`, `Status.ConnectionID` → its connection ids);
  caps `share.state` at ≤ 4 per second; `ShareCaps.audioBackend` = `Probe.Audio.Backend` (§5.1; no `Mode`, no
  `exclude-one`); the single-instance lock (so `replaced` doesn't happen); the SPA's `paused`
  rendering; the `playback.stop` sender and receiver (03 §12.2) on `relay.gen.ts`, and web tabs sending
  `playback.state` to the app's publisher connection; "Open in the isshoni app" from `user.connections`; the upload
  and CPU hints; the "Sharing" pill from `ShareSnapshot.NoOSIndicator` (not from a request); global hotkeys calling
  `SetPaused`/`Stop`; builds `-tags dev`, `-tags e2e` (fake engine) and `-tags production` (release).
- **04-windows-engine.md**: `internal/media/native` per 04 §12, implementing `media.Engine` and `media.Session` as
  §7.1 requires (events merged into `Next`, §5.2's status and event tables, `EventLayer` at start and before every
  `REINIT`, `Stats` decoding, `Listed`, `KEEP_BORDER`, the `fake` encoder token, `media.AudioTestTone` → `TEST_TONE`); the header
  and its ABI check (04 §5.5); `im_session_info` flags and per-layer encoder and profile; single-use picks with
  `im_release_source`; the rules of §5.3 (first keyframe ≤ 500 ms, keyframe on request also on a static screen within
  04's 250 ms spacing, `Reconfigure` without capture restart, `Stop` ≤ 200 ms); per-encoder `IM_ENCF_LIVE_BITRATE`;
  the `test:pattern` source; IM_ENC_FAKE's output for Conformance checks 5 and 7; the loader
  `native.Open(native.Options)` (04 §12.2); `Probe.Audio` filled with `media.AudioCapsFromFlags(im_caps.audio_flags)`
  (§5.1: no `Mode`, no `Degraded`); E7's comparison of layer-0 VBV at 0.5 s against 0.1–0.2 s (frame-size p99, g2g
  p95 with cuts, text quality after the static refresh), with the chosen value recorded in 04 §9.3 (§12.1).
- **05-windows-audio.md**: `audio.go` (`AudioConfig`, `AudioMode`, `Exclusions`, `AudioController`, `AudioReport`,
  …) and `internal/media/native/audio_windows.go`; the fake's `AudioController` with 05 §13.3's scenarios;
  `Service.AudioConfig()` and `SetViewerBrowser`; `SetAudioMuted` semantics on pause (10 ms fade, silent frames);
  audio never fails `Start` or `Next`; the audiotest's audio-only sessions and `KindPCM` through `media.Session.Next`;
  `AudioTestTone` as the test-tone carrier for Conformance (`AudioConfig.Mode`, 05 §13.1); `AudioEverything` rejected
  with `CodeInvalid` in M2 (README §3 item 15); the meaning of `im_caps.audio_flags`
  (05 §11.1) that `AudioCaps` decodes; §10.3's chunk times at the Windows mix time, checked by audiotest case 20
  before the g2g run (§12.1 lever 1); and, only if the sound-on g2g run misses, A4/A6 lowering §9.3's running fill
  target to 20–25 ms with the drift simulation still passing (§12.1 lever 2).
- **06-windows-release.md**: CI jobs (on windows-2025: `windows-go` with the DLL-free Go tests, `-race` with MinGW and
  then `CGO_ENABLED=0`, and `windows-desktop` with the Go tests that need the DLL; macos-15 `go test
  ./internal/client/... ./internal/media/...`; the gomobile and cross-compile checks in lint-go; the self-hosted
  runner and its labels for `isshoni_hw`); the nightly `compat-servers` job running `TestCompatServer` with the env
  vars and the admin file of §14; the release check that no `-tags production` build of `cmd/isshoni-desktop` links
  `internal/media/fake`; the M2 exit-test record where §12.2's results go.
- **M1** as built: 01 §3.2, §8.7, §8.10, §8.14, §9, §10; M1 02 §8, §9, §11, §15; README §3's carried items (the
  packet type, the depguard rule); `internal/server/signal/share.go`'s `onShareStop` (room-scoped lookup, ok for
  unknown ids).

## 17. Implementation slices

In order; each is testable on its own. "Where" says where its acceptance runs.

| # | Slice | Scope | Acceptance | Needs | Where |
|---|---|---|---|---|---|
| C01 | Media contract | `internal/media` (§5: `media.go`, `probe.go` with `AudioCapsFromFlags`, `errors.go`; 05's `audio.go` types as declared in 05 §13.1); `fake.Packet`/`Kind` aliases, `Flash`/`Beep` removed (fake tests read Markers); `publish.Source` on `media.Packet`; depguard rules of §4 (siblings allowed, `internal/desktop/...` forbidden, 05's rows); gomobile and cross-compile checks in lint-go | `go test ./...` green with no change to M1 assertions; `task lint:go` with the new rules; `GOOS=android`/`ios` package builds pass | – | Mac, CI ubuntu |
| C02 | Fake engine + conformance | `fake.Engine`/`Session` (§6.2), `Source.Now`/`Origin`; `internal/media/mediatest` with `Options` (§6.4); the fake's `AudioController` is 05's `audio_ctl.go` (a no-op stub until 05's slice lands) | fake passes Conformance (Synthetic; Decodable minus 5); control, source-rule and fault-injection tests | C01 | Mac, CI ubuntu, macos |
| C03–C04 | (moved) | The Windows binding is 04's: slices E1 (pure half), E2 (loader) and E5 (`session_windows.go`) build `internal/media/native` against §7.1 | 04's acceptance | – | – |
| C05 | Publisher M2 | §8: `Clock` anchor, opt-in pacer (`Options.Pacing`, `SetPacingTargets`), responder 4096, `Reinit` handling, `640c` alias next to `6400`, stats (bitrate, fps, capture-to-send, RTT) | §14's publish rows; M1 SFU and itest suites green | C01 | Mac, CI |
| C06 | ratectl | `internal/client/ratectl` (§9): per-layer `Live`, `SetLive`, `SetLimit`, clamps | §14's ratectl table | C01 | Mac, CI |
| C07 | Protocol additions | `ShareUpdate.Paused`, `ShareStatusPaused`, `OwnConnection.DeviceName`; `internal/protocol/relay` (`playback.stop`, `playback.state`, reserved kinds); fixtures, compat, tygo entry, `task gen` | protocol and relay rows of §14; `task gen:check` clean | – | Mac, CI |
| C08 | Hub: pause and user.connections | `internal/server/signal` (§10.3 hub part, §10.4, the `Revalidate` trigger) | signal rows of §14 | C07; 02 DA6 (`Identity.DeviceName`) | Mac, CI |
| C09 | SFU: native estimator and sharer pause | `uplink_native.go` (loss and delay rows), `RawLossPct`, `QueueDelay` (sampled in the Layer's RTP loop, baseline and window of §10.1), `UplinkDecision.Reason`, `sharerPaused`, `FaultConn.SetInboundLink` with M1's policer and `NewQueueLink` | sfu and sfutest rows of §14; an SFU-level test with a Go publisher behind a 3 Mbit/s inbound policer receives a congestion hint ≤ 6 s; the same publisher behind `NewQueueLink(3 Mbit/s, 1 s)` at 4 Mbit/s receives a congestion hint before any datagram is lost at the queue | C07 | Mac, CI |
| C10 | Core: connection and start/stop | `core.go`, `conn.go`, `share.go` (`StartShare` semantics, stop, room join/leave, `StopOther`), `pump.go`, `events.go` (`Status.ConnectionID`, `EventShare` rule), stop table with `Auth.SessionEnded`, `Run` once and `Close`, cookie auth for tests | itest D1, D2, D14, D15, D17; core unit rows | C02, C05, C06 | Mac, CI |
| C11 | Core: recovery | `pubdriver.go` (§11.6), §11.7 hold, resume, republish, reconcile | itest D3–D6 | C10 | Mac, CI |
| C12 | Core: hints, codec, pause | `hints.go`: codec switch (with the actual-profile re-offer), ratectl wiring, layer pausing (`SetLayerActive`), pause/resume (`SetPaused`, `SetAudioMuted`), preset reconcile, `EventEncoderChanged` notice | itest D7, D8, D11–D13 | C10, C08, C09 | Mac, CI |
| C13 | Core: congestion end to end | D9, D10, D19; tuning of §10.1 constants within their rows | itest D9, D10, D19 | C12 | Mac, CI |
| C14 | Core: stats, server stats, playback state, compat test | `stats.go` (engine `Stats` poll, `stats` messages), `WatchServerStats`, `relay.go` (`playback.state` only), `TestCompatServer` | itest D18; `stats` messages validate and reach the hub's metrics; `TestCompatServer` passes against a locally started current server and skips without the env var | C10, C07 | Mac, CI |
| C15 | Bearer auth | core with 02's `Auth` and the server's bearer path; itests switch to a linked device; forced refresh once on `unauthenticated`; `SessionEnded` on scope-session stops | itest D1–D3 with bearer; `unauthenticated` → refresh → new connection → republish; revoked → `SessionEnded` then `StopUnlinked`, the manager unlinked | C10; 02 DA6 (bearer hello) and DA11 (`linkedManager`) | Mac, CI |
| C16 | Latency proxies | itest D16; `web/e2e/latency.spec.ts` | both pass in CI; values logged | C10 (D16); C05 (spec) | CI ubuntu, Mac |
| C17 | g2g tool | `tools/g2g` (§12.2): clock page (strip, counter, moving content, content cuts every 5 s with `?cuts=0`, the Start button and its tone), GDI capture, decoder, report with `-run gate\|nosound\|nocuts` (adds `golang.org/x/sys` to `tools/go.mod`, an integrator-owned file); decoder unit tests on recorded PNG fixtures (including a transition frame, a 50 % scaled copy, and a frame on each cut image) | decoder tests on Mac and CI; a dry run on the owner's PC decodes ≥ 90 % of screenshots with cuts on | – | Mac, CI ubuntu (decoder), owner's PC |
| C18 | Hardware checks | `TestCaptureToSend` (`isshoni_hw`), Conformance per encoder on the runner | p95 ≤ 35 ms, p50 ≤ 20 ms per encoder | 04's E5 and E7, C10 | owner's PC |
| C19 | Exit measurement | run book §12.2 on Win11 and Win10 with the release candidate, after the audiotest gate on the same rc | `gate` run (Share sound on, tone, viewer audio subscribed, cuts on): p95 < 200 ms on both; `nocuts` and `nosound` recorded; results in the exit-test record | C17, 03's app, 04, 05 | owner's PC (manual) |

C01–C09 can be built in parallel groups (C01 first; then C02, C05, C06, C07; then C08, C09); the Windows binding is
04's E1, E2 and E5; C10–C14 follow in order; C15 waits for 02; C17 is independent.

## 18. Open questions for the owner

1. **Reading of the exit criterion.** This spec takes "glass-to-glass latency under 200 ms at 1080p60" as **p95 <
   200 ms** over 60 s of a high-motion 1080p60 monitor share, through a server with RTT ≤ 50 ms from the PC, on Win11
   and Win10 (§12.2). The share is a normal one: Share sound on with a continuous tone and the viewer's audio
   subscribed, so Chrome's lip-sync hold counts, and full-screen content cuts every 5 s, so the burst after a window
   or tab switch counts. p95 without cuts and a sound-off run are recorded for diagnosis only. The alternatives are
   the median (easier), the maximum (fails on any IDR burst), or the steady, sound-off case (it passes while real
   shares with sound and cuts may not). Confirm p95, the RTT condition and the content, or name another.
