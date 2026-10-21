# M2 design 04: the native engine (C ABI v1) and the Windows DLL

Status: draft for M2, 2026-10-21. `docs/PLAN.md` is the source of truth above this doc. Evidence:
`spikes/s1-win-audio` (README results, `native/engine.cpp`, `native/isshoni_audio.h`, `engine_windows.go`),
`spikes/s4-sfu/README.md` (codec findings), the M1 record (`docs/m1/README.md` §3 and §6, `docs/m1/02-sfu.md` §8,
§9.7, §9.8, §11 and §15.1) and the M1 code in `internal/media/fake`, `internal/client/publish` and `internal/protocol`.

Written in parallel with `01-client-core.md`, `02-device-auth.md`, `03-desktop-app.md`, `05-windows-audio.md` and
`06-windows-release.md`. Other docs are referenced by file name; every interface this doc assumes from them is stated
in §20, and every interface it provides is listed in §19. A bare `01` … `06` means the M2 doc of that number; M1
docs are written `M1 02 §8.6` or `02 §8.6 of M1`.

Tags: **M2** = build now. **later (M3/M4/M5)** = leave room now, build in that milestone.

## 1. Scope

In this doc (all M2 unless tagged):
- **`native/include/isshoni_media.h`**: the C ABI v1. The Windows DLL implements it in M2; the macOS engine (M3,
  cgo, static) and the Linux shim (M4, cgo) implement the same header.
- **`native/common/`**: portable C++20 parts with no OS headers outside a small `os/` shim: rings and packet memory,
  the H.264 Annex B normalizer and SPS parser, the pacer, the encoder policy (fallback order, live change versus
  re-init), the Opus stage and PCM tap (`common/audio/`), the session handle table, JSON and log helpers. Their unit
  tests run on the Mac and on every CI OS. 05's `common/audiopolicy/` and `common/audiomix/` live beside them.
- **`native/windows/`**: `isshoni-media.dll`. Capture (Windows.Graphics.Capture with `GraphicsCapturePicker`, DXGI
  Desktop Duplication), the D3D11 GPU path (copy, cursor, VideoProcessor BGRA → NV12 and the preview downscale),
  encode through libavcodec (NVENC, QSV, AMF, Media Foundation) and Cisco OpenH264 loaded at runtime, the Opus stage
  behind 05's mixer, window protection, the probe.
- **`internal/media/native`**: the Go binding (x/sys/windows, no cgo). It implements 01's `media.Engine` and
  `media.Session` contract; its binding logic (ABI mirror, status mapping, JSON decoding, packet copy, the session
  state machine) carries no build tag and is tested on every OS against a fake caller (§12).
- **`cmd/isshoni-enginetest`** (Go): the encoder/capture smoke, bench, session-limit and soak tool. Glass-to-glass
  latency is measured with 01's `tools/g2g` (01 §12.2); this doc supplies component numbers (§15).
- The requirements on the FFmpeg build (06 builds and publishes it) and the engine's view of the installed layout
  (06 owns it).

Not in this doc: the include-set mixer, exclusion rules, mic detection, the "What friends hear" JSON, the audio
exports' semantics and the audiotest (`05-windows-audio.md`); the Go client core, the publisher, the uplink
rate-control policy and its hysteresis (`01-client-core.md`); the Wails app, share sheet, "Sharing" pill, tray,
hotkeys, first-run wizard, settings and every UI string (`03-desktop-app.md`); device auth (`02-device-auth.md`); the
installer, updater, signing, release workflow and CI workflow files (`06-windows-release.md`).

Not in M2: the macOS and Linux engines (header only); the 720p30 middle layer (M5, rid `h` reserved); direct AMF/MF
bitrate control (M5); HDR, 10-bit, AV1, HEVC; presets above 60 fps; Windows on ARM; crash minidumps (later).

The engine has no Wails dependency. 03 pins Wails; the current v3 tag on 2026-10-21 is `v3.0.0-beta.34` (no GA yet).
The engine needs only the main window's `HWND`, which 03 passes in as the picker's owner (§7.2). 03 sets
`WDA_EXCLUDEFROMCAPTURE` on its own windows itself (§11).

## 2. Decisions at a glance

