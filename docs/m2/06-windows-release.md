# M2 design 06: Windows build, packaging, updates and release

Part of the M2 ("Windows app") design set. Source of truth: `docs/PLAN.md`. Sibling docs, written in parallel:
`01-client-core.md`, `02-device-auth.md`, `03-desktop-app.md`, `04-windows-engine.md`, `05-windows-audio.md`. M1 is
built and merged; `docs/m1/06-deploy-and-ci.md` ("M1 06" below) is the record of the CI, release pipeline, license
gate and site that this document extends, and its section numbers are cited as "M1 06 §x".

**Scope marker.** Everything here is **M2** unless it is marked **later (M3/M4/M5)**. Later items are here only so M2
leaves room for them (mostly the macOS app of M3, which shares Wails, the updater and the release pipeline).

**Covers:** the FFmpeg LGPL build and its workflow; CI for the Windows DLL, the Wails app and the cross-OS client
core; the self-hosted audio-test runner; the NSIS installer; `install-desktop.ps1` and the server changes that serve
it and the `/download` page; the in-app updater (manifest, signing, verification, apply); unsigned 0.x releases,
SmartScreen and the SignPath Foundation application; the LGPL compliance bundle, the desktop `THIRD_PARTY_NOTICES`
and the license gate; the `release.yml` extension; versioning and compatibility; the first 0.x release checklist and
the owner's actions.

**Does not cover:** what the app does at runtime (window, tray, wizard, share sheet, settings storage: `03`), the
engine and its C ABI (`04`), audio exclusion and the audio-test harness itself (`05`), the device flow, token storage
and the deep-link grammar (`02`; deep-link grammar: 02 §12), the Go client core and its compatibility test content
(`01`). Section 18 lists exactly what this document takes from them.

## 1. Key decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Per-user install by default** into `%LOCALAPPDATA%\Programs\isshoni`, `RequestExecutionLevel user`, every registry entry under HKCU. No all-users mode in M2 | No UAC prompt to install, to update or for `irm … \| iex`, so updates can run silently; a friend on a shared PC installs only for themself; the same model as Discord and VS Code's user setup. The cost (one install per Windows account, files writable by the user) is the user's own trust boundary anyway |
| D2 | **The updater runs the app's own NSIS installer** silently (`/S /UPDATE /RELAUNCH`); Go never swaps files itself | One install code path for fresh installs, upgrades and updates (registry, shortcuts, protocol handler, stale-file removal, uninstaller); a running `.exe` can't be overwritten on Windows, and the installer already waits for the app to exit |
| D3 | **Update manifests are signed with an Ed25519 key in OpenSSH's SSHSIG format** (`ssh-keygen -Y sign`, namespace `isshoni-update`), with a key of their own (`isshoni-update-1`, secret `RELEASE_UPDATE_SIGNING_KEY` in the `release` environment) and an offline backup key; the app embeds both public keys | The protected `sign` job keeps running nothing but `ssh-keygen` (M1 06 D4); anyone can verify a manifest by hand with the same tool as `checksums.txt`; the app's verifier is about 100 lines on `golang.org/x/crypto/ssh`, fuzzed. A key separate from the checksums key, as the plan says, limits what one leak can do |
| D4 | **Stable channel reads `releases/latest/download/isshoni-desktop-update.json`; the opt-in beta channel reads the releases API** | The stable path has no API rate limit and no JSON listing to parse; GitHub's "latest" is only ever a final release (M1 06 §9.3 `publish`) |
| D5 | **FFmpeg is built once per pin** by `.github/workflows/ffmpeg.yml` and published as a non-SemVer prerelease `deps-ffmpeg-<ver>-r<rev>`; the repository pins its assets by SHA-256 in `native/ffmpeg/lock.json`; CI and releases download and verify, never rebuild | The shipped DLLs are byte-identical across releases until the pin changes, so the LGPL source tarball corresponds exactly; release day doesn't depend on MSYS2 mirrors; GitHub Actions caches evict after 7 days, a release asset doesn't. The server's release check, the site and the updater ignore non-SemVer tags (M1 04 §11.5) |
| D6 | **FFmpeg configure: LGPL, shared, only `libavcodec` + `libavutil`, four H.264 encoders, `--disable-autodetect`, static CRT, `/Brepro`** (§4.2) | Exactly the plan's line, made explicit; no decoders, demuxers or parsers means most FFmpeg CVEs (which are in decoders and demuxers) don't reach isshoni |
| D7 | **Static CRT everywhere** (`/MT` for `isshoni-media.dll`, FFmpeg and libvpl; vcpkg triplet `x64-windows-static`), checked with `dumpbin /dependents` | No Visual C++ Redistributable to install or ship; spike S1's DLL already used it (`spikes/s1-win-audio/CMakeLists.txt`) |
| D8 | **The installer is built with makensis 3.13 on `windows-2025`** (pinned portable zip, SHA-256), in **prebuilt-uninstaller mode** in CI; local builds on the Mac use Homebrew's makensis and plain `WriteUninstaller` | SignPath can later sign `uninstall.exe` and the installer as separate requests without restructuring the pipeline (§12.4); the `.nsi` builds on both hosts |
| D9 | **The WebView2 Evergreen bootstrapper is embedded** in the installer and run only when the runtime is missing; CI checks its Authenticode signer | Windows 11 always has the runtime and most Windows 10 22H2 PCs do, but not all; NSIS can't download over HTTPS without third-party plugins |
| D10 | **The executable is `isshoni.exe`** (the plan's name), AppUserModelID `io.isshoni.desktop` (the macOS bundle id) | One name in Task Manager, audio-exclusion logs and docs |
| D11 | **`install-desktop.ps1` verifies `checksums.txt.sig` with Windows' inbox OpenSSH** (`ssh-keygen -Y verify`, which needs OpenSSH 8.1; Windows 10 22H2's inbox client is expected to be 8.1p1, Windows 11's newer), the same key, principal and namespace as `install.sh`, and fails closed | The release's one signature covers the desktop installer too; no new trust root; PowerShell 5.1 has no Ed25519. Files fetched with `Invoke-WebRequest` carry no Mark of the Web, so SmartScreen doesn't prompt (the plan's Windows analogue of the macOS curl path) |
| D12 | **The server serves `/install-desktop.ps1` from `deploy/install-desktop.ps1`**, embedded by a new Go package `deploy` (`deploy/embed.go`) and pre-filled once at startup; `/install-desktop.sh` answers **404** until M3 | One source file; the pre-filled block is the only difference; an SPA fallback must never pipe `index.html` into `sh` |
| D13 | **The matching desktop version is the server's own version** (one tag), computed by the SPA from `/api/v1/info` `server.version`; no server lookup, no new config key | Server, web and desktop come from one SemVer tag (plan); a non-release server build links to the latest release |
| D14 | **Every `v*` tag ships the Windows artifacts**; `release.yml` fails without them | The `/download` links of every server are deterministic URLs; a server-only release would leave them dead |
| D15 | **Desktop artifacts enter goreleaser as `extra_files`**, so the one `checksums.txt` lists them and M1's sign, verify and publish chain applies unchanged in shape | No second checksums file, no second signature on the installers |
| D16 | **The Windows build jobs are unprivileged** (`contents: read`, no secrets, no write token); the privileged `build` job only checksums and uploads their outputs as data | M1 06 §9.3's rule: no write token next to third-party build code (vcpkg ports, FFmpeg's build system, makensis, Go modules) |
| D17 | **The audio test runs on the owner's PC through a dispatch-only workflow**; the runner is a non-admin account started by hand for a test session | A public repository must never let a pull request reach a self-hosted runner (§10.5) |
| D18 | **`cmd/isshoni-desktop` builds only for `windows` and `darwin && cgo`**; elsewhere a stub `main` exits 1 | `go test ./...` on Linux and M1's `GOOS=darwin go vet ./...` cross-check stay green without GTK or a darwin C toolchain (Wails needs cgo on darwin, none on Windows) |
| D19 | **Wails v3 is pinned exactly** (03 sets it; as of 2026-10-21 the newest tag is `v3.0.0-beta.34`, no GA or rc yet) in `go.mod`, in `tools/wails3/go.mod` and, for `@wailsio/runtime`, in `web/package.json`; Dependabot ignores both. A bump PR must pass 03's origin test against `protocol.DesktopOrigins()`; if the new Wails serves the SPA from a spelling that the **previous release's** `DesktopOrigins()` lacks, the bump waits until a release containing that spelling is out (02 §6.4) | Wails tags a beta almost daily; a bump needs the Windows smoke and a look at the app, so it is a hand-made PR like M1's tool pins. N−1 servers must accept an auto-updated app, or every not-yet-updated server answers CORS failures and a 403 at `/ws` |
| D20 | **Windows x64 only** in M2. ARM64 Windows can run the x64 build under emulation; the installer warns and continues. A native ARM64 build is **later** | No ARM64 hardware to test on; Qualcomm encoding would be MF-only |
| D21 | **Manifest expiry is 180 days after the commit date**; the update floor `minVersion` comes from a committed file `deploy/desktop/min-version`, raised only for security fixes | A long expiry needs no re-signing chores while releases come monthly; a floor that is a reviewed file needs no secret and no notes parsing |
| D22 | **No runtime integrity check of the FFmpeg DLLs** beyond 04's version check: a user may replace them with an interface-compatible build with the same major versions (`avcodec-<C>.dll`, `avutil-<U>.dll`, same file names, in the install folder) and at least the shipped minor versions; isshoni checks `avcodec_version()`/`avutil_version()` at load (04 §9.2) | LGPL-2.1 §6 lets the user replace them with an interface-compatible build; integrity is checked at download time (signature and SHA-256), not at load time |
| D23 | **JSON is camelCase** (M1 README §3): the plan's manifest `{version, channel, min_version, expires_at}` is written `version`, `channel`, `minVersion`, `expiresAt` | One JSON convention in the repository |

## 2. Artifacts, names and the installed layout

### 2.1 Release assets added in M2

Every `v*` tag adds these GitHub Release assets to M1 06 §3's list. Names are an interface: the server's `/download`
page, `install-desktop.ps1`, the updater and the site build URLs from them. `<v>` is the version without `v`
(`0.2.0`, `0.2.0-rc.1`).

| Asset | Contents | In `checksums.txt` |
|---|---|---|
| `isshoni-desktop_<v>_windows_amd64_setup.exe` | NSIS installer (§6) | yes |
| `isshoni-desktop_<v>_windows_amd64_symbols.zip` | `isshoni-media.pdb`, FFmpeg's `.pdb` files, `native-manifest.json` (§5.1) | yes |
| `isshoni-desktop_<v>_windows_amd64.sbom.json` | SPDX JSON SBOM of the staged install tree (syft) | yes |
| `isshoni-desktop_<v>_ffmpeg-source.tar` | the LGPL corresponding source, byte-identical to the deps release's `…-source.tar` (§4.5) | yes |
| `isshoni-desktop-update.json` | update manifest (§8.2), fixed name so `releases/latest/download/…` works | yes |
| `isshoni-desktop-update.json.sig` | SSHSIG of the manifest by the update key (§8.3), uploaded by `sign` | **no** (it is a signature, like `checksums.txt.sig`) |
| `install-desktop.ps1` | the generic script, stamped with `<v>` and an empty server (§7) | yes |
| `isshoni-desktop_<v>_windows_amd64_tools.zip` | **reserved**: the `native:dist` zip (§17: `isshoni-enginetest.exe`, `g2g.exe`, `hwcheck.ps1`, the release DLL and the FFmpeg DLLs), published only if the owner answers 04 §17 Q3 (community testing) yes | yes, when published |

All but the `.sig` get GitHub build-provenance attestations (M1 06 §9.3 `build` step 9). The release notes say that
`isshoni_<v>_windows_{amd64,arm64}.zip` are the experimental **server** builds, not the app (§11.7).

**Later (M3):** `isshoni-desktop_<v>_darwin_universal.tar.gz` (the signed `.app`) and its manifest artifact entry;
**later (M4):** the agent's deb/rpm, which are updated by package managers and have no manifest entry.

### 2.2 Installed files

```
%LOCALAPPDATA%\Programs\isshoni\            $INSTDIR (per-user; /D= on a fresh install may choose another folder)
  isshoni.exe                               the Wails app (Go, cmd/isshoni-desktop, 03)
  isshoni-media.dll                         the Windows engine (04)
  avcodec-<N>.dll, avutil-<N>.dll           FFmpeg, unmodified, unsigned (N from the FFmpeg build, §4.5)
  uninstall.exe
  install-files.txt                         every file this installer wrote, relative to $INSTDIR, one per line (§6.6)
  licenses\LICENSE.txt                      Apache-2.0 (the repository's LICENSE)
  licenses\NOTICE.txt                       the repository's NOTICE
  licenses\THIRD_PARTY_NOTICES.txt          the desktop notices (§9.2)
  licenses\ffmpeg\COPYING.LGPLv2.1.txt      from the FFmpeg source tree
  licenses\ffmpeg\LICENSE.md                from the FFmpeg source tree
  licenses\ffmpeg\README-LGPL.txt           version, configure line, where the source is, how to replace the DLLs (§9.1)
```

