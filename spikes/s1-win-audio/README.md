# Spike S1: Windows "all audio except voice apps"

Throwaway spike for plan milestone M0. **Question:** on real Windows 10 22H2 and Windows 11, can isshoni capture
everything the PC plays **except** Discord / TeamSpeak / a browser / isshoni itself, without clicks, and pick up apps
that start mid-share?

**Status:** written 2026-09-28/29 on macOS and cross-compiled (MinGW, no warnings). Two adversarial review rounds found
27 issues, all fixed. It **has not run on Windows yet.** The results table below is empty until the owner runs it.

## How it works

WASAPI process loopback (`ActivateAudioInterfaceAsync` + `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`) takes
**one** target process tree per stream, so one EXCLUDE stream can't keep out Discord *and* TeamSpeak *and* isshoni.
The engine (`native/engine.cpp`, a C ABI DLL) instead keeps an **include set**:

1. Every 500 ms the **poller** lists audio sessions on every output device (and microphone sessions, to spot voice
   apps).
2. It classifies each process:
   - *app rules* match the process or an ancestor of the **same app**. The walk stops at launchers such as Explorer,
     Steam or cmd, and at a different install folder, so a browser Discord opened for a link isn't treated as Discord;
   - *instance rules* match a PID plus all its descendants;
   - *mic users* optionally catch any app holding the microphone. This is sticky for 10 s, so a brief device hiccup
     doesn't leak a voice app;
   - isshoni's own tree is always excluded.
3. It opens one `INCLUDE_TARGET_PROCESS_TREE` stream per allowed process. It skips processes already covered by an
   included ancestor (no double capture), and never includes a tree that contains any excluded process, playing or
   not.
4. The **mixer** pulls 10 ms from each stream on a high-resolution timer:
   - each stream buffers 40 ms before it starts playing;
   - every start and stop is a 10 ms fade, including when a stream is about to run dry and when latency above 100 ms
     is trimmed, so streams come and go without clicks.
   The output is 48 kHz stereo float, pulled by the host.
5. `im_audio_status` returns **"what friends hear"**: live streams with levels, excluded apps with the reason, and
   warnings.

`s1.exe` (Go, no cgo) loads the DLL. `selftest` uses **fake apps**, renamed copies of `s1.exe` started under
`explorer.exe` like normal apps. Each plays a pure tone:
- a "game" at 1031 Hz that friends *should* hear;
- "voice apps" at 453/571 Hz that they must *not* hear.

The frequencies sit between musical notes, so background music can't fake a result. The self-test records and measures
each frequency (Goertzel) and detects clicks. It also samples the engine's own status every second, to prove every fake
app actually played and was classified as expected. A test that can't prove that is reported as **INCONCLUSIVE**, never
PASS.

## Run it on Windows (≈5 minutes)

1. Copy **`dist/s1.exe`** and **`dist/isshoni_audio.dll`** into one folder, e.g. `C:\isshoni-s1`.
   - They're prebuilt from the Mac: `build/build-dll.sh`, and
     `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/s1.exe .`
   - To build on Windows instead, see "Build on Windows" below.
   - The files are unsigned: if Windows blocks them, right-click → Properties → **Unblock**, or "More info → Run
     anyway".
2. Before you start:
   - set the **Windows volume slider to 50% or more** (turn the speakers or headphones down physically instead);
   - set Sound settings → More sound settings → **Communications → "Do nothing"**, so opening a mic doesn't duck the
     other audio;
   - close or pause other apps that play sound.

   Then open **PowerShell** (normal, not admin) in that folder:

   ```powershell
   .\s1.exe probe        # Windows build + whether per-app loopback works
   .\s1.exe selftest     # ~2 min of beeps; PASS/FAIL/INCONCLUSIVE per scenario; writes s1-report.json
   ```

