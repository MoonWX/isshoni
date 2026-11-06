# M3 design 01: The desktop app on macOS (`cmd/isshoni-desktop`, `internal/desktop`, the SPA)

The same Wails app that M2 ships for Windows, made a real macOS app: the darwin OS layer
(`internal/desktop/macos`), the start order and process contract on macOS (LaunchServices, single instance, the
`isshoni://` scheme, one-shot modes), the window, Dock and menu bar extra, the WKWebView security settings and their
checklist, the TCC permission UX (the wizard's macOS step, denied states, deep links into System Settings), the share
flow with SCContentSharingPicker (own-window exclusion, auto-minimize), hotkeys, notifications, autostart, the
Keychain credential store, settings, logs, and every place the SPA branches on the OS.

Inputs: `docs/PLAN.md` (owner-approved: "Decisions", "User journeys", "Desktop clients", "Engines per OS" (macOS
column), "Audio exclusion rules" with "Verified on macOS 27", "Share safety", "Privacy and trust model", "Install and
distribution" (macOS), "Verification", "Main risks", and the M3 roadmap entry), the M2 record (`docs/m2/README.md`
§3–§4 and the six M2 specs, mainly `03-desktop-app.md`, which this doc extends section by section and calls "03"),
the code on `main` (`internal/desktop/*`, `cmd/isshoni-desktop`, `internal/client/credstore`, `internal/protocol`,
`internal/media`, `web/src/platform/desktop`, `web/src/share/native`, `web/src/desktop`), Wails `v3.0.0-beta.34`'s
darwin sources, and the M0 spikes `spikes/s2-mac-audio` (process tap, responsible PID, self-signed identity, Local
Network check) and `spikes/s2-mac-vt`.

The sibling M3 docs are written in parallel; this doc names them by file and states the interface it assumes (§21):
- `02-macos-engine.md`: the cgo + Objective-C engine behind the C ABI v1 (SCContentSharingPicker, SCStream,
  VideoToolbox, Opus), its Go binding (`internal/media/native`, darwin), the pick flags and events this doc reads;
- `03-macos-audio.md`: the process tap, exclusion rules, mic detection, the sound check, the System Audio Recording
  permission request and its 30 s watchdog, `isshoni-audiotest` on macOS;
- `04-macos-release.md`: the self-signed identity, bundle assembly and signing (it consumes this doc's Info.plist keys,
  entitlements and icons), DMG, `install-desktop.sh`, the Homebrew tap, the updater's swap and relaunch, release CI;
- `05-exit-and-verification.md`: the exit runs on macOS 14.4 through 27, the owner's Macs and VMs, the run book.

**Tags.** Everything here is **M3** unless marked *later (M4)*, *later (M5)* or *pending* (the paid Apple program). A
"later" item gets only the room it needs now. Section numbers "03 §n" are `docs/m2/03-desktop-app.md`'s.

---

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **One app, one SPA, the same Wails pin** (`v3.0.0-beta.34`, 03 D1). macOS-only behavior lives in a new package **`internal/desktop/macos`** (cgo + Objective-C, `//go:build darwin`), the darwin counterpart of `winos`, implementing the existing `app.OSHooks` plus small optional interfaces found by type assertion (03 §6.1's pattern). Wails stays imported only by `cmd/isshoni-desktop` (03 D2) | The app logic is already OS-neutral and tested on every OS; M3 adds an OS layer, not a second app |
| D2 | **Universal binary (arm64 + x86_64), macOS 14.4 minimum.** `desktop-macos` (runner from `deploy/macos/tools.json`, `macos-26`, with 04's pinned Xcode) builds and tests the untagged and `e2e`/`dev` darwin code; the universal `production,imstatic` builds run on `macos-26` too (02's `macos-native` link check and 04's release job), because the engine archive needs the macOS 26 SDK (03's `CATapDescription.bundleIDs`). The built arm64 slice runs on `macos-15` and the x86_64 slice on `macos-15-intel` in 04's `macos-runtime` job (PRs) and `macos-runtime-full` (nightly); nightly also runs the x86_64 tests of `internal/desktop/macos`, `credstore` and `cmd/isshoni-desktop` with `GOARCH=amd64 CC='clang -arch x86_64'` under `arch -x86_64` on the arm64 runner (02's approach) | The plan keeps the Intel slice ("one universal binary … Intel slice kept"); 14.4 is the plan's floor and the first release with `SCShareableContent.getCurrentProcessShareableContent`. Building only on `macos-26` avoids an older SDK on the Intel image; the Intel image only runs what was built. When GitHub retires it, the x86_64 slice is built but only Rosetta- and community-tested |
| D3 | **The asset origin is `wails://localhost`**, already `protocol.DesktopOrigins()[1]` (02 §6.4); M3 adds the constant `protocol.DesktopAssetOriginMacOS`. **No server change** unless WM1 (§3.2) finds that WebKit sends another `Origin` on cross-origin requests from a custom-scheme page | The server has accepted this origin, bearer-only, since M2 (02 decision 9) |
| D4 | **`LSUIElement` = true and the activation policy follows the window**: `.regular` (Dock icon, app menu, ⌘-Tab) while the main window is shown **or a share is live, paused, reconnecting or unreachable**, `.accessory` while it is hidden in the menu bar with no share, or never opened (`--autostart`) | An `--autostart` start must not flash a Dock icon; a sharer must always have a visible way back (the menu bar item can be hidden by the notch or the user, and macOS's own stop item exists only on 15+), and a Dock click reopens the window |
| D5 | **The menu bar extra replaces the tray** with 03 §11.1's view model unchanged (`internal/desktop/tray`): a monochrome template icon when idle, **red** while live, amber paused, yellow warn, and the watcher count as the item's title while live | PLAN "Share safety": the tray turns red, "Sharing <source> · N watching" |
| D6 | **No Sharing pill on macOS.** macOS shows its own screen-capture indicator for every SCStream; isshoni adds the red menu bar extra with its count, the hotkeys and notifications | The pill exists on Windows only because a borderless capture shows nothing (03 §11.4). A floating isshoni panel would itself be captured on 15+ (SCK ignores `sharingType`) |
| D7 | **Own windows**: every isshoni window gets `sharingType = NSWindowSharingNone` (effective for SCK on 14.x); the engine excludes isshoni from the `SCContentFilter` where the OS lets it, choosing its strategy per OS version from EM9's measurements (02 §8.8). **The app never decides per OS itself: it reads 02's flags.** When the pick carries `IM_SRCF_SELF_VISIBLE` (`Source.SelfVisible`) or the started session reports `IM_SESS_SELF_VISIBLE` (`EventLayer.SelfVisible`), **the app minimizes its window and mutes in-app viewer audio until the share ends** (the plan's rule): for a pick flag, after the first-share sheet is confirmed and **before** `Start`, so the first frames never carry isshoni's window; for the session flag, when the share goes live (or the flag turns true while live). A pick of an isshoni window is refused (as 03 §10.1) | PLAN Engines table, macOS "Own windows" row; one per-OS table (02's) instead of two that could disagree |
| D8 | **Global hotkeys with Carbon `RegisterEventHotKey`**, registered only while live (03 D9); defaults **⌃⌥⌘P** (Pause/Resume) and **⌃⌥⌘S** (Stop). No Accessibility or Input Monitoring permission is needed | `RegisterEventHotKey` needs no TCC grant (unlike `NSEvent` global monitors or event taps); ⌃⌥⌘ + letter never types a character on any layout, and F-keys on Mac keyboards are media keys unless fn is held |
| D9 | **Single instance: an `flock` on `instance.lock` in the data root plus a per-run Unix socket** in the per-user temporary folder, whose path the lock file holds (03 §13.3's design, POSIX calls). LaunchServices already routes Finder, Dock, `open` and URL launches to the running instance | Wails' darwin single instance posts on `NSDistributedNotificationCenter`, which any process can post to and which can't be ordered against 03 §13.4's steps (W6's reason) |
| D10 | **LaunchServices relaunch replaces Windows' shell relaunch** (03 D16): a start whose responsible process isn't itself (launched from a terminal or another app's child) relaunches through `NSWorkspace` and exits | TCC attributes grants to the responsible app, and 03-macos-audio keeps isshoni's WKWebView out by responsible PID (S2: "Running s2 from a terminal would attribute it to the terminal app") |
| D11 | **`isshoni://` arrives as a `kAEGetURL` Apple event** (Info.plist `CFBundleURLTypes`; Wails' `ApplicationLaunchedWithUrl`), and becomes the same `--open-url <url>` command line 03 §13.1 parses; a link never starts a share | One grammar and one handler (02 §12, 03 §13.2) |
| D12 | **Autostart: `SMAppService.mainAppService`** (13.0+), "Open at Login"; a login-item launch is detected from the launch Apple event (`keyAELaunchedAsLogInItem`) and behaves like `--autostart` | The plan names SMAppService; `mainAppService` takes no arguments, the Apple event is the documented way to tell |
| D13 | **Notifications through Wails' notifications service (UNUserNotificationCenter)** with the bundle id; authorization is asked in the wizard's macOS step, never at a share; denied notifications fall back to the in-window notice and the menu bar | UNUserNotificationCenter needs a bundle and asks the user once |
| D14 | **TCC**: System Audio Recording is requested in the wizard (PLAN "First-run wizard" step 2); Screen Recording is never requested (the picker needs none); Local Network (15+) is handled where the first LAN connection fails, with a guided fix; Settings gains a **Permissions** section with deep links | PLAN; S2 measured both prompts and that the grants survive self-signed rebuilds |
| D15 | **Refresh tokens in the macOS Keychain** (login keychain, generic password, service = bundle id, account `isshoni:v1:<leaf>:<origin>` where `<leaf>` is the first 8 bytes, hex, of the SHA-256 of the running code's signing leaf certificate), with **user interaction never allowed**; a CI check re-signs build N as N+1 and asserts the item is still read without a prompt, and that a build signed by another identity links again cleanly (**KC**, §13.3). A change of signing identity (a leaked key's rotation, the move to Developer ID) therefore means one new link per server, never a Keychain prompt and never a `save_failed` loop. If KC fails, the darwin store falls back to 0600 `credentials.json` in the data root, the same same-user threat model as Windows' Credential Manager, and the owner is told | 02 §11.5's plan; a keychain prompt after each update would be worse than the file |
| D16 | **Data root `~/Library/Application Support/isshoni`**, found with `NSFileManager` (never `$HOME`, as 03 §14.1 never reads the environment); logs, settings, servers, `instance.lock`, `pending.json` and 04's `updates/` all under it. WebKit's own stores are listed for `--purge-data` | One root keeps 03's purge rule; Console users find logs through "Open logs folder" |
| D17 | **Hardened runtime with one entitlement, `com.apple.security.device.audio-input`**; no sandbox; **no `NSMicrophoneUsageDescription`** | PLAN "Install and distribution"; isshoni never opens a microphone, and without the key any accidental mic access is refused by TCC instead of prompting |
| D18 | **Navigation lock on WKWebView**: layers 1 and 2 of 03 §5.5 unchanged, and layer 3 reads the WKWebView's URL **natively** after every committed navigation and loads the start page when it is off the asset origin. New windows are impossible (Wails implements no `createWebViewWithConfiguration`) | The URL comes from WebKit, not page script, so layer 3 is stronger than Windows' `postMessage` check |
| D19 | **Keep-awake = an IOKit power assertion plus an `NSProcessInfo` activity** while live | The display must not sleep, and App Nap must not throttle a hidden app that is sharing |
| D20 | **One `DesktopPlatform`**: the SPA branches only on `desktop.app.os`, and OS-specific texts use i18next's `context` (`key_macos` falling back to `key`); §16 lists every branch | Keeps the Windows SPA byte-for-byte the same in its snapshots |
| D21 | **Testing**: everything without a TCC grant runs on GitHub's macOS runners (`desktop-macos` on `macos-26`: unit tests, the KC re-sign check, the Go-side WKWebView checklist rows; 05's WK spec drives the real e2e app through 01's `e2edriver`); tests that need a grant carry the `isshoni_hw` tag, call 05's `macenv.Require`, and run on the owner's Mac inside 05's signed Dev Harness through `task mac:hw:desktop`; the desktop-in-Chrome harness gains `-os macos` | GitHub's macOS runners have no TCC grants for screen or audio capture; 05 V-D5–V-D7 |
| D22 | **Windows behavior unchanged**: every shared change is additive or keyed on the OS; the M2 Windows jobs (`windows-go`, `windows-desktop` with its CDP spec, `e2e`) are the gate | §18 |

---

## 2. Scope

**M3 (this doc)**: `internal/desktop/macos`; the darwin files of `cmd/isshoni-desktop` (start order, shell options,
menus, icons, Info.plist content, entitlements, notifications, flavors); `internal/client/credstore`'s Keychain store;
the shared changes in `internal/desktop/{app,bridge,hotkey,settings,share,i18n,applog,e2ehost}` and
`internal/protocol/desktop.go`; the SPA's macOS branches and new pages (the wizard's permission step, Settings →
Permissions); the WKWebView checklist; tests.

**Not in this doc**: capture, encode and the picker's internals (02-macos-engine), the tap, rules, mic detection,
permission request internals and the sound check's capture path (03-macos-audio), bundle assembly, signing, DMG,
`install-desktop.sh`, Homebrew, the updater's swap and release CI (04-macos-release), the exit run (05).

**Later**: Developer ID signing and notarization (*pending*, a certificate change, PLAN), a Sharing pill if users ask
for one (*later (M5)*), desktop "X started sharing" notifications (*later (M5)*, 03 D15), Linux (M4).

---

## 3. Wails on macOS: the pin and the WKWebView checklist

### 3.1 The pin

Unchanged: `v3.0.0-beta.34` (03 §3.1). A bump re-runs 03 §3.2 (W1–W21) **and** §3.2 below (WM1–WM26); a row that
fails on either OS takes its fallback. Relevant facts of the pinned darwin backend (read in its sources):
- the asset server is a `WKURLSchemeHandler` for `wails://` (origin `wails://localhost`);
- `application_darwin_delegate.m` registers a `kAEGetURL` handler (`StartCustomProtocolHandler`) that emits
  `events.Common.ApplicationLaunchedWithUrl` with the URL;
- the window's `WKUIDelegate` implements only the file panel and `requestMediaCapturePermissionForOrigin` (the
  window's `Permissions` decide it); there is no `createWebViewWithConfiguration`, so no page can open a window;
- `webViewWebContentProcessDidTerminate` reloads the page itself and emits `WebViewWebContentProcessDidTerminate`;
- `MacWebviewPreferences` has `TabFocusesLinks`, `FullscreenEnabled`, `AllowsBackForwardNavigationGestures`,
  `AllowsMagnification`, `JavaScriptCanOpenWindowsAutomatically`, `EnableAutoplayWithoutUserAction`,
  `ApplicationNameForUserAgent`; `MacWindow.EnableFraudulentWebsiteWarnings`;
- `SystemTray` on darwin has `SetTemplateIcon`, `SetIcon`, `SetLabel`, `SetTooltip`, `SetMenu`;
- `Options.ShouldQuit func() bool` is asked on ⌘Q, the Dock's Quit and logout; `Mac.ActivationPolicy` sets the
  start policy;
- Wails' own darwin single instance, autostart and global-shortcut code exist but are not used (D9, D12, D8: the app's
  own code in `internal/desktop/macos` is testable without Wails and follows 03's rules).

### 3.2 The WKWebView checklist (WM rows)

Each row is a test or an exact manual step. "CI" means `desktop-macos` (runner from `deploy/macos/tools.json`,
`macos-26`) against an e2e-flavor bundle (§19.2), in one of two ways: the Go-side rows (WM4, WM10, WM11, WM12 and
WM19's env scrub) run in `go test -tags isshoni_hw -run TestWailsChecklist ./cmd/isshoni-desktop` (needs only 05's
`gui`), and every page-side row runs in 05's WK spec, which drives the real app through 01's in-app driver
`e2edriver_darwin.{go,m}` (§4.5, §19.2; key rows WM9 and WM22 use its `POST /v1/key`). GitHub's macOS runners have a
logged-in GUI session, and none of these rows needs a TCC grant. 05's `macos-wk` runs the WK spec on `macos-15` as
well as `macos-26`, so WebKit on 15 (no Navigation API, WM5) is covered in CI. "Mac" means the owner's
Mac (macOS 27) and, for rows marked "all", the VMs of 05 (14.4, 15, 26). The slice that runs a row records the result
under "Wails checklist (macOS)" in its PR and in this table.

| # | Behavior on macOS | How it is checked | Fallback |
|---|---|---|---|
| WM1 (W1) | `location.origin` is `wails://localhost`; `protocol.DesktopAssetOriginMacOS` equals `DesktopOrigins()[1]`; **the server receives `Origin: wails://localhost`** on a cross-origin `fetch` and on the `/ws` handshake | CI: the checklist page fetches the checklist's own httptest server, which records the header; `TestDesktopOrigins` | If WebKit sends `Origin: null`: a server change in 02's `internal/protocol/desktop.go` (accept `null` exactly as the desktop origins are accepted: CORS and `/ws` only with a bearer token, never cookies), shipped one server release before an app depends on it (02 §6.4's rule). The token, not the origin, is the identity (02 decision 9), so this widens nothing a script without a token can do |
| WM2 (W2) | `window.isSecureContext` on `wails://localhost` | CI | `DesktopPlatform` already skips the check (03 §7.2). The SPA's secure-context APIs are `crypto.randomUUID` (replaced by `crypto.getRandomValues` in `platform/desktop` if false) and `navigator.clipboard.writeText` (Copy link; if false, a bound `App.CopyText(text)` ≤ 4 KiB over `NSPasteboard`) |
| WM3 (W3) | The CSP **header** from the scheme handler is enforced, including `sandbox allow-scripts allow-same-origin allow-forms` | CI: an injected inline script doesn't run; `window.open` from a fresh iframe returns `null` | The `<meta>` CSP (03 §5.3; `sandbox` isn't allowed in `<meta>`, and new windows are impossible anyway, D18) |
| WM4 (W4) | `AssetOptions.Middleware` sees `/wails/*`; WKWebView sends neither `Origin` nor `Sec-Fetch-Site` to its scheme handler (measured on the dev build at beta.34) | CI: `TestGuard*` and the checklist's foreign-origin case through `assets.GuardWKWebView` | 03 §5.6's exception stays macOS-only |
| WM5 (W5) | A script's `location.href = 'https://example.com'` doesn't leave; `window.open` returns `null` and opens nothing; `<a target=_blank>` opens nothing in the webview (layer 1 sends it to the browser) | CI: the page stays on `wails://localhost`, or is back within 2 s through layer 3 (§8.3); no second `NSWindow`; **on `macos-15` (05's `macos-wk` leg of the WK spec) after a forced foreign navigation (`e2edriver`'s `/v1/navigate`) a stub `window._wails.dispatchWailsEvent` installed by the foreign test page records no `isshoni:*` event, and the window is back on `wails://localhost` within 2 s** | Layer 3 is the fallback where WebKit lacks the Navigation API (WebKit before Safari 26.2: macOS 14.4 and 15 without a Safari update); it acts at provisional start, and the event gate of §8.3 keeps `isshoni:*` events away from a foreign page that commits anyway |
| WM6 (W6) | Single instance | Not Wails': `macos` tests (§6.2) | — |
| WM7 (W7) | Close (red button, ⌘W) is turned into hide; the page reports `visibilityState` `hidden`; the process lives; **viewer audio keeps playing while hidden** | CI for the hide; Mac for audio (WM23) | Wails' window option to hide on close, plus our Quit path |
| WM8 (W8) | Notifications with action buttons from a dev-signed bundle; an action after a restart reaches the new run; authorization survives an N → N+1 re-sign | Mac (needs the user's "Allow") | Notifications without buttons; the in-window notice |
| WM9 (W9) | Production build: no Web Inspector (no "Inspect Element", `isInspectable` false, not listed in Safari's Develop menu), no default context menu outside text fields, no pinch zoom (`AllowsMagnification` false), no swipe navigation, ⌘R and ⌘+/⌘− do nothing (no View menu, §7.3), a dropped file or link doesn't navigate | CI where observable (key events through `e2edriver`'s `POST /v1/key`, which posts `NSEvent`s inside the app's own process and needs no grant; 05's WK9); Mac for the trackpad (05's M-MAC-19) | The SPA-level mitigations of 03 §5.4 |
| WM10 (W10) | The main window's `NSWindow*` is available (`NativeWindow()`) | CI | Find the window in `NSApp.windows` by its title on the main thread |
| WM11, WM12 | Binding context and error rules (OS-independent; hold without a window) | CI | 03's |
| WM13 (W13) | `<audio>.play()` and a new `AudioContext` run without a gesture (`EnableAutoplayWithoutUserAction`) | CI | The M1 "tap to unmute" |
| WM14 (W14) | ⌘Q, the Dock menu's Quit and the menu bar extra's Quit run the shutdown hook (share stopped, close 1000) | CI for ⌘Q and the menu item; Mac for logout (§7.5) | Our own menu items call `App.Quit` first |
| WM15 (W15) | Bindings generated on macOS equal the committed ones; the e2e-only `E2E.Report` service (§20) is excluded from generation, so it never changes them | already holds (03 W15); the generator runs without the `e2e` tag | — |
| WM16 (W16) | Element fullscreen fills the screen (`FullscreenEnabled` true) and Esc leaves it | Mac (a real Esc) | CSS pseudo-fullscreen |
| WM17 (W17) | A WebContent crash (`kill` of the WebContent process) is followed by Wails' reload, and the heartbeat comes back | CI (the test kills the WebContent process it finds by responsible PID) | 03's watchdog |
| WM18 (W18) | The status item's icon, template flag, title, tooltip and menu change at runtime | CI (accessibility API of its own process); Mac by eye | Rebuild the item on every change |
| WM19 (W19) | Production builds can't be inspected from outside: `isInspectable` false whatever the environment; the env scrub (§6.1) removes `FRONTEND_DEVSERVER_URL`, `WAILS_UPDATER_HELPER*` and every `WEBKIT_*` | CI: start the production bundle with `WEBKIT_X=1` and assert it's gone from the process's own environment | Record |
| WM20 (W20) | `go-licenses` and `govulncheck` pass for the darwin app (cgo) | CI (`desktop-macos` already runs go-licenses natively) | 03's |
| WM21 (W21) | The runtime's own objects get 403 from the real webview: `Browser.OpenURL('x-apple.systempreferences:com.apple.preference.security')`, `Clipboard.Text()`, `Window.Hide()`; a bound method answers | CI | 03's |
| WM22 | ⌘C, ⌘V, ⌘X, ⌘A, ⌘Z work in the SPA's text fields (Edit menu, §7.3); ⌘H hides; ⌘M minimizes | CI (`e2edriver`'s `POST /v1/key`, 05's WK spec) | — |
| WM23 | WebRTC in WKWebView: an H.264 High share decodes (`framesDecoded > 0`); audio plays while the window is hidden in the menu bar and while minimized; video subscriptions turn off after 10 s hidden (M1 rule) | Mac, all | Record; if hidden playback stops, keep the window minimized instead of hidden while the user watches an audible share |
| WM24 | Every System Settings URL of §9.4 opens the named pane | Mac, all | The fixed fallback per row of §9.4 |
| WM25 | A login-item launch is recognized (§12.2) | Mac: log out and in | `SMAppService.agent(plistName:)` with `--autostart` (§12.3) |
| WM26 | An `isshoni://` link that starts the app reaches `Boot` (the add step opens), and one that arrives while the app runs reaches `HandleArgs` | Mac (Safari, Chrome and `open isshoni://…`), and CI with a production-flavor bundle (`open` with a test URL) | — |

---

## 4. Processes, threads and the bundle

### 4.1 Architecture

```
 isshoni.app/Contents/MacOS/isshoni (Go + cgo, universal; one per user; launched through LaunchServices)
 ├─ cmd/isshoni-desktop: start order (§6) ── Wails v3 darwin: NSApplication, main window, NSStatusItem, menus
 │     └─ WKWebView at wails://localhost: the bundled SPA
 │           │ bindings + events                         │ REST: Bearer (CORS) · WSS /ws: hello.auth
 │           ▼                                           ▼
 ├─ internal/desktop/bridge ─► internal/desktop/app ── C1: SPA viewer connection (WebKit Networking) ─► server
 │                               ├─ session: C2 publisher connection (01's core) ────────────────────► (same Participant)
 │                               ├─ share controller ─► media.Engine (02: SCK picker, VideoToolbox, tap via 03)
 │                               ├─ audio.Service (05, with 03-macos-audio's engine)
 │                               ├─ deviceauth.Servers (02) over credstore's Keychain store (§13)
 │                               ├─ internal/desktop/macos: main-thread bridge, hotkeys, system events, keep-awake,
 │                               │     window sharing type, activation policy, SMAppService, settings pages, TCC helpers
 │                               └─ settings · logs · i18n · update.Updater (06 / 04)
 └─ WebKit XPC services (responsible process = isshoni): com.apple.WebKit.WebContent, .GPU (plays viewer audio),
    .Networking
```

### 4.2 Processes

| Process | Started by | Notes |
|---|---|---|
| `isshoni` | LaunchServices (Finder, Dock, Launchpad, `open`, a URL, the login item, 04's installer and updater) | §6.1 step 2 makes sure its responsible process is itself. There is no `--sound-check-helper` child on macOS: the engine plays the sound check's chime itself (03-macos-audio §13.3; step 0 stays for Windows) |
| `com.apple.WebKit.WebContent`, `.GPU`, `.Networking` | WebKit, for the main window | Their responsible PID is isshoni's (S2 finding 5): 03-macos-audio keeps the GPU process's audio out of every share by that, and TCC attributes their network and capture use to isshoni |
| `isshoni` in a one-shot mode (§6.4) | 04's installer and uninstaller, 04's `update.Apply` (`--apply-update`), tests | No window, no single instance |
| a second `isshoni` | a terminal, `open -n`, a test | Forwards (§6.2) and exits within 2 s |

### 4.3 Threads

- **The main thread** is AppKit's: Wails runs `[NSApp run]` on it. Everything that touches `NSWindow`, `NSApp`,
  `NSStatusItem`, Carbon hotkeys, `NSWorkspace` notifications and `SMAppService` runs there.
- **`macos.OnMain(func())`** is the one way Go code reaches it: `dispatch_async(dispatch_get_main_queue(), …)` with a
  `cgo.Handle`, and `macos.OnMainWait(ctx, func() error) error` waits for the result with a 2 s bound. Calling
  `OnMainWait` on the main thread runs the function inline (`pthread_main_np()`), so no deadlock is possible.
- **Callbacks from AppKit to Go** (a hotkey press, a system event, a menu action) never block: the Objective-C side
  calls one exported Go function per kind (`isshoniMacEvent(kind C.int, arg C.int64_t)`), which only does a
  non-blocking send on a buffered channel (64; a full channel drops with a rate-limited WARN). Native code never calls
  Go from a real-time thread (01 D5; the engine's threads are 02's and never call Go).
- Go code never holds a lock while it waits on the main thread.

### 4.4 The bundle (template, entitlements and icon owned by 04-macos-release; run-time values required here)

```
isshoni.app/Contents/
├─ Info.plist                  04's cmd/isshoni-desktop/build/darwin/Info.plist.tmpl, filled by tools/desktoppkg
├─ MacOS/isshoni               the universal binary (04 lipos the two slices)
├─ Resources/AppIcon.icns      04's cmd/isshoni-desktop/build/darwin/AppIcon.icns (CFBundleIconFile AppIcon)
├─ Resources/en.lproj/InfoPlist.strings   04 (the usage strings, ready for translation)
├─ Resources/licenses/THIRD_PARTY_NOTICES.txt   (04; App.OpenNotices opens it, §14.4)
└─ _CodeSignature/             (04)
```

**04 is the single owner** of `Info.plist.tmpl` (its `@BUNDLE_ID@`, `@BUNDLE_NAME@`, `@SHORT_VERSION@`,
`@BUNDLE_VERSION@`, `@VERSION@`, `@COMMIT@`, `@URL_SCHEME@` placeholders), the entitlements files, `AppIcon.icns` and
the bundle golden that `tools/desktoppkg bundle` checks (04 §4.1, §4.2, §5.5 check 4). This doc does not keep its own
copy or test of them. It owns only the **values its run-time behavior depends on**, stated here as requirements on
04's template:

| Key | Required value | Why (this doc) |
|---|---|---|
| `CFBundleIdentifier` | `io.isshoni.desktop` (production); `io.isshoni.desktop.dev`, `io.isshoni.desktop.e2e` for the dev and e2e bundles | dev and e2e bundles get their own TCC grants, notification settings and Keychain service, never the user's (§13, §14.1) |
| `LSUIElement` | `true` | D4: no Dock icon until a window shows (an `--autostart` or login-item start must not flash one); the activation policy follows the window and the share at run time (§7.2). 04 §4.1 currently lists it as not set; this row asks 04 to add it |
| `NSQuitAlwaysKeepsWindows` | `false` | no window restoration; the app opens its own window (§7.1) |
| `CFBundleURLTypes` | **production only**: the dev and e2e variants declare no `CFBundleURLTypes` at all (04's template drops the array for them instead of filling `@URL_SCHEME@` with another scheme) | D11, §6.3: a developer's build never takes the installed app's links |
| `NSAppTransportSecurity` → `NSAllowsLocalNetworking` = true | dev and e2e variants only | their `http` loopback test servers (§19.2); production sets no ATS key |
| `LSMinimumSystemVersion` | `14.4` | D2 |
| `NSBonjourServices` | not set (§9.5: M-MAC-16/MD-M9 checks it; MD11 asks 04 to add `[_isshoni._udp]` only if a LAN viewer fails) | |

The usage strings are 04 §4.1's, cited verbatim: `NSAudioCaptureUsageDescription` "isshoni shares the sound of your
Mac with friends while you share your screen, minus voice apps." (the System Audio Recording prompt, 03-macos-audio;
S2), `NSScreenCaptureUsageDescription` "isshoni shows friends the window or screen you pick, only while you share."
and `NSLocalNetworkUsageDescription` "isshoni connects to isshoni servers and friends on your local network." (the
15+ Local Network prompt). Deliberately absent (D17): `NSMicrophoneUsageDescription`, `NSCameraUsageDescription`,
`NSAppleEventsUsageDescription` (the app sends no Apple events to other apps).

**Entitlements**: 04's `isshoni.entitlements` (exactly `com.apple.security.device.audio-input`, hardened runtime, no
sandbox; D17). The app's code assumes nothing else.

**Menu bar images** (this doc's): `build/darwin/tray/` (18 × 18 pt at 1x and 2x PNG): `tray-idle.png` (black with
alpha, used as a template image), `tray-live.png` (red), `tray-paused.png` (amber), `tray-warn.png` (yellow with a
dark mark, readable on light and dark menu bars). `shell_darwin.go` embeds them (`trayIcon(id)`'s darwin half); they
are not bundle resources.

### 4.5 Repository layout and import rules

```
cmd/isshoni-desktop/
├─ platform_darwin.go     (changed) data root from macos.DataRoot, or <ISSHONI_E2E_DATA_DIR>/isshoni in e2e builds
│                         (§14.1), OS facts, the macOS shell options, credentials
├─ start_darwin.go        (new) step 2 (LaunchServices relaunch), step 2a (applying.lock), --wait-pid, the URL Apple
│                         events into fwd, SIGTERM = Quit(true) (§7.5)
├─ shell_darwin.go        (new) menus (§7.3), activation policy, tray icons, navigation layer 3 (§8.3)
├─ notify_darwin.go       (replaced) Wails' notifications service on darwin (§11)
├─ firstaction_darwin.go  (unchanged)
├─ oneshot_darwin.go      (new) --self-check's macOS JSON, --purge-data's extra paths, --apply-update dispatch (§6.4)
├─ engine_fake.go, engine_release.go   (constraints changed, §6.4); engine_noengine_darwin.go (new, §6.4)
├─ e2edriver_darwin.go/.m (new; //go:build darwin && cgo && e2e) 05 §6.2's in-app loopback driver and the e2e-only
│                         bound service E2E.Report (§19.2, §20)
├─ menu_test.go           (new) TestMainMenuByFlavor (§7.3)
├─ checklist_darwin_test.go (extended) the Go-side WM rows
└─ build/darwin/          tray/*.png (this doc's); Info.plist.tmpl, entitlements, AppIcon.icns are 04's (§4.4)
internal/desktop/macos/   (new; //go:build darwin for the cgo files, *_other.go stubs so it builds everywhere)
├─ doc.go, macos.go       Hooks (app.OSHooks), New, Close
├─ main_darwin.go/.m      OnMain, OnMainWait, the event callback
├─ paths_darwin.go        DataRoot, TempDir, BundleID, BundlePath, InBundle, OSVersion, ComputerName, WebKitVersion
├─ hotkeys_darwin.go/.m   Carbon hotkeys, typesChar (UCKeyTranslate)
├─ events_darwin.go/.m    lock, unlock, sleep, wake, power-off, screens, session switch
├─ awake_darwin.go        IOPMAssertion + NSProcessInfo activity
├─ window_darwin.go/.m    sharing type, miniaturize, activation policy, WKWebView URL read
├─ autostart_darwin.go    SMAppService, launched-as-login-item
├─ pages.go, pages_darwin.go   the System Settings URL table and NSWorkspace open
├─ singleinstance.go, singleinstance_darwin.go   flock + Unix socket (§6.2)
├─ relaunch_darwin.go/.m  responsible PID, NSWorkspace relaunch, wait for a PID
├─ pending.go             pending.json (app.PendingStore)
├─ localnet.go            Local Network failure classification (pure; §9.5)
├─ tcc_darwin.go          notification authorization state (UNUserNotificationCenter) for Permissions
└─ *_other.go             stubs: every function answers app.ErrNotSupported or a zero value
internal/client/credstore/
├─ keychain_darwin.go/.m  (new) the Keychain store (§13)
web/src/desktop/AudioPermissionStep.tsx, PermissionsSettings.tsx (new)
web/src/platform/desktop/os.ts (new): desktopOS(), the i18n context
```

depguard (`.golangci.yml`, added): `internal/desktop/macos` may import `internal/desktop/app`,
`internal/desktop/hotkey`, the standard library and `golang.org/x/sys/unix`; nothing else in `internal/desktop` imports
`macos` (only `cmd/isshoni-desktop` does, like `winos`). `winos` keeps its `*_other.go` stubs, and `cmd/isshoni-desktop`
picks `winos.New()` or `macos.New()` in `platform_{windows,darwin}.go` (`newOSHooks()`), so `main.go` names neither. The
Objective-C files gate every newer API with `@available`. Their flags come only from `#cgo darwin CFLAGS:` and `#cgo
darwin LDFLAGS:` directives in the `internal/desktop/macos` and `internal/client/credstore` source files, never from the
environment (04 §4.6 sets `CGO_CFLAGS`/`CGO_LDFLAGS` from 02's `cgo.env`, which would lose or collide with anything
passed that way): `CFLAGS: -fobjc-arc -Wall -Werror`; `LDFLAGS: -framework AppKit -framework Carbon -framework IOKit
-framework ServiceManagement -framework UserNotifications -framework SystemConfiguration` (`macos`) and `-framework
Security -framework LocalAuthentication` (`credstore`). The deployment target comes from 04's build
(`MACOSX_DEPLOYMENT_TARGET=14.4`), not from a directive.

**Symbol prefixes.** `internal/desktop/macos`, `credstore`'s Keychain `.m` and 02's static engine archive link into
one Mach-O, so two Objective-C classes with the same name would fail the link or misbehave at run time. Objective-C
classes and non-static C functions in `internal/desktop/macos` and `credstore` use the prefix **`ISM`** /
**`ism_`** (e.g. `ISMEventObserver`, `ism_on_main`); 02 uses `IM` / `im::mac` with the Objective-C class prefix
`IMMac` (02 §6.2). File-local helpers are `static`. `desktop-macos` adds a link-time check: `nm -m` on the built
app's binary lists no `_OBJC_CLASS_$_` name twice (Wails' own classes included).

---

## 5. The darwin OS layer (`internal/desktop/macos`)

### 5.1 Shared changes in `internal/desktop/app` (additive)

```go
// oshooks.go: comments widened from "the Windows layer" to "the OS layer (winos, macos)"; no method changes.

// OSBuild stays the Windows build number (0 on macOS). bridge.Share.Capabilities sets ShareCaps.osBuild from the
// probe only when GOOS is windows (0 on macOS, where the probe's os_build is 02's packed major×10000+minor×100+patch),
// so the SPA never sees two osBuild values; macOS version checks use AppInfo.osVersion. New optional interface:
type OSVersioner interface{ OSVersion() string } // "27.2" on macOS, "10.0.26100" on Windows (winos adds it)

// New OSPage values (App.OpenOSSettings). Validity becomes per OS: OSHooks.OpenSettingsPage answers
// ErrNotSupported for a page its OS doesn't have, which the bridge maps to invalid_argument as before.
const (
	PageAudioRecording OSPage = "audio-recording" // macOS: Privacy & Security → (Screen &) System Audio Recording
	PageScreenRecording OSPage = "screen-recording"
	PageLocalNetwork    OSPage = "local-network"
	PageLoginItems      OSPage = "login-items"
)
// Valid() gains the four; "graphics-capture", "sound-communications" and "taskbar" stay Windows-only, "dnd" maps to
// Focus on macOS, "notifications" and "sound" exist on both.

// Optional interfaces of the OS layer, found by type assertion on Deps.OS (03 §6.1's pattern):
type DockPolicy interface{ SetDockVisible(on bool) }            // activation policy .regular / .accessory (D4)
type LoginItemLaunch interface{ LaunchedAsLoginItem() bool }     // §12.2
type WindowSharing interface {                                   // §10.2
	MinimizeForShare(nsWindow uintptr) error                    // -[NSWindow miniaturize:] on the main thread
}                                                                // (whether isshoni is visible is 02's flag, never an OS check here)
type AutostartState interface{ AutostartStatus() (AutostartStatus, error) } // §12.1
type AutostartStatus string // "enabled" | "disabled" | "requires-approval" | "unavailable"
```

`EventLock`, `EventUnlock`, `EventSuspend`, `EventResume`, `EventEndSession`, `EventEndSessionConfirmed` and
`EventDisplayChange` keep their meaning; §7.5 says what produces each on macOS. The app's lock-pause state machine,
the close notice and the end-of-session rule (03 §11.6) run unchanged on top of them.

### 5.2 `macos.Hooks`

```go
package macos // internal/desktop/macos

func New(o Options) (*Hooks, error) // starts the event observers on the main thread; safe before Wails runs
type Options struct {
	Log      *slog.Logger
	BundleID string // the running bundle's id ("" when unbundled: dev build, go test)
}
var _ app.OSHooks = (*Hooks)(nil)

func (*Hooks) OSBuild() int                                          // 0
func (*Hooks) OSVersion() string                                     // NSProcessInfo.operatingSystemVersion: "27.2"
func (*Hooks) RegisterHotkey(id app.HotkeyID, k hotkey.Combo) error // §10.4; app.ErrHotkeyTaken
func (*Hooks) UnregisterHotkey(id app.HotkeyID)
func (*Hooks) Hotkeys() <-chan app.HotkeyID
func (*Hooks) Pill() app.Pill                                        // the no-op pill (D6)
func (*Hooks) ExcludeFromCapture(nsWindow uintptr) error             // sharingType = NSWindowSharingNone on that window and on
                                                                     // every other window of NSApp (the status item's included)
func (*Hooks) IncludeInCapture(nsWindow uintptr) error               // NSWindowSharingReadOnly on the main window only
func (*Hooks) SetAutostart(on bool, exe string) error                // §12; exe is ignored (the bundle is the item)
func (*Hooks) Autostart() (bool, error)
func (*Hooks) AutostartStatus() (app.AutostartStatus, error)
func (*Hooks) OpenSettingsPage(p app.OSPage) error                   // §9.4's table, NSWorkspace openURL
func (*Hooks) KeepAwake(on bool)                                     // §7.6
func (*Hooks) Events() <-chan app.SystemEvent                        // §7.5
func (*Hooks) EndSessionHandled()                                    // releases ShouldQuit's wait (§7.5)
func (*Hooks) OpenPath(path string) error                            // NSWorkspace openURL: fileURLWithPath (Finder)
func (*Hooks) SetDockVisible(on bool)
func (*Hooks) LaunchedAsLoginItem() bool
func (*Hooks) MinimizeForShare(nsWindow uintptr) error
func (*Hooks) Close() error

// Process-level helpers used by main before the app exists:
func DataRoot(dev bool) (string, error) // NSFileManager URLForDirectory:NSApplicationSupportDirectory + "isshoni"
                                        // ("isshoni-dev" for dev builds, 03's dev root); never $HOME
func TempDir() (string, error)          // confstr(_CS_DARWIN_USER_TEMP_DIR): per user, 0700, never $TMPDIR
func BundleID() string; func BundlePath() string; func InBundle() bool
func ComputerName() string              // SCDynamicStoreCopyComputerName ("Alex's MacBook Pro"); 02's device name
func WebKitVersion() string             // WebKit.framework's CFBundleVersion, for About
func Translated() bool                  // sysctl.proc_translated == 1 (an x86_64 slice under Rosetta)
func ResponsiblePID(pid int) (int, bool) // dlsym("responsibility_get_pid_responsible_for_pid"); false if the SPI is gone
func InstallLaunchObserver()            // §12.2: must run before application.New
```

Every method is safe for concurrent use and returns within 2 s (`OnMainWait`'s bound); a timed-out call returns
`ErrMainThreadBusy` and logs WARN once a minute.

---

## 6. Start order and the process contract on macOS

### 6.1 The order of `main` (03 §13.4 on macOS)

**First action**: none on macOS (`restrictDLLSearch` is a no-op; the hardened runtime already ignores `DYLD_*`).

0. **`--sound-check-helper`**: Windows only, unchanged; macOS never spawns it (the engine plays the chime itself,
   03-macos-audio §13.3), and `helper_other.go`'s stub stays.
1. **One-shot modes** (§6.4): unchanged rules (skipped with `--open-url`), plus darwin's `--apply-update`.
2. **LaunchServices relaunch** (D10), replacing the shell relaunch: skipped with `--relaunched`, when not running
   from a bundle (`macos.InBundle()` false: `task desktop:dev`, tests), and when the single-instance lock is held
   (`macos.LockHeld(root)`; this start will only forward). If `ResponsiblePID(getpid()) != getpid()`:
   `NSWorkspace openApplicationAtURL:<bundle> configuration:` with `arguments = ["--relaunched", <the same args>]`,
   `createsNewApplicationInstance = YES`, `activates = YES`, `addsToRecentItems = NO`; wait ≤ 3 s for the completion
   handler; on success exit 0, on any error log WARN and continue in place (the share still works; only grant
   attribution and the WebKit match may be wrong, which 03-macos-audio's report shows). When the SPI is missing
   (`ResponsiblePID` false), skip with one INFO line.
2a. **Update applying** (the macOS twin of 03 §13.4 step 2a, 04 §2.4): `starter.installerRunning` =
   `flock(<root>/updates/applying.lock, LOCK_EX|LOCK_NB)` answers `EWOULDBLOCK` (the lock is taken and released at
   once when free; a missing file or folder = not running; any other error = WARN and start normally);
   `starter.writePending` = `macos.PendingFile(root).Write(app.Pending)` (the link of this command line, when there
   is one). When the lock is held, the start writes the pending link and **exits 0 without becoming an instance**,
   so nothing runs from a bundle 04's helper is renaming; the relaunched app reads the link at step 6. This is M2's
   `installerHandover` (`start.go`) with darwin's two functions; 04's updater e2e case F asserts it.
   **`--wait-pid=<pid>`** (new, macOS and accepted on Windows as a no-op): before step 3 the start waits until that
   process has exited (`kqueue` `EVFILT_PROC`/`NOTE_EXIT`), at most 10 s, then continues. Only `App.Relaunch` (§9.7)
   passes it, so the new process doesn't forward into the one that is quitting. 04's helper doesn't need it: it
   relaunches only after the old process has exited (§9.7).
3. **Single instance** (§6.2).
4. **The running marker**: none (Windows' mutex exists for the NSIS installer). 04's helper waits for the PID it
   was given (kqueue, 30 s).
5. **Start-time update**: 06's/04's `PendingAtStartup` as on Windows (04 decides whether macOS applies at start).
6. **Pending values**: `macos.PendingFile(root).TakePending()` reads and deletes `<root>/pending.json`
   (`{"server": "https://…", "room": "…"}`, ≤ 4 KiB; written only by this doc's own step 2a; a file that isn't the
   user's own regular file is deleted unread), the macOS counterpart of `PendingServer`/`PendingRoom`. 04's
   `install-desktop.sh` writes no file: it hands its server over as an `isshoni://<host>/` link through `open -a`,
   which arrives as `kAEGetURL` (§6.3).
7. **The app**: env scrub (§8.4), `macos.InstallLaunchObserver()`, logs (the `start` line, §15), then
   **`update.CleanupStaging(log)`** (04 §7.5: removes leftover `.isshoni-update-*` staging folders; before the
   updater starts), servers, settings, engine, asset handler, Wails, then `HandleArgs` and the pending link. The
   engine build check (03 §13.4 step 7) is unchanged. In e2e builds the data root is
   `<ISSHONI_E2E_DATA_DIR>/isshoni` when that variable is set (§14.1).

`HandleArgs` gains no flag but `--wait-pid` (consumed by `start`, ignored by the app). `--open-url`'s injection rule
(03 §13.1) applies to synthesized link command lines too (§6.3).

### 6.2 Single instance

- **Scope**: one instance per user and data root (03 §13.3's reasoning: `settings.json`, `servers.json`, the
  WebKit store and the device credentials are per user).
- **The lock**: `open(<root>/instance.lock, O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC, 0600)`, then `fstat`: a regular
  file, `st_uid == getuid()`, `st_nlink == 1`, mode without group or other bits; otherwise the name is removed
  (`unlink`, never following a link) and created again with `O_EXCL` (03 §13.3's repair, logged by base name and
  reason). `flock(fd, LOCK_EX|LOCK_NB)`: held → first instance; `EWOULDBLOCK` → second instance; any other error →
  start as the first instance without the lock, with a WARN (03's rule: never exit as a "second" instance on doubt).
  The lock dies with the process.
- **The socket**: the first instance binds `<TempDir>/isshoni-<32 hex>.sock` (16 `crypto/rand` bytes per run; the
  per-user temp folder keeps the path under `sun_path`'s 104 bytes and is 0700), `chmod 0600`, listens, and only then
  writes the path into `instance.lock` (truncated, `fsync`). A second launch reads the path, connects, checks the peer
  with `getpeereid` (uid must equal its own), writes one JSON line `{"v":1,"args":[…]}` (≤ 64 KiB), and exits 0 within
  2 s; a missing path, a refused connect or a foreign peer are retried until the 2 s bound, then logged and the launch
  exits 0 (03's rule). The first instance checks its peer the same way, reads one line with a 2 s deadline, and hands
  the args to `fwd` (03's queue, ≤ 8 before `Boot`). Close unlinks the socket and clears the file.
- **LaunchServices**: a launch from Finder, the Dock, Spotlight, `open -a isshoni` or a URL never starts a second
  process while one runs: macOS activates the running app (Wails' `ApplicationShouldHandleReopen` → `ShowWindow`) or
  delivers `kAEGetURL` to it. The lock and socket cover a terminal start, `open -n`, the relaunch race and tests.
- Tests (`macos`, every OS for the pure parts, darwin for the rest): `TestLockRepair` (a symlink, a hard link, a file of
  mode 0644, a directory at the name), `TestSecondInstanceForwards` (two processes, one root),
  `TestTwoRootsIndependent`, `TestForeignPeerRefused` (a socket bound by a process that `setuid`s; it needs root, so
  where it can't it skips with a `macenv` message and has a line in 05's `.github/macos-skips.txt`),
  `TestStaleSocketPath`, `TestWaitPID`.

### 6.3 The `isshoni://` scheme

- Registered by the production bundle's `CFBundleURLTypes` (§4.4). LaunchServices registers the scheme at the
  bundle's first launch through `open` (04's installer always launches through `open`; the DMG path registers on the
  Finder copy). No `lsregister` call is assumed; WM26 checks a link right after a fresh install, and only if that
  fails on a fresh `--no-launch` install does MD4 ask 04 to add `lsregister -f "<app>" 2>/dev/null || true` to its
  step 9.
- Delivery: Wails' `kAEGetURL` handler emits `ApplicationLaunchedWithUrl` with the URL string. `start_darwin.go`
  registers a listener **before** `Run` that turns it into the command line `["--open-url", url]` and puts it into
  `fwd` (03 §13.3's queue), so a link that launched the app waits for `Boot` exactly like a forwarded command line,
  and one that arrives later goes to `HandleArgs`. A URL longer than 2048 bytes or not starting with `isshoni:` (case
  insensitive) is dropped with a debug line (scheme and host only, 03 §13.1).
- Links are rate-limited at one per second (03 §13.2) whatever the source.
- Browsers ask "Open isshoni?" before handing a link over; nothing about that changes the 03 §13.2 table.

### 6.4 One-shot modes on macOS

| Mode | macOS behavior |
|---|---|
| `--version` | the M2 JSON plus `"arch": runtime.GOARCH` (`"arm64"` \| `"amd64"`), added on **all** OSes (additive; the Windows golden gains it in the same PR); 04's `macos-runtime` asserts it per runner |
| `--self-check=<path>` | the engine is linked statically: `native.Open` returns the darwin engine (02); the JSON is 03 §13.4's without `ffmpegLoaded`/`ffmpegLicense` (absent, not false), with the macOS `engine` object `{"loaded":true,"abi":1,"testHooks":false,"name":"darwin","os":"macos","archiveSHA":"<hex>","encoders":["videotoolbox"]}` (`name` = `Info().Name`, `os` = `BuildInfo().OS`, `archiveSHA` = 02's exported `native.StaticArchiveSHA()`, `encoders` from the probe) and `"bundle": {"id": macos.BundleID(), "path": macos.BundlePath()}`. `ok` needs `loaded`, `abi == media.ABIVersion`, `testHooks` false, `spa.ok`, and on darwin `name == "darwin"`; it must **not** depend on `encoders` being non-empty or on any TCC API (hosted runners are VMs without a hardware encoder, 04 §2.4). 04's packaging check (`desktop-macos`, `macos-pkg`, `verify-macos`) compares `archiveSHA` with the packaged archive and `bundle.id` with `io.isshoni.desktop` |
| `--apply-update=<path>` | darwin only (other OS: an unknown flag, logged and ignored). Must be the only argument; runs 04's `update.RunApplyHelper(ctx, path, log)` and exits with its result (0 ok, 1 error); opens no window and takes no single-instance lock; skipped with `--open-url` like every one-shot. `app/args.go` gains `ModeApplyUpdate OneShot = update.ApplyHelperFlag // --apply-update=<path>, darwin only` and `Args.ApplyUpdatePath` |
| `--forget-credentials` | 02's `ForgetAll` over the Keychain store (every item of the service with the `isshoni:v1:` account prefix: the current leaf's items, then other leaves' items best effort, §13.1) |
| `--purge-data` | `ForgetAll`, then delete the data root (03's retry rule), then: `~/Library/WebKit/<bundle id>`, `~/Library/Caches/<bundle id>`, `~/Library/HTTPStorages/<bundle id>` and `<bundle id>.binarycookies`, `~/Library/Saved Application State/<bundle id>.savedState`; the app's preferences domain through `CFPreferencesSetAppValue` for every key plus `CFPreferencesAppSynchronize` (cfprefsd caches the plist, so deleting the file isn't enough); `SMAppService.mainAppService unregister`; and `tccutil reset All <bundle id>` (the user's own grants). Paths come from `NSFileManager` (Library folder) and the running bundle id, never the environment. Exit 0 always |
| `--update-now` | 04's (updatetest builds) |
| `-Embedding` | Windows only; on macOS it is an unknown flag (logged, ignored) |

**Which engine a build links** (`cmd/isshoni-desktop`). M2's constraints gave every `dev`/`e2e` build the fake engine
and left a darwin production build without `imstatic` compiling silently into `unsupported_os`. M3 changes them so
the grant tests (§19.4) can run the real tap in a dev build and a production build without the engine can't compile:
- `engine_fake.go`: `//go:build (windows || (darwin && cgo)) && (dev || e2e) && !imstatic`;
- `engine_release.go`: `//go:build (windows || (darwin && cgo)) && ((!dev && !e2e) || imstatic)`;
- `engine_noengine_darwin.go` (new): `//go:build darwin && cgo && production && !imstatic`, containing a reference to
  an undefined identifier (`var _ = productionDarwinBuildNeedsTheImstaticTag`), so the build fails at compile time.
Windows never sets `imstatic`, so its constraints evaluate exactly as before (`windows-go` builds both flavors).
Release builds use `-tags production,imstatic` through 04's `task desktop:app:macos`; the §19.4 bundle uses
`TAGS=dev,imstatic`. 02 §6.4's sentence becomes "`dev`/`e2e` keep the fake engine unless `imstatic` is also set".

---

## 7. Window, Dock, menus and the menu bar extra

### 7.1 The window

- 03 §4.4's size rules (min 800 × 560 pt, default 1200 × 760 centered, then the saved bounds clamped to a visible
  screen). `Mac.TitleBar` default (a native title bar; full-size content off), `Backdrop` normal,
  `TabbingMode` disallowed (no window tabs), `CollectionBehavior` default plus `fullScreenPrimary`.
- At creation, `OSHooks.ExcludeFromCapture(NativeWindow())` (when `hideFromCapture`, 03 §11.5).
- `--autostart` or a login-item launch: no window is shown (§12.2). darwin always builds the deferred shell
  (`newDeferredShell`) and shows the window at `ApplicationStarted` unless the launch was a login item, because the
  login-item flag is known only once AppKit has finished launching.

### 7.2 Dock and activation policy (D4)

- `LSUIElement` true makes the app start as `.accessory`. `Shell.ShowWindow` calls `SetDockVisible(true)` (policy
  `.regular`) first, then shows and activates the window (`activateIgnoringOtherApps` on 14.x, `activate` on 14.0+
  where available). `HideWindow` hides it and then sets the policy that `dockVisible` gives: **`.regular` stays while
  a share is live, paused, reconnecting or unreachable**, `.accessory` otherwise. When the share ends while the window
  is still hidden, the app re-evaluates and switches to `.accessory` (the Dock icon goes away with the share).
- The decision is one pure function in `internal/desktop/app/dock.go` (all OSes; only darwin's `DockPolicy` hook acts
  on it):

  ```go
  // dockVisible reports whether the app should have a Dock icon (activation policy .regular).
  func dockVisible(windowShown, windowMinimized bool, st share.State) bool {
  	return windowShown || windowMinimized || st.Active()
  }
  ```

  `share.State` gains the exported `Active()` (live, paused, reconnecting, unreachable), which the existing
  unexported `active()` in `internal/desktop/share/mapping.go` becomes; its callers are renamed, behavior unchanged.

  The app calls it after `ShowWindow`, `HideWindow`, a minimize/restore and every share state change, and calls
  `SetDockVisible` only when the answer changes. `TestDockVisible` (`internal/desktop/app`, every OS) is its table:
  hidden + idle → false; hidden + each sharing state → true; hidden + ended → false; shown or minimized → true.
- With `.regular`, a Dock click while the window is hidden (or minimized) emits `ApplicationShouldHandleReopen` →
  `ShowWindow`. The Dock icon's menu is AppKit's default (Show All Windows, Hide, Quit → `ShouldQuit`).
- A minimized window keeps `.regular` (it lives in the Dock).

### 7.3 The main menu (Wails `application.Menu`, set once in `shell_darwin.go`)

| Menu | Items |
|---|---|
| **isshoni** | About isshoni (opens Settings → About) · — · Settings… ⌘, · Check for Updates… · — · Hide isshoni ⌘H · Hide Others ⌥⌘H · Show All · — · Quit isshoni ⌘Q |
| **Edit** | Undo ⌘Z · Redo ⇧⌘Z · — · Cut ⌘X · Copy ⌘C · Paste ⌘V · Select All ⌘A (AppKit roles; without them WKWebView's text fields get no clipboard keys, WM22) |
| **Window** | Minimize ⌘M · Zoom · — · Close ⌘W (→ the close hook, §7.5) · — · Bring All to Front |
| **Help** | isshoni Help (opens the project site's `/guide/` through `App.OpenExternal`'s rules) |
| **View** (dev and e2e builds only) | Reload ⌘R · Show Web Inspector ⌥⌘I |

"Settings…" and "About isshoni" show the window and emit **`isshoni:app.command`** `{command: "settings" | "about" |
"check-updates"}` (a new transient event; `bridge.Events` gains it). The SPA navigates to `/desktop/settings`,
`/desktop/settings#about` or `#updates` (the latter also calls `Updates.Check()`); the event carries no path, so no
native code can steer the page anywhere else. Menu titles come from the catalog (`desktop.native.menu.*`), AppKit's
standard items keep their system titles.

The menu is built by a pure function of the flavor (`mainMenu(flavor) []menuItem`, turned into Wails' `Menu` by
`shell_darwin.go`), so **`TestMainMenuByFlavor`** (`cmd/isshoni-desktop`, every OS) asserts that the production
flavor's menu has no View menu, no Reload and no Web Inspector item, and that dev and e2e have them. This replaces a
"menu dump through `--self-check`" (a one-shot builds no `NSApp` menu); 05's WM9 row cites this test.

### 7.4 The menu bar extra (the tray on macOS)

- Wails' `SystemTray` (an `NSStatusItem`) driven by `tray.Render` unchanged (03 §11.1): icon id → `tray-idle` as a
  **template** image (`SetTemplateIcon`, follows light/dark menu bars), `tray-live` red, `tray-paused` amber,
  `tray-warn` yellow (`SetIcon`, not templates: the color is the message).
- **Title**: while a share is live or paused, the item's title is the watcher count (`SetLabel("3")`; "0" is shown,
  since "nobody is watching" is information too); idle: no title. The sharer always sees who is watching (PLAN).
- **Tooltip**: `View.Tooltip` ("Sharing Minecraft · 3 watching", source cut to 40). Before 15.2 a pick carries no
  identity (02 §8.2.1: `Source.Name` empty), so when the share's label is empty `tray.Render` uses the kind-based
  label "a window" / "your screen" (`desktop.share.label.window|screen`, through `Catalog.TFor`): "Sharing your
  screen · 3 watching", never "Sharing  · 3 watching" (§10.1).
- **Click**: macOS convention, a click opens the menu (a status item has no separate "left click" action); the
  menu's first enabled item is "Open isshoni". The menu is 03 §11.1's, with the hotkeys formatted the macOS way
  (`hotkey.Combo.Display(hotkey.MacOS)`: "Pause sharing ⌃⌥⌘P").
- VoiceOver reads the item by its accessibility label, which `shell_darwin.go` sets to the tooltip text on every
  change (`button.accessibilityLabel` through `macos.SetStatusItemLabel`; Wails sets none).
- The item exists for the app's whole life (03 §11.1). On notched displays macOS may hide items that don't fit (and
  users can hide menu bar items on recent macOS versions); while sharing the Dock icon stays (§7.2), so the close
  notice (§7.5) names the menu bar item, the Dock icon and the Stop hotkey as the ways back, and Settings → App says
  so (no "keep visible" control, which macOS doesn't offer on 14 and 15).

### 7.5 Closing, quitting and what the system does (03 §11.6 on macOS)

| Event (macOS source) | While live (or paused, reconnecting, unreachable) | Otherwise |
|---|---|---|
| Window close (red button, ⌘W, Window → Close): Wails' `WindowClosing` hook | hide the window (`HideWindow`; the policy stays `.regular` while sharing, §7.2, so the Dock icon remains); the first time ever, the notification "isshoni is still sharing. Stop it from the red isshoni icon in the menu bar, the isshoni icon in the Dock, or ⌃⌥⌘S." and the window notice next time (03 §11.6's handshake, text `desktop.native.toast.stillSharing_macos`; the hotkey is the user's current Stop combination in `hotkey.Combo.Display(hotkey.MacOS)` form, and the clause is omitted when no Stop hotkey is registered) | quit, or hide when "Keep isshoni running in the menu bar when I close the window" (03's `closeToTray`) |
| ⌘Q, Dock → Quit, menu bar extra → Quit: `Options.ShouldQuit` | answers **false**, shows the window and emits `app.command {command: "confirm-quit"}`; the SPA asks "Stop sharing and quit?" and calls `App.Quit(true)` | answers true; 03's Quit path (stop, close 1000, flush) runs in `OnShutdown` (W14) |
| Logout, restart, shut down: `ShouldQuit` while `NSAppleEventManager.currentAppleEvent`'s `kAEQuitReason` is `kAELogOut`, `kAEReallyLogOut`, `kAEShowRestartDialog`, `kAEShowShutdownDialog`, `kAERestart` or `kAEShutDown` (also signalled earlier by `NSWorkspaceWillPowerOffNotification` → `EventEndSession`) | stop the share within 2 s (`system`), then answer true: isshoni never cancels a logout | answer true |
| Screen locked: distributed notification `com.apple.screenIsLocked`; fast user switch away: `NSWorkspaceSessionDidResignActiveNotification` | `EventLock`: 03's lock pause and `KeepAwake(false)` | — |
| Unlocked: `com.apple.screenIsUnlocked`; switched back: `NSWorkspaceSessionDidBecomeActiveNotification` | `EventUnlock`: 03's 5 min / 30 min rules unchanged | — |
| Sleep: `NSWorkspaceWillSleepNotification` | `EventSuspend`: stop (`system`); after wake (`NSWorkspaceDidWakeNotification` → `EventResume`) the notification "Sharing stopped because your Mac went to sleep" | C1 and C2 reconnect after wake |
| Screens changed: `NSApplicationDidChangeScreenParametersNotification` | `EventDisplayChange` (window re-clamp) | same |
| `SIGTERM` (`signal.Notify` in `cmd/isshoni-desktop`, darwin; 04's `macos-runtime` ends the app this way) | `App.Quit(true)` on the main thread: stops the share without the confirm-quit question, then the `OnShutdown` path (close 1000, flush); a second `SIGTERM` or 5 s without exit → exit 0 | the same Quit path |

`EventEndSessionConfirmed` is sent right after the stop, since macOS has no "cancelled logout" message that matters to
isshoni (a cancelled logout leaves isshoni running and idle). `EndSessionHandled` releases `ShouldQuit` early when the
stop finished before 2 s.

### 7.6 Keep-awake (D19)

`KeepAwake(true)`: `IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleDisplaySleep, kIOPMAssertionLevelOn,
"isshoni is sharing your screen", &id)` and `[NSProcessInfo.processInfo beginActivityWithOptions:
NSActivityUserInitiated | NSActivityIdleDisplaySleepDisabled | NSActivityLatencyCritical reason:@"Sharing"]`;
`KeepAwake(false)` releases both. Idempotent. `pmset -g assertions` shows the reason (tested on CI: the test asserts its
own assertion by name through `IOPMCopyAssertionsByProcess`).

---

## 8. Webview security on WKWebView

### 8.1 What stays as on Windows

The asset handler (03 §5.1), `bridge.js` (03 §5.2), the CSP header and `<meta>` (03 §5.3, `connect-src` with the
active host), the `/wails/*` Guard (03 §5.6, in its WKWebView form `assets.GuardWKWebView`, re-checked by WM4/WM21
against the release build), tokens only in Go and in a closure (03 §5.8), the three navigation layers (03 §5.5).
`BridgeConfig.App` gains `osVersion` ("27.2"; Windows fills "10.0.<build>"), `os` is `"macos"`, `osBuild` 0, `arch`
`"arm64"` or `"amd64"`. `ShareCaps.osBuild` is likewise set only when `app.os === 'windows'`:
`internal/desktop/bridge/share.go` copies `probe.OS.Build` only on Windows and sends 0 on macOS (where the probe's
`os_build` is 02's packed version, e.g. 270200), so no Windows-build threshold in the SPA can misfire; macOS version
checks use `AppInfo.osVersion`. A bridge golden case for macOS asserts it.

### 8.2 WKWebView settings (`mainWindowOptions()`'s `Mac` half and `appOptions()`'s `Mac` half)

| Setting | Value | Row |
|---|---|---|
| Web Inspector | off in production (Wails sets `developerExtrasEnabled` only with devtools; `isInspectable` false, 13.3+); on in dev and e2e | WM9 |
| Default context menu | off (`DefaultContextMenuDisabled`; the SPA also cancels `contextmenu` outside text fields) | WM9 |
| `AllowsMagnification` | false | WM9 |
| `AllowsBackForwardNavigationGestures` | false | WM9 |
| `JavaScriptCanOpenWindowsAutomatically` | false | WM5 |
| `EnableAutoplayWithoutUserAction` | true (no "tap to unmute", 03 §5.4's autoplay flag) | WM13 |
| `FullscreenEnabled` | true (element fullscreen, 12.3+'s `elementFullscreenEnabled`) | WM16 |
| `TabFocusesLinks` | true (keyboard users reach links with Tab, as in the browser) | — |
| `EnableFraudulentWebsiteWarnings` | false (no Safe Browsing lookups: the webview loads only its own pages; PLAN "No telemetry") | — |
| `ApplicationNameForUserAgent` | `isshoni/<version>` (replaces Wails' "wails.io" token) | — |
| `Permissions` | camera, microphone, geolocation, notifications denied (03 §5.4; Wails resolves `requestMediaCapturePermission` from them) | — |
| `EnableFileDrop` | false; the SPA cancels `dragover`/`drop` | WM9 |
| `Mac.ApplicationShouldTerminateAfterLastWindowClosed` | **false** (was true in the M2 dev build): the close rule of §7.5 decides | WM7 |
| `Mac.ActivationPolicy` | `ActivationPolicyAccessory` (D4) | — |

WebKit's website data store is the default one for the bundle id (`~/Library/WebKit/<bundle id>/`); there is no
user-data-folder option, and `audio.SelfInfo.WebViewDataDir` is `""` on macOS (03-macos-audio matches the WebKit
processes by responsible PID instead).

### 8.3 Navigation layer 3 on macOS (D18)

Where WebKit lacks the Navigation API (14.4 and 15 without a Safari update, WM5), M2's layer 2 in `bridge.js` does
nothing, so a script navigation really starts loading; and at the pinned Wails every Go → page event is delivered by
`ExecJS` calling `window._wails.dispatchWailsEvent(...)` in **whatever page is loaded**. Layer 3 on macOS therefore
acts as early as Wails lets it and also gates the events:

- `shell_darwin.go` listens to the main window's `events.Mac.WebViewDidStartProvisionalNavigation`,
  `events.Mac.WebViewDidCommitNavigation` and `events.Mac.WebViewDidFinishNavigation` (by the constant names of the
  pinned Wails' `pkg/events`, never their numbers). On each, `macos.WebViewURL(nsWindow)` (main thread: finds the
  `WKWebView` in the window's content view hierarchy and returns `webView.URL.absoluteString`; at provisional start
  that is already the target URL) is compared by `scheme://host` with the asset origin; `about:blank`, an empty URL
  and any other origin load the start page (`SetURL("/")`), which cancels a provisional load before it commits. The
  same rate limit applies across the three events: at most 3 reloads a minute, then one ERROR line; the WARN names the
  host only (03 §17).
- **Event gate**: the shell keeps `onAssetOrigin atomic.Bool`, set from every committed URL (`DidCommit`, `DidFinish`;
  true at start). `bridge.Deps` gains `EmitAllowed func() bool` (nil = always allowed); `bridge.New` wraps `d.Emit`
  once, so every `isshoni:*` event (`audio.*`, `share.*`, `auth.state`, `settings.changed`, `update.state`,
  `permissions.changed`, `app.command`) is dropped while `EmitAllowed()` is false, with one DEBUG line per drop burst.
  `cmd/isshoni-desktop` passes `shell.OnAssetOrigin` on darwin and nil on Windows, so Windows' emit path is the same
  function as today and its bridge tests prove it unchanged. Dropped events need no replay: after the start page loads,
  the SPA pulls every state (`Share.State`, `Permissions.State`, …) as it does at any start.
- `Shell.Reload()` loads the start page when the last URL seen was off the asset origin. `navguard.go`'s Windows half
  (`isshoni:where` over `postMessage`) is not used on macOS.
- Wails' `external` script message handler passes messages on without an origin check; on macOS isshoni registers no
  handler that acts on it, and the `/wails/*` Guard (`GuardWKWebView`, WM4/WM21) still decides every runtime call.

### 8.4 Environment scrub

Production builds unset `FRONTEND_DEVSERVER_URL`, every `WAILS_UPDATER_HELPER*` (03 §5.4) and every `WEBKIT_*`
(defense in depth; WebKit honors a few debug variables in some builds) before `application.New`; `scrubEnv` takes the
OS's prefix list (`scrubPrefixes` per `GOOS`), so Windows' list is unchanged (`TestScrubEnv` per OS).

### 8.5 The origin on the server

No change (D3): `wails://localhost` is `DesktopOrigins()[1]`, accepted bearer-only since M2. M3 adds in 02's file:

```go
// DesktopAssetOriginMacOS is the origin the desktop app's bundled SPA has on macOS (Wails v3's wails:// scheme in
// WKWebView). 01-desktop-macos passes it as its asset handler's Options.Origin and checks it with WM1.
const DesktopAssetOriginMacOS = "wails://localhost"
```

`platform_darwin.go`'s `assetOrigin` becomes that constant, and `TestDesktopOrigins` asserts
`DesktopOrigins()[1] == DesktopAssetOriginMacOS`. WM1's fallback (an `Origin: null`) is the only server change M3 could
need; it is decided by M3's first slice (MD6) on the dev build, before any app release depends on it.

---

## 9. TCC permissions

### 9.1 What macOS asks, and when

| Permission (TCC service) | When macOS asks | Needed for | Asked by isshoni |
|---|---|---|---|
| **System Audio Recording** (`kTCCServiceAudioCapture`, 14.2+; Info.plist `NSAudioCaptureUsageDescription`) | when isshoni first creates a process tap (03-macos-audio §13.2): the wizard's sound check; the tap delivers zeros until the user answers | sharing sound | **in the wizard's `audio-permission` step** (PLAN), never first at a share |
| **Screen Recording** (`kTCCServiceScreenCapture`) | never with `SCContentSharingPicker` | — | never; Settings → Permissions says "Not needed: you choose what to share in macOS's picker each time" |
| **Local Network** (15+; `NSLocalNetworkUsageDescription`) | the first connection to a LAN address (a server on the LAN, LAN ICE candidates); that first attempt fails (S2: `EPIPE`/`EHOSTUNREACH` until allowed) | servers and friends on the same network | reactively, where the first LAN connection fails (§9.5) |
| **Notifications** (UNUserNotificationCenter authorization) | `requestAuthorizationWithOptions:` | safety notices, mic prompts, update notices | in the wizard's macOS step, a separate button |
| Accessibility, Input Monitoring, Microphone, Camera | never | — | never (D8, D17) |

All grants are tied to the designated requirement of the one self-signed identity, so they survive updates N → N+1
(S2: builds 1→9 without a second prompt on 27; the 14.4, 15 and 26 runs are 05's).

### 9.2 The `Permissions` bound service (new, all OSes)

```go
// internal/desktop/bridge/permissions.go
type PermissionsView struct {
	AudioCapture    string `json:"audioCapture"`    // "granted" | "denied" | "not_determined" | "unknown" | "not_needed"
	Notifications   string `json:"notifications"`   // "granted" | "denied" | "not_determined" | "unsupported"
	LocalNetwork    string `json:"localNetwork"`    // "ok" | "blocked" | "unknown" | "not_needed"
	ScreenRecording string `json:"screenRecording"` // "not_needed" (picker) on macOS and Windows
}
type AudioPermissionResult struct {
	State    string `json:"state"`    // as AudioCapture; "not_determined" after the 30 s watchdog
	WaitedMs int    `json:"waitedMs"` // measured by the bridge around the sound check
}
```

There is no separate permission API in the engine: 03-macos-audio's prompt appears only when a tap is built, and its
answer comes back in the sound check's result and in the audio report (03 §13.2, §13.3). So the bridge builds both
values from those:
- **`Permissions.RequestAudioCapture()`** runs the audio service's `SoundCheck` (the same call `Audio.SoundCheck`
  makes) and maps `media.SoundCheckResult.Permission` to `State`: `granted` → `granted`, `denied` → `denied`,
  `pending` → `not_determined`, `unknown` → `unknown`.
- **`Permissions.State().audioCapture`** is read fresh, not from the cache: M2's audio service polls reports only
  while watching or live (`activeLocked()` = `watcherOn || live`, `internal/desktop/audio/service.go`), so a cached
  report is stale or absent exactly when the user comes back from System Settings with nothing shared. New in
  `internal/desktop/audio`:

  ```go
  // PermissionState asks the engine once, bounded at 1 s, for the TCC preflight (03-macos-audio §13.2: the
  // engine reads it on every im_list_audio_apps call, idle, watching or live, and builds no tap).
  // It returns the report's Tap.Permission; on an engine error or timeout it returns the cached report's value
  // ("unknown" when there is none) together with the error, which the caller logs at DEBUG.
  func (s *Service) PermissionState(ctx context.Context) (string, error)
  ```

  It calls `eng.AudioReport` under `context.WithTimeout(ctx, time.Second)` and updates the cache. The value is mapped
  as for `RequestAudioCapture`. On darwin `Permissions.State()` and the key-window re-read below use it; Windows
  answers `not_needed` without calling it.
- **The bound**: M2's `Audio.SoundCheck` wraps the call in `withTimeout(ctx, ioTimeout)` (10 s), which would cut 03's
  35 s darwin call (30 s permission wait + 3 s + 0.5 s) and answer `internal/timeout` to anyone who takes more than
  about 7 s to answer the macOS prompt. `internal/desktop/bridge/audio.go` gains `soundCheckBound` = **40 s on darwin**
  (`runtime.GOOS`), still 10 s on Windows; `Audio.SoundCheck` and `Permissions.RequestAudioCapture` both use it. A
  bridge test with a fake policy that answers after 15 s on darwin asserts the result arrives; Windows goldens and
  tests are unchanged.

| Method | In → out | Errors | Notes |
|---|---|---|---|
| `Permissions.State()` | → `PermissionsView` | — | pull for `permissions.changed`; ≤ 1 s on darwin (`audio.Service.PermissionState`); Windows answers `not_needed` for audio and local network and the notification state Windows reports, so the SPA can call it everywhere |
| `Permissions.RequestAudioCapture()` | → `AudioPermissionResult` | `busy` (a share is live), `not_implemented` (no engine) | ≤ 40 s (`soundCheckBound`): a sound check whose tap build raises the prompt, with 03-macos-audio's 30 s watchdog |
| `Permissions.RequestNotifications()` | → `string` (the new state) | — | `UNAuthorizationOptionAlert \| UNAuthorizationOptionSound`; ≤ 60 s (the user may take a while) |
| `Permissions.LocalNetworkRelevant(origin)` | `string` → `bool` | `invalid_argument` | true on 15+ when `origin`'s host is a `.local` name or resolves (≤ 2 s) to a private, link-local or ULA address; false elsewhere |
| `App.Relaunch()` | → — | `busy` (live) | §9.7 |

Event **`isshoni:permissions.changed`** (`PermissionsView`): emitted after each request, when the window becomes key
again (the user may have changed System Settings meanwhile; `NSWindowDidBecomeKeyNotification` → a fresh
`PermissionState` call, never the cached report), and when
`localnet` (§9.5) records a block or a success.

### 9.3 The wizard's macOS step (`audio-permission`)

`DesktopFeatures.wizardSteps()` is `['link', 'audio-permission', 'sound', 'voice']` when `app.os === 'macos'`
(03 §7.1 reserved it). Like `sound` and `voice`, it follows the link step only on first install (`wizardDone` false).
`web/src/desktop/AudioPermissionStep.tsx`:

```
 [audio-permission]
   Let isshoni record your Mac's sound
   isshoni sends your Mac's sound to friends, without your voice apps. macOS asks once: click Allow.
   isshoni never uses your microphone.
                                                      [Continue]
       │ Permissions.RequestAudioCapture()   "Waiting for your answer in the macOS dialog…" (spinner, ≤ 30 s;
       │                                     "Checking…" instead while the report's permission is `unknown`)
       ├─ granted ────────────────────────► "✓ isshoni can record your Mac's sound"  → notifications row → [Next]
       ├─ denied ─────────────────────────► denied panel (below)
       ├─ not_determined (watchdog, 30 s) ► "macOS hasn't shown or answered its dialog. It may be behind this window."
       │                                     [Try again] [Open System Settings] [Continue without sound]
       └─ unknown (preflight unavailable) ► [Next]: the sound check decides
   Notifications (recommended): isshoni tells you when sharing stops or an app starts using your microphone.
                                                      [Allow notifications]  (Permissions.RequestNotifications)
```

- **Denied panel**: "macOS blocks isshoni from recording sound. Friends would see your screen without sound." Steps,
  numbered: 1. [Open System Settings] (`App.OpenOSSettings('audio-recording')`); 2. "Turn on **isshoni** under
  **System Audio Recording Only**" (14.x: "System Audio Recording"; the text uses `osVersion`); 3. come back here.
  On `permissions.changed` with `granted` the panel turns into the granted line. If the state is still `denied` the
  second time the window becomes key, the panel adds [Quit and reopen isshoni] (`App.Relaunch()`, §9.7).
  [Continue without sound] goes on; the step never blocks Done (03 §8.4's rule for the sound check).
- **Preflight unavailable**: while the last report's `tap.permission` is `unknown` (03 §13.2: the TCC preflight SPI is
  absent or answered an unexpected value), the spinner line is "Checking…" (`desktop.setup.audioPermission.checking`),
  not "Waiting for your answer in the macOS dialog…": 03 §13.3 step 3 then skips the 30 s wait and plays its chime
  at once, so no dialog may be on screen. Vitest covers both lines.
- The step calls the request **only on Continue** (a click), so the macOS dialog never appears without the user
  having read why.
- The permission step's request **is** a sound check (§9.2); the **sound check** step (03 §8.4) then runs a second,
  normal one (3 s), whose result is 03-macos-audio's: `captured && backend === 'process-tap'` → "✓ isshoni can capture
  your Mac's sound, without your voice apps"; `!captured` with reason `permission_denied` → "macOS still blocks
  isshoni from recording sound" with [Back to the permission step]; reason `permission_pending` → the permission
  step's not-determined text ("macOS hasn't shown or answered its dialog…") with the same button; other reasons as
  03 §8.4.
- The voice step is unchanged (05's list; macOS voice apps are 03-macos-audio's).

### 9.4 Denied states and their fixes

`OSHooks.OpenSettingsPage` opens only these fixed URLs (`internal/desktop/macos/pages.go`), with `NSWorkspace
openURL:`; when the URL fails to open, the row's fallback opens. WM24 checks each on every OS version.

| Page | URL | Fallback | Shown where |
|---|---|---|---|
| `audio-recording` | chosen by OS version in `pages.go`, because `openURL:` usually returns YES even when System Settings lands on a wrong or generic pane: **14.x** `x-apple.systempreferences:com.apple.preference.security?Privacy_AudioCapture` first; **15+** `…security?Privacy_ScreenCapture` ("Screen & System Audio Recording", whose "System Audio Recording Only" list 03 §14.5's text names) first | the other of the two, then `…com.apple.preference.security`; WM24 records the result per version | wizard, Settings → Permissions, 03-macos-audio's permission banner |
| `screen-recording` | `x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture` | `…com.apple.preference.security` | no UI button: the picker never needs this grant (§9.1), so no screen sends a user there; kept in `pages.go` for docs and tools only (WM24 still checks it) |
| `local-network` | `x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork` | `…com.apple.preference.security` | §9.5 |
| `notifications` | `x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=<bundle id>` | `…com.apple.Notifications-Settings.extension` | Settings → Permissions, Settings → App |
| `dnd` (Focus) | `x-apple.systempreferences:com.apple.Focus-Settings.extension` | `x-apple.systempreferences:com.apple.preference.notifications` | the DND tip (§10.1) |
| `login-items` | `x-apple.systempreferences:com.apple.LoginItems-Settings.extension` | `x-apple.systempreferences:com.apple.preferences.users` | autostart `requires-approval` (§12.1) |
| `sound` | `x-apple.systempreferences:com.apple.Sound-Settings.extension` | `x-apple.systempreferences:com.apple.preference.sound` | sound check `no_device` |

| State | What the user sees | Fix |
|---|---|---|
| System Audio Recording denied (or never answered) | wizard panel (§9.3); during a share 03-macos-audio's `audio-permission` banner (warning level, `params.cause` `pending` \| `denied`; texts `desktop.audio.banner.audioPermission*`, 03 §14.5); Settings → Permissions "Not allowed" | [Open System Settings] → `audio-recording`; [Check again]; [Quit and reopen isshoni] |
| Screen capture refused (`engine.permission_denied` from `Pick`/`Start`; the picker needs no grant, but macOS may refuse a stream: a declined system confirmation, or a management profile) | share sheet: "macOS didn't allow this capture. Try again and choose Share in macOS's picker. If your Mac is managed by an organization, its settings may block screen sharing." | [Try again]; the link `/troubleshooting#macos-permissions-reset`. No [Open System Settings]: a Screen Recording grant is broad, persistent, brings macOS 15+'s periodic reminders, and isn't what isshoni uses |
| Local Network blocked (15+) | add/probe step, Offline screen or share panel line (§9.5) | [Try again] after Allow; [Open System Settings] → `local-network` |
| Notifications denied | Settings → Permissions "Off: isshoni can't warn you outside its window"; nothing else changes (safety notices also show in the window, and the menu bar extra turns red) | [Open System Settings] → `notifications` |
| Login item needs approval | Settings → App: "macOS needs your OK: System Settings → General → Login Items" | [Open Login Items] → `login-items` |

### 9.5 Local Network (15+)

- `macos.ClassifyLocalNetwork(err error, ip netip.Addr, osMajor int) bool` (pure, tested on every OS): true when
  `osMajor >= 15`, the address is private (RFC 1918), link-local, CGNAT is excluded (a VPN's 100.64/10 is not "local
  network" for macOS), ULA `fc00::/7`, or the host is a `.local` name, and the error wraps `EHOSTUNREACH`, `EPIPE`,
  `ENETUNREACH` or `EPERM` (the failures S2 saw while the grant was missing).
- **Go side**: `servers.Probe` (03 §8.1) maps such a failure to the new bridge code **`local_network`** (`params:
  {host}`; `bridge/codes` and `codes.gen.ts`); `deviceauth`'s link and C2's dial log it once and set `localnet`'s state
  to `blocked`; a later success sets `ok`. The add and probe steps show: "macOS asks whether isshoni may connect to
  devices on your local network. Click **Allow** in its dialog, then try again." [Try again] [Open System Settings].
- **Page side**: C1 and the viewer run in WebKit, whose failures the page can't classify. When `apiFetch`'s `/info`
  fails and `Permissions.LocalNetworkRelevant(serverOrigin)` is true, the Offline screen adds the same line and
  buttons. When a share is `unreachable` and the server is relevant, the panel adds it under "Can't reach the server's
  media port".
- Servers on the public internet (the plan's VPS default) never meet any of this.
- **`NSBonjourServices` is not set** (§4.4). MD-M9 (05's M-MAC-16) also checks that a LAN viewer (WKWebView WebRTC
  with mDNS `.local` host candidates) connects on 15+ after Allow; if it doesn't, MD11 adds `NSBonjourServices`
  `[_isshoni._udp]` through 04's template.

### 9.6 Notifications permission

The wizard's button and Settings → Permissions ask; nothing else does. While `not_determined`, the app shows no
notification (it doesn't call `requestAuthorization` from `Notify`); safety notices that would have been sent are
covered by the window notice and the menu bar (03 §11.6 already has both). `Notify` on a `denied` state returns nil
and logs debug.

### 9.7 Relaunch after a grant change

`App.Relaunch()`: `busy` while live; else spawns the bundle through `NSWorkspace openApplicationAtURL:` with
`createsNewApplicationInstance = YES` and `arguments = ["--relaunched", "--wait-pid=<this pid>"]`, then runs Quit. The
new process waits for the old one (§6.1) and takes the lock. Used by the permission panels. `--wait-pid` exists for
this method only: 04's helper relaunches with `open -a <bundle> --args --updated-from=<v>` after the old process has
exited (04 §7.3 step 7), and `open` makes isshoni its own responsible process, so step 2 is a no-op there.

---

## 10. Sharing on macOS

### 10.1 The flow

The SPA flow (03 §10.2) and the share controller (03 §10.1) are unchanged except where named:

- **Pick**: on darwin the controller does **not** show, raise or activate the window before `Pick` (Windows keeps its
  "show and activate first", 03 §10.1, so the picker dialog has an owner): it calls `eng.Pick(ctx,
  media.PickRequest{OwnerWindow: <NSWindow*>, Kinds: nil})` directly (nil = window first, PLAN "The picker defaults to a
  window"). 02-macos-engine presents `SCContentSharingPicker` (14.0+, system UI, not owned by the window; configured
  with `excludedBundleIDs = [<bundle id>]` so isshoni never appears in it). In window style the user clicks the target
  **on the live desktop**, where isshoni's own window (excluded, but still drawn; 1200×760 centered by default) can
  cover the game or browser to share. The controller's step is one optional interface on its `share.Window`
  (`internal/desktop/share/share.go`): `type PickPreparer interface{ PreparePick() (restore func()) }`. When the window
  implements it (darwin's adapter), `controller.go`'s `Pick` calls it instead of `w.ShowWindow(true)`; Windows' adapter
  doesn't implement it, so its `ShowWindow(true)` call stays byte-for-byte. Darwin's `PreparePick` is chosen by EM8's
  record (02's M-PICK-MAC, run on 14.4, 15.x and 27 with isshoni's window centered over the target): if the target can
  be clicked through or around isshoni's window, `PreparePick` does nothing (and returns a no-op `restore`); if not, it
  miniaturizes the main window (`MinimizeForShare`, the same hook §10.2 uses; ordering it back with `orderBack:` if EM8
  shows that is enough). `restore` runs when the pick completes, is cancelled or times out, and restores the window
  **before** the first-share sheet so the sheet is visible; on a completed pick of a self-visible screen §10.2's
  minimize then follows. Cancel or the 120 s timeout → `ErrCancelled` → `null`. While open, the sheet says "Choose what
  to share in the macOS picker…" (`desktop.share.sheet.picking_macos`).
- **First-share sheet and DND tip** (03 §10.2): unchanged rules. The DND tip's text on macOS: "Sharing your whole
  screen? Turn on a Focus (Do Not Disturb) so notifications don't show up for friends. Allow isshoni in that Focus, so
  its warnings still reach you." [Open Focus settings] (`dnd` → Focus, §9.4). The first-share sheet's hotkey line uses
  the macOS symbols.
- **Start, Pause, Preset, Share sound, Stop**: unchanged (01's core, 05's audio service over 03-macos-audio's engine).
- **Pick of an isshoni window** (should be impossible with `excludedBundleIDs`; `IM_SRCF_SELF` still guards it):
  03 §10.5's `self_window` row, unchanged.
- **A pick without identity** (14.4–15.1; 02 §8.2.1: `Source.Name`, `PID`, `Exe` and `MonitorID` are empty): when
  `Picked.Label` is empty, the SPA (`ShareView.label`, the sheet, the panel) and `tray.Render` (the tooltip, §7.4)
  show a kind-based label: "a window" / "your screen" (`desktop.share.label.window|screen` in the SPA, and the same
  keys under `desktop.native.*` through `Catalog.TFor` in Go). `controller.go` keeps passing `Label: src.Name`; the
  fallback is applied where the label is shown, so the server never receives a made-up name.
- **Switching content mid-share** from macOS's menu bar is **later (M4/M5)**: 02 sets
  `allowsChangingSelectedContent = NO`, so the share always keeps the picked content (stopping from macOS's UI still
  works, §10.3). When it lands, 02 §15's room applies (`updateContentFilter:` plus a protocol message carrying the new
  kind and name) and this doc's controller updates `ShareView.label` and `kind`.

### 10.2 Own windows: exclusion, auto-minimize, mute (D7)

The engine decides per OS version which exclusion strategy works (02 §8.8, measured in EM9) and reports only the
outcome; the app has no OS-version branch of its own:

```
 a whole-screen pick (Source.Kind screen)
   ├─ neither flag set: 02's filter (or, by 02's per-OS table, sharingType on 14.x) keeps isshoni out  → nothing to do
   └─ Source.SelfVisible (pick: IM_SRCF_SELF_VISIBLE 0x8)
      or snapshot.SelfVisible (session: IM_SESS_SELF_VISIBLE 0x4, the latest EventLayer.SelfVisible: a rebuilt
         filter SCK refused at im_start, so 02 fell back to the picker's original filter)
        → ShareView.selfVisible = true
        → pick flag (known after Pick): once the user confirms the first-share sheet (or the SPA calls Start without
          one) and BEFORE Start, the viewer
          mute applies and MinimizeForShare(main window) runs (if shown and not minimized); if Start fails, the
          window is restored (ShowWindow) so the error is visible
        → session flag only (known after im_start): when the share is live (or turns true while live):
          MinimizeForShare(main window) once, if it is shown and not minimized
        → in-app viewer audio muted until the share ends (the viewer mute below)
 a window or app pick: isshoni is not in it (02 sets the flag only on display picks)                 → nothing to do
```

- **The rule**: `ShareView.selfVisible = Source.Kind == screen && (Source.SelfVisible || snapshot.SelfVisible)`.
- **Shared core change**: in `internal/client/core/share.go`, `ShareSnapshot` gains `SelfVisible bool`, set from
  `ev.SelfVisible` next to `snap.NoOSIndicator = ev.NoOSIndicator` (both from `EventLayer`). Windows' engine never sets
  it, so Windows snapshots are unchanged. `internal/desktop/share/controller.go` copies `Source.SelfVisible ||
  snapshot.SelfVisible` (screen picks only) into `ShareView.selfVisible`. With `Source.SelfVisible` on a screen pick,
  `Start` sets `selfVisible` (so the viewer mute applies) and calls the minimize **before** `c.o.Engine.Start`/the
  core start, and on a `Start` error calls `ShowWindow(true)` before reporting it; with only the session flag it runs
  the minimize when the flag turns true while live. Either way the minimize happens at most once per share.
- **`ShareView.selfVisible`** (new, additive; Windows always `false`). When the pick already carries the flag, the
  first-share sheet shows "isshoni will minimize itself while you share your whole screen: this version of macOS can't
  hide it from your stream." The panel (when the user brings the window back during the share) shows two
  `role="status"` lines: "Friends can see this window while you share your whole screen." [Minimize]
  (`Window.Minimize()`, a new bound method), and "isshoni mutes friends' streams while you share your whole screen,
  so they don't hear themselves" (`desktop.share.panel.selfVisibleMuted`). The window is not minimized again by
  force: the user chose to show it.
- **The viewer mute** (`web/src/viewer/endpointMute.ts`; **this doc is its single owner**): the predicate becomes
  `mutesViewer(fh, share) = fh.live && (fh.engine === 'endpoint' || fh.selfAudible === true || share.selfVisible)`.
  `fh.selfAudible` is 03-macos-audio's `FriendsHear.selfAudible` (isshoni's own playback can't be kept out of the tap,
  e.g. the responsible-PID SPI is missing; PLAN: "If that fails, mute in-app viewer audio while sharing with audio");
  `share.selfVisible` is this doc's. M2's `attachEndpointMute` only subscribes to `audio.report` (`FriendsHear`), so it
  also subscribes to the share snapshot (`share.changed`, initial value from `Share.State`). 03 §14.4 only adds
  `FriendsHear.selfAudible` to `bridge.ts` and refers here for the predicate.
- **Banners**: the `viewer-muted` banner (computed in Go by `internal/desktop/audio`'s `friends.go`) keeps 03's
  meaning (`endpoint` or `selfAudible`) with 05's generic text; there is no `viewerMuted_macos` context. The
  `selfVisible` mute is explained only by the panel line above, because the audio service never learns
  `selfVisible`.
- `hideFromCapture` (Settings → App) keeps its meaning: off sets `NSWindowSharingReadOnly` on the main window (so
  screenshots show it). Where 02's table uses sharingType (14.x), the engine then reports `SelfVisible` on the next
  display pick; a change during a share takes effect at the next pick (02 §8.8), which the setting's text says.

### 10.3 What isshoni adds to macOS's capture indicator (D6)

macOS shows its own indicator in the menu bar while any app captures the screen (from 14.0 for SCStream; 15+ also
offers stopping the share from it). isshoni adds:
- the red menu bar extra with the watcher count and "Sharing <source> · N watching" (§7.4);
- the hotkeys (§10.4);
- notifications when the share stops without the user, pauses by hotkey with the window not in front, or a voice app
  becomes audible (03 §15's table, unchanged).
A stop from macOS's indicator ends the stream; 02 reports it as `ENDED{IM_ERR_CANCELLED, os_stopped}` (02 §5.1, §8.9
`UserStopped`). **Shared core change**: M2's `engineEnd` (`internal/client/core/share.go`) maps every engine code
other than `source_gone` to `EndError`, so it gains

```go
case merr.Code == media.CodeCancelled:
	return EndInfo{Reason: EndUser, Media: media.CodeCancelled, MediaReason: merr.Reason} // ByThisApp false
```

with a core unit test (fake engine ending with `cancelled`/`os_stopped` → `EndUser`, `ByThisApp` false). Windows'
engine never sends a cancelled `ENDED`, so its behavior is unchanged. The controller then shows `endReason: 'user'`
with the panel line "You stopped sharing from the macOS menu bar" (§10.6) and no error notification. 02 §5.4's UI
column points to §10.6 for the string. Settings → Sharing has no "Sharing pill" row on macOS; `pill` in
`settings.json` is kept and ignored.

### 10.4 Global hotkeys

- `macos.Hooks.RegisterHotkey`: `RegisterEventHotKey(keyCode, mods, {signature 'isHK', id}, GetApplicationEventTarget(),
  0, &ref)` on the main thread, with one `InstallApplicationEventHandler` for `kEventHotKeyPressed` installed at `New`.
  `eventHotKeyExistsErr` (-9878) → `app.ErrHotkeyTaken`; other errors → wrapped `ErrInvalid`. Presses go to
  `Hotkeys()` through §4.3's callback. Registered at `live`, unregistered at the end (03 D9); no TCC permission
  involved, and they work while another app (a full-screen game) is in front.
- **`internal/desktop/hotkey`** (pure) gains the `Cmd` modifier and an OS parameter:

  ```go
  type OS int
  const ( Windows OS = iota; MacOS )
  func ParseFor(s string, os OS) (Combo, error)   // Parse(s) == ParseFor(s, Windows), unchanged
  func (c Combo) ValidateFor(os OS) error
  func (c Combo) Display(os OS) string            // "Ctrl+Alt+Shift+F9" (Windows), "⌃⌥⌘P" (macOS)
  func (c Combo) Spoken(os OS) string             // "Control Option Command P", for aria-label and VoiceOver
  func (c Combo) MacKeyCode() (uint16, bool)      // kVK_ANSI_*, kVK_F1…F20, kVK_Home …; Num0–9 → kVK_ANSI_Keypad*
  func (c Combo) CarbonModifiers() uint32         // cmdKey | optionKey | controlKey | shiftKey
  var DefaultPauseMac = MustParseFor("Ctrl+Alt+Cmd+P", MacOS)
  var DefaultStopMac  = MustParseFor("Ctrl+Alt+Cmd+S", MacOS)
  ```

  Text form stays `Ctrl`, `Alt`, `Shift`, then `Cmd` on macOS (`Ctrl` = ⌃ Control, `Alt` = ⌥ Option), then the key;
  Windows refuses `Cmd` as before (its `winNames` list). macOS rules: at least two modifiers, one of them `Ctrl` or
  `Cmd`; or an F-key with at least one modifier; `Alt`/`Shift` alone with a letter is refused (Option + letter types
  characters on every Mac layout). Fuzzed round trips per OS.
- `settings.json` keeps one `hotkeys` object; its defaults are per OS (`settings.Defaults(os)`), so a Mac's file
  holds the Mac defaults. `Settings.TestHotkey`'s `typesChar` on macOS: `UCKeyTranslate` with the current layout
  (`TISCopyCurrentKeyboardLayoutInputSource`) and the combo's modifiers; any result with Ctrl or Cmd counts as no
  character.

### 10.5 Pause, Stop, tray actions

Unchanged (03 §11.3): every path goes through the controller. The pause notification text uses the Mac combination
("Sharing paused. ⌃⌥⌘P resumes."). The Pause hotkey isn't registered on a server without `share.pause` (03's rule).

### 10.6 Engine errors → UI on macOS

03 §10.5's tables apply; the rows below replace or add macOS texts (keys with the `_macos` context, §17). Reason
tokens are 02-macos-engine's (§21).

| Code / reason | During | macOS UI |
|---|---|---|
| `permission_denied` / `capture_denied` (SCK `UserDeclined`) | pick, start, live | "macOS didn't allow this capture. Try again and choose Share in macOS's picker. If your Mac is managed by an organization, its settings may block screen sharing." [Try again] and the link `/troubleshooting#macos-permissions-reset` (live: the share ends with this text). No Screen Recording button (§9.4) |
| `unsupported_os` | probe | "Sharing needs macOS 14.4 or later." (viewing works) |
| `no_encoder` / `no_hardware` | probe, start | "This Mac can't encode video in hardware." with a link to `/troubleshooting#macos-encoder` |
| `no_encoder` (other reasons, not naming `q`) | probe | "This Mac's video encoder isn't available." with a link to `/troubleshooting#macos-encoder` |
| `no_encoder` / `layer_failed: q` | start | the 4K choice below |
| `cancelled` / `os_stopped` | live | ended quietly as `user` (§10.3): "You stopped sharing from the macOS menu bar" (no notification) |
| `source_gone` / `capture_restart_failed` | live | "macOS stopped the capture and it couldn't restart." [Share again]. (Not "after your Mac slept": §7.5 already stops the share at `NSWorkspaceWillSleepNotification`, so 02's restart path only follows system stops, display sleep and lock.) |
| `source_gone` (other) | live | "The window you shared was closed" (03's row) |
| `encoder_lost` / `device_removed` | live | "macOS's video encoder stopped, so the share stopped." [Share again] |
| `bad_state` / `no_run_loop` | any | the generic "Something went wrong" (03's row), logged at ERROR with the reason |
| `missing`, `version`, `abi` | open, probe | "Part of isshoni is damaged. Reinstall isshoni from your server's Download page." [Open download page] |
| `driver_too_old`, `no_media_foundation`, `blocked_by_policy` | — | never on macOS |

**4K60 over the VideoToolbox budget** (PLAN Engines table; 02 §9.3, §5.4; M2 04 §16's contract). A two-layer start
over the budget fails `Start` with `IM_ERR_NO_ENCODER`, reason `layer_failed: q`. Nothing on `main` handles it yet,
and an inactive `q` still counts against 02's budget, so "4K60 alone" can't be done by marking `q` inactive. M3
specifies:
- `internal/desktop/share/controller.go`: when `Start` fails with `no_encoder` and a reason naming `q`, the attempt
  ends with the new SPA state `fourKChoice` (sheet, not an error), offering **[4K30 with preview]** and **[4K60
  without preview]**. Each retries `Start` with an override: `share.StartRequest.Override` →
  `core.ShareRequest.Override core.ShareOverride{FullFPSCap int; NoPreview bool}`.
- `internal/client/core/share.go`, `config()`: `FullFPSCap: 30` caps layer `f`'s `MaxFPS` at 30; `NoPreview: true`
  omits `q` from `cfg.Layers` and publishes `q` as inactive to the server (so viewers don't wait for it).
- SPA: `web/src/share/native/engineErrors.ts` (the `layer_failed: q` mapping) and `NativeSharePanel.tsx` (the two
  buttons; texts `desktop.share.fourK.*`).
- Tests: a core test with the fake engine configured to refuse a two-layer 4K60 start (asserts the override's
  config: `f` at 30 fps with `q`, or `f` alone with `q` published inactive); a controller test for `fourKChoice` and
  each retry; Vitest for the sheet. The code is OS-neutral: the same choice appears if
  Windows' engine answers `layer_failed: q` (M2 04 §16's contract); without that answer nothing changes, so M2's
  Windows tests stay as they are.

The OpenH264 opt-in is absent on macOS (`Settings.OpenH264()` answers `unavailable`, 03 §14.4's existing rule hides
it), since VideoToolbox is always there (PLAN: "Software fallback: not needed").

---

## 11. Notifications

- `notify_darwin.go` uses Wails' notifications service (`pkg/services/notifications`, `notifications_darwin.m`:
  `UNUserNotificationCenter`) instead of M2's log-only notifier. The service refuses to start without a bundle id
  (`checkBundleIdentifier`), so `task desktop:dev` (unbundled) keeps the log-only notifier; bundles get the real one.
- **Categories**: one `UNNotificationCategory` per 03 §15 kind with buttons (`mic-prompt`: Keep out, Include;
  `browser-mic`: Keep browser out, Include; `lock-resume`: Resume, Stop; `open`: Open; `update-ready`: Restart now),
  registered at start. Each notification's `userInfo` carries 03 §15's nonce; the action rules (a nonce of this run,
  not settled) are unchanged, so an action delivered after a restart is dropped except `open`, which shows the
  window. A click on the notification body is `open`.
- **When the app isn't running**: macOS launches it and delivers the response to the delegate after launch (no
  `-Embedding` mode); the delegate is installed by the service's start, which runs at `ApplicationStarted` before
  `Boot`.
- **Foreground**: `willPresentNotification` presents banners even while isshoni is frontmost only for the safety kinds
  (still sharing, sharing stopped, voice apps heard); the others are suppressed when the window is key (03's
  `windowInFront` rule decides before sending anyway).
- **Removal**: `RemoveToasts(ids…)` maps to `removeDeliveredNotificationsWithIdentifiers:` (macOS removes single
  notifications by id; none of Windows' workaround is needed). `macos.Hooks` implements 03 §15's optional remover.
- **Authorization** (§9.6) persists per bundle id across updates (WM8 checks N → N+1).
- Texts: `desktop.native.toast.*` with `_macos` context where Windows words appear ("tray icon (^)" → "red isshoni
  icon in the menu bar", "PC" → "Mac").

---

## 12. Autostart

### 12.1 The setting

Settings → App: "Open isshoni when I log in to this Mac" (`autostart`, off). `SetAutostart(true)`:
`[SMAppService.mainAppService registerAndReturnError:]` on the main thread; `false`: `unregisterAndReturnError:`
(`SMAppServiceStatusNotRegistered` counts as success). `AutostartStatus()` maps `status`: `Enabled` → `enabled`,
`RequiresApproval` → `requires-approval`, `NotRegistered`/`NotFound` → `disabled`. A `requires-approval` result
keeps the setting on and shows §9.4's row. 03 §16.1's "the OS is the truth" applies: at `Run`, `settings.json`'s
`autostart` follows `status` (`enabled` or `requires-approval` → true). Only a bundle in `/Applications` or
`~/Applications` can register (macOS needs a stable path); elsewhere the control is disabled with "Move isshoni to
Applications to use this" (`macos.BundlePath()`).

### 12.2 Recognizing a login-item launch

`macos.InstallLaunchObserver()` (before `application.New`) adds an `NSApplicationDidFinishLaunchingNotification`
observer on the main queue. Inside it, `[[NSAppleEventManager sharedAppleEventManager] currentAppleEvent]` is the
`kAEOpenApplication` event; when its `keyAEPropData` parameter's enum value is `keyAELaunchedAsLogInItem`, the launch
is a login item. `LaunchedAsLoginItem()` answers that flag. The shell (always deferred on darwin, §7.1) then creates
no window, C2 starts without a room, and update checks run, exactly as `--autostart` on Windows (03 §16.1).

### 12.3 Fallback (WM25 fails)

If the flag is never set on a real login (WM25), M3 switches to `SMAppService.agentServiceWithPlistName:
@"io.isshoni.desktop.autostart.plist"` with `Contents/Library/LaunchAgents/io.isshoni.desktop.autostart.plist`
(`Label` = `io.isshoni.desktop.autostart`, `BundleProgram` = `Contents/MacOS/isshoni`, `ProgramArguments` =
`["isshoni", "--autostart"]`, `RunAtLoad` = true, `LimitLoadToSessionType` = `Aqua`, `ProcessType` = `Interactive`),
which shows under "Allow in the Background"; 04 adds the plist to the bundle in that case. The decision is recorded
in WM25's row.

---

## 13. Credentials in the Keychain (`internal/client/credstore`, darwin)

### 13.1 The store

```go
// keychain_darwin.go
func OpenKeychain(o Options) (Store, error) // the login keychain unless o.Keychain names a file keychain (tests)
// Options gains (ignored elsewhere):
//   Service  string // kSecAttrService; "" = "io.isshoni.desktop" (the production bundle id; dev/e2e pass theirs)
//   Keychain string // path of a .keychain-db for tests (kSecUseKeychain on every call); "" = default
```

- One **generic password** per server: `kSecClass` = `kSecClassGenericPassword`, `kSecAttrService` = `Service`,
  `kSecAttrAccount` = `Prefix + leaf + ":" + origin` (`isshoni:v1:3f9a0c1e2b7d4a65:https://watch.example.com`; 02
  §11.5's key `isshoni:v1:<origin>` with the signing leaf inserted, see "Signing leaf" below), `kSecAttrLabel` =
  "isshoni: watch.example.com" (shown in Keychain Access), `kSecAttrDescription` = "isshoni sign-in", `kSecValueData` =
  the file store's blob (02 §11.5: the Windows blob plus `userName`), `kSecAttrSynchronizable` = false,
  `kSecUseDataProtectionKeychain` = false (the data protection keychain needs a keychain-access-groups entitlement,
  which needs a provisioning profile, i.e. the paid program). 02's `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`
  is dropped: it applies only to the data protection keychain.
- **No UI, ever**: every call passes `kSecUseAuthenticationContext` = an `LAContext` with `interactionNotAllowed =
  YES`. A call that would prompt fails with `errSecInteractionNotAllowed` instead.
- **Signing leaf** (`leaf`, 16 hex characters): computed once at `OpenKeychain` from the running code's own signature:
  `SecCodeCopySelf` → `SecCodeCopyStaticCode` → `SecCodeCopySigningInformation(…, kSecCSSigningInformation)`, first
  element of `kSecCodeInfoCertificates`, `SecCertificateCopyData`, SHA-256, first 8 bytes in lowercase hex. No
  certificate (ad-hoc or unsigned) → `OpenKeychain` returns `ErrUnavailable` and `appCredentialOptions` falls back to
  the file store with its WARN (as the unbundled dev build does). Why: an item's ACL trusts the identity that created
  it, so items keyed by the leaf are only ever read and written by that identity. After an identity change the new
  build simply finds no item for its own leaf (`ErrNotFound`, the user links again once) and its `Put` creates a fresh
  item whose ACL trusts only itself, instead of hitting `errSecDuplicateItem` on an item it isn't allowed to update
  (which would answer `save_failed` on every run).
- `Put`: `SecItemAdd`; `errSecDuplicateItem` → `SecItemUpdate` of data, label and description (same leaf, so the same
  ACL). `Get`: `SecItemCopyMatching` with `kSecReturnData`, `kSecMatchLimitOne`, the current leaf's account. `List`:
  `kSecMatchLimitAll` with `kSecReturnAttributes`, filtered by the current prefix `isshoni:v1:<leaf>:`, sorted.
  `Delete(origin)`: `SecItemDelete` of the current leaf's item, `errSecItemNotFound` → nil; then, best effort, every
  item of the service whose account is `isshoni:v1:<other leaf>:<origin>` (found by an attributes-only query, which
  returns no secret); a refused delete is logged at WARN ("an isshoni sign-in saved by another signing identity
  couldn't be removed; delete it in Keychain Access") and doesn't fail the call. `ForgetAll` does the same for every
  item with the `isshoni:v1:` prefix. Foreign items are never read.
- **Migration**: there is none to run. Items written by an earlier identity stay unread; the first start after an
  identity change finds no credential for any server, and each server shows 02's "link again" state (the
  `ErrNotFound` path). Tokens in the old items stay readable by the old identity until the user removes that device on
  each server; 04 §5.8's leak advisory says so (§21).
- **Errors**: `errSecItemNotFound` → `ErrNotFound`; an unreadable blob → `ErrNotFound` with a WARN (02's rule);
  `errSecInteractionNotAllowed` and `errSecAuthFailed` on a **read** → `ErrNotFound` with a WARN "the macOS Keychain
  refused isshoni's sign-in for one server; link again" (the user links again instead of seeing a password dialog);
  `errSecNoSuchKeychain`, `errSecNotAvailable` and a locked keychain on a write → `ErrUnavailable` (02's `save_failed`
  path: linked for this run only). Other `OSStatus` values are wrapped with their number only, never the data.
- Calls are serialized with a package mutex (03's Credential Manager lesson), never run on the main thread, and the
  same rules as every `Store` apply (02 §11.5: sizes, NUL, sorted `List`; the shared `credstore_test.go` table runs
  against it with a temporary keychain).
- `appCredentialOptions` (darwin): bundles (production, e2e) use `OpenKeychain` with `Service = macos.BundleID()`
  (the e2e build's random prefix stays, 03's `flavor.credPrefix`, placed before `isshoni:v1:<leaf>:`); the unbundled
  dev build, and any bundle without a signing leaf, keeps the file store with its WARN (an unsigned binary's Keychain
  ACL would change at every rebuild).
- `save_failed`'s text on macOS: "Couldn't save the sign-in to the macOS Keychain. You'll have to link again after
  restarting isshoni."

### 13.2 Will update N → N+1 still read the item?

The item is created by `SecItemAdd` in the login keychain, so its ACL trusts the creating app by its **designated
requirement** (`identifier "io.isshoni.desktop" and certificate leaf = H"…"`, 04's), which N+1 satisfies. Since macOS
10.12 an item also has a **partition list**; for code without an Apple team id it isn't documented whether the entry
is stable across a re-sign with the same identity. **KC decides it before any release** (§13.3).

A **different** identity (04 §5.2's identity change, §5.8's leak rotation, the later Developer ID move) has a
different leaf, so by §13.1's naming it never touches the old items' data: no prompt, one new link per server; KC's
step 5 proves it.

### 13.3 The KC check (CI `desktop-macos`, and the owner's Mac)

`tools/kccheck` (Go, darwin, `isshoni_kc` tag) with a script step in `desktop-macos`:
1. `sh deploy/macos/ci-identity.sh` (04's ephemeral CI identity, which writes `ci-requirement.txt`); KC's own part is
   only a second temporary keychain `kc.keychain-db` that becomes the default for this job only (`security
   default-keychain -s`; restored in a trap);
2. build a tiny bundle `io.isshoni.kccheck` (the store's code, `Service` = that id) twice with different content (two
   CDHashes), and sign each with `sh deploy/macos/sign.sh -mode ci -tool <kccheck.app> -tool-id io.isshoni.kccheck`
   (builds N and N+1; hardened runtime);
3. run build N: `Put` a credential; run build N+1 (via `open -W`, so LaunchServices starts it): `Get` must return it
   with no `errSecInteractionNotAllowed`; `List` and `Delete` work;
4. negative control: an ad-hoc signed build N+2 (`codesign -s -`), run with the test-only flag `-raw-account
   isshoni:v1:<leaf of N>:<origin>` (bypasses the leaf prefix; `kccheck` only), must fail to read N's item (the ACL
   works);
5. **identity A → identity B**: create a second ephemeral CI identity (`sh deploy/macos/ci-identity.sh -name
   kccheck-b`: the same script, another key, another leaf) and sign build N+3 with it. N+3's `Get` returns
   `ErrNotFound` (no prompt, no other error); its `Put` then succeeds and a second `Get` returns the new value (linking
   again works); N's item is still there unchanged (read back by build N); N+3's `ForgetAll` removes its own item and
   either removes N's item or logs §13.1's WARN, and never fails.

KC passing keeps D15's Keychain store. KC failing switches darwin bundles to the file store (`credentials.json`, 0600,
in the data root, its WARN removed for bundles), `docs/security.md` says so, and the owner is told in the PR (a
product decision, §23). The real-identity proof is 05's M-MAC-9 (the Keychain item is read without a prompt after an
N → N+1 update, on 27 and one VM) and M-MAC-8 (the update itself).

---

## 14. Settings and local storage

### 14.1 Where things live (macOS)

| What | Where | Owner |
|---|---|---|
| Data root | `~/Library/Application Support/isshoni/` (dev: `…/isshoni-dev/`) from `NSFileManager`; in e2e builds `<ISSHONI_E2E_DATA_DIR>/isshoni` when the variable is set (05 §6.3 launches the e2e app that way, so a CI run never touches the runner user's real root or `instance.lock`). `platform_darwin.go` reads the e2e override from the flavor field, renamed from `flavor.localAppData` to `flavor.dataDirOverride` and documented as used on both OSes (Windows: the `LocalAppData` replacement, unchanged); `TestE2ESettings` and the start tests gain a darwin case | this doc |
| Settings | `<root>/settings.json` (+ `.bak`, `.tmp`; 03 §14.2) | this doc |
| Servers | `<root>/servers.json` | 02 |
| Tokens | login keychain, generic passwords, service = bundle id, account `isshoni:v1:<leaf>:<origin>` (§13.1; or `<root>/credentials.json` if KC fails) | this doc (§13) |
| Logs, crash files | `<root>/logs/` (03 §17's names) | this doc |
| Single-instance lock; socket | `<root>/instance.lock`; `<per-user temp>/isshoni-<hex>.sock` | this doc |
| Pending link | `<root>/pending.json` (written by step 2a while 04's helper holds `updates/applying.lock`; read and deleted at step 6, §6.1) | this doc |
| Updates | `<root>/updates/` | 04 |
| WebKit data (Web Storage, cache, cookies none) | `~/Library/WebKit/<bundle id>/`, `~/Library/Caches/<bundle id>/`, `~/Library/HTTPStorages/<bundle id>*` | WebKit; purged by §6.4 |
| AppKit preferences | the `<bundle id>` defaults domain (`ApplePersistenceIgnoreState` = YES registered at start; nothing else is written) | AppKit; purged by §6.4 |
| Login item | Background Task Management (`SMAppService`), no file of ours | this doc |
| TCC grants, notification settings | the system's databases, keyed by bundle id and designated requirement | macOS; `tccutil reset` in purge |

### 14.2 `settings.json`

Schema 1 unchanged; per-OS defaults (`hotkeys`), `pill` ignored on macOS (kept), `closeToTray` labeled "menu bar".
`SettingsView` gains nothing; `SettingsPatch.autostart` on macOS may answer `io_error` with `params.reason`
`requires_approval` or `not_in_applications` (the setting is then saved as the OS reports, 03 §16.1).
**Managed update policy** (04 §7.2): when the first start creates `settings.json`, `updates.check` takes the value of
`CFPreferencesCopyAppValue("UpdateChecks", "io.isshoni.desktop")` if it is a `CFBoolean` (an administrator's profile
or `defaults write`), read **once**; any other type or absence keeps the default. Later starts don't read it again.

### 14.3 The Settings page on macOS (`/desktop/settings`)

| Section | macOS difference |
|---|---|
| Servers | none |
| Sharing | no "Sharing pill" row; hotkeys with macOS rules and symbols; "Video encoder: VideoToolbox (hardware)" from the probe; no OpenH264 |
| Audio | 03-macos-audio's list and tips (no Windows ducking tip) |
| **Permissions** `#permissions` (macOS only) | System Audio Recording (Allowed / Not allowed / Not asked yet / Unknown) with [Ask now] or [Open System Settings] and [Check again]; Screen Recording: "Not needed: you choose what to share in macOS's picker each time"; Local Network: "Used only for servers on your home network" with its last state and [Open System Settings]; Notifications with [Allow] or [Open System Settings]; [Quit and reopen isshoni]; help links `/download#macos-permissions` (what each grant is for) and `/troubleshooting#macos-permissions-reset` (the fix when one was denied) |
| App | "Open isshoni when I log in to this Mac" (+ approval and Applications rows, §12.1); "Keep isshoni running in the menu bar when I close the window"; "Hide isshoni windows from screen capture (recommended)" with the macOS note "On macOS 15 and later this hides isshoni from screenshots; whole-screen shares keep it out in another way"; "Show notifications"; no "Keep the tray icon visible"; Quit isshoni |
| Updates | help link `/download#macos-updates`; otherwise 04's/06's states, plus: `Status.reason` → `desktop.update.notInstalled.<reason>_macos` ("Move isshoni to Applications to get updates" for `translocated` and `read_only`; "isshoni can't update itself here. Install your own copy: open your server's Download page and run its Terminal command (curl … \| sh)" for `not_writable`, 04 §7.2; both with the `/download` link); the bridge codes `update.bad_archive`, `update.codesign_mismatch`, `update.app_management`, `update.downgrade` (added to `bridge/codes`, `codes.gen.ts` and `desktop.update.error.*`; `downgrade`: "isshoni didn't install this update because it isn't newer than the installed version" (04 §7.3 step 5); `app_management` links `/troubleshooting#macos-app-management`); "Turned off by your administrator" when `PolicyDisabled` (04 §7.2, §7.4, §7.6) |
| About | "macOS 27.2" instead of "Windows build"; "WebKit 621.1.15" instead of WebView2; "Apple silicon" / "Intel" (and "(Rosetta)" when `Translated()`); engine summary; Copy version info with those lines; Third-party notices → `Contents/Resources/licenses/THIRD_PARTY_NOTICES.txt` |

---

## 15. Logging

03 §17 unchanged, with: the home folder written as `~` (`applog.Options.Profile` = the home folder from
`getpwuid_r(getuid())`, never `$HOME`; the redaction token is `~` on macOS and `%USERPROFILE%` on Windows); the
`macos` component name; no bundle path, computer name or user name in any line (the computer name goes to the server
as the device name, 02, never to the log); native crashes of the engine produce macOS's own `.ips` report in
`~/Library/Logs/DiagnosticReports`, which "isshoni closed unexpectedly last time" mentions on macOS ("macOS saved a
crash report in Console → Crash Reports").

**Startup-budget lines** (new, all OSes, INFO, for 05 §8's startup budget): `start` at process start, with `mono_ns`
(monotonic nanoseconds since process start, from the process's start time) and `pid`; `window shown` at the first
`ShowWindow`, with `elapsed_ms` since process start. Both are written once per run; 05 reads them from the log.

---

## 16. SPA changes: every OS branch

The SPA learns the OS from `desktop.app.os` (`bridge.js`). `web/src/platform/desktop/os.ts` exports `desktopOS():
'windows' | 'macos'` and `osContext()` (`'macos'` or `undefined`) for i18next. Every branch:

| # | File | Branch |
|---|---|---|
| 1 | `platform/desktop/DesktopPlatform.ts` | `client.browser`: `'safari'` on macOS (WebKit), `'edge'` on Windows; `wizardSteps()` adds `'audio-permission'` on macOS |
| 2 | `platform/desktop/config.ts` | accepts `app.osVersion` (string, optional for N−1 bundles) |
| 3 | `platform/desktop/bridge.ts` | `AppInfo.osVersion`; `ShareCaps.osBuild` documented as Windows-only (0 on macOS, §8.1); `OSPage` union adds `audio-recording`, `screen-recording`, `local-network`, `login-items`; `ShareCaps.audioBackend` adds `'process-tap'` (03-macos-audio); `ShareView.selfVisible`; `PermissionsView`, `AudioPermissionResult`, `NativeBridge.permissions`, `app.relaunch`, `window.minimize`; event names `permissions.changed`, `app.command` |
| 4 | `platform/desktop/wails/WailsBridge.ts`, `http/HttpBridge.ts`, `bindings/` | the new methods (generated bindings, drift-checked) |
| 5 | `share/native/NativeShareSheet.tsx` | "…in the macOS picker…" (context) |
| 6 | `share/native/FirstShareSheet.tsx` | hotkey line via `Combo.display(os)`; the `selfVisible` line |
| 7 | `share/native/NativeSharePanel.tsx` | the two `selfVisible` status lines with [Minimize] (§10.2); the Local Network line under `unreachable` (§9.5); the `os_stopped` end line; the 4K choice (§10.6); the kind-based label when `label` is empty (§10.1) |
| 8 | `share/native/DndTip.tsx` | Focus text (context) |
| 9 | `share/native/engineErrors.ts` | §10.6's macOS rows (incl. `capture_restart_failed`, `no_hardware`, `no_run_loop`, `layer_failed: q`); `permission_denied` on macOS has no OS-settings button (the §10.6 text, [Try again] and the `/troubleshooting#macos-permissions-reset` link); Windows keeps `graphics-capture` |
| 10 | `share/native/audioText.ts`, `FriendsHear.tsx` | 03-macos-audio's banner codes and their actions (`audio-recording` page) |
| 11 | `viewer/endpointMute.ts` | `mutesViewer(fh, share)` and the second subscription (§10.2), OS-neutral; this doc owns the file |
| 12 | `desktop/SetupPage.tsx` | the `audio-permission` step; the `local_network` probe error |
| 13 | `desktop/AudioPermissionStep.tsx` (new) | macOS only |
| 14 | `desktop/SoundCheck.tsx` | `process-tap` success text; `permission_denied` → "macOS still blocks isshoni…" with [Back to the permission step]; `permission_pending` → the step's not-determined text |
| 15 | `desktop/PermissionsSettings.tsx` (new), `SettingsPage.tsx` | the `#permissions` section, macOS only |
| 16 | `desktop/AppSettings.tsx` | autostart label and states; "menu bar" wording; no tray-visibility row |
| 17 | `desktop/SharingSettings.tsx` | no pill row on macOS |
| 18 | `desktop/AboutSettings.tsx` | macOS version, WebKit, chip; Copy version info lines |
| 19 | `desktop/HotkeyField.tsx`, `desktop/hotkeys.ts` | `Cmd`, macOS rules and symbols, `aria-label` with spoken names |
| 20 | `desktop/UpdateParts.tsx` | `download#macos-updates`; `Status.reason` texts (`notInstalled.<reason>_macos`), the `bad_archive`, `codesign_mismatch`, `app_management`, `downgrade` errors, "Turned off by your administrator" (§14.3) |
| 21 | `desktop/authText.ts` | `save_failed` Keychain text (context) |
| 22 | `rooms/` Offline screen | Local Network line (§9.5), only when relevant |
| 23 | `rooms/DesktopLinks.tsx` (or `app/App.tsx`'s desktop shell) | handles `app.command` (settings, about, check-updates, confirm-quit) |
| 24 | `app/testing/desktop.ts`, `platform/desktop/testing/fakeBridge.ts` | a macOS fixture beside the Windows one |

Nothing else reads the OS. The entry chunk's 4 KB budget (03 §7.2): `os.ts` is two functions; the new step and
section are lazy chunks of `desktop/`. `chunks.node.test.ts` and `size.node.test.ts` keep their assertions.

---

## 17. i18n and accessibility

- **Context, not copies**: OS-specific texts are i18next contexts, `key_macos`, falling back to `key` (i18next's
  built-in `context` option; `t(key, {context: osContext()})`). Go's `internal/desktop/i18n.Catalog` gains `TFor(os
  string, key string, args …)` with the same rule, used for `desktop.native.*` (tray, menus, notifications, window
  notice). A test asserts every `_macos` key has a base key and that no base key's text names Windows-only things
  that macOS shows (a list of words: "PC", "tray", "Windows", "taskbar") unless a `_macos` key exists for it.
- New namespaces: none; new keys under `desktop.permissions.*`, `desktop.native.menu.*`,
  `desktop.setup.audioPermission.*`, `desktop.settings.permissions.*`, `desktop.share.label.*` (also under
  `desktop.native.*`), `desktop.share.panel.selfVisibleMuted`, `desktop.share.fourK.*`,
  `desktop.update.notInstalled.*_macos` and
  `desktop.update.error.{bad_archive,codesign_mismatch,app_management,downgrade}`.
- **VoiceOver**: the menu bar extra's label (§7.4); hotkeys announced with spoken names; the wizard step's states are
  `role="status"`; WKWebView exposes the SPA's ARIA as WebKit does in Safari (03 §18.2's checks re-run with VoiceOver
  on the owner's Mac, 05's M-MAC-18 / MD-M12).
- Respect `prefers-reduced-motion` and macOS's "Increase contrast" (`prefers-contrast: more`) in the existing CSS; the
  menu bar icons have a dark mark inside the yellow warn icon for contrast.

---

## 18. Windows unchanged: how CI proves it

- Every shared Go change is additive (new constants, optional interfaces, new bound service, `osVersion`), or keyed on
  `GOOS`/`hotkey.OS`. `winos` is untouched except `OSVersion()` and the per-OS `OSPage` check.
- `windows-go` runs the whole Go suite on `windows-2025` (unchanged job), including `hotkey`'s Windows round trips,
  `settings` defaults, `TestScrubEnv` with the Windows list, `bridge`'s golden JSON (`AppInfo` gains only
  `osVersion`, added to the goldens in the same PR).
- `windows-desktop` runs the CDP spec against the real `isshoni.exe` (W1–W21); the new `Permissions` service is
  answered on Windows (`not_needed`), and the CDP spec gains one assertion that `wizardSteps()` is still `['link',
  'sound', 'voice']` and `client.browser` is `'edge'`.
- The SPA's Windows snapshots (`share/native/testdata/sheets/*`, Settings component tests) are unchanged; the macOS
  variants are new files. `e2e` runs the desktop specs with the harness's default (`-os windows`) and once more with
  `-os macos` (§19.3).
- `lint-go` keeps its `GOOS=windows` pass and adds `GOOS=darwin CGO_ENABLED=1` in `desktop-macos` (runner from
  `deploy/macos/tools.json`, `macos-26`).
- Other shared changes and their Windows proof: `--version`'s `arch` key (the Windows golden gains it in the same
  PR); `ShareCaps.osBuild` still set from the probe on Windows (bridge golden unchanged for Windows, a new macOS case);
  `engine_fake.go`/`engine_release.go`'s added `imstatic` terms (Windows never sets the tag, so `windows-go` builds
  the same files for every flavor); `engineEnd`'s `cancelled` case and `ShareSnapshot.SelfVisible` (Windows' engine
  never sends either; core tests unchanged); `soundCheckBound` (10 s on Windows, its bridge tests unchanged);
  `flavor.dataDirOverride` (a rename, `TestE2ESettings`' Windows case unchanged); the `start`/`window shown` log lines
  (additive).

---

## 19. Test plan

### 19.1 Go unit tests

"CI macos" below is `desktop-macos` (runner from `deploy/macos/tools.json`, `macos-26`); the x86_64 runs of the
darwin packages are nightly under `arch -x86_64` on the same arm64 runner (D2). Every darwin test that skips on CI
does so with a 05 `macenv` message and has a line in `.github/macos-skips.txt` (05 V-D7).

| Package | Asserted | Runs on |
|---|---|---|
| `internal/desktop/macos` (pure parts) | `ClassifyLocalNetwork` table (OS major, address classes incl. CGNAT and ULA, `.local`, each errno); the pages table (every `OSPage` → URL and fallback, `audio-recording`'s order on 14.x vs 15+; Windows-only pages → `ErrNotSupported`); `pending.json` parsing (size cap, bad origin, bad room, foreign file deleted unread); the lock-file state machine (03 §13.3's `TestLockState` analog) | Mac, CI ubuntu/macos/windows |
| `internal/desktop/macos` (darwin) | `TestLockRepair`, `TestSecondInstanceForwards`, `TestTwoRootsIndependent`, `TestStaleSocketPath`, `TestWaitPID`, `TestForeignPeerRefused` (root only; else `macenv` skip + ledger); `TestOnMainWait` (inline on the main thread, bound 2 s); `TestKeepAwakeAssertion` (the named IOPM assertion exists, then not); `TestHotkeyRegister` (register, a second register of the same combo in-process → `ErrHotkeyTaken`, unregister, re-register); `TestDataRootNotFromEnv` (`HOME=/nonexistent` changes nothing); `TestResponsiblePID` (a child of the test is responsible to the test); `TestComputerName` (non-empty, ≤ 64 code points); `TestApplyingLockProbe` (held / free / missing folder / other error) | Mac, CI macos (a `TestMain` that runs an `NSApplication` on the main thread) |
| `internal/desktop/hotkey` | `ParseFor`/`ValidateFor`/`Display`/`Spoken` per OS; Windows refuses `Cmd`; macOS refuses Option-only letters; `MacKeyCode` table; fuzzed round trips per OS | everywhere |
| `internal/desktop/settings` | per-OS defaults; `pill` kept on macOS; autostart reasons; `UpdateChecks` read once on first start (a fake preferences reader: bool, non-bool, absent) | everywhere |
| `internal/desktop/tray` | `Render` with an empty label → "Sharing your screen · N watching" / "Sharing a window · N watching" (fixture) | everywhere |
| `internal/client/core` | `ShareSnapshot.SelfVisible` from `EventLayer`; `engineEnd`: `cancelled`/`os_stopped` → `EndUser`, `ByThisApp` false; the 4K override (fake engine refusing two-layer 4K60 with `layer_failed: q`; `FullFPSCap` caps `f` at 30; `NoPreview` omits `q` and publishes it inactive) | everywhere |
| `internal/desktop/share` | `selfVisible` rule table: screen/window × `Source.SelfVisible` × `EventLayer.SelfVisible`; auto-minimize once, not again after a restore: **for a pick flag before `Start` (the fake records minimize, then start) with the window restored on a `Start` failure**, for the session flag at `live` or when it turns true while live; `os_stopped` → `user` end; the `fourKChoice` state and both retries | everywhere (fake engine, fake OS hooks) |
| `internal/desktop/app` | the darwin start of the deferred shell (login item → no window); `TestDockVisible` (§7.2's table) and the call order (hide while live keeps `.regular`, the end of the share while hidden switches to `.accessory`, with a fake `DockPolicy`); `ShouldQuit` answers (live → false + `confirm-quit`; end-session reason → stop then true); `Quit(true)` from SIGTERM stops a live share; Local Network state transitions | everywhere (fake OS hooks) |
| `internal/desktop/bridge` | `Permissions.*` on fakes (Windows `not_needed`; macOS states mapped from `SoundCheckResult.Permission` and the report's `tap.permission`; **idle recovery**: fake engine idle (no watcher, no share), its preflight flipped from `denied` to `granted`, a key-window event → `permissions.changed` with `granted`; after a fresh start with no report, `State()` answers `granted`, not `unknown`; an engine error falls back to the cache; `RequestAudioCapture` busy while live; `WaitedMs`); `soundCheckBound`: a fake policy answering after 15 s on darwin succeeds, Windows keeps 10 s; `App.Relaunch` busy; `app.command` event; golden JSON with `osVersion` and `selfVisible`; a macOS golden with `ShareCaps.osBuild` 0 | everywhere |
| `internal/client/credstore` (darwin) | the shared `Store` table against `OpenKeychain` on a temporary keychain; error mapping (`errSecInteractionNotAllowed` → `ErrNotFound` + WARN); the account format `isshoni:v1:<leaf>:<origin>` and the leaf computation (pure part: certificate bytes → 16 hex characters; no leaf → `ErrUnavailable`); `List` ignores other leaves' items; `Delete`/`ForgetAll` try other leaves' items and only WARN on refusal; no secret in any error or log line; concurrent writes serialized | Mac, CI macos |
| `tools/kccheck` | KC (§13.3) | CI macos (`desktop-macos` step), owner's Mac |
| `cmd/isshoni-desktop` | `TestMainMenuByFlavor` (§7.3); start order with fakes (`--wait-pid`, step 2 skipped unbundled and with `--relaunched`, step 2a with `applying.lock` held → pending written and exit 0, URL events into `fwd` before `Boot`); `--version`'s `arch`; `--self-check` JSON shape on macOS (`name`, `os`, `archiveSHA`, `bundle`; `ok` independent of `encoders`); `--apply-update` dispatch (only argument, darwin only); `--purge-data` path list from a fake `NSFileManager` root; `TestScrubEnv` darwin list; `TestE2ESettings` darwin case (`ISSHONI_E2E_DATA_DIR`); the `start` and `window shown` log lines | everywhere for the pure parts; darwin parts on Mac, CI macos |
| `internal/protocol` | `TestDesktopOrigins`: `[1] == DesktopAssetOriginMacOS` | everywhere |

The Info.plist template, entitlements and icon are checked by 04's `desktoppkg` bundle golden (§4.4); this doc has no
test of its own for them. `desktop-macos` also runs the duplicate-ObjC-class `nm` check (§4.5).

### 19.2 The real app on macOS without a grant (CI `desktop-macos`, and the Mac)

`desktop-macos` gains, after its Go tests: build an **e2e-flavor bundle** (`task desktop:app:macos
VERSION=0.0.0-ci.<run> TAGS=e2e SIGN=ci`, 04's task; relies on 04's e2e variant) and run:
- `go test -tags isshoni_hw -run TestWailsChecklist ./cmd/isshoni-desktop` with `ISSHONI_CHECKLIST_BUNDLE`
  pointing at it: the Go-side WM rows only (WM4, WM10, WM11, WM12, WM19's env scrub), printing one annotation line per
  row (03 §3.2's WF19 format) into the job summary. It calls `macenv.Require(t, macenv.GUISession)` and needs no grant.
- **05's WK spec** (Playwright, `web/e2e/wk/`) against the same bundle, driven through **`e2edriver_darwin.{go,m}`**
  (`//go:build darwin && cgo && e2e`, §4.5): the in-app loopback driver of 05 §6.2 (random port and token,
  `POST /v1/key` for real `NSEvent` keys, `/v1/quit`, `state`) and the e2e-only bound service `E2E.Report`. The WK
  spec covers every page-side WM row and the end-to-end case (link, open a room, share the fake engine's pattern, a
  viewer receives frames, close while live, quit with the share gone ≤ 3 s, no home path in the logs). There is no
  separate `TestMacSmoke` and no `/__e2e/*` control in the app: one driver, one smoke (05 V9 builds on it). The bundle
  is launched with `ISSHONI_E2E_DATA_DIR=<temp>` (§14.1).
These need no TCC: nothing captures the screen or system audio.

### 19.3 SPA tests and the desktop-in-Chrome harness

- Vitest: every §16 branch has a component test with the macOS fixture beside the Windows one (wizard steps per OS,
  the permission step's five states, Settings → Permissions, the about lines, hotkey field rules and `aria-label`,
  `selfVisible` lines, engine error rows per OS, i18n context fallback).
- `internal/desktop/e2ehost` gains `-os macos` (`BridgeConfig.App.OS`, the macOS hotkey defaults, a fake `Permissions`
  with `/__e2e/permissions?audio=denied|granted|not_determined&notifications=…` and a fake `selfVisible` source). The
  `e2e` job runs `desktop-link.spec` (with the permission step), `desktop-share.spec` and a new
  `desktop-macos.spec` (permission step states and recovery, Settings → Permissions, the `selfVisible` line and
  [Minimize] call, the Local Network probe error, `app.command` navigation) under `-os macos` in Chrome on ubuntu.
  WebKit-specific behavior is §19.2's and §19.4's.

### 19.4 Tests that need a TCC grant (`isshoni_hw`, the owner's Mac through 05's Dev Harness)

These tests are built under `isshoni_hw` in CI too (the checklist needs the tag), so they gate themselves at run time
by 05's rules (V-D5–V-D7): each calls `macenv.Require` first, CI never declares a grant (`ISSHONI_TEST_TCC` unset), so
they skip there with a `macenv:` message and a ledger line in `.github/macos-skips.txt`; CI's `-run` filter also
leaves them out. On the owner's Mac they run as `go test -c` binaries **inside 05's signed `isshoni Dev Harness.app`**
(`io.isshoni.dev.harness`, the dev identity) through the new suite **`task mac:hw:desktop`** (added to 05 §5.4's
`task mac:hw`); the harness holds the grants, so a declared grant that is missing **fails** (V-D6). The engine they
use is the real one: their binaries are built with `-tags isshoni_hw,dev,imstatic` (§6.4), so `dev` doesn't swap in
the fake. Reset a grant with `tccutil reset AudioCapture io.isshoni.dev.harness`.

| Test | `macenv.Require` | Asserted |
|---|---|---|
| `TestAudioPermissionFlow` | `tcc:audio`; the Don't Allow and unanswered cases also `human` | `Permissions.RequestAudioCapture()` answers `granted` within 2 s with the grant; with the grant reset and the owner clicking Don't Allow (`human`) it answers `denied`; left unanswered (`human`), `not_determined` at 30 s (watchdog), within the 40 s bound |
| `TestSoundCheckMac` (this doc's) | `tcc:audio`, `hw:audio-out` | the bridge's `Audio.SoundCheck` answers `{captured: true, backend: "process-tap", permission: "granted"}` |
| `TestNotificationsRoundTrip` | `gui`, `human` (the owner allows notifications once) | a notification with buttons is delivered; an action reaches the app; `RemoveToasts` removes it |
| `TestLoginItemRoundTrip` | `gui`, `human` (registers a login item on the owner's account) | register → `enabled` or `requires-approval`; unregister → `disabled` |

The gate differs on purpose from 03-macos-audio's native tests (`ISSHONI_TCC_AUDIO=1`, ctest label `tcc`, grant on
the terminal app): those exercise the tap in isolation; these exercise the app's bridge and services as the harness's
responsible process. `internal/desktop/...`, `internal/client/credstore` and `tools/kccheck` are added to 05's
`skipcheck` scoped packages, `desktop-macos` runs `skipcheck`, and these tests join 05 §5.2's table.

### 19.5 Manual checks (owner's Mac on 27; VMs on 14.4, 15, 26 per 05; recorded in PRs and the release checklist)

Slice-level checks carry this doc's own ids (`MD-M`); the matrix ids are 05 §7's `M-MAC-*`, run at exit.

| # | Step | Expect | 05 matrix row |
|---|---|---|---|
| MD-M1 | Fresh install (04's curl path), first run | wizard: link → permission step (macOS dialog after Continue) → sound check ✓ → voice → room; no Dock flash at an `--autostart` start | M-MAC-1 |
| MD-M2 | Share a window via the picker (window first), then the whole screen; on a 15.0/15.1 VM too; **with isshoni's window centered over the target window** | isshoni absent from the picker; **the target can be picked without moving isshoni's window, and isshoni's window is back (not minimized) for the first-share sheet after the pick or a cancel**; live; where 02 reports neither self-visible flag, isshoni's window isn't in the stream; where it does, the window minimizes, viewer audio is muted and the panel's `selfVisibleMuted` line explains it; before 15.2 the panel and the tooltip say "your screen" / "a window" | M-MAC-5, M-MAC-6 |
| MD-M3 | During a self-visible whole-screen share, open isshoni from the Dock | the "Friends can see this window" line; [Minimize] works | — (part of M-MAC-6) |
| MD-M4 | Stop from macOS's own screen-sharing indicator | "You stopped sharing from the macOS menu bar"; no error notification | M-MAC-7 (system stop) |
| MD-M5 | Hotkeys ⌃⌥⌘P / ⌃⌥⌘S with a full-screen game in front | pause/resume and stop work; the menu bar item shows amber/idle | M-MAC-7 |
| MD-M6 | Close while live, quit with ⌘Q while live, log out while live | hide + one notification naming the menu bar icon, the Dock icon and the Stop hotkey; **after the close the Dock icon is present and clicking it shows the window; after Stop with the window hidden, the Dock icon goes away**; "Stop sharing and quit?"; logout isn't cancelled and the share is gone at the viewer | M-MAC-14 |
| MD-M7 | Lock the screen 1 min, 6 min; close the lid | 03's lock rules; sleep stops the share with the notification after wake | M-MAC-12 |
| MD-M8 | `isshoni://` links from Safari and Chrome, app running and not running | 03 §13.2's table (WM26) | M-MAC-10 |
| MD-M9 | Local Network on 15+: a server on the LAN, and a LAN viewer (mDNS host candidates) | probe fails with the Local Network text; Allow; Try again succeeds; the LAN viewer connects (else MD11 adds `NSBonjourServices`, §9.5); the grant survives an N → N+1 update | M-MAC-16 |
| MD-M10 | Deny System Audio Recording, then fix it from Settings → Permissions | §9.4's steps restore sound without reinstalling (relaunch if needed); the pane opened is the right one for the OS version (WM24) | M-MAC-4 |
| MD-M11 | Autostart on, log out and in | no window, menu bar item idle, C2 online without a room (WM25) | M-MAC-11 |
| MD-M12 | VoiceOver through wizard, share panel, menu bar item | every control named; hotkeys spoken | M-MAC-18 |
| MD-M13 | Update N → N+1 (04's updater) | still linked (Keychain read without a prompt), grants and notification settings kept | M-MAC-8, M-MAC-9 |
| MD-M14 | WM rows marked Mac in §3.2 | as the table says | (rows cite their own M-MAC ids) |

---

## 20. Interfaces other docs rely on

| Export | Where | Consumers |
|---|---|---|
| `protocol.DesktopAssetOriginMacOS = "wails://localhost"`; no server change (WM1 decides the `null` fallback) | §8.5 | 02's wiring (unchanged), 05 |
| Run-time requirements on 04's `Info.plist.tmpl`: bundle ids `io.isshoni.desktop` (production), `.dev`, `.e2e`; `LSUIElement` = true; `NSQuitAlwaysKeepsWindows` = false; `CFBundleURLTypes` only in production; `NSAllowsLocalNetworking` only in dev/e2e; `NSBonjourServices` unset unless MD-M9 fails. 04 owns the template (`@…@` placeholders), entitlements, `AppIcon.icns` and the bundle golden; this doc owns `build/darwin/tray/*.png` | §4.4 | 04 (template, assembly, signing, designated requirement), 03-macos-audio (bundle id for self rules), 05 |
| `CFBundleURLTypes` only in production bundles; the scheme is registered by LaunchServices at the first `open` launch (no `lsregister` assumed; WM26) | §6.3 | 04 |
| Command line on macOS: `--open-url` (also synthesized from `kAEGetURL`), `--server`, `--autostart`, `--relaunched`, **`--wait-pid=<pid>`** (wait ≤ 10 s before single instance; passed only by `App.Relaunch`), `--updated-from=<v>` | §6.1 | 04: its helper relaunches with `open -a <bundle> --args --updated-from=<v>` after the old process has exited (no `--wait-pid` needed; `open` makes isshoni its own responsible process, so step 2 is a no-op) |
| Step 2a: `flock(<root>/updates/applying.lock, LOCK_EX\|LOCK_NB)` probe before single instance; when held, the start writes `pending.json` and exits 0 (04 §2.4) | §6.1 | 04 (helper holds the lock; updater e2e case F) |
| One-shot modes on macOS: `--version` (+ `arch`, all OSes); `--self-check=<path>` without `ffmpeg*` keys, with `engine` `{loaded, abi, testHooks, name, os, archiveSHA, encoders}` and `bundle` `{id, path}`, `ok` independent of `encoders` and of TCC; `--forget-credentials`; `--purge-data` with §6.4's path list; **`--apply-update=<path>`** (darwin only, only argument, `update.RunApplyHelper`, exit 0/1); `update.CleanupStaging(log)` early at every start (step 7) | §6.4, §6.1 | 04 (`install-desktop.sh --uninstall/--purge`, packaging checks, updater), 05 |
| Engine selection: `engine_fake.go` `(windows \|\| (darwin && cgo)) && (dev \|\| e2e) && !imstatic`; `engine_release.go` `(windows \|\| (darwin && cgo)) && ((!dev && !e2e) \|\| imstatic)`; `engine_noengine_darwin.go` fails a `darwin && cgo && production && !imstatic` build; release builds use `-tags production,imstatic` through 04's task | §6.4 | 02 (§6.4's sentence), 04 |
| `SIGTERM` = `App.Quit(true)` (stops a live share without confirm-quit, then the shutdown path; second `SIGTERM` or 5 s → exit 0) | §7.5 | 04 (`macos-runtime` ends the app this way) |
| `<root>/pending.json` `{"server","room"}` (≤ 4 KiB): written only by step 2a, read and deleted at step 6 | §6.1 | this doc only; 04's installer hands its server over as an `isshoni://<host>/` link through `open -a` (`kAEGetURL`) |
| Data root `~/Library/Application Support/isshoni/` with `updates/` for 04; `instance.lock`; the WebKit, Caches, HTTPStorages, Saved Application State and defaults paths; in e2e builds `<ISSHONI_E2E_DATA_DIR>/isshoni` | §14.1 | 04 (uninstall/purge), 05 §6.3 (e2e launch), 03-macos-audio (none) |
| LaunchServices relaunch guarantees `ResponsiblePID(isshoni) == isshoni` for every normal start; `audio.SelfInfo{PID: os.Getpid(), WebViewDataDir: "", Exe: soundCheckExe() ("" on darwin), RulesOS: runtime.GOOS}` set by `cmd/isshoni-desktop/main.go`'s `newAudioService` (changed: sets `RulesOS`, passes `webviewData` "" on darwin); no `--sound-check-helper` child on macOS | §6.1, §8.2, §4.2 | 03-macos-audio (self rule by responsible PID; the engine plays the chime) |
| `PickRequest{OwnerWindow: NSWindow*, Kinds: nil}`; the app sets `sharingType` on its windows; the app reads `Source.SelfVisible` (`IM_SRCF_SELF_VISIBLE` 0x8) and `EventLayer.SelfVisible` (`IM_SESS_SELF_VISIBLE` 0x4) and has no OS-version branch of its own; `ENDED{cancelled, os_stopped}` → `EndUser`; `no_encoder`/`layer_failed: q` → the 4K choice; an empty `Source.Name` → a kind-based label | §10.1, §10.2, §10.3, §10.6 | 02 |
| Shared core changes: `ShareSnapshot.SelfVisible`; `engineEnd`'s `cancelled` case; `core.ShareRequest.Override` (`FullFPSCap`, `NoPreview`) in `config()` | §10.2, §10.3, §10.6 | 02, 05 |
| `ShareView.selfVisible`; `web/src/viewer/endpointMute.ts` (owned here): `mutesViewer(fh, share) = fh.live && (fh.engine === 'endpoint' \|\| fh.selfAudible === true \|\| share.selfVisible)`; the panel line `desktop.share.panel.selfVisibleMuted` (no `viewerMuted_macos`) | §10.2 | 03-macos-audio (adds `FriendsHear.selfAudible` to `bridge.ts`; its `viewer-muted` banner keeps the generic text) |
| `Permissions` bound service, `PermissionsView`, `AudioPermissionResult`, `permissions.changed`; `App.Relaunch`; `Window.Minimize`; event `app.command`; bridge code `local_network`; `soundCheckBound` 40 s on darwin for `Audio.SoundCheck` and `Permissions.RequestAudioCapture` | §9.2, §9.5, §7.3 | SPA, 03-macos-audio (supplies `SoundCheckResult.Permission` and the report's `tap.permission`, read by the new `audio.Service.PermissionState(ctx)` through one bounded `eng.AudioReport` call even while idle), 05 |
| `OSPage` values `audio-recording`, `screen-recording` (docs and tools only; no UI opens it), `local-network`, `login-items` and §9.4's URL table (`audio-recording` ordered by OS version) | §9.4 | 03-macos-audio (banner actions), 05 |
| The wizard's `audio-permission` step before `sound`; the sound check's `process-tap`, `permission_denied` and `permission_pending` mapping | §9.3 | 03-macos-audio |
| Keychain layout (service = bundle id, account `isshoni:v1:<leaf sha256[:8] hex>:<origin>`, login keychain, no UI; an identity change means linking again, never a prompt) and the KC check result (incl. the identity A → B case) | §13 | 02 (§11.5's write-back), 04 (uninstall: `--forget-credentials`), `docs/security.md` |
| `updates.check` seeded once from the `UpdateChecks` managed preference; `Status.reason` texts; bridge codes `update.bad_archive`, `update.codesign_mismatch`, `update.app_management`, `update.downgrade` | §14.2, §14.3 | 04 |
| Project-site anchors: `/download#macos-updates`, `/download#macos-permissions`, `/troubleshooting#macos-permissions-reset`, `/troubleshooting#macos-encoder`, `/troubleshooting#macos-local-network`, `/troubleshooting#macos-app-management` | §9, §10.6, §14.3 | 04 (site pages and `TestDocsAnchors`; asked to add `#macos-encoder` and `#macos-local-network` to `docs/troubleshooting.md`, §15's anchor list and `TestDocsAnchors`) |
| `e2edriver_darwin.{go,m}` (`darwin && cgo && e2e`): 05 §6.2's driver; the e2e-only bound service `E2E.Report`, registered only by `flavor_e2e` on darwin, excluded from binding generation (committed bindings unchanged, WM15), its method id added only to the e2e build's `GuardWKWebView` allowlist; no `e2edriver` symbol in production | §4.5, §19.2 | 05 (WK spec, V-D8, E1) |
| Log lines `start` (`mono_ns`) and `window shown` (`elapsed_ms`); `TestMainMenuByFlavor` (production has no View/Reload/Inspector) | §15, §7.3 | 05 (§8's startup budget; WM9's menu check) |
| CI: `desktop-macos` (runner from `deploy/macos/tools.json`, `macos-26`) gains the darwin lint pass, the e2e-bundle build, `TestWailsChecklist` (Go-side rows), the WK spec (05), KC, the duplicate-ObjC-class `nm` check and `skipcheck`; `e2e` gains the `-os macos` run; nightly runs the x86_64 tests under `arch -x86_64` on the arm64 runner; the built slices run in 04's `macos-runtime` (`macos-15`, `macos-15-intel`) | §18, §19 | 04 (owns `.github/workflows/*`), 05 |
| Grant tests: `TestAudioPermissionFlow`, `TestSoundCheckMac`, `TestNotificationsRoundTrip`, `TestLoginItemRoundTrip` behind `macenv.Require`, run by `task mac:hw:desktop` inside the Dev Harness | §19.4 | 05 (§5.2's table, §5.4's task, `skipcheck` scope) |

---

## 21. Depends on

The shapes below are what this doc assumes; each sibling's names win, and the adapters in
`internal/desktop/{app,share,bridge}` map them.

**02-macos-engine.md**
- `native.Open(native.Options{…})` on darwin (built with `imstatic`) returns the statically linked engine implementing
  `media.Engine` and `media.AudioController` (with 03's audio); `Info().ABI == media.ABIVersion`, `Info().Name ==
  "darwin"`, `BuildInfo().OS == "macos"`; `func StaticArchiveSHA() string` exported from `internal/media/native`
  (`archive_darwin.go`; `""` in `engine_other.go` and on Windows), listed in 02 §18 "For 01" and "For 04".
- `Engine.Pick(ctx, PickRequest)` presents `SCContentSharingPicker` with `excludedBundleIDs = [<own bundle id>]`,
  window first when `Kinds` is nil, `allowsChangingSelectedContent = NO`; returns `ErrCancelled` on cancel or timeout.
  Before 15.2 a pick carries no identity (02 §8.2.1: `Name`, `PID`, `Exe`, `MonitorID` empty).
- **`IM_SRCF_SELF_VISIBLE 0x8`** → `media.Source.SelfVisible` (true = isshoni's own windows will appear in this
  display capture; set only on display picks, by 02's per-OS strategy table, §8.8) and **`IM_SESS_SELF_VISIBLE
  0x4`** → `media.Event.SelfVisible` on `EventLayer` (a rebuilt filter SCK refused at `im_start`, so the picker's
  original filter is used). `Source.OSBorder` and `NoOSIndicator` are always false on macOS. The fake engine's
  `EngineConfig.SelfVisible` drives both in tests.
- `IM_SRCF_SELF` for a pick of an isshoni window (→ `invalid`/`self_window`, 03's row).
- A stop from macOS's own UI ends the session with **`ENDED{IM_ERR_CANCELLED, os_stopped}`** (02 §5.1, §8.9
  `UserStopped`); 02 §5.4's UI column points to §10.6 for the string. No content-change event exists in M3.
- `ENDED{SOURCE_GONE, capture_restart_failed}`, mid-share `ENDED{PERMISSION_DENIED, capture_denied}`, `no_encoder`
  `no_hardware`, `IM_ERR_BAD_STATE` `no_run_loop`, and `IM_ERR_NO_ENCODER` `layer_failed: q` for a two-layer start
  over the budget (an inactive `q` still counts), rendered by §10.6.
- Probe: `encoders` lists `videotoolbox` (possibly empty on a VM); `unsupported_os` below 14.4; `OS.Build` is the
  packed version, which the bridge doesn't forward on macOS (§8.1).
- Objective-C class prefix `IMMac` (02 §6.2); this doc uses `ISM`/`ism_` (§4.5).
- `dev`/`e2e` keep the fake engine unless `imstatic` is also set (02 §6.4).
- Toolchain: the engine archive needs CLT/SDK ≥ 26, so universal `production,imstatic` links run on `macos-26`.

**03-macos-audio.md**
- No permission API of its own: the System Audio Recording prompt appears when the first tap is built (§13.2); the
  sound check returns `SoundCheckResult.Permission` (`unknown` | `pending` | `granted` | `denied`) and `Reason`
  `permission_denied` | `permission_pending` | …, with `Backend` `"process-tap"`; the audio report carries
  `tap.permission`. The darwin sound check takes ≤ 35 s (§13.3, §14.1), within this doc's 40 s bridge bound.
- `FriendsHear.selfAudible bool` (isshoni's own playback can't be kept out); 03 §14.4 adds it to `bridge.ts` and
  refers to §10.2 for `mutesViewer`. Its `viewer-muted` banner keeps 05's generic text.
- The `audio-permission` banner: warning level, `params.cause` `pending` | `denied`, texts
  `desktop.audio.banner.audioPermission*` (03 §14.5), action naming `OSPage` `audio-recording`; no `audio.alert`
  notification for it.
- `audio.SelfInfo` on macOS: `PID`, `WebViewDataDir` `""`, `Exe` `""` (the engine plays the chime, `helper_pid` 0),
  `RulesOS` `"darwin"`.
- Its native grant tests are gated by `ISSHONI_TCC_AUDIO=1` and ctest label `tcc` (grant on the terminal app); this
  doc's are gated by `macenv` in the Dev Harness (§19.4), on purpose.

**04-macos-release.md**
- `task desktop:app:macos VERSION= [TAGS=production|dev|e2e] [SIGN=none|ci|dev] [OUT=]` builds a universal (or
  host-arch, for dev) bundle (relies on 04 adding the `e2e` variant; extra tags such as `imstatic` pass through
  `TAGS`); release signing happens only in the `macos-sign` job. `deploy/macos/ci-identity.sh` and `sign.sh -mode ci
  -tool <bundle> -tool-id <id>` (KC, §13.3); `ci-identity.sh` takes `-name <n>` so one job can create a second,
  independent ephemeral identity (KC step 5).
- 04 §5.2 and §5.8 match §13.1's leaf-keyed Keychain items: a change of identity re-prompts for the TCC grants only;
  for the Keychain it means the user links each server again (no Keychain prompt). The leak plan's advisory tells users
  that tokens stored before the rotation stay readable by the leaked key, and to remove the "isshoni on Mac" device on
  every server and link again.
- The single owner of `Info.plist.tmpl`, the entitlements, `AppIcon.icns`, the bundle golden; it honors §4.4's
  run-time requirements (asked: `LSUIElement` = true, `NSQuitAlwaysKeepsWindows` = false, no URL types in dev/e2e).
- `install-desktop.sh` hands its server over as an `isshoni://<host>/` link through `open -a`, delivered as
  `kAEGetURL` (04 §6.3); it never writes `pending.json`, and calls `lsregister -f` only on a copy it installed to
  `~/Applications` beside an admin's copy in `/Applications` (04 §6.3 step 4), so the link opens the user's copy.
- The updater's helper (`--apply-update`, `update.RunApplyHelper`, `update.ApplyHelperFlag`) holds
  `<root>/updates/applying.lock` during the swap, waits for the old pid (kqueue, 30 s) and relaunches with `open -a
  <bundle> --args --updated-from=<from>` (04 §7.3 step 7); `update.CleanupStaging(log)` is called by this doc at step
  7; `Status.Reason` values (`translocated`, `read_only`, `not_writable`), `PolicyDisabled`, and the error codes
  `bad_archive`, `codesign_mismatch`, `app_management`, `downgrade` (04 §7.2–§7.6).
- Uninstall calls `--forget-credentials` (and `--purge-data` for a purge); `macos-runtime` ends the app with
  `SIGTERM` and asserts `--version`'s `arch` and `--self-check`'s `bundle.id`.
- Site anchors `/download#macos-permissions`, `/troubleshooting#macos-permissions-reset`, `#macos-app-management`,
  plus the two this doc asks for (`#macos-encoder`, `#macos-local-network`, §20).
- `release.yml`/`ci.yml` changes listed in §20's CI row; `desktop-macos` on the runner of `deploy/macos/tools.json`.

**05-exit-and-verification.md**
- `internal/testenv/macenv` (`Require`, the needs `tcc:audio`, `gui`, `human`, `hw:audio-out`), the ledger
  `.github/macos-skips.txt` and `tools/skipcheck` (asked to add `internal/desktop/...`, `internal/client/credstore` and
  `tools/kccheck` to its scope and a step to `desktop-macos`); the Dev Harness and `task mac:hw` (asked to add
  `mac:hw:desktop`).
- The WK spec and its driver protocol (05 §6.2), which this doc hosts in the e2e build; `ISSHONI_E2E_DATA_DIR`.
- Runs the M-MAC matrix (§7) on the owner's Mac and the 14.4/15/26 VMs, which this doc's MD-M rows map to (§19.5);
  the real-identity Keychain proof is M-MAC-9 (+ M-MAC-8). Records WM rows marked "all". WM9's menu check cites
  `TestMainMenuByFlavor`.

**02-device-auth (M2)**
- `credstore.Store` unchanged; `Options` gains `Service` and `Keychain` (darwin only); the 02 §11.5 text is written
  back with §13's layout (account `isshoni:v1:<leaf>:<origin>` on macOS; Windows' `isshoni:v1:<origin>` unchanged)
  and KC's result.

---

## 22. Implementation slices

Ordered; each is testable on its own. Sizes: S ≈ 1–2 days, M ≈ 3–4 days, L ≈ 5+ days. Ids `MD` ("macOS desktop").

| # | Slice | Size | Needs | Acceptance | Verified on |
|---|---|---|---|---|---|
| MD1 | **Interfaces first**: `internal/desktop/macos` with its whole public API (§5.2) as darwin stubs answering `ErrNotSupported` plus `*_other.go`; `app` additions (§5.1: `OSPage` values, optional interfaces, per-OS page validity); `protocol.DesktopAssetOriginMacOS` + `TestDesktopOrigins`; `hotkey.OS` API declared; bridge `Permissions` service, `App.Relaunch`, `Window.Minimize`, `app.command`, `local_network` code, `ShareView.selfVisible`, `AppInfo.osVersion` (all `not_implemented` or zero), bindings regenerated; depguard rule | M | — | builds and tests on linux, darwin, windows; Windows goldens change only by `osVersion`; `windows-go` and `windows-desktop` green; bindings drift check passes | Mac, CI ubuntu/macos/windows |
| MD2 | **Main thread, paths, system facts**: `OnMain`/`OnMainWait`, the event callback, `DataRoot`, `TempDir`, `BundleID`, `InBundle`, `OSVersion`, `ComputerName`, `WebKitVersion`, `Translated`, `ResponsiblePID`; `platform_darwin.go` uses them (data root, device name, `osVersion`, `~` redaction) | M | MD1 | §19.1's darwin tests for these; `TestDataRootNotFromEnv`; the dev build still runs with `task desktop:dev`; the `ISM`/`ism_` prefix and the duplicate-ObjC-class `nm` check in `desktop-macos` | Mac, CI macos |
| MD3 | **Bundle inputs**: the menu bar PNGs (`build/darwin/tray/`) and the per-flavor ids and run-time plist values (§4.4) consumed by 04's MR1 template | S | MD1; 04's MR1 | 04's bundle golden contains §4.4's required values; 04's task builds a dev bundle that launches with no Dock icon until the window shows | Mac, CI macos |
| MD4 | **Start order on macOS**: step 2 LaunchServices relaunch, step 2a (`applying.lock`, `writePending`), `--wait-pid`, single instance (lock + socket, repair), `pending.json`, the URL Apple events into `fwd`, the darwin one-shot modes (§6.4: `--version` `arch`, `--self-check` fields, `--apply-update`), `update.CleanupStaging` at step 7, the engine build constraints and `engine_noengine_darwin.go`, `flavor.dataDirOverride` on darwin, the `start`/`window shown` log lines | L | MD2, MD3; 04's `update` helper API | §6.2's tests; start-order tests with fakes (step 2a held → exit 0 with pending written); a terminal start of the dev bundle relaunches (log line) and its WebKit GPU process's responsible PID is isshoni's; `open isshoni://…` against a production-flavor bundle reaches `Boot` and `HandleArgs` (WM26, also right after a fresh install); `go build -tags production` on darwin without `imstatic` fails; Windows goldens change only by `arch` | Mac, CI macos, windows |
| MD5 | **Window, Dock, menus, menu bar extra, quit and system events**: activation policy, main menu with `app.command`, tray icons and label and accessibility label, close/⌘Q/logout (`ShouldQuit` with the quit reason), lock/sleep/wake/power-off/screens/session events, keep-awake | L | MD2 | `TestKeepAwakeAssertion`; app tests for `ShouldQuit` and SIGTERM = `Quit(true)`; `TestDockVisible`; `TestMainMenuByFlavor`; WM7, WM14, WM18, WM22 in CI (WK spec); MD-M6, MD-M7 | Mac, CI macos |
| MD6 | **WKWebView settings, navigation layer 3, env scrub and the macOS test driver**: §8.2's options, §8.3, §8.4; `TestWailsChecklist` Go-side rows; **`e2edriver_darwin.{go,m}`** and `E2E.Report` (e2e only, excluded from bindings, e2e-only Guard allowlist); the `desktop-macos` e2e-bundle step; **WM1's Origin result recorded first** (decides whether a server change is needed). 05's V9 (WK spec) builds on this driver | M | MD3, MD4 | every WM row marked CI recorded in the PR and in §3.2; WM1 and WM2 results written back here; bindings drift check unchanged; the production binary has no `e2edriver` symbol | CI macos, Mac |
| MD7 | **Keychain store and KC**: `keychain_darwin.go/.m`, `Options.Service/Keychain`, the shared store table on a temp keychain, `tools/kccheck` and its CI step, `appCredentialOptions` darwin | M | MD3 | store tests green in `desktop-macos`; KC passes with 04's CI identity, including step 5 (identity A → B: `ErrNotFound`, then `Put` and `Get` succeed) (or the fallback is taken and the owner told); no secret in logs | CI macos, Mac |
| MD8 | **Hotkeys**: `hotkey` per-OS API and rules, Carbon registration, `typesChar` with `UCKeyTranslate`, per-OS settings defaults, `HotkeyField` and `hotkeys.ts` macOS rules and symbols | M | MD2 | `hotkey` fuzz and tables per OS; `TestHotkeyRegister`; SPA field tests; MD-M5 | Mac, CI all, owner's Mac |
| MD9 | **Autostart**: `SMAppService` main app service, status mapping, `requires-approval` and Applications rows, login-item launch detection, deferred shell on darwin | M | MD4, MD5 | app tests (login item → no window); `TestLoginItemRoundTrip` (`task mac:hw:desktop`); WM25/MD-M11 on a real login (fallback §12.3 if it fails) | owner's Mac |
| MD10 | **Notifications**: Wails' service on darwin bundles, categories and nonces, foreground presentation rule, `RemoveToasts`, `_macos` texts | M | MD3, MD5 | app toast tests unchanged and green; `TestNotificationsRoundTrip` (`task mac:hw:desktop`); WM8 | owner's Mac |
| MD11 | **TCC UX**: `Permissions` service over the audio service's sound check and report (§9.2), `soundCheckBound` 40 s on darwin, and the notification state, `permissions.changed` on key-window, `localnet` classification and the `local_network` probe code, `App.Relaunch`, §9.4's page table; SPA: `AudioPermissionStep`, `PermissionsSettings`, Offline and panel Local Network lines, SoundCheck mapping; e2ehost `-os macos` with fake permissions and `desktop-macos.spec` | L | MD1, MD2, MD4; 03-macos-audio's permission slice | Vitest for every state (incl. "Checking…" for `unknown`); `desktop-macos.spec` green in CI; bridge tests (15 s fake answer on darwin; idle denied → granted on key-window via `PermissionState`); `TestAudioPermissionFlow` and `TestSoundCheckMac` (`task mac:hw:desktop`); MD-M1, MD-M9 (incl. the LAN viewer and the `NSBonjourServices` decision), MD-M10; WM24 on every OS version | Mac, CI ubuntu, owner's Mac and VMs |
| MD12 | **Share flow on macOS**: no window activation before `Pick` on darwin, `share.PickPreparer` (darwin adapter) per EM8's record (minimize or order back, restore after pick/cancel/timeout); the kind-based label for identity-less picks (SPA and `tray.Render`); `selfVisible` from `Source.SelfVisible` and `EventLayer.SelfVisible` (core `ShareSnapshot.SelfVisible`), auto-minimize once (before `Start` for a pick flag, restored on a `Start` failure; at live for the session flag), `Window.Minimize`, the panel's `selfVisibleMuted` line, `mutesViewer(fh, share)`; core `engineEnd`'s `cancelled` case (`os_stopped` → `user`); §10.6's rows; the 4K choice (`core.ShareRequest.Override`, `fourKChoice`); DND/Focus tip text; SPA branches 5–11 | M | MD5, MD11; 02's pick slice (`SelfVisible`, EM9), 03-macos-audio's `selfAudible` | core tests (`SelfVisible`, `cancelled` end, 4K override with a fake refusing two-layer 4K60); share-controller tests for the rule table and `fourKChoice`, for darwin's no-activate pick (a fake window implementing `PickPreparer`: no `ShowWindow` before `Pick`; `restore` runs on success, cancel and timeout; a fake without it still gets `ShowWindow(true)`, Windows' path); tray fixture with an empty label; Vitest per branch; the WK spec's end-to-end share in CI; MD-M2, MD-M3, MD-M4 | CI macos, ubuntu, windows; owner's Mac and VMs (15.0/15.1 for MD-M2) |
| MD13 | **SPA polish and i18n**: `os.ts`, i18n `context` in the SPA and `TFor` in Go, every remaining §16 branch (About, App, Sharing, Updates, authText, DesktopPlatform's `browser`), the `_macos` key test; macOS fixtures | M | MD1 | Vitest; Windows snapshots unchanged; `check:size` passes | Mac, CI ubuntu |
| MD14 | **Write-backs and the manual run**: WM table results, KC result, WM1/WM2 decisions written into this doc, 02 §11.5 and `docs/security.md`; MD-M1…MD-M14 on the owner's Mac (27) recorded; VM and exit runs: 05's M-MAC rows | S | MD1–MD13 | the doc matches the code as merged; the manual table filled | owner's Mac |

---

## 23. Open questions for the owner

1. **The whole-screen fallback's mute (D7).** The plan says "auto-minimize and mute during full-screen shares" when the
   filter can't keep isshoni out. This doc mutes in-app viewer audio for that case (whenever 02 reports `SelfVisible`
   for a whole-screen share: an OS version where no strategy works, a filter SCK refused, or `hideFromCapture` off where
   02 relies on sharingType) until the share ends, so someone sharing a game with whole-screen capture on those versions
   can't keep listening to a friend's stream in isshoni meanwhile. Keep the plan's rule as written, or mute only when
   03-macos-audio can't confirm isshoni's own audio is excluded?
2. **Closing the window when not sharing quits isshoni** (03 D13's rule, kept for both OSes). Most Mac apps keep
   running after their last window closes. Keep the Windows rule, or make "keep running in the menu bar" the macOS
   default?
3. **Default hotkeys ⌃⌥⌘P (Pause) and ⌃⌥⌘S (Stop)** instead of Windows' F9/F10 combos (D8). Acceptable?
4. **If KC fails** (§13.3): accept the 0600 file store on macOS (same-user threat model as Windows' Credential
   Manager), or treat a Keychain password prompt after each update as acceptable instead?