Not installed: OpenH264 (downloaded at runtime into `%LOCALAPPDATA%\isshoni\codecs\` after the user sees Cisco's
notice, by 03 through 04's `native.InstallOpenH264`; never in an installer), x264 (dev builds only), the WebView2
bootstrapper (it runs from `$PLUGINSDIR` and is deleted with it).

**One data root** (per user and per PC; nothing roams, because device ids and the `CRED_PERSIST_LOCAL_MACHINE`
credentials are per PC; 03 §14.1):

| Root | Holds | Owner |
|---|---|---|
| `%LOCALAPPDATA%\isshoni\` | `settings.json` (+ `.bak`) | 03 (§14.1) |
| | `servers.json` (origins, server names, usernames, device ids, last room) | 02 (§11.6) |
| | `logs\`, `WebView2\` | 03 |
| | `codecs\` (OpenH264, written through 04's `native.InstallOpenH264`) | 03 |
| | `updates\` (§8.7) | this document |

The refresh tokens are not in the data root: they are Windows Credential Manager entries `isshoni:v1:<origin>` (02
§11.5). Every uninstall forgets the credentials (§6.8); the data root stays unless the user asks to purge.

### 2.3 Registry

All values are written by the installer unless noted. HKLM is never written.

| Key | Value | Type | Meaning |
|---|---|---|---|
| `HKCU\Software\isshoni` | `InstallDir` | SZ | `$INSTDIR`; the updater applies only when the running exe is in it (§8.7) |
| | `Version` | SZ | installed SemVer |
| | `DesktopShortcut` | DWORD | 1/0, the user's choice, kept across updates |
| | `UpdateChecks` | DWORD | the installer's "Check for updates automatically" choice; 03 reads it as the initial value of its setting when its settings file has none, then never again |
| | `WinVer` | SZ | the installed version's winver (§5.3), for the installer's version compare (§6.6) |
| | `PendingServer` | SZ | a server origin from `/SERVER=` (§6.4), or written by 03 before a start-time update (§8.7) or by a start from a link that found the installing mutex (§2.4); read and deleted by the app at its next start (03). The app validates it with 02's `deeplink.NormalizeOrigin(v, allowInsecureLoopback)`: `http://` loopback passes only in dev builds, and a release app drops such a value with one WARN. A valid value is then handled exactly like the link `deeplink.Build(origin, PendingRoom)`, under 02 §12.3: an unknown server gets "Add server {host}?" before any network request, and nothing ever starts a share |
| | `PendingRoom` | SZ | written only by 03, next to `PendingServer`, when a pending update is applied at a start that came from an `isshoni://` link (§8.7), or when such a start finds the installing mutex (§2.4); read and deleted with it |
| | (subkeys) | | any subkey the app writes under `HKCU\Software\isshoni` (for example Wails' notifications service's `NotificationCategories` when the Wails app `Name` is `isshoni`; **written by the app (03)**); the uninstaller deletes the whole key (§6.8) |
| `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\isshoni` | `DisplayName` = `isshoni`, `DisplayVersion` = `<v>`, `Publisher` = `isshoni project`, `DisplayIcon` = `"$INSTDIR\isshoni.exe",0`, `InstallLocation` = `$INSTDIR`, `UninstallString` = `"$INSTDIR\uninstall.exe"`, `QuietUninstallString` = `"$INSTDIR\uninstall.exe" /S`, `URLInfoAbout` = `https://moonwx.github.io/isshoni/`, `HelpLink` = `https://moonwx.github.io/isshoni/download#windows`, `NoModify` = 1, `NoRepair` = 1, `EstimatedSize` (KiB) | | Apps & features entry |
| `HKCU\Software\Classes\isshoni` | (Default) = `URL:isshoni`, `URL Protocol` = `""` | SZ | the `isshoni://` scheme |
| `HKCU\Software\Classes\isshoni\DefaultIcon` | (Default) = `"$INSTDIR\isshoni.exe",0` | SZ | |
| `HKCU\Software\Classes\isshoni\shell\open\command` | (Default) = `"$INSTDIR\isshoni.exe" --open-url "%1"` | SZ | 02 §12.2, 03 §13.1: a launch that contains `--open-url` takes exactly the next argument as the link, which must start with `isshoni:`, and honors no other flag, so `--purge-data`, `--forget-credentials` and `--update-now` cannot be smuggled in |
| `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` | `isshoni` | SZ | written by 03's "Start with Windows" setting, **removed** by the uninstaller |
| `HKCU\Software\Classes\AppUserModelId\io.isshoni.desktop` | `DisplayName`, `IconUri`, `CustomActivator` = `{<guid>}`, … | SZ | **written by the app (03)**: Wails' notifications service registers the toast AUMID at run time (03 §15); deleted by the uninstaller (§6.8) |
| `HKCU\Software\Classes\CLSID\{<guid>}` (the `CustomActivator` above) | `LocalServer32` (Default) = `"$INSTDIR\isshoni.exe" …` | SZ | **written by the app (03)**: the toast COM activator; deleted by the uninstaller only when `LocalServer32` points into `$INSTDIR` (§6.8) |
| `HKLM\SOFTWARE\Policies\isshoni` | `DisableUpdateChecks` | DWORD | **read only**: 1 turns the updater off for every user of the PC (§8.8) |

Shortcut: `$SMPROGRAMS\isshoni.lnk` (the user's Start menu) → `$INSTDIR\isshoni.exe`, with the shell property
`System.AppUserModel.ID` = `io.isshoni.desktop` (set with NSIS's built-in System plugin through `IShellLinkW` and
`IPropertyStore`), so 03's toast notifications and taskbar grouping match the shortcut. Optional
`$DESKTOP\isshoni.lnk` with the same property.

### 2.4 Process contract between the installer and the app

| Item | Owner | Contract |
|---|---|---|
| Named mutex `Local\io.isshoni.desktop.running` | 03 creates it at start and holds it until the process exits | the installer and uninstaller wait for it to disappear (§6.7) |
| Named mutex `Local\io.isshoni.desktop.installing` | the installer (`installer.nsi`) creates it in `.onInit`, in every mode (interactive, `/S`, `/S /UPDATE`), and holds it for its whole run; it closes the handle only right before it starts the app (`/LAUNCH`, `/RELAUNCH`, the Finish checkbox), which is its last action, or when it exits | 03 probes it with `OpenMutexW(SYNCHRONIZE, FALSE, …)` at every normal start, after the shell relaunch and before single instance and the running mutex (03 §13.4, new step 2a): when it exists, a start from a link writes the link's origin to `PendingServer` and its room id to `PendingRoom` (§2.3; the newest link wins), any start then exits 0 without becoming the first instance, without building the updater and without calling `Apply`. The installer's own app start then handles the link. A second installer that finds it exits 8 (§6.4, §6.7) |
| `isshoni.exe --version` | 03 | prints one JSON line `{"version","commit","date","protocol","engineAbi",…}` to stdout (also when stdout is redirected from a GUI-subsystem process) and exits 0; opens no window; `engineAbi` is the ABI major Go was built for (`media.ABIVersion`, `1`; 01 §5.1), read without loading the DLL |
| `isshoni.exe --self-check=<path>` | 03 with 04 | opens no window; `native.Open(native.Options{Dir: <exe dir>})`, then `Engine.Probe(ctx, false)` (quick; it loads FFmpeg, 04 §5.3), then `Engine.BuildInfo()`, then 03's asset handler over the bundled SPA (03 §13.4); writes `{"ok": bool, "version": "<v>", "engine": {"loaded": bool, "abi": 1, "ffmpegLoaded": bool, "ffmpegLicense": "<BuildInfo.ffmpeg.license>", "testHooks": false}, "spa": {"ok": bool}}` and exits 0 when `ok`; `abi` is the DLL's ABI major (01 §5.1's `Info().ABI`). `ok` requires `loaded`, `abi` 1, `ffmpegLoaded`, `ffmpegLicense` = `"LGPL version 2.1 or later"`, `testHooks` false and `spa.ok`; no encoder is required, so it passes on a GPU-less CI VM |
| `isshoni.exe --open-url <url>` | 03 parses it; grammar 02 §12 | the protocol handler's entry (§2.3); a launch that contains `--open-url` takes exactly the next argument, which must start with `isshoni:`, and honors no other flag (02 §12.2, 03 §13.1) |
| `isshoni.exe --updated-from=<old>` | 03 | started by the installer after an update (§8.7) |
| `isshoni.exe --forget-credentials` | 03 parses it; 02 implements it | opens no window; runs 02 §11.7's `deviceauth.Servers.ForgetAll` (every linked server revoked concurrently under one 5 s budget, then every `isshoni:v1:*` credential deleted); exits 0 even when offline. Run by **every** uninstall (§6.8) |
| `isshoni.exe --purge-data` | 03 with 02 | opens no window; runs 02's `deviceauth.Servers.ForgetAll` exactly as `--forget-credentials` (the same one 5 s budget), then deletes the data root of §2.2 (`%LOCALAPPDATA%\isshoni\`); exit 0. Run by a `/PURGE` uninstall (§6.8) |
| `isshoni.exe --update-now` | 03, only in builds with the `updatetest` tag | runs one check without waiting for `WizardDone` and applies a found update without a window (§8.11) |
| No job object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` around the app | 03 | the installer started by the updater must outlive the app |

## 3. Toolchains and pins

| What | Pin (as of 2026-10-21) | Where pinned | Bumped |
|---|---|---|---|
| Go | the M1 toolchain (`go.mod` `toolchain`, `.tool-versions`) | unchanged | M1's monthly tool PR |
| Wails v3 | `v3.0.0-beta.34` unless 03 picks another exact tag; follows the GA | `go.mod` (03), `tools/wails3/go.mod` (`tool github.com/wailsapp/wails/v3/cmd/wails3`) and `@wailsio/runtime` in `web/package.json` + `web/package-lock.json` (the exact version 03's DK1 records for the pinned Wails), checked by `task lint:pins` | by hand, all three together (03 §3.3), with a Windows smoke and 03's origin test against `protocol.DesktopOrigins()` (D19; Dependabot ignores both) |
| Windows runner | `windows-2025` (not `windows-latest`, which moves) | workflows | when GitHub retires it |
| Visual Studio / MSVC | any VS 2022 or VS 2026 instance (`[17.0,19.0)`) with the base C++ tools component, **toolset `14.44`** (an exact minor, not a prefix), Windows SDK `10.0.26100.0`; the selection step is below | `deploy/windows/tools.json`; the exact toolset build (`14.44.<build>`) is recorded in every build manifest | when the runner image drops the 14.44 toolset: a hand-made tool PR moves `toolset`, which also changes FFmpeg's reproducibility record (§4.6) |
| vcpkg | the commit in `native/vcpkg.json` `builtin-baseline` (04 owns the file and `native/vcpkg-configuration.json`, both at the CMake root `native/`, 04 §2) | 04 | by hand, monthly with the tool pins |
| NSIS | 3.13, portable zip from SourceForge with SHA-256 | `deploy/windows/tools.json` | by hand |
| FFmpeg | `9.0.2` tarball, SHA-256, plus nv-codec-headers, AMF headers, libvpl (§4.1) | `native/ffmpeg/pins.json` | on an FFmpeg point release or a security fix (§4.7) |
| Pester, PSScriptAnalyzer | newest 5.x / 1.x at slice time, exact versions | `deploy/windows/tools.json` | by hand |
| syft, cosign | M1's | unchanged | M1 |

`deploy/windows/tools.json`:

```json
{
  "nsis": {"version": "3.13", "url": "https://downloads.sourceforge.net/project/nsis/NSIS%203/3.13/nsis-3.13.zip",
           "sha256": "<pin at slice time>"},
  "msvc": {"vs": "[17.0,19.0)", "toolset": "14.44", "windowsSdk": "10.0.26100.0"},
  "webview2Bootstrapper": {"url": "https://go.microsoft.com/fwlink/p/?LinkId=2124703",
                           "signer": "CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US"},
  "pester": "<exact 5.x>",
  "psScriptAnalyzer": "<exact 1.x>"
}
```

- **The MSVC environment step** (`deploy/windows/msvc-env.ps1`, one PowerShell script used by `ffmpeg.yml`'s build
  (§4.3), every `windows-native` job (§5.1), `windows-asan` (§10.3), `audiotest.yml`'s build (§10.4) and
  `task native:build`/`task ffmpeg:build` on a Windows PC; no third-party action):
  1. `vswhere.exe -latest -products * -version "<msvc.vs>" -requires
     Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath` (vswhere from
     `${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\`). No instance → fail with "no VS 2022/2026
     instance with the C++ x64 tools".
  2. Fail fast unless `<instance>\VC\Tools\MSVC\<msvc.toolset>.*` exists (one or more folders such as `14.44.35207`;
     the newest build wins), and unless `${env:ProgramFiles(x86)}\Windows Kits\10\Include\<msvc.windowsSdk>` exists.
     The message names what is missing and what was found.
  3. `cmd /c "call ""<instance>\VC\Auxiliary\Build\vcvars64.bat"" -vcvars_ver=<msvc.toolset> <msvc.windowsSdk> && set"`,
     then `PATH`, `INCLUDE`, `LIB` and `LIBPATH` go to `GITHUB_ENV` (CI) or the current session (a PC).
  4. Prints one line to the log and the job summary: the instance's `installationVersion` and `displayName`, the
     toolset build (`14.44.<build>`, from `VCToolsVersion`) and the SDK (`WindowsSDKVersion`); the build manifests
     record the same values.

  Why not `-version "[17.0,18.0)"`: GitHub's `windows-2025` label runs the Windows Server 2025 image with Visual
  Studio 2026 (18.x) and no VS 2022 instance; it carries the 14.44 toolset only as the component
  `Microsoft.VisualStudio.Component.VC.14.44.17.14.x86.x64` (runner-images readme, read 2026-10-21). Why not that
  versioned component as the only selector: VS 2022 17.14 ships 14.44 as its default `VC.Tools.x86.x64` and has no
  separate versioned component, so an owner's PC with VS 2022 would match nothing. The presets use Ninja, not a VS
  generator (04 §13.1), so pinning the toolset is enough; `-vcvars_ver=14.44` keeps FFmpeg's `/Brepro` output tied to
  one compiler minor (§4.6).

  **The owner's PC** needs either VS 2022 17.14 Build Tools (workload "Desktop development with C++", whose default
  toolset is 14.44) or VS 2026 Build Tools with the individual component "MSVC v143 - VS 2022 C++ x64/x86 build tools
  (v14.44)" (`Microsoft.VisualStudio.Component.VC.14.44.17.14.x86.x64`), plus the Windows 11 SDK 10.0.26100. The
  same script sets up its shell (`task native:build` runs it), so a "Developer PowerShell" with another default
  toolset is not used.
- `.github/dependabot.yml` gains `ignore: [{dependency-name: "github.com/wailsapp/wails/v3"}]` for `/`, and
  `ignore: [{dependency-name: "@wailsio/runtime"}]` on the existing npm entry for `/web` (whose `npm-minor-patch`
  group matches `"*"` and would otherwise propose runtime bumps on their own); its "Not covered here" comment lists
  `tools/wails3`, `@wailsio/runtime`, `deploy/windows/tools.json`, `native/ffmpeg/pins.json` and the vcpkg baseline
  next to M1's hand-bumped pins (M1 06 §7.1).
- `task lint:pins` (extended): the Wails version in `go.mod` equals `tools/wails3/go.mod`'s; the exact
  `@wailsio/runtime` version in `web/package.json` (no range) and in `web/package-lock.json` equals the Go module
  version of Wails in `go.mod` without the leading `v` (03 §3.1: library, CLI and runtime share one version; no
  separate file records it).
- **Hand-bump checklist** (the PR template's Wails section): library, CLI and `@wailsio/runtime` moved together;
  03's checklist re-run; the Windows smoke; 03's origin test against `protocol.DesktopOrigins()` green, and the new
  asset-origin spelling present in the **previous** release's `DesktopOrigins()` (else the bump waits, D19).
- `tools/wails3/go.mod` is a module of its own like `tools/goreleaser` (M1 06 §7.1): the wails3 CLI's dependency graph
  stays out of `tools/go.mod`. `task release:wails3` builds it into `.bin/wails3`.
- Wails' Windows dependency set at beta.34, measured on a scratch module (`GOOS=windows CGO_ENABLED=0 go list -deps`):
  `github.com/adrg/xdg`, `github.com/go-ole/go-ole`, `github.com/mattn/go-colorable`, `github.com/mattn/go-isatty`,
  `golang.org/x/sys`, all MIT or BSD-3-Clause; the Windows build cross-compiles from the Mac with `CGO_ENABLED=0`.
  03's choice of Wails services (notifications, …) can add modules; the license gate (§9.4) checks whatever is linked.

## 4. FFmpeg LGPL build (`native/ffmpeg`, `.github/workflows/ffmpeg.yml`)

### 4.1 Pins and lock

`native/ffmpeg/pins.json` (inputs; each URL's SHA-256 is pinned, and the bump PR also checks FFmpeg's PGP signature
by hand against FFmpeg's release key):

```json
{
  "revision": 1,
  "ffmpeg":          {"version": "9.0.2", "url": "https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz", "sha256": "<pin>"},
  "nvCodecHeaders":  {"tag": "n12.1.14.1", "url": "https://github.com/FFmpeg/nv-codec-headers/archive/refs/tags/n12.1.14.1.tar.gz", "sha256": "<pin>"},
  "amfHeaders":      {"tag": "v1.5.3", "url": "https://github.com/GPUOpen-LibrariesAndSDKs/AMF/releases/download/v1.5.3/AMF-headers-v1.5.3.tar.gz", "sha256": "<pin>"},
  "libvpl":          {"tag": "v2.17.0", "url": "https://github.com/intel/libvpl/archive/refs/tags/v2.17.0.tar.gz", "sha256": "<pin>"}
}
```

- `revision` starts at 1 and goes up whenever the build changes without a version change (a flag, the script).
- Each entry stays on one line, as above: `build.sh` reads `pins.json` line by line (no JSON parser in MSYS2's bash),
  and `tools/desktoppkg`'s check of `pins.json` enforces it. `native/ffmpeg/.gitattributes` (`eol=lf`) and
  `tools/desktoppkg/testdata/.gitattributes` (`-text`) keep the bytes that `inputsSha256` hashes the same in every
  checkout, Git for Windows' `core.autocrlf` included.
- **nv-codec-headers set the minimum NVIDIA driver.** n12.1.x asks for the NVENC 12.1 API (Windows driver ≈531 or
  newer); n13.x would raise the floor to the 570 series. 04 decides the floor; n12.1.14.1 is the default here. The
  exact driver numbers are checked against NVIDIA's SDK notes when the pin is set.
- AMF headers and libvpl are MIT; nv-codec-headers are MIT (headers only, the driver library is loaded at run time).
- **libvpl's dispatcher and the DLL search** (04 §13.3 layer 4, §17): if 04's marker test (`dll_search_test`'s
  vendor-runtime and libvpl part, slice E7, README WA44) shows the dispatcher loading a runtime from the current
  directory or `PATH`, this pin moves to a libvpl version or build option whose dispatcher doesn't, in a tool PR
  (a new `revision`) before WA44 merges.

`native/ffmpeg/lock.json` (outputs; written by the owner after a publishing run, §4.5):

```json
{
  "revision": 1,
  "tag": "deps-ffmpeg-9.0.2-r1",
  "inputsSha256": "<sha256 over pins.json, build.sh, ffcheck.c (§4.4)>",
  "assets": {
    "ffmpeg-9.0.2-r1-win-x64.zip": "<sha256>",
    "ffmpeg-9.0.2-r1-source.tar": "<sha256>"
  },
  "avcodecMajor": 62,
  "avutilMajor": 60,
  "dlls": {"avcodec-<avcodecMajor>.dll": "<sha256>", "avutil-<avutilMajor>.dll": "<sha256>"},
  "msvcToolset": "14.44.yyyyy",
  "commit": "<the commit the run built>"
}
```

`avcodecMajor` and `avutilMajor` (integers; the numbers above are placeholders) are written by the publish run from
`ffcheck`'s `avcodec_version() >> 16` and `avutil_version() >> 16` in `ffmpeg-config.json` (§4.4). 04's CMake reads
them to compile in the runtime DLL names and the load-time version check (04 §9.2).

`inputsSha256` is `sha256` of the three lines `"<sha256>  <path>\n"` for `native/ffmpeg/pins.json`,
`native/ffmpeg/build.sh` and `native/ffmpeg/ffcheck.c`, in that order (`tools/desktoppkg ffmpeg-inputs` prints it).
Every consumer fails with "native/ffmpeg/lock.json is stale: run the FFmpeg workflow with publish and update the lock"
when it differs from the current files, so a pin change can't merge without its build.

### 4.2 Configure line

Run in `WORKDIR/build` on the source in `WORKDIR/build/src`, with `PKG_CONFIG_PATH=../prefix/lib/pkgconfig`:

```
./src/configure --toolchain=msvc --arch=x86_64 --target-os=win64 --prefix=../install \
  --disable-gpl --disable-nonfree --disable-version3 \
  --enable-shared --disable-static \
  --disable-programs --disable-doc --disable-everything --disable-autodetect \
  --disable-network --disable-x86asm \
  --disable-avformat --disable-avdevice --disable-avfilter --disable-swscale --disable-swresample \
  --enable-avcodec --enable-avutil \
  --enable-d3d11va --enable-mediafoundation \
  --enable-ffnvcodec --enable-nvenc --enable-amf --enable-libvpl \
  --enable-encoder=h264_nvenc,h264_amf,h264_qsv,h264_mf \
  --extra-cflags='-MT -Brepro -I../prefix/include' \
  --extra-ldflags='-Brepro -PDBALTPATH:%_PDB% -INCREMENTAL:NO -LIBPATH:../prefix/lib' \
  --pkg-config-flags=--static
```

- **Every path is relative** to the build folder (the prefixes, `-I`, `-LIBPATH`, `PKG_CONFIG_PATH`, and the `.pc`
  files: `ffnvcodec.pc`'s absolute `prefix=` is rewritten to `${pcfiledir}/../..`), so neither the compiled-in source
  file names nor the configuration string `avcodec_configuration()` returns names a path of the build machine, and the
  DLLs don't depend on where `WORKDIR` is (§4.6). `-PDBALTPATH:%_PDB%` keeps the PDB's path out of the DLLs;
  `-INCREMENTAL:NO` makes a release link (with FFmpeg's `-debug`, `link.exe` links incrementally by default, with
  jump thunks and padding in the DLLs).

- The plan's line (`--disable-gpl --disable-nonfree --enable-shared --disable-programs --disable-everything`, the four
  encoders and libvpl), plus what makes it exact: `--disable-version3` (stays LGPL-2.1-or-later, not v3);
  `--disable-autodetect` (nothing enters by being found on the runner); only the two libraries the engine links (04
  does colour conversion with the D3D11 VideoProcessor and packetization is Go's, so no swscale, swresample or
  avformat); `--disable-x86asm` (the hardware encoders need no assembly, and nasm leaves the toolchain).
- `-MT` is D7; `-Brepro` makes the MSVC compiler and linker output deterministic (§4.6). Debug information stays on
  (FFmpeg's default `-Z7` and `-debug` for MSVC), so the build has PDBs for the symbols zip.
- If 04 finds an encoder that needs one more component (a parser or bitstream filter), it is added here by name with
  a one-line reason in `build.sh`, never by dropping `--disable-everything`.

### 4.3 The build (`native/ffmpeg/build.sh`, run by `ffmpeg.yml` and by `task ffmpeg:build` on a Windows PC)

`native/ffmpeg/build.sh [-w WORKDIR] [-j JOBS]` runs in MSYS2's bash (preinstalled at `C:\msys64` on GitHub's Windows
runners) inside the MSVC environment of §3, which `deploy/windows/msvc-env.ps1` sets up and passes on: in CI the
workflow's step, on a PC `powershell -NoProfile -File deploy\windows\msvc-env.ps1 C:\msys64\usr\bin\bash.exe
native/ffmpeg/build.sh` from the repository root. `WORKDIR` defaults to the git-ignored `.deps/ffmpeg-build`. The
script installs nothing and changes nothing outside `WORKDIR`: MSYS2's packages come from the workflow's
`pacman -Sy --noconfirm --needed make pkgconf diffutils zip` (once by hand on a PC; their versions go into the build
manifest). No third-party action. The same command rebuilds the DLLs from an unpacked source archive (§4.5).

1. **Preflight**: MSYS2's bash and the MSVC variables (`VCToolsInstallDir`, `VCToolsVersion`, `WindowsSDKVersion`)
   are required. MSVC's `Hostx64/x64` folder goes first on `PATH`, before `/usr/bin`, so MSYS2's coreutils `link.exe`
   can't shadow MSVC's linker; a PC's MSYS2 install is left untouched (no `mv /usr/bin/link.exe`). CMake and Ninja
   come from the VS instance ("C++ CMake tools for Windows"), else from `PATH`.
2. **Sources**: each `pins.json` tarball into `WORKDIR/src/`, downloaded with MSYS2's `curl` (3 retries) unless
   already there or in the source archive's `src/`, and checked against its SHA-256 either way.
3. **libvpl**: `cmake -G Ninja -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF
   -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded -DUSE_MSVC_STATIC_RUNTIME=ON -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF
   -DINSTALL_EXAMPLES=OFF`, build, install into `WORKDIR/prefix` (gives `vpl.lib` and `vpl.pc`). `vpl.pc` gets
   `Libs.private: -ladvapi32 -lole32`: libvpl names the static dispatcher's system libraries for MinGW only, and
   without them configure's libvpl check can't link under MSVC.
4. **Headers**: nv-codec-headers with `make PREFIX=… install` (headers and `ffnvcodec.pc`, whose `prefix=` is made
   relative, §4.2). The AMF headers tarball has `AMF/` at its top (not `amf/public/include`); it is copied to
   `prefix/include/AMF`.
5. **FFmpeg**: configure (§4.2), `make -j<jobs>`, `make install` into `WORKDIR/install`.
6. **Checks** of §4.4, then the layout `ffmpeg-<ver>-r<rev>-win-x64/`: `bin/` (the two DLLs), `pdb/`, `lib/`
   (`avcodec.lib`, `avutil.lib` import libraries), `include/`, `licenses/{ffmpeg/COPYING.LGPLv2.1,ffmpeg/LICENSE.md,
   ffmpeg/README-LGPL.txt,libvpl/LICENSE,nv-codec-headers/LICENSE,amf/LICENSE.txt}`, `ffmpeg-config.json` (§4.4) and
   `build-manifest.json` (every input's SHA-256, the toolset, SDK and MSYS2 package versions, the commit). Neither
   the AMF headers nor nv-codec-headers ship a license file, so `build.sh` writes `amf/LICENSE.txt` and
   `nv-codec-headers/LICENSE` from the headers' own MIT notices: each distinct notice once, after the files that
   carry it.
7. **Archives** in `WORKDIR/out/`: `ffmpeg-<ver>-r<rev>-win-x64.zip` (the layout folder at its top; names sorted, no
   extra fields, every timestamp `SOURCE_DATE_EPOCH`, the commit time) and the source archive (§4.5) with `tar
   --format=gnu --sort=name --owner=0 --group=0 --numeric-owner --mtime=@$SOURCE_DATE_EPOCH`; plus `result.json`
   (names, SHA-256s, majors, toolset) and `lock-candidate.json`, the `lock.json` a publishing run prints (§4.1).

Time: about 10 minutes on a hosted runner, most of it `configure` under MSYS2.

### 4.4 Checks (gating, in `ffmpeg.yml` and again in every consumer job)

- `native/ffmpeg/ffcheck.c` (ASCII only, compiled with `cl -MT` against the build) loads both DLLs and writes
  `ffmpeg-config.json`: `avcodec_version()`, `avutil_version()` (major, minor and micro), `avcodec_configuration()`,
  `avcodec_license()`, `avutil_license()`, the names of the encoders and decoders `av_codec_iterate` returns, `ok`
  and `problems` (each failed check, also on stderr). Exit 0 when every check passes, 1 when one fails, 2 on a usage
  or I/O error.
- Fails unless: `avcodec_license()` and `avutil_license()` are `"LGPL version 2.1 or later"`; the configuration
  contains none of `--enable-gpl`, `--enable-nonfree`, `--enable-version3`; the encoder list is exactly `h264_amf`,
  `h264_mf`, `h264_nvenc`, `h264_qsv`, and there is no decoder; each DLL's major equals its header's
  (`LIBAVCODEC_VERSION_MAJOR`, `LIBAVUTIL_VERSION_MAJOR`); the generated `config.h` has `#define CONFIG_GPL 0`,
  `#define CONFIG_NONFREE 0` and `#define CONFIG_VERSION3 0`.
- `dumpbin /dependents` of both DLLs lists only DLLs from a clean Windows 10 22H2 `System32` (no `VCRUNTIME*`,
  `MSVCP*`, `api-ms-win-crt-*`, `ucrtbase*`); vendor runtimes (`nvEncodeAPI64.dll`, `amfrt64.dll`, the oneVPL
  runtime) are loaded at run time, never imported.
- Neither FFmpeg DLL statically imports `mfplat.dll`, `mf.dll`, `mfreadwrite.dll` or `evr.dll` (FFmpeg's `mf_utils`
  loads MFPlat at run time). Those DLLs are absent on N editions and on Windows Server (the `windows-2025` runner
  included); a static import would keep `avcodec` from loading there and take NVENC, QSV and AMF with it, against 04
  §17 ("MF missing → others/OpenH264 remain"). A violation fails the pin.
- The DLL names match `avcodec-[0-9]+\.dll` and `avutil-[0-9]+\.dll`, and equal `avcodec-<avcodecMajor>.dll` and
  `avutil-<avutilMajor>.dll` of `lock.json`; otherwise the check fails.

### 4.5 `ffmpeg.yml` and the deps release

| Trigger | What happens |
|---|---|
| `pull_request` touching `native/ffmpeg/**`, the workflow, `deploy/windows/msvc-env.ps1` or `deploy/windows/tools.json` | `build` and `rebuild`, then `report`: the summary says whether the two builds and `lock.json` have byte-identical DLLs (§4.6) |
| `push` to `main` touching the same paths | `build`, then `report` |
| `workflow_dispatch` (the owner, from any ref) | `build` and `rebuild`, then `report`; with `publish: true` also `publish` |
| `schedule` weekly (Sunday) | `build` of the commit `lock.json` names, then `report`: the reproducibility check |

- **`build`** (`windows-2025`, `contents: read`, no secrets, timeout 40 min): the MSVC environment step (§3), MSYS2's
  packages, §4.3 and §4.4, then uploads the zip and the source tar as the artifact `ffmpeg-build` and their
  SHA-256s, the DLL names and hashes, the toolset build and `inputsSha256` as job outputs. It also checks that
  `msvc-env.ps1` fails, naming the toolsets it found, for a toolset the image lacks.
- **`rebuild`** (pull requests and manual runs): the same steps on a second runner (one set of steps for both, through
  YAML anchors), so a pull request shows automatically whether a second build is byte-identical.
- **`report`** (`ubuntu-24.04`, `contents: read`): the `inputsSha256` that `build.sh` recorded must equal what
  `desktoppkg ffmpeg-inputs` prints; the DLLs are compared with `rebuild`'s, and with `lock.json`'s whenever its
  `inputsSha256` is the one `build` hashed (also in the weekly run while a pins bump waits for its publishing run),
  under §4.6's rule; a `lock.json` that is absent or stale is a note (publishing after the merge fixes it). A table of
  the hashes and toolsets goes into the job summary.
- **`publish`** (`ubuntu-24.04`, `needs: [build, report]`, both successful, permissions `contents: write`,
  `id-token: write`, `attestations: write`, timeout 10 min; runs no build code): downloads the artifact, checks both
  SHA-256s against `build`'s outputs, `actions/attest-build-provenance` on both, then `gh release create
  deps-ffmpeg-<ver>-r<rev> --target <sha> --prerelease --latest=false --title "FFmpeg <ver> (r<rev>) for isshoni"` with
  both files and a `SHA256SUMS`, and the notes "Build input of the isshoni Windows app; not an isshoni release". It
  prints the `lock.json` to commit. Refuses to run when that tag already exists (bump `revision` instead).
- The tag is not SemVer, so the server's release check (M1 04 §11.5), `site.yml` (`releases/latest`, never a
  prerelease) and the updater (§8.4) never see it. A ruleset makes `deps-*` tags undeletable and immutable (§14).
- **The source archive** `ffmpeg-<ver>-r<rev>-source.tar` holds one top-level folder, `ffmpeg-<ver>-r<rev>-source/`,
  with: the four upstream tarballs exactly as downloaded (`src/`), `native/ffmpeg/{pins.json,build.sh,ffcheck.c,
  README-LGPL.txt}`, `deploy/windows/{msvc-env.ps1,tools.json}` (so the archive alone rebuilds the DLLs, the relink
  check of §9.1), `CONFIGURE.txt` (the configuration string `ffcheck` printed, the toolset and SDK, and how to build),
  and `SHA256SUMS` of the tarballs. The zip likewise has one top-level folder, `ffmpeg-<ver>-r<rev>-win-x64/`. Every
  isshoni release copies the source archive as `isshoni-desktop_<v>_ffmpeg-source.tar` (§11.1).
- **Consumers** (the `windows-native` job of both ci.yml and release.yml, `task ffmpeg:fetch`) download
  `https://github.com/MoonWX/isshoni/releases/download/<tag>/<asset>` and fail unless the SHA-256 matches
  `lock.json`; release.yml also runs `gh attestation verify <zip> --repo MoonWX/isshoni --signer-workflow
  MoonWX/isshoni/.github/workflows/ffmpeg.yml`. `task ffmpeg:fetch` (`go -C tools run ./desktoppkg ffmpeg-fetch`)
  works on the Mac too, into the git-ignored `.deps/ffmpeg/`.

### 4.6 Reproducibility

- Same pins, same `build.sh` and the same MSVC toolset give byte-identical DLLs: `/Brepro` removes timestamps,
  `-PDBALTPATH` the PDB path, the relative paths of §4.2 the build folder, the zip and tar are normalized, and
  `SOURCE_DATE_EPOCH` is the commit time.
- Every PR and manual run compares `build`'s DLLs with `rebuild`'s, and every run with `lock.json`'s while the lock
  describes these inputs (§4.5). A difference with **the same toolset** build is something non-deterministic; a
  difference with **another toolset build** (the runner image moved from one `14.44.<build>` to another, or a tool PR
  moved `toolset`) is a warning in the summary, since the shipped bytes stay the locked ones.
- **`FFMPEG_REPRO`**, the workflow-level `env` of `ffmpeg.yml`, decides what a same-toolset difference does: `strict`
  (the value now) fails `report`; `warn` makes it a warning and keeps it in the summary. If a same-toolset rebuild
  differs (in a PR's `rebuild`, a manual run or the weekly run) and `build.sh` can't remove the cause, a tool PR sets
  `FFMPEG_REPRO: warn`, and this section records why and what differs; `build-manifest.json` stays the record of the
  inputs, so the fallback is documented, not hidden. The first PR run's report summary (README WA07's "Results for
  the spec") is recorded here by README WB2: WB1 could not read it, since it lives on GitHub.

### 4.7 Security updates

- FFmpeg's security fixes come as point releases on the 9.0 branch. The owner watches FFmpeg's security page; a fix
  in `libavutil`, in one of the four encoders or in `hwcontext_d3d11va` means: bump `pins.json`, publish, update the
  lock, cut an isshoni patch release. Fixes in decoders, demuxers and filters don't apply to this build (D6), and the
  release notes say so when they skip one.

## 5. Building the app

### 5.1 `isshoni-media.dll` (job `windows-native`, in both ci.yml and release.yml)

- **MSVC environment**: the MSVC environment step of §3 (`deploy/windows/msvc-env.ps1`, as for §4.3: VS 2022 or
  2026 with the C++ tools, fail fast without toolset `14.44` or SDK `10.0.26100.0`, `vcvars64.bat
  -vcvars_ver=14.44`), writing `PATH`, `INCLUDE`, `LIB` and `LIBPATH` to `GITHUB_ENV` and printing the instance,
  toolset build and SDK it found; no third-party action.
- vcpkg: CI clones `https://github.com/microsoft/vcpkg` at the `builtin-baseline` commit of `native/vcpkg.json` into
  `$RUNNER_TEMP\vcpkg`, runs `bootstrap-vcpkg.bat -disableMetrics`, sets `VCPKG_ROOT` and `VCPKG_DISABLE_METRICS=1`.
  Manifest mode with triplet `x64-windows-static` (static libraries, static CRT; libopus is linked into the DLL).
- **Binary cache (CI only)**: `VCPKG_BINARY_SOURCES=clear;files,$RUNNER_TEMP\vcpkg-cache,readwrite` with
  `actions/cache` keyed on `hashFiles('native/vcpkg.json', 'native/vcpkg-configuration.json')`, the triplet and the
  toolset build the MSVC step printed (`14.44.<build>`), no restore keys. `release.yml` uses no cache (M1 06 §9.3); libopus builds in about a minute.
- FFmpeg: the locked zip is unpacked to `$RUNNER_TEMP\ffmpeg` and passed as `-DIM_FFMPEG_ROOT=…`. 04's CMake uses only
  `include/` from the zip (FFmpeg is loaded at run time, never import-linked, 04 §9.2); `lib/` stays informational.
  Until slice R3b, `IM_FFMPEG_ROOT` is optional (04's skeleton DLL builds without it).
- Build, with 04's presets (04 §13.1):
  1. **`win-x64-ci`** (CI only): `cmake --preset win-x64-ci -DIM_FFMPEG_ROOT=<unpacked locked zip>`,
     `cmake --build --preset win-x64-ci`, `ctest --preset win-x64-ci --output-on-failure` (04 §18.2's Windows tests;
     the job needs outbound network for 04's `openh264_test`).
  2. **`win-x64-release`**: `cmake --preset win-x64-release -DIM_FFMPEG_ROOT=… -DIM_VERSION=<v> -DIM_WINVER=<winver>
     -DIM_COMMIT=<short sha>` (§5.3), `cmake --build --preset win-x64-release`. `dll-check`, the notices and packaging
     use **only** this output. release.yml's `windows-native` builds `win-x64-release` only.
  3. `clang-format` runs in `native-host` (§10.1); `win-x64-asan` runs nightly (`windows-asan`, §10.3).
- **Checks** (`tools/desktoppkg dll-check`, PowerShell around `dumpbin` from the VS environment):
  - **ABI**: the names in `dumpbin /exports isshoni-media.dll` equal the functions declared with `IM_API` in
    `native/include/isshoni_media.h` (04 marks every exported function with that macro). An extra or missing export
    fails.
  - **Imports**: `deploy/windows/dll-imports.txt` has two sections, `static:` and `delay:`. Every DLL in `dumpbin
    /dependents` must be listed in the section that matches how it is imported. `mfplat.dll` and `avrt.dll` are
    allowed only under `delay:` (04 §13.1; mfplat is absent on N editions and Windows Server). The `api-ms-win-core-*`
    API sets that `windowsapp.lib` and `synchronization.lib` pull in are listed by 04 in slice E2; 05's audio DLLs by
    05; any other system DLL by PR with a reason. An import of `avcodec-*` or `avutil-*` **always fails**, like
    `VCRUNTIME*`, `MSVCP*`, `api-ms-win-crt-*`, `ucrtbase*`, debug CRTs and `opus*.dll`: the engine must load, probe
    and capture audio when FFmpeg is missing or blocked (04 §9.2, §13.3).
  - **Load config** (conditional): only if slice E2 (README WA11) shows that the delay-load helper honors
    `/DEPENDENTLOADFLAG:0x800` (04 §13.1), `dumpbin /loadconfig isshoni-media.dll` must show `DependentLoadFlags`
    0x800 (`LOAD_LIBRARY_SEARCH_SYSTEM32`); otherwise this check doesn't exist and 04's delay-load hook is the guard.
    WA11's PR records the verdict.
  - **Version resource**: `ProductVersion` = `<v>`, `FileVersion` = `<winver>`, `ProductName` = `isshoni`,
    `CompanyName` = `isshoni project`, `LegalCopyright` = `Copyright the isshoni authors. Apache-2.0.`,
    `OriginalFilename` = `isshoni-media.dll` (04's `.rc`, fed from `IM_VERSION` and `IM_WINVER`). SignPath requires
    product metadata on every file it signs.
  - The FFmpeg DLLs' SHA-256 equal `lock.json`'s (unmodified).
- **WebView2 bootstrapper**: downloaded from the `tools.json` URL; `Get-AuthenticodeSignature` must say `Valid` with
  exactly the pinned signer subject; its SHA-256 is recorded.
- Artifact `windows-native`, two folders:
  - `release\`: the `win-x64-release` `isshoni-media.dll` and `isshoni-media.pdb`, the FFmpeg `bin/` and `pdb/`,
    FFmpeg's `licenses/`, the vcpkg copyright files **from the `win-x64-release` build tree only** (its
    `vcpkg_installed/x64-windows-static/share/*/copyright`, built without 04's vcpkg feature `tests`, so no gtest) as
    `licenses/vcpkg/<port>/copyright`, `native/windows/third_party/openh264/LICENSE` as `licenses/openh264/LICENSE`,
    the C++/WinRT MIT license (committed by 04 as `native/windows/third_party/cppwinrt/LICENSE`) as
    `licenses/cppwinrt/LICENSE`, `MicrosoftEdgeWebview2Setup.exe`, and `native-manifest.json` (below).
  - `ci\` (ci.yml only): the `win-x64-ci` `isshoni-media.dll` with the FFmpeg DLLs next to it (the engine loads FFmpeg
    from its own folder), for `ISSHONI_TEST_ENGINE_DLL` (§10.1). It has 04's test hooks (fake encoder, fault
    injection, luma tap) and is never staged: staging and `smoke.ps1` require `testHooks: false` from `--self-check`
    (§2.4, §6.2).
- CI's `windows-native` also uploads `isshoni-native-tools` (kept 7 days): the `native:dist` zip of §17, for the owner's
  PC and the `hwcheck` run (§10.4), plus a `ci\` folder with the `win-x64-ci` `isshoni-media.dll` and the FFmpeg DLLs
  for the checks that need 04's luma tap (`isshoni-enginetest avsync -dll ci`, 04 §14, 05 §19.3). Never staged.

`native-manifest.json`:

```json
{"version": "0.2.0", "winver": "0.2.0.65000",
 "dll": {"name": "isshoni-media.dll", "sha256": "…", "abi": 65536},
 "ffmpeg": {"tag": "deps-ffmpeg-9.0.2-r1", "dlls": {"avcodec-<N>.dll": "…", "avutil-<N>.dll": "…"}},
 "vcpkg": {"baseline": "<commit>", "ports": {"opus": "1.5.2"}},
 "msvcToolset": "14.44.yyyyy", "vsInstance": "<installationVersion>", "windowsSdk": "10.0.26100.0",
 "webview2Bootstrapper": {"sha256": "…", "signer": "CN=Microsoft Corporation, …"}}
```

### 5.2 `isshoni.exe` (the Wails build)

`task desktop:exe:windows VERSION=<v> [TAGS=<tags>]`, the same on the Mac, on Linux CI and on Windows. `TAGS`
defaults to `production`, and the release passes nothing. Test builds pass other tags (03's e2e build: `TAGS=e2e`, which
03's `task desktop:build:e2e` wraps; the updater e2e: `TAGS="production updatetest"`) and are written to
`dist-desktop/windows_amd64-test/` instead, a folder that `tools/desktoppkg stage -release` refuses.

1. `.bin/wails3 generate syso -arch amd64 -icon cmd/isshoni-desktop/build/windows/icon.ico -manifest
   cmd/isshoni-desktop/build/windows/app.manifest -info <generated info.json> -out
   cmd/isshoni-desktop/rsrc_windows_amd64.syso` (git-ignored). The icon and the application manifest are 03's files;
   the manifest's contents are 03 §24's list: `asInvoker`, PerMonitorV2 DPI awareness, the Windows 10/11
   `supportedOS` GUID, Common Controls v6, `activeCodePage` UTF-8, `longPathAware`. `info.json` carries the version
   fields of §5.1 with `OriginalFilename` = `isshoni.exe` and `FileDescription` = `isshoni`. If the pinned wails3
   lacks `generate syso`, `github.com/tc-hib/go-winres` (0BSD) in `tools/go.mod` does the same.
2. `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags "$TAGS" -ldflags "-s -w -H windowsgui
   -X github.com/MoonWX/isshoni/internal/version.version=<v> -X …version.commit=<sha> -X …version.date=<commit date>"
   -o dist-desktop/windows_amd64/isshoni.exe ./cmd/isshoni-desktop` (test builds: `-o
   dist-desktop/windows_amd64-test/isshoni.exe`). `production` is Wails' release tag (no devtools). The SPA is `web.Dist()`, the same `web/dist` as the server's, built with the same `ISSHONI_VERSION` (M1
   06 §7.2). If 03 needs a desktop-specific SPA build, the web job builds it as a second artifact and this step embeds
   that one; nothing else here changes.
3. Checks (on every default-`TAGS` build and again in `stage -release`): `go version -m` shows `-trimpath`,
   `CGO_ENABLED=0`, `GOOS=windows`, the version ldflag, and the build tags as **exactly** `production`; it fails on
   `dev` (03: adds `http://localhost:5173` to the CSP), `e2e` (03: keeps the `WEBVIEW2_*` variables, so another
   program could open a remote-debugging port and read access tokens, and allows http on loopback) or `updatetest`.
   The binary must contain neither `ISSHONI_UPDATE_TEST` (§8.11) nor `localhost:5173`. Size warning above 45 MB.

**Build constraints (D18)**, required of 03: the real `main` files of `cmd/isshoni-desktop` carry
`//go:build windows || (darwin && cgo)`, and one `main_other.go` with `//go:build !windows && !(darwin && cgo)` prints
"isshoni-desktop runs on Windows and macOS; Linux uses isshoni-agent" and exits 1. Packages under `internal/` that
the app uses but that don't import Wails build everywhere, so the client core tests run on Linux with the fake engine.

### 5.3 Windows version numbers (`winver`)

PE version resources and NSIS's `VIProductVersion` need four 16-bit numbers. `tools/desktoppkg winver <semver>`:

| SemVer | winver | Rule |
|---|---|---|
| `0.2.0` | `0.2.0.65000` | a final release: W = 65000 |
| `0.2.0-rc.2` | `0.2.0.2` | a prerelease whose last identifier is a number N: W = N mod 65000 |
| `0.0.1-ci.1234` | `0.0.1.1234` | same rule (CI and dry-run builds) |
| `0.0.0-dev+1a2b3c` | `0.0.0.0` | any other prerelease |

X, Y and Z above 65535 are an error. An rc sorts below its final release, which is all NSIS's `${VersionCompare}`
needs (§6.6).

### 5.4 On the Mac

- `task desktop:exe:windows` cross-compiles `isshoni.exe` (the Windows build needs no cgo).
- `task desktop:package:windows NATIVE_DIR=<dir>` builds an installer with Homebrew's `makensis`, using
  `WriteUninstaller` (no prebuilt uninstaller, which would need Windows to run); `<dir>` holds `isshoni-media.dll` and
  the FFmpeg DLLs, from a CI run's `isshoni-desktop-windows-pr` artifact (downloaded in the browser) or from the
  owner's Windows PC. The result installs on the PC for manual tests; it is never released.
- The DLL itself builds only with MSVC: on the Windows PC (`task native:build`, presets `win-x64-release` and
  `win-x64-ci`, with VS 2022 17.14 Build Tools or VS 2026 Build Tools plus the 14.44 x64 component, §3) or in CI. The portable part (`native/common`) builds and tests on the Mac with `task native:test`
  (preset `host-debug`).

## 6. The installer (NSIS)

### 6.1 Files

```
deploy/windows/
  tools.json                 pins (§3)
  msvc-env.ps1               the MSVC environment step (§3), shared by every MSVC build
  dll-imports.txt            the DLL import allowlist (§5.1)
  nsis/installer.nsi         the installer (MUI2)
  nsis/uninst-gen.nsi        prebuilt-uninstaller mode: an exe that writes uninstall.exe and quits (§6.9)
  nsis/common.nsh            defines, registry paths, macros: mutex wait, OS checks, file list, flags
  nsis/uninstall.nsh         the uninstall section, included by both scripts
  nsis/lang/English.nsh      every user-visible string as a LangString
deploy/test/windows/smoke.ps1   install/upgrade/uninstall assertions: -Mode ci, -Mode release (verify-windows),
                                -Mode update (the updater e2e's cases, §16)
tools/desktoppkg/            winver, stage, nsis defines, check, manifest, ffmpeg-inputs, ffmpeg-fetch, dll-check
```

Only built-in NSIS plugins are used (System, nsDialogs, Banner, plus the headers LogicLib, FileFunc, WordFunc, WinVer,
x64, MUI2). `makensis -WX` (warnings are errors). `SetCompressor /SOLID lzma`. `Unicode true`.

### 6.2 Defines passed by `tools/desktoppkg nsis`

`VERSION` (SemVer), `WINVER`, `STAGE` (the staged tree), `OUTFILE`, `PREBUILT_UNINSTALLER` (path, CI only),
`WEBVIEW2_SETUP` (path). `VIProductVersion ${WINVER}`; `VIAddVersionKey` `ProductName` `isshoni`, `ProductVersion`
`${VERSION}`, `FileVersion` `${WINVER}`, `CompanyName` `isshoni project`, `LegalCopyright` (as §5.1),
`FileDescription` `isshoni setup`. `uninstall.exe` gets the same keys with `FileDescription` `isshoni uninstaller`.

**Staging** (`tools/desktoppkg stage [-release]`): copies exactly the §2.2 tree into `dist-desktop/stage/`, converts
text files to `.txt` names, sets every file's mtime to the commit time (so the installer is reproducible for the same
makensis), and writes `install-files.txt`. It fails on a file it doesn't expect, and on a missing one. It takes the DLL
only from the `windows-native` artifact's `release\` folder. On Windows it then runs the staged `isshoni.exe
--self-check` and fails unless `ok` and `engine.testHooks` is false (a `win-x64-ci` DLL can never reach a stage); on
the Mac (local packages, never released) that step is skipped with a note. `-release` (release.yml's `desktop-pkg`,
and the `windows-desktop` smoke installer) also re-runs §5.2 step 3 on the staged exe; the updater e2e's
`updatetest` builds are staged without it.

### 6.3 Pages (interactive mode)

1. **Welcome**: one paragraph ("isshoni shares your screen and sound with friends, without your voice apps"),
   "Needs Windows 10 22H2 or later", a link to `https://moonwx.github.io/isshoni/download#windows`.
2. **Options** (nsDialogs): `[x] Create a desktop shortcut`, `[x] Check for updates automatically (asks GitHub once a
   day; see the privacy page)` with a link to `/privacy#desktop`. No directory page: the folder is fixed per user
   (`/D=` exists for those who want another one, §6.4).
3. **Install** (progress).
4. **Finish**: `[x] Start isshoni` and one line, "Next: go back to your browser and click Open isshoni" (the
   `/download` page's step 3; with the checkbox on, the app's first window says the same, 03 §8.2).

No license page: Apache-2.0 asks for no acceptance, and the plan's rule is that the user isn't asked to accept terms;
the licenses are installed in `licenses\`. The uninstaller has a confirm page with `[ ] Also remove my settings and
logs on this PC` (unchecked) and a progress page. The sign-ins (the linked devices and their credentials) are removed
by every uninstall, checked or not (§6.8).

### 6.4 Command line and exit codes

| Flag | Effect |
|---|---|
| `/S` | silent (NSIS) |
| `/UPDATE` | an update run (from the updater): no pages, the shortcut and update choices stay as the registry has them, waits up to 60 s for the app to exit (§6.7). With `/S` it still shows one small non-interactive window for its whole run, `$(UpdatingBanner)` = "Updating isshoni to ${VERSION}…" (the built-in Banner plugin: `Banner::show` in `.onInit` right after the installing mutex, `Banner::destroy` right before the app is started or the installer exits, on success and on every error path); no buttons. If slice R4 finds that `Banner::show` displays nothing under `/S`, the fallback is recorded there: in `/UPDATE` mode the installer runs without `/S` and shows only the instfiles page (`AutoCloseWindow true`, no other page, no Cancel), and D2, §8.7 and `apply_windows_test.go` change from `/S /UPDATE /RELAUNCH` to `/UPDATE /RELAUNCH` together. Why: the start-time update (§8.7 (b)) otherwise shows nothing from the friend's click until the relaunched window, and Discord, the model here, shows an updating window too |
| `/RELAUNCH` | after a successful install, start `isshoni.exe --updated-from=<old version>` |
| `/LAUNCH` | after a successful install, start `isshoni.exe` (the ps1 uses it) |
| `/SERVER=<url>` | write `PendingServer` (§2.3); `https://` URLs only, or `http://` loopback (for a dev server's ps1), at most 2048 bytes, else ignored with a log line. The installer only filters; the app validates the value with 02's `deeplink.NormalizeOrigin` and a release app drops an `http://` value (§2.3) |
| `/DESKTOPSHORTCUT=0\|1` | silent default 0 on a fresh install; ignored on upgrades (the stored choice wins) |
| `/NOUPDATECHECKS` | write `UpdateChecks` = 0 |
| `/ALLOWDOWNGRADE` | allow installing an older version over a newer one |
| `/D=<dir>` | install folder on a **fresh** install (NSIS; must be last, unquoted); ignored when an install exists |

A started app never runs elevated: when the installer runs elevated (an administrator PowerShell), `/LAUNCH`,
`/RELAUNCH` and the Finish checkbox start the app through `"$WINDIR\explorer.exe" "$INSTDIR\isshoni.exe"`, which
starts it with the shell's unelevated token (no arguments can pass that way, so after an elevated update the app
starts without `--updated-from`).

| Exit code | Meaning (installer) |
|---|---|
| 0 | installed |
| 1 | cancelled by the user |
| 2 | other error (NSIS default) |
| 3 | isshoni is still running (60 s in `/UPDATE`, or the user cancelled the "close isshoni" prompt) |
| 4 | a newer version is installed and `/ALLOWDOWNGRADE` was not given |
| 5 | unsupported Windows: build below 19041, or a 32-bit OS |
| 6 | a file could not be written |
| 7 | the WebView2 runtime is missing or older than 120.0 (§6.5), and its installer didn't fix that |
| 8 | another isshoni setup is running (the named mutex `Local\io.isshoni.desktop.installing` already existed in `.onInit`, §6.7); nothing was changed. Interactive: a message box `$(AnotherSetupRunning)` "Another isshoni setup is running. Wait for it to finish, then try again." first |

Uninstaller: `/S`, `/PURGE` (run `isshoni.exe --purge-data` instead of `--forget-credentials`, then remove everything,
the data root included; §6.8); exit 0, 1 (cancelled), 3 (running).

### 6.5 System checks

- **OS**: `${AtLeastBuild} 19041` (WinVer.nsh) or exit 5; builds 19041 to 19044 get a warning page ("Windows 10 22H2
  or later is supported; this may work") in interactive mode and proceed in silent mode. A 32-bit OS → exit 5.
  `${IsNativeARM64}` → a warning ("the x64 app runs under emulation; not tested"), then proceed.
- **WebView2**: present when `pv` of `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}` under
  `HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` or `HKCU\Software\Microsoft\EdgeUpdate\Clients\` exists and
  is at least `${WEBVIEW2_MIN}` = `120.0.0.0` (`${VersionCompare}`); `0.0.0.0`, a missing value or an older version
  counts as missing. `WEBVIEW2_MIN` is defined once in `common.nsh` with a comment naming 03 §5.4, whose runtime
  minimum (Evergreen 120.0; older → message box and exit 1) it must equal; a stale runtime (updates blocked) must not
  install cleanly and then refuse to start. When missing, the embedded `MicrosoftEdgeWebview2Setup.exe /silent
  /install` runs (per-user when not elevated; it downloads the runtime from Microsoft) with up to 15 minutes, then the
  check repeats; still missing or still too old → a message with Microsoft's WebView2 download page (interactive) or
  exit 7.

### 6.6 Fresh install, upgrade in place, downgrade

- **Existing install**: `InstallDir` from `HKCU\Software\isshoni` (also the Uninstall key's `InstallLocation`). The
  installer always installs into it.
- **Version compare**: `${VersionCompare}` on the installed `Version`'s winver (re-derived by the same rule, stored in
  `HKCU\Software\isshoni\WinVer`) against `${WINVER}`. Older installed → upgrade. Same → reinstall (repair). Newer →
  exit 4 unless `/ALLOWDOWNGRADE`.
- **Stale files**: after writing the new files, the installer reads the previous `install-files.txt` and deletes every
  listed file that the new list doesn't have (for example `avcodec-62.dll` after a move to `avcodec-63.dll`). A listed
  path with `..`, a drive letter or a leading `\` is skipped and logged. Then the new list is written.
- **Never recursive deletes in `$INSTDIR`**: neither the installer nor the uninstaller ever runs `RMDir /r $INSTDIR`;
  folders are removed with plain `RMDir` only when empty. A user who chose `/D=C:\Users\me\Desktop` loses nothing but
  isshoni's files.

### 6.7 A running app

- The installer checks `OpenMutexW(SYNCHRONIZE, FALSE, "Local\io.isshoni.desktop.running")` through the System plugin.
  Interactive: a Retry/Cancel box "isshoni is running. Quit it from the tray icon, then click Retry." (Cancel → exit
  3). `/UPDATE`: poll every 500 ms for up to 60 s, then exit 3.
- After the mutex is gone, the exiting process may still map its images for a moment: `isshoni.exe`, and also
  `isshoni-media.dll` and the FFmpeg DLLs, since 04 never calls `FreeLibrary` (04 §5.3, §12.4) and `im_stop` may leave
  a thread stuck in a driver call (04 §6.3). Overwriting a still-mapped DLL fails with a sharing violation, which
  would exit 6 and spend one of the updater's 2 attempts. So for **every PE file** of the previous `install-files.txt`
  that the new install overwrites (`isshoni.exe`, `isshoni-media.dll`, `avcodec-*.dll`, `avutil-*.dll`), the
  installer renames `<file>` to `<file>.old` in a loop for up to 10 s (a mapped image can be renamed but not
  overwritten; the retries cover other open handles, such as a virus scanner's), then writes the new file and deletes
  each `.old` (or leaves it for the next run's stale-file pass, §6.6, if Windows still holds it). A rename that still
  fails after 10 s exits 6.
- 04 §13.4 gets one sentence: "DllMain does no work on `DLL_PROCESS_DETACH` (no joins or waits under the loader
  lock), so the engine never delays process exit".
- **The installing mutex** (§2.4): the first statement of `.onInit`, before any other check, is
  `System::Call 'kernel32::CreateMutexW(p 0, i 0, w "Local\io.isshoni.desktop.installing") p .r0 ?e'`, then `Pop $1`
  (the last error, which `?e` pushes) and `StrCpy $InstallingMutex $0` (`Var InstallingMutex` in `common.nsh`).
  `$1` = 183 (`ERROR_ALREADY_EXISTS`) → close the handle, `SetErrorLevel 8`, `Quit` (§6.4). Otherwise the handle is
  closed with `kernel32::CloseHandle` only right before the installer starts the app (`/LAUNCH`, `/RELAUNCH` or the
  Finish checkbox, its last action; otherwise the handle closes when the process exits). It is created with `bInitialOwner` = FALSE and used, like the running mutex, as a named object whose
  existence is the signal. Closing it before starting the app matters: the relaunched app's step 2a (03 §13.4) would
  otherwise see it and exit. The order in `/UPDATE` mode is: installing mutex, banner, wait for the running mutex
  (60 s), install, close the installing mutex, relaunch. Effects: two installers never write the same files (a second
  start of the old app during an update can't reach `Apply`, and a second setup exits 8); a start during an install
  can't map half-written FFmpeg DLLs or take the running mutex that the installer waits for. During an interactive
  setup a start of the installed app exits 0 the same way; the setup's own window is on screen.

### 6.8 Uninstall

Removes, in order:
1. Waits for the mutex (§6.7).
2. **Always** (also under `/S`) runs `"$INSTDIR\isshoni.exe" --forget-credentials` (02 §11.7's `Servers.ForgetAll`:
   every linked device is revoked where reachable, under one 5 s budget for all servers, and every `isshoni:v1:*`
   credential deleted), so no live 90-day refresh token stays in Credential Manager and no dead device on the Devices
   page. With `/PURGE` or the checkbox it runs `"$INSTDIR\isshoni.exe" --purge-data` instead (the same `ForgetAll`,
   then the data root). Either has a 30 s cap; a timeout or a non-zero exit is logged and the uninstall continues.
3. The files listed in `install-files.txt` (and any `<file>.old` left by §6.7), then `install-files.txt`,
   `uninstall.exe` (NSIS deletes itself from its temp copy), then empty folders; both shortcuts.
4. The Uninstall key; `HKCU\Software\Classes\isshoni` **only when its command points into `$INSTDIR`** (another
   install may own it); the `Run` value `isshoni`.
5. The toast registration that Wails' notifications service wrote at run time (03 §15): read `CustomActivator` from
   `HKCU\Software\Classes\AppUserModelId\io.isshoni.desktop`; delete `HKCU\Software\Classes\CLSID\<guid>` (with its
   subkeys) **only when its `LocalServer32` points into `$INSTDIR`** (a COM server pointing at a deleted exe would
   otherwise stay); then `DeleteRegKey HKCU "Software\Classes\AppUserModelId\io.isshoni.desktop"`.
6. `DeleteRegKey HKCU "Software\isshoni"`: the whole key, values and subkeys (for example `NotificationCategories`).

The data root of §2.2 stays unless purged.

### 6.9 Prebuilt-uninstaller mode (SignPath-ready)

`uninst-gen.nsi` includes `uninstall.nsh` and, when run with `/S`, calls `WriteUninstaller "$EXEDIR\uninstall.exe"` and
quits. CI compiles it, runs it on the Windows runner, and passes the result to `installer.nsi` as
`PREBUILT_UNINSTALLER`, which then uses `File` instead of `WriteUninstaller`. In 0.x nothing happens between the two
steps; after SignPath approval, a signing request for `isshoni.exe`, `isshoni-media.dll` and `uninstall.exe` slots in
between, and a second one for the finished installer (§12.4). Without the define (the Mac), `WriteUninstaller` is used.

### 6.10 Strings

Every visible string is a `LangString` in `nsis/lang/English.nsh` (i18n-ready the NSIS way: another language is one
more file); `tools/desktoppkg check` fails when `installer.nsi`, `common.nsh` or `uninstall.nsh` has a quoted literal
in `MessageBox`, `DetailPrint`, `nsDialogs::Create*`, `Banner::show` or `!define MUI_*TEXT*` that is not a `$(…)`
reference. The NSIS
strings are not in the SPA's `en.json`: the installer is no Go or web code.

## 7. `install-desktop.ps1` and the server's `/download`

### 7.1 The script (`deploy/install-desktop.ps1`)

Usage, as the `/download` page shows it: `irm https://<server>/install-desktop.ps1 | iex`.

Rules:
- Windows PowerShell 5.1 compatible (no `??`, ternaries, `&&`, `-Parallel`), **ASCII only**, LF line ends.
- The body is one `& { … }` block, so its variables don't leak into the user's session, with `Set-StrictMode
  -Version 3`, `$ErrorActionPreference = 'Stop'` and `$ProgressPreference = 'SilentlyContinue'` (Windows PowerShell's
  progress bar makes `Invoke-WebRequest` many times slower).
- **It never calls `exit`**: under `iex` that would close the user's window. Errors are `Write-Host` lines prefixed
  `isshoni:` followed by `return`.
- It writes only to a new temp folder `$env:TEMP\isshoni-install-<guid>`, removed in `finally`.

Layout:

```powershell
# isshoni desktop installer for Windows. Usage: irm https://<your isshoni server>/install-desktop.ps1 | iex
# Docs: https://moonwx.github.io/isshoni/download#windows-powershell
# >>> isshoni-prefill (the server rewrites this block; see docs/m2/06-windows-release.md 7.4)
$IsshoniServer  = ''
$IsshoniVersion = '@VERSION@'
# <<< isshoni-prefill
$AllowedSigners = @'
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA... isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA... isshoni-release-backup-1
# END ALLOWED SIGNERS
'@
& { … }
```

`@VERSION@` is stamped by the release (`task release:prepare`, like `install.sh`'s `ISSHONI_INSTALLER_VERSION`), and
the key block is written by `task release:keys` and checked by `task lint:keys`, exactly as for `install.sh` (M1 06
§4.3). The release asset `install-desktop.ps1` is this file stamped; the server's copy is the same file with the
prefill block rewritten (§7.4).

Flow:
1. **Checks**: `$PSVersionTable.PSVersion` ≥ 5.1; `[Environment]::OSVersion.Version.Build` ≥ 19041 (warn below
   19045); `[Environment]::Is64BitOperatingSystem`; `$env:PROCESSOR_ARCHITECTURE` `AMD64` (or `ARM64` with the
   emulation warning). Running elevated is allowed; the app is then not started (§6.4) and the script says "Start
   isshoni from the Start menu". **Smart App Control**, before anything is downloaded: read the DWORD
   `VerifiedAndReputablePolicyState` under `HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy`
   (`Get-ItemProperty … -ErrorAction SilentlyContinue`; a missing key or value, as on Windows 10, means off). `1`
   (on) → print "isshoni: Smart App Control is on, and this version of the app can't run on your PC yet: share from
   Chrome or Edge instead. More: https://moonwx.github.io/isshoni/download#windows-smart-app-control", then "Nothing
   was installed." and return; `2` (evaluation) and `0` (off) proceed. Why: Smart App Control blocks unsigned 0.x
   builds with no override (§12.1), so saying so up front saves the friend the attempt.
2. **TLS**: `[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor
   [Net.SecurityProtocolType]::Tls12`. This runs only after the user's own `irm https://<server>/install-desktop.ps1`
   has fetched the script from an isshoni server, whose Go TLS stack requires TLS 1.2. If a stock Windows PowerShell
   5.1 doesn't offer TLS 1.2 by default, that first request fails before any isshoni code runs ("The underlying
   connection was closed"), so the displayed command then carries the same setting as a prefix (§7.5 `ps1Command`);
   which case holds is measured on a clean Windows 10 22H2 in slice R10 and again in §11.8 item 7(b). This step stays
   either way, for the GitHub downloads.
3. **Version**: `$env:ISSHONI_VERSION`, else `$IsshoniVersion` (when it is not the literal `@VERSION@`), else the
   latest release through the redirect of `$Base/latest` (`…/releases/tag/vX.Y.Z`), as `install.sh` does (no API).
   Must match `^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`.
4. **Download** with `Invoke-WebRequest -UseBasicParsing -OutFile` (3 attempts, 2 s apart) from
   `$Base/download/v<v>/`, where `$Base` is `$env:ISSHONI_DOWNLOAD_BASE` or `https://github.com/MoonWX/isshoni/releases`:
   `isshoni-desktop_<v>_windows_amd64_setup.exe`, `checksums.txt`, `checksums.txt.sig`.
5. **Verify the signature** (§7.2). Any failure: "Nothing was installed." and return.
6. **Verify the file**: exactly one line of `checksums.txt` is `<64 hex>  <name>` or `<64 hex> *<name>` for the whole
   name; it must equal `(Get-FileHash -Algorithm SHA256 $file).Hash` (case-insensitive).
7. **Install**: `Start-Process -FilePath $setup -ArgumentList $a -Wait -PassThru`, inside `try { … } catch { … }`:
   any launch failure (Smart App Control, WDAC or AppLocker blocking the file, an antivirus quarantine, a file that is
   not a valid executable) prints "isshoni: Windows blocked the installer (<$_.Exception.Message>)" and the link
   `https://moonwx.github.io/isshoni/download#windows-smart-app-control`, then "Nothing was installed." and returns,
   instead of a raw red exception under `$ErrorActionPreference = 'Stop'` right after "signature OK". Arguments:
   `/S`, `/SERVER=<server>` when `$IsshoniServer` is set, `/NOUPDATECHECKS` when `$env:ISSHONI_NO_UPDATE_CHECKS` is
   `1`, `/ALLOWDOWNGRADE` when `$env:ISSHONI_ALLOW_DOWNGRADE` is `1`, `/LAUNCH` unless elevated or
   `$env:ISSHONI_NO_LAUNCH` is `1`. Exit codes map
   to messages (§6.4): 3 "Quit isshoni from its tray icon and run the command again", 4 "A newer isshoni is installed
   (set ISSHONI_ALLOW_DOWNGRADE=1 to replace it)", 5 "This Windows version is not supported", 7 "WebView2 could not be
   installed: <link>", 8 "Another isshoni setup is running; run the command again when it has finished".
8. "isshoni <v> is installed." plus "It is starting now." or the Start menu hint.

`Start-Process -ArgumentList` in Windows PowerShell 5.1 joins arguments with spaces and quotes nothing, and `$env:TEMP`
contains the user's name, which may have spaces: every path argument is passed through a helper that wraps it in
double quotes.

### 7.2 Verification with the inbox OpenSSH

- `ssh-keygen.exe` is taken from `$env:SystemRoot\System32\OpenSSH\` (the inbox OpenSSH Client, an optional feature
  that current Windows 10 and 11 install by default; Windows 10 22H2 is expected to ship `OpenSSH_for_Windows_8.1p1`,
  the first version with `-Y verify`, and Windows 11 24H2 a 9.x; §11.8 item 7 confirms both on the owner's PC), else
  from `Get-Command ssh-keygen.exe -CommandType Application` (Git for Windows).
- The script writes `$AllowedSigners` to `allowed_signers` (UTF-8 without BOM, LF) and runs
  `Start-Process -FilePath $sk -ArgumentList '-Y','verify','-f',"`"$allowed`"",'-I','isshoni-release','-n',
  'isshoni-checksums','-s',"`"$sig`"" -RedirectStandardInput $sums -RedirectStandardOutput $o -RedirectStandardError
  $e -NoNewWindow -Wait -PassThru`. `-RedirectStandardInput` passes the file's bytes unchanged, which a PowerShell pipe
  would not (it re-encodes text). Exit code 0 is required; the "Good … signature … SHA256:…" line is printed so the
  user sees the fingerprint.
- **No `ssh-keygen`**: fail closed with "Windows' OpenSSH Client is needed to check the download (Settings → System →
  Optional features → OpenSSH Client), or download the installer from your server's Download page", linking
  `/download#windows-verify`. An `ssh-keygen` older than 8.1 shows a usage error, which the script names ("your
  ssh-keygen is older than OpenSSH 8.1").
- **Constraint from Windows 10's 8.1p1**: `deploy/keys/allowed_signers` may use only options that 8.1 parses
  (`namespaces=`, `cert-authority`). A rotation that needs `valid-before=` (OpenSSH 8.8+, M1 06 §9.4) removes the old
  key line instead; `repocheck keys` fails on any other option. M1 06 §9.4's rotation text gets this note.
- **Later (after SignPath)**: when `ssh-keygen` is missing, a valid Authenticode signature whose subject is SignPath
  Foundation's certificate for isshoni is accepted as the fallback (`Get-AuthenticodeSignature`), never instead of a
  present `ssh-keygen`.

### 7.3 Environment names

`ISSHONI_VERSION` and `ISSHONI_DOWNLOAD_BASE` mean what they mean for `install.sh`. New: `ISSHONI_NO_UPDATE_CHECKS`,
`ISSHONI_NO_LAUNCH`, `ISSHONI_ALLOW_DOWNGRADE`, and for the updater's test builds `ISSHONI_UPDATE_TEST_BASE`,
`ISSHONI_UPDATE_TEST_SIGNERS` (§8.11), for tests 04's `ISSHONI_TEST_ENGINE_DLL` (the path of the `win-x64-ci`
`isshoni-media.dll`, 04 §12.1, §18.2; §10.1), `ISSHONI_COMPAT_SERVER_URL` and `ISSHONI_COMPAT_ADMIN_FILE` (§10.3), and
for the release notes `ISSHONI_DESKTOP_MIN_VERSION` and `ISSHONI_FFMPEG_VERSION` (§11.7). They go onto the server's
reserved env list (M1 04 §4.2: `reservedEnv` in `internal/server/config/keys.go`, which per its comment must match
this list) so the server never warns about them.

### 7.4 Server: `GET /install-desktop.ps1`

M1 reserved the path (M1 04 §9.2). M2 adds:

```go
// Package deploy embeds the files the server serves from deploy/ (M2: the desktop install script).
package deploy

import _ "embed"

//go:embed install-desktop.ps1
var InstallDesktopPS1 []byte
```

```go
package httpapi

// DesktopScripts is what the server needs to serve the desktop install scripts.
type DesktopScripts struct {
	PS1     []byte // deploy.InstallDesktopPS1
	Origin  string // config.Site.Origin, e.g. "https://watch.example.com"
	Version string // internal/version.Version()
}

// RouterOptions gains:
//	Desktop *DesktopScripts // nil: /install-desktop.ps1 answers 404 (tests that don't set it)

// RenderInstallPS1 rewrites the prefill block: exactly one "# >>> isshoni-prefill" line and one
// "# <<< isshoni-prefill" line after it, else an error.
func RenderInstallPS1(tmpl []byte, origin, version string) ([]byte, error)
```

- The block becomes:

  ```powershell
  # >>> isshoni-prefill (written by the isshoni server at https://watch.example.com)
  $IsshoniServer  = 'https://watch.example.com'
  $IsshoniVersion = '0.2.0'
  # <<< isshoni-prefill
  ```

  `$IsshoniServer` is `origin` with every `'` doubled, kept only when it is ASCII, has no control characters and is
  `https://…` or `http://` with a loopback host (a dev server); otherwise empty, with one WARN at startup.
  `$IsshoniVersion` is the server's version when it matches `^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`, else empty
  (a dev, CI or e2e build: the script installs the latest release).
- The wiring renders once at startup (origin and version are fixed for the process, M1 04 §6.2) and fails startup
  (exit 78) if the template has no valid block: that is a build defect, and a test catches it first.
- Response: `200`, `Content-Type: text/plain; charset=utf-8` (Windows PowerShell decodes text without a charset as
  ISO-8859-1), `Cache-Control: no-cache`, `ETag` = first 16 hex of the body's SHA-256, `Content-Length`. `HEAD` works
  (Go's GET pattern). No auth: it says nothing that `/api/v1/info` doesn't.
- `GET /install-desktop.sh` answers `404` with `text/plain` "Not available yet." until M3, registered explicitly so the
  SPA fallback can never serve `index.html` to `curl … | sh`.
- Owners: `deploy/embed.go` and `internal/server/httpapi/desktopscripts.go` (new files, this document); the
  registration in the router and the wiring line in `internal/server/wire.go`. `deploy` may be imported only by
  `internal/server` (M1 04 §2's import table and its depguard rule gain the row).

### 7.5 The `/download` page (SPA, `web/src/download/`)

M1's page is a placeholder (05 §15.4). M2 rewrites it; it stays public (it needs only `/api/v1/info`).

`web/src/download/links.ts`:

```ts
export interface DesktopLinks {
  release: boolean;              // server.version is a release version (the regex of §7.4)
  version: string;               // server.version
  windowsInstallerUrl: string;   // release ? exact asset URL : 'https://github.com/MoonWX/isshoni/releases/latest'
  ps1Command: string;            // `irm ${publicUrl}/install-desktop.ps1 | iex`, or, if R10's measurement (below)
                                 // finds TLS 1.2 off by default, the TLS-prefixed form below
  releasePageUrl: string;        // …/releases/tag/v<version>, or …/releases/latest
  openAppUrl: string;            // appLinkFor(publicUrl, roomId) from web/src/lib/appLink.ts (02 §10.2, §12);
                                 // "" unless https://
}
// roomId: the page's ?room= value (03 §12.3 links /download?room=<id>), passed only when it matches 02 §12's room-id
// rule; an invalid or absent value gives the plain server link.
export function desktopLinks(serverVersion: string, publicUrl: string, roomId?: string): DesktopLinks;
```

The exact installer URL is
`https://github.com/MoonWX/isshoni/releases/download/v<version>/isshoni-desktop_<version>_windows_amd64_setup.exe`.

`ps1Command` is one of two exact strings, chosen once by slice R10's measurement on a clean Windows 10 22H2
(§11.8 item 7(b) describes it) and asserted exactly by `links.test.ts`:
- TLS 1.2 in a fresh `powershell.exe`'s default `[Net.ServicePointManager]::SecurityProtocol`:
  `irm https://watch.example.com/install-desktop.ps1 | iex`;
- otherwise: `[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072; irm
  https://watch.example.com/install-desktop.ps1 | iex` (one line; 3072 is `[Net.SecurityProtocolType]::Tls12`, the
  number keeps the line short).

The same form (with `<your server>` in place of the origin) appears everywhere the command is shown: the script's header
comment (§7.1), the project site's `#windows-powershell` and the release notes header (§11.7). `desktopLinks` never
encodes the link itself: `openAppUrl` is `appLinkFor(publicUrl, roomId)` (02's one helper for the deep-link grammar of
02 §12), so for `https://watch.example.com` it is `isshoni://watch.example.com/`, and with a valid `?room=k3j9x2m4p7q8`
it is `isshoni://watch.example.com/r/k3j9x2m4p7q8`; `links.test.ts` asserts both exact strings and that an invalid
`?room=` (too long, other characters, a second value) gives the plain link. Links are https-only (02 §12.1), so for an
`http://` `publicUrl` (a dev server on `http://localhost`) it is `""` and the button is hidden.

Page content:
- **Windows** (first when the browser reports Windows): three numbered steps that stay visible the whole time, so a
  friend who comes back from the installer knows what's next: **1 Download** ("Download for Windows", the installer
  URL), **2 Run it** (the warning lines below), **3 Open isshoni** (the "Open isshoni" button, disabled with "after
  the install" until the download was clicked, hidden when `openAppUrl` is `""`). Then the requirements
  "Windows 10 22H2 or later, 64-bit", "a current GPU driver (NVIDIA 531.61 or newer for NVIDIA encoding)" and "Windows
  N editions: install the Media Feature Pack" linking `/troubleshooting#desktop-gpu-driver` and
  `#desktop-media-feature-pack` on the project site (04 §19); three warning lines, in the order the friend meets
  them: "Your browser may say the file isn't commonly downloaded: choose Keep (Edge: … → Keep → Show more → Keep
  anyway)" and "Windows may warn that the app is unrecognized: choose More info → Run anyway", both linking
  `/download#windows-smartscreen` on the project site, and "If Windows says Smart App Control blocked it, this version
  can't run on your PC yet: share from Chrome or Edge instead" linking `/download#windows-smart-app-control` (§12.1:
  no override exists there for unsigned 0.x builds; the line is revisited when signed builds ship, R18, §20); a
  second block "Or install with PowerShell (no warning, and the app opens set up for this server; you approve it once
  in this browser)" with the
  `ps1Command` and a Copy button; step 3's **"Open isshoni"** button has `href` = `openAppUrl` (03's wizard then
  shows the add step for this server, with the room when one was passed).
- **macOS** "Coming (M3)"; **Linux** "Coming (M4)"; a link to the project site's `/download`.
- On a non-release server: "This server is a development build (<version>); the app comes from the latest release"
  and the latest-release link.
- The page renders only in browsers: in the desktop app, 03 §7.5 (D7) wraps `/download` in `RequireBrowser`, which
  opens it in the system browser, so there is no desktop variant.
- Texts are `account.download.*` in the lazy `account` namespace, as in M1.

### 7.6 How a server knows the matching desktop version

It is its own version: one tag builds the server, the SPA and the app (D13, D14), so
`/api/v1/info`'s `server.version` names the release whose Windows asset matches. No lookup, no config key, nothing
the server fetches. A server older than M2 has no desktop page; a dev, CI or e2e build links to the latest release. The
app installed this way then follows its own update channel, which may move it past the server's version; that is
allowed by the compatibility rule (§11.6).

## 8. The updater (`internal/update`)

### 8.1 Trust model

- **Root of trust**: two ssh-ed25519 public keys embedded in the app, `isshoni-update-1` (active; its private half is
  the secret `RELEASE_UPDATE_SIGNING_KEY` of the `release` environment) and `isshoni-update-backup-1` (offline, with a
  passphrase). A manifest signed by either verifies.
- **Source**: GitHub Releases only (plan), over HTTPS; redirects are followed only to `https://github.com/` and
  `https://*.githubusercontent.com/` (where GitHub serves assets), at most 5. The server the app is linked to is never
  asked.
- **What the signature covers**: the manifest bytes, which name the installer's URL, size and SHA-256. The installer
  is checked against them before it runs, and again right before launch.
- **Rollback and freeze**: never installs a version ≤ the running one; refuses a manifest past `expiresAt`.
- **Key revocation**: a manifest's `revokedKeys` lists key fingerprints the app must stop trusting. It is honoured
  **only when the manifest's signature verifies with the offline backup key** (an embedded key marked backup, §8.3);
  in a manifest signed by any other key the list is ignored (one INFO line with the count, no error). The backup key
  itself is never revoked through the list: an entry naming it is dropped, and the backup changes only through an app
  release that embeds a new backup (§8.12). A revocation that would leave no trusted key is refused as a whole. What
  is honoured is persisted in `state.json` (§8.7) whether or not the manifest offers a newer version. Why: a leaked
  active key, with the release access the leak case assumes, could otherwise publish a manifest, newer or not, that
  revokes the backup and so removes the only recovery path of §8.12.

### 8.2 Manifest (`isshoni-desktop-update.json`)

```json
{
  "schema": 1,
  "product": "isshoni-desktop",
  "version": "0.2.0",
  "channel": "stable",
  "minVersion": "0.2.0",
  "publishedAt": "2026-12-01T10:00:00.000Z",
  "expiresAt": "2027-05-30T10:00:00.000Z",
  "notesUrl": "https://github.com/MoonWX/isshoni/releases/tag/v0.2.0",
  "artifacts": [
    {
      "os": "windows",
      "arch": "amd64",
      "kind": "nsis",
      "name": "isshoni-desktop_0.2.0_windows_amd64_setup.exe",
      "url": "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe",
      "size": 15204352,
      "sha256": "<64 lowercase hex>"
    }
  ],
  "revokedKeys": []
}
```

| Field | Rule |
|---|---|
| `schema` | `1`. Another value: the app ignores the manifest (`manifest_unsupported`) and never installs from it |
| `product` | `isshoni-desktop` |
| `version` | canonical SemVer without `v`, equal to the tag |
| `channel` | `stable` for `X.Y.Z`, `beta` for `X.Y.Z-rc.N`; `ci` for `X.Y.Z-ci.N` (written only for the updater e2e's test manifests; accepted only by `updatetest` builds, §8.11) |
| `minVersion` | the update floor from `deploy/desktop/min-version` (one SemVer line; starts at the first desktop release). An app below it shows the update as required (§8.5) |
| `publishedAt` | the tag commit's date, M1's wire time form (`api.WireTime`) |
| `expiresAt` | `publishedAt` + 180 days (D21) |
| `notesUrl` | the release page |
| `artifacts` | one entry per (os, arch); `kind` `nsis` in M2 (**later (M3)**: `darwin-app`, a `.tar.gz` of the signed `.app`); `url` must be exactly `https://github.com/MoonWX/isshoni/releases/download/v<version>/<name>`; `size` ≤ 256 MiB |
| `revokedKeys` | SHA256 fingerprints as `ssh-keygen -l` prints them (`SHA256:…`), from `deploy/keys/update_revoked` (empty normally); honoured only in a manifest signed by the backup key, never for the backup key itself (§8.1) |

Unknown fields are ignored (the bytes are signed; the schema number guards incompatible changes). The manifest is at
most 64 KiB. `tools/desktoppkg manifest` writes it in the unprivileged packaging job (§11.1) from the installer file,
the tag and the two committed files; the JSON is written with sorted keys and LF so it is reproducible.

### 8.3 Signature format (`isshoni-desktop-update.json.sig`)

The armored SSHSIG that `ssh-keygen -Y sign -f <key> -n isshoni-update isshoni-desktop-update.json` writes. Verifying
by hand:

```
ssh-keygen -Y verify -f update_signers -I isshoni-update -n isshoni-update \
  -s isshoni-desktop-update.json.sig < isshoni-desktop-update.json
```

`deploy/keys/update_signers`:

```
isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-1
isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-backup-1
```

The app's verifier (`internal/update/sshsig.go`, following OpenSSH's `PROTOCOL.sshsig`):
1. Strip the `-----BEGIN SSH SIGNATURE-----` / `-----END SSH SIGNATURE-----` armor, base64-decode (at most 8 KiB).
2. Parse: magic `SSHSIG`, `uint32` version = 1, `string` public key, `string` namespace, `string` reserved, `string`
   hash algorithm, `string` signature; no trailing bytes.
3. Require namespace = `isshoni-update`, hash algorithm = `sha512`, and a public key whose wire form equals one of the
   trusted keys (embedded, minus persisted revocations; a persisted entry naming an embedded backup key is ignored).
   The verifier returns the matched key and whether it is a backup key, which decides whether `revokedKeys` is
   honoured (§8.1).
4. Signed data = `SSHSIG` ‖ `string(namespace)` ‖ `string(reserved)` ‖ `string("sha512")` ‖ `string(SHA-512(manifest))`;
   verify with `ssh.ParsePublicKey(...).Verify(data, sig)` (`golang.org/x/crypto/ssh`), which for `ssh-ed25519` is
   plain Ed25519.

The embedded keys live in `internal/update/signers.go` between `// BEGIN UPDATE SIGNERS` and `// END UPDATE SIGNERS`,
written from `deploy/keys/update_signers` by `task release:update-keys` and checked equal by `repocheck keys`
(`task lint:keys`). Each entry is `{Name, Backup, Key}`; `Backup` is true exactly when the line's key comment matches
`^isshoni-update-backup-[0-9]+$`. `repocheck keys` requires exactly one backup line and at least one other line in
`update_signers`; `ISSHONI_UPDATE_TEST_SIGNERS` (§8.11) follows the same comment rule. An app built with an empty block reports `disabled` with `no_keys`; `release.yml` refuses to build
a release while the file is missing or empty (§11.1).

### 8.4 Where the app looks

| Channel | Requests (each with `User-Agent: isshoni-desktop/<version>`, no cookies, no `Referer`) |
|---|---|
| `stable` (default) | `GET https://github.com/MoonWX/isshoni/releases/latest/download/isshoni-desktop-update.json` and `….json.sig` |
| `beta` (opt-in in 03's settings) | `GET https://api.github.com/repos/MoonWX/isshoni/releases?per_page=10` (`Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`, ≤ 1 MiB), then the newest non-draft release with a SemVer tag that has both assets; its `browser_download_url`s must be the exact `…/releases/download/v<version>/…` URLs. A `beta` app accepts manifests of both channels; a `stable` app only `stable` |

Non-SemVer tags (`deps-ffmpeg-…`) are skipped. HTTP 403/429 from the API is `rate_limited` and waits for the next
scheduled check.

### 8.5 Check algorithm and states

```
          ┌────────── Enabled() false ──────────► off ◄──── (re-asked before every network request)
 idle ──► checking ──► verify sig ──► parse ──► version ≤ current ──────────► up_to_date
              │            │fail        │fail       │ newer, expired ───────► error(manifest_expired)
              │            │            │           │ newer, no artifact ───► error(no_artifact)
              │            │            │           │ newer, not running from InstallDir ──► error(not_installed){Available, NotesURL}
              │            ▼            ▼           │ newer, loop guard tripped for it ────► error(install_failed){Available}
              └► error(network|rate_limited) error(bad_signature|bad_manifest|manifest_unsupported|wrong_channel)
                                                    ▼ newer
                                              downloading ──► verify size+sha256 ──► ready{version, required}
                                                    │fail ──► error(download_failed|size_mismatch|hash_mismatch|disk)
 disabled: not a release build (Eligible false) or no embedded keys; never makes a request
```

- `Eligible(version)`: `version.Parse` (internal/version, 02 §13.2) succeeds and the prerelease part is empty or
  `rc.N`. Dev, CI, e2e and dry-run builds are not eligible (in `updatetest` builds only, `ci.N` is too, §8.11).
- **Every version comparison** uses `version.Compare` and `version.Parse` from `internal/version` (02 §13.2, moved
  from the hub so client and server agree): "version ≤ current", `required` = `Compare(current, minVersion) < 0`,
  rc < final, and Eligible's parse. A manifest `version` or `minVersion` that doesn't parse is `bad_manifest`.
- **Not an installed copy**: a newer, verified manifest seen by a copy whose exe is not in `InstallDir` (§8.7) gives
  `State=error`, `ErrorCode=not_installed`, `Available=<v>` and `NotesURL`; nothing is downloaded. 03 shows "isshoni
  <v> is available" with the release page link.
- **Loop guard tripped** (§8.7): after 2 failed attempts for a version, the state is `error` with `install_failed`
  and `Available=<v>` until a newer version appears (which starts a normal download). 03 shows "The update didn't
  install" with a link to `/download#windows-updates`.
- `required` = current < `minVersion`. 03 shows a required update as a banner that can't be dismissed; it never blocks
  using the app (the server's `minClientVersion` does that, M1 01 §6.1).
- A newer manifest that is expired is never installed; 03 says "Update info is out of date; get the new version from
  your server's Download page" (rare: it means no release for 180 days and an app behind). An expired manifest that is
  not newer is just `up_to_date`.
- The local clock decides expiry. A far-off clock can make every manifest look expired; the error text says to check
  the clock.

### 8.6 Schedule, limits, timeouts

| Item | Value |
|---|---|
| First check | 60–300 s after start (uniform random), not before 03 says the first-run wizard is done. Only this **scheduled** first check waits for `WizardDone`: `CheckNow`, a user action (02 §13.3's "Update now" can appear during wizard step 1 when a server's `minClientVersion` is above the app), never waits for it, but still honors `Enabled()` |
| Then | every 24 h ± 1 h while running |
| After an error | next check in 6 h ± 30 min |
| `CheckNow` | at most once per 30 s (a second call returns the running check's result) |
| Manifest / signature | 20 s each; ≤ 64 KiB / ≤ 8 KiB; `Content-Encoding` other than identity/gzip refused |
| Installer download | stall timeout 60 s without a byte, 60 min total; must equal `size` exactly; ≤ 256 MiB |
| Disk | free space ≥ `size` + 64 MiB in the updates folder before downloading |
| TLS | Go's defaults (TLS 1.2+), system roots |
| One download at a time | yes; a newer manifest during a download cancels it |

### 8.7 Download and apply on Windows

- Folder `%LOCALAPPDATA%\isshoni\updates\` (inherits the user-only ACL of `LOCALAPPDATA`). The download goes to
  `<name>.partial` (`os.CreateTemp`), is checked (size, SHA-256), then renamed to `<name>`.
- `state.json` (atomic write: temp file and rename), at most 16 KiB:

  ```json
  {"v": 1,
   "pending": {"version": "0.2.1", "file": "isshoni-desktop_0.2.1_windows_amd64_setup.exe", "size": 15204352,
               "sha256": "…", "expiresAt": "2027-06-01T10:00:00.000Z", "attempts": 0},
   "revokedKeys": [],
   "lastCheckAt": "2026-12-05T09:12:00.000Z"}
  ```

- **When it applies**: (a) when the user clicks "Restart and update" (03): 03 takes the verified installer from
  `Ready()` (non-nil only in `StateReady`) and calls `Apply`; and (b) **at the next start** of the app, before its
  window opens, when `pending` is newer than the running version, not expired and its file verifies
  (`PendingAtStartup`). (b) never happens at a start with `--autostart` (03 §16.1: no window until the user opens
  one; the installer's relaunch carries only `--updated-from`, so the main window would open at every sign-in after an
  update): the update stays `ready` and the tray offers "Update to X and restart". 03 never applies while a share is
  live; at start nothing is live yet. Rationale: friends update without having to click, at a moment when nothing
  runs (as Discord does), and version skew with the server stays short.
- **What the friend sees, and the lock**: from the click to the relaunched window the installer shows its
  "Updating isshoni to {v}…" window (§6.4 `/UPDATE`), and it holds `Local\io.isshoni.desktop.installing` for its whole
  run (§2.4, §6.7). A second click (the shortcut again, or "Open in the isshoni app" again after the browser's
  "Nothing happened?" line, 03 §12.3) starts an `isshoni.exe` that finds that mutex at 03 §13.4 step 2a: it writes
  its link, if any, to `PendingServer`/`PendingRoom` and exits 0 without calling `Apply`, so it neither starts a second
  installer nor takes the running mutex that the installer waits for. The relaunched app then opens that link. So a
  double-click during an update ends with one install, `attempts` incremented once, and the link handled.
- **Loop guard**: `attempts` is incremented before each launch; after 2 failed attempts for the same version (the app
  starts again still older), automatic applying stops for that version: the state is `error` with `install_failed`
  and `Available=<v>` until a newer version appears (§8.5), and 03 shows "The update didn't install" with a link to
  `/download#windows-updates`.
- **Only real installs update**: the running exe's folder must equal `HKCU\Software\isshoni\InstallDir`; otherwise (a
  build run from a folder) a newer verified manifest gives `error` / `not_installed` with `Available` and `NotesURL`
  and nothing is downloaded (§8.5), and `Apply` returns `ErrNotInstalled`.
- **Launch** (`Apply`): verify the file again (size and SHA-256 against `state.json`; a mismatch deletes it and
  returns `hash_mismatch`), return `ErrNotInstalled` outside `InstallDir`, then start it with `/S /UPDATE /RELAUNCH`
  (`exec.Cmd` with `SysProcAttr{HideWindow: true, CreationFlags: CREATE_NEW_PROCESS_GROUP}`), wait until
  `Local\io.isshoni.desktop.installing` exists (`OpenMutexW` every 50 ms, up to 10 s; on timeout log WARN and return
  nil anyway), and return; 03 then quits within 5 s (closing its connections normally). The wait hands the lock over
  without a gap: the app's running mutex is still held while the installer's installing mutex appears, so no start
  in between can become a first instance. The installer waits for the running mutex to go, installs, closes the
  installing mutex and starts `isshoni.exe --updated-from=<old>`.
- **A deep link at start**: if the app was started by an `isshoni://` link and applies a pending update, 03 first
  parses the link with 02's `deeplink.Parse` and writes `deeplink.Link.Origin` into `PendingServer` and `.RoomID` into
  `PendingRoom` (§2.3), so the relaunched app opens the link as if it had just arrived, under 02 §12.3's rules
  ("Add server {host}?" for an unknown server; never a share).
- **Cleanup**: at every start, files in `updates\` that are not the pending version's are deleted, and `pending` is
  cleared once the running version ≥ its version.

### 8.8 Turning it off

- 03's setting `updates.check` (default on, initial value from the installer's `UpdateChecks`, §2.3). Off means no
  request at all.
- Machine policy `HKLM\SOFTWARE\Policies\isshoni\DisableUpdateChecks` = 1 → `off` with `PolicyOff: true`; 03 shows
  "Turned off by your administrator" and greys out the setting.
- `Enabled()` is asked before every request, so turning the setting off mid-check stops the next request.

### 8.9 Go API

```go
package update

type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta" // stable releases and -rc.N releases
)

type State string

const (
	StateDisabled    State = "disabled"     // not a release build, or no embedded keys
	StateOff         State = "off"          // turned off by the user or by policy
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateUpToDate    State = "up_to_date"
	StateDownloading State = "downloading"
	StateReady       State = "ready"        // a verified installer waits
	StateError       State = "error"
)

type Status struct {
	State     State
	Current   string    // running version
	Available string    // newer version found, "" otherwise
	Required  bool      // Current < minVersion
	NotesURL  string
	Progress  float64   // 0..1 while downloading
	CheckedAt time.Time // zero before the first check
	ErrorCode string    // §8.10, "" unless StateError (or StateDisabled: "no_keys", "not_release")
	PolicyOff bool
}

type Options struct {
	CurrentVersion string            // internal/version.Version()
	Channel        func() Channel    // 03's setting, asked at each check
	Enabled        func() bool       // 03's setting AND !PolicyDisabled(); asked before every request
	WizardDone     func() bool       // only the scheduled first check waits for it; CheckNow never does
	DataDir        string            // %LOCALAPPDATA%\isshoni (03); the updater uses DataDir\updates
	HTTP           *http.Client      // nil: the updater's own client with the redirect rule of §8.1
	Now            func() time.Time  // nil: time.Now
	Rand           func() float64    // nil: math/rand/v2
	Logger         *slog.Logger
}

type Pending struct {
	Version  string
	Path     string
	Required bool
}

func New(o Options) (*Updater, error)
func (u *Updater) Run(ctx context.Context) error              // the schedule; returns when ctx ends
func (u *Updater) CheckNow(ctx context.Context) (Status, error) // user action: never waits for WizardDone; honors Enabled
func (u *Updater) Status() Status
func (u *Updater) Subscribe() (<-chan Status, func())         // latest-value channel for 03's binding; cancel func
// PendingAtStartup: nil, nil when nothing should be applied now, and always nil when autostart is true
// (a start with --autostart, 03 §16.1; the update stays ready).
func (u *Updater) PendingAtStartup(autostart bool) (*Pending, error)
func (u *Updater) Ready() *Pending                            // non-nil only in StateReady: the "Restart and update" source
// Apply re-verifies p's file (size, SHA-256), returns ErrNotInstalled when the exe is not in InstallDir, then starts
// the installer, waits (≤ 10 s) until the installer's mutex Local\io.isshoni.desktop.installing exists, and returns.
func (u *Updater) Apply(ctx context.Context, p *Pending, relaunch bool) error

func Eligible(version string) bool
func PolicyDisabled() bool // Windows: the HKLM policy value; other OS: false

var (
	ErrNotInstalled = errors.New("update: not running from the installed folder")
	ErrUnsupported  = errors.New("update: no apply method on this OS") // apply_other.go
)
```

- Files: `update.go` (states, schedule), `manifest.go`, `sshsig.go`, `fetch.go` (HTTP client, limits, redirects),
  `state.go` (`state.json`), `signers.go` (the embedded keys), `apply_windows.go`, `policy_windows.go`,
  `apply_other.go`; **later (M3)** `apply_darwin.go` (`.app` swap after `codesign -R` against the designated
  requirement, plan).
- Imports: stdlib, `golang.org/x/crypto/ssh`, `golang.org/x/sys/windows{,/registry}`, `internal/version` (also for
  every SemVer parse and comparison: `version.Parse`, `version.Compare`, 02 §13.2, so the updater and 02's
  `update_app` screen never disagree on which version is newer; no `golang.org/x/mod/semver`), `internal/logx`. Never
  `internal/server/...` (a depguard rule).
- Logging: component `update`; versions and error codes only, never full URLs with query strings (GitHub's signed asset
  URLs).

### 8.10 Error codes

`network`, `rate_limited`, `bad_signature`, `bad_manifest`, `manifest_unsupported`, `manifest_expired`,
`wrong_channel`, `no_artifact`, `download_failed`, `size_mismatch`, `hash_mismatch`, `disk`, `install_failed`,
`not_installed`; for `disabled`: `no_keys`, `not_release`. Stable, snake_case, never renamed (M1 README §4). 03 maps
them to `desktop.update.errors.<code>` in `web/src/i18n/lazy/desktop.en.json`. 03 adds these codes to
`internal/desktop/bridge/codes`, so `check:i18n` fails when a code has no text (03 §6.4 and §18.1 get the same row).

### 8.11 Test hooks (`updatetest` build tag)

Only in builds with `-tags updatetest` (CI test builds, never a release):
- `ISSHONI_UPDATE_TEST_BASE` replaces both the stable URL base and the beta API (an `http://127.0.0.1:<port>` origin
  is then allowed);
- `ISSHONI_UPDATE_TEST_SIGNERS` names an allowed-signers file used instead of the embedded keys;
- `Eligible` also accepts a `-ci.N` prerelease, and a manifest with channel `beta` (or `ci`) is accepted whatever
  the app's channel setting, so the CI builds `0.0.1-ci.N` of §16 check and update;
- 03's `--update-now` flag exists and skips the `WizardDone` wait.

03's DK12 tests (an update offered in a test channel) build with `-tags updatetest` (`internal/desktop/e2ehost`, and
the CDP build if it is used) and set `ISSHONI_UPDATE_TEST_BASE` and `ISSHONI_UPDATE_TEST_SIGNERS`. Release builds keep
the strict rule (`Eligible`: empty or `rc.N` only); the check of §5.2 step 3 fails if either variable name is in a
shipped binary or the build tags are not exactly `production`.

### 8.12 Key rotation and a leak

- **Rotation**: generate `isshoni-update-2`; add it to `update_signers` (keep `-1`); release; after one release cycle
  move the secret to `-2` and drop `-1` from the file in the next release. Apps that skipped versions still trust the
  backup key.
- **Rotation never uses `revokedKeys`**: dropping `-1` from the file of a later release is enough for apps that
  update, and an active-key manifest's list would be ignored anyway (§8.1).
- **Leak of the active key**: sign with the backup key a release whose manifest lists the leaked key in
  `revokedKeys` and whose app embeds the backup and a new key; publish a security advisory. For that one release the
  owner puts a passphrase-free copy of the backup's private key (`ssh-keygen -p` on a temporary copy, deleted after)
  into `RELEASE_UPDATE_SIGNING_KEY` (replacing the leaked key) and, once the release is
  published, puts the new active key there; the release after it embeds a new backup (`isshoni-update-backup-2`),
  since the old one has been in CI. The `sign` job refuses a manifest whose `revokedKeys` is non-empty unless the key
  it holds is the backup line of `update_signers` (same fingerprint), and `check-draft.sh` checks the same before
  `publish` (§11.1, §11.3), because apps ignore revocations signed by any other key. Apps that check before an
  attacker reaches them stop trusting the leaked key for good. An attacker holding the leaked key can't revoke the
  backup (§8.1), so this path stays open for every app. Apps that never check again can't be protected by any
  offline scheme; that limit is written on `/security`.
- **Leak or loss of the backup key**: it can't be revoked through a manifest; a normal release (signed with the
  active key) embeds a new backup line and drops the old one, and apps that update stop trusting the old backup. A
  leaked backup is the worst case (it can sign updates and revoke the active key in the apps that see its manifest,
  which then need a manual reinstall from the `/download` page), which is why it stays offline with a passphrase and
  is used only for the leak release above.
- The same steps are the owner's checklist in `deploy/keys/README.md`.

## 9. Licenses

### 9.1 The LGPL compliance bundle

For every release:
- `avcodec-<N>.dll` and `avutil-<N>.dll` exactly as the deps release built them from unmodified FFmpeg source
  (SHA-256 checked against `lock.json` in every job), dynamically linked, never signed by the project (plan).
- `licenses\ffmpeg\COPYING.LGPLv2.1.txt`, `LICENSE.md`, and `README-LGPL.txt` in the install tree, saying: FFmpeg
  `<ver>` (libavcodec, libavutil) under LGPL-2.1-or-later; the configure line (from `ffmpeg-config.json`); that the
  complete corresponding source with the build script is the release asset `isshoni-desktop_<v>_ffmpeg-source.tar`
  (and the deps release, by URL); and that the user may replace both DLLs with "an interface-compatible build with the
  same major versions (`avcodec-<C>.dll`, `avutil-<U>.dll`, same file names, in the install folder) and at least the
  shipped minor versions; isshoni checks `avcodec_version()`/`avutil_version()` at load" (D22, 04 §9.2; the README
  names the shipped `<C>.<minor>` and `<U>.<minor>`; an update installs the shipped ones again).
- The source archive is published in the same place as the binaries (the GitHub Release that the installer, the ps1
  and the updater download from), which is how LGPL-2.1 §6 lets the source accompany the object code.
- **Relink check** (manual, once per FFmpeg pin change): rebuild the DLLs from the release's source archive with its
  `build.sh`, put them into `$INSTDIR`, and confirm the app's self-check and a share still work.

### 9.2 `THIRD_PARTY_NOTICES` for the desktop

`tools/notices` gains two inputs; the desktop file is generated in the packaging job:

```
go -C tools run ./notices -root .. -pkg ./cmd/isshoni-desktop -goos windows \
  -npm ../web/dist/licenses.txt -extra ../deploy/notices/extra.txt \
  -native ../deploy/notices/native-windows.json -native-root <windows-native artifact dir> \
  -version <v> -o ../dist-desktop/stage/licenses/THIRD_PARTY_NOTICES.txt
```

Sections: Go modules linked into `isshoni.exe` for windows (plus the Go runtime), "Web client (bundled JavaScript)",
"Native components" (new), "Adapted source code and data" (`extra.txt`). Deterministic, as in M1 06 §8.4. The server
archives keep M1's file unchanged. **Later (M3)**: `-goos darwin` with `native-darwin.json`.

`deploy/notices/native-windows.json`:

```json
[
  {"name": "FFmpeg (libavcodec, libavutil)", "version": "9.0.2", "license": "LGPL-2.1-or-later",
   "linking": "dynamic", "files": ["avcodec-*.dll", "avutil-*.dll"],
   "source": "https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz",
   "licenseFiles": ["ffmpeg/COPYING.LGPLv2.1", "ffmpeg/LICENSE.md"]},
  {"name": "Intel libvpl (dispatcher)", "version": "2.17.0", "license": "MIT",
   "linking": "static, inside avcodec", "licenseFiles": ["libvpl/LICENSE"]},
  {"name": "nv-codec-headers", "version": "12.1.14.1", "license": "MIT",
   "linking": "headers, inside avcodec", "licenseFiles": ["nv-codec-headers/LICENSE"]},
  {"name": "AMD AMF headers", "version": "1.5.3", "license": "MIT",
   "linking": "headers, inside avcodec", "licenseFiles": ["amf/LICENSE.txt"]},
  {"name": "Opus", "vcpkgPort": "opus", "license": "BSD-3-Clause",
   "linking": "static, inside isshoni-media.dll", "licenseFiles": ["vcpkg/opus/copyright"]},
  {"name": "OpenH264 headers", "version": "2.6.0", "license": "BSD-2-Clause",
   "linking": "headers, inside isshoni-media.dll", "licenseFiles": ["openh264/LICENSE"]},
  {"name": "C++/WinRT", "license": "MIT",
   "linking": "headers, inside isshoni-media.dll", "licenseFiles": ["cppwinrt/LICENSE"]},
  {"name": "Microsoft Edge WebView2 Runtime bootstrapper", "license": "LicenseRef-Microsoft-WebView2-Distribution",
   "linking": "separate program inside the installer, run only when the runtime is missing",
   "licenseFiles": ["../deploy/notices/licenses/webview2-bootstrapper.txt"]}
]
```

Whoever adds a native dependency (04, 05) adds its entry in the same pull request. The list matches 04 §13.5's table
(GoogleTest is tests-only and never listed; the Cisco OpenH264 binary is never shipped, §9.5). `-native-root` is the
`windows-native` artifact's `release\` folder, whose `licenses/` holds every `licenseFiles` path above (§5.1).

### 9.3 The native gate (`tools/notices`, run in the packaging job; fails the build)

- Each entry's `license` is on M1's shipped allowlist (M1 06 §8.3), **or** it is `LGPL-2.1-or-later` with
  `linking: "dynamic"`, the FFmpeg entry, and the compliance files of §9.1 present in the stage, **or** it is a
  `LicenseRef-*` listed with a reason in `deploy/notices/exceptions.md`.
- The vcpkg ports with a `share/<port>/copyright` in the artifact equal the set of `vcpkgPort` values: a port that is
  shipped but not listed (or listed but gone) fails.
- `ffmpeg-config.json` passes §4.4's LGPL checks again.
- `tools/desktoppkg check`: every binary in the stage is ours (`isshoni.exe`, `isshoni-media.dll`, `uninstall.exe`) or
  matches a `files` glob of an entry.

### 9.4 Go and npm

- `task licenses` already runs go-licenses over `./cmd/...` for linux, darwin and windows (M1 06 §8.3); with D18 the
  windows run covers `cmd/isshoni-desktop` and its Wails modules. The darwin run on Linux sees the stub (no cgo), so
  the `desktop-macos` job runs `go-licenses check ./cmd/isshoni-desktop` natively with cgo, so M3 finds no surprise.
- The SPA bundled in the app is the server's `web/dist`, already under the npm gate.
- `govulncheck` covers `cmd/isshoni-desktop` through `./...`.

### 9.5 Never shipped

OpenH264 (downloaded at run time by 04 after Cisco's notice, its own license shown there), x264 (dev builds only),
test frameworks and tools (MSVC, NSIS, MSYS2, Pester, 7-Zip, dumpbin): tools, not dependencies (M1 06 §8.3).

## 10. CI

### 10.1 `ci.yml` additions

New `changes` filters:
- `desktop`: `cmd/isshoni-desktop/**`, `internal/client/**`, `internal/media/**`, `internal/update/**`,
  `internal/desktop/**`, `native/**`, `deploy/windows/**`, `deploy/install-desktop.ps1`, `deploy/embed.go`,
  `deploy/notices/native-windows.json`, `tools/desktoppkg/**`, `tools/wails3/**`, `tools/notices/**`, `go.mod`,
  `go.sum`, `web/**` (the SPA is bundled), `.github/workflows/ci.yml`.
- `native`: `native/**`, `.github/workflows/ci.yml`, `Taskfile.yml` (`native-host` runs `task native:test` and
  `task lint:native`, so the Taskfile is one of its inputs, as M1 counts each job's own inputs).
- `update`: `internal/update/**`, `deploy/windows/nsis/**`, `tools/desktoppkg/**` (runs the updater e2e).
- `ps`: `deploy/install-desktop.ps1`, `deploy/test/install-desktop.Tests.ps1`, `deploy/test/windows/*.ps1`,
  `native/windows/tools/*.ps1`, `deploy/windows/msvc-env.ps1`, `deploy/windows/tools.json`.
- `pins`: `web/package.json`, `web/package-lock.json`: they run `lint-deploy`, so a pull request that changes only
  `@wailsio/runtime` still runs `lint:pins` (§3), which neither the `go` nor the `deploy` filter would trigger.
- M1's `run-e2e` decision also counts `internal/desktop/**` (the desktop e2e specs).
- M1's `protocol` filter also counts `internal/desktop/bridge/**`, `cmd/isshoni-desktop/**`, `tools/wails3/**` and
  `web/src/platform/desktop/**`.

| Job | Runs when | Runner | Steps and assertions | Timeout | Gates |
|---|---|---|---|---|---|
| `lint-go` (changed) | go | ubuntu-24.04 | M1's steps plus `GOOS=windows golangci-lint run ./...` (Windows-only files: the x/sys/windows loader in `internal/media/native` (04 §12), `apply_windows.go`, 03's `winos`), `go vet` with `GOOS=darwin` and `GOOS=windows`, and `task lint:go:cross`: `GOOS=windows CGO_ENABLED=0 go build ./internal/... ./cmd/...` (the Windows app needs no cgo, so the cross-compile stays possible) and the gomobile check, package builds of `./internal/client/...` and `./internal/media` for `android` and `ios` on arm64 (01 §4); for the `tools/` module also `GOOS=windows go vet ./...` (g2g's GDI capture is Windows-only) | 10 | yes |
| `test-go` (unchanged) | go | ubuntu-24.04 | `go test -race ./...`: the client core, `internal/update` and the app's non-Wails packages with the fake engine; the app main is the stub | 20 | yes |
| `protocol` (changed) | protocol | ubuntu-24.04 | M1's `task gen:check`, now also covering `web/src/platform/desktop/codes.gen.ts` (03's `tygo.yaml` entry for `internal/desktop/bridge/codes`); then builds `.bin/wails3` (`task release:wails3`) and runs `GOOS=windows .bin/wails3 generate bindings` into `web/src/platform/desktop/bindings/`, failing on any diff (with GOOS unset the generator sees only the D18 stub on linux, so no services) | 10 | yes |
| `build` (changed) | as M1 | ubuntu-24.04 | M1's steps plus `go build -o bin/isshoni-desktop-e2ehost ./internal/desktop/e2ehost`, uploaded with M1's binaries for `e2e` (like `isshoni-loadtest`) | 10 | yes |
| `e2e` (changed) | run-e2e | ubuntu-24.04 | M1's steps with `DESKTOP_E2E_BIN` = the downloaded `bin/isshoni-desktop-e2ehost` (`chmod +x`), so 03's desktop specs (03 §22.4 A) run in the same `npm run e2e` | as M1 | yes |
| `desktop-macos` | desktop | macos-15 | `go test -race` of `./internal/client/... ./internal/media/... ./internal/update/...` and the app's packages; `go build ./cmd/isshoni-desktop` (darwin, cgo, fake engine until M3); `go-licenses check ./cmd/isshoni-desktop` | 20 | yes |
| `native-host` | native | ubuntu-24.04, macos-15 (matrix) | `task native:test PRESET=host-debug` and `PRESET=host-tsan` with `VCPKG_DIR=$RUNNER_TEMP/vcpkg` (the task clones vcpkg at `native/vcpkg.json`'s baseline, bootstraps it with metrics off, then runs `cmake --preset`, `cmake --build --preset` and `ctest --preset`; 04 §18.1: `native/common` tests, the libopus host build of 04 E3/E4), with vcpkg's `files` binary cache keyed by OS, image and manifest (no restore keys); on ubuntu `vm.mmap_rnd_bits=28` before `host-tsan` (TSan's fixed shadow layout fails with Ubuntu 24.04's 32-bit mmap randomization), and `task lint:native CLANG_FORMAT=clang-format-18` (`clang-format --dry-run --Werror` over the tracked `native/**/*.{h,c,cpp}` with `native/.clang-format`). The clang-format version is the ubuntu image's `clang-format-18` (installed with apt if the image lacks it), not pinned in a repository file | 20 | yes |
| `windows-go` | desktop | windows-2025 | no `needs`, no DLL: `go test -race ./internal/client/... ./internal/update/... ./internal/media/...` with the image's MinGW gcc (as M1's nightly), the fake engine (04's DLL tests skip without `ISSHONI_TEST_ENGINE_DLL`), then `CGO_ENABLED=0 go test ./internal/client/... ./internal/update/...` (the shipped build mode; this is the Windows job 02 §16.1 expects, on `windows-2025` rather than `windows-latest`, which moves). Runs 02's credstore and deviceauth tests (DA9, DA10) and this document's R7/R8 on Windows from R1 on | 15 | yes |
| `windows-native` | desktop | windows-2025 | §5.1 with the vcpkg cache: FFmpeg from the lock (and its staleness check, §4.1), the MSVC environment, `win-x64-ci` build and `ctest` (outbound network for 04's `openh264_test`), `win-x64-release` build, the DLL checks on the release output; uploads `windows-native` (`release\` and `ci\`, kept 1 day) and `isshoni-native-tools` (§17 `native:dist`, kept 7 days) | 30 | yes |
| `windows-desktop` | desktop | windows-2025 | needs `windows-native` and `web`: only the DLL-dependent Go tests, `go test -race ./internal/media/...` with `ISSHONI_TEST_ENGINE_DLL=<artifact>\ci\isshoni-media.dll` (04 §18.2, `TestNativeIntoPublisher`; everything else is in `windows-go`); `task desktop:exe:windows`; NSIS (pinned zip); desktop notices and the native gate (§9.3); `stage -release`, prebuilt uninstaller, installer; `deploy/test/windows/smoke.ps1` (§16); Pester under Windows PowerShell 5.1 (`ps` filter); the updater e2e (`update` filter, §8.11, §16); the server-rendered ps1 test (§16); then, before the CDP spec, a step that reads the WebView2 `pv` from §6.5's keys and, when it is missing or below 120.0, runs the checked `MicrosoftEdgeWebview2Setup.exe /silent /install` from the `windows-native` artifact (so the CDP spec doesn't depend on `smoke.ps1`'s install having run first); **03's CDP spec** (03 §22.4 B): `task desktop:exe:windows TAGS=e2e`, `go build ./cmd/isshoni`, `npm ci` in `web/`, then the CDP Playwright config (`connectOverCDP`, no browser download); uploads `isshoni-desktop-windows-pr` (installer and symbols, kept 7 days, for manual tests on the owner's PC) | 50 | yes |
| `lint-deploy` (changed) | deploy, readme, repocheck, ps, pins | ubuntu-24.04 | M1's tasks plus `lint:ps` (PSScriptAnalyzer at Warning and Error and the ASCII check over `deploy/install-desktop.ps1`, `deploy/test/windows/*.ps1`, `deploy/windows/msvc-env.ps1` and `native/windows/tools/*.ps1`; the PowerShell 5.1 grammar check is in `windows-desktop`) and `test:ps` (Pester under `pwsh`); `lint:keys` now also checks the ps1 key block, `internal/update/signers.go` and the update keys' README rows; `lint:pins` (§3) | 10 | yes |
| `goreleaser-check` (changed) | as M1 | ubuntu-24.04 | `task release:snapshot` creates stub desktop files (`ISSHONI-DESKTOP-STUB`) when `dist-desktop/` has none, so the goreleaser config's desktop `extra_files` are checked by name; `release:assets` checks the names; `FULL=true` refuses stubs | 20 | yes |
| `ci-ok` | always | | needs the new jobs too (`native-host`, `windows-go`, `windows-native`, `windows-desktop`, `desktop-macos`) | 2 | the only required check |

Windows jobs: every third-party action pinned by SHA (M1 06 §8.1), `persist-credentials: false`, `contents: read`,
`shell: pwsh` by default and `powershell` (5.1) where 5.1 behaviour is the point.

### 10.2 Caches

CI only (release.yml and the nightly use none, M1 06 §9.3): Go modules and build cache per job (M1's), the vcpkg
binary cache (§5.1), the pinned NSIS zip keyed by its SHA-256, `.bin/wails3` keyed by `tools/wails3/go.{mod,sum}`.
The FFmpeg zip is not cached: it is a 1-second download from the deps release, verified each time.

### 10.3 `nightly.yml` additions

| Job | What |
|---|---|
| `go-cross-os` (changed) | the `windows-2025` leg also runs the desktop packages; the `macos-15` leg builds `cmd/isshoni-desktop` |
| `windows-full` | `windows-native` + `windows-desktop` without caches, the updater e2e always |
| `native-fuzz` | ubuntu-24.04: preset `host-fuzz` (04 §18.1), each fuzz target (`annexb_fuzz`, `sps_fuzz`, …) for 60 s with the corpus `native/common/tests/fuzz/corpus/`; a crash uploads the input as an artifact; 20 min |
| `windows-asan` | windows-2025: the MSVC environment and vcpkg of §5.1 (no cache), `cmake --preset win-x64-asan`, build, `ctest --preset win-x64-asan --output-on-failure`; 45 min |
| `goreleaser-full` (changed) | needs `windows-full`'s artifacts, so the snapshot carries real desktop files and `release:assets FULL=true DESKTOP=true` runs (the cosign bundle stand-in stays, M1 06 §8.5) |
| `compat-servers` | the client core against the last two final server releases (§11.6): downloads each `isshoni_<v>_linux_amd64.tar.gz`, `checksums.txt` and `.sig`, verifies with `deploy/keys/allowed_signers`, starts each with `deploy/dev/isshoni.e2e.toml` on free ports and a temp data dir, creates the admin through `setup-url --json` and `POST /api/v1/auth/setup/complete` (as M1's distro tests do), then writes `ISSHONI_COMPAT_ADMIN_FILE` (below) and runs `go test ./internal/client/... -run '^TestCompatServer' -count=1` with `ISSHONI_COMPAT_SERVER_URL` and `ISSHONI_COMPAT_ADMIN_FILE`, so 02's `TestCompatServerDeviceFlow` (link and refresh against N−1, 02 §13.4) runs next to 01's `TestCompatServer` (01 §14: join, publish with the fake engine, receive). Against a server whose `/api/v1/info` lacks `deviceFlow` (every 0.1.x server), the tests assert only the `compat.FromInfo` verdict `update_server`/`no_device_flow` (02 §13.4), never join or publish: no M2 app can link to or use such a server. Skips with a note while fewer releases exist |

`ISSHONI_COMPAT_ADMIN_FILE` is a JSON file, mode 0600, written by the job after `setup/complete`:

```json
{"username": "<admin>", "password": "<random>"}
```

The tests log in with it through `POST /api/v1/auth/login` to get the cookie session that the `UserSession` routes
`/device/lookup` and `/device/decide` need (02 §6.4) to approve the device code they start. The server is
`ISSHONI_COMPAT_SERVER_URL` (`http://127.0.0.1:<port>`), its only source (README §3 item 24).

`ffmpeg.yml`'s weekly run is the reproducibility check (§4.6).

### 10.4 `audiotest.yml` (the exit criterion's audio half)

```yaml
on:
  workflow_dispatch:
    inputs:
      os:    {type: choice, options: [win11, win10], required: true}
      suite: {type: choice, options: [release, full, hwcheck], default: release}
      release_run_id: {type: string, required: false}   # suite release: the release.yml run whose isshoni-exit-tools runs
permissions: {contents: read}
jobs:
  build:   # GitHub-hosted windows-2025, skipped when release_run_id is set: the DLL (as windows-native),
           # isshoni-audiotest.exe, and the isshoni-native-tools zip (§17 native:dist) for the hwcheck suite
  fetch:   # GitHub-hosted ubuntu-24.04, permissions {actions: read}, only when release_run_id is set: downloads that
           # release.yml run's isshoni-exit-tools (download-artifact with run-id), checks the DLL against
           # native-manifest.json, re-uploads it as this run's artifact
  run:     # needs: [build, fetch] (one of them skipped, hence the result test)
    if: >-
      !cancelled() && github.repository == 'MoonWX/isshoni' && github.actor == github.repository_owner &&
      (needs.build.result == 'success' || needs.fetch.result == 'success')
    runs-on: [self-hosted, windows, x64, isshoni-audio, "${{ inputs.os }}"]
    environment: audiotest   # required reviewer: the owner, so every run waits for the owner's approval (05 §18.8)
    timeout-minutes: 30
```

- **Which build runs**: with `release_run_id` (required for suite `release`), `fetch` brings that `release.yml` run's
  `isshoni-exit-tools` artifact (§11.1) into this run, failing unless `isshoni-media.dll`'s SHA-256 equals its
  `native-manifest.json`; that is the only form that counts for the exit gate and the release tiers (05 §18.10).
  Without it (suites `full` and `hwcheck` on any ref), `build` rebuilds from the dispatched ref with CI's stamping;
  such a report is diagnostic and never gates a release. The token that reads another run stays on GitHub's runner.
- `run` downloads this run's artifact (no checkout, no secrets, no token use), runs `isshoni-audiotest.exe probe`, then
  the suite through 05's CLI (05 §18.2, the interface; `-out` names the report's folder, README §3 item 25): suite
  `release` → `isshoni-audiotest.exe run -gate -out .`
  (on the `isshoni-exit-tools` of the run named by `release_run_id`), suite `full` → `isshoni-audiotest.exe run -gate
  -long -out .`; exit codes are 05's (0 every required case PASS, 1 a FAIL, 2 INCONCLUSIVE, 3 a required case
  SKIPPED, 4 setup error), and any non-zero code fails the job. It uploads the report and writes a summary table
  (scenario, verdict, excluded-tone level). An inconclusive result fails the job with "re-run with `-case <id>`".
- **`suite: hwcheck`** (04 §14, §18.4): `run` unpacks `isshoni-native-tools` and executes `hwcheck.ps1` (probe,
  `capture` per available encoder, `bench`, `sessions -count 5`, `protect-check`) under the same guard and labels,
  with no checkout and no token, and uploads its result bundle (which carries no machine names or paths, 04 §14).
  M-LATENCY stays manual (it needs Chrome and the server, §11.8 item 6).
- Never `pull_request`, `pull_request_target` or `workflow_run` triggers; the guard condition is defence in depth,
  since a fork can't add a trigger to this file without a merge.

### 10.5 The self-hosted runner (on the owner's PC; written up in `deploy/windows/runner/README.md`)

- One registration per installed OS (the PC dual-boots Win11 and Win10 22H2): Settings → Actions → Runners → New
  self-hosted runner → Windows x64, into `C:\actions-runner`, labels `isshoni-audio` and `win11` (or `win10`), as an
  **ephemeral** runner (`config.cmd --ephemeral`, 05 §18.8: it takes one job and then deregisters, so each test
  session registers it again). Names are generic labels, never the machine's host name.
- Runs as a **dedicated non-admin local account** (suggested name `isshoni-ci`) with no access to the owner's files or
  credentials, and **not as a service**: the owner signs in to that account (local or over RDP with "Remote Audio";
  spike S1's second run used RDP audio) and starts `run.cmd` for a test session, then stops it. WASAPI process
  loopback needs an interactive session with an audio endpoint.
- The account's audio setup follows spike S1: Windows volume ≥ 50 %, Communications → "Do nothing", no other app
  playing.
- Repository settings: Actions → General → "Require approval for all external contributors" (stricter than M1's
  first-time-only default), and the environment `audiotest` with the owner as required reviewer (§10.4's `run` job
  waits for that approval). The runner being offline except during a session is the real protection: a public
  repository's pull request can bring its own workflow file.
- The manual Discord check (real Discord desktop in a call, YouTube in a browser, `isshoni-audiotest record`) runs in
  the same session and its one-line result goes into the release-prep PR (§11.8).

### 10.6 Dependabot

`github-actions` picks up the new workflows. Wails is ignored on both ecosystems (D19):
`ignore: [{dependency-name: "github.com/wailsapp/wails/v3"}]` on the `gomod` entry for `/` and
`ignore: [{dependency-name: "@wailsio/runtime"}]` on the npm entry for `/web` (its `npm-minor-patch` group's `"*"`
pattern would otherwise propose runtime bumps alone). A Wails bump is a hand-made PR with the checklist of §3 (the
Windows smoke, and 03's origin test against `protocol.DesktopOrigins()` with the previous release's list). vcpkg's
baseline (`native/vcpkg.json` and `native/vcpkg-configuration.json`), NSIS, the FFmpeg pins and the PowerShell modules
are bumped by hand with M1's monthly tool PR.

## 11. Release

### 11.1 `release.yml` extension

```
tag v* ─► version ─┬─► web ────────────────────────────┬─────────────────────► build ─► sign ─► verify ─────────┬─► publish ─► site
                   ├─► desktop-exe (ubuntu) ─────────┐  │                         ▲               └► verify-windows ┘
                   └─► windows-native (windows) ─────┴──┴─► desktop-pkg (windows) ┘
 dry run: the same jobs with sign-dry instead of sign; nothing is published
 later (after SignPath approval): desktop-pkg is split around two SignPath requests (§12.4)
```

| Job | Runner, permissions | Steps | Timeout |
|---|---|---|---|
| `windows-native` (the same name as ci.yml's job) | windows-2025, `contents: read`, `attestations: read` (`GH_TOKEN` only on the attestation step) | FFmpeg from the lock with `gh attestation verify --signer-workflow …/ffmpeg.yml`; §5.1 without caches, building **`win-x64-release` only** (no `ci\` folder, no tests, `IM_COMMIT` = the tag's short SHA); the WebView2 bootstrapper check; uploads `windows-native` (`release\`) | 45 |
| `desktop-exe` | ubuntu-24.04, `contents: read`, no Node | downloads `web-dist`; `task release:wails3`; `task desktop:exe:windows VERSION=<v>` and its checks (§5.2); uploads `windows-exe`; also cross-builds the exit tools with `GOOS=windows CGO_ENABLED=0 -trimpath` and the tag's version stamp (`isshoni-audiotest.exe`, `isshoni-enginetest.exe`, and `g2g.exe` with `go -C tools build ./g2g`) and uploads them as `windows-exit-bin` | 15 |
| `desktop-pkg` | windows-2025, `contents: read` | downloads `windows-native` and `windows-exe`; pinned NSIS; desktop notices and the native gate; `stage -release` (§6.2, with the `--self-check` `testHooks: false` gate); prebuilt uninstaller; installer; `smoke.ps1` (install, `--version`, `--self-check` with `ffmpegLoaded` and `testHooks: false`, uninstall `/S /PURGE`); `tools/desktoppkg manifest`; symbols zip; copies the locked source tar as `isshoni-desktop_<v>_ffmpeg-source.tar` (SHA-256 checked); the stamped `install-desktop.ps1`; uploads `desktop-release` (the §2.1 files) and `desktop-stage`; uploads the workflow artifact **`isshoni-exit-tools`** (kept 90 days; not a release asset, so 04 §17's tools question is unaffected): `windows-exit-bin`'s three exes, `native/windows/tools/hwcheck.ps1`, and `isshoni-media.dll`, the FFmpeg DLLs and `native-manifest.json` copied byte for byte from `windows-native`'s `release\` (a step fails unless the DLL's SHA-256 equals the manifest's), plus a `README.txt` naming the run and the tag | 30 |
| `build` (changed) | as M1 | `needs: [version, web, desktop-pkg]`; downloads `desktop-release` into `dist-desktop/` and `desktop-stage` into `dist-desktop/stage/` as data (nothing from them runs); new step before goreleaser: fails while `deploy/keys/update_signers` is missing or empty (not in a dry run), then `task lint:keys`; syft over `dist-desktop/stage` → `isshoni-desktop_<v>_windows_amd64.sbom.json`; goreleaser (§11.2); `task release:assets VERSION=<v> FULL=true DESKTOP=true`; the attestation step adds `dist-desktop/isshoni-desktop*` and `dist-desktop/install-desktop.ps1` | 45 |
| `sign` (changed) | as M1 | after the checksums signature: fails without the secret `RELEASE_UPDATE_SIGNING_KEY`; removes an earlier `isshoni-desktop-update.json.sig`; writes the key under `umask 077`; when the manifest's `revokedKeys` is non-empty, fails unless the key's fingerprint (`ssh-keygen -l`) equals the backup line's of `deploy/keys/update_signers` (apps honour revocations only from the backup key, §8.1, §8.12); `ssh-keygen -Y sign -f "$RUNNER_TEMP/u" -n isshoni-update rel/isshoni-desktop-update.json`; prints the fingerprint; `ssh-keygen -Y verify -f deploy/keys/update_signers -I isshoni-update -n isshoni-update -s rel/isshoni-desktop-update.json.sig < rel/isshoni-desktop-update.json`; `gh release upload … --clobber`; the `release-signed` artifact includes the new `.sig`. The manifest's SHA-256 was already checked by step 2's `sha256sum --strict -c`, before any signature | 10 |
| `sign-dry` (changed) | as M1 | also makes an ephemeral update key, signs the manifest with it, and adds its `update_signers` to `release-keys-dryrun` | 10 |
| `verify` (unchanged) | | the Linux install in two systemd containers | 20 |
| `verify-windows` (new) | windows-2025, `contents: read`, `attestations: read`; **no write token** | §11.4 | 25 |
| `publish` (changed) | as M1 | `needs: [version, verify, verify-windows]` with `if: ${{ !cancelled() && needs.verify.result == 'success' && needs.verify-windows.result == 'success' && needs.version.outputs.dry-run == 'false' }}`; runs `sh deploy/release/check-draft.sh rel deploy/keys/allowed_signers deploy/keys/update_signers` (§11.3) | 10 |

Rules kept from M1 06 §9.3: no caches; actions pinned by SHA; checkout never persists credentials; the jobs that run
third-party build code hold no write permission; one run per tag. A Windows-runner outage blocks a release (D14);
the fix is to re-run, not to skip the desktop.

### 11.2 `.goreleaser.yaml` changes

```yaml
checksum:
  extra_files:                         # M1's three, plus:
    - glob: dist-desktop/isshoni-desktop*        # installer, symbols, SBOM, FFmpeg source, update manifest
    - glob: dist-desktop/install-desktop.ps1
release:
  extra_files:                         # the same two globs added
  header: |                            # §11.7 adds the Windows lines
```

goreleaser fails on an `extra_files` glob that matches nothing; that is why snapshots without a Windows build get stub
files (§10.1) and `release:assets FULL=true` refuses them. `task release:prepare` also stamps `@VERSION@` in a copy of
`deploy/install-desktop.ps1` into `dist-desktop/` (the packaging job does the same for its artifact; `release:assets`
compares the two).

`task release:assets … DESKTOP=true` additionally checks: every §2.1 name; the manifest's `version`, `channel`,
`artifacts[0].{name,url,size,sha256}` against the installer file and the tag; `minVersion` equals
`deploy/desktop/min-version`; the source tar's SHA-256 equals `lock.json`'s; `install-desktop.ps1` is stamped with
`<v>` and has the production key block; the installer's file list (`7z l`, which reads NSIS archives) equals the
stage's list.

### 11.3 `check-draft.sh` and its test

`deploy/release/check-draft.sh DIR ALLOWED_SIGNERS [UPDATE_SIGNERS]` (M1 06 §9.3, README F15):
- the asset-list exceptions are `checksums.txt`, `checksums.txt.sig`, `checksums.txt.sigstore.json` and
  `isshoni-desktop-update.json.sig`;
- when `DIR` holds `isshoni-desktop-update.json`, `UPDATE_SIGNERS` is required (exit 2 without it) and, after M1's
  three checks, `ssh-keygen -Y verify -f UPDATE_SIGNERS -I isshoni-update -n isshoni-update -s
  DIR/isshoni-desktop-update.json.sig < DIR/isshoni-desktop-update.json` must pass. Then, unless the manifest holds
  the exact line `  "revokedKeys": [],` or `  "revokedKeys": []` (what `tools/desktoppkg manifest` writes for an empty
  list; any other spelling counts as non-empty, so the check fails closed), the same verify must also pass with a
  signers file that holds only `UPDATE_SIGNERS`' backup line (comment `isshoni-update-backup-<n>`): apps honour
  revocations only from the backup key (§8.1), so a release must not publish a revocation that every app would
  ignore. No JSON parser is needed; the script keeps needing only `sha256sum` and `ssh-keygen`.
- `sign`'s and `sign-dry`'s inline asset check gets the same exception.
- `deploy/test/check-draft-test.sh` gains: passing with a signed manifest; failing with a missing manifest
  signature, a manifest signed by the checksums key, the update key's signature in the namespace
  `isshoni-checksums`, a changed manifest re-listed in a re-signed `checksums.txt` but with the old manifest
  signature, an `UPDATE_SIGNERS` for another principal, and an extra `.sig` of another name (must fail the asset list);
  and for revocations: a manifest with a non-empty `revokedKeys` signed by the active key fails, the same manifest
  signed by the backup key passes.

### 11.4 `verify-windows`

1. Downloads `release-signed` (a dry run: sign-dry's, and `release-keys-dryrun`).
2. Prints `C:\Windows\System32\OpenSSH\ssh-keygen.exe`'s version (`ssh -V`) and verifies `checksums.txt.sig` and the
   manifest signature with it, using the repository's keys (a dry run: the dry-run keys).
3. `gh attestation verify isshoni-desktop_<v>_windows_amd64_setup.exe --repo MoonWX/isshoni` (`GH_TOKEN` on this step
   only).
4. Builds `tools/relserve`, imports its test CA into `Cert:\LocalMachine\Root` (the hosted runner is an
   administrator; no dialog), serves the signed files as `https://127.0.0.1:<port>/download/v<v>/…`.
5. Takes the **released, stamped** `install-desktop.ps1` from those files (a dry run replaces its key block with the
   dry-run key, as M1's `verify` does for `install.sh`) and runs it in **Windows PowerShell 5.1** with
   `ISSHONI_DOWNLOAD_BASE=https://127.0.0.1:<port>` and `ISSHONI_NO_LAUNCH=1`.
6. `deploy/test/windows/smoke.ps1 -Version <v> -Mode release`: files and their SHA-256 against the stage list,
   registry values, the protocol handler command exactly `"<InstallDir>\isshoni.exe" --open-url "%1"`, shortcut
   and its AUMID, `isshoni.exe --version` = `<v>` (with `engineAbi` 1), `--self-check` `ok` with `ffmpegLoaded`
   true, `ffmpegLicense` `LGPL version 2.1 or later` and `testHooks` false, `licenses\` complete; then seeds
   `cmdkey /generic:isshoni:v1:https://smoke.invalid /user:t /pass:x` and the toast keys of §2.3 (an
   `AppUserModelId\io.isshoni.desktop` key with `CustomActivator` and its `CLSID` key whose `LocalServer32` points
   into `InstallDir`, plus a `Software\isshoni\NotificationCategories` subkey), runs `uninstall.exe /S /PURGE` and
   asserts: nothing of §2.2 (the data root included) and §2.3 left, and `cmdkey /list` shows no `isshoni:v1:`
   target.

### 11.5 Signing keys (M1 06 §9.4's table, updated)

| Key | Signs | Lives | Verified by |
|---|---|---|---|
| `isshoni-release-1` / `-backup-1` | `checksums.txt` (now covering the desktop assets) | as M1 | `install.sh`, `install-desktop.ps1`, the site, users |
| **`isshoni-update-1`** (ssh-ed25519, no passphrase) | `isshoni-desktop-update.json` (namespace `isshoni-update`, principal `isshoni-update`) | secret `RELEASE_UPDATE_SIGNING_KEY` in environment `release` only | the desktop app (embedded), `check-draft.sh`, users |
| **`isshoni-update-backup-1`** (ssh-ed25519, passphrase) | nothing until a rotation or a leak | offline (password manager and a printed copy) | the same |
| Sigstore keyless, GitHub attestations | as M1, plus the desktop assets and the deps release | | |
| SignPath Foundation certificate | **later**: `isshoni.exe`, `isshoni-media.dll`, `uninstall.exe`, the installer (§12.4) | SignPath's HSM | Windows (Authenticode) |
| **Later (M3)** macOS self-signed identity | the `.app` | environment `release` | macOS |

The update keys' fingerprints go into the root `README.md`'s "Release signing keys" section and `/security#keys`, and
`repocheck keys` checks them like the release keys (M1 06 §10.3).

### 11.6 Versioning and compatibility

- **One version** names the server binary, the SPA, `isshoni.exe`, `isshoni-media.dll`'s version resource, the
  installer and the manifest (`<v>`); `hello.client {kind: desktop, version: <v>, os: windows}` (M1 01). The engine's C
  ABI has its own integer (`im_version`, 04); the app refuses a DLL with another ABI major (only a dev setup can mix
  them).
- **Protocol**: N and N−1 (M1 01 §6.1). A desktop release never drops the previous protocol version, so an app that
  updated past its server still talks to it.
- **Supported pairs**: a desktop app and a server whose minor versions differ by at most one (0.x; patches are free in
  both directions). Outside that: 03's screens for `protocol_unsupported` and `client_outdated`, with "Open in browser".
  A server without `deviceFlow` in `/api/v1/info` `features` (every 0.1.x server) supports no desktop app: there is
  no device flow and no bearer path (M1 left `AuthenticateBearerToken` a stub). The app shows 02's `update_server`
  screen (reason `no_device_flow`, 02 §13.2 rule 3) with Open in browser, and links nothing.
- **How it is tested**: M1's golden N−1 fixtures (old clients' messages against the current hub); the nightly
  `compat-servers` job (current client core against the last two final servers, §10.3); a manual check in each minor
  release's checklist (the previous desktop release against the rc server, §11.8).
- **Two floors, two owners**: the server admin's `minClientVersion` (M1 03 setting; rejects older apps) and the
  project's `minVersion` in the manifest (marks the update required, never blocks). `deploy/desktop/min-version` is
  raised only in a release-prep PR that fixes a security problem in the app, and the release notes say so.
- **The bundled SPA is never "stale"**: the app's SPA may differ from the server's version by design; 03 turns off the
  web client's stale-build reload in the desktop platform (M1 05 §16.4).

### 11.7 Release notes header additions (`.goreleaser.yaml` `release.header`)

```
**Windows app:** isshoni-desktop_{{ .Version }}_windows_amd64_setup.exe — install it from your isshoni server's
Download page, or `irm https://<your server>/install-desktop.ps1 | iex`. 0.x builds are not code-signed yet: Windows
may say "Windows protected your PC" (More info → Run anyway); see https://moonwx.github.io/isshoni/download#windows-smartscreen.
PCs with Smart App Control turned on can't run unsigned apps.
**Desktop update floor:** {{ .Env.ISSHONI_DESKTOP_MIN_VERSION }} (TODO: raised in this release? yes/no)
**macOS and Windows archives** (isshoni_*_darwin_*.tar.gz, isshoni_*_windows_*.zip) are experimental server builds, not the app.
FFmpeg {{ .Env.ISSHONI_FFMPEG_VERSION }} (LGPL-2.1-or-later): source in isshoni-desktop_{{ .Version }}_ffmpeg-source.tar.
```

(The `build` job exports `ISSHONI_DESKTOP_MIN_VERSION` from `deploy/desktop/min-version` and `ISSHONI_FFMPEG_VERSION`
from `native/ffmpeg/pins.json` before goreleaser runs; both names join the reserved list of §7.3. The `TODO` makes
`publish` refuse until the owner edits it, as in M1.)

### 11.8 The first 0.x release with the app (`v0.2.0-rc.N`, then `v0.2.0`)

Assumes `v0.1.0` (M1) is out. The checklist joins M1 06 §9.1's "Before tagging" list; items marked (owner) need the
PC or a GitHub setting.

1. (owner) Keys and settings of §14 done: update keys created, `update_signers` merged, secret in `release`; the FFmpeg
   deps release published and `lock.json` merged; the `deps-*` tag ruleset; the self-hosted runner registered on both
   OS installs; Pages on (M1).
2. CI green on `main`; the last nightly green, including `windows-full` and `compat-servers`.
3. The release dry run green end to end, including `verify-windows`.
4. Tag `v0.2.0-rc.1`; approve; the release is a prerelease.
5. (owner) **Audiotest** on Win11 and on Win10 22H2 with the rc's own DLL: the `isshoni-exit-tools` artifact of the
   rc's `release.yml` run, by hand (05 §18.8) or through `audiotest.yml` (suite `release`, `release_run_id` = that
   run); both PASS with `dll.sha256` equal to the rc's `native-manifest.json`; the reports are attached to the
   release-prep PR. **Discord desktop** real-app check on both (§10.5). This is half of the exit
   criterion.
6. (owner) **Latency**, the other half of the exit criterion: **M-LATENCY** (01 §12.2's `tools/g2g` run book; 04
   §15, §18.4): p95 < 200 ms over 60 s in the `gate` run of `g2g.exe` with a monitor share on one monitor, on Win11
   (borderless WGC) and on Win10 22H2 (Desktop Duplication), each with a hardware encoder, Chrome and the rc server at
   RTT ≤ 50 ms; the `g2g` JSON files attached to the release-prep PR with the GPU, driver and encoder used. Plus 04's
   **M-WDA**, **M-DD** and **M-WGC-WIN** (04 §18.4, §18.5) repeated on the rc installer build. The release isn't cut
   with only one OS measured.
7. (owner) **Install paths**: (a) Win11, Edge and Chrome: the browser download, the SmartScreen path, then the
   `/download` page's "Open isshoni" button (`appLinkFor`, §7.5) opens the app for the rc server, which then links
   through 02's device flow; also once as Finish → Start isshoni, then "Open isshoni" on the page: it passes with no
   address typed and the add step shown for the page's server, and from a room's `/download?room=<id>` link the room
   opens after linking; (b) Win10 22H2 and Win11: the `/download` page's displayed command (§7.5 `ps1Command`)
   verifies with the inbox OpenSSH, installs without a SmartScreen dialog and opens the wizard for that server. On
   Windows 10 22H2 this runs on a **clean install** (a VM, or a fresh user profile on a PC with nothing extra
   installed: no Visual Studio, .NET SDK or other developer tools, which can set machine-wide TLS defaults); record
   in the release-prep PR the value of `[Net.ServicePointManager]::SecurityProtocol` printed by a fresh
   `powershell.exe -NoProfile`, and whether `SchUseStrongCrypto` and `SystemDefaultTlsVersions` exist under
   `HKLM\SOFTWARE\Microsoft\.NETFramework\v4.0.30319` and `HKLM\SOFTWARE\WOW6432Node\Microsoft\.NETFramework\v4.0.30319`.
   If TLS 1.2 is not in that default, the displayed command must be the TLS-prefixed form of §7.5 (`links.test.ts`
   asserts the exact string) and §7.1 step 2 stays; a release whose page shows the bare form on such a PC is not cut;
   (c) Windows Defender: no detection of the installer or the installed files (also checked on VirusTotal); a false
   positive is submitted to Microsoft's file submission portal before the final tag; (d) the firewall prompt at the
   first share is noted, and sharing works whichever button is chosen; (e) when a clean Windows 11 VM (or PC) with
   Smart App Control on is available: the server page's Smart App Control line matches what Windows shows for the
   browser-downloaded installer, and the ps1 one-liner prints its Smart App Control line and downloads nothing; when
   none is available, the release-prep PR says (e) was not run.
8. Tag `v0.2.0-rc.2` (any change, or a rebuild); (owner) the rc.1 app on the beta channel finds it, downloads it,
   updates at the next start and comes back as rc.2 (`--updated-from`). During that start the "Updating isshoni to
   0.2.0-rc.2…" window shows until the rc.2 window opens. Repeat once with a second rc (or by reinstalling rc.1 and
   letting it download again), this time starting the update from the browser's "Open in the isshoni app" for a room
   and double-clicking the Start-menu shortcut while the window shows: one install happens (the app's log shows one
   `Apply` and one start that exited at the installing mutex; no "The update didn't install" notice), and the
   relaunched app opens that room.
9. (owner) Uninstall keeping data: the device disappears from the Devices page and the `isshoni:v1:` entry from
   Credential Manager, and settings remain. Then reinstall and uninstall with purge: the data root is gone.
10. (owner) The relink check of §9.1 (first FFmpeg pin).
11. The previous release's web client against the rc server: watch and share both ways. The rc app against `v0.1.x`
    shows "Ask your admin to update the server" with Open in browser, and nothing gets linked (§11.6).
12. Site and docs merged (§13): `/download`, `/privacy#desktop`, `/code-signing`, `/security` with the update keys'
    fingerprints, troubleshooting entries; README's keys table.
13. Release notes: known limits (unsigned; Smart App Control; the WGC yellow border for windows on Windows 10; x64
    only; FFmpeg encoders need current GPU drivers; the firewall prompt). M1's header line answered: "Database
    migration: yes (schema 2, device flow, 02 §4.1). Going back to 0.1.x needs `isshoni admin restore --offline
    backups/pre-1-<ts>.db`." (a 0.1.x binary refuses the migrated database).
14. Release-prep PR (M1 06 §9.1 step 4, with `release:compat`), then tag `v0.2.0`, approve, publish. This is "the first
    public release" after which the owner applies to SignPath (§12.3).

**Every later release** (the plan's tiers): the audiotest on Win11 and the smoke (install or update, one share with the
Discord app running, a browser viewer). **Every minor release**: 05 §18.9's R7 for every app whose built-in row (05
§5.4) changed since the last minor release; the full matrix, Win10 22H2 and Win11, Discord stable/PTB/Canary/Web and
TeamSpeak/Mumble.

## 12. Signing: unsigned 0.x, SmartScreen, Smart App Control, SignPath

### 12.1 What an unsigned 0.x build does

- **Browser download**: Edge and Chrome may first say the file "isn't commonly downloaded" (Edge: "…" → Keep → Show
  more → Keep anyway; Chrome: Keep). On launch, SmartScreen says "Windows protected your PC": **More info → Run
  anyway**. The reputation of a new unsigned file starts at zero for every release.
- **`irm … | iex`**: `Invoke-WebRequest` writes no Mark of the Web, so SmartScreen doesn't check the installer, and the
  installer's files carry none either.
- **In-app updates**: Go's `net/http` writes no Mark of the Web either; no prompt.
- **Smart App Control** (on for some clean Windows 11 installs): blocks unsigned executables and very likely unsigned
  DLLs, wherever they came from, with no "run anyway"; turning it off can't be undone without reinstalling Windows. 0.x
  builds don't run there, and the docs say so and point to the browser sharer until signed builds exist.
- **Windows Defender** may flag a new unsigned Go binary heuristically; the release checklist checks this (§11.8 7c).
- No UPX or other packer is ever used: packed binaries are flagged far more often.

### 12.2 What the docs say (`/download`, anchors of §13)

`#windows-smartscreen` (the two browser warnings and SmartScreen, with screenshots, why: "isshoni is applying for free
code signing; until then Windows doesn't know the publisher", and how to check the file instead: `#windows-verify`);
`#windows-smart-app-control` (as above, honestly); `#windows-powershell` (the one-liner, what it checks, the OpenSSH
Client requirement, running it elevated, what each error means); `#windows-verify` (PowerShell commands, §13).

### 12.3 The SignPath Foundation application (owner, right after `v0.2.0`)

**Prerequisites and their status:**

| SignPath asks for | Status after M1 | M2 work |
|---|---|---|
| An OSI license, public source, active project | Apache-2.0, public on GitHub | — |
| A released version that would be signed | — | `v0.2.0` with the Windows app |
| A code-signing policy page on the project site | `/code-signing` exists (M1 06 §10.2): what is signed, roles (committers, reviewers and approvers: @MoonWX), build rules, privacy statement, contact | update (below) |
| The line "Free code signing provided by SignPath.io, certificate by SignPath Foundation" | already in the Windows row | keep; show it on the release notes and README only after approval |
| A privacy statement for the **signed software** | the statement on `/code-signing` lists only the server's connections | add the desktop app's: the update check (on by default, can be turned off at install and in settings), OpenH264 only when the user turns it on, the WebView2 bootstrapper when the runtime is missing, and the servers the user links (`/privacy#desktop`) |
| Builds only from the repository on GitHub-hosted runners, from tags; MFA on owner accounts; manual approval of each signing request | M1's release pipeline (tags, the `release` environment with owner approval) | the SignPath jobs run in the same pipeline (§12.4) |
| Product name and version in every signed PE file | — | §5.1, §5.2, §6.2 |
| Third-party binaries not signed with the project's certificate | stated on `/code-signing` | the list of unsigned files: `avcodec-*.dll`, `avutil-*.dll` |

**`/code-signing` changes in M2**: the Windows row becomes "0.x: unsigned (see `/download#windows-smartscreen`). After
approval: Free code signing provided by SignPath.io, certificate by SignPath Foundation — signed: `isshoni.exe`,
`isshoni-media.dll`, `uninstall.exe`, `isshoni-desktop_<v>_windows_amd64_setup.exe`; not signed: the FFmpeg DLLs
(third-party, unmodified)"; a row for the update manifest key; the privacy section's exceptions gain the desktop app's.

**The application** (owner, on SignPath Foundation's site): project URL, description (stress that every capture
starts only after the user picks a window or screen in the operating system's own picker, that a sharing indicator is
always shown, and that there is no hidden capture), the release URL, the policy URL, GitHub Actions as the build
system, artifact types (PE executables and DLLs, an NSIS installer). The review can take weeks; M2 ends with the
application sent.

### 12.4 After approval (**later**: slice R18, likely in M3's time)

1. (owner) In SignPath: connect the GitHub repository as a trusted build system; create the signing policy
   `release-signing` with the owner as approver; the artifact configurations `payload` (a zip with `isshoni.exe`,
   `isshoni-media.dll`, `uninstall.exe`, each Authenticode-signed with SHA-256 and an RFC 3161 timestamp; the FFmpeg
   DLLs are not in the zip) and `installer` (the setup exe); a CI user token → secret `SIGNPATH_API_TOKEN` in the
   `release` environment.
2. `desktop-pkg` splits into `desktop-payload` (stage, prebuilt uninstaller, the `payload` zip as an artifact) →
   `signpath-payload` (`environment: release`; `signpath/github-action-submit-signing-request` pinned by SHA, with
   `github-artifact-id`, waits for completion) → `desktop-installer` (makensis with the signed files) →
   `signpath-installer` → the rest as in §11.1. SignPath's origin verification needs the artifacts to come straight from
   the workflow's own jobs, which this shape gives.
3. `verify-windows` adds `Get-AuthenticodeSignature` = `Valid` with SignPath Foundation's subject for the four files;
   `install-desktop.ps1` gains the Authenticode fallback of §7.2; the updater may additionally check the installer's
   signature (defence in depth; the manifest stays the root of trust).
4. Measure Smart App Control on a Windows 11 PC with it on: if it blocks the unsigned FFmpeg DLLs next to a signed exe,
   that is the open question of §20.

## 13. Project site and docs

| Page | Change |
|---|---|
| `docs/download.md` (`/download`) | from "Coming" to the Windows page: `#windows` requirements (Windows 10 22H2 or later, 64-bit; a current GPU driver, NVIDIA 531.61 or newer for NVIDIA encoding, linking `/troubleshooting#desktop-gpu-driver`; Windows N and KN editions need the Media Feature Pack, linking `#desktop-media-feature-pack`; 04 §19); install from your server's Download page; `#windows-powershell`; `#windows-smartscreen`; `#windows-smart-app-control`; `#windows-verify`; `#windows-updates` (how updates work, channels, turning them off, the policy value, what to do when an update fails); `#windows-uninstall` (every uninstall unlinks the app from its servers and removes its sign-ins; keep settings and logs vs purge, which removes the data root `%LOCALAPPDATA%\isshoni\`); `#windows-firewall` (the prompt, either answer works); `#windows-webview2`; `#ffmpeg-source` (the LGPL source and how to replace the DLLs); `#macos` (M3) and `#linux` (M4) "coming". It shows the latest version and the installer's SHA-256 from `release.json` |
| `docs/privacy.md` | **R14 owns `/privacy#desktop`** and removes "(with the desktop apps, which come later)" from "What never leaves your device"; 02's DA12 adds only its server-side device rows and links here for the PC side (one owner, no merge conflict). The `#desktop` section (03 §19, 02 §9): **connections** (the servers you link; GitHub for update checks, once a day, with `User-Agent: isshoni-desktop/<version>`, off in settings, at install or by policy; Cisco for OpenH264 only when turned on, 04's URL; Microsoft's WebView2 bootstrapper only when missing; Windows' own SmartScreen check of browser downloads); **sent to a server at linking**: the device name (the computer's name by default, editable in the wizard); **never sent**: window titles, share labels, app lists, exclusion rules, levels, logs; **kept on the PC**, all under `%LOCALAPPDATA%\isshoni\` (nothing roams): settings (`settings.json`), the server list (`servers.json`: origins, server names, usernames, device ids, last room), the WebView2 profile, logs, `codecs\` and the updates folder; the refresh token in Windows Credential Manager (target `isshoni:v1:<origin>`), no access token on disk (memory only); **no telemetry** |
| `docs/code-signing.md` | §12.3 |
| `docs/security.md` | the update keys in `#keys`; `#verify-desktop`: verifying the installer on Windows (below) and a manifest with `ssh-keygen`; the limit of key revocation (§8.12) |
| `docs/troubleshooting.md` | `#desktop-update-failed`, `#desktop-smart-app-control`, `#desktop-webview2`, `#desktop-openssh`; `#desktop-windows-n` (Windows N or KN: viewing H.264 fails in the app and in Edge until the Media Feature Pack is installed; 03 links it from its codec-banner text, 03 §5.4, and from its N-edition guided fix); `#desktop-media-feature-pack` (installing the Media Feature Pack and what it restores: H.264 decoding for viewing and the MF encoders for sharing, 04 §9.1); `#desktop-gpu-driver` (NVIDIA 531.61 or newer for NVENC, current AMD and Intel drivers, what 03's `driver_too_old` fix means); `#desktop-fullscreen-games` (full-screen exclusive games may capture black or the desktop; use borderless, 04 §7.7); `#desktop-crash-dumps` (Windows Error Reporting `LocalDumps` steps and the release's `…_symbols.zip`, 04 §13.4); `#desktop-windows10-border` (the yellow border around a shared window on Windows 10, expected); and **`#desktop-linking`**, a fixed anchor for 02's "Linking the app" section (02 DA12 writes the section's content; this document fixes the anchor and checks it) |
| root `README.md` | the update keys in "Release signing keys"; the Windows app in Install |
| `site.yml` | serves `/keys/update_signers` (verified against main's file like `allowed_signers`); `release.json` gains `{"desktop": {"windowsInstaller": "<name>", "sha256": "<hex>"}, "updateKeyFingerprints": […]}` from the verified `checksums.txt` and `update_signers` |
| `internal/server/ops/doctor/docsanchors_test.go` | the new fixed anchors above join the checked list (M1 06 §10.4: `TestDocsAnchors`), since the app (03), the SPA, the installer and the ps1 link to them: every anchor of §17's "Site anchors", including `/troubleshooting#desktop-linking`, `#desktop-windows-n`, `#desktop-media-feature-pack`, `#desktop-gpu-driver`, `#desktop-fullscreen-games`, `#desktop-crash-dumps` and `#desktop-windows10-border` |

`#verify-desktop`:

```powershell
$V = '0.2.0'; $B = "https://github.com/MoonWX/isshoni/releases/download/v$V"; $F = "isshoni-desktop_${V}_windows_amd64_setup.exe"
foreach ($n in $F, 'checksums.txt', 'checksums.txt.sig') { Invoke-WebRequest -UseBasicParsing "$B/$n" -OutFile $n }
Invoke-WebRequest -UseBasicParsing https://moonwx.github.io/isshoni/keys/allowed_signers -OutFile allowed_signers
cmd /c "ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt"
(Get-FileHash $F -Algorithm SHA256).Hash.ToLower(); Select-String -Path checksums.txt -SimpleMatch -Pattern $F
```

(`cmd /c` because PowerShell has no `<` redirection, and a pipe would change the bytes.)

## 14. Owner actions

Before the first rc with the app:
1. **Update keys** (on the Mac, outside the repository), then a PR:
   ```sh
   ssh-keygen -t ed25519 -N '' -C isshoni-update-1 -f isshoni-update-1
   ssh-keygen -t ed25519 -C isshoni-update-backup-1 -f isshoni-update-backup-1     # asks for a passphrase
   for k in isshoni-update-1 isshoni-update-backup-1; do
     printf 'isshoni-update namespaces="isshoni-update" %s\n' "$(cat "/path/to/$k.pub")"
   done > deploy/keys/update_signers
   task release:update-keys     # writes internal/update/signers.go, prints fingerprints, runs lint:keys
   ```
   Put the two fingerprints into README's "Release signing keys" table, merge; secret `RELEASE_UPDATE_SIGNING_KEY` =
   the private `isshoni-update-1` in the `release` environment; the backup into the password manager and on paper;
   delete local private copies. `deploy/keys/README.md` gets these steps.
2. **FFmpeg**: Actions → FFmpeg → Run workflow with `publish` on (from the branch of the pins PR); commit the printed
   `lock.json` into that PR.
3. **Rulesets**: a tag ruleset for `deps-*` (block update and deletion; creation by repository writers, which is only
   the owner).
4. **Actions setting**: "Require approval for all external contributors".
5. **Self-hosted runner** on the Win11 and the Win10 22H2 install (§10.5, ephemeral), and the environment `audiotest`
   with the owner as required reviewer, created before the first dispatch (§10.4).
6. Consider GitHub's immutable releases (M1's suggestion); with them the deps releases can't change either.

During the release: the checklist items marked (owner) in §11.8.

After `v0.2.0` is public: the SignPath application (§12.3); after approval, §12.4 step 1.

Nothing needs to be bought or rented: the rc server is M1's test VPS.

## 15. Main risks

| Risk | Mitigation |
|---|---|
| Unsigned 0.x: SmartScreen friction, Smart App Control blocks, Defender false positives | the ps1 path (no Mark of the Web), honest docs, Defender/VirusTotal check per release with false-positive submissions, SignPath right after the first public release, no packers |
| Smart App Control may block the unsigned FFmpeg DLLs even after SignPath | measured after approval (§12.4); the owner's decision (§20) |
| Wails v3 beta churn | exact pin, hand bumps with the Windows smoke, no Dependabot; the app's Wails surface is 03's to keep small |
| Runner image drift (MSVC, MSYS2, makensis) | VS selected by version range `[17.0,19.0)` plus the C++ tools component, toolset pinned to `14.44` with a fail-fast check that prints the instance, toolset build and SDK found (§3; `windows-2025` already moved to VS 2026); pinned NSIS zip; FFmpeg built once and locked; toolset recorded; static CRT; `dumpbin` checks catch a new runtime import |
| FFmpeg security fixes | small build (encoders only), §4.7's process, a patch release |
| Update key leak | separate key in the protected environment, offline backup, `revokedKeys`, an advisory (§8.12) |
| Self-hosted runner abuse from a public repository | dispatch-only workflow, runner offline outside sessions, non-admin account, approval for all external contributors |
| GitHub API rate limits on shared IPs | stable channel avoids the API; beta waits for the next check |
| Windows 10's inbox OpenSSH (8.1p1) quirks | allowed-signers options limited to what 8.1 parses; checked on the owner's Win10 22H2 in the checklist; fail-closed message with the alternative |
| WebView2 missing or stale (older than 120.0) on a Windows 10 PC | embedded bootstrapper, per-user install, the installer's minimum equal to 03's (§6.5), clear exit code 7 and doc |
| An update that fails to install loops | `attempts` guard, link to the manual path |
| A second start or a second setup during an update (two installers on the same files, or a new instance taking the running mutex the installer waits for) | the installer's `Local\io.isshoni.desktop.installing` mutex for its whole run, exit 8 for a second setup, 03's step 2a, `Apply`'s handover wait, the "Updating isshoni to {v}…" window so the friend sees the update (§2.4, §6.7, §8.7) |

## 16. Test plan

| Level | Test | Asserts | Where |
|---|---|---|---|
| Unit | `internal/update` `sshsig_test.go` | committed vectors (an ephemeral key made once, its public key, message and signature in `testdata/`) verify; a flipped byte in message, signature or blob, the wrong namespace, `sha256` instead of `sha512`, an untrusted or revoked key, trailing bytes, bad armor and oversize input all fail; the verifier reports which key matched and whether it is the backup (an entry whose comment is `isshoni-update-backup-<n>`), and a persisted revocation naming the backup key doesn't stop it verifying; when `ssh-keygen` is on the PATH, signatures it makes now verify too; `FuzzSSHSIG` (30–60 s per PR, seeds in `testdata/fuzz`) | CI ubuntu, macos, windows; Mac |
| Unit | `manifest_test.go` | schema, product, channel acceptance per app channel (`ci` only with the `updatetest` tag), SemVer compare through `internal/version` (rc < final; a `version` or `minVersion` that doesn't parse → `bad_manifest`), `Eligible` (empty and `rc.N` yes, `ci.N` only with `updatetest`, dev no), expiry at the boundary, `minVersion` → required, artifact selection per OS/arch, the exact URL rule; `revokedKeys`: an active-key manifest revoking the backup → ignored, nothing persisted, the backup still verifies the next manifest; an active-key manifest revoking another active key → ignored; a backup-key manifest revoking the active key → persisted in `state.json` (also when the manifest is not newer) and a later manifest signed by that key → `bad_signature`; a backup-key manifest listing the backup itself → that entry dropped, the rest honoured; the internal `applyRevocations` helper called directly with a list that covers every trusted key (unreachable through a manifest, since the backup entry is always dropped; the check is defence in depth) → refused as a whole, nothing persisted; unknown fields ignored, 64 KiB limit | same |
| Unit | `update_test.go` (httptest TLS server, `testing/synctest`) | first check in 60–300 s and not before the wizard; `CheckNow` before `WizardDone` makes the request (and makes none when `Enabled` is false); daily ± 1 h; 6 h after an error; `CheckNow` debounce; `Ready()` non-nil only in `StateReady`; a newer verified manifest in a copy outside `InstallDir` → `error`/`not_installed` with `Available` and `NotesURL` and zero download requests; after 2 failed attempts → `error`/`install_failed` with `Available` until a newer manifest appears, which downloads; `Apply` re-verifies (a changed file → `hash_mismatch`, deleted) and returns `ErrNotInstalled` outside `InstallDir`; `PendingAtStartup(true)` (an `--autostart` start) returns nil while `PendingAtStartup(false)` returns the same pending update; `Enabled` false → the server counts zero requests; redirects to another host or to http refused; stall and total timeouts; size and hash mismatch delete the partial file; disk check; `state.json` atomic and versioned; `PendingAtStartup` rules (newer, not expired, verifies, attempts < 2); `ErrNotInstalled`; beta listing parse, 403/429 → `rate_limited`, non-SemVer tags skipped; goleak | same |
| Unit | `apply_windows_test.go`, `policy_windows_test.go` | the installer is started with exactly `/S /UPDATE /RELAUNCH` (a fake installer, a Go test binary, records its arguments); `Apply` returns only once the fake installer has created `Local\io.isshoni.desktop.installing`, and returns nil with one WARN after 10 s when it never does (an injected clock and mutex probe); the HKLM policy value is read (a test key under HKCU through an injected root) | CI windows |
| Unit | `tools/desktoppkg` | the winver table; stage list and mtimes; NSIS defines; manifest golden; `check` rules (unknown binary, literal strings in NSIS, size budget: warn above 40 MB, fail above 100 MB); `ffmpeg-inputs` hash; `dll-check` against fixture `dumpbin` output (an `avcodec-*`/`avutil-*` import fails; `mfplat.dll` and `avrt.dll` pass only as delay imports; an FFmpeg DLL importing `mfplat.dll` fails; DLL names unequal to `lock.json`'s majors fail); `stage -release` refuses a `windows_amd64-test` exe and a DLL whose `--self-check` reports `testHooks: true` (fixture) | CI ubuntu; Mac |
| Unit | `tools/notices` desktop golden | the native section; LGPL accepted only dynamic and with the bundle; a GPL native entry fails; an unlisted vcpkg port fails; a `LicenseRef` without an exception fails | CI ubuntu |
| Unit | `internal/server/httpapi` `desktopscripts_test.go` | `RenderInstallPS1`: `'` doubled, a non-ASCII origin or an `http://` non-loopback origin gives an empty server, a dev/CI/e2e version gives an empty version, rc kept, missing or doubled markers error; the route: 200, content type with charset, `Cache-Control`, `ETag`, `HEAD` without body; `/install-desktop.sh` is 404 text, never the SPA; the template itself (`deploy.InstallDesktopPS1`) renders | CI ubuntu; Mac |
| Unit | `web/src/download/links.test.ts`, `DownloadPage.test.tsx` | release vs non-release links, the exact installer URL, the ps1 command, `openAppUrl` exactly `isshoni://watch.example.com/` for `https://watch.example.com` (via `appLinkFor`) and `""` for an `http://localhost` origin, the "Open isshoni" button hidden then, the requirement lines, the three warning lines (browser download, SmartScreen, Smart App Control) with their project-site anchors, OS ordering | CI ubuntu; Mac |
| Unit | `deploy/test/install-desktop.Tests.ps1` (Pester) | the checksum-line lookup (exact name, both forms, duplicates refused), version regex, the quoting helper with spaces, `ssh-keygen` discovery (mocked), the exit-code messages (8 included), no `exit` anywhere in the script; the Smart App Control read (mocked `Get-ItemProperty`): `1` → the `isshoni:` Smart App Control line, "Nothing was installed." and no `Invoke-WebRequest` call; `2`, `0` and a missing key → the download proceeds; a mocked `Start-Process` that throws → "isshoni: Windows blocked the installer (" with the exception's message, no error record escapes the script block | `pwsh` on CI ubuntu; Windows PowerShell 5.1 on CI windows |
| Unit | `check-draft-test.sh` new cases (§11.3); `repocheck` tests for `update_signers` (exactly one `isshoni-update-backup-<n>` line and at least one other; `Backup` set in `signers.go` exactly for it), `signers.go` block, ps1 block, the 8.1-option rule | | CI ubuntu; Mac |
| Static | PSScriptAnalyzer, ASCII-only, PowerShell 5.1 parse (`[System.Management.Automation.Language.Parser]::ParseFile` under `powershell.exe`), `makensis -WX`, actionlint, the DLL export/import/version checks, FFmpeg's license checks | | CI ubuntu and windows |
| Integration | `smoke.ps1 -Mode ci` in `windows-desktop` | fresh silent install: files, registry, the protocol command exactly `"<InstallDir>\isshoni.exe" --open-url "%1"`, shortcut AUMID, `--version` (`engineAbi` 1), `--self-check` (`ok`, `ffmpegLoaded`, `testHooks` false); upgrade from a build with an extra fake `avcodec-00.dll` in `install-files.txt` removes it; an upgrade while a helper process keeps `isshoni-media.dll` and an FFmpeg DLL mapped for 3 s after the mutex is gone succeeds (the `.old` rename loop, §6.7); same-version reinstall; an older build over a newer one exits 4 and `/ALLOWDOWNGRADE` works; a process holding the mutex makes `/UPDATE` exit 3 after 60 s; `/D=` into a folder with a foreign file, then uninstall: the foreign file and the folder stay. **The installing mutex**: while a helper process holds `Local\io.isshoni.desktop.installing` (`[System.Threading.Mutex]::new($false, 'Local\io.isshoni.desktop.installing')` in a second PowerShell), a second `setup.exe /S` exits 8 and changes no file or registry value, and starting the installed `isshoni.exe` through the registered protocol command (read from the registry, `%1` = `isshoni://smoke.invalid/r/abc`) exits 0 within 10 s with no `isshoni.exe` left running and no `Local\io.isshoni.desktop.running` ever created (polled every 100 ms), and leaves `PendingServer` = `https://smoke.invalid` and `PendingRoom` = `abc` (deleted by the test afterwards); during a `/S /UPDATE` run the installer's own process holds the mutex (`OpenMutexW` succeeds) and after it exits the mutex is gone. **Both uninstall modes** first seed `cmdkey /generic:isshoni:v1:https://smoke.invalid /user:t /pass:x`, the toast keys of §2.3 (an `AppUserModelId\io.isshoni.desktop` key whose `CustomActivator` names a `CLSID` key with `LocalServer32` in `InstallDir`, plus a second `CLSID` key pointing elsewhere) and a `Software\isshoni\NotificationCategories` subkey; afterwards `cmdkey /list` shows no `isshoni:v1:` target, the AUMID key, the matching `CLSID` key and `Software\isshoni` are gone and the foreign `CLSID` key stays. A keep-data uninstall keeps `%LOCALAPPDATA%\isshoni`; `/PURGE` removes it, and nothing of §2.2 and §2.3 is left | CI windows |
| Integration | server-rendered ps1 (`windows-desktop`) | `isshoni serve` (the CI binary, off mode on loopback) serves `/install-desktop.ps1`; with `ISSHONI_DOWNLOAD_BASE` pointing to relserve holding the CI installer signed by an ephemeral key (the block replaced as in §11.4), Windows PowerShell 5.1 installs it, `PendingServer` equals the server's origin; a tampered installer is refused with nothing installed; a `checksums.txt` signed by another key is refused; an `isshoni-desktop_<v>_windows_amd64_setup.exe` that is not an executable (a text file, correctly listed in a correctly signed `checksums.txt`) ends with the line `isshoni: Windows blocked the installer (` and "Nothing was installed.", and the `powershell.exe -NoProfile -Command "irm … \| iex"` process exits 0 with nothing on stderr (no unhandled error) | CI windows |
| Integration | updater e2e (`windows-desktop` on `update` changes; nightly always) | two `updatetest` builds (`TAGS="production updatetest"`, §5.2; eligible only through §8.11's `ci.N` rule) `0.0.1-ci.1` and `0.0.1-ci.2`, manifests with channel `ci` signed by an ephemeral key, served by relserve over `http://127.0.0.1`; install ci.1; `isshoni.exe --update-now`; within 120 s the registry says ci.2 and `--version` agrees and the relaunched process has `--updated-from=0.0.1-ci.1`; a manifest whose SHA-256 is wrong leaves ci.1 intact; a manifest for `0.0.1-ci.0` is ignored. **A start during an update** (`smoke.ps1 -Mode update`, the cases that need an eligible pending update): with ci.1 installed, ci.2's installer staged as the pending update (`updates\state.json`, `attempts` 0) and a helper holding `Local\io.isshoni.desktop.installing`, a start through the protocol command with `isshoni://smoke.invalid/r/abc` exits 0 within 10 s, `attempts` is still 0, no process runs from the staged installer's path and the registry still says ci.1; after the helper releases the mutex, a plain start applies ci.2 at start (`attempts` 1), and the relaunched ci.2 reads and deletes `PendingServer`/`PendingRoom` | CI windows |
| Integration | Go tests with the real DLL (04's content, 04 §18.2) | `go test -race ./internal/media/...` with `ISSHONI_TEST_ENGINE_DLL` = the `win-x64-ci` DLL of the artifact's `ci\` folder: load, probe, sessions with the fake encoder, `TestNativeIntoPublisher` | CI windows (`windows-desktop`) |
| Integration | DLL-free Go tests on Windows (`windows-go`) | `go test -race` (MinGW) and `CGO_ENABLED=0 go test` of the client core, `internal/update` and `internal/media` with the fake engine; 02's credstore (Credential Manager) and deviceauth | CI windows |
| Integration | native C++ (`native-host`, `windows-native`) | `host-debug` and `host-tsan` ctest of `native/common` and clang-format on ubuntu and macOS; `win-x64-ci` ctest (04 §18.2) and the DLL checks of §5.1 on windows | CI ubuntu, macos, windows; Mac (`task native:test`) |
| Integration | nightly `native-fuzz`, `windows-asan` | 60 s per fuzz target without a crash; `win-x64-asan` ctest clean | GitHub |
| Static | generated-code drift (`protocol` job) | `gen:check` incl. `web/src/platform/desktop/codes.gen.ts`; `GOOS=windows wails3 generate bindings` gives no diff in `web/src/platform/desktop/bindings/` | CI ubuntu; Mac |
| E2E | 03's desktop specs and CDP spec (03's content, run by 06's jobs) | `e2e` with `DESKTOP_E2E_BIN` (03 §22.4 A); `windows-desktop` with the `TAGS=e2e` exe over CDP (03 §22.4 B) | CI ubuntu, windows; Mac |
| E2E | release dry run | build → sign-dry → verify (Linux) and verify-windows (ps1 install, smoke, uninstall) | GitHub |
| E2E | Playwright `download.spec` | on the e2e server (non-release version): the latest-release link and the ps1 command with the server's origin; with `/api/v1/info` routed to `0.2.0`: the exact installer URL; `GET /install-desktop.ps1` is pre-filled | CI ubuntu; Mac |
| E2E | nightly `compat-servers` | against servers with `deviceFlow`: the client core joins, publishes with the fake engine and receives media, and 02's test links and refreshes; against a 0.1.x server: only the `compat.FromInfo` verdict `update_server`/`no_device_flow` | GitHub |
| Manual | `audiotest.yml` on Win11 and Win10 22H2; Discord real-app check | the exit criterion (audio) | owner's PC |
| Manual | `audiotest.yml` suite `hwcheck` (04's `hwcheck.ps1`) | 04 §18.4's automated set; the result bundle attached | owner's PC (self-hosted runner) |
| Manual | **M-LATENCY** (01 §12.2; 04 §15, §18.4) | p95 < 200 ms over 60 s in `g2g.exe`'s `gate` run, a monitor share on one monitor, on Win11 (borderless WGC) and on Win10 22H2 (DD), hardware encoder, Chrome, rc server RTT ≤ 50 ms; the `g2g` JSON files attached with GPU, driver and encoder; plus 04's M-WDA, M-DD and M-WGC-WIN on the rc installer build | owner's PC |
| Manual | §11.8 items 7–11 | install paths, SmartScreen, inbox OpenSSH, Defender, update rc.1 → rc.2 (the "Updating isshoni to {v}…" window shows from the click to the relaunch; a double-click, and a second "Open in the isshoni app" click, during a start-time update end with one install and the link handled), uninstall keeping data (device and credential gone, settings kept) then purge, relink, cross-version (0.1.x: "update the server" screen) | owner's PC, rc server |

Everything automatable runs on the Mac (`task test`, `task lint`, `task native:test`, `task desktop:exe:windows`,
Homebrew makensis) or in GitHub Actions; only §11.8's manual items, the audiotest and `hwcheck` need the owner's PC.

## 17. Interfaces other docs rely on

- **Asset names** of §2.1 and the URL patterns `https://github.com/MoonWX/isshoni/releases/download/v<v>/<name>` and
  `…/releases/latest/download/isshoni-desktop-update.json`.
- **Install layout** (§2.2): `$INSTDIR` = `%LOCALAPPDATA%\Programs\isshoni`, `isshoni.exe`, `isshoni-media.dll`,
  `avcodec-<N>.dll`, `avutil-<N>.dll` in the same folder, `licenses\THIRD_PARTY_NOTICES.txt` (03's About → Licenses
  opens it), `licenses\ffmpeg\README-LGPL.txt`. One data root, `%LOCALAPPDATA%\isshoni\` (nothing roams): 03's
  `settings.json`, 02's `servers.json`, `codecs\` (03's: OpenH264, written through 04's `native.InstallOpenH264`),
  `updates\` (this document's), and `logs\` and `WebView2\` (03's).
- **Registry** (§2.3): `HKCU\Software\isshoni\{InstallDir,Version,DesktopShortcut,UpdateChecks,WinVer,PendingServer,
  PendingRoom}` (`PendingRoom` written by 03 next to `PendingServer` before a start-time update, §8.7, or at a start
  from a link that found the installing mutex, §2.4; the whole key,
  subkeys included, is deleted by the uninstaller), the protocol handler command `"…\isshoni.exe" --open-url "%1"`,
  the `Run` value name `isshoni`, the policy `HKLM\SOFTWARE\Policies\isshoni\DisableUpdateChecks`; AUMID
  `io.isshoni.desktop` on the Start-menu shortcut; the app-written toast keys
  `HKCU\Software\Classes\AppUserModelId\io.isshoni.desktop` and its `CustomActivator`'s `CLSID` key, which the
  uninstaller removes (§6.8).
- **Process contract** (§2.4): the mutex `Local\io.isshoni.desktop.running`; the installer's mutex
  `Local\io.isshoni.desktop.installing` (held for the installer's whole run, closed right before it starts the app;
  `update.Apply` returns once it exists; 03 §13.4 step 2a exits 0 at a start that finds it, after writing a link to
  `PendingServer`/`PendingRoom`); the flags `--version` (with `engineAbi`
  = 1, the ABI major, without loading the DLL), `--self-check=<path>` (its JSON with 03's `spa.ok`, and the `ok` rule),
  `--open-url <url>`, `--updated-from=<v>`, `--forget-credentials` (every uninstall), `--purge-data` (purge),
  `--update-now` (updatetest only); no kill-on-close job object.
- **Installer flags and exit codes** (§6.4, exit 8 = another setup is running); the `/UPDATE` banner; the
  installer's WebView2 minimum `WEBVIEW2_MIN` = 03 §5.4's 120.0.
- **`internal/update`** Go API (incl. `Ready()`, `PendingAtStartup(autostart)`, `Apply`'s re-verify and
  `ErrNotInstalled`), states and error codes (§8.9, §8.10); its use of `internal/version` for every comparison.
- **Manifest** format (§8.2), signature format and namespace `isshoni-update`, principal `isshoni-update`,
  `deploy/keys/update_signers`, `deploy/desktop/min-version`, `deploy/keys/update_revoked`; the `updatetest` hooks
  (§8.11) that 03's DK12 tests use.
- **Build**: `task desktop:exe:windows [TAGS=…]` (default `production`), `task desktop:package:windows`,
  `task ffmpeg:fetch`, `task ffmpeg:build`, `task release:wails3`, `task release:update-keys`, `task lint:ps` (also
  `native/windows/tools/*.ps1` and `deploy/test/windows/*.ps1`), `task test:ps`, `task lint:pins` (Wails library, CLI
  and `@wailsio/runtime`); four tasks WA04 added beyond this list, so CI and the Mac share one command each:
  `task lint:native` (`clang-format --dry-run --Werror` over the tracked C and C++ files under `native/`;
  `CLANG_FORMAT=<binary>` picks the binary, and outside CI a missing clang-format is skipped with a note),
  `task lint:go:cross` (§10.1's cross-builds), `task gen:bindings` (`GOOS=windows wails3 generate bindings` into
  `web/src/platform/desktop/bindings/`) and `task gen:bindings:check` (the drift check); `lint:native` and `lint:ps`
  are part of `task lint`, `test:ps` of `task test`. Outputs go to the git-ignored `.deps/` (vcpkg, FFmpeg) and
  `dist-desktop/`. The native tasks 04 relies on:
  - `task native:test`: vcpkg bootstrapped at `native/vcpkg.json`'s baseline into `.deps/vcpkg` (`VCPKG_DIR=<dir>`
    elsewhere; CMake and Ninja from `PATH` when present, else vcpkg's SHA-512-checked `vcpkg fetch` copies, so nothing
    is installed globally), then configure, build and ctest of preset `host-debug` (`PRESET=host-tsan` or `host-asan`
    for the sanitizers; Mac, Linux);
  - `task native:build` (Windows): presets `win-x64-release` and `win-x64-ci` with `IM_FFMPEG_ROOT` = the
    `ffmpeg:fetch` output in `.deps/ffmpeg/`;
  - `task native:dist`: a flat zip of `GOOS=windows` `isshoni-enginetest.exe`, `g2g.exe` (01's `tools/g2g`),
    `native/windows/tools/hwcheck.ps1`, the release `isshoni-media.dll` and the FFmpeg DLLs (CI uploads it as
    `isshoni-native-tools`, §5.1);

  the MSVC environment step `deploy/windows/msvc-env.ps1` (VS `[17.0,19.0)` with the C++ tools component, toolset
  `14.44`, SDK `10.0.26100.0`, from `tools.json` `msvc`; `task native:build` runs it, §3);

  the build tags `production` (the only tags a release has), `updatetest`, and 03's `dev` and `e2e` (never shipped,
  §5.2 step 3); the `cmd/isshoni-desktop` build constraint (§5.2); winver (§5.3); version-resource fields (§5.1).
- **FFmpeg artifact**: the `IM_FFMPEG_ROOT` layout `include/` (all 04 compiles against), `lib/` (informational),
  `bin/*.dll`, `pdb/`, `licenses/`, `ffmpeg-config.json`; the four encoders and nothing else; no static import of
  `mfplat.dll`, `mf.dll`, `mfreadwrite.dll` or `evr.dll`; `native/ffmpeg/lock.json`, whose integer fields
  **`avcodecMajor` and `avutilMajor`** 04's CMake reads for the DLL names and the version check (04 §9.2).
- **DLL checks** that 04's build must pass: exports = `IM_API` functions of `isshoni_media.h`; imports on
  `deploy/windows/dll-imports.txt` (`static:` and `delay:` sections; `mfplat.dll`/`avrt.dll` delay only; no
  `avcodec-*`/`avutil-*` import ever); static CRT; version resource from `IM_VERSION`/`IM_WINVER` (and `IM_COMMIT`
  for `im_build_info`); presets `win-x64-release` (shipped) and `win-x64-ci` (tests; never staged).
- **Server**: `GET /install-desktop.ps1` (text/plain; charset=utf-8, pre-filled), `GET /install-desktop.sh` 404 until
  M3; the Go package `deploy` (`deploy.InstallDesktopPS1`); `httpapi.DesktopScripts`, `RouterOptions.Desktop`,
  `httpapi.RenderInstallPS1`.
- **SPA**: `web/src/download/` and `desktopLinks()` with `openAppUrl` = `appLinkFor(publicUrl)` (§7.5); the page is
  browser-only.
- **Native notices**: `deploy/notices/native-windows.json` format; an entry per native dependency, added with it.
- **Env names** (§7.3) for the server's reserved env list (M1 04 §4.2), with `ISSHONI_TEST_ENGINE_DLL` in place of the
  former `ISSHONI_TEST_MEDIA_DIR`.
- **Site anchors**: `/download#windows`, `#windows-powershell`, `#windows-smartscreen`, `#windows-smart-app-control`,
  `#windows-verify`, `#windows-updates`, `#windows-uninstall`, `#windows-firewall`, `#windows-webview2`,
  `#ffmpeg-source`, `/privacy#desktop`, `/security#verify-desktop`, `/troubleshooting#desktop-update-failed`,
  `#desktop-smart-app-control`, `#desktop-webview2`, `#desktop-openssh`, `#desktop-windows-n`,
  `#desktop-media-feature-pack`, `#desktop-gpu-driver`, `#desktop-fullscreen-games`, `#desktop-crash-dumps`,
  `#desktop-windows10-border`, `#desktop-linking` (all in `TestDocsAnchors`).
- **CI**: the job names `windows-native` (ci.yml and release.yml), `windows-desktop`, `windows-go`, `native-host`,
  `desktop-macos`, nightly `native-fuzz` and `windows-asan`; the artifacts `windows-native` (`release\`, `ci\`),
  `isshoni-native-tools` and `isshoni-desktop-windows-pr`; `ISSHONI_TEST_ENGINE_DLL` (= `<artifact>\ci\isshoni-media.dll`)
  for real-DLL tests; `DESKTOP_E2E_BIN` in `e2e`; the bindings and `codes.gen.ts` drift checks in `protocol`;
  `audiotest.yml` (suites `release`, `full`, `hwcheck`; input `release_run_id`) and its runner labels
  `isshoni-audio`, `win10`, `win11`; the `release.yml` workflow artifact `isshoni-exit-tools` (§11.1: the
  tag's `isshoni-audiotest.exe`, `isshoni-enginetest.exe`, `g2g.exe`, `hwcheck.ps1`, and the release `release\` DLLs
  with `native-manifest.json`), which the exit run and the release tiers use (05 §18.8, §18.10).

## 18. Depends on

**From `03-desktop-app.md`**
- `cmd/isshoni-desktop` with the build constraints of §5.2, the Wails pin, `build/windows/icon.ico` and
  `build/windows/app.manifest`; the flags and the mutex of §2.4; the updater UI (status, "Check now", channel, the
  required banner, "Restart and update" through `Ready()` + `Apply`, the `not_installed` and `install_failed` notices)
  bound to `internal/update`; applying at start before the window opens (`PendingAtStartup(autostart)`, never at an
  `--autostart` start, where the tray offers "Update to X and restart") and never during a live share; the error-code
  texts `desktop.update.errors.<code>` in `desktop.en.json` with the codes in `internal/desktop/bridge/codes` (§8.10).
- What 03 reads and writes in the registry: reads `UpdateChecks` (once, as the initial setting), `InstallDir` (via
  the updater), `PendingServer` and `PendingRoom` (read and deleted at start, validated with 02's
  `deeplink.NormalizeOrigin` and handled under 02 §12.3, §2.3); writes `PendingServer` and `PendingRoom` before a
  start-time update from a link (§8.7), the `Run` value, and, through Wails' notifications service, the toast AUMID and
  `CLSID` keys and any subkey of `HKCU\Software\isshoni` (§2.3). `Version`, `DesktopShortcut` and `WinVer` are the
  installer's only.
- Parsing the flags of §2.4 (`--open-url <url>` with 02 §12.2's one-argument rule, `--forget-credentials`,
  `--purge-data`, `--self-check`, `--version`, `--updated-from`, `--update-now` in updatetest builds, `--autostart`).
- **03 §13.4 step 2a** (new, between the shell relaunch and single instance): `OpenMutexW(SYNCHRONIZE, FALSE,
  "Local\io.isshoni.desktop.installing")`; when it succeeds, a start from a link writes the link's origin to
  `PendingServer` and its room id to `PendingRoom` (overwriting older values), and every start then exits 0 without
  single instance, the running mutex, the updater or `Apply`. Tested by an app unit test with a fake mutex probe (03)
  and by §16's smoke cases.
- The `/download` page's app button is this document's **"Open isshoni"** button (§7.5); 03 calls it that in §23,
  §24 and M-DESK-1. Its link comes from 02's `appLinkFor`, not from a 03 format.
- Two settings this document assumes, named `updates.check` (bool, default true, initial value from `UpdateChecks`)
  and `updates.channel` (`stable`|`beta`, default `stable`), passed to `update.Options`; the desktop SPA's
  stale-build reload turned off; About → Licenses opening `licenses\THIRD_PARTY_NOTICES.txt`; whether the app uses
  `web/dist` as is (assumed) or a desktop build of the SPA.
- The `@wailsio/runtime` version equal to `go.mod`'s Wails version without the `v` (§3, 03 §3.1; checked by
  `lint:pins`); the origin test against
  `protocol.DesktopOrigins()` (D19); the codec-banner text linking `/troubleshooting#desktop-windows-n` (03 §5.4) and
  the guided fixes linking `#desktop-gpu-driver` and `#desktop-media-feature-pack`; `internal/desktop/e2ehost`, the
  CDP spec and its Playwright config (03 §22.4).

**From `04-windows-engine.md`**
- At the CMake root `native/` (04 §2): `CMakePresets.json` with `win-x64-release`, `win-x64-ci`, `win-x64-asan`,
  `host-debug`, `host-tsan`, `host-fuzz` (04 §13.1); `native/vcpkg.json` (with `builtin-baseline`, feature `tests`)
  and `native/vcpkg-configuration.json`, triplet `x64-windows-static`; the cache variables `IM_FFMPEG_ROOT`,
  `IM_VERSION`, `IM_WINVER` and `IM_COMMIT` (the `.rc` version resource fed by `IM_VERSION`/`IM_WINVER`; 04 §13.1
  lists `IM_WINVER` not yet and adds it next to `IM_VERSION`, since a PE `FileVersion` needs four 16-bit numbers,
  §5.3); `IM_API` on
  every export in `native/include/isshoni_media.h`; static CRT; ctest tests in `win-x64-ci`; the DLL output
  `isshoni-media.dll` + `.pdb`; `native/.clang-format`; the fuzz corpus `native/common/tests/fuzz/corpus/`.
- The x/sys/windows loader in `internal/media/native` (04 §12: `engine_windows.go`, `session_windows.go`,
  `call_windows.go`): `native.Open(native.Options{Dir: <exe folder>})` loads `isshoni-media.dll` by full path with
  `LoadLibraryExW(…, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32)`, so FFmpeg resolves from the same
  folder and never from the PATH or the working directory; no hash check of the FFmpeg DLLs at load (D22); `Engine.Probe`
  and `Engine.BuildInfo` (with `ffmpeg.license` and `testHooks`) for `--self-check`; `ISSHONI_TEST_ENGINE_DLL` for its
  DLL tests.
- FFmpeg loaded at run time only (never import-linked), with the major/minor version check of 04 §9.2; DllMain doing no
  work on `DLL_PROCESS_DETACH` (§6.7).
- `--self-check`'s engine part works without a GPU; the minimum NVIDIA driver (nv-codec-headers choice, §4.1); the
  OpenH264 URL and checksum for the privacy page; the component checks next to M-LATENCY (04 §15; the measurement is
  01 §12.2's `tools/g2g`) and the manual checks M-WDA, M-DD, M-WGC-WIN; `isshoni-enginetest` and
  `native/windows/tools/hwcheck.ps1` (04 §14).
- Any new system DLL import added to `deploy/windows/dll-imports.txt` (the `api-ms-win-core-*` sets in slice E2), any
  native dependency to `deploy/notices/native-windows.json`; the C++/WinRT license file under
  `native/windows/third_party/cppwinrt/LICENSE` and OpenH264's under `native/windows/third_party/openh264/LICENSE`.

**From `05-windows-audio.md`**
- `cmd/isshoni-audiotest` and its fixtures, built on `windows-2025` (Go and CMake), the CLI of 05 §18.2 (`selfcheck`,
  `probe`, `run -gate [-long] -out …`, `record`, `merge`), exit codes 0–4, a report JSON with per-scenario
  verdicts, and the requirement of an interactive session with an audio endpoint; its audio system DLLs in
  `deploy/windows/dll-imports.txt`.

**From `02-device-auth.md`**
- `deviceauth.Servers.ForgetAll` (02 §11.7, slice DA10) behind `--forget-credentials` and `--purge-data`;
  `servers.json` in `%LOCALAPPDATA%\isshoni` (§2.2); `POST /api/v1/device/revoke` for the best-effort
  self-revoke; the `deviceFlow` feature in `/info` and `compat.FromInfo`'s `update_server`/`no_device_flow` (02
  §13.2); `TestCompatServerDeviceFlow` (§10.3).
- The deep-link grammar of 02 §12 (`isshoni://<host>[:port]/[r/<roomId>]`, https only), the handler command
  `--open-url "%1"` (02 §12.2), `appLinkFor(serverOrigin, roomId?)` in `web/src/lib/appLink.ts` (02 §10.2, slice
  DA7), `deeplink.Parse`, `deeplink.Build`, `deeplink.NormalizeOrigin` and the §12.3 behavior table.
- `internal/version` `Parse`, `Compare`, `Below` (02 §13.2, slice DA1) for every updater comparison.
- `protocol.DesktopOrigins()` (02 §6.4) for the Wails bump rule (D19).
- 02's troubleshooting section "Linking the app" under the fixed anchor `#desktop-linking`; 02's privacy rows on the
  server side only (R14 owns `/privacy#desktop`).

**From `01-client-core.md`**
- The client core building and testing on linux, darwin and windows with the fake engine; 01's `TestCompatServer`
  (01 §14, `internal/client/core/compat_test.go`) driven by `ISSHONI_COMPAT_SERVER_URL` and `ISSHONI_COMPAT_ADMIN_FILE`
  (§10.3); protocol N/N−1 kept by the desktop.

**From M1 (as built)**
- M1 06: `release.yml` (`version`, `web`, `build`, `sign`, `sign-dry`, `verify`, `publish`, `site`), `.goreleaser.yaml`,
  `deploy/release/check-draft.sh` and its test, `task release:{prepare,assets,snapshot,keys}`, `tools/notices`,
  `tools/repocheck keys`, `tools/relserve`, `site.yml` and `release.json`, `TestDocsAnchors`, the `release`
  environment, the `ci-ok` rule.
- M1 04: the reserved routes (M1 04 §9.2), `config.Site.Origin`, `internal/version`, the reserved env list (M1 04
  §4.2, `reservedEnv` in `internal/server/config/keys.go`), the import table (M1 04 §2).
- M1 03: `/api/v1/info` with `server.version` and `server.publicUrl`; the `minClientVersion` setting.
- M1 05: `web/src/download/` (the placeholder), the `account` lazy namespace, the Platform's `kind` and `openExternal`.

## 19. Implementation slices

In order; each is testable on its own. Days are rough, for one developer (about 22 days in all, much of it parallel to
03/04/05). "Verified" says where acceptance can be checked.

| # | Slice | Scope | Acceptance | Depends on | Verified |
|---|---|---|---|---|---|
| R1 | Cross-OS desktop CI (1 d) | `lint-go`'s Windows steps, `desktop-macos` job, the DLL-free `windows-go` job (windows-2025, `desktop` filter, no needs, 15 min, in `ci-ok`), the `native-host` job and `task native:test`, `tools/wails3` + `task release:wails3` + the extended `lint:pins` (Wails library, CLI and `@wailsio/runtime` in `web/package.json` and `web/package-lock.json`), the Dependabot ignores (gomod and npm), `changes` filters (`desktop`, `native`) | a PR touching the app: ubuntu cross-builds `isshoni.exe` (`CGO_ENABLED=0`), macOS builds the darwin app with the fake engine, client-core tests pass on both and on `windows-go` (02's DA9/DA10 and this document's R7/R8 get Windows CI from here on); `native-host` is green on 04 E1's host tests on ubuntu and macOS; `lint:pins` fails when `tools/wails3` and `go.mod` name different Wails versions, and when `@wailsio/runtime` is a range or differs from the version 03's DK1 recorded | 03's app skeleton with the build constraints; 04 E1 for `native-host` | CI ubuntu, macos, windows; Mac |
| R2 | FFmpeg LGPL build (2 d) | `native/ffmpeg/{pins.json,build.sh,ffcheck.c,README-LGPL.txt}`, `ffmpeg.yml` (build, publish, weekly), `tools/desktoppkg ffmpeg-inputs/ffmpeg-fetch`, `lock.json`; the MSVC environment step `deploy/windows/msvc-env.ps1` and the `msvc` entry of `deploy/windows/tools.json` (§3; R3a adds the other entries) | the PR build is green on `windows-2025` (VS 2026 image) with toolset 14.44, and the log and summary name the VS instance, the toolset build and the SDK; with `toolset` set to a version the image lacks, the step fails naming the toolsets it found; the zip holds the two DLLs, import libs, headers, PDBs and licenses; `ffcheck` shows LGPL 2.1+, exactly the four encoders, no gpl/nonfree/version3; dependents are system DLLs only; a second build is byte-identical (or the slice records that MSVC isn't and the check becomes a warning, §4.6); the owner's dispatch creates `deps-ffmpeg-9.0.2-r1` as a non-latest prerelease with attestations; `lock.json` merged | — | CI windows; owner dispatch |
| R3a | `windows-native` job, no FFmpeg yet (0.75 d) | lands **in the same PR as 04 E2**: vcpkg clone at `native/vcpkg.json`'s baseline + cache, the MSVC environment, presets `win-x64-ci` (build, ctest) and `win-x64-release` (build), `tools/desktoppkg dll-check` (exports, `static:`/`delay:` imports, version resource), `deploy/windows/dll-imports.txt` and the rest of `tools.json` (NSIS, WebView2, Pester, PSScriptAnalyzer; R2's `msvc-env.ps1` reused), the WebView2 bootstrapper check, the `release\`/`ci\` artifact, `task native:build`; `IM_FFMPEG_ROOT` optional until R3b; the Go load test (`ISSHONI_TEST_ENGINE_DLL`) in this job until R5 exists | green on 04's skeleton DLL (E2's acceptance: both presets with `/W4 /WX`, exports equal the `.def`, Go loads the DLL); an extra export, a `VCRUNTIME140.dll` or `avcodec-62.dll` import, a static `mfplat.dll` import or a wrong `ProductVersion` each fail the job | R1, 04 E2 (same PR) | CI windows |
| R3b | FFmpeg in `windows-native` (0.5 d) | the locked zip from the deps release as `IM_FFMPEG_ROOT`, the lock staleness check (§4.1), the FFmpeg DLL hash check, `task native:dist` and the `isshoni-native-tools` artifact | a changed `pins.json` without a new lock fails the job; the `ci\` folder carries the FFmpeg DLLs next to the DLL; the tools zip unpacks on the owner's PC (its `isshoni-enginetest probe -quick` check is 04 E7's, README WA44, since the tool comes with E7); lands **before 04 E7** | R2, R3a | CI windows; owner's PC |
| R4 | Installer (2.5 d) | `deploy/windows/nsis/*` (incl. the `.old` rename loop for every overwritten PE file, the WebView2 120.0 minimum, the toast-key cleanup, `--forget-credentials` on every uninstall, the installing mutex with exit 8 and the `/UPDATE` banner), `tools/desktoppkg {winver,stage,nsis,check}`, `task desktop:exe:windows [TAGS]`, `task desktop:package:windows`, `deploy/test/windows/smoke.ps1` | on the Mac: an installer builds with Homebrew makensis; on Windows CI: every `smoke.ps1 -Mode ci` case of §16 passes (credentials and toast keys gone in both uninstall modes, the data root only on purge; a second setup exits 8 and an app start exits 0 with the Pending values written while the installing mutex is held); `makensis -WX` clean; the strings rule holds (`Banner::show` included); on the owner's PC, `setup.exe /S /UPDATE /RELAUNCH` over an installed copy shows the "Updating isshoni to {v}…" window until the relaunch (or the slice records the §6.4 fallback) | R3b, 03's flag parsing (`--open-url`, `--forget-credentials`, `--purge-data`, `--self-check`), the running mutex and step 2a (the installing-mutex probe, 03 §13.4), 02 DA10 (`ForgetAll`) | CI windows; Mac; owner's PC (pages, interactive prompts, the banner) |
| R5 | `windows-desktop` job (0.75 d) | the job of §10.1: the DLL-dependent Go tests (`go test -race ./internal/media/...` with the `ci\` DLL), the installer and smoke, 03's CDP spec with the `TAGS=e2e` build, the PR artifact, `ci-ok` wiring; plus `protocol`'s bindings and `codes.gen.ts` drift checks and `build`/`e2e`'s `DESKTOP_E2E_BIN` | a PR changing the app runs it; 04's `TestNativeIntoPublisher` runs (not skipped) on the runner; the CDP spec passes; a hand-edited binding fails `protocol`; the artifact installs on the owner's PC | R4, 03's `e2ehost` and CDP config | CI ubuntu, windows; owner's PC |
| R6 | Desktop notices and native gate (1 d) | `tools/notices` `-native`, `deploy/notices/native-windows.json`, `deploy/notices/licenses/webview2-bootstrapper.txt`, the LGPL files in the stage, `go-licenses` on macOS | the installed `THIRD_PARTY_NOTICES.txt` lists Wails' modules, the npm packages and the native section; the §16 negative cases fail | R5 | CI ubuntu, windows, macos |
| R7 | Manifest and signature format (1.5 d) | `internal/update/{manifest,sshsig,signers}.go` with tests and fuzzing, `tools/desktoppkg manifest`, `deploy/desktop/min-version`, `deploy/keys/update_revoked`, `task release:update-keys`, `repocheck keys` extension (signers block with the `Backup` flag, exactly one backup line, ps1 block, README rows, the 8.1 option rule), the `sign` job's backup-only rule for a non-empty `revokedKeys` (lands with R13) | §16's sshsig and manifest tests pass (SemVer through `internal/version`), including: an active-key manifest revoking the backup is ignored, a backup-key manifest revoking the active key is persisted; `ssh-keygen`-made signatures verify; `lint:keys` fails on a drifted block and on an `update_signers` with zero or two backup lines | 02 DA1 (`internal/version` exported) | CI all OS; Mac |
| R8 | Updater (2.5 d) | `internal/update/{update,fetch,state,apply_windows,policy_windows,apply_other}.go` (incl. `Ready`, `PendingAtStartup(autostart)`, the `not_installed` and `install_failed` states), the `updatetest` hooks | §16's update tests pass with goleak; a release build contains no test-hook strings | R7, 02 DA1 | CI all OS; Mac |
| R9 | Updater e2e on Windows (1 d) | the e2e of §16 in `windows-desktop` and the nightly (`smoke.ps1 -Mode update`) | ci.1 → ci.2 via `--update-now`, the failure and downgrade cases; a start while the installing mutex is held leaves `attempts` at 0 and starts no installer, and the following start applies the pending ci.2 and hands the link over through `PendingServer`/`PendingRoom` | R5, R8, 03's `--update-now` and the start-time apply | CI windows |
| R10 | `install-desktop.ps1` (1.5 d) | the script (incl. the Smart App Control check and the `try`/`catch` around `Start-Process`, §7.1), Pester tests, PSScriptAnalyzer, `lint:ps`/`test:ps`, `task release:keys` and `lint:keys` for its block, `release:prepare` stamping; the TLS measurement on a clean Windows 10 22H2 (§7.1 step 2, §11.8 item 7(b)) recorded in the PR, which fixes the `ps1Command` form for R12 | Pester passes under `pwsh` and Windows PowerShell 5.1 (Smart App Control `1` → message and no download; a throwing `Start-Process` → the `isshoni:` line); on Windows CI against relserve it installs a CI build and refuses tampered files and wrong keys; the PR states the measured default `SecurityProtocol` and the chosen `ps1Command` form | R5 | CI ubuntu, windows; owner's PC (Win10 22H2 inbox OpenSSH, clean install for the TLS check) |
| R11 | Server route (0.5 d) | `deploy/embed.go`, `internal/server/httpapi/desktopscripts.go`, router and wiring lines, the reserved env names of §7.3 in `reservedEnv` (M1 04 §4.2; `ISSHONI_TEST_ENGINE_DLL`, not `ISSHONI_TEST_MEDIA_DIR`), M1 04 §2's import-table row | §16's httpapi tests; `task dev` serves a pre-filled script for `http://localhost:5173`; `/install-desktop.sh` is 404; the server logs no unknown-env warning for any §7.3 name | R10 (the template) | CI ubuntu; Mac |
| R12 | `/download` page (1 d) | `web/src/download/*` (browser-only, `openAppUrl` via `appLinkFor`), `account.download.*` texts, `download.spec` | Vitest and Playwright as in §16 (the three numbered Windows steps; `?room=` valid and invalid; the three warning lines with their anchors; `ps1Command` exactly the form R10 recorded); axe passes (M1's a11y spec covers the page) | R11, R10 (the `ps1Command` form), 02 DA7 (`appLinkFor`) | CI ubuntu; Mac |
| R13 | Release pipeline (2.5 d) | `release.yml` jobs of §11.1, `.goreleaser.yaml`, `release:assets DESKTOP=true` and the stubs, `check-draft.sh` + test, `verify-windows`, the server-rendered ps1 integration test in `windows-desktop` | the dry run is green end to end with ephemeral keys, `verify-windows` included; `check-draft-test.sh` new cases pass; `goreleaser-check` passes with stubs; the server-rendered ps1 test's cases of §16 pass, including the non-executable installer ending with the `isshoni: Windows blocked the installer (` line and no unhandled error; the dry run uploads `isshoni-exit-tools` whose `isshoni-media.dll` SHA-256 equals `native-manifest.json`'s and whose `isshoni-audiotest.exe selfcheck` passes on `verify-windows`' runner; an `audiotest.yml` dispatch with `release_run_id` = the dry run's run skips `build` and its `fetch` job succeeds (its `run` job may be cancelled when no runner is online); `check-draft-test.sh` fails a manifest with a non-empty `revokedKeys` signed by the active key and passes it signed by the backup (§11.3) | R6, R7, R10 | GitHub dry run; Mac (`test:sh`) |
| R14 | Site and docs (1 d) | §13's pages (R14 owns `/privacy#desktop` incl. 02 §9's "On the PC" facts; the troubleshooting anchors incl. `#desktop-linking` for 02's section), `site.yml` and `release.json`, `TestDocsAnchors` additions, README keys table, `deploy/keys/README.md` and `deploy/windows/runner/README.md` | the `site` job and the anchor test pass with every §17 anchor; nothing is loaded from another origin | R13 | CI ubuntu |
| R15 | Audiotest workflow (0.5 d) | `audiotest.yml` with the suites `release`, `full` and `hwcheck`, the `release_run_id` input and the `fetch` job | a dispatch on the owner's PC under Win11 and under Win10 produces a report artifact and a summary; the `fetch` job and `release_run_id` exist (exercised end to end by R13's dry run); `suite: hwcheck` uploads `hwcheck.ps1`'s bundle; the workflow has no PR trigger (a test in `repocheck` reads the file) | 05's audiotest, R3b (`isshoni-native-tools`) | owner's PC |
| R16 | Nightly additions (1 d) | `windows-full`, `native-fuzz`, `windows-asan`, `goreleaser-full` with real desktop files, `compat-servers` (with `ISSHONI_COMPAT_ADMIN_FILE` and the `no_device_flow` branch for 0.1.x), the cross-OS legs | one green nightly with every row (or `compat-servers` skipped with its note before two releases exist; against `v0.1.0` it asserts only `update_server`/`no_device_flow`) | R13, 01's compat test, 02's `TestCompatServerDeviceFlow`, 04 E1 (fuzz targets) | GitHub |
| R17 | First release with the app (1 d of agent work plus the owner's checks) | §11.8 | the exit criterion recorded: audiotest PASS on Win10 and Win11, and M-LATENCY (01 §12.2's `tools/g2g`; 04 §15, §18.4) p95 < 200 ms over 60 s in `g2g.exe`'s `gate` run, a monitor share on one monitor, on Win11 (borderless WGC) and on Win10 22H2 (DD), hardware encoder, Chrome, rc server RTT ≤ 50 ms, the `g2g` JSON files attached with GPU, driver and encoder; plus 04's M-WDA, M-DD and M-WGC-WIN on the rc installer build; `v0.2.0` published; the site serves the new `/download` | everything above, 03/04/05 done | owner's PC; GitHub |
| R18 | SignPath (**later**; owner application in M2, integration after approval, 2 d) | §12.3 application; §12.4 jobs, Authenticode checks, the ps1 fallback, `/code-signing` and README lines | a signed release: Authenticode `Valid` with SignPath Foundation's subject on the four files, FFmpeg DLLs unsigned; Smart App Control measured | R17 and SignPath's approval | GitHub; owner's PC |

## 20. Open questions for the owner

1. **Smart App Control and the unsigned FFmpeg DLLs.** Unsigned 0.x builds can't run on PCs with Smart App Control on;
   that is expected. After SignPath signs `isshoni.exe` and `isshoni-media.dll`, Smart App Control may still refuse to
   load the FFmpeg DLLs, which the plan ships unsigned. If the measurement in §12.4 shows that, should the project ask
   SignPath to sign the FFmpeg DLLs that its own CI builds from unmodified source (which changes the plan's "FFmpeg
   DLLs ship unsigned"), or accept that those PCs fall back to the browser sharer?
