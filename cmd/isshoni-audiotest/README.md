# isshoni-audiotest

The audio exit gate of M2 (design: `docs/m2/05-windows-audio.md` §18). It drives the Windows audio engine
(`isshoni-media.dll`) through the app's own audio policy service, plays tones from renamed copies of itself started
under `explorer.exe`, records the PCM tap (what the Opus encoder gets) and proves that friends hear every app except
those that must be kept out: voice apps, apps that hold the microphone, the browser that plays your own isshoni
viewer, and isshoni itself.

It is a test tool, not part of any installer. For a release candidate, `release.yml` builds it from the tag next to
the DLL and uploads both as the workflow artifact `isshoni-exit-tools`. The analyzer, the verdicts and the report are
portable and tested on every OS (`task test`); the cases run only on Windows with an audio endpoint.

## Run the gate (about 15 minutes per OS)

1. Download the `isshoni-exit-tools` artifact of the release candidate's `release.yml` run and unzip it to
   `C:\isshoni-audiotest`. Check that `isshoni-media.dll`'s SHA-256 equals the one in `native-manifest.json`. If
   SmartScreen blocks the program: Properties → Unblock, or "More info → Run anyway" (unsigned 0.x builds).
2. Close or pause every app that plays sound. **Quit Discord, TeamSpeak, Steam and any other voice app, tray icons
   included**: the fast-path cases (4, 5, 26) need that nothing but the test's own apps is kept out, and name what
   is still running. Connect a microphone and check Settings → Privacy & security → Microphone → "Let desktop apps
   access your microphone" is on. Recommended: Sound settings → More sound settings → Communications → "Do nothing".
3. In PowerShell (normal, not admin):

   ```powershell
   cd C:\isshoni-audiotest
   .\isshoni-audiotest.exe run -gate -out .
   ```

   Minor releases add the 30-minute soak: `run -gate -long -out .`.
4. The real-app checks (R1–R7 below), one `record` each:

   ```powershell
   .\isshoni-audiotest.exe record -label R1 -d 30 -o r1.wav
   ```

   It records for 30 s, printing every second what friends hear and what is kept out, then asks `Pass? [y/n]`.
   Listen to the WAV before you answer. The answer goes into `manual-R1.json` next to the WAV.
5. Copy the report JSON and the `manual-*.json` files to the development machine, merge them and commit the result
   (see "Record the results").

Before the first run on a new machine, `.\isshoni-audiotest.exe selfcheck` loads the DLL, checks every export, runs
the startup probe and a watcher start and stop, and tests the analyzer, all without sound. `apps` prints "What
friends hear" for the apps running now (names stay on the console), `probe` the engine's caps.

## Commands

| Command | What |
|---|---|
| `selfcheck` | Loads the DLL, checks `im_version` and every export, runs the probe, starts and stops the watcher, runs the analyzer on synthetic signals. No sound. Exit 0 or 1 (CI Windows runs it) |
| `probe` | Prints the caps, the startup probe's result, the ducking preference and the endpoint counts |
| `apps` | One-shot "What friends hear" with the built-in rules, as text on the console |
| `run [-case a,b] [-gate] [-long] [-keep-wav dir] [-out dir] [-no-redact]` | Runs the cases (all of them by default, by number or name with `-case`). `-gate` records the gate configuration; `-long` adds the soak; `-keep-wav` keeps each case's tap as a WAV (never commit them); `-out` is the folder for the report (default `.`); `-no-redact` is for local debugging only and writes `…-unredacted.json` |
| `record -label R1 [-d 30] [-o r1.wav] [-exclude exe,…]` | A real-app check: records the tap, prints friends hear / kept out every second, asks `Pass? [y/n]` and stores `{label, pass, os}` in `manual-<label>.json`. `-exclude` keeps more apps out for this recording |
| `merge [-o out.json] report.json manual-*.json` | Adds the manual entries to a gate report (in place, or to `-o`). Refuses an entry recorded on another OS build (product, build and UBR), a label already merged with the other answer, and a report written with `-no-redact` |
| `fixture <verb> …` | Internal: what a fixture copy runs |

