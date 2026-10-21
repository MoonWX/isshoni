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
asks the owner to confirm how "under 200 ms" is read.

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
  Win10 22H2 against the release candidate's own DLL.
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
  block's semantics, `audio.go`, `audio_windows.go`, the audio folders and the audiotest. 04 owns the Opus stage and
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
    PR.
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
  names of 06 §7.3 join M1's reserved list (WA66).
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
- If M2 runs late, these can move after `v0.2.0` without touching the exit criterion: WA51 (OpenH264; MF software stays
  the fallback), WA69 (the hardware capture-to-send check), WA86 (nightly rows). A moved slice leaves every Needs list.

### Shared files and their owners

| File | Owner | Rule for every other slice |
|---|---|---|
| `go.mod`, `go.sum` | M1's rule | WA22 adds Wails `v3.0.0-beta.34` (it raises no server requirement, 03 §3.1); no other new module is planned. On a conflict the integrator takes the base files, re-runs each slice's `go get`, then `go mod tidy` |
| `tools/go.mod` | integrator | WA08 adds `golang.org/x/sys`. `tools/wails3/go.mod` is a module of its own (WA04) |
| `Taskfile.yml` | WA04 | WA04 writes every M2 task up front (06 §17's list, `desktop:dev`, `native:*`, `ffmpeg:*`), with real bodies for `native:test`, `ffmpeg:build`, `release:wails3`, `lint:go` and `lint:pins` and stubs that exit 0 with a note. Later slices change only the bodies of tasks they own: `native:build` (WA11), `desktop:dev` and `desktop:exe:windows` (WA22; WA67 extends the latter), `ffmpeg:fetch` and `native:dist` (WA24; WA76 adds `hwcheck.ps1` and `g2g.exe`), `release:update-keys` (WA25), `lint:ps`, `test:ps`, `release:prepare` (WA49), `desktop:package:windows` (WA67), `release:assets` (WA81) |
| `.golangci.yml` | WA02 | WA02 writes every M2 depguard rule (01 §4, 03 §4.5, 05's rows); then integrator only |
| `.github/workflows/ci.yml` | WA04 | only the slices that list it (WA11, WA24, WA49, WA67, WA72, WA78, WA81, WA88), never two in one group |
| `.github/workflows/{ffmpeg,audiotest,release,nightly}.yml` | WA07, WA75, WA81, WA86 | `nightly.yml`: WA88 adds the updater e2e |
| `.github/dependabot.yml` | WA04 | integrator only |
| `tygo.yaml` | integrator | WA18 adds the `relay` entry, WA48 the `bridge/codes` entry |
| `internal/protocol`, `internal/protocol/api` | M1's rule | additive changes only (WA05, WA18), called out in the PR; the integrator re-runs `task gen` |
| `web/src/protocol/*.gen.ts`, `web/src/platform/desktop/codes.gen.ts`, `web/src/platform/desktop/bindings/` | generated | never edited or merged by hand: after each merge run `task gen` and `wails3 generate bindings` and commit the output |
| `web/package.json`, `web/package-lock.json` | M1's rule | WA22 adds `@wailsio/runtime` at the Go module version of Wails without the `v` (`3.0.0-beta.34`), exact, which `lint:pins` checks against `go.mod` (03 §3.1); other packages go through the integrator |
| `web/src/app/router.tsx` | WA42 | WA42 declares every M2 route (the desktop pages and `/link`), each lazily loading its folder; page slices fill only their folders |
| `web/src/i18n/en.json`, `web/src/i18n/lazy/*.en.json` | M1's rule | keys under the slice's own namespace: `desktop` in `lazy/desktop.en.json` (created by WA41; WA42's stub pages add no keys), `link` in `lazy/link.en.json` (WA63), `account.download.*` (WA74), two `errors.*` keys (WA63); conflicts keep both sides; `check:i18n` must pass |
| `web/e2e/` | file-level | each spec file belongs to one slice; `fixtures.ts` gains `DESKTOP_E2E_BIN` in WA48; `desktop-share.spec.ts` is WA65's, extended by WA80 |
| `internal/server/wire.go` | M1's rule | only WA56 and WA66 |
| `internal/server/config/keys.go` | integrator | WA66 adds only 06 §7.3's names to the reserved env list |
| `internal/server/itest` | file-level | each slice adds its own test files; `desktop_helpers_test.go` is WA29's (WA73 switches it to bearer), `device_helpers_test.go` is WA56's (WA64 adds `linkedManager`) |
| `native/include/isshoni_media.h` | WA03 | WA03 writes all of v1.0; later changes only through the integrator with 04's and 05's owners (none planned in M2) |
| `native/CMakeLists.txt`, `CMakePresets.json`, `vcpkg.json`, `vcpkg-configuration.json`, `.clang-format` | WA03 | integrator only; the vcpkg baseline is bumped by hand |
| `native/common/CMakeLists.txt` | WA03 | adds `audio/`, `audiopolicy/`, `audiomix/` when present and globs each library's `tests/`; slices add files only |
| `native/windows/CMakeLists.txt`, `native/windows/src/api/` | WA11 | the CMake file globs each `src/` folder and `tests/`; every export forwards to an area function whose stub lives in that area's folder, and a slice replaces its own area's stub |
| `native/windows/tests/` | file-level | each test file belongs to one slice |
| `cmd/isshoni-enginetest` | WA44 | WA44 writes the tool (flags, dispatch, `probe`, `capture`, `bench`); each later subcommand is one file registered in `init()` and owned by its slice (`pick.go` WA50, `openh264.go` WA51, `sources.go` WA60, `sessions.go` WA61, `protect.go` WA70, `soak.go` and `avsync.go` WA76) |
| `deploy/notices/native-windows.json` | WA03 | created in 06 §9.2's format; append-only, one entry per native dependency with the slice that adds it; WA78 adds the gate |
| `deploy/windows/dll-imports.txt` | WA11 | append-only: any new system DLL import in the same PR |
| `deploy/windows/tools.json`, `deploy/windows/msvc-env.ps1` | WA07 (the `msvc` entry and the script) | WA11 adds the other entries (NSIS, WebView2, Pester, PSScriptAnalyzer) and reuses the script; later changes are hand-made tool PRs (06 §3) |
| `docs/m2/*.md` | WA01 | then only write-back slices, WA60 (04 §7.5) and WA87 (03 §3.2, §20). A slice that measures something a spec records puts the result in its PR body under the heading "Results for the spec": WA07 (06 §4.6), WA22, WA48 and WA68 (their 03 §3.2 rows, under "Wails checklist"), WA44 (04 §9.3, layer 0's VBV included). WA87 copies the 03 §3.2 rows; the integrator's `WB2` and `WB5` copy 06 §4.6 and 04 §9.3 (`WB1` could not: WA07's first PR run summary is on GitHub) |
| `docs/*.md` (site pages) | 06 | WA57 (device rows, `#desktop-linking`), then WA85 |

**Dependencies and pins** (each must pass the license gate; the first slice to need one adds it at the pin below)

| Dependency | Pin | First needed by |
|---|---|---|
| `github.com/wailsapp/wails/v3` (and the `wails3` CLI, `@wailsio/runtime`) | `v3.0.0-beta.34`, exact; they move together, by hand, at most once per minor release (03 §3.3) | WA04 (CLI), WA22 |
| `golang.org/x/sys` (main module, already v0.49.0) | the `windows` package for the binding, Credential Manager, `winos`, the updater | WA10 |
| `golang.org/x/sys` in `tools/go.mod` | `g2g`'s GDI capture | WA08 |
| `golang.org/x/crypto/ssh`, `golang.org/x/net/idna` (already required) | the SSHSIG verifier; deep-link hosts | WA25; WA20 |
| vcpkg `opus`, `speexdsp` (BSD-3-Clause, static), `gtest` (feature `tests`, never shipped) | `builtin-baseline` in `native/vcpkg.json` | WA03 |
| FFmpeg 9.0.2 with nv-codec-headers 12.1, AMF headers, libvpl | `native/ffmpeg/pins.json`; published once as `deps-ffmpeg-9.0.2-r1`, assets pinned in `lock.json` | WA07 |
| Cisco OpenH264 | the newest Cisco release at WA51 (2.6.0 today), URL and SHA-256 compiled in; downloaded at run time, never shipped | WA51 |
| VS 2022 17.14 or VS 2026 (vswhere `[17.0,19.0)`, `VC.Tools.x86.x64`), MSVC toolset 14.44 exact through `deploy/windows/msvc-env.ps1`, Windows SDK 10.0.26100, NSIS 3.13, the WebView2 bootstrapper, Pester 5.x, PSScriptAnalyzer 1.x | `deploy/windows/tools.json` | WA07 (MSVC), WA11, WA49, WA67 |
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
| WA07 | 1 | FFmpeg LGPL build | 06 R2; 06 §3, §4 | `native/ffmpeg/` (`pins.json`, `build.sh`, `ffcheck.c`, `README-LGPL.txt`, `lock.json`), `.github/workflows/ffmpeg.yml`, `tools/desktoppkg` (new: the dispatch, `ffmpeg-inputs`, `ffmpeg-fetch`), `deploy/windows/msvc-env.ps1`, `deploy/windows/tools.json` (the `msvc` entry) | – | 2 | CI windows; owner dispatch | The PR build is green on windows-2025 (VS 2026 image) with toolset 14.44, and the log and summary name the VS instance, the toolset build and the SDK; with `toolset` set to one the image lacks, `msvc-env.ps1` fails naming the toolsets it found; the zip holds both DLLs, import libraries, headers, PDBs and licenses; `ffcheck` reports LGPL 2.1 or later, exactly `h264_amf`, `h264_mf`, `h264_nvenc`, `h264_qsv`, and no gpl, nonfree or version3; the DLLs depend on system DLLs only; a second build is byte-identical, or the PR records why not under "Results for the spec" and that check becomes a warning (06 §4.6; `WB1` writes it back). Owner: Actions → FFmpeg → Run workflow with `publish` creates `deps-ffmpeg-9.0.2-r1` as a non-latest prerelease with attestations, and the printed `lock.json` is committed before WA24 |
| WA08 | 1 | Glass-to-glass tool | 01 C17; 01 §12.1, §12.2 | `tools/g2g/`, `tools/go.mod` | – | 2 | Mac, CI ubuntu; owner's PC | Decoder tests on recorded PNG fixtures (a transition frame, a 50 % scaled copy, nested copies, and each content-cut image) pass on the Mac and in CI; the clock page makes a full-screen content cut every 5 s (`?cuts=0` turns them off) and plays its tone after the Start button; `g2g measure -run gate\|nosound\|nocuts` labels the JSON; `GOOS=windows go -C tools build ./g2g`; `g2g serve` serves the clock page; exit codes 0, 1, 2 as 01 §12.2. Manual: a dry run on the owner's PC decodes ≥ 90 % of the screenshots |
| WA09 | 1 | Windows 10 audio gate and the onset-fade re-run (owner) | 05 A0; 05 §3, §18.10 | `spikes/s1-win-audio/README.md` (results only) | – | 0.5 + the runs | owner's PC | Owner: `s1.exe selftest` on Win10 22H2 and `s1.exe selftest -only late-start,late-start-endpoint` on Win11, results recorded. Pass: the include-set scenarios pass on Win10 and late-start has no click on Win11. If Win10 fails, the plan's gate applies before WA45 merges: Win10 builds below 22000 use endpoint loopback with the banner, the Win10 audio gate takes 05 §18.10's reduced form, and the owner is told |
| WA10 | 2 | Go binding skeleton | 04 E1 (Go half), E2 (Go loader); 04 §12.1–§12.4; 01 §7 | `internal/media/native` (`abi.go`, `types.go`, `errors.go`, the `caller` interface and `fakeCaller`, `layout_test.go`, `engine_other.go`, `engine_windows.go`, `call_windows.go`, `clock_windows.go`; the whole 04 §12.2 API declared) | WA02, WA03 | 2 | Mac, CI ubuntu; CI windows | `layout_test` agrees with `abi_layout.json`; through `fakeCaller` on the Mac and ubuntu: every status and token → `*media.Error` with code, `Reason` and `Op`, the snprintf retry, the clock cross-check (passes; a 2 ms offset fails `Open`); `Open` returns `ErrUnsupportedOS` off Windows; `GOOS=windows` vet and lint clean; with WA11's DLL (integrator, after the group): `Open` loads it by full path with the DLL-folder and System32 search flags, the ABI check passes, a missing export is `internal`/`abi` naming it, `BuildInfo` decodes |
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
| WA23 | 3 | Engine session core on fakes | 04 E5; 04 §6.3, §6.4, §12.3, §18.2 | `native/windows/src/{session,engine,testhooks}/`, `native/windows/tests/` (`session_fake_test.cpp`), `internal/media/native` (`bridge.go`, `session_windows.go`) | WA10, WA11, WA12, WA13, WA15, WA16 | 3 | CI windows; Mac, CI ubuntu | `session_fake_test` (04 §18.2: each layer's first packet is `KEYFRAME\|REINIT` within 500 ms, also on a static pattern; a keyframe within one interval; `im_set_layer_active`; pause; profile switch; injected failures; ring overflow; `im_stop` ≤ 2 s with a blocked consumer; the fifth session is `IM_ERR_LIMIT`; the pick-token rules); `bridge_test` through `fakeCaller` on the Mac and ubuntu (04 §18.3); with the `win-x64-ci` DLL under goleak: `mediatest.Conformance` on `test:pattern` with `IM_ENC_FAKE` (checks 5 and 7 included) and `TestNativeIntoPublisher` (frames on both layers, a PLI answered within one frame, flash/beep offset < 45 ms) |
| WA24 | 3 | FFmpeg in `windows-native` | 06 R3b; 06 §4.1, §5.1 | `.github/workflows/ci.yml` (`windows-native`: the locked zip as `IM_FFMPEG_ROOT`, the lock staleness and DLL hash checks, the `isshoni-native-tools` artifact), `Taskfile.yml` (`ffmpeg:fetch`, `native:dist`) | WA07 (with `lock.json`), WA11 | 0.5 | CI windows; owner's PC | A `pins.json` change without a new `lock.json` fails the job; the `ci\` folder carries the FFmpeg DLLs next to `isshoni-media.dll`; `task native:dist` builds the zip; `isshoni-native-tools` also carries `ci\` with the `win-x64-ci` DLL and the FFmpeg DLLs (for `enginetest avsync`, 06 §5.1). Manual: the zip unpacks on the owner's PC (the `enginetest probe -quick` check is WA44's) |
| WA25 | 3 | Update manifest and signature | 06 R7; 06 §8.2, §8.3, §8.12, §11.5 | `internal/update` (`manifest.go`, `sshsig.go`, `signers.go` with the `Backup` flag), `tools/desktoppkg` (`manifest`), `tools/repocheck` (`keys`: the signers block, README rows, the OpenSSH 8.1 option rule), `deploy/desktop/min-version`, `deploy/keys/update_revoked`, `Taskfile.yml` (`release:update-keys`) | WA05 | 1.5 | Mac, CI ubuntu/macos/windows | 06 §16's sshsig and manifest tests: committed vectors verify; a flipped byte, the wrong namespace, `sha256`, an untrusted or revoked key, trailing bytes, bad armor and oversize input fail; signatures made by `ssh-keygen` verify when it is on the PATH; versions compare through `internal/version`; `FuzzSSHSIG` 30–60 s; `lint:keys` fails on a drifted signers block and on an `update_signers` with zero or two `isshoni-update-backup-<n>` lines; an active-key manifest revoking the backup is ignored and a backup-key manifest revoking the active key is persisted |
| WA26 | 3 | Go audio types and the fake's audio scenarios | 05 A2; 05 §12, §13.1, §13.3 | `internal/media` (`audio.go`: `ParseAudioReport`, the status → error table), `internal/media/testdata/audio/`, `internal/media/fake` (`audio_ctl.go`, `EngineConfig.AudioScenario`, the `Call` fields) | WA02, WA15 | 1.5 | Mac, CI | The golden reports parse; every scenario of 05 §13.3 behaves as scripted under synctest; the fake's `AudioController` works before `Start`; builds for windows, darwin and linux and the gomobile package builds pass |
| WA27 | 3 | Hub: `share.pause` and `user.connections` | 01 C08; 01 §10.3 (hub), §10.4 | `internal/server/signal` | WA05, WA18 | 1.5 | Mac, CI | 01 §14's signal row: `paused` ignored without the feature; status precedence stalled > paused > live; pausing a starting share; any connection of the same user may pause; `user.connections` on welcome, open, close, detach, room change and after a `Revalidate` that changed `DeviceName`, at most one per 200 ms, only to the same user's connections with the feature |
| WA28 | 3 | SFU: native uplink estimator and sharer pause | 01 C09; 01 §10.1, §10.3 (SFU) | `internal/server/sfu` (with `LayerUplinkStats.QueueDelay`), `internal/server/sfu/sfutest` (`FaultConn.SetInboundLink`, `NewQueueLink`) | WA18 | 2 | Mac, CI | `nativeUplink` table tests per §10.1 row; `RawLossPct` counts gaps before repair; hints ≥ 15 % and ≥ 2 s apart; the `web` and `tool` kinds keep `m1Uplink`; `sharerPaused` swallows keyframe requests and asks one per watched layer on resume; a Go publisher behind a 3 Mbit/s inbound link gets a congestion hint within 6 s; `LayerUplinkStats.QueueDelay` is sampled in the Layer's RTP loop at each frame's first packet (500 ms window minimum, baseline rising at most 0.5 ms/s) and feeds `nativeUplink`'s delay rows; `SetInboundLink` accepts `sfutest.NewQueueLink` (`SetLink` stays policer-only), and a publisher behind `NewQueueLink(3 Mbit/s, 1 s)` gets a congestion hint before any loss at the queue; M1's integrations 1–19 stay green |
| WA29 | 3 | Client core: connection, share start and stop | 01 C10; 01 §11.1–§11.5, §11.10 | `internal/client/core` (the whole §11.2 API), `internal/server/itest` (`desktop_helpers_test.go`, `desktop_share_test.go`) | WA15, WA16, WA17 | 3 | Mac, CI | itest D1 (live ≤ 3 s, both layers and audio, continuity, A/V ≤ 10 ms), D2 (one Participant, in no room outside a share), D14 (source gone), D15 (`StopOther`), D17 (goleak); 01 §14's core unit rows (the stop table, `Run` once, `Close` ≤ 3 s, `ErrNotReady`, `ConnectionID` and `EventStatus`); cookie auth through `Options.Header` until WA73 |
| WA30 | 3 | Device flow and password link in auth | 02 DA3; 02 §5, §14 | `internal/server/auth` | WA19 | 2.5 | Mac, CI ubuntu | 02 §16.1's device-flow row with a fake clock: the poll state machine (pending; `slow_down` at 3.9 s but not at 4.0 s; the 60 s cap; denied, expired, consumed; the approver disabled); idempotent and parallel decides; the 20-device cap with eviction; the four buckets (`device-code-ip48` per IPv6 /48 included) with their order and refunds, and the 5,000 pending-code cap; `PasswordDeviceLogin` with login's order and buckets (a counting hasher); the audit rows `device.approved`, `device.denied`, `device.linked` with `via` |
| WA31 | 3 | `deviceauth`: linking, refresh, servers | 02 DA10; 02 §11.2–§11.4, §11.6, §11.7 | `internal/client/deviceauth` | WA20, WA21 | 3 | Mac, CI ubuntu, CI windows | Against an httptest server with a memory store under synctest: the poll outcomes; the credential written before `linked` and before a token is returned; `save_failed`; one refresh for 100 concurrent callers; retries keeping the `exchangeId`; the `invalid_grant` notices; `PasswordLink` and `LinkWithPassword`; offline `Unlink`; the `servers.json` round trip, the cap of 20 and corrupt-file recovery; concurrent `ForgetAll`; no redirect followed; `StartLink` refuses foreign-scheme, other-host, extra-query and mismatched-code verification URIs with `ErrBadResponse`; goleak |
| WA32 | 3 | SPA platform: `DesktopPlatform` | 03 DK4 (platform part); 03 §7.1–§7.4, §7.6; 02 §11.8 | `web/src/platform/` (`types.ts`, `detect.ts`, `desktop/`: `DesktopPlatform`, the `NativeBridge` interface, `HttpBridge`, `tokens.ts`, prefixed storage, link interception), `web/src/protocol/signal-client.ts` and its test (the `auth` option) | WA22 | 2.5 | Mac, CI ubuntu | 03 §22.3's platform and signal tests, including 02 §11.8's rules (every token call is `accessToken(origin, force)` with the page's own `serverOrigin`, and an `auth.state` for another origin is ignored; no `Authorization` header without a token, one retry with a forced token after `invalid_token`, the `HelloAuthProvider` before every `hello`, no stale-build reload); `DesktopPlatform` ≤ 4 KB gzip in the entry chunk (`check:size`); the browser e2e suite green without changes |
| WA33 | 4 | GPU path | 04 E6; 04 §8 | `native/windows/src/gpu/`, `native/windows/shaders/`, `native/windows/tests/` (`gpu_warp_test.cpp`), `deploy/windows/dll-imports.txt` (append) | WA23 | 2.5 | CI windows (WARP); owner's PC | `gpu_warp_test`: the shader against a CPU BT.709 limited-range reference within 1 code value on flat areas and 2 on edges; the VideoProcessor within 2 of the shader when WARP offers one; letterbox, rotation 90/180/270, cursor shapes against golden images, `IM_START_HIDE_CURSOR`; the GPU test pattern renders. Manual: on the owner's PC the VideoProcessor matches the shader within 2 on each available GPU vendor |
| WA34 | 4 | Engine audio bridge | 04 E13; 04 §10.1, §10.2 | `native/windows/src/audio_bridge/`, `native/windows/tests/` (`audio_bridge_test.cpp` with a fake `AudioEngine`) | WA13, WA23 | 1.5 | CI windows | `audio_bridge_test` (04 §18.2): `im_init`/`im_shutdown` call `Init`/`Shutdown`; every audio export forwards its arguments (`shared_pid` copied); `exclusions` → `SetRules` before `Mixer::Start`; mode OFF creates no mixer; `im_set_paused` → `Mixer::SetPaused`; the fake mixer's chunks come out as Opus every 20 ms and, with `IM_AUDIOF_TAP_PCM`, as PCM every 10 ms; audio-only sessions; mute and pause keep silent frames flowing |
| WA35 | 4 | Windows audio watcher (no capture) | 05 A5; 05 §5.6, §5.7, §7, §12, §13.2 | `native/windows/src/audio/` (process table, endpoint and session notifications, meters, mic detection, device notifications, the watcher half of `AudioEngine`), `native/windows/tests/` (watcher tests), `internal/media/native` (`audio.go`, `audio_windows.go`, the `AudioTestHooks` wrappers), `deploy/windows/dll-imports.txt` (append) | WA11, WA14, WA23, WA26 | 2.5 | Mac, CI ubuntu, CI windows; owner's PC | Caller-fake tests of the Go side on the Mac and ubuntu; on `windows-native`: process-table tests against spawned children (creation times, image folders, `--user-data-dir`), 100 watcher start/stop cycles with the handle count back to baseline, the no-device state, `IM_INIT_NO_AUDIO`, `im_caps.audio_flags`; the binding's single mode mapping (`AudioEverything` and `AudioOff` at start, and `AudioEverything` in `SetAudioMode`, → `CodeInvalid` with no call, §3 item 15). Manual, right after group 6 with WA62a's tool: `isshoni-audiotest apps` gives the right verdicts for Discord, a browser and a Steam game |
| WA36 | 4 | Audio mixer core | 05 A4; 05 §9, §10.3, §10.4, §17 | `native/common/audiomix/` | WA03, WA26 | 3 | Mac, CI | The drift simulation (±500 ppm, jittered 10 ms bursts, 2 simulated hours): ring fill 40 ± 15 ms, zero underruns and trims, resampling ratio standard deviation ≤ 30 ppm; chunk times exactly on the 10 ms grid with 05 §10.4's `OnGap` rules; S1's click detector finds no click on any fade path, the onset fade included, also for a new stream with 30 ms of silence before its tone; the PI controller never emits ppm = 0; no allocation on the mixer thread after `Mixer::Start` (Linux `host-debug`); chunks exactly 10 ms apart with sound from t = 0; the C++ report writer matches WA26's goldens byte for byte |
| WA37 | 4 | Desktop audio policy service | 05 A8; 05 §14 | `internal/desktop/audio` | WA06, WA26 | 2.5 | Mac, CI ubuntu/macos/windows | 05 §19.1's Go tests (the rules builder, prompts with `Decide` limited to keep-out/include and `DismissPrompt`, the viewer browser per connection id, banners and tips, `FriendsHear` sorting, polling rates under synctest, the errors); the lifecycle test against the fake (watch, start, prompt, decide, pause, stop) under goleak; `RunSoundCheckHelper` stubbed off Windows |
| WA38 | 4 | Client core: recovery | 01 C11; 01 §11.6, §11.7 | `internal/client/core` (`pubdriver.go`, the recovery paths), `internal/server/itest` (`desktop_recovery_test.go`) | WA29 | 2 | Mac, CI | itest D3 (socket killed: resume, viewer gap < 500 ms, no republish), D4 (UDP black hole 5 s: ICE restart, rebuild when needed, viewer resumes on a keyframe), D5 (server restart: republish with `replaces` ≤ 5 s, a new `ConnectionID`), D6 (hold expiry at 30 s ± 1 s, engine stopped) |
| WA39 | 4 | Device tokens in auth | 02 DA4; 02 §6 | `internal/server/auth` | WA30 | 2 | Mac, CI ubuntu | Rotation, reuse revocation and every grace case (the same `exchangeId` after 10 min, no id at 100 s once, at 121 s, another id); bearer shape and expiry checks without a DB read; the 30 s cache invalidated before `ConnCloser`; `Touch`, `RenameDevice`, `RevokeOwnDevice`, the bearer cases of `RevokeDevice` and `LogoutEverywhere`; the `refresh_token_reused` alert; a grace re-issue keeps the replaced child as a spent row, and presenting it afterwards (at +101 s, inside the window, and at +15 min) gives `invalid_grant {reason: reused}`, the device gone, `device.refresh_reused` and the alert; M1's revocation matrix green |
| WA40 | 4 | Updater | 06 R8; 06 §8.4–§8.11 | `internal/update` (`update.go`, `fetch.go`, `state.go`, `apply_windows.go`, `policy_windows.go`, `apply_other.go`, the `updatetest` hooks) | WA25 | 2.5 | Mac, CI ubuntu/macos/windows | 06 §16's `update_test` (the schedule; `Enabled` false makes zero requests; the redirect rule; timeouts; hash and size mismatch; the disk check; atomic `state.json`; `PendingAtStartup` and the attempts guard; `ErrNotInstalled`; the beta listing and rate limits), `apply_windows_test` (exactly `/S /UPDATE /RELAUNCH`; `Apply` returns once `Local\io.isshoni.desktop.installing` exists, and nil with a WARN after 10 s) and `policy_windows_test`, under goleak; a release build contains no test-hook strings |
| WA41 | 4 | Desktop Go foundations and the process contract, part 1 | 03 DK3; 03 §8.1, §13.1–§13.4, §14, §17, §18.1 | `internal/desktop/{settings,applog,i18n,hotkey,servers,app,winos}` (the App skeleton and `OSHooks`, single instance, the running mutex), `cmd/isshoni-desktop` (main's first action, the DLL search order of 03 §13.4; main's §13.4 order, `--version`, `--updated-from`, the `--open-url` rule, the Pending values, `engine_release.go`, `engine_fake.go`), `web/desktop_catalog.go` (`web.DesktopCatalog()`, in the `web` root package), `web/src/i18n/lazy/desktop.en.json` and `desktop.ts` | WA10, WA15, WA20, WA22, WA31 | 3 | Mac, CI ubuntu/windows | 03 §22.1's rows for `settings` (atomic writes, `.bak`/`.bad` recovery, migrations), `applog`, `i18n`, `hotkey`, `servers` and `app` (link handling, the Pending values) pass on all OSes; on CI windows: W6 (a second launch forwards `--open-url` and exits 0), the mutex, `--version` (one JSON line with `engineAbi` 1, the ABI major, no window), and 03 §22.2's marker test (after main's first action, `im-marker.dll` in the current directory and on `PATH` doesn't load); a `PendingServer` value opens the "Add server" step without a network request |
| WA42 | 4 | SPA boot, routes and guards for the desktop | 03 DK4 (app part); 03 §7.2, §7.5 | `web/src/app/` (the boot desktop branch, `buildVersion: ''`, every M2 route, `RequireDesktop`, `RequireBrowser`, the desktop `RequireAuth`), `web/src/desktop/` (stub pages), the `role` audit's fixes in M1 folders (named in the PR) | WA32 | 2 | Mac, CI ubuntu | 03 §22.3's router and chunk tests; `check:size` passes; desktop routes never load in a browser and `/link` never in the desktop; the browser e2e suite green unchanged |
| WA43 | 4 | Latency proxies | 01 C16; 01 §12.4 | `internal/server/itest` (`desktop_latency_test.go`), `web/e2e/latency.spec.ts` | WA16, WA29 | 1 | CI ubuntu, Mac | D16: last-packet latency p95 < 60 ms and max < 200 ms over 20 s (Synthetic 1080p60 at 8 Mbit/s; under `-race` except on Windows); `latency.spec`: median `expectedDisplayTime − captureTime` < 150 ms and p95 < 250 ms, or median `expectedDisplayTime − receiveTime` < 100 ms with an annotation when `captureTime` is absent; values logged |
| WA44 | 5 | libavcodec encoders and the probe | 04 E7; 04 §9.1–§9.5, §9.10, §13.3, §14 | `native/windows/src/encode/` (`ffmpeg_api`, `ffmpeg_encoder`, `selector`, `probe` with its live-bitrate check, 04 §9.10), `native/windows/tests/` (`ffmpeg_load_test.cpp`, `mf_sw_test.cpp`), `native/common/tests/testdata/aus/`, `cmd/isshoni-enginetest` (new) | WA24, WA33 | 3 | CI windows; owner's PC | `ffmpeg_load_test` (the version checks; an empty folder gives `IM_ERR_DEPENDENCY` `missing` while an audio-only tone session still works; another major gives `version`) and `mf_sw_test` (or a logged skip) in CI; `dll_search_test`'s vendor-runtime and libvpl part (marker DLLs named after the vendor runtimes and libvpl's runtimes, with `ONEVPL_*` pointed at them, never load during a full probe) and the Go process-default check of `isshoni-enginetest`'s startup in CI; the committed AU fixtures feed WA12's `annexb_test`. Manual (owner's PC, both OSes): the full probe < 2 s; both `capture` runs per available encoder pass 04 §14's assertions at 1080p60 (first keyframe ≤ 500 ms; `-bitrate-steps`: rate within ±25 %, no `KEYFRAME`/`REINIT` between steps for a live encoder, exactly one `REINIT\|KEYFRAME` per applied step for AMF and MF; if QSV emits IDRs, the probe's live check clears its flag and the run passes as re-init); `bench` layer `f` encode p99 ≤ 16 ms; the tools zip's `isshoni-enginetest probe -quick` runs; 04 §9.3's settings and layer 0's VBV comparison (0.5 s against 0.1–0.2 s) in the PR under "Results for the spec" (`WB5` writes them back) |
| WA45 | 5 | Windows audio capture and mix, part 1: the include set | 05 A6 (part); 05 §6.2, §6.3, §8, §9, §10.5 | `native/windows/src/audio/` (activator threads, held `SYNCHRONIZE` handles, the commit check, process-loopback streams, the multiplexed MMCSS capture thread, the include set, the `Mixer` feeding 04's `PcmSink`), `native/windows/tests/` | WA09, WA34, WA35, WA36 | 2.5 | CI windows; owner's PC | The DLL builds; tests with fakes on `windows-native` for the activation timeout and the commit check (no INCLUDE on a tree that holds an excluded process, re-validated at commit). Manual (owner's PC, after the group, with WA44's tool): `isshoni-enginetest capture -audio mix` with Discord in a call and a browser playing gives a playable `audio.ogg` with the browser and without Discord (04 E13's PC acceptance) |
| WA46 | 5 | Client core: hints, codec switch, pause, encoder fallback | 01 C12; 01 D13–D15, §11.8 | `internal/client/core` (`hints.go`), `internal/server/itest` (`desktop_hints_test.go`) | WA27, WA28, WA38 | 2.5 | Mac, CI | itest D7 (codec switch through `Reconfigure` and a re-offer, back to High 60 s after the CB viewer leaves), D8 (layer pausing with `SetLayerActive`), D11 (pause: no media within 50 ms, `paused` and `audio_muted` in `Calls`, a late joiner gets no frame, resume order and a keyframe ≤ 500 ms, a pause from a web-role connection reconciled), D12 (a preset from another connection), D13 (encoder changed, then lost) |
| WA47 | 5 | Device REST routes | 02 DA5; 02 §6.4, §7 | `internal/server/httpapi` | WA39 | 2 | Mac, CI ubuntu | 02 §7.1's endpoint table (#44–#49, #52, `/device/password`, `PATCH /me/devices/{id}`); the credential matrix (5 access levels × none, cookie, bearer, bad bearer, both); the CORS matrix (desktop origins only with bearer, no `Allow-Credentials`, headers on error answers, preflight cached 600 s); `/info` lists `deviceFlow`; an admin's bearer on any `/api/v1/admin/*` route → 403 `forbidden {credential: session}`, while #21–#23 accept it; `TestAuditCoverage` and `TestAuditActionsInSource` pass with the new rows |
| WA48 | 5 | Bridge services and the desktop e2e harness | 03 DK5 (with DK4's Wails bridge); 03 §6, §7.4, §22.4 A | `internal/desktop/bridge` (every §6.2 method, one file per service; events; error JSON) and `bridge/codes`, `internal/desktop/e2ehost`, `cmd/isshoni-desktop` (`shell.go`: service and event registration), `web/src/platform/desktop/` (`WailsBridge`, `bindings/`, `codes.gen.ts`), `tygo.yaml` (`bridge/codes`), `web/src/i18n/lazy/desktop.en.json` (`desktop.errors.*`), `web/e2e/fixtures.ts`, `web/e2e/desktop-security.spec.ts`, the CDP config and spec skeleton under `web/e2e/` | WA21, WA31, WA41, WA42 | 3 | Mac, CI ubuntu; owner's PC | `desktop-security.spec` passes in Chrome on the Mac and in CI (the CSP, `/sw.js` 404, external links open the system browser, no `isa_` in storage, a foreign-Origin `/wails/runtime` gets 403, the runtime's own objects of W21 refused from the page: `Browser.OpenURL('ms-settings:…')` and `('search-ms:…')`, `Clipboard.Text()`, `Window.Hide()` → 403, `/link` opens the browser); the bridge unit tests pass; W11, W12 and W15 recorded in the PR under "Wails checklist"; the generated bindings are committed and regenerate without a diff; the CDP skeleton attaches to a `TAGS=e2e` build on the owner's PC and checks W1–W3, W5 and W7 (in CI from WA72) |
| WA49 | 5 | `install-desktop.ps1` | 06 R10 (its Windows run is WA72's); 06 §7.1–§7.3, §11.8 item 7(b) | `deploy/install-desktop.ps1`, `deploy/test/install-desktop.Tests.ps1`, `tools/repocheck` (`keys`: the ps1 key block), `Taskfile.yml` (`lint:ps`, `test:ps`, `release:prepare` stamping), `.github/workflows/ci.yml` (`lint-deploy` runs `lint:ps` and `test:ps`) | WA25 | 1.5 | Mac, CI ubuntu; owner's PC | Pester passes under `pwsh` (the checksum lookup, version regex, quoting helper, `ssh-keygen` discovery mocked, the exit messages; Smart App Control state 1 gives the message and no download, 2, 0 or missing proceed; a throwing `Start-Process` gives the `isshoni: Windows blocked the installer (…)` line); PSScriptAnalyzer at Warning and the ASCII check clean; no `exit` in the script; `lint:keys` fails when the script's key block differs from `deploy/keys/allowed_signers`. Owner (clean Windows 10 22H2): the default `SecurityProtocol` of a fresh `powershell.exe` and the two .NET registry values recorded in the PR, which fixes the `ps1Command` form (06 §7.5) |
| WA50 | 6 | Windows.Graphics.Capture and the picker | 04 E10; 04 §7.1–§7.3, §7.6–§7.9 | `native/windows/src/capture/` (the picker's STA thread with `IInitializeWithWindow`, pick tokens, window identity, WGC with borderless, `MinUpdateInterval`, `IncludeSecondaryWindows`, cursor, resize, minimize, close), `native/windows/tests/` (`window_match_test.cpp`), `native/windows/third_party/cppwinrt/`, `cmd/isshoni-enginetest` (`pick.go`), `deploy/notices/native-windows.json` (append) | WA44 | 3 | owner's PC; CI windows | `window_match_test` in CI. Manual (04 §18.4): M-PICK (both OSes), M-WGC-WIN, M-WGC-MON and M-SECONDARY (Win11) |
| WA51 | 6 | OpenH264 at run time | 04 E8; 04 §9.9, §12.5 | `native/windows/src/encode/` (the OpenH264 backend's files, registered through WA44's selector table), `native/windows/third_party/openh264/`, `native/windows/tests/` (`openh264_test.cpp`), `internal/media/native` (`openh264.go`), `cmd/isshoni-enginetest` (`openh264.go`), `deploy/notices/native-windows.json` (append) | WA33, WA35, WA44 | 2 | Mac, CI ubuntu/windows; owner's PC | 04 §18.3's OpenH264 helper tests on the Mac and ubuntu: short, long and oversized bodies rejected with nothing left behind, and a bzip2 bomb stopped after `Size + 1` bytes with the heap growing < 16 MB; `openh264_test` with Cisco's DLL, hash checked: encode/decode PSNR ≥ 35 dB, profile `42e0`, SPS and PPS before every IDR, a live bitrate step within ±25 %; a tampered download is rejected and leaves nothing behind; a wrong version gives `IM_ERR_DEPENDENCY` `version`; the Go pin equals `codec_ver.h`. Manual: M-OPENH264 |
| WA52 | 6 | Engine pause, stats and log privacy | 04 E14; 04 §5.6, §8.5 | `native/windows/src/session/` (pause, the stats JSON, the info-level log rule), `native/windows/tests/` | WA23, WA34, WA44 | 1.5 | CI windows | With the luma tap, no packet after pause plus one frame interval shows new content; audio ramps down in 10 ms, silent frames continue at 50/s and the PCM tap is digital zero; un-pausing keeps a user mute set with `im_audio_set_muted`; the stats JSON has every 04 §5.6 field; a log-capture test finds no window title or process name at info level |
| WA53 | 6 | Windows audio, part 2: fast path, endpoint engine, probe, sound check | 05 A6 (part); 05 §6.4, §6.5, §8.5, §11 | `native/windows/src/audio/`, `native/windows/tests/` | WA45 | 2 | CI windows; owner's PC | The DLL builds; tests with fakes: the fast path exits within 15 ms of a voice app's `session_created` and cuts retroactively at C − 20 ms, the kill switch works, the endpoint engine follows default-device changes, a failed startup probe falls back with the banner, the mode, mute and pause flags, the silent-stream health check, the test exports. Hardware: right after group 6 merges, the integrator runs WA62a's cases on the owner's PC (WA62a's acceptance); a failure becomes a `WF` slice that lists `native/windows/src/audio/` |
| WA54 | 6 | Client core: congestion end to end | 01 C13; 01 §10.1 | `internal/server/sfu` (the §10.1 constants, within their rows), `internal/client/ratectl` (constants), `internal/server/itest` (`desktop_congestion_test.go`) | WA46 | 1.5 | Mac, CI | itest D9 (a 3 Mbit/s uplink: a hint within 6 s, `f` settles ≤ 2.3 Mbit/s within 15 s, unrecovered loss < 2 %, the upload hint "540p" or "720p", back to 8 Mbit/s within 60 s once the limit lifts) D10 (a re-init encoder: no two changes < 10 s apart or < 30 %, each followed by a `Reinit` keyframe) and D19 (a 6 Mbit/s queue link with a 1 s limit: the queue < 150 ms within 6 s of the ramp crossing capacity, a congestion hint, no RTP lost); WA28's estimator tests still green |
| WA55 | 6 | Client core: stats, playback state, compat test | 01 C14; 01 §11.9, §11.11, §14 | `internal/client/core` (`stats.go`, `relay.go`, `compat_test.go`), `internal/server/itest` (`desktop_relay_test.go`) | WA18, WA46 | 2 | Mac, CI | itest D18 (`playback.state` → `EventPlaybackState` with `From`; an invalid payload ignored); `stats` messages every 10 s validate and reach the hub's metrics; `WatchServerStats` works; `TestCompatServer` passes against a locally started current server and skips without `ISSHONI_COMPAT_SERVER_URL` |
| WA56 | 6 | Device auth: hub and wiring | 02 DA6; 02 §6.3, §6.5 | `internal/server/signal` (the pre-auth refund, `DeviceName` filled), `internal/server` (`wire.go`: `site.ExtraOrigins = protocol.DesktopOrigins()`, `httpapi.Deps.DesktopOrigins`, `AuthenticateBearerToken`, `identityOf` and `Revalidate` copying `DeviceName`), `internal/server/itest` (`device_helpers_test.go` with `linkDevice`, `device_test.go` cases 1–3, 5 and 9–12) | WA47 | 1 | Mac, CI ubuntu | 02 §16.2's cases 1–3, 5 and 9–12 (the spent grace child, the admin bearer, the /48 bucket, the unknown network); a socket from `http://wails.localhost` with `hello.auth` is accepted, and with a cookie gets 403; 21 sequential bearer sockets from one IP all pass; on a device revoke both sockets close with 4401 within 100 ms; `client_outdated` works for the `desktop` kind; goleak clean |
| WA57 | 6 | Device-auth docs and write-backs | 02 DA12; 02 §9 | `docs/privacy.md` (server-side device rows), `docs/security.md` (credential threat model), `docs/troubleshooting.md` ("Linking the app" at `#desktop-linking`), `docs/m1/03-accounts-and-store.md` (pointers to 02) | WA47 | 0.5 | Mac, CI ubuntu | `task site:build` green; the privacy page's device rows match 02 §9; every M1 03 section that 02 supersedes points to it; M1 03 §7.12's bearer line reads "every user endpoint and the WebSocket; admin routes cookie-only" |
| WA58 | 6 | Native sharing, Go side | 03 DK7 (Go part); 03 §9, §10.1, §10.5, §10.6 | `internal/desktop/session` (C2 on 01's core, the `core.Auth` adapter over 02's Manager), `internal/desktop/share` (the controller and both mapping tables), `internal/desktop/bridge` (`share.go`), `internal/desktop/app` (wiring) | WA29, WA48 | 2.5 | Mac, CI ubuntu | 03 §22.1's `share` and `session` tests: the controller's synctest suite (picks held ≤ 120 s, `Release` on back-out, `StartShare` with its 10 s wait, every end reason, a share follows the SPA's room, `StopOther`), 03 §10.5's error mapping, the `core.Auth` adapter (`SessionEnded`, `HelloRefused`, a new core client after a relink) |
| WA59 | 6 | Process contract, part 2: one-shot modes and the installer handover | 03 DK12 (part); 03 §13.4; 06 §2.4 | `cmd/isshoni-desktop` (`--self-check=<path>`, `--forget-credentials`, `--purge-data`, step 2a: the `Local\io.isshoni.desktop.installing` probe and its Pending values) | WA31, WA41, WA44 | 1 | Mac, CI windows; owner's PC | `--self-check` writes §3 item 4's JSON and exits 0 only when `ok`; unit tests with fakes on the Mac and ubuntu; in `windows-go`, with a test credential prefix, `--forget-credentials` and `--purge-data` leave no credential and exit 0 while offline, and `--purge-data` also removes the data root; none opens a window or takes the mutex; 03 §22.1's start-order row (installer mutex present: a link start writes `PendingServer`/`PendingRoom` and exits 0 with no single instance, running mutex or `Apply`; absent: the normal order). Manual: `--self-check` passes against the `windows-native` artifact on the owner's PC (in CI from WA67's smoke) |
| WA62a | 6 | Audiotest harness, part 1: runner, analyzer, the cases without a microphone | 05 A7 (part); 05 §18.1–§18.7, §18.10 (gate configuration) | `cmd/isshoni-audiotest` (runner, fixtures with explorer reparenting, the analyzer ported from S1's `analyze.go`, report schema, `selfcheck`, `apps`, `run -gate` with the gate configuration, the cases 1–8, 10, 11, 15–21 and 24–27) | WA26, WA35, WA37 | 2 | Mac, CI; owner's PC | Analyzer and verdict tests on synthetic recordings pass on the Mac and in CI: Goertzel levels, the present and absent rule, clicks, the positive controls, every INCONCLUSIVE reason, a 20 ms burst of 440 Hz at −18 dBFS in a 10 s window FAILs absent, a faded 1000 Hz tone and its onsets never trip a short-window check at 907 Hz, case 5's session-lag rule (21 ms → FAIL), and the gate configuration (shipped `fastPath` false → cases 4, 5 and 26 INFO, the exit code unaffected); the Windows build; `selfcheck` passes on CI windows. Integrator, right after group 6 merges (owner's PC, Win11, then Win10 22H2): `run -case` with cases 1–8, 10, 11, 15–21 and 24–27, every required one passing (this is WA45's and WA53's hardware gate), and WA35's `apps` check; a failure becomes a `WF` slice that lists `native/windows/src/audio/`, never a harness change |
| WA60 | 7 | Desktop Duplication | 04 E11; 04 §7.4, §7.5 | `native/windows/src/capture/` (DD capture, access-lost handling, rotation, DD cursor shapes, the Win10 monitor mapping, `im_list_sources`), `native/windows/tests/`, `cmd/isshoni-enginetest` (`sources.go`), `docs/m2/04-windows-engine.md` (§7.5 only) | WA50 | 2 | owner's PC (Win10); CI windows | The Windows half of the monitor mapping passes with fake outputs in CI. Manual (Win10 22H2): M-DD (`capture_path` DD with `MAPPED`, no border, pointer shapes, a UAC prompt never captured, a re-init at a new resolution, upright output) and M-WIN10-WINDOW (the yellow border); 04 §7.5's mapping steps 1–3 and 5 only; what `DisplayName()` returns for each Win10 monitor (two outputs if available) recorded in 04 §7.5 with the verdict; if names are ambiguous for identical monitors, the integrator adds a `WF` slice for step 4 |
| WA61 | 7 | Encoder selection, fallback, device loss, profiles | 04 E9; 04 §9.6–§9.8 | `native/windows/src/encode/` (the selector), `native/windows/src/session/` (device loss ending the session, `im_set_profile`, `im_update_layer`, `im_set_content_hint` re-inits), `native/windows/tests/`, `cmd/isshoni-enginetest` (`sessions.go`) | WA51, WA52 | 2.5 | CI windows; owner's PC | The extended fault-injection cases of `session_fake_test`: candidate order, preview-layer fallback, one profile per session, re-init spacing of 10 s with the latest value winning, 1 s coalescing, the fallback event order (`ENCODER_CHANGED` before the new encoder's first `REINIT\|KEYFRAME`), injected device removal → `ENDED{IM_ERR_ENCODER_LOST}` `device_removed` within 1 s, then a new session on a new device. Manual: M-NVENC-LIMIT (`sessions -count 5` while OBS records with NVENC: every session starts, some layers report MF) |
| WA62b | 7 | Audiotest harness, part 2: mic cases, soak, records, redaction | 05 A7 (part); 05 §18.2, §18.5, §18.7–§18.9 | `cmd/isshoni-audiotest` (the mic cases 9 and 12–14, `multi-endpoint`, `soak` with `-long`, `record`, `merge`, report redaction, `README.md` with its results table) | WA62a | 1.5 | Mac, CI; owner's PC | Report redaction tests (no names, paths relative to the work folder, `other` for non-fixture apps) and a `merge` round trip on golden reports pass on the Mac and in CI; the Windows build. Manual (owner's PC, Win11): `run -gate` runs every case of 05 §18.5 and exits 0; a `record`/`merge` of R1 lands in a report. A failure in audio code becomes a `WF` slice that lists `native/windows/src/audio/`, never a change in this PR |
| WA63 | 7 | Device-link approval and Devices pages | 02 DA7; 02 §10 | `web/src/link/`, `web/src/account/` (DevicesPage rows: current, rename, remove; cookie-only actions hidden for devices), `web/src/lib/appLink.ts`, `web/src/i18n/` (`lazy/link.en.json`, two `errors.*` keys), `web/e2e/link.spec.ts`, `web/e2e/a11y.spec.ts` (the link states) | WA42, WA56 | 2 | Mac, CI ubuntu | `task lint typecheck test e2e` green; `link.spec`: start, approve, a Node poll gets an `isa_` token, a Node WebSocket gets `welcome`, Remove closes it with 4401, the signed-out redirect, the different- and unknown-network notes each need the tick and `same` has none; the initial JS stays under M1 05's 200 KB budget |
| WA64 | 7 | Device auth end to end | 02 DA11; 02 §13.4, §16.2 | `internal/server/itest` (`device_test.go` cases 4 and 6–8; `linkedManager`), `internal/client/deviceauth` (`compat_server_test.go`) | WA31, WA56 | 1.5 | Mac, CI ubuntu | 02 §16.2 complete with the real Manager and the Go signal client: presence shows one participant for the app's two connections; a revoke moves the Manager to `unlinked`/`signed_out` with the store emptied; reuse gives `refresh_reused`; `minClientVersion` gives the update verdict; goleak; `TestCompatServerDeviceFlow` skips without its env var and passes against a locally started server |
| WA65 | 7 | Native sharing, SPA side | 03 DK7 (SPA part); 03 §10.2–§10.4 | `web/src/platform/desktop/` (`DesktopSharing`, `DesktopShare`), `web/src/share/native/` (share sheet, first-share sheet, panel bar, Details disclosure, the unreachable state with Test connection), `web/src/rooms/` (`RoomSession.adoptShare`, `findShareElsewhere` with connection ids, `stopOther`), `web/src/viewer/` (the own share at the low layer only), `web/e2e/desktop-share.spec.ts`, `web/e2e/desktop-reload.spec.ts` | WA48, WA58 | 3 | Mac, CI ubuntu | `desktop-share.spec` without Pause (a fake pick → the first-share sheet → live; a web viewer decodes frames and audio energy; the own tile gets the low layer and no audio; the tray shows "Live, 1 watching") and `desktop-reload.spec` pass; component tests of the sheets. Manual: `task desktop:dev` on the Mac shares the 640×360 Decodable pattern into a room that a browser watches |
| WA66 | 7 | Server route `/install-desktop.ps1` | 06 R11; 06 §7.3, §7.4 | `deploy/embed.go` (new Go package `deploy`), `internal/server/httpapi` (`desktopscripts.go`, the router line), `internal/server` (`wire.go` lines), `internal/server/config` (the reserved env list only) | WA49, WA56 | 0.5 | Mac, CI ubuntu | 06 §16's `desktopscripts_test` (quotes doubled; an empty server for a non-ASCII or non-loopback `http://` origin; an empty version for dev builds; rc kept; markers checked; 200 with charset, `Cache-Control`, `ETag`, `HEAD` without a body); `/install-desktop.sh` is a 404 text and never the SPA; `task dev` serves a script pre-filled with `http://localhost:5173`; no unknown-env warning for 06 §7.3's names |
| WA67 | 7 | NSIS installer | 06 R4; 06 §2, §6 | `deploy/windows/nsis/`, `tools/desktoppkg` (`winver`, `stage`, `nsis`, `check`), `deploy/test/windows/smoke.ps1`, `Taskfile.yml` (`desktop:exe:windows`, `desktop:package:windows`), `.github/workflows/ci.yml` (`windows-desktop`, first part: exe, NSIS, stage, installer, smoke) | WA24, WA31, WA41, WA59 | 2.5 | Mac; CI windows; owner's PC | An installer builds on the Mac with Homebrew makensis; `makensis -WX` clean; the strings rule holds; on CI windows every `smoke.ps1 -Mode ci` case of 06 §16 passes: files, registry, the protocol command `"<InstallDir>\isshoni.exe" --open-url "%1"`, the shortcut AUMID, `--version`, `--self-check`, stale-file removal and the `.old` rename loop, repair, downgrade exit 4 and `/ALLOWDOWNGRADE`, a running app gives exit 3, `/D=` safety, credentials and toast keys gone after both uninstall modes, the data root removed only on purge; while `Local\io.isshoni.desktop.installing` is held, a second setup exits 8 and an app start exits 0 with `PendingServer`/`PendingRoom` written; the Finish page's "Next: go back to your browser and click Open isshoni" line comes from the language file. Manual: the interactive pages on the owner's PC; the `/UPDATE` banner shows (or 06 §6.4's fallback is recorded) |
| WA68 | 7 | Tray, window, notifications, system events | 03 DK9; 03 §4.4, §11.1, §11.5, §11.6, §15, §17 | `internal/desktop/{tray,app,winos}`, `cmd/isshoni-desktop` (`shell.go`: the Wails tray, the close hook, toasts, `-Embedding`; `build/tray/`), `web/e2e/desktop-system.spec.ts` | WA58, WA59 | 3 | Mac, CI ubuntu/windows; owner's PC | 03 §22.1's `app` and `tray` tests, the §22.2 Windows tests (WDA on the main window, the `winos` thread, the toast registry keys) and `desktop-system.spec` (a lock pauses through the controller and an unlock resumes, a suspend stops the share with a toast) pass; W7, W8, W14, W17 and W18 recorded in the PR under "Wails checklist". Manual: M-DESK-4, M-DESK-6, M-DESK-11 |
| WA69 | 7 | Hardware capture-to-send check | 01 C18; 01 §12.3 | `internal/client/core` (`capture_hw_test.go`, build tag `isshoni_hw`) | WA44, WA55 | 1 | owner's PC | Manual (owner's PC, both OSes; on the runner if one is registered): `TestCaptureToSend` gives capture → send p95 ≤ 35 ms and p50 ≤ 20 ms over 30 s at 1080p60 for each encoder the probe lists as tested; `mediatest.Conformance` passes with a real monitor per encoder |
| WA70 | 8 | Isshoni's own windows out of every capture | 04 E12; 04 §11 | `native/windows/src/window/`, `internal/media/native` (`IM_SRCF_SELF` → `invalid`/`self_window`, `ProtectWindow`, used only by `protect-check`), `native/windows/tests/`, `cmd/isshoni-enginetest` (`protect.go`) | WA51, WA60, WA68 | 1 | owner's PC | `isshoni-enginetest protect-check` passes on Win11 and Win10 for DD and WGC. Manual: M-WDA checks the affinity that 03's `winos` sets on the main window (WA68): the main window and its WebView2 content are absent from a full-screen share watched from a phone; the pill joins in WA80 |
| WA71 | 8 | First-run wizard, servers and the password link | 03 DK6, 02 §14 (the app's form); 03 §8.1–§8.3 | `web/src/desktop/` (`SetupPage` with the server, add, confirm and link steps including "Sign in with username and password instead", `RelinkPage`, the server switcher), `internal/desktop/bridge` (`auth.go`), `internal/desktop/app` (`auth.state`, `SessionEnded` from C1, server switching, Sign out, Remove) | WA48, WA63, WA68 | 3 | Mac, CI ubuntu | `desktop-link.spec` (address → confirm → code, QR code and short URL → a second browser context approves at `/link` → linked; a denied code; an unknown server shows "Add server {host}?" with no request before Add; the password path with a wrong, then a right password; the second-server case: with A active and B added and linked, no request or hello to A ever carries a token B issued) and `desktop-relink.spec` pass; component tests show the text for every probe error code and notice; the QR test passes; WebView2 autofill and password saving stay off |
| WA72 | 8 | `windows-desktop` job complete | 06 R5, R10 (its Windows run); 06 §10.1 | `.github/workflows/ci.yml` (`windows-desktop`: Go tests with `ISSHONI_TEST_ENGINE_DLL`, the CDP spec on a `TAGS=e2e` build, Pester and the grammar check under Windows PowerShell 5.1, the ps1 install against relserve, `isshoni-audiotest selfcheck`, the PR artifact; `protocol`: the bindings and `codes.gen.ts` drift checks; `build` and `e2e`: `DESKTOP_E2E_BIN`) | WA48, WA49, WA62a, WA65, WA67, WA68 | 1 | CI ubuntu/windows; owner's PC | A PR changing the app runs the job green: `TestNativeIntoPublisher` runs (not skipped), the CDP skeleton passes, the ps1 installs a CI build with the inbox `ssh-keygen` and refuses a tampered file and a wrong key, `selfcheck` passes; a hand-edited binding fails `protocol`; the desktop specs run in `e2e`. Manual: the PR artifact installs on the owner's PC, then M-APP-PICK (04 §18.4) on Win11 and then Win10: Share → the OS picker is modal over the main window, which keeps repainting → pick a window, then a monitor (Win10: the DD path) → live and watched in a browser; Cancel returns to the sheet; picking an isshoni window gives `self_window` and goes back to the picker |
| WA73 | 8 | Client core on bearer auth | 01 C15; 01 §11.3 | `internal/client/core`, `internal/server/itest` (`desktop_helpers_test.go` switches to `linkedManager`) | WA55, WA56, WA64, WA69 | 1.5 | Mac, CI | itest D1–D3 pass with bearer; `unauthenticated` → one forced refresh → a new connection → republish; a revoked device → `SessionEnded`, then `StopUnlinked` with the manager unlinked |
| WA74 | 8 | `/download` page | 06 R12; 06 §7.5, §7.6 | `web/src/download/`, `web/src/i18n/lazy/account.en.json` (`account.download.*`), `web/e2e/download.spec.ts` | WA49, WA63, WA66 | 1 | Mac, CI ubuntu | `links.test` and `DownloadPage.test`: release and non-release links, the exact installer URL, the ps1 command with the server's origin exactly in the form WA49 recorded (bare, or with the `[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072; ` prefix), `openAppUrl` = `isshoni://watch.example.com/` through `appLinkFor` and none for `http://localhost`, a valid `?room=` giving `isshoni://watch.example.com/r/<id>` and an invalid one the plain link; the three numbered Windows steps; the three warning lines (browser download, SmartScreen, Smart App Control) with their project-site anchors; `download.spec` on the e2e server and with `/info` routed to `0.2.0`; axe passes |
| WA75 | 8 | Audiotest workflow and the runner docs | 06 R15; 06 §10.4, §10.5 | `.github/workflows/audiotest.yml`, `tools/repocheck` (the no-PR-trigger test), `deploy/windows/runner/README.md` | WA24, WA62b | 0.5 | Mac, CI; owner's PC | The workflow has only `workflow_dispatch` (a repocheck test reads it), the repository and actor guard, §3 item 10's labels and approval environment, no checkout and no token use in `run`; the `release_run_id` input and the GitHub-hosted `fetch` job, the only job with `actions: read` (run end to end in WA81's dry run). Owner, only if the runner is registered (§7 question 6): a dispatch under Win11 and under Win10 produces a report artifact and a summary, and `suite: hwcheck` uploads WA76's bundle |
| WA76 | 8 | Soak, A/V sync and the hardware run script | 04 E15 (code part); 04 §14, §18.4; 05 §19.3 | `cmd/isshoni-enginetest` (`soak.go`, `avsync.go`: the flash window, the click process, the luma-tap and PCM-tap pairing), `native/windows/tools/hwcheck.ps1`, `Taskfile.yml` (`native:dist` adds `hwcheck.ps1` and `g2g.exe`) | WA53, WA60, WA61 | 1.5 | owner's PC | `hwcheck.ps1` runs the probe, `capture` per encoder, `bench`, `sessions -count 5` and `protect-check` and zips the reports without machine names or paths; PSScriptAnalyzer clean; `avsync`'s pairing logic has a unit test on synthetic times. Manual: `soak -hours 2` passes (≤ 5 % growth after the first 10 minutes, no `RING_OVERFLOW`, no `ENDED`); a first `avsync -dll ci` run on Win11 recorded (the gate run is WA90's); M-GAME recorded |
| WA77 | 9 | Audio in the app: What friends hear | 03 DK8 = 05 A9; 03 §8.4, §10.3, §12.1; 05 §14, §22 | `internal/desktop/bridge` (`audio.go`), `internal/desktop/share` (05's calls, the SharedPID hand-off), `internal/desktop/settings` (`audio.SettingsStore`), `internal/desktop/app` (`SelfInfo`, `SetVisible`, `EventPlaybackState` → `SetViewerBrowser`), `internal/desktop/winos` (the shell relaunch), `cmd/isshoni-desktop` (`--sound-check-helper` right after main's first action, the relaunch step), `web/src/share/native/` (What friends hear, levels, the mic prompt card, banners, the volume note), `web/src/desktop/` (the sound and voice wizard steps, Settings → Audio), `web/src/viewer/` (the endpoint-mode viewer mute) | WA37, WA65, WA71 | 3 | Mac, CI ubuntu/windows; owner's PC | Component tests of the panel, sheets and wizard steps; the share sheet renders under every `ISSHONI_FAKE_AUDIO` scenario (one snapshot each); `desktop-link.spec` runs the real sound and voice steps on the fake; the relaunch test passes where a shell exists; `bridge.Audio` binds `DismissPrompt` and no sound-mode method (§3 item 15); Settings → Audio shows the built-in list as locked text ("Always kept out: Discord, TeamSpeak, …"), the mic toggle, the per-app rules and the sound-check rerun, with no sound-mode control. Manual (owner's PC): with Discord in a call, the list shows Discord kept out and a browser heard; Share sound off, pause and resume stay silent; 05's R4 passes; M-DESK-5 |
| WA78 | 9 | Desktop notices and the native license gate | 06 R6; 06 §9 | `tools/notices` (`-native`, `-native-root`), `deploy/notices/` (the gate's checks of `native-windows.json`, `licenses/webview2-bootstrapper.txt`), `tools/desktoppkg` (`stage`: the LGPL files), `.github/workflows/ci.yml` (`desktop-macos`: go-licenses; `windows-desktop`: the notices step) | WA72 | 1 | CI ubuntu/windows/macos | The installed `THIRD_PARTY_NOTICES.txt` lists Wails' modules, the npm packages and the native section; a GPL native entry, LGPL without dynamic linking, an unlisted vcpkg port and a `LicenseRef` without an exception each fail; the `licenses\ffmpeg\` files are staged |
| WA79 | 9 | M2 exit run book | 01 §12.2; 04 §18.4, §18.5; 05 §18.8–§18.10, §19.3; 06 §11.8 | `docs/m2/exit-test/` (`README.md`, `results-template.md`) | WA08, WA62b, WA76 | 1 | Mac | §6's outline written out with exact commands: preconditions (the rc's `isshoni-exit-tools` artifact and its hash check, the `ci\` DLL for `avsync`), the audio half (the gate, R1–R5, R7, `avsync`), the latency half (the `gate`, `nocuts` and `nosound` runs), the release-candidate checks, the record template and the pass rules; no real names, hosts or IPs; every link resolves |
| WA80 | 10 | Share safety: hotkeys, Pause, the Sharing pill, audio alerts outside the window | 03 DK10; 03 §11.1–§11.4, §11.6, §11.7, §15; 05 §14.7 "Outside the window" | `internal/desktop/{share,app,winos,tray}` (hotkeys registered only while live, Pause from every control, the Win32 pill with WDA, accessible buttons and the alert text and amber marker, the DND tip, `tray.State`'s `VoiceAppsHeard` and `Questions`, the `audio.alert` toast, the close-to-tray notice in the window), `web/src/viewer/` (the `paused` tile, no freeze detection while paused), `web/src/share/native/` (the Pause control, the first-share sheet's hotkey line, the window notice), `web/e2e/desktop-share.spec.ts` (Pause, the late joiner, hotkeys, the audio alert case) | WA46, WA68, WA70, WA77 | 3 | Mac, CI ubuntu/windows; owner's PC | `desktop-share.spec` with Pause, a late joiner and hotkeys passes, and its audio alert case (`ISSHONI_FAKE_AUDIO=degrade-live`: `tray.Render` with `tray-warn`, "Friends hear voice apps" first and "1 question waiting"; `PillState` with the alert and the marker; exactly one `voiceAppsHeard` toast); `tips.trayNoticeShown` is set only once the window notice was shown; 03 §22.2's pill tests (topmost, no-activate, WDA, DPI, positions per monitor) pass on CI windows. Manual: M-DESK-2, M-DESK-3, M-DESK-8 (Win10), and M-WDA again with the pill |
| WA81 | 10 | Release pipeline with the desktop artifacts | 06 R13; 06 §11.1–§11.7 | `.github/workflows/release.yml` (`native-windows`, `desktop-exe`, `desktop-pkg`, `verify-windows`; the `build`, `sign`, `sign-dry` and `publish` changes), `.goreleaser.yaml` (`extra_files`, the header), `Taskfile.yml` (`release:assets`), `deploy/release/check-draft.sh` and its test, `.github/workflows/ci.yml` (the server-rendered ps1 test in `windows-desktop`) | WA25, WA49, WA66, WA78 | 2.5 | GitHub dry run; Mac | The release dry run is green end to end with ephemeral keys, with `verify-windows` installing through the released ps1 under Windows PowerShell 5.1; `sign` signs the manifest and `check-draft.sh` checks it; the new `check-draft-test.sh` cases pass; `goreleaser-check` passes with stubs; the server-rendered ps1 test installs a CI build and refuses tampered files, and its non-executable installer case ends with the `isshoni: Windows blocked the installer (` line and no unhandled error; `sign` refuses, and `check-draft.sh` fails, a non-empty `revokedKeys` not signed by the backup key (`check-draft-test.sh` covers both); `desktop-exe` cross-builds the exit tools and `desktop-pkg` uploads the workflow artifact `isshoni-exit-tools` (the tag's `isshoni-audiotest.exe`, `isshoni-enginetest.exe`, `g2g.exe`, `hwcheck.ps1`, and `isshoni-media.dll`, the FFmpeg DLLs and `native-manifest.json` copied byte for byte from `windows-native`'s `release\`; a step fails unless the DLL's SHA-256 equals the manifest's); not a release asset; an `audiotest.yml` dispatch with `release_run_id` = the dry run skips `build` and its `fetch` succeeds |
| WA82 | 10 | Settings page and version screens (SPA) | 03 DK12 (SPA part); 03 §14.4, §16.2, §16.3 | `web/src/desktop/` (`SettingsPage`: every §14.4 section, the hotkey field, the update states, About with versions, logs, notices and copy version info, the OpenH264 opt-in), `web/src/app/` (the desktop actions of `VersionMismatch`) | WA48, WA77 | 2 | Mac, CI ubuntu | A component test per settings section and per update state of 03 §16.2 (required banner, expired, failed, beta channel, `policyOff`); `VersionMismatch` offers "Update the app" and "Open in browser" (the current room) in the desktop and is unchanged in the browser; axe in both color schemes; `check:size` |
| WA83 | 11 | Handoff and the web entries | 03 DK11 (web part); 03 §12.2, §12.3, §13.2 | `web/src/rooms/` (`handoff.ts`: the `playback.stop` sender in the desktop and its receiver in tabs, the `playback.state` sender, "Open in the isshoni app" from `user.connections`), `web/src/viewer/` (the `handedOff` state, "Watch here instead"), `web/src/protocol/signal-client.ts` (hello features `agent.relay`, `user.connections`), `web/e2e/desktop-handoff.spec.ts` | WA55, WA63, WA77, WA80 | 2.5 | Mac, CI ubuntu; owner's PC | `desktop-handoff.spec`: a same-user tab stops with "Now watching in the isshoni app", "Watch here instead" resumes and its `playback.state` reaches the app, closing the tab sets `playing: false`, a phone-emulated viewer keeps playing; 03 §13.2's deep-link table tests. Manual: M-DESK-7, M-DESK-9 |
| WA84 | 11 | Settings services, autostart and updates in the app | 03 DK12 (Go part); 03 §13.4 step 5, §14.2, §16.1, §16.2 | `internal/desktop/bridge` (`settings.go`, `updates.go`), `internal/desktop/app` (the updater: refused while live, the tray offer at an autostart start), `internal/desktop/winos` (the `Run` value), `cmd/isshoni-desktop` (the start-time apply with `PendingRoom`, `--update-now`, `--autostart`) | WA40, WA59, WA80, WA82 | 2.5 | Mac, CI ubuntu/windows; owner's PC | `Settings.*` and `Updates.*` unit tests; the autostart round trip on CI windows (`--autostart` opens no window); an update offered in a test channel is shown, refused while live and applied at the next start with the Pending values (end to end in WA88); the OpenH264 opt-in installs and removes through WA51's helpers. Manual: M-DESK-10 |
| WA85 | 11 | Project site and docs for the app | 06 R14; 06 §12.2, §13 | `docs/download.md`, `docs/privacy.md` (`#desktop`), `docs/code-signing.md`, `docs/security.md` (`#keys`, `#verify-desktop`), `docs/troubleshooting.md` (the desktop anchors), `.github/workflows/site.yml` (`/keys/update_signers`, the `release.json` desktop fields), `internal/server/ops/doctor/docsanchors_test.go`, `README.md` (the keys table, the Windows app), `deploy/keys/README.md` | WA57, WA81 | 1 | CI ubuntu | The `site` job and `TestDocsAnchors` pass with every anchor of 06 §17; nothing is loaded from another origin; `/privacy#desktop` states 03 §19's and 02 §9's facts |
| WA86 | 11 | Nightly additions | 06 R16; 06 §10.3 | `.github/workflows/nightly.yml` (`windows-full`, `native-fuzz`, `windows-asan`, `goreleaser-full` with real desktop files, `compat-servers`, the cross-OS legs) | WA12, WA55, WA64, WA81 | 1 | GitHub | One green nightly with every row, or `compat-servers` skipped with its note while fewer than two final releases exist; against `v0.1.x` it asserts only `update_server`/`no_device_flow` |
| WA87 | 12 | Real-app e2e and the desktop manual matrix | 03 DK13; 03 §3.2, §20, §22.4 B, §22.5 | `web/e2e/` (the CDP spec complete), `docs/m2/03-desktop-app.md` (§3.2 results and §20 measurements only) | WA80, WA82, WA83, WA84 | 2.5 | CI windows; owner's PC | The CDP spec is green on CI windows (W1–W3, W5, W7, W9, W13, W16, W21's assertions: the runtime's `Browser.OpenURL('ms-settings:…')`, `('search-ms:…')`, `Clipboard.Text()` and `Window.Hide()` get 403 and no window opens, a device-flow link, a fake share watched in Chrome, close-to-tray while live, Quit within 3 s; a production build's debugging port stays closed); every M-DESK-1…12 row passed on Win11 and Win10 or has an accepted, recorded exception; 03 §3.2 is complete (the rows copied from the PRs' "Wails checklist" sections); 03 §20's startup and memory budgets are measured |
| WA88 | 12 | Updater end to end on Windows | 06 R9; 06 §8.11, §16 | `.github/workflows/ci.yml` (`windows-desktop` on `update` changes), `.github/workflows/nightly.yml` (`windows-full` always) | WA72, WA84, WA86 | 1 | CI windows | An installed `0.0.1-ci.1` updates to `ci.2` within 120 s through `--update-now` and relaunches with `--updated-from=0.0.1-ci.1`; a wrong SHA-256 leaves `ci.1` intact; a manifest for `ci.0` is ignored; `smoke.ps1 -Mode update`: under a held installing mutex a start leaves `attempts` at 0 and starts no installer, and the next start applies the pending `ci.2` and hands the link over |
| WA89 | 12 | Device-auth manual checks (owner) | 02 DA13; 02 §16.4 | – (results in the PR) | WA67, WA71, WA84 | 0.5 + the runs | owner's PC | Manual: M-DA-1 to M-DA-9 on Win11 and Win10 22H2 pass or have an issue filed |
| WA90 | 13 | M2 exit: audiotest gate and glass-to-glass latency (owner) | 05 A10, 01 C19, 04 E15 (exit run); 06 §11.8 items 5–6; §6 | `cmd/isshoni-audiotest/results/`, `cmd/isshoni-audiotest/README.md` (the results table), `docs/m2/exit-test/` (the record) | every slice of groups 1–12; an rc from WA81 | 1 + the runs | owner's PC | On `v0.2.0-rc.N`: `isshoni-audiotest run -gate` exits 0 on Win11 (24H2 or later) and Win10 22H2 from the rc's `isshoni-exit-tools` (`dll.sha256` equal to its `native-manifest.json`); R1, R3, R4 and R5 pass on both and R2 on one; R7 passes once on Win11 for each app the owner can run, the rest marked "unverified"; `enginetest avsync` gives \|median A/V offset\| ≤ 20 ms on Win11 (WGC) and Win10 (DD); the `gate` run of `g2g measure` (Share sound on, content cuts on, 01 §12.2) gives p95 < 200 ms over 60 s at 1080p60 on both (§7 question 1), with the `nocuts` and `nosound` runs recorded for diagnosis and the encoder, RTT and component numbers recorded; M-WDA, M-DD and M-WGC-WIN repeated on the rc installer; every result in the record, with no machine names, IPs or user names |
| WA91 | 13 | First release with the app, then the SignPath application (owner) | 06 R17, R18 (application); 06 §11.8, §12.3 | the release-prep PR (release notes header, README's keys table) | WA85, WA86, WA87, WA88, WA89, WA90 | 1 + the release | owner's PC; GitHub | 06 §11.8's checklist done: install paths (browser download and SmartScreen; the ps1 path without a dialog), Defender and VirusTotal, the rc.1 → rc.2 update, uninstall keeping data then purge, relink, cross-version (the rc app against `v0.1.x` shows "Ask your admin to update the server"); `v0.2.0` tagged, approved and published; the site serves the new `/download`; the SignPath Foundation application sent. Later (M3's time): 06 R18's signing jobs after approval |

## 6. M2 exit run book (outline)

WA79 writes this out as `docs/m2/exit-test/README.md`, with a results template; the owner runs it in WA90.

**Pass**: both halves on both Windows versions, on one release candidate.
1. Audio (05 §18.10): `isshoni-audiotest run -gate` exits 0 on Win11 (24H2 or later) and Win10 22H2 with the
   harness and DLL of the rc (the report's `dll.sha256` equals the rc's `native-manifest.json`); R1, R3, R4 and R5
   pass on both, R2 on at least one, R7 once on Win11 for each app the owner can run (the others "unverified");
   `enginetest avsync` gives |median A/V offset| ≤ 20 ms on both (WGC on Win11, DD on Win10); both reports with their
   manual entries are committed under `cmd/isshoni-audiotest/results/`.
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
.`; the real-app checks R1–R5 (and R7 on Win11) with `record -label Rn`; `isshoni-enginetest avsync -dll ci` (Win11
`-source pick` on the flash window, Win10 `-source mon:<id>`); copy the reports to the Mac; `merge`; commit as
`cmd/isshoni-audiotest/results/<date>-<win10|win11>-<build>.json` with `avsync.json` and add the README row. With a
registered runner, `audiotest.yml` (suite `release`, `release_run_id` = the rc's run) replaces the copy steps; the
real-app checks and `avsync` stay by hand.

**B. Latency** (about 25 minutes per OS; 01 §12.2 steps 1–10), after A on the same rc: `g2g serve`; the clock page in
Edge on the shared monitor, **Start** (the tone plays), F11; in the app share that monitor (Win11: borderless WGC;
Win10: Desktop Duplication), preset Auto, **Share sound on**; wait for 8 Mbit/s in Details; open the room in Chrome
signed in as the same user, focus the share and turn on its speaker, at 50 % size over the bottom-right half; check
that What friends hear shows Edge heard and Chrome kept out. Then `g2g measure -d 60s -run gate -server https://<rc
server> -o g2g-<win10|win11>-gate.json`, and the two diagnostic runs `nocuts` (`?cuts=0`) and `nosound` (a new share
with Share sound unchecked): three JSON files per OS. Record the encoder and GPU, the RTT, the versions, and the
component numbers next to them: the share's `latencyMs` and `isshoni-enginetest bench` (capture-to-packet p95 ≤ 25 ms).

**C. Release-candidate checks** on the same build: 04's M-WDA, M-DD and M-WGC-WIN; 03's M-DESK-1, 2, 3 and 8; 02's
M-DA-1, 2 and 7; 06 §11.8 items 7–11 (part of WA91).

**D. Record**: `docs/m2/exit-test/results/<date>.md` from the template, with the six `g2g` JSON files, both
`avsync.json` files, the paths of the two audiotest reports, every manual row and its result; no machine names, IPs or
user names.

**E. On a failure**: a fix slice and a new rc. A change to `native/`, `internal/media/` or `internal/desktop/audio`
repeats A on both OSes; any change repeats B and the C rows it can affect. When both pass: WA91 (release-prep PR, tag
`v0.2.0`, then the SignPath application).

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

## 8. Owner actions (not decisions)

- Group 1: run WA09 (the S1 bundle on Win10 22H2 and the late-start re-run on Win11) before WA45 merges in group 5.
- After WA07 merges: Actions → FFmpeg → Run workflow with `publish`, commit the printed `native/ffmpeg/lock.json`
  (before WA24's acceptance), and add a tag ruleset for `deps-*` (06 §14 items 2–3).
- Before WA81 (the release dry run): create the update signing keys on the Mac, merge `deploy/keys/update_signers`
  and the fingerprints, put `RELEASE_UPDATE_SIGNING_KEY` into the `release` environment, keep the backup offline
  (06 §14 item 1); set Actions to "Require approval for all external contributors" (item 4).
- If question 6 is answered yes: register the runner on both OS installs as 06 §10.5 and WA75's README describe.
- The "Manual:" items of the slices that list the owner's PC, with the `hwcheck` bundle where one applies (WA33,
  WA44, WA50, WA51, WA60, WA61, WA70, WA76), WA62a's audio cases right after group 6 merges (Win11, then Win10), and
  the matrices of WA62b, WA68, WA72 (M-APP-PICK), WA77, WA80, WA83, WA87 and WA89.
- With WA49: the TLS measurement on a clean Windows 10 22H2 (a VM or a fresh user profile), recorded in its PR.
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
| scope-7 | minor | Applied the cheaper option: results go into the PR body ("Results for the spec", "Wails checklist"); WA87 copies 03 §3.2, and `WB1` and `WB5` copy 06 §4.6 and 04 §9.3 (shared files row; 03 §3.2, 04 §9.3 and 06 §4.6 say so) |
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
