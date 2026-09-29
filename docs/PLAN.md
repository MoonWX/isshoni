# isshoni (一緒に): Project Plan

## Context

Discord keeps high-quality screen sharing behind Nitro. Free users get 720p30; full Nitro gets up to 4K60. **isshoni** is a
self-hostable, Go-based alternative for friend groups: everyone can share their screen **and system audio** and watch
each other at the same time, while still talking in their usual voice app (Discord, Discord Web, TeamSpeak, …).

**Main differentiator: "all system audio except voice apps".** Voice-app audio is left out of the shared stream, so
friends never hear their own voices echoed back. Discord has no such mode; it captures only the shared app's process
tree. Screego (the closest prior art: Go, GPL-3.0) runs in the browser only, so it can't exclude apps, and it sends
peer-to-peer to each viewer. isshoni fills that gap with:
- a single Go server with an SFU;
- native desktop sharers with per-app audio exclusion and hardware encoding;
- zero-install viewers in the browser, including phones.

Starting point: an empty repository. One developer; MacBook Pro M5 plus a
Windows x64 PC plus iPhone/iPad. The plan is based on two research rounds (12 researchers plus fact-checkers, about 250
claims checked, as of 2026-09-28) and a 4-lens adversarial review of the draft.

## Decisions

**Confirmed with the owner**

| Topic | Decision |
|---|---|
| Audio | Desktop: all system audio **except** voice apps (auto-detected) and isshoni's own playback |
| Scope | Screen + audio only. No built-in voice/text chat |
| Web | Viewer on every browser including phones (installable web app); basic sharer on Chrome/Edge desktop |
| Concurrency | Everyone shares and watches at once. **No numeric limits** on people or shares; only abuse guards and optional admin soft limits |
| Access | Local accounts (SQLite): admin plus invite links |
| Platforms | Windows, macOS, Linux desktop. Phones (iOS and Android) use the installable web app to watch. **Native iOS and Android apps are *pending***, until the owner registers as an Apple/Google developer (likely once the project gains traction) |
| Native code | Go owns everything except capture/encode, which lives in thin per-OS shims (C++ / ObjC / C) |
| Frontend | React + TypeScript (Vite) |
| OS support | "Broader": Windows 10 22H2+, macOS 14.4+, Ubuntu 22.04 / Mint 21 / PulseAudio-only Linux (degraded where noted) |
| Signing | macOS: free for now, with a stable self-signed certificate. When the owner joins the Apple program, switch to Developer ID plus notarization. Windows: SignPath Foundation. Mobile: pending |
| License | Apache-2.0: no GPL deps or copied GPL code (no x264, OBS, win-capture-audio or Screego code) |
| Hosting | Tuned for a cloud VPS with a public IP |
| Language | English only, i18n-ready |
| Watch-together | A "Movie" quality preset (screen-share any player). Streaming a local file comes later. No synced watch party |
| Team | Solo developer. All clients build from a shared core from day one; releases are delivered one platform at a time |

**Defaults chosen from the research**
- Custom Pion SFU inside the single binary; a LiveKit sidecar is Plan B.
- H.264 everywhere in v1; 1080p60 default, up to 4K60.
- The protocol supports N shares per user; the v1 UI offers one.
- End-to-end encryption, OBS/WHIP ingest, TURN, HDR, AV1/HEVC come later.
- TOML config.

## User journeys (the "foolproof" contract)

1. **Admin setup**
   - `curl … | sh` asks one question: "Domain name (Enter = use this server's IP)".
   - It offers to open ufw/firewalld ports, gets the certificate, then prints the setup link and a QR code.
   - The browser wizard has three steps:
     1. create the admin;
     2. connection test: the admin's browser does real ICE checks and shows UDP 7882 ✓/✗, TCP ✓/✗ and RTT, with fix
        text specific to the cloud provider;
     3. copy the invite link.
   - With Docker: `docker compose up`, then `docker compose exec isshoni isshoni setup-url`.
2. **Friend joins**
   - Invite link, then one form (username and password), and the friend lands directly in the default room
     **"Lounge"**.
   - The video plays right away and needs one "tap to unmute".
   - The room list and "Create room" stay hidden until an admin creates a second room.
3. **Watching**
   - A tile grid of everyone's shares; click to focus, and there is a fullscreen mode.
   - **Audio follows focus**: only the focused share's audio plays, and other tiles show a speaker button.
   - The most recent share is focused automatically.
4. **Sharing from the browser (Chrome/Edge)**
   - Share → browser picker. The recommended choice is "window + its audio", which gives per-app audio on Windows 11
     and macOS 14.2+ with a current Chrome.
   - A whole-screen share with system audio shows a warning that voice apps will be heard, with a link to the desktop
     app.
5. **Sharing from the desktop app**
   - The Share button offers "Get the desktop app (keeps Discord out of your audio)". It opens the server's `/download`
     page with per-OS installers.
   - The app links to the account with a browser approval (device flow), so the friend never types a password or
     server URL into the app.
   - Share → OS picker → Live. On the first share, the "Friends won't hear: Discord, TeamSpeak…" sheet appears.
6. **Phones**: open the invite link, then "Add to Home Screen" to get notifications ("Alex started streaming").
   Watching only. Sharing from a phone waits for the pending native apps.

## Architecture

```
                       ┌──────────── isshoni (one Go binary, CGO_ENABLED=0) ───────────────────┐
 Web / PWA (React) ──┐ │ :443 TLS ── HTTPS: SPA (go:embed) · REST /api/v1 · WSS signaling      │
 Win/mac app (Wails)─┼►│      └───── first byte ≠ 0x16 → ICE-TCP (RFC 4571) ─┐                 │
 Linux agent ────────┤ │ :7882/udp ICE mux · :7882/tcp ICE-TCP ───────────────┴─► Pion SFU      │
 (mobile apps: pending)┘│ SQLite (modernc) · certmagic · Web Push (VAPID) · admin unix socket  │
                       └────────────────────────────────────────────────────────────────────────┘
```

| Layer | Owns |
|---|---|
| **Native shim** (per OS) | Capture, GPU color convert and downscale, hardware H.264 encode (2 layers), audio exclusion capture and mix, Opus, capture timestamps |
| **Go** (server + `internal/client`) | Signaling, RTP packetization and RTCP SR, uplink rate control, device auth, settings, SFU |
| **TypeScript** (SPA) | All UI, viewer PeerConnection, browser sharing, stats display |

**Client roles and sessions**:
- A **Participant** is (user, room) and can hold several **Connections**.
- `hello` carries `role: full|viewer|publisher|agent` and `client {kind: web|desktop|mobile, version, os}`.
- The desktop app opens one Go `publisher`/`agent` connection (device-token bearer) and its SPA opens a `viewer`
  connection. Both attach to the same Participant: presence shows one person, and share rules apply per Participant.
- Two PeerConnections per publishing client: **publish** (the client offers) and **subscribe** (the server offers and
  renegotiates).