Exit codes of `run`: **0** every required case PASS · **1** a FAIL · **2** no FAIL but an INCONCLUSIVE · **3** a
required case SKIPPED (the gate is incomplete; for example no microphone, or no render endpoint at all) · **4** a
setup error (DLL missing, wrong ABI). `record` exits 0 for pass, 1 for fail and 4 on an error or without an answer;
`merge` exits 0 when it merged and 4 when it refused.

## Cases

Every case records about 6–12 s after a 1 s settle (the soak 30 minutes). "Required" counts toward the exit code;
"fast path" cases count only when the release ships `audio.fastPath` on (the gate configuration), else they run as
INFO; "mic" cases SKIP without a microphone with desktop-app access. A failure in a required case is an engine bug:
it becomes a fix slice in `native/windows/src/audio/`, never a change of the harness.

| # | Case | What it proves | Required |
|---|---|---|---|
| 1 | `baseline-endpoint` | endpoint loopback hears a game and a voice app: the audio setup works (if not, every later case is INCONCLUSIVE) | yes |
| 2 | `builtin-voice` | a built-in voice app (Discord) is kept out, the game heard | yes |
| 3 | `two-voice-apps` | two voice apps kept out at once | yes |
| 4 | `fast-path` | with no voice app, one EXCLUDE stream on isshoni's tree; isshoni's own tone kept out | fast path |
| 5 | `fast-path-late-voice` | a voice app starting mid-share never leaks; session lag ≤ 20 ms, fast-path exit ≤ 15 ms | fast path |
| 6 | `helper-tree` | a voice app's grandchild helper and a shared-runtime helper are kept out | yes |
| 7 | `late-helper` | a helper spawned mid-share is kept out from its first sample | yes |
| 8 | `late-app` | an app starting mid-share is heard within 500 ms, without a click | yes |
| 9 | `steam-game` | a game in Steam's folder is heard while Steam, holding the mic, is kept out | mic |
| 10 | `isshoni-self` | isshoni's own playback and its WebView2 child are never captured | yes |
| 11 | `viewer-browser` | the browser playing your isshoni viewer is kept out whole | yes |
| 12 | `browser-mic` | a browser whose tab holds the mic is kept out whole and asked about | mic |
| 13 | `mic-user` | an unknown mic user is kept out and asked about; "include" fades it in within 500 ms | mic |
| 14 | `shared-app-mic` | the shared app is heard although it holds the mic, with a warning | mic |
| 15 | `runtime-rule` | a keep-out rule added mid-share cuts the app within 100 ms, without a click | yes |
| 16 | `app-boundary` | an app a voice app launched from another folder is still heard | yes |
| 17 | `tree-contains-excluded` | a launcher whose tree holds a voice app is not captured; the voice app kept out | yes |
| 18 | `orphan` | a helper whose parent exits mid-share stays kept out | yes |
| 19 | `probe-fallback` | a failed probe falls back to endpoint loopback with the banner, and really captures | yes |
| 20 | `timestamps` | tap onset times match the render clock (median ≤ 15 ms, spread ≤ 5 ms); Opus every 20 ms | yes |
| 21 | `opus` | the Opus stream carries the mix: 1000 Hz within 1.5 dB of the tap, 440 Hz absent, the bitrate change visible | yes |
| 22 | `multi-endpoint` | whether a tone on a second render endpoint is captured (SKIP with one endpoint) | INFO |
| 23 | `soak` | 30 minutes (`-long`): no underruns, trims, resyncs or clicks; resampler within 1000 ppm | minor releases |
| 24 | `viewer-browser-included` | the viewer rule beats a saved "include" for that browser | yes |
| 25 | `late-child-heard` | a heard app's late child in the same folder is covered by its stream | yes |
| 26 | `fast-path-late-child` | a late isshoni child stays inside the EXCLUDE stream | fast path |
| 27 | `pid-reuse-tree` | whether Windows follows a reused PID into a heard app's tree | INFO |