| Topic | Decision | Why |
|---|---|---|
| ABI shape | Pull-only. Process-wide `im_init`/`im_shutdown`, exclusions, audio watcher and probe; per-share **sessions** behind a 64-bit handle (index plus generation) | The protocol allows N shares per user (plan "Defaults"); a stale handle can never reach freed memory; M3/M4 engines keep the same shape |
| Calls | The plan's 11 calls plus the rest of §5.2, each with a named caller (§5.3): init/shutdown, build info, clock, picker (pick, cancel, release), session info, profile, content hint, layer active and update, pause, audio bitrate, stats, window protection, OpenH264 path, logs, layout; 05's audio watcher, mode, mute, levels and sound check; three shipped test exports | The plan's list has no way to show the OS picker, switch profiles (01 §8.10 of M1), follow layer pausing (02 §11 of M1), pause a share (plan "Share safety"), follow a preset change or show "What friends hear" before a share |
| Events | Delivered through `im_next_packet` as `kind = IM_PACKET_EVENT` | One pull loop per session; native code never calls Go, from any thread |
| Packet flags | The plan's `keyframe` and `reinit` are bits of `im_packet.flags`, next to `refresh` and `discontinuity` | One field to extend additively |
| Variable data | JSON (UTF-8, camelCase, snprintf-style return) for lists, status, stats and events; fixed structs for the hot path, the probe and the start config | S1's JSON outputs worked; hot-path structs avoid parsing per packet |
| Struct evolution | Every top-level struct starts with `struct_size`; a minor ABI bump only appends fields or functions. Structs embedded by value or used as array elements have no `struct_size` and are frozen in v1 | Old Go with new DLL and the reverse both keep working within major 1; a nested struct that grew would shift every later offset |
| Detail tokens | Every failure that a UI state depends on carries a fixed token (§5.7): at the start of `im_last_error`'s text, in `im_encoder_info.reason` and in the `ENDED` JSON | The UI maps codes plus tokens, never free text |
| Go loading | `x/sys/windows` `LoadLibraryEx` + `SyscallN`, no cgo, no purego | S1's loader ran on Win11 25H2 and Server 2025; `x/sys` is already a dependency |
| COM apartments | No API call does COM, WinRT or DXGI work on the caller's thread: one engine worker thread (MTA) runs it; the picker has its own STA thread | Goroutines hop OS threads, and the Wails UI thread is an STA |
| Capture paths | Win11: WGC for windows and monitors, borderless when the OS allows it. Win10 22H2: WGC for windows (yellow border), DXGI Desktop Duplication (DD) for monitors | Plan "Engines per OS" |
| Win10 monitor pick | The OS picker, then the picked item is mapped to a DXGI output (size, then name) and captured with DD; WGC with the border when that leaves several. A frame match is added only if E11 finds that names don't tell identical monitors apart (§7.5) | Keeps "OS pickers only" and avoids the yellow border without building a matcher that may never be reached |
| GPU path | One D3D11 device per session on the encoder's adapter; captured BGRA → our texture slot → cursor → VideoProcessor to NV12 per layer (BT.709 limited range); a pixel-shader converter as fallback and CI reference | Zero copy into hardware encoders; deterministic reference on WARP |
| Pacer | Event-driven on a nominal schedule per layer (`next_due += interval`), not fixed ticks | Adds no latency when the source runs at or below the target rate; a faster source (75–240 Hz) averages exactly the target, so CBR budgets hold |
| Encoders | libavcodec from the project's LGPL FFmpeg build, **loaded at runtime** (`LoadLibraryExW` + `GetProcAddress`). Order: the capture adapter's NVENC/QSV/AMF → MF hardware → other adapters → MF software → OpenH264 (user-enabled) | The engine loads, probes and captures audio even when FFmpeg is missing or blocked; the user can replace the LGPL DLLs (major-version check, no hash pin) |
| GOP | Time-based and engine-forced: `f` 4 s, `q` 2 s; no periodic IDR while the content hasn't changed since the last one | Frame-count GOPs stretch to minutes on a static screen; a static IDR every 4 s would waste about 0.6 Mbps |
| Output size | Fixed per encoder session. A resize letterboxes at once and re-inits after the size has been stable for 1 s | Drag-resizing a window would otherwise re-init dozens of times a second |
| Profiles | One H.264 profile for every layer of a session; the CB constraint byte is normalized to `e0` | Simulcast layers share one transceiver and payload type; the SFU's table keys are `6400` and `42e0` (02 §8.2 of M1) |
| Static screens | The last frame is re-encoded as an IDR on a keyframe request, and as a refresh 200 ms after the last change and then every 1 s | Plan "Keyframes"; the refresh lets CBR encoders converge on sharp text |
| Portable code | `native/common` in C++20 with GoogleTest on macOS, Linux and Windows | The owner develops on the Mac; M3 can reuse it from ObjC++ |
| Build root | CMake root, presets and vcpkg manifest at `native/` (not `native/windows/` as the plan's layout shows) | `common/` and the host test presets need one root; the Windows sources stay in `native/windows/` |
| Go package | `internal/media/native` instead of `internal/media/engine_windows.go`; its public surface is 01's `media.Engine`/`media.Session` contract plus 05's `media.AudioController`, errors are always `*media.Error` | The binding logic and its tests compile on the Mac and Linux CI; 01's `internal/media` stays free of OS code; the core and 03 see one engine type and one error type |
| Tools | `cmd/isshoni-enginetest` (Go) for probe, capture, bench, sessions and soak; glass-to-glass is 01's `tools/g2g` | One Go latency tool, cross-compiled from the Mac, that works with a single monitor (01 §12.2) |
| Test builds | `IM_TEST_HOOKS` (fake encoder, failure injection, luma tap) only in the `win-x64-ci` preset. Shipped exceptions: the GPU test pattern, the test tone, 05's test exports (`im_test_render_tone`, `im_test_render_bursts`, `im_test_opus_decode`) and the audio test flags `IM_AUDIOF_TAP_PCM` and `IM_AUDIOF_TEST_FAIL_PROBE` | M1's rule that test code doesn't ship; these capture nothing, the smoke tool needs the pattern and tone, and 05's gate must run on the release DLL itself (its report's `dll.sha256` equals the release candidate's) |
| CRT | Static CRT (`/MT`) in `isshoni-media.dll`; FFmpeg built with `-MT` too | No VC++ redistributable on users' PCs (S1 did the same) |

## 3. From the spikes and M1 to this design

| Evidence | What it means here |
|---|---|
| S1: include-set mixer PASSED on Win11 25H2 and Server 2025 (excluded tones −133 to −145 dB); the late-start click's fix (an onset fade) is not yet re-verified on Windows | 05 builds the mixer behind `im::audio::Mixer` (§10). The **Win10 22H2 run is still pending** on the owner's PC; the M2 exit criterion needs it (05) |
| S1 draft ABI v0: JSON outputs, thread-local `im_last_error`, `x/sys/windows` loader that locks the OS thread around a call and its error read | Kept. Changed: snprintf-style lengths instead of `IM_ERR_BUFFER_TOO_SMALL`; a positive `IM_TIMEOUT` instead of S1's −7; `LoadLibraryEx` with explicit search flags instead of `LazyDLL`'s default search. S1's −1, −4, −5, −6 and −99 keep their values |
| S1: static CRT, `/W4 /permissive- /utf-8 /EHsc` | Kept, plus `/WX` and the hardening flags of §13.1 |
| S1: capture timestamps in QPC nanoseconds | One clock for everything: WGC `SystemRelativeTime`, DD `LastPresentTime`, WASAPI QPC positions (05) are all QPC, converted with one formula (§12.3). Go reads the same QPC for `Session.Now`; `im_clock` only cross-checks it at `Open`. 01 anchors SRs from `Session.Now` (01 §8.2) |
| S4 finding 3: Firefox decodes only Constrained Baseline and Baseline | `im_set_profile` re-inits every layer with an IDR; CB output carries constraint byte `e0` |
| S4 finding 5, S2-VT finding 7: High is `6400`, WebRTC never uses B-frames | `max_b_frames = 0` is a required option for every encoder |
| M1 `fake.Packet {Kind, Layer, Keyframe, CaptureNS, Data}` mirrors the plan's ABI packet | Native packets map 1:1 to 01's `media.Packet` (§12.3), plus `Reinit` and `KindEvent` |
| M1 `publish.ProfileSource.SetProfile` (M1 02 §15.1) | 01's consumer endpoint calls `Session.Reconfigure{Profile}`, which the binding maps to `im_set_profile` |
| M1 layer pausing (02 §11, README S88): `f` inactive 10 s after the last `high` request, a PLI right after the resume hint | `im_set_layer_active`: an inactive layer is not encoded; the first frame after resume is an IDR |
| M1 encodings per preset (02 §8.6): `f` ≤ 8 Mbps, 60 fps (Text 30), `maxPixels` 2 073 600; `q` 0.3 Mbps, 15 fps, 230 400 | `im_layer_config` takes `max_pixels`, `fps` and `bitrate_bps` straight from `ShareParams.encodings` |
| M1 SFU throttles PLI to one per 500 ms per layer and sends none for a paused layer (M1 02 §9.7); it paces keyframe bursts per viewer (M1 02 §9.8) | The engine coalesces keyframe requests (at most one forced IDR per 250 ms per layer) and does not pace its output |
| M1 `stalled` is media-agnostic: it means the pub PC is down (M1 01 §4.4) | A static screen needs no keep-alive frames; the 1 s refresh exists for quality only |

## 4. Files and ownership

```
native/
├─ include/isshoni_media.h        C ABI v1 (§5)                                                    04 (audio block: 05)
├─ CMakeLists.txt  CMakePresets.json  vcpkg.json  vcpkg-configuration.json  .clang-format          04
├─ ffmpeg/pins.json  ffmpeg/lock.json   FFmpeg inputs and the published build (06 §4.1; CMake reads lock.json)  06
├─ common/                         portable C++20
│  ├─ include/im/{abi,ring,h264,pace,encpolicy,json,log,os}/…                                       04
│  ├─ src/…                                                                                          04
│  ├─ audio/                       PcmSink, the Opus stage, the PCM tap, the test tone (§10.3)       04
│  ├─ audiopolicy/                 rules, classification, planner; target im_audiopolicy             05
│  ├─ audiomix/                    rings, resampling, mixing; target im_audiomix                     05
│  └─ tests/  tests/fuzz/          GoogleTest; libFuzzer targets on clang hosts                      04 (05: its own)
└─ windows/
   ├─ src/api/                     exports.cpp (the C functions), guards.cpp (noexcept wrappers)      04
   ├─ src/engine/                  process state, worker thread, probe cache, handle table use,
   │                               delayload.cpp (the System32-only delay-load hook, §13.1)          04
   ├─ src/session/                 session state machine, video thread, supervisor                    04
   ├─ src/capture/                 picker, wgc, dd, monitor_map, window_match, sources, test_pattern  04
   ├─ src/gpu/                     device, slots, video_processor, shader_convert, cursor            04
   ├─ src/encode/                  ffmpeg_api, ffmpeg_encoder, openh264_encoder, selector, probe     04
   ├─ src/audio/                   the watcher, the mixer and everything WASAPI                        05
   ├─ src/audio_bridge/            session wiring between 05's AudioEngine/Mixer and common/audio     04
   ├─ src/window/                  protect.cpp (WDA_EXCLUDEFROMCAPTURE)                              04
   ├─ isshoni-media.def            the exports (`LIBRARY isshoni-media`): exactly the header's IM_API functions  04
   ├─ shaders/*.hlsl               compiled with fxc at build time into headers                      04
   ├─ resources/version.rc.in      VERSIONINFO                                                        04
   ├─ tests/                       GoogleTest: fakes, WARP, OpenH264, ABI                             04 (05 adds audio tests)
   ├─ tools/hwcheck.ps1            the owner's-PC hardware run (§14)                                  04
   └─ third_party/openh264/        codec_api.h, codec_app_def.h, codec_def.h, codec_ver.h and LICENSE
                                   (Cisco's BSD-2-Clause text at the pinned tag)                      04
internal/media/native/             Go binding (§12); audio_windows.go is 05's                        04
cmd/isshoni-enginetest/            Go tool (§14)                                                      04
```

## 5. The C ABI v1 (`native/include/isshoni_media.h`)

### 5.1 Conventions

- **Calling convention and linkage**: plain C, `extern "C"`, `IM_CALL` = `__cdecl` on Windows (x64 and arm64 have
  one convention, so it only documents intent). The DLL exports exactly the functions the header declares with
  `IM_API` (`isshoni-media.def`, no decoration; §13.1 says how that is checked).
- **Integers**: fixed-width only. Booleans are `uint32_t` 0/1. Enumerations are `#define`d constants on `uint32_t`
  fields, never C `enum` types, so their size never depends on the compiler.
- **`struct_size`**: the first field of every top-level struct. The caller sets it to `sizeof` of the struct it was
  built with. The engine reads input structs up to `min(struct_size, its own sizeof)` and treats missing fields as
  zero; it writes output structs up to `min(struct_size, its own sizeof)` and sets `struct_size` to the number of
  bytes it wrote. A `struct_size` smaller than the v1.0 size is `IM_ERR_ABI`. Structs embedded by value or used as
  array elements (`im_adapter_info`, `im_encoder_info`, `im_layer_config`, `im_audio_config`, `im_layer_info`,
  `im_rule`, `im_audio_level`) have no `struct_size` and are frozen in v1: new behavior uses their `reserved` fields
  or new constant values.
- **Strings**: UTF-8, NUL-terminated. Window titles from Windows (UTF-16) are converted; invalid surrogates become
  U+FFFD. Fixed `char[N]` output fields are always NUL-terminated and truncated on a UTF-8 boundary.
- **JSON outputs** (`im_build_info`, `im_list_sources`, `im_list_audio_apps`, `im_audio_sound_check`, `im_get_stats`,
  `im_abi_layout`) follow `snprintf`: they return the full length `n` of the document (without the NUL); when
  `cap > 0` they write `min(n, cap − 1)` bytes and a NUL. The caller retries with `n + 1` bytes when `n ≥ cap`.
  A negative return is an `im_status`. Field names are camelCase, like every isshoni JSON. `im_read_logs` differs:
  it writes whole lines only and returns the bytes written (§5.6).
- **Input memory**: the engine copies everything it needs from caller memory before the call returns and never keeps
  a pointer into it. This is what lets Go pass heap memory through `SyscallN` without pinning beyond the call (§12.4).
- **Output memory**: `im_packet.data` points into engine memory that stays valid until the next `im_next_packet` on
  the same session, or `im_stop` of that session, whichever comes first. The caller copies it.
- **Errors**: functions return `im_status` (0 = OK, positive = a non-error outcome, negative = an error) or a length.
  After a negative status, `im_last_error` on the **same OS thread** returns a one-line English detail for logs
  (never shown to users; the UI maps codes and tokens, §5.7). Whenever a detail token of §5.7 applies, the text
  starts with `<token>: ` (`blocked_by_policy: avcodec-<C>.dll: LoadLibraryExW failed (0x4EC)`); Go splits it off
  into `media.Error.Reason`. Every export clears the thread's detail first.
- **Threading**: every function may be called from any thread, concurrently, with two exceptions: `im_next_packet`
  takes one caller at a time per session, and `im_init`/`im_shutdown` must not race other calls. The engine never
  calls into the host. Details in §6.

### 5.2 The header

```c
/*
 * isshoni_media.h: the native media engine C ABI, v1.
 * Spec: docs/m2/04-windows-engine.md (audio block: docs/m2/05-windows-audio.md §4). Implemented by the Windows DLL
 * isshoni-media.dll (M2), the macOS engine (M3, static) and the Linux shim (M4, static).
 * SPDX-License-Identifier: Apache-2.0
 */
#ifndef ISSHONI_MEDIA_H
#define ISSHONI_MEDIA_H

#include <stdint.h>

#if defined(_WIN32)
#  if defined(IM_BUILDING_DLL)
#    define IM_API __declspec(dllexport)
#  elif defined(IM_STATIC)
#    define IM_API
#  else
#    define IM_API __declspec(dllimport)
#  endif
#  define IM_CALL __cdecl
#else
#  define IM_API __attribute__((visibility("default")))
#  define IM_CALL
#endif

#ifdef __cplusplus
extern "C" {
#endif

/* ---------------------------------------------------------------- version */
#define IM_ABI_MAJOR 1u
#define IM_ABI_MINOR 0u
#define IM_ABI_VERSION ((IM_ABI_MAJOR << 16) | IM_ABI_MINOR)

/* ---------------------------------------------------------------- status */
typedef int32_t im_status;
#define IM_OK                         0
#define IM_TIMEOUT                    1   /* im_next_packet: nothing within timeout_ms */
#define IM_ENDED                      2   /* im_next_packet: the session ended (its last event said why) */
#define IM_ERR_INVALID_ARG           -1
#define IM_ERR_BAD_STATE             -2   /* not initialized, stopped session, call in the wrong state */
#define IM_ERR_ABI                   -3   /* struct_size too small, ABI major mismatch */
#define IM_ERR_UNSUPPORTED_OS        -4
#define IM_ERR_PERMISSION_DENIED     -5
#define IM_ERR_AUDIO_DEVICE_CHANGED  -6   /* never returned by the Windows engine in M2 (S1 value kept) */
#define IM_ERR_SOURCE_GONE           -7   /* also a consumed, expired or evicted pick token (token pick_expired) */
#define IM_ERR_ENCODER_LOST          -8
#define IM_ERR_NO_ENCODER            -9
#define IM_ERR_CANCELLED            -10   /* the user closed the OS picker, or the pick timed out */
#define IM_ERR_LIMIT                -11   /* too many sessions or picks */
#define IM_ERR_DEPENDENCY           -12   /* FFmpeg or OpenH264 missing, wrong version or blocked */
#define IM_ERR_AUDIO_UNAVAILABLE    -13   /* never returned in M2 on Windows (audio never fails a call, 05) */
#define IM_ERR_NOT_IMPLEMENTED      -14
#define IM_ERR_TIMEOUT              -15   /* an engine thread didn't answer in time (§6.1) */
#define IM_ERR_INTERNAL             -99

/* ---------------------------------------------------------------- init, build info, clock */
#define IM_LOG_ERROR 0u
#define IM_LOG_WARN  1u
#define IM_LOG_INFO  2u
#define IM_LOG_DEBUG 3u

#define IM_INIT_NO_AUDIO 0x1u        /* never touch audio APIs (video-only tools) */

typedef struct im_init_params {
  uint32_t struct_size;
  uint32_t abi_version;              /* IM_ABI_VERSION the caller was built with */
  uint32_t flags;                    /* IM_INIT_* */
  uint32_t log_level;                /* IM_LOG_* */
  const char *ffmpeg_dir;            /* absolute dir of the FFmpeg DLLs; NULL = the engine DLL's own directory */
  const char *openh264_path;         /* absolute path of a verified OpenH264 library; NULL = none */
} im_init_params;

IM_API uint32_t  IM_CALL im_version(void);                 /* the DLL's IM_ABI_VERSION; callable before im_init */
IM_API im_status IM_CALL im_init(const im_init_params *params);
IM_API void      IM_CALL im_shutdown(void);                /* stops every session; im_init may follow */
IM_API int32_t   IM_CALL im_build_info(char *json, int32_t cap);
IM_API im_status IM_CALL im_set_openh264_path(const char *path_or_null);

typedef struct im_clock_sample {
  uint32_t struct_size;
  uint32_t reserved;
  int64_t mono_ns;                   /* engine clock: QPC ns (§12.3), mach_absolute_time (macOS), CLOCK_MONOTONIC (Linux) */
  int64_t unix_ns;                   /* wall clock at the same instant */
  int64_t uncertainty_ns;            /* half the bracket around the wall-clock read */
} im_clock_sample;

IM_API im_status IM_CALL im_clock(im_clock_sample *out);

/* ---------------------------------------------------------------- probe */
#define IM_MAX_ADAPTERS 4u
#define IM_MAX_ENCODERS 8u

#define IM_ENC_AUTO          0u
#define IM_ENC_NVENC         1u          /* token "nvenc" (§5.6) */
#define IM_ENC_QSV           2u          /* "qsv" */
#define IM_ENC_AMF           3u          /* "amf" */
#define IM_ENC_MF_HW         4u          /* "mf-hw" */
#define IM_ENC_MF_SW         5u          /* "mf-sw" */
#define IM_ENC_OPENH264      6u          /* "openh264" */
#define IM_ENC_VIDEOTOOLBOX  7u          /* "videotoolbox", later (M3) */
#define IM_ENC_GSTREAMER     8u          /* "gstreamer", later (M4) */
#define IM_ENC_FAKE        100u          /* "fake", IM_TEST_HOOKS builds only (§18.2) */

#define IM_ENCF_AVAILABLE    0x01u       /* opened and passed the one-frame test encode */
#define IM_ENCF_HARDWARE     0x02u
#define IM_ENCF_LIVE_BITRATE 0x04u       /* bitrate changes apply without a re-init */
#define IM_ENCF_ZERO_COPY    0x08u       /* takes D3D11 textures (no CPU readback) */
#define IM_ENCF_HIGH         0x10u       /* can encode High (6400) */
#define IM_ENCF_CB           0x20u       /* can encode Constrained Baseline (42e0) */
#define IM_ENCF_FORCED_IDR   0x40u       /* honours a per-frame IDR request */

#define IM_ADAPTER_SOFTWARE    0x1u      /* WARP / Microsoft Basic Render Driver */
#define IM_ADAPTER_HAS_OUTPUTS 0x2u

#define IM_OS_SUPPORTED              0x00000001u  /* meets isshoni's minimum (Win10 22H2 = build 19045) */
#define IM_OS_WGC                    0x00000002u  /* GraphicsCaptureSession::IsSupported() */
#define IM_OS_WGC_PICKER             0x00000004u
#define IM_OS_WGC_BORDERLESS         0x00000008u  /* IsBorderRequired present and access Allowed */
#define IM_OS_WGC_MIN_INTERVAL       0x00000010u  /* MinUpdateInterval present (24H2+): > 60 fps possible */
#define IM_OS_WGC_SECONDARY_WINDOWS  0x00000020u  /* IncludeSecondaryWindows present (24H2+) */
#define IM_OS_WGC_CURSOR_TOGGLE      0x00000040u  /* IsCursorCaptureEnabled present (19041+) */
#define IM_OS_DESKTOP_DUPLICATION    0x00000080u  /* DuplicateOutput1 works on at least one output */
#define IM_OS_EXCLUDE_FROM_CAPTURE   0x00000100u  /* WDA_EXCLUDEFROMCAPTURE (19041+) */
#define IM_OS_MEDIA_FOUNDATION       0x00000200u  /* MFStartup works (absent on N editions without the Media Feature Pack) */
#define IM_OS_HDR_DISPLAY            0x00000400u  /* at least one output has advanced color on */

/* audio_flags (probe flags; their names stay IM_AUDIO_*): semantics in docs/m2/05-windows-audio.md */
#define IM_AUDIO_PROCESS_LOOPBACK    0x1u
#define IM_AUDIO_ENDPOINT_LOOPBACK   0x2u
#define IM_AUDIO_SESSION_EVENTS      0x4u
#define IM_AUDIO_MIC_DETECTION       0x8u

typedef struct im_adapter_info {
  uint64_t luid;                     /* (HighPart << 32) | LowPart */
  uint32_t vendor_id;                /* PCI vendor: 0x10DE NVIDIA, 0x8086 Intel, 0x1002 AMD, 0x1414 Microsoft */
  uint32_t device_id;
  uint64_t driver_version;           /* UMD version, four 16-bit parts */
  char name[128];
  uint32_t flags;                    /* IM_ADAPTER_* */
  uint32_t reserved;
} im_adapter_info;

typedef struct im_encoder_info {
  uint32_t kind;                     /* IM_ENC_*; its token names the encoder everywhere (§5.6) */
  uint32_t adapter_index;            /* into im_caps.adapters; 0xFFFFFFFF for software encoders */
  uint32_t flags;                    /* IM_ENCF_* */
  im_status status;                  /* IM_OK, or why it is unavailable */
  uint32_t max_width, max_height;    /* 0 = unknown */
  uint32_t test_encode_us;           /* open + one 1080p IDR + close, wall time */
  uint32_t reserved[3];
  char codec[32];                    /* diagnostic: the FFmpeg codec name ("h264_nvenc") or "openh264" */
  char reason[32];                   /* the §5.7 token when status != IM_OK ("driver_too_old"); "" when IM_OK */
} im_encoder_info;

typedef struct im_caps {
  uint32_t struct_size;
  uint32_t os_build;                 /* e.g. 19045, 22631, 26100, 26200 */
  uint32_t os_ubr;                   /* update build revision */
  uint32_t os_flags;                 /* IM_OS_* */
  uint32_t audio_flags;              /* IM_AUDIO_* probe flags */
  uint32_t adapter_count;
  im_adapter_info adapters[IM_MAX_ADAPTERS];
  uint32_t encoder_count;
  uint32_t reserved0;
  im_encoder_info encoders[IM_MAX_ENCODERS];   /* in the engine's fallback order */
  uint32_t probe_ms;
  uint32_t reserved[15];
} im_caps;

#define IM_PROBE_QUICK 0x0u          /* OS flags and which encoders load; no test encodes (< 100 ms) */
#define IM_PROBE_FULL  0x1u          /* plus a one-frame test encode per encoder (< 2 s) */

IM_API im_status IM_CALL im_probe(uint32_t flags, im_caps *caps);

/* ---------------------------------------------------------------- sources */
#define IM_SOURCE_MONITOR       1u
#define IM_SOURCE_WINDOW        2u
#define IM_SOURCE_TEST_PATTERN  3u

#define IM_CAPTURE_WGC   1u
#define IM_CAPTURE_DD    2u
#define IM_CAPTURE_TEST  3u
#define IM_CAPTURE_SCK   4u                 /* later (M3) */
#define IM_CAPTURE_PORTAL 5u                /* later (M4) */
#define IM_CAPTURE_X11   6u                 /* later (M4) */

#define IM_SRCF_OS_BORDER 0x1u              /* Windows will draw its yellow capture border */
#define IM_SRCF_SELF      0x2u              /* the user picked an isshoni window */
#define IM_SRCF_MAPPED    0x4u              /* Win10: picked monitor mapped to a DD output */

/* JSON: {"monitors":[…],"windows":[]} (§5.6). Windows lists monitors only: windows come from the OS picker. */
IM_API int32_t IM_CALL im_list_sources(uint32_t flags, char *json, int32_t cap);

typedef struct im_pick_params {
  uint32_t struct_size;
  uint32_t flags;                    /* reserved (macOS M3: picker modes) */
  void *parent_window;               /* HWND (Windows), NSWindow* (macOS); NULL = an engine-owned owner window */
  uint32_t timeout_ms;               /* 0 = 120000; at most 600000 */
  uint32_t reserved;
} im_pick_params;

typedef struct im_source_info {
  uint32_t struct_size;
  uint32_t kind;                     /* IM_SOURCE_* */
  char id[64];                       /* opaque: "pick:7", "mon:\\.\DISPLAY1", "test:pattern" */
  char name[256];                    /* window title or monitor name: shown locally, never sent anywhere */
  char monitor_id[64];               /* monitor sources: the im_list_sources id; else "" */
  uint32_t width, height;            /* physical pixels at pick time */
  uint32_t capture_path;             /* IM_CAPTURE_*: what im_start will use */
  uint32_t flags;                    /* IM_SRCF_* */
  uint32_t pid;                      /* window sources: owning process if identified (§7.6), else 0 */
  uint32_t reserved0;
  uint64_t window;                   /* window sources: HWND if identified, else 0 */
  uint32_t reserved[4];
} im_source_info;

/* A pick token is single use: a successful im_start consumes it, a failed one leaves it valid. It expires 5 min after
   the pick; at most 4 unused ones are kept (the oldest is evicted). im_start with a consumed, expired or evicted token
   is IM_ERR_SOURCE_GONE with token pick_expired. */
IM_API im_status IM_CALL im_pick_source(const im_pick_params *params, im_source_info *out);
IM_API im_status IM_CALL im_cancel_pick(void);               /* makes a blocked im_pick_source return IM_ERR_CANCELLED */
IM_API void      IM_CALL im_release_source(const char *id);  /* forget an unused pick */

/* ---- audio (docs/m2/05-windows-audio.md §4; semantics, report JSON and rule matching are 05's) ---- */
#define IM_AUDIO_SAMPLE_RATE 48000u
#define IM_AUDIO_CHANNELS    2u
#define IM_MAX_RULES         512u
#define IM_MAX_RULE_VALUE    520u      /* bytes including the NUL (MAX_PATH-sized paths, UTF-8) */
#define IM_APP_ID_LEN        64u       /* bytes including the NUL */
#define IM_MAX_LEVEL_APPS    64u

#define IM_AUDIO_MODE_OFF          0u  /* im_start: no audio packets; im_audio_set_mode mid-share: muted, no capture */
#define IM_AUDIO_MODE_EXCLUDE_APPS 1u  /* default: everything except kept-out apps and isshoni */
#define IM_AUDIO_MODE_EVERYTHING   2u  /* reserved (later): everything except isshoni ("All sound"); in M2, im_start and
                                          im_audio_set_mode answer IM_ERR_INVALID_ARG (README §3 item 15) */
#define IM_AUDIO_MODE_TEST_TONE  100u  /* 04 (§10.3): 440 Hz, a 1 kHz beep in the first 100 ms of each second; no capture */

#define IM_RULE_APP_EXCLUDE      0u    /* value: exe file name; matches the process or an app ancestor */
#define IM_RULE_APP_ALLOW        1u    /* value: exe name; the "Include" answer */
#define IM_RULE_INSTANCE         2u    /* value: decimal pid; matches it and every OS descendant */
#define IM_RULE_STOP             3u    /* value: launcher/shell exe name; ends app walks */
#define IM_RULE_SHARED_RUNTIME   4u    /* value: exe name that belongs to its parent's app whatever its folder */
#define IM_RULE_BROWSER          5u    /* value: exe name of a browser */
#define IM_RULE_WEBVIEW_DATA_DIR 6u    /* value: full path of isshoni's WebView2 user data folder */

#define IM_RULE_SRC_BUILTIN 0u         /* the built-in voice-app list */
#define IM_RULE_SRC_USER    1u         /* a remembered user decision */
#define IM_RULE_SRC_SELF    2u         /* isshoni itself */
#define IM_RULE_SRC_VIEWER  3u         /* a browser that plays the user's isshoni viewer */
#define IM_RULE_SRC_DATA    4u         /* launchers, shared runtimes, browsers */

typedef struct im_rule {             /* an array element: frozen in v1; new behavior means new kind values */
  uint32_t kind;                     /* IM_RULE_* */
  uint32_t source;                   /* IM_RULE_SRC_* */
  const char *value;                 /* UTF-8, < IM_MAX_RULE_VALUE; compared case-insensitively */
  const char *app_id;                /* report id, < IM_APP_ID_LEN, [a-z0-9:._-]; NULL or "" = "exe:<value>" */
  const char *name;                  /* display name for reports; NULL = the exe's FileDescription */
} im_rule;

#define IM_EXCLF_KEEP_OUT_MIC_USERS 0x1u  /* "Keep out apps that use my microphone" (default on) */
#define IM_EXCLF_FAST_PATH          0x2u  /* allow the single-EXCLUDE fast path (default on) */

typedef struct im_exclusions {
  uint32_t struct_size;              /* sizeof(im_exclusions) */
  uint32_t flags;                    /* IM_EXCLF_* */
  const im_rule *rules;
  uint32_t rule_count;               /* <= IM_MAX_RULES */
  uint32_t shared_pid;               /* the shared window's process (from the pick, via Go); 0 = monitor or unknown */
} im_exclusions;

#define IM_AUDIOF_TAP_PCM          0x1u         /* also emit IM_PACKET_PCM chunks (audiotest; shipped) */
#define IM_AUDIOF_TEST_FAIL_PROBE  0x80000000u  /* audiotest only: behave as if the startup probe failed (shipped) */

typedef struct im_audio_config {     /* embedded by value in im_start_config: fixed size, no struct_size */
  uint32_t mode;                     /* IM_AUDIO_MODE_* */
  uint32_t bitrate_bps;              /* 32000..510000, 0 = 128000 */
  uint32_t frame_ms;                 /* 0 = 20; M2 only 20 */
  uint32_t flags;                    /* IM_AUDIOF_* */
  uint32_t reserved[4];
} im_audio_config;

typedef struct im_audio_level {
  char     app_id[IM_APP_ID_LEN];    /* NUL-terminated; matches apps[].id of the report */
  float    peak_db;                  /* max over the last 100 ms, dBFS; -120 = silence */
  float    rms_db;                   /* captured: RMS over 100 ms; activity: = peak_db */
  uint32_t source;                   /* 0 = captured (what friends hear), 1 = activity (session peak meter) */
  uint32_t reserved;
} im_audio_level;

typedef struct im_audio_levels {
  uint32_t       struct_size;        /* sizeof(im_audio_levels), set by the caller */
  uint32_t       report_seq;         /* equals the report's "seq"; a change means: fetch the report */
  int64_t        qpc_ns;             /* QPC time of this sample */
  float          mix_peak_db;        /* the mix fed to Opus, last 100 ms; -120 when not live or muted */
  float          mix_rms_db;
  uint32_t       count;              /* entries written to apps[] */
  uint32_t       reserved;
  im_audio_level apps[IM_MAX_LEVEL_APPS];
} im_audio_levels;

/* "What friends hear": 05's report JSON (05 §12.1). Live or watching: the current report; otherwise a one-shot
   snapshot (<= 500 ms). */
IM_API int32_t   IM_CALL im_list_audio_apps(char *json, int32_t cap);
/* Watcher without capture (the share sheet before going live). Idempotent; the rules replace the previous ones. */
IM_API im_status IM_CALL im_audio_watch(const im_exclusions *ex);
IM_API im_status IM_CALL im_audio_unwatch(void);                  /* no-op while a share with audio runs */
/* Replaces the rules (and flags, shared_pid) of the watcher and of a running share (copied). Applied within 50 ms;
   without a watcher or share they are stored for the next im_audio_watch or im_start. */
IM_API im_status IM_CALL im_set_exclusions(const im_exclusions *ex);
/* Mid-share: set_mode (OFF, EXCLUDE_APPS; EVERYTHING is IM_ERR_INVALID_ARG in M2) crossfades in 20 ms; set_muted is
   the pause mute 01's core sets:
   it fades in 10 ms and silent Opus frames keep flowing. */
IM_API im_status IM_CALL im_audio_set_mode(uint32_t mode);
IM_API im_status IM_CALL im_audio_set_muted(uint32_t muted);
/* Level meters (05 §12.2). Lock-free, < 50 µs. IM_ERR_BAD_STATE (token not_watching) when neither watching nor live. */
IM_API im_status IM_CALL im_audio_levels_get(im_audio_levels *out);
/* First-run sound check (05 §11.3): captures the process tree of helper_pid, outside the policy, and looks for the
   chime. Result JSON as 05 §11.3. */
IM_API int32_t   IM_CALL im_audio_sound_check(uint32_t helper_pid, uint32_t timeout_ms, char *json, int32_t cap);

/* ---------------------------------------------------------------- sessions */
#define IM_MAX_LAYERS   3u           /* M2 uses 2: "f" and "q"; "h" is M5 */
#define IM_ALL_LAYERS   0xFFFFFFFEu
#define IM_NO_LAYER     0xFFFFFFFFu  /* audio and PCM packets; events that name no layer */

#define IM_PROFILE_HIGH 0x6400u      /* profile_idc << 8 | constraint byte, as protocol CodecKey "h264/6400" */
#define IM_PROFILE_CB   0x42E0u      /* "h264/42e0" */

#define IM_CONTENT_AUTO   0u         /* presets Auto */
#define IM_CONTENT_MOTION 1u         /* Game, Movie */
#define IM_CONTENT_DETAIL 2u         /* Text */

#define IM_LAYER_INACTIVE 0x1u       /* start with this layer paused (im_set_layer_active) */

typedef struct im_layer_config {
  char rid[4];                       /* "f", "q"; NUL-terminated */
  uint32_t width, height;            /* bounding box in pixels; 0 = none */
  uint32_t max_pixels;               /* pixel budget (ShareParams maxPixels); 0 = none */
  uint32_t fps;                      /* 1..240 */
  uint32_t bitrate_bps;              /* initial target (01: ratectl's initial value, <= maxBitrate) */
  uint32_t gop_ms;                   /* 0 = default (layer 0: 4000, others: 2000); 1000..10000 */
  uint32_t flags;                    /* IM_LAYER_* */
  uint32_t reserved[2];
} im_layer_config;

#define IM_START_HIDE_CURSOR          0x1u  /* don't draw the pointer (0 = drawn: WGC cursor capture on, DD blends it) */
#define IM_START_PAUSED               0x2u
#define IM_START_ALLOW_LISTED_SOURCE  0x4u  /* accept "mon:" and "test:" ids (tools, tests); the app never sets it */
#define IM_START_KEEP_BORDER          0x8u  /* don't ask for borderless capture */

typedef struct im_start_config {
  uint32_t struct_size;
  uint32_t flags;                    /* IM_START_* */
  const char *source_id;             /* from im_pick_source (or im_list_sources with ALLOW_LISTED); NULL = audio only */
  uint32_t profile;                  /* IM_PROFILE_* */
  uint32_t content_hint;             /* IM_CONTENT_* */
  uint32_t encoder;                  /* IM_ENC_AUTO, or one kind to force */
  uint32_t layer_count;              /* 0 (audio only) .. IM_MAX_LAYERS */
  im_layer_config layers[IM_MAX_LAYERS];   /* wire order: "f" first */
  im_audio_config audio;             /* mode IM_AUDIO_MODE_OFF = no audio packets at all */
  const im_exclusions *exclusions;   /* NULL = the set from the last im_set_exclusions/im_audio_watch */
  uint32_t reserved[8];
} im_start_config;

#define IM_SESS_NO_OS_INDICATOR 0x1u /* no OS border (WGC borderless or DD): the app must show its own indicator */
#define IM_SESS_AUDIO           0x2u

#define IM_AUDIO_STATE_OFF       0u  /* mirrors 05's report "state" (05 §12.1); stats JSON uses the strings */
#define IM_AUDIO_STATE_STARTING  1u  /* "starting": the startup probe runs */
#define IM_AUDIO_STATE_OK        2u  /* "ok" */
#define IM_AUDIO_STATE_DEGRADED  3u  /* "degraded": probe failed or unsupported, endpoint loopback (05) */
#define IM_AUDIO_STATE_NO_DEVICE 4u  /* "no-device": silent frames until a render endpoint appears */

typedef struct im_layer_info {
  char rid[4];
  uint32_t encoder;                  /* IM_ENC_* in use */
  uint32_t width, height, fps, bitrate_bps;
  uint32_t profile;                  /* the SPS's key, e.g. 0x6400 */
  uint32_t flags;                    /* IM_ENCF_* of that encoder */
  uint32_t reserved[2];
} im_layer_info;

typedef struct im_session_info {
  uint32_t struct_size;
  uint32_t flags;                    /* IM_SESS_* */
  uint32_t capture_path;             /* IM_CAPTURE_* */
  uint32_t layer_count;
  int64_t epoch_ns;                  /* engine clock at im_start; every capture_ts_ns, of every kind, >= epoch_ns */
  uint32_t audio_state;              /* IM_AUDIO_STATE_* */
  uint32_t reserved0;
  im_layer_info layers[IM_MAX_LAYERS];
  uint32_t reserved[8];
} im_session_info;

typedef uint64_t im_session;         /* 0 is never a valid session */

IM_API im_status IM_CALL im_start(const im_start_config *cfg, im_session *out, im_session_info *info);
IM_API im_status IM_CALL im_get_session_info(im_session s, im_session_info *info);

/* ---------------------------------------------------------------- packets and events */
#define IM_PACKET_VIDEO 1u
#define IM_PACKET_AUDIO 2u           /* one 20 ms Opus packet */
#define IM_PACKET_EVENT 3u
#define IM_PACKET_PCM   4u           /* 480 float32 stereo frames (3840 bytes), only with IM_AUDIOF_TAP_PCM */
#define IM_PACKET_DEBUG 100u         /* IM_TEST_HOOKS builds only */

#define IM_PKT_KEYFRAME      0x1u    /* video: IDR access unit with SPS and PPS in front */
#define IM_PKT_REINIT        0x2u    /* video: the first AU after an encoder (re)open (with KEYFRAME): new SPS/PPS,
                                        maybe size or profile. Audio: the first packet of a session and the first
                                        after a new timestamp grid starts (PcmSink::OnGap); the time step may be any
                                        value */
#define IM_PKT_REFRESH       0x4u    /* video: a re-encode of an unchanged frame */
#define IM_PKT_DISCONTINUITY 0x8u    /* packets of this stream were dropped before this one (ring full, stale); the
                                        audio grid is unchanged */

#define IM_EVENT_ENDED                 1   /* final; status = the cause (IM_OK after im_stop) */
#define IM_EVENT_SOURCE_RESIZED        2
#define IM_EVENT_SOURCE_MINIMIZED      3
#define IM_EVENT_SOURCE_RESTORED       4
#define IM_EVENT_CAPTURE_INTERRUPTED   5   /* secure desktop, mode change: the last frame repeats */
#define IM_EVENT_CAPTURE_RESUMED       6
#define IM_EVENT_ENCODER_CHANGED       7   /* status IM_ERR_ENCODER_LOST: fell back to the next encoder; queued
                                              before the new encoder's first REINIT|KEYFRAME packet */
#define IM_EVENT_DEVICE_RESET          8   /* reserved: the GPU was reset or removed and the pipeline was rebuilt
                                              (device-loss recovery, M5); the M2 Windows engine never sends it */
#define IM_EVENT_LIMIT_CHANGED         9   /* the encoder can't keep up ("cpu", "gpu") or can again ("none") */
#define IM_EVENT_RING_OVERFLOW        10   /* the consumer fell behind; packets were dropped */
/* 32..63 reserved (audio reports go through im_list_audio_apps and im_audio_levels_get, 05) */

typedef struct im_packet {
  uint32_t struct_size;
  uint32_t kind;                     /* IM_PACKET_* */
  uint32_t layer;                    /* video: layer index; audio, PCM: IM_NO_LAYER; events may name one */
  uint32_t flags;                    /* IM_PKT_* */
  int64_t capture_ts_ns;             /* engine clock (im_clock), >= epoch_ns; events: when it happened */
  const uint8_t *data;               /* Annex B AU, one Opus packet, PCM f32 interleaved, or event detail JSON */
  uint32_t size;
  uint32_t duration_us;              /* audio: 20000 (Opus); PCM: 10000; else 0 */
  uint16_t width, height;            /* video */
  uint32_t profile;                  /* video: the SPS's key */
  int32_t event;                     /* IM_EVENT_*; 0 for media */
  im_status status;                  /* events: the code this event carries */
  uint32_t reserved[4];
} im_packet;

/* Blocks up to timeout_ms (at most 1000). IM_OK: *out is filled. IM_TIMEOUT: nothing yet. IM_ENDED: done. */
IM_API im_status IM_CALL im_next_packet(im_session s, uint32_t timeout_ms, im_packet *out);

/* ---------------------------------------------------------------- control (non-blocking, any thread) */
IM_API im_status IM_CALL im_request_keyframe(im_session s, uint32_t layer);          /* or IM_ALL_LAYERS */
IM_API im_status IM_CALL im_set_bitrate(im_session s, uint32_t layer, uint32_t bps); /* video layers only */
IM_API im_status IM_CALL im_set_layer_active(im_session s, uint32_t layer, uint32_t active);
IM_API im_status IM_CALL im_update_layer(im_session s, uint32_t layer, const im_layer_config *cfg);
IM_API im_status IM_CALL im_set_profile(im_session s, uint32_t profile);
IM_API im_status IM_CALL im_set_content_hint(im_session s, uint32_t hint);  /* IM_CONTENT_*: re-inits every layer */
IM_API im_status IM_CALL im_set_paused(im_session s, uint32_t paused);
IM_API im_status IM_CALL im_set_audio_bitrate(im_session s, uint32_t bps);
IM_API int32_t   IM_CALL im_get_stats(im_session s, char *json, int32_t cap);
IM_API im_status IM_CALL im_stop(im_session s);                                     /* blocks <= 2 s */

/* ---------------------------------------------------------------- own windows */
#define IM_PROTECT_OFF 0x1u                   /* undo (tests) */
IM_API im_status IM_CALL im_protect_window(void *native_window, uint32_t flags);

/* ---------------------------------------------------------------- diagnostics */
IM_API int32_t IM_CALL im_last_error(char *buf, int32_t cap);   /* this thread's last failure ("<token>: …") */
IM_API int32_t IM_CALL im_read_logs(char *buf, int32_t cap);    /* drains whole NDJSON lines; 0 = empty */
IM_API int32_t IM_CALL im_abi_layout(char *json, int32_t cap);  /* sizeof and offsetof of every struct */

/* -------------------------------------------- test exports: shipped, capture nothing (docs/m2/05 §4.1) */
/* For cmd/isshoni-audiotest only; stable within v1, not for apps. They render or decode test signals. */
IM_API im_status IM_CALL im_test_render_tone(const char *endpoint_id, float hz, float amp, int32_t ms);
IM_API int32_t   IM_CALL im_test_render_bursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms,
                                               int32_t period_ms, int32_t count, int64_t *onset_qpc_ns, int32_t cap);
IM_API int32_t   IM_CALL im_test_opus_decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames);

#ifdef __cplusplus
}
#endif
#endif /* ISSHONI_MEDIA_H */
```

How 05's §4.1 declarations were brought into this header's conventions: `#define`d constants on `uint32_t` fields
instead of C enums (`IM_RULE_SRC_*` for 05's `IM_SRC_*`, `IM_EXCLF_*` for 05's `IM_EXCL_*`, `IM_AUDIO_MODE_*` and
`IM_AUDIOF_*` for 05's mode and flag names); `IM_API`/`IM_CALL` and `im_status` returns; snprintf lengths instead of
05's `IM_ERR_BUFFER_TOO_SMALL`; `IM_ERR_BAD_STATE` with token `not_watching` instead of 05's `IM_ERR_NOT_STARTED`;
`im_audio_config` is a fixed 32-byte struct and `im_start_config` points at `im_exclusions` instead of nesting it, so
the start config keeps append-only evolution; audio and PCM packets carry `layer = IM_NO_LAYER` and the Opus target
is set with `im_set_audio_bitrate` (05's `IM_LAYER_AUDIO` is not defined); 05's `im_test_qpc_ns` is
`im_clock().mono_ns` (no separate export). Everything here is v1.0 (`IM_ABI_MINOR 0`) and lands in slice E1.

### 5.3 Call reference

"Blocks" is the longest the call may take on a healthy system; "Caller" is who needs it.

| Call | Blocks | Runs on | Caller | Notes |
|---|---|---|---|---|
| `im_version` | no | caller | Go `Open` | Before `im_init`; §5.5 |
| `im_init` | ≤ 500 ms | worker (started here) | Go `Open` | Starts the worker thread, calls 05's `AudioEngine::Init(flags)` (nothing audio starts with `IM_INIT_NO_AUDIO`), loads FFmpeg lazily (first probe/start), sets the log level. A second `im_init` without `im_shutdown` is `IM_ERR_BAD_STATE`. Never fails because FFmpeg is missing: that shows in the probe |
| `im_shutdown` | ≤ 3 s | caller + worker | Go `Engine.Close` | `im_stop` on every session, `AudioEngine::Shutdown()`, then stops the worker. `im_init` may follow. The DLL is never unloaded (`FreeLibrary` is not called: a hung driver thread could still run its code) |
| `im_build_info` | no | caller | Go `Open` (`Info().Build`, `BuildInfo`); logs, About, diagnostics | §5.6 |
| `im_set_openh264_path` | ≤ 50 ms | worker | 03 after the user enables OpenH264 (Go `SetOpenH264Path`) | Checks the file exists and `WelsGetCodecVersion` equals the pinned version (§9.9); `NULL` disables it. Takes effect at the next probe and `im_start`. Failures: `IM_ERR_DEPENDENCY` with `missing` or `version` |
| `im_clock` | no | caller | Go binding: the clock cross-check at `Open` (§12.3), and tools | Three brackets of QPC around `GetSystemTimePreciseAsFileTime`, the tightest kept. Not used for SR mapping: 01 anchors SRs from `Session.Now` (01 §8.2) |
| `im_probe` | quick < 100 ms, full < 2 s | worker (audio flags from 05's watcher) | 03 at startup through Go `Probe` (full, in the background), enginetest | Full probes run one test encode per encoder (§9.10). The result is cached until the next full probe; a quick probe re-reads OS flags only |
| `im_list_sources` | < 50 ms | worker | Go `Sources` (03 pill placement, diagnostics; tests), enginetest | §5.6. Monitors only on Windows |
| `im_pick_source` | until the user picks; default timeout 120 s | picker thread (STA) | Go `Pick` (03 share flow), enginetest `pick` | §7.2. Never call it from the UI thread. One pick at a time (`IM_ERR_LIMIT`, `pick_busy`, otherwise) |
| `im_cancel_pick` | no | caller | Go `Pick`'s ctx (03: Escape in the share sheet, app quit) | |
| `im_release_source` | no | caller | Go `Release` (03 when the user backs out) | Token rules: §7.2 |
| `im_list_audio_apps` | < 1 ms watching or live; ≤ 500 ms one-shot | 05's watcher | 05's `audio.Service` (`AudioController.AudioReport`); Go `AudioApps` | 05's report (05 §12.1) |
| `im_audio_watch`, `im_audio_unwatch` | < 10 ms (posted) | 05's watcher | 05's `audio.Service` (`WatchAudio`, `UnwatchAudio`) | 05 §4 |
| `im_set_exclusions` | < 10 ms; applied ≤ 50 ms | caller (copies, posts to 05's watcher) | 05's `audio.Service` (`SetExclusions`) | 05's semantics; `im_start_config.exclusions` is the same set at start |
| `im_audio_set_mode`, `im_audio_set_muted` | < 10 ms (posted) | 05's watcher and mixer | 01's core (`SetAudioMuted`, the pause mute) and 05's `audio.Service` (`SetAudioMode` for "Share sound" off/on) | `im_audio_set_muted` is the pause mute 01's core sets (05 owns the mix bus); "Share sound" off is `im_audio_set_mode(OFF)`; `IM_AUDIO_MODE_EVERYTHING` → `IM_ERR_INVALID_ARG` in M2; pause is `im_set_paused` |
| `im_audio_levels_get` | lock-free, < 50 µs | caller | 05's `audio.Service` (`AudioLevels`), 10 Hz | 05 §12.2 |
| `im_audio_sound_check` | ≤ `timeout_ms` + 500 ms | 05's watcher and activator threads | 05's `Service.SoundCheck` (03's first-run wizard) | 05 §11.3 |
| `im_start` | ≤ 3 s typical | worker | 01 (Go `Engine.Start`) | Opens capture, both encoders and audio (`Mixer::Start` ≤ 100 ms). A success consumes a picked `source_id` (single use); a failure leaves it valid (§7.2). Fills `im_session_info`: the layers really running, their encoder and profile. Each layer's first `KEYFRAME\|REINIT` is ready within 500 ms after it returns (§6.3) |
| `im_get_session_info` | no | caller | Go binding (`EventLayer` synthesis, §12.3) | Changes after re-inits and fallbacks |
| `im_next_packet` | ≤ `timeout_ms` (≤ 1000) | caller | 01's pump through Go `Session.Next` | One caller per session; §6.2 |
| `im_request_keyframe` | no | caller | 01 on PLI/FIR (`Session.RequestKeyframe`) | Coalesced (§9.4) |
| `im_set_bitrate` | no | caller | 01's rate control (`Session.SetBitrate`) | Video layers. Live or deferred re-init (§9.5) |
| `im_set_layer_active` | no | caller | 01 on `quality.hint` encodings `active` (`SetLayerActive`, `SetBitrate(layer, 0)`) | Inactive layers aren't encoded; the first frame after reactivation is an IDR |
| `im_update_layer` | no (validates, then async) | caller | 01 on a preset change (fps, e.g. Text 30) through `Reconfigure` | No resolution adaptation in M2 (01 D11; M5). An fps or size change = re-init with IDR |
| `im_set_profile` | no (async) | caller | 01 on `quality.hint{codec}` (M1 01 §8.10) through `Reconfigure{Profile}` | Re-inits every layer; the next packets carry `REINIT\|KEYFRAME` and the new `profile` |
| `im_set_content_hint` | no (async) | caller | 01 on a preset change (`Reconfigure{Preset}`) | Re-inits every layer with `REINIT\|KEYFRAME`, coalesced with the other re-inits, spaced at least 1 s apart (§9.5) |
| `im_set_paused` | no | caller | 01's core (`Share.SetPaused`, which 03's controls and hotkeys call; feature `share.pause`) | §8.5: no new captured content after the next frame interval; calls `Mixer::SetPaused` |
| `im_set_audio_bitrate` | no | caller | 01 on a preset change (Movie 256 kbps) through `Reconfigure{AudioBitrate}` or `SetBitrate(media.LayerAudio)` | Live (`OPUS_SET_BITRATE`) |
| `im_get_stats` | < 5 ms | caller (snapshot) | 01's core, polled at 1 Hz into `ShareSnapshot` for 03's stats panel and debug overlay; enginetest | §5.6 |
| `im_stop` | ≤ 2 s | caller + session threads | 01 (`Session.Stop`); the Go binding after `ENDED` | Idempotent on a live handle; after it returns the handle is invalid (`IM_ERR_INVALID_ARG`) |
| `im_protect_window` | < 5 ms | caller | `enginetest protect-check` (Go `ProtectWindow`) in M2; the macOS engine's SCContentFilter exclusions in M3. Not the app: 03's `winos` sets `WDA_EXCLUDEFROMCAPTURE` itself (§11) | §11 |
| `im_last_error` | no | caller | Go binding | Thread-local; `<token>: ` prefix (§5.7) |
| `im_read_logs` | no | caller | Go binding, 1 Hz and after every error | §5.6 |
| `im_abi_layout` | no | caller | Go tests | §5.4 |
| `im_test_render_tone`, `im_test_render_bursts` | ≤ the rendered length + 500 ms | 05's threads | 05's audiotest | 05 §18; shipped (§2) |
| `im_test_opus_decode` | < 1 ms | caller | 05's audiotest | Decodes one Opus packet with the DLL's libopus |

Posted jobs (the worker, 05's watcher) wait at most their row's "Blocks" value + 1 s; then the call returns
`IM_ERR_TIMEOUT` with token `engine_timeout` (§6.1).

### 5.4 Struct layouts (amd64 and arm64)

Natural C alignment, no packing pragmas. C++ `static_assert`s pin these sizes and offsets in `native/common/tests/
abi_layout_test.cpp` and `api/exports.cpp`; the Go mirror asserts the same numbers in `layout_test.go` (every OS),
and on Windows compares them with the DLL's `im_abi_layout()` output. A size change without a minor bump fails the
build. Every struct below is v1.0: the golden file `native/common/tests/testdata/abi_layout.json` is written once in
slice E1 from these numbers (including 05's audio structs).

| Struct | Size | Notes |
|---|---|---|
| `im_init_params` | 32 | pointers at 16 and 24 |
| `im_clock_sample` | 32 | |
| `im_adapter_info` | 160 | `name` at 24 |
| `im_encoder_info` | 104 | `codec` at 40, `reason` at 72 |
| `im_caps` | 1568 | `adapters` at 24, `encoder_count` at 664, `encoders` at 672, `probe_ms` at 1504 |
| `im_pick_params` | 24 | `parent_window` at 8 |
| `im_source_info` | 440 | `id` 8, `name` 72, `monitor_id` 328, `width` 392, `pid` 408, `window` 416 |
| `im_rule` | 32 | `value` 8, `app_id` 16, `name` 24 |
| `im_exclusions` | 24 | `rules` 8, `rule_count` 16, `shared_pid` 20 |
| `im_audio_config` | 32 | `mode` 0, `bitrate_bps` 4, `frame_ms` 8, `flags` 12 |
| `im_audio_level` | 80 | `peak_db` 64, `rms_db` 68, `source` 72 |
| `im_audio_levels` | 5152 | `qpc_ns` 8, `mix_peak_db` 16, `count` 24, `apps` 32 |
| `im_layer_config` | 40 | |
| `im_start_config` | 224 | `source_id` 8, `layers` 32, `audio` 152, `exclusions` 184, `reserved` 192 |
| `im_layer_info` | 40 | |
| `im_session_info` | 184 | `epoch_ns` 16, `layers` 32 |
| `im_packet` | 72 | `capture_ts_ns` 16, `data` 24, `size` 32, `width` 40, `profile` 44, `event` 48, `status` 52 |

`im_abi_layout` returns `{"abi":65536,"structs":{"im_packet":{"size":72,"fields":{"struct_size":0,"kind":4,…}},…}}`.

### 5.5 Versioning and how Go checks it

- `IM_ABI_VERSION` = `major << 16 | minor`. v1.0 = `0x00010000`. Majors are shared by every engine (Windows, macOS,
  Linux); the header is the one source.
- **Minor bump** (additive): new functions, new constants, new fields appended to structs (with their `reserved`
  space used first where it fits), new JSON fields. **Major bump**: anything else. There is no major 2 planned; a
  v1 change that can't be additive is a design error to fix in review.
- **Go's check at load** (`native.Open`): `im_version()` must have major 1 and minor ≥ `goMinMinor` (the newest minor
  whose functions or fields Go uses unconditionally; 0 in M2). Exports beyond that minor are resolved with
  `GetProcAddress` as optional and gate their feature. A failure is `*media.Error{Code: internal, Reason: "abi"}`
  with a detail naming the missing export or both versions, and the app treats the engine as missing (sharing off,
  viewing works; 03 shows "Reinstall isshoni").
- **`im_init.abi_version`** tells the DLL which minor the caller expects; the DLL logs a mismatch and keeps working
  (struct sizes carry the real compatibility).
- **Build version**: the DLL's `VERSIONINFO` and `im_build_info().engine` carry the release SemVer (06 sets it from
  the tag). Go logs both versions at startup and warns on a mismatch; it does not refuse, because the installer
  always ships both together and dev builds mix them on purpose.
- **Static engines (M3, M4)**: `im_version` is still called, so a stale static library linked into a newer Go build
  fails loudly in CI.

### 5.6 JSON payloads owned by this doc

`im_build_info`:
```json
{"engine":"0.4.0","abi":65536,"commit":"0affed6","testHooks":false,"os":"windows","arch":"amd64",
 "compiler":"MSVC 19.44","ffmpeg":{"loaded":true,"dir":".","tag":"deps-ffmpeg-9.0.2-r1","avcodec":"NN.NN.100",
 "avutil":"NN.NN.100","license":"LGPL version 2.1 or later"},"opus":"libopus 1.5.2",
 "openh264":{"loaded":false,"version":null}}
```
`ffmpeg.dir` is relative to the engine DLL's directory (`"."` when the FFmpeg DLLs sit next to it, as installed),
never an absolute path; `tag` is `native/ffmpeg/lock.json`'s `tag`, compiled in.

**Encoder tokens.** One token per `im_encoder_info.kind` names an encoder everywhere: the stats JSON `encoder`
field, `ENCODER_CHANGED`'s `from`/`to`, Go's `media.EncoderInfo.Name` and `media.Event.Encoder`, enginetest's
`-encoder` flag, and (through 01) the server's `stats.outbound.encoder`, the toast "Switched to <encoder>" and
`LayerSnapshot`. The FFmpeg codec name is diagnostic only (`im_encoder_info.codec`, the JSON field `codec`).

| Kind | Token | `codec` |
|---|---|---|
| `IM_ENC_NVENC` | `nvenc` | `h264_nvenc` |
| `IM_ENC_QSV` | `qsv` | `h264_qsv` |
| `IM_ENC_AMF` | `amf` | `h264_amf` |
| `IM_ENC_MF_HW` | `mf-hw` | `h264_mf` |
| `IM_ENC_MF_SW` | `mf-sw` | `h264_mf` |
| `IM_ENC_OPENH264` | `openh264` | `openh264` |
| `IM_ENC_FAKE` | `fake` | `fake` |
| `IM_ENC_VIDEOTOOLBOX`, `IM_ENC_GSTREAMER` (later) | `videotoolbox`, `gstreamer` | the platform's name |

`im_list_sources` (Windows; `windows` stays empty because windows come from the OS picker):
```json
{"monitors":[{"id":"mon:\\\\.\\DISPLAY1","name":"DELL U2723QE","primary":true,"x":0,"y":0,"width":3840,
 "height":2160,"dpi":144,"refreshHz":60,"rotation":0,"hdr":false,"adapter":0}],"windows":[]}
```
Coordinates are physical pixels in the virtual desktop; `dpi` is `GetDpiForMonitor(MDT_EFFECTIVE_DPI)`; `rotation`
is 0/90/180/270; `adapter` indexes `im_caps.adapters`.

`im_get_stats`:
```json
{"layers":[{"rid":"f","encoder":"nvenc","codec":"h264_nvenc","hw":true,"width":1920,"height":1080,
  "active":true,"fpsIn":60.0,"fpsOut":59.8,"bitrateBps":7900000,"targetBps":8000000,"keyframes":3,"refreshes":0,
  "reinits":0,"drops":{"pacer":12,"ring":0,"stale":0},"encodeMs":{"p50":3.1,"p95":4.8,"p99":6.0},
  "latencyMs":{"p50":9.0,"p95":14.0},"limit":"none"},
  {"rid":"q","encoder":"nvenc","codec":"h264_nvenc","hw":true,"width":640,"height":360,"active":true,
  "fpsIn":60.0,"fpsOut":15.0,"bitrateBps":290000,"targetBps":300000,"keyframes":5,"refreshes":0,"reinits":0,
  "drops":{"pacer":0,"ring":0,"stale":0},"encodeMs":{"p50":0.9,"p95":1.4,"p99":2.0},
  "latencyMs":{"p50":6.0,"p95":9.0},"limit":"none"}],
 "capture":{"path":"wgc","sourceWidth":2560,"sourceHeight":1440,"frames":3600,"interrupted":false},
 "audio":{"state":"ok","packets":3000,"drops":0,"bitrateBps":128000},"paused":false}
```
`audio.state` is `off`, `starting`, `ok`, `degraded` or `no-device` (`IM_AUDIO_STATE_*`, the strings of 05's report
`state`).
`fpsIn` counts captured frames, `fpsOut` encoded ones over the last 2 s. `latencyMs` is capture time → packet in
the ring. `limit` is `none`, `cpu` (a software encoder can't keep up), `gpu` (a hardware encoder or the GPU path
can't) or `source` (the source delivers fewer frames, for example a static screen). The fields match the SPA's
`ShareStatsSample` (`encoder`, `hw`, `limit`), which 03 fills from them.

Event detail JSON (`im_packet.data` of `IM_PACKET_EVENT`):

| Event | Detail |
|---|---|
| `ENDED` | `{"reason":"stopped"\|"source_gone"\|"encoder_lost"\|"permission_denied"\|"internal","detail":"<token>: …"}` (`detail` starts with a §5.7 token when one applies, like `im_last_error`) |
| `SOURCE_RESIZED` | `{"width":1280,"height":720}` (the re-init follows after 1 s of stability, §7.7) |
| `SOURCE_MINIMIZED`, `SOURCE_RESTORED` | `{}` |
| `CAPTURE_INTERRUPTED`, `CAPTURE_RESUMED` | `{"cause":"access_lost"\|"secure_desktop"\|"mode_change"}` |
| `ENCODER_CHANGED` | `{"layer":"f","from":"nvenc","to":"mf-hw","codec":"h264_mf","reason":"open_failed"\|"encode_error"\|"session_limit"}` (a device removal ends the session instead, §9.8) |
| `DEVICE_RESET` | `{"reason":"0x887A0006","attempt":1}` (reserved for M5's recovery; never sent in M2) |
| `LIMIT_CHANGED` | `{"layer":"f","limit":"cpu"\|"gpu"\|"none","fps":31.5}` |
| `RING_OVERFLOW` | `{"stream":"f"\|"q"\|"audio"\|"pcm","dropped":12}` |

Event JSON is at most 1 KiB. Audio has no events: its state, banners and report events come from
`im_list_audio_apps` and `im_audio_levels_get` (05 §12).

`im_read_logs`: newline-delimited JSON records, oldest first, whole lines only, at most `cap − 1` bytes; records that
don't fit stay queued; 0 = empty. Record: `{"t":"2026-10-21T12:00:00.000Z","level":"info","component":"encode",
"msg":"encoder opened","attrs":{"layer":"f","encoder":"nvenc","w":1920,"h":1080}}`. The log ring holds 512
records; real-time threads drop records when it is full (counted in the next record). **Privacy**: no record at any
level, debug included, contains a window title or names an executable, a process or an app (README §3 item 18; 03
§17 drains this ring at every level). Logs stay on the device (plan "Privacy and trust model").

### 5.7 Status codes, events and the UI state each maps to

The plan's rule: each error maps to a UI state. The engine reports; 03 renders (all strings are 03's); 01 decides
what the publisher does.

| Code / event | Where it appears | What the engine already did | UI state (03) |
|---|---|---|---|
| `IM_ERR_PERMISSION_DENIED` | `im_pick_source`, `im_start`, `ENDED` | nothing | **Guided fix**: a dialog explaining the Windows privacy setting that blocked capture, with Retry |
| `IM_ERR_ENCODER_LOST` in `ENCODER_CHANGED` | event | **fell back to the next encoder** (§9.8); the share continues | 01's `encoder_fallback` notice from `EventEncoderChanged` (01 §11.4): "Switched to <encoder>" (03's string) |
| `IM_ERR_ENCODER_LOST` in `ENDED` | event | no encoder left | **Stop with a notice**: "Video encoding stopped" with "Update your graphics driver" or "Turn on OpenH264" |
| `IM_ERR_ENCODER_LOST` in `ENDED`, token `device_removed` | event | the GPU device was removed or reset (TDR, driver update, GPU unplugged); the session ended at once; the next `im_start` uses a new device (§9.8) | **Stop with a notice** offering **"Share again"** (a new pick and share; the driver usually comes back within seconds) |
| `IM_EVENT_DEVICE_RESET` | never in M2 (reserved for M5's recovery) | — | none |
| `IM_ERR_SOURCE_GONE` in `ENDED` | event | ended the session | **Stop with a notice**: "The window you shared was closed" / "The screen you shared was disconnected" |
| `IM_ERR_SOURCE_GONE`, token `pick_expired` | `im_start` | nothing (the token was consumed, expired or evicted) | **Pick again**: back to the OS picker, no error text (01 maps it to `source_gone`) |
| audio (no codes, no events) | 05's report and levels | **rebuilt audio without stopping video** on a device change (05's `default_device_changed`/`device_removed` report events) | 05's FriendsHear banners (05 §14.7): `reconnecting-sound` for the plan's "rebuild audio without stopping video", the plan's "Voice apps can't be excluded on this PC…" for `degraded` |
| `IM_ERR_UNSUPPORTED_OS` | `im_start`, probe flag `IM_OS_SUPPORTED` clear | nothing | sharing from the app is off: "Needs Windows 10 22H2 or newer"; viewing works; the browser sharer remains |
| `IM_ERR_NO_ENCODER` | `im_start` | nothing | **Guided fix**: "Turn on OpenH264" when it is off, else "Update your graphics driver" |
| `IM_ERR_DEPENDENCY` | probe (`encoders[].status` and `.reason`), `im_set_openh264_path`, `im_start` | audio-only still works | "Part of isshoni is missing or blocked: reinstall"; token `blocked_by_policy` gets the Smart App Control text (§17); `driver_too_old` names the needed driver (§13.2) |
| `IM_ERR_CANCELLED` | `im_pick_source` | nothing | silent: back to the share sheet |
| `IM_ERR_LIMIT` | `im_start`, `im_pick_source` | nothing | "Stop your other share first" (`session_limit`), or nothing for `pick_busy` (the picker is already open) |
| `IM_ERR_TIMEOUT` | any posted call | logged with detail | "Something went wrong" (the call may be retried) |
| `SOURCE_MINIMIZED` | event | keeps the last frame | "Your shared window is minimized: friends see the last picture" |
| `CAPTURE_INTERRUPTED` | event | repeats the last frame, retries | none (it resumes by itself) |
| `LIMIT_CHANGED` `cpu`/`gpu`, then `none` | event (`EventLimit` in Go, both edges) | lowers the frame rate (§8.4); `none` after 10 s above 95 % | the SPA's `cpu-limited` hint while `cpu`/`gpu`, cleared on `none` (01 also holds and releases ratectl's ramp with `ratectl.SetLimit`) |
| `IM_ERR_INTERNAL`, others | any | logged with detail | "Something went wrong" (plus "Copy diagnostics" in M5) |

`IM_ERR_AUDIO_DEVICE_CHANGED` and `IM_ERR_AUDIO_UNAVAILABLE` are never returned by the Windows engine in M2; they keep
their values for S1 compatibility and for later engines.

**Detail tokens** (fixed in v1; new ones are additive). They start `im_last_error`'s text (`<token>: `), fill
`im_encoder_info.reason`, and start the `ENDED` event's `detail`. Go puts them into `media.Error.Reason` and
`media.EncoderInfo.Reason` (§12.3).

| Token | Status | Meaning |
|---|---|---|
| `missing` | `IM_ERR_DEPENDENCY` | an FFmpeg DLL or the OpenH264 file is absent from its folder |
| `version` | `IM_ERR_DEPENDENCY` | wrong FFmpeg major (or minor below the build's), or `WelsGetCodecVersion` ≠ the pin |
| `blocked_by_policy` | `IM_ERR_DEPENDENCY` | `ERROR_ACCESS_DISABLED_BY_POLICY`, `STATUS_INVALID_IMAGE_HASH` (Smart App Control, WDAC) |
| `driver_too_old` | `IM_ERR_DEPENDENCY` (encoder status) | NVENC's API version is newer than the driver |
| `disabled` | `IM_ERR_DEPENDENCY` (encoder status) | OpenH264 is not turned on |
| `no_media_foundation` | `IM_ERR_DEPENDENCY` (encoder status) | `MFStartup` failed (N editions without the Media Feature Pack) |
| `slow_open` | `IM_ERR_NO_ENCODER` (encoder status) | the probe's open took more than 1 s |
| `open_failed` | `IM_ERR_NO_ENCODER` | `avcodec_open2` (or OpenH264's `InitializeExt`) failed |
| `test_encode_failed` | `IM_ERR_NO_ENCODER` (encoder status) | the test encode gave no SPS, PPS and IDR slice |
| `layer_failed` | `IM_ERR_NO_ENCODER` | `im_start`: no encoder opened for one layer; the rest of the text names its rid |
| `forced_failed` | `IM_ERR_NO_ENCODER` | the forced encoder (`im_start_config.encoder`) failed to open |
| `device_removed` | `IM_ERR_ENCODER_LOST` (`ENDED`) | `DXGI_ERROR_DEVICE_REMOVED`/`_RESET`, or an encoder error with `GetDeviceRemovedReason() != S_OK` (§9.8) |
| `session_limit` | `IM_ERR_LIMIT`, encoder status | the engine's 4 sessions are in use, or NVENC refused another session |
| `pick_busy` | `IM_ERR_LIMIT` | another `im_pick_source` is open |
| `pick_expired` | `IM_ERR_SOURCE_GONE` | the pick token was consumed, expired or evicted (§7.2) |
| `self_window` | (Go only) | the user picked an isshoni window (`IM_SRCF_SELF`); Go's `Pick` returns `invalid` with it (§12.3) |
| `os_too_old` | `IM_ERR_UNSUPPORTED_OS` | build below 19045 (19041 for `im_protect_window`) |
| `capture_denied` | `IM_ERR_PERMISSION_DENIED` | Windows' screen-capture privacy setting or `GraphicsCaptureAccess` denied it |
| `dpi_unaware` | `IM_ERR_INTERNAL` | DD needs a per-monitor DPI-aware process (§7.4) |
| `engine_timeout` | `IM_ERR_TIMEOUT` | a posted job didn't finish within its bound (§6.1) |
| `not_initialized` | `IM_ERR_BAD_STATE` | a call before `im_init` or after `im_shutdown` |
| `not_watching` | `IM_ERR_BAD_STATE` | `im_audio_levels_get` while neither watching nor live |
| `abi` | `IM_ERR_ABI` | `struct_size` below v1.0, or an ABI major mismatch |

Go maps every status to a `*media.Error` with 01's code and this token as `Reason` (§12.3); 03 maps with `errors.Is`
on 01's sentinels and reads `Reason`.

## 6. Threading, memory and the session state machine

### 6.1 Threads

| Thread | How many | Started by | Priority | Does | Never |
|---|---|---|---|---|---|
| **Engine worker** | 1 per process | `im_init` | normal, COM MTA | every API call that touches COM, WinRT, DXGI or D3D creation (probe, sources, start and stop steps); the API call posts a job and waits. **No WASAPI work**: the probe's audio flags (`AudioEngine::ProbeFlags`) and the audio app list (`ListAppsJson`) are posted to 05's watcher | media work |
| **Picker** | 1, on demand | `im_pick_source` | normal, **STA** with a message loop and a hidden owner window | `GraphicsCapturePicker` (§7.2) | anything else |
| **WGC callback** | the system's pool | WGC `FrameArrived` | system | `TryGetNextFrame`, one `CopySubresourceRegion` into a free slot, publish, signal (≤ 1 ms) | wait, allocate, encode |
| **DD capture** | 1 per DD session | `im_start` | MMCSS "Capture" | `AcquireNextFrame`, copy to a slot, pointer shape, release | encode |
| **Video** | 1 per session | `im_start` | MMCSS "Capture", `THREAD_PRIORITY_ABOVE_NORMAL` | pacer, cursor blend, VideoProcessor, every layer's encoder, the AU normalizer, writes to the rings | wait on the consumer |
| **Supervisor** | 1 per session | `im_start` | normal | turns real-time flags into events, `IsIconic` polling (500 ms), resize debounce, staleness, ending the session on device loss (§9.8), test-pattern timer | real-time work |
| **Audio watcher** (05) | 1 per process | `im_init` (`AudioEngine::Init`) | normal, COM MTA | all WASAPI enumeration, session and device notifications, the process table, the report, `im_list_audio_apps`, `im_audio_watch`, `im_set_exclusions`, the sound check's requests (05 §7) | real-time work |
| **Audio activators** (05) | 2 per process | 05's watcher | normal, COM MTA | process-loopback activation (`ActivateAudioInterfaceAsync`), 3 s limit (05 §8.1) | real-time work |
| **Audio capture** (05) | 1 while capturing | 05 | MMCSS "Pro Audio" | waits on up to 63 stream events, fills 05's rings (05 §8.4) | locks, allocation, Go |
| **Audio mixer** (05) | 1 while capturing | 05 | MMCSS "Pro Audio" | 10 ms mix ticks; calls `PcmSink::OnChunk`, which runs the Opus stage and the PCM tap inline (§10.3) | locks, allocation, Go |
| **Test tone** | 1 per session in `IM_AUDIO_MODE_TEST_TONE` | `im_start` | MMCSS "Pro Audio" | stands in for 05's mixer: feeds the same `PcmSink` every 10 ms (§10.3) | locks, allocation, Go |
| **Consumer** | the caller's | Go | Go's | `im_next_packet` | — |

**Posted jobs**: an API call that posts a job (to the worker, or through `AudioEngine` to 05's watcher) waits at most
its §5.3 row's "Blocks" value + 1 s; then it returns `IM_ERR_TIMEOUT` (token `engine_timeout`) and the job's late
result is discarded (a late `im_start` is stopped and freed by the worker itself).

Rules for every real-time thread (WGC callback, DD capture, video, audio capture and mixer, test tone): no
allocation after warm-up (pools are sized at start), no lock that another thread holds across blocking work, no
synchronous logging (the log ring is a bounded lock-free MPSC queue and drops on overflow), and never a call into the
host. The D3D11 immediate context is
shared by the capture copy and the video thread; it is guarded by one recursive lock held only around D3D calls, never
across a wait (§8.1).

Session control calls (`im_request_keyframe` … `im_set_audio_bitrate`) validate their arguments, push a command onto the
session's command queue (64 entries, a mutex held for the push only) and return. The video thread drains it at every
wake-up; the result shows in packets, events and stats.

### 6.2 Rings and packet memory

- **One SPSC ring per stream**: per video layer (producer: the video thread), one for Opus audio and one for the PCM
  tap (producer of both: the mixer thread, or the test-tone thread, through the Opus stage). Each ring is a slot
  array plus a byte arena; a slot holds `{offset, size, flags, capture_ts, width, height, profile, duration}`. The
  consumer is the single `im_next_packet` caller.

  | Stream | Slots | Arena |
  |---|---|---|
  | layer 0 (`f`) | 64 | 8 MiB (≈ 2 s at 30 Mbps; a 1080p IDR is 150–400 KB) |
  | other layers | 64 | 1 MiB |
  | audio, Opus | 128 (2.56 s) | 256 KiB |
  | PCM tap (`IM_PACKET_PCM`, only with `IM_AUDIOF_TAP_PCM`) | 128 (1.28 s) | 512 KiB (3840 bytes per 10 ms) |

- **Events**: one bounded MPSC queue per session, 64 entries of at most 1 KiB of JSON each. Real-time threads only set
  atomic flags; the supervisor turns them into events. On overflow the oldest non-`ENDED` event goes (counted in the
  next `RING_OVERFLOW`).
- **`im_next_packet` order**: events first; then, among the streams with a ready packet, the one with the smallest
  `capture_ts_ns`. There is no holding back: order is strict **within** a stream, not across streams. 01's publisher
  stamps each track from its own packets' capture times, so cross-stream order doesn't matter (M1's fake returned
  capture order across streams; nothing in `publish` relies on it, and 01 keeps it that way).
- **Release**: the slot handed out by one call is released at the start of the next call on that session (or by
  `im_stop`). That is the "valid until the next call" rule of the plan.
- **Wake-up**: producers bump a 32-bit sequence and wake it (`WakeByAddressSingle`; the `os/` shim maps it to
  `futex` on Linux and `os_sync_wake_by_address_any` on macOS later); the consumer waits with `WaitOnAddress` and its
  timeout. No lock on the hot path.
- **A full ring**: video drops the access unit, marks the layer "needs IDR" (the next encoded frame of that layer is
  an IDR, encoding pauses until there is room for it) and sets `DISCONTINUITY` on the next delivered packet. Audio
  drops the newest packet and sets `DISCONTINUITY`. Both count, and the supervisor sends `RING_OVERFLOW`.
- **Stale packets**: `im_next_packet` drops video access units whose capture time is more than 1 s old at dequeue,
  then drops that layer until its next keyframe and asks for one; audio older than 1 s is dropped too. A consumer
  that stopped pulling (a pub PC rebuild, a GC pause) thus resumes with fresh media and an IDR within one frame,
  like M1's fake skipping its clock forward (02 §15.1 of M1). Events never expire.

### 6.3 Session state machine

```
                 im_start ok
   (none) ─────────────────────► RUNNING ◄────── im_set_paused(0) ────── PAUSED
                                  │      └──────── im_set_paused(1) ──────►  │
                                  │  (encoder fallback stays RUNNING: ENCODER_CHANGED, §9.8)
                                  │                                          │
   device removed or reset,       │                                          │
   candidates exhausted,          │                                          │
   SOURCE_GONE, PERMISSION_DENIED,│                                          │ ENDED{status}
   fatal internal error ──────────┴──────────────────────────────────────────┴──► ENDED ── im_next_packet → IM_ENDED
   im_stop (any state) ──────────────────────────────────────────────────────────►  │
                                                                                     │ im_stop
                                                                                     ▼
                                                                           freed (handle invalid)
```

- `im_start` either returns a RUNNING (or PAUSED, with `IM_START_PAUSED`) session or an error with nothing left
  behind (a pick token stays valid after a failure, §7.2). The first video packet of each layer is `KEYFRAME|REINIT`.
- **First keyframe bound**: each layer's first `KEYFRAME|REINIT` packet is ready within 500 ms after `im_start`
  returns, also on a static screen (the pacer encodes the first captured frame at once; WGC and DD both deliver an
  initial frame; else the last frame is re-encoded at 300 ms: should no capture have landed by then, the slot's
  content, black, is encoded as the IDR and the first real frame follows as soon as it arrives). Asserted in
  `session_fake_test` and in `enginetest capture` (01 §5.3 and Conformance check 2 rely on it).
- No RECOVERING state in M2: a device removal ends the session with `ENDED{IM_ERR_ENCODER_LOST}`, token
  `device_removed` (§9.8). Rebuilding the pipeline inside a live session is M5 (§16).
- ENDED: the `ENDED` event is the last packet; afterwards `im_next_packet` returns `IM_ENDED` at once. Resources stay
  until `im_stop`, which the host must still call.
- `im_stop` from any state: capture stops first (the privacy-relevant part, within one frame interval), then the
  threads join (≤ 2 s; a thread stuck in a driver call is abandoned with an error log, never killed), then the handle
  is freed. A consumer blocked in `im_next_packet` on that session returns `IM_ENDED` within 50 ms.
- Handles are `index << 32 | generation` into a 16-slot table; a stale or foreign handle is `IM_ERR_INVALID_ARG`.

### 6.4 Timeouts and limits

| What | Value |
|---|---|
| Sessions per process | 4 (`IM_ERR_LIMIT` beyond; the v1 UI uses 1) |
| Sessions with audio | 1 (05's mixer is per process) |
| Layers per session | 3 (M2: 2) |
| Output size per side | 64 .. 4096 pixels, even |
| fps | 1 .. 240 (M2 presets: 60, Text 30, preview 15) |
| Bitrate | layer 0: 100 kbps .. 50 Mbps; other layers: 50 kbps .. 2 Mbps; Opus 32 .. 510 kbps |
| GOP | 1000 .. 10000 ms (defaults 4000 / 2000) |
| Forced IDR spacing | at most one per 250 ms per layer; later requests wait for the 250 ms mark |
| Re-init spacing | bitrate re-init (AMF, MF): ≥ 10 s, latest value wins; size, fps, profile or content-hint re-init: ≥ 1 s, coalesced |
| Resize debounce | 1 s of a stable source size |
| Static refresh | 200 ms after the last change, then every 1 s |
| Picker | default 120 s, at most 600 s; 1 pick at a time; unused picks live 5 min from the pick, at most 4 (oldest evicted); a failed `im_start` keeps the token |
| `im_start` | ≤ 3 s typical (encoder opens dominate); no step waits more than 2 s |
| First keyframe per layer | ≤ 500 ms after `im_start` returns (§6.3) |
| Posted job wait | the §5.3 "Blocks" value + 1 s, then `IM_ERR_TIMEOUT` (§6.1) |
| Event JSON | ≤ 1 KiB |
| `im_stop` | ≤ 2 s |
| `im_next_packet` timeout | ≤ 1000 ms |
| Device loss | the session ends (`ENDED{IM_ERR_ENCODER_LOST}`, `device_removed`) within 1 s of detection; no recovery in M2 (M5) |
| DD access lost | retry every 250 ms for as long as the output exists; output absent for 5 s → `SOURCE_GONE` |
| Stale packet | 1 s |
| Log ring | 512 records |

## 7. Capture

### 7.1 Paths per Windows version

| | Windows 11 (build ≥ 22000) | Windows 10 22H2 (build 19045) |
|---|---|---|
| **Window** | WGC from the picker. Borderless when `IsBorderRequired` exists and `GraphicsCaptureAccess::RequestAccessAsync(Borderless)` returns `Allowed`. `IncludeSecondaryWindows(true)` where present (24H2+), so the shared app's menus and popups show | WGC from the picker; Windows draws the yellow border (no API to remove it) |
| **Monitor** | WGC from the picker, borderless as above. If WGC fails to start on a monitor, DD on the same output | Picker → mapped to a DXGI output (§7.5) → DD, no border. WGC with the border if the mapping fails |
| **Frame rate** | `MinUpdateInterval` = half the frame interval where present (24H2+, §7.3): DWM then delivers between fps and 2 × fps frames on any monitor (72 at 144 Hz, 82.5 at 165 Hz, 120 at 240 Hz) instead of every refresh, and the pacer thins them to fps (§8.4). Without it, WGC delivers up to the refresh rate (and about 60 fps on older builds) | WGC: as delivered; DD: one frame per present |
| **Cursor** | `IsCursorCaptureEnabled` | WGC: the same flag; DD: we draw the pointer (§7.8) |

The plan's "borderless on 24H2" is met by feature detection rather than build numbers: the engine sets
`IM_OS_WGC_BORDERLESS` only when the property exists and access is granted, and verifies the session reads back
`IsBorderRequired() == false`. Every borderless session sets `IM_SESS_NO_OS_INDICATOR`, and **so does every DD
session** (DD never shows a border). 03 then shows the always-on-top "Sharing" pill (plan "Share safety").

The engine's hard floor is build 19041 (WDA_EXCLUDEFROMCAPTURE, `IsCursorCaptureEnabled` and process loopback all
need it); `IM_OS_SUPPORTED` means build ≥ 19045, the plan's minimum. 03 enforces the product floor.

### 7.2 The picker

1. `im_pick_source` posts the pick to the **picker thread**, an STA with a message loop that the engine creates on
   first use. It never runs on the caller's thread: a blocked Wails UI thread would freeze the window for as long as
   the user takes, and the UI thread is an STA where `IAsyncOperation::get()` is not allowed.
2. The thread creates a `GraphicsCapturePicker`, queries `IInitializeWithWindow` and calls `Initialize(parent)`:
   `params.parent_window` (03 passes the main window's `HWND`), or, when it is `NULL` (tools), a hidden owner window
   of its own centered on the primary monitor.
3. `PickSingleItemAsync()`; the thread pumps messages (`MsgWaitForMultipleObjectsEx`) until the operation completes,
   `im_cancel_pick` or the timeout fires (both call `Cancel()` on the operation).
4. A null item is `IM_ERR_CANCELLED`. Otherwise the engine keeps the `GraphicsCaptureItem` under a new token
   `pick:<n>` and fills `im_source_info`: kind (a picked item doesn't say whether it is a window or a monitor, so
   the engine first tries §7.6's window match; when no window matches and the size equals a monitor's, it is that
   monitor; otherwise a window of unknown identity), `name` = `DisplayName()`, size = `Size()`, the capture path
   it will use (§7.1, §7.5), `IM_SRCF_OS_BORDER` when Windows will draw one, `pid` and `window` when §7.6 identified
   them, `IM_SRCF_SELF` when that window belongs to this process or its `msedgewebview2` children.
5. **Token rules**: the token is single use. A successful `im_start` consumes it; **a failed `im_start` leaves it
   valid** (the user can retry without picking again). It expires 5 min after the pick (counted from the pick, not
   from the last use); at most 4 unused tokens are kept, and a fifth pick evicts the oldest. `im_start` with a
   consumed, expired or evicted token returns `IM_ERR_SOURCE_GONE` with token `pick_expired`, which 01 maps to
   `source_gone` and 03 renders as "pick again" (back to the OS picker) rather than an error. `im_release_source`
   forgets a token at once.

The Windows picker can't be told to default to windows or monitors (plan: "the picker defaults to a window" is met
on macOS in M3); on Windows it shows both. An isshoni window, if the picker offers it at all, would capture as black
because of §11; a pick with `IM_SRCF_SELF` makes 03 ask the user to pick again (Go's `Pick` returns `invalid` with
token `self_window` and releases the token, §12.3).

**Verified on hardware in slice E10** with `enginetest pick` (the picker's own hidden owner window, step 2): the
picker is modal over its owner, cancel returns `IM_ERR_CANCELLED`, and a window and a monitor pick each return a
source. **With the Wails window as the parent** (03 passes its `HWND`), the check is M-APP-PICK (§18.4): the picker is
modal over isshoni's main window, the window keeps repainting, a pick goes live and is watched in a browser, cancel
returns to the share sheet, and a pick of an isshoni window gives `self_window`. It runs on the owner's PC once the
app shares through the engine (README's WA72, after 03's share sheet and tray slices); E10 can't run it because the
app's share path doesn't exist yet.

### 7.3 Windows.Graphics.Capture

- Device: the session's D3D11 device (§8.1), wrapped with `CreateDirect3D11DeviceFromDXGIDevice`.
- `Direct3D11CaptureFramePool::CreateFreeThreaded(device, B8G8R8A8UIntNormalized, 2, item.Size())`;
  `pool.CreateCaptureSession(item)`; `IsCursorCaptureEnabled(true)` unless `IM_START_HIDE_CURSOR`; borderless as in
  §7.1;
  `MinUpdateInterval(TimeSpan{10'000'000 / (2 × fps)})` (8.33 ms at 60 fps, from layer 0's fps; re-set on an fps
  re-init) and `IncludeSecondaryWindows(true)` where present (checked with `ApiInformation::IsPropertyPresent`);
  `StartCapture()`. **Why half the interval**: DWM delivers on the first vsync at least `MinUpdateInterval` after the
  previous frame, so exactly 1/fps quantizes to every third vsync at 144 Hz (≈ 48 fps) and 55 fps at 165 Hz; 0.8/fps
  or 1/fps minus half a refresh period still give 55 fps at 165 Hz (and the latter 50 fps at 100 Hz). Half the
  interval yields at least fps and at most 2 × fps on every refresh rate, needs no refresh rate (a window moved to
  another monitor keeps working), and the pacer's nominal schedule (§8.4) thins the surplus to exactly fps.
- `FrameArrived` (free-threaded): `TryGetNextFrame()`; if `ContentSize()` differs from the pool size, call
  `pool.Recreate(…, newSize)` and report the change to the supervisor (§7.7); copy the content rectangle into a free
  slot (§8.2) under the context lock; `capture_ts` = `SystemRelativeTime()` (QPC-based, 100 ns units) × 100; close
  the frame; signal the video thread.
- `item.Closed` → `SOURCE_GONE` (window closed, monitor removed).
- Protected content (DRM players) captures as black, on every path (plan); nothing to do.

### 7.4 DXGI Desktop Duplication

- The device must be on the adapter that owns the output (DD fails across adapters with `DXGI_ERROR_UNSUPPORTED`).
  §8.1 picks the encoder with that in mind.
- DD needs a per-monitor DPI-aware process, and physical coordinates. The Wails app's manifest declares PerMonitorV2
  (03); `isshoni-enginetest` has no manifest, so it calls `SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)` at
  startup. Every engine thread that touches monitors also calls
  `SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)` first. When the process isn't aware,
  `im_probe` clears `IM_OS_DESKTOP_DUPLICATION` (logged with token `dpi_unaware`) and `im_start` of a DD source fails
  with `IM_ERR_INTERNAL`, token `dpi_unaware` (a packaging bug, not a user state).
- `IDXGIOutput5::DuplicateOutput1(device, 0, 1, {DXGI_FORMAT_B8G8R8A8_UNORM})`. Asking for 8-bit only makes DXGI
  hand an HDR desktop over converted to SDR (§7.9).
- Loop: `AcquireNextFrame(timeout)` with the timeout = time to the pacer's next deadline, at most 100 ms.
  - `LastPresentTime == 0`: only the pointer moved; update the cursor state (§7.8).
  - Otherwise: `CopyResource` of the desktop texture into a free slot; `capture_ts` = `LastPresentTime` (QPC ticks)
    in ns by §12.3's formula; rotation from `DXGI_OUTDUPL_DESC.Rotation` (§7.9).
  - `ReleaseFrame()` right after the copy.
- `DXGI_ERROR_ACCESS_LOST` (mode change, secure desktop such as UAC or Ctrl+Alt+Del, lock screen, full-screen
  exclusive app switching modes): `CAPTURE_INTERRUPTED`; the pacer keeps repeating the last frame at the refresh
  cadence; re-create the duplication every 250 ms; on success `CAPTURE_RESUMED` and, if the size changed, the resize
  path. If the output is gone from `EnumOutputs` for 5 s: `SOURCE_GONE`.
- `DXGI_ERROR_WAIT_TIMEOUT` is normal (static screen).

### 7.5 Windows 10: mapping a picked monitor to a DXGI output

The picker returns a `GraphicsCaptureItem` with no `HMONITOR`. To capture it with DD:
1. Enumerate outputs (`EnumAdapters1` → `EnumOutputs` → `DXGI_OUTPUT_DESC`: `DeviceName`, `DesktopCoordinates`,
   `Monitor`, `Rotation`).
2. Candidates: outputs whose desktop size equals `item.Size()`.
3. One candidate: done. Several: keep those whose `DeviceName` or `DISPLAYCONFIG_TARGET_DEVICE_NAME` friendly name
   equals `item.DisplayName()`. One left: done.
4. **Conditional, not built in E11**: a frame match for identical monitors (a WGC frame of the item and one DD frame
   per candidate, downscaled to 64×36 luma on the GPU, the best sum of absolute differences winning if below half the
   second best). It is needed only if step 3 can leave several candidates, which depends on what `DisplayName()`
   returns for a monitor on Win10 22H2 (undocumented).
5. Otherwise: no mapping; capture with WGC (border shown) and log at info.

The pick reports the result (`capture_path = IM_CAPTURE_DD`, `IM_SRCF_MAPPED`, `monitor_id`). **M2 ships steps 1–3
and 5** (slice E11). E11 records on the owner's PC what `DisplayName()` returns for each monitor on Win10 22H2 (two
outputs if available, ideally two of the same model) next to each output's `DeviceName` and friendly name:
- **Names unique** (a device name such as `\\.\DISPLAY2`, or a string step 3 can match one-to-one): step 3 resolves
  every case; step 4 moves to M5 (§16) and only covers exotic setups.
- **Names ambiguous for identical monitors** (e.g. only the model name): E11 records it here, and a fix slice (`WF…`,
  README §5) adds step 4 in M2, because otherwise users with two identical monitors lose the plan's borderless Win10
  monitor capture (they'd get WGC with the border).

Unit tests (`monitor_map_test`) cover steps 2, 3 and 5 with fake outputs, including two identical monitors falling
through to step 5.

### 7.6 Window identity, minimized windows, isshoni's own windows

A picked window item carries no `HWND`. The engine identifies it once at pick time: enumerate visible top-level
windows (`EnumWindows`, skipping cloaked ones via `DWMWA_CLOAKED`) whose title equals `DisplayName()` and whose
`DWMWA_EXTENDED_FRAME_BOUNDS` size equals `item.Size()` within 2 pixels. Exactly one match gives `window` and `pid`
(`GetWindowThreadProcessId`); otherwise both stay 0. **Frame-hosted apps**: when the matched window's process image
(`QueryFullProcessImageNameW`, base name compared case-insensitively) is `applicationframehost.exe` (UWP and other
frame-hosted apps; 05 §5.4 lists it as a launcher, `IM_RULE_STOP`), `pid` is taken from its child
`FindWindowExW(hwnd, nullptr, L"Windows.UI.Core.CoreWindow", nullptr)` with `GetWindowThreadProcessId`; with no such
child, `pid` is 0 (`window` stays the frame's). The identity-matcher unit tests cover both cases. Uses (all local):
- **05**: the shared app is never auto-excluded from audio, and gets the plan's warning. The pick's `pid` reaches 05
  only through Go: `media.Source.PID` → `audio.Service.OnShareStarted(pid, withAudio)` → `im_exclusions.shared_pid`,
  via `im_set_exclusions` or `im_start_config.exclusions`. The engine passes nothing to the mixer itself, so there is
  one path and one value (05's audiotest case `shared-app-mic` sets it for an audio-only session with no pick).
- **Minimized**: the supervisor polls `IsIconic(window)` every 500 ms and sends `SOURCE_MINIMIZED` / `SOURCE_RESTORED`.
  WGC delivers no frames while a window is minimized, so viewers keep the last picture.
- **Self**: `IM_SRCF_SELF` (§7.2).

Unidentified windows simply lack these extras; capture is unaffected.

### 7.7 Source changes

| Change | Detection | Handling |
|---|---|---|
| Window resized | WGC `ContentSize()` | Recreate the frame pool at the new size at once. Until the size has been stable for 1 s, the VideoProcessor fits the new content into the current output size (aspect kept, black bars). Then, if the output size computed for the new source (§8.3) differs, re-init the encoders (`REINIT\|KEYFRAME`); `SOURCE_RESIZED` goes out when the change is first seen |
| Monitor mode change | DD `ACCESS_LOST` then a new size; WGC `ContentSize()` | The same path |
| Window minimized | §7.6 | Last frame stays; event |
| Window closed, monitor unplugged | `item.Closed`; DD output gone for 5 s | `SOURCE_GONE` → `ENDED` |
| Secure desktop, UAC, lock screen | DD `ACCESS_LOST`; WGC keeps its last frame | §7.4; no frames leave meanwhile except refreshes of the last one |
| Full-screen exclusive game | WGC: may capture black or the desktop on older games; DD on Win10: mode switches cause `ACCESS_LOST` | Documented limitation (troubleshooting page, 06); most current games use borderless or flip-model presentation, which both paths capture |

### 7.8 Cursor

- The pointer is drawn unless `IM_START_HIDE_CURSOR` is set, so a zero config (what 01's `media.Config` gives) shows
  it.
- WGC: `IsCursorCaptureEnabled(true)`; `IM_START_HIDE_CURSOR` sets it to false.
- DD: the desktop image has no pointer. The DD thread keeps the pointer position (`PointerPosition`, visible flag)
  and, when `PointerShapeBufferSize > 0`, the shape (`GetFramePointerShape`). Shapes are converted on the CPU to a
  BGRA texture with alpha: color shapes as they are; masked-color shapes with the mask bit → inverted pixels drawn as
  opaque black or white by luminance; monochrome shapes (AND/XOR) → black, white, transparent, and "invert" drawn as
  black with a white outline. The video thread blends the pointer into the slot copy with a textured quad (alpha
  blending, precompiled shaders) before the VideoProcessor, at the pointer's position adjusted for rotation. With
  `IM_START_HIDE_CURSOR` the DD thread skips the blend.
- The cursor is drawn into the copy, never into the desktop texture.

### 7.9 HDR, odd sizes, rotation, DPI, multiple monitors

- **HDR displays**: both paths request 8-bit BGRA, so Windows converts the desktop to SDR (with the user's SDR content
  brightness). HDR highlights clip. HDR sharing is later (plan). `IM_OS_HDR_DISPLAY` and the monitor's `hdr` flag let
  03 explain it. Verified on an HDR monitor where one is available, otherwise "community-tested".
- **Odd and tiny sizes**: windows can be any size (1001×751, 120×40). The source rectangle is used as is; the output
  is even-sized and at least 64×64 (§8.3), with the content letterboxed when it is smaller than the minimum.
- **Rotation**: DD reports `DXGI_MODE_ROTATION_*`; the VideoProcessor rotates (`VideoProcessorSetStreamRotation`), the
  shader path rotates its texture coordinates. WGC delivers monitor content already oriented.
- **DPI**: capture sizes are physical pixels on every path. `im_list_sources` reports bounds in physical pixels and
  the effective DPI per monitor, which 03 uses to place and size the pill on the shared monitor.
- **Multiple monitors**: one source is one monitor or one window. Capturing the whole virtual desktop is not offered
  (the picker doesn't offer it either). Mixed refresh rates don't matter: each session paces itself.

## 8. GPU path

### 8.1 Devices and adapters

- `D3D11CreateDevice(adapter, D3D_DRIVER_TYPE_UNKNOWN, …, D3D11_CREATE_DEVICE_BGRA_SUPPORT |
  D3D11_CREATE_DEVICE_VIDEO_SUPPORT, {11_1, 11_0})`. `ID3D10Multithread::SetMultithreadProtected(TRUE)` as a safety net.
- **One device per session**, on the adapter of the chosen encoder (§9.1): for DD that is the output's adapter by
  construction; for WGC any adapter works (WGC copies across adapters itself). On an Optimus or hybrid laptop the
  internal panel is on the iGPU, whose QSV/AMF encoder then wins; that keeps the path zero-copy.
- **The context lock**: a `CRITICAL_SECTION` (recursive, like FFmpeg's own default D3D11 lock, because FFmpeg may
  take it again while holding it) around every use of the immediate context, by the capture copy, the video thread
  and FFmpeg. The FFmpeg D3D11 device context gets the same lock through `AVD3D11VADeviceContext.lock`/`unlock`
  (§9.2), so encoder submissions and our copies never interleave on the context.
- **Device loss**: any `DXGI_ERROR_DEVICE_REMOVED`/`_RESET`, or an encoder error with
  `GetDeviceRemovedReason() != S_OK`, ends the session (§9.8).

### 8.2 Frame slots

Three BGRA textures of the source size (`D3D11_BIND_SHADER_RESOURCE | D3D11_BIND_RENDER_TARGET`) form a lock-free
triple buffer: the capture side writes a slot that is neither the latest nor the one being read, then publishes it
as the latest with a sequence number; the video thread takes the latest. Both share one immediate context, so the
copy is ordered before the reads without fences. A capture that arrives while the video thread still reads the only
free slot overwrites the previous unread frame (the newest frame wins).

### 8.3 Color conversion and scaling

- **Output size per layer** (computed at start and after every stable resize):
  `s = min(1, sqrt(max_pixels / (sw·sh)) if max_pixels, width/sw if width, height/sh if height)`;
  `out = (floor_even(sw·s), floor_even(sh·s))`, then each side clamped to [64, 4096] with the content letterboxed if
  clamping changed the aspect. Examples: 2560×1440 with 2 073 600 → 1920×1080; 3440×1440 → 2224×930; preview
  230 400 of 1920×1080 → 640×360; a 120×40 window → 120×64 with bars.
- **VideoProcessor** (primary): one `ID3D11VideoProcessor` per layer, input BGRA at the source size, output NV12 at
  the layer size. Color: `ID3D11VideoContext1::VideoProcessorSetStreamColorSpace1(DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709)`
  and `VideoProcessorSetOutputColorSpace1(DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709)`: BT.709, limited range. Auto
  processing off (`VideoProcessorSetStreamAutoProcessingMode(FALSE)`), so drivers add no "enhancement". Source
  rectangle = the content; destination rectangle = the letterboxed fit; background = black (Y 16, Cb/Cr 128).
- **Shader converter** (fallback and CI reference): two pixel-shader passes write the Y plane (`R8_UNORM` render
  target view) and the half-size CbCr plane (`R8G8_UNORM` view) of the NV12 texture, with a bilinear (preview: 2×2
  box then bilinear) filter and the same BT.709 limited matrix. Used when `CreateVideoProcessorEnumerator` or the
  first `VideoProcessorBlt` fails; WARP runs it in CI as the reference that the VideoProcessor output is compared to
  (max difference 2 code values on flat areas).
- **Encoder VUI**: every encoder signals BT.709 primaries, transfer and matrix with limited range (§9.3), so browsers
  don't fall back to BT.601 and shift colors.
- The output NV12 textures come from the encoder's hardware frame pool (§9.2); for CPU encoders from our own pool,
  read back through a staging texture (`CopyResource`, `Map` with `D3D11_MAP_READ`).

### 8.4 Pacer

Event-driven, per session, on the video thread; layer 0 sets the cadence, other layers follow their own interval:

```
interval[l] = 1e9 / fps[l]
next_due[l] = −∞                      until the layer's first encode (and after a re-init or reactivation)
wake on: new frame published | timer | command | stop
for each active layer l:
  due = now >= next_due[l] − 2 ms                       (2 ms tolerance for timer and vsync jitter)
  if new frame since last_encode[l] and due:           encode(latest, l); advance(l)
  elif force_idr[l] and (due or no new frame for one interval[0]):   encode(latest or last, l, IDR); advance(l)
  elif no new frame for 200 ms and refresh due[l]:     encode(last, l, REFRESH); advance(l)
arm the timer at the earliest next_due[l] − 2 ms of a layer with a pending new frame or IDR, else the next refresh

advance(l):                           the nominal schedule
  if now − next_due[l] > interval[l]:  next_due[l] = now + interval[l]      (more than one interval behind: resync)
  else:                                next_due[l] = next_due[l] + interval[l]
```

- A source at or below the target rate is encoded the moment a frame arrives (no tick alignment, no added latency):
  every frame finds its layer due.
- A faster source (DD on a 75–240 Hz monitor, WGC with `MinUpdateInterval` at 72–120 fps, §7.3) is thinned to the
  target by keeping the newest frame. Because `next_due` advances by exactly one interval per encode, independent of
  when the encode happened, the long-run rate is exactly the target. (Testing against `last_encode + interval − 2 ms`
  instead would restart the interval from each late encode: on a 75–144 Hz source that gives encodes every 14.67 ms,
  about 68 fps, 13 % above the rate the CBR budget assumes.) The resync rule keeps a source that resumes after a
  pause or a static stretch from being encoded in a catch-up burst.
- The preview layer reuses the same captured frame at its own 15 fps cadence.
- Capture timestamps pass through unchanged (they are monotonic per stream; a timestamp not after the previous one is
  bumped by 1 µs). A refresh or a static IDR is stamped with **now**, because RTP time must advance and the picture is
  current.
- **Overload**: when an encode takes longer than the interval, the next due frame is simply encoded later, so the frame
  rate drops by itself. When `fpsOut` stays below 90 % of `min(target, fpsIn)` for 3 s, `LIMIT_CHANGED` reports `cpu`
  (software encoder) or `gpu`; it returns to `none` after 10 s above 95 %.
- Timer: `CreateWaitableTimerExW(…, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, …)`; the thread waits on the frame event,
  the timer, the command event and the stop event (`WaitForMultipleObjects`).

### 8.5 Static screens, refresh, keyframes and pause

- **Keyframe request on a static screen**: re-encode the last NV12 frame of that layer as an IDR at the next allowed
  instant (≤ one frame interval, and the 250 ms spacing of §6.4). The plan's "last frame re-encoded on PLI".
- **Refresh**: 200 ms after the last change, then every 1 s, re-encode the last frame without forcing a type
  (`REFRESH`). CBR encoders spend the bits on quality, so text sharpens within a frame or two of stopping.
- **No periodic IDR while static** (§9.4).
- **Pause** (`im_set_paused(1)`): from the next frame interval on, the pacer ignores captured frames (capture keeps
  running so resume is instant), encodes only refreshes of the frozen last frame, and answers keyframe requests with
  IDRs of it. Audio: `im_set_paused(s, p)` calls `Mixer::SetPaused(p)`; the effective mute is the user's mute
  (`im_audio_set_muted`, 05) OR pause, so un-pausing never clears a user's "Sound off". Muted audio fades out in
  10 ms and encoded digital silence keeps flowing every 20 ms on the unbroken grid (§10.3). No new screen content or
  captured sound leaves the engine while paused. Resume: the next captured frame is encoded at once.
- **Inactive layer** (`im_set_layer_active(l, 0)`, M1 layer pausing): no encodes at all for that layer; its first
  frame after reactivation is an IDR (`KEYFRAME`, no `REINIT`).

## 9. Encode

### 9.1 Encoders and the fallback order

| Kind | Through | Input | Zero copy | Bitrate change | Profiles | Notes |
|---|---|---|---|---|---|---|
| NVENC | libavcodec `h264_nvenc` | `AV_PIX_FMT_D3D11` (NV12) | yes | live | High, CB | GeForce allows 8 concurrent sessions per system, shared with OBS, the NVIDIA app, Discord… (≈ 4 isshoni shares) |
| QSV | libavcodec `h264_qsv` (libvpl) | `AV_PIX_FMT_QSV` frames mapped from our D3D11 frames | yes | live (encoder reset inside FFmpeg) | High, CB | |
| AMF | libavcodec `h264_amf` | `AV_PIX_FMT_D3D11` | yes | **re-init + IDR** (plan; direct control M5) | High, CB | |
| MF hardware | libavcodec `h264_mf`, `hw_encoding=1` | `AV_PIX_FMT_D3D11` when the build lists it as a hardware config, else NV12 in system memory | if listed | **re-init + IDR** | High, CB | Vendor MFTs (also covers GPUs FFmpeg has no native encoder for) |
| MF software | libavcodec `h264_mf`, `hw_encoding=0` | NV12 in system memory | no | **re-init + IDR** | High, CB | Microsoft's H.264 MFT, part of Windows. Capped at 1080p30. Absent on N editions without the Media Feature Pack |
| OpenH264 | Cisco's DLL, its C API | I420 in system memory | no | live (`ENCODER_OPTION_BITRATE`) | CB only | Only after the user turned it on (§9.9). Capped at 1080p30 |

**Order** (`IM_ENC_AUTO`; the probe's `encoders[]` lists it the same way):
1. The hardware vendor encoder (NVENC, QSV or AMF) on the **capture adapter**: the DD output's adapter, or for WGC the
   adapter of the monitor that holds most of the source (the primary monitor when unknown).
2. MF hardware on that adapter.
3. Vendor encoders, then MF hardware, on the other adapters (WGC only; DD needs the output's adapter, so for DD these
   come with a GPU → CPU → GPU copy and are only used when 1–2 failed).
4. MF software.
5. OpenH264, if enabled.

Only encoders whose probe status is `IM_OK` take part; an encoder that fails to open at start is skipped silently
(logged), and the next one tried. `im_start_config.encoder` can force one kind (enginetest, and a hidden setting of
03 for support cases); a forced encoder that fails is `IM_ERR_NO_ENCODER`, never a silent fallback.

### 9.2 libavcodec integration

- **Loading**: on first use the engine calls `LoadLibraryExW(<ffmpeg_dir>\avutil-<U>.dll)` then `avcodec-<C>.dll`, each
  with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32`, and resolves the ~40 functions it uses into a
  table (`encode/ffmpeg_api.h`). `<ffmpeg_dir>` is `im_init_params.ffmpeg_dir`, or the engine DLL's own directory
  when it is NULL (the installed layout, §13.3). `<U>` and `<C>` are the majors of the pinned FFmpeg build: CMake
  reads `native/ffmpeg/lock.json` (06 §4.1) and takes `avcodecMajor` and `avutilMajor` (integers) to compile the DLL
  names `avcodec-<C>.dll`/`avutil-<U>.dll` and the majors in; `tag` goes into `im_build_info`. Not import-linked:
  the engine loads, probes and captures audio when the FFmpeg DLLs are missing, damaged or blocked by policy
  (`IM_ERR_DEPENDENCY` with token `missing`, `version` or `blocked_by_policy` for `ERROR_ACCESS_DISABLED_BY_POLICY`,
  `STATUS_INVALID_IMAGE_HASH`).
- **Version check**: `avutil_version() >> 16 == LIBAVUTIL_VERSION_MAJOR`, `avcodec_version() >> 16 ==
  LIBAVCODEC_VERSION_MAJOR`, and the minors at least the ones built against; struct layouts are stable within a major.
  **No hash check**: the LGPL lets users replace the library, and a same-major build of their own must keep working.
- **Hardware device**: `av_hwdevice_ctx_alloc(AV_HWDEVICE_TYPE_D3D11VA)` wrapping the session's device
  (`AVD3D11VADeviceContext.device`, `device_context`, and `lock`/`unlock`/`lock_ctx` = our context lock), then
  `av_hwdevice_ctx_init`. QSV: `av_hwdevice_ctx_create_derived(AV_HWDEVICE_TYPE_QSV)` from it.
- **Frames**: `av_hwframe_ctx_alloc` with `format = AV_PIX_FMT_D3D11`, `sw_format = AV_PIX_FMT_NV12`, the layer
  size, `initial_pool_size` = 8 (`f`) / 6 (`q`), `AVD3D11VAFramesContext.BindFlags = D3D11_BIND_RENDER_TARGET` (plus
  `D3D11_BIND_VIDEO_ENCODER` for MF, which needs it). The VideoProcessor writes into the pool's texture array slice of
  a frame from `av_hwframe_get_buffer`. QSV: `av_hwframe_ctx_create_derived(AV_PIX_FMT_QSV, …,
  AV_HWFRAME_MAP_DIRECT)` and `av_hwframe_map` per frame.
- **Zero copy decision**: an encoder is zero-copy when `avcodec_get_hw_config(codec, i)` lists `AV_PIX_FMT_D3D11` (or
  `AV_PIX_FMT_QSV`) with `AV_CODEC_HW_CONFIG_METHOD_HW_FRAMES_CTX`. Otherwise the NV12 frame is read back through a
  staging texture into an `AVFrame` in system memory. Whether `h264_mf` in the pinned build takes D3D11 frames is
  recorded by the probe (`IM_ENCF_ZERO_COPY`), not assumed.
- **Encode loop**: `avcodec_send_frame` then `avcodec_receive_packet` until `EAGAIN`. `AVFrame.pts` = capture time in
  µs since `epoch_ns` (`time_base = 1/1000000`); a small table maps packet `pts` back to the capture time, so encoders
  with one frame of delay (some MF and QSV configurations) keep correct timestamps. A forced IDR sets
  `frame->pict_type = AV_PICTURE_TYPE_I` with the encoder's forced-IDR option on (table below).
- **Logging**: `av_log_set_callback` routes FFmpeg's messages at warning and above into the engine log ring (FFmpeg
  calls it from encoder threads; the callback formats into a thread-local buffer and pushes without blocking).

### 9.3 Per-encoder settings

Common `AVCodecContext` fields: `width`/`height` = layer size; `time_base = {1, 1000000}`; `framerate = {fps, 1}`;
`max_b_frames = 0` (**required**); `gop_size = fps × 3600` (the engine forces IDRs itself, §9.4); `bit_rate = target`;
`rc_max_rate = target`; `rc_buffer_size = target × vbv` with `vbv` = 0.5 s for layer 0 (1.0 s for `DETAIL`) and 1.0 s
for other layers; `flags |= AV_CODEC_FLAG_LOW_DELAY` and **no** `AV_CODEC_FLAG_GLOBAL_HEADER` (headers stay in-band);
`color_range = AVCOL_RANGE_MPEG`, `colorspace = AVCOL_SPC_BT709`, `color_primaries = AVCOL_PRI_BT709`,
`color_trc = AVCOL_TRC_BT709`; `profile` from the session profile; `thread_count = 1` for hardware encoders.

Encoder options (`av_opt_set` on `priv_data`). **R** = required: the open fails without it. Every other option that
the build doesn't know is skipped with a debug log, so an FFmpeg patch release that renames one degrades instead of
breaking.

| Encoder | Options (Auto / Motion) | `DETAIL` changes | Profile |
|---|---|---|---|
| NVENC | `preset=p4`, `tune=ll`, `rc=cbr`, `zerolatency=1` (R), `delay=0`, `rc-lookahead=0`, `forced-idr=1` (R), `no-scenecut=1`, `multipass=disabled`, `b_ref_mode=disabled`, `aud=0` | `spatial-aq=1` | `profile=high` / `profile=baseline` (R) |
| QSV | `preset=veryfast`, `look_ahead=0`, `async_depth=1` (R), `forced_idr=1` (R), `scenario=displayremoting`, `adaptive_i=0`, `adaptive_b=0`, `idr_interval=0` | — | `profile=high` / `profile=baseline` |
| AMF | `usage=ultralowlatency` (R), `quality=speed`, `rc=cbr`, `enforce_hrd=1`, `filler_data=0`, `frame_skipping=0`, `preencode=0`, `forced_idr=1`, `header_insertion_mode=idr` | `vbaq=1`, `quality=balanced` | `profile=high` / `profile=constrained_baseline` |
| MF (hw and sw) | `hw_encoding=1` or `0` (R), `scenario=display_remoting` (R), `rate_control=cbr` | — | `AVCodecContext.profile` = `AV_PROFILE_H264_HIGH` / `AV_PROFILE_H264_CONSTRAINED_BASELINE` |
| OpenH264 (C API) | `SEncParamExt`: `iUsageType=SCREEN_CONTENT_REAL_TIME`, `iRCMode=RC_BITRATE_MODE`, `fMaxFrameRate=fps`, `iTargetBitrate`=`iMaxBitrate`=target, `uiIntraPeriod=0`, `eSpsPpsIdStrategy=CONSTANT_ID`, `iEntropyCodingModeFlag=0`, `bEnableFrameSkip=true`, `iMultipleThreadIdc=min(4, cores)` with 4 fixed slices, `iComplexityMode=MEDIUM_COMPLEXITY` | — | always CB |

These numbers are the starting point; slice E7 measures them on the owner's PC and records the result in its PR, and
a write-back slice after its group copies any change here (README §5, shared files). **Layer 0's VBV** is one of
them: E7 compares `vbv` = 0.5 s against 0.1–0.2 s on each available hardware encoder (frame-size p99 of `f`, 01
§12.2's g2g p95 of the `gate` run, with sound on and content cuts, and text quality after the static refresh of §9.4)
and keeps the smallest value that loses no text quality; 01 §12.1 lever 3 relies on it. Preset tuning beyond this is
M5.

### 9.4 GOP, IDR, SPS/PPS and the access-unit normalizer

- **Time-based GOP**: the engine forces an IDR on a layer when `now − last_idr ≥ gop_ms` **and** the content changed
  since that IDR. A static screen gets no periodic IDR: new viewers and losses ask for keyframes through PLI anyway
  (M1 02 §9.7), and a 300 KB IDR every 4 s would cost about 0.6 Mbps for nothing.
- **Forced IDRs**: on `im_request_keyframe`, on `ring full` recovery (§6.2), on layer reactivation, after a stale drop,
  and as the first frame after every (re)open. Requests coalesce into one IDR per frame and at most one per 250 ms
  per layer.
- **The normalizer** (`common/h264/annexb`) runs on every access unit before it enters the ring, whatever the encoder:
  - splits NAL units on 3- and 4-byte start codes and re-emits them with 4-byte start codes;
  - drops access unit delimiters (9), end of sequence (10), end of stream (11), filler (12) and SEI (6);
  - keeps the latest SPS and PPS; **an IDR access unit always goes out as SPS, PPS, then the slices**: missing headers
    are inserted from the cache, duplicates removed (the plan's "SPS/PPS in-band before every IDR", for every encoder);
  - parses the SPS (`common/h264/sps`): profile_idc, constraint flags, level, size after cropping; these fill
    `im_packet.profile`, `width` and `height`;
  - for a CB session, rewrites the SPS constraint byte to `0xE0` (constraint_set0/1/2), so the stream's key is
    `42e0` like the SFU's table expects (02 §8.2 of M1). Hardware and OpenH264 baseline streams use no FMO, ASO or
    redundant slices, which is what those flags promise. The byte sits right after profile_idc and before level_idc;
    changing it can't create an emulation-prevention sequence, and the normalizer checks that anyway;
  - checks that a High session's SPS says profile_idc 100; an encoder that emits anything else is closed and the next
    one tried (`ENCODER_CHANGED`, reason `encode_error`);
  - `KEYFRAME` = the access unit contains an IDR slice (type 5). The encoder's own key flag is only cross-checked.
- The normalizer is fuzzed (§18) and idempotent.

### 9.5 Rate control: live changes versus re-init

Go owns the policy (01: the server's `quality.hint{maxBitrate}` applied with hysteresis, changes of at least 15 %,
at most every 2 s, and for re-init encoders only changes of 30 % or more). The engine applies what it is told and
guards the expensive case:
- **Live** (`IM_ENCF_LIVE_BITRATE`: NVENC, QSV, OpenH264, unless the probe cleared it): the new `bit_rate`,
  `rc_max_rate` and `rc_buffer_size` are set before the next `avcodec_send_frame` (FFmpeg reconfigures NVENC through
  `NvEncReconfigureEncoder` and resets QSV); OpenH264 gets `SetOption(ENCODER_OPTION_BITRATE)`. Takes effect within a frame or two; no IDR.
  **"No IDR" is checked, not assumed**: QSV's change is an encoder reset inside FFmpeg (`MFXVideoENCODE_Reset`), and
  whether that starts a new sequence depends on the runtime and the HRD settings. The full probe (§9.10 step 3)
  verifies it for every encoder flagged live and clears `IM_ENCF_LIVE_BITRATE` for one that emits an IDR, which
  makes it a re-init encoder (01 D10 already branches on the flag); `capture -bitrate-steps` (§14) asserts the same
  on real content in E7.
- **Re-init** (AMF, MF): close and reopen the `AVCodecContext` with the new rate (the hardware frame pool is kept),
  then an IDR with `REINIT`. **At most once per 10 s per layer** (the plan's rule): a request inside the window is
  stored and applied when it ends, the latest value winning. A re-init takes 50–300 ms, during which that layer
  sends nothing.
- `im_set_bitrate` with the current value is a no-op. Values outside §6.4's range are `IM_ERR_INVALID_ARG`.
- `im_update_layer` (new `max_pixels`, `width`/`height` or `fps`) always re-inits with an IDR, at most once per
  second per layer (coalesced); a bitrate in the same call is applied with it. In M2 01 changes only `fps` this way
  (a preset change, e.g. Text 30); resolution stays fixed per share (01 D11; adaptation is M5).
- `im_set_content_hint(s, hint)` (a preset change: Auto → `AUTO`, Game and Movie → `MOTION`, Text → `DETAIL`)
  re-opens every layer with the new tuning of §9.3 (spatial AQ, AMF quality, the 1.0 s VBV of `DETAIL`) and emits
  `REINIT|KEYFRAME`. It is coalesced with the other size, fps, profile and content re-inits, which are spaced at
  least 1 s apart per layer; the same hint again is a no-op.

### 9.6 Profiles

- `im_start_config.profile` is the room's codec policy from `ShareParams.codec` (01 maps `h264/6400` → `0x6400`).
- **All layers of a session carry the same profile**: the layers share one transceiver and payload type. Each layer
  takes the first encoder in the order of §9.1 that supports the session profile. If the only encoder that opens for
  some layer is CB-only (OpenH264), the session profile becomes CB for every layer, and `im_session_info` says so;
  `im_start` never returns a mixed session. 01 offers the profile the session reports (`media.Event.Profile` of the
  `EventLayer` events, from `im_session_info.layers[].profile`, §12.3).
- `im_set_profile(s, p)`: every layer re-opens with the new profile and emits `REINIT|KEYFRAME` (the M1 publisher
  sends that keyframe on the new payload type, M1 02 §15.1). Asking for High when the encoders can't do it leaves the
  session at CB and returns `IM_OK`; the packets' `profile` tells 01 the truth.

### 9.7 Two layers, session limits, layer fallback

- Each layer is its own encoder session (`f` and `q`), fed from the same captured frame (§8.4).
- When layer 0's encoder opens but a further layer's session doesn't (NVENC's session limit, an encoder out of
  memory), that layer takes the next encoder in the order that supports the session profile (usually MF software:
  360p15 costs a few percent of one core) and the open is retried once after 500 ms if every option failed.
- **A layer never silently disappears**: if no encoder opens for a layer at `im_start`, the start fails with
  `IM_ERR_NO_ENCODER` (detail names the layer); if one fails mid-session with nothing left, the session ends with
  `IM_ERR_ENCODER_LOST`. A share with only one layer is a different offer (M1: no rids), which only 01 can make; 01
  may retry `im_start` with one layer after `IM_ERR_NO_ENCODER`.
- NVENC's limit is counted per system across processes, so it can't be predicted; the probe's NVENC status is a
  snapshot.

### 9.8 Device loss and encoder failure

| Failure | Engine | Events |
|---|---|---|
| `avcodec_open2` fails at start | next candidate (§9.1) | none (logged) |
| Encoder error mid-session (`avcodec_send_frame`/`receive_packet` < 0 and not `EAGAIN`), device fine | reopen the same encoder once; if that fails, the next candidate for that layer | `ENCODER_CHANGED` (when the kind changed), queued **before** the new encoder's first packet; packets `REINIT\|KEYFRAME` |
| Device removed or reset (TDR, driver update, GPU unplugged) | **ends the session** (M2): capture stops, the session's encoders, frame pools, VideoProcessors, WGC frame pool or DD duplication and device are released; the engine marks its probe cache stale. The next `im_start` re-enumerates adapters (the LUID may change after a driver update), creates a new device and re-probes the candidates it needs first (lazily); "Share again" is a new pick, since the ended session consumed its token (§7.2) | `ENDED{IM_ERR_ENCODER_LOST}`, detail token `device_removed`; 03 offers "Share again" (§5.7) |
| A layer's candidates exhausted | session ends | `ENDED{IM_ERR_ENCODER_LOST}` |

**Event order for a fallback**: the video thread queues `ENCODER_CHANGED` itself, before it writes the new encoder's
first `REINIT|KEYFRAME` packet (a fallback re-opens an encoder, so this is not a real-time moment; the event queue is
the session's MPSC queue, §6.2). Because `im_next_packet` returns events first, the binding always delivers
`EventEncoderChanged` before that packet's synthesized `EventLayer` (§12.3).

**Why no recovery in M2**: the plan's error rule asks only for "fall back to the next encoder" or "stop with a
notice"; rebuilding device, frame pool, VideoProcessors and encoders inside a live session (with retries and a
hardware TDR run) is a lot of code and owner's-PC time for a rare event that usually takes the game down with it.
Device-loss recovery with `IM_EVENT_DEVICE_RESET` (reserved in the header) is M5 (§16).

Audio is not affected by any of these (it has its own device handling, 05); an `ENDED` from device loss ends audio
with the session.

### 9.9 OpenH264 (runtime download, user-enabled)

- **Never shipped** in an installer (plan). The user turns it on in 03's settings (or from the `IM_ERR_NO_ENCODER`
  guided fix) after 03 shows Cisco's notice; the attribution "OpenH264 Video Codec provided by Cisco Systems, Inc."
  stays visible in About while it is installed, and turning it off deletes the file.
- **Pinned release**: one version per isshoni release, compiled into the engine (`IM_OPENH264_VERSION`, the headers
  in `native/windows/third_party/openh264/` match it) and into the Go binding (`native.OpenH264`, §12.5): version,
  URL, SHA-256 and byte size of the decompressed DLL. Today that is 2.6.0 from
  `http://ciscobinary.openh264.org/openh264-2.6.0-win64.dll.bz2` (Cisco's binary host; it serves HTTP, which is why
  the hash is pinned); slice E8 takes the newest Cisco release at that time, checks that the URL resolves (and uses
  HTTPS if the host offers it), and records the hash and the decompressed size.
- **Download and verify** (Go, `native.InstallOpenH264`), bounded before anything is trusted, because anyone on the
  network path controls the body of a plain-HTTP response (a small bzip2 bomb must not fill memory or the user's
  disk before the hash check):
  1. `GET` with a 60 s timeout; the **compressed** body is read through `io.LimitReader(body, 10 MiB + 1)`, and more
     than 10 MiB is an error.
  2. `compress/bzip2` decompresses it as a stream (bounded memory: a few MB for one block of at most 900 KB); the
     **decompressed** stream is read through `io.LimitReader(bz, OpenH264.Size + 1)`.
  3. It is copied with `io.Copy(io.MultiWriter(tmp, sha256.New()), …)` into a temp file created with `os.CreateTemp`
     in the target folder, so the hash is computed while streaming and at most `Size + 1` bytes are ever written.
  4. More than `Size` bytes, fewer than `Size`, or a hash that differs from `OpenH264.SHA256` is an error; only a match
     is closed and renamed to `%LOCALAPPDATA%\isshoni\codecs\openh264-<v>-win64.dll`. On **any** failure (network,
     limits, hash, rename) the temp file is removed (`defer`), so a failed install leaves nothing behind.

  Go verifies the size and hash again (`VerifyOpenH264`: `os.Stat` size first, then a streamed SHA-256) before every
  `im_set_openh264_path` (the file is user-writable). The engine loads it with `LoadLibraryExW(path, nullptr,
  LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32)` and requires `WelsGetCodecVersion()` to equal the
  pinned version.
- **Use**: `WelsCreateSVCEncoder`, `InitializeExt` (§9.3), `EncodeFrame` with I420 planes (the NV12 readback
  de-interleaved on the CPU, about 1 ms at 1080p), `ForceIntraFrame(true)` for IDRs, `WelsDestroySVCEncoder`. CI also
  uses its decoder (`WelsCreateDecoder`) to check our output (§18).

### 9.10 Probe

`im_probe(IM_PROBE_FULL)`, on the worker:
1. OS: `RtlGetVersion` (build), UBR from `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\UBR`, the `IM_OS_*` flags
   (`GraphicsCaptureSession::IsSupported`, `ApiInformation::IsPropertyPresent` per property,
   `RequestAccessAsync(Borderless)` result, a trial `DuplicateOutput1` per output, `MFStartup`, advanced color via
   `DisplayConfigGetDeviceInfo(DISPLAYCONFIG_DEVICE_INFO_GET_ADVANCED_COLOR_INFO)`).
2. Adapters: `IDXGIFactory6::EnumAdapterByGpuPreference`, up to 4, with vendor, device, `CheckInterfaceSupport` UMD
   version.
3. Encoders in the order of §9.1: for each, create a device on its adapter, open the encoder at 1920×1080 (or the
   adapter's maximum if smaller), encode one mid-gray NV12 frame as an IDR, check the output has SPS, PPS and an IDR
   slice, record `test_encode_us`, the flags (zero copy from the hardware config, live bitrate and forced IDR from
   the table of §9.1, High and CB from trial opens) and close. **Live-bitrate check** (§9.5), for every encoder the
   table marks live: after the test IDR, encode two P frames, apply a 50 % bitrate change exactly as §9.5 does,
   encode two more frames; any IDR among those two clears `IM_ENCF_LIVE_BITRATE` for that encoder (logged at info,
   `live_bitrate_idr`), so 01 and the engine treat it as a re-init encoder. It adds about 4 frames per live encoder
   (a few ms on hardware). An open that takes more than 1 s marks the encoder
   unavailable (token `slow_open`). NVENC's "driver too old for this API version" message maps to
   `IM_ERR_DEPENDENCY` with token `driver_too_old` (03 tells the user which driver version is needed, from §13.2).
   Every unavailable encoder's `status` and `reason` (§5.7 token) are written into its `im_encoder_info`, so Go and
   03 never parse log text.
4. Audio flags from 05's `AudioEngine::ProbeFlags()`, posted to 05's watcher (the worker does no WASAPI work).

The whole probe stays under 2 s on the owner's PC (acceptance in E7). 03 runs it once at startup in the background
and before the first share if it hasn't finished.

## 10. Audio in the engine (the boundary with 05)

The engine owns capture, mix and Opus (plan). **05 specifies and builds the watcher, capture and mix**, and the
semantics of the audio exports; this doc owns the header file, the session wiring, the Opus stage, the PCM tap and
the test tone.

### 10.1 The C ABI surface

The audio block of §5.2 is 05's §4.1 in this header's conventions (the differences are listed after §5.2): the
watcher (`im_audio_watch`, `im_audio_unwatch`), the rules (`im_rule`, `im_exclusions`, `im_set_exclusions` and
`im_start_config.exclusions`), the report (`im_list_audio_apps`), mode and mute (`im_audio_set_mode`,
`im_audio_set_muted`), levels (`im_audio_levels_get`), the sound check (`im_audio_sound_check`), `im_audio_config`
(mode, bitrate, frame length, flags) in the start config, the probe's `audio_flags`, `IM_AUDIO_STATE_*`, the packet
kinds `IM_PACKET_AUDIO` and `IM_PACKET_PCM`, and the three shipped test exports. 04's own audio calls are
`im_set_audio_bitrate` and `im_set_paused` (which pauses the mixer too). JSON schemas, rule semantics, modes 0–2 and
the report's states and events are 05's. Everything lands in v1.0 (`IM_ABI_MINOR 0`) in slice E1, so M2 needs no
minor bump; additions after M2 are minor bumps of this header that 05 asks for and this doc makes.

There are no audio events and no audio error codes: audio never fails a call, never ends a session and never makes
`im_next_packet` fail (05 §4.3).

### 10.2 The C++ boundary inside the DLL

```cpp
// native/common/audio/include/im/audio/pcm_sink.h  (04)
namespace im::audio {
inline constexpr uint32_t kRate = 48000, kChannels = 2, kChunkFrames = 480;   // 10 ms
class PcmSink {
 public:
  virtual ~PcmSink() = default;
  // Every 10 ms on the mixer thread: exactly kChunkFrames interleaved stereo float frames in [-1, 1], after the mix
  // bus (what friends hear, muted = zeros); captureNs = engine clock (QPC ns) of the first frame. Within one grid,
  // consecutive chunks are exactly 10 000 000 ns apart. Real-time safe: never blocks, never allocates.
  virtual void OnChunk(const float* frames, int64_t captureNs) noexcept = 0;
  // A new timestamp grid starts (first chunk, mixer resync, silence re-anchor; 05 §10.4): the next OnChunk's
  // captureNs may be any value. Device changes and engine switches keep the grid (05) and don't call it.
  virtual void OnGap(int64_t captureNs) noexcept = 0;
};
}

// native/windows/src/audio/mixer.h  (05; the shape 04 calls)
namespace im::audio {
struct MixerConfig {
  uint32_t mode;          // IM_AUDIO_MODE_EXCLUDE_APPS (EVERYTHING never reaches it in M2: the exports refuse it;
                          // OFF makes no mixer; TEST_TONE uses 04's tone)
  uint32_t flags;         // IM_AUDIOF_*
  PcmSink* sink;          // owned by the session, outlives the mixer
};
class Mixer {
 public:
  virtual ~Mixer() = default;
  virtual im_status Start(const MixerConfig&) = 0;  // on the worker inside im_start; <= 100 ms: 05's startup probe
                                                    // runs asynchronously (<= 600 ms, 05 §11.2)
  virtual void Stop() = 0;                          // <= 500 ms; no OnChunk after it returns
  virtual void SetPaused(bool paused) = 0;          // im_set_paused; effective mute = user mute OR paused
  virtual uint32_t State() const = 0;               // IM_AUDIO_STATE_* (im_session_info.audio_state, stats)
};
class AudioEngine {      // process-wide (05)
 public:
  static AudioEngine& Get();
  virtual im_status Init(uint32_t init_flags) = 0;          // im_init: starts 05's watcher (not with IM_INIT_NO_AUDIO)
  virtual void Shutdown() = 0;                              // im_shutdown; Init may follow
  virtual uint32_t ProbeFlags() = 0;                        // im_caps.audio_flags (runs on 05's watcher)
  virtual int32_t ListAppsJson(char* buf, int32_t cap) = 0; // im_list_audio_apps (snprintf convention)
  virtual im_status SetRules(const im_exclusions* ex) = 0;  // im_set_exclusions, im_start_config.exclusions; copies
  virtual im_status Watch(const im_exclusions* ex) = 0;     // im_audio_watch
  virtual im_status Unwatch() = 0;                          // im_audio_unwatch
  virtual im_status SetMode(uint32_t mode) = 0;             // im_audio_set_mode
  virtual im_status SetMuted(bool muted) = 0;               // im_audio_set_muted (the user's mute)
  virtual im_status LevelsGet(im_audio_levels* out) = 0;    // im_audio_levels_get
  virtual int32_t SoundCheck(uint32_t helper_pid, uint32_t timeout_ms, char* json, int32_t cap) = 0;
                                                            // im_audio_sound_check
  virtual std::unique_ptr<Mixer> NewMixer() = 0;
};
}
```

There is no event sink: 05 reports through `im_list_audio_apps` (`seq`, `state`, `events[]`) and `im_audio_levels_get`
(`report_seq`), which Go polls (05 §14.4). `api/exports.cpp` forwards the audio exports to `AudioEngine` after its
argument checks. In M2 those checks refuse `IM_AUDIO_MODE_EVERYTHING` in `im_start` and `im_audio_set_mode` with
`IM_ERR_INVALID_ARG` (README §3 item 15; the value stays reserved). `audio_bridge/` creates the session's `Mixer` in
`im_start` (for `audio.mode` EXCLUDE_APPS), calls `SetRules` first when `im_start_config.exclusions` is non-NULL, wires
the session's Opus stage as the sink, and maps `im_set_paused` to `Mixer::SetPaused`. 04's tests run against a fake
`AudioEngine` (§18.2).

Audio timestamps come from WASAPI's QPC positions (05), the same clock as video capture, which is what A/V sync rests
on (plan "A/V sync").

### 10.3 The Opus stage, the PCM tap and the test tone (04)

- **Opus** (`common/audio/opus_stage`), a `PcmSink`, with 05 §10.1's settings exactly: `opus_encoder_create(48000, 2,
  OPUS_APPLICATION_AUDIO)`, `OPUS_SET_BITRATE(cfg)` (0 → 128 000), `OPUS_SET_VBR(1)`, `OPUS_SET_VBR_CONSTRAINT(1)`,
  `OPUS_SET_COMPLEXITY(10)`, `OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC)`, `OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_FULLBAND)`,
  `OPUS_SET_INBAND_FEC(0)`, `OPUS_SET_PACKET_LOSS_PERC(0)`, `OPUS_SET_DTX(0)`, `OPUS_SET_LSB_DEPTH(24)`. FEC stays off
  because these bitrates stay in CELT, where in-band FEC does nothing (plan). Two chunks make one 20 ms frame
  (`opus_encode_float` into a 1275-byte buffer, ≤ 1 ms, inline on the mixer thread: no extra thread, no extra
  latency). `im_set_audio_bitrate` applies `OPUS_SET_BITRATE` before the next frame (live, no reset, no reinit).
- **Timestamps**: the lookahead `La` is read once with `OPUS_GET_LOOKAHEAD` (312 samples for this application). A
  packet's `capture_ts_ns` = its first chunk's `captureNs` − `La × 1 000 000 000 / 48 000`, so consecutive packets of
  one grid differ by exactly 20 000 000 ns (05 §10.3; 05's audiotest case 20 and the A/V budget rest on it).
- **Flags** (§5.2): the first packet of a session and the first packet after `OnGap` carry `IM_PKT_REINIT` (a new
  grid; the time step may be any value); `OnGap` drops a half-filled frame. Packets dropped because the ring was full
  or stale (§6.2) set `IM_PKT_DISCONTINUITY` on the next delivered one; the grid is unchanged, so Go's RTP timestamps
  keep the gap.
- **Start at `epoch_ns`**: the mixer starts inside `im_start` and 05 stamps chunks `L` in the past, with `L ≥ 0` (`L0` =
  50 ms at start, then the measured value, ≈ 50 ms; 05 §10.3), so the first chunks are older than `epoch_ns`. The Opus
  stage drops chunks while the packet they would start would be stamped before `epoch_ns`
  (`captureNs − La·1e9/48000 < epoch_ns`); the PCM tap drops chunks with `captureNs < epoch_ns`. The first audio packet
  is thus the first full 20 ms frame stamped at or after `epoch_ns` (at most about 100 ms of start-up sound is lost) and
  carries `IM_PKT_REINIT`. `capture_ts_ns ≥ epoch_ns` therefore holds for every packet kind, audio included (§5.2,
  §12.3, §19).
- **PCM tap** (`IM_AUDIOF_TAP_PCM`, 05's audiotest): in addition to Opus, each chunk becomes one `IM_PACKET_PCM` of
  480 stereo float32 frames (3840 bytes, `duration_us = 10000`, `layer = IM_NO_LAYER`) in the PCM ring of §6.2,
  stamped with the chunk's `captureNs` (no lookahead; 05 §10.3). 05's audiotest cases 20 and 21 compare the tap with
  the Opus stream of the same session, so both always flow together. There is no PCM-instead-of-Opus mode.
- **Mute and pause**: the mix bus is 05's. The user's mute (`im_audio_set_muted`) and pause (`im_set_paused` →
  `Mixer::SetPaused`) each fade the bus to zero in 10 ms (effective mute = user mute OR pause). The Opus stage keeps
  emitting encoded digital silence every 20 ms on the unbroken grid (05 §10.2), so the track never stalls and
  receivers never conceal a loss; the PCM tap shows digital zero meanwhile. No captured sound leaves the engine while
  muted or paused.
- **Test tone** (`IM_AUDIO_MODE_TEST_TONE`): a 04-owned source thread on a high-resolution waitable timer stands in
  for 05's mixer and feeds the same sink every 10 ms with the M1 fake's signal (440 Hz, a 1 kHz beep in the first
  100 ms of every second, the beeps starting at `epoch_ns + k × 1 s`), on an exact 10 ms QPC grid; it applies the
  same 10 ms fade for pause. For enginetest, Go integration tests on CI runners without audio devices, and the A/V
  checks. Under `IM_TEST_HOOKS` with `IM_ENC_FAKE`, the tone's Opus packets also carry M1's audio Marker, as
  `fake.Source`'s Opus packets do: the stage re-frames each packet as a code-3 packet (RFC 6716 §3.2.5) whose padding
  is the 18-byte Marker of `internal/media/fake/marker.go` (RID 0, `Frame` = packet index, `CaptureNS` =
  `capture_ts_ns − epoch_ns`, `Beep` on the first packet whose capture time is at or after each beep start).
  Decoders ignore padding.
- An audio-only session (`source_id = NULL`, `layer_count = 0`) is valid: 05's audiotest uses it. The first-run
  sound check is **not** a session: 05's `Service.SoundCheck` starts `isshoni.exe --sound-check-helper` and calls
  `im_audio_sound_check` (05 §11.3), which opens its own INCLUDE stream on the helper outside the policy. (The helper
  is an isshoni descendant, so the always-on `IM_RULE_INSTANCE(GetCurrentProcessId())` would keep it out of any
  session.)

## 11. Own windows (`WDA_EXCLUDEFROMCAPTURE`)

- **Who sets it in M2**: 03's `winos`, in Go, on the main window and the "Sharing" pill
  (`OSHooks.ExcludeFromCapture`, one `SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE)` call; 03 §6.1, §11.5,
  tested in 03 §22.2). The app does not call the engine for it: 03's depguard rule `desktop-no-engine-impl` keeps
  `internal/desktop/**` from importing `internal/media/native`, and one Win32 call needs no ABI. One owner, one path.
- **`im_protect_window` stays in ABI v1** for M3, where the macOS engine adds the window to the SCContentFilter's
  exclusions where a picker filter allows it (S2) and the call can't be one Win32 line in Go. On Windows it is
  implemented: `im_protect_window(hwnd, 0)` calls `SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE)` and
  verifies with `GetWindowDisplayAffinity`; `IM_PROTECT_OFF` sets `WDA_NONE`. It must be called from the process that
  owns the window (the engine is in-process). `IM_ERR_UNSUPPORTED_OS` before build 19041; `IM_ERR_INVALID_ARG` for a
  window of another process. In M2 its only caller is `enginetest protect-check` (Go `ProtectWindow`), which is
  also the engine-side proof that excluded windows vanish from both capture paths.
- What the engine guarantees for any excluded window, whoever set the affinity: it vanishes from WGC monitor captures
  and from DD alike (the content behind it shows), so friends never see the isshoni window, its viewer or the pill.
- The affinity applies to the top-level window and everything composed into it, which should include WebView2's
  child window owned by `msedgewebview2.exe`. **Verified on hardware in slice E12** (M-WDA with the app, checking the
  affinity 03's `winos` sets). **Note to 03**: popups that WebView2 itself creates as separate top-level windows (a
  native `<select>` dropdown, the context menu) belong to another process and can't be protected by anyone; the share
  UI should use custom dropdowns and disable the webview's context menu.
- The tray menu (a transient system menu window) is not protected; it is open for seconds and shows only isshoni's
  own items.
- Windows can't be captured individually by WGC once excluded: a user who picks an isshoni window gets
  `IM_SRCF_SELF` (§7.2).

## 12. The Go binding (`internal/media/native`)

04 owns the package. Its public surface implements 01's contract (01 §5): `*Engine` is a `media.Engine`, the session
is a `media.Session`, packets are `media.Packet`, and every error is a `*media.Error`. `*Engine` also implements 05's
`media.AudioController`. The C mirror types are unexported; the only exported types of the package are the
Windows-only extras of §12.2 and the OpenH264 helpers.

### 12.1 Files

| File | Builds on | What |
|---|---|---|
| `abi.go` | all | constants mirroring the header; the unexported C-layout mirrors (`imPacket`, `imStartConfig`, `imLayerConfig`, `imAudioConfig`, `imExclusions`, `imRule`, `imCaps`, `imEncoderInfo`, `imAdapterInfo`, `imSourceInfo`, `imPickParams`, `imSessionInfo`, `imLayerInfo`, `imClockSample`, `imInitParams`, `imAudioLevels`); the status and token → `*media.Error` mapping; the encoder token table (§5.6) |
| `bridge.go` | all | everything behind the `caller` interface (§12.4): the `Engine` and `session` logic, the snprintf retry, JSON decoding (build info, sources, stats, events), `media.Config` → `imStartConfig`, `imCaps` → `media.Probe`, the packet copy and validation rules, ENDED handling, the event mapping and `EventLayer` synthesis, the session state machine, the PCM tap, the clock cross-check |
| `types.go` | all | the exported Windows-only types: `BuildInfo`, `ClockSample`, `SourceDetail`, `PCMChunk` (the test tone is `media.AudioTestTone`, 05 §13.1; this package declares no mode constant) |
| `openh264.go` | all | `OpenH264` pin, `InstallOpenH264`, `VerifyOpenH264`, `RemoveOpenH264` (pure Go; the DLL only exists on Windows, but the code and tests run everywhere) |
| `engine_windows.go` | windows | `Open` (load, version, exports, `im_init`, clock check), `Close` (`im_shutdown`), the log drain |
| `call_windows.go` | windows | the `caller` over `syscall.SyscallN`, with `runtime.LockOSThread` around each call and its `im_last_error` read |
| `clock_windows.go` | windows | `qpcNS()` from `windows.QueryPerformanceCounter` and the cached `QueryPerformanceFrequency` (x/sys/windows) |
| `audio_windows.go` | windows | `media.AudioController` on `*Engine` (05 §13.1; **owned by 05**), over the same `caller` |
| `engine_other.go` | !windows | `Open` returns an error wrapping `media.ErrUnsupportedOS` (M3 adds `engine_darwin.go` with cgo, M4 `engine_linux.go`) |
| `layout_test.go` | all | sizes and offsets of §5.4 via `unsafe.Sizeof`/`Offsetof` against the golden file |
| `bridge_test.go` | all | the binding logic driven by a `fakeCaller` (§18.3) |
| `dll_windows_test.go` | windows | tests against a real DLL (`ISSHONI_TEST_ENGINE_DLL`, set by CI and enginetest only) |

### 12.2 API

```go
package native // import "github.com/MoonWX/isshoni/internal/media/native"

// Open loads the engine. On Windows: LoadLibraryEx(<Dir>\isshoni-media.dll,
// LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32); im_version (major 1, §5.5); every required export
// resolved; im_init; the clock cross-check (§12.3). A version or export problem is a *media.Error with Code
// internal and Reason "abi". Elsewhere it returns an error wrapping media.ErrUnsupportedOS.
func Open(o Options) (*Engine, error)

var (
	_ media.Engine          = (*Engine)(nil)
	_ media.AudioController = (*Engine)(nil) // audio_windows.go (05 §13.1)
	_ media.Session         = (*session)(nil)
)

type Options struct {
	Dir          string       // directory of isshoni-media.dll; "" = the directory of os.Executable()
	FFmpegDir    string       // "" = Dir (im_init_params.ffmpeg_dir = NULL)
	OpenH264Path string       // "" = none; must have passed VerifyOpenH264
	Logger       *slog.Logger // engine logs with component=engine; nil = slog.Default()
	LogLevel     slog.Level   // mapped to IM_LOG_*
	NoAudio      bool         // IM_INIT_NO_AUDIO
}

// media.Engine (01 §5.1). Safe for concurrent use.
func (e *Engine) Info() media.Info                                             // {"windows", 1, im_build_info().engine}
func (e *Engine) Probe(ctx context.Context, refresh bool) (media.Probe, error) // im_probe(FULL) first and on refresh
func (e *Engine) Sources(ctx context.Context) ([]media.Source, error)          // im_list_sources: monitors
func (e *Engine) Pick(ctx context.Context, r media.PickRequest) (media.Source, error) // ctx done → im_cancel_pick
func (e *Engine) AudioApps(ctx context.Context) ([]media.AudioApp, error)      // the apps of 05's report
func (e *Engine) Start(ctx context.Context, cfg media.Config) (media.Session, error) // ErrBusy while one runs
func (e *Engine) Close() error                                                 // stops sessions, im_shutdown; DLL stays
// Contract addition (01 §5.1).
func (e *Engine) Release(sourceID string)                                      // im_release_source

// media.AudioController (05 §13.1; audio_windows.go, owned by 05): WatchAudio → im_audio_watch, UnwatchAudio →
// im_audio_unwatch, SetExclusions(ctx, media.Exclusions) → im_set_exclusions, SetAudioMode → im_audio_set_mode,
// SetAudioMuted → im_audio_set_muted, AudioReport → im_list_audio_apps, AudioLevels → im_audio_levels_get,
// SoundCheck → im_audio_sound_check.

// Windows-only extras: not in the contract; 03 and the tools type-assert *native.Engine.
func (e *Engine) BuildInfo() BuildInfo                         // im_build_info (§5.6)
func (e *Engine) Clock() (ClockSample, error)                  // im_clock
func (e *Engine) SetOpenH264Path(path string) error            // verifies the hash first; "" disables
func (e *Engine) ProtectWindow(hwnd uintptr) error             // im_protect_window (§11); enginetest only (app: 03's winos)
func (e *Engine) SourceDetail(id string) (SourceDetail, bool)  // the im_source_info fields media.Source lacks
func (e *Engine) StartExtra(ctx context.Context, cfg media.Config) (media.Session, error) // enginetest only

type SourceDetail struct {
	MonitorID   string  // monitor sources: the im_list_sources id (03 places the pill on it); "" otherwise
	CapturePath string  // "wgc" | "dd" | "test"
	OSBorder    bool    // IM_SRCF_OS_BORDER: Windows draws its yellow border
	Mapped      bool    // IM_SRCF_MAPPED: a Win10 monitor pick mapped to a DD output
	Window      uintptr // the HWND when identified (§7.6), else 0
}

type PCMChunk struct {
	CaptureNS int64     // the chunk's first sample, on the clock of Session.Now
	Frames    []float32 // 960 values: 480 interleaved stereo frames
}

// The session is an unexported type returned as media.Session. Next is for one goroutine; every other method may be
// called from any goroutine, also while Next blocks.
func (s *session) Next(ctx context.Context) (media.Packet, error)
func (s *session) Now() int64                                     // qpcNS() − epoch_ns (§12.3)
func (s *session) RequestKeyframe(layer string)                   // im_request_keyframe; unknown layers ignored
// SetBitrate: video → im_set_bitrate; 0 → im_set_layer_active(l, 0), and the next non-zero value reactivates the
// layer (IDR); media.LayerAudio → im_set_audio_bitrate.
func (s *session) SetBitrate(layer string, bps int) error
func (s *session) Reconfigure(r media.Reconfig) error             // below
func (s *session) Stop() error                                    // §12.3
// Contract additions (01 §5.1).
func (s *session) SetLayerActive(layer string, active bool) error // im_set_layer_active
func (s *session) SetPaused(paused bool) error                    // im_set_paused (video frozen, Mixer::SetPaused)
func (s *session) Stats() (media.Stats, error)                    // im_get_stats (§5.6), decoded
// Windows-only extras (type assertion).
func (s *session) SetAudioBitrate(bps int) error                  // im_set_audio_bitrate
func (s *session) PCMTap() <-chan PCMChunk                        // buffered 128; nil unless AudioConfig.TapPCM
```

- **One session at a time**: `Start` returns `media.ErrBusy` while a session it started runs (01 §5.3, Conformance
  check 9). `StartExtra` skips that check (up to the DLL's 4 sessions; video only, since audio is one per process) so
  that `enginetest sessions` can open several; the app never calls it. (These two extras, `SourceDetail` and
  `StartExtra`, are the only additions beyond `BuildInfo`, `Clock`, `SetOpenH264Path` and `ProtectWindow`: 03 needs
  the monitor id for the pill, and the session-limit check needs several sessions.)
- **Audio lives on `*Engine`**: mute, mode, exclusions, levels and the report are process-wide in the ABI, so the
  session has no audio surface beyond `SetAudioBitrate` and `PCMTap`; 05's `audio.Service` calls the
  `media.AudioController` methods. There are no native audio types (`media.Exclusions`, `media.AudioConfig` and the
  report types are 05's). Should 01's final `media.Session` keep audio methods (01 §5.1 lists `SetExclusions` and
  `AudioStatus` today), the session forwards them to the engine's `AudioController` (`SetExclusions` →
  `im_set_exclusions`, `AudioStatus` → `AudioReport`), with no native types and no extra exports.
- **`PCMTap`**: `Next` never returns PCM; the binding copies every `IM_PACKET_PCM` into a `PCMChunk` and sends it
  without blocking (a full channel drops the chunk, counted and logged at warn at most once a second). The channel is
  closed when the session stops. `cmd/isshoni-audiotest` (05) type-asserts `interface{ PCMTap() <-chan
  native.PCMChunk }`.

**`media.Config` → `im_start_config`**

| `media.Config` | `im_start_config` |
|---|---|
| `Source` | `source_id`. A `mon:` or `test:` id adds `IM_START_ALLOW_LISTED_SOURCE`: the app (03) passes only `Pick` results (plan "OS pickers only"); listed ids serve Conformance, 01 §12.3 and enginetest |
| `Encoder` | `""` → `IM_ENC_AUTO` (what 01 always passes); a token of §5.6 → that kind (enginetest, 03's hidden support override) |
| `Profile` | `"6400"` → `IM_PROFILE_HIGH`, `"42e0"` → `IM_PROFILE_CB`, `"640c"` → `IM_PROFILE_HIGH` (the engine never sets constraint_set4/5, so packets and `EventLayer` say `6400`) |
| `Preset` | `content_hint`: `auto` → `AUTO`, `game` and `movie` → `MOTION`, `text` → `DETAIL` |
| `GOP` | `layers[0].gop_ms`; other layers 0 (default 2000) |
| `Layers[i]` | `layers[i]`: `rid`; `max_pixels` = `MaxPixels`; `fps` = `MaxFPS`; `bitrate_bps` = `Bitrate` (01's ratectl initial target, ≤ `maxBitrate`); `width`/`height` 0 |
| `Audio == nil` | `audio.mode` = `IM_AUDIO_MODE_OFF` |
| `Audio` (05's `media.AudioConfig`) | `audio.mode` from `Mode` through 05 §13.1's single mode mapping (`""` and `exclude-apps` → `EXCLUDE_APPS`, `media.AudioTestTone` → `TEST_TONE`; `everything` and `off` → `*media.Error{Code: invalid}` without a DLL call in M2); `bitrate_bps` = `BitrateBPS`; `flags`: `TapPCM` → `IM_AUDIOF_TAP_PCM`, `TestFlags` passed through (`IM_AUDIOF_TEST_FAIL_PROBE`); `exclusions` → an `imExclusions` built from `Exclusions` (rules in order, `KeepOutMicUsers` → `IM_EXCLF_KEEP_OUT_MIC_USERS`, `FastPath` → `IM_EXCLF_FAST_PATH`, `SharedPID`) |
| `Borderless == false` | `IM_START_KEEP_BORDER` |
| (nothing) | `IM_START_HIDE_CURSOR` is never set: shares show the pointer |

**`Reconfigure`** maps zero fields to "keep" and applies, in this order, returning the first error:
`Profile` → `im_set_profile`; `Preset` → `im_set_content_hint` (the mapping above); each `Layers[i]` (by `RID`) with
`MaxFPS` or `MaxPixels` set → `im_update_layer` with the layer's current config and those fields replaced (its
`Bitrate`, if set, goes in the same call), with only `Bitrate` set → `im_set_bitrate`; `AudioBitrate` →
`im_set_audio_bitrate`. The engine coalesces the resulting re-inits (§9.5).

**`im_caps` → `media.Probe`**: `OS` = `{"windows", "10.0.<os_build>", os_build}`. `Encoders`: the usable ones
(`status == IM_OK`) in the engine's order, then the unusable ones with `Tested = false` and `Reason` = their token;
`Name` = token, `HW` = `IM_ENCF_HARDWARE`, `Profiles` = `"6400"` if `IM_ENCF_HIGH` and `"42e0"` if `IM_ENCF_CB`,
`LiveBitrate` = `IM_ENCF_LIVE_BITRATE`, `MaxLayers` = 2 (NVENC's limit is per system and can't be probed, §9.7),
`Tested` = `IM_ENCF_AVAILABLE`. `Capture` = `{Picker: IM_OS_WGC_PICKER, Borderless: IM_OS_WGC_BORDERLESS,
MonitorList: IM_OS_DESKTOP_DUPLICATION, MaxFPS: 60}`. `Audio` =
`media.AudioCapsFromFlags(im_caps.audio_flags)` (01 §5.1: `ProcessLoopback`, `EndpointLoopback`, `SessionEvents`,
`MicDetection`, `Backend`), decoded in that one place so the binding and the fake agree; there is no `Mode` or
`Degraded` field (05's report carries banners).

**`Pick`**: `OwnerWindow` → `parent_window`; `Kinds` is ignored on Windows (the picker shows both, §7.2); the result
becomes `media.Source{ID, Kind: screen|window, Name, Width, Height, PID, Exe}` with `Exe` = the base name of
`QueryFullProcessImageNameW` for `PID` ("" when `PID` is 0 or the query fails). A pick with `IM_SRCF_SELF` is
released at once and returns `*media.Error{Code: invalid, Reason: "self_window"}` (03: "pick again"). `Sources`
returns each monitor as `media.Source{ID: "mon:…", Kind: screen, Name, Width, Height}`.

**Errors**: always `*media.Error{Code, Op, Detail, Reason}`. `Op` is the Go method (`"start"`, `"pick"`, `"next"`,
…), `Reason` the §5.7 token split off `im_last_error` (or the `ENDED` detail), `Detail` the rest, for logs only.

| Status | `Code` |
|---|---|
| `IM_ERR_PERMISSION_DENIED` | `permission_denied` |
| `IM_ERR_ENCODER_LOST` | `encoder_lost` |
| `IM_ERR_SOURCE_GONE` | `source_gone` (`pick_expired`: 03 shows "pick again") |
| `IM_ERR_UNSUPPORTED_OS` | `unsupported_os` |
| `IM_ERR_NO_ENCODER` | `no_encoder` (`Reason`: `open_failed`, `layer_failed`, `forced_failed`, …) |
| `IM_ERR_DEPENDENCY` | `dependency` (`Reason`: `missing`, `version`, `blocked_by_policy`, `driver_too_old`, `disabled`, `no_media_foundation`); 01 §5.2's table, README §3 item 17 |
| `IM_ERR_ABI` | `dependency` with `Reason` `abi` (01 §5.2) |
| `IM_ERR_CANCELLED` | `cancelled` |
| `IM_ERR_LIMIT` | `busy` |
| `IM_ERR_INVALID_ARG` | `invalid` (from `im_next_packet` during `Stop`: `stopped`, §12.3) |
| `IM_ERR_TIMEOUT` | `timeout`: 01's `CodeTimeout`, whose `Unwrap` returns `context.DeadlineExceeded`, so `errors.Is(err, context.DeadlineExceeded)` holds (there is no `native.ErrTimeout`: the package declares no sentinels of its own) |
| `IM_ERR_BAD_STATE` (except from an audio call, 05 §13.1), `IM_ERR_NOT_IMPLEMENTED`, `IM_ERR_INTERNAL`, `IM_ERR_AUDIO_*`, others | `internal` |

### 12.3 Next, events, the clock and the session state machine

**Clock.** `qpcNS()` = `q / f × 1e9 + (q % f) × 1e9 / f` with `q` = `QueryPerformanceCounter` and `f` = the cached
`QueryPerformanceFrequency` (integer arithmetic, no overflow). The engine converts DD's `LastPresentTime` and
WASAPI's QPC positions (05) with the same formula, and WGC's `SystemRelativeTime` (100 ns units derived from QPC) by
× 100, so Go's and the engine's readings agree to well under 1 µs. `Session.Now()` = `qpcNS() − epoch_ns`: the same
origin as `Packet.CaptureNS`, so 01's SR anchor `wall0 = time.Now() − (Now() − CaptureNS)` (01 §8.2) stays exact and
uses no absolute clock; `im_clock` is not used for SR mapping. **At `Open`** Go reads `qpcNS()` before and after
`im_clock`; `mono_ns` outside `[before − 1 ms, after + 1 ms]` fails `Open` with `internal` and a detail naming both
values (it would break A/V sync and the SR anchor).

**Next** pulls in a loop of `im_next_packet(s, 50, &pkt)` (pull timeout 50 ms):
- Before each call: after `Stop`, return `media.ErrStopped` without calling the DLL. On `IM_TIMEOUT`: return
  `ctx.Err()` if ctx is done, else loop.
- On `IM_OK`, by kind; every `CaptureNS` is `capture_ts_ns − epoch_ns`, ≥ 0 for every kind (§10.3):
  - `IM_PACKET_VIDEO` → `media.Packet{Kind: KindVideo, Layer: rid, Keyframe: KEYFRAME, Reinit: REINIT, CaptureNS,
    Data}`. A `REINIT` packet is preceded by the synthesized `EventLayer` (below): Next returns the event and keeps
    the copied packet for its next call.
  - `IM_PACKET_AUDIO` → `media.Packet{Kind: KindAudio, Reinit: REINIT (a new grid, 05 §10.4), CaptureNS, Data}`.
  - `IM_PACKET_PCM` → `PCMTap()` (§12.2); never returned; loop.
  - `IM_PACKET_EVENT` → `media.Packet{Kind: KindEvent, Event: &media.Event{…}}` per the table below, in
    `im_next_packet` order (events first, as the C ABI delivers them); unmapped events are logged and skipped.
    **`ENDED` is the exception**: its status becomes Next's error (`IM_OK` → `media.ErrStopped`, else the mapped
    `*media.Error`), the binding calls `im_stop` itself (≤ 2 s) to free the session, and later calls return
    `media.ErrStopped`. An `IM_ENDED` return without a preceding `ENDED` event (defensive) takes the same path with
    `internal`.
  - `IM_PACKET_DEBUG` (test hooks) → the test's luma-tap hook if set, else dropped.
- **Copy and limits**: `Data` is a fresh `[]byte` copied from native memory before the next DLL call (no pooling,
  01 §7.4). Video ≤ 8 MiB (the `f` arena), audio ≤ 4096 B, PCM exactly 3840 B, event JSON ≤ 1 KiB. A packet over its
  limit, with an invalid kind or layer, or with a negative `CaptureNS`, is dropped with a warn log, and for video
  `RequestKeyframe(layer)` follows. `DISCONTINUITY` and `REFRESH` have no `media.Packet` field: the engine already
  requests the IDR a video gap needs, and an audio gap keeps the grid (Go's RTP timestamps keep the gap).
- An `IM_ERR_INVALID_ARG` from `im_next_packet` racing a `Stop` maps to `media.ErrStopped`.

**Events**

| Engine | `media.Event` |
|---|---|
| (synthesized) after `im_start`, one per layer from its `im_session_info`, before any media; and before each video `REINIT` packet, for that layer, re-reading `im_get_session_info` | `EventLayer`: `Layer` = rid; `Width`, `Height`, `FPS`; `Encoder` = token (§5.6); `HW` = `IM_ENCF_HARDWARE`; `Profile` = `"6400"` or `"42e0"` from `layers[].profile`; `LiveBitrate` = `IM_ENCF_LIVE_BITRATE`; `NoOSIndicator` = `IM_SESS_NO_OS_INDICATOR` |
| `SOURCE_RESIZED` | `EventSourceResized{Width, Height}` (the `EventLayer` of the re-init follows) |
| `LIMIT_CHANGED` (`cpu`, `gpu` **and** `none`) | `EventLimit{Layer, Limit}`, every edge passed (the engine applies the hysteresis, §8.4): 01 feeds `ratectl.SetLimit` and the layer's `Limit` (01 §5.2), so the `none` edge is what releases the ramp hold and clears the CPU hint |
| `ENCODER_CHANGED` | `EventEncoderChanged{Layer, Encoder}` (`Encoder` = the `to` token), returned **before** the synthesized `EventLayer` of the new encoder's first `REINIT` packet (the engine queues it first, §9.8). 01 raises its `encoder_fallback` notice ("Switched to <encoder>") from it (01 §11.4) |
| `SOURCE_MINIMIZED`, `SOURCE_RESTORED` | `EventSourceMinimized`, `EventSourceRestored` (01 adds the two codes; 03 shows §5.7's line) |
| `CAPTURE_INTERRUPTED`, `CAPTURE_RESUMED` | `EventCaptureInterrupted`, `EventCaptureResumed` (01 logs them; no UI state, §5.7) |
| `DEVICE_RESET` | `EventDeviceReset` (log only; the M2 Windows engine never sends it, §9.8) |
| `RING_OVERFLOW` | not passed: logged by the binding at warn |
| `ENDED` | Next's error (above), never a `KindEvent` packet |

This is 01 §5.2's event table; there is no other mapping (no `EventOverload`).

**Stop and the session state machine**

```
            Start ok                 ENDED event (Next returns its error; binding calls im_stop)
  (none) ───────────► running ───────────────────────────────────────────────────► stopped
                         │ Stop(): set stopping, wait for the pull lock (≤ 50 ms + a copy),   ▲
                         └────── im_stop (≤ 2 s), close PCMTap ──────────────────────────────┘
```

- `Stop`: the first call runs `im_stop` (≤ 2 s) and returns its result; later calls return nil, and so does a `Stop`
  after the binding already ran `im_stop` for an `ENDED` (the handle is never passed to the DLL twice). The pump holds a
  per-session pull lock from each `im_next_packet` through its copy; `Stop` sets `stopping` first and takes the lock
  before `im_stop`, so the engine never frees a slot that Go is still copying. A blocked `Next` returns
  `media.ErrStopped` within 50 ms (its call times out and sees `stopping`), inside 01's 200 ms (01 §5.3).
- `Next` after `Stop` or after `ENDED` returns `media.ErrStopped` without calling the DLL.
- `Stats()` decodes §5.6's JSON. A background goroutine per `Engine` drains `im_read_logs` every second (and right
  after any error) into the logger.

### 12.4 Calling the DLL safely without cgo

```go
// bridge.go (no build tag)
type proc uint8 // index into the resolved export table: procVersion, procInit, …, procTestOpusDecode

type caller interface {
	// call runs one export with SyscallN semantics. When the result, read as int32, is negative it also returns
	// im_last_error read on the same locked OS thread.
	call(p proc, args ...uintptr) (r uintptr, lastErr string)
	qpcNS() int64
}
```

- `call_windows.go`: `windows.LoadLibraryEx` + `windows.GetProcAddress`; calls through `syscall.SyscallN` with
  `runtime.LockOSThread` held across the call **and** the `im_last_error` read (S1's `do`), because the detail is
  thread-local. The required exports are every `IM_API` function of v1.0, 05's audio exports and the test exports
  included; one missing: `Open` fails with `internal`, `Reason` `abi`, naming the export ("stale build?", as S1 did). `bridge_test.go`'s `fakeCaller` writes into the `imPacket` through the same pointer
  arithmetic and emulates timeouts, snprintf lengths, oversized and invalid packets, `ENDED`, fatal statuses, a
  `Stop` race and a clock offset, so the binding's logic runs on the Mac and CI ubuntu (01 §7.7).
- Structs passed in are allocated on the heap (`new`), with every string and array they point to (`im_start_config`
  → `source_id`, `exclusions` → `rules` → strings) built with `windows.BytePtrFromString` or `make`, and kept alive
  with `runtime.KeepAlive` until the call returns. The engine never keeps a pointer after a call (§5.1), so nothing
  needs pinning beyond the call. Pointer conversions are written inside the `SyscallN` argument list, which
  `go vet`'s `unsafeptr` check accepts.
- `im_packet.data` is a `uintptr` field in the Go mirror, so the GC never sees a pointer into native memory;
  `unsafe.Slice((*byte)(unsafe.Pointer(p.data)), p.size)` is copied before the next call (the one `unsafeptr`
  exception, with a comment).
- `float` arguments (`im_test_render_*`) are passed as `uintptr(math.Float32bits(x))`: Go's `asmstdcall` mirrors the
  first four integer argument registers into XMM0–3, and later float arguments are read from the low 4 bytes of their
  8-byte stack slot.
- JSON outputs: a 64 KiB buffer, one retry with `n + 1` bytes when the returned `n ≥ cap` (snprintf), at most 1 MiB,
  else `internal`.
- Session control calls go straight to the DLL (thread-safe, §5.1); only the pull lock orders `im_next_packet`, its
  copy and `im_stop`.
- `im_pick_source` and `im_start` block for seconds: they run on their own goroutine, never on Wails's UI thread
  (03's rule).
- No `FreeLibrary`, ever.

### 12.5 OpenH264 helpers

```go
type OpenH264Release struct {
	Version, URL string
	SHA256       string // hex SHA-256 of the decompressed DLL
	Size         int64  // byte size of the decompressed DLL: the decompression limit (§9.9)
	FileName     string
}
var OpenH264 = OpenH264Release{Version: "2.6.0", URL: "http://ciscobinary.openh264.org/openh264-2.6.0-win64.dll.bz2",
	SHA256: "<set in slice E8>", Size: 0 /* set in slice E8 */, FileName: "openh264-2.6.0-win64.dll"}