- Control messages (`share.request`, exclusions, "What friends hear") are relayed **only between connections of the
  same user**. This is how the web page drives the Linux agent and hands off to the desktop app.

## Repository layout and development environment

```
isshoni/
├─ go.mod                     module github.com/<owner>/isshoni · go 1.26 · toolchain go1.27.x
├─ cmd/isshoni/               server: serve | setup-url | doctor | healthcheck | admin (backup/restore/users/rotate-secrets)
├─ cmd/isshoni-desktop/       Wails v3 app (Windows, macOS)
├─ cmd/isshoni-agent/         Linux headless share agent
├─ cmd/isshoni-loadtest/  cmd/isshoni-audiotest/
├─ internal/protocol/         signaling structs (source of truth → TS via tygo) + golden fixtures
├─ internal/server/{config,httpapi,auth,store,signal,sfu,push,tlsmgr,ops}/
├─ internal/client/{signal,publish,ratectl}/   imports only Pion, x/net, protocol (gomobile-friendly)
├─ internal/media/            Engine interface + fake engine (test pattern + tones)
│    ├─ engine_windows.go     isshoni-media.dll via purego / x/sys (no cgo)
│    ├─ engine_darwin.go      cgo + ObjC (.m), static libopus
│    └─ engine_linux.go       cgo GStreamer shim (video) + pure-Go Pulse-protocol audio
├─ native/include/isshoni_media.h   C ABI v1 (shared by all engines and the audiotest)
├─ native/windows/            C++20 · CMake presets · MSVC · vcpkg.json (libopus)
├─ web/                       React + TS SPA (src/{platform,viewer,share,rooms,auth,admin,protocol,i18n})
├─ spikes/sN-*/               throwaway spikes, each with a README of results/decisions (not in release builds)
├─ deploy/{install.sh,install-desktop.sh,install-desktop.ps1,systemd/,docker/,compose.yaml,dev/linux.Dockerfile}
├─ docs/ (VitePress → GitHub Pages: install, code-signing policy, privacy)  ·  Taskfile.yml  ·  .tool-versions
└─ LICENSE (Apache-2.0) · NOTICE · THIRD_PARTY_NOTICES (generated)
```

**Development environment**:
- **Toolchains**: mise pins Go, Node 26, pnpm (via corepack) and Task.
- **`task dev`**: runs the server with `tls.mode=off` on localhost, plus Vite with a proxy for `/api` and `/ws`.
  localhost is a secure context, so getDisplayMedia works.
- **Other tasks**: `task gen` (tygo), `task test`, `task desktop:dev`.
- **macOS engine**: developed natively on the M5. macOS 14.4 and 15 VMs (UTM/Tart) are used for tap and picker checks.
- **Windows**: the Go side cross-compiles from the Mac (`GOOS=windows CGO_ENABLED=0`). The DLL builds with MSVC on the
  Windows PC (SSH/RDP) or in CI. The Windows PC dual-boots Win11 and Win10 22H2 to test both.
- **Linux**: Ubuntu 22.04/24.04 arm64 VMs for development, plus `deploy/dev/linux.Dockerfile`. **Hardware encode and
  audio tests run on Ubuntu booted from USB on the Windows PC.**
- **iPhone/iPad**: web viewer and PWA tests. Android Chrome: the emulator on the Mac, plus friends' phones.
- Rows without hardware (other GPU vendors, Fedora, …) are marked "community-tested" in release notes.

## Server

**SFU and media plane (`internal/server/sfu`)**, pinned to pion/webrtc v4.2.x (avoid APIs already deprecated for v5):
- **Model**: Room → Participant → Share (`shareId`), where a share is one video track (simulcast `rid`s) plus one Opus
  track with the same msid stream.
- **Two `webrtc.API` instances share one ICE mux.**
  - The *publish* API negotiates transport-cc (the TWCC feedback generator lets browsers run their own GCC), a NACK
    generator and goog-remb (so the server can send bitrate caps).
  - The *subscribe* API (v1) negotiates goog-remb, nack, pli and ccm fir plus abs-send-time, but **not** transport-cc.
    It has no Pion NACK responder and no default SR generator.
  - v2 turns on transport-cc/GCC for downlink.
- **DownTrack per subscriber** (a custom `TrackLocal` with its own queue and goroutine):
  - rewrites SSRC/PT/seq/timestamp across layer switches and header extensions (never forwards the publisher's TWCC
    seq);
  - forwards the publisher's latest SR, with RTP shifted by the DownTrack's timestamp offset;
  - keeps a compact sequence map from its own seq numbers back to (upstream seq, layer).
- **Packet cache per incoming layer**, sized by time (about 1 s at the measured bitrate), serves NACK/RTX, including
  audio NACKs. There is no per-viewer copy, so memory doesn't grow with the number of viewers.
- **Layer selection**:
  - The client sends `subscribe.update {shareId, video: high|low|off, audio: on|off}` (focused, thumbnail or hidden).
  - The server downgrades when the REMB/loss estimate is too low (Galene-style) and tells the viewer.
  - Layers switch only on keyframes. Forwarded PLI/FIR is throttled to about 1 per 500 ms per track.
- **Uplink control for native publishers** is server-driven: the SFU runs a loss and receive-rate estimator and sends
  REMB plus `quality.hint {maxBitrate}`.
- **Code reuse** (keep NOTICE attribution): Galene (MIT) for packetcache, estimator and protocol ideas. LiveKit
  `pkg/sfu` (Apache-2.0) for forwarder, rtpmunger and layer-selector designs.
- **Performance**: large UDP buffers (the installer sets sysctl) and keyframe-burst pacing. Use `sendmmsg` batching
  (pion/transport batchconn) if egress goes above ~50k pps.

**Networking**:
- **443/tcp** is multiplexed on the first byte: `0x16` goes to TLS (HTTPS/WSS); anything else goes to ICE-TCP
  (RFC 4571) through a custom `net.Listener` into `ice.TCPMux`. **ICE-TCP on 443 therefore works without TURN or a
  domain.**
- **7882/udp**: `ice.UDPMux` (IPv4 and IPv6), used for all media.
- **7882/tcp**: ICE-TCP, needed when `tls.mode=off`.
- **80/tcp**: ACME and redirect.
- The public IP is advertised with `SetICEAddressRewriteRules` (auto-detected by STUN, or set in config). Not the
  deprecated `SetNAT1To1IPs`.
- Embedded TURN is **Later**. When added, it relays UDP only, filters destinations by IP **and port** (only the ICE mux
  address), uses ≤10-minute HMAC credentials, allows at most 4 allocations per user, and has a test that refuses relays
  to loopback, metadata, private and :22 addresses.

**Signaling, sessions and reconnect (`internal/server/signal`)**:
- Versioned WebSocket JSON messages: `hello`, `room.join/leave`, `room.state`, `share.start/stop/request`,
  `pc.offer/answer/ice` (for the pub and sub PCs), `subscribe.update`, `quality.hint`, `agent.*` (same-user relay),
  `stats`, `error{code,retryable,scope}`, `ping`.
