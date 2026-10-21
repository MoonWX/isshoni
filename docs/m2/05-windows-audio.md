# M2 design 05: Windows audio, "all system audio except voice apps"

Status: draft for M2, 2026-10-21. `docs/PLAN.md` is the source of truth above this doc. Evidence: spike S1
(`spikes/s1-win-audio/`: the README results, `native/engine.cpp`, `selftest_windows.go`, `analyze.go` and the two
reports in `results/`), M1 as built (`docs/m1/README.md` §6, `internal/protocol`, `internal/client/publish`,
`internal/media/fake`).

Written in parallel with `01-client-core.md`, `02-device-auth.md`, `03-desktop-app.md`, `04-windows-engine.md` and
`06-windows-release.md`. Where this doc needs something from another one, §22 states the assumed interface exactly.

Tags: **M2** = build now. **later (Mx)** = leave room now, build in milestone x.

This doc is the M2 exit criterion "Discord is excluded (the audiotest passes on Win10 and Win11)". §18 is the exact
gate.

## 1. Scope

In this doc (all M2 unless tagged):
- The audio half of the Windows engine (`isshoni-media.dll`), up to 04's `im::audio::PcmSink::OnChunk`: session
  watcher, process table, exclusion policy, include-set planner, process-loopback capture, mixer, adaptive resampling,
  chunk timestamps, startup probe, sound check, "What friends hear" report and level meters. It implements the C++
  boundary 04 §10.2 defines (§10.5 here).
- Requirements on 04's Opus stage (§10.1, §10.2); 04 builds the stage (04 §10.3).
- The audio part of the C ABI v1 (`native/include/isshoni_media.h`, §4). 04 owns the file; §4.1 is the text 04 merges
  under 04 §5.1's conventions.
- Two portable C++20 libraries without OS headers, testable on the Mac: `native/common/audiopolicy` (rules,
  classification, planning; CMake target `im_audiopolicy`) and `native/common/audiomix` (rings, resampling, mixing,
  chunk timestamps; target `im_audiomix`), both added by 04's `native/common/CMakeLists.txt`.
- Go: the OS-neutral audio types and the `AudioController` interface (`internal/media/audio.go`, stdlib only, 01 D1);
  the Windows binding in `internal/media/native` (untagged logic in `audio.go`, procs in `audio_windows.go`, 01 D3); the
  fake engine's scripted audio (`internal/media/fake/audio_ctl.go`); the built-in data (`internal/media/audiorules`);
  and the desktop policy service (`internal/desktop/audio`: settings, prompts, the `FriendsHear` model the share sheet
  renders, and `RunSoundCheckHelper`).
- `cmd/isshoni-audiotest`: the harness, its fixtures, the pass rule, the cases, where it runs and how results are
  recorded.

Not in this doc: video capture and encode, the DLL's build system, `im_start`/`im_stop`/`im_next_packet`, the Opus
stage, the PCM tap, the audio rings and the bridge from the mixer to `im_next_packet` (04 §6.2, §10.2, §10.3); RTP
packetization, SRs and rate control (01); the share sheet, wizard, tray and the Wails bridge (03); device auth (02);
installers, release jobs and notices files (06).

Not in M2: macOS taps (M3), the Linux Pulse mixer (M4); per-app volume sliders, app icons, loss-adaptive Opus (M5);
the Discord Web separate-window launcher (Later).

## 2. Decisions at a glance

