# M2 exit test: <yyyy-mm-dd>

<!-- Copy this file to docs/m2/exit-test/results/<yyyy-mm-dd>.md and fill in every row (run book: README.md in
     docs/m2/exit-test/). The JSON files go into docs/m2/exit-test/results/<yyyy-mm-dd>/. The repository is public:
     no PC names, user names, server domains or IP addresses; people are A (the owner) and B (a friend) only. -->

- **Release candidate:** `v0.2.0-rc.<N>` (commit `<short hash>`), `release.yml` run `<run id>`
- **`isshoni-exit-tools`:** `README.txt` names `v0.2.0-rc.<N>`; DLL hash check: `isshoni-media.dll` and both FFmpeg
  DLLs equal `native-manifest.json` (yes / no); `native-manifest.json` `dll.sha256` = `<sha256>`
- **`ci\` DLL for `avsync`:** `isshoni-native-tools` of the `ci.yml` run `<run id>` on the rc's commit
- **rc server:** M1's exit-test server / another server (no address here), version `<from /api/v1/info>`
- **Manual checks carried over from an earlier rc** (run book §7): none / `<rows>` from `v0.2.0-rc.<M>`
- **Result:** PASS / FAIL (<the rows that failed>)

## The PC

| | Windows 11 | Windows 10 22H2 |
|---|---|---|
| OS build (with UBR) and edition | | |
| GPU, driver version | | |
| Encoder on the full layer (Details), hardware | | |
| Monitor: the gate JSON's `screen` and `refreshHz` (must be 1920 × 1080, 60) | | |
| App `--version` (`version`, `commit`) | | |
| Communications set to "Do nothing" | | |
| Microphone with desktop-app access | | |

## A. Audio

| # | Check | Windows 11 | Windows 10 22H2 |
|---|---|---|---|
| pre | `isshoni-audiotest selfcheck` ends with `selfcheck: ok` (a precondition, run book §2.3 step 5) | | |
| A-1 | `run -gate -out .`: exit code and summary line counts (`n/n PASS · FAIL · INCONCLUSIVE · SKIP · INFO`) | exit …, … | exit … (not judged), … |
| A-1 | Win10 reduced form: case 1 `baseline-endpoint`, case 19 `probe-fallback` | – | 1: …, 19: … |
| A-2 | report `dll.sha256` equals the manifest's | | |
| A-3 | banner `exclusion-unavailable` shown during a share with sound | – | |
| A-4 | R1 (Discord desktop) | | not required |
| A-4 | R3 (Steam game; protected title used: yes / no) | | not required |
| A-4 | R4 (the friend B doesn't hear their own audio) | | not required |
| A-4 | R5 (device change: game back within 2 s, Discord absent, `reconnecting-sound` gone within 3 s) | | not required |
| A-5 | R2 (Discord Web in Chrome: prompt, Chrome kept out, Edge heard) | | not required |
| A-6 | R7-teams | pass / fail / unverified | – |
| A-6 | R7-zoom | pass / fail / unverified | – |
| A-6 | R7-slack | pass / fail / unverified | – |
| A-6 | R7-vesktop | pass / fail / unverified | – |
| A-7 | `avsync -dll ci` (Win11 `-source pick`, WGC; Win10 `-source` the monitor's id from `sources`, DD): exit code, pairs, median offset (ms), p5/p95 (ms) | exit …, … pairs, … ms (…/…) | exit …, … pairs, … ms (…/…) |
| A-8 | committed report | `cmd/isshoni-audiotest/results/<yyyy-mm-dd>-win11-<build>.json` | `cmd/isshoni-audiotest/results/<yyyy-mm-dd>-win10-<build>.json` |
| A-8 | `avsync.json` | `results/<yyyy-mm-dd>/avsync-win11.json` | `results/<yyyy-mm-dd>/avsync-win10.json` |
| A-8 | row added to `cmd/isshoni-audiotest/README.md`'s results table (and its R7 table) | | |

How the gate run was made: by hand / `audiotest.yml` (suite `release`, run `<run id>`).

## B. Latency

Each `g2g` run: `.\g2g.exe measure -d 60s -run <run> -server https://<rc server> -o g2g-<os>-<run>.json`.

| Run | | Windows 11 (borderless WGC) | Windows 10 22H2 (Desktop Duplication) |
|---|---|---|---|
| `gate` (sound on, cuts on) | exit code, result | | |
| | p50 / p95 / max (ms) | | |
| | decoded % (≥ 90) | | |
| | RTT `rttMs` (≤ 50) | | |
| | screen, `refreshHz`, copy scale | | |
| `nocuts` (diagnostic) | exit code, p50 / p95 / max (ms), decoded % | | |
| `nosound` (diagnostic) | exit code, p50 / p95 / max (ms), decoded % | | |
| Lip-sync hold | `gate` p95 − `nosound` p95 (ms) | | |
| Content cuts | `gate` p95 − `nocuts` p95 (ms) | | |
| Component | Details during `gate`: full layer CaptureToSend p50 / p95 (ms) | | |
| Component | `isshoni-enginetest bench`: encoder, capture → packet p50 / p95 / p99 (p95 ≤ 25 ms) | | |
| Versions | `g2g version` (g2g, Go, Windows); server version in the JSON | | |
| Files | in `results/<yyyy-mm-dd>/` | `g2g-win11-gate.json`, `g2g-win11-nocuts.json`, `g2g-win11-nosound.json` | `g2g-win10-gate.json`, `g2g-win10-nocuts.json`, `g2g-win10-nosound.json` |
| What friends hear before `gate` | Edge heard, Chrome kept out | | |

## C. Release-candidate checks

| Check | Windows 11 | Windows 10 22H2 |
|---|---|---|
| M-WDA: the isshoni window and the pill never in the stream | | |
| M-DD: DD with `MAPPED`, no border, pointer shapes, UAC never captured, re-init, upright | – | |
| M-WGC-WIN: no border, cursor, letterbox then resize, minimize/restore events, `ENDED{SOURCE_GONE}` | | – |
| `protect-check`: exit code; DD outcome | | |
| `protect-check`: WGC outcome (`pass` / `fail` / **WGC not covered on this PC** for `not_reachable`; never a pass) | | |
| M-DESK-1 (a), (b), (c) | | – |
| M-DESK-2 | | – |
| M-DESK-3 | | |
| M-DESK-8 | – | |
| M-DA-1 | | – |
| M-DA-2 | | – |
| M-DA-7 (online, then offline) | | – |
| 06 §11.8 items 7–11 (WA91; in the release-prep PR) | run / not run today | run / not run today |
| `hwcheck.ps1` bundle (optional) | attached to the release-prep PR / not run | attached / not run |

## Pass rules (README.md §1)

| Rule | Result |
|---|---|
| Audio, Win11: A-1 exit 0, A-2, A-4, A-5, A-6 (apps run), A-7, A-8 | |
| Audio, Win10 22H2: A-1 reduced form, A-2, A-3, A-7, A-8 | |
| Latency, Win11: `gate` exit 0 with L-2's conditions; `nocuts` and `nosound` recorded | |
| Latency, Win10 22H2: the same | |
| C rows pass on this rc, or carried over with their rc named (WGC not covered is recorded, not passed) | |
| Every automated result (A-1, A-2, A-7, `g2g`) from this one rc; carried-over manual checks name their rc | |

## Notes

<Anything unusual: repeated runs and why (an inconclusive g2g run, an INCONCLUSIVE audiotest case and what was
playing), a fix slice and the rc it led to, deviations from the run book.>
