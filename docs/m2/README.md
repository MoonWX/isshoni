# M2 design: overview and implementation plan

M2 is "Windows app": the Wails desktop app for Windows, its native engine, device linking, the installer, the updater
and the first 0.x release with the app. This page is the short version of the six M2 specs (about 12,500 lines
together). It says what M2 delivers, how the parts fit, the rules every slice follows, and the order in which the work
gets built. `docs/PLAN.md` stays the source of truth for product decisions; the six specs are the source of truth for
their own parts; M1 (`docs/m1/README.md` and its six specs) is the record of what exists. Where this page and a spec
disagree, the spec wins and this page gets fixed, except for the conflicts that §3 decides: slice WA01 writes those
into the specs.

## 1. What M2 delivers

From the plan's roadmap:
- A Wails v3 shell (pinned to `v3.0.0-beta.34`, the newest v3 tag on the Go proxy as of 2026-10-21, published
  2026-10-20; there is no GA yet) that loads only the bundled SPA, and a client core that builds and runs on the fake
  engine on Windows, macOS and Linux (macOS and Linux compile and test in CI).
- The Windows engine `isshoni-media.dll`: WGC through the OS picker (borderless on Windows 11), DXGI Desktop
  Duplication for monitors on Windows 10, a D3D11 VideoProcessor for NV12 and the preview layer, libavcodec from an LGPL
  FFmpeg build (NVENC, QSV, AMF, MF), the include-set mixer of spike S1, and Opus.
- Device-flow linking (RFC 8628) with rotating refresh tokens in Windows Credential Manager, the share sheet with
  "What friends hear", the first-run wizard, and share safety (red tray, hotkeys, Pause, the Sharing pill, no isshoni
  window in any capture).
- An NSIS installer, `install-desktop.ps1` served by every server's `/download` page, a signed in-app updater, the
  first 0.x release with the app (`v0.2.0`), then the SignPath Foundation application.

Not in M2: the macOS app (M3, which shares Wails, the updater, the C ABI and `native/common`), the Linux agent (M4),
web-initiated desktop shares through `share.request` (M4), direct AMF/MF bitrate control and the 720p30 middle layer
(M5), "Copy diagnostics" and desktop "X started sharing" toasts (M5), Authenticode signing (after SignPath approves,
M3's time), Windows on ARM64, and an "All sound" mode. M2 leaves room for each (01 §2, 03 §21, 04 §16, 05 §20).

**Exit criterion**: Discord is excluded (the audiotest passes on Windows 10 22H2 and Windows 11), and glass-to-glass
latency is under 200 ms at 1080p60. The exact gates are 05 §18.10 and 01 §12.2; §6 outlines the run book, and §7
asks the owner to confirm how "under 200 ms" is read. Windows 10 takes the plan's gate (§3 item 52): it gets endpoint
loopback with the banner, and its audio gate is 05 §18.10's reduced form.

### The six specs

- [**01-client-core.md**](01-client-core.md): the Go code below the window. `internal/media` is the stdlib-only media
  contract (`Engine` and `Session` mirroring the C ABI, `Packet`, `Event`, errors, probe types), so `internal/client`
  stays gomobile-friendly and M1's carried depguard rule goes live; `fake.Engine` and `mediatest.Conformance` stand in
  for the DLL. The publisher keeps M1's packetizer, anchors SRs on the engine's QPC clock, sends no RTX (a NACK
  responder of 4096 packets) and paces video at 3× its targets. Rate control is server-estimated (a new SFU
  `nativeUplink`) and client-applied (`ratectl`: 4 → 8 Mbit/s ramp, 15 %/2 s live or 30 %/10 s re-init guards, "Your
  upload allows about 720p"). `internal/client/core` runs the app's Go `publisher` connection: it joins a room only
  while sharing, owns one media pump, holds a share 30 s through outages and republishes with `replaces`. It also owns
  the server pieces only it needs (`share.pause`, `user.connections`, the native estimator), the relay payloads
  `playback.stop` and `playback.state`, and the latency measurement (`tools/g2g`) with two CI proxies.
- [**02-device-auth.md**](02-device-auth.md): the RFC 8628 flow M1 left as stubs. `POST /device/code`, approval only
  in a cookie-signed-in browser at `/link?code=…` (code, app and version, editable device name, a different-network
  warning), 8-letter codes valid 10 minutes, polling every 5 s. Access tokens last 15 minutes; refresh tokens last 90
  days, rotate on every use and are stored as HMAC hashes, and a reused one revokes the device except for an honest
  retry. Desktop origins work for CORS and `/ws` only with bearer tokens. The plan's in-app username/password
  fallback is `POST /device/password`. Client side: `deviceauth` (linking, single-flight refresh, `servers.json`),
  `credstore` (Credential Manager, target `isshoni:v1:<origin>`), `deeplink` (`isshoni://<host>[:port]/[r/<roomId>]`,
  no query string ever read) and `compat` ("Update the app" or "Ask your admin").
- [**03-desktop-app.md**](03-desktop-app.md): the Wails app. Only `cmd/isshoni-desktop` imports Wails; all logic is in
  `internal/desktop/*`, which tests on every OS, and the app runs on the Mac with the fake engine. Our asset handler
  serves `web.Dist()` with `bridge.js`, a strict CSP and no service worker; navigation is locked. The SPA calls the
  server directly with bearer tokens it gets from Go (only Go holds and refreshes the refresh token). The SPA's viewer
  connection (C1) and Go's publisher connection (C2) are one Participant. A Go share controller owns the share; the
  desktop share sheet, first-share sheet and panel show "What friends hear". Share safety: red tray, hotkeys registered
  only while live, Pause through `share.pause`, a native Win32 Sharing pill, `WDA_EXCLUDEFROMCAPTURE` on every window,
  pause on lock. Handoff from a browser tab uses `playback.stop`. Tests: a desktop-in-Chrome harness, the real
  `isshoni.exe` over CDP on windows-2025, and a manual matrix on the owner's PC.
- [**04-windows-engine.md**](04-windows-engine.md): the C ABI v1 (`native/include/isshoni_media.h`: pull-only,
  sessions behind generation-checked handles, events through `im_next_packet`, `struct_size` everywhere, snprintf-style
  JSON) and the C++20 DLL. Portable code in `native/common` (rings, Annex B normalizer, pacer, encoder policy, the Opus
  stage) tests on the Mac. Capture via WGC and DD; one D3D11 device per session; libavcodec loaded at run time (the
  engine still works audio-only without FFmpeg) with a fixed fallback order down to MF software and the opt-in
  OpenH264; time-based GOPs, SPS/PPS before every IDR, one profile per session; a lost GPU device ends the share with
  "Share again" (recovery is M5). The Go binding
  `internal/media/native` uses `x/sys/windows` (no cgo, no purego) and its logic tests everywhere against a fake
  caller. Tools: `cmd/isshoni-enginetest` and `hwcheck.ps1`.
- [**05-windows-audio.md**](05-windows-audio.md): "all system audio except voice apps". S1's include set (one process
  loopback per allowed audio process, mixed on a 10 ms QPC clock with S1's fades and the onset fade), with
  classification and planning in portable C++ (`audiopolicy`), mixing and adaptive resampling in `audiomix`.
  Unclassified processes are never captured, mic users stay out until their app's root exits, held handles stop PID
  reuse, and the fast path (one EXCLUDE stream on isshoni's tree) is cut retroactively when a voice app appears. Go owns
  the rule data, settings, prompts and the `FriendsHear` model. `cmd/isshoni-audiotest` is the exit gate: 24 required
  cases (4, 5 and 26 only while the release ships the fast path) on a pre-Opus PCM tap, run on the owner's Win11 and
  Win10 22H2 against the release candidate's own DLL (Win10 in 05 §18.10's reduced form: it takes the plan's gate,
  §3 item 52).
- [**06-windows-release.md**](06-windows-release.md): per-user NSIS install under HKCU (no UAC), an updater that runs
  that installer silently, Ed25519 update manifests in SSHSIG format with their own key, FFmpeg 9.0.2 built once per
  pin and locked by SHA-256, `install-desktop.ps1` verifying `checksums.txt.sig` with Windows' inbox OpenSSH and served
  pre-filled by each server, the `/download` page, the CI and nightly jobs (`native-host`, `windows-go`,
  `windows-native`, `windows-desktop`, `desktop-macos`, a dispatch-only `audiotest.yml`), goreleaser `extra_files`, the
  first-release checklist, unsigned 0.x with SmartScreen docs, and the SignPath application.

## 2. Architecture

```
 Windows PC: isshoni.exe (Go, CGO_ENABLED=0; one per Windows user, single instance, a child of explorer.exe)
 ├─ cmd/isshoni-desktop: mode dispatch (03 §13.4) · Wails v3 shell: window, tray, toasts
 │    └─ WebView2 at http://wails.localhost: bundled SPA (web.Dist) + DesktopPlatform
 │          │ bindings + events (NativeBridge)          ├─ REST /api/v1 with Bearer (CORS) ─────────────────► server
 │          ▼                                           └─ C1: WSS /ws, role viewer, hello.auth ────────────► server
 ├─ internal/desktop (03): app · bridge · share controller · session · settings · tray · winos (hotkeys, pill, WDA)
 │    ├─ audio.Service (05): rules, prompts, banners, What friends hear
 │    ├─ deviceauth.Servers (02): servers.json, one Manager per server ── /device/code, /device/token ──► server
 │    │     └─ credstore: Windows Credential Manager (the refresh token; never on disk, never in the SPA)
 │    └─ update.Updater (06) ── signed manifest, daily, can be turned off ──────────────────► GitHub Releases
 ├─ internal/client/core (01): C2, role publisher ── WSS /ws (hello.auth) + pub PC (RTP, own SRs) ──► server
 │    media pump (the only caller of Session.Next) · publish (pacer) · ratectl ◄── quality.hint (nativeUplink)
 │          ▲ media.Engine (internal/media contract; fake.Engine in tests and on macOS/Linux)
 │    internal/media/native (04): x/sys/windows LoadLibraryEx + SyscallN, no cgo
 └─ isshoni-media.dll (04, audio half 05): C ABI v1, pull-only (im_next_packet), never calls into Go
      video: GraphicsCapturePicker → WGC (Win11; Win10 windows) | DXGI DD (Win10 monitors) → D3D11 NV12 + preview
             → libavcodec NVENC/QSV/AMF/MF (avcodec/avutil DLLs, loaded at run time) | OpenH264 (opt-in download)
      audio: session watcher → include-set process loopback → 10 ms QPC mixer → Opus 20 ms (04's stage)

 isshoni server (M1, plus): /api/v1/device/*, bearer on REST and /ws, desktop origins bearer-only (02)
   · share.pause, user.connections, SFU nativeUplink (01) · GET /install-desktop.ps1 and the /download page (06)
 Browser tabs of the same user ◄── relay playback.stop / playback.state ──► the app (handoff, 03 §12.2)
```

C1 and C2 authenticate as the same device, so presence shows one person; C2 is in a room only while it shares there,
and a share follows the SPA's room (01 D12, 03 D6). Pause drops media at the pump at once, then freezes the engine's
frame and mutes its audio (01 D14).

| Path | Spec | What |
|---|---|---|
| `cmd/isshoni-desktop` | 03 | Wails app `main`: mode dispatch, flags, shell, engine choice (`native` in release builds, `fake` with `dev`/`e2e`) |
| `internal/desktop/{app,bridge,bridge/codes,assets,servers,session,share,settings,i18n,applog,tray,hotkey,winos,e2ehost}` | 03 | app logic, bound services, asset handler, share controller, C2 session, OS hooks, the e2e harness |
| `internal/desktop/audio` | 05 | audio policy service: settings, prompts, banners, `FriendsHear`, the sound-check helper |
| `internal/client/core` | 01 | the app's Go publisher connection: shares, media pump, recovery, hints, pause, stats |
| `internal/client/{publish,ratectl}` | 01 | RTP publisher (M1 plus clock, pacer, stats); uplink rate control |
| `internal/client/signal` | M1 01 | unchanged |
| `internal/client/{deviceauth,credstore,deeplink,compat}` | 02 | linking and tokens, `servers.json`; Credential Manager; `isshoni://` links; version verdicts |
| `internal/media` | 01 (`audio.go`: 05) | the media contract, stdlib only |
| `internal/media/{fake,mediatest}` | 01 (`fake/audio_ctl.go`: 05) | fake engine; engine conformance suite |
| `internal/media/native` | 04 (`audio*.go`: 05) | Go binding of `isshoni-media.dll` |
| `internal/media/audiorules` | 05 | built-in voice apps, launchers, runtimes, browsers |
| `internal/protocol` additions, `internal/protocol/relay` | 01 (`desktop.go`: 02) | `share.pause`, `user.connections`, relay payloads; the desktop origins |
| `internal/protocol/api` (`device.go`), `internal/version` (`semver.go`) | 02 | device DTOs; SemVer for the hub, `compat` and the updater |
| `internal/update` | 06 | the updater |
| `internal/server/{store,auth,httpapi}` device parts, `internal/server/signal` pre-auth refund and `DeviceName`, `wire.go` origins | 02 | device flow, tokens, CORS, bearer |
| `internal/server/signal` (`share.pause`, `user.connections`), `internal/server/sfu` (`nativeUplink`, sharer pause) | 01 | server counterparts of the core |
| `internal/server/httpapi/desktopscripts.go`, `deploy/embed.go` (Go package `deploy`) | 06 | `GET /install-desktop.ps1` |
| `internal/server/itest` (`desktop_*` / `device_*`) | 01 / 02 | integration tests with the core / the device flow |
| `native/include/isshoni_media.h` | 04 (audio block: 05) | the C ABI v1 |
| `native/{CMakeLists.txt,CMakePresets.json,vcpkg.json,vcpkg-configuration.json,.clang-format}`, `native/common/` (with `audio/`) | 04 | CMake root, portable C++20 and its tests |
| `native/common/{audiopolicy,audiomix}` | 05 | portable audio policy and mixer |
| `native/windows/` (`src/audio/`: 05) | 04 | the DLL |
| `native/ffmpeg/` | 06 | FFmpeg pins, build script, `lock.json` |
| `cmd/isshoni-enginetest`, `native/windows/tools/hwcheck.ps1` | 04 | engine smoke, bench, soak, hardware run |
| `cmd/isshoni-audiotest` | 05 | the audio exit gate |
| `tools/g2g` | 01 | glass-to-glass measurement |
| `tools/{desktoppkg,wails3}`, `tools/notices` (native), `tools/repocheck` (pins, keys) | 06 | packaging, the Wails CLI pin, notices, repo checks |
| `web/src/platform/desktop`, `web/src/desktop`, `web/src/share/native`, desktop parts of `web/src/{app,rooms,viewer}` | 03 | `DesktopPlatform`, desktop pages, native share UI, handoff |
| `web/src/link`, `web/src/lib/appLink.ts`, DevicesPage rows | 02 | approval page, app links, devices |
| `web/src/download` | 06 | the `/download` page |
| `deploy/windows/`, `deploy/install-desktop.ps1`, `deploy/desktop/`, `deploy/keys/update_*`, `deploy/notices/native-windows.json`, `.github/workflows/*` | 06 | installer, script, update keys, notices, CI and release |
| `docs/` site pages | 06 (device rows and `#desktop-linking`: 02) | download, privacy, code signing, security, troubleshooting |

**Import rules** (01 §4 and 03 §4.5 are the tables; `.golangci.yml` encodes them): Wails is imported only by
`cmd/isshoni-desktop`; non-test files under `internal/client/` import from this repository only their sibling client
packages, `internal/protocol` (and `api`, `relay`), the `internal/media` root, `internal/version` and `internal/logx`;
engines enter only through `cmd/isshoni-desktop` (`native` always, `fake` in `dev || e2e` files) and
`internal/desktop/e2ehost`; the server never imports `internal/desktop` or Wails, and the app never imports
`internal/server`. Third-party modules are governed by the license gate, not by depguard (01 D2). As WA02 wrote
them: a catch-all `wails` rule keeps Wails out of every package except `cmd/isshoni-desktop` (and `tools/wails3`);
06's `deploy-importers` (only `internal/server` imports `deploy`, 06 §7.4) and `update` (06 §8.9) rows are there
too; `media` and `media-audiorules` allow only the standard library; and `desktop-no-wails`, `server-no-desktop`,
`desktop-main-no-fake` and `wails` also cover test files, while the other M2 rules, like M1's, cover non-test files.

## 3. Integration decisions

The specs were written in parallel and then reconciled. The outcomes:

**Changes against the plan**
- The Windows binding lives in `internal/media/native` (not `internal/media/engine_windows.go`) and uses
  `x/sys/windows` (`LoadLibraryEx`, `SyscallN`), not purego (01 D3–D4, 04 §2). The CMake root, presets and vcpkg
  manifest are at `native/`, not `native/windows/`, so portable `native/common` and its host tests share one root.
- The update manifest is camelCase JSON `{version, channel, minVersion, expiresAt}` plus the installer's URL, size and
  SHA-256, signed in OpenSSH's SSHSIG format (namespace `isshoni-update`) with a key of its own (06 D3, D23).
- Per-user install under HKCU with no UAC prompt, and the updater runs the NSIS installer silently (06 D1–D2).
- The desktop SPA serves rooms, viewing, sharing and its own pages; login, invite, account, admin and `/link` open in
  the system browser (03 D7).
- No "All sound" mode in M2: the built-in voice apps are always kept out, and the per-share "Share sound" switch is the
  only off (05 §2). Mic users stay kept out until their app's root process exits (S1 used 10 s).
- Resolution and frame rate stay fixed per share; only bitrates adapt (01 D11). Native publishers send no RTX (01 D7).
- Windows x64 only (06 D20).
- New packages not in the plan's layout: `internal/client/{core,deviceauth,credstore,deeplink,compat}`,
  `internal/desktop/*`, `internal/update`, `internal/media/{native,mediatest,audiorules}`, `internal/protocol/relay`,
  the Go package `deploy`, `tools/{g2g,desktoppkg,wails3}`, `cmd/isshoni-enginetest`.

**Who owns what**
- 01 owns the media contract and the fake; 04 owns the header, the DLL and `internal/media/native`; 05 owns the audio
  block's semantics, `internal/media/audio.go`, `internal/media/native/audio.go` (WA10 put the audio procs in 04's
  export table, so no `audio_windows.go` is needed), the audio folders and the audiotest. 04 owns the Opus stage and
  the path from the mixer to `im_next_packet`; 05 implements `AudioEngine`/`Mixer` behind 04's `PcmSink`.
- Server changes: 01 owns those that serve the core (`share.pause`, `user.connections`, `nativeUplink`); 02 owns every
  device change (store, auth, device routes, CORS, bearer, the origins in `wire.go`); 06 owns `/install-desktop.ps1`
  and `/download`. No other server change is planned.
- 02 owns the origin constants, the deep-link grammar and parser; 03 owns the command line, `HandleLink` and `main`;
  06 owns the registry, the installer and the process-contract table (06 §2.4), which 03 implements.
- 03 owns every app string (namespace `desktop`, with 05 §14.8's English); 02 owns `link`; 06 owns
  `account.download.*`. 03 pins Wails; 04 owns the CMake files; 06 owns every workflow file and, after WA04, the
  Taskfile's structure.
- Glass-to-glass is 01's `tools/g2g`; 04 supplies the component numbers (stats `latencyMs`, `enginetest bench`).

**Conflicts between the specs, decided here** (WA01 wrote each into the named sections)
1. **Deep-link command line**: `"<install dir>\isshoni.exe" --open-url "%1"` (02 §12.2, 03 §13.1). A launch that
   contains `--open-url` takes exactly the next argument, which must start with `isshoni:`, and honors no other flag.
   06 §2.3, §2.4, §16, §17, §18 and R4 drop `--deeplink -- "%1"`.
2. **One data root**: `%LOCALAPPDATA%\isshoni\` holds `settings.json`, `servers.json`, `WebView2\`, `logs\`,
   `codecs\` and `updates\` (03 §14.1); nothing roams, because device ids and the `CRED_PERSIST_LOCAL_MACHINE`
   credentials are per PC. 02 §11.6 (the `Dir` comment), §18 and 06 §2.2, §6.8, §13 change from `%APPDATA%`.
3. **Uninstall flags**: every uninstall runs `--forget-credentials`, `/PURGE` runs `--purge-data` (06 §2.4, §6.8);
   both call 02's `Servers.ForgetAll` with one 5 s budget, and `--purge-data` then deletes the data root. 03 §13.4 and
   §23 add `--forget-credentials`; 02 §15 and M-DA-7 name both.
4. **`--self-check` JSON**: 06's engine fields (`loaded`, `abi`, `ffmpegLoaded`, `ffmpegLicense`, `testHooks`) plus
   03's `spa.ok`; `ok` needs all of them (03 §13.4, 06 §2.4).
5. **Loader name**: `native.Open(native.Options)` (01, 03, 04 §19, 05); 06 §2.4 and §18 say `Load`.
6. **DLL name**: `isshoni-media.dll` (04, 06, the plan); 01 §2, §7.1 and 05 §1, §18.1, §21, §22 say
   `isshoni_media.dll`. The exit tools artifact (§6) carries `isshoni-media.dll`.
7. **Latency measurement**: 01 §12.2's `tools/g2g` (monitor share, one monitor, server RTT ≤ 50 ms, p95 < 200 ms over
   60 s, Win11 and Win10 22H2) is the exit measurement. 06 §2.1, §11.8 item 6, §16, §17, §18 and R17 drop
   `isshoni-latency.exe`, the window share and RTT ≤ 40 ms; `task native:dist` ships `g2g.exe`.
8. **Password fallback**: built in M2, because the plan says "The fallback is an in-app username/password form" (02
   decision 12, §14). 03 binds `Auth.LinkWithPassword` (02 §11.7's `AuthService` method) and adds "Sign in with
   username and password instead" to the wizard's link step (WA71), keeps WebView2's autofill and password saving off,
   and drops "the app has no password field in M2" (§5.4) and "no password form" (§8.2).
9. **Relay into the app**: 01's typed `EventPlaybackState{From, Playing, Browser, OS}` replaces 03's generic
   `EventRelay` (03 §12.2, §24, DK11); in M2 the core serves no relay request kind (01 D16), so 03 §2's "01 serves
   `share.request`" is corrected.
10. **Audiotest CLI and runner**: 05's CLI is the interface (`run -gate [-long] -out`, exit codes 0–4); 06 §10.4 maps
    suite `release` to `run -gate` (on the `isshoni-exit-tools` of the run named by `release_run_id`), `full` to
    `run -gate -long` and `hwcheck` to `hwcheck.ps1`. Runner labels are
    06's (`self-hosted`, `windows`, `x64`, `isshoni-audio`, `win11` or `win10`), with 05's two extra safeguards:
    ephemeral registration and an environment that needs the owner's approval.
11. **Windows CI**: `windows-2025` everywhere (06 §3); 01 §14 and §16 say windows-latest. Go tests on Windows run in
    `windows-go` (MinGW `-race`, then `CGO_ENABLED=0`, the shipped mode) and, with the DLL, in `windows-desktop`.
12. **`TestCompatServer`** exists (01 §14, WA55): 06 §10.3's and §18's "open gap" notes go. 06's references to 03's
    slices `DA1`–`DA13` mean `DK1`–`DK13`, and 03 §24 and M-DESK-1 call the `/download` button "Open isshoni" (06 §7.5).
13. **Moved acceptance items**: 06 R3b's `enginetest probe -quick` check moves to WA44 (the tool comes with 04 E7);
    05 A5's and A6's owner's-PC cases run right after group 6 merges, with WA62a's harness (the first half of 05 A7);
    a hardware failure there becomes a `WF` slice that lists `native/windows/src/audio/`, never a change in a harness
    PR. As built (WA62a): cases 20, 21 and 27 drive real capture that no Mac run has exercised, so a first failure in
    one of them is judged (harness or engine) before a `WF` slice is opened (§5, WA62a's row); the same holds for
    WA62b's endpoint reads, cases 13, 14 and 22 and the soak (WA62b's row, §8).
14. **CMake**: WA03's `native/common/CMakeLists.txt` adds `audio/`, `audiopolicy/` and `audiomix/` with `if(EXISTS …)`
    guards, and each library globs its own `tests/*_test.cpp`, so 05's slices touch only their folders.
15. **No sound mode**: M2 exposes no sound-mode choice, and the per-share "Share sound" switch (05's
    `Service.SetShareSound`) is the only off. 03 binds no `Audio.SetMode` and declares no `AudioMode` in the bridge;
    Settings → Audio shows the built-in list as locked text ("Always kept out: Discord, TeamSpeak, …"), the mic toggle,
    the per-app rules and the sound-check rerun. Go rejects `media.AudioEverything` with `CodeInvalid`, without a DLL
    call, both in `media.Config.Audio` at start and in `SetAudioMode`; the DLL answers `IM_ERR_INVALID_ARG` for
    `IM_AUDIO_MODE_EVERYTHING` in `im_start` and `im_audio_set_mode`; the constant stays reserved. Each layer refuses
    it, so the plan's "always kept out" doesn't depend on which layer a later caller uses (01 §5.1, §7.1, §11.2; 03
    §6.2, §7.4, §14.4, §24, DK8; 04 §5.2, §5.3, §10.2, §12.2, §18.2–§18.3; 05 §4.1, §6.1, §10.5, §13.1, §19.1).
16. **Engine events**: 04 §12.3 follows 01 §5.2's table: `LIMIT_CHANGED` cpu, gpu and none → `EventLimit{Layer,
    Limit}`; `ENCODER_CHANGED` → `EventEncoderChanged{Layer, Encoder}`, delivered before the new encoder's
    `EventLayer`; there is no `EventOverload`. Fallback is the engine's (01 D15): 03 §10.5 shows "Switched to
    {encoder}" on `share.notice` `encoder_fallback`, has no core-level fallback or per-share count, and ends the share
    on `encoder_lost` from `Next`; reason `device_removed` gets the stop notice with "Share again" (04 §5.7, §9.8).
17. **Engine error codes**: 01 §5.2's mapping wins. `IM_ERR_DEPENDENCY` → `dependency` and `IM_ERR_ABI` →
    `dependency` with Reason `abi` (the reinstall, Smart App Control and driver texts); `IM_ERR_NO_ENCODER` →
    `no_encoder`; the detail token is `Reason` in both. A pick of an isshoni window makes `Pick` return
    `*media.Error{invalid, self_window}` after the binding released the token; `media.Source` has no `Self` field, so
    no caller can forget to check a flag (01 §5.1–§5.2, 03 §10.1, §10.5, §22.3, 04 §12.2, §18.3).
18. **Logs name no app**: no executable, process or app name at any level, debug included, in Go logs or in the
    engine's log ring that the binding drains (03 §17; 04 §5.6 and E14's log test at info and at debug level; 05 §16
    and its debug-level log test).

Items 15–18 came out of the design review and were in the named sections before WA01, which checked them.

**Decided after group 1** (WB1 wrote each into the named sections; they are the inconsistencies (a)–(h) that WA01's
review found and left open)
19. **`FFmpegDir` default**: `native.Options.FFmpegDir` `""` means `Dir`, so the binding passes
    `im_init_params.ffmpeg_dir = NULL`, the engine DLL's own folder: the installer puts the FFmpeg DLLs next to
    `isshoni-media.dll` (06 §2.2), and `im_build_info`'s `ffmpeg.dir` is `"."` then (04 §5.2, §5.6, §12.2, §13.3).
    01 §7.1 drops `<Dir>\ffmpeg`.
20. **Audio backend in the probe**: `Probe.Audio.Backend`, computed by `media.AudioCapsFromFlags`; there is no `Mode`
    (or `Degraded`) field (01 §5.1, 03 §6.3, §24, 04 §12.2). WA01's review fixes already removed 03 §24's
    `Probe.Audio.Mode`; no spec names it now.
21. **`playback.state` has no `v`**: the payload is `{playing, browser, os}`, `relay.PlaybackState{Playing, Browser,
    OS}` (01 §11.11, WA18). Its only receiver is the core, which ignores unknown fields, so a later field is additive
    and a change of meaning takes a new kind. `playback.stop` keeps `v: 1` (its receiver is the web client). 03
    §12.2, §23 and §24 drop `v: 1` and `V`.
22. **`SetViewerBrowser` takes the sender's connection first**: `SetViewerBrowser(connID, family, os string, playing
    bool)` (05 §14.2), called with `EventPlaybackState`'s `From`, `Browser`, `OS`, `Playing` in that order (01
    §11.10–§11.11, 03 §12.2, 05 §22), so several tabs are tracked apart. `os` is M1's `ClientOS` spelling, which
    `audiorules.OSNames.For` accepts next to GOOS (05 §5.4, WA06).
23. **Link binding names**: 03's `Auth` service binds 02 §11.7's methods as `Auth.Link`, `Auth.CancelLink` and
    `Auth.LinkWithPassword` (item 8; 02 §11.7, §14, §18, §20; 03 §6.2, §8.2); there is no `Link` service and no
    `Link.Password`.
24. **The compat admin file**: `ISSHONI_COMPAT_ADMIN_FILE` is `{"username", "password"}` (mode 0600), and both compat
    tests, 01's `TestCompatServer` and 02's `TestCompatServerDeviceFlow`, take the server only from
    `ISSHONI_COMPAT_SERVER_URL`: one source for the URL (01 §14, §15; 02 §13.4, §18; 06 §10.3). 02 and 06 drop the
    file's `origin`.
25. **Audiotest `-out` is a folder**: `run … -out <dir>` (default `.`) writes the report into that folder under 05
    §18.7's name, as 05 §18.8, 06 §10.4 and the exit run book (§6) call it with `-out .`; 05 §18.2 drops `-out file`.
26. **The Actions approval setting** is GitHub's "Require approval for all external contributors" (06 §10.5, §14
    item 4; §8 here); 05 §18.8 drops "outside collaborators".

**Decided at group 2's merge and for group 3** (WB2 wrote each into the named sections)
27. **The build-version warning is the app's**: `internal/media/native` logs the engine's build, ABI and commit at
    `Open` and doesn't know the app's version (depguard keeps `internal/version` out of it, 01 §4); the app compares
    `Info().Build` with its own version at startup and logs a warning on a mismatch, refusing nothing (WA10-R4's
    option (b); 04 §5.5, 03 §13.4 step 7).
28. **One session at a time**: `native.Engine.Start` returns `media.ErrBusy` while a session it started runs, with or
    without audio, like the fake and the core's one-share rule (D12); `StartExtra`, used only by
    `isshoni-enginetest sessions`, runs up to the DLL's 4 video-only sessions (`IM_ERR_LIMIT` beyond). "Native: up to
    4" describes the DLL, not `Start` (01 §5.1's `Engine` comment, §5.3's Sessions rule, §13; 04 §12.2).
29. **`SourceDetail` holds only what `media.Source` lacks**: `Mapped` and `Window`; the monitor id, capture path and
    OS border are `media.Source`'s `MonitorID`, `Capture` and `OSBorder` (04 §12.2, and the references in §18.4 and
    §20). `internal/media/native/types.go` declares exactly these two fields: group 3's integration dropped the
    `MonitorID`, `CapturePath` and `OSBorder` that WA10 had declared, and adjusted `bridge.go`'s `decodeSource`.
30. **"Toolset build" means the compiler and linker builds** (WF1): FFmpeg's `build.sh` records `cl.exe`'s and
    `link.exe`'s banner versions as `msvcCompiler` and `msvcLinker` next to `msvcToolset`, which stays the
    `VCToolsVersion` folder, and `report`'s strict/warn split compares the toolset folder **and** the compiler and
    linker builds, for the rebuild and the lock alike, because VS servicing updates `cl.exe` in place under the same
    folder name (06 §3, §4.1, §4.3, §4.5, §4.6).
31. **The SFU's SRTP replay window is 4096 on publish PCs** (WA28): Pion's default of 64 would drop the native
    publisher's plain retransmissions (they reuse the original SRTP index) once 64 newer packets arrived, so most
    video repairs from the 4096-packet NACK responder would be lost (01 D7, §8.4, §10.5, §13, §14).

**Decided at group 3's merge and for group 4** (WB3 wrote each into the named sections)
32. **PCM leaves the engine as `media.KindPCM` packets through `Session.Next`**: with `AudioConfig.TapPCM`,
    `internal/media/native` returns every `IM_PACKET_PCM` as `media.Packet{Kind: media.KindPCM, CaptureNS, Data}`
    (3840 bytes) from `Next`, as 01 §5.1, §6.2 and 05 §13.2 already said and the fake does; there is no `PCMTap()`
    channel and no `PCMChunk` type, and the audiotest pulls Opus and tap packets with `Session.Next`. It settles the
    conflict group 3 left open (04 §12.2–§12.3 and WA23's code sent PCM to `PCMTap()`). 04 §12.1, §12.2, §12.3,
    §18.2, §18.3, §19, §20 and E5 drop `PCMTap`/`PCMChunk`; WA35 removes them from `bridge.go` and `types.go` in
    group 4 (its row).
33. **No uplink-hint counter and no dashboard cap in M2** (WA28-R5): 01 §10.1's metrics bullet asked for
    `isshoni_sfu_uplink_hints_total{reason}` to gain `congestion` and for the dashboard's per-share row to show the
    cap, but M1 has no such counter, a new `sfu.Metrics` field needs `wire.go`'s collector mapping (only WA56 and
    WA66 may touch `wire.go`, and neither is an SFU slice), and the dashboard lives in `internal/server`. Dropping
    both is the plan-consistent choice ("no other server change is planned", §3 "Who owns what"): 01 §10.1 and §10.2
    say so, and a later milestone adds the counter, its wiring and the cap in one slice.
34. **No `/DEPENDENTLOADFLAG`**: WA11's `dll_search_test` verdict from the group 2 PR's `windows-native` run is "not
    honored" (the default delay-load helper still loaded a planted DLL from the current directory with the flag), so
    the DLL is linked without it, 06 §5.1's conditional `dll-check -load-config` check stays out, and the engine's
    System32-only delay-load hook is the guard (04 §13.1, §18.2; 06 §5.1).

**Decided at group 4's merge and for group 5** (WB4 wrote each into the named sections)
35. **`im_start` with `EXCLUDE_APPS` after `IM_INIT_NO_AUDIO` is `IM_ERR_BAD_STATE`; the test tone still starts**
    (WA34-R1): 05 §4.3 said `mode ≠ OFF`, while WA23's session answers `IM_OK` there and runs the share without audio
    packets. Only `IM_AUDIO_MODE_EXCLUDE_APPS` is refused: the test tone (mode 100) is 04's own source and touches no
    audio API (04 §10.3), so video-only tools and CI runners without audio keep their A/V checks. WA52, the next slice
    that lists `native/windows/src/session/` for the audio start, adds the check and its test (its row); until then
    `audio_bridge_test` pins only what both readings share, that neither `SetRules` nor `NewMixer` is called (05
    §4.3).
36. **One C++ report writer**: WA35's `native/windows/src/audio/report.cpp` (the watcher landed first) and WA36's
    `im_audiomix` writer (05 A4) both write 05 §12.1's JSON byte for byte against the goldens in
    `internal/media/testdata/audio/`. WA45, which wires the mixer to the watcher, keeps one of them with its golden
    test and removes the other (its row; 05 §12.1); until then both run (`audio_report_test`, `mix_report_test`).
37. **The GPU test pattern in the session** (WA33-1): WA33 built the pattern in `gpu/` but couldn't wire it, because
    `src/session/` was outside its Touches. WF2 (group 5) wires gpu/'s `Device`, `FrameSlots`, `TestPattern` and one
    `Converter` per layer into `Session::StartVideo`, the supervisor's pattern branch, the video loop and the encode
    input, and removes WA23's CPU frame-time pattern (`frame_ts_`, `kFrameSlots`). `IM_ENC_FAKE` sessions stay off
    WARP, so `session_fake_test`'s timing cases keep their bounds. `test_pattern` and `pattern_layout` live in `gpu/`,
    not `capture/` (04 §4's tree, §6.1, §8.2, §18.2, E6).
38. **dll-check has no load-config check in any job**: WA11's verdict was "not honored" (item 34), so dll-check prints
    the load config and checks nothing unless `-load-config` is passed, and no job passes it; the flag and its check
    stay in dll-check, unused. WF2 replaces the printed note that called the verdict pending (06 §5.1, §16).

**Decided at group 5's merge and for group 6** (WB5 wrote each into the named sections)
39. **A paused starting share shows `paused`, and its start timeout waits for the resume** (WA46-R2, WF3): a paused
    publisher sends no frame (D14), so a share paused before its first keyframe reached the SFU, or republished while
    paused after a server restart (WA46 sends `paused: true` again for the new id), can't go live. WA27's hub kept its
    30 s start timeout and ended it with `media_timeout`, while `room.state` showed `starting` and the core
    `starting` or `reconnecting`. WF3 (merged in group 6, `internal/server/signal`) has the hub show such a share
    `paused` at once, suspend its start timeout while the pause bit is set, and give it a fresh 30 s start timeout from
    the resume; the core's reconcile already counts `paused` as live (01 §10.3, §11.7, §11.8, §14). As built, a first
    keyframe that arrives while the bit is set still makes the share live and sends `share.started` while `room.state`
    shows `paused`, and a paused starting share has no server-side bound while its connection lives (WB6 wrote both
    into 01 §10.3, and the core's side of it, WF3-3, into §11.8).
40. **The isshoni-self fixture tone moves at least 200 Hz from 1000 Hz and from every other fixture tone**
    (WA62a-R3): 05 §18.6's "at most about −64 dBFS" was wrong. A −18 dBFS 1000 Hz tone with the mixer's 5 ms linear
    onset or fade reads up to −44.3 dBFS at 907 Hz in a 50 ms window, over the −50 dBFS limit, so the absent checks of
    cases 4, 10 and 26 could fail on a correct engine (the other option was to leave windows that overlap a present
    tone's onset or cut out of the short-window maximum). WA62b picks the frequency and changes the analyzer's
    expectations, and un-skips WA62a's 5 ms onset subtest. 1250 Hz was the first choice, but it is 70 Hz from case
    27's 1320 Hz chime partial, so the tone lies at or below 240 Hz, in 1520–1800 Hz or at or above 2200 Hz (case 20's
    bursts are 2000 Hz). WA62b chose **1700 Hz** (`freqSelf`; WB6 named it in 05 §18.3, §18.5, §18.6, §19.1 and A7).
41. **PR #41's CI fixes that changed specs** (the group 4 PR's fix commits, merged into `m2/windows-app` with group 5)
    were written by those commits themselves: 01 §12.4 item 2 (`latency.spec` takes its legs less Chrome's lip-sync
    hold, which follows the viewer's audio output, and bounds the hold: p95 < 400 ms, and < 60 ms beyond what the audio
    path explains), 03 §14.2 and §20 (no JSON object in `settings.json` has more than 1024 keys; a write keeps every
    key the app knows and as many unknown ones as fit) and 01 §5.2's `media.Error.Redacted`. WB5 checked them against
    the merged `web/e2e/latency.spec.ts`, `internal/desktop/settings` and `internal/media`, and added two details of
    the spec to 01 §12.4: the 300-frame floor of a leg and the sign check on the capture leg as shown.
42. **Update error texts are `desktop.update.error.<code>`** (WA48): 03 §6.4 and §16.2 spelled `error`, 06 §8.10 and
    §18 `errors`. The code spells `error` (`bridge/codes`, `codes.gen.ts`, `platform/types.ts`,
    `codes.node.test.ts`), so 06 §8.10, §18 and 02 §13.3 now say it too; WA82 writes the texts with the update
    screens.

**Decided at group 6's merge and for group 7** (WB6 wrote each into the named sections)
43. **The core's rate tick runs on the actor's clock, and `EndInfo` carries the media reason** (WA54-R2, WA58-4; WF4):
    the share actor passed the rate ticker's channel value to `ratectl.Tick`, which since Go 1.23 is when the tick was
    due, not when the actor took it, while hints use `time.Now()`. A `Tick` taken late recorded its change up to that
    lag (0.54 s under `-race`) before the engine call, so the next change could reach the engine less than 10 s after
    the last one, and D10 asserted 9 s (`callSlack`). WF4 passes `time.Now()` (`case <-rate.C: s.rateTick(time.Now())`),
    so every instant ratectl gets is the actor's clock, and D10's slack goes, or shrinks to Windows' clock step (20 ms).
    And `core.EndInfo` had only the media code: `engineEnd` dropped the `*media.Error`'s `Reason`, so a live share that
    ended with `encoder_lost`/`device_removed` reached the page with an empty `params.reason` and 03 §10.5's "Your
    graphics driver restarted" row couldn't show. WF4 adds `EndInfo.MediaReason`, set for a live share's engine failure
    and a failed start's alike, and the share controller's `endError` returns `*media.Error{Code: Media, Reason:
    MediaReason}`. As built (WF4, merged in group 7; WB7 wrote it into 01 §9, §9.1, §9.6, §11.5, §11.8, §11.9, §14 D10
    and 03 §10.1, §10.5): the actor's rate timer is a `time.Timer` re-armed after each `Tick`'s engine calls
    (`rate.Reset(ratectl.TickEvery)` after `s.rateTick(time.Now())`), because with `time.Now()` on a ticker's fixed
    schedule two Ticks exactly 20 periods apart still passed a change 9.999895 s after the last one reached the engine;
    so D10 asserts the full `ReinitMinInterval` with no slack. An engine error that isn't a `*media.Error` (or has no
    `Code`) is `internal` without a token, and the "share ended" log line carries `media_reason`. PR #44 found the limit
    that remains: ratectl dates a change when it decides it, so a slow engine call shortens the spacing the engine sees
    by its duration (`TestRateTickClock` measures from the issue); a native call returns in microseconds, and a slow
    path would need an `Applied(layer, at)` feedback (01 §9).
44. **01 §10.1's loss row counts the loss once, and D9's recovery bound is 75 s** (WA54-R1 and the loss row WA54 found;
    WF5): `max(500 kbit/s, 0.8 × rate × (1 − loss/100))` counted the loss twice, since `rate` (the layers' receive
    rates at the SFU) already is what got through; the row is `max(500 kbit/s, 0.8 × rate)`. D9's way back to
    8 Mbit/s took 30–57 s under `-race` on the Mac with the tuned `RampEvery` (3 s, which D19 needs) and `nativeGrowth`
    (1.151), most after a backoff right at the lift, against a bound of 60 s; the bound is 75 s, not a change of
    `nativeUplink.backOff`'s logic. WF5 changes `uplink_native.go` and D9's `recoverWithin` (01 §10.1, §14 D9; WA54's
    row). As built (WF5, merged in group 7; WB7 wrote it into 01 §10.1 and §14): with the loss counted once, D9's
    settle bound and D10 no longer passed every run, so WF5's review fixes went further inside `internal/server/sfu`.
    The loss row backs off from what arrived in the ticker's **last second** (`max(500 kbit/s, 0.8 × arrived)`; the
    Layer's new `layerStats.arrived` in `layer.go`, carried as the unexported `LayerUplinkStats.arrived` in
    `uplink.go`), not from the 2 s average, whose backoffs came down to the policer's rate in steps that ended under the
    15 % hint threshold (WF5-R1); and the growth row needs `rate ≥ 0.75 × estimate` (`nativeGrowthRate`, was 0.85), so
    a re-init layer's step up to an estimate that stops growing is always 30 % or more (WF5-R2). D9 then passed 5 of 5
    under `-race` (back at 8 Mbit/s 29–54 s after the lift) and D10 and D19 3 of 3. What the estimator can't fix is the
    re-init guard's floor (item 47).
45. **The SPA holds a pick for 300 s, as Go does** (WA58-8): 03 §10.1, §20 and §22.1 hold a pick 300 s from the pick,
    04's pick-token life (04 §7.2), and WA58's controller does (`share.PickHold`), while WA58's row, 01 §16 and
    `bridge.ts`'s `Picked` comment said 120 s, the picker's own timeout mixed in. WA65 made the SPA follow 03 (group
    7: `bridge.ts`'s `Picked` says 300 s from the pick, and that the picker itself closes after 120 s); 01 §16 and
    WA58's row say 300 s (03 §10.1, §10.2).
46. **PR #42's CI fixes that changed specs or limits** (fix commits on `m2/windows-app` while groups 5 and 6 were on
    the PR) were written by those commits or by WB6: `latency.spec` bounds the share's A/V offset by its sender reports
    instead of the hold beyond the audio path (01 §12.4 item 2, written by that commit; WB6 checked it against
    `web/e2e/latency.spec.ts` and added the floor of half the intervals and that the hold beyond the audio path is
    only recorded); `TestServerRebuild` times the rebuild spacing from the first request, which the core can't act on
    earlier (§4's lessons); and `task test:go` runs each test binary of the main module with `-timeout 25m` (`tools/`
    keeps 15m), while CI's `test-go` job has 45 minutes (06 §10.1, §4).

**Decided at group 7's merge and for group 8** (WB7 wrote each into the named sections)
47. **ratectl's change floor doesn't block a move that a cap or the ceiling forces** (WF5-R3; WF6): the re-init guard
    (01 §9.4) refused every change under `ReinitMinChange` (30 %) that the admin's cap didn't force. Once the loss row
    counted the loss once (item 44), WF5's review saw a re-init `f` at 2.72 Mbit/s behind D10's 3 Mbit/s policer want
    1.98 Mbit/s (−27 %) and stay above the policer for the whole limit (3 of 4 runs); and a re-init `f` that was less
    than 30 % below its ceiling after the limit never stepped up to it (2 of 3 runs ended at 6.57 or 7.00 Mbit/s). D10
    asserts neither. AMF and MF users on a slow uplink would sit in steady loss. The floor applies to optional moves
    only: a re-init layer's decrease to a cap is always allowed, and its last step up to the ceiling is allowed when it
    is under 30 %; `ReinitMinInterval` still holds, so a layer still changes at most once per 10 s. WF6 changes the
    guard in `internal/client/ratectl` with unit tests for both, and D10 still passes (it changes D10's 30 % check only
    if a run hits such a move). Until WF6 merges, only the admin's cap passes a re-init step below 30 % (01 §9.4, §14's
    ratectl row and D10, C13; WF6's row). As built (WF6, merged in group 8; WB8 checked it against `ratectl.go`): a
    step up to exactly the ceiling passes whatever its size and encoder, and a re-init layer applied above its share
    of the cap comes down to it however small the step; a rise to a higher cap below the ceiling still waits for
    30 %. D10 now accepts a change under 30 % only when it is such a forced move (`forcedMove`, from the core's `rate
    action` log), and 01 §9.3's one-step ramp to the ceiling passes at any size (01 §9.3, §9.4, §14 D10).
48. **The server's reserved env list includes M2's dev and test names** (WA66's note; WF7): M2's tasks, tests and
    desktop code read `ISSHONI_*` names that 06 §7.3 doesn't list (`ISSHONI_DESKTOP_DEV_URL`, `ISSHONI_FAKE_AUDIO`,
    `ISSHONI_E2E_*`, `ISSHONI_NATIVE_*` and others), so a server started beside them (`task desktop:dev` next to `task
    dev` in one shell) warned about unknown env vars. They join `reservedEnv` like 06 §7.3's names, found with a grep
    of the repository and pinned by a test that lists them; an unknown name still warns. WF7 changes
    `internal/server/config` only; until it merges, the server warns about them. M1 04 §4.2's documented list, outside
    `docs/m2`, follows with the integrator, with WA66's other M1 04 rows (06 §7.3, §7.4; WF7's row). As built (WF7,
    merged in group 8; WB8 checked it against `keys.go`): 22 exact names, never prefixes, listed in 06 §7.3; and
    `TestRepoEnvNamesReserved` scans the repository's tracked files outside `docs/` and `spikes/` for `ISSHONI_*`
    names, so a slice that starts reading a new one adds it to `reservedEnv` and `devEnvNames` in the same PR, through
    the integrator (§4 "Config", §5's shared files), or takes another prefix for a name that lives only inside one
    task (WA24's `NATIVE_DIST_*`, item 54). M1 04 §4.2 still lists neither 06 §7.3's names nor these; the integrator
    owns that change.
49. **PR #43's and #44's CI fixes that changed specs or limits** (fix commits on `m2/windows-app` while group 7 was on
    the PRs), written by WB7: D10 checks each re-init's keyframe at the source (`engineTap` through
    `recoveringDesktopOn`'s wrap: the engine hands the core the layer's `EventLayer` and then a `Reinit` keyframe
    within 1 s of each bitrate call) and at the viewers by the keyframe's `CaptureNS`, with a change that no viewer
    watched logged (the SFU's downlink allocator moves a viewer of `f` to `q` under the policer) and one seen change
    of `f` required; a missed keyframe counts as lost at the policer only when the policer lost datagrams while it
    passed (01 §14 D10); `TestRateTickClock` measures a held engine call from its issue, which is the limit 01 §9 now
    states (item 43); the `e2e` job may take 45 minutes and every `apt-get` in the workflows has 30 s timeouts with
    three retries (06 §10.1); and the `DeviceLinkPage` tests wait for the focus that the page's effect moves (§4's
    lessons). `latency.spec`'s A/V-offset check (PR #42) still needs CI's baseline from a passing run's
    `latency.json` (01 §12.4 item 2, §8).

**Decided at groups 8 and 9's merges, with the owner on 2026-11-03, and for group 10** (WB8 wrote each into the named
sections)
50. **The FFmpeg publish is done** (the owner's action of §8): `ffmpeg.yml`'s publish was dispatched on 2026-11-03 at
    the owner's request from `main` at `7e3f7bb`; it created the prerelease `deps-ffmpeg-9.0.2-r1`, and its printed
    `lock.json` is committed on `m2/windows-app` (`avcodec-63.dll`, `avutil-61.dll`, toolset 14.44.35207, compiler
    19.44.35229, linker 14.44.35229.0). `ffmpeg-inputs -check` matches it, and `task ffmpeg:fetch` fetches the
    release's zip and checks it and its DLLs against the lock (with `SOURCE=true` the source archive too), on the Mac
    as well. So WA24 ran in group 9, and WA44 runs in group 10 (06 §4.1, §4.6, §14 item 2, R2; §5 "As run"; §8). The
    `deps-*` tag ruleset (06 §14 item 3) stays the owner's.
51. **WA09 ran on a GitHub runner, not on the owner's PC** (the owner's decision, 2026-11-03; PR #48):
    `.github/workflows/s1-audio.yml` builds S1's DLL with MSVC and `s1.exe` with Go on `windows-2025` (Windows Server
    2025, build 26100), starts the Windows Audio services and installs VB-CABLE Driver Pack 45 (VB-Audio's virtual
    cable, donationware) as the runner's output and microphone, then runs `s1 probe`, the full `s1 selftest` and the
    `-only late-start,late-start-endpoint` re-run. The driver is downloaded at run time from the vendor's host with a
    pinned SHA-256 checked before it is unpacked, lives only on the throwaway runner, and is never committed or
    shipped (its licence allows use as is, not bundling); the pin is in the workflow, not in
    `deploy/windows/tools.json`, which is the product's. Results (`spikes/s1-win-audio/README.md`): the include-set
    scenarios (include-set, exclude-one, helper-process, app-boundary, self, runtime-rule, mic-user) pass in both runs
    with excluded tones at −133 to −145 dB; the onset fade holds; run 2 has 8 PASS, 2 INFO and 1 FAIL,
    `endpoint-baseline`, because plain endpoint loopback of the cable stays digital silence on the runner even with
    one output and every role pinned to it, so the endpoint engine (Windows 10's path, WA53) isn't checked there, and
    the hosted runner doesn't replace the owner's PC for the gate. It covers Server 2025 with a virtual endpoint, not a
    real sound card, Windows 11 client or Windows 10 (05 §2, §3, §18.8, A0; the WA09 row; §8).
52. **Windows 10 22H2 is untested and takes the plan's gate** (the owner's decision, 2026-11-03): A0 had no Windows 10
    run, so builds below 22000 (`kMinIncludeSetBuild`) use endpoint loopback with the banner `exclusion-unavailable`
    (WA53 builds the check with the endpoint engine), and the Win10 half of the audio gate is 05 §18.10's reduced form:
    cases 1 and 19 pass and the banner shows; the exclusion checks don't apply there. A later Windows 10 run that
    passes can lift the gate, as an owner's decision (05 §1, §2, §3, §6.1, §15, §18.8, §18.10, A0, A6, §24; §1's exit
    criterion; §6; §8; the WA09, WA45, WA53 and WA62a rows).
53. **The tail fade joins the onset fade** (WA09's CI runs): Windows' per-app tap also cuts an app's **end**, on a
    10 ms packet boundary through its own fade-out, then sends digital silence; the running stream passed the step,
    and late-start clicked where game2 stops (6.13–6.14 s on the runner, 6.11 s in the Win11 PC's run 1). The spike's
    fix, which run 2 verified (late-start passes in both runs, one tail fade each): sound followed by ≥ 5 ms of
    digital silence has its last 5 ms ramped to 0, read ahead from audio already buffered, so 0 ms of added latency;
    counted (`tail_fades`) with an event at most once per second per channel. 05 §9.5 specifies it beside the onset
    fade, on the stream's input before the resampler; WA45 builds it into `native/common/audiomix` with unit tests on
    synthetic input; the report gains the counter `tailFades` and the event `tail_fade`, additive (05 §2, §3, §9.5,
    §9.6, §12.1, §19.1, A6, §24; WA45's row).
54. **WA24's deviations are accepted** (group 9): `windows-native` runs on `desktop` or `native`, so a
    `Taskfile.yml`-only change runs it too, since the job runs `task native:dist` and M1 counts each job's inputs;
    `native:dist` and `ffmpeg:fetch` pass their values through env names of their own (`NATIVE_DIST_*`,
    `FFMPEG_FETCH_*`), which take no `ISSHONI_` prefix because they live only in the task's processes (item 48);
    `release\` gets the FFmpeg DLLs and PDBs flat next to `isshoni-media.dll`, the installed layout, and FFmpeg's
    license folders under `release\licenses\`. Its review's fix also assembles `windows-native`'s `ci\` folder
    before the Go DLL tests and points `ISSHONI_TEST_ENGINE_DLL` at it, so WA44's engine finds FFmpeg next to the test
    DLL (06 §5.1, §10.1, §17, R3b; WA24's and WA44's rows).

**Decided at group 10's merge, for group 11** (WB9 wrote each into the named sections)
55. **WA44's open items went to WF10; its other deviations are accepted** (group 10's WA44 review): four items needed
    paths outside WA44's, so as WA44 merged the engine had the probe and the encoders but (a) the session's
    `EncoderCatalog()` listed only the test hooks' entries, so a release DLL had no real encoder and every hardware run
    waited; (b) `session::EncodedAu` had no capture time, so an encoder that holds a frame back carried the next frame's
    time; (c) `media.Config`/`native.StartExtra` had no per-layer GOP, so `capture -bitrate-steps` kept `f`'s 4 s GOP,
    tolerated a periodic IDR and listed the deviation in `report.json`; (d) the dead `probeFallback` stayed in
    `session_dll_test.go`. WF10 (group 11) did all four, as item 60 records. Accepted as built: the probe's encoder
    budget is 1500 ms with one 1280×720 retry after a failed 1080p open; a quick probe after a stored full one returns
    that probe with the OS flags re-read, and one before any full probe lists the build's encoders as `IM_OK` without
    `IM_ENCF_AVAILABLE` and no `audio_flags`, and that first quick probe can take longer than 04 §5.3's 100 ms; until
    WF12 (group 12, items 61 and 67) made the probe call WA50's `capture::OsFlags()`, its WGC flags came from the build
    number alone and `IM_OS_WGC_BORDERLESS` stayed clear (04 §9.10's "OS flags", now as built); the `ProbeFlags` check
    is in `ffmpeg_load_test.cpp` (with the probe it checks), not `audio_bridge_test.cpp`; the committed `annexb_test`
    fixtures are VideoToolbox streams made on the Mac, and the Windows encoders' folders come from the owner's PC with
    `capture -dump-aus 12`, which WF10 made possible; 04 §9.3's "Results for the spec" (layer 0's VBV included) wait for
    the same runs (04 §9.1, §9.2, §9.10, §14, §18.1–§18.3; the WA44 and WF10 rows; §8).
56. **The tail fade reaches the report through code, not by narrowing the docs** (group 10's open issue): item 53 and 05
    §9.5, §12.1 and §19.1 say the report gains the counter `tailFades` and the event `tail_fade`, but WA45's merged code
    keeps `tail_fades` in `MixCounters` and only logs the mixer's `kTailFade` event (the watcher's "tail fades in the
    share"). WF11 (group 11) added both to the one C++ writer with the Go types and goldens, so the spec text stands as
    written (item 60; WA45's and WF11's rows).
57. **`im_audiomix`'s writer is the one kept, and `instance_gone` is a report-level warning** (WA45; item 36 settled):
    WA45 removed WA35's interim writer, and `native/windows/src/audio/report.cpp` now maps the planner's vocabulary to
    `im_audiomix`'s model; `mix_report_test` is the golden test. The planner's `instance_gone` (05 §5.3) became
    `AppWarning::kInstanceGone`, written only in the top-level `warnings[]`; WF11 named it in
    `internal/media/audio.go`'s `AudioWarning` comment beside `multi_process_session`, the other top-level code
    (05 §12.1; WA45's and WF11's rows).
58. **M1 04 §4.2's reserved env list is written by WB9**: it lists every name of `internal/server/config`'s
    `reservedEnv` in its order, 06 §7.3's names, WF7's 22 and the two group 10's integration added for WA44
    (`ISSHONI_ENGINETEST_TEST_CHILD`, `ISSHONI_TEST_FFMPEG_DIR`). From now on the integrator updates that list with
    `keys.go` when a name is added (§4 "Config"; 06 §7.3; WB9's row).
59. **Group 11 as run**: WA50, WA53 and WA59 (which waited for WA44 and WA45), WF10, WF11 and WB9 (§5 "As run").

**Decided at group 11's merge and in PR #52's CI, with the owner on 2026-11-04, for group 12** (WB10 wrote each into
the named sections)
60. **WF10 and WF11 are built as items 55–57 asked, and their deviations are accepted**: the session's catalog lists
    `encode::CatalogEntries()` after the test hooks' entries (a release DLL: the probe's encoders only), so sessions
    open NVENC, QSV, AMF and MF; `EncodedAu.capture_ts_ns` and `session/au_times.h` give each packet its own frame's
    capture time and `IM_PKT_REFRESH` flag, at least one RTP tick past the layer's previous packet, and the latency
    is measured from it; `probeFallback` is gone (`TestDLLConformance` runs on the session engine, whose `win-x64-ci`
    probe lists `IM_ENC_FAKE` as tested); `probe.cpp` asserts at compile time that the encoder budget plus one open of
    at most 1 s and its IDR fit in `bound::kProbeFull` (no assert claims 04 §9.10's 2 s, which is the typical probe,
    not a bound). Accepted: the per-layer GOP is native-only, `native.StartWith`/`StartExtra` with
    `StartOption`s and `WithGOP(rid, d)` (plus `GOPOf` to read options back), and `media.Config` keeps no GOP field,
    since the app's sessions run the defaults and the fake would ignore one (`capture` needs `StartWith` because it may
    run with audio, which `StartExtra` refuses); `Session::StartFrames` creates a device only when the session's
    **first** candidate takes pixels, so fake sessions stay off WARP with real entries in the catalog; WF11's writer
    change is in `native/common/audiomix` (the kept writer; `audio/report.cpp` maps vocabulary and needed no change),
    and `internal/media/audio_test.go` gained `tailFades` in its sample (01 §6.4, §14; 04 §6.1, §8.1, §9.1, §9.2,
    §9.10, §12.2, §14, §18.2, §18.3; 05 §12.1; the WF10 and WF11 rows).
61. **WA50's deviations are accepted, and its session wiring and the probe's OS flags go to WF12** (group 11's WA50
    review): `MinUpdateInterval100ns` and the resize and minimize trackers live in the portable
    `capture/source_watch.h` so `window_match_test` checks them without Windows headers; `monitor_id` stays empty and
    a Windows 10 monitor pick reports WGC with the border until WA60 (04 §7.5); the hidden owner window is a 1×1
    transparent layered tool window shown without activation only while its pick is open; "`msedgewebview2`
    children" are WebView2 processes whose parent chain (at most 4 links) reaches this process; `pick_test.go` joins
    `pick.go` in `cmd/isshoni-enginetest`. Outside WA50's paths and so not done: nothing starts `capture::WgcCapture`
    (a real pick runs a session without frames, `IM_SESS_NO_OS_INDICATOR` is set unconditionally and no `SOURCE_*`
    event goes out), and `encode/probe.cpp`'s `BuildFlags` doesn't call `capture::OsFlags()` (no
    `IM_OS_WGC_BORDERLESS`). WF12 (group 12) does both, from WA50's hand-over (04 §7.2, §7.3, §9.10; the WA50 and
    WF12 rows). M-WGC-WIN, M-WGC-MON and M-SECONDARY waited for WF12, which merged in group 12 (item 67); M-PICK
    can run since the FFmpeg r2 lock was committed (items 64, 69; §8).
62. **WA53's deviations are accepted** (group 11's WA53 review; the slice's own review fixed its six findings): the fast
    path's exit skips the 20 ms debounce but runs the normal full refresh and reconcile, inside §7.3's budget but not
    yet measured on Windows; "process loopback stops working twice in 10 s" counts only system-wide failures (the
    EXCLUDE stream, every joined stream, or two or more targets in one pump; a target in its backoff never counts),
    ignores the 3 s after a default-device change, and caches `failed` for that endpoint until a re-probe passes; on a
    Windows 10 build the probe doesn't run, the report shows `unsupported` and `degraded` (Go's banner rules show
    `exclusion-unavailable` unchanged), and the sound check takes the endpoint path; the probe and the sound check
    measure the highest Goertzel level over sliding windows (160 ms and 100 ms) without lining up timestamps, and
    `levelDb` is the weaker partial; the probe opens its render client first, has a 600 ms deadline and is aborted
    without waiting when a share stops; the silent-stream check also watches the fast path's EXCLUDE stream (05 §6.4,
    §6.5, §7.5, §8.5, §11.2, §11.3, §12.1; the WA53 row). Its hardware gate is WA62a's cases on the owner's PC (§8).
63. **WA59's deviations are accepted, and 03 §13.4 and 06 §2.4 stand as written** (group 11's WA59 review): the
    review made `--purge-data` exit 0 again when the data root survives its retries (logged at ERROR), as both specs
    say, and `--forget-credentials` remove a data root that only its own save created; step 2a's probe is
    `mutexOpens` (not `winos.MutexExists`, which counts `ACCESS_DENIED` as present) and the installer mutex's name a
    constant in `start.go`; `--self-check` always opens the shipped engine (dev and e2e builds too) and its `spa.ok`
    also serves `GET /` with the bridge tag (03 §13.4; the WA59 row).
64. **FFmpeg r2 patches libvpl, and MF software's test encode is paced** (PR #52's CI and the owner's decision of
    2026-11-04): on windows-2025, `dll_search_test`'s libvpl part (in `ffmpeg_load_test`'s child) caught FFmpeg r1's
    dispatcher (libvpl v2.17.0, static in `avcodec`) loading the `libmfx64-gen.dll` and `libmfxhw64.dll` markers from a
    `PATH` entry: `MFXLoad` scans the executable's folder and `PATH` itself and loads by full path, past the process's
    DLL search settings, and libvpl has no build option for it. The owner decided to patch it:
    `native/ffmpeg/patches/libvpl-no-exe-dir-or-path.patch` (applied by `build.sh` with `patch -p1 --fuzz=0`, an input
    of `inputsSha256` and part of the source archive) removes the executable's folder, `PATH`, `ONEVPL_SEARCH_PATH` and
    `ONEVPL_PRIORITY_PATH` from the Windows search and keeps the driver stores, the dispatcher's registry keys and
    System32/SysWOW64. It closes the search to planted files, not to a host that controls the environment or HKCU. Since
    r1 was published, this is revision 2 (`deps-ffmpeg-9.0.2-r2`): `pins.json` said 2 while `lock.json` stayed r1's, so
    every lock consumer (`windows-native` first) failed by design until the owner published r2 and committed its lock,
    which happened on 2026-11-04 (item 69, §8). In the same PR, MF software's probe test encode sends the frames after
    the IDR one frame interval apart (`h264_mf` asks the synchronous MFT for output only inside a send;
    `encpolicy::TestEncodePlan`), the budget stays 3 more frames with no extra frame, and when none of the 4 brought an
    AU a drain only logs what the MFT still held (`probe: first AU only on drain`) before the test encode fails
    (`test_encode_failed`), since a session never drains; `mf_sw_test` skips only an `h264_mf` refusal inside MF
    software's own probe on Windows Server, and the vendor-runtime check opens the vendor encoders without a device
    where the runner's device has no video support (04 §9.10, §13.3, §17, §18.2; 06 §4.1, §4.3, §4.5; §5's pins table).
65. **Group 12 as run**: WF12 (WA50's session wiring and the probe's WGC flags), WA51, WA52, WA67, WA68, WA69 and
    WB10 (§5 "As run").

**Decided at group 12's merge, for group 13** (WB11 wrote each into the named sections)
66. **OpenH264 is pinned at 2.7.0 over HTTPS, and WA51's deviations are accepted** (group 12's WA51 review; its seven
    findings were fixed): 04 §9.9 asked for the newest Cisco release at WA51 and for HTTPS where the host offers it,
    so the pin is 2.7.0 from `https://ciscobinary.openh264.org/openh264-2.7.0-win64.dll.bz2`, 975 352 bytes
    decompressed, SHA-256 `0ef5e7f3b38753b15c13afa0418d4d7805fa228e6adfd3e425bbf758cff1c288`, in
    `src/encode/openh264_pin.h` and `native.OpenH264` alike (`TestOpenH264PinMatchesEngine`), and 2.6.0 over HTTP is
    superseded. The Go helpers' failures carry the Go-only Reasons `download` and `corrupt` (and `missing` from
    `VerifyOpenH264`) under `dependency`. Accepted: WA10's `open_test.go` no longer expects the OpenH264 helpers to be
    "not implemented"; `openh264_test` downloads Cisco's DLL itself (`tests/openh264_fetch.h`); Cisco's headers carry
    a `.clang-format` with `DisableFormat`; `CatalogEntries` and `StoredProbe` re-read the OpenH264 entry, so the
    setting applies at the next probe and `im_start` as 04 §5.3 says; `enginetest openh264 verify -engine` loads the
    file into the engine and runs a full probe; past OpenH264's 1080p30 cap a picture larger than 1920×1088 fails to
    open (the next candidate takes the layer) and a higher frame rate runs as 30 fps (04 §5.7, §9.1, §9.9, §12.5,
    §14; 06 §9.2's example and §12.3's privacy row; §5's pins table; the WA51 row).
67. **WF12 is built as item 61 asked, and its deviations are accepted** (group 12's WF12 review): a WGC pick starts
    `capture::WgcCapture` in the session, `IM_SESS_NO_OS_INDICATOR` is set only when it is borderless, and the
    probe's WGC flags come from `capture::OsFlags()`, so 04 §9.10's "OS flags" bullet and item 55's "until WF12" are
    history. Accepted: the seam `capture/capture_source.{h,cpp}` (`CaptureSource`, `CaptureFactory`,
    `StartSourceCapture`, `SetCaptureFactoryForTest`; `CaptureSink` and `CaptureOptions` moved there);
    `BorderlessAllowed(wait_ms)` and `OsFlags(borderless_wait_ms)` take a wait, 250 ms in a full probe (the encoders'
    1500 ms budget counts from the probe's start and a quick probe must stay fast), 0 in a quick probe after a stored
    one, 2 s in the picker, 0 at WGC's start (the pick's kept answer); `VideoFrames::PublishTime` for a capture without slots; the supervisor
    stops the capture as soon as the session ends, and draws no test pattern while a capture runs (04 §7.3, §9.10;
    the WF12 row).
68. **MF software on the CI runner gives its first AU only on drain, so the probe refuses it there** (PR #52's CI
    after the paced test encode, item 64): on `windows-2025` Microsoft's H.264 MFT through `h264_mf` delivers complete
    AUs only at end of stream, the diagnostic drain logs `probe: first AU only on drain`, and MF software's entry is
    `test_encode_failed`, as designed, because a session never drains. A likely cause, not proven: FFmpeg's `mfenc`
    sets `CODECAPI_AVLowLatencyMode` as `VT_UI4`, while Microsoft documents the property as `VT_BOOL`; a patch that
    sets it as `VT_BOOL` is a candidate for a later FFmpeg revision (06 §4.1's patch rule, a new publish), not part of
    r2. `mf_sw_test` skips on Windows Server only: for an MFT refusal (an `h264_mf` error line with the MFT's HRESULT
    inside MF software's own probe), and, since `ed2b4f0` on `m2/windows-app` (merged at group 13's integration),
    also when MF software's `probe: first AU only on drain` record shows complete drained AUs (`sps`, `pps` and `idr`
    all true); the skip prints that record. On a client Windows this outcome still fails the test, and the `VT_BOOL`
    patch stays a candidate. The owner's PC shows whether a client Windows behaves the same (04 §9.10, §17, §18.2;
    §8). WB11's review: `mf_sw_test.cpp`'s header comment named only the MFT refusal as a skip; group 13's integration
    reworded it to name both skips.
69. **FFmpeg r2 is published and locked** (the owner, 2026-11-04): `ffmpeg.yml`'s publish run from `m2/windows-app` at
    `957cce6` (the libvpl patch of item 64 and `build.sh`'s fallback to the published source archive when an upstream
    tarball download fails, 06 §4.3, since ffmpeg.org was unreachable from the runners) created `deps-ffmpeg-9.0.2-r2`,
    and its `lock.json` is committed as `b437d0e` (`inputsSha256` `27c3b458…`, `avcodec-63.dll` and `avutil-61.dll`, the
    same toolset build as r1), merged into `m2/windows-app` before group 12's slices. The stale-lock failures of item 64
    are over: `windows-native` and `windows-desktop` run with r2, and the owner's hardware runs that waited for the lock
    can run (04 §9.10, §14, §17; 06 §4.1, §4.6, §14; §5's pins table; §8).
70. **WA52 is built, with the test tone's mute and the audio-init log finished in the integration** (group 12's WA52
    review; WA52-1 and WA52-5 needed files outside its paths): `im_audio_set_muted` (`src/api/exports.cpp`) calls
    `session::SetToneMuted` beside `AudioEngine::SetMuted` and answers `IM_OK` when either took it, so it covers the
    test tone, also after `IM_INIT_NO_AUDIO`; `engine.cpp`'s "audio init failed" warning logs only the detail's
    token, with `session_log_test`'s `AFailedAudioInitLogsOnlyItsToken`; `im_start` with `EXCLUDE_APPS` after
    `IM_INIT_NO_AUDIO` answers `IM_ERR_BAD_STATE` (item 35). Accepted: the log test runs at info and at debug level;
    the video thread checks `im_set_paused`'s flag right before it takes a frame. Open for the binding's owner: that
    `im_start`'s `IM_ERR_BAD_STATE` maps to `internal` today, not `media.ErrAudioNotRunning` (05 §13.1's mapping covers
    the audio calls), and whether WA51's install and verify Reasons need bridge codes in 03 (04 §5.6, §8.5; 05 §4.3;
    the WA52 row).
71. **WA67's deviations are accepted** (group 12's WA67 review): the stricter strings rule; `Win\COM.nsh` and
    `Win\Propkey.nsh` for the AUMID; flags parsed by the scripts, `/SERVER=` ignored at `NSIS_MAX_STRLEN`; plain `/S`
    and a silent uninstall poll the running mutex for 60 s; `/PURGE`'s `RMDir /r` of a leftover data root; the LGPL
    files staged now and the notices without `-native` until WA78; the version ldflag found in the exe's bytes; the
    UTF-8 locale for makensis off Windows; `windows-desktop` in `ci-ok` and `smoke.ps1` cases 7 and 8. Open:
    `windows-native`'s release preset still computes the four-part version inline, and the `ci.yml` slice that next
    owns that part (WA72 in group 13, if its part includes it) switches it to `desktoppkg winver` (06 §5.1, §5.2,
    §6.1–§6.10; the WA67 row).
72. **WA68's deviations are accepted, and its leftovers go to WF13** (group 12's WA68 review and integration): `SetTray`
    is on `app.TrayShell`, and `IncludeInCapture`, `EndSessionHandled` and `OpenPath` are optional interfaces found by
    type assertion, so e2ehost's recorder keeps compiling; the W8 registry test runs under a random test id; the
    close-to-tray toast shows once per run until WA80's window notice sets `tips.trayNoticeShown`; the lock-pause
    state machine lives in `internal/desktop/app`; end of session quits the app; `-Embedding` waits up to 4 s for the
    activation (unverified until M-DESK-5). Group 12's integration mapped `app.ErrIO` to `io_error`, made the recorder
    a `TrayShell` and passed `-lock-resume`/`-lock-stop` into `app.Deps`. WF13 (group 13) does the rest: the SPA's
    `App.Heartbeat` (W17's watchdog), `lockResumeMs`/`lockStopMs` in `web/e2e/fixtures.ts` for `desktop-system.spec`'s
    long-lock cases, e2ehost's `end-session-confirmed` and the removal of toasts that no longer apply from Action
    Center (03 §6.1, §6.4, §11.6, §15; the WA68 and WF13 rows).
73. **WA69's deviations are accepted** (group 12's WA69 review): the constraint is `isshoni_hw && windows`; the 30 s
    are three 10 s windows of the core's rolling summary, each within both bounds; the share's audio is the test tone;
    measuring starts at 8 Mbit/s and 1080p, and a primary monitor below 1080p fails the test. It needs WA60's `mon:`
    ids, so until WA60 merges `TestCaptureToSend` fails with the engine's error on the owner's PC (01 §12.3; the WA69
    row).
74. **Group 13 as run**: WA60, WA61, WA71, WA72, WA73 and WA75, with one fix slice, WF13 (item 72), and WB11 (§5
    "As run"). WA60 also writes 04 §7.5, so WB11 leaves that section to it and the integrator merges 04.

**Decided at group 13's merge, for group 14** (WB12 wrote each into the named sections)
75. **The engine holds the MTA for the life of the process, and every Windows test prints its crash** (PR #53's CI
    fixes): every Windows test binary that called `im_probe` crashed on the engine worker at the first probe after an
    `im_shutdown` and a new `im_init`. C++/WinRT caches the activation factories of `GraphicsCaptureSession`,
    `ApiInformation` and `GraphicsCaptureAccess` (WF12's `capture::OsFlags`, new in the probe) for the life of the
    DLL; the worker's `CoUninitialize` ended the process's last apartment, COM unloaded their DLLs, and the next probe
    called through a dangling cache entry. `os::EnterWorkerThread` now also takes one `CoIncrementMTAUsage`, never
    released, and a failure of it fails the worker's start; `exports_test`'s `EveryInitCycleProbesLikeTheFirst` runs
    three init/probe/shutdown rounds. Every `tests/*_test.cpp` executable links `tests/support/crash_report.cpp` (the
    OBJECT library `im_test_crash_report`): a last-chance exception filter that prints the code, module+offset, an
    access violation's target and a symbolized stack, then chains to the CRT's filter, and makes stdout unbuffered,
    since gtest guards only the main thread's test bodies. In the same PR `smoke.ps1` passes its parameters to the
    functions that read them (PSScriptAnalyzer's unused-parameter rule) (04 §2, §6.1, §13.4, §18.2).
76. **`openh264_test` checks the bitrate step's lower bound only where the content can fill the target** (PR #53's
    last fix): CI measured 53.5 kbit/s after the step to 500 kbit/s. `test:pattern` needs about 55 kbit/s at 720p30,
    OpenH264's rate control caps rather than pads, and layer 0's 100 kbit/s floor keeps every legal target above that,
    so the upper bound (125 %) and the no-`REINIT`/`KEYFRAME` rule are always checked, and the lower bound (75 %) only
    when the second before the step already ran at 75 % of the new target or more, otherwise reported. M-OPENH264 on
    the owner's PC checks the lower bound with real moving content (04 §18.2, §18.4; §8).
77. **`audio_live_test` expects the engine that the rules and the probe imply** (PR #53): WA53 runs the fast path from
    the first plan (05 §6.4), so with `IM_EXCLF_FAST_PATH` and no other kept-out app a share is `exclude-self`, with or
    without an audio device, and the old expectation of `include-set` was wrong on every machine.
    `AMixerFeedsTheGridReportsLiveAndStops` turns the fast path off and expects `include-set`, or `endpoint` after a
    failed or unsupported probe, read from the settled report (no render endpoint, as on CI, means a skipped probe and
    still process loopback); `TheFastPathRunsFromTheFirstPlan` expects `exclude-self` from the start and after sound off
    and on, and skips with the reason where process loopback is unavailable or another kept-out app runs (05 §19.2).
78. **WA60 is built, with the session's half done in group 13's integration, and its deviations are accepted** (group
    13's WA60 review and integration): DD captures a Windows 10 monitor pick that maps and every listed `mon:` id.
    The integration did what WA60's paths didn't allow (its R1 a–e and R2): `SourceRegistry::Resolve` answers `mon:`
    ids with `capture::ResolveMonitor`; the session's sink sends `CAPTURE_INTERRUPTED`/`CAPTURE_RESUMED` with the
    cause; a DD source's session device is on its output's adapter (`capture::OutputAdapterLuid`,
    `encode::AdapterIndexOfLuid`); `im_session_info.capture_path` and the stats' path report the running capture; and
    `im_probe` clears `IM_OS_DESKTOP_DUPLICATION` with `dpi_unaware` when the process isn't per-monitor DPI aware.
    WA60's R3 was fixed in the slice: a re-creation failure that waiting won't heal fails the capture instead of
    leaving the session interrupted for ever. Accepted: the pointer is blended on the DD thread into the slot before
    it is published, not on the video thread as 04 §7.8 says (into the copy, never the desktop texture, still holds);
    `CaptureSink` gained `OnInterrupted`/`OnResumed` with empty defaults; 04 §7.5's `DisplayName()` table and verdict
    stay "pending (owner's PC)" until M-DD runs on the owner's Windows 10 install. Open: a 90°/270° monitor's first
    frame builds one VideoProcessor of the swapped size (a performance detail); WGC's source monitor is not reported
    as the capture adapter; the integration's `session/` and `encode/` changes have not yet been compiled by MSVC
    (`windows-native`). M-DD, M-WIN10-WINDOW and WA69's `TestCaptureToSend` (item 73) can run now (04 §6.1, §7.4,
    §7.5, §7.8; §8; the WA60 row).
79. **WA61 is built, without 04 §9.1's step 3** (group 13's WA61 review; its three findings were fixed: a helper's
    parsed report is kept whole when its exit code disagrees, and an `ENDED` session fails `sessions`, so
    M-NVENC-LIMIT shows every session working at the limit): one D3D device per session, on its first pixel
    candidate's adapter; a hardware encoder on another adapter is skipped (`open_failed`, `why` `other_adapter`), and
    `Candidate::cross_adapter_copy` stays unread. The capture adapter is a DD source's output adapter, else the
    primary monitor's. The cross-adapter copy and WGC's source monitor wait for a later `WF` slice (04 §9.1; the WA61
    row).
80. **WA71 is built; the app opens the approval page, and a bad verification URI is the link step's generic error**
    (group 13's WA71 review; R1 and R3 were fixed): `App.OnAuthState` (`internal/desktop/app`), not 02's `Manager`,
    opens `verificationUriComplete` in the browser once per new code of the active server (`openLinkPage`,
    `TestLinkPageOpened`). `Auth.Link` maps 02's `ErrBadResponse` (a verification URI that failed 02 §11.2's check) to
    `internal {reason: bad_response}`, which the link step shows as "Couldn't start linking with this server", as 03
    §8.2 asked, rather than §6.4's `server_unreachable` for other `ErrBadResponse`s. The confirm step of an add question
    whose server calls itself another host adds under the question's origin, so the pending link is answered (R1).
    Accepted: the server step keeps focus in the address field; the add step is remounted after an `AddServer` refusal;
    three test files outside its paths changed because WA48's `not_implemented` stubs went away
    (`web/src/app/router.test.tsx`, `web/src/i18n/check-i18n-lazy.node.test.ts`, `internal/desktop/e2ehost/main_test.go`,
    none weakening a product check). Open (R4): the desktop account menu of 03 §7.5 (Settings, Switch server,
    Account ↗, Admin ↗, "Sign out of <server>" through `Auth.Unlink`, never `POST /auth/logout`); the desktop app still
    does M1's cookie logout until WF14 (group 14) builds it (03 §6.4, §8.2; the WA71 and WF14 rows).
81. **WA72's review fixes are accepted** (group 13's WA72 review; its three findings were fixed): `windows-desktop` runs
    `go test -tags e2e ./cmd/isshoni-desktop/` and `lint-go` vets the app with `GOOS=windows -tags e2e`; the job also
    runs on the `native` filter, as `windows-native` does, since it runs `Taskfile.yml`'s bodies and now holds the
    DLL-dependent Go tests (item 54's rule); and the e2e build opens its credential store under a random
    `isshoni-test:<16 hex>:` prefix, in the start and the one-shot modes, and deletes that prefix's entries at exit, so an
    e2e run never touches the user's `isshoni:v1:` entries (`TestE2ECredentials`; 03 §22.4 B; the WA72 row).
82. **WA73 is built: the desktop itests use a bearer device** (group 13's WA73 review): `desktop()` and
    `recoveringDesktopOn()` link a device through the real `deviceauth.Manager` (`linkApp` over WA64's
    `linkedManager`), and `desktop_auth_test.go` adds D20 `TestDesktopForcedRefresh` (unauthenticated → one forced
    refresh → a new connection → republish) and D21 `TestDesktopDeviceRevoked` (revoked → the share ends, then
    `SessionEnded`, then `StopUnlinked` with the manager unlinked). 01 §14's "cookie auth until WA73" is history
    (01 §14; 02 §16.2's `linkedManager` note; the WA73 row).
83. **WA75 is built; the `audiotest` environment is the owner's to create, and the bundle's path waits for WA76**
    (group 13's WA75 review; R1 and R2 were fixed, R3 documented): the workflow's `environment` job (ubuntu,
    `contents: read`) fails the run unless the environment `audiotest` exists with the owner as a required reviewer,
    because GitHub creates an unprotected environment when a workflow names one that doesn't exist; when the default
    token may not read environments it reads the public repository's environment without a token, retrying four times
    15 s apart and naming a rate limit apart from other failures (neither path is tested against the live API yet:
    WA81's dry run or the first dispatch shows it). `run` needs `environment`'s success. The owner creates the
    environment before the first dispatch, with "Prevent self-review" off (§8). Open: the `hwcheck` suite starts
    `hwcheck.ps1` without arguments and looks for its bundle (a zip in the current folder, or a newer one next to the
    script); WA76 defines the script's output argument and the integrator passes it in `audiotest.yml` at WA76's merge.
    WF15 (group 14) makes repocheck's workflow tests run on a PR that only changes the workflows, recognize a
    self-hosted job by its registered labels or a runner group and not only by the literal `self-hosted`, and cover
    job- and workflow-level `env` token use (06 §10.4, §10.5, §14; the WA75 and WF15 rows).
84. **WF13 is built; the heartbeat has a gap while the window is hidden, and the app's toast removal is WF14's**
    (group 13's WF13 review and integration): the page beats at once, every 5 s and again when it becomes visible, so
    W17's watchdog arms. Open (R2, plausible, not reproduced on a Mac): Go treats a minimized or fully covered window as
    visible (Wails' `IsVisible` reads the `WS_VISIBLE` style bit) while Chromium throttles the hidden page's timers to
    about once a minute after 5 min, so during a full-screen game the watchdog may reload the page every 15–60 s; a
    Worker is ruled out by the CSP's `worker-src 'none'`, so the fix is the app's or the shell's (watch only a page that
    isn't minimized or occluded, or `--disable-background-timer-throttling`), in a later slice, checked by M-DESK-11's
    variant. 03 §20's cap of 3 reloads per 5 min and its error page are not built either. `winos.Hooks.RemoveToasts`
    removes toasts from Action Center (an untagged one by clearing and re-delivering the others, so callers serialize
    it with `Shell.Notify` and never hold `App.mu`), but the app doesn't call it yet (R3), so WF13's acceptance item
    for stale toasts stays failed until WF14 (group 14) wires it and `desktop-system.spec` expects the removed id. Fixed
    in the slice: R1, R4, R5, R6 (03 §11.6, §15, §20, §22.5; the WF13 and WF14 rows).
85. **Group 14 as run**: WA70 (which waited for WA60), WA76 (WA60, WA61), WA77 (WA71), WA78 (WA72), two fix slices,
    WF14 (items 80 and 84: the app's toast removal and the desktop account menu) and WF15 (item 83: the repocheck
    workflow guards), and WB12 (§5 "As run").

**Decided at group 14's merge, for group 15** (WB13 wrote each into the named sections)
86. **WF14 is built: the desktop account menu and the app's toast removal** (group 14's WF14 review; R1 fixed, R2 and R3
    handed on): the desktop menu is Settings, Switch server (more than one server in `Auth.Servers()`, read each time
    the menu opens), Account ↗, Invite friends, My invites ↗ (members with `createInvites`; opened in the system browser
    like Account and Admin, because `/admin/invites` is behind `RequireBrowser`), Admin ↗, Test my connection, About and
    "Sign out of <server>" (asks first, then `Auth.Unlink()`, never `POST /auth/logout`; an unconfirmed unlink leaves a
    toast that stays), with no Notifications; the browser's menu is unchanged. **Switch server goes to Settings →
    Servers** (`/desktop/settings#servers`, the Offline screen's target too), not to an embedded `ServerSwitcher`, which
    would pull the `desktop` chunk and its lazy namespace into the main bundle; so WA82's `#servers` section must render
    `desktop/ServerSwitcher` (Switch, Sign out, Remove, Add), with a component test that two servers show Switch; until
    then the link lands on a settings page that has only WA77's Audio section. Accepted: the texts are new `room.menu.*`
    keys in `en.json` (the menu is in the main bundle, where `check:i18n` forbids `desktop.*`); `AccountMenu.test.tsx`
    is new. The toast removal: `toastBook.dropKind` keeps the dropped toasts' ids, `removeStaleToasts` removes them once
    `App.mu` is released through the optional `toastRemover` (`winos.Hooks.RemoveToasts`, kept in step by compile-time
    checks), serialized with `Shell.Notify`, and `notify` removes stale toasts before it shows a new one; group 14's
    merge resolved `toasts.go` with WA77 (a pending toast keeps both its id and its ref, and `dropRefsExcept` records
    dropped mic-prompt toasts as stale) (03 §7.5, §15; the WF14 and WA82 rows).
87. **WA77 is built, and its deviations are accepted** (group 14's WA77 review; all six findings were fixed): `main.go`
    passes 05's `audio.Service` as `app.Deps.Audio` and `bridge.Deps.Audio`. **`wizardDone`**: no spec said how the
    voice step's Done sets it, so `Settings.Update` applies `{wizardDone: true}` alone before WA84 (`false` is
    `invalid_argument`, any other field `not_implemented`), and Go's `bridge.SettingsPatch` and `bridge.ts`'s carry
    the field (the latter added in group 14's integration); the sound and voice steps follow only the link of a
    server just added (`?link=1`) while `wizardDone` is false, so relinking skips them. **The mic prompt**: the Wails
    shell reports `WindowFocused` (shown, not minimized, focused), so a prompt becomes a toast while the window is
    visible but not in front (WA77-1); the card shows at the top of the panel and in both sheets; the corner card
    (`MicPromptCorner`, shown while no panel or sheet hosts the prompts) is built but not yet mounted: mounting it once
    for the whole desktop app is open for group 15's integration. `App.ToastDismissed` (a dismissed toast →
    `DismissPrompt`) is built and tested without a caller: the pinned Wails reports no toast dismissal, so a
    swiped-away toast leaves the prompt pending, still kept out, until the next Wails pin. **The endpoint mute** is
    attached from `ViewerLayout` (`useEndpointMute`), so it holds only while the room's viewer is mounted; it belongs
    in `web/src/rooms/runtime.ts` (`attachEndpointMute` is exported for the move), for a later slice that owns that
    file. Settings → Audio reads the rules again only when a report changes a decision or the mic toggle. Accepted
    changes outside its paths: `web/e2e/desktop-link.spec.ts` (the real sound and voice steps), `router.test.tsx` and
    `boot.test.tsx` (`SettingsPage` is no longer the stub; it shows only the Audio section until WA82), the
    regenerated bindings and new `desktop.*` keys, and `internal/desktop/bridge/settings.go` (WA84's file, for
    `wizardDone`). Open: `SOUND_HELP`'s "Learn more" points at `troubleshooting#desktop-sound`, an anchor that WA85
    adds to `docs/troubleshooting.md` (03 §6.2, §8.2, §10.3, §12.1, §15; the WA77, WA80 and WA85 rows).
88. **WA70 is built; `protect-check` judges presence by bit rate, and WGC that can't be reached passes as
    `not_reachable`** (group 14's WA70 review; R1–R4 were fixed, R5 is this item): `im_protect_window` needs a
    top-level window of this process for both flags (`GetAncestor(GA_ROOT)`; a child window is
    `IM_ERR_INVALID_ARG`), which 04 §11 now says. `protect-check` reads no pixels (the shipped DLL has no luma tap and
    the tool no decoder): it judges presence by what the noise window costs layer `f`, its non-keyframe slice bit
    rate by capture time (unprotected ≥ 500 kbit/s, protected ≤ 20 % of that), with a still desktop behind the window;
    `protect_window_test` makes the real pixel check for raw DD. A listed `mon:` id always runs DD, so 04 §14's "WGC
    (listed source)" can't happen: WGC is reached only through a monitor pick; on Windows 10 a pick maps to DD and on
    Windows 11 a failed WGC start falls back to DD, and the verdict follows the running path
    (`native.CapturePath`), so WGC is then `not_reachable` and the run passes (exit 0) on DD alone. **Whether that is
    acceptable for the "Win10 for WGC" acceptance is the owner's** (§7 question 11). A picked monitor other than the
    noise window's and an interrupted run exit 2. One session per path, unprotected half-way by the tool's own
    `SetWindowDisplayAffinity(WDA_NONE)` (no `UnprotectWindow` in the binding). `hwcheck.ps1` runs it as
    `protect-check -wgc off`, since the default `-wgc pick` waits on the OS picker; the WGC half is its own
    interactive step on the owner's PC (group 14's integration). Accepted: the subcommand is `protect.go` plus
    `protect_windows.go` and `protect_other.go` (tests in `protect_test.go`), and `internal/media/native` lost
    `open_test.go`'s `TestNotImplementedYet` and gained `protect_test.go` and `protect_dll_windows_test.go` (04 §11,
    §14, §18.2; the WA70 row).
89. **WA76 is built, without the luma tap, which goes to WF16** (group 14's WA76 review; R1, R2, R4 and R6 were fixed,
    R3 and R5 handed on): `avsync`'s luma tap is declared and answers a "not implemented" dependency error (exit 2),
    because no session emits `IM_PACKET_DEBUG`, the binding drops it and 04 specified no payload; **WF16** (group 15)
    builds the `IM_TEST_HOOKS` producer in `session/`, its payload in 04, a `StartOption` in `internal/media/native`
    that passes the packets on, and `avsync`'s use of it, and the "first `avsync -dll ci` run on Win11" item stays
    blocked until then (`avsync -no-luma` checks the Windows half meanwhile, never a pass). Accepted: `bench` reports
    encode p95, so `hwcheck.ps1` checks p95 ≤ 16 ms (necessary for 04 §18.4's p99, not proof) and capture-to-packet
    p95 ≤ 25 ms with a hardware `f`; `hwcheck.ps1` captures only with available hardware encoders and adds `-dump-aus
    12`; `soak` measures growth from the first sample after the warm-up to the last and defaults to `-audio tone`;
    `hwcheck.ps1 -Out DIR`, which group 14's integration passes from `audiotest.yml` (the zip-next-to-the-script
    fallback is gone, item 83). Its leftover check runs on the text without the redaction tokens, and the micro sign
    and Greek mu fold to `u` before a case-insensitive hash literal (04 §14; the WA76, WF16 and WA79 rows).
90. **PR #53's CI lessons** (the Windows jobs of PR #53, after group 14's merge; each cost a round): (a) **the engine
    links no umbrella library**: WA50's `#pragma comment(lib, "windowsapp.lib")` bound the static CRT's and
    C++/WinRT's functions to unlisted `api-ms-win-core-*` sets and failed `dll-check`; C++/WinRT links
    `runtimeobject.lib`, `im_engine` passes `/NODEFAULTLIB` for `windowsapp`, `onecore`, `onecoreuap` and `mincore`,
    and `GetDpiForMonitor` is resolved at run time from System32's `shcore.dll` (96 dpi without it) (04 §5.6, §8.1,
    §13.1; 06 §5.1); the MTA held for the process (item 75) is the same family. (b) **Engine threads**: a WGC start
    uses the pick's kept borderless answer, DD creates its owner before its thread, a late picker thread is told to
    quit, `RunOnMta` carries an exception's detail (04 §7.3, §8.1). (c) **Toasts**: `ToastNotificationHistory.Remove`
    takes only the calling app's toasts, so the process must carry the app's AppUserModelID (the Windows test makes
    its random id the process's), a toast delivered again keeps its group or gets `keepGroup`, and a fresh id's
    `NotificationSetting` is `ERROR_NOT_FOUND` until its first toast (03 §15). (d) **makensis on Windows splits a
    `File` path only at `\`**, so every source joins `${STAGE}` with `\` (06 §6.1). (e) **One round reports every
    independent failure and every skip**: `windows-desktop` gates each check on its own prerequisites, `windows-go`
    runs the `CGO_ENABLED=0` tests after a failed `-race` run (30 minutes) and both list their skips, native tests
    write gtest XML and `windows-native` lists every skipped case (known ones as notices, others as warnings), the
    `-tags e2e` step requires its three PASS lines, and the CDP results upload from `web/test-results/cdp/` (06
    §10.1). (f) Smaller ones: `smoke.ps1` wraps a function's result in `@()` before `.Count` (StrictMode 3); the
    audio scenario fixtures don't read the host's ducking preference (`SelfInfo.NoDucking`); a test that waits for
    two events accepts them in either order (`TestRepublishNewConnection`; `nextEvent` drops what it skips);
    `install-desktop.ps1`'s stderr check ignores a progress-only CLIXML block; Task's shell on Windows has no `/tmp`
    and its `mktemp` takes no folder in the template (§4 "Testing").
91. **The shell relaunch starts the child on the shell's desktop and watches it** (PR #53; WA77's relaunch): the
    relaunched process takes the shell's token, and with `lpDesktop` unset it opened the starting process's window
    station and desktop and died with `STATUS_DLL_INIT_FAILED` (0xC0000142) before `main`, so it now names
    `winsta0\default`; `startUnderShell` refuses a shell in another session or of another user, and watches the child
    for 1.5 s: a nonzero exit is `errRelaunchDied` and the start goes on in place. An elevated start relaunches
    de-elevated (a refusal tried in the PR rested on a wrong reading of the runner and was removed). On GitHub's hosted
    image the child dies with 0xC0000142 even on `winsta0\default`, with Explorer in the test's session and of its user
    (a shell elsewhere is refused before any relaunch; item 100), so there `TestRelaunchUnderShell` checks the in-place
    fallback (exit 3 with `errRelaunchDied`) and skips the explorer-parent half, which runs on the owner's PC (03 §13.4,
    §22.2).
92. **Wails checklist at beta.34: W5 fails and its fallback is built; W7 holds** (PR #53): Wails registers no
    `NewWindowRequested` handler and drops `NavigationStarting`'s arguments, so Go can cancel neither; 03 §5.5's
    layer 2 is built: `bridge.js`'s Navigation API `navigate` listener, the HTML's header CSP `sandbox allow-scripts
    allow-same-origin allow-forms` (no `allow-popups`), and Go's `navguard.go`, which sends a window off the asset
    origin home with `SetURL("/")` (5 s for an answer, at most 3 times a minute); `Shell.Reload` and W17's watchdog
    load the start page when the window was last seen elsewhere. W7 holds: after `WM_CLOSE` the main HWND exists and
    is hidden, the page reports `hidden` and the process lives; the CDP spec attaches with `connectOverCDP(…,
    { noDefaults: true })`, since Playwright's focus emulation would keep `visibilityState` `visible` (03 §3.2, §5.2,
    §5.3, §5.5, §22.4; WA87 copies the rows).
93. **WA78 and WF15 are built** (group 14's reviews): WA78's `desktoppkg stage` refuses notices without the "3. Native
    components" section, so group 14's integration gave `desktop:package:windows`'s notices call `-native
    ../deploy/notices/native-windows.json -native-root "$native"` (WA78-1); accepted: `licenseFiles` paths resolve
    against `-native-root`'s `licenses/` (a `../` path is a repository file), LicenseRef exceptions get a second
    table in `exceptions.md` keyed by the entry's name, 06 §9.3's `ffmpeg-config.json` check stays in
    `ffmpeg-fetch` and `stage`, and the `desktop` filter counts `deploy/notices/exceptions.md` and
    `deploy/notices/licenses/**`. WF15 counts a job as self-hosted by the literal, a runner group, a label registered
    in `.github/actionlint.yaml` or one of GitHub's default self-hosted labels (`self-hosted`, `linux`, `windows`,
    `macos`, `x64`, `arm`, `arm64`, case-insensitive: no hosted runner answers to a bare `windows`), and the no-token
    rule matches case-insensitively and catches `toJSON(secrets)`, `secrets['K']`, `github['token']` and
    `github_token`; accepted: a repocheck step in `lint-deploy` instead of the `go` filter (06 §9, §10.1, §10.4; the
    WA78 and WF15 rows).
94. **Group 15 as run**: WA80 (share safety, which waited for WA70 and WA77), WA81 (the release pipeline, WA78), WA82
    (the settings page, WA77), one fix slice, WF16 (item 89: `avsync`'s luma tap), and WB13 (§5 "As run"). WA79
    (the exit run book, which needs WA76) is not in this group; its `avsync` step names the tap WF16 delivers.

**Decided at group 15's merge, for group 16** (WB14 wrote each into the named sections)
95. **WA80 is built: share safety, with the pill's state as built** (group 15's WA80 review; R1–R5 were fixed, R3's
    wiring by the integration, item 96, and R6 is this item's e2e note): the hotkeys are registered from live to the
    end of the share (the Pause hotkey only when the server has `share.pause`); Pause comes from the panel, the
    hotkey, the tray, the pill and the lock (§11.6) through one path. **`app.PillState`** gains `Positions`
    (`pill.positions`), `PauseUnavailable` and `Text` (`PillText {Line, Pause, Resume, Stop, PauseUnavailable,
    Questions, Collapsed}`, the catalog's words), so the Windows pill reads neither the settings nor the catalog;
    **`Monitor`** is the share's source (`share.View.Monitor`: `media.Source.MonitorID` without `mon:` for a monitor
    share, `""` for a window share, which goes on the primary monitor; WA80-R1, which first left it `""` always);
    actions gain **`open`** (`PillOpen`: a click on the amber marker shows the window); the optional
    **`app.PillShowing`** (`Shown()`, which `winos`' pill implements: false while hidden and after Windows refused the
    window or its affinity) decides the pause toast's "no pill shown" (R4); the collapsed pill's window name is
    `Text.Collapsed` (R5); **the disabled Pause's accessible name** is "Pause needs a newer server" (its window text;
    it still draws "Pause"), not a separate tooltip control. The pill's tests send `WM_COMMAND`/`BN_CLICKED`, not
    `BM_CLICK`, which can need the foreground. **In the SPA panel Pause is disabled only after the server answered
    `feature_disabled`**: the SPA can't see the server's features, while the tray and the pill can. **The window
    notice**: the page pulls `App.WindowNotice()` at mount and whenever it becomes visible, besides the `window.notice`
    that `App.ShowWindow` emits, so a window opened by a link, a second launch or a server change shows a due notice
    too (R2), and reports it with `App.WindowNoticeShown`, which sets `tips.trayNoticeShown`. **degrade-live**: the
    degrade makes the prompting app heard, so 05 drops its prompt; `desktop-share.spec` checks the question and the
    marker live before the degrade and the alert after it, `TestAudioAlertOutsideWindow` checks them together, and "a
    second degrade within 60 s records none" is 05's unit test (the fake degrades once per share; 05 resets its alert
    memory per share). The mic prompt's toast while the window is hidden stays `TestMicPromptToast`'s (the harness's
    window never hides). Accepted: the new `desktop.native.pill.liveAlert` and `.collapsed` keys and
    `en.json`'s `viewer.state.paused`; `desktop-system.spec`'s long-lock case checks the pill's state (the
    shared-files table lets WA80 extend it). Handed on: the panel's lock-prompt bar (`ShareView.lockPrompt`) and
    `desktop-share.spec`'s close-to-tray step (an e2ehost hook for `WindowClosing`) to WF17; `Settings.Hotkeys` over
    `app.App.Hotkeys()` to WA84 (03 §6.2, §6.3, §10.3, §11.3, §11.4, §11.6, §22.2, §22.4; the WA80, WF17 and WA84 rows).
96. **Group 15's integration wired WA80's notice and the corner card, and ended the `update.*` exemption**:
    `bridge.Core` and the bound `App` service gain `WindowNotice()` and `WindowNoticeShown(code)` over `app.App`'s
    (`app.ErrNoNotice` → `not_found`), `window.notice` (`app.EventWindowNotice`) joins `bridge.Events`, now **13
    events**, and the bindings were regenerated; `bridge.ts` gains the `WindowNotice` type, the `window.notice` entry of
    `BridgeEvents` and `NativeBridge.app`'s `windowNotice`/`windowNoticeShown`. **The app shell** (`App.tsx`) mounts
    `WindowNotice` (under the required-update banner) and WA77's `MicPromptCorner` once for every desktop page, lazily
    through `desktop/shell.ts` (which re-exports them from `share/native`, so the browser fetches neither); the share
    panel no longer mounts its own notice (items 87, 95). **`check-i18n.mjs` rule 6** requires
    `desktop.update.error.<code>` for each `update.<code>` (WA82-R2; WA82's run had neither file in its paths), with
    `check-i18n-codes.node.test.ts` and `codes.node.test.ts` (item 42). The Windows preflight renamed
    `luma_sample_test.cpp`'s `small` (a `<windows.h>` macro) and clang-formatted `luma_sample.h` (03 §6.2, §6.3, §10.3,
    §11.6, §18.1).
97. **WA82 is built, and its deviations are accepted** (group 15's WA82 review; R1 and R4 were fixed, R2 is item 96's,
    R3 goes to WF17): every settings section and update state of 03 §14.4 and §16.2 with a component test; the
    Sharing section lists the usable encoders (or `ShareCaps.reason`) without 03 §10.5's per-encoder notes, which
    `ShareCaps` can't carry yet; the SPA's `/licenses.txt` opens in a dialog read from the app's files (the app never
    navigates away, 03 §5.5); the desktop `VersionMismatch` says **Update the app** (the acceptance's text) and has
    Try again (`Auth.Recheck()`); `openH264Control()` answers `null`, hiding the opt-in, until WA84 binds the OpenH264
    methods; new `desktop.settings.*`, `desktop.update.*`, `desktop.version.*` and `desktop.servers.linkAgain` keys.
    The committed axe case for `/desktop/settings` (light and dark, the hotkey recorder and the licenses dialog open)
    is WF17's in `web/e2e/a11y.spec.ts`; the required-update banner and the desktop version screen join it after WA84,
    since `Updates.*` answer `not_implemented` until then (03 §14.4, §16.3; the WA82, WA84 and WF17 rows).
98. **WF16 is built: the luma tap, with its start flag outside the public header** (group 15's WF16 review; R1–R4 were
    fixed before the merge): 04 §5.8 is the tap as built (the 48-byte payload, the centered `min(256, side)` box,
    `missed`, the 64-reading ring with `DISCONTINUITY`, Go's `WithLumaTap`, `LumaTapOf` and `LumaSample`), and 04
    §12.1 and §12.2 list `luma.go` and its API (R3). **`IM_START_TEST_LUMA_TAP` (0x80000000) stays out of
    `isshoni_media.h`**: the header changes only through the integrator, so the bit lives in `luma::kStartFlag` and
    `luma.go`'s `startTestLumaTap`, which `TestLumaConstantsMatchTheEngine` keeps equal and clear of every
    `IM_START_*`; adding it to the public ABI later takes `abi.go`'s mirror and `abi_test.go` in the same change.
    Release builds refuse the bit (`IM_ERR_INVALID_ARG`) like `IM_ENC_FAKE`, and the Go binding refuses the option
    with `dependency` when the DLL's build info says `testHooks: false`. `avsync` centers its flash window on the
    captured monitor for `-source mon:<id>` (R1) and discards an edge after a lost reading (`edgesDiscarded`, R2). The
    C++ was not compiled with MSVC as group 15 merged: the group PR's first `windows-native` run checks it (04 §5.8,
    §12.1, §12.2, §14; the WF16, WF17 and WA79 rows).
99. **WA81 is built, with two edits outside its listed paths** (group 15's WA81 review; four findings were fixed, and
    WA81-3 is this item): **`deploy/test/windows/smoke.ps1 -Mode release`** is WA81's (the file says so; WA67 owns the
    rest and WA88 its `-Mode update`): it checks a release build as a user gets it, with `-Setup` a fresh silent
    install (`desktop-pkg`) and without it the install the released `install-desktop.ps1` made (`verify-windows`),
    against `-Stage`, then `--version`, `--self-check` and `uninstall.exe /S /PURGE`. **`release:snapshot`'s body**
    writes the desktop stubs through the new **`release:desktop-stubs`** (`ISSHONI-DESKTOP-STUB` files for this
    version unless its real installer is there, earlier stubs removed) and exports **`ISSHONI_DESKTOP_MIN_VERSION`**
    and **`ISSHONI_FFMPEG_VERSION`** (from `deploy/desktop/min-version` and `native/ffmpeg/pins.json`) for the release
    notes' desktop lines, which goreleaser's `.Env` needs; it reads them with `sed`, so a snapshot needs no `jq`
    (WA81-5), and fails with a clear message when either is missing; `release.yml`'s `build` job exports the same two.
    `release:assets` gains **`DESKTOP_STUBS=true`** (a `FULL=true` run that accepts stubs, for `nightly.yml`'s
    `goreleaser-full` until it has real desktop files) and `PKG_PS1` (the packaging job's stamped
    `install-desktop.ps1`); `DESKTOP=true` needs `jq`, `unzip` and `7z` and says so. `windows-native` also uploads
    **`ffmpeg-source`** (the locked source tar, so `desktop-pkg` doesn't download it again) and `desktop-pkg` downloads
    **`web-dist`** (the notices need `web/dist/licenses.txt`); `verify-windows` runs `isshoni-audiotest selfcheck` from
    `isshoni-exit-tools` (06 R13, WA81-1); the `desktop` filter counts the server's script rendering and serving paths
    (WA81-4); the notes header puts the archive patterns in backticks. **Open**: `nightly.yml`'s `goreleaser-full`
    calls goreleaser directly, so it gets no stubs and `release:assets FULL=true` fails there until WA86 runs
    `release:desktop-stubs`, passes `DESKTOP_STUBS=true` and exports the two variables (WA81-2); the 7z listing of
    NSIS files, `isshoni serve` on an AF_UNIX socket on `windows-2025` and syft 1.54.1's `--source-name`/
    `--source-version` are settled only by CI and the dry run, which stays the owner's step (§8) (the WA81 and WA86
    rows).
100. **WB13's leftovers, corrected by WB14** (group 15's WB13 review, R1–R3 left in the text): the hosted runner's
     0xC0000142 child dies with Explorer **in the same session as the test and of the same user**:
     `relaunch_windows_test.go` reaches that branch only after `shellIsOurs` accepted the shell, and a shell elsewhere
     takes the refusal branch, so "whatever the desktop, session or elevation" (item 91 and 03 §22.2) was wrong. 04
     §11's `im_protect_window` logging follows `protect.cpp`: only a refused `SetWindowDisplayAffinity` logs a WARN with
     the Win32 code, a failed `GetWindowDisplayAffinity` logs nothing, an affinity that isn't kept logs a WARN with
     `off`, `want` and `got`, and the build check is skipped when the build is unknown (0). Prose stays wrapped at 120
     columns (03 §13.4, §22.2; 04 §11, §14).
101. **Group 16 as run**: WA79 (the exit run book, which waited for WA76 and, for its `avsync` step, WF16), WA83
     (handoff and the web entries, which waited for WA80), WA84 (the settings services and updates, WA80 and WA82),
     WA85 (the site, WA81), WA86 (the nightly, WA81), one fix slice, WF17 (group 15's follow-ups: the lock-prompt bar,
     the close-to-tray e2e step, the settings axe case, and what is left of `avsync`'s monitor and missed-reading
     report), and WB14 (§5 "As run"). WA87 (WA80, WA82, WA83, WA84), WA88 (WA84, WA86) and WA89 (WA84) come after
     them, then WA90 and WA91.

**Decided at group 16's merge, for group 17** (WB15 wrote each into the named sections)
102. **WA79 is built: the exit run book, `docs/m2/exit-test/`** (group 16's WA79 review; R1–R5 were fixed): the book
     (`README.md`) and the record's template (`results-template.md`) write out §6 with the exact commands, and §6 now
     follows them. **Windows 10 follows 05 §18.10, not §6's old first sentence**: 05 replaces audio items 1 and 2 as a
     whole on Windows 10 22H2, so there `run -gate`'s exit code is recorded, not judged (the reduced form: cases 1 and
     19 PASS and the `exclusion-unavailable` banner shows), and **no R row is required on Windows 10** (R3 runs on
     Win11 only, and R5's other half isn't asked for either, whatever 05 §18.9's OS column lists). `avsync` and the
     committed report stay on both. **The record's layout**: `docs/m2/exit-test/results/<date>.md` with its JSON
     files in `results/<date>/` (the six `g2g-<win11|win10>-<run>.json` and `avsync-<win11|win10>.json`); the two
     audiotest reports stay under `cmd/isshoni-audiotest/results/`. **A `protect-check` row** joins the
     release-candidate checks (§6 C didn't name it, but it is where a WGC `not_reachable` gets recorded: "WGC not
     covered on this PC", never a WGC pass; question 11 stays open). **The rerun rule** (R1): every new rc repeats
     `run -gate` and `avsync` on both OSes (the DLL carries the rc's version and commit, so an older report's
     `dll.sha256` never equals the new `native-manifest.json`) and all of B; the real-app checks R1–R5 and R7 and the
     banner repeat only after a change to `native/`, `internal/media/` or `internal/desktop/audio`, and a
     carried-over manual check names its rc. `-dll` is `isshoni-enginetest`'s global flag (`-dll ci avsync`), and
     `-source` takes the id as `sources` prints it (`mon:\\.\DISPLAYn`, R2); the record's L-2 conditions are read
     from each gate JSON's `screen`, `refreshHz` and `rttMs`, never pre-filled (R3); M-DA-7's offline repeat keeps the
     device on the Devices page (02 §16.4, R4); `selfcheck` is a precondition, not rule A-1 (R5). 06 §11.8 items 7–11
     are WA91's, recorded in the release-prep PR (§6, 05 §18.10; the WA79 and WA90 rows).
103. **WA83 is built: the handoff and the web entries, with four parts left to WF18** (group 16's WA83 review; R5–R8
     were fixed, R1–R4 are WF18's): `rooms/handoff.ts`, attached by the room runtime, keeps `user.connections`, sends
     `App.OwnConnections` from the desktop page, sends `playback.stop` (`toRole: full`) after a `join()` that asked for
     a room (`RoomSession`'s new `'join'` event, never for the welcome's re-join, R6), and in tabs follows a stop whose
     sender the snapshot lists as the user's own desktop connection, checking a not-yet-listed sender again with the
     snapshots of the next 3 s (`HANDOFF_VERIFY_MS`); the handed-off tab's subscriptions are all off (`layerPolicy`
     rule 9; `desktop-handoff.spec` asserts video and audio, R8) and it keeps its own share's preview tile (R7). Each
     tab sends `playback.state` on every change of audible playback, to a desktop connection that appears, to all of
     them after a welcome, and **once more 3 s after a desktop connection appears** (`PLAYBACK_STATE_REPEAT_MS`): a
     stopgap for Go's race (R3: `App.followPlayback` subscribes to C2's events only after `Ready`, so a report decoded
     right after C2's welcome is dropped; without the repeat the spec failed 2 runs in 3). WA77's endpoint mute moved
     from `ViewerLayout` into `rooms/runtime.ts` (item 87's hand-over). **"Open in the isshoni app"** (`OpenInApp.tsx`)
     is in the browser room page's header, not yet beside "Get the desktop app"; its "Nothing happened?" line links
     `/download?room=<roomId>` (`downloadPath`). **The SPA's deep links** (`rooms/DesktopLinks.tsx`, lazily mounted by
     `InRoomBar` because `app/App.tsx` was outside the paths; texts `room.deepLink.*` in `en.json`) act on Go's
     `deeplink.open` and `App.PendingLink()` per 03 §13.2, and pull `PendingLink()` again whenever the page becomes
     ready (R5). **Left to WF18**: ShareSheet's and ScreenAudioWarning's `/download?room=`, the Windows-only rule
     (`share.sheet.desktopAppWindowsOnly`) and `OpenInApp` beside the link (R1); `SwitchServer` keeping a confirmed
     `{switch}` link's room pending (R2); Go subscribing before `Ready`, so the repeat goes (R3); the `too_many_servers`
     link cleared once shown (R4) (03 §12.1, §12.2, §12.3, §13.2; the WA83 and WF18 rows).
104. **WA84 is built: the settings services, autostart and updates in the app** (group 16's WA84 review; R1, R3–R5,
     R7 were fixed, R2 is item 108's CI step, R6 is WF18's, R8 is this item): `Settings.Update` takes every field of
     03 §14.2's patch (autostart goes to the Run value first, so a value Windows refused is never saved), then
     `app.App.SettingsChanged(prev)` applies the capture exclusion, a running share's hotkeys, the pill, the log level
     and, for `updates.check` or `updates.channel`, an immediate `CheckNow` (off publishes `off`: the ready offer and
     the tray's item go, R1). `Settings.Hotkeys` maps `app.App.Hotkeys()`; `TestHotkey` is bound. **The OpenH264
     opt-in's methods** are `Settings.OpenH264()`, `EnableOpenH264()` and `DisableOpenH264()` → `OpenH264Status`
     (`{state: unavailable|off|on, version}`; `NativeBridge.settings`' `openH264`, `enableOpenH264`, `disableOpenH264`);
     `DisableOpenH264` answers `busy` while a share is live (its encoder may hold the DLL). `Updates.*` work over the
     app's updater; **"Restart and update" and a share's start share a gate**: `Updates.Install` is `busy` while a share
     is live or starting, and `Share.Start` is `busy` while an install runs (`app.App.Shares()` hands out the gated
     controller; R4). **The start** (03 §13.4): step 5 opens `settings.json` and the updater for step 7
     (`earlyParts`), writes the newest link (this start's own or the newest a second launch forwarded) to the Pending
     values, and takes them back when `Apply` fails, so the link reaches the app once (R5); `--update-now` exists only
     in `updatetest` builds and uses 06's default options, not `settings.json`; `TestInstallerHandover` runs with a
     pending update (R3). **Autostart**: an `--autostart` start makes the tray but no main window until one is asked
     for (`newDeferredShell`; `Reload` has nothing to reload then), and at `Run`'s start `settings.json`'s `autostart`
     follows the Run value (R7); the non-Windows `winos` stub answers `ErrNotSupported`. **`settings.changed`** goes out
     only after `Settings.Update`; the app's own writes (`tips.firstShareShown`, `tips.trayNoticeShown`, pill
     positions) emit none until WF18 adds `settings.Store.Subscribe` (R6). **Outside the listed paths, accepted**
     (R8): `internal/desktop/bridge/bridge.go` (`Deps.Updates`, `Deps.OpenH264`, the `update.state` subscription, the
     `Settings` fields), `internal/desktop/app/share.go` (`Shares()` gated), `internal/desktop/winos/oshooks_other.go`
     and `winos_other_test.go` (03 §6.2, §6.3, §13.4, §16.1, §16.2; the WA84 row, §5's shared files).
105. **WA85 is built; the site serves `update_signers` and does not check the update manifest** (group 16's WA85
     review; WA85-1 to 3 were fixed, 4 and 5 by item 108): `site.yml` copies main's `deploy/keys/update_signers` to
     `/keys/update_signers` (checked after the build against main's file, like `allowed_signers`), fills
     `release.json`'s `desktop {windowsInstaller, sha256}` from the verified `checksums.txt` (`null` before 0.2.0; a
     0.2.0 or later release without the installer fails the build) and `updateKeyFingerprints`, and fails when the
     latest release has the app but main has no `update_signers`. **The manifest's signature is checked by
     `check-draft.sh` before a release is published (06 §11.3) and by every app before it trusts one (06 §8), never by
     the site**: the site neither serves nor uses the manifest, so checking it there protects nothing the site shows,
     and it would break the build whenever the owner drops a key from `update_signers` ahead of the release signed with
     the next one (a rotation, or a leak, when the site's security text matters most; WA85-1). `TestDocsAnchors` also
     scans the product's and the site's links for anchors (web/src, deploy's ps1, nsi and sh, `.goreleaser.yaml`, the
     pages). `#windows-verify` says what to do without the OpenSSH Client (WA85-2); `#windows-smartscreen`'s
     screenshots are owed by the owner with the first release (WA91; a comment in `docs/download.md` marks the spot,
     WA85-3). `/security#keys` says a rotated-out key is removed (06 §7.2 bans `valid-before`) (06 §13; §8).
106. **WA86 is built: the nightly as it runs** (group 16's WA86 review; WA86-1 was adopted, WA86-2 is item 108's):
     `windows-full` is two jobs, `windows-full-native` (ci.yml's `windows-native`, no caches, 60 min) and
     `windows-full` (ci.yml's `windows-desktop` without caches plus release.yml `desktop-pkg`'s release files, 75 min,
     output `desktop-files`), with a `web` job for `web/dist`, all at version `0.0.0-nightly.<run>`; their artifacts
     are kept 1 day. **`goreleaser-full`** runs after `windows-full` (`if: !cancelled()`): with its real files
     `release:assets FULL=true DESKTOP=true`; on a night without them it still checks the server half over
     `release:desktop-stubs` with `FULL=true DESKTOP_STUBS=true` and then fails (06 §10.1's one-round rule; item 99's
     open point is settled this way). **`compat-servers`** starts each release with the tag's own
     `deploy/dev/isshoni.e2e.toml` (the checkout's when the tag lacks it). `windows-asan` (45 min), `native-fuzz` (20
     min, crashes kept 30 days) and the cross-OS legs are as 06 §10.3. The updater e2e is a placeholder step ("The
     updater e2e (README WA88)") that WA88 replaces. **Still unverified until the first nightly** (its
     `goreleaser-full` with real desktop files runs both): WA81's 7z listing of NSIS files in
     `release:assets DESKTOP=true` and syft 1.54.1's `--source-name`/`--source-version` for the desktop SBOM; item
     99's third point, `isshoni serve` on an AF_UNIX socket on `windows-2025`, stays the owner's dry run's (06 §10.3;
     the WA86 and WA88 rows).
107. **WF17 is built, with four paths outside its listed ones** (group 16's WF17 review; R1–R4 were fixed or are this
     item): `ShareView.lockPrompt` is the share controller's `View.LockPrompt` (`Controller.SetLockPrompt`, shown only
     while paused), set by the app from `App.sys.prompt` on every tray flush under a mutex of its own, and dropped by
     the controller itself when a share starts and when the view is live, ended or failed (R1). The panel's status line
     takes the lock-prompt state ("Sharing is still paused since you locked your PC." with **Resume sharing** and
     Stop) instead of a second bar, and is always `role="status"` (R2). `e2ehost` gains
     `POST /__e2e/window?action=close|open` (`App.WindowClosing`, `App.ShowWindow`); `desktop-share.spec` has the
     close-to-tray step and `a11y.spec` the `/desktop/settings` case. `avsync` places its flash window with
     `flashWindowPos` (tested on synthetic monitor rects) and its report counts `lumaMissed` and `lumaRingDrops`.
     **Accepted outside the listed paths** (R3): `web/src/platform/desktop/bridge.ts` and `testing/shareView.ts` (the
     hand-written `ShareView` and its fixture), `web/src/i18n/lazy/desktop.en.json` (`desktop.share.panel.lockPrompt`,
     `.resumeSharing`) and `web/e2e/desktop-system.spec.ts`'s long-lock case (03 §6.3, §11.6, §22.4; 04 §5.8, §14;
     the WF17 row).
108. **Group 16's integration** reserved `ISSHONI_REQUIRE_AUTOSTART_E2E` (WA84's autostart test reads it; 06 §7.3);
     added **the autostart round trip** to ci.yml's `windows-desktop` and nightly's `windows-full`, after the
     `-tags e2e` tests and gated on steps `go`, `webdist` and `webview2`: `CGO_ENABLED=0`,
     `ISSHONI_REQUIRE_AUTOSTART_E2E=1` (a missing prerequisite fails instead of skipping),
     `go test -run '^TestAutostartRoundTrip$' ./cmd/isshoni-desktop/`, and a `--- PASS` line required (WA84-R2; it
     fails if an isshoni instance still holds the running mutex, which a smoke left behind would); added
     `web/src/**/*.ts`, `web/src/**/*.tsx` and `.goreleaser.yaml` to ci.yml's **site filter**, which
     `TestDocsAnchors`' product-links scan reads (`deploy/**` already runs the site job through the deploy filter;
     WA85-4); fixed the comment above `SOUND_HELP` (WA85-5) and `Taskfile.yml`'s comments on `release:desktop-stubs`
     and `release:assets` to describe item 106's `goreleaser-full` (WA86-2) (06 §7.3, §10.1, §10.3).
109. **Group 17 as run**: WA87 (the real-app e2e and the manual matrix, which waited for WA83 and WA84), WA88 (the
     updater e2e, which waited for WA84 and WA86), one fix slice, WF18 (group 16's follow-ups: the playback
     subscription before `Ready`, the confirmed `{switch}` link's room, the `too_many_servers` link, `settings.changed`
     after every change, the download link with the room and the Windows-only rule, the update banner's and version
     screen's axe cases; items 103–104), and WB15 (§5 "As run"). WA89 (the owner's M-DA runs) can run once WA84's app
     is installed on the owner's PC; WA90 and WA91 come last. §7 question 11 stays open.

**Re-cut in this plan**
- 04 E1 splits into the header and native skeleton (WA03) and the binding's pure Go half, which joins E2's Go loader
  (WA10). E2's C++ and 06 R3a are one slice (WA11), as 06 asks. 04 E15 splits into its code (WA76) and the exit run,
  which is one measurement with 01 C19 (WA90).
- 03 DK1's engine files move to DK3 (WA41), because the binding and the fake are built beside DK1. DK4 splits into the
  platform (WA32) and the app's routes and boot (WA42); its `WailsBridge`, generated bindings and drift check move to
  DK5 (WA48), which declares the services they are generated from. DK7 splits into Go (WA58) and SPA (WA65). DK12
  splits into the process contract's one-shot modes that the installer needs (WA59), the SPA settings and version
  screens (WA82) and the Go settings, autostart and updater wiring (WA84). 05 A9 is DK8 (WA77), as both specs say, and
  DK11's Go receiver (`EventPlaybackState` → `SetViewerBrowser`) moves into it.
- 05 A6 splits into the include set and mixer (WA45) and the fast path, endpoint engine, probe and sound check (WA53);
  its `AudioTestHooks` Go wrappers move to A5 (WA35), so A6 touches no Go.
- 05 A7 splits into the harness core with every case that needs no microphone (WA62a, group 6, beside WA53, so the
  real capture code gets its hardware run right after group 6 instead of in group 7) and the mic cases, `soak`,
  `multi-endpoint`, `record`/`merge`, redaction and the README (WA62b, group 7).
- Results a spec records (06 §4.6, 03 §3.2, 04 §9.3) go into the measuring slice's PR first (§5, shared files).
- Review additions placed here: 03 §13.4's first action (the DLL search order) in WA41 and its step 2a (the installer
  handover) in WA59; the outside-the-window audio alerts (05 §14.7; 03 §11.1, §11.4, §11.6, §15) in WA80;
  `enginetest avsync` (04 E15, 05 §19.3) in WA76.
- 02 DA1 (WA05) also declares `signal.Identity.DeviceName`, filled by DA6 (WA56), so 01 C08 (WA27) needn't wait.
- 06 R10's Windows run moves into R5 (WA72), which also runs the audiotest `selfcheck`; R14's runner README moves to
  R15 (WA75). 06 R18's signing jobs are later (after SignPath approves); M2 ends with the application (WA91).
- The specs themselves moved 01 C03–C04 into 04 (E1, E2, E5); 03 DK2 is unused (the origin is 02's).

**Carried from M1** (`docs/m1/README.md` §3): the packet type leaves `internal/media/fake` (WA02), and the four
device-flow methods of M1 03 §7.13 land with their DTOs (WA05, WA30, WA39). F07 (pub-PC tracks under SSRC churn) stays
the owner's; if it lands during M2, it merges first in the group that touches `internal/server/sfu` (WA28 in group 3,
WA54 in group 6). M2's first release assumes `v0.1.0` (M1's exit) is out (06 §11.8).

## 4. Conventions

M1's conventions (`docs/m1/README.md` §4: errors, context and shutdown, logging, config, testing, naming, interfaces
first, branches and PRs) apply unchanged. This section lists only what M2 adds or changes.

### Errors
- Engine failures are `*media.Error{Code, Reason, Op}` (01 §5.2). `Reason` carries the C ABI's fixed detail token
  (04 §5.7), which is also the first word of `im_last_error`. Only adapters translate: `internal/media/native` maps
  `im_status` to `media.Error`, the core maps errors to `StopReason` and `EventNotice`, and `internal/desktop/bridge`
  maps Go errors to the bridge codes of 03 §6.4 (`bridge/codes`, generated into `codes.gen.ts`).
- No English crosses the C ABI or the binding to users: the SPA maps codes and tokens to `desktop.errors.<code>`;
  `im_last_error` text is for logs only. C ABI status codes and tokens are never renamed or reused.
- C++: no exception or C++ type crosses the ABI; every export and thread entry is `noexcept` and turns exceptions
  into `IM_ERR_INTERNAL` (04 §13.4).
- Exit codes are per program: the app's one-shot modes (03 §13.4), the installer (06 §6.4), `isshoni-enginetest`
  (0, 1, 2, 64), `isshoni-audiotest` (0–4), `g2g` (0, 1, 2).

### Context, threads and shutdown
- M1's Go rules apply. On Windows, `runtime.LockOSThread` is used only on the `winos` thread and around a DLL call
  whose error is read on the same thread (04 §12.4). COM, WinRT and DXGI work runs only on engine threads (an MTA
  worker; the picker on its own STA), never on a goroutine and never on the Wails UI thread (04 §6.1, 03 §4.3).
- One goroutine per session calls `Session.Next` (01 D5); native code never calls into Go. Bounds: `Next` honors its
  context within 50 ms, `Session.Stop` ≤ 200 ms, `core.Close` ≤ 3 s, Quit stops a live share first (01 §13, 03 W14).

### Logging
- Go code logs through `internal/logx`; the app writes rotating files through `applog` (03 §17), and the binding
  drains the engine's log ring (`im_read_logs`) into slog under the same rules.
- Never logged, at any level, in addition to M1's list: window titles, share labels, source names, app and exe names,
  audio rules and levels, device and user codes, access and refresh tokens, and the password of the link form
  (`protocol.Secret`). At no level, debug included, do Go or the engine's log ring name an executable, process or app
  (§3 item 18; 04 E14, 05 §16).
- Committed reports (the `hwcheck` bundle, audiotest results, `g2g` JSON, the exit record) carry no user, host,
  machine or device names, paths or IPs: the repository is public.

### Config, settings and build tags
- The server gains no TOML key in M2 (06 D13; the device flow uses constants and M1's `minClientVersion`). The env
  names of 06 §7.3 join M1's reserved list (WA66), and so do M2's dev and test names (WF7, §3 item 48).
- **A new `ISSHONI_*` name** that the repository starts to read (a task, a test, a script, the app) goes onto
  `reservedEnv` and `keys_test.go`'s `devEnvNames` in the same PR, or `internal/server/config`'s
  `TestRepoEnvNamesReserved` fails; `keys.go` is the integrator's (§5's shared files), so a slice that needs one says
  so in its PR and the integrator adds it, or lists the file in its Touches, and M1 04 §4.2's documented list gains
  it in the same change or the next write-back (§3 item 58). A name that lives only inside one task's own processes
  takes another prefix instead (`NATIVE_DIST_*`, `FFMPEG_FETCH_*`; §3 item 54; 06 §7.3).
- The app has no config file or env knobs in release builds: `settings.json` schema 1 (03 §14.2) and `servers.json`
  (02 §11.6) under `%LOCALAPPDATA%\isshoni\`, the registry values of 06 §2.3, and the refresh token only in Credential
  Manager. Release builds read no environment variable for paths (04 §13.3).
- Build tags: `production` (the only tags a release has), `dev` and `e2e` (fake engine, never shipped), `updatetest`,
  `isshoni_hw` (owner's-PC tests). The CMake option `IM_TEST_HOOKS` is on only in the test presets `win-x64-ci` and
  `win-x64-asan` (04 §13.1), never in a shipped build. 06's release check
  fails a release build that links `internal/media/fake` or test hooks.

### Testing
- M1's rules apply (`task test:go` with `-race`, goleak, golden files, fuzzers 30–60 s per PR, `testing/synctest`).
- Every OS-specific Go package builds and tests on linux, darwin and windows, with stubs elsewhere; `lint-go` also runs
  golangci-lint with `GOOS=windows` and package builds of `internal/client/...` and `internal/media` for `android` and
  `ios` (01 §4). DLL-dependent Go tests skip unless `ISSHONI_TEST_ENGINE_DLL` is set.
- Native code: GoogleTest; `task native:test` (preset `host-debug`) on the Mac; `host-debug` and `host-tsan` in
  `native-host` (ubuntu-24.04, macos-15); `win-x64-ci` ctest on windows-2025 with WARP, fakes and `IM_ENC_FAKE`;
  nightly `host-fuzz` and `win-x64-asan`; `clang-format --dry-run --Werror`.
- E2E: M1's browser e2e, the desktop-in-Chrome harness (`internal/desktop/e2ehost`, `DESKTOP_E2E_BIN`) in the same
  `e2e` job, and the real `isshoni.exe -tags e2e` over CDP on windows-2025 (03 §22.4).
- The fake engine is the engine of every test that doesn't need hardware; the app's code runs on the Mac with it.
- **Windows lessons from groups 1–4** (each cost a CI round or more): checkouts on `windows-go`, `windows-native` and
  the owner's PC are CRLF unless `.gitattributes` says otherwise, so a test that compares a checked-in file byte for
  byte needs a `text eol=lf` line for it; Windows' `time.Now` advances in timer ticks of up to 15.6 ms, so a test never
  orders events by comparing `time.Now()` values (a sequence counter, as `mediatest.Conformance`'s `rec.seq`, 01 §6.4)
  and never asserts a duration shorter than a tick; a test that reads what a fake server received right after a call
  returns waits for it to arrive (the core's sends are queued: `fakeServer.settledTypes`, 01 §14); and Credential
  Manager lost one of eight concurrent writes on windows-2025, so `credstore` makes one call at a time (02 §11.5). Group
  4 added two: a test that loops must have a timeout (a Mac harness of group 4 spun for days in a loop that never
  ended), and a sanitizer build's shortened run must still reach every timed step (`mix_alloc_test` removed its second
  stream after minute 6 of a 2-minute host-tsan run until group 4's integration made it 60 % of the run). PR #41's CI
  and group 5 added: Windows headers define macros named `small`, `near`, `far` and `interface`, so code that
  includes them never uses those as identifiers; CI's gcc `-Wshadow` and clang-format 18 differ from the Mac's clang
  and clang-format 17 (no bare `{ lock_guard }` blocks); a test that compares a built binary's version with the test
  binary's builds it with `-buildvcs=false`; CI's coverage builds run pure-Go hot loops about 10 times slower; the
  Go coverage runtime names its temporary meta-data file by the clock alone, so parallel `testscript` children each
  need a `GOCOVERDIR` of their own (`cmd/isshoni`'s `TestScripts`); and itest timing tests flake on a loaded runner,
  so they assert what the acceptance requires and no more. Group 6 and PR #42 added: a test that times a spacing
  measures it from the request the code can't act on earlier, never from a later observation (`TestServerRebuild`
  timed from the first rebuilt PC's offer, one ICE gathering later, and read 494 ms for a 600 ms spacing on
  `windows-go`); a `time.Ticker`'s channel value is when the tick was due, not when it was taken, so code that mixes it
  with `time.Now()` values runs on two clocks (§3 item 43); and the timeouts follow the slowest package:
  `internal/server/itest` takes about 12 minutes under `-race` with group 6's desktop cases, so `test:go` runs each
  test binary of the main module with `-timeout 25m` (`tools/` keeps 15m) and CI's `test-go` job has 45 minutes
  (06 §10.1), and new itest cases keep their wall time lean. Group 7 and PRs #43–#44 added: React effects run after
  the commit, so a test that checks focus or effect-driven state waits for it (`waitFor`; `findByRole` resolves at the
  commit that shows the new heading, before the effect moves the focus); an itest that checks a property at the
  viewers through the SFU allows for the downlink allocator moving a viewer to another layer under loss, and checks
  the property at its source first (D10's `engineTap`, §3 item 49); a timed spacing that the code under test issues
  is measured from the issue, not from a slow call's return (`TestRateTickClock`); `internal/server/itest` took 716–732
  s under `-race` on the Mac, inside the 25 m; and the e2e job's apt mirror can be slow without stalling (25 minutes
  once), so that job has 45 minutes too (06 §10.1). Groups 8 and 9 added: a test fixture never carries a fixed date
  that will pass (WF9: the logout test's invite had `expiresAt` 2026-10-15, so it now derives it from the test
  clock), and a test that waits for a page waits for what tells it apart (WF9: the invite page's checking state has
  a heading with the same name as the form's, so `findByRole` returned it just before the form replaced it; the test
  waits for the heading and the Username field together); and timing tests flake under load on the Mac too:
  `internal/server/itest` (`TestDesktopUplinkCongestion` at 2.28 % viewer loss against 2 %, and `TestLayers`), the
  e2e specs `layers.spec` (the thumbnail's `frameHeight`) and `pwa.spec`'s offline navigation, and the Vitest
  `runConnTest` "runs the three probes in parallel" (`rttMs` 20 instead of 0) each failed once while other heavy
  checks ran and passed alone, so the integrator runs itest alone; `runConnTest`'s flake is still open. Group 10
  added: a frame captured before a request may still be queued in the session when the reader pulls it, so a check
  that counts frames after a request counts those captured after it (`mediatest.Conformance` check 4, 01 §6.4, after
  a slow `desktop-macos` runner); `DeviceLinkPage`'s three remaining focus checks wait for the effect, like group 7's;
  and `internal/server/itest` took about 700 s under `-race` on the Mac, past `go test`'s default 10 minutes, so it is
  run with `-timeout 25m` as `task test:go` runs it. Groups 13–14 and PR #53 added (§3 items 75, 90–92; each a class to
  check across a whole diff before it goes to the Windows jobs): MSVC's `/W4 /WX` (C4996's CRT calls, C4127, narrowing)
  and gosec's G115 (`golangci-lint`, also with `GOOS=windows`, on every integer conversion) fail what the Mac's builds
  accept, so a slice runs the pinned linters itself; never link an umbrella library (`windowsapp`, `onecore`): C++/WinRT
  uses `runtimeobject.lib`, and an API whose import would add an API set is resolved at run time; the engine holds the
  MTA for the process; a crash on a thread other than gtest's main one ends a test binary silently unless
  `tests/support/crash_report.cpp` prints it; ctest counts an all-skipped binary as passed, so the skips are listed
  (gtest XML, `go test -v`); the `windows-2025` runner has no GPU (Basic Render) and, in `windows-native`, no audio
  device, its job is elevated (UAC off, one full admin token) with an elevated Explorer in the job's own session, a
  process relaunched under that Explorer dies with 0xC0000142, and toasts need the process's AppUserModelID; PowerShell
  under `Set-StrictMode -Version 3` needs `@()` around a function's result before `.Count`, PSScriptAnalyzer fails an
  unused parameter, scripts stay ASCII and 5.1-compatible; makensis on Windows splits paths only at `\`; Task's shell on
  Windows has no `/tmp` and its `mktemp` takes no folder; host state (the registry's ducking preference) must not leak
  into a fixture; Playwright over CDP needs `connectOverCDP(…, { noDefaults: true })` to see `visibilityState`; Wails
  beta.34 can't cancel a navigation or a new window (03 §3.2 W5's fallback) and its `Reload()` reloads the page showing
  (`SetURL("/")` goes home); and events can arrive in either order, so a test that waits for two accepts both orders.
- The owner's PC (dual boot Win11 24H2 or later and Win10 22H2) runs what needs hardware, audio endpoints or the
  picker. Its items are written as exact steps in the specs (02 §16.4, 03 §22.5, 04 §18.4, 05 §18.9); a slice's
  "Manual:" items are recorded in its PR, with the `hwcheck` bundle attached where one applies, and don't block the
  merge. The exit items (WA90) do.

### C++, PowerShell and NSIS
- C++20 with 04 §13.1's flags (`/W4 /WX /permissive-` …; host `-Wall -Wextra -Wpedantic -Wshadow -Werror`),
  `native/.clang-format` (LLVM base, 120 columns). `native/common` includes no OS header outside its `os/` shim.
- Names: namespace `im` with sub-namespaces per area (`im::audio`), files `snake_case.{h,cpp}`, tests
  `<name>_test.cpp` in their library's `tests/`. The C ABI uses `im_*` functions and `IM_*` constants, fixed-width
  integers, `#define`d values, `struct_size` first, and copies all input (04 §5.1).
- Static CRT only; any new system DLL import goes onto `deploy/windows/dll-imports.txt`, and any native dependency
  onto `deploy/notices/native-windows.json`, in the same PR. vcpkg ports only through `native/vcpkg.json` at its
  pinned baseline. No GPL code or dependency anywhere (the plan).
- PowerShell: ASCII only, Windows PowerShell 5.1 compatible, PSScriptAnalyzer at Warning, Pester. NSIS: `makensis
  -WX`, every user-visible string in the language file (06 §6.10).

### Naming
- Slice ids are `WA01`… (not `W`, because 03 §3.2's Wails checklist rows are W1–W21 and M1 has W00). WA62 is split
  into `WA62a` and `WA62b` (§3, "Re-cut").
- `isshoni.exe`, `isshoni-media.dll`; AppUserModelID and single-instance id `io.isshoni.desktop`; the running mutex
  `Local\io.isshoni.desktop.running`; registry `HKCU\Software\isshoni`; credential targets `isshoni:v1:<origin>`.
- Relay kinds are dotted lower case (`playback.stop`); bridge error codes snake_case; i18n keys `desktop.*` (03 §18.1),
  `link.*` (02), `account.download.*` (06).

### Interfaces first
The first slice of a package or binary declares its whole public interface, with fakes, and unfinished parts return
a clear "not implemented" error: WA02 (`internal/media`, with 05's audio types), WA03 (all of the C ABI v1), WA10
(`internal/media/native`), WA11 (every DLL export, and the `AudioEngine`/`Mixer`/`PcmSink` boundary), WA15 (the
fake), WA29 (`internal/client/core` §11.2), WA41 (`internal/desktop/app` and `OSHooks`), WA42 (every M2 route), WA48
(every bound service), WA04 (every M2 task).

### Branches and PRs
- M2 work lands on `m2/windows-app` first (cut from `main` after M1's code merged). Each slice has its own branch,
  `m2/waNN-<short-name>`, created from the tip of `m2/windows-app` when its group starts, and its own git worktree.
- Each slice opens a PR into `m2/windows-app`. The integrator merges a group's PRs in ID order, then runs `task lint
  test build e2e native:test` and checks the Windows jobs on the result before the next group starts.
- `m2/windows-app` goes into `main` by PR at the end of each group (and at the end of M2), with `ci-ok` green.
- Every commit is signed (SSH) under the owner's identity; PRs are merged in the GitHub web UI; nobody force-pushes
  `main` or `m2/windows-app` or rewrites merged history. Commit subjects start with the slice id: `WA23: Engine
  session core on fakes`.
- No workflow that can reach the owner's PC is ever triggered by a pull request (06 §10.4–§10.5).

## 5. Implementation plan

### How the plan works
- 92 slices in 13 groups: 12 build groups and the exit group. A group starts when the previous one is merged into
  `m2/windows-app` and green. Write-back slices that an integration needs get ids `WB<n>` after group n (like M1's
  S00…), and fix slices `WF01`, `WF02`… (like M1's F02…); the integrator adds them to the table.
- Slices in one group touch **disjoint paths** ("Touches"), so separate agents can build them at once in separate
  worktrees. A Go package means its own directory only; other paths are prefixes; the file-level exceptions are the
  rows of the next table. A slice changes only its listed paths and the shared files below; if it needs more, it
  stops and asks the integrator.
- "Needs" lists slices that must be merged first; they are always in earlier groups. A check that needs another slice
  of the same group is run by the integrator right after the group merges (as in M1).
- Days are rough, for one developer: about 172 in all. Built group by group with parallel agents, the longest slice of
  each group adds up to about 36 working days; with about a day of integration per group that is about 10 weeks, the
  plan's estimate.
- The critical path is the app: WA05 → WA22 → WA32 → WA42 → WA48 → WA58 → WA68 → WA71 → WA77 → WA80 → WA84 → WA87 →
  WA90 → WA91. Beside it run the engine (WA03 → WA11 → WA23 → WA33 → WA44 → WA50 → WA60 → WA70 → WA76), audio (WA14
  → WA35 → WA45 → WA53, whose hardware cases run with WA62a right after group 6, → WA62b), the server's device flow
  (WA05 → WA19 → WA30 → WA39 → WA47 → WA56 → WA63) and the release (WA07 → WA24 → WA67 → WA72 → WA78 → WA81 → WA85).
- Milestones: native packets from the DLL through M1's SFU after group 3 (WA23's `TestNativeIntoPublisher`); hardware
  encoding from `isshoni-enginetest` after group 5 (WA44); the include-set capture's first hardware run (WA62a's cases
  on the owner's PC) after group 6; the app sharing the fake pattern from the Mac, real WGC/DD capture, the complete
  audiotest and an installer after group 7; "What friends hear" in the app after group 9; a release pipeline dry run
  after group 10; `v0.2.0-rc.1` and the exit run after group 12.
- As run: WA44 waits for the owner's FFmpeg publish and WA45 for the owner's WA09 run (§8), so group 5 merged without
  them and with WA62a (planned for group 6), group 6 ran WA54–WA58, WA62b (planned for group 7), WF3 and WB5, and group
  7 runs WA63–WA66, WF4, WF5 and WB6. WA50–WA52 and WA59, which need WA44, move to the group after WA44's, and WA53 to
  the group after WA45's; so do the group 7 slices that need them: WA60 (WA50), WA61 (WA51, WA52), WA67 and WA68 (WA59;
  WA67 also WA24, which waits for the FFmpeg lock too) and WA69 (WA44). So the app's critical path waits for the FFmpeg
  publish at WA58 → WA68 as well. The hardware-encoding milestone comes after WA44's group, and the owner's-PC run of
  WA62a's cases (WA45's and WA53's hardware gate) right after WA53's group merges: that is what "right after group 6"
  means in §3 item 13, the critical path above and WA53's and WA62a's rows. Of group 7's milestone, the app sharing the
  fake pattern from the Mac comes with WA65 and the complete audiotest came with WA62b in group 6; real WGC/DD capture
  and the installer come after WA44's line. Group 8 runs WA74, WF6, WF7 and WB7: its other slices wait for slices that
  wait for the FFmpeg publish or the WA09 run, WA70 (WA51, WA60, WA68), WA71 (WA68), WA72 (WA67, WA68), WA73 (WA69,
  which needs WA44), WA75 (WA24) and WA76 (WA53, WA60, WA61). On 2026-11-03 the owner's FFmpeg publish was done (§3 item
  50) and WA09 was run on a GitHub runner instead of the owner's PC, with Windows 10 taking the gate (§3 items 51–52),
  so group 9 ran WA24 with two fix slices, WF8 (the tools modules' Dependabot alerts) and WF9 (the flaky logout test),
  and group 10 runs WA44, WA45 and WB8. WA50–WA52 and WA59 (which need WA44) and WA53 (which needs WA45) were to come in
  the group after group 10, and the slices behind them after them. Group 10 merged WA44, WA45 and WB8; group 11 runs
  WA50, WA53 and WA59 with two fix slices, WF10 (WA44's follow-ups: the real encoders in the session's catalog, AU
  capture times, a per-layer GOP, `probeFallback` removed) and WF11 (the audio report's `tailFades`/`tail_fade` and
  `instance_gone`), and WB9 (§3 items 55–59). WA51 and WA52 (which need WA44 too) are not in group 11: their paths
  (`native/windows/src/encode/`, `native/windows/src/session/`) overlap WF10's, so they wait for it, and WA60 (WA50) and
  WA61 (WA51, WA52) and the slices behind them come later; the owner's hardware runs of WA44 wait for WF10, since until
  it merges no session opens a real encoder. Group 11 merged all six; WA50's session wiring and the probe's call of its
  OS flags were outside its paths (§3 item 61). Group 12 runs WF12 (that wiring, `session/` and `encode/probe.cpp`),
  WA51 and WA52 (which waited for WF10), WA67 and WA68 (which waited for WA59), WA69 (which waited for WA44) and WB10
  (§3 items 60–65). WA60 (WA50) is not in group 12: its `capture/` overlaps WF12's, so it waits for it, and WA61 (WA51,
  WA52), WA70 and the slices behind them come later; the owner's hardware runs of WA44 and WA45's `capture -audio mix`
  waited for WF10, which merged, and can run since the FFmpeg r2 lock was committed (§3 items 64, 69), while M-WGC-WIN,
  M-WGC-MON and M-SECONDARY waited for WF12 (§8). Group 12 merged all seven, with the r2 lock (`deps-ffmpeg-9.0.2-r2`,
  the libvpl patch, published on 2026-11-04 and committed as `b437d0e`) merged before them (§3 items 66–73). Group 13
  runs WA60 (Desktop Duplication, which waited for WF12's `capture/`), WA61 (encoder selection, which waited for WA51
  and WA52), WA71 (the first-run wizard, which waited for WA68), WA72 (the `windows-desktop` job, which waited for WA67
  and WA68), WA73 (bearer auth in the core, which waited for WA69), WA75 (the audiotest workflow, whose Needs WA24 and
  WA62b merged in groups 9 and 6), one fix slice, WF13 (WA68's leftovers: the SPA's heartbeat, the long-lock fixtures,
  `end-session-confirmed`, Action Center), and WB11 (§3 item 74). WA70 (which needs WA60) and WA76 (WA60, WA61) come
  after them, and so do the slices behind those. Group 13 merged WA60, WA61, WA71, WA72, WA75, WF13 and WB11 (WA73's
  commits were already on `m2/windows-app`), after PR #53's CI fixes (the MTA held for the process, a crash report in
  the Windows tests, `audio_live_test`'s engine and `openh264_test`'s lower bound, §3 items 75–77), and its integration
  did WA60's session half (§3 item 78). Group 14 runs WA70 (Isshoni's own windows out of every capture, which waited
  for WA60), WA76 (soak, A/V sync and `hwcheck.ps1`, which waited for WA60 and WA61), WA77 (What friends hear, which
  waited for WA71), WA78 (the desktop notices and the native license gate, which waited for WA72), two fix slices,
  WF14 (the app's toast removal and the desktop account menu, §3 items 80 and 84) and WF15 (repocheck's workflow
  guards, §3 item 83), and WB12 (§3 item 85). WA79 (which needs WA76), WA80 (WA70, WA77), WA81 (WA78), WA82 (WA77)
  and the slices behind them come after group 14. Group 14 merged all seven, after which PR #53's last CI fixes (the
  umbrella libraries, the installer's paths, the toast removal's app id, the shell relaunch, W5's fallback; §3 items
  90–92) merged with it into `main`. Group 15 runs WA80 (share safety, which waited for WA70 and WA77), WA81 (the
  release pipeline with the desktop artifacts, which waited for WA78), WA82 (the settings page and version screens,
  which waited for WA77), one fix slice, WF16 (`avsync`'s luma tap, item 89), and WB13 (§3 item 94). Group 15 merged
  all five, and its integration wired WA80's window notice through the bridge, mounted the notice and the corner card
  in the app shell and ended rule 6's `update.*` exemption (§3 item 96). Group 16 runs WA79 (the exit run book, which
  waited for WA76 and WF16), WA83 (handoff, which waited for WA80), WA84 (the settings services and updates, which
  waited for WA80 and WA82), WA85 (the site, which waited for WA81), WA86 (the nightly, which waited for WA81), one fix
  slice, WF17 (group 15's follow-ups), and WB14 (§3 item 101); WA87, WA88 and WA89, which need WA84 (and WA83 or
  WA86), come after group 16. Group 16 merged all seven, and its integration reserved WA84's test switch, added the
  autostart round trip to `windows-desktop` and `windows-full` and widened the site filter (§3 item 108). Group 17
  runs WA87 (the real-app e2e and the manual matrix, which waited for WA83 and WA84), WA88 (the updater e2e, which
  waited for WA84 and WA86), one fix slice, WF18 (group 16's follow-ups, §3 items 103–104), and WB15 (§3 item 109);
  WA89 is the owner's runs, then WA90 and WA91. The Grp column stays the plan: the groups as run are this bullet's,
  and no row is renumbered.
- If M2 runs late, these can move after `v0.2.0` without touching the exit criterion: WA51 (OpenH264; MF software stays
  the fallback), WA69 (the hardware capture-to-send check), WA86 (nightly rows). A moved slice leaves every Needs list.

### Shared files and their owners

| File | Owner | Rule for every other slice |
|---|---|---|
| `go.mod`, `go.sum` | M1's rule | WA22 adds Wails `v3.0.0-beta.34` (it raises no server requirement, 03 §3.1); no other new module is planned. On a conflict the integrator takes the base files, re-runs each slice's `go get`, then `go mod tidy` |
| `tools/go.mod` | integrator | WA08 adds `golang.org/x/sys`. `tools/wails3/go.mod` is a module of its own (WA04) |
| `Taskfile.yml` | WA04 | WA04 writes every M2 task up front (06 §17's list, `desktop:dev`, `native:*`, `ffmpeg:*`), with real bodies for `native:test`, `ffmpeg:build`, `release:wails3`, `lint:go` and `lint:pins` and stubs that exit 0 with a note. Later slices change only the bodies of tasks they own: `native:build` (WA11), `desktop:dev` and `desktop:exe:windows` (WA22; WA67 extends the latter), `ffmpeg:fetch` and `native:dist` (WA24; WA76 adds `hwcheck.ps1` and `g2g.exe`), `release:update-keys` (WA25), `lint:ps`, `test:ps`, `release:prepare` (WA49), `desktop:package:windows` (WA67), `release:assets` (WA81) |
| `.golangci.yml` | WA02 | WA02 writes every M2 depguard rule (01 §4, 03 §4.5, 05's rows); then integrator only |
| `.github/workflows/ci.yml` | WA04 | only the slices that list it (WA11, WA24, WA49, WA67, WA72, WA78, WA81, WA88; as run, WA44 in group 10 for `windows-native`'s test steps, where the FFmpeg DLLs sit), never two in one group, except in group 14: WA78 changes its jobs' steps (`desktop-macos`, `windows-desktop`) and WF15 only the paths filter that decides when `test-go` or a repocheck step runs; the integrator merges both. In group 15 only WA81 (`windows-desktop`'s server-rendered ps1 test); in group 16 none, and group 16's integration added `windows-desktop`'s autostart round trip and the site filter's `web/src/**/*.ts`, `web/src/**/*.tsx` and `.goreleaser.yaml` (§3 item 108); in group 17 only WA88 (`windows-desktop`'s updater e2e on the `update` filter) |
| `.github/workflows/{ffmpeg,audiotest,release,nightly}.yml` | WA07, WA75, WA81, WA86 | `nightly.yml`: WA88 adds the updater e2e (in group 17 it replaces `windows-full`'s placeholder step "The updater e2e (README WA88)", and only WA88 changes the file); group 16's integration added `windows-full`'s autostart round trip beside ci.yml's (§3 item 108); `audiotest.yml`: at WA76's merge (group 14) the integrator passes `hwcheck.ps1`'s output argument, which WA76 defines, in the `hwcheck` suite's step (§3 item 83) |
| `.github/dependabot.yml` | WA04 | integrator only |
| `tygo.yaml` | integrator | WA18 adds the `relay` entry, WA48 the `bridge/codes` entry |
| `internal/protocol`, `internal/protocol/api` | M1's rule | additive changes only (WA05, WA18), called out in the PR; the integrator re-runs `task gen` |
| `web/src/protocol/*.gen.ts`, `web/src/platform/desktop/codes.gen.ts`, `web/src/platform/desktop/bindings/` | generated | never edited or merged by hand: after each merge run `task gen` and `wails3 generate bindings` and commit the output |
| `web/package.json`, `web/package-lock.json` | M1's rule | WA22 adds `@wailsio/runtime` at the Go module version of Wails without the `v` (`3.0.0-beta.34`), exact, which `lint:pins` checks against `go.mod` (03 §3.1); other packages go through the integrator |
| `web/src/app/router.tsx` | WA42 | WA42 declares every M2 route (the desktop pages and `/link`), each lazily loading its folder; page slices fill only their folders |
| `web/src/i18n/en.json`, `web/src/i18n/lazy/*.en.json` | M1's rule | keys under the slice's own namespace: `desktop` in `lazy/desktop.en.json` (created by WA41; WA42's stub pages add no keys, and its four entry-chunk texts are `common.*` keys, 03 §18.1), `link` in `lazy/link.en.json` (WA63), `account.download.*` (WA74), two `errors.*` keys (WA63); conflicts keep both sides; `check:i18n` must pass |
| `web/e2e/` | file-level | each spec file belongs to one slice; `fixtures.ts` gains `DESKTOP_E2E_BIN` in WA48 and `lockResumeMs`/`lockStopMs` in WF13 (group 13); `desktop-share.spec.ts` is WA65's, extended by WA80; `desktop-system.spec.ts` is WA68's, extended by WF13 (group 13), WF14 (group 14: only the Action Center branch, which then expects the removed toast's id) and WA80 (group 15); in group 16 WF17 extends `desktop-share.spec.ts` (the close-to-tray step) and `desktop-system.spec.ts`'s long-lock case (the lock-prompt bar), and adds the `/desktop/settings` case to `a11y.spec.ts` (M1's, which WA63 extended); WA83 writes `desktop-handoff.spec.ts`. In group 17 WA87 completes `web/e2e/cdp/` (the CDP spec) and WF18 adds the required-update banner's and the desktop version screen's cases to `a11y.spec.ts` (and runs `desktop-handoff.spec.ts` without the web repeat); no other spec file changes |
| `internal/desktop/app`, `web/src/rooms/AccountMenu.tsx` | WA41 (the app; WA68, WA71 since); M1 (the menu) | in group 14 WA77 changes `internal/desktop/app` only for `SelfInfo`, `SetVisible` and `EventPlaybackState` → `SetViewerBrowser`, and WF14 only for the toast removal (`toastBook.dropKind`, `clearLockLocked`, the pending toast keeping its notification id, an optional remover interface); the integrator resolves conflicts. WF14 builds `AccountMenu.tsx`'s desktop menu (03 §7.5), which WA71's row lists but its run left out (§3 item 80); the browser's menu stays as it is. In group 15 only WA80 changes `internal/desktop/app` (its row's share-safety parts); `web/src/share/native/` is WA80's and `web/src/desktop/` WA82's, and mounting WA77's `MicPromptCorner` once for the desktop app (§3 item 87) is the integrator's at group 15's merge unless one of them does it in its own paths (group 15's integration mounted it in `web/src/app/App.tsx`, §3 item 96). In group 16 WA84 changes `internal/desktop/app` for the updater (refused while live, the tray offer at an autostart start) and `internal/desktop/bridge` only in `settings.go` and `updates.go` (with the OpenH264 methods), plus `web/src/platform/desktop/` and `web/src/desktop/` for wiring them; WF17 changes `web/src/platform/desktop/bridge.ts` only for `ShareView`'s `lockPrompt`; WF17 changes `internal/desktop/bridge` only for `ShareView.lockPrompt`, `internal/desktop/share` or `app` only to fill it from `App.sys.prompt`, `internal/desktop/e2ehost` (a hook that calls `WindowClosing`) and `web/src/share/native/` (the bar); each keeps its change focused, and the integrator resolves conflicts and regenerates the bindings. As run (§3 items 104, 107): WA84 also changed `internal/desktop/bridge/bridge.go` (`Deps.Updates`, `Deps.OpenH264`, the `update.state` subscription, the `Settings` fields), `internal/desktop/app/share.go` (`Shares()` hands out the gated controller) and `internal/desktop/winos`' non-Windows stubs (`oshooks_other.go`, `winos_other_test.go`: `Autostart` answers `ErrNotSupported`); WF17 also changed `web/src/platform/desktop/testing/shareView.ts` (the fixture), `lazy/desktop.en.json` (two `desktop.share.panel.*` keys) and `desktop-system.spec.ts`'s long-lock case; all accepted. In group 17 only WF18 changes `internal/desktop/app` (`followPlayback` subscribing before `Ready`, `SwitchServer` keeping a confirmed `{switch}` link's room pending, the `too_many_servers` link cleared once shown) and `internal/desktop/settings` (`Store.Subscribe`, so `settings.changed` follows every change), with `web/src/rooms/` (the 3 s repeat removed), `web/src/desktop/SetupPage.tsx`, `web/src/share/ShareSheet.tsx` and `ScreenAudioWarning.tsx` (and `en.json`'s `share.sheet.desktopAppWindowsOnly`) |
| `internal/server/wire.go` | M1's rule | only WA56 and WA66 |
| `internal/server/config/keys.go` | integrator | WA66 adds only 06 §7.3's names to the reserved env list; WF7 (group 8) adds M2's dev and test names (§3 item 48). From then on, any slice that starts reading a new `ISSHONI_*` name needs its line here and in `keys_test.go`'s `devEnvNames` in the same PR (`TestRepoEnvNamesReserved`; §4 "Config"), through the integrator; group 10's integration added WA44's `ISSHONI_ENGINETEST_TEST_CHILD` and `ISSHONI_TEST_FFMPEG_DIR`, and group 16's WA84's `ISSHONI_REQUIRE_AUTOSTART_E2E` (§3 item 108; 06 §7.3 lists it, and M1 04 §4.2's list, outside `docs/m2/`, still needs it), and M1 04 §4.2's list follows the file (WB9, §3 item 58) |
| `internal/server/itest` | file-level | each slice adds its own test files; `desktop_helpers_test.go` is WA29's (WA73 switches it to bearer, and with it `recoveringDesktop` in WA38's `desktop_recovery_test.go`, which copies its cookie auth), `device_helpers_test.go` is WA56's (WA64 adds `linkedManager`); `desktop_congestion_test.go` is WA54's, and in group 7 WF4 changes only D10's `callSlack` and WF5 only D9's `recoverWithin` (the integrator merges both; PR #43's fixes then rewrote D10's keyframe check with `engineTap` and gave WA38's `recoveringDesktop` a twin with an engine wrap, `recoveringDesktopOn`); in group 8 only WF6 may change it, and only D10's bounds |
| `native/include/isshoni_media.h` | WA03 | WA03 writes all of v1.0; later changes only through the integrator with 04's and 05's owners (none planned in M2) |
| `native/CMakeLists.txt`, `CMakePresets.json`, `vcpkg.json`, `vcpkg-configuration.json`, `.clang-format`, `triplets/` | WA03 (`triplets/x64-windows-static.cmake`: group 2's CI fixes) | integrator only; the vcpkg baseline is bumped by hand, and the triplet's `VCPKG_PLATFORM_TOOLSET_VERSION` moves with `deploy/windows/tools.json`'s toolset (`task lint:pins` checks it) |
| `native/common/CMakeLists.txt` | WA03 | adds `audio/`, `audiopolicy/`, `audiomix/` when present and globs each library's `tests/`; slices add files only |
| `native/windows/CMakeLists.txt`, `native/windows/src/api/` | WA11 | the CMake file globs each `src/` folder and `tests/`; every export forwards to an area function whose stub lives in that area's folder, and a slice replaces its own area's stub |
| `native/windows/src/audio/` | WA35/WA45 | in group 11, WF11 changes only `audio/report.cpp`'s mapping and `watcher.cpp`'s tail-fade counter and event lines; WA53 owns the rest; the integrator resolves `watcher.cpp` conflicts (as merged, WF11's change was in `native/common/audiomix`'s writer and `watcher.cpp`, and group 11's integration kept WA53's helpers and WF11's `kTailFade` mapping in `watcher.cpp`) |
| `native/windows/src/session/` | WA23 (WF2, WF10 since) | in group 12, WF12 changes `session_object.{h,cpp}` only for the WGC capture (starting `capture::WgcCapture`, its sink, the supervisor's `SOURCE_*` events, `IM_SESS_NO_OS_INDICATOR`, `SetFps`, the stop order) and WA52 only for pause, the stats JSON, the info-level log rule and the `IM_INIT_NO_AUDIO` start check; each keeps its change focused, and the integrator resolves `session_object` conflicts; in group 13 only WA61 changes it (device loss, `im_set_profile`, `im_update_layer`, `im_set_content_hint` re-inits); in group 15 only WF16 (the `IM_TEST_HOOKS`-only `IM_PACKET_DEBUG` luma-tap producer, §3 item 89) |
| `native/windows/src/encode/` | WA44 (WF10 since) | in group 12, WF12 changes only `probe.cpp`'s `BuildFlags` (to call `capture::OsFlags()`) and WA51 adds the OpenH264 backend's own files, registered through WA44's selector table; `native/windows/src/capture/` is WA50's, and in group 12 only WF12 changes it, as little as the wiring needs; in group 13 only WA61 changes `encode/` (the selector) and only WA60 `capture/` (DD as another `CaptureFactory` beside WGC's, 04 §7.3) |
| `native/windows/tests/` | file-level | each test file belongs to one slice, and so do the support header and `testdata/` folder that only it uses (`gpu_warp_support.h` and `testdata/gpu/` are WA33's `gpu_warp_test`'s); the `ProbeFlags` check of group 4's hand-over (04 §9.10) is in WA44's `ffmpeg_load_test.cpp`, with the probe it checks, not in WA34's `audio_bridge_test.cpp` (§3 item 55) |
| `cmd/isshoni-enginetest` | WA44 | WA44 writes the tool (flags, dispatch, `probe`, `capture`, `bench`); each later subcommand is one file registered in `init()` and owned by its slice (`pick.go` WA50, with its `pick_test.go`, `openh264.go` WA51, `sources.go` WA60, `sessions.go` WA61, `protect.go` WA70, `soak.go` and `avsync.go` WA76); WF10 (group 11) changes WA44's `capture.go` and `check.go` for `-bitrate-steps`' GOP (§3 item 55); WF16 (group 15) changes WA76's `avsync.go` (and its tests) to use the luma tap; WF17 (group 16) changes `avsync.go`, `avsync_windows.go` and their tests only for the flash window's monitor and the missed readings in the report |
| `deploy/notices/native-windows.json` | WA03 | created in 06 §9.2's format; append-only, one entry per native dependency with the slice that adds it; WA78 adds the gate |
| `deploy/windows/dll-imports.txt` | WA11 | append-only: any new system DLL import in the same PR |
| `deploy/windows/tools.json`, `deploy/windows/msvc-env.ps1` | WA07 (the `msvc` entry and the script) | WA11 adds the other entries (NSIS, WebView2, Pester, PSScriptAnalyzer) and reuses the script; later changes are hand-made tool PRs (06 §3) |
| `docs/m2/*.md` | WA01 | then only write-back slices, WA60 (04 §7.5) and WA87 (03 §3.2, §20). A slice that measures something a spec records puts the result in its PR body under the heading "Results for the spec": WA07 (06 §4.6), WA22, WA48 and WA68 (their 03 §3.2 rows, under "Wails checklist"), WA44 (04 §9.3, layer 0's VBV included). WA87 copies the 03 §3.2 rows; the integrator's `WB2` and `WB5` copy 06 §4.6 and 04 §9.3 (`WB1` could not: WA07's first PR run summary is on GitHub; `WB5` could not either: WA44 hasn't run yet, since it waits for the owner's FFmpeg publish, so the write-back slice after WA44's group copies 04 §9.3; `WB5` wrote WA48's W11, W12 and W15 into 03 §3.2; `WB9` could not copy 04 §9.3 either: WA44's hardware runs wait for WF10, §3 item 55, so a write-back after those runs does; `WB10` could not either: WF10 merged in group 11, but the owner's runs haven't happened yet, §8; nor could `WB11`, for the same reason, and in group 13 it leaves 04 §7.5 to WA60, which runs in the same group; nor could `WB12`: the owner's runs of WA44 and WA60's M-DD (04 §7.5's `DisplayName()` table, still "pending (owner's PC)") haven't happened yet, §8, so the write-back slice after them copies both; nor could `WB13` or `WB14`, for the same reason). In group 15 WF16 writes 04's luma-tap payload (its section of §5 and `avsync`'s row and "As built" lines in §14) and WB13 the rest of the group 14 write-backs, 04 §14's `protect-check` and WA76 paragraphs included; the integrator merges 04. In group 16 WB14 writes group 15's write-backs, and WA79 writes only its own folder, `docs/m2/exit-test/` (not a spec file). In group 17 WA87 writes only 03 §3.2 (its rows copied from the PRs' "Wails checklist") and §20 (the measurements), and WB15 group 16's write-backs everywhere else; the integrator merges 03 |
| `docs/*.md` (site pages) | 06 | WA57 (device rows, `#desktop-linking`), then WA85 |

**Dependencies and pins** (each must pass the license gate; the first slice to need one adds it at the pin below)

| Dependency | Pin | First needed by |
|---|---|---|
| `github.com/wailsapp/wails/v3` (and the `wails3` CLI, `@wailsio/runtime`) | `v3.0.0-beta.34`, exact; they move together, by hand, at most once per minor release (03 §3.3) | WA04 (CLI), WA22 |
| `golang.org/x/sys` (main module, already v0.49.0) | the `windows` package for the binding, Credential Manager, `winos`, the updater | WA10 |
| `golang.org/x/sys` in `tools/go.mod` | `g2g`'s GDI capture | WA08 |
| `golang.org/x/crypto/ssh`, `golang.org/x/net/idna` (already required) | the SSHSIG verifier; deep-link hosts | WA25; WA20 |
| vcpkg `opus`, `speexdsp` (BSD-3-Clause, static), `gtest` (feature `tests`, never shipped) | `builtin-baseline` in `native/vcpkg.json` | WA03 |
| FFmpeg 9.0.2 with nv-codec-headers 12.1, AMF headers, libvpl | `native/ffmpeg/pins.json` (revision 2); published as `deps-ffmpeg-9.0.2-r1` (2026-11-03) and `deps-ffmpeg-9.0.2-r2` (the libvpl patch, 2026-11-04, from `957cce6`), assets pinned in `lock.json`, which is r2's (committed as `b437d0e`, §3 item 69) | WA07 |
| Cisco OpenH264 | 2.7.0, the newest Cisco release at WA51, from `https://ciscobinary.openh264.org/openh264-2.7.0-win64.dll.bz2` (HTTPS); the decompressed DLL's size (975 352 bytes) and SHA-256 (`0ef5e7f3…c288`) compiled into the engine (`openh264_pin.h`) and the binding (`native.OpenH264`), which a test keeps equal; downloaded at run time, never shipped (04 §9.9, §3 item 66) | WA51 |
| VS 2022 17.14 or VS 2026 (vswhere `[17.0,19.0)`, `VC.Tools.x86.x64`), MSVC toolset 14.44 exact through `deploy/windows/msvc-env.ps1` (vcpkg's ports too, through `native/triplets/`), Windows SDK 10.0.26100, NSIS 3.13, the WebView2 bootstrapper, Pester 5.x, PSScriptAnalyzer 1.x | `deploy/windows/tools.json` | WA07 (MSVC), WA11, WA49, WA67 |
| GitHub Windows runner | `windows-2025` | WA04 |

### Slices

"Grp" is the parallel group. "Spec" gives the source slice and sections (`04 E5`, `03 DK7`). "Where": Mac, CI
(ubuntu, macos, windows), owner's PC, GitHub (dry run or nightly).

| ID | Grp | Slice | Spec | Touches | Needs | Days | Where | Acceptance |
|---|---|---|---|---|---|---|---|---|
| WA01 | 1 | M2 design set and its write-backs | this README §3 | `docs/m2/` (the six specs and this README), `docs/.vitepress/config.mts` (`srcExclude` gains `'m2/**'`, and its comment) | – | 1 | Mac, CI ubuntu | Each of §3's decisions 1–14 is written into the sections it names, and 15–18 are checked to be there, with no other content change; the superseded spellings (`--deeplink`, `native.Load`, `isshoni_media.dll`, `isshoni-latency`, `%APPDATA%\isshoni`, `run --suite`, windows-latest as a runner, `Audio.SetMode` or `Service.SetMode` as a bridge or Service method, `native.AudioTestTone`, `tools/wails3/runtime-version`, `isshoni-audiotest-windows-amd64.zip`, `Source.Self`) appear in no spec except where one explains why it isn't used; `task site:build` passes, its output has no page under `m2/`, and `check-site` passes |
| WA02 | 1 | Media contract | 01 C01; 01 D1–D3, §4, §5; 05 §13.1 (declarations) | `internal/media` (root: `media.go`, `probe.go`, `errors.go`, `audio.go`), `internal/media/fake` (the aliases only), `internal/client/publish` (`Source` on `media.Packet` only), `.golangci.yml` | – | 1.5 | Mac, CI ubuntu | `go test -race ./...` green with no M1 assertion changed (fake tests read keyframe and flash flags from the Markers); `go list -deps ./internal/media` shows only the standard library; `fake.Packet`, `Kind`, `Video`, `Audio` are aliases; 01 §5's surface and 05 §13.1's audio types and `AudioController` are declared (`ParseAudioReport` returns a not-implemented `*media.Error` until WA26); `task lint:go` passes with every M2 depguard rule, and a scratch import of Wails into `internal/desktop/x` and of `internal/media/native` into `internal/client/x` each fail it (shown in the PR); `GOOS=android` and `GOOS=ios` `GOARCH=arm64 CGO_ENABLED=0 go build ./internal/client/... ./internal/media` pass |
| WA03 | 1 | C ABI v1 header and the portable native skeleton | 04 E1 (C++ part); 04 §5, §13.1, §13.5; 05 §4 | `native/include/`, `native/CMakeLists.txt`, `native/CMakePresets.json`, `native/vcpkg.json`, `native/vcpkg-configuration.json`, `native/.clang-format`, `native/common/` (status, handle table, JSON writer, log ring, `CMakeLists.txt`, `tests/`, `tests/testdata/abi_layout.json`; not `audio/`, `audiopolicy/`, `audiomix/`), `deploy/notices/native-windows.json` (new: libopus, speexdsp) | – | 2.5 | Mac, CI ubuntu/macos (MSVC: WA11) | `isshoni_media.h` declares all of v1.0: 04 §5.2 with 05 §4.1's audio block and the three test exports, `IM_ABI_MINOR 0`; it compiles as C11 and as C++20 with `-Werror` under clang and gcc; `cmake --preset host-debug`, build and `ctest` pass on the Mac (`handles_test`, `json_test`, `abi_layout_test` against the golden file with 04 §5.4's sizes); `host-tsan` clean; `native/common/CMakeLists.txt` follows §3 item 14; `clang-format --dry-run --Werror` clean; `vcpkg.json` pins `builtin-baseline` with opus, speexdsp and the `tests` feature |
| WA04 | 1 | Cross-OS desktop CI and the M2 tasks | 06 R1; 06 §3, §10.1, §10.6, §17 | `.github/workflows/ci.yml` (filters `desktop`, `native`, `ps`, `update`; `lint-go`'s Windows, gomobile and cross-build steps; jobs `windows-go`, `native-host`, `desktop-macos`; `ci-ok`), `.github/dependabot.yml` (the Wails ignores), `Taskfile.yml`, `tools/wails3/` (own module, the CLI at `v3.0.0-beta.34`), `tools/repocheck` (`pins`) | – | 1.5 | CI ubuntu/macos/windows, Mac | actionlint clean; `task --list` shows every M2 task, those without inputs exiting 0 with a note; once group 1 is merged, a PR touching `internal/client` runs `windows-go` (MinGW `-race`, then `CGO_ENABLED=0`) and `desktop-macos` green, and `native-host` runs WA03's host tests on ubuntu-24.04 and macos-15 green; jobs whose inputs don't exist yet are skipped and count as success; `lint:pins` fails when `tools/wails3/go.mod` and `go.mod` name different Wails versions (and passes while `go.mod` has none) and when `@wailsio/runtime` is a range or differs from `go.mod`'s Wails version without the `v` (in `web/package.json` and in the lockfile) |
| WA05 | 1 | Device-auth shared names | 02 DA1; 02 §2.1, §6.4, §7.3, §13.2 | `internal/protocol/api` (`device.go`), `internal/protocol` (`desktop.go`), `internal/version` (`semver.go`), `internal/server/signal` (its `semver.go` replaced by `internal/version`; `Identity.DeviceName` declared), `web/src/protocol/*.gen.ts` (generated) | – | 1 | Mac, CI ubuntu | 02 §16.1's rows for `api`, `protocol` and `version` (golden JSON with redaction, `NormalizeUserCode`/`FormatUserCode`, SemVer tables); `DesktopOrigins()[0] == DesktopAssetOriginWindows == "http://wails.localhost"`; the hub's tests unchanged and green; `task gen:check` clean |
| WA06 | 1 | Built-in audio rules | 05 A1; 05 §5.4 | `internal/media/audiorules` | – | 0.5 | Mac, CI | Names lower case and unique across lists; `msedgewebview2` is not a browser; the browser families are complete; standard library only; tests green on linux, darwin and windows |
| WA07 | 1 | FFmpeg LGPL build | 06 R2; 06 §3, §4 | `native/ffmpeg/` (`pins.json`, `build.sh`, `ffcheck.c`, `README-LGPL.txt`, `lock.json`), `.github/workflows/ffmpeg.yml`, `tools/desktoppkg` (new: the dispatch, `ffmpeg-inputs`, `ffmpeg-fetch`), `deploy/windows/msvc-env.ps1`, `deploy/windows/tools.json` (the `msvc` entry) | – | 2 | CI windows; owner dispatch | The PR build is green on windows-2025 (VS 2026 image) with toolset 14.44, and the log and summary name the VS instance, the toolset build and the SDK; with `toolset` set to one the image lacks, `msvc-env.ps1` fails naming the toolsets it found; the zip holds both DLLs, import libraries, headers, PDBs and licenses; `ffcheck` reports LGPL 2.1 or later, exactly `h264_amf`, `h264_mf`, `h264_nvenc`, `h264_qsv`, and no gpl, nonfree or version3; the DLLs depend on system DLLs only; a second build is byte-identical, or the PR records why not under "Results for the spec" and that check becomes a warning (06 §4.6; `WB2` writes it back: `WB1` could not read the run on GitHub). Owner: Actions → FFmpeg → Run workflow with `publish` creates `deps-ffmpeg-9.0.2-r1` as a non-latest prerelease with attestations, and the printed `lock.json` is committed before WA24 |
| WA08 | 1 | Glass-to-glass tool | 01 C17; 01 §12.1, §12.2 | `tools/g2g/`, `tools/go.mod` | – | 2 | Mac, CI ubuntu; owner's PC | Decoder tests on recorded PNG fixtures (a transition frame, a 50 % scaled copy, nested copies, and each content-cut image) pass on the Mac and in CI; the clock page makes a full-screen content cut every 5 s (`?cuts=0` turns them off) and plays its tone after the Start button; `g2g measure -run gate\|nosound\|nocuts` labels the JSON; `GOOS=windows go -C tools build ./g2g`; `g2g serve` serves the clock page; exit codes 0, 1, 2 as 01 §12.2. Manual: a dry run on the owner's PC decodes ≥ 90 % of the screenshots |
| WA09 | 1 | Windows 10 audio gate and the onset-fade re-run (owner) | 05 A0; 05 §3, §18.10 | `spikes/s1-win-audio/README.md` (results only) | – | 0.5 + the runs | owner's PC | Owner: `s1.exe selftest` on Win10 22H2 and `s1.exe selftest -only late-start,late-start-endpoint` on Win11, results recorded. Pass: the include-set scenarios pass on Win10 and late-start has no click on Win11. If Win10 fails, the plan's gate applies before WA45 merges: Win10 builds below 22000 use endpoint loopback with the banner, the Win10 audio gate takes 05 §18.10's reduced form, and the owner is told. As run (PR #48, merged into `main` and `m2/windows-app` on 2026-11-03; §3 items 51–53): by the owner's decision the runs were on a GitHub `windows-2025` runner with VB-CABLE (`.github/workflows/s1-audio.yml`, `spikes/s1-win-audio/ci/setup-audio.ps1`), not on the owner's PC: the full `selftest` and the `-only late-start,late-start-endpoint` re-run, twice. The include-set scenarios pass on Server 2025 (−133 to −145 dB); late-start has no onset click, and its stop click in run 1 led to the spike's tail fade, with which late-start passes in both runs of run 2; `endpoint-baseline` fails on the runner's silent plain loopback. Windows 10 was not run: the owner took the gate (builds below 22000 → endpoint loopback with the banner; the Win10 gate in 05 §18.10's reduced form). Results are in the spike's README (not in `results/`: a virtual output on a CI runner) |
| WA10 | 2 | Go binding skeleton | 04 E1 (Go half), E2 (Go loader); 04 §12.1–§12.4; 01 §7 | `internal/media/native` (`abi.go`, `types.go`, `errors.go`, the `caller` interface and `fakeCaller`, `layout_test.go`, `engine_other.go`, `engine_windows.go`, `call_windows.go`, `clock_windows.go`; the whole 04 §12.2 API declared) | WA02, WA03 | 2 | Mac, CI ubuntu; CI windows | `layout_test` agrees with `abi_layout.json`; through `fakeCaller` on the Mac and ubuntu: every status and token → `*media.Error` with code, `Reason` and `Op`, the snprintf retry, the clock cross-check (passes; a 2 ms offset fails `Open`); `Open` returns `ErrUnsupportedOS` off Windows; `GOOS=windows` vet and lint clean; with WA11's DLL (integrator, after the group): `Open` loads it by full path with the DLL-folder and System32 search flags, the ABI check passes, a missing export is `dependency`/`abi` naming it, `BuildInfo` decodes |
| WA11 | 2 | DLL skeleton and the `windows-native` job | 04 E2 (C++), 06 R3a; 04 §5.3, §6.1, §6.4, §10.2, §13.1, §13.4; 06 §5.1 | `native/windows/` (`CMakeLists.txt`; `src/api/` exports and noexcept guards; `src/engine/` worker thread; the `AudioEngine`/`Mixer`/`PcmSink` boundary with a stub `AudioEngine`; `isshoni-media.def`; `resources/version.rc.in`; `tests/exports_test.cpp`), `.github/workflows/ci.yml` (`windows-native`, also running the Go DLL tests until WA72), `tools/desktoppkg` (`dll-check`), `deploy/windows/` (`tools.json`'s remaining entries, reusing WA07's `msvc-env.ps1`; `dll-imports.txt`), `Taskfile.yml` (`native:build`) | WA03, WA04, WA07 | 2.5 | CI windows | `win-x64-release` and `win-x64-ci` build with `/W4 /WX`, and the header compiles as C11 under MSVC; `exports_test`: the `.def` equals the header's `IM_API` list, null and invalid arguments return their documented codes with the 04 §5.7 token, `struct_size` below v1.0 is `IM_ERR_ABI`, JSON follows snprintf, `im_clock` is monotonic, `im_abi_layout` equals the golden file, a stalled worker gives `IM_ERR_TIMEOUT`; WA03's host tests pass under MSVC; `dll-check` passes (exports, `static:`/`delay:` imports, VERSIONINFO with `OriginalFilename` `isshoni-media.dll`) and fails on fixtures with an extra export, a `VCRUNTIME140.dll` or `avcodec-*` import, a static `mfplat.dll` import and a wrong `ProductVersion`; 04 §18.2 `dll_search_test`'s delay-load part (a planted `im-test-missing.dll` in the current directory and on `PATH` is refused while `mfplat.dll` still loads from System32), and the PR records whether the delay-load helper honors `/DEPENDENTLOADFLAG:0x800`, which decides 06 §5.1's conditional load-config check; `IM_FFMPEG_ROOT` is optional until WA24 |
| WA12 | 2 | Rings, normalizer, pacer, encoder policy | 04 E3; 04 §6.2, §7.5, §8.3–§8.5, §9.1, §9.4, §18.1 | `native/common/` (new files of the core library, `tests/`, `tests/fuzz/`; not `audio/`, `audiopolicy/`, `audiomix/`) | WA03 | 3 | Mac, CI ubuntu/macos/windows | 04 §18.1's `ring_test`, `event_queue_test`, `annexb_test` (synthetic cases; the golden AUs come with WA44), `sps_test`, `pacer_test`, `encpolicy_test`, `scale_test`, `monitor_map_test` and `resize_test` pass in `host-debug`, `host-tsan` and `win-x64-ci`; `annexb_fuzz` and `sps_fuzz` run 60 s each without a failure, seeds in `tests/fuzz/corpus/` |
| WA13 | 2 | Opus stage, PCM tap, test tone | 04 E4; 04 §10.3; 05 §10.1–§10.4 | `native/common/audio/` (with its `tests/`) | WA03 | 2 | Mac, CI | `opus_stage_test` as 04 §18.1: 05 §10.1's settings read back; 10 → 20 ms framing; packet times exactly 20 000 000 ns apart after the lookahead correction; `REINIT` on the first packet and after `OnGap`; `DISCONTINUITY` after a ring drop; chunks before `epoch_ns` dropped; a decoded 1 kHz tone at 128 kbps with SNR ≥ 30 dB; encoded silence at 50 packets/s; 3840-byte PCM packets; the test tone's Marker in golden bytes that M1's `ParseAudioMarker` reads |
| WA14 | 2 | Audio policy core | 05 A3; 05 §5, §6 | `native/common/audiopolicy/` | WA03 | 3 | Mac, CI ubuntu/macos/windows | About 60 GoogleTest table cases (05 §5.10's examples, precedence, launchers, the WebView2 path rule, lineage, mic stickiness, the 48-stream cap) and the 10 000-seed property test of invariants I1–I5 pass in `host-debug`, `host-tsan` (clang, gcc) and `win-x64-ci` (MSVC) |
| WA15 | 2 | Fake engine and the conformance suite | 01 C02; 01 §6 | `internal/media/fake` (`engine.go`, `session.go`, `Source.Now`/`Origin`, a no-op `audio_ctl.go`), `internal/media/mediatest` | WA02 | 2 | Mac, CI ubuntu/macos | `fake.Engine` passes `mediatest.Conformance` (Synthetic; Decodable without check 5); single-use picks, `Release`, `Listed`; `SetBitrate(0)` is `invalid`; `SetLayerActive` pauses and resumes with a keyframe and no `Reinit`; `SetPaused` stops video only; re-init mode marks `Reinit`; `Reconfigure{Profile}` swaps the SPS and emits `EventLayer`; `Pick` cancel, timeout and ctx; `Fail`, `Emit`, `Calls`; goleak clean |
| WA16 | 2 | Publisher M2 | 01 C05; 01 §8 | `internal/client/publish` | WA02 | 2 | Mac, CI | 01 §14's publish row: SR NTP = `wall(now) − (Now − capture)` within 1 ms for 25 ms-old packets; pacer at 24 Mbit/s: a 300 KB frame's last packet leaves after 55–70 ms from a full bucket and 95–110 ms from an empty one, audio delayed ≤ 2 ms; backpressure blocks and never drops; a NACK for a packet 3000 back is answered (responder 4096); RTT from a crafted RR; `CaptureToSend`; `KindPCM` dropped; M1's sfu and itest suites green with pacing off by default |
| WA17 | 2 | Uplink rate control | 01 C06; 01 §9 | `internal/client/ratectl` | WA02 | 1.5 | Mac, CI | 01 §14's ratectl table at fixed instants: initial targets per preset, §9.2's split examples, clamps to 04 §6.4's ranges, the ramp 4 → 8 Mbit/s in 5 steps and 10 s, live guards (15 %, 2 s, latest wins) and re-init guards (30 %, 10 s) per layer with `SetLive`, `OpActive` with the resume target, the admin cap, the upload hint (on after 6 s of a binding cap, never during the ramp, off after 10 s), `SetLimit` cpu/gpu |
| WA18 | 2 | Protocol additions and relay payloads | 01 C07; 01 §10.3, §10.4, §11.11 | `internal/protocol` (additive: `ShareUpdate.Paused`, `ShareStatusPaused`, `OwnConnection.DeviceName`, the features), `internal/protocol/relay`, `tygo.yaml` (the `relay` entry), `web/src/protocol/*.gen.ts` (generated) | WA05 | 1 | Mac, CI | Fixtures for `share.update{paused}`, `room.state` with `paused` and `user.connections` with `deviceName`; relay round trips for `playback.stop` (`{v: 1, roomId}`) and `playback.state`; the reserved kinds named; M1's v1 fixtures replay unchanged; `task gen:check` clean with `relay.gen.ts` |
| WA19 | 2 | Device-flow store | 02 DA2; 02 §4 | `internal/server/store` | WA05 | 1.5 | Mac, CI ubuntu | Migration `0002_device_flow.sql` and every 02 §4.2 method; the migration test from a schema-1 file with device rows; a parallel spend gives `ErrConflict`; the trims; the devices prune rule and `PruneStats.Devices` at their boundaries; M1 03 §15's schema tests green with the new golden schema |
| WA20 | 2 | Version verdicts and deep links | 02 DA8; 02 §12, §13 | `internal/client/compat`, `internal/client/deeplink` | WA05 | 1 | Mac, CI ubuntu | The compat table, including the undecided `FromError` case; the `testdata/links.json` cases (query and fragment never read, ≤ 2048 bytes, the room-id rule); the `NormalizeOrigin` table; `FuzzParse` runs 30–60 s clean; `GOOS=windows go vet` clean |
| WA21 | 2 | Credential store | 02 DA9; 02 §11.5 | `internal/client/credstore` | – | 1.5 | Mac, CI ubuntu, CI windows | The memory and file stores (non-Windows, mode 0600, atomic writes) pass; in `windows-go`: put, get, overwrite, delete and list under a random prefix, the blob size check and `ErrNotFound`, with `ERROR_NO_SUCH_LOGON_SESSION` mapped to a skip |
| WA22 | 2 | Wails shell skeleton | 03 DK1 (without the engine files); 03 §3, §4.5, §5 | `cmd/isshoni-desktop` (`main.go`, a minimal `shell.go`, `main_other.go`, `doc.go`, `build/windows/`), `internal/desktop/assets`, `go.mod`/`go.sum`, `web/package.json` and its lockfile, `Taskfile.yml` (`desktop:dev`, `desktop:exe:windows`) | WA04, WA05 | 3 | Mac, CI ubuntu/macos/windows, owner's PC | The app opens the SPA on the owner's PC and with `task desktop:dev` on the Mac; `CGO_ENABLED=0` windows/amd64 builds from the Mac and ubuntu; `GOOS=darwin CGO_ENABLED=0 go vet ./...` passes on ubuntu; `go mod tidy` changes none of the server's direct requirements; the `assets` tests (`bridge.js`, the CSP header and meta, no service worker, the `/wails/*` guard with 03 §5.6's allowlist: object and method from the body or `?object=`, chunked bodies refused, 64 KiB cap, every runtime object but bound `Call`/`CancelCall` refused) pass on all three OSes; the license gate and govulncheck pass for the Windows binary; 03 §3.2's rows W1–W4, W9–W13 and W19–W21 recorded in the PR under "Wails checklist" (rows that need CDP by hand on the owner's PC until WA72; W15 comes with the generated bindings in WA48) |
| WB1 | 2 | Group 1 write-backs into docs/m2 | group 1's integration and review notes (the slices' deviations and blocked items, the integration fixes); §3 items 19–26 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged, with no code change: 04 §5.2 (the header as WA03 wrote it, with 05 §4.1's `flags`, `int32_t timeout_ms` and `onset_ns`), §5.3, §5.6 (`im_read_logs`: the 8192-byte line bound, `"dropped":N`, the string bounds), §10.2 (05 §10.5's `ListAppsJson`/`SoundCheck`), §2, §13.1 (WA03's CMake options, build folders, vcpkg configuration, the `win-x64-asan` flags), §18.1 (WA03's helpers and tests), §12.2, §20 (WA02: 01 §5's names, `media.EngineStats`, no `MaxLayers`, `MonitorList` or `Engine.AudioApps`, `Config.Listed`, no GOP or audio reconfig field); 05 §5.4 (WA06's API), §13.1 (WA02's report type names); 02 §7.3, §13.2, §17 (WA05: `NetworkMatch*`, the user-code constants, the params names, `ErrNotSemVer`, `SemVer.Compare`); 01 §5.2 (`ErrAudioNotRunning`, `Unwrap`), §12.2 (WA08: the primary monitor, the measured scale, half a frame, the plausibility filter, the JSON and the fixtures); 06 §4.1–§4.6 (WA07's build as written and the `FFMPEG_REPRO` rule; the first run's numbers wait for WB2), §10.1, §17 (WA04's filters, jobs and tasks, WA08-R5's tools vet); 03 §24's encoder-fallback line; README §2, §4, §3 wording and the shared-files row. The open inconsistencies (a)–(h) of WA01's review are settled as §3 items 19–26 and written into their sections |
| WA23 | 3 | Engine session core on fakes | 04 E5; 04 §6.3, §6.4, §12.3, §18.2 | `native/windows/src/{session,engine,testhooks}/`, `native/windows/tests/` (`session_fake_test.cpp`), `internal/media/native` (`bridge.go`, `session_windows.go`, `types.go`'s `SourceDetail`) | WA10, WA11, WA12, WA13, WA15, WA16 | 3 | CI windows; Mac, CI ubuntu | `session_fake_test` (04 §18.2: each layer's first packet is `KEYFRAME\|REINIT` within 500 ms, also on a static pattern; a keyframe within one interval; `im_set_layer_active`; pause; profile switch; injected failures; ring overflow; `im_stop` ≤ 2 s with a blocked consumer; the fifth session is `IM_ERR_LIMIT`; the pick-token rules); `bridge_test` through `fakeCaller` on the Mac and ubuntu (04 §18.3); with the `win-x64-ci` DLL under goleak: `mediatest.Conformance` on `test:pattern` with `IM_ENC_FAKE` (checks 5 and 7 included) and `TestNativeIntoPublisher` (frames on both layers, a PLI answered within one frame, flash/beep offset < 45 ms); `SourceDetail` is `{Mapped, Window}` (§3 item 29) |
| WA24 | 3 | FFmpeg in `windows-native` | 06 R3b; 06 §4.1, §5.1 | `.github/workflows/ci.yml` (`windows-native`: the locked zip as `IM_FFMPEG_ROOT`, the lock staleness and DLL hash checks, the `isshoni-native-tools` artifact), `Taskfile.yml` (`ffmpeg:fetch`, `native:dist`) | WA07 (with `lock.json`), WA11 | 0.5 | CI windows; owner's PC | A `pins.json` change without a new `lock.json` fails the job; the `ci\` folder carries the FFmpeg DLLs next to `isshoni-media.dll`; `task native:dist` builds the zip; `isshoni-native-tools` also carries `ci\` with the `win-x64-ci` DLL and the FFmpeg DLLs (for `enginetest avsync`, 06 §5.1). Manual: the zip unpacks on the owner's PC (the `enginetest probe -quick` check is WA44's). As built (WA24, merged in group 9 after the FFmpeg publish, §3 items 50 and 54): the job also runs on `Taskfile.yml`-only changes (06 §10.1); `native:dist` passes its values through `NATIVE_DIST_*` and `ffmpeg:fetch` through `FFMPEG_FETCH_*` (06 §17); `release\` gets the FFmpeg DLLs and PDBs flat (06 §5.1); the `windows-native` artifact, with `ci\`, is assembled before the Go DLL tests, which use `ci\`. The `ci\` and artifact checks run in the next PR's `windows-native`; the zip test is the owner's |
| WA25 | 3 | Update manifest and signature | 06 R7; 06 §8.2, §8.3, §8.12, §11.5 | `internal/update` (`manifest.go`, `sshsig.go`, `signers.go` with the `Backup` flag), `tools/desktoppkg` (`manifest`), `tools/repocheck` (`keys`: the signers block, README rows, the OpenSSH 8.1 option rule), `deploy/desktop/min-version`, `deploy/keys/update_revoked`, `Taskfile.yml` (`release:update-keys`) | WA05 | 1.5 | Mac, CI ubuntu/macos/windows | 06 §16's sshsig and manifest tests: committed vectors verify; a flipped byte, the wrong namespace, `sha256`, an untrusted or revoked key, trailing bytes, bad armor and oversize input fail; signatures made by `ssh-keygen` verify when it is on the PATH; versions compare through `internal/version`; `FuzzSSHSIG` 30–60 s; `lint:keys` fails on a drifted signers block and on an `update_signers` with zero or two `isshoni-update-backup-<n>` lines; an active-key manifest revoking the backup is ignored and a backup-key manifest revoking the active key is persisted |
| WA26 | 3 | Go audio types and the fake's audio scenarios | 05 A2; 05 §12, §13.1, §13.3 | `internal/media` (`audio.go`: `ParseAudioReport`, the status → error table), `internal/media/testdata/audio/`, `internal/media/fake` (`audio_ctl.go`, `EngineConfig.AudioScenario`, the `Call` fields) | WA02, WA15 | 1.5 | Mac, CI | The golden reports parse; every scenario of 05 §13.3 behaves as scripted under synctest; the fake's `AudioController` works before `Start`; builds for windows, darwin and linux and the gomobile package builds pass |
| WA27 | 3 | Hub: `share.pause` and `user.connections` | 01 C08; 01 §10.3 (hub), §10.4 | `internal/server/signal` | WA05, WA18 | 1.5 | Mac, CI | 01 §14's signal row: `paused` ignored without the feature; status precedence stalled > paused > live; pausing a starting share; any connection of the same user may pause; `user.connections` on welcome, open, close, detach, room change and after a `Revalidate` that changed `DeviceName`, at most one per 200 ms, only to the same user's connections with the feature |
| WA28 | 3 | SFU: native uplink estimator and sharer pause | 01 C09; 01 §10.1, §10.3 (SFU) | `internal/server/sfu` (with `LayerUplinkStats.QueueDelay`), `internal/server/sfu/sfutest` (`FaultConn.SetInboundLink`, `NewQueueLink`) | WA18 | 2 | Mac, CI | `nativeUplink` table tests per §10.1 row; `RawLossPct` counts gaps before repair; hints ≥ 15 % and ≥ 2 s apart; the `web` and `tool` kinds keep `m1Uplink`; `sharerPaused` swallows keyframe requests and asks one per watched layer on resume; a Go publisher behind a 3 Mbit/s inbound link gets a congestion hint within 6 s; `LayerUplinkStats.QueueDelay` is sampled in the Layer's RTP loop at each frame's first packet (500 ms window minimum, baseline rising at most 0.5 ms/s) and feeds `nativeUplink`'s delay rows; `SetInboundLink` accepts `sfutest.NewQueueLink` (`SetLink` stays policer-only), and a publisher behind `NewQueueLink(3 Mbit/s, 1 s)` gets a congestion hint before any loss at the queue; M1's integrations 1–19 stay green |
| WA29 | 3 | Client core: connection, share start and stop | 01 C10; 01 §11.1–§11.5, §11.10 | `internal/client/core` (the whole §11.2 API), `internal/server/itest` (`desktop_helpers_test.go`, `desktop_share_test.go`) | WA15, WA16, WA17 | 3 | Mac, CI | itest D1 (live ≤ 3 s, both layers and audio, continuity, A/V ≤ 10 ms), D2 (one Participant, in no room outside a share), D14 (source gone), D15 (`StopOther`), D17 (goleak); 01 §14's core unit rows (the stop table, `Run` once, `Close` ≤ 3 s, `ErrNotReady`, `ConnectionID` and `EventStatus`); cookie auth through `Options.Header` until WA73 |
| WA30 | 3 | Device flow and password link in auth | 02 DA3; 02 §5, §14 | `internal/server/auth` | WA19 | 2.5 | Mac, CI ubuntu | 02 §16.1's device-flow row with a fake clock: the poll state machine (pending; `slow_down` at 3.9 s but not at 4.0 s; the 60 s cap; denied, expired, consumed; the approver disabled); idempotent and parallel decides; the 20-device cap with eviction; the four buckets (`device-code-ip48` per IPv6 /48 included) with their order and refunds, and the 5,000 pending-code cap; `PasswordDeviceLogin` with login's order and buckets (a counting hasher); the audit rows `device.approved`, `device.denied`, `device.linked` with `via` |
| WA31 | 3 | `deviceauth`: linking, refresh, servers | 02 DA10; 02 §11.2–§11.4, §11.6, §11.7 | `internal/client/deviceauth` | WA20, WA21 | 3 | Mac, CI ubuntu, CI windows | Against an httptest server with a memory store under synctest: the poll outcomes; the credential written before `linked` and before a token is returned; `save_failed`; one refresh for 100 concurrent callers; retries keeping the `exchangeId`; the `invalid_grant` notices; `PasswordLink` and `LinkWithPassword`; offline `Unlink`; the `servers.json` round trip, the cap of 20 and corrupt-file recovery; concurrent `ForgetAll`; no redirect followed; `StartLink` refuses foreign-scheme, other-host, extra-query and mismatched-code verification URIs with `ErrBadResponse`; goleak |
| WA32 | 3 | SPA platform: `DesktopPlatform` | 03 DK4 (platform part); 03 §7.1–§7.4, §7.6; 02 §11.8 | `web/src/platform/` (`types.ts`, `detect.ts`, `desktop/`: `DesktopPlatform`, the `NativeBridge` interface, `HttpBridge`, `tokens.ts`, prefixed storage, link interception), `web/src/protocol/signal-client.ts` and its test (the `auth` option) | WA22 | 2.5 | Mac, CI ubuntu | 03 §22.3's platform and signal tests, including 02 §11.8's rules (every token call is `accessToken(origin, force)` with the page's own `serverOrigin`, and an `auth.state` for another origin is ignored; no `Authorization` header without a token, one retry with a forced token after `invalid_token`, the `HelloAuthProvider` before every `hello`, no stale-build reload); `DesktopPlatform` ≤ 4 KB gzip in the entry chunk (`check:size`); the browser e2e suite green without changes |
| WB2 | 3 | Group 2 write-backs into docs/m2 | group 2's integration and review notes (the slices' deviations and rejected items, the integration fixes, PR #38's CI fixes), the FFmpeg workflow's first complete run; §3 items 27–31 | `docs/m2/*.md`; `docs/m1/01-protocol.md` §15.2 (one line) | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA10-R7's five corrections (this table's WA10 row: `dependency`/`abi`; 04 §12.1: QPC through kernel32's `NewLazySystemDLL`; 04 §12.2: `IM_ERR_AUDIO_UNAVAILABLE` → `audio_unavailable`; 04 §12.4: `runtime.Pinner`; 01 §7.1 item 2: `im_test_*` required); 04 §5.5 and 03 §13.4 (item 27); 04 §13.1 and 06 §5.1 (the `/DEPENDENTLOADFLAG` verdict awaits the group PR's `windows-native` run); 06 §5.1 and §16 (dll-check reads the PE itself, its extra checks, synthetic PE fixtures); 06 §5.2 (`syso.go` instead of `wails3 generate syso`, the ignored `.syso`); 03 §5.4, §22.4 B and W13/W19 (Wails overwrites five `WEBVIEW2_*` variables in every build; the e2e CDP port through `AdditionalBrowserArgs`, WA72); the vcpkg overlay triplet (04 §13.1, 06 §3, §5.1); 01 §6.4 (`SkipBitrate`; the Decodable run skipped in coverage builds, round 3 having removed `Slack`; checks 5 and 7 on `IM_ENC_FAKE`); 04 §18.1 (`opus_stage_rt_test` under ASan/TSan, `test_tone_test`, the golden Marker file and `markergolden`); the slices' deviations that change a spec (WA11–WA13 in 04, WA14 in 05, WA15–WA18 in 01, WA19–WA21 in 02, WA22 in 03 and 06); items 28–31 in their sections; 06 §4.6 carries the first FFmpeg run's measured results (image, VS, toolsets, `cl` build, SDK, MSYS2, the byte-identical DLLs and source archive, the zip differing only in the two PDBs); M1 01 §15.2 names `internal/version` instead of `semver.go` |
| WF1 | 3 | FFmpeg: record and compare the compiler build, not only the toolset folder | §3 item 30; 06 §3, §4.1, §4.3, §4.5, §4.6 | `native/ffmpeg/build.sh`, `native/ffmpeg/README-LGPL.txt`, `tools/desktoppkg` (`ffmpeg.go`), `.github/workflows/ffmpeg.yml` | WA07 | 0.5 | Mac; GitHub (the FFmpeg workflow) | `build.sh` reads `cl.exe`'s and `link.exe`'s banner builds in preflight (a pipeline without an early-exiting reader under `pipefail`; an empty value fails the build) and records them as `msvcCompiler` and `msvcLinker` next to `msvcToolset` in `build-manifest.json`, `result.json`, `lock-candidate.json` and `CONFIGURE.txt`; `desktoppkg`'s `Lock` requires both; `report`'s strict and warn branches compare the toolset folder and both builds, for the rebuild and the lock alike, and the summary shows all three; `bash -n`, `shellcheck`, `shfmt -i 2 -d`, `actionlint` and `go -C tools test ./desktoppkg/...` pass. The Windows build itself runs in the group PR's FFmpeg workflow; the owner dispatches its publish only after WF1 has merged, so `lock.json` carries both builds |
| WA33 | 4 | GPU path | 04 E6; 04 §8 | `native/windows/src/gpu/`, `native/windows/shaders/`, `native/windows/tests/` (`gpu_warp_test.cpp`), `deploy/windows/dll-imports.txt` (append) | WA23 | 2.5 | CI windows (WARP); owner's PC | `gpu_warp_test`: the shader against a CPU BT.709 limited-range reference within 1 code value on flat areas and 2 on edges; the VideoProcessor within 2 of the shader when WARP offers one; letterbox, rotation 90/180/270, cursor shapes against golden images, `IM_START_HIDE_CURSOR`; the GPU test pattern renders. Manual: on the owner's PC the VideoProcessor matches the shader within 2 on each available GPU vendor |
| WA34 | 4 | Engine audio bridge | 04 E13; 04 §10.1, §10.2 | `native/windows/src/audio_bridge/`, `native/windows/tests/` (`audio_bridge_test.cpp` with a fake `AudioEngine`) | WA13, WA23 | 1.5 | CI windows | `audio_bridge_test` (04 §18.2): `im_init`/`im_shutdown` call `Init`/`Shutdown`; every audio export forwards its arguments (`shared_pid` copied); `exclusions` → `SetRules` before `Mixer::Start`; mode OFF creates no mixer; `im_set_paused` → `Mixer::SetPaused`; the fake mixer's chunks come out as Opus every 20 ms and, with `IM_AUDIOF_TAP_PCM`, as PCM every 10 ms; audio-only sessions; mute and pause keep silent frames flowing |
| WA35 | 4 | Windows audio watcher (no capture) | 05 A5; 05 §5.6, §5.7, §7, §12, §13.2 | `native/windows/src/audio/` (process table, endpoint and session notifications, meters, mic detection, device notifications, the watcher half of `AudioEngine`), `native/windows/tests/` (watcher tests), `internal/media/native` (`audio.go`; an `audio_windows.go` only if WA35 needs Windows-only helpers, 05 §13.2; the `AudioTestHooks` wrappers; `bridge.go`'s PCM path, now `KindPCM` packets from `Next`, and its start-time audio mapping folded into `audio.go` with 05 §13.1's NUL and length checks; `types.go`'s `PCMChunk` removed, §3 item 32), `deploy/windows/dll-imports.txt` (append) | WA11, WA14, WA23, WA26 | 2.5 | Mac, CI ubuntu, CI windows; owner's PC | Caller-fake tests of the Go side on the Mac and ubuntu; on `windows-native`: process-table tests against spawned children (creation times, image folders, `--user-data-dir`), 100 watcher start/stop cycles with the handle count back to baseline, the no-device state, `IM_INIT_NO_AUDIO`, `im_caps.audio_flags`; the binding's single mode mapping (`AudioEverything` and `AudioOff` at start, and `AudioEverything` in `SetAudioMode`, → `CodeInvalid` with no call, §3 item 15). Manual, right after group 6 with WA62a's tool: `isshoni-audiotest apps` gives the right verdicts for Discord, a browser and a Steam game |
| WA36 | 4 | Audio mixer core | 05 A4; 05 §9, §10.3, §10.4, §17 | `native/common/audiomix/` | WA03, WA26 | 3 | Mac, CI | The drift simulation (±500 ppm, jittered 10 ms bursts, 2 simulated hours): ring fill 40 ± 15 ms, zero underruns and trims, resampling ratio standard deviation ≤ 30 ppm; chunk times exactly on the 10 ms grid with 05 §10.4's `OnGap` rules; S1's click detector finds no click on any fade path, the onset fade included, also for a new stream with 30 ms of silence before its tone; the PI controller never emits ppm = 0; no allocation on the mixer thread after `Mixer::Start` (Linux `host-debug`); chunks exactly 10 ms apart with sound from t = 0; the C++ report writer matches WA26's goldens byte for byte |
| WA37 | 4 | Desktop audio policy service | 05 A8; 05 §14 | `internal/desktop/audio` | WA06, WA26 | 2.5 | Mac, CI ubuntu/macos/windows | 05 §19.1's Go tests (the rules builder, prompts with `Decide` limited to keep-out/include and `DismissPrompt`, the viewer browser per connection id, banners and tips, `FriendsHear` sorting, polling rates under synctest, the errors); the lifecycle test against the fake (watch, start, prompt, decide, pause, stop) under goleak; `RunSoundCheckHelper` stubbed off Windows |
| WA38 | 4 | Client core: recovery | 01 C11; 01 §11.6, §11.7 | `internal/client/core` (`pubdriver.go`, the recovery paths), `internal/server/itest` (`desktop_recovery_test.go`) | WA29 | 2 | Mac, CI | itest D3 (socket killed: resume, viewer gap < 500 ms, no republish), D4 (UDP black hole 5 s: ICE restart, rebuild when needed, viewer resumes on a keyframe), D5 (server restart: republish with `replaces` ≤ 5 s, a new `ConnectionID`), D6 (hold expiry at 30 s ± 1 s, engine stopped) |
| WA39 | 4 | Device tokens in auth | 02 DA4; 02 §6 | `internal/server/auth` | WA30 | 2 | Mac, CI ubuntu | Rotation, reuse revocation and every grace case (the same `exchangeId` after 10 min, no id at 100 s once, at 121 s, another id); bearer shape and expiry checks without a DB read; the 30 s cache invalidated before `ConnCloser`; `Touch`, `RenameDevice`, `RevokeOwnDevice`, the bearer cases of `RevokeDevice` and `LogoutEverywhere`; the `refresh_token_reused` alert; a grace re-issue keeps the replaced child as a spent row, and presenting it afterwards (at +101 s, inside the window, and at +15 min) gives `invalid_grant {reason: reused}`, the device gone, `device.refresh_reused` and the alert; M1's revocation matrix green |
| WA40 | 4 | Updater | 06 R8; 06 §8.4–§8.11 | `internal/update` (`update.go`, `fetch.go`, `state.go`, `apply_windows.go`, `policy_windows.go`, `apply_other.go`, the `updatetest` hooks) | WA25 | 2.5 | Mac, CI ubuntu/macos/windows | 06 §16's `update_test` (the schedule; `Enabled` false makes zero requests; the redirect rule; timeouts; hash and size mismatch; the disk check; atomic `state.json`; `PendingAtStartup` and the attempts guard; `ErrNotInstalled`; the beta listing and rate limits), `apply_windows_test` (exactly `/S /UPDATE /RELAUNCH`; `Apply` returns once `Local\io.isshoni.desktop.installing` exists, and nil with a WARN after 10 s) and `policy_windows_test`, under goleak; a release build contains no test-hook strings |
| WA41 | 4 | Desktop Go foundations and the process contract, part 1 | 03 DK3; 03 §8.1, §13.1–§13.4, §14, §17, §18.1 | `internal/desktop/{settings,applog,i18n,hotkey,servers,app,winos}` (the App skeleton and `OSHooks`, single instance, the running mutex), `cmd/isshoni-desktop` (main's first action, the DLL search order of 03 §13.4; main's §13.4 order, `--version`, `--updated-from`, the `--open-url` rule, the Pending values, `engine_release.go`, `engine_fake.go`), `web/desktop_catalog.go` (`web.DesktopCatalog()`, in the `web` root package), `web/src/i18n/lazy/desktop.en.json` and `desktop.ts` | WA10, WA15, WA20, WA22, WA31 | 3 | Mac, CI ubuntu/windows | 03 §22.1's rows for `settings` (atomic writes, `.bak`/`.bad` recovery, migrations), `applog`, `i18n`, `hotkey`, `servers` and `app` (link handling, the Pending values) pass on all OSes; on CI windows: W6 (a second launch forwards `--open-url` and exits 0), the mutex, `--version` (one JSON line with `engineAbi` 1, the ABI major, no window), and 03 §22.2's marker test (after main's first action, `im-marker.dll` in the current directory and on `PATH` doesn't load); a `PendingServer` value opens the "Add server" step without a network request |
| WA42 | 4 | SPA boot, routes and guards for the desktop | 03 DK4 (app part); 03 §7.2, §7.5 | `web/src/app/` (the boot desktop branch, `buildVersion: ''`, every M2 route, `RequireDesktop`, `RequireBrowser`, the desktop `RequireAuth`), `web/src/desktop/` (stub pages), the `role` audit's fixes in M1 folders (named in the PR), among them `web/src/rooms/`'s call of `platform.desktop.signalStopped()` when C1 stops (redial, relink, version, wait; 03 §7.3, WA32's leftover) | WA32 | 2 | Mac, CI ubuntu | 03 §22.3's router and chunk tests; `check:size` passes; desktop routes never load in a browser and `/link` never in the desktop; the browser e2e suite green unchanged |
| WA43 | 4 | Latency proxies | 01 C16; 01 §12.4 | `internal/server/itest` (`desktop_latency_test.go`), `web/e2e/latency.spec.ts` | WA16, WA29 | 1 | CI ubuntu, Mac | D16: last-packet latency p95 < 60 ms and max < 200 ms over 20 s (Synthetic 1080p60 at 8 Mbit/s; under `-race` except on Windows); `latency.spec`, its legs less Chrome's lip-sync hold (01 §12.4): median `expectedDisplayTime − captureTime` < 150 ms and p95 < 250 ms, or median `expectedDisplayTime − receiveTime` < 100 ms with an annotation when `captureTime` is absent; the hold's p95 < 400 ms; the share's A/V offset by its SRs (from `estimatedPlayoutTimestamp`, against NetEq + output and the receive → display leg) at a median under 60 ms; values logged |
| WB3 | 4 | Group 3 write-backs into docs/m2 | group 3's integration and review notes (the slices' deviations and rejected items, the integration fixes, the open issues), PR #38's and #39's CI findings, the FFmpeg workflow's WF1 run; §3 items 32–34 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA23 (04 §5.3's `im_start` source rules and `im_set_audio_bitrate` without audio, §6.1–§6.4: the full command queue, controls after `ENDED`, event-queue overflow logged and `RING_OVERFLOW` at most once per stream and second, re-init spacing without the first open, MMCSS through a run-time `avrt.dll`, the CPU test pattern and the `AudioEngine` wiring; §9.1's encoder catalog and `probeFallback`; §12.2: `SetBitrate(0)` is `invalid`, `Pick`'s `self_window` rule, no second `EventLayer` for an unchanged first `REINIT`; §18.2's `TestNativeIntoPublisher` as built, the offset on `f`; the WA70 row); WA25 (06 §8.2: `ci` for any prerelease but `rc.N`, `min-version` from `0.2.0-rc.1`, the extra parser and writer checks, `-releases`; §8.3: `repocheck update-keys`, the `update_signers` rules, LF checkout; §8.9's split with WA40, `Error`/`CodeOf`; §8.11's hooks; §7.2's rotation without `valid-before`; §10.1's filters; §16); WA26 (05 §13.1: `ParseAudioReport`'s errors, the status table's home, the binding's NUL and length checks, `AudioApp`'s order; §13.3: `SetAudioStatus` on the Engine, verdicts from the current rules, `AudioOff` silencing, the scenarios' choices; 01 §6.2: `Call.Mode`, `Call.Muted`, `"audio_mode"`); WA27 (01 §10.3: `paused` also needs the feature on the publisher, a starting share keeps its start timeout, `sfuplane` forwards `Paused`); WA28 (01 §10.1: the unpause grace, ticks without a verdict, the empty-tick baseline, the hint reasons, the reader goroutine, `TestNativeUplinkQueue`'s phases and window; §14); WA29 (01 §11.2–§11.4, §11.7, §11.9: Publisher layers, ratectl's timing, the stop table and `EndInfo` reasons, the finish order, the placeholders until WA38, the exported constants); WA30 (02 §3, §5.2–§5.6, §6.8, §14: the cap inside the `Write`, the refunds, idempotent decides, device names, the not-implemented parts, the notifications; WA30-R2 in §5.4 and §16.1); WA31 (02 §11.2–§11.4, §11.6; WA31-5 in §11.3's `AccessToken` comment, §16.1, §16.2, §17, §18 and 01 §11.2, §16); WA32 (03 §5.5's lint rule, §7.1: `Platform.desktop` optional, `PickedSource.preview` with WA65, the extra `DesktopFeatures` members; §7.3: `canShare`, `sharing`, `apiFetch`'s 401, the `SignalClient` rules, `signalStopped`; §7.4: the `WailsBridge` stub, the `HttpBridge` wire, the hand-written DTOs; §16.3: `updateApp`'s outcome); WB2's leftovers (§3 item 29 and 04 §12.2's `SourceDetail` bullet match the trimmed struct; the `/DEPENDENTLOADFLAG` verdict "not honored" in 04 §13.1, 06 §5.1 and §3 item 34; the first real-DLL `dll-check` and ctest results in 04 §18.2; `credstore`'s one-call-at-a-time rule in 02 §11.5; 01 §6.4 orders a call's answers by the reader's sequence, with no `Slack`, and §14 names `fakeServer.settledTypes`; WF1's `msvcCompiler`/`msvcLinker` values in 06 §3, §4.1, §4.6); §4's Windows test lessons; §3 items 32 (PCM as `KindPCM` packets through `Next`; 04 drops `PCMTap`/`PCMChunk`), 33 (no hint counter or dashboard cap in M2; 01 §10.1, §10.2) and 34 in their sections; the rows of WA35, WA42, WA46, WA50, WA65 and WA70 carry what group 3 handed them; §8's FFmpeg publish line says it runs once WF1 has merged |
| WA44 | 5 | libavcodec encoders and the probe | 04 E7; 04 §9.1–§9.5, §9.10, §13.3, §14 | `native/windows/src/encode/` (`ffmpeg_api`, `ffmpeg_encoder`, `selector`, `probe` with its live-bitrate check, 04 §9.10), `native/windows/tests/` (`ffmpeg_load_test.cpp`, `mf_sw_test.cpp`, and `audio_bridge_test.cpp`'s `ProbeFlags` case), `native/common/tests/testdata/aus/`, `cmd/isshoni-enginetest` (new); as run (group 10): `.github/workflows/ci.yml` (`windows-native`: where the FFmpeg DLLs sit for the in-job tests) | WA24, WA33 | 3 | CI windows; owner's PC | `ffmpeg_load_test` (the version checks; an empty folder gives `IM_ERR_DEPENDENCY` `missing` while an audio-only tone session still works; another major gives `version`) and `mf_sw_test` (or a logged skip) in CI; `audio_bridge_test`'s `ProbeFlags` case: `encode::Probe` fills `im_caps.audio_flags` from `audio_bridge::Engine().ProbeFlags()` (04 §9.10; group 4's hand-over); `dll_search_test`'s vendor-runtime and libvpl part (marker DLLs named after the vendor runtimes and libvpl's runtimes, with `ONEVPL_*` pointed at them, never load during a full probe) and the Go process-default check of `isshoni-enginetest`'s startup in CI; the committed AU fixtures feed WA12's `annexb_test`. Manual (owner's PC, both OSes): the full probe < 2 s; both `capture` runs per available encoder pass 04 §14's assertions at 1080p60 (first keyframe ≤ 500 ms; `-bitrate-steps`: rate within ±25 %, no `KEYFRAME`/`REINIT` between steps for a live encoder, exactly one `REINIT\|KEYFRAME` per applied step for AMF and MF; if QSV emits IDRs, the probe's live check clears its flag and the run passes as re-init); `bench` layer `f` encode p99 ≤ 16 ms; the tools zip's `isshoni-enginetest probe -quick` runs; 04 §9.3's settings and layer 0's VBV comparison (0.5 s against 0.1–0.2 s) in the PR under "Results for the spec" (`WB5` writes them back). As run (group 10, after the FFmpeg publish, §3 item 50): FFmpeg is `deps-ffmpeg-9.0.2-r1` (`avcodec-63`, `avutil-61`; `task ffmpeg:fetch` works on the Mac); WA24's review already put the FFmpeg DLLs next to the `ci\` test DLL before the Go DLL tests (§3 item 54), so `ffmpeg_load_test` and the in-job Go tests find FFmpeg; the hardware "Manual" items stay the owner's (§8), and a later write-back slice copies "Results for the spec". As built (merged in group 10, §3 item 55): the `ProbeFlags` check is in `ffmpeg_load_test.cpp`, whose child process also forces the vendor opens that a GPU-less runner's probe never plans, so the vendor-runtime and libvpl markers are really exercised (04 §18.2); the probe's encoder budget is 1500 ms with a 1280×720 retry, and a quick probe reuses a stored full probe (04 §9.10); the committed AU fixtures are VideoToolbox streams from the Mac (04 §14, §18.1); `EncoderCatalog()`, `EncodedAu`'s capture time, the per-layer GOP for `-bitrate-steps` and `probeFallback`'s removal were outside its paths and are WF10's, so the hardware "Manual" items and the Windows fixtures wait for WF10 (§8). Its CI checks (`ffmpeg_load_test`, `mf_sw_test`, `TestDLLSearchMarker`, `TestProbeWithDLL`) run first on `windows-native` and `windows-go` |
| WA45 | 5 | Windows audio capture and mix, part 1: the include set | 05 A6 (part); 05 §6.2, §6.3, §8, §9, §10.5 | `native/windows/src/audio/` (activator threads, held `SYNCHRONIZE` handles, the commit check, process-loopback streams, the multiplexed MMCSS capture thread, the include set, the `Mixer` feeding 04's `PcmSink` through `im_audiomix`, `NewMixer`), `native/windows/tests/`, `native/common/audiomix/` (only to remove its report writer and `mix_report_test` if WA35's writer is the one kept, §3 item 36); as run (group 10): `native/common/audiomix/` also for the tail fade beside WA36's onset fade (`mix_engine.cpp`, `constants.h`, `channel.h`, `mix_engine_test.cpp`) and the report writer's `tailFades`/`tail_fade` (05 §12.1); the Go goldens and `AudioCounters` go through the integrator (§3 item 53) | WA09, WA34, WA35, WA36 | 2.5 | CI windows; owner's PC | The DLL builds; tests with fakes on `windows-native` for the activation timeout and the commit check (no INCLUDE on a tree that holds an excluded process, re-validated at commit). Group 4's hand-overs: **one C++ report writer and one golden test remain** (§3 item 36: WA35's `audio/report.cpp` or `im_audiomix`'s, the other removed); the two-line `PcmSink` → `audiomix::ChunkSink` adapter, the Windows `TickClock` and MMCSS through `MixerOptions::thread_hook`, and the report and counter mapping (05 §10.5); the `Mixer` copes with destruction without `Stop` after a failed `Start`; `audio/mixer.h`'s header comment says the session, not `audio_bridge/`, wires the `Mixer` (04 §10.2). Manual (owner's PC, after the group, with WA44's tool): `isshoni-enginetest capture -audio mix` with Discord in a call and a browser playing gives a playable `audio.ogg` with the browser and without Discord (04 E13's PC acceptance). As run (group 10; WA09 done on CI, §3 items 51–53): the mixer also applies the tail fade beside WA36's onset fade (the S1 spike's `spikes/s1-win-audio/native/engine.cpp`, 05 §9.5), counted in the mixer's `tail_fades` with a `kTailFade` event (the report's `tailFades` and `tail_fade` are WF11's, §3 item 56), with 05 §19.1's tail-fade unit tests on synthetic input that run on the Mac and in CI (no step above S1's click threshold for a tone cut on a 10 ms boundary, one ending inside a chunk, or a ramp straddling two ticks, counted once; a gap shorter than 5 ms and a loud stream unchanged sample for sample; no added latency). As built (merged in group 10, §3 items 56–57): `im_audiomix`'s writer is the one kept (WA35's removed; `audio/report.cpp` maps the planner's vocabulary), with `AppWarning::kInstanceGone` for the top-level `instance_gone` (05 §12.1); the watcher logs the tail fades and the `kTailFade` event instead of reporting them until WF11; the planner runs without `IM_EXCLF_FAST_PATH`, as if process loopback works (the fast path, the endpoint engine and the startup probe are WA53's); a failed stream is reopened after 100 ms, 500 ms and 2 s before it gives up (05 §8.5). The DLL build and the tests with fakes run first on `windows-native`; the manual `capture -audio mix` run waits for WF10, since `capture` needs a video encoder |
| WA46 | 5 | Client core: hints, codec switch, pause, encoder fallback | 01 C12; 01 D13–D15, §11.8 | `internal/client/core` (`hints.go`), `internal/server/itest` (`desktop_hints_test.go`) | WA27, WA28, WA38 | 2.5 | Mac, CI | itest D7 (codec switch through `Reconfigure` and a re-offer, back to High 60 s after the CB viewer leaves), D8 (layer pausing with `SetLayerActive`), D11 (pause: no media within 50 ms, `paused` and `audio_muted` in `Calls`, a late joiner gets no frame, resume order and a keyframe ≤ 500 ms, a pause from a web-role connection reconciled), D12 (a preset from another connection), D13 (encoder changed, then lost; the core's `EventEncoderChanged` handling and snapshot updates came with WA29, 01 §11.5, so D13 verifies them); group 4's hand-over: `paused` and the preset are re-sent after a republish (01 §11.7), with a test |
| WA47 | 5 | Device REST routes | 02 DA5; 02 §6.4, §7 | `internal/server/httpapi` | WA39 | 2 | Mac, CI ubuntu | 02 §7.1's endpoint table (#44–#49, #52, `/device/password`, `PATCH /me/devices/{id}`); the credential matrix (5 access levels × none, cookie, bearer, bad bearer, both); the CORS matrix (desktop origins only with bearer, no `Allow-Credentials`, headers on error answers, preflight cached 600 s); `/info` lists `deviceFlow`; an admin's bearer on any `/api/v1/admin/*` route → 403 `forbidden {credential: session}`, while #21–#23 accept it; `TestAuditCoverage` and `TestAuditActionsInSource` pass with the new rows |
| WA48 | 5 | Bridge services and the desktop e2e harness | 03 DK5 (with DK4's Wails bridge); 03 §6, §7.4, §22.4 A | `internal/desktop/bridge` (every §6.2 method, one file per service; events; error JSON) and `bridge/codes`, `internal/desktop/e2ehost`, `cmd/isshoni-desktop` (`shell.go`: service and event registration), `web/src/platform/desktop/` (`WailsBridge`, `bindings/`, `codes.gen.ts`), `tygo.yaml` (`bridge/codes`), `web/src/i18n/lazy/desktop.en.json` (`desktop.errors.*`), `web/e2e/fixtures.ts`, `web/e2e/desktop-security.spec.ts`, the CDP config and spec skeleton under `web/e2e/` | WA21, WA31, WA41, WA42 | 3 | Mac, CI ubuntu; owner's PC | `desktop-security.spec` passes in Chrome on the Mac and in CI (the CSP, `/sw.js` 404, external links open the system browser, no `isa_` in storage, a foreign-Origin `/wails/runtime` gets 403, the runtime's own objects of W21 refused from the page: `Browser.OpenURL('ms-settings:…')` and `('search-ms:…')`, `Clipboard.Text()`, `Window.Hide()` → 403, `/link` opens the browser); the bridge unit tests pass; W11, W12 and W15 recorded in the PR under "Wails checklist"; the generated bindings are committed and regenerate without a diff; the CDP skeleton attaches to a `TAGS=e2e` build on the owner's PC and checks W1–W3, W5 and W7 (in CI from WA72) |
| WA49 | 5 | `install-desktop.ps1` | 06 R10 (its Windows run is WA72's); 06 §7.1–§7.3, §11.8 item 7(b) | `deploy/install-desktop.ps1`, `deploy/test/install-desktop.Tests.ps1`, `tools/repocheck` (`keys`: the ps1 key block), `Taskfile.yml` (`lint:ps`, `test:ps`, `release:prepare` stamping), `.github/workflows/ci.yml` (`lint-deploy` runs `lint:ps` and `test:ps`) | WA25 | 1.5 | Mac, CI ubuntu; owner's PC | Pester passes under `pwsh` (the checksum lookup, version regex, quoting helper, `ssh-keygen` discovery mocked, the exit messages; Smart App Control state 1 gives the message and no download, 2, 0 or missing proceed; a throwing `Start-Process` gives the `isshoni: Windows blocked the installer (…)` line); PSScriptAnalyzer at Warning and the ASCII check clean; no `exit` in the script; `lint:keys` fails when the script's key block differs from `deploy/keys/allowed_signers`. Owner (clean Windows 10 22H2): the default `SecurityProtocol` of a fresh `powershell.exe` and the two .NET registry values recorded in the PR, which fixes the `ps1Command` form (06 §7.5) |
| WB4 | 5 | Group 4 write-backs into docs/m2 | group 4's integration and review notes (the slices' deviations and rejected items, the integration fixes, the open issues); §3 items 35–38 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA36 (05 §6.2, §6.4, §9.2–§9.6, §10.3: the 62 500 denominator, the head-age control and its fill floor, Kp 100 / Ki 2.5, the onset before the resampler, the shaped tail, one group per engine with one takeover, the backlog dropped at the join, the shorter cut fade; §12.1's `warnings` cap; §17, §19.1, A4), WA34 (04 §10.2: the mixer wiring stays in `session/`, what `audio_bridge/` holds, `im_test_opus_decode`'s rules; 05 §10.5), WA33 (04 §4's tree with `gpu/test_pattern`, `pattern_layout`, the converter files and a test's own support files, §8.1–§8.3, §13.1's `.hlsli` dependency, §18.2), WA35 (05 §4.3, §7.4, §11.1, §12.1's one-shot snapshot, §13.1–§13.2; the "until WA35" `PCMTap` parentheticals of 04 §12.2 and 05 §13.2 dropped), WA37 (05 §5.9, §14.1–§14.7: `SetVisible`, `ErrClosed`, `SoundCheckHelperFlag`, `DefaultSettings`, the settings JSON, the debounce, viewer tabs across shares, `included-uses-mic`; 03 §4.4's stale `05 §12.4`), WA38 (01 §11.4, §11.6–§11.7: no trickle, the 15 s deadline for rebuilt and republished PCs, the owed rebuild, the republish rules; WA29's placeholders gone), WA39 (02 §6.1–§6.4, §6.8, §8, §2.1's layout and the DA4 and DA6 rows: `TouchPrincipal`, `RefreshReusedError`, the grace paths' trim and touch, the 422 order, bearer `RevokeSession` and `Logout`, the `via` values; WA39-R2 and R3), WA40 (06 §8.4–§8.8, §8.11), WA41 (03 §3.2's W6, §6.1, §6.3's `PendingLink`, §13.3, §13.4's System32-only flags and the modes not yet built, §14.2, §17), WA42 (03 §7.2 step 4 and the incompatible server, §7.5, §7.6's role audit, §18.1), WA43 (01 §12.4) and the integration fixes (06 §10.1's `native` filter and `updatetest` step, 04 §13.1, 02 §8's audit rows, 03 §18.1's `READ_WHOLE`); §3 items 35–38 in their sections; §4's Windows lessons from group 4; the rows of WA44, WA45, WA46, WA52, WA56, WA65, WA71 and WA73 carry what group 4 handed them; this table gains WB4's and WF2's rows |
| WF2 | 5 | The GPU path in the session | §3 items 37–38; 04 E5, E6; 04 §6.1, §8; group 4's WA33 notes (WA33-1) | `native/windows/src/session/` (`session_object.{h,cpp}`, `encoder.h` and what else the wiring needs there), `native/windows/tests/` (`session_fake_test.cpp` and any new session test), `tools/desktoppkg` (`dllcheck.go`: the load-config message) | WA23, WA33 | 1 | Mac; CI windows | `test:pattern` in release builds is WA33's GPU test pattern through gpu/'s `Device`, `FrameSlots` and `Converter` (`Session::StartVideo`, the supervisor's pattern branch, the video loop and the encode input), and the session's CPU frame-time pattern (`frame_ts_`, `kFrameSlots`) is gone; `IM_ENC_FAKE` sessions stay off WARP so `session_fake_test`'s timing cases keep their bounds; `session_fake_test` and `gpu_warp_test` pass in `win-x64-ci` (CI windows), and the portable parts build and pass on the Mac; dll-check's load-config line no longer says "README WA11's verdict decides 06 §5.1's check" (the verdict was "not honored": no check, §3 item 38) and its test is updated; `lint:native` clean |
| WA50 | 6 | Windows.Graphics.Capture and the picker | 04 E10; 04 §7.1–§7.3, §7.6–§7.9 | `native/windows/src/capture/` (the picker's STA thread with `IInitializeWithWindow`, pick tokens (`capture::ReleaseSource`, which `im_release_source` forwards to, calls `session::Sources().Release(id)`, and real picks register with `Sources().AddPick`: WA23's hand-over, since WA11's no-op stub keeps a released fake pick valid until it expires), window identity, WGC with borderless, `MinUpdateInterval`, `IncludeSecondaryWindows`, cursor, resize, minimize, close), `native/windows/tests/` (`window_match_test.cpp`), `native/windows/third_party/cppwinrt/`, `cmd/isshoni-enginetest` (`pick.go`), `deploy/notices/native-windows.json` (append) | WA44 | 3 | owner's PC; CI windows | `window_match_test` in CI. Manual (04 §18.4): M-PICK (both OSes), M-WGC-WIN, M-WGC-MON and M-SECONDARY (Win11). As run (group 11, §3 item 59): WA44's probe decides `IM_OS_WGC`, `IM_OS_WGC_PICKER` and the other WGC flags from the build number and leaves `IM_OS_WGC_BORDERLESS` clear (`encode/probe.cpp`'s "README WA50" notes; 04 §9.10's "OS flags"). WA50 provides the C++/WinRT checks (`GraphicsCaptureSession::IsSupported`, the property checks and `RequestAccessAsync(Borderless)`, as 04 §9.10 step 1 says) as a function in its own folder (e.g. `capture::WgcOsFlags()`) and does not edit `encode/`; `probe.cpp`'s `BuildFlags` calls it in a follow-up after group 11, because WF10 owns `encode/` in this group (§3 item 55); `pick.go` joins WA44's `cmd/isshoni-enginetest`. The manual items need a real encoder for a `capture -source pick`, so they follow WF10. As built (merged in group 11, §3 item 61): the picker, its tokens, window identity and `capture::WgcCapture` with the trackers of `capture/source_watch.h` (`MinUpdateInterval100ns`, `ResizeTracker`, `MinimizeTracker`) and `capture::OsFlags()`; `pick_test.go` beside `pick.go`; `monitor_id` empty until WA60; the hidden owner window shown without activation only while its pick is open (04 §7.2, §7.3). Not wired: the session doesn't start `WgcCapture` and the probe doesn't call `OsFlags()`, so a real pick captures nothing yet; WF12 (group 12) wires both, and M-WGC-WIN, M-WGC-MON and M-SECONDARY wait for it, while M-PICK can run once the FFmpeg r2 lock is committed (§3 item 64, §8). As of group 12's merge both happened: WF12 merged (§3 item 67) and the r2 lock is committed (§3 item 69), so all four can run |
| WA51 | 6 | OpenH264 at run time | 04 E8; 04 §9.9, §12.5 | `native/windows/src/encode/` (the OpenH264 backend's files, registered through WA44's selector table), `native/windows/third_party/openh264/`, `native/windows/tests/` (`openh264_test.cpp`), `internal/media/native` (`openh264.go`), `cmd/isshoni-enginetest` (`openh264.go`), `deploy/notices/native-windows.json` (append) | WA33, WA35, WA44 | 2 | Mac, CI ubuntu/windows; owner's PC | 04 §18.3's OpenH264 helper tests on the Mac and ubuntu: short, long and oversized bodies rejected with nothing left behind, and a bzip2 bomb stopped after `Size + 1` bytes with the heap growing < 16 MB; `openh264_test` with Cisco's DLL, hash checked: encode/decode PSNR ≥ 35 dB, profile `42e0`, SPS and PPS before every IDR, a live bitrate step within ±25 %; a tampered download is rejected and leaves nothing behind; a wrong version gives `IM_ERR_DEPENDENCY` `version`; the Go pin equals `codec_ver.h`. Manual: M-OPENH264. As run (group 12, §3 item 65): WA44's selector table and WF10's catalog wiring are merged, so the OpenH264 entry reaches sessions through `encode::CatalogEntries()` like the libavcodec ones (`session/encoder.h`); in this group `encode/` is shared with WF12, which changes only `probe.cpp`'s `BuildFlags` (§5's shared files) As built (group 12, §3 item 66): the pin is 2.7.0 over HTTPS (975 352 bytes, SHA-256 `0ef5e7f3…c288`), the Go helpers' Reasons `download` and `corrupt` are in 04 §5.7, and its deviations are accepted; the Windows checks (`openh264_test`, the MSVC build) and M-OPENH264 run on the merged tree's first `windows-native` run and the owner's PC |
| WA52 | 6 | Engine pause, stats and log privacy | 04 E14; 04 §5.6, §8.5 | `native/windows/src/session/` (pause, the stats JSON, the info-level log rule, the `IM_INIT_NO_AUDIO` start check of §3 item 35), `native/windows/tests/` | WA23, WA34, WA44 | 1.5 | CI windows | With the luma tap, no packet after pause plus one frame interval shows new content; audio ramps down in 10 ms, silent frames continue at 50/s and the PCM tap is digital zero; un-pausing keeps a user mute set with `im_audio_set_muted`; after `im_init(IM_INIT_NO_AUDIO)`, `im_start` with `IM_AUDIO_MODE_EXCLUDE_APPS` → `IM_ERR_BAD_STATE` while the test tone still starts (§3 item 35, 05 §4.3); the stats JSON has every 04 §5.6 field; a log-capture test finds no window title or process name at info level. As run (group 12, §3 item 65): WF12 changes `session_object.{h,cpp}` in the same group for the WGC capture, so both keep their changes focused and the integrator resolves the file (§5's shared files) As built (group 12, §3 item 70): group 12's integration wired `im_audio_set_muted` to the tone's mute (05 §4.3) and made `engine.cpp`'s audio-init warning log only the token; the Windows tests run on the merged tree's first `windows-native` run |
| WA53 | 6 | Windows audio, part 2: fast path, endpoint engine, probe, sound check | 05 A6 (part); 05 §6.4, §6.5, §8.5, §11 | `native/windows/src/audio/`, `native/windows/tests/` | WA45 | 2 | CI windows; owner's PC | The DLL builds; tests with fakes: the fast path exits within 15 ms of a voice app's `session_created` and cuts retroactively at C − 20 ms, the kill switch works, the endpoint engine follows default-device changes, a failed startup probe falls back with the banner, the mode, mute and pause flags, the silent-stream health check, the test exports. Hardware: right after group 6 merges, the integrator runs WA62a's cases on the owner's PC (WA62a's acceptance); a failure becomes a `WF` slice that lists `native/windows/src/audio/`. As decided (§3 item 52): on builds below 22000 the planner picks the endpoint engine with the banner (05 §6.1, §15), and a test with a faked build number checks it. As run (group 11, §3 item 59): WA45's include set leaves the fast path's EXCLUDE stream and the endpoint stream unopened and the watcher plans without `IM_EXCLF_FAST_PATH` (the "README WA53" notes in `native/windows/src/audio/`); WA53 opens both, gives the planner the flag, and replaces `test_exports_stub.cpp` and `im_audio_sound_check`'s stub. Its endpoint engine is what Windows 10 runs (§3 item 52), which the CI runner can't verify (its VB-CABLE endpoint loopback records digital silence); whether the onset and tail fades apply there (a stream that stops inside the endpoint mix carries the app's own fade) is checked against the S1 spike's endpoint mode (`spikes/s1-win-audio`). As built (merged in group 11, §3 item 62): the fast path, the endpoint engine, the startup probe with its gate and cache, the sound check and the test exports; its review made "twice in 10 s" count only system-wide failures, kept an app's last reopen across a loud sample, gated the sound check on Windows 10, gave the probe a 600 ms deadline and an abort without waiting, and extended the silent-stream check to the EXCLUDE stream (05 §6.4, §6.5, §11.2, §11.3, §12.1). WA62a's cases on the owner's PC are its hardware gate (§8) |
| WA54 | 6 | Client core: congestion end to end | 01 C13; 01 §10.1 | `internal/server/sfu` (the §10.1 constants, within their rows), `internal/client/ratectl` (constants), `internal/server/itest` (`desktop_congestion_test.go`; in WA43's `desktop_latency_test.go` only the removal of the `fullAt8Mbps` fallback, which WA46's ramp made dead code, group 5's hand-over, 01 §12.4) | WA46 | 1.5 | Mac, CI | itest D9 (a 3 Mbit/s uplink: a hint within 6 s, `f` settles ≤ 2.3 Mbit/s within 15 s, unrecovered loss < 2 %, the upload hint "540p" or "720p", back to 8 Mbit/s within 75 s once the limit lifts; §3 item 44: the 60 s first written here had too little margin, and WF5 moved the test's bound in group 7) D10 (a re-init encoder: no two changes < 10 s apart or < 30 %, each followed by a `Reinit` keyframe) and D19 (a 6 Mbit/s queue link with a 1 s limit: the queue < 150 ms within 6 s of the ramp crossing capacity, a congestion hint, no RTP lost); WA28's estimator tests still green; D16 passes without the `fullAt8Mbps` fallback (it logs `ramped true`), which is gone. As built: "settles" is read as `f`'s applied target first coming down to 2.3 Mbit/s or less within 15 s, with the final loss measured from then to the lift (01 §14 D9); the tuning moved ratectl's `RampEvery` to 3 s (with the ratectl tests' instants) and `nativeGrowth` to 1.151 (01 §9.3, §10.1); D9 and D19 put their Link at the core's end of the uplink, since `servertest` has no SFU-side `PacketConns` seam (01 §10.1); D10 asserted 9 s until WF4 made it the full 10 s (§3 item 43); D16 waits for the ramp (at most `RampDelay + 4·RampEvery + 5 s`) and logs `ramped in <duration>`, not `ramped true` (01 §12.4 item 1) |
| WA55 | 6 | Client core: stats, playback state, compat test | 01 C14; 01 §11.9, §11.11, §14 | `internal/client/core` (`stats.go`, `relay.go`, `compat_test.go`), `internal/server/itest` (`desktop_relay_test.go`) | WA18, WA46 | 2 | Mac, CI | itest D18 (`playback.state` → `EventPlaybackState` with `From`; an invalid payload ignored); `stats` messages every 10 s validate and reach the hub's metrics; `WatchServerStats` works; `TestCompatServer` passes against a locally started current server and skips without `ISSHONI_COMPAT_SERVER_URL` |
| WA56 | 6 | Device auth: hub and wiring | 02 DA6; 02 §6.3, §6.5 | `internal/server/signal` (the pre-auth refund, `DeviceName` filled), `internal/server` (`wire.go`: `site.ExtraOrigins = protocol.DesktopOrigins()`, `httpapi.Deps.DesktopOrigins`, `AuthenticateBearerToken`, `identityOf` and `Revalidate` copying `DeviceName`, `sessionService` and `Revalidate` on `auth.TouchPrincipal`, 02 §6.3 item 3), `internal/server/itest` (`device_helpers_test.go` with `linkDevice`, `device_test.go` cases 1–3, 5 and 9–12) | WA47 | 1 | Mac, CI ubuntu | 02 §16.2's cases 1–3, 5 and 9–12 (the spent grace child, the admin bearer, the /48 bucket, the unknown network); a socket from `http://wails.localhost` with `hello.auth` is accepted, and with a cookie gets 403; 21 sequential bearer sockets from one IP all pass; on a device revoke both sockets close with 4401 within 100 ms; `client_outdated` works for the `desktop` kind; goleak clean |
| WA57 | 6 | Device-auth docs and write-backs | 02 DA12; 02 §9 | `docs/privacy.md` (server-side device rows), `docs/security.md` (credential threat model), `docs/troubleshooting.md` ("Linking the app" at `#desktop-linking`), `docs/m1/03-accounts-and-store.md` (pointers to 02) | WA47 | 0.5 | Mac, CI ubuntu | `task site:build` green; the privacy page's device rows match 02 §9; every M1 03 section that 02 supersedes points to it; M1 03 §7.12's bearer line reads "every user endpoint and the WebSocket; admin routes cookie-only" |
| WA58 | 6 | Native sharing, Go side | 03 DK7 (Go part); 03 §9, §10.1, §10.5, §10.6 | `internal/desktop/session` (C2 on 01's core, the `core.Auth` adapter over 02's Manager), `internal/desktop/share` (the controller and both mapping tables), `internal/desktop/bridge` (`share.go`; as built also `bridge.go`'s controller lookup and event forwarding and `errors.go`'s controller rows, 03 §6.2, §6.4), `internal/desktop/app` (wiring: `app.New` builds the session and the controller when `Deps.Session` is nil, 03 §6.1) | WA29, WA48 | 2.5 | Mac, CI ubuntu | 03 §22.1's `share` and `session` tests: the controller's synctest suite (picks held ≤ 300 s, 03 §10.1 and §3 item 45, `Release` on back-out, `StartShare` with its 10 s wait, every end reason, a share follows the SPA's room, `StopOther`), 03 §10.5's error mapping, the `core.Auth` adapter (`SessionEnded`, `HelloRefused`, a new core client after a relink) |
| WA59 | 6 | Process contract, part 2: one-shot modes and the installer handover | 03 DK12 (part); 03 §13.4; 06 §2.4 | `cmd/isshoni-desktop` (`--self-check=<path>`, `--forget-credentials`, `--purge-data`, step 2a: the `Local\io.isshoni.desktop.installing` probe and its Pending values) | WA31, WA41, WA44 | 1 | Mac, CI windows; owner's PC | `--self-check` writes §3 item 4's JSON and exits 0 only when `ok`; unit tests with fakes on the Mac and ubuntu; in `windows-go`, with a test credential prefix, `--forget-credentials` and `--purge-data` leave no credential and exit 0 while offline, and `--purge-data` also removes the data root; none opens a window or takes the mutex; 03 §22.1's start-order row (installer mutex present: a link start writes `PendingServer`/`PendingRoom` and exits 0 with no single instance, running mutex or `Apply`; absent: the normal order). Manual: `--self-check` passes against the `windows-native` artifact on the owner's PC (in CI from WA67's smoke). As run (group 11, §3 item 59): WA44's `im_probe` is what `--self-check` runs; until WF10 a probe lists the real encoders while no session can open them (04 §9.1), which doesn't change `--self-check`'s verdict (its `ok` needs the engine fields and `spa.ok`, §3 item 4). As built (merged in group 11, §3 item 63): `--purge-data` exits 0 when the data root survives (logged at ERROR), as 03 §13.4 and 06 §2.4 say; `--forget-credentials` removes a data root only its own save created; step 2a uses `mutexOpens` and `start.go`'s `installingMutex`; `--self-check` always opens the shipped engine and serves `GET /` for `spa.ok` (03 §13.4) |
| WA62a | 6 | Audiotest harness, part 1: runner, analyzer, the cases without a microphone | 05 A7 (part); 05 §18.1–§18.7, §18.10 (gate configuration) | `cmd/isshoni-audiotest` (runner, fixtures with explorer reparenting, the analyzer ported from S1's `analyze.go`, report schema, `selfcheck`, `apps`, `run -gate` with the gate configuration, the cases 1–8, 10, 11, 15–21 and 24–27) | WA26, WA35, WA37 | 2 | Mac, CI; owner's PC | Analyzer and verdict tests on synthetic recordings pass on the Mac and in CI: Goertzel levels, the present and absent rule, clicks, the positive controls, every INCONCLUSIVE reason, a 20 ms burst of 440 Hz at −18 dBFS in a 10 s window FAILs absent, a faded 1000 Hz tone and its onsets never trip a short-window check at 907 Hz, case 5's session-lag rule (21 ms → FAIL), and the gate configuration (shipped `fastPath` false → cases 4, 5 and 26 INFO, the exit code unaffected); the Windows build; `selfcheck` passes on CI windows. Integrator, right after group 6 merges (owner's PC, Win11, then Win10 22H2): `run -case` with cases 1–8, 10, 11, 15–21 and 24–27, every required one passing (this is WA45's and WA53's hardware gate), and WA35's `apps` check; a failure becomes a `WF` slice that lists `native/windows/src/audio/`, never a harness change. As built (WA62a, merged in group 5): cases 20, 21 and 27 drive the real capture and are first exercised on the owner's PC, since the Mac can't load the DLL, so a first failure in one of them may be the harness's and is judged before a `WF` slice is opened. As decided (§3 item 52): on Win10 22H2 the engine is the endpoint one, so there only cases 1 and 19 and the banner are required (05 §18.10's reduced form) |
| WB5 | 6 | Group 5 write-backs into docs/m2 | group 5's integration and review notes (the slices' deviations and rejected items, the integration fixes, the open issues), PR #41's CI fixes; §3 items 39–42 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA46 (01 §11.7: the republish's params, codec policy and owed pause, the core's own changes before a `room.state` that disagrees; §11.8: `SetBitrate` before `SetLayerActive`, no Tick while paused, the codec switch's per-layer check and `refusedHold`, the errors and order of `SetPaused` and `SetPreset`, the paused starting share; §12.4 item 1's fallback; §14's D5 and D7), WA47 (02 §6.1 and §6.4: the bearer path's order, `Content-Type` before the token, the recovered 500's headers; §7.2: #52 notifies on every 200, `GET /me`'s device fields; §8: the audit table complete, one notification helper), WA48 (03 §3.2's W1, W11, W12 and W15; §6.2: registration, plain strings, what is still `not_implemented`, `Capabilities`; §6.3's events; §6.4's codes and error mapping; §7.4's `WailsBridge`; §18.1's codes rule; §22.4 A's harness and CI wiring and B's data root; 02 §11.7's `AddServer`), WA49 (06 §3's module provisioning, §7.1–§7.2 as built, §10.1's `lint-deploy`), WA62a (05 §18.7's `measured`, `info`, the unread machine fields and the summary line; the WA62a row), WF2 (04 §4, §5.6's `fpsIn`, §6.1, §8.1, §8.3's encoder side, §8.5's pause, §9.8, §18.2's `session_gpu_test` and `session_frames_test`; 06 §5.1's note) and the integration fixes (06 §7.1's `release:keys` and `.gitattributes`, §10.1's `build`, `e2e` and `protocol` rows, §10.2); §3 items 39–42 in their sections (item 40's frequency named by the write-back after WA62b; item 42 also in 02 §13.3); §4's lessons from group 5 and PR #41; the rows of WA54, WA62b, WA71, WA72, WA77 and WA82 carry what group 5 handed them, and WA74's row and §8 the open TLS measurement (WA49-R4); "How the plan works" says how groups 5 and 6 ran; this table gains WB5's and WF3's rows |
| WF3 | 6 | Hub: a paused starting share | §3 item 39; 01 §10.3; group 5's WA46-R2 | `internal/server/signal`, `internal/server/itest` (`desktop_pause_test.go`; in WA46's `desktop_hints_test.go` only `TestDesktopRepublishKeepsPauseAndPreset`) | WA27, WA46 | 0.5 | Mac, CI | A share paused while it is starting shows `paused` in `room.state` at once and doesn't end at its start timeout while the bit is set; clearing the bit before it went live shows `starting` again and gives it a fresh 30 s start timeout from the resume (`media_timeout` if no keyframe arrives in it); a share republished with `paused: true` stays while paused; WA27's `TestSharePauseStarting` asserts the new rule, and M1's signal tests stay green |
| WA60 | 7 | Desktop Duplication | 04 E11; 04 §7.4, §7.5 | `native/windows/src/capture/` (DD capture, access-lost handling, rotation, DD cursor shapes, the Win10 monitor mapping, `im_list_sources`), `native/windows/tests/`, `cmd/isshoni-enginetest` (`sources.go`), `docs/m2/04-windows-engine.md` (§7.5 only) | WA50 | 2 | owner's PC (Win10); CI windows | The Windows half of the monitor mapping passes with fake outputs in CI. Manual (Win10 22H2): M-DD (`capture_path` DD with `MAPPED`, no border, pointer shapes, a UAC prompt never captured, a re-init at a new resolution, upright output) and M-WIN10-WINDOW (the yellow border); 04 §7.5's mapping steps 1–3 and 5 only; what `DisplayName()` returns for each Win10 monitor (two outputs if available) recorded in 04 §7.5 with the verdict; if names are ambiguous for identical monitors, the integrator adds a `WF` slice for step 4 As run (group 13, §3 item 74): WA50 and WF12 are merged, so `capture/` has the WGC capture, the `capture_source` seam (a DD factory plugs in beside WGC's, 04 §7.3) and `capture::OsFlags()`; `monitor_id` is empty and a Windows 10 monitor pick reports WGC with the border until this slice; WA69's `isshoni_hw` tests wait for its `mon:` ids. Of `docs/m2`, it changes 04 §7.5 only; WB11 runs in the same group and leaves that section alone (§5's shared files) |
| WA61 | 7 | Encoder selection, fallback, device loss, profiles | 04 E9; 04 §9.6–§9.8 | `native/windows/src/encode/` (the selector), `native/windows/src/session/` (device loss ending the session, `im_set_profile`, `im_update_layer`, `im_set_content_hint` re-inits), `native/windows/tests/`, `cmd/isshoni-enginetest` (`sessions.go`) | WA51, WA52 | 2.5 | CI windows; owner's PC | The extended fault-injection cases of `session_fake_test`: candidate order, preview-layer fallback, one profile per session, re-init spacing of 10 s with the latest value winning, 1 s coalescing, the fallback event order (`ENCODER_CHANGED` before the new encoder's first `REINIT\|KEYFRAME`), injected device removal → `ENDED{IM_ERR_ENCODER_LOST}` `device_removed` within 1 s, then a new session on a new device. Manual: M-NVENC-LIMIT (`sessions -count 5` while OBS records with NVENC: every session starts, some layers report MF) As run (group 13, §3 item 74): WA51 (OpenH264 2.7.0) and WA52 (pause, stats) are merged and WF10 put `encode::CatalogEntries()` in the session's catalog; on the GPU-less runner MF software is refused by the probe (its first AU only on drain, §3 item 68), so its CI cases can't count on a real encoder there (`session_fake_test`'s fault injection runs on the test hooks' entries, 04 §18.2); in this group only WA61 changes `encode/` and `session/` |
| WA62b | 7 | Audiotest harness, part 2: mic cases, soak, records, redaction | 05 A7 (part); 05 §18.2, §18.5, §18.7–§18.9 | `cmd/isshoni-audiotest` (the mic cases 9 and 12–14, `multi-endpoint`, `soak` with `-long`, `record`, `merge`, report redaction, `README.md` with its results table; the isshoni-self tone of §3 item 40) | WA62a | 1.5 | Mac, CI; owner's PC | The isshoni-self tone (`freqSelf`) at least 200 Hz from 1000 Hz and from every other fixture tone (§3 item 40), the analyzer's expectations moved with it, and WA62a's 5 ms onset subtest no longer skipped; report redaction tests (no names, paths relative to the work folder, `other` for non-fixture apps) and a `merge` round trip on golden reports pass on the Mac and in CI; the Windows build. Manual (owner's PC, Win11): `run -gate` runs every case of 05 §18.5 and exits 0; a `record`/`merge` of R1 lands in a report. A failure in audio code becomes a `WF` slice that lists `native/windows/src/audio/`, never a change in this PR. As built (WA62b, merged in group 6): `freqSelf` is 1700 Hz (§3 item 40); its endpoint reads (`mmdevice_windows.go`), cases 13, 14 and 22 and the soak are first exercised on the owner's PC, so a first failure there may be the harness's and is judged before a `WF` slice is opened, as for WA62a's cases 20, 21 and 27 |
| WA63 | 7 | Device-link approval and Devices pages | 02 DA7; 02 §10 | `web/src/link/`, `web/src/account/` (DevicesPage rows: current, rename, remove; cookie-only actions hidden for devices), `web/src/lib/appLink.ts`, `web/src/i18n/` (`lazy/link.en.json`, two `errors.*` keys), `web/e2e/link.spec.ts`, `web/e2e/a11y.spec.ts` (the link states) | WA42, WA56 | 2 | Mac, CI ubuntu | `task lint typecheck test e2e` green; `link.spec`: start, approve, a Node poll gets an `isa_` token, a Node WebSocket gets `welcome`, Remove closes it with 4401, the signed-out redirect, the different- and unknown-network notes each need the tick and `same` has none; the initial JS stays under M1 05's 200 KB budget. Group 7 also gave it two hand-overs that had no owner: `link` in WA42's `router.test` folder list (group 4's WA42), and 03 §18.1's `codes.gen.ts` rule in `web/scripts/check-i18n.mjs` (group 5's WA48, WB5-R2). As built (02 §10.1, §10.2, §16.3; 03 §18.1): the spoken code is visually hidden text (an `aria-label` there is an axe violation); a failed decide keeps the card with a Notice, and only a failed lookup shows `error`; a session that ends under the card puts the code back into the URL first; the Devices page's texts are `link.devices.*` and `account/index.ts` loads `link` (no `account.*` key changed); `errors.invalid_token` reads "This app's sign-in expired. Link the app again."; `appLinkFor` follows Go's `canonicalHost` except the bidi rule (WA63-R3, for WA74); `check-i18n.mjs`'s rule 6, with the `update.*` exemption WA82 ends; tests next to its paths (`appLink.node.test.ts`, `check-i18n-codes.node.test.ts`, the i18n tests for the new lazy namespace) |
| WA64 | 7 | Device auth end to end | 02 DA11; 02 §13.4, §16.2 | `internal/server/itest` (`device_test.go` cases 4 and 6–8; `linkedManager`), `internal/client/deviceauth` (`compat_server_test.go`) | WA31, WA56 | 1.5 | Mac, CI ubuntu | 02 §16.2 complete with the real Manager and the Go signal client: presence shows one participant for the app's two connections; a revoke moves the Manager to `unlinked`/`signed_out` with the store emptied; reuse gives `refresh_reused`; `minClientVersion` gives the update verdict; goleak; `TestCompatServerDeviceFlow` skips without its env var and passes against a locally started server. As built (02 §16.2): `TestDeviceManagerLink` (cases 1–4 and 7) and `TestDeviceManagerPassword` (cases 6 and 5) beside WA56's raw-HTTP tests; `linkedManager` keeps 02's one-value signature, on `newManager` and `linkManager`, unused until WA73; case 1's "one poll interval" is real time (under two intervals); case 4 takes the restart grace path; case 6 reads `linked_via` from the server's SQLite file read-only; case 7 renames through the resume handshake's `Revalidate`, since the hub's 5-minute ticker has no test seam (a seam in `internal/server` and `servertest` is WA64's suggestion, no slice yet) |
| WA65 | 7 | Native sharing, SPA side | 03 DK7 (SPA part); 03 §10.2–§10.4 | `web/src/platform/desktop/` (`DesktopSharing`, `DesktopShare`, plugged into `DesktopPlatformOptions.sharing`), `web/src/platform/types.ts` (`PickedSource.preview: MediaStream \| null`, 03 §7.1) with its readers in `web/src/share/` (`BrowserSharing`, `BrowserShare`, `shareStore`), `web/src/share/native/` (share sheet, first-share sheet, panel bar, Details disclosure, the unreachable state with Test connection), `web/src/rooms/` (`RoomSession.adoptShare`, `findShareElsewhere` with connection ids, `stopOther`, the Share gates of `RoomPage.tsx` and `RoomStage.tsx` on `capabilities().canShare`, 03 §7.6), `web/src/viewer/` (the own share at the low layer only), `web/e2e/desktop-share.spec.ts`, `web/e2e/desktop-reload.spec.ts` | WA48, WA58 | 3 | Mac, CI ubuntu | `desktop-share.spec` without Pause (a fake pick → the first-share sheet → live; a web viewer decodes frames and audio energy; the own tile gets the low layer and no audio; the tray shows "Live, 1 watching") and `desktop-reload.spec` pass; component tests of the sheets. Manual: `task desktop:dev` on the Mac shares the 640×360 Decodable pattern into a room that a browser watches. Group 7 also gave it: the SPA holds a pick for 03 §10.1's 300 s, as Go does (§3 item 45; `bridge.ts`'s `Picked` said 120 s), and `desktop-security.spec`'s loopback stand-in for the desktop origin goes, since WA56 wires `protocol.DesktopOrigins()` (group 5's WA48 hand-over). As built (03 §6.3, §7.1, §7.3, §7.6, §9, §10.2–§10.5, §18.1, §22.4): `SharingProvider.pick` takes `withAudio`, and `ready()`, `onChange()` and `ActiveShare.roomId` let `rooms/runtime.ts` follow the app's share to its room (`RoomSession.followShareRoom`) and adopt it there; a native share's end is C2's (`serverEnded` refreshes the view); `DesktopSharing` loads in its own chunk behind `lazySharing`; `shareStore` gains `confirming` (`first-share`, `dnd`, `screen-audio`), `setPreset`, `confirm(withAudio?)` and nullable `params`; `viewerStore`'s `ViewerSelf.localConnectionIds` and `ViewerShare.localPreview`, `LayerInputs.localShares` (rule 7) and `viewer.tile.lowPreview` in `en.json`; `useShareAvailability` and `NativeShareDisabled` for the Share gates; `share.state` carries `ShareView \| null`; M1's `ShareButton` and `SharePanel` stay, and native mode renders `NativeShareButton` and `NativeSharePanel` through `rooms/lazyNative.ts`; paths beyond the Touches line, all needed: `share/elsewhere.ts` (a list of connection ids), three tests' `preview?.`, `i18n/en.json`, `lazy/desktop.en.json` and `check-i18n-lazy.node.test.ts`, `desktop-security.spec.ts`. Hand-overs: the first-share sheet's "Friends will hear", the volume note and `Audio.Watch` to WA77; Pause, the hotkey line, the tray step and the mic prompt step of `desktop-share.spec` to WA80 (their rows); open, with no slice yet: `ShareCaps` carrying each encoder's status and reason (Go, `internal/desktop/bridge`), which 03 §10.5's probe-time `driver_too_old` note and the `no_media_foundation` and `blocked_by_policy` texts need (WA65-R8) |
| WA66 | 7 | Server route `/install-desktop.ps1` | 06 R11; 06 §7.3, §7.4 | `deploy/embed.go` (new Go package `deploy`), `internal/server/httpapi` (`desktopscripts.go`, the router line), `internal/server` (`wire.go` lines), `internal/server/config` (the reserved env list only) | WA49, WA56 | 0.5 | Mac, CI ubuntu | 06 §16's `desktopscripts_test` (quotes doubled; an empty server for a non-ASCII or non-loopback `http://` origin; an empty version for dev builds; rc kept; markers checked; 200 with charset, `Cache-Control`, `ETag`, `HEAD` without a body); `/install-desktop.sh` is a 404 text and never the SPA; `task dev` serves a script pre-filled with `http://localhost:5173`; no unknown-env warning for 06 §7.3's names. As built (06 §7.3, §7.4, §16): the wiring line is `Desktop: s.desktopScripts()` in `http.go`'s `routerOptions`, outside the slice's paths, so group 7's integration added it, with `internal/server/desktopscripts_test.go` and Vite's dev proxy for both script paths (`web/vite.config.ts`); `wire.go` checks the script at startup (a `*config.OperatorError` with no `Reason`, exit 78) and `NewRouter` renders it again and logs the WARN; `keys_test.go` pins the reserved names. M1 04's three rows (§2 `deploy`, §4.2's list, §9.2's route) are outside `docs/m2` and still to write (the integrator's) |
| WA67 | 7 | NSIS installer | 06 R4; 06 §2, §6 | `deploy/windows/nsis/`, `tools/desktoppkg` (`winver`, `stage`, `nsis`, `check`), `deploy/test/windows/smoke.ps1`, `Taskfile.yml` (`desktop:exe:windows`, `desktop:package:windows`), `.github/workflows/ci.yml` (`windows-desktop`, first part: exe, NSIS, stage, installer, smoke) | WA24, WA31, WA41, WA59 | 2.5 | Mac; CI windows; owner's PC | An installer builds on the Mac with Homebrew makensis; `makensis -WX` clean; the strings rule holds; on CI windows every `smoke.ps1 -Mode ci` case of 06 §16 passes: files, registry, the protocol command `"<InstallDir>\isshoni.exe" --open-url "%1"`, the shortcut AUMID, `--version`, `--self-check`, stale-file removal and the `.old` rename loop, repair, downgrade exit 4 and `/ALLOWDOWNGRADE`, a running app gives exit 3, `/D=` safety, credentials and toast keys gone after both uninstall modes, the data root removed only on purge; while `Local\io.isshoni.desktop.installing` is held, a second setup exits 8 and an app start exits 0 with `PendingServer`/`PendingRoom` written; the Finish page's "Next: go back to your browser and click Open isshoni" line comes from the language file. Manual: the interactive pages on the owner's PC; the `/UPDATE` banner shows (or 06 §6.4's fallback is recorded). As run (group 12, §3 item 65): WA59's one-shot modes and the installer mutex `Local\io.isshoni.desktop.installing` are merged in `cmd/isshoni-desktop`, and WA24's `task native:dist` and the `windows-native` artifacts exist; the FFmpeg DLLs come from `native/ffmpeg/lock.json`'s release, read from the lock rather than a hard-coded tag, and revision 2 (the libvpl patch, §3 item 64) was published and locked before the group's merge (§3 item 69) As built (group 12, §3 item 71): 06 §6's "As built" list; the first `windows-desktop` run is the merged tree's |
| WA68 | 7 | Tray, window, notifications, system events | 03 DK9; 03 §4.4, §11.1, §11.5, §11.6, §15, §17 | `internal/desktop/{tray,app,winos}`, `cmd/isshoni-desktop` (`shell.go`: the Wails tray, the close hook, toasts, `-Embedding`; `build/tray/`), `web/e2e/desktop-system.spec.ts` | WA58, WA59 | 3 | Mac, CI ubuntu/windows; owner's PC | 03 §22.1's `app` and `tray` tests, the §22.2 Windows tests (WDA on the main window, the `winos` thread, the toast registry keys) and `desktop-system.spec` (a lock pauses through the controller and an unlock resumes, a suspend stops the share with a toast) pass; W7, W8, W14, W17 and W18 recorded in the PR under "Wails checklist". Manual: M-DESK-4, M-DESK-6, M-DESK-11. As run (group 12, §3 item 65): its Needs, WA58 and WA59, are merged, and it waited only for WA59, which waited for WA44 As built (group 12, §3 item 72): `app.TrayShell` and the optional OS hooks; its leftovers are WF13's |
| WA69 | 7 | Hardware capture-to-send check | 01 C18; 01 §12.3 | `internal/client/core` (`capture_hw_test.go`, build tag `isshoni_hw`) | WA44, WA55 | 1 | owner's PC | Manual (owner's PC, both OSes; on the runner if one is registered): `TestCaptureToSend` gives capture → send p95 ≤ 35 ms and p50 ≤ 20 ms over 30 s at 1080p60 for each encoder the probe lists as tested; `mediatest.Conformance` passes with a real monitor per encoder. As run (group 12, §3 item 65): WF10 merged, so sessions open the probe's real encoders and `TestCaptureToSend` can run per tested encoder; the hardware runs are the owner's (§8), can run since the FFmpeg r2 lock was committed (§3 items 64, 69) and are listed in its PR As built (group 12, §3 item 73): `isshoni_hw && windows`, three 10 s windows; it needs WA60's `mon:` ids |
| WB6 | 7 | Group 6 write-backs into docs/m2 | group 6's integration and review notes (the slices' deviations and rejected items, the integration fixes, the open issues), PR #42's CI fixes; §3 items 43–46 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA54 (01 §9.3 and §9.6: `RampEvery` 3 s and the 14 s ramp; §10.1: `nativeGrowth` 1.151, the measured timing, the Link at the core's end of the uplink; §12.4 item 1: D16 waits for the ramp; §14: the ratectl row, D9's script and its "settles" reading, D10's measurement, D19's timing; C13), WA55 (01 §11.2's and §11.9's `WatchServerStats`, §11.9's engine stats and `stats` reports as built, §11.10; §14's `TestCompatServer` against a server without `deviceFlow`), WA56 (02 §6.3 items 1–3: `ExtraOrigins` at the top of `wireAccounts`, `Revalidate`'s fresh user read; §16.2's cases 2, 3 and 7 and the two extra tests), WA57 (02 §6.8's logout note and §9's first bullet; 03 §8.1, §9 and §23: the "See why" link is `DOCS_URL` + `troubleshooting#desktop-linking`; 06 §13's `#linked-apps` hand-over and WA85's row), WA58 (01 §16 and this table's WA58 row: the 300 s hold; 03 §6.1's wiring, §6.2's `Share` service and `WatchServerStats`, §6.3's `server`, §6.4's controller rows, §10.1's bounds, views, 05 calls and `ShareRequest` without `Exclusions`, §10.2, §10.3, §20, §22.1), WA62b (05 §18.2's `run`, `record` and `merge`, §18.3's 1700 Hz, §18.4's `machine.mic`, §18.5's soak and INFO cases, §18.6, §18.7's machine fields and redaction, §19.1 and A7), WF3 (01 §10.3, §11.8 and §14 D5 as built, with the first keyframe while paused, the unbounded paused start and WF3-3), PR #42's (01 §12.4 item 2 checked; 06 §10.1's `test-go` timeouts; §4's lessons); §3 items 43–46 in their sections (01 §9, §10.1, §11.5, §11.9, §14 D9 and D10; 03 §10.1, §10.2, §10.5; the WA54, WA58 and WA65 rows; WA63's row has its two hand-overs, and WA77's row and 05 A9 WA58's: 05's share calls are built, so `main.go` passes the service as `app.Deps.Audio` too); items 13 (WA62b's first hardware run, also in its row and 05 A7), 39 and 40 as built; "How the plan works" says how group 7 runs (the Grp column stays the plan); §8 has WA62b's first hardware run; this table gains WB6's, WF4's and WF5's rows. §9's review log and the earlier WB rows stay historical |
| WF4 | 7 | Core: the rate tick on the actor's clock, and the media reason in `EndInfo` | §3 item 43; 01 §9, §11.9; group 6's WA54-R2 and WA58-4 | `internal/client/core` (`share.go`'s rate tick, `EndInfo.MediaReason` in `engineEnd` and `startEnd`, and their tests), `internal/desktop/share` (`mapping.go`'s `endError` and the controller's tests), `internal/server/itest` (`desktop_congestion_test.go`: D10's `callSlack` only; WF5 changes only D9's bound in the same file) | WA54, WA58 | 0.5 | Mac, CI | ratectl gets only the actor's `time.Now()` (no ticker channel value); D10 asserts `ReinitMinInterval` between two engine calls of a layer with at most Windows' clock step (20 ms) of slack, and passes; a live share that ends with `encoder_lost`/`device_removed` has `EndInfo{Reason: error, Media: encoder_lost, MediaReason: device_removed}`, a start that fails with a reason keeps it too, and the controller's view carries `engine.encoder_lost` with `params.reason: device_removed` (03 §10.5's row; `TestEndReasonTable` has the row); M1's and the core's suites stay green. As built (§3 item 43; 01 §9, §11.5, §11.8, §11.9, §14): the actor re-arms a `time.Timer` after each `Tick`'s engine calls besides passing `time.Now()`, and D10 asserts the full `ReinitMinInterval` with no slack (gaps of 10.0047–10.0123 s); `TestRateTickClock`, `TestEngineEndReason` and `TestEndInfoOf`; WF4's D1 change to `desktop_share_test.go` was dropped in review (WA29's file; 13e1f99 already let D1's quiet second hold the ramp's steps); the bridge's view test of `device_removed` came with group 7's integration; PR #44 then measured `TestRateTickClock`'s held call from its issue (01 §9's known limit) |
| WF5 | 7 | SFU: the loss row counts the loss once; D9's 75 s | §3 item 44; 01 §10.1, §14 D9; group 6's WA54-R1 and its loss-row finding | `internal/server/sfu` (`uplink_native.go`'s loss row and its tests), `internal/server/itest` (`desktop_congestion_test.go`: D9's `recoverWithin` only; WF4 changes only D10's slack in the same file) | WA54 | 0.5 | Mac, CI | The loss row is `max(500 kbit/s, 0.8 × rate)` (a table test: 20 % loss with 5 Mbit/s arriving gives 4 Mbit/s); WA28's other estimator tests stay green; D9 passes with its 75 s bound, and D10 and D19 still pass. As built (§3 item 44; 01 §10.1, §14 D9): with the loss counted once, D9's settle bound failed about one run in five and D10 could stall, so the review fixes made the loss row `max(500 kbit/s, 0.8 × arrived)`, from the last second's arrivals (`layerStats.arrived` in `layer.go`, `LayerUplinkStats.arrived` in `uplink.go`, inside the package), and `nativeGrowthRate` 0.75 (was 0.85); `TestNativeUplinkLossOnce` pins the value; D9 passed 5 of 5 under `-race` (back in 29–54 s), D10 and D19 3 of 3. The re-init guard's floor, which no estimator change can fix, is WF6's (§3 item 47) |
| WA70 | 8 | Isshoni's own windows out of every capture | 04 E12; 04 §11 | `native/windows/src/window/`, `internal/media/native` (`ProtectWindow`, used only by `protect-check`; WA23 already built `Pick`'s `IM_SRCF_SELF` → `invalid`/`self_window` rule), `native/windows/tests/`, `cmd/isshoni-enginetest` (`protect.go`) | WA51, WA60, WA68 | 1 | owner's PC | `isshoni-enginetest protect-check` passes on Win11 and Win10 for DD and WGC. Manual: M-WDA checks the affinity that 03's `winos` sets on the main window (WA68): the main window and its WebView2 content are absent from a full-screen share watched from a phone; the pill joins in WA80 As run (group 14, §3 item 85): WA51, WA60 and WA68 are merged, and group 13's integration made `mon:` ids startable with DD (§3 item 78), so `protect-check` can run a DD session on Win10 from a listed `mon:` id; the running capture's path is in `im_session_info` and the stats; `native/windows/tests/` is file-level (its own test files only). As built (§3 item 88): `native/windows/src/window/protect.{h,cpp}` (every call needs a top-level window of this process, 04 §11), `native/windows/tests/protect_window_test.cpp` (04 §18.2), `internal/media/native`'s `ProtectWindow` and `CapturePath` with `protect_test.go` and `protect_dll_windows_test.go` (`open_test.go`'s `TestNotImplementedYet` removed), and the subcommand as `protect.go` (the portable logic) plus `protect_windows.go` (the Win32 noise window) and `protect_other.go`, tested in `protect_test.go`. Presence is judged by layer `f`'s inter-frame bit rate, a listed `mon:` is always DD, WGC (a pick) that ran DD is `not_reachable` with exit 0 (an owner decision, §7 question 11), and `hwcheck.ps1` runs `protect-check -wgc off` (04 §14). Open: the Win11 and Win10 runs, the interactive WGC half and M-WDA on the owner's PC; `protect_window_test` and `TestDLLProtectWindow` on `windows-native`/`windows-desktop` |
| WA71 | 8 | First-run wizard, servers and the password link | 03 DK6, 02 §14 (the app's form); 03 §8.1–§8.3 | `web/src/desktop/` (`SetupPage` with the server, add, confirm and link steps including "Sign in with username and password instead", `RelinkPage`, the server switcher), `internal/desktop/bridge` (`auth.go`), `internal/desktop/app` (`auth.state`, `SessionEnded` from C1, server switching, Sign out, Remove), `web/src/rooms/AccountMenu.tsx` (03 §7.5's desktop menu, unowned until group 4's review found it) | WA48, WA63, WA68 | 3 | Mac, CI ubuntu | `desktop-link.spec` (address → confirm → code, QR code and short URL → a second browser context approves at `/link` → linked; a denied code; an unknown server shows "Add server {host}?" with no request before Add; the password path with a wrong, then a right password; the second-server case: with A active and B added and linked, no request or hello to A ever carries a token B issued) and `desktop-relink.spec` pass; component tests show the text for every probe error code and notice; the QR test passes; WebView2 autofill and password saving stay off; in the desktop the account menu offers Settings, Switch server (with more than one server), Account ↗, Admin ↗ for admins and "Sign out of <server>" through `Auth.Unlink()`, never `POST /auth/logout` and no Notifications (03 §7.5, 02 §11.8), and the browser's menu is unchanged; `Auth.AddServer`, `SwitchServer`, `RemoveServer` and `Unlink` replace WA48's `not_implemented` (03 §6.2) As run (group 13, §3 item 74): WA68 is merged (the tray, `app.TrayShell`, the lock pause); check that `Auth.Unlink`, `AddServer`, `SwitchServer` and `RemoveServer` still answer WA48's `not_implemented` before replacing them; WF13 changes `internal/desktop/winos`, `internal/desktop/e2ehost` and `web/src/platform/desktop/` in the same group, not this slice's paths |
| WA72 | 8 | `windows-desktop` job complete | 06 R5, R10 (its Windows run); 06 §10.1 | `.github/workflows/ci.yml` (`windows-desktop`: Go tests with `ISSHONI_TEST_ENGINE_DLL`, the CDP spec on a `TAGS=e2e` build, Pester and the grammar check under Windows PowerShell 5.1, the ps1 install against relserve, `isshoni-audiotest selfcheck`, the PR artifact; `protocol`: the bindings and `codes.gen.ts` drift checks; `build` and `e2e`: `DESKTOP_E2E_BIN`), `cmd/isshoni-desktop` (`flavor_e2e.go`: the CDP port from an e2e-only variable or flag into `WindowsOptions.AdditionalBrowserArgs`, and the data root from `ISSHONI_E2E_DATA_DIR` instead of `FOLDERID_LocalAppData`, the two variables WA48's skeleton sets, `ISSHONI_E2E_CDP_PORT` for the port, 03 §22.4 B). Group 5's integration already added `build`'s and `e2e`'s `DESKTOP_E2E_BIN` wiring and `protocol`'s `gen:bindings:check` with its own `wails3` cache and 15 min timeout (06 §10.1); WA72 keeps them | WA48, WA49, WA62a, WA65, WA67, WA68 | 1 | CI ubuntu/windows; owner's PC | A PR changing the app runs the job green: `TestNativeIntoPublisher` runs (not skipped), the CDP skeleton passes, attached through that port (beta.34 overwrites `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS`), the ps1 installs a CI build with the inbox `ssh-keygen` and refuses a tampered file and a wrong key, `selfcheck` passes; a hand-edited binding fails `protocol`; the desktop specs run in `e2e`. Manual: the PR artifact installs on the owner's PC, then M-APP-PICK (04 §18.4) on Win11 and then Win10: Share → the OS picker is modal over the main window, which keeps repainting → pick a window, then a monitor (Win10: the DD path) → live and watched in a browser; Cancel returns to the sheet; picking an isshoni window gives `self_window` and goes back to the picker; the CDP fixture finds the app's log or WebView2 profile in the `ISSHONI_E2E_DATA_DIR` folder, so the run touched none of the runner's or the owner's real data As run (group 13, §3 item 74): WA67 wrote the job's first part (exe, NSIS, stage, installer, smoke, its `ci-ok` wiring); it is the only `ci.yml` slice of the group, and if `windows-native`'s release preset is in its part it switches the inline four-part version to `desktoppkg winver` (§3 item 71) |
| WA73 | 8 | Client core on bearer auth | 01 C15; 01 §11.3 | `internal/client/core`, `internal/server/itest` (`desktop_helpers_test.go` switches to `linkedManager`, and so does `recoveringDesktop` in WA38's `desktop_recovery_test.go`, which copies its cookie auth: since PR #43 the core is built in `recoveringDesktopOn`, the variant with an engine wrap that D10's `engineTap` uses; `linkedManager` then drops its `nolint:unused` directive) | WA55, WA56, WA64, WA69 | 1.5 | Mac, CI | itest D1–D3 pass with bearer; `unauthenticated` → one forced refresh → a new connection → republish; a revoked device → `SessionEnded`, then `StopUnlinked` with the manager unlinked As run (group 13, §3 item 74): its last Need, WA69, merged in group 12 |
| WA74 | 8 | `/download` page | 06 R12; 06 §7.5, §7.6 | `web/src/download/`, `web/src/i18n/lazy/account.en.json` (`account.download.*`), `web/e2e/download.spec.ts` | WA49, WA63, WA66 | 1 | Mac, CI ubuntu | `links.test` and `DownloadPage.test`: release and non-release links, the exact installer URL, the ps1 command with the server's origin in the form 06 §7.1 records (bare, unless the owner's TLS measurement, WA49-R4, finds TLS 1.2 off by default; then with the `[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072; ` prefix), `openAppUrl` = `isshoni://watch.example.com/` through `appLinkFor` and none for `http://localhost`, a valid `?room=` giving `isshoni://watch.example.com/r/<id>` and an invalid one the plain link; the three numbered Windows steps; the three warning lines (browser download, SmartScreen, Smart App Control) with their project-site anchors; `download.spec` on the e2e server and with `/info` routed to `0.2.0`; axe passes |
| WA75 | 8 | Audiotest workflow and the runner docs | 06 R15; 06 §10.4, §10.5 | `.github/workflows/audiotest.yml`, `tools/repocheck` (the no-PR-trigger test), `deploy/windows/runner/README.md` | WA24, WA62b | 0.5 | Mac, CI; owner's PC | The workflow has only `workflow_dispatch` (a repocheck test reads it), the repository and actor guard, §3 item 10's labels and approval environment, no checkout and no token use in `run`; the `release_run_id` input and the GitHub-hosted `fetch` job, the only job with `actions: read` (run end to end in WA81's dry run). Owner, only if the runner is registered (§7 question 6): a dispatch under Win11 and under Win10 produces a report artifact and a summary, and `suite: hwcheck` uploads WA76's bundle As run (group 13, §3 item 74): WA09 ran S1 on a `windows-2025` runner with VB-CABLE (`.github/workflows/s1-audio.yml`, §3 items 51–52), so the workflow reuses that setup where it needs a virtual device and the runner docs say what the hosted runner can and cannot check; §7 question 6 (the self-hosted runner) stays open |
| WA76 | 8 | Soak, A/V sync and the hardware run script | 04 E15 (code part); 04 §14, §18.4; 05 §19.3 | `cmd/isshoni-enginetest` (`soak.go`, `avsync.go`: the flash window, the click process, the luma-tap and PCM-tap pairing), `native/windows/tools/hwcheck.ps1`, `Taskfile.yml` (`native:dist` adds `hwcheck.ps1` and `g2g.exe`) | WA53, WA60, WA61 | 1.5 | owner's PC | `hwcheck.ps1` runs the probe, `capture` per encoder, `bench`, `sessions -count 5` and `protect-check` and zips the reports without machine names or paths; PSScriptAnalyzer clean; `avsync`'s pairing logic has a unit test on synthetic times. Manual: `soak -hours 2` passes (≤ 5 % growth after the first 10 minutes, no `RING_OVERFLOW`, no `ENDED`); a first `avsync -dll ci` run on Win11 recorded (the gate run is WA90's); M-GAME recorded As run (group 14, §3 items 79, 83, 85): WA53, WA60 and WA61 are merged; `sessions` fails a run in which a session `ENDED` (WA61), and `mon:` ids start DD sessions (§3 item 78). WA75's `audiotest.yml` starts `hwcheck.ps1` without arguments and looks for its zip in the current folder or next to the script: WA76 defines the script's output argument (04 §14) and lists it in its PR, and the integrator passes it in `audiotest.yml` at the merge (§5's shared files); the hardware runs are the owner's and the PR lists them |
| WB7 | 8 | Group 7 write-backs into docs/m2 | group 7's integration and review notes (the slices' deviations and rejected items, the integration fixes, the open issues), PR #43's and #44's CI fixes; §3 items 47–49 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WF4 (01 §9's one clock with the re-armed timer and the slow-call limit, §9.1, §9.6, §11.5, §11.8, §11.9's `MediaReason` as built, §14's core row and D10 without slack; 03 §10.1, §10.5), WF5 (01 §10.1: the loss row from the last second's `arrived`, the growth row's 0.75, the constants; §14's sfu row and D9's measurements), WA63 (02 §10.1: the visually hidden code, the decide Notice, the 401 path, `errors.invalid_token`; §10.2: `link.devices.*`, the focus after Remove, `appLinkFor` as built; §16.3; 03 §18.1's rule 6), WA64 (02 §16.2: the Manager tests and cases 1, 4, 6 and 7 as built), WA65 (03 §6.3's `ShareView \| null`, §7.1, §7.3's `lazySharing`, §7.6's `useShareAvailability`, §9's `followShareRoom`, §10.2's `shareStore` and `NativeShareButton`, §10.3, §10.4's `localPreview` and `localShares`, §10.5's end texts and the `ShareCaps` gap, §22.3, §22.4, §23), WA66 (06 §7.3, §7.4: the wiring line in `http.go`, the double render, the `OperatorError` with no `Reason`, the dev proxy; §16; R11), PRs #43–#44 (01 §14 D10's `engineTap`, §12.4 item 2's CI baseline as an owner action; 06 §10.1's `e2e` 45 minutes and the apt-get retries; §4's lessons); §3 items 43–45 as built and 47–49 in their sections (01 §9.4, §14; 06 §7.3; the rows of WA54, WA63–WA66, WA73, WA77, WA80, WA82, WF4 and WF5); "How the plan works" says how group 8 runs; §8 has the `latency.json` baseline; this table gains WB7's, WF6's and WF7's rows. M1 04's rows for WA66 (§2, §4.2, §9.2) are outside `docs/m2` and go to the integrator. §9's review log and the earlier WB rows stay historical |
| WF6 | 8 | ratectl: a re-init encoder's decrease below the change floor | §3 item 47; 01 §9.4; group 7's WF5-R3 | `internal/client/ratectl` (the re-init guard) and its tests, `internal/server/itest` (`desktop_congestion_test.go`: only if D10 needs a bound change) | WF5 | 0.5 | Mac, CI | A re-init encoder's layer whose target must fall by less than `ReinitMinChange` (30 %) to get under a congestion cap, or rise by less than it to reach its ceiling, still gets there: the change floor applies to optional moves, not to one that a cap or the ceiling forces (a decrease to a cap is always allowed; the last step up to the ceiling is allowed when it is under 30 %), still at most one change per `ReinitMinInterval`; ratectl unit tests for both; D10 still passes |
| WF7 | 8 | Server: M2's dev env names are reserved | §3 item 48; 06 §7.3; group 7's WA66 note | `internal/server/config` (the reserved env list) and its tests | WA66 | 0.5 | Mac, CI | `ISSHONI_DESKTOP_DEV_URL`, `ISSHONI_FAKE_AUDIO`, `ISSHONI_E2E_*` and `ISSHONI_NATIVE_*` (and any other `ISSHONI_*` name the repo's tasks, tests or desktop code read: grep for them) no longer make the server warn about an unknown env var; a test lists them; unknown names still warn |
| WA77 | 9 | Audio in the app: What friends hear | 03 DK8 = 05 A9; 03 §8.4, §10.3, §12.1; 05 §14, §22 | `internal/desktop/bridge` (`audio.go`), `internal/desktop/share` (the SharedPID hand-off at pick, release and expiry; 05's other calls came with WA58 through `share.AudioPolicy`, 03 §10.1), `internal/desktop/settings` (`audio.SettingsStore`), `internal/desktop/app` (`SelfInfo`, `SetVisible`, `EventPlaybackState` → `SetViewerBrowser`), `internal/desktop/winos` (the shell relaunch), `cmd/isshoni-desktop` (`--sound-check-helper` right after main's first action, the relaunch step), `web/src/share/native/` (What friends hear, levels, the mic prompt card, banners, the volume note; in the ShareSheet and the first-share sheet too, with `Audio.Watch(true)` while they are open: WA65's hand-over, 03 §10.2), `web/src/desktop/` (the sound and voice wizard steps, Settings → Audio), `web/src/viewer/` (the endpoint-mode viewer mute) | WA37, WA65, WA71 | 3 | Mac, CI ubuntu/windows; owner's PC | Component tests of the panel, sheets and wizard steps; the share sheet renders under every `ISSHONI_FAKE_AUDIO` scenario (one snapshot each); `desktop-link.spec` runs the real sound and voice steps on the fake; the relaunch test passes where a shell exists; `bridge.Audio` binds `DismissPrompt` and no sound-mode method (§3 item 15); Settings → Audio shows the built-in list as locked text ("Always kept out: Discord, TeamSpeak, …"), the mic toggle, the per-app rules and the sound-check rerun, with no sound-mode control. Manual (owner's PC): with Discord in a call, the list shows Discord kept out and a browser heard; Share sound off, pause and resume stay silent; 05's R4 passes; M-DESK-5. `main.go` passes 05's `audio.Service` as `app.Deps.Audio`, where `app.New` finds `share.AudioPolicy` and `AudioConfig` (03 §6.1), and as `bridge.Deps.Audio`, which WA48 left nil, so `Audio.*` answer `not_implemented` until then (03 §6.2) As run (group 14, §3 item 85): WA71 is merged; from the hand-over notes, `audio.SettingsStore` must return `audio.DefaultSettings()` when `settings.json` has no `audio` section, and `main.go` still passes no `Audio` to `bridge.Deps`; WF14 changes `internal/desktop/app` in the same group, only for the toast removal (§5's shared files) |
| WA78 | 9 | Desktop notices and the native license gate | 06 R6; 06 §9 | `tools/notices` (`-native`, `-native-root`), `deploy/notices/` (the gate's checks of `native-windows.json`, `licenses/webview2-bootstrapper.txt`), `tools/desktoppkg` (`stage`: the LGPL files), `.github/workflows/ci.yml` (`desktop-macos`: go-licenses; `windows-desktop`: the notices step) | WA72 | 1 | CI ubuntu/windows/macos | The installed `THIRD_PARTY_NOTICES.txt` lists Wails' modules, the npm packages and the native section; a GPL native entry, LGPL without dynamic linking, an unlisted vcpkg port and a `LicenseRef` without an exception each fail; the `licenses\ffmpeg\` files are staged As run (group 14, §3 item 85): WA72 is merged; the shipped and downloaded third-party pieces are FFmpeg r2 (with the libvpl patch, §3 item 69) and OpenH264 2.7.0 (downloaded at run time, never shipped, §3 item 66); `native-windows.json`'s C++/WinRT entry names `licenseFiles` `cppwinrt/LICENSE` while the file is at `native/windows/third_party/cppwinrt/LICENSE`, so the gate confirms the staging path (group 11's note); WF15 changes `ci.yml` in the same group, only its paths filter (§5's shared files) |
| WA79 | 9 | M2 exit run book | 01 §12.2; 04 §18.4, §18.5; 05 §18.8–§18.10, §19.3; 06 §11.8 | `docs/m2/exit-test/` (`README.md`, `results-template.md`) | WA08, WA62b, WA76 | 1 | Mac | §6's outline written out with exact commands: preconditions (the rc's `isshoni-exit-tools` artifact and its hash check, the `ci\` DLL for `avsync`), the audio half (the gate, R1–R5, R7, `avsync`), the latency half (the `gate`, `nocuts` and `nosound` runs), the release-candidate checks, the record template and the pass rules; no real names, hosts or IPs; every link resolves As run (group 16, §3 item 101): WA76, WF16 and WA81 are merged. `avsync` reads WF16's luma tap, so it runs with `-dll` at the `ci\` DLL (a DLL without test hooks exits 2), opens the flash window on the captured monitor for `-source mon:<id>` and counts `edgesDiscarded` in `avsync.json` (04 §5.8, §14); `isshoni-exit-tools` is `desktop-pkg`'s workflow artifact (kept 90 days, not a release asset), its DLL checked against `native-manifest.json`, and `verify-windows` runs its `isshoni-audiotest selfcheck` (§3 item 99). The commands and flags are the merged tools' (`cmd/isshoni-enginetest`, `cmd/isshoni-audiotest`, `hwcheck.ps1`, `release.yml`, `audiotest.yml`), never invented ones. While §7 question 11 is open, the record template has a row that records `protect-check`'s WGC `not_reachable` as "WGC not covered on this PC", never as a WGC pass As built (group 16, §3 item 102): the book and the template, with the Windows 10 rules of 05 §18.10 (the gate's exit code recorded, not judged; no R row), the record's folder `results/<date>/` for the g2g and `avsync` JSON files, a `protect-check` row among the release-candidate checks, and one rerun rule (every rc repeats `run -gate`, `avsync` and B; the real-app checks only after an audio change); §6 follows the book |
| WF8 | 9 | Tools modules: the open Dependabot alerts | M1 06 §8 (dependency policy); GitHub's Dependabot alerts on `main` | `tools/go.mod`, `tools/go.sum`, `tools/goreleaser/go.mod`, `tools/goreleaser/go.sum`, `tools/wails3/go.mod`, `tools/wails3/go.sum` (and `tools.json` or `repocheck` pins only if a pinned tool's version must change) | – | 0.5 | Mac, CI | The three open alerts are fixed by the smallest bumps to the first fixed version or later: `github.com/go-jose/go-jose/v4` (a DoS parsing JWKs with RSA keys) in `tools/go.mod` and `tools/goreleaser/go.mod`, and `github.com/klauspost/compress` (s2's integer overflow and out-of-bounds write) in `tools/wails3/go.mod`; `go mod tidy` and `go build` for each module; `task lint:pins`, `task licenses` and the modules' tests pass; `govulncheck` as the repository's task runs it is clean. As built (merged in group 9): go-jose v4.1.4 → v4.1.6, compress v1.18.3 → v1.18.7, nothing else changed; the root module's `govulncheck` notes `x/net` and `x/crypto` vulnerabilities the code doesn't call (06 §10.6) |
| WF9 | 9 | Web: the flaky logout tab-B test | group 8's integration open issue; M1's web test rules | `web/src/auth/logout.test.tsx` (and only the test helpers it uses, if the fix belongs there) | – | 0.5 | Mac, CI | 'a logout in tab A leaves tab B alone while it is on a public page' passes 30 of 30 runs alone (`vitest --run -t` in a repeat loop) and in the full suite; its invite no longer uses a fixed `expiresAt` in the past; it waits for the invite heading as the acceptance needs (`findByRole` with the heading's name, or `waitFor`), not by sleeping; no product code changes unless the test shows a real product bug, which is then reported. As built (merged in group 9): test-only, no product bug; it waits for the heading and the Username field together (the checking state's heading has the same name), and `expiresAt` is a week after the test clock's now (§4's lessons) |
| WA80 | 10 | Share safety: hotkeys, Pause, the Sharing pill, audio alerts outside the window | 03 DK10; 03 §11.1–§11.4, §11.6, §11.7, §15; 05 §14.7 "Outside the window" | `internal/desktop/{share,app,winos,tray}` (hotkeys registered only while live, Pause from every control, the Win32 pill with WDA, accessible buttons and the alert text and amber marker, the DND tip, `tray.State`'s `VoiceAppsHeard` and `Questions`, the `audio.alert` toast, the close-to-tray notice in the window), `web/src/viewer/` (the `paused` tile, no freeze detection while paused), `web/src/share/native/` (the Pause control, the first-share sheet's hotkey line, the window notice), `web/e2e/desktop-share.spec.ts` (Pause, the late joiner, hotkeys, the audio alert case; and WA65's hand-overs: the tray step, a note until WA68 gives the harness its tray, asserting "Live, 1 watching" with WA68's names, and the `normal` scenario's mic prompt step on WA77's card, 03 §22.4) | WA46, WA68, WA70, WA77 | 3 | Mac, CI ubuntu/windows; owner's PC | `desktop-share.spec` with Pause, a late joiner and hotkeys passes, and its audio alert case (`ISSHONI_FAKE_AUDIO=degrade-live`: `tray.Render` with `tray-warn`, "Friends hear voice apps" first and "1 question waiting"; `PillState` with the alert and the marker; exactly one `voiceAppsHeard` toast); `tips.trayNoticeShown` is set only once the window notice was shown; 03 §22.2's pill tests (topmost, no-activate, WDA, DPI, positions per monitor) pass on CI windows. Manual: M-DESK-2, M-DESK-3, M-DESK-8 (Win10), and M-WDA again with the pill As run (group 15, §3 item 94): WA46, WA68, WA70 and WA77 are merged; `im_protect_window` needs a top-level window of this process (04 §11), though the pill's affinity is `winos`'s own `SetWindowDisplayAffinity` (03 §11.5); WA77's mic prompt card is in the panel and sheets, and `MicPromptCorner` (the corner card) is built but not mounted (§3 item 87); `App.ToastDismissed` has no Wails caller; the app's stale toasts leave Action Center through WF14's `removeStaleToasts`, so a new toast kind that a later answer settles goes through `toastBook` (`dropKind`/`dropRefsExcept`) As built (group 15, §3 items 95–96): the Mac's items passed (`desktop-share.spec` with Pause, the late joiner, the hotkeys and the degrade-live case as 03 §22.4 now says; `tips.trayNoticeShown` only after the notice was shown); the pill tests run on CI windows and the manual items on the owner's PC. `PillState` is 03 §11.4's as built, with `Monitor` from the share's source; group 15's integration bound the window notice and mounted it and `MicPromptCorner` in the app shell. The panel's lock-prompt bar and `desktop-share.spec`'s close-to-tray step went to WF17, `Settings.Hotkeys` to WA84 |
| WA81 | 10 | Release pipeline with the desktop artifacts | 06 R13; 06 §11.1–§11.7 | `.github/workflows/release.yml` (`native-windows`, `desktop-exe`, `desktop-pkg`, `verify-windows`; the `build`, `sign`, `sign-dry` and `publish` changes), `.goreleaser.yaml` (`extra_files`, the header), `Taskfile.yml` (`release:assets`), `deploy/release/check-draft.sh` and its test, `.github/workflows/ci.yml` (the server-rendered ps1 test in `windows-desktop`) | WA25, WA49, WA66, WA78 | 2.5 | GitHub dry run; Mac | The release dry run is green end to end with ephemeral keys, with `verify-windows` installing through the released ps1 under Windows PowerShell 5.1; `sign` signs the manifest and `check-draft.sh` checks it; the new `check-draft-test.sh` cases pass; `goreleaser-check` passes with stubs; the server-rendered ps1 test installs a CI build and refuses tampered files, and its non-executable installer case ends with the `isshoni: Windows blocked the installer (` line and no unhandled error; `sign` refuses, and `check-draft.sh` fails, a non-empty `revokedKeys` not signed by the backup key (`check-draft-test.sh` covers both); `desktop-exe` cross-builds the exit tools and `desktop-pkg` uploads the workflow artifact `isshoni-exit-tools` (the tag's `isshoni-audiotest.exe`, `isshoni-enginetest.exe`, `g2g.exe`, `hwcheck.ps1`, and `isshoni-media.dll`, the FFmpeg DLLs and `native-manifest.json` copied byte for byte from `windows-native`'s `release\`; a step fails unless the DLL's SHA-256 equals the manifest's); not a release asset; an `audiotest.yml` dispatch with `release_run_id` = the dry run skips `build` and its `fetch` succeeds As run (group 15, §3 item 94): WA78 is merged, and `desktop:package:windows` passes `-native` and `-native-root` to the notices (§3 item 93), so a `desktop-pkg` job that runs that task gets the native section and its gate; FFmpeg is `deps-ffmpeg-9.0.2-r2`, read from `native/ffmpeg/lock.json`; installer sources join `${STAGE}` with `\` (06 §6.1); the release dry run is the owner's step (it writes permanent public Rekor entries), so the slice builds and lints it and never dispatches it; it is the only slice that changes `ci.yml` in group 15 As built (group 15, §3 item 99): two edits outside the listed paths are WA81's: `deploy/test/windows/smoke.ps1 -Mode release` (with an optional `-Setup`; WA88 keeps it when it fills `-Mode update`) and `release:snapshot`'s body, which writes the desktop stubs through the new `release:desktop-stubs` and exports `ISSHONI_DESKTOP_MIN_VERSION` and `ISSHONI_FFMPEG_VERSION` for the notes' desktop lines (read with `sed`, so a snapshot needs no `jq`); `release:assets` gains `DESKTOP_STUBS` (FULL=true over stubs, for the nightly until WA86) and `PKG_PS1`, and its `DESKTOP=true` needs `jq`, `unzip` and `7z` and checks for them. `windows-native` also uploads `ffmpeg-source` (the locked source tar) and `desktop-pkg` downloads `web-dist` (for `licenses.txt`); `verify-windows` runs `isshoni-audiotest selfcheck` from `isshoni-exit-tools` (06 R13); `goreleaser-check` passed with stubs. The dry run, the server-rendered ps1 test, `desktop-exe`/`desktop-pkg` and the `audiotest.yml` fetch wait for CI and the owner; `nightly.yml`'s `goreleaser-full` fails until WA86 stubs it |
| WA82 | 10 | Settings page and version screens (SPA) | 03 DK12 (SPA part); 03 §14.4, §16.2, §16.3 | `web/src/desktop/` (`SettingsPage`: every §14.4 section, the hotkey field, the update states, About with versions, logs, notices and copy version info, the OpenH264 opt-in), `web/src/app/` (the desktop actions of `VersionMismatch`), `web/src/platform/desktop/codes.node.test.ts` (drop the `update.*` exemption), `web/scripts/check-i18n.mjs` (rule 6, WA63's: map an `update.*` code to `desktop.update.error.<code>` instead of exempting it, 03 §18.1) | WA48, WA77 | 2 | Mac, CI ubuntu | A component test per settings section and per update state of 03 §16.2 (required banner, expired, failed, beta channel, `policyOff`); `VersionMismatch` offers "Update the app" and "Open in browser" (the current room) in the desktop and is unchanged in the browser; every `update.*` code of `internal/desktop/bridge/codes` has its `desktop.update.error.<code>` text in `lazy/desktop.en.json`, and `codes.node.test.ts` and `check:i18n`'s rule 6 check it (§3 item 42, 03 §6.4, §18.1); axe in both color schemes; `check:size` As run (group 15, §3 item 94): WA77 is merged, so `SettingsPage` already renders the Audio section (`#audio`, `AudioSettings.tsx`) and the other sections join it; the `#servers` section renders `desktop/ServerSwitcher` (Switch, Sign out, Remove, Add), because the desktop account menu's Switch server and the Offline screen link there (§3 item 86), with a component test that two servers show Switch; WA51's OpenH264 opt-in reasons (`download`, `corrupt`, `missing`, `cancelled`, `timeout`) need texts As built (group 15, §3 item 97): the sections, update states and `VersionMismatch` actions have their component tests; rule 6's update mapping and `codes.node.test.ts`'s came with group 15's integration (§3 item 96), since this run's paths were `web/src/desktop/` and `web/src/app/` only; the axe check ran once with an uncommitted spec, so the committed `/desktop/settings` case is WF17's |
| WB8 | 10 | Groups 8–9 and WA09 write-backs into docs/m2 | groups 8 and 9's integration and review notes (the slices' deviations and findings, the open issues), WA09's results and the owner's 2026-11-03 decisions; §3 items 50–54 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app` (`spikes/s1-win-audio` and `.github/workflows/s1-audio.yml` included, as on `main`), with no code change: WF6 (01 §9.3's one step to the ceiling, §9.4's guard as built, §14 D10's `forcedMove`; §3 item 47), WF7 (06 §7.3's 22 names and the rule for a new `ISSHONI_*` name; §3 item 48, §4 "Config", the `keys.go` row), WA74 (02 §10.2's `appLinkFor` as built and the go1.27 tables; 03 §12.3's empty link for WA83; 06 §7.5's step 3, `PS1_NEEDS_TLS12_PREFIX` and `links.node.test.ts`, axe in `download.spec`; §16; R12's a11y claim), WA24 (06 §5.1's FFmpeg steps, `release\` and `ci\`, `native-manifest.json`; §10.1's trigger; §17's `native:dist`; R3b), WF8 (06 §10.6's govulncheck note), WF9 and the flakes (§4's lessons), the FFmpeg publish (06 §4.1, §4.6, §14 item 2, R2), WA09 (05 §1, §2, §3, §6.1, §9.5's tail fade with its evidence, §9.6, §12.1, §15, §18.8, §18.10, §19.1, A0, A6, §24); §3 gains items 50–54 and items 47–48 say what was built; "How the plan works" says how groups 9 and 10 run; this table gains WF8's, WF9's and WB8's rows, and the WA09, WA24, WA44, WA45, WA53 and WA62a rows say what was run or decided; §1's exit criterion and §6 have Windows 10's reduced gate; §8 says what the owner's WA09 and FFmpeg actions became. M1 04 §4.2's env list is outside `docs/m2` and goes to the integrator. §9's review log and the earlier WB rows stay historical |
| WA83 | 11 | Handoff and the web entries | 03 DK11 (web part); 03 §12.2, §12.3, §13.2 | `web/src/rooms/` (`handoff.ts`: the `playback.stop` sender in the desktop and its receiver in tabs, the `playback.state` sender, "Open in the isshoni app" from `user.connections`), `web/src/viewer/` (the `handedOff` state, "Watch here instead"), `web/src/protocol/signal-client.ts` (hello features `agent.relay`, `user.connections`), `web/e2e/desktop-handoff.spec.ts` | WA55, WA63, WA77, WA80 | 2.5 | Mac, CI ubuntu; owner's PC | `desktop-handoff.spec`: a same-user tab stops with "Now watching in the isshoni app", "Watch here instead" resumes and its `playback.state` reaches the app, closing the tab sets `playing: false`, a phone-emulated viewer keeps playing; 03 §13.2's deep-link table tests. Manual: M-DESK-7, M-DESK-9 As run (group 16, §3 item 101): WA80 is merged (the `paused` tile, Pause from every control; the app shell mounts the window notice and `MicPromptCorner`, §3 item 96). WA77's endpoint mute is still attached from `ViewerLayout` (§3 item 87): when handoff's playback state touches it, this slice moves `attachEndpointMute` into `web/src/rooms/runtime.ts` As built (group 16, §3 item 103): the handoff, `OpenInApp` in the browser room page's header and `DesktopLinks` (mounted by `InRoomBar`) as 03 §12.2, §12.3 and §13.2 now say; the tab repeats `playback.state` 3 s after a desktop connection appears until Go subscribes before `Ready`; ShareSheet's and ScreenAudioWarning's `/download?room=`, the Windows-only rule, the `{switch}` link's room and the `too_many_servers` link are WF18's. Manual M-DESK-7 and M-DESK-9 wait for the owner's PC |
| WA84 | 11 | Settings services, autostart and updates in the app | 03 DK12 (Go part); 03 §13.4 step 5, §14.2, §16.1, §16.2 | `internal/desktop/bridge` (`settings.go`, `updates.go`), `internal/desktop/app` (the updater: refused while live, the tray offer at an autostart start), `internal/desktop/winos` (the `Run` value), `cmd/isshoni-desktop` (the start-time apply with `PendingRoom`, `--update-now`, `--autostart`) | WA40, WA59, WA80, WA82 | 2.5 | Mac, CI ubuntu/windows; owner's PC | `Settings.*` and `Updates.*` unit tests; the autostart round trip on CI windows (`--autostart` opens no window); an update offered in a test channel is shown, refused while live and applied at the next start with the Pending values (end to end in WA88); the OpenH264 opt-in installs and removes through WA51's helpers. Manual: M-DESK-10 As run (group 16, §3 item 101): WA80 and WA82 are merged, and the slice's paths also take `web/src/platform/desktop/` and `web/src/desktop/` (wiring the new bindings) and the regenerated bindings. Its hand-overs: the bound `Settings.Hotkeys` maps `app.App.Hotkeys()` (until then the SPA shows `Settings.Get()`'s combinations as idle); the OpenH264 opt-in's methods are bound and `openH264Control()` builds a control over them (`OpenH264OptIn.tsx` declares `OpenH264Control` and the reason texts); `Settings.Update` beyond `wizardDone`, `TestHotkey` and `Updates.*` are bound; WA82-R1 and R4 were fixed in WA82's review. Once `Updates.*` work, the required-update banner and the desktop version screen can join `a11y.spec.ts`'s desktop case (WA87 or the integrator; §3 item 97) As built (group 16, §3 item 104): every `Settings.*` and `Updates.*` method and the OpenH264 methods are bound; the start-time update, `--update-now` (`updatetest` only) and the deferred window of an `--autostart` start are 03 §13.4 and §16.1 as they now read; the paths also took `bridge/bridge.go`, `app/share.go` and `winos`' non-Windows stubs (accepted); the autostart round trip runs in `windows-desktop` since group 16's integration (§3 item 108); the end to end is WA88's; `settings.changed` after the app's own writes is WF18's. Manual M-DESK-10 waits for the owner's PC |
| WA85 | 11 | Project site and docs for the app | 06 R14; 06 §12.2, §13 | `docs/download.md`, `docs/privacy.md` (`#desktop`), `docs/code-signing.md`, `docs/security.md` (`#keys`, `#verify-desktop`), `docs/troubleshooting.md` (the desktop anchors), `.github/workflows/site.yml` (`/keys/update_signers`, the `release.json` desktop fields), `internal/server/ops/doctor/docsanchors_test.go`, `README.md` (the keys table, the Windows app), `deploy/keys/README.md` | WA57, WA81 | 1 | CI ubuntu | The `site` job and `TestDocsAnchors` pass with every anchor of 06 §17; nothing is loaded from another origin; `/privacy#desktop` states 03 §19's and 02 §9's facts, and `/privacy#linked-apps` links to it (WA57 linked `/security#linked-apps` there while `#desktop` didn't exist, 06 §13); `TestDocsAnchors` includes `/troubleshooting#desktop-linking` (03 §8.1, §9). Hand-over (§3 item 87): WA77's sound check (`SOUND_HELP` in `SoundCheck.tsx`, its result's "Learn more") opens `troubleshooting#desktop-sound`, so `docs/troubleshooting.md` gains a `{#desktop-sound}` section on desktop sound (what friends hear, kept-out apps, the endpoint mode) and `TestDocsAnchors` lists it As run (group 16, §3 item 101): WA81 is merged (the release pipeline, `release.json`'s desktop fields, `update_signers`); the `#desktop-sound` hand-over above stands, and every anchor the app or the docs link to is in `TestDocsAnchors` As built (group 16, §3 item 105): `site.yml` serves `/keys/update_signers` and does not check the update manifest (`check-draft.sh` and the app do); `TestDocsAnchors` also scans the product's links; the `#windows-smartscreen` screenshots are WA91's |
| WA86 | 11 | Nightly additions | 06 R16; 06 §10.3 | `.github/workflows/nightly.yml` (`windows-full`, `native-fuzz`, `windows-asan`, `goreleaser-full` with real desktop files, `compat-servers`, the cross-OS legs) | WA12, WA55, WA64, WA81 | 1 | GitHub | One green nightly with every row, or `compat-servers` skipped with its note while fewer than two final releases exist; against `v0.1.x` it asserts only `update_server`/`no_device_flow` As run (group 16, §3 item 101): WA81 is merged, and since then `goreleaser-full` (goreleaser called directly, so no stubs) fails `release:assets FULL=true`: until it builds real desktop files it runs `task release:desktop-stubs VERSION=…` before goreleaser, `release:assets … DESKTOP_STUBS=true`, and exports `ISSHONI_DESKTOP_MIN_VERSION` and `ISSHONI_FFMPEG_VERSION` as `release:snapshot` and `release.yml`'s `build` job do (§3 item 99). A slice can't run the nightly: it is linted (actionlint, repocheck's workflow tests), stays schedule and dispatch only, and its first green run is the owner's or the next scheduled run's As built (group 16, §3 item 106): `windows-full-native`, `windows-full` and a `web` job; `goreleaser-full` after `windows-full`, `DESKTOP=true` over its real files, else the server half over stubs and a failure; `compat-servers` with each tag's own e2e config; the updater e2e a placeholder step until WA88. Not run yet: the first nightly (WA81's 7z and syft points) |
| WB9 | 11 | Group 10 write-backs into docs/m2 and the M1 reserved env list | group 10's integration and review notes (the slices' deviations, findings and rejected items, the integration fixes, the open issues); §3 items 55–59 | `docs/m2/*.md`, `docs/m1/04-server-platform.md` (only §4.2's reserved env list) | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA44 (04 §9.1's "Before WA44" paragraph in the past tense with what WA44 built and left to WF10; §9.2's AU capture times as merged; §9.10's budget, 720p retry, quick probe and tests as built; §14's `-bitrate-steps` GOP deviation, the exit 2 until WF10, `-dump-aus`, the Ogg granule, the process-default check in CI; §18.1–§18.3's fixtures, `ffmpeg_load_test` and `probeFallback`), WA45 (05 §12.1's top-level `warnings[]` with `instance_gone`, the kept writer, the live report; the tail-fade text stays as WF11 implements it), group 10's integration (06 §7.3's two names; 01 §6.4's check 4), M1 04 §4.2's reserved list equal to `keys.go`'s `reservedEnv` (06 §7.3's names, WF7's and group 10's); §3 gains items 55–59; §4 gains group 10's lessons and the M1 list rule; "How the plan works" says how group 11 runs; the shared-files rows of `native/windows/tests/` (`ffmpeg_load_test.cpp` for the `ProbeFlags` check), `keys.go`, `cmd/isshoni-enginetest` and `docs/m2/*.md`; this table gains WB9's, WF10's and WF11's rows, and the WA44, WA45, WA50, WA53 and WA59 rows say what was built or handed over; §8's owner items wait for WF10 where they need an encoder. §9's review log and the earlier WB rows stay historical |
| WF10 | 11 | WA44 follow-ups: real encoders in the catalog, AU capture times, per-layer GOP | §3 item 55; 04 §9.1, §9.2, §14; group 10's WA44 review (R3, R4, R5, R11; R1, R2 and R6 were fixed in WA44's review, 04 §9.10, §18.2) | `native/windows/src/session/` (`encoder_catalog.cpp`, `EncodedAu` and the session's AU stamping), `native/windows/src/encode/` (only what these items need), `native/windows/tests/`, `internal/media/native` (an optional per-layer GOP on `StartExtra` mapped to `im_layer_config.gop_ms`; `session_dll_test.go`'s `probeFallback`), `internal/media` (`media.Config`, only if the GOP field belongs there), `cmd/isshoni-enginetest` (`capture.go`, `check.go`) | WA44 | 1.5 | Mac (Go), CI windows | `encode::CatalogEntries()` is in the session's encoder catalog in 04 §9.1's order for `IM_ENC_AUTO`, and `session_fake_test`, `mf_sw_test` and the encpolicy fallback tests still pass with the real entries present (tests that assumed none are fixed with their intent kept); an AU carries the capture time of the frame it encodes (a unit test with a fake encoder that holds one frame back gives each AU its own frame's time and the first IDR the first frame's); `media.Config`/`native.StartExtra` take an optional per-layer GOP that reaches `im_layer_config.gop_ms` (Go and C++ tests), and `capture -bitrate-steps` uses `f`'s GOP of 10 000 ms with `checkBitrateSteps`' periodic-IDR tolerance and `report.json`'s deviation removed; `probeFallback` is gone and `TestDLLConformance` uses the session engine directly. WA44-R1 (the vendor opens in `ffmpeg_load_test`'s child), R2 (the 1500 ms encoder budget) and R6 (a quick probe returns the stored full probe) were fixed in WA44's review (04 §9.10, §18.2) and need nothing here. Then WA44's hardware "Manual" items and the Windows AU fixtures can run (§8). As built (merged in group 11, §3 item 60): the per-layer GOP is `native.StartWith`/`StartExtra` with `WithGOP` (and `GOPOf`), `media.Config` unchanged; `session/au_times.h` stamps each AU with its frame's capture time and refresh flag; `StartFrames` follows the first candidate only; `probe.cpp`'s `static_assert` keeps the budget plus one 1 s open inside `bound::kProbeFull` (04 §6.1, §9.1, §9.2, §9.10, §12.2, §14, §18.2, §18.3) |
| WF11 | 11 | Audio report: the tail-fade counter and event, `instance_gone` | §3 items 53, 56, 57; 05 §9.5, §12.1, §19.1; group 10's integration notes | `native/common/audiomix/` (the report writer), `native/windows/src/audio/` (the watcher's event and counter mapping), `internal/media/audio.go` (`AudioCounters.TailFades`, the `tail_fade` event code, `AudioWarning`'s comment naming `instance_gone`), their goldens and tests (`audio_golden_test`, `mix_report_test`) | WA45 | 0.5 | Mac, CI | The audio report gains the additive counter `tailFades` and the event code `tail_fade` (05 §9.5, §12.1; §3 item 53), written by the one C++ writer from the mixer's `tail_fades` and its tail-fade event (at most once per second per channel, as the spike), with the Go types, goldens and both golden tests updated together; `instance_gone` is named in `AudioWarning`'s comment and accepted by any Go check of warnings; reports without the new field still parse. As built (merged in group 11, §3 item 60): the writer change is in `native/common/audiomix` (and `mix_engine.h`'s `kTailFade` comment); `audio/report.cpp` needed none; `internal/media/audio_test.go` gained `tailFades` (05 §12.1) |
| WA87 | 12 | Real-app e2e and the desktop manual matrix | 03 DK13; 03 §3.2, §20, §22.4 B, §22.5 | `web/e2e/` (the CDP spec complete), `docs/m2/03-desktop-app.md` (§3.2 results and §20 measurements only) | WA80, WA82, WA83, WA84 | 2.5 | CI windows; owner's PC | The CDP spec is green on CI windows (W1–W3, W5, W7, W9, W13, W16, W21's assertions: the runtime's `Browser.OpenURL('ms-settings:…')`, `('search-ms:…')`, `Clipboard.Text()` and `Window.Hide()` get 403 and no window opens, a device-flow link, a fake share watched in Chrome, close-to-tray while live, Quit within 3 s; a production build's debugging port stays closed); every M-DESK-1…12 row passed on Win11 and Win10 or has an accepted, recorded exception; 03 §3.2 is complete (the rows copied from the PRs' "Wails checklist" sections); 03 §20's startup and memory budgets are measured As run (group 17, §3 item 109): WA80, WA82, WA83, WA84 and WF17 are merged. The CDP spec skeleton is `web/e2e/cdp/desktop.cdp.ts` (W5 and W7 pass on CI windows with W5's fallback, §3 item 92); a check the GPU-less, elevated `windows-2025` runner can't run gets a skip that names the runner fact, never a weaker assertion; the M-DESK rows and 03 §20's measurements stay the owner's (§8), so their rows are written as not run with the exact steps. WF18 changes `a11y.spec.ts` in the same group, not the CDP spec |
| WA88 | 12 | Updater end to end on Windows | 06 R9; 06 §8.11, §16 | `.github/workflows/ci.yml` (`windows-desktop` on `update` changes), `.github/workflows/nightly.yml` (`windows-full` always) | WA72, WA84, WA86 | 1 | CI windows | An installed `0.0.1-ci.1` updates to `ci.2` within 120 s through `--update-now` and relaunches with `--updated-from=0.0.1-ci.1`; a wrong SHA-256 leaves `ci.1` intact; a manifest for `ci.0` is ignored; `smoke.ps1 -Mode update`: under a held installing mutex a start leaves `attempts` at 0 and starts no installer, and the next start applies the pending `ci.2` and hands the link over As run (group 17, §3 item 109): WA84 (`Updates.*`, `--update-now` in `updatetest` builds with 06's default options, the start-time update writing and taking back the Pending values) and WA86 (`windows-full`'s placeholder step "The updater e2e (README WA88)", which this slice replaces) are merged; `smoke.ps1 -Mode release` is WA81's and the installer WA67's. The test channel and its manifests are signed with ephemeral keys made in the job, never the release keys |
| WA89 | 12 | Device-auth manual checks (owner) | 02 DA13; 02 §16.4 | – (results in the PR) | WA67, WA71, WA84 | 0.5 + the runs | owner's PC | Manual: M-DA-1 to M-DA-9 on Win11 and Win10 22H2 pass or have an issue filed |
| WB10 | 12 | Group 11 write-backs into docs/m2 | group 11's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues), PR #52's libvpl and MF software changes; §3 items 60–65 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WF10 and WF11 as built (01 §6.4 and its Windows CI note; 04 §6.1, §8.1, §9.1, §9.2, §9.10, §14, §18.2, §18.3; §3 items 55–57 in the past tense), 04 §12.2 with `StartWith`, `StartOption`, `WithGOP` and `GOPOf`; WA50 (04 §7.2's hidden owner window, the late picker thread, `monitor_id` empty until WA60, the self rule; §7.3's `WgcCapture`, `MinUpdateInterval100ns` and the trackers in `source_watch.h`, what is left to WF12; §9.10's OS flags), WA53 (05 §6.4, §6.5's "twice in 10 s", §11.2, §11.3, §12.1's live probe and state; §4.3 and §6.1's stale "until WA53"), WA59 (03 §13.4 step 1 and step 2a as built), 05 §12.1's top-level `warnings[].code` values (`multi_process_session`, `instance_gone`) and WF11's `tailFades`; PR #52's libvpl patch and MF software probe in §3; §3 gains items 60–65; "How the plan works" says how group 12 runs; the shared-files rows of `session/`, `encode/` and `capture/` for group 12; this table gains WF12's and WB10's rows, and the WA50, WA51, WA52, WA53, WA59, WA67, WA68, WA69, WF10 and WF11 rows say what was built or how they run; §8's owner items follow WF10 and WF12 and add the r2 publish. §9's review log and the earlier WB rows stay historical |
| WF12 | 12 | WGC capture in the session; the probe asks Windows for the WGC flags | §3 item 61; 04 §5.3, §7.1–§7.3, §7.6–§7.9; group 11's WA50 hand-over and integration notes | `native/windows/src/session/` (`session_object.{h,cpp}`: start `capture::WgcCapture` for a WGC source, its sink, the supervisor's `SOURCE_*` events, `IM_SESS_NO_OS_INDICATOR` only when borderless, `SetFps`, the stop order), `native/windows/src/encode/probe.cpp` (`BuildFlags` uses `capture::OsFlags()`), `native/windows/src/capture/` (only what the wiring needs), `native/windows/tests/` | WA50 | 1.5 | CI windows; owner's PC | A session whose source is a WGC pick starts `capture::WgcCapture` with the start flags (hide cursor, keep border, layer 0's fps) on the GPU path; `IM_SESS_NO_OS_INDICATOR` is set only when the capture is borderless; frames reach the encoder (`frames_captured_`, the video waiter); a closed source ends the session with `IM_ERR_SOURCE_GONE`; `ResizeTracker` sends `SOURCE_RESIZED` and re-inits the layers whose size changes (`REINIT\|KEYFRAME`); `MinimizeTracker` sends `SOURCE_MINIMIZED`/`SOURCE_RESTORED`; a layer-0 fps re-init calls `SetFps`; `Stop` stops the capture before `ReleaseVideo` frees the slots and the device; the probe's WGC OS flags come from `capture::OsFlags()` (`IM_OS_WGC_BORDERLESS` when Windows grants it); tests with a fake capture source (an interface seam if needed) for the event and stop-order rules run on `windows-native`, and the pure trackers' tests run on the Mac where they build. Manual (owner's PC): M-WGC-WIN, M-WGC-MON, M-SECONDARY As built (group 12, §3 item 67): the `capture_source` seam, the probe's 250 ms borderless wait (04 §7.3, §9.10) |
| WF13 | 13 | WA68 leftovers: the heartbeat, the long-lock fixtures, `end-session-confirmed`, Action Center | §3 item 72; 03 §11.5, §11.6, §15, §17, §22; group 12's WA68 hand-over | `web/src/platform/desktop/` (`App.Heartbeat` on 03's interval), `web/e2e/fixtures.ts` (`lockResumeMs`, `lockStopMs`), `web/e2e/desktop-system.spec.ts` (WA68's, for its long-lock and end-of-session cases), `internal/desktop/e2ehost` (the `end-session-confirmed` event), `internal/desktop/winos` (removing a toast from Action Center), their tests | WA68 | 1 | Mac, CI ubuntu/windows | The desktop SPA calls `App.Heartbeat` on 03's interval, so W17's watchdog arms (a test with a fake bridge: heartbeats arrive; in the Go test their absence trips the watchdog); `web/e2e/fixtures.ts` offers `lockResumeMs` and `lockStopMs` and `desktop-system.spec`'s long-lock cases use them; e2ehost accepts `end-session-confirmed` and the spec covers it; a toast that no longer applies is removed from Action Center (03 §15), with a Windows test on `windows-go`; lint, typecheck and the web tests pass |
| WB11 | 13 | Group 12 write-backs into docs/m2 | group 12's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues), the libvpl and MF software findings; §3 items 66–74 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: WA51's pin 2.7.0 over HTTPS (§5's pins table, 04 §9.9 and §12.5, 06 §9.2's notices example and §12.3's privacy row) and its Reasons `download` and `corrupt` (04 §5.7); WF12 as built (04 §7.3, §9.10's "OS flags", item 55); 05 §4.3's note that `im_audio_set_muted` covers the test tone; every line that said the FFmpeg r2 lock was pending says it was published (`deps-ffmpeg-9.0.2-r2`, the libvpl patch) and committed (README, 04 §14, §17; 06 §4.1, §4.6, §14); the MF software finding (04 §9.10, §17, §18.2: on the `windows-2025` runner the MFT gives complete AUs only on drain, so the probe refuses it; `mfenc` sets `CODECAPI_AVLowLatencyMode` as `VT_UI4` where Microsoft documents `VT_BOOL`, a likely cause and a candidate FFmpeg patch for a later revision; `mf_sw_test` skips on Windows Server only); WA52, WA67, WA68 and WA69 as built (04 §5.6, §8.5; 06 §5.1, §5.2, §6; 03 §6.1, §11.6; 01 §12.3); §3 gains items 66–74; "How the plan works" says how group 13 runs; the shared-files rows for group 13; this table gains WF13's and WB11's rows, and the rows of group 12's slices and of WA60, WA61, WA71, WA72, WA73 and WA75 say what was built or how they run. 04 §7.5 is WA60's in this group; §9's review log and the earlier WB rows stay historical |
| WF14 | 14 | App follow-ups: stale toasts removed, the desktop account menu | §3 items 80 and 84; 03 §7.5, §8.3, §15; group 13's WF13-R3 and WA71-R4 | `internal/desktop/app` (the `RemoveToasts` wiring: `toastBook.dropKind`, `clearLockLocked`, the pending toast keeping its notification id, an optional remover interface, serialized with `Shell.Notify` and never under `App.mu`), `web/src/rooms/AccountMenu.tsx` (the desktop variant), `web/e2e/desktop-system.spec.ts` (the Action Center branch), their tests | WA68, WA71 | 1 | Mac, CI ubuntu/windows | The app removes a toast from Action Center when it no longer applies (the `winos` `RemoveToasts` call through an interface the tests fake), and `desktop-system.spec`'s note-only branch now expects `expect.poll(...).toEqual([toast.ID])`; the desktop account menu shows Settings, Switch server, Account, Admin and "Sign out of <server>" (through `Auth.Unlink`; the desktop never POSTs `/auth/logout`), with component tests, and the browser's menu is unchanged; lint, typecheck, web tests and Go tests pass |
| WF15 | 14 | repocheck: the workflow guards run on workflow PRs and see self-hosted runners by labels | §3 item 83; 06 §10.4, §10.5; group 13's WA75 findings | `tools/repocheck` (the workflow tests), `.github/workflows/ci.yml` (only the filter that decides when `test-go` or a repocheck step runs) | WA75 | 0.5 | Mac, CI | A PR that only edits `.github/workflows/**` runs the repocheck workflow tests (a filter change in `ci.yml`, or a repocheck step in `lint-deploy`); a job counts as self-hosted when its `runs-on` labels match a registered self-hosted label set (`isshoni-audio`, `win11`, `win10`, from `.github/actionlint.yaml` or one list in repocheck), when it uses a runner group, or when the literal `self-hosted` appears, not only the literal; the no-token rule covers job- and workflow-level `env`; tests for each case |
| WB12 | 14 | Group 13 write-backs into docs/m2 | group 13's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues), PR #53's CI fixes; §3 items 75–85 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: 03 §8.2 (the app's `OnAuthState` opens the approval page; `Auth.Link`'s `ErrBadResponse` is `internal {reason: bad_response}`, also in §6.4); 01 §14 (the desktop itests on a bearer device, D20 `TestDesktopForcedRefresh` and D21 `TestDesktopDeviceRevoked`); 03 §15, §20 and §22.5 (WF13's toast removal, the heartbeat as built with its gap while the window is hidden, M-DESK-11's variant); PR #53's fixes: the MTA held for the process and why (C++/WinRT's factory cache; 04 §2, §6.1), `tests/support/crash_report.cpp` (04 §13.4, §18.2), `exports_test`'s init cycles (04 §18.2), `audio_live_test`'s engine expectation (05 §19.2), `openh264_test`'s lower bound only where the content fills the target and M-OPENH264's lower bound with real moving content (04 §18.2, §18.4); WA60 as built (04 §6.1, §7.4, §7.5, §7.8: the pointer blended on the DD thread, the `DisplayName()` table pending the owner); WA61 as built (04 §9.1: step 3 not built); the `audiotest` environment as an owner action (§8); §3 gains items 75–85; "How the plan works" says how group 14 runs; the shared-files rows for group 14; this table gains WF14's, WF15's and WB12's rows, and the rows of WA70, WA76, WA77 and WA78 say how they run. 04 §7.5's table and 04 §9.3 wait for the owner's runs; §9's review log and the earlier WB rows stay historical |
| WF16 | 15 | A/V sync: the luma tap's producer | §3 item 89; 04 §14 (`avsync`), 04 §5 (packet kinds); group 14's WA76-3 | `native/windows/src/session/` (an `IM_TEST_HOOKS`-only `IM_PACKET_DEBUG` luma-tap producer), `native/windows/tests/` (its own test file), `internal/media/native` (a `StartOption` that asks for the tap and passes its packets on), `cmd/isshoni-enginetest` (`avsync.go` uses it), `docs/m2/04-windows-engine.md` (the payload) | WA76 | 1 | CI windows, Mac (Go) | In an `IM_TEST_HOOKS` build, a session started with the luma-tap option emits `IM_PACKET_DEBUG` packets carrying the mean luma of a fixed region per captured frame with its capture time (the payload specified in 04); release builds never emit them; a C++ test on the fake/WARP path and a Go test of the `StartOption`; `isshoni-enginetest avsync` pairs them with the PCM tap and its unit test passes. Manual: the owner's first `avsync -dll ci` run on Win11 As built (group 15, §3 item 98): 04 §5.8's tap and payload; the start flag lives in `luma::kStartFlag` and `luma.go` only, not in the public header; the review settled R1 (the flash window on the captured monitor), R2 (`edgesDiscarded`), R3 (04 §12.1, §12.2) and R4 (the wrap). The Go checks and `avsync`'s unit tests passed on the Mac; the C++ tests, `TestDLLLumaTap` and the MSVC `/W4 /WX` build wait for CI windows, and the first `avsync -dll ci` run for the owner's PC |
| WB13 | 15 | Group 14 write-backs and PR #53's CI lessons into docs/m2 | group 14's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues), PR #53's CI fixes; §3 items 86–94 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: 03 §7.5 (the desktop menu built by WF14; Switch server → `#servers`; My invites in the browser); WA77 as built in 03 §6.2, §8.2, §10.3, §12.1, §15 (`wizardDone` through `Settings.Update`, the corner card built but not mounted, the endpoint mute in `ViewerLayout`, `ToastDismissed` without a Wails caller, `WindowFocused`); 04 §14's `protect-check` as built (presence by bit rate, a listed `mon:` always DD, WGC `not_reachable` exits 0, an owner decision in §7) and `hwcheck.ps1`'s `-wgc off`; 04 §11 (a top-level window); 04 §18.2's `protect_window_test` row; the WA70 row; PR #53's lessons as built: 04 §8 and §13 (the MTA held, no umbrella libraries, `runtimeobject.lib`, `GetDpiForMonitor` at run time), 03 §3.2's W5 (fails at beta.34; the fallback as built) and W7 (holds; `noDefaults`), 03 §13.4 (the relaunch watches the child; the hosted runner's 0xC0000142), 06 §6 (makensis backslashes), 06 §10.1 (every independent failure and every skip reported); §3 gains items 86–94 and §7 question 11; "How the plan works" says how group 15 runs; the shared-files rows for group 15; this table gains WF16's and WB13's rows, and the rows of WA70, WA80, WA81 and WA82 say what was built or how they run. `docs/troubleshooting.md`'s `#desktop-sound` anchor is WA85's (a site page); 04 §7.5's table and 04 §9.3 still wait for the owner's runs; §9's review log and the earlier WB rows stay historical |
| WF17 | 16 | Group 15 follow-ups: the lock-prompt bar, the close-to-tray e2e step, the settings axe case, the luma tap's monitor | §3 items 95, 97, 98; 03 §11.5, §11.6, §22; 04 §14 (`avsync`); group 15's open issues (WA80's lock prompt and close-to-tray step, WA82-R3, WF16-R1 and R2) | `internal/desktop/bridge` (`ShareView.lockPrompt` only), `internal/desktop/share` or `internal/desktop/app` (filling it from `App.sys.prompt`), `web/src/share/native/` (the "Sharing is still paused since you locked your PC. [Resume sharing] [Stop]" bar), `internal/desktop/e2ehost` and `web/e2e/desktop-share.spec.ts` (a harness hook that calls `WindowClosing`, and the close-to-tray step), `web/e2e/desktop-system.spec.ts` (the long-lock case: the panel's lock-prompt bar), `web/src/platform/desktop/bridge.ts` (`ShareView.lockPrompt` only), `web/e2e/a11y.spec.ts` (`/desktop/settings` in light and dark with the hotkey recorder and the licenses dialog open), `cmd/isshoni-enginetest` (`avsync`: the flash window on the captured monitor for `-source mon:<id>`, WF16-R1; `LumaSample.Missed` and ring drops in the report, WF16-R2), the regenerated bindings | WA80, WA82, WF16 | 1.5 | Mac, CI ubuntu/windows | The panel shows the lock-prompt bar while `ShareView.lockPrompt` is set and its buttons resume or stop (a component test, and the long-lock case in `desktop-system.spec`); `desktop-share.spec` closes the window while live, the app stays live in the tray, and the close-to-tray notice follows 03 §11.6's handshake; `a11y.spec`'s `/desktop/settings` case passes in both color schemes; `avsync` with `-source mon:<id>` places its flash window on that monitor (a unit test of the placement on synthetic monitor rects) and its report counts missed readings; Go, web and e2e checks pass. As run (group 16, §3 item 101): WF16's review already passes `-monitor` to the flash role and discards an edge after a lost reading (`edgesDiscarded`, 04 §14), so the `avsync` part keeps those and adds what is missing (the placement test on synthetic rects, the missed readings' count in `avsync.json`) As built (group 16, §3 item 107): the panel's status line takes the lock-prompt state (always `role="status"`), the controller drops a stale prompt itself, the harness's `POST /__e2e/window` drives the close-to-tray step, and `avsync.json` counts `lumaMissed` and `lumaRingDrops`; `bridge.ts`, its `ShareView` fixture, two `desktop.share.panel.*` keys and `desktop-system.spec.ts` were changed outside the listed paths (accepted) |
| WB14 | 16 | Group 15 write-backs into docs/m2 | group 15's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues), WB13's review leftovers; §3 items 95–101 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: 03 §6.2 and §6.3 (`App.WindowNotice`/`WindowNoticeShown`, the `window.notice` event, 13 events); 03 §11.4 (`PillState` as built: `Positions`, `PauseUnavailable`, `Text` with `Collapsed`, `Monitor` from the share's source; `PillOpen`; the optional `PillShowing`; the disabled Pause's accessible name); 03 §11.6 (the notice's handshake; `WindowNotice` and `MicPromptCorner` mounted in the app shell); 03 §10.3 and §11.3 (Pause disabled only after `feature_disabled` in the panel); 03 §22.4 (the degrade-live alert row as tested); 03 §18.1 (rule 6 maps `update.<code>` to `desktop.update.error.<code>`); 03 §14.4 and §16.3 (WA82 as built); 04 §5.8 and §14 (the luma tap as built, the start flag kept out of the public header, WA76's paragraph in the past tense) and 04 §12.1–§12.2 (`luma.go`, `WithLumaTap`, `LumaTapOf`, `LumaSample`; WF16-R3); §3 and the WA81 row (`smoke.ps1 -Mode release`, `release:snapshot`'s stubs and env, `release:desktop-stubs`, `DESKTOP_STUBS`, the `ffmpeg-source` and `web-dist` artifacts; a snapshot needs no `jq`); WB13-R1 (03 §13.4, §22.2 and §3 item 91: the hosted runner's 0xC0000142 child dies in the same session as the same user, as `relaunch_windows_test.go` has it), WB13-R2 (04 §11's `im_protect_window` logging as `protect.cpp` does it) and R3 (prose wrapped at 120 columns); §3 gains items 95–101; "How the plan works" says how group 16 runs; the shared-files rows for group 16; this table gains WF17's and WB14's rows, and the rows of WA79–WA86 and WF16 say what was built or how they run. The owner's runs (04 §7.5, §9.3) still wait, §8 As built (group 16): R1 (WF17's missing paths, now recorded with §3 item 107) and R2 (the ragged lines of 03 §10.3 and item 91, reflowed) were fixed |
| WF18 | 17 | Group 16 follow-ups: the playback subscription, the switch link, `settings.changed`, the download link with the room | §3 items 103–104; 03 §6.3, §12.2, §12.3, §13.2, §14.2; group 16's open issues (WA83-R1 to R4, WA84-R6, WA82-R3's remainder) | `internal/desktop/app` (`followPlayback` subscribes to C2's events before `Ready`, WA83-R3; `SwitchServer` keeps a confirmed `{switch}` link's room pending so it opens after the reload, WA83-R2; the `too_many_servers` pending link is cleared once shown, WA83-R4), `internal/desktop/settings` (`Store.Subscribe`, so `settings.changed` follows every change, WA84-R6), `web/src/rooms/` (the 3 s `playback.state` repeat removed once Go subscribes early), `web/src/desktop/SetupPage.tsx` (`LinkHandled` for `too_many_servers`), `web/src/share/ShareSheet.tsx` and `ScreenAudioWarning.tsx` ("Get the desktop app" → `/download?room=<roomId>`, the Windows-only rule with `share.sheet.desktopAppWindowsOnly`, `OpenInApp` mounted above it, WA83-R1), `web/e2e/a11y.spec.ts` (the required-update banner and the desktop version screen, WA82-R3's remainder), their tests | WA83, WA84 | 1.5 | Mac, CI ubuntu/windows | A Go test shows a `playback.state` decoded right after C2's welcome reaches the app, and `desktop-handoff.spec` passes 5 times in a row without the web repeat; a confirmed `{switch}` link opens its room after the reload (a Go test and `DesktopLinks.test`); a `too_many_servers` link shows once; `settings.changed` is emitted after a change made outside `Settings.Update`; 03 §12.3's link and Windows-only rule have component tests; `a11y.spec` covers the update banner and the version screen in both color schemes; Go, web and e2e checks pass |
| WB15 | 17 | Group 16 write-backs into docs/m2 | group 16's integration and review notes (the slices' deviations, findings and fixes, the integration fixes, the open issues); §3 items 102–109 | `docs/m2/*.md` | – | 0.5 | Mac | The write-backs are in the named sections and match the code as merged on `m2/windows-app`, with no code change: §6 against 05 §18.10 on Windows 10, the exit record's folder and the `protect-check` row (WA79); 03 §12.1, §12.2, §12.3 and §13.2 as built (WA83); 03 §6.2, §6.3, §13.4, §16.1 and §16.2 and the WA84 shared-files row (`bridge.go`, `app/share.go`, the `winos` stubs); 03 §6.3, §11.6 and §22.4 and 04 §5.8 and §14's `avsync` as built (WF17); 06 §10.1 and §10.3 (the `windows-desktop` autostart step, the site filter, the nightly as WA86 built it, WA81's 7z and syft points unverified until the first nightly); 06 §13's `site.yml` as built (it serves `update_signers`; the manifest's signature is checked by `check-draft.sh` and the app, not the site, and why); §3 gains items 102–109; "How the plan works" says how group 17 runs; the shared-files rows for group 17; this table gains WF18's and WB15's rows, and the rows of group 16's slices and of WA87 and WA88 say what was built or how they run; §7 question 11 stays open |
| WA90 | 13 | M2 exit: audiotest gate and glass-to-glass latency (owner) | 05 A10, 01 C19, 04 E15 (exit run); 06 §11.8 items 5–6; §6 | `cmd/isshoni-audiotest/results/`, `cmd/isshoni-audiotest/README.md` (the results table), `docs/m2/exit-test/` (the record) | every slice of groups 1–12; an rc from WA81 | 1 + the runs | owner's PC | On `v0.2.0-rc.N`: `isshoni-audiotest run -gate` exits 0 on Win11 (24H2 or later) and Win10 22H2 from the rc's `isshoni-exit-tools` (`dll.sha256` equal to its `native-manifest.json`); R1, R3, R4 and R5 pass on both and R2 on one; R7 passes once on Win11 for each app the owner can run, the rest marked "unverified"; `enginetest avsync` gives \|median A/V offset\| ≤ 20 ms on Win11 (WGC) and Win10 (DD); the `gate` run of `g2g measure` (Share sound on, content cuts on, 01 §12.2) gives p95 < 200 ms over 60 s at 1080p60 on both (§7 question 1), with the `nocuts` and `nosound` runs recorded for diagnosis and the encoder, RTT and component numbers recorded; M-WDA, M-DD and M-WGC-WIN repeated on the rc installer; every result in the record, with no machine names, IPs or user names |
| WA91 | 13 | First release with the app, then the SignPath application (owner) | 06 R17, R18 (application); 06 §11.8, §12.3 | the release-prep PR (release notes header, README's keys table) | WA85, WA86, WA87, WA88, WA89, WA90 | 1 + the release | owner's PC; GitHub | 06 §11.8's checklist done: install paths (browser download and SmartScreen; the ps1 path without a dialog), Defender and VirusTotal, the rc.1 → rc.2 update, uninstall keeping data then purge, relink, cross-version (the rc app against `v0.1.x` shows "Ask your admin to update the server"); `v0.2.0` tagged, approved and published; the site serves the new `/download`; the SignPath Foundation application sent. Later (M3's time): 06 R18's signing jobs after approval |

## 6. M2 exit run book (outline)

WA79 writes this out as `docs/m2/exit-test/README.md`, with a results template (`results-template.md`); the owner
runs it in WA90. As built (group 16, §3 item 102), this outline follows the book; where they differ, the book's
exact commands and rules stand and this outline is fixed.

**Pass**: both halves on both Windows versions, on one release candidate.
1. Audio (05 §18.10): on Win11 (24H2 or later) `isshoni-audiotest run -gate` exits 0 with the harness and DLL of the
   rc (the report's `dll.sha256` equals the rc's `native-manifest.json`; also on Win10); R1, R3, R4 and R5 pass, and
   R2 (Win11 is the "at least one", since Win10 can't take it); R7 once for each app the owner can run (the others
   "unverified"); `enginetest avsync` gives |median A/V offset| ≤ 20 ms on both (WGC on Win11, DD on Win10); both
   reports with their manual entries are committed under `cmd/isshoni-audiotest/results/`. On Win10 22H2 the audio
   part is 05 §18.10's reduced form (§3 item 52), which replaces items 1 and 2 as a whole: `run -gate`'s exit code is
   recorded, not judged; cases 1 (`baseline-endpoint`) and 19 (`probe-fallback`) pass and the
   `exclusion-unavailable` banner shows in What friends hear; **no R row is required on Win10** (the endpoint engine
   keeps nothing out, and R3 runs on Win11 only); `avsync` and the committed report stay on both.
2. Latency (01 §12.2): the `gate` run, with Share sound on and the clock page's content cuts on, gives p95 < 200 ms
   over 60 s of a high-motion 1080p60 monitor share, server RTT ≤ 50 ms, on both. The `nocuts` and `nosound` runs are
   recorded for diagnosis only.

**Preconditions**
- `v0.2.0-rc.N` built by `release.yml` and installed through the rc server's `/download` page with
  `install-desktop.ps1`; the rc server is M1's exit-test VPS running the same rc.
- The owner's PC in each OS: a hardware encoder on a current driver, one 1080p monitor (or the primary one), a
  microphone with desktop-app access, Discord desktop signed in, Chrome, Edge, Steam with one game; nothing else
  playing; Communications set to "Do nothing".
- From the rc's `release.yml` run, the workflow artifact `isshoni-exit-tools` (06 §11.1; not a release asset):
  the tag's `isshoni-audiotest.exe`, `isshoni-enginetest.exe`, `g2g.exe` and `hwcheck.ps1`, and the rc's own
  `isshoni-media.dll`, FFmpeg DLLs and `native-manifest.json`. Check the DLL's SHA-256 against the manifest. For
  `avsync`, the `ci\` folder of `isshoni-native-tools` from the rc commit's CI run (the luma tap is a test hook).

**A. Audio** (about 15 minutes per OS; 05 §18.8): unzip `isshoni-exit-tools`; `isshoni-audiotest.exe run -gate -out
.`; on Win11 the real-app checks R1–R5 and R7 with `record -label Rn`; `isshoni-enginetest -dll ci avsync` (`-dll` is
the tool's global flag; Win11 `-source pick` on the flash window, Win10 `-source` the monitor's id as `sources`
prints it, `mon:\\.\DISPLAYn`); copy the reports to the Mac; `merge`; commit as
`cmd/isshoni-audiotest/results/<date>-<win10|win11>-<build>.json` and add the harness README's row; the two
`avsync.json` go into the record's folder (D). With a registered runner, `audiotest.yml` (suite `release`,
`release_run_id` = the rc's run) replaces the copy steps; the real-app checks and `avsync` stay by hand.

**B. Latency** (about 25 minutes per OS; 01 §12.2 steps 1–10), after A on the same rc: `g2g serve`; the clock page in
Edge on the shared monitor, **Start** (the tone plays), F11; in the app share that monitor (Win11: borderless WGC;
Win10: Desktop Duplication), preset Auto, **Share sound on**; wait for 8 Mbit/s in Details; open the room in Chrome
signed in as the same user, focus the share and turn on its speaker, at 50 % size over the bottom-right half; check
that What friends hear shows Edge heard and Chrome kept out. Then `g2g measure -d 60s -run gate -server https://<rc
server> -o g2g-<win10|win11>-gate.json`, and the two diagnostic runs `nocuts` (`?cuts=0`) and `nosound` (a new share
with Share sound unchecked): three JSON files per OS. Record the encoder and GPU, the RTT, the versions, and the
component numbers next to them: the share's `latencyMs` and `isshoni-enginetest bench` (capture-to-packet p95 ≤ 25 ms).

**C. Release-candidate checks** on the same build: 04's M-WDA, M-DD and M-WGC-WIN; `isshoni-enginetest
protect-check` on both OSes (04 §14: exit 0, DD `pass`, and WGC as it ran: a WGC `not_reachable` is recorded as "WGC
not covered on this PC", never as a WGC pass; §7 question 11); 03's M-DESK-1, 2, 3 and 8; 02's M-DA-1, 2 and 7 (M-DA-7
offline: the uninstaller finishes within 30 s and the credential is gone, while the device stays on the Devices page,
02 §16.4). 06 §11.8 items 7–11 are WA91's: they are recorded in the release-prep PR, and in the record only when run
on the same day.

**D. Record**: `docs/m2/exit-test/results/<date>.md` from the template, and next to it the folder
`docs/m2/exit-test/results/<date>/` with the six `g2g` JSON files (`g2g-<win11|win10>-<gate|nocuts|nosound>.json`)
and both `avsync` reports (`avsync-<win11|win10>.json`); the record names the paths of the two audiotest reports
(under `cmd/isshoni-audiotest/results/`) and every manual row with its result and, for one carried over, its rc. The
L-2 conditions (screen, refresh rate, RTT) are read from each gate JSON, never pre-filled. No machine names, IPs,
domains or user names; WAVs, `-no-redact` reports and the downloaded tools never go into the repository.

**E. On a failure**: a fix slice and a new rc. Every new rc repeats, on both OSes, A's `run -gate` and `avsync` (the
DLL carries the rc's version and commit, so an older report's `dll.sha256` never equals the new `native-manifest.json`)
and all of B. The real-app checks (R1–R5, R7) and the Win10 banner repeat only after a change to `native/`,
`internal/media/` or `internal/desktop/audio`, and otherwise carry over naming their rc; the C rows repeat where the
change can affect them. Every automated result in the record comes from the one rc that passed. When both pass: WA91
(release-prep PR, tag `v0.2.0`, then the SignPath application).

## 7. Questions for the owner

Each has a default that the specs implement until it is answered.
1. **Reading of the latency criterion** (01 §18): p95 < 200 ms over 60 s of a high-motion 1080p60 monitor share
   through a server with RTT ≤ 50 ms, on Win11 and Win10, taken with Share sound on and a full-screen content cut
   every 5 s (a normal share); the runs without cuts and without sound are diagnostic. The alternatives are the median
   (easier) or the maximum (fails on any IDR burst).
2. **Close button when not sharing** (03 §26): quit (default), or keep running in the tray like Discord? A setting
   covers both; only the default is the question.
3. **Locking the PC while sharing** (03 §26): pause, then resume on unlock (default), or stay paused until the user
   resumes? The bounds hold under either answer and aren't part of the question: keep-awake is released while a lock
   pause lasts, only an unlock within 5 minutes resumes by itself (a later one asks "Resume sharing?"), and a lock
   pause stops the share after 30 minutes.
4. **Handoff scope** (03 §26): stop playback in every desktop browser tab of the user in that room (default; a tab on
   another computer gets "Watch here instead"), or only in the tab that clicked "Open in the isshoni app"?
5. **Probe failure while a voice app runs** (05 §25): the plan's endpoint loopback with a banner (default, so friends
   hear the call until the sharer reacts), or start that share with sound off and ask "Share all sound anyway / Keep
   sound off" (05 recommends asking)?
6. **Self-hosted runner on your PC** (05 §25, 06 D17): the plan says the Windows audiotest runs on a self-hosted
   runner; 05 recommends running the zip by hand in M2, because a runner on a public repository is a standing risk.
   Default: WA75 builds the dispatch-only workflow, registering the runner is your call, and the gate accepts either.
7. **Community test tools** (04 §17): attach `isshoni-enginetest` and `hwcheck.ps1` as a separate zip to each GitHub
   Release so friends with other GPUs can send reports (the tools asset of 06 §2.1 is reserved), or keep them CI-only
   (default)? The exit run's `isshoni-exit-tools` is a workflow artifact either way.
8. **Escape hatch for built-in voice apps** (05 §25): none in M2 (default), so a Zoom webinar can't be shared with
   sound; an "All sound" choice can come later when users ask (§3 item 15 keeps the value reserved).
9. **Defer the fast path?** (05 §25 question 4): the single EXCLUDE stream when only one tree is kept out is the one
   fail-open mode, and it runs only when no voice app runs. Default: keep it, as the plan says; if case 5 or 26 fails
   on real hardware, the release ships with `audio.fastPath` false and the gate treats those cases as INFO.
10. **Later, after SignPath approves** (06 §20): if Smart App Control blocks the unsigned FFmpeg DLLs next to a
    signed `isshoni.exe`, ask SignPath to sign the unmodified DLLs our CI builds (changing the plan's "FFmpeg DLLs ship
    unsigned"), or accept that those PCs use the browser sharer? No answer is needed before that measurement.
11. **`protect-check`'s WGC half where WGC can't be reached** (04 §14, §3 item 88; group 14's WA70 review): WA70's
    acceptance says `protect-check` passes on Win11 and Win10 for DD and WGC. A listed `mon:` source always runs DD,
    so WGC is reached only through a monitor pick, and on Windows 10 a monitor pick maps to DD (04 §7.5) unless the
    mapping fails; on Windows 11 a WGC start that fails falls back to DD (04 §7.1). The tool then reports WGC as
    `not_reachable` and passes (exit 0) on DD alone. Default (as built): accept that, with the exit run recording a
    WGC `not_reachable` as "WGC not covered on this PC", never as a WGC pass; the WGC half on Win11 is its own
    interactive step (`hwcheck.ps1` runs `-wgc off`). The alternatives: make a Win11 `not_reachable` (a fallback)
    exit 1, or require a Win10 PC where the mapping fails (which may not exist among the test machines). Still open
    after group 16: WA79's exit book records `protect-check` under the default (§6 C, §3 item 102).

## 8. Owner actions (not decisions)

- Done, differently (§3 items 51–52): WA09 was to be the S1 bundle on Win10 22H2 and the late-start re-run on Win11,
  on the owner's PC. On 2026-11-03 the owner decided to run it on a GitHub `windows-2025` runner with VB-CABLE
  instead (`.github/workflows/s1-audio.yml`, PR #48), and Windows 10 was not tested: it takes the plan's gate (builds
  below 22000 use endpoint loopback with the banner; the Win10 audio gate is 05 §18.10's reduced form). WA45 runs in
  group 10.
- Done (§3 items 64, 69): FFmpeg revision 2 (the libvpl patch, owner's decision of 2026-11-04) was published:
  `ffmpeg.yml`'s publish run from `m2/windows-app` at `957cce6` created the prerelease `deps-ffmpeg-9.0.2-r2`, and the
  `lock.json` it printed is committed (`b437d0e`, merged before group 12's slices; `ffmpeg-inputs -check` matches,
  `ffmpeg-fetch -source` verified the release and both attestations verified). Still to see: `dll_search_test`'s
  libvpl part passing on windows-2025 with r2, in the first `windows-native` run of the merged tree.
- Done (§3 item 50): the FFmpeg publish, dispatched on 2026-11-03 at the owner's request from `main` at `7e3f7bb`;
  `deps-ffmpeg-9.0.2-r1` exists and its `lock.json` (with `msvcCompiler` and `msvcLinker`, §3 item 30) is committed,
  so WA24 ran in group 9 and WA44 runs in group 10. Still the owner's: a tag ruleset for `deps-*` (06 §14 item 3).
- Before WA81 (the release dry run): create the update signing keys on the Mac, merge `deploy/keys/update_signers`
  and the fingerprints, put `RELEASE_UPDATE_SIGNING_KEY` into the `release` environment, keep the backup offline
  (06 §14 item 1); set Actions to "Require approval for all external contributors" (item 4).
- Before the first `audiotest.yml` dispatch (WA75 merged in group 13, §3 item 83): create the environment `audiotest`
  (Settings → Environments → New environment) with yourself as its required reviewer and "Prevent self-review" **off**
  (you dispatch the run and approve it yourself), as `deploy/windows/runner/README.md` describes (06 §14 item 5).
  GitHub creates an unprotected environment when a workflow names one that doesn't exist, so the workflow's
  `environment` job fails every dispatch until it exists with that reviewer. The first dispatch also shows whether
  that job reads the environment with the default token or takes its no-token fallback.
- If question 6 is answered yes: register the runner on both OS installs as 06 §10.5 and WA75's README describe.
- The "Manual:" items of the slices that list the owner's PC, with the `hwcheck` bundle where one applies (WA33, WA44,
  WA50, WA51, WA60, WA61, WA70, WA76), WA62a's audio cases right after WA53's group merges (WA45's and WA53's gate, §5
  "As run"; Win11, then Win10, where only the endpoint cases apply, §3 item 52), and the matrices of WA62b, WA68, WA72
  (M-APP-PICK), WA77, WA80, WA83, WA87 and WA89. WA44's (the full probe < 2 s, `capture` per encoder at 1080p60,
  `-bitrate-steps`, `bench`, 04 §9.3's settings and layer 0's VBV under "Results for the spec") and the Windows AU
  fixtures (`isshoni-enginetest capture -encoder <token> -dump-aus 12` for `nvenc`, `qsv`, `amf`, `mf-hw`, `mf-sw`,
  committed under `native/common/tests/testdata/aus/`) and WA45's `capture -audio mix` run waited for WF10, which merged
  in group 11; they can run now that the FFmpeg r2 lock is committed (the item above; §3 items 55, 60, 64, 69). Of
  WA50's, M-PICK (`isshoni-enginetest pick -count 3`, Win11 and Win10 22H2) can run too, and so can M-WGC-WIN, M-WGC-MON
  and M-SECONDARY, since WF12 merged in group 12 (§3 items 61, 67). WA51's M-OPENH264 and WA69's `TestCaptureToSend` are
  new; WA69's needed WA60's `mon:` ids (§3 item 73), which group 13's integration made startable, so it can run now,
  and so can WA60's M-DD (with the `DisplayName()` record of 04 §7.5: put the values under "Results for the spec") and
  M-WIN10-WINDOW on Windows 10 22H2, WA61's M-NVENC-LIMIT and WA72's M-APP-PICK (§3 items 78, 79, 81). M-OPENH264
  checks `f`'s bitrate against its target's lower bound with real moving content, which `openh264_test` can't on the
  test pattern (§3 item 76). On the owner's PC, MF software also shows whether a client Windows
  gives its first AU before a drain (§3 item 68). WA59's `--self-check` against the `windows-native` artifact runs on
  the owner's PC or in WA67's smoke. WA62b's first hardware run (`isshoni-audiotest run -gate -out .`, `record -label R1
  -d 30 -o r1.wav`, then `merge` on the Mac, 05 §18.8) is also the first real run of its endpoint reads
  (`mmdevice_windows.go`), of cases 13, 14 and 22 and of the soak, so a first failure there may be the harness's and is
  judged before a `WF` slice is opened, as for WA62a's cases 20, 21 and 27.
- Still open after WA49 merged (WA49-R4): on a clean Windows 10 22H2 (a VM or a fresh user profile), record
  `[Net.ServicePointManager]::SecurityProtocol` and the `SchUseStrongCrypto` and `SystemDefaultTlsVersions` values
  (the 64-bit and the `WOW6432Node` key). If TLS 1.2 is off by default, a `WF` slice switches line 1 of
  `deploy/install-desktop.ps1`, the Pester test 'starts with the usage line and the docs link' and
  `PS1_NEEDS_TLS12_PREFIX` in `web/src/download/links.ts` (WA74; `links.node.test.ts` checks that it agrees with the
  script's line 1) together to 06 §7.5's prefixed form. WA66 and WA74 merged with the bare form; this still applies
  before the first release with the app. Windows 10 being untested for audio (§3 item 52) doesn't change it: the
  script runs on Windows 10 too.
- Still open after groups 8 and 9: from a passing run of the next group PR's `e2e` job, download the `e2e-results`
  artifact (kept 7 days) and read
  `latency.spec`'s `latency.json`: its `avOffset` median is CI's A/V offset with correct stamps (01 §12.4 item 2,
  §3 item 49). Near the Mac's −13 to −22 ms the 60 ms bound stands; near −40 ms a `WF` slice tightens the check.
- After group 15 (§3 items 95, 98, 99): WA81's release dry run (Actions → Release with `dry_run`, ephemeral keys; it
  writes permanent public Rekor entries, so only the owner dispatches it), then an `audiotest.yml` dispatch with its
  `release_run_id`; WA80's M-DESK-2, M-DESK-3, M-DESK-8 (Windows 10) and M-WDA with the pill, which now goes on the
  shared monitor; and, since WF16 merged, the first `isshoni-enginetest avsync -dll ci` run on Windows 11 (M-AVSYNC's
  first record, 04 §18.4), which `-no-luma` no longer stands in for.
- After group 16 (§3 items 102–108): WA83's M-DESK-7 and M-DESK-9 and WA84's M-DESK-10 on the owner's PC; a first
  scheduled or dispatched nightly (WA86: `windows-full`, `goreleaser-full` with real desktop files, which settles
  WA81's 7z and syft points, and `compat-servers`); the screenshots of `/download#windows-smartscreen` with the first
  release (WA91's checklist, 06 §12.2); and, when `deploy/keys/update_signers` is added, the root README's key rows
  with their fingerprints in the same PR (06 §14 item 1).
- `v0.1.0` (M1's exit test, README S94-day) before WA91's release-prep.
- WA90 (the exit run) and WA91 (the release, then the SignPath application). Nothing needs to be bought or rented: the
  rc server is M1's test VPS.

## 9. Review log

The design review kept 55 findings and refuted 1 (not listed). None was declined: what the doc editors returned as
"declined" were the cross-doc follow-ups of findings they had applied, and this pass made each of them in the doc it
names or in the slice plan. Severity is listed where it reached this editor (the four README findings); the doc
editors' results came without it.

| Id | Severity | Outcome |
|---|---|---|
| ux-5 | major | Applied: §3 item 15 (no sound mode; Go and the DLL both refuse "everything"), WA01's superseded spellings (`Audio.SetMode`, `Service.SetMode`), WA77's acceptance (the locked built-in list, no sound-mode control); 01's `SetPaused` comment names `SetShareSound`. 03 had already removed the binding |
| scope-2 | major | Applied with one mechanism: `release.yml`'s `desktop-pkg` uploads the workflow artifact `isshoni-exit-tools` (the tag's tools and the rc's own DLLs, checked against `native-manifest.json`; not a release asset, so §7 question 7 stands); `audiotest.yml` gets `release_run_id` (06 §5.1, §10.4, §11.1, §11.8, §17, R13, R15; 05 §18.1, §18.8, §21, §22; §6, WA75, WA79, WA81, WA90). One change to the fix: a GitHub-hosted `fetch` job, not `run`, downloads it, so the self-hosted runner never uses a token (WA75: no token use in `run`). 05 §18.1's DLL name fixed; the latency tool is `g2g.exe` (§3 item 7) |
| scope-4 | major | Applied: WA62 split into WA62a (group 6, Needs WA26, WA35, WA37; the integrator's hardware run right after group 6 is WA45's and WA53's gate) and WA62b (group 7); hardware failures go to `WF` slices that list `native/windows/src/audio/`. WA62a takes every case without a microphone, adding 8, 11 and 24–27 to the proposed list (their code exists by group 6). §3 item 13, "Re-cut", WA35, WA53, WA72, WA75, WA79 and §8 updated; 92 slices, about 172 days |
| scope-7 | minor | Applied the cheaper option: results go into the PR body ("Results for the spec", "Wails checklist"); WA87 copies 03 §3.2, and `WB1` and `WB5` copy 06 §4.6 and 04 §9.3 (shared files row; 03 §3.2, 04 §9.3 and 06 §4.6 say so). Since WB1, `WB2` copies 06 §4.6: WB1 could not read WA07's run on GitHub |
| security-1 | – | Applied in 03; follow-ups made in 02 §11.7, §11.8, §16.3, §17, §18 (`AccessToken(origin, force)`, `SessionEnded`/`HelloRefused` with the origin, reload on every server change) and in WA32 and WA71 (the second-server case) |
| security-6 | – | Applied in 03; follow-ups made in §3 item 15, 01 §5.1, §7.1, §11.2, 04 §5.2, §5.3, §10.2, §12.2, §18.2, §18.3 and 05 §4.1, §6.1, §10.5, §13.1, §19.1. The DLL status is `IM_ERR_INVALID_ARG` (the header has no `IM_ERR_INVALID`) |
| security-7 | – | Applied in 03 (W21, the guard's allowlist); follow-ups made in §4 Naming (W1–W21), WA22, WA48 and WA87 |
| security-11 | – | Applied in 03; follow-ups made in §3 item 18, §4 Logging, 04 §5.6 and E14 (info and debug level); 05 already ran its log test at debug level |
| security-12 | – | Applied in 03; no cross-doc change |
| media-audio-13 | – | Applied in 03; follow-ups made in 05 §14.2, §14.6, §19.1 (`DismissPrompt`, `Decide` limited to keep-out/include) and WA37, WA77 |
| media-audio-14 | – | Applied in 03; no cross-doc change |
| ux-1 | – | Applied in 03; follow-ups made in 06 §6.3 (the Finish line), §7.5 (three numbered steps, `?room=`), §11.8 item 7(a), R12, and WA67, WA74 |
| ux-3 | – | Applied in 03; follow-up made in §7 question 3 (the bounds hold under either answer) |
| ux-6 | – | Applied in 03; §3 item 17 decides for 01's mapping (`dependency` and `no_encoder` stay apart) and 04's `invalid`/`self_window` error (no `Source.Self`); 01 §5.1–§5.2, 03 §10.1, §10.5, §22.3 and 04 §12.2, §18.3 aligned |
| ux-9 | – | Applied in 03; no cross-doc change |
| ux-10 | – | Applied in 03; no cross-doc change |
| ux-12 | – | Applied in 03; no cross-doc change |
| ux-13 | – | Applied in 03; no cross-doc change |
| scope-6 | – | Applied in 03; follow-ups made in 06 §3, §18 (the version comes from `go.mod`, no `tools/wails3/runtime-version`) and the shared files row, WA04, WA22 |
| security-2 | – | Applied in 02; follow-ups made in WA39 (the spent grace child) and WA56 (cases 1–3, 5 and 9–12) |
| security-3 | – | Applied in 02; follow-up made in WA63 |
| security-4 | – | Applied in 02; follow-ups made in WA47 and WA57 |
| security-5 | – | Applied in 02; follow-ups made in 03 §6.1 (`OpenURL` validates itself), §8.2 (canonical `LinkState` URIs, `ErrBadResponse`), §22.1, and WA31 |
| security-13 | – | Applied in 02; follow-up made in WA30 (four buckets, the 5,000 cap) |
| ux-11 | – | Applied in 02; follow-up made in 03 §8.1 and §9 ("Opens in your browser but not here? See why") |
| security-8 | – | Applied in 04; follow-ups made in 03 §13.4 (main's first action) and §22.2 (marker test), 06 §4.1 (libvpl) and §5.1 (conditional load-config check), and WA11, WA41, WA44 |
| security-9 | – | Applied in 04; follow-up made in WA51 |
| media-audio-5 | – | Applied in 04; follow-ups made in §3 item 16 and 03 §10.5 (the engine's fallback, no core-level count) |
| media-audio-8 | – | Applied in 04; no cross-doc change |
| media-audio-19 | – | Applied in 04; follow-up made in WA44 |
| scope-8 | – | Applied in 04; follow-up made in WA70 (M-WDA on `winos`'s affinity, Needs WA68) |
| scope-9 | – | Applied in 04; follow-up made in WA72 (M-APP-PICK; Needs WA65, WA68) |
| scope-10 | – | Applied in 04; follow-ups made in WA60 (§7.5 steps 1–3 and 5), WA61 (renamed, device loss ends the session) and 03 §10.5 (`device_removed` with "Share again") |
| security-10 | – | Applied in 06; follow-ups made in WA25 and WA81 |
| ux-2 | – | Applied in 06; follow-ups made in 03 §13.2, §13.4 (step 2a, step 5's handover), §22.1, DK12, and WA40, WA59 (step 2a), WA67, WA88 |
| ux-7 | – | Applied in 06; follow-ups made in WA49, WA74 and WA81 |
| ux-8 | – | Applied in 06; follow-ups made in WA49 (the clean Win10 measurement) and WA74 (Needs WA49, exact string) |
| scope-1 | – | Applied in 06; follow-ups made in 04 §13.1, the pins and shared files rows, WA07 and WA11 |
| media-audio-1 | – | Applied in 05; follow-ups made in WA62a (analyzer tests, cases 24–27, gate configuration) and WA36 |
| media-audio-2 | – | Applied in 05; follow-ups made in 04 §10.3 (L ≥ 0), §14 and §18.4 (`avsync`, M-AVSYNC), E15, 06 §5.1 (`ci\` in `isshoni-native-tools`), and WA76 (+0.5 day), WA79, WA90, §6 |
| media-audio-6 | – | Applied in 05; no cross-doc change |
| media-audio-7 | – | Applied in 05; its tests are in WA36's acceptance |
| media-audio-9 | – | Applied in 05; its tests are in WA36's acceptance |
| media-audio-10 | – | Applied in 05; its tests are in WA36's acceptance |
| media-audio-12 | – | Applied in 05; follow-ups made in 01 §6.4, §7.1, §16 and 04 §12.1, §12.2, §14, §19 (`media.AudioTestTone` is the one carrier) |
| media-audio-15 | – | Applied in 05; no cross-doc change |
| media-audio-16 | – | Applied in 05; no cross-doc change |
| media-audio-17 | – | Applied in 05; follow-ups made in 06 §11.8 (R7 at minor releases) and WA90 |
| media-audio-18 | – | Applied in 05; no cross-doc change |
| ux-4 | – | Applied in 05; follow-ups made in 03 §6.3, §10.2, §11.1, §11.4, §11.6, §15, §18.1, §22.4, §24, and WA80 (tray, pill, toast, window notice, DND line, the `degrade-live` e2e) |
| scope-3 | – | Applied in 05, with 50 ms short windows instead of 20 ms (a 20 ms window's main lobe would put 907 Hz inside 1000 Hz's); follow-up made in §7 question 9 |
| media-audio-3 | – | Applied in 01; follow-ups made in §6 B, WA90 (the `gate` run gates) and 05 §9.3 (the fill target may drop only if the gate misses) |
| media-audio-4 | – | Applied in 01; follow-ups made in WA28 and WA54 (D19) |
| media-audio-11 | – | Applied in 01; follow-ups made in 04 §12.2 and 03 §6.3 (`Probe.Audio` from `AudioCapsFromFlags`) |
| media-audio-20 | – | Applied in 01; follow-ups made in 04 §9.3 and E7 (layer 0's VBV), §6, §7 question 1, WA08 and WA44 |