| Topic | Decision | Why |
|---|---|---|
| Capture model | Include set: one `PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE` stream per allowed session process, mixed. One `EXCLUDE` stream on isshoni's own tree when that is the only excluded tree ("fast path") | Plan; S1 passed on Win11 25H2 and Server 2025 with excluded tones at −133 to −145 dB. One EXCLUDE stream can drop only one tree (S1 `exclude-one-limit`) |
| Default for an unclassified process | Not captured until classified (fail-closed). The fast path is the one fail-open mode, guarded by a retroactive cut (§6.4) and its own audiotest cases (4, 5, 26), which gate whenever the release ships it on (§18.10) | A voice app must never leak while the engine is still looking |
| Policy location | Classification and planning in native code, on a fresh process snapshot, in the thread that opens streams. Go owns the rule data, settings and prompts | The invariant "no INCLUDE on a tree holding an excluded process" must be checked at commit time; a Go round trip would open a race |
| Policy code | Portable C++20 (`native/common/audiopolicy`), unit-tested on the Mac and on CI ubuntu/macos/windows. The Windows glue only feeds it snapshots | Most of the risk is logic, not WASAPI; it should run in `task test` on the Mac |
| Session discovery | `IAudioSessionNotification` + `IAudioSessionEvents` + `IMMNotificationClient`, with a 1 s poll as a safety net | S1 limitation: a 500 ms poll lost first sounds |
| PID reuse | Hold a `SYNCHRONIZE` handle on every include target while its stream lives (snapshot checks when the handle can't be opened, §5.7); creation-time checks on parent links; a lineage cache for orphans; loose (PID-only) ancestry for the safety invariant, reported as its own reason `pid_reuse` | A held handle makes reuse of that PID impossible; the loose check covers whatever Windows itself uses for "tree" until the INFO case `pid-reuse-tree` measures it |
| Mic users | Kept out by default, sticky for the life of the app's root process, one-tap prompt remembered per app (by exe name) | Push-to-talk apps toggle their capture stream; a 10 s timeout (S1) would let incoming voices leak between presses |
| Mixer clock | 10 ms ticks on QPC, 48 kHz stereo float; per-stream PI-controlled adaptive resampling (speexdsp); silence is corrected by dropping or padding zeros | S1 limitation: QPC vs device clock drift caused rare trims; at 100 ppm that is a dip every ~6 minutes |
| Click safety | S1's fades as built (preroll 40 ms, 10 ms fades, 64-frame tail ramp) plus the onset fade (5 ms ramp after ≥ 50 ms of digital silence; a new channel counts as silent, as in S1's code) | S1 runs 1–2: the only artifact was the stream-attach click; the onset fade is the diagnosed fix |
| Opus (04's stage, 05's requirements) | 48 kHz stereo, 20 ms frames, `OPUS_APPLICATION_AUDIO`, signal music, constrained VBR, complexity 10, bitrate from `ShareParams.AudioBitrate` (128 k, Movie 256 k), in-band FEC off, DTX off (§10.1) | FEC only works in SILK/hybrid, which these bitrates never use (plan); continuous frames keep jitter buffers and A/V sync steady |
| Timestamps | The mixer hands 04's stage chunks stamped on an exact 10 ms grid in the QPC domain: `tick_k − L`, `L` = the fixed estimate of the delay from the audio engine's mix (the loopback QPC position) to the mixer's pop, so a chunk is stamped with the time Windows mixed it, without the sharer's output-device latency (§10.3); `L0` = 50 ms until a silence re-anchor measures it; 04's stage subtracts the encoder lookahead, so packets sit on an exact 20 ms grid | Video stamps DWM's composition time (WGC `SystemRelativeTime`, DD `LastPresentTime`), not the display's; friends never go through the sharer's speakers. Go derives RTP timestamps from capture times (M1 publisher); any jitter would break the 960-sample RTP step |
| Startup probe | A 200 ms, −60 dBFS, 997 Hz tone in a private session, captured by an INCLUDE stream on isshoni itself. Failure → endpoint loopback + banner | Plan. Process loopback is digital and pre-volume (S1: muted 19% volume still gave −12 dB), so a nearly inaudible tone suffices |
| No "All sound" mode in M2 | The user can't turn off the built-in list; `IM_AUDIO_MODE_EVERYTHING` stays reserved in the ABI (later) | Plan: "A short, tested built-in list is always kept out"; the per-share **Share sound** switch (03) is the only "off" |
| isshoni's own audio | Instance rule on the DLL's process tree (always on, even if Go forgets), an app rule on `isshoni.exe` (other instances), and a match on isshoni's WebView2 `--user-data-dir` (orphans) | Plan: exclude all isshoni.exe descendants (msedgewebview2) |
| Reports | JSON report (`im_list_audio_apps`, changes rarely, has `seq`) + fixed-size level struct polled at 10 Hz | No callbacks into Go (pull-based ABI); meters need no JSON parsing |
| Privacy | App names, exe names, rules and levels never leave the device and are never logged, at any level; logs carry codes and counts | Plan "Privacy and trust model"; 01 §11.12 |
| Harness | One Go binary that copies itself as fixtures (S1's proven approach), drives the shipping DLL, analyzes a PCM tap with Goertzel (over the whole window and over sliding 50 ms windows, so a leak of a few milliseconds fails) and click detection, and writes a redacted JSON report | The S1 self-test design, extended to the plan's cases; S1's single whole-window level can't see a 30 ms leak; the PCM tap avoids needing an Opus decoder in Go |
| Gate location | The owner's physical PC, Win11 and Win10 22H2 (dual boot). GitHub-hosted Windows runners have no audio endpoint; they build, unit-test and run `selfcheck` | Process loopback needs a real render endpoint and an interactive session |

## 3. From spike S1 to M2

| S1 result or shortcut | M2 |
|---|---|
| Include set passed (Win11 26200; Server 2025 26100): two voice apps at −140 dB, helper process, self, runtime rule (no click), mic user, app boundary after the fixture fix | Same algorithm (§5, §6), ported into `audiopolicy` with the same two notions of tree |
| Real Discord desktop kept out, YouTube (Brave) heard (Win11) | Manual check R1 in every gate run (§18.9) |
| Process loopback accepted plain float32 48 kHz on the first try | Same format attempts in the same order (§8.2) |
| Loopback level unaffected by master volume and mute | Documented for users: muting the PC doesn't mute the stream; Pause or **Share sound** off does (03) |
| Steam holds a microphone session, so it was auto-excluded as a mic user (its own UI sounds; games are separate apps) | Kept: launchers stop the app walk, so Steam games stay heard (§5.4); audiotest case `steam-game` |
| late-start clicks: Windows drops the first milliseconds of an app that starts playing after the loopback attaches; the stream then starts at full level | Onset fade (§9.5). **Not yet re-verified on Windows** (pending owner run); slice A0 and cases `late-app`, `late-helper` |
| Poll every 500 ms; first sound of a new app lost | Session notifications plus a 1 s poll; new apps heard ≤ 500 ms after their session appears, usually ≈ 100 ms (§7) |
| Mixer on QPC without drift correction | Adaptive resampling (§9.3) |
| Endpoint mode didn't follow default-device changes | `IMMNotificationClient`; the endpoint stream reopens on the new default (§7.5) |
| One capture thread per stream | One MMCSS capture thread multiplexing ≤ 63 stream events (§8.4) |
| Activation on the poller thread, 5 s timeout | Two activator threads, 3 s timeout, re-validation at commit (§8.1) |
| `im_audio_*` draft ABI, PCM out | Audio part of ABI v1 (§4); the mixer feeds 04's `PcmSink` (Opus stage), whose packets reach `im_next_packet` through 04's audio ring (04 §6.2, §10.3) |
| Win10 22H2: not run | **Gate A0** before any M2 audio code merges (§23). If the include set fails on Win10, the plan's gate applies: Win10 gets endpoint loopback only, and the owner is told |
| Mic stickiness 10 s | Life of the app root process (§5.6) |
| Test tones 1031/453/571/683/907 Hz between musical notes | The plan's 1 kHz game and 440 Hz voice, plus distinct extra tones; background music is caught by a silence pre-check instead (§18.4) |

## 4. C ABI: the audio part of v1 (normative)

04 owns `native/include/isshoni_media.h` and its conventions (04 §5.1): fixed-width fields, `#define`d `uint32_t`
constants (never C `enum` types), `struct_size` first in every top-level struct, every export `IM_API im_status IM_CALL`
(JSON exports `IM_API int32_t IM_CALL`, snprintf-style), thread-local `im_last_error`. 04 also owns `im_init`,
`im_probe`, `im_start`, `im_stop`, `im_next_packet`, `im_set_paused`, `im_set_audio_bitrate`, `im_clock`, the status
codes (with `IM_ERR_TIMEOUT` = −15, which 04 adds for this doc), the packet kinds (`IM_PACKET_AUDIO`,
`IM_PACKET_PCM`), `IM_AUDIO_STATE_*`, and the structs `im_audio_config`, `im_start_config`, `im_caps` and
`im_packet`. §4.1 is the audio block of 04's header (04 §5.2, §10.1), in 04's conventions; modes 0–99 and the
semantics of every audio declaration are this doc's. There is no separate audio-enable or audio-status export: sound
on/off is `im_audio_set_mode`, status is `im_list_audio_apps`. §4.2 lists what this doc needs in 04's structs; §4.3
gives the semantics.

### 4.1 Declarations

```c
/* ---------------------------------------------------------------- audio (semantics: docs/m2/05-windows-audio.md) */

#define IM_AUDIO_SAMPLE_RATE 48000u
#define IM_AUDIO_CHANNELS    2u
#define IM_MAX_RULES         512u
#define IM_MAX_RULE_VALUE    520u      /* bytes including the NUL (MAX_PATH-sized paths, UTF-8) */
#define IM_APP_ID_LEN        64u       /* bytes including the NUL */
#define IM_MAX_LEVEL_APPS    64u

/* im_audio_config.mode and im_audio_set_mode: modes 0..99 (100 = IM_AUDIO_MODE_TEST_TONE, 04) */
#define IM_AUDIO_MODE_OFF          0u  /* im_start: no audio track. Mid-share: capture closed, silence (§6.5) */
#define IM_AUDIO_MODE_EXCLUDE_APPS 1u  /* everything except kept-out apps and isshoni: the only M2 mode */
#define IM_AUDIO_MODE_EVERYTHING   2u  /* reserved (later): everything except isshoni; M2: IM_ERR_INVALID_ARG */

/* im_audio_config.flags */
#define IM_AUDIOF_TAP_PCM          0x1u         /* also emit IM_PACKET_PCM chunks of the mix (audiotest only) */
#define IM_AUDIOF_TEST_FAIL_PROBE  0x80000000u  /* audiotest only: behave as if the startup probe failed */

/* im_rule.kind */
#define IM_RULE_APP_EXCLUDE        0u  /* value: exe file name ("discord.exe"); matches the process or an app ancestor */
#define IM_RULE_APP_ALLOW          1u  /* value: exe name; "Include": never kept out for mic use (viewer rules still win) */
#define IM_RULE_INSTANCE           2u  /* value: decimal pid; matches it and every OS descendant */
#define IM_RULE_STOP               3u  /* value: launcher/shell exe name; ends app walks; never an include target */
#define IM_RULE_SHARED_RUNTIME     4u  /* value: exe name that belongs to its parent's app whatever its folder */
#define IM_RULE_BROWSER            5u  /* value: exe name of a browser (report flag "browser", browser prompt) */
#define IM_RULE_WEBVIEW_DATA_DIR   6u  /* value: isshoni's WebView2 user data folder, full path (§5.3 row 1) */

/* im_rule.source */
#define IM_RULE_SRC_BUILTIN        0u  /* the built-in voice-app list */
#define IM_RULE_SRC_USER           1u  /* a remembered user decision */
#define IM_RULE_SRC_SELF           2u  /* isshoni itself */
#define IM_RULE_SRC_VIEWER         3u  /* a browser that plays the user's isshoni viewer */
#define IM_RULE_SRC_DATA           4u  /* launchers, shared runtimes, browsers */

typedef struct im_rule {             /* array element: no struct_size */
  uint32_t kind;                     /* IM_RULE_* */
  uint32_t source;                   /* IM_RULE_SRC_* */
  const char *value;                 /* UTF-8, NUL-terminated, < IM_MAX_RULE_VALUE; compared case-insensitively */
  const char *app_id;                /* report id for matches, < IM_APP_ID_LEN, [a-z0-9:._-]; NULL or "" = "exe:<value>" */
  const char *name;                  /* display name for reports; NULL = the exe's FileDescription */
} im_rule;

/* im_exclusions.flags */
#define IM_EXCLF_KEEP_OUT_MIC_USERS 0x1u  /* "Keep out apps that use my microphone" (default on) */
#define IM_EXCLF_FAST_PATH          0x2u  /* allow the single-EXCLUDE fast path (default on, §6.4) */

typedef struct im_exclusions {
  uint32_t struct_size;              /* sizeof(im_exclusions) */
  uint32_t flags;                    /* IM_EXCLF_* */
  const im_rule *rules;
  uint32_t rule_count;               /* <= IM_MAX_RULES */
  uint32_t shared_pid;               /* process of the shared window (im_source_info.pid); 0 = a monitor or unknown */
} im_exclusions;

typedef struct im_audio_level {      /* array element: no struct_size */
  char app_id[IM_APP_ID_LEN];        /* NUL-terminated; matches apps[].id of the report */
  float peak_db;                     /* max over the last 100 ms, dBFS; -120 = silence */
  float rms_db;                      /* captured: RMS over 100 ms; activity: = peak_db */
  uint32_t source;                   /* 0 = captured (what friends hear), 1 = activity (session peak meter) */
  uint32_t reserved;
} im_audio_level;

typedef struct im_audio_levels {
  uint32_t struct_size;              /* set by the caller */
  uint32_t report_seq;               /* equals the report's "seq"; a change means: fetch the report */
  int64_t qpc_ns;                    /* engine clock (QPC ns, im_clock().mono_ns) of this sample */
  float mix_peak_db;                 /* the mix fed to the Opus stage, last 100 ms; -120 when not live or silenced */
  float mix_rms_db;
  uint32_t count;                    /* entries written to apps[] */
  uint32_t reserved;
  im_audio_level apps[IM_MAX_LEVEL_APPS];
} im_audio_levels;

/* "What friends hear": §12.1's JSON, snprintf rule. flags: 0 (reserved). Works while watching or live; otherwise it
   takes a one-shot snapshot (<= 500 ms). */
IM_API int32_t   IM_CALL im_list_audio_apps(uint32_t flags, char *json, int32_t cap);

/* Watcher without capture (the share sheet before going live). Idempotent; the rules replace the previous ones. */
IM_API im_status IM_CALL im_audio_watch(const im_exclusions *ex);
IM_API im_status IM_CALL im_audio_unwatch(void);     /* the watcher keeps running while a share with audio runs */

/* Replaces the rules, flags and shared_pid of the watcher and of a running share (copied). Applied within 50 ms. */
IM_API im_status IM_CALL im_set_exclusions(const im_exclusions *ex);

/* The running audio session (one per process, 04 §6.4). set_mode crossfades (20 ms); set_muted fades (10 ms). */
IM_API im_status IM_CALL im_audio_set_mode(uint32_t mode);       /* IM_AUDIO_MODE_* */
IM_API im_status IM_CALL im_audio_set_muted(uint32_t muted);     /* 0 or 1: the pause mute (01 D14), not "Sound off" */

/* Level meters (§12.2). Lock-free read, < 50 us. */
IM_API im_status IM_CALL im_audio_levels_get(im_audio_levels *out);

/* First-run sound check (§11.3): captures the chime of helper_pid's process tree; 0 = the DLL plays the chime itself.
   §11.3's JSON, snprintf rule. timeout_ms: 1000..10000. */
IM_API int32_t   IM_CALL im_audio_sound_check(uint32_t helper_pid, int32_t timeout_ms, char *json, int32_t cap);

/* Test exports: stable for cmd/isshoni-audiotest, not for apps. In every build: the gate tests the release DLL.
   endpoint_id: an IMMDevice id (UTF-8); NULL or "" = the default render endpoint (eConsole). */
IM_API im_status IM_CALL im_test_render_tone(const char *endpoint_id, float hz, float amp, int32_t ms);
/* Returns the number of onsets written to onset_ns, or a negative status. onset_ns: each burst's predicted
   device-position time (engine clock), from the render client's IAudioClock, never from the mixer (§13.2). */
IM_API int32_t   IM_CALL im_test_render_bursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms,
                                               int32_t period_ms, int32_t count, int64_t *onset_ns, int32_t cap);
/* Returns the frames per channel decoded into pcm (interleaved stereo float), or a negative status. */
IM_API int32_t   IM_CALL im_test_opus_decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames);
```

Layouts for 04 §5.4 (amd64 and arm64): `im_rule` 32 (`value` 8, `app_id` 16, `name` 24); `im_exclusions` 24 (`rules`
8, `rule_count` 16, `shared_pid` 20); `im_audio_level` 80 (`peak_db` 64); `im_audio_levels` 5 152 (`qpc_ns` 8,
`mix_peak_db` 16, `count` 24, `apps` 32). Engine and timestamps use 04's clock: the harness reads it through `im_clock`
(there is no audio-specific clock export).

### 4.2 Fields this doc needs in 04's structs

| 04 struct | Field | Meaning |
|---|---|---|
| `im_start_config` | `audio` (`im_audio_config`, 04's fixed 32-byte layout: `mode`, `bitrate_bps`, `frame_ms`, `flags`, `reserved[4]`) | `mode` = `IM_AUDIO_MODE_*`; `IM_AUDIO_MODE_OFF` → no audio packets at all. `bitrate_bps` 32 000..510 000, 0 = 128 000. `frame_ms` 20 (0 = 20; 10 is later (M5)). `flags` = `IM_AUDIOF_*`. A config with zero video layers and audio on is valid (04 §10.3; the audiotest needs it) |
| `im_start_config` | `exclusions` (`const im_exclusions *`) | NULL = keep the process-wide set (the last `im_set_exclusions` or `im_audio_watch`); otherwise replaces it before audio starts, exactly as `im_set_exclusions` would |
| `im_caps` | `audio_flags` | §11.1. The OS build is `im_caps.os_build` |
| `im_packet` | `kind` | `IM_PACKET_AUDIO`: one 20 ms Opus packet (`duration_us` 20 000). `IM_PACKET_PCM` (04): one tap chunk of 480 frames, `size` 3 840, `duration_us` 10 000, only with `IM_AUDIOF_TAP_PCM` |
| `im_packet` | `layer` | `IM_NO_LAYER` for both |
| `im_packet` | `flags & IM_PKT_REINIT` | the first audio packet after `im_start` and the first after a timestamp-grid discontinuity (§10.4) |
| `im_packet` | `flags & IM_PKT_KEYFRAME` | never on audio |
| `im_packet` | `flags & IM_PKT_DISCONTINUITY` | 04's ring and Opus stage (04 §6.2, §10.3) |
| `im_packet` | `capture_ts_ns` | §10.3, engine clock (QPC ns, `im_clock().mono_ns`); for PCM: the chunk's first sample, same domain |

### 4.3 Semantics

- **Threads.** Every function may be called from any thread, concurrently. All COM work happens on DLL-owned MTA
  threads (§7, §8). An ABI call that needs COM (`im_list_audio_apps` one-shot snapshot) posts a request to the watcher
  thread and waits at most **3 s**, then returns `IM_ERR_TIMEOUT` (−15). `im_audio_sound_check` is exempt from the 3 s
  limit: it waits `timeout_ms + 500` (§11.3). The DLL never calls `CoInitializeEx` on a caller thread (Go threads
  arrive through `SyscallN` in any COM state, 04 §12.4).
- **Memory.** The DLL copies `im_exclusions`, its rule array and every string during the call (04 §5.1); the caller's
  memory may be freed when the call returns. JSON exports follow 04's snprintf rule: they return the full length `n`,
  and when `n ≥ cap` the caller retries once with `n + 1` bytes (Go: 01 §7.3's buffer helper, starting at 64 KiB). The
  report is at most 256 KiB by construction (§12.1 caps).
- **Validation** (04 §5.1). `struct_size` below the v1.0 size of `im_exclusions` (24) or `im_audio_levels` (5 152) →
  `IM_ERR_ABI`. A larger `struct_size` is accepted; so is a smaller one that is still at least the v1.0 size (an older
  caller on a newer DLL): fields beyond it read as zero, and output is written only up to it. Unknown rule kinds or
  sources, empty values, `rule_count > IM_MAX_RULES`, a malformed pid or an `app_id` outside `[a-z0-9:._-]{1,63}` →
  `IM_ERR_INVALID_ARG` with `im_last_error` naming the rule index; nothing is applied. Unknown `flags` bits are
  ignored. An unknown mode → `IM_ERR_INVALID_ARG`.
- **Implicit rules.** The DLL always adds `IM_RULE_INSTANCE(GetCurrentProcessId())` with source `IM_RULE_SRC_SELF` and
  app id `isshoni`, whatever Go passes.
- **Before start.** `im_set_exclusions` without a watcher or share stores the rules for the next `im_audio_watch` or
  `im_start` (unless that start passes its own `exclusions`) and returns `IM_OK`.
- **State.** Neither watching nor live: `im_audio_levels_get` → `IM_ERR_BAD_STATE`. No running audio session:
  `im_audio_set_mode` and `im_audio_set_muted` → `IM_ERR_BAD_STATE`.
- **Silence flags.** The mix bus is silent while any of three flags is set: **paused** (04's `im_set_paused`, which
  reaches `Mixer::SetPaused`, §10.5), **muted** (`im_audio_set_muted`: the mute 01's core sets with the pause, 01 D14),
  or **mode off** (`im_audio_set_mode(IM_AUDIO_MODE_OFF)`: the per-share "Share sound" switch). Each flag is set and
  cleared only by its own call, so a resume never turns on sound that was switched off. Go switches sound with
  `AudioController.SetAudioMode` only, never with `SetAudioMuted` (§14.2).
- **Bitrate.** `im_set_audio_bitrate(s, bps)` (04) sets the Opus target from the next frame, 32 000..510 000 (out of
  range → `IM_ERR_INVALID_ARG`), live `OPUS_SET_BITRATE`, no reinit. Go's binding maps 01's
  `Session.SetBitrate(media.LayerAudio, bps)` onto it.
- **Errors.** Audio never ends a share and never makes `im_next_packet` fail. `im_start` fails for audio reasons in
  exactly two cases:
  - a second concurrent session with `mode ≠ IM_AUDIO_MODE_OFF` → `IM_ERR_LIMIT` (04 §6.4: one session with audio per
    process; a programming error, the v1 UI runs one share);
  - after `im_init` with `IM_INIT_NO_AUDIO`, `mode ≠ IM_AUDIO_MODE_OFF` → `IM_ERR_BAD_STATE`.

  Device loss, audio-service restarts, a missing render endpoint and stream failures are handled inside (§7.5, §8.5)
  and appear in the report (`state`, `events`); an audio-only config on a machine without a render endpoint starts in
  state `no-device` and feeds silence. The plan's `AUDIO_DEVICE_CHANGED` is the report event `default_device_changed`
  / `device_removed`, not a return code (04 has no audio events, §10.5).
- **`IM_INIT_NO_AUDIO`.** The DLL touches no audio API: `audio_flags = 0`; `im_audio_watch`, `im_list_audio_apps`,
  `im_audio_levels_get` and `im_audio_sound_check` return `IM_ERR_BAD_STATE`; `im_start` with audio as above.

## 5. Exclusion policy

### 5.1 Identities

- **Process key** = (pid, creation time as FILETIME 100 ns units). Creation time comes from `GetProcessTimes`; 0 when
  the process can't be opened (some protected processes). Every map in the engine is keyed by process key, never by
  pid alone.
- **App identity** = the exe file name, lower case (`discord.exe`). Rules, remembered decisions and built-ins all match
  exe names, not paths: Squirrel-updated apps (Discord, Slack) move to a new `app-x.y.z` folder on every update, and a
  path rule would forget the user's decision. Two different programs with the same exe name share a decision; that
  errs toward keeping more out.
- **App id** (report rows, decisions): the `app_id` of the rule that matched in the app walk (`discord`, `isshoni`), else
  `exe:<exe of the app root>` (`exe:spotify.exe`).
- **Display name**: the rule's `name` (Go fills it for built-ins), else `FileDescription` from the app root's version
  resource (`GetFileVersionInfoSizeExW` / `GetFileVersionInfoExW` / `VerQueryValueW`, first language), else the exe name
  without `.exe`. Cached per image path.

### 5.2 Two kinds of tree (S1)

- **OS tree** of R: R and every process whose parent chain reaches R. This is what an INCLUDE or EXCLUDE stream
  captures, so it decides whether R is a safe include target and whether one target already covers another.
  - *Strict* parent link: the parent exists and was created before the child (otherwise the PID was reused).
  - *Loose* parent link: the PID alone. The safety invariant (§6.2) uses the union of strict and loose links, because
    Windows' own notion of a tree is not documented; the loose link can only make the engine keep more out.
  - A target blocked **only** through a loose link (the kept-out process is in its tree by the union but not by strict
    links) is reported with reason `pid_reuse`, never `tree`: the common case is a dead launcher's PID reused by a
    game's helper (Discord's parent, Squirrel's `Update.exe`, exits right after launch), and "Started Discord" would
    then be false. Counter `pidReuseBlocks` counts such verdicts. The owner's-PC INFO case `pid-reuse-tree` (§18.5,
    case 27) measures whether Windows' tree follows a reused PID at all: if it shows Windows ignores reused-PID links,
    the loose union is dropped (I1 then uses strict links only); if it can't get a matching PID, the union stays with
    this separate reason.
- **App tree**: which app a process belongs to. P's app parent is its strict parent, unless
  - the parent is a launcher or shell (`IM_RULE_STOP`), or
  - P is not a shared runtime (`IM_RULE_SHARED_RUNTIME`, e.g. `msedgewebview2.exe`) and P's image folder is neither the
    parent's folder nor below it, or either folder is unknown.

  The **app walk** of P is P, its app parent, that one's app parent, … (at most 64 steps). The **app root** is the last
  process of the walk. So a game launched from Steam is its own app (Steam is a launcher), Teams' WebView2 audio belongs
  to Teams (shared runtime), and a browser opened from a Discord link is not Discord (another folder; S1
  `app-boundary`).

### 5.3 Rule matching and precedence

A process P is classified by the first matching row. "Walk hits X" means some process in P's app walk (or in its
lineage entry, §5.7) has exe X.

| # | Condition | Verdict | Reason | User control |
|---|---|---|---|---|
| 1 | P is in the OS tree (strict or loose) of an `IM_RULE_INSTANCE` pid; or the walk hits `isshoni.exe` (source SELF); or P is `msedgewebview2.exe` whose `--user-data-dir` equals an `IM_RULE_WEBVIEW_DATA_DIR` value or lies below it (see the path rule after the table) | kept out | `isshoni` | locked |
| 2 | The walk hits an `IM_RULE_APP_EXCLUDE` with source BUILTIN | kept out | `builtin` | locked |
| 3 | The walk hits an `IM_RULE_APP_EXCLUDE` with source USER | kept out | `user` | toggle |
| 4 | The walk hits an `IM_RULE_APP_EXCLUDE` with source VIEWER | kept out | `viewer` | toggle (per-run override) |
| 5 | The walk hits an `IM_RULE_APP_ALLOW` | heard (if capturable, §6) | — | toggle |
| 6 | `IM_EXCLF_KEEP_OUT_MIC_USERS` is set, P's app root is a mic root (§5.6), and that app root is not the shared app root (§5.8) | kept out | `mic` | toggle (prompt) |
| 7 | P is a launcher (`IM_RULE_STOP`) and the engine is the include set | not captured | `launcher` | — |
| 8 | Otherwise | heard, or not captured with `tree` / `pid_reuse` / `limit` / `capture_failed` (§6) | — | toggle |

- The viewer row (4) sits above saved decisions (5) on purpose: a saved "Include" (row 5) means only "don't keep this
  out for mic use" and suppresses row 6, never row 4. Mic stickiness lasts the browser's lifetime (§5.6), so the
  natural answer to a browser mic prompt after Meet or Discord Web is a saved "Include"; if that also beat the viewer
  row, a tab of that browser playing an isshoni viewer (another room, "Watch here instead", a tab that joins later:
  the cases §5.9 says only the viewer row protects) would be captured and echoed to friends, against the plan's "never
  re-capture isshoni playback". The only way past row 4 is the per-run viewer override (§5.9 step 4), which removes
  that exe's row-4 rules from the rule set and writes no row-5 rule. Neither overrides a built-in voice app or isshoni
  (rows 1–2); M2 has no way to hear those (§25 question 3). The service writes a row-5 rule only for a saved "Include"
  decision.
- **WebView2 data-folder path rule** (row 1): WebView2 starts its browser process with
  `--user-data-dir=<the host's user data folder>\EBWebView`, a subfolder of the folder 03 passes. Both paths are
  normalized (`/` → `\`, trailing separators stripped) and compared case-insensitively (`CompareStringOrdinal` with
  `bIgnoreCase = TRUE`). P matches when the parsed value equals the rule value or starts with the rule value followed
  by `\` (a path-separator boundary, so `…\isshoni2` never matches `…\isshoni`).
- `IM_RULE_INSTANCE` with a pid that doesn't exist is ignored (and reported in `warnings` as `instance_gone`).
- The warnings of §5.8 attach to the row of the shared app.

### 5.4 Built-in data (`internal/media/audiorules`, Go, passed as rules)

Voice apps (source BUILTIN, locked). Each row lists the Windows exe names; `MacOS` and `Linux` fields exist empty for
M3/M4.

| App id | Name | Windows exe names |
|---|---|---|
| `discord` | Discord | `discord.exe`, `discordptb.exe`, `discordcanary.exe`, `discorddevelopment.exe` |
| `vesktop` | Vesktop | `vesktop.exe`, `equibop.exe` |
| `teamspeak` | TeamSpeak | `ts3client_win64.exe`, `ts3client_win32.exe` (TeamSpeak 3), `teamspeak.exe` (TeamSpeak 5 and 6) |
| `mumble` | Mumble | `mumble.exe` |
| `zoom` | Zoom | `zoom.exe` |
| `teams` | Microsoft Teams | `ms-teams.exe` (new Teams), `teams.exe` (classic) |
| `slack` | Slack | `slack.exe` |

Launchers and shells (`IM_RULE_STOP`, from S1 plus the plan's names): `explorer.exe`, `steam.exe`,
`epicgameslauncher.exe`, `battle.net.exe`, `battle.net launcher.exe`, `galaxyclient.exe`, `eadesktop.exe`,
`ealauncher.exe`, `upc.exe`, `riotclientservices.exe`, `heroic.exe`, `playnite.desktopapp.exe`,
`playnite.fullscreenapp.exe`, `xboxpcapp.exe`, `gamelaunchhelper.exe`, `overwolf.exe`, `itch.exe`, `services.exe`,
`svchost.exe`, `wininit.exe`, `winlogon.exe`, `userinit.exe`, `sihost.exe`, `runtimebroker.exe`, `dllhost.exe`,
`taskhostw.exe`, `applicationframehost.exe`, `cmd.exe`, `powershell.exe`, `pwsh.exe`, `windowsterminal.exe`,
`openconsole.exe`, `conhost.exe`, `wsl.exe`, `bash.exe`. A launcher is never an include target: its OS tree is
"everything started from it".

Shared runtimes (`IM_RULE_SHARED_RUNTIME`): `msedgewebview2.exe`.

Browsers (`IM_RULE_BROWSER`, with families for the viewer rule of §5.9):

| Family | Exe names |
|---|---|
| `chrome` | `chrome.exe`, `chromium.exe`, `brave.exe`, `vivaldi.exe`, `opera.exe`, `arc.exe`, `thorium.exe` |
| `edge` | `msedge.exe` |
| `firefox` | `firefox.exe`, `librewolf.exe`, `waterfox.exe`, `floorp.exe`, `zen.exe` |

All names are lower case, unique across lists (a test enforces it), and `msedgewebview2.exe` is not a browser. Other
voice apps (KOOK, QQ, WeChat, Signal, WhatsApp, Telegram, Element, in-browser voice) are caught by mic detection, as the
plan says. Every built-in gets a real-app check (§18.9): Discord in R1, TeamSpeak and Mumble in R6, and new Teams,
Zoom, Slack and Vesktop in R7, once on Win11 before `v0.2.0` for each app the owner can run. A failed check fixes the
exe list or the folder and shared-runtime rules; it never removes the app from the list (the plan keeps the built-ins
always out, and mic detection stays the backstop). An M2 built-in that hasn't been checked yet still ships and is
listed as "unverified" in the results table of `cmd/isshoni-audiotest/README.md`. A built-in added after M2 needs its
check before it ships.

### 5.5 Browsers

Every Chromium browser plays all tabs from one audio-service utility process (`--utility-sub-type=audio.mojom.AudioService`),
Firefox from its parent process; both sit in the browser's own folder, so the whole browser is one app. Tabs can't
be told apart. Consequences:
- A browser that uses the mic (Discord Web, Google Meet) is kept out as a whole by row 6. Its prompt is the browser
  variant (§14.6): "Your browser is using the mic (Discord Web?). Keep the whole browser out; friends won't hear any of
  its tabs. Tip: the Discord app works best."
- A browser playing the user's isshoni viewer is kept out by row 4 (§5.9), even after a saved "Include" for it.
- Opening Discord Web in a separate window that isshoni can exclude on its own: Later (plan).

### 5.6 Mic detection

- **Mic users** are processes with a non-expired session (active or inactive) on any active capture endpoint
  (`EnumAudioEndpoints(eCapture, DEVICE_STATE_ACTIVE)`), except isshoni's own tree. The **mic root** is that process's
  app root. S1 counted inactive sessions too, which is how it caught Steam.
- **Stickiness.** Once a process key is a mic root, it stays one until that process exits, even when its capture
  session goes away. Push-to-talk and voice-activity apps stop and start capture streams while the call's incoming
  voices keep playing; S1's 10 s window would let them leak.
- Processes keep their mic-root attribution through orphaning via the lineage cache (§5.7).
- Mic detection runs whenever the watcher runs (sheet open or live), independent of the flag: the flag only decides
  row 6. The report always carries `usesMic`.
- Loopback captures and virtual cables: process loopback is not a capture endpoint, so isshoni's and OBS's loopback
  captures never count. An app recording from "Stereo Mix" or a virtual cable does count (it gets a prompt).

### 5.7 PID reuse and orphans

- **Include targets** are opened with a `SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION` handle taken before
  activation and held while the stream lives. A PID can't be reused while a handle to its process is open, so a stream
  can never end up on a different process. `RegisterWaitForSingleObject(WT_EXECUTEONLYONCE)` on the handle posts
  "process exited" to the watcher, which removes the stream at once. The handle is opened after the snapshot; if its
  creation time differs from the snapshot's, the plan is recomputed.
- **No handle.** If `OpenProcess(SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION)` fails (an anti-cheat driver or a
  protected process stripping rights; rare, since most strip neither right), the target is activated anyway: dropping
  it would silently lose the shared game's sound, the main content. Without the handle, I3 is checked by snapshot: on
  every reconcile (≤ 1 s apart, §7.3) the target's pid must still be in the Toolhelp snapshot with the planned
  creation time (or, when the creation time is unknown, the same exe name and parent pid), otherwise the stream fades
  out at once; exit is detected through `session_disconnected` or that 1 s poll instead of the wait. The session row
  gets the note `no_handle` (§12.1). The window in which the pid could be reused unnoticed is at most one poll, and
  reuse needs the process to have exited first, which ends its own capture.
- **Parent links** are trusted only when the parent was created first (strict link, §5.2).
- **Lineage cache.** When a walk with intact links finds that P belongs to a kept-out app (rows 1–4, 6), the result
  is cached under P's process key for P's lifetime. A process whose parent exits later (orphan) keeps that attribution,
  so a voice app's helper with another exe name stays out after the main process dies (audiotest case `orphan`).
- Known limit: a helper orphaned before isshoni started, with an exe name that no rule names and no mic session, is
  classified on its own. Built-in voice apps play voice from processes with their own exe name (Discord's voice engine
  runs in `discord.exe`), so this doesn't affect them.

### 5.8 The shared app

- The **shared app root** is the app root of `shared_pid` (the process of the window being shared: 04's pick result
  `im_source_info.pid`, passed by Go through `SetSharedPID` from the pick on, §14.2; 0 for a monitor).
- Mic-based exclusion (row 6) never applies to the shared app root: games have their own voice chat, and their sound
  is what is being shared (plan). Instead the report adds warning `shared_app_uses_mic` to that row.
- Built-in, user and viewer rules (rows 2, 3, 4) still apply to the shared app, with warning `shared_app_kept_out`.
  Go offers "Include it" for user and viewer rules; for built-ins the text explains that voice apps are always kept out.

### 5.9 The user's isshoni viewer in a browser

Plan: a browser that has the user's isshoni viewer session open is excluded automatically, with the reason shown. M2
mechanism (the relay messages are 01's, the SPA side is 03's, §22):
1. `playback.stop` stops the user's own browser tabs from playing; this doc doesn't send it. In M2 the desktop SPA
   sends it when it joins a room, to the user's `full` connections, and tabs act on it only for that room (03 §12.2).
   A handoff from a relayed `share.request` (later (M4), 01 D16) sends it to the requesting tab only, never as a
   broadcast, so tabs on the user's other PCs keep playing. Tabs that act on it stop playing and show "Now watching
   in the isshoni app". Tabs in other rooms, tabs that join later and tabs whose user chose "Watch here instead" keep
   playing; steps 2–4 cover them. In the common case no browser plays isshoni audio at all.
2. A tab that plays reports `playback.state {playing, browser, os}` over the relay. The core receives it (01 §11.11)
   and emits `EventPlaybackState` with the sender's connection id; 03 calls
   `audio.Service.SetViewerBrowser(connID, family, os, playing)`. `family` is M1's `client.browser` value (`chrome`,
   `edge`, `firefox`, `safari`, `other`), `os` M1's `ClientOS`. A connection that leaves the user's connections
   (01's `user.connections`) counts as `playing: false` for its connID (03 makes that call).
3. If `os == "windows"` and the family is `chrome`, `edge` or `firefox`, the service adds `IM_RULE_APP_EXCLUDE`
   (source VIEWER, app id `exe:<name>`) for every exe of that family (§5.4) whose session is active in the latest
   report; if none is active yet, it re-checks each report for 30 s. `safari`, `other` and unknown values add nothing
   and show the banner `viewer-unknown-browser` (§14.7) while that connID plays. A family stays kept out while **any**
   connID of that family reports playing; `playing: false` for one connID clears only that connID.
4. The rules last until no connID of that family plays, the share ends, or the user picks "Include" on that row. A tab
   on another PC of the same user would otherwise mute a local browser for no reason; the row says why. **"Include" on
   a `viewer` row** sets a per-run, unsaved viewer override for that exe: it suppresses only the row-4 rules for that
   exe until isshoni quits, writes nothing to `appRules`, and leaves mic keep-out (row 6) in force. So dismissing a
   temporary viewer rule never makes Discord Web in that browser audible later. This override is the only thing that
   suppresses viewer rules: a saved "Include" for the browser (row 5, e.g. from an earlier mic prompt) does not, so the
   browser stays kept out while it plays an isshoni viewer (audiotest case 24).

### 5.10 Classification examples

| Process (parent) | Rules | Result |
|---|---|---|
| `discord.exe` (`update.exe` in the parent folder) → `discord.exe` audio child | built-in | both kept out (`builtin`), app id `discord` |
| `ms-teams.exe` → `msedgewebview2.exe` in `C:\Program Files (x86)\Microsoft\EdgeWebView\…` | built-in | WebView2 kept out (shared runtime joins Teams' app) |
| `steam.exe` (mic session) → `steamapps\common\Game\game.exe` | built-in, mic flag | Steam kept out (`mic`); game heard (Steam is a launcher) |
| `chrome.exe` → audio service utility with a capture session (Discord Web) | mic flag | whole Chrome kept out (`mic`), browser prompt |
| `msedge.exe` with a saved "Include", playing the user's isshoni viewer | viewer, user allow | kept out (`viewer`): row 4 beats the saved allow |
| `isshoni.exe` → `msedgewebview2.exe` → audio utility | implicit | kept out (`isshoni`) |
| `discord.exe` → `browser.exe` in another folder | built-in | browser heard (app boundary) |
| `launcherx.exe` (plays sound) → `discord.exe` | built-in | Discord kept out; launcherx not captured (`tree`: its OS tree holds Discord) |
| `game.exe` whose helper got the PID of Discord's exited `update.exe` | built-in | Discord kept out; game not captured with `pid_reuse` (loose link only), not `tree` |
| Shared window is `game.exe`, which holds the mic | mic flag, `shared_pid` | heard, warning `shared_app_uses_mic` |

## 6. Planner

The planner (`audiopolicy::plan`) runs on the watcher thread on every reconcile (§7.3). Inputs: a process snapshot,
the session list, the rules, mic and lineage state, the live streams, the mode and the probe result. Output: the engine
kind and its targets.

### 6.1 Engine selection

| Mode | Process loopback works (probe ok) | Excluded set is one tree and fast path allowed | Engine |
|---|---|---|---|
| any | no | — | `endpoint`: classic loopback of the default render endpoint (`eConsole`); nothing is excluded, banner (§15) |
| `IM_AUDIO_MODE_EVERYTHING` (reserved, later; refused with `IM_ERR_INVALID_ARG` in M2) | yes | — | `exclude-self`: one EXCLUDE stream on isshoni's tree (the fast path's code with R = isshoni) |
| `IM_AUDIO_MODE_EXCLUDE_APPS` | yes | yes | `exclude-self` (the fast path), target R (§6.4) |
| `IM_AUDIO_MODE_EXCLUDE_APPS` | yes | no | `include-set` (§6.2) |
| `IM_AUDIO_MODE_OFF` | — | — | `none` |

While the probe runs (≤ 600 ms after start, §11.2), the engine starts as if the probe passed: the include set never
leaks, and a failed probe switches to `endpoint`.

### 6.2 Include set (S1 `PlanIncludeSet`, extended)

1. Classify every process in the snapshot, not just the ones with sessions (an excluded process that isn't playing
   still makes its ancestors' trees unsafe). Let X be the excluded set (rows 1–4, 6).
2. For each render session process S with verdict "heard":
   - S is a launcher → not captured (`launcher`).
   - Some x ∈ X, x ≠ S, is in S's OS tree by strict links → not captured (`tree`, detail = x's app name).
   - Otherwise some x ∈ X is in S's OS tree only through the loose union (strict ∪ loose) → not captured
     (`pid_reuse`, detail = x's app name; counter `pidReuseBlocks`, §5.2). The safety outcome is the same as `tree`.
   - Otherwise S is a candidate target.
3. Drop a candidate whose OS ancestor (strict) is also a candidate or a live include target: that stream already covers
   it (session note `covered`, §12.1). This assumes `INCLUDE_TARGET_PROCESS_TREE` also captures descendants created
   after the ancestor's stream was activated; S1 tested late top-level apps and excluded helpers, never a late child of
   an included target, so the required case `late-child-heard` (§18.5, case 25) checks it. **Fallback if that case
   fails** on Windows: step 3 drops only candidates created before the ancestor's stream was activated (creation time
   < the activation's QPC time), and a later child gets its own stream (still subject to step 2).
4. Order the remaining targets: active sessions first, then by most recent activity, then oldest process first. Take at
   most **48** (§17). The rest are not captured (`limit`) until a slot frees.
5. **Commit check** (on the watcher thread, right before a newly activated stream joins the mixer): take a fresh
   snapshot and repeat step 2 for that target. The stream joins the mixer in `Preroll` (§9.2), so nothing it captured
   is mixed for the next 40 ms; any reconcile in that window that finds the target unsafe drops it without output.
6. **Running targets** are re-checked on every reconcile. A target that became unsafe (a kept-out process appeared in
   its tree), whose app became kept out, or whose process exited, fades out over the next 10 ms of its buffer (§9.4).
   A session that merely vanished while its process lives keeps its stream for 3 polls (3 s), as in S1
   (`kMissingPollsBeforeRemoval`).

### 6.3 Invariants (asserted in tests and, in debug builds, at runtime)

- I1: no running include stream has a kept-out process (strict ∪ loose) in its target's OS tree.
- I2: no include stream targets a launcher, a kept-out process or isshoni's tree.
- I3: a stream's target process key equals the key it was planned for (held handle).
- I4: in `exclude-self` (fast path), every kept-out process is in the OS tree of the target R, and no heard session
  process is.
- I5: at most one EXCLUDE or endpoint stream exists outside a 20 ms crossfade.

### 6.4 The fast path (`exclude-self`)

- **Condition**: the fast-path flag is set, and the excluded set X (every kept-out process, playing or not, including
  sticky mic roots and lineage entries) lies in the OS tree of one process R that is not a launcher, and no heard
  session process lies in R's tree. In practice R is `isshoni.exe` and X is isshoni's own tree: no voice app runs.
  (If a voice app launched isshoni, R is that voice app.)
- **Why**: one stream instead of N, every app heard from its first sample (no discovery latency), lower CPU.
- **At engine start** the condition is evaluated on the first snapshot and, if it holds, the fast path is used from
  the first chunk.
- **Entering** from the include set needs the condition to hold for 10 s without a break (Discord restarting doesn't
  flap the engine). The EXCLUDE stream opens and prerolls, then a 20 ms crossfade moves from the include mix to the
  EXCLUDE stream; the include streams then close.
- **Leaving** is immediate when a kept-out process appears outside R's tree (session event, mic event, or poll). While
  the fast path runs, a session event skips the 20 ms debounce of §7.3: the watcher refreshes only the new session's
  process and its ancestors and classifies it at once (≤ 5 ms), because the cut below must land inside the 40 ms
  preroll:
  - Let C be the QPC time of the event that revealed it (the `OnSessionCreated` callback, the capture-session event,
    or the poll). The EXCLUDE stream's ring stores QPC positions per packet (§8.3), so audio captured after `C − 20 ms`
    is still unmixed when detection took less than 20 ms of the 40 ms preroll. The ring is cut at `C − 20 ms` with a 5 ms
    fade ending at the cut, and the stream closes. An app can't play before its session exists, so the voice app's
    first sample is never mixed, **provided** the `OnSessionCreated` callback reaches the watcher at most 20 ms after
    the app's first rendered sample. That notification lag is not documented; case 5 measures it from the fixture's
    own logged first-render time and fails above 20 ms (§18.5, §18.6).
  - **Late isshoni descendants.** A process started under isshoni after the EXCLUDE stream was activated (a WebView2
    audio utility, a sound-check helper) must stay inside that stream's excluded tree. Case 26
    (`fast-path-late-child`) checks it; if it leaks, the fast path also exits (the same cut) on any new isshoni
    descendant's `session_created`, and re-enters only after the usual 10 s.
  - If the cut point was already mixed (detection slower than ≈ 20 ms, typically poll-found), the stream fades out over
    5 ms at once; counter `fastpathLateCuts` and event `fastpath_late_cut`.
  - Include streams open as usual; other apps return after ≈ 60–80 ms (activation plus preroll). Event `fastpath_exit`.
- **Kill switch**: settings key `audio.fastPath` (default `true`; no UI in M2: the release default or a hand edit of
  `settings.json`). If the audiotest's `fast-path-late-voice` or `fast-path-late-child` case fails on real hardware,
  the default becomes `false` for that release. The gate follows the shipped default (§18.10 "gate configuration"):
  with it `true`, cases 4, 5 and 26 are required; with it `false`, they run as INFO and don't block the gate.
- Known difference: launcher sounds (Explorer, Steam UI) and the Windows system-sounds session are heard in
  `exclude-self` but not in the include set. The report reflects the engine actually running.

### 6.5 Mode switches

| From → to | How | Artifact |
|---|---|---|
| include set ↔ exclude-self | §6.4 | 20 ms crossfade in; 60–80 ms gap out |
| any → endpoint (probe failed, or process loopback stops working twice in 10 s) | Open endpoint stream, preroll, 20 ms crossfade | banner |
| endpoint → process loopback (re-probe passed after a default-device change) | Same, reverse | banner removed |
| live → off (`im_audio_set_mode(OFF)`: "Share sound" off) | Fade out 10 ms, close the capture streams; the mixer keeps its clock and grid and feeds zeros, which 04's stage encodes as digital silence (04 §10.3); the watcher keeps running, so "What friends hear" stays current | none |
| off → exclude-apps (`im_audio_set_mode(EXCLUDE_APPS)`: "Share sound" on) | Plan and open streams as at start (§6.1), preroll, 10 ms fade in | ≈ 60–80 ms of silence |
| exclude-apps ↔ everything | reserved (later): the same crossfade as include set ↔ exclude-self | none |

## 7. Session watcher

One thread, COM MTA, owns all WASAPI enumeration, notifications, the process table and the planner. It is a small
event loop over one queue.

### 7.1 Queue events

`session_created(endpoint)`, `session_state(session, state)`, `session_disconnected(session, reason)`,
`device_event(kind, id)`, `process_exited(key)`, `activation_done(result)`, `stream_failed(stream, hr)`,
`rules_changed`, `mode_changed`, `request(kind, reply)`, `tick_100ms`, `tick_1s`. COM callbacks only enqueue (no COM
calls, no locks held across calls, no waiting), as Windows requires of `IMMNotificationClient` and session callbacks.

### 7.2 Endpoints and sessions

- On start and on every device event: `IMMDeviceEnumerator::EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE)` and
  `(eCapture, …)`; for each endpoint, `IMMDevice::Activate(IAudioSessionManager2)`, then `GetSessionEnumerator` (it
  must exist before `RegisterSessionNotification` delivers anything), then `RegisterSessionNotification`. Removed
  endpoints are unregistered from the watcher thread, never from inside a callback.
- Each session: `IAudioSessionControl2::GetProcessId` (accept `S_OK` and `AUDCLNT_S_NO_SINGLE_PROCESS`, skip pid 0),
  skip `IsSystemSoundsSession() == S_OK`, skip `AudioSessionStateExpired`; `RegisterAudioSessionNotification` with an
  `IAudioSessionEvents` sink for `OnStateChanged` and `OnSessionDisconnected`; `QueryInterface(IAudioMeterInformation)`
  for the activity meter (as EarTrumpet does; on failure `playing` falls back to the session state).
- Multi-process sessions (`AUDCLNT_S_NO_SINGLE_PROCESS`): the engine sees one pid; the other processes get no stream
  (not heard, never leaked). Warning `multi_process_session` on that row.

### 7.3 Reconcile

- Triggers: any session event (debounced **20 ms**, so a burst of sessions plans once; not debounced while the fast
  path runs, §6.4), `rules_changed` and
  `mode_changed` (immediately), `process_exited`, `activation_done`, `tick_1s` (the safety net that also refreshes the
  process table and re-checks I1 for every running target).
- Steps: refresh the process table (§7.4), update mic and lineage state, classify, plan (§6), start activations for new
  targets, mark removals, publish the report (`seq` + 1 if anything visible changed).
- Budget: one reconcile ≤ 5 ms with 300 processes and 40 sessions (excluding activations, which are asynchronous).

### 7.4 Process table

- `CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)` gives pid, parent pid and exe name for every process (protected ones
  included).
- For a pid not seen before (or whose parent or exe changed): `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)`,
  `GetProcessTimes` (creation), `QueryFullProcessImageNameW` (folder). For `msedgewebview2.exe` only:
  `NtQueryInformationProcess(ProcessCommandLineInformation = 60)` and parse `--user-data-dir=` (quoted or not),
  normalized as §5.3's path rule says. Failures leave the field empty: an unknown folder is an app boundary; an unknown creation time accepts the
  parent link (fail toward more matches).
- Refresh cost ≤ 3 ms for 300 processes (details cached per process key).

### 7.5 Device changes and the audio service

- `IMMNotificationClient` (`RegisterEndpointNotificationCallback`): `OnDefaultDeviceChanged(eRender, eConsole)`,
  `OnDeviceStateChanged`, `OnDeviceAdded`, `OnDeviceRemoved`.
- On a default render change: event `default_device_changed`; the endpoint stream (if that engine runs) reopens on the
  new default with preroll and fade; the probe result is invalidated (the next audio start re-probes; a live share in
  `endpoint` re-probes once, 2 s after the change, and may switch back to process loopback); include and EXCLUDE
  streams stay, and the silent-stream check of §8.5 watches them for 3 s.
- On any device event: rebuild the endpoint list and session registrations; reconcile.
- No active render endpoint: `state = "no-device"`, streams stay (they get invalidated or go quiet), the mixer keeps
  sending silent frames; when an endpoint appears, everything reconciles.
- A rebuild that takes longer than 1 s sets `rebuilding` in the report (banner `reconnecting-sound`).
- Audio service restart (`AUDCLNT_E_SERVICE_NOT_RUNNING` from any call): tear down all COM objects, wait 1 s, re-create
  the enumerator, registrations and streams; at most once per 5 s; event `audiosrv_restart`.
- Bluetooth headsets switching to the hands-free profile when a voice app opens the mic change the render endpoint;
  this path handles it, and friends still get 48 kHz (process loopback captures before the endpoint).

## 8. Capture streams

### 8.1 Activation

- Process loopback: `ActivateAudioInterfaceAsync(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, IID_IAudioClient, &blob, handler,
  &op)` with `AUDIOCLIENT_ACTIVATION_PARAMS{AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK, {pid,
  PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE or …_EXCLUDE_…}}`. The completion handler is agile (`IAgileObject`)
  and holds an extra reference until `ActivateCompleted` runs (S1's `CompletionHandler`).
- `ActivateAudioInterfaceAsync` is loaded from `mmdevapi.dll` with `LoadLibraryExW(…, LOAD_LIBRARY_SEARCH_SYSTEM32)`.
- Endpoint loopback: `GetDefaultAudioEndpoint(eRender, eConsole)` → `IMMDevice::Activate(IAudioClient)`.
- Activations run on two activator threads (MTA), at most **8** in flight, **3 s** timeout each (S1: 5 s; observed
  completions are milliseconds). The result goes back to the watcher, which runs the commit check (§6.2 step 5)
  before the stream joins the mixer. A stop aborts pending waits through an event (S1).

### 8.2 Initialize

- Process loopback (evented): `IAudioClient::Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK |
  AUDCLNT_STREAMFLAGS_EVENTCALLBACK [| AUTOCONVERTPCM | SRC_DEFAULT_QUALITY], 1 000 000 /* 100 ms */, 0, fmt, nullptr)`.
  Attempts in order, each on a freshly activated client (S1): float32 48 kHz stereo; PCM16; float32 + convert; PCM16 +
  convert. `GetMixFormat` is not used (process loopback doesn't support it).
- Endpoint loopback (polled every 5 ms): float32 + convert first, then PCM16 + convert, float32, PCM16; buffer 200 ms.
- Then `SetEventHandle` (evented), `GetService(IAudioCaptureClient)`, `Start`.
- The format that worked and the failed attempts go into the report (`sessions[].format`, `events`).

### 8.3 Rings with timestamps

- One lock-free single-producer/single-consumer ring per stream: **500 ms** of float32 stereo (producer: capture
  thread; consumer: mixer). On overflow the oldest frames are dropped (`overflows` counter).
- A side ring of `{first_frame_index, qpc_ns}` per captured packet, from `GetBuffer`'s `pu64QPCPosition` (100 ns
  units → ns). When the position is 0 or flagged `AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR`, the read time minus the frames
  already buffered is used. The mixer reads the QPC time of the ring head from it (§10.3) and the fast-path cut uses it
  (§6.4).
- `AUDCLNT_BUFFERFLAGS_SILENT` packets push zeros; `DATA_DISCONTINUITY` counts `discontinuities` and adds an event.

### 8.4 Capture thread

- One thread, MMCSS `AvSetMmThreadCharacteristicsW(L"Pro Audio")`, `WaitForMultipleObjects` on up to 63 stream events
  plus one control event (20 ms timeout). On wake it drains every stream whose `GetNextPacketSize` is non-zero (S1's
  loop). 48 include streams + 1 EXCLUDE/endpoint stream + control = 50 handles, so one thread suffices.
- PCM16 is converted to float on the way into the ring.
- COM objects of a stream are released on the capture thread (MTA), as S1 does.

### 8.5 Failures and health

- A `GetBuffer`/`GetNextPacketSize` failure (`AUDCLNT_E_DEVICE_INVALIDATED`, `AUDCLNT_E_RESOURCES_INVALIDATED`, …)
  marks the stream failed: it fades out (§9.4) and the watcher reopens it after 100 ms, 500 ms, then 2 s. After the third
  failure the app is not captured (`capture_failed`) until the next device or session event for it.
- **Silent-stream check** (the Windows counterpart of macOS's zero-buffer rule): a heard app whose activity meter peak
  exceeds −60 dBFS for 1 s while all its include streams deliver digital silence for that second gets its streams
  reopened, at most once per 5 s per app (`silentReopens`, event `silent_reopen`). Two failed reopens in a row make
  it `capture_failed` and raise the `capture-trouble` banner while it plays.
- Apps using WASAPI exclusive mode bypass the shared mixer; nothing can capture them. Their session (if any) shows
  `playing` while the stream is silent; after two silent reopens they show `capture_failed`. Documented limit.

## 9. Mixer

### 9.1 Clock

- One thread, MMCSS "Pro Audio", `CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION,
  TIMER_ALL_ACCESS)` (available on every supported build: the product floor is 19045, 04 `IM_OS_SUPPORTED`).
- Tick k is due at `tick0 + k × 10 ms` (QPC ns, computed, never accumulated from measurements). Each tick produces one
  chunk of 480 frames of 48 kHz float stereo.
- If the thread falls more than **100 ms** behind (suspend, debugger), it resynchronizes to now (S1), counts
  `mixerResyncs`, and starts a new timestamp grid (§10.4).

### 9.2 Channel states (one channel per stream)

| State | Enter | Leave |
|---|---|---|
| `Preroll` | stream joined, or after an underrun/trim | ≥ **40 ms** buffered → `Running`, gain ramps 0 → 1 over the first chunk (10 ms) |
| `Running` | from `Preroll` | buffered < **20 ms** → fade out over this chunk, back to `Preroll` (`underruns`); buffered > **100 ms** → fade out, trim to 40 ms, `Preroll` (`trims`); removal or failure → `Fading` |
| `Fading` | removal, failure, unsafe target | gain reached 0 (10 ms) → removed; the watcher frees it outside the mixer |

With adaptive resampling, underruns and trims are emergency paths; the target is zero of both in a 2-hour run (§17).

### 9.3 Adaptive resampling

- Every stream has a PI controller on its buffer fill. Fill is sampled at each tick and smoothed with a 1 s EWMA;
  every 100 ms the error `e = fill − 40 ms` updates the ratio:
  `ppm = clamp(Kp·e + Ki·∫e dt, −1000, +1000)` with Kp = 50 ppm/ms, Ki = 1 ppm/(ms·s), the integral clamped to
  ±400 ms·s, and a slew limit of 20 ppm per update. These are starting values; slice A4 tunes them in simulation
  against the acceptance numbers of §17.
- **Active streams** run through a speexdsp resampler, created on the watcher thread before the channel joins the
  mixer with `speex_resampler_init_frac(2, 1 000 001, 1 000 000, 48000, 48000, 5, &err)` (quality 5). Starting from a
  non-unity ratio makes speexdsp pick its interpolated filter and allocate that table (1 288 entries at quality 5:
  base length 80 × oversample 16 + 8) up front; `speex_resampler_init(…, 48000, 48000, …)` would reduce to 1:1, pick
  the 80-entry direct table, and `speex_realloc` the interpolated one on the mixer thread at the first non-zero ppm.
  The ratio is updated with `speex_resampler_set_rate_frac(st, 1 000 000 + ppm, 1 000 000, 48000, 48000)` (integer
  ppm; the ratio is input per output, so a too-full buffer drains), and **ppm is never 0**: the controller's output is
  clamped to |ppm| ≥ 1 (0 becomes +1, or −1 when the previous value was negative). At ppm = 0 the ratio reduces to
  1:1 again, which flips speexdsp back to the direct table (a recompute on the mixer thread) and truncates
  `samp_frac_num`, a sub-sample phase jump; with drift near zero ppm would cross 0 often. The filter length stays 80
  for every |ppm| ≤ 1000, so with these two rules updates allocate nothing on the mixer thread. The mixer asks it for
  exactly 480 output frames per tick.
- **Silent streams** (the last 100 ms digital silence) skip the resampler and correct the fill directly by dropping or
  padding zeros, up to 48 frames per tick: zeros are zeros, so this is exact and free. Most inactive-session streams
  live here. When a stream leaves this bypass, the mixer calls `speex_resampler_reset_mem(st)` before feeding it, so
  the filter history from before the silence never comes out at the onset (reset_mem only zeroes memory; it doesn't
  allocate).
- The endpoint stream uses the same controller.
- The 40 ms fill target is part of the audio's age at capture (01 §12.1). It may drop to 20–25 ms only if 01's
  sound-on g2g gate (01 §12.2, the `gate` run) misses, and only with A4's drift simulation (§17) still passing at the
  lower target (01 §12.1 lever 2); the change is then recorded here.

### 9.4 Fades and clicks (S1 as built)

- Every start and stop is a 10 ms linear gain ramp over audio that is still buffered (packets arrive in whole 10 ms
  blocks, so waiting for an empty ring would leave nothing to fade).
- A chunk that ends early while gain > 0 gets a 64-frame tail ramp from its last value to zero (backstop).
- Removal of a newly kept-out app takes effect from the next tick on its buffered audio: friends stop hearing it within
  ≈ 60 ms of the rule change (plus network).
- Crossfades between engines (§6.5) are 20 ms equal-gain.

### 9.5 Onset fade (the S1 click fix)

When a stream's input turns non-silent after at least **50 ms** of digital silence (|x| < 1e-6 on both channels), its
next **5 ms** (240 frames) are ramped in linearly. Windows drops the first milliseconds of an app that starts playing
right after a loopback attaches, so the app's own fade-in never arrives and the waveform starts mid-cycle (S1 run 2,
diagnosed from the WAV). Applied after resampling, before the channel gain.

The silence counter of a **new channel**, and of a channel re-entering `Running` from `Preroll`, starts **saturated**
(≥ 50 ms, as S1's `size_t silent_frames = kOnsetSilenceFrames;`), so its first non-silent sample always ramps. S1's
diagnosed click was exactly that case: a stream silent for only 30 ms after "running", then the tone at full level; a
counter starting at 0 would meet the 50 ms rule and still click.

### 9.6 Mix bus

- Sum of channels × gain × onset ramp.
- Soft clip above −1 dBFS: for |x| > t (t = 0.891), `y = sign(x)·(t + (1 − t)·tanh((|x| − t)/(1 − t)))`; continuous
  slope at t. Replaces S1's hard clamp.
- **Silence flags** (§4.3): `paused` (`Mixer::SetPaused`, from 04's `im_set_paused`), `muted` (`im_audio_set_muted`,
  01's pause mute) and mode off (`im_audio_set_mode(OFF)`, "Share sound" off). The bus gain is 0 while
  `paused || muted || off`, reached with a 10 ms fade; each flag is set and cleared only by its own call, so resuming
  a pause never re-enables sound that was switched off. The mixer keeps calling `OnChunk` with zeros on the unbroken
  grid, which 04's stage encodes as digital silence (04 §10.3).
- **Levels**: per channel and for the bus, peak and RMS over the last 100 ms (10 ticks), in dBFS with a −120 floor,
  published through a seqlock for `im_audio_levels_get`. Per-app levels sum the app's channels.
- **PCM tap** (`IM_AUDIOF_TAP_PCM`): 04's stage also emits each finished chunk as an `IM_PACKET_PCM` packet with the
  chunk's capture time (§4.2, §10.2).

## 10. Chunks, timestamps and the Opus stage

05 builds everything up to `PcmSink::OnChunk`; 04 builds the Opus stage, the PCM tap, the audio rings and the bridge to
`im_next_packet` (04 §6.2, §10.3). §10.1 and §10.2 are requirements 04's stage adopts; §10.3–§10.5 are 05's.

### 10.1 Requirements on 04's Opus stage (04 §10.3)

| Setting | Value |
|---|---|
| Create | `opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO, &err)` |
| `OPUS_SET_BITRATE` | `im_audio_config.bitrate_bps` at start (01 fills it from `ShareParams.AudioBitrate`: 128 000, or 256 000 for Movie); later `im_set_audio_bitrate(s, bps)`, which Go reaches as `Session.SetBitrate(media.LayerAudio, …)` |
| `OPUS_SET_VBR` / `OPUS_SET_VBR_CONSTRAINT` | 1 / 1 (constrained VBR) |
| `OPUS_SET_COMPLEXITY` | 10 |
| `OPUS_SET_SIGNAL` | `OPUS_SIGNAL_MUSIC` |
| `OPUS_SET_MAX_BANDWIDTH` | `OPUS_BANDWIDTH_FULLBAND` |
| `OPUS_SET_INBAND_FEC` | 0: these bitrates stay in CELT, where in-band FEC does nothing (plan "Audio"); audio NACK from the SFU cache covers loss |
| `OPUS_SET_DTX` | 0: continuous 20 ms frames keep the capture-time grid, the receivers' jitter buffers and A/V sync steady and simple, and digital silence costs a few bytes per frame under VBR. M1's SFU has no packet-liveness check (`stalled` means the pub PC has been down 2 s, `stalledAfter` in `internal/server/sfu/config.go`), so DTX would not stall a share either way |
| `OPUS_SET_PACKET_LOSS_PERC` | 0 (loss-adaptive tuning: later (M5)) |
| `OPUS_SET_LSB_DEPTH` | 24 |
| Frame | 20 ms = 960 samples per channel, two mixer chunks; `opus_encode_float` into a 1 275-byte buffer |
| Lookahead | `OPUS_GET_LOOKAHEAD` at init (312 samples, 6.5 ms, for this application); the stage subtracts it from packet times (§10.3) |

The stage runs inline on the mixer thread inside `OnChunk` (≈ 0.3–0.5 ms per frame, 04 §10.3); §17's CPU budget
includes it. The SDP side (`minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1` offered by the M1 publisher;
`maxaveragebitrate` from the SFU's answer) is unchanged from M1; the encoder targets the same number the answer
carries.

### 10.2 Requirements on 04's packets and rings

- **Framing**: one `IM_PACKET_AUDIO` per 20 ms (two chunks). Its `capture_ts_ns` is its first chunk's time minus the
  lookahead (§10.3).
- **Gaps**: after `OnGap(t)` the stage drops a half-filled frame, and the next packet carries `IM_PKT_REINIT` (a new
  timestamp grid, §10.4); so does the first packet of a session. Whether the encoder is reset is 04's choice.
- **Rings** (04 §6.2): a per-stream audio ring of 128 slots (2.56 s) and 256 KiB. On overflow the producer drops the
  newest packet, sets `IM_PKT_DISCONTINUITY` on the next delivered one, counts it in `im_get_stats` `audio.drops` and
  raises `RING_OVERFLOW`; audio older than 1 s is dropped at dequeue. The grid is unaffected: dropped packets leave
  gaps in Go's RTP timestamps, never shifts.
- **Silence**: nothing playing, paused, muted or sound off → the stage keeps encoding the zeros it gets every 20 ms on
  the unbroken grid (digital silence, a few bytes per packet; 04 §10.3). While a share is paused the core drops audio
  at the pump anyway (01 D14).
- **Bitrate** changes are live (`OPUS_SET_BITRATE`, no reset, no reinit).
- **PCM tap** (`IM_AUDIOF_TAP_PCM`): in addition to Opus, each chunk becomes one `IM_PACKET_PCM` in 04's PCM ring
  (128 slots), stamped with the chunk's capture time (no lookahead); Opus packets are unchanged.

### 10.3 Capture timestamps

- **What a chunk time means.** The QPC time at which the Windows audio engine mixed the chunk's first sample: the
  loopback packet's QPC position (`pu64QPCPosition`, §8.3), which is the same device-position clock a render client's
  `IAudioClock::GetPosition` reports. It does **not** add the sharer's output-device latency
  (`IAudioClient::GetStreamLatency`, Bluetooth delay): friends never hear the sharer's speakers, and video capture
  times are DWM's composition times (WGC `SystemRelativeTime`, DD `LastPresentTime`, 04 §12.3), not the display's, so
  both media stamp the same depth of the OS pipeline. (Adding the stream latency to L would, with
  `chunk time = tick − L`, move stamps *earlier* than the mix, about 2 × the latency too early: the wrong sign.)
- **Delay estimate L.** For every running channel the mixer measures `d = tick_time − qpc(ring head)` at each pop
  (§8.3), i.e. how long ago the audio engine mixed the audio it mixes now, and keeps a 1 s EWMA of it per channel. The
  measured value of the mix is `L_meas` = the median over running channels of those EWMAs. A typical `d` is the ring's
  40 ms preroll fill plus one 10 ms capture packet, so ≈ 50 ms.
- **Start value.** Chunks are stamped from the very first chunk with the fixed **`L0 = 50 ms`**. When the first chunk
  is due no channel has prerolled yet (the mixer starts inside `im_start`, §10.5), so there is nothing to measure.
  L is never taken from the running EWMA directly (that would break the exact 10 ms spacing) and never jumps at a
  fixed time (that would step the grid during sound, the common case being a game already playing at share start).
  A measured value replaces L only through the silence re-anchor below. If sound plays throughout a share, L0 stays,
  and the stamp error is `|L_true − L0|`, about 10 ms or less.
- **Re-anchor during silence.** While the mix has been digital silence since the last anchor, a newly running stream
  may re-measure L (its `L_meas` after its preroll); if the new value L' differs from L by more than 5 ms, the grid
  restarts (`OnGap`, §10.4). Nobody hears a jump in silence. A new grid's first chunk time is
  `max(tick − L', previous chunk time + 10 ms)`, so its first packet time is at least
  `max(tick − L' − lookahead, previous packet time + 20 ms)`. If the clamp binds, L moves only part of the way (to
  `tick − chunk time`) and is re-measured at the next silence. Chunk and packet times never decrease (01 §5.3).
  L is always ≥ 0 (`d` is positive by causality; a negative measurement, from a bad timestamp, is clamped to 0), so
  04's start-at-`epoch_ns` drop rule (04 §10.3) holds for any L.
- **Chunk time** (what 05 passes to `OnChunk`) = `tick_k − L`, on the exact 10 ms grid. **Packet time** = the chunk
  time of its first chunk − lookahead; the subtraction happens in 04's stage.
- So consecutive chunks differ by **exactly 10 000 000 ns** and packets by **exactly 20 000 000 ns**, and Go's
  `rtp_ts = base + rtpTicks(capture − t0)` (M1 publisher; `rtpTicks` rounds `(Δns × 48 000 + 5e8) / 1e9`) advances by
  exactly 960. Go must not adjust audio timestamps (§22, 01).
- Accuracy target: the mix time of a sound (its device-position time, as `im_test_render_bursts` predicts it from
  `IAudioClock`, §13.2) and its chunk time agree within **15 ms** (median) with ≤ 5 ms spread (audiotest case
  `timestamps`), which leaves ≥ 30 ms of the plan's 45 ms A/V budget to video. The A/V offset of real shares (real
  WGC or DD frames against real loopback audio) is measured by `enginetest avsync` on the owner's PC (§19.3):
  |median offset| ≤ 20 ms.

