# Spike S2 (macOS): VideoToolbox H.264 encoding

Throwaway spike for plan milestone M0 (the encoder part of S2; the audio taps, picker and signing checks are separate).
**Question:** can VideoToolbox H.264 hardware encoding on a MacBook Pro M5 (macOS 27.0) meet the plan's sender targets
(1080p60 at ~8 Mbps, 1440p60 at 12–20 Mbps, 4K60 at 25–40 Mbps)? The pass criterion is that **1440p60 encodes in
≤16 ms per frame** (submit → output callback) at a real 60 fps cadence, with no dropped frames. The spike also checks
whether low-latency rate control (LLRC) works at each size, and what a second 360p15 preview layer costs.

Not production code. M3 reuses the findings, not the code.

## Run

```bash
export SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk
export MACOSX_DEPLOYMENT_TARGET=14.4   # otherwise ld warns that runtime/cgo objects target the host OS
go vet ./... && go test ./...
go run . -json results/out.json                              # full matrix, ~17 min; keep the Mac idle
go run . -sizes 1440p -formats nv12 -llrc on -suites paced   # one config, ~15 s
go run . -list-encoders                                      # H.264 encoders, and which one VT picks per size
# the repeats file:
go run . -sizes 1440p -formats nv12,bgra -llrc on -suites paced,twolayer -twolayer-sizes 1440p -repeat 3 -json out.json
```

Other flags: `-suites paced,tput,twolayer,variants`, `-duration`, `-warmup`, `-twolayer-sizes`, `-variant-sizes`,
`-repeat N`, `-csv dir` (per-frame records), `-dump dir` (Annex B `.h264` for each layer), `-activity`.

## What it does

- **Frames.** Before timing starts, the tool pre-renders a ring of 90 frames (64 for 4K BGRA, capped at 2 GiB) into
  IOSurface-backed CVPixelBuffers from a CVPixelBufferPool. Each frame shows:
  - a code editor scrolling 4 px/frame at 1440p, with syntax-coloured glyphs that are not antialiased;
  - a white document window scrolling at half that speed, a static sidebar and a frame counter;
  - a moving cursor;
  - a "video" window covering about 25% of the screen: moving plasma, ±16 grain that changes every frame, and a
    bouncing checkerboard.

  Every frame differs, and the wrap every 1.5 s acts as a scene cut. BGRA frames are drawn directly. NV12 frames
  (`420v`, BT.709) are converted from BGRA with VTPixelTransferSession.
- **Sessions.**
  - Specification: `RequireHardwareAcceleratedVideoEncoder`, with and without `EnableLowLatencyRateControl`.
  - Properties: High AutoLevel, `RealTime`, `AllowFrameReordering=false`, `ExpectedFrameRate`, `AverageBitRate`,
    `DataRateLimits` (1.5× over 1 s), `MaxKeyFrameInterval`/`Duration` of 2 s, `PrioritizeEncodingSpeedOverQuality`,
    and BT.709 colour.
  - Every OSStatus from setting a property, and every read-back, goes into the JSON (`session_examples`).
  - `PrepareToEncodeFrames` runs before the first frame.
- **Cadence.** A time-constraint (real-time) thread submits frames on a `mach_absolute_time` schedule. The process
  holds an `NSActivityLatencyCritical` activity.
- **Latency.** Measured from the frame's scheduled capture tick to the VT output callback. When the submit is on time,
  this is exactly submit → callback. When an earlier `EncodeFrame` call blocked, the wait counts too, as it would for
  an SCK callback. The JSON also has submit → callback (`encoder_latency_ms`) and how long the `EncodeFrame` call took.
- **Suites.**
  - *Paced*: a 3 s warm-up in the same session (reported separately), then 10 s measured. One keyframe is forced
    halfway through.
  - *Throughput*: unpaced, at most 8 frames in flight, 4 s.
  - *Two layers*: every 4th frame, a separate serial queue downscales the full frame to 640×360 NV12 with
    VTPixelTransferSession and encodes it in a second session (15 fps, 0.3 Mbps). The results are compared with the
    one-layer run and with a preview-only run.
  - *Variants*: one property changed at a time (NV12, 1440p and 4K).
- **Validation.**
  - The output callback converts AVCC to Annex B and puts SPS/PPS before every IDR, as the engine will.
  - Go parses every access unit: the SPS (including VUI), slice types and NAL types. It checks for High
    (profile_idc 100), no B slices, SPS/PPS before every IDR, and that VT's sync flag matches the IDRs.
  - Every stream is decoded from its Annex B with VTDecompressionSession. For NV12 input, luma PSNR is measured against
    the source.
  - `h264_test.go` tests the parser against SPS units captured from VT and decoded by hand, plus synthetic streams.

