# M3 design: overview and implementation plan

M3 is "macOS app": the same Wails desktop app as M2, made a real macOS app, with a static cgo + Objective-C engine
(SCContentSharingPicker, ScreenCaptureKit, VideoToolbox), the Core Audio process tap with every robustness rule of the
plan, a self-signed signing pipeline, the DMG, a curl installer, a Homebrew tap and the updater's macOS half. This page
is the short version of the five M3 specs (about 7,600 lines together). It says what M3 delivers, how the parts fit,
the rules every slice follows and the order in which the work gets built. `docs/PLAN.md` stays the source of truth for
product decisions; the five specs are the source of truth for their own parts; M1 (`docs/m1/README.md`) and M2
(`docs/m2/README.md` and its six specs) are the record of what exists. Where this page and a spec disagree, the spec
wins and this page gets fixed, except for the conflicts that §3 decides: slice MA01 writes those into the specs.

## 1. What M3 delivers

From the plan's roadmap ("Picker, VideoToolbox, taps with all robustness rules. Self-signed pipeline, curl installer,
Homebrew tap, updater. *Exit*: the same checks on 14.4 through 27"):
- **The app on macOS 14.4 through 27**, one universal binary (arm64 + x86_64; Intel best effort), the same Wails pin
  (`v3.0.0-beta.34`, M2 03 §3.1) and the same bundled SPA. A new darwin OS layer `internal/desktop/macos` (cgo +
  Objective-C) mirrors M2's `winos`: LaunchServices relaunch (isshoni is always its own responsible process), single
  instance, `isshoni://` through Apple events, a menu bar extra that turns red while live, Carbon hotkeys,
  notifications, `SMAppService` autostart, the login Keychain, the TCC permission step in the wizard and Settings →
  Permissions.
- **The engine** `libisshoni_media.a` (cgo + Objective-C++, linked statically) behind the same C ABI, now **v1.1**
  (macOS-only constants; Windows ignores them): the OS picker only, SCStream NV12 at half the frame interval,
  VideoToolbox H.264 with low-latency rate control at every size (two sessions, `VTPixelTransferSession` for the
  preview), the pixel-rate budget that turns 4K60 + preview into M2's "4K60 alone or 4K30 + preview" choice, and own-
  window exclusion per macOS version with the plan's auto-minimize-and-mute fallback.
- **Audio**: one global process tap (`stereoGlobalTapButExcludeProcesses`) on a private aggregate device clocked by
  the real output; voice apps by bundle-ID prefix and PID (Electron helpers through the responsible process), plus
  `bundleIDs` with `processRestoreEnabled` on 26+; rebuilds on process-list, default-device and sample-rate changes and
  on runs of zero buffers; isshoni's own WKWebView GPU process kept out by responsible PID; mic detection through the
  process's input devices; the 30 s permission watchdog; M2's `audiomix` for the 10 ms grid and Opus.
- **Distribution**: a self-signed identity `isshoni-codesign-1` with an explicit designated requirement (S2: grants
  survive rebuilds), an offline backup identity, hardened runtime with one entitlement, the
  `isshoni-desktop_<v>_darwin_universal.tar.gz` (updater, script, Homebrew) and a DMG with the "Open Anyway" path,
  `install-desktop.sh` served pre-filled by every server, the tap `MoonWX/homebrew-isshoni`, and the updater's
  `darwin-app` entry (same Ed25519 manifest, no downgrades) applied by a helper that swaps the bundle atomically.
- **Verification**: the macOS halves of `isshoni-audiotest`, `isshoni-enginetest` and `tools/g2g`; tests that need a
  TCC grant run only inside a signed harness app; every skip on GitHub's macOS runners is declared in a ledger; a
  real-app end-to-end test on WKWebView; macOS 26 on a second volume of the development Mac, and a VM lab there for
  14.4, 15 and 26's first-run rows.

Not in M3: Developer ID signing and notarization (*pending* the paid Apple program; a certificate change, PLAN), a
Sharing pill on macOS (*later (M5)*, only if users ask: macOS shows its own capture indicator), per-app meters in "What
friends hear" (one tap is one mixed stream; *later (M5)*), desktop "X started sharing" notifications (*later (M5)*, M2
03 D15), the Linux agent (M4). Each spec leaves room for these (01 §2, 02 §15, 03 §1, 04 header, 05 header).

**Exit criterion**: voice apps are excluded (the macOS audiotest gate passes) and glass-to-glass latency is under
200 ms at 1080p60, on macOS 14.4, 15, 26 and 27, on one release candidate `v0.3.0-rc.N`. How each version is checked
is 05 §2 (and §6 below): 27 and 26 run everything on the bare-metal Mac (26 on a required second APFS volume); 14.4
and 15 run in VMs, where the latency gate is replaced by a functional share check because a VM has no hardware
encoder. The owner signs this narrowing off at MA01 (§7 question 1).

**Where M2 stands**: M2's code is merged; its exit run and `v0.2.0` (M2 README WA90, WA91) are the owner's and still
open. If `v0.2.0` hasn't shipped when M3's release candidate is ready, both apps ship in the same tag and both
checklists apply (04 §10.7).

### The five specs

