# M3 design 03: macOS audio, "all system audio except voice apps"

Status: draft for M3, 2026-11-06. `docs/PLAN.md` is the source of truth above this doc. Evidence: spike S2
(`spikes/s2-mac-audio/`: the README results, `tap_engine.m`, `selftest_darwin.go`, `testapps.m`, `build.sh`,
`dev-signing.sh` and `results/2026-09-29-m5-macos27.json`), and M2 as built (`docs/m2/README.md` §3 and §4,
`docs/m2/04-windows-engine.md`, `docs/m2/05-windows-audio.md`, `native/include/isshoni_media.h`,
`native/common/{audio,audiomix,audiopolicy}`, `internal/media`, `internal/desktop/audio`, `cmd/isshoni-audiotest`).

Written in parallel with `01-desktop-macos.md`, `02-macos-engine.md`, `04-macos-release.md` and
`05-exit-and-verification.md`. Where this doc needs something from another one, §20 states the assumed interface
exactly. "05" below always means `docs/m2/05-windows-audio.md` and "04" `docs/m2/04-windows-engine.md`; the M3 docs are
named by file name.

Tags: **M3** = build now. **later (Mx)** = leave room now, build in milestone x.

This doc is the audio half of the M3 exit criterion "the same checks on 14.4 through 27": voice apps excluded,
proved by `isshoni-audiotest run -gate` on macOS 14.4, 15, 26 and 27 (§16.10). The latency half and the run book are
`05-exit-and-verification.md`'s.

## 1. Scope

In this doc (all M3 unless tagged):
- The audio half of the macOS engine, up to 04's `im::audio::PcmSink`: the Core Audio process tap
  (`stereoGlobalTapButExcludeProcesses`) on a private aggregate device, the IOProc and its ring, sample-rate handling,
  tap generations with live updates and the 20 ms aligned crossfade, rebuild triggers, the zero-run and permission
  watchdogs, mic detection, the process snapshot with responsible PIDs, the "What friends hear" report, levels, the
  sound check, and the shipped test exports on macOS (`im_test_render_tone`, `im_test_render_bursts`).
- A new portable C++20 library `native/common/tappolicy` (target `im_tappolicy`): classification of audio processes by
  bundle ID, enclosing bundles, responsible process and PID, and the tap plan (which objects and bundle IDs the tap
  excludes, and whether a change needs a crossfade). Tested on the Mac and on CI ubuntu, macOS and Windows.
- Two additive changes to M2's shared native code: an aligned takeover in `native/common/audiomix` (§8.4) and the
  macOS values of the report writer (§12).
- The audio part of the C ABI v1.1 additions (§4): macOS semantics of the existing audio declarations and the few new
  constants. `02-macos-engine.md` owns `native/include/isshoni_media.h` and merges §4.1.
- Go: macOS data in `internal/media/audiorules`, the per-OS rules builder and ids in `internal/desktop/audio`, the
  additive report and sound-check fields in `internal/media/audio.go`, the fake's macOS scenarios.
- The SPA's small changes for the macOS report (engine and audio backend `process-tap`, two banners,
  `FriendsHear.selfAudible` that 01's `mutesViewer` reads, no per-app meters).
- `cmd/isshoni-audiotest` on macOS: the darwin driver, the fixture apps (beside 05's harness apps, §16.3), the macOS
  case table, the pass rule, the report schema, where it runs and how results are recorded. This is the exit gate. The
  harness bundles themselves (`isshoni Dev Harness.app`, `isshoni Exit Harness.app`), their launcher and
  `harness.sh` are `05-exit-and-verification.md`'s.

Not in this doc: video capture and encode, the engine's build system, `im_start`/`im_stop`/`im_next_packet`, the
session and its rings, the Opus stage itself, the cgo binding (`02-macos-engine.md`); the window, wizard, tray and
the `OpenOSSettings` targets (`01-desktop-macos.md`); signing, packaging, the update path and TCC persistence across
updates (`04-macos-release.md`); A/V latency and the exit run book (`05-exit-and-verification.md`).

Not in M3: per-app level meters and per-app volume (M5, §11.3); "Copy diagnostics" (M5); a separate-window launcher
for Discord Web (Later); capturing one output device only (Later); an "All sound" mode (reserved, as in M2).

## 2. Decisions at a glance

| Topic | Decision | Why |
|---|---|---|
| Capture model | One global process tap `initStereoGlobalTapButExcludeProcesses:` on a private, tap-only aggregate device; excluded apps are listed by Core Audio process object, and on macOS 26+ also by bundle ID with `processRestoreEnabled` | Plan; S2: 12/12 PASS on macOS 27, excluded apps at −149 to −172 dBFS (digital silence). No per-app streams, so no include-set planner and no `tree`/`pid_reuse`/`limit` verdicts |
| Aggregate device | Tap-only (`kAudioAggregateDeviceTapListKey`, `TapAutoStart`, drift compensation on); the tap runs on the default output device's clock. Fallback when a tap-only device doesn't start within 500 ms: the output device as main sub-device with its input streams switched off for the IOProc (`kAudioDevicePropertyIOProcStreamUsage`) | S2 finding 6: tap-only works and never opens a headset's mic (an AirPods mic would drop the headset to the hands-free profile). The variant with the output device also worked in S2; 14.x is unmeasured, hence the fallback |
| Policy location | Classification in native code on the engine's queue, on the process-list listener; Go owns the rule data, settings and prompts (as M2) | S2 finding 2: a new process object appears before its first buffer and the listener updates the tap ≈ 4 ms later. A Go round trip (polled, no callbacks in the ABI) would lose that race |
| Policy code | New portable `native/common/tappolicy`; reuses `audiopolicy::ParseExclusions` for rule validation and the implicit self rule | macOS identity is bundle ID + enclosing bundles + responsible process, not exe names and folder trees; the logic is the risk and must run in `task native:test` |
| Rule values | The same `im_rule` kinds; on macOS a value is a bundle ID (dot-boundary, case-insensitive prefix) or `exe:<name>` for unbundled executables | No ABI kind is added; S2's matching, generalized |
| Self | Implicit instance rule on the engine's pid plus an implicit app rule on the running main bundle's own ID (exact match, §4.2); Go adds no self app rule on darwin; WebKit's GPU and WebContent processes matched by responsible PID (`responsibility_get_pid_responsible_for_pid` via `dlsym`); never by `com.apple.WebKit.GPU` | Plan; S2 `self-webview` −157 dBFS. Matching WebKit by bundle ID would also mute Safari; the self rule always equals the bundle that actually runs (`io.isshoni.desktop`, `.dev`, `.e2e`, a harness) |
| SPI missing, or isshoni not its own responsible process | Report `selfAudible: true`; 01's `mutesViewer` mutes the in-app viewer's audio while live, with the `viewer-muted` banner | Plan's fallback ("mute in-app viewer audio while sharing with audio"); a terminal-started dev run or a failed LaunchServices relaunch (01 §6.1) attributes WebKit's processes to the terminal |
| Mixer and grid | Reuse M2's `audiomix::MixEngine` (one channel per tap generation) on a real-time mixer thread: exact 10 ms grid on the engine clock, PI-controlled adaptive resampling against device-clock drift, silence flags, levels | The Opus stage needs chunks exactly 10 ms apart (04's `PcmSink`); the device clock drifts against `mach_absolute_time`. Solved and tested in M2 |
| Sample rate | The engine always converts itself: a fixed-ratio speexdsp stage on the IO thread when the tap runs at another rate than 48 kHz, on every macOS version | The plan's "own resampler on 14.x", generalized to one code path; speexdsp is already linked (BSD-3), real-time safe after init. S2's AudioConverter path was only unit-tested |
| Crossfade | An app that changes sides **while it plays** gets a second tap generation and a 20 ms equal-gain crossfade, aligned by capture time; new or silent apps get the instant live update | S2 finding 3: a live update cuts within one sample (click); two taps align to 0 ± 1 samples |
| Rebuild triggers | Default output device, its nominal sample rate, the tap format, `kAudioHardwarePropertyServiceRestarted`, a failed live update, and runs of zero buffers while a heard process has output running (backoff) | Plan; risk "taps silently output zeros" |
| Mic detection | `kAudioProcessPropertyIsRunningInput` **and** a real input device in `kAudioProcessPropertyDevices` (input scope; aggregate and unresolvable devices don't count); sticky for the life of the app's root process | S2 finding 4 (tap readers report `IsRunningInput`); M2's push-to-talk stickiness (05 §5.6) instead of S2's 10 s |
| Permission | TCC preflight (SPI, never prompts) in every report, plus a 30 s watchdog that turns only a real denial into `denied` (an unanswered prompt stays `pending`); denial never fails a call: the share runs with silent audio and the `audio-permission` banner | Plan; "audio never ends a share" (05 §4.3); 01's wizard and Settings → Permissions read the state without building a tap |
| "What friends hear" | Rows per app from the process objects; one mix meter; per-row "output active" dot instead of per-app meters | The tap is one mixed stream; per-app meters would need a tap per app (M5) |
| Opus | M2's `OpusStage` (04 §10.3) unchanged, libopus from vcpkg built for macOS; timestamps on the engine clock (`mach_absolute_time` ns) | Same packets, same Go side; vcpkg already builds opus and speexdsp on the Mac (`host-*` presets) |
| No capture while only watching | The tap exists only during a live share with audio and during the sound check | macOS shows its recording indicator for system audio capture; the share sheet must not light it |
| Harness | `isshoni-audiotest` runs as a tool inside 05's `isshoni Dev Harness.app` / `isshoni Exit Harness.app` through `harness.sh`; fixture apps in a sibling `fixtures/` folder (never inside the harness bundle), signed with the harness's identity, started through LaunchServices; the same analyzer and pass rule as M2 | S2's proven fixture design (S2 built fixtures in a sibling `dist/fixtures/`); the plan's "harness .app signed with the same identity and granted once"; one harness bundle per identity, so dev and release grants never overwrite each other |
| Gate location | The owner's MacBook Pro (macOS 27) and macOS 14.4, 15 and 26 in 05's VM lab; GitHub's runners (02's/05's `macos-native` job, `macos-26`) build, unit-test and run `selfcheck` only | Hosted runners have no TCC grant for audio capture and no answerable prompt |

## 3. From spike S2 to M3

| S2 result or shortcut | M3 |
|---|---|
| Global exclude tap, 12/12 PASS on macOS 27 (M5, output at 48 kHz) | Same primitive (§6); every S2 scenario becomes an audiotest case (§16.5) |
| Draft ABI `im_audio_*` with PCM out via `im_audio_read` | Audio part of ABI v1 as M2 built it (§4): `im_audio_watch`, `im_set_exclusions`, `im_list_audio_apps`, levels, sound check, Opus packets through 02's session |
| Reader-side crossfade with its own alignment code | `MixEngine` channels with an aligned takeover (§8.4); the alignment rule is S2's (capture time of the reader's head) |
| `AudioConverter` when the tap rate ≠ 48 kHz, unit-tested only | speexdsp fixed-ratio stage on the IO thread (§7.4); real-device check on 44.1 kHz (case 33) |
| Classification snapshot 20–30 ms with ≈ 45 objects (sysctl walks) | Per-process cache keyed by (pid, start time) (§9.6); a list change classifies only new objects, ≤ 5 ms |
| Mic stickiness 10 s | Life of the app's root process (§10.3) |
| During a pending crossfade the old tap gets no list changes | Refined: the old tap still gets additions of objects that are not playing (a voice app starting mid-fade must not leak through the old tap for ≈ 140 ms); audible changes are queued (§8.5) |
| 2 s crossfade timeout, then a hard cut | 500 ms (the mixer thread always reads, unlike S2's pull-driven reader) (§8.5) |
| No zero-run rebuild, no permission watchdog, no "skip unknown objects" | §7.6, §7.7, §9.2 |
| Tap-only aggregate default, output-device variant as an A/B flag | Tap-only default, output-device variant as the start fallback (§7.2) |
| Fixtures: copies of `s2` in signed bundles in a sibling `dist/fixtures/`, started with `open -n -g -j` | Same design for `isshoni-audiotest` (§16.3): a sibling `fixtures/` folder beside the harness app, test-only bundle IDs (never real apps' IDs, §16.3) |
| Not covered: 14.4, 15, 26; real Discord; default-device change; 44.1 kHz on a device | AM0 runs S2's `run-selftest.sh -mic` in 05 V1's `-granted` VM clones of 14.4/15/26 (§19); M-R1 real Discord, M-R5 device change (§16.9); case 33 (§16.5) |
| Permission persistence across rebuilds (builds 1→9, macOS 27) | `04-macos-release.md` owns the N → N+1 check on every version; the harness reuses the same identity (§16.1) |

## 4. C ABI: the audio part on macOS (normative)

`02-macos-engine.md` owns `native/include/isshoni_media.h`. Every audio declaration of v1.0 (05 §4.1, as built) keeps
its signature and, unless this section says otherwise, its semantics from 05 §4.3. This section lists what changes on
macOS and the additions, which land in the minor bump `IM_ABI_MINOR 1` that `02-macos-engine.md` makes. Windows ignores
the new `IM_AUDIOF_TEST_*` bits (its `im_start` passes audio flags through unchecked) and never sets the new
`audio_flags` bits or states.

### 4.1 Additions (text for the header, 04 §5.1 conventions)

```c
/* audio_flags (probe) on macOS (docs/m3/03-macos-audio.md §13.1); Windows never sets these bits */
#define IM_AUDIO_PROCESS_TAP      0x10u  /* CATapDescription and AudioHardwareCreateProcessTap present (14.2+) */
#define IM_AUDIO_TAP_BUNDLE_IDS   0x20u  /* CATapDescription.bundleIDs/processRestoreEnabled usable (26.0+ and SDK 26) */
#define IM_AUDIO_RESPONSIBLE_PID  0x40u  /* responsibility_get_pid_responsible_for_pid found (dlsym) */
#define IM_AUDIO_TCC_PREFLIGHT    0x80u  /* TCCAccessPreflight found (dlsym); says nothing about the grant itself,
                                            which the report's tap.permission carries (§12.1, §13.2) */
/* IM_AUDIO_SESSION_EVENTS (0x4): the process-object-list listener registered; IM_AUDIO_MIC_DETECTION (0x8):
   kAudioProcessPropertyDevices readable and at least one input device exists. 0x1 and 0x2 stay Windows-only.
   Go decodes 0x10-0x80 in media.AudioCapsFromFlags (§14.2): Backend "process-tap" when 0x10 is set. */

/* IM_AUDIO_STATE_* additions (im_session_info.audio_state; the report's "state" strings in §12.1) */
#define IM_AUDIO_STATE_PERMISSION 5u   /* "permission-pending" or "permission-denied": no audio is captured */
#define IM_AUDIO_STATE_FAILED     6u   /* "tap-failed": the tap can't be built or started; retried with backoff */

/* im_audio_config.flags additions: audiotest only, shipped, ignored on Windows */
#define IM_AUDIOF_TEST_NO_EXCLUSIONS 0x40000000u /* the tap excludes nothing, not even isshoni (case 1, pre-check) */
#define IM_AUDIOF_TEST_NO_BUNDLE_IDS 0x20000000u /* 26+: don't set CATapDescription.bundleIDs (the 14-15 path) */
```

The `im_rule` comment block gains the macOS meaning (the text of §4.2's table, one line per kind). Layouts don't
change (`im_abi_layout` golden unchanged).

### 4.2 Rule kinds on macOS

`audiopolicy::ParseExclusions` validates exactly as on Windows (05 §4.3: kinds, sources, UTF-8, lengths, app id
charset, pid syntax) and folds values to lower case. Then `tappolicy` interprets them:

| Kind | macOS value | Matches (§9.4) | Default app id |
|---|---|---|---|
| `IM_RULE_APP_EXCLUDE`, `IM_RULE_APP_ALLOW` | a bundle ID (`com.hnc.discord`), or `exe:<name>` for an unbundled executable | bundle ID: dot-boundary prefix of the process's own bundle ID, any enclosing bundle's ID, or (unless its responsible process is a launcher) the responsible process's IDs; `exe:`: the executable file name of the process or of its non-launcher responsible process | `bundle:<value>` (cut: §9.5); `exe:` values are their own id |
| `IM_RULE_INSTANCE` | decimal pid | the pid, its strict descendants (parent started before child), and every process whose responsible pid it is | `isshoni` for source SELF, else `exe:<value>` as on Windows |
| `IM_RULE_STOP` | a launcher's bundle ID or `exe:<shell>` | not a match: a launcher responsible for P never gives P its identity or a rule match (§9.3) | — |
| `IM_RULE_SHARED_RUNTIME` | — (Go sends none on macOS; accepted and ignored) | — | — |
| `IM_RULE_BROWSER` | a browser's bundle ID | marks the app row `browser: true` (browser mic prompt) | — |
| `IM_RULE_WEBVIEW_DATA_DIR` | — (Go sends none on macOS; accepted and ignored) | — | — |

**Implicit rules** (whatever Go passes, like 05 §4.3): `IM_RULE_INSTANCE(getpid())` with source SELF and app id
`isshoni`, and `IM_RULE_APP_EXCLUDE([NSBundle mainBundle].bundleIdentifier)` with source SELF and app id `isshoni` when
the process has a bundle ID (01's `io.isshoni.desktop`, `io.isshoni.desktop.dev`, `io.isshoni.desktop.e2e`; 05's
`io.isshoni.dev.harness`, `io.isshoni.exitharness`). The watcher starts with these two alone. **The implicit app rule
matches exactly**, not at a dot boundary: a process whose `bundle_id` or one of whose `enclosing[*]` IDs **equals** the
main bundle's ID (so `io.isshoni.desktop` running beside a dev build doesn't keep `io.isshoni.desktop.dev`'s audio out,
nor the reverse). Go sends no SELF app rule on darwin (§14.1), so this implicit rule is always the running bundle's
own ID. A run without a bundle (an unbundled dev binary) has only the instance rule.

### 4.3 Semantics that differ from Windows

- **Threads.** All Core Audio and classification work runs on one serial dispatch queue (`io.isshoni.audio`), as in
  S2. An ABI call that needs it posts a block and waits at most **3 s** on a semaphore, then returns `IM_ERR_TIMEOUT`
  (−15), like 05's COM waits. `im_set_exclusions` copies the rules and returns; the queue applies them within 50 ms
  (typically < 5 ms). HAL listener blocks are registered on that queue. The HAL IO thread and the mixer thread never
  take a lock, allocate or call Objective-C (§7.3, §8.2).
- **Watching needs no permission.** `im_audio_watch`, `im_list_audio_apps` and `im_set_exclusions` read process
  objects only; no tap exists until a share with audio starts or the sound check runs. `im_audio_levels_get` while
  watching returns `mix_peak_db = -120`, `count = 0`.
- **Errors.** As on Windows, audio never ends a share and never makes `im_next_packet` fail. `im_start` fails for audio
  reasons in 05 §4.3's two cases only. Tap and aggregate failures, denied permission, device loss and service restarts
  are report states and events (§12.1); `IM_ERR_PERMISSION_DENIED` and `IM_ERR_AUDIO_DEVICE_CHANGED` are never
  returned by an audio call (§13.4 maps every failure).
- **Modes.** `IM_AUDIO_MODE_EXCLUDE_APPS` and `IM_AUDIO_MODE_OFF` as on Windows (OFF tears the tap down, so the macOS
  recording indicator goes off); `IM_AUDIO_MODE_EVERYTHING` → `IM_ERR_INVALID_ARG`; `IM_AUDIO_MODE_TEST_TONE` is 04's
  tone source and touches no Core Audio API. `im_audio_set_mode` crossfades over 20 ms (the bus's sound-off flag, then
  the tap is closed 30 ms later; opening it again builds a generation, §7).
- **Fast path.** `IM_EXCLF_FAST_PATH` is ignored on macOS (one tap is always the "fast path").
- **`shared_pid`** comes from 02's pick result (`im_source_info.pid`: the picked window's process on 15.2+; 0 for a
  display, and 0 for a window pick on 14.4–15.1, where the picker doesn't say which app, per `02-macos-engine.md`
  §8.2.1; §9.9 says what that costs). Application picks are later (`02-macos-engine.md` §1).
- **`IM_INIT_NO_AUDIO`.** No Core Audio call at all; `audio_flags = 0`; the calls answer as 05 §4.3 says.

## 5. The engine's audio components (`native/macos/src/audio/`, Objective-C++)

`02-macos-engine.md` owns the layout of `native/macos/`, its CMake target and the static library; this doc owns the
files below (all `.mm` unless noted, ARC on, `-fno-objc-arc` nowhere) and their tests in `native/macos/tests/audio/`.

```
native/macos/src/audio/
  audio_engine.h           im::audio::{MixerConfig, Mixer, AudioEngine}: M2's boundary, copied verbatim from
                           native/windows/src/audio/mixer.h, plus 02's macOS-only RenderTone/RenderBursts (§5.1)
  tap_engine.{h,mm}        TapEngine final : im::audio::AudioEngine; AudioEngine::Get() returns it; owns the queue
                           and the state (§5.1)
  proc_snapshot.{h,mm}     Core Audio process objects → tappolicy::ProcInfo, with the per-process cache (§9.6)
  responsibility.{h,mm}    dlsym of responsibility_get_pid_responsible_for_pid and the fallbacks (§9.7)
  tap_generation.{h,mm}    one CATapDescription + aggregate + IOProc + CaptureRing writer + rate stage (§7)
  tap_controller.{h,mm}    generations, live updates, crossfades, rebuild triggers, watchdogs (§7, §8)
  tap_mixer.{h,mm}         TapMixer final : im::audio::Mixer over audiomix::Mixer, its mach TickClock and thread
                           hook (§8.2)
  tcc.{h,mm}               TCCAccessPreflight via dlsym (§13.2)
  sound_check.{h,mm}       im_audio_sound_check (§13.3)
  test_render.{h,mm}       im_test_render_tone / im_test_render_bursts on macOS (§13.5)
  report_mac.{h,mm}        tappolicy verdicts + controller state → audiomix::Report (§12)
native/common/tappolicy/   portable C++20, no OS headers (§9)
  include/im/tappolicy/{types,classify,plan}.h
  src/{classify,plan,identity}.cpp
  tests/tappolicy_{classify,plan,identity,property,cases}_test.cpp
```

### 5.1 The facade (C++ boundary with 02's export layer)

This doc implements M2's boundary **unchanged** (`02-macos-engine.md` §10, M2 04 §10.2 in the form of 05 §10.5).
`native/macos/src/audio/audio_engine.h` (this doc's file) copies the declarations of
`native/windows/src/audio/mixer.h` verbatim, namespace `im::audio`: `MixerConfig {mode, flags, sink}`, `Mixer
{Start(const MixerConfig &), Stop(), SetPaused(bool), State()}` and `AudioEngine {Get(), Init, Shutdown, ProbeFlags,
ListAppsJson, SetRules, Watch, Unwatch, SetMode, SetMuted, LevelsGet, SoundCheck, NewMixer}`, plus the two macOS-only
virtuals `02-macos-engine.md` §10 adds for the render exports (`RenderTone(const char *uid, float hz, float amp,
int32_t ms)` and `RenderBursts(…)`, filled by §13.5). 02's exports, `session.mm` and `audio_bridge_test` (with a fake
`AudioEngine`) compile against it; sessions reach the engine through `audio_bridge::Engine()` (`SetEngineForTest` for
a fake), as on Windows.

```cpp
namespace im::audio {

// tap_engine.h. AudioEngine::Get() (defined in tap_engine.mm) returns the process-wide TapEngine.
class TapEngine final : public AudioEngine {
 public:
  im_status Init(uint32_t init_flags) override;             // im_init; IM_INIT_NO_AUDIO → inert (§4.3)
  void Shutdown() override;                                 // im_shutdown: stops everything, clears stored rules
  uint32_t ProbeFlags() override;                           // im_caps.audio_flags (§13.1), <= 50 ms, cached
  int32_t ListAppsJson(uint32_t flags, char *buf, int32_t cap) override; // im_list_audio_apps (§12.1)
  im_status SetRules(const im_exclusions *ex) override;     // im_set_exclusions and im_start_config.exclusions
  im_status Watch(const im_exclusions *ex) override;        // im_audio_watch
  im_status Unwatch() override;                             // im_audio_unwatch
  im_status SetMode(uint32_t mode) override;                // im_audio_set_mode (§4.3)
  im_status SetMuted(bool muted) override;                  // im_audio_set_muted
  im_status LevelsGet(im_audio_levels *out) override;       // im_audio_levels_get, lock-free (§12.2)
  int32_t SoundCheck(uint32_t helper_pid, int32_t timeout_ms, char *buf, int32_t cap) override; // §13.3
  std::unique_ptr<Mixer> NewMixer() override;               // a TapMixer; nullptr only after IM_INIT_NO_AUDIO
  im_status RenderTone(const char *uid, float hz, float amp, int32_t ms) override;   // §13.5
  // RenderBursts: 02's signature, §13.5's semantics.
};

// tap_mixer.h: one share's audio. Created by NewMixer, owned by 02's session.
class TapMixer final : public Mixer {
 public:
  im_status Start(const MixerConfig &cfg) override; // <= 100 ms: audiomix::Mixer running, first generation queued
  void Stop() override;                             // <= 300 ms (inside M2's <= 500 ms): mixer joined, taps destroyed
  void SetPaused(bool paused) override;             // from im_set_paused (05 §9.6's pause flag)
  uint32_t State() const override;                  // IM_AUDIO_STATE_* for im_session_info
};

}  // namespace im::audio
```

`TapMixer::Start(cfg)` returns `IM_OK` within 100 ms with the mixer thread running and the first generation being
built on the engine queue (state `starting` until the first IO callback, then `ok`, or `permission-pending`, §13.2).
`cfg.sink` is 04's `OpusStage` (and PCM tap), owned by the session and outliving the mixer; chunks reach it from the
mixer thread from the first tick on (silence until the tap delivers), exactly as 05 §10's mixer does on Windows.
`Start` fails only with `IM_ERR_INVALID_ARG` (no sink) or `IM_ERR_INTERNAL` (no thread). There is no limit check
here: 02's `im_start` already refuses a second audio session with `IM_ERR_LIMIT` `session_limit` before it calls
`NewMixer`, as `native/windows/src/session/session.cpp` does. `SetPaused` and `State` are per mixer; mode and mute
stay engine-wide (`SetMode`, `SetMuted`) and apply to the running mixer.

## 6. The tap

### 6.1 Description (macOS 14.2+; the product floor is 14.4)

```objc
CATapDescription *d =
    [[CATapDescription alloc] initStereoGlobalTapButExcludeProcesses:excludedObjects]; // NSArray<NSNumber *> of AudioObjectID
d.name = @"isshoni";
d.privateTap = YES;                 // invisible to other processes, destroyed with ours
d.muteBehavior = CATapUnmuted;      // we never mute what the user hears
if (@available(macOS 26.0, *)) {    // compiled only with the macOS 26 SDK (IM_HAVE_SDK26, §6.4)
  if (useBundleIDs) {               // not with IM_AUDIOF_TEST_NO_BUNDLE_IDS or NO_EXCLUSIONS
    d.bundleIDs = plan.bundle_ids;  // §9.8
    d.processRestoreEnabled = YES;
  }
}
AudioObjectID tap = kAudioObjectUnknown;
OSStatus st = AudioHardwareCreateProcessTap(d, &tap);
```

- `excludedObjects` is the tap plan's object list (§9.8), sorted, and **filtered right before use** to objects that
  still exist (`kAudioProcessPropertyPID` readable and > 0). This is the plan's "skip unknown process objects": a
  vanished object makes `AudioHardwareCreateProcessTap` or the live update fail with `kAudioHardwareBadObjectError`
  ('!obj'). On that status the engine re-snapshots once and retries; a second failure is a build failure (§7.5).
- With `IM_AUDIOF_TEST_NO_EXCLUSIONS` the list is empty and no bundle IDs are set.
- The tap's own format is read from `kAudioTapPropertyFormat` (§7.4); the tap UID from `kAudioTapPropertyUID`.

### 6.2 Live update

`desc.processes = objects; desc.bundleIDs = ids (26+)` then `AudioObjectSetPropertyData(tap,
kAudioTapPropertyDescription, sizeof(CFTypeRef), &desc)`. S2 measured this in place on 27; on 14.x and 15 it is
unmeasured, so any failure falls back to a new generation with a crossfade (never a hard cut, §8.5), and the counter
`tapUpdateErrors` shows it.

### 6.3 What the tap hears

- Every process's output mix on the system's output path, before the device's volume (to be confirmed per version by
  manual check M-R10: S1 showed Windows' loopback is pre-volume; S2 didn't measure it). The SPA's `VolumeNote`
  ("Muting your PC doesn't mute your stream") is shown on macOS only if M-R10 confirms it on 14.4 and 27; slice AM12
  records the answer in `cmd/isshoni-audiotest/README.md`.