## Results (2026-09-29, MacBook Pro M5 Mac17,2, macOS 27.0 (26A428), AC power, Go 1.26.5, SDK 26.2)

Machine-readable results:
- [`results/2026-09-29-m5-macos27.json`](results/2026-09-29-m5-macos27.json): the full run;
- [`results/2026-09-29-m5-macos27-1440p-repeats.json`](results/2026-09-29-m5-macos27-1440p-repeats.json): three more
  1440p runs.

⚠ The Mac was not idle: a VM and several apps were running, and for part of the time the S2 audio self-test and its
builds ran in parallel (load average 5–6 on 10 cores, thermal state nominal).
Single-frame outliers of 18–53 ms show up in some runs, so treat the **max** columns as noisy and p99 as the stable
number. Encoders:
- `rtvc` = `com.apple.videotoolbox.videoencoder.h264.rtvc`, which VT picks when LLRC is on;
- `avc` = `com.apple.videotoolbox.videoencoder.ave.avc`, the normal hardware encoder.

### Gate: ✅ PASS for 1440p60 with LLRC and NV12 input

Across 4 runs (2,400 measured frames), 1440p60 NV12 with LLRC had:
- p99 of 7.6–10.2 ms, zero dropped frames, and 60.0 fps output;
- max ≤16 ms in 3 of the 4 runs; the fourth had 2 frames at up to 18.1 ms.

With the second (preview) session added, p99 is 9.3–12.1 ms, and 2–4 of 600 frames per run exceed 16 ms (max
17–29 ms). **Without LLRC, 1440p60 fails every time** (p99 ≈18–19 ms in every run and variant): see finding 2.

### Paced 60 fps, one layer (latency in ms, 600 measured frames)

| size | input | LLRC | encoder | p50 | p95 | p99 | max | >16 ms | Mbps (1 s max) | PSNR-Y | result |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1080p | NV12 | on | rtvc | 5.6 | 6.5 | 6.8 | 13.5 | 0 | 8.0 (8.7) | 35.6 dB | ✅ within one frame interval |
| 1080p | NV12 | off | avc | 10.4 | 10.8 | 10.9 | 16.4 | 1 | 8.0 (9.6) | 36.1 dB | ✅ within one frame interval |
| 1080p | BGRA | on | rtvc | 8.7 | 9.8 | 10.2 | 10.4 | 0 | 8.0 (8.8) | – | ✅ within one frame interval |
| 1080p | BGRA | off | avc | 11.9 | 13.0 | 13.4 | 13.6 | 0 | 8.0 (9.6) | – | ✅ within one frame interval |
| **1440p** | **NV12** | **on** | rtvc | **7.4** | 8.1 | **10.2** | 18.1 | 2 | 16.0 (17.0) | 37.4 dB | ✅ PASS at p99 |
| 1440p | NV12 | off | avc | 17.0 | 18.1 | 18.8 | 20.4 | 544 | 16.0 (18.6) | 37.5 dB | ❌ FAIL |
| 1440p | BGRA | on | rtvc | 9.2 | 10.4 | 10.8 | 12.3 | 0 | 16.0 (17.0) | – | ✅ PASS |
| 1440p | BGRA | off | avc | 18.4 | 20.8 | 21.5 | 22.6 | 450 | 16.0 (18.6) | – | ❌ FAIL |
| 4K | NV12 | on | rtvc | 14.7 | 15.1 | 15.6 | 18.3 | 3 | 29.9 (31.5) | 37.5 dB | ✅ sustains 60 fps, little headroom |
| 4K | NV12 | off | avc | 2,743 | 4,721 | 4,897 | 4,941 | 600 | 30.1 | 37.3 dB | ❌ 43.7 fps; queue grows without bound |
| 4K | BGRA | on | rtvc | 17.1 | 18.1 | 19.2 | 21.0 | 596 | 30.0 (32.0) | – | ⚠ 60 fps, but p99 > 16.7 ms |
| 4K | BGRA | off | avc | 2,163 | 3,009 | 3,134 | 3,178 | 600 | 30.1 | – | ❌ 48.1 fps |

In all 12 runs: no dropped or failed frames, SPS/PPS before every IDR, no B slices, and 780/780 frames decoded.