const maxOpenH264Compressed = 10 << 20 // the compressed body limit (§9.9)

// InstallOpenH264 downloads, bounds, hashes while streaming and renames into dir (§9.9 steps 1–4); on any failure
// nothing is left in dir.
func InstallOpenH264(ctx context.Context, c *http.Client, dir string) (path string, err error)
func VerifyOpenH264(path string) error // size, then SHA-256
func RemoveOpenH264(dir string) error
```
`dir` is `%LOCALAPPDATA%\isshoni\codecs` (03 passes it). The pin and the engine's compiled `IM_OPENH264_VERSION` must
agree; a unit test reads both.

## 13. Build, runtime layout, symbols, licenses

### 13.1 CMake, presets, vcpkg, flags

- **Root** `native/CMakeLists.txt` (CMake ≥ 3.28): `common/` always (including 05's `audiopolicy` and `audiomix`
  through `add_subdirectory`); `windows/` when `WIN32`; tests with `IM_BUILD_TESTS=ON`. Options: `IM_TEST_HOOKS`
  (OFF), `IM_FFMPEG_ROOT` (path to the unpacked FFmpeg build zip; only its `include/` is used at build time, its
  `bin/` only at run time), `IM_VERSION` (SemVer, default `0.0.0-dev`), `IM_WINVER` (four 16-bit numbers, default
  `0.0.0.0`, set by 06 from `tools/desktoppkg winver`, 06 §5.3), `IM_COMMIT`. CMake reads `native/ffmpeg/lock.json`
  (§9.2).
- **`native/CMakePresets.json`** (configure, build and test presets with the same names):

  | Preset | Host | Generator, config | What |
  |---|---|---|---|
  | `win-x64-release` | Windows | Ninja, RelWithDebInfo | the shipped DLL and PDB; `IM_TEST_HOOKS=OFF`; no `VCPKG_MANIFEST_FEATURES` (libopus and speexdsp only) |
  | `win-x64-debug` | Windows | Ninja, Debug | development |
  | `win-x64-ci` | Windows | Ninja, RelWithDebInfo | `IM_TEST_HOOKS=ON`, `IM_BUILD_TESTS=ON`, vcpkg feature `tests`: the DLL CI tests and Go's Windows tests load |
  | `win-x64-asan` | Windows | Ninja, Debug, `/fsanitize=address` | owner's PC and the nightly; feature `tests` |
  | `host-debug` | macOS, Linux | Ninja, Debug, clang or gcc | `common/` and its tests only; feature `tests` |
  | `host-tsan`, `host-asan` | macOS, Linux | clang | the same with ThreadSanitizer / AddressSanitizer+UBSan |
  | `host-fuzz` | macOS, Linux | clang `-fsanitize=fuzzer,address` | the fuzz targets |

  Windows presets expect VS 2022 17.14 or VS 2026 (vswhere range `[17.0,19.0)` with
  `Microsoft.VisualStudio.Component.VC.Tools.x86.x64`) and the exact toolset 14.44 from `deploy/windows/tools.json`
  `msvc.toolset` (06 §3; a bump is 06's hand-made tool PR), activated by `deploy/windows/msvc-env.ps1` in CI and on the
  PC (`task native:build` runs it), and Windows SDK 10.0.26100, whose C++/WinRT headers declare the 24H2 capture
  properties; every API newer than build 19041 is guarded at runtime (`ApiInformation`, `GetProcAddress`).
- **vcpkg** in manifest mode, `native/vcpkg.json`: dependencies `opus` (libopus, BSD-3-Clause) and `speexdsp`
  (BSD-3-Clause, 05's resampler), both static (`x64-windows-static`: static libs, static CRT) and linked into
  `isshoni-media.dll`; feature `tests` adds `gtest` (BSD-3-Clause, never shipped). `win-x64-release` sets no
  `VCPKG_MANIFEST_FEATURES`, so its port set is exactly libopus and speexdsp (06's port-set gate, 06 §9.3);
  `win-x64-ci`, `win-x64-asan` and the host presets add `tests`. `builtin-baseline` pins a vcpkg commit (set in E1).
  Elsewhere the default host triplet.
- **MSVC flags**: `/std:c++20 /W4 /WX /permissive- /utf-8 /EHsc /Zc:__cplusplus /Zc:preprocessor /sdl /guard:cf /Zi
  /Gy /Gw`, `/MT` (Release) or `/MTd` (Debug); defines `WIN32_LEAN_AND_MEAN NOMINMAX UNICODE _UNICODE
  _WIN32_WINNT=0x0A00 WINVER=0x0A00 IM_BUILDING_DLL`. Linker: `/DEBUG:FULL /OPT:REF /OPT:ICF /guard:cf /CETCOMPAT
  /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT /DEF:isshoni-media.def`, output `isshoni-media.dll` and `isshoni-media.pdb`.
  Libraries: `d3d11 dxgi dxguid user32 ole32 oleaut32 windowsapp dwmapi shcore synchronization delayimp`, plus 05's
  audio libraries. **Delay-loaded**: `mfplat.dll` (absent on N editions; the probe checks it with
  `__HrLoadAllImportsForDll` before any call) and `avrt.dll`. Any new system DLL import goes onto 06's
  `deploy/windows/dll-imports.txt` in the same PR.
- **Delay-load hook** (`src/engine/delayload.cpp`, M2, slice E2): the default helper loads a delay-loaded DLL with
  `LoadLibraryExA(name, NULL, 0)`, i.e. the default search order (application folder, System32, Windows, the
  **current directory**, `PATH`) unless the process called `SetDefaultDllDirectories`. On an N edition `mfplat.dll`
  is not in System32, so a planted copy in the current directory (inherited from the browser on a link launch) or a
  `PATH` entry would load into `isshoni.exe`. The DLL therefore defines `ExternC const PfnDliHook
  __pfnDliNotifyHook2 = imDelayHook;`, and on `dliNotePreLoadLibrary` the hook returns
  `LoadLibraryExA(pdli->szDll, nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32)`. When that fails it must **not** return
  NULL (NULL makes the helper fall back to its default search); it raises the helper's own exception,
  `RaiseException(VcppException(ERROR_SEVERITY_ERROR, ERROR_MOD_NOT_FOUND), 0, 1, (ULONG_PTR*)&pdli)`, which
  `__HrLoadAllImportsForDll` turns into a failure `HRESULT` (the probe then reports `no_media_foundation`). Every
  delay-loaded DLL is a System32 DLL, so the hook applies to all of them, and because it lives in the engine it
  protects every host process: the app, `isshoni-enginetest`, 05's `isshoni-audiotest` and 01's `tools/g2g`.
  `/DEPENDENTLOADFLAG:0x800` (System32-only search for the image's dependencies), with a `dll-check` assertion of
  that load-config flag (06), is added only if E2's test shows the delay-load helper honors it; the hook doesn't
  depend on it.
- **Host flags**: `-std=c++20 -Wall -Wextra -Wpedantic -Wshadow -Werror`.
- **Shaders**: `shaders/*.hlsl` compiled by `fxc /T vs_5_0|ps_5_0 /O3 /Fh` at build time into headers (no runtime
  compiler, no `d3dcompiler_47.dll` dependency).
- **Formatting**: `native/.clang-format` (LLVM base, 120 columns, like the Go and TS line length); 06's `native-host`
  job runs `clang-format --dry-run --Werror`.
- **Exports**: `isshoni-media.def` (`LIBRARY isshoni-media`) must list exactly the header's `IM_API` functions,
  checked by `exports_test` at build time (it parses the header and the `.def`). The DLL's export table is checked
  against the header by 06's `tools/desktoppkg dll-check` (06 §5.1); there is no separate dumpbin-versus-`.def` step.

### 13.2 What the FFmpeg build must be (06 builds it)

06's FFmpeg workflow (plan: "built from a pinned n9.0.x tag in its own workflow") owns the exact configure line
(06 §4.2), the zip layout (06 §4.3) and the source archive (06 §4.5). This doc states only the requirements:
- **License**: LGPL version 2.1 or later: no `gpl`, `version3` or `nonfree`.
- **Libraries**: shared `avcodec` and `avutil` only.
- **Encoders**: exactly `h264_nvenc`, `h264_qsv`, `h264_amf` and `h264_mf`, with `d3d11va`, `mediafoundation`,
  `ffnvcodec`/`nvenc`, `amf` and `libvpl` enabled.
- **CRT and symbols**: `-MT` (static CRT); PDBs for the symbols zip.
- **No static Media Foundation imports**: the vendor runtimes and MF are loaded at run time, so the engine still
  loads on N editions.
- **nv-codec-headers n12.1.x**: NVIDIA driver 531.61 or newer on Windows; newer headers would shut out older drivers.
- **Majors**: `native/ffmpeg/lock.json` records `avcodecMajor` and `avutilMajor` as integers (and `tag`), which
  CMake compiles in (§9.2).
- **What 04 uses**: only the zip's `include/` at build time and `bin/` at run time.

### 13.3 Runtime layout and DLL loading

06 §2.2 owns the installed layout; this is the engine's view of it:

```
%LOCALAPPDATA%\Programs\isshoni\    $INSTDIR (per-user; 06 §2.2)
  isshoni.exe
  isshoni-media.dll                 loaded by Go by full path (§12.4)
  avcodec-<C>.dll                   loaded by the engine by full path from its folder (§9.2); unmodified, unsigned
  avutil-<U>.dll
  uninstall.exe
  install-files.txt
  licenses\LICENSE.txt
  licenses\NOTICE.txt
  licenses\THIRD_PARTY_NOTICES.txt
  licenses\ffmpeg\COPYING.LGPLv2.1.txt
  licenses\ffmpeg\LICENSE.md
  licenses\ffmpeg\README-LGPL.txt
%LOCALAPPDATA%\isshoni\codecs\openh264-<v>-win64.dll     only after the user turned OpenH264 on (§9.9)
```

No DLL may be picked up from the current directory or `PATH`. That takes four layers, because loads happen in code
that isshoni doesn't control:
1. **Our explicit loads** name a full path and restrict dependency search to the DLL's own folder and System32
   (`LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32`): Go loading `isshoni-media.dll` (§12.4), the
   engine loading `avutil`/`avcodec` (§9.2) and OpenH264 (§9.9).
2. **The process default** (assumed from 03, §20): `isshoni.exe`'s `main`, before any other step (the sound-check
   helper included), calls `windows.SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` and
   `SetDllDirectoryW("")`, so every bare-name load in the process (Wails, Go, a vendor runtime loaded by FFmpeg, the
   default delay-load helper) skips the current directory and `PATH`; the shell relaunch's inherited working
   directory (03 §13.4 step 2) is then harmless. Explicit `LOAD_LIBRARY_SEARCH_*` flags (layer 1) still apply.
   `isshoni-enginetest` makes the same two calls at startup (§14).
3. **Delay-loaded system DLLs** (`mfplat.dll`, `avrt.dll`): the engine's delay-load hook (§13.1) loads them from
   System32 only, whatever the host did.
4. **Inside FFmpeg**: its own runtime loads (`nvEncodeAPI64.dll`, `nvcuda.dll`, `amfrt64.dll`, the MF DLLs) go
   through `compat/w32dlfcn.h`'s `win32_dlopen`, which searches the application folder and System32 only. libvpl's
   dispatcher (static inside `avcodec`) has its own search, which reads `ONEVPL_SEARCH_PATH` and
   `ONEVPL_PRIORITY_PATH`: `im_init` clears both with `SetEnvironmentVariableW(name, nullptr)` (release builds, and
   every other build too, so the test below runs on the CI DLL; there is no opt-out), before FFmpeg is first loaded
   (lazily, at the first probe or start), so neither the process environment nor the static CRT that FFmpeg
   initializes at its load sees them. Whether the pinned libvpl also scans the current directory or `PATH` itself is
   checked by `dll_search_test` (§18.2, §17).

Release builds read no environment variable for paths; `im_init_params.ffmpeg_dir` (Go's `Options.FFmpegDir`) is
for tests and tools, and NULL means the engine DLL's own directory.

- The engine depends on system DLLs only (plus the delay-loaded ones of §13.1); without FFmpeg it still loads.

### 13.4 Version resource, symbols, crash reporting

- `VERSIONINFO` of `isshoni-media.dll` (06's `dll-check` verifies it; SignPath needs product metadata later): binary
  `FILEVERSION` and `PRODUCTVERSION` = `IM_WINVER`. Strings: `FileVersion` = `IM_WINVER`, `ProductVersion` =
  `IM_VERSION`, `ProductName` `isshoni`, `CompanyName` `isshoni project`, `FileDescription` `isshoni media engine`,
  `LegalCopyright` `Copyright the isshoni authors. Apache-2.0.`, `OriginalFilename` `isshoni-media.dll`,
  `InternalName` `isshoni-media`.
- PDBs go into the release asset `isshoni-desktop_<v>_windows_amd64_symbols.zip` (06 §2.1): `isshoni-media.pdb` and
  FFmpeg's PDBs, never into the installer.
- Every export and every engine thread entry is `noexcept` and catches C++ and WinRT exceptions (`winrt::hresult_error`,
  `std::exception`), turning them into `IM_ERR_INTERNAL` (with a log record) or a session `ENDED{IM_ERR_INTERNAL}`.
  Access violations are **not** caught (the state would be corrupt); they crash the process with Go's report.
- Crash minidumps and "Copy diagnostics" are later (M5/M6). Until then the troubleshooting page (06) explains Windows
  Error Reporting's `LocalDumps`, and the published PDBs make a user's dump readable.

### 13.5 Licenses and notices

Entries for 06's native notices list (plan: `THIRD_PARTY_NOTICES` per artifact, with a hand-kept native list). Each
entry is in 06's `deploy/notices/native-windows.json`, added in the slice that introduces it (E1 libopus and
speexdsp, since the manifest lists both from the start; E8 the OpenH264 headers; E10 C++/WinRT).

| Component | License | How it ships |
|---|---|---|
| libopus | BSD-3-Clause | statically linked into `isshoni-media.dll` |
| speexdsp | BSD-3-Clause | statically linked into `isshoni-media.dll` (05's resampler) |
| FFmpeg (`avcodec`, `avutil`) | LGPL-2.1-or-later | separate DLLs, dynamically loaded, unmodified, with license, configure line and source |
| nv-codec-headers, AMF headers, libvpl | MIT | compiled into the FFmpeg DLLs |
| OpenH264 headers (`codec_api.h` …) | BSD-2-Clause | compiled into `isshoni-media.dll`; license text in `native/windows/third_party/openh264/LICENSE` |
| Cisco OpenH264 binary | Cisco's binary license | never shipped; downloaded by the user's action; attribution shown in About while installed |
| C++/WinRT headers | MIT | compiled in (from the Windows SDK) |
| GoogleTest | BSD-3-Clause | tests only, never shipped (vcpkg feature `tests`) |

No GPL code anywhere in `native/` (plan): 06's license job also scans vcpkg's installed `share/*/copyright` files and
fails on GPL or AGPL. The S1 engine code that 05 promotes is the project's own.

## 14. The engine test tool (`cmd/isshoni-enginetest`)

A Go program (GOOS=windows, cross-compiled from the Mac) that loads the DLL through `internal/media/native`, so it
tests the binding too. Not part of the installer. Global flags: `-dll DIR` (default: the executable's folder), `-v`,
`-json`. Exit codes: **0** pass, **1** an assertion failed, **2** environment (unsupported OS, no encoder of the asked
kind, no GPU), **64** usage. At startup, before loading the DLL, it calls
`windows.SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` and `SetDllDirectoryW("")` (the process default of
§13.3 layer 2) and `SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)` (§7.4).

| Command | Does | Asserts |
|---|---|---|
| `probe [-quick]` | prints `im_build_info` and the caps; writes `probe.json` | full probe < 2 s; every encoder has a status |
| `sources` | lists monitors | — |
| `pick` | shows the OS picker (its own owner window, §7.2 step 2), prints the picked source | — (manual: M-PICK; the Wails window as owner is M-APP-PICK, §18.4) |
| `capture` | `-source pick\|mon:<id>\|test:pattern` (default `test:pattern`), `-seconds 5`, `-preset auto\|game\|movie\|text`, `-encoder auto\|nvenc\|qsv\|amf\|mf-hw\|mf-sw\|openh264` (the §5.6 tokens), `-profile high\|cb`, `-audio off\|tone\|mix` (`tone` = `media.AudioTestTone`, `mix` = `exclude-apps`), `-keyframes 500ms`, `-bitrate-steps 8M,2M,8M`, `-dump-aus N`, `-out DIR`. Writes `DIR/f.h264`, `DIR/q.h264` (Annex B, playable in ffplay, mpv or VLC), `DIR/audio.ogg` (Opus in Ogg, a ~150-line Go muxer in the tool), `DIR/report.json` | each layer's first packet is `KEYFRAME\|REINIT` (preceded by its `EventLayer`) and arrives within 500 ms of `Start` returning, also on a static monitor; every keyframe AU is SPS, PPS, IDR; `profile` matches; capture times monotonic per stream and ≥ 0; audio packets exactly 20 ms apart within a grid; the pointer is drawn (shares never hide it; `IM_START_HIDE_CURSOR` is covered by the C++ tests); with `test:pattern`: `fpsOut` ≥ 90 % of target per layer; with `-keyframes`: every request answered within one frame interval + 5 ms; with `-bitrate-steps` (not combined with `-keyframes`; it sets layer `f`'s GOP to 10 000 ms, §6.4's maximum): output rate within ±25 % of each step's target over the 2 s after it applied (after the deferred re-init for AMF/MF), and the keyframe rule of §9.5: for an encoder whose session reports `LIVE_BITRATE`, the steps are applied 2.5 s apart from 1 s after `Start` (all before the first periodic IDR) and `f` carries **no** `KEYFRAME` or `REINIT` packet from the first step until 2 s after the last; for a re-init encoder, exactly one `REINIT\|KEYFRAME` per applied step, the deferred ones included (periodic IDRs carry no `REINIT`), so `-seconds` must cover the 10 s spacing (hwcheck uses 25 s). A live encoder that fails this fails the run, and the probe's live check (§9.10) must then be fixed to catch it; with `-audio tone`: every beep packet's capture time equals a flash frame's within one frame interval |
| `bench` | `capture` without files, `-seconds 30` | prints encode and capture-to-packet percentiles and the process CPU |
| `sessions -count N` | N sessions of `test:pattern` at 1080p60 with two layers, through `StartExtra` | reports each layer's encoder; passes when every session started (fallbacks allowed) |
| `soak -hours H` | one session; samples private bytes, handles, GDI objects, threads every 60 s | after the first 10 min, growth ≤ 5 %; no `RING_OVERFLOW`; no `ENDED` |
| `avsync [-source pick\|mon:<id>] [-seconds 25]` | 05 §19.3: starts the flash window (a borderless 1280×720 flip-model swap chain, white for 100 ms once a second; each white frame's `SyncQPCTime` from `IDXGISwapChain::GetFrameStatistics` is `P_k`) and the click process (`im_test_render_bursts`, 5 ms clicks once a second, predicted times `A_k`) as children of `explorer.exe`; captures with real capture and `-audio mix` (`TapPCM`) at 1080p60; `V_k` = `capture_ts_ns` of the first frame after each flash whose mean luma in the luma tap exceeds 50 %, `U_k` = each click's PCM-tap onset; pairs each flash with the nearest click. Needs a DLL with `IM_TEST_HOOKS` (the luma tap): `-dll` points at the `win-x64-ci` DLL (06 §5.1's `isshoni-native-tools` carries it in `ci\`) | over ≥ 15 pairs, \|median((U_k − V_k) − (A_k − P_k))\| ≤ 20 ms (05 §19.3); writes `avsync.json` with every pair |
| `protect-check` | creates a window with a known pattern, protects it, captures its monitor with DD and with WGC (listed source); then unprotects and captures again | the pattern is absent while protected and present after |
| `openh264 install\|verify\|remove` | the app's own Go code | hash checks |

`native/windows/tools/hwcheck.ps1` runs the automated set on the owner's PC (probe, `capture` per available encoder,
`bench`, `sessions -count 5`, `protect-check`) and zips the reports, so one command gives a result bundle to attach
to the slice's PR. No machine names or paths go into the reports (they are attached to a public repository).

## 15. Glass-to-glass latency (M2 exit criterion)

The criterion (p95 under 200 ms at 1080p60) is measured once, with 01's tool and run book: `tools/g2g` (01 §12.2,
D17) on the owner's PC, Win11 and Win10 22H2, a monitor share, one monitor, server RTT ≤ 50 ms, p95 < 200 ms over
60 s. One Go tool, cross-compiled from the Mac, that works with a single monitor; 01 §12.1 holds the budget.

This doc contributes **component checks**, reported next to each g2g run so a miss can be located:
- the stats' `latencyMs` (capture time → packet in the ring) per layer, which 01 polls at 1 Hz;
- `enginetest bench` at 1080p60 with a hardware encoder: capture-to-packet p95 ≤ 25 ms and layer `f` encode p99
  ≤ 16 ms (§18.4).

The engine's part of 01 §12.1's budget: capture after composition ≤ 17 ms (one refresh), pacer 0 for a 60 Hz source
(§8.4), VideoProcessor plus encode ≤ 8 ms p95 (E7 measures), ring → Go ≤ 1 ms.

## 16. Room for later milestones

| Later | Room made now |
|---|---|
| macOS engine (M3) | The same header: `IM_CAPTURE_SCK`, `IM_ENC_VIDEOTOOLBOX`, `parent_window` = `NSWindow*`, `im_protect_window` → SCContentFilter exclusion, mono clock = `mach_absolute_time`, `IM_STATIC` for the cgo static library, `internal/media/native/engine_darwin.go`. `native/common` compiles as ObjC++. 4K60 without a preview layer (S2) is a two-layer `im_start` failing with `IM_ERR_NO_ENCODER`, token `layer_failed` naming `q`, after which 01 offers 4K60 alone or 4K30 with preview (plan) |
| Linux shim (M4) | `IM_CAPTURE_PORTAL`, `IM_CAPTURE_X11`, `IM_ENC_GSTREAMER`; `im_list_sources` fills `windows` on X11; the OpenH264 backend moves to `common/` behind a `dlopen` shim; the Pulse audio side is Go (plan), so the audio report is produced by Go there (events 32–63 stay reserved) |
| 720p30 middle layer (M5) | `IM_MAX_LAYERS` = 3; rid `h` |
| Direct AMF/MF bitrate (M5) | `IM_ENCF_LIVE_BITRATE` simply becomes set for them; 01's policy already branches on it |
| "Copy diagnostics" (M5) | `im_build_info`, the probe, `im_get_stats` and `im_read_logs` are the inputs |
| Device-loss recovery (M5) | `IM_EVENT_DEVICE_RESET` and its JSON stay reserved, Go maps it to `EventDeviceReset`. M5 adds a RECOVERING state (release device, frame pool or duplication, VideoProcessors and encoders; retry after 250 ms, 1 s and 3 s on re-enumerated adapters; `framePool.Recreate(newDevice, …)` keeps the `GraphicsCaptureItem`), its fault-injection tests and an M-TDR run (`dxcap -forcetdr`: `DEVICE_RESET` within 10 s, the share continues). M2 ends the session with `device_removed` (§9.8) |
| Win10 frame match (M5, unless E11 pulls it into M2) | §7.5 step 4; `monitor_map_test` gains the frame-match cases (including a tie) when it lands |
| HDR, AV1/HEVC, > 60 fps, several shares in the UI, arm64 Windows (later) | `reserved` fields in `im_start_config` and `im_layer_config` (a codec field), `IM_OS_WGC_MIN_INTERVAL` already wired, sessions are already N, no x64-only code outside `#if` guards |

