# M3 design 04: macOS build, signing, packaging, install and updates

Part of the M3 ("macOS app") design set. Source of truth: `docs/PLAN.md`. Sibling docs, written in parallel:
`01-desktop-macos.md` (the app on macOS: the darwin OS layer, window, tray, wizard, permissions, deep links, settings,
Keychain), `02-macos-engine.md` (the static cgo + Objective-C engine behind C ABI v1: picker, ScreenCaptureKit,
VideoToolbox), `03-macos-audio.md` (the process tap, exclusion, mic detection, the macOS audiotest),
`05-exit-and-verification.md` (the exit run, the OS matrix 14.4 → 27, the manual `M-MAC-*` rows, the optional
self-hosted Mac runner). M2's record is `docs/m2/README.md` and its specs; `docs/m2/06-windows-release.md` ("M2 06"
below) is the document this one extends, cited by section ("M2 06 §8.7"). M1's pipeline is `docs/m1/06-deploy-and-ci.md`
("M1 06").

**Scope marker.** Everything here is **M3** unless marked **later (M4/M5)** or **pending** (the paid Apple Developer
Program, PLAN "Pending").

**Covers:** the `.app` bundle (Info.plist, entitlements, hardened runtime, universal binary, version numbers); the
self-signed code-signing identity (creation, storage, CI keychains, designated requirement, why TCC grants and Keychain
ACLs survive updates, rotation and leak); Gatekeeper and quarantine on each install path; the `.tar.gz` and the DMG;
`install-desktop.sh` and the server route that serves it pre-filled; the Homebrew tap; uninstall and purge; the macOS
`THIRD_PARTY_NOTICES`; the updater's macOS half (manifest entry, bundle swap through a helper, code-signature check);
the CI, nightly and release jobs for macOS; the `/download` page, `release.json` and the docs pages; the owner's
actions.

**Does not cover:** what the app does at run time (window, tray, wizard, permission prompts, deep-link handling,
settings and the data root: `01`), the engine and its static library (`02`), the audio tap and the audiotest harness
(`03`), the exit measurements and the macOS version matrix run book (`05`). §16 lists exactly what this document
takes from them.

## 1. Key decisions