- **Reconnect**:
  - The WebSocket retries with jittered backoff from 0.5 to 10 s and sends `hello{resumeToken}`.
  - The server keeps the Participant, its shares and PeerConnections for a **30 s grace** period, then ends the shares.
  - ICE `disconnected` for 3 s triggers an ICE restart; `failed` rebuilds only that PeerConnection.
  - After a server restart, clients rejoin their last room and re-publish; the desktop keeps its encoder running for
    30 s.

**Accounts, auth and safety guards (`internal/server/{store,auth}`)**:
- **Tables**: `users`, `sessions`, `devices`, `invites`, `rooms`, `push_subscriptions`, `settings`, `audit_log`.
  Lounge is auto-created on first run.
- **Registration**: `invite` (default: 7-day links, 10 uses), `approval` (an admin approves sign-ups) or `closed`.
  There is no unmoderated open mode.
- **Setup and invite tokens** go in the URL **fragment** (`/setup#t`, `/invite#t`), so they never reach logs. They are
  stored hashed, and setup tokens are single-use and last 24 h. `/setup` returns 404 once an admin exists.
- Pages send `Referrer-Policy: no-referrer`; auth responses send `Cache-Control: no-store`.
- **Web sessions**: an HttpOnly/Secure/SameSite=Lax cookie, plus CSRF/Origin checks and a WebSocket Origin allowlist.
- **Native apps** (desktop, agent, and future mobile apps) use an **OAuth device authorization flow (RFC 8628)**:
  - The app shows a code and opens `https://server/link?code=…`; the user approves in their logged-in browser; the app
    polls and receives a rotating refresh token plus a short-lived access token.
  - Tokens are stored in the OS credential store; the server keeps only hashes. Refresh tokens rotate with reuse
    detection.
  - A Devices page can revoke any device. A password reset revokes everything.
  - The fallback is an in-app username/password form.
- **`isshoni://` deep links** carry only a server URL and room ID, never credentials. A new server needs an
  "Add server <host>?" confirmation. A link **never starts a share** by itself.
- **Guards** (limit abuse, not people):
  - argon2id with m=19 MiB, t=2, p=1 behind a semaphore of max(2, NumCPU/2) concurrent hashes. Per-IP and per-username
    throttles run first, and unknown usernames hash a dummy value.
  - At most 20 pre-auth WebSocket handshakes per IP per minute; WebSocket read limit 64 KiB; at most 2 PeerConnections
    per connection.
  - 10 s ICE-TCP/DTLS handshake timeout.
  - Admin transfer alerts.

**Config, TLS, secrets and storage**:
- `/etc/isshoni/isshoni.toml` is read-only to the service. Every key has an `ISSHONI_*` env override, and flags are also
  accepted.
- **TLS modes** (certmagic):
  - `auto`: ACME for a domain.
  - `ip`: Let's Encrypt short-lived IP certificate; `DefaultServerName=<IP>`; pin a certmagic version that allows IP
    identifiers.
  - Self-signed certificates are not supported: webviews, PWAs and native apps reject them. Every mode ends with a
    publicly trusted certificate or a proxy that has one.
  - `manual`: user-supplied certificate files.
  - `off`: behind a proxy, trusting `X-Forwarded-*` only from configured CIDRs.
- **Generated secrets** (session, invite, resume and VAPID keys) are created once in `/var/lib/isshoni/secrets.json`
  (0600), next to the certmagic storage. systemd uses `StateDirectory` and `ConfigurationDirectory`.
  `isshoni admin rotate-secrets` rotates them.
- **SQLite**:
  - WAL mode with busy_timeout; migrations are forward-only.
  - Before migrating, the server writes `VACUUM INTO backups/pre-<ver>-<ts>.db` and keeps the last 5.
  - It refuses to start on a newer schema and tells the user which backup to restore.
- **Admin CLI**: `isshoni admin backup|restore` covers the database, secrets and certs. Admin commands go through
  `/run/isshoni/admin.sock` (in Docker, via `docker exec`), so root never creates the WAL files.

**Operations**:
- `/healthz` and `/readyz` (DB, TLS and UDP mux ready); the `isshoni healthcheck` command doubles as the Docker
  HEALTHCHECK.
- Logs use slog: text on a TTY, JSON under systemd or Docker. A `Secret` type redacts tokens and passwords. No SDP in
  logs; HTTP access logs are off.
- Optional Prometheus `/metrics` on `127.0.0.1:9469`, off by default.
- **Admin dashboard**: live rooms, and per share: bitrate, layer, loss and egress; client versions; month-to-date
  transfer; last doctor result; "isshoni vX available (security fix)", from a daily GitHub Releases check that can be
  turned off.
- **`isshoni doctor [--json]`**: DNS, public IP and CGNAT detection, TLS, clock skew, and a bandwidth calculator
  (egress ≈ Σ viewers × (focus bitrate + thumbnails × preview bitrate)). Its CLI port test is labeled *local only*; the
  real reachability test is the browser connection test.

## Media and quality

| | v1 |
|---|---|
| **Codec policy** | **H.264 for every sender.** Clients report decode capabilities in `hello`; the server keeps a per-room safe set. Native senders use High profile (advertised as 64001f and 640c1f) and switch to Constrained Baseline 42e01f with an IDR if any viewer can't decode High. Firefox is assumed unable until S4 proves otherwise. Browsers call `setCodecPreferences` with H.264 first. MediaEngine: H.264 42001f/42e01f/4d001f/640c1f/64001f with RTX, plus Opus. VP8/VP9/AV1/HEVC come Later |
| **Layers** | **full** (source, ≤1080p60 by default, ~8 Mbps; 1440p60 12–20 or 4K60 25–40 Mbps under Advanced) plus **preview** (360p15, ~0.3 Mbps). A middle 720p30 layer comes in M5 |
| **Audio** | Opus 48 kHz stereo, `contentHint=music`. The publish answer sets `maxaveragebitrate` to the preset (128k by default, 256k for Movie). The subscribe side advertises 510k and `stereo=1`, which the Chrome viewer adds to its own SDP. FEC is negotiated but only helps in SILK/hybrid mode. Audio NACK is served from the cache |
| **A/V sync** | The native engine stamps every access unit and audio frame with its **capture time** (QPC / mach_absolute_time / CLOCK_MONOTONIC). Go computes `rtp_ts = base + (t_capture − t0) × clock` using its own packetizer, and **sends its own SRs**: NTP = wall clock at capture time, RTP = that frame's timestamp. No `report.SenderInterceptor` on publish tracks. The SFU forwards SRs (see DownTrack). Target offset under 45 ms |
| **Rate control** | Browsers: their own GCC over TWCC. Native: server hints applied with hysteresis (changes of at least 15%, at most every 2 s). NVENC, QSV, VideoToolbox, MediaCodec and GStreamer change bitrate live. AMF and MF via FFmpeg need a re-init plus IDR (at most every 10 s, only for changes of 30% or more); direct AMF/MF control comes in M5. Pion GCC stays off until `pion/bwe` ships |
| **Keyframes** | Sent on PLI (throttled); GOP 2–4 s. SPS/PPS go in-band before every IDR. The last frame is re-encoded on PLI when the screen is static |
| **Presets** | "Optimize for: **Auto** · Game · Movie · Text". **Auto**: balanced, 1080p60 cap, starts at ~4 Mbps and ramps to 8 Mbps, never above ~80% of measured uplink. **Game**: motion, keep frame rate. **Movie**: motion plus 256 kbps audio (the watch-together preset). **Text**: detail, keep resolution. When upload limits quality, the sharer sees "Your upload allows about 720p" |