**Repeats of the gate config** (3 more full runs of 1440p60 with LLRC):
- NV12, one layer: p99 7.6 / 8.0 / 8.5 ms, max 7.9 / 9.2 / 13.8 ms. All three are strict passes.
- NV12, two layers: p99 12.0 / 11.1 / 9.3 ms, max 28.6 / 17.0 / 25.6 ms.
- BGRA, one layer: p99 10.6 / 13.8 / 13.0 ms, max 12.6 / 27.0 / 14.3 ms.
- BGRA, two layers: p99 12.6 / 16.6 / 14.1 ms; the second run fails.

### Two layers: full + 640×360@15 preview (LLRC on; latency in ms)

| full | input | full p50 / p99 / max | Δ p50 / p99 vs one layer | preview p50 / p99 | preview alone | downscale p50 / p95 | result |
|---|---|---|---|---|---|---|---|
| 1080p60 | NV12 | 6.3 / 13.2 / 52.9 | +0.7 / +6.4 | 6.9 / 8.5 | 3.8 / 4.5 | 1.5 / 2.1 | ✅ both keep up |
| 1080p60 | BGRA | 6.3 / 8.7 / 11.6 | −2.3 / −1.5 | 7.3 / 9.0 | 5.6 / 6.9 | 1.8 / 2.9 | ✅ both keep up |
| **1440p60** | **NV12** | 7.7 / 12.1 / 17.6 | +0.3 / +1.9 | 8.5 / 9.3 | 4.2 / 6.0 | 1.7 / 2.4 | ✅ PASS at p99 (4 of 600 >16 ms) |
| 1440p60 | BGRA | 9.4 / 13.8 / 20.5 | +0.2 / +3.0 | 9.9 / 12.2 | 5.4 / 7.1 | 2.3 / 3.6 | ✅ PASS at p99 (not reliably, see repeats) |
| 4K60 | NV12 | 25.2 / 61.2 / 64.9 | +10.5 / +45.6 | 27.8 / 64.9 | 4.7 / 6.0 | 2.4 / 3.3 | ❌ encoder saturated |
| 4K60 | BGRA | 2,225 / 3,213 / – | – | 2,159 / 3,137 | 6.2 / 8.2 | 4.4 / 5.6 | ❌ 48.6 fps |
| 4K**30** (variant) | NV12 | 15.9 / 34.2 / 36.7 | – | 24.7 / 26.6 | – | – | ✅ sustains 30 + 15 fps |

Without LLRC, the full layer behaves as it does with one layer (1440p60 p99 20.1 ms: FAIL). The preview then waits
behind it (p50 17.7 ms instead of 2.4 ms alone).

### Throughput (unpaced, ≤8 in flight, frames/s)

| | 1080p | 1440p | 4K |
|---|---|---|---|
| NV12, LLRC | 217 | 133 | 67 |
| NV12, no LLRC | 174 | 155 | 72 |
| BGRA, LLRC | 240 | 134 | **47** |
| BGRA, no LLRC | 220 | 155 | 72 |

With LLRC and NV12 at ≥1440p, `EncodeFrame` returns only after the frame is encoded (finding 4). With a single
submitting thread, throughput is then 1 / latency.

### Variants (NV12, one property changed, latency in ms)

