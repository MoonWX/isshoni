# M3 design 05: Exit and verification (macOS 14.4 through 27)

How M3 is verified and when it is done: the exit gate exactly, the machines and macOS versions it runs on, the tools'
macOS halves (`isshoni-audiotest`, `isshoni-enginetest`, `tools/g2g`), what GitHub's macOS runners run and what they
skip (and how the skip says why), what the development Mac runs with its capture grants, the real-app end-to-end test
on WKWebView, the Wails checklist rows for macOS, the manual matrix (`M-MAC-*`), budgets and soak, the exit run book's
outline and record, and the risks.

Inputs: `docs/PLAN.md` (owner-approved: "Decisions", "Desktop clients" incl. "Engines per OS", "Audio exclusion rules"
with "Verified on macOS 27", "Share safety", "Media and quality", "Privacy and trust model", "Install and
distribution", "Build, CI and release", "Verification", the M3 roadmap entry and "Main risks"); the M1 and M2 records
(`docs/m1/README.md`, `docs/m2/README.md` §3–§6) and specs `docs/m2/01`–`06` (mainly 01 §6.4, §12; 03 §3.2, §20, §22;
04 §14–§15, §18; 05 §18–§19; 06 §10–§11); the M2 exit run book `docs/m2/exit-test/README.md` and its template; the code
on `main` (`tools/g2g`, `cmd/isshoni-enginetest`, `cmd/isshoni-audiotest`, `internal/media/{native,fake,mediatest}`,
`cmd/isshoni-desktop` with its `isshoni_hw` checklist test and e2e flavor, `web/e2e/cdp`, `.github/workflows/ci.yml`
jobs `desktop-macos` and `native-host`); the M0 spikes `spikes/s2-mac-audio` (12/12 on macOS 27, grants kept across
self-signed rebuilds 1→9) and `spikes/s2-mac-vt` (1440p60 NV12 with low-latency RC, p99 7.6–10.2 ms; fails without it).

The sibling M3 docs are written in parallel; this doc names them by file and states the interface it assumes (§14):
- `01-desktop-macos.md`: the Wails app on darwin (OS hooks, menu bar item, Carbon hotkeys, own-window exclusion and its
  fallback, Keychain, deep links, `SMAppService`, lock/sleep, the e2e flavor on darwin), the Wails checklist's macOS
  results;
- `02-macos-engine.md`: the static cgo + Objective-C engine behind C ABI v1 (SCContentSharingPicker, SCK capture,
  VideoToolbox, the test-hooks build), `internal/media/native/engine_darwin.go`;
- `03-macos-audio.md`: the Core Audio process tap, rules, mic detection, sound check, the audiotest's darwin driver,
  fixtures and case table;
- `04-macos-release.md`: the self-signed identities, signing, DMG, `install-desktop.sh`, Homebrew tap, updater,
  `release.yml`/`ci.yml` build and package jobs.

**Tags.** Everything here is **M3** unless marked *later (M4/M5)*. "Mac (local)" is the development Mac (Apple silicon
M5, macOS 27, bare metal; macOS 26, its shipping version, on a second APFS volume, §3.1); "VM" is a macOS guest on that
Mac (Virtualization.framework, §3.2); "CI macos" is a GitHub-hosted runner (§5.3); "rc" is a release candidate
`v0.3.0-rc.<N>`.

---

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| V-D1 | **The exit gate is M2's two halves, per macOS version**: audio (audiotest gate, voice apps ≥ 40 dB below, real-app checks, `avsync`) and latency (`g2g` p95 < 200 ms at 1080p60 with sound and content cuts), on **one rc**, plus the release-candidate checks (§2) | Roadmap: "the same checks on 14.4 through 27" as M2's exit |
| V-D2 | **macOS 27 and 26 run on bare metal** (the M5: 27 on its main volume, 26 on a **required** second APFS volume); **14.4 and 15 run in VMs** on the M5 (Tart from Apple's IPSW restore images; UTM works the same). A 26 VM is kept as well, for the C rows that need a fresh TCC database per run (`-clean` clones) | Apple silicon Macs can't boot a macOS older than the one they shipped with (the M5 shipped with 26, so 26 boots on bare metal and 14/15 don't); VMs are the only 14.4/15 the owner has (S2's "still open"). 26 is the version most users run, and SCK/VT behaviour (LLRC, frame delivery) can differ between 26 and 27, so it gets real-hardware numbers |
| V-D3 | **The latency half (L-1, L-2) gates on bare metal: 26 and 27 on the M5.** On 14.4 and 15 (VMs) it is replaced by a functional share check (L-F) and recorded numbers that never gate | A guest has a paravirtual GPU and display and (expected, V1 records it) no hardware H.264 encoder: its g2g number measures the VM, not isshoni. This narrows "the same checks on 14.4 through 27" for 14.4 and 15 only; the owner signs it off at the start (§16 Q1, before MA01 merges), not at the end. An LLRC probe (`enginetest probe` on a 14.x/15 Mac) is recorded if a borrowed or community Mac happens to be available; it is not required |
| V-D4 | **Audio gate, `avsync` and the grant checks run on all four versions**; in a VM `avsync` uses the test-hooks fake encoder | Taps, TCC and SCK capture times don't depend on the encoder; the A/V stamps are what `avsync` checks |
| V-D5 | **Tests that need a TCC grant run inside a signed harness app**, never from a terminal: `isshoni Dev Harness.app` (dev identity, `task mac:hw`) and `isshoni Exit Harness.app` (the **test-tools identity** `isshoni-testtools-1`, rc exit tools; never the release identity, §4.1) | The grant goes to the harness's designated requirement, which survives rebuilds (S2: builds 1→9, no second prompt), instead of to Terminal, which would give every shell capture rights. A separate stable identity keeps the release certificate's Authority on `io.isshoni.desktop` only, and keeps the `-granted` golden images valid across rc builds |
| V-D6 | **A grant is declared, not probed**: `ISSHONI_TEST_TCC=screen,audio,mic,localnet`. Undeclared → the test SKIPs with `macenv: needs tcc:<grant>`; declared but not granted → the test FAILs | CI never touches a capture API (no prompt can hang a job); on the Mac a lost grant (a signing regression) is a failure, not a quiet skip |
| V-D7 | **Every macOS skip is in a ledger** (`.github/macos-skips.txt`) checked by `tools/skipcheck` | M2's lesson: ctest counts an all-skipped binary as passed (README §4); an unlisted skip fails the job |
| V-D8 | **The real-app e2e on WKWebView uses an in-app driver** compiled only into the `-tags e2e` darwin build (loopback HTTP, token, `ExecJS` + an e2e-only bound method), not Safari's Web Inspector | Playwright's WebKit is its own build and can't attach to a system WKWebView; `safaridriver` drives Safari only; Web Inspector's remote protocol isn't an automation API on macOS. The driver's key events also go through AppKit, unlike CDP's renderer-level keys (M2 PR #56's lesson) |
| V-D9 | **`tools/g2g` gets a darwin capturer** (SCStream of the main display at 20 fps, cgo, `darwin && cgo`); the Windows build stays `CGO_ENABLED=0` | The method (clock page and its viewer copy in one screenshot) is the M2 method; only the screenshot API changes. SCK composites overlays, so Chrome's overlay flag isn't needed |
| V-D10 | **CI macOS images**: everything is **built once on `macos-26`** (newest SDK; 04's `desktop-macos`, 02's `macos-native`, 04's `macos-update`); the WK spec runs on the e2e app in `macos-wk` (`macos-15` and `macos-26`); launch checks of that build run in 04's `macos-runtime` (`macos-14`, `macos-15`, `macos-15-intel`) and this doc's `macos-compat` (`macos-14`: the gtest and `go test -c` binaries). The x86_64 slice's tests run under Rosetta in nightly (02 §6.7: `mac-x64-ci` ctest and the amd64 Go tests), and the slice launches natively in `macos-runtime`'s `macos-15-intel` leg | 02/03 need SDK 26 (`CATapDescription.bundleIDs`), which only `macos-26`'s Xcode has by default; 14 is where weak linking breaks first; Intel runners end with `macos-15-intel` (GitHub's schedule), then Rosetta only |
| V-D11 | **Intel: built, unit-tested under Rosetta in nightly, launched natively on `macos-15-intel` (04), smoke-tested under Rosetta on the M5; not gated on Intel hardware** (none available); community test reports | The plan keeps the Intel slice in one universal binary; VMs can't run x86_64 macOS. Owner question §16 Q3 |
| V-D12 | **A self-hosted runner on the M5 is optional** (`machw.yml`, `workflow_dispatch` only, environment approval); the gate's path is the owner running `task mac:hw` and the run book | Mirrors M2's audiotest.yml; a public repo never routes PR code to the owner's machine |
| V-D13 | **The record lives in `docs/m3/exit-test/`** (run book `README.md`, `results-template.md`, `vm-lab.md`, `results/<date>.md` + `results/<date>/`), audiotest reports under `cmd/isshoni-audiotest/results/` | Same shape as M2's record, so the release-prep PR links it the same way |

---

## 2. The exit criterion, exactly

**Pass**: on one rc (`v0.3.0-rc.<N>`, built by `release.yml`, installed through the rc server's `/download` page with
`install-desktop.sh`), every row below holds on the version columns it names. Every automated result comes from that
rc; a manual check carried over from an earlier rc names it (§9.5).

### 2.1 Audio (03-macos-audio's gate; the plan's "1 kHz present and 440 Hz at least 40 dB lower")

| # | Rule | 14.4 (VM) | 15 (VM) | 26 (M5, 2nd volume) | 27 (M5) |
|---|---|---|---|---|---|
| A-1 | `harness.sh isshoni-audiotest run -gate -out <dir>` exits **0**: every case that 03's darwin table marks required PASSes (pass rule: 03 §16.6, which is M2 05 §18.6 with case 15's budget at 150 ms on bare metal and 150 ms + one recorded guest IO buffer (`ioBufferMs`) in a VM, five runs each meeting it (03 §16.5): present ≥ −45 dBFS; absent ≤ −50 dBFS, ≤ P_min − 40 dB and every 50 ms window ≤ −50 dBFS; no clicks). The 44.1 kHz path is case 33 (`device-rate-change`), required unless it SKIPs because the device offers no 44.1 kHz. **Exit 3 is accepted on a VM** (14.4/15) when the only SKIPs are 03's mic cases (9, 12, 13, 13v, 14) for lack of a guest input device and the 27 run has every mic case PASS (03 §16.10 item 1); those SKIPs are listed in the record | ✓ | ✓ | ✓ (exit 0) | ✓ (exit 0) |
| A-2 | The report's `engine.commit` is the rc's commit, `engine.testHooks` is false, and `tool.sha256` equals the `isshoni-audiotest` line of the exit tools' `SHA256SUMS` (the engine is static, so the tool's hash replaces M2's `dll.sha256`) | ✓ | ✓ | ✓ | ✓ |
| A-3 | 03's **M-R8** (`tccutil reset AudioCapture io.isshoni.desktop`; wizard: deny, then grant in System Settings): denial shows banner `audio-permission` (`params.cause` `denied`) and the sound check returns `reason: permission_denied`, silence is sent (never everything); the grant restores sound (with or without restart, recorded). In the 14.4 VM the wizard and sound-check half runs there; the banner during a live share runs on 27 (a VM share has no video, §2.2 L-F) | ✓ (wizard, sound check) | – | – | ✓ |
| A-4 | 03's **M-R1** (Discord desktop, Mic Test or a call; YouTube in Safari; `record -label MR1`): Discord absent, YouTube heard; Discord row `kept_out`/`builtin` | ✓ | ✓ | ✓ | ✓ |
| A-5 | 03's **M-R2** (Discord Web in Chrome with the mic) and **M-R10** (tap pre-volume; gates the VolumeNote) on 27; **M-R4** (isshoni self: a friend doesn't hear their own audio) on 27 and 14.4 | M-R4 | – | – | M-R2, M-R4, M-R10 |
| A-6 | 03's **M-R5** (default output changes mid-share: built-in → AirPods → built-in): the game heard again within 2 s, Discord stays absent, `reconnecting-sound` gone within 3 s. 14.4 runs it with a Multi-Output Device when 03's M-R5 OS column lists 14.4 | per 03's M-R5 column | – | – | ✓ |
| A-7 | *Removed*: the 44.1 kHz path is case 33 inside A-1 (id kept so later ids stay stable) | – | – | – | – |
| A-8 | 03's **M-R7**: once for each of Teams, Zoom, Slack, Vesktop the owner can run | – | – | – | ✓ (others "unverified") |
| A-9 | `harness.sh isshoni-enginetest-hooks avsync -source pick` (§4.3): exit 0, \|median A/V offset\| ≤ 20 ms over ≥ 15 pairs | ✓ `-encoder fake` | ✓ `-encoder fake` | ✓ VideoToolbox | ✓ VideoToolbox |
| A-10 | Reports merged and committed (`cmd/isshoni-audiotest/results/<date>-macos<major>-<build>[-vm].json`), a row in the harness README's results table; every `avsync.json` in the record | ✓ | ✓ | ✓ | ✓ |

The manual real-app checks are 03-macos-audio's **M-R1…M-R10** (03 §16.9), recorded with `record -label MR<n>`; 03's
table is the source of truth for their setup and OS columns. M-R3 (a Steam game, if the owner has one), M-R6
(TeamSpeak/Mumble, minor releases) and M-R9 are recorded, not gating.

### 2.2 Latency (M2 01 §12.2's method, darwin capture, §4.2)

| # | Rule | 14.4 / 15 (VM) | 26 and 27 (M5, bare metal) |
|---|---|---|---|
| L-1 | `harness.sh g2g measure -d 60s -run gate -server https://<rc server> -o g2g-macos<26\|27>-gate.json` (Share sound on, Chrome's viewer audio subscribed, content cuts on) exits **0**: p95 **< 200 ms** over 60 s, ≥ 90 % of screenshots decoded | – | ✓ |
| L-2 | Conditions of that run: the shared display at **60 Hz** (`refreshHz` 60: ProMotion set to 60 Hz, or an external 60 Hz monitor); the full layer at the **1080p pixel budget** at 60 fps (Details: 1920 × 1080 on a 16:9 display; about 1786 × 1160 on the built-in 3024 × 1964 display, 02 §8.7's scale rule; layer `f`'s size read from Details and stats) and about 8 Mbit/s; a **hardware** VideoToolbox encoder (Details); server RTT ≤ 50 ms (`rttMs`); on AC power with Low Power Mode off (Safari throttles `requestAnimationFrame` to 30 fps in Low Power Mode); Chrome's output on the built-in speakers or a wired device (never Bluetooth, which adds 150–250 ms to Chrome's lip-sync hold) | – | ✓ |
| L-3 | The `nocuts` and `nosound` runs, `enginetest bench -source mon:<main>` at 1080p60 (02 §14: capture → packet p95 ≤ 25 ms, layer `f` encode p99 ≤ 16 ms with VideoToolbox) and the share's CaptureToSend p50/p95 recorded | – | recorded, not gating |
| L-F | Functional check in the VM. The release engine never opens a software encoder (02 §5.4, §9.10: `IM_ERR_NO_ENCODER` `no_hardware`), so: (1) the installed app's Share shows 01's "This Mac can't encode video in hardware" and the share doesn't start; (2) the video path is checked with `harness.sh isshoni-enginetest-hooks capture -source pick -encoder fake` (the pick, SCK capture and the session, verified by the luma tap), exit 0; (3) the audio path with `audiotest`'s audio-only session (A-1). If V1 finds a hardware VT encoder in a guest, L-F there is the full share instead: the app shares the VM's display with sound to a Chrome viewer on the host, which decodes frames and hears the tone, and `g2g measure`'s JSON (`machine.virtual` true) is recorded, never judged | ✓ each | – |

`g2g measure` exit codes are M2's: 0 pass, 1 latency failure, 2 inconclusive (repeated, never counted).

### 2.3 Release-candidate checks (on the same rc)

| # | Rule | 14.4 | 15 | 26 | 27 |
|---|---|---|---|---|---|
| C-1 | `harness.sh isshoni-enginetest-hooks protect-check -sck pick -json` (§4.3.2) on a display pick: the `outcome` equals the strategy that 02 §8.8's filled-in table (EM9) names for that OS version: `pass-filter` (strategy A or B), `pass-sharingtype` (C, 14.x) or `fallback-needed` (`SELF_VISIBLE`; then M-MAC-6's fallback steps prove auto-minimize and mute). It fails on `fail` (noise visible while the engine claimed exclusion) or on an outcome that differs from the table. Also on the `macos15-0` VM (15.0/15.1), where the fallback is the designed outcome | ✓ | ✓ (+ 15.0/15.1) | ✓ | ✓ |
| C-2 | M-MAC-1 (curl install from `/download?room=`), M-MAC-2 (DMG + Open Anyway), M-MAC-4 (grants and the 30 s watchdog), M-MAC-5 (picker), M-MAC-6 (own windows), M-MAC-8 (**update N → N+1 keeps every grant**), M-MAC-23 (run from the mounted DMG), M-MAC-22 (App Management at update and reinstall; 15, 26 and 27 only), and 01's WM23 (WebRTC in WKWebView, playback hidden and minimized) and WM24 (each System Settings URL of 01 §9.4 opens its pane), by hand as 01 §3.2 says. In VMs each row runs its grant, picker and update-keeps-grants parts; "goes live and decodes at the viewer" runs on 26 and 27 bare metal (§2.2 L-F). On 26 the rows that need a first run (M-MAC-1, 2, 4, 8, 22, 23) run in a `-clean` 26 VM clone and the live half on the 26 volume | ✓ | ✓ | ✓ | ✓ |
| C-3 | M-MAC-16 (Local Network grant on 15+, kept across the update) | – | ✓ | ✓ | ✓ |
| C-4 | M-MAC-3 (Homebrew), M-MAC-11 (autostart) | ✓ | – | – | ✓ |
| C-5 | M-MAC-7, 9, 10, 12, 13, 14, 15, 17, 19, 20 | – | – | – | ✓ |
| C-6 | The WK spec (§6) green on the rc's commit in `ci.yml` (`macos-wk`, `macos-15` and `macos-26`) | CI | CI | CI | CI |