Content protected by streaming-service DRM (Netflix, etc.) is captured as black on every platform, and the docs say so.

Bandwidth example: 10 people all sharing, each watching 1 focused share plus 8 thumbnails, uses about **10.5 Mbps per
viewer** and **~105 Mbps of server egress**. Everyone watching everything at full quality would be about 730 Mbps.

## Web client (`web/`: React 19 + TS + Vite)

- **Stack**: React Router, Zustand, TanStack Query, CSS modules. Vitest for unit tests; Playwright (channel `chrome`) for
  e2e. The SPA is embedded with `//go:embed all:dist`.
- **i18n**: react-i18next with one catalog, `en.json`, enforced by ESLint `i18next/no-literal-string`. Go-rendered
  strings (tray, notifications) use the same catalog. Servers return error `code`s, never English text.
- **Accessibility**: the grid, focus and fullscreen are keyboard-navigable; controls are labeled; `aria-live` announces
  share start/stop; `prefers-reduced-motion` is respected; `jsx-a11y` lint and axe checks run in e2e.
- **Platform adapter** (`src/platform`):
  - `BrowserPlatform`: getDisplayMedia plus RTCPeerConnection.
  - `DesktopPlatform` with two transports: Wails bindings (Windows/macOS) and the same-user agent relay over signaling
    (Linux agent, and web → desktop handoff).
  - The viewer component is shared by all of them. A `MobilePlatform` slot is reserved for the pending native apps.
- **Web sharer**:
  - `getDisplayMedia` with `frameRate` 60, `windowAudio:'window'`, `systemAudio:'include'`, `restrictOwnAudio:true`,
    `suppressLocalAudioPlayback:false`, and echo cancellation, noise suppression and AGC off.
  - Always `setParameters()` with maxBitrate/maxFramerate and simulcast (full plus preview).
  - Detect features at runtime rather than relying on Chrome version numbers.
- **PWA**: manifest plus a service worker (app shell only) and **Web Push** (VAPID, RFC 8291) for "X started streaming".
  It works on desktop browsers, Android Chrome and iOS 16.4+ Home Screen web apps, with no Firebase and no APNs. The
  server rejects push endpoints that resolve to private IPs.
- **Pages**: invite sign-up, room (Lounge by default), download (per-OS installers), account (devices, notifications),
  admin (users, invites, approval queue, limits, dashboard, doctor).

## Desktop clients

### Windows and macOS app (`cmd/isshoni-desktop`, Wails v3)

Wails v3 is pinned to an exact beta (e.g. `v3.0.0-beta.26`) and follows its GA.

