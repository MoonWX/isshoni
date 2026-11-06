# M3 design 02: the macOS engine (cgo + Objective-C++, static)

Status: draft for M3, 2026-11-06. `docs/PLAN.md` is the source of truth above this doc. Evidence: `spikes/s2-mac-vt`
(README findings 1–9, `vtbench.m`), `spikes/s2-mac-audio` (README, `tap_engine.m`, `dev-signing.sh`), the M2 record
(`docs/m2/README.md` §3 and §4) and the M2 specs `docs/m2/01-client-core.md` (core, media contract),
`docs/m2/04-windows-engine.md` (the C ABI v1, `native/common`, the Go binding, `isshoni-enginetest`) and
`docs/m2/05-windows-audio.md` (the audio exports' semantics), plus the code in `native/include/isshoni_media.h`,
`native/common`, `internal/media/{native,fake,mediatest}`, `cmd/isshoni-enginetest` and `cmd/isshoni-desktop`.

Written in parallel with `01-desktop-macos.md`, `03-macos-audio.md`, `04-macos-release.md` and
`05-exit-and-verification.md` (all in `docs/m3/`). A bare `01` … `05` means the M3 doc of that number; M2 docs are
written `M2 04 §6.2`, M1 docs `M1 02 §9.7`. Every interface this doc assumes from the others is stated in §19, and
every interface it provides in §18.

Tags: **M3** = build now. **later (M4/M5)** = leave room now, build in that milestone.

## 1. Scope

In this doc (all M3 unless tagged):
- **`native/macos/`**: the macOS engine, Objective-C++ (`.mm`, ARC) over `native/common`, built with CMake + Ninja
  and the Xcode Command Line Tools into one **static archive** `libisshoni_media.a` per architecture, which Go links
  through cgo. It implements the C ABI v1 (`native/include/isshoni_media.h`) with the Windows DLL's semantics: capture
  with ScreenCaptureKit behind `SCContentSharingPicker`, VideoToolbox H.264 in two layers, the session machinery
  (rings, pump, pacer, events, stats, controls), the probe, own-window exclusion and the boundary to 03's audio.
- **The C ABI v1.1 delta**: macOS constants and one named reserved field (§5.1). No size or offset changes, no new
  functions.
- **Shared portable code** that M3 adds to or moves into `native/common` (§7.7): the Apple branch of the clock, an
  AVCC → Annex B converter, and the test pattern, luma sample and fake-H.264 generator moved out of `native/windows`.
- **The Go binding's darwin half** (`internal/media/native`): the cgo caller, the link files and build tags, the
  clock, and the OS-aware parts of the shared binding (§11). The media contract additions that the macOS
  own-window fallback needs (§11.5).
- **`isshoni-enginetest` on macOS** (probe, sources, pick, capture, bench, sessions, soak, protect-check, avsync): the
  two binaries `isshoni-enginetest` and `isshoni-enginetest-hooks`, run inside 05's Dev Harness (§13);
  `native/macos/tools/hwcheck.sh`.
- The `macos-native` CI job and the Taskfile tasks that build the archive (§6.5, §6.7).

Not in this doc: the Core Audio process tap, mixer, exclusion rules on macOS, mic detection, the sound check and
`im_test_render_*` on macOS (`03-macos-audio.md`); the Wails app on macOS, its window, tray, share sheet, the
auto-minimize fallback, the first-run wizard and every UI string (`01-desktop-macos.md`); signing, the .app bundle,
Info.plist, entitlements, DMG, curl installer, Homebrew, updater and the release workflow (`04-macos-release.md`); the
M3 exit run book and the OS matrix record (`05-exit-and-verification.md`).

Not in M3: changing the shared content mid-share from macOS's menu bar (`allowsChangingSelectedContent`, later);
application picks (`SCContentSharingPickerModeSingleApplication`, later); HDR and 10-bit; HEVC/AV1; presets above
60 fps; device-loss recovery inside a session (M5, as on Windows).

## 2. Decisions at a glance

| Topic | Decision | Why |
|---|---|---|
| Where | `native/macos/` beside `native/windows/`, under the existing CMake root `native/`; the Go side stays in `internal/media/native` (darwin files) | One CMake tree, one vcpkg manifest, the same `native/common` and GoogleTest; Go's package only binds |
| Language | Objective-C++ (`.mm`) with ARC (`-fobjc-arc`); `native/common` stays C++20 and is included, not recompiled as ObjC++ | ObjC for the Apple frameworks, C++ for the portable parts; one exception boundary per export |
| Linking | Static archive per architecture, merged with `libtool -static` from the engine, `native/common` libraries, libopus and speexdsp; Go links it with `#cgo LDFLAGS: ${SRCDIR}/…/libisshoni_media.a` | Plan "macOS (cgo + ObjC, static)"; no dylib to sign, no library validation question (plan, Install) |
| Go build tag | `imstatic` selects the real engine on `darwin && cgo`, in any flavor (`production`, `dev`, `e2e`); without it `Open` stays `unsupported_os` (M2 behavior) and `dev`/`e2e` link the fake; `imtesthooks` selects the test-hooks archive | `go test ./...` on a Mac and dev/e2e bundles keep working without a native build; releases add the tag and 04's packaging verifies the engine is in the binary (§6.6) |
| Go calls | A cgo caller behind M2's `caller` interface: one C dispatcher `im_go_call(proc, args[], n, …)` reads `im_last_error` inside the same C call | `bridge.go`'s logic, `fakeCaller` and every M2 Go test stay as they are; thread-local errors need no `LockOSThread` |
| Callbacks | **None**: the archive exports no function that calls Go, and the package has no `//export` (a lint check) | Plan: native code never calls into Go from any thread |
| ABI | v1.1: macOS `os_flags` bits, picker-mode flags, `IM_SRCF_SELF_VISIBLE`, `IM_SESS_SELF_VISIBLE`, `im_caps.max_capture_fps` (from reserved space), new detail tokens. Same sizes, same functions | M2 04 §5.5's minor-bump rule; Windows ignores every new constant |
| Clock | `mach_absolute_time` in ns = `clock_gettime_nsec_np(CLOCK_UPTIME_RAW)`, everywhere: SCK display times, Core Audio host times (03), `im::os::SteadyClock`, Go's `Session.Now` | The header's macOS clock; one formula, no drift between Go and engine |
| Capture | `SCContentSharingPicker` only for the app (window first); `SCStream` with `420v` NV12 BT.709, `minimumFrameInterval` = half the frame interval, `queueDepth` 8 (SCK's maximum; §7.4's count), SCK scales to layer `f`'s size | Plan "OS pickers only"; S2-vt finding 3 (NV12); M2 04 §7.3's half-interval rule for 75–144 Hz displays |
| Encode | VideoToolbox H.264, one encoder kind (`videotoolbox`) in two modes: **LLRC** (`EnableLowLatencyRateControl` + hardware, `EncoderID` …`rtvc`) at every size, else **classic** (`…ave.avc`, `RealTime=false`, ≤ 1440p60) | S2-vt findings 1 and 2; Apple's software H.264 encoder is not usable (finding 2) |
| Layers | `f` encoded from SCK's buffer (zero copy); `q` downscaled with `VTPixelTransferSession` into the engine's pool; one encode worker thread per layer | S2-vt findings 4 and 5: `EncodeFrame` blocks at ≥ 1440p with LLRC |
| 4K | A pixel-rate budget per mode (§9.3): LLRC: one layer ≤ 3840×2160×60, two layers ≤ 3840×2160×30; classic ≤ 2560×1440×60. A two-layer start over budget fails `IM_ERR_NO_ENCODER`, `layer_failed` naming `q` | M2 04 §16 fixed this contract; S2-vt finding 5 |
| Live changes | Bitrate live (`AverageBitRate` + `DataRateLimits`, no IDR, verified by the probe); size, fps, profile and content hint re-open the session **make-before-break** | S2-vt finding 6 (first-frame hitch 40–120 ms) |
| Own windows | The app's OS layer (01's `macos.Hooks.ExcludeFromCapture`) owns `sharingType`, as M2's winos owns `WDA_EXCLUDEFROMCAPTURE`. The engine excludes isshoni's app in the `SCContentFilter` where the OS lets it; on 14.x it observes `sharingType` at pick time; otherwise it reports `SELF_VISIBLE` so 01 minimizes and mutes | Plan "Own windows" row; M2's split (`internal/desktop/app/window.go` → `OS.ExcludeFromCapture`); strategies verified per OS in slice EM9 |
| OS indicator | macOS always shows its own capture indicator for SCK: `IM_SESS_NO_OS_INDICATOR` is never set | No "Sharing" pill on macOS (plan "Share safety" requires it only for borderless Windows) |
| Software fallback | None (VT only). No OpenH264, no FFmpeg on macOS | Plan "Software fallback: not needed" |
| Threads | Dispatch queues for API work and SCK callbacks; real threads with `THREAD_TIME_CONSTRAINT_POLICY` for the video thread, the test tone and the test pattern | S2-vt finding 9: normal threads wake 2–4 ms late |
| Architectures | **Universal**: arm64 and x86_64 archives, two Go builds, `lipo` (04). x86_64 is best effort: CI builds it and runs its unit tests under Rosetta; no Intel hardware is verified unless the owner has one (§16 question 1) | Plan "one universal binary … Intel slice kept"; macOS 26 is the last release for Intel Macs, 27 is Apple silicon only |
| OS floor | Deployment target **14.4** (`-mmacosx-version-min=14.4`, `-Werror=unguarded-availability-new`), newer APIs behind `@available` | Plan "OS support"; `getCurrentProcessShareableContent` and `os_sync_wait_on_address` are 14.4 |
| SDK and tools | Xcode Command Line Tools with the macOS 26 SDK or newer; no Xcode project | 03 needs SDK 26 symbols (`CATapDescription.bundleIDs`); CLT carries the SDK |
| CI | `macos-native` job on `deploy/macos/tools.json`'s `runner` (04 §3; today GitHub's `macos-26` image, arm64) with 04's pinned Xcode: native unit tests, Go tests with `imstatic,imtesthooks`, the x86_64 link; no TCC grants there, so nothing captures the screen (§17) | GitHub's macOS runners are VMs without Screen Recording grants and, expectedly, without the hardware encoder |

## 3. From the spikes, M2 and the plan to this design

| Evidence | What it means here |
|---|---|
| S2-vt finding 1: LLRC works at 1080p, 1440p, 4K; only it sustains 4K60; `EncoderID` …`rtvc` is the only reliable signal | LLRC at every size; the probe and every session read `EncoderID` back and report the mode in `codec` (§9.1) |
| S2-vt finding 2: without LLRC VT clocks the encoder down; `RealTime=false` avoids it; the software encoder is unusable (870 ms) | Classic mode = hardware `avc` with `RealTime=false`, at most 1440p60; no software encoder outside test hooks |
| S2-vt finding 3: NV12 (`420v`, BT.709) beats BGRA in latency, CPU and tail | `SCStreamConfiguration.pixelFormat = '420v'`, `colorMatrix` 709 (§8.4) |
| S2-vt finding 4: `EncodeFrame` blocks ~7 ms at 1440p, ~15 ms at 4K with LLRC + NV12 | Each layer encodes on its own worker thread, never in the SCK handler (§7.1) |
| S2-vt finding 5: preview downscale 1.1–2.4 ms; both sessions share one encoder; 4K60 + preview saturates | `VTPixelTransferSession` for `q`; the pixel-rate budget (§9.3) |
| S2-vt finding 6: 20–120 ms first-frame hitch; change bitrate live; start a new session before stopping the old | Live bitrate; make-before-break re-opens (§9.5) |
| S2-vt finding 7: VT's High SPS has constraint byte `00`; SPS/PPS come from the format description | The AVCC converter inserts SPS/PPS; `native/common`'s normalizer keeps its constraint-byte rules (M2 04 §9.4) |
| S2-vt finding 8: a forced IDR costs one frame time with LLRC | PLI answered at once, coalesced to one per 250 ms per layer (M2 04 §6.4) |
| S2-vt finding 9: user-interactive threads wake 2–4 ms late; time-constraint threads within 0.06 ms | `im::mac::SetRealtime` for the video thread, test tone and test pattern (§7.1) |
| S2-audio: the self-signed identity kept the System Audio Recording and Local Network grants across rebuilds; LaunchServices launch attributes TCC to the app, not the terminal | enginetest runs inside 05's Dev Harness (an .app signed with the dev identity, hardened runtime, `audio-input`), started through `harness.sh` (§13.1); Screen Recording grants for tools follow the same rule |
| M2 04 §5–§6: pull ABI, rings, pump, events, handles, log ring, pacer, encoder limits, Opus stage, test tone in `native/common` | Reused as they are (§7.7). The Windows session, GPU path and encoders are rewritten for macOS |
| M2 04 §12.4: the `caller` interface and `fakeCaller` keep the binding's logic OS-free | The cgo caller implements the same interface; the M2 Go tests run unchanged on every OS |
| M2 04 §16: 4K60 two-layer is `IM_ERR_NO_ENCODER` `layer_failed` naming `q`, after which 01 offers 4K60 alone or 4K30 + preview | Kept exactly (§9.3) |
| Plan: SCK ignores `sharingType` on 15+ | §8.8's strategies per OS version, and the `SELF_VISIBLE` fallback |
| Plan "Still open": macOS 14.4/15/26, picker/window exclusion, DMG/curl install | This doc closes the first two for the engine: EM8/EM9 and §17.4 run the picker and exclusion checks on 14.4, 15.x, 26 and 27. DMG/curl install is 04's |

## 4. Files and ownership

```
native/
├─ include/isshoni_media.h        v1.1 delta (§5.1)                                                 02 (audio constants: 03)
├─ CMakeLists.txt                 + add_subdirectory(macos) when APPLE                              02
├─ CMakePresets.json              + mac-* presets (§6.1)                                            02
├─ triplets/arm64-osx-isshoni.cmake, x64-osx-isshoni.cmake   vcpkg triplets, deployment target 14.4  02
├─ common/                        additions and moves (§7.7)                                        02
└─ macos/
   ├─ CMakeLists.txt              im_macos (STATIC, ObjC++), im_media_archive (merged .a), tests      02
   ├─ src/api/exports.mm          every IM_API function; @try/try guards; N/A answers (§5.2)         02
   ├─ src/engine/                 engine.mm (process state), worker.mm (serial queue + bounded waits),
   │                              main_queue.mm (main-thread rule, §7.2), realtime.mm (SetRealtime),
   │                              os_info.mm (version, flags), probe.mm, cf_ref.h (RAII for CF types)  02
   ├─ src/capture/                picker.mm, pick_tokens.{h,cpp}, filter.mm (self-exclusion, §8.8),
   │                              sck_stream.mm, listed.mm (mon:), sources.mm (im_list_sources),
   │                              window_watch.mm, display_watch.mm, power_watch.mm,
   │                              capture_source.h (the interface the session sees), test_pattern.mm,
   │                              frame_info.{h,mm} (source pixel size from attachments, §8.5)       02
   ├─ src/video/                  frame_slots.{h,mm} (CVPixelBuffer triple buffer), pixel_transfer.mm,
   │                              budget.{h,cpp} (§9.3), nv12_pattern.{h,cpp} (CPU test pattern)     02
   ├─ src/encode/                 encoder.h (the session's encoder interface), vt_encoder.mm,
   │                              vt_props.mm, vt_probe.mm                                           02
   ├─ src/session/                session.mm (state machine), video_thread.mm, layer_worker.mm,
   │                              supervisor.mm, commands.h, stats.mm, luma_tap.mm (test hooks)      02
   ├─ src/audio_bridge/           audio_bridge.{h,mm}: which AudioEngine the exports call, test override 02
   ├─ src/audio/                  the process tap, mixer, watcher, render tests; test_exports.h
   │                              (im::audio::TestRenderTone/TestRenderBursts, Windows' signatures)  03
   ├─ src/window/protect.mm       im_protect_window (§8.8; enginetest and tests only)                02
   ├─ src/testhooks/              IM_TEST_HOOKS only: fake encoder adapter, fake picker, fault
   │                              injection, software-VT switch                                      02
   ├─ tests/                      GoogleTest (§17.1); 03 adds its audio tests; skips via 05's
   │                              tests/support/macenv.h                                             02 (03, 05)
   └─ tools/hwcheck.sh            the hardware bundle, run through 05's harness.sh (§13.3)            02
native/common/include/im/os/clock.h  + im::os::MachTicksToNs (Apple branch, §7.7)                     02
native/dist/                      build output for Go (git-ignored): darwin-{arm64,amd64}[-testhooks]/  02
internal/media/native/            darwin files (§11.1) and the OS-aware edits of bridge.go         02
internal/media/                   media.go: Source.SelfVisible, Event.SelfVisible (§11.5)           02
internal/media/fake/              EngineConfig.SelfVisible (§11.5)                                   02
cmd/isshoni-enginetest/           darwin files (§13); two outputs: isshoni-enginetest[-hooks]        02
cmd/isshoni-desktop/engine_{fake,release}.go  build constraints only (§6.4)                          02
Taskfile.yml                      native:mac; lint:native's .m/.mm globs                             02
.github/workflows/ci.yml          job macos-native, the path filter, ci-ok (§6.7)                    02
.gitignore                        native/dist/                                                       02
```

Files under `native/include`, `native/common`, `Taskfile.yml`, `ci.yml`, `internal/media/media.go` and `bridge.go` are
shared; the slice that changes them names them in its PR, as M2's README §5 rules require of the integrator.

## 5. The C ABI on macOS

### 5.1 The v1.1 delta

Lands in slice EM1. `IM_ABI_MINOR` becomes `1u` (`IM_ABI_VERSION` 0x00010001 = 65537). Besides the constants below, one
reserved field gets a name: `im_caps.reserved[0]` (offset 1508) becomes `uint32_t max_capture_fps` and the array shrinks
to `reserved[14]` (0 = unknown; the macOS engine writes 120 when an active display refreshes at ≥ 120 Hz, else 60;
Windows writes 0). Struct sizes and offsets stay M2 04 §5.4's. The Windows DLL is built from the same header and reports
1.1; it never sets or reads the new constants. 03 §4.1's audio constants are part of the same v1.1 bump and land in EM1
unconditionally, together with 03 §4.2's macOS meaning in the `im_rule` comment block: `IM_AUDIO_PROCESS_TAP` 0x10,
`IM_AUDIO_TAP_BUNDLE_IDS` 0x20, `IM_AUDIO_RESPONSIBLE_PID` 0x40, `IM_AUDIO_TCC_PREFLIGHT` 0x80,
`IM_AUDIO_STATE_PERMISSION` 5, `IM_AUDIO_STATE_FAILED` 6, `IM_AUDIOF_TEST_NO_EXCLUSIONS` 0x40000000 and
`IM_AUDIOF_TEST_NO_BUNDLE_IDS` 0x20000000 (03 owns their text and meaning). Any later addition is v1.2.

```c
/* ---- v1.1: macOS (docs/m3/02-macos-engine.md) ---- */
#define IM_ABI_MINOR 1u

/* im_caps.os_flags on macOS. IM_OS_SUPPORTED (0x1) keeps its meaning: macOS 14.4 or newer. The Windows bits
   (0x2..0x400) are never set on macOS; the macOS bits start at 0x10000 and are never set on Windows. */
#define IM_OS_SCK_PICKER        0x00010000u /* SCContentSharingPicker is available (14.0+) */
#define IM_OS_SCK_PICK_IDENTITY 0x00020000u /* a pick says which window or display it is (SCContentFilter.included*, 15.2+) */
#define IM_OS_SCK_SELF_EXCLUDE  0x00040000u /* isshoni's windows can be kept out of a display share on this OS (02 §8.8) */
#define IM_OS_VT_LOW_LATENCY    0x00080000u /* a full probe opened a low-latency RC session (EncoderID ...rtvc) */
#define IM_OS_SCREEN_RECORDING  0x00100000u /* the process holds Screen Recording: listed mon: sources work (tools) */
#define IM_OS_TRANSLATED        0x00200000u /* an x86_64 build running under Rosetta */

/* im_pick_params.flags on macOS (ignored on Windows). 0 = WINDOW | DISPLAY, windows first (plan). */
#define IM_PICK_WINDOW  0x1u
#define IM_PICK_DISPLAY 0x2u

/* im_source_info.flags: isshoni's own windows will appear in this capture (a display pick that the engine can't
   filter, 02 §8.8). The app minimizes and mutes its viewer while such a share runs. */
#define IM_SRCF_SELF_VISIBLE 0x8u
/* im_session_info.flags: the running session may show isshoni's windows (the pick's prediction, or a filter the
   system refused at im_start, 02 §8.8). */
#define IM_SESS_SELF_VISIBLE 0x4u

/* im_caps, v1.1: the first reserved word gets a name (same offset 1508, same size 1568) */
  uint32_t probe_ms;
  uint32_t max_capture_fps; /* v1.1: frames per second capture can deliver (macOS); 0 = unknown (Windows) */
  uint32_t reserved[14];
```