## 17. Risks and open questions

| Risk | Mitigation |
|---|---|
| **Smart App Control / WDAC may block the unsigned FFmpeg DLLs** once `isshoni.exe` is SignPath-signed (measured in 06 §12.4 step 4): no hardware encoding at all (MF goes through FFmpeg too). Unsigned 0.x builds don't run under Smart App Control anyway (06 §12.1) | The engine detects it (`IM_ERR_DEPENDENCY`, `blocked_by_policy`), keeps working audio-only and with OpenH264 (Cisco signs its binary); 03 explains. A real fix needs the DLLs signed (06 §20 question 1) or direct MF without FFmpeg (M5) |
| Windows 10 22H2 audio exclusion is unverified (S1's Win10 row is empty) | 05; it gates the M2 exit criterion. The engine side is OS-version-neutral |
| What `GraphicsCaptureItem.DisplayName()` returns for a monitor on Win10 is undocumented | §7.5's size → name chain, worst case WGC with the border; E11 records the names, and if they don't tell identical monitors apart a `WF` slice adds the frame match in M2 (§7.5) |
| WebView2 content under `WDA_EXCLUDEFROMCAPTURE` is unverified | E12 checks it first (M-WDA with the affinity 03's `winos` sets); fallback: minimize the main window during full-screen shares (the plan's macOS fallback) |
| A planted DLL in the current directory or a `PATH` entry (the working directory is inherited from the browser on a link launch) | §13.3's four layers: full-path loads, 03's process default, the delay-load hook, FFmpeg's `win32_dlopen` and libvpl's variables cleared. `dll_search_test` (§18.2) plants markers under the vendor runtime names in the current directory and on `PATH` and runs a full probe. If the pinned libvpl dispatcher scans either location itself, that test fails and QSV waits on a fix (a libvpl build or version without that scan, which is 06's FFmpeg pin) while Intel GPUs use MF hardware, Intel's MFT (§9.1 step 2) |
| A real TDR is only exercised by fault injection in M2 | `session_fake_test` injects the removal; a TDR in the field ends the share with "Share again" (§5.7); the hardware run is M5's M-TDR (§16) |
| A GPU-bound game starves the copies and the VideoProcessor; isshoni doesn't run elevated, so it can't raise GPU priority like OBS does | Measured in E15 (M-GAME); `LIMIT_CHANGED gpu` tells the user; M5 may try a compute-queue converter |
| NVENC sessions are shared with other apps (8 per system on GeForce) | Per-layer fallback to MF (§9.7) |
| NVENC API version versus driver age | nv-codec-headers 12.1 (driver ≥ 531.61); older drivers get `driver_too_old` and fall back |
| FFmpeg option names drift between releases | Only required options can fail an open; the probe runs them on every start of the app |
| Hybrid laptops (panel on the iGPU, NVENC on the dGPU) | The capture adapter's encoder comes first (QSV/AMF on the iGPU), keeping zero copy |
| Media Foundation is missing on N editions and Windows Server | OpenH264 remains; the guided fix says so |

**Questions settled elsewhere**
- **Hardware checks**: decided by 06 D17, §10.4 and §10.5: a self-hosted runner on the owner's PC, dispatch-only
  (never pull requests: the repository is public), a non-admin account started by hand; the hardware run is 06
  §10.4's `hwcheck` suite, which runs this doc's `hwcheck.ps1`. Without the runner, the same script runs by hand.
- **Signing FFmpeg's DLLs**: 06 §20 question 1 (with the Smart App Control measurement in 06 §12.4 step 4).

**Open question for the owner**
1. **Community testing**: attach `isshoni-enginetest` and `hwcheck.ps1` as a separate zip to each GitHub Release, so
   friends with other GPUs can send reports (plan: community "test report" template), or keep them CI-only?

## 18. Test plan

### 18.1 Portable C++ unit tests (`native/common/tests`)

Run on the Mac (`task native:test`, preset `host-debug`), in CI on ubuntu-24.04 and macos-15 (`host-debug` and
`host-tsan`), and on windows-2025 inside the `win-x64-ci` build.

| Test | Asserts |
|---|---|
| `abi_layout_test` | every size and offset of §5.4 against `native/common/tests/testdata/abi_layout.json`, the golden file Go's `layout_test.go` and the Windows DLL test also read |
| `ring_test` | wrap-around of slots and arena; an AU that doesn't fit at the arena's end wraps; release on the next call; full ring → drop, `DISCONTINUITY`, needs-IDR flag; stress: 3 producer streams and 1 consumer, 10⁶ packets, strict order within each stream, no loss unless full; clean under TSan |
| `event_queue_test` | 4 producers; overflow drops the oldest non-`ENDED`; `ENDED` always delivered last |
| `handles_test` | generations; stale and foreign handles rejected; 16 slots; wrap-around |
| `annexb_test` | golden AUs per encoder (`testdata/aus/<encoder>/`, small preview-layer AUs captured with `enginetest capture -dump-aus` in E7) and synthetic cases: 3-byte start codes, AUD/SEI/filler/EOS dropped, an IDR without SPS/PPS gets them from the cache, duplicates removed, CB byte → `e0`, a non-100 profile in a High session flagged, `KEYFRAME` from NAL type 5, idempotence |
| `sps_test` | size with cropping (1920×1088 coded → 1080), profile, constraint flags, level, emulation prevention |
| `pacer_test` (virtual clock) | target 60 with sources at 30, 60, 144 Hz, irregular and static: no added latency at or below the target (a 30, 50 or 59.94 Hz source loses no frame), the newest frame kept above it; **rate bound**: over 10 s, `fpsOut` within [59, 60.5] for sources at 75, 100, 120, 144, 165 and 240 Hz (±0.5 ms vsync jitter), both with every vsync delivered (DD, WGC without `MinUpdateInterval`) and with deliveries quantized by a 1/(2 × fps) `MinUpdateInterval` (the first vsync at least that long after the previous delivery, §7.3); the old `last_encode + interval − 2 ms` rule fails this case (≈ 68 fps), as a regression guard; a 144 Hz source resuming after 1 s static gets no catch-up burst (at most 61 encodes of `f` in the first second after it resumes); `q` at 15 fps; keyframe on a static screen within one interval; refresh at 200 ms then every 1 s; pause ignores new frames yet answers IDRs; inactive layer encodes nothing and restarts with an IDR; 250 ms IDR spacing; overload → `cpu`/`gpu` after 3 s, back to `none` after 10 s |
| `encpolicy_test` | candidate order for DD and WGC, single and hybrid GPUs; OpenH264 forces CB on every layer; preview-layer fallback; live versus re-init; 10 s spacing with the latest value winning; 1 s coalescing of size/fps/profile/content-hint re-inits; failure → next candidate; a live encoder whose probe saw an IDR after a bitrate change is treated as re-init; the §5.6 encoder token of every kind |
| `scale_test` | §8.3's examples, odd and tiny sizes, clamping |
| `monitor_map_test` | §7.5 steps 2, 3 and 5 with fake outputs: one size match, a name match among several, two identical monitors with the same name falling through to WGC with the border (frame-match cases come with step 4, §16) |
| `resize_test` | 1 s debounce; letterbox meanwhile; no re-init when the computed output size is unchanged |
| `opus_stage_test` | 05 §10.1's settings read back with `OPUS_GET_*` (constrained VBR, FEC 0, loss 0, fullband, DTX 0); 10 ms → 20 ms framing; packet time = first chunk − lookahead, consecutive packets exactly 20 000 000 ns apart; `REINIT` on the first packet and after `OnGap` (a half frame dropped, any time step); `DISCONTINUITY` after a ring drop with the grid unchanged; chunks before `epoch_ns` dropped and the first packet ≥ `epoch_ns` with `REINIT`; a decoded 1 kHz tone at 128 kbps has SNR ≥ 30 dB; zero chunks (muted bus) → encoded silence keeps flowing at 50 packets/s; PCM tap: 3840-byte packets stamped at chunk time, flowing together with Opus; live bitrate change; the test tone's code-3 padding holds an 18-byte Marker that M1's `ParseAudioMarker` reads (golden bytes) |
| `json_test` | escaping (quotes, control characters, emoji, lone surrogates → U+FFFD), snprintf return values |

**Fuzzing** (`host-fuzz`): `annexb_fuzz`, `sps_fuzz` (no crash, no out-of-bounds, idempotent output), 60 s per
target in the slice that adds it and in the nightly; seeds in `native/common/tests/fuzz/corpus/`.

### 18.2 Windows tests without a GPU or audio device (CI, windows-2025, `win-x64-ci`)

| Test | Asserts |
|---|---|
| `exports_test` | every export with null and invalid arguments returns its documented code, and `im_last_error` starts with the §5.7 token where one applies; `struct_size` below v1.0 → `IM_ERR_ABI`, above → accepted; JSON functions follow snprintf; `im_clock` is monotonic and within 50 ms of the system time; `im_abi_layout` equals the golden file; `isshoni-media.def` lists exactly the header's `IM_API` functions (both parsed); a hook that stalls the worker makes a posted call return `IM_ERR_TIMEOUT` (`engine_timeout`) after its bound + 1 s |
| `session_fake_test` | `test:pattern` + `IM_ENC_FAKE` + test tone: first packet per layer `KEYFRAME\|REINIT` within 500 ms after `im_start` returns, also with a static pattern; `IM_ENC_FAKE` output as §18.2's note (real SPS/PPS for the profile and size, CB byte `e0`, AU sizes, Marker `CaptureNS` = `capture_ts_ns − epoch_ns`); every `capture_ts_ns ≥ epoch_ns` for every kind; `f` ≈ 60 fps, `q` 15 fps; keyframe within one interval; `im_set_layer_active` stops and restarts `f` with an IDR; pause yields only `REFRESH`/IDR and audio keeps 50 silent packets/s; `im_set_profile` → `REINIT\|KEYFRAME` with the new `profile`; `im_set_content_hint` → `REINIT\|KEYFRAME` on every layer, coalesced with an `im_update_layer` in the same second; injected encoder error → `ENCODER_CHANGED`, delivered by `im_next_packet` before the new encoder's first `REINIT\|KEYFRAME` packet; injected device removal → `ENDED{IM_ERR_ENCODER_LOST}` with detail token `device_removed` within 1 s (no `DEVICE_RESET`, no RECOVERING), and a following `im_start` on a new listed source runs on a new device; exhausted candidates → `ENDED{IM_ERR_ENCODER_LOST}`; a consumer that stops for 2 s gets `RING_OVERFLOW`, stale drops and an IDR when it resumes; `im_stop` with a blocked consumer returns in ≤ 2 s and the consumer gets `IM_ENDED` in ≤ 50 ms; the fifth session → `IM_ERR_LIMIT`; pick tokens through the test hooks' fake picker: a failed `im_start` keeps the token, consumed, expired (clock hook at 5 min after the pick) and evicted (fifth pick) tokens → `IM_ERR_SOURCE_GONE` `pick_expired` |
| `audio_bridge_test` | with a fake `AudioEngine`: `im_init`/`im_shutdown` call `Init`/`Shutdown`; every audio export forwards its arguments (rules, flags, `shared_pid` copied); `im_start_config.exclusions` → `SetRules` before `Mixer::Start`; mode OFF creates no mixer; `im_start` with `audio.mode` `IM_AUDIO_MODE_EVERYTHING` and `im_audio_set_mode(IM_AUDIO_MODE_EVERYTHING)` → `IM_ERR_INVALID_ARG`, no mixer created and the fake's mode unchanged; `im_set_paused` → `Mixer::SetPaused`; the fake mixer's chunks come out as Opus packets every 20 ms and, with `IM_AUDIOF_TAP_PCM`, PCM packets every 10 ms |
| `window_match_test` | §7.6's matcher over a fake window list: one match gives `window` and `pid`; two matches or none give 0; cloaked windows skipped; an `applicationframehost.exe` match takes its `Windows.UI.Core.CoreWindow` child's pid, and 0 without one |
| `gpu_warp_test` | on WARP: the shader converter against a CPU BT.709 limited-range reference (≤ 1 code value on flat areas, ≤ 2 on edges); the VideoProcessor against the shader output (≤ 2) when WARP offers one, else skipped with the reason; letterbox geometry; rotation 90/180/270; cursor blending of color, masked and monochrome shapes against golden images, and no blend with `IM_START_HIDE_CURSOR`; the GPU test pattern renders |
| `ffmpeg_load_test` | with the pinned artifact: loads, versions check, encoders listed (none available without a GPU, MF software if the runner has Media Foundation); `ffmpeg_dir` pointed at an empty folder → `IM_ERR_DEPENDENCY` (`missing`) and an audio-only test-tone session still works; a stub DLL with another major → `IM_ERR_DEPENDENCY` (`version`) |
| `dll_search_test` (§13.3) | Run as a child process whose current directory and one `PATH` entry each hold marker DLLs (a tiny test DLL whose `DllMain` writes a file named after itself; built by the tests, never shipped). **Delay-load hook** (E2): the `win-x64-ci` build's test binary links the engine code with an extra `/DELAYLOAD:im-test-missing.dll` (a name that is in no system folder; the windows-2025 image has `mfplat.dll` in System32, which is searched before the current directory, so a planted `mfplat.dll` could never show the bug); with `im-test-missing.dll` planted in both places, `__HrLoadAllImportsForDll("im-test-missing.dll")` fails, no marker file appears and `GetModuleHandleW(L"im-test-missing.dll")` is NULL; the same check with `mfplat.dll` passes and loads it from System32 (`GetModuleFileNameW`). **Vendor runtimes and libvpl** (E7, in `ffmpeg_load_test`'s harness with the pinned FFmpeg): markers named `nvEncodeAPI64.dll`, `nvcuda.dll`, `amfrt64.dll`, `libmfx64-gen.dll` and `libmfxhw64.dll` (none exist on the GPU-less runner) planted in both places, `ONEVPL_SEARCH_PATH` and `ONEVPL_PRIORITY_PATH` set to the marker folder, and **no** process default set (the host-independent case); after `im_init` and a full `im_probe`, no marker file exists and `EnumProcessModules` lists none of them. A failure of the libvpl names is §17's libvpl risk, not a flake |
| `openh264_test` | CI downloads Cisco's DLL and checks the pinned hash (a mismatch fails the job); `test:pattern` 1280×720@30 for 3 s through the full session (WARP, readback, OpenH264) and back through OpenH264's decoder: PSNR ≥ 35 dB on the pattern's static regions; profile key `42e0`; SPS and PPS before every IDR; a live bitrate step lands within ±25 % |
| `mf_sw_test` | the same through MF software when `MFStartup` works on the runner; otherwise skipped and logged |

**`IM_ENC_FAKE`** (`IM_TEST_HOOKS` builds; for a `test:` source `IM_ENC_AUTO` selects it first, so CI is deterministic
without a GPU). Every access unit carries a real SPS and PPS for the session profile and the layer size (High:
profile_idc 100; CB: profile_idc 66 with constraint byte `e0`; level from size and fps; cropping for odd heights), as
an IDR AU (SPS, PPS, one NAL 5 slice) or a P AU (one NAL 1 slice). AU size ≈ target / fps, an IDR ≈ 10× a P frame
(the P size is lowered so the 2 s average stays within ±10 % of the target); bitrate changes are live. Each slice
body starts with M1's 18-byte Marker (with emulation prevention, like M1's synthetic slices): `RID`, `Keyframe`,
`Frame` = index in the layer, `CaptureNS` = `capture_ts_ns − epoch_ns` (the value Go's `Packet.CaptureNS` gets), and
`Flash` on each layer's first frame at or after `epoch_ns + k × 1 s`, the instants where the test tone's beeps start
(§10.3). So 01's Conformance checks 5 and 7 and its latency arithmetic (`Origin + Marker.CaptureNS`, 01 §12.4) work
on native output.

**Go on Windows** (`go test ./internal/media/native/...` with `ISSHONI_TEST_ENGINE_DLL` = the `win-x64-ci` DLL):
`Open` and the ABI check; a missing export → `*media.Error{internal, Reason "abi"}` naming it; the clock cross-check
passes; quick `Probe`; **`mediatest.Conformance`** (01 §6.4) with the `test:pattern` source (`IM_ENC_FAKE`; audio from
05's mixer, which on a runner without a render endpoint sends silent frames, 05 §4.3); `Next`/`Stop` under `goleak`;
every status maps to its `*media.Error` code and `Reason`; `EventLayer` before media and before each `REINIT`;
`PCMTap` with `TapPCM`; `InstallOpenH264` against an `httptest` server (a bz2 fixture; a tampered file is rejected and
leaves nothing behind); **the process default** (§13.3 layer 2): `cmd/isshoni-enginetest`'s startup function, run in
a child process whose current directory and one `PATH` entry hold `im-marker.dll` (a name in no system folder), after
which `LoadLibraryExW(L"im-marker.dll", nullptr, 0)` fails and the marker's `DllMain` never ran (03 runs the same
check for `isshoni.exe`'s first step); and **`TestNativeIntoPublisher`**: a native session (`IM_ENC_FAKE` plus the test tone, both
carrying M1 Markers) → `publish.Publisher` → M1's in-process SFU harness → `sfutest.Viewer`: frames arrive on both
layers, a PLI gets a keyframe within one frame, and the flash/beep A/V offset read from the video and audio Markers is
under 45 ms (M1 integration 5's check, now on native timing).

### 18.3 Go tests everywhere (`go test ./internal/media/native/...` on the Mac, ubuntu, macOS)

`layout_test` against the golden file; JSON decoding of build info, sources, stats and event fixtures; OpenH264
helpers against `httptest` (with a test pin of `Size` and `SHA256`): a good fixture installs; a wrong hash, a body
one byte short and one byte long each fail and leave `dir` empty; a compressed body over 10 MiB fails; **a bzip2-bomb
fixture** (a few KB that expand to 1 GiB of zeros) fails after at most `Size + 1` bytes were written to the temp file
(a counting writer hook), the temp file is gone, and the heap grows by less than 16 MB (`runtime.ReadMemStats`
before and after); `VerifyOpenH264` rejects a file of the wrong size without hashing it; `Open` returns an error
wrapping `media.ErrUnsupportedOS`. **`bridge_test`** drives the
whole binding through `fakeCaller` (01 §7.7): every status and token → `*media.Error` (code, `Reason`, `Op`); the
snprintf retry; `media.Config` → `imStartConfig` for every row of §12.2 (including `imExclusions` and the listed-source
flag); `Reconfigure`'s calls and order; `AudioConfig.Mode` `everything` (at start) and `SetAudioMode(AudioEverything)` →
`*media.Error{invalid}` with no DLL call (README §3 item 15); `imCaps` → `media.Probe` (`Probe.Audio` from
`media.AudioCapsFromFlags`); `IM_ERR_DEPENDENCY` → `dependency` and `IM_ERR_NO_ENCODER` → `no_encoder`, each with its
token as `Reason`; the copy and limits (oversized, invalid kind or
layer, negative `CaptureNS` → dropped and a keyframe requested); `ENDED` → Next's error and exactly one `im_stop`;
`Stop` idempotent, `Next` after `Stop` → `ErrStopped` without a DLL call, `INVALID_ARG` during `Stop` → `ErrStopped`,
a blocked `Next` returns within 50 ms of `Stop`; the event table of §12.3 row by row, in particular `LIMIT_CHANGED`
`gpu` then `none` → two `EventLimit` packets with `Limit` `"gpu"` and `"none"` (the `none` edge reaches `Next`), and
`ENCODER_CHANGED` followed by a `REINIT` packet → `EventEncoderChanged{Layer, Encoder: <to>}`, then the new
encoder's `EventLayer`, then the packet; `EventLayer` synthesis after start and before a `REINIT` (the packet held for
the next call); PCM routed to `PCMTap` and never returned by `Next`; `Now` = `qpcNS −
epoch_ns`; the clock cross-check (pass, and a 2 ms offset fails `Open`); a self pick → `invalid`/`self_window` and the
token released.

### 18.4 The owner's PC (Windows 11 24H2 or 25H2 and Windows 10 22H2, dual boot)

**Automated** (`hwcheck.ps1`, §14): full probe < 2 s (its live-bitrate check included, §9.10); per available
hardware encoder, two `capture` runs of `test:pattern` at 1080p60: 5 s with `-keyframes 500ms`, and 25 s with
`-bitrate-steps 8M,2M,8M` (§14's rate and keyframe assertions: no `KEYFRAME`/`REINIT` between steps for a live
encoder, exactly one `REINIT|KEYFRAME` per applied step for a re-init one); `bench`: layer `f` encode p99 ≤ 16 ms and
capture-to-packet p95 ≤ 25 ms with a hardware encoder; `sessions -count 5`; `protect-check`. `soak -hours 2` runs
separately. Each slice that needs it attaches the bundle to its PR.

**Manual checklist** (exact steps; results recorded in the PR, like M1's "Manual:" items):

| ID | OS | Steps | Expected |
|---|---|---|---|
| M-PICK (E10) | both | `enginetest pick`; move the picker, cancel once, then pick a window, then a monitor | the picker is modal over the tool's own owner window (centered on the primary monitor); cancel → `IM_ERR_CANCELLED`; each pick prints a source of the right kind |
| M-APP-PICK (README WA72, after the app's share sheet and tray slices) | Win11, then Win10 22H2 | the installed PR artifact: `isshoni.exe` → Share; while the picker is open, drag another window over isshoni's main window; Cancel; Share again, pick a window → live, watch from a browser; stop; Share, pick a monitor → live, watched (Win10: the DD path); Share, pick isshoni's own window | the OS picker is modal over the main window (the Wails `HWND` as owner, §7.2) and the main window keeps repainting; Cancel returns to the share sheet with no error; window and monitor shares are live and decode in the browser (Win10 monitor: `SourceDetail.CapturePath` `dd`, no border); the own-window pick gives `self_window` and goes back to the picker |
| M-WGC-WIN | Win11 | `capture -source pick -seconds 30`, pick a browser window; drag-resize it, minimize and restore it, then close it | no yellow border; cursor visible; letterboxed during the drag, `f.h264` changes size about 1 s after; `SOURCE_MINIMIZED`/`RESTORED`; `ENDED{SOURCE_GONE}` |
| M-WGC-MON | Win11 | pick a monitor | no border; session flag `NO_OS_INDICATOR` |
| M-SECONDARY | Win11 24H2+ | share a window, open one of its dropdown menus | the menu is in the capture |
| M-DD | Win10 | pick a monitor; move the pointer over text (I-beam) and a link (hand); open a UAC prompt; change the resolution; rotate the display if possible | `capture_path` DD with `MAPPED`; no border; pointer shapes drawn; `CAPTURE_INTERRUPTED`/`RESUMED` and the UAC prompt never captured; re-init at the new size; upright output |
| M-WIN10-WINDOW | Win10 | pick a window | yellow border (expected, documented) |
| M-HDR | an HDR monitor, if available | share it with HDR on | SDR picture, highlights clipped, not washed out; else "community-tested" |
| M-144 | a high-refresh monitor, if available | share it for 30 s (`capture -source pick -seconds 30`, the monitor, with a moving window on it) | `fpsIn` up to the refresh rate (DD, or WGC without `MinUpdateInterval`), between 60 and 120 with `MinUpdateInterval` (72 at 144 Hz, 82.5 at 165 Hz); `fpsOut` within [59, 60.5]; `f`'s bitrate at most 10 % above its target (no CBR overshoot) |
| M-NVENC-LIMIT | NVIDIA GPU | record with NVENC in OBS, then `sessions -count 5` | every session starts; some layers report MF |
| M-WDA (E12; again with the pill in README WA80) | both | in the desktop app, share the full screen with the isshoni window on top and a share-sheet dropdown open; watch from a phone | the isshoni window (its WebView2 content included) and the pill never appear. This checks the affinity 03's `winos` sets (§11), on WGC (Win11) and DD (Win10) |
| M-GAME | both | share a GPU-bound game in borderless mode at 1080p60 | `fpsOut` ≥ 50, or `LIMIT_CHANGED gpu` recorded with the game's GPU load |
| M-AVSYNC (E15) | both | `isshoni-enginetest avsync -dll ci` (05 §19.3's steps): Win11 with `-source pick` (the flash window, WGC), Win10 22H2 with `-source mon:<id>` (DD) | \|median A/V offset\| ≤ 20 ms over ≥ 15 pairs; `avsync.json` attached to the exit record (05 §18.10 item 3) |
| M-OPENH264 | both | turn OpenH264 on in the app, force it (`encoder` override), share, watch in Chrome | download, verification, attribution; Chrome decodes |
| M-LATENCY | both | 01 §12.2's run book (`tools/g2g`, a monitor share, one monitor, RTT ≤ 50 ms) | p95 < 200 ms over 60 s at 1080p60; the share's `latencyMs` and a `bench` run (§15) recorded next to it |

### 18.5 Exit criterion

The M2 exit run (with 01, 03, 05 and 06's slices done): **M-LATENCY passes on Windows 11 and Windows 10 22H2**
(01 §12.2's `tools/g2g` measurement, with this doc's component checks of §15 next to it), and the audiotest passes on
both (05). The run also repeats M-WDA, M-DD and M-WGC-WIN on the release
build that 06 produces.

### 18.6 Where each part runs

| Part | Mac | CI ubuntu | CI macos | CI windows | Owner's PC |
|---|---|---|---|---|---|
| `native/common` tests, TSan | ✓ | ✓ | ✓ | ✓ (MSVC) | — |
| Fuzz 60 s | ✓ | nightly | — | — | — |
| DLL build `/W4 /WX`, export check | — | — | — | ✓ | ✓ |
| Windows C++ tests (fakes, WARP, OpenH264, MF if present) | — | — | — | ✓ | ✓ |
| Go pure tests | ✓ | ✓ | ✓ | ✓ | — |
| Go tests with the DLL, `TestNativeIntoPublisher` | — | — | — | ✓ | ✓ |
| `dll_search_test` (delay-load hook, vendor-runtime and libvpl markers), the process-default check | — | — | — | ✓ | — |
| Hardware encoders, capture paths, picker (tool and app), WDA, latency components (g2g itself: 01), soak | — | — | — | — | ✓ |

## 19. Interfaces other docs rely on

- **`native/include/isshoni_media.h`** exactly as §5.2: every function, struct, constant, status code and event code
  (05's audio block in this header's conventions, all v1.0); the conventions of §5.1 (`struct_size`, frozen embedded
  structs, snprintf JSON, input copied, packet memory valid until the next call, one `im_next_packet` caller per
  session, the engine never calls the host, the `<token>: ` prefix of `im_last_error`); the call table of §5.3 with its
  bounds and `IM_ERR_TIMEOUT`; the layouts of §5.4 and their golden file
  `native/common/tests/testdata/abi_layout.json`; the versioning rules of §5.5.
- **The status/event → UI mapping** of §5.7, its fixed **detail tokens**, the encoder tokens of §5.6 and the event
  detail JSON (03 renders, 01 acts on `ENDED`).
- **JSON payloads** of §5.6: `im_build_info`, `im_list_sources`, `im_get_stats` (fields match the SPA's
  `ShareStatsSample`; `encoder` is a token, `codec` diagnostic; `audio.state` uses 05's state strings), `im_read_logs`
  records.
- **Pick tokens** (§7.2): single use, a failed `im_start` keeps the token, 5 min from the pick, at most 4,
  `IM_ERR_SOURCE_GONE` + `pick_expired` afterwards.
- **`internal/media/native`** (§12.2): `Open`, `Options`; `*Engine` implementing `media.Engine` (with `Release`) and
  `media.AudioController`; the session implementing `media.Session` (with `SetLayerActive`, `SetPaused`, `Stats`); the
  Windows-only extras `BuildInfo`, `Clock`, `SetOpenH264Path`, `ProtectWindow` (enginetest's `protect-check` only in M2;
  the app's WDA is 03's `winos`, §11), `SourceDetail`, `StartExtra`, `SetAudioBitrate`, `PCMTap`/`PCMChunk` (the tone is
  `media.AudioTestTone`); errors always `*media.Error` with `Reason`; the mappings of §12.2 (config, reconfig, probe,
  pick, errors) and §12.3 (packets, events = 01 §5.2's table, `EventLayer`, clock, stop); `OpenH264` (with
  `Size`)/`InstallOpenH264`/`VerifyOpenH264`/`RemoveOpenH264`.
- **Session semantics** 01 relies on: `CaptureNS` and `Now()` share the origin `epoch_ns` and `CaptureNS` is never
  negative, audio included; order is strict within a stream only; each layer's first packet is `KEYFRAME|REINIT`
  within 500 ms of `Start`, also on a static screen, and every video packet after a re-init is `KEYFRAME|REINIT`
  (video only; audio `REINIT` marks a new 20 ms grid); one `EventLayer` per layer before media and before each video
  `REINIT`; all layers share one profile, reported in `EventLayer.Profile`; `EventLayer.LiveBitrate` says whether a
  bitrate change is cheap (the probe has checked that a live change emits no IDR); the engine falls back between
  encoders itself and reports it as `EventEncoderChanged{Layer, Encoder}` before the new encoder's `EventLayer`; it
  ends with `encoder_lost` only when no candidate is left or the GPU device was removed (`Reason` `device_removed`,
  no recovery in M2); `EventLimit` reports both edges (`cpu`/`gpu` and back to `none`), hysteresis applied by the
  engine; stale packets (> 1 s) are dropped by the engine; a session survives any
  signaling or PC outage until `Stop`; mute and pause keep silent Opus frames flowing every 20 ms.
- **`IM_ENC_FAKE`** output (§18.2) for 01's Conformance and latency proxy.
- **The audio boundary** (§10.2): `im::audio::PcmSink` (04 provides; `OnGap` = a new timestamp grid), and the
  `Mixer`/`AudioEngine` shape 04 calls (05 implements); the Opus stage with 05 §10.1's settings and lookahead-corrected
  timestamps, the PCM tap (`IM_PACKET_PCM`), the start-at-`epoch_ns` rule, audio-only sessions for the audiotest, the
  test tone mode; the pick's `pid` reaching 05 only through Go (`im_exclusions.shared_pid`, §7.6), with the
  `applicationframehost.exe` rule.
- **Build outputs** for 06: presets `win-x64-release` (no vcpkg features), `win-x64-ci`, `win-x64-asan`,
  `host-debug`, `host-tsan`, `host-fuzz`; outputs `isshoni-media.dll` + `isshoni-media.pdb`; inputs `IM_VERSION`,
  `IM_WINVER`, `IM_COMMIT`, `IM_FFMPEG_ROOT`; `native/vcpkg.json` (opus, speexdsp; feature `tests`); the VERSIONINFO
  fields of §13.4; `isshoni-media.def` checked against the header by `exports_test`; CMake's use of
  `native/ffmpeg/lock.json`; the engine's view of the layout (§13.3); the FFmpeg requirements of §13.2; the notices
  of §13.5; `cmd/isshoni-enginetest` and `native/windows/tools/hwcheck.ps1`.
- **Minimum driver note** for 03's guided fixes and 06's requirements page: NVIDIA ≥ 531.61 for NVENC (with
  nv-codec-headers 12.1); Windows 10 22H2 (19045) or Windows 11; Media Foundation (Media Feature Pack on N editions)
  for the MF encoders.

## 20. Depends on

**01-client-core.md**
- `internal/media` (01 §5) as the contract this binding implements: `media.Engine` (`Info`, `Probe`, `Sources`,
  `Pick`, `AudioApps`, `Start`, `Close`, plus the addition `Release`), `media.Session` (`Next`, `Now`,
  `RequestKeyframe`, `SetBitrate`, `Reconfigure`, `Stop`, plus the additions `SetLayerActive`, `SetPaused` and
  `Stats() (media.Stats, error)` with §5.6's fields), `media.Packet` with `KindEvent` and `Event`, `media.Config`,
  `media.Reconfig`, `media.Probe`, `media.Source`, `media.PickRequest`, `media.LayerAudio`, and `*media.Error`. The
  additions 04 assumes in 01's types: `media.Error.Reason`, `media.EncoderInfo.Reason`, `CodeTimeout` (whose `Unwrap`
  returns `context.DeadlineExceeded`), `media.Event` fields `Profile`, `LiveBitrate`, `NoOSIndicator`, `Limit`, and
  01 §5.2's event codes as the binding uses them (`EventLimit`, `EventEncoderChanged`, `EventSourceMinimized`,
  `EventSourceRestored`, `EventCaptureInterrupted`, `EventCaptureResumed`, `EventDeviceReset`; no `EventOverload`).
  01 feeds every `EventLimit` edge, `none` included, to `ratectl.SetLimit`, and raises the `encoder_fallback` notice
  from `EventEncoderChanged` (01 §11.4). `internal/media` does not import `internal/media/native`.
- 01 builds `media.Config` from `ShareParams`: layers from `encodings` in wire order (`f`, `q`: `maxPixels`,
  `maxFramerate`), `bitrate_bps` = 01's ratectl initial target (≤ `maxBitrate`; 01 starts `f` at 4 Mbit/s and ramps),
  `Profile` from `codec`, `Preset`, the Opus bitrate from `audioBitrate`. 01 always passes `IM_ENC_AUTO`
  (`Encoder: ""`), never restarts sessions for an encoder fallback (the engine falls back itself), and ends the share
  on `ENDED{ENCODER_LOST}`. 01 offers the profile the session reports (`EventLayer.Profile`).
- 01 applies `quality.hint`: `codec` → re-offer then `Reconfigure{Profile}`; `encodings[].active` →
  `SetLayerActive`/`SetBitrate(layer, 0)`; `maxBitrate` → `SetBitrate` with the plan's hysteresis (≥ 15 %, ≤ every
  2 s) and, for layers without `LiveBitrate`, only changes ≥ 30 %. A preset change goes through `Reconfigure`
  (`Preset` → `im_set_content_hint`, fps → `im_update_layer`, Opus target → `im_set_audio_bitrate`). Resolution stays
  fixed per share in M2 (01 D11).
- 01 calls `RequestKeyframe` on PLI/FIR, anchors SR NTP once per Publisher from `Session.Now` and Go's wall clock
  (01 §8.2; `im_clock` is not used for SRs), pulls `Next` continuously while a session runs (one pump goroutine), polls
  `Stats` at 1 Hz into `ShareSnapshot`, calls `SetPaused` from `Share.SetPaused`, and keeps a session across
  reconnects for up to 30 s (plan). It never re-`Start`s with a consumed pick token (a listed `mon:`/`test:` source can
  be started again).
- 01's `tools/g2g` and its run book (01 §12.1, §12.2) measure the exit criterion.

**03-desktop-app.md**
- Passes the main window's `HWND` as `PickRequest.OwnerWindow`, calls `Pick` and `Start` off the UI thread, and
  offers cancel; on `source_gone`/`pick_expired` or `invalid`/`self_window` it returns to the OS picker.
- Sets `WDA_EXCLUDEFROMCAPTURE` on every isshoni window itself (`winos`, `OSHooks.ExcludeFromCapture`, 03 §11.5): the
  main window and the pill; it does not call `ProtectWindow` (§11). Note from §11: uses no native `<select>` popups
  and disables the webview's context menu in the share UI (WebView2's own popups are separate top-level windows of
  another process that nothing can protect).
- **DLL search** (§13.3 layer 2): `main`'s very first action, before `--sound-check-helper` and every other step of
  03 §13.4, calls `windows.SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` and `SetDllDirectoryW("")`
  (`LOAD_LIBRARY_SEARCH_APPLICATION_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32` if a Wails or WebView2 bare-name load turns out
  to need the app folder; both exclude the current directory and `PATH`), with 03's own marker test that a planted
  `im-marker.dll` in the current directory and on `PATH` no longer loads by bare name. Changing to the exe's folder is
  then optional.
- On `encoder_lost` with `Reason` `device_removed`, shows the stop notice with **"Share again"** (§5.7). On
  `EventEncoderChanged` (through 01's `encoder_fallback` notice) the share stays live; 01 never falls back or
  restarts itself (01 D15).
- Shows the "Sharing" pill whenever `EventLayer.NoOSIndicator` is set; places it on the shared monitor using
  `SourceDetail(id).MonitorID` and `Sources()`.
- Maps every status, token and event per §5.7 and owns every string; never sends a source name, window title, `pid`
  or app list to the server (plan "Privacy").
- Owns the OpenH264 setting, Cisco's notice, the attribution in About and the `%LOCALAPPDATA%\isshoni\codecs` folder;
  calls `InstallOpenH264` then `SetOpenH264Path`.
- Runs the full probe at startup in the background; may expose a hidden encoder override for support (a token of
  §5.6 in `media.Config.Encoder`). The first-run sound check is 05's `Service.SoundCheck`, which starts
  `isshoni.exe --sound-check-helper` (03 handles that flag in `main`) and calls `im_audio_sound_check` (05 §11.3).
- The app manifest declares PerMonitorV2 DPI awareness.

**05-windows-audio.md**
- Implements `AudioEngine` and `Mixer` of §10.2 in `native/windows/src/audio/` and `native/common/audiopolicy/` and
  `audiomix/`, calling `PcmSink::OnChunk` every 10 ms on its mixer thread with QPC-stamped chunks after the mix bus,
  `OnGap` only when a new grid starts, `Mixer::Start` ≤ 100 ms with the probe asynchronous, never blocking the mixer.
- Owns the semantics of the audio block of §5.2 (rules, modes 0–2, flags, the report JSON of `im_list_audio_apps`,
  levels, the sound check and the test exports), the user mute (`im_audio_set_muted`), the report state that
  `IM_AUDIO_STATE_*` mirrors, and the FriendsHear banners; asks for header additions after M2 as minor bumps.
- Owns `internal/media/native/audio_windows.go` (`media.AudioController` on `*Engine`) and passes the shared
  window's pid as `im_exclusions.shared_pid` from `audio.Service.OnShareStarted`.
- The audiotest uses this C ABI on the shipping DLL: audio-only sessions, `IM_AUDIOF_TAP_PCM` and `PCMTap`,
  `IM_AUDIOF_TEST_FAIL_PROBE`, the test exports. 05's A6 accepts the real-mixer path through the C ABI (§21 E13).

**06-windows-release.md**
- The FFmpeg workflow, `native/ffmpeg/pins.json` and `lock.json` (06 §4.1, with `avcodecMajor`, `avutilMajor` and
  `tag`), the zip of 06 §4.3 and the source archive of 06 §4.5.
- CI jobs: `native-host` (ubuntu and macos host presets plus clang-format), `windows-native` (`win-x64-ci` build,
  `ctest`, `dll-check`, `win-x64-release` build), `windows-desktop` (Go tests with `ISSHONI_TEST_ENGINE_DLL`), the
  nightly `native-fuzz` and `windows-asan`; all feeding `ci-ok`. The hardware run is 06 §10.4's `hwcheck` suite on
  the §10.5 runner (dispatch-only).
- Task targets `native:test` (host presets), `native:build`, `native:dist` (a zip of `isshoni-enginetest.exe`,
  `hwcheck.ps1`, the DLL and the FFmpeg DLLs, to copy to the owner's PC) and `ffmpeg:fetch` (fetch and verify the
  FFmpeg zip).
- The release build (`win-x64-release` with `IM_VERSION` and `IM_WINVER` from the tag), the installed layout of
  06 §2.2, `tools/desktoppkg dll-check` (exports against the header, imports, VERSIONINFO), the symbols zip
  `isshoni-desktop_<v>_windows_amd64_symbols.zip`, the native notices (`deploy/notices/native-windows.json`) and the
  license scan of vcpkg's copyright files. M2 releases are unsigned (06 §12.1); Authenticode signing of
  `isshoni.exe`, `isshoni-media.dll`, `uninstall.exe` and the installer via SignPath is later (06 §12.4, R18).

**02-device-auth.md**: nothing. The engine knows nothing about servers, users or tokens.

**M1 (as built)**: `publish.Publisher` with `Source`, `ProfileSource` and `SetActive` (M1 02 §15.1); `sfutest` harness
and `Viewer` with Marker parsing (M1 02 §17); the Marker format of `internal/media/fake/marker.go`; the SFU's codec
table, PLI throttle and layer pausing (M1 02 §8.2, §9.7, §11).

## 21. Implementation slices

Ordered; each one is testable on its own. "CI" means GitHub Actions on the named runners; "PC" means the owner's
Windows PC (Win11 and Win10 22H2).

| # | Slice | Scope | Acceptance | Where | Needs |
|---|---|---|---|---|---|
| E1 | ABI and portable skeleton | `isshoni_media.h` complete, including 05's audio block and the shipped test exports (all v1.0, `IM_ABI_MINOR 0`); `native/CMakeLists.txt`, presets, `vcpkg.json` (opus, speexdsp, feature `tests`; baseline pinned); `common/`: status, handles, JSON writer, log ring, the layout golden file; host tests; Go `internal/media/native` tagless half: `abi.go` (mirror, status and token mapping, encoder tokens), `types.go`, the `caller` interface and `fakeCaller`, `layout_test.go`, `engine_other.go`; notices entries for libopus and speexdsp | `task native:test` green on the Mac; the header compiles as C11 and C++20 with warnings as errors under clang, gcc and MSVC; Go and C++ layout tests agree with the golden file (§5.4's sizes); the status/token mapping tests pass on the Mac; `GOOS=windows go vet` clean | Mac, CI ubuntu/macos/windows | — |
| E2 | DLL skeleton and Go loader | `exports.cpp` with every export (init, shutdown, version, build info, clock, last error, logs, layout real; the audio exports forwarded to a stub `AudioEngine`; the rest `IM_ERR_NOT_IMPLEMENTED`), `isshoni-media.def`, `version.rc` (§13.4), the worker thread with the posted-job bound, the delay-load hook (`delayload.cpp`, §13.1) and, in `win-x64-ci` only, the extra `/DELAYLOAD:im-test-missing.dll` with its stub; `im_init` clearing `ONEVPL_SEARCH_PATH`/`ONEVPL_PRIORITY_PATH` (§13.3); `engine_windows.go`, `call_windows.go`, `clock_windows.go` (`Open`, ABI check, clock cross-check, `BuildInfo`, `Clock`, log drain) | `win-x64-release` and `win-x64-ci` build with `/W4 /WX`; `exports_test`: the `.def` equals the header's `IM_API` list; 06's `dll-check` passes (exports, imports, VERSIONINFO with `OriginalFilename` `isshoni-media.dll`); `dll_search_test`'s delay-load part: a planted `im-test-missing.dll` in the current directory and on `PATH` is refused, `mfplat.dll` loads from System32; whether the delay-load helper honors `/DEPENDENTLOADFLAG:0x800` recorded in §13.1 (flag and `dll-check` assertion added only if it does); Go opens the DLL, the layout matches, a missing export is `internal`/`abi` naming it, the clock cross-check passes | CI windows | E1 |
| E3 | Rings, normalizer, pacer, policy | `common/`: rings, event queue, Annex B normalizer, SPS parser, pacer, encoder policy, scaling rule, resize debounce, monitor-map logic; fuzz targets | §18.1 tests pass, TSan clean, 60 s per fuzz target clean | Mac, CI | E1 |
| E4 | Opus stage | `common/audio`: `PcmSink`, the Opus stage with 05 §10.1's settings and lookahead-corrected timestamps, the start-at-`epoch_ns` drop, the `REINIT`/`DISCONTINUITY` rules, the PCM tap, the tone generator and its Marker framing; libopus through vcpkg | `opus_stage_test` | Mac, CI | E1 |
| E5 | Session core on fakes | session state machine, video thread, supervisor, command queue, rings wired to `im_next_packet`, controls (including `im_set_content_hint`), stats, events, the first-keyframe bound; test hooks (`IM_ENC_FAKE` as §18.2, fault injection, the fake picker), test tone; Go `bridge.go` (`Next`, `Stop`, the event table and `EventLayer` synthesis, `PCMTap`, the §12.2 mappings) | `session_fake_test`; `bridge_test` on the Mac and ubuntu; Go tests with the DLL under `goleak`, including `mediatest.Conformance` on `test:pattern`; `TestNativeIntoPublisher` | CI windows, Mac, CI ubuntu | E2, E3, E4 |
| E6 | GPU path | device and lock, slots, shader converter, VideoProcessor, cursor blending (and `IM_START_HIDE_CURSOR`), the GPU test pattern (`test:pattern` in release builds) | `gpu_warp_test`; on the PC the VideoProcessor matches the shader output within 2 on each available GPU vendor | CI windows (WARP), PC | E5 |
| E7 | libavcodec encoders and probe | `ffmpeg_api`, `FfmpegEncoder` for NVENC, QSV, AMF, MF (hw and sw), zero-copy decisions, normalizer wiring, `im_probe` with `status` and `reason` per encoder and the live-bitrate check (§9.10); `enginetest probe`, `capture` (with `-bitrate-steps`' keyframe assertions), `bench`, and the tool's process setup (§14); CMake reading `native/ffmpeg/lock.json` | `ffmpeg_load_test`, `mf_sw_test` (if MF present), `dll_search_test`'s vendor-runtime and libvpl part, and the Go process-default check in CI; on the PC: full probe < 2 s, both `capture` runs per available encoder pass §14's assertions at 1080p60 (first keyframe ≤ 500 ms included; `-bitrate-steps`: rate within ±25 %, no `KEYFRAME`/`REINIT` between steps for NVENC and QSV, one `REINIT\|KEYFRAME` per applied step for AMF and MF; if QSV emits IDRs, the probe's check clears its live flag and the run passes as re-init), `bench` encode p99 ≤ 16 ms; AU fixtures committed for E3's tests; §9.3's settings, layer 0's VBV comparison (0.5 s against 0.1–0.2 s) included, recorded in the PR for the write-back to §9.3 | CI windows, PC | E5, E6, 06's FFmpeg workflow |
| E8 | OpenH264 | Go `InstallOpenH264`/`VerifyOpenH264`/`RemoveOpenH264`; engine backend; `im_set_openh264_path`; pinned version, hash and decompressed size, the bounded streaming install of §9.9; `third_party/openh264/LICENSE` and its notices entry | `openh264_test` in CI with Cisco's real DLL; §18.3's OpenH264 helper tests on the Mac and ubuntu (tampered, short and long downloads rejected with nothing left behind; the bzip2 bomb stops after `Size + 1` bytes with bounded memory); wrong version → `IM_ERR_DEPENDENCY` `version`; Go pin equals `codec_ver.h` | CI windows, PC | E6 |
| E9 | Selection, fallback, device loss, profiles | candidate order, preview-layer fallback, profile consistency, `im_set_profile`, `im_update_layer`, `im_set_content_hint` re-inits, re-init spacing, `ENCODER_CHANGED` queued before the new encoder's first packet, device loss ending the session (`device_removed`, the probe cache marked stale; no recovery, M5) | fault-injection cases of `session_fake_test` extended (fallback event order; injected device removal → `ENDED{IM_ERR_ENCODER_LOST}` `device_removed` within 1 s, then a new session starts on a new device); on the PC M-NVENC-LIMIT passes | CI windows, PC | E7 |
| E10 | WGC and the picker | picker thread and `IInitializeWithWindow`, pick tokens (§7.2's rules), window identity with the `applicationframehost.exe` rule, WGC capture (borderless, `MinUpdateInterval` at half the frame interval, `IncludeSecondaryWindows`, cursor), resize/minimize/close handling; `enginetest pick`; the C++/WinRT notices entry | M-PICK (the tool's own owner window), M-WGC-WIN, M-WGC-MON, M-SECONDARY on the PC, and M-144 where a high-refresh monitor is available; `window_match_test`. The picker with the Wails window as owner is M-APP-PICK, run later in README's WA72 (§7.2) | PC, CI (build and units) | E7 |
| E11 | Desktop Duplication | DD capture, access-lost handling, rotation, DD cursor shapes, §7.5 mapping **steps 1–3 and 5** (no frame match), `im_list_sources` | M-DD and M-WIN10-WINDOW on Win10; `monitor_map_test`; what `DisplayName()` returns for each monitor on Win10 22H2 (two outputs if available) recorded in §7.5 with the verdict: names unique → step 4 stays M5; names ambiguous for identical monitors → recorded, and the integrator adds a `WF` slice for step 4 in M2 | PC, CI | E10 |
| E12 | Own windows | `im_protect_window` (ABI v1, for M3; in M2 used only by the tool), `IM_SRCF_SELF` (Go: `invalid`/`self_window`), `enginetest protect-check` | `protect-check` passes on Win11 and Win10 for DD and WGC; M-WDA with the app (the affinity 03's `winos` sets on the main window; WebView2 content excluded; the pill joins in README's WA80) | PC | E10, E11, 03's window slice (README WA68) for M-WDA |
| E13 | Audio bridge | `audio_bridge/`: 05's `AudioEngine`/`Mixer` wired to sessions (`SetRules` from `im_start_config.exclusions`, `Mixer::SetPaused`), the audio exports forwarded (`im_audio_watch` … `im_audio_sound_check`, the test exports), audio-only sessions, the PCM tap; a fake `AudioEngine` for 04's tests | CI, with the fake `AudioEngine`: `audio_bridge_test` (forwarding, exclusions passed through, PCM tap cadence, mute and pause keep silent frames flowing, session events unaffected by audio). The real-mixer acceptance on the PC (`capture -audio mix` gives a playable `audio.ogg`; 05's audiotest runs through the C ABI) belongs to 05's A6 | CI windows | E4, E5 |
| E14 | Pause, stats, log privacy | `im_set_paused` (frozen frame, `Mixer::SetPaused`), the full stats JSON, the log privacy rule of §5.6 (any level) | CI: with the luma tap, no packet after pause + one interval shows new content; audio: 10 ms ramp, then silent frames continue at 50/s and the PCM tap shows digital zero; un-pausing keeps a user mute set through `im_audio_set_muted`; a log-capture test, run once at info and once at debug level, finds no window title and no process, executable or app name in the drained ring | CI windows | E5, E13 |
| E15 | Soak, hardware run, exit run | `enginetest soak`, `enginetest avsync` (05 §19.3), `hwcheck.ps1`; the exit-criterion run | M-LATENCY by 01 §12.2's `tools/g2g` (p95 < 200 ms over 60 s at 1080p60) on Win11 and Win10 22H2, with this doc's component checks (stats `latencyMs`, `bench` capture-to-packet p95 ≤ 25 ms) recorded next to it; `soak -hours 2` passes; M-AVSYNC on Win11 (WGC) and Win10 22H2 (DD); M-GAME recorded | PC | E7–E12, 01 (`tools/g2g`) and 03 (the app shares) |
