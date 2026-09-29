# Spike S2 (audio): macOS "all system audio except voice apps"

Throwaway spike for plan milestone M0 (S2, audio part; the VideoToolbox part is in `../s2-mac-vt`). **Question:** can
one Core Audio process tap capture everything except voice apps (Discord and its helper processes, apps that start
mid-share, isshoni's own playback including its WKWebView), cleanly and without leaks? And does a stable self-signed
identity keep the System Audio Recording grant across rebuilds, without a paid Apple account?

Not production code. M3 reimplements this as the macOS engine behind `native/include/isshoni_media.h`. It uses the
same C ABI as S1 (`isshoni_audio.h`), so the Go side is the same on both platforms.

## Run

```bash
./dev-signing.sh      # once: stable self-signed identity in its own keychain (~/.isshoni-dev/signing)
./build.sh            # dist/isshoni-s2.app + fake apps in dist/fixtures, all signed with that identity
./run-selftest.sh     # [-only a,b] [-mic] [-keep]; the first run asks for permission once
go test ./...         # analysis + resampler unit tests
```

`run-selftest.sh` starts the app through LaunchServices, so macOS attributes the permission to "isshoni S2". Running
`s2` from a terminal would attribute it to the terminal app instead. Other tools:
- `s2 probe`;
- `s2 apps [-json]`: every audio process with its responsible app and classification;
- `s2 record -d 20 -o out.wav`;
- `./run-app.sh lancheck`: the Local Network permission (sends one UDP datagram to the default gateway).

## What it does

- **One global tap with an exclude list.** `CATapDescription initStereoGlobalTapButExcludeProcesses:` (macOS 14.2+),
  private, on a private aggregate device (`tapautostart`, drift compensation; tap-only by default). An SPSC ring
  carries audio from the HAL IO thread to `im_audio_read`, which delivers 10 ms chunks at 48 kHz with capture times.
  An AudioConverter resamples when the tap runs at another rate (unit-tested 44.1 → 48 kHz).
- **Rules** (same kinds as Windows, macOS semantics):
  - *App rule* = bundle ID, matched at a dot boundary against:
    - the process's own bundle ID;
    - every `.app`/`.xpc`/`.appex` enclosing its executable;
    - its **responsible process** (`responsibility_get_pid_responsible_for_pid`, looked up at runtime).

    This catches Electron helpers (`com.hnc.Discord.helper.Renderer`, …) and plain helper executables.
  - *Instance rule* = pid; matches the pid, its descendants, and anything it is responsible for.
  - *isshoni itself*: its pid, its children, and everything it is responsible for (WebKit's GPU process).
  - *Mic users* (optional flag): processes with `kAudioProcessPropertyIsRunningInput`, sticky for 10 s, plus every
    process with the same responsible app.
- **Live updates.** The engine listens to `kAudioHardwarePropertyProcessObjectList` and polls every 250 ms (mic state).
  When the excluded set changes, it sets `kAudioTapPropertyDescription` on the running tap (no restart).
  - On macOS 26+ the tap also gets `bundleIDs`: each rule plus the standard Electron helper suffixes, with
    `processRestoreEnabled`. So a helper that starts later is excluded by Core Audio itself, before the engine has
    seen it.
  - A change of default output device or tap format rebuilds the tap.
- **Self-test fixtures.** Every fake app is a copy of `s2` in its own signed bundle, with its own bundle ID, started
  through LaunchServices (`open -n -g -j`) like a normal app:
  - `FakeVoice.app` with an Electron-style `FakeVoice Helper.app` (started with posix_spawn);
  - a plain `voice-cli-helper` with no bundle;
  - `FakeTS`, `FakeGame`/`FakeGame2`, `FakeBrowser` (opened *by* FakeVoice through LaunchServices);
  - `FakeVoIP`, which holds the microphone.

  Tones are −20 dBFS at frequencies between musical notes; Goertzel levels per check, plus per-10 ms leak timing.

## Results (2026-09-29, MacBook Pro M5, macOS 27.0, output at 48 kHz)

Full run, `./run-selftest.sh -mic` (build 6): **12 PASS, 3 INFO (controls), 0 failed**. Report:
`results/2026-09-29-m5-macos27.json`. Other apps running on the Mac are counted but not named. Test tones play at
−20 dBFS; "absent" means ≥40 dB below that and below −50 dBFS. Digital silence is about −150 dB.

| Scenario | Result | Levels (dBFS) |
|---|---|---|
| endpoint-baseline: no exclusions, hears everything | ✅ | game −20, voice −20 |
| exclude: two voice apps at once | ✅ | game −20, voice −161, TeamSpeak-like −156 |
| helper-process: Electron-style helper app plays the voice | ✅ | voice −165 |
| helper-responsible: helper with **no bundle ID**, matched only through its responsible app | ✅ | voice −149 |
| app-boundary: an app the voice app opens via LaunchServices stays audible | ✅ | −20 |
| self: isshoni's own playback | ✅ | −150 |
| **self-webview: isshoni's WKWebView** (audio plays in `com.apple.WebKit.GPU`; excluded via responsible PID = isshoni) | ✅ | −157 |
| late-start: voice app starts mid-share | ✅ | voice −113, no 10 ms window above −40 (strongest −92) |
| late-start-helper: a never-seen helper starts mid-share | ✅ | −113, no leak |
| late-start-helper **without bundleIDs** (control: the macOS 14–15 path) | ℹ️ same | −113, no leak |
| runtime-rule: exclude an app **while it plays** | ✅ no click | crossfade done 65 ms after the change |
| runtime-rule-hard-cut (control: live tap update in place) | ℹ️ | **click at 4.046 s** |
| runtime-rule-removed: re-include an app while it plays | ✅ no click | crossfade done 63 ms after the change |
| aggregate-with-output (variant) | ℹ️ works too | voice −157 |
| mic-user: unknown app holding the microphone | ✅ | −172 |

**Permission persistence (plan gate, macOS 27 part): ✅** Builds 1→9 changed the code each time (a new CDHash), but
all were signed with the same self-signed identity. The designated requirement stayed
`identifier "io.isshoni.spike.s2" and certificate root = H"0f43…7e54"`.
- **System Audio Recording:** macOS asked **once** (build 1). Every later build captured audio within 0.3–0.5 s with
  no prompt, and the private preflight reported "allowed".
- **Local Network:** granted on build 8 and kept on build 9 with no prompt. Before the grant, the send failed with
  EPIPE; the terminal context, which already has the grant, sent the same packet fine.

  A native app can always connect to the Mac's **own** LAN address (S4's block of Firefox there was
  Firefox-specific). That's why `lancheck` targets the gateway.

The identity is untrusted, and no trust settings were changed. `codesign` accepts it when it is selected by SHA-1 hash,
and TCC is satisfied by the stable designated requirement. **No paid Apple account is needed to keep grants across
updates.**

## Findings that change the M3 design

1. **The global exclude tap is the right primitive.** Excluded apps come out as digital silence. There is no mixer and
   no per-app stream bookkeeping, unlike Windows' include-set.
2. **Apps that start mid-share don't leak, even without `bundleIDs`.** A process object appears when the app creates
   its audio client, before its first buffer plays. The process-list listener updates the tap about 4 ms later. So
   the macOS 14–15 path, without `bundleIDs`, is as good here. `bundleIDs` stays as a belt-and-braces measure on 26+
   (rule plus Electron helper suffixes).
3. **A tap update cuts audio hard.** Changing the exclude list of a running tap removes or adds an app within one
   sample, and an audible app produces a click. Fix (implemented here): when an app that is **already playing**
   changes sides (rule change, mic detected), build a second tap with the new list and crossfade over 20 ms, then
   destroy the old tap. New apps (not yet playing) still get the instant live update.
   - Two concurrent taps align to **0 ± 1 samples** by capture timestamps.
   - Building a tap plus its aggregate takes 16–26 ms. The switch completes 63–137 ms after the change.
   - Two details were needed:
     1. while the fade is pending, the old tap must not receive further list changes;
     2. the reader must wait up to one IO buffer for the new tap, because each tap has its own IO thread and the new
        one can run a buffer behind.
4. **`kAudioProcessPropertyIsRunningInput` is not "uses the microphone".** It is also true for processes that only
   read a process tap: isshoni itself, and another app on this Mac that visualizes system audio.
   - A real mic user lists a real input device in `kAudioProcessPropertyDevices` (input scope).
   - A tap reader lists none, because its private aggregate is invisible to other processes.
   - Mic detection must use the device list. Aggregate devices don't count; virtual mics (Krisp) do.
5. **Responsible PID is the key to helpers and XPC services.**
   - WebKit's GPU process belongs to the app that owns the WKWebView (seen for several apps on this Mac). isshoni's
     own is matched without touching Safari's.
   - Plain helper executables with no bundle ID are matched the same way.
   - `responsibility_get_pid_responsible_for_pid` is SPI (looked up with dlsym). If it disappears, the fallbacks are
     the parent chain plus enclosing bundles.
6. **Tap-only aggregate devices work.** No output device is needed in the aggregate, so a headset's mic never gets
   opened by accident. The variant with the output device as clock source also works.
7. **Per-process audio classification costs ~20–30 ms per snapshot** with about 45 audio process objects (sysctl
   ancestor walks). That's fine every 250 ms on a background queue, but M3 should cache per pid.

## Not covered here (still open for S2)

- **Other macOS versions:** 14.4, 15 and 26. That includes permission persistence across updates and the 44.1 kHz
  resampler path on a real device (only unit-tested here). This needs macOS VMs (UTM/Tart) or other Macs.
- **Real Discord:** installed on this Mac, but it needs a logged-in voice call.
  `s2 apps` / `s2 record` are ready for a manual check.
- **Default-output-device changes mid-share:** the rebuild path exists, but switching the default device is the
  owner's setting, so it's untested.
- **Video side of S2:** SCContentSharingPicker and excluding isshoni's own windows, plus the DMG "Open Anyway" and curl
  install paths. The VideoToolbox encode benchmark is in `../s2-mac-vt`.