| # | Decision | Why |
|---|---|---|
| D1 | **One universal `.app`** (arm64 + x86_64 in one Mach-O, `lipo`), `LSMinimumSystemVersion` 14.4, deployment target 14.4 for every C/ObjC/Go object; runtime `@available` gating is 02's | The plan's Engines-per-OS row ("one universal binary … Intel slice kept"). macOS 26 is the last release for Intel Macs, so Intel owners are on 14.4–26 and still need the app; the slice costs one extra cgo build (both build on the arm64 runner by cross-compiling) and about 30 MB. The Intel slice is **best effort**: CI runs it (§9), nobody on the team owns an Intel Mac, the docs say so (§11) |
| D2 | **Signed with one self-signed code-signing identity** `isshoni-codesign-1` (RSA-3072, 30 years), with the **explicit** designated requirement `identifier "io.isshoni.desktop" and certificate leaf = H"<SHA-1>"`; no notarization, no timestamp | PLAN "Install and distribution: macOS". S2 showed the mechanism on macOS 27: builds 1→9 with new CDHashes kept the System Audio Recording and Local Network grants because the requirement stayed the same. RSA, because `security import` crashed on EC keys (S2 `dev-signing.sh`). The leaf form (not `root`) pins this exact key, so a later identity change re-prompts once and takes the grants away from the old key (§5.8) |
| D3 | **An offline backup identity** `isshoni-codesign-backup-1`, created with the first one; its certificate is committed and the **updater** accepts a bundle signed by either | Mirrors the update keys' active/backup pair (M2 06 D3). After a leak of the active key, the next release is signed with the backup and still reaches every app through the updater (one TCC re-prompt), instead of needing a manual reinstall on every Mac |
| D4 | **Hardened runtime on**, one entitlement `com.apple.security.device.audio-input`; no App Sandbox, no `com.apple.developer.*`, no `cs.*` exceptions, no `get-task-allow` in releases | PLAN. The engine and its libraries are linked statically and the rest is Apple's frameworks, so library validation (which rejects any non-Apple dylib under a self-signed identity, since it has no Team ID) blocks nothing of ours and does block `DYLD_INSERT_LIBRARIES` injection into a process that holds capture grants. S2 needed `audio-input` for the tap's aggregate device under the hardened runtime |
| D5 | **No `keychain-access-groups` and no data-protection keychain**: 01's Keychain store uses the file-based login keychain, whose item ACLs trust the creating app by its designated requirement | `keychain-access-groups` is a restricted entitlement that needs a provisioning profile (paid account); without it AMFI kills the app at launch. That a re-signed N+1 still reads the items without a prompt is **expected; decided by 01's KC check (`tools/kccheck` in `desktop-macos`, 01 §13.3) before any release**; the partition-list behavior is undocumented (01 §13.2). If KC fails, 01 switches darwin to a 0600 `credentials.json` and this decision's Keychain part reads n/a |
| D6 | **The release asset that updates, installs and feeds Homebrew is `isshoni-desktop_<v>_darwin_universal.tar.gz`** (the signed `.app`, written deterministically by Go); the **DMG** is only for people who download in a browser | M2 06 §2.1 reserved the name. A tarball is extractable by Go (updater), `/usr/bin/tar` (script) and Homebrew without mounting anything; Go's writer adds no AppleDouble `._` files or extended attributes, and a bundle's signature lives in the Mach-O and `_CodeSignature/`, not in xattrs |
| D7 | **`install-desktop.sh` is the primary install**: POSIX sh in `main()`, served pre-filled by every server, verifies `checksums.txt.sig` with macOS's own `/usr/bin/ssh-keygen` (same key, principal and namespace as `install.sh` and the ps1) plus `codesign -R` against the committed requirement, installs to `/Applications` when the user can write there without `sudo`, else `~/Applications`, never uses `sudo`, launches through `open` | PLAN: curl sets no quarantine flag, so Gatekeeper never checks the app and there is no dialog. One trust root for every installer (M2 06 D11). Never `sudo`: a root-owned bundle could not be updated by the app. `open` makes the app its own TCC "responsible process" (started from a shell, Terminal would be asked for the grants instead) |
| D8 | **The updater swaps the bundle with `renamex_np(RENAME_SWAP)`** from a one-shot helper (`isshoni --apply-update=<request>`, the old app's own executable) that waits for the app to exit, then relaunches with `open -a <bundle> --args --updated-from=<old>` | PLAN ("swaps the .app atomically while not running; a helper relaunch"). One rename call is atomic on APFS and HFS+; the helper is code the user already trusts, never the downloaded one; `open` keeps TCC attribution right (D7) |
| D9 | **Before the swap the staged bundle must pass `/usr/bin/codesign --verify --strict -R=<requirement>`**, its `CFBundleIdentifier` must be `io.isshoni.desktop`, and its `--version` must equal the manifest's version | PLAN. The manifest's Ed25519 signature stays the root of trust (M2 06 §8.1); the requirement check is defence in depth and guarantees that what is installed keeps the grants (D2) |
| D10 | **Manifest entry**: `{"os": "darwin", "arch": "universal", "kind": "darwin-app", "name": "isshoni-desktop_<v>_darwin_universal.tar.gz", …}`; a darwin app selects its own `GOARCH` entry first, else `universal` | M2's parser already accepts this entry and never selects it on Windows (`tokenRE` passes `universal` and `darwin-app`; an unknown kind is checked, not selected, M2 06 §8.2), so released Windows apps keep updating. Per-arch entries stay possible if the Intel slice is ever dropped (D1) |
| D11 | **Updates swap in place only where the user can write**: a bundle that is translocated, on a read-only volume (a mounted DMG), not named `isshoni.app` or whose folder isn't writable gives `not_installed` with a reason | M2 06 §8.7's "only real installs update", in macOS terms. 01 turns the reasons into "Move isshoni to Applications" or "Reinstall for your account" |
| D12 | **The macOS jobs that run third-party build code hold no secret**; a separate `macos-sign` job (environment `release`, `contents: read`) runs only Apple's tools and one reviewed script `deploy/macos/sign.sh` over a bundle it receives as data | M1 06 §9.3 / M2 06 D16: no secret next to Go modules, Wails' cgo, vcpkg ports or CMake |
| D13 | **Every PR that builds the app signs it with an ephemeral identity** made in the job (`deploy/macos/ci-identity.sh`), and runs the same checks as a release | The signing, requirement and packaging steps are exercised on every change without any secret on a PR runner |
| D14 | **Build host: `macos-26` (Apple silicon) with an exact Xcode** from `deploy/macos/tools.json`; run-time legs on `macos-14`, `macos-15` and `macos-15-intel`; no TCC-dependent test on hosted runners | The engine needs the macOS 26 SDK for the 26-only tap properties (02, 03); hosted macOS runners have no Screen Recording or System Audio Recording grants and can't get them, so capture and tap tests run on the owner's Mac (§9.5, 05). **Deviates from PLAN's "macos-15 (cgo)" native job**: the macOS 26 SDK is required (02/03); written back to PLAN "Build, CI and release" in MR1 and listed in §18 for the owner. The fallback stays `macos-15` with Xcode 26 selected (§3) |
| D15 | **The Homebrew tap is `MoonWX/homebrew-isshoni`** with one cask `isshoni` (`brew install --cask moonwx/isshoni/isshoni`), updated by `release.yml` for final releases only, through a deploy key scoped to that repository | PLAN "an own Homebrew tap (postflight strips quarantine)". A deploy key can write to one repository only, so a leak touches nothing else; rc releases stay out because Homebrew has no beta channel |
| D16 | **Bundle id `io.isshoni.desktop`** for releases (M2's AUMID); dev builds are `io.isshoni.desktop.dev` ("isshoni Dev") with the dev identity, e2e builds `io.isshoni.desktop.e2e` ("isshoni E2E", 01), neither declaring a URL scheme; test-tag builds (`updatetest`) keep the release id but run only on CI runners (the scripts that use them refuse elsewhere, §9.1) | TCC and Keychain key by bundle id plus requirement: a dev build with the release id but another certificate would overwrite the release app's grants on the owner's Mac and make it re-prompt |
| D17 | **No FFmpeg on macOS**, so no LGPL bundle and no source tarball for the macOS assets; libopus and speexdsp (BSD-3-Clause, merged into 02's static archive) and whatever else 02/03 link statically are listed in `deploy/notices/native-darwin.json` | VideoToolbox encodes (PLAN Engines per OS). BSD-3 asks for the notice in binary distributions: `Contents/Resources/licenses/THIRD_PARTY_NOTICES.txt` |
| D18 | **Every `v*` tag from the first M3 release on ships the macOS assets**; `release.yml` fails without them | M2 06 D14's rule: every server's `/download` links are deterministic URLs |
| D19 | **Version numbers in Info.plist**: `CFBundleShortVersionString` = `X.Y.Z`, `CFBundleVersion` = `X.Y.(Z·100000 + W)` with M2's W rule, plus the custom keys `IsshoniVersion` (full SemVer) and `IsshoniCommit` | Apple documents both as one to three integers; this form is monotonic across rc → final (§4.5) and the full SemVer stays readable without executing the binary |

## 2. Artifacts, names and the installed layout

### 2.1 Release assets added in M3

`<v>` is the version without `v`. Names are an interface (the `/download` page, the script, the updater, the cask and
the site build URLs from them).

| Asset | Contents | In `checksums.txt` |
|---|---|---|
| `isshoni-desktop_<v>_darwin_universal.tar.gz` | the signed `isshoni.app` (§6.1) | yes |
| `isshoni-desktop_<v>_darwin_universal.dmg` | the same signed bundle, an `Applications` link and a help link (§6.2) | yes |
| `isshoni-desktop_<v>_darwin_universal_symbols.zip` | the two unstripped thin executables `isshoni-arm64`, `isshoni-x86_64` (same `LC_UUID`s as the shipped slices, for `atos`), 02's per-arch `build.json` renamed `native-manifest-darwin-arm64.json` / `native-manifest-darwin-amd64.json` and `archive.sha256` renamed `archive-darwin-arm64.sha256` / `archive-darwin-amd64.sha256` (from `native/dist/darwin-{arm64,amd64}/`), `build-info.json` (§4.6) | yes |
| `isshoni-desktop_<v>_darwin_universal.sbom.json` | SPDX JSON SBOM of the stage (syft, §10.1) | yes |
| `install-desktop.sh` | the generic script, stamped with `<v>` and an empty server (§6.3) | yes |
| `isshoni-desktop-update.json` (changed) | gains the darwin entry (§7.1) | yes (as M2) |

All but signatures get build-provenance attestations (M2 06 §2.1's rule). Not release assets: the workflow artifact
`isshoni-exit-tools-macos` (§10.1, job `macos-exit-tools`; 05 §4.5's layout: `isshoni Exit Harness.app` signed with the
test-tools identity `isshoni-testtools-1` (§5.1), `harness.sh`, `SHA256SUMS`, `exit-tools-manifest.json`, `README.txt`).

### 2.2 The bundle

```
isshoni.app/
  Contents/
    Info.plist                       §4.1 (XML, generated from the template)
    PkgInfo                          "APPL????"
    MacOS/isshoni                    universal Mach-O (arm64, x86_64), hardened runtime, signed (§5)
    Resources/AppIcon.icns           cmd/isshoni-desktop/build/darwin/AppIcon.icns
    Resources/en.lproj/InfoPlist.strings   the three usage strings (i18n-ready, §4.1)
    Resources/licenses/LICENSE.txt         the repository's LICENSE (Apache-2.0)
    Resources/licenses/NOTICE.txt          the repository's NOTICE
    Resources/licenses/THIRD_PARTY_NOTICES.txt   §8
    _CodeSignature/CodeResources     written by codesign
```

Nothing else: no frameworks, dylibs, helpers, XPC services or login-item bundles (01's autostart is
`SMAppService.mainApp`, which needs none). `desktoppkg check -os darwin` fails on any other Mach-O in the bundle (§8.1).

Optional, only if 01's WM25 fails (01 §12.3): `Contents/Library/LaunchAgents/io.isshoni.desktop.autostart.plist`,
copied from `cmd/isshoni-desktop/build/darwin/` by `desktoppkg bundle` when that file exists (a plist, not code; the
bundle golden covers both cases, and `machocheck`'s "no other Mach-O" rule is unaffected).

### 2.3 Locations on a Mac

| Path | Holds | Owner |
|---|---|---|
| `/Applications/isshoni.app` or `~/Applications/isshoni.app` | the bundle (§6.3's rule) | this document |
| 01's data root (assumed `~/Library/Application Support/isshoni/`) | `settings.json`, `servers.json`, logs, … | 01, 02 |
| `<data root>/updates/` (0700) | the downloaded archive, `state.json` (M2 06 §8.7's format), `apply-<v>.json` (§7.3), `applying.lock`, `helper.log` | this document |
| `<bundle's folder>/.isshoni-update-<v>-<rand>/` | the staged bundle during an apply; removed by the helper and by `update.CleanupStaging` at start | this document |
| login keychain, items of 01/02's Keychain store | refresh tokens (ACL: the app's DR, D5) | 01, 02 |
| `~/Library/Preferences/io.isshoni.desktop.plist` | `UpdateChecks` (bool, written only by `install-desktop.sh`, read once by 01 as the initial `updates.check`, like M2's `HKCU\…\UpdateChecks`) and whatever Wails/AppKit keep | 01 reads; script writes |
| `/Library/Managed Preferences/io.isshoni.desktop.plist` (MDM) | `DisableUpdateChecks` (forced bool) | read only (§7.4) |
| `~/Library/{Caches,WebKit,HTTPStorages}/io.isshoni.desktop`, `~/Library/Saved Application State/io.isshoni.desktop.savedState` | WKWebView and AppKit state, keyed by bundle id | the OS; removed by a purge (§6.7) |

### 2.4 Process contract on macOS

M2 06 §2.4's flags keep their meaning (`--version`, `--self-check=<path>`, `--updated-from=<v>`,
`--forget-credentials`, `--purge-data`, `--update-now` in `updatetest` builds). Differences and additions:

| Item | Owner | Contract |
|---|---|---|
| `isshoni --version` | 01 | as M2 (one JSON line, exit 0, no window); runs from a shell without TCC prompts; on macOS also `"arch": "arm64"\|"amd64"` (the running slice) |
| `isshoni --self-check=<path>` | 01 with 02 | as M2 minus the FFmpeg fields: `ok` requires `engine.loaded`, `abi` 1, `testHooks` false and `spa.ok`; **touches no TCC-protected API and requires no hardware encoder** (hosted runners are VMs without grants); adds `"bundle": {"id": "<CFBundleIdentifier>", "path": "<bundle>"}` so CI can assert the release id, plus `menu`: the app menu's item titles (01), used by 05's WK24; building that list touches no TCC API |
| `isshoni --apply-update=<request.json>` | this document (`update.RunApplyHelper`); 01 dispatches it in its one-shot table | darwin only; takes exactly one argument, honors no other flag, opens no window, never builds the app or takes 01's single-instance lock (like `--self-check`); started only by `update.Apply` through `posix_spawn` (`exec.Cmd` with `Setsid`), never by LaunchServices |
| `<data root>/updates/applying.lock` | the helper holds `flock(LOCK_EX)` on it for its whole run | `Apply` returns once it is held (≤ 10 s); **01's start step** (the macOS twin of M2 03 §13.4 step 2a) tries `flock(LOCK_EX\|LOCK_NB)` before single instance: when it is held, every start exits 0 within 10 s without becoming an instance and without a window |
| Deep link at an update | 01 | a link that launches isshoni while `applying.lock` is held is dropped with one INFO line (its URL Apple event arrives only after `NSApp` runs, 01 §6.3, so the exiting start never sees it); the user clicks it again after the relaunch. A link already known to a running app that applies an update (Restart now, or Apply at Quit, §7.3) is kept in 01's pending-link store; the relaunch carries only `--updated-from` |
| SIGTERM | 01 | quits like the menu's Quit (stops a share, closes connections). `install-desktop.sh` never sends it (§6.3 refuses while the app runs); Homebrew's `uninstall quit:` uses Apple Events |
| Launching through `open` | installers, updater, `task desktop:run:macos` | the installers (§6.3, the cask), the update helper's relaunch (§7.3) and `task desktop:run:macos` always start the app through LaunchServices (`open`), so the app is its own TCC responsible process (D7). Executing `Contents/MacOS/isshoni` directly is allowed only where no TCC API is touched: `--version`, `--self-check`, and the e2e/CI flavors on hosted runners (05 §5.3, §6.3) |

## 3. Toolchains and pins

| What | Pin (as of 2026-11-06) | Where | Bumped |
|---|---|---|---|
| Build runner | `macos-26` (arm64). If GitHub retires or lacks it, `macos-15` with the pinned Xcode 26 selected (that image carries Xcode 26) | `deploy/macos/tools.json` `runner`, read by a `setup` step | by hand |
| Xcode | one exact `Xcode_26.x.app` present on the image, chosen at slice MR1 | `tools.json` `xcode`; **on CI only** (`CI=true`) the step runs `sudo xcode-select -s /Applications/Xcode_<x>.app`, printing the Xcodes it found when that one is missing (the `msvc-env.ps1` pattern, M2 06 §3); everywhere (CI and the Mac) it then fails unless `xcrun --sdk macosx --show-sdk-version` ≥ `sdkMin` (26.0). Every macOS job leg that compiles (the build jobs and the `testRunners` legs) runs this same step | monthly tool PR |
| Deployment target | `14.4` | `tools.json` `deploymentTarget`; exported as `MACOSX_DEPLOYMENT_TARGET` and `-mmacosx-version-min=14.4` in `CGO_CFLAGS`/`CGO_LDFLAGS` (S2 `build.sh`: Go's own `runtime/cgo` objects need it too) | never lowered below 14.4 (PLAN) |
| Run-time legs | `macos-14` (arm64, macOS 14: where weak linking breaks first) while GitHub offers it, `macos-15` (arm64, macOS 15), `macos-15-intel` (x86_64) while GitHub offers it; after `macos-15-intel`, the x86_64 slice runs under Rosetta on `macos-26`; after `macos-14`, its check moves to 05's 14.4 VM run at minor releases (05 §9.6) (§9.1) | `tools.json` `runtimeRunners` | when GitHub retires one |
| Test legs (05's jobs) | `macos-15`, `macos-26`, `macos-15-intel`, `macos-14` | `tools.json` `testRunners`, checked against 05's `macos-native` and `macos-compat` | when GitHub retires one |
| Go, Wails | M2's (`go.mod`; Wails `v3.0.0-beta.34` with its CLI and `@wailsio/runtime`, M2 06 D19) | unchanged | M2's hand-bump rule; the PR template's Wails checklist gains "the macOS app builds and `desktop-macos` passes" |
| vcpkg (libopus, speexdsp for macOS) | 02's overlay triplets `arm64-osx-isshoni` / `x64-osx-isshoni` (`native/triplets/`, static, deployment target 14.4) at `native/vcpkg.json`'s baseline | 02 | 02 |
| Homebrew (for `brew style` only) | the runner image's | none | — |
| relserve, syft, cosign | M1/M2's | unchanged | — |

`deploy/macos/tools.json`:

```json
{
  "runner": "macos-26",
  "xcode": "26.1",
  "sdkMin": "26.0",
  "deploymentTarget": "14.4",
  "archs": ["arm64", "x86_64"],
  "runtimeRunners": ["macos-14", "macos-15", "macos-15-intel"],
  "testRunners": ["macos-15", "macos-26", "macos-15-intel", "macos-14"]
}
```

`task lint:pins` (extended): `tools.json` parses; `deploymentTarget` (14.4) equals `LSMinimumSystemVersion` in the
Info.plist template (§4.1) and each of 02's three settings: every `mac-*` preset's `CMAKE_OSX_DEPLOYMENT_TARGET` in
`native/CMakePresets.json`, `VCPKG_OSX_DEPLOYMENT_TARGET` in both overlay triplets
`native/triplets/{arm64,x64}-osx-isshoni.cmake`, and the `MACOSX_DEPLOYMENT_TARGET` of the Taskfile's darwin cgo builds
(`native:mac`, `desktop:app:macos`). `runs-on` equals `runner` for the jobs that produce shipped or packaged bundles
(`desktop-macos`, `macos-update`, `macos-app`, `macos-sign`, `macos-pkg`, `macos-exit-tools`); the legs of 05's
`macos-native` and `macos-compat` must each be in `testRunners`, and `macos-runtime`'s in `runtimeRunners`. If
`macos-15-intel` carries no Xcode ≥ 26, its `macos-native` leg builds on `macos-26` and only runs the x86_64 test
binaries there (05 V-D10's fallback).

**On the owner's Mac** (macOS 27, Apple silicon): any Xcode ≥ 26 or only the Command Line Tools with a ≥ 26 SDK (02
§6.3: full Xcode is not required; no `xcode-select` is run locally); the same tasks build both slices (§4.6). The
release identity never comes to the Mac's keychain; local builds use the dev identity (§5.7).

## 4. The app bundle

### 4.1 Info.plist (`cmd/isshoni-desktop/build/darwin/Info.plist.tmpl`)

`tools/desktoppkg bundle` fills the `@…@` fields, then checks the result: well-formed XML plist, every key below
present exactly once, no other key, values as listed; on macOS also `plutil -lint`. **01 owns the wording of the usage
strings** (01's Info.plist table); this document owns the template and the golden test, and 03 points here for the
key and to 01 for the text.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>@BUNDLE_ID@</string>            <!-- io.isshoni.desktop; dev: …desktop.dev; e2e: …desktop.e2e -->
  <key>CFBundleName</key><string>@BUNDLE_NAME@</string>                <!-- isshoni; dev: isshoni Dev; e2e: isshoni E2E -->
  <key>CFBundleDisplayName</key><string>@BUNDLE_NAME@</string>
  <key>CFBundleExecutable</key><string>isshoni</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleSignature</key><string>????</string>
  <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
  <key>CFBundleDevelopmentRegion</key><string>en</string>
  <key>CFBundleLocalizations</key><array><string>en</string></array>
  <key>CFBundleShortVersionString</key><string>@SHORT_VERSION@</string> <!-- X.Y.Z -->
  <key>CFBundleVersion</key><string>@BUNDLE_VERSION@</string>          <!-- §4.5 -->
  <key>IsshoniVersion</key><string>@VERSION@</string>                  <!-- full SemVer, read by install-desktop.sh -->
  <key>IsshoniCommit</key><string>@COMMIT@</string>
  <key>LSMinimumSystemVersion</key><string>14.4</string>
  <key>LSArchitecturePriority</key><array><string>arm64</string><string>x86_64</string></array>
  <key>LSApplicationCategoryType</key><string>public.app-category.social-networking</string>
  <key>LSUIElement</key><true/>                                        <!-- 01 D4 -->
  <key>NSQuitAlwaysKeepsWindows</key><false/>                          <!-- 01 §4.4 -->
  <key>NSHighResolutionCapable</key><true/>
  <key>NSSupportsAutomaticGraphicsSwitching</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
  <key>NSHumanReadableCopyright</key><string>Copyright the isshoni authors. Apache-2.0.</string>
  <key>NSAudioCaptureUsageDescription</key>
  <string>isshoni shares your Mac's sound with friends, without your voice apps.</string>
  <key>NSScreenCaptureUsageDescription</key>
  <string>isshoni shows friends the window or screen you pick, only while you share.</string>
  <key>NSLocalNetworkUsageDescription</key>
  <string>isshoni connects to isshoni servers and friends on your local network.</string>
  <!-- @URL_TYPES_BEGIN@ (emitted only for TAGS=production; -scheme "" omits the whole key) -->
  <key>CFBundleURLTypes</key>
  <array>
    <dict>
      <key>CFBundleURLName</key><string>io.isshoni.desktop.link</string>
      <key>CFBundleTypeRole</key><string>Viewer</string>
      <key>CFBundleURLSchemes</key><array><string>@URL_SCHEME@</string></array> <!-- isshoni -->
    </dict>
  </array>
  <!-- @URL_TYPES_END@ -->
  <!-- dev and e2e only: NSAppTransportSecurity { NSAllowsLocalNetworking = true } (§4.8) -->
</dict>
</plist>
```

Decisions behind the keys:
- **`NSAudioCaptureUsageDescription`** (process taps, 14.2+): required; without it the tap fails without a prompt
  (S2's bundle had it). **`NSLocalNetworkUsageDescription`**: macOS 15+ asks before LAN connections and LAN ICE
  candidates (S4, S2 `lancheck`). **`NSScreenCaptureUsageDescription`**: the release app never requests Screen
  Recording (01 §9.1, 02 §8.1, §5.4: the picker is the consent and needs no grant, the engine never calls
  `CGRequestScreenCaptureAccess`, and the self-lookup `getCurrentProcessShareableContent`, 14.4+, needs no TCC); the
  key is kept only as a harmless string, so the text exists if macOS ever shows the prompt. Screen Recording is used
  only by `IsshoniEngineTest.app`, the harness apps and the `isshoni_hw` tests on the owner's Mac (02 §17.3, 05 §4.1).
- **`LSUIElement`** = true and **`NSQuitAlwaysKeepsWindows`** = false: 01 D4 (the activation policy follows the
  window, so an `--autostart` or login-item start never flashes a Dock icon) and 01 §4.4 (no window restoration).
- **Not set**: `NSMicrophoneUsageDescription` (isshoni never opens a microphone; mic *detection* reads process
  properties, 03), `NSBonjourServices` (isshoni neither browses nor advertises Bonjour; if 01 finds WKWebView's WebRTC
  mDNS host candidates need it on 15+, 01 adds `_isshoni._udp` in its slice with M-MAC-16's evidence),
  **`LSMultipleInstancesProhibited`** (it blocks a second *user* on the same Mac, which M2's M-DESK-15 allows; single
  instance is 01's lock), `NSAppTransportSecurity` (release apps talk https only, M2 02 §12; the dev and e2e bundles add
  `NSAllowsLocalNetworking` for `http://localhost`, §4.8), `ITSAppUsesNonExemptEncryption` (App Store only).
- **`CFBundleURLTypes`**: the `isshoni://` scheme of M2 02 §12; on macOS the link arrives as a `kAEGetURL` Apple Event
  (01), not as `--open-url`. **Production only**: `desktoppkg bundle -scheme ""` (dev and e2e) omits the whole key, so a
  developer's or test build never takes links from the installed app (01 §4.4, §6.3); the golden covers both cases.
- **`InfoPlist.strings`** in `Resources/en.lproj/` repeats the three usage strings (`"NSAudioCaptureUsageDescription" =
  "…";`, written as UTF-8 with LF, which `plutil -lint` accepts): a translation later adds `<lang>.lproj/` without
  touching the template.

### 4.2 Entitlements (`cmd/isshoni-desktop/build/darwin/isshoni.entitlements`)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>com.apple.security.device.audio-input</key><true/>
</dict></plist>
```

`isshoni-dev.entitlements` adds `com.apple.security.get-task-allow` (lldb) and is used only by `SIGN=dev` (§4.8).
Every other key is refused by the post-sign check (§5.5). What a self-signed identity may and may not use:

| Entitlement | Allowed self-signed? | isshoni |
|---|---|---|
| hardened runtime flag (`--options runtime`) | yes | on |
| `com.apple.security.device.audio-input`, `…camera`, `…automation.apple-events`, `cs.allow-jit`, `cs.disable-library-validation`, … (unrestricted) | yes | only `audio-input` |
| `com.apple.security.app-sandbox` | yes | no (PLAN: no sandbox; taps, picker filters and process inspection need it off) |
| `keychain-access-groups`, `com.apple.application-identifier`, `com.apple.developer.*` (restricted) | **no**: AMFI kills the app without a matching provisioning profile | never (D5) |

### 4.3 Hardened runtime and library validation

- Library validation is implied by the hardened runtime and accepts only Apple-signed code or code with the same Team
  ID; a self-signed identity has **no Team ID** (`TeamIdentifier=not set`), so isshoni could not load a dylib of its
  own. It has none: Go links statically, 02 links the engine, libopus and speexdsp statically, and Wails' darwin backend
  is cgo in the same binary. `desktoppkg machocheck` (§4.7) enforces it: every `LC_LOAD_DYLIB` is under `/usr/lib/` or
  `/System/Library/`, no `LC_RPATH`, no `@rpath`/`@loader_path`/`@executable_path` loads.
- WKWebView's JIT runs in Apple's WebContent process, not in isshoni, so `cs.allow-jit` is not needed; Go needs no
  writable-executable memory on darwin.

### 4.4 Icon

`cmd/isshoni-desktop/build/darwin/AppIcon.icns` is committed (made once with `iconutil -c icns` from an
`AppIcon.iconset` of the master artwork; the iconset is not kept). It is not generated at build time, so the bundle is
the same on any host. The Windows `icon.ico` and the tray icons (M2) stay as they are; 01 owns the macOS menu-bar
template images.

### 4.5 Version numbers (`desktoppkg macver`)

| SemVer | `CFBundleShortVersionString` | `CFBundleVersion` |
|---|---|---|
| `0.3.0` | `0.3.0` | `0.3.65000` |
| `0.3.0-rc.2` | `0.3.0` | `0.3.2` |
| `0.3.1` | `0.3.1` | `0.3.165000` |
| `0.0.1-ci.1234` | `0.0.1` | `0.0.101234` |
| `0.0.0-dev+1a2b3c` | `0.0.0` | `0.0.0` |

`CFBundleVersion` = `X.Y.(Z·100000 + W)`, where W is M2's winver rule (65000 for a final, N mod 65000 for a prerelease
ending in a number, 0 otherwise, M2 06 §5.3). X, Y > 65535 or Z > 21000 is an error (keeps the third number in 31
bits). Nothing in isshoni compares these numbers; they keep LaunchServices' "newest copy" choice right when two copies
exist and show in Finder's Get Info. The updater and the script compare `IsshoniVersion` / `--version` with
`internal/version` (M2 06 §8.5).

### 4.6 Building the universal app (`task desktop:app:macos`)

`task desktop:app:macos VERSION=<v> [TAGS=production,imstatic] [SIGN=dev|ci|none] [OUT=dist-desktop/darwin_universal]`.
`TAGS` other than `production,imstatic` write to `dist-desktop/darwin_universal-test/`, which `stage -release` refuses
(M2 06 §5.2's rule). `imstatic` selects the real engine (02 §6.4; without it darwin compiles `engine_other.go`,
`unsupported_os`). `imtesthooks` is refused in any stage that `stage -release` accepts (the task fails when `TAGS`
contains it and `OUT` isn't the `-test` folder; `machocheck -native` also requires `testHooks` false). Steps (one shell
task, the same on the Mac and in CI):

1. **Xcode check** (§3: `xcode-select -s` only when `CI=true`; locally only `xcrun --sdk macosx --show-sdk-version` ≥
   `sdkMin`) and `web/dist` present (built by the `web` job or `task web:build`).
2. **Native, both arches in one call** (02 §6.5), only when `TAGS` contains `imstatic`:
   `task native:mac PRESETS="mac-arm64-release mac-x64-release" VERSION=<v> COMMIT=<sha>`, which writes
   `native/dist/darwin-arm64/` and `native/dist/darwin-amd64/`, each with `libisshoni_media.a`, `archive.sha256` and
   `build.json`. No `cgo.env`: 02 §6.4's `link_darwin_{arm64,amd64}.go` carry the archive path and the frameworks in
   `#cgo LDFLAGS`, so adding flags here would link the archive twice.
3. **Go per arch** (`A` = `arm64` / `x86_64`, `GOARCH` = `arm64` / `amd64`):

   ```sh
   CGO_ENABLED=1 GOOS=darwin GOARCH=$GOARCH MACOSX_DEPLOYMENT_TARGET=14.4 \
   CC="xcrun --sdk macosx clang -arch $A" CXX="xcrun --sdk macosx clang++ -arch $A" \
   CGO_CFLAGS="-mmacosx-version-min=14.4" CGO_LDFLAGS="-mmacosx-version-min=14.4" \
   go build -trimpath -tags "$TAGS" -ldflags "-X github.com/MoonWX/isshoni/internal/version.version=<v> \
     -X …version.commit=<sha> -X …version.date=<commit date> \
     -X github.com/MoonWX/isshoni/internal/media/native.staticArchiveSHA=$(cat native/dist/darwin-$GOARCH/archive.sha256)" \
     -o dist-desktop/.build/darwin/isshoni-$A ./cmd/isshoni-desktop
   ```

   The `staticArchiveSHA` flag (02 §6.6) changes the link's action ID, because Go's build cache doesn't hash the archive
   named in `#cgo LDFLAGS`; it is omitted only for builds without `imstatic`. No `-s -w`: the unstripped thin binaries
   go into the symbols zip (§2.1). `go version -m` and the symbol table of each thin binary are checked (step 6).
4. **Strip and merge**: `strip -S -x` on copies (keeps `LC_UUID`; Go's stack traces use `pclntab`, not the symbol
   table), then `lipo -create -output <stage>/isshoni.app/Contents/MacOS/isshoni isshoni-arm64.stripped
   isshoni-x86_64.stripped`. If a stripped binary fails step 6's `--version` run, the slice that finds it ships
   unstripped slices and records it (size only).
5. **Bundle**: `go -C tools run ./desktoppkg bundle -app <stage>/isshoni.app -version <v> -commit <sha> -bundle-id
   io.isshoni.desktop -name isshoni -scheme isshoni -plist cmd/isshoni-desktop/build/darwin/Info.plist.tmpl -icon
   …/AppIcon.icns -licenses <dir>` writes `Info.plist`, `PkgInfo`, `Resources/` (licenses from §8), sets modes (dirs
   0755, the executable 0755, the rest 0644) and removes any `.DS_Store`.
6. **Checks** (`desktoppkg machocheck`, §4.7) before signing.
7. **Sign** per `SIGN`: `none` (CI's unprivileged release job), `ci` (an ephemeral identity, §5.6), `dev` (§5.7).
8. `build-info.json` next to the bundle: version, commit, the toolchain (`xcodebuild -version` when it succeeds; with
   only the Command Line Tools `"xcode": "CommandLineTools"` plus `clang --version`'s first line), the SDK version
   (`xcrun --show-sdk-version`), each slice's `LC_UUID`, minos and SDK; it goes into the symbols zip.

`task desktop:package:macos APP=<signed .app> VERSION=<v>` then writes the `.tar.gz` (§6.1) and the DMG (§6.2);
`task desktop:run:macos` runs `open <bundle>` (never the executable directly: D7).

### 4.7 `desktoppkg machocheck`

`go -C tools run ./desktoppkg machocheck -app <bundle> -version <v> [-thin <dir>] [-native <dir>] [-requirement
<text>]`; reads Mach-O with `debug/macho` (including `FatFile`) plus its own load-command parser for `LC_BUILD_VERSION`,
so it runs on Linux too. Every problem is reported; exit 1 if any:
- `Contents/MacOS/isshoni` is a fat file with exactly `arm64` and `x86_64`; each slice `MH_EXECUTE`, `MH_PIE`,
  `LC_BUILD_VERSION` platform macOS with `minos` 14.4 and `sdk` ≥ `tools.json` `sdkMin`; `LC_UUID` present and
  different per slice.
- Loads only from `/usr/lib/` and `/System/Library/`, no `LC_RPATH` (§4.3).
- With `-thin`: each thin binary's `go version -m` shows `-trimpath`, `CGO_ENABLED=1`, `GOOS=darwin`, the right
  `GOARCH`, the version ldflag, tags exactly `production,imstatic` (`production,imstatic,updatetest` is accepted only
  for the `-test` output folder); neither `ISSHONI_UPDATE_TEST` nor `localhost:5173` in the binary (M2 06 §5.2 step
  3's rule). Two engine checks per thin binary, so a binary without the engine fails before signing: (a) `go version
  -m`'s `-ldflags` contains `staticArchiveSHA=` equal to that arch's `native/dist/darwin-<goarch>/archive.sha256`; (b)
  its symbol table (`debug/macho`) defines `_im_start` and `_im_next_packet` (02 §6.6's `nm -U` rule).
- With `-native <dir>` (`native/dist`): for each arch, `<dir>/darwin-<goarch>/build.json` has `sdk` ≥ `tools.json`
  `sdkMin` (03's 26-only tap properties need the 26 SDK), `minOS` = `deploymentTarget` and `testHooks` false. Used in
  `desktop-macos` and `macos-app`.
- With `-requirement`: the binary contains, byte for byte, the update requirement text that `internal/update`'s
  generated block holds (§5.3), so a release app trusts the identity that signs it.
- Info.plist matches §4.1 for `-version`; no other Mach-O anywhere in the bundle.
- Size: warn above 120 MB for the universal executable, fail above 250 MB.
- On macOS only (skipped elsewhere with a note): `arch -arm64 <exe> --version` (and `arch -x86_64` when Rosetta is
  installed) prints `version` = `-version` and `engineAbi` 1.

### 4.8 Dev and test variants

| Variant | `TAGS` | Bundle id / name / scheme | Signed with | Where |
|---|---|---|---|---|
| release | `production,imstatic` | `io.isshoni.desktop` / isshoni / `isshoni` | release identity (`macos-sign`) | release.yml |
| CI | `production,imstatic` | same | ephemeral (`SIGN=ci`) | ci.yml, nightly, dry run |
| updater e2e | `production,imstatic,updatetest` | same | ephemeral | ci.yml `macos-update`, nightly; output `dist-desktop/darwin_universal-test/` |
| e2e | `e2e` (M2's `flavor_e2e.go`; fake engine, 02 §6.4) | `io.isshoni.desktop.e2e` / isshoni E2E / no scheme (+ `NSAppTransportSecurity` `NSAllowsLocalNetworking`) | ephemeral (`SIGN=ci`) | ci.yml `desktop-macos`, uploaded for 05's WK spec (E17, V9); output `dist-desktop/darwin_universal-test/` (`stage -release` refuses it); `machocheck -thin` is not run on it |
| dev | `dev,imstatic` (01's dev flavor with the real engine) | `io.isshoni.desktop.dev` / isshoni Dev / no scheme (+ `NSAppTransportSecurity` `NSAllowsLocalNetworking`) | dev identity, `isshoni-dev.entitlements` | the owner's Mac |

`task desktop:dev` (M2's macOS dev task) keeps running the unbundled dev binary for quick UI work; plain `TAGS=dev`
links the fake engine and suits UI-only work. Anything that needs TCC (taps, picker) uses `task desktop:app:macos
TAGS=dev,imstatic SIGN=dev` (02 §6.4's rule: `imstatic` selects the real engine in any flavor) and `task
desktop:run:macos`, so the dev grants persist across rebuilds exactly as S2's did. The build passes `-scheme isshoni`
only for the production variants and `-scheme ""` for dev and e2e.

## 5. Code signing

### 5.1 The identity

| | Active | Backup |
|---|---|---|
| Name (key comment, file stem) | `isshoni-codesign-1` | `isshoni-codesign-backup-1` |
| Subject | `CN=isshoni code signing 1, O=isshoni project` | `CN=isshoni code signing backup 1, O=isshoni project` |
| Key | RSA-3072 | RSA-3072 |
| Validity | 10950 days (30 years) | 10950 days |
| Extensions | `basicConstraints=critical,CA:false`, `keyUsage=critical,digitalSignature`, `extendedKeyUsage=critical,codeSigning` | same |
| Private half | `.p12` in the `release` environment (`MACOS_CODESIGN_P12_BASE64`, `MACOS_CODESIGN_P12_PASSWORD`) and offline | offline only (password manager plus an encrypted copy on removable media) |
| Committed | `deploy/keys/isshoni-codesign-1.pem` (certificate only) | `deploy/keys/isshoni-codesign-backup-1.pem` |

**Creation (owner, once, on a Mac, outside the repository; `/usr/bin/openssl` is LibreSSL, whose PKCS#12 output
`security import` reads):**

```sh
umask 077; mkdir isshoni-codesign && cd isshoni-codesign
for n in 1 backup-1; do
  cn="isshoni code signing $(echo "$n" | tr - ' ')"
  /usr/bin/openssl req -x509 -newkey rsa:3072 -sha256 -nodes -keyout "isshoni-codesign-$n.key" \
    -out "isshoni-codesign-$n.pem" -days 10950 -subj "/CN=$cn/O=isshoni project" \
    -addext "basicConstraints=critical,CA:false" -addext "keyUsage=critical,digitalSignature" \
    -addext "extendedKeyUsage=critical,codeSigning"
  pass=$(/usr/bin/openssl rand -base64 30)
  /usr/bin/openssl pkcs12 -export -inkey "isshoni-codesign-$n.key" -in "isshoni-codesign-$n.pem" \
    -name "$cn" -out "isshoni-codesign-$n.p12" -passout "pass:$pass"
  printf '%s  %s\n' "isshoni-codesign-$n" "$pass"   # into the password manager with the .p12
done
```

Then (a PR): copy the two `.pem` files to `deploy/keys/`, run `task release:codesign-keys` (writes
`deploy/keys/macos_codesign`, the requirement blocks of `internal/update/signers.go` and `deploy/install-desktop.sh`,
prints both SHA-1 and SHA-256 fingerprints), add the fingerprints to the root README's "Release signing keys" table;
merge. Secrets: `MACOS_CODESIGN_P12_BASE64` = `base64 -i isshoni-codesign-1.p12`, `MACOS_CODESIGN_P12_PASSWORD` in
the `release` environment (owner approval required, protected `v*` tags; M1). Then delete the `.key` files and keep
only the encrypted `.p12`s offline. `deploy/keys/README.md` gains these steps.

**The test-tools identity `isshoni-testtools-1`** (05 V-D5) signs only 05's `isshoni Exit Harness.app` and its
nested tools and fixtures, never `io.isshoni.desktop`, so the release certificate's Authority appears on the app
alone and the exit harness's grants survive rc builds. It is made with the same commands (the loop run once with
`n=testtools-1`, file stem `isshoni-testtools-1`, subject `CN=isshoni test tools 1, O=isshoni project`), has **no
backup** (losing it costs a one-time re-grant of the exit harness, nothing a user has), and its private half lives in
its own `testtools` environment as `MACOS_TESTTOOLS_P12_BASE64` and `MACOS_TESTTOOLS_P12_PASSWORD`. Its certificate
is committed as `deploy/keys/isshoni-testtools-1.pem`; `sign.sh -mode testtools` takes the expected SHA-1 from that
file. It never appears in `macos_codesign`, the requirement blocks or the update requirement.

### 5.2 The designated requirement, and why grants survive updates

Every release bundle is signed with the explicit requirement

```
designated => identifier "io.isshoni.desktop" and certificate leaf = H"<SHA-1 of isshoni-codesign-1's DER>"
```

(`codesign` would derive `certificate root = H"…"` for a self-signed certificate, which is the same hash, S2's
README; writing it out makes the bundle's requirement a reviewed constant that `machocheck` and `verify-macos` compare.)

| What persists | Keyed by | Why N → N+1 keeps it |
|---|---|---|
| TCC: System Audio Recording (`kTCCServiceAudioCapture`). Screen Recording is never requested (01 §9.1, 02 §8.1); `NSScreenCaptureUsageDescription` is kept only as a harmless string | bundle id + the code requirement stored at grant time | N+1 satisfies the stored requirement (same identifier, same leaf certificate) though its CDHash changed: S2, builds 1→9, no second prompt |
| TCC: Local Network (15+) | the same, per S2 on macOS 27 | S2 builds 8→9. **On 15 and 26 this is unverified**: macOS 15's implementation was reported to key apps by the main executable's `LC_UUID`, which changes per build. 05's M-MAC-16 with M-MAC-8's update (pass criteria in §14) measures it on 15 and 26; §13 has the contingency |
| Login keychain items (refresh tokens, D5) | the item ACL's trusted-application entries, which store the creating app's designated requirement | expected; decided by 01's KC check (`tools/kccheck` in `desktop-macos`, 01 §13.3) before any release; if KC fails, 01 switches darwin to a 0600 `credentials.json` and this row reads n/a. 05's M-MAC-9 keeps the manual N → N+1 check |
| `SMAppService` login item, notification permission | bundle id (and path) | the swap keeps both (§7.3) |
| Gatekeeper's first-launch approval ("Open Anyway") | recorded once; updates carry no quarantine | the updater writes no quarantine flag (Go's `net/http` and its own tar reader) |

A **change of identity** (a leak, §5.8; or the pending move to Developer ID) makes every app re-prompt once for each
grant, then the new requirement is stored. The Keychain item is not read (01 §13.1 keys the account by the old signing
leaf, `isshoni:v1:<leaf>:<origin>`): the user links each server again, with no Keychain prompt. The plan's later upgrade
(an offline root CA with the requirement on the root) costs that same one-time reset when it happens; it is not done in
M3 because the leaf form already lasts 30 years and a root-anchored requirement would keep trusting a leaked leaf.

### 5.3 Committed files and their checks

- `deploy/keys/macos_codesign` (LF, one line per identity, written by `task release:codesign-keys`):

  ```
  isshoni-codesign-1 sha1=<40 hex> sha256=<64 hex>
  isshoni-codesign-backup-1 sha1=<40 hex> sha256=<64 hex>
  ```
- Two generated requirement texts, from that file:
  - **bundle requirement** (signing, §5.5): `identifier "io.isshoni.desktop" and certificate leaf = H"<sha1 of -1>"`;
  - **update requirement** (updater and script): `identifier "io.isshoni.desktop" and (certificate leaf = H"<sha1 of
    -1>" or certificate leaf = H"<sha1 of backup-1>")`.
- `internal/update/signers.go` gains a block `// BEGIN CODESIGN REQUIREMENT` … `// END CODESIGN REQUIREMENT` holding
  `const codesignRequirement = "<update requirement>"`; `deploy/install-desktop.sh` a block `# BEGIN CODESIGN
  REQUIREMENT` … `# END CODESIGN REQUIREMENT` with `CODESIGN_REQUIREMENT='<update requirement>'`. An empty file gives
  empty blocks: the updater then reports `disabled`/`no_keys` on macOS and the script refuses as a development copy.
- `repocheck keys` (`task lint:keys`) extends M2's checks: each `.pem` parses, is self-signed, RSA ≥ 3072, has the
  extensions of §5.1 and a `notAfter` at least 20 years after `notBefore`; its SHA-1/SHA-256 equal the file's line;
  exactly one `isshoni-codesign-<n>` and one `isshoni-codesign-backup-<n>`; both generated blocks equal the renderer's
  output; the README rows list both fingerprints. `deploy/keys/isshoni-testtools-1.pem`, when present, is checked with
  the same certificate rules (self-signed, RSA ≥ 3072, §5.1's extensions, ≥ 20 years) but outside the active/backup
  pair: it is never counted as either, and a `testtools` line in `macos_codesign` fails. `release:keys` runs the
  codesign writer too, so one owner command refreshes every key block (M2's WA49 pattern).

### 5.4 The CI keychain (`deploy/macos/sign.sh`)

POSIX sh, used by `macos-sign`, by `SIGN=ci`, by `SIGN=dev` and by 02's `native:enginetest-app`. Three forms, any
combination in one call (one keychain setup):

```
sh deploy/macos/sign.sh -mode release|testtools|ci|dev
   [-app <isshoni.app>]                                            # §5.5, the app's entitlements and requirement
   [-tool <bundle> -tool-id <id> -tool-entitlements <file>]        # a flat bundle with one executable
   [-harness <bundle> -signlist tools/macharness/sign-list.txt]    # a bundle with nested code (05 §4.1)
```

At least one of `-app`, `-tool`, `-harness` is required. Example (02's enginetest bundle, dev identity):
`sh deploy/macos/sign.sh -mode dev -tool native/dist/IsshoniEngineTest.app -tool-id io.isshoni.enginetest
-tool-entitlements native/macos/tools/enginetest-app/enginetest.entitlements`. `dev-signing.sh` (§5.7) only creates or
locates the dev identity; `sign.sh -mode dev` calls it. Release mode reads `MACOS_CODESIGN_P12_BASE64` and
`MACOS_CODESIGN_P12_PASSWORD` from the environment and the expected SHA-1 of `isshoni-codesign-1` from
`deploy/keys/macos_codesign`:

```sh
umask 077
kc="$RUNNER_TEMP/isshoni-sign-$$.keychain-db"; kp=$(/usr/bin/openssl rand -hex 24)
trap 'security delete-keychain "$kc" 2>/dev/null; rm -f "$RUNNER_TEMP/id.p12"' EXIT
security create-keychain -p "$kp" "$kc"                 # not added to the search list
security set-keychain-settings -lut 1800 "$kc"
security unlock-keychain -p "$kp" "$kc"
printf '%s' "$MACOS_CODESIGN_P12_BASE64" | base64 -D > "$RUNNER_TEMP/id.p12"
security import "$RUNNER_TEMP/id.p12" -k "$kc" -P "$MACOS_CODESIGN_P12_PASSWORD" -f pkcs12 -T /usr/bin/codesign
rm -f "$RUNNER_TEMP/id.p12"
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$kp" "$kc" >/dev/null
security find-identity -p codesigning "$kc" | grep -q "$SHA1" || fail "the secret is not isshoni-codesign-1"
```

`find-identity` is used **without `-v`**: an untrusted self-signed identity is listed with `CSSMERR_TP_NOT_TRUSTED`
and filtered out by `-v`, yet `codesign` signs with it when selected by hash (S2). No trust setting is ever changed.
The secret values never reach a log (`set -x` is off; the step's env is the only place they exist).

`-mode testtools` is the same keychain setup with `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` and the SHA-1 of
`deploy/keys/isshoni-testtools-1.pem` ("the secret is not isshoni-testtools-1" otherwise); it accepts only
`-harness` and `-tool` and refuses `-app`, and `-mode release` refuses `-harness`, so neither identity can sign the
other's code.

### 5.5 Signing and the checks after it

```sh
req="identifier \"$BUNDLE_ID\" and certificate leaf = H\"$sha1_lower\""
codesign --force --options runtime --timestamp=none --identifier "$BUNDLE_ID" \
  --entitlements cmd/isshoni-desktop/build/darwin/isshoni.entitlements \
  --requirements "=designated => $req" --keychain "$kc" --sign "$SHA1" "$app"
```

- `--timestamp=none`: Apple's timestamp service is for Apple-issued certificates; the signature's validity rests on
  the 30-year certificate.
- **`-tool`**: the bundle is signed the same way with `-tool-id` as identifier, `-tool-entitlements` and the
  requirement `identifier "<id>" and certificate leaf = H"<sha1>"`.
- **`-harness`**: the sign list is a committed TSV, `tools/macharness/sign-list.txt` (reviewed like `sign.sh`, so
  `macos-sign` never takes identifiers or entitlement paths from the unprivileged build job). One line per code item,
  inside-out (nested bundles' executables before their bundles, every fixture bundle before `Contents/MacOS`'s tools,
  the outer bundle last as `.`): `<relative path>\t<identifier>\t<entitlements file or ->`, for example
  `Contents/Resources/fixtures/bin/voice-cli-helper\tio.isshoni.test.voice-cli-helper\t-` (a bare Mach-O); a second
  executable such as `fixtures/FakeGame.app/Contents/MacOS/game-child` gets its own line before its bundle's line
  (identifiers, entitlements and usage strings of the fixtures: 03 §16.3). Each line
  is signed with `codesign --force --options runtime --timestamp=none -i <id> [--entitlements <file>]
  --requirements "=designated => identifier \"<id>\" and certificate leaf = H\"<sha1>\""`; nothing is ever signed
  with `--deep`. `sign.sh` refuses a list whose paths don't exist, a Mach-O in the bundle that no line names, or a
  line out of inside-out order. 05's `tools/macharness` writes the bundle to match the list, and a macharness test
  compares the built layout with it.
- After signing, checks 1–3 below run on every bundle line (and on `-tool`'s bundle); checks 4–5 are adapted:
  `Identifier` = the line's id, and the entitlements equal the listed file (none when `-`).
- **Checks** (fail closed, in `sign.sh` and again in `macos-pkg`/`verify-macos`):
  1. `codesign --verify --strict --verbose=2 "$app"`;
  2. `codesign --verify --strict -R="=$req" "$app"`;
  3. `codesign -d -r- "$app"` prints exactly `designated => $req`;
  4. `codesign -d --entitlements - --xml "$app"` equals the committed entitlements (compared after `plutil -convert
     xml1`);
  5. `codesign -dv "$app"` shows `Identifier=io.isshoni.desktop`, `flags=0x10000(runtime)`, `TeamIdentifier=not set`,
     `Authority=isshoni code signing 1`, and the CDHash list has an `arm64` and an `x86_64` entry;
  6. `spctl --assess --type execute -vv "$app"` is **expected to reject** (no notarization); the output is printed and
     only the word `rejected` is asserted, so a future macOS wording change doesn't break CI.

### 5.6 The ephemeral CI identity (`deploy/macos/ci-identity.sh`)

Same `openssl` lines as §5.1 with `-days 2`, subject `CN=isshoni CI ephemeral` and a random `.p12` password, into
`$RUNNER_TEMP`; prints `SHA1=` and writes `$RUNNER_TEMP/ci-requirement.txt` (the bundle requirement for this
identity). `ci-identity.sh -name <n>` (default `ci`) names the files and the subject (`CN=isshoni CI ephemeral
<n>`), so one job can hold two identities: updater e2e builds use a **second** ephemeral identity for the "wrong
signer" case (§9.1), and 01's KC job one for its identity A → B step (01 §13.3 step 5). `sign.sh -mode ci` imports the
one named by `-name` like §5.4. In a dry run of `release.yml` a third (`-name testtools`) stands in for
`isshoni-testtools-1` in `macos-sign-tools`. The dry run of `release.yml` uses the default (`ci`) identity in place of
`isshoni-codesign-1`'s secret (§10.1), and `verify-macos` then checks against the dry-run requirement, as M1's dry run
swaps keys.

### 5.7 The dev identity (`deploy/macos/dev-signing.sh`)

S2's `dev-signing.sh`, moved and generalized: it only creates or locates one identity `isshoni Dev Code Signing` in its
own keychain under `$ISSHONI_DEV_SIGNING_DIR` (default `~/.isshoni-dev/signing`, outside the repository), not on the
search list, no trust change, and prints its SHA-1 and keychain path; signing is always `sign.sh -mode dev`. `SIGN=dev`
signs `io.isshoni.desktop.dev` with it; 02's `IsshoniEngineTest.app` (`io.isshoni.enginetest`) and 05's dev harness
(`io.isshoni.dev.harness`) use the same identity through `sign.sh -tool` / `-harness`. Dev grants persist across
rebuilds (S2), and never touch the release app's grants (D16). The exit harness is never signed with it (nor with the
release identity): it gets `isshoni-testtools-1` (§5.1) in `macos-sign-tools` (§10.1).

### 5.8 Rotation, leak and the Developer ID switch

- **Planned rotation**: none within the certificate's 30 years. Replacing the active identity without a reason would
  reset everyone's grants once.
- **Leak of `isshoni-codesign-1`** (someone else can sign code that satisfies the bundle requirement, so it would
  inherit users' capture grants and read the Keychain items):
  1. publish a security advisory on `/security` naming the fingerprint, with `tccutil reset All io.isshoni.desktop`
     for anyone worried;
  2. the next release is signed with `isshoni-codesign-backup-1` (the owner puts its `.p12` into the two secrets for
     that release and from then on). In the same release `macos_codesign` drops the leaked line, keeps
     `isshoni-codesign-backup-1` with the suffix `role=active` (the certificate's hash is what counts, so it keeps
     its name) and adds a new offline backup `isshoni-codesign-backup-2` with `role=backup`; the bundle requirement
     now names `backup-1`, the update requirement `backup-1` or `backup-2`;
  3. apps update to it (their embedded update requirement accepts the backup), macOS re-prompts once for each grant
     and stores the new requirement, so the leaked key loses every TCC grant on updated Macs. It does **not** lose
     the Keychain items stored before the update: their ACL still names the old requirement, so code signed with
     the leaked key can read those refresh tokens (the updated app ignores them, 01 §13.1, and links again);
  3a. the advisory (step 1) and the release notes tell every user to revoke the "isshoni on Mac" device on each of
     their servers (Settings → Devices) and link again, which invalidates the refresh tokens the leaked key could
     still read;
  4. the leaked key can't push updates by itself: the updater installs only what a manifest signed by the update key
     names (M2 06 §8.1).
  `repocheck keys` therefore accepts `role=active` / `role=backup` suffixes on the lines once a rotation happened; the
  plain names of §5.3 imply the roles.
- **Leak of the backup**: a normal release drops it and adds `backup-2`; no app re-prompts (the bundle requirement
  names only the active key).
- **Loss of the active `.p12`** (not leaked): handled like a leak, without the advisory.
- **Pending: Developer ID.** When the owner joins the Apple Developer Program, the requirement becomes `anchor apple
  generic and certificate leaf[subject.OU] = "<TeamID>" and identifier "io.isshoni.desktop"`, the release adds
  notarization (`notarytool`, stapling) and a timestamp, and the update requirement accepts both the self-signed
  active key and the Team ID for one release, so apps move over through the updater; every Mac re-prompts once
  (PLAN Pending). Nothing in M3 blocks it: `sign.sh` takes the identity and requirement as inputs.

### 5.9 What Gatekeeper shows on each install path

| Path | Quarantine flag on the app? | First launch |
|---|---|---|
| `curl … \| sh` (§6.3) | no (curl and `/usr/bin/tar` set none; the script also removes one if a mirror's file carried it) | opens, no dialog |
| Homebrew cask (§6.6) | Homebrew sets it; the cask's `postflight` removes it | opens, no dialog (if Homebrew or its audit rules out the postflight: the DMG's Open Anyway path once, §6.6) |
| In-app update (§7) | no | opens, no dialog |
| DMG downloaded in a browser, dragged to Applications | yes (propagated from the DMG) | **macOS 14**: "“isshoni” can't be opened because Apple cannot check it for malicious software" → OK; then Control-click (or right-click) the app → Open → Open, or System Settings → Privacy & Security → Open Anyway. **macOS 15, 26, 27**: "“isshoni” Not Opened … Apple could not verify “isshoni” is free of malware" → Done; then **System Settings → Privacy & Security → (Security) "“isshoni” was blocked to protect your Mac." → Open Anyway** → authenticate → Open. The Control-click shortcut no longer exists there, and the Open Anyway button stays only for about an hour after the blocked attempt |
| The app run from the mounted DMG | yes, and translocated | as above; the updater then says "Move isshoni to Applications" (D11) |

The `/download` page and `docs/download.md#macos-open-anyway` show these steps (with screenshots the owner adds at the
first release, as for Windows, M2 06 §12.2). Every page that shows the DMG also shows the terminal command first as
"no warning".

## 6. Packaging and installing

### 6.1 The `.tar.gz` (`desktoppkg macpack`)

`go -C tools run ./desktoppkg macpack -app <signed bundle> -version <v> -date <commit date> -out
dist-desktop/isshoni-desktop_<v>_darwin_universal.tar.gz`:
- entries `isshoni.app/` and below, sorted by path, PAX format; directories 0755, regular files 0644 or 0755 (0755
  exactly when any execute bit was set); uid/gid 0, empty user/group names; mtime = the commit date for every entry;
  no symlinks, hard links, devices or extended-attribute records (the bundle has none, §2.2; any found fails);
- gzip level 9, empty name, zero mtime, OS byte 255;
- reproducible: the same signed bundle gives the same bytes (a test checks two runs);
- after writing, it lists the archive and compares the list with the bundle (paths, modes, sizes, SHA-256 of each
  file); `release:assets` repeats the comparison against the stage list.

### 6.2 The DMG

`deploy/macos/make-dmg.sh <signed bundle> <v> <out.dmg>` (Apple tools only, run in `macos-pkg`):

```sh
stage=$(mktemp -d); ditto --norsrc --noextattr --noqtn "$app" "$stage/isshoni.app"
ln -s /Applications "$stage/Applications"
cp deploy/macos/dmg/If-macOS-blocks-isshoni.webloc "$stage/If macOS blocks isshoni.webloc"
for i in 1 2 3; do   # hdiutil on hosted runners sometimes fails with "Resource busy"
  hdiutil create -volname "isshoni" -srcfolder "$stage" -fs HFS+ -format ULFO -ov "$out" && break; sleep 5
done
hdiutil verify "$out"
```

- `.webloc` is a plist with `URL` = `https://moonwx.github.io/isshoni/download#macos-open-anyway`; double-clicking it
  opens the browser at the steps of §5.9.
- `ULFO` (LZFSE) needs 10.11+, smaller than `UDZO`; HFS+ for the widest compatibility of a read-only image.
- The DMG itself is **not signed** (a self-signed DMG signature changes nothing in Gatekeeper's verdict); its integrity
  is `checksums.txt`'s.
- Not reproducible (hdiutil writes UUIDs and times); that is accepted, since `checksums.txt` and the attestation cover
  it and the bundle inside is byte-identical to the tarball's.
- **Check** (`macos-pkg`): attach `-readonly -nobrowse -mountpoint`, `diff -r` the mounted `isshoni.app` against the
  signed bundle, run §5.5's checks 1–3 on the mounted copy, detach.
- **Later (M5)**: a background image and icon positions (a committed `.DS_Store` made once by the owner in Finder).

### 6.3 `install-desktop.sh` (`deploy/install-desktop.sh`)

Usage, as the `/download` page shows it: `curl -fsSL https://<server>/install-desktop.sh | sh`, or from a room's
`/download?room=<id>` link `curl -fsSL https://<server>/install-desktop.sh | sh -s -- --room <id>` (§6.5); flags after
`sh -s --`.

Rules (as `install.sh`, M1 06 §4): POSIX sh (`dash`, `bash --posix`, macOS's `/bin/sh`), the body in `main()` called
on the last line so a truncated download runs nothing, `set -eu`, ASCII only, LF, no `sudo` anywhere, every temporary
file under one `mktemp -d` removed by a trap. It is the script for macOS **and later the Linux agent** (M4): on Linux
it prints "isshoni's Linux helper isn't available yet; share from Chrome or Edge meanwhile:
https://moonwx.github.io/isshoni/download#linux" and exits 5.

Layout:

```sh
#!/bin/sh
# isshoni desktop installer for macOS. Usage: curl -fsSL https://<your isshoni server>/install-desktop.sh | sh
# Docs: https://moonwx.github.io/isshoni/download#macos-terminal
# >>> isshoni-prefill (the server rewrites this block; see docs/m3/04-macos-release.md 6.4)
ISSHONI_PREFILL_SERVER=''
ISSHONI_PREFILL_VERSION='@VERSION@'
# <<< isshoni-prefill
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
ALLOWED_SIGNERS='…'
# END ALLOWED SIGNERS
# BEGIN CODESIGN REQUIREMENT (generated from deploy/keys/macos_codesign; do not edit here)
CODESIGN_REQUIREMENT='identifier "io.isshoni.desktop" and (certificate leaf = H"…" or certificate leaf = H"…")'
# END CODESIGN REQUIREMENT
main() { … }
main "$@"
```

`@VERSION@` is stamped by `task release:prepare` (as `install.sh` and the ps1). The functions shared in spirit with
`install.sh` (`die`, `say`, `download` with 3 attempts, `resolve_version` through the `releases/latest` redirect,
`version_cmp`, `checksum_for`, `verify_release`, `need_key`) are copied, not sourced (each script is one file); a
`repocheck` test compares the copied function bodies with `install.sh`'s so a fix lands in both.

Options and environment:

| Flag | Env | Meaning |
|---|---|---|
| `--version V` | `ISSHONI_VERSION` | `0.3.0`, `v0.3.0` or `latest`; default the prefill's version, else latest. Given by either the flag or the env, the version is **explicit** (step 5) |
| `--room ID` | | the room the app opens after the install (step 11); must match M2 02 §12's room id `^[A-Za-z0-9_-]{1,64}$`, else exit 2 "Not a room id: <ID>" before anything is downloaded; ignored without a usable prefilled server |
| `--dir DIR` | `ISSHONI_INSTALL_DIR` | install into `DIR/isshoni.app` (must be writable without sudo) |
| `--no-launch` | `ISSHONI_NO_LAUNCH=1` | don't start the app |
| `--allow-downgrade` | `ISSHONI_ALLOW_DOWNGRADE=1` | replace a newer installed version |
| `--no-update-checks` | `ISSHONI_NO_UPDATE_CHECKS=1` | `defaults write io.isshoni.desktop UpdateChecks -bool false` (01 reads it once) |
| `--uninstall` | | §6.7 |
| `--purge` | | with `--uninstall`: also the data |
| | `ISSHONI_DOWNLOAD_BASE` | a mirror of `…/releases` (tests: relserve) |

Flow (install):
1. **Platform**: `uname -s` `Darwin` (Linux: the M4 line above; other: exit 5). `sw_vers -productVersion` ≥ 14.4,
   else exit 5 "isshoni needs macOS 14.4 or later (this Mac has X)". `id -u` = 0 → exit 2 "Run this without sudo:
   isshoni installs for your account and updates itself." `uname -m` `arm64` or `x86_64` (anything else exit 5).
2. **Tools**: `/usr/bin/curl`, `/usr/bin/ssh-keygen` (macOS 14 ships OpenSSH 9.x, so `-Y verify` exists),
   `/usr/bin/shasum`, `/usr/bin/tar`, `/usr/bin/codesign`, `/usr/bin/open`, `/usr/bin/defaults`; a missing one exits 4
   naming it. `need_key`: an empty `ALLOWED_SIGNERS` or `CODESIGN_REQUIREMENT` block → exit 3 "This copy of the script
   carries no release key (a development copy)…".
3. **Version**: as `install.sh`; regex `^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`.
4. **Destination**: `--dir`; else an existing `/Applications/isshoni.app` that this user can replace (`[ -w
   /Applications ] && [ -w /Applications/isshoni.app ]`, the same rule as `update.Location`'s rule 4, §7.2), else an
   existing `~/Applications/isshoni.app`; else `/Applications` when `[ -w /Applications ]` (admin users), else
   `~/Applications` (created). Two usable existing copies: the `/Applications` one is updated and a note names the
   other. **An existing `/Applications/isshoni.app` this user can't replace** (an admin installed it; a standard user
   runs the script, 05 §3.2) is left alone: the destination becomes `~/Applications`, the note "isshoni in
   /Applications was installed by another account and can't be updated from yours; installing your own copy in
   ~/Applications. isshoni links and updates use your copy." is printed, and after the swap (step 9) the script runs
   `/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f
   "$HOME/Applications/isshoni.app"` so LaunchServices prefers the user's copy for `isshoni://` (a failure there is one
   warning line, not an error). Step 11's `open -a "<app>"` names the user's copy by path either way. `--dir` naming a
   folder this user can't write → exit 9 (below) before anything is downloaded.
5. **Existing copy**: its version is `defaults read "<app>/Contents/Info" IsshoniVersion` (a copy without the key is
   older than M3's first release and is replaced). **Already installed, no explicit version** (neither `--version` nor
   `ISSHONI_VERSION`, so the target is the prefill's or the latest version) and `version_cmp` installed ≥ target: the
   script prints "isshoni X is already installed in <dir>.", skips steps 6–9 (and 10 unless `--no-update-checks` was
   given), still runs step 11 (the server link, with `--room` if given) and exits 0. Whether the app runs doesn't matter
   here: `open` hands the link to a running app, which takes it through M2's single-instance path (01 §6.3's
   `kAEGetURL`). This is the common case of a friend who has a self-updated app and runs another server's command to
   join it. Otherwise (an explicit version, or a real upgrade): `version_cmp` new < installed → exit 7 "A newer isshoni
   (X) is installed; set ISSHONI_ALLOW_DOWNGRADE=1 to replace it". Running (`ps -U "$(id -u)" -o comm=` has a line
   ending in `/isshoni.app/Contents/MacOS/isshoni`) → exit 6 "Quit isshoni (menu bar icon → Quit) and run the command
   again". An update in progress counts too: the `--apply-update` helper is an isshoni process with that same executable
   path (`ps -U "$(id -u)" -o args=` shows `/isshoni.app/Contents/MacOS/isshoni --apply-update=…`), so the test above
   already catches it (POSIX sh on macOS has no `flock(1)` to probe `applying.lock`).
6. **Download** to the temp dir with `curl -fsSL --proto '=https' --tlsv1.2 --retry 3` from
   `$BASE/download/v<v>/`: `isshoni-desktop_<v>_darwin_universal.tar.gz`, `checksums.txt`, `checksums.txt.sig`
   (`checksums.txt.sigstore.json` and `cosign verify-blob` only when cosign is installed, as `install.sh`).
7. **Verify** (`verify_release`): `ssh-keygen -Y verify -f <allowed_signers> -I isshoni-release -n isshoni-checksums -s
   checksums.txt.sig < checksums.txt`; exactly one checksums line for the archive; `shasum -a 256` equal. Any failure:
   exit 3, "Nothing was installed or changed.", the first line `ssh-keygen` said, and
   `https://moonwx.github.io/isshoni/security#verify-desktop`.
8. **Stage** on the destination's volume: `tar -tzf` must list only paths under `isshoni.app/` without `..` or a
   leading `/`; extract into `<dest>/.isshoni-install-<pid>/` (created with `mkdir -m 0700`; a "Permission denied" here
   is exit 9); `codesign --verify --strict
   -R="=$CODESIGN_REQUIREMENT" <staged app>`; `defaults read <staged>/Contents/Info CFBundleIdentifier` =
   `io.isshoni.desktop`; `<staged>/Contents/MacOS/isshoni --version` reports `<v>`; `xattr -dr com.apple.quarantine
   <staged app> 2>/dev/null || true`. Any failure: exit 3 as in step 7.
9. **Swap**: without an existing copy, `mv staged → <dest>/isshoni.app`. With one: `mv <app> <stage>/old.app`, then
   `mv staged → <app>`; if the second fails, `mv <stage>/old.app <app>` back. A first `mv` that fails with "Operation
   not permitted" is macOS's App Management protection (§13): exit 8 "macOS didn't let Terminal replace isshoni. Allow
   your terminal app in System Settings → Privacy & Security → App Management, or update from inside isshoni.
   More: https://moonwx.github.io/isshoni/troubleshooting#macos-app-management". A `mkdir` or `mv` that fails with
   "Permission denied" (EACCES: the folder or the existing bundle belongs to another account) → exit 9 "You can't
   change isshoni in <dir> from this account. Run the command again without --dir to install your own copy in
   ~/Applications, or ask the person who installed it to update it." (with the step-9 rollback first when the first
   `mv` already happened). Then remove the temp folders.
10. `--no-update-checks` → `defaults write`.
11. **Launch** unless `--no-launch`: with a usable prefilled server (`^https://[A-Za-z0-9.:\[\]-]+$`), `open -a "<app>"
    "isshoni://<host[:port]>/"`, or with `--room <id>` `open -a "<app>" "isshoni://<host[:port]>/r/<id>"` (M2 02 §12's
    grammar, the same string `appLinkFor(origin, id)` builds; the app shows "Add server {host}?" before any request,
    M2 02 §12.3, then opens the room); otherwise `open "<app>"`.
12. "isshoni <v> is installed in <dir>." plus "It is starting now." or "Open it from Applications.".

Exit codes: 0 ok; 1 unexpected; 2 usage (incl. sudo); 3 download or verification failed (nothing changed); 4 a tool is
missing; 5 unsupported OS; 6 isshoni is running; 7 newer version installed; 8 macOS blocked the replacement (App
Management); 9 this account can't write the destination (EACCES). Exits 6 and 7 happen only with an explicit version
or a real upgrade (step 5).

### 6.4 Server: `GET /install-desktop.sh`

M2 registered the path as a 404 (M2 06 §7.4). M3 serves the script, mirroring the ps1:
- `deploy/embed.go` gains `//go:embed install-desktop.sh` → `var InstallDesktopSH []byte`.
- `internal/server/httpapi/desktopscripts.go`: `DesktopScripts` gains `SH []byte`; new
  `func RenderInstallSH(tmpl []byte, origin, version string) ([]byte, error)` with the same marker rules as
  `RenderInstallPS1` (exactly one `# >>> isshoni-prefill` line and one `# <<< isshoni-prefill` after it, matched by
  line prefix). The block becomes:

  ```sh
  # >>> isshoni-prefill (written by the isshoni server at https://watch.example.com)
  ISSHONI_PREFILL_SERVER='https://watch.example.com'
  ISSHONI_PREFILL_VERSION='0.3.0'
  # <<< isshoni-prefill
  ```

  The origin is kept only when ASCII, without control characters, `'`, `\` or whitespace, and `https://…` (or
  `http://` loopback in a dev server, which the script then ignores for the link); otherwise empty with one WARN. The
  version only when it matches `^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`.
- Response as the ps1's: `200`, `Content-Type: text/plain; charset=utf-8`, `Cache-Control: no-cache`, `ETag`,
  `Content-Length`, `HEAD`. `SH` nil → the M2 404 text (tests). The startup check (`checkDesktopScripts` in
  `internal/server/wire.go`) renders both scripts and refuses to start (exit 78) on a template without a valid block.
- The wiring line in `internal/server/http.go`'s `routerOptions()` passes `deploy.InstallDesktopSH`.
- M1 04 §9.2's route row ("reserved") and M2's "404 until M3" texts follow with the integrator.
- No API change: `/api/v1/info` already has `server.version` and `server.publicUrl`.

### 6.5 The `/download` page (`web/src/download/`)

`links.ts` gains, in `DesktopLinks`:

```ts
macDmgUrl: string;       // release ? …/releases/download/v<v>/isshoni-desktop_<v>_darwin_universal.dmg : …/releases/latest
shCommand: string;       // `curl -fsSL ${publicUrl}/install-desktop.sh | sh`, plus ` -s -- --room ${roomId}` only when
                         // validRoomId(roomId) (the same gate as openAppUrl)
brewCommand: string;     // 'brew install --cask moonwx/isshoni/isshoni'
```

Page content, **macOS** block (first when `navigator.userAgentData?.platform === 'macOS'` or the UA says
`Macintosh` and `navigator.maxTouchPoints <= 1`, so an iPad in desktop mode isn't taken for a Mac):
- **Install with Terminal (recommended: no warning, and the app opens set up for this server, and in the room when the
  page came from a room link)**: `shCommand` with a Copy button and one line "Open Terminal (Applications →
  Utilities), paste, press Return." The command already carries the room, so a Terminal user needs no other step.
- **Or download**: three steps that stay visible: **1 Download** ("Download for Mac", `macDmgUrl`), **2 Open it**
  ("Drag isshoni to Applications, then open it. macOS will say it can't verify the app: open System Settings →
  Privacy & Security and click Open Anyway (macOS 14: Control-click the app → Open)", linking
  `/download#macos-open-anyway` on the project site), **3 Open isshoni** (the M2 "Open isshoni" button, `openAppUrl`,
  same enabling rules as Windows, plus: a click on the Terminal command's Copy button also enables it, for a friend
  who already has the app).
- **Homebrew**: `brewCommand` with Copy.
- Requirements: "macOS 14.4 or later; Apple silicon (Intel Macs: best effort)".
- Texts `account.download.macos.*` in the lazy `account` namespace; the Linux block stays "Coming (M4)".
- The Windows block, its order rule and its texts are unchanged; on a non-Mac, non-Windows browser both blocks show,
  Windows first (M2's order).

`links.test.ts` asserts the exact DMG URL, `shCommand` (without a room, with a valid room → the `-s -- --room <id>`
suffix, with an invalid room → no suffix) and `brewCommand`; `links.node.test.ts` checks that line 2 of
`deploy/install-desktop.sh` shows the same command form, allowing the optional `-s -- --room <id>` suffix;
`DownloadPage.test.tsx` the macOS ordering and the iPad rule.

### 6.6 The Homebrew tap

**Repository** (the owner creates it, §12): `MoonWX/homebrew-isshoni`, public, Apache-2.0, a README with the install
line, `Casks/isshoni.rb`, and **no other code**. The release pipeline is its only writer.

`deploy/homebrew/isshoni.rb.tmpl` in this repository (rendered by `sh deploy/homebrew/render.sh <v> <sha256> <
isshoni.rb.tmpl > isshoni.rb`, a `sed` over the two fields that checks both against their regexes; shell, so the release
job that holds the tap key runs no Go):

```ruby
cask "isshoni" do
  version "@VERSION@"
  sha256 "@SHA256@"

  url "https://github.com/MoonWX/isshoni/releases/download/v#{version}/isshoni-desktop_#{version}_darwin_universal.tar.gz",
      verified: "github.com/MoonWX/isshoni/"
  name "isshoni"
  desc "Share your screen and system audio with friends, minus voice apps"
  homepage "https://moonwx.github.io/isshoni/"

  livecheck do
    url :url
    strategy :github_latest
  end

  auto_updates true
  depends_on macos: ">= :sonoma"

  app "isshoni.app"

  postflight do
    system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{appdir}/isshoni.app"]
  end

  uninstall quit: "io.isshoni.desktop"

  zap trash: [
    "~/Library/Application Support/isshoni",
    "~/Library/Caches/io.isshoni.desktop",
    "~/Library/HTTPStorages/io.isshoni.desktop",
    "~/Library/Preferences/io.isshoni.desktop.plist",
    "~/Library/Saved Application State/io.isshoni.desktop.savedState",
    "~/Library/WebKit/io.isshoni.desktop",
  ]

  caveats <<~EOS
    isshoni updates itself. Before `brew uninstall`, unlink it from your servers in isshoni's settings
    (or run: curl -fsSL https://<your server>/install-desktop.sh | sh -s -- --uninstall).
    Requires macOS 14.4 or later.
  EOS
end
```

- `auto_updates true`: the app's own updater swaps the bundle (§7); `brew upgrade` skips the cask unless `--greedy`.
- **No `uninstall` script** that forgets credentials: Homebrew runs `uninstall` directives on upgrades too, which would
  unlink the app from every server on each `brew upgrade --greedy`. Credentials are forgotten through the app or the
  script (§6.7), as the caveat says. The `zap` list equals §2.3's OS-managed folders and 01's data root; `zap` can't
  reach Keychain items, which the docs say.
- `depends_on macos: ">= :sonoma"` admits 14.0–14.3, which `LSMinimumSystemVersion` then refuses to launch; the
  caveat names 14.4.
- If a future Homebrew forbids removing the quarantine flag in a tap's `postflight`, brew installs get the DMG's Open
  Anyway path once; the docs' Homebrew section says what to do. The same holds when `brew audit --cask --strict` (MR14)
  objects to the `xattr` postflight: the postflight is dropped and `#macos-homebrew` documents Open Anyway, rather than
  dropping `--strict`. Either outcome is acceptable: the release checks (§10.7 items 4c and 11) pass on "opens without
  a dialog, or with exactly the documented Open Anyway path once".
- **Updated by `release.yml`** job `homebrew` (§10.1) for final releases only, as a commit to the tap's `main`
  ("isshoni <v>"), with the author `isshoni release <isshoni-release@users.noreply.github.com>` (a fixed bot identity,
  never a personal address), through the secret `HOMEBREW_TAP_DEPLOY_KEY` (an ed25519 deploy key
  with write access to the tap only) and GitHub's pinned SSH host keys in `deploy/homebrew/known_hosts`.
- **Checked on every change** of the template (`homebrew` filter) in `desktop-macos`: render with a dummy version and
  hash, `brew style --cask <file>`, and `brew audit --cask --strict` through a throwaway local tap (`brew tap-new
  isshoni-ci/test --no-git`); the nightly also installs the cask from that local tap with the URL pointed at relserve
  (§9.3).
- **Later**: an `isshoni@beta` cask for rc releases, if friends ask.

### 6.7 Uninstall and purge

| Path | What it does |
|---|---|
| `curl -fsSL https://<server>/install-desktop.sh \| sh -s -- --uninstall` | refuses while isshoni runs (exit 6); runs `<app>/Contents/MacOS/isshoni --forget-credentials` (M2 02's `ForgetAll`: every linked server revoked under one 5 s budget, the Keychain items deleted; exit 0 offline); removes the bundle after checking its `CFBundleIdentifier`; keeps the data root |
| … `--uninstall --purge` | as above but `--purge-data` (forget, then 01's data root deleted, and 01 unregisters the `SMAppService` login item), then removes §2.3's OS-managed folders, `defaults delete io.isshoni.desktop`, and `tccutil reset All io.isshoni.desktop` (the user's own TCC rows; no admin needed) |
| Homebrew `brew uninstall --cask isshoni` / `--zap` | quits the app and removes it; `--zap` removes the folders of §6.6; credentials: see the caveat |
| Dragging the app to the Trash | removes only the bundle; the docs (`#macos-uninstall`) say to unlink first in Settings (or revoke the device from the server's Devices page afterwards) and how to remove the rest |

The script's uninstall finds the bundle with the destination rule of §6.3 step 4 (`--dir` respected; a copy this user
can't replace is skipped as there, so a standard user uninstalls their own `~/Applications` copy and a note names the
admin's copy).

## 7. The updater on macOS (`internal/update`)

Everything of M2 06 §8 holds unchanged (trust model, signature format, channels, schedule, limits, states, error
codes, loop guard, `revokedKeys`, `minVersion`, `Enabled`, the `updatetest` hooks). This section is only what macOS
adds or changes.

### 7.1 Manifest

`isshoni-desktop-update.json` from the first M3 release on:

```json
{
  "schema": 1,
  "product": "isshoni-desktop",
  "version": "0.3.0",
  "channel": "stable",
  "minVersion": "0.2.0-rc.1",
  "publishedAt": "2027-01-15T10:00:00.000Z",
  "expiresAt": "2027-07-14T10:00:00.000Z",
  "notesUrl": "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0",
  "artifacts": [
    {"os": "darwin", "arch": "universal", "kind": "darwin-app",
     "name": "isshoni-desktop_0.3.0_darwin_universal.tar.gz",
     "url": "https://github.com/MoonWX/isshoni/releases/download/v0.3.0/isshoni-desktop_0.3.0_darwin_universal.tar.gz",
     "size": 41943040, "sha256": "<64 lowercase hex>"},
    {"os": "windows", "arch": "amd64", "kind": "nsis",
     "name": "isshoni-desktop_0.3.0_windows_amd64_setup.exe",
     "url": "https://github.com/MoonWX/isshoni/releases/download/v0.3.0/isshoni-desktop_0.3.0_windows_amd64_setup.exe",
     "size": 15204352, "sha256": "<64 lowercase hex>"}
  ],
  "revokedKeys": []
}
```

Code changes in `internal/update/manifest.go`:
- `artifactKinds` = `{"windows": "nsis", "darwin": "darwin-app"}`.
- `Artifact.check`: a `darwin-app` artifact's `name` must be `isshoni-desktop_<v>_darwin_<arch>.tar.gz` with `arch` in
  `universal`, `arm64`, `amd64`.
- `Manifest.Artifact(goos, goarch)`: for `darwin`, the entry with `Arch == goarch` and kind `darwin-app` if any, else
  the one with `Arch == "universal"`; Windows unchanged (exact `amd64`).
- **Released M2 apps** accept this manifest unchanged (D10): the darwin entry passes M2's token, name, URL, size and
  hash rules and is never selected on Windows. `manifest_test.go` gains this exact manifest as a vector for both
  OSes, and `testdata/` keeps M2's Windows-only vectors byte for byte.

`tools/desktoppkg manifest` gains `-macos-archive <file>` next to M2's `-installer <file>`; at least one is required;
artifacts are written sorted by (`os`, `arch`) (darwin first), so a Windows-only manifest (`-installer` alone, as
ci.yml's and nightly's Windows updater e2e write it) is byte-identical to M2's (its golden stays unchanged).
`release:assets DESKTOP=true MACOS=true` requires both entries.

### 7.2 Which copy may update (`update.Location`)

```go
// Location describes the running copy and whether it can update itself in place (06 §8.7, 04 §7.2).
type Location struct {
	Path      string // macOS: the bundle (…/isshoni.app); Windows: the exe's folder
	Installed bool
	Reason    string // "" when Installed; macOS: "not_bundle", "translocated", "read_only", "not_writable";
	                 // Windows: "not_install_dir"
}

func CurrentLocation() (Location, error)
```

macOS rules, in order (`apply_darwin.go`):
1. `os.Executable()`, `filepath.EvalSymlinks`; it must end in `/isshoni.app/Contents/MacOS/isshoni` and the bundle's
   `Info.plist` must say `CFBundleIdentifier` `io.isshoni.desktop` (an `encoding/xml` walk; a dev bundle is
   `not_bundle`) → else `not_bundle`.
2. The path contains `/AppTranslocation/` → `translocated`.
3. `unix.Statfs(bundle)` has `MNT_RDONLY` → `read_only` (a mounted DMG).
4. `unix.Access(parent, W_OK)` and `unix.Access(bundle, W_OK)` both succeed → installed; else `not_writable`.

A newer verified manifest seen by a copy that is not installed gives M2's `error`/`not_installed` with `Available`,
`NotesURL` and now `Status.Reason` (new field; empty on Windows except `not_install_dir`), and nothing is downloaded.
01 shows "Move isshoni to Applications" (`translocated`, `read_only`) or, for `not_writable` (typical when an admin
installed to `/Applications` and a standard user runs it), "isshoni can't update itself here. Install your own copy:
open your server's Download page and run its Terminal command (curl … | sh)" with the `/download` link; the script
then installs into `~/Applications` and makes that copy the one links open (§6.3 step 4).

### 7.3 Download and apply

- Folder `<data root>/updates/` (`Options.DataDir` + `updates`, created 0700); `state.json` exactly as M2 06 §8.7,
  plus `pending.lastError` (a §7.6 code or `""`), written by the helper when a swap fails, so the next start shows why.
- The download, size and SHA-256 checks, the free-space rule and one-download-at-a-time are M2's.
- **`PendingAtStartup` does not apply on darwin** (01 §6.1's start step 5 is a no-op there): updates apply only from a
  running app (Restart now, or Apply at Quit), where a pending link is already known. A link that launches the app
  arrives as `kAEGetURL` only after `NSApp` runs (01 §6.3), so a start-time apply could not keep it.
- **`Apply(ctx, p, relaunch)` on macOS:**
  1. Re-verify the archive (size, SHA-256; a mismatch deletes it, `hash_mismatch`); `CurrentLocation()` must be
     installed (else `ErrNotInstalled`).
  2. Extract into `<bundle folder>/.isshoni-update-<v>-<8 random hex>/` (created with mode 0700; same volume as the
     bundle, so the swap is one rename) with the package's own tar reader: only `isshoni.app/…` paths, only regular
     files and directories, no `..`, no absolute paths, modes masked to 0755/0644, at most 20 000 entries and 512 MiB
     unpacked, files written with `O_EXCL`; anything else is `bad_archive`.
  3. `/usr/bin/codesign --verify --strict -R=<"=" + codesignRequirement> <staged>/isshoni.app` (30 s timeout) → else
     `codesign_mismatch`; the staged `Info.plist`'s `CFBundleIdentifier` and `IsshoniVersion` equal
     `io.isshoni.desktop` and the manifest's version; `<staged>/Contents/MacOS/isshoni --version` (10 s) reports the
     same version → else `bad_archive`.
  4. Write `apply-<v>.json` (0600) into `updates/`:

     ```json
     {"v": 1, "pid": 4242, "bundle": "/Applications/isshoni.app",
      "staged": "/Applications/.isshoni-update-0.3.1-9f2c4e1a/isshoni.app",
      "from": "0.3.0", "to": "0.3.1", "relaunch": true}
     ```
  5. Increment `attempts` in `state.json` (the loop guard, M2 06 §8.7), start `<bundle>/Contents/MacOS/isshoni
     --apply-update=<request>` (the **running** app's own executable; `exec.Cmd` with `SysProcAttr{Setsid: true}`,
     stdio to `/dev/null`, its context never done), wait until `applying.lock` is held by another process (try-lock
     every 50 ms, ≤ 10 s; on timeout one WARN and return nil, as M2), and return. 01 then quits within 5 s, never
     during a live share (M2 03's rule).
- **`RunApplyHelper(ctx, request, log)`** (the `--apply-update` one-shot; logs to `updates/helper.log`, at most 1 MiB,
  rotated once):
  1. `flock(LOCK_EX)` on `applying.lock` (blocking ≤ 5 s; another helper holding it → exit 1).
  2. Read the request (≤ 4 KiB, unknown fields refused). The request path must be `<data root>/updates/apply-<to>.json`
     and a regular file (`lstat`, not a symlink) with mode 0600 (consistency with Apply step 4; the same user can
     write there, so the real checks are steps 4–5). `bundle` must equal this executable's own bundle; `staged` must
     be `<dir of bundle>/.isshoni-update-<to>-<hex>/isshoni.app`.
  3. Wait for `pid` to exit: `kqueue` `EVFILT_PROC`/`NOTE_EXIT` (an `ESRCH` at registration means already gone), at
     most 30 s; still running → remove the staging folder, release the lock, exit 1 (the app keeps running and the
     next start retries, counted by `attempts`).
  4. Re-run step 3's `codesign -R` on `staged` (the staging folder is the user's, so this only narrows the window;
     step 6 closes it).
  5. **Version check** (the helper re-derives trust instead of taking the request's word, since any process of the
     same user can write a request and stage an older, genuinely signed release): read `IsshoniVersion` from the
     staged `Contents/Info.plist` (sealed by the signature step 4 just verified) and from the helper's **own** bundle's
     `Info.plist` (its version is also compiled in; both must agree, else `bad_archive`). The staged version must
     equal the request's `to`, and `version.Compare(to, <own IsshoniVersion>) > 0`. Otherwise: refuse, remove the
     staging folder, `pending.lastError` = `downgrade` (§7.6), release the lock, relaunch as step 7 after a failed
     swap, exit 1. This keeps PLAN's "no downgrades" rule in the only process that swaps (Apply step 3's check alone
     ran in the app, which a forged request skips), so isshoni can't be used as a confused deputy past App
     Management. (An old signed release run from elsewhere already passes the requirement; what this stops is the
     persistent in-place downgrade of the copy the user launches.) The installer's `ISSHONI_ALLOW_DOWNGRADE` has no
     helper counterpart.
  6. `unix.RenamexNp(staged, bundle, unix.RENAME_SWAP)`; on `ENOTSUP` (a volume without swap) two renames with a
     rollback (`bundle → <staging>/old.app`, `staged → bundle`; on failure `old.app → bundle`). `EPERM`/`EACCES` →
     `pending.lastError` = `app_management` (§13), nothing swapped. **Then verify what was installed**: `codesign
     --verify --strict -R=<"=" + update requirement> <bundle>` and step 5's version check, both on the installed
     path (the staging folder sits in a folder the user, or on `/Applications` another admin, can write, so it could
     have been replaced between steps 4 and 6). On failure: swap back with a second `RENAME_SWAP` (or, after the
     two-rename fallback, `bundle → <staging>/bad.app`, `old.app → bundle`), `pending.lastError` =
     `codesign_mismatch`, and continue to the cleanup with the old version in place. Then `os.RemoveAll(<staging
     folder>)`, which now holds the old (or the rejected) bundle.
  7. If `relaunch`: `/usr/bin/open -a <bundle> --args --updated-from=<from>` (after a failed or reverted swap the same
     command relaunches the old version without the flag), release the lock, exit 0 (1 after a failed or reverted
     swap).
- **Cleanup at start** (`update.CleanupStaging(log)`, called by 01 early at every start): removes
  `.isshoni-update-*` folders next to the bundle and `apply-*.json` files whose helper isn't running (the lock is
  free); M2's rules for stale archives and `pending` apply.
- **What survives a swap**: the bundle path, so the Dock item (a bookmark that falls back to the path once the old
  inode is gone), the `SMAppService` login item, LaunchServices' URL-scheme registration and the TCC/Keychain grants
  (§5.2). 05's M-MAC-8 (with §14's pass criteria) checks each on the owner's Macs.
- **Homebrew installs** update in place the same way; Homebrew's own record then lags, which `auto_updates true`
  declares as expected.

### 7.4 Turning it off

- 01's `updates.check` (initial value from `UpdateChecks` in the user's preferences, §2.3) and channel, as M2.
- **Policy**: `PolicyDisabled()` on darwin (`policy_darwin.go`, `//go:build darwin && cgo`) returns true when
  `CFPreferencesAppValueIsForced(CFSTR("DisableUpdateChecks"), CFSTR("io.isshoni.desktop"))` and the value is true
  (an MDM profile or `/Library/Managed Preferences/`); `darwin && !cgo` returns false. 01 shows "Turned off by your
  administrator" as on Windows.

### 7.5 Go API changes (all OS-neutral signatures; M2 06 §8.9 otherwise unchanged)

```go
type Status struct {
	// … M2's fields …
	Reason string // with ErrorCode "not_installed": Location.Reason
}

type Location struct{ Path string; Installed bool; Reason string }
func CurrentLocation() (Location, error)

// RunApplyHelper is --apply-update (darwin); other OS: ErrUnsupported.
func RunApplyHelper(ctx context.Context, requestPath string, log *slog.Logger) error

// CleanupStaging removes leftovers of an earlier apply (darwin); other OS: no-op.
func CleanupStaging(log *slog.Logger)

const ApplyHelperFlag = "--apply-update"
```

Files: `apply_darwin.go` (`//go:build darwin`: `defaultSystem`, `CurrentLocation`, `Apply`'s launcher),
`helper_darwin.go` (`RunApplyHelper`, `CleanupStaging`), `untar.go` (the tar reader; OS-neutral, so its tests and
fuzzing run on Linux), `codesign_darwin.go`, `policy_darwin.go` / `policy_darwin_nocgo.go`; `apply_other.go` becomes
`//go:build !windows && !darwin`; `apply_windows.go` gains `CurrentLocation` (wrapping M2's `installedCopy`), no
behavior change. Imports add `golang.org/x/sys/unix` (already in the module graph). The package still builds with
`CGO_ENABLED=0` for darwin (`lint-go`'s cross builds).

### 7.6 Error codes and test hooks

- New codes (M2 06 §8.10's list, stable, snake_case): `bad_archive`, `codesign_mismatch`, `app_management`,
  `downgrade` (the helper refused a staged bundle that isn't newer than the running app or isn't the requested
  version, §7.3 helper step 5). 01 adds
  `update.<code>` to `internal/desktop/bridge/codes` and texts `desktop.update.error.<code>`; `app_management` links
  `/troubleshooting#macos-app-management`.
- `updatetest` hooks (M2 06 §8.11) gain `ISSHONI_UPDATE_TEST_CODESIGN_REQUIREMENT` (a requirement text used instead of
  the embedded one); the same "no test strings in a release binary" check applies (§4.7).

## 8. Licenses

### 8.1 `THIRD_PARTY_NOTICES` for macOS

Generated in `macos-app` (on macOS, because the darwin build's module graph needs cgo):

```
go -C tools run ./notices -root .. -pkg ./cmd/isshoni-desktop -goos darwin \
  -npm ../web/dist/licenses.txt -extra ../deploy/notices/extra.txt \
  -native ../deploy/notices/native-darwin.json -native-root ../native/dist/darwin-arm64 \
  -version <v> -o ../dist-desktop/darwin_stage/licenses/THIRD_PARTY_NOTICES.txt
```

`deploy/notices/native-darwin.json` (02 and 03 add an entry for every native dependency they link, in the same PR):

```json
[
  {"name": "libopus", "vcpkgPort": "opus", "license": "BSD-3-Clause",
   "linking": "static, inside the isshoni executable", "licenseFiles": ["vcpkg/opus/copyright"]},
  {"name": "speexdsp", "vcpkgPort": "speexdsp", "license": "BSD-3-Clause",
   "linking": "static, inside the isshoni executable", "licenseFiles": ["vcpkg/speexdsp/copyright"]}
]
```

- Both are merged into 02's `libisshoni_media.a` (02 §6.2); 03's resampler stage uses speexdsp (03 §7.4). Names follow
  `native-windows.json`.
- No FFmpeg, OpenH264 or LGPL component on macOS (D17); Apple's frameworks are system libraries and not listed.
- The native gate (M2 06 §9.3) runs with the darwin file: licenses on M1's allowlist (no LGPL entry is accepted for
  darwin), the vcpkg ports in `native-root/licenses/vcpkg/*/copyright` equal the `vcpkgPort` set, and `desktoppkg check
  -os darwin`: the only Mach-O in the stage is `Contents/MacOS/isshoni`, no `.dylib`, `.framework`, `.bundle`,
  `.plugin` or `.xpc`.
- `go-licenses check ./cmd/isshoni-desktop` already runs natively in `desktop-macos` (M2 WA78).
- 01's About → Licenses opens `Contents/Resources/licenses/THIRD_PARTY_NOTICES.txt` (assumed, as on Windows).

## 9. CI

### 9.1 `ci.yml` changes

Filters:
- `desktop` gains `cmd/isshoni-desktop/build/darwin/**`, `deploy/macos/**`, `deploy/notices/native-darwin.json`,
  `deploy/keys/macos_codesign`, `deploy/keys/isshoni-codesign-*.pem`.
- `update` gains `internal/update/**` (already), `cmd/isshoni-desktop/oneshot*.go`, `deploy/test/macos/**`.
- new `sh`: `deploy/install-desktop.sh`, `deploy/test/install_desktop_unit.sh`, `deploy/test/macos/**`.
- new `homebrew`: `deploy/homebrew/**`.
- `repocheck`: `deploy/keys/macos_codesign`, `deploy/keys/isshoni-codesign-*.pem` (lint:keys).

| Job | Runs when | Runner | Steps and assertions | Timeout | Gates |
|---|---|---|---|---|---|
| `desktop-macos` (changed) | desktop, homebrew | `macos-26` (from `tools.json`) | Xcode select (§3). **First, M2's steps unchanged** (02 §6.7's proof that the untagged darwin build stays green): `go test -race` set without `imstatic`, the dev-flavor compile, `go-licenses`, 01's `tools/kccheck` step (KC). **Then the new imstatic app build**: `task desktop:app:macos VERSION=0.0.0-ci.<run> SIGN=ci` (`TAGS=production,imstatic`) with 02's `native:mac` (vcpkg binary cache, §9.2), `machocheck -thin -native native/dist`, `sign.sh -mode ci` and §5.5's checks, `--version` and `--self-check` (`ok`, `bundle.id` `io.isshoni.desktop`, `testHooks` false) under both `arch -arm64` and `arch -x86_64` (Rosetta installed first with `softwareupdate --install-rosetta --agree-to-license` when missing), `task desktop:package:macos` (tar.gz reproducibility run twice, DMG attach/diff/verify), the notices and native gate; `go build -tags imstatic ./cmd/isshoni-audiotest` (arm64, cgo, `MACOSX_DEPLOYMENT_TARGET=14.4`, the `-X …staticArchiveSHA` flag) and `isshoni-audiotest selfcheck`, its skipped TCC tests listed in the step summary (03 §17); the e2e variant `task desktop:app:macos VERSION=0.0.0-ci.<run> TAGS=e2e SIGN=ci OUT=dist-desktop/darwin_universal-test` (§4.8; no `machocheck -thin`); on `homebrew`: the cask render, `brew style`, `brew audit --cask --strict`; uploads `isshoni-desktop-macos-pr` (tar.gz, dmg, `ci-requirement.txt`; 7 days) and `isshoni-desktop-macos-e2e` (a tar of the e2e bundle, 7 days) for 05's WK spec job | 50 | yes |
| `macos-native` (02 §6.7; listed here because 04 owns the runner pins) | desktop | `tools.json` `runner` (`macos-26`) | the same Xcode-select step; 02 §6.7's steps 1–5 (native ctest, `imstatic,imtesthooks` Go tests, both arches' link check, lint) | 02's | yes |
| `macos-runtime` (new) | desktop | matrix `tools.json` `runtimeRunners`: `macos-14`, `macos-15`, `macos-15-intel` | needs `desktop-macos`; downloads the PR artifact; `tar -xzf`; §5.5 checks 1–3 against `ci-requirement.txt`; `--version` (`arch` reports `arm64` on `macos-14`/`macos-15`, `amd64` on `macos-15-intel`) and `--self-check` `ok`, failing on any dyld `Symbol not found` or `Library not loaded` in stderr (on `macos-14` this proves the 14.4 deployment target's weak linking); then `open` the bundle, `pgrep` sees it within 10 s, and it is ended with SIGTERM (proves LaunchServices accepts and launches the bundle on macOS 14, 15 and on Intel). Optionally the `macos-15-intel` leg also builds the x86_64 `isshoni-audiotest` and runs its `selfcheck`. When GitHub retires `macos-14`, that leg is dropped and the check moves to 05's 14.4 VM run at minor releases (05 §9.6) | 15 | yes |
| `macos-update` (new) | update, sh | `macos-26` | builds `0.0.1-ci.1` and `0.0.1-ci.2` with `TAGS=production,imstatic,updatetest SIGN=ci` and a third bundle `0.0.1-ci.3` signed by a **second** ephemeral identity; relserve over HTTPS with its CA trusted for the step (`sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain <ca>`, removed in the step's cleanup); `desktoppkg manifest -releases <base> -macos-archive …` for each, signed with an ephemeral update key; runs `deploy/test/macos/update-e2e.sh` (cases below), then `deploy/test/macos/install-e2e.sh` (§14) against `isshoni serve` (the CI binary, off mode on loopback) and relserve | 40 | yes |
| `lint-deploy` (changed) | + sh, homebrew | ubuntu-24.04 | `shellcheck -s sh deploy/install-desktop.sh deploy/macos/*.sh deploy/test/macos/*.sh`, the ASCII and LF check of `install-desktop.sh`, `deploy/test/install_desktop_unit.sh` under `dash` and `bash --posix`, `lint:keys` (codesign blocks), `lint:pins` (`tools.json`) | 10 | yes |
| `test-go` (changed) | go | ubuntu-24.04 | M2's, now including `untar.go`'s tests and `FuzzUntar` (30 s), `desktoppkg` `macver`/`bundle`/`machocheck` (against Mach-O fixtures a test writer builds, like M2's `pebuild_test.go`)/`macpack`/`cask`/manifest tests | as M2 | yes |
| `ci-ok` | always | | needs `macos-native`, `macos-runtime` and `macos-update` too (and 05's macOS jobs, 05 §5.3) | 2 | the only required check |

**Updater e2e cases** (`update-e2e.sh`; every bundle installed to `$HOME/Applications` by the e2e itself; the app uses
the real `~/Library/Application Support/isshoni`, since the hosted runner is ephemeral). `update-e2e.sh` and
`install-e2e.sh` refuse to run unless `GITHUB_ACTIONS=true` (exit 2, "runs only on GitHub's hosted runners: it installs
io.isshoni.desktop with a throwaway identity and uses the real data root"), so they never touch a developer's
release app, grants or data (D16):
- **A, ci.1 → ci.2**: `open -a ci.1 --args --update-now`; within 120 s `defaults read …/Info IsshoniVersion` =
  `0.0.1-ci.2`, `--version` agrees, no `.isshoni-update-*` folder is left, `ps` shows the relaunched process with
  `--updated-from=0.0.1-ci.1`, `codesign -R` passes, no quarantine xattr.
- **B, wrong signer**: a manifest for ci.3 (whose bundle the other ephemeral identity signed): the check ends in
  `error`/`codesign_mismatch` before any helper starts, ci.1 is untouched and the staging folder is removed.
- **C, hash mismatch** (`badsha/` root, M2's pattern): ci.1 untouched.
- **D, downgrade**: a manifest for `0.0.1-ci.0` is ignored.
- **E, read-only copy**: ci.1 run from a mounted DMG reports `not_installed` with reason `read_only`; no download
  request reaches relserve.
- **F, start during apply**: while a helper process holds `applying.lock` (a test helper), starting the app with
  `open -a … isshoni://smoke.invalid/r/abc` exits 0 within 10 s, with no second instance and no window. (The link is
  dropped by design, §2.4.)

Whether a re-signed N+1 reads the Keychain item without a prompt is 01's KC check (`tools/kccheck`), not a case here.

Every macOS job: actions pinned by SHA, `persist-credentials: false`, `contents: read`, no secrets. The jobs list every
skipped test with its reason in the step summary (M2's "every skip in one round" rule).

**How Windows CI proves Windows is unchanged**: `windows-go`, `windows-native`, `windows-desktop` and nightly's
`windows-full` run as before on any change to `internal/update`, `tools/desktoppkg` or `cmd/isshoni-desktop`; the
Windows updater e2e (M2 WA88) still writes its manifests with `-installer` alone, which is byte-identical to M2's golden
(§7.1); `manifest_test.go` checks that a Windows app selects the `nsis` entry from the two-OS manifest; M2's ps1, NSIS
and smoke files are untouched.

### 9.2 Caches

CI only (M2 06 §10.2): vcpkg's binary cache for `arm64-osx-isshoni`/`x64-osx-isshoni` (keyed by triplet, Xcode
version, the vcpkg manifests and `native/triplets/*.cmake`), Go per job; never in `release.yml` or the nightly.

### 9.3 `nightly.yml` additions

| Job | What |
|---|---|
| `macos-full` (new) | `desktop-macos` + `macos-update` without caches, at `0.0.0-nightly.<run>`; the release-shaped files (`tar.gz`, DMG, symbols zip, stamped `install-desktop.sh`) uploaded as `desktop-release-macos`; the cask installed from a local tap whose URL points at relserve (`brew install --cask isshoni-ci/test/isshoni`), then launched once and uninstalled with `--zap`; 90 min |
| `macos-runtime-full` (new) | `macos-runtime`'s matrix on `macos-full`'s bundle |
| `goreleaser-full` (changed) | needs `macos-full` too; `release:assets FULL=true DESKTOP=true MACOS=true` with real macOS files (stubs when `macos-full` uploaded none, failing at the end, M2's rule) |
| `go-cross-os` (changed) | its `macos-15` leg is replaced by `macos-26` running `desktop-macos`'s Go tests only |
| `macos-native-full` (new; 02 §6.7's nightly steps, 04 owns the file) | on `tools.json` `runner`, without caches: `mac-arm64-asan` and `mac-arm64-tsan` ctest; `mac-x64-ci` ctest and the Go tests built for amd64 (`-tags imstatic,imtesthooks`) run under Rosetta (`arch -x86_64`); 60 s `avcc_fuzz`; 90 min |

### 9.4 Dependabot

No new ecosystem. Xcode (`tools.json`) and the runner labels are hand-bumped with M1's monthly tool PR.

### 9.5 What needs TCC, and why CI never does

| Grant | Needed by | Runs on |
|---|---|---|
| System Audio Recording | 03's tap tests, the audiotest, 05's harness | the owner's Mac (and its OS VMs); enabled by `ISSHONI_TCC_AUDIO=1` and the ctest label `tcc` (03 §16.8), and in Go by 05's `internal/testenv/macenv` needs `tcc:audio` (and `tcc:mic` for the mic fixtures). Hosted runners never set them; CI lists the skips |
| Screen Recording | never the release app (02 §8.1, §5.4; `NSScreenCaptureUsageDescription` is kept only as a harmless string); used by `IsshoniEngineTest.app`, the harness apps and the `isshoni_hw` tests for listed `mon:` sources | the owner's Mac (02 §17.3) |
| Local Network | 05's M-MAC-16 | the owner's Mac and VMs |
| none | everything in §9.1 and §9.3 (bundle, signing, packaging, script, updater e2e, `--self-check`) | GitHub's hosted runners |

GitHub's macOS images have no way to pre-grant TCC (SIP is on and the TCC database is protected). Nothing in this
document's automated tests opens a picker, a tap or a LAN connection.

## 10. Release

### 10.1 `release.yml` extension

```
tag v* ─► version ─┬─► web ──┬──────────────────────────────────────────────────────────────┐
                   ├─► windows-native ─► (M2) desktop-exe ─► desktop-pkg ───────────┐         │
                   └─► macos-app ─► macos-sign ─► macos-pkg ─────────────────────────┴─► desktop-manifest ─► build ─► sign ─► verify ─────────┬─► publish ─► site
                                                                                                                       ├► verify-windows ─┤            └► homebrew (final only)
                                                                                                                       └► verify-macos ───┘
 macos-app ─► macos-sign-tools ─► macos-exit-tools (workflow artifact isshoni-exit-tools-macos; not on the publish path)
 dry run: macos-sign and macos-sign-tools with ephemeral identities; sign-dry; no publish, no homebrew
```

| Job | Runner, permissions | Steps | Timeout |
|---|---|---|---|
| `macos-app` (new) | `macos-26`, `contents: read`, no secrets | Xcode select; downloads `web-dist`; without cache, 02's `task native:mac PRESETS="mac-arm64-release mac-x64-release mac-arm64-ci mac-x64-ci" VERSION=<v> COMMIT=<sha>` (the release archives for the app and the harness tools, the test-hooks archives only for `isshoni-enginetest-hooks`); `task desktop:app:macos VERSION=<v> SIGN=none` (`TAGS=production,imstatic`); `machocheck -thin -native native/dist -requirement <generated update requirement>` (skipped requirement check in a dry run); notices + native gate; symbols zip; then the exit harness, unsigned: `task mac:harness EXIT=true VERSION=<v> SIGN=none OUT=dist-desktop/harness` (05's; it runs `go -C tools run ./macharness -exit -o dist-desktop/harness`, universal). Every harness tool is built with `-tags imstatic` against `native/dist/darwin-<arch>/` (the release archive, `testHooks` false, checked with `-native` like the app), except `isshoni-enginetest-hooks` (05 §4.1, §4.3: `-tags imstatic,imtesthooks`, the `-testhooks` archives), which exists only inside `isshoni Exit Harness.app` (`io.isshoni.exitharness`, never `io.isshoni.desktop`, never a release asset) and is recorded as `testHooks: true` in `exit-tools-manifest.json`. Dev-only test-hooks builds (02 §12, §13: `capture -software-vt`, local `avsync` runs) also come from `task native:enginetest-app` with `imtesthooks` on the owner's Mac. Packs the unsigned bundle and the harness with `COPYFILE_DISABLE=1 /usr/bin/tar -cf` (upload-artifact would drop execute bits) as `macos-unsigned` | 75 |
| `macos-sign` (new) | `macos-26`, `contents: read`, **`environment: release`** (owner approval), secrets `MACOS_CODESIGN_P12_BASE64`/`_PASSWORD` | checkout (sparse: `deploy/macos/sign.sh`, the app's entitlements, `deploy/keys/macos_codesign`; no `tools/macharness`); downloads `macos-unsigned`; extracts with `/usr/bin/tar`; `sign.sh -mode release -app …` (the app only; a dry run: `-mode ci`); §5.5's checks; re-packs the app as `macos-signed`. Runs no Go, no Node, no build tool, and never executes the bundle | 20 |
| `macos-sign-tools` (new) | `macos-26`, `contents: read`, **`environment: testtools`**, only the secrets `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` | needs `macos-app`; checkout (sparse: `deploy/macos/sign.sh`, `deploy/keys/isshoni-testtools-1.pem`, `tools/macharness/sign-list.txt` and every entitlements file the sign list names, under `cmd/isshoni-audiotest/macapp/` and `tools/macharness/`); downloads `macos-unsigned`; `sign.sh -mode testtools -harness dist-desktop/harness/"isshoni Exit Harness.app" -signlist tools/macharness/sign-list.txt` (a dry run: an ephemeral `ci-identity.sh -name testtools` identity with `-mode ci`); §5.5's checks 1–3 per sign-list line; packs `macos-signed-tools`. Runs no built code, no Go, no Node | 20 |
| `macos-pkg` (new) | `macos-26`, `contents: read`, no secrets | downloads `macos-signed`; §5.5's checks again; `--version`, `--self-check` (`ok`, `bundle.id`, `testHooks` false) under both archs; `desktoppkg macpack`; `make-dmg.sh` and the DMG check (§6.2); the stamped `install-desktop.sh` (`release:prepare`'s copy, compared); uploads `macos-release` (tar.gz, DMG, symbols zip, the script) and `macos-stage` (bundle plus thin arm64 binary for syft) | 20 |
| `macos-exit-tools` (new; content and checks are 05 §4.5's) | `macos-26`, `contents: read`, no secrets | needs `macos-sign-tools`; downloads the signed harness from `macos-signed-tools`; assembles 05 §4.5's layout (`isshoni Exit Harness.app`, `harness.sh`, `SHA256SUMS`, `exit-tools-manifest.json`, `README.txt` naming the run and tag); runs 05 §4.5's checks: `codesign --verify --strict` on the bundle and every nested bundle, `codesign -d -r-` shows `isshoni-testtools-1`'s leaf hash and no line carries the release identity's (in a dry run, the ephemeral test-tools identity's requirement), `shasum -a 256 -c SHA256SUMS`, `isshoni-audiotest selfcheck` and `isshoni-enginetest probe` exit 0 (executing built code is fine here: no secrets, unlike `macos-sign`, D12); uploads the workflow artifact **`isshoni-exit-tools-macos`** (90 days; not a release asset, like M2's `isshoni-exit-tools`) | 20 |
| `desktop-pkg` (changed) | as M2 | stops writing the manifest; everything else unchanged | as M2 |
| `desktop-manifest` (new) | ubuntu-24.04, `contents: read` | needs `desktop-pkg`, `macos-pkg`; `desktoppkg manifest -version <v> -installer … -macos-archive …`; uploads `desktop-manifest` | 10 |
| `build` (changed) | as M2 | also needs `desktop-manifest`; downloads `macos-release` into `dist-desktop/` and `macos-stage`; syft over the macOS stage → `isshoni-desktop_<v>_darwin_universal.sbom.json` (a test asserts it lists `github.com/wailsapp/wails/v3`); `release:assets … DESKTOP=true MACOS=true` (names; the manifest's darwin entry against the tar.gz; the tar.gz listing against the stage; `install-desktop.sh` stamped with `<v>` and with non-empty key and codesign blocks); attestations add the macOS files | as M2 |
| `sign`, `sign-dry` | unchanged | the manifest is still signed in `sign` (M2) | — |
| `verify-macos` (new) | matrix `macos-15`, `macos-26`; `contents: read`, `attestations: read` (`GH_TOKEN` on the attestation step only) | downloads `release-signed`; `ssh -V`; verifies `checksums.txt.sig` and the manifest signature with `/usr/bin/ssh-keygen` (a dry run: the dry-run keys); `gh attestation verify` the tar.gz and DMG; relserve with its CA trusted for the job; runs the **released, stamped** `install-desktop.sh` (a dry run replaces its key and codesign blocks) with `ISSHONI_DOWNLOAD_BASE`, `ISSHONI_NO_LAUNCH=1`, `--dir $HOME/Applications`; asserts §5.5's checks with the release requirement (a dry run: the ephemeral one), no `com.apple.quarantine`, `--version`, `--self-check` `ok`; the DMG attaches and its app equals the installed one; `spctl` rejects (recorded); then `--uninstall --purge` and asserts the bundle and §2.3's folders are gone | 25 |
| `publish` (changed) | as M2 | `needs` adds `verify-macos` (success required) | as M2 |
| `homebrew` (new) | ubuntu-24.04, `contents: read`, `environment: release`, secret `HOMEBREW_TAP_DEPLOY_KEY` | after `publish`, only when the version is final (no prerelease) and not a dry run; downloads the published `checksums.txt` and `.sig` from the release, verifies them with `deploy/keys/allowed_signers`, takes the tar.gz's SHA-256 from it, renders the cask with `deploy/homebrew/render.sh` (shell only; the job runs no Go, Node or build tool), clones the tap with the deploy key and `deploy/homebrew/known_hosts`, commits `Casks/isshoni.rb` ("isshoni <v>"), pushes to `main`. A failure doesn't affect the release; the job is re-runnable | 10 |

Rules kept: no caches in `release.yml`; actions pinned by SHA; one run per tag; a macOS-runner outage blocks a
release (D18; re-run, never skip).

### 10.2 `.goreleaser.yaml`

The globs `dist-desktop/isshoni-desktop*` and `dist-desktop/install-desktop.ps1` already cover the macOS files except
the script: `checksum.extra_files` and `release.extra_files` add `dist-desktop/install-desktop.sh`. `release:snapshot`
writes macOS stubs (`ISSHONI-DESKTOP-STUB`) when `dist-desktop/` has none, `FULL=true` refuses them (M2's rule).
Release notes header (§10.6).

### 10.3 `check-draft.sh`

`deploy/release/check-draft.sh` keeps its interface. Additions: when `isshoni-desktop-update.json` is present, every
`"name": "<file>"` line in it (the writer puts one field per line) must name an asset in `DIR` that `checksums.txt`
lists; and from the first M3 release on (a `deploy/desktop/macos-since` file with `0.3.0-rc.1`), a release at or above
it must carry the four macOS assets of §2.1 and `install-desktop.sh`. `check-draft-test.sh` gains: a draft without the
DMG fails; a manifest naming a tar.gz that isn't in the draft fails; an M2-shaped draft below `macos-since` passes.

### 10.4 Signing keys (M2 06 §11.5's table, updated)

| Key | Signs | Lives | Verified by |
|---|---|---|---|
| `isshoni-release-1` / backup | `checksums.txt` (now with the macOS files) | as M1 | `install.sh`, both desktop scripts, the site, users |
| `isshoni-update-1` / backup | the manifest (with its darwin entry) | as M2 | the apps |
| **`isshoni-codesign-1`** (RSA-3072, self-signed) | the `.app` and the harness bundles | `release` environment (`.p12` + password), offline copy | macOS (TCC, Keychain, `codesign -R`), the updater, `install-desktop.sh`, `verify-macos`, users (`/security#verify-desktop-macos`) |
| **`isshoni-codesign-backup-1`** | nothing until a leak (§5.8) | offline only | the updater, the script |
| `HOMEBREW_TAP_DEPLOY_KEY` (ed25519) | pushes to the tap | `release` environment | GitHub |

### 10.5 Versioning and compatibility

As M2 06 §11.6, with `hello.client {kind: desktop, version: <v>, os: macos}`. The first macOS release supports servers
with `deviceFlow` (0.2.x and later; minor skew ≤ 1). A macOS app reaching a 0.1.x server shows M2 02's `update_server`
screen like Windows.

### 10.6 Release notes header additions

```
**Mac app:** install it from your isshoni server's Download page, with `curl -fsSL https://<your server>/install-desktop.sh | sh`
(no warning), or `brew install --cask moonwx/isshoni/isshoni`. The DMG (isshoni-desktop_{{ .Version }}_darwin_universal.dmg)
is signed with the project's own certificate, not by Apple: macOS asks you to approve it once in System Settings →
Privacy & Security → Open Anyway; see https://moonwx.github.io/isshoni/download#macos-open-anyway. macOS 14.4 or later;
Intel Macs are best effort.
```

The M2 line about `isshoni_*_darwin_*.tar.gz` being experimental **server** builds stays and now also says "the Mac
app is isshoni-desktop_…".

### 10.7 The first release with the macOS app (`v0.3.0-rc.N`, then `v0.3.0`)

Assumed version: M3's minor `0.3.0` (if M2's `0.2.0` hasn't shipped by then, both apps ship in the same tag and both
checklists apply). Joins M2 06 §11.8's list; (owner) items need the Mac, its VMs or a GitHub setting.

1. (owner) §12 done: both identities created, `macos_codesign` and the `.pem`s merged, the secrets set; the tap
   repository and its deploy key; the macOS VMs of 05 §3.2 ready.
2. CI and the last nightly green, including `macos-full`, `macos-runtime-full` and `goreleaser-full` with real macOS
   files; the dry run green including `verify-macos`.
3. Tag `v0.3.0-rc.1`; approve (`release` environment, twice: `macos-sign` and `sign`).
4. (owner) **Install paths on 14.4, 15, 26 and 27** (05's M-MAC-1, M-MAC-2, M-MAC-3, M-MAC-23; the pass criteria below
   are what those rows cite): (a) the server's terminal command, copied from `/download?room=<id>`: no dialog, the app
   opens with "Add server {host}?" for that server and then shows the room, with no URL typed; as a standard user with
   an admin's copy in `/Applications`, the copy lands in `~/Applications` (the note printed) and an `isshoni://` link
   opens that copy; run again with the app already installed (and once while it runs), it prints "already installed" and
   the app opens the room; (b) the DMG from Safari and from Chrome: the Open Anyway path of §5.9 exactly as the docs
   show it, then the app opens; (c) Homebrew from the real tap (after rc: the final only; for rc a local tap with the
   rc's URL): opens without a dialog, or with exactly the documented Open Anyway path once (recorded, §6.6); (d) running
   from the mounted DMG shows "Move isshoni to Applications".
5. (owner) **Grants across an update** on each OS (05's M-MAC-8, M-MAC-9, M-MAC-16): grant System Audio Recording and
   Local Network (15+), link a server, then update rc.1 → rc.2 through the updater; no prompt for any grant or the
   Keychain, no Screen Recording prompt during a picker share, the sound check passes, a LAN server connects. A
   re-prompt on any OS is a release blocker until §13's contingency is applied or the owner accepts it in the release
   notes.
6. (owner) **Update handover** (M-MAC-8's added steps): during the rc.1 → rc.2 update from a room link, the relaunched
   app opens the room; the Dock item, the login item and `isshoni://` links still work; `helper.log` shows one apply.
7. (owner) **App Management** (05's M-MAC-22): on 15, 26 and 27, an update of a copy installed through the DMG + Open
   Anyway path and one installed by the script; and the script updating over an existing copy. Record whether App
   Management blocked anything (§13).
8. (owner) the exit run: 05 §2's exit criterion (A, L-1/L-2 on 27, L-F on the VMs, C) on the rc's own
   `isshoni-exit-tools-macos`.
9. (owner) Uninstall paths of §6.7 (keep data, then purge): the device disappears from the server's Devices page,
   Keychain Access shows no isshoni item, `tccutil`-reset grants re-prompt on reinstall.
10. Site and docs merged (§11), README keys table with both codesign fingerprints.
11. Tag `v0.3.0`; approve; publish; the `homebrew` job pushes the cask; `brew install --cask moonwx/isshoni/isshoni`
    on the Mac installs it and it opens without a dialog, or with exactly the documented Open Anyway path once
    (recorded; §6.6).

**Every later release**: the smoke on the newest macOS (install or update, one share with Discord running, a browser
viewer) and the audiotest (PLAN tiers); **every minor**: the full 14.4–27 matrix of item 4–5 (05).

## 11. Project site and docs

| Page | Change |
|---|---|
| `docs/download.md` | `#macos` (requirements: macOS 14.4+, Apple silicon, Intel best effort; three ways, terminal first), `#macos-terminal` (the command, what it checks, where it installs, no sudo, options and exit codes), `#macos-open-anyway` (§5.9 for 14 and for 15+, screenshots added by the owner), `#macos-homebrew` (command, `auto_updates`, uninstall caveat), `#macos-verify` (link to `/security#verify-desktop-macos`), `#macos-updates` (how the in-app update works, channels, turning off, the MDM key, "Move to Applications"), `#macos-uninstall` (§6.7), `#macos-permissions` (System Audio Recording and Local Network: what each is for, where to change it; that the app never asks for Screen Recording because the macOS picker is the consent; 01 links here). It shows the DMG's and tarball's SHA-256 from `release.json` |
| `docs/privacy.md` `#desktop` | a macOS paragraph: data under 01's data root and `~/Library/{Caches,WebKit,HTTPStorages}/io.isshoni.desktop`; refresh tokens in the login keychain; connections as Windows (GitHub update checks, linked servers) minus Cisco/WebView2, plus Apple's own Gatekeeper/XProtect checks of downloads; no telemetry |
| `docs/code-signing.md` | macOS row: "self-signed, not notarized (no paid Apple account yet); identity `isshoni-codesign-1`, SHA-256 `<…>`; designated requirement …; why grants persist; what changes when Developer ID comes" |
| `docs/security.md` | `#keys` gains both codesign certificates and their fingerprints, served at `/keys/isshoni-codesign-1.pem` and `/keys/isshoni-codesign-backup-1.pem`; `#verify-desktop-macos` (below); the leak plan of §5.8 in one paragraph |
| `docs/troubleshooting.md` | `#macos-app-management`, `#macos-permissions-reset` (`tccutil reset All io.isshoni.desktop`, then re-grant), `#macos-move-to-applications`, `#macos-intel` (best effort, how to report), `#macos-update-failed` |
| root `README.md` | the Mac app in Install; codesign fingerprints in "Release signing keys" |
| `site.yml` | serves `/keys/isshoni-codesign-*.pem` (checked against main like `allowed_signers`); `release.json`'s `desktop` gains `macos: {"dmg": "<name>", "dmgSha256": "<hex>", "archive": "<name>", "archiveSha256": "<hex>"}` (null for a release below `deploy/desktop/macos-since`) and the top level `codesignFingerprints: ["SHA256:…", …]`; a release ≥ `macos-since` whose signed `checksums.txt` lacks either file exactly once fails the build |
| `internal/server/ops/doctor/docsanchors_test.go` | every anchor above joins `TestDocsAnchors` |

`#verify-desktop-macos`:

```sh
V=0.3.0; B=https://github.com/MoonWX/isshoni/releases/download/v$V; F=isshoni-desktop_${V}_darwin_universal.dmg
curl -fsSLO "$B/$F" -O "$B/checksums.txt" -O "$B/checksums.txt.sig"
curl -fsSL https://moonwx.github.io/isshoni/keys/allowed_signers -o allowed_signers
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
grep " $F\$" checksums.txt | shasum -a 256 -c -
codesign -v -R='=identifier "io.isshoni.desktop" and certificate leaf = H"<sha1>"' /Applications/isshoni.app && echo signed-by-isshoni
```

## 12. Owner actions

Before the first rc with the Mac app:
1. **Codesign identities** (§5.1) on the Mac, outside the repository; the PR with the `.pem`s, `macos_codesign` and
   `task release:codesign-keys`'s output; the secrets `MACOS_CODESIGN_P12_BASE64` and `MACOS_CODESIGN_P12_PASSWORD` in
   the `release` environment; the `.p12`s and passwords in the password manager plus an encrypted offline copy.
1a. **Test-tools identity** `isshoni-testtools-1` (§5.1, no backup): its `.pem` in the same PR as
   `deploy/keys/isshoni-testtools-1.pem`; a `testtools` environment (owner approval, `v*` tags only) holding only
   `MACOS_TESTTOOLS_P12_BASE64` and `MACOS_TESTTOOLS_P12_PASSWORD`; the `.p12` in the password manager.
2. **The tap**: create the public repository `MoonWX/homebrew-isshoni` (README, `Casks/` with a `.gitkeep`, license
   Apache-2.0); generate `ssh-keygen -t ed25519 -N '' -C isshoni-homebrew-tap -f tap-key`; add `tap-key.pub` as a
   deploy key **with write access** to that repository only; put `tap-key` into `HOMEBREW_TAP_DEPLOY_KEY` in the
   `release` environment; delete the local copy.
3. **The macOS VMs of 05 §3.2** (Tart golden images for 14.4, 15, 26, each with an admin and a standard account); 27
   is the Mac itself (05 owns the matrix and the VMs).
4. Review that the `release` environment still requires the owner's approval and allows only `v*` tags (M1).
5. After the first final release, the screenshots for `#macos-open-anyway` (14 and 15+).

## 13. Main risks

| Risk | Mitigation |
|---|---|
| **Local Network is re-asked after each update on macOS 15/26** (reports that 15 keys apps by `LC_UUID`) | 05's M-MAC-16 and M-MAC-8 on 15 and 26 before the first release. Contingency (owner's choice, §18 Q1): give each slice a constant `LC_UUID` derived from the bundle id and arch (written before signing by `desktoppkg`), at the cost of matching crash logs to the symbols zip by version instead of UUID |
| **App Management blocks the swap** (macOS 13+ protects apps from modification by other code; the rule for apps without a Team ID is undocumented) | the helper is the app's own executable and its responsible process is the app itself; 05's M-MAC-22 measures it on every OS; on `EPERM` the update fails with `app_management`, nothing is half-replaced, and 01's guided fix explains the App Management toggle or the terminal command |
| Apple tightens Gatekeeper for self-signed apps (PLAN risk) | the curl, Homebrew and in-app paths never set quarantine; the browser sharer stays; the switch to Developer ID is a certificate and requirement change (§5.8) |
| The codesign key leaks: malware inherits capture grants and reads tokens | `release` environment with approval; the key never in a job that runs build code (D12); backup identity and the §5.8 path; published fingerprints |
| Keychain ACL prompts after an update | D5 and the explicit requirement; 01's KC check (`tools/kccheck`) on CI with the 0600-file fallback; 05's M-MAC-9 |
| Hosted macOS runner images change (Xcode removed, label retired) | `tools.json` pins Xcode exactly with a fail-fast step that lists what's there; the fallback runner is named (§3) |
| Intel slice regressions nobody sees | CI runs it natively on `macos-15-intel` and under Rosetta; docs say best effort; per-arch manifest entries allow dropping it in a minor release with a notice |
| `strip` breaks a Go binary | step 6's `--version` run catches it; fallback ships unstripped slices |
| hdiutil flakiness on hosted runners | three attempts; the DMG isn't on the updater's path |
| Homebrew forbids `postflight` quarantine removal | brew users get the one-time Open Anyway; docs |
| Released M2 Windows apps reject the new manifest | D10's analysis plus the two-OS vector in `manifest_test.go`; the owner's rc.1 → rc.2 Windows update in M2 06 §11.8 item 8 repeated for 0.3.0 |

## 14. Test plan

| Level | Test | Asserts | Where |
|---|---|---|---|
| Unit | `internal/update` `manifest_test.go` | the two-OS manifest: darwin selects `universal` (and a same-arch entry first when both exist), Windows selects `nsis`; the `darwin-app` name rule; M2's vectors unchanged | CI ubuntu, macos, windows; Mac |
| Unit | `untar_test.go`, `FuzzUntar` | regular files and dirs only; symlink, hard link, device, `..`, absolute path, an entry outside `isshoni.app/`, too many entries, too many bytes, a truncated gzip → `bad_archive` with nothing left outside the staging folder; modes masked; `O_EXCL` | CI ubuntu (all OS) |
| Unit | `apply_darwin_test.go`, `helper_darwin_test.go` | `CurrentLocation` reasons with fixture bundles (a symlinked exe, `/AppTranslocation/`, a read-only `hdiutil` image, a non-writable folder, a dev bundle id); `Apply` writes the request and returns when a fake helper holds the lock, WARN after 10 s otherwise; `attempts` incremented; the helper: waits for a child test process to exit, swaps with `RENAME_SWAP`, removes the staging folder, records `app_management` on an `EPERM` injected through a fake `renamex`, relaunches through an injected `open`; refuses a request whose `bundle` isn't its own or whose `staged` is elsewhere, and a request file outside `updates/` or not 0600; **downgrade**: a staged bundle whose version equals or is older than the running one, and one whose version differs from `to`, are each refused with `downgrade` in `pending.lastError`, nothing swapped, the staging folder removed; **post-swap check**: an injected hook replaces the staged bundle (with one signed by another identity) between the verify and the rename, and the helper reverts the swap, records `codesign_mismatch`, leaves the old bundle at the installed path and relaunches it; the staging folder is created 0700; `CleanupStaging` keeps a locked staging folder | CI macos; Mac |
| Unit | `codesign_darwin_test.go` | with an ephemeral identity made by the test (skipped with a note when `security` can't make a keychain): a bundle signed by it passes its requirement, a bundle signed by a second identity fails, a modified resource fails | CI macos; Mac |
| Unit | `policy_darwin_test.go` | the forced-value check through an injected CFPreferences reader | CI macos |
| Unit | `tools/desktoppkg` | `macver` table (incl. the Z bound); `bundle` Info.plist golden for release, dev and e2e (with `CFBundleURLTypes` only for release, `LSUIElement` and `NSQuitAlwaysKeepsWindows` in all, `NSAllowsLocalNetworking` only for dev/e2e), with and without the optional LaunchAgents plist, modes, no `.DS_Store`; `machocheck` against synthetic Mach-O fat/thin fixtures (missing slice, minos 14.0, an `@rpath` load, `LC_RPATH`, a Homebrew dylib path, a missing `LC_UUID`, test strings, an extra Mach-O in Resources, tags `production` without `imstatic`, a missing or wrong `staticArchiveSHA`, no `_im_start` symbol) and `build.json` fixtures for `-native` (SDK 15.5, `minOS` 14.0, `testHooks` true); `macpack` byte-reproducibility and refusal of symlinks/xattrs; `render.sh` golden; `manifest` with `-macos-archive` golden and M2's `-installer`-only golden unchanged | CI ubuntu; Mac |
| Unit | `tools/repocheck keys` | the codesign `.pem` rules (RSA ≥ 3072, extensions, validity, self-signed), fingerprints vs `macos_codesign`, exactly one active and one backup, the generated blocks in `signers.go` and `install-desktop.sh`, README rows; `role=` lines after a rotation | CI ubuntu; Mac |
| Unit | `deploy/test/install_desktop_unit.sh` (sources the script with `ISSHONI_INSTALL_SOURCED=1`, as M1's `install_unit.sh`) | version regex and `version_cmp` (rc < final); destination rule (`/Applications` writable or not, existing copies in both, `--dir`); the checksums-line lookup; the tar listing filter (`..`, absolute, outside `isshoni.app/`); the server-origin rule for the deep link; `--room`: a valid id gives `isshoni://<host>/r/<id>`, an invalid one exits 2; an admin's non-writable `/Applications/isshoni.app` → destination `~/Applications`, the note, and the `lsregister -f` call (stubbed); EACCES at staging or swap (stubbed `mv`/`mkdir`) → exit 9; already installed with no explicit version, same and newer installed versions, with the app running and not running → "already installed", no download, step 11's `open` called, exit 0; the same with `--version` → exits 6/7 as before; the Linux line and exit 5; `id -u` 0 → exit 2; no `sudo` string in the script | CI ubuntu (`dash`, `bash --posix`), macos (`/bin/sh`) |
| Unit | `internal/server/httpapi` `desktopscripts_test.go` | `RenderInstallSH`: `'`, `\`, spaces or non-ASCII in the origin → empty server; http non-loopback → empty; dev versions empty; rc kept; marker errors; the route 200 with headers and `HEAD`; nil → 404; `TestCheckDesktopScripts` covers both scripts | CI ubuntu; Mac |
| Unit | `web/src/download/*.test.ts(x)` | exact DMG URL, `shCommand` (no room, a valid room with the `-s -- --room` suffix, an invalid room without it), `brewCommand`; the Open isshoni step enabled by the Copy click; macOS first on a Mac UA, not on an iPad UA; Windows block unchanged | CI ubuntu; Mac |
| Unit | `check-draft-test.sh` | §10.3's cases | CI ubuntu; Mac |
| Static | shellcheck, actionlint, `brew style`/`brew audit --cask --strict`, `plutil -lint`, `lint:keys`, `lint:pins` | | CI ubuntu, macos |
| Integration | `desktop-macos` | the universal app builds, `machocheck` passes, signs with an ephemeral identity, §5.5's checks, both slices' `--version`/`--self-check`, tar.gz reproducible, DMG matches | CI macos-26 |
| Integration | `macos-runtime` | the bundle runs and launches through LaunchServices on macOS 15 (arm64) and natively on Intel | CI macos-15, macos-15-intel |
| Integration | `macos-update` updater e2e | cases A–F of §9.1 | CI macos-26 |
| Integration | `install-e2e.sh` | `isshoni serve` renders the script for its origin; with `ISSHONI_DOWNLOAD_BASE` at relserve (CI bundle, ephemeral release key, blocks replaced) it installs to `$HOME/Applications` without quarantine and passes `codesign -R`; re-run with the same version reinstalls; an older version exits 7 unless `ISSHONI_ALLOW_DOWNGRADE=1`; a tampered archive, a `checksums.txt` signed by another key, and an archive signed by the second ephemeral identity each exit 3 with nothing changed; a running copy exits 6 with an explicit `--version` (and without one, the same version prints "already installed", exits 0 and the running app logs the forwarded link); `--uninstall --purge` removes everything of §2.3; `--no-launch` starts nothing; with launch and `--room <id>`, `ps` shows the app started by `open` with the deep link `isshoni://<host>/r/<id>` delivered (01's log line) | CI macos-26 |
| E2E | release dry run | `macos-app → macos-sign (ephemeral) → macos-pkg → desktop-manifest → build → sign-dry → verify-macos` green on macos-15 and macos-26 | GitHub |
| E2E | nightly `macos-full`, `macos-runtime-full`, `goreleaser-full` | §9.3, including the cask from a local tap | GitHub |
| E2E | Playwright `download.spec` | the macOS block on a Mac UA (`page.emulate` UA + platform), the terminal command with the server's origin, `GET /install-desktop.sh` pre-filled; axe in both colour schemes | CI ubuntu; Mac |
| Manual | **05's M-MAC-8, M-MAC-9, M-MAC-16** (grants) | pass criteria: on 14.4, 15, 26, 27, after granting once, an updater update N → N+1 (rc.1 → rc.2) asks for no TCC grant and no Keychain access, and no Screen Recording prompt appears during a picker share; the sound check passes, a LAN server connects, a share starts | owner's Mac + VMs |
| Manual | **M-MAC-8's added steps** (update handover) | pass criteria: the link known before the update is opened after it, Dock item, login item, `isshoni://` still work; `helper.log` one apply; a share live blocks the apply (01's rule) | owner's Mac |
| Manual | **05's M-MAC-22** (App Management) | pass criteria: §10.7 item 7 | owner's Mac + VMs |
| Manual | **05's M-MAC-1, M-MAC-2, M-MAC-3, M-MAC-23** (install) | pass criteria: §10.7 item 4 (curl, DMG + Open Anyway in Safari and Chrome, Homebrew, run-from-DMG shows "Move isshoni to Applications") and item 9 (uninstall) | owner's Mac + VMs |
| Manual | exit run | 05 §2's exit criterion on `isshoni-exit-tools-macos` and the rc bundle | owner's Mac (M5, macOS 27) and its 14.4/15/26 VMs, by hand per 05 §9 (05's optional self-hosted runner can run only exit-audio) |

Everything automated runs on the Mac too (`task test`, `task lint`, `task desktop:app:macos SIGN=ci`) except
`update-e2e.sh` and `install-e2e.sh`, which install the release id with a throwaway identity and use the real data root,
so they refuse to run outside GitHub's hosted runners (§9.1, D16); only the manual rows need the owner, and none of the
automated rows needs a TCC grant (§9.5).

## 15. Interfaces other docs rely on

- **Asset names** (§2.1) and URL patterns; the manifest's darwin entry and selection rule (§7.1).
- **Bundle**: id `io.isshoni.desktop` (dev `io.isshoni.desktop.dev`, e2e `io.isshoni.desktop.e2e`), executable
  `Contents/MacOS/isshoni`, scheme `isshoni` in production only (dev and e2e declare none), Info.plist keys of §4.1
  (incl. `IsshoniVersion`, `LSUIElement` true, `NSQuitAlwaysKeepsWindows` false; 01 owns the usage-string wording),
  entitlements of §4.2 (no others allowed), no embedded code besides the executable (§2.2; the optional
  `Contents/Library/LaunchAgents/io.isshoni.desktop.autostart.plist` only if 01's WM25 fails),
  `Resources/licenses/THIRD_PARTY_NOTICES.txt`.
- **Install locations** (`/Applications` or `~/Applications`, §6.3), `updates/` in 01's data root, the
  `UpdateChecks` preference, the MDM key `DisableUpdateChecks` (§2.3, §7.4).
- **Process contract** (§2.4): `--apply-update=<request>` (one-shot, dispatched by 01 to `update.RunApplyHelper`),
  `applying.lock` (01's start step exits 0 within 10 s while it is held, without a window; a launching link is
  dropped), no start-time apply on darwin (§7.3), `--self-check` without TCC and without a hardware encoder plus
  `bundle.id` and `menu`, `--version` with `arch`, SIGTERM = Quit, launches through `open` for installers, the updater
  and `task desktop:run:macos` (direct exec only for `--version`, `--self-check` and e2e/CI flavors).
- **`internal/update`**: `CurrentLocation`/`Location` and the reasons, `Status.Reason`, `RunApplyHelper`,
  `CleanupStaging`, `ApplyHelperFlag`, the new codes `bad_archive`, `codesign_mismatch`, `app_management`, `downgrade`,
  `PolicyDisabled` on darwin, `ISSHONI_UPDATE_TEST_CODESIGN_REQUIREMENT`.
- **Signing**: the bundle requirement and update requirement texts (§5.3), `deploy/keys/macos_codesign`, the
  `.pem`s, `deploy/macos/sign.sh` (`-mode release|ci|dev`, `-app`, `-tool <bundle> -tool-id <id> -tool-entitlements
  <file>` for flat bundles such as 02's `IsshoniEngineTest.app`, `-harness <bundle> -signlist <file>` with the
  committed `tools/macharness/sign-list.txt` for 05's harness), `-mode testtools` with the test-tools identity
  `isshoni-testtools-1` (`deploy/keys/isshoni-testtools-1.pem`, secrets `MACOS_TESTTOOLS_P12_BASE64`/`_PASSWORD` in
  the `testtools` environment) for 05's Exit Harness only, the ephemeral identity script (`-name <n>`) and
  `ci-requirement.txt`, the dev identity script (creates or locates the identity only).
- **Build tasks**: `task desktop:app:macos VERSION= [TAGS=production,imstatic] [SIGN=] [OUT=]`, `task
  desktop:package:macos`, `task desktop:run:macos`, `task release:codesign-keys`; `deploy/macos/tools.json`; `desktoppkg
  macver | bundle | machocheck [-thin] [-native] | macpack | manifest -macos-archive`; `deploy/homebrew/render.sh`;
  `tools.json` `runner`, `runtimeRunners`, `testRunners`.
- **Server**: `GET /install-desktop.sh` (pre-filled, text/plain; charset=utf-8), `deploy.InstallDesktopSH`,
  `DesktopScripts.SH`, `httpapi.RenderInstallSH`.
- **SPA**: `desktopLinks()`'s `macDmgUrl`, `shCommand` (with ` -s -- --room <id>` when `validRoomId`), `brewCommand`;
  `account.download.macos.*`.
- **`install-desktop.sh` CLI**: `--room ID` (opens `isshoni://<host>/r/<id>`), the "already installed" exit 0 without
  an explicit version, exit codes 0–9 of §6.3 (9 = EACCES on the destination) for 05's M-MAC-1 and the docs.
- **CI**: jobs `desktop-macos`, `macos-runtime`, `macos-update` (and 02's `macos-native` on the pinned runner, in
  `ci-ok`); release jobs `macos-app`, `macos-sign` (the app only), `macos-sign-tools` (the exit harness),
  `macos-pkg`, `macos-exit-tools`, `desktop-manifest`,
  `verify-macos`, `homebrew`; nightly `macos-full`, `macos-runtime-full`, `macos-native-full`; the artifacts
  `isshoni-desktop-macos-pr`, `isshoni-desktop-macos-e2e` (the `-tags e2e` bundle for 05's WK spec) and
  **`isshoni-exit-tools-macos`** (05 §4.5's layout, from the tag's own build).
- **Site anchors**: `/download#macos`, `#macos-terminal`, `#macos-open-anyway`, `#macos-homebrew`, `#macos-verify`,
  `#macos-updates`, `#macos-uninstall`, `#macos-permissions`, `/security#verify-desktop-macos`,
  `/troubleshooting#macos-app-management`, `#macos-permissions-reset`, `#macos-move-to-applications`, `#macos-intel`,
  `#macos-update-failed` (all in `TestDocsAnchors`).
- **Env names** for the server's reserved list (`internal/server/config/keys.go` `reservedEnv` and `devEnvNames`, in
  the same PR that reads them): `ISSHONI_INSTALL_DIR`, `ISSHONI_UPDATE_TEST_CODESIGN_REQUIREMENT`,
  `ISSHONI_DEV_SIGNING_DIR`, `ISSHONI_PREFILL_SERVER`, `ISSHONI_PREFILL_VERSION` (script-internal, reserved so a
  server started from a shell that sourced the script doesn't warn).

## 16. Depends on

**From `01-desktop-macos.md`**
- The darwin flavor of `cmd/isshoni-desktop` for releases: the data root (from `NSFileManager`, assumed
  `~/Library/Application Support/isshoni/`; never from `$HOME`, `TestDataRootNotFromEnv`), the Keychain store in the
  **login keychain without access groups** (D5) and its KC check (`tools/kccheck`, with the 0600 `credentials.json`
  fallback), the pending-link store and the start step that probes `applying.lock` (§2.4; a launching link is dropped),
  start step 5 a no-op on darwin (§7.3), `--apply-update` in the one-shot table, calling `update.CleanupStaging` at
  start, `--version`'s `arch`, `--self-check`'s darwin rule with the `bundle` and `menu` fields, SIGTERM = Quit,
  `UpdateChecks` read once as the initial `updates.check`, the updater UI incl. `Status.Reason` texts and the three new
  error codes, "Move isshoni to Applications", the `SMAppService` login item unregistered by `--purge-data`, About →
  Licenses opening the bundle's notices, the menu-bar template icons, the URL Apple Event handling, `LSUIElement` /
  `NSQuitAlwaysKeepsWindows` (D4, §4.4), the usage-string wording, the e2e flavor's id `io.isshoni.desktop.e2e`, dev and
  e2e bundles without a URL scheme, and the WM25 fallback plist if needed (§2.2).
- Whether 01 needs `NSBonjourServices` (it adds the key in its slice). Screen Recording is never requested (01 §9.1).

**From `02-macos-engine.md`**
- `task native:mac PRESETS="mac-arm64-release mac-x64-release" VERSION=<v> COMMIT=<sha>` (02 §6.5, with `VERSION` and
  `COMMIT` feeding `IM_VERSION`/`IM_COMMIT`), writing `native/dist/darwin-{arm64,amd64}/libisshoni_media.a` with
  `archive.sha256` and `build.json` (`sdk`, `minOS`, `testHooks`), built with deployment target 14.4 (presets and the
  overlay triplets `arm64-osx-isshoni`/`x64-osx-isshoni`) and the macOS 26 SDK, linking only Apple frameworks; the
  `-ci` presets' `native/dist/darwin-{arm64,amd64}-testhooks/` for the exit harness's `isshoni-enginetest-hooks`; the
  vcpkg copyright files under `native/dist/<dir>/licenses/vcpkg/<port>/copyright` for the native gate; Go tags
  `production,imstatic` and `link_darwin_{arm64,amd64}.go` carrying the archive path and frameworks (no `cgo.env`);
  `-X …native.staticArchiveSHA` (02 §6.6); `im_probe(quick)` that touches no TCC-protected API and needs no hardware
  encoder; the `libopus` and `speexdsp` entries in `native-darwin.json`; the `macos-native` job and the nightly steps of
  02 §6.7 (04 places them, §9.1, §9.3); `IsshoniEngineTest.app` (`io.isshoni.enginetest`, from `task
  native:enginetest-app`), a harness bundle signed with `sign.sh -tool` and its entitlements file
  `native/macos/tools/enginetest-app/enginetest.entitlements`.

**From `03-macos-audio.md` and `05-exit-and-verification.md`**
- 05 §4.1: the bundles `isshoni Exit Harness.app` (`io.isshoni.exitharness`) and the dev harness `isshoni Dev
  Harness.app` (`io.isshoni.dev.harness`), built unsigned by 05's `tools/macharness` (`task mac:harness EXIT=true
  VERSION=<v> SIGN=none OUT=<dir>`), and its committed sign list `tools/macharness/sign-list.txt` (§5.5); the nested
  ids `io.isshoni.test.*` (03's fixtures, 05's `io.isshoni.test.avsync.flash`/`.click`); 05 §4.5's artifact layout and
  checks for `macos-exit-tools`.
- 03 §16.3: the fixture bundle ids, entitlements (under `cmd/isshoni-audiotest/macapp/`) and usage strings; 03 §17's
  `isshoni-audiotest selfcheck` on CI.
- The TCC switches: `ISSHONI_TCC_AUDIO=1` and the ctest label `tcc` (03 §16.8), 05's `internal/testenv/macenv`
  (`tcc:audio`, `tcc:mic`).
- 05's OS matrix (05 §3, §7) and VMs (05 §3.2); the manual rows M-MAC-1, 2, 3, 8, 9, 16, 22, 23 that cite §10.7 and §14
  as their pass criteria (M-MAC-22 App Management and M-MAC-23 run-from-DMG are rows 05 adds for this document).

**From M2 (as built)**
- `internal/update` (M2 06 §8), `tools/desktoppkg`, `tools/notices`, `tools/repocheck keys`, `tools/relserve`,
  `deploy/embed.go` and `desktopscripts.go`, `web/src/download/`, `check-draft.sh`, `release.yml`'s jobs and the
  `release` environment, `deploy/desktop/min-version`, the update keys; M2 02's `deeplink`/`appLinkFor`, M2 02's
  `ForgetAll`.

**From M1**
- `install.sh`'s functions and conventions (copied, §6.3), `deploy/keys/allowed_signers`, `release:keys`, the site
  pipeline and `release.json`, `TestDocsAnchors`, `reservedEnv`.

## 17. Implementation slices

Days are rough, for one developer, on the Mac unless noted (about 17 days, much parallel to 01–03).

| # | Slice | Scope | Acceptance | Depends on | Verified |
|---|---|---|---|---|---|
| MR1 | Toolchain and bundle skeleton (1.5 d) | `deploy/macos/tools.json`, the Xcode step, `lint:pins` additions; `cmd/isshoni-desktop/build/darwin/{Info.plist.tmpl,isshoni.entitlements,isshoni-dev.entitlements,AppIcon.icns}`; `desktoppkg macver`, `bundle`, `machocheck`; `task desktop:app:macos SIGN=none` built **without the `imstatic` tag** (fake/unsupported engine, the `-test` output folder); the PLAN write-back of D14 (`macos-26` for the native job) | a universal bundle builds on the Mac and on `macos-26`; `machocheck` passes and its negative fixtures fail; `--version` on both slices (Rosetta); Info.plist golden (release, dev, e2e) | M2 app skeleton | Mac; CI macos, ubuntu |
| MR2 | Signing (1.5 d) | `deploy/macos/{sign.sh,ci-identity.sh,dev-signing.sh}` (`-mode testtools`, `ci-identity.sh -name`), `SIGN=ci|dev`, §5.5's checks, `desktop-macos` extended; the dev bundle id variant | an ephemeral identity signs on `macos-26` with the explicit requirement; every §5.5 check passes and `spctl` rejects; `SIGN=dev` rebuild keeps the owner's dev System Audio Recording grant (S2 repeated with the real app id `.dev`) | MR1 | CI macos; Mac |
| MR3 | Keys and requirement blocks (1 d) | `deploy/keys/macos_codesign`, `task release:codesign-keys`, `repocheck keys` extension, `signers.go` block, README rows | `lint:keys` fails on a drifted block, a 2048-bit key, a missing backup, a CA:true certificate, a `testtools` line in `macos_codesign`; checks `isshoni-testtools-1.pem` outside the active/backup pair; passes on test fixtures; the owner's real identities merged (owner actions 1 and 1a) | MR2 | CI ubuntu; Mac |
| MR4 | Packaging (1.5 d) | `desktoppkg macpack`, `deploy/macos/make-dmg.sh`, the `.webloc`, `task desktop:package:macos` | tar.gz byte-reproducible; DMG attaches, its app equals the bundle, §5.5 checks on the mounted copy | MR2 | CI macos; Mac |
| MR5 | Real engine in the build (1 d) | consuming 02's `task native:mac PRESETS="mac-arm64-release mac-x64-release"` and `-tags production,imstatic` with the `staticArchiveSHA` flag, `machocheck -native` and the engine checks of `-thin`, the vcpkg cache, `native-darwin.json`, notices + native gate for darwin, symbols zip | `machocheck` still passes (no dylib of ours), `--self-check` `ok` with the real engine on `macos-26` without TCC; a build without `imstatic` fails `-thin`; the notices list libopus and speexdsp | MR1, 02's static build | CI macos; Mac |
| MR6 | Manifest and selection (1 d) | `manifest.go` changes, `desktoppkg manifest -macos-archive`, two-OS vectors | §14's manifest tests on all OS; M2's golden unchanged; Windows CI green | — | CI all OS |
| MR7 | macOS apply and helper (2.5 d) | `untar.go` + fuzz, `apply_darwin.go`, `helper_darwin.go`, `codesign_darwin.go`, `policy_darwin.go`, `CurrentLocation`, `Status.Reason`, new codes, `updatetest` requirement hook | §14's darwin unit tests; goleak; a release build has no test strings | MR6, 01's one-shot dispatch | CI macos, ubuntu; Mac |
| MR8 | Updater e2e on macOS (1.5 d) | `macos-update` job, `deploy/test/macos/update-e2e.sh`, relserve CA trust on macOS | cases A–F green; both scripts exit 2 outside GitHub Actions | MR4, MR7, 01's `--update-now` and start step | CI macos |
| MR9 | `install-desktop.sh` (2 d) | the script, its unit test, shellcheck, `release:keys`/`lint:keys`/`release:prepare` for its blocks, the copied-functions check against `install.sh` | unit tests under dash, bash --posix and macOS sh; `install-e2e.sh` against relserve passes every case of §14 | MR3, MR4 | CI ubuntu, macos; Mac |
| MR10 | Server route (0.5 d) | `deploy/embed.go`, `RenderInstallSH`, route, startup check, `reservedEnv` names | §14's httpapi tests; `task dev` serves a pre-filled script; no unknown-env warning | MR9 | CI ubuntu; Mac |
| MR11 | `/download` page (1 d) | `links.ts`, `DownloadPage.tsx`, texts, `download.spec` | Vitest and Playwright as §14; axe passes | MR10, M2's page | CI ubuntu; Mac |
| MR12 | `macos-runtime` and nightly (1 d) | `macos-runtime` matrix, `macos-full`, `macos-runtime-full`, `goreleaser-full` changes | one green nightly with real macOS files; `macos-15-intel` runs the x86_64 slice natively | MR5, MR8, MR9 | GitHub |
| MR13 | Release pipeline (2 d) | `macos-app`, `macos-sign`, `macos-sign-tools`, `macos-pkg`, `desktop-manifest`, `build`/`publish` changes, `verify-macos`, `.goreleaser.yaml`, `check-draft.sh` + test, `deploy/desktop/macos-since`, `macos-exit-tools` and `isshoni-exit-tools-macos` (with 05's V11) | the dry run green end to end on `macos-15` and `macos-26`; `macos-sign` and `macos-sign-tools` run no Go/Node; check-draft's new cases | MR5, MR9, MR12 | GitHub dry run |
| MR14 | Homebrew (1 d) | `deploy/homebrew/{isshoni.rb.tmpl,render.sh,known_hosts}`, `brew style`/`audit` in `desktop-macos`, the `homebrew` job, the local-tap install in `macos-full` | the template passes `brew audit --cask --strict` (if the audit objects to the `xattr` postflight, the postflight is dropped and Open Anyway documented, §6.6; `--strict` stays); the nightly installs and zaps from a local tap; the job's push tested once against the real tap with a dry-run-only branch (`ci-check`), then deleted | MR13, owner action 2 | CI macos; GitHub |
| MR15 | Site and docs (1 d) | §11's pages, `release.json`, `/keys/*.pem`, `TestDocsAnchors` additions, README | the `site` job and anchor test pass; nothing loads from another origin | MR13 | CI ubuntu |
| MR16 | First release with the Mac app (1 d + owner checks) | §10.7 | the checklist's evidence in the release-prep PR: 05's M-MAC-1, 2, 3, 8, 9, 16, 22, 23 on 14.4/15/26/27 with §14's pass criteria; 05's exit results; `v0.3.0` published; the tap carries 0.3.0 | everything above; 01–03, 05 | owner's Mac + VMs; GitHub |

## 18. Open questions for the owner

1. **If macOS 15 or 26 re-asks for Local Network after every update** (M-MAC-16, §13): accept a constant per-arch
   `LC_UUID` (crash logs are then matched to the symbols zip by version, not by UUID), or accept one Local Network
   prompt per update on those versions and say so in the release notes?
2. **Homebrew for release candidates**: publish rc builds as a separate `isshoni@beta` cask, or keep the tap final-only
   (this document's default)?
3. **Build runner** (D14): the macOS build and native jobs move from PLAN's "macos-15 (cgo)" to `macos-26`, because
   the engine needs the macOS 26 SDK (02/03); MR1 writes this back to PLAN "Build, CI and release". The fallback is
   `macos-15` with Xcode 26 selected. Confirm.