3. **Real apps** (the actual goal):
   - **Discord desktop:** open Settings → Voice & Video → *Mic Test* (Discord plays your own voice back), or join a
     call with a friend. At the same time play a YouTube video in Edge. Then:

     ```powershell
     .\s1.exe apps                    # every app playing audio, and whether friends would hear it
     .\s1.exe record -d 30 -o real.wav
     ```

     `record` prints "friends hear … / kept out …" every second. **Listen to `real.wav`:** YouTube should be there;
     Discord voices must not be.
   - **Discord Web:** Discord in Chrome while YouTube plays in Edge:
     `.\s1.exe record -exclude chrome.exe -o web.wav`. The whole of Chrome is kept out, as designed.
   - **TeamSpeak / Mumble**, if installed: same as Discord (they're on the built-in list).
4. Do steps 2–3 on **Windows 11** and on **Windows 10 22H2** (dual boot).
5. Send back both `s1-report.json` files, plus one line per real-app test ("heard YouTube, no Discord ✓" etc.).

## Pass criteria (plan S1)

| Scenario | Pass means |
|---|---|
| `endpoint-baseline` | Sanity check: plain loopback hears both tones. If this fails, the audio setup is the problem, not isshoni |
| `include-set` | Game (1031 Hz) present; **two** voice apps (453 and 571 Hz) at least 40 dB lower; no clicks |
| `exclude-one` | Single-EXCLUDE fast path keeps one voice app out |
| `exclude-one-limit` | INFO only: shows one EXCLUDE stream can't drop two apps (why the include set exists) |
| `helper-process` | Voice audio played by a *child* of the voice app is kept out |
| `app-boundary` | An app the voice app *launched* from a different folder (like a browser opened from a Discord link) is still heard |
| `self` | isshoni's own playback (907 Hz) never gets re-captured |
| `late-start` | Voice app started mid-share is kept out; a game started mid-share is heard within about 1.5 s; no clicks |
| `runtime-rule` | Adding an exclusion mid-share removes the app within about 1 s, without a click |
| `mic-user` | An unknown app holding the microphone is kept out automatically (skipped if there is no mic) |
| Real apps | Discord / Discord Web (whole browser) / TeamSpeak inaudible in `real.wav`; YouTube audible |

**INCONCLUSIVE** means the test couldn't prove its own premise, and the scenario is re-runnable with
`-only <name>`. Causes:
- a fake app didn't start or play;
- this program stalled;
- another app was playing.

Exit codes: 0 all passed, 1 something failed, 2 something was inconclusive.

**Gate:** if the include set fails on Windows 10 22H2, Windows 10 gets endpoint loopback only, and the plan says so.

## Results

Raw reports: [`results/`](results/).

| Machine | Build | Self-test | Real apps | Notes |
|---|---|---|---|---|
| Win 11 PC (run 1, 2026-09-29) | 26200 (25H2) | **7 PASS**, 1 INFO; include-set, exclude-one, helper-process, self, runtime-rule (no click), mic-user and endpoint-baseline pass. Excluded tones at −133 to −145 dB. app-boundary: *test fixture bug* (browser copy sat in a subfolder of the voice app, which the engine treats as the same app; fixed). late-start: clicks at 3.2 s / 6.13 s with **zero** engine underruns/trims/drops (re-run with the new plain-loopback control + event log) | **Discord desktop kept out, YouTube (Brave) heard** ✓ | Process loopback accepted plain float32 on the first try. Loopback levels are unaffected by master volume/mute (volume 19% and muted still gave −12 dB). Steam holds a mic session, so it's auto-excluded as a mic user (its own UI sounds only; games are separate apps) |
| AWS VM (run 2, 2026-09-29, via RDP "Remote Audio") | Server 2025, 26100 | **9 PASS**, 1 INFO, mic-user SKIP (no mic in RDP). app-boundary **passes** after the fixture fix. late-start: 1 click at 4.05 s, **not** present in the plain-loopback control. Diagnosis from the WAV: the new app's stream was silent for 30 ms after "running", then its tone appeared at full level, because Windows drops the first ms of an app that starts playing after the loopback attaches (its 20 ms fade-in never arrived). **Fix: onset fade** (5 ms ramp when audio starts after ≥50 ms of digital silence), not yet re-verified on Windows | n/a | Same exclusion quality as Win11 (−138 to −145 dB) |
| Win 10 22H2 | | | | |

## Build on Windows

- DLL (MSVC, the official toolchain): from a Developer PowerShell,
  `cmake -S . -B build/msvc -G "Visual Studio 17 2022" -A x64 && cmake --build build/msvc --config Release`.
- `s1.exe`: `go build -o s1.exe .`
- Put both files in one folder.

## Known spike limitations (fix in M2, not here)

- Poll-based session discovery: a new app is heard after 0.5 s to 1 s. Its first sound is lost, and an excluded app
  is never heard during this window, because exclusion is the default. M2 uses `IAudioSessionNotification`, keeping
  polling as a fallback.
- The mixer runs on the system clock (QPC), not the audio device clock. Slow drift eventually triggers a trim or
  underrun, i.e. a rare click. M2 adds adaptive resampling.
- Endpoint mode doesn't follow default-device changes.
- Output is PCM only. Opus encoding and the full `isshoni_media.h` ABI (with video) come in M2.
