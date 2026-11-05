# M2 exit test: run book

The checklist for the M2 exit run (README WA90). It writes out the outline of [README §6](../README.md) with the exact
commands. The specs are the source of truth for their parts; where this page and a spec disagree, the spec wins and
this page gets fixed:

- the audio half: [05 §18.8–§18.10 and §19.3](../05-windows-audio.md) (the harness's own page is
  [`cmd/isshoni-audiotest/README.md`](../../../cmd/isshoni-audiotest/README.md));
- the latency half: [01 §12.2](../01-client-core.md) (the tool is [`tools/g2g`](../../../tools/g2g/main.go), whose
  package comment has every flag) with the component checks of [04 §15 and §18.4–§18.5](../04-windows-engine.md);
- the release-candidate checks: [04 §18.4](../04-windows-engine.md), [03 §22.5](../03-desktop-app.md),
  [02 §16.4](../02-device-auth.md) and [06 §11.8](../06-windows-release.md).

**Exit criterion (plan, README §1):** on one release candidate, the audio half and the latency half both pass, on
Windows 11 (24H2 or later) and on Windows 10 22H2. On a pass, WA91 follows: the release-prep PR, the `v0.2.0` tag,
then the SignPath application (06 §11.8 item 14, §12.3).

**What goes into the repository:** the record `docs/m2/exit-test/results/<yyyy-mm-dd>.md`, made from
[`results-template.md`](results-template.md), with its JSON files in `docs/m2/exit-test/results/<yyyy-mm-dd>/`, and
the two audiotest reports under `cmd/isshoni-audiotest/results/`. The repository is public: no PC names, user names,
server domains or IP addresses, in the record or in any file next to it, and no screenshots that show them. WAVs,
`g2g -dump` crops, `-no-redact` reports and the downloaded tools never go into the repository.

