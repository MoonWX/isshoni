# M2 design 03: Desktop app (`cmd/isshoni-desktop`, `internal/desktop`, `web/src/platform/desktop`)

The Wails v3 app that M2 ships for Windows: one window that runs the bundled SPA, a tray icon, the first-run wizard,
the native share sheet with "What friends hear", share safety (tray, hotkeys, Pause, the Sharing pill), viewing in
WebView2, the web → desktop handoff, deep links, the process contract with the installer, settings, logs,
notifications and autostart. On the SPA side it is the `DesktopPlatform` of `web/src/platform` and the desktop-only
pages and panels.

Inputs: `docs/PLAN.md` (owner-approved; "Desktop clients", "Share safety", "Audio exclusion rules", "Privacy and trust
model", "Versioning and compatibility"), the M1 record (`docs/m1/README.md` §6 and the six M1 specs, mainly 01 §3,
§6, §8.14, 03 §7.5–§7.6, §7.12, 04 §4.4, §9.6 and 05 §4–§13), the code on `main` (`web/src/platform`,
`internal/protocol`, `internal/client/{signal,publish}`, `internal/server/signal/upgrade.go`, `internal/server/wire.go`)
and the M0 spikes `spikes/s1-win-audio` and `spikes/s4-sfu`.

The sibling M2 docs are written in parallel; this doc names them by file and states the interface it assumes:
- `01-client-core.md`: the Go client core (`internal/client/core`: publisher connection, share pipeline, media pump,
  rate control), the `internal/media` contract and fake engine, and the M2 protocol additions (`share.pause`,
  `user.connections` served, the relay kinds of `internal/protocol/relay`);
- `02-device-auth.md`: device flow and bearer tokens, server and client side (`internal/client/{deviceauth,credstore,
  deeplink,compat}`), `servers.json`, the `isshoni://` grammar, version compatibility, the `/link` approval page, the
  desktop origins and CORS;
- `04-windows-engine.md`: the C++ DLL, the C ABI, the Windows picker, capture and encode;
- `05-windows-audio.md`: the include-set mixer, exclusion rules, mic detection, the desktop policy service
  `internal/desktop/audio` ("What friends hear", prompts, banners), the sound check, the audiotest;
- `06-windows-release.md`: NSIS installer, updater (`internal/update`), release CI, `/download`,
  `install-desktop.ps1`, the installed layout and the process contract, SignPath.

**Tags.** Everything here is **M2** unless marked *later (M3/M4/M5)*. A "later" item gets only the room it needs now
(a type, a reserved name, an interface), never code.

---

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Wails `v3.0.0-beta.34`** (newest on proxy.golang.org on 2026-10-21, published 2026-10-20; no GA yet), pinned exactly in `go.mod`, with the `wails3` CLI at the same version in its own module `tools/wails3/go.mod`, built by `task release:wails3` into `.bin/wails3` (06 D19). Bumps follow §3.3 | The plan pins an exact beta and follows GA. Betas ship every 1–5 days (beta.26, the plan's example, is from 2026-09-25), so a bump is a deliberate act with a checklist, never a side effect of `go get -u` |
| D2 | **Wails is imported only by `cmd/isshoni-desktop`.** All app logic lives in `internal/desktop/*`, which builds and tests on Linux, macOS and Windows; Windows-only OS code sits behind the `OSHooks` interface with stubs elsewhere | The beta's API churn stays in one thin package; CI on ubuntu runs almost every test |
| D3 | **The app runs on macOS in M2 with the fake engine** (`task desktop:dev`). Only the Windows hooks (picker, pill, hotkeys, WDA, autostart, toasts, the shell relaunch) need the Windows PC | The owner develops on a Mac; the wizard, share sheet, panel, tray model and handoff can all be built and seen there. It also keeps Wails shared with M3 from day one |
| D4 | **The asset handler is ours** (`internal/desktop/assets`): it serves `web.Dist()`, injects one `<script src="/desktop/bridge.js">` into `index.html`, sets a strict CSP (header and `<meta>`), serves no service worker or manifest, and guards `/wails/*` by Origin | One small, testable `http.Handler` controls everything the webview loads; `bridge.js` gives the SPA its synchronous config without inline script |
| D5 | **The SPA talks to the server directly**: REST with `Authorization: Bearer` (cross-origin, 02's CORS) and the WebSocket with `hello.auth`. **Only Go holds the refresh token and only Go refreshes** (02's `Manager`, single flight); the SPA asks a binding for a short-lived access token and keeps it in memory | PLAN: "The SPA talks to the server with bearer device tokens". Two refreshers would trip 03's reuse detection; the refresh token never reaches page script |
| D6 | **Two connections, one Participant**: C1 = the SPA's `viewer` connection; C2 = Go's `publisher` connection (01's core). C2 joins a room only while it shares, and **a share follows the SPA's room** | PLAN "Client roles and sessions". A publisher that sits in a room without a share would keep the user "present" after the SPA left |
| D7 | **The desktop SPA offers rooms, viewing, sharing and its own pages only.** Login, invite, setup, reset, account, admin and the `/link` approval pages open in the system browser | Those are cookie flows or admin pages; a bundled SPA of version A against a server of version B is safest on the smallest surface (version skew, §16.3) |
| D8 | **The Sharing pill is a native Win32 window drawn by Go** (`internal/desktop/winos`), not a second webview; `WDA_EXCLUDEFROMCAPTURE` on it and on the main window | PLAN risk "Wails v3 is still beta: … no multi-window"; standard Win32 controls are accessible to Narrator and cost no WebView2 instance |
| D9 | **Global hotkeys are registered only while a share is live** (`RegisterHotKey`), defaults `Ctrl+Alt+Shift+F9` (Pause/Resume) and `Ctrl+Alt+Shift+F10` (Stop) | The app never steals keys from other apps when it isn't sharing. F-keys, because Windows delivers AltGr as Ctrl+Alt: a letter default such as `Ctrl+Alt+Shift+S` is AltGr+Shift+S, which types Ś (Polish) or Ș (Romanian), so typing that letter would stop the share and the letter would vanish; no layout types a character on F9/F10, and F12 is avoided because Windows reserves it for the debugger (`RegisterHotKey` docs) |
| D10 | **Handoff is the relay kind `playback.stop`** (reserved in 01 §8.14), sent by the desktop SPA when it joins a room, to the user's `full` connections; tabs that play anyway report **`playback.state`** to the app (§12.2), which tells 05 | No new server message; only the user's own desktop browser tabs (role `full`) stop, phones (role `viewer`) don't; 05 keeps a playing browser out of the share |
| D11 | **The app's own share is shown through the SFU's preview layer** (`low` only, never audible) | A native capture has no `MediaStream` in the page; 0.3 Mbps shows the sharer exactly what friends get |
| D12 | **Deep links use 02 §12's grammar**, `isshoni://<host>[:<port>]/` and `isshoni://<host>[:<port>]/r/<roomId>`, arriving only as `--open-url <url>` (06 §2.3); a link never starts a share; a new server needs "Add server {host}?" before any network request | PLAN: links carry only a server URL and a room id; one grammar for the app, the web page and 06's installer |
| D13 | **Closing the window while live hides it to the tray; when not live it quits** (a setting keeps the app in the tray). Locking the PC pauses a live share and releases keep-awake; unlocking within 5 min resumes it, a later unlock asks "Resume sharing?", and a share a lock has kept paused for 30 min stops; sleep stops it (§11.6) | PLAN's close rule, plus the least surprising default for an app that has no reason to run hidden. The bounds hold under either answer to §26 Q2: a sharer who locks and walks away must neither keep the PC awake all night nor start broadcasting again hours later on unlock |
| D14 | **Automated tests drive the real Go services**: a "desktop in Chrome" harness (`internal/desktop/e2ehost`) runs the SPA against the real `bridge` services over an HTTP transport, and the real `isshoni.exe` is driven over CDP on `windows-2025` | Playwright can't open a Wails window on Linux, but everything except the Wails transport is the shipped code |
| D15 | **"X started sharing" toasts in the desktop app, and the diagnostics bundle, are later (M5).** M2 has the safety and share toasts of §15 and "Open logs folder" | Web Push in the browser already notifies; the plan puts "Copy diagnostics" in M5 |
| D16 | **`isshoni.exe` runs as a child of the user's shell.** Started by anything that is not Explorer or a launcher (a browser opening a link, the installer), it relaunches itself under `explorer.exe` (§13.4) | isshoni's tree is always kept out of shares; inside a browser's tree it would keep that browser (Firefox plays from its parent process) out for the whole session (05 §6.2) |

---

## 2. Scope

**M2 (this doc)**: `cmd/isshoni-desktop` (Windows build, macOS dev build), `internal/desktop/*` (except 05's
`internal/desktop/audio`), the SPA's `DesktopPlatform` and desktop pages, the native share sheet and panel, share
safety, handoff, deep link handling, single instance, the mode dispatch and process contract with the installer,
settings, logs, autostart, notifications, the updates UI, and the tests of all of it.

**Not in this doc**: the device flow, token storage, `servers.json`, the `isshoni://` parser and the version
compatibility rules (02), the desktop origins and their server wiring (02), the engine and picker internals (04),
exclusion rules, the mixer and the audio policy service (05), the installer, updater mechanics, `/download` page and
release CI (06), the client core and the protocol additions (01).

**Later**: the macOS build that shares (M3, §21), the Linux agent's relay transport for `NativeBridge` (M4, §21),
"Copy diagnostics" and desktop "X started sharing" toasts (M5), a taskbar overlay icon while sharing (M5),
web-initiated desktop shares through `share.request` (M4; in M2 the core serves no relay request kind, 01 D16),
Windows on ARM64 (later).

---

## 3. Wails v3: pin, checklist, bumps

### 3.1 The pin

- `go.mod`: `github.com/wailsapp/wails/v3 v3.0.0-beta.34`. Its `go.mod` asks for `go 1.26.0` (ours: `go 1.26.0`,
  toolchain `go1.27.x`) and for module versions at or below the ones the server already uses (`coder/websocket`
  1.8.14 < 1.8.15, `golang.org/x/sys` 0.48 < 0.49, `modernc.org/sqlite` 1.44.3 < 1.60.1), so adding it raises no
  server dependency. DK1 checks that `go mod tidy` changes nothing in the server's direct requirements.
- `tools/wails3/go.mod` (a module of its own, like `tools/goreleaser`; 06 §3): `tool
  github.com/wailsapp/wails/v3/cmd/wails3` at the same version. `task release:wails3` builds it into `.bin/wails3`,
  which the bindings generator and the syso step call (the build itself is plain `go build`). The CLI's dependency
  graph stays out of `tools/go.mod`, and Dependabot's `/tools` entry never bumps it. `task lint:pins` checks that the
  two versions are equal.
- `web/package.json`: `@wailsio/runtime` at exactly the Go module's version without the `v` (`3.0.0-beta.34`; npm's
  versions follow the Go tags one to one, published the same day). `wails3 init`'s templates at beta.34 write the
  dist-tag `"latest"`, not a version, so they can't be the source. `lint:pins` derives the expected value from
  `go.mod`'s `github.com/wailsapp/wails/v3` requirement and checks `web/package.json` (an exact version, no range or
  tag) and `web/package-lock.json`; no hand-recorded version file exists.