| variant | 1440p60 p50 / p99 | 4K60 p50 / p99 |
|---|---|---|
| no LLRC, `RealTime=false` | **7.2 / 7.8** ✅ | 14.6 / 15.2 ✅ (an earlier full run: 42 fps ❌) |
| no LLRC, `ExpectedFrameRate` unset | 16.4 / 18.3 ❌ | 30.4 / 44.7 ❌ |
| no LLRC, `ExpectedFrameRate=120` | 16.1 / 19.1 ❌ | 44 fps ❌ |
| no LLRC, `PrioritizeEncodingSpeedOverQuality=false` | 17.1 / 18.6 ❌ | 47 fps ❌ |
| LLRC, no `DataRateLimits` | 7.4 / 7.8 ✅ | 14.3 / 35.7 (noise) |
| LLRC, `ExpectedFrameRate` unset | 7.4 / 8.0 ✅ | 14.7 / 16.3 ✅ |
| LLRC, 8-frame ring (like SCK's small surface pool) | 7.5 / 8.3 ✅ | 14.4 / 15.0 ✅ |
| no LLRC, 8-frame ring | 16.5 / 18.4 ❌ | 43 fps ❌ |
| two layers, preview on Apple's **software** encoder | full 7.4 / 8.0; preview **871** / 875 ❌ | full 14.4 / 15.1; preview 873 / 876 ❌ |

### Stream checks (every run)

- **Profile.** High, constraint byte `00`, levels chosen by AutoLevel: `64002a` (1080p), `640033` (1440p), `640034`
  (4K) and `640016` (preview).
- **Structure.** POC type 0, one slice per frame, only I and P slices. Every frame is a reference frame: no temporal
  layers by default (`BaseLayerFrameRateFraction` = −1).
- **LLRC SPS.** `bitstream_restriction` with `max_num_reorder_frames=0`, and 4 / 12 / 5 reference frames (1080p /
  1440p / 4K). No SEI.
- **Non-LLRC SPS.** One reference frame and no `bitstream_restriction`. One user-data-unregistered SEI comes before each
  IDR.
- **Keyframes.** Both encoders honour `MaxKeyFrameIntervalDuration`, **including LLRC**: 8 IDRs in 780 frames, one
  every 2 s plus the forced one. VT's header says LLRC means "infinite GOP", which is not what happens here.
  `ForceKeyFrame` produces an IDR on that very frame:
  - with LLRC, it takes normal-frame latency (6.6 ms at 1440p) and is **143 KB, 4.3× a P frame**;
  - without LLRC, it takes 16.8 ms and is **298 KB, 9.3× a P frame**.
- **Rate.** The average bitrate is within 0.5% of the target. The largest 1 s window reaches 1.05–1.10× the target
  with LLRC and 1.16–1.20× without it.
- **Decoding and quality.** VTDecompressionSession decoded 100% of frames, and no output was held back by the decoder.
  Luma PSNR with and without LLRC is within 0.5 dB: 35.6 / 37.4 / 37.5 dB at 8 / 16 / 30 Mbps on this deliberately
  hard content.

### Does low-latency RC work at each size?

**Yes, at 1080p, 1440p and 4K.** No fallback was ever seen:
- a session with `EnableLowLatencyRateControl` plus `RequireHardwareAcceleratedVideoEncoder` is created at every
  size;
- `EncoderID` switches to `…h264.rtvc`, which `VTCopyVideoEncoderList` does not list.

Setting `PrioritizeEncodingSpeedOverQuality` fails with `kVTPropertyNotSupportedErr` (−12900); every other property is
accepted. On `rtvc`, read-back fails for `UsingHardwareAcceleratedVideoEncoder`, `DataRateLimits` and
`MaxFrameDelayCount`, and `EnableLowLatencyRateControl` is not readable on either encoder. **`EncoderID` is the only
reliable signal** that LLRC is active.

### Findings that change the M3 design

1. **Use LLRC at every size, including 4K.** The plan's "low-latency RC up to 1440p60, RealTime mode at 4K" is wrong on
   this Mac:
   - LLRC is the only configuration that sustains 4K60 (p50 14.7 ms);
   - without LLRC, 4K60 falls to 44–48 fps and the queue grows to seconds.

   Quality is the same with LLRC, IDRs are half the size, and the largest 1 s window stays within 1.1× the target.
   The engine should read `EncoderID` back to confirm `rtvc`, and treat any other ID as "LLRC unavailable".
2. **Without LLRC, VT clocks the encoder down to just meet `ExpectedFrameRate`.** After a few seconds, latency steps
   up from ~7 to ~17 ms at 1440p (and from ~4.5 to ~10.5 ms at 1080p): about one frame interval.
   - `RealTime=false` avoids it (1440p60 p99 7.8 ms).
   - Setting `ExpectedFrameRate` to 120, leaving it unset, or turning off the speed priority does not help.
   - So the fallback for Macs or OS versions where LLRC can't be created (Intel is untested) is `avc` with
     `RealTime=false` at ≤1440p. 4K without LLRC is unreliable: with `RealTime=false`, the committed run sustained
     60 fps but an earlier full run reached only 42 fps.
   - **Apple's software H.264 encoder is not an option**: 870 ms latency, and it rejects `MaxFrameDelayCount`. This
     confirms the plan's "software fallback: not needed" only in the sense that there is none to use.
3. **Ask ScreenCaptureKit for NV12 (`kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange`, BT.709 `colorMatrix`), not
   BGRA.**
   - Both encoders list only YUV formats as native input (`420v`/`420f`, their compressed variants, and luma-only).
     With BGRA, VT converts into its own pool (`PixelBufferPoolIsShared=false`).
   - BGRA adds 1.8–3.1 ms of median latency and has a worse tail: one of four 1440p two-layer runs failed.
   - It costs 2–4× the process CPU (8–11% of a core vs 2–5%).
   - At 1440p and 4K it raises the first-frame hitch from ~50 ms to ~90–120 ms.
   - At 4K it leaves no headroom: paced p99 is 19.2 ms, above one frame interval, and unpaced throughput is 47–60 fps
     (vs 67–71 fps with NV12).
4. **`EncodeFrame` blocks with LLRC and NV12 at ≥1440p**: the call returns only after the frame is encoded, about 7 ms
   at 1440p and 15 ms at 4K. At 1080p, with BGRA input, or without LLRC, the call is asynchronous (p99 ≤0.8 ms while
   the encoder keeps up).
   The engine should therefore encode each layer on **its own serial queue**, never inline in a shared SCK handler that
   also feeds the other layer. The spike does this for the preview. A 4K encode leaves only ~2 ms of each 16.7 ms
   frame free on that queue.
5. **The preview layer is cheap up to 1440p60, but does not fit at 4K60.**
   - The VTPixelTransferSession downscale (p50) takes 1.1–1.7 ms from NV12 at 1080p–1440p and 2.1–2.4 ms at 4K
     (p95 ≤3.3 ms). From BGRA it takes 1.3–2.9 ms at ≤1440p and 2.8–4.4 ms at 4K.
   - Both sessions share one hardware encoder, which serialises them: the preview waits for the full frame (preview
     p50 8.5 ms instead of 4.2 ms alone at 1440p). The full layer barely moves (+0.3 ms p50, +2 ms p99).
   - At 4K60 the encoder is already ~90% busy (67–71 fps maximum). Adding the preview pushes the full layer to p99
     61 ms.
   - M3 options for 4K: 4K30 plus the preview (it fits), 4K60 with no preview layer, or a lower cap such as 4K48. The
     UI should not offer 4K60 together with the thumbnails.
6. **Expect a 20–120 ms hitch on the first frame of every new session** (40–120 ms with LLRC), depending on size and
   input format. The next few frames queue behind it. `PrepareToEncodeFrames` (≤2 ms) does not warm the encoder up. Change bitrate live
   (`AverageBitRate`), and don't re-create sessions for rate control. When a session has to be re-created (for example
   a resolution change), start the new one before stopping the old one.