- Apps that play to a non-default output device: case 22 (INFO) records whether the tap hears them.
- When a voice app opens a Bluetooth headset's mic, macOS switches that headset to its hands-free profile and the
  output rate drops (16 or 24 kHz): the tap's format changes, the engine rebuilds (§7.6), and friends hear the
  narrower band the device mix carries. Documented in `docs/troubleshooting.md` (slice AM12), not fixable by isshoni.
- Apps that use Apple's voice processing (`kAudioUnitSubType_VoiceProcessingIO`) may duck other audio during calls;
  friends hear the ducking too. Windows' ducking tip has no macOS equivalent (`ducking_other.go` keeps it hidden);
  the troubleshooting page says so.

### 6.4 SDK and availability gating

- `CATapDescription`, `AudioHardwareCreateProcessTap`, `kAudioTap*`, `kAudioAggregateDeviceTapListKey`: 14.2+.
  `kAudioHardwarePropertyProcessObjectList`, `kAudioProcessProperty{PID,BundleID,Devices,IsRunningInput,
  IsRunningOutput}`, `kAudioHardwarePropertyTranslatePIDToProcessObject`: 14.0+. All are behind `@available` (the
  binary's deployment target is 14.4, `02-macos-engine.md`).
- `bundleIDs` and `processRestoreEnabled` need the macOS 26 SDK. `IM_HAVE_SDK26` is set whenever the engine compiles
  with SDK ≥ 26.0 (`#if defined(__MAC_26_0) && __MAC_OS_X_VERSION_MAX_ALLOWED >= __MAC_26_0`); its `#else` branch is
  `#error "the macOS 26 SDK is required"`, so a silent no-`bundleIDs` build can't exist (`02-macos-engine.md`'s
  `task native:mac` already refuses an SDK below 26). There is no separate build-info key: 02's `im_build_info.sdk`
  (Go `BuildInfo.SDK`, e.g. `"26.2"`) and the archive's `build.json` `sdk` report the SDK, and 04's machocheck
  enforces it (§21). CI builds the engine in 02's `macos-native` job on `macos-26`, with the Xcode that 04's
  `tools.json` step selects (§17).

## 7. Tap generations

### 7.1 One generation

A generation (`TapGeneration`) owns: the tap, a private aggregate device, one IOProc, the optional rate stage (§7.4),
and one `audiomix::CaptureRing` (500 ms at 48 kHz, as 05 §17). It is built on the engine queue in 16–26 ms (S2) and
destroyed there.

### 7.2 Aggregate device

```objc
NSDictionary *agg = @{
  @kAudioAggregateDeviceNameKey: @"isshoni capture",
  @kAudioAggregateDeviceUIDKey: NSUUID.UUID.UUIDString,     // fresh per generation
  @kAudioAggregateDeviceIsPrivateKey: @YES,
  @kAudioAggregateDeviceIsStackedKey: @NO,
  @kAudioAggregateDeviceTapAutoStartKey: @YES,
  @kAudioAggregateDeviceTapListKey: @[ @{ @kAudioSubTapUIDKey: tapUID, @kAudioSubTapDriftCompensationKey: @YES } ],
};
AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)agg, &aggID);
```

**Fallback** (variant `tap+output`): when `AudioDeviceStart` fails or no IO callback arrives within 500 ms, the
generation is torn down and rebuilt once with `kAudioAggregateDeviceMainSubDeviceKey` and
`kAudioAggregateDeviceSubDeviceListKey` set to the default output device's UID; before `AudioDeviceStart` the engine
sets `kAudioDevicePropertyIOProcStreamUsage` (input scope) so the IOProc uses only the tap's input stream and none of
the sub-device's input streams (so a headset mic is never opened). The report shows which variant runs
(`tap.aggregate`: `tap-only` or `tap+output`). If both fail, the build fails (§7.5).

### 7.3 IOProc (HAL real-time thread)

`AudioDeviceCreateIOProcIDWithBlock(&procID, aggID, NULL, block)`; the block calls a C function that:
1. finds the tap's buffers: the last `channels` buffers when non-interleaved, else the last buffer (taps come after any
   sub-device inputs; S2's `ringWrite`);
2. counts the callback, measures the buffer's peak and keeps the zero-run counter (frames of exact `0.0f` in a row;
   reset by any non-zero sample) and the "ever non-zero" flag (§7.7);
3. at 48 kHz: writes the interleaved stereo frames to the `CaptureRing` with `qpc_ns = hostNs(inInputTime->mHostTime)`
   (when `kAudioTimeStampHostTimeValid`, else `hostNs(inNow->mHostTime)` and the counter `untimedBuffers`) and
   `arrival_ns = hostNs(mach_absolute_time())`;
4. at another rate: runs the rate stage (§7.4) into a preallocated 4096-frame buffer and writes its output with
   `qpc_ns` shifted by the stage's latency.