**Exit = A (all rows) + L-1/L-2 on 26 and 27 (bare metal) + L-F on the 14.4 and 15 VMs + C**, all on one rc. On a pass:
the release-prep PR, the `v0.3.0` tag (04-macos-release's checklist).

---

## 3. Machines and macOS versions

### 3.1 What runs where

| Machine | macOS | Runs | Gates |
|---|---|---|---|
| Mac (local): MacBook Pro M5, bare metal | 27 | everything: `task mac:hw` (§5.4), the exit tools, L-1, real apps, multi-display, sleep, Rosetta smoke | A, L-1, C on 27 |
| Mac (local), second APFS volume (**required**) | 26 (latest 26.x; the M5's shipping version) | A, L-1, L-2, L-3 (recorded), C-1, the live halves of C-2/C-3, its own `task mac:hw` run before the exit run (the dev harness granted once there); about an hour per minor release | A, L-1, C on 26 |
| VM on the M5 | 14.4.x, 15.0 or 15.1 (`macos15-0`), 15 (latest), 26 (latest) | 14.4/15: A, L-F, C rows of §2; 26: the first-run C rows (fresh TCC per clone); `macos15-0` runs M-MAC-5, M-MAC-6 and C-1 only (the identity-less, pre-15.2 picker path) | A, L-F, C on 14.4/15; C on 26 |
| CI macos (GitHub-hosted, §5.3) | 26 (arm64: every build), 15 and 26 (WK spec), 14 (arm64), 15 (x86_64) (launch checks) | build, unit, conformance (fake engine), WK spec, launch checks | required checks of `ci-ok` |
| Owner's Windows PC | – | the M2 jobs that prove Windows unchanged (§5.7); a Chrome/Edge viewer for M-MAC rows when handy | – |
| Phone (iPhone) | – | viewer for own-window and Pause rows | – |
| Other Macs / community | any 14.4+ incl. Intel | the community test report (§7, M-MAC-17) | no |

### 3.2 The VM lab (Virtualization.framework on Apple silicon)

**Tool**: Tart (CLI, `--from-ipsw`, APFS clones for golden images; a host-only tool, never shipped or linked) or UTM
(Apache-2.0, GUI, `utmctl`). Both use Apple's Virtualization.framework; the run book gives Tart's commands.

```sh
tart create macos14 --from-ipsw <Apple's 14.4.x UniversalMac restore image URL> --disk-size 80
tart set macos14 --cpu 4 --memory 8192 --display 1920x1080
tart clone macos14 macos14-clean          # golden image: first-run rows start from a clone of it
tart run macos14-clean --dir=exit:<host folder with the exit tools>   # the folder appears under /Volumes/My Shared Files/exit
```

Likewise `macos15`, `macos26` from the latest 15.x and 26.x restore images, and `macos15-0` from a 15.0 or 15.1 restore
image (before 15.2: no picked-content identity, `IM_OS_SCK_PICK_IDENTITY` off, strategy B unavailable, so 02 expects the
`SELF_VISIBLE` fallback there); its build is recorded in `vm-versions.json`. Each VM gets two golden images: `-clean`
(installer defaults, no isshoni, no grants; for M-MAC-1/2/4/8) and `-granted` (the exit harness granted once, Discord
installed and signed in, Steam not needed). **Each golden image has one admin and one standard account** (04 owner
action 3). M-MAC-1, M-MAC-8 and M-MAC-22 run once as the standard user on at least one VM: the script installs to
`~/Applications` (`/Applications` isn't writable for that user), and an admin-installed copy in `/Applications` run by
the standard user shows the updater's `not_writable` reason ("isshoni can't update itself here. Install your own copy:
open your server's Download page and run its Terminal command", 04 §7.2); the script run by that user over the admin's
copy installs to `~/Applications`, prints the note naming the admin's copy, and an `isshoni://` link then opens the
user's copy (04 §6.3 step 4, M-MAC-1). A run always starts from a fresh clone; the clone is deleted after the record is
written.

**Limits and what works** (expected values; slice V1 records the real ones per guest version in
`docs/m3/exit-test/vm-lab.md`, and the design holds either way, §11):

| Capability | In a macOS guest | Consequence |
|---|---|---|
| Guest versions | macOS 12+ on Apple silicon; older than the host is fine. A restore image must still be personalized by Apple for VMs: if 14.4.x isn't, use the oldest 14.x that is and record its build | 14.4 is the floor the plan names; V1 records which 14.x the lab really has, and first of all whether each version boots on the M5 at all (unmeasured so far; §11 R3 if not) |
| Concurrency | at most 2 macOS guests at once (Apple's licence and the framework's limit) | run the VMs one or two at a time |
| Display | one virtual display, set to 1920 × 1080 (Tart `--display`), fixed 60 Hz, paravirtual Metal GPU | the 1080p60 share has the exact pixel budget; timing is the VM's |
| ScreenCaptureKit and SCContentSharingPicker | expected to work (the guest's own WindowServer) | picker, own-window and `protect-check` rows run in VMs |
| VideoToolbox hardware H.264 | expected **absent** (no media engine passthrough; hardware-required sessions fail with `kVTCouldNotFindVideoEncoderErr` −12908) | L-1 never in a VM (V-D3: 26 and 27 on bare metal); `avsync -encoder fake`; L-F per §2.2 |
| Core Audio process taps | expected to work: the guest has a virtio sound device (output, and input from the host's microphone, which the host tool asks for once) | A-1 in VMs; if V1 finds no usable output device or silent taps in a guest version, §11 R3 applies |
| Microphone | the host's mic through virtio (expected; V1 records it) | the audiotest's mic cases (9, 12, 13, 13v, 14) run; if a guest has no input device, A-1's exit-3 rule applies (those cases SKIP there, all PASS on 27) |
| TCC | the guest's own database: fresh in every `-clean` clone | first-run, grant and update-keeps-grants rows are real per version |
| Local Network (15+) | NAT network: the host's LAN address is "local network" to the guest | M-MAC-16 runs in the 15 and 26 VMs (26 also on the bare-metal volume) |
| Apple ID, App Store | not needed; Discord and Chrome are installed from their sites | – |
| Sleep, lid, multi-display, AirPods | not available | those rows run on the M5 only |

---

## 4. The tools on macOS

All three tools keep their M2 command lines, exit codes and JSON; macOS adds files behind build constraints. None is
part of the app or any installer.

### 4.1 The harness apps

**Why**: TCC attributes a capture to the responsible process. A tool started from Terminal is Terminal's; started
through LaunchServices from a signed bundle it is its own (S2: `run-selftest.sh` used LaunchServices for exactly this).

**Bundles** (built by `tools/macharness`, a darwin-only Go package in the tools module):

| | Dev harness | Exit harness |
|---|---|---|
| Name | `isshoni Dev Harness.app` | `isshoni Exit Harness.app` |
| `CFBundleIdentifier` | `io.isshoni.dev.harness` | `io.isshoni.exitharness` |
| Signed with | the dev identity (04's `deploy/macos/dev-signing.sh`, S2's script productized), by `deploy/macos/sign.sh -mode dev` | the **test-tools identity `isshoni-testtools-1`** (a stable self-signed code-signing identity made like 04 §5.1's, used for nothing else), by `sign.sh -mode testtools` in 04's `macos-sign-tools` job: its `.p12` is the secret pair `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` in its own environment `testtools`, **outside the `release` environment**; that job holds no other secret and runs no built code. The release identity signs only `io.isshoni.desktop` |
| Contents/MacOS | `harness` (launcher), `go test -c` binaries of the `isshoni_hw` packages (§5.2), the native hw gtest binary, `isshoni-audiotest`, `isshoni-enginetest`, `isshoni-enginetest-hooks`, `g2g` | `harness`, `isshoni-audiotest`, `isshoni-enginetest`, `isshoni-enginetest-hooks`, `g2g` |
| Contents/Resources | `fixtures/*.app` (03's audiotest fixtures), `avsync/AVSync Flash.app`, `avsync/AVSync Click.app` | the same |
| Built where | the Mac (`task mac:harness SIGN=dev`) | 04's `macos-app` builds it unsigned (`task mac:harness SIGN=none`), `macos-sign-tools` signs it, `macos-pkg` assembles and checks the artifact (§4.5) |

Info.plist (both): `CFBundleExecutable` `harness`, `LSMinimumSystemVersion` `14.4`, `LSUIElement` `true`,
`NSAudioCaptureUsageDescription` ("Records system audio to check what friends hear."),
`NSMicrophoneUsageDescription` (fixtures only), `NSLocalNetworkUsageDescription` ("Checks connections to servers on
your network.").

**Building and signing.** `task mac:harness [VERSION=] [SIGN=dev|ci|none] [OUT=]` builds the **unsigned** bundle with
`tools/macharness` (it never signs) and then, unless `SIGN=none`, calls 04's `deploy/macos/sign.sh -mode <SIGN>` with
the harness bundle (04 §5.4's tool form, `-tool <bundle> -tool-id <id>`), the only signer. What gets signed is
committed and reviewable so `macos-sign-tools` runs no Go:
- `tools/macharness/sign-list.txt`: one line per nested item, innermost first: `<relative path> <identifier>
  <entitlements file or ->` (every Mach-O in `Contents/MacOS`, each fixture and avsync helper bundle, then the
  harness bundle itself);
- `tools/macharness/harness.entitlements`: exactly `com.apple.security.device.audio-input` (the app's entitlement,
  so TCC treats the harness as it treats the app);
- `tools/macharness/fixture.entitlements`: `com.apple.security.device.audio-input`, which also covers 03's fixtures
  that hold the microphone.
Hardened runtime (`--options runtime`) everywhere; nested items first, then the bundle (no `--deep`). Release CI
builds with `SIGN=none` in `macos-app`; `macos-sign-tools` signs the bundle from the list. The test-tools certificate
(no private key) is committed as `deploy/keys/isshoni-testtools-1.pem`; `repocheck keys` checks it with the codesign
`.pem` rules (RSA ≥ 3072, self-signed, code-signing EKU, CA:false) but outside the codesign active/backup pair, and its
SHA-256 fingerprint is printed in the artifact's `README.txt` (§4.5) and in `docs/m3/exit-test/README.md`, so a
tester can compare `codesign -dvvv` of the harness with it.

**The launcher** `harness [-status FILE] [-env K=V]… -- <tool> [args…]`: resolves `<tool>` in `Contents/MacOS`
(nothing else; a path is refused), sets the `-env` variables, `posix_spawn`s it with stdio inherited and
`posix_spawnattr_setbinpref_np` set to the launcher's own CPU type (so a launcher started as x86_64 runs the tool's
x86_64 slice), waits, writes the exit code as decimal text to `-status`, and exits with it. Exit 64 for a usage error.

**`harness.sh`** (POSIX sh, next to the bundle; `open -W` doesn't return the child's status, so the file carries it):

```sh
#!/bin/sh
# harness.sh <tool> [args...]: runs <tool> inside the harness app, attached to this terminal.
# harness.sh --cleanup: removes the harness's and its fixtures' TCC grants (run when finished; §4.1).
set -eu
app="${ISSHONI_HARNESS_APP:-$(cd "$(dirname "$0")" && pwd)/isshoni Exit Harness.app}"
if [ "${1:-}" = "--cleanup" ]; then
  id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")
  for svc in ScreenCapture AudioCapture; do echo "tccutil reset $svc $id"; tccutil reset "$svc" "$id" || true; done
  for f in "$app"/Contents/Resources/fixtures/*.app; do
    [ -d "$f" ] || continue
    fid=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$f/Contents/Info.plist")
    echo "tccutil reset Microphone $fid"; tccutil reset Microphone "$fid" || true
  done
  echo "done: now delete this folder"
  exit 0
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
t=$(tty)
arch_opt=""
[ -n "${ISSHONI_HARNESS_ARCH:-}" ] && arch_opt="--arch $ISSHONI_HARNESS_ARCH"   # x86_64: Rosetta
# shellcheck disable=SC2086
open -W -n $arch_opt -a "$app" --stdin "$t" --stdout "$t" --stderr "$t" --args -status "$tmp/status" \
  -env "ISSHONI_TEST_TCC=${ISSHONI_TEST_TCC:-}" -env "PWD=$PWD" -- "$@"
exit "$(cat "$tmp/status" 2>/dev/null || echo 70)"
```

`ISSHONI_HARNESS_ARCH=x86_64` is how the x86_64 slice runs under Rosetta (M-MAC-17): `arch -x86_64 harness.sh`
would only change `sh`, because `open` launches the bundle natively; `open --arch x86_64` starts the launcher's
x86_64 slice, and the launcher's binpref passes it on to the tool.

The launcher `chdir`s to `PWD` so `-out .` means the terminal's folder. Interactive prompts (`record`'s `Pass?
[y/n]`) read the terminal. Exit 70 means the harness never wrote a status (it was killed or didn't start).

**Grants**: the first run of a capture prompts once per grant for the harness (Screen Recording on 14.x/"Screen &
System Audio Recording" on 15+; System Audio Recording; Local Network on 15+). Fixture apps that hold the microphone
prompt once for Microphone. Reset for a first-run test: `tccutil reset ScreenCapture io.isshoni.dev.harness`,
`tccutil reset AudioCapture io.isshoni.dev.harness`. On 15+ a harness that captures listed sources gets macOS's
periodic "continue to allow" reminder; the app itself never does, because it only uses the picker (M-MAC-5 checks
that).

**What a granted harness allows.** Once a harness (dev or exit) holds Screen Recording and System Audio Recording,
**any process of that user can make it capture the screen and system audio without a picker and without a prompt**:
the launcher runs any bundled capture tool (`enginetest capture -source mon:<id>`, `g2g`, `audiotest record`) with any
arguments. M2's boundary (same-user code is outside it, M2 03 §5.4) is unchanged, but on macOS TCC is the boundary the
plan relies on, so the harness is treated as a capture tool that is granted only while it is needed:
- the owner's Mac keeps the Dev Harness granted (re-prompting on every `task mac:hw` run would defeat V-D6); the
  owner accepts that this Mac's user can capture through it;
- VM clones are thrown away after each run (§3.2), so they need no cleanup;
- anyone else (a community tester, a borrowed Mac: M-MAC-17, §11 R3) runs **`./harness.sh --cleanup`** when finished
  and then deletes the folder. `--cleanup` runs, for the bundle it points at,
  `tccutil reset ScreenCapture <harness id>`, `tccutil reset AudioCapture <harness id>`, and
  `tccutil reset Microphone <fixture id>` for each fixture bundle id in `Contents/Resources/fixtures` (read from their
  Info.plists), prints each line and exits 0 (`tccutil` errors are printed, not fatal). The artifact's `README.txt` and
  the run book say this in their first lines.

### 4.2 `tools/g2g` on macOS

New files `tools/g2g/capture_darwin.go` and `capture_darwin.m` (`//go:build darwin && cgo`); `capture_other.go`
becomes `//go:build !windows && !(darwin && cgo)`. The Windows build line is unchanged; the darwin one is

```sh
MACOSX_DEPLOYMENT_TARGET=14.4 CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go -C tools build -trimpath \
  -ldflags "-X main.version=<v>" -o ../bin/g2g-arm64 ./g2g     # and GOARCH=amd64, then lipo -create
```

- **Capture** (`openSCK() (capturer, error)`, replacing `openGDI` on darwin): `SCShareableContent` → the `SCDisplay`
  whose `displayID` is `CGMainDisplayID()`; `SCContentFilter initWithDisplay:excludingWindows:@[]`;
  `SCStreamConfiguration`: `width`/`height` = the display mode's `pixelWidth`/`pixelHeight` (physical pixels, as GDI
  with DPI awareness), `pixelFormat` `kCVPixelFormatType_32BGRA`, `minimumFrameInterval` `CMTimeMake(1, 20)`,
  `showsCursor` NO, `queueDepth` 3, `scalesToFit` NO. The output handler keeps only the latest frame whose
  `SCStreamFrameInfoStatus` is `Complete` (retained `CVPixelBuffer` in a one-slot mailbox under a lock). `Grab()`
  waits ≤ 100 ms for a frame newer than the last one taken (else a `capture` decode failure), copies it into a Go
  buffer, and stamps it with `SCStreamFrameInfoDisplayTime` converted to ns (`mach_timebase_info`), the composition
  time, which is a better capture time than GDI's call time. API: SCK, macOS 12.3+ (fine at 14.4).
- **Permission**: Screen Recording for the responsible process: run `g2g measure` through `harness.sh` (§4.1).
  `CGPreflightScreenCaptureAccess()` false → exit 2 with "g2g needs Screen Recording for the harness app".
- **Refresh rate**: `CGDisplayModeGetRefreshRate(CGDisplayCopyDisplayMode(main))`; when it returns 0 (adaptive
  modes), the nominal period of a `CVDisplayLink` on that display. L-2 needs 60.
- **Versions**: `osVersion()` returns `macOS <kern.osproductversion> (<kern.osversion>) <arch>`. The JSON gains
  `"machine": {"virtual": <kern.hv_vmm_present == 1>, "arch": "arm64", "model": "<hw.model>"}` (a model identifier
  such as `Mac17,2`, never a name or serial). `measure` refuses nothing on a VM; the run book doesn't count it.
- **Unchanged**: the clock page, decoder, calibration, plausibility filter, report, exit codes. Overlays: Chrome on
  macOS presents video through CoreAnimation layers that WindowServer composites, so SCK sees them (the GDI-only
  `--disable-direct-composition-video-overlays` step has no macOS counterpart).
- **Tests**: `capture_darwin_test.go`: the BGRA → decoder buffer conversion and the mach → ns stamp (CI macos, no
  TCC); `TestSCKCaptureHW` (`isshoni_hw`, `macenv.Require(t, ScreenRecording, GUISession)`) opens the clock page in
  a WKWebView window of the test itself plus a half-size copy (`?freeze=` frames are not enough: both must animate)
  and asserts the decoder finds both strips with `decodedPct` ≥ 99 and Δ p95 ≤ 34 ms (two windows of one process
  differ by at most two frames).

### 4.3 `cmd/isshoni-enginetest` on macOS

The darwin engine is linked in (02), so `-dll` is Windows-only (on darwin it is a usage error, exit 64). Two binaries
come from one package: `isshoni-enginetest` (the release engine) and `isshoni-enginetest-hooks` (built with 02's
test-hooks link selection: luma tap, `IM_ENC_FAKE`, `test:pattern`). Commands that need a hook in the release binary
exit 2 with "needs isshoni-enginetest-hooks". New darwin files: `avsync_darwin.go`/`.m`, `protect_darwin.go`,
`soak_darwin.go`, `cpu_darwin.go`, `raw_darwin.go`, `startup_darwin.go` with 02 §13.2's constraint
`darwin && cgo && imstatic`; the `_other.go` files become `!windows && !(darwin && cgo && imstatic)` (02 §13.2), so a
darwin build without `imstatic` (M2's `test:go`, `desktop-macos`) still compiles.

| Command | macOS behaviour (02 §13.2) | Needs |
|---|---|---|
| `probe` | `im_probe(FULL)`: VideoToolbox encoders (hardware/software flags, LLRC), OS feature flags (02), audio permission state | – |
| `sources` | `im_list_sources` with `mon:<CGDirectDisplayID>` ids (CoreGraphics only) | – |
| `pick` | SCContentSharingPicker; prints the picked source (15.2+: kind and name; before 15.2: kind only, 02 §8.2.1) | human |
| `capture -source pick\|mon:<id>\|test:pattern` | as M2, `f.h264`/`q.h264` and events | pick: human; `mon:`: tcc:screen |
| `bench` | for L-3 and §8: `-source mon:<main>` at 1080p60 (02 §14), 30 s; prints `capture → packet p50 … p95 … p99 …`, encode p99; `-size 1440p\|2160p`, `-layers 1\|2` for 02's 4K rule (4K60 alone, 4K30 + preview) | hw:h264, tcc:screen |
| `avsync` (hooks) | §4.3.1 | tcc:audio; human (pick) or tcc:screen (`mon:`) |
| `protect-check` (hooks) | §4.3.2 | human (`-sck pick`); tcc:screen (`-sck listed`) |
| `soak -hours 2 -audio tone` | one session; samples every 60 s (`phys_footprint`, threads, descriptors, IOSurfaces); M2's engine rule: growth ≤ 5 % after 10 min, no `RING_OVERFLOW`, no `ENDED`. §8's 10 % app-footprint row is a separate check | hw:h264 |
| `sessions -count 4` | 4 concurrent sessions of `test:pattern` (the engine's limit; a 5th returns `IM_ERR_LIMIT` `session_limit`, 02 §7.6) | hw:h264 for the release binary; none for the hooks binary with `-encoder fake` |

#### 4.3.1 `avsync` on macOS

Same steps and pass rule as M2 05 §19.3; the platform parts:
1. **Helpers outside the tool's tree**: `AVSync Flash.app` (`io.isshoni.test.avsync.flash`) and `AVSync Click.app`
   (`io.isshoni.test.avsync.click`) from the harness's `Resources/avsync/`, started with `NSWorkspace
   openApplicationAtURL:configuration:` (`createsNewApplicationInstance` YES; arguments: a Unix socket path in the
   tool's temp folder). LaunchServices makes launchd their parent and each its own responsible process, so the
   exclude-apps share neither hides the flash nor mutes the clicks (S2: fixtures launched with `open -n -g -j` were
   classified as their own apps); this is the macOS counterpart of M2's helpers under `explorer.exe`.
2. **Flash** (macOS 10.15.4+ APIs): a borderless 1280 × 720 `NSWindow` centered on the main display with a
   `CAMetalLayer` (`displaySyncEnabled` YES), black, white for 100 ms once a second; for each first white drawable,
   `addPresentedHandler:` gives `presentedTime` (host time in seconds) → `P_k` in mach ns.
3. **Click**: `im_test_render_bursts(NULL, 2000, 0.5, 5, 1000, n, …)` (03's darwin implementation: 5 ms clicks once a
   second on the default output) returns each click's predicted device time `A_k` in mach ns.
4. **Session**: `-source pick` (the flash window; the default) or `-source mon:<id>`; layer `f` 1080p60; audio
   exclude-apps with `TapPCM`; `-encoder fake` in a VM (recorded as `"encoder": "fake"`); 25 s.
5. Output `avsync.json` as M2 plus `"os": {"version", "build", "virtual", "arch"}` and `"encoder"`:

```json
{"tool":"isshoni-enginetest-hooks","version":"0.3.0-rc.2","os":{"version":"14.4.1","build":"<build>","virtual":true,"arch":"arm64"},
 "source":"pick","encoder":"fake","pairs":21,"medianOffsetMs":4.1,"p5Ms":-1.8,"p95Ms":9.7,"edgesDiscarded":0,
 "result":"pass","exitCode":0,"pairsDetail":[{"pNs":0,"aNs":0,"vNs":0,"uNs":0,"offsetMs":4.0}]}
```

(`pNs`… are relative to the first flash; no machine, window or path.)

#### 4.3.2 `protect-check` on macOS

Checks 01/02's own-window rule ("SCK ignores `sharingType` on 15+: exclude isshoni in the SCContentFilter if a picker
filter allows it, otherwise auto-minimize and mute"). 02 §13.2 owns the command (`protect-check [-sck
listed|pick|off] [-seconds S] [-json]`): a noise window **in the tool's own process** (which the engine treats as
"self" exactly as it treats the app's windows), `ProtectWindow`, capture of the display through `mon:` (`listed`)
and/or a display pick (`pick`, §8.8's strategy for this OS), unprotect half-way; presence is judged by layer `f`'s
non-IDR slice bytes as M2 (`protectMinShownBps` 500 kbit/s, `protectMaxRatio` 20 %). The exit gate (C-1) uses
`-sck pick -json`. 02 §13.2 adopts these outcome tokens in its `-json` (`outcome`, next to the strategy and the OS
version):
- `pass-filter`: strategy A or B excluded the tool's application (pattern absent while protected, present after);
- `pass-sharingtype`: strategy C (`NSWindowSharingNone`, 14.x);
- `fallback-needed`: the pick reported `self_visible` (`IM_SESS_SELF_VISIBLE`), so the app takes the auto-minimize
  and mute fallback (M-MAC-6 checks that path by hand);
- `fail`: the pattern is in the frames while the engine claimed exclusion, or it is absent after unprotect.
Exit 0 for `pass-*`, 1 for `fail`, 2 for `fallback-needed`. Whether an outcome passes the gate is decided by
02 §8.8's filled-in per-OS table (C-1), not by the exit code alone: on 15.0/15.1 `fallback-needed` is the designed
result.

### 4.4 `cmd/isshoni-audiotest` on macOS

03-macos-audio owns `driver_darwin.go`, the fixtures and the darwin case table. This doc fixes what the exit gate
reads from it:
- Commands, flags and exit codes as M2 (`run -gate`, `record`, `merge`, `selfcheck`, `probe`, `apps`, `fixture
  <verb>`; 0/1/2/3/4). `selfcheck` has no sound and needs no TCC on macOS (03 §16.2).
- Report additions: `"os": {"product":"macOS","version":"<15.x.y>","build":"<build>","virtual":true,"arch":"arm64",
  "model":"VirtualMac2,1"}`, `"engine": {"version","commit","abi","testHooks"}` (from `im_build_info`; `version` is
  filled from build info's `engine` key, 02 §5.3 and M2's `BuildInfo`) and
  `"tool": {"sha256"}` (the running Mach-O, the universal file), replacing M2's `dll`. Report name
  `audiotest-macos<major>-<build>-<yyyyMMdd-HHmm>.json`.
- `merge` refuses a manual entry of another OS build (`version` + `build`) and a `-no-redact` report, as M2.
- Redaction: paths under a home folder become `~/…`; app names of non-fixture processes are redacted as M2; the VM's
  and the Mac's computer names never appear (no field reads them).
- Committed name: `cmd/isshoni-audiotest/results/<date>-macos<major>-<build>.json`, with `-vm` before `.json` when
  `os.virtual` is true.
- Cases that M2 marks as Windows concepts (fast path 4, 5, 26; `tree-contains-excluded` 17) are 03's to map or mark
  not applicable on darwin; the gate counts whatever 03 marks required.

### 4.5 The exit tools artifact (`isshoni-exit-tools-macos`)

04's `macos-pkg` job assembles and checks it after `macos-sign-tools` has signed the harness (04 owns the jobs' build
and signing steps; this doc owns the artifact's content and checks; `macos-sign-tools` holds only the test-tools
identity and, like `macos-sign`, runs no built code, 04 D12, so every check that executes a tool runs in the secret-free
`macos-pkg`). Built from the tag, uploaded as a **workflow artifact** (kept 90 days, not a release asset, like M2's
`isshoni-exit-tools`):

```
isshoni-exit-tools-macos/
  isshoni Exit Harness.app/       universal, test-tools identity isshoni-testtools-1, hardened runtime (§4.1)
  harness.sh
  SHA256SUMS                      shasum -a 256 format: every Mach-O in the bundle, every fixture's main executable
  exit-tools-manifest.json        {"version":"0.3.0-rc.N","commit":"<40 hex>","engine":{…im_build_info…},
                                   "tools":{"isshoni-audiotest":{"sha256":"…","testHooks":false},
                                            "isshoni-enginetest-hooks":{"sha256":"…","testHooks":true}, …}}
  README.txt                      first: "a granted harness lets any process of your user capture the screen and
                                  audio; when finished run ./harness.sh --cleanup and delete this folder" (§4.1);
                                  then the run id, the tag and the test-tools certificate's SHA-256 fingerprint
```

`macos-pkg`'s checks before upload: `codesign --verify --strict` on the bundle and every nested bundle; `codesign -d
-r-` shows the test-tools identity's leaf hash, equal to `deploy/keys/isshoni-testtools-1.pem`'s (the ephemeral
identity in a dry run), and the bundle carries no signature by the release identity; `shasum -a 256 -c SHA256SUMS`
passes; `isshoni-audiotest selfcheck` exits 0 (no sound, no TCC on macOS); `isshoni-enginetest probe` exits 0.

---

## 5. The automatable part

### 5.1 Skips that name their reason (`internal/testenv/macenv`)

```go
// Package macenv gates tests that need what CI macOS runners lack (docs/m3/05 §5.1). Test files only.
package macenv

type Need string

const (
	ScreenRecording Need = "tcc:screen"   // SCK of listed sources, g2g
	SystemAudio     Need = "tcc:audio"    // Core Audio process taps
	Microphone      Need = "tcc:mic"      // fixture apps that hold the mic
	LocalNetwork    Need = "tcc:localnet" // LAN peers on 15+
	HardwareEncoder Need = "hw:h264"      // a VideoToolbox encoder with IM_ENCF_HARDWARE
	AudioOutput     Need = "hw:audio-out" // a default output device
	AudioInput      Need = "hw:audio-in"
	GUISession      Need = "gui"          // a WindowServer session (CGSessionCopyCurrentDictionary != nil)
	BareMetal       Need = "baremetal"    // kern.hv_vmm_present == 0
	Human           Need = "human"        // a person clicks the picker: ISSHONI_TEST_HUMAN=1
)

// Require skips t unless every need is met. tcc:* needs are declared in ISSHONI_TEST_TCC (comma list of
// screen,audio,mic,localnet); a declared grant that the OS reports missing fails t instead of skipping it.
func Require(t testing.TB, needs ...Need)
```

- Skip message (exact form, which `skipcheck` parses): `macenv: needs <need>[, <need>] (<why>; docs/m3/05 §5.4)`,
  e.g. `macenv: needs tcc:audio (System Audio Recording for the test's responsible app; run it through task mac:hw;
  docs/m3/05 §5.4)`. On a non-darwin OS: `macenv: needs darwin`.
- Declared-but-missing checks: `tcc:screen` → `CGPreflightScreenCaptureAccess()`; `tcc:audio` → the engine's probe
  audio-permission flag (02/03: the preflight S2 used); `tcc:mic` → `isshoni-audiotest fixture mic-status` run in
  each mic fixture (03 §16.4: `AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio` inside the fixture);
  `tcc:localnet` → a UDP send to the default gateway (S2's `lancheck`) succeeds. A failure message names the bundle
  id and the `tccutil reset` line.
- `hw:h264` runs `im_probe(IM_PROBE_FULL)` once per process (< 2 s, cached; the quick probe only returns the last
  full probe's cached entry, 02 §9.10) and requires a `videotoolbox` entry with `IM_ENCF_AVAILABLE|IM_ENCF_HARDWARE`;
  `IM_OS_VT_LOW_LATENCY` is recorded in the skip/pass log but not required. `hw:audio-*` ask Core Audio for the
  default device; `baremetal` reads `kern.hv_vmm_present`.
- Native counterpart `native/macos/tests/support/macenv.h`: `IM_REQUIRE("tcc:screen", "hw:h264")` → `GTEST_SKIP()` with
  the same message form, or `FAIL()` for a declared grant that is missing.
- Rule: **no test touches a capture, tap or SCShareableContent API before `Require`** (listing shareable content
  alone prompts for Screen Recording). The fake engine and `test:pattern` + test tone sessions need nothing.

**The ledger** `.github/macos-skips.txt`, one line per expected skip. It covers only tests that **CI compiles** and
`macenv` gates: the native `IM_REQUIRE` gtests and untagged (or `imstatic,imtesthooks`) Go tests. Go tests tagged
`isshoni_hw` (M2's tag) are never compiled on CI, so they never skip there and get no ledger line.

```
# <job>/<matrix>  <go package or gtest binary>  <test name pattern>  <need>
macos-native/macos-26        vt_encoder_test                                    VTEncoder.Hardware*   hw:h264
macos-native/macos-26        im_macos_hw_tests                                  MacTapHW.*            tcc:audio
macos-native/macos-26        github.com/MoonWX/isshoni/internal/media/native    TestStaticConformanceVT  hw:h264
```

`tools/skipcheck -job <job>/<matrix> -ledger .github/macos-skips.txt -go go-test.json -gtest 'gtest-*.xml'` (tools
module, standard library): reads `go test -json` events and gtest XML `<skipped message=…>`; fails when a skip in the
scoped packages (`internal/media/...`, `internal/client/core`, `cmd/isshoni-enginetest`, `cmd/isshoni-audiotest`,
`cmd/isshoni-desktop`, `tools/g2g`, `native/macos`) has no `macenv:` message, isn't in the ledger, or names another
need; prints a notice (not a failure) for a ledger line whose test ran, so a runner that gains a capability shows up.
It writes the list of skips to the job summary.

### 5.2 Which tests need which grant

| Test (owner doc) | Kind | Needs | Mac (`task mac:hw`) | CI macos |
|---|---|---|---|---|
| `native/macos` unit tests (02 §17.1, 03): rules, bundle-ID/PID matching, resampler, AVCC → Annex B, rate policy, tap watchdog against an injected HAL, crossfade arithmetic, `session_fake_test` (fake capture source: resize, gone, `os_stopped`, `session_limit`) | gtest | – | ✓ | ✓ |
| `vt_encoder_test` (02): real VT, hardware if present, else software, else skip; the hardware-only assertions (`VTEncoder.Hardware*`) gate with `IM_REQUIRE("hw:h264")` | gtest | – (hardware part: hw:h264) | ✓ | ✓ (structure; hardware part skips, ledger line) |
| `MacTapHW.*` (03, `im_macos_hw_tests`): tap create/destroy, rebuild on rate and default-device change, zero-buffer rebuild, two-tap alignment ±1 sample | gtest | tcc:audio, hw:audio-out | ✓ | skip (ledger line) |
| `MacMicHW.*` (03, `im_macos_hw_tests`): a fixture holding the mic is a mic user; the harness's own tap reader isn't | gtest | tcc:audio, tcc:mic, hw:audio-in | ✓ | skip (ledger line) |
| `internal/media/native` 02's `TestStatic*` incl. `TestStaticConformance` (§5.6: `test:pattern` + fake) and `TestStaticSoftwareVT` (structure only) | Go `imstatic,imtesthooks` | – | ✓ | ✓ |
| `TestStaticConformanceVT` (§5.6: `Encoder: "videotoolbox"`) | Go `imstatic,imtesthooks` | hw:h264 | ✓ | skip (ledger line) |
| `TestConformanceNativeHW` (hardware VT, then `mon:<main>`); 02's `TestHWVideoToolbox` (1080p60/1440p60 encode p99 ≤ 16 ms), `TestHWListedDisplay`, `TestHWProtectListed` | Go `imstatic,isshoni_hw` | hw:h264; + tcc:screen | ✓ | not compiled |
| `internal/media/native` `TestTap*HW`, `TestAudioControllerHW` (03) | Go `isshoni_hw` | tcc:audio, hw:audio-out | ✓ | not compiled |
| `TestTapWithoutGrant` (§5.3) | Go `imstatic,imtesthooks` | – | skipped when `ISSHONI_TEST_TCC` declares `audio` (the premise is "no grant") | ✓ |
| `internal/client/core` `TestCaptureToSend` darwin (M2 01 §12.3's bounds: p95 ≤ 35 ms, p50 ≤ 20 ms) | Go `isshoni_hw` | hw:h264, tcc:screen | ✓ | not compiled |
| `cmd/isshoni-audiotest run` (all cases) | tool | tcc:audio, tcc:mic, hw:audio-out, hw:audio-in | ✓ | never |
| `cmd/isshoni-audiotest selfcheck` (no sound, no TCC on macOS, 03 §16.2) | tool | – | ✓ | ✓ |
| `cmd/isshoni-enginetest` `TestCommands*` / 02's `TestRun*Static` against the linked engine (probe, `test:pattern` capture, sessions and bench with fake) | Go | – | ✓ | ✓ |
| `tools/g2g` `TestSCKCaptureHW` | Go `isshoni_hw` | tcc:screen, gui | ✓ | not compiled |
| `cmd/isshoni-desktop` `TestWailsChecklist` darwin (01's WM rows marked CI) | Go `isshoni_hw` (no grant) | gui | ✓ | ✓ (`desktop-macos`, 01 §19.2) |
| WK spec (§6) | Playwright | gui | ✓ (`task e2e:wk`) | ✓ (`macos-wk`) |
| `LocalNetworkHW` (a LAN peer through the engine's sockets) | Go `isshoni_hw` | tcc:localnet, baremetal | ✓ (15+) | not compiled |
| picker paths (`pick`, `avsync -source pick`, `protect-check -sck pick`) | tool | human | by hand | never |

### 5.3 GitHub's macOS runners

Images: `macos-26` and `macos-15` (arm64 VMs, 3 vCPU, 7 GB), `macos-15-intel` (x86_64), `macos-14` (arm64). They have
a logged-in GUI session, no TCC grants for screen or audio capture, no audio devices that a test may rely on, and
(expected) no hardware video encoder. Free for this public repository. 04 owns `.github/workflows/*` and the jobs'
build steps; this doc adds the steps below. Every job that builds native code runs 04's Xcode-select step from
`deploy/macos/tools.json` first.

| Job (`ci.yml`) | Runner | What (this doc's steps; the build steps are 02's/04's) |
|---|---|---|
| `desktop-macos` (changed, 04's) | `macos-26` (04's `tools.json`) | M2's Go test step stays as it is (no `imstatic`: the untagged darwin build and the fake engine, 02 §6.7); 04's packaging steps build, sign (ephemeral) and package the universal app, which links the real engine; 01 adds the e2e-flavor bundle build, `TestWailsChecklist`/`TestMacSmoke`, KC, the darwin lint pass and the production-flavor `open isshoni://…` check (WM26). This doc adds one step: upload the e2e-flavor bundle as the artifact `isshoni-desktop-macos-e2e` (1 day) for `macos-wk` |
| `macos-native` (new, 02's) | `macos-26` (02 §6.7) | 02's steps: `task native:mac PRESETS="mac-arm64-ci mac-x64-release"` (ctest of `mac-arm64-ci` with gtest XML; the x86_64 release archive as a link check), `go test -race -count=1 -tags imstatic,imtesthooks` of `internal/media/native/...` and `cmd/isshoni-enginetest/...`, the real-engine app link (step 4), lint. This doc adds: `go test -json` of `cmd/isshoni-audiotest/...` and `tools/g2g` (darwin, cgo); `isshoni-audiotest selfcheck`; `skipcheck`; uploads `mac-test-bins` (the gtest binaries and `go test -c` binaries, arm64) for `macos-compat` |
| `macos-compat` (new) | `macos-14` | downloads `mac-test-bins`; runs the gtest binaries and `go test -c` binaries (built with the newest SDK and deployment target 14.4); fails on any dyld "Symbol not found"/"Library not loaded" in stderr. Proves the `@available` gating and weak linking on 14.x. The app's `--version`/`--self-check`/dyld checks on `macos-14` are 04's `macos-runtime` (04 adds `macos-14` to its matrix and `runtimeRunners`). When GitHub retires `macos-14`, this job is removed and its steps run in the 14.4 VM at every minor release (§9.6) |
| `macos-wk` (new) | matrix `macos-15`, `macos-26`; `needs: desktop-macos` | downloads `isshoni-desktop-macos-e2e` and `isshoni-desktop-macos-pr` (04's artifact); runs the WK spec (§6) on the e2e `.app`; on the production `.app` from the PR artifact: `go tool nm` of its binary has no `e2edriver` symbol and the production menu check of WM9 (§6.5). Entitlements, `testHooks: false` and the binary checks are 04's (§5.5 check 4, `machocheck`, `--self-check` in `desktop-macos`), not repeated here |
| `ci-ok` | ubuntu | needs the new jobs when their path filters ran (`native/macos/**`, `internal/media/native/*darwin*`, `internal/desktop/**`, `cmd/isshoni-desktop/**`, `tools/g2g/**`, `tools/macharness/**`, `tools/skipcheck/**`, `web/e2e/wk/**`, `.github/macos-skips.txt`) |

`nightly.yml` runs 02's nightly steps on `macos-26` (`mac-arm64-asan` and `mac-arm64-tsan` ctest; `mac-x64-ci` ctest
and the amd64 Go tests under Rosetta, 02 §6.7) and `macos-native`'s Go tests with `-count=3` for timing flakes. A
CI macOS job never sets `ISSHONI_TEST_TCC` or `ISSHONI_TEST_HUMAN`.

The tap's permission handling is tested on CI without a grant (03 §13.2: audio never fails `im_start`, and
`IM_ERR_PERMISSION_DENIED` is never returned by an audio call): `TestTapWithoutGrant` (`internal/media/native`, `-tags
imstatic,imtesthooks`, no `Require`) starts an audio-only session (02 §10: `source_id` NULL, `layer_count` 0) with
exclude-apps and `TAP_PCM` and asserts: `Start` returns nil (`IM_OK`) within 1 s; within 1 s
`im_session_info.audio_state` is `IM_AUDIO_STATE_PERMISSION`, or the audio report's `state` is `permission-pending`,
`permission-denied`, `tap-failed` or **`no-device`** (a hosted runner may have no output device; 03 §13.4 maps that to
`IM_OK` with `state: no-device`); `im_next_packet` keeps delivering Opus packets (silence) for 3 s; `im_stop` returns
within 300 ms; nothing blocks (the test's 40 s timeout reports a regression that waits on a prompt past the 30 s
watchdog). The property under test is "never blocks, never fails the share", whichever of those states the runner gives;
the test logs the state it saw (`t.Logf("audio state: %s", state)`), so a change of runner image shows in the log. No
virtual audio driver is installed on the runner (it would need a HAL plug-in and admin rights). The watchdog itself is a
native unit test with an injected HAL whose request never answers (deterministic, 03).

### 5.4 The development Mac with grants (`task mac:hw`)

```
task mac:harness          # builds and signs isshoni Dev Harness.app into .dev/mac-harness/ (dev identity)
task mac:hw               # all suites below, through the harness; summary in .dev/mac-hw/<stamp>/summary.json
task mac:hw:engine        # native hw gtests, Conformance with VT and mon:, bench, sessions
task mac:hw:audio         # native tap/mic gtests, Go tap tests, isshoni-audiotest run (all cases, no -gate)
task mac:hw:core          # TestCaptureToSend darwin
task mac:hw:g2g           # TestSCKCaptureHW
task mac:hw:human         # pick, avsync -source pick, protect-check (asks for clicks; ISSHONI_TEST_HUMAN=1)
```

- Each suite runs `harness.sh` with `ISSHONI_TEST_TCC=screen,audio,mic` (plus `localnet` on 15+); the harness writes
  `go test -json`, gtest XML and tool JSON into `.dev/mac-hw/<stamp>/` (git-ignored).
- **First use**: the owner grants the dev harness Screen Recording and System Audio Recording, and each fixture app the
  Microphone, once. Later rebuilds keep them (same dev identity and identifier: S2). A suite whose grant is gone fails
  (V-D6) with the `tccutil` hint; it never silently skips.
- What it needs from the Mac: an output device (built-in speakers, volume up but nothing else playing), the built-in
  microphone, AC power, no other capture app running.
- Runs from any terminal; `task` itself needs no grant. Slices name the suite they need in their "Manual:" items.
- In a VM (`-granted` clone with the repository's build tools) `task mac:hw` runs as well; hw:h264 tests skip there
  with their reason, which is how V1 measures the lab.

### 5.5 Optional self-hosted runner (`.github/workflows/machw.yml`)

`workflow_dispatch` only (inputs `suite`: `hw` | `exit-audio`; `release_run_id` for `exit-audio`); `if:
github.repository == 'MoonWX/isshoni'`; environment `machw` with the owner as required reviewer; `runs-on: [self-hosted,
macOS, ARM64, isshoni-mac-hw]`. The runner is registered `--ephemeral` and started by hand with `./run.sh` in a Terminal
of the owner's logged-in session (TCC and the picker need the Aqua session; a launchd daemon has neither), online only
while a run is wanted. `hw` runs `task mac:hw` (without `human`) and uploads `.dev/mac-hw/<stamp>/`; `exit-audio`
downloads the rc's `isshoni-exit-tools-macos`, checks `SHA256SUMS`, runs `harness.sh isshoni-audiotest run -gate` and
uploads the redacted report. No `pull_request` trigger ever targets these labels; "Require approval for all external
contributors" stays on. Real-app checks and `avsync` stay by hand.

### 5.6 Conformance against the real engine

02 owns `TestStaticConformance` (`-tags imstatic,imtesthooks`: `mediatest.Conformance` on `test:pattern` with the
fake encoder; CI `macos-native` and the Mac). This doc adds its VideoToolbox twin in the same package
(`internal/media/native/conformance_static_darwin_test.go`, same tags):

```go
func TestStaticConformanceVT(t *testing.T) {
	macenv.Require(t, macenv.HardwareEncoder) // hw:h264: im_probe(IM_PROBE_FULL), AVAILABLE|HARDWARE
	mediatest.Conformance(t, openNative, media.Source{ID: "test:pattern"},
		mediatest.Options{Listed: true, Encoder: "videotoolbox"}) // encoder kinds are strings (abi.go)
}
```

- CI macos: `TestStaticConformance` (fake) always; `TestStaticConformanceVT` skips with `macenv: needs hw:h264`
  (ledger line). The release engine never opens a software encoder (02 §9.1), and VT's software encoder (≈ 870 ms)
  would break Conformance's 500 ms `firstPacketBound` anyway. CI's VideoToolbox coverage is structural: 02's
  `TestStaticSoftwareVT` (one 320×180 session through the test-hooks software switch) and `vt_encoder_test`.
- Mac (local): `TestStaticConformanceVT` passes with hardware VT; additionally `TestConformanceNativeHW`
  (`imstatic,isshoni_hw`): hardware VT on `mon:<main display>` with tcc:screen; check 5's live bitrate holds (VT
  changes bitrate live, plan "Rate control"). The audio side is the engine's test tone (no tap).
- `mediatest.Options.Encoder` already exists (M2, `conformance.go`); no change to `internal/media/mediatest`.

### 5.7 Windows unchanged, and how CI proves it

Shared files M3 verification touches and the Windows proof:

| File | Change | Proof |
|---|---|---|
| `tools/g2g/capture_other.go` | build constraint only | `GOOS=windows go -C tools vet ./g2g` in `lint-go`; the Windows g2g build line unchanged in `release.yml` |
| `cmd/isshoni-enginetest/*_other.go` (`!windows && !(darwin && cgo && imstatic)`), new `*_darwin.go` | constraints; `-dll` refused on darwin | `windows-native` builds and runs `TestCommands*`; `main_windows_test.go` unchanged; `desktop-macos`'s untagged darwin build still compiles the tool |
| `cmd/isshoni-audiotest/driver_other.go` (03), report fields `os`/`engine`/`tool` (additive; `dll` stays on Windows) | additive | `windows-desktop`'s `selfcheck`; `merge` golden tests on every OS |
| `web/e2e/cdp/*` → shared helpers moved to `web/e2e/desktop/common.ts` (§6.4) | pure move | `windows-desktop`'s CDP step: same cases pass, same annotation rows in `annotations.md` |
| `ci.yml` (`changes` filters, new jobs, `ci-ok`), `Taskfile.yml` (`mac:*`, `e2e:wk`) | additive | `lint:actions` (actionlint), every Windows job still triggered by its own filters |

---

## 6. Real-app end-to-end on WKWebView

### 6.1 Three tiers

1. **Desktop in Chrome** (`internal/desktop/e2ehost`, M2 03 §22.4 A): unchanged and OS-neutral; covers the bridge,
   app, session and audio policy logic with the fake engine on ubuntu and the Mac.
2. **The WK spec** (new, `web/e2e/wk/`): the real `-tags e2e` darwin `.app` (WKWebView, Wails' darwin backend, the
   fake engine of the e2e flavor) driven through the in-app driver (§6.2), with a Chrome friend viewer from Playwright,
   in `macos-wk` (`macos-15`/`macos-26`) and on the Mac (`task e2e:wk`). The macOS counterpart of M2's CDP spec.
3. **Manual** (`M-MAC-*`, §7): picker, grants, real capture, hardware.

Safari's Web Inspector stays for humans: dev and e2e builds set `WKWebView.inspectable = YES` (macOS 13.3+); the
production build sets NO and `macos-wk`'s production checks (§5.3), `TestWailsChecklist`'s WM19 and M-MAC-19 hold it.

### 6.2 The in-app driver (e2e builds only)

File `cmd/isshoni-desktop/e2edriver_darwin.go` (+ `.m`), `//go:build darwin && cgo && e2e` (01 hosts it in the e2e
flavor; this doc fixes its protocol). Started after the main window exists when `ISSHONI_E2E_DRIVER=1`:
- listens on `127.0.0.1:0`; writes `{"port":<n>,"token":"<64 hex>"}` to **`<ISSHONI_E2E_DATA_DIR>/e2e-driver.json`**
  (the directory the fixture passes, not the `isshoni` data root below it; mode 0600) for the fixture; every request
  needs `Authorization: Bearer <token>`; bodies ≤ 1 MiB; one request at a time. The driver refuses to start when
  `ISSHONI_E2E_DATA_DIR` is unset.
- **Results from the page** come back through an e2e-only bound service `E2E.Report(id string, ok bool, value
  string)` that only the e2e flavor registers (its method id is added to the e2e build's `assets.GuardWKWebView`
  allowlist only). The driver wraps each script as
  `(async () => { try { const v = await (<script>); report(id, true, JSON.stringify(v ?? null)); } catch (e) {
  report(id, false, String(e)); } })()` and runs it with Wails' `WebviewWindow.ExecJS`; `report` is a raw
  `/wails/runtime` call built like the CDP spec's `bound()`. Scripts run natively (`evaluateJavaScript`), so the page's
  CSP (no `unsafe-eval`) doesn't apply to them, and they are not user gestures (W13's premise holds).

| Endpoint | Body → answer | What it does |
|---|---|---|
| `POST /v1/eval` | `{"script":"…","timeoutMs":30000}` → `{"ok":true,"value":<json>}` / `{"ok":false,"error":"…"}` | runs the script in the main window's page |
| `GET /v1/state` | → `{"pid":…,"windows":[{"id":1,"main":true,"visible":true,"key":true,"miniaturized":false,"fullscreen":false,"frame":[x,y,w,h]}],"screen":[w,h],"url":"wails://localhost/…","webkitPids":[…],"footprint":{"app":<bytes>,"webkit":<bytes>},"runningBundleIds":[…]}` | AppKit's view of the app; footprints from `proc_pid_rusage(RUSAGE_INFO_V4).ri_phys_footprint` of the app and of the WebKit processes whose responsible PID is the app |
| `POST /v1/navigate` | `{"url":"https://example.com"}` | a navigation the app starts (Wails `SetURL`), the counterpart of CDP's `Page.navigate` |
| `POST /v1/key` | `{"key":"escape","modifiers":["cmd"]}` (keys: `escape`, `q`, `w`, `r`, `f`, `equal`, `minus`, `i`, `p`, `u`, `c`, `v`, `x`, `a`, `z`, `h`, `m`) | posts `NSEvent` key down/up to `NSApp` with the main window key: AppKit menus, key equivalents and WKWebView see it as a real key |
| `POST /v1/close` | – | `[window performClose:]`, the red button |
| `POST /v1/quit` | – | `[NSApp terminate:nil]`, what Cmd+Q's menu item does (so it goes through 01's `ShouldQuit`, §7.5) |
| `POST /v1/webcontent/kill` | → `{"killed":<pid>}` | `kill -9` of the page's WebContent process (W17's case) |

No TCC is needed for any of it (in-process events, same-user `proc_pid_rusage` and `kill`). The production build
can't contain it: §5.3's `nm` check, and the file's constraint.

### 6.3 Launch on CI

The fixture (`web/e2e/wk/fixture.ts`) starts `isshoni.app/Contents/MacOS/isshoni --relaunched` directly (a direct exec,
which 04 §2.4 allows for e2e builds; no TCC involved) with `ISSHONI_E2E_DRIVER=1` and `ISSHONI_E2E_DATA_DIR=<temp>` (01
extends the e2e flavor's data-dir override to darwin: `<dir>/isshoni` replaces `~/Library/Application Support/isshoni`),
waits ≤ 30 s for `<temp>/e2e-driver.json`, then for `/v1/eval` of `document.readyState === 'complete'`. On darwin
`--relaunched` is not M2's Windows shell-relaunch skip: it is 01 §6.1 step 2's flag that skips the **LaunchServices**
relaunch, which a direct exec would otherwise trigger (the responsible process of a child of the test runner isn't the
app). Credentials use the e2e flavor's `isshoni-test:<hex>:` Keychain prefix in the runner's login keychain and are
deleted at exit (as M2's Credential Manager entries); R13 has the fallback. The server is M1's e2e fixture (`isshoni
serve` on 127.0.0.1: loopback is exempt from Local Network privacy, and a native app may always reach the Mac's own
addresses, S2).

### 6.4 The spec

`web/e2e/wk/desktop.wk.ts`, config `web/e2e/wk/playwright.config.ts` (a project without a browser for the app; Chrome
for the friend), `web/e2e/wk/driver.ts` (`WKPage`: `evaluate(fn, arg)`, `waitForFunction(fn, arg, {timeout})`,
`state()`, `navigate(url)`, `key(k, mods)`, `close()`, `quit()`, `killWebContent()`), and the M2 annotations reporter
(`web/e2e/cdp/annotations.ts`, unchanged) so every WM row the spec checks is a line of the job log, a row of the job
summary and of `annotations.md`. The CDP spec's pure helpers (`methodId`, `bound`, `boundJSON`, the tone and frame
assertions) move to `web/e2e/desktop/common.ts`, typed against `Pick<Page, 'evaluate'>`, which `WKPage` implements.

Cases: the WM rows where §6.5 says "WK spec", and **the app end to end** (CDP's case, with 01 §7.5's quit rules):
device-flow link → room `lounge`; a fake share watched by the Chrome friend (`framesDecoded > 0`, tone energy); the app
watching a loadtest publisher (the decode half of WM23); close while live → hidden, process alive, share continues (30
more frames at the friend); **quit while live**: `/v1/quit` (and, in a second run, `/v1/key` cmd+q) → the window is
shown and the "Stop sharing and quit?" dialog is visible (01's `app.command` `confirm-quit`), the process still alive
and the friend still receiving; then confirmation through `/v1/eval` (clicking the dialog's confirm button, or calling
`App.Quit(true)`) → the process exits within 3 s of the confirmation and the share is gone at the friend within 2 s;
**quit while idle**: `/v1/quit` → exit 0 within 3 s with no dialog. Annotations "Budget (05 §8)": spawn →
`DOMContentLoaded` (the page's navigation timing), footprints of the app and of its WebKit processes while the fake
engine shares 640 × 360 to one viewer (trend lines, not the budget's measurement).

### 6.5 The Wails checklist on macOS (01's WM1–WM26)

01-desktop-macos §3.2 owns the rows (WM1–WM26: behaviour, fallback, results table); this doc only says which check
covers each, with the same ids. "WK spec" = §6.4 in `macos-wk` and on the Mac; "`TestWailsChecklist`" = 01 §19.2's
darwin test in `desktop-macos`; M-MAC rows are §7's. The exit record (§9.5) lists every WM row with its result, and
the rows 01 marks "Mac, all" with one result per OS version.

| Row (01 §3.2) | Checked by |
|---|---|
| WM1 | WK spec (`location.origin` `wails://localhost`); `TestWailsChecklist` darwin (the `Origin` header at the test server); `TestDesktopOrigins` |
| WM2 | WK spec (`isSecureContext` recorded); `TestWailsChecklist` darwin |
| WM3 | WK spec (an injected inline script doesn't run; `window.open` from a fresh iframe returns `null`) |
| WM4 | `TestWailsChecklist` darwin (`TestGuard*` and the foreign-origin case through `assets.GuardWKWebView`) |
| WM5 | WK spec (a script navigation, `window.open`, an app-started navigation through `/v1/navigate`: the page stays on or is back on the asset origin within 2 s; no second window in `state()`; on the `/v1/navigate` case a stub `window._wails.dispatchWailsEvent` on the foreign page records no `isshoni:*` event, 01 §8.3's event gate, and the window is back on `wails://localhost` within 2 s), in `macos-wk` on `macos-15` and `macos-26`; `TestWailsChecklist` darwin |
| WM6 | WK spec: a direct second exec `isshoni --open-url isshoni://<test server>/…` with the same `ISSHONI_E2E_DATA_DIR` forwards over 01's lock and socket and exits 0 within 2 s; the first instance's page shows the add/room step. LaunchServices' `kAEGetURL` delivery is WM26's (e2e bundles declare no URL scheme, 01 §4.4/§6.3) |
| WM7 | WK spec: `/v1/close` **while live** → window not visible, process alive, `visibilityState` `hidden`, the friend still receiving; `/v1/close` **while idle** with `closeToTray` off → the process exits. Viewer audio while hidden: WM23 |
| WM8 | M-MAC-15 (action buttons; the authorization prompt can't be answered on CI); M-MAC-8 (authorization kept across the N → N+1 re-sign) |
| WM9 | WK spec (`/v1/key` cmd+equal, cmd+minus, cmd+r, cmd+option+i, cmd+p: no zoom change, no reload, no `beforeprint`); the **production menu check** in `macos-wk` (the production `.app`, driver absent: the menu titles from `--self-check`'s menu dump, 01, contain no Reload and no developer items; ⌘W hides like WM7); M-MAC-19 for pinch, swipe, drop, the context menu and Safari's Develop menu |
| WM10 | `TestWailsChecklist` darwin |
| WM11, WM12 | `TestWailsChecklist` darwin (hold without a window, M2) |
| WM13 | WK spec (`<audio>.play()` of a `blob:` tone resolves; `AudioContext` `running`) |
| WM14 | WK spec (§6.4's quit cases: while live `/v1/quit` and `/v1/key` cmd+q show the window and "Stop sharing and quit?", the process alive; after confirmation the shutdown hook runs, share stopped, close 1000, exit ≤ 3 s; while idle exit ≤ 3 s); `TestWailsChecklist` darwin (the menu item); logout: M-MAC-14 |
| WM15 | holds (M2) |
| WM16 | WK spec (`requestFullscreen()`, `state().fullscreen`, `/v1/key` escape through AppKit); M-MAC-21 (a real Esc) |
| WM17 | WK spec (`/v1/webcontent/kill`: heartbeat back within 15 s, friend still receiving); `TestWailsChecklist` darwin |
| WM18 | `TestWailsChecklist` darwin (accessibility API of its own process); view-model tests; M-MAC-14 by eye |
| WM19 | `TestWailsChecklist` darwin (`WEBKIT_X=1` scrubbed; `isInspectable` false); `macos-wk`'s `nm` check; M-MAC-19 |
| WM20 | `desktop-macos` (go-licenses, govulncheck) |
| WM21 | WK spec (403 for `Browser.OpenURL('x-apple.systempreferences:…')`, `Browser.OpenURL('ssh://x')`, `Clipboard.Text()`, `Window.Hide()`; within 3 s no `com.apple.systempreferences` in `runningBundleIds`; window still visible; a bound method answers) |
| WM22 | WK spec (`/v1/key` cmd+c, cmd+v, cmd+x, cmd+a, cmd+z in an SPA text field change its value and selection as expected; cmd+h hides, `state()` not visible; cmd+m minimizes, `state()` `miniaturized`) |
| WM23 | WK spec for the decode half (the app views an `isshoni-loadtest -decodable` publisher: `framesDecoded > 0` for High and Constrained Baseline, audio energy); by hand on all four versions (C-2) for playback while hidden and minimized and the 10 s hidden rule |
| WM24 | by hand on all four versions (C-2): each System Settings URL of 01 §9.4 opens its named pane |
| WM25 | M-MAC-11 |
| WM26 | M-MAC-10; `desktop-macos`'s production-flavor `open isshoni://…` check (01 §3.2) |

Local Network on 15+ is not a checklist row: M-MAC-16 covers the prompt and M-MAC-8 its survival across the update.

---

## 7. The manual matrix (`M-MAC-*`)

Run on the rc (exit) or on a slice's PR artifact (the slice's "Manual:" items). "VM" rows start from a fresh `-clean`
clone unless they say `-granted`. Viewer = Chrome on the host, a phone, or the Windows PC. **In VMs** the release engine
has no encoder (`no_hardware`, §2.2 L-F), so a row's "goes live and decodes at the viewer" part runs on 26 and 27 bare
metal; in the VM the same row checks the grant, picker and update-keeps-grants parts (the pick completes, the
`no_hardware` notice shows, the audio grant is checked through `audiotest`'s audio-only session). If V1 finds a hardware
VT encoder in a guest, the full rows apply there. M-MAC-1, M-MAC-8 and M-MAC-22 also run once as the standard user
(§3.2).

| Id | OS | Steps | Pass |
|---|---|---|---|
| M-MAC-1 | all | From a room's "Get the desktop app" (`/download?room=<id>`) in Safari: copy the Terminal command (`curl … install-desktop.sh \| sh -s -- --room <id>`, 04 §6.5) into Terminal; then the wizard: link (device flow), "System Audio Recording" step, sound check, "Friends won't hear" | room shown within 2 minutes, no URL or password typed; no Gatekeeper dialog; `xattr -p com.apple.quarantine <app>` reports no such attribute; the audio prompt appears at the wizard step (not at the first share); the sound check passes; the app opens the room from the script's `--room` with no URL typed (04 §10.7 item 4a). Standard user (one VM, §3.2): with an admin's copy in `/Applications`, the script installs to `~/Applications`, prints the note naming the admin's copy, and an `isshoni://` room link opens the user's copy |
| M-MAC-2 | all | Uninstall; download the DMG in Safari; drag to Applications; open; follow the page's `#macos-open-anyway` guide for this version (04 §5.9: 14.x Control-click → Open, or Open Anyway; 15+ System Settings → Privacy & Security → Open Anyway within about an hour of the blocked attempt) | the dialog text and steps match 04 §5.9 and the docs' screenshots for this version; then the app starts and links |
| M-MAC-3 | 14.4, 27 | Render the rc's cask with `deploy/homebrew/render.sh` into a local tap (`brew tap-new isshoni-ci/test --no-git`); `brew install --cask isshoni-ci/test/isshoni`; launch and grant; render the next rc's cask and `brew upgrade --greedy --cask isshoni-ci/test/isshoni` (the cask has `auto_updates true`, so plain `brew upgrade` skips it; alternatively note that the app's own updater updated it); `brew uninstall --zap --cask isshoni-ci/test/isshoni` (04's tap is final-only, 04 §10.7 item 4c) | opens without a dialog (the postflight strips quarantine), or with exactly the documented Open Anyway path once (recorded; 04 §6.6: without the postflight if `brew audit --cask --strict` refuses it); the upgrade keeps grants (as M-MAC-8); zap removes the folders of 04 §6.6 (the data root and the OS-managed folders); **Keychain items remain**, as the cask's caveat says (the script's `--uninstall` forgets credentials) |
| M-MAC-4 | all | Deny "System Audio Recording" in the wizard; then share with sound; then reset with `tccutil` and leave the prompt unanswered for 30 s with a sound playing; then grant from the guided fix | denied: banner `audio-permission` (cause `denied`), silence sent, "Open System Settings" opens the right pane (`x-apple.systempreferences:`); unanswered: the wizard shows 01 §9.3's `not_determined` panel after 30 s ("macOS hasn't shown or answered its dialog…" with [Try again] [Open System Settings] [Continue without sound]); during a share the banner is `audio-permission` cause `pending` and **stays `pending`** after 30 s with a sound playing (03 §13.2: preflight still not-determined → event `permission_pending`, detail `watchdog`; never "macOS blocks isshoni" for a dialog nobody answered); the wizard's sound check returns `reason: permission_pending` after its 30 s wait (03 §13.3), shown as 01 §9.3's not-determined panel. (The `denied`-by-watchdog path, preflight `unknown` with audio playing, is not a manual row: 03's native unit and integration tests cover it with the preflight SPI stubbed out); the app stays responsive; granted: the next share has sound without restarting the app (or, if 03 needs a restart, the UI says so) |
| M-MAC-5 | all, + `macos15-0` | Share → the OS picker: cancel; pick a window; pick a display; pick an app (where offered); share a second time; with isshoni's window centered over the target window, pick that target in window mode | cancel returns to the share sheet with no error; each kind goes live and decodes at the viewer (27; VMs: the pick completes, then the `no_hardware` notice); the share sheet names the source (15.2+) or its kind (before 15.2); isshoni's windows are never offered (`excludedBundleIDs`); **no Screen Recording prompt and no "continue to allow" reminder ever** (the picker needs none); the picker is never shown without the user clicking Share; the target window under a centered isshoni window can be picked without moving isshoni (01 §10.1's `PickPreparer`, as 02 EM8 recorded for this version) |
| M-MAC-6 | all, + `macos15-0` | (a) Share the full display with the isshoni window on top and a share-sheet menu open, then a window of another app. (b) **Fallback, on the 14.4 VM** (and wherever C-1's outcome is `fallback-needed`, e.g. `macos15-0`): turn off Settings → App "Hide isshoni windows from screen capture", then share the whole display; during the share bring the window back from the Dock; then end the share | (a) isshoni's windows (WKWebView content included) never in the stream (the strategy C-1 recorded for this OS); the result agrees with C-1. (b) the first-share sheet shows the `selfVisible` line ("isshoni will minimize itself while you share your whole screen…"); the window minimizes when the share turns live; in-app viewer audio is muted with the `viewer-muted` banner (macOS text, 01 §10.2); bringing the window back shows "Friends can see this window while you share your whole screen." with [Minimize]; viewer audio is unmuted when the share ends. An isshoni window never appears in the picker (`excludedBundleIDs`), so an "own-window pick" is not a case. The fallback branch can't be reached on 27 (15.2+ always excludes, 01 §10.2), and a VM share goes live only if V1 finds a hardware encoder in the guest: without one, the VM checks (b) up to the first-share sheet's `selfVisible` line, and the live half (minimize at live, mute and banner, the restore line, unmute at the end) is asserted by the WK spec with the e2e flavor's fake engine reporting `SelfVisible` (§14 E4); the record names which applied |
| M-MAC-7 | 27; M-OS-STOP also on the 15 VM | In a full-screen game or video: Pause, Resume, Stop hotkeys; then stop from macOS's own screen-sharing menu bar item (15+) | "Paused" with the last frame and silence within 1 s (a phone joining sees the Paused tile); resume within 1 s; stop ends the share; the system stop ends the share with the panel line "You stopped sharing from the macOS menu bar" and **no** notification (01 §10.3: `os_stopped` → `endReason` `user`) |
| M-MAC-8 | all | Install rc N−1 and rc N, **both from `release.yml`** (release identity); set the app's update channel to Beta first (the stable channel never lists an rc); grant everything (incl. notifications), share once; update to rc N in the app (Updates → Install), **started from an `isshoni://` room link**; share again | **no prompt of any kind after the update**; the relaunched app opens the room; capture with sound within 1 s of Share (27; VMs: the audio grant through `audiotest`'s audio-only session); the sound check passes; `codesign -dr - <app>` shows the same designated requirement; notification authorization kept (Settings → Permissions shows Allowed; a test notification is shown; closes WM8's re-sign part); the Dock item, the login item and `isshoni://` links still work; `updates/helper.log` shows one apply. Downgrades and expired manifests are not repeated by hand: 04 §9.1 case D and M2's `update_test.go` expiry test cover them. This closes S2's "N → N+1 keeps the capture grants on 14.4, 15, 26 and 27" |
| M-MAC-9 | 27, one VM | Link; quit; relaunch; update (M-MAC-8); sign out | Keychain Access lists the item under 02/01's service name; relaunch and update start linked with **no** "wants to use your confidential information" prompt; sign out deletes it |
| M-MAC-10 | 27 | Open `isshoni://<host>/…` from Safari and Chrome, with the app closed and running; a link to another host | closed: the app starts and opens the room; running: forwarded to it; another host: "Add server" confirmation before any request |
| M-MAC-11 | 14.4, 27 | Turn autostart on; log out and in; turn it off | Login Items lists isshoni (SMAppService); after login it starts in the menu bar without a window; off removes it; macOS's "Background item added" notice appears once |
| M-MAC-12 | 27 | While live: lock (Ctrl+Cmd+Q) and unlock after 1 min; after 6 min; after 31 min; close the lid (sleep) and open it | as M2's M-DESK-6: 1 min paused then live; 6 min "Resume sharing?"; 31 min stopped with the notice; sleep stops the share, notice within 5 s of wake; the tap and SCK come back clean on the next share |
| M-MAC-13 | 27 | With an external display: share each display; different scale factors (Retina + 1080p); unplug the shared display mid-share; ProMotion at 120 Hz | each display goes live at the right size; unplug → `SOURCE_GONE`, stopped with the notice; 120 Hz capped to 60 fps at the full layer |
| M-MAC-14 | 27 | Close the window while live; click the Dock icon; use the menu bar item's menu; Stop with the window hidden; Cmd+Q while live; Dock → Quit while live; log out while live | hidden with the one-time notice; the Dock icon stays while live and clicking it shows the window; after Stop with the window hidden the Dock icon goes away (01 D4, §7.2); a red menu bar icon whose title is the watcher count, with tooltip and VoiceOver label "Sharing <source> · N watching" (01 §7.4); Cmd+Q / Dock Quit while live asks "Stop sharing and quit?"; confirming stops the share and exits within 3 s; logout isn't blocked |
| M-MAC-15 | 27 | While live with the window hidden, start an app not on the list that holds the mic (03's fixture `FakeVoIP.app`) | a notification with Keep out / Include; the choice applies and is remembered; with the app closed, clicking the notification opens isshoni on the prompt |
| M-MAC-16 | 15, 26, 27 | Add a server on a LAN address (or a LAN viewer for ICE); deny once, then allow | the Local Network prompt once; denied: 01's guided message; allowed: works; kept after M-MAC-8's update |
| M-MAC-17 | 27 (Rosetta); Intel if available | `open --arch x86_64 -a <app>` (the app's x86_64 slice); share 1080p60 with sound to a viewer; `ISSHONI_HARNESS_ARCH=x86_64 ./harness.sh isshoni-audiotest run -gate` (§4.1) | Rosetta: the x86_64 slice links, shares, decodes at the viewer, audiotest exit 0 and the report's `machine.cpu` is "x86_64 under Rosetta" (03 §16.4; recorded as "Rosetta", never as Intel); an Intel Mac (owner's or a community report): the same plus the encoder in Details; on any Mac that isn't the owner's, the tester finishes with `./harness.sh --cleanup` and deletes the exit-tools folder (§4.1) |
| M-MAC-18 | 27 | VoiceOver on: the wizard, the share sheet, the menu bar item; Increase Contrast; larger text | every control has a name and is reachable; readable |
| M-MAC-19 | 27 | Production build: Safari → Develop menu; right-click outside text fields; pinch; two-finger swipe; drag a file and a link onto the window | isshoni isn't listed under Develop; no "Inspect Element"/"Reload"; no zoom, no navigation, no drop |
| M-MAC-20 | 27 | §8's budgets with Activity Monitor and `footprint` after 30 minutes of sharing 1080p60 while watching 1 focused share + 8 thumbnails | within §8; numbers recorded in §8's table and the record |
| M-MAC-21 | 27 | Watch a share in the app; fullscreen (F) and Esc; "Open in the isshoni app" from a Chrome tab with the app running | stage fullscreen and back; handoff: the tab shows "Now watching in the isshoni app", sound only from the app, and the app's playback is kept out of its own share (03's WKWebView GPU-process rule) |
| M-MAC-22 | 15, 26, 27 | App Management (04 §10.7 item 7, M-APPMGMT): update through the in-app updater (a) a copy installed by the DMG + Open Anyway path and (b) one installed by the script; (c) run the script over an existing copy | each: the swap is done, or the update ends with `app_management` (in-app) / exit 8 (script) with nothing half-replaced and 01's guided fix shown; the record says which |
| M-MAC-23 | all | Run the app from the mounted DMG (not copied); open Settings → Updates (04 §10.7 item 4d) | Updates shows "Move isshoni to Applications" (`not_installed`, reason `read_only`/`translocated`) and nothing is downloaded |

**02's manual checks** (02 §17.5 hands them to this record):

| 02 check | Covered here by |
|---|---|
| M-PICK-MAC | M-MAC-5 plus `enginetest pick` on 27 and every VM (incl. `macos15-0`) |
| M-SCK-WINDOW, M-SCK-DISPLAY | M-MAC-13 plus a window row: 02 §17.5's window steps (resize, minimize/restore, other Space, full-screen app, close → `source_gone`) on 27 |
| M-SELF-EXCLUDE | C-1 and M-MAC-6, with 02 §8.8's filled-in table copied into the record |
| M-SLEEP | M-MAC-12 (27; VM suspend for the VMs) |
| M-OS-STOP | M-MAC-7 (27 and the 15 VM) |
| M-BENCH-MAC | `bench-macos27.txt` (02's `hwcheck.sh` bundle, redacted) |
| M-SOAK-MAC | §8's soak (`enginetest soak -hours 2 -audio tone`) |
| M-AVSYNC-MAC | A-9 |

---

## 8. Budgets and soak (mirror of M2 03 §20)

| What | Budget | Measured by |
|---|---|---|
| Startup to window | ≤ 1.5 s warm, ≤ 3 s cold (after a reboot) on the M5, from `open -a isshoni` to the window shown (no shell relaunch on macOS) | M-MAC-20 (the app log's `start` → `window shown` stamps, 01); WK spec trend line on CI |
| Memory: the app process | physical footprint ≤ **150 MB** while sharing 1080p60 (Activity Monitor's "Memory", `footprint <pid>`) | M-MAC-20 |
| Memory: WebKit processes of the app (WebContent, GPU, Networking; responsible PID = the app) | ≤ **350 MB** together while watching 1 focused share + 8 thumbnails | M-MAC-20 |
| Capture → packet (engine) | p95 ≤ 25 ms at 1080p60 with VideoToolbox, layer `f` encode p99 ≤ 16 ms (02 §14, M2 04 §15); S2's encode p99 6.8 ms at 1080p leaves the rest to SCK | `enginetest bench -source mon:<main>`, `task mac:hw:engine` |
| CaptureToSend (core) | p95 ≤ 35 ms, p50 ≤ 20 ms | `TestCaptureToSend` darwin |
| Pick → live | ≤ 2 s after the pick (SCK start + VT prepare + the core's C2) | M-MAC-5 (recorded) |
| Quit while live | exit ≤ 3 s **measured from the confirmation** of "Stop sharing and quit?" (01 §7.5); share gone at the viewer ≤ 2 s | WK spec (asserted, §6.4) |
| Pause → silence and last frame at viewers | ≤ 1 s | M-MAC-7 |
| Audio device change → heard again | ≤ 2 s | M-R5 (A-6) |
| Sample-rate change → heard again | 03's case 33 timing | A-1 (case 33) |
| Permission prompt watchdog | 30 s, then the guided state (01 §9.3's not-determined panel; the banner stays cause `pending`, event `permission_pending` detail `watchdog`; `denied` only with preflight `unknown` and a sound playing) | M-MAC-4 (the `pending` path); 03's native unit and integration tests with the preflight SPI stubbed (the `unknown` → `denied` path) |
| Soak (engine) | `enginetest soak -hours 2 -audio tone` on the M5: 60 s samples, M2's rule (growth ≤ 5 % after 10 min, no `RING_OVERFLOW`, no `ENDED`) | `task mac:hw:engine`; M-SOAK-MAC |
| Soak (app) | 2 h on the M5: share 1080p60 with sound (Movie preset) while watching 1 + 8 in the app; footprint growth ≤ 10 % between minute 10 and minute 120 for both memory rows above; no SCK stream restart, VT re-init or tap rebuild without a logged cause; audio counters: 0 underruns, 0 trims, `maxResamplePpm` recorded | the app's 2-hour session with `counters` read after (M2 05 §19.4's check) and footprints sampled with `footprint` |
| Audio soak | `audiotest run -gate -long` case 23 (30 min) passes | minor releases, on 27 |

Footprint is higher than M2's 120 MB private working set on purpose: on Apple silicon's unified memory a process's
physical footprint includes its IOSurfaces (SCK's frame pool and VideoToolbox's buffers), which Windows' private
working set leaves out. The numbers are budgets until M-MAC-20 measures them; a miss is recorded with the cause and
becomes an M5 item unless it is above 2× (then a fix slice before the exit).

**The macOS latency budget at 1080p60** (for diagnosing an L-1 miss; M2 01 §12.1's tables with the macOS stages):
capture (SCK frame after composition) 8–17 ms; NV12 straight from SCK, downscale for `q` only (VTPixelTransferSession,
S2: 1.5–2.1 ms, off the `f` path); encode (VT LLRC) 5–7 ms; the rest as Windows (≈ 70–160 ms video). Audio: the tap's
IO buffer (the aggregate's 512 frames, ≈ 11 ms) plus the ring and 10 ms chunks, Opus ≈ 17 ms, then as Windows: the
lip-sync hold decides the sound-on number, and 03's stamps (chunk time = the tap's host time, without the output's
latency) are lever 1, which A-1's `timestamps` case checks before B runs.

---

## 9. The exit run book (outline for `docs/m3/exit-test/README.md`)

Slice V12 writes it out with exact commands, plus `results-template.md` (the M2 template's shape with one column per
macOS version) and `vm-lab.md` (V1's findings and the golden-image steps). Where they differ, the specs win and the
book is fixed. The book's privacy rule is M2's: no computer, VM, user, server or network names or addresses in the
record or beside it; WAVs, `-dump` crops, `-no-redact` reports and the downloaded tools never go into the repository.

### 9.1 Preconditions

- `v0.3.0-rc.<N>` tagged; its `release.yml` run green including `macos-pkg` (which uploads the exit tools); the rc
  server (M1's exit-test VPS, RTT ≤ 50 ms from the Mac) upgraded to the same rc (M1 run book §1.2's command).
- Each machine (M5 on 27; the M5 booted from its 26 volume; fresh clones of `macos14-granted`, `macos15-granted`;
  `-clean` clones of `macos14`, `macos15`, `macos26` for the first-run C rows): install from `https://<rc
  server>/download` with the page's curl line; `--version` of the installed app prints `0.3.0-rc.<N>`; link it.
- Exit tools: download `isshoni-exit-tools-macos` on the M5 (browser, signed in to GitHub), unzip into the shared
  folder, then on each machine:

  ```sh
  cd "/Volumes/My Shared Files/exit/isshoni-exit-tools-macos"   # on the M5: the unzipped folder
  shasum -a 256 -c SHA256SUMS                  # every line OK, else stop
  grep -F 'v0.3.0-rc.<N>' README.txt
  xattr -dr com.apple.quarantine "isshoni Exit Harness.app"
  ./harness.sh isshoni-audiotest selfcheck     # ends with "selfcheck: ok"
  ./harness.sh isshoni-enginetest probe        # VideoToolbox: hardware on the M5; what the VM has
  ```

- Each machine: a microphone (VM: the host's), output to built-in speakers or a wired device, nothing else playing,
  Discord desktop signed in, Chrome, Safari; on the M5 also Steam with one game. Before A-1, quit Discord, Steam and
  every voice app and anything else that plays sound (03 §16.4's silence pre-check makes a case INCONCLUSIVE if other
  apps play). Each mic fixture has Microphone access (`isshoni-audiotest fixture mic-status`).

### 9.2 A. Audio (≈ 15 minutes per version)

1. `./harness.sh isshoni-audiotest run -gate -out ~/isshoni-exit/<os>` → exit 0, or exit 3 under A-1's mic rule
   (A-1); compare the report's `engine.commit` with `exit-tools-manifest.json` and `tool.sha256` with `SHA256SUMS`
   (A-2).
2. Real-app checks with `./harness.sh isshoni-audiotest record -label MR1 -d 30 -o mr1.wav` (and the other M-R rows
   as §2.1 lists per version, `-label MR<n>`), answering `Pass? [y/n]` after listening.
3. `./harness.sh isshoni-enginetest-hooks avsync -source pick -out avsync-<os>` (VM: add `-encoder fake`), picking
   the window `AVSync Flash` → exit 0 (A-9).
4. Copy the report, `manual-*.json` and `avsync.json` to the M5's git-ignored `.dev/exit-test/<date>/<os>/`; merge
   with `go run ./cmd/isshoni-audiotest merge …`; commit under the names of §4.4 and add the harness README rows.

### 9.3 B. Latency (≈ 25 minutes each on the M5's 27 and 26; ≈ 10 minutes L-F per VM)

1. Display at 60 Hz (System Settings → Displays → Refresh Rate), AC power, Low Power Mode off, one display (or the
   main display of several: g2g captures `CGMainDisplayID`).
2. `./harness.sh g2g serve` (no grant needed; keep it running); open `http://127.0.0.1:8765/` in **Safari**, **Start**
   (the tone plays), then make the window fill the display without entering a full-screen Space (Window → Fill on
   15+, or Option-click the green button: a full-screen Space would hide the Chrome window).
3. In the app: Share → the picker → **the display**, preset Auto, Share sound on; Details: the full layer at the 1080p
   budget at 60 fps and about 8 Mbit/s, the encoder VideoToolbox (hardware).
4. Chrome, same user, the room; focus the share, its speaker on; "Watch here instead" if shown; Chrome at 50 % over the
   bottom-right half, the clock strip visible.
5. What friends hear: Safari heard, Chrome kept out as your viewer (else stop).
6. `./harness.sh g2g measure -d 60s -run gate -server https://<rc server> -o g2g-macos<26|27>-gate.json` → exit 0 (L-1);
   then `nocuts` (`?cuts=0`) and `nosound` (a new share with Share sound unchecked), and
   `./harness.sh isshoni-enginetest bench` (L-3).
7. VMs (L-F): the app's Share shows the `no_hardware` message and doesn't start; then
   `./harness.sh isshoni-enginetest-hooks capture -source pick -encoder fake` → exit 0 (pick the VM's display). If
   V1 found a hardware encoder in that guest: the same share from the VM's app to Chrome on the host, frames and tone
   at the viewer, and `g2g measure … -run gate -o g2g-macos<ver>-vm.json` recorded, not judged.

### 9.4 C. Release-candidate checks

C-1 (`protect-check`) per version, then the M-MAC rows of §2.3 on the version columns named there. 04's release
checklist items (install paths, rc → rc update, uninstall) are run on the same rc when convenient and recorded in the
release-prep PR.

### 9.5 D. Record

`docs/m3/exit-test/results/<date>.md` from the template, and `docs/m3/exit-test/results/<date>/` with
`g2g-macos{26,27}-{gate,nocuts,nosound}.json`, `capture-macos{14,15-0,15}-vm.txt` (L-F's fake-encoder capture; or
`g2g-macos<ver>-vm.json` where a guest had an encoder), `avsync-macos{14,15,26,27}.json`, `bench-macos{26,27}.txt` (02's
`hwcheck.sh` bundle, M-BENCH-MAC), `vm-versions.json` (each guest's product version and build incl. `macos15-0`, Tart's
version, the restore image builds). The record names the four audiotest report paths (with any mic-case SKIPs of A-1's
exit-3 rule), every M-MAC row with its result and, for one carried over, its rc; every WM row of 01 §3.2 with its result
(one per OS version for the rows 01 marks "Mac, all"); 02's manual checks through §7's mapping table (M-PICK-MAC,
M-SCK-WINDOW, M-SCK-DISPLAY, M-SELF-EXCLUDE with 02 §8.8's filled-in table, M-SLEEP, M-OS-STOP, M-BENCH-MAC, M-SOAK-MAC,
M-AVSYNC-MAC); and 02 §14's component numbers (layer `f` encode p99 and capture → packet p95 from `bench`) beside the
g2g result. Before committing, `grep -rniF '<one name>'` over the added files for each of: the Mac's and each VM's
computer name, the user name, the server's domain, every IP address; no match may remain.

On a failure: a fix slice and a new rc. Every new rc repeats A-1, A-2, A-9 and B on every version (the tools carry
the rc's commit, so an older report's `engine.commit` never matches); the M-R rows, A-3 and the C rows repeat only after
a change that can affect them (`native/macos/`, `internal/media/`, `internal/desktop/`, `cmd/isshoni-desktop/`, the
packaging), otherwise they carry over naming their rc.

### 9.6 Release tiers on macOS (plan "Verification", from `v0.3.0` on)

- **Every release**: `audiotest run -gate` and M-R1 on 27 (bare metal), `avsync`, a smoke (M-MAC-1 on a `-clean`
  clone of the newest VM, M-MAC-8 from the previous release on 27); the report attached to the GitHub Release as
  `audiotest-macos27.json`.
- **Minor releases**: the full exit matrix (§2) on the 14.4/15 VMs, 26 (bare-metal volume, plus the 26 VM's first-run
  rows) and 27, `run -gate -long`, the macOS rows of the plan's matrix; after `macos-14`'s retirement, `macos-compat`'s
  steps in the 14.4 VM.
- **A new macOS major** (the yearly beta, e.g. 28): `task mac:hw` and `audiotest run` on a beta VM as soon as Apple
  ships a beta restore image; a failure is a release-blocking issue for the first release after that macOS ships
  (§11 R1).

---

## 10. How M3 closes S2's open items

| S2 open item (plan) | Closed by |
|---|---|
| macOS 14.4, 15, 26 (audio, the 44.1 kHz resampler path on a real device) | A-1 (incl. case 33, the 44.1 kHz rate change, on 14.x's own resampler; the "real device" in a VM is the guest's HAL device), A-4 (M-R1) and A-9 in the 14.4 and 15 VMs and on the M5's bare-metal 26 volume; A-3/A-5 (M-R8, M-R4) on 14.4; case 33 also on 27 bare metal |
| Permission persistence across updates on 14.4, 15, 26, 27, incl. Local Network on 15+ | M-MAC-8 (rc N−1 → rc N, both release-signed) and M-MAC-16 on each version (fresh guest TCC per clone); `task mac:hw` re-proves it on every dev rebuild (V-D6) |
| Picker and window exclusion | M-MAC-5, M-MAC-6, `protect-check -sck pick` (C-1) on every version and on `macos15-0` (the pre-15.2 path); the picks complete in VMs and the live video half runs on 27 (VMs have no encoder: `no_hardware`, with the fake-encoder `capture -source pick` as the VM's functional check, §2.2 L-F); the fallback (auto-minimize and mute) in M-MAC-6 (b); `TestHWListedDisplay` and `TestHWProtectListed` on the M5 |
| DMG "Open Anyway" and curl install | M-MAC-1, M-MAC-2, M-MAC-23 on every version (`-clean` clones: a first launch with no history), M-MAC-22 on 15/26/27 |
| Real Discord | M-R1 (A-4) on every version, M-R2 with Discord Web on 27 |
| Default-output changes mid-share | M-R5 (A-6: 27 real devices; 14.4 a Multi-Output Device where 03 lists it), `MacTapHW` rebuild tests |
| VideoToolbox at 1440p60 (passed) | kept: 02's `TestHWVideoToolbox` asserts S2's numbers on the M5 (1080p60 and 1440p60 LLRC encode p99 ≤ 16 ms) so a regression in 02 or a new macOS shows |

---

## 11. Risks and fallbacks

| # | Risk | Fallback |
|---|---|---|
| R1 | A new macOS changes the tap API (silent taps, a new permission, `bundleIDs` semantics) | Beta VM runs (§9.6); the engine's sound check and zero-buffer rebuild keep it visible to users (plan "Main risks"); 03's degraded banner; a release note; the Chrome web sharer as the user's fallback |
| R2 | SCContentSharingPicker limits before 15.2 (no picked-content details; no rebuilt display filter, strategy B) | The share sheet shows the kind only (02/01); `protect-check`'s `pass-sharingtype` on 14.x and `fallback-needed` on 15.0/15.1 as 02 §8.8's table says; M-MAC-6 per version incl. `macos15-0` |
| R3 | **A guest version won't boot on the M5, or has no usable output device for taps, or its taps are silent** (V1 records boot and the virtio output/input per version under Tart, otherwise UTM, as its first result) | That version's A and C rows move to: the M5's 26 volume (for 26), a borrowed Mac, or a community exit-harness report (14.4, 15; the tester ends with `harness.sh --cleanup`, §4.1), named in the record; the owner decides (§16 Q2). A lab blocked this way counts as "written down with its design consequence" for 03's AM0 (README's MA18), so the tap generation (MA26) is not held up |
| R4 | Apple no longer personalizes a 14.4.x restore image for VMs | Use the oldest 14.x it does; record it; the API floor (14.2 for taps) is still covered by `macos-compat`'s weak-linking check |
| R5 | VMs have no hardware encoder, so L-1 is bare-metal 26 and 27 only | V-D3; on 14.4 and 15 the per-version differences in the video path (SCK, VT availability flags) are covered by L-F (the `no_hardware` branch and the fake-encoder capture) and `TestHWVideoToolbox` on the M5 (26 and 27); signed off at the start (Q1) |
| R6 | GitHub retires `macos-14` or `macos-15-intel` | `macos-compat` moves to the 14.4 VM (minor releases); the x86_64 slice keeps its Rosetta tests in nightly on `macos-26` (02 §6.7; `softwareupdate --install-rosetta --agree-to-license` when missing) and loses only 04's native Intel launch check |
| R7 | A runner image starts pre-granting a TCC service | Nothing changes: tests run only with `ISSHONI_TEST_TCC` set (V-D6), which CI never sets |
| R8 | The harness's grant is lost after a toolchain or signing change | V-D6 makes it a failure in `task mac:hw`, which is the signal that the app's own updates would lose grants too |
| R9 | g2g's own SCStream competes with isshoni's capture | It runs at 20 fps without the cursor; the `nosound`/`nocuts` runs and a `bench` beside it tell the components apart; V5 measures g2g's cost (bench with and without g2g running) |
| R10 | Self-hosted runner on a public repo | V-D12: dispatch only, environment approval, ephemeral, started by hand |
| R11 | Chrome's lip-sync hold on macOS output devices (Bluetooth) | L-2 requires built-in or wired output; `nosound` shows the hold |
| R12 | The WK spec's driver changes what it tests (in-process events) | Its keys go through AppKit (more real than CDP's); the rest is the same page and bridge; manual rows cover pinch, swipe, drop and the real keyboard |
| R13 | Keychain prompts on CI for the e2e build | The e2e prefix's items are created by the same binary (no ACL prompt); if a runner image prompts, the e2e flavor on darwin selects 01's existing file store (`credentials.json`, 0600, in the e2e data root; 01 §13.1, the store unbundled builds already use) and the job records it. 01 notes that switch in §13.1; no new environment variable |

---

## 12. Test plan

| Kind | What is asserted | Where |
|---|---|---|
| Unit (Go) | `macenv` decisions and messages (fake env/probes); `skipcheck` against recorded `go test -json`/gtest XML (unlisted skip fails, listed skip that ran is a notice, non-`macenv` skip in scope fails); `g2g` darwin conversions and stamps; audiotest report `os`/`engine`/`tool` fields, names, `-vm` suffix, merge refusals, home-path redaction; `avsync` darwin pairing on recorded events; the harness launcher's argument and status handling | Mac, CI ubuntu (portable parts), CI macos |
| Unit (native) | 02/03's `native/macos` suites (02 §17.1: `vt_encoder_test`, `session_fake_test`, …) incl. the tap watchdog with an injected HAL; `macenv.h` skip/fail forms | Mac, CI macos (`macos-native` on `macos-26`; nightly `mac-arm64-asan`, `mac-arm64-tsan`, `mac-x64-ci` under Rosetta) |
| Integration | 02's `TestStaticConformance` (fake) and this doc's `TestStaticConformanceVT` (hw:h264: skips on CI); `TestTapWithoutGrant` (`IM_OK`, a permission/`tap-failed`/`no-device` state, silence packets, no block); the real-engine app link (`macos-native` step 4, 04's packaging in `desktop-macos`) | CI macos, Mac |
| Integration with grants | `isshoni_hw` suites of §5.2 | Mac (`task mac:hw`), VMs (`-granted`), optional self-hosted runner |
| Compatibility | `macos-compat`: newest-SDK binaries run on 14.x without dyld errors | CI `macos-14`; then the 14.4 VM |
| E2E | Desktop in Chrome (unchanged); the WK spec (01's WM rows of §6.5 + the end-to-end case with 01 §7.5's quit rules + budget trend lines) | CI ubuntu, CI macos (`macos-wk`), Mac |
| E2E (Windows unchanged) | the CDP spec and every Windows job after the helper move | CI windows |
| Exit tools | 04's `macos-pkg`: signatures, `SHA256SUMS`, `selfcheck`, `probe` | CI macos (release) |
| Manual | `M-MAC-1`…`23`, 03's M-R1…M-R10, 01's WM rows marked Mac, 02's manual checks (§7's mapping), `avsync`, `protect-check -sck pick`, g2g L-1, L-F | M5 (27, and 26 on its second volume), VMs (14.4/15.0-15.1/15/26), Rosetta; Intel and other Macs by community report |
| Soak | §8's engine soak (`soak -hours 2 -audio tone`), the app's 2-hour soak and `audiotest -long` | M5 |

---

## 13. Interfaces other docs rely on

- **`internal/testenv/macenv`**: `Require(t, needs...)`, the `Need` constants, `ISSHONI_TEST_TCC`
  (`screen,audio,mic,localnet`), `ISSHONI_TEST_HUMAN=1`, the skip message form `macenv: needs <need> (…)`; native
  `native/macos/tests/support/macenv.h` with `IM_REQUIRE(...)`. 02 and 03 gate every hardware or TCC test with them.
- **Build tag** `isshoni_hw` (M2's) for Go tests that need hardware or a grant on darwin.
- **`.github/macos-skips.txt`** and `tools/skipcheck` (flags §5.1); 02/03/01 add a ledger line with each skipping test.
- **Harness**: bundle ids `io.isshoni.dev.harness`, `io.isshoni.exitharness`; launcher grammar `harness [-status FILE]
  [-env K=V]… -- <tool> [args…]`; `harness.sh <tool> [args…]` with exit 70 for "no status" and
  `ISSHONI_HARNESS_ARCH=x86_64`; `task mac:harness`,
  `task mac:hw[:engine|:audio|:core|:g2g|:human]`; `Resources/fixtures/` (03's fixture apps) and `Resources/avsync/`.
- **`isshoni-exit-tools-macos`** artifact layout, `SHA256SUMS`, `exit-tools-manifest.json` (§4.5); 04's `macos-pkg`
  job assembles and checks it after `macos-sign-tools` has signed the harness with the test-tools identity
  `isshoni-testtools-1` (`deploy/keys/isshoni-testtools-1.pem`); `harness.sh --cleanup` (§4.1);
  `task mac:harness [VERSION=] [SIGN=dev|ci|none]
  [OUT=]`, `tools/macharness/sign-list.txt` and `tools/macharness/{harness,fixture}.entitlements` for 04's `sign.sh`.
- **`tools/g2g`** on darwin: `measure` through the harness; JSON `machine {virtual, arch, model}`; `osVersion()`
  string.
- **`isshoni-enginetest`** on darwin: two binaries (`isshoni-enginetest`, `isshoni-enginetest-hooks`); `-dll`
  Windows-only; `avsync` helper bundles `io.isshoni.test.avsync.flash`/`.click` and their Unix-socket JSON lines
  (`{"kind":"flash","pNs":…}`, `{"kind":"click","aNs":…}`); `protect-check -json` `outcome` tokens
  `pass-filter`, `pass-sharingtype`, `fallback-needed`, `fail` (exits 0/0/2/1; 02 §13.2 adopts them), judged against
  02 §8.8's per-OS table (C-1).
- **audiotest report** fields `os`, `engine`, `tool`; report and committed file names (§4.4).
- **The e2e driver protocol** (§6.2), `ISSHONI_E2E_DRIVER=1`, `<ISSHONI_E2E_DATA_DIR>/e2e-driver.json`, the e2e-only
  bound service `E2E.Report` in the e2e build's `assets.GuardWKWebView` allowlist; `web/e2e/wk/` and
  `web/e2e/desktop/common.ts`; `task e2e:wk`; the `ci.yml` job `macos-wk` and the artifact
  `isshoni-desktop-macos-e2e`.
- **Row ids**: 01's `WM1`–`WM26` (§6.5 says which check covers each), `M-MAC-1`–`M-MAC-23`, 03's `M-R1`–`M-R10`
  (`record -label MR<n>`), `A-*`, `L-*`, `C-*` rules.
- **Budgets** (§8): the macOS counterpart of M2 03 §20; 01 provides the `start`/`window shown` log lines (01 §15);
  M-MAC-20 measures them.
- **Record paths**: `docs/m3/exit-test/{README.md,results-template.md,vm-lab.md,results/<date>.md,results/<date>/}`.

---

## 14. Depends on (assumed interfaces of the parallel docs)

- **01-desktop-macos.md**: (E1) the e2e flavor builds on darwin (`flavor_e2e.go` already allows `darwin && cgo`) and
  honours `ISSHONI_E2E_DATA_DIR` on darwin, and hosts `e2edriver_darwin.go` with §6.2's protocol; (E2) the app logs
  `start` and `window shown` with monotonic stamps; (E3) `--version` and `--self-check` (with `testHooks` and a menu
  dump) on darwin; (E4) own-window exclusion via the engine's self filter and the auto-minimize-and-mute fallback
  (01 §10.2), with the e2e flavor's fake engine able to report `SelfVisible` (01's fake `selfVisible` source) so the
  WK spec can assert the fallback's live half (M-MAC-6 (b)); `protocol.DesktopAssetOriginMacOS`; the WM rows
  (WM1–WM26), their fallbacks and the results table (01 §3.2); 01 §7.5's quit rules (`ShouldQuit` while live →
  `confirm-quit`); `desktop-macos`'s e2e-flavor bundle build (01 §19.2), uploaded as `isshoni-desktop-macos-e2e`;
  `--relaunched` skipping the LaunchServices relaunch (01 §6.1 step 2); the file-store switch for the e2e flavor
  (R13, noted in 01 §13.1); Keychain service naming; deep links via Apple Events; `SMAppService`; lock/sleep behaviour
  equal to M2 03 §11.6; optionally a pointer in 01 §19 to §8 here as the place of its budgets.
- **02-macos-engine.md**: (E5) **resolved by 02**: the release engine never opens a software encoder; with no
  hardware encoder `im_probe` and `im_start` report `IM_ERR_NO_ENCODER` `no_hardware` (02 §5.4, §9.10), so VMs take
  L-F's `no_hardware` branch and CI's VT coverage is structural (§5.6); (E6) the test-hooks link selection for Go
  (`-tags imstatic,imtesthooks`: luma tap, `IM_ENC_FAKE`, `test:pattern`, `IM_START_ALLOW_LISTED_SOURCE` with
  `mon:<CGDirectDisplayID>`), the presets `mac-arm64-ci`, `mac-x64-ci`, `mac-arm64-asan`, `mac-arm64-tsan`,
  `mac-x64-release` and `task native:mac PRESETS="…"` (02 §6.1, §6.5), the `macos-native` job on `macos-26` whose
  steps this doc extends (02 §6.7), the tests `TestStaticConformance`, `TestStaticSoftwareVT`, `vt_encoder_test`,
  `TestHWVideoToolbox`, `TestHWListedDisplay`, `TestHWProtectListed`, and §17.5's manual checks handed to this record;
  (E7) `im_build_info` JSON with keys `engine` (the SemVer), `commit`, `abi`, `testHooks` (plus `sdk` and `minOS`);
  02 §13.2's `protect-check -json` `outcome` tokens and exits of §4.3.2, the `_other.go` constraint
  `!windows && !(darwin && cgo && imstatic)`, `sessions` ≤ 4, `soak` 60 s samples; 02 §8.8's filled-in strategy
  table per OS; (E8) the
  engine clock is `mach_absolute_time` in ns; capture stamps are SCK's display time; (E9) a self filter that excludes
  the calling process's windows (used by the app and by `protect-check`), and a flag when a pick can't be filtered;
  (E10) the engine never calls SCShareableContent or opens a tap unless asked (so CI never prompts).
- **03-macos-audio.md**: (E11) `driver_darwin.go`, fixture apps as signed bundles launched through LaunchServices,
  the darwin case table with required flags (case 33 for the 44.1 kHz rate change), M-R1…M-R10 with their OS
  columns (`record -label MR<n>`), §16.6's pass rule, §16.10's exit-3 rule for guests without a microphone, the
  `fixture mic-status` verb (03 adds it to §16.3's list), `selfcheck` without sound or TCC; (E12)
  `im_test_render_bursts` on darwin returning predicted device times in mach ns; (E13) an audio-permission flag in
  the probe (the preflight); audio never fails `im_start`: without the grant `im_start` returns `IM_OK`, the session
  sends silence and `im_session_info.audio_state` is `IM_AUDIO_STATE_PERMISSION` (report state `permission-pending`/
  `permission-denied`, or `no-device` without an output device, 03 §13.4); the 30 s watchdog (03 §13.2: preflight
  `pending` stays `pending` with event `permission_pending` detail `watchdog`; `denied` only with preflight `unknown`
  while a heard process had output running) with an injectable HAL and a stubbable preflight for its unit and
  integration tests; (E14) the `audio-permission` banner (`params.cause` `pending`/`denied`) and the sound check's
  `reason: permission_pending` / `permission_denied`. The default-output-device change stays manual (M-R5); no helper is
  assumed.
- **04-macos-release.md**: (E16) `deploy/macos/dev-signing.sh` (the dev identity), `deploy/macos/sign.sh` as the only
  signer (modes `release|ci|dev|testtools`, its tool form for the harness bundles from
  `tools/macharness/sign-list.txt`), and the release jobs `macos-app` (builds the harness unsigned, `SIGN=none`),
  `macos-sign` (signs only the app, runs no built code), **`macos-sign-tools`** (environment `testtools`, secrets
  `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` only; signs the exit harness with `sign.sh -mode testtools`, runs no built
  code; in a dry run the ephemeral identity) and `macos-pkg` (assembles and checks the exit tools, §4.5); the test-tools
  identity `isshoni-testtools-1` made with 04 §5.1's commands (no backup needed: losing it only costs one re-grant in
  each `-granted` image), its certificate committed as `deploy/keys/isshoni-testtools-1.pem` and checked by `repocheck
  keys` outside the codesign active/backup pair; (E17) the universal `.app` (arm64 + x86_64,
  `MACOSX_DEPLOYMENT_TARGET=14.4`, `LSMinimumSystemVersion` 14.4) with hardened runtime and only the `audio-input`
  entitlement, **built once on 04's build runner (`macos-26`, newest SDK)** in `desktop-macos` and uploaded as
  `isshoni-desktop-macos-pr`; §5.5's checks and `machocheck`; `macos-runtime` with `macos-14` added to its matrix and
  `runtimeRunners` (the app's `--version`/`--self-check`/dyld checks on 14); the Xcode-select step from
  `deploy/macos/tools.json`; direct exec of e2e builds (04 §2.4); (E18) `install-desktop.sh`, the DMG with its "Open
  Anyway" guide (04 §5.9), the Homebrew cask template and `deploy/homebrew/render.sh` (final-only tap, `auto_updates
  true`, zap's reach, 04 §6.6), the updater's `not_writable`/`read_only` reasons and `app_management` code,
  `update-e2e.sh` case D (downgrade) — M2's `update_test.go` covers expiry; (E19) the release checklist that links this
  record (04 §10.7).
- **M2 as built**: `tools/g2g` (01 §12.2), `isshoni-enginetest` (04 §14), `isshoni-audiotest` (05 §18), the CDP spec
  and annotations reporter (03 §22.4 B), `mediatest.Conformance` (01 §6.4), M2's exit record shape.

---

## 15. Implementation slices

Each slice is a PR with its own tests; "Manual:" items go into the PR, as in M2. V1 and V2 can start at once.

| Slice | Scope | Acceptance | Verified where | Depends on |
|---|---|---|---|---|
| **V1** VM lab | first result: per version, whether the guest boots on the M5 and has a usable virtio output and input device under Tart (otherwise UTM), so §11 R3 is decided early; the M5's second APFS volume with the latest 26.x (V-D2); Tart golden images for 14.4.x, 15.0/15.1 (`macos15-0`), 15, 26 (`-clean`, `-granted`; each with an admin and a standard account); run S2's tools (`s2 probe`, `run-selftest.sh`, `s2-mac-vt -list-encoders`) in each; write `docs/m3/exit-test/vm-lab.md` | `vm-lab.md` has, per guest: boots (yes/no, with Tart or UTM), output and input device present; product version and build; SCK picker works; tap self-test result; VT encoders (hardware or not); mic present; Local Network prompt (15+); the guest's `ioBufferMs` (the aggregate's BufferFrameSize, 03 §16.5) and case 15's cut distribution (five runs); case 13v's device rows where a guest input exists (03 §10.2); the commands to rebuild the images | VMs on the M5 | – |
| **V2** `macenv` + `skipcheck` + ledger | `internal/testenv/macenv`, `native/macos/tests/support/macenv.h`, `tools/skipcheck`, `.github/macos-skips.txt` (empty), Taskfile `lint` wiring | unit tests (§12); a fixture run with an unlisted skip fails; non-darwin skips say `macenv: needs darwin` | Mac, CI ubuntu, macos | – |
| **V3** Harness | `tools/macharness` (launcher with binpref, unsigned bundle assembly, Info.plist), `tools/macharness/sign-list.txt` and `{harness,fixture}.entitlements`, `harness.sh` (incl. `ISSHONI_HARNESS_ARCH`), `task mac:harness [VERSION=] [SIGN=dev|ci|none] [OUT=]` calling 04's `sign.sh` | a test binary in the harness sees `CGPreflightScreenCaptureAccess` true after one grant; three rebuilds keep it with no prompt (S2 re-proved for the harness); status propagation (0, 1, 2, 64, 70) tested; with `ISSHONI_HARNESS_ARCH=x86_64` a tool reports `sysctl.proc_translated` 1 | Mac; unit tests CI macos | V2, 04 (E16) |
| **V4** `task mac:hw` | suites of §5.4 over whatever hw tests exist; `summary.json`; grant-loss failure path | with grants: no `tcc:*` skips; after `tccutil reset`: the suite fails with the hint | Mac | V3 |
| **V5** g2g on macOS | `capture_darwin.{go,m}`, refresh rate, `osVersion`, `machine`, tests (§4.2) | `TestSCKCaptureHW` passes on the M5; `g2g measure` on Safari's clock page and a Chrome copy (no isshoni, Chrome showing the page itself at 50 %) reads Δ p95 ≤ 34 ms and decoded ≥ 99 %; bench with and without g2g running differs ≤ 2 ms p95 (R9) | Mac; CI macos (unit) | V3 |
| **V6** enginetest on macOS | darwin files (02 §13.2's constraints), two binaries, `-dll` refusal, probe/sources/pick/capture/bench/sessions/soak, `avsync` helpers, `protect-check` with the `outcome` tokens (with 02) | M5: `avsync -source pick` exit 0 (≤ 20 ms); `protect-check -sck pick` outcome as 02 §8.8's table says for 27; `bench -source mon:<main>` p95 ≤ 25 ms; `sessions -count 4`; VM: `avsync -encoder fake` exit 0, `capture -source pick -encoder fake` exit 0 | Mac, VMs; CI macos (`TestCommands*`) | V3, 02 (E5–E10), 03 (E12) |
| **V7** audiotest exit wiring | report `os`/`engine`/`tool`, names, `-vm`, merge refusals, redaction | M5: `run -gate` exit 0 through the dev harness; unit tests of names and merge | Mac; CI ubuntu/macos | 03 (E11, E13) |
| **V8** CI jobs | this doc's steps in 02's `macos-native` (`macos-26`), `macos-compat` (`macos-14`), `TestStaticConformanceVT`, `TestTapWithoutGrant`, ledger lines, `ci-ok`, the nightly steps on `macos-26` (02's asan/tsan/Rosetta plus `-count=3`) | all green with the ledger; the Windows jobs green and the CDP annotation rows unchanged | CI | V2, 02 (E6) |
| **V9** e2e driver + WK fixture | `e2edriver_darwin.{go,m}` (with 01), `web/e2e/wk/{driver,fixture}.ts`, the helper move to `web/e2e/desktop/common.ts`, `task e2e:wk`, the `macos-wk` job and the `isshoni-desktop-macos-e2e` upload in `desktop-macos` | WM1, WM2, WM7 pass on the Mac and in `macos-wk` (`macos-15`, `macos-26`); production `nm` check passes; CDP spec green on `windows-desktop` | Mac, CI macos, CI windows | 01 (E1), 04 (E17) |
| **V10** WK spec complete | every WM row §6.5 assigns to the WK spec (incl. WM6's direct second exec, WM14's confirm-quit, WM22's Edit keys), the production menu check of WM9, the end-to-end case, budget annotations | every such WM row annotated in `macos-wk` on `macos-15` and `macos-26`; quit while live shows "Stop sharing and quit?" and exits ≤ 3 s after confirmation; quit while idle ≤ 3 s | CI macos, Mac | V9 |
| **V11** Exit tools artifact | the artifact's content and checks (§4.5) in 04's `macos-pkg` job, after `macos-sign-tools` has signed the harness; `harness.sh --cleanup`; README.txt's cleanup note and fingerprint | an rc-like run (`workflow_dispatch` on a pre-release tag, or a snapshot) yields the artifact; on the M5 `shasum -c`, `selfcheck`, `probe` pass; the test-tools-signed harness keeps its grants across two rc builds; `--cleanup` removes them (the next run prompts again) | CI (release), Mac | V3, V6, V7, 04 (E16) |
| **V12** Run book and template | `docs/m3/exit-test/README.md`, `results-template.md` (outline §9) | a dry run of A and B on the M5 with a pre-rc build finds no missing step | Mac | V5–V7, V11 |
| **V13** Self-hosted option | `machw.yml`, runner notes in `deploy/macos/runner/README.md` | actionlint passes; if the owner registers a runner: one `hw` dispatch uploads a summary | CI (lint); Mac (optional) | V4, V11 |
| **V14** Budgets and soak | footprint sampling in `enginetest soak` and the WK spec; M-MAC-20's procedure | a 2-hour soak report on the M5 within §8, or misses recorded with causes | Mac | V6, V10 |
| **V15** Exit run (owner) | §9 on one rc across the four versions (26 and 27 on the M5's bare-metal volumes, 14.4 and 15 in VMs, the 26 VM for first-run C rows) | the record committed; PASS, or fix slices and a new rc | M5, VMs | all above; 01–04's slices |

---

## 16. Open questions for the owner

1. **Latency on 14.4 and 15 (answer before MA01 merges, not at the end).** VMs can't measure glass-to-glass
   (paravirtual display, expected no hardware encoder), and the M5 can't boot 14 or 15. Decided default: L-1/L-2 gate
   on bare metal 26 (the M5's required second volume, about an hour per minor release) and 27; 14.4 and 15 run L-F
   (§2.2), which narrows the roadmap's "the same checks on 14.4 through 27" for those two versions. An LLRC probe from
   a borrowed or community 14.x/15 Mac is recorded if one turns up, never required. The owner confirms this at MA01,
   before the EM/AM slices start.
2. **If a guest version won't boot on the M5, has no usable output device for taps, or its taps are silent** (V1
   decides, §11 R3): move that version's A and C rows to the M5's 26 volume (for 26), a borrowed Mac or a community
   exit-harness report (same rules, ending with `harness.sh --cleanup`), named in the record; or hold M3 until a real
   Mac on that version is found?
3. **Intel.** No Intel Mac is available. Default: ship the universal binary with the x86_64 slice tested on CI (Rosetta
   tests in nightly; native launch on `macos-15-intel` in 04's `macos-runtime`) and under Rosetta on the M5, labelled
   "community-tested" for Intel hardware. Accept, or drop the Intel slice until someone tests it?
4. **Self-hosted runner on the M5** (V13): register one for `machw.yml`, or keep running `task mac:hw` by hand
   (default)?