7. **SDP / codec matching.** VT's High stream uses constraint byte `00`, so it is `6400xx`, not `640c1f`, at levels
   4.2 / 5.1 / 5.2. The native sender should advertise and match on profile_idc + constraint bits and ignore the level,
   as S4 found. No B frames are ever produced, so treating it as compatible with Constrained High receivers is safe.
   SPS/PPS come out of the format description, so the packetizer must add them in band before every IDR, as this
   spike does.
8. **PLI response is fast with LLRC.** A forced keyframe costs one normal frame time and ~4× a P frame. Without LLRC it
   costs ~2× the latency and ~9× a P frame. A GOP of 2–4 s with a PLI throttle is fine with either encoder.
9. **Timing threads need a real-time policy.** On macOS 27, a normal user-interactive thread woke from
   `mach_wait_until` 2–4 ms late, even while holding `NSActivityLatencyCritical`. A `THREAD_TIME_CONSTRAINT_POLICY`
   thread woke within 0.06 ms. This matters for the audio mixer's 10 ms clock and any pacing thread. SCK delivers
   frames on its own schedule, so video capture doesn't need it.

Minor: `rtvc` advertises undocumented properties such as `PeriodicRefreshMode`, `EnableLTR`,
`NumberOfTemporalLayers` and `RequestedMaxEncoderLatency`, but this spike does not use them. The documented
`BaseLayerFrameRateFraction` could later add non-reference frames that the SFU can drop to thin the frame rate.

### Not covered

- One machine only (M5): no Intel Mac, no macOS 14.4 / 15 / 26, and no real SCK capture. The frames are synthetic, and
  the IOSurface count made no difference (see the 8-frame-ring variant).
- PSNR is computed only for NV12 inputs.
- DataRateLimits accepts a value on `rtvc` but cannot be read back. Removing it changed nothing measurable, so whether
  LLRC enforces it is unknown.

## Files

- `vtbench.h`, `vtbench.m`: the native benchmark. It renders the ring, runs the sessions, the pacing thread and the
  preview queue, converts output to Annex B, and runs the decode check. It exposes a small pull-based C API; VT
  callbacks never call into Go.
- `vt_darwin.go`: the cgo bindings.
- `main.go`: the CLI, analysis, verdicts and JSON output. `main_other.go` is a stub for other OSes.
- `h264.go`, `h264_test.go`: Annex B, SPS/VUI and slice header parsing, and stream checks.
- `stats.go`: percentiles.
- `results/`: the JSON from the runs above.