- [**01-desktop-macos.md**](01-desktop-macos.md): the app on macOS (D1–D22). `internal/desktop/macos` implements
  M2's `app.OSHooks` plus small optional interfaces found by type assertion; main-thread work goes through
  `OnMain`/`OnMainWait`. The asset origin `wails://localhost` is already accepted bearer-only (M2 02), so only the
  constant `protocol.DesktopAssetOriginMacOS` is added (a server change only if WM1 finds `Origin: null`). Start order:
  LaunchServices relaunch, `flock` + per-run Unix socket for single instance, `kAEGetURL` into the same `--open-url`
  command line, `--wait-pid`, `pending.json`. `LSUIElement` with the activation policy following the window. No
  Sharing pill. `sharingType = none` on every window; the engine's `SelfVisible` flag drives auto-minimize and the
  in-app viewer mute. Hotkeys ⌃⌥⌘P / ⌃⌥⌘S without Accessibility. Notifications through Wails' UNUserNotificationCenter
  service. TCC: System Audio Recording in the wizard (03's request and watchdog), Screen Recording never asked, Local
  Network handled where the first LAN connection fails. The M2 Wails checklist re-run as WM1–WM26; navigation lock
  layer 3 reads the WKWebView's URL natively. Credentials in the login keychain with no UI; CI's KC check proves a
  re-signed N+1 reads them without a prompt (fallback: a 0600 file store).
- [**02-macos-engine.md**](02-macos-engine.md): `native/macos/` in Objective-C++ with ARC on the unchanged
  `native/common`, built by the existing CMake root with `mac-*` presets and osx triplets (deployment target 14.4,
  Command Line Tools with SDK ≥ 26), merged by `libtool` into one `libisshoni_media.a` per architecture in
  `native/dist/darwin-{arm64,amd64}[-testhooks]/`. Go links it under the tag `imstatic` (`imtesthooks` for the test
  archive); without the tag darwin keeps M2's `unsupported_os` path. A C dispatcher `im_go_call` implements M2's
  `caller` interface and reads `im_last_error` in the same C call, so `bridge.go`, `fakeCaller` and every M2 Go test
  stay; no Go callback exists. ABI v1.1. Capture through `SCContentSharingPicker` only (window first; identity from
  15.2); `'420v'` BT.709, `queueDepth` 8 (SCK's maximum; 02 §7.4), SCK scales to layer `f`; one encode worker per layer;
  real-time threads. VideoToolbox in LLRC mode at every size, else classic up to 1440p60; live bitrate;
  make-before-break re-opens. Own windows by a strategy table filled by measurement (EM9). CI on `macos-26` with
  test-hooks fakes and software VT; real capture on the owner's Mac through 05's Dev Harness.
- [**03-macos-audio.md**](03-macos-audio.md): the tap on a tap-only private aggregate (fallback: the output device as
  main sub-device with its input streams off), classification native on the engine's queue in the new portable library
  `native/common/tappolicy` (dot-boundary bundle-ID matching against the process, its enclosing bundles and its
  responsible process unless a launcher; lineage cache; mic users sticky per app root using input devices). Go keeps
  M2's rules data and service, chosen per OS through `SelfInfo.RulesOS`. M2's `MixEngine` with one channel per tap
  generation and a new opt-in aligned takeover for S2's 20 ms crossfade (Windows never sets it); a speexdsp fixed-ratio
  stage on every version (the plan's "own resampler", generalized). Permission: a never-prompting TCC preflight plus the
  30 s watchdog; a share never fails, audio fails closed to silence; a missing responsible-PID SPI sets `selfAudible`,
  which mutes the in-app viewer. The audiotest gate on macOS: M2's cases mapped plus six macOS-only ones (28–33), M2's
  pass rule (case 15 allows 150 ms, plus one guest IO buffer in a VM), test-only fixture bundle IDs
  `com.isshoni.test.*`.
- [**04-macos-release.md**](04-macos-release.md): the universal `.app` built on `macos-26` with an exact Xcode
  (`deploy/macos/tools.json`); the self-signed identity (RSA-3072, 30 years, no timestamp) with the requirement
  `identifier "io.isshoni.desktop" and certificate leaf = H"<sha1>"` and an offline backup the updater also accepts;
  hardened runtime with only `com.apple.security.device.audio-input`, no sandbox, no keychain access groups. The
  deterministic tarball feeds the updater, `install-desktop.sh` (POSIX sh; `/usr/bin/ssh-keygen` checks
  `checksums.txt.sig`, then `codesign -R`; `/Applications` if writable, else `~/Applications`; never `sudo`; launches
  through `open`) and the cask; the DMG is for browsers only. The updater stages and code-checks the new bundle; a
  one-shot helper (the old app's own executable, `--apply-update`) swaps with `renamex_np(RENAME_SWAP)` and relaunches
  through `open -a`. CI never holds secrets in jobs that run build code: `macos-app` → `macos-sign` → `macos-pkg` →
  `desktop-manifest`; `verify-macos` gates publish; the `homebrew` job pushes the cask for final releases. Grant
  persistence on each OS, App Management and the Local Network re-prompt are manual release gates with contingencies.
- [**05-exit-and-verification.md**](05-exit-and-verification.md): the gate (V-D1–V-D13): audio (`run -gate`, the
  real-app rows, `avsync`) on all four versions, latency L-1 on bare-metal 26 and 27, L-F in the 14.4 and 15 VMs, and
  the release-candidate rows. Grants are declared (`ISSHONI_TEST_TCC`), never probed: undeclared → skip with a `macenv:`
  reason, declared but missing → fail. Every macOS skip is in `.github/macos-skips.txt`, checked by `tools/skipcheck`.
  Harness apps (dev identity for development, the test-tools identity `isshoni-testtools-1` for the exit) hold the
  grants. GitHub's runners: `macos-26` builds, `macos-15`/`macos-26` run the WK spec, `macos-14` and `macos-15-intel`
  run launch and compatibility checks. The real-app e2e on WKWebView uses an in-app loopback driver in the `e2e` build.
  Budgets and soak, the M-MAC manual matrix, the exit tools artifact, the run book and the VM lab.

## 2. Architecture

```
 Mac: isshoni.app (universal Mach-O, Go + cgo; one per user, single instance; started through LaunchServices,
      so isshoni is its own responsible process for TCC and for the tap's self rule)
 ├─ cmd/isshoni-desktop: mode dispatch (01 §6) · Wails v3 shell: window, menu bar extra, main menu, notifications
 │    └─ WKWebView at wails://localhost: bundled SPA (web.Dist) + DesktopPlatform (branches on desktop.app.os)
 │          │ bindings + events (NativeBridge)          ├─ REST /api/v1 with Bearer (CORS) ──────────────────► server
 │          ▼                                           └─ C1: WSS /ws, role viewer, hello.auth ─────────────► server
 ├─ internal/desktop (M2 03; 01): app · bridge (+ Permissions) · share · session · settings · tray · hotkey · e2ehost
 │    ├─ internal/desktop/macos (01; cgo + ObjC, prefix ISM/ism_): OnMain, paths, single instance, relaunch,
 │    │     Apple events, Carbon hotkeys, lock/sleep events, keep-awake, sharingType, SMAppService, Settings links
 │    ├─ audio.Service (M2 05; 03): rules (RulesOS "darwin"), banners, What friends hear, selfAudible
 │    ├─ deviceauth.Servers (M2 02) ── /device/code, /device/token ──────────────────────────────────────► server
 │    │     └─ credstore: login Keychain, no UI (01 §13; KC-proven), else a 0600 file
 │    └─ update.Updater (M2 06; 04): darwin-app tar.gz, codesign -R, helper swap (RENAME_SWAP) ───► GitHub Releases
 ├─ internal/client/core (M2 01): C2, role publisher ── WSS /ws + pub PC (RTP, SRs on the mach clock) ──► server
 │    media pump (the only caller of Session.Next) · publish (pacer) · ratectl ◄── quality.hint (nativeUplink)
 │          ▲ media.Engine (internal/media contract; fake.Engine in dev/e2e builds without imstatic)
 │    internal/media/native (02): cgo caller im_go_call (-tags imstatic), no //export, no LockOSThread
 └─ libisshoni_media.a (02, audio 03): C ABI v1.1, pull-only (im_next_packet), never calls into Go; per arch,
      merged with libopus and speexdsp; the two Go builds are joined by lipo (04)
      video: SCContentSharingPicker → SCStream '420v' (SCK scales to f) → VideoToolbox H.264 LLRC (f)
             + VTPixelTransferSession (q) → one encode worker per layer → Annex B
      audio: process tap (global, excluding voice apps and isshoni) on a private aggregate (output clock)
             → speexdsp to 48 kHz → audiomix 10 ms grid (aligned crossfades) → Opus 20 ms
      own windows: excluded in the SCContentFilter (15.2+) | sharingType (14.x) | SELF_VISIBLE → minimize + mute

 WebKit's GPU/WebContent/Networking processes: separate, responsible PID = isshoni → kept out of the tap (03 §9.7)
 isshoni server (M1, M2; no M3 change unless WM1): GET /install-desktop.sh pre-filled (04) · /download macOS block
 Browser tabs of the same user ◄── relay playback.stop / playback.state ──► the app (M2 handoff, unchanged)
```

Everything above the engine line is M2's code with darwin files added; the engine is new. Pick, Start and every
engine call run on goroutines, never on the main thread (02 §7.2: a call on the main thread fails at once with
`IM_ERR_BAD_STATE` `main_thread`); AppKit's run loop is Wails' and is never blocked for more than 2 s.

| Path | Spec | What |
|---|---|---|
| `cmd/isshoni-desktop` (`*_darwin.go`, `build/darwin/tray/`) | 01 | start order, shell options, menus, notifications, one-shot modes, `e2edriver_darwin` (e2e builds only) |
| `cmd/isshoni-desktop/engine_{fake,release}.go`, `engine_noengine_darwin.go` | 02 | engine selection by `imstatic` (§3 item 18) |
| `cmd/isshoni-desktop/build/darwin/{Info.plist.tmpl,*.entitlements,AppIcon.icns}` | 04 | bundle template (01's run-time values, 01 §4.4), entitlements, icon |
| `internal/desktop/macos` | 01 | the darwin OS layer (new; `*_other.go` stubs build everywhere) |
| `internal/desktop/{app,bridge,hotkey,settings,share,tray,i18n,applog,e2ehost}` darwin and shared changes | 01 | additive or keyed on the OS |
| `internal/desktop/audio`, `internal/media/audiorules` (macOS lists) | 03 | per-OS rules, banners, `selfAudible` |
| `internal/client/credstore` (`keychain_darwin.*`), `tools/kccheck` | 01 | the Keychain store and the KC check |
| `internal/client/core` (`SelfVisible`, `cancelled` end, `ShareRequest.Override`) | 01 | small shared changes for the share flow |
| `internal/media` (`media.go`: `SelfVisible`), `internal/media/fake` | 02 | contract additions |
| `internal/media` (`audio.go`, `probe.go`), `internal/media/native/audio.go` | 03 | additive audio types and test flags |
| `internal/media/native` (darwin files, `bridge.go`, `abi.go`) | 02 | the cgo binding |
| `internal/protocol` (`desktop.go`) | 01 | `DesktopAssetOriginMacOS` |
| `internal/update` | 04 | darwin manifest entry, untar, apply, helper, codesign check, policy |
| `native/include/isshoni_media.h` | 02 (audio block: 03) | C ABI v1.1 |
| `native/{CMakeLists.txt,CMakePresets.json,triplets/*-osx-isshoni.cmake}` | 02 | presets and triplets |
| `native/common/` additions and moves (`os/clock`, `h264/avcc`, `testpattern`, `testenc`) | 02 | portable C++ |
| `native/common/tappolicy`, `native/common/audiomix` (aligned takeover, macOS report) | 03 | portable audio policy and mixer changes |
| `native/macos/` (`src/audio/`: 03; `tests/support/`: 05) | 02 | the engine |
| `cmd/isshoni-enginetest` darwin files, `native/macos/tools/hwcheck.sh` | 02 | engine tools |
| `cmd/isshoni-audiotest` darwin driver, cases, fixtures, `macapp/` | 03 | the audio gate |
| `tools/g2g` (`capture_darwin.*`), `tools/macharness`, `tools/skipcheck`, `internal/testenv/macenv`, `.github/macos-skips.txt`, `web/e2e/wk`, `.github/workflows/machw.yml`, `docs/m3/exit-test/` | 05 | latency, harness apps, skips, WK spec, exit record |
| `deploy/macos/` (`tools.json`, `sign.sh`, `ci-identity.sh`, `dev-signing.sh`, `make-dmg.sh`), `deploy/install-desktop.sh`, `deploy/homebrew/`, `deploy/keys/macos_codesign`, `deploy/keys/isshoni-codesign-*.pem`, `deploy/notices/native-darwin.json`, `deploy/test/macos/`, `.github/workflows/*` | 04 | signing, packaging, installer, tap, CI and release |
| `tools/desktoppkg` (`macver`, `bundle`, `machocheck`, `macpack`, `manifest -macos-archive`), `tools/repocheck` (pins, keys) | 04 | packaging and repo checks |
| `internal/server/httpapi` (`RenderInstallSH`), `deploy/embed.go` | 04 | `GET /install-desktop.sh` |
| `web/src/platform/desktop` (`os.ts`), `web/src/desktop`, `web/src/share/native`, `web/src/viewer/endpointMute.ts` | 01 (audio rows and banners: 03) | the SPA's macOS branches |
| `web/src/download` | 04 | the `/download` page's macOS block |
| `docs/` site pages | 04 (03's troubleshooting text, 01's anchors) | download, privacy, code signing, security, troubleshooting |

**Import and cgo rules** (`.golangci.yml` and one grep encode them; 01 §4.5, 02 §6.4):
- M2's rules stay: Wails only in `cmd/isshoni-desktop` (and `tools/wails3`); `internal/client/` imports only its
  sibling client packages, `internal/protocol`, the `internal/media` root, `internal/version`, `internal/logx`; the
  server never imports `internal/desktop`; engines enter only through `cmd/isshoni-desktop` and `e2ehost`.
- New depguard row `desktop-macos`: `internal/desktop/macos` imports only `internal/desktop/app`,
  `internal/desktop/hotkey`, the standard library and `golang.org/x/sys/unix`; only `cmd/isshoni-desktop` imports it
  (`newOSHooks()` in `platform_{windows,darwin}.go`, so `main.go` names neither OS layer).
- cgo appears only in `internal/media/native` (darwin), `internal/desktop/macos`, `internal/client/credstore`
  (darwin), `tools/g2g` (darwin), `cmd/isshoni-enginetest` and `cmd/isshoni-audiotest` (darwin), `cmd/isshoni-desktop`
  (`e2edriver_darwin`, e2e builds only) and `tools/kccheck`. `internal/client/...` stays cgo-free and gomobile-
  buildable; Windows builds stay `CGO_ENABLED=0`.
- No `//export` anywhere in `internal/media/native` (a grep in `macos-native`): native code never calls Go.
- cgo flags come only from `#cgo darwin CFLAGS/LDFLAGS` directives in the source files (02's `link_darwin_*.go`
  carries the archive path and frameworks), never from `CGO_CFLAGS`/`CGO_LDFLAGS` set per package; the deployment
  target alone comes from the environment (`MACOSX_DEPLOYMENT_TARGET=14.4`, 04 §3).

## 3. Integration decisions

The five specs were written in parallel and then reconciled. The outcomes:

**Changes against the plan**
1. **The macOS build and native jobs run on `macos-26`**, not the plan's "macos-15 (cgo)" ("Build, CI and
   release"): the engine needs the macOS 26 SDK for `CATapDescription.bundleIDs` (02, 03); the deployment target
   stays 14.4 and `macos-14`/`macos-15`/`macos-15-intel` run what was built (04 D14, 05 V-D10). `native-host` keeps
   `macos-15`. MA01 writes the plan's line once the owner confirms (§7 question 10); the fallback runner is `macos-15`
   with Xcode 26 selected.
2. **The "own resampler on 14.x" runs on every version**: a speexdsp fixed-ratio stage whenever the tap's rate isn't
   48 kHz (03 §7.4), one code path instead of two.
3. **The harness**: the plan's "harness .app signed with the same identity and granted once" becomes two harness apps
   (05 V-D5): `isshoni Dev Harness.app` (`io.isshoni.dev.harness`, dev identity) for development and `isshoni Exit
   Harness.app` (`io.isshoni.exitharness`) in the rc's exit tools, signed with a separate stable test-tools identity
   `isshoni-testtools-1` in its own release job `macos-sign-tools` (04 §5.1, §10.1), so the release identity signs
   only `io.isshoni.desktop`. Both carry the audiotest,
   enginetest and g2g tools; fixtures sit in a sibling `fixtures/` folder (03 §16.1).
4. **14.4 and 15 are checked in VMs** on the development Mac (Apple silicon can't boot a macOS older than it
   shipped with); **26 runs on bare metal** on a required second APFS volume of that Mac (26 VM clones only for the
   first-run C rows). L-1/L-2 gate on bare-metal 26 and 27; the 14.4 and 15 VMs run the functional check L-F (05
   V-D2, V-D3). The owner signs this off at MA01, before the EM/AM slices (§7 question 1).
5. **Intel is best effort**: the x86_64 slice is built and cross-linked on `macos-26`, its tests run under Rosetta in
   the nightly, it launches natively on `macos-15-intel` while GitHub offers it; no Intel hardware is verified (02,
   04 D1, 05 V-D11; §7 question 3).
6. New paths not in the plan's layout: `internal/desktop/macos`, `native/macos/`, `native/common/{tappolicy,
   testpattern,testenc}`, `internal/testenv/macenv`, `tools/{macharness,skipcheck,kccheck}`, `deploy/macos/`,
   `deploy/homebrew/`, `web/e2e/wk/`, `docs/m3/exit-test/`.

**Who owns what**
- 02 owns the header (03 owns the audio block's semantics and text, merged by EM1), the layout and CMake of
  `native/macos/`, the export layer, the session, capture and encode, `internal/media/native`'s darwin files and
  `bridge.go`, and `cmd/isshoni-enginetest`'s commands. 03 owns `native/macos/src/audio/`, `native/common/tappolicy`,
  the `audiomix` changes, `internal/media/native/audio.go`, `internal/desktop/audio`, `audiorules` and the darwin
  audiotest. The boundary is M2's `AudioEngine`/`Mixer`/`PcmSink` (02 §10, 03 §5.1).
- 01 owns `internal/desktop/macos`, the darwin files of `cmd/isshoni-desktop` (except the engine constraints, item 18,
  and the bundle template, entitlements and icon, which are 04's), the Keychain store, every app string (namespace
  `desktop`, with 03 §14.5's English) and every SPA branch except the audio rows and banners (03) and `/download` (04).
- 04 owns every workflow file, the Taskfile's structure and `deploy/macos/tools.json` (as M2's 06 did), signing,
  packaging, the installer, the server route, the updater's macOS half, the tap and the site pages.
- 05 owns `macenv`, the ledger, `skipcheck`, the harness apps, `g2g` on darwin, the WK spec, `machw.yml`, the exit
  tools' content and checks, the VM lab and the exit record. 05's CI steps live in 02's and 04's jobs.
- The server changes only through 04 (`GET /install-desktop.sh`); 01's WM1 decides whether a bearer-only
  `Origin: null` is needed (none expected).

**Conflicts between the specs, decided here** (MA01 writes each into the named sections)
1. **TCC gating is 05's alone**: `internal/testenv/macenv` (`Require`, `ISSHONI_TEST_TCC`) and native
   `IM_REQUIRE` (`native/macos/tests/support/macenv.h`). There is no `ISSHONI_TCC_AUDIO` and no ctest label `tcc`
   (03 §16.8 already says so); 01 §19.4 and §21 and 04 §9.5 and §16 drop them.
2. **No `IsshoniEngineTest.app` and no `task native:enginetest-app`**: enginetest runs as a tool inside 05's harness
   through `harness.sh` (02 §13.1, §19). 04 §4.2, §5.4, §5.7, §9.5, §15 and §16 drop the bundle; `sign.sh` keeps its
   `-tool` form (01's KC bundle) and its `-harness` form.
3. **The exit tools are assembled by 04's `macos-exit-tools` job** (secret-free, `needs: macos-sign`) with 05 §4.5's
   content and checks; 05 §4.5 and §9.1 say `macos-exit-tools` where they say `macos-pkg`.
4. **The WK spec runs in 05's `macos-wk` job** (`macos-15` and `macos-26`) from the artifact
   `isshoni-desktop-macos-e2e`, which 04's `desktop-macos` uploads (7 days; 05 §5.3 drops its own 1-day upload step);
   01 D21 and §19.2 say "05's `macos-wk`" where they say `desktop-macos` runs the WK spec.
5. **`isshoni_hw` tests of `cmd/isshoni-desktop` compile on CI**: `TestWailsChecklist` needs the tag and runs its
   Go-side WM rows in `desktop-macos` (01 §19.2), so the grant tests in that package compile there too and skip
   through `macenv` with ledger lines. 05 §5.1's "never compiled on CI" gains this exception; `skipcheck`'s scope adds
   `internal/desktop/...`, `internal/client/credstore` and `tools/kccheck`, and `desktop-macos` runs `skipcheck`.
6. **The audio gate's real-app rows are 05 §2.1's** (M-R1 on all four versions; M-R4 and M-R8 on 27 and 14.4; M-R2,
   M-R5, M-R10 on 27); 03 §16.10 item 2 cites 05 §2.1 instead of its own subset.
7. **Site anchors are 04 §11's list**, which adds 01's `/troubleshooting#macos-encoder` and `#macos-local-network`
   and 03's troubleshooting sections (Bluetooth hands-free, voice-processing ducking, the permission reset). 03 §14.4
   and §21 link `/troubleshooting#macos-permissions-reset` instead of `#desktop-macos-permissions`.
8. **`native/macos/src/audio/audio_engine.h` and the stub `AudioEngine` are written by 02's skeleton slice** (MA05),
   verbatim from `native/windows/src/audio/mixer.h` plus `RenderTone`/`RenderBursts`, so the export layer links from
   day one. MA05 also writes the **final forwarding** of every `im_audio_*`, `im_list_audio_apps`,
   `im_set_exclusions` and test-render export through `audio_bridge::Engine()`/`SetEngineForTest`
   (`native/macos/src/audio_bridge/`); MA35 adds only the session wiring. From then on the folder is 03's and MA20
   replaces the stub (M2's WA11 pattern).
9. **enginetest commands are 02's** (`probe`, `capture`, `bench`, `sessions` in EM6; `sources` EM7; `pick` EM8;
   `protect-check` EM9; `soak`, `avsync` EM11); 05's V6 adds only the exit wiring: the avsync helper bundles in the
   harness's `Resources/avsync/`, judging `protect-check`'s outcome against 02 §8.8's table, `TestCommands*` and the
   acceptance runs.
10. **`e2edriver_darwin.{go,m}` is built by 01's MD6** (01 §4.5); 05's V9 builds the client (`web/e2e/wk/`), the
    fixture, the helper move to `web/e2e/desktop/common.ts`, `task e2e:wk` and the job's steps.
11. **`desktop-macos` runs on `tools.json`'s runner (`macos-26`)**: 01's slice summary and 03 §17 already say so;
    nothing runs the desktop app's darwin code on `macos-15` except 04's `macos-runtime` and 05's `macos-wk`.
12. **01's bundle inputs are folded into 04's bundle slice** (MA14): 04 owns `Info.plist.tmpl` and its golden; 01's
    §4.4 run-time values (`LSUIElement` true, `NSQuitAlwaysKeepsWindows` false, `CFBundleURLTypes` only in production,
    `NSAllowsLocalNetworking` only in dev/e2e, `NSAudioCaptureUsageDescription` with 01's text, no
    `NSMicrophoneUsageDescription`) and the menu bar PNGs land in the same PR.
13. **Every M3 CI job is written up front** (MA02, like M2's WA04), each step guarded by the existence of its input;
    later slices change only the steps their row names (§5's shared-files table).
14. **AM5's "`isshoni-audiotest apps` lists real apps"** moves to AM10 (MA55), where the darwin driver exists; AM5
    is accepted by `proc_snapshot_test`, the `report_mac` goldens and the export path reaching `TapEngine`.
15. **The e2e flavor's credentials**: if a runner image prompts for the Keychain, the e2e flavor on darwin uses 01's
    0600 file store in its own data root (05 R13); 01 §13.1 notes it. No new environment variable.
16. **Grant tests of the app** (`TestAudioPermissionFlow`, `TestSoundCheckMac`, `TestNotificationsRoundTrip`,
    `TestLoginItemRoundTrip`) run as `go test -c` binaries inside the Dev Harness through `task mac:hw:desktop`
    (01 §19.4); MA02 declares that task, MA40 fills it.
17. **Harness bundle ids and fixture ids** are 05's (`io.isshoni.dev.harness`, `io.isshoni.exitharness`,
    `io.isshoni.test.avsync.{flash,click}`) and 03's (`com.isshoni.test.*`); 04's `sign.sh -harness` signs them from
    `tools/macharness/sign-list.txt` (`-mode dev` for the Dev Harness, `-mode testtools` for the Exit Harness).
18. **Engine selection files**: 02's EM4 (MA12) writes `engine_fake.go` (`(windows || (darwin && cgo)) && (dev ||
    e2e) && !imstatic`), `engine_release.go` (`… && ((!dev && !e2e) || imstatic)`) and `engine_noengine_darwin.go`
    (fails a `darwin && cgo && production && !imstatic` build); 01 §4.5 and §6.4 point to them.
19. **The `macos-native` job's path filter is the existing `desktop` filter**, extended by 02 and 04 (`tools/g2g/**`,
    `tools/macharness/**`, `tools/skipcheck/**`, `.github/macos-skips.txt`, `cmd/isshoni-desktop/build/darwin/**`,
    `deploy/macos/**`, the codesign keys and notices); 05 §5.3's `ci-ok` paths are the same list.
20. **`--self-check` on darwin** (01 §6.4, 04 §2.4): `engine` `{loaded, abi, testHooks, name, os, archiveSHA,
    encoders}`, `bundle` `{id, path}` and `menu`; no `ffmpeg*` keys; `ok` doesn't depend on `encoders` or on TCC, so it
    passes on hosted runners and in VMs.
21. **The release version is `0.3.0`** (`v0.3.0-rc.N` for the exit); if `v0.2.0` hasn't shipped, one tag carries both
    apps (04 §10.7).

## 4. Conventions

M1's and M2's conventions (`docs/m1/README.md` §4, `docs/m2/README.md` §4: errors, context and shutdown, logging,
config, build tags, testing, C++, naming, interfaces first, branches and PRs) apply unchanged. This section lists only
what M3 adds or changes.

### Errors
- The engine's failures stay `*media.Error{Code, Reason, Op}`; ABI v1.1 adds detail tokens (`os_stopped`, `platform`,
  `no_run_loop`, `no_hardware`, `capture_restart_failed`, `main_thread`, 03's permission tokens) that are never
  renamed or reused. The SPA maps them to `desktop.errors.<code>` with the `_macos` context (01 §10.6).
- Objective-C: every export and thread or block entry catches both C++ exceptions and `NSException` (`@try` around
  the C++ `try`) and turns them into `IM_ERR_INTERNAL`; `NSError` never crosses the ABI; its domain and code go to the
  log ring only (02 §7.3).
- Audio never fails a share: a missing grant or a broken tap is a report state and a banner (03 §13.4).

### Threads, run loops and shutdown
- AppKit, `SCContentSharingPicker` and `NSWindow` work run on the main queue only; Wails owns the main run loop. The
  app reaches it through `macos.OnMain`/`OnMainWait` (bounded waits); the engine through its own main-queue helper
  (02 §7.2). Go never calls the engine from the main thread.
- The engine uses serial dispatch queues for API work and SCK callbacks and real threads with
  `THREAD_TIME_CONSTRAINT_POLICY` for the video thread, test tone and test pattern (S2-vt finding 9). No allocation on
  real-time threads (`rt_alloc_test`).
- M2's bounds hold: `Next` honors its context within 50 ms, `Session.Stop` ≤ 200 ms, `core.Close` ≤ 3 s, quit while
  live asks first and exits within 3 s of the answer (05's WK spec asserts it). `SIGTERM` = `App.Quit(true)`.

### Logging
- M2's never-logged list gains: bundle IDs of other apps (they name apps), window titles from SCK, the responsible-PID
  lineage, Keychain item attributes, and home paths (`~` redaction in `applog`, 01 §15). The engine's log ring never
  names an app, process or bundle ID at any level.
- Committed reports (`hwcheck.sh`'s bundle, audiotest results, `g2g` JSON, `vm-versions.json`, the exit record)
  carry no computer, VM, user, host or server names, paths or IPs (05 §9.5's `grep` step before every commit).

### Config, build tags and environment
- No server TOML key. New `ISSHONI_*` names go onto `reservedEnv` and `keys_test.go`'s `devEnvNames` in the same PR
  through the integrator (M2 §4 "Config"): `ISSHONI_TEST_TCC`, `ISSHONI_TEST_HUMAN`, `ISSHONI_HARNESS_ARCH`,
  `ISSHONI_E2E_DRIVER`, `ISSHONI_CHECKLIST_BUNDLE`, `ISSHONI_INSTALL_DIR`, `ISSHONI_UPDATE_TEST_CODESIGN_REQUIREMENT`,
  `ISSHONI_DEV_SIGNING_DIR`, `ISSHONI_PREFILL_SERVER`, `ISSHONI_PREFILL_VERSION`. Names private to one task's own
  processes take another prefix.
- The app reads no environment variable for paths in release builds; the data root comes from `NSFileManager`
  (`TestDataRootNotFromEnv`). Settings stay `settings.json` schema 1; the only macOS-only preference is the MDM key
  `DisableUpdateChecks` and the one-time `UpdateChecks` seed (04 §7.4).
- Build tags: M2's `production`, `dev`, `e2e`, `updatetest`, `isshoni_hw`, plus **`imstatic`** (the real engine on
  `darwin && cgo`, any flavor) and **`imtesthooks`** (the test-hooks archive). A release is exactly
  `production,imstatic`; 04's `machocheck -thin` fails anything else and any test-hooks symbol. CMake's
  `IM_TEST_HOOKS` is on only in `mac-arm64-ci`, `mac-x64-ci`, `mac-arm64-asan`, `mac-arm64-tsan`.

### Objective-C, cgo and the toolchain
- Objective-C++ (`.mm`) for the engine, Objective-C (`.m`) in Go packages, ARC everywhere (`-fobjc-arc`), `-Wall
  -Werror`, `-Werror=unguarded-availability-new`, deployment target 14.4; every newer API behind `@available` with the
  version it needs. `native/.clang-format` covers `.m` and `.mm` (`lint:native`'s globs).
- One Mach-O holds the app, the OS layer and the engine, so names must not collide: Objective-C classes and non-static
  C functions are `ISM`/`ism_` in `internal/desktop/macos` and `credstore`, `IMMac`/`im::mac` in the engine;
  file-local helpers are `static`. `desktop-macos` fails on a duplicate `_OBJC_CLASS_$_` (01 §4.5).
- Core Foundation objects in the engine are held by `cf_ref.h`'s RAII types (02 §4).
- No Xcode project: CMake for the engine, `go build` for Go, Command Line Tools with SDK ≥ 26 on the Mac, the exact
  Xcode of `tools.json` on CI.
- Static only: no dylib of ours in the bundle (`machocheck`); any native dependency goes onto
  `deploy/notices/native-darwin.json` in the same PR. No GPL code or dependency anywhere.

### Testing
- M1's and M2's rules apply (`-race`, goleak, goldens, fuzzers 30–60 s per PR, `synctest`, the pinned linters run by
  the slice itself, `golangci-lint` with `GOOS=windows` too).
- **Grants** (05 §5): a test that needs a grant or hardware calls `macenv.Require` / `IM_REQUIRE` before touching any
  capture, tap or `SCShareableContent` API (listing shareable content alone prompts). Undeclared → skip with a
  `macenv:` reason; declared but missing → fail. Every macOS skip has a line in `.github/macos-skips.txt`; an unlisted
  skip fails `skipcheck`. CI never sets `ISSHONI_TEST_TCC` or `ISSHONI_TEST_HUMAN`.
- **The owner's Mac**: grant tests run only inside the Dev Harness (`task mac:hw[:engine|:audio|:core|:g2g|:human|
  :desktop]`), never from a terminal, so the grants go to a stable designated requirement. Grants needed once: the Dev
  Harness gets Screen Recording and System Audio Recording, each mic fixture the Microphone (05 §5.4).
- **Which grant each test needs** is 05 §5.2's table; slices add rows there through the next write-back.
- **Windows unchanged**: every group's merge runs `windows-go`, `windows-native` (1.1 DLL), `windows-desktop` (the CDP
  spec with its annotation rows unchanged) and `e2e`; M2's goldens and the update manifest golden stay byte-identical
  (01 §18, 02 §11.6, 03 §17, 04 §9.1, 05 §5.7).
- **M2's lessons that apply here** (`docs/m2/README.md` §4 "Testing"):
  - every test that loops has a timeout, and a sanitizer run's shortened duration still reaches every timed step;
  - a crash on a thread other than gtest's main one ends a test binary silently, so `native/macos/tests/support/`
    carries a crash reporter as `native/windows/tests/support/crash_report.cpp` does (MA06);
  - ctest counts an all-skipped binary as passed: the skips are listed (gtest XML, `go test -json`) and checked;
  - CI's clang-format and compilers differ from the Mac's (gcc `-Wshadow`, clang-format 18): run the pinned versions;
  - gosec's G115 fails integer conversions the compiler accepts, which cgo code has many of: check the whole diff;
  - timing tests flake under load (hosted macOS runners have 3 vCPU): assert what the acceptance requires and no more,
    time a spacing from the request the code can't act on earlier, and accept events in either order;
  - a frame captured before a request may still be queued, so count only frames captured after it;
  - fixtures never carry a fixed date that will pass;
  - host state must not leak into a fixture (the default output device, its sample rate, Focus modes; case 33
    restores the rate);
  - Wails beta.34 can't cancel a navigation or a new window (W5), so navigation layer 3 checks after the fact
    (01 D18), and `Reload()` reloads the current page;
  - the WebView runtime may start its helper processes with an environment of its own: the WebKit env scrub and WM19
    are checked in WebKit's processes, not only in isshoni's;
  - one CI round costs about an hour: a macOS e2e that can fail in several links checks each link on its own and
    prints what both sides logged;
  - a test that compares a built binary's version builds it with `-buildvcs=false`; parallel `testscript` children
    each need their own `GOCOVERDIR`;
  - Credential Manager lost concurrent writes; the Keychain store makes one call at a time as well (01 §13).
- Real-device runs ("Manual:" items) are written as exact steps in the specs (01 §19.5, 02 §17.5, 03 §16.9, 04 §14,
  05 §7), recorded in the slice's PR, and don't block the merge; the exit items (MA72) do.

### Naming
- Slice ids `MA01`…; write-back slices `MB<n>` (after group n); fix slices `MF01`… The specs' own ids stay as source
  references (`01 MD4`, `02 EM6`, `03 AM7`, `04 MR9`, `05 V3`); checklist rows are 01's `WM1`–`WM26`, manual rows 05's
  `M-MAC-1`–`M-MAC-23` and 03's `M-R1`–`M-R10`, exit rules `A-*`, `L-*`, `C-*`.
- Bundle ids `io.isshoni.desktop` (release; also the Keychain service), `.dev`, `.e2e`; executable
  `Contents/MacOS/isshoni`; data root `~/Library/Application Support/isshoni/`; Keychain account
  `isshoni:v1:<leaf sha256[:8] hex>:<origin>` (01 §13.1); identities `isshoni-codesign-1`,
  `isshoni-codesign-backup-1`, the test-tools `isshoni-testtools-1`; asset
  `isshoni-desktop_<v>_darwin_universal.{tar.gz,dmg}`.

### Interfaces first
The first slice of a package or binary declares its whole interface with fakes, and unfinished parts return a clear
"not implemented": MA02 (every M3 job and task), MA03 (`internal/desktop/macos`, the new bound services and events),
MA04 (all of ABI v1.1, with 03's audio constants), MA05 (every export on macOS, the audio boundary), MA06 (`macenv`
and the ledger format), MA12 (the darwin binding), MA30 (the harness launcher grammar).

### Branches and PRs
- M3 work lands on **`m3/macos-app`** first (cut from `main` after M2's code merged). Each slice has its own branch
  `m3/maNN-<short-name>`, created from the tip of `m3/macos-app` when its group starts, in its own git worktree.
- Each slice opens a PR into `m3/macos-app`. The integrator merges a group's PRs in id order, runs `task lint test
  build e2e native:test native:mac` on the Mac, checks the macOS and Windows jobs on the result, and adds the group's
  `MB<n>` before the next group starts.
- `m3/macos-app` goes into `main` by PR at the end of each group (and at the end of M3), with `ci-ok` green, as M2 did.
- Every commit is signed (SSH) under the owner's identity; PRs are merged in the GitHub web UI; nobody force-pushes
  `main` or `m3/macos-app`. Commit subjects start with the slice id: `MA19: Engine session core on fakes`.
- No workflow that can reach the owner's Mac is ever triggered by a pull request (`machw.yml` is dispatch-only with
  environment approval, 05 §5.5).

## 5. Implementation plan

### How the plan works
- **73 slices in 12 groups**: 11 build groups and the exit group. A group starts when the previous one is merged into
  `m3/macos-app` and green. Write-back slices `MB<n>` and fix slices `MF<nn>` are added by the integrator.
- Slices in one group touch **disjoint paths** ("Touches"), so separate agents can build them at once in separate
  worktrees. A Go package means its own directory only, except for the file-level packages in the next table; other
  paths are prefixes. A slice changes only its listed paths and the shared files below; if it needs more, it stops and
  asks the integrator.
- "Needs" lists slices that must be merged first, always in earlier groups. A check that needs another slice of the
  same group is run by the integrator right after the group merges.
- **Days** are rough, for one developer: about 148 in all. Built group by group with parallel agents, the longest
  slice of each group adds up to about 30 working days; with about a day of integration per group, about 9 weeks
  (the plan says ≈ 8), plus the owner's VM and exit runs.
- **Critical paths**: the app MA03 → MA13 → MA22 → MA31 → MA45 → MA51 → MA60 → MA71 → MA72 → MA73; the engine MA04 →
  MA12 → MA19 → MA25 → MA34 → MA46 → MA52 → MA59 → MA64 → MA67 → MA69 → MA72; audio MA08 → MA17 → MA47 and MA07/MA08 →
  MA20 → MA26 → MA36 → MA47 → MA55 → MA61 → MA65 → MA67; the release MA02 → MA14 → MA21 → MA29 → MA39 → MA49 → MA56 →
  MA62/MA63 → MA73.
- **Milestones**: real `--self-check` of a universal bundle with the linked engine (`engine.loaded`) after group 4
  (MA27); hardware VideoToolbox from `isshoni-enginetest` after group 4 (MA25); the first tap capture of a fixture tone
  after group 4 (MA26, a native `MacTapHW` test without a session), through an audio-only session after group 5
  (MA35); the updater e2e and `install-desktop.sh` on hosted runners after group 5; a real pick going
  live and watched in a browser after group 6 (MA46); the complete audiotest after group 7 (MA55); a release dry run
  after group 7 (MA56); the app sharing a real pick with own-window handling after group 8 (MA60); `v0.3.0-rc.1` and
  the exit run in group 12.
- **Owner runs that pace the plan**: the sign-off of §7 question 1 at MA01, before the EM/AM slices; the 26 volume and
  the VM lab (MA10) before the S2 re-run (MA18), which must pass (or have its consequences written down; a lab blocked
  by boot or device failure counts) before the tap generation (MA26) merges; the signing identities before MA28's real
  files and before the first rc; the tap repository before MA62's real push.
- If M3 runs late, these can move after `v0.3.0` without touching the exit criterion: MA70 (self-hosted runner), MA68's
  footprint annotations (the soak stays), MA62's real-tap push (the cask template and audit stay). A moved slice
  leaves every Needs list.

### Shared files and their owners

| File | Owner | Rule for every other slice |
|---|---|---|
| `go.mod`, `go.sum` | M1's rule | no new module is planned (cgo uses system frameworks; `golang.org/x/sys/unix` is already required); Wails stays `v3.0.0-beta.34` |
| `tools/go.mod` | integrator | `tools/g2g` (darwin cgo), `tools/macharness`, `tools/skipcheck` and `tools/kccheck` use the standard library and `x/sys`; anything else through the integrator |
| `Taskfile.yml` | MA02 | MA02 writes every M3 task up front (§3 item 13), with the real body of `native:mac` and stubs that exit 0 with a note. Later slices change only the bodies of tasks they own: `lint:skips` (integrator at group 1's merge, from MA06's tool), `desktop:app:macos` (MA14; MA21 the signing step; MA27 the native step), `desktop:package:macos` (MA29), `release:codesign-keys` (MA28), `mac:harness` (MA30), `mac:hw*` (MA40), `e2e:wk` (MA50), M2's `release:keys`/`release:prepare` blocks (MA39) |
| `.github/workflows/ci.yml` | MA02 | MA02 writes every M3 job and step (02 §6.7, 04 §9.1, 05 §5.3), guarded by input existence. Afterwards at most one slice per group changes it, and only its named job's steps: group 3 MA21 (`desktop-macos` signing), group 4 MA27 (`desktop-macos` native gate and vcpkg cache), group 6 MA49 (`macos-runtime`), group 7 MA58 (`macos-compat`, 05's `macos-native` steps), group 8 MA62 (`desktop-macos`'s `brew` steps) |
| `.github/workflows/nightly.yml` | MA49 | MA49 writes `macos-full`, `macos-runtime-full`, `macos-native-full` and the `goreleaser-full`/`go-cross-os` changes; MA58 adds `-count=3` for `macos-native`'s Go tests; MA62 the local-tap install in `macos-full` |
| `.github/workflows/release.yml`, `.goreleaser.yaml` | MA56 | MA62 adds the `homebrew` job; MA67 fills `macos-exit-tools`' content and checks |
| `.github/workflows/machw.yml` | MA70 | – |
| `.github/macos-skips.txt` | MA06 | append-only: each slice adds the lines of the tests it adds that skip on CI; conflicts keep both sides |
| `.golangci.yml` | MA03 | MA03 writes the M3 depguard row; then integrator only |
| `deploy/macos/tools.json` | MA02 | later changes are hand-made tool PRs (Xcode bumps, runner retirements) |
| `deploy/notices/native-darwin.json` | MA27 | append-only, one entry per native dependency with the slice that adds it |
| `internal/server/config/keys.go` | integrator | the env names of §4 "Config", in the PR that first reads each |
| `native/include/isshoni_media.h` | MA04 | MA04 writes all of v1.1 with 03's audio block; later changes only through the integrator with 02's and 03's owners (none planned) |
| `native/CMakeLists.txt`, `CMakePresets.json`, `native/triplets/*-osx-isshoni.cmake` | MA05 | integrator only; `lint:pins` keeps the deployment target equal in presets, triplets, `tools.json` and the Info.plist template |
| `native/common/CMakeLists.txt` | M2's WA03 | MA07 adds `tappolicy/`, MA11 adds `testpattern/` and `testenc/`; libraries add files only |
| `native/macos/CMakeLists.txt`, `native/macos/src/api/` | MA05 | the CMake file globs each `src/` folder and `tests/`; every export forwards to an area function whose stub lives in that area's folder, and a slice replaces its own area's stub; the audio and test-render exports' forwarding through `audio_bridge::Engine()` is MA05's final code (§3 item 8), so MA35 doesn't touch `api/` |
| `native/macos/src/session/` | MA19 | MA34 changes only `supervisor.mm` (restart after sleep, lock and source changes); MA35 only `session.mm`'s audio wiring; the integrator resolves conflicts |
| `native/macos/src/audio/` | 03 (MA05 writes `audio_engine.h` and the stub) | file-level: MA20 `proc_snapshot`, `responsibility`, `report_mac`, `tap_engine`, `tcc.mm`'s read half; MA26 `tap_generation`, `tap_mixer`; MA36 `tap_controller`; MA37 `test_render`; MA47 `tcc.mm`'s state machine and `sound_check` |
| `native/macos/src/capture/` | MA19 (fake source, fake picker, test pattern) | file-level: MA34 `sck_stream`, `listed`, `sources`, `frame_info`, watchers; MA46 `picker`, `pick_tokens`; MA52 `filter.mm` |
| `native/macos/tests/` | file-level | each test file belongs to one slice, and so does a support header only it uses; `tests/support/` is MA06's |
| `internal/media/native` | file-level | MA04 `abi.go`, `errors.go` and their tests; MA12 the darwin files, `bridge.go`, `types.go`, `engine_other.go` (MA46 adds the `Pick` mapping, MA52 `SelfVisible` and `UnprotectWindow` in `bridge.go`: different groups); MA17 `audio.go`; MA19 `static_*_test.go`; MA58 its two test files |
| `internal/media` (root) | file-level | MA17 `audio.go`, `probe.go` (additive); MA52 `media.go` (`SelfVisible`) |
| `internal/desktop/macos` | MA03 (`doc.go`, `macos.go`, `*_other.go`) | file-level, by 01 §4.5's list: MA13 `main_darwin.*`, `paths_darwin.go`, `relaunch_darwin.*` (`ResponsiblePID` only); MA22 `singleinstance*.go`, `pending.go`; MA23 `window_darwin.*`; MA24 `hotkeys_darwin.*`; MA31 `relaunch_darwin.*` (the relaunch); MA32 `events_darwin.*`, `awake_darwin.go`; MA42 `webview_darwin.*`; MA43 `autostart_darwin.go`; MA44 `tcc_darwin.go`; MA45 `pages*.go`, `localnet.go`. A slice that adds a `Hooks` method body adds it in its own file; `macos.go`'s wiring conflicts are the integrator's |
| `cmd/isshoni-desktop` | file-level | MA12 `engine_*.go`; MA13 `platform_darwin.go`; MA22 `start_darwin.go` (single instance, Apple events, `--wait-pid`); MA23 `shell_darwin.go`, `menu_test.go`; MA31 `start_darwin.go` (steps 2, 2a, 7) and `oneshot_darwin.go`; MA32 `signal_darwin.go`; MA33 `creds_darwin.go`; MA42 `webview_darwin.go`, `e2edriver_darwin.{go,m}`, `checklist_darwin_test.go`; MA43 the deferred-shell change; MA44 `notify_darwin.go`; the bundle files under `build/darwin/` are MA14's. M2's Windows files stay untouched |
| `internal/desktop/app`, `internal/desktop/bridge` | MA03 | later slices change them only for their row's named parts (MA22 `PendingStore`; MA32 quit reason; MA43 login-item launch; MA45 `Relaunch`, `permissions.changed`, `permissions.go`; MA60 auto-minimize); the integrator resolves conflicts and regenerates the bindings |
| `internal/update` | MA09 (`manifest.go`) | MA15 every other file; MA28 the requirement block in `signers.go` |
| `tools/desktoppkg` | file-level | MA09 `manifest -macos-archive`; MA14 `macver`, `bundle`, `machocheck`; MA27 `machocheck -native`, symbols; MA29 `macpack` |
| `cmd/isshoni-enginetest` | MA25 | each later command is its own file (`sources` MA34, `pick` MA46, `protect` MA52, `soak`/`avsync`/`raw` MA59); MA64 changes only `protect-check`'s judging and the darwin `TestCommands*`; MA68 only `soak`'s footprint sampling |
| `cmd/isshoni-audiotest` | MA55 | MA61 the specials and `record`/`merge` fields; MA65 the A-2 comparison, names and redaction |
| `tools/macharness` | MA30 | MA40 `summary.json`; MA64 `Resources/avsync/`; MA67 the exit variant's checks |
| `web/src/platform/desktop/bindings/`, `codes.gen.ts`, `web/src/protocol/*.gen.ts` | generated | never edited by hand: after each merge run `task gen` and `wails3 generate bindings` and commit the output |
| `web/src/i18n/lazy/desktop.en.json`, `web/src/i18n/en.json` | M1's rule | keys under the slice's namespace (`desktop.*` with `_macos` contexts; `account.download.macos.*` MA53); conflicts keep both; `check:i18n` passes |
| `web/e2e/` | file-level | `desktop-macos.spec.ts` MA51; `web/e2e/wk/` MA50 then MA54 (MA68 its budget annotations); `web/e2e/desktop/common.ts` and the CDP spec's imports MA50; `download.spec.ts`'s macOS cases MA53; the darwin audio scenarios MA57 |
| `docs/m3/*.md` | MA01 | then only `MB<n>` slices and MA66 (02 §8.8, §8.2, §8.10 as measured) and MA71 (01 §3.2, §13). A slice that measures what a spec records puts it in its PR body under "Results for the spec"; the next `MB<n>` copies it |
| `docs/m3/exit-test/` | MA10 (`vm-lab.md`), MA69 (`README.md`, `results-template.md`) | results only by MA66 and MA72 |
| `docs/*.md` (site pages), `site.yml`, root `README.md` | MA63 | MA71 only `docs/security.md`'s Keychain line |
| `docs/PLAN.md` | owner | MA01 changes only the native-job runner line, after §7 question 10 |

**Dependencies and pins** (each passes the license gate)

| Dependency | Pin | First needed by |
|---|---|---|
| Wails v3, the `wails3` CLI, `@wailsio/runtime` | `v3.0.0-beta.34`, unchanged (M2 03 §3.3); the PR template's Wails checklist gains "the macOS app builds and `desktop-macos` passes" | – |
| Xcode on CI | one exact `Xcode_26.x.app` (`tools.json` `xcode`), SDK ≥ 26.0 (`sdkMin`), deployment target 14.4 | MA02 |
| Command Line Tools on the Mac | any with a macOS SDK ≥ 26 (02 §6.3; no Xcode needed) | MA05 |
| vcpkg `opus`, `speexdsp` (BSD-3-Clause, static), `gtest` (tests only) | `native/vcpkg.json`'s baseline with the overlay triplets `arm64-osx-isshoni`, `x64-osx-isshoni` | MA05 |
| GitHub macOS runners | `macos-26` (build, `macos-native`, `desktop-macos`, `macos-update`, release), `macos-15` (`native-host`, `macos-wk`, `macos-runtime`), `macos-15-intel` (`macos-runtime`, while offered), `macos-14` (`macos-runtime`, `macos-compat`, while offered) | MA02 |
| Homebrew on the runner image | the image's (only `brew style` / `brew audit --cask --strict`) | MA62 |
| shellcheck, dash | M1/M2's | MA39 |
| Tart (or UTM) with Apple's restore images | a tool on the owner's Mac for the VM lab, not a dependency of the repository | MA10 |

### Slices

"Grp" is the parallel group. "Spec" gives the source slice and sections (`02 EM6`, `01 §6.2`). "Where": Mac (the
development Mac, macOS 27), VMs (14.4, 15.0/15.1, 15, 26 on it), CI (ubuntu, macos, windows), GitHub (dry run or
nightly), owner's Mac (a manual run with grants).

| ID | Grp | Slice | Spec | Touches | Needs | Days | Where | Acceptance |
|---|---|---|---|---|---|---|---|---|
| MA01 | 1 | M3 design set and its write-backs | this README §3 | `docs/m3/` (the five specs and this README; not `exit-test/`), `docs/.vitepress/config.mts` (`srcExclude` gains `'m3/**'`, and its comment), `docs/PLAN.md` (only the native-job runner line, after §7 question 10) | – | 1 | Mac, CI ubuntu | Each of §3's conflicts 1–21 is written into the sections it names, and items 1–6 of "Changes against the plan" are stated where the specs discuss them, with no other content change; the superseded spellings (`ISSHONI_TCC_AUDIO`, ctest label `tcc`, `IsshoniEngineTest.app`, `native:enginetest-app`, `IsshoniAudiotest.app`, `macos-pkg` as the exit tools' job, `/troubleshooting#desktop-macos-permissions`, `desktop-macos` on `macos-15`) appear in no spec except where one says why it isn't used; `task site:build` passes with no page under `m3/`; `check-site` passes; the owner's sign-off of §7 question 1 (latency gating on bare-metal 26 and 27, L-F on 14.4 and 15) is recorded in the PR before any EM/AM slice starts |
| MA02 | 1 | macOS CI, pins and the M3 tasks | 04 MR1 (the toolchain half), 04 §3, §9.1–§9.3; 02 §6.5, §6.7; 05 §5.3 | `deploy/macos/tools.json`, `.github/workflows/ci.yml` (filters `desktop`, `update`, `sh`, `homebrew`; the Xcode-select step; `desktop-macos` on `tools.json`'s runner with M2's steps first; jobs `macos-native`, `macos-runtime`, `macos-update`, `macos-wk`, `macos-compat` with every step guarded by its input; `ci-ok`), `Taskfile.yml` (every M3 task; `native:mac`'s real body), `tools/repocheck` (`pins` for `tools.json`), `.gitignore` (`native/dist/`, `.dev/`) | – | 2 | CI ubuntu/macos/windows, Mac | actionlint clean; `task --list` shows every M3 task, the stubs exiting 0 with a note; a PR touching `internal/client` runs `desktop-macos` on `macos-26` green with M2's steps unchanged, and the Xcode step prints the Xcode and SDK it selected (and fails listing the Xcodes it found when `tools.json` names a missing one, shown in the PR); the new jobs skip their guarded steps until the inputs exist and count as success in `ci-ok`; `lint:pins` fails on a deployment target that differs between `tools.json` and a preset, triplet or the Info.plist template (fixtures) and passes while those files don't exist; the Windows jobs green and unchanged |
| MA03 | 1 | Desktop interfaces first | 01 MD1; 01 §5, §9.2, §20 | `internal/desktop/macos` (01 §5.2's whole API as darwin stubs answering `app.ErrNotSupported`, `*_other.go`), `internal/desktop/app` (01 §5.1: `OSPage` values, optional interfaces, per-OS page validity), `internal/desktop/bridge` (`Permissions`, `App.Relaunch`, `Window.Minimize`, `app.command`, code `local_network`, `ShareView.selfVisible`, `AppInfo.osVersion`: `not_implemented` or zero), `internal/desktop/hotkey` (per-OS API declared), `internal/protocol` (`desktop.go`), `web/src/platform/desktop/bridge.ts` (the new types), generated bindings and codes, `.golangci.yml` | – | 3 | Mac, CI ubuntu/macos/windows | Builds and tests on linux, darwin and windows; `TestDesktopOrigins`: `DesktopAssetOriginMacOS == "wails://localhost" == DesktopOrigins()[1]`; Windows goldens change only by `osVersion`; `windows-go` and `windows-desktop` green; the bindings drift check passes; a scratch import of `internal/desktop/macos` from `internal/desktop/app` fails `lint:go` (shown in the PR) |
| MA04 | 1 | C ABI v1.1 | 02 EM1; 02 §5.1; 03 §4.1, §4.2 | `native/include/isshoni_media.h`, `native/common/tests/` (the ABI tests and `testdata/abi_layout.json`), `internal/media/native` (`abi.go`, `errors.go` and their tests) | – | 1 | Mac, CI ubuntu/macos/windows | 02 EM1's: `IM_ABI_MINOR 1`, macOS `os_flags` from 0x10000, `IM_PICK_*`, `IM_SRCF_SELF_VISIBLE`, `IM_SESS_SELF_VISIBLE`, `im_caps.max_capture_fps` from `reserved[0]`, the new tokens, all of 03 §4.1's audio constants with the `im_rule` comment block; `abi_layout.json` `abi` 65537; `abi_layout_test`, `abi_header_c_test`, Go `layout_test`/`abi_test` (incl. `TestHeaderConstants`)/`errors_test` pass on every OS; `windows-native` green with the 1.1 DLL and M2's Go tests unchanged |
| MA05 | 1 | Engine build skeleton and every export | 02 EM3; 02 §4, §5.2, §6.1–§6.3; 03 §5.1 | `native/macos/` (not `tests/support/`), `native/CMakeLists.txt`, `native/CMakePresets.json` (`mac-*`), `native/triplets/{arm64,x64}-osx-isshoni.cmake` | – (MA04's header and MA02's job: integrator) | 2.5 | Mac, CI macos | 02 EM3's: the merged archive per arch with `archive.sha256`, `build.json` (version, commit, vcpkg ports) and `licenses/vcpkg/`; `exports.mm` with every export (version, init, shutdown, build info, clock, last error, logs, layout real; the final forwarding of every `im_audio_*`, `im_list_audio_apps`, `im_set_exclusions` and test-render export to the `AudioEngine` facade through `audio_bridge::Engine()`/`SetEngineForTest`, written here in `src/audio_bridge/`; the rest `IM_ERR_NOT_IMPLEMENTED`); `exports_test` shows each audio export reaching a fake set with `SetEngineForTest`; `audio_engine.h` verbatim from M2 plus `RenderTone`/`RenderBursts` (§3 item 8); worker queue; `SetRealtime`; `exports_test` and the `nm` check pass for arm64 and x86_64; after the group, `exports_test` against MA04's golden and `macos-native` steps 1–2 green |
| MA06 | 1 | macenv, skipcheck and the ledger | 05 V2; 05 §5.1 | `internal/testenv/macenv`, `native/macos/tests/support/` (`macenv.h`, the crash reporter), `tools/skipcheck`, `.github/macos-skips.txt` (empty) | – | 1.5 | Mac, CI ubuntu/macos | 05 V2's: unit tests of `Require`'s decisions and messages with fake env and probes; a fixture run with an unlisted skip fails `skipcheck`, a listed skip that ran is a notice; non-darwin skips say `macenv: needs darwin`; a declared grant that is missing fails; a gtest that crashes on a second thread prints its stack (the crash reporter) |
| MA07 | 1 | tappolicy | 03 AM1; 03 §9, §10 | `native/common/tappolicy/`, `native/common/CMakeLists.txt` (adds `tappolicy/`) | – | 3 | Mac, CI ubuntu/macos/windows | 03 AM1's: about 70 table cases and a 10,000-seed property test green under `host-debug`, `host-tsan` and `win-x64-ci`; no OS header; clang-format clean |
| MA08 | 1 | audiomix aligned takeover and the macOS report model | 03 AM2; 03 §8.4, §12 | `native/common/audiomix/` | – | 2 | Mac, CI ubuntu/macos/windows | 03 AM2's: the aligned crossfade of two simulated rings stays within ±0.3 dB with no click, and both fallbacks work; the report writer's process-tap engine, states, `selfAudible`, `tap`, counters and events with macOS goldens; every M2 `audiomix` test and golden byte-identical |
| MA09 | 1 | The update manifest's darwin entry | 04 MR6; 04 §7.1 | `internal/update` (`manifest.go`, its tests and testdata), `tools/desktoppkg` (`manifest -macos-archive`) | – | 1 | CI ubuntu/macos/windows | 04 MR6's: the two-OS manifest selects `universal` (a same-arch entry first) on darwin and `nsis` on Windows; the `darwin-app` name rule; M2's Windows-only golden byte-identical; Windows CI green |
| MA10 | 1 | VM lab (owner) | 05 V1; 05 §3.2 | `docs/m3/exit-test/vm-lab.md` | – | 2 + the runs | VMs on the Mac | 05 V1's: first, per version, whether the guest boots and has virtio output and input under Tart (otherwise UTM), so 05 R3 is decided early; the M5's 26 volume installed (latest 26.x); Tart golden images for 14.4.x, `macos15-0` (15.0/15.1), 15 and 26, `-clean` and `-granted` clones with an admin and a standard account; per guest: product version and build, whether the SCK picker works, S2's tap self-test, VT encoders (hardware or not), a mic, the Local Network prompt (15+), the guest sound-device setup 03 needs, and the commands to rebuild the images |
| MA11 | 2 | Common additions and moves | 02 EM2; 02 §7.7 | `native/common/` (`os/clock.cpp`'s Apple branch and `MachTicksToNs`, `h264/avcc`, `testpattern/` (moved), `testenc/` (`FakeH264`), `encpolicy`'s `videotoolbox` row, their `CMakeLists.txt` entries), `native/windows/` (the adapter and the moved includes) | MA04 | 2 | Mac, CI | 02 EM2's: `clock_test`, `avcc_test` and a clean 60 s `avcc_fuzz` on `native-host` ubuntu and macos; Windows `session_fake_test`, `gpu_warp_test`, the luma tests and Go `TestDLLConformance` unchanged and green |
| MA12 | 2 | Go darwin binding | 02 EM4; 02 §6.4, §6.6, §11 | `internal/media/native` (darwin files, `call_darwin.c`, `link_darwin_*.go`, `bridge.go`, `types.go`, `engine_other.go`; not `audio.go`), `cmd/isshoni-desktop` (`engine_fake.go`, `engine_release.go`, `engine_noengine_darwin.go`) | MA04, MA05 | 2.5 | Mac, CI | 02 EM4's: `TestStaticProcTable`, `TestStaticLayout`, `TestStaticClock`, `TestDecodeCapsMacOS`; `go build -tags production,imstatic ./cmd/isshoni-desktop` links for arm64 and amd64; `-tags dev,imstatic` links the engine and `-tags dev` the fake (`go list -deps`); `-tags production` on darwin without `imstatic` fails to build; M2's Go suites unchanged on ubuntu and windows; `windows-desktop` green |
| MA13 | 2 | Main thread, paths and system facts | 01 MD2; 01 §5.2, §14.1 | `internal/desktop/macos` (`main_darwin.*`, `paths_darwin.go`, `relaunch_darwin.*` with `ResponsiblePID` only), `cmd/isshoni-desktop/platform_darwin.go` | MA03 | 3 | Mac, CI macos | 01 MD2's: `TestOnMainWait`, `TestDataRootNotFromEnv` and the other darwin tests of 01 §19.1; `platform_darwin.go` uses the data root, device name, `osVersion` and `~` redaction; `task desktop:dev` still runs; the `ISM`/`ism_` prefix and the duplicate-class `nm` check pass in `desktop-macos` |
| MA14 | 2 | Bundle skeleton and menu bar icons | 04 MR1 (the bundle half), 01 MD3; 04 §4; 01 §4.4, §7.4 | `cmd/isshoni-desktop/build/darwin/` (`Info.plist.tmpl`, `isshoni.entitlements`, `isshoni-dev.entitlements`, `AppIcon.icns`, `tray/*.png`), `tools/desktoppkg` (`macver`, `bundle`, `machocheck`), `Taskfile.yml` (`desktop:app:macos`'s body, `SIGN=none`, no `imstatic`) | MA02, MA03 | 2 | Mac, CI macos/ubuntu | 04 MR1's: a universal bundle builds on the Mac and on `macos-26`; `machocheck` passes and fails each negative Mach-O fixture; `--version` runs on both slices (Rosetta); the Info.plist golden for release, dev and e2e holds §3 item 12's values; the dev bundle launches with no Dock icon until its window shows |
| MA15 | 2 | macOS apply and helper | 04 MR7; 04 §7.2–§7.6 | `internal/update` (`untar.go` + `FuzzUntar`, `apply_darwin.go`, `helper_darwin.go`, `codesign_darwin.go`, `policy_darwin.go`, `CurrentLocation`, `Status.Reason`, the new codes, the `updatetest` requirement hook; not `manifest.go`) | MA09 | 2.5 | CI macos/ubuntu, Mac | 04 §14's darwin unit tests (locations, lock handover, `RENAME_SWAP`, `EPERM` → `app_management`, request validation) pass with goleak; no test strings in a release build; M2's Windows updater tests unchanged |
| MA16 | 2 | SPA OS context and i18n | 01 MD13; 01 §16, §17 | `web/src/platform/desktop/os.ts`, 01 §16's SPA branches not owned by later slices (About, App, Sharing, Updates, `authText`, `DesktopPlatform.browser`), `web/src/i18n/` (the i18next `context`), `web/src/i18n/lazy/desktop.en.json` (`_macos` keys), `internal/desktop/i18n` (`Catalog.TFor`), macOS fixtures in `web/src/platform/desktop/testing/` | MA03 | 3 | Mac, CI ubuntu | 01 MD13's: Vitest for each branch with the macOS fixture beside the Windows one; the `_macos` key test (every `_macos` key has its base key); Windows snapshots unchanged; `check:size` passes |
| MA17 | 2 | Audio rules data and service on macOS | 03 AM3; 03 §11, §14.1–§14.3 | `internal/media/audiorules`, `internal/media` (`audio.go`, `probe.go`), `internal/media/native/audio.go`, `internal/media/fake` (macOS scenarios), `internal/desktop/audio`, `internal/desktop/e2ehost` (`-audio-os darwin`) | MA08 | 2.5 | Mac, CI ubuntu/macos/windows | 03 AM3's: the MacOS lists, `FamilySafari` and darwin `Contains`; `SelfInfo.RulesOS`/`TestData`; per-OS ids, rules builder, viewer rule, banners and `selfAudible`; MA08's goldens parsed; the new Go tests pass and every M2 test passes unchanged with `RulesOS` empty on ubuntu and `windows-go` |
| MA18 | 2 | S2 re-run on 14.4, 15 and 26 (owner) | 03 AM0 | `spikes/s2-mac-audio/README.md` (results only) | MA10 | 0.5 + the runs | VMs, Mac | 03 AM0's: `run-selftest.sh -mic` passes 12/12 in each `-granted` guest and on a 44.1 kHz device on 27, or each failure is written down with its design consequence before MA26 merges; a lab blocked by boot or device failure (no boot, no usable output device, silent taps; 05 R3) counts as written down, with R3's fallback named, so MA26 is not held up |
| MA19 | 3 | Engine session core on fakes | 02 EM5; 02 §7 | `native/macos/src/{session,video,engine,testhooks}/`, `native/macos/src/capture/` (the fake source, fake picker, `test_pattern.mm`), its own files in `native/macos/tests/`, `internal/media/native` (`static_*_test.go`, `lockmain_darwin_test.go`) | MA11, MA12 | 3 | Mac, CI macos | 02 EM5's: `session_fake_test` (incl. the mid-stream resize case: one REINIT per layer, no encode error), `session_luma_tap_test`, `frame_slots_test`, `frame_slots_soak`, `nv12_pattern_test`, `rt_alloc_test` pass and TSan is clean; Go `TestStaticConformance`, `TestStaticNativeIntoPublisher`, `TestStaticSession`, `TestStaticStartErrors`, `TestStaticFakePick`, `TestStaticLumaTap`, `TestStaticAudioTone` pass in `macos-native` |
| MA20 | 3 | Process snapshot, responsibility and the macOS report | 03 AM5; 03 §9.6, §9.7, §12, §13.2 | `native/macos/src/audio/` (`proc_snapshot`, `responsibility`, `report_mac`, `tap_engine`'s `Watch`/`Unwatch`/`SetRules`/`ListAppsJson`/`LevelsGet`, `tcc.mm`'s read half; MA05's stub replaced), `native/macos/tests/audio/` | MA07, MA08, MA12 | 3 | Mac, CI macos | `proc_snapshot_test` passes (≤ 5 ms for one new object) and §9.6's budgets hold; `report_mac` goldens; `im_list_audio_apps` through the export layer returns schema v1 with `tap.permission` from the never-prompting preflight; the real-apps listing moves to MA55 (§3 item 14) |
| MA21 | 3 | Signing | 04 MR2; 04 §5.4–§5.7 | `deploy/macos/{sign.sh,ci-identity.sh,dev-signing.sh}` (incl. `-mode testtools` and `ci-identity.sh -name`), `Taskfile.yml` (`desktop:app:macos`'s `SIGN=ci\|dev` step), `ci.yml` (`desktop-macos`'s signing steps) | MA14 | 1.5 | CI macos, Mac | 04 MR2's: an ephemeral identity signs on `macos-26` with the explicit requirement; every §5.5 check passes and `spctl` rejects; a `SIGN=dev` rebuild keeps the dev bundle's System Audio Recording grant (S2 repeated with `io.isshoni.desktop.dev`) |
| MA22 | 3 | Single instance, pending links and URL events | 01 MD4 (first half); 01 §6.2, §6.3 | `internal/desktop/macos` (`singleinstance*.go`, `pending.go`), `cmd/isshoni-desktop/start_darwin.go` (single instance, `kAEGetURL` into `fwd`, `--wait-pid`), `internal/desktop/app` (`PendingStore` wiring) | MA13 | 3 | Mac, CI macos | 01 §6.2's tests (repair, forward, two roots, stale socket, `--wait-pid`); `open isshoni://…` against a production-flavor bundle reaches `Boot` and `HandleArgs` (WM26), also right after a fresh install; a link never starts a share |
| MA23 | 3 | Window, Dock, menus and the menu bar extra | 01 MD5 (first half); 01 §7.1–§7.4 | `internal/desktop/macos` (`window_darwin.*`), `cmd/isshoni-desktop` (`shell_darwin.go`, `menu_test.go`), `internal/desktop/tray` (title, accessibility label) | MA13, MA14 | 3 | Mac, CI macos | The activation policy follows the window (`.regular` shown, `.accessory` hidden or `--autostart`); `TestMainMenuByFlavor` (production has no View, Reload or Inspector); `app.command` navigates; tray fixtures: template icon idle, red live with the watcher count as title, amber paused, yellow warn; `sharingType` none on every window; 01's MD-M6 |
| MA24 | 3 | Hotkeys | 01 MD8; 01 §10.4 | `internal/desktop/hotkey`, `internal/desktop/macos` (`hotkeys_darwin.*`), `internal/desktop/settings` (per-OS defaults), `web/src/desktop/` (`HotkeyField`, `hotkeys.ts`) | MA13 | 3 | Mac, CI all, owner's Mac | 01 MD8's: per-OS hotkey tables and fuzz; `TestHotkeyRegister` (a duplicate is `ErrHotkeyTaken`); `typesChar` via `UCKeyTranslate`; SPA field tests with the ⌃⌥⌘ symbols; MD-M5 |
| MA25 | 4 | VideoToolbox | 02 EM6; 02 §9 | `native/macos/src/{encode,video}/` (`vt_encoder`, `vt_props`, `vt_probe`, `pixel_transfer`, `budget`), its tests, `cmd/isshoni-enginetest` (darwin `main`, the two binaries, `probe`/`capture`/`bench`/`sessions` on darwin) | MA19 | 3 | Mac, CI | 02 EM6's: `vt_encoder_test`, `pixel_transfer_test`, `budget_test` (CI: software VT or an `IM_REQUIRE` skip with a ledger line; Mac: hardware); on the Mac `capture -encoder videotoolbox` passes M2's stream assertions incl. `-bitrate-steps`, `bench -size 1080p -fps 60` encode p99 ≤ 16 ms, `bench -size 2160p -fps 60 -layers 2` reports `layer_failed: q`, `sessions -count 4` all start |
| MA26 | 4 | Tap generation and mixer | 03 AM6; 03 §6, §7.1–§7.5, §8.1–§8.3, §8.7 | `native/macos/src/audio/` (`tap_generation`, `tap_mixer`), its tests | MA18, MA20 | 3 | Mac (System Audio Recording) | 03 AM6's: the native gtest `MacTapHW.*` (`IM_REQUIRE` `tcc:audio` and `hw:audio-out`, run through `harness.sh`; MA30 is in the same group, so the integrator runs it right after group 4 merges) drives `TapGeneration` and `TapMixer` through the `AudioEngine`/`Mixer` facade with a test `PcmSink` and no session: a fixture tone from spawned `AudioQueue` helpers (as `proc_snapshot_test`'s) arrives at −18 ± 1 dBFS, Opus-encoded and decoded inside the test, and a kept-out helper is digital silence; the rate-stage test; the tap-only aggregate and its fallback both start; no leaks (one Instruments Leaks run recorded) |
| MA27 | 4 | The real engine in the build | 04 MR5 | `tools/desktoppkg` (`machocheck -native`, the symbols zip), `tools/notices` (darwin), `deploy/notices/native-darwin.json` (new), `Taskfile.yml` (`desktop:app:macos`'s native step, `TAGS=production,imstatic`, `-X …staticArchiveSHA`), `ci.yml` (`desktop-macos`'s vcpkg cache and native gate) | MA12, MA14 | 1 | CI macos, Mac | 04 MR5's: `machocheck` still passes (no dylib of ours); `--self-check` `ok` with the real engine on `macos-26` without TCC (`engine.loaded`, `abi` 1, `testHooks` false); a build without `imstatic` fails `-thin`; the notices list libopus and speexdsp |
| MA28 | 4 | Codesign keys and requirement blocks | 04 MR3; 04 §5.3 | `deploy/keys/macos_codesign`, `deploy/keys/isshoni-codesign-*.pem` and `isshoni-testtools-1.pem` (test fixtures until the owner's PR), `tools/repocheck` (`keys`), `internal/update/signers.go` (the requirement block), `Taskfile.yml` (`release:codesign-keys`), root `README.md` (key rows) | MA21 | 1 | CI ubuntu, Mac | 04 MR3's: `lint:keys` fails on a drifted block, a 2048-bit key, a missing backup, a `CA:true` certificate and a `testtools` line in `macos_codesign`, checks `isshoni-testtools-1.pem` with the same rules outside the active/backup pair, and passes on the fixtures and, once merged, on the owner's real files |
| MA29 | 4 | Packaging | 04 MR4; 04 §6.1, §6.2 | `tools/desktoppkg` (`macpack`), `deploy/macos/make-dmg.sh`, the `.webloc`, `Taskfile.yml` (`desktop:package:macos`) | MA21 | 1.5 | CI macos, Mac | 04 MR4's: the tar.gz is byte-reproducible (two runs); the DMG attaches, its app equals the bundle, §5.5's checks pass on the mounted copy |
| MA30 | 4 | Harness apps | 05 V3; 05 §4.1 | `tools/macharness` (launcher, bundle assembly, Info.plist, `sign-list.txt`, `{harness,fixture}.entitlements`, `harness.sh`), `Taskfile.yml` (`mac:harness`) | MA06, MA21 | 2 | Mac; CI macos (unit) | 05 V3's: a test binary in the Dev Harness sees `CGPreflightScreenCaptureAccess` true after one grant, and three rebuilds keep it with no prompt; exit statuses 0, 1, 2, 64 and 70 propagate; with `ISSHONI_HARNESS_ARCH=x86_64` a tool reports `sysctl.proc_translated` 1 |
| MA31 | 4 | Relaunch, step 2a and the one-shot modes | 01 MD4 (second half); 01 §6.1, §6.4 | `internal/desktop/macos` (`relaunch_darwin.*`: the relaunch), `cmd/isshoni-desktop` (`start_darwin.go` steps 2, 2a and 7, `oneshot_darwin.go`, the `start`/`window shown` log lines) | MA22, MA15 | 2.5 | Mac, CI macos/windows | Start-order tests with fakes (2a held → exit 0 with `pending.json` written); a terminal start of the dev bundle relaunches (log line) and its WebKit GPU process's responsible PID is isshoni's; `--version` has `arch`; `--self-check` has §3 item 20's fields; `--apply-update` dispatches to `update.RunApplyHelper`; `update.CleanupStaging` runs at step 7; Windows goldens change only by `arch` |
| MA32 | 4 | Quit, system events and keep-awake | 01 MD5 (second half); 01 §7.5, §7.6 | `internal/desktop/macos` (`events_darwin.*`, `awake_darwin.go`), `internal/desktop/app` (quit reason in `ShouldQuit`), `cmd/isshoni-desktop/signal_darwin.go` | MA23 | 2.5 | Mac, CI macos | `TestKeepAwakeAssertion`; app tests for `ShouldQuit` (close, ⌘Q, logout) and `SIGTERM` = `Quit(true)`; lock, unlock, sleep, wake, power-off, screens and session events reach the app with M2 03 §11.6's behavior; MD-M7 |
| MA33 | 4 | Keychain store and the KC check | 01 MD7; 01 §13 | `internal/client/credstore` (`keychain_darwin.*`, `Options.Service`/`Keychain`), `tools/kccheck`, `cmd/isshoni-desktop/creds_darwin.go` | MA14, MA21 | 3 | CI macos, Mac | 01 MD7's: the shared store table passes on a temporary keychain in `desktop-macos`; KC passes (build N re-signed as N+1 with the same CI identity reads the item without a prompt; an ad-hoc build is refused), or the 0600 fallback is taken and the owner told (§7 question 7); no secret in any log |
| MA34 | 5 | SCK listed capture and source changes | 02 EM7; 02 §8.3–§8.7, §8.9, §8.10 | `native/macos/src/capture/` (`sck_stream`, `listed`, `sources`, `frame_info`, the watchers), `native/macos/src/session/supervisor.mm`, its tests, `cmd/isshoni-enginetest` (`sources`) | MA25, MA30 | 3 | Mac, CI | 02 EM7's: `sck_errors_test`, `frame_info_test` and the fake-source cases of `session_fake_test` in CI; on the Mac a 60 s share of the built-in Retina display at the 1080p budget with `layers[].reinits` 0 after the first keyframe and no `SOURCE_RESIZED`, and `frame_slots_soak`'s bound on the ProMotion display (`capture.held` ≤ 6); in the Dev Harness (`task mac:hw:engine`): `TestHWListedDisplay`, `TestHWVideoToolbox`; M-SCK-DISPLAY and M-SLEEP recorded on 27 |
| MA35 | 5 | Audio session wiring | 02 EM10; 02 §10 | `native/macos/src/session/session.mm` (audio wiring: the Mixer, audio-only sessions, the PCM tap, the `session_limit` check), its tests (`audio_bridge_test`) | MA19, MA26 | 1.5 | Mac, CI | 02 EM10's: `audio_bridge_test` with a fake `AudioEngine` in CI; on the Mac an audio-only session captures a fixture tone (PCM and decoded Opus) with a kept-out fixture absent; a second audio session is `IM_ERR_LIMIT` `session_limit`; on the Mac `capture -audio mix` gives Opus on the 20 ms grid and the PCM tap matches it |
| MA36 | 5 | Tap controller | 03 AM7; 03 §7.6, §7.7, §8.5 | `native/macos/src/audio/tap_controller.*`, its tests (incl. the fake HAL) | MA26, MA06 | 3 | Mac (TCC), CI macos | 03 AM7's: the fake-HAL suite green in `macos-native`; on the Mac a runtime rule on an audible app has no click and finishes ≤ 200 ms, and a default-device switch rebuilds without a voice leak; `MacTapHW.*` with their ledger lines |
| MA37 | 5 | Test render and the stamp offset | 03 AM9; 03 §13.5 | `native/macos/src/audio/test_render.mm`, its tests | MA26 | 1.5 | Mac (TCC) | 03 AM9's: case 20's bounds hold on the Mac (median ≤ 15 ms, spread ≤ 5 ms); `im_test_render_bursts` returns predicted device times in mach ns; the measured offset goes under "Results for the spec" (03 §21's change log) |
| MA38 | 5 | Updater e2e on macOS | 04 MR8; 04 §9.1 | `deploy/test/macos/update-e2e.sh` (and the relserve CA trust for the step) | MA29, MA31 | 1.5 | CI macos | 04 MR8's: cases A–F green in `macos-update` (ci.1 → ci.2, wrong signer, hash mismatch, downgrade, read-only copy, start during apply); the script exits 2 outside GitHub Actions |
| MA39 | 5 | install-desktop.sh | 04 MR9; 04 §6.3 | `deploy/install-desktop.sh`, `deploy/test/install_desktop_unit.sh`, `deploy/test/macos/install-e2e.sh`, `Taskfile.yml` (`release:keys`/`release:prepare` blocks) | MA28, MA29 | 2 | CI ubuntu/macos, Mac | 04 MR9's: unit tests under dash, `bash --posix` and macOS sh; shellcheck clean; the copied-functions check against `install.sh`; `install-e2e.sh` passes every case of 04 §14 (install, reinstall, downgrade refusal, tampered, wrong-key and wrong-signer refusals, running app exit 6, uninstall `--purge`) |
| MA40 | 5 | task mac:hw | 05 V4; 05 §5.4 | `Taskfile.yml` (`mac:hw*` bodies incl. `mac:hw:desktop`), `tools/macharness` (`summary.json`) | MA30 | 1 | Mac | 05 V4's: with the grants given, no `tcc:*` skips in `summary.json`; after `tccutil reset`, the suite fails with the `tccutil` hint |
| MA41 | 5 | g2g on macOS | 05 V5; 05 §4.2 | `tools/g2g` (`capture_darwin.{go,m}`, refresh rate, `osVersion`, `machine`) | MA30 | 2 | Mac; CI macos (unit) | 05 V5's: `TestSCKCaptureHW` passes on the Mac; Safari's clock page against a 50 % Chrome copy (no isshoni) reads Δ p95 ≤ 34 ms with ≥ 99 % decoded; `bench` with and without g2g running differs by ≤ 2 ms p95 |
| MA42 | 5 | WKWebView settings, navigation layer 3 and the e2e driver | 01 MD6; 01 §3.2, §8; 05 §6.2 | `cmd/isshoni-desktop` (`webview_darwin.go`, `e2edriver_darwin.{go,m}`, `checklist_darwin_test.go`), `internal/desktop/macos` (`webview_darwin.*`: the native URL read), `internal/desktop/assets` (the e2e-only `GuardWKWebView` allowlist entry) | MA14, MA31 | 3 | CI macos, Mac | 01 MD6's: WM1's `Origin` recorded first and the server decision written in the PR (none expected); every WM row marked CI recorded; `TestWailsChecklist`'s Go-side rows annotated in `desktop-macos`; the bindings drift check unchanged (`E2E.Report` excluded); the production binary has no `e2edriver` symbol |
| MA43 | 5 | Autostart | 01 MD9; 01 §12 | `internal/desktop/macos/autostart_darwin.go`, `internal/desktop/app` (login-item launch), `cmd/isshoni-desktop` (the deferred shell on darwin) | MA31, MA32 | 2.5 | owner's Mac, CI macos | 01 MD9's: app tests (a login-item launch shows no window); `TestLoginItemRoundTrip` (`task mac:hw:desktop`); WM25/MD-M11 on a real login, else 01 §12.3's fallback with 04's LaunchAgents plist |
| MA44 | 5 | Notifications | 01 MD10; 01 §11 | `cmd/isshoni-desktop/notify_darwin.go`, `internal/desktop/macos/tcc_darwin.go`, the `_macos` notification texts in `desktop.en.json` | MA14, MA32 | 2.5 | owner's Mac, CI macos | 01 MD10's: M2's toast tests unchanged and green; `TestNotificationsRoundTrip` (`task mac:hw:desktop`); WM8 |
| MA45 | 5 | Permissions service | 01 MD11 (Go half); 01 §9 | `internal/desktop/macos` (`pages*.go`, `localnet.go`), `internal/desktop/bridge` (`permissions.go`, `soundCheckBound` 40 s on darwin), `internal/desktop/app` (`Relaunch`, `permissions.changed` on key-window), `internal/desktop/audio` (`PermissionState`, 03 §14.1) | MA17, MA31 | 3 | Mac, CI | Bridge tests (a 15 s fake answer on darwin, the 40 s bound; idle denied → granted on key-window through `PermissionState`, which answers within 1 s and builds no tap); `localnet` classification table; 01 §9.4's System Settings URL table by OS version; `TestAudioPermissionFlow` and `TestSoundCheckMac` compile and skip in CI with ledger lines, and run on the owner's Mac once MA47 has merged |
| MA46 | 6 | The picker | 02 EM8; 02 §8.2 | `native/macos/src/capture/` (`picker.mm`, `pick_tokens`), `internal/media/native` (`Pick` mapping, `SourceDetail.BundleID`), `cmd/isshoni-enginetest` (`pick`, `main_darwin.go`, `runloop_darwin.m`) | MA34, MA30 | 3 | Mac, VMs, CI | 02 EM8's: `pick_tokens_test` and `picker_logic_test` (incl. `main_thread`); M-PICK-MAC on 27 and in the 14.4, 15 and 26 VMs; M-SCK-WINDOW and M-OS-STOP on 27; a pick goes live and is watched in a browser |
| MA47 | 6 | Permission state and the sound check | 03 AM8; 03 §13.2, §13.3 | `native/macos/src/audio/` (`tcc.mm`'s state machine, `sound_check.mm`), `internal/desktop/audio` (the 35 s context on darwin) | MA36, MA17 | 2 | Mac (TCC, `tccutil reset`) | 03 AM8's: the fake-HAL permission tests; on the Mac, M-R8's flow (deny, banner, grant, sound check passes) works end to end with the wizard, and an unanswered prompt gives `permission_pending`, not `denied` |
| MA48 | 6 | The server serves install-desktop.sh | 04 MR10; 04 §6.4 | `deploy/embed.go`, `internal/server/httpapi` (`RenderInstallSH`, the route, the startup check) | MA39 | 0.5 | CI ubuntu, Mac | 04 MR10's: httpapi tests; `task dev` serves a pre-filled script; no unknown-env warning (the integrator reserves the names) |
| MA49 | 6 | macos-runtime and the nightly | 04 MR12; 04 §9.3 | `.github/workflows/nightly.yml` (`macos-full`, `macos-runtime-full`, `macos-native-full`, `goreleaser-full`, `go-cross-os`), `ci.yml` (`macos-runtime`'s steps) | MA27, MA38, MA39 | 1 | GitHub | 04 MR12's: one green nightly with real macOS files; `macos-15-intel` runs the x86_64 slice natively and `macos-14` launches the bundle without a dyld error |
| MA50 | 6 | The WK spec's driver and fixture | 05 V9; 05 §6 | `web/e2e/wk/` (driver client, fixture, rows WM1, WM2, WM7), `web/e2e/desktop/common.ts` (the CDP helpers moved), `web/e2e/cdp/` (imports only), `Taskfile.yml` (`e2e:wk`) | MA42 | 2.5 | Mac, CI macos/windows | 05 V9's: WM1, WM2 and WM7 pass on the Mac and in `macos-wk` (`macos-15`, `macos-26`); the production `nm` check passes; the CDP spec stays green on `windows-desktop` with its annotation rows unchanged |
| MA51 | 6 | SPA permissions and the macOS harness | 01 MD11 (SPA half); 01 §9.3–§9.5, §19.3 | `web/src/desktop/` (`AudioPermissionStep.tsx`, `PermissionsSettings.tsx`, the wizard step, Offline's Local Network line), `web/src/share/native/` (the panel's Local Network line), `internal/desktop/e2ehost` (`-os macos`, fake `Permissions`), `web/e2e/desktop-macos.spec.ts` | MA45 | 3 | Mac, CI ubuntu | 01 MD11's SPA half: Vitest for every permission state; `desktop-macos.spec` green under `-os macos` in Chrome on ubuntu (permission step and recovery, Settings → Permissions, the `local_network` error, `app.command`); MD-M1, MD-M9, MD-M10 on the owner's Mac; WM24 on every OS version |
| MA52 | 7 | Own windows | 02 EM9; 02 §8.8 | `native/macos/src/capture/filter.mm`, `native/macos/src/window/protect.mm`, `internal/media` (`media.go`: `Source.SelfVisible`, `Event.SelfVisible`), `internal/media/fake` (`SelfVisible`), `internal/media/native` (`SelfVisible`, `UnprotectWindow`), `cmd/isshoni-enginetest` (`protect-check` on darwin) | MA46 | 2.5 | Mac, VMs, CI | 02 EM9's: `os_info_test`'s strategy table, `picker_logic_test`'s strategy-C cases, Go `TestSelfVisible*`; M-SELF-EXCLUDE on 14.4, 15.0/15.1, 15.2+, 26 and 27 with the results written into 02 §8.8's table and `filter.mm`; a strategy that shows a TCC prompt counts as not working |
| MA53 | 7 | The /download page's macOS block | 04 MR11; 04 §6.5 | `web/src/download/`, `account.download.macos.*`, `web/e2e/download.spec.ts` (macOS cases) | MA48 | 1 | CI ubuntu, Mac | 04 MR11's: Vitest and Playwright assert the DMG URL, `shCommand`, `brewCommand`, the Mac ordering and the iPad rule; axe passes; the Windows block unchanged |
| MA54 | 7 | The WK spec complete | 05 V10; 05 §6.4, §6.5 | `web/e2e/wk/` | MA50 | 2.5 | CI macos, Mac | 05 V10's: every WM row §6.5 assigns to the WK spec annotated in `macos-wk` on both images; quit while live shows "Stop sharing and quit?" and exits ≤ 3 s after confirmation; quit while idle ≤ 3 s; the end-to-end case (link, room, share the fake pattern, a viewer receives frames) |
| MA55 | 7 | audiotest on darwin | 03 AM10; 03 §16.1–§16.7 | `cmd/isshoni-audiotest` (driver and run loop, fixture verbs, `casesmac.go`, `fixtureplan_mac.go`, `macapp/`, preconditions, the report schema) | MA20, MA26, MA36, MA37, MA47, MA30 | 3 | Mac, CI all | 03 AM10's: `selfcheck` in `macos-native` and `desktop-macos`; `isshoni-audiotest apps` lists real apps with correct rows (§3 item 14); on the Mac `harness.sh isshoni-audiotest run` gives every case of 03 §16.5 a verdict; the portable tests green on all CI |
| MA56 | 7 | Release pipeline | 04 MR13; 04 §10.1–§10.6 | `.github/workflows/release.yml` (`macos-app`, `macos-sign` (the app only), `macos-sign-tools` (the exit harness, `testtools` environment), `macos-pkg`, `macos-exit-tools` shell, `desktop-manifest`, `build`/`publish`, `verify-macos`), `.goreleaser.yaml`, `check-draft.sh` and its test, `deploy/desktop/macos-since` | MA27, MA39, MA49, MA30 | 2 | GitHub dry run | 04 MR13's: the dry run is green end to end incl. `verify-macos` on `macos-15` and `macos-26`; `macos-sign` and `macos-sign-tools` run no Go or Node, and `macos-sign`'s sparse checkout has no `tools/macharness`; `check-draft.sh`'s new cases pass |
| MA57 | 7 | The SPA's audio on macOS | 03 AM4; 03 §14.4, §14.5 | `web/src/platform/desktop/bridge.ts` (`FriendsHear.selfAudible`, `ShareCaps.audioBackend`), the audio rows without meters, the `audio-permission` and `tap-failed` banners and their texts, About's backend key, the darwin scenarios' Playwright cases | MA17, MA51 | 1.5 | Mac, CI ubuntu | 03 AM4's: lint, typecheck, Vitest and Playwright green with the darwin scenarios; the banner action calls `App.OpenOSSettings('audio-recording')`; Windows snapshots unchanged |
| MA58 | 7 | Real-engine conformance and the CI ledger | 05 V8; 05 §5.3, §5.6 | `internal/media/native` (`TestStaticConformanceVT`, `TestTapWithoutGrant`), `ci.yml` (`macos-compat`, 05's `macos-native` steps), `nightly.yml` (`-count=3`), `.github/macos-skips.txt` lines | MA25, MA35 | 1.5 | CI | 05 V8's: every macOS job green with the ledger; `TestTapWithoutGrant` (no grant: `IM_OK`, the permission state, silence packets for 3 s, stop ≤ 300 ms); `macos-compat` runs the newest-SDK binaries on `macos-14` with no dyld error; the Windows jobs green and the CDP annotation rows unchanged |
| MA59 | 8 | Engine tools on macOS | 02 EM11; 02 §13 | `cmd/isshoni-enginetest` (`soak_darwin.go`, `avsync_darwin.{go,m}`, `flash_darwin.m`, `raw_darwin.go`, `*_other.go`), `native/macos/tools/hwcheck.sh` | MA46, MA52, MA35 | 2.5 | Mac, VMs, CI | 02 EM11's: the portable tool tests; on the Mac `hwcheck.sh` produces a bundle with no names or paths, `soak -hours 2` passes, `avsync -source pick` gives a verdict; in a VM `avsync -encoder fake` exits 0 |
| MA60 | 8 | Share flow on macOS | 01 MD12; 01 §10 | `internal/desktop/share`, `internal/client/core` (`ShareSnapshot.SelfVisible`, `engineEnd`'s `cancelled`, `ShareRequest.Override`), `internal/desktop/app` (auto-minimize once), `internal/desktop/tray` (the kind-based label), `web/src/share/native/`, `web/src/viewer/endpointMute.ts` | MA32, MA45, MA51, MA52, MA57 | 3 | CI macos/ubuntu/windows, owner's Mac, VMs | 01 MD12's: core tests (`SelfVisible`, the `cancelled` end, the 4K override with a fake refusing two-layer 4K60); share-controller tests for the self-exclusion rule table and `fourKChoice`; `mutesViewer` with `selfVisible` and `selfAudible`; Vitest per branch; the WK spec's end-to-end share; MD-M2 (incl. 15.0/15.1), MD-M3, MD-M4 |
| MA61 | 8 | audiotest specials on darwin | 03 AM11 | `cmd/isshoni-audiotest` (timestamps, opus, multi-endpoint, soak, device-rate-change; `record`/`merge` with macOS fields) | MA55 | 2 | Mac | 03 AM11's: each special produces its report section; the 30 min soak completes on the Mac |
| MA62 | 8 | Homebrew | 04 MR14; 04 §6.6 | `deploy/homebrew/` (`isshoni.rb.tmpl`, `render.sh`, `known_hosts`), `release.yml` (the `homebrew` job), `ci.yml` (`desktop-macos`'s `brew` steps), `nightly.yml` (the local-tap install in `macos-full`) | MA56 | 1 | CI macos, GitHub | 04 MR14's: the cask passes `brew audit --cask --strict` (if the audit objects to the `xattr` postflight, the postflight is dropped and Open Anyway documented; `--strict` stays); the nightly installs and zaps it from a local tap; one push to the real tap verified on a `ci-check` branch, then deleted (owner action 2 first) |
| MA63 | 8 | Site and docs | 04 MR15; 04 §11; 01 §20 (anchors); 03 §22 (troubleshooting text) | `docs/*.md` site pages, `site.yml`, `release.json`'s `macos` fields, `/keys/*.pem`, `internal/server/ops/doctor/docsanchors_test.go`, root `README.md` (install section) | MA56 | 1.5 | CI ubuntu | 04 MR15's with §3 item 7's anchors: the `site` job and `TestDocsAnchors` pass; nothing loads from another origin |
| MA64 | 9 | enginetest exit wiring | 05 V6; 05 §4.3 | `tools/macharness` (`Resources/avsync/` helper bundles), `cmd/isshoni-enginetest` (`protect-check`'s judging against 02 §8.8's table, darwin `TestCommands*`) | MA59, MA30, MA37 | 2 | Mac, VMs, CI macos | 05 V6's: on the Mac `avsync -source pick` exits 0 (≤ 20 ms), `protect-check -sck pick`'s outcome is the one 02 §8.8 names for 27, `bench -source mon:<main>` p95 ≤ 25 ms, `sessions -count 4`; in a VM `avsync -encoder fake` and `capture -source pick -encoder fake` exit 0; `TestCommands*` in `macos-native` |
| MA65 | 9 | audiotest exit wiring | 05 V7; 05 §4.4 | `cmd/isshoni-audiotest` (the A-2 comparison, file names with `-vm`, merge refusals, home-path redaction) | MA61 | 1 | Mac; CI ubuntu/macos | 05 V7's: `run -gate` exits 0 on the Mac through the Dev Harness; unit tests of names, merge and redaction |
| MA66 | 9 | Hardware and OS record (owner) | 02 EM12; 02 §17.4, §17.5 | `docs/m3/exit-test/results/` (component numbers), `docs/m3/02-macos-engine.md` (§8.8, §8.2 step 4, §8.10 as measured) | MA59 | 1 + the runs | Mac, VMs | 02 EM12's: M-BENCH-MAC, M-SOAK-MAC and M-AVSYNC-MAC on 27; the functional VM checks on 14.4, 15 and 26 (`-encoder fake`); an Intel run if a machine is found; results without machine names |
| MA67 | 10 | The exit tools artifact | 05 V11; 05 §4.5 | `release.yml` (`macos-exit-tools`' content and checks), `tools/macharness` (the exit variant's checks) | MA56, MA64, MA65 | 1.5 | CI (release), Mac | 05 V11's: an rc-like run yields `isshoni-exit-tools-macos`; on the Mac `shasum -a 256 -c SHA256SUMS`, `selfcheck` and `probe` pass; the Exit Harness signed with `isshoni-testtools-1` keeps its grants across two rc builds, and `harness.sh --cleanup` removes them |
| MA68 | 10 | Budgets and soak | 05 V14; 05 §8 | `cmd/isshoni-enginetest` (`soak`'s footprint sampling), `web/e2e/wk/` (budget annotations) | MA64, MA54 | 1.5 | Mac | 05 V14's: a 2-hour soak report from the Mac within 05 §8's budgets (app ≤ 150 MB, WebKit processes ≤ 350 MB, start ≤ 1.5 s warm / ≤ 3 s cold), or each miss recorded with its cause |
| MA69 | 11 | Exit run book and template | 05 V12; 05 §9 | `docs/m3/exit-test/README.md`, `docs/m3/exit-test/results-template.md` | MA41, MA64, MA65, MA67 | 1.5 | Mac | 05 V12's: a dry run of A and B on the Mac with a pre-rc build finds no missing step; the book follows §6 below |
| MA70 | 11 | Optional self-hosted runner | 05 V13; 05 §5.5 | `.github/workflows/machw.yml`, `deploy/macos/runner/README.md` | MA40, MA67 | 1 | CI (lint), Mac (optional) | 05 V13's: actionlint passes; dispatch-only with environment approval; if the owner registers a runner, one `hw` dispatch uploads a summary |
| MA71 | 9 | Desktop write-backs and the manual run | 01 MD14; 01 §3.2, §13, §19.5 | `docs/m3/01-desktop-macos.md`, `docs/m2/02-device-auth.md` (§11.5), `docs/security.md` (the Keychain line) | MA33, MA42, MA43, MA44, MA60 | 0.5 + the runs | owner's Mac | 01 MD14's: the WM table, KC's result and the WM1/WM2 decisions written in; docs/m2/02 §11.5 records the macOS account format `isshoni:v1:<leaf>:<origin>`; MD-M1…MD-M14 on the owner's Mac (27) recorded; the VM rows handed to 05's M-MAC rows |
| MA72 | 12 | M3 exit: audio on four versions, latency on 26 and 27 (owner) | 05 V15, 03 AM12; 05 §2, §9; §6 below | `cmd/isshoni-audiotest/results/`, `cmd/isshoni-audiotest/README.md` (the results table), `docs/m3/exit-test/results/` | every slice of groups 1–11; an rc from MA56 | 1 + the runs | Mac (27, and 26 on its volume), VMs (14.4, 15.0/15.1, 15, 26 for first-run rows) | On `v0.3.0-rc.N`: §6's A on all four versions (`run -gate` exit 0, or 3 under A-1's mic rule in a VM; A-2's commit and hash match; the M-R rows of 05 §2.1; `avsync` ≤ 20 ms), B's L-1 on 26 and 27 (p95 < 200 ms at the 1080p pixel budget and 60 Hz with sound and content cuts) and L-F in the 14.4 and 15 VMs, C's rows per version; 03's troubleshooting VolumeNote decided from M-R10; the record committed with no machine, VM, user or server names |
| MA73 | 12 | First release with the Mac app (owner) | 04 MR16; 04 §10.7 | the release-prep PR (release notes header, README key rows) | MA62, MA63, MA72 | 1 + the release | Mac, VMs, GitHub | 04 §10.7's checklist: M-MAC-1, 2, 3, 8, 9, 16, 22, 23 on 14.4, 15, 26 and 27 with 04 §14's pass criteria; `v0.3.0` tagged, approved and published; the tap carries 0.3.0 and `brew install --cask moonwx/isshoni/isshoni` opens without a dialog, or with exactly the documented Open Anyway path once (recorded) |

The Grp column is the plan; the integrator records how the groups actually run in "How the plan works", as M2's
README did, and no row is renumbered.

## 6. M3 exit run book (outline)

MA69 writes this out as `docs/m3/exit-test/README.md` with `results-template.md` (one column per macOS version) and
links MA10's `vm-lab.md`; the owner runs it in MA72. Where the book and the specs differ, the specs win and the book
is fixed (05 §9).

**Pass**: on one rc `v0.3.0-rc.N`, every row of 05 §2 on the versions it names: **A** (all rows) + **L-1/L-2 on 26
and 27** (bare metal) + **L-F in the 14.4 and 15 VMs** + **C**.

**Preconditions** (05 §9.1)
- The rc tagged; its `release.yml` run green including `macos-exit-tools`; the rc server (M1's exit-test VPS, RTT ≤
  50 ms) on the same rc.
- Each machine: the Mac on 27, the Mac's 26 volume, and fresh clones of `macos14-granted`, `macos15-granted` (and
  `-clean` clones, incl. `macos26-clean`, for the install and first-run rows); the app installed from the rc server's
  `/download` with the curl line, `--version` = the rc, linked.
- The exit tools `isshoni-exit-tools-macos` downloaded once, then on each machine `shasum -a 256 -c SHA256SUMS`, the tag
  and the test-tools fingerprint in `README.txt`, the quarantine removed from `isshoni Exit Harness.app`, `harness.sh
  isshoni-audiotest selfcheck` and `harness.sh isshoni-enginetest probe`.
- A microphone (VM: the host's), output on built-in or wired speakers, nothing else playing; Discord desktop signed
  in, Chrome, Safari; Steam with one game on the Mac; each mic fixture has Microphone access.

**A. Audio** (≈ 15 minutes per version, 05 §9.2): `harness.sh isshoni-audiotest run -gate -out …` → exit 0 (exit 3
accepted in a VM when the only SKIPs are the mic cases and 27 passed them); compare `engine.commit` and `tool.sha256`
with the exit tools' manifest (A-2); the M-R rows of 05 §2.1 with `record -label MR<n>`; `harness.sh
isshoni-enginetest-hooks avsync -source pick` (VMs add `-encoder fake`) → |median| ≤ 20 ms; reports merged on the Mac
and committed as `cmd/isshoni-audiotest/results/<date>-macos<major>-<build>[-vm].json` with the README row.

**B. Latency** (≈ 25 minutes each on 27 and 26; ≈ 10 minutes L-F per VM, 05 §9.3): display at 60 Hz, AC power, Low Power
Mode off; `harness.sh g2g serve`, the clock page in Safari filling the display (not a full-screen Space), **Start**;
share the display (preset Auto, Share sound on; Details: the 1080p pixel budget at 60 fps, about 8 Mbit/s, hardware
VideoToolbox); Chrome as the same user at 50 % over the bottom-right half with the share's speaker on; What friends hear
shows Safari heard and Chrome kept out. Then `g2g measure -d 60s -run gate` → exit 0 (L-1), the diagnostic `nocuts` and
`nosound` runs and `isshoni-enginetest bench` (L-3). In the 14.4 and 15 VMs (L-F): the app's Share shows the
`no_hardware` message, then `harness.sh isshoni-enginetest-hooks capture -source pick -encoder fake` → exit 0.

**C. Release-candidate checks** (05 §2.3, §9.4): `protect-check -sck pick` per version (including `macos15-0`)
matching 02 §8.8's table; the M-MAC rows per version (install by curl, DMG + Open Anyway, grants and the watchdog,
picker, own windows, **update N → N+1 keeps every grant**, run from the mounted DMG, App Management on 15+, Local
Network on 15+, Homebrew and autostart on 14.4 and 27, the rest on 27); WM23 and WM24 by hand; the WK spec green on the
rc's commit.

**D. Record** (05 §9.5): `docs/m3/exit-test/results/<date>.md` from the template and the folder
`docs/m3/exit-test/results/<date>/` with the `g2g` JSON files, the VM captures, the `avsync` reports, the `bench`
output, `vm-versions.json`; the four audiotest report paths, every M-MAC and WM row with its result (and, if carried
over, its rc), 02's manual checks and the component numbers beside g2g. Before committing, `grep` the added files for
each computer, VM, user and server name and every IP: no match may remain.

**E. On a failure**: a fix slice and a new rc. Every new rc repeats A-1, A-2, A-9 and B on every version; the M-R rows,
A-3 and the C rows repeat only after a change that can affect them (`native/macos/`, `internal/media/`,
`internal/desktop/`, `cmd/isshoni-desktop/`, the packaging), otherwise they carry over naming their rc. When everything
passes: MA73 (release-prep PR, `v0.3.0`, the tap).

**Release tiers from `v0.3.0` on** (05 §9.6): every release runs `run -gate` and M-R1 on 27 with `avsync` and a smoke,
and attaches `audiotest-macos27.json`; every minor release runs the full matrix on all four versions; each new macOS
beta gets `task mac:hw` and `audiotest run` in a beta VM. A community or borrowed-Mac run (M-MAC-17, 05 R3) ends
with `./harness.sh --cleanup` and deletes the exit-tools folder: a granted harness lets any process of that user
capture without a prompt.

## 7. Questions for the owner

Each has a default that the specs implement until it is answered.
1. **Sign-off at MA01, before the EM/AM slices: latency on 14.4 and 15** (05 Q1, V-D3): the Mac can't boot 14 or 15
   and VMs have no hardware encoder. Default: L-1/L-2 gate on bare-metal 26 (a required second APFS volume, about an
   hour per minor release) and 27; 14.4 and 15 run the audio gate, the picker, own windows and grants in VMs plus a
   functional share check (L-F). The alternative is to borrow Macs on 14 and 15.
2. **If a guest won't boot, has no output device for taps, or its taps are silent** (05 Q2, R3): default: that
   version's A and C rows move to the 26 volume (for 26), a borrowed Mac, or a community exit-harness report named in
   the record. The alternative is to hold M3 until such a Mac is found.
3. **Intel** (02 Q1, 03 Q3, 05 Q3): no Intel Mac is available. Default: ship the universal binary; the x86_64 slice is
   tested on CI (native launch on `macos-15-intel`, unit tests under Rosetta) and its audio is labelled
   "community-tested". The alternative is to drop the Intel slice until someone tests it on real hardware.
4. **Closing the window when not sharing** (01 Q2): default: quit, as on Windows (M2 03 D13). Most Mac apps keep
   running after their last window closes; the alternative makes "keep running in the menu bar" the macOS default.
5. **Default hotkeys** (01 Q3): default ⌃⌥⌘P (Pause/Resume) and ⌃⌥⌘S (Stop); no Accessibility permission is needed.
6. **Whole-screen share where isshoni can't be kept out of the picture** (01 Q1; macOS 15.0–15.1, or a filter macOS
   refuses): default, the plan's rule: minimize isshoni and mute its in-app viewer for the whole share. The alternative
   mutes only when 03 can't confirm isshoni's own audio is excluded.
7. **If a re-signed update can't read the Keychain without a prompt** (01 Q4): default: a 0600 credentials file in the
   data root (the same same-user threat model as Windows' Credential Manager). The alternative is a Keychain password
   prompt after each update.
8. **If macOS 15 or 26 asks for Local Network again after every update** (04 Q1): default: give each slice a constant
   `LC_UUID` (crash logs are then matched to symbols by version). The alternative is one prompt per update, stated in
   the release notes.
9. **Homebrew for release candidates** (04 Q2): default: the tap carries final releases only. The alternative is a
   separate `isshoni@beta` cask.
10. **The macOS build runner** (04 Q3, §3 item 1): default: `macos-26`, written back into the plan's "Build, CI and
    release" by MA01; fallback `macos-15` with Xcode 26.
11. **Audiotest case 33 changes the output device's sample rate** during the run, then restores it (03 Q1): default:
    a required gate case (it SKIPs on a device without 44.1 kHz). The alternative makes it opt-in.
12. **Self-hosted runner on the Mac** (05 Q4): default: run `task mac:hw` and the gate by hand; MA70 builds the
    dispatch-only workflow, and registering a runner is your call.

## 8. Owner actions (not decisions)

- **Before the first rc, and so that MA28's checks run on the real files** (04 §12): create the codesign identities
  `isshoni-codesign-1` and the offline backup `isshoni-codesign-backup-1` and the test-tools identity
  `isshoni-testtools-1` (no backup) on the Mac, outside the repository; open the PR with the three `.pem`s,
  `deploy/keys/macos_codesign` and `task release:codesign-keys`' output (MA28's checks then run on the real files);
  put `MACOS_CODESIGN_P12_BASE64` and `MACOS_CODESIGN_P12_PASSWORD` into the `release` environment and
  `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` alone into a new `testtools` environment (approval, `v*` tags); keep the
  `.p12`s
  and passwords in the password manager plus an encrypted offline copy. Check that `release` still requires your
  approval and allows only `v*` tags (and that `testtools` does too).
- **The dev identity**: run `deploy/macos/dev-signing.sh` once on the Mac after MA21 merges (S2's identity, which
  keeps grants across rebuilds); it never leaves the Mac.
- **The Homebrew tap, before MA62's real push**: create the public repository `MoonWX/homebrew-isshoni` (README,
  `Casks/.gitkeep`, Apache-2.0), add a write deploy key for that repository only, put its private half into
  `HOMEBREW_TAP_DEPLOY_KEY` in the `release` environment and delete the local copy (04 §12 item 2).
- **The 26 volume and the VM lab (MA10, group 1)**: macOS 26 (latest 26.x) on a required second APFS volume of the
  Mac; Tart (or UTM) golden images for 14.4.x, 15.0/15.1, 15 and 26 from Apple's restore images, first checked for
  boot and virtio output/input, each with an admin and a standard account, `-clean` and `-granted` clones; S2's spike
  tools in each guest (MA18).
- **Grants** (once each, then kept by the stable identities): after MA30, the Dev Harness gets Screen Recording and
  System Audio Recording and each mic fixture the Microphone, on the Mac and in each `-granted` VM clone; at the exit,
  the test-tools-signed Exit Harness the same (also on the 26 volume); the dev bundle `io.isshoni.desktop.dev` System
  Audio Recording when its wizard asks. Never grant a terminal app.
- **Test accounts and apps** on the Mac and the VMs: Discord desktop signed in, Chrome, Safari; on the Mac also Steam
  with one game and, where you can, Teams, Zoom, Slack and Vesktop for M-R7 (the others are marked "unverified").
- **The "Manual:" items** of the slices that list the owner's Mac or the VMs (MA25, MA26, MA34, MA36, MA37, MA41,
  MA43, MA44, MA46, MA47, MA51, MA52, MA59, MA60, MA64), recorded in their PRs; MA66's hardware record and MA71's manual
  run; then MA72 (the exit) and MA73 (the release), including the "Open Anyway" screenshots for 14 and 15+ after the
  first final release.
- **Optional**: an Intel Mac on 14.4–26 for one hardware run (§7 question 3); a self-hosted runner registered for
  `machw.yml` (§7 question 12).
- **Still open from M2** (`docs/m2/README.md` §8): the M2 exit run and `v0.2.0` (WA90, WA91), the Windows checks that
  wait for the PC, and M0's open spike items (S1's audio exclusion on the PC, S4's Firefox and Safari viewer check).
  M3's release can carry both apps if `v0.2.0` hasn't shipped (§3 item 21).

## 9. Review log

The design review's findings, as kept after verification (4 further findings were refuted and are not listed). Ids are
the review's; "applied" names where the change landed.

**This README and the slice plan**
- scope-1 (major): applied. MA26 is accepted by the native `MacTapHW` gtest through the `AudioEngine`/`Mixer` facade
  with a test `PcmSink` and in-test Opus (no session); the audio-only session capture moved to MA35; §5's milestone,
  03 AM6's scope and dependency (no longer "Opus through 02's session") and 02 EM10's acceptance match.
- scope-7 (minor): applied. MA05 (02 EM3) writes the final forwarding of every audio and test-render export through
  `audio_bridge::Engine()`/`SetEngineForTest`; MA35 (02 EM10) is session wiring only and no longer touches `api/`;
  §3 item 8, the shared-files row, 03 AM5's dependency and acceptance (no `audio_bridge_test`) and 02 §10 match.

**Cross-doc follow-ups** (the doc editors applied their own side; these are the other docs' parts)
- security-1 (05): applied. 04 §5.1 (the identity), §5.3 (`repocheck keys`), §5.4 (`-mode testtools`), §5.6
  (`ci-identity.sh -name`), §5.7, §10.1 (`macos-sign-tools`; `macos-sign` signs only the app with no
  `tools/macharness`), §12 item 1a, §15, MR2/MR3/MR13; 03 §16.1; README §1, §3 items 3 and 17, §4 naming, MA21, MA28,
  MA56, MA67, §6 (`--cleanup`, fingerprint) and §8.
- media-audio-4 (05): applied. README §1, §3 item 4, §6, §7 Q1 (now a sign-off at MA01), §8, MA01, MA10, MA72;
  02 Q2 points to 05 V-D3, 02's §16 and §19 name the 26 volume.
- scope-3 (05): applied. MA10 starts with boot and virtio output/input; MA18 and 03 AM0 count a lab blocked by boot or
  device failure as written down; §7 Q2 widened.
- ux-4 (05): applied. 03 §18.2 adds `MacTapPermission.WatchdogUnknownDenies` (preflight stubbed to `unknown`, a heard
  process playing → `denied`, detail `watchdog`), named in AM7 and AM8.
- scope-9 (04): applied. MA73's and MA62's acceptance; 05 M-MAC-3's pass criterion.
- ux-1 (04): applied. 05 M-MAC-1 copies the `sh -s -- --room <id>` command and asserts the room opens with no URL.
- ux-6 (04): applied. 05 M-MAC-1 and §3.2's standard-user run; 01 §14.3's `not_writable` text; 01 §21's `lsregister`
  line follows 04 §6.3 step 4.
- security-2 (04): applied. 01 adds `update.downgrade` (codes, text, §17, §20, §21) and cites 04 §7.3 step 7 for the
  relaunch.
- security-3 (01): applied. 04 §5.2 (no Keychain prompt; link again), §5.8 step 3 and new 3a (revoke and relink),
  §5.6 `-name`; README §4's account format and MA71's docs/m2/02 §11.5 write-back.
- security-5 (01): applied in part. 05's WM5 row gains the event-gate assertion. Declined for 04's `macos-runtime`:
  the WK spec runs in 05's `macos-wk` on `macos-15` and `macos-26` (§3 item 4), which already covers WebKit on 15, so
  01 §3.2's WM5 row and intro now name `macos-wk` instead.
- ux-2 (01): applied. 05 M-MAC-14's Dock icon steps.
- ux-3 (01): applied. 03 §13.2 states the idle-read interface (≤ 1 s, preflight only, no tap); 03 §14.1 adds
  `PermissionState`; MA45 touches `internal/desktop/audio` for it.
- ux-5 (01): applied. 02 EM8 records the centered-window pick on 14.4, 15.x and 27; 05 M-MAC-5's step.
- media-audio-2 (02): applied. `queueDepth` 8 in §1; `frame_slots_soak` in MA19 (and its ProMotion bound in MA34).
- media-audio-7 (02): applied. MA19's `session_fake_test` resize case; MA34 adds `frame_info` and the 60 s Retina run.
- scope-6 (02): applied. MA19 lists `lockmain_darwin_test.go`.
- media-audio-3 (03): applied. 05 A-1's mic-case list and the VM microphone row include 13v; V1 records 13v's rows.
- media-audio-8 (03): applied. 05 A-1's pass-rule text; V1 records `ioBufferMs` and case 15's cuts; README §1.
- ux-10 (03): applied. 01 §9.3 shows "Checking…" (`desktop.setup.audioPermission.checking`) for `unknown`, with a
  Vitest case in MD11.

**Applied by the doc editors in their own docs**: 01 security-3, security-5, ux-2, ux-3, ux-5, ux-8, ux-9; 02
media-audio-1, media-audio-2, media-audio-7, scope-6; 03 media-audio-3, media-audio-5, media-audio-6, media-audio-8,
ux-10; 04 security-2, security-4, ux-1, ux-6, ux-7, scope-9; 05 security-1, media-audio-4, ux-4, scope-5, scope-3.