No locks, no allocation, no Objective-C, no logging (counters only). `hostNs` is `mach_absolute_time` scaled by
`mach_timebase_info` (128-bit multiply), the engine clock of `im_clock().mono_ns` (`02-macos-engine.md`). The thread
does not join a workgroup (it is the HAL's); the mixer thread does (§8.2).

### 7.4 Format and sample rate (the "own resampler")

- The tap format must be linear PCM float32 (`kAudioFormatFlagIsFloat`, 32 bits), 1 or 2 channels, interleaved or
  not; anything else is a build failure with event `format_unsupported`. Mono is duplicated to both channels.
- Rate 48 000 Hz (±0.5): written as is.
- Any other rate (44 100, 88 200, 96 000, 16 000, 24 000, …): a speexdsp resampler created at generation build on the
  engine queue, `speex_resampler_init(2, in_rate, 48000, 5, &err)`, fed with `speex_resampler_process_interleaved_float`
  on the IO thread. Its timestamp shift is `speex_resampler_get_input_latency() / in_rate` seconds, subtracted from
  `qpc_ns`. This fixed-ratio stage does only the nominal conversion; drift between the device clock and the engine
  clock is the mixer's (§8.3).
- Unit test (Mac, CI): 44.1 → 48 kHz, 96 → 48 kHz and 24 → 48 kHz of a 1 kHz tone keep its level within ±0.2 dB, the
  frequency within 1 Hz, and the timestamps of a step within one sample after the latency shift
  (`tap_rate_stage_test`, in `native/macos/tests/audio/`, runs on CI macos without a device).

### 7.5 Build failures and retries

Any of `AudioHardwareCreateProcessTap`, the format read, `AudioHardwareCreateAggregateDevice`,
`AudioDeviceCreateIOProcIDWithBlock` or `AudioDeviceStart` failing (both aggregate variants) is a build failure: event
`tap_build_failed` with the OSStatus as a four-character code or decimal in `detail`, counter `activationFailures`.
With no generation running the state is `tap-failed` (`IM_AUDIO_STATE_FAILED`), the mixer feeds silence, and the
controller retries after 1, 2, 5 and 10 s, then every 10 s, and at once on a default-device change. A share never
stops for it. Not being able to create a tap at all on 14.4+ is not expected; the state exists so it can never be
silent.

### 7.6 Rebuild triggers (state stays `ok`; `rebuilding: true` while no generation delivers)

| Trigger | Listener or check | Action |
|---|---|---|
| Default output device changed | `kAudioHardwarePropertyDefaultOutputDevice` on the system object | rebuild (new generation, no crossfade: the old tap's audio is from the old device) |
| Nominal rate of the default output changed | `kAudioDevicePropertyNominalSampleRate` on that device (re-registered on device change) | rebuild |
| Tap format changed (rate or channels) | `kAudioTapPropertyFormat` on the tap (S2) | rebuild |
| Output device gone | `kAudioDevicePropertyDeviceIsAlive` on the device; `kAudioHardwarePropertyDevices` | rebuild; with no output device at all: state `no-device`, silence, rebuild on the next device-list change |
| Core Audio restarted | `kAudioHardwarePropertyServiceRestarted` | forget all object ids and the cache's object ids, re-snapshot, rebuild |
| Live update failed | `AudioObjectSetPropertyData` ≠ noErr (after one '!obj' retry) | new generation with a crossfade (§8.5) |
| Zero run | §7.7 | rebuild with backoff |

A rebuild is: build the new generation first (the old one keeps running), add its channel (non-crossfading, it fades
in after its preroll), remove the old channel (10 ms fade), destroy the old generation once its channel is released.
Within the gap the old tap's frames still play, so a rebuild for a device change usually leaves less than 100 ms of
silence. **Exclusions hold across a rebuild**: the new generation is built from the current plan; the old one keeps its
last list (and receives additions, §8.5) until it is gone. Case 33 checks no leak across a rate-change rebuild.

### 7.7 Zero-run watchdog (the plan's "runs of zero buffers")

A tap that silently delivers zeros (the plan's main macOS risk) looks like silence. The controller checks every
250 ms on the engine queue:
- **Suspect** when the ring's zero run is ≥ 2 s **and** some process classified `heard` has
  `kAudioProcessPropertyIsRunningOutput` true for the whole run **and** the permission state is `granted` or
  `unknown` (§13.2).
- **Action**: rebuild (event `zero_run_rebuild`, counter `zeroRunRebuilds`). Backoff while the condition persists:
  2 s, 10 s, 30 s, then every 60 s. Reset when any non-zero sample arrives.
- **Never** a banner on the first suspicion: an app can hold its output running while it plays digital silence
  (browsers with a paused video, games in menus). After three rebuilds in a row without a non-zero sample the report
  gets the warning `possibly_silent_tap` (top-level `warnings[]`), which the service shows as `capture-trouble`
  without an app name only when the sound check failed in this app run as well; otherwise it is diagnostics only.
- A rebuild of a silent tap is inaudible, so the backoff is about CPU, not sound.

## 8. Mixer, crossfade and timestamps

### 8.1 Why reuse `MixEngine`

04's Opus stage needs 10 ms chunks on an exact grid of the engine clock (04 `pcm_sink.h`). The tap runs on the output
device's clock, which drifts against `mach_absolute_time` (USB and Bluetooth devices by up to a few hundred ppm). M2's
`audiomix::MixEngine` already turns timestamped capture rings into that grid with PI-controlled speexdsp resampling
(05 §9.3: Kp 100, Ki 2.5, the 62 500 denominator), the stamping delay L (05 §10.3), the silence flags (05 §9.6), levels,
the onset and tail fades and the soft clip. On macOS it mixes one channel, or two during a crossfade. Tested values
carry over: drift ±500 ppm holds the fill at 40 ± 15 ms (05 §17).

### 8.2 Mixer thread

- One M2 `audiomix::Mixer` per running share (`native/common/audiomix/include/im/audiomix/mixer.h`, as Windows' live
  mixer uses it), created by `TapMixer::Start`, no hand-written thread loop. `MixerOptions`: `sink` = a `ChunkSink`
  adapter onto `cfg.sink` (the Windows adapter's pattern), `clock` = `MachTickClock` (a `TickClock` whose `NowNs` is
  `hostNs(mach_absolute_time())` and whose `WaitUntil` is `mach_wait_until` on the deadline, returning early on stop),
  `params` as on Windows, and `thread_hook` = a function that names the thread `isshoni.audio.mix`, calls 02's
  `im::mac::SetRealtime(10 ms, 2 ms, 10 ms)` (time-constraint policy) and joins the running generation's IO workgroup
  (`kAudioDevicePropertyIOThreadOSWorkgroup` of the aggregate, `os_workgroup_join`) when `starting`, and leaves it
  when not. On a rebuild the controller sets an atomic "rejoin" flag; `MachTickClock::WaitUntil`, which runs on the
  mixer thread, sees it before its next wait, leaves the old workgroup and joins the new one (`os_workgroup_join`
  with a join token on the stack, no allocation). Join failures are counted, never fatal.
- The sink is 04's `OpusStage` (passed by 02's session in `MixerConfig.sink`), inline on the mixer thread.
- `TapMixer::Stop`: `audiomix::Mixer::Stop` (join ≤ 50 ms), `MixEngine::ReleaseAll`, then the generations are
  destroyed on the queue (≤ 300 ms in all).

### 8.3 Channels

Each generation's `CaptureRing` becomes one `audiomix::Channel` (`ChannelOptions{ring, tag = generation number, group
= generation number & 1, crossfade, align}`; speexdsp resampler, M2's controller parameters). Group values 0 and 1
alternate, so every crossfade is a takeover between two groups (05's "any value works"). `MixParams` as on Windows,
L0 = 50 ms.

### 8.4 Aligned takeover (new, `native/common/audiomix`)

The crossfade of S2 mixes two taps that carry the same audio except for the app that changes sides, so the two signals
must be sample-aligned (an unaligned equal-gain fade of the same music comb-filters for 20 ms). M2's takeover
(`ChannelOptions::crossfade`) starts the incoming channel after a fresh 40 ms preroll, so its head age differs from the
outgoing channel's by up to 10 ms. This doc adds:

```cpp
struct ChannelOptions {
  // ... M2's fields ...
  // With crossfade: start sample-aligned with the channel it takes over (docs/m3/03-macos-audio.md §8.4). Windows
  // never sets it.
  bool align = false;
};
```

Semantics, all on the mixer thread:
1. An aligned crossfading channel skips Preroll's "empty the ring and fill 40 ms". At each tick it looks up the
   outgoing group's audible channel with the highest gain (the **reference**) and the capture time `t_ref` of the
   reference's next input frame (`CaptureRing::HeadQpc` before this tick's consumption).
2. If its own ring holds the frame captured at `t_ref` plus `kInFrames` frames after it (`FramesBefore(t_ref)` gives
   the index; S2: two taps align to 0 ± 1 samples by capture time), it drops the frames before that index, copies the
   reference's controller state (steps, integral, smoothed age) and resampler ratio, enters Running at gain 0 and the
   takeover starts: 20 ms, equal gain (`g = 0.5 − 0.5·cos(π·k/960)` in, `1 − g` out), exactly M2's engine crossfade.
3. If its ring doesn't reach `t_ref + kInFrames` within 50 ms of ticks (the new tap's IO thread can run one buffer
   behind, S2), or the frame at `t_ref` was already dropped (overflow), it falls back to M2's unaligned takeover and
   raises the event `xfade_unaligned` (counter `unalignedCrossfades`).
4. Sub-sample phase isn't copied (speexdsp's state is private): the residual offset is < 1 sample, a level dip
   under 0.3 dB at 1 kHz, which case 29 bounds.

Tests (Mac, CI ubuntu/macos/windows): two simulated rings fed the same tone with the same timestamps (one starting
30 ms later): with `align` the output during the fade stays within ±0.3 dB of the steady level and the click detector
(05 §18.6) finds nothing; without `align` the existing behavior and every existing `audiomix` test are unchanged; the
50 ms fallback; the overflow fallback; no allocation on the mixer thread (`mix_alloc_test` extended).

### 8.5 When to crossfade, and what the old tap gets

The tap controller compares each new plan (§9.8) with the running one:

| Change | Action |
|---|---|
| Only objects that appeared since the last plan (new processes), or objects whose process has running output in neither the previous nor the current snapshot (nor the fresh read below), change sides; bundle IDs change | **live update** of the running tap (§6.2); instant, inaudible (S2: the late-start cases) |
| An object whose process has running output in the previous **or** the current snapshot (or the fresh read below) changes sides (a rule change, a mic detected, a decision) | **crossfade**: build generation N+1 with the new plan, add its channel with `crossfade = align = true`; when its takeover completes, destroy N |
| A second audible change while a crossfade is pending | queued; applied as the next crossfade after promotion (at most one pending) |
| A new object to exclude while a crossfade is pending | added to **both** generations by live update (adding a non-playing object cuts nothing); this is where M3 differs from S2's "the old tap gets no changes", which would let a voice app that starts during the 60–140 ms fade leak through the old tap |
| N+1's channel not Running within 500 ms (tap never delivered) | abort: destroy N+1, apply the plan to N by live update (a hard cut, counter `crossfadeAborts`, event `crossfade_aborted`) |
| Live update failure | crossfade instead (§7.6) |

**Why both snapshots and a fresh read**: the flags are polled every 250 ms (§9.6), so an app whose output started after
the previous poll would look silent in it, and a live update would cut its waveform (S2 finding 3: an audible app
clicks). The likeliest case is a voice app detected as a mic user in the same poll that first sees its call audio. So
the decision takes running output from the previous **or** the current snapshot. For plans built outside a poll (a
rule change, a decision, a viewer rule), the controller first re-reads `kAudioProcessPropertyIsRunningOutput` for
just the objects that change sides, on the engine queue (one property read each, microseconds), and counts a `true`
read as running. Unit test (`tappolicy` and the fake-HAL controller suite, §18.1): previous snapshot not running, the
current snapshot or the fresh read running → crossfade, never a live update.

Exclusions that are already in force are never removed from the old tap while it is audible, so a crossfade can't
leak. Budget: a rule change of an audible app is complete (old tap released) ≤ 200 ms after the change reaches the
engine; the audio cut it makes is measured by case 15 (≤ 150 ms on bare metal, plus one guest IO buffer in a VM,
§16.5).

### 8.6 Timestamps and A/V sync

- Each chunk's capture time is M2's: `tick_k − L` on the engine clock, with every channel's controller holding its
  head age at L (05 §10.3, `mix_engine.h`). The ring's `qpc_ns` is the IO buffer's `mHostTime`, the HAL's time for the
  first frame of the tap's input buffer: the time Core Audio mixed that audio for the device, **without** the output
  device's latency. Video (SCK) stamps the frame's display time (`02-macos-engine.md`); friends never hear the sharer's
  speakers, so neither side adds device latency.
- Offset to verify: case 20 measures the difference between the bursts' predicted output time (§13.5) and the tap's
  stamps. The offset can depend on the aggregate variant (`tap-only` has no clock sub-device; `tap+output` carries the
  output device's safety offset and buffer size) and on the OS version, and VMs' virtio audio uses larger buffers. So
  slice AM9 records case 20's median on macOS 27 bare metal, and AM12's gate runs (in 05's V1 VMs too) record it for
  every (OS major, aggregate variant) that runs, all in §21's change log (`stampOffsetMedianMs` in the report, §16.7).
  The correction is a small table in `tap_generation.mm`:
  ```cpp
  enum class TapAggregate { kTapOnly, kTapPlusOutput };          // the report's `tap.aggregate`
  struct TapStampOffset { int os_major; TapAggregate variant; int64_t offset_ns; };
  // Filled from §21 only for recorded medians outside ±5 ms; the {0, …, 0} sentinel keeps the array non-empty.
  constexpr TapStampOffset kTapStampOffsetNs[] = {{0, TapAggregate::kTapOnly, 0}};
  int64_t TapStampOffsetNs(int os_major, TapAggregate variant);  // 0 when no entry matches; added to qpc_ns
  ```
  An entry is added only when a recorded median falls outside ±5 ms; otherwise the offset stays 0. `tap-only` stays
  the default variant (S2 finding 6; `tap+output` is the fallback of §7.2).
- Targets: chunk spacing exactly 10 ms, Opus packet spacing exactly 20 ms within a grid (04's stage); mix-time error
  ≤ 15 ms median and ≤ 5 ms p5–p95 spread (case 20); real-share A/V offset |median| ≤ 20 ms (`isshoni-enginetest
  avsync`, run by `05-exit-and-verification.md`; the plan's limit is 45 ms).
- Opus: 04's stage subtracts its lookahead (312 samples = 6.5 ms), so packets sit on an exact 20 ms grid; Go's
  publisher computes RTP timestamps and sends its own SRs (M1, unchanged).

### 8.7 Opus

M2's `im::audio::OpusStage` unchanged (05 §10.1): 48 kHz stereo, 20 ms frames, `OPUS_APPLICATION_AUDIO`, signal
music, constrained VBR, complexity 10, bitrate from `ShareParams.AudioBitrate` (128 k, Movie 256 k), FEC off, DTX off.
libopus and speexdsp come from vcpkg (`native/vcpkg.json` as is) for `02-macos-engine.md` §6.1's overlay triplets
(`native/triplets/arm64-osx-isshoni.cmake` and `x64-osx-isshoni.cmake`: static linkage, `VCPKG_OSX_DEPLOYMENT_TARGET`
14.4) and are linked into the engine's static library. No libopus build of our own: the
port is pinned by the manifest's baseline, BSD-3, and already builds on the Mac (`host-*` presets). The PCM tap
(`IM_AUDIOF_TAP_PCM`) and `im_test_opus_decode` are 04's and work unchanged.

## 9. Exclusion policy (`native/common/tappolicy`)

### 9.1 Inputs

```cpp
namespace im::tappolicy {

struct ProcKey { int32_t pid = 0; uint64_t start_us = 0; };  // kinfo_proc p_starttime as µs since the Unix epoch

struct ProcInfo {                    // one Core Audio process object, as proc_snapshot.mm fills it
  uint32_t object = 0;               // AudioObjectID
  ProcKey key;
  int32_t ppid = 0;                  // kp_eproc.e_ppid; 1 = launchd
  uint64_t ppid_start_us = 0;        // the parent's start time (0 = unknown)
  int32_t rpid = 0;                  // responsible pid (= pid without the SPI, §9.7)
  uint64_t rpid_start_us = 0;
  std::string bundle_id;             // kAudioProcessPropertyBundleID, lower case; "" when none
  std::vector<std::string> enclosing;      // IDs of every .app/.appex/.xpc enclosing the executable, inner → outer
  std::string exe;                         // executable file name, lower case (proc_pidpath's last component)
  std::vector<std::string> resp_enclosing; // the same for the responsible process (empty when rpid == pid)
  std::string resp_bundle_id;              // the responsible process's own bundle ID, lower case
  std::string resp_exe;
  std::string outer_app;                   // outermost enclosing .app's ID ("" = none)
  std::string resp_outer_app;
  std::string display_name;                // localized name of the identity's bundle (§9.5); never logged
  bool output_running = false;             // kAudioProcessPropertyIsRunningOutput
  bool input_running = false;              // kAudioProcessPropertyIsRunningInput
  bool mic_device = false;                 // an input device that is not an aggregate (§10.2)
  bool mic_device_indirect = false;        // input list non-empty but only aggregate/unresolvable devices; §10.2's
                                           // VPIO extension, only if AM5's measurement needs it
  bool other_user = false;                 // proc_pidpath failed with EPERM: classified by bundle_id only
  int64_t first_seen_ns = 0;               // engine clock
};

struct Snapshot {
  std::vector<ProcInfo> procs;
  int32_t self_pid = 0;
  bool physical_input_running = false;     // some non-aggregate input device has DeviceIsRunningSomewhere
                                          // (§10.2's VPIO extension, same condition)
  int64_t now_ns = 0;
};

}  // namespace im::tappolicy
```

### 9.2 Skipped and unknown objects

- An object whose `kAudioProcessPropertyPID` read fails or is ≤ 0: skipped (it is going away; it can't play).
- A pid that `sysctl(KERN_PROC_PID)` doesn't find: skipped the same way.
- A process of another user (`proc_pidpath` → EPERM): classified by `bundle_id` alone, `other_user = true`.
- The object list is re-read before every tap build and live update (§6.1).

### 9.3 App identity (the report row and the mic root)

The **identity process** of P is P itself, unless P has no enclosing `.app` and `rpid ≠ pid` and the responsible
process is alive and not a launcher (`IM_RULE_STOP`, §11.1): then it is the responsible process.
The **app identity** (row id basis) of P is, in order:
1. the identity process's outermost enclosing `.app` ID (`com.hnc.discord` for every Discord helper inside
   `Discord.app`);
2. else its `bundle_id` property (a bare `.xpc` or a process with an Info.plist section);
3. else `exe:<its executable name>`.

Examples: WebKit's GPU process (an `.xpc` inside WebKit.framework, no `.app`) gets its responsible app's identity
(Safari, isshoni, Mail); a game started by Steam with `posix_spawn` (responsible Steam, a launcher) is its own app;
`afplay` run from Terminal (a launcher) is `exe:afplay`.

The **mic root** of P is the identity process's key (pid, start time).

### 9.4 Rule matching and precedence

`Match(rule value v, process P)` for app rules:
- `v` is a bundle ID: true when any of P's `bundle_id`, `enclosing[*]`, and, if the identity process is the
  responsible process (§9.3), `resp_bundle_id` and `resp_enclosing[*]`, equals `v` or starts with `v + "."`
  (lower case). So `com.hnc.discord` matches `com.hnc.discord.helper.renderer`, but not `com.hnc.discordptb`.
- `v` is `exe:<name>`: true when P's `exe` (or, as above, `resp_exe`) equals `<name>`.

P is classified by the first matching row (M2's table, 05 §5.3, with macOS matching):

| # | Condition | Verdict | Reason | User control |
|---|---|---|---|---|
| 1 | P matches an `IM_RULE_INSTANCE` pid (§4.2: the pid, strict descendants, or `rpid` = it); or the implicit main-bundle rule matches exactly (§4.2) | kept out | `isshoni` | locked |
| 2 | an `IM_RULE_APP_EXCLUDE` with source BUILTIN matches | kept out | `builtin` | locked |
| 3 | … source USER matches | kept out | `user` | toggle |
| 4 | … source VIEWER matches | kept out | `viewer` | toggle (per-run override, 05 §5.9) |
| 5 | an `IM_RULE_APP_ALLOW` matches | heard | — | toggle |
| 6 | `IM_EXCLF_KEEP_OUT_MIC_USERS` is set, P's mic root (§9.3) is a sticky mic root (§10.3), and it is not the shared app's mic root (§9.9) | kept out | `mic` | toggle (prompt) |
| 7 | otherwise | heard | — | toggle |

There is no `launcher`, `tree`, `pid_reuse`, `limit` or `capture_failed` on macOS: one global tap can always hear a
process without hearing another one with it. A launcher's own sounds are heard.

**Lineage cache.** The first classification of a process key whose identity came from a live responsible process or
parent is kept for that key's lifetime (S2 case `helper-responsible`, audiotest case 18 `orphan`): a helper whose
voice-app parent exits keeps its identity and verdict inputs, whatever `responsibility_get_pid_responsible_for_pid`
returns for it later. Entries die with the process (its object disappears and `sysctl` no longer finds the key).

**PID reuse.** Every map is keyed by (pid, start time). A parent or responsible link counts only when the linked
process started before P (strict link); the instance rule follows strict links only, as 05 §5.3's row 1 does.

### 9.5 App ids and names

- `bundle:<identity bundle ID>` (lower case) for bundle identities, `exe:<name>` for unbundled ones. App ids must fit
  `[a-z0-9:._-]{1,63}` (05 §4.3): bundle IDs are `[a-z0-9.-]` after folding; an identity whose id would exceed 63
  bytes, or an executable name outside `[a-z0-9._-]`, gets the id `long:<first 16 hex of SHA-256(identity)>`, which
  rows, prompts and per-run decisions use but which **can't be saved** (no rule value can be rebuilt from it; the
  Service treats it like a dismissed prompt, §14.2). Built-in rules carry their own app id (`discord`).
- Display name: the identity bundle's `CFBundleDisplayName`, else `CFBundleName` (from its Info.plist, localized by
  `NSFileManager displayNameAtPath:` on the `.app`), else the executable name. Cached per bundle path. Like on Windows,
  names leave the engine only through `im_list_audio_apps` (05 §16).
- Row `exe` field (05 §12.1): on macOS the identity's **rule value**: the bundle ID or `exe:<name>`. Go builds user
  rules from it (§14.1).

### 9.6 The snapshot and its cache (`proc_snapshot.mm`)

- `kAudioHardwarePropertyProcessObjectList` → for each object: `kAudioProcessPropertyPID`, `BundleID`,
  `IsRunningOutput`, `IsRunningInput`, and, when `IsRunningInput`, `kAudioProcessPropertyDevices` (input scope).
- Per process key (cached until the key disappears): `sysctl(KERN_PROC_PID)` (ppid, start time, `p_comm`),
  `proc_pidpath`, enclosing bundle IDs (walk up the path; read each `Contents/Info.plist`'s `CFBundleIdentifier`; cache
  per bundle path, at most 2 000 entries), responsible pid (§9.7) and the same data for it, display name.
- Budget: a list change with one new object classified ≤ 5 ms; a full snapshot of 64 objects ≤ 30 ms (S2 measured
  20–30 ms uncached, so the cache is what makes the late-start path fast). Measured in `proc_snapshot_test` (Mac).
- The 250 ms poll re-reads only the per-object flags (`IsRunningOutput`, `IsRunningInput`, `Devices`), and, only when
  §10.2's VPIO extension is on, `kAudioDevicePropertyDeviceIsRunningSomewhere` of each input-capable non-aggregate
  device.

### 9.7 Responsible PID (`responsibility.mm`)

- `responsibility_get_pid_responsible_for_pid` looked up once with `dlsym(RTLD_DEFAULT, …)` (S2). Signature
  `pid_t (*)(pid_t)`. Probe bit `IM_AUDIO_RESPONSIBLE_PID`; report `tap.responsibleSpi`.
- **Fallback without it**: `rpid = pid`; identity comes from the enclosing bundles and the parent chain is used for the
  instance rule only. Consequences, all shown: (1) isshoni's WebKit GPU and WebContent processes can't be matched, so
  the report sets `selfAudible: true` and the in-app viewer is muted while live (§14.4); (2) unbundled voice-app
  helpers launched outside the app bundle can be heard (the built-in voice apps keep their helpers inside the bundle;
  `bundleIDs` on 26+ also covers Electron helper IDs); (3) Safari's WebKit processes get no app identity:
  `exe:com.apple.webkit.gpu`, which stays heard unless the user keeps it out (that would also keep isshoni's own
  WebKit audio out, which is fine). The check runs at `Init`; there is no runtime switch.
- **Not its own responsible process**: at `Init`, when the SPI exists and
  `responsibility_get_pid_responsible_for_pid(getpid()) != getpid()` (isshoni started from a terminal: the unbundled
  `task desktop:dev` run, or 01 §6.1's LaunchServices relaunch failed and the app continued in place), WebKit's GPU and
  WebContent processes are attributed to the terminal, not to isshoni, so rule 1 can't match them. The report then sets
  `selfAudible: true` too, with the same consequence (1) as above. Checked once at `Init` (a process's responsible
  process doesn't change). Tests: a `tappolicy` fake snapshot whose self process has `rpid ≠ pid` and a WebKit GPU
  process responsible to that terminal (the GPU process stays heard, `selfAudible` true), and the golden
  `report-macos-self-audible-not-responsible.json` (§12.1).

### 9.8 The tap plan

```cpp
struct TapPlan {
  std::vector<uint32_t> objects;          // sorted AudioObjectIDs of kept-out processes
  std::vector<std::string> bundle_ids;    // 26+: sorted, unique (below)
  std::vector<int32_t> audible_changed;   // pids whose verdict changed while output was running in prev or now
                                          // (incl. the controller's fresh reads folded into `now`) → crossfade
};
TapPlan Plan(const Snapshot &now, const Verdicts &v, const Snapshot *prev, const Verdicts *prev_v,
             const Exclusions &ex, bool use_bundle_ids);
```

For a plan built outside a poll, the controller copies the last snapshot to `now` and overwrites `output_running`
of the objects whose verdict changes with a fresh `kAudioProcessPropertyIsRunningOutput` read (§8.5) before calling
`Plan`.

- `objects`: every kept-out process's object.
- `bundle_ids` (26+, unless `IM_AUDIOF_TEST_NO_BUNDLE_IDS`): every `IM_RULE_APP_EXCLUDE` value that is a bundle ID
  (sources BUILTIN, USER, VIEWER, SELF), each also with the Electron helper suffixes `.helper`, `.helper.renderer`,
  `.helper.gpu`, `.helper.plugin` (S2), plus the own `bundle_id` of every process kept out by rows 1–4 when that bundle
  ID itself matches the rule's value at a dot boundary (never a bundle ID reached through the responsible process or an
  instance rule: WebKit's `com.apple.webkit.gpu` also serves Safari and every other app). Never mic users by bundle ID
  (mic exclusion is per process and sticky, not per app install), never allow rules. Core Audio matches these exactly,
  so this list is a backstop for helpers that start before the listener reacts; the object list stays authoritative.
- Precedence holds: a bundle ID enters the list only through a rule whose row is above every row that could make a
  matching process heard (rows 1–4 beat row 5), so `bundleIDs` never keeps out a process the table says is heard.
  `tappolicy_property_test` asserts it on random snapshots.

### 9.9 The shared app

`shared_pid` (§4.3) → its app identity and mic root. Row 6 never applies to that mic root (games have their own voice
chat; plan): the row gets warning `shared_app_uses_mic`. Rows 2–4 still apply, with warning `shared_app_kept_out`, as
on Windows (05 §5.8). With `shared_pid = 0` (a display) nothing is special.

Before 15.2 a window pick gives `shared_pid = 0` too (`02-macos-engine.md` §8.2.1), so no app is treated as shared. A
shared game that uses the mic (in-game voice chat) is kept out by row 6 like any mic user, and the user gets M2's
`mic` prompt for it (allow → heard). The report row has no `shared` flag. This is an accepted degradation for
14.4–15.1; case 14 (§16.5) records it as that version range's expectation.

### 9.10 Browsers

Every Chromium browser plays all tabs from one utility helper inside its `.app`; Firefox from its own process or a
helper inside its `.app`; Safari from WebKit's GPU process, whose responsible app is Safari. So on macOS too a browser
is one app (05 §5.5): a browser that uses the mic is kept out whole (`browser-mic` prompt), and the viewer rule keeps
the whole browser out (§14.3).

### 9.11 Classification examples

| Process | Rules | Result |
|---|---|---|
| `Discord.app/Contents/Frameworks/Discord Helper (Renderer).app` (`com.hnc.discord.helper.renderer`) | built-in `com.hnc.discord` | kept out `builtin`, row `discord` |
| `com.apple.WebKit.GPU`, responsible = isshoni | implicit | kept out `isshoni` (row 1 via `rpid`) |
| `com.apple.WebKit.GPU`, responsible = Safari, Safari playing the user's isshoni viewer | viewer `com.apple.safari` | kept out `viewer`, row `bundle:com.apple.safari` |
| Game spawned by Steam (`posix_spawn`, responsible Steam), Steam holds the mic | launcher `com.valvesoftware.steam`, mic flag | Steam kept out `mic`; game heard (its own identity) |
| `Google Chrome Helper.app` with an input device (Discord Web) | browser `com.google.chrome`, mic flag | whole Chrome kept out `mic`, `browser-mic` prompt |
| An app Discord opened with LaunchServices | built-in | heard (own responsible process; S2 `app-boundary`) |
| A plain helper executable outside any bundle, spawned by FakeVoice | user keep-out on FakeVoice | kept out via responsible (S2 `helper-responsible`) |
| Shared window's app holds the mic | mic flag, `shared_pid` | heard, warning `shared_app_uses_mic` |

## 10. Mic detection

### 10.1 Why not `IsRunningInput` alone

S2 finding 4: `kAudioProcessPropertyIsRunningInput` is also true for processes that read a process tap (isshoni
itself, other visualizers and recorders), because they record from a private aggregate device.

### 10.2 The rule

A process is a **mic user** when `IsRunningInput` is true **and** `kAudioProcessPropertyDevices` (scope
`kAudioObjectPropertyScopeInput`) lists at least one device whose `kAudioDevicePropertyTransportType` is readable and is
neither `kAudioDeviceTransportTypeAggregate` nor `kAudioDeviceTransportTypeAutoAggregate`. Built-in, USB, Bluetooth and
virtual microphones (Krisp, Loopback-style drivers) count; another process's private aggregate is unresolvable and
doesn't. Polled every 250 ms on the engine queue while watching or live (property listeners on every object would cost
more than the poll; the late-start race is about output, not input).

**Voice processing (VPIO), to be measured.** Many voice clients capture through Apple's voice processing
(`kAudioUnitSubType_VoiceProcessingIO`): FaceTime-style apps, Zoom, Teams, Chromium WebRTC with system echo
cancellation. VPIO runs its IO on a private aggregate device that it creates itself, and S2 finding 4 says another
process can't resolve such an aggregate. If `Devices` then lists only that aggregate (or an unresolvable id), the rule
above never counts the app and its call audio would leak. S2 verified only plain input (FakeVoIP). So:
- **Measure (slice AM5, before AM7)**: with the `-mic-vpio` fixture (§16.3) on the Mac (27), record what
  `IsRunningInput` and `Devices` (input scope; each entry's transport type and whether it resolves) report for the
  VPIO process, and write the result into §21's change log. AM12's gate runs record the same per version through case
  13v's report rows.
- **Extend the rule only if the measurement shows the gap** (no real input device listed): a process also counts as a
  mic user when `IsRunningInput` is true, **and** its input device list is non-empty but holds only aggregate or
  unresolvable devices (`ProcInfo::mic_device_indirect`), **and** it is not isshoni (row 1's self, §9.4) or one of
  isshoni's own processes, **and** at least one physical (non-aggregate) input device has
  `kAudioDevicePropertyDeviceIsRunningSomewhere` = true (`Snapshot::physical_input_running`, read on the same 250 ms
  poll from `kAudioHardwarePropertyDevices`, input-capable devices only).
- **The false positive this accepts**: a tap reader (a recorder or visualizer reading its own process tap) that runs
  while some other app uses the microphone is counted as a mic user and kept out (with M2's `mic` prompt, so the user
  can allow it). Only isshoni is a known tap reader, and it is excluded by name. Recorders that capture system audio
  while the user is in a call are rare and the cost is that their own playback is not heard.
- If the measurement shows VPIO lists a real input device, the rule stays as above and the two fields stay unused
  (`false`); the `tappolicy` tests for the extension are added either way so a later OS change can switch it on with
  one flag (`kVpioIndirectMic` in `tappolicy/rules.cpp`).

### 10.3 Stickiness, prompts and isshoni itself

- Once a process is a mic user, its **mic root** (§9.3) stays a mic root until that root process exits (its key leaves
  the snapshot and `sysctl` no longer finds it). Every process with the same app identity is kept out with it (row 6).
  This replaces S2's 10 s window, for the push-to-talk reason of 05 §5.6.
- Mic detection runs whenever the watcher runs, whatever the flag; the report always carries `usesMic`.
- isshoni never opens an input device. A self-test (`MacMicHW.*`, §16.8, Mac with the grant) asserts that isshoni's
  own process object, while a share's tap runs, lists no real input device, so macOS never shows its orange microphone
  indicator for isshoni (manual check M-R5 also looks at it).

## 11. Built-in data (`internal/media/audiorules`, macOS lists)

All values lower case, unique across lists (the existing uniqueness test extends to `MacOS`), each a bundle ID
(`[a-z0-9-]+(\.[a-z0-9-]+)+`) or `exe:<name>`. **Every bundle ID below is the expected one and is verified on a real
install before `v0.3.0`** (manual checks M-R1, M-R6, M-R7 and the `mdls -name kMDItemCFBundleIdentifier` step of
§16.9); an ID that turns out wrong is fixed, the app is never dropped (05 §5.4's rule), and unverified rows are marked
"unverified" in `cmd/isshoni-audiotest/README.md`.

### 11.1 Lists

| App id | Name | MacOS |
|---|---|---|
| `discord` | Discord | `com.hnc.discord`, `com.hnc.discordptb`, `com.hnc.discordcanary`, `com.hnc.discorddevelopment` |
| `vesktop` | Vesktop | `dev.vencord.vesktop`, `io.github.equicord.equibop` |
| `teamspeak` | TeamSpeak | `com.teamspeak.teamspeak3`, `com.teamspeak.teamspeak` |
| `mumble` | Mumble | `info.mumble.mumble`, `net.sourceforge.mumble.mumble` |
| `zoom` | Zoom | `us.zoom.xos`, `us.zoom.cpthost` |
| `teams` | Microsoft Teams | `com.microsoft.teams2`, `com.microsoft.teams` |
| `slack` | Slack | `com.tinyspeck.slackmacgap` |

Launchers and shells (`IM_RULE_STOP`, MacOS): `com.apple.finder`, `com.apple.dock`, `com.apple.terminal`,
`com.googlecode.iterm2`, `dev.warp.warp-stable`, `com.mitchellh.ghostty`, `net.kovidgoyal.kitty`,
`com.github.wez.wezterm`, `com.valvesoftware.steam`, `com.epicgames.epicgameslauncher`, `net.battle.app`,
`com.heroicgameslauncher.hgl`, `com.gog.galaxy`, `com.codeweavers.crossover`, `com.isaacmarovitz.whisky`,
`com.apple.launchpad.launcher`, `exe:launchd`, `exe:zsh`, `exe:bash`, `exe:sh`, `exe:fish`, `exe:login`.

Shared runtimes (MacOS): none (WebKit's XPC services are handled by the responsible PID).

Browsers (`IM_RULE_BROWSER`, MacOS):

| Family | MacOS |
|---|---|
| `chrome` | `com.google.chrome`, `com.google.chrome.canary`, `org.chromium.chromium`, `com.brave.browser`, `com.vivaldi.vivaldi`, `com.operasoftware.opera`, `company.thebrowser.browser`, `com.thorium.thorium` |
| `edge` | `com.microsoft.edgemac`, `com.microsoft.edgemac.beta`, `com.microsoft.edgemac.dev` |
| `firefox` | `org.mozilla.firefox`, `org.mozilla.firefoxdeveloperedition`, `org.mozilla.nightly`, `io.gitlab.librewolf-community`, `net.waterfox.waterfox`, `one.ablaze.floorp`, `app.zen-browser.zen` |
| `safari` (new family, M3) | `com.apple.safari`, `com.apple.safaritechnologypreview` |

### 11.2 Package changes (`internal/media/audiorules`)

- `FamilySafari = "safari"`; `Browsers()` gains it (its `Windows` list empty). M1's `client.browser` value `safari`
  now has a family; on Windows it still adds no rule (the list is empty) and keeps M2's banner (§14.3).
- `OSNames.Contains(os, name)`: for `darwin`/`macos`, true when `name` equals an entry or starts with an entry followed
  by `.` (both lower case; `exe:` entries match exactly). Windows and Linux unchanged (exact).
- `VoiceAppByProcess` and `BrowserByProcess` use `Contains`, so they work with macOS row `exe` values unchanged.
- Tests: the macOS values' syntax; no bundle ID is a prefix-at-dot of an entry of another list (so a browser is never
  also a launcher by prefix); `com.hnc.discord` doesn't contain `com.hnc.discordptb`; the Windows lists are byte for
  byte M2's.

## 12. Reports

### 12.1 `im_list_audio_apps` on macOS (schema v1, additive)

```json
{
  "v": 1,
  "seq": 57,
  "qpcNs": 912345678901234,
  "mode": "exclude-apps",
  "engine": "process-tap",
  "state": "ok",
  "muted": false,
  "paused": false,
  "live": true,
  "rebuilding": false,
  "probe": {"result": "ok", "osBuild": 270200, "format": "float32", "levelDb": -120.0, "ms": 21},
  "device": {"present": true, "renderEndpoints": 2, "captureEndpoints": 1},
  "selfAudible": false,
  "tap": {"rate": 48000, "channels": 2, "aggregate": "tap-only", "bundleIds": true, "responsibleSpi": true,
          "permission": "granted", "generations": 1},
  "apps": [
    {"id": "bundle:com.spotify.client", "name": "Spotify", "exe": "com.spotify.client", "verdict": "heard",
     "reason": "", "playing": true, "usesMic": false, "browser": false, "shared": false, "pids": 2, "sessions": 1,
     "streams": 0, "detail": "", "warnings": []},
    {"id": "discord", "name": "Discord", "exe": "com.hnc.discord", "verdict": "kept_out", "reason": "builtin",
     "playing": true, "usesMic": true, "browser": false, "shared": false, "pids": 5, "sessions": 2,
     "streams": 0, "detail": "", "warnings": []},
    {"id": "isshoni", "name": "isshoni", "exe": "io.isshoni.desktop", "verdict": "kept_out", "reason": "isshoni",
     "playing": true, "usesMic": false, "browser": false, "shared": false, "pids": 2, "sessions": 1,
     "streams": 0, "detail": "", "warnings": []}
  ],
  "sessions": [
    {"pid": 4120, "created": "1793500000123456", "exe": "com.spotify.client", "appId": "bundle:com.spotify.client",
     "flow": "render", "state": "active", "multiProcess": false, "verdict": "heard", "reason": "",
     "stream": "none", "format": "", "note": ""}
  ],
  "warnings": [],
  "counters": {"streams": 1, "activations": 1, "activationFailures": 0, "underruns": 0, "trims": 0, "tailFades": 0,
    "overflows": 0, "discontinuities": 0, "mixerResyncs": 0, "fastpathExits": 0, "fastpathLateCuts": 0,
    "silentReopens": 0, "deviceChanges": 0, "pidReuseBlocks": 0, "maxResamplePpm": 48, "audioCpuMsPerS": 2.1,
    "tapUpdates": 14, "tapUpdateErrors": 0, "tapRebuilds": 0, "crossfades": 1, "crossfadeAborts": 0,
    "unalignedCrossfades": 0, "zeroRunRebuilds": 0, "lastTapUpdateMs": 0.6, "maxTapUpdateMs": 3.9},
  "events": [{"tNs": 912345600000000, "code": "crossfade_done", "app": "bundle:com.example.app", "detail": "62"}]
}
```

macOS values (everything else as 05 §12.1):

| Field | macOS |
|---|---|
| `engine` | `process-tap` while live; `none` while only watching; `process-tap` during the sound check |
| `state` | `starting` (first generation building), `ok`, `no-device`, `stopped`, and new: `permission-pending`, `permission-denied`, `tap-failed` (mapped to `IM_AUDIO_STATE_PERMISSION`/`FAILED`) |
| `probe` | no probe tone on macOS: `result` is `pending` until the first IO callback of the share's first generation, then `ok`; `skipped` with no output device. Never `failed`/`unsupported`, so Go's Windows banners never fire. `osBuild` = `major·10000 + minor·100 + patch`; `format` `float32`; `levelDb` −120; `ms` = time to the first IO callback |
| `device` | `renderEndpoints`/`captureEndpoints` = output/input devices from `kAudioHardwarePropertyDevices` |
| `selfAudible` (new) | true when isshoni's own playback can't be kept out: no responsible-PID SPI, or isshoni is not its own responsible process at `Init` (§9.7); absent on Windows (false) |
| `tap` (new object) | **always present on macOS**, in every report, whether watching, live, both or neither (so `media.AudioReport.Tap` is non-nil on darwin even with no generation): `rate`/`channels` of the running generation's tap format (0 when none), `aggregate` (`tap-only`, `tap+output`, `""` when none), `bundleIds` (26+ path in use), `responsibleSpi`, `permission` (`unknown`, `pending`, `granted`, `denied`; §13.2), `generations` (0, 1, or 2 during a crossfade) |
| `tap.permission` | a fresh `TCCAccessPreflight` read (§13.2), cached ≤ 1 s, made for every `im_list_audio_apps` call; it never creates a tap, so it never prompts. `unknown` when the SPI is missing. While a generation runs, the watchdog's decision (§13.2) overrides `pending`/`unknown` with `granted` once a non-zero sample proved it. This field is 01's `Permissions.State` source and what 05's `macenv` `tcc:audio` check and the tools' `probe` print |
| `apps[].exe` | the identity's rule value (§9.5) |
| `apps[].playing` | `IsRunningOutput` now or within the last 2 s (it means "has sound output open": an app can hold it while silent) |
| `apps[].streams` | always 0; `verdict` never `not_captured` or `pending` |
| `sessions[]` | one row per process object and flow: `render` when `IsRunningOutput` (state `active`) or the process has output devices (state `inactive`), `capture` when it is a mic user; `created` = process start time in µs since the Unix epoch, decimal string; `stream` `none`, `format` `""`, `note` `""` |
| `counters` new | `tapUpdates`, `tapUpdateErrors`, `tapRebuilds`, `crossfades`, `crossfadeAborts`, `unalignedCrossfades`, `zeroRunRebuilds`, `lastTapUpdateMs`, `maxTapUpdateMs` (0 on Windows). `streams` = live generations; `activations`/`activationFailures` = generation builds; `deviceChanges` counts device-triggered rebuilds |
| `events[].code` new | `tap_built`, `tap_build_failed`, `tap_updated`, `tap_update_failed`, `tap_rebuild` (detail: `device`, `rate`, `format`, `service`, `zero_run`, `update_failed`), `crossfade_start`, `crossfade_done` (detail: ms), `crossfade_aborted`, `xfade_unaligned`, `zero_run_rebuild`, `format_unsupported`, `permission_pending`, `permission_granted`, `permission_denied`; plus M2's `session_created`/`session_expired` (a process object appeared/disappeared), `default_device_changed`, `device_added`, `device_removed`, `no_device`, `rules_changed`, `muted`, `unmuted`, `mode_changed`, `mixer_resync`, `underrun`, `trim`, `reanchor` |
| `warnings[]` new | `possibly_silent_tap` (§7.7) |

Rows: apps with a process object, plus running processes matched by a built-in rule (from
`NSWorkspace.runningApplications` on the engine queue, so "Discord · kept out" shows while Discord is silent), plus the
shared app. NSWorkspace refreshes that list only while the host's **main run loop** runs; the engine relies on the host
to run it: the app (Wails' NSApplication), `isshoni-enginetest` (02 §13.1's `runloop_darwin.m`) and `isshoni-audiotest`
(§16.1) all do. Caps, `seq` and the one-shot snapshot (state `stopped`, kept 1 s) as 05 §12.1.

**The writer** stays M2's single writer, `native/common/audiomix/src/report.cpp`: `ReportEngine` gains
`kProcessTap`, `ReportState` gains `kPermissionPending`, `kPermissionDenied`, `kTapFailed`, the counters struct the
new fields, the report the optional `selfAudible` (written only when the model sets `has_self_audible`) and `tap`
object (written only when `has_tap`), so every M2 golden stays byte for byte identical. New goldens
`internal/media/testdata/audio/report-macos-{normal,crossfade,permission-pending,permission-denied,self-audible,
self-audible-not-responsible,watch,idle}.json` (`idle`: neither watching nor live, `tap` with zeros and a preflight
`permission`) come from
`mix_report_test` and are parsed by Go's tests.

### 12.2 Levels

`im_audio_levels_get` on macOS: `mix_peak_db`/`mix_rms_db` from `MixEngine` while live (what friends hear, after the
silence flags), −120 otherwise; `count = 0` always (no per-app meters). `report_seq` as on Windows.

## 13. Probe, permission, sound check, test exports

### 13.1 `im_probe` audio flags (≤ 50 ms, cached until a device or service event)

`IM_AUDIO_PROCESS_TAP` when `@available(macOS 14.2, *)` and `CATapDescription` is linked; `IM_AUDIO_TAP_BUNDLE_IDS`
when 26+ and `IM_HAVE_SDK26`; `IM_AUDIO_RESPONSIBLE_PID` and `IM_AUDIO_TCC_PREFLIGHT` from the `dlsym` results;
`IM_AUDIO_SESSION_EVENTS` when the process-list listener registers (registered and removed once when not watching);
`IM_AUDIO_MIC_DETECTION` when `kAudioProcessPropertyDevices` is readable on the engine's own object (created by
`kAudioHardwarePropertyTranslatePIDToProcessObject`, which returns `kAudioObjectUnknown` until the process has used
audio: then the bit is set when at least one input device exists).

### 13.2 Permission ("System Audio Recording") and the 30 s watchdog

- **Preflight** (`tcc.mm`): `TCCAccessPreflight(CFSTR("kTCCServiceAudioCapture"), NULL)` from
  `/System/Library/PrivateFrameworks/TCC.framework/Versions/A/TCC` via `dlopen`/`dlsym`, read-only, never
  `TCCAccessRequest`. Result 0 → `granted`, 1 → `denied`, 2 → `pending` (not determined), missing SPI or other values
  → `unknown`. It is read for **every** `im_list_audio_apps` report (cached ≤ 1 s; `tap.permission`, §12.1), whether
  watching, live or neither, at every generation build, and every 5 s while a generation exists and the state is not
  `granted`. A preflight read creates no tap and never prompts, so 01's Settings → Permissions, its
  `permissions.changed` re-read on key-window and the `isshoni_hw` test skips can all read the state through
  `media.AudioReport.Tap.Permission` without side effects.
- **Idle reads (the interface 01 §9.2 relies on)**: Go may call `eng.AudioReport` (`im_list_audio_apps`) while idle,
  with no watcher and no share, at most once per key-window event and per `Permissions.State()` call. Such a call
  answers within 1 s, carries `tap.permission` from this preflight (the ≤ 1 s cache), and builds no tap, aggregate or
  generation; the process snapshot it lists from is the cached one (§9.6), never a new HAL listener.
- **The prompt** appears when the first tap is created or started in a process that has never been asked; macOS
  attributes it to the responsible app (isshoni, or the harness app, `05-exit-and-verification.md` §4.1). The tap
  delivers zeros until the user answers. 01's wizard runs the sound check (§13.3) as its permission step, so the prompt
  appears there, not at the first share (plan).
- **Watchdog**: after a generation starts, the permission state is decided by the first of:
  - a non-zero sample from the tap, or preflight `granted` → `granted` (event `permission_granted`);
  - preflight `denied` → `denied` (event `permission_denied`);
  - 30 s since the first generation started with preflight **`unknown`** and no non-zero sample while some heard
    process had output running → `denied` (event `permission_denied`, detail `watchdog`). With nothing playing for
    30 s and preflight `unknown`, the state stays `unknown` (silence is not proof);
  - 30 s with preflight **`pending`** (the macOS dialog unanswered or hidden behind a window) → the state **stays
    `pending`**: report state `permission-pending`, event `permission_pending` with detail `watchdog`. Only the user's
    answer (preflight `granted`/`denied`, or a non-zero sample) leaves it. So 01's wizard (§9.3) can tell "macOS hasn't
    shown or answered its dialog" from a real denial.
  While undecided and preflight says `pending`, the report state is `permission-pending` (`IM_AUDIO_STATE_PERMISSION`)
  and the event `permission_pending` is raised once when it starts (no detail) and once at the 30 s mark (detail
  `watchdog`).
- **Denied**: state `permission-denied`, the mixer feeds silence (Opus frames keep flowing, so the share and A/V sync
  are intact), the generation is kept (cheap) and preflight polled every 5 s; when it turns `granted`, the engine
  rebuilds and the state returns to `ok`. Manual check M-R8 verifies whether a grant given in System Settings takes
  effect without restarting isshoni on 14.4 and 27; if it doesn't on a version, the banner's second sentence
  (`desktop.audio.banner.audioPermissionRestart`, §14.5) tells the user to quit and reopen isshoni. During a share
  that banner shows only this text, **no button**: 01's `App.Relaunch()` (label "Quit and reopen isshoni") answers
  `busy` while live. The button lives in 01's wizard and Settings → Permissions (01 §9.3, §9.7).
- The watchdog's 30 s is also the sound check's extra wait (§13.3), except with preflight `unknown`, where the sound
  check doesn't wait and its chime decides.

### 13.3 Sound check (`im_audio_sound_check`)

On macOS Go always passes `helper_pid = 0` (`SelfInfo.Exe` is `""` on darwin, as `cmd/isshoni-desktop`'s
`soundCheckExe` already returns), and the engine plays the chime itself. A nonzero `helper_pid` is accepted and treated
the same way, except that the helper's process object is the one kept in. Steps:
1. Create the chime's output `AudioQueue` (48 kHz stereo float; 05 §11.3's chime: 880 Hz + 1320 Hz, −14 dBFS, 800 ms,
   10 ms ramps) without starting it: this creates isshoni's own process object.
2. Build a sound-check generation: a **global exclude tap** with the current plan's objects (built-ins, user rules,
   mic users, and isshoni's other processes such as its WebKit GPU process) minus isshoni's own main process object,
   and on 26+ the plan's `bundleIDs` without the SELF values (isshoni's own bundle ID would keep the chime out).
   This exercises the same tap kind a share uses, which a per-process tap wouldn't.
3. Wait for the permission decision (§13.2), at most 30 s, **not** counted in `timeout_ms`. **Except** when the
   preflight is `unknown` (the SPI is absent, so the probe bit `IM_AUDIO_TCC_PREFLIGHT` is off, or it returns an
   unexpected value): then step 3 is skipped, because the only `granted` proof left is a non-zero sample and the chime
   is what makes one; without the skip every sound check would wait the full 30 s.
4. Start the chime 100 ms after the first IO callback (with preflight `unknown` too: a captured chime sets the
   permission state to `granted`, §13.2; silence until `timeout_ms` gives the existing reasons, `silent` or
   `timeout`, with `"permission":"unknown"`); capture until both partials reach −40 dBFS (Goertzel, sliding
   100 ms windows, hop 20 ms, as 05 §11.3's as-built) or `timeout_ms` passes; destroy the generation.
5. Result JSON (05 §11.3's, additive `permission`):
   `{"captured":true,"levelDb":-14.6,"ms":1320,"backend":"process-tap","permission":"granted"}`, or
   `{"captured":false,"reason":"permission_denied|permission_pending|silent|tap_failed|no_device|timeout",
   "backend":"process-tap","permission":"denied"}`. `permission_pending` is the result when the 30 s wait ends with
   preflight still `pending` (the dialog unanswered):
   `{"captured":false,"reason":"permission_pending","backend":"process-tap","permission":"pending"}`; 01's wizard
   shows its "macOS hasn't shown or answered its dialog… [Try again]" for it, and its "macOS blocks isshoni…" only
   for `permission_denied`. The call waits at most 30 s + `timeout_ms` + 500 ms; Go's service keeps
   `soundCheckTimeout` at 3 s and gives the call a 35 s context on darwin (§14.2).

### 13.4 Error mapping (every failure, where it goes)

| Failure | ABI result | Report | UI (§14.5) |
|---|---|---|---|
| Engine queue doesn't answer in 3 s | `IM_ERR_TIMEOUT` (`CodeTimeout`) | — | the caller's error text |
| Invalid rules, unknown mode, `EVERYTHING` | `IM_ERR_INVALID_ARG` (`CodeInvalid`) | — | programming error |
| Levels/mode/mute without watch or share | `IM_ERR_BAD_STATE` (`ErrAudioNotRunning`) | — | — |
| Second share with audio | `im_start` → `IM_ERR_LIMIT` | — | — |
| Tap or aggregate can't be built/started | `IM_OK` | `state: tap-failed`, `tap_build_failed` | banner `tap-failed` |
| Permission not answered yet (also after the 30 s watchdog when preflight says `pending`) | `IM_OK`; sound check JSON `reason: permission_pending` | `state: permission-pending`, `permission_pending` (detail `watchdog` at 30 s) | banner `audio-permission` (cause `pending`) |
| Permission denied (preflight `denied`, or watchdog with preflight `unknown`) | `IM_OK`; sound check JSON `reason: permission_denied` | `state: permission-denied` | banner `audio-permission` (cause `denied`) with "Open System Settings" (`audio-recording`) |
| No output device | `IM_OK` | `state: no-device` | `no-output-device` |
| Device/rate/format change, service restart | `IM_OK` | `rebuilding`, `tap_rebuild` | `reconnecting-sound` after 1 s |
| Live update failed | `IM_OK` | `tap_update_failed`, then a crossfade | — |
| Zero run | `IM_OK` | `zero_run_rebuild`, maybe `possibly_silent_tap` | `capture-trouble` only with a failed sound check (§7.7) |
| Responsible SPI missing, or isshoni not its own responsible process (§9.7) | `IM_OK` | `selfAudible: true` | `viewer-muted`, viewer muted while live (01's `mutesViewer`) |
| macOS < 14.2 (defensive; the app doesn't share below 14.4) | `IM_OK` | `state: tap-failed`, event `tap_build_failed` detail `unsupported_os` | banner `tap-failed` |

The plan's `PERMISSION_DENIED` and `AUDIO_DEVICE_CHANGED` thus surface as report states and events, never as return
codes, on macOS as on Windows (05 §4.3).

### 13.5 Test exports on macOS (shipped, capture nothing)

- `im_test_render_tone(endpoint_id, hz, amp, ms)`: plays a tone through an AUHAL output unit
  (`kAudioUnitSubType_HALOutput`, `kAudioOutputUnitProperty_CurrentDevice`), `endpoint_id` = a Core Audio device UID
  (UTF-8), NULL/"" = the default output device.
- `im_test_render_bursts(endpoint_id, 2000, amp, 5, 1000, n, onset_ns, cap)`: the same unit renders 5 ms bursts; each
  onset is predicted from the render callback's `AudioTimeStamp`: `hostNs(mHostTime) + (frame offset in buffer) ·
  1e9 / rate` (the time the HAL presents that frame, independent of the tap and the mixer), engine clock.
- `im_test_opus_decode`: 04's, unchanged.
These run in the calling process, so the audiotest calls them from a fixture process (`bursts` verb) when the sound
must not count as isshoni's (§16.3).

## 14. Go side

### 14.1 `internal/desktop/audio`

- **`SelfInfo`** gains two fields (zero values keep M2 exactly):
  ```go
  type SelfInfo struct {
  	// ... M2's PID, WebViewDataDir, Exe, NoDucking ...
  	// RulesOS is the OS whose rule values the service builds: "windows" or "darwin" (GOOS values); "" = "windows"
  	// (M2's behavior: the fake engine scripts a Windows PC). cmd/isshoni-desktop sets runtime.GOOS on darwin.
  	RulesOS string
  	// TestData adds test-only built-ins, launchers and browsers (cmd/isshoni-audiotest only; nil in the app).
  	TestData *TestData
  }
  type TestData struct {
  	VoiceApps []audiorules.VoiceApp // locked, source BUILTIN, like the real list
  	Launchers []string              // values for RulesOS
  	Browsers  []audiorules.Browser
  }
  ```
- **`ids.go`**: the constant `rulesOS` becomes the Service field `s.rulesOS`. Per OS:

  | | windows (M2) | darwin (M3) |
  |---|---|---|
  | user app id | `exe:<name>.exe` (M2's `validAppID`) | `bundle:<bundle id>` (`[a-z0-9-]+(\.[a-z0-9-]+)+`, total ≤ 63) or `exe:<name>` (`[a-z0-9._-]{1,59}`) |
  | rule value of an app id | `exe:` stripped | `bundle:` stripped; `exe:` ids are their own value |
  | self rules | instance pid, app `isshoni.exe`, WebView2 data dir | instance pid only. **No Go-side self app rule**: the engine's implicit, exact-match `IM_RULE_APP_EXCLUDE(mainBundle.bundleIdentifier)` (§4.2) always equals the running bundle (`io.isshoni.desktop`, `.dev`, `.e2e`, or a harness), which a Go constant couldn't |
  | `long:` ids (§9.5) | — | valid in prompts and `Decide` for the run; `Decide` doesn't save them (`appRules` unchanged), like `DismissPrompt` |

- **`rules.go`** (`buildExclusions`): data rows by `s.rulesOS` (`Launchers().For`, `SharedRuntimes().For` (empty on
  darwin), `Browsers()`), plus `TestData`; no `RuleWebViewDataDir` and no self app rule on darwin. ≤ 512 rules still
  holds (darwin: 1 self from Go plus the engine's 2 implicit ones, 22 + 0 + 20 data, 15 built-in, ≤ 256 decisions,
  ≤ 20 viewer).
- **`viewer.go`** (05 §5.9 step 3): "`os == "windows"`" becomes "`os` is the ClientOS of `s.rulesOS`" (`windows`, or
  `macos` for darwin). On darwin the families `chrome`, `edge`, `firefox` and `safari` add rules for every bundle ID of
  the family whose app has a process object in the latest report; `other` and unknown values show the
  `viewer-unknown-browser` banner while that connID plays. Windows unchanged (`safari` has no Windows names: banner).
- **`friends.go`**: banners (05 §14.7) gain

  | Code | Shown when | Level |
  |---|---|---|
  | `audio-permission` | state `permission-pending` (`params.cause = "pending"`) or `permission-denied` (`"denied"`) | warning |
  | `tap-failed` | state `tap-failed` | warning |
  | `viewer-muted` | engine `endpoint` while live (M2), **or** `selfAudible` while live | info |
  | `capture-trouble` (no `params.app`) | warning `possibly_silent_tap` and the sound check failed in this app run | warning |

  `FriendsHear` gains `selfAudible bool` (copied from the report) and the engine value `process-tap` passes through.
  `selfAudible` drives the `viewer-muted` banner here; muting the viewer itself is 01 §10.2's `mutesViewer`, which
  reads `FriendsHear.selfAudible`.
  `alert.voiceAppsHeard` keeps M2's rule (error banners only); none of the new banners is error-level, because voice
  apps are never heard on macOS when capture fails (it fails closed, to silence).
- **`PermissionState`** (01 §9.2's `audio.Service.PermissionState(ctx)`): one `eng.AudioReport` call bounded at
  1 s that may run while idle (no watcher, no share) and builds no tap (§13.2's idle reads); the result updates the
  cached report, and an error falls back to the cache.
- **`soundcheck.go`**: on darwin the engine call gets `context.WithTimeout(ctx, 35*time.Second)` (§13.3); Windows
  unchanged. `RunSoundCheckHelper` keeps `helper_other.go`'s darwin stub (never called: `Exe` is `""`).

### 14.2 `internal/media/audio.go` (additive)

```go
type AudioReport struct {
	// ... M2's fields ...
	SelfAudible bool      `json:"selfAudible"` // macOS: isshoni's own playback can't be kept out (§9.7)
	// Tap is non-nil in every darwin report, even with no generation (Rate/Channels 0, Aggregate ""); nil on Windows.
	// Tap.Permission is 01's Permissions.State source for System Audio Recording (§12.1, §13.2).
	Tap *AudioTap `json:"tap,omitempty"`
}
type AudioTap struct {
	Rate           int    `json:"rate"`
	Channels       int    `json:"channels"`
	Aggregate      string `json:"aggregate"`  // "tap-only" | "tap+output" | ""
	BundleIDs      bool   `json:"bundleIds"`
	ResponsibleSPI bool   `json:"responsibleSpi"`
	Permission     string `json:"permission"` // "unknown" | "pending" | "granted" | "denied"
	Generations    int    `json:"generations"`
}
// AudioCounters gains TapUpdates, TapUpdateErrors, TapRebuilds, Crossfades, CrossfadeAborts, UnalignedCrossfades,
// ZeroRunRebuilds (int) and LastTapUpdateMs, MaxTapUpdateMs (float64), json names as §12.1.
// SoundCheckResult gains Permission string `json:"permission,omitempty"` ("unknown"|"pending"|"granted"|"denied");
// Backend may be "process-tap"; Reason's documented values gain "permission_denied" and "permission_pending" (§13.3).
// Event codes and state strings of §12.1 get constants beside M2's (media.AudioEvent*, media.AudioState*).
```

`ParseAudioReport` ignores unknown fields already; the goldens of §12.1 are parsed in `audio_golden_test.go`.

**`internal/media/probe.go`** (M2's `AudioCapsFromFlags` ignores bits above 0x8, so on macOS `Probe.Audio.Backend`
and with it `ShareCaps.audioBackend` (`bridge/share.go`) would be `"none"`):

```go
type AudioCaps struct {
	// ... M2's ProcessLoopback, EndpointLoopback, SessionEvents, MicDetection ...
	ProcessTap     bool // IM_AUDIO_PROCESS_TAP (0x10), macOS
	TapBundleIDs   bool // IM_AUDIO_TAP_BUNDLE_IDS (0x20), macOS 26+
	ResponsiblePID bool // IM_AUDIO_RESPONSIBLE_PID (0x40), macOS
	TCCPreflight   bool // IM_AUDIO_TCC_PREFLIGHT (0x80), macOS; the SPI only, not the grant (§13.2)
	// Backend: "process-tap" when ProcessTap (checked first); else M2's order unchanged.
	Backend string
}
const (
	audioFlagProcessTap     = 0x10
	audioFlagTapBundleIDs   = 0x20
	audioFlagResponsiblePID = 0x40
	audioFlagTCCPreflight   = 0x80
	audioBackendProcessTap  = "process-tap"
)
```

`AudioCapsFromFlags`' doc comment changes to "bits above 0x80 are ignored". Windows never sets 0x10–0x80, so its
values stay byte for byte M2's. `probe_test.go` gains rows: `0x10|0x4|0x8` → Backend `process-tap` with the macOS
fields; `0xF0|0x1` → `process-tap` (0x10 is checked first); every M2 row unchanged.

**`internal/media/native/audio.go`**: the `Config.Audio.TestFlags` check (M2: only `IM_AUDIOF_TEST_FAIL_PROBE`, lines
186–188) accepts `imAudiofTestFailProbe | imAudiofTestNoExclusions | imAudiofTestNoBundleIDs` (new constants
`0x40000000`, `0x20000000`, §4.1); its error text names the three. Windows' DLL ignores the two new bits (§4).
`audio_test.go` gains a table row per new bit (accepted, passed into `im_audio_config.flags`) and one for an unknown
bit (still refused).

### 14.3 The binding and the fake

- `internal/media/native/audio.go` (untagged) is reused over 02's darwin caller with one change, the `TestFlags`
  check of §14.2: rule marshalling, the status table, mode mapping and the snprintf helper don't depend on the OS. The
  darwin `AudioTestHooks` (render tone/bursts, opus decode, engine clock) are 02's caller exposing the same exports.
  `Probe.Audio` on darwin comes from `media.AudioCapsFromFlags` with §14.2's bits (`02-macos-engine.md` §11.4 delegates
  "Audio from audio_flags" here).
- `internal/media/fake/audio_ctl.go` gains scripted macOS scenarios `mac-basic`, `mac-crossfade`,
  `mac-permission-pending`, `mac-permission-denied`, `mac-self-audible` (reports with engine `process-tap` and a `tap`
  object), selectable with M2's existing scenario selection; their probe sets `audio_flags` `0x10|0x4|0x8` (plus
  `0x40|0x80`). There is no audio-specific e2ehost flag: 01 §19.3's `e2ehost -os macos` also sets the audio Service's
  `SelfInfo.RulesOS = "darwin"`, so the SPA's macOS share sheet is tested on CI ubuntu with one consistent OS.

### 14.4 The SPA (`web/src`)

- `platform/desktop/bridge.ts`: `FriendsHear.engine` adds `'process-tap'`; `FriendsHear.selfAudible?: boolean`;
  `ShareCaps.audioBackend` adds `'process-tap'`; `Banner.code` adds `'audio-permission' | 'tap-failed'`.
- `desktop/AboutSettings.tsx`: `BACKEND_KEYS` gains `'process-tap': 'desktop.settings.about.audioBackend.processTap'`
  (§14.5), so About shows the macOS backend instead of failing the record type.
- `viewer/endpointMute.ts` is **not** changed here: 01 §10.2 owns `mutesViewer` (its predicate covers
  `selfAudible` and 01's `selfVisible`). This doc provides `FriendsHear.selfAudible` (bridge and Go) and the
  `viewer-muted` banner on it.
- `share/native/FriendsHear.tsx`: with `engine === 'process-tap'` the rows show no meter; a heard or kept-out row with
  `playing` shows a dot labelled `desktop.audio.row.outputActive`; the mix meter stays. Windows rendering unchanged
  (its engines keep the per-row meters).
- `share/native/audioText.ts` and `i18n/lazy/desktop.en.json`: §14.5's keys. The `audio-permission` banner (cause
  `denied`) has a button that calls `App.OpenOSSettings('audio-recording')` (01's `OSPage`; 01 §9.4's table owns the
  URLs and fallbacks). Its pane instruction is 01's version-aware `desktop.permissions.audioPane` text (a 14.x
  variant chosen from `AppInfo.osVersion`), with a link to `/troubleshooting#desktop-macos-permissions` (01 §20).
  During a live share it shows `audioPermissionRestart` as text only (no relaunch button, §13.2).
- `VolumeNote` shows on macOS only once M-R10 confirms the tap is pre-volume (§6.3); until then it is hidden on
  `process-tap`.

### 14.5 English text (namespace `desktop`)

| Key | Text |
|---|---|
| `desktop.audio.banner.audioPermissionPending` | Allow isshoni to record system audio so friends hear this Mac. Look for the macOS prompt. |
| `desktop.audio.banner.audioPermissionDenied` | isshoni can't record this Mac's sound, so friends hear nothing. |
| (pane instruction) | not a key of this doc: 01's version-aware `desktop.permissions.audioPane` (15+ "Screen & System Audio Recording → System Audio Recording Only", 14.x "System Audio Recording"), shown after the sentence above |
| `desktop.settings.about.audioBackend.processTap` | Process tap (all apps except kept-out ones) |
| `desktop.audio.banner.audioPermissionRestart` | If it still doesn't work, quit and reopen isshoni. |
| `desktop.audio.banner.openSettings` | Open System Settings |
| `desktop.audio.banner.tapFailed` | isshoni can't capture this Mac's sound right now, so friends hear nothing. isshoni keeps trying. |
| `desktop.audio.banner.captureTroubleUnknown` | Friends may not hear this Mac's sound. Run the sound check in Settings → Audio. |
| `desktop.audio.row.outputActive` | Sound output open |
| `desktop.audio.soundCheck.permissionDenied` | isshoni isn't allowed to record system audio, so friends would hear nothing from this Mac. |
| `desktop.audio.soundCheck.permissionPending` | macOS hasn't shown or answered its dialog yet. Look for it behind other windows, then try again. |
| `desktop.audio.sheet.voiceAppsMac` | Friends won't hear: {{apps}}. Apps that use your microphone are kept out too; you'll be asked. |

`NSAudioCaptureUsageDescription`: 04 §4.1's Info.plist template (`cmd/isshoni-desktop/build/darwin/Info.plist.tmpl`)
carries it, with text owned by `01-desktop-macos.md` (proposed: "isshoni shares your Mac's sound with friends, without
your voice apps."), and no `NSMicrophoneUsageDescription`. Any further Info.plist key goes through 04's template and
its golden (strict key check).

## 15. Privacy, performance and limits

- Privacy as 05 §16: bundle IDs, executable names, display names, paths, pids, rules, prompts and levels never leave the
  device and are never logged at any level; `tap_engine.mm` writes only codes and counts into 02's log ring (event
  codes, OSStatus values, counters). The report's `events[].app` carries app ids (local only, like Windows).
- Budgets:

| Item | Budget |
|---|---|
| Live update after a process-list change (listener fired → `AudioObjectSetPropertyData` returned) | ≤ 10 ms p95 (S2: ≈ 4 ms); `maxTapUpdateMs` recorded per case |
| Generation build | ≤ 50 ms (S2: 16–26 ms) |
| Crossfade complete (old generation released) after the change reaches the engine | ≤ 200 ms (S2: 63–137 ms) |
| Audible cut of a newly kept-out playing app (case 15's measured cut − T_set) | ≤ 150 ms |
| New app heard | ≤ 100 ms after its object appears (nothing to activate; record in cases 8 and 25; budget kept at M2's 500 ms for the verdict) |
| Rebuild gap on a device or rate change | ≤ 1 s of silence (then `reconnecting-sound`) |
| Audio CPU (IO block + mixer + Opus 256 k, 1 generation) | ≤ 2 % of one core on an M1 (`audioCpuMsPerS` ≤ 20) |
| Audio memory | ≤ 12 MiB (2 generations: 2 rings of 500 ms, 2 resamplers, caches) |
| Report | ≤ 256 KiB (05's caps) |
| `im_audio_levels_get` | < 50 µs, lock-free |
| Drift | as 05 §17 (±500 ppm, fill 40 ± 15 ms, zero underruns and trims in 2 h) |

## 16. The audiotest harness on macOS (the M3 exit gate)

### 16.1 Layout and packaging

```
cmd/isshoni-audiotest/
  driver_darwin.go      //go:build darwin && cgo && imstatic. The darwin driver: engine via internal/media/native
                        (02's static archive), audio.Service with RulesOS "darwin" and TestData, fixtures through
                        LaunchServices
  main_darwin.go        //go:build darwin && cgo && imstatic. Locks the main goroutine to the main thread
                        (runtime.LockOSThread in init), runs the tool on another goroutine and NSApp on the main one
  runloop_darwin.m      the NSApplication run loop (02 §13.1's enginetest runloop_darwin.m pattern, copied; or a
                        shared internal/desktop/macrun package if 01 needs the same)
  fixture_darwin.go     //go:build darwin && cgo && imstatic. Fixture verbs on macOS (AudioQueue tones, mic hold,
                        posix_spawn, WKWebView tone); its cgo preamble has
                        #cgo LDFLAGS: -framework AVFoundation -framework WebKit (harness-only frameworks)
  fixture_darwin.m      the AudioQueue/WKWebView helpers (from S2's testapps.m), via cgo
  casesmac.go           the macOS case table (portable data, no build tag; tested on every OS)
  fixtureplan_mac.go    argv and folder layout of the macOS fixtures (portable)
  macapp/               the fixtures' Info.plist templates and entitlements only (no harness bundle here), consumed
                        by 05's tools/macharness, which writes unsigned output
  driver_other.go       now //go:build !windows && !(darwin && cgo && imstatic)
```

The constraints are `02-macos-engine.md`'s pattern for the enginetest `*_other.go` files: `internal/media/native`'s
darwin engine exists only with `-tags imstatic` and a built archive (02 §6.4), so untagged `go test ./...` on a Mac,
`desktop-macos` and lint build the stub driver. Every darwin build of the tool passes `-tags imstatic`,
`CGO_ENABLED=1`, `MACOSX_DEPLOYMENT_TARGET=14.4` and `-ldflags "-X
github.com/MoonWX/isshoni/internal/media/native.staticArchiveSHA=$(cat native/dist/<dir>/archive.sha256) -X
main.engineArchiveSHA256=<same hex> -X main.engineVersion=<build.json version>"` (§16.7); the amd64 half of a
universal build uses `GOARCH=amd64 CC='clang -arch x86_64'` and the `darwin-amd64` archive, then `lipo -create`.

**The harness is 05's.** `isshoni-audiotest` is one Mach-O inside `05-exit-and-verification.md` §4.1's
`isshoni Dev Harness.app` (`io.isshoni.dev.harness`, dev identity, `task mac:harness`) or `isshoni Exit Harness.app`
(`io.isshoni.exitharness`, test-tools identity `isshoni-testtools-1`, signed by `release.yml`'s `macos-sign-tools`
job and packed by `macos-exit-tools`), in `Contents/MacOS`. Each harness
has its own bundle ID, so the dev and release builds never overwrite each other's System Audio Recording grant, and
each keeps its grant across rebuilds with the same identity (S2: builds 1→9, no second prompt). Signing is always
04's `sign.sh` (`-mode dev` locally, `-mode release` in `macos-sign`); no Go tool signs.

Running: every capture run goes through 05's `harness.sh isshoni-audiotest <args>` (e.g. `harness.sh
isshoni-audiotest run -gate -out .`), so TCC attributes capture to the harness, not the terminal (S2's
`run-selftest.sh`), the exit code comes back through 05's status file (70 when there is none) and `record`'s
`Pass? [y/n]` reads the terminal. `selfcheck`, `probe`, `apps` work from a terminal too (no capture).

The darwin `main` runs an NSApplication (`activationPolicy` accessory) on the main thread for the whole run:
`NSWorkspace.runningApplications` (§12.1) is refreshed only while the main run loop runs, and the `webview` fixture
verb needs the main thread.

### 16.2 Subcommands

05 §18.2's set, unchanged in syntax: `selfcheck`, `probe`, `apps`, `run [-case] [-gate] [-long] [-keep-wav] [-out]
[-no-redact]`, `record -label … [-d] [-o] [-exclude bundle,…]`, `merge`, `fixture <verb>`. On macOS:
- `selfcheck`: engine version and every export, `im_probe` (prints the audio flags), watcher start/stop, the analyzer
  on synthetic signals. No sound, no TCC. Runs in 02's `macos-native` CI job (`macos-26`; 05's matrix adds
  `macos-15` and `macos-15-intel`) and in 05's `macos-exit-tools` job.
- `probe`: also the System Audio Recording state, read as `tap.permission` from an `im_audio_watch` +
  `im_list_audio_apps` report (§12.1; never a tap, never a prompt), the default output device's transport and nominal
  rate, input devices (redacted to `{transport}`). `isshoni-enginetest probe` prints the same field.
- `-exclude` takes bundle IDs or `exe:` names.

### 16.3 Fixtures

**Fixtures live beside the harness bundle, never inside it**: `<dir of the harness .app>/fixtures/*.app`,
`fixtures/steamapps/TestGame.app` and `fixtures/bin/voice-cli-helper` (in `.dev/mac-harness/` for the dev harness,
in `isshoni-exit-tools-macos/` for the exit harness; 05 §4.1/§4.5 lay out the sibling `fixtures/` and `avsync/`
folders the same way). A bundle nested in the harness would get the harness's app identity (§9.3: the outermost
enclosing `.app`) and match the implicit SELF rule (§4.2), so every fixture would be kept out as `isshoni` and cases
1–3 would fail (S2 built its fixtures in a sibling `dist/fixtures/` for the same reason). The darwin driver finds them
as `filepath.Join(filepath.Dir(bundlePath), "fixtures")` where `bundlePath` is `[NSBundle mainBundle].bundlePath`.
Each fixture is signed with the harness's identity, inner bundles first, by 04's `sign.sh`. A `fixtureplan_mac.go`
test asserts that no fixture path has an ancestor ending in `.app` other than its own outer bundle (and that
`bin/voice-cli-helper` has none).

Every fixture is a bundle in `fixtures/`, started through LaunchServices with
`NSWorkspace openApplicationAtURL:configuration:` (`createsNewApplicationInstance = YES`, `activates = NO`,
`addsToRecentItems = NO`, `arguments = ["fixture", verb, …, "-role", R, "-out", <file>]`) so each is its own
responsible process, like an app opened from the Dock (the macOS analogue of 05's explorer parenting). Fixtures write
05's JSON lines (`{"role","pid","startNs"}`, `{"role","firstRenderNs"}`) to their `-out` file, which the harness
tails; `firstRenderNs` is `mach_absolute_time` in ns right before `AudioQueueStart`.

**Test bundle IDs only.** No fixture uses a real app's bundle ID (`com.hnc.discord`): LaunchServices would register a
second app with that ID, `open -b` could start the fixture instead of the real app, and a microphone request would make
TCC replace the real app's stored code requirement, so the user's real Discord would be asked again. The built-in,
launcher and browser paths are exercised through `SelfInfo.TestData` with test IDs, matched by exactly the code that
matches the real list; the real IDs are checked by M-R1/M-R6/M-R7 and the `audiorules` tests.

Signing columns: every fixture is signed with the hardened runtime (`--options runtime`). A fixture that opens an
input gets the entitlement `com.apple.security.device.audio-input` and `NSMicrophoneUsageDescription` = "isshoni test
fixture: holds the microphone so isshoni can detect it." (S2's FakeVoIP had both); all others get no entitlements and
no usage string.

| Bundle (in `fixtures/`) | Bundle ID | TestData role | Entitlements | `NSMicrophoneUsageDescription` |
|---|---|---|---|---|
| `FakeDiscord.app` | `com.isshoni.test.discord` | built-in voice app (app id `testdiscord`) | `audio-input` | yes (case 23: idle with mic) |
| `FakeDiscord.app/Contents/Frameworks/FakeDiscord Helper.app` | `com.isshoni.test.discord.helper` | its Electron-style helper | — | — |
| `FakeDiscord.app/Contents/Frameworks/FakeDiscord Helper (Renderer).app` | `com.isshoni.test.discord.helper.renderer` | late helper (case 7) | — | — |
| `bin/voice-cli-helper` (no bundle, outside every `.app`) | — (identifier `io.isshoni.test.voice-cli-helper` in its signature only) | helper matched only by responsible PID | — | — |
| `FakeTS.app` | `com.isshoni.test.teamspeak` | second built-in (app id `testteamspeak`) | — | — |
| `FakeVoice.app` | `com.isshoni.test.fakevoice` | the plan's fake voice app, for user rules | — | — |
| `FakeKOOK.app` | `com.isshoni.test.kook` | unknown mic user (case 13 `-mic`, case 13v `-mic-vpio`) | `audio-input` | yes |
| `FakeSteam.app` | `com.isshoni.test.steam` | launcher (holds the mic) | `audio-input` | yes |
| `steamapps/TestGame.app` (a **sibling** of `FakeSteam.app`, like real Steam games under `~/Library/Application Support/Steam/steamapps/common`) | `com.isshoni.test.steamgame` | game that FakeSteam `posix_spawn`s: its responsible process stays FakeSteam (a launcher, so no identity from it) and its identity is its own bundle (§9.11's Steam row) | — | — |
| `FakeChrome.app` (+ `FakeChrome Helper.app`, `FakeChrome Helper (Renderer).app` inside) | `com.isshoni.test.chrome` (+ `.helper`, `.helper.renderer`) | browser, family `chrome` | `FakeChrome.app` and `FakeChrome Helper.app`: `audio-input` (case 12's `Helper -tone 440 -mic`); Renderer: — | `FakeChrome.app`, `FakeChrome Helper.app`: yes |
| `FakeGame.app`, `FakeGame2.app` | `com.isshoni.test.game`, `com.isshoni.test.game2` | games | `FakeGame.app`: `audio-input` (case 14); `FakeGame2.app`: — | `FakeGame.app`: yes |
| `FakeGame.app/Contents/MacOS/game-child` | (inside FakeGame.app) | late child in the same app (case 25) | — | — |
| `FakeLauncherX.app` | `com.isshoni.test.launcherx` | an ordinary app that spawns FakeDiscord's executable (case 17) | — | — |
| `FakeBrowser.app` | `com.isshoni.test.browser` | an app FakeDiscord opens through LaunchServices (case 16) | — | — |
| `Bursts.app` | `com.isshoni.test.bursts` | case 20's renderer | — | — |

Verbs (05 §18.3's, macOS implementation): `tone -f HZ[,HZ…] -d SEC [-amp A] [-mic]` (AudioQueue, 48 kHz float stereo,
S2's generator with 20 ms fades, −18 dBFS default; `-mic` also runs an `AudioQueueNewInput` on the default input;
`-mic-vpio` instead plays the tone through an `AUVoiceProcessingIO` unit (`kAudioUnitType_Output`,
`kAudioUnitSubType_VoiceProcessingIO`) with input enabled (`kAudioOutputUnitProperty_EnableIO` = 1 on input bus 1, the
input pulled by `AudioUnitRender` in the input callback and discarded), like a voice client's call path; it sets
`kAUVoiceIOProperty_OtherAudioDuckingConfiguration` (14.0+) to `{mEnableAdvancedDucking = false, mDuckingLevel =
kAUVoiceIOOtherAudioDuckingLevelMin}` so the other fixtures' levels stay measurable, and the case also checks them);
`spawn [-at SEC] [-tone HZ] [-mic] [-d SEC] [-orphan-after SEC] [-ls] PATH ARGS…` (`posix_spawn` of `PATH`, so the
child's responsible process is this fixture; `-ls` instead opens the bundle at `PATH` through LaunchServices, so it is
its own responsible process); `idle -d SEC [-mic]`; `webview -f HZ -d SEC` (a WKWebView in an off-screen borderless
window plays the tone with WebAudio; main thread; S2's `im_test_webview_tone`); `bursts -n N` (`im_test_render_bursts`
in the fixture's own process, printing the onsets); `endpoint-tone -endpoint UID -f HZ -d SEC`.

Frequencies: 05 §18.3's (game 1000, voice 440, second voice 571, late game 683, browser tab 797, isshoni self S =
1700, bursts 2000), all −18 dBFS.

### 16.4 Preconditions (checked, recorded)

- A default output device (else exit 3, every case SKIP).
- **System Audio Recording granted to the harness**: the harness plays a 200 ms −40 dBFS 997 Hz tone itself during a
  short session with `IM_AUDIOF_TEST_NO_EXCLUSIONS` and must capture it (≥ −46 dBFS). If not: exit 4, "grant System
  Audio Recording to isshoni Exit Harness (or isshoni Dev Harness) (System Settings → Privacy & Security → Screen &
  System Audio Recording → System Audio Recording Only; on 14.x Privacy & Security → System Audio Recording), then run
  again". On the very first run this is where macOS asks; the harness waits for the answer (30 s watchdog; a still
  `pending` preflight after it is reported as such, §13.2).
- **Silence pre-check** before every case: 1 s with `IM_AUDIOF_TEST_NO_EXCLUSIONS`; every case frequency ≤ −70 dBFS and
  RMS ≤ −60 dBFS, else INCONCLUSIVE ("other apps are playing").
- **Microphone** for the mic cases (9, 12, 13, 13v, 14) and the soak (23): an input device exists and every bundle that
  opens an input has Microphone access: `FakeKOOK.app`, `FakeSteam.app`, `FakeGame.app`, `FakeDiscord.app`,
  `FakeChrome.app` and `FakeChrome.app/Contents/Frameworks/FakeChrome Helper.app` (the six bundles with
  `audio-input` in §16.3). The harness checks with `AVCaptureDevice authorizationStatusForMediaType:` run inside each
  of them (`fixture mic-status`); a bundle that is not determined is started once with `-mic` so macOS asks (the
  README tells the owner to click Allow once per bundle on a new machine: up to six times, fewer where TCC attributes a
  helper's request to its responsible app); without access the mic cases SKIP, so a `-gate` run exits 3.
- Recorded, not enforced (§16.7's fields): macOS version, build and build id (`sw_vers`), `kern.hv_vmm_present`,
  model family (`hw.model` reduced to its family, e.g. `MacBookPro`, `VirtualMac`), CPU (`arm64`, `x86_64`, or
  `x86_64 under Rosetta`), output device `{transport, rate}`, input present, the output volume and mute,
  `tap.aggregate`, `tap.bundleIds`, `tap.responsibleSpi`, harness version, the engine's `im_build_info` and the
  tool's own SHA-256.

### 16.5 Cases

Pass rule §16.6. Window [1.5 s, end] unless stated; every case checks "no clicks" unless marked; timings relative to the
recording start (1 s after the initial fixtures run). Numbers follow M2's table where the case carries over; M2's 4, 5,
19, 26 and 27 are Windows-only and absent; 28–33 are macOS-only.

| # | Case | Setup | Checks | Required |
|---|---|---|---|---|
| 1 | `baseline-tap` | `IM_AUDIOF_TEST_NO_EXCLUSIONS`; FakeGame 1000 + FakeDiscord 440 | both present. If it fails, every later case is INCONCLUSIVE | yes |
| 2 | `builtin-voice` | FakeDiscord 440; FakeGame 1000 | 440 absent, 1000 present; row `testdiscord` `kept_out`/`builtin` | yes |
| 3 | `two-voice-apps` | FakeDiscord 440, FakeTS 571, FakeGame 1000 | 440, 571 absent; 1000 present; engine `process-tap` throughout | yes |
| 6 | `helper-tree` | FakeDiscord (spawn) → `FakeDiscord Helper.app` plays 440; FakeDiscord (spawn) → `bin/voice-cli-helper` plays 571; FakeGame 1000 | 440 (enclosing bundle) and 571 (responsible only) absent; 1000 present | yes |
| 7 | `late-helper` | FakeDiscord idle; at t = 3 s it spawns `FakeDiscord Helper (Renderer).app` (never seen before) playing 440 at once (`firstRenderNs`) | 440 absent over [1.5, end] with short windows from `firstRenderNs`; 1000 present; `maxTapUpdateMs` recorded | yes |
| 7b | `late-helper-legacy` | as 7 with `IM_AUDIOF_TEST_NO_BUNDLE_IDS` (the 14–15 path, also run on 26 and 27) | as 7 | yes |
| 8 | `late-app` | FakeDiscord idle; at t = 3 s FakeGame2 starts 683 for 3 s | 683 present in [3.5, 5.8]; onset ≤ 500 ms after its `session_created` (recorded); no clicks | yes |
| 9 | `steam-game` | mic flag; FakeSteam (`spawn -mic`) → `fixtures/steamapps/TestGame.app` (a sibling bundle; posix_spawn, responsible = FakeSteam) 1000 | 1000 present (the launcher stop); FakeSteam row `kept_out`/`mic`; TestGame row heard | yes (mic) |
| 10 | `isshoni-self` | harness plays 797 (AudioQueue); harness `webview -f 1700` in its own process (WebKit GPU process, responsible = harness); FakeGame 1000 | 797, 1700 absent; 1000 present; rows `isshoni` | yes |
| 11 | `viewer-browser` | `SetViewerBrowser("c_test1", "chrome", "macos", true)`; FakeChrome → Helper plays 440, Helper (Renderer) plays 797; FakeGame 1000 | 440, 797 absent (reason `viewer`); 1000 present | yes |
| 12 | `browser-mic` | FakeChrome → Helper `-tone 440 -mic`; Helper (Renderer) 797; FakeGame 1000 | both absent (reason `mic`, browser); a `browser-mic` prompt | yes (mic) |
| 13 | `mic-user` | FakeKOOK `tone -mic` 440; at t = 5 s `Service.Decide(include)`; FakeGame 1000 | 440 absent in [1.5, 5.0], present in [5.6, end]; onset ≤ 500 ms after T_set (recorded); the `mic` prompt existed; no clicks (crossfade) | yes (mic) |
| 13v | `mic-user-vpio` | as 13, with FakeKOOK `tone -mic-vpio` 440 (voice processing, §10.2) | as 13 (1000 present under the pass rule even if ducked); the 1000 level recorded next to case 13's (VPIO ducking, §6.3); the FakeKOOK row's `sessions[]` `capture` entry recorded (what `Devices` resolved to, §10.2's measurement) | yes (mic) |
| 14 | `shared-app-mic` | FakeGame `tone -mic` 1000; `OnShareStarted(<its pid>, true)` | 1000 present; warning `shared_app_uses_mic` | yes (mic) |
| 14b | `shared-app-mic-pre15.2` | as 14 with `OnShareStarted(0, true)` (what a window pick gives on 14.4–15.1, §9.9) | expectation recorded for 14.x and 15.0–15.1: 1000 absent, FakeGame row `kept_out`/`mic`, a `mic` prompt, no `shared` flag; after `Decide(include)` 1000 present | INFO (all versions; documents the accepted degradation) |
| 15 | `runtime-rule` | FakeVoice 440 + FakeGame 1000; at t = 4 s `SetAppDecision("bundle:com.isshoni.test.fakevoice", "keep-out")` | 440 present in [1.5, 4.0], absent in [T_set + B, end]; measured cut − T_set ≤ **B**, where B = 150 ms on bare metal and 150 ms + one guest IO buffer in a VM (`os.virtual`; the buffer is the aggregate's `kAudioDevicePropertyBufferFrameSize` at the tap rate, recorded as `ioBufferMs`); no clicks; `crossfades` ≥ 1, `crossfadeAborts` = 0. Also recorded, never gated: `cutFromEngineMs` = measured cut − the `tNs` of the engine's `rules_changed` event for this change (excludes Go and guest scheduling), and, in a VM, the case runs 5 times (each run must meet B) and the five cuts are recorded as the distribution | yes |
| 16 | `app-boundary` | FakeDiscord `spawn -ls FakeBrowser.app` → FakeBrowser plays 1000 | 1000 present | yes |
| 17 | `spawned-voice-app` | FakeLauncherX (`spawn -tone 683`) posix_spawns FakeDiscord's executable, which plays 440 | 440 absent (its enclosing bundle identifies it; that LauncherX is its responsible app doesn't matter); 683 present (on macOS a parent is never "not captured") | yes |
| 18 | `orphan` | FakeDiscord (`spawn -orphan-after 4`) → `bin/voice-cli-helper` plays 440 for the whole case | 440 absent over [1.5, end], short windows included, around and after the parent's exit (lineage cache) | yes |
| 20 | `timestamps` | `Bursts.app`, 10 bursts | tap onsets vs predicted output times (§13.5): median abs error ≤ 15 ms, p95 − p5 ≤ 5 ms; Opus spacing exactly 20 000 000 ns; 50 ± 1 packets/s | yes |
| 21 | `opus` | FakeGame 1000 + FakeDiscord 440, 128 k then 256 k at t = 4 s | decoded with `im_test_opus_decode`: 1000 within ±1.5 dB of the tap, 440 absent; sizes change; no `IM_PKT_REINIT` after the first packet | yes |
| 22 | `multi-endpoint` | ≥ 2 output devices; FakeGame on the default, `endpoint-tone` 683 on another | records whether 683 is captured | INFO (SKIP with one device) |
| 23 | `soak` (`-long`) | 30 min: FakeGame music-like (1000 + 683, 0.1 Hz AM), FakeDiscord idle with mic | zero underruns, trims, `mixerResyncs`, `zeroRunRebuilds`; no clicks; `maxResamplePpm` ≤ 1000; 440 never present | minor tier |
| 24 | `viewer-browser-included` | as 11 with `appRules {"bundle:com.isshoni.test.chrome": "include"}` before the Service starts | 440, 797 absent (`viewer` beats the saved allow) | yes |
| 25 | `late-child-heard` | FakeGame 1000 from the start; at t = 3 s it spawns `game-child` (same app) playing 683 | 683 present in [3.6, end]; onset ≤ 500 ms after the child's `session_created`; one app row, heard | yes |
| 28 | `runtime-rule-removed` | FakeVoice kept out by a saved rule, playing 440; at t = 4 s `SetAppDecision(…, "auto")` | 440 absent in [1.5, 4.0], present in [T_set + 200 ms, end]; no clicks (crossfade) | yes |
| 29 | `crossfade-alignment` | FakeGame plays 1000 + 683 steady; FakeVoice 440; at t = 4 s keep FakeVoice out | the 1000 and 683 levels in 10 ms windows over [T_set, T_set + 300 ms] stay within 1.0 dB of their level in [2, 4]; no clicks; `unalignedCrossfades` = 0 | yes |
| 30 | `self-webview-late` | FakeGame 1000; at t = 3 s the harness starts `webview -f 1700` (a fresh WebKit GPU process may start then) | 1700 absent with short windows from its `firstRenderNs` | yes |
| 31 | `voice-during-crossfade` | FakeVoice 440 playing; at t = 4 s keep it out; at T_set + 30 ms FakeDiscord (not running before) starts 571 | 571 absent over [T_set, end] with short windows (the old tap gets the addition, §8.5); 440 absent after T_set + 150 ms | yes |
| 32 | `no-bundle-ids-boundary` (26+ only; SKIP below) | built-in FakeDiscord rule; FakeBrowser (own ID `com.isshoni.test.browser`) opened by FakeDiscord plays 1000 | 1000 present: `bundleIDs` never keeps out an app the table hears | yes on 26, 27 |
| 33 | `device-rate-change` | FakeGame 1000, FakeDiscord 440; at t = 3 s the harness sets the default output's nominal rate to 44 100 Hz (restored at the end, and at the next harness start if a marker file says it wasn't) | 1000 present in [1.5, 2.9] and [4.0, end]; 440 absent over [1.5, end] **including the rebuild**; `tap.rate` 44100 after; `tapRebuilds` ≥ 1. SKIP when the device doesn't offer 44.1 kHz | yes |

Notes: case 17 is M2's number with a new meaning (M2's `tree-contains-excluded` has no macOS equivalent). Case 1 is
M2's `baseline-endpoint` re-purposed (macOS has no endpoint engine). Case 33 changes a device setting: the README says
so, and the harness restores it.

### 16.6 Pass rule (exact)

05 §18.6 unchanged: analysis on the PCM tap, mono `(L+R)/2`, 48 kHz; Goertzel with Hann; **present** ≥ −45 dBFS;
**absent** ≤ −50 dBFS and ≤ P_min − 40 dB (the plan's "1 kHz present and 440 Hz at least 40 dB lower") **and** every
50 ms window (hop 10 ms) ≤ −50 dBFS; measured cut and onset from the same windows; clicks: |x[i] − 2x[i−1] + x[i−2]| >
0.03 merged within 10 ms; positive controls from the report sampled every 250 ms (every fixture's process appears as a
`render` session `active` at least once; absent fixtures `kept_out` in every sample inside the window; present ones
never `kept_out`); verdicts PASS / INCONCLUSIVE / SKIP / FAIL with 05's causes (a gap > 30 ms in tap capture times is
"the harness lost audio"; a non-fixture row heard above −60 dBFS cannot be seen on macOS, since levels are not per app,
so the silence pre-check and case 1 carry that duty instead). The only budget that differs is case 15's B (150 ms on
bare metal, + one recorded guest IO buffer in a VM; §16.5).

### 16.7 Report

**This doc owns the macOS report schema**; `05-exit-and-verification.md` §4.4 only reads it (A-2 reads
`engine.commit`, `engine.testHooks`, `tool.sha256`; A-10 reads `os.virtual`). 05 §18.7's schema v1, file
`audiotest-macos<major>-<buildId>-<YYYYMMDD-HHMM>.json`, with:
- `os`: `{"product": "macos", "version": "27.2", "build": 270200, "buildId": "27C55", "ubr": 0, "edition": "",
  "virtual": false}`: `build` stays M2's int (`major·10000 + minor·100 + patch`), `buildId` (new, string, from
  `kern.osversion`) and `virtual` (new, `kern.hv_vmm_present == 1`);
- `engine` (new): `{"version", "commit", "abi", "testHooks", "archiveSha256"}`: the first four from
  `im_build_info`, so a gate report names the engine of the release candidate;
- `tool` (new): `{"sha256"}` of the running Mach-O (the universal `isshoni-audiotest` file), which 05's A-2 compares
  with `SHA256SUMS`;
- `dll`: omitted on darwin (`omitempty`; Windows unchanged). The engine archive is identified instead by
  `engine.archiveSha256` = `main.engineArchiveSHA256` (set per arch with `-X main.engineArchiveSHA256=<hex>` and
  `-X main.engineVersion=<v>` from 02's `native/dist/<dir>/archive.sha256` and `build.json`, §16.1): the running
  slice's archive hash;
- `machine`: `{"cpu", "model", "outputs": [{"transport": "builtin", "rate": 48000}], "mic", "outputVolume", "muted"}`
  with `cpu` `arm64` | `x86_64` | `x86_64 under Rosetta` and `model` the `hw.model` family (`MacBookPro`,
  `VirtualMac`), never the full identifier, a name or a serial;
- `gateConfig`: `{"bundleIds": true|false}` (whether the 26+ path ran);
- per case `counters` also carries `tapUpdates`, `maxTapUpdateMs`, `crossfades`, `crossfadeAborts`, `tapRebuilds`;
  case 15 adds `budgetMs` (B), `ioBufferMs`, `cutMs` (a list: one entry, five in a VM) and `cutFromEngineMs` (same
  shape); case 20 adds `stampOffsetMedianMs` with `tap.aggregate` (§8.6's table input).
Redaction as 05 §18.7 (device names → `{transport}`; real apps → `other`; fixture app ids → `fixture:<role>`; paths
under a home folder → `~/…`). `merge` on darwin refuses a manual entry whose `product`, `build`, `ubr` **or
`buildId`** differ (two 27.2 builds, 27C55 and 27C60, have the same int `build`). Summary line: `isshoni-audiotest
v0.3.0-… · engine 0.3.0 (commit …) · tool sha256:… · macOS 27.2 (27C55) arm64 · 27/27 PASS · … · gate=PASS`.

### 16.8 Where it runs

| Where | What | Grants | Gating |
|---|---|---|---|
| Mac (`task test`, `task native:test`) | analyzer, verdicts, `casesmac.go`/`fixtureplan_mac.go` data tests, `tappolicy`/`audiomix` suites in `host-debug`/`host-tsan`, `GOOS=darwin` build | none | CI too |
| CI `native-host` (ubuntu-24.04, macos-15), windows-2025 | the portable parts above: `native/common` incl. `tappolicy` and `audiomix` (`audiomix` with the align option also in `win-x64-ci`) | none | required (`ci-ok`) |
| CI `desktop-macos` (`macos-26` from 04's `tools.json` `runner`; untagged, fake engine) | the untagged Go tests; `cmd/isshoni-audiotest` builds its stub driver there (`driver_other.go`) | none | required |
| CI `macos-native` (02's job on `macos-26`, Xcode from `tools.json`; 05's matrix adds `macos-15` and `macos-15-intel`) | the static engine; ctest of `mac-arm64-ci` incl. `native/macos/tests/audio` (rate stage, fake-HAL controller, snapshot of spawned AudioQueue fixtures guarded by `IM_REQUIRE("hw:audio-out")`); `MacTapHW.*`/`MacMicHW.*` skip through `IM_REQUIRE` with ledger lines; `go build -tags imstatic ./cmd/isshoni-audiotest` and `isshoni-audiotest selfcheck`; `go test -tags imstatic,imtesthooks ./cmd/isshoni-audiotest/...` and the darwin binding (`TestTap*HW`/`TestAudioControllerHW` carry `isshoni_hw`; where 05's job builds that tag they skip through `macenv` with ledger lines) | none (TCC not available; the job never sets `ISSHONI_TEST_TCC`) | required |
| Mac, `task mac:hw:audio` (05 §5.4) | through `harness.sh` in `isshoni Dev Harness.app` with `ISSHONI_TEST_TCC=audio,mic`: gtest `MacTapHW.*`, `MacMicHW.*`; Go `TestTap*HW`, `TestAudioControllerHW` (`imstatic,isshoni_hw`); `isshoni-audiotest run` (all cases, no `-gate`) | System Audio Recording for the dev harness; Microphone for the six mic fixture bundles (§16.4) | local; a declared but missing grant fails (05 V-D6) |
| Owner's MacBook Pro, macOS 27 | `harness.sh isshoni-audiotest run -gate` (exit harness) + M-R1…M-R10 | System Audio Recording for isshoni Exit Harness; Microphone for the six mic fixture bundles | **M3 gate**, every release |
| macOS 26, 15 and 14.4 (05's VM lab, `-granted` clones) | `run -gate` + M-R1, M-R4, M-R8 | the same, per VM | **M3 gate**, minor releases and the M3 exit |

**TCC gating** follows `05-exit-and-verification.md` §5.1–§5.2 (no `ISSHONI_TCC_AUDIO`, no ctest label):
- Native grant tests are gtest suites in `native/macos/tests/audio/`, each starting with 05's `IM_REQUIRE(...)`
  (`native/macos/tests/support/macenv.h`): `MacTapHW.*` = `IM_REQUIRE("tcc:audio", "hw:audio-out")`: tap create and
  destroy, rebuild on rate and default-device change, zero-buffer rebuild, two-tap alignment ±1 sample, kept-out
  silence, the 44.1 kHz path; `MacMicHW.*` = `IM_REQUIRE("tcc:audio", "hw:audio-out", "tcc:mic", "hw:audio-in")`:
  isshoni's own process lists no input device while its tap runs; a mic fixture is detected as a mic user; a
  `-mic-vpio` fixture's `IsRunningInput` and `Devices` are logged (AM5's measurement) and, after §10.2's decision, it
  is detected as a mic user.
- Device-dependent tests that need no grant (`proc_snapshot_test`'s AudioQueue fixtures) use
  `IM_REQUIRE("hw:audio-out")`.
- Go grant tests are `TestTap*HW` (PCM capture of a fixture tone, kept-out silence, `TapMixer` stop releases the
  aggregate) and `TestAudioControllerHW` (crossfade dip on a steady tone) in `internal/media/native`, build tags
  `imstatic,isshoni_hw`, each starting with `macenv.Require(t, macenv.SystemAudio, macenv.AudioOutput)`.
- Every one of them that skips on a CI job has one line in `.github/macos-skips.txt` (05 V-D7), added by the slice
  that adds the test; `tools/skipcheck` fails an unlisted skip.

VMs: 05's VM lab (`05-exit-and-verification.md` §3.2, `vm-lab.md`) owns the guests, their sound device and any
fallback for a guest without one. The report records the device's `transport` (e.g. `virtual`). Microphone cases need
a guest input device or SKIP there (the minor-release gate requires them on 27 only, §16.10). Remote gate runs, if
wanted, go through 05 §5.5's dispatch-only self-hosted `machw.yml` (`exit-audio`); hosted runners never get a grant.

Intel: the universal binary's x86_64 slice (`02-macos-engine.md`) runs the same code; no audio code is
architecture-specific. Without an Intel Mac the x86_64 slice's audiotest is "unverified" in the README's results
table, and a community test report fills it (the plan's long tail). The x86_64 build's unit tests and
`isshoni-audiotest selfcheck` run on `macos-15-intel` where 05's `macos-native` matrix includes it (Rosetta on
`macos-26` after GitHub retires that image, 05 R6); none of them captures.

### 16.9 Manual real-app checks (macOS)

| Id | Steps | Pass | OS |
|---|---|---|---|
| M-R1 | Discord desktop: Settings → Voice & Video → Mic Test, or a call; YouTube in Safari at the same time; `record -label MR1 -d 30` | Safari audible, no Discord voice; row `discord` `kept_out`/`builtin` | 27 every release; all four at minor releases |
| M-R2 | Discord Web in Chrome in a voice channel; YouTube in Safari; open the share sheet | `browser-mic` prompt; Chrome kept out; Safari heard | 27 |
| M-R3 | A Steam game on the Mac (if the owner has one), `record` | the game heard; Steam's own sounds follow the mic setting | 27 |
| M-R4 | The real app: share a window with sound while watching a friend's share in the app | the friend doesn't hear themselves; `isshoni` row kept out; `selfAudible` false | all four |
| M-R5 | During a share with a game and Discord in a call: switch output (built-in → AirPods → built-in) | game heard again ≤ 2 s; Discord absent throughout; `reconnecting-sound` gone ≤ 3 s; isshoni never shows the orange microphone indicator and AirPods stay out of hands-free unless Discord itself uses their mic | 27 |
| M-R6 | TeamSpeak and Mumble, if installed, as M-R1 | voices absent | minor |
| M-R7 | Teams, Zoom (test meeting), Slack huddle, Vesktop; for each `mdls -name kMDItemCFBundleIdentifier /Applications/<app>.app` and the helpers' IDs | voice absent; IDs equal §11.1's (else fix the data) | once before `v0.3.0`, then when a row changes |
| M-R8 | `tccutil reset AudioCapture io.isshoni.desktop`; run the first-run wizard: deny; then grant in System Settings | denial: banner `audio-permission` (denied), sound check `permission_denied`; grant: sound check passes, with or without restart (record which, §13.2) | 27 and 14.4 |
| M-R9 | Share a full display with the app window visible and a video playing in the app | no isshoni playback heard (tap) — the video side is `02-macos-engine.md`'s | 27 |
| M-R10 | `record` while a game tone plays; set output volume to 10 %, then mute | level unchanged → the tap is pre-volume (record the answer; it gates the VolumeNote, §6.3) | 14.4 and 27 |

### 16.10 The exit gate and release tiers

**M3 exit (audio part)**, on the release-candidate commit:
1. `harness.sh isshoni-audiotest run -gate` exits **0** on macOS 27 (the MacBook Pro) and on 26, 15 and 14.4, with
   the exit harness of that commit: the report's `engine.commit` = the rc commit, `engine.testHooks` false,
   `tool.sha256` = `SHA256SUMS`' `isshoni-audiotest` line, and `engine.archiveSha256` = the rc's per-arch
   `archive.sha256`, which 05's `exit-tools-manifest.json` repeats (05 §2.1 A-2). On a
   machine or VM without a usable microphone the mic cases SKIP and the exit is 3: such a run counts for 14.4, 15 and
   26 only when the 27 run has all mic cases PASS, and the SKIPs are listed in the results table.
2. M-R1, M-R4 and M-R8 pass on 27 and 14.4; M-R2, M-R5 and M-R10 on 27; M-R7 once for every app the owner can run
   (others "unverified").
3. Case 20's values and `enginetest avsync` (|median| ≤ 20 ms, run by `05-exit-and-verification.md`) recorded with
   the reports.
4. The reports, merged with their manual entries, are committed as
   `cmd/isshoni-audiotest/results/YYYY-MM-DD-macos<major>-<buildId>[-vm].json` (`-vm` when `os.virtual`) with a row in
   the README's results table.

**Release tiers**: every release: `run -gate` and M-R1 on macOS 27; the owner attaches the report to the GitHub
Release after publish as `audiotest-macos27.json` (like M2's `audiotest-win11.json`; 05 §9.6). Minor releases: the
gate on all four versions, each report attached as `audiotest-macos<major>.json` (`audiotest-macos14.json`,
`-macos15`, `-macos26`, `-macos27`), M-R1…M-R10 as the table says, the soak on 27. 04 §10.7 carries the matching
checklist line, and 04's `check-draft.sh` neither requires nor refuses these files.

## 17. CI (who changes what)

- `ci.yml` `native-host` (ubuntu-24.04, macos-15): builds `native/common` including `tappolicy` and the `audiomix`
  align option; unchanged job definition (new tests are found by `im_add_tests`). Nothing of `native/macos` runs here.
- `ci.yml` `windows-native`: rebuilds the DLL with the changed `audiomix` and report writer; all M2 tests and goldens
  must pass unchanged, which is the proof that Windows behavior didn't move. `windows-go`: `internal/desktop/audio` and
  `internal/media` tests with `RulesOS` empty (M2's expectations unchanged), the new `probe_test` and `TestFlags` rows
  (Windows values byte for byte M2's), plus the new darwin-table tests (portable). `windows-desktop`:
  `isshoni-audiotest selfcheck` against the DLL as before.
- `ci.yml` `desktop-macos` (`macos-26`, 04's `tools.json` `runner`): unchanged by this doc; untagged, so
  `cmd/isshoni-audiotest` builds its stub driver (§16.1).
- `ci.yml` `macos-native` (`02-macos-engine.md` §6.7, `macos-26`; 05 §5.3's matrix adds `macos-15` and
  `macos-15-intel`): this doc adds (1) nothing for native tests: ctest of `mac-arm64-ci` picks up
  `native/macos/tests/audio` automatically, and its grant tests skip through `IM_REQUIRE` (§16.8); (2) `go build -tags
  imstatic ./cmd/isshoni-audiotest` with 02's env and `-X` flags, then `isshoni-audiotest selfcheck`; (3)
  `./cmd/isshoni-audiotest/...` in its `go test -tags imstatic,imtesthooks` list; (4) the ledger lines for
  `MacTapHW.*`, `MacMicHW.*`, `TestTap*HW` and `TestAudioControllerHW`. `02-macos-engine.md` §6.7 steps 3–4 carry the
  same audiotest build, test and selfcheck lines.
- No hosted runner ever gets a TCC grant (05 V-D6/R7); remote gate runs, if wanted, are 05 §5.5's dispatch-only
  self-hosted `machw.yml` (`exit-audio`).
- `release.yml`: 05's `macos-exit-tools` job builds the artifact `isshoni-exit-tools-macos` (not a release asset);
  its layout is `05-exit-and-verification.md` §4.5's, with the `fixtures/` folder beside `isshoni Exit Harness.app`
  (§16.3). This doc only requires that `SHA256SUMS` lists `isshoni-audiotest` and every fixture's executable
  (including `fixtures/bin/voice-cli-helper` and `fixtures/steamapps/TestGame.app`'s).

## 18. Test plan

### 18.1 Unit

- **`tappolicy`** (Mac, CI ubuntu/macos/windows; ≈ 70 table cases on fake snapshots): every row of §9.4; §9.11's
  examples; identity rules of §9.3 (outermost `.app`, bare `.xpc`, unbundled, responsible fallback, launcher stop, dead
  responsible process); dot-boundary matching (`com.hnc.discord` vs `com.hnc.discordptb`, case folding, `exe:` values);
  instance rule over strict parent and responsible links, never over a link to a younger process; lineage cache (orphan
  keeps its verdict; cache entry dies with the key); mic detection inputs (aggregate-only device lists are not mic
  users; virtual mics are; with `kVpioIndirectMic` on: an aggregate-only list counts while `physical_input_running`, not
  without it, never for isshoni's own processes, §10.2); stickiness until the root exits; the shared app suppresses row
  6 only; viewer beats allow; `long:` ids; the plan: objects sorted, bundle-ID list with suffixes, no mic users and no
  allow rules in it, `audible_changed` only for processes with running output in the previous or the current snapshot
  (incl. a fresh read folded in: previous not running, current running → in `audible_changed`, so a crossfade, §8.5); a
  property test (10 000 random snapshots in CI, 500 with `-short`): `bundle_ids` never matches a process whose verdict
  is heard, every kept-out process's object is in `objects`, and plans are deterministic; the implicit SELF bundle rule
  matches exactly (`io.isshoni.desktop` doesn't match `io.isshoni.desktop.dev`); a self process with `rpid ≠ pid` leaves
  its WebKit GPU process heard and sets `selfAudible` (§9.7).
- **`audiomix`** (all CI): §8.4's aligned takeover tests; every M2 test unchanged; report writer goldens: M2's
  byte-identical, macOS goldens new.
- **`native/macos/tests/audio`** (Mac; CI in 02's `macos-native` job on `macos-26`, where `IM_REQUIRE` skips the grant
  suites with ledger lines, §16.8): rate stage (§7.4); `TapEngine`/`TapMixer` against 02's `audio_bridge_test`
  expectations (the `im::audio` interface, §5.1); `proc_snapshot_test` (`IM_REQUIRE("hw:audio-out")`) against spawned
  helper processes that open an `AudioQueue` (bundle IDs, enclosing bundles, responsible pid with and without the SPI
  forced off, cache budget ≤ 5 ms for one new object); the tap controller against a **fake HAL** (an injected interface
  over the Core Audio calls the controller makes: build, live update, failures with '!obj', listener events, IO
  delivery, zero runs): live update vs crossfade decisions, the pending crossfade's additions to the old generation,
  queueing, the 500 ms abort, rebuild triggers, retry backoff, the zero-run watchdog's backoff and reset, the fresh
  `IsRunningOutput` read for a plan built outside a poll (fake HAL previous poll not running, fresh read running →
  crossfade, §8.5), the permission state machine (preflight values, non-zero proof, the 30 s watchdog with nothing
  playing → stays `unknown`, the 30 s watchdog with preflight `pending` → stays `pending` with event detail `watchdog`,
  preflight `unknown` + zeros while a heard process plays → `denied`), `tap.permission` in a report while neither
  watching nor live (a preflight read, no tap built), with a fake clock; the sound check with the preflight stubbed to
  `unknown` (SPI absent): no 30 s wait, the chime starts within 200 ms of the first IO callback, a captured chime gives
  `granted` (§13.3).
- **Go**: `audiorules` macOS lists and `Contains` semantics; `internal/desktop/audio` with `RulesOS: "darwin"`
  (rules builder with no Go self app rule, ids, `long:` ids not saved, viewer rules for `safari`, banners of §14.1,
  `selfAudible` → `FriendsHear.selfAudible` and the `viewer-muted` banner, `TestData`); every M2 test with `RulesOS`
  empty unchanged; `ParseAudioReport` on the macOS goldens (`Tap` non-nil in every one, incl. `idle`);
  `media.AudioCapsFromFlags` rows (§14.2); `internal/media/native` `TestFlags` rows (§14.2); the fake's macOS
  scenarios; harness: `casesmac.go` (every case's roles exist in the fixture table, frequencies spaced as 05 §18.3,
  case 15's 150 ms budget, case 33's SKIP rule), `fixtureplan_mac.go` (no fixture has a foreign `.app` ancestor,
  TestGame is a sibling of FakeSteam, the mic-entitled set equals §16.4's list), report fields of §16.7 (`os.buildId`,
  `os.virtual`, `engine`, `tool`, no `dll`), `merge` refusing another `buildId`, the results name with `-vm`, redaction
  of macOS fields.
- **SPA** (vitest): `FriendsHear.selfAudible` reaches 01's `mutesViewer`; `FriendsHear` without per-row meters on
  `process-tap`; the two banners, the version-aware pane text and the `audio-recording` action; About's
  `process-tap` backend key; Windows snapshots unchanged.

### 18.2 Integration

- **Mac, no grant needed**: `im_audio_watch` with real processes: an AudioQueue fixture appears in the report within
  250 ms, classified by its bundle ID; a fixture inside a bundle spawned by another is matched through enclosing
  bundles and responsible pid; isshoni's own WKWebView (a test host app) is matched by responsible pid.
- **Mac, `task mac:hw:audio`** (05 §5.4; System Audio Recording for the dev harness, run through `harness.sh`):
  `TestTap*HW`/`TestAudioControllerHW` (`imstatic,isshoni_hw`, `macenv.Require`) and `MacTapHW.*`/`MacMicHW.*`
  (`IM_REQUIRE`): a share-less engine session with audio only: the PCM tap captures a fixture tone at −18 ± 1 dBFS; a
  kept-out fixture is digital silence; the 44.1 kHz path by switching the built-in output (restored); crossfade
  measured on a steady tone (≤ 0.3 dB dip); `TapMixer::Stop` releases the aggregate (no isshoni device left in
  `kAudioHardwarePropertyDevices`, which private devices never show anyway, and the IOProc count is 0).
  `MacTapPermission.WatchdogUnknownDenies`: the real tap controller and a spawned `AudioQueue` helper that plays (a
  heard process with `IsRunningOutput` true), the preflight SPI stubbed to `unknown` and the generation's IO samples
  zeroed through in-process test seams (`tcc::SetPreflightForTest`, `TapGeneration::SetIoFilterForTest`; native
  tests only, no ABI flag) and the watchdog period shortened to 2 s: the report ends in `permission-denied` with
  event `permission_denied` detail `watchdog`, the mixer keeps sending silence, and with the helper stopped the
  same run stays `unknown`. It replaces the manual `denied`-by-watchdog case that 05's M-MAC-4 dropped.
- **CI ubuntu (e2ehost `-os macos` + Playwright, 01 §19.3)**: the macOS share sheet against the fake's macOS
  scenarios: rows without meters, output-active dots, `audio-permission` banner with its `audio-recording` button
  calling the bridge, `selfAudible` reaching 01's viewer mute.

### 18.3 End to end

- **Mac**: `harness.sh isshoni-audiotest run -gate` (05's harness app); `record` with real apps (M-R1…).
- **Mac + server**: the real app shares to a browser viewer on another machine; the viewer hears the game and not the
  sharer's Discord; `enginetest avsync` (`05-exit-and-verification.md`).

### 18.4 Manual

§16.9's M-R checks; first-run wizard permission flow on a fresh user account on 14.4 and 27 (M-R8).

## 19. Implementation slices

Small, ordered; each is testable on its own. "Mac" = the developer's MacBook Pro (macOS 27).

| Slice | Scope | Depends on | Acceptance | Where |
|---|---|---|---|---|
| AM0 | Run S2's `run-selftest.sh -mic` (`spikes/s2-mac-audio`, unchanged) in 05 V1's `-granted` clones of 14.4, 15 and 26; add the 44.1 kHz device run on 27; results appended to the spike README | 05's slice V1 (VM lab; the VM sound-device setup, incl. any NullAudio fallback, is in 05's `vm-lab.md`) | 12/12 on each version, or each failure is written down with its design consequence before AM6 merges. A lab blocked by boot or device failure (a guest that won't boot on the M5, has no usable output device for taps, or whose taps are silent; 05 §11 R3) counts as written down, with 05 R3's fallback named for that version, so AM6 is not held up | owner's Mac + 05's VMs |
| AM1 | `native/common/tappolicy`: types, identity, matching, rows, mic stickiness, lineage, plan; property test | — | §18.1's tappolicy suite green under `host-debug`, `host-tsan`, `win-x64-ci`; clang-format clean | Mac, CI ubuntu/macos/windows |
| AM2 | `audiomix`: aligned takeover; report writer's macOS model (engine, states, `selfAudible`, `tap`, counters, events); macOS goldens | — | §8.4 tests pass; every M2 `audiomix` test and golden unchanged; new goldens written | Mac, CI all |
| AM3 | Go data and service: `audiorules` MacOS lists, `FamilySafari`, darwin `Contains`; `SelfInfo.RulesOS`/`TestData`; per-OS ids, rules builder, viewer rule, banners, `selfAudible`; `internal/media/audio.go` additive types; `probe.go` macOS bits and `process-tap` backend; `native/audio.go` `TestFlags`; goldens parsed; fake macOS scenarios (selected under 01's `e2ehost -os macos`) | AM2 (goldens) | §18.1's Go tests; all M2 tests unchanged on ubuntu and `windows-go` | Mac, CI ubuntu/macos/windows |
| AM4 | SPA: bridge types (`FriendsHear.selfAudible`, `ShareCaps.audioBackend` `process-tap`), About's `BACKEND_KEYS`, rows without meters on `process-tap`, banners and texts (pane text from 01's version-aware key), `audio-recording` action; Playwright with 01's `e2ehost -os macos` (not `mutesViewer`: 01's MD12) | AM3; 01's `-os macos` flag | lint, typecheck, vitest, Playwright green; Windows snapshots unchanged | Mac, CI ubuntu |
| AM5 | `audio_engine.h` (M2's declarations), `proc_snapshot.mm`, `responsibility.mm` (incl. the own-responsibility check), `report_mac.mm`, `TapEngine::Watch/Unwatch/SetRules/ListAppsJson/LevelsGet`, probe flags, the preflight read in every report (`tcc.mm`'s read half) | AM1, AM2; 02's engine skeleton and export layer (EM3, which already forwards every audio export through `audio_bridge::Engine()`) | 02's `exports_test` forwarding cases compile and pass against `audio_engine.h`; `im_list_audio_apps` through the export layer returns schema v1 with `tap.permission` from the preflight; `isshoni-audiotest apps` lists real apps on the Mac with correct rows and `tap.permission`; `proc_snapshot_test` passes; budgets of §9.6 met; `selfcheck` passes in 02's `macos-native` (`macos-26`); §10.2's VPIO measurement on the Mac (a minimal `-mic-vpio` process, Microphone grant) written into §21 and `kVpioIndirectMic` set accordingly | Mac, CI macos-native |
| AM6 | `tap_generation.mm` (tap, aggregate with fallback, IOProc, rate stage), `tap_mixer.mm` (`NewMixer` → `TapMixer` over `audiomix::Mixer` with `MachTickClock` and the `SetRealtime`/workgroup hook; `Start`/`Stop`/`SetPaused`/`State`), engine-wide mode/mute (the session's Opus stage and PCM tap are 02's EM10) | AM5; 02's `im::mac::SetRealtime` (EM3) | the native gtest `MacTapHW.*` (`IM_REQUIRE("tcc:audio", "hw:audio-out")`, run through `harness.sh` by `task mac:hw:audio`) drives `TapGeneration` and `TapMixer` through the `AudioEngine`/`Mixer` facade with no session: a test `PcmSink` receives a fixture tone from spawned `AudioQueue` helpers (as `proc_snapshot_test`'s) at −18 ± 1 dBFS, the test encodes and decodes it with Opus, and a kept-out helper is digital silence; capture through an audio-only session is 02 EM10's acceptance; rate-stage test; no leaks (Instruments Leaks run recorded once) | Mac (TCC) |
| AM7 | `tap_controller.mm`: listeners, live updates, crossfade policy of §8.5, rebuild triggers, zero-run watchdog, retries; fake-HAL tests; `MacTapHW.*` with ledger lines | AM6; 05's `macenv.h` (V2) | fake-HAL suite green in `macos-native`; `MacTapPermission.WatchdogUnknownDenies` (§18.2) on the Mac; on the Mac: the S2 runtime-rule scenario has no click and finishes ≤ 200 ms; a default-device switch rebuilds without a voice leak | Mac (TCC), CI macos |
| AM8 | permission state machine (incl. `pending` at 30 s), `sound_check.mm` (`permission_pending` reason), the Go 35 s context | AM7, AM3 | the fake-HAL permission tests and `MacTapPermission.WatchdogUnknownDenies` (§18.2: preflight `unknown` with a heard process playing ends in `denied`, event `permission_denied` detail `watchdog`; 05's M-MAC-4 no longer covers this path); on the Mac, M-R8's flow works end to end with the wizard (01's), and an unanswered prompt gives `permission_pending`, not `denied` | Mac (TCC) |
| AM9 | `test_render.mm` (tone, bursts), timestamp offset measurement and the `kTapStampOffsetNs` table (§8.6) | AM6 | case 20's bounds hold on the Mac (median ≤ 15 ms, spread ≤ 5 ms) for `tap-only` and, forced, `tap+output`; both medians written into §21 | Mac (TCC) |
| AM10 | `isshoni-audiotest` darwin: driver and main-thread run loop (build tags of §16.1), fixture verbs, `casesmac.go`, `fixtureplan_mac.go`, `macapp/` fixture templates and entitlements (§16.3), sibling `fixtures/` layout, preconditions, the report schema of §16.7 (`os`, `engine`, `tool`, `merge` by `buildId`); `macos-native` additions of §17 | AM5–AM9; 05's `tools/macharness`, `harness.sh` and `task mac:harness` (V-slices); 04's `sign.sh` | `selfcheck` in `macos-native`; on the Mac, `harness.sh isshoni-audiotest run` (dev harness) executes every case of §16.5 with a verdict; portable tests green on all CI | Mac, CI all |
| AM11 | Specials on darwin: timestamps, opus, multi-endpoint, soak, device-rate-change; `record`/`merge` with macOS fields (05's V7 only adds the A-2 comparison on top) | AM10 | each special produces its report section; the soak completes 30 min on the Mac | Mac |
| AM12 | Gate runs (exit harness): 27, then 26, 15, 14.4; M-R checks; results committed (`…-macos<major>-<buildId>[-vm].json`); README results table; troubleshooting page (Bluetooth hands-free, voice-processing ducking, permission); VolumeNote decision from M-R10; per version and VM: case 20's median per aggregate variant, case 15's `ioBufferMs`/cut distribution and case 13v's device rows recorded in §21 (a table entry added when a median is outside ±5 ms) | AM10, AM11; 04's release identity for the rc; 05's V1 | §16.10 items 1–4 met | owner's Mac + VMs |

## 20. Interfaces other docs rely on

- **`im::audio::AudioEngine` / `Mixer` / `MixerConfig`** (§5.1): M2's boundary unchanged, declared for macOS in
  `native/macos/src/audio/audio_engine.h` (this doc's file, verbatim from `native/windows/src/audio/mixer.h`, plus 02's
  macOS-only `RenderTone`/`RenderBursts`). `TapEngine` is `AudioEngine::Get()`; `NewMixer()` returns a `TapMixer`
  (`Start(cfg)` ≤ 100 ms with `cfg.sink` = 04's `OpusStage`, `Stop()` ≤ 300 ms, per-mixer `SetPaused`/`State`),
  nullptr only after `IM_INIT_NO_AUDIO`. 02's exports, `session.mm` and `audio_bridge` (`Engine()`,
  `SetEngineForTest`) use it; 02's `im_start` does the one-audio-session `IM_ERR_LIMIT` check.
- **ABI 1.1 audio additions** (§4.1) and the macOS rule semantics (§4.2) for 02 to merge into the header; report JSON
  macOS values (§12.1, `tap` in every report with `tap.permission` from a never-prompting preflight), sound-check JSON
  (§13.3, reasons incl. `permission_pending`), error mapping (§13.4).
- **Go**: `audio.SelfInfo.RulesOS` and `.TestData`, `audio.TestData`; `media.AudioReport.SelfAudible`, `.Tap`
  (`media.AudioTap`, non-nil in every darwin report; `.Tap.Permission` is 01's `Permissions.State` source), the new
  counters, `media.SoundCheckResult.Permission` and reasons `permission_denied`/`permission_pending`;
  `media.AudioCaps.{ProcessTap, TapBundleIDs, ResponsiblePID, TCCPreflight}` and Backend `process-tap`;
  `internal/media/native` accepting `IM_AUDIOF_TEST_NO_EXCLUSIONS`/`NO_BUNDLE_IDS`; `audiorules.FamilySafari` and the
  darwin `Contains` semantics; banner codes `audio-permission` (params `cause`) and `tap-failed`;
  `FriendsHear.selfAudible`; engine value `process-tap`.
- **SPA**: `App.OpenOSSettings('audio-recording')` (01's `OSPage`) is called by the `audio-permission` banner;
  `FriendsHear.selfAudible` is read by 01 §10.2's `mutesViewer`; `ShareCaps.audioBackend` `'process-tap'` and About's
  `desktop.settings.about.audioBackend.processTap`.
- **Harness**: `cmd/isshoni-audiotest` darwin driver (tags `darwin && cgo && imstatic`), running inside 05's harness
  apps through `harness.sh`; the sibling `fixtures/` folder, its fixture bundle IDs `com.isshoni.test.*`, entitlements
  and usage strings (§16.3), and `cmd/isshoni-audiotest/macapp/` as the templates 05's `tools/macharness` consumes; the
  report schema (§16.7: `os.build` int + `os.buildId` + `os.virtual`, `engine`, `tool`, no `dll` on darwin), `run -gate`
  exit codes (05's), the results path `cmd/isshoni-audiotest/results/YYYY-MM-DD-macos<major>-<buildId>[-vm].json`, the
  release attachments `audiotest-macos<major>.json`.
- **Tests for 05's ledger**: `MacTapHW.*`, `MacMicHW.*` (gtest, `IM_REQUIRE`), `TestTap*HW`, `TestAudioControllerHW`
  (Go, `imstatic,isshoni_hw`, `macenv.Require`), with their ledger lines (§16.8).
- **For `05-exit-and-verification.md`**: §16.10 is the audio half of the exit; §16.8 says where each version runs;
  case 20 and §8.6 give the audio timestamps `avsync` relies on. 05's `macenv` `tcc:audio` check reads `tap.permission`
  after `im_audio_watch` + `im_list_audio_apps` and fails only on `denied` or `pending` (`unknown` = unverifiable, not
  a failure).

## 21. Depends on (assumed interfaces of the parallel docs)

- **`02-macos-engine.md`**: (1) `native/macos/` exists as an Objective-C++ CMake target linked statically into the
  cgo binaries (the app, `isshoni-audiotest`, `isshoni-enginetest`), with `native/common`'s `im_common`, `im_audio`,
  `im_audiomix`, `im_audiopolicy` and this doc's `im_tappolicy`, libopus and speexdsp from vcpkg (overlay triplets
  `arm64-osx-isshoni`, `x64-osx-isshoni` in `native/triplets/`: static, deployment target 14.4), and its §11.2 cgo
  frameworks (CoreAudio, AudioToolbox, AppKit, Foundation, which cover the engine's audio code) unchanged; the
  harness-only AVFoundation and WebKit are linked by `cmd/isshoni-audiotest/fixture_darwin.go`'s own preamble, never
  by `internal/media/native`; deployment target 14.4, built with SDK ≥ 26 (`task native:mac` refuses less). (2) The
  export layer validates with 04's `api/` rules and calls §5.1's `im::audio` interface through `audio_bridge::Engine()`;
  the session creates the `OpusStage` and PCM ring as 04 §10.3 does, refuses a second audio session with `IM_ERR_LIMIT`
  `session_limit` before `NewMixer`, and passes the sink in `MixerConfig`. (3) `im_clock().mono_ns` =
  `mach_absolute_time` in ns via `mach_timebase_info`; `im::mac::SetRealtime(period, computation, constraint)`.
  (4) `im_source_info.pid` is the picked window owner's pid on 15.2+, 0 for a display and for window picks on
  14.4–15.1 (§8.2.1). (5) `im_caps.os_build` = `major·10000 + minor·100 + patch`. (6) The header minor bump to
  `IM_ABI_MINOR 1` with §4.1's text; the darwin cgo caller exposes the same exports to `internal/media/native/audio.go`
  and `AudioTestHooks`. (7) `im_build_info` with `"sdk"` and `"minOS"` (Go `BuildInfo.SDK`, `.MinOS`); per-arch
  `native/dist/<dir>/archive.sha256` and `build.json`; and `func StaticArchiveSHA() string` exported from
  `archive_darwin.go` (returns `""` without `imstatic`; 02 §6.4/§18 add it) so tools can log the linked archive.
  (8) `mac-arm64-ci`'s ctest picks up `native/macos/tests/audio`; `macos-native` (§6.7 steps 3–4) builds, tests and
  `selfcheck`s `cmd/isshoni-audiotest` (§17). (9) 02 §13.1's `runloop_darwin.m` pattern, which §16.1 copies.
- **`01-desktop-macos.md`**: (1) `cmd/isshoni-desktop` on darwin builds `audio.SelfInfo{PID: getpid, RulesOS:
  "darwin", Exe: ""}`; its bundle IDs are `io.isshoni.desktop`, `.dev`, `.e2e`. (2) The first-run wizard's permission
  step calls `Audio.SoundCheck` and renders its `permission` and `reason` (`permission_pending` → "macOS hasn't shown or
  answered its dialog… [Try again]", `permission_denied` → "macOS blocks isshoni…"); `Permissions.State` reads
  `media.AudioReport.Tap.Permission`; the `audio-permission` banner's action is `App.OpenOSSettings('audio-recording')`,
  whose URLs and fallbacks are 01 §9.4's table; the version-aware pane text `desktop.permissions.audioPane` (14.x
  variant from `AppInfo.osVersion`) and `/troubleshooting#desktop-macos-permissions` (01 §20). (3) 04 §4.1's Info.plist
  template carries `NSAudioCaptureUsageDescription` (text owned by 01) and no `NSMicrophoneUsageDescription` (isshoni
  never opens an input). (4) 01 §10.2's `mutesViewer` reads `FriendsHear.selfAudible`. (5) `App.Relaunch()`, labelled
  "Quit and reopen isshoni" (busy while live), in the wizard and Settings → Permissions, for when M-R8 shows a grant
  needs a restart. (6) `e2ehost -os macos` (01 §19.3) also sets the audio Service's `SelfInfo.RulesOS = "darwin"`.
- **`04-macos-release.md`**: (1) The self-signed identity and the hardened runtime with
  `com.apple.security.device.audio-input`; `sign.sh` (`-mode dev` locally, `-mode release` in `macos-sign`) signs the
  harness apps and the fixtures (§16.3's entitlements) as data. (2) The Info.plist template and its golden (the
  `NSAudioCaptureUsageDescription` key named under `01-desktop-macos.md` (3) above). (3) The check that TCC grants
  (System Audio Recording, Local Network) survive N → N+1 on 14.4, 15, 26 and 27; this doc only relies on it. (4)
  machocheck fails a build whose slices or whose engine `build.json` report sdk < 26.0. (5) §10.7's release checklist:
  every release attaches `audiotest-macos27.json`; every minor one per version of 05's matrix; `check-draft.sh`'s macOS
  asset rule neither requires nor refuses these files.
- **`05-exit-and-verification.md`**: runs §16.10 on the four versions; owns the VM lab (`vm-lab.md`, incl. guest sound
  devices), the exit run book, `enginetest avsync`; `tools/macharness`, both harness apps (`io.isshoni.dev.harness`,
  `io.isshoni.exitharness`), `harness.sh`, `task mac:harness`, `task mac:hw:audio`, the `macos-exit-tools` job and the
  `isshoni-exit-tools-macos` layout (§4.5) with sibling `fixtures/` and `avsync/` folders; `internal/testenv/macenv`,
  `IM_REQUIRE`, `.github/macos-skips.txt` and `skipcheck`; `machw.yml`. 05 §4.4 points to §16.7 for the report schema,
  and its V7 depends on AM10.

Change log of measured constants (filled by slices):
- `kTapStampOffsetNs` table (§8.6): case 20's median per (OS major, aggregate variant), incl. VMs: 27 `tap-only` and
  `tap+output` (AM9); 26, 15, 14.4 (AM12, 05's V1 VMs); entries only for medians outside ±5 ms.
- VPIO mic detection (§10.2): what `IsRunningInput` and `Devices` report for a `-mic-vpio` process, and whether
  `kVpioIndirectMic` is on: 27 (AM5); 26, 15, 14.4 where a guest input exists (AM12, case 13v).
- Case 15 in VMs: guest `ioBufferMs`, the five cuts and `cutFromEngineMs` per version (AM12, 05's V1).
- Grant-without-restart per version (M-R8); tap pre-volume per version (M-R10).

## 22. Risks

| Risk | Mitigation |
|---|---|
| A live tap update or tap-only aggregate behaves differently on 14.x/15 (S2 ran only on 27) | AM0 before AM6; live-update failure → crossfade; aggregate fallback (§7.2); case 7b runs the 14–15 path on every version |
| `responsibility_get_pid_responsible_for_pid` disappears, or isshoni runs under a terminal's responsibility | `selfAudible` + viewer mute (§9.7); helpers inside bundles still match; `bundleIDs` on 26+ |
| A tap silently delivers zeros | zero-run watchdog (§7.7), sound check in onboarding, `possibly_silent_tap` |
| Bundle IDs in §11.1 are wrong for some app | mic detection keeps unknown voice apps out by default; M-R7 verification; "unverified" marking |
| Fixtures with real bundle IDs would disturb the owner's real apps' TCC grants | test IDs only, `TestData` (§16.3) |
| VMs have no sound device | 05's VM lab owns the fix (`vm-lab.md`); other Macs |
| Crossfade alignment fails on some device (IO threads far apart) | 50 ms wait, unaligned fallback counted; case 29 |
| Voice clients using VPIO list only a private aggregate as their input, so mic detection misses them | AM5 measures; §10.2's conditional extension; case 13v on every version with an input |
| Case 15's cut or case 20's offset differs in VMs or with `tap+output` | VM budget B (§16.5); per-(version, variant) offset table (§8.6), recorded in §21 |

## 23. Open questions for the owner

1. Case 33 changes the default output device's sample rate during the run (restored afterwards). Is that acceptable
   on your Mac, or should it be an opt-in flag that the gate doesn't require?
2. Older versions: the gate on 14.4, 15 and 26 runs in 05's VM lab; the open question about the lab (and a guest
   sound device) is `05-exit-and-verification.md`'s.
3. Intel: without an Intel Mac, the x86_64 slice's audio stays "unverified" (community reports). OK for M3?