Tones: game 1000 Hz, voice 440, second voice 571, late game 683, browser tab 797, isshoni itself 1700 (05 §18.3 says
907; it moved at least 200 Hz from every other tone because a 1000 Hz onset leaks about −44 dBFS into 907 Hz, over
the −50 dBFS absent limit), the sound-check chime 880 + 1320, bursts 2000. All at −18 dBFS.

## Reports and redaction

`run` writes `audiotest-<win10|win11>-<build>-<YYYYMMDD-HHMM>.json` (schema v1, 05 §18.7) and prints one summary
line. The repository is public, so the report is redacted by default: no user, host, domain or device names;
endpoints only as `{formFactor, bus}`; paths relative to the fixtures' work folder (other paths become `<path>`, the
profile folder `<home>`); fixture apps as `fixture:<role>` and every other app as `other`, with no name; any app or
exe name the engine reported during the run, and the local user and host names, become `<name>` in free text. WAVs
are written only with `-keep-wav` or by `record`, and are never committed.

## Record the results

On the development machine, merge the manual entries into the report and commit it under `results/`:

```sh
isshoni-audiotest merge audiotest-win11-26200-20260101-1200.json manual-*.json
git mv … cmd/isshoni-audiotest/results/2026-01-01-win11-26200.json   # YYYY-MM-DD-<win10|win11>-<build>.json
```

Then add a row to the results table below: the date, the OS build with its UBR, the harness and DLL versions, the
summary line's counts, the manual checks and any notes. Never commit a `-no-redact` report (`merge` refuses one) or a
WAV.

### Manual real-app checks (05 §18.9)

| Id | Steps | Pass | OS |
|---|---|---|---|
| R1 | Discord desktop in a call (or Settings → Voice & Video → Mic Test), YouTube in Edge at the same time; `record -label R1 -d 30` | YouTube audible, no Discord voice; Discord row `kept_out`/`builtin` | Win11, Win10 |
| R2 | Discord Web in Chrome in a voice channel with the mic allowed, YouTube in Edge; open the app's share sheet | the "Your browser is using the mic" prompt; Chrome kept out; Edge heard in `record -label R2` | one per gate, both at minor releases |
| R3 | A game started from the Steam library; `record -label R3 -d 30` (once with an anti-cheat-protected title if available) | the game heard; Steam's own sounds follow the mic setting | Win11, Win10 |
| R4 | The real app: share a window with sound while watching a friend's share with sound | the friend doesn't hear their own audio; the isshoni row `kept_out` | Win11, Win10 |
| R5 | During a share with a game and Discord in a call: plug or unplug a headset, or change the default output | the game heard again within 2 s; Discord absent; `reconnecting-sound` gone within 3 s | Win11, Win10 |
| R6 | TeamSpeak 3/5/6 and Mumble, if installed, as R1 | voices absent | minor releases |
| R7 | Each app below with YouTube in Edge; `record -label R7-<app> -d 30` | the app's voice absent, YouTube audible, its row `kept_out`/`builtin` | once on Win11 before `v0.2.0`, again when its row changes |

R7 apps (an app not run yet stays "unverified"; a failure fixes the app's rule, never removes it):

| App | Label | Status | Report |
|---|---|---|---|
| New Teams ("Meet now" or a test call) | `R7-teams` | unverified | – |
| Zoom (test meeting) | `R7-zoom` | unverified | – |
| Slack (huddle) | `R7-slack` | unverified | – |
| Vesktop (Discord call) | `R7-vesktop` | unverified | – |

### Results

| Date | OS build | Harness | DLL | Summary | Manual | Notes |
|---|---|---|---|---|---|---|
| – | – | – | – | no gate run recorded yet | – | – |