- The app stays in the root module (the plan's layout): one `go.mod`, depguard keeps Wails out of everything but
  `cmd/isshoni-desktop` (§4.5), and the license gate runs on the Windows desktop binary.

### 3.2 The checklist (DK1, and again on every bump)

Each row is a test or an exact manual step on Windows 11 (owner's PC or `windows-2025`). The slice that runs it
records the result in its PR under the heading "Wails checklist"; README's WA87 copies the rows into this table
(README §5, shared files). A row that fails takes its fallback; none of them blocks M2.

| # | Behavior of the pinned Wails on Windows | How it is checked | Fallback |
|---|---|---|---|
| W1 | The asset origin is `http://wails.localhost` | CDP e2e asserts `location.origin == protocol.DesktopAssetOriginWindows`; that this value is in `protocol.DesktopOrigins()` (02 §6.4) needs no webview and is `internal/protocol`'s `TestDesktopOrigins` (`DesktopOrigins()[0] == DesktopAssetOriginWindows`; WA48's CDP skeleton names it in the row) | Change the constant in 02's `internal/protocol/desktop.go` one release before an app uses it (02 §6.4) |
| W2 | `window.isSecureContext` is true there | CDP e2e | None needed: `DesktopPlatform` skips the NeedsHttps check (§7.2) and WebRTC works in any context |
| W3 | Response headers from the asset handler (CSP) are enforced | CDP e2e: an injected inline script is refused and logs a CSP violation | The `<meta>` CSP that the handler injects anyway (§5.3) |
| W4 | `AssetOptions.Middleware` sees the runtime's `/wails/*` requests | Go test on the middleware + CDP e2e: a request with a foreign `Origin` gets 403 | Record it; navigation stays locked (§5.5), which is what makes a foreign page impossible |
| W5 | Top-level navigation and new windows can be cancelled (WebView2 `NavigationStarting`, `NewWindowRequested`) | CDP e2e: `location.href = 'https://example.com'` and `window.open(…)` leave the page on the asset origin | The SPA-level interception of §5.5 (always present); on a foreign URL after `NavigationCompleted`, `Shell.Reload()` |
| W6 | Single instance forwards the second launch's arguments (`SingleInstanceOptions.OnSecondInstanceLaunch`) | Windows CI test: launch twice, the first gets the `--open-url` link | Own named mutex `Local\io.isshoni.desktop.v1` plus pipe `\\.\pipe\io.isshoni.desktop.v1.<user SID>` in `winos` (distinct from the running mutex of §13.4). **Taken at beta.34 (WA41)**: the pinned Wails takes its single-instance lock inside `application.New`, which is §13.4's step 7, after steps 4–6, so a second instance would read and delete the Pending values and take the running mutex's place in the order; testing Wails' option would also need its message loop, a real window on CI. `winos` serves the fallback (§13.3); the macOS dev build has no single instance (a stub) |
| W7 | Closing the window can be cancelled and turned into `Hide()` (a `WindowClosing` hook) | CDP e2e + Go hook test | Window option to hide on close, plus our own quit path from the tray |
| W8 | The notifications service shows toasts with action buttons for an unpackaged app; its toast AppID (the Wails application `Options.Name`, §15), the process AppUserModelID and 06's shortcut property are the same id | Windows CI test reads the HKCU keys of §15 after one toast; manual on the owner's PC (M-DESK-5) for the buttons | Toasts without buttons (a click opens the window on the prompt, §15); if the ids can't be made equal, record the actual toast AppID here for 06 |
| W9 | WebView2 settings: DevTools off in production builds, default context menu off, zoom control off, browser accelerator keys off, general autofill and password save off, status bar off, swipe navigation off, external drop off | CDP e2e per setting where observable; else manual | The SPA-level mitigations of §5.4 |
| W10 | The main window's native handle (HWND) is available to Go | Go test on Windows | `FindWindowExW` by the window class Wails registers, on the UI thread |
| W11 | Bound methods may take `context.Context` first, cancelled when the JS call is cancelled | Bindings test. **Holds at beta.34 (WA48)**, without a window: `cmd/isshoni-desktop`'s `TestBindingContextIsTheCalls` calls the bound `Auth.Link` through Wails' own `Bindings` and `BoundMethod.Call` (`application.New` creates no native app; `Run` would), cancels the call's context, and the method's context ends with `context.Canceled` and the call answers `internal {reason: cancelled}` (§6.4). The real webview's half is the `isshoni_hw` `TestWailsChecklist` on the owner's PC, not run yet | No context; each method bounds its own work (§6.2 timeouts) |
| W12 | A Go `error` rejects the JS promise with `err.Error()` in its message, verbatim | Bindings test. **Holds at beta.34 (WA48)**, without a window: `TestBindingErrorIsVerbatim`: a bound method's `*bridge.Error` comes back as a `RuntimeError` `CallError` whose message is the error's JSON exactly and whose cause is the same object, as the HTTP transport answers it; the page's half (the runtime rejects with that message, which `toBridgeError` parses) is `WailsBridge.test.ts`. The real webview: `TestWailsChecklist`, as W11 | Methods return `{result, error}` structs instead (one code change in `bridge`) |
| W13 | WebView2 user data folder and additional browser arguments can be set | CDP e2e: audio plays without a click (autoplay flag) | Environment `WEBVIEW2_USER_DATA_FOLDER` and `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS` set by Go before the webview starts (unusable at beta.34, which overwrites both in every build, §5.4; the app sets both through Wails' options) |
| W14 | Quit runs our shutdown hook before the process exits (share stopped, close 1000 sent) | Go test with a fake share | Our tray Quit stops the share first, then calls Wails' quit |
| W15 | `.bin/wails3 generate bindings` (TypeScript) is deterministic and OS-independent for our services | CI: generate on ubuntu and macOS, diff. **The macOS half holds (WA48, group 5's integration)**: `task gen:bindings:check` (`GOOS=windows`) regenerates the committed `web/src/platform/desktop/bindings/` without a diff on the Mac, and `TestGeneratedBindingsMatchTheServices` checks, without the CLI, that every generated function calls the method id Wails gives the same Go method and every bound method has one. The ubuntu half is `ci.yml`'s `protocol` job (group 5's integration), whose first run, in the group PR, decides it | Commit the bindings generated on one OS only (Linux CI) |
| W16 | Element fullscreen inside WebView2 plus `Window.SetFullscreen(true)` fills the screen and returns cleanly on Esc | CDP e2e + manual | CSS pseudo-fullscreen (`fullscreen: 'none'`, as M1 has for browsers without the API) |
| W17 | A WebView2 process failure is reported (or the page is reloaded) | Manual: kill a renderer in Task Manager | Our watchdog: the SPA calls `App.Heartbeat` every 5 s; 15 s without one → `Shell.Reload()` |
| W18 | Tray icon, tooltip and menu can be changed at runtime; left click is reported | Manual + Go test of the view model | Rebuild the tray item on every change |
| W19 | Unsetting every `WEBVIEW2_*` environment variable (prefix, case-insensitive, §5.4) before the webview starts takes effect | CDP e2e build vs production build: the production build started with `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=<p>`, `WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER=<name>` and an arbitrary `WEBVIEW2_X=1` opens no debugging port or pipe and its webview sees none of them. At beta.34 Wails itself overwrites the first two in every build (§5.4), so only `WEBVIEW2_X` tells whether the scrub works (§22.4 B) | None needed for function; record |
| W20 | License gate (`go-licenses`, GOOS=windows) and `govulncheck` pass for `./cmd/isshoni-desktop` | CI | Replace or drop the offending module before DK1 merges |
| W21 | The Wails runtime's own objects. At beta.34 the same `/wails/runtime` endpoint serves, besides bound-method `Call`/`CancelCall`: `Clipboard` (`SetText`, `Text`), `Application` (`Quit`, `Hide`, `Show`), `Window` (52 methods, e.g. `Hide`, `SetAlwaysOnTop`, `Reload`, `OpenDevTools`, `Print`), `Dialog`, `ContextMenu`, `Screens`, `System`, `Events.Emit` and `Browser.OpenURL` (whose check refuses only `javascript:`, `data:`, `file:`, `ftp:`, empty schemes, shell metacharacters and backslashes, so `ms-settings:`, `search-ms:` and any registered protocol pass). `Events.On`/`Off` are JS-local. The postMessage IPC (`System.invoke`: `wails:runtime:ready`, drag and resize) is a separate path. The row records, at every pin: each object and method with its wire id, both request forms (the object in the JSON body or in the `?object=` query), the transport's chunking (its header or field names), what the runtime itself calls at load, and the postMessage messages | Go test of `assets.Guard` against requests built as the pinned runtime builds them; CDP e2e (§22.4 B): from the page, `Browser.OpenURL('ms-settings:display')`, `Browser.OpenURL('search-ms:query=x')`, `Clipboard.Text()` and `Window.Hide()` get 403 and have no effect (no settings page or search opens, the window stays visible), and a bound method still answers | If the Guard can't see the object (W4 fails, or the body can't be read and restored without breaking the runtime): record it, prefer a pin where it can, and rely on the navigation lock, CSP and no raw HTML (§5.5, §5.8) until then |

### 3.3 Bumps

- At most one Wails bump per isshoni minor release, in a PR of its own, with the checklist re-run and the CDP e2e
  and desktop e2e green. A security fix in Wails may be bumped in a patch release with the same checks.
- When `v3.0.0` GA ships, the next minor release moves to it.
- The `wails3` CLI (`tools/wails3/go.mod`), `@wailsio/runtime` and the library always move together.
- Wails tags a beta almost daily (beta.30 on 2026-10-15 to beta.34 on 2026-10-20), so "newest" says nothing about
  quality. DK1 runs §3.2 (W1–W21) on the pinned tag before committing it and records the tag's date beside the
  results; a newer tag is chosen only when the whole checklist passes on it.

---

## 4. Process, thread and window model

### 4.1 Architecture

```
 isshoni.exe  (Go, CGO_ENABLED=0 on Windows; one process per Windows user, single instance, a child of explorer.exe)
 ├─ cmd/isshoni-desktop: mode dispatch (§13.4) ── Wails v3 shell: main window, tray, notifications, single instance
 │     └─ WebView2 (msedgewebview2.exe children) at http://wails.localhost: the bundled SPA (web.Dist())
 │           │ bindings + events (Wails runtime)              │ REST: Bearer (CORS) · WSS /ws: hello.auth
 │           ▼                                                ▼
 ├─ internal/desktop/bridge ─► internal/desktop/app ─────── C1: SPA viewer connection ──────► isshoni server
 │                               ├─ session: C2 publisher connection (01's core) ───────────►   (same Participant)
 │                               ├─ share controller ─► media.Engine (04 DLL: capture, encode, audio)
 │                               ├─ audio.Service (05): exclusions, What friends hear, prompts, banners
 │                               ├─ deviceauth.Servers (02): servers.json, one Manager per server, Credential Manager
 │                               ├─ winos thread: hotkeys, pill, WDA, lock/sleep/end-session, keep-awake
 │                               └─ settings · logs · i18n · update.Updater (06)
 └─ short-lived: isshoni.exe --sound-check-helper (05) · one-shot modes (§13.4) · a second instance that forwards
```

### 4.2 Processes

| Process | Parent | Lifetime | Notes |
|---|---|---|---|
| `isshoni.exe` | `explorer.exe`: the Start menu, autostart, Explorer-launched installer starts, or the shell relaunch of §13.4 when a browser, the installer or another non-launcher started it; a launcher of 05 §5.4 (terminal, Steam, …) is kept | until Quit | Loads `isshoni-media.dll` in-process (04). A native crash ends the app and the share; §17 covers crash files |
| `msedgewebview2.exe` (browser, renderer, GPU, utility/audio, network) | `isshoni.exe` | with the window's environment | Its own user data folder (§14.1), so the WebView2 browser process is always a child of `isshoni.exe`: 05's instance rule "exclude all isshoni.exe descendants" keeps viewer audio out of shares |
| `isshoni.exe --sound-check-helper` | `isshoni.exe` | ≤ 3 s, during a sound check | 05's helper (05 §11.3): plays the chime and exits. `main` dispatches it before anything else (§13.4) |
| `isshoni.exe` in a one-shot mode (`--version`, `--self-check=…`, `--forget-credentials`, `--purge-data`, `--update-now`, `-Embedding`) | the installer, a test, COM | seconds | No window, no single instance, no mutex (§13.4) |
| `isshoni.exe <args>` (second instance) | whoever launched it | ≤ 2 s | Forwards its arguments to the first instance (§13.3), calls `AllowSetForegroundWindow(ASFW_ANY)` first, exits 0 |

The binary is named `isshoni.exe` (the installer's name, 06). `-H windowsgui`: no console. `--log-level debug`
raises the log level for one run. `main` handles its modes in the order of §13.4.

### 4.3 Threads (Windows)

- **Wails UI thread** (the process's main thread): the window, the tray, the WebView2 controller.
- **`winos` thread**: one goroutine with `runtime.LockOSThread()` running a `GetMessageW` loop. It owns a hidden
  top-level window (`WS_POPUP`, never shown, not `HWND_MESSAGE`, because message-only windows get no broadcasts) for
  `WM_HOTKEY`, `WM_WTSSESSION_CHANGE` (`WTSRegisterSessionNotification`), `WM_POWERBROADCAST`
  (`RegisterSuspendResumeNotification`), `WM_QUERYENDSESSION`/`WM_ENDSESSION`, `WM_DISPLAYCHANGE` and
  `WM_SETTINGCHANGE`, plus the pill window. Requests reach it by `PostMessageW(WM_APP+n)`; results come back on Go
  channels. Nothing on it blocks on the network or the engine.
- **Engine threads** (04, 05): capture, encode, mixer. Go drains `im_next_packet` on its own goroutine (01's pump).
- **Go runtime**: sessions, bridge calls (each Wails call on its own goroutine), settings writes, 05's polling loop.

### 4.4 The window

- One main window, title `isshoni`, native frame (not frameless: simpler, accessible, Windows snap works), minimum
  800 × 560 px, default 1200 × 760 px centered on the primary monitor, then the last bounds and the maximized flag
  from settings (`window`), clamped to a visible monitor.
- It loads `/` from the asset origin. The SPA routes inside it; the window never navigates elsewhere (§5.5).
- `WDA_EXCLUDEFROMCAPTURE` is set on it when it is created (§11.5).
- Shown at start, except with `--autostart` (§16.1), where no window is created until the user opens one from the
  tray.
- Hiding (close while live, close with "keep in tray") keeps the page running: WebView2 reports
  `document.visibilityState = 'hidden'`, and the M1 viewer turns video subscriptions off after 10 s while the
  audible share keeps playing (M1 05 §12.4's layer policy). The window's visibility also goes to 05
  (`audio.Service.SetVisible`, 05 §14.2), which only records it: 05 gives it no effect on the policy in M2.

### 4.5 Repository layout and import rules

```
cmd/isshoni-desktop/            package main
├─ main.go                      //go:build windows || (darwin && cgo) — mode dispatch (§13.4), flags, env scrub,
│                               //   single instance, Wails app, run
├─ shell.go                     //go:build windows || (darwin && cgo) — app.Shell over Wails (window, tray, events, toasts)
├─ engine_release.go            //go:build (windows || (darwin && cgo)) && !dev && !e2e — native.Open (01 §7.1)
├─ engine_fake.go               //go:build (windows || (darwin && cgo)) && (dev || e2e) — 01's fake engine with 05's
│                               //   audio scenarios (§10.3, §22.4)
├─ main_other.go                //go:build !windows && !(darwin && cgo) — prints "isshoni-desktop runs on Windows and
│                               //   macOS; Linux uses isshoni-agent" and exits 1 (06 §5.2, D18; so ./... builds and
│                               //   vets everywhere, also GOOS=darwin with cgo off)
├─ platform_windows.go, platform_darwin.go   the asset origin and its Guard, the data root from the shell's known
│                               //   folder (never the environment), the OS build (README WA22)
├─ flavor.go, flavor_{production,dev,e2e}.go  what the build tags change, each tag's file setting its part in init()
│                               //   (dev: ISSHONI_DESKTOP_DEV_URL, §5.3); a release compiles no dev or e2e code path
├─ webview2.go, webview2_windows.go  the WebView2 runtime check and its message box (§5.4)
├─ main_test.go                 the scrub, logging and option tests
├─ checklist_test.go, checklist_{windows,darwin}_test.go, testdata/checklist/   the `isshoni_hw` Wails checklist
│                               //   harness (§3.2)
├─ .gitignore                   /rsrc_windows_*.syso (the resource object exists only during a build, 06 §5.2)
├─ doc.go
└─ build/
   ├─ windows/icon.ico          app icon (16/20/24/32/48/256); 06's `task desktop:exe:windows` reads it
   ├─ windows/app.manifest      asInvoker; PerMonitorV2 DPI awareness (PerMonitor fallback); Windows 10/11
   │                            supportedOS GUID; Common Controls v6 dependency (the pill's BUTTON controls);
   │                            activeCodePage UTF-8; longPathAware
   ├─ windows/syso.go           //go:build ignore — writes rsrc_windows_<arch>.syso with app.manifest byte for byte,
   │                            //   and checks a built exe for it (06 §5.2 step 1): `wails3 generate syso` round-trips
   │                            //   the manifest through winres.AppManifest, which at beta.34 drops activeCodePage and
   │                            //   turns the PerMonitor fallback into System
   └─ tray/                     tray icons (idle, live, paused, warn; .ico 16/20/24/32), go:embed by shell.go (WA68)
internal/desktop/
├─ app/         App: owns the share controller, sessions, tray view, window and close policy, system events, wiring
├─ bridge/      the Wails-bound services (§6.2); plain Go structs, no Wails import
│  └─ codes/    the bridge error codes (§6.4), stdlib only, exported by tygo to platform/desktop/codes.gen.ts
├─ assets/      asset handler, bridge.js, CSP, /wails guard (§5)
├─ servers/     `Normalize` (address field rules and the invite/setup hint) and `Probe` over 02's client (§8.1)
├─ session/     C2: 01's core for the active linked server, and `core.Auth` over 02's Manager (§9)
├─ share/       share controller: picks, state machine, pause, tips, the 01/05 calls (§10.1)
├─ settings/    settings.json: schema, load, atomic save, migration; 05's audio.SettingsStore (NewAudioStore, §14)
├─ i18n/        Go lookups in the SPA's catalog (§18.1)
├─ applog/      slog to rotating files, redaction, crash output (§17)
├─ tray/        pure tray view model (§11.1)
├─ hotkey/      combo parsing and formatting (pure)
├─ winos/       Windows: the winos thread, hotkeys, pill, WDA, autostart, power and session events, keep-awake,
│               OS settings pages, AppUserModelID, the shell relaunch and the running mutex (§13.4);
│               oshooks_other.go: no-op stubs for darwin and linux
├─ audio/       05's policy service (owned by 05; listed here because it sits under internal/desktop)
└─ e2ehost/     package main: the desktop-in-Chrome harness (§22.4); never shipped
web/src/platform/desktop/       DesktopPlatform (entry chunk, small) + lazy: wails/, http/, bindings/ (§7)
web/src/desktop/                page folder: SetupPage, RelinkPage, SettingsPage (lazy, namespace `desktop`)
web/src/share/native/           native share sheet, first-share sheet, DND tip, What friends hear, mic prompt card,
                                Details disclosure
web/src/i18n/lazy/desktop.en.json · desktop.ts   the `desktop` namespace; Go reads its `desktop.native.*` keys
```

The desktop origin constants (`internal/protocol/desktop.go`), the `isshoni://` parser (`internal/client/deeplink`)
and the server list (`internal/client/deviceauth`) are 02's; this doc consumes them (§5.7, §8.1, §13).

depguard rules added to `.golangci.yml` (06 owns the file's CI; the rules are this doc's):

| Rule | Files | Deny | Why |
|---|---|---|---|
| `desktop-no-wails` | `internal/desktop/**` | `github.com/wailsapp/wails` | D2 |
| `desktop-no-server` | `internal/desktop/**`, `cmd/isshoni-desktop/**` | `github.com/MoonWX/isshoni/internal/server` | The app is a client |
| `server-no-desktop` | `internal/server/**`, `cmd/isshoni/**` | `github.com/MoonWX/isshoni/internal/desktop`, `github.com/wailsapp/wails` | The server binary never links the app |
| `desktop-no-engine-impl` | `internal/desktop/**` except `internal/desktop/e2ehost/**` | `github.com/MoonWX/isshoni/internal/media/native`, `github.com/MoonWX/isshoni/internal/media/fake` | Engines enter through the app's `main` (01's table: nothing under `internal/client` imports them either) |
| `desktop-main-no-fake` | `cmd/isshoni-desktop/**` | `github.com/MoonWX/isshoni/internal/media/fake` | The default lint run has no `dev`/`e2e` tags, so only `engine_fake.go` may import it; 06's release check confirms no release artifact links it |

Allowed: `cmd/isshoni-desktop` imports `internal/media/native` (all builds) and `internal/media/fake` (files tagged
`dev || e2e` only; 01 §6.3 calls this the `isshoni_fake` build); `internal/desktop/e2ehost` imports
`internal/media/fake`; no other `internal/desktop` package imports either. `internal/desktop` may import
`internal/protocol`, `internal/protocol/api`, `internal/protocol/relay`, `internal/client/*` (`core`, `deviceauth`,
`credstore`, `deeplink`, `compat`), `internal/media` (the root), `internal/version`, `internal/update` (in
`internal/desktop/app` only, 06), `golang.org/x/sys/windows` and the standard library. `golangci-lint` runs a second
time with `GOOS=windows` over `./cmd/isshoni-desktop/... ./internal/desktop/...`, and `GOOS=darwin CGO_ENABLED=0 go
vet ./...` stays green on ubuntu (M1's lint-go job and `task licenses` both run that pass).

---

## 5. Asset handler and webview security

### 5.1 The handler (`internal/desktop/assets`)

```go
package assets

type Options struct {
	Dist   fs.FS                // web.Dist(); unused (may be empty) when DevServer is set
	Bridge func() BridgeConfig  // read on every bridge.js and index.html request
	Origin string               // the asset origin: 02's protocol.DesktopAssetOriginWindows on Windows
	Dev    bool                 // dev and e2e builds only (-tags dev or e2e): see §5.3
	// DevServer is the Vite dev server's http:// loopback origin (dev builds only, from ISSHONI_DESKTOP_DEV_URL,
	// `task desktop:dev`): the SPA comes from there with live reload instead of from Dist (§5.3). New refuses it
	// without Dev, and only flavor_dev.go reads the variable, so a release can't set it.
	DevServer string
}

// New checks that Dist holds an index.html with exactly one `<script src="/boot-check.js">` tag (the injection
// point) and returns the handler for Wails' AssetOptions.Handler.
func New(o Options) (http.Handler, error)

// Guard is Wails' AssetOptions.Middleware: requests under /wails/ pass only from the asset origin (§5.6).
func Guard(origin string) func(http.Handler) http.Handler
// GuardWKWebView is Guard for an asset origin that WKWebView serves through a URL-scheme handler (wails://localhost,
// the macOS dev build): §5.6's one exception, for a custom-scheme origin only.
func GuardWKWebView(origin string) func(http.Handler) http.Handler

type BridgeConfig struct {
	V         int           `json:"v"`         // 1
	Transport string        `json:"transport"` // "wails"; e2ehost: "http"
	App       AppFacts      `json:"app"`
	Server    *ServerFacts  `json:"server"`    // the active server, null before the first server is added
	ServerIDs []string      `json:"serverIds"` // the derived id of every server in servers.json, for storage cleanup (§14.3)
	Dev       bool          `json:"dev"`
}
type AppFacts struct {
	Version string `json:"version"` // internal/version
	OS      string `json:"os"`      // protocol.ClientOS: "windows" (M2), "macos" (dev, M3)
	OSBuild int    `json:"osBuild"` // e.g. 26100
	Arch    string `json:"arch"`    // "amd64"
}
type ServerFacts struct {
	ID     string `json:"id"`     // derived local id (§8.1), never stored
	Origin string `json:"origin"` // "https://watch.example.com"
	Name   string `json:"name"`   // /info server.name, from 02's servers.json entry
}
```

| Request | Response |
|---|---|
| `GET /desktop/bridge.js` | 200 `text/javascript; charset=utf-8`, `Cache-Control: no-store`, body `window.__ISSHONI_DESKTOP__=Object.freeze(<BridgeConfig as JSON>);` (`encoding/json`, which escapes `<`, `>`, `&`, U+2028, U+2029) |
| `GET /` and every path without a dot | `index.html` with `<script src="/desktop/bridge.js"></script>` inserted right before the `boot-check.js` tag (the rewrite is done once in `New`; the CSP `<meta>` per request, §5.3), `Content-Type: text/html`, `Cache-Control: no-store`, the headers of §5.3 |
| `GET /assets/*` and other files of `dist/` | the file (MIME from the extension, like 04 §9.5), `Cache-Control: public, max-age=31536000, immutable` for `/assets/*`, `no-cache` otherwise; `.br`/`.gz` siblings are not served (no compression needed locally) |
| `GET /sw.js`, `/manifest.webmanifest` | 404: no PWA in the app |
| a missing dotted path | 404 |
| `/wails/*` | Wails' runtime, behind `Guard` |
| any method but GET or HEAD outside `/wails/*` | 405 |

The handler never proxies to the server and never reads tokens. It is tested on every OS (§22.1). `--self-check`
(§13.4) builds it once over `web.Dist()` to prove the bundle is intact.

### 5.2 `bridge.js`

Example (the active server set):

```js
window.__ISSHONI_DESKTOP__=Object.freeze({"v":1,"transport":"wails","app":{"version":"0.2.0","os":"windows",
"osBuild":26100,"arch":"amd64"},"server":{"id":"k3m9p2qx","origin":"https://watch.example.com","name":"Friends"},
"serverIds":["k3m9p2qx"],"dev":false});
```

- It is a classic script before `boot-check.js` and the module entry, so `detectPlatform()` (boot step 2, 05 §4)
  reads it synchronously.
- It holds no token, no user data beyond the server's public name, and nothing a page on another origin could use.
- It is generated per request: switching to another server (§8.1) changes the config and reloads the window.

### 5.3 CSP and headers

Header (and the same policy as `<meta http-equiv="Content-Security-Policy">` in the served `index.html`, so W3's
result doesn't matter; two identical policies intersect to the same one):

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:;
connect-src 'self' https://<active-host> wss://<active-host>; worker-src 'none'; manifest-src 'none';
font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; frame-ancestors 'none'
```

- `<active-host>` is the active server's host with its port, from `BridgeConfig.Server.Origin`. With no active server
  `connect-src` is `'self'` only: the wizard reaches servers only through Go (§8).
- In dev and e2e builds only, when the active origin is an `http://` loopback origin (allowed only there, §8.1),
  `connect-src` names `http://<active-host>` and `ws://<active-host>` instead. A release-build test asserts the policy
  never holds `http:` or `ws:`.
- `'self'` is the asset origin, which covers the Wails runtime's calls to `/wails/runtime`; which of those calls are
  answered is the Guard's allowlist (§5.6), not the CSP's.
- No `unsafe-inline`, no `unsafe-eval`. If a pinned Wails needed either, that version is not used (§3.2).
- Dev builds (`-tags dev`, never released) with `assets.Options.DevServer` (`ISSHONI_DESKTOP_DEV_URL`, an http
  loopback origin such as `http://localhost:5173`) keep the page on the asset origin and take the SPA from Vite: a
  route gets Vite's `index.html` with the bridge tag and the CSP, every other GET or HEAD (modules, `/@vite/client`,
  public files) is proxied to the dev server, and only the live-reload WebSocket goes to it directly. The dev origin
  is added to `script-src`, `style-src` and `connect-src` (with its `ws://` for live reload), and a **per-request
  nonce** goes into `script-src` and `style-src`, with Vite's `<meta property="csp-nonce">` in the page: Vite's
  React-refresh preamble is an inline module script and Vite injects `<style>` elements, and the nonce lets exactly
  those run. There is still no `'unsafe-inline'`. A release-build test asserts the policy has no `localhost:5173`.
- Other headers on HTML: `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy:
  same-origin`, `Permissions-Policy: fullscreen=(self), picture-in-picture=(self), autoplay=(self), display-capture=(),
  camera=(), microphone=(), geolocation=(), usb=(), payment=()`. `display-capture=()`: the app shares natively, so
  `getDisplayMedia` stays off in its webview.

### 5.4 WebView2 settings

| Setting | Value | Fallback if the pinned Wails can't set it (W9) |
|---|---|---|
| DevTools | off in production builds (`-tags production`), on in dev and e2e builds | none needed: production builds are the only released ones |
| Default context menu | off in production | the SPA cancels `contextmenu` outside text fields |
| Zoom control (Ctrl + wheel, Ctrl +/−) | off; OS text scaling still applies | the SPA cancels `wheel` with Ctrl and the zoom keys |
| Browser accelerator keys (F5, Ctrl+R, Ctrl+P, Ctrl+F, Ctrl+U, Ctrl+Shift+I) | off | the SPA cancels those `keydown`s (capture phase) |
| General autofill, password save | off (the link step's password form, 02 §14 and §8.2, is the app's only password field; the setting stays off so WebView2 stores nothing that form or any page holds) | `autocomplete="off"` on any form; accepted residual risk, recorded |
| Status bar (link preview) | off | none needed |
| Swipe navigation, pinch zoom | off | `touch-action` in CSS |
| External drop (a dropped file or link navigates) | off | the SPA cancels `dragover` and `drop` on `document` |
| User data folder | `%LOCALAPPDATA%\isshoni\WebView2`, expanded to an absolute path once at start; the same string (no trailing separator) is `audio.SelfInfo.WebViewDataDir` (§6.1) | none at beta.34: Wails overwrites the variable in every build (W13); the app sets it through `WindowsOptions` (`WebviewUserDataPath`) |
| Additional browser arguments | `--autoplay-policy=no-user-gesture-required` (viewer audio plays without the "tap to unmute"; 05 §10.3's unlock then never shows) | none at beta.34: Wails overwrites the variable in every build (W13); the app sets it through `WindowsOptions` (`AdditionalBrowserArgs`) |

- **Environment scrub.** Before `application.New` (Wails reads its updater-helper variables there, WebView2 reads
  `WEBVIEW2_*` when the window's environment is created), production builds unset every environment variable whose
  name starts with `WEBVIEW2_` (compared case-insensitively, as Windows compares names; e.g.
  `WEBVIEW2_WAIT_FOR_SCRIPT_DEBUGGER` and any a later runtime adds), so nothing outside the app can start its webview
  with a remote debugging port or a script debugger attached (W19). It keeps Wails' own placeholders: at beta.34 Wails
  sets five variables in **every** build, at package init and again right before it creates the webview environment,
  overwriting what the parent passed (`internal/webview2/webviewloader`, `preventEnvAndRegistryOverrides`):
  `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS`, `WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER`, `WEBVIEW2_BROWSER_EXECUTABLE_FOLDER` and
  `WEBVIEW2_USER_DATA_FOLDER` to `""`, `WEBVIEW2_RELEASE_CHANNEL_PREFERENCE` to `"0"`, because a variable that exists
  stops WebView2's registry override of the same setting. In every build the scrub also unsets
  `FRONTEND_DEVSERVER_URL` (it would move Wails' start URL to another port, off the asset origin the Guard accepts) and
  every `WAILS_UPDATER_HELPER*` (Wails' own updater would swap the app's files when `application.New` sees them;
  isshoni's updates are 06's). This is defense in depth, not a boundary: a process that can set isshoni's environment
  runs as the same user and could read the refresh token from Credential Manager directly (02 §11.5). The W13
  environment fallback can't work at beta.34 (Wails overwrites both variables); the app sets the user data folder and
  the autoplay argument through Wails' `WindowsOptions` (`WebviewUserDataPath`, `AdditionalBrowserArgs`). e2e builds
  keep the `WEBVIEW2_*` variables, but Wails' overwrite applies to them too (§22.4 B).
- **Permission policy**: the main window denies the webview's microphone, camera, geolocation and notification
  permissions (`WebviewWindowOptions.Permissions`). Without any policy the pinned Wails grants every permission without a
  prompt (`SetGlobalPermission(Allow)`); the kinds left out keep the webview's own prompt, and the page's
  `Permissions-Policy` (§5.3) turns camera, microphone and geolocation off too.
- **WebView2 runtime**: Evergreen, minimum 120.0 (`webview2Min`, which must equal the installer's `WEBVIEW2_MIN`, 06
  §6.5). The version EdgeUpdate records is compared part by part as numbers; empty, unparsable and `0.0.0.0` (what an
  uninstall leaves) count as none. When it is missing or older, a native message box says "isshoni needs the Microsoft
  Edge WebView2 Runtime" with Microsoft's WebView2 download page and the project site's
  `/troubleshooting#desktop-webview2` (the text is `desktop.native.webview2.missing` from the app's catalog, WA41;
  neither link is clickable in a message box, Ctrl+C copies its text), and the app exits 1: the pinned Wails would only
  log it and show a blank window. The installer installs it when missing (06).
- **Windows N editions** have no H.264 decoder without the Media Feature Pack: `detectCaps()` then reports no H.264
  decode, and the M1 codec banner (05 §10.6) shows; the troubleshooting page names the Media Feature Pack (06).

### 5.5 Navigation, links and drops

Three layers, so no single one has to be perfect:
1. **The SPA never navigates away.** Outside `platform/`, nothing sets `location` or calls `window.open`
   (`web/eslint.config.js`: `no-restricted-syntax` selectors for `window.open` (also on `globalThis` and `self`),
   `location.assign` and `location.replace`, and assigning `location` or `location.href`, for `src/` outside
   `platform/`; `no-restricted-properties` can't match `window.location.assign` or an assignment; group 3's
   integration). `DesktopPlatform` installs, at construction:
   - a capture-phase `click` and `auxclick` listener on `document`: an `<a>` whose resolved URL is not on the asset
     origin is cancelled and handed to `openExternal(url)`;
   - `window.open = (url) => { openExternal(String(url)); return null; }`;
   - `dragover` and `drop` listeners that cancel the default (no navigation on drop).
2. **Go cancels foreign navigation** (W5): a top-level navigation or new window whose URL is not on the asset origin
   is cancelled; an http(s) URL goes to the system browser through the same validation as `OpenExternal`.
3. **`/wails/*` is guarded by Origin and by an allowlist** (§5.6), so even a foreign page could not call a binding,
   and a script injected into the SPA can't reach the Wails runtime's own `Browser.OpenURL` (which would bypass
   `App.OpenExternal`'s rules below), `Clipboard` or `Window` objects.

`App.OpenExternal(url)` accepts only absolute `https:` URLs, and `http:` URLs whose host is `localhost` or a
`.localhost` name (or the active http loopback origin) in dev and e2e builds; anything else (`file:`, `javascript:`,
`ms-settings:`, custom schemes) is `invalid_argument`. OS settings pages have their own method with a fixed list
(§6.2, `App.OpenOSSettings`).

### 5.6 The `/wails/*` guard

`assets.Guard(origin)` applies three checks to every request under `/wails/`; requests outside `/wails/` pass through
unchanged. Its WARN lines go to `slog.Default()` with `component=assets` (the process's default logger carries no
component of its own, so a JSON line never has the key twice).

1. **Origin**: the request passes only when its `Origin` header equals the asset origin, or it has no `Origin` and
   `Sec-Fetch-Site` is `same-origin`. Otherwise 403 and one rate-limited WARN line (`desktop: runtime request from
   another origin`, the origin truncated to 128 bytes). **The one exception** is `assets.GuardWKWebView(origin)`, for
   a custom-scheme asset origin only (`wails://localhost`, the macOS dev build): WKWebView hands its scheme handler no
   `Origin` and no `Sec-Fetch-Site` at all, not even on a POST (measured with the checklist at beta.34), so a request
   with neither header also passes, but on `/wails/runtime` only when it carries the runtime's own
   `x-wails-client-id` header: no simple request can carry it, so a sender on another origin would need a CORS
   preflight, which the Guard never answers with a 2xx. A request that names another origin or site is refused as by
   `Guard`, and an http(s) origin gets no exception. Windows uses `Guard` (`platform_windows.go`); if WebView2 also
   sent neither header, the owner's checklist run shows W4/W21 failing and the integrator decides between extending
   the exception and W4's fallback. The macOS release (M3) re-checks it against its webview.
2. **Runtime allowlist** (W21): `/wails/runtime` takes only POST (405 otherwise). The Guard reads the object and
   method the way the pinned transport does, from the JSON body or, when the body is empty, from the
   `?object=`/`?method=` query (buffered up to 64 KiB, then restored for the runtime with
   `io.NopCloser(bytes.NewReader(…))`; a larger body gets 413; a body with object or method also in the query is
   refused), and lets through only the
   bound-method objects `Call` and `CancelCall`, plus any read-only method that W21 finds the runtime itself calls at
   load. Everything else is refused with 403 and one rate-limited WARN line (`desktop: runtime object refused`,
   object and method names only): `Browser`, `Clipboard`, `Dialog`, `ContextMenu`, `Screens`, `System`,
   `Application`, `Window` (the app has its own `Window` service, §6.2) and `Events` (at beta.34 `Events.On`/`Off`
   are JS-local, and Go registers no listener for frontend events, so `Events.Emit` has no use). A request that uses
   the transport's chunking is refused (our binding arguments are far below one chunk), so the Guard never has to
   check an object after reassembly; a request whose object it can't parse is refused. The allowlist is a table in
   `internal/desktop/assets/runtime_allow.go`, keyed by the wire ids W21 records, and is re-checked at every bump.
3. **Path allowlist**: under `/wails/`, only `/wails/runtime` (check 2) and GET or HEAD of the runtime's static
   scripts (`/wails/runtime.js`, `/wails/transport.js`, `/wails/custom.js`) and of `/wails/eventpayload/<id>` (an
   event over 8 KiB that Go parked for the page) pass; every other `/wails/` path gets 403, GoStream's
   `/wails/stream/` included (beta.34 serves it; the app registers no stream). The handler itself answers 404 for any
   `/wails/` path Wails didn't serve.

The SPA uses no WML attributes (`data-wml-*`, `wml-*`): an ESLint `no-restricted-syntax` rule on JSX attribute names
matching `^(data-)?wml-` fails on any in `web/src`, so no markup can trigger a runtime call by itself.

### 5.7 The desktop origin on the server (02's)

This doc changes nothing on the server. 02 owns `internal/protocol/desktop.go` (`protocol.DesktopAssetOriginWindows`
and `protocol.DesktopOrigins()` with its five spellings, 02 §6.4) and the wiring (`signal.Config.AllowedOrigins` and
`httpapi.Deps.DesktopOrigins` from it, 02 DA1/DA6). This doc consumes:
- `protocol.DesktopAssetOriginWindows` as `assets.Options.Origin` and as the Guard's origin on Windows;
- the W1 assertion (§3.2): `location.origin == protocol.DesktopAssetOriginWindows`, and that value is in
  `protocol.DesktopOrigins()`.

The server's own CSP (04 §9.6) does not apply to the desktop webview, whose pages come from the asset handler.

### 5.8 Tokens and page script

- The refresh token lives in Go and in Windows Credential Manager (02); no binding returns it.
- **Every token call names its origin.** `Auth.AccessToken(origin, force)` (02 §11.7's `AccessToken(force)` plus the
  origin) returns the active server's access token (`isa_…`, 15 min, 03 §7.12) as `{token, expiresAt}` (`expiresAt`
  in ms since the epoch) **only when `origin` equals the active server's origin**; for any other origin it returns
  `null` without touching a `Manager` and writes one debug line (`desktop: token request for a server that isn't
  active`, no origin). Go reads the active `Manager` once and compares that `Manager`'s origin, so a token it returns
  is always `origin`'s own, even if the active server changes during the call. It also returns `null` when the active
  server isn't linked. Go returns a token with at least
  120 s left, refreshing first when needed (02 §11.4, single flight per server; concurrent calls share one refresh);
  `force: true` (after the server refused the token the SPA had) is 02's `Manager.AccessTokenWithExpiry(ctx, true)`:
  at most one forced refresh per 10 s, a second forced call within that window gets that refresh's token. There is no
  separate "token rejected" call. `Auth.SessionEnded(origin, code)` and `Auth.HelloRefused(origin, error)` follow the
  same rule: for an origin that isn't active they do nothing and reject with `not_found`.
- Why: the active server can change while a page built for another server is still loaded (an added server becomes
  active before the window reloads, §8.2), and that page's `apiFetch` and C1 still talk to its own `serverOrigin`. A
  token bound to the active server, handed to such a page, would reach the other server's operator. The window reloads
  right after every change of the active server (§8.1, §8.2, §13.2), and the origin check covers the moment before.
- `DesktopPlatform` passes `bridge.js`'s `server.origin` as `origin` in every call and ignores `isshoni:auth.state`
  events whose `origin` differs from it.
- The SPA keeps the token in a closure in `platform/desktop/tokens.ts`: never in Web Storage, never in a store, never
  logged (the lint rule `no-restricted-globals` on `localStorage` already confines storage to `platform/`; a unit test
  asserts that no `isa_` string reaches `storage.local`). `tokens.ts` asks again 60 s before `expiresAt`.
- What a script in the page could do with the access token is what a script in a browser tab can do with the session
  cookie; CSP without inline script, no raw HTML (05 §20) and the navigation lock are the defenses.

---

## 6. Go side: App, services, events

### 6.1 The App and what it consumes

```go
package app // internal/desktop/app

type Deps struct {
	Settings *settings.Store     // settings.json; 05's audio.SettingsStore through settings.NewAudioStore (§14.2)
	Servers  *deviceauth.Servers // 02: servers.json, one Manager per server, the credential store
	Session  Session             // C2: 01's core for the active linked server (§9); nil: New builds one (WA58)
	Engine   media.Engine        // native.Open on Windows; fake.NewEngine in dev and e2e builds (§4.5)
	Audio    AudioService        // 05's *audio.Service (internal/desktop/audio; wired by WA77)
	Updater  Updater             // 06's *update.Updater, built in the first instance only (§13.4; wired by WA84)
	Shell    Shell               // cmd/isshoni-desktop over Wails; e2ehost: a recording fake
	OS       OSHooks             // winos on Windows; stubs elsewhere
	I18n     *i18n.Catalog
	Clock    func() time.Time
	Log      *slog.Logger
}

func New(d Deps) (*App, error)            // starts nothing
func (a *App) Run(ctx context.Context) error // session, audio.Service.Run, updater Run, tray, system events, probe; returns after Quit
func (a *App) HandleArgs(args []string)   // first instance and forwarded args: --open-url, --server, --autostart (§13)
func (a *App) HandleLink(l deeplink.Link) // a parsed link, or PendingServer/PendingRoom (§13.2)
func (a *App) Quit(stopShare bool)        // stops the share (01's Share.Stop), audio.Service.Close, flushes settings, quits

// Shell is what the app needs from the Wails shell.
type Shell interface {
	ShowWindow(focus bool) // creates it first after --autostart
	HideWindow()
	WindowVisible() bool
	SetFullscreen(on bool)
	Reload() // reloads the SPA (every change of the active server, crash recovery)
	MainWindowHandle() uintptr
	SetTray(v tray.View)
	Emit(name string, data any) // Wails event to the SPA
	OpenURL(url string) error   // system browser; validates with App.OpenExternal's rule (§5.5) itself: absolute https,
	                            // http loopback only in dev/e2e builds, else invalid_argument (file:, ms-settings:,
	                            // search-ms:, javascript:, UNC paths refused whoever calls it)
	Notify(n Notification) error
	Quit()
}

// OSHooks is the Windows layer (internal/desktop/winos); darwin and linux get no-op stubs in M2.
type OSHooks interface {
	OSBuild() int
	RegisterHotkey(id HotkeyID, k hotkey.Combo) error // ErrHotkeyTaken
	UnregisterHotkey(id HotkeyID)
	Hotkeys() <-chan HotkeyID
	Pill() Pill
	ExcludeFromCapture(hwnd uintptr) error // SetWindowDisplayAffinity(WDA_EXCLUDEFROMCAPTURE)
	SetAutostart(on bool, exe string) error
	Autostart() (bool, error)
	OpenSettingsPage(p OSPage) error
	KeepAwake(on bool) // SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED|ES_DISPLAY_REQUIRED)
	Events() <-chan SystemEvent // Lock, Unlock, Suspend, Resume, EndSession, DisplayChange
}
```

Wiring in `main.go` (first instance, §13.4 step 7):
- `deviceauth.OpenServers(deviceauth.ServersOptions{Dir: <data dir>, Store: credstore.Open(credstore.Options{}),
  App: deviceauth.AppInfo{Version: version.Version(), ClientKind: desktop, OS: windows, DeviceName: <02's default:
  os.Hostname() cut to 64 code points, or "Windows PC">}, AllowInsecureLoopback: <dev or e2e build>, OnState:
  app.OnAuthState})`. `<data dir>` is `%LOCALAPPDATA%\isshoni` (§14.1): every per-PC file under one folder, which
  06's `--purge-data` removes; the Credential Manager entries it points to don't roam either.
- `audio.New(engine.(media.AudioController), settingsStore, audio.SelfInfo{PID: uint32(os.Getpid()),
  WebViewDataDir: <the same absolute, expanded path given to WebView2 (`WindowsOptions.WebviewUserDataPath`, W13),
  no trailing separator>, Exe: os.Executable()}, log)`; `App.Run` starts `Service.Run(ctx)`, `Quit` calls
  `Service.Close()`.
- `update.New(update.Options{CurrentVersion: version.Version(), Enabled: func() bool { return
  settings.Updates().Check && !update.PolicyDisabled() }, Channel: func() update.Channel { return
  settings.Updates().Channel }, WizardDone: settings.WizardDone, DataDir: <data dir>, Logger: log})`; `App.Run`
  runs `Updater.Run(ctx)` (first instance only).
- `session.New(session.Options{Servers, Engine, Client: protocol.ClientInfo{Kind: desktop, Version, OS}, Log})`
  (§9). As built (WA58): `app.New` builds it itself when `Deps.Session` is nil and an engine is open
  (`session.ServersOf(Deps.Servers)`, `Client {Kind: desktop, Version: version.Version()}` with the core's OS, and 05's
  `AudioConfig` when `Deps.Audio` has it), so `main.go` and the e2e host pass no session and get C2 and the share
  controller without a change; a `Deps.Session` without `*session.Session`'s C2 methods (`Ready`, `ConnectionID`, …)
  gets no C2 for the controller (`Share.Start` answers `publisher_offline`). `app.New` also builds the share controller
  (§10.1) over the session, the engine's picker, 05's audio policy when `Deps.Audio` implements `share.AudioPolicy`, the
  shell's main window and `settings.json`. Both of 05's interfaces are found by a type assertion on `Deps.Audio`, so
  `deps_test.go` asserts at compile time that `*audio.Service` has them: a signature that drifted would otherwise drop
  05's calls silently.
- The engine's probe (`Engine.Probe(ctx, false)`) runs once at start in the background; `Share.Capabilities()` waits
  for it at most 2 s.

The consumer interfaces for the sibling docs are in §24 with the exact shapes this doc assumes; each sibling's names
win and the adapter in `internal/desktop/*` maps them.

As built (WA41, interfaces first): `Deps.Session`, `Audio` and `Updater` are small interfaces that `app` declares
(`Session` with `Run`; `AudioService` and `Updater` with `Run` and `Close`), because WA41 merged before the concrete
types; `*audio.Service` and `*update.Updater` fit them, and `settings.AudioStore[audio.Settings]` fits
`audio.SettingsStore` (compile-time checks in `internal/desktop/app/deps_test.go`). 05's `audio.SettingsStore` is a
generic adapter, `settings.NewAudioStore[T](store, defaults)`, not methods on `*settings.Store`, so `settings` doesn't
import `internal/desktop/audio`: with `T = audio.Settings` its methods are exactly the interface's, the section's
unknown keys are kept, and the defaults apply under the saved values (§14.2). `app` also declares the methods WA48's
`App` service calls (`Info`, `OpenExternal` with `ValidExternalURL`, `OpenOSSettings`, `Heartbeat`, `Live`, `Log`,
`OwnConnections`; `OpenLogsFolder` and `OpenNotices` return `ErrNotImplemented` until their slices); `Shell` has no
`SetTray` until WA68's tray package.

### 6.2 Bound services (`internal/desktop/bridge`)

The only app services that page script can call. The pinned Wails runtime serves its own objects (`Browser`,
`Clipboard`, `Window`, `Dialog`, `Events.Emit`, …) on the same `/wails/runtime` endpoint; the Guard refuses all of
them (§5.6, W21), so these bound services are the only Go surface page script reaches. Each service is a plain struct
over `*app.App`, registered in `main.go` with `application.NewService`. Arguments are validated; errors follow §6.4.
TypeScript types come from the bindings generator (`web/src/platform/desktop/bindings/`, committed, drift-checked
like tygo's output).

As built (WA48): `bridge.New(bridge.Deps{…})` builds the seven services, and `shell.go`'s `registerServices` registers
each with `application.NewService` between `application.New` and `Run` (`main.go` calls it once the app exists); a
service missing there gets no binding. Argument and result types use plain strings for the enum-like values (05's
`FriendsHear` and `AppRule` go out as they are); the generated TypeScript types them as strings, so `WailsBridge` checks
its arguments against the generated signatures and casts results to `bridge.ts`'s unions (§7.4). `main.go` passes
`update.PolicyDisabled` as `Deps.PolicyOff` (§14.2's `updates.policyOff`). Methods whose slice hasn't landed answer
`not_implemented` (README §4): `Auth.Unlink`, `AddServer`, `SwitchServer` and `RemoveServer` until WA71 (they need the
app's reload and share stop), `Settings.Update`, `Hotkeys`, `TestHotkey` and `Updates.*` until WA84; `Audio.*` work
over 05's service once `main.go` passes it as `Deps.Audio` (WA77). As built (WA58): every `Share` method works through
the app's share controller (§10.1), which `bridge.New` finds on `Deps.App` (`*app.App`'s `Shares`; `bridge.Deps.Shares`
overrides it) and whose events it forwards as `share.state`, `share.stats` and `share.notice`; a `Core` without a
controller (`cmd/isshoni-desktop`'s test core) gets a stand-in whose calls that would make or change a share answer
`not_implemented` (`Current` and `Stats` answer null; `Stop`, `Release` and `WatchServerStats` do nothing), so
`bridge.Core` gained no method. `Share.Capabilities` already answers from the probe: each usable encoder kind once, in
04's order (the probe lists a kind once per adapter), with `reason` the media code when `canShare` is false.

| Service.Method | In → out | Errors | Notes |
|---|---|---|---|
| `App.Info()` | → `AppInfo {version, os, osBuild, arch, publisherConnectionId, wizardDone, dev}` | — | `publisherConnectionId` is `core.Status().ConnectionID` of C2, "" while it isn't ready |
| `App.Heartbeat()` | → — | — | the W17 watchdog |
| `App.OpenExternal(url)` | `string` → — | `invalid_argument` | §5.5 |
| `App.OpenOSSettings(page)` | `"notifications" \| "dnd" \| "graphics-capture" \| "sound" \| "sound-communications" \| "taskbar"` → — | `invalid_argument` | `ms-settings:` URIs by OS build (§11.7, §10.5); `sound-communications` is `ShellExecuteW(nil, "open", "control.exe", "mmsys.cpl,,3", …)` with that fixed argument string only (05's ducking tip) |
| `App.Quit(stopShare)` | `bool` → — | `busy` (a share is live and `stopShare` is false) | |
| `App.Log(level, msg, attrs)` | `"debug"\|"info"\|"warn"\|"error"`, ≤ 1 KiB, ≤ 16 attrs of ≤ 256 bytes → — | — | ≤ 10 per second, excess dropped and counted (§17) |
| `App.OpenLogsFolder()`, `App.OpenNotices()` | → — | `io_error` | Explorer at `%LOCALAPPDATA%\isshoni\logs`; `<exe dir>\licenses\THIRD_PARTY_NOTICES.txt` (06 §2.2) |
| `App.PendingLink()`, `App.LinkHandled()` | → `PendingLink \| null` (the `deeplink.open` payload) / — | — | the link not yet acted on, e.g. the one that started the app before the SPA ran; `LinkHandled` clears it after the SPA navigated (room) or the add step was answered (Add or Cancel) |
| `App.OwnConnections(ids)` | `string[]` (≤ 64) → — | `invalid_argument` | the online connection ids of C1's `user.connections`, sent by the SPA on every change; expires `playback.state` senders (§12.2) |
| `Auth.State()` | → `AuthState` (02 §11.7) | — | the active server's state; pull for `auth.state` |
| `Auth.Servers()` | → `ServerInfo[]` = 02's `ServerEntry` plus `{id, active, linked}` | — | `id` derived (§8.1); `user` is `api.User {id, username, role}`; `linked = user != null` |
| `Auth.Probe(address)` | `string` → `ServerProbe {origin, name, version}` | `invalid_address`, `server_unreachable`, `tls_untrusted`, `not_isshoni`, `server_not_set_up`, `incompatible_server` | 03's own (§8.1); never adds the server |
| `Auth.AddServer(origin)` | → — | `too_many_servers`, `invalid_address`, `busy` | 02's `Servers.Add`: makes it active, then reloads the window to `/desktop/setup?link=1` with the new server's `bridge.js` (§8.2); called from the confirm step's Link button, or from the add step's Add when the probe confirms the host (§8.2) |
| `Auth.SwitchServer(origin)` | → — | `not_found`, `busy` (live share) | makes it active and reloads the window (for a server that isn't linked, the reloaded page shows its link step, §7.5's `RequireAuth`, §13.2) |
| `Auth.RemoveServer(origin)` | → — | `not_found`, `busy` (live share on it) | 02's `Servers.Remove` (Unlink, then drop the entry) |
| `Auth.Link()`, `Auth.CancelLink()` | → — | `server_unreachable`, `incompatible_server`, `rate_limited` | 02's `Manager.Link` on the active server; progress arrives as `auth.state` |
| `Auth.LinkWithPassword(username, password)` | `string`, `string` → `AuthState` | `invalid_argument`, `invalid_credentials`, `account_pending`, `account_disabled`, `rate_limited` (`params.retryAfter`), `server_busy`, `validation_failed` (`params.fields`: M1's login field codes `required`, `too_long`, `invalid` for `username` and `password`) (02's `*api.Error` codes, passed through), `server_unreachable`, `incompatible_server` | the link step's "Sign in with username and password instead" (§8.2): 02's `Manager.LinkWithPassword` on the active server with `AppInfo`'s device name (02 §14); the password crosses the binding once as a `protocol.Secret` and is never stored, retried or logged (Wails' own logger too, §17) |
| `Auth.Unlink()` | → `{confirmed: bool}` | — | Sign out (§8.3); a live share on the active server is stopped first |
| `Auth.Recheck()` | → `AuthState` | — | the version screen's "Try again" (02 §13.1) |
| `Auth.AccessToken(origin, force)` | `string`, `bool` → `{token, expiresAt}` or `null` (not linked, or `origin` isn't the active server's) | `server_unreachable` (02's `ErrOffline`), `incompatible_server` (`ErrIncompatible`) | §5.8; 02's `Manager.AccessTokenWithExpiry(ctx, force)` on the active server only |
| `Auth.SessionEnded(origin, code)` | `string`, `"unauthenticated" \| "session_revoked" \| "account_disabled"` → `AuthState` | `invalid_argument`, `not_found` (`origin` isn't active) | 02's `Manager.SessionEnded` after C1 was stopped with that code (§7.3, §8.3) |
| `Auth.HelloRefused(origin, error)` | `string`, `{code: "client_outdated" \| "protocol_unsupported", params}` → `AuthState` | `invalid_argument`, `not_found` (`origin` isn't active) | 02's `Manager.HelloRefused` for C1's refused hello; the version screen follows the returned state (§16.3) |
| `Share.Capabilities()` | → `ShareCaps {canShare, reason?, encoders[], borderless, osBuild, audioBackend}` | — | from the engine's probe (≤ 2 s); `audioBackend` (§6.3) |
| `Share.Pick()` | → `*Picked {pickId, kind, label, border, confirm[]}` or `null` (cancelled) | `already_sharing`, `engine.<code>` | opens the OS picker owned by the main window (§10.1) |
| `Share.Release(pickId)` | → — | — | |
| `Share.Start(req)` | `{pickId, roomId, preset, withAudio}` → `ShareView` | `pick_expired`, `already_sharing`, `not_linked`, `publisher_offline`, `engine.<code>`, wire codes (`share_limit`, `room_full`, `room_not_found`, `forbidden`, …) | resolves at `starting`; `live` comes as an event |
| `Share.Current()` | → `*ShareView` | — | pull for the `share.state` event (§6.3) |
| `Share.Stop()` | → — | — | never fails; idempotent |
| `Share.StopOther(roomId, shareId)` | → — | wire codes | 01's `Client.StopOther`: C2 sends `share.stop` for another connection's share of this user (the SPA's role `viewer` may not, 01 §6.3) |
| `Share.SetPaused(paused)` | → — | `feature_disabled` (server without `share.pause`), `not_sharing` | §11.3 |
| `Share.SetPreset(preset)` | → — | wire codes, `not_sharing` | 01's `Share.SetPreset`: C2 sends `share.update` |
| `Share.SetAudioEnabled(on)` | → — | `not_sharing` | local only: 05's `audio.Service.SetShareSound(on)`; no wire message; a no-op for a share started without sound, whose switch isn't shown (§10.3) |
| `Share.Stats()` | → `*ShareStatsSample` | — | M1's shape (05 §8), mapped from 01's snapshot (§6.3) |
| `Share.WatchServerStats(on)` | → — | — | reference-counted; 01's `Client.WatchServerStats(count > 0)` while the Details disclosure is open (§10.3). As built (WA58): the controller keeps the wish and passes it to C2's core client at each `Start` and on every change while a share runs, and turns it off on that client once the share ended, since 01's switch is sticky on its client: no later share keeps `stats.watch` on without an open disclosure |
| `Audio.FriendsHear()` | → `FriendsHear` (05 §14.5) | — | pull for `audio.report` |
| `Audio.Watch(on)` | → — | — | reference-counted in the bridge; 05's `Service.Watch(count > 0)`. The ShareSheet, first-share sheet, the wizard's voice step and Settings → Audio call it on mount and unmount |
| `Audio.WatchLevels(on)` | → — | — | reference-counted; 10 Hz `audio.levels` while > 0 |
| `Audio.SetAppDecision(appId, decision)` | `"keep-out" \| "include" \| "auto"` → — | `invalid_argument`, `not_found` | 05 owns the rules; locked rows refuse with `invalid_argument` |
| `Audio.SetKeepOutMicUsers(on)` | → — | — | |
| `Audio.Decide(promptId, decision)` | `"keep-out" \| "include"` → — | `not_found` (already answered, dismissed or gone), `invalid_argument` (`params.field: decision`) | 05's `Decide`: saves the answer per app (05 §14.6) |
| `Audio.DismissPrompt(promptId)` | `string` → — | `not_found` (05's `ErrUnknownPrompt`) | 05's `DismissPrompt`: the app stays kept out, isn't asked about again this run, nothing is saved (05 §14.6) |
| `Audio.DismissTip(code)` | → — | `invalid_argument` | |
| `Audio.AppRules()` | → the saved per-app decisions with names (05) | — | Settings → Audio |
| `Audio.SoundCheck()` | → `SoundCheckResult` (05's `media.SoundCheckResult`) | `busy` (a share is live) | ≤ 5 s |
| `Settings.Get()` | → `SettingsView` | — | the user-facing part of §14.2 |
| `Settings.Update(patch)` | `SettingsPatch` (every field optional) → `SettingsView` | `invalid_argument` with the field | §14.2 |
| `Settings.Hotkeys()` | → `[]HotkeyStatus {id, combo, state: "registered"\|"idle"\|"taken"\|"invalid"}` | — | |
| `Settings.TestHotkey(combo)` | → `HotkeyStatus` | `invalid_argument` | registers and unregisters at once; `HotkeyStatus.typesChar: true` when the combination types a character in the current keyboard layout (`VkKeyScanExW`/`ToUnicodeEx` with `GetKeyboardLayout`; Ctrl+Alt is AltGr), which the hotkey field shows as a warning (§11.2) |
| `Window.SetFullscreen(on)` | → — | — | §12.1 |
| `Updates.Status()` | → `UpdateStatus` | — | §16.2 |
| `Updates.Check()` | → `UpdateStatus` | `update.<code>` (06 §8.10) | 06's `CheckNow` |
| `Updates.Install()` | → — | `busy` (a share is live; 03 enforces it, the updater isn't told), `update.not_ready`, `update.not_installed` | 06's `Apply(ctx, u.Ready(), true)`, then Quit (§11.6) within 5 s |

Timeouts inside the methods (W11 fallback and in general): `Probe` 10 s, `Link` 15 s, `Start` 20 s (C2 not ready
after 10 s → `publisher_offline`), `AccessToken` 15 s, `Updates.Check` 45 s, every other method that does I/O 10 s.

**No sound-mode binding.** M2 has no "All sound" mode: the built-in voice apps are always kept out (05 §2, §14.1;
README §7 Q8), so nothing in the bridge or the SPA can set an audio mode. Sound is switched per share only, through
`Share.SetAudioEnabled` → 05's `SetShareSound`. The bridge never builds a `media.AudioConfig` itself (05's
`AudioConfig()` does, always `AudioExcludeApps`).

### 6.3 Events (Go → SPA)

Names carry the prefix `isshoni:` (not shown below) so they never collide with Wails' own. Every stateful event has
a pull method; the SPA subscribes first and then pulls, so it misses nothing. As built (WA48): the services' events go
out through the shell's `Emit` (`bridge.Deps.Emit`); none is registered with `application.RegisterEvent`, because
the pinned Wails drops an event whose data type isn't exactly the registered one.

| Event | Payload | When | Pull |
|---|---|---|---|
| `share.state` | `ShareView` (below) | every change, coalesced to ≤ 10 per second | `Share.Current()` |
| `share.stats` | `ShareStatsSample` | every 2 s while live | `Share.Stats()` |
| `share.notice` | `{code, params}`: `encoder_fallback {encoder}` | 01's `EventNotice` (§10.5) | — (transient) |
| `audio.report` | `FriendsHear` (05 §14.5) | on change, ≤ 4 per second | `Audio.FriendsHear()` |
| `audio.levels` | 05's `Levels {seq, mixPeakDb, mixRmsDb, apps: Record<appId, {peakDb, source}>}` | 10 Hz while the `Audio.WatchLevels` count > 0 | — |
| `audio.prompt` | 05's `Prompt {id, appId, name, kind: 'mic' \| 'browser-mic', since}` | a new prompt | part of `FriendsHear.prompts` |
| `audio.alert` | 05's `Banner` (an error-level banner: `exclusion-unavailable`, `windows-too-old`) | once per code per share during a live share (05 §14.4); Go also shows the §15 toast | part of `FriendsHear.banners` |
| `auth.state` | `AuthState` (02 §11.7; its `origin` names the server) | every change of the active server's state; the SPA ignores one whose `origin` isn't its `serverOrigin` (§5.8) | `Auth.State()` |
| `deeplink.open` | `PendingLink`, exactly one shape: `{roomId}` for the active server (`""` = the SPA's own last room); `{add: {origin, host, hostUnicode, roomId}}` for an unknown server (`host`/`hostUnicode` from `deeplink.Link`); `{switch: {origin, host, hostUnicode, roomId}, live: {roomId}}` for a known server while a share is live (§13.2's question); `{error: "too_many_servers"}` for a 21st server (WA41) | a deep link, `--server` or `PendingServer` (§13.2) | `App.PendingLink()` |
| `window.state` | `{visible, focused, fullscreen}` | changes | — |
| `settings.changed` | `SettingsView` | after any change, from any source | `Settings.Get()` |
| `update.state` | `UpdateStatus` | changes | `Updates.Status()` |

`ShareView` (the bridge's DTO, TS `ShareView`, mapped from 01's `core.ShareSnapshot` plus the controller's own
fields; named apart from `core.ShareSnapshot` on purpose):

```json
{"state": "live", "shareId": "s_q7m2k9x4w3bd8c1a", "connectionId": "c_k3v9q2m7xw4pa8d1", "roomId": "lounge",
 "kind": "window", "label": "Minecraft", "preset": "game", "withAudio": true, "audioOn": true,
 "border": false, "pill": true, "minimized": false, "watching": 3, "liveSince": "2026-10-21T12:00:00.000Z",
 "hint": null, "encoder": "nvenc", "hw": true,
 "layers": [{"rid": "f", "width": 1920, "height": 1080, "fps": 60, "targetKbps": 8000, "sentKbps": 7712,
             "captureToSendMs": {"p50": 14, "p95": 22}},
            {"rid": "q", "width": 640, "height": 360, "fps": 15, "targetKbps": 300, "sentKbps": 296,
             "captureToSendMs": {"p50": 12, "p95": 19}}],
 "server": null, "endReason": null, "error": null}
```

- `state`: `picking | starting | live | paused | reconnecting | unreachable | stopping | ended | failed` (mapping from
  01 in §10.1).
- `label` is the picker's display name (`media.Source.Name`: a window title or a monitor name), filled by the
  controller. It stays on this PC: the SPA shows it, the tray shows it, and nothing sends it to the server
  (`core.ShareRequest.Label` is always `""`, so 01 §8.5's share labels default to "Screen"/"Window").
- `watching` = `len(core.ShareSnapshot.Watchers)`.
- `minimized` = 01's snapshot `Minimized` (04's `SOURCE_MINIMIZED`/`SOURCE_RESTORED`, 01's event table).
- `hint`: `ShareHint | null` (M1's shape): `{kind: 'upload-limited', approxHeight}` from `Upload.Limited` and
  `Upload.ApproxHeight`, `{kind: 'cpu-limited'}` from `Overloaded`, else `null` (§10.3).
- `encoder`, `hw`, `layers`, `server`: the Details disclosure (§10.3): `layers` from `LayerSnapshot {RID, Width,
  Height, FPS, TargetBps/1000, SentBps/1000, CaptureToSend}`; `server` = `{layers: ShareSnapshot.Server}` (an object
  around 01's `ServerLayerStats[]`, as built by WA58: WA48's DTO types it `map[string]any`, TS `Record<string,
  unknown>`) while `Share.WatchServerStats` is on, else `null`.
- `endReason`: `user | hotkey | tray | source-gone | elsewhere | server | server-unreachable | unlinked | system | error`.
- `error`: `{code, params}` per §6.4.

`ShareStatsSample` (`Share.Stats()`, `share.stats`): `layers[]` = `{rid, width, height, fps, kbps: SentBps/1000,
encoder, hw, limit}` with `limit` = `bandwidth` while `Upload.Limited`, `cpu` while `Overloaded`, else `none`;
`audioKbps` = `ShareSnapshot.AudioSentBps / 1000` (the field 01 §11.9 adds, from the audio track's
`TrackStats.Bitrate`); `at` is set by the TypeScript wrapper on receipt (`performance.now()`).

`ShareCaps.audioBackend` = `Probe.Audio.Backend` (01 §5.1's `media.AudioCapsFromFlags` of `im_caps.audio_flags`:
`process-loopback`, `endpoint` or `none`), copied as is. Live banners during a share come from 05's report
(`FriendsHear.banners`), never from this field.

### 6.4 Bridge errors

A failing method returns a Go error whose `Error()` is the JSON `{"code":"…","params":{…}}`; the TypeScript wrapper
parses the rejection's message into `BridgeError {code, params}` (W12). Codes are snake_case and stable:

| Code | Meaning | SPA text key |
|---|---|---|
| `invalid_argument` | a bad argument (`params.field`) | `desktop.errors.invalid_argument` |
| `not_found`, `busy`, `io_error`, `rate_limited`, `feature_disabled` | as named | `desktop.errors.*` |
| `not_implemented` (`params.method`, the bound name, when the bridge itself answers it), `internal` (`params.reason`: `timeout` or `cancelled` when that is all there is to say) | a method whose slice hasn't landed (README §4; no release has one); a bug, a transport failure, a deadline that ran out | `desktop.errors.*` |
| `invalid_address`, `server_unreachable`, `tls_untrusted`, `not_isshoni`, `server_not_set_up`, `incompatible_server` (`params.verdict`, `reason`, `serverVersion`, `minClientVersion`: 02 §13.2's `compat.Check`), `too_many_servers` | server probe and `Auth` (§8.1) | `desktop.errors.*`; `incompatible_server` by `verdict` (02 §13.3) |
| `not_linked` | no credential for the active server | the link step (§8.2) |
| `publisher_offline` | C2 isn't ready within 10 s | `desktop.errors.publisher_offline` |
| `already_sharing`, `not_sharing`, `pick_expired` | share controller | `desktop.errors.*` |
| `engine.` + 01's `media.ErrorCode` (01 §5.2): `engine.permission_denied`, `engine.encoder_lost`, `engine.source_gone`, `engine.unsupported_os`, `engine.no_encoder`, `engine.dependency`, `engine.internal`, `engine.invalid`, `engine.busy`, and (WA48) every other media code but `cancelled`: `engine.audio_unavailable`, `engine.audio_not_running`, `engine.stopped`, `engine.timeout`, each with its text; `params.reason` = `media.Error.Reason` (04 §5.7's detail token: `pick_expired`, `self_window`, `driver_too_old`, `no_media_foundation`, `blocked_by_policy`, `missing`, `version`, `abi`, …; `""` when none) | the engine (§10.5) | `desktop.errors.engine.*`, chosen by code and then by `params.reason` (§10.5's reason rows win over the code's row); `invalid` and `busy` share `internal`'s text; an unknown media code or reason is shown with its code's text, else `internal`'s; `engine.cancelled` is never surfaced (`Pick` returns `null`) |
| `update.` + 06's code (06 §8.10), plus `update.not_ready` | `Updates.*` | `desktop.update.error.<code>` |
| any 01 §12.1 wire code; the M1 REST codes `Auth.LinkWithPassword` passes through (`invalid_credentials`, `account_pending`, `account_disabled`, `server_busy`, `validation_failed` with `params.fields`) | passed through from C2 or 02 | `errors.<code>` (M1 catalog); `validation_failed` also shows M1's `fieldErrors.<field>.<code>` under each field |

As built (WA48, `bridge/errors.go`): every service maps its Go errors through one table. A deadline that ran out is
`internal {reason: timeout}`, because a method's own bound (`Audio.SoundCheck`'s 10 s, a local file) is no server's
fault; only `Auth`, whose deadlines bound calls to a server, makes it `server_unreachable`. A call the page cancelled
(W11) is `internal {reason: cancelled}`, which nobody reads. 02's `ErrOffline`, `ErrNetwork` and `ErrBadResponse`
are `server_unreachable`; an error no rule knows is `internal`, logged once at WARN with its cause (the packages below
never put a token, password, name or path into an error's text). Params hold codes and numbers only, never English.
The update codes' texts are `desktop.update.error.<code>` (README §3 item 42), written with the update screens (WA82).
As built (WA58): the share controller's errors are checked first, since they may wrap a wire or media error:
`pick_expired` (with `params.reason: pick_expired`, like the engine's `source_gone` of an expired token, so §10.5's
reason row matches both), `already_sharing`, `not_sharing`, `publisher_offline`, `feature_disabled` (with
`params.feature: share.pause`), `busy` (`StopOther` while C2 shares in another room) and `invalid_argument`; then the
rest of the table as above.

---

## 7. The SPA: Platform changes and `DesktopPlatform`

### 7.1 Changes to `web/src/platform/types.ts`

All additive for `BrowserPlatform`, which implements the new optional members as "absent":

```ts
export interface Platform {
  // … M1 members …
  /** desktop: { url, auth } with a bearer provider; browser: { url } (cookie). Was `auth?: HelloAuth` (unused). */
  signaling(): { url: string; auth?: HelloAuthProvider };
  /** M2: the desktop app's features; null (or absent) in the browser. Optional (WA32): read it as
   *  `platform.desktop ?? null`, so the M1 test platforms (test/, rooms/, viewer/, account/) stay valid. */
  readonly desktop?: DesktopFeatures | null;
}
export type HelloAuthProvider = (o: { forceRefresh: boolean }) => Promise<HelloAuth | undefined>;

export interface PickedSource {
  kind: ShareKind;
  audioScope: 'window' | 'tab' | 'system' | 'none';   // native: 'system' (everything but the exclusions) or 'none'
  warning: 'screen-with-system-audio' | 'no-audio' | null; // native: null (voice apps are kept out)
  preview: MediaStream;                                // WA65: MediaStream | null (null for a native pick)
  /** M2, native: the picker's name for the source; shown on this PC only, never sent. */
  label?: string;
  /** M2, native: one-time sheets to show before going live, in order (§10.2). */
  confirm?: ('first-share' | 'dnd')[];
  release(): void;
}

export type LocalEndReason =
  | 'user' | 'browser-stopped' | 'server' | 'server-unreachable' | 'error'
  | 'source-gone' | 'system' | 'unlinked';            // M2, native

export interface SharingProvider {
  readonly mode: 'in-page' | 'native';
  pick(opts: { preset: Preset }): Promise<PickedSource | null>;
  start(src: PickedSource, opts: { preset: Preset; withAudio: boolean }, ctx: ShareContext): Promise<ActiveShare>;
  /** M2, native: the share the app publishes now (e.g. after the page reloaded), or null. */
  current?(): ActiveShare | null;
  /** M2: connections whose shares are this page's local shares beyond the page's own (desktop: C2). */
  localConnectionIds?(): string[];
  /** M2, native: share.stop for another connection's share of this user (the desktop SPA's role is viewer). */
  stopOther?(roomId: string, shareId: string): Promise<void>;
}

export interface DesktopFeatures {
  readonly app: { version: string; os: ClientOS; osBuild: number };
  readonly server: { id: string; origin: string; name: string } | null;
  readonly bridge: NativeBridge;                       // §7.4
  wizardSteps(): WizardStep[];                         // Windows: ['link', 'sound', 'voice']; later (M3) adds 'audio-permission'
  /** Auth.SessionEnded and Auth.HelloRefused with this page's serverOrigin bound (02 §11.8). */
  sessionEnded(code: SessionEndCode): Promise<AuthState>;
  helloRefused(e: HelloRefusal): Promise<AuthState>;
  /** C1 stopped with err (its SignalStateInfo.error): calls the two above as §7.3 says and tells the room runtime
   *  what to do next. */
  signalStopped(err: { code: string; params?: Readonly<Record<string, unknown>> }): Promise<SignalStopAction>;
}
export type WizardStep = 'link' | 'audio-permission' | 'sound' | 'voice';
export type SessionEndCode = 'unauthenticated' | 'session_revoked' | 'account_disabled';
export interface HelloRefusal { code: 'client_outdated' | 'protocol_unsupported'; params?: Record<string, unknown> }
/** redial: SessionEnded says still linked, start C1 again once; relink: it isn't (the relink page follows auth.state,
 *  §8.3); version: HelloRefused's state drives 02 §13.3's screen (§16.3); wait: the bridge answered not_found (this
 *  page's server isn't active any more), stay stopped until the reload; none: not a desktop case, M1's handling. */
export type SignalStopAction = 'redial' | 'relink' | 'version' | 'wait' | 'none';
```

**As built by WA32** (where the code departs from the block above as first written): `Platform.desktop` is optional,
not required, because a required member would break the M1 test platforms in `web/src/test/platform.ts`, `rooms/`,
`viewer/` and `account/`; `BrowserPlatform` sets it to `null` explicitly and callers read `platform.desktop ?? null`.
`PickedSource.preview` stays `MediaStream` until WA65's `DesktopSharing`, which makes it `MediaStream | null` together
with its readers in `share/` (`BrowserSharing`, `BrowserShare`, `shareStore`; README WA65's row). `DesktopFeatures` has
three members beyond the first draft, `sessionEnded`, `helloRefused` and `signalStopped`, which bind the page's
`serverOrigin` (02 §11.8) and keep §7.3's C1-stop rules inside `platform/`, where they are tested; the room runtime
calls `signalStopped` (WA42, in `web/src/rooms/`).

`ActiveShare.setPaused` works for a native share when C2's `core.Status().Features` lists `share.pause`
(01-client-core); `LocalShareState` `'paused'` is now used.

### 7.2 Detection and boot

`detect.ts`:

```ts
export function detectPlatform(): Platform {
  const cfg = readDesktopConfig(globalThis.__ISSHONI_DESKTOP__);  // validates v === 1 and the shape, else null
  if (cfg) return new DesktopPlatform(cfg);
  // pending (mobile apps): window.Capacitor?.isNativePlatform?.() → new MobilePlatform()
  return new BrowserPlatform();
}
```

`DesktopPlatform` itself is in the entry chunk and stays under 4 KB gzip: it imports neither the Wails runtime nor the
generated bindings. `bridge` loads `platform/desktop/wails/WailsBridge.ts` (or `http/HttpBridge.ts` when
`transport === 'http'`) with a dynamic `import()` started in the constructor. `app/chunks.node.test.ts` gains: no
module of `@wailsio/runtime`, `platform/desktop/{wails,http,bindings}/`, `desktop/` or `share/native/` in the initial
chunks.

Boot (05 §4) in the desktop:
- Step 0 (fragment stash): unchanged and harmless (desktop routes have no fragment tokens).
- Step 1 (secure context): **only for `BrowserPlatform`**. The desktop's pages come from its own asset handler.
- Step 4: when `platform.desktop.server` is null, boot skips `/info` and renders the router at `/desktop/setup`, or
  at the desktop page the app opened (`/desktop/setup`, `/desktop/relink`, `/desktop/settings`, e.g. Settings).
  The app's own pages (`isDesktopPage`) skip `/info` **with a server too** (WA42): they talk to the bridge only, so an
  `isshoni://` link for a new server opens the add step (`/desktop/setup?add=…`) while the active server is down, and
  Settings opens directly. Every other path asks for `GET /api/v1/info` through `apiFetch` (a Public route: when the
  active server isn't linked, `apiFetch` sends it without `Authorization`, §7.3); the Offline screen gains "Switch
  server" (it opens the router at `/desktop/settings#servers` without `/info`) in the desktop. When Go refuses the
  token because the active server is incompatible (`Auth.AccessToken` rejects with `incompatible_server`, which
  `api()` wraps in a `NetworkError`), `loadInfo` answers `incompatible` at once, without retries, and the router
  starts at `/desktop/setup`, whose link step shows the version texts with Try again (§8.2, §16.3); `['me']` then
  resolves to null, so `RequireAuth` follows the `AuthState` instead of showing Offline.
- Step 6: the service worker is already registered only when `platform.pwa` is set (`app/boot.tsx`); the desktop's is
  `null`.
- `createConnection` (`web/src/rooms/connection.ts`, where the `SignalClient` is built) passes `buildVersion: ''` for
  the desktop, even when `opts.buildVersion` is given, which turns `staleBuild` off (`isStaleBuild` needs a non-empty
  build version): the desktop SPA is the app's own build and a reload can't change it (§16.3).

### 7.3 `DesktopPlatform` behavior

| Member | Desktop behavior |
|---|---|
| `kind` | `'desktop'` |
| `client` | `{kind: 'desktop', version: app.version, os: app.os, browser: 'edge'}` |
| `role` | `'viewer'` (C1 never publishes; C2 does) |
| `serverOrigin` | `server.origin`, `''` when none |
| `capsNow()` | 01's `detectCaps()` in WebView2 (decode capabilities matter; C1 sends nothing) |
| `capabilities()` | `caps`, `canShare` = `sharing !== null && Share.Capabilities().canShare` (so `canShare` keeps implying a sharing provider), `fullscreen: 'element'` (W16, else `'none'`), `pip` by feature detection, `wakeLock` by feature detection, `push: 'unsupported'`, `install: 'none'` |
| `apiFetch(path, init)` | 02 §11.8, with the origin bound: get `Auth.AccessToken(serverOrigin, false)`; on `null`, send the request **without** `Authorization` (Public routes such as `/info` work; others get the server's 401 `unauthenticated`, which clears `['me']`, 05 §6.2); with a token, `fetch(new URL(path, serverOrigin), {...init, headers + Authorization: Bearer <token>, credentials: 'omit', mode: 'cors', referrerPolicy: 'no-referrer'})`; on 401 `invalid_token` or `unauthenticated`: `Auth.AccessToken(serverOrigin, true)` and one retry; a second 401 goes to the SPA, and so does the first one when the forced call fails (offline, `server_unreachable`), so the SPA's 401 rule runs either way. `serverOrigin` is always `bridge.js`'s, so a token is only ever requested for the server the request goes to (§5.8) |
| `signaling()` | `{url: wss://<host>/ws, auth: ({forceRefresh}) => …}`: `Auth.AccessToken(serverOrigin, forceRefresh)`; the result as `{scheme: 'bearer', token}` (`undefined` when `null`, so a hello to a server that isn't active carries no token) |
| `createPeerConnection` | `new RTCPeerConnection(config)` |
| `sharing` | a `DesktopSharing` over `bridge` (§10); non-null on Windows and in dev/e2e builds (fake engine). It comes from `DesktopPlatformOptions.sharing`, a factory WA65 plugs `DesktopSharing` into; until then `sharing` is `null` |
| `notifications` | `support()` → `'unsupported'` (no Web Push in WebView2; the push card never shows) |
| `pwa` | `null` |
| `requestWakeLock()` | the Screen Wake Lock API when present, else `null` (Go keeps the display awake while sharing, §11.6) |
| `inAppBrowser()` | `null` |
| `openExternal(url)` | `App.OpenExternal(url)`; rejected URLs are logged and dropped |
| `storage.local` | Web Storage with the key prefix `isshoni:<serverId>:` (§14.3); `storage.session` unprefixed |
| `versionActions()` | `updateApp` (`() => Promise<AppUpdateOutcome>`) and `openInBrowser` (§16.3): `openInBrowser = openExternal(serverOrigin + (currentRoomId ? '/r/' + currentRoomId : '/'))`, the room taken from `location.pathname` (`/r/<room>`) |
| `desktop` | the features object of §7.1 |

Tokens: `platform/desktop/tokens.ts` is created with `serverOrigin` and passes it in every call; it caches `{token,
expiresAt}` and asks again 60 s before `expiresAt` (Go hands out tokens with at least 120 s left); concurrent callers
share one in-flight call; `rejected(token)` clears the cache when it holds that token, and the next call passes
`force: true`. It ignores `isshoni:auth.state` events whose `origin` differs from `serverOrigin` (any such event means
the window is about to reload, §8.2), and clears its cache when the state for its origin leaves `linked`.

**`SignalClient` change** (`web/src/protocol/signal-client.ts`, an M2 addition to 01 §16's TypeScript client):
`SignalClientOptions.auth?: HelloAuthProvider`. Before every `hello` (the resuming ones too) the client awaits
`auth({forceRefresh: false})` and puts the result in `hello.auth`; it is called before the socket opens, so the
server's hello timeout never waits for it, and a rejection or no answer within `AuthTimeoutMs` (20 s, longer than the
bridge's own 15 s `AccessToken` budget, §20) fails that attempt like a failed connect. When a bearer connection is
stopped with a session-scope `unauthenticated` (01 §3.2), the client asks with `forceRefresh: true` and reconnects as
a new connection; it keeps forcing in each attempt until one forced call answers (a failed forced call doesn't use up
the refresh). A second `unauthenticated` before that connection has been ready for `BackoffResetMs` (10 s) stops it
as in M1; the allowance resets only after 10 s of ready, not at every welcome, which stops a "welcome, then
unauthenticated" loop. `createConnection` passes `platform.signaling().auth`. `BrowserPlatform` gives none, so
browsers are unchanged.
In the desktop, the room runtime hands every C1 stop to `platform.desktop.signalStopped(info.error)` (WA42 wires it in
`web/src/rooms/`) and acts on the `SignalStopAction` it returns: a stop with `session_revoked` or `account_disabled`,
or a second `unauthenticated` right after the forced refresh, calls `Auth.SessionEnded(serverOrigin, code)` and
`redial`s once if the result's `status` is `linked`, else `relink` (the relink page follows `auth.state`, §8.3). The
one redial comes back only once a connection has stayed ready for `BackoffResetMs` (10 s), as the `SignalClient`'s
forced refresh does (WA42): a server that welcomes the PC and revokes the session at once, while Go's refresh keeps
succeeding, gets M1's reaction on the second stop instead of a loop. A
hello refused with `client_outdated` or `protocol_unsupported` calls `Auth.HelloRefused(serverOrigin, error)` and
shows 02 §13.3's screen from the returned state (`version`, §16.3). A `not_found` from either (the server isn't active
any more) stops C1 and waits for the reload (`wait`).

`DesktopPlatform` uses TypeScript `private` members, not `#private` fields (the build target lowers those into
helper code, which would count against the 4 KB budget), and keeps the C1-stop and update logic in `actions.ts`, a
separate chunk loaded on first use.

### 7.4 `NativeBridge`: one interface, two transports

`platform/desktop/bridge.ts` declares what the SPA needs from the native side, independent of how it travels:

```ts
export interface NativeBridge {
  app: { info(): Promise<AppInfo>; openExternal(url: string): Promise<void>; openOSSettings(p: OSPage): Promise<void>;
         quit(stopShare: boolean): Promise<void>; log(l: LogLevel, msg: string, attrs?: Record<string, string>): void;
         openLogsFolder(): Promise<void>; openNotices(): Promise<void>; ownConnections(ids: string[]): void;
         pendingLink(): Promise<PendingLink | null>; linkHandled(): Promise<void> };
  auth: { state(): Promise<AuthState>; servers(): Promise<ServerInfo[]>; probe(address: string): Promise<ServerProbe>;
          addServer(origin: string): Promise<void>; switchServer(origin: string): Promise<void>;
          removeServer(origin: string): Promise<void>; link(): Promise<void>; cancelLink(): Promise<void>;
          linkWithPassword(username: string, password: string): Promise<AuthState>;
          unlink(): Promise<{ confirmed: boolean }>; recheck(): Promise<AuthState>;
          accessToken(origin: string, force: boolean): Promise<AccessToken | null>;
          sessionEnded(origin: string, code: 'unauthenticated' | 'session_revoked' | 'account_disabled'): Promise<AuthState>;
          helloRefused(origin: string, e: { code: 'client_outdated' | 'protocol_unsupported';
                       params?: Record<string, unknown> }): Promise<AuthState> };
  share: { capabilities(): Promise<ShareCaps>; pick(): Promise<Picked | null>; release(pickId: string): void;
           start(r: StartRequest): Promise<ShareView>; current(): Promise<ShareView | null>; stop(): Promise<void>;
           stopOther(roomId: string, shareId: string): Promise<void>; setPaused(p: boolean): Promise<void>;
           setPreset(p: Preset): Promise<void>; setAudioEnabled(on: boolean): Promise<void>;
           stats(): Promise<ShareStatsSample | null>; watchServerStats(on: boolean): void };
  audio: { friendsHear(): Promise<FriendsHear>; watch(on: boolean): void; watchLevels(on: boolean): void;
           setAppDecision(appId: string, d: Decision): Promise<void>; setKeepOutMicUsers(on: boolean): Promise<void>;
           decide(promptId: string, d: PromptAnswer): Promise<void>; dismissPrompt(promptId: string): Promise<void>;
           dismissTip(code: string): Promise<void>; appRules(): Promise<AppRule[]>; soundCheck(): Promise<SoundCheckResult> };
  settings: { get(): Promise<SettingsView>; update(p: SettingsPatch): Promise<SettingsView>; hotkeys(): Promise<HotkeyStatus[]>;
              testHotkey(combo: string): Promise<HotkeyStatus> };
  window: { setFullscreen(on: boolean): Promise<void> };
  updates: { status(): Promise<UpdateStatus>; check(): Promise<UpdateStatus>; install(): Promise<void> };
  on<E extends keyof BridgeEvents>(e: E, fn: (p: BridgeEvents[E]) => void): () => void;
}
export type Decision = 'keep-out' | 'include' | 'auto';  // setAppDecision ('auto' = forget the rule)
export type PromptAnswer = 'keep-out' | 'include';        // decide; dismissing is dismissPrompt
// No audio-mode type or method: M2 has no "All sound" mode (§6.2).
```

- **`WailsBridge`** (M2): the generated bindings plus `@wailsio/runtime`'s `Events.On`; errors parsed per §6.4. As
  built (WA48, `wails/WailsBridge.ts`, which replaced WA32's stub): every method calls the generated binding of the
  same Go method, so a renamed, removed or re-typed Go argument fails the typecheck; results are cast to `bridge.ts`'s
  types, which keep the string unions the Go side documents (the generated classes type them as plain strings); a
  rejection's message is parsed into a `BridgeError` (W12), and a Wails `TypeError` or `ReferenceError` (a binding out
  of step with Go) becomes `internal`; the fire-and-forget methods log their failures, as `HttpBridge` does; events
  are `isshoni:<name>`, and `share.stats` gets `at` on receipt. `DesktopPlatform` loads it by dynamic `import()` only
  for `transport: "wails"`, so the runtime and the bindings stay out of the entry chunk.
- **`HttpBridge`** (M2, e2e only in practice), served by `internal/desktop/e2ehost` (§22.4) on the asset origin and
  selected only by `bridge.js`'s `transport`, which only `e2ehost` sets to `"http"`. The wire: a call is `POST
  /__bridge/call` with `Content-Type: application/json` and `{"service", "method", "args"}`, where `service` and
  `method` are the bound Go names (group `auth` → service `Auth`, method `accessToken` → `AccessToken`) and `args` is
  the argument list without trailing `undefined`s; a 2xx body is the method's JSON result (`null` or no body for
  none), any other status carries §6.4's bridge error JSON `{"code", "params"}` (a body that isn't one is
  `internal`). Events: an `EventSource` on `/__bridge/events`, opened at once, each message's data `{"name":
  "isshoni:<event>", "data": <payload>}`.
- **The DTO types** (`AuthState`, `ShareView`, `FriendsHear`, `SettingsView`, `UpdateStatus`, …) are written by hand in
  `platform/desktop/bridge.ts` from 02 §11.7, this doc's §6.2–§6.3 and §14.2, 05 §14.5 and 06 §8.9, and stay so with
  WA48's generated bindings, whose classes have the same fields with plain strings for the unions (above).
  `share.stats` and its event follow §6.3's `ShareStatsSample`, with `at` set by the transport on receipt
  (`performance.now()`).
- *Later (M4)*: a `RelayBridge` over 01's same-user relay (`share.request`, `share.status`, `exclusions.*`) lets a
  browser page drive the Linux agent through the same `DesktopSharing` code. Nothing of it is built now; the interface
  is shaped so its `share` and `audio` parts can be implemented over messages.

### 7.5 Routes in the desktop

New routes in `app/router.tsx` (the page-folder contract of 05 §5; folder `desktop/`, chunk `desktop`):

| Path | Page | Access | Notes |
|---|---|---|---|
| `/desktop/setup` | `SetupPage` | desktop only | the wizard (§8.2); `?add=<origin>` for a new server from a link, `--server` or `PendingServer` |
| `/desktop/relink` | `RelinkPage` | desktop only | the active server signed this PC out (§8.3) |
| `/desktop/settings` | `SettingsPage` | desktop only | §14.4; anchors `#servers`, `#sharing`, `#audio`, `#app`, `#updates`, `#about` |

- `RequireDesktop` (a layout route in `app/guards.tsx`): NotFound in the browser. In the desktop `/desktop` and
  `/desktop/` show NotFound too (an index route under it), not an empty layout.
- Two route tables (WA42): `createAppRoutes(sources, target)` and `preloadRoute(path, sources, target)` take a
  `RouteTarget` (`'browser'` by default; `routeTargetOf(platform)`), and the desktop's table leaves every page behind
  `RequireBrowser` unloaded, not only `/link`. The stub pages render M1's `PageUnavailable` ("Not in this build yet")
  until WA71 and WA82 fill them, and the `version` action reuses M1's `VersionMismatch` screen (no reload marker,
  never `stillStale`) until WA82 adds the desktop actions.
- `RequireBrowser`: wraps `/login`, `/invite`, `/signup`, `/pending`, `/reset`, `/setup`, `/admin/*`, `/account`,
  `/account/notifications`, `/account/devices`, `/download` and `/link` (02 §10.1: approval needs a cookie session; a
  bearer gets 403 on `/device/lookup`). In the desktop it calls `openExternal(serverOrigin + path + search)` once (for
  `/link` that keeps `?code=`) and renders "Opened in your browser" with a Back button; in the browser it renders its
  child. (D7.)
- `RequireAuth` in the desktop, when `['me']` is `null`, decides by `AuthState`: `unlinked` with a notice of a
  refused refresh (`signed_out`, `refresh_reused`, `account_disabled`; 02 §11.3–§11.4, §8.3) → `/desktop/relink`;
  `unlinked` with a link-step outcome (`denied`, `code_expired`, `save_failed`, which the wizard shows) or without a
  notice, `linking` and `incompatible` (the link step shows it with Try again, §22.3) → `/desktop/setup` (the link
  step for the active server); `linked` or `offline` (a 401 that a refresh cures) → refetch `['me']` after the next
  `auth.state`. Never `/login`. With no server it goes to `/desktop/setup` without asking for `['me']` (`apiFetch`
  would throw with no `serverOrigin`).
- The account menu in the desktop: "Settings" (`/desktop/settings`), "Switch server" (when more than one), "Account ↗"
  and, for admins, "Admin ↗" (both open the browser), and "Sign out of <server>" (§8.3) through `Auth.Unlink()`,
  never `POST /auth/logout` (02 §11.8). "Invite friends" stays (its dialog is a REST call). Not built yet: the menu is
  `web/src/rooms/AccountMenu.tsx`, which WA42's paths didn't include; WA71 builds it (README WA71's row), and until
  then the desktop still shows M1's menu with its Sign out and Notifications.
- `/about` works in both.

### 7.6 What the browser keeps unchanged

The browser runs the same code; it sees only these M2 changes, each of which is a plain improvement there too:
- hello `features` gains `agent.relay` and `user.connections` (`rooms/connection.ts`), so its tabs can receive
  `playback.stop` (§12.2), report `playback.state` to the user's desktop app (§12.2) and see the app online (§12.3);
- the "Now watching in the isshoni app" state of the viewer (§12.2);
- the "Open in the isshoni app" entry beside "Get the desktop app" (§12.3);
- the viewer's `paused` tile for any share whose status is `paused` (§11.3);
- `RoomPage` opens M1's connection test dialog when its URL has `?conntest=1` and drops the parameter with
  `history.replaceState` (the desktop's "Test connection", §10.1);
- the `SignalClient`'s optional `auth` (unused) and the `Platform` types of §7.1.

The rule from 05 §8 stays: nothing outside `platform/` reads cookies or `location.origin`, builds `/api` URLs or
constructs `RTCPeerConnection`. UI decides whether to offer Share from `capabilities().canShare`, never from `role`
(the desktop is `role: 'viewer'` and can share); DK4 audits the M1 code for `role` checks. WA42's audit found no
reader of `platform.role` but the hello in `rooms/connection.ts`, and three Share gates that read
`platform.sharing !== null` instead of `capabilities().canShare`: `rooms/RoomPage.tsx` (the header's Share button
and the `SharePanel`) and `rooms/RoomStage.tsx` (`EmptyRoom`). In the browser the two agree; in the desktop they
differ once WA65 plugs in `DesktopSharing`, whose engine can report `canShare` false (`unsupported_os`,
`no_encoder`), so WA65 switches them to `capabilities().canShare`, read asynchronously as `viewer/useControls.ts`
does (README WA65's row).

---

## 8. Servers, linking and the first-run wizard

### 8.1 Servers and the probe

- A **server** is an origin `https://host[:port]` with 02's entry in `servers.json` (02 §11.6: `name`, `addedAt`,
  `user` as `api.User {id, username, role}`, `device`, `lastRoomId`), its credential (target `isshoni:v1:<origin>`,
  02) and this doc's per-server preferences (`serverPrefs` in settings.json, §14.2). `servers.json` is the only
  server list and the only record of the active server. The app knows at most 20 ("You've added 20 servers. Remove
  one first."); **one is active**. The window shows the active server; `Auth.SwitchServer` is refused while a share
  is live (`busy`) and otherwise reloads the window with the new `bridge.js`. **Every change of the active server
  reloads the window** (`SwitchServer`, `AddServer`, a link to a known server, §8.2, §13.2), so a page only ever runs
  with the `bridge.js`, CSP and token origin of the active server; the origin check of §5.8 covers the moment before
  the reload. C2 follows the active server (§9).
- **The local id** (Web Storage prefix, `bridge.js`): derived, never stored: lowercase Crockford base32 of the first 5
  bytes of SHA-256(origin), 8 characters (`servers.LocalID(origin)`, with a Go and a TypeScript test vector).
- **Address normalization** (`servers.Normalize`): accepts `watch.example.com`, `https://watch.example.com`, an IP
  literal, or any URL of the server (`https://watch.example.com/r/lounge`). An address that has `/invite` or `/setup`
  as its path gets the hint "This is an invite link. Open it in your browser first to create your account" with a
  button that opens it in the browser (the fragment stays local to the browser). Then the path, query and fragment are
  dropped (never sent anywhere) and the rest goes through 02's `deeplink.NormalizeOrigin(raw, allowInsecureLoopback =
  dev || e2e build)`: `http:` only for `localhost`, `127.0.0.1` and `[::1]` in those builds. The address field and
  `--server` are the only places http loopback is ever accepted; a link never is.
- **Probe** (`Auth.Probe`, Go, no cookies): 02's `deviceauth.Client.Info` on a client made with `ClientOptions.HTTP`
  = a copy of `deviceauth.DefaultHTTPClient()` (no redirects, 64 KiB body limit, the system roots, 02's
  `User-Agent: isshoni-desktop/<version> (<os>)`) whose dialer has a 5 s connect timeout, under a 10 s context.
  First match wins:

  | Result | Code |
  |---|---|
  | DNS, TCP, connect or overall timeout | `server_unreachable`, with the extra line "Opens in your browser but not here? See why" that opens the project site's `DOCS_URL + 'troubleshooting#desktop-linking'` (`web/src/conntest/links.ts`, as M1's other docs links) through `openExternal` (02 DA12: proxies, filtering software and `HTTPS_PROXY`, which WebView2 and Go see differently). Not `<the typed origin>/troubleshooting…`: an isshoni server has no `/troubleshooting` route, and the SPA's catch-all would show its 404 (WA57-R6) |
  | TLS failure (untrusted, expired, wrong name) | `tls_untrusted`: "isshoni servers need a trusted certificate (04 §8)" |
  | status ≠ 200, or a body that is not 03's `Info` (02's `ErrBadResponse`) | `not_isshoni` |
  | `setupRequired: true` | `server_not_set_up` |
  | `compat.FromInfo(info, compat.Default())` not `ok` (02 §13.2; `reason` covers `no_device_flow`, a 0.1.x server) | `incompatible_server` with `{verdict, reason, serverVersion, minClientVersion}` |
  | otherwise | `ServerProbe {origin = NormalizeOrigin(info.server.publicUrl), name, version}` |

  The server's own `publicUrl` becomes the stored origin, because its Origin, Host and CSRF checks use that one
  (04 §4.4). When its host differs from what the user typed or what the add step showed, the confirm step shows it
  ("This server calls itself watch.example.com"). `incompatible_server` texts follow 02 §13.3 (`update_app`: "This server needs a newer isshoni
  app…"; `update_server`: "…Ask your admin to update it.").

### 8.2 The wizard (`/desktop/setup`)

Shown when no server is linked, and (only its add, confirm and link steps) for every server added later. `wizardDone`
in settings (per install) records that the sound and voice steps were seen once.

```
 [server]  only when no server came with a link, --server or PendingServer:
           "Installed from your isshoni page? Go back to it and click Open isshoni."
           or, as the fallback, "Your isshoni server: [ watch.example.com ] [Continue]"
     │ probe ok
     ▼
 [confirm] "Link isshoni to Friends (watch.example.com)?" · trust line · [Link] [Cancel]
     │ Link                  ▲ probe ok, but the server calls itself another host
     │                       │
     │      [add]  ?add=<origin> (link, --server, PendingServer): "Add server watch.example.com?" · no network
     │             request yet · trust line · [Add] [Cancel] → probe ──┐ probe ok, same host
     ▼                                                                 ▼
 Auth.AddServer(origin): it becomes active → the window reloads at /desktop/setup?link=1 with its bridge.js → Auth.Link()
     ▼
 [link]    device flow (02): "Friends (watch.example.com)" · trust line · big code WDJB-MJHT · QR code
     │      "or open watch.example.com/link and enter WDJB-MJHT" · browser opened at /link?code=…
     │      [Open browser again] [Copy link] · 10-minute countdown
     │      "You can rename this PC on the approval page or later on your Devices page."
     │      "Sign in with username and password instead" → username, password [Sign in] (02 §14)
     │ status linked
     ▼
 [sound]   sound check (first install only)        [voice]   "Friends won't hear: …" (first install only) → [Done]
     ▼                                                  ▼
 Done → /r/<lastRoomId of that server, else welcome.defaultRoomId>
```

- **Server step**: it leads with "Installed from your isshoni page? Go back to it and click **Open isshoni**." (the
  page's button carries the server and the room, 06 §7.5; this is what a friend sees who left the installer's Finish
  page on "Start isshoni"), and offers the address field below it as the fallback. The field takes focus, accepts a
  host or any URL of the server, including a pasted room or invite URL (§8.1); Enter probes; errors per §8.1 under the
  field. A link that arrives while this step is open is handled (it isn't a confirmation, §13.2) and opens the add
  step. With exactly one candidate from `--server`, a deep link or `PendingServer`, this step is skipped and the add
  step runs instead.
- **Add step** (`?add=<origin>`: a deep link, `--server` or the installer's `PendingServer`): shown first, **before
  any network request**, so a link can't work as a tracking pixel (02 §12.3): "Add server {host}?" with the host
  large, as `hostUnicode (host)` when the two differ (against look-alike names; both from the `deeplink.open`
  payload or `App.PendingLink()`, computed in Go by 02's `deeplink`), "Only add servers you
  trust. The server's admin can see what you share." [Add] [Cancel]. Only on Add does the probe run. When it returns
  ok and its origin (`NormalizeOrigin(info.server.publicUrl)`, §8.1) has the same host as the add step showed, Add
  goes straight to `Auth.AddServer(origin)` and the link step: the friend already answered the trust question once.
  When the host differs, the confirm step shows first ("This server calls itself …"), so the browser never opens at a
  host the friend didn't confirm. Probe errors show on the add step with [Try again] [Cancel].
- **Confirm step** (an address the friend typed, or an add step whose server calls itself another host): the probe's
  name and host; the trust line of §19; [Link] calls `Auth.AddServer(origin)`.
- **Activation reloads the window.** `AddServer` makes the new server active (02) and Go then reloads the window at
  `/desktop/setup?link=1`, so `bridge.js`, the CSP and the token origin name the new server before its link step
  renders. The reloaded page drops `link=1` with `history.replaceState` and calls `Auth.Link()` once, when
  `AuthState.status` is `unlinked` without a notice; that call answers the click on Link (or Add) a moment earlier,
  which is the user gesture 02 §11.3 asks for before the browser opens. Until the reload, the old page's `apiFetch`
  and C1 still talk to the old server, and every token request they make names the old origin and gets `null`
  (§5.8): no token of the new server ever reaches the old page.
- **Link step**: renders from `AuthState` (02 §11.7) of the active server, which is this page's server; it shows the
  server name and host and the trust line of §19 above the code:
  - Every URI this step opens, draws or copies is 02's canonical `LinkState` value (`verificationUri` = the server's
    origin + `/link`, `verificationUriComplete` = that + `?code=` + the user code; 02 §11.2 builds them in Go and
    refuses a server answer whose URIs differ, with `ErrBadResponse`), never a string from the server.
  - `status: 'linking'`: `link.userCode` large; a QR code of `link.verificationUriComplete`, drawn in the SPA with
    M1's `web/src/setup/QrCode.tsx` (uqr), moved to `web/src/ui/QrCode.tsx` if the page-folder import rule (05 §5)
    forbids `desktop/` importing from `setup/`; the short URL `link.verificationUri` with the code ("or open {url}
    and enter {code}"); the countdown to `link.expiresAt`. Go opens `verificationUriComplete` in the default browser
    once per new code (the click on Link is the user gesture, 02 §11.3); [Open browser again] repeats it;
    [Copy link] copies `verificationUriComplete`.
  - `status: 'linked'` → "Linked as {user.username}", then the next step.
  - `notice: 'denied'` → "Linking was declined in the browser. [Try again]" (`Auth.Link()`).
  - `notice: 'code_expired'` (600 s, 03 §7.12) → "The code expired. [Get a new code]" (`Auth.Link()`).
  - `status: 'incompatible'` → the version texts of 02 §13.3 with [Try again] (`Auth.Recheck()`).
  - a `Link()` error (`server_unreachable`, `rate_limited`) → its text with [Try again]; 02's `ErrBadResponse` (a
    verification URI that failed 02 §11.2's check) → the generic link error "Couldn't start linking with this server"
    with [Try again].
  - **"Sign in with username and password instead"** (02 §14: the plan's in-app fallback; the device flow stays the
    default): a link below the code opens a form with username and password fields and [Sign in] [Back]. [Sign in]
    calls `Auth.LinkWithPassword(username, password)` (§6.2), which cancels a running device-flow link first;
    `linked` continues as above; an error stays on the form with its text (`invalid_credentials`, `account_pending`,
    `account_disabled`, `rate_limited` with its wait, `server_busy`, `server_unreachable`; `validation_failed` with
    M1's `fieldErrors.username.*` / `fieldErrors.password.*` text under the field, e.g. a username that is too long)
    and clears the password field. [Back] returns to the code (`Auth.Link()`). The fields carry
    `autocomplete="off"`, WebView2's general autofill and password saving stay off (§5.4), and the SPA keeps the
    password only in the field until the call returns.
  Cancel (or leaving the page) calls `Auth.CancelLink()`. The app sends 02's default device name; the page says "You
  can rename this PC on the approval page or later on your Devices page." There is no name field.
- **Sound check step** (§8.4).
- **Voice step**: calls `Audio.Watch(true)` on mount (`false` on unmount). "Friends won't hear:" with 05's built-in
  voice apps from `FriendsHear.voiceApps` (Discord, TeamSpeak, Mumble, …; "running" from its `running` flag),
  "apps that use your microphone (you'll be asked)" with the toggle **Keep out apps that use my microphone** (on;
  `Audio.SetKeepOutMicUsers`), and "isshoni itself". The note "Change this anytime in Settings → Audio". **Done** sets
  `wizardDone`.
- The wizard has no "Skip" for linking (nothing works unlinked); Cancel on the first server quits to an empty
  "Link isshoni to your server" page with [Start].

### 8.3 Relink, sign out, remove

- **Relink** (`/desktop/relink`): entered only when `AuthState.status` is `unlinked` **with** a notice (02 §11.3;
  the server answers `invalid_grant` for every cause, and 02 turns it into a notice). Texts per notice:
  - `signed_out`: "This PC was signed out of {server} (removed on the Devices page, or not used for 90 days)";
  - `refresh_reused`: "Someone else used this PC's sign-in, so {server} signed it out. Link again and check your
    Devices page";
  - `account_disabled`: "Your account on {server} is disabled".
  It offers **Link again** (the link step for the same server, `Auth.Link()`), **Switch server** and **Remove
  server**. A live share was already stopped by the server (01 §3.2 revocation closes the connection).
- **`save_failed`** (the credential couldn't be written; linked for this run only) is a non-blocking banner, not the
  relink page: "Couldn't save the sign-in to Windows Credential Manager. You'll have to link again after restarting
  the app."
- **Who decides**: C1 stopped with `session_revoked` or `account_disabled` → the SPA calls
  `Auth.SessionEnded(serverOrigin, code)` (02 §11.8) and redials if the result is `linked` (a successful refresh means
  "dial again", not relink). C2's adapter calls `Manager.SessionEnded` itself (§9). The relink page follows `auth.state`
  only.
- **Sign out of <server>** (account menu or Settings → Servers): confirm "Sign out isshoni on this PC from Friends?"
  → `Auth.Unlink()`: stops a live share on it, 02's revoke (`POST /api/v1/device/revoke`) and credential delete,
  keeps the server as "Not linked". When the result is `{confirmed: false}` (the server wasn't reached): "Remove
  {device} on your Devices page to finish".
- **Remove server**: `Auth.RemoveServer(origin)` (02: Unlink first if linked, then drop the entry); this doc deletes
  its `serverPrefs` entry, and its prefixed Web Storage keys go at the next boot (§14.3).

### 8.4 The sound check, and what Windows asks instead of a capture permission

- **Windows has no screen-capture permission prompt for desktop apps.** The consent is the OS picker at every share
  (`GraphicsCapturePicker`, 04): a page or a link can't start a capture. Process-loopback audio capture needs no
  permission either, and mic detection reads audio sessions, not the microphone (05). So the Windows wizard has no
  permission step; `wizardSteps()` on Windows is `['link', 'sound', 'voice']`, and *later (M3)* macOS adds
  `'audio-permission'` before `'sound'`.
- Borderless capture (no yellow border, Windows 11 24H2) depends on Windows' own "Screenshot borders" privacy
  setting; 04 asks for it at the first share and reports it in the pick (`Source.OSBorder`, §10.1). When Windows
  refuses capture (`engine.permission_denied`), the guided fix opens `App.OpenOSSettings('graphics-capture')` (§10.5).
- **Sound check** (`Audio.SoundCheck`): 05 §11.3 owns the helper (`isshoni.exe --sound-check-helper`, dispatched by
  `main` before anything else, §13.4), the chime and its own capture path. The page shows "Playing a test sound…"
  with a spinner (no live meter: no levels exist during a check), then maps 05's `SoundCheckResult {captured,
  levelDb, ms, backend, reason}`:
  - `captured && backend === 'process-loopback'` → "✓ isshoni can capture your PC's sound, without your voice apps"
    and the measured `levelDb`;
  - `captured && backend === 'endpoint'` → "isshoni can capture your PC's sound, but it can't keep voice apps out on
    this PC. Set your voice app to a different output device. [Learn more]" (the plan's degraded banner);
  - `!captured` → by `reason`: `no_device` → 05's no-output-device text; `device_busy` (another app holds the
    default render device in exclusive mode, 05 §11.3) → `desktop.audio.soundCheck.deviceBusy`: "Another app is using
    your speakers on its own (exclusive mode). Close it, or turn off "Allow applications to take exclusive control of
    this device" in the device's Sound properties, then Try again." [Open sound settings]
    (`App.OpenOSSettings('sound')`) [Try again] [Continue]; `silent`, `activation_failed`, `timeout` and any reason
    this page doesn't know → "isshoni couldn't capture any sound (05's `audio.soundCheck.failed`). Friends will see
    your screen without sound. [Try again] [Continue]".
  The step never blocks Done.

---

## 9. Connections and presence

| | C1: SPA viewer | C2: Go publisher |
|---|---|---|
| Opened by | the SPA's room runtime (05 §7), like a browser tab | `internal/desktop/session` when a server is active and linked |
| `hello` | `role: viewer`, `client {kind: desktop, os, version, browser: edge}`, `auth` bearer, features `agent.relay`, `user.connections` | `role: publisher`, `client {kind: desktop, …}`, bearer, features exactly 01 §11.1's (`agent.relay`, `share.pause`) |
| In a room | from the room page's join until leave, logout or quit (05 §11.1) | **only while sharing**: 01's `StartShare` joins the share's room, `Share.Stop` leaves after the share ends (01 §11.4, D12) |
| Carries | `room.state`, subscriptions, the sub PC, `stats`, `agent.send` (`playback.stop`), `user.connections` | `share.*`, the pub PC, `quality.hint`, the relay (`playback.state` in → 05, §12.2; it serves no relay request kind and sends none in M2, 01 D16) |
| Recovery | 05 §11.1 and 01 §10 unchanged | 01-client-core (resume, `replaces`, the 30 s hold) |

- Both attach to the same Participant (01 §4.1), so presence shows one person; the people panel may show two
  connections of kind `desktop` for the user, which is what `room.state` says.
- **`internal/desktop/session`** runs `core.New(core.Options{ServerURL: <active origin>, Auth: <adapter>, Client,
  Engine, Logger})` and its `Run(ctx)` for the active linked server, and rebuilds it on a server switch, a relink and
  after a terminal stop that 02 cured. The adapter implements `core.Auth` over the `Manager` of the origin the core
  was built for, captured at construction (never "whichever server is active now", so a C2 still connected to the old
  server during a switch can't receive the new server's token, §5.8): `AccessToken(ctx, force)` =
  `Manager.AccessToken(ctx, force)` (02's own forced-refresh rule); `ErrNotLinked` is wrapped in an error whose
  `RelinkRequired()` returns true; on `ErrIncompatible` the session closes the core and the version screen shows
  (§16.3); any other error is transient. The session closes that core before it builds one for a new active server.
- **A share follows the SPA's room.** Switching rooms in the SPA while the app shares asks the M1 question "Stop
  sharing and switch to <room>?" (05 §11.1); yes stops the native share (01's `Share.Stop` leaves the room) before C1
  joins. `session.leave()` stops it too. A deep link to another room while live asks the same (§13).
- **The SPA reloads, the share stays.** C1 may drop (page reload, WebView2 crash) while C2 keeps sharing. At boot the
  runtime asks `platform.sharing.current()`: a live share makes its room the desired room, and after the first
  `room.state` the runtime calls **`RoomSession.adoptShare(share)`** (new, `rooms/RoomSession.ts`): it sets
  `session.share` and wires its events exactly as `startShare` does.
- Watcher counts for the tray and the pill come from `core.ShareSnapshot.Watchers`; the names in the panel from C1's
  `room.state`, as in M1.
- C2 that isn't ready for 10 s when `Share.Start` runs → `publisher_offline` ("Can't reach the server", plus the
  line "Opens in your browser but not here? See why", which opens the project site's `DOCS_URL +
  'troubleshooting#desktop-linking'` through `openExternal`, as §8.1 does; no proxy detection, and Go's
  `http.Transport` is unchanged). C2's stops
  (01 §11.3): `update_required` → `compat.FromError` → the version screen (§16.3); `unlinked` with code
  `session_revoked` and `account_disabled` → the adapter calls `Manager.SessionEnded(ctx, code)` and rebuilds the core
  if 02 stays `linked`; otherwise the relink page follows `auth.state`.

---

## 10. Sharing

### 10.1 The share controller (Go, `internal/desktop/share`)

The controller is the single owner of the share; the SPA, the tray, the hotkeys and the pill all call it. It calls
only 01's `core.Client.StartShare` and `core.Share` methods for the share itself (the core joins and leaves the room,
sends `share.start`/`share.stop` and runs the engine) and 05's `audio.Service` for the audio policy; it owns the
hotkeys, pill, keep-awake and tray.

```
          Pick()                      Start(pickId, roomId, preset, withAudio)       01: State live
 idle ─────────► picking ──picked──► picked ───────────────────────────────► starting ─────────────────► live
   ▲               │ cancel            │ Release / 300 s                         │ error                 │  ▲
   │               ▼                   ▼                                         ▼                       │  │ SetPaused
   ├─────────────── idle ◄──────────── idle                                    failed ──(dismiss)──► idle │  ▼
   │                                                                                                    paused
   │     C2 down or pub PC recovering: live|paused ⇄ reconnecting ⇄ unreachable (01; 30 s hold)           │
   │                                                                                                      │
   └──── stopping ◄── Stop(): SPA, tray, hotkey, pill · source_gone · server ended · SPA left the room ·  ─┘
                      sign out · sleep · end of session · Quit
```

- **Pick**: `eng.Pick(ctx, media.PickRequest{OwnerWindow: shell.MainWindowHandle()})` opens `GraphicsCapturePicker`
  owned by the main window (the window is shown and focused first); `errors.Is(err, media.ErrCancelled)` → `null`. The
  `media.Source` is held under a random `pickId` (16 hex) for 300 s from the pick, 04's token life (04 §7.2), so a
  friend who reads the first-share sheet or follows the DND tip into Windows Settings comes back to a pick that still
  works; one pick at a time (a new `Pick` releases the old one). A pick of an isshoni window (the binding returns
  `*media.Error{Code: invalid, Reason: "self_window"}` and has already released it, README §3 item 17; `media.Source`
  has no `Self` field) makes `Pick` reject with `engine.invalid`, `params.reason: "self_window"` (§10.5). `kind` =
  `Source.Kind`, `label` = `Source.Name` (the controller fills it; local only), `border` from `Source.OSBorder` at pick
  time and from `core.ShareSnapshot.NoOSIndicator` once the share is live (the fields 04 and 01 add). A window share's
  pill goes on the primary monitor unless 04 adds a monitor to `Source`. `confirm` lists the one-time sheets due
  (§10.2). `Pick` has no bound of its own: the picker's 120 s timeout (04 §7.2) applies. The 300 s hold is
  `share.PickHold` (`Options.PickHold` overrides it in tests); README WA58's row and 01 §16 said 120 s, the picker's
  timeout mixed in (README §3 item 45).
- **Start**, in order: C2 ready (≤ 10 s, else `publisher_offline`) → `audio.OnShareStarted(uint32(src.PID),
  withAudio)` (the shared app is never auto-excluded, PLAN; `src.PID` is 0 for a monitor) →
  `core.Client.StartShare(ctx, core.ShareRequest{RoomID: roomId, Source: src, Preset: preset, Audio: withAudio,
  Label: ""})` with its own 10 s → `starting`. 01's `ShareRequest` has no `Exclusions` or `Borderless` (WA58): 05's
  exclusions reach the engine through C2's `core.Options.AudioConfig` (05's `AudioConfig()`, §6.1), and the core takes
  `Borderless` from the probe itself (01 §11.4 step 3). The core joins the room itself (01 §11.4 step 1; its errors
  `room_not_found`, `room_full`, `forbidden` come back as wire codes). If `StartShare` fails, `audio.OnShareStopped()`
  is called, and the view shows `failed` with the error while the pick stays held for another `Start`, unless the
  engine said `source_gone` or `pick_expired` or a `Stop` aborted the start (that start ends with the Stop's cause).
  `serverPrefs[origin].preset` remembers the preset.
- **Pause** (§11.3): `core.Share.SetPaused(ctx, p)`, then, once it succeeded, `audio.SetPaused(p)` (05's extra mute;
  not the mechanism). On a server without `share.pause` it returns `feature_disabled` and changes nothing (§6.2,
  §11.3), rather than 01's local-only pause.
- **Preset**: `core.Share.SetPreset(ctx, p)` (C2's `share.update`). **Share sound**: `audio.SetShareSound(on)`, local
  only (§10.3).
- **Stop** is idempotent and ordered: `core.Share.Stop(ctx)` (the core runs `share.stop` → `pc.close` → Publisher
  closed → engine `Stop` → `room.leave`, 01 §11.4) → `audio.OnShareStopped()` → hotkeys unregistered, pill hidden,
  keep-awake off, tray idle. On **every** share end (also one the server or the core made), the controller runs the
  same local steps once when `Share.Done()` closes, `audio.OnShareStopped()` included. As built (WA58): the bound
  `Share.Stop` waits for the server at most 10 s (the local part always runs), and `Share.StopOther`'s 10 s include
  the wait for C2.
- **05's calls** (WA58): through `share.AudioPolicy`, which `app.New` passes when `Deps.Audio` implements it (§6.1):
  `OnShareStarted` before `StartShare`, `OnShareStopped` at every end, `SetPaused` after the core's pause and
  `SetShareSound`. The `SetSharedPID` hand-off at pick, release and expiry is WA77's.
- **Views** (WA58): `Current()` and `share.state` show the picker's view (`picking`) while it is open, then the share's;
  after an end they keep the last share's final view (`ended` or `failed`) until the next `Pick`, and carry `null`
  when there has been no share. 01's `reconnecting` shows only once it has lasted 2 s (`share.ReconnectingAfter`), so
  a short drop that 01 cures doesn't flash "Reconnecting…" in the panel, the tray and the pill; `unreachable` shows at
  once.
- **Notices**: 01's `EventNotice` `encoder_fallback {encoder}` becomes `share.notice` (§10.5). `audio_rebuilt` is
  logged only; the audio state shows through 05's banners.
- `liveSince` is stamped when the controller first sees `live` and kept across `replaces` (the share's id changes,
  its local identity doesn't).
- The controller is pure Go over interfaces and tested with `testing/synctest` on every OS.

**States** (01's `ShareState` and `Paused` → `ShareView.state`; `picking` and `stopping` are the controller's own):

| 01 | `ShareView.state` |
|---|---|
| `joining`, `starting` | `starting` |
| `live` with `Paused` | `paused` |
| `live` | `live` |
| `reconnecting` | `reconnecting` |
| `unreachable` (5 rebuilds without connecting, 01 §11.6) | `unreachable`: "Can't reach the server's media port" with [Test connection], which opens `serverOrigin + '/r/' + roomId + '?conntest=1'` in the browser (M1's connection test dialog, §7.6) |
| `ended` | `ended`, or `failed` for reason `error` |

**End reasons** (01's `EndInfo` → `endReason`):

| `EndInfo.Reason` | `endReason` |
|---|---|
| `user` | `user`, `hotkey`, `tray` (also the pill's Stop) or `system` (sleep, end of session): the cause the controller passed with its own `Share.Stop` |
| `source-gone` | `source-gone`: "The window you shared was closed" |
| `server` with `EndInfo.Server` ∈ {`stopped`, `left`} and `!EndInfo.ByThisApp` (fields 01 adds) | `elsewhere`: "Sharing was stopped from another device" |
| any other `server` | `server` (e.g. `media_timeout`) |
| `server-unreachable` | `server-unreachable` (01's 30 s hold ran out) |
| `unlinked` | state `ended`, `endReason: unlinked`; the session adapter calls `Manager.SessionEnded` when `StopError.Code` is `session_revoked` or `account_disabled` (§9); the relink page follows `auth.state` (§8.3) |
| `error` | state `failed`, `error.code` = `engine.<media code>` with `params.reason` = `EndInfo.MediaReason` when `EndInfo` names a media code (README §3 item 43: until WF4 adds the field, `params.reason` is empty, so §10.5's `device_removed` row can't show for a live share), else the protocol code |

### 10.2 The SPA flow

`DesktopSharing` (`platform/desktop/DesktopSharing.ts`) is a `SharingProvider` with `mode: 'native'`:
- `pick()` → `bridge.share.pick()` → a `PickedSource {kind, audioScope: withAudio ? 'system' : 'none', warning: null,
  preview: null, label, confirm, release}`; no transient-activation rule applies, but M1's ShareButton still calls it
  first in the click handler, which is fine. The SPA counts on the same 300 s hold as Go (§10.1, README §3 item 45;
  `bridge.ts`'s `Picked` said 120 s until WA65): a `start` after it gets `pick_expired`, which reopens the picker
  (§10.5).
- `start(src, opts, ctx)` → `bridge.share.start({pickId, roomId: ctx.roomId, preset, withAudio})` → a
  `DesktopShare`. `ctx.signal` is unused (C2 publishes).
- `current()`, `localConnectionIds()` (`[publisherConnectionId]`), `stopOther(roomId, shareId)` per §7.1.

`DesktopShare` implements `ActiveShare` and 05 §11.1's `ShareRecovery`: it mirrors `share.state` (its `shareId`
changes when C2 re-publishes with `replaces`, and it emits `state` then); `resync` and `republish` resolve at once
(C2 recovers on its own); `serverEnded` only updates the UI, since C2 reports the authoritative end; `stop()` calls
`Share.Stop()`; `stats()` returns `share.stats`' last sample. Its `hint` event comes straight from `ShareView.hint`:
native shares use no `HintDetector` (01 already applies the web's 6 s on / 10 s off rule in ratectl, and native
shares keep their resolution, 01 D11, so M1's height-based detector would never fire).

The M1 share machine (05 §13.1) gains two `confirming` reasons beside the whole-screen warning, both rendered by
the host `ShareButton` from `PickedSource.confirm`, in order:

1. **First-share sheet** (`'first-share'`, once per install, `tips.firstShareShown`; `Audio.Watch(true)` while
   open): "Ready to go live?" · "Friends will see: <label>" · the three groups of `FriendsHear` with the row rules of
   §10.3 ("Friends will hear:" `heard`; "Friends won't hear:" `keptOut` with their reasons; "Not captured:"
   `notCaptured`), or "everything on this PC" when `engine === 'endpoint'` · under Share sound, the line
   `desktop.audio.sheet.volumeNote` "Muting your PC doesn't mute your stream. Use Pause or turn Share sound off." ·
   the "Optimize for" picker · the line "Pause: {pauseCombo} · Stop: {stopCombo}, also from the tray icon" with the
   registered combinations (`desktop.share.first.hotkeys`; omitted for a hotkey that isn't registered) · **Go live** ·
   Cancel. Shown before live, because it is the moment the user learns what
   leaves the PC.
2. **DND tip** (`'dnd'`, the first share of `kind: 'screen'`, `tips.dndShown`): "Sharing your whole screen? Turn on
   Do not disturb so notifications don't show up for friends. Allow isshoni in priority notifications, so its
   warnings still reach you." · **Open Do not disturb settings**
   (`App.OpenOSSettings('dnd')`) · **Share now**.

The **desktop ShareSheet** (opened by the Share button; `Audio.Watch(true)` while open) replaces the browser's tip
with: the "Optimize for" picker (Auto, Game, Movie, Text, one line each, as 05 §13.5), a **Share sound** checkbox (on)
with the `volumeNote` line under it, a one-line "What friends hear" summary ("Friends will hear 2 apps · Discord kept
out · [Details]" from `Audio.FriendsHear()`; before a share 05 lists playing apps without capturing them), and
**Share** (→ picker). While the picker is open the sheet shows "Choose what to share in the Windows picker…". "You're
already sharing from another tab or device" (05 §13.1) counts the app's own C2 share as local, through
`findShareElsewhere(shares, {userId, connectionIds: [c1, ...localConnectionIds()]})` (`share/elsewhere.ts` takes a
list now); its **Stop it** calls `platform.sharing.stopOther(roomId, shareId)`. No `beforeunload` is registered for
native shares (it is `BrowserShare`'s).

### 10.3 The share panel in the desktop

M1's `SharePanel` (05 §13.7) with native extras from `share/native/` (lazy, loaded only when
`platform.sharing.mode === 'native'`):

- **Bar**: red dot, "You're live · <label> · Game · 3 watching", **Pause**, **Stop**. Paused: amber, "Paused ·
  nothing new leaves your PC; friends see "Paused" and hear nothing", **Resume**, **Stop**. Reconnecting:
  "Reconnecting…". Unreachable: "Can't reach the server's media port" [Test connection] (§10.1). The hotkeys are
  named in the buttons' tooltips ("Pause (Ctrl+Alt+Shift+F9)"). While `ShareView.minimized` is true, a line under the
  bar says "Your shared window is minimized: friends see the last picture" (04's `SOURCE_MINIMIZED`, §10.5).
- **Expander**:
  - **Optimize for**; **Share sound** (on/off: `Share.SetAudioEnabled` → 05's `SetShareSound`; shown only for a
    share started with sound, as in M1: a share started without it has `ShareRequest.Audio = false` and no audio
    track). Friends hear nothing while it's off (05's mixer keeps sending silent Opus frames, so the track never looks
    stalled; nothing goes on the wire, `protocol.ShareUpdate` has no audio field).
  - **What friends hear** (`Audio.WatchLevels(true)` while the expander is open): three groups from 05's
    `FriendsHear`: `heard`, `keptOut` and `notCaptured`. Each row has its level bar (−60 to 0 dBFS from
    `audio.levels`' `apps[appId].peakDb`, 10 Hz), its reason from 05's `reason` keys (`builtin`, `user`, `mic`,
    `viewer`, `isshoni`, `launcher`, `tree` with `detail`, `limit`, `capture-failed`), and its warning from
    `row.warning` with 05's texts. **Keep out** or **Include** shows only when `row.control === 'toggle'`
    (`Audio.SetAppDecision(appId, …)`); a locked built-in shows `desktop.audio.warning.sharedVoiceApp` when it is the
    shared app, else its reason only. The overall level meter (`mixPeakDb`). M1's "No sound captured" rule (10 s of
    digital silence) does **not** apply to native shares: with the include set or the fast path a quiet PC is digital
    silence, so the line would read as a fault whenever nothing plays, and the real failure (a playing app captured
    as silence) is 05's silent-stream check and its `capture-trouble` banner (05 §8.5). Instead, while sound is on and
    no `heard` row is `playing`, a neutral line says "Nothing is playing right now". `FriendsHear.tips` (05's ducking
    tip) show with a dismiss button (`Audio.DismissTip`) and "Open sound settings"
    (`App.OpenOSSettings('sound-communications')`).
  - **Watchers** (M1).
  - **Details** (a disclosure, also toggled with Ctrl+Shift+D while the window has focus): encoder and HW; per layer
    width × height @ fps, target and sent kbit/s, CaptureToSend p50/p95; and the server's view of the layers. Data from
    `share.state` / `Share.Current()` (`ShareView.encoder`, `hw`, `layers`, `server`); while open it calls
    `Share.WatchServerStats(true)` (`false` on close; the bridge counts the open disclosures and the controller keeps
    the wish across shares, §6.2). The server's view arrives as `ShareView.server = {layers: [...]}` (§6.3). It shows
    no source or app names. 01 §12.2's latency run reads the encoder and the full layer's bitrate here.
- **Banners** (always visible, PLAN "Transparency"): `FriendsHear.banners[]` as they arrive, codes from 05 §14.7 and
  texts from 05 §14.8's keys (in the `desktop` namespace as `desktop.audio.*`, §18.1). This doc has no banner strings
  of its own.
- **Mic prompt card**: from `audio.prompt` / `FriendsHear.prompts`; until answered, the app is kept out (05's
  default). The card, at the top of the panel and also as a non-modal card in the window's corner while the panel is
  closed, chooses its text and buttons by `Prompt.kind`: `mic` → "KOOK is using your microphone. Keep it out of your
  stream? [Keep out] [Include]"; `browser-mic` → 05's browser text with [Keep browser out] [Include]. Buttons call
  `Audio.Decide(promptId, 'keep-out' | 'include')`; dismissing (the card's ✕ or Esc) calls
  `Audio.DismissPrompt(promptId)`: the app stays kept out and isn't asked about again this run, and nothing is saved
  (05 §14.6). 05 remembers an answer per app. When the window isn't in the foreground, the same question goes out as a toast (§15).
- **Upload hint**: from `ShareView.hint` (`upload-limited` → "Your upload allows about {approxHeight}p";
  `cpu-limited` → M1's CPU text).
- **Encoder notice**: on `share.notice` `encoder_fallback`, the panel shows "Switched to <encoder>" for 5 s.

On the Mac and in the e2e harness the same `audio.Service` runs on 05's fake engine audio (`ISSHONI_FAKE_AUDIO`
scenarios, 05 §13.3), so every row rule, prompt and banner above is exercised without Windows.

### 10.4 The app's own share in its viewer

The native share is "own" (same user) but comes from C2, not from the page. With `localConnectionIds()`,
`syncRoom` marks it `local` (05 §12.2); a local share without a preview stream (the desktop has none) is:
- subscribed with **video `low` at most and audio `off` always** (`viewer/layerPolicy.ts`, a new rule for local shares
  without `localPreviews`), so its tile and the stage show the preview layer (360p15, about 0.3 Mbps) labeled
  "Preview: what friends see";
- never focused automatically and never audible (M1's rules for own shares);
- not counted as watching (the server counts participants other than the owner, 01 §4.1).

While the app shares in 05's `endpoint` engine, every other share in its viewer is muted too (§12.1).

### 10.5 Engine errors → UI

Keyed on 01's `media.ErrorCode` (bridge code `engine.<code>`) and then on `media.Error.Reason` (`params.reason`,
04 §5.7's detail tokens). A reason row wins over its code's row; `EncoderInfo.Status`/`Reason` from the probe
(01 §5) feed the same rows when Share is disabled at probe time.

**By reason** (the code in brackets is what 01 and 04 return after README §3 item 17: `IM_ERR_DEPENDENCY` →
`dependency`, `IM_ERR_NO_ENCODER` → `no_encoder`, a self pick → `invalid`/`self_window`):

| `params.reason` | During | UI |
|---|---|---|
| `pick_expired` (`source_gone` from `Start`; also the controller's own `pick_expired` for a `pickId` it no longer holds) | start | **Pick again**: the OS picker reopens (`Share.Pick()`), no error text (04 §5.7) |
| `self_window` (`invalid` from `Pick`; the binding already released the token, §10.1) | pick | the OS picker reopens with one line above the share sheet: "Pick something other than isshoni" (`desktop.errors.engine.reason.selfWindow`) |
| `driver_too_old` (`dependency`; an encoder's probe status) | probe, start | "Update your graphics driver (NVIDIA 531.61 or newer)" with a link to the project site's `/troubleshooting#desktop-gpu-driver` (04 §19). When another encoder took over (the probe lists NVENC with this reason and a working fallback), the same text is a quiet note in Settings → Sharing → Video encoder and in the Details disclosure, never a banner |
| `no_media_foundation` (`dependency`; MF encoder status) | probe | "This edition of Windows (N or KN) needs Microsoft's Media Feature Pack for video" with a link to `/troubleshooting#desktop-media-feature-pack`; when no encoder works, Share is disabled with this text instead of `no_encoder`'s |
| `blocked_by_policy` (`dependency`) | probe, start | "Windows (Smart App Control or your organization's policy) blocked part of isshoni's video engine." with a link to `/troubleshooting#desktop-smart-app-control`; when 04 offers it, [Enable Cisco OpenH264…] (Cisco signs its binary, 04 §17) |
| `missing`, `version`, `abi` (`dependency`) | open, probe, start | "Part of isshoni is missing or damaged. Reinstall isshoni from your server's Download page." [Open download page] (`serverOrigin + '/download'`); sharing off, viewing works |
| `device_removed` (`encoder_lost` from `Next`: the GPU was removed or reset, 04 §5.7, §9.8) | live | the share ends with code `encoder_lost`: state `failed`, toast and panel "Your graphics driver restarted, so the share stopped." [Share again], which starts a new pick (`Share.Pick()`; the engine's next session uses a new device). The reason reaches the view through 01's `EndInfo.MediaReason` (README §3 item 43, WF4); WA58's `endError` had only `EndInfo.Media`, so until WF4 this row showed as `encoder_lost`'s |

**By code** (when no reason row applies):

| `media.ErrorCode` | During | UI |
|---|---|---|
| `permission_denied` | pick or start | Sheet: "Windows didn't allow isshoni to capture that." [Open Windows settings] (`graphics-capture`: `ms-settings:privacy-graphicsCaptureProgrammatic`, and `…WithoutBorder` for borderless) [Try again] |
| `encoder_lost` | live | Fallback is the engine's (01 D15, 04 §9.8): it switches the layer to the next encoder and emits `EventEncoderChanged`, which 01's core turns into `share.notice` `encoder_fallback {encoder}`; the panel shows "Switched to <encoder>" for 5 s, no toast, and the share stays live. There is no core-level fallback and no per-share count. `encoder_lost` from `Next` means no candidate is left: the share ends with code `encoder_lost`, state `failed`, "Your PC's video encoder stopped working" [Settings → Sharing] (the `device_removed` reason has its own row above) |
| `source_gone` | live | share ends `source-gone`: toast and panel "The window you shared was closed" |
| audio device change (05's report, never an error code: 05 §4.3) | live | video continues; banner `reconnecting-sound` from `FriendsHear.banners` while 05 is `rebuilding` |
| `unsupported_os` | probe | Share is disabled: "Sharing needs Windows 10 22H2 or later" (viewing works) |
| `no_encoder`, `dependency` (no reason row) | probe | Share disabled with "No H.264 encoder works on this PC" and, when 04 offers it, [Enable Cisco OpenH264…] → Settings → Sharing (§14.4) |
| `SOURCE_MINIMIZED` (an event, not an error: `ShareView.minimized`) | live | a line under the panel bar: "Your shared window is minimized: friends see the last picture"; it goes away on `SOURCE_RESTORED` |
| `internal` (and `invalid`, `busy`) | any | share `failed`: "Something went wrong in isshoni's video engine" [Open logs folder] (the detail is logged, never shown; 01 §5.2) |
| `cancelled` | pick | never surfaced: `Share.Pick()` returns `null` |

### 10.6 Optimize for

Same four presets as M1 (05 §13.5); the server turns a preset into `ShareParams` (02-sfu M1). C2 sends `preset` in
`share.start` and `share.update`; 01's core applies the encodings and the native rate control. The panel's picker
calls `Share.SetPreset`. The desktop never relies on a `quality.hint` with reason `preset` reaching C2 (M1 decided
after group 5): the publisher itself sends the update.

---

## 11. Share safety

### 11.1 Tray (`internal/desktop/tray`, pure; Wails tray in `shell.go`)

```go
type State struct {
	Mode        Mode   // Idle, Live, Paused, Reconnecting
	Source      string // the pick's label, local only
	Watching    int
	Hotkeys     map[HotkeyID]string // registered combos, for the menu
	UpdateReady string              // a version in update state ready, or ""
	VoiceAppsHeard bool             // 05's FriendsHear.alert.voiceAppsHeard (live only)
	Questions      int              // 05's FriendsHear.alert.questions: pending mic prompts
}
type View struct { Icon IconID; Tooltip string; Menu []MenuItem }
func Render(s State, t *i18n.Catalog) View
```

| Mode | Icon | Tooltip (≤ 127 characters; `Source` cut to 40 with "…") | Menu |
|---|---|---|---|
| Idle | `tray-idle` | "isshoni" | Open isshoni · (Restart and update to 0.3.0) · Quit |
| Live | `tray-live` (red) | "Sharing Minecraft · 3 watching" | *Sharing Minecraft · 3 watching* (disabled) · Pause sharing (Ctrl+Alt+Shift+F9) · Stop sharing (Ctrl+Alt+Shift+F10) · — · Open isshoni · Stop sharing and quit |
| Paused | `tray-paused` (amber) | "Sharing paused · 3 watching"; "Sharing paused since you locked your PC · Resume sharing?" while a lock pause awaits an answer (§11.6) | as Live with Resume sharing (first, and bold while a lock pause awaits an answer) |
| Reconnecting | `tray-warn` | "Sharing · reconnecting…" | as Live (also for `unreachable`) |

- **Audio alert outside the window** (05 §14.7 "Outside the window"): while `VoiceAppsHeard`, the icon is
  `tray-warn` whatever the mode, the tooltip and the menu's first (disabled) line read "Friends hear voice apps"
  (`desktop.native.audio.voiceAppsHeard`) before the mode's own text; while `Questions > 0`, the menu gains "1 question
  waiting" / "N questions waiting" (`desktop.native.audio.questionsWaiting_*`) right after that line, which opens the
  window on the mic prompt card.
- A left click opens and focuses the window. The tray icon exists for the app's whole life.
- Updates are debounced to 250 ms, so a burst of watcher changes rebuilds the menu once.
- "Restart and update" appears only in Idle (never while a share is live) and runs `Updates.Install()`.
- Windows 11 hides new tray icons in the overflow; the one-time close notice (§11.6) says where the icon is, and
  Settings → App has "Keep the icon visible" opening `ms-settings:taskbar` (`App.OpenOSSettings('taskbar')`).

### 11.2 Global hotkeys

- Implemented in `winos` with `RegisterHotKey(hiddenHwnd, id, mods | MOD_NOREPEAT, vk)`; `WM_HOTKEY` → `Hotkeys()`.
- Registered when a share goes `live`, unregistered when it ends (D9). A combo that is taken
  (`ERROR_HOTKEY_ALREADY_REGISTERED`) is reported as `taken`: the panel shows "Pause hotkey unavailable: another app
  uses Ctrl+Alt+Shift+F9" with a link to Settings; the buttons, tray and pill still work.
- Defaults `Ctrl+Alt+Shift+F9` (Pause/Resume toggle) and `Ctrl+Alt+Shift+F10` (Stop, no confirmation; D9: no keyboard
  layout types a character on them, unlike a letter with Ctrl+Alt, which Windows also delivers for AltGr). Settings
  records a new combo by key press (`HotkeyField`): it needs two modifiers, or an F-key with at least one; `Win` is not
  allowed (Windows owns most `Win` combos); `Settings.TestHotkey` checks it at once and the field warns "This
  combination types a character on your keyboard layout; typing it during a share would trigger the hotkey" when
  `typesChar` is set (§6.2). The same check runs when the hotkeys are registered at `live`, and a combination that
  types a character is logged at info (the combination, never the character).
- Format (`internal/desktop/hotkey`): `Ctrl`, `Alt`, `Shift` in that order, then the key: `A`–`Z`, `0`–`9`, `F1`–`F24`,
  `Pause`, `ScrollLock`, `Insert`, `Home`, `End`, `PageUp`, `PageDown`, `Num0`–`Num9`. Parse and format round-trip;
  fuzzed.
- *Later (M3)*: macOS hotkeys behind the same `OSHooks` methods.

### 11.3 Pause

- **Meaning** (PLAN, 01 D14): nothing captured while paused leaves the PC. Viewers who had video keep the last frame,
  dimmed, with "Paused"; they hear nothing.
- **Path**: `Share.SetPaused(true)` (button, hotkey, tray, pill, lock) → the controller → 01's core:
  1. 01's core drops video and audio at its pump from the moment `Share.SetPaused(true)` is called (01 §11.5, §11.8).
     The engine keeps capturing, so resume is instant. 05's mixer mute (`audio.Service.SetPaused`, called by the
     controller once the core's call succeeded) is an extra, not the mechanism;
  2. C2 sends `share.update {shareId, paused: true}` (feature `share.pause`, 01 §10.3); the share's status in
     `room.state` becomes `paused`;
  3. keyframe requests are ignored while paused (core and SFU, `sharerPaused`): a keyframe made now would encode a
     frame captured during the pause. Viewers who already had video keep the last frame, dimmed, with "Paused";
     viewers who join while paused see a "Paused" tile with no frame.
  Resume reverses it: the pump asks for keyframes and forwards from them, then `share.update {paused: false}`, then
  `audio.SetPaused(false)`.
- A server without `share.pause` in C2's `core.Status().Features`: `Share.SetPaused` answers `feature_disabled`, the
  Pause button, tray item and pill button are disabled with the tooltip "Pause needs a newer server", the Pause hotkey
  is not registered (Stop still is), and a screen lock does not pause (the share goes on, as it would in a browser).
- **Viewer** (`viewer/`, all platforms): a share with status `paused` shows its last frame, dimmed, with "Paused" and
  the sharer's name when the viewer has one, else a Paused placeholder; its audio is silent anyway. M1's freeze and
  "connection unstable" detection is off while the share's status is `paused` (01 §10.3 leaves this to 03). The M1
  tile states (05 §12.6) gain `paused`.
- **Feedback** where the user is: the panel and pill turn amber, the tray icon changes; when no pill is shown and
  the window isn't in the foreground, a toast "Sharing paused. Ctrl+Alt+Shift+F9 resumes" (once per pause; the
  registered combination, from settings).

### 11.4 The Sharing pill (`winos`, Windows)

- **Required** while a share is live and its capture has no border (`ShareView.border === false`: WGC on
  Windows 11 24H2 with the border off, or Desktop Duplication on Windows 10, 04). It can be collapsed, not closed.
  Settings → Sharing → "Sharing pill": **Only when Windows shows no border** (default) or **Always**.
- **Window**: `CreateWindowExW(WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE | WS_EX_LAYERED, …, WS_POPUP)`,
  rounded corners (`DWMWA_WINDOW_CORNER_PREFERENCE = DWMWCP_ROUND` on Windows 11), per-monitor DPI aware
  (`GetDpiForWindow`, `WM_DPICHANGED`), `SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE)` before it is first
  shown. A failed affinity call (older build) hides the pill and logs WARN: a pill that would be captured is not shown.
- **Content** (about 260 × 36 px at 100 %): a red dot, "Sharing · 3 watching" (or "Paused", "Paused · Resume
  sharing?" while a lock pause awaits an answer, §11.6, or "Reconnecting…"; "Friends hear voice apps" replaces the
  watcher count while `PillState.VoiceAppsHeard`), an amber marker beside the dot while `PillState.Questions > 0` (a
  mic prompt waits; a click on it opens the window on the prompt card), and two
  standard owner-drawn `BUTTON` children (Common Controls v6, from the app manifest), **Pause/Resume** and **Stop**,
  so Narrator reads their names; colors follow high contrast (`SPI_GETHIGHCONTRAST` → system colors). The label area
  is a drag handle (`WM_NCHITTEST` → `HTCAPTION`); a double click collapses it to a 24 px red dot (and back).
- **Position**: top center of the captured monitor (for a window share: the primary monitor, unless 04 adds the
  window's monitor to `media.Source`), 8 px below the top; the last position per monitor device name is kept in
  settings (`pill.positions`) and clamped to the work area. `WM_DISPLAYCHANGE` re-clamps it.
- **Updates**: `Pill.Set(PillState)` from the app on every snapshot (idempotent), with `PillState {Visible, Mode
  (live, paused, lockPrompt, reconnecting), Watching int, VoiceAppsHeard bool, Questions int, Monitor string}`; the
  last two come from 05's `FriendsHear.alert`; actions come back on
  `Pill.Actions()` (`pause`, `resume`, `stop`, `collapse`, `expand`, `moved {monitor, x, y}`).
- Exclusive-fullscreen games draw over topmost windows; the hotkeys and the tray are the controls there. Most current
  games use borderless or flip-model windows, where the pill shows.
- Whether Desktop Duplication on Windows 10 honors `WDA_EXCLUDEFROMCAPTURE` is checked on the owner's PC (M-DESK-8);
  if it doesn't, the pill still shows (it says only "Sharing · N watching") and the result is recorded here.

### 11.5 `WDA_EXCLUDEFROMCAPTURE` on every isshoni window

- The main window gets it at creation (`OSHooks.ExcludeFromCapture(MainWindowHandle())`), so a full-screen share never
  shows friends' streams back to them (no mirror tunnel) and never shows the panel. The pill always has it.
- Trade-off: screenshots of the app (Snipping Tool) show nothing where the window is. Settings → App → "Hide isshoni
  windows from screen capture (recommended)", on by default, turns it off for the main window only (bug-report
  screenshots); the pill keeps it regardless.
- A pick of isshoni's own window is refused, not shared as black: the controller releases it and the picker reopens
  with "Pick something other than isshoni" (§10.1, §10.5).

### 11.6 Closing, quitting, and what the system does

| Event | While live (or paused, reconnecting, unreachable) | Otherwise |
|---|---|---|
| Window close (X, Alt+F4) | hide to the tray; the first time ever, a toast "isshoni is still sharing. Stop it from the tray icon (^)." and the same line as a dismissible notice at the top of the window the next time it opens (a game's Do-not-disturb may swallow the toast); `tips.trayNoticeShown` is set only once that window notice was shown | quit, or hide when Settings → App → "Keep isshoni running in the tray when I close the window" (off) |
| Tray "Stop sharing and quit" / "Quit" | stop (`tray`), then quit | quit |
| Settings → "Quit isshoni" | asks "Stop sharing and quit?" | quit |
| Screen lock (`WTS_SESSION_LOCK`) | pause (as §11.3) and `KeepAwake(false)`, so the power plan applies again: if the PC then sleeps, the suspend row stops the share. A share the user had already paused stays paused and isn't a lock pause | — |
| Unlock (`WTS_SESSION_UNLOCK`) within 5 min of the lock | resume, if the lock paused it (a pause the user chose stays), and `KeepAwake(true)` again | — |
| Unlock later than 5 min after the lock | stay paused; the panel shows "Sharing is still paused since you locked your PC. [Resume sharing] [Stop]", the pill "Paused · Resume sharing?", the tray its Paused menu with Resume first, and the §15 toast with Resume and Stop. Resume from any of them, the hotkey included, ends the lock pause and calls `KeepAwake(true)` | — |
| A lock pause reaches 30 min (counted from the lock, locked or not, until any resume) | stop with `endReason: system`; the toast "Sharing stopped while your PC was locked" (§15) | — |
| Suspend (`PBT_APMSUSPEND`) | stop (`system`); after resume a toast "Sharing stopped because your PC went to sleep" | C2 and C1 reconnect after resume (01) |
| End of session (`WM_QUERYENDSESSION`) | stop within 2 s (`share.stop`, close 1000), then allow | close sockets with 1000 |
| Keep awake | `KeepAwake(true)` from `live` to the end, except while a lock pause holds it released (above): the PC and the display don't sleep while sharing | off |

Quit always stops a share first (`core.Share.Stop`: `share.stop`, engine stop, `room.leave`; then
`audio.OnShareStopped()`), closes C2 (`core.Client.Close`, 1000) and C1 (1000), calls `audio.Service.Close()`,
flushes settings and logs, then exits; it waits at most 3 s for the server's replies. "Restart and update" quits the
same way within 5 s of starting the installer (06 §8.7).

### 11.7 The DND tip

The tip of §10.2 opens Windows' own setting: on Windows 11 (build ≥ 22000) `ms-settings:notifications` (Do not
disturb), on Windows 10 `ms-settings:quiethours` (Focus assist). isshoni doesn't read or change the setting.

---

## 12. Viewing in WebView2 and the web → desktop handoff

### 12.1 Viewing

- The viewer is M1's, unchanged: tile grid, focus, audio follows focus, keyboard, layers (05 §12). The desktop
  differs only in what the platform gives it: no "tap to unmute" (autoplay flag, §5.4); fullscreen through the
  element API plus `Window.SetFullscreen` on every `fullscreenchange` (W16); PiP where WebView2 reports it.
- WebView2 decodes H.264 High (S4: Chromium decodes all four profiles), so the room's codec set stays High when only
  desktop and Chrome viewers watch.
- Viewer audio is played by WebView2's processes, which are `isshoni.exe` descendants, so 05 keeps them out of any
  share (PLAN "Never re-capture isshoni playback").
- **Endpoint mute** (05 §11.2, §15): endpoint loopback re-captures isshoni's own playback, so while a share is live
  and `FriendsHear.engine === 'endpoint'`, `viewerStore.audioMutedForShare` forces `audio: off` on every subscription
  (`layerPolicy`). It reverts when the engine leaves `endpoint` or the share ends; 05's `viewer-muted` banner explains
  it.

### 12.2 Handoff: `playback.stop` and `playback.state`

The relay kind `playback.stop` is reserved in 01 §8.14 and defined here; `playback.state` is new and registered in
01's relay registry (`internal/protocol/relay`: `KindPlaybackState = "playback.state"`, payload `PlaybackState`).

```json
{"type": "agent.send", "id": "41", "data": {"toRole": "full", "kind": "playback.stop",
  "payload": {"v": 1, "roomId": "lounge"}}}
{"type": "agent.send", "id": "57", "data": {"to": "c_k3v9q2m7xw4pa8d1", "kind": "playback.state",
  "payload": {"playing": true, "browser": "firefox", "os": "windows"}}}
```

**`playback.stop`**:
- **Sender**: the desktop SPA (C1), once each time the user brings it into a room: the first `room.state` after a
  `join()` that the user or a deep link asked for. Not after a resync re-join (01 §10.5), so a tab whose user chose
  "Watch here instead" isn't stopped again by a network blip. `agent_target_not_found` (no tab) is normal and
  ignored; any other error is logged at debug.
- **Targets**: the user's connections with role `full` and the `agent.relay` feature, which are desktop browser tabs
  (phones are `viewer` and keep playing). The web client asks for `agent.relay` in M2 (§7.6).
- **Receiver** (`rooms/handoff.ts`, all browsers): on `agent.recv {kind: 'playback.stop'}` with `payload.v === 1`,
  `payload.roomId` equal to the session's room, and `from` listed in the user's own participant entry of
  `room.state` with `kind: 'desktop'`: the viewer enters **handed-off**:
  - every subscription goes to `{video: off, audio: off}` (`viewerStore.handedOff`, read by `layerPolicy`);
  - the stage shows "Now watching in the isshoni app" with **Watch here instead**, which clears the state and sends
    the normal subscriptions again;
  - the page's own share (if any) is untouched;
  - the state ends with a room switch or a reload.
  Any other payload (unknown `v`, another room, a sender that isn't a desktop connection) is ignored, like unknown
  kinds (01 §8.14).

**`playback.state`** (05 §5.9: a browser that plays the user's isshoni viewer is kept out of the share):
- **Payload**: `{playing: bool, browser, os: client.os}` (from the sender's own M1 `ClientInfo`; 01's
  `relay.PlaybackState`, typed by `relay.gen.ts`). `browser` is the tab's `client.browser` mapped to one of the four
  families of the generated `Browser` union: `chrome`, `edge` and `firefox` as they are, `safari` and anything else
  `other` (the core ignores a report with another value, README WA18). It has no `v`: its only receiver, the core, ignores unknown fields, so a later field is
  additive, and a change of meaning takes a new kind (README §3 item 21). `playback.stop` keeps its `v: 1`.
- **Sender**: `rooms/handoff.ts` in every browser. Addressed with `to` (never `toRole`, 01 §11.11) to each online
  connection with `kind: desktop` in the tab's `user.connections`, whenever the tab's audible playback starts or stops
  (any subscription with audio on and an unmuted element), and again to a desktop connection that newly appears in
  the list. Errors are ignored like `playback.stop`'s.
- **Receiver**: the Go App on C2, through 01's typed `EventPlaybackState{From, Playing, Browser, OS}`, which the core
  emits after decoding the payload strictly (01 §11.11 rule 1; §24). The App calls
  `audio.Service.SetViewerBrowser(From, Browser, OS, Playing)`. The desktop's C1 also receives it and ignores the kind.
- **Expiry**: the desktop SPA sends C1's online `user.connections` ids to Go (`App.OwnConnections`); when a sender
  disappears from that list, the App calls `SetViewerBrowser(fromConnID, …, false)`. 05 also drops every viewer rule
  when the share ends.

### 12.3 Browser-side entries

- **"Get the desktop app (keeps Discord out of your audio)"** stays in the browser's ShareSheet and in the whole-screen
  warning (05 §13.1, §13.3; `web/src/share/ShareSheet.tsx`, `ScreenAudioWarning.tsx`) and opens the server's
  `/download?room=<roomId>` page with the room the user is in (06 writes the page and turns `room` into its "Open
  isshoni" link, 06 §7.5; M1's plain `/download` link is replaced). The room id is added only when it passes 02's
  room-id rule (`[A-Za-z0-9_-]{1,64}`), else the link is plain `/download`. Only in the browser: the desktop's own sheet
  never shows it.
- **Only on Windows until M3/M4**: the link renders only when `platform.client.os === 'windows'`, because `/download`
  offers only Windows in M2 (macOS "Coming (M3)", Linux "Coming (M4)", 06 §7.5). On other OSes the sheet and the
  warning keep M1's explanation and add the plain text "Desktop app: Windows now, Mac soon" (a new key
  `share.sheet.desktopAppWindowsOnly` beside M1's `share.sheet.desktopApp`, used by both), with no link. Each OS gets
  its link back in the release that ships its app.
- **"Open in the isshoni app"** appears above it when the user probably has the app: `user.connections` lists a
  `desktop` connection that is online, or `GET /api/v1/me/devices` lists a device with `clientKind: desktop` and the
  same `os` as this browser's `client.os`. It opens `appLinkFor(serverOrigin, roomId)` (02's
  `web/src/lib/appLink.ts`, which builds `isshoni://<host>/r/<roomId>`) through `platform.openExternal`; 3 s later a
  quiet line says "Nothing happened? [Get the desktop app]". The app then joins the room and sends `playback.stop`
  (§12.2).

---

## 13. Deep links, command line, single instance, process contract

### 13.1 Grammar and command line (consumes 02 §12)

```
isshoni://<host>[:<port>]/                  → server https://<host>[:<port>], the SPA's own last room
isshoni://<host>[:<port>]/r/<roomId>        → that server, room <roomId>
```

- 02 owns the grammar and the parser: `internal/client/deeplink.Parse` (https only; a query and a fragment are
  dropped unread; ≤ 2048 bytes; room id `[A-Za-z0-9_-]{1,64}`; IDN hosts with `HostUnicode`; shared test vectors in
  `internal/client/deeplink/testdata/links.json`). A link never carries credentials, never starts a share and never
  changes a setting; an `http:` server is never accepted from a link.
- **`--open-url <url>`** is the only way a link arrives (06 §2.3 registers `"<install dir>\isshoni.exe" --open-url
  "%1"`). A launch whose arguments contain `--open-url` takes exactly the one following value, which must start with
  `isshoni:` (scheme compared case-insensitively), and honors no other flag in that launch: every other argument is
  ignored and logged by name, one-shot modes included (02 §12.2's injection rule; a link that smuggles a quote and
  flags can only ever be a malformed link). The one exception is `--relaunched`, which only turns the shell relaunch
  off (§13.4) and which the relaunch itself puts first.
- **`--server <origin>`**: a command-line convenience, validated with `deeplink.NormalizeOrigin(raw,
  allowInsecureLoopback = dev || e2e build)` like the manual address field (§8.1); handled like a link that carries
  only a server.
- `--autostart` (§16.1), `--log-level debug`, `--updated-from=<v>` (the installer's relaunch after an update: accepted,
  logged at info, never a parse error), `--relaunched` (§13.4).
- Bare positional arguments (a URL without `--open-url`) are ignored and logged.
- Parse errors are logged with the scheme and host only (never the path) and otherwise ignored.

### 13.2 Handling

At start the first instance reads, then deletes, `HKCU\Software\isshoni\PendingServer` and `PendingRoom` (REG_SZ;
written by 06's installer for `/SERVER=`, by this app before a start-time update (§13.4 step 5), or by a start from a
link that found an installer running (§13.4 step 2a)) **before** `HandleArgs`,
turns them into the same `deeplink.Link` value that `deeplink.Parse` returns (origin through
`deeplink.NormalizeOrigin(raw, dev || e2e)`, room by the room-id rule, an invalid value dropped with a log line) and
handles it like any deep link. A `PendingServer` value counts as the wizard's single candidate (§8.2).

| Situation | Action |
|---|---|
| server unknown | show the window; `deeplink.open {add: {origin, host, hostUnicode, roomId}}` (kept as `App.PendingLink()` until answered) → the SPA opens `/desktop/setup?add=<origin>`: the "Add server {host}?" confirmation, before any network request (§8.2) |
| server known, linked, not active, no live share | `Auth.SwitchServer`, then the room (if any) |
| server known, not linked | make it active (`SwitchServer`, which reloads the window so the link step runs with that server's `bridge.js`, §8.1) and show its link step; open the room after linking |
| server known, not active, live share | "You're sharing in {room}. Open {target}? Your share will stop." in the window (02 §12.3); a link never stops a share silently |
| server active | focus the window; `deeplink.open {roomId}` → the SPA navigates to `/r/<roomId>`, or, for `/`, to its own last room (else `welcome.defaultRoomId`), then calls `App.LinkHandled()`; the M1 switch question if a share is live in another room. A link that started the app is read at boot with `App.PendingLink()` |
| a link arrives while any confirmation or the link step is open | ignored (a debug line): a newer link must not swap the host under the friend's cursor |
| more than 20 servers | "You've added 20 servers. Remove one first." |

Links are handled at most once per second; extra ones within that second are dropped.

### 13.3 Single instance

- One instance per Windows user session, through W6's fallback in `winos` (WA41; §3.2 says why Wails' own option
  can't be used at beta.34): the first instance holds the mutex `Local\io.isshoni.desktop.v1` and serves the pipe
  `\\.\pipe\io.isshoni.desktop.v1.<user SID>`, whose DACL admits only the current user and no remote client. It
  creates the next pipe instance before it closes the one it served, so the name is never free while it serves, and
  serving starts at §13.4 step 3, right after the mutex is taken; the first pipe instance is retried for up to 5 s.
  Command lines that arrive before `Boot` has run wait in a queue (at most 8), so a second click during a cold
  WebView2 start isn't lost after the 2 s forward bound; the queue stops at Wails' shutdown, and the mutex and pipe
  are released right after Wails' `Run` returns (a forward into a dead window would otherwise block the exit). A
  second launch connects at identification level only (the server can't impersonate it) and forwards nothing to a
  server that runs as another account. The macOS dev build has no single instance.
- A second launch forwards `os.Args[1:]` unchanged to the first and exits 0 within 2 s; before forwarding it calls
  `AllowSetForegroundWindow(ASFW_ANY)`, so the first instance may come to the front (Windows' focus rules otherwise
  only flash its taskbar button). The first instance runs `HandleArgs` with them and applies the same `--open-url`
  rule (§13.1).
- Forwarded arguments are as untrusted as any command line: the same validation applies, and one-shot modes
  (§13.4) are never honored from forwarded arguments.
- The installer registers the scheme (`HKCU\Software\Classes\isshoni`, `URL Protocol`, command
  `"<install dir>\isshoni.exe" --open-url "%1"`; 06 §2.3).

### 13.4 Process contract with the installer, and the order of `main`

`main.go` handles a start in this order (06 §2.4 owns the contract's other side):

**First action, before step 0 and in every mode** (04 §13.3 layer 2): `windows.SetDefaultDllDirectories(
LOAD_LIBRARY_SEARCH_SYSTEM32)` and `windows.SetDllDirectory("")` (`SetDllDirectoryW`), so no later bare-name load, by
Go, Wails, WebView2's loader or a system DLL, searches the current directory or `PATH`. If the slice that owns this step
finds that a Wails or WebView2 bare-name load needs the app folder, the flags are `LOAD_LIBRARY_SEARCH_APPLICATION_DIR |
LOAD_LIBRARY_SEARCH_SYSTEM32`, which still exclude the current directory and `PATH`; the choice is recorded in that
slice's PR. **WA41's choice: `LOAD_LIBRARY_SEARCH_SYSTEM32` only**, without `APPLICATION_DIR`: the pinned Wails loads
WebView2 through its own Go loader by full path, and `isshoni-media.dll` is loaded by full path (04 §13.3). It is not
yet confirmed on the owner's PC; a bare-name load that fails there moves the flags to the pair above. A failure is
logged at WARN once logging exists and the start goes on (both calls exist on every supported build). A `chdir` to the
exe's folder is then optional. §22.2's marker test checks it on windows-2025.

0. **`--sound-check-helper`**: when `os.Args[1] == "--sound-check-helper"`, `os.Exit(audio.RunSoundCheckHelper())`
   (05 §11.3) before any other step but the first action: no single instance, no Wails, no WebView2 check, no
   relaunch, no env scrub.
1. **One-shot modes** (skipped entirely when `--open-url` is present, §13.1): each runs first, opens no window, skips
   single instance and does not create the mutex:
   - `--version`: prints exactly one JSON line `{"version","commit","date","protocol":{"min","max"},"engineAbi"}`
     (`internal/version`, `protocol.MinVersion`/`Version`, `media.ABIVersion`: the ABI major, 1, as 06 §2.4, not the
     DLL's packed `im_version`, 65536 = major << 16 | minor) to stdout
     and exits 0. A GUI-subsystem process writes to its inherited stdout handle; when it has none, it calls
     `AttachConsole(ATTACH_PARENT_PROCESS)` first.
   - `--self-check=<path>`: loads `isshoni-media.dll` and FFmpeg from the exe's folder (`native.Open`), reads
     `Info().ABI`, runs `Probe(ctx, false)` (20 s; no encoder required, so it passes on a GPU-less VM) and reads
     `BuildInfo()`, builds `assets.New(assets.Options{Dist: web.Dist(), …})`, and writes 06 §2.4's JSON `{"ok",
     "version", "engine": {"loaded", "abi", "ffmpegLoaded", "ffmpegLicense", "testHooks"}}` plus `"spa": {"ok":
     bool}` to `<path>`, so 06's smoke catches a broken bundle too. Exits 0 only when `ok`, which needs all of them:
     engine loaded, `abi == media.ABIVersion` (major 1), `ffmpegLoaded`, `ffmpegLicense` = `"LGPL version 2.1 or
     later"`, `testHooks` false and `spa.ok`.
   - `--forget-credentials` (run by **every** uninstall, 06 §2.4, §6.8): 02's `Servers.ForgetAll(ctx)`: revoke every
     linked server in parallel under one 5 s budget (inside the uninstaller's 30 s), then delete every credential under
     the `isshoni:v1:` prefix (02's `DeleteAll`), whatever the revokes answered; exit 0, also when offline.
   - `--purge-data` (run by a `/PURGE` uninstall instead): the same `Servers.ForgetAll(ctx)` with the same one 5 s
     budget, then delete `%LOCALAPPDATA%\isshoni`, retrying locked files for up to 10 s (WebView2 children may still
     be exiting); exit 0.
   - `--update-now` (only in `updatetest` builds, 06 §8.11): one `CheckNow`; when `ready`, `Apply(ctx, p, true)`;
     exits 0 (applied or up to date) or 1.
   - `-Embedding` (COM activation of a toast button while no instance runs, §15): registers the Wails notification
     activator and takes the activation. Every action carries a nonce of a run that is gone (§15), so all are dropped,
     except `open`, which continues as a normal start (steps 2–7); otherwise it exits 0 within 5 s.
   - As built after WA41: `--version` is real; `-Embedding` exits 0 (every action dropped) until WA68 adds `open`;
     `--self-check`, `--forget-credentials` and `--purge-data` (WA59), `--update-now` (WA84) and
     `--sound-check-helper` as step 0 (WA77) log an error and exit 2 with no window, single instance or mutex until
     their slices. The "closed unexpectedly" crash notice (§17) is only logged until its UI comes.
2. **Shell relaunch** (Windows, D16): skipped with `--relaunched` and when `Local\io.isshoni.desktop.running`
   already exists (this start will only forward). Look up the parent with a Toolhelp snapshot
   (`CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS)`, own entry's `th32ParentProcessID`); a parent that is gone, or was
   created after this process (`GetProcessTimes`: a reused PID), needs no relaunch. If the parent's exe (lower case) is
   neither `explorer.exe` nor in `audiorules.Launchers` (05 §5.4): `GetShellWindow` → `GetWindowThreadProcessId` →
   `OpenProcess(PROCESS_CREATE_PROCESS)`; `CreateProcessW` of the same exe with `--relaunched` first and then the
   same arguments (quoted with `syscall.EscapeArg`), the same environment and working directory,
   `EXTENDED_STARTUPINFO_PRESENT` with `PROC_THREAD_ATTRIBUTE_PARENT_PROCESS` = the shell process; then
   `AllowSetForegroundWindow(childPid)` and exit 0. On any failure (no shell window, access denied), log WARN and
   continue in place.
2a. **Installer running** (06 §2.4, §8.7): `OpenMutexW(SYNCHRONIZE, FALSE, "Local\\io.isshoni.desktop.installing")`.
   When it succeeds (an installer runs right now: interactive, `/S`, or a start-time update), a start from
   `--open-url` writes the link's origin to `PendingServer` and its room id to `PendingRoom` (overwriting older
   values: the newest link wins), and **every** start then exits 0 without single instance, the running mutex, the
   updater or `Apply`; the installer's own app start (`/LAUNCH`, `/RELAUNCH`, Finish) handles the link (step 6).
   `ERROR_FILE_NOT_FOUND` means no installer: the start continues; any other error is logged and the start continues.
3. **Single instance** (W6, §13.3): a second instance forwards and exits.
4. **The running mutex**: the first instance calls `CreateMutexW(nil, FALSE, "Local\\io.isshoni.desktop.running")`
   and never closes the handle before the process exits; the installer and uninstaller wait on it (06 §6.7).
5. **Start-time update** (06 §8.7(b)): the first instance builds the updater (second instances never do) and calls
   `p, _ := upd.PendingAtStartup()`. If `p != nil` and the start is not `--autostart` (a PC that just booted doesn't run
   an installer before the user is there; that update applies at the next manual start or through "Restart and update"):
   when the start came from `--open-url`, write the link's server URL to `PendingServer` and its room id to
   `PendingRoom`; call `upd.Apply(ctx, p, true)`; exit 0. `Apply` returns only once the installer it started has created
   `Local\io.isshoni.desktop.installing` (≤ 10 s, 06 §8.7; after 10 s it returns nil with a WARN), so the exit leaves no
   gap in which another start could become the first instance. No window or connection is opened first. If `Apply`
   returns an error, it is logged and the start continues with step 6 (which then handles the link from the Pending
   values just written).
6. **Pending values**: read and delete `PendingServer` and `PendingRoom` (§13.2).
7. **The app**: env scrub (§5.4), `SetCurrentProcessExplicitAppUserModelID("io.isshoni.desktop")`, the WebView2
   runtime check (§5.4), settings, logs, the services of §6.1, Wails; then `HandleArgs(os.Args[1:])` and
   `HandleLink` for the pending values. Once the engine is open, the app compares `Info().Build` with its own
   `internal/version` and logs one warning when they differ (both versions named); it never refuses, since the
   installer ships both together and dev builds mix them on purpose (04 §5.5; README §3 item 27: the binding logs only
   the engine's build, because depguard keeps `internal/version` out of `internal/media/native`).

Also part of the contract:
- **No job object**: the app never puts itself or its children in a job with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, so
  the installer started by the updater outlives it.
- **`--updated-from=<v>`** is accepted in step 7 and logged at info.
- The CDP e2e build is started with `--relaunched`, so Playwright keeps the process it launched (§22.4).

---

## 14. Settings and local storage

### 14.1 Where things live (Windows)

| What | Where | Owner |
|---|---|---|
| Settings | `%LOCALAPPDATA%\isshoni\settings.json` (+ `settings.json.bak`, the last good copy, and `settings.json.tmp` during a write) | this doc |
| Server list and active server | `%LOCALAPPDATA%\isshoni\servers.json` (03 passes this folder as `ServersOptions.Dir`, §6.1) | 02 (§11.6) |
| Tokens | Windows Credential Manager, one generic credential per server, target `isshoni:v1:<origin>` | 02 |
| WebView2 profile (Web Storage, cache) | `%LOCALAPPDATA%\isshoni\WebView2\` (also `audio.SelfInfo.WebViewDataDir`) | this doc |
| Logs, crash files | `%LOCALAPPDATA%\isshoni\logs\` | this doc (§17) |
| OpenH264 download | `%LOCALAPPDATA%\isshoni\codecs\` | 04 |
| Updates | `%LOCALAPPDATA%\isshoni\updates\` | 06 (§8.7) |
| Autostart | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `isshoni` = `"<exe>" --autostart` | this doc; the uninstaller removes it (06) |
| `HKCU\Software\isshoni\PendingServer`, `PendingRoom` (REG_SZ) | written by 06's installer (`/SERVER=`) or by 03 before a start-time update; read and deleted by the first instance at start (§13.2) | 06 / 03 |
| `HKCU\Software\isshoni\UpdateChecks` (DWORD) | written by 06's installer; read once as the initial `updates.check` (§14.2) | 06 |
| Notification registration, written by Wails' notifications service at runtime (beta.34, `pkg/services/notifications/notifications_windows.go`): `HKCU\Software\Classes\AppUserModelId\<Options.Name>` (value `CustomActivator` = a random GUID), `HKCU\Software\Classes\CLSID\{GUID}\LocalServer32` = `"<exe>" %1`, `HKCU\Software\<Options.Name>\NotificationCategories`; plus the toast icon `%TEMP%\<Options.Name>{GUID}.png` | the app writes them; 06's uninstaller removes them (§15) | this doc |
| Named mutex `Local\io.isshoni.desktop.running` | held by the first instance (§13.4) | this doc |

Every per-PC file is under `%LOCALAPPDATA%\isshoni\`, which `--purge-data` removes (§13.4); nothing is in the
roaming profile (device ids, pill positions per monitor and the Credential Manager entries are per PC). §22.4 B's
temporary profile therefore needs only another data root (and a test credential prefix); since the app reads
`FOLDERID_LocalAppData`, never the environment, the e2e build takes it from `ISSHONI_E2E_DATA_DIR` (WA72).

### 14.2 `settings.json` (schema 1)

```json
{
  "schema": 1,
  "serverPrefs": {"https://watch.example.com": {"preset": "auto"}},
  "wizardDone": true,
  "window": {"x": 120, "y": 80, "w": 1200, "h": 760, "maximized": false},
  "closeToTray": false,
  "autostart": false,
  "hideFromCapture": true,
  "notifications": true,
  "hotkeys": {"pause": "Ctrl+Alt+Shift+F9", "stop": "Ctrl+Alt+Shift+F10"},
  "pill": {"mode": "auto", "positions": {"\\\\.\\DISPLAY1": {"x": 830, "y": 8}}},
  "updates": {"check": true, "channel": "stable"},
  "tips": {"firstShareShown": false, "dndShown": false, "trayNoticeShown": false},
  "log": {"level": "info"},
  "audio": {}
}
```

- The server list, the active server, users and devices are 02's `servers.json`; settings.json keeps only per-server
  preferences under `serverPrefs`, keyed by origin. An entry whose origin is not in `servers.json` is dropped at load.
- `audio` is 05's section (05 §14.1). `settings.NewAudioStore[audio.Settings](store, audio.DefaultSettings)`
  implements `audio.SettingsStore` (a generic adapter, so `settings` doesn't import `internal/desktop/audio`, §6.1):
  `AudioSettings()` unmarshals the opaque section into 05's `audio.Settings` with 05's defaults under the saved values
  (05 owns defaults and validation), and `SaveAudioSettings` marshals it back, keeping the section's unknown keys,
  through the debounced atomic write below.
- `updates.check`: when settings.json is missing or has no `updates.check`, the load reads the installer's
  `HKCU\Software\isshoni\UpdateChecks` DWORD once (0 → `false`, anything else → `true`; absent → `true`) and saves
  the result. It is never read again.
- No token, password, window title or share label is ever written here.
- **Writes**: debounced 500 ms, then `settings.json.tmp` written and fsynced, the old file copied to
  `settings.json.bak`, and the temp file renamed over (Go's `os.Rename`, `MoveFileEx` with `REPLACE_EXISTING`).
- **Reads**: unknown fields are kept and written back (a newer app's settings survive a downgrade), also inside the
  entries of `serverPrefs` and `pill.positions`, which are merged rather than replaced; a file that doesn't parse is
  renamed `settings.json.bad-<UTC timestamp>` and `settings.json.bak` is tried; if both fail, defaults. A file that
  can't be read for another reason than not existing (a sharing violation, an ACL problem) is neither set aside nor
  overwritten at start and doesn't trigger the `UpdateChecks` read: `settings.json.bak` or the defaults stay in memory
  until a change is saved (WA41). Credentials are never touched by a settings problem (as 02 §11.6 does for
  `servers.json`).
- **Updates** check only the values they change: a loaded value this build can't parse (a hotkey of a newer build, a
  hand edit) is kept and reported (`invalid`) and never blocks saving another field (WA41).
- **Size**: no JSON object anywhere in the file has more than 1024 keys (four times the largest one, 05's `appRules`
  with at most 256; `serverPrefs` has at most 20). A file with a larger object counts as one that doesn't parse;
  `Update` and `SetAudioSection` refuse a value with one (`FieldError`). A write keeps every key the app knows and as
  many unknown keys as fit, the first ones in key order (one WARN with the count when some don't), so the app never
  writes a file it would set aside, and a file it read never makes its writes fail.
- **Migrations** are forward-only, one function per schema step, run at load; a file with a newer `schema` than the app
  knows is read best-effort and not migrated.
- `SettingsView` (to the SPA) is everything above except `audio` (05's own view through `Audio.*`), plus
  `updates.policyOff` (from `update.PolicyDisabled()`); `SettingsPatch` accepts `closeToTray`, `autostart`,
  `hideFromCapture`, `notifications`, `hotkeys.*`, `pill.mode`, `updates.check`, `updates.channel`
  (`"stable" | "beta"`), `tips.*` (to show a sheet again), `log.level` and `serverPrefs[origin].preset`.

### 14.3 The SPA's storage per server

- `DesktopPlatform.storage.local` prefixes every key with `isshoni:<serverId>:` (the derived id of §8.1), so
  `isshoni.lastRoomId`, the volume and the dismissed hints (05 §6.1) are per server. With no active server the prefix
  is `isshoni:_:`.
- At boot it removes keys whose prefix names an id that is not in `bridge.js`'s `serverIds` (a removed server).
- `storage.session` is unprefixed (one tab).

### 14.4 The Settings page (`/desktop/settings`)

| Section | Content |
|---|---|
| Servers `#servers` | the list from `Auth.Servers()` (name, host, "Signed in as {username}" or "Not linked", active mark) with Switch, Sign out, Link again, Remove; **Add server** (the wizard's server, confirm and link steps) |
| Sharing `#sharing` | Default "Optimize for" for this server; Sharing pill (Only when Windows shows no border / Always); Pause and Stop hotkeys (`HotkeyField` with status); "Show the first-share sheet again"; Video encoder: what 04 found (NVENC, QSV, AMF, MF), with §10.5's quiet reason notes (e.g. "Update your graphics driver (NVIDIA 531.61 or newer)" for an NVENC with `driver_too_old` while another encoder works) and, when 04 offers it, the Cisco OpenH264 opt-in with Cisco's notice and attribution (04 owns the download and checksum) |
| Audio `#audio` (`Audio.Watch(true)` while open) | The built-in list as locked text, "Always kept out: Discord, TeamSpeak, Mumble, …" (from `FriendsHear.voiceApps`); Keep out apps that use my microphone; the per-app decisions (`Audio.AppRules()`: Kept out, Included; built-in voice apps listed as locked) with Change (`Audio.SetAppDecision`); the ducking tip's text with [Open sound settings] (`App.OpenOSSettings('sound-communications')`); Run the sound check again. No sound-mode choice: M2 has no "All sound" mode (§6.2) |
| App `#app` | Start isshoni when I sign in to Windows (off); Keep isshoni running in the tray when I close the window (off); Hide isshoni windows from screen capture (on); Show notifications (on; the safety notices of §15 always show); Keep the tray icon visible (opens Windows settings); Quit isshoni |
| Updates `#updates` | Check for updates automatically (on; greyed out with "Turned off by your administrator" when `policyOff`); Get release candidates (beta channel) (off; `updates.channel`); Check now; the status line of §16.2 with **Restart and update** when `ready`; "Updates come only from GitHub Releases" |
| About `#about` | Version, Windows build, WebView2 version, engine summary (encoders, audio backend); Open logs folder; Copy version info (a text block with app, Windows, WebView2 and engine versions; no paths, no server); Third-party notices (`App.OpenNotices` → `<exe dir>\licenses\THIRD_PARTY_NOTICES.txt`, and the SPA's `/licenses.txt`); links to the project site's `/privacy#desktop` and `/code-signing` |

---

## 15. Notifications

Windows toasts through Wails' notifications service (W8). The app sets its AppUserModelID `io.isshoni.desktop` at
start (`SetCurrentProcessExplicitAppUserModelID`); the installer's Start-menu shortcut carries the same id (06 §2.3).
At the pinned beta.34 the service registers its toast AppID as the Wails application's `Options.Name` and writes the
HKCU keys and the temp icon listed in §14.1 at runtime; so the app sets `Options.Name = "io.isshoni.desktop"`, which
makes the toast AppID, the process AUMID and the shortcut agree. W8 asserts that, or records the actual toast AppID
for 06's uninstaller and shortcut. A toast button pressed after the app exited starts `isshoni.exe -Embedding`
(the `LocalServer32` command), handled by §13.4 step 1.

| Toast | When | Buttons | Setting |
|---|---|---|---|
| "isshoni is still sharing. Stop it from the tray icon (^)." | the first close-to-tray while live, once ever | — | always |
| `desktop.native.toast.micPrompt`: "KOOK is using your microphone. Keep it out of your stream?" | a 05 prompt of kind `mic` while the window isn't in the foreground | Keep out · Include | always |
| `desktop.native.toast.browserMicPrompt`: 05's browser text | a 05 prompt of kind `browser-mic` while the window isn't in the foreground | Keep browser out · Include | always |
| `desktop.native.toast.voiceAppsHeard`: "Voice apps can't be kept out right now, so friends hear everything, including your calls. Open isshoni to turn sound off." | 05's `audio.alert` during a live share (once per code per share, again only after 60 s; 05 §14.4), whether or not the window is in the foreground | Open | always |
| "Sharing stopped: <reason>" | a share ended without the user (`source-gone`, `elsewhere`, `server`, `server-unreachable`, `system`, `unlinked`) while the window isn't in the foreground | Open | always |
| "Sharing paused. Ctrl+Alt+Shift+F9 resumes." (the registered combination) | a pause (hotkey) with no pill shown and the window not in the foreground | — | "Show notifications" |
| "Sharing is still paused since you locked your PC. Resume sharing?" | an unlock more than 5 min after a lock paused the share (§11.6) | Resume · Stop | always |
| "Sharing stopped while your PC was locked" | a share that a lock kept paused for 30 min was stopped (§11.6) | Open | always |
| "isshoni 0.3.0 is ready. Restart to update." | `update.state` becomes `ready` (06) | Restart now (when no share is live: `Updates.Install()`) | "Show notifications" |

- A mic-prompt toast action calls `audio.Service.Decide(prompt.id, 'keep-out' | 'include')`; a dismissed toast calls
  `audio.Service.DismissPrompt(prompt.id)` (kept out, not asked again this run, nothing saved; 05 §14.6).
- **Action safety**: every toast with buttons carries a random 128-bit nonce; the app accepts an action only for a
  nonce of a toast it sent in this run and hasn't settled. Should actions arrive through protocol activation
  (`isshoni://…`) rather than COM activation, a web page can't forge them without the nonce.
- Without buttons (W8's fallback) a click opens the window on the mic prompt card or the panel.
- Toast texts come from the catalog (§18.1).
- *Later (M5)*: "Alex started sharing in Lounge" toasts in the desktop app.

---

## 16. Autostart, updates, version skew

### 16.1 Autostart (off by default)

- Settings → App toggles `OSHooks.SetAutostart(on, exe)`: the Run value of §14.1.
- Started with `--autostart`, the app creates the tray and C2's session (no room, so no presence) but **no window**
  until the user opens one; it handles deep links and update checks meanwhile, and never applies a pending update at
  that start (§13.4 step 5). No SPA runs, so the user is never shown in a room just because the PC started.

### 16.2 Updates (the UI; 06 owns the updater)

- `UpdateStatus` is the camelCase mirror of 06's `update.Status` (06 §8.9): `{state, current, available, required,
  notesUrl, progress, checkedAt, errorCode, policyOff}` with 06's eight states `disabled | off | idle | checking |
  up_to_date | downloading | ready | error`.
- The schedule is 06's (§8.6: the first check 60–300 s after start and after the wizard, then every 24 h ± 1 h, 6 h
  after an error); "Check now" (`Updates.Check()` → `CheckNow`) always works. Updates come only from GitHub Releases,
  never from the connected server (PLAN), with 06's signature, expiry and no-downgrade rules.
- **The flow**: check → background download (`downloading`, with `progress`) → `ready` (the §15 toast and the tray's
  "Restart and update") → **Restart and update** in Settings → Updates, the tray or the toast's "Restart now" runs
  `Updates.Install()`: 06's `Apply(ctx, u.Ready(), true)` (06 adds `Ready()`: the verified pending installer, nil when
  none), then Quit (§11.6) within 5 s. A pending update also applies at the next start (§13.4 step 5).
- **Never while a share is live**: `Updates.Install()` answers `busy` (03 enforces it; the updater isn't told); the
  button says "Finish sharing first".
- **UI per state**:
  - `up_to_date`: "Up to date, checked <time>" (`checkedAt`);
  - `downloading`: "Downloading {available}… {progress} %";
  - `ready`: "isshoni {available} is ready" [Restart and update]; with `required`, a banner that can't be dismissed
    at the top of every page: "An important update is ready" [Restart and update];
  - `error` with `manifest_expired`: "Update info is out of date; get the new version from your server's Download
    page" plus "Check that your PC's clock is right" (06 §8.5), with a link to the project site's
    `/download#windows-updates`;
  - `error` with `install_failed`: "The update didn't install" with a link to the project site's
    `/download#windows-updates` (06 §8.7);
  - other errors: `desktop.update.error.<code>` with [Check now];
  - `off` with `policyOff`: the setting greyed out with "Turned off by your administrator"; `off` without it: "Turned
    off";
  - `disabled` (not a release build): "Updates are off in this build".

### 16.3 Version skew between the app and a server

- The bundled SPA is the app's version, the server may be older or newer. The protocol rules of PLAN (N and N−1) and
  `minClientVersion` decide, through 02's `compat` (02 §13): `compat.FromInfo` at start and before linking,
  `compat.FromError` when C1 or C2 is stopped with `client_outdated` or `protocol_unsupported` (01 §6.1). The screens
  are 02 §13.3's:
  - **`update_app`**: "This server needs a newer isshoni app. You have {app version}; the server runs {server
    version}." **[Update now]** (`versionActions().updateApp`) and **[Open in browser]**;
  - **`update_server`**: "This server runs isshoni {server version}, which is too old for this app. Ask your admin to
    update it." **[Open in browser]**;
  - both have **[Try again]** (`Auth.Recheck()`), and 02 re-checks every 10 min.
- `versionActions().updateApp(): Promise<AppUpdateOutcome>` (WA32; it returns what happened so the version screen,
  WA82, can show the texts below): `Updates.Check()`, then wait on `update.state` for `ready`, `up_to_date` or
  `error`. `ready` → `Updates.Install()` → `{kind: 'installing'}` (the app quits within 5 s and the installer starts
  it again); `up_to_date` → `{kind: 'up-to-date'}`: "No newer version is available yet" with a link that opens
  `serverOrigin + '/download'` (the server's page, which 06 pre-fills with its URL); `error` with `not_installed` (a
  build run from a folder) → open `notesUrl`, `{kind: 'not-installed', notesUrl}`; any other error (and `off` or
  `disabled`) → `{kind: 'error', code}` with its text (`desktop.update.error.<code>`, or a bridge error's code). Nothing
  in M1 implements `updateApp`.
- `versionActions().openInBrowser`: `openExternal(serverOrigin + (currentRoomId ? '/r/' + currentRoomId : '/'))`.
- `staleBuild` is off in the desktop (§7.2).
- REST: the desktop SPA uses only the room's endpoints (`/info`, `/me`, `/rooms`, `/invites` for the invite dialog,
  `/me/devices` for "Open in the isshoni app" in the browser only) and treats a 404 on any of them as "needs a newer
  server" with Open in browser; that is the reason for D7.
- Server features come from `welcome.features` (e.g. `share.pause`); a missing feature hides its UI.

---

## 17. Logging, crashes, diagnostics

- **Go log** (`internal/desktop/applog`): `log/slog` JSON lines to `%LOCALAPPDATA%\isshoni\logs\isshoni.log`, rotated at
  5 MiB, 5 files kept (`isshoni.1.log` … `isshoni.5.log`); level `info` (Settings → About or `--log-level debug`).
  Component attribute `component` (`app`, `share`, `session`, `bridge`, `winos`, `assets`, `spa`, …).
- **Redaction**: tokens and passwords travel as `protocol.Secret` (prints `[redacted]`); the user's profile path is
  written as `%USERPROFILE%`; never logged: window titles and share labels, SDP, ICE candidates, server responses'
  bodies, a deep link's path, and **executable and app names at any level, debug included** (`discord.exe`,
  "Discord", a picked window's `Source.Exe`; README §4, 05 §16), in Go lines and in the engine log ring that the
  binding drains into this file (04 `im_read_logs`). App ids and names stay in 05's in-memory report and the UI; a log
  line that needs to tell apps apart uses a count or 05's reason code. `remote_ip` never (the app knows only the
  server). 01 §11.12's list applies to the core's lines. `log.level` is a persisted setting (§14.2), and log files are
  what users attach to public issues, so debug is held to the same rule as info.
- **Wails' own logger**: the Wails application's `Options.Logger` is never applog's debug-capable logger. It is
  applog's handler with a fixed minimum level of Info whatever `log.level` is, because the pinned Wails logs each bound
  call's raw JSON arguments and result at Debug ("Binding call complete"), before Go wraps a password in
  `protocol.Secret`: otherwise `Auth.LinkWithPassword`'s password and `Auth.AccessToken`'s token would reach the log
  at `log.level: debug`.
- **SPA log**: `lib/log.ts` (05) forwards `warn` and `error` to `App.Log` in the desktop (≤ 10 per second; above that
  one "N messages dropped" line per minute), with `component: spa` as the record's one component key and the page's
  attributes in an `attrs` group, so none can duplicate the record's own keys (WA41). The SPA's desktop code never
  passes an app name, app id, window title or share label to `log` (the same rule as above).
- **Crashes**: at start the app calls `debug.SetCrashOutput` on `logs\crash-<UTC timestamp>.txt` (Go panics and fatal
  runtime errors land there). The next start that finds a crash file newer than the last clean exit shows once:
  "isshoni closed unexpectedly last time. [Open logs folder]". A native crash in the DLL ends the process without a Go
  trace; 04 decides whether the DLL writes a minidump.
- **Clean exit marker**: `logs\last-exit.json {"clean": true, "at": …}` written by Quit.
- **Diagnostics bundle** ("Copy diagnostics": logs, probe, settings without secrets): *later (M5)*, as PLAN's M5
  lists. M2 has "Open logs folder" and "Copy version info".

---

## 18. i18n and accessibility

### 18.1 i18n

- One namespace `desktop` in `web/src/i18n/lazy/desktop.en.json` (05 §16.5's lazy namespaces), loaded by the
  `desktop/` page folder and `share/native/`. 05's proposed texts of 05 §14.8 live in it as `desktop.audio.*`
  (05's `audio.*` keys with the `desktop.` prefix, since only the desktop shows them), plus
  `desktop.audio.sheet.volumeNote` "Muting your PC doesn't mute your stream. Use Pause or turn Share sound off." and
  this doc's own `desktop.audio.soundCheck.deviceBusy` (§8.4).
- **Go reads the same file**: `web/embed.go` gains `//go:embed src/i18n/lazy/desktop.en.json` and
  `func DesktopCatalog() []byte`. `internal/desktop/i18n` looks up keys under `desktop.native.*` (tray, pill, toasts,
  native message boxes) with i18next's forms: `{{name}}` interpolation and the `_one`/`_other` plural suffixes chosen
  by a `count` variable. A missing key returns the key and logs WARN once. The toast keys include
  `desktop.native.toast.micPrompt`, `desktop.native.toast.browserMicPrompt` and the buttons
  `desktop.native.toast.keepOut`, `.keepBrowserOut`, `.include` (same texts as 05's prompt keys), and 05 §14.8's
  outside-the-window keys `desktop.native.audio.voiceAppsHeard`, `desktop.native.audio.questionsWaiting_one`/`_other`
  and `desktop.native.toast.voiceAppsHeard`; the SPA adds `desktop.share.first.hotkeys` and the DND tip's priority
  line.
- `check:i18n` gains `desktop.native` in `READ_WHOLE` (Go uses it; group 4's integration added it to
  `web/scripts/check-i18n.mjs`), and a Go test (`i18n_test.go`) scans `internal/desktop` for `T("desktop.native.…")`
  literals and fails when one is missing from the file.
- The texts of `RequireBrowser`'s "Opened in your browser" screen and the Offline screen's "Switch server" are in the
  entry chunk, where the lazy `desktop` namespace isn't loaded, so they are four `common.*` keys in `en.json`
  (`back`, `openedInBrowser.title`, `openedInBrowser.body`, `screens.offline.switchServer`; WA42).
- Error codes of §6.4 map to `desktop.errors.<code>` (the wire codes they pass through keep M1's `errors.<code>`). The
  codes are constants of a small stdlib-only package, `internal/desktop/bridge/codes`, which `tygo.yaml` exports to
  `web/src/platform/desktop/codes.gen.ts`; a test reads that file, so a new code without a text fails `test:web`
  (`engine.internal`, `engine.invalid` and `engine.busy` each have a key; the last two point to the same text). The
  reason texts of §10.5 are keys `desktop.errors.engine.reason.<reason>` (`selfWindow`, `driverTooOld`,
  `noMediaFoundation`, `blockedByPolicy`, `deviceRemoved` for §10.5's `device_removed` row (WA48), and `reinstall` for
  `missing`, `version` and `abi`; `pick_expired` has none, it reopens the picker silently). As built (WA48): the rule
  lives in `web/src/platform/desktop/codes.node.test.ts`, which `test:web` runs, not in `check-i18n.mjs`, which doesn't
  read `codes.gen.ts` (no M2 slice lists `web/scripts/`); every code but the `update.*` ones needs its
  `desktop.errors.<code>` text. The `update.*` codes' texts are `desktop.update.error.<code>` (§6.4), which WA82 writes
  with the update screens; it drops the test's `update.*` exemption, so from then on every update code needs its text
  too (README WA82's row).

### 18.2 Accessibility

- The SPA parts follow 05 §16.6 (keyboard, labels, `aria-live`, reduced motion, axe in e2e): the wizard is a stepper
  with headings and focus moved to each step's heading; the QR code has a text alternative (the short URL and the
  code); the mic prompt card is `role="alertdialog"` without focus theft (a polite announcement, then Tab reaches it);
  "Paused" and "Sharing stopped" are announced assertively.
- The tray menu is native. The pill uses standard Win32 buttons with names (§11.4). Every action has a keyboard path:
  the hotkeys, the panel, the tray menu (Win+B, then arrows).
- High contrast: the pill follows system colors; the SPA's tokens already have a `forced-colors` path (05).

---

## 19. Privacy

- **What the app sends to an isshoni server**: what the protocol needs (01: client kind, OS, version, role, caps,
  share kind, preset, flags), the device name (02's default: the computer name; renamed on the `/link` approval page or
  the Devices page, 02 §5.6), the `playback.state` relay between the user's own connections (§12.2: browser family and
  OS, never app names), and REST calls the SPA makes. Never: window titles, share labels, app lists, exclusion rules,
  "What friends hear", levels, logs.
- **What it sends elsewhere**: update checks to GitHub Releases when on (06); the system browser opens the server's
  pages and the project site on request. No telemetry.
- **What it keeps on the PC**: §14.1: `settings.json`, 02's `servers.json` (server names, origins, usernames, device
  names, last rooms), the WebView2 profile, logs, the update folder. The Credential Manager entry holds the refresh
  token; the access token lives only in memory (Go and the page).
- The login page's trust-model lines (05 §6.3) are shown in the wizard's confirm step in one line: "The server's admin
  could see streams; there's no recording and no telemetry. [More]" (`/about`).
- 06's privacy page (`/privacy#desktop`) lists the desktop app's facts from this section.

---

## 20. Limits and timeouts

| What | Value |
|---|---|
| Pick held without Start | 300 s from the pick (04's pick-token life, 04 §7.2) |
| `Share.Start` waiting for C2 | 10 s, then `publisher_offline`; then 01's `StartShare` ≤ 10 s (`Start` ≤ 20 s in all) |
| `Share.Stop` waiting for the server | ≤ 10 s (the local part always runs) |
| `reconnecting` shown | after 2 s of 01's `reconnecting` (`share.ReconnectingAfter`); `unreachable` at once |
| Hotkey or button → no media leaves the core | ≤ 100 ms (01's pump); viewers see "Paused" within 1 s |
| Lock pause: unlock that resumes on its own | within 5 min of the lock; later → "Resume sharing?" (§11.6) |
| Lock pause: stop | 30 min after the lock without a resume (`endReason: system`) |
| Tray update debounce | 250 ms |
| `share.state` events | coalesced, ≤ 10 per second |
| `audio.report` / `audio.levels` | ≤ 4 per second / 10 Hz while watched |
| `share.stats` | every 2 s while live |
| Encoder notice | "Switched to <encoder>" for 5 s |
| Access token | Go returns ≥ 120 s left (02 §11.4); `tokens.ts` re-asks at `expiresAt − 60 s`; forced refresh ≤ 1 per 10 s (02) |
| Server probe | 5 s connect, 10 s total, body ≤ 64 KiB, no redirects |
| Device code | 600 s (03), polling per server `interval` and `slow_down` (02) |
| Servers | ≤ 20 (02) |
| Deep links | ≤ 1 handled per second; ≤ 2048 bytes |
| Second instance forward | 2 s |
| `-Embedding` start without an `open` action | exits within 5 s |
| `--forget-credentials`, `--purge-data` | revokes ≤ 5 s in all (one budget); `--purge-data`'s locked files retried ≤ 10 s |
| `--self-check` probe | 20 s |
| Quit waiting for the server | 3 s; after `Updates.Install()` the app quits within 5 s |
| End of session | share stopped within 2 s |
| WebView2 watchdog (W17 fallback) | heartbeat every 5 s, reload after 15 s; at most 3 reloads per 5 min, then an error page with "Restart isshoni" |
| Settings write debounce | 500 ms |
| `settings.json` objects | ≤ 1024 keys each (§14.2) |
| Logs | 5 MiB × 5 files |
| SPA → Go log | ≤ 10 lines/s, ≤ 1 KiB each |
| Bridge calls | `Probe` 10 s, `Link` 15 s, `Start` 20 s, `AccessToken` 15 s, `Updates.Check` 45 s, other I/O 10 s |
| `/wails/runtime` request body read by the Guard | ≤ 64 KiB (413 above); chunked runtime requests refused (§5.6) |
| `App.OwnConnections` | ≤ 64 ids |
| Window | min 800 × 560 px, default 1200 × 760 px |
| Startup to window | ≤ 1.5 s warm, ≤ 3 s cold on the owner's PC, the shell relaunch included (measured in DK13) |
| Pill shown after `live` | ≤ 300 ms |
| Memory (budget, measured in DK13) | `isshoni.exe` ≤ 120 MB private working set while sharing 1080p60 (engine buffers included, GPU memory excluded); the WebView2 processes ≤ 350 MB together while watching one focused share and 8 thumbnails |

---

## 21. The room left for macOS (M3) and Linux (M4)

- **macOS (M3)**: the same `cmd/isshoni-desktop` (Wails' darwin backend, cgo; the `darwin && cgo` build constraint of
  §4.5), the same SPA and bridge. To add: `protocol.DesktopAssetOriginMacOS` in 02's `internal/protocol/desktop.go`,
  whose `DesktopOrigins()` already lists `wails://localhost` (checked like W1), `OSHooks` for darwin (Carbon hotkeys,
  a pill replacement per PLAN: SCContentFilter exclusion or auto-minimize, `SMAppService` autostart,
  `x-apple.systempreferences:` pages), the Keychain store (02), the wizard's `'audio-permission'` step before
  `'sound'`, and whether `wails://` pages are secure contexts in WKWebView (the boot already skips the check for the
  desktop). The macOS dev build of M2 already runs the app with the fake engine. The shell relaunch (§13.4 step 2) is
  Windows-only.
- **Linux agent (M4)**: no Wails. `NativeBridge` gets a `RelayBridge` over the same-user relay, so a browser page drives
  the agent with `DesktopSharing`'s code; `internal/desktop/{settings,share,applog}` and 02's
  `internal/client/deeplink` are reusable by `cmd/isshoni-agent`.
- **Mobile (pending)**: untouched; `MobilePlatform` keeps its slot.

---

## 22. Test plan

Where: **Mac** = any developer machine (`task test`, `task e2e`); **CI ubuntu/macos/windows** = GitHub-hosted runners
(`windows-2025`, 06); **owner's PC** = the Windows PC (Win11 and Win10 22H2, dual boot), manual or a self-hosted runner
there.

### 22.1 Go unit tests (Mac, CI ubuntu, macos, windows)

| Package | Asserts |
|---|---|
| `assets` | `index.html` gets the bridge tag exactly once before `boot-check.js`; `New` fails without the tag; CSP header and `<meta>` equal, with the active host in `connect-src` and `'self'` only without a server; `http:`/`ws:` only in dev/e2e for a loopback origin; release build has no dev origin and no `http:`/`ws:`; `bridge.js` JSON escapes `<`, `>`, `&`, U+2028/9 and never holds a token (fuzzed server names); `/sw.js` and `/manifest.webmanifest` 404; dotted miss 404; 405 for POST; `Guard`: foreign Origin 403, same-origin passes, no Origin plus `Sec-Fetch-Site: same-origin` passes, no Origin without it 403; runtime allowlist with requests built as the pinned runtime builds them (W21's ids), in both forms (JSON body and `?object=` query): `Call` and `CancelCall` pass with the body intact for the next handler, `Browser.OpenURL`, `Clipboard.Text`/`SetText`, `Window.Hide`, `Application.Quit`, `Dialog`, `System`, `Screens`, `ContextMenu` and `Events.Emit` get 403 with one rate-limited WARN; an unparsable object, a chunked request and a body over 64 KiB are refused |
| `servers` | `Normalize` table (hosts, IPs, ports, paths dropped, fragments never kept, invite and setup paths flagged, http refused in release, http loopback in dev/e2e through 02's `NormalizeOrigin`); `LocalID` vectors (shared with the TypeScript test); probe against `httptest` TLS servers in §8.1's order: untrusted cert → `tls_untrusted`, redirect not followed, 404 → `not_isshoni`, `setupRequired` → `server_not_set_up`, a 0.1.x `/info` without `deviceFlow` → `incompatible_server` with `reason: no_device_flow`, protocol ranges → `incompatible_server` with 02's verdicts, `publicUrl` adopted |
| `settings` | round trip; unknown fields kept; corrupt file → `.bad-*` and `.bak` used, credentials untouched (a fake `credstore` sees no call); migration from schema 0 fixture; atomic write (temp + rename) with a fault-injected writer; no secrets serialized (reflection test on the struct); no file and `UpdateChecks=0` → `updates.check=false`, read once and never again; `serverPrefs` for an unknown origin dropped; `audio.SettingsStore` round trip of 05's `Settings` |
| `share` | the state machine of §10.1 with a fake core (01's `Client`/`Share` shapes), fake engine, fake `audio.Service` and clock (`testing/synctest`): pick expiry at 300 s (a Start at 299 s works, one at 301 s returns `pick_expired`), one pick at a time, `ErrCancelled` → null, a fake `Pick` returning `invalid`/`self_window` → no pick held and `engine.invalid` with `params.reason: self_window`; Start calls in order (`OnShareStarted(pid, withAudio)`, then `StartShare` with `Label: ""`, 05's exclusions going through C2's `AudioConfig`, §10.1; `OnShareStopped` when `StartShare` fails); the controller sends no `room.join`, `room.leave` or `share.stop` of its own (the join-before-`share.start` order is 01's itests D1, D2); Stop calls `Share.Stop` then `OnShareStopped`, and a server-made end runs the same local steps once; pause calls `Share.SetPaused` then `audio.SetPaused`; `SetAudioEnabled` calls only `SetShareSound`; every row of the state and end-reason tables (`elsewhere` from `Server` ∈ {stopped, left} and `!ByThisApp`, `unlinked`, `failed` with `engine.<code>`); `liveSince` kept across `replaces`; `publisher_offline` at 10 s; lock pauses and releases keep-awake, an unlock at 4 min resumes and takes keep-awake again, an unlock at 6 min stays paused with the "Resume sharing?" state (pill, tray, toast) and resumes only on Resume, a lock pause stops at 30 min with `endReason: system` (also when the unlock came at 6 min and no Resume followed), an unlock at 31 min finds it stopped, a pause the user chose before the lock stays and never auto-resumes; suspend stops |
| `session` | `core.Auth` adapter: bound to the `Manager` of the origin its core was built for (after the active server changes, the old core's `AccessToken` never returns the new server's token); `force` is passed to `Manager.AccessToken(ctx, force)`; `ErrNotLinked` → an error with `RelinkRequired() == true`; `ErrIncompatible` closes the core; `StopUnlinked` with `session_revoked` calls `SessionEnded` and rebuilds the core when 02 stays `linked` |
| `app` | close policy table of §11.6; deep link handling table of §13.2 with fakes (unknown server → `?add=` with no network call; a known server that isn't linked → active and one `Shell.Reload()`; a link during a confirmation ignored; 21st server refused); every change of the active server (`SwitchServer`, `AddServer`) reloads the window once, `AddServer` at `/desktop/setup?link=1`; PendingServer/PendingRoom read and deleted before `HandleArgs`; `--open-url 'isshoni://watch.example.com/r/lounge' --purge-data` handles the link and never purges; a bare positional URL ignored; one-shot modes ignored in forwarded args; start-time update with a fake updater: pending at start → `Apply` and exit; a link start writes both Pending values first; `--autostart` never applies; a second instance never builds the updater; `Updates.Install` → `busy` while live; server switch refused while live; `playback.state` → `SetViewerBrowser`, and a sender missing from `OwnConnections` → `playing=false`; quit order (share stop before shell quit, 3 s bound); goleak |
| `tray` | `Render` for every mode, tooltip truncation at 127 characters with a 40-character source, menu items and hotkey labels, "Restart and update" only in Idle |
| `hotkey` | parse/format round trip, rejected combos (one modifier, Win), the defaults are `Ctrl+Alt+Shift+F9`/`F10`, fuzz |
| `i18n` | interpolation, plurals, missing key; every Go-used key exists in `desktop.en.json` |
| `applog` | rotation at 5 MiB keeping 5; `Secret` redacted; profile path replaced; crash-file detection; at `log.level: debug`, a run of the `app`, `share` and `bridge` tests over 05's fake `normal` scenario writes no exe name (`*.exe`), no app display name, window title or share label from the fixtures into the log (§17); at `log.level: debug`, the logger `main` gives Wails as `Options.Logger` has Debug disabled, and Wails' "Binding call complete" lines for `Auth.LinkWithPassword` (the password in `args`) and `Auth.AccessToken` (the token in `result`) sent through it leave neither the password nor the token in the log (§17) |
| `bridge` | each method validates arguments and maps errors to §6.4's JSON (02's `ErrOffline` → `server_unreachable`, `ErrIncompatible` → `incompatible_server`, `ErrTooManyServers` → `too_many_servers`; media codes → `engine.<code>`; `engine.cancelled` never returned); `Audio.Watch`/`WatchLevels`/`Share.WatchServerStats` reference counts; `OpenExternal` accepts only https (and dev http localhost); `Shell.OpenURL` refuses `file:`, `ms-settings:`, `search-ms:`, `javascript:` and a UNC path (`\\host\share`) with `invalid_argument` and opens nothing, even when called from Go code paths that skip `OpenExternal`; `OpenOSSettings` URI builder incl. `sound-communications`; `Log` rate limit; no method returns a refresh token (type check over every result type); `Auth.AccessToken(origin, force)` with an origin that isn't the active server's → `null` and no `Manager` call, with the active one → the token (and `force` reaches `AccessTokenWithExpiry`); a call racing an `AddServer` returns either `null` or the requested origin's own token, never the new server's; `SessionEnded`/`HelloRefused` with another origin → `not_found` and no `Manager` call; no `Audio` method sets a sound mode (the service type has no such method); `Audio.Decide` with `auto` → `invalid_argument`; `Audio.DismissPrompt` → 05's `DismissPrompt`, `ErrUnknownPrompt` → `not_found`; `TestHotkey` sets `typesChar` from a fake layout |
| `cmd/isshoni-desktop` (start order) | §13.4 with fakes for the mutex probe, the registry and the updater: installer mutex present → a start from `--open-url` writes `PendingServer`/`PendingRoom` (replacing older values) and exits 0, a plain start writes nothing and exits 0, and neither calls single instance, creates the running mutex, builds the updater or calls `Apply`; mutex absent → the normal order; `--sound-check-helper` still runs after the first action only |
| `cmd/isshoni-desktop` (build) | `GOOS=darwin CGO_ENABLED=0 go vet ./...` and `GOOS=linux go build ./...` pass (the stub prints its line and exits 1) |

### 22.2 Windows-only Go tests (CI windows, owner's PC)

- `winos`: register and unregister a hotkey; a second registration of the same combo → `ErrHotkeyTaken`; `SendInput`
  of the combo delivers `WM_HOTKEY` (interactive session; skipped with a reason when the runner has none).
- Pill: created, `GetWindowDisplayAffinity` reads `WDA_EXCLUDEFROMCAPTURE`, extended styles include `WS_EX_TOPMOST |
  WS_EX_NOACTIVATE`, positions clamp to the work area, actions on simulated button clicks.
- Autostart: set, read, unset under a test value name (`isshoni-test-<random>`), never the real one.
- DLL search order (§13.4's first action, 04 §13.3 layer 2): a child process runs `main`'s first step with
  `im-marker.dll` (a test DLL whose `DllMain` writes a file named after itself; a name in no system folder) in its
  current directory and in a `PATH` entry; afterwards `LoadLibraryExW(L"im-marker.dll", nullptr, 0)` fails and the
  marker file doesn't exist. On windows-2025.
- Single instance (W6): a second process forwards an `--open-url` link and exits 0; the first receives it.
- Running mutex: the first instance holds `Local\io.isshoni.desktop.running`; one-shot modes never create it.
- Shell relaunch: a helper (`internal/desktop/winos/internal/relaunchee`) started from the test process (not a
  launcher) calls `winos.RelaunchUnderShell` and the surviving process writes its parent's exe name to a file:
  `explorer.exe`; started with `--relaunched` it stays; skipped with a reason when `GetShellWindow` returns 0 (no
  shell on the runner), and run on the owner's PC runner.
- `--version` prints one JSON line with the fields of §13.4 when stdout is redirected; `--self-check=<path>` on a
  GPU-less runner, against `windows-native`'s `release\` DLL and FFmpeg DLLs, writes `ok: true` with `engine.loaded`,
  `ffmpegLoaded`, `testHooks: false` and `spa.ok` (06's smoke runs the same against the install).
- Notifications (W8): after one toast, the three HKCU keys of §14.1 exist under `Options.Name =
  io.isshoni.desktop`, and `CustomActivator` matches the CLSID key.
- AppUserModelID set; `OpenSettingsPage` maps pages to the right URIs by build, and `sound-communications` to
  `control.exe` with exactly `mmsys.cpl,,3` (URI builder only; nothing opened).
- `-race` where the runner has a C compiler; the shared packages already run `-race` on Linux and macOS.

### 22.3 SPA unit and component tests (Vitest; Mac, CI ubuntu)

- `DesktopPlatform` with a fake `NativeBridge`: every `accessToken`, `sessionEnded` and `helloRefused` call passes
  `bridge.js`'s `server.origin`; `apiFetch` adds the bearer, uses `credentials: 'omit'`, sends no `Authorization` when
  `accessToken(origin, false)` is `null` (the `/info` call at boot succeeds, and so does a call made after the fake
  switched its active server: no header), and on 401 `invalid_token` or `unauthenticated` with a token calls
  `accessToken(origin, true)` and retries once; a second 401 reaches the SPA; `signaling().auth` gives a fresh token,
  and with `forceRefresh` calls `accessToken(origin, true)`; `tokens.ts` re-asks 60 s before `expiresAt`, ignores an
  `auth.state` event for another origin and clears its cache when its own origin leaves `linked`; storage prefixes
  (derived id vectors) and cleanup of unknown server ids; link interception (`click`, `auxclick`, `window.open`,
  drop); no `isa_` string in storage after a session; `capabilities()` mapping; C1 `session_revoked` or a second
  `unauthenticated` → `sessionEnded(origin, code)` and a redial only when `linked`; `client_outdated` →
  `helloRefused(origin, error)` and the version screen; `not_found` from either stops C1 without a redial.
- `readDesktopConfig`: refuses a wrong `v`, missing fields, extra types; `detectPlatform` picks Desktop only with a
  valid config.
- `SignalClient` (`auth` option): awaited before every hello; forced refresh once after `unauthenticated`, then stop;
  a failed or slow forced call doesn't use up the refresh; the allowance resets only after 10 s of ready.
- `DesktopSharing`/`DesktopShare`: state mapping from `share.state` (`ShareView`), `shareId` change on `replaces`,
  recovery methods as no-ops, `stopOther(roomId, shareId)`, `localConnectionIds`; the `hint` event from
  `ShareView.hint` (no `HintDetector`); `findShareElsewhere` with a list.
- Share machine: `confirm` order (`first-share`, then `dnd`), cancel at each sheet releases the pick; the first-share
  sheet's groups and the `volumeNote` line; "everything on this PC" when `engine === 'endpoint'`.
- Native panel: rows from `FriendsHear` in three groups, Keep out/Include only for `control: 'toggle'`, the locked
  shared voice app's warning, reasons (incl. `tree` with `detail`) and warnings by key, tips with `dismissTip`;
  levels only while open (`watchLevels` ref count); banners from `FriendsHear.banners`; mic prompt card text and
  buttons by `Prompt.kind`, `decide(id, 'keep-out' | 'include')` on the buttons and `dismissPrompt(id)` on ✕ and Esc
  (never `decide(id, 'auto')`); Share sound shown only for shares started with sound; no "No sound captured" line for
  a native share however long the mix is silent, "Nothing is playing right now" while sound is on and no `heard` row
  is `playing`, gone when one plays; the minimized line while `ShareView.minimized`; paused bar; the "Resume
  sharing?" bar after a long lock (Resume calls `setPaused(false)`); the unreachable bar's Test connection URL; the
  Details disclosure (Ctrl+Shift+D, `watchServerStats` on open and off on close, no names, the `driver_too_old`
  note); "Switched to <encoder>" for 5 s.
- Engine errors (§10.5), one case per row: `pick_expired` (from `start` and the controller's own code) reopens the
  picker with no text; `self_window` reopens it with "Pick something other than isshoni"; `driver_too_old`,
  `no_media_foundation`, `blocked_by_policy` and `missing`/`version`/`abi` show their texts and links (each arriving as
  `dependency`, README §3 item 17); `device_removed` shows its stop notice and [Share again] starts a new pick; a probe
  with NVENC `driver_too_old` and a working fallback shows only the quiet note; an unknown reason falls back to its
  code's text; `permission_denied`, `source_gone`, `unsupported_os`, `no_encoder` and `internal` as their rows.
- Browser entries (§12.3): with `client.os` `windows` the ShareSheet and the whole-screen warning link to
  `/download?room=<roomId>` (plain `/download` for an id that fails the room-id rule); with `macos`, `linux`,
  `android` and `ios` they show "Desktop app: Windows now, Mac soon" and no link.
- Viewer: local share without preview subscribed `low`/`off`; `handedOff` turns everything off and back; `paused`
  tile with the last frame or a placeholder, no "connection unstable" while paused; endpoint mute: `audio: off` for
  every subscription while live with `engine === 'endpoint'`, reverting after.
- Handoff: the `playback.stop` receiver ignores another room, another `v`, a non-desktop sender, unknown kinds; the
  `playback.state` sender addresses each online `kind: desktop` connection with `to`, on start and stop of audible
  playback and when a new desktop connection appears.
- Router: `RequireDesktop` NotFound in the browser; `RequireBrowser` opens the browser once in the desktop (incl.
  `/link?code=…` with its query); desktop `RequireAuth` by `AuthState` (`/desktop/relink` only with a notice).
- Wizard pages with MSW-free fakes (the bridge does the network): every step; the empty server step leads with
  "Installed from your isshoni page? Go back to it and click Open isshoni" above the address field, which accepts a
  pasted room or invite URL; the add step makes no bridge call before Add except the local `App.PendingLink()`, and
  shows `hostUnicode (host)`; after Add, a probe whose origin has the add step's host goes straight to `addServer`
  (no confirm step), and one whose host differs shows the confirm step with "This server calls itself …" before any
  `addServer`; a typed address always gets the confirm step; every probe error and `incompatible_server` verdict;
  `addServer` is followed by no other bridge call from that page (the reload follows), and the page loaded at
  `/desktop/setup?link=1` drops the parameter and calls `link()` exactly once, only when the state is `unlinked`
  without a notice; link states from `AuthState` (`linking` with code, QR, short URL, the server name and the trust
  line; `linked`; `denied`; `code_expired`; `incompatible` with `recheck`); an `auth.state` event for another origin
  changes nothing on the page; the QR code encodes `verificationUriComplete`; the password form: each error text of
  §8.2 (`validation_failed` per field), the password field cleared after an error, `autocomplete="off"` on both
  fields, exactly one `linkWithPassword` call per [Sign in], and [Back] calling `link()`; the voice step calls
  `audio.watch(true)` on mount and shows `voiceApps[].running`; sound check results by `backend` and `reason`, incl.
  `device_busy` with its text, [Open sound settings], [Try again] and [Continue], and an unknown reason with the
  generic text; relink texts per notice and the `save_failed` banner; Unlink with `confirmed: false`.
- Settings: Updates section per `UpdateStatus` state (required banner, `manifest_expired`, `install_failed`,
  `policyOff`, beta channel switch); Audio section (`appRules`, the ducking tip's button; no sound-mode control);
  Sharing section's hotkey field warns when `testHotkey` reports `typesChar`.
- `app/chunks.node.test.ts`: the new initial-chunk rules of §7.2; `check:size` still passes.

### 22.4 End-to-end

**A. Desktop in Chrome** (Playwright on Google Chrome; Mac and CI ubuntu, in the existing `e2e` job). The harness
`internal/desktop/e2ehost` (package main, built in CI like `LOADTEST_BIN` and passed as `DESKTOP_E2E_BIN`) runs the
real `app`, `bridge`, `session` and 05's real `audio.Service` with:
- 01's fake engine `fake.NewEngine(fake.EngineConfig{Mode: fake.Decodable, SourceSize: [2]int{640, 360},
  AudioScenario: os.Getenv("ISSHONI_FAKE_AUDIO")})` (05 §13.3's scenarios; default `normal`). Decodable mode encodes
  Constrained Baseline only, so the core offers `42e0` (01 §11.4 step 3 falls back to the encoder's only profile);
  640×360 keeps its I_PCM IDRs small enough for the pacer (01 §6.2). `-tags e2e` builds and `task desktop:dev` use the
  same configuration;
- `audio.New(fakeEngine, store, audio.SelfInfo{PID: os.Getpid(), WebViewDataDir: <temp>, Exe: ""}, log)`;
- `deviceauth.OpenServers(deviceauth.ServersOptions{Dir: <temp>, Store: credstore.NewMemory(),
  AllowInsecureLoopback: true, …})` against the unmodified e2e server of `fixtures.ts` (the real `isshoni serve`);
- a recording `Shell` and `OSHooks` whose state is at `GET /__e2e/state` (tray view, pill state, keep-awake, toasts,
  opened URLs, registered hotkeys, reloads, and for each access token handed to the page the origin it was asked for,
  the active origin and the first 8 bytes of the token's SHA-256, never the token);
- e2e-only flags, absent from every other build: `-reload-delay=<d>` (the recording `Shell.Reload()` waits that long,
  so a spec can act in the moment before a reload), and `-lock-resume=<d>`, `-lock-stop=<d>` (the 5 min and 30 min of
  §11.6, shortened for `desktop-system.spec`);
- a log at `log.level: debug` in the harness's temp folder.
It serves the SPA with `bridge.js` `transport: "http"`, `/__bridge/call` and `/__bridge/events`, and
`POST /__e2e/hotkey?id=…`, `/__e2e/system?event=lock|unlock|suspend` to inject OS events and `POST /__e2e/args` to
deliver arguments as a second instance would (e.g. `--server http://127.0.0.1:<port>`: links are https-only, so the
add step is reached through `--server` here). A stub `/wails/runtime` handler sits behind the real `assets.Guard`, so
the allowlist is exercised in Chrome too. The page is served at the
origin **`http://wails.localhost`**: the spec launches its Chrome context with `--host-resolver-rules="MAP
wails.localhost 127.0.0.1:<harness port>"`, so the unmodified server accepts the page as a desktop origin (bearer-only
`/ws` and 02's CORS) and no server knob is added. If that proves impossible, 02 is asked for a `servertest`-only hook,
never a config key. The CSP of a dev/e2e build allows the http loopback server (§5.3).

As built (WA48, `internal/desktop/e2ehost`): the flags are `-listen` (default `127.0.0.1:0`), `-data` (default a
new temporary folder, removed at exit), `-server <origin>` (added and made active at start, standing in for the add
step until WA71), `-origin` (default `DesktopAssetOriginWindows`), `-reload-delay`, and `-lock-resume`/`-lock-stop`
(accepted, used once the share controller takes them, WA68); arguments after the flags are the app's command line.
Once it listens it prints one JSON line `{addr, origin, log}`. `/__e2e/system` also takes `resume`, `end-session` and
`display-change`. Each token record in `/__e2e/state` has `origin` (asked for), `activeOrigin` (the active server
when `Auth.AccessToken` was called, the moment that decides whose token it is, §5.8, which `desktop-link.spec`'s
"no token for A while B was active" asserts on), `activeAtReturn` (for the failure message) and the hash prefix; the
recording `Shell.Reload` only counts reloads (a spec reloads the page itself). Until the e2e server answers CORS for
the desktop origins (WA56 wired `httpapi.Deps.DesktopOrigins` in group 6; WA47's default is none),
`desktop-security.spec`'s `/link` and storage case uses a small loopback stand-in server with the e2e server's own
`/info` plus `deviceFlow`, `/device/password`, `/device/token` and `/device/revoke`; the spec switches to the worker's
e2e server by itself once that one answers desktop CORS and lists `deviceFlow`, and the stand-in branch goes with WA65
(README WA65's row). In CI (group 5's integration) the `build` job, which has `web/dist`, builds the harness
(`CGO_ENABLED=0 go build -trimpath -o bin/e2ehost ./internal/desktop/e2ehost`) and uploads it as `e2ehost-linux-amd64`;
the `e2e` job downloads it and sets `DESKTOP_E2E_BIN` (06 §10.1).

| Spec | Asserts |
|---|---|
| `desktop-link.spec` | wizard: server address → confirm → device code, QR code and short URL shown → a second browser context signed in on the server approves at `/link` → sound step (05's fake: `captured`) → voice step (`voiceApps` from the scenario) → the room; a denied code shows "declined"; the password path: "Sign in with username and password instead", a wrong password shows the error and clears the field, the right one links; an unknown server delivered as `--server` shows "Add server {host}?" and the harness records no request to it before Add, and after Add (same host) the link step follows with no confirm step. **Second server, no token crosses**: with the page in a room on server A (C1 open) and a second e2e server B from `fixtures.ts` (its own port and data dir), the harness started with `-reload-delay=2s`: add B through `--server`, Add; during the delay the spec creates a room on A (A's `invalidate` makes the page refetch `['rooms']` from A) and closes C1 through Playwright's `page.routeWebSocket` (C1 reconnects and resumes), so the page's refetch and C1's resume hello reach A while B is active; then B's link step renders on B's `bridge.js` and is approved. Asserted: no request to A (`page.on('request')`, the `Authorization` header) and no `hello` to A (`page.on('websocket')`, `framesent`) carries a token whose hash `/__e2e/state` lists for B; the harness lists no token handed out for origin A while B was active; the window reloaded exactly once, at `/desktop/setup?link=1` |
| `desktop-share.spec` | Share → sheet → fake pick → first-share sheet → live; afterwards the harness's debug-level log contains no exe name (`.exe`), no app display name of the scenario, no window title and no share label (§17); a web viewer tab decodes frames and audio energy (M1's `stats.ts`); the desktop's own tile shows the preview layer only (`low`, no audio subscription); tray state `Live`, "1 watching"; Pause (button, then `/__e2e/hotkey?id=pause`) → viewer tile "Paused", no new frames, no audio energy; a viewer that joins while paused gets no frame; Resume; the `normal` scenario's mic prompt at t = 5 s shows the card and a recorded toast while hidden; Stop by hotkey → share gone, hotkeys unregistered, keep-awake off |
| `desktop-share.spec` (audio alert case) | with `ISSHONI_FAKE_AUDIO=degrade-live`: Share → live; 5 s later the fake degrades to `endpoint`; the harness's debug endpoint shows `tray.Render` with `tray-warn`, the first menu line "Friends hear voice apps" and, with a scripted prompt, "1 question waiting"; `PillState` with `VoiceAppsHeard` and `Questions` 1; exactly one recorded `desktop.native.toast.voiceAppsHeard` toast; a second degrade within 60 s records none |
| `desktop-handoff.spec` | a web tab of the same user watches Lounge; the desktop page joins Lounge → the tab shows "Now watching in the isshoni app" and its subscriptions are all off; "Watch here instead" resumes and the tab's `playback.state {playing: true}` reaches the app (`/__e2e/state` shows 05's viewer rule input); closing the tab sets it to `playing: false`; a phone-emulated tab (role `viewer`) keeps playing |
| `desktop-reload.spec` | reload the desktop page while sharing → the share continues for viewers and the page adopts it (panel back, live) |
| `desktop-security.spec` | the HTML response carries the CSP; `/sw.js` 404; an external link click records an `OpenURL` and the page stays; no `isa_` in `localStorage`; `/wails/runtime` with a foreign Origin 403 (assets handler mounted the same way); same-origin runtime requests from the page for `Browser.OpenURL('ms-settings:display')`, `Browser.OpenURL('search-ms:query=x')`, `Clipboard.Text()` and `Window.Hide()` get 403 and reach no handler, while a bound-method `Call` reaches the stub; `/link` opens the browser |
| `desktop-relink.spec` | revoke the device from the web Devices page → C1 and C2 closed → `Auth.SessionEnded` → the desktop shows `/desktop/relink` with the `signed_out` text; Link again works |
| `desktop-system.spec` | with `-lock-resume=2s -lock-stop=6s`: lock pauses and keep-awake goes off; unlock within 2 s resumes and keep-awake is on again; unlock after 3 s stays paused with "Resume sharing?" in the panel, the pill state, the tray menu and a recorded toast, and Resume makes it live; left locked for 6 s it stops with `endReason: system` and the toast "Sharing stopped while your PC was locked"; suspend stops with `endReason: system` and a recorded toast |

Every spec also runs `axe` on its desktop pages (05's `a11y` helper) in both color schemes.

**B. The real app over CDP** (CI windows, owner's PC). 06's `task desktop:exe:windows TAGS=e2e` builds `isshoni.exe`
with `-tags e2e`: the fake engine with 05's audio scenarios (as in A), http servers on loopback allowed, the
`WEBVIEW2_*` scrub off, DevTools on, and `credstore.Open(credstore.Options{Prefix: "isshoni-test:<random>:"})` whose
entries are deleted at exit (never the user's `isshoni:v1:` entries). The spec starts an `isshoni` server
(`tls.mode=off` on `127.0.0.1`), launches the app with `--relaunched` (so Playwright keeps its process), a remote
debugging port and a temporary data root, and attaches with `chromium.connectOverCDP`. **The data root can't come
through `LOCALAPPDATA`** either (WA48's review): the app reads `FOLDERID_LocalAppData` through the shell's known
folder, never from the environment, so `flavor_e2e.go` (WA72) also takes the data root from the e2e-only
`ISSHONI_E2E_DATA_DIR`, and the port from `ISSHONI_E2E_CDP_PORT`. WA48's skeleton (`web/e2e/cdp/`) sets both, seeds
`isshoni\settings.json` there with `closeToTray: true` (W7 then needs no live share) and fails before any test unless
the app wrote into that folder (its log file or WebView2 profile): an e2e build that ignored it would use the owner's
real servers, settings and WebView2 profile. **The port can't come
through `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS`**: at beta.34 Wails overwrites that variable (and
`WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER`) in every build (§5.4), e2e builds included. WA72 adds an e2e-only path instead:
`flavor_e2e.go` reads the port from an e2e-only variable or flag and appends `--remote-debugging-port=<port>` to
Wails' `WindowsOptions.AdditionalBrowserArgs`, next to the autoplay argument; no other build compiles that file
(README WA72). The spec checks W1–W3, W5, W7, W9 (observable rows), W13, W16; then a device-flow link approved by a
second browser context, a fake share seen by a Chrome viewer, close-to-tray while live (the window hides, the share
continues), and Quit (share ended, process exited 0 within 3 s). W21: from the page, raw runtime
requests built as the pinned runtime builds them for `Browser.OpenURL('ms-settings:display')`,
`Browser.OpenURL('search-ms:query=x')`, `Clipboard.Text()` and `Window.Hide()` get 403, no Settings or search window
opens (no new top-level window of `SystemSettings.exe` or `SearchHost.exe` appears) and the main window stays
visible, while a bound method answers. It never runs against a production build: a production build's test (W19)
starts it with `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=<port>`,
`WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER=isshoni-test-<random>` and an arbitrary `WEBVIEW2_X=1` and asserts that no
debugging port is open, no pipe `\\.\pipe\isshoni-test-<random>` exists and the webview's process command lines carry
none of them. Because Wails overwrites the first two in every build, "no port, no pipe" would pass even without the
scrub; what proves the scrub is `WEBVIEW2_X` (and any other `WEBVIEW2_*` Wails doesn't set) missing from the
webview processes' environment and command lines, and the unit test of `scrubEnv`. e2e builds keep the variables.

### 22.5 Manual (owner's PC; recorded in the PR and in the release checklist)

| Id | Steps | Pass |
|---|---|---|
| M-DESK-1 | Win11, from a room's "Get the desktop app" (so the page is `/download?room=<id>`): (a) install with `install-desktop.ps1` (06: `/SERVER=` → `PendingServer`, `/LAUNCH`), confirm "Add server", approve in the browser, finish the wizard; (b) uninstall, then run the downloaded installer and click the page's **Open isshoni** after it; (c) uninstall, run the installer, leave the Finish page's "Start isshoni" checked, then follow the server step's "Go back to it and click Open isshoni" | Each: room shown within 2 minutes, no server URL or password typed; in (b) and (c) the add step shows the page's server and the app opens the room the page came from; in (c) the server step leads with the "Open isshoni" hint |
| M-DESK-2 | Win11 24H2+: share a monitor (borderless); watch from a phone | The pill is visible locally and absent from the stream; the isshoni window is absent from the stream; the pill drags and collapses; Narrator reads its buttons |
| M-DESK-3 | In a borderless full-screen game: press Pause, then Resume, then Stop hotkeys | Viewers see "Paused" with the last frame and silence within 1 s; a phone that joins while paused sees the Paused tile; resume within 1 s; stop ends the share |
| M-DESK-4 | Close the window while live; use the tray menu; Quit while live | Hidden to tray with the one-time toast; red icon and "Sharing … · N watching"; Quit stops the share and exits |
| M-DESK-5 | While live with the window minimized, start a voice app not on the list (or `fakevoice.exe` of 05 holding the mic) | Toast with Keep out/Include; the choice applies and is remembered (W8); with the app closed, a toast button starts `-Embedding` and opens nothing unexpected |
| M-DESK-6 | Lock the PC while live and unlock after 1 min; lock again and unlock after 6 min; lock again for 31 min (the display may turn off); sleep and wake | 1 min: paused while locked, live after unlock. 6 min: still paused, "Resume sharing?" in the window, pill, tray and a toast; Resume makes it live. 31 min: the PC followed its power plan while locked (display off, sleep if the plan says so: then the suspend row applies), and on unlock the share has stopped with "Sharing stopped while your PC was locked". Sleep stops the share with the toast after wake |
| M-DESK-7 | Chrome and Firefox tabs watching Lounge on the same PC; click "Open in the isshoni app" with the app closed | The app opens Lounge; the tab shows "Now watching in the isshoni app"; sound only from the app; Process Explorer shows `isshoni.exe` under `explorer.exe`, not under the browser (§13.4) |
| M-DESK-8 | Win10 22H2: share a monitor (Desktop Duplication) and a window (WGC) | Monitor: no border, pill shown and checked against the stream (records whether DD honors the affinity); window: yellow border, no pill by default |
| M-DESK-9 | Open `isshoni://<another host>/` from a web page | "Add server" confirmation with the host, before any request (no hit in that server's log); nothing starts; Cancel leaves no trace |
| M-DESK-10 | High contrast on; text size 150 %; 200 % display scaling | Pill, tray menu and SPA readable and usable |
| M-DESK-11 | Kill a `msedgewebview2.exe` renderer while live (W17) | The page comes back; the share never stopped |
| M-DESK-12 | Measure startup and memory (§20) with Task Manager after 30 minutes of sharing 1080p60 while watching | Within §20's budgets; numbers recorded |

Discord exclusion is 05's audiotest. Glass-to-glass is 01 §12.2's run: a Chrome viewer on the same PC and monitor,
measured with `tools/g2g`. This doc's shell takes part in both: the share starts from the app, the Details disclosure
shows the encoder and the full layer's bitrate, and its windows stay out of the capture (WDA, §11.5).

---

## 23. Interfaces other docs rely on

| Export | Where | Consumers |
|---|---|---|
| Consumes 02's `protocol.DesktopAssetOriginWindows` (asset origin, W1) and `protocol.DesktopOrigins()`; adds nothing on the server | §5.7 | 02 (its wiring and W1 result for DA6) |
| Relay kinds `playback.stop` (payload `{v: 1, roomId}`) and `playback.state` (payload `{playing, browser, os}`), sender and receiver rules | §12.2 | 01 (relay registry: `KindPlaybackState`, `PlaybackState`), 05 (`SetViewerBrowser`), web client |
| Web client hello features `agent.relay`, `user.connections`; C2's features are exactly 01 §11.1's | §7.6, §9 | 01-client-core (server side of `user.connections`) |
| Consumes 02 §12 (`deeplink.Parse/Build/NormalizeOrigin`, `appLinkFor`); command line `--open-url <url>` (the only link entry, with the injection rule), `--server <origin>`, `--autostart`, `--updated-from=<v>`, `--relaunched` | §13.1 | 02, 06 (scheme registration `"…\isshoni.exe" --open-url "%1"`, 06 §2.3) |
| Process contract (§13.4): mutex `Local\io.isshoni.desktop.running`; `--version` JSON; `--self-check=<path>` JSON (06 §2.4's engine fields plus `spa.ok`; `ok` needs all); `--forget-credentials` (every uninstall: 02's `ForgetAll`, one 5 s budget); `--purge-data` (`/PURGE`: the same `ForgetAll`, then the data dir); `--update-now` (updatetest); `-Embedding`; no job object; PendingServer/PendingRoom read and delete; PendingRoom written before a start-time update; the initial `updates.check` from `UpdateChecks` | §13.4, §14.2 | 06 (installer, uninstaller, smoke, R4, R9) |
| `--sound-check-helper` mode: 03 dispatches it first in `main`, 05 implements it (05 §11.3) | §4.2, §13.4 | 05 |
| Shell relaunch under `explorer.exe` (§13.4 step 2) | §13.4 | 05 (§6.2's include rules assume it) |
| AppUserModelID, Wails `Options.Name` and single-instance id `io.isshoni.desktop`; exe name `isshoni.exe`; Run value name `isshoni` | §4.2, §13.3, §14.1, §15 | 06 (shortcut, uninstall), 05 (own tree by PID, plus the exe name `isshoni.exe` and the WebView2 user data folder; changing either needs 05's rule change) |
| Data locations (§14.1): settings, `servers.json` folder, WebView2 profile, logs, the Pending values, the notification registry keys and temp icon written at runtime | §14.1 | 06 (uninstall and purge: what to keep or remove), 02 (credential naming is 02's) |
| `settings.json` `audio` section through `settings.NewAudioStore` as `audio.SettingsStore`; `audio.SelfInfo{PID, WebViewDataDir, Exe}`; window visibility to `SetVisible` | §6.1, §14.2 | 05 |
| The endpoint viewer mute (`viewerStore.audioMutedForShare`) | §12.1 | 05 (`viewer-muted` banner) |
| `Engine.Pick(ctx, media.PickRequest{OwnerWindow: <main HWND>})`; `ErrCancelled` → null; the app applies `WDA_EXCLUDEFROMCAPTURE` to its own windows (04 doesn't) | §10.1, §11.5 | 04, 01 |
| `ShareView` fields used by the shell: `border` (from `Source.OSBorder`, then `ShareSnapshot.NoOSIndicator`), `label` (the controller's, from `Source.Name`; `ShareRequest.Label` always `""`), `kind`, `watching` (`len(Watchers)`) | §6.3, §10.1 | 01 and 04 add `NoOSIndicator` and `OSBorder` |
| Bound services and events (§6.2, §6.3), bridge error codes (§6.4) in `internal/desktop/bridge/codes` → `web/src/platform/desktop/codes.gen.ts` (a new `tygo.yaml` entry) | §6, §18.1 | SPA; M3; M4's `RelayBridge` mirrors `NativeBridge` |
| TS: `Platform.desktop` (optional, §7.1), `DesktopFeatures` (with `sessionEnded`, `helloRefused`, `signalStopped`), `HelloAuthProvider`, `PickedSource.preview \| null` (with WA65), `label`, `confirm`, `SharingProvider.current/localConnectionIds/stopOther(roomId, shareId)`, `LocalEndReason` additions, `NativeBridge`, `ShareView`, `RoomSession.adoptShare`, `SignalClientOptions.auth`; `RoomPage`'s `?conntest=1` | §7 | 05-web (browser parts unchanged), M3, M4 |
| `web.DesktopCatalog()`, namespace `desktop`, Go keys `desktop.native.*` (incl. the mic-prompt toasts); 05's texts as `desktop.audio.*` | §18.1 | 05 (texts) |
| Viewer tile state `paused` for `ShareStatus paused`; no freeze detection while paused | §11.3 | 01-client-core (server status) |
| Project-site anchors the app links: `/download#windows-updates` (install_failed, manifest_expired), `/troubleshooting#desktop-webview2` (WebView2 message box), `#desktop-gpu-driver`, `#desktop-media-feature-pack` and `#desktop-smart-app-control` (§10.5's reason rows), `#desktop-linking` (§8.1's and §9's "See why", 02 DA12's section), `/privacy#desktop` and `/code-signing` (About) | §5.4, §8.1, §9, §10.5, §14.4, §16.2 | 06 (`TestDocsAnchors`) |
| The `Auth` binding's token contract: `AccessToken(origin, force)`, `SessionEnded(origin, code)` (`unauthenticated`, `session_revoked`, `account_disabled`), `HelloRefused(origin, error)`, answered only for the active server's origin; no `TokenRejected`; the window reloads on every change of the active server (`AddServer` at `/desktop/setup?link=1`); C2's `core.Auth` adapter is bound to its core's origin | §5.8, §6.2, §7.3, §8.2, §9 | 02 (§11.7, §11.8 write the same signatures) |
| The browser's "Get the desktop app" links to `/download?room=<roomId>` (room id by 02's rule, else plain `/download`), shown only when `client.os` is `windows` | §12.3 | 06 (`/download` turns `room` into "Open isshoni"'s `appLinkFor(publicUrl, roomId)`) |
| `bridge.Audio` wraps 05's `DismissPrompt(promptID)`; `Decide` takes only `keep-out` or `include`; no sound-mode binding | §6.2, §10.3, §15 | 05 (§14.2, §14.6) |
| `internal/desktop/e2ehost` harness and its `/__e2e/*` hooks; `DESKTOP_E2E_BIN`; the `http://wails.localhost` host-resolver mapping | §22.4 | 05 and 01 e2e reuse it; 06 CI |
| CI needs: `GOOS=windows` cross build and lint of `./cmd/isshoni-desktop/... ./internal/desktop/...` on ubuntu; `GOOS=darwin CGO_ENABLED=0 go vet ./...`; `go test` of the same on windows; `cmd/isshoni-desktop` compile on macos-15; bindings drift check beside tygo's; desktop e2e specs in `e2e`; the CDP spec on windows; `task desktop:exe:windows` with `TAGS` | §22 | 06 (owns `.github/workflows/*` and the task) |

---

## 24. Depends on

The shapes below are what this doc assumes; each sibling's own names win, and the adapters in
`internal/desktop/{session,share,app}` map them.

**01-client-core.md**
- `internal/client/core` as 01 §11.2 defines it, plus the additions named here:

  ```go
  c, _ := core.New(core.Options{ServerURL, Auth, Client, Engine, Logger})
  c.Run(ctx)                                    // until ctx ends or a terminal *StopError
  c.Status()                                    // Conn, Stop, ConnectionID (01 adds), Features, Share
  c.Subscribe()                                 // EventStatus, EventShare, EventNotice, EventPlaybackState (01 adds), EventResync, …
  c.StartShare(ctx, core.ShareRequest{RoomID, Source, Preset, Audio, Label: ""}) // exclusions: Options.AudioConfig
  c.ActiveShare()
  c.StopOther(ctx, roomID, shareID)             // 01 adds: share.stop for another connection's share of this user
  c.WatchServerStats(on)
  c.Close()
  s.Snapshot(); s.SetPreset(ctx, p); s.SetPaused(ctx, p); s.Stop(ctx); s.Done()
  ```
  `core.Auth` is implemented by `internal/desktop/session` over 02's `Manager` (§9). `ShareSnapshot` as 01 §11.9 plus
  `AudioSentBps int` (§11.9 adds it), `NoOSIndicator bool`, and `EndInfo.Server`, `EndInfo.ByThisApp` (for `elsewhere`)
  and `EndInfo.MediaReason` (§10.5's reason rows for a share's end, README §3 item 43). As built (WA58): `ShareRequest`
  has no `Exclusions` or `Borderless`; 05's exclusions come through `Options.AudioConfig` and the core takes
  `Borderless` from the probe (§10.1). `EventNotice` codes `encoder_fallback {encoder}` and `audio_rebuilt`.
  `EventPlaybackState{From, Playing, Browser, OS}` for a decoded `playback.state` (01 §11.11); the core serves no relay
  request kind (01 D16).
- `Share.SetPaused` per 01 §11.8 / D14 (pump drops at once, keyframe requests ignored while paused, `share.update
  {paused}`); `share.pause` on the server (`ShareUpdate.Paused`, `ShareStatus paused`, `sharerPaused` in the SFU).
- `StartShare` joins the room and `Share.Stop` leaves it (01 §11.4, D12); the app sends no room or share messages of
  its own on C2.
- No encoder fallback in the core: D15's fallback is the engine's (04 §9.8, README §3 item 16). The core raises the
  `encoder_fallback` notice from `EventEncoderChanged`, never restarts a session for it, and ends the share on
  `encoder_lost` from `Next` (§10.5).
- The `media` contract of 01 §5 (`Pick(ctx, PickRequest)`, `ErrCancelled`, `Source{Kind, Name, PID, …}` with no self
  flag (a self pick is `invalid`/`self_window`), `Probe.Audio.Backend`, `ErrorCode` incl. `dependency`, `Error.Reason`
  and `EncoderInfo.Status`/`Reason` with 04's tokens, `ShareSnapshot.Minimized`), `native.Open` (01 §7.1), and
  `fake.NewEngine` with `Mode: fake.Decodable, SourceSize: [2]int{640, 360}` as the app's dev/e2e configuration (01
  §6.3 should name it).
- `internal/protocol/relay` registers `KindPlaybackState` and `PlaybackState {Playing, Browser, OS}` (§12.2; no `V`,
  README §3 item 21).

**02-device-auth.md**
- Server: device flow endpoints (03 §12.4.7), bearer on REST and `/ws`, `protocol.DesktopOrigins()` and its wiring
  (CORS, bearer-only allowed origins), revocation closing C1 and C2, the `/link` approval page, linked devices on the
  Devices page, `deviceFlow` in `/info.features`.
- Client (Go): `deviceauth.OpenServers(ServersOptions{Dir, Store, App, AllowInsecureLoopback, OnState})` with `Add`,
  `SetActive`, `Remove`, `Active`, `List`, `ForgetAll` (§11.6; this doc passes `Dir = %LOCALAPPDATA%\isshoni`, the data
  root, so the purge removes it); `Manager` with `Link`, `CancelLink`, `LinkWithPassword`,
  `AccessToken(ctx, force)`, `AccessTokenWithExpiry(ctx, force)`, `SessionEnded`, `HelloRefused`, `Unlink`
  (`ErrRevokeNotConfirmed` → `confirmed: false`), `State`, and a `Recheck` (no `Rejected`: a forced refresh is
  `force`); errors `ErrNotLinked`, `ErrOffline`, `ErrIncompatible`,
  `ErrTooManyServers`; `deviceauth.Client.Info` and `DefaultHTTPClient`; `credstore.Open`, `credstore.NewMemory`, a
  `DeleteAll` behind `ForgetAll`; `deeplink.Parse/Build/NormalizeOrigin` and `links.json`; `compat.FromInfo`,
  `compat.FromError`, `compat.Default`. The `AuthService` contract of §11.7 is bound as this doc's `Auth` service
  (§6.2) with 02's semantics and JSON, plus `Probe`, with one change that 02 §11.7 and §11.8 adopt: every token-scoped
  method names its origin, `AccessToken(origin, force)`, `SessionEnded(origin, code)`, `HelloRefused(origin, error)`,
  and Go answers only when the origin is the active server's (§5.8).
- Web: `appLinkFor(serverOrigin, roomId?)` in `web/src/lib/appLink.ts`; the `/link` route.
- The uninstall flags are 06's: `--forget-credentials` on every uninstall and `--purge-data` on `/PURGE`, both over
  `ForgetAll` (02 §11.7, §15); the scheme command is `--open-url "%1"` (02 §12.2, 06 §2.3); this doc implements 02's
  injection rule on `--open-url`. The QR code is drawn in the SPA (uqr), not with `skip2/go-qrcode`.

**04-windows-engine.md**
- The engine behind 01's `media.Engine`: `Probe` once at start (encoders, borderless support, OS build,
  `unsupported_os`; the audio backend as `Probe.Audio.Backend`; `avcodec_license()` for `--self-check`),
  `Pick(ctx, media.PickRequest{OwnerWindow})` returning a `media.Source` with `Kind`, `Name`, `PID`, and
  `OSBorder` (whether Windows will draw the yellow border; 04 adds it), the monitor of a window source when 04 can add
  it; `media.ErrCancelled` for a closed picker; picker cancellation when the context ends.
- The error codes of PLAN's C ABI, surfaced through 01's `media.ErrorCode`, with 04 §5.7's detail tokens in
  `media.Error.Reason` (`pick_expired`, `self_window`, `driver_too_old`, `no_media_foundation`, `blocked_by_policy`,
  `missing`, `version`) and the 5 min pick-token life that §10.1's pick hold matches; the OpenH264 opt-in state for
  Settings.

**05-windows-audio.md**
- The concrete `*audio.Service` (`internal/desktop/audio`, 05 §14.2):
  `New(eng media.AudioController, SettingsStore, SelfInfo{PID, WebViewDataDir, Exe}, log)`, `Run`, `Close`; bound
  through `bridge.Audio`: `FriendsHear`, `Watch`, `SetVisible`, `WatchLevels` (the bridge's 10 Hz `Levels` events while
  counted), `SetAppDecision`, `SetKeepOutMicUsers`, `Decide` (`keep-out` or `include` only),
  `DismissPrompt(promptID) error` (new in 05: keeps the app out, doesn't ask again this run, saves nothing,
  `ErrUnknownPrompt` for an unknown id), `DismissTip`, `AppRules`, `SoundCheck` (its `reason` values incl.
  `device_busy`); no sound-mode method (M2 has no "All sound" mode); called by the app: `Exclusions`,
  `OnShareStarted(sharedPID, withAudio)` (before `StartShare`), `OnShareStopped` (after every share end), `SetPaused`
  (after 01's `Share.SetPaused` succeeds), `SetShareSound(on)` (new in 05: the Share sound switch, silent Opus frames),
  `SetSharedPID` (only if 05 keeps it apart from `OnShareStarted`; the controller passes the PID in `OnShareStarted`),
  `SetViewerBrowser(fromConnID, browser, os, playing)` (05 adds the sender's connection id, so several tabs are tracked
  apart), `Subscribe` (events `audio.report`, `audio.levels`, `audio.prompt`, `audio.alert` → this doc's
  `isshoni:audio.report`, `isshoni:audio.levels`, `isshoni:audio.prompt`, `isshoni:audio.alert`; the alert also drives
  the tray, the pill and the §15 toast).
- `audio.SettingsStore` (`AudioSettings`, `SaveAudioSettings`) and `audio.Settings`; `FriendsHear`, `Levels`,
  `Prompt`, `Banner`, `Tip` (05 §14.5); banner rules and texts (05 §14.7, §14.8) plus `volumeNote`;
  `media.SoundCheckResult`; `audio.RunSoundCheckHelper() int` for `--sound-check-helper`; `audiorules.Launchers`
  (05 §5.4) for the shell relaunch; the fake engine's audio scenarios (`ISSHONI_FAKE_AUDIO`, 05 §13.3).
- 05 §23 A9 is this doc's DK8 (audio part); no separate code.

**06-windows-release.md**
- `internal/update` (06 §8.9): `New(Options{CurrentVersion, Enabled, Channel, WizardDone, DataDir, Logger})`, `Run`,
  `CheckNow`, `Status`, `Subscribe`, `PendingAtStartup`, `Apply(ctx, *Pending, relaunch)`, `PolicyDisabled`, and a
  `Ready() *Pending` (the verified pending installer, for "Restart and update"; 06 adds it); error codes of §8.10.
- The installer: per-user install, the scheme command `--open-url "%1"`, the Start-menu shortcut with AppUserModelID
  `io.isshoni.desktop`, WebView2 bootstrap, `licenses\THIRD_PARTY_NOTICES.txt`, `/SERVER=` → `PendingServer`,
  `/LAUNCH` (through explorer.exe when elevated), `UpdateChecks`, uninstall removing the Run value and the
  notification keys of §14.1, every uninstall → `--forget-credentials`, `/PURGE` → `--purge-data`; `/download` with
  its "Open isshoni" button (06 §7.5), which turns `?room=<id>` into its link to that room (§12.3) and shows the
  Windows steps Download, Run it, Open isshoni; `install-desktop.ps1` passing the server through `/SERVER=`.
- Build: `cmd/isshoni-desktop/build/windows/icon.ico` and `build/windows/app.manifest` (this doc's files, contents in
  §4.5), `info.json` generated by `tools/desktoppkg` from the tag, `task desktop:exe:windows` with a `TAGS` variable
  (default `production`; `e2e` for §22.4 B), `tools/wails3` and `task release:wails3`; the release build with
  `-tags production`, `-H windowsgui`.
- CI: the jobs listed in §23's last row. Docs: the anchors listed in §23.

**M1 (as built)**: 01 §3.1 step 2 and `upgrade.go`'s bearer-only allowed origins; 01 §8.14's relay; 03 §7.5–§7.6,
§7.12; 04 §4.4 `Site.ExtraOrigins` and `wire.go`'s hub wiring; 05 §5's page-folder contract, §8's Platform, §11.1's
`RoomSession` and `ShareRecovery`, §12's viewer, §13's share machine and panel, §16.5's lazy namespaces and
`check:i18n`; `web/src/setup/QrCode.tsx`; `rooms/ConnTestDialog.tsx`.

---

## 25. Implementation slices

Ordered; each is testable on its own. Sizes: S ≈ 1–2 days, M ≈ 3–4 days, L ≈ 5+ days. Ids are `DK` ("desktop"; 02
keeps `DA` for "device auth"). Cross-doc needs name the sibling's slice.

| # | Slice | Size | Needs | Acceptance | Verified on |
|---|---|---|---|---|---|
| DK1 | **Shell skeleton and the Wails checklist**: Wails `v3.0.0-beta.34` in `go.mod`, `wails3` in `tools/wails3/go.mod` with `task release:wails3` (with 06 R1), `@wailsio/runtime` pinned to `3.0.0-beta.34`, `lint:pins` deriving it from `go.mod`; `cmd/isshoni-desktop` (`main.go`, `shell.go` minimal and `main_other.go` with 06 §5.2's constraints, `engine_release.go`/`engine_fake.go`, `build/windows/icon.ico`, `build/windows/app.manifest`, `build/tray/`); 06's `task desktop:exe:windows` builds it (with `TAGS`); `internal/desktop/assets` complete (§5.1–§5.6, the Guard's runtime allowlist included); WebView2 settings and the `WEBVIEW2_*` prefix scrub; the WML lint rule; `task desktop:dev` (macOS); depguard rules (§4.5) and the `GOOS=windows` lint | M | 02 DA1 (`protocol.DesktopAssetOriginWindows` for W1) | The app opens the SPA (boot shows NotFound or a placeholder) on the owner's PC and on the Mac; `go build` for windows/amd64 from the Mac and ubuntu with `CGO_ENABLED=0`; `GOOS=darwin CGO_ENABLED=0 go vet ./...` passes on ubuntu; `assets` tests pass on all three OSes; license gate and govulncheck pass for the Windows binary; §3.2 rows W1–W4, W9–W13, W15, W19, W20, W21 recorded with results and the tag's date (§3.3); the Guard's allowlist tests pass; `lint:pins` fails on a range, a tag or a version other than `go.mod`'s | Mac, CI ubuntu/macos/windows, owner's PC |
| DK2 | *Not used*: the desktop origin moved to 02 (DA1 constants, DA6 wiring); 02 DA6 needs only DK1's W1 result | — | — | — | — |
| DK3 | **Go foundations and the process contract, part 1**: `settings` (LOCALAPPDATA, `serverPrefs`, `UpdateChecks` initial read, `audio.SettingsStore`), `applog` (rotation, crash output, marker), `i18n` (+ `web.DesktopCatalog()`, `desktop.en.json` seeded), `hotkey`, `servers` (`Normalize`, `LocalID`, `Probe` on 02's client and `compat`), `app` skeleton with `HandleArgs`/`HandleLink` (the `--open-url` rule, `--server`, bare-argument logging); `main`'s §13.4 order with the running mutex, `--version`, `--updated-from`, the PendingServer/PendingRoom read; single instance (W6) | M | DK1; 02 DA8 (`deeplink`, `compat`), DA10 (`deviceauth.Client`) | §22.1 rows for these packages pass on all OSes; W6, mutex and `--version` tests pass on CI windows; §22.2's DLL search-order marker test passes on windows-2025 (main's first action); a link forwarded as `--open-url` to the running app reaches `HandleLink`; a `PendingServer` value opens the add step | Mac, CI ubuntu/windows |
| DK4 | **SPA platform**: `types.ts` changes, `detect.ts`, `DesktopPlatform` (entry chunk), `NativeBridge`, `WailsBridge` + generated bindings + drift check, `HttpBridge`, `tokens.ts`, prefixed storage, link interception, `SignalClient` `auth`, boot desktop branch, `buildVersion: ''`, routes and guards (`RequireDesktop`, `RequireBrowser` with `/link`, desktop `RequireAuth` by `AuthState`), `desktop/` folder with stub pages, `role` audit | L | DK1 | §22.3 platform, signal, router and chunk tests pass; `check:size` passes with DesktopPlatform ≤ 4 KB gzip in the entry; the browser e2e suite stays green unchanged | Mac, CI ubuntu |
| DK5 | **Bridge services and the e2e harness**: `internal/desktop/bridge` (every method of §6.2, unfinished ones returning `not_implemented`), events, error JSON (W11, W12), `bridge/codes` with its `tygo.yaml` entry and `desktop.errors.*` texts (incl. `engine.internal`); `internal/desktop/e2ehost` at `http://wails.localhost` (host-resolver mapping) with `deviceauth.OpenServers` on `credstore.NewMemory`, fake shell, OS hooks and `/__e2e/*`; `DESKTOP_E2E_BIN` in `fixtures.ts`; `desktop-security.spec` | M | DK3, DK4; 02 DA9, DA10 | `desktop-security.spec` passes in Chrome on the Mac and CI; bridge unit tests pass; the CDP spec skeleton attaches to the real app on CI windows and checks W1–W3, W5, W7 | Mac, CI ubuntu/windows |
| DK6 | **Wizard and servers**: `SetupPage` (server step with the "Open isshoni" hint, add step without network that skips the confirm step for the same host, confirm, the reload at activation, link step with QR, short URL, name and trust line, and its "Sign in with username and password instead" form over `Auth.LinkWithPassword`), `RelinkPage` (notices, `save_failed` banner), server switcher, Sign out (`confirmed`) and Remove, `Auth.*` over 02's `Servers` and `Manager` with the origin-bound token methods (§5.8), the `auth.state` event, `Auth.SessionEnded`/`HelloRefused` from C1 | M | DK5; 02 DA10 (client), DA5–DA7 (server, `/link`) | `desktop-link.spec` (with its password path and its second-server case: no token of B reaches A) and `desktop-relink.spec` pass; probe and link texts shown for each code and notice, both add-step branches (component tests); the QR test passes | Mac, CI ubuntu |
| DK7 | **Native sharing on the fake engine**: `session` (C2) on 01's core with the `core.Auth` adapter, `share` controller (§10.1, both mapping tables), `Share.*`, `ShareView`, `DesktopSharing`/`DesktopShare` (hint from the snapshot), desktop ShareSheet, first-share sheet, panel bar and Details disclosure (`WatchServerStats`), unreachable state with Test connection (`?conntest=1`), §10.5's engine errors by code and reason (pick again, self window, driver, Media Feature Pack, Smart App Control, reinstall, minimized), `RoomSession.adoptShare`, `findShareElsewhere` list and `stopOther(roomId, shareId)`, own-share `low` rule in `layerPolicy` | L | DK5; 01 C10, C14 (core, server stats), C02 (fake engine) | `desktop-share.spec` (without Pause) and `desktop-reload.spec` pass; §22.1 `share` and `session` tests pass; on the Mac, `task desktop:dev` shares the 640×360 Decodable test pattern into a room that a browser watches | Mac, CI ubuntu |
| DK8 | **Audio integration and UI**: `bridge.Audio` over 05's Service (`Watch` and `WatchLevels` counts, `SetAppDecision`, `Decide`, `DismissPrompt`, `DismissTip`, `AppRules`, `SoundCheck`; no sound-mode binding); the controller's `OnShareStarted`, `OnShareStopped`, `SetPaused`, `SetShareSound` and the SharedPID hand-off; What friends hear (sheet summary, first-share groups, panel list, levels, tips), mic prompt card and toasts by `Prompt.kind` (dismiss → `DismissPrompt`), banners from `FriendsHear`, the `volumeNote`, "Nothing is playing right now" (no M1 "No sound captured" rule); sound and voice wizard steps (incl. `device_busy`); Settings → Audio; the `--sound-check-helper` dispatch and the shell relaunch (§13.4); the endpoint viewer mute; `audio.SelfInfo` wiring | M | DK7; 05 A2 (fake scenarios), A8 (Service) | Component tests of §22.3 for the panel, sheets and wizard steps pass; the share sheet renders correctly under every `ISSHONI_FAKE_AUDIO` scenario (one Vitest/Playwright snapshot each); `desktop-link.spec` runs the real sound and voice steps on 05's fake; the relaunch test passes where a shell exists; on the owner's PC with 05's engine, the list shows Discord kept out and a browser heard, and R4 (05 §18.9) passes | Mac, CI ubuntu/windows, owner's PC |
| DK9 | **Tray, window, notifications, system events**: `tray` + Wails tray (W18), close and quit policy (W7, W14), one-time notice, toasts with nonce actions (W8, `Options.Name`, the registry keys test), `-Embedding`, WDA on the main window, `hideFromCapture`, keep-awake, `winos` thread with lock/unlock/suspend/end-session, crash watchdog (W17) | M | DK7 | §22.1 `app` and `tray` tests, §22.2 tests and `desktop-system.spec` pass; M-DESK-4, M-DESK-6, M-DESK-11 pass on the owner's PC | Mac, CI ubuntu/windows, owner's PC |
| DK10 | **Share safety**: global hotkeys (F9/F10 defaults, the `typesChar` warning), Pause end to end (controls, hotkey, tray, lock with its 5 min and 30 min bounds and keep-awake release; `Share.SetPaused` then `audio.SetPaused`; viewer `paused` tile with no freeze detection), the pill (§11.4), the DND tip | L | DK9; 01 C08, C09, C12 (`share.pause`), 04 (`OSBorder`, `NoOSIndicator` through 01) | `desktop-share.spec` with Pause, the late joiner and hotkeys passes; `desktop-system.spec`'s lock cases (resume within the bound, "Resume sharing?" after it, the stop at the second bound, keep-awake released and re-taken) and the controller's synctest lock cases pass; pill tests of §22.2 pass; M-DESK-2, M-DESK-3, M-DESK-6, M-DESK-8 (Win10) pass | Mac, CI ubuntu/windows, owner's PC |
| DK11 | **Handoff and web entries**: `playback.stop` sender and receiver, the `playback.state` sender (`rooms/handoff.ts`) and receiver (01's `EventPlaybackState` → `SetViewerBrowser`, expiry through `App.OwnConnections`), web hello features, `handedOff` viewer state, "Open in the isshoni app" with `appLinkFor`, deep links end to end (§13.2) | M | DK7; 01 C07, C08, C14 (relay kinds, `user.connections`, `EventPlaybackState`); 02 DA7 (`appLink.ts`) | `desktop-handoff.spec` passes; deep link table tests pass; M-DESK-7 and M-DESK-9 pass | Mac, CI ubuntu, owner's PC |
| DK12 | **Settings page, autostart, updates UI, version screens, process contract part 2**: `SettingsPage` (all sections of §14.4), `Settings.*` and hotkey field, autostart (`--autostart` without a window), `Updates.*` on 06's updater with every state of §16.2 (required banner, expired and failed texts, beta channel, `policyOff`), the start-time apply and `PendingRoom`, `--update-now`, `--forget-credentials` and `--purge-data` (after 02's `DeleteAll`), `--self-check` (after 04's probe), About (versions, logs, notices, copy version info, anchors), VersionMismatch desktop actions (02 §13.3, server `/download`), OpenH264 opt-in placement | M | DK9; 06 R8 (updater), 02 DA10 (`ForgetAll`), 04 (probe and encoder caps) | Component tests per section pass; autostart round trip on CI windows; an update offered in a 06 test channel is shown, refused while live, and applied at start with the Pending values (06 R9's `--update-now` e2e); `--forget-credentials` leaves no test credential, and `--purge-data` no `%LOCALAPPDATA%\isshoni` and no test credential; `--self-check` passes on CI windows; §22.1's start-order row (step 2a, the installing mutex) passes; M-DESK-10 passes | Mac, CI ubuntu/windows, owner's PC |
| DK13 | **Real-app e2e and the manual matrix**: the CDP spec complete (§22.4 B), the remaining checklist rows (W8, W16, W17, W18), the manual matrix M-DESK-1…12 on Win11 and Win10, the budgets of §20 measured and recorded | M | DK1–DK12; 06 R4 (installer for M-DESK-1), R10 (ps1) | CDP spec green on CI windows; every M-DESK row passed or has a recorded, accepted exception; §3.2 fully filled in | CI windows, owner's PC |

The cross-doc order that matters: DK1 before 02's DA6 (the W1 result); DK3 after 02's DA8; DK6 after 02's DA10;
DK7 after 01's C10; DK8 can start on 05's fake (A2) and Service (A8) and switch to 05's engine on Windows; DK10
after 01's C12 and 04's border report; DK12 after 06's R8.

---

## 26. Open questions for the owner

1. **Close button when not sharing**: quit (this doc's default, D13) or keep running in the tray like Discord? The
   setting exists either way; only the default is the question.
2. **Locking the PC while sharing**: pause and resume on unlock (this doc), or pause and stay paused until the user
   resumes? Either way the bounds of §11.6 hold and aren't part of the question: keep-awake is released while a lock
   pause lasts, a lock pause stops the share after 30 min, and with this doc's default only an unlock within 5 min
   resumes on its own (a later one asks "Resume sharing?"). Answering "stay paused" makes every unlock ask.
3. **Handoff scope**: stop playback in every desktop browser tab of the user in that room (this doc, D10; a tab on
   another computer gets "Watch here instead"), or only in the tab that clicked "Open in the isshoni app"?