Also in EM1: `internal/media/native/abi.go` mirrors these constants, names `imCaps.maxCaptureFPS` and sets
`imABIMinor = 1`; it also mirrors 03's constants as `imAudioProcessTap`, `imAudioTapBundleIDs`,
`imAudioResponsiblePID`, `imAudioTCCPreflight`, `imAudioStatePermission`, `imAudioStateFailed`,
`imAudiofTestNoExclusions` and `imAudiofTestNoBundleIDs`, each listed in `abiConstants`, because
`abi_test.go`'s `TestHeaderConstants` requires a Go mirror for every header `#define` (a define without one breaks
every OS's Go tests);
`native/common/tests/testdata/abi_layout.json`'s `"abi"` becomes 65537 (sizes unchanged). Go's `goMinMinor` stays 0:
nothing in the binding needs 1.1 unconditionally, so a 1.0 DLL with a 1.1 Go build keeps working on Windows.

**New detail tokens** (M2 04 §5.7's table gains these; `errors.go` and `status.h` list them):

| Token | Status | Meaning |
|---|---|---|
| `os_stopped` | `IM_ERR_CANCELLED` (`ENDED`) | the user stopped the share from macOS's own UI (the menu bar's screen-sharing item): `SCStreamErrorUserStopped` |
| `platform` | `IM_ERR_UNSUPPORTED_OS` | the call has no meaning on this OS (`im_set_openh264_path` with a path on macOS) |
| `no_run_loop` | `IM_ERR_BAD_STATE` | a block posted to the main queue (picker, `im_protect_window`) didn't start within 2 s: the host doesn't run the main run loop (§7.2) |
| `main_thread` | `IM_ERR_BAD_STATE` | `im_pick_source` was called on the main thread; it must be called from another thread, which waits while the main queue runs the picker (§7.2) |
| `no_hardware` | `IM_ERR_NO_ENCODER` (encoder status) | VideoToolbox has no hardware H.264 encoder here (virtual machines) |
| `capture_restart_failed` | `IM_ERR_SOURCE_GONE` (`ENDED`) | SCK stopped the stream (sleep, a system stop) and one restart with the same filter failed (§8.10) |

Reused tokens with their macOS causes: `capture_denied` (Screen Recording missing for a listed source, or SCK's
`SCStreamErrorUserDeclined`), `device_removed` (`ENDED`, `IM_ERR_ENCODER_LOST`: the VT session became invalid twice
within 10 s, e.g. after the media server restarted; 01 renders "Share again" as on Windows), `layer_failed`,
`open_failed`, `test_encode_failed`, `slow_open`, `pick_busy`, `pick_expired`, `session_limit`, `engine_timeout`,
`not_initialized`, `not_watching`, `abi`, `os_too_old` (below 14.4).

### 5.2 Call reference on macOS

"N/A" calls are exported (Go resolves all 39 symbols at link time) and answer as stated. Bounds are M2 04 §5.3's
unless the row says otherwise; posted jobs wait their bound + 1 s, then `IM_ERR_TIMEOUT` `engine_timeout`.

| Call | macOS behavior | Runs on |
|---|---|---|
| `im_version` | 0x00010001 | caller |
| `im_init` | Creates the worker queue, reads OS info, calls 03's `AudioEngine::Init(flags)` (nothing audio with `IM_INIT_NO_AUDIO`), sets the log level. `ffmpeg_dir` and `openh264_path` are ignored (a non-NULL value logs one warn). A failed audio init is logged and `IM_OK` (M2 04 §6.1) | caller + worker |
| `im_shutdown` | `im_stop` on every session, cancels a pick, `AudioEngine::Shutdown()`, drains the worker; `im_init` may follow (3 cycles tested) | caller + worker |
| `im_build_info` | §5.3's JSON | caller |
| `im_set_openh264_path` | N/A: `NULL` → `IM_OK` (nothing to disable); a path → `IM_ERR_UNSUPPORTED_OS`, `platform` | caller |
| `im_clock` | three brackets of `clock_gettime_nsec_np(CLOCK_UPTIME_RAW)` around `clock_gettime(CLOCK_REALTIME)`, the tightest kept | caller |
| `im_probe` | §9.10 | worker |
| `im_list_sources` | monitors from `CGGetActiveDisplayList` (no TCC needed); `windows` empty (windows come from the picker) (§8.3) | worker |
| `im_pick_source` | `SCContentSharingPicker` (§8.2); `parent_window` is ignored (the picker is system UI); `flags` = `IM_PICK_*`. Called on the main thread → `IM_ERR_BAD_STATE` `main_thread` at once (§7.2) | main queue + caller waits |
| `im_cancel_pick`, `im_release_source` | as M2 04 §7.2 | caller |
| Audio exports (`im_list_audio_apps` … `im_audio_sound_check`) | forwarded to 03's `AudioEngine` after the M2 argument checks (§10). Bounds: `im_audio_sound_check` returns within 30 s + `timeout_ms` + 500 ms on macOS (M2's `timeout_ms` + 500 ms, plus up to 30 s for the System Audio Recording decision; the header comment of `im_audio_sound_check` gains "macOS: plus up to 30 s for the System Audio Recording decision"); every other audio call that posts to 03's queue returns within 3 s, else `IM_ERR_TIMEOUT` `engine_timeout` | 03's queues |
| `im_start` | §7.5, §8, §9; `IM_START_KEEP_BORDER` accepted and ignored (no border exists); `IM_START_HIDE_CURSOR` → `showsCursor = NO`; listed `mon:` ids need Screen Recording (§8.3) | worker |
| `im_get_session_info` | `capture_path` `IM_CAPTURE_SCK` (or `IM_CAPTURE_TEST`); flags `IM_SESS_AUDIO`, `IM_SESS_SELF_VISIBLE`; never `IM_SESS_NO_OS_INDICATOR` | caller |
| `im_next_packet` | `native/common`'s pump, unchanged | caller |
| Session controls (`im_request_keyframe` … `im_set_audio_bitrate`) | validate, push to the session's 64-entry command queue, return; `im_update_layer` also checks the pixel-rate budget synchronously (§9.3) | caller |
| `im_get_stats` | §5.3's JSON | caller |
| `im_stop` | ≤ 2 s: `stopCaptureWithCompletionHandler:` first (bounded wait 500 ms), then the threads, then the VT sessions (`CompleteFrames`, `Invalidate`) | caller + worker |
| `im_protect_window` | `native_window` = `NSWindow *` of this process (§8.8); used by enginetest and tests only, the app sets `sharingType` itself (01) | main queue + caller waits; inline when called on the main thread |
| `im_last_error`, `im_read_logs`, `im_abi_layout` | `native/common`'s, unchanged | caller |
| `im_test_render_tone`, `im_test_render_bursts` | 03 (endpoint id = a Core Audio device UID; `NULL`/"" = default output) | 03's threads |
| `im_test_opus_decode` | `native/common`'s decoder, unchanged | caller |

Events never sent on macOS: `IM_EVENT_DEVICE_RESET` (reserved, M5). `IM_ERR_AUDIO_DEVICE_CHANGED` and
`IM_ERR_AUDIO_UNAVAILABLE` follow 03 (audio never fails a call, M2 05 §4.3).

### 5.3 JSON payloads on macOS