**Window and UI**:
- **Window-first**: one main window runs the **bundled** SPA. The share sheet (exclusions, "What friends hear", "Optimize
  for") is a React panel in that window.
- **OS pickers only**: GraphicsCapturePicker on Windows (via IInitializeWithWindow) and SCContentSharingPicker on macOS.
  Every share requires the user to pick in OS UI, so a web page cannot start a capture silently.
- **Tray**: a live indicator plus Stop / Open. Closing the window while live minimizes to the tray with a one-time
  notice.

**Security**:
- The webview loads **only the bundled SPA**. Navigating to other origins is blocked; external links open in the system
  browser; a strict CSP applies.
- The SPA talks to the server with bearer device tokens, and the server's Origin allowlist includes the Wails asset
  origins. Version skew is handled by the compatibility policy.

**First-run wizard**:
1. Link the app to the account (device flow).
2. macOS: request "System Audio Recording" now, not at the first share.
3. **Sound check**: a helper plays a chime and the app confirms it was captured, which catches a denied grant or silent
   tap.
4. "Friends won't hear: Discord…" → Done.

**Viewing**: in the webview (WebView2 on Windows, WKWebView on macOS). The updater is Ed25519-signed (see Install).

### Linux agent (`cmd/isshoni-agent`, no webview)

WebKitGTK ships without WebRTC until about 2027, and Wails needs different GTK builds for 22.04 and 24.04. So on Linux
there is no webview:
- **Agent**: a headless Go agent with a cgo GStreamer shim for video and a pure-Go Pulse client for audio.
- **UI**: the user's isshoni web page. It shows "Your Linux helper: ready" and drives the agent through the same-user
  relay. Source selection uses the portal's own dialog on Wayland, or a list the agent provides on X11.
  **Remote-initiated shares always show the portal dialog** (no restore token) plus a desktop notification.
- **Viewing**: the agent opens the room in an **isshoni-launched browser app window** (Chrome/Chromium/Edge/Brave:
  `--user-data-dir=<isshoni> --app=<room>`, or Firefox `--profile <dir> --no-remote`), so its audio can be excluded by
  process tree. If no such browser is found, the user's normal browser is excluded automatically, with the reason shown.
- **Tray**: StatusNotifierItem over D-Bus (fyne.io/systray, no GTK), plus XDG autostart.
- **Build**: one build in an `ubuntu:22.04` container (glibc 2.35), with GStreamer elements probed at runtime. One
  `.deb`/`.rpm` covers 22.04 through current releases.

### Native engine C ABI (`native/include/isshoni_media.h`, v1)

- **Calls**: `im_version`, `im_probe(caps*)` (encoders found, plus a one-frame test encode, audio backend and OS feature
  flags), `im_list_sources`, `im_list_audio_apps` (with a `uses_mic` flag), `im_start(cfg{layers:[{rid,w,h,fps,bps}],
  audio, exclusions})`, `im_set_exclusions`, `im_next_packet(timeout)`, `im_request_keyframe(layer)`,
  `im_set_bitrate(layer,bps)`, `im_stop`, `im_last_error`.
- **Packets** from `im_next_packet` are `{kind, layer, keyframe, capture_ts_ns, annexb|opus, reinit}`.
- **Pull-based**: native code writes to lock-free rings and a Go goroutine drains them. Native code never calls into Go
  from a real-time thread. Packet memory stays owned by native code until the next call, and Go copies it.
- **Errors**: `PERMISSION_DENIED`, `ENCODER_LOST`, `SOURCE_GONE`, `AUDIO_DEVICE_CHANGED`, `UNSUPPORTED_OS`. Each maps to
  a UI state: guided fix, fall back to the next encoder, stop with a notice, or rebuild audio without stopping video.
- **Fake engine**: a Go fake (test pattern plus tones) implements the same interface. Every client compiles and runs in
  CI from day one, and the load test uses it.

### Engines per OS

| | Windows (C++ DLL) | macOS (cgo + ObjC, static) | Linux (cgo GStreamer + Go) |
|---|---|---|---|
| **Video** | WGC via GraphicsCapturePicker; Win11: borderless and >60 fps on 24H2. Win10: DXGI Desktop Duplication for monitors (no border); WGC windows keep the yellow border | SCContentSharingPicker (no Screen Recording permission needed); know what was picked on 15.2+ | Wayland: portal (godbus, host Registry) → `pipewiresrc fd= path=` (`target-object` on newer PipeWire), keepalive 100 ms. X11: `ximagesrc use-damage=false`, cropped per monitor, fixed-size scaler |
| **Encode** | libavcodec from an LGPL FFmpeg build: NVENC / QSV / AMF / MF (`scenario=display_remoting`) from D3D11 textures. Two sessions via a D3D11 VideoProcessor downscale. GeForce allows 8 NVENC sessions (≈4 shares) | VideoToolbox H.264, 2 sessions (VTPixelTransferSession downscale); low-latency RC up to 1440p60, RealTime mode at 4K | Probe order: `vah264enc` (≥1.22) → `vaapih264enc` (22.04) → `nvh264enc` → OpenH264. `tee` produces 2 layers |
| **Software fallback** | Cisco OpenH264 loaded at runtime (user-enabled, checksum-verified, attribution shown); MF H.264 comes first | not needed | Cisco OpenH264 loaded at runtime (the C API directly, not `openh264enc`) |
| **Audio minus apps** | **Include-set mixer**: watch audio sessions on all endpoints; open one `INCLUDE_TARGET_PROCESS_TREE` loopback per non-excluded process; mix on a 10 ms QPC clock with explicit 48 kHz float format. Use a single `EXCLUDE` stream when the excluded set is one tree. **Startup probe** (capture a 200 ms tone) → endpoint loopback plus banner if it fails | **Core Audio process tap** `stereoGlobalTapButExcludeProcesses` on a private aggregate device clocked by the real output. Match bundle-ID prefixes and PIDs (Electron helpers). Update or rebuild on process-list, default-device and sample-rate changes and on runs of zero buffers. `bundleIDs` + `processRestoreEnabled` on 26+. Own resampler on 14.x; skip unknown process objects; 30 s permission-prompt watchdog | **Pulse protocol in pure Go** (jfreymuth/pulse): one record stream per non-excluded sink-input (`DirectOnInputIndex`, DONT_MOVE), mixed on a 10 ms clock, reopened when a stream moves. Works on **both PulseAudio and PipeWire** (pipewire-pulse). A native PipeWire patchbay comes Later |
| **Mic detection** | Sessions on capture endpoints | `kAudioProcessPropertyIsRunningInput` (polled) | source-outputs |
| **Own windows** | `WDA_EXCLUDEFROMCAPTURE` on every isshoni window | SCK ignores `sharingType` on 15+: exclude isshoni in the SCContentFilter if a picker filter allows it (S2), otherwise auto-minimize and mute during full-screen shares | No API: warn when a full screen is shared while the viewer window is open |
| **Min OS** | Win10 22H2 (process loopback works from 19041 in practice); Win11 24H2 recommended | 14.4; one universal binary, runtime `@available` gating; Intel slice kept | Ubuntu 22.04 / Mint 21 (Pulse, GStreamer 1.20, X11 or Wayland) through current |

### Audio exclusion rules

**Default: "Keep out apps that use my microphone".**
- A short, tested built-in list is always kept out: Discord / PTB / Canary / Vesktop, TeamSpeak 3/5/6, Mumble, Zoom,
  Teams, Slack.
- Any other app that starts using the microphone (KOOK, QQ, in-browser voice, …) triggers a one-tap prompt, remembered
  per app: "X is using your microphone. Keep it out of your stream? [Keep out] [Include]".
- The app being shared is **never** auto-excluded, because games have their own voice chat. A warning is shown instead.

**Matching**:
- *App rules* match the audio-producing process or any ancestor. The ancestor walk stops at launchers and shells
  (explorer, steam, Epic, Battle.net, Heroic, Lutris, launchd, systemd, shells), so games launched from Steam are never
  muted.
- *Instance rules* (isshoni itself, and browser windows isshoni launched) match every descendant.
- Windows never opens an INCLUDE stream on a process whose subtree contains an excluded process.

**Never re-capture isshoni playback**:
- Windows: exclude all isshoni.exe descendants (msedgewebview2).
- macOS: match the WKWebView GPU process by *responsible PID* (dlsym SPI; S2 verifies). Never match
  `com.apple.WebKit.GPU` by bundle ID, because that also catches Safari. If that fails, mute in-app viewer audio while
  sharing with audio, or play it natively.
- Linux: the isshoni browser window is excluded by process tree.
- After handoff from the web, the server tells that user's web tab to stop playback ("Now watching in the isshoni app").
- A browser that has the user's isshoni viewer session open is excluded automatically, with the reason shown.

**Verified on Windows (2026-09-29):** include-set exclusion works on Windows 11 25H2 and Server 2025. The only artifact
found (a click when a new app's stream attaches) is fixed by an onset fade. Firefox viewers work, but a fresh Firefox
needs OpenH264 first, so the SFU must retry a subscription that failed on the codec.

**Verified on macOS 27 (2026-09-29, `spikes/s2-mac-audio`).** One global exclude tap keeps voice apps out as digital
silence. That covers Electron-style helpers, helpers with no bundle ID, apps that start mid-share, and isshoni's own
WKWebView (matched by responsible PID). Changes to the design:
- Excluding or re-including an app **while it plays** cuts it within one sample (a click). The engine builds a second
  tap and crossfades over 20 ms; two taps align within ±1 sample.
- `IsRunningInput` is also true for tap readers. Mic detection must check the process's input devices
  (`kAudioProcessPropertyDevices`) for a real device.
- Apps that start mid-share don't leak even without `bundleIDs`: the process-list listener wins the race.

**Discord Web's own screen share** (checked in its code, build 622805, 2026-09-29) passes none of Chrome's
audio-exclusion options. Sharing the entire screen or a window with system audio therefore echoes the call back to
everyone, and includes every tab. Sharing a single tab carries only that tab's audio (measured in Edge 153: YouTube tab heard, the call's own tab
absent). This is part of isshoni's pitch.

**Discord Web** (browsers mix all tabs into one process): when a browser uses the mic, show "Your browser is using the
mic (Discord Web?). Keep the whole browser out; friends won't hear any of its tabs. Tip: the Discord app works best."
Opening Discord Web in a separate window is an opt-in feature for Later.

**Transparency**: a live **"What friends hear"** list plus a level meter. Degraded modes are always shown, never silent.
Example banner: "Voice apps can't be excluded on this PC. Set your voice app to a different output device."

### Share safety

- While sharing, the tray turns red: "Sharing <source> · N watching".
- Global hotkeys for Pause (frozen frame plus muted audio) and Stop. On Windows with a borderless capture, an
  always-on-top "Sharing" pill (itself excluded from capture) is required.
- The picker defaults to a window. The first full-screen share shows a Do Not Disturb/Focus tip.
- The sharer always sees who is watching. There are no hidden viewers.

## Mobile

**Now (from M1)**: every phone can watch through the web app. It is a responsive SPA that can be installed (Add to Home
Screen) and sends Web Push notifications. Known limits on iOS: foreground only, tap to unmute, unreliable PiP.

**Pending: native iOS and Android apps.** They start when the owner decides to register as an Apple or Google developer
(e.g. once isshoni gains traction). Nothing is built for them now, but the design keeps the door open:
- `internal/client` stays gomobile-friendly;
- the protocol has `client.kind=mobile` from v1;
- the SPA has a `MobilePlatform` slot;
- the web app's Web Push works unchanged.

The designs below come from the research, so work can start directly when unblocked.

- **Shared shape**: Capacitor (8 → 9) wraps the bundled SPA; viewing uses the WebView's WebRTC. Sharing uses a thin
  native plugin for capture and hardware encode, which hands encoded frames to the **same Go client core** through
  gomobile (`Start / WriteVideo / WriteAudio / Stop` plus an Events callback).
- **iOS** (needs the $99 program, which unlocks TestFlight/App Store and also Developer ID plus notarization for macOS):
  - iOS 27+ allows in-app ScreenCaptureKit: `SCContentSharingPicker` → `SCStream` with `capturesAudio`, no broadcast
    extension needed. Limits: no frame-rate or format control, a restricted picker (check `isAvailable`), and
    `excludesCurrentProcessAudio` works only from 27.2. iOS ≤26 would need the deprecated ReplayKit extension; skip
    it unless friends need it.
  - There is no per-app audio filter on iOS. Evidence suggests the OS already leaves VoIP audio out of the capture.
  - **Gate**: a real-device spike checking 30+ min of background capture during a game, and that Discord voice is
    excluded while game audio is kept. A native PiP viewer uses `AVSampleBufferDisplayLayer`.
- **Android** (Google developer verification is needed for smooth installs from 2027; $25 plus ID, which also unlocks
  Play):
  - **Viewing**: Activity PiP auto-enter, a mediaPlayback foreground service, UnifiedPush (no Firebase).
  - **Sharing** uses a Kotlin plugin:
    - a mediaProjection|microphone foreground service with fresh consent per session and an `onStop` handler (screen
      lock and the status-bar chip end the share);
    - VirtualDisplay → MediaCodec hardware H.264 with no B-frames, headers before every sync frame, repeat-previous-frame
      and bitrate changes through `setParameters`;
    - AudioPlaybackCapture (MEDIA/GAME/UNKNOWN only, so call audio is never captured), plus `excludeUid` for a voice-app
      list (Discord, TeamSpeak, Mumla, Element, Signal, WhatsApp, Telegram, Zoom, Teams) and for isshoni itself
      (`allowAudioPlaybackCapture=false`);
    - encoded frames go to the **same Go client core** through a gomobile AAR (`Start / WriteVideo / WriteAudio / Stop`
      plus an Events callback).
  - Build targets: minSdk 29, target 36, gomobile `-androidapi 24`, NDK r28+ (16 KB alignment by default), and a CI
    check of that alignment.
  - **Distribution**: Google Play, GitHub Releases (Obtainium), IzzyOnDroid, F-Droid (reproducible builds), all
    shipping one developer-signed APK.
- **Effort once unblocked**: about 8 weeks for Android (viewer, then sharer) and a similar amount for iOS after its
  spike.

## Privacy and trust model (stated on the login page, in "Add server", and in the docs)

- **The server is trusted in v1.** Media is encrypted in transit, but the SFU and its operator could see streams.
- There is no recording code path and there are no hidden viewers; admins also show up in "Watching now".
- **What the server stores**: usernames and argon2id hashes; sessions and devices (token hashes, name, last seen, last
  IP); invites, rooms and push subscriptions; an audit log of logins and admin actions with IPs, pruned after 30 days.
  HTTP access logs are off. Nothing that contains tokens or SDP is logged.
- **What never leaves the device**: window titles, app lists, the exclusion list and "What friends hear". Share labels
  default to "Screen" or "Window".
- **No telemetry.** The server never phones home except the optional release check. Desktop apps check GitHub Releases
  for updates, and that can be turned off.

## Versioning and compatibility

- Server, web and desktop are released **together from one SemVer tag** (0.x until v1.0). Future mobile apps will
  follow the same tag.
- The **protocol** has its own integer version:
  - `hello` sends `{protocol, minProtocol, features[], client}`; the reply is
    `{protocol, serverVersion, features[], limits, iceServers, minClientVersion}`.
  - Changes within a protocol version are additive; unknown fields are ignored.
  - The server and all apps speak N and N−1.
- REST lives under `/api/v1`, and `GET /api/v1/info` needs no auth. Errors carry stable `code`s.
- **On a version mismatch**, the app shows a friendly screen ("Ask your admin to update" or "Update the app") with an
  "Open in browser" button; the server-served SPA always matches the server. Admins can raise `min_client_version`.
- CI replays golden N−1 fixtures and runs the current client core against the last two server releases.
- Docker tags: `:1`, `:1.4`, `:latest`. Release notes flag migrations and protocol bumps.

## Install and distribution

**Server, shell** (systemd Linux, amd64/arm64): `curl -fsSL https://<project-site>/install.sh | sh`
- POSIX sh wrapped in `main()`. It detects OS and architecture and downloads the release tarball.
- **Always verifies** `checksums.txt.sig` with `ssh-keygen -Y verify` against a public key embedded in the script, and
  fails closed. cosign or `gh attestation` add an extra check when installed.
- Installs `/usr/local/bin/isshoni`, creates an `isshoni` user, writes `/etc/isshoni/isshoni.toml` (only if missing) and
  creates `/var/lib/isshoni`.
- Installs a hardened systemd unit (`CAP_NET_BIND_SERVICE`, `ProtectSystem=strict`, `NoNewPrivileges`,
  `StateDirectory`) and sets UDP-buffer sysctls.
- Runs the domain/IP question, firewall offer and certificate wait, then prints the setup URL plus QR code.
- Idempotent: re-running upgrades. Supports `ISSHONI_VERSION` pinning, `--uninstall` (keeps data) and `--purge`.

**Server, Docker**: `ghcr.io/<owner>/isshoni`, multi-arch, `FROM gcr.io/distroless/static-debian13:nonroot`,
`HEALTHCHECK ["/isshoni","healthcheck"]`.
- `compose.yaml` defaults to **bridge networking**. Docker then lets non-root bind 443, and the file publishes 80/tcp,
  443/tcp and 7882/udp+tcp. The public IP comes from STUN or `ISSHONI_PUBLIC_IP`. The data volume is mandatory, and
  doctor fails if data would be written inside the container.
- A documented **host-network alternative** uses `user: "0:0"`, `cap_drop: [ALL]` and `cap_add: [NET_BIND_SERVICE]`.
- The docs cover the host sysctls for UDP buffers, and that published ports bypass ufw.

**Desktop** (every isshoni server's `/download` page links to the matching version, and serves
`install-desktop.sh`/`.ps1` pre-filled with its own URL):
- **Windows**:
  - NSIS installer plus `irm https://<server>/install-desktop.ps1 | iex`.
  - The first 0.x releases are unsigned; the docs explain SmartScreen's "More info → Run anyway".
  - Apply to **SignPath** right after the first public release. That needs the project site with a code-signing policy
    and privacy page. FFmpeg DLLs ship unsigned and unmodified.
- **macOS (no paid account)**:
  - **One self-signed code-signing certificate that never changes** (20–30 years, kept in a protected CI environment).
    The explicit designated requirement is `identifier "io.isshoni.desktop" and certificate leaf = H"…"`. Upgrade path:
    an offline root CA with the requirement anchored on the root.
  - This keeps TCC grants across updates.
  - No sandbox and no `com.apple.developer.*` entitlements. Native code is linked statically, so library validation is
    not an issue. Hardened runtime is on, with the `audio-input` entitlement.
  - `NSLocalNetworkUsageDescription` is set: macOS 15+ asks before connecting to LAN servers and ICE candidates, and
    that grant also stays stable because of the fixed identity.
  - **Primary install**: `curl -fsSL https://<server>/install-desktop.sh | sh`. curl sets no quarantine flag, so there is
    no Gatekeeper dialog. The script checks the pinned sha256 plus the minisign/Ed25519 signature.
  - **Also**: an own Homebrew tap (postflight strips quarantine), and a DMG with an "Open Anyway" guide.
  - **Updates**: the in-app updater downloads with Go's `net/http` (no quarantine), verifies the Ed25519 signature and
    `codesign -R` against the designated requirement, then swaps the .app atomically.
- **Linux agent**: `.deb`/`.rpm` plus `install-desktop.sh`. AUR and AppImage come later. Flatpak comes later because
  audio capture is riskiest inside its sandbox.
- **Mobile apps**: pending (see Mobile).

**Update signing**:
- The Ed25519 key lives in a GitHub Environment that requires the owner's approval, on protected `v*` tags.
- Apps embed an active key and an offline backup key.
- The manifest contains `{version, channel, min_version, expires_at}`. Apps refuse downgrades and expired manifests.
- Updates come only from GitHub Releases, never from the connected server, and update checks can be turned off.

## Build, CI and release (GitHub Actions)

- **PR checks**:
  - Go: `golangci-lint`, `go test -race`.
  - Web: `pnpm lint typecheck test`.
  - **Playwright e2e** (Google Chrome): a "Tone" tab is captured via `--auto-select-tab-capture-source-by-title`; the
    viewer tab asserts `framesDecoded > 0` and audio energy.
  - Protocol: tygo drift check plus golden N−1 fixtures.
  - Native compile jobs: windows-latest (MSVC DLL), macos-15 (cgo), `ubuntu:22.04` container (agent).
  - **Linux audio-exclusion harness as a required gate**: ubuntu-22.04 with a Pulse null sink, and ubuntu-24.04 with
    pipewire, wireplumber and pipewire-pulse under dbus.
  - **License gate**: go-licenses and `pnpm licenses` fail the build on GPL/AGPL.
- **Release on a `v*` tag**:
  - goreleaser OSS: server binaries (linux/darwin/windows × amd64/arm64), deb/rpm, multi-arch `dockers_v2` images,
    checksums plus `ssh-keygen` signature, SBOM, cosign v3, provenance attestation.
  - Desktop matrix: Windows NSIS plus DLLs; macOS universal .app signed with the self-signed identity; Linux agent
    deb/rpm.
  - A protected job signs the update manifest.
- **Third-party licenses**:
  - FFmpeg is built from a pinned n9.0.x tag in its own workflow (MSVC, `--disable-gpl --disable-nonfree
    --enable-shared --disable-programs --disable-everything`, plus only the nvenc/amf/qsv/mf H.264 encoders and libvpl).
  - Every Release ships the DLLs unmodified with the LGPL text, the configure line and the source tarball.
  - `THIRD_PARTY_NOTICES` is generated per artifact (go-licenses, pnpm, and a hand-kept native list) and installed with
    each app.
  - OpenH264 is downloaded only after the user sees Cisco's notice, and never ships in installers.
  - x264 is allowed only in local dev builds.

## Verification

- **Go unit tests**:
  - store/auth (argon2, invites, device flow, token rotation);
  - protocol round-trips plus golden fixtures;
  - the SFU munger (seq/ts continuity across layer switches, SR offset translation);
  - PLI throttle, packet-cache sizing, guards.
- **Go integration**: an in-process server, fake-engine publishers and Pion subscribers. Assert:
  - media arrives, and `subscribe.update high↔low` switches on a keyframe;
  - audio-follows-focus;
  - **A/V offset under 45 ms** (flash plus beep);
  - reconnect cases: WebSocket killed mid-share, UDP black-holed for 5 s, server restart;
  - no goroutine leaks (goleak).
- **Load test**: `isshoni-loadtest` on a second VM, 10 publishers × 10 subscribers, each watching 1 focus plus 8
  thumbnails plus focused audio (expected ≈105 Mbps). Pass: CPU under 50% of 4 vCPU, loss under 0.5%, no PLI storms,
  under 1 MB RSS per DownTrack. A separate all-high stress run.
- **Audio-exclusion harness** (`isshoni-audiotest`, same C ABI):
  - Per-OS fixtures: `fakevoice.exe`, `com.isshoni.test.fakevoice.app` and a Linux binary playing 440 Hz, plus a
    1 kHz "game".
  - **Pass**: 1 kHz present and 440 Hz at least 40 dB lower.
  - Cases: helper process trees, helpers that start late, a Steam-launched game, an isshoni viewer tab, and a browser
    that is excluded.
  - Runs on CI for Linux. Windows and macOS run on self-hosted runners (the Windows PC; the Mac with a harness .app
    signed with the same identity and granted once).
- **Deploy tests**:
  - install.sh on fresh Debian 12/13, Ubuntu 22.04/24.04 and Fedora VMs: idempotent re-run, upgrade from N−1 with
    seeded data, downgrade refusal, restore from backup.
  - Docker bridge and host modes both bind 443.
  - doctor output.
- **Release tiers**:
  - Every release: the audiotest plus a smoke test per shipped OS (Win11, latest macOS, Ubuntu 24.04), with the Discord
    app and a browser viewer.
  - Minor releases: the full matrix. That covers Win10 22H2/Win11, macOS 14.4/15/26/27, Ubuntu 22.04/24.04 and Mint
    X11; Discord stable/PTB/Canary/Web and TeamSpeak/Mumble; iOS Safari PWA and Android Chrome viewers.
  - Community "test report" issue template for the GPU and voice-app long tail.

## Roadmap (solo developer; the estimates are rough)

**M0: Spikes (≈5–7 weeks, in sequence).** Each spike lives in `spikes/`, has written pass/fail results, and targets the
draft C ABI.
- **S4, SFU** (browser → Pion → browser). **Status 2026-09-28: passed for Chrome and 10×10; Firefox/Safari viewer
  check is manual and pending.** Results and M1 design changes are in `spikes/s4-sfu/README.md`: a DTLS-ready gate
  for DownTracks, `ReadSimulcastRTCP`, Firefox decodes only CB/Baseline, Chrome on macOS needs High for hardware
  simulcast encode, and macOS Local Network privacy affects LAN ICE. The original criteria were:
  - two layers; switches within one keyframe interval; zero decode errors in Chrome, Firefox, Safari and iOS Safari;
  - settles whether Firefox and Safari decode High or need Constrained Baseline;
  - 10×10 publishers × subscribers in-process.
- **S1, Windows mixer** (CLI → WAV + FFT) on **physical Win10 22H2 and Win11**:
  - Discord, TeamSpeak and a Chrome profile are excluded at ≥40 dB;
  - streams are added and removed without clicks.
- **S2, macOS**. **Status 2026-09-29: audio passed on macOS 27** (12/12 scenarios, crossfade for audible changes,
  refined mic detection). **The TCC grant survived 5 rebuilds** signed with one self-signed identity (no prompt).
  Still open: macOS 14.4/15/26, picker/window exclusion, and DMG/curl install. See `spikes/s2-mac-audio/README.md`
  and `spikes/s2-mac-vt/README.md`.
  - tap exclusion covering Discord helpers, helpers that start late, and the WKWebView GPU process matched by
    responsible PID;
  - picker and filter exclusion of isshoni's own windows;
  - **the self-signed build N → N+1 update keeps the capture grants on 14.4, 15, 26 and 27**, including the Local Network
    grant on 15+;
  - the DMG + "Open Anyway" path works for a self-signed app, and a curl install launches without a prompt;
  - VideoToolbox 1440p60 encodes in ≤16 ms per frame.
- **Gates**:
  - If S4 misses, adopt the LiveKit sidecar before M1.
  - If S1 fails on Win10, Win10 gets endpoint loopback only (and the owner is told).
  - If S2 persistence fails, the paid-Apple question goes back to the owner.

**M1: "Watch together", server + web (≈8 weeks)**
- Accounts, invites and approval; the Lounge room; signaling with reconnect; the SFU with 2 layers and
  audio-follows-focus.
- Web viewer everywhere, PWA and Web Push; Chrome/Edge sharer.
- TLS auto/ip/manual/off; ICE-TCP on 443.
- install.sh and compose.yaml; setup wizard and connection test; doctor; ops endpoints.
- Project site with install scripts, privacy and code-signing policy.
- *Exit*: 5 friends finish a 2-hour session on a small VPS with no manual fixes; iPhone and Android can watch.

**M2: Windows app (≈10 weeks)**
- Wails shell; client core on the fake engine (macOS and Linux compile in CI).
- Windows engine: WGC/DD, libavcodec, include-set mixer, Opus.
- Device-flow linking, share sheet, first-run wizard, share safety.
- NSIS installer, updater, first 0.x release, then apply to SignPath.
- *Exit*: Discord is excluded (audiotest passes on Win10 and Win11); glass-to-glass latency under 200 ms at 1080p60.

**M3: macOS app (≈8 weeks)**
- Picker, VideoToolbox, taps with all robustness rules.
- Self-signed pipeline, curl installer, Homebrew tap, updater.
- *Exit*: the same checks on 14.4 through 27.

**M4: Linux agent (≈6 weeks)**
- Portal/X11 GStreamer shim, Pulse-protocol exclusion, agent relay, isshoni browser window, deb/rpm.
- *Exit*: Ubuntu 22.04 and 24.04 on the Windows PC booted from USB.

**M5: Polish (≈5 weeks)**
- Direct AMF/MF bitrate control, 720p30 middle layer, BWE improvements, preset tuning, "Copy diagnostics", accessibility
  pass, docs.

**M6: Hardening → v1.0 (≈4 weeks)**
- Load and soak tests, security review and threat model, contributor docs.

Total: roughly a year of solo work to v1.0. M1 is already usable by friends; each later milestone adds a platform.

**Pending (starts when the owner registers as a developer)**
- Native Android app: Capacitor viewer, then the Kotlin sharer on the gomobile core (≈8 weeks). Needs Google developer
  verification.
- Native iOS app: a device spike, then an iOS 27+ SCK sharer (≈8 weeks). Needs the Apple Developer Program. The same
  membership moves macOS to Developer ID plus notarization; this is only a certificate change and resets TCC grants
  once.

**Later** (no order):
- Stream a local file (the extended watch-together).
- WHIP ingest for OBS.
- Optional E2EE (LiveKit-style frame format).
- AV1/HEVC/VP9; HDR; multiple shares per user in the UI.
- Embedded TURN.
- Native PipeWire patchbay.
- Linux in-app viewer (WebCodecs, or WebKitGTK ≥2.56 once WebRTC is on).
- Discord Web separate-window launcher; room locks.
- OIDC login; P2P 1:1 mode; multi-node; Flatpak; signed apt/rpm repositories; more UI languages.

## Main risks

| Risk | Mitigation |
|---|---|
| Windows process-tree semantics (orphans, PID reuse, brokers) let voice audio leak through | S1 on physical Win10/11; ancestor rules with launcher stops; mic detection; "What friends hear" |
| macOS: taps silently output zeros; helpers start late; WKWebView audio in XPC processes | Rebuild triggers; process-list listener; `bundleIDs` on 26+; sound check in onboarding; S2 (late helpers and WKWebView verified on 27) |
| Apple tightens Gatekeeper for self-signed apps (macOS 28?) | curl and Homebrew paths don't depend on the first-launch check; Chrome web sharer as fallback; signing is configurable so switching to Developer ID is only a certificate change |
| Self-signed or update key leaks, and signed malware inherits users' capture grants | Protected CI environment with owner approval; an offline backup key; published fingerprints; planned move to an offline root CA |
| Custom SFU downlink adaptation is the biggest engineering cost | Explicit layers first, then REMB/loss, then TWCC; LiveKit sidecar as Plan B (S4 gate) |
| Self-hosters underestimate bandwidth | Preview layers, audio-follows-focus, bandwidth calculator, VPS-first docs, transfer alerts |
| Wails v3 is still beta | Pin the exact version; Linux doesn't use Wails at all; windows are kept simple (no multi-window or tray-heavy UI) |
| Friends expect to share from phones before the native apps exist | The web-app viewer works on every phone from M1; the mobile designs are ready in the Pending section; the go-ahead is tied to developer registration |
| One solo developer across 3 desktop platforms plus web | Shared core and fake engine; staggered milestones, each shippable; community test reports for hardware coverage |
| H.264 patents (US until ~2030) and GPL creep | Hardware/OS encoders; OpenH264 downloaded at runtime only; license gate in CI |