**Time:** about 15 minutes per OS for the audio half (A), 25 for the latency half (B), and an hour or two for the
release-candidate checks (C), most of them once. A comes before B on each OS (01 §12.2 step 10: A's case 20 checks
the audio timestamps that B's lip-sync hold depends on).

Commands for the PC are PowerShell (Windows PowerShell 5.1 is enough), in a normal, not elevated, window. Commands
for the development machine run in the repository's checkout. `<N>` is the release candidate's number, `<rc server>`
the rc server's address (it never goes into a file), `<date>` the run's date as `yyyy-mm-dd`.

## 1. Pass rules

The run passes when every row below holds, **both halves on both Windows versions, on one release candidate**
(README §6): every automated result (A-1, A-2, A-7, the `g2g` runs) comes from that rc. A new rc repeats what §7
says; manual checks that carry over from an earlier rc name it in the record.

**Audio** (05 §18.10; README §3 item 52 for Windows 10):

| # | Rule | Win11 (24H2 or later) | Win10 22H2 |
|---|---|---|---|
| A-1 | `isshoni-audiotest run -gate` with the rc's `isshoni-exit-tools` | exits **0** | its exit code doesn't count: the reduced form, cases 1 (`baseline-endpoint`) and 19 (`probe-fallback`) **PASS** |
| A-2 | the report's `dll.sha256` equals the rc's `native-manifest.json` `dll.sha256` | yes | yes |
| A-3 | the banner `exclusion-unavailable` ("Voice apps can't be kept out on this PC …") shows in the app's What friends hear during a share with sound | – | yes |
| A-4 | R1, R3, R4 and R5 (05 §18.9) | pass | not required (the endpoint engine can't keep anything out; R3 runs on Win11 only) |
| A-5 | R2 | pass (Win10 can't take it, so Win11 is the "at least one") | not required |
| A-6 | R7: once for each app of 05 §18.9 the owner can run | pass; the others stay "unverified" in the harness README's R7 table | – |
| A-7 | `isshoni-enginetest avsync` | exit 0: \|median A/V offset\| ≤ 20 ms over ≥ 15 pairs, with WGC (`-source pick`) | the same, with Desktop Duplication (`-source` the monitor's id as `sources` prints it, `mon:\\.\DISPLAYn`) |
| A-8 | both reports, with their manual entries merged, committed under `cmd/isshoni-audiotest/results/`, a row in the harness README's results table; both `avsync.json` in the record | yes | yes |

**Latency** (01 §12.2, D17; README §7 question 1's default reading):

| # | Rule | Win11 | Win10 22H2 |
|---|---|---|---|
| L-1 | the `gate` run of `g2g measure` (Share sound on, Chrome's viewer audio subscribed, content cuts on) exits **0**: p95 **< 200 ms** over 60 s with ≥ 90 % of the screenshots decoded | borderless WGC monitor share | Desktop Duplication monitor share |
| L-2 | the conditions of that run: a 1080p monitor at 60 Hz (the JSON's `screen` and `refreshHz`), a hardware encoder on the full layer (Details), the full layer at 8 Mbit/s before measuring, server RTT ≤ 50 ms (the JSON's `rttMs`; g2g warns above 50 ms, and such a run doesn't count) | yes | yes |
| L-3 | the `nocuts` and `nosound` runs are made and recorded, with the encoder, GPU and driver, the RTT, the versions, the share's CaptureToSend p50/p95 and `isshoni-enginetest bench`'s capture-to-packet p95 (04 §15: ≤ 25 ms with a hardware encoder) | recorded, not gating | recorded, not gating |

Exit codes of `g2g measure`: 0 pass, 1 the latency failed, 2 inconclusive (too few screenshots decoded, interrupted,
or the run couldn't measure). Only the `gate` run decides; a 2 is repeated, never counted as a pass.

**Release-candidate checks** (README §6 C; 06 §11.8 item 6 asks for 04's three on the rc installer): M-WDA, M-DD and
M-WGC-WIN (04 §18.4, §18.5), M-DESK-1, 2, 3 and 8 (03 §22.5) and M-DA-1, 2 and 7 (02 §16.4) pass as their specs say.
`protect-check`'s WGC path is recorded as it ran: a WGC outcome `not_reachable` is written "WGC not covered on this
PC", **never as a WGC pass** (README §7 question 11, undecided; 04 §14). 06 §11.8 items 7–11 belong to WA91; their
results go into the release-prep PR, and the record lists them only if they were run on the same day.

## 2. Preconditions

### 2.1 The release candidate

- [ ] `v0.2.0-rc.<N>` is tagged and its `release.yml` run finished green, including `desktop-pkg` (which uploads
  `isshoni-exit-tools`) and `verify-windows` (06 §11.1, §11.4). Note the run's id: the number at the end of its URL,
  `…/actions/runs/<run id>`.
- [ ] The rc server is M1's exit-test server (or another isshoni server with RTT ≤ 50 ms from the PC, 01 §12.2 step 1)
  running **the same rc**. On the server, the installer of that release upgrades in place (M1 run book §1.2):

  ```sh
  curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.2.0-rc.<N>/install.sh | sh -s -- --domain <rc server>
  ```

  Then `https://<rc server>/api/v1/info` shows the rc's version (g2g records it too).
- [ ] On each Windows install, the app comes from the rc server: open `https://<rc server>/download` in Edge, copy the
  command the page displays (06 §7.5's `ps1Command`, `install-desktop.ps1`) into PowerShell and run it; link the app to
  your account (02's device flow) when the wizard asks. Check the installed version:

  ```powershell
  & "$env:LOCALAPPDATA\Programs\isshoni\isshoni.exe" --version | Write-Output
  ```

  It prints one JSON line whose `version` is `0.2.0-rc.<N>`.

### 2.2 The PC (each Windows install)

- Windows 11 24H2 or later (build 26100 or higher), and Windows 10 22H2 (build 19045); `winver` shows the build.
- A GPU with a hardware H.264 encoder (NVENC, QSV or AMF) on a current driver. Note the GPU and the driver version.
- One 1080p monitor, or a 1080p primary monitor of a larger setup (g2g captures the primary monitor), at 60 Hz.
- A microphone, with Settings → Privacy & security → Microphone → "Let desktop apps access your microphone" on
  (the mic cases SKIP without it, so `run -gate` would exit 3).
- Sound settings → More sound settings → Communications → **"Do nothing"** (recorded by the harness, 05 §18.4).
- Installed: Discord desktop (signed in), Chrome, Edge, Steam with one game; for R7, any of new Teams, Zoom, Slack
  and Vesktop the owner can run.
- Nothing else playing sound. Before A's `run -gate`, **quit** Discord, TeamSpeak, Steam and every other voice app,
  tray icons included (the fast-path cases 4, 5 and 26 name what still runs); start them again for the real-app
  checks.

### 2.3 The tools: `isshoni-exit-tools` and the `ci\` DLL

1. In the browser (signed in to GitHub), open the rc's `release.yml` run (its run id from §2.1) → **Artifacts** →
   download **`isshoni-exit-tools`** (kept 90 days; a workflow artifact, not a release asset, 06 §11.1). It holds the
   tag's `isshoni-audiotest.exe`, `isshoni-enginetest.exe`, `g2g.exe` and `hwcheck.ps1`, the rc's own
   `isshoni-media.dll` and FFmpeg DLLs, `native-manifest.json` and a `README.txt` naming the run and the tag.
2. Unzip it and unblock the files (unsigned 0.x tools; SmartScreen would stop each one otherwise):

   ```powershell
   Expand-Archive -Path "$HOME\Downloads\isshoni-exit-tools.zip" -DestinationPath C:\isshoni-audiotest
   cd C:\isshoni-audiotest
   Get-ChildItem -Recurse | Unblock-File
   Get-Content .\README.txt
   ```

   `README.txt` must name `v0.2.0-rc.<N>`.
3. **Hash check**: every DLL's SHA-256 against `native-manifest.json` (`desktop-pkg` checked the same before the
   upload; this checks the download):

   ```powershell
   $m = Get-Content -Raw .\native-manifest.json | ConvertFrom-Json
   $want = @{}
   $want[[string]$m.dll.name] = [string]$m.dll.sha256
   foreach ($p in $m.ffmpeg.dlls.PSObject.Properties) { $want[$p.Name] = [string]$p.Value }
   foreach ($name in $want.Keys) {
     $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $name).Hash.ToLowerInvariant()
     if ($got -ceq $want[$name]) { "OK    $name $got" } else { "FAIL  $name $got (manifest: $($want[$name]))" }
   }
   "manifest version: $($m.version)"
   ```

   Three `OK` lines (`isshoni-media.dll` and the two FFmpeg DLLs) and the version `0.2.0-rc.<N>`; anything else
   stops the run (download again; if it repeats, the rc is broken).
4. **The `ci\` DLL for `avsync`**: `avsync` needs 04 §5.8's luma tap, a test hook that only the `win-x64-ci` build
   has (the release DLL has none and fails the start with `dependency`, exit 2). It comes from CI, not from the
   release: find the commit of the rc on the development machine with `git rev-list -n 1 v0.2.0-rc.<N>`, open the
   `ci.yml` run of that commit's push to `main` (Actions → CI → filter by branch `main`), and download its
   artifact **`isshoni-native-tools`** (kept 7 days: when it has expired, re-run that run's `windows-native` job, which
   uploads it again). Copy its `ci` folder, with the test-hooks `isshoni-media.dll` and the FFmpeg DLLs next to it,
   into the tools folder:

   ```powershell
   Expand-Archive -Path "$HOME\Downloads\isshoni-native-tools.zip" -DestinationPath C:\isshoni-native-tools
   Copy-Item -Recurse C:\isshoni-native-tools\ci C:\isshoni-audiotest\ci
   Get-ChildItem -Recurse C:\isshoni-audiotest\ci | Unblock-File
   ```

   Only `avsync` uses `ci\` (`-dll ci`); everything else uses the rc's DLL next to the tools.
5. Once per Windows install, the harness loads the rc's DLL and checks every export, without sound:

   ```powershell
   .\isshoni-audiotest.exe selfcheck
   .\isshoni-audiotest.exe probe
   ```

   `selfcheck` ends with `selfcheck: ok` (exit 0).

## 3. A. Audio (about 15 minutes per OS; 05 §18.8)

Windows 11 first, then Windows 10 22H2. In `C:\isshoni-audiotest`:

1. Quit every voice app and Steam (§2.2), pause everything that plays sound, then run the gate:

   ```powershell
   .\isshoni-audiotest.exe run -gate -out .
   $LASTEXITCODE
   ```

   It writes `audiotest-<win10|win11>-<build>-<yyyyMMdd-HHmm>.json` and prints one summary line ending in
   `gate=PASS` or `gate=FAIL`. Exit codes: 0 every required case PASS, 1 a FAIL, 2 INCONCLUSIVE (no FAIL: usually
   something else played sound; find the cause and run again), 3 a required case SKIPPED (no microphone access or no
   render endpoint), 4 a setup error. Check the report's DLL hash against the manifest (rule A-2):

   ```powershell
   $report = Get-ChildItem audiotest-win*-*.json | Sort-Object LastWriteTime | Select-Object -Last 1
   $r = Get-Content -Raw $report.FullName | ConvertFrom-Json
   $m = Get-Content -Raw .\native-manifest.json | ConvertFrom-Json
   "$($report.Name): dll $($r.dll.sha256) manifest $($m.dll.sha256) equal=$($r.dll.sha256 -ceq $m.dll.sha256)"
   $r.cases | Where-Object { $_.name -in 'baseline-endpoint', 'probe-fallback' } | Format-Table name, result
   ```

   **Windows 10 22H2** (reduced form): the two cases shown must be `PASS`; the exit code is recorded, not judged.
   Then start the app, share a window **with sound**, and check that What friends hear shows the banner
   `exclusion-unavailable` ("Voice apps can't be kept out on this PC, so friends hear everything it plays …");
   stop the share (rule A-3). The real-app checks below are Windows 11's (rules A-4 to A-6).
2. **Real-app checks on Windows 11** (05 §18.9), one `record` each. Each records the PCM tap for 30 s while printing
   what friends hear and what is kept out every second, then asks `Pass? [y/n]`; listen to the WAV before answering.
   The answer goes into `manual-<label>.json` next to the WAV (`record` exits 0 pass, 1 fail, 4 an error):

   | Check | Set up first | Command | Pass |
   |---|---|---|---|
   | R1 | Discord desktop in a call (or Settings → Voice & Video → Mic Test), YouTube playing in Edge | `.\isshoni-audiotest.exe record -label R1 -d 30 -o r1.wav` | `r1.wav`: YouTube audible, no Discord voice; Discord row `kept_out`/`builtin` |
   | R2 | Discord Web in Chrome, in a voice channel with the mic allowed; YouTube in Edge; open the isshoni app's share sheet | `.\isshoni-audiotest.exe record -label R2 -d 30 -o r2.wav` | the "Your browser is using the mic" prompt appeared; Chrome kept out; Edge heard |
   | R3 | a game started from the Steam library (once with an anti-cheat-protected title, if there is one) | `.\isshoni-audiotest.exe record -label R3 -d 30 -o r3.wav` | the game heard; Steam's own sounds follow the mic setting |
   | R4 | in the isshoni app, share a window with sound while watching a friend's share with sound; the friend (B in the record) listens | `.\isshoni-audiotest.exe record -label R4 -d 30 -o r4.wav` during it; answer from the friend's report | the friend doesn't hear their own audio; the isshoni row `kept_out` in the app |
   | R5 | a share with a game playing and Discord in a call; during the recording unplug or plug a headset, or change the default output in Sound settings | `.\isshoni-audiotest.exe record -label R5 -d 30 -o r5.wav` | the game heard again within 2 s; Discord stays absent; `reconnecting-sound` gone within 3 s |
   | R7 | for each app the owner can run (new Teams "Meet now" or a test call, Zoom's test meeting, a Slack huddle, Vesktop in a Discord call), with YouTube in Edge | `.\isshoni-audiotest.exe record -label R7-teams -d 30 -o r7-teams.wav` (labels `R7-teams`, `R7-zoom`, `R7-slack`, `R7-vesktop`) | the app's voice absent, YouTube audible; its row `kept_out`/`builtin` (new Teams: every process that plays) |

   Quit the call apps afterwards. Delete the WAVs when the record is written; they never go into the repository.
3. **`avsync`** (05 §19.3, 04 §14; rule A-7), with the `ci\` DLL. Nothing else may play sound; the tool starts a
   flash window (black, white for 100 ms once a second) and a click process under Explorer and captures 25 s.

   Windows 11 (WGC): pick the window `isshoni-enginetest avsync flash` in the OS picker when it opens.

   ```powershell
   .\isshoni-enginetest.exe -dll ci avsync -source pick -out avsync-win11
   $LASTEXITCODE
   ```

   Windows 10 22H2 (Desktop Duplication): `sources` prints one line per monitor, starting with its source id
   (`mon:\\.\DISPLAYn`); pass that first field as it is to `-source` (the flash window opens centered on that
   monitor).

   ```powershell
   .\isshoni-enginetest.exe -dll ci sources
   .\isshoni-enginetest.exe -dll ci avsync -source <that id> -out avsync-win10
   $LASTEXITCODE
   ```

   Each prints every pair and ends with `<n> pairs: median offset … ms`; exit 0 is the pass (≥ 15 pairs,
   \|median\| ≤ 20 ms), 1 a failed assertion, 2 the environment (a DLL without the luma tap, a cancelled pick, no
   audio device), 64 a usage error. The result is `avsync-win11\avsync.json` (or `avsync-win10\…`); it names no
   machine, window or path. `-no-luma` is only a diagnostic of the Windows half and never a pass.
4. **Copy to the development machine**: the report `audiotest-…json`, every `manual-*.json` and
   `avsync-<win11|win10>\avsync.json`, into the git-ignored `.dev/exit-test/<date>/<win11|win10>/` (any way: a USB
   stick, a network share).
5. **Merge and commit the report** (05 §18.8 step 5), on the development machine, in the checkout at the rc's commit
   or later (`merge` is portable and refuses entries of another OS build and `-no-redact` reports):

   ```sh
   d=.dev/exit-test/<date>/win11      # then win10
   go run ./cmd/isshoni-audiotest merge "$d"/audiotest-win11-*.json "$d"/manual-*.json
   mkdir -p cmd/isshoni-audiotest/results
   cp "$d"/audiotest-win11-<build>-*.json cmd/isshoni-audiotest/results/<date>-win11-<build>.json
   ```

   `<build>` is the OS build as in the report's name. Windows 10 has no manual entries: copy its report as it is
   (`<date>-win10-<build>.json`). Read each report before committing it (the redaction's placeholders show as they
   are). Add a row per OS to the results table of `cmd/isshoni-audiotest/README.md` (date, OS build with UBR, harness
   and DLL versions, the summary line's counts, the manual checks, notes; Windows 10: "reduced form, cases 1 and 19")
   and update its R7 table for each app run. Copy each `avsync.json` to the record's folder as
   `docs/m2/exit-test/results/<date>/avsync-<win11|win10>.json`.

**With a registered runner** (05 §18.8; README §7 question 6; the runner's setup is
[`deploy/windows/runner/README.md`](../../../deploy/windows/runner/README.md)): the gate run and the copy steps can
come from [`audiotest.yml`](../../../.github/workflows/audiotest.yml) instead. Actions → Audiotest → Run workflow
from `main`, `os` = `win11` (or `win10`), `suite` = `release`, `release_run_id` = the rc's `release.yml` run id; its
`fetch` job checks the DLLs against the manifest, `run` waits for the owner's approval, runs `run -gate` and uploads
the redacted report. The real-app checks and `avsync` stay by hand as above, and their `manual-*.json` are merged into
the downloaded report the same way.

## 4. B. Latency (about 25 minutes per OS; 01 §12.2)

After A, on the same rc and the same Windows install. One monitor; everything below is on it.

1. The app is linked to the rc server, nothing else plays sound, and the PC has one monitor at 1080p60 (or a 1080p60
   primary one). The RTT needs no separate step: each `g2g measure -server` records it (`rttMs`, rule L-2).
2. In one PowerShell window, serve the clock page and leave it running:

   ```powershell
   cd C:\isshoni-audiotest
   .\g2g.exe serve
   ```

   It serves `http://127.0.0.1:8765/`. Open that page in **Edge** on the shared monitor, click **Start** (the page
   shows `tone on` and the 440 Hz tone plays), then press **F11** (full screen). The counter line shows
   `cuts on`.
3. In the isshoni app: **Share** → the OS picker → **that monitor** (Windows 11: borderless WGC; Windows 10: Desktop
   Duplication), preset **Auto**, **Share sound on** (the default, Exclude apps). Open **Details** (Ctrl+Shift+D)
   and wait until the full layer shows 1920 × 1080 @ 60 at about 8 Mbit/s (about 14 s after the share goes live).
   Note the encoder and that it is hardware.
4. In **Chrome** (not the isshoni app, whose windows are kept out of capture), signed in as **the same user**, open
   the room. Click the share to focus it, then its **speaker** button (your own share is never made audible by
   focus, and its audio must be subscribed for Chrome to lip-sync). If Chrome shows "Now watching in the isshoni app",
   click **Watch here instead**. Place the Chrome window over the bottom-right half of the monitor at 50 % size,
   leaving the clock's strip at the top-left visible. Nested copies of the page inside the viewer are expected; g2g
   takes the largest. If g2g can't find the copy later (the video sits on a hardware overlay that GDI doesn't see),
   close Chrome and start it with:

   ```powershell
   & "$env:ProgramFiles\Google\Chrome\Application\chrome.exe" --disable-direct-composition-video-overlays
   ```

5. In the app's **What friends hear**: Edge **heard**, Chrome **kept out** as your viewer. If Chrome is heard, stop:
   the share would feed its own sound back. Then move the app's window and the pill off the clock's strip and off
   Chrome's copy (or minimize the window; the share keeps running).
6. **The `gate` run** (rule L-1): in a second PowerShell window, kept small over the bottom-left of the monitor so
   that it covers neither strip:

   ```powershell
   cd C:\isshoni-audiotest
   .\g2g.exe measure -d 60s -run gate -server https://<rc server> -o g2g-win11-gate.json
   $LASTEXITCODE
   ```

   (On Windows 10, `g2g-win10-gate.json`, and likewise below.) The first 2 s calibrate, then it measures for 60 s.
   Exit 0 is the pass, 1 a latency failure, 2 inconclusive (repeat the run; `-dump dump-gate` saves PNG crops of the
   strips for diagnosis, which stay off the repository).
7. **The diagnostic runs** (rule L-3; not gating, they tell a miss's cause):
   - `nocuts`: in Edge, open `http://127.0.0.1:8765/?cuts=0`, click **Start**, press **F11** (the counter line shows
     `cuts off`), then:

     ```powershell
     .\g2g.exe measure -d 60s -run nocuts -server https://<rc server> -o g2g-win11-nocuts.json
     ```

   - `nosound`: stop the share and start a **new** one of the same monitor with **Share sound** unchecked in the share
     sheet (turning sound off mid-share is not the same: the mixer keeps sending silent frames and Chrome keeps
     syncing to them). Reload the default page `http://127.0.0.1:8765/` (cuts on), **Start**, **F11**. In Chrome
     click the new share to focus it, wait for the full layer at 8 Mbit/s in Details, then:

     ```powershell
     .\g2g.exe measure -d 60s -run nosound -server https://<rc server> -o g2g-win11-nosound.json
     ```

8. **Component numbers** (04 §15), next to the runs: the full layer's CaptureToSend p50/p95 from Details during the
   `gate` run (the share's latency figure), and, after stopping the share, the engine bench (30 s of a 1080p60 test
   pattern):

   ```powershell
   .\isshoni-enginetest.exe bench
   ```

   Record its `capture → packet p50 … p95 … p99 …` line and the encoder; with a hardware encoder on `f`, p95 must be
   ≤ 25 ms (a miss there locates a latency miss, it doesn't gate by itself). Stop `g2g serve` with Ctrl+C.
9. **Versions**: `.\g2g.exe version`; the app's `--version` line (§2.1); the OS build (`winver`); the GPU, driver
   and encoder. Each `g2g-*.json` already carries g2g's, Go's, Windows' and the server's versions and the RTT, and
   names no host, user, machine or path.
10. Copy the three `g2g-<win11|win10>-*.json` files into `docs/m2/exit-test/results/<date>/` on the development
    machine.

On a `gate` miss, the diagnostic runs show where it went: `gate` − `nosound` is the lip-sync hold, `gate` − `nocuts`
the content cuts; 01 §12.1's levers apply in their order (the stamps first, which A's case 20 checks).

## 5. C. Release-candidate checks (on the same build)

Each on the rc installer from §2.1 (after a new rc, the rows §7 names are repeated; the others carry over), on the
OS the row names; the steps are the specs' own (open them next to this page). The tools are the rc's, from
`C:\isshoni-audiotest`.

| Check | OS | What to do | Pass |
|---|---|---|---|
| M-WDA (04 §18.4) | both | in the app, share the full screen with the isshoni window on top and a share-sheet dropdown open; watch from a phone | the isshoni window (its WebView2 content included) and the pill never appear in the stream (WGC on Win11, DD on Win10) |
| M-DD (04 §18.4) | Win10 | `.\isshoni-enginetest.exe capture -source pick -seconds 30 -out m-dd`, pick a monitor; move the pointer over text and a link; open a UAC prompt; change the resolution; rotate the display if possible | capture path DD with `MAPPED`; no border; pointer shapes drawn; `CAPTURE_INTERRUPTED`/`RESUMED`, the UAC prompt never captured; re-init at the new size; upright output |
| M-WGC-WIN (04 §18.4) | Win11 | `.\isshoni-enginetest.exe capture -source pick -seconds 30 -out m-wgc-win`, pick a browser window; drag-resize it, minimize and restore it, then close it | no yellow border; cursor visible; letterboxed during the drag, `f.h264` changes size about 1 s after; `SOURCE_MINIMIZED`/`RESTORED`; `ENDED{SOURCE_GONE}` |
| `protect-check` (04 §14) | both | `.\isshoni-enginetest.exe protect-check` with a still desktop; when the picker opens, pick the monitor the noise window was on | exit 0; DD `pass`; WGC as it ran: `pass`, or `not_reachable` recorded as **"WGC not covered on this PC"** (never a WGC pass) |
| M-DESK-1 (03 §22.5) | Win11 | from a room's "Get the desktop app" (`/download?room=<id>`): (a) `install-desktop.ps1`, (b) the downloaded installer then **Open isshoni**, (c) the installer with "Start isshoni" checked, as 03 §22.5 lists | each: the room within 2 minutes, no server URL or password typed |
| M-DESK-2 (03 §22.5) | Win11 | share a monitor (borderless); watch from a phone | the pill visible locally, absent from the stream; the isshoni window absent; the pill drags and collapses; Narrator reads its buttons |
| M-DESK-3 (03 §22.5) | both | in a borderless full-screen game: Pause, Resume, then Stop hotkeys | "Paused" with the last frame and silence within 1 s (a phone joining while paused sees the Paused tile); resume within 1 s; stop ends the share |
| M-DESK-8 (03 §22.5) | Win10 | share a monitor (DD) and a window (WGC) | monitor: no border, pill shown and checked against the stream; window: yellow border, no pill by default |
| M-DA-1 (02 §16.4) | Win11 | install, first run, "Link this app" | the browser opens `/link?code=…` with the app's code; "Linked" within 10 s; Credential Manager lists `isshoni:v1:https://<server>` |
| M-DA-2 (02 §16.4) | Win11 | restart Windows | the app starts linked without a prompt; the Devices page shows "last seen just now" |
| M-DA-7 (02 §16.4) | Win11 | uninstall keeping the data, reinstall, link, uninstall with "remove my data"; repeat with the network cable unplugged | online: each time the Credential Manager entry is gone and the device leaves the Devices page; offline: the uninstaller finishes within 30 s and the entry is gone (the device stays on the Devices page, 02 §16.4) |

06 §11.8 items 7–11 (install paths, the update path from rc to rc, uninstall, the relink check, the previous release
against the rc server) are WA91's release checklist; run them on the same rc when convenient and record them in the
release-prep PR.

Optionally, `powershell -NoProfile -ExecutionPolicy Bypass -File .\hwcheck.ps1 -Out .` gives 04 §18.4's automated
bundle (probe, captures per hardware encoder, bench, sessions, `protect-check -wgc off`) for the release-prep PR;
it redacts the PC's names itself.

## 6. D. Record

On the development machine:

1. Copy [`results-template.md`](results-template.md) to `docs/m2/exit-test/results/<date>.md` and fill in every row.
2. Next to it, `docs/m2/exit-test/results/<date>/` holds the six g2g files (`g2g-win11-gate.json`,
   `g2g-win11-nocuts.json`, `g2g-win11-nosound.json` and the three `g2g-win10-*.json`) and `avsync-win11.json` and
   `avsync-win10.json`. The two audiotest reports are under `cmd/isshoni-audiotest/results/`; the record names their
   paths.
3. Before committing, search everything you add for the PC's name, your user name, the server's domain and any IP
   address (each one in turn):

   ```sh
   grep -rniF '<one name>' docs/m2/exit-test/results cmd/isshoni-audiotest/results
   ```

   No match may remain; replace it with a role (A, B, "the rc server") or remove the file.
4. The record, the JSON files and the audiotest reports go into WA90's PR; the release-prep PR (WA91) links them
   (06 §11.8 items 5–6).

## 7. E. On a failure

- A failure becomes a fix slice (`WF…`) and a **new rc**; nothing is patched on the PC. A failed required audiotest
  case is an engine bug in `native/windows/src/audio/`, never a change of the harness (05 §18.5).
- Every new rc repeats, on both OSes, A's `run -gate` (rules A-1, A-2) and `avsync` (A-7), because the DLL changes
  with every rc (it carries the rc's version and commit, so an older report's `dll.sha256` never equals the new
  `native-manifest.json`), plus all of B. The real-app checks R1–R5 and R7 (A-4 to A-6) and A-3's banner are repeated
  only after a change to `native/`, `internal/media/` or `internal/desktop/audio`; otherwise they carry over, and the
  record names the rc they ran on. C rows are repeated where the change can affect them. Every automated result in
  the record comes from the one rc that passed; carried-over manual checks name their rc.
- When both halves pass on both OSes: WA91 (the release-prep PR, tag `v0.2.0`, then the SignPath application).