### 10.4 Grid discontinuities

A new grid starts (the mixer calls `sink->OnGap(t)` with the new grid's first chunk time, and 04's stage marks the next
packet `IM_PKT_REINIT`) on exactly three occasions: the first chunk after `Mixer::Start`; a mixer resync (§9.1); a
silence re-anchor (§10.3). The new grid never starts earlier than the previous chunk + 10 ms (§10.3). Go keeps its RTP
mapping (one t0 for all tracks, 01 §8.2): the new grid's first packet gets `rtp_ts = base + rtpTicks(CaptureNS − t0)`,
so the jump goes out as is (M1's `rtpTicks` rounds `(Δns × 48 000 + 5e8) / 1e9`, so steps within a grid are exactly
960); no marker bit is set (01's choice; M1 sets the marker on video only). Engine switches (§6.5), sound off and on,
pause, device changes and stream reopenings never break the grid and never call `OnGap`.

### 10.5 Implementing 04 §10.2 (the C++ boundary inside the DLL)

05 implements the classes 04 §10.2 declares, in `native/windows/src/audio/`, on top of `im_audiopolicy` and
`im_audiomix`:

```cpp
namespace im::audio {
// 04 provides PcmSink {OnChunk, OnGap} and MixerConfig {mode, flags, sink}.

class AudioEngine {                          // process-wide; AudioEngine::Get()
 public:
  static AudioEngine& Get();
  virtual im_status Init(uint32_t init_flags) = 0;   // im_init (04's worker): starts the watcher thread unless
                                                     // IM_INIT_NO_AUDIO; <= 100 ms
  virtual void Shutdown() = 0;                       // im_shutdown: Mixer::Stop, unwatch, joins every audio thread
                                                     // <= 2 s; Init may follow
  virtual uint32_t ProbeFlags() = 0;                 // im_caps.audio_flags (§11.1); cached, <= 50 ms the first time
  virtual int32_t ListAppsJson(uint32_t flags, char* buf, int32_t cap) = 0;   // im_list_audio_apps (§12.1), snprintf
  virtual im_status SetRules(const im_exclusions* ex) = 0;   // im_set_exclusions, im_start_config.exclusions; copies
  virtual im_status Watch(const im_exclusions* ex) = 0;      // im_audio_watch
  virtual im_status Unwatch() = 0;                           // im_audio_unwatch
  virtual im_status SetMode(uint32_t mode) = 0;              // im_audio_set_mode
  virtual im_status SetMuted(bool muted) = 0;                // im_audio_set_muted
  virtual im_status LevelsGet(im_audio_levels* out) = 0;     // im_audio_levels_get (§12.2), lock-free
  virtual int32_t SoundCheck(uint32_t helper_pid, int32_t timeout_ms, char* buf, int32_t cap) = 0;   // §11.3
  virtual std::unique_ptr<Mixer> NewMixer() = 0;
};

class Mixer {
 public:
  virtual ~Mixer() = default;
  virtual im_status Start(const MixerConfig& cfg) = 0;  // 04's worker inside im_start; returns once the mixer clock
                                                        // runs (<= 100 ms); the startup probe goes on
                                                        // asynchronously (§11.2)
  virtual void Stop() = 0;                              // <= 500 ms; no OnChunk or OnGap after it returns
  virtual void SetPaused(bool paused) = 0;              // from 04's im_set_paused: the pause flag of §9.6
  virtual uint32_t State() const = 0;                   // IM_AUDIO_STATE_*: the report's state (§12.1)
};
}
```

- **Threads.** `Init` starts only the watcher thread (MTA, §7). The two activator threads, the capture thread and the
  mixer thread start with the first `Watch` or `Mixer::Start` and stop 5 s after neither runs. `Shutdown` stops them
  all; a thread stuck in a driver call is abandoned with a log record after 2 s, never killed (04 §6.3's rule).
- **`Mixer::Start`** applies `cfg.mode` (`IM_AUDIO_MODE_EXCLUDE_APPS`; 04's export checks refuse the reserved
  `_EVERYTHING` with `IM_ERR_INVALID_ARG` before a mixer exists, README §3 item 15) and `cfg.flags`
  (`IM_AUDIOF_*`), starts the watcher if it isn't watching, starts the mixer clock and returns; the probe (§11.2) and
  the first activations continue asynchronously, so the first chunks are silence (§17). The shared process comes from
  `im_exclusions.shared_pid` (Go's `SharedPID`, from the pick).
- **The mixer thread** calls `sink->OnChunk(frames, tick_k − L)` every 10 ms and never blocks on it. It calls
  `sink->OnGap(t)` exactly when §10.4 starts a new grid (the first chunk, a resync, a re-anchor), and never on device
  changes, engine switches or sound off/on, which keep the grid.
- **`Mixer::SetPaused(bool)`** sets the pause flag, which the mix bus ORs with the mute and mode off (§9.6). It never
  touches the other two.
- **No events.** There is no event sink and there are no audio events or audio error codes (04 §10.1, §10.2): the
  report's `seq`, `state` and `events[]` and the levels' `report_seq` carry everything, and Go polls them (§14.4).

## 11. Probe, fallback and sound check

### 11.1 `im_probe`: `im_caps.audio_flags` (no sound, at app start)

04 declares four bits; their meaning is this doc's. `AudioEngine::ProbeFlags()` computes them in ≤ 50 ms, without
sound, and caches them until the next device event.

| Bit | Set when |
|---|---|
| `IM_AUDIO_PROCESS_LOOPBACK` (0x1) | an INCLUDE stream on the DLL's own pid activates and initializes (S1's `im_audio_probe`) |
| `IM_AUDIO_ENDPOINT_LOOPBACK` (0x2) | endpoint loopback of the default render endpoint initializes |
| `IM_AUDIO_SESSION_EVENTS` (0x4) | session notifications register on every active render endpoint (the probe registers and unregisters once when the watcher isn't running) |
| `IM_AUDIO_MIC_DETECTION` (0x8) | at least one active capture endpoint is enumerated |

Neither loopback bit means backend `none`: no active render endpoint, or the audio service is unavailable. Endpoint
counts are in the report's `device` (§12.1); the OS build is `im_caps.os_build`. The product floor is build 19045
(04 `IM_OS_SUPPORTED`), so there is no pre-1803 path and no timer capability bit.

### 11.2 Startup probe (each audio start)

1. Open an INCLUDE stream on the DLL's own pid (activation ≤ 500 ms).
2. On the default render endpoint, open a shared render client (float32 48 kHz stereo, `AUTOCONVERTPCM`) with a fixed
   private session GUID (`AudioSessionGuid` of `Initialize`); set its `ISimpleAudioVolume` to 1.0, unmuted (so a user's
   Volume Mixer setting for isshoni can't fail the probe). (Timestamps don't use its `GetStreamLatency`, §10.3.)
3. Play **200 ms** of **997 Hz** at **−60 dBFS** with 10 ms raised-cosine ramps (barely audible; the loopback is digital
   and pre-volume, S1).
4. Goertzel (Hann) on the captured window [tone start + 20 ms, tone end − 20 ms]:
   - level in [−66, −54] dBFS → `ok`;
   - level below −80 dBFS → `failed` (silent loopback);
   - between → `ok` with warning `probe_level_mismatch`.
5. Activation failure or `E_NOTIMPL` → `unsupported`. No render endpoint, or the render client
   can't initialize (another app holds the device exclusively) → `skipped`: nothing can play or be captured right now,
   the engine keeps its mode, and the next device event re-probes.

The probe runs in parallel with video start, takes ≤ 600 ms, and its result is cached per default render endpoint id
for the process lifetime (a failed result is retried once per audio start). `IM_AUDIOF_TEST_FAIL_PROBE` forces
`failed`. `failed`/`unsupported` → engine `endpoint` (§6.1) and banner `exclusion-unavailable` or `windows-too-old`;
in `endpoint`, isshoni's own playback is captured too, so the desktop app mutes its viewer while live (§22, 03) and the
banner `viewer-muted` explains it.

### 11.3 Sound check (first-run wizard, plan step 3)

1. `audio.Service.SoundCheck` (§14.2) with `SelfInfo.Exe` set starts `<Exe> --sound-check-helper` (03's `main` only
   dispatches the flag to `audio.RunSoundCheckHelper()`, before Wails starts) and calls
   `im_audio_sound_check(helper_pid, 3000, …)`. With `Exe == ""` (the fake engine on the Mac, `e2ehost` on CI ubuntu,
   the audiotest) it starts no process and calls the engine with `helper_pid = 0`: the DLL then plays the chime itself
   from a private render session (like §11.2's probe tone, at the chime's level) and captures its own process.
2. **`func RunSoundCheckHelper() int`** lives in `internal/desktop/audio`. On Windows (`helper_windows.go`) it waits
   400 ms, plays the chime built into the binary (an 800 ms WAV generated in memory by `chimeWAV()`: two partials,
   880 Hz and 1320 Hz, −14 dBFS, 48 kHz 16-bit stereo, 10 ms ramps) with `PlaySoundW(SND_MEMORY | SND_SYNC)` and
   returns 0, the process exit code. Other OSes (`helper_other.go`) return 2 and play nothing. The helper is an isshoni
   descendant, so it is never heard in a live share; the sound check opens its own stream on the helper's pid, outside
   the policy.
3. **Capture path**: an INCLUDE stream on the helper's pid when `audio_flags` has `IM_AUDIO_PROCESS_LOOPBACK`. When it
   lacks that bit but has `IM_AUDIO_ENDPOINT_LOOPBACK`, or the INCLUDE activation returns unsupported (`E_NOTIMPL`, or
   no format initializes), the chime is captured with endpoint loopback of the default render endpoint and the result
   says `"backend":"endpoint"`: sound is captured, but voice apps can't be kept out on this PC (the degraded case of
   the plan's step 3, which 03's wizard shows).
4. **Pass**: Goertzel level of both partials ≥ −40 dBFS within the timeout.
5. **Waiting**: the call is exempt from §4.3's 3 s limit and waits `timeout_ms + 500`, so a slow but valid check is not
   cut off; only then does it return `IM_ERR_TIMEOUT`.
6. **Result JSON**: `{"captured":true,"levelDb":-14.3,"ms":1460,"backend":"process-loopback"}` (or `"endpoint"`), or
   `{"captured":false,"reason":"silent|activation_failed|no_device|device_busy|timeout","backend":"…"}`. `backend` is
   `process-loopback`, `endpoint` or `none`. `device_busy`: another app holds the default render device in exclusive
   mode (`AUDCLNT_E_DEVICE_IN_USE`). `timeout`: the helper's session never appeared within `timeout_ms`.

## 12. Reports

### 12.1 `im_list_audio_apps` JSON (v1)

```json
{
  "v": 1,
  "seq": 118,
  "qpcNs": 81234567890123,
  "mode": "exclude-apps",
  "engine": "include-set",
  "state": "ok",
  "muted": false,
  "paused": false,
  "live": true,
  "rebuilding": false,
  "probe": {"result": "ok", "osBuild": 26100, "format": "float32", "levelDb": -60.1, "ms": 231},
  "device": {"present": true, "renderEndpoints": 2, "captureEndpoints": 1},
  "apps": [
    {"id": "exe:spotify.exe", "name": "Spotify", "exe": "spotify.exe", "verdict": "heard", "reason": "",
     "playing": true, "usesMic": false, "browser": false, "shared": false, "pids": 4, "sessions": 1,
     "streams": 1, "detail": "", "warnings": []},
    {"id": "discord", "name": "Discord", "exe": "discord.exe", "verdict": "kept_out", "reason": "builtin",
     "playing": true, "usesMic": true, "browser": false, "shared": false, "pids": 6, "sessions": 2,
     "streams": 0, "detail": "", "warnings": []},
    {"id": "exe:chrome.exe", "name": "Google Chrome", "exe": "chrome.exe", "verdict": "kept_out", "reason": "mic",
     "playing": true, "usesMic": true, "browser": true, "shared": false, "pids": 14, "sessions": 1,
     "streams": 0, "detail": "", "warnings": []},
    {"id": "exe:eldenring.exe", "name": "ELDEN RING", "exe": "eldenring.exe", "verdict": "heard", "reason": "",
     "playing": true, "usesMic": true, "browser": false, "shared": true, "pids": 1, "sessions": 1,
     "streams": 1, "detail": "", "warnings": ["shared_app_uses_mic"]},
    {"id": "exe:launcherx.exe", "name": "LauncherX", "exe": "launcherx.exe", "verdict": "not_captured",
     "reason": "tree", "playing": false, "usesMic": false, "browser": false, "shared": false, "pids": 1,
     "sessions": 1, "streams": 0, "detail": "Discord", "warnings": []}
  ],
  "sessions": [
    {"pid": 4120, "created": "133725183442731234", "exe": "spotify.exe", "appId": "exe:spotify.exe",
     "flow": "render", "state": "active", "multiProcess": false, "verdict": "heard", "reason": "",
     "stream": "running", "format": "float32", "note": ""}
  ],
  "warnings": [{"code": "multi_process_session", "app": "exe:foo.exe"}],
  "counters": {"streams": 7, "activations": 9, "activationFailures": 0, "underruns": 0, "trims": 0,
    "overflows": 0, "discontinuities": 0, "mixerResyncs": 0, "fastpathExits": 0,
    "fastpathLateCuts": 0, "silentReopens": 0, "deviceChanges": 0, "pidReuseBlocks": 0, "maxResamplePpm": 41,
    "audioCpuMsPerS": 6.2},
  "events": [{"tNs": 81234560000000, "code": "stream_running", "app": "exe:spotify.exe", "detail": ""}]
}
```

| Field | Values |
|---|---|
| `mode` | `exclude-apps`, `off`; `everything` is reserved (later) |
| `muted`, `paused` | the `im_audio_set_muted` flag and the pause flag (`im_set_paused`), §9.6 |
| `engine` | `include-set`, `exclude-self`, `endpoint`, `none` |
| `state` | `starting` (probe running), `ok`, `degraded` (probe failed/unsupported), `no-device`, `stopped` (watcher not running: one-shot snapshot). `Mixer::State()` returns the matching `IM_AUDIO_STATE_*` (04: `STARTING`, `OK`, `DEGRADED`, `NO_DEVICE`; `OFF` for `stopped`) |
| `probe.result` | `pending`, `ok`, `failed`, `unsupported`, `skipped` |
| `apps[].verdict` | `heard`, `kept_out`, `not_captured`, `pending` (seen < 20 ms ago, not yet classified; never captured) |
| `apps[].reason` | kept out: `builtin`, `user`, `mic`, `viewer`, `isshoni`. Not captured: `launcher`, `tree`, `pid_reuse` (blocked only through a loose PID link, §5.2), `limit`, `capture_failed`. Heard: `""` |
| `apps[].detail` | for `tree` and `pid_reuse`: the kept-out app's name; else `""` |
| `apps[].warnings` | `shared_app_uses_mic`, `shared_app_kept_out`, `included_uses_mic`, `multi_process_session` |
| `apps[].playing` | session active within the last 2 s, or its activity meter above −70 dBFS within the last 2 s |
| `sessions[].created` | FILETIME as a decimal string (JSON numbers lose precision above 2^53) |
| `sessions[].stream` | `none`, `activating`, `preroll`, `running`, `fading` |
| `sessions[].note` | `""`; `covered` (captured by an ancestor's stream, §6.2 step 3); `no_handle` (an include target running without a held process handle, §5.7) |
| `events[].code` | `session_created`, `session_expired`, `stream_opened`, `stream_running`, `stream_removed`, `stream_failed`, `activation_failed`, `fastpath_enter`, `fastpath_exit`, `fastpath_late_cut`, `default_device_changed`, `device_added`, `device_removed`, `no_device`, `audiosrv_restart`, `probe_ok`, `probe_failed`, `mixer_resync`, `underrun`, `trim`, `silent_reopen`, `rules_changed`, `muted`, `unmuted`, `mode_changed`, `reanchor` |

`counters` are the mixer's; packets dropped after the mixer are 04's (`im_get_stats` `audio.drops`). Caps: `apps` ≤ 128
rows, `sessions` ≤ 256, `events` = the last 300, strings ≤ 260 bytes. Rows: apps with a session,
plus running processes matched by a built-in rule (so "Discord · kept out" shows while Discord is silent), plus the
shared app. `seq` increases whenever anything except levels, counters, `qpcNs` and `events` changes. Unknown fields are
ignored by Go (additive changes only).

### 12.2 Levels

`im_audio_levels_get` fills `im_audio_levels` (§4.1): one entry per app id with a stream or a session, `source = 0`
(captured, post-fade, what friends hear) for apps with running streams while live, `source = 1` (activity: the session
peak meter, sampled every 100 ms) otherwise, including kept-out apps (their meter proves the right app is out). At most
64 entries, most recently active first.

## 13. Go side: types, binding, fake

### 13.1 `internal/media/audio.go` (all OSes)

OS-neutral types only: stdlib, no build tags, no `x/sys` (01 D1). The Opus bitrate is set with 01's
`Session.SetBitrate(media.LayerAudio, bps)` (`LayerAudio = "audio"`, 01 §5.1); this doc declares no layer constant.

```go
package media

type AudioMode string

const (
	AudioExcludeApps AudioMode = "exclude-apps" // IM_AUDIO_MODE_EXCLUDE_APPS: the M2 mode
	AudioOff         AudioMode = "off"          // IM_AUDIO_MODE_OFF: SetAudioMode only ("Share sound" off)
	AudioEverything  AudioMode = "everything"   // IM_AUDIO_MODE_EVERYTHING: reserved (later); rejected in M2 (CodeInvalid)
	AudioTestTone    AudioMode = "test-tone"    // IM_AUDIO_MODE_TEST_TONE (100, 04 §10.3): AudioConfig.Mode at start only;
	                                            // tests, mediatest.Conformance and enginetest; no capture
)

type RuleKind uint32 // values = IM_RULE_*
const (
	RuleAppExclude RuleKind = iota
	RuleAppAllow
	RuleInstance
	RuleStop
	RuleSharedRuntime
	RuleBrowser
	RuleWebViewDataDir
)

type RuleSource uint32 // values = IM_RULE_SRC_*
const (
	SourceBuiltin RuleSource = iota
	SourceUser
	SourceSelf
	SourceViewer
	SourceData
)

type Rule struct {
	Kind   RuleKind
	Source RuleSource
	Value  string // exe name, pid or path
	AppID  string // "" = "exe:" + Value
	Name   string // "" = from the exe
}

type Exclusions struct {
	KeepOutMicUsers bool
	FastPath        bool
	Rules           []Rule // <= 512
	SharedPID       uint32 // 0 = none
}

// AudioConfig is used as media.Config.Audio *AudioConfig (01 §5.1): nil = no audio (IM_AUDIO_MODE_OFF at im_start).
// One type, declared here; 01 declares no AudioConfig of its own.
type AudioConfig struct {
	BitrateBPS int        // Opus target in bit/s; the core fills it from ShareParams.AudioBitrate (0 = 128000)
	Exclusions Exclusions // from audio.Service.AudioConfig() (§14.2)
	Mode       AudioMode  // "" = AudioExcludeApps; AudioTestTone = 04's tone source (the one carrier of the test tone);
	                      // AudioOff, and AudioEverything (reserved) in M2, rejected (CodeInvalid)
	TapPCM     bool       // audiotest only: IM_AUDIOF_TAP_PCM (Session.Next then also returns KindPCM packets, 01 §5.1)
	TestFlags  uint32     // audiotest only: IM_AUDIOF_TEST_FAIL_PROBE
}

// AudioReport mirrors §12.1 (json tags camelCase, as the DLL writes them); AudioSession, AudioCounters, AudioEvent
// likewise. Parse with ParseAudioReport, which rejects v != 1 and ignores unknown fields.
type AudioReport struct { /* fields of §12.1 */ }

// AudioStatus is 01's name for "What friends hear" (Session.AudioStatus, core events).
type AudioStatus = AudioReport

// AudioApp is one apps[] row of §12.1. 01 relies on ID, Name and UsesMic.
type AudioApp struct {
	ID, Name, Exe, Verdict, Reason, Detail string
	Playing, UsesMic, Browser, Shared       bool
	PIDs, Sessions, Streams                 int
	Warnings                                []string
}

func ParseAudioReport(b []byte) (AudioReport, error)

type AudioLevel struct {
	AppID         string
	PeakDB, RMSDB float32
	Captured      bool // source 0
}

type AudioLevels struct {
	ReportSeq           uint32
	QPCNanos            int64 // engine clock of the sample (im_audio_levels.qpc_ns)
	MixPeakDB, MixRMSDB float32
	Apps                []AudioLevel
}

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

// ErrAudioNotRunning: neither watching nor live (IM_ERR_BAD_STATE from an audio call).
var ErrAudioNotRunning = &Error{Code: CodeAudioNotRunning}
```

Errors follow 01 §5.2 (`*media.Error{Code, Op, Detail}`, `Error.Is` by code). This doc adds two codes to 01's
`ErrorCode` list, declared once in 01's `errors.go`: `CodeAudioNotRunning ErrorCode = "audio_not_running"` and
`CodeTimeout ErrorCode = "timeout"`. Status names are the header's (§4.1, 04 §5.2):

| Native status | From | Go error |
|---|---|---|
| `IM_ERR_BAD_STATE` | `im_audio_levels_get`, `im_audio_set_mode`, `im_audio_set_muted`, and any audio call after `IM_INIT_NO_AUDIO` | `&Error{Code: CodeAudioNotRunning, Op, Detail}`, so `errors.Is(err, media.ErrAudioNotRunning)` holds. This is the audio exception to 04 §12.2's table, which maps `IM_ERR_BAD_STATE` from other calls to `internal`; `internal/media/native` declares no sentinels of its own |
| `IM_ERR_INVALID_ARG` | `im_set_exclusions`, `im_audio_watch`, `im_audio_set_mode` | `&Error{Code: CodeInvalid, Op, Detail: im_last_error}`, e.g. `Op: "set_exclusions"`; matches 01's `ErrInvalid` |
| `IM_ERR_TIMEOUT` (−15) | the COM wait of `im_list_audio_apps` and `im_audio_sound_check` | `&Error{Code: CodeTimeout, Op}`, e.g. `Op: "list_audio_apps"` (01's `CodeTimeout` unwraps to `context.DeadlineExceeded`, 04 §12.2); never retried |
| `IM_ERR_ABI` | a struct the DLL doesn't accept | `&Error{Code: CodeDependency, Reason: "abi", Op, Detail}` (01 §5.2, README §3 item 17: an installation mixing two releases gets 03's reinstall text) |
| others | — | 01 §7.2's mapping |

`Op` is the export's name without `im_`. **Mode validation** happens in Go, without a call, in the binding's single
mode mapping (`audio.go`, §13.2):
- `AudioConfig.Mode` (at `im_start`): `""` and `AudioExcludeApps` → `IM_AUDIO_MODE_EXCLUDE_APPS`, `AudioTestTone` →
  `IM_AUDIO_MODE_TEST_TONE` (100); `AudioOff`, `AudioEverything` (reserved; rejected in M2, README §3 item 15) and
  anything else → `CodeInvalid`. The DLL refuses `IM_AUDIO_MODE_EVERYTHING` too (`IM_ERR_INVALID_ARG`, 04 §10.2), so
  neither layer can turn the built-in list off.
- `SetAudioMode`: `AudioOff` → `IM_AUDIO_MODE_OFF`, `AudioExcludeApps` → `IM_AUDIO_MODE_EXCLUDE_APPS`; anything else
  (including `AudioEverything`, which has no M2 caller, and `AudioTestTone`, which exists only at start) →
  `CodeInvalid`.

`AudioTestTone` is declared here, once, so OS-neutral code (01's `mediatest.Conformance` with its `TestTone` option,
the fake, `enginetest -audio tone`) requests the tone without importing the Windows binding: the tone is
`media.Config.Audio = &AudioConfig{Mode: AudioTestTone}`. There is no `TestFlags` bit and no separate field for it, and
`internal/media/native` declares no mode constant of its own. The fake treats `AudioTestTone` like its default audio
(M1's tone packets, §13.3).

Golden fixtures: `internal/media/testdata/audio/report-*.json` (normal, degraded, no-device, fast path, many apps,
browser mic, shared mic) are parsed in Go tests and produced by the C++ report writer's tests (the same files), so the
two sides can't drift.

### 13.2 The Windows binding (`internal/media/native/audio.go`, `audio_windows.go`)

- **`audio.go`** (no build tag, 01 §7.7): the C-layout mirrors `imRule`, `imExclusions`, `imAudioLevel` and
  `imAudioLevels` (sizes and offsets of §4.1 asserted in `layout_test.go`); `buildExclusions` (the struct, the rule
  array and a NUL-terminated byte slice per string, all heap memory kept alive with `runtime.KeepAlive` until the call
  returns: the DLL copies everything, §4.3); the status → error table of §13.1; the single mode mapping of §13.1 (used
  by 04's `media.Config` → `imStartConfig` conversion for `AudioConfig.Mode` and by `SetAudioMode`); level decoding;
  report decoding through `media.ParseAudioReport`. The methods of `media.AudioController` are implemented here, on
  the engine type `native.Open` returns (01 §7.1, 04 §12.2), calling procs through the `caller` interface, so all of
  this is tested on the Mac and CI ubuntu with 01 §7.7's `caller` fake.
- **`audio_windows.go`** only adds the audio procs to the set `native.Open` resolves on the DLL handle it loaded
  (absolute path, `LoadLibraryEx(…, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32)`, 01 D4, 04 §12.4)
  and calls them with 04's `call_windows.go` helpers: `SyscallN` with the `im_last_error` read on the same locked OS
  thread. No `LazyDLL`, no purego. It also holds `AudioTestHooks` (below).
- **Buffers**: `im_list_audio_apps` and `im_audio_sound_check` go through 01 §7.3's buffer helper under 04's snprintf
  rule (64 KiB first, one retry with `n + 1` bytes, at most 4 MiB). No second buffer rule.
- **Serialization**: the audio exports are control calls under 01 §7.3's mutex, never under the pump, except
  `im_audio_sound_check` and the one-shot `im_list_audio_apps`, which may block for seconds and run outside it (the
  DLL is thread-safe, §4.3).
- **Bitrate**: 01's `Session.SetBitrate(media.LayerAudio, bps)` maps to `im_set_audio_bitrate(s, bps)`.
- **Packets**: Opus packets become `media.Packet{Kind: KindAudio, Layer: "", CaptureNS, Data, Reinit}` (04 §12.3);
  `IM_PACKET_PCM` packets become `media.Packet{Kind: KindPCM, CaptureNS, Data}` (01 §5.1). Only a config with
  `TapPCM` produces them, and the core never sets it, so the pump and the publisher never see PCM.
- **`AudioTestHooks`** (Windows only; `cmd/isshoni-audiotest/driver_windows.go` is its one user):

  ```go
  // AudioTestHooks returns the im_test_* exports of the engine native.Open returned; for any other media.Engine an
  // error wrapping media.ErrUnsupportedOS.
  func AudioTestHooks(e media.Engine) (*AudioTest, error)

  func (t *AudioTest) RenderTone(endpointID string, hz, amp float32, d time.Duration) error
  func (t *AudioTest) RenderBursts(endpointID string, hz, amp float32, burst, period time.Duration, n int) (onsetEngineNS []int64, err error)
  func (t *AudioTest) OpusDecode(pkt []byte, pcm []float32) (frames int, err error) // pcm: interleaved stereo, len >= 2*5760
  func (t *AudioTest) EngineClockNS() (int64, error)                              // im_clock().mono_ns
  ```

  There is no audio-specific clock export. Burst onsets are in the engine clock (QPC ns, the `im_clock` domain); the
  harness maps them to `media.Packet.CaptureNS` with `offset = EngineClockNS() − Session.Now()` sampled back to back
  (01 §7.5, 04 §12.3), then compares them with the tap's `KindPCM` packets.

  **How `im_test_render_bursts` predicts onsets** (independently of the mixer, so a timestamp mistake can't appear on
  both sides): it opens its own shared-mode render client (float32 48 kHz stereo, `AUTOCONVERTPCM`, private session
  GUID, its own `ISimpleAudioVolume` at 1.0) and writes zeros with a burst (`burst_ms` of `hz`, 1 ms raised-cosine
  ramps) every `period_ms`, starting ≥ 200 ms after `Start`. For each burst it knows the stream frame index `f_k` of
  its first sample. After every buffer write it calls `IAudioClock::GetPosition(&pos, &qpc)` (position in
  `IAudioClock::GetFrequency` units, `qpc` in 100 ns units) and converts the pair to (frame, engine-clock ns); the
  prediction is `onset_k = qpc_ns + (f_k − pos_frames) × 1e9 / 48000`, taken from the last sample before the burst's
  first frame is played (`pos_frames ≤ f_k`). This is the device-position time, the same clock the loopback's QPC
  positions use (§10.3); no stream latency is added. The call returns after the last burst has played.

### 13.3 The fake (`internal/media/fake/audio_ctl.go`)

`*fake.Engine` (not `*fake.Session`) implements `media.AudioController` with scripted scenarios, so 03 builds the share
sheet on the Mac and `internal/desktop/audio` is tested without Windows. It works before any `Start`: the share sheet
watches before a share. `SetAudioMode` and `SetAudioMuted` need a running session with audio (else
`ErrAudioNotRunning`, as native) and are recorded in 01's call log (`Session.Calls()`, Op `"audio_mode"` or
`"audio_muted"`, in two fields 05 adds to `fake.Call`: `Mode media.AudioMode` and `Muted bool`), so the core's pause
test can assert them. The scenario comes from `fake.EngineConfig.AudioScenario string` (default `"normal"`; the
desktop app's dev build reads `ISSHONI_FAKE_AUDIO`; never in release builds). `Session.AudioStatus` (01) returns the
same scripted report unless a test set one with `SetAudioStatus`.

| Scenario | Script (seconds since watch start) |
|---|---|
| `normal` (default) | t0: Spotify heard (levels: −18 dB ± 6 sine at 0.5 Hz), Discord kept out (builtin, activity −24 dB), Steam kept out (mic, silent). t5: Chrome starts using the mic → kept out (mic, browser) |
| `degraded` | engine `endpoint`, probe `failed`, everything heard including Discord |
| `degrade-live` | as `normal` until 5 s after a session with audio starts (seconds since that start, not since watch start); then process loopback "fails twice in 10 s" (§6.5): engine `endpoint`, state `degraded`, probe `failed`, events `stream_failed` ×2 and `probe_failed`, everything heard including Discord, so `FriendsHear` gains the error-level banner `exclusion-unavailable` during the live share (03's e2e of the tray, pill and toast, §14.7) |
| `old-windows` | probe `unsupported`, osBuild 18363 |
| `no-device` | state `no-device`, no apps playing |
| `fast-path` | only Spotify and a game, engine `exclude-self`; t8: Discord starts → `include-set` |
| `shared-mic` | shared app `exe:valorant.exe` uses the mic, heard with `shared_app_uses_mic` |
| `many` | 60 apps (row limit and scrolling) |

`SetExclusions` re-derives verdicts with the same precedence (a small Go port of §5.3 over the scripted apps, enough
for the fake). `SetAudioMode` validates modes as §13.1 does; `SetAudioMode(AudioOff)` makes the report's `mode`
`off`; `SetAudioMuted` sets `muted`. `SoundCheck`
answers after 1.5 s (the test clock): `{captured: true, backend: "process-loopback", levelDb: -14}`; `degraded` and
`old-windows` → `backend: "endpoint"`; `no-device` → `{captured: false, reason: "no_device", backend: "none"}`. Its
tone Opus packets are unchanged from M1, whatever `AudioConfig.Mode` is (`AudioTestTone` included).

## 14. Go side: the desktop policy service (`internal/desktop/audio`)

The package builds and tests on Linux, macOS and Windows (03 D2): Windows-only code sits in `_windows.go` files with
stubs in `_other.go` (the ducking read of §14.7, the sound-check helper of §11.3). `e2ehost` runs this service on CI
ubuntu against the fake engine.

### 14.1 Settings (device-wide, in 03's settings store under key `audio`)

```json
{
  "keepOutMicUsers": true,
  "appRules": {"exe:kook.exe": "keep-out", "exe:chrome.exe": "include"},
  "fastPath": true,
  "dismissedTips": []
}
```

| Key | Default | Notes |
|---|---|---|
| `keepOutMicUsers` | `true` | "Keep out apps that use my microphone" (plan default) |
| `appRules` | `{}` | app id → `keep-out` or `include`. Ids of built-ins and `isshoni` are ignored (locked). At most 256 entries; the oldest decision is dropped beyond that. Per-run viewer overrides (§5.9 step 4) are never stored here |
| `fastPath` | `true` | the kill switch `audio.fastPath` (§6.4); no UI in M2: the release default or a hand edit of `settings.json` |
| `dismissedTips` | `[]` | tip codes (§14.7) |

There is no persisted sound mode: the built-in list can't be turned off (plan), and the per-share **Share sound**
switch (03, `SetShareSound`) is the only "off". Unknown keys are ignored and written back unchanged.

### 14.2 API

```go
package audio // internal/desktop/audio

type Decision string // "keep-out" | "include" | "auto" (auto = forget)

type SelfInfo struct {
	PID            uint32 // os.Getpid()
	WebViewDataDir string // 03: the WebView2 user data folder the host sets (WebView2 adds \EBWebView, §5.3)
	Exe            string // the binary started with --sound-check-helper (03: os.Executable()); "" = no helper
}

type SettingsStore interface {
	AudioSettings() Settings
	SaveAudioSettings(Settings) error
}

type Settings struct {
	KeepOutMicUsers bool                `json:"keepOutMicUsers"`
	AppRules        map[string]Decision `json:"appRules"`
	FastPath        bool                `json:"fastPath"`
	DismissedTips   []string            `json:"dismissedTips"`
}

// AppRule is one remembered decision (Settings → Audio in 03).
type AppRule struct {
	ID       string   `json:"id"`       // "exe:kook.exe"
	Name     string   `json:"name"`     // the display name; the exe name when the display name is unknown
	Decision Decision `json:"decision"` // "keep-out" | "include"
}

func New(eng media.AudioController, st SettingsStore, self SelfInfo, log *slog.Logger) *Service
func (s *Service) Run(ctx context.Context) error // the polling loop; returns when ctx ends
func (s *Service) Close() error

// Wrapped one-to-one by 03's bridge.Audio service (same names, 03 §6.2), which the SPA calls.
func (s *Service) FriendsHear() FriendsHear
func (s *Service) AppRules() []AppRule // from appRules, sorted by name
func (s *Service) Watch(on bool) error  // the share sheet is open
func (s *Service) WatchLevels(on bool)  // a level meter is on screen; reference-counted
func (s *Service) Decide(promptID string, d Decision) error // "keep-out" | "include" only
func (s *Service) DismissPrompt(promptID string) error      // keeps the app out for this run; saves nothing
func (s *Service) SetAppDecision(appID string, d Decision) error
func (s *Service) SetKeepOutMicUsers(on bool) error
func (s *Service) DismissTip(code string) error
func (s *Service) SoundCheck(ctx context.Context) (media.SoundCheckResult, error)

// Called by 03's App and share controller (Go), not by the SPA.
func (s *Service) Exclusions() media.Exclusions
func (s *Service) AudioConfig() media.AudioConfig // for core.Options.AudioConfig (01)
func (s *Service) SetSharedPID(pid uint32)
func (s *Service) OnShareStarted(sharedPID uint32, withAudio bool)
func (s *Service) OnShareStopped()
func (s *Service) SetPaused(paused bool)
func (s *Service) SetShareSound(on bool) error
func (s *Service) SetViewerBrowser(connID, family, os string, playing bool)
func (s *Service) Subscribe(fn func(Event)) (cancel func())

type Event struct {
	Name    string // "audio.report" | "audio.levels" | "audio.prompt" | "audio.alert"
	Payload any    // FriendsHear | Levels | Prompt | Banner
}

var (
	ErrUnknownPrompt   = errors.New("audio: unknown prompt")   // Decide, DismissPrompt: answered, dismissed or gone
	ErrLockedApp       = errors.New("audio: locked app")       // SetAppDecision: a built-in voice app or isshoni
	ErrInvalidAppID    = errors.New("audio: invalid app id")   // SetAppDecision: not "exe:<lower-case exe name>"
	ErrInvalidDecision = errors.New("audio: invalid decision") // Decide, SetAppDecision
	ErrUnknownTip      = errors.New("audio: unknown tip")      // DismissTip
)
```

- **`AudioConfig()`** returns `{Mode: media.AudioExcludeApps, Exclusions: the current rules including SharedPID}`
  and first flushes the 50 ms debounce of §14.3 (a pending rebuild calls `SetExclusions` at once), so the DLL and the
  start config agree. `BitrateBPS` stays 0: the core fills it from `ShareParams.AudioBitrate` (01). 03 sets
  `core.Options.AudioConfig` to it; the core calls it for a share whose `ShareRequest.Audio` is set and leaves
  `media.Config.Audio` nil otherwise. `Exclusions()` is `AudioConfig().Exclusions`.
- **`SetSharedPID(pid)`** rebuilds the rules (debounced 50 ms) with `SharedPID = pid`; `OnShareStarted` uses it too.
  03 calls it after `Share.Pick` with `Source.PID`, and with 0 on Release or pick expiry, so the first-share sheet
  (after the pick, before Start: the moment the user learns what leaves the PC) already shows the shared app first and
  its `shared_app_uses_mic` / `shared_app_kept_out` warnings.
- **Order around a share** (03's share controller): `OnShareStarted(uint32(req.Source.PID), req.Audio)` right before
  `core.StartShare`; `OnShareStopped()` when `Share.Done()` closes, whoever ended the share (the user, source gone,
  the server, unlinked). `OnShareStarted` sets `SharedPID`, `soundOn = withAudio` and `paused = false`, and starts the
  watcher if the sheet hasn't. `OnShareStopped` drops the viewer rules (§5.9), sets `SharedPID` to 0 and stops the
  watcher unless `Watch(true)` holds it; per-run viewer overrides stay until isshoni quits.
- **`SetPaused(paused)`** is idempotent and makes no engine call. 03's share controller calls it on every pause and
  resume, including screen lock and unlock and a pause from another device that 01's snapshot reports. The engine side
  of pause is not the Service's: 01's core pauses the Session (04's `im_set_paused`, which fades audio through
  `Mixer::SetPaused`) and mutes it with `AudioController.SetAudioMuted` (01 D14, §11.8); the DLL ORs these flags with
  mode off (§9.6). `SetPaused` only updates `FriendsHear` at once (`paused`, the `sound-off` banner with cause
  `paused`) instead of waiting for the next report.
- **`SetShareSound(on)`** is the per-share **Share sound** switch. During a live share with audio it calls
  `SetAudioMode(AudioOff)` for off and `SetAudioMode(AudioExcludeApps)` for on (§6.5). Pause and sound are separate
  engine flags (§4.3), so friends hear silence while `paused || !soundOn`, and a resume keeps a switched-off share
  silent. During a share started without audio there is no track to turn on: it only records the value, makes no
  engine call, and `FriendsHear.mode` stays `off` (03 disables the checkbox for such a share). Outside a share it does
  nothing and returns nil.
- **`WatchLevels(on)`** adds or removes one watcher of the meters (never below 0); §14.4 sets the polling rate from the
  count.
- **`SoundCheck`** (§11.3): with `SelfInfo.Exe` it starts `<Exe> --sound-check-helper` (`exec.CommandContext`; on
  Windows with `CREATE_NO_WINDOW`), calls `eng.SoundCheck(ctx, pid, 3 s)` and waits for the helper (killed after 5 s);
  with `Exe == ""` it calls `eng.SoundCheck(ctx, 0, 3 s)` and starts nothing.
- **`SetViewerBrowser(connID, family, os, playing)`**: §5.9 steps 2–4.

| Method | Errors |
|---|---|
| `Decide` | `ErrUnknownPrompt` (no pending prompt with that id); `ErrInvalidDecision` (`d` is not `keep-out` or `include`: `auto` and anything else are refused; a dismissal is `DismissPrompt`) |
| `DismissPrompt` | `ErrUnknownPrompt` (no pending prompt with that id) |
| `SetAppDecision` | `ErrInvalidAppID` (not `exe:` + a lower-case exe file name of 5–255 bytes ending in `.exe`, without `\ / : * ? " < > \|` or control characters); `ErrLockedApp` (a built-in voice app's id or one of its `exe:` ids, `isshoni`, `exe:isshoni.exe`); `ErrInvalidDecision` (`d` is not `keep-out`, `include` or `auto`) |
| `DismissTip` | `ErrUnknownTip` (not a tip code of §14.7) |
| others | the settings store's or the engine's error, wrapped |

03's bridge maps `ErrUnknownPrompt` and `ErrUnknownTip` to `not_found`, and the others to `invalid_argument` with
`params.field` = `appId` (`ErrLockedApp`, `ErrInvalidAppID`) or `decision` (`ErrInvalidDecision`).

### 14.3 Rules builder

Every call that changes inputs rebuilds `media.Exclusions` and calls `SetExclusions` (debounced 50 ms; `AudioConfig()`
flushes the debounce):
1. Self: `RuleInstance(PID)`, `RuleAppExclude("isshoni.exe", SourceSelf, "isshoni", "isshoni")`,
   `RuleWebViewDataDir(dir)` with the host's folder (the DLL also matches folders below it, which covers WebView2's
   `\EBWebView`, §5.3).
2. Data: `RuleStop` per launcher, `RuleSharedRuntime("msedgewebview2.exe")`, `RuleBrowser` per browser exe.
3. Built-ins: `RuleAppExclude(exe, SourceBuiltin, id, name)` per voice-app exe.
4. User: `keep-out` → `RuleAppExclude(exe, SourceUser)`; `include` → `RuleAppAllow(exe, SourceUser)` (for `exe:<name>`
   ids).
5. Viewer: `RuleAppExclude(exe, SourceViewer)` per §5.9, skipped only for exes with a per-run viewer override. A saved
   `include` decision doesn't skip them (it becomes a row-5 allow rule, which the viewer row 4 beats, §5.3), so a
   browser included after a mic prompt is still kept out while it plays an isshoni viewer. **"Include" on a row whose
   reason is `viewer`** (`SetAppDecision(id, "include")`) sets that override for the exe: it suppresses only these
   row-4 rules until isshoni quits and doesn't write `appRules`, so mic keep-out (row 6) still applies to that browser.
6. Flags from settings; `SharedPID` from `SetSharedPID` and `OnShareStarted`.

`Watch(true)` calls `WatchAudio` with the current rules; `Watch(false)` calls `UnwatchAudio` unless a share is live.

### 14.4 Polling and events

- Watching or live, levels watch count above 0: `AudioLevels` every 100 ms and `audio.levels` at 10 Hz.
- Watching or live, count at 0: `AudioLevels` every 250 ms, only to see `ReportSeq` change; no `audio.levels`.
- When `ReportSeq` changes: fetch `AudioReport`, rebuild `FriendsHear`, emit `audio.report` (at most 4 per second).
- Not watching and not live: no polling at all.
- New prompts emit `audio.prompt` too, so 03 can show a Windows notification while the window is hidden.
- **`audio.alert`** (payload: the `Banner`): an error-level banner (`exclusion-unavailable`, `windows-too-old`, §14.7)
  appeared while a share is live (it was absent in the previous `FriendsHear`, or the share just started with it). At
  most once per banner code per share; a banner that goes and comes back within the same share alerts again only
  after 60 s. 03 turns it into a toast, whatever the window's state (§14.7). It is never emitted outside a live share
  (the sheet shows the banner then).
- 03 forwards the events unchanged as `isshoni:audio.report`, `isshoni:audio.levels`, `isshoni:audio.prompt` and
  `isshoni:audio.alert` (03 §6.3).

### 14.5 `FriendsHear` (the share sheet's data; JSON through Wails, camelCase)

```ts
interface FriendsHear {
  seq: number;
  live: boolean;                       // a share is running
  mode: 'exclude-apps' | 'off';        // 'off' while the live share has no sound (started without, or Share sound off)
  engine: 'include-set' | 'exclude-self' | 'endpoint' | 'none';
  muted: boolean;                      // the report's muted
  paused: boolean;                     // SetPaused, or the report's paused
  keepOutMicUsers: boolean;
  heard: AppRow[];                     // shared app first, then playing, then name
  keptOut: AppRow[];
  notCaptured: AppRow[];
  prompts: Prompt[];                   // oldest first, at most 3
  banners: Banner[];                   // degraded modes: always shown, not dismissable
  tips: Tip[];                         // dismissable
  voiceApps: { id: string; name: string; running: boolean }[]; // "Friends won't hear: Discord, TeamSpeak…"
  alert: Alert;                        // what the tray and the pill show outside the window (§14.7)
}
interface Alert {
  voiceAppsHeard: boolean;             // live, and an error-level banner (exclusion-unavailable, windows-too-old) shows
  questions: number;                   // = prompts.length (pending mic prompts)
}
interface AppRow {
  id: string;
  name: string;                        // local only, never sent anywhere
  reason: 'builtin' | 'user' | 'mic' | 'viewer' | 'isshoni' | 'launcher' | 'tree' | 'pid-reuse' | 'limit'
        | 'capture-failed' | '';
  detail?: string;                     // 'tree', 'pid-reuse': the kept-out app's name
  playing: boolean;
  usesMic: boolean;
  browser: boolean;
  shared: boolean;
  control: 'locked' | 'toggle';
  decision: 'auto' | 'keep-out' | 'include';
  warning?: 'shared-uses-mic' | 'shared-kept-out' | 'included-uses-mic';
}
interface Prompt { id: string; appId: string; name: string; kind: 'mic' | 'browser-mic'; since: number }
interface Banner {
  code: 'exclusion-unavailable' | 'windows-too-old' | 'no-output-device' | 'reconnecting-sound'
      | 'sound-off' | 'capture-trouble' | 'viewer-muted' | 'viewer-unknown-browser';
  level: 'error' | 'warning' | 'info';
  params?: Record<string, string | number>;
}
interface Tip { code: 'ducking' }
interface Levels {
  seq: number;
  mixPeakDb: number;
  mixRmsDb: number;
  apps: Record<string, { peakDb: number; source: 'captured' | 'activity' }>;
}
```

Example (Discord in a call, KOOK just opened the mic, sharing a game window):

```json
{
  "seq": 42, "live": true, "mode": "exclude-apps", "engine": "include-set", "muted": false, "paused": false,
  "keepOutMicUsers": true,
  "heard": [
    {"id": "exe:eldenring.exe", "name": "ELDEN RING", "reason": "", "playing": true, "usesMic": false,
     "browser": false, "shared": true, "control": "toggle", "decision": "auto"},
    {"id": "exe:spotify.exe", "name": "Spotify", "reason": "", "playing": true, "usesMic": false,
     "browser": false, "shared": false, "control": "toggle", "decision": "auto"}
  ],
  "keptOut": [
    {"id": "discord", "name": "Discord", "reason": "builtin", "playing": true, "usesMic": true,
     "browser": false, "shared": false, "control": "locked", "decision": "auto"},
    {"id": "exe:kook.exe", "name": "KOOK", "reason": "mic", "playing": true, "usesMic": true,
     "browser": false, "shared": false, "control": "toggle", "decision": "auto"}
  ],
  "notCaptured": [],
  "prompts": [{"id": "p3", "appId": "exe:kook.exe", "name": "KOOK", "kind": "mic", "since": 1792575123000}],
  "banners": [],
  "tips": [{"code": "ducking"}],
  "voiceApps": [{"id": "discord", "name": "Discord", "running": true}, {"id": "teamspeak", "name": "TeamSpeak",
    "running": false}, {"id": "mumble", "name": "Mumble", "running": false}],
  "alert": {"voiceAppsHeard": false, "questions": 1}
}
```

03's Settings → Audio renders `AppRules()` for remembered decisions (including apps that aren't running) and
`FriendsHear.voiceApps` for the built-ins; its Change control calls `SetAppDecision(id, 'keep-out' | 'include' |
'auto')`.

### 14.6 Prompts

- A prompt appears for an app id with reason `mic` and decision `auto`, once per app id per app run. Kind
  `browser-mic` when the row is a browser, else `mic`.
- While unanswered the app stays kept out (the default "Keep out apps that use my microphone").
  **`DismissPrompt(id)`** (03's card close button and toast dismissal) leaves it kept out, doesn't ask again this run,
  saves nothing (`appRules` unchanged, so the next app run asks again) and removes the prompt from `FriendsHear`.
- `Decide(id, "keep-out" | "include")` saves the decision in `appRules` (remembered per app, plan) and rebuilds rules;
  errors as §14.2 lists.
- An app with decision `include` that uses the mic is heard and carries warning `included-uses-mic`.
- With `keepOutMicUsers` off: no prompts; mic users are heard with `included-uses-mic`.
- While the window is hidden, 03 shows the prompt as a toast with the `desktop.native.toast.*` texts (§14.8).

### 14.7 Banner and tip rules

| Code | Shown when | Level |
|---|---|---|
| `exclusion-unavailable` | probe `failed`, or `unsupported` with `osBuild >= 19041` | error |
| `windows-too-old` | probe `unsupported` with `osBuild < 19041` (defensive: below 19045 the app doesn't share, 04 `IM_OS_SUPPORTED`) | error |
| `no-output-device` | state `no-device` | warning |
| `reconnecting-sound` | `rebuilding` true | info |
| `sound-off` | during a live share: `!soundOn` (the report's mode `off`) → `params.cause = "off"`; else paused (`SetPaused` or the report's `paused`) or the report's `muted` → `params.cause = "paused"` | info |
| `capture-trouble` | a heard, playing app is `capture_failed`; `params.app` = its name | warning |
| `viewer-muted` | engine `endpoint` while live (03 mutes the in-app viewer) | info |
| `viewer-unknown-browser` | a viewer tab on Windows that plays reported family `safari`, `other` or an unknown value | warning |
| tip `ducking` | the ducking preference is not 3 ("Do nothing") or absent, a mic user or voice app runs, and the tip isn't dismissed | — |

The ducking preference is read in `internal/desktop/audio/ducking_windows.go` from
`HKCU\Software\Microsoft\Multimedia\Audio\UserDuckingPreference` (REG_DWORD; absent = Windows' default, "Reduce the
volume of other sounds by 80%"). `ducking_other.go` (Linux, macOS: e2ehost, CI) reports "unknown", which hides the tip.

**Outside the window** (plan: "Degraded modes are always shown, never silent"). A sharer in a game sees neither the
hidden window's banners nor, under Windows' automatic Do-not-disturb rules for games and full-screen apps, most toasts.
So during a live share 05 gives 03 two more signals, and 03 renders them (03 §11.1 tray, §11.4 pill, §15 toasts):
- `FriendsHear.alert` (§14.5): `voiceAppsHeard` while an error-level banner shows, for the tray icon (03's
  `tray-warn`), its tooltip and the tray menu's first line, and the pill's short text, all
  `desktop.native.audio.voiceAppsHeard` ("Friends hear voice apps"); `questions` for the tray menu item
  `desktop.native.audio.questionsWaiting` that opens the window on the prompt card, and the pill's amber marker.
- `audio.alert` (§14.4): one toast `desktop.native.toast.voiceAppsHeard` when an error-level banner appears during the
  live share (a §15 toast row in 03, setting: always).

A mid-share switch to `endpoint` (§6.5: process loopback stops working twice in 10 s) is the case these exist for:
without them the sharer wouldn't know their Discord call is on the stream.

### 14.8 English text (for 03's `web/src/i18n/lazy/desktop.en.json`, namespace `desktop`)

| Key | Text |
|---|---|
| `desktop.audio.banner.exclusionUnavailable` | Voice apps can't be kept out on this PC, so friends hear everything it plays, including your calls. Set your voice app to a different output device, or turn sound off. |
| `desktop.audio.banner.windowsTooOld` | Update Windows to keep voice apps out. Until then, friends hear everything this PC plays. |
| `desktop.audio.banner.noOutputDevice` | No speakers or headphones are connected. Friends hear nothing until you connect one. |
| `desktop.audio.banner.reconnectingSound` | Your sound device changed. Reconnecting sound… |
| `desktop.audio.banner.soundOff` | Sound is off. Friends hear nothing. |
| `desktop.audio.banner.soundPaused` | Sharing is paused. Friends hear nothing. |
| `desktop.audio.banner.captureTrouble` | Friends can't hear {{app}} right now. isshoni keeps trying. |
| `desktop.audio.banner.viewerMuted` | Sound from shares you watch is off while you share sound, so friends don't hear themselves. |
| `desktop.audio.banner.viewerUnknownBrowser` | isshoni is playing in a browser on this PC. Pause it there, or friends may hear themselves. |
| `desktop.audio.prompt.mic` | {{app}} is using your microphone. Keep it out of your stream? |
| `desktop.audio.prompt.browserMic` | Your browser is using the mic (Discord Web?). Keep the whole browser out; friends won't hear any of its tabs. Tip: the Discord app works best. |
| `desktop.audio.prompt.keepOut` / `desktop.audio.prompt.keepBrowserOut` / `desktop.audio.prompt.include` | Keep out / Keep browser out / Include |
| `desktop.native.toast.micPrompt` | {{app}} is using your microphone. Keep it out of your stream? |
| `desktop.native.toast.browserMicPrompt` | Your browser is using the mic (Discord Web?). Keep the whole browser out; friends won't hear any of its tabs. Tip: the Discord app works best. |
| `desktop.native.toast.keepOut` / `desktop.native.toast.keepBrowserOut` / `desktop.native.toast.include` | Keep out / Keep browser out / Include |
| `desktop.audio.warning.sharedUsesMic` | {{app}} is the app you're sharing and it uses your microphone. Friends will hear its voice chat. |
| `desktop.audio.warning.sharedKeptOut` | You're sharing {{app}}, but its sound is kept out. |
| `desktop.audio.warning.sharedVoiceApp` | {{app}} is a voice app, so its sound is always kept out. |
| `desktop.audio.warning.includedUsesMic` | {{app}} uses your microphone and friends hear it, because you chose Include. |
| `desktop.audio.reason.builtin` / `.mic` / `.user` / `.viewer` / `.isshoni` | Voice app / Uses your microphone / You kept it out / Your browser is playing isshoni / isshoni |
| `desktop.audio.reason.launcher` / `.tree` / `.limit` / `.captureFailed` | Launcher sounds aren't shared / Started {{detail}}, which is kept out / Too many apps are playing / Can't be captured right now |
| `desktop.audio.reason.pidReuse` | Not shared, to be safe: Windows may count {{detail}} as part of it |
| `desktop.native.audio.voiceAppsHeard` | Friends hear voice apps |
| `desktop.native.audio.questionsWaiting_one` / `_other` | 1 question waiting / {{count}} questions waiting |
| `desktop.native.toast.voiceAppsHeard` | Voice apps can't be kept out right now, so friends hear everything, including your calls. Open isshoni to turn sound off. |
| `desktop.audio.sheet.voiceApps` | Friends won't hear: {{apps}}. Apps that use your microphone are kept out too; you'll be asked. |
| `desktop.audio.tip.ducking` | Windows turns other sounds down during calls, and friends hear that too. To stop it: Sound settings → More sound settings → Communications → Do nothing. |
| `desktop.audio.soundCheck.failed` | We couldn't capture the test sound, so friends would hear nothing from this PC. |

The toast texts repeat the prompt texts word for word because Go reads only `desktop.native.*` (03 §18.1). "Open sound
settings" runs `control.exe mmsys.cpl,,3` (the Communications tab).

## 15. Degraded modes (always shown, never silent)

| Situation | Engine | What friends hear | Banner | Notes |
|---|---|---|---|---|
| Process loopback works | include set / exclude-self | everything except kept-out apps and isshoni | none | normal |
| Probe failed (silent loopback, broken driver or APO, policy) | endpoint | everything on the default output, voice apps and isshoni included | `exclusion-unavailable`, `viewer-muted` | re-probed after a default-device change |
| Windows older than 19041 | endpoint | as above | `windows-too-old` | defensive only: below 19045 the app doesn't share (04 `IM_OS_SUPPORTED`); tools must not crash |
| Win10 fails the gate (A0/§18) | endpoint on Win10 by build check (`kMinIncludeSetBuild = 22000`) | as above | `exclusion-unavailable` | only if the gate fails; the plan's fallback |
| No output device | any | nothing (silent frames) | `no-output-device` | |
| Device changing (> 1 s) | any | gap, then normal | `reconnecting-sound` | |
| An app can't be captured | include set | everything else | `capture-trouble` while it plays | row in `notCaptured` |
| Paused / Share sound off | any | nothing | `sound-off` (cause `paused` / `off`) | resume keeps sound off when it was switched off (§9.6) |

The two error-level rows (`exclusion-unavailable`, `windows-too-old`), whether at start or mid-share, are also shown
outside the window during a live share: tray warning state, pill text and one toast (§14.7 "Outside the window").

## 16. Privacy, logging and diagnostics

- App names, exe names, pids, rules, prompts, decisions and levels never leave the device: they flow DLL → Go → SPA over
  Wails bindings only. M2 never sends them over the same-user relay; the reserved relay kinds `exclusions.get` and
  `exclusions.set` stay unused until the Linux agent's web UI needs them (M4), which also needs a privacy-page change.
- Logs (slog, `component=audio`, at any level including 03's debug level): engine, state, probe result, counters,
  event codes and counts only (for example "3 heard, 2 kept out, 1 prompt"). App ids, exe names, display names,
  paths, pids and rules are never logged, at any level (01 §11.12; log files are what users attach to bug reports).
  There is no environment switch for them. App-level diagnostics stay in the report (`events`, `counters`), which
  M5's "Copy diagnostics" includes with names hashed. 05's code in the DLL writes no executable name into 04's log
  ring at any level, and the DLL writes no files.
- Audiotest reports are redacted by default (§18.7).

## 17. Performance and limits

| Item | Limit or budget |
|---|---|
| Include streams | ≤ 48 (+ 1 EXCLUDE or endpoint stream) |
| Rules | ≤ 512; value ≤ 519 bytes; app id ≤ 63 bytes |
| Report | ≤ 256 KiB (128 apps, 256 sessions, 300 events) |
| Ring per stream | 500 ms (192 KiB) |
| Audio memory | ≤ 30 MiB with 48 streams |
| Audio CPU (DLL audio threads including 04's inline Opus stage, 10 running streams, Opus at 256 kbps) | ≤ 3% of one core on a 4-core desktop CPU of 2018 or newer (`audioCpuMsPerS` ≤ 30) |
| Reconcile | ≤ 5 ms; event debounce 20 ms; poll 1 s |
| New app heard | ≤ 500 ms after its session appears, or after the `im_set_exclusions` call that lets it in (typ. ≈ 100 ms: activation + 40 ms preroll). Measured by the audiotest's short-window onset (§18.6) in cases 8, 13 and 25 |
| Newly kept-out app silent | ≤ 100 ms after the rule change (the `im_set_exclusions` call; the Service's 50 ms debounce comes before it, §14.3) or detection (+ network). Measured by the short-window cut (§18.6) in case 15 |
| Fast-path exit | decided ≤ 15 ms after the session event (no debounce); cut at C − 20 ms, which needs the session notification ≤ 20 ms after the app's first sample (measured in case 5); include streams heard ≤ 150 ms later |
| First audio packet | ≤ L0 + 50 ms after `im_start` returns (silent frames; 04 §10.3 starts audio with the first frame stamped at or after `epoch_ns`); `Mixer::Start` ≤ 100 ms; probe done ≤ 600 ms |
| Drift handling | steady-state fill 40 ± 15 ms for drift within ±500 ppm; zero underruns and trims in 2 h (simulated in A4, real in soak) |
| Timestamps | mix-time error ≤ 15 ms median, ≤ 5 ms p5–p95 spread (case 20); real-share A/V offset \|median\| ≤ 20 ms (`enginetest avsync`, §19.3); chunk spacing exactly 10 ms, packet spacing exactly 20 ms within a grid, from the first chunk (L0, §10.3); a new grid starts ≥ 10 ms (chunks) / ≥ 20 ms (packets) after the previous time, so times never decrease (§10.3) |
| `im_audio_levels_get` | < 50 µs, lock-free |
| `im_set_exclusions` | applied ≤ 50 ms |
| COM-waiting calls | `im_list_audio_apps` one-shot ≤ 3 s, then `IM_ERR_TIMEOUT`; `im_audio_sound_check` ≤ `timeout_ms + 500` |

## 18. The audiotest harness (the M2 exit gate)

### 18.1 Layout

```
cmd/isshoni-audiotest/
  main.go            subcommands; the same binary is every fixture (S1's design)
  cases.go           the case table (OS-neutral descriptions + Windows fixture specs; M3/M4 add theirs)
  run.go             runner, positive controls, verdicts, exit codes (portable)
  analyze.go         Goertzel, clicks, onset, bursts (portable; S1's analyze.go as the base)
  report.go          report schema, redaction, summary line (portable)
  fixture.go         fixture verbs (portable parts)
  driver_windows.go  the engine from native.Open, used as media.Engine and media.AudioController, plus
                     native.AudioTestHooks (§13.2); explorer parenting, PlaySound, waveIn, fixture copies
  driver_other.go    "not supported on this OS yet" (M3, M4)
  results/           committed, redacted gate reports (§18.8)
  README.md          how to run, preconditions, results table
```

The harness drives the engine through `internal/desktop/audio.Service` (with `SelfInfo.Exe = ""`) and an in-memory
settings store, so the rules, prompts and `FriendsHear` it checks are exactly the app's. The `media.AudioController`
it passes to `audio.New` is the engine wrapped in a small recorder (`run.go`) that stamps every `SetExclusions` call
with the engine clock (`AudioTest.EngineClockNS`) right before forwarding it; cases 13 and 15 measure from those
stamps (§18.6). It starts an audio-only
session itself (`media.Config{Audio: &cfg}` with no layers) from `Service.AudioConfig()` plus the test-only `TapPCM`
and `TestFlags`, and pulls both Opus (`KindAudio`) and tap (`KindPCM`) packets with `Session.Next`.

It builds on every OS (`GOOS=windows CGO_ENABLED=0` from the Mac). It is not part of any installer. For a release
candidate, 06's `release.yml` uploads the workflow artifact `isshoni-exit-tools` (06 §11.1; not a release asset):
`isshoni-audiotest.exe` built from the tag next to `isshoni-media.dll` and the FFmpeg DLLs copied byte for byte from
that run's `windows-native` `release\` folder, with its `native-manifest.json`, so the report's `dll.sha256` equals
the manifest's `dll.sha256`. For other commits, `audiotest.yml`'s `build` job (06 §10.4) makes the same layout from
CI's release preset; that DLL is not byte-identical to any release and never counts for the gate (§18.10). The
engine loads FFmpeg lazily and probes and captures audio without it (04 §9.2, §13.3); the probe then reports every
encoder as `IM_ERR_DEPENDENCY`, which the audiotest ignores.

### 18.2 Subcommands

| Command | What |
|---|---|
| `selfcheck` | Loads the DLL (`native.Open`), checks `im_version` and every export (audio and `im_test_*` included), runs `im_probe`, starts and stops the watcher, runs the analyzer on synthetic signals. No sound. Exit 0/1. Runs on GitHub-hosted Windows |
| `probe` | Prints caps, the startup-probe result, ducking preference, endpoints (redacted names) |
| `apps` | One-shot "What friends hear" with the built-in rules, as text |
| `run [-case a,b] [-gate] [-long] [-keep-wav dir] [-out file] [-no-redact]` | Runs cases (§18.5). `-gate`: every required case with the gate configuration (§18.10); INFO and optional cases still run but never change the exit code |
| `record -d SEC -o file.wav [-label R1] [-exclude exe,…]` | Real-app checks: records the PCM tap, prints "friends hear … / kept out …" every second, then asks "Pass? [y/n]" and stores `{label, pass, os}` in `manual-<label>.json` |
| `merge report.json manual-*.json` | Adds the manual entries to a gate report (§18.8 step 5); refuses entries from another OS build |
| `fixture <verb> …` | Internal: what a fixture copy runs |

Exit codes: 0 all required cases PASS; 1 a FAIL; 2 no FAIL but INCONCLUSIVE; 3 a required case SKIPPED (gate
incomplete); 4 setup error (DLL missing, wrong ABI).

### 18.3 Fixtures

A fixture is a copy of `isshoni-audiotest.exe` under another name and folder in a fresh temp work folder; all copies
are made before any recording (a first-run antivirus scan must not land inside a timed window, S1). Fixtures are started
as children of the user's `explorer.exe` (`SysProcAttr.ParentProcess`, same session), so they look like apps from the
Start menu and are not descendants of the harness, which plays "isshoni". A run where they could not be reparented is
INCONCLUSIVE (S1).

| Verb | Behavior |
|---|---|
| `tone -f HZ[,HZ…] -d SEC [-amp A] [-mic]` | `PlaySoundW(SND_MEMORY \| SND_ASYNC)` of a generated WAV (S1's generator: 20 ms linear fades at both ends; a list of frequencies sums the partials, each at `-amp`), amplitude 0.125 (−18 dBFS) by default; `-mic` also opens the default microphone with `waveInOpen` and keeps recording (S1's `openMic`). Just before its first `PlaySoundW` call it prints one JSON line `{"firstRenderNs": N}` on stdout, N = `QueryPerformanceCounter` converted with 04 §12.3's `qpcNS()` formula (so it is in the engine clock); the harness reads it from the fixture's pipe |
| `spawn [-at SEC] [-tone HZ] [-mic] [-d SEC] [-orphan-after SEC] EXE ARGS…` | Starts EXE as its own child (a normal descendant) after `-at` seconds; optionally plays a tone (and logs `firstRenderNs` as `tone` does) and holds the mic itself; stays alive `-d` seconds or exits after `-orphan-after` (orphan case). Chains of children are spawns of spawns |
| `reuse-pid -target PID -parent PID [-max N]` | Case 27 only: creates suspended copies of itself (`CREATE_SUSPENDED`, `PROC_THREAD_ATTRIBUTE_PARENT_PROCESS` = `-parent`, which needs only `PROCESS_CREATE_PROCESS` on a process of the same user) and terminates each whose PID isn't `-target`, up to N (default 5 000) attempts; prints `{"matched": true, "attempts": n}` and leaves the match suspended (alive) until killed, or `{"matched": false, "attempts": n}` |
| `idle -d SEC [-mic]` | Runs silent (a voice app waiting for its helper), optionally holding the mic |
| `bursts -dll PATH -n N` | Loads the DLL (`native.Open`) and calls `AudioTest.RenderBursts` (`im_test_render_bursts`: 2 kHz, 5 ms bursts, one per second); prints the predicted onset times (device-position times from `IAudioClock`, engine clock, `im_clock` domain; §13.2 says how they are predicted; the harness maps them to `CaptureNS`) as JSON on stdout |
| `endpoint-tone -dll PATH -endpoint ID -f HZ -d SEC` | `AudioTest.RenderTone` (`im_test_render_tone`) on a chosen endpoint |

Folder layout in the work folder (names matter: they exercise the folder and launcher rules):

```
Discord\app-1.0.9999\discord.exe, voicehelper.exe, audiohelper.exe
TeamSpeak\teamspeak.exe
Teams\ms-teams.exe              WebView2Runtime\msedgewebview2.exe
Steam\steam.exe                 Steam\steamapps\common\TestGame\game.exe
Games\game.exe                  Games2\game2.exe
Google\Chrome\Application\chrome.exe
Microsoft\Edge\Application\msedge.exe
KOOK\kook.exe                   FakeVoice\fakevoice.exe
LauncherX\launcherx.exe         Browser\browser.exe
SelfWebView\msedgewebview2.exe  (started as the harness's own child)
```

`fakevoice.exe` (the plan's name) is the voice fixture for user rules; the built-in path needs a fixture named
`discord.exe`, and the browser rules need `chrome.exe` and `msedge.exe`. Real processes with the same names don't
disturb the checks (they are per fixture pid), as long as they play nothing.

Frequencies (Hz, all at −18 dBFS, ≥ 90 Hz apart): game **1000**, voice **440** (the plan's pair), second voice 571,
late game 683 (also the late child of case 25), browser tab 797, isshoni self 907 (also the late isshoni child of
case 26), browser voice 440. Case 27 alone uses the sound check's chime partials, 880 and 1320 (§11.3). The closest
pair, 907 and 1000 Hz (93 Hz), stays separable in the 50 ms windows of §18.6 (main lobe ±40 Hz).

### 18.4 Preconditions (checked, recorded in the report)

- At least one active render endpoint (else exit 3 with every case SKIP).
- **Silence pre-check** before every case: 1 s of endpoint loopback; every case frequency ≤ −70 dBFS and overall RMS
  ≤ −60 dBFS, else the case is INCONCLUSIVE ("other apps are playing"). This replaces S1's off-note tones.
- A microphone with desktop-app access for the mic cases (else those cases SKIP).
- For the fast-path cases (4, 5, 26): no non-fixture process may be kept out, so Discord, TeamSpeak, Steam (it holds the
  mic, S1) and other real voice apps must be quit, tray icons included. Otherwise those cases are INCONCLUSIVE and name
  the process. The README asks for this before every run.
- Recorded, not enforced: master volume and mute (irrelevant to loopback, S1), ducking preference (the README asks for
  "Do nothing"; the thresholds leave 27 dB of margin for a 14 dB duck), endpoint form factors, OS build and UBR,
  edition, CPU vendor, harness version, DLL version and SHA-256.

### 18.5 Cases

"Present"/"absent" use the pass rule of §18.6; every "absent" check includes its short-window rule (50 ms windows,
§18.6), so a leak of a few milliseconds anywhere in the window fails. Window [1.5 s, end] unless stated. Every case
also checks "no clicks" in its window unless marked. "Required: yes (fast path)" means required when the gate
configuration has the fast path on (§18.10), INFO otherwise.

| # | Case | Setup | Checks | Required |
|---|---|---|---|---|
| 1 | `baseline-endpoint` | endpoint engine via `IM_AUDIOF_TEST_FAIL_PROBE`; game 1000 + discord.exe 440 under explorer | both present. If this fails, every later case is INCONCLUSIVE ("the audio setup is broken, not isshoni") | yes |
| 2 | `builtin-voice` | built-in rules only; `Discord\app-1.0.9999\discord.exe` tone 440; game 1000 | 440 absent, 1000 present; Discord row `kept_out`/`builtin` | yes |
| 3 | `two-voice-apps` | discord.exe 440, `TeamSpeak\teamspeak.exe` 571, game 1000 | 440 and 571 absent, 1000 present; engine `include-set` throughout | yes |
| 4 | `fast-path` | game 1000, game2 683, harness itself plays 907 | 1000, 683 present; 907 absent; engine `exclude-self` throughout | yes (fast path) |
| 5 | `fast-path-late-voice` | as 4 (without 907); at t = 3 s discord.exe starts and plays 440 at once (its `firstRenderNs` logged, §18.3) | 440 absent over [1.5, end], whose short-window rule covers every 50 ms window from Discord's `firstRenderNs` to the end (the leak this case exists for); 1000 present; `fastpath_exit` ≤ 15 ms after Discord's `session_created`; `fastpathLateCuts` = 0; **session lag** = `session_created` time − `firstRenderNs`, recorded, FAIL above 20 ms (the cut margin of §6.4) | yes (fast path) |
| 6 | `helper-tree` | discord.exe (spawn) → voicehelper.exe (spawn) → audiohelper.exe plays 440 (a grandchild whose own name matches no rule: the ancestor walk must catch it); ms-teams.exe (spawn) → WebView2Runtime\msedgewebview2.exe plays 571 (another folder: the shared-runtime rule must catch it); game 1000 | 440, 571 absent; 1000 present | yes |
| 7 | `late-helper` | discord.exe from the start (`spawn -at 4`, i.e. t = 3 s after the 1 s settle) → audiohelper.exe playing 440 at once (`firstRenderNs` logged) | 440 absent over [1.5, end], short windows included (in particular from the helper's `firstRenderNs`: the include set must never have captured it); 1000 present; no clicks | yes |
| 8 | `late-app` | discord.exe idle (forces the include set); at t = 3 s game2 starts playing 683 for 3 s | 683 present in [3.5, 5.8]; measured onset (§18.6) ≤ 500 ms after its `session_created` (value recorded); no clicks in [1.5, end] (verifies the onset fade) | yes |
| 9 | `steam-game` | mic flag on; `Steam\steam.exe` (`spawn -mic`) → `steamapps\common\TestGame\game.exe` 1000 (a subfolder of Steam's: only the launcher stop separates them) | 1000 present; Steam row `kept_out`/`mic`; game row heard | yes (mic) |
| 10 | `isshoni-self` | harness plays 797 itself; harness's child `SelfWebView\msedgewebview2.exe` → its child `msedgewebview2.exe` plays 907; game 1000 | 797, 907 absent; 1000 present; rows `isshoni` | yes |
| 11 | `viewer-browser` | `Service.SetViewerBrowser("c_test1", "edge", "windows", true)` as 03 calls it; `Microsoft\Edge\Application\msedge.exe` (spawn) → msedge.exe (`spawn -tone 440`, the "isshoni tab") → msedge.exe playing 797 ("other tab"); game 1000 | 440 and 797 absent (whole browser out, reason `viewer`); 1000 present | yes |
| 12 | `browser-mic` | `Google\Chrome\Application\chrome.exe` (spawn) → chrome.exe (`spawn -tone 440 -mic`, Discord Web) → chrome.exe playing 797; game 1000 | both absent (reason `mic`, browser); a `browser-mic` prompt in `FriendsHear` | yes (mic) |
| 13 | `mic-user` | `KOOK\kook.exe tone -mic` 440; at t = 5 s the case answers the prompt "include" through `Service.Decide`; game 1000 | 440 absent in [1.5, 5.0] (short windows included), present in [5.6, end]; measured onset (§18.6) ≤ 500 ms after the recorder's `SetExclusions` stamp that follows the decision (value recorded); the `mic` prompt existed; no clicks | yes (mic) |
| 14 | `shared-app-mic` | `Games\game.exe tone -mic` 1000; `Service.OnShareStarted(<its pid>, true)` | 1000 present; warning `shared_app_uses_mic` | yes (mic) |
| 15 | `runtime-rule` | `FakeVoice\fakevoice.exe` 440 and game 1000; at t = 4 s `Service.SetAppDecision("exe:fakevoice.exe", "keep-out")`, which reaches `im_set_exclusions` after the Service's 50 ms debounce (§14.3); T_set = the recorder's stamp of that call (≈ 4.05 s) | 440 present in [1.5, 4.0]; absent in [T_set + 100 ms, end] (short windows included); measured cut (§18.6) − T_set ≤ 100 ms (§17; value recorded); no clicks | yes |
| 16 | `app-boundary` | `Discord\…\discord.exe` (spawn) → `Browser\browser.exe` plays 1000 | 1000 present (another folder: not Discord) | yes |
| 17 | `tree-contains-excluded` | `LauncherX\launcherx.exe` (`spawn -tone 683`) → discord.exe playing 440; game 1000 | 440 absent (required); launcherx row `not_captured`/`tree` with detail "Discord" (required); 683 level recorded (INFO: not heard by design, §6.2) | yes |
| 18 | `orphan` | discord.exe (`spawn -orphan-after 4`) → voicehelper.exe plays 440 for the whole case: the parent exits 3 s into the recording, after the watcher walked the intact link | 440 absent over [1.5, end], short windows included, in particular around and after the parent's exit (lineage cache) | yes |
| 19 | `probe-fallback` | `IM_AUDIOF_TEST_FAIL_PROBE`; game 1000, discord 440 | engine `endpoint`; banner `exclusion-unavailable`; 1000 and 440 present (degraded mode really captures and says so) | yes |
| 20 | `timestamps` | `bursts` fixture, 10 bursts | PCM-tap onset times vs the predicted device-position times of `im_test_render_bursts` (`IAudioClock`, independent of the mixer, §13.2): median abs error ≤ 15 ms, p95 − p5 ≤ 5 ms; Opus packet spacing exactly 20 000 000 ns; 50 ± 1 packets/s | yes |
| 21 | `opus` | game 1000 + discord 440, Opus at 128 k then 256 k (`Session.SetBitrate(media.LayerAudio, 256000)` at t = 4 s) | Opus packets decoded with `AudioTest.OpusDecode` (`im_test_opus_decode`): 1000 within ±1.5 dB of the PCM tap level, 440 absent; bitrate change visible in packet sizes; no `IM_PKT_REINIT` after the first packet | yes |
| 22 | `multi-endpoint` | needs ≥ 2 active render endpoints; game on the default, `endpoint-tone` 683 on another | records whether 683 is captured | INFO (SKIP with one endpoint) |
| 23 | `soak` (`-long`) | 30 min of a music-like fixture (1000 Hz + 683 Hz with slow AM) and discord idle with mic | zero underruns, trims and `mixerResyncs`; no clicks; `maxResamplePpm` ≤ 1000; 440 never present | release tier "minor" (§18.10) |
| 24 | `viewer-browser-included` | as 11, with `appRules {"exe:msedge.exe": "include"}` in the settings store before the Service starts (a saved Include, e.g. from an earlier mic prompt) | 440 and 797 absent (reason `viewer`: the viewer row beats the saved allow, §5.3); 1000 present | yes |
| 25 | `late-child-heard` | `Games\game.exe spawn -tone 1000 -at 4 Games\game.exe tone -f 683 -d 10`: the game plays 1000 from the start (heard, include target) and at t = 3 s starts a child in the same folder playing 683 (a normal descendant created after the game's stream was activated; `firstRenderNs` logged) | 683 present in [3.6, end]; measured onset ≤ 500 ms after the child's `session_created` (value recorded); the child's session `note` = `covered` and the game's app row heard (one row: same folder, same app). A FAIL triggers §6.2 step 3's fallback (later children get their own stream) | yes |
| 26 | `fast-path-late-child` | as 4 without 907 (engine `exclude-self`); at t = 3 s the harness starts its own child `SelfWebView\msedgewebview2.exe tone -f 907` (an isshoni descendant, row 1 by the instance rule, created after the EXCLUDE stream was activated; `firstRenderNs` logged) | 907 absent over [1.5, end], short windows included (from its `firstRenderNs`); 1000 and 683 present. A FAIL triggers §6.4's re-plan on every new isshoni descendant | yes (fast path) |
| 27 | `pid-reuse-tree` | `Games\game.exe` plays 1000; `LauncherX\launcherx.exe spawn -orphan-after 2 Discord\app-1.0.9999\discord.exe tone -f 880,1320` (B, a built-in voice app, plays the chime partials; its parent A exits at 2 s); the harness waits for A's exit, closes every handle it holds on A, then runs `fixture reuse-pid -target <A's pid> -parent <game's pid>`; on a match, `AudioController.SoundCheck(<game's pid>, 3 s)`, which opens an INCLUDE stream on the game's tree outside the policy (§11.3) | records `{matched, attempts, captured, gameReason}`: `captured` = Windows' tree follows the reused PID (B's chime in the game's stream); `gameReason` expected `pid_reuse` (§5.2), never `tree`. §5.2 decides the loose union from it | INFO |

Timings in the table are relative to the recording start, which begins 1 s after the case's initial fixtures run
(S1's settle).

### 18.6 Pass rule (exact)

Analysis runs on the PCM tap (pre-Opus, what the Opus encoder gets), mono `(L + R)/2`, 48 kHz.
- **Level** of frequency f in window [a, b]: Goertzel with a Hann window, amplitude in dBFS of the sine peak (a
  full-scale sine is 0 dB), as in S1's `toneLevelDB`.
- **Present**: level ≥ −45 dBFS.
- **Absent**: level ≤ −50 dBFS **and** level ≤ P_min − 40 dB, where P_min is the lowest level among the case's
  present checks. This is the plan's "1 kHz present, 440 Hz at least 40 dB lower", made robust to ducking.
- **Short-window rule** (part of every absent check): the level of f is also computed over **50 ms** Hann windows
  hopped every **10 ms** across the check window (only windows that lie fully inside it), and the **maximum** must be
  ≤ −50 dBFS. Why: one Hann window over the whole check window barely moves for a short leak. A 30 ms leak at
  −18 dBFS, 17 % into an 8.5 s window (Hann weight ≈ 0.28), reads about −72 dBFS and would pass both the −50 limit
  and P_min − 40; S1's `analyze.go` has the same whole-window design. In a 50 ms window the same leak reads above
  −25 dBFS, and even a 1 ms leak at −18 dBFS reads about −46 dBFS. A 50 ms Hann window's main lobe is ±40 Hz, so the
  closest pair of the tone plan (907 and 1000 Hz, 93 Hz apart) stays separable: a −18 dBFS 1000 Hz tone with the
  mixer's 5 ms onset or 10 ms fade leaks at most about −64 dBFS into the 907 Hz bin (asserted in the analyzer tests,
  §19.1), and the fixtures' WAVs fade over 20 ms (§18.3). The check records `maxShortDb` and the time of that window.
- **Measured cut and onset.** For a check that follows an event (a rule change, a decision, a new session), the
  analyzer reports times from the same 50 ms windows, each window placed at its centre: the **cut** is the centre of
  the last window after the event whose level is above −50 dBFS, and the **onset** the centre of the first window
  after the event at or above −45 dBFS. With −18 dBFS tones the centre reads a cut about 17 ms late (conservative) and an onset about 15 ms early,
  small against §17's 100 ms and 500 ms. The case's event time comes from the engine (`session_created` events), the
  recorder's `SetExclusions` stamps (§18.1) or the fixtures' `firstRenderNs` (§18.3), all in the engine clock and
  mapped to tap times with the offset of §13.2. Cases assert §17's budgets on these values and record them.
- **Event-relative windows.** A window bound written as an event (`firstRenderNs`, `T_set + 100 ms`) is resolved per
  run from that event's time; if the event never happened, the case is INCONCLUSIVE ("fixture didn't start").
- **Clicks**: any sample on either channel with |x[i] − 2x[i−1] + x[i−2]| > 0.03 inside the click window, events
  merged within 10 ms (S1's `clickEvents`). Five −18 dBFS tones up to 1 kHz stay below 0.013.
- **Positive controls** (S1): every fixture the case plays must appear in the report's `sessions` as `active` at least
  once and must not exit early; a fixture with an "absent" check must be `kept_out` (or the expected `not_captured`) in
  every report sample inside that check's window where it is active; a fixture with a "present" check must not be
  `kept_out` in any sample inside that check's window (a case whose verdict changes, 13 and 15, checks each window
  against its own expectation); engine and banner expectations must hold in every sample of the window. The report is
  sampled every 250 ms; these samples check verdicts, not audio, which is why the short-window rule exists.
- **Verdict**:
  - **PASS**: every check, the click check, the controls and the expectations hold.
  - **INCONCLUSIVE**: the premise couldn't be proven: silence pre-check failed; a fixture didn't start, didn't
    appear as active, or exited early; fixtures weren't reparented; the harness lost audio (a gap > 30 ms in PCM-tap
    capture times); another app was heard (a non-fixture row heard with peak > −60 dBFS during the window).
  - **SKIP**: a precondition is missing (mic, second endpoint).
  - **FAIL**: anything else.
  - A FAIL is downgraded to INCONCLUSIVE only by the reasons above, never by "it was close".

### 18.7 Report

`audiotest-<win10|win11>-<build>-<YYYYMMDD-HHMM>.json`, schema v1:

```json
{
  "v": 1,
  "harness": "v0.2.0-14-g1a2b3c4",
  "dll": {"version": "0.2.0", "sha256": "…"},
  "os": {"product": "win11", "build": 26200, "ubr": 6899, "edition": "Professional"},
  "machine": {"cpu": "AuthenticAMD", "renderEndpoints": [{"formFactor": "Headphones", "bus": "usb"}],
              "mic": true, "masterVolume": 0.42, "muted": false, "duckingPreference": 3},
  "gate": true,
  "gateConfig": {"fastPath": true},
  "summary": {"pass": 24, "fail": 0, "inconclusive": 0, "skip": 0, "info": 2, "exit": 0},
  "cases": [
    {"name": "two-voice-apps", "required": true, "result": "PASS",
     "checks": [{"freq": 1000, "from": 1.5, "to": 0, "expect": "present", "levelDb": -18.1, "pass": true},
                {"freq": 440, "from": 1.5, "to": 0, "expect": "absent", "levelDb": -141.7, "maxShortDb": -128.4,
                 "maxShortAtS": 6.31, "pass": true}],
     "measured": {},
     "clicksAtS": [], "problems": [], "engine": ["include-set"], "counters": {"underruns": 0, "trims": 0},
     "events": [{"tMs": 1210, "code": "stream_running", "app": "fixture:game"}]},
    {"name": "runtime-rule", "required": true, "result": "PASS",
     "checks": [{"freq": 440, "from": 1.5, "to": 4.0, "expect": "present", "levelDb": -18.2, "pass": true},
                {"freq": 440, "from": 4.153, "to": 0, "expect": "absent", "levelDb": -139.0, "maxShortDb": -61.7,
                 "maxShortAtS": 4.153, "pass": true}],
     "measured": {"cutMs": 58, "budgetMs": 100},
     "clicksAtS": [], "problems": [], "engine": ["include-set"], "counters": {"underruns": 0, "trims": 0},
     "events": []}
  ],
  "manual": [{"label": "R1", "pass": true}]
}
```

`checks[].maxShortDb`/`maxShortAtS` are the short-window rule's maximum and where it was (absent checks only);
`measured` holds the event-relative values of §18.6, each with its budget: `cutMs` (case 15, budget 100), `onsetMs`
(cases 8, 13, 25, budget 500), `sessionLagMs` (case 5, budget 20) and `fastpathExitMs` (case 5, budget 15); `to: 0`
means the end of the recording.
`gateConfig` is §18.10's gate configuration.

Redaction (default; `-no-redact` for local debugging only): no user, host or device friendly names; endpoint names
become `{formFactor, bus}`; paths become relative to the work folder; app ids of fixtures become `fixture:<role>`, and
any non-fixture app appears as `other` with no name. WAVs are never written unless `-keep-wav` is given, and are never
committed. The harness prints one summary line:

```
isshoni-audiotest v0.2.0-14-g1a2b3c4 · DLL 0.2.0 sha256:3f9a1c0b7d2e · Windows 11 Pro 26200.6899 · 24/24 PASS · 0 FAIL · 0 INCONCLUSIVE · 0 SKIP · gate=PASS
```

### 18.8 Where it runs and how results are recorded

| Where | What runs | Gating |
|---|---|---|
| Mac (`task test`, `task native:test`) | analyzer, verdict logic and report tests on synthetic signals; `audiopolicy` and `audiomix` GoogleTest suites in `host-debug`/`host-tsan`; `GOOS=windows` build | yes (CI) |
| CI ubuntu-24.04 and macos-15 | the same | required check (06's `ci-ok`) |
| CI windows-2025 (GitHub-hosted) | MSVC build of the DLL (`win-x64-ci`); all native unit tests incl. Windows-only ones that need no endpoint (process table against spawned children, report writer); `isshoni-audiotest selfcheck` | required check |
| CI windows, experimental job | tries `Start-Service audiosrv` and `isshoni-audiotest run -gate`; expected result on hosted runners: exit 3 (no render endpoint) | never required |
| Owner's PC, Win11 (≥ 24H2; now 25H2) | `isshoni-audiotest run -gate` + manual R1–R5 | **M2 gate**, every release |
| Owner's PC, Win10 22H2 (dual boot) | the same | **M2 gate**, minor releases |
| Optional self-hosted runner on the owner's PC | the same, triggered by `workflow_dispatch` | see below |

Exact manual run (≈ 10 minutes per OS):
1. Download the `isshoni-exit-tools` artifact of the release candidate's `release.yml` run (06 §11.1; for a commit
   that isn't a release, `audiotest.yml`'s `build` artifact, which is diagnostic only), unzip to
   `C:\isshoni-audiotest`, and check that `isshoni-media.dll`'s SHA-256 equals `native-manifest.json`'s. If
   SmartScreen blocks it: Properties → Unblock, or "More info → Run anyway" (unsigned 0.x).
2. Close or pause every app that plays sound. Connect a microphone and check Settings → Privacy & security →
   Microphone → "Let desktop apps access your microphone" is on. Recommended: Sound settings → More sound settings →
   Communications → "Do nothing".
3. In PowerShell (normal, not admin): `cd C:\isshoni-audiotest; .\isshoni-audiotest.exe run -gate -out .`
4. Real apps (R1–R5, §18.9) with `.\isshoni-audiotest.exe record -label R1 -d 30 -o r1.wav` etc.
5. Copy the report JSON and the `manual-*.json` files to the Mac. The developer merges the manual entries into the
   report with `isshoni-audiotest merge report.json manual-*.json`, commits it as
   `cmd/isshoni-audiotest/results/YYYY-MM-DD-<win10|win11>-<build>.json` and adds a row (date, OS build, harness and
   DLL version, summary, notes) to the results table in `cmd/isshoni-audiotest/README.md`.

Self-hosted runner (optional, owner's decision, §25 question 2): an ephemeral runner (`config.cmd --ephemeral`, 06's
labels `self-hosted, windows, x64, isshoni-audio, win11|win10`, 06 §10.4–§10.5) started by hand with `run.cmd` in the
owner's interactive desktop session (process loopback and explorer-parented fixtures need the user session; a service in
session 0 can't do this), online only while the owner runs a test. The workflow `audiotest.yml` (06) triggers only on
`workflow_dispatch`, runs only `if: github.repository == 'MoonWX/isshoni'`, uses an environment that requires the
owner's approval (06's `audiotest`), downloads the `isshoni-exit-tools` artifact of the release run named by its
`release_run_id` input (suite `release`; 06 §10.4), runs `run -gate` (suite `full`: `run -gate -long`), and uploads the
redacted report as an artifact. No `pull_request` trigger ever targets these labels, and "Require approval for all
outside collaborators" stays on for fork workflows.

### 18.9 Manual real-app checks

| Id | Steps | Pass | OS |
|---|---|---|---|
| R1 | Discord desktop: join a call with a friend, or Settings → Voice & Video → Mic Test (Discord plays your voice back). At the same time play a YouTube video in Edge. Run `record -label R1 -d 30` | `r1.wav`: YouTube audible, no Discord voice; Discord row `kept_out`/`builtin` | Win11, Win10 |
| R2 | Discord Web in Chrome, in a voice channel with the mic allowed; YouTube in Edge. Open the isshoni app's share sheet | the "Your browser is using the mic" prompt appears; Chrome kept out; Edge heard in `record` | one of the two per gate; both at minor releases |
| R3 | Start a game from the Steam library; `record -label R3 -d 30`. When the owner has an anti-cheat-protected title, use it once (it exercises §5.7's no-handle path) | the game is heard; Steam's own sounds follow the mic setting; the protected game, if used, heard (with or without the `no_handle` note) | Win11, Win10 |
| R4 | The real app: share a window with sound while watching a friend's share (with sound) in the isshoni app; the friend listens | the friend doesn't hear their own audio; the isshoni row is `kept_out` | Win11, Win10 |
| R5 | During a share with a game playing and Discord in a call: unplug or plug a headset, or change the default output in Sound settings | the game is heard again within 2 s; Discord stays absent; `reconnecting-sound` disappears within 3 s | Win11, Win10 |
| R6 | TeamSpeak 3/5/6 and Mumble, if installed, as R1 | voices absent | minor releases |
| R7 | For each app the owner can run: new Teams ("Meet now" or a test call), Zoom's test meeting, a Slack huddle, Vesktop in a Discord call; YouTube in Edge at the same time; `record -label R7-<app> -d 30` | the app's voice absent, YouTube audible; its row `kept_out`/`builtin` (new Teams: every process that plays, WebView2 or module host included). A failure fixes the exe list or the folder and shared-runtime rules (§5.4), never removes the app; apps not run yet are "unverified" in the README's results table | once on Win11 before `v0.2.0`; again when that app's row changes |

### 18.10 The exit gate and release tiers

**M2 exit (audio part)**: on the release-candidate commit,
1. `isshoni-audiotest run -gate` exits **0** on the owner's Win11 (≥ 24H2) and on Win10 22H2, with the harness and DLL
   of that commit (the report's `dll.sha256` equals the release candidate's DLL);
2. R1, R3, R4 and R5 pass on both, R2 on at least one; R7 passes once on Win11 for every app of it the owner can run
   (the others are marked "unverified" in the README's results table, §5.4);
3. `enginetest avsync` (§19.3) gives |median A/V offset| ≤ 20 ms on both (WGC on Win11, DD on Win10), recorded with
   the reports;
4. both reports, with their manual entries, are committed under `cmd/isshoni-audiotest/results/`.

**Gate configuration** (what `run -gate` uses, recorded as the report's `gateConfig`): built-in rules and settings
defaults of the commit under test, with `Exclusions.FastPath` set to the release's shipped default of `audio.fastPath`
(§14.1). With that default `true`, cases 4, 5 and 26 are required; with it `false` (the kill switch of §6.4 used for
that release), they still run, with the fast path forced on, as INFO and never change the exit code. Case 4's
"`exclude-self` throughout" is thus never demanded of a release that doesn't ship the fast path.

If the Win10 include set fails (A0 or the gate), the plan's gate applies: Win10 builds below 22000 use endpoint
loopback with the banner, and the Win10 gate becomes "cases 1 and 19 pass and the banner shows", once the owner has been
told.

**Release tiers** (plan "Verification"; 06 runs the checklist): every release: `run -gate` and R1 on Win11, report
attached to the GitHub Release as `audiotest-win11.json`. Minor releases: the full gate on Win11 and Win10, R1–R6
(R7 for an app whose row changed), `soak`, both reports attached.

## 19. Test plan

### 19.1 Unit (Mac, CI ubuntu-24.04, macos-15, windows-2025)

Native tests use GoogleTest through 04's vcpkg `tests` feature and run with `task native:test` in 04's presets
`host-debug` and `host-tsan` (Mac, CI ubuntu and macos) and `win-x64-ci` (CI windows).

- **`audiopolicy`** (≈ 60 table cases on fake snapshots): every row of §5.3; the §5.10 examples; the audiotest cases
  as pure-logic mirrors; S1's `app-boundary` folder rule (subfolder = same app; sibling folder = other app; unknown
  folder = boundary); launcher stops; shared runtimes; instance trees with strict and loose links; PID reuse (a child
  older than its "parent" is not linked strictly, but is linked loosely for I1); a target blocked only through a
  loose link gets `pid_reuse` (detail = the kept-out app, `pidReuseBlocks` + 1), one blocked through a strict link
  `tree`, and a strict-and-loose block `tree`; orphans keep lineage; mic stickiness across capture-session gaps and its
  end on process exit; the shared app suppresses row 6 only; a saved allow beats mic (row 6) but not a viewer rule
  (row 4 beats row 5), and without the viewer rules (the per-run override) the allowed browser is heard; the WebView2
  data-folder path rule (equal; `<dir>\EBWebView` below it; `/` and trailing separators normalized; case; `<dir>2`
  not matching); planner steps 2–4 (dirty trees, ancestor coverage with session note `covered`, ordering, the 48 cap)
  and step 3's fallback mode (a child created after its ancestor's activation gets its own target); the no-handle
  path of §5.7 (OpenProcess fails → the target is still planned and committed, note `no_handle`; a snapshot without
  the pid, or with another creation time, removes it on the next reconcile; I3 holds by snapshot); fast-path
  condition including "isshoni launched by Discord"; invariants I1–I5 checked after randomized event sequences
  (property test, 10 000 seeds in CI, 500 in `-short`).
- **`audiomix`**: ring wraparound and QPC side ring; preroll/low/high water; fade lengths; tail ramp; onset fade (ramp
  after exactly 50 ms of silence, not after 49 ms; **a new stream with 30 ms of zeros, then a −6 dBFS tone, is
  ramped** because its counter starts saturated, and the click detector finds nothing; the same after a
  `Preroll` → `Running` re-entry); a stream leaving the silent bypass gets `speex_resampler_reset_mem` (a stream fed
  a tone, then 200 ms of zeros, then the tone again shows no pre-silence samples at the onset); soft clip continuity;
  crossfade; PI controller in simulation (drift −500…+500 ppm, 10 ms bursts with ±3 ms jitter, 2 simulated hours:
  fill 40 ± 15 ms after 60 s, zero underruns/trims, ratio std dev ≤ 30 ppm); the controller never emits ppm = 0 (a
  drift of exactly 0 ppm for 10 simulated minutes: every update has |ppm| ≥ 1), and a ppm sweep through −1/+1 around
  zero produces no click; **no allocation on the mixer thread after `Mixer::Start`** (Linux `host-debug`: a
  `malloc`/`realloc`/`free` interposer counts calls from the mixer thread through 10 simulated minutes with ppm
  changes, onsets and bypass exits; must be 0; other hosts skip it with a log line); silence drop/pad; chunk
  timestamps exactly on the 10 ms grid; **sound from t = 0** (a channel playing before the first chunk): chunk
  spacing exactly 10 000 000 ns through the first 2 s, stamps use L0 = 50 ms, and no `OnGap` after the first chunk; a
  recording `PcmSink` sees `OnGap` exactly at the first chunk, a resync and a silence re-anchor, and never on a device
  change, an engine switch or sound off/on; re-anchor only in silence, it replaces L0 with the measured value, and a
  re-measured L' more than 20 ms above L never gives a chunk time ≤ the previous one (the clamp, §10.3); chunk time =
  `qpc(head)` within 1 ms for a simulated ring with known QPC positions (no device-latency term); the silence flags:
  pause, sound off, resume → the bus stays at 0 until sound on; S1's click detector run on every fade path finds no
  click. The Opus settings of §10.1 (read back
  with `OPUS_GET_*`, encode/decode round trip within ±1 dB) are checked by 04's `opus_stage_test`.
- **Report writer** (C++): produces the golden JSON files of §13.1 byte for byte from fixed inputs.
- **Go binding logic** (`internal/media/native`, 01 §7.7's `caller` fake, Mac and CI ubuntu): `im_exclusions` and
  `im_audio_levels` layouts; the status → error table of §13.1 (`IM_ERR_BAD_STATE` from an audio call matches
  `media.ErrAudioNotRunning`; `IM_ERR_INVALID_ARG` → `CodeInvalid` with `im_last_error` as `Detail`; `IM_ERR_TIMEOUT` →
  `CodeTimeout`, called exactly once; `IM_ERR_ABI` → `CodeDependency` with Reason `abi`); the snprintf retry through
  01 §7.3's helper; mode mapping and Go-side rejection (`AudioConfig.Mode`: `""`, exclude-apps and `AudioTestTone` →
  modes 1, 1, 100; `AudioOff` and everything → `CodeInvalid` with no `im_start` call; `SetAudioMode`: off and
  exclude-apps only, `AudioTestTone` and everything → `CodeInvalid` with no call); `SetBitrate(media.LayerAudio, …)` →
  `im_set_audio_bitrate`; `IM_PACKET_PCM` becomes `KindPCM` and appears only with `TapPCM`.
- **Go service** (`go test -race`): `ParseAudioReport` on the goldens (and fuzzed, 60 s); `audiorules` (lower case,
  unique, no browser is a launcher, `msedgewebview2.exe` not a browser); the rules builder (§14.3) for every settings
  combination; `AudioConfig` flushes the debounce and returns `Mode: exclude-apps`; `SetSharedPID` (pick, release,
  expiry); prompts (once per app per run, kept out while unanswered, decisions persisted; after `DismissPrompt` the app
  is still kept out, no re-prompt this run, `appRules` unchanged, and an unknown id gives `ErrUnknownPrompt`); every
  error of §14.2's table; `AppRules` lists decisions of apps that aren't running; banner and tip rules (§14.7)
  table-driven, including `sound-off` causes; `FriendsHear` sorting; `FriendsHear.alert` (`voiceAppsHeard` only while
  live with an error-level banner, `questions` = pending prompts) and `audio.alert` (once per code per share, again only
  after 60 s, never outside a live share; driven by the fake's `degrade-live`); `SetViewerBrowser` with active and
  inactive sessions, two connIDs of one family (one stops: still kept out; both stop: rules gone), `safari`/`other` →
  banner, and the 30 s re-check (synctest); viewer rules are built for an exe with a saved `include` decision too (both
  its row-5 allow and its row-4 viewer rule are in `Exclusions`); "Include" on a `viewer` row (row-4 rules gone,
  `appRules` unchanged, mic keep-out still applies, gone in a new Service); polling rates by levels watch count
  (synctest: 100 ms with a count above 0, 250 ms at 0, none when idle; the count never goes below 0);
  `RunSoundCheckHelper` returns 2 off Windows; the ducking stub hides the tip; a log-capture test with a report full of
  names finds none in the log at debug level; the fake's scenarios, its `AudioController` before any `Start`, and its
  call log; the harness analyzer and verdict logic on synthetic recordings (present/absent thresholds, P_min rule,
  clicks, gaps, INCONCLUSIVE causes), including the short-window rule of §18.6: **a 20 ms burst of 440 Hz at −18 dBFS
  inside a 10 s window FAILs absent** (the whole-window level alone would pass); a −18 dBFS 1000 Hz tone with 5 ms and
  10 ms linear fade-ins and fade-outs, and with its onsets, never trips a short-window absent check at 907 Hz; a tone
  with S1's 20 ms WAV fades doesn't either; measured cut and onset of a synthetic step lie within 20 ms of the true time
  (cut late, onset early, as §18.6 says); event- relative windows resolve from `firstRenderNs` and recorder stamps; case
  5's session-lag rule (21 ms → FAIL); the gate configuration (shipped `fastPath` false → cases 4, 5, 26 INFO, exit code
  unaffected by their result).

### 19.2 Integration

- **Windows CI (hosted, no endpoint)**: process table against real spawned children (parents, creation times, image
  folders, `--user-data-dir` parsing of fake `msedgewebview2.exe` command lines: quoted and unquoted, forward
  slashes, and the `--user-data-dir=<dir>\EBWebView` form WebView2 really uses); watcher start/stop with no endpoint
  (state `no-device`, no crash, no leak: handle count back to baseline after 100 start/stop cycles); `IM_INIT_NO_AUDIO`
  returns of §4.3; `selfcheck`.
- **Go, any OS**: `internal/desktop/audio` against the fake through a full share lifecycle with goleak: watch → pick
  (`SetSharedPID`) → start (`OnShareStarted`) → prompt → decide → sound off → pause → resume (stays silent) → sound on
  → stop (`OnShareStopped`). Asserts the fake's call log (`audio_mode off` at sound off, no audio call at pause or
  resume from the Service, `audio_mode exclude-apps` at sound on) and the `sound-off` banner's cause at each step.

### 19.3 End to end

- **Owner's PC**: the audiotest gate (§18.10) on Win11 and Win10.
- **Owner's PC**: the smoke of the real app (03/06 run it), whose audio part is R4.
- **Owner's PC: `isshoni-enginetest avsync`** (04's tool, with the `win-x64-ci` DLL for its luma tap; the audio side
  is this doc's). It checks what case 20 and 04's tone-based checks can't: real WGC/DD frame times against real
  loopback times. Steps, exact:
  1. The tool starts two helper processes as children of `explorer.exe`: a **flash window** (a borderless
     1280×720 flip-model swap chain, `DXGI_SWAP_EFFECT_FLIP_DISCARD`, black, white for 100 ms once a second) that
     logs, for each white frame, the QPC time it became visible (`IDXGISwapChain::GetFrameStatistics` →
     `SyncQPCTime` of the present that showed it), `P_k`; and a **click process** that calls
     `im_test_render_bursts(NULL, 2000, 0.5, 5, 1000, 20, …)` (5 ms clicks once a second) and gets each click's
     predicted device-position time `A_k` (§13.2).
  2. It captures for 25 s with real capture and real audio: `-source pick` (the flash window; WGC) on Win11,
     `-source mon:<id>` (DD) on Win10, `-audio mix` (exclude-apps, with `TapPCM`), at 1080p60.
  3. Video: `V_k` = `capture_ts_ns` of the first frame after each flash whose mean luma in the luma tap exceeds 50 %.
     Audio: `U_k` = the PCM-tap onset time of each click (first 1 ms with |x| > 0.05, at chunk time + offset/48 kHz).
     Each flash is paired with the click nearest to it (both run on 1 s grids; they need not coincide, because each
     pair's true offset `A_k − P_k` is known).
  4. **Pass**: over ≥ 15 pairs, |median of `(U_k − V_k) − (A_k − P_k)`| ≤ **20 ms**, i.e. the stamped A/V offset
     matches the true one within 20 ms, leaving the rest of the plan's 45 ms to the network and the viewer. Recorded
     with the M2 exit record (§18.10 item 3), on Win11 (WGC) and Win10 (DD).

### 19.4 Manual

R1–R7 (§18.9); the first-run sound check on both OSes; a 2-hour watch-together session (Movie preset) with
`counters` checked afterwards (zero underruns/trims, `maxResamplePpm` recorded).

## 20. Room for later milestones

- **M3 (macOS)**: `audiorules` already has `MacOS` fields; `AudioController`, the report schema, `FriendsHear`, the
  Go service and the harness runner are OS-neutral; the macOS engine (`internal/media/native/engine_darwin.go`, 04
  §16) implements `AudioController` and may compile `native/common/audiopolicy` through cgo (C++ sources are
  supported) for its bundle-ID and PID matching, or keep its own; `RunSoundCheckHelper` gets a darwin file;
  `driver_darwin.go` adds the macOS fixtures (`com.isshoni.test.fakevoice.app`).
- **M4 (Linux)**: the Pulse-protocol mixer in Go implements `AudioController`; `driver_linux.go`; the relay kinds
  `exclusions.get`/`exclusions.set` carry `FriendsHear` to the web page (with the privacy-page change).
- **M5**: per-app volume, app icons, loss-adaptive Opus (`PACKET_LOSS_PERC`), "Copy diagnostics" with hashed names,
  a 10 ms frame option (`frame_ms` already accepts it).
- **Later**: a native "separate window" launcher for Discord Web; capturing one output device only; an "All sound"
  escape hatch if the owner wants one (`IM_AUDIO_MODE_EVERYTHING` and `media.AudioEverything` are reserved, §25
  question 3).

## 21. Interfaces other docs rely on

1. **C ABI audio block (§4)**: the §4.1 declarations in 04's header and conventions; §4.2's use of
   `im_audio_config` (mode values 0–2, `IM_AUDIOF_*`), `im_start_config.exclusions` (a pointer, NULL = keep the set),
   `im_caps.audio_flags` (§11.1) and `im_packet` (`IM_PACKET_AUDIO`/`IM_PACKET_PCM`, `layer = IM_NO_LAYER`, capture
   times per §10.3, `IM_PKT_REINIT` per §10.4); the semantics of §4.3 (3 s COM wait → `IM_ERR_TIMEOUT`, the sound
   check's `timeout_ms + 500`, `IM_ERR_BAD_STATE`, `IM_ERR_LIMIT` for a second audio session, `IM_INIT_NO_AUDIO`, the
   three silence flags). Audio never fails `im_next_packet`.
2. **The C++ boundary (§10.5)**: `AudioEngine` and `Mixer` as 04 §10.2 declares them, the `OnChunk`/`OnGap` rules,
   `Mixer::SetPaused` as one of three ORed silence flags, no event sink.
3. **Requirements on 04's Opus stage and rings (§10.1, §10.2)**: the encoder settings (FEC off, DTX off, 20 ms,
   lookahead subtracted), `IM_PKT_REINIT` after `OnGap`, continuous silence frames, the PCM tap.
4. **Report JSON v1 (§12.1)** and the level struct (§12.2), with golden files in `internal/media/testdata/audio/`.
5. **Go `internal/media`** (`audio.go`): `AudioMode` and its constants, including `AudioTestTone` (`"test-tone"`, the
   one carrier of 04's test tone: `AudioConfig.Mode` at start only, §13.1), `Rule`, `RuleKind`, `RuleSource`,
   `Exclusions`, `AudioConfig` (one type: `BitrateBPS`, `Exclusions`, `Mode`, `TapPCM`, `TestFlags`), `AudioReport`
   with `AudioStatus = AudioReport`, `AudioApp`, `ParseAudioReport`, `AudioLevels`, `SoundCheckResult`,
   `AudioController`, `ErrAudioNotRunning`; the status → error table of §13.1 (codes `CodeAudioNotRunning` and
   `CodeTimeout` declared in 01's `errors.go`).
6. **Go `internal/media/native`**: `media.AudioController` on the engine `native.Open` returns (`audio.go`,
   `audio_windows.go`); `KindPCM` packets only with `TapPCM`; `AudioTestHooks` and `AudioTest` (Windows only).
7. **Go `internal/media/audiorules`**: `VoiceApps`, `Launchers`, `SharedRuntimes`, `Browsers` (with families), per OS.
8. **Go `internal/desktop/audio`**: `Service` and its methods (§14.2: `AudioConfig`, `Exclusions`, `SetSharedPID`,
   `OnShareStarted`, `OnShareStopped`, `SetPaused` (no engine call), `SetShareSound`, `WatchLevels`, `AppRules`,
   `SetViewerBrowser(connID, family, os, playing)`, …) and its errors (`ErrUnknownPrompt`, `ErrLockedApp`,
   `ErrInvalidAppID`, `ErrInvalidDecision`, `ErrUnknownTip`); `Settings` (§14.1); the `FriendsHear` (with `alert`),
   `Alert`, `AppRow` (reason `pid-reuse` added), `AppRule`, `Prompt`, `Banner`, `Tip`, `Levels` JSON shapes (§14.5);
   event names `audio.report`, `audio.levels`, `audio.prompt`, `audio.alert` (03 emits them as `isshoni:audio.*`;
   `audio.alert` is the toast trigger of §14.4); the banner and tip codes and the "outside the window" signals
   (§14.7); the `desktop.audio.*`, `desktop.native.audio.*` and `desktop.native.toast.*` English texts (§14.8).
9. **Audio packets for 01**: Opus, 20 ms, 48 kHz stereo; `CaptureNS` on an exact 20 ms grid that never goes back,
   from the first packet (L0, §10.3); a capture time means the Windows audio engine's mix time minus the encoder
   lookahead, the same pipeline depth as video's composition times, without output-device latency; `Reinit` marks a
   new grid (informational: Go keeps its mapping and needs no action).
10. **Audiotest CLI, exit codes, report schema, summary line and results path (§18)** for 06's release checklist and
    CI, and what the harness needs next to it (`isshoni-media.dll` and the FFmpeg DLLs of the build under test, with
    `native-manifest.json` for the hash check: 06's `isshoni-exit-tools`, §18.1); the gate configuration
    (§18.10: cases 4, 5, 26 required only when the release ships `audio.fastPath` on); R7 and the "unverified" column
    of the README's results table.
11. **The audio half of `enginetest avsync`** (§19.3): the click process through `im_test_render_bursts`, the
    PCM-tap onset rule and the pass rule (|median| ≤ 20 ms), for 04's tool and the M2 exit record.
12. **`--sound-check-helper` and `audio.RunSoundCheckHelper() int`** (§11.3): 03's `main` dispatches the flag to it
    before Wails starts and exits with its return value.
13. **The fake's `degrade-live` scenario** (§13.3) for 03's e2e of the tray, pill and toast under a mid-share
    degradation.

## 22. Depends on (assumed interfaces of the parallel docs)

- **01-client-core.md**
  - `media.Config.Audio *AudioConfig` (nil = no audio), filled via `core.Options.AudioConfig` from
    `Service.AudioConfig()`; the core overwrites `BitrateBPS` with `ShareParams.AudioBitrate` and forwards the rest
    unread. 01 declares `LayerAudio = "audio"`, `KindPCM`, `ErrInvalid` and the codes `CodeAudioNotRunning` and
    `CodeTimeout`; it declares no `AudioConfig` of its own. The engines `native.Open` and `fake.NewEngine` return also
    implement `media.AudioController` (type assertion). 05's audio errors are `*media.Error` values (§13.1:
    `ErrAudioNotRunning = &Error{Code: CodeAudioNotRunning}`, invalid input is `CodeInvalid`); there is no plain
    `errors.New` sentinel and no `ErrInvalidAudioConfig`.
  - `media.Packet` has `Kind` audio, `CaptureNS`, `Data` (one Opus packet) and `Reinit`. Go keeps its RTP mapping
    (one t0 for all tracks, 01 §8.2): `rtp_ts = base + rtpTicks(CaptureNS − t0)` for audio, without smoothing or
    fix-ups, so a new grid is just a timestamp jump; no marker bit on audio; SRs from capture times (M1 publisher
    behavior).
  - 01 never lowers audio in M2: the Opus target is `ShareParams.AudioBitrate` (128 000; Movie 256 000), set at start
    and on every `share.update` reply (01 §9.2, §9.4), through `Session.SetBitrate(media.LayerAudio, bps)`.
  - Pause: the core owns the engine side of pause (01 D14, §11.8): it drops audio at the pump, calls
    `Session.SetPaused` (04's `im_set_paused`) and `AudioController.SetAudioMuted`, and undoes both on resume.
    "Share sound" off is `SetAudioMode(AudioOff)` from 05's Service, never `SetAudioMuted`, so the core's unmute at
    resume can't turn a switched-off share back on (§4.3).
  - Relay: the core receives `playback.state` (01 §11.11) and emits `EventPlaybackState{From, Playing, Browser, OS}`;
    03 calls `SetViewerBrowser(From, Browser, OS, Playing)`. The core sends no `playback.stop` in M2 (03 §12.2 does).
  - Native binding (01 §7): `native.Open` resolves 05's procs on its handle (`LoadLibraryEx` with explicit search
    flags, 01 D4); the `caller` interface of §7.7 for tests; the buffer helper of §7.3 (snprintf rule, up to 4 MiB);
    the control-call mutex with §13.2's two exceptions; the timeout status looped only for `im_next_packet`, returned
    as `CodeTimeout` elsewhere.
  - The core compares `AudioStatus.Seq` (or doesn't read the report at all) to decide whether "What friends hear"
    changed: the value also carries counters and events, which change every second.
  - fake: `fake.EngineConfig.AudioScenario` (§13.3), and the two fields 05 adds to `fake.Call` (`Mode`, `Muted`) for
    Ops `audio_mode` and `audio_muted`.
  - `mediatest.Conformance`'s `TestTone` option (01 §6.4) requests the tone as
    `media.Config.Audio = &media.AudioConfig{Mode: media.AudioTestTone}` (§13.1); `TestFlags` carries no tone bit.
- **03-desktop-app.md**
  - `bridge.Audio` wraps the SPA-facing Service methods one to one (same names) with 03's error JSON (§6.4, mapping
    of §14.2) and forwards Service events as `isshoni:audio.report`, `isshoni:audio.levels`, `isshoni:audio.prompt`,
    `isshoni:audio.alert`.
  - **Outside the window** (§14.7): during a live share the tray uses `tray-warn` with the tooltip and first menu line
    `desktop.native.audio.voiceAppsHeard` while `FriendsHear.alert.voiceAppsHeard`, and a menu item
    `desktop.native.audio.questionsWaiting` (opens the window on the prompt card) while `alert.questions > 0`; the
    pill shows the same short text and an amber marker; `audio.alert` becomes a toast (`desktop.native.toast.
    voiceAppsHeard`, a §15 row, setting: always); 03's e2e drives it with the fake's `degrade-live`.
    03's App and share controller, not the SPA, call `Exclusions`, `AudioConfig`, `OnShareStarted`/`OnShareStopped`,
    `SetPaused`, `SetShareSound`, `SetSharedPID` and `SetViewerBrowser`. It renders `FriendsHear` (share sheet,
    first-share sheet with `voiceApps`, prompts as in-window cards and as toasts while hidden) and `AppRules()` in
    Settings → Audio.
  - Share controller order (03 §10.1): `SetSharedPID(Source.PID)` after `Share.Pick`, `SetSharedPID(0)` on Release or
    pick expiry; `OnShareStarted(uint32(req.Source.PID), req.Audio)` right before `core.StartShare`;
    `OnShareStopped()` when `Share.Done()` closes; `SetPaused` on every pause and resume, including screen lock and
    unlock and remote pauses; `SetShareSound` from the **Share sound** checkbox; `core.Options.AudioConfig =
    svc.AudioConfig`.
  - Provides `SettingsStore` (key `audio`) and `SelfInfo{PID: os.Getpid(), WebViewDataDir, Exe: os.Executable()}`;
    `main` dispatches `--sound-check-helper` to `audio.RunSoundCheckHelper()` before Wails starts.
  - Calls `SetViewerBrowser(connID, family, os, playing)` on `EventPlaybackState`, and with `playing = false` when
    that connection leaves `user.connections`.
  - **Relaunch under Explorer**: when `isshoni.exe` starts with a parent other than `explorer.exe` or a launcher of
    §5.4 (e.g. a browser that opened an `isshoni://` link), it relaunches itself as a child of the user's
    `explorer.exe` (`PROC_THREAD_ATTRIBUTE_PARENT_PROCESS`, same session) and exits. Otherwise isshoni sits in the
    browser's OS tree, and Firefox, whose audio plays from its parent process, could never be included (§6.2 step 2).
  - Mutes the in-app viewer while the report's engine is `endpoint` during a live share (banner `viewer-muted`).
  - Explains in the share sheet that muting the PC doesn't mute the stream (S1: loopback ignores master volume).
  - Logs no audio app names at any level (§16); 03 §17's "image names only at debug" becomes "never".
  - Carries §14.8's keys in `web/src/i18n/lazy/desktop.en.json`.
- **04-windows-engine.md**
  - Owns `isshoni_media.h` and the DLL (`isshoni-media.dll`); carries §4.1 in its conventions, including
    `IM_ERR_TIMEOUT` (−15), `IM_PACKET_PCM`, the `im_start_config.exclusions` pointer, the `flags` argument of
    `im_list_audio_apps` (0, reserved), `int32_t timeout_ms` in `im_audio_sound_check`, and §4.1's layouts in 04 §5.4
    and `im_abi_layout`. Its call-reference note for `im_audio_set_muted` reads "the pause mute 01's core sets", not
    "the user's Sound off" (§4.3).
  - Build: one CMake root `native/`; 04's `native/common/CMakeLists.txt` (README WA03) adds `audio/`, `audiopolicy/`
    (`im_audiopolicy`) and `audiomix/` (`im_audiomix`) behind `if(EXISTS …)` guards, and each library globs its own
    `tests/*_test.cpp`, so 05's slices only add files in their folders; `speexdsp` (BSD-3-Clause) in
    `native/vcpkg.json` beside `opus`; GoogleTest through the `tests` feature; presets `host-debug`, `host-tsan`,
    `win-x64-ci`; `task native:test`; CI ubuntu-24.04, macos-15, windows-2025.
  - The C++ boundary of 04 §10.2 as §10.5 implements it; the Opus stage adopts §10.1 and §10.2; the rings of 04 §6.2.
  - The start-at-`epoch_ns` drop (04 §10.3) assumes only L ≥ 0 (chunks stamped L ≈ 50 ms in the past at start, L0),
    not L ≥ 50 ms (§10.3).
  - The binding's `media.Config` → `imStartConfig` conversion uses §13.1's single mode mapping for `AudioConfig.Mode`
    (`media.AudioTestTone` → `IM_AUDIO_MODE_TEST_TONE`); `internal/media/native` declares no `AudioTestTone` of its
    own, and `enginetest -audio tone` uses `media.AudioTestTone`.
  - `isshoni-enginetest avsync` (§19.3): the subcommand, the flash window and the luma-tap reading; its audio half and
    pass rule are §19.3's.
  - `im_set_paused` calls `Mixer::SetPaused`; `im_set_audio_bitrate` is live; `im_start` accepts an audio-only config
    (04 §10.3) and allows one session with audio per process (04 §6.4).
  - The pick result exposes `pid` (`im_source_info.pid`, 04 §7.6), which Go passes as `Exclusions.SharedPID`.
  - FFmpeg is loaded lazily (04 §9.2, §13.3): the engine loads, probes and captures audio without it.
  - Slice E13 (audio bridge) wires 05's `AudioEngine`/`Mixer` into sessions; its fake-`AudioEngine` CI tests come
    first, and its PC acceptance closes together with A6.
  - Applies `WDA_EXCLUDEFROMCAPTURE` (video) independently; nothing here depends on it.
- **06-windows-release.md**
  - Uploads the workflow artifact `isshoni-exit-tools` from every `release.yml` run (not in the installer, not a
    release asset): `isshoni-audiotest.exe` built from the tag, and `isshoni-media.dll`, the FFmpeg DLLs and
    `native-manifest.json` copied byte for byte from that run's `windows-native` `release\` folder (§18.1);
    `audiotest.yml` takes a `release_run_id` and runs that artifact; lists libopus and speexdsp in the native
    `THIRD_PARTY_NOTICES` list; adds the CI jobs of §18.8 (04's native tests on ubuntu, macos, windows; `selfcheck`
    on windows; the experimental job; `audiotest.yml` if the owner opts into a runner); runs the release tiers of
    §18.10 and attaches the reports.
- **02-device-auth.md**: nothing.

## 23. Implementation slices

Each slice is independently testable. "Mac" means `task test` on the developer's Mac; "CI" the required GitHub checks;
"PC" the owner's Windows PC.

| # | Slice | Scope | Acceptance | Verified | Needs | Days |
|---|---|---|---|---|---|---|
| A0 | Win10 gate and onset-fade re-run (owner action) | Run the existing S1 bundle: `s1.exe selftest` on Win10 22H2; `s1.exe selftest -only late-start,late-start-endpoint` on Win11 | Results recorded in `spikes/s1-win-audio/README.md`. Pass: include-set scenarios pass on Win10 and late-start has no click on Win11. A Win10 failure triggers the plan's gate (endpoint-only on Win10, owner told) before A6 merges | PC | — | 0.5 |
| A1 | `audiorules` | Go data of §5.4 + tests | lower case, unique, families complete; `go test` green | Mac, CI | — | 0.5 |
| A2 | Go audio types and fake | `internal/media/audio.go` (§13.1), goldens, `fake/audio_ctl.go` (`EngineConfig.AudioScenario`, the scenarios of §13.3, the call-log fields) | goldens parse; fake scenarios scripted under synctest; the fake's `AudioController` works before `Start`; `GOOS=windows/darwin/linux` and the gomobile package builds pass | Mac, CI | 01's C01 (media package) | 1.5 |
| A3 | `audiopolicy` core | portable C++20 of §5–6 as CMake target `im_audiopolicy` in `native/common/audiopolicy/` (added by 04's `native/common/CMakeLists.txt`): snapshot, rules, classifier, lineage, mic state, planner, invariants, the WebView2 path rule | ≈ 60 GoogleTest table cases and the 10 000-seed property test pass under `task native:test` in `host-debug` and `host-tsan` (macOS clang, ubuntu-24.04) and `win-x64-ci` (windows-2025, MSVC) | Mac, CI | 04's E1 | 3 |
| A4 | `audiomix` core | target `im_audiomix` in `native/common/audiomix/`: rings with QPC side ring, PI resampler (speexdsp from `native/vcpkg.json`, `init_frac` creation, \|ppm\| ≥ 1, `reset_mem` on leaving the bypass), silence drop/pad, channel states, fades, onset fade (saturated counter for new channels), soft clip, levels, silence flags, chunk timestamps (mix time, L0 = 50 ms, re-anchor only in silence) and the `OnGap` rules (§10.3, §10.4); report writer producing the goldens | §19.1 `audiomix` and report-writer tests pass, including the 30 ms-silence onset, ppm never 0, no mixer-thread allocation (Linux `host-debug`) and sound from t = 0; the drift simulation meets §17 | Mac, CI | A2 (goldens), 04's E1 | 3 |
| A5 | Windows watcher (no capture) | process table, endpoints, session notifications, meters, mic detection, device notifications, `AudioEngine::Init/Shutdown/Watch/Unwatch/SetRules/ListAppsJson/LevelsGet/ProbeFlags`, `im_caps.audio_flags`; Go `internal/media/native/audio.go` and `audio_windows.go` | caller-fake tests on the Mac and CI ubuntu; hosted CI windows: process-table tests, 100 start/stop cycles without leaks, `no-device` state, `IM_INIT_NO_AUDIO` returns. PC (right after README group 6 merges, with A7's harness core, README WA62a; a hardware failure becomes a `WF` fix slice that lists `native/windows/src/audio/`, never a change in a harness PR): `isshoni-audiotest apps` lists playing apps with correct verdicts for Discord, a browser and a Steam game | Mac, CI, PC | A3, 04's E2 (DLL skeleton and loader) | 2.5 |
| A6 | Windows capture and mixer | activators, held handles (and the no-handle path, §5.7), commit check, loopback streams, capture thread, include set (`pid_reuse` verdicts, `covered` notes), fast path with retroactive cut, endpoint engine, probe, mode/mute/pause flags, sound check, health checks, `Mixer` of §10.5 calling 04's `PcmSink` (packets reach `im_next_packet` through 04's bridge), test exports (`im_test_render_bursts` predicting from `IAudioClock`, §13.2), `AudioTestHooks` | hosted CI builds and `selfcheck` passes. PC: cases 1–4, 10, 15, 19–21 pass (right after README group 6 merges, with A7's harness core, README WA62a; a hardware failure becomes a `WF` fix slice that lists `native/windows/src/audio/`, never a change in a harness PR), and 04's E13 acceptance: `enginetest capture -audio mix` gives a playable `audio.ogg` | CI windows, PC | A0, A4, A5, 04's E13 | 4 |
| A7 | Audiotest harness | §18 complete: analyzer (S1's as base, plus the 50 ms short-window rule and measured cut/onset of §18.6), runner, the `SetExclusions` recorder, fixtures (`firstRenderNs`, `reuse-pid`), all cases (24–27 included), gate configuration, report, redaction, summary, `record`, `merge`, README with the "unverified" column for R7 | Mac: analyzer and verdict tests, including the 20 ms burst that must FAIL absent and the faded 1000 Hz tone that must not trip 907 Hz. PC: all required cases run; any failure is a real engine bug, fixed in A6's code by a `WF` fix slice that lists `native/windows/src/audio/`, never in a harness PR (README §3 item 13; a case 25 or 26 failure switches on its fallback, §6.2, §6.4) | Mac, CI, PC | A6 (runs), A2, A8 (the harness drives the service) | 3 |
| A8 | Desktop policy service | `internal/desktop/audio`: settings, rules builder, prompts, viewer browser (connIDs, override), banners/tips (`ducking_windows.go`/`ducking_other.go`), `FriendsHear` (with `alert`), `AppRules`, errors, `AudioConfig`, `SetSharedPID`, `SetShareSound`, `SetPaused`, `WatchLevels` polling, events (`audio.alert` included), `RunSoundCheckHelper` (`helper_windows.go`/`helper_other.go`) | §19.1 Go tests and the lifecycle integration test with the fake pass with goleak, on the Mac and CI ubuntu, macos, windows | Mac, CI | A1, A2 | 2.5 |
| A9 | App integration (audio part) | with 03: `bridge.Audio` wrapping and events, the `--sound-check-helper` dispatch, viewer mute in endpoint mode, `SetViewerBrowser` from `EventPlaybackState`, `SetSharedPID` after the pick, `OnShare*`/`SetPaused`/`SetShareSound` from the share controller, `core.Options.AudioConfig` | Mac: share sheet driven by every fake scenario (`ISSHONI_FAKE_AUDIO`). PC: share with Discord in a call shows Discord kept out; Share sound off, pause, resume stays silent; R4 passes | Mac, PC | A8, 03's shell, 04's picker | 1.5 |
| A10 | Gate | §18.10 on Win11 and Win10, R1–R5, R7 (Win11), `soak`, `enginetest avsync` (§19.3, 04's tool); results committed | `run -gate` exit 0 on both in the gate configuration; manual checks pass; avsync \|median\| ≤ 20 ms on both; reports in `cmd/isshoni-audiotest/results/` | PC | A7, A9, 04's `avsync`, release candidate (06) | 1 |

A1–A4 touch disjoint paths and can be built in parallel; A8 can run beside A5–A6. Critical path: A3 → A5 → A6 → A7
→ A10 (A0 must be done before A6 merges). About 23 working days of the milestone's ≈ 50.

## 24. Risks

| Risk | Mitigation |
|---|---|
| The include set fails on Win10 22H2 (never run there) | A0 first; the plan's gate (endpoint-only on Win10 with the banner); the probe also catches silent loopback per machine |
| The onset fade doesn't remove the stream-attach click on real hardware | Cases `late-app`, `late-helper` with click checks; the fade length (5 ms) and silence threshold (50 ms) are constants to tune in A6 |
| The fast path leaks the first milliseconds of a voice app that starts mid-share | Retroactive cut at C − 20 ms; case 5 with the 50 ms short-window rule from the fixture's own first-render time (a few milliseconds of leak fail), the measured session-notification lag (FAIL above the 20 ms cut margin) and `fastpathLateCuts = 0`; case 26 for late isshoni children; `audio.fastPath` kill switch, with the gate following the shipped default (§18.10). Deferring the fast path altogether is the owner's call (§25 question 4) |
| Windows fixes an INCLUDE stream's tree when the stream is activated, so a later child of a heard app is silently lost | Required case 25 (`late-child-heard`); if it fails, §6.2 step 3's fallback gives later children their own stream |
| A dead parent's PID is reused inside a heard app's tree (Discord's `Update.exe` exits at once), blocking the main content through a loose link | Reason `pid_reuse` with its own text and counter instead of a false "Started Discord"; INFO case 27 measures whether Windows follows reused PIDs, and §5.2 drops the loose union if it doesn't |
| Unknown voice apps play before they open the mic | Inherent to mic detection (same in both engines); built-in list for the common apps; "What friends hear" shows the app with its meter; one tap keeps it out |
| Process loopback may not cover apps on non-default endpoints | INFO case 22 records the behavior on the owner's PC; if missing, a per-endpoint follow-up comes later, and the docs say so |
| Anti-cheat-protected games deny process queries or the include target's handle | Unknown folders are app boundaries and unknown creation times accept links (fail toward keeping more out); a target whose `SYNCHRONIZE` handle can't be opened is captured anyway, checked by snapshot on every reconcile, note `no_handle` (§5.7); R3 with a protected title if the owner has one |
| A built-in voice app plays from a process the exe list misses (new Teams' call media in WebView2 or a module host) | R7 once on Win11 before `v0.2.0` per app the owner can run; failures fix the exe list or folder rules; unchecked apps marked "unverified"; mic detection stays the backstop |
| Too many streams load `audiodg.exe` | 48-stream cap; silent streams are cheap; CPU measured in `counters` and in the soak |
| Windows changes process-loopback semantics in a future build | Every release runs the gate on current Win11; the probe and the silent-stream check degrade visibly instead of silently |
| Self-hosted runner on a public repo runs untrusted code | Not required; if used: ephemeral, interactive, dispatch-only, approval environment, outside-collaborator approval on |

## 25. Open questions for the owner

1. **Probe failure while a voice app runs.** The plan says: probe fails → endpoint loopback plus a banner. Then friends
   hear the call (and themselves) until the sharer reacts. Alternative: in that one case (probe failed *and* a
   built-in voice app or a mic user is running), start the share with sound off and ask "Voice apps can't be kept out
   on this PC. [Share all sound anyway] [Keep sound off]". Recommendation: ask. The engine supports both (it is a Go
   policy choice in `internal/desktop/audio`); this doc implements the plan's behavior until you decide.
2. **Self-hosted runner.** Register an ephemeral GitHub Actions runner on your Windows PC (started by hand in your
   desktop session, `workflow_dispatch` only, approval environment; §18.8), or run the audiotest zip by hand for the
   M2 gate and each release (≈ 10 minutes per OS)? Recommendation: by hand in M2; the runner only saves the copy step,
   and a runner on a public repository is a standing risk.
3. **An escape hatch for built-in voice apps.** M2 has no way to hear a built-in voice app (the plan: "A short,
   tested built-in list is always kept out"), and no "All sound" mode: the per-share Share sound switch is the only
   "off". Someone who wants friends to hear, say, a Zoom webinar can't. `IM_AUDIO_MODE_EVERYTHING` and
   `media.AudioEverything` stay reserved, so an "All sound" choice (or a per-app override of a built-in) could come
   later with a settings key and UI. Recommendation: none in M2; decide after the first users ask.
4. **Defer the fast path?** The plan says "use a single `EXCLUDE` stream when the excluded set is one tree", and this
   doc builds it (§6.4). It is the one fail-open mode, it is new (S1 didn't test the retroactive cut), and it runs only
   when no voice app (and not Steam) is running, which isn't the main use case. Alternative: ship M2 with the
   fail-closed include set plus the endpoint fallback only, keep `IM_EXCLF_FAST_PATH` reserved and ignored, drop
   cases 4, 5, 26 from the gate, and revisit in M5. Recommendation: keep it, as the plan says, now that case 5 measures
   millisecond leaks and the notification lag and the gate follows the shipped default (§18.10); if case 5 or 26 fails
   on real hardware, ship with `audio.fastPath` false and decide then. This doc implements the plan until you decide.