`im_build_info` (the keys `ffmpeg` and `openh264` are omitted; Go's `BuildInfo` decodes them as zero values):
```json
{"engine":"0.3.0","abi":65537,"commit":"0affed6","testHooks":false,"os":"macos","arch":"arm64",
 "compiler":"Apple clang 17.0.0","sdk":"26.2","minOS":"14.4","opus":"libopus 1.5.2"}
```
`sdk` and `minOS` are new keys (additive); Go's `BuildInfo` gains `SDK` and `MinOS` string fields.

`im_list_sources`:
```json
{"monitors":[{"id":"mon:1","name":"Built-in Retina Display","primary":true,"x":0,"y":0,"width":3024,
 "height":1964,"dpi":192,"scale":2.0,"refreshHz":120,"rotation":0,"hdr":false,"adapter":0}],"windows":[]}
```
`id` is `mon:<CGDirectDisplayID>` (decimal; stable while the display stays attached). `x`, `y` are the display's
global origin in **points** (`CGDisplayBounds`, top-left origin); `width`, `height` are **pixels** of the current mode
(`CGDisplayModeGetPixelWidth/Height`); `scale` (new, additive) = pixels per point; `dpi` = round(96 × `scale`), so
Windows' meaning (96 = 100 %) holds; `name` = `NSScreen.localizedName` of the matching `NSScreenNumber`; `hdr` =
`NSScreen.maximumPotentialExtendedDynamicRangeColorComponentValue > 1`; `adapter` 0 (one entry in `im_caps.adapters`).

`im_get_stats`: M2 04 §5.6's schema. macOS values: `capture.path` `"sck"` (or `"test"`), `layers[].encoder`
`"videotoolbox"`, `layers[].codec` `"h264.rtvc"` (LLRC) or `"ave.avc"` (classic) — the last two components of
`EncoderID` — and `hw` true. Additive keys: `capture.held` (SCK buffers the session holds now; §7.4) and
`capture.status` (`{"complete":3600,"idle":120,"blank":0,"suspended":0}`, frame statuses since start).
`audio.state` (the string of `Mixer::State()`, `stats.mm`'s `AudioStateName`) uses M2's names for 0–4 plus 03's
states: 5 (`IM_AUDIO_STATE_PERMISSION`) → `"permission"`, 6 (`IM_AUDIO_STATE_FAILED`) → `"tap-failed"`; unknown values
stay `"off"` as on Windows. The pending/denied split is in 03's report (03 §12.1), not in the stats.
`session_fake_test` covers 5 and 6 with a fake `Mixer`.

Event detail JSON: M2 04 §5.6's table, with these macOS values (additive):
- `ENDED`: `reason` gains `"cancelled"` (status `IM_ERR_CANCELLED`, detail `os_stopped: …`).
- `CAPTURE_INTERRUPTED` / `CAPTURE_RESUMED`: `cause` is `"sleep"`, `"locked"`, `"display_sleep"` or
  `"system_stopped"` (§8.10).
- `ENCODER_CHANGED` is never sent: there is one encoder kind; a re-created VT session in another mode emits only
  `REINIT|KEYFRAME` and the binding's `EventLayer` (§9.9).

### 5.4 Status codes and the UI state each maps to (macOS rows)

The engine reports; 01 renders. Rows that differ from M2 04 §5.7:

| Code / event | Where | Engine did | UI state (01) |
|---|---|---|---|
| `IM_ERR_PERMISSION_DENIED` `capture_denied` | `im_start` of a listed source (tools), `ENDED` on `SCStreamErrorUserDeclined` | nothing | Guided fix (01's text); the app itself never needs Screen Recording |
| `IM_ERR_CANCELLED` `os_stopped` in `ENDED` | event | stopped capture | Normal stop with the notice "You stopped sharing from the menu bar" (01's string); no error |
| `IM_ERR_CANCELLED` | `im_pick_source` | nothing | silent: back to the share sheet |
| `IM_ERR_BAD_STATE` `no_run_loop`, `main_thread` | `im_pick_source` (both), `im_protect_window` (`no_run_loop`) | nothing | "Something went wrong" (a programming error in the host; logged) |
| `IM_ERR_NO_ENCODER` `layer_failed` naming `q` | `im_start` (budget, §9.3) | nothing | 01 offers 4K60 alone or 4K30 with preview (M2 04 §16) |
| `IM_ERR_NO_ENCODER` `no_hardware` | probe, `im_start` | nothing | "This Mac can't encode video in hardware" (VMs only, in practice) |
| `IM_ERR_ENCODER_LOST` `device_removed` in `ENDED` | event | ended the session | "Share again" (M2 04 §5.7) |
| `IM_ERR_SOURCE_GONE` `capture_restart_failed` in `ENDED` | event | one restart failed | Stop with a notice: "Sharing stopped after your Mac slept" |
| `IM_ERR_UNSUPPORTED_OS` `os_too_old` | probe (`IM_OS_SUPPORTED` clear), `im_start` | nothing | "Needs macOS 14.4 or newer"; viewing works |
| `IM_SRCF_SELF_VISIBLE`, `IM_SESS_SELF_VISIBLE` | pick, session info | nothing | 01's auto-minimize and viewer mute for that share (§8.8) |
| `SOURCE_MINIMIZED` | event (window sources) | keeps the last frame | as Windows |

## 6. Build and linking

### 6.1 CMake presets and vcpkg

`native/CMakePresets.json` gains (condition `hostSystemName == Darwin`; generator Ninja; vcpkg toolchain from
`VCPKG_ROOT` as the existing `base`):

| Preset | Arch | Build type | `IM_TEST_HOOKS` | Tests | Writes |
|---|---|---|---|---|---|
| `mac-arm64-dev` | arm64 | RelWithDebInfo | OFF | ON | `native/dist/darwin-arm64/` |
| `mac-arm64-ci` | arm64 | RelWithDebInfo | ON | ON | `native/dist/darwin-arm64-testhooks/` |
| `mac-arm64-asan` | arm64 | Debug, `IM_SANITIZE=address,undefined` | ON | ON | nothing (ctest only) |
| `mac-arm64-tsan` | arm64 | Debug, `IM_SANITIZE=thread` | ON | ON | nothing (ctest only) |
| `mac-arm64-release` | arm64 | RelWithDebInfo (`-O2 -g`) | OFF | OFF | `native/dist/darwin-arm64/` |
| `mac-x64-release` | x86_64 | RelWithDebInfo | OFF | OFF | `native/dist/darwin-amd64/` |
| `mac-x64-ci` | x86_64 | RelWithDebInfo | ON | ON | `native/dist/darwin-amd64-testhooks/` |

Each sets `CMAKE_OSX_ARCHITECTURES` (`arm64` or `x86_64`), `CMAKE_OSX_DEPLOYMENT_TARGET=14.4` and
`VCPKG_TARGET_TRIPLET` = `arm64-osx-isshoni` or `x64-osx-isshoni`. The overlay triplets (`native/triplets/`):
```cmake
# arm64-osx-isshoni.cmake (x64-osx-isshoni.cmake: x64 / x86_64)
set(VCPKG_TARGET_ARCHITECTURE arm64)
set(VCPKG_CRT_LINKAGE dynamic)
set(VCPKG_LIBRARY_LINKAGE static)
set(VCPKG_CMAKE_SYSTEM_NAME Darwin)
set(VCPKG_OSX_ARCHITECTURES arm64)
set(VCPKG_OSX_DEPLOYMENT_TARGET 14.4)
```
`native/vcpkg.json` is unchanged (opus, speexdsp; feature `tests` → gtest). The `host-*` presets keep building only
`native/common` on macOS and Linux (`native-host` job, unchanged).

`native/CMakeLists.txt` gains, after `add_subdirectory(common)`:
```cmake
if(APPLE AND EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/macos/CMakeLists.txt" AND IM_BUILD_MACOS)
  enable_language(OBJCXX)
  add_subdirectory(macos)
endif()
```
`option(IM_BUILD_MACOS "Build the macOS engine (mac-* presets)" OFF)`; the mac presets set it ON, `host-*` leave it
OFF so `native-host` on macos-15 builds exactly what it built in M2.

### 6.2 The engine library and the merged archive

`native/macos/CMakeLists.txt`:
- `im_macos` (STATIC): `src/**/*.mm` and `src/**/*.cpp`, `-fobjc-arc`, `im_build_flags` (`-Wall -Wextra -Wpedantic
  -Wshadow -Werror`), plus `-Werror=unguarded-availability-new` and `-fvisibility=hidden` (the header's `IM_API` keeps
  `im_*` visible). `IM_STATIC` defined. Links `im_common`, `im_audio`, `im_audiopolicy`, `im_audiomix` and 03's
  portable `im_tappolicy` (`native/common/tappolicy`). `testhooks/` sources compile only with `IM_TEST_HOOKS`.
- `im_media_archive` (custom target, `ALL`): one archive from every static library the engine needs:
  ```
  libtool -static -no_warning_for_no_symbols -o libisshoni_media.a \
    $<TARGET_FILE:im_macos> $<TARGET_FILE:im_common> $<TARGET_FILE:im_audio> \
    $<TARGET_FILE:im_audiopolicy> $<TARGET_FILE:im_audiomix> $<TARGET_FILE:im_tappolicy> \
    <vcpkg>/lib/libopus.a <vcpkg>/lib/libspeexdsp.a
  ```
  then copies into the preset's `native/dist/<dir>/`:
  - `libisshoni_media.a` and `archive.sha256` (its SHA-256, hex);
  - `licenses/vcpkg/<port>/copyright` for every installed port (today `opus` and `speexdsp`), copied from
    `<vcpkg_installed>/<triplet>/share/<port>/copyright` (the Windows `licenses/vcpkg` pattern of M2's ci.yml);
  - `build.json` in M2's native-manifest shape (04 ships it as `native-manifest-darwin-<arch>.json`):
    ```json
    {"preset":"mac-arm64-release","version":"0.3.0","commit":"0affed6","testHooks":false,"arch":"arm64",
     "minOS":"14.4","sdk":"26.2",
     "archive":{"name":"libisshoni_media.a","sha256":"<hex>","abi":65537},
     "vcpkg":{"baseline":"<builtin-baseline of native/vcpkg.json>","ports":{"opus":"<version>","speexdsp":"<version>"}}}
    ```
  A post-build check fails the build when `nm -gU libisshoni_media.a` lists a global text symbol outside `_im_*`,
  `_opus_*`, `_speex_*` and C++ mangled names (`im_tappolicy` exports C++ symbols only, which the rule already
  allows), so no stray C API leaks into the app's namespace.
- `IM_VERSION` and `IM_COMMIT` (existing cache variables, defaults `0.0.0-dev` and empty) feed `im_build_info` and
  `build.json`; `task native:mac` passes them to every preset's configure (§6.5). `IM_WINVER` is unused on macOS.

Frameworks the archive needs (declared by the Go link file, §6.4, and by the test executables):
`ScreenCaptureKit VideoToolbox CoreMedia CoreVideo CoreGraphics IOSurface AppKit Foundation CoreFoundation
CoreAudio AudioToolbox Metal` and `-lc++`. All exist at 14.4, so they link strongly; newer symbols are weak through
the deployment target and guarded with `@available`.

### 6.3 Toolchain

- **Xcode Command Line Tools** with the macOS 26 SDK or newer (`xcrun --show-sdk-version` ≥ 26.0; the owner's Mac has
  SDK 27). `task native:mac` fails early with a clear message otherwise. Full Xcode is not required locally; no
  `.xcodeproj`. In CI, `macos-native` runs on `deploy/macos/tools.json`'s `runner` and uses 04 §3's Xcode-select
  step (the pinned `xcode`, SDK ≥ `sdkMin`), so local and CI builds share one pin and `lint:pins` checks the job's
  `runs-on`. Rosetta (for the nightly x86_64 tests) is installed with 04's `softwareupdate --install-rosetta
  --agree-to-license` step when missing.
- CMake ≥ 3.28 and Ninja from vcpkg's tool fetch, as `task native:test` does in M2.
- `clang-format` from `xcrun --find clang-format` or CI's clang-format 18; `lint:native` globs `.m` and `.mm` too.

### 6.4 How Go links it: files and tags

| File (`internal/media/native/`) | Build constraint | Contents |
|---|---|---|
| `engine_other.go` | `!windows && !(darwin && cgo && imstatic)` (was `!windows`) | unchanged: `unsupported_os` |
| `engine_darwin.go` | `darwin && cgo && imstatic` | `Open` (§11.4) |
| `call_darwin.go` | `darwin && cgo && imstatic` | the cgo caller; `#cgo CFLAGS: -I${SRCDIR}/../../../native/include -DIM_STATIC`; `#cgo LDFLAGS: -framework …` (§6.2) `-lc++` |
| `call_darwin.c` | `//go:build darwin && cgo && imstatic` (first line) | the dispatcher `im_go_call` (§11.2) |
| `clock_darwin.go` | `darwin && cgo && imstatic` | `machNS()` (§11.3) |
| `link_darwin_arm64.go` | `darwin && cgo && imstatic && !imtesthooks` | `#cgo LDFLAGS: ${SRCDIR}/../../../native/dist/darwin-arm64/libisshoni_media.a` |
| `link_testhooks_darwin_arm64.go` | `darwin && cgo && imstatic && imtesthooks` | `… /native/dist/darwin-arm64-testhooks/libisshoni_media.a` |
| `link_darwin_amd64.go`, `link_testhooks_darwin_amd64.go` | the same for amd64 | `darwin-amd64[-testhooks]` |
| `archive_darwin.go` | `darwin && cgo && imstatic` | `var staticArchiveSHA string` (set with `-X`, §6.6), logged at `Open` |
| `session_darwin.go` | `darwin && cgo && imstatic` | `SourceDetail.BundleID` and `Source.Exe` of a window pick (§11.4) |
| `*_darwin_test.go` | `darwin && cgo && imstatic` | §17.2 |
| `lockmain_darwin_test.go` | `darwin && cgo && imstatic` | `func init() { runtime.LockOSThread() }`: pins the main goroutine (which only waits in `m.Run`) to thread 0 for the whole test binary, so no test goroutine is ever scheduled on the main OS thread and `pthread_main_np()` is false in every test (§7.2) |

Every darwin cgo build sets `MACOSX_DEPLOYMENT_TARGET=14.4` (otherwise `ld` warns that Go's `runtime/cgo` objects
target the host OS: S2-vt's note) and `CGO_ENABLED=1`. An amd64 build on an arm64 Mac also sets
`CC="clang -arch x86_64"` and `GOARCH=amd64`. The universal app is two Go builds and `lipo -create` (04).

Tags in use: `imstatic` (any build that should share: the release app, enginetest, audiotest on macOS, the
`macos-native` Go tests, and dev bundles that need the real engine), `imtesthooks` (with `imstatic`: links the `-ci`
archive; never in a release; 04's release check fails a binary whose `im_build_info().testHooks` is true, as on
Windows), and M2's `isshoni_hw` for owner-Mac tests, always combined as `-tags imstatic,isshoni_hw` on darwin (§17.3).

**`imstatic` selects the real engine in any flavor.** `production`, `dev`, `e2e` keep their meaning; the app's engine
choice (`cmd/isshoni-desktop`) changes its build constraints only:

| File | M2 constraint | M3 constraint |
|---|---|---|
| `engine_fake.go` | `(windows \|\| (darwin && cgo)) && (dev \|\| e2e)` | `(windows \|\| (darwin && cgo)) && (dev \|\| e2e) && !imstatic` |
| `engine_release.go` | `(windows \|\| (darwin && cgo)) && !dev && !e2e` | `(windows \|\| (darwin && cgo)) && ((!dev && !e2e) \|\| imstatic)` |

So `TAGS=dev` (and `e2e`) link the fake and need no native build, and `TAGS=dev,imstatic` is the dev-signed bundle
with the real engine that 04 §4.8's dev row and 01 use for picker and tap work under the dev identity's grants. On
Windows nothing sets `imstatic`, so both files select exactly what they did in M2; `windows-desktop` (dev and
production builds) proves it.

### 6.5 Taskfile tasks

```yaml
native:mac:          # macOS only; vars PRESETS (default "mac-arm64-dev"), VERSION (default "0.0.0-dev"),
                     # COMMIT (default `git rev-parse --short=7 HEAD`)
  # fetches vcpkg at native/vcpkg.json's baseline (as native:test), configures each preset with
  # -DIM_VERSION={{.VERSION}} -DIM_COMMIT={{.COMMIT}} (written into im_build_info and build.json, §6.2), builds,
  # runs ctest for presets with tests (ctest --preset <p>), then the archive target; prints
  # native/dist/<dir>/libisshoni_media.a and its sha256. Fails on Linux/Windows with a note.
desktop:app:macos:   # 04 §4.6 owns it (VERSION= [TAGS=] [SIGN=] [OUT=]); always universal. Only when TAGS
  # includes production or imstatic: runs native:mac PRESETS="mac-arm64-release mac-x64-release"
  # VERSION={{.VERSION}} COMMIT=<sha>, builds with -tags <TAGS>,imstatic (production → production,imstatic)
  # plus §6.6's -ldflags. TAGS=dev or e2e without imstatic links the fake, runs no native build and needs no
  # SDK 26, so it builds on macos-15 (01's e2e bundle).
```
`test:go` is unchanged (no `imstatic`): on a Mac without a native build it runs the binding against the fake caller
as in M2. `task test:go:mac-engine` runs `native:mac PRESETS=mac-arm64-ci` and then `go test -race -count=1 -tags
imstatic,imtesthooks ./internal/media/native/... ./cmd/isshoni-enginetest/...`. The enginetest binaries for the
owner's Mac are built and placed into 05's Dev Harness by `task mac:harness` (05 §4.1); this doc adds no harness
task of its own.

### 6.6 Go's build cache and the archive; proving the engine is linked

Go's cache does not hash files named in `#cgo LDFLAGS`, so a rebuilt archive could be skipped by a cached link. Every
task that links the engine passes `-ldflags "-X github.com/MoonWX/isshoni/internal/media/native.staticArchiveSHA=$(cat
native/dist/<dir>/archive.sha256)"`, which changes the link's action ID, and runs tests with `-count=1`. `Open` logs the
value.

A release binary must contain the engine: 04's `machocheck -thin` checks `_im_start` in each thin binary's symbol
table and that `go version -m`'s ldflags carry `staticArchiveSHA` equal to `archive.sha256`. The app's
`--self-check` (Probe quick) then requires `engine.loaded` (false with `engine_other.go`), `abi` 1 and `testHooks`
false. It needs no hardware encoder and no TCC grant (on hosted VMs the quick probe's encoder entry has no
`AVAILABLE` flag, which `--self-check` does not look at). The deeper checks (`Info().Name == "darwin"`,
`BuildInfo().OS == "macos"`, a `videotoolbox` entry with a status) are `isshoni-enginetest probe`'s on the owner's
Mac (§13.2).

### 6.7 CI (`.github/workflows/ci.yml`)

New job **`macos-native`** on `deploy/macos/tools.json`'s `runner` (today `macos-26`, arm64 VM) after 04 §3's
Xcode-select step (SDK ≥ `sdkMin`), run when the existing `desktop` path filter changes; that filter (which already
covers `native/**`, `internal/media/**`, `cmd/isshoni-enginetest/**`) is extended with `tools/g2g/**`,
`tools/macharness/**`, `tools/skipcheck/**` and `.github/macos-skips.txt`, so 02 and 05 share one filter. Added to
`ci-ok`'s needs:
1. vcpkg binary cache keyed like `native-host`'s, plus the triplets.
2. `task native:mac PRESETS="mac-arm64-ci mac-x64-release"`: ctest of `mac-arm64-ci` (§17.1) with
   `GTEST_OUTPUT=xml:$RUNNER_TEMP/gtest/` (and `ctest --output-junit`), so 05's `skipcheck` reads gtest XML; the
   x86_64 release archive (cross-compiled; a link check of the Intel slice).
3. `go test -race -count=1 -json -tags imstatic,imtesthooks ./internal/media/native/... ./cmd/isshoni-enginetest/...
   ./cmd/isshoni-audiotest/...` with the `-X` flag of §6.6 (§17.2), output kept as `go-test.json`.
4. `go build -tags production,imstatic ./cmd/isshoni-desktop` for arm64, and `GOARCH=amd64 CC="clang -arch x86_64"
   go build -tags production,imstatic -o /dev/null ./cmd/isshoni-desktop` against `darwin-amd64` (link check).
5. `golangci-lint run --build-tags imstatic,imtesthooks ./internal/media/native/... ./cmd/isshoni-enginetest/...`
   and a grep that fails on `//export` in `internal/media/native`.
6. 05-owned steps (05 §5): `go -C tools test ./g2g`, `isshoni-audiotest selfcheck`, and
   `tools/skipcheck -job macos-native/<runner> -ledger .github/macos-skips.txt -go go-test.json -gtest 'gtest-*.xml'`.
7. Upload artifact `mac-test-bins`: the arm64 gtest binaries of `mac-arm64-ci` and the `go test -c` binaries of the
   `imstatic,imtesthooks` packages of step 3, for 05's `macos-compat` job on `macos-14`.

`desktop-macos` is 04's (§9.1); it keeps M2's untagged Go tests and dev-flavor build as its first steps.
`native-host` keeps its `host-*` presets on ubuntu-24.04 and macos-15.

**Nightly** (`nightly.yml`, 04 owns the file; this doc owns the steps): `mac-arm64-asan` and `mac-arm64-tsan` ctest;
`mac-x64-ci` ctest and the Go tests built for amd64 run under Rosetta (`arch -x86_64`) on the arm64 runner; 60 s per
fuzz target added in §7.7 (`avcc_fuzz`) with `host-fuzz` on ubuntu.

**Windows is unchanged and proven by its own jobs**: `windows-native` (ctest of `win-x64-ci` and `win-x64-asan`
after the moves of §7.7, Go's `TestDLL*` against the DLL that now reports 1.1), `windows-go`, `windows-desktop` and
`lint-go`'s `GOOS=windows` run, plus the portable fake-caller tests that pin M2's Windows mappings (§11.6).

## 7. Threading, queues, memory and lifetime

### 7.1 Threads and queues

| Thread / queue | How many | Created by | QoS / policy | Does | Never |
|---|---|---|---|---|---|
| **API callers** | Go's | Go | — | validate, copy input, post, wait bounded | wait unbounded on the main queue |
| **Worker** `io.isshoni.media.worker` | 1 serial queue per process | `im_init` | user-initiated | probe, start and stop steps, `SCShareableContent` lookups, VT session creation for `im_start` | media work; waiting for a completion that targets itself |
| **Main queue** | the process's | AppKit (Wails) | main | picker presentation and observer, `NSWindow` work (`im_protect_window`), `NSWorkspace` and distributed notifications | waiting on the worker or a session |
| **SCK sample queue** `…capture.<n>` | 1 serial queue per session | `im_start` | user-interactive, `DISPATCH_AUTORELEASE_FREQUENCY_WORK_ITEM` | read the sample's attachments, retain the `CVPixelBuffer` into the frame slots, signal the video thread (≤ 0.2 ms) | encode, allocate after warm-up, lock against the video thread |
| **SCK delegate** | SCK's | SCK | — | `stream:didStopWithError:`: set an atomic flag and wake the supervisor | anything else |
| **Video** | 1 thread per session | `im_start` | `THREAD_TIME_CONSTRAINT_POLICY` (period = layer 0's interval, computation 2 ms, constraint = interval) | `native/common`'s `Pacer`, drains the command queue, resolves frames, posts encode jobs to the layer workers | encode, wait on a worker |
| **Layer worker** | 1 thread per layer | `im_start` | user-interactive (`QOS_CLASS_USER_INTERACTIVE`) | `VTPixelTransferSession` (layer `q`), `VTCompressionSessionEncodeFrame` (may block 7–15 ms, S2-vt finding 4), applies bitrate changes, re-opens sessions | wait on another layer |
| **VT output** | VT's | VideoToolbox | VT's | the output callback: AVCC → Annex B into the layer's preallocated scratch, normalizer, push to the layer's ring | allocate, lock, log synchronously |
| **Supervisor** | 1 thread per session | `im_start` | default | events from flags, window and display polling (500 ms), resize debounce, power and lock state, SCK restart, test-pattern start | real-time work |
| **Test pattern** | 1 thread per `test:pattern` session | `im_start` | time-constraint, period = interval | renders the CPU pattern into a pool buffer on the pattern grid and publishes it like a captured frame | — |
| **Test tone** | 1 per tone session | `im_start` | time-constraint, period 10 ms | `common/audio`'s `TestToneSource` into the session's `PcmSink` | locks, allocation |
| **03's audio threads** | 03 | 03 | 03 (HAL IO thread, mixer) | the tap, mix and `PcmSink::OnChunk` | Go |
| **Consumer** | the caller's | Go | — | `im_next_packet` | — |

`im::mac::SetRealtime(period_ns, computation_ns, constraint_ns)` (`engine/realtime.mm`) converts to mach ticks and
calls `thread_policy_set(…, THREAD_TIME_CONSTRAINT_POLICY, …)`; failure is logged once and the thread runs normal.
03 uses the same helper for its mixer.

**Layer mailbox.** The video thread posts each `Pacer` decision to its layer worker through a one-slot mailbox
`{CVPixelBufferRef (retained), idr, refresh, ts_ns, frame}`; when both layers are due on one frame they receive the same
buffer reference (each mailbox retains it once; no copy, §7.4). When the slot is still occupied, the new job replaces
it: the replaced job's `idr` is OR-ed into the new one, its buffer released, and the layer's `drops.pacer` counts it
(the encoder fell behind). The worker signals completion so the stats' `encodeMs` and the pacer's overload rule
(`LIMIT_CHANGED gpu`) see real encode times.

**Session control** is M2 04 §6.1's: validate, push onto a 64-entry command queue (a mutex held for the push only),
return; the video thread applies; a full queue answers `IM_ERR_LIMIT`; a control after `ENDED` answers `IM_OK`.

### 7.2 The main-thread rule

The app (Wails v3, pinned `v3.0.0-beta.34`) runs AppKit's run loop on the main thread; the engine never blocks it.
Calls that need the main queue (`im_pick_source`'s presentation and `im_protect_window`) `dispatch_async` a block and
wait on a semaphore: if the block hasn't **started** within 2 s, the call returns `IM_ERR_BAD_STATE` `no_run_loop`
(the block, when it runs later, sees a cancelled flag and does nothing). Calls made **on** the main thread:
- `im_protect_window` runs its block inline (it completes there; nothing waits for a later main-queue callback).
- `im_pick_source` returns `IM_ERR_BAD_STATE` with detail `main_thread` at once, before any validation side effect.
  Running the presentation inline and then waiting for the observer, whose callbacks are delivered on the main
  queue, would deadlock the main thread (not a 2 s `no_run_loop`). 01's share controller calls `Pick` from its own
  goroutine, never through `OnMain` (01 §10.1).

`isshoni-enginetest` runs an `NSApplication` on its main thread for this (§13.1); Go tests that need the main queue
to run (a real pick, `im_protect_window`) run inside it, or in 01's `internal/desktop/macos` tests, whose `TestMain`
runs an `NSApplication` on the main thread (01 §19.1); the others need no run loop (VT and SCK deliver on their own
queues). A plain `go test` binary does not lock the main goroutine to thread 0, so a test goroutine could land on the
main OS thread and get `main_thread` at random: `internal/media/native`'s `lockmain_darwin_test.go` (§6.4) calls
`runtime.LockOSThread()` in `init`, which keeps the main goroutine (blocked in `m.Run`) on thread 0 and every test
off it. That package needs no run loop: its only pick test is the fake (`TestStaticFakePick`), and in
`IM_TEST_HOOKS` builds `IM_PICK_TEST_FAKE` (§8.2.2) is evaluated **before** the `main_thread` check and never
touches the main queue, so the fake pick is deterministic on any thread.

SCK and VT completion handlers are delivered on global or SCK-owned queues, never on the worker, so a worker job can
wait for them with `dispatch_semaphore_wait` and a bound (start: 2 s per step, stop: 500 ms).

### 7.3 Objective-C lifetime and exceptions

- ARC for Objective-C objects. Core Foundation, CoreMedia, CoreVideo and VideoToolbox objects are held in
  `im::mac::CfRef<T>` (retain on copy, `CFRelease` on destruction); `CVPixelBufferRef` in `CfRef` too.
- Every thread entry and every block that touches Objective-C runs in `@autoreleasepool`; every queue the engine
  creates uses `DISPATCH_AUTORELEASE_FREQUENCY_WORK_ITEM`.
- Every export is wrapped: `@try`/`@catch (NSException *)` and C++ `try`/`catch (...)` → `IM_ERR_INTERNAL` with the
  detail `internal: <exception class>` (no message text, which could name a window). Thread entries and blocks have
  the same guard and end their session with `ENDED{IM_ERR_INTERNAL}` instead.
- Delegates and observers hold a `std::weak_ptr` (via an ObjC wrapper object) to their session, so a late SCK or
  picker callback after `im_stop` finds nothing and returns.

### 7.4 Rings, frames and memory

- Packet rings, arenas, event queue, pump, stale rule and release rule are `native/common`'s, sized as M2 04 §6.2
  (layer 0: 64 slots / 8 MiB; other layers 1 MiB; Opus 128 / 256 KiB; PCM 128 / 512 KiB; debug 64 readings).
- **Frame slots** (`video/frame_slots`): a triple buffer of retained `CVPixelBufferRef` plus each layer's "last" buffer
  for refreshes and static IDRs. SCK's pool holds `queueDepth` = **8** surfaces (SCK's maximum). The video thread
  posts the **same** buffer reference to both layers' mailboxes (§7.1), so `q` never owns an extra copy of a frame.
  Distinct SCK surfaces the session can hold at once, worst case:

  | Holder | Distinct surfaces |
  |---|---|
  | Triple-buffer slots not yet taken by the video thread (latest + one being published by the sample queue) | ≤ 2 |
  | The video thread's current frame, i.e. the buffer in `f`'s and `q`'s mailboxes (one reference, two retains) | ≤ 1 |
  | `f`'s in-flight encodes (VT encodes asynchronously at 1080p and above and keeps the source after `EncodeFrame` returns, S2-vt finding 4) together with `f`'s "last" (the newest of them, or the last completed one) | ≤ 2 |
  | `q`'s in-flight `VTPixelTransfer` (≈ 1–2.4 ms) when it lags `f` by one frame; otherwise the same buffer as `f`'s | ≤ 1 |
  | **Total** | **≤ 6 = `queueDepth` − 2** |

  So SCK keeps 2 free surfaces even with 120 fps delivery on ProMotion (`minimumFrameInterval` at half the interval,
  §8.4). `capture.held` reports the count; a debug assertion (and a test-hooks counter) fails when it exceeds
  `queueDepth − 2` = 6. Test-hooks soak (§17.1 `frame_slots_soak`): two layers at 1080p60 from a 120 Hz source for
  60 s, `capture.held` ≤ 6 throughout and no gap between consecutive SCK frames above 2 delivery intervals. Only if
  that soak fails on the Mac does EM7 add an engine-owned copy pool for "last" (4 IOSurface-backed buffers at `f`'s
  size, one `VTPixelTransferSessionTransferImage` ≈ 1 ms after each encode), so SCK surfaces are held only while in
  flight; the slice records whether it was needed.
- Layer `q`'s buffers come from its own `CVPixelBufferPool` (IOSurface-backed, 4 buffers); the test pattern's from
  a pool of 4 at layer `f`'s size.
- VT output scratch: one per layer, sized at session start to the ring's largest packet (M2 04 §9.4's
  `MaxOutputSize`), so the output callback never allocates.
- Sessions per process: 4 (`IM_ERR_LIMIT`), at most one with audio; 2 VT sessions per session.

### 7.5 Session state machine

M2 04 §6.3's machine, unchanged (RUNNING ↔ PAUSED → ENDED → freed; `im_stop` from any state; first `KEYFRAME|REINIT`
per layer within 500 ms of `im_start` returning, also on a static screen, with the 300 ms black-IDR fallback).
macOS adds one internal transition without a new state: a **capture restart** (§8.10) keeps the session RUNNING,
sends `CAPTURE_INTERRUPTED{system_stopped}` and, on success, `CAPTURE_RESUMED`; on failure the session ends with
`ENDED{IM_ERR_SOURCE_GONE, capture_restart_failed}`.

`im_stop` order: set stopping; `[stream stopCaptureWithCompletionHandler:]` (bounded 500 ms: the privacy-relevant
part, within one frame interval of the call in practice); join the video thread, layer workers, supervisor and
pattern/tone threads (≤ 2 s; a stuck thread is abandoned with an error log, never killed); `VTCompressionSession
CompleteFrames` + `Invalidate` for both layers; release slots and pools; free the handle.

### 7.6 Limits

M2 04 §6.4's table applies with these macOS values: sessions 4; output size per side 64..4096, even (VT H.264 level
5.2 allows 4096×2304); fps 1..240 accepted as the ABI says, but SCK delivers at most the display's refresh
(`max_capture_fps` in the probe: 120 on a ≥ 120 Hz display, else 60; presets use ≤ 60);
the pixel-rate budget (§9.3); picker timeout default 120 s, at most 600 s; SCK restart: one attempt per interruption,
at most 3 per session per 10 min; main-queue start bound 2 s.

### 7.7 What is reused, moved, added, rewritten

| Part | Status |
|---|---|
| `native/common`: `abi/*` (status, handles, struct_size, layout, utf8), `json`, `log` ring, `ring/*` (packet ring, event queue, pump, signal), `pace` (pacer, rate meter), `encpolicy` (limits, traits: gains the `videotoolbox` traits row), `h264` (annexb normalizer, sps, nal), `video` (scale, resize), `audio` (PcmSink, Opus stage, PCM tap, test tone, decoder), `audiopolicy`, `audiomix` (03 decides what it reuses) | **Reused** unchanged (encpolicy: one row) |
| `native/common/src/os/clock.cpp` + its header | **Changed** (`#if __APPLE__` branch): `SteadyClock::NowNs` = `clock_gettime_nsec_np(CLOCK_UPTIME_RAW)`, which Apple documents as `mach_absolute_time` converted with the timebase; **added** `int64_t im::os::MachTicksToNs(uint64_t ticks) noexcept` (Apple only; `mach_timebase_info` cached once, 128-bit multiply), the one conversion for SCK display times (§8.5) and 03's HAL `mHostTime` (03 §7.3, §13.5); `clock_test` (new) asserts `MachTicksToNs(mach_absolute_time())` against `SteadyClock::NowNs` within 1 µs |
| `native/common/tappolicy` (`im_tappolicy`) | **Added by 03** (portable tap policy); linked into `im_macos` and merged into the archive (§6.2) |
| `native/common/src/os/wake.cpp` | Reused: its Apple branch already uses `os_sync_wait_on_address` (14.4+) |
| `native/common/include/im/h264/avcc.h` | **Added**: `AvccToAnnexB(std::span<const uint8_t> avcc, uint32_t length_size, std::span<const std::span<const uint8_t>> param_sets, bool prepend, std::span<uint8_t> out) → size or error`; `avcc_test` and `avcc_fuzz` (no out-of-bounds on lying lengths, 1/2/4-byte lengths) |
| `native/windows/src/gpu/pattern_layout.{h,cpp}`, `session/pattern_grid.h`, `session/luma_sample.h` | **Moved** to `native/common/include/im/testpattern/` and `src/testpattern/` (namespace `im::testpattern`, pure C++); Windows includes the new paths. No behavior change |
| The AU generator inside `native/windows/src/testhooks/fake_encoder.cpp` | **Extracted** to `native/common/testenc/` (`im::testenc::FakeH264`, compiled only with `IM_TEST_HOOKS`); Windows' `FakeEncoder` becomes an adapter of its `VideoEncoder` interface around it; macOS has its own adapter |
| Windows session, GPU path, FFmpeg/OpenH264 encoders, picker, DD, protect | Not used on macOS |
| macOS session, capture, VT encoder, probe, picker, protect, exports | **Rewritten** in `native/macos` |

Acceptance for the moves (EM2): `windows-native`'s `session_fake_test`, `gpu_warp_test`, `luma_sample_test`,
`session_luma_tap_test` and Go's `TestDLLConformance` pass unchanged; `native-host` builds the moved files on
ubuntu and macos-15.

## 8. Capture (ScreenCaptureKit)

### 8.1 APIs and the OS version each needs

| API | Min | Use |
|---|---|---|
| `SCContentSharingPicker`, `SCContentSharingPickerConfiguration`, `SCContentSharingPickerObserver`, `presentPickerUsingContentStyle:` | 14.0 | the only way the app picks (§8.2) |
| `SCContentFilter.style`, `.contentRect`, `.pointPixelScale` | 14.0 | kind and pixel size of a pick |
| `SCContentFilter.includedDisplays`, `.includedWindows`, `.includedApplications` | 15.2 | which display/window was picked (`IM_OS_SCK_PICK_IDENTITY`) |
| `+[SCShareableContent getCurrentProcessShareableContentWithCompletionHandler:]` | 14.4 | isshoni's own windows and `SCRunningApplication`, without Screen Recording |
| `+[SCShareableContent getShareableContentExcludingDesktopWindows:onScreenWindowsOnly:completionHandler:]` | 12.3 | listed `mon:` sources only (needs Screen Recording) |
| `SCStreamConfiguration`: `captureResolution`, `ignoreShadowsSingleWindow`, `ignoreGlobalClipSingleWindow`, `shouldBeOpaque`, `preservesAspectRatio` | 14.0 | §8.4 |
| `SCStreamConfiguration.includeChildWindows` | 14.2 | menus and popups of a window share |
| `SCStreamConfiguration.captureDynamicRange` (`SCCaptureDynamicRangeSDR`), `.showMouseClicks` | 15.0 | HDR off; no click rings |
| `SCStreamErrorSystemStoppedStream` | 15.0 | §8.10 |
| `CGPreflightScreenCaptureAccess` | 10.15 | `IM_OS_SCREEN_RECORDING` (never `CGRequestScreenCaptureAccess` in the engine) |
| `clock_gettime_nsec_np(CLOCK_UPTIME_RAW)` | 10.12 | the engine clock |

The picker needs **no Screen Recording grant** (plan): the user's pick is the consent. Starting an `SCStream` with a
picker filter therefore works for a self-signed app on every supported version; macOS 15's periodic Screen
Recording re-confirmation does not apply to picker sessions. Only tools that start listed sources need the grant.

### 8.2 The picker

1. `im_pick_source` validates (one pick at a time: `IM_ERR_LIMIT` `pick_busy`; `timeout_ms` 0 → 120 000, ≤ 600 000),
   assigns a pick id, and dispatches to the main queue (§7.2).
2. On the main queue: `SCContentSharingPicker *p = SCContentSharingPicker.sharedPicker;` the engine's observer is
   added once (`addObserver:`). A configuration is built:
   ```objc
   SCContentSharingPickerConfiguration *c = [SCContentSharingPickerConfiguration new];
   c.allowedPickerModes = modes;            // from flags: SingleWindow and/or SingleDisplay
   c.excludedBundleIDs = @[ NSBundle.mainBundle.bundleIdentifier ?: @"" ]; // isshoni's own windows are never offered
   // no excludedWindowIDs: excludedBundleIDs already covers every window of this process
   c.allowsChangingSelectedContent = NO;     // later: switching content mid-share
   p.defaultConfiguration = c;
   p.active = YES;
   [p presentPickerUsingContentStyle:style]; // SCShareableContentStyleWindow unless flags == IM_PICK_DISPLAY
   ```
   `flags` 0 or `WINDOW|DISPLAY` → modes `SingleWindow | SingleDisplay`, style Window (the plan's "defaults to a
   window"); `IM_PICK_DISPLAY` → `SingleDisplay`, style Display; `IM_PICK_WINDOW` → `SingleWindow`, style Window.
3. The observer methods run on the main queue and complete the pending pick only when its id matches:
   - `contentSharingPicker:didUpdateWithFilter:forStream:` with `stream == nil` → success with that filter;
   - `contentSharingPicker:didCancelForStream:` → `IM_ERR_CANCELLED`;
   - `contentSharingPickerStartDidFailWithError:` → `SCStreamErrorUserDeclined` → `IM_ERR_PERMISSION_DENIED`
     `capture_denied`; anything else → `IM_ERR_INTERNAL` with the code.
   A callback with no pending pick (the user used the system menu while `active` was on) is logged and ignored.
4. After each pick, `p.active = NO` (isshoni leaves the system's screen-sharing menu). Timeout and `im_cancel_pick`
   set `active = NO`, complete the pick with `IM_ERR_CANCELLED` and ignore any later callback of that id. Whether
   `active = NO` also closes an open picker panel is checked in EM8 (M-PICK-MAC) and recorded here; the late-callback
   rule makes it harmless either way.
5. The result is stored under `pick:<n>` with M2 04 §7.2's token rules (single use, a failed `im_start` keeps it,
   5 min, 4 unused, `pick_expired`). `im_source_info` is filled from the filter (§8.2.1).

#### 8.2.1 What a pick says, per OS version

| Field | 15.2 and newer (`IM_OS_SCK_PICK_IDENTITY`) | 14.4 – 15.1 |
|---|---|---|
| `kind` | `filter.style`: Window → `IM_SOURCE_WINDOW`, Display → `IM_SOURCE_MONITOR` | the same |
| `width`, `height` | `filter.contentRect.size × filter.pointPixelScale`, rounded | the same |
| `name` | window: `includedWindows[0].title`, else its `owningApplication.applicationName`; display: `NSScreen.localizedName` of `includedDisplays[0].displayID` | "" (01 shows a generic "Window" / "Screen") |
| `pid` | `includedWindows[0].owningApplication.processID` | 0 |
| `window` | `includedWindows[0].windowID` (CGWindowID) | 0 |
| `monitor_id` | display: `mon:<includedDisplays[0].displayID>` | "" |
| `IM_SRCF_SELF` | `pid == getpid()` (cannot happen with `excludedBundleIDs`; checked anyway) | never |
| `IM_SRCF_SELF_VISIBLE` | §8.8 | §8.8 |
| `capture_path` | `IM_CAPTURE_SCK` | the same |

Consequence for 03: before 15.2 the shared window's pid is unknown, so `im_exclusions.shared_pid` is 0 (03 decides
how "the shared app is never auto-excluded" degrades there; §19).

#### 8.2.2 Test hooks: the fake picker

In `IM_TEST_HOOKS` builds, bit `0x80000000` of `im_pick_params.flags` (`IM_PICK_TEST_FAKE`, header-external like the
luma bit; Go's constant `pickTestFake` with a test tying the two) returns at once a `pick:` token backed by a
`test:pattern` source of 1280×720 (kind window), so Go's `Pick` path, token rules and `SourceDetail` run in CI. The
bit is checked right after argument validation and before §7.2's `main_thread` check; the fake never dispatches to
the main queue, so it answers the same on any thread (`picker_logic_test` calls it on the main thread too).
A release build refuses the bit with `IM_ERR_INVALID_ARG`.

### 8.3 Listed sources and `im_list_sources`

- `test:pattern`: the CPU test pattern (§8.7), no TCC, in release builds too (enginetest needs it, as on Windows).
- `mon:<id>` (with `IM_START_ALLOW_LISTED_SOURCE` only; the app never sets it): `CGPreflightScreenCaptureAccess()`
  false → `IM_ERR_PERMISSION_DENIED` `capture_denied` (the engine never prompts); else
  `getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:YES` (bounded 2 s), the `SCDisplay` with that
  `displayID` (absent → `IM_ERR_SOURCE_GONE`), and the filter `initWithDisplay:excludingApplications:@[self app]
  exceptingWindows:@[]` (self app from the same content), so listed captures always exclude isshoni.
- `im_list_sources` uses only CoreGraphics and AppKit (no TCC): `CGGetActiveDisplayList`, `CGDisplayBounds`,
  `CGDisplayCopyDisplayMode` (pixel size, refresh), `CGDisplayIsMain`, `CGDisplayRotation`, `NSScreen.screens`.

### 8.4 `SCStream` configuration

```objc
SCStreamConfiguration *c = [SCStreamConfiguration new];
c.width  = out_w;  c.height = out_h;                 // layer f's output size (§8.7), even
c.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;   // '420v', S2-vt finding 3
c.colorMatrix = kCGDisplayStreamYCbCrMatrix_ITU_R_709_2;
c.colorSpaceName = kCGColorSpaceSRGB;                 // Display P3 content is color-matched to sRGB/709
c.minimumFrameInterval = CMTimeMake(1, 2 * fps0);     // half the interval: 75-144 Hz displays deliver > fps0,
                                                      // the pacer thins them (M2 04 §7.3's rule)
c.queueDepth = 8;                                     // SCK's maximum; §7.4's holder count
c.showsCursor = !(flags & IM_START_HIDE_CURSOR);
c.capturesAudio = NO;                                  // audio is 03's tap
c.scalesToFit = YES;  c.preservesAspectRatio = YES;   // letterbox on resize until the re-init (§8.6)
c.captureResolution = SCCaptureResolutionBest;        // pixels, not points
c.ignoreShadowsSingleWindow = YES;  c.shouldBeOpaque = YES;  c.ignoreGlobalClipSingleWindow = YES;
if (@available(macOS 14.2, *)) c.includeChildWindows = YES;
if (@available(macOS 15.0, *)) { c.captureDynamicRange = SCCaptureDynamicRangeSDR; c.showMouseClicks = NO; }
c.backgroundColor = CGColorGetConstantColor(kCGColorBlack);
c.streamName = @"isshoni";
```
`[[SCStream alloc] initWithFilter:filter configuration:c delegate:d]`, `addStreamOutput:type:SCStreamOutputTypeScreen
sampleHandlerQueue:` the session's sample queue, `startCaptureWithCompletionHandler:` (bounded 2 s; an error maps
by §8.9's table). `im_start` returns once the stream started; the first frame usually follows within one refresh.

### 8.5 Frames, statuses, timestamps

On the sample queue, per `CMSampleBuffer`: read `SCStreamFrameInfoStatus` from the first attachment dictionary:
- `SCFrameStatusComplete`: take `CMSampleBufferGetImageBuffer`, `SCStreamFrameInfoDisplayTime` (mach ticks) →
  `capture_ts_ns` = `im::os::MachTicksToNs(ticks)` (§7.7), and the **source pixel size** for §8.6 (below); publish
  into the frame slots and signal the video thread. A time not after the previous frame's is bumped by 1 µs; none is
  before `epoch_ns`.

**Source pixel size** (`capture/frame_info.{h,mm}`, `SourceSize FrameSourceSize(CFDictionaryRef attachments)`, pure
over the attachment dictionary): `round_even(SCStreamFrameInfoContentRect.size / SCStreamFrameInfoContentScale ×
SCStreamFrameInfoScaleFactor)` per side (all three attachments exist since 12.3). With `scalesToFit = YES` the
content rect describes the content **after** SCK scaled it into `out_w×out_h` (§8.4), so `ContentRect ×
ScaleFactor` alone is the scaled size, not the source's: on every scaled display share (e.g. the built-in 3024×1964
at the 1080p budget, 1786×1160, §8.7) it would differ from the source size on every frame and loop §8.6's
resize/re-open path. Dividing by `ContentScale` (output pixels per source point, < 1 when SCK downscales) undoes
SCK's scaling. A frame missing any of the three keys or with `ContentScale ≤ 0` yields no size (no resize decision
from that frame). §8.6 compares the result with the configured source size with a tolerance of **±2 px** per side
(rounding of the scale chain). Slice EM7 confirms the formula on the real Retina display (its acceptance below); if
it does not hold there, §8.6 falls back to the supervisor's 500 ms checks as the only resize detector
(`CGWindowListCopyWindowInfo` `kCGWindowBounds` × the window's display scale for windows, the display
reconfiguration callback and `CGDisplayCopyDisplayMode` pixel size for displays), and this paragraph records which
detector shipped.
- `SCFrameStatusIdle`: nothing changed; nothing published (the pacer's refresh and static-IDR rules work from the
  last frame).
- `SCFrameStatusBlank`, `SCFrameStatusSuspended`: counted; for window sources they feed §8.6's minimized rule.
- `SCFrameStatusStarted`, `SCFrameStatusStopped`: logged at debug.
Statuses are counted in `capture.status`. A frame whose pixel buffer isn't `'420v'` (never expected) is dropped with
one warn log.

### 8.6 Source changes

| Change | Detection | Action |
|---|---|---|
| Window resized; display mode changed | the source pixel size (§8.5) differs from the configured source size by more than 2 px on a side | `SOURCE_RESIZED{w,h}` at once; SCK letterboxes meanwhile; after 1 s of a stable size (`native/common`'s resize debounce) the **resize sequence** below |
| Window minimized | window source: no Complete frame for 500 ms **and** `CGWindowListCopyWindowInfo(kCGWindowListOptionIncludingWindow, id)` says `kCGWindowIsOnscreen` false (bounds and on-screen keys need no TCC) | `SOURCE_MINIMIZED`; keep the last frame; `SOURCE_RESTORED` with the next Complete frame. Before 15.2 (no window id) only the frame rule applies, after 2 s |
| Window on another Space, behind a full-screen app | SCK keeps delivering (`ignoreGlobalClipSingleWindow`) | nothing; verified in M-SCK-WINDOW |
| Window closed / app quit | window id gone from the window list for 1 s, or `stream:didStopWithError:` `SCStreamErrorNoCaptureSource` | `ENDED{IM_ERR_SOURCE_GONE}` |
| Display disconnected | `CGDisplayRegisterReconfigurationCallback` `kCGDisplayRemoveFlag` for the display (or the stream error) and still absent after 5 s | `ENDED{IM_ERR_SOURCE_GONE}` |
| Display rearranged, second display added | reconfiguration callback | nothing unless the size changed |

The supervisor runs the window-list and display checks every 500 ms for the session's source only.

**Resize sequence** (supervisor, after the debounce; order matters because `updateConfiguration:` takes effect
asynchronously and buffers already in flight keep the old size):
1. Compute the new configured source size and layer `f`'s new output size (§8.7) and `q`'s (§9.3).
2. Post a re-open to each layer worker (§9.5) and wait (bounded 2 s) until both **new** VT sessions exist and are
   prepared; the old sessions keep encoding meanwhile. A failed creation keeps the old configuration and follows §9.9.
3. Only then `[stream updateConfiguration:c completionHandler:]` with the new `width`/`height` (14.0+, no stream
   restart).
4. Layer `f`'s worker picks a session **by the incoming buffer's** `CVPixelBufferGetWidth/Height`: a buffer at the
   old size goes to the old session (or is dropped and counted in `drops.stale` once the old session is retired); the
   first buffer at the new size goes to the new session as `REINIT|KEYFRAME`. VT never receives a buffer whose size
   differs from its session's `srcAttrs` (§9.2), so `f` stays zero copy and no hidden VT scaling or encode error can
   occur.
5. The old session is then `CompleteFrames` + `Invalidate`d on a background queue (§9.5). Layer `q` needs only its
   own re-open at its new output size: `VTPixelTransferSessionTransferImage` accepts any source size, so the switch
   of `f`'s buffer size needs nothing else there; its first packet from the new session is `REINIT|KEYFRAME`.
`session_fake_test`'s mid-stream resize case asserts no encode error and exactly one `REINIT` per layer (§17.1).

### 8.7 Size, retina, cursor, color, HDR

- **Size**: source pixel size = content size in points × scale (`captureResolution = Best`). Layer `f`'s output is
  `native/common`'s scale rule (`video/scale`: fit into the layer's `max_pixels` and box, even sides, 64..4096) —
  e.g. a 3024×1964 Retina display at the default 2 073 600 px becomes 1786×1160. SCK scales on the GPU, so layer `f`
  receives exactly its encoder size (zero copy into VT). Layer `q` is scaled from `f`'s buffer (§9.3).
- **Cursor**: `showsCursor`; the pointer is composited by SCK. No click rings.
- **Color**: BT.709 limited range NV12; VT tags BT.709 primaries, transfer and matrix (§9.2).
- **HDR**: off. `captureDynamicRange = SDR` on 15+; 14.x captures SDR only. HDR and 10-bit are later.
- **Multiple displays**: one display per pick; a window spanning displays is SCK's to composite.
- **DRM**: protected video captures as black (plan, Media and quality).

### 8.8 isshoni's own windows

Goal (plan): viewers never see isshoni's windows (the viewer showing friends' streams would loop). **Ownership is
M2's split**: the app's OS layer sets `sharingType` on its own windows (01's `macos.Hooks.ExcludeFromCapture` /
`IncludeInCapture`, called from `internal/desktop/app/window.go` as M2 calls `OS.ExcludeFromCapture` for
`WDA_EXCLUDEFROMCAPTURE`), so dev and e2e builds on the fake engine protect windows the same way. The engine never
needs to be told about a window: it keeps isshoni out of the filter where it can and **observes** the windows
otherwise. Three mechanisms, chosen per OS version by `capture/filter.mm`'s table, which EM9 fills from
measurements:

| Strategy | OS | How | Status |
|---|---|---|---|
| **A. Picker exclusion** | 14.0+ | `excludedBundleIDs` = isshoni's bundle id (§8.2 step 2; it covers every window of the process, so no `excludedWindowIDs`). Always applied: isshoni windows are never offered as window picks. Whether it also removes them **from a display pick's content** is unknown | EM9 measures it on 14.4, 15.x, 26, 27 |
| **B. Rebuilt display filter** | 15.2+ | for a display pick: `SCDisplay *d = filter.includedDisplays[0]`; own app from `getCurrentProcessShareableContent` (14.4+, no TCC); `[[SCContentFilter alloc] initWithDisplay:d excludingApplications:@[ownApp] exceptingWindows:@[]]`, used instead of the picker's filter | EM9 measures whether SCK accepts it **and** whether macOS shows any TCC prompt for it |
| **C. Observed `sharingType`** | 14.x | SCK honors `NSWindowSharingNone` before 15 (plan). At pick time, on the main queue, the engine reports the display pick as not self-visible only if every visible window in `NSApp.windows` (`isVisible`, including miniaturized ones) has `sharingType == NSWindowSharingNone`; otherwise it sets `IM_SRCF_SELF_VISIBLE` | EM9 measures it on 14.4 |
| **Fallback** | where none works | the pick carries `IM_SRCF_SELF_VISIBLE`; 01 auto-minimizes isshoni and mutes in-app viewer audio while the share runs (plan) | — |

**A strategy that makes macOS show any TCC prompt (Screen Recording) counts as not working on that OS.** B is used only
where M-SELF-EXCLUDE saw it start with no prompt, in a build signed with 04's release-shaped requirement; otherwise the
release app would ask for Screen Recording, which breaks §5.4's "the app itself never needs Screen Recording" and 04's
grant model (§5.2), which has no Screen Recording grant to keep. EM9 fills this table (the empty cells are measurements,
not decisions):

| OS | A filters display content | B starts | B prompts | C honored | Chosen |
|---|---|---|---|---|---|
| 14.4 | (EM9) | n/a (15.2+) | n/a | (EM9) | A, else C, else fallback |
| 15.0 / 15.1 | (EM9) | n/a | n/a | n/a (ignored on 15+) | A, else fallback |
| 15.2+ | (EM9) | (EM9) | (EM9) | n/a | A, else B if no prompt, else fallback |
| 26 | (EM9) | (EM9) | (EM9) | n/a | as 15.2+ |
| 27 | (EM9) | (EM9) | (EM9) | n/a | as 15.2+ |

Rules:
- A **window** pick never needs any of this (isshoni's windows aren't offered, and a window capture shows only that
  window). `IM_SRCF_SELF_VISIBLE` is set only on display picks.
- At pick time the engine applies the first strategy that the table marks verified for the running OS: A (if it
  proved to filter display content), else B (15.2+, no prompt), else C (14.x, by observation), else the fallback.
  The pick's `IM_SRCF_SELF_VISIBLE` says the outcome; `IM_OS_SCK_SELF_EXCLUDE` in the probe says whether A or B works
  on this OS (C depends on the app's setting, so it doesn't set the flag).
- With C, the decision is made once per pick. If the user turns `hideFromCapture` off during a share (01 sets
  `NSWindowSharingReadOnly`), the running share keeps its flag; the change takes effect at the next pick, and 01's
  settings text says so.
- At `im_start`, a rebuilt filter (B) that SCK refuses (`startCapture` error `SCStreamErrorUserDeclined` or
  `…FailedToStart`) is retried once with the picker's original filter within the same `im_start`; the session then
  carries `IM_SESS_SELF_VISIBLE` (Go reports it in `EventLayer`, §11.5) and logs the strategy failure at warn, so 01
  minimizes after the fact. That costs one extra stream start (≤ 300 ms) only in the failing case.
- Listed `mon:` sources always use the exclude-own-app filter (§8.3).

**`im_protect_window(NSWindow *w, flags)`** stays in the ABI for `isshoni-enginetest protect-check` and tests only
(the app never calls it; `(*native.Engine).ProtectWindow` exists in `bridge.go` and gains `UnprotectWindow`, §11.1).
On the main queue (§7.2; inline when called on the main thread): checks in order NULL → unknown flags →
`[NSApp.windows containsObject:w]` (a top-level window of this process; else `IM_ERR_INVALID_ARG`). Then `flags` 0:
`w.sharingType = NSWindowSharingNone`; `IM_PROTECT_OFF`: `sharingType = NSWindowSharingReadOnly`. Below 14.4:
`IM_ERR_UNSUPPORTED_OS` `os_too_old`. The engine keeps no protected set: C reads `sharingType` itself, so a window
protected by the app's OS layer and one protected through this call are the same to it.

### 8.9 SCK errors and macOS's own indicator

macOS shows its own capture indicator for every SCK stream (the menu bar's screen-sharing item and the Control Center
entry), so `IM_SESS_NO_OS_INDICATOR` is never set and 01 needs no "Sharing" pill. The user can stop a share from
that item; SCK then reports `SCStreamErrorUserStopped`.

`stream:didStopWithError:` and `startCapture` errors (`SCStreamErrorDomain`), by name:

| Error | Result |
|---|---|
| `UserStopped` | `ENDED{IM_ERR_CANCELLED, os_stopped}` |
| `UserDeclined` | at start: §8.8's retry rule for a rebuilt filter, else `IM_ERR_PERMISSION_DENIED` `capture_denied`; mid-share: `ENDED` with the same |
| `NoCaptureSource`, `NoWindowList`, `NoDisplayList` | `ENDED{IM_ERR_SOURCE_GONE}` |
| `SystemStoppedStream` (15+), `FailedApplicationConnectionInterrupted`, `FailedApplicationConnectionInvalid` | the restart of §8.10 |
| `MissingEntitlements` | `IM_ERR_PERMISSION_DENIED` (never expected: the picker needs none) |
| `InvalidParameter`, `InternalError`, `FailedToStart`, others | at start `IM_ERR_INTERNAL` with the code; mid-share one restart (§8.10) |

### 8.10 Sleep, lock, display sleep, restart

- The supervisor observes `NSWorkspaceWillSleepNotification` / `DidWake`, `NSWorkspaceScreensDidSleep` / `DidWake`
  and the distributed `com.apple.screenIsLocked` / `com.apple.screenIsUnlocked` (registered on the main queue,
  forwarded as atomic flags). It sends `CAPTURE_INTERRUPTED{sleep|display_sleep|locked}` and, after wake or unlock
  plus the next Complete frame, `CAPTURE_RESUMED`. The share is not paused: no frames arrive, the pacer refreshes the
  last frame (M2 04 §8.5), audio follows 03.
- The engine clock stops during system sleep (`CLOCK_UPTIME_RAW`), and so do capture and audio timestamps; Go's
  `Session.Now` reads the same clock, so SR mapping stays consistent (01 anchors SRs from `Now` and wall time).
- **Restart**: when SCK stops the stream for a recoverable reason (§8.9) or no Complete or Idle frame arrived for 3 s
  after a wake, the supervisor sends `CAPTURE_INTERRUPTED{system_stopped}`, releases the old `SCStream`, creates a new
  one with the same filter and configuration and starts it (bounded 2 s). Success → `CAPTURE_RESUMED` and the layers
  get an IDR; failure → `ENDED{IM_ERR_SOURCE_GONE, capture_restart_failed}`. At most 3 restarts per 10 minutes. M-SLEEP
  (§17.5) verifies that a picker filter can be restarted without a new pick on 27 (system sleep on bare metal), plus
  26 if the owner adds 05's optional second volume; guests can't sleep, and suspending a VM neither sleeps the guest
  nor makes SCK send its system stop (05 §3.2), so VMs check only lock/unlock and `pmset displaysleepnow`. The restart
  path on older versions is covered by fault injection in `session_fake_test`. Where it is unverified, or an OS
  refuses the restart, the fallback applies: the share ends with `capture_restart_failed` and 01 offers "Share again"
  (results recorded in this section).

## 9. Encode (VideoToolbox)

### 9.1 One encoder kind, two modes

`IM_ENC_VIDEOTOOLBOX`, token `videotoolbox`. Modes:

| Mode | Encoder specification | `EncoderID` read back | Sizes |
|---|---|---|---|
| **LLRC** (preferred) | `kVTVideoEncoderSpecification_EnableLowLatencyRateControl: YES`, `kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder: YES` | `com.apple.videotoolbox.videoencoder.h264.rtvc` | every size (budget §9.3) |
| **Classic** | `RequireHardwareAcceleratedVideoEncoder: YES` | `…ave.avc` (or another hardware id on Intel) | ≤ 2560×1440×60 |

Selection: the full probe tries LLRC first (High, 1920×1080); success with `EncoderID` ending `.rtvc` sets
`IM_OS_VT_LOW_LATENCY`, and sessions use LLRC. Otherwise sessions use classic. A session whose LLRC creation fails
at `im_start` (never seen on Apple silicon, S2-vt) falls back to classic for that session and logs it. In
`IM_TEST_HOOKS` builds, start flag `0x40000000` (`IM_START_TEST_VT_SOFTWARE`, header-external; Go's
`WithSoftwareVT()` option) drops `RequireHardware…`, so CI VMs exercise the VT code path with Apple's software
encoder at small sizes; a release build refuses the bit.

### 9.2 Session creation and properties

`VTCompressionSessionCreate(NULL, w, h, kCMVideoCodecType_H264, spec, srcAttrs, NULL, OutputCallback, layerCtx,
&session)` with `srcAttrs = {kCVPixelBufferPixelFormatTypeKey: '420v', Width: w, Height: h,
kCVPixelBufferIOSurfacePropertiesKey: {}}`. Every property's `OSStatus` is logged at debug; a failure of a property
marked **required** fails the open (`open_failed`); others are best effort.

| Property | Value | Required |
|---|---|---|
| `kVTCompressionPropertyKey_ProfileLevel` | High: `kVTProfileLevel_H264_High_AutoLevel`; CB: `kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel` (12.0+) | yes |
| `kVTCompressionPropertyKey_RealTime` | LLRC: `YES`; classic: `NO` (S2-vt finding 2) | yes |
| `kVTCompressionPropertyKey_AllowFrameReordering` | `NO` (WebRTC: no B frames) | yes |
| `kVTCompressionPropertyKey_ExpectedFrameRate` | the layer's fps | no |
| `kVTCompressionPropertyKey_AverageBitRate` | the layer's bps | yes |
| `kVTCompressionPropertyKey_DataRateLimits` | `[bps / 8 × k, 1.0]` with k = 1.5 (Auto, Motion), 2.0 (Detail) | no (not readable on `rtvc`, S2-vt) |
| `kVTCompressionPropertyKey_MaxKeyFrameInterval`, `…IntervalDuration` | `0` (no limit): the engine forces IDRs (§9.4) | no |
| `kVTCompressionPropertyKey_MaxFrameDelayCount` | `0` | no |
| `kVTCompressionPropertyKey_H264EntropyMode` | High: `kVTH264EntropyMode_CABAC`; CB: `…CAVLC` | no |
| `kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality` | classic only: `YES` (`rtvc` rejects it with −12900) | no |
| `kVTCompressionPropertyKey_ColorPrimaries`, `TransferFunction`, `YCbCrMatrix` | `ITU_R_709_2` each | no |
| `kVTCompressionPropertyKey_AllowTemporalCompression` | `YES` | no |

Then `VTCompressionSessionPrepareToEncodeFrames`, and `VTSessionCopyProperty(kVTCompressionPropertyKey_EncoderID)`
→ the stats' `codec`. Content hint (`IM_CONTENT_*`) changes only `DataRateLimits`' k and `ExpectedFrameRate`
(Text 30 comes through `im_update_layer`); the session is still re-opened, as the header says, so behavior matches
Windows (preset changes are rare; the re-open is make-before-break).

### 9.3 Two layers, the downscale and the budget

- Layer `f` encodes SCK's buffer as is (`IM_ENCF_ZERO_COPY`). Layer `q` runs `VTPixelTransferSessionTransferImage`
  from that buffer into its pool at its own size (the scale rule with `q`'s `max_pixels` 230 400, e.g. 640×360),
  `kVTPixelTransferPropertyKey_ScalingMode = kVTScalingMode_Normal`, on `q`'s worker (1.1–2.4 ms, S2-vt finding 5).
  Each layer has its own VT session; both share the hardware encoder.
- **Pixel-rate budget** (`video/budget.h`), checked at `im_start` and by `im_update_layer`, on layer `f`'s output
  size × fps (after the scale rule):

  | Mode | One active video layer | Two layers |
  |---|---|---|
  | LLRC | ≤ 3840×2160×60 = 497 664 000 px/s | `f` ≤ 3840×2160×30 = 248 832 000 px/s |
  | Classic | ≤ 2560×1440×60 = 221 184 000 px/s | the same |

  Over the two-layer limit with `q` present → `IM_ERR_NO_ENCODER`, `layer_failed: q` (M2 04 §16's contract; 01 then
  offers 4K60 alone or 4K30 with preview). Over the one-layer limit → `layer_failed: f`. A `q` started inactive
  (`IM_LAYER_INACTIVE`) still counts: it can be activated any time. `im_update_layer` over budget → `IM_ERR_NO_ENCODER`
  synchronously, nothing changes. 1440p60 + preview (221 Mpx/s, S2-vt's gate) and 1080p60 + preview fit everywhere.

### 9.4 GOP, IDR, SPS/PPS, Annex B

- GOP and IDR policy are `native/common`'s pacer, as on Windows: time-based (`f` 4 s, `q` 2 s) only when the content
  changed; forced IDRs on `im_request_keyframe` (coalesced, ≤ 1 per 250 ms per layer), ring-full recovery, layer
  reactivation, stale drops and every (re)open. A forced IDR is `kVTEncodeFrameOptionKey_ForceKeyFrame: YES` in the
  frame properties; S2-vt confirmed it yields an IDR on that very frame with LLRC (finding 8).
- **Output callback** (`OutputCallback(layerCtx, sourceFrameRefcon, status, flags, sample)`): `status != noErr` →
  §9.9. `kVTEncodeInfo_FrameDropped` → counted, the pacer gets `RequireKeyframe` if the dropped frame was an IDR.
  Else: `CMSampleBufferGetFormatDescription` → `CMVideoFormatDescriptionGetH264ParameterSetAtIndex` (count, NAL
  length size); sync = no `kCMSampleAttachmentKey_NotSync`; `CMBlockBufferCopyDataBytes` into the layer scratch;
  `im::h264::AvccToAnnexB(…, prepend = sync)` puts SPS and PPS in front of a sync sample; then `native/common`'s
  normalizer (4-byte start codes, drops AUD/SEI/filler, cached SPS/PPS before every IDR, the constraint-byte rewrite:
  `00` for High, `e0` for CB, M2 04 §9.4), and the ring push with `capture_ts_ns` from the sample's PTS (§9.8).
  `KEYFRAME` is the normalizer's IDR-slice check; VT's sync flag is only cross-checked (a mismatch is a warn log).
- The output callback runs on VT's thread: no allocation (scratch preallocated), no lock, logs only through the ring.

### 9.5 Rate control and re-opens

- **Bitrate** (`im_set_bitrate`): live. The layer worker sets `AverageBitRate` and `DataRateLimits` with
  `VTSessionSetProperty` before its next encode; no IDR. `IM_ENCF_LIVE_BITRATE` is set unless the probe's check
  (§9.10 step 4) saw an IDR follow a change, in which case the layer re-opens instead (at most once per 10 s, M2 04
  §9.5's rule). 01's hysteresis (15 %, 2 s) is unchanged.
- **Re-open** (size after the resize debounce, fps or size from `im_update_layer`, profile, content hint): the worker
  creates the new session first (§9.2), encodes the next frame with it as `REINIT|KEYFRAME`, then completes and
  invalidates the old one on a background queue. When the re-open changes `f`'s **input** size (a source resize, or an
  `im_update_layer` size change, both of which need SCK's `updateConfiguration:`), §8.6's resize sequence applies: the
  new session exists before `updateConfiguration:` is called, and the worker routes each buffer to the session whose
  size matches `CVPixelBufferGetWidth/Height`, so the "next frame" is the first buffer at the new size (S2-vt finding 6:
  the new session's 40–120 ms first-frame hitch delays only that frame). Re-opens are coalesced and spaced ≥ 1 s per
  layer (M2 04 §6.4); the pacer gets `Reopen`.
- `im_set_layer_active(l, 0)`: no encodes; reactivation starts with an IDR (`KEYFRAME`, no `REINIT`).

### 9.6 Profiles

All layers carry one profile (M2 04 §9.6). High = `6400` (VT emits constraint byte `00`, S2-vt finding 7), CB = `42e0`
(normalizer rewrite). VT does both in both modes (the committed `native/common/tests/testdata/aus/videotoolbox-cb/`
streams came from VT, M2 04 §14); the probe confirms CB per mode, and a CB-less mode would make `im_set_profile(CB)`
leave the session at High and return `IM_OK` (packets carry the truth), as on Windows. `im_set_profile` re-opens both
layers make-before-break.

### 9.7 Static screens, refresh, pause

M2 04 §8.5 unchanged, through the shared pacer: on a static screen the last frame of each layer (the retained
`CVPixelBuffer`) is re-encoded as an IDR on a keyframe request and as a refresh 200 ms after the last change and then
every 1 s; no periodic IDR while static. Pause freezes the frame (the video thread takes no new slot), refreshes and
IDRs use the frozen buffer, `Mixer::SetPaused` mutes audio (03), resume encodes the newest frame at once.

### 9.8 Timestamps

`capture_ts_ns` (engine clock, mach ns) is stamped by the pacer (`Encode::ts_ns`: the frame's capture time, or now for
refreshes, static IDRs and frames older than 500 ms) and passed to VT as the PTS `CMTimeMake(ts_ns, 1000000000)` with
duration `1e9 / fps`; the output callback reads it back from `CMSampleBufferGetPresentationTimeStamp`. PTS strictly
increase per layer (the pacer's 1 µs bump). Go's packetizer and RTCP SR (01) use these values unchanged.

### 9.9 Failures and recovery

| Failure | Action |
|---|---|
| `VTCompressionSessionCreate` fails at `im_start` | LLRC → classic once; classic fails → `IM_ERR_NO_ENCODER` `open_failed` (`layer_failed: <rid>` for a layer) |
| `EncodeFrame` or the callback returns an error (`kVTVideoEncoderMalfunctionErr`, `kVTInvalidSessionErr`, …) | re-create that layer's session (same mode) and continue with `REINIT|KEYFRAME`; no `ENCODER_CHANGED` |
| A second failure of a layer within 10 s | `ENDED{IM_ERR_ENCODER_LOST, device_removed}` (the media service is gone; "Share again") |
| Encoder can't keep up | the pacer's `LIMIT_CHANGED gpu` (hardware) after 3 s below 90 %, `none` after 10 s above 95 % |

Fault injection (test hooks) covers each row (§12).

### 9.10 The probe

- **Quick** (< 100 ms): OS version and flags (`IM_OS_SUPPORTED` = 14.4+, `IM_OS_SCK_PICKER`,
  `IM_OS_SCK_PICK_IDENTITY` = 15.2+, `IM_OS_SCK_SELF_EXCLUDE` from §8.8's table, `IM_OS_SCREEN_RECORDING` from
  `CGPreflightScreenCaptureAccess`, `IM_OS_TRANSLATED` from `sysctl.proc_translated`), the adapter, the cached
  encoder entry of the last full probe (or one with `status` 0 and no `AVAILABLE` flag before the first), and 03's
  `audio_flags`.
- **Full** (< 2 s), on the worker:
  1. LLRC session at 1920×1080 High; encode two black NV12 frames from a pool buffer, the first forced IDR; require an
     Annex B AU with SPS, PPS and an IDR slice within 1 s (`test_encode_failed`); read `EncoderID`.
  2. If LLRC fails: the same in classic mode. No hardware encoder at all → `status IM_ERR_NO_ENCODER`, reason
     `no_hardware`. An open slower than 1 s → `slow_open`.
  3. CB in the chosen mode (one IDR) → `IM_ENCF_CB`.
  4. Live-bitrate check (M2 04 §9.5): change `AverageBitRate` before frame 3 and require frames 3–4 to be non-IDR;
     an IDR clears `IM_ENCF_LIVE_BITRATE`.
- `im_caps`: `os_build` = major × 10000 + minor × 100 + patch (14.4.1 → 140401; 27.0 → 270000), `os_ubr` 0;
  `adapter_count` 1: `name` = `MTLCreateSystemDefaultDevice().name` ("Apple M5"), `vendor_id` 0x106B (Apple) on
  Apple silicon or the GPU's PCI vendor on Intel (`IORegistry`), `luid` = `registryID`, flags `HAS_OUTPUTS`;
  `encoder_count` 1 (test-hooks builds: `fake` first, then `videotoolbox`, as Windows' catalog orders them):
  `kind VIDEOTOOLBOX`, `adapter_index 0`, flags `AVAILABLE|HARDWARE|LIVE_BITRATE|ZERO_COPY|HIGH|CB|FORCED_IDR` as
  measured, `max_width 4096`, `max_height 2304`, `codec` = the `EncoderID`'s last two components, `reason` "" or the
  token.

Example (`isshoni-enginetest probe -json`, abbreviated):
```json
{"build":{"engine":"0.3.0","abi":65537,"os":"macos","arch":"arm64","testHooks":false,"sdk":"26.2","minOS":"14.4"},
 "os":{"name":"macos","version":"27.0.0","build":270000,"supported":true},
 "capture":{"picker":true,"borderless":true,"desktopDuplication":false,"excludeFromCapture":true,"maxFPS":120},
 "encoders":[{"name":"videotoolbox","adapter":0,"hw":true,"tested":true,"liveBitrate":true,
  "profiles":["6400","42e0"],"zeroCopy":true,"status":"","reason":"","codec":"h264.rtvc"}],
 "osFlags":["supported","sck_picker","sck_pick_identity","sck_self_exclude","vt_low_latency"],"probeMs":412}
```

## 10. Audio in the engine (the boundary with 03)

03 builds the tap, the mixer, the watcher, mic detection, the report, the sound check and the render test exports
under `native/macos/src/audio/`; this doc owns the exports' argument checks, the session wiring, the Opus stage, the
PCM tap and the test tone, exactly as M2 04 §10 split it with M2 05.

- **The boundary** is M2 04 §10.2's interfaces, unchanged: `im::audio::PcmSink` (`native/common`), and `Mixer`,
  `MixerConfig`, `AudioEngine` with the same virtual methods (`Init`, `Shutdown`, `ProbeFlags`, `ListAppsJson`,
  `SetRules`, `Watch`, `Unwatch`, `SetMode`, `SetMuted`, `LevelsGet`, `SoundCheck`, `NewMixer`), declared for macOS in
  `native/macos/src/audio/audio_engine.h` (03's file; the declarations are copied verbatim from Windows' `mixer.h`) and
  reached through `audio_bridge::Engine()` (02; `SetEngineForTest` for a fake; both written by EM3 together with the
  export forwarding, so 03's slices reach their code through the export layer before EM10). The render test exports are
  **free functions**, as M2 built them (`native/windows/src/audio/test_exports.h`, called directly by `exports.cpp`):
  `exports.mm` calls `im::audio::TestRenderTone(const char *endpoint_id, float hz, float amp, int32_t ms)` and
  `im::audio::TestRenderBursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms, int32_t period_ms,
  int32_t count, int64_t *onset_ns, int32_t cap)`, declared with exactly the Windows signatures in
  `native/macos/src/audio/test_exports.h` and implemented by 03's `test_render.mm` (03 §5). `AudioEngine` gains no
  method.
- **Session wiring** (`session.mm`): `audio.mode` EXCLUDE_APPS → `SetRules` with the start's exclusions, `NewMixer`,
  `Start({mode, flags, sink})` with the session's Opus stage (and PCM tap) as sink, `SetPaused` on pause, `State()` as
  `im_session_info.audio_state`, `Stop` in `im_stop`. A failure is logged (token only, M2 04 §5.6's privacy rule) and
  the share runs without audio packets. `TEST_TONE` uses the tone thread (§7.1). `OFF` makes no mixer. An audio-only
  session (`source_id NULL`, `layer_count 0`) is valid (03's audiotest).
- **Clock**: 03 stamps `PcmSink::OnChunk`'s `captureNs` on the engine clock: the HAL's `mHostTime` (mach ticks)
  through `im::os::MachTicksToNs` (§7.7), the same function SCK display times go through, so audio and video share
  one conversion. That is what A/V sync rests on.
- **Exports**: `im_list_audio_apps`, `im_audio_watch`/`unwatch`, `im_set_exclusions`, `im_audio_set_mode`/`muted`,
  `im_audio_levels_get`, `im_audio_sound_check`, `im_test_render_*` forward after M2's checks (`EVERYTHING` →
  `IM_ERR_INVALID_ARG`). `im_test_opus_decode` is `native/common`'s.
- `im_exclusions.shared_pid` is the pick's `pid` (0 before 15.2, §8.2.1); 01 passes it as on Windows.

## 11. The Go binding on darwin

### 11.1 Files

§6.4's table, plus these edits of shared files (Windows behavior unchanged, §11.6):
- `bridge.go`: `media.Info.Name` from the build info's `os` (`"windows"` → `"windows"`, `"macos"` → `"darwin"`);
  `decodeCaps(c, os)` maps by OS (§11.4); `Pick` maps `media.PickRequest.Kinds` to `IM_PICK_*` (ignored by Windows);
  the source mapping fills `Capture` `"sck"` and `SelfVisible`; `EventLayer` gets `SelfVisible` from
  `IM_SESS_SELF_VISIBLE`; new `(*Engine).UnprotectWindow(window uintptr) error` (`IM_PROTECT_OFF`), beside the
  existing `ProtectWindow`; both are for `isshoni-enginetest protect-check` and tests only (§8.8).
- `abi.go` / `errors.go`: v1.1 constants and tokens (§5.1), including the Go mirrors of 03's audio constants in
  `abiConstants`. `types.go`: `BuildInfo.SDK`, `BuildInfo.MinOS`,
  `SourceDetail.BundleID`.
- `luma.go`: unchanged (the macOS luma tap uses the same bit and payload, §12).
- New portable `startTestVTSoftware` / `WithSoftwareVT()` and `pickTestFake` beside `luma.go`'s constant, each with a
  test tying it to the engine's value.

### 11.2 The cgo caller

```go
//go:build darwin && cgo && imstatic

package native

/*
#cgo CFLAGS: -I${SRCDIR}/../../../native/include -DIM_STATIC
#cgo LDFLAGS: -framework ScreenCaptureKit -framework VideoToolbox -framework CoreMedia -framework CoreVideo
#cgo LDFLAGS: -framework CoreGraphics -framework IOSurface -framework AppKit -framework Foundation
#cgo LDFLAGS: -framework CoreFoundation -framework CoreAudio -framework AudioToolbox -framework Metal -lc++
#include <stdint.h>
uint64_t im_go_call(uint32_t proc, const uint64_t *args, uint32_t n, uint32_t fallible,
                    char *err, int32_t errcap, int32_t *errlen);
uint32_t im_go_proc_count(void);
const char *im_go_proc_name(uint32_t proc);
*/
import "C"

// staticCaller is the caller over the statically linked engine (02 §11.2): one cgo call per export. The dispatcher
// reads im_last_error inside the same C call, on the same OS thread, so no LockOSThread is needed.
type staticCaller struct{}

func (staticCaller) call(p proc, args ...uintptr) (uintptr, string) {
	var a [8]C.uint64_t // the widest export, im_test_render_bursts, has 8 arguments
	for i, v := range args {
		a[i] = C.uint64_t(v)
	}
	var buf [lastErrorCap]C.char
	var n C.int32_t
	r := C.im_go_call(C.uint32_t(p), &a[0], C.uint32_t(len(args)), boolU32(p.fallible()), &buf[0], lastErrorCap, &n)
	if n <= 0 {
		return uintptr(r), ""
	}
	return uintptr(r), C.GoStringN(&buf[0], C.int(min(int(n), lastErrorCap-1)))
}

func (staticCaller) qpcNS() int64 { return machNS() } // the engine clock: mach ns on macOS (§11.3)
```
`call_darwin.c` holds `im_go_call`: a `switch (proc)` over the same order as `bridge.go`'s `proc` constants, each case
casting `args[i]` to the export's parameter types (pointers from `uintptr_t`, `im_session` as `uint64_t`, `uint32_t`
and `int32_t` truncations, floats from the low 32 bits as `bridge.go` packs them with `math.Float32bits`), the result
widened to `uint64_t` (`void` exports return 0), and, when `fallible` and `(int32_t)r < 0`, `im_last_error(err,
errcap)` into `*errlen`. `im_go_proc_count` and `im_go_proc_name` let `TestStaticProcTable` check that the C order
equals `procNames` and `procCount`. A `lastErrorCap` move into a shared file (it is in `call_windows.go` today) keeps
one constant.

Pointers: arguments are addresses of memory the binding pinned with `runtime.Pinner` for the call, nested buffers
included (`bridge.go` and `audio.go` already pin `exclusions`, rules and strings), passed as integers; the engine
keeps no pointer after a call (M2 04 §5.1). That meets cgo's pinning rule. `im_packet.data` stays a `uintptr` in the
mirror and is copied before the next call (M2 04 §12.4).

### 11.3 The clock

```go
// #include <time.h>
// static inline int64_t im_go_mach_ns(void) { return (int64_t)clock_gettime_nsec_np(CLOCK_UPTIME_RAW); }
func machNS() int64 { return int64(C.im_go_mach_ns()) }
```
Same value as the engine's `SteadyClock` and SCK's display times converted (§8.5). `openEngine`'s clock cross-check
(M2 04 §12.3: `mono_ns` within `[before − 1 ms, after + 1 ms]`) runs unchanged.

### 11.4 `Open`, `Info`, `Probe`, `Pick` on darwin

- `Open(o Options)`: `checkVersion(C.im_version())`, then `openEngine(staticCaller{}, o)`. `Dir`, `FFmpegDir` and
  `OpenH264Path` are ignored (a non-empty `OpenH264Path` is `invalid`). There is nothing to load, so no `missing` or
  `blocked_by_policy`. `Open` logs `staticArchiveSHA`.
- `Info()` = `{"darwin", 1, im_build_info().engine}`.
- `decodeCaps` for `os == "macos"`: `OS = {Name: "macos", Version: "<major>.<minor>.<patch>" unpacked from
  os_build, Build: os_build, Supported: IM_OS_SUPPORTED}`; `Capture = {Picker: IM_OS_SCK_PICKER, Borderless: true,
  DesktopDuplication: false, ExcludeFromCapture: IM_OS_SCK_SELF_EXCLUDE, MaxFPS: max_capture_fps}` (§5.1: 120 when an
  active display refreshes at ≥ 120 Hz, else 60; 0 is read as 60); encoders as on Windows; `Audio` from `audio_flags`
  (03 defines the bits). For `os == "windows"` (or a 1.0 DLL) the M2 mapping runs unchanged and `max_capture_fps` is
  ignored.
- `Pick(ctx, r)`: `Kinds` nil or both → flags 0; `[screen]` → `IM_PICK_DISPLAY`; `[window]` → `IM_PICK_WINDOW`.
  `OwnerWindow` is passed as `parent_window` and ignored by the engine. The result's `Capture` is `"sck"`, `OSBorder`
  false, `Exe` = the base name of `proc_pidpath(PID)` (`""` when `PID` is 0), `SelfVisible` from
  `IM_SRCF_SELF_VISIBLE`; `SourceDetail{Window: CGWindowID, BundleID: NSRunningApplication's bundleIdentifier of PID}`
  (`session_darwin.go`, a small cgo helper; `""` when unknown). `IM_SRCF_SELF` → `invalid`, `self_window`, as on
  Windows.
- `ProtectWindow(nsWindow uintptr)` / `UnprotectWindow`: `im_protect_window(w, 0 | IM_PROTECT_OFF)`; enginetest
  and tests only. The app's windows are protected by its OS layer (01), not through the engine.
- `SetOpenH264Path`, `InstallOpenH264` and friends stay portable Go; on macOS 01 never calls them, and the engine
  answers a path with `unsupported_os` `platform`.

### 11.5 Media contract additions (shared `internal/media`)

All additive; Windows and the fake keep their M2 values unless configured.
```go
// media.go
type Source struct {
	// … M2 fields …
	// SelfVisible: isshoni's own windows will appear in this capture (macOS display picks the engine can't filter,
	// 02-macos-engine §8.8): the app minimizes itself and mutes its viewer while the share runs. Always false on
	// Windows (WDA_EXCLUDEFROMCAPTURE) and for window picks.
	SelfVisible bool
}

type Event struct {
	// … M2 fields …
	// SelfVisible, for EventLayer: the running session may show isshoni's windows (IM_SESS_SELF_VISIBLE). It can be
	// true when the pick's Source.SelfVisible was false (a refused filter at start, 02 §8.8).
	SelfVisible bool
}
```
`internal/media/fake`: `EngineConfig.SelfVisible bool` sets both fields for display picks and every `EventLayer`
(01's tests drive the auto-minimize path with it). `media.Source.Capture`'s comment lists `"sck"`. No window
protection interface is added: the app's OS layer owns `sharingType` (§8.8), on the fake engine as on the real one.

### 11.6 How Windows stays unchanged

- Every shared edit is keyed on the build info's `os` or on new constants Windows never sets. The M2 fake-caller
  tests (`bridge_test.go`, `abi_test.go`, `errors_test.go`, `layout_test.go`) keep their Windows expectations
  byte-for-byte; new tests add the macOS cases (`TestDecodeCapsMacOS`, `TestPickKindsFlags`, `TestSelfVisible*`).
- CI proof: `windows-go` (all Go tests on Windows), `windows-native` (ctest and `TestDLL*` with the 1.1 DLL),
  `windows-desktop` (the app and installer smoke), `lint-go`'s `GOOS=windows` pass.

## 12. Test hooks on macOS

`IM_TEST_HOOKS` (presets `mac-arm64-ci`, `mac-x64-ci`, `mac-arm64-asan`, `mac-arm64-tsan`; never shipped):
- **`IM_ENC_FAKE`** through the macOS adapter of `im::testenc::FakeH264` (§7.7): deterministic AUs with M1's Marker,
  no VT. The test-hooks catalog lists `fake` before `videotoolbox`; `IM_ENC_AUTO` on a `test:` source picks `fake`,
  on other sources `videotoolbox` (M2 04 §6.1's WF10 rule).
- **Fake picker** (§8.2.2) and a **fake capture source** (`capture_source.h`'s interface with a scripted
  implementation: frame statuses, content-size changes, minimize, stream errors, silence after wake), so the session's
  §8.5–§8.10 logic runs without SCK.
- **Fault injection**: VT create failure (LLRC only, both modes), encode error once / twice within 10 s, frame drop,
  SCK start error by code, `didStopWithError:` by code, restart failure, main-queue starvation (`no_run_loop`).
- **Luma tap** (`IM_START_TEST_LUMA_TAP`, bit 0x80000000 of `im_start_config.flags`, M2 04 §5.8's payload and
  ring): the video thread computes the centered box's mean from the NV12 **Y plane** of the frame it takes
  (`CVPixelBufferLockBaseAddress(…, kCVPixelBufferLock_ReadOnly)`; video-range `Y` mapped to full range,
  `(Y − 16) × 65535 / 219`, clamped) — no GPU staging needed. Go's `WithLumaTap` is unchanged.
- **Software VT** (§9.1) for CI VMs.
A release build refuses the test-hooks bits (`IM_PICK_TEST_FAKE`, `IM_START_TEST_VT_SOFTWARE`,
`IM_START_TEST_LUMA_TAP`) with `IM_ERR_INVALID_ARG`, and `im_build_info().testHooks` is false. The shipped audiotest
bits `IM_AUDIOF_TEST_*` (03 §4.1, M2 05 §4.1) are accepted: the release-signed Exit Harness's audiotest and avsync's
`IM_AUDIOF_TEST_NO_EXCLUSIONS` (§13.2) rely on them.

## 13. `isshoni-enginetest` on macOS

### 13.1 Two binaries, the Dev Harness and the run loop

**Two outputs from one package** (`cmd/isshoni-enginetest`), as 05 §4.3 expects:
- `isshoni-enginetest`: `-tags imstatic` (the release archive). Commands and flags that need a test hook (`avsync`,
  `-encoder fake`, `-software-vt`, `protect-check`'s luma judge) exit `exitEnv` (2) with
  "needs isshoni-enginetest-hooks".
- `isshoni-enginetest-hooks`: `-tags imstatic,imtesthooks` (the `-ci` archive; `testHooks` true).
- `-dll` on darwin is a usage error (`exitUsage`, 64). Exit codes stay M2's: 0 pass, 1 `exitFail`, 2 `exitEnv`, 64
  `exitUsage`.

**Where it runs.** macOS attributes TCC grants and the picker to an app, not to a binary started from a terminal
(S2-audio). This doc adds **no harness app of its own**: both binaries live in `Contents/MacOS` of 05's
`isshoni Dev Harness.app` (`io.isshoni.dev.harness`, built and signed by `task mac:harness`, 05 §4.1) and, for rc
runs, `isshoni Exit Harness.app`. They are started with `harness.sh isshoni-enginetest[-hooks] <command> [flags]`,
which carries the exit status through the launcher's `-status` file. The harness is signed with the hardened runtime
(`--options runtime`) and exactly the app's entitlement `com.apple.security.device.audio-input` (04 §4.2's
`isshoni.entitlements`; 04 D4: the tap's aggregate device under the hardened runtime needs it), with
`NSAudioCaptureUsageDescription` in its Info.plist, so `-audio mix` and `avsync` open 03's tap with the same signing
shape the app's grants are tested with. The owner grants Screen Recording and System Audio Recording once, to the
harness, for every tool (enginetest, audiotest, g2g, the `isshoni_hw` test binaries).

**Self-identity under the harness.** The launcher `posix_spawn`s the tool from the bundle's `Contents/MacOS`, so the
tool's `NSBundle.mainBundle` resolves to the harness bundle: `excludedBundleIDs` (§8.2) and the own-app filter
(§8.3, §8.8 B) name `io.isshoni.dev.harness`, which covers the tool's own windows (protect-check's noise window).

**Run loop.** On darwin, `main_darwin.go` locks the main goroutine to the main thread in `init`, runs the command on
another goroutine and calls `C.enginetest_run_app()` (`runloop_darwin.m`: `NSApplication.sharedApplication`,
activation policy Accessory, `[NSApp run]`); the command's end calls `[NSApp stop:]` and exits with its code. This
stays inside the tool binary, so it applies when the launcher spawns it. Commands that need no run loop and no grant
(`probe`, `sessions`, `capture -source test:pattern`) also work as a plain CLI from a terminal.

### 13.2 Commands

Size and layer flags (new; `capture`, `bench`, `sessions`): `-size 1080p|1440p|2160p` sets layer `f`'s `max_pixels`
(2 073 600 / 3 686 400 / 8 294 400), `-fps 30|60` its fps, `-layers 1|2` (2 = with layer `q`). They override
`-preset`'s values. Over-budget combinations return `IM_ERR_NO_ENCODER` `layer_failed: q` (§9.3), which `bench`
reports as the expected refusal (`"refused":"layer_failed: q"`, exit 0) for 2160p60 with 2 layers. 05 §4.3 uses
these exact names.

| Command | Binary | macOS behavior | Needs | Asserts |
|---|---|---|---|---|
| `probe [-quick]` | both | §9.10; writes `probe.json` | nothing | full probe < 2 s; the `videotoolbox` entry has a status; `Info().Name` `darwin`, `BuildInfo().OS` `macos` |
| `sources` | both | `im_list_sources` | nothing | — |
| `pick [-kinds window,screen]` | both | the picker, in the harness | the harness | — (manual M-PICK-MAC) |
| `capture -source pick\|mon:<id>\|test:pattern` | both (`-encoder fake`, `-software-vt`: hooks) | as M2 (`-seconds`, `-preset`, `-encoder auto\|videotoolbox\|fake`, `-profile`, `-audio off\|tone\|mix`, `-keyframes`, `-bitrate-steps`, `-dump-aus`, `-out`) plus `-size`/`-fps`/`-layers`; `-software-vt` (hooks) | `mon:`: Screen Recording; `pick`: the harness; `-audio mix`: System Audio Recording (03) | M2's assertions (first keyframe ≤ 500 ms, SPS/PPS before every IDR, no IDR after a bitrate step, profile key). With `-software-vt` the ≤ 500 ms first-keyframe assertion is skipped (Apple's software encoder takes ~870 ms, S2-vt finding 2) and the report says `"firstKeyframe":"skipped (software VT)"` |
| `bench` | both | `capture` without files, `-seconds 30`, same flags | as `capture` | prints encode and capture-to-packet percentiles and process CPU (`getrusage`) |
| `sessions -count N` | both | N `test:pattern` sessions at 1080p60 + preview through `StartExtra` (≤ 4) | nothing | every session started; each layer's `codec` reported |
| `soak -hours H` | both | one session; samples every 60 s: `phys_footprint` (`task_info TASK_VM_INFO`), thread count (`task_threads`), open descriptors (`proc_pidinfo PROC_PIDLISTFDS`), IOSurface count (`capture.held` from stats) (`soak_darwin.go`, cgo) | as the source | M2's growth ≤ 5 % after 10 min; no `RING_OVERFLOW`; no `ENDED` |
| `protect-check [-sck listed\|pick\|off] [-encoder auto\|videotoolbox\|fake] [-seconds S]` | both (luma judge: hooks) | creates a borderless `NSWindow` in the tool's process over the luma tap's centered box, calls `ProtectWindow` (the engine's `im_protect_window`, §8.8), captures the display through `mon:` (`listed`, §8.3's filter) and/or a display pick (`pick`, §8.8's strategy for this OS), unprotects half-way. Two judges: **bitrate** (release binary, hardware VT): a noise window (contents change 60 times a second), layer `f`'s non-IDR slice bytes as M2 (`protectMinShownBps` 500 kbit/s, `protectMaxRatio` 20 %); **luma** (hooks binary, used with `-encoder fake` and whenever the probe has no hardware encoder, i.e. VMs): a solid white box (flashing white/black at 2 Hz) on a black desktop, the luma tap's mean of the centered box — protected = mean within 5 % of the background's, unprotected = mean ≥ 50 % of full scale. The release binary in a VM exits 2 ("needs isshoni-enginetest-hooks") | `listed`: Screen Recording; `pick`: the harness | outcome token (05 §4.3.2): `pass-filter` (A or B kept the window out), `pass-sharingtype` (14.x, C), `fallback-needed` (the pick said `self_visible`; exit 2 when a display was picked), `fail` (the window is in the frames; exit 1); `-json` names the outcome, the strategy, the judge and the OS version, no window or display names |
| `avsync [-source pick\|mon:<id>] [-encoder auto\|videotoolbox\|fake] [-seconds 25]` | hooks | 05 §4.3.1's design, adopted here: the flash and click run in **helper processes**, as M2's `-role flash\|click` helpers parented to `explorer.exe`, so the exclude-apps share treats them as foreign apps. The helpers are copies of `isshoni-enginetest-hooks` in the harness's `Resources/avsync/AVSync Flash.app` (`io.isshoni.test.avsync.flash`) and `AVSync Click.app` (`io.isshoni.test.avsync.click`), run with M2's `-role flash\|click` and launched with `NSWorkspace openApplicationAtURL:configuration:completionHandler:` (`createsNewApplicationInstance` YES), so launchd is their parent and each is its own responsible process. Flash: a borderless `NSWindow` with a `CAMetalLayer`, white for 100 ms once a second, each white frame's `P_k` from `presentedTime` (host seconds → mach ns). Click: the click helper calls `im_test_render_bursts` (03) **in its own process** and reports `A_k`. The helpers return M2's result-file payload over the tool's Unix socket. The luma tap and the PCM tap give `U_k`; pairs as M2 05 §19.3 (`avsync_darwin.go`, `avsync_darwin.m`). The session's audio uses `Config.Audio.TestFlags \|= IM_AUDIOF_TEST_NO_EXCLUSIONS` (03's shipped bit; needs 03 §14.3's `audio.go` `TestFlags` change), so the clicks are captured wherever the click helper runs: 03's implicit INSTANCE rule on the engine pid and APP_EXCLUDE on the main bundle id would otherwise drop them | System Audio Recording; `mon:`: Screen Recording | M2's verdict and `avsync.json` with `"encoder"` and 05's `"os"` block |
| `openh264 …` | both | exit 2: "OpenH264 is Windows-only" | — | — |

`raw_darwin.go` (`darwin && cgo && imstatic`) calls `im_probe` directly through the tool's own cgo preamble for
`probe -raw` (the symbols come from the archive the binding links). `startup_darwin.go`: nothing to do (no DLL search
path on macOS). The `*_other.go` files' constraints become `!windows && !(darwin && cgo && imstatic)`.

### 13.3 `hwcheck.sh`

`native/macos/tools/hwcheck.sh [-out DIR]` (bash, `set -euo pipefail`), the macOS twin of `hwcheck.ps1`: runs every
step through 05's `harness.sh` (Dev Harness) with `isshoni-enginetest`: `probe`, `capture -source test:pattern
-encoder videotoolbox` (5 s, `-keyframes 500ms -dump-aus 12`), a 25 s `-bitrate-steps 8M,2M,8M` run, `capture
-source mon:<main>`, and `bench` at `-size 1080p -fps 60 -layers 2`, `-size 1440p -fps 60 -layers 2`, `-size 2160p
-fps 30 -layers 2` and `-size 2160p -fps 60 -layers 1` (each on `mon:<main>` when the display is large enough, else
`test:pattern`; plus `-size 2160p -fps 60 -layers 2`, which must report the `layer_failed: q` refusal), `sessions
-count 4`, `protect-check -sck listed`; then redacts user, host, volume and path names (`<redacted:…>` tokens and the
leftover check of M2) and zips the bundle. Needs the Dev Harness with Screen Recording; never run by CI.
`task mac:hw:engine` (05) runs it with the native hw gtests and the `isshoni_hw` Go tests.


## 14. Component latency (for 05's glass-to-glass run)

05 runs `tools/g2g` (M2 01 §12.2) on macOS; this doc supplies the component checks reported beside it:
- stats `latencyMs` (capture → packet in the ring) per layer, polled at 1 Hz by 01;
- `enginetest bench` at 1080p60 with a display source on Apple silicon: capture-to-packet p95 ≤ 25 ms, layer `f`
  encode p99 ≤ 16 ms (S2-vt: 1080p60 LLRC p99 6.8 ms one layer, 13.2 ms with the preview); at 1440p60 + preview
  encode p99 ≤ 16 ms (S2-vt: 9.3–12.1 ms).
The engine's share of the budget: capture after composition ≤ 17 ms (one refresh), pacer 0 for a 60 Hz source,
encode ≤ 8 ms p95 at 1080p60 LLRC, ring → Go ≤ 1 ms.

## 15. Room for later milestones

| Later | Room made now |
|---|---|
| Change content mid-share (menu bar) | `allowsChangingSelectedContent` + `contentSharingPicker:didUpdateWithFilter:forStream:` with a stream → `updateContentFilter:` and `SOURCE_RESIZED`; needs a protocol message for the new kind/name |
| Application picks | `IM_PICK_*` gains an application bit; `IM_SOURCE_*` would need an application kind |
| HDR / 10-bit / HEVC | `captureDynamicRange`, `'x420'`, `kCMVideoCodecType_HEVC`; `im_start_config.reserved` |
| > 60 fps presets | `max_capture_fps` already reported; SCK at 120 Hz works with the half-interval rule |
| Temporal layers instead of a separate preview | VT's `BaseLayerFrameRateFraction` (S2-vt minor note) |
| Device-loss recovery (M5) | §9.9's re-create already rebuilds a layer; a full pipeline restart would join §8.10's restart |
| Developer ID (pending) | nothing in the engine changes; TCC grants reset once (04) |

## 16. Risks and open questions

| Risk | Mitigation |
|---|---|
| A display pick can't exclude isshoni on some versions (strategies A/B/C fail) | `SELF_VISIBLE` → 01's auto-minimize and mute (plan's fallback); EM9 records which strategy works per OS |
| SCK refuses a filter rebuilt from a picker display without Screen Recording, or shows a Screen Recording prompt for it | §8.8's retry with the picker's own filter inside `im_start`, then `IM_SESS_SELF_VISIBLE`; a prompt disqualifies B on that OS (§8.8) |
| Before 15.2 a pick has no identity (name, pid) | Generic names in 01; 03 handles `shared_pid` 0 |
| A picker stream can't be restarted after sleep on some version | End with `capture_restart_failed` ("Share again"); M-SLEEP records it on 27 and on the 26 volume; older versions rely on fault injection and the fallback |
| No hardware VT and no TCC on CI | `IM_ENC_FAKE` and software VT in test-hooks builds; real capture and encode checks run on the owner's Mac (§17.3) |
| VMs (for 14.4/15/26) have no hardware encoder | Functional checks in VMs with the hooks binary's `-encoder fake` (capture, protect-check's luma judge, avsync); `-software-vt` only for stream-structure tests (`vt_encoder_test`, `TestStaticSoftwareVT`), never latency; performance only on real hardware (§17.4) |
| Go's build cache reuses a link with a stale archive | §6.6's `-X staticArchiveSHA` and `-count=1` |
| Intel Macs untested on hardware (LLRC availability, classic budget) | CI builds x86_64 and runs its unit tests under Rosetta nightly; the probe picks classic mode where LLRC fails; question 1 |
| `active = NO` may not close an open picker panel on cancel | late callbacks are ignored by pick id; EM8 records the behavior |
| Wails beta's main thread blocked by the app's own code | The 2 s `no_run_loop` bound turns a hang into an error |

**Open questions for the owner**
1. **Intel Macs**: the universal app keeps an x86_64 slice (plan), but no Intel hardware is available to verify it.
   Ship M3 with Intel marked "best effort, untested on hardware" (CI build + Rosetta unit tests), or find an Intel
   Mac on 14.4–26 for one hardware run before the M3 exit?
2. **Older macOS versions**: answered by 05 V-D2/V-D3 (signed off at MA01): latency gates on bare metal 26 (the
   M5's second volume) and 27; 14.4 and 15 run the functional checks (picker, self-exclusion, sleep restart, grants)
   in VMs with L-F.

## 17. Test plan

### 17.1 Native unit tests (`native/macos/tests`, `native/common/tests`)

Run on the Mac (`task native:mac PRESETS=mac-arm64-ci`) and in CI (`macos-native`, `mac-arm64-ci`; nightly asan, tsan,
x86_64). No TCC grant is needed by any of them. Every native skip uses `IM_REQUIRE(...)` from 05's
`native/macos/tests/support/macenv.h` (e.g. `IM_REQUIRE("hw:h264")`), whose message `tools/skipcheck` parses, and has
a line in `.github/macos-skips.txt`.

| Test | Asserts |
|---|---|
| `clock_test` (common) | `MachTicksToNs(mach_absolute_time())` equals `SteadyClock::NowNs` within 1 µs; `MachTicksToNs` exact on known tick/timebase pairs (incl. the 125/3 Apple-silicon and 1/1 Intel timebases) and without overflow at 2⁶³ ticks; monotonic over 10⁶ reads |
| `avcc_test` (common) + `avcc_fuzz` | length sizes 1/2/4; SPS/PPS prepended only for sync samples; lying lengths, zero-length NALs and truncation rejected without out-of-bounds; output then passes the normalizer idempotently |
| `exports_test` | every export's argument checks; `struct_size` rule; N/A answers (§5.2); `im_version` 0x00010001; `im_abi_layout` equals the golden file; three `im_init`/`im_shutdown` cycles; `im_last_error` per thread |
| `os_info_test` | version packing (`os_build`), flags per version through an injected version, strategy table lookup (§8.8) |
| `budget_test` | §9.3's table: 1080p60, 1440p60, 4K30 + `q` pass; 4K60 + `q` → `layer_failed: q`; 4K60 alone in LLRC pass, in classic `layer_failed: f`; inactive `q` counts |
| `frame_slots_test` | retain/release balance with a `CVPixelBufferPool` of 8; both layers' mailboxes get the same buffer reference; with a fake VT holding 2 async encodes, `q` lagging one frame and the sample queue publishing, never more than `queueDepth − 2` = 6 pool buffers held (§7.4's table); latest wins |
| `frame_slots_soak` (test hooks; Mac and CI on the fake capture source) | two layers at 1080p60 from a 120 Hz source for 60 s (fake encoder with 7–15 ms async completion): `capture.held` ≤ 6 at every sample and no gap between consecutive delivered frames above 2 intervals |
| `frame_info_test` | `FrameSourceSize` (§8.5) on fake attachment dictionaries: a scaled display (3024×1964 into 1786×1160, `ContentScale` < 1), a scaled window and an unscaled window each give the source size within ±2 px; missing keys or `ContentScale` 0 → no size; fed through the fake capture source's resize detector, a static source of each kind raises no `SOURCE_RESIZED` over 600 frames |
| `pick_tokens_test` | M2's token rules (single use, failed start keeps, 5 min, 4 max, `pick_expired`), late callbacks ignored by id, one pick at a time |
| `picker_logic_test` | flags → modes and style; cancel and timeout; `no_run_loop` with a starved fake main queue; a call on the main thread returns `IM_ERR_BAD_STATE` `main_thread` at once with no pick pending afterwards, while an `IM_PICK_TEST_FAKE` call on the main thread succeeds; strategy C's observation (all visible windows `NSWindowSharingNone` → not self-visible; one `ReadOnly` window → `IM_SRCF_SELF_VISIBLE`) |
| `sck_errors_test` | §8.9's table for every `SCStreamError` code |
| `vt_encoder_test` | real VT: hardware if present, else software (`IM_START_TEST_VT_SOFTWARE`'s path); 320×180 and 1280×720: first AU is IDR with SPS/PPS; `ForceKeyFrame` → IDR on that frame; `AverageBitRate` change → no IDR (hardware) ; CB → `42e0`, High → `6400`; PTS round trip; re-open make-before-break emits `REINIT|KEYFRAME` once; asserts stream structure only, no latency; `IM_REQUIRE("hw:h264")` gates the hardware cases, and the whole test skips through `macenv.h` when VT creates no session at all |
| `pixel_transfer_test` | 1920×1080 → 640×360 NV12: the pattern's mean luma per band within 2 of the CPU model |
| `nv12_pattern_test` | the CPU pattern equals `im::testpattern::PatternPixel` converted to BT.709 limited range, exactly |
| `session_fake_test` | the whole session on `test:pattern` + `IM_ENC_FAKE` + the fake capture source: first keyframe ≤ 500 ms (static included), pacer rates at 30/60/120 Hz sources, pause/resume, inactive layers, stale drops, events (resize with 1 s debounce, minimize/restore, gone, interrupted/resumed, restart ok and failed, `os_stopped`), the budget refusal, 4 sessions and `session_limit`, `im_stop` ≤ 2 s with a blocked consumer released within 50 ms, controls after `ENDED`, fault injection of §9.9 and §8.10's restart (ok and failed, the path older OS versions rely on); a mid-stream resize of the fake capture source (§8.6's sequence, buffers at the old size still in flight after the switch): no encode error, new sessions created before the fake's `updateConfiguration`, no buffer reaches a session of another size, exactly one `REINIT` per layer; stats `audio.state` `"permission"` and `"tap-failed"` with a fake `Mixer` returning 5 and 6 (§5.3) |
| `session_luma_tap_test` | readings on the pattern grid with the CPU model's mean; no debug packets without the bit; release refusal |
| `audio_bridge_test` | with a fake `AudioEngine`: forwarding, exclusions passed at start, `SetPaused`, audio-only sessions, the PCM tap, the tone thread's 10 ms grid |
| `log_privacy_test` | no record at any level carries a window title, app name, bundle id or display name (pick and start with named fakes) |
| `rt_alloc_test` | the SCK sample handler path, the VT output callback path and the layer mailbox make no allocation after warm-up (global `operator new` counter, as M2's `opus_stage_rt_test`) |

### 17.2 Go tests

| Where | Tags | What |
|---|---|---|
| every OS (ubuntu `test-go`, Mac, windows-go, desktop-macos) | none | M2's fake-caller suite unchanged + `TestDecodeCapsMacOS`, `TestInfoNameFromBuild`, `TestPickKindsFlags`, `TestSelfVisibleSource`, `TestSelfVisibleEvent`, `TestUnprotectWindow`, `TestBuildInfoMacOSKeys`, `TestTestBitsMatch` (`pickTestFake`, `startTestVTSoftware` vs the C headers), `TestHeaderConstants` with 03's mirrors, the fake's `SelfVisible` |
| CI `macos-native`, Mac | `imstatic,imtesthooks` + §6.6's `-X staticArchiveSHA`, after `task native:mac` | `TestStaticProcTable`, `TestStaticLayout` (Go mirror vs `im_abi_layout`), `TestStaticClock` (the `Open` cross-check, `machNS` vs `im_clock`), `TestStaticConformance` (`mediatest.Conformance` on `test:pattern` + `fake`), `TestStaticNativeIntoPublisher` (01's publisher over the static engine), `TestStaticSession` (Stop, Next after ENDED, controls, stats decode), `TestStaticStartErrors` (listed id without `Listed`, unknown `test:` id, budget `layer_failed`), `TestStaticFakePick` (Pick → Start → token consumed), `TestStaticLumaTap`, `TestStaticSoftwareVT` (one 320×180 VT session, stream structure only; when VT creates no session at all it skips with a `macenv: needs …` message (05 §5.1) and a ledger line), `TestStaticAudioTone` (Opus packets on the 20 ms grid with `media.AudioTestTone`) |
| Mac only (owner), through `task mac:hw:engine` in the Dev Harness | `imstatic,isshoni_hw` | `TestHWListedDisplay` (`mon:<main>` 3 s: frames, size, `capture.path` `sck`, `codec` `h264.rtvc` on Apple silicon), `TestHWVideoToolbox` (hardware LLRC at 1080p60 and 1440p60: encode p99 ≤ 16 ms over 600 frames), `TestHWProtectListed` (the protect-check logic in-process) |
| `cmd/isshoni-enginetest` | none and `imstatic,imtesthooks` | the tool's portable tests (flag parsing incl. `-size`/`-fps`/`-layers`, `-dll` → 64 on darwin, hooks-only flags → 2 in the release build, redaction, verdicts, protect-check outcome tokens for both judges); with tags `TestRunCaptureStatic` on `test:pattern`, `TestRunSessionsStatic`, `TestRunAVSyncFake` |

Tests needing a grant never run in CI. Each `isshoni_hw` test calls `macenv.Require(t, macenv.ScreenRecording,
macenv.GUISession)` (plus `macenv.HardwareEncoder` where it encodes) from 05's `internal/testenv/macenv`, so a skip
carries the `macenv: needs …` message `tools/skipcheck` parses, and a grant declared in `ISSHONI_TEST_TCC` but
missing FAILs (05 V-D6). They run only as `go test -c` binaries inside the signed Dev Harness (`task mac:hw:engine`,
05), never from Terminal (05 V-D5), so the grant belongs to the harness, not to a shell.

### 17.3 Grants per test (owner's Mac)

| Test | Process | Grant |
|---|---|---|
| native ctest, Go tests with `imstatic,imtesthooks`, `probe`, `sessions`, `capture -source test:pattern` | any (terminal or Dev Harness) | none |
| `isshoni_hw` Go tests, native hw gtests (`task mac:hw:engine`) | Dev Harness | Screen Recording (once) |
| `sources`, `capture`/`bench`/`soak`/`protect-check -sck listed` with `mon:` | Dev Harness | Screen Recording (once; kept across rebuilds by the dev identity) |
| `pick`, `capture -source pick`, `protect-check -sck pick`, `avsync -source pick` (video) | Dev Harness | none (the pick is the consent) |
| `capture -audio mix`, `avsync` (PCM tap of the real mix) | Dev Harness | System Audio Recording (03) |

### 17.4 OS versions and machines

| Machine | Runs |
|---|---|
| Owner's M5, macOS 27 (hardware) | everything in §17.1–§17.3, `hwcheck.sh`, §17.5's manual checks, 05's g2g |
| macOS 14.4, 15.x (15.0 or 15.1 and 15.2+), 26 in 05's VM lab on the M5 (Virtualization framework) | `probe`, `pick` (M-PICK-MAC), `capture -source pick -encoder fake` (hooks binary) for the functional check, `protect-check -sck pick -encoder fake` (luma judge; M-SELF-EXCLUDE, 05 C-1), `avsync -encoder fake` (M-AVSYNC-MAC), M-SLEEP's lock/unlock and `pmset displaysleepnow` only, the grants check of 04 — functional only, no latency (no hardware encoder in VMs: `no_hardware`, and the release build has no software encoder) |
| An Intel Mac (if found, question 1) | `probe`, `bench` at 1080p60 and 1440p60, M-PICK-MAC, M-SELF-EXCLUDE |
| CI `macos-native` (`tools.json` `runner`) | §17.1 and §17.2's CI rows; x86_64 link; nightly asan/tsan and Rosetta |

### 17.5 Manual checks (recorded in 05's exit record)

| ID | Check | Where |
|---|---|---|
| M-PICK-MAC | Picker: window first; `-kinds screen` shows displays; cancel → `cancelled`; timeout and `im_cancel_pick` close or orphan the panel (record which); isshoni windows never offered; pick fields per §8.2.1 | 27 (hardware) + 14.4, 15.x, 26 (VMs) |
| M-SCK-WINDOW | Window share: resize (letterbox then re-init after 1 s, exactly one `REINIT` per layer per settled resize), minimize/restore events, other Space, full-screen app, close → `source_gone`, child menus visible | 27 |
| M-SCK-DISPLAY | Retina scaling (a 60 s share of the built-in display at the 1080p budget: `layers[].reinits` 0 after the first keyframe, no `SOURCE_RESIZED`), ProMotion 120 Hz (60 fps out, no drift; `capture.held` ≤ 6), mode change, second display unplugged → `source_gone` after 5 s, cursor on/off | 27 |
| M-SELF-EXCLUDE | `protect-check -sck pick` and `-sck listed` per OS (hardware: bitrate judge; VMs: `-encoder fake`, luma judge); which strategy works and whether B shows any TCC prompt (signed with 04's release-shaped requirement); fill §8.8's table and `IM_OS_SCK_SELF_EXCLUDE` | 14.4, 15.0/15.1, 15.2+, 26, 27 |
| M-SLEEP | Sleep/wake (bare metal only), lock/unlock, display sleep during a share: events, restart without a new pick or `capture_restart_failed` | system sleep: 27 (+ 26 if 05's optional volume exists); VMs: lock/unlock and `pmset displaysleepnow` only |
| M-OS-STOP | Stop from the menu bar → `ENDED cancelled os_stopped` | 27, 15.x |
| M-BENCH-MAC | `hwcheck.sh`: `-size 1080p -fps 60 -layers 2`, `-size 1440p -fps 60 -layers 2`, `-size 2160p -fps 30 -layers 2`, `-size 2160p -fps 60 -layers 1` (and the 2160p60 two-layer refusal); §14's thresholds | 27 |
| M-SOAK-MAC | `soak -hours 2` with `-audio tone` | 27 |
| M-AVSYNC-MAC | `harness.sh isshoni-enginetest-hooks avsync -source pick` (05 A-9) | 27 (VideoToolbox) + 14.4, 15, 26 VMs (`-encoder fake`) |


## 18. Interfaces other docs rely on

- **For 01 (desktop app)**: `native.Open(native.Options{Logger, LogLevel})` in builds with `-tags imstatic`;
  `imstatic` selects the real engine in any flavor (§6.4: `engine_fake.go` gains `&& !imstatic`, `engine_release.go`
  becomes `((!dev && !e2e) || imstatic)`), so `TAGS=dev,imstatic` is a dev bundle with the real engine and
  `TAGS=dev`/`e2e` keep the fake; `Info().Name == "darwin"`; `Probe` as §9.10 / §11.4 (`OS.Name "macos"`,
  `Capture.Picker`, `Capture.ExcludeFromCapture`, `Capture.MaxFPS`); `Pick` with `PickRequest.Kinds` (nil = window
  first), called from the share controller's own goroutine, never on the main thread and never through `OnMain`:
  on the main thread it fails at once with `IM_ERR_BAD_STATE` `main_thread` (§7.2); `media.Source` on macOS:
  `Capture "sck"`, `OSBorder false`, `Name`/`PID`/`Exe` empty before 15.2, `SelfVisible`; `EventLayer.SelfVisible`
  and `NoOSIndicator` always false. **Window protection stays the app's**: 01's `macos.Hooks.ExcludeFromCapture` /
  `IncludeInCapture` set `sharingType`; the engine never needs to be told about a window. On 14.x a display pick is
  reported not self-visible only if every visible isshoni window has `NSWindowSharingNone` at pick time (§8.8 C); a
  `hideFromCapture` change during a share takes effect at the next pick. The error and token table of §5.4 (incl.
  `ENDED cancelled os_stopped`, `capture_restart_failed`, `device_removed`, `layer_failed: q` for the 4K choice,
  `main_thread`); `CAPTURE_INTERRUPTED` causes of §5.3; stats `audio.state` `"permission"`/`"tap-failed"` (§5.3); no
  OpenH264 on macOS. 01 MD12's needs are EM8 (picker) and EM9 (`SelfVisible`).
- **For 03 (audio)**: §10's boundary (Windows' `AudioEngine`/`Mixer`/`PcmSink` declarations, unchanged; the render
  exports call the free functions `im::audio::TestRenderTone`/`TestRenderBursts` declared in
  `native/macos/src/audio/test_exports.h` with the Windows signatures); files under `native/macos/src/audio/`, built
  into `im_macos`; `im_tappolicy` linked into `im_macos` and merged into the archive (§6.2); tests under
  `native/macos/tests/` (the same CMake glob); the engine clock (mach ns, `im::os::SteadyClock`) and
  `im::os::MachTicksToNs` for `mHostTime` (§7.7); `im::mac::SetRealtime`; `audio_bridge::SetEngineForTest`;
  `shared_pid` 0 before 15.2; 03 §4.1's constants merged unconditionally in EM1 with their Go mirrors (§5.1); the
  audio call bounds of §5.2 (sound check ≤ 30 s + `timeout_ms` + 500 ms; other posted calls ≤ 3 s, then
  `IM_ERR_TIMEOUT` `engine_timeout`); the stats names for states 5 and 6 (§5.3); release builds accept
  `IM_AUDIOF_TEST_*` (§12); avsync's use of `IM_AUDIOF_TEST_NO_EXCLUSIONS` (§13.2); `IM_INIT_NO_AUDIO`.
- **For 04 (release)**: presets `mac-arm64-release` and `mac-x64-release` (§6.1); `task native:mac PRESETS=…
  VERSION=<v> COMMIT=<sha>` (§6.5), run by `desktop:app:macos` only when `TAGS` includes `production` or `imstatic`;
  per arch in `native/dist/darwin-{arm64,amd64}/`: `libisshoni_media.a`, `archive.sha256`, `build.json` (M2's
  native-manifest shape with `version`, `commit`, `archive{name,sha256,abi}`, `vcpkg{baseline,ports}`, §6.2; 04 ships
  it as `native-manifest-darwin-<arch>.json` in the symbols zip) and `licenses/vcpkg/<port>/copyright` (04's
  `-native-root` is `native/dist/darwin-<arch>/`); Go tags `production,imstatic`; env `CGO_ENABLED=1
  MACOSX_DEPLOYMENT_TARGET=14.4` (+ `CC="clang -arch x86_64" GOARCH=amd64` for Intel); `-X …native.staticArchiveSHA`;
  `lipo -create` of the two app binaries; §6.6's checks (`machocheck -thin`: `_im_start` and `staticArchiveSHA` ==
  `archive.sha256`; `--self-check`: `engine.loaded`, `abi` 1, `testHooks` false, no hardware or TCC needed); the
  engine needs **no** entitlement and no Info.plist key for video (the picker needs no TCC); `LSMinimumSystemVersion`
  14.4; the native notices: libopus (BSD-3-Clause) and speexdsp (BSD-3-Clause) linked statically, Apple system
  frameworks not redistributed; `testHooks` false in a release; the `macos-native` job on `tools.json`'s `runner` with
  04 §3's Xcode-select step.
- **For 05 (exit)**: build tags `imstatic`, `imtesthooks` and `imstatic,isshoni_hw` (§6.4); the two binaries
  `isshoni-enginetest` (`-tags imstatic`) and `isshoni-enginetest-hooks` (`-tags imstatic,imtesthooks`), for the
  Dev and Exit Harness `Contents/MacOS`; enginetest commands and flags (§13.2: `-size 1080p|1440p|2160p`, `-fps
  30|60`, `-layers 1|2`, `-encoder auto|videotoolbox|fake`, `-software-vt`) and exit codes 0/1/2/64 (hooks-only flags
  → 2 "needs isshoni-enginetest-hooks" in the release binary; `-dll` → 64); `IM_START_ALLOW_LISTED_SOURCE` with
  `mon:<id>`; the luma tap; protect-check's outcome tokens `pass-filter`, `pass-sharingtype`, `fallback-needed`,
  `fail` and its two judges; avsync's helper bundles (`AVSync Flash.app` `io.isshoni.test.avsync.flash`, `AVSync
  Click.app` `io.isshoni.test.avsync.click`, copies of the hooks binary run with `-role flash|click`) and `-encoder`;
  `no_hardware` in VMs (no software encoder in release builds); presets `mac-arm64-ci`, `mac-arm64-asan`,
  `mac-arm64-tsan` with gtest XML (`GTEST_OUTPUT`, `ctest --output-junit`); `macos-native`'s `go test -json`
  output, the 05-owned steps and the `mac-test-bins` artifact (§6.7); `hwcheck.sh` (through `harness.sh`); §14's
  component numbers; §17.4–§17.5's checks and where each runs.

## 19. Depends on

- **01**: owns `sharingType` on every isshoni window through its OS layer (`macos.Hooks.ExcludeFromCapture` /
  `IncludeInCapture`), as M2's winos does on Windows; calls `Pick` from its own goroutine, never on the main thread
  (01 §10.1); runs AppKit's main run loop (Wails) and never blocks it for more than 2 s; implements the
  `SelfVisible` fallback (auto-minimize, mute the in-app viewer) and the macOS strings for §5.4; says in its settings
  text that a `hideFromCapture` change applies from the next share; offers the 4K choice on `layer_failed: q`.
- **03**: implements `AudioEngine` on macOS (tap, mixer, watcher, report, levels, sound check) and the free
  functions `TestRenderTone`/`TestRenderBursts` in `test_render.mm` with `test_exports.h` (03 §5); its `audio_flags`
  bits and the §4.1 constants with the `im_rule` comment block (EM1); the portable `im_tappolicy` library
  (`native/common/tappolicy`), which §6.2 links and merges; stamps chunks with `im::os::MachTicksToNs` (03 §7.3,
  §13.5); exempts `im_audio_sound_check` from its 3 s rule (03 §4.3) and updates `audio.go`'s `SoundCheck` comment
  (03 §14.3); accepts `IM_AUDIOF_TEST_NO_EXCLUSIONS` in `audio.go`'s `TestFlags` check (03 §14.3; avsync needs it);
  cites §5.3's `audio.state` names in 03 §12.1.
- **04**: `deploy/macos/dev-signing.sh` (the stable dev identity, from S2-audio) and `sign.sh`, used by 05's harness;
  `cmd/isshoni-desktop/build/darwin/isshoni.entitlements` (`com.apple.security.device.audio-input`), which the
  harness reuses; `deploy/macos/tools.json` (`runner`, `xcode`, `sdkMin`) and the Xcode-select and Rosetta steps
  (04 §3); `task desktop:app:macos` (04 §4.6) calling `native:mac` with `VERSION`/`COMMIT`; `machocheck -thin` and
  `--self-check` (§6.6); the dev row `TAGS=dev,imstatic` (04 §4.8); the release workflow runs §6.1's release
  presets; the notices file from `licenses/vcpkg`; the `desktop-macos` job (04 §9.1); the `nightly.yml` steps of
  §6.7.
- **05**: `internal/testenv/macenv` (`Require`, `ISSHONI_TEST_TCC`) and `native/macos/tests/support/macenv.h`
  (`IM_REQUIRE`); the Dev Harness, `task mac:harness`, `harness.sh` and `task mac:hw:engine` (the replacement for a
  harness app of this doc's own; EM8 needs 05 V3); the avsync helper bundles in the harness's `Resources/avsync/`;
  `.github/macos-skips.txt` and `tools/skipcheck`; the `macos-compat` job that consumes `mac-test-bins`; the VM lab with
  14.4, 15.0/15.1, 15.2+ and 26 guests (and the required 26 bare-metal volume, 05 V-D2, for M-SLEEP and the latency
  gate); the g2g run and the OS-matrix record that §17.4–§17.5 feed.
- **M2 as built**: `native/common` (rings, pump, pacer, normalizer, Opus stage, test tone, log ring, handles),
  `internal/media/native`'s `caller` design and tests (incl. `TestHeaderConstants`), `bridge.go`'s
  `ProtectWindow`, `native/windows/src/audio/test_exports.h`'s signatures, 01's media contract and
  `mediatest.Conformance`, `tools/g2g`, enginetest's `-role flash|click` helpers and exit codes.
- **External**: GitHub's macOS image named by `tools.json` (today `macos-26`, Xcode 26, SDK ≥ 26.0), `macos-15` for
  `native-host` and fake-engine dev/e2e builds, `macos-14` for 05's `macos-compat`; vcpkg at `native/vcpkg.json`'s
  baseline; Go 1.27 (darwin/amd64 and arm64).


## 20. Implementation slices

Ordered; each is testable on its own. "Mac" = the owner's M5 on macOS 27; "VMs" = macOS 14.4/15/26 guests on it;
"CI" = GitHub Actions.

| # | Slice | Scope | Acceptance | Where | Needs |
|---|---|---|---|---|---|
| EM1 | ABI v1.1 | §5.1's constants, `max_capture_fps`, tokens; `abi.go`/`errors.go` mirrors; `abi_layout.json` (`abi` 65537, the field name); all of 03 §4.1's audio constants with 03 §4.2's `im_rule` comment block, and their Go mirrors in `abi.go`/`abiConstants` (`imAudioProcessTap`, `imAudioTapBundleIDs`, `imAudioResponsiblePID`, `imAudioTCCPreflight`, `imAudioStatePermission`, `imAudioStateFailed`, `imAudiofTestNoExclusions`, `imAudiofTestNoBundleIDs`); the `im_audio_sound_check` header comment (§5.2) | `abi_layout_test`, `abi_header_c_test`, Go `layout_test`/`abi_test` (incl. `TestHeaderConstants`)/`errors_test` on every OS; `windows-native` green with the 1.1 DLL | Mac, CI (all) | — |
| EM2 | Common additions and moves | `clock.cpp` Apple branch, `im::os::MachTicksToNs` + `clock_test`; `h264/avcc` + `avcc_test` + `avcc_fuzz`; move pattern layout, pattern grid, luma sample to `im/testpattern`; extract `im::testenc::FakeH264`; `encpolicy` traits row for `videotoolbox` | new tests pass on ubuntu/macos `native-host`; 60 s `avcc_fuzz` clean; Windows `session_fake_test`, `gpu_warp_test`, luma tests and Go `TestDLLConformance` unchanged and green | Mac, CI | EM1 |
| EM3 | Build skeleton and exports | `native/macos/CMakeLists.txt`, presets, triplets, `IM_BUILD_MACOS`, merged archive (incl. `im_tappolicy` once 03 adds it) with `archive.sha256`, `build.json` (native-manifest shape), `licenses/vcpkg/` and the `nm` check; `IM_VERSION`/`IM_COMMIT` from `native:mac`'s `VERSION`/`COMMIT`; `exports.mm` with every export (version, init/shutdown, build info, clock, last error, logs, layout real; the final forwarding of every `im_audio_*`, `im_list_audio_apps`, `im_set_exclusions` and `im_test_render_*` export after M2's checks, through `audio_bridge::Engine()`/`SetEngineForTest` (`src/audio_bridge/`, written here) to a stub `AudioEngine`; the rest `IM_ERR_NOT_IMPLEMENTED`); worker queue; `SetRealtime`; `task native:mac`; `macos-native` job steps 1–2 (on `tools.json`'s `runner`, 04's Xcode step, gtest XML) | `exports_test` (incl. each audio and render export reaching a fake set with `SetEngineForTest`); both archives build; `nm` check passes; `build.json` carries the passed version and commit and the vcpkg ports; job green | Mac, CI macos-26 | EM1; 04's `tools.json` |
| EM4 | Go darwin binding | §6.4's files and tags, `call_darwin.c` dispatcher, `machNS`, `Open`, OS-aware `bridge.go` (`Info`, `decodeCaps`, kinds, `SelfVisible`, `UnprotectWindow`), `types.go` fields, `engine_other.go` constraint, `cmd/isshoni-desktop`'s `engine_fake.go`/`engine_release.go` constraints (§6.4), `-X staticArchiveSHA`; `macos-native` steps 3–5 and 7 (`go test -json`, `mac-test-bins`) | `TestStaticProcTable`, `TestStaticLayout`, `TestStaticClock`, `TestDecodeCapsMacOS` etc.; `go build -tags production,imstatic ./cmd/isshoni-desktop` arm64 and amd64 link; `go build -tags dev,imstatic` links the engine and `-tags dev` the fake (`go list -deps`); M2 Go suites unchanged on ubuntu/windows; `windows-desktop` green (dev and production flavors unchanged) | Mac, CI | EM3 |
| EM5 | Session core on fakes | `session.mm`, video thread (RT), layer workers and mailbox, frame slots, supervisor, command queue, stats, events, pause, `test:pattern` CPU source on the pattern grid, `IM_ENC_FAKE` adapter, fake capture source, fake picker, luma tap, fault injection skeleton, test tone thread | `session_fake_test` (incl. the mid-stream resize case), `session_luma_tap_test`, `frame_slots_test`, `frame_slots_soak`, `nv12_pattern_test`, `rt_alloc_test`; Go (with `lockmain_darwin_test.go`) `TestStaticConformance`, `TestStaticNativeIntoPublisher`, `TestStaticSession`, `TestStaticStartErrors`, `TestStaticFakePick`, `TestStaticLumaTap`, `TestStaticAudioTone`; stats `audio.state` names for 5 and 6; TSan clean | Mac, CI | EM2, EM4 |
| EM6 | VideoToolbox | `vt_encoder.mm` (both modes, §9.2's properties, AVCC wiring, ForceKeyFrame, live bitrate, profiles, make-before-break re-open, §9.9's recovery), `pixel_transfer.mm`, `budget`, `vt_probe.mm` (quick/full, live-bitrate check), software-VT test switch; the two enginetest binaries; `probe`, `capture`, `bench`, `sessions` on `test:pattern` with `-size`/`-fps`/`-layers`, `-dll` → 64, hooks-only flags → 2 in the release binary | `vt_encoder_test`, `pixel_transfer_test`, `budget_test` (CI: software VT or an `IM_REQUIRE` skip with a ledger line; Mac: hardware); Mac: `capture -encoder videotoolbox` passes M2's stream assertions incl. `-bitrate-steps`; `bench -size 1080p -fps 60` on `test:pattern` encode p99 ≤ 16 ms; `bench -size 2160p -fps 60 -layers 2` reports the `layer_failed: q` refusal; `sessions -count 4` | Mac, CI | EM5 |
| EM7 | SCK listed capture and source changes | `sck_stream.mm`, `listed.mm`, `sources.mm`, frame statuses and timestamps, `window_watch`/`display_watch`/`power_watch`, `frame_info.mm`, resize debounce + §8.6's resize sequence (new sessions, then `updateConfiguration`, routing by buffer size), §8.9 error table, sleep/lock events and restart; enginetest `sources`, `capture -source mon:` | `sck_errors_test`, `frame_info_test` and the fake-source cases of `session_fake_test`; Mac: a 60 s `capture -source mon:<built-in>` share of the built-in Retina display at the 1080p budget shows `layers[].reinits` 0 after the first keyframe and no `SOURCE_RESIZED` (confirms §8.5's formula on real attachments; otherwise switch to the supervisor detector and record it); `frame_slots_soak`'s bound on the real ProMotion display (two layers 1080p60, 60 s: `capture.held` ≤ 6, no SCK gap > 2 intervals; the copy pool for "last" only if it fails); Mac (`imstatic,isshoni_hw`, `task mac:hw:engine` in the Dev Harness): `TestHWListedDisplay`, `TestHWVideoToolbox`; M-SCK-DISPLAY, M-SLEEP on 27 | Mac, CI | EM6; 05 V2 (`macenv`), V3 (harness) |
| EM8 | The picker | `picker.mm`, `pick_tokens`, §8.2.1's identity per version, kinds, cancel/timeout, main-queue rule incl. `main_thread`, `os_stopped`; window-source events; Go `Pick` mapping, `SourceDetail.BundleID`; the tool's run loop (`main_darwin.go`, `runloop_darwin.m`), `enginetest pick` run through `harness.sh` | `pick_tokens_test`, `picker_logic_test` (incl. the `main_thread` case); M-PICK-MAC on 27 and in the 14.4/15/26 VMs, recording on 14.4, 15.x and 27 whether a target window under a centered isshoni window can be clicked in `SCContentSharingPicker`'s window mode (01 §10.1's `PickPreparer` choice: nothing, miniaturize or order back); M-SCK-WINDOW and M-OS-STOP on 27; a pick goes live and is watched in a browser | Mac, VMs, CI | EM7; 05 V3 (Dev Harness, `harness.sh`) |
| EM9 | Own windows | `protect.mm` (enginetest/tests only), `UnprotectWindow`, `filter.mm` strategies A/B and C by observation, `IM_SRCF_SELF_VISIBLE`/`IM_SESS_SELF_VISIBLE`, the start-time retry; `media` additions (§11.5) and the fake's `SelfVisible`; enginetest `protect-check` (mac) with both judges and 05's outcome tokens | `os_info_test`'s table; `picker_logic_test`'s strategy-C cases; Go `TestSelfVisible*`; M-SELF-EXCLUDE on 14.4, 15.0/15.1, 15.2+, 26, 27 (VMs: `-encoder fake`, luma judge) with results, including whether B shows any TCC prompt, written into §8.8's table and `filter.mm`; a strategy that prompts counts as not working | Mac, VMs, CI | EM8 |
| EM10 | Audio bridge | session wiring of 03's `Mixer` in `session.mm` (the export forwarding and `audio_bridge::Engine()` are EM3's), audio-only sessions, PCM tap, the `session_limit` check | `audio_bridge_test` with the fake `AudioEngine` (CI); with 03's engine on the Mac: an audio-only session captures a fixture tone (PCM and decoded Opus) with a kept-out fixture absent, `capture -audio mix` gives Opus on the 20 ms grid, the PCM tap matches it (03's audiotest); a second audio session is `IM_ERR_LIMIT` `session_limit` | Mac, CI | EM5; 03's engine slices |
| EM11 | Tools on macOS | `soak_darwin.go`, `avsync_darwin.go`/`.m` (helpers via `NSWorkspace`, `-role flash\|click`, `-encoder`, `IM_AUDIOF_TEST_NO_EXCLUSIONS`), `raw_darwin.go`, constraints of `*_other.go`, `hwcheck.sh` (through `harness.sh`) with redaction | portable tool tests; Mac: `hwcheck.sh` bundle with no names or paths; `soak -hours 2`; `avsync -source pick` verdict; VM: `avsync -encoder fake` exit 0 | Mac, VMs, CI | EM8, EM9, EM10; 05 V3 (helper bundles in the harness); 03's `TestFlags` change |
| EM12 | Hardware and OS record | M-BENCH-MAC, M-SOAK-MAC on 27; M-AVSYNC-MAC on 27 and the 14.4/15/26 VMs (`-encoder fake`); VM checks on 14.4/15/26; Intel if found; §14's numbers beside 05's g2g | results committed to 05's record (no machine names); §8.8, §8.2 step 4 and §8.10 updated with what was measured | Mac, VMs | EM11 |
