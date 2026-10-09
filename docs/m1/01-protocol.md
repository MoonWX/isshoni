# M1 design 01: Signaling protocol v1 and `internal/protocol`

Part of the M1 ("Watch together", server + web) design set. This doc owns:
- the signaling protocol, version 1 (WebSocket at `/ws`);
- `internal/protocol` (message types, the single source of truth; generated into TypeScript);
- `internal/server/signal` (the hub that speaks the protocol: connections, participants, rooms, shares, resume);
- `internal/client/signal` (the Go signaling client, used by tests and the load test in M1, by native apps from M2);
- `internal/server/sfuplane` (the thin adapter that implements `signal.MediaPlane` on top of 02's `*sfu.SFU`, §15.4);
- `web/src/protocol/` (generated types plus the TypeScript `SignalClient`).

Integration status: reconciled with 02–06 by the integrator (see [`README.md`](README.md), "Integration decisions").

Other M1 docs, referenced by file name and not repeated here:
[`01-protocol.md`](01-protocol.md) (this doc), [`02-sfu.md`](02-sfu.md) (SFU, load test),
[`03-accounts-and-store.md`](03-accounts-and-store.md) (SQLite, auth, sessions, invites, REST),
[`04-server-platform.md`](04-server-platform.md) (binary, config, TLS, 443 mux, logging, ops, Web Push),
[`05-web-client.md`](05-web-client.md) (SPA), [`06-deploy-and-ci.md`](06-deploy-and-ci.md) (install, Docker, CI, release).

Every item is marked **M1** or **later (Mx)**. "Later" items only reserve names and shapes now, so M2 (Windows app),
M3 (macOS app) and M4 (Linux agent) need no breaking change.

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | One WebSocket per client instance (browser tab, app process) at `GET /ws`. JSON text frames, one message per frame. No subprotocol, no compression | Version lives in `hello`, so the path never changes. Messages are small; compression would cost memory per socket for nothing |
| D2 | Auth is resolved during the handshake: the session cookie on the upgrade request (web), or `hello.auth` with a bearer token (native apps, M2+). Both paths exist in v1 | Browsers can't set headers on a WebSocket, and a webview SPA on a `wails://` origin gets no cookie. One path for all clients |
| D3 | Envelope `{type, id, re, data}`. The protocol version is fixed per connection by `hello`/`welcome`, not repeated per message | A per-message version could disagree with the negotiated one; the negotiated version is the only one that matters |
| D4 | Every request gets exactly one reply: `ok` or `error` (`welcome` for `hello`). Everything else is a notification | Simple request/response typing in Go and TS; no ambiguous "maybe a reply" messages |
| D5 | Reconnect is **state-based resync**, not message replay. Messages are either idempotent state (snapshots, desired subscriptions, statuses, hints) or negotiation steps guarded by `(gen, neg)` counters | No buffers to size, and any lost message is repaired by the next state exchange |
| D6 | Room state goes out as a full `room.state` snapshot (coalesced, at most one per 200 ms per room) plus discrete `room.event`s for toasts and `aria-live` | Rooms are small (friend groups); snapshots can't drift. Events avoid fake "joined" toasts after a reconnect |
| D7 | Subscriptions are explicit desired state from the client (`subscribe.update`). Audio-follows-focus is a client policy expressed through it; the server simply doesn't forward audio that is `off` | Server stays policy-free about focus; muted shares cost zero audio bandwidth |
| D8 | Each PeerConnection has a fixed offerer: **pub** is offered by the client, **sub** by the server. `gen` counts PC rebuilds, `neg` counts offers within a gen. Offers carry an explicit `mid → share` map | No glare; stale messages are recognizable; no reliance on browser-specific msid behavior |
| D9 | Clients report codec capabilities (`caps`) in `hello` and `caps.update`. A subscription that can't be decoded reports `reason: codec` and is retried by the server, which rebuilds the sub PC (02 §8.5) | S4: a fresh Firefox can't decode H.264 until OpenH264 has downloaded |
| D10 | Errors carry a stable snake_case `code`, a `scope` and `retryable`, never English text. Clients handle unknown codes generically by scope and `retryable` | i18n (plan), and older clients survive new codes |
| D11 | Read limit 64 KiB for every message, except `pc.offer`/`pc.answer` after a successful `hello`, which may be up to 256 KiB | SDP grows with the number of watched shares: about 3.3 KB per share in a Chrome answer, so 64 KiB would cap a viewer at about 19 shares. See §13 and open question 1 |
| D12 | `internal/protocol` is stdlib-only Go. TS types come from tygo (union enums) plus a small registry generator; CI fails on drift. Golden fixtures in `internal/protocol/testdata/v1`, frozen copies per release | One source of truth; N−1 compatibility is tested, not hoped for |

## 2. Scope

**M1**: everything in this doc not marked later: the web client (`kind: web`, roles `full` and `viewer`), the in-process test
client and the load test (`kind: tool`), cookie auth, the bearer field in `hello` (parsed and shape-checked; the M1
wiring's `AuthenticateBearer` always returns `ErrInvalid`, so a bearer `hello` gets `unauthenticated`; 03's
`AuthenticateBearerToken` backs it from M2), rooms, shares, subscriptions, negotiation, reconnect, errors, stats,
notifications, and the same-user relay transport (`agent.send`/`agent.recv`, small and cuttable, see slice P12).

**Later**: `publisher`/`agent` roles in use (M2, M4), `user.connections` (M2), share pause (M2), relay payload
schemas (`share.request` etc., M2/M4), a middle layer (M5), admin kick with a rejoin block (the `kicked` codes are
reserved; M1 admins disable an account instead), TURN credential refresh (Later), E2EE (Later).

**Not in this doc**: SFU internals and codec policy (02), REST API and the account model (03), TLS and the 443 mux,
config loading, Web Push delivery, the admin dashboard endpoint (04), UI (05). The browser connection test in the
setup wizard is **not** a signaling feature; it uses a REST endpoint owned by 04/05 (see §21).

## 3. Transport

### 3.1 Endpoint and upgrade (M1)

`GET /ws` with `Upgrade: websocket`, served by the main HTTPS handler (the TLS side of the 443 mux, or the plain
listener in `tls.mode=off`). URL for the SPA: `new URL('/ws', location.href)` with `wss:` (or `ws:` in `task dev`).

The upgrade handler (`signal.Hub.ServeHTTP`) runs these checks in order. HTTP status codes are only visible to
non-browser clients; browsers see a failed socket and fall back to backoff (§10.2). That is why step 5 answers
in-band.

| Step | Check | On failure |
|---|---|---|
| 1 | Hub is not shutting down | `503`, `Retry-After: 2` |
| 2 | `Origin` header absent (non-browser client), or exactly equal (scheme, host case-insensitive, port with defaults normalized) to `Config.PublicOrigin` (04's `Site.Origin`) or an entry of `Config.AllowedOrigins`. `AllowedOrigins` is empty in M1 and is not a config key; M2 adds the Wails asset origins in code, accepted only for bearer connections | `403` |
| 3 | `Authenticator.AuthenticateRequest(r)` on every upgrade (the hub never parses cookie names; 03 owns them). Success → the connection is **cookie-authenticated**. `ErrNoCredentials` or `ErrInvalid` → **pre-auth** | any other error: `503`, `Retry-After: 2` |
| 4 | Pre-auth only: ≤ 20 pre-auth upgrades per client IP (IPv4 address; IPv6 keyed by its /64, as in 03 §7.3) per minute (plan guard; 04 key `limits.ws_handshakes_per_ip_per_minute`), ≤ 500 concurrent pre-auth sockets server-wide | `429` with `Retry-After`, or `503` |
| 5 | Cookie-authenticated only: the user has < 16 connections (constant; detached connections and sockets still in the handshake count). Bearer connections are checked at `hello` (`too_many_connections`) | accept (so browsers can show the notice), then wait for `hello`: a resume needs no slot, because the connection it resumes has its own, and the resume token only arrives with `hello`. A `hello` that resumes a connection goes on; any other is answered `error{too_many_connections, scope: connection}` and closed `4429`. At most 16 sockets per user wait at the cap like this; one more gets the error right after accept |
| 6 | `websocket.Accept` with `CompressionMode: CompressionDisabled`, no subprotocols, and `InsecureSkipVerify: true`: step 2 already did the exact Origin check (coder/websocket's own check compares Origin with `r.Host`, which breaks behind proxies that rewrite `Host`) | — |

The hub owns every upgrade-time check for `/ws` (Origin, pre-auth limits, per-user cap). 03's auth only validates
credentials (`Authenticator`, below); it has no WebSocket-specific code.

After accept: read limit 64 KiB; the first message must be `hello` within **10 s**, else `error{hello_timeout}` and
close `4408`.

Client IP is `Deps.ClientIP(r)` from 04 (honors `X-Forwarded-For` only from trusted proxy CIDRs in `tls.mode=off`).

### 3.2 Authentication (M1)

| Client | Credential | Where | Notes |
|---|---|---|---|
| Web SPA (browser, PWA) | HttpOnly session cookie (03) | Upgrade request | Origin must be allowlisted (step 2). CSRF protection for the socket is the Origin check plus `SameSite=Lax` |
| Load test, Go tests (`kind: tool`) | Session cookie from `POST /api/v1/auth/login` (03), sent as a `Cookie` header | Upgrade request | No `Origin` header; not counted as pre-auth, so the load test isn't throttled |
| Desktop app Go connection, desktop SPA in webview, Linux agent (M2+) | Bearer access token from the device flow (03) | `hello.auth` | The field and its shape check exist in v1. If the upgrade already carried a valid cookie, `hello.auth` must be absent (`bad_request`). Native clients never put the token in a header or a subprotocol: `hello.auth` is the only bearer path (03 follows this). M1: always `unauthenticated`. M2: the app refreshes its access token before every `hello`. On `unauthenticated` it refreshes once and retries, and relinks only if that also fails (there is no `invalid_token` on the WebSocket) |

Rules:
- The identity (`signal.Identity`: user, session or device) is fixed for the connection's lifetime.
- A bearer token is checked once, at `hello`. The connection then stays valid until its device is revoked, not
  until the access token expires. A resume or new connection needs a fresh token. This avoids in-band token refresh.
- Revocation is pushed: 03's revocation paths (logout, password change or reset, account disabled or deleted, device
  revoked) call `Hub.CloseConnections(sel, code)` through the wiring adapter for 03's `auth.ConnCloser` (04 §6.6). The
  hub sends `error{session_revoked|account_disabled, scope: session}` and closes (`4401`/`4403`).
- The hub re-validates every connection at connect and every **5 min** with `Authenticator.Revalidate` (03 `Touch`,
  which also refreshes last-seen, so a long session never idles out). Only `ErrInvalid` (session or device gone or
  expired, or user not active) closes the connection, with `session_revoked`/4401. Any other error is transient: the
  connection stays, the hub logs WARN and retries at the next tick. Session expiry is not pushed by 03; this check
  finds it.
  - A `Revalidate` result with a changed `Name` or `Admin` is applied like `UpdateUser` (identity and `room.state`
    refreshed). This is how admin-CLI role changes reach open connections.
- A resume token (§10.3) is never a credential. The connection must authenticate as the same user and the same
  session or device as the connection it resumes (§10.3).

### 3.3 Frames and limits (M1)

- Text frames only; each frame is one JSON object (§5). A binary frame closes the socket with `1003`.
- Read limit (coder/websocket `SetReadLimit`, changeable at any time):
  - **64 KiB** until `welcome` is sent;
  - then **256 KiB**. Any message other than `pc.offer`/`pc.answer` larger than 64 KiB gets
    `error{message_too_large, scope: request}` and is dropped.
  - Over the socket limit, coder/websocket closes with `1009`.
- Compression: off (`CompressionDisabled`, also coder/websocket's default).
- Writes: one writer goroutine per socket, 10 s write timeout per message. The send queue holds at most 512 messages or
  8 MiB; when it overflows, the hub sends nothing more and closes with `4503` (`slow_connection`). The connection then
  enters grace like any other drop. (An overflow of the connection actor's inbox also closes with `4503`, but ends
  the connection for good: §15.2.)

### 3.4 Heartbeat (M1)

- Browsers can't send or observe WebSocket ping frames, so client-side liveness is an application-level
  `ping`/`pong`. Browsers do answer the server's ping frames automatically, even in hidden tabs whose timers are
  throttled (Chrome wakes chained timers once a minute after 5 min hidden).
- The client sends `ping` every `limits.pingIntervalMs` (15 s). With no `pong` within 10 s, it closes the socket
  itself and reconnects (§10.2).
- The client also sends an immediate `ping` (3 s timeout) when:
  - any of its PCs becomes `disconnected` (the PC code calls `SignalClient.probe()`, §16);
  - the page becomes visible;
  - the browser fires `online`.
- The hub sends a WebSocket ping frame every 15 s (`Conn.Ping` with a 10 s timeout) from a per-socket goroutine. A
  missed pong does not close the socket by itself; the idle rule below does.
- The server closes a socket from which no frame (any message, or a pong frame) arrived for `limits.idleTimeoutMs`
  (45 s): `error{idle_timeout}`, close `4408`. The connection then enters grace.

## 4. Model

### 4.1 Entities (M1)

| Entity | Identity | Lives in | Lifetime |
|---|---|---|---|
| **User** | `userId` (03: 12 lowercase Crockford base32 chars; the protocol accepts any opaque string ≤ 64 chars `[A-Za-z0-9_-]`) | SQLite (03) | Account lifetime |
| **Room** | `roomId` (03, same format). Lounge has the fixed id `lounge` and is created on first run | SQLite (03); in memory while anyone is in it | Until deleted by an admin |
| **Connection** | `connectionId` = `c_` + 16 lowercase base32 chars (80 random bits) | Hub memory | From `welcome` until closed. **Survives WebSocket reconnects** through resume within the 30 s grace |
| **Participant** | `(roomId, userId)` | Hub memory | While at least one of the user's connections is in the room (grace included) |
| **Share** | `shareId` = `s_` + 16 base32 chars | Hub memory, media in the SFU | From `share.start` until ended (§4.4) |
| **Subscription** | `(connectionId, shareId)` | Hub (desired state) and SFU (DownTracks) | Until the share ends or the connection leaves the room |

Rules:
- A connection is in **at most one room** at a time. `room.join` while in a room first leaves it, which ends that
  connection's shares there.
- A Participant merges all connections of one user in one room: a browser tab plus a phone, or (M2) the desktop app's Go
  `publisher` connection plus its SPA `viewer` connection. Presence shows one person, and share limits count per user.
- A share is published through exactly one connection (the one whose pub PC carries its media) but belongs to the
  user: any connection of the same user may `share.update` or `share.stop` it (web → desktop handoff in M2).
- A user can be in several rooms at once through different connections; each (room, user) is a separate Participant.
- Share ids are random rather than sequential, so a stale id from before a server restart never matches a new share.
- **Watchers**: for each share, the room's participants other than its owner that have at least one connection with a
  subscription where `video != off` or `audio == on`. Per participant, `video` is the maximum over its connections
  (`high > low > off`) and `audio` is `on` if any connection has it on. Watchers are computed from the
  **desired** state, so "N watching" may over-count a viewer who is still connecting but never under-counts
  (no hidden viewers).

### 4.2 Connection state (server, M1)

```
            hello ok                    socket lost / idle / slow        grace (30 s) expired
handshaking ────────► ready ◄──────────────────────────────► detached ─────────────────────────► closed
     │                  │   resume (hello with its token)          │
     │ timeout/error    │ close 1000/1001 from client, room kick,   │ 03 revocation
     └──────────────────┴─ revocation, fatal error ─────────────────┴──────────────────────────► closed
```

- `detached`: the socket is gone, but PCs, shares and subscriptions stay. Media keeps flowing if the PCs are healthy
  (a WebSocket drop is not a media drop).
- Close codes `1000` and `1001` **from the client** mean it left on purpose (logout, page unload); the web client
  sends 1000, and 1001 comes from browsers' automatic close on unload and from Go clients. The hub skips grace and
  closes immediately, so a reloaded sharer's frozen share disappears at once.
- What the end of its socket means for a connection:
  - a client close with `1000` or `1001`: closed at once, `EndReason left` (above). Any other client close code,
    the reconnect code `4000` included, keeps the grace;
  - the "fatal error" of the diagram: the hub closed the socket with `4400` (`bad_message`, a second `hello`) or
    `1003` (a binary frame). The client stops on these (§12.2), so nothing would resume the connection: closed at
    once, `disconnected`;
  - a revocation (`4401`, `4403`): closed, `left`; a shutdown (`1012`): closed, `server_shutdown`;
  - the connection's inbox overflowed (§15.2): closed at once, `disconnected`;
  - everything else keeps the grace: the hub's retryable closes (`4408` idle timeout, `4503` send queue full, `4429`
    flood, `1011`), a message over the read limit (`1009`) and a socket that just went away (`1006`).
  - **A flood close keeps the 30 s grace** (decided after group 4). `error{rate_limited, scope: connection}` with
    `4429` ends the socket, not the connection: it stays `detached` with its shares and subscriptions, and its media
    keeps flowing, like after any other retryable close. The rate limits apply again after the resume: a resumed
    connection keeps its rate-limit buckets (§10.3) and, with them, the start of its flood (the 10 s of §12.1), so
    a resume forgives nothing. The buckets refill while the socket is gone, and the flood is over once a message
    passes with both global buckets at least half full. A client that resumes at once and still floods is closed
    with `4429` again at its first refused message. A client that follows §10.2 waits at least 30 s after `4429`,
    which is the whole grace, so its `hello` normally opens a new connection (`resumed: false`) and it rejoins.
- `closed`: subscriptions removed, own shares ended (`disconnected` or the specific reason), MediaPeer closed,
  participant removed if it was the last connection (`room.event participant.left`). A connection closed by 03
  revocation (`CloseConnections`) skips grace and uses `EndReason left` for its shares and, if it was the last
  connection, for `participant.left`.

### 4.3 Participant state (M1)

`present` (at least one connection `ready`) ↔ `reconnecting` (all its connections `detached`) → removed. No
`participant.left` event is sent for a participant that recovers within grace.

### 4.4 Share state (M1; `paused` later M2)

```
share.start ok                first video keyframe (SFU)
────────────► starting ──────────────────────────────► live ◄──────────► stalled
                 │ 30 s without media                   │   pub PC not       │ 30 s stalled
                 ▼                                      │   connected ≥ 2 s  ▼
               ended ◄──────────────────────────────────┴────────────────── ended
                 (share.stop · pc.close · connection closed · room left · kick · room closed · shutdown)
```

- **Ownership**: the hub owns the share lifecycle and both 30 s timeouts. The SFU only reports media facts (first
  keyframe, pub PC not connected, tracks re-bound) through `MediaSink.ShareMedia`; it never ends a share on its own
  (02 §5.3). `stalled → live` happens on the first keyframe after the pub PC is connected again, with or without a
  new offer (02 §5.3).
- `starting` shares appear in `room.state`, so the owner's other devices know. Viewers render no tile for them and
  subscribe only once they are `live`.
- `stalled` means the publisher's pub PC is not connected (disconnected, failed or being rebuilt). It is media-agnostic
  on purpose: a static screen can legitimately send almost nothing. Viewers keep the last frame with a "connection
  unstable" overlay.
- When a share's pub PC is rebuilt (new `gen`), the SFU re-binds the new tracks to the same `shareId`. Viewers keep
  their subscriptions and see a keyframe-gated switch, with no renegotiation on their side (02).
- `ended` removes the share from `room.state` and emits `room.event share.stopped` with an `EndReason`.

## 5. Envelope and JSON conventions (M1)

```json
{"type": "share.start", "id": "7", "data": {"kind": "screen", "preset": "auto", "audio": true, "ref": "k2j9"}}
{"type": "ok", "re": "7", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "codec": "h264/6400", "encodings": [], "audioBitrate": 128000}}
{"type": "room.event", "data": {"kind": "share.started", "roomId": "lounge", "userId": "k3m9p2qxw7ht"}}
```

| Field | Type | Rule |
|---|---|---|
| `type` | string, required | Dotted lowercase `noun.verb` (`room.join`), or a bare word for protocol-level messages (`hello`, `welcome`, `ok`, `error`, `ping`, `pong`, `stats`, `invalidate`) |
| `id` | string, requests only | Chosen by the requester. 1–32 chars `[A-Za-z0-9_-]`, unique among that side's outstanding requests on the socket. A decimal counter is fine |
| `re` | string, replies only | The `id` of the request this answers. Present on `welcome`, `ok` and request-scoped `error` |
| `data` | object, optional | Payload for `type`. Absent means `{}` |

JSON conventions:
- Keys are camelCase. Unknown keys are **ignored** by both sides; never use `DisallowUnknownFields` outside tests.
- IDs are strings, never numbers.
- Units live in the name: bit rates in bits/s, named `…Bitrate` (integers); durations in ms, named `…Ms`; timestamps are
  RFC 3339 UTC strings with exactly three fractional digits and a `Z` suffix (`2026-10-12T19:04:05.123Z`, like
  JavaScript's `Date.toISOString`; the fraction is truncated), named `…At` or `serverTime`. The form has a fixed
  width, so it also sorts as text. 03's REST API sends the same form (03 §3.3, `api.WireTime`). Readers accept any
  RFC 3339 form.
- Enums are lowercase strings (`high`, `screen`); error codes are snake_case.
- Optional fields are omitted when empty (`omitempty`/`omitzero`), and their zero value always means "old behavior".
  Booleans are named so that `false` is the default.
- Arrays in snapshots and replies are always present (`[]`, never `null`). The Go encoder must initialize slices;
  a unit test asserts no `null` appears in any golden fixture or hub output.
- Unknown enum values received from the other side (newer peer):
  - server receiving an unknown value in a request: `bad_request` with `params.field`;
  - client receiving one: the fallback in §8.13.
- Strings from users that other users see (`label`): trimmed; 1–40 Unicode code points; Unicode categories Cc, Cf,
  Zl and Zp rejected (`bad_request`, `params.field: "label"`). Room and user names come from the database (03).
- Text on the wire is never user-facing English. The only human text is user content (`label`, names).

## 6. Handshake, versions, features, roles (M1)

### 6.1 Version negotiation

`internal/protocol` defines `Version = 1` and `MinVersion = 1`. The server speaks `[MinVersion, Version]`: once v2
exists, `MinVersion` stays at `Version − 1` (plan: N and N−1).

The client sends `protocol` (its newest) and `minProtocol` (its oldest). The server picks
`chosen = min(client.protocol, Version)`:
- if `chosen < max(client.minProtocol, MinVersion)`: reply `error{protocol_unsupported, scope: connection}` with
  `params {serverMin, serverMax, serverVersion}` and close `4426`;
- otherwise `welcome.protocol = chosen`, and the whole connection uses it.

`hello`, `welcome`, `error` and the envelope are **frozen across all versions** (additive changes only), so any
client can always read the server's answer.

After `welcome`:
- **Native clients** (M2+): if the own version is below `welcome.minClientVersion`, show the "Update the app" screen
  (plan) and stop. The server also rejects them: `client_outdated`, close `4426`. The floor is 03's setting
  `minClientVersion` (admin UI), which 04's config key `clients.min_version` can pin; the hub reads it through
  `Deps.Policy` at every `hello`.
- **Web client**: if `welcome.serverVersion !== BUILD_VERSION` and neither is a dev build (§16), the SPA is stale (a
  cached PWA shell). It asks the service worker to update and reloads once, guarded by a `sessionStorage` flag to
  avoid loops. The server does not reject a mismatched web build while its protocol is supported.

### 6.2 Features

- A feature is a named optional capability within a protocol version.
- The client lists what it understands (`hello.features`). The server replies with the intersection of that list and
  what it has enabled (`welcome.features`). A feature is active only if it is in `welcome.features`.
- Rule for changes after the first release (0.1.0): a new client→server message type or enum value must be behind a
  feature. New server→client fields and enum values need a defined client fallback instead (§8.13).

| Feature | Meaning | Status |
|---|---|---|
| `agent.relay` | The server relays `agent.send` between connections of the same user | M1 (server), used M2/M4 |
| `user.connections` | Server sends `user.connections` to all of a user's connections | later (M2) |
| `share.pause` | `share.update{paused}` and `ShareStatus paused` | later (M2) |
| `layer.mid` | `VideoLayer mid` (720p30 middle layer, rid `h`) | later (M5) |
| `ice.refresh` | `ice.servers` event with fresh TURN credentials | later (TURN) |

### 6.3 Roles

`hello.role` decides what a connection may do. Unknown roles are rejected with `bad_request`.

| Message (client → server) | `full` | `viewer` | `publisher` | `agent` |
|---|---|---|---|---|
| `room.join`, `room.leave`, `ping`, `caps.update`, `stats`, `stats.watch` | ✓ | ✓ | ✓ | ✓ |
| `share.start`, `share.update`, `share.stop`; `pc.*` for `pub` | ✓ | – | ✓ | ✓ |
| `subscribe.update`; `pc.*` for `sub` | ✓ | ✓ | – | – |
| `agent.send` (feature `agent.relay`) | ✓ | ✓ | ✓ | ✓ |

A disallowed message gets `error{forbidden, scope: request}`.

Role use:
- **M1**: the web SPA uses `full` when `navigator.mediaDevices.getDisplayMedia` exists, otherwise `viewer` (phones).
- **Later (M2)**: the desktop app's Go side uses `publisher`, its SPA `viewer`.
- **Later (M4)**: the Linux agent uses `agent`, which is `publisher` plus accepting relayed commands.

## 7. Message catalog

Kinds: **req** = request (has `id`, gets one reply), **ntf** = notification (no `id`, no reply).

| Type | Dir | Kind | Payload → reply payload | Status |
|---|---|---|---|---|
| `hello` | C→S | req | `Hello` → `welcome` `Welcome` | M1 |
| `welcome` | S→C | reply | `Welcome` | M1 |
| `ok` | S→C | reply | per request (below) | M1 |
| `error` | S→C | reply or ntf | `Error` | M1 |
| `ping` / `pong` | C→S / S→C | ntf | `Ping` / `Pong` | M1 |
| `room.join` | C→S | req | `RoomJoin` → `RoomJoinResult` | M1 |
| `room.leave` | C→S | req | `Empty` → `Empty` | M1 |
| `room.state` | S→C | ntf | `RoomState` | M1 |
| `room.event` | S→C | ntf | `RoomEvent` | M1 |
| `share.start` | C→S | req | `ShareStart` → `ShareParams` | M1 |
| `share.update` | C→S | req | `ShareUpdate` → `ShareParams` | M1 (UI optional) |
| `share.stop` | C→S | req | `ShareStop` → `Empty` | M1 |
| `pc.offer` | C→S (pub), S→C (sub) | ntf | `PCOffer` | M1 |
| `pc.answer` | S→C (pub), C→S (sub) | ntf | `PCAnswer` | M1 |
| `pc.ice` | both | ntf | `PCICE` | M1 |
| `pc.restart` | both | ntf | `PCRestart` | M1 |
| `pc.close` | C→S | ntf | `PCClose` | M1 |
| `subscribe.update` | C→S | req | `SubscribeUpdate` → `SubscribeResult` | M1 |
| `subscribe.status` | S→C | ntf | `SubscribeStatus` | M1 |
| `quality.hint` | S→C | ntf | `QualityHint` | M1 |
| `caps.update` | C→S | ntf | `CapsUpdate` | M1 |
| `stats` | C→S | ntf | `ClientStats` | M1 |
| `stats.watch` | C→S | req | `StatsWatch` → `Empty` | M1 |
| `stats` | S→C | ntf | `ServerStats` (only while watched) | M1 |
| `invalidate` | S→C | ntf | `Invalidate` | M1 |
| `server.shutdown` | S→C | ntf | `ServerShutdown` | M1 |
| `agent.send` | C→S | req | `AgentSend` → `AgentSendResult` | M1 transport (feature `agent.relay`); payloads later (M2/M4) |
| `agent.recv` | S→C | ntf | `AgentRecv` | same |
| `user.connections` | S→C | ntf | `UserConnections` | later (M2) |

`stats` is one message type whose payload depends on direction (`ClientStats` from clients, `ServerStats` from the
server). The registry (§15.1) records both.

Ordering guarantees (server → one connection, FIFO over one socket):
- `ok` for `share.start` is sent before any `room.state` that contains the new share.
- `ok` for `room.join` (and a resumed `welcome`) is followed immediately by a `room.state` for that room, before any
  other room traffic.
- The hub processes one connection's messages strictly in order (one actor per connection, §15.2).

## 8. Messages

All Go types live in package `protocol` (`internal/protocol`). Enum constants use the `<Type><Value>` naming that tygo
needs for union types (§14.4).

### 8.1 Common types

```go
package protocol

const (
	Version    = 1 // newest protocol version this build speaks
	MinVersion = 1 // oldest version this build still speaks (Version-1 once Version >= 2)
)

// Envelope is every WebSocket message.
type Envelope struct {
	Type MessageType     `json:"type"`
	ID   string          `json:"id,omitempty"`
	Re   string          `json:"re,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Empty is the payload of requests and replies without fields.
type Empty struct{}

// Secret is a string that never appears in logs or fmt output; JSON encodes it verbatim.
// String/GoString/Format/LogValue all return "[redacted]"; Reveal returns the value.
type Secret string

// SDP is a session description; LogValue prints only its length ("[sdp 5123 B]").
type SDP string

// CodecKey names a codec as clients report it: "opus", or "h264/<profile_idc><constraint byte>" in lowercase hex,
// e.g. "h264/42e0" (constrained baseline), "h264/4200" (baseline), "h264/4d00" (main), "h264/640c" (constrained
// high), "h264/6400" (high). The level is ignored (S4 finding 5: Chrome advertises High as 640034).
// Declared with single-line consts (no const group) so tygo keeps it a plain string, not a closed union.
type CodecKey string

type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

type PCKind string

const (
	PCKindPub PCKind = "pub" // publish PC: the client offers
	PCKindSub PCKind = "sub" // subscribe PC: the server offers
)

type VideoLayer string

const (
	VideoLayerHigh VideoLayer = "high" // rid "f": full (source, <=1080p60 by default)
	VideoLayerLow  VideoLayer = "low"  // rid "q": preview (360p15)
	VideoLayerOff  VideoLayer = "off"
	// later (M5, feature layer.mid): VideoLayerMid = "mid", rid "h"
)

type AudioState string

const (
	AudioStateOn  AudioState = "on"
	AudioStateOff AudioState = "off"
)

// RIDs used in simulcast for each layer. Fixed for all senders (browser and native).
const (
	RIDHigh = "f"
	RIDLow  = "q"
)
```

### 8.2 `hello` → `welcome` (M1)

```json
{"type": "hello", "id": "1", "data": {
  "protocol": 1,
  "minProtocol": 1,
  "features": [],
  "client": {"kind": "web", "version": "0.1.0", "os": "macos", "browser": "chrome"},
  "role": "full",
  "resumeToken": "r1.AQID…",
  "caps": {
    "decode": ["h264/42e0", "h264/4200", "h264/4d00", "h264/640c", "h264/6400", "opus"],
    "encode": ["h264/42e0", "h264/4200", "h264/4d00", "h264/6400", "opus"],
    "simulcast": true,
    "displayCapture": true
  }
}}
```

A native app (later M2) adds `"auth": {"scheme": "bearer", "token": "…"}` and uses
`"client": {"kind": "desktop", "version": "0.2.0", "os": "windows"}, "role": "publisher"`.

```json
{"type": "welcome", "re": "1", "data": {
  "protocol": 1,
  "serverVersion": "0.1.0",
  "minClientVersion": "0.1.0",
  "features": [],
  "limits": {
    "maxMessageBytes": 65536, "maxSdpBytes": 262144, "maxSharesPerUser": 4,
    "messagesPerSecond": 20, "messageBurst": 100,
    "pingIntervalMs": 15000, "idleTimeoutMs": 45000, "graceMs": 30000
  },
  "iceServers": [],
  "connectionId": "c_k3v9q2m7xw4pa8d1",
  "resumeToken": "r1.BAUG…",
  "resumed": false,
  "defaultRoomId": "lounge",
  "user": {"id": "k3m9p2qxw7ht", "name": "Alex", "admin": true},
  "serverTime": "2026-10-12T19:04:05.123Z"
}}
```

```go
type Hello struct {
	Protocol    int        `json:"protocol"`
	MinProtocol int        `json:"minProtocol"`
	Features    []Feature  `json:"features"`
	Client      ClientInfo `json:"client"`
	Role        Role       `json:"role"`
	ResumeToken Secret     `json:"resumeToken,omitempty"`
	Auth        *HelloAuth `json:"auth,omitempty"` // only when the upgrade carried no valid cookie
	Caps        Caps       `json:"caps"`
}

type HelloAuth struct {
	Scheme AuthScheme `json:"scheme"`
	Token  Secret     `json:"token"`
}

type AuthScheme string

const AuthSchemeBearer AuthScheme = "bearer" // the only scheme in v1 (TS: plain string)

type ClientInfo struct {
	Kind    ClientKind `json:"kind"`
	Version string     `json:"version"`           // SemVer of the client build; web = SPA build = server release
	OS      ClientOS   `json:"os"`
	Browser string     `json:"browser,omitempty"` // chrome|edge|firefox|safari|other; diagnostics only
}

type ClientKind string

const (
	ClientKindWeb     ClientKind = "web"
	ClientKindDesktop ClientKind = "desktop" // later (M2/M3/M4)
	ClientKindMobile  ClientKind = "mobile"  // pending native apps
	ClientKindTool    ClientKind = "tool"    // isshoni-loadtest, tests
)

type ClientOS string

const (
	ClientOSWindows  ClientOS = "windows"
	ClientOSMacOS    ClientOS = "macos"
	ClientOSLinux    ClientOS = "linux"
	ClientOSIOS      ClientOS = "ios"
	ClientOSAndroid  ClientOS = "android"
	ClientOSChromeOS ClientOS = "chromeos"
	ClientOSOther    ClientOS = "other"
)

type Role string

const (
	RoleFull      Role = "full"
	RoleViewer    Role = "viewer"
	RolePublisher Role = "publisher" // later (M2)
	RoleAgent     Role = "agent"     // later (M4)
)

type Feature string

const (
	FeatureAgentRelay      Feature = "agent.relay"
	FeatureUserConnections Feature = "user.connections" // later (M2)
	FeatureSharePause      Feature = "share.pause"      // later (M2)
	FeatureLayerMid        Feature = "layer.mid"        // later (M5)
	FeatureICERefresh      Feature = "ice.refresh"      // later (TURN)
)

// Caps are the client's media capabilities. Web: from RTCRtpReceiver/RTCRtpSender.getCapabilities, H.264 entries with
// packetization-mode=1 only, mapped to CodecKey (web/src/protocol/codecs.ts).
type Caps struct {
	Decode         []CodecKey `json:"decode"`
	Encode         []CodecKey `json:"encode,omitempty"`
	Simulcast      bool       `json:"simulcast,omitempty"`      // can send simulcast
	DisplayCapture bool       `json:"displayCapture,omitempty"` // can share (getDisplayMedia or native capture)
}

type Welcome struct {
	Protocol         int         `json:"protocol"`
	ServerVersion    string      `json:"serverVersion"`
	MinClientVersion string      `json:"minClientVersion"` // "" = no minimum
	Features         []Feature   `json:"features"`
	Limits           Limits      `json:"limits"`
	ICEServers       []ICEServer `json:"iceServers"` // M1: always [] (no STUN/TURN needed: the server has a public address)
	ConnectionID     string      `json:"connectionId"`
	ResumeToken      Secret      `json:"resumeToken"`
	Resumed          bool        `json:"resumed"`
	RoomID           string      `json:"roomId,omitempty"` // set when resumed into a room
	DefaultRoomID    string      `json:"defaultRoomId"`    // "lounge" (03 §8; changing the default room is later)
	User             UserInfo    `json:"user"`
	ServerTime       time.Time   `json:"serverTime"`
}

type UserInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`            // what the UI shows: the username in M1 (03 has no display names yet)
	Admin bool   `json:"admin,omitempty"`
}

type Limits struct {
	MaxMessageBytes     int   `json:"maxMessageBytes"`               // 65536
	// MaxSDPBytes: max pc.answer (sub) message; pub offers are limited to 65536 bytes and 8 m-lines by the SFU.
	MaxSDPBytes         int   `json:"maxSdpBytes"`                   // 262144
	MaxSharesPerUser    int   `json:"maxSharesPerUser"`              // 4
	MaxRoomParticipants int   `json:"maxRoomParticipants,omitempty"` // admin soft limit (03 maxParticipantsPerRoom); 0 = none
	MaxRoomShares       int   `json:"maxRoomShares,omitempty"`       // admin soft limit (03 maxSharesPerRoom); 0 = none
	MaxVideoBitrate     int64 `json:"maxVideoBitrate,omitempty"`     // admin cap per share in bit/s (03 maxShareBitrateKbps × 1000); 0 = preset default
	MessagesPerSecond   int   `json:"messagesPerSecond"`             // 20
	MessageBurst        int   `json:"messageBurst"`                  // 100
	PingIntervalMs      int   `json:"pingIntervalMs"`                // 15000
	IdleTimeoutMs       int   `json:"idleTimeoutMs"`                 // 45000
	GraceMs             int   `json:"graceMs"`                       // 30000
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential Secret   `json:"credential,omitempty"`
}
```

Server behavior on `hello`:
1. A second `hello` gets `bad_request` (scope connection), close `4400`. Any other type before `hello` gets
   `hello_required`, close `4400`.
2. Validate: `client.kind` matches `[a-z]{1,16}` (unknown kinds are accepted and treated like `web` for limits),
   `role` is known, `features` ≤ 32 entries, `caps.decode` ≤ 32 entries.
3. Authenticate (§3.2). Failure: `unauthenticated` (scope session), close `4401`. A `hello` without a
   `resumeToken` then needs a connection slot: a bearer-authenticated user with 16 connections already gets
   `too_many_connections`, close `4429`, and so does a cookie socket that was accepted at the cap (§3.1 step 5).
   With a `resumeToken` the cap is checked in step 5, after the token.
4. Negotiate the version (§6.1), then check `Policy().MinClientVersion` for non-web kinds.
5. Resume (§10.3) if `resumeToken` is set and valid; else create a new connection. A token that resumes nothing
   opens a new connection when the user has a slot for one, and gets `too_many_connections`, close `4429`, when
   not.
6. Raise the read limit to 256 KiB and send `welcome`.
7. If resumed into a room: send `room.state`, then call `MediaPeer.Resync()`.

### 8.3 `ping` / `pong` (M1)

```json
{"type": "ping", "data": {"t": 1760295845123}}
{"type": "pong", "data": {"t": 1760295845123, "serverTimeMs": 1760295845160}}
```

```go
type Ping struct {
	T int64 `json:"t"` // sender's clock in ms, echoed back
}

type Pong struct {
	T            int64 `json:"t"`
	ServerTimeMs int64 `json:"serverTimeMs"`
}
```

### 8.4 `room.join`, `room.leave` (M1)

```json
{"type": "room.join", "id": "2", "data": {"roomId": "lounge"}}
{"type": "ok", "re": "2", "data": {"room": {"id": "lounge", "name": "Lounge"}}}
{"type": "room.leave", "id": "9", "data": {}}
{"type": "ok", "re": "9"}
```

```go
type RoomJoin struct {
	RoomID string `json:"roomId"`
}

type RoomJoinResult struct {
	Room RoomInfo `json:"room"`
}

type RoomInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
```

`room.join`:
- Errors: `room_not_found`; `forbidden` (`RoomDirectory.CanJoin`, room locks later); `room_full` (admin soft limit,
  `params.limit`). *Later*: `kicked` (rejoin blocked after an admin kick, `retryAfterMs`).
  - `room_not_found` also when the room was closed by `CloseRoom` after the join read it (checked under the hub
    lock).
  - A `roomId` that isn't ≤ 64 chars `[A-Za-z0-9_-]` gets `room_not_found` (not `bad_request`), matching 03's REST
    404.
- Effects:
  - joining the room the connection is already in replies `ok` and resends `room.state`;
  - otherwise the connection leaves its current room: its shares there end with `left`, and its MediaPeer closes;
  - it then attaches to the Participant and gets a new MediaPeer (`MediaPlane.NewPeer`);
  - others get `room.event participant.joined` if the Participant is new.

`room.leave`: ends the connection's shares (`left`), closes its MediaPeer and detaches it from the Participant. It
replies `ok`, also when the connection is in no room.

### 8.5 `room.state` (M1)

A full snapshot of one room. It is sent:
- to one connection right after `room.join` and after a resumed `welcome`;
- to every connection in the room after any change, coalesced: when nothing was sent in the last 200 ms it goes out
  at once, otherwise at `last + 200 ms`.

Every connection in the room gets byte-identical snapshots: one encode per broadcast.

```json
{"type": "room.state", "data": {
  "roomId": "lounge",
  "rev": 42,
  "participants": [
    {"userId": "k3m9p2qxw7ht", "name": "Alex", "admin": true, "status": "present",
     "joinedAt": "2026-10-12T19:00:01.000Z",
     "connections": [{"id": "c_k3v9q2m7xw4pa8d1", "kind": "web", "role": "full", "status": "online"}]},
    {"userId": "b8f2n4r6t0vz", "name": "Bea", "status": "reconnecting", "joinedAt": "2026-10-12T19:01:12.000Z",
     "connections": [{"id": "c_7d2m9x4k1q8v3p6z", "kind": "web", "role": "viewer", "status": "reconnecting"}]}
  ],
  "shares": [
    {"id": "s_q7m2x9c4v8b1n5k3", "userId": "k3m9p2qxw7ht", "connectionId": "c_k3v9q2m7xw4pa8d1",
     "kind": "screen", "preset": "movie", "audio": true, "status": "live",
     "layers": ["high", "low"], "codec": "h264/6400", "startedAt": "2026-10-12T19:02:30.000Z",
     "watchers": [{"userId": "b8f2n4r6t0vz", "video": "high", "audio": "on"}]}
  ]
}}
```

```go
type RoomState struct {
	RoomID       string            `json:"roomId"`
	Rev          uint64            `json:"rev"` // grows with every change in this server process (one hub-wide counter,
	                                             // so a room's revs may skip numbers); clients reset on every welcome
	Participants []ParticipantInfo `json:"participants"` // sorted by joinedAt, then userId
	Shares       []ShareInfo       `json:"shares"`       // sorted by startedAt, then id
}

type ParticipantInfo struct {
	UserID      string            `json:"userId"`
	Name        string            `json:"name"`
	Admin       bool              `json:"admin,omitempty"` // the user is an admin (03's role); absent: a member
	Status      ParticipantStatus `json:"status"`
	JoinedAt    time.Time         `json:"joinedAt"`
	Connections []ConnectionInfo  `json:"connections"`
}

type ParticipantStatus string

const (
	ParticipantStatusPresent      ParticipantStatus = "present"
	ParticipantStatusReconnecting ParticipantStatus = "reconnecting"
)

// ConnectionInfo is what everyone in the room sees about a connection. OS and version are deliberately absent
// (privacy); admins get them from the live snapshot (§15.2), the user's own devices from user.connections (M2).
type ConnectionInfo struct {
	ID     string           `json:"id"`
	Kind   ClientKind       `json:"kind"`
	Role   Role             `json:"role"`
	Status ConnectionStatus `json:"status"`
}

type ConnectionStatus string

const (
	ConnectionStatusOnline       ConnectionStatus = "online"
	ConnectionStatusReconnecting ConnectionStatus = "reconnecting"
)

type ShareInfo struct {
	ID           string       `json:"id"`
	UserID       string       `json:"userId"`
	ConnectionID string       `json:"connectionId"` // the publishing connection
	Kind         ShareKind    `json:"kind"`
	Label        string       `json:"label,omitempty"` // absent: viewers render t('share.label.<kind>')
	Preset       Preset       `json:"preset"`
	Audio        bool         `json:"audio"`  // declared at start; after live: whether an audio track arrived
	Status       ShareStatus  `json:"status"`
	Layers       []VideoLayer `json:"layers"` // layers whose tracks arrived (paused layers included), e.g. ["high","low"]; [] while starting
	Codec        CodecKey     `json:"codec,omitempty"` // H.264 profile currently published
	StartedAt    time.Time    `json:"startedAt"`
	Replaces     string       `json:"replaces,omitempty"` // re-publish after a restart/rebuild (§10.6)
	Watchers     []Watcher    `json:"watchers"`           // excludes the owner (§4.1)
}

type Watcher struct {
	UserID string     `json:"userId"`
	Video  VideoLayer `json:"video"`
	Audio  AudioState `json:"audio"`
}

type ShareKind string

const (
	ShareKindScreen ShareKind = "screen" // getDisplayMedia displaySurface "monitor"
	ShareKindWindow ShareKind = "window" // "window"
	ShareKindTab    ShareKind = "tab"    // "browser"
)

type Preset string

const (
	PresetAuto  Preset = "auto"
	PresetGame  Preset = "game"
	PresetMovie Preset = "movie"
	PresetText  Preset = "text"
)

type ShareStatus string

const (
	ShareStatusStarting ShareStatus = "starting"
	ShareStatusLive     ShareStatus = "live"
	ShareStatusStalled  ShareStatus = "stalled"
	// later (M2, feature share.pause): ShareStatusPaused = "paused"
)
```

- Share labels default to nothing on the wire. Viewers show the localized "Screen", "Window" or "Tab", so no English
  goes over the wire and window titles never leave the device (plan, privacy).
- "N watching" for the sharer is `len(share.watchers)`.
- `admin` is the user's role (03), for the people panel's admin badge (05 §11.2). It is `true` on every participant
  who is an admin and absent on members, like `welcome.user.admin`.
  - The hub takes it from the connection's `Identity`, so a role change reaches the room like a rename: `UpdateUser`
    or a `Revalidate` result (§3.2) changes the participant and sends a new snapshot.
  - A participant has one name and one role for all of the user's connections in the room (§4.1), at first those of
    the connection that created it. A connection that joins later can know more, because a change that only
    `Revalidate` reports (the admin CLI) reaches each connection at its own tick and a new connection at its
    handshake. So at every revalidation tick of a connection in the room, a successful `Revalidate` applies that
    connection's identity to the participant again, changed or not: `room.state` is right within 5 min of such a
    change, whichever of the user's connections come and go. A participant that is already in line sends nothing.
  - The field is additive (§14.1): a client that doesn't know it ignores it, and a client that does reads a snapshot
    without it as "members only". Fixture `room.state.admin.json` holds it; `room.state.json` stays as it was.
  - It is a label, not a permission. Admin actions are REST calls that the server checks against the store (03).

### 8.6 `room.event` (M1)

Discrete, human-facing changes: toasts, `aria-live`, push-like in-app notices. They are never sent as part of a
snapshot, on join or on resume. The UI renders from `room.state`; events only announce.

```json
{"type": "room.event", "data": {"kind": "share.started", "roomId": "lounge", "userId": "k3m9p2qxw7ht",
  "name": "Alex", "shareId": "s_q7m2x9c4v8b1n5k3", "at": "2026-10-12T19:02:31.020Z"}}
{"type": "room.event", "data": {"kind": "share.stopped", "roomId": "lounge", "userId": "k3m9p2qxw7ht",
  "name": "Alex", "shareId": "s_q7m2x9c4v8b1n5k3", "reason": "media_timeout", "at": "2026-10-12T19:40:00.000Z"}}
{"type": "room.event", "data": {"kind": "participant.left", "roomId": "lounge", "userId": "b8f2n4r6t0vz",
  "name": "Bea", "reason": "disconnected", "at": "2026-10-12T19:41:07.500Z"}}
```

```go
type RoomEvent struct {
	Kind     RoomEventKind `json:"kind"`
	RoomID   string        `json:"roomId"`
	UserID   string        `json:"userId"`
	Name     string        `json:"name"`               // so a toast works after the person left the snapshot
	ShareID  string        `json:"shareId,omitempty"`
	Replaces string        `json:"replaces,omitempty"` // share.started of a re-publish: clients suppress the toast
	Reason   EndReason     `json:"reason,omitempty"`   // share.stopped, participant.left
	At       time.Time     `json:"at"`
}

type RoomEventKind string

const (
	RoomEventKindParticipantJoined RoomEventKind = "participant.joined"
	RoomEventKindParticipantLeft   RoomEventKind = "participant.left"
	RoomEventKindShareStarted      RoomEventKind = "share.started" // on starting → live, once per share
	RoomEventKindShareStopped      RoomEventKind = "share.stopped"
)

type EndReason string

const (
	EndReasonStopped        EndReason = "stopped"         // share.stop, pc.close, or tracks removed by the owner
	EndReasonLeft           EndReason = "left"            // room.leave / room.join elsewhere / deliberate close / closed by revocation (03)
	EndReasonDisconnected   EndReason = "disconnected"    // grace expired
	EndReasonMediaTimeout   EndReason = "media_timeout"   // starting or stalled for 30 s
	EndReasonKicked         EndReason = "kicked"          // reserved: admin kick is later
	EndReasonRoomClosed     EndReason = "room_closed"
	EndReasonServerShutdown EndReason = "server_shutdown" // shares only; never seen by clients of the new process
)
```

Events go to every connection in the room, including the actor's own connections; clients suppress toasts for their
own user. The owner's connections use `share.stopped.reason` to tell the sharer why the server ended its share
(05 §13.1); no separate error is sent (the one exception is `codec_not_supported`, §9 rule 7). On `share.started`
the hub also calls `PushNotifier.ShareStarted` (04) unless `replaces` is set.

**No push without the room's name** (S40). The notification names the room, and the hub never caches room names, so
it reads the name with `RoomDirectory.GetRoom` when the share goes live (§15.2). That read is a store call: it runs
on a goroutine of its own, bounded at 10 s, and never holds up the connection's messages. When it fails (the room
was deleted in that moment, or the store doesn't answer), the hub logs a WARN line and does not call
`ShareStarted`: a notification that can't say where is not sent. `room.event share.started` went out before the
read and is not affected.

### 8.7 `share.start`, `share.update`, `share.stop` (M1)

```json
{"type": "share.start", "id": "5", "data": {"kind": "screen", "preset": "auto", "audio": true, "ref": "1s7fq2"}}
{"type": "ok", "re": "5", "data": {
  "shareId": "s_q7m2x9c4v8b1n5k3",
  "codec": "h264/6400",
  "encodings": [
    {"rid": "f", "layer": "high", "active": true, "maxBitrate": 8000000, "maxFramerate": 60, "maxPixels": 2073600},
    {"rid": "q", "layer": "low", "active": true, "maxBitrate": 300000, "maxFramerate": 15, "maxPixels": 230400}
  ],
  "audioBitrate": 128000
}}
{"type": "share.update", "id": "11", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "preset": "movie"}}
{"type": "share.stop", "id": "12", "data": {"shareId": "s_q7m2x9c4v8b1n5k3"}}
{"type": "ok", "re": "12"}
```

```go
type ShareStart struct {
	Kind     ShareKind `json:"kind"`
	Label    string    `json:"label,omitempty"` // user-typed only; <= 40 code points
	Preset   Preset    `json:"preset"`
	Audio    bool      `json:"audio"`              // an audio track will be sent
	Ref      string    `json:"ref"`                // client idempotency key, 1-32 chars, unique per connection
	Replaces string    `json:"replaces,omitempty"` // previous shareId of the same local capture (§10.6)
}

// ShareParams tells the sharer how to encode. The server computes it from the preset, the room's codec safe set and
// admin limits (policy and numbers: 02). Encodings is in wire order, high first (f, then q); that is not the order
// a web client gives the browser. Web clients (05 §13.4) pass complete sendEncodings to addTransceiver in ascending
// order (q, then f), each with rid, active, maxBitrate, maxFramerate and scaleResolutionDownBy =
// max(1, sqrt(width*height/maxPixels)). Later changes go through RTCRtpSender.setParameters, matched by rid.
type ShareParams struct {
	ShareID      string     `json:"shareId"`
	Codec        CodecKey   `json:"codec"`        // H.264 profile to put first in setCodecPreferences
	Encodings    []Encoding `json:"encodings"`    // wire order, high first (f, q); not the sendEncodings order
	AudioBitrate int        `json:"audioBitrate"` // Opus target in bit/s (the pub answer's maxaveragebitrate)
}

type Encoding struct {
	RID          string     `json:"rid"`   // RIDHigh or RIDLow
	Layer        VideoLayer `json:"layer"`
	Active       bool       `json:"active"`
	MaxBitrate   int64      `json:"maxBitrate"`
	MaxFramerate int        `json:"maxFramerate"`
	MaxPixels    int        `json:"maxPixels"` // pixel budget, aspect-ratio neutral (ultrawide screens)
}

type ShareUpdate struct {
	ShareID string  `json:"shareId"`
	Label   *string `json:"label,omitempty"` // "" clears the label
	Preset  Preset  `json:"preset,omitempty"`
	// later (M2, feature share.pause): Paused *bool `json:"paused,omitempty"`
}

type ShareStop struct {
	ShareID string `json:"shareId"`
}
```

`share.start`:
- **N shares per user.** The protocol allows up to `maxSharesPerUser` concurrent shares per user; the M1 UI starts
  at most one, and shows "You're already sharing" when `room.state` has a live share of the same user from another
  connection.
- **Checks**:
  - the connection is in a room (`not_in_room`) and has a publishing role (`forbidden`);
  - the user's `starting + live + stalled` shares across all rooms < `maxSharesPerUser` (`share_limit`,
    `params {limit, per: "user"}`);
  - the room's shares < `maxRoomShares` when set (`share_limit`, `per: "room"`).
- **Idempotency**: a repeated `ref` on the same connection returns the existing share's `ShareParams`, so a reply lost
  in a reconnect can be retried safely.
- **Effect**: the share is created in `starting` (visible in `room.state`); `MediaPeer.CreateShare` returns the
  `ShareParams`.
- **Then the client** adds its transceivers and sends `pc.offer` on the pub PC with `tracks` naming this `shareId`
  (§9). Web (05 §13.4, the setup S4 validated):
  - set the video track's `contentHint`, then add one video transceiver with **complete** `sendEncodings` in
    ascending order (`q`, then `f`), each carrying `rid`, `active`, `maxBitrate`, `maxFramerate` and
    `scaleResolutionDownBy`, so nothing is left to browser defaults;
  - `setCodecPreferences` with `codec` first, then the other H.264 profiles and RTX;
  - the audio transceiver with `sendEncodings: [{maxBitrate: audioBitrate}]`;
  - one `setParameters` call on the video sender for `degradationPreference` only (try/catch), then `pc.offer`.

  After the offer, `setParameters` is used only for hints (§8.10), source resizes and preset changes, and always
  matches encodings by `rid`.
- **Timeout**: without a first keyframe within 30 s the share ends with `media_timeout`.

`share.update`:
- It changes the label or preset of a share of the same user (`share_not_found`, `forbidden` if another user's).
- The reply carries the new `ShareParams`. The client applies `encodings` with `setParameters`, matched by `rid`.
  If `audioBitrate` changed, it re-offers the pub PC so the answer carries the new Opus `maxaveragebitrate`.
- **From another connection of the user** (as README S40 built it). Only the publishing connection's actor may call
  that connection's `MediaPeer` (§15.2), so the hub hands the update to it and replies when it has answered:
  - The requesting connection is not held up meanwhile. It handles its later messages before this reply, and the
    `room.state` with the change may reach it first; the request still gets exactly one reply, matched by `id`.
  - The wait is bounded at **10 s**. When the publishing connection doesn't get to the update in that time, the
    reply is `error{internal}` (with a logged `ref`), and the update is dropped if its turn comes later. When the
    publishing connection has left the room or is closing, or the hub shuts down, the reply is `share_not_found`:
    the share ends with its connection.
  - The reply belongs to the socket the request came on. If the requesting connection lost that socket meanwhile,
    the reply is dropped: the client gave the request up with the socket (`connection_lost`, §12.4), and a resumed
    client may use the `id` again.
  - A `share.update` on the publishing connection itself is answered in order: the reply goes out before the
    `room.state` with the change.
- **The publishing connection is not told about an update made elsewhere** (decided after group 5). `room.state`
  shows the new label and preset to everyone, and the SFU keeps the new preset (the Opus bitrate follows with the
  next pub answer, 02 §6.1), but the hub sends the publishing connection no `quality.hint`: `HintReasonPreset`
  (§8.10) is not emitted in M1, so that client keeps the encodings, `contentHint` and `degradationPreference` of the
  preset it started with. Nothing in M1 needs more: the web UI changes a preset only from the page that publishes
  the share (05 §13.5), which applies its own reply. Pushing the new `ShareParams` to the publishing connection
  arrives with the desktop handoff in M2, where the connection that publishes (the app's Go side, role `publisher`)
  and the one that shows the controls (its SPA) are two connections of one user (§6.3).

`share.stop`:
- It is idempotent: an unknown or already ended share replies `ok`. Stopping another user's share is `forbidden`.
- The share ends with `stopped`. The client then stops the share's transceivers and re-offers, or sends `pc.close`
  when no shares remain.
- Server fallbacks:
  - For every pub offer, of any `gen`, the hub ends with `stopped` each share of this connection that an earlier pub
    offer bound and that the new `tracks` omit. It calls `EndShare` before the offer is applied
    (`MediaPeer.HandleOffer`). A `starting` share that no pub offer has bound yet is left alone; its 30 s start
    timeout still applies.
  - `pc.close` on a pub PC ends the shares that PC carried with `stopped`: those that a pub offer bound (S40). A
    `starting` share that no offer has bound yet was never on that PeerConnection, so it is left to its 30 s start
    timeout, as above. The hub ends the shares first and then calls `MediaPeer.ClosePC`.

**What README S40 settled** (the hub's share and `pc.*` plumbing; each item is written out where it belongs):
- `pc.close{pc: pub}` ends only the shares a pub offer bound (above; §9 rule 10).
- A `pc.*` message for the wrong PC kind is `error{bad_request, scope: pc}` with `params {field: "pc", reason:
  "invalid"}`: `pc.offer` for `sub`, `pc.answer` for `pub`, `pc.restart` for `pub` (§9 rule 1).
- A `share.update` from another connection of the user waits up to 10 s for the publishing connection, without
  blocking the requester (above).
- The hub reads `MediaPeer.Stats()` every 2 s for every connection that has media in its room, not only for the
  ones that sent `stats.watch` (§8.11).
- A share that goes live sends no Web Push when the room's name can't be read (§8.6).

### 8.8 `pc.offer`, `pc.answer`, `pc.ice`, `pc.restart`, `pc.close` (M1)

```json
{"type": "pc.offer", "data": {"pc": "pub", "gen": 1, "neg": 1, "sdp": "v=0\r\n…",
  "tracks": [{"mid": "0", "shareId": "s_q7m2x9c4v8b1n5k3", "kind": "video"},
             {"mid": "1", "shareId": "s_q7m2x9c4v8b1n5k3", "kind": "audio"}]}}
{"type": "pc.answer", "data": {"pc": "pub", "gen": 1, "neg": 1, "sdp": "v=0\r\n…"}}
{"type": "pc.offer", "data": {"pc": "sub", "gen": 2, "neg": 1, "sdp": "v=0\r\n…",
  "tracks": [{"mid": "0", "shareId": "s_q7m2x9c4v8b1n5k3", "kind": "video"},
             {"mid": "1", "shareId": "s_q7m2x9c4v8b1n5k3", "kind": "audio"}]}}
{"type": "pc.ice", "data": {"pc": "pub", "gen": 1, "candidate": {"candidate":
  "candidate:1 1 udp 2122260223 198.51.100.23 54321 typ host", "sdpMid": "0", "sdpMLineIndex": 0,
  "usernameFragment": "a1b2"}}}
{"type": "pc.ice", "data": {"pc": "pub", "gen": 1}}
{"type": "pc.restart", "data": {"pc": "sub", "gen": 1, "mode": "ice", "reason": "disconnected"}}
{"type": "pc.close", "data": {"pc": "pub", "gen": 1}}
```

```go
type PCOffer struct {
	PC     PCKind     `json:"pc"`
	Gen    uint32     `json:"gen"`    // PC generation, starts at 1; the side that creates PCs increments it
	Neg    uint32     `json:"neg"`    // offer number within gen, starts at 1; the offerer increments it
	SDP    SDP        `json:"sdp"`    // an ICE restart shows only here (new ICE credentials); there is no flag
	Tracks []TrackRef `json:"tracks"` // every m-section currently carrying a share
}

type PCAnswer struct {
	PC  PCKind `json:"pc"`
	Gen uint32 `json:"gen"`
	Neg uint32 `json:"neg"` // echoes the offer
	SDP SDP    `json:"sdp"`
}

type TrackRef struct {
	MID     string    `json:"mid"`
	ShareID string    `json:"shareId"`
	Kind    TrackKind `json:"kind"`
}

type PCICE struct {
	PC        PCKind        `json:"pc"`
	Gen       uint32        `json:"gen"`
	Candidate *ICECandidate `json:"candidate,omitempty"` // absent = end of candidates (optional; the server ignores it)
}

// ICECandidate has the JSON shape of RTCIceCandidateInit and pion's webrtc.ICECandidateInit.
type ICECandidate struct {
	Candidate        string  `json:"candidate"` // <= 512 bytes
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// PCRestart asks the offerer of a PC to restart ICE or rebuild the PC (§10.4).
// Client -> server: sub only (mode ice or rebuild). Server -> client: pub only, mode rebuild (reason failed).
type PCRestart struct {
	PC     PCKind        `json:"pc"`
	Gen    uint32        `json:"gen"` // generation the requester has now; stale requests are ignored
	Mode   RestartMode   `json:"mode"`
	Reason RestartReason `json:"reason"`
}

type RestartMode string

const (
	RestartModeICE     RestartMode = "ice"
	RestartModeRebuild RestartMode = "rebuild"
)

type RestartReason string

const (
	RestartReasonDisconnected RestartReason = "disconnected"
	RestartReasonFailed       RestartReason = "failed"
	// No "codec" reason: codec-blocked subscriptions are retried by the server (02 §8.5), never by the client.
)

// PCClose tells the server the client closed its pub PC on purpose (no shares left, or leaving).
// M1 clients send it only for pub; the server ignores pc: sub.
type PCClose struct {
	PC  PCKind `json:"pc"`
	Gen uint32 `json:"gen"`
}
```

Rules are in §9.

### 8.9 `subscribe.update`, `subscribe.status` (M1)

```json
{"type": "subscribe.update", "id": "14", "data": {"subs": [
  {"shareId": "s_q7m2x9c4v8b1n5k3", "video": "low", "audio": "off"},
  {"shareId": "s_z1x2c3v4b5n6m7k8", "video": "high", "audio": "on"}]}}
{"type": "ok", "re": "14", "data": {"ignored": []}}
{"type": "subscribe.status", "data": {"subs": [
  {"shareId": "s_z1x2c3v4b5n6m7k8", "video": "low", "audio": "on", "requestedVideo": "high", "reason": "bandwidth"}]}}
```

```go
type SubscribeUpdate struct {
	Subs []SubscriptionWant `json:"subs"` // 1..64 items, unique shareIds
}

type SubscriptionWant struct {
	ShareID string     `json:"shareId"`
	Video   VideoLayer `json:"video"`
	Audio   AudioState `json:"audio"`
}

type SubscribeResult struct {
	Ignored []string `json:"ignored"` // shareIds that don't exist (any more) in the connection's room
}

type SubscribeStatus struct {
	Subs []SubscriptionStatus `json:"subs"` // only the subscriptions whose status changed
}

type SubscriptionStatus struct {
	ShareID        string       `json:"shareId"`
	Video          VideoLayer   `json:"video"` // being forwarded now
	Audio          AudioState   `json:"audio"`
	RequestedVideo VideoLayer   `json:"requestedVideo"`
	Reason         StatusReason `json:"reason,omitempty"` // absent: video == requestedVideo
}

type StatusReason string

const (
	StatusReasonBandwidth   StatusReason = "bandwidth"   // server downgraded (REMB/loss estimate, 02)
	StatusReasonUnavailable StatusReason = "unavailable" // publisher doesn't send that layer (now)
	StatusReasonCodec       StatusReason = "codec"       // this client can't decode the share's codec (§11.7)
	StatusReasonWaiting     StatusReason = "waiting"     // sub PC not connected yet, or waiting for a keyframe
)
```

02 reports finer internal reasons (`codec_mismatch`, `decoder_unavailable`, `decoder_failed`, `no_preview_layer`, …);
the adapter maps them to these four (§15.4). "Give up and reload" after a codec wait is a client timer (§11.7), so no
extra wire value is needed.

- `subscribe.update` is **desired state per share**, merged into the connection's existing wants. Items for shares
  not in the room are ignored and listed in `ignored`, never an error: shares can end while the request is in flight.
  An item that fails for another reason (for example too many subscriptions) fails the whole request with the mapped
  error (§15.4); the items before it stay applied.
- `video: off` with `audio: off` pauses a subscription but keeps its transceivers, so toggling a tile needs no
  renegotiation. A subscription is removed only when its share ends or the connection leaves the room.
- **Audio-follows-focus** (M1): which layer and audio each share gets (auto/manual focus, speaker button, PiP, hidden
  page) is 05's policy (05 §12.2–§12.4). Typical results: focused `{high, on}`, visible `{low, off}`, not visible
  `{off, off}`.

  The client sends all changes of one user action in **one** `subscribe.update`, so the server applies them
  atomically: the old audio stops and the new one starts in the same step.
- After every resume or rejoin, the client re-sends its full desired set (§10.5).
- The server sends `subscribe.status` when forwarding differs from the request or changes back. Statuses are
  re-emitted after a resume (`MediaPeer.Resync`).

### 8.10 `quality.hint`, `caps.update` (M1)

```json
{"type": "quality.hint", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "reason": "codec", "codec": "h264/42e0"}}
{"type": "quality.hint", "data": {"shareId": "s_q7m2x9c4v8b1n5k3", "reason": "viewers",
  "encodings": [
    {"rid": "f", "layer": "high", "active": false, "maxBitrate": 8000000, "maxFramerate": 60, "maxPixels": 2073600},
    {"rid": "q", "layer": "low", "active": true, "maxBitrate": 300000, "maxFramerate": 15, "maxPixels": 230400}]}}
{"type": "caps.update", "data": {"caps": {"decode": ["h264/42e0", "h264/4200", "opus"]}}}
```

```go
// QualityHint tells a sharer to change how it encodes one share. Policy (when and what): 02.
type QualityHint struct {
	ShareID    string     `json:"shareId"`
	Reason     HintReason `json:"reason"`
	Codec      CodecKey   `json:"codec,omitempty"`      // set: switch profile (the client re-offers the pub PC with it first)
	Encodings  []Encoding `json:"encodings,omitempty"`  // set: the complete list; apply every entry with setParameters (active, maxBitrate, ...)
	MaxBitrate int64      `json:"maxBitrate,omitempty"` // native publishers (M2+): total cap, applied with hysteresis
}

type HintReason string

const (
	HintReasonViewers    HintReason = "viewers"    // e.g. nobody watches high: deactivate "f"
	HintReasonCongestion HintReason = "congestion" // server uplink estimate
	HintReasonCodec      HintReason = "codec"      // room codec safe set changed
	HintReasonPreset     HintReason = "preset"     // reserved: never sent in M1 (§8.7 share.update); M2 desktop handoff
	HintReasonAdmin      HintReason = "admin"
)

type CapsUpdate struct {
	Caps Caps `json:"caps"`
}
```

- Browser sharer on `quality.hint`:
  - `encodings` set: apply within 1 s with `RTCRtpSender.setParameters` (per `rid`: `active`, `maxBitrate`,
    `maxFramerate`, `scaleResolutionDownBy` from `maxPixels`);
  - `codec` set: call `setCodecPreferences` with that profile first and re-offer the pub PC (same `gen`, `neg + 1`).
    The server's answer puts that profile first, and Chrome switches encoders with a keyframe.
  - A `codec` equal to the one the client last applied needs no re-offer, because hints are re-sent after every
    resume.
- After a resume (`Resync()`), the server re-sends per share the codec hint and the last encodings hint, if one was
  sent.
- `caps.update`: the client sends it when its capabilities change (web: poll the capabilities every 5 s while any
  subscription has `reason: codec`). The server updates the room's codec safe set (02) and retries codec-blocked
  subscriptions (§11.7). At most 12 per minute. It is the client's only part in codec recovery: all rebuild retries
  are server-driven (02 §8.5).

### 8.11 `stats`, `stats.watch` (M1)

- Client → server `stats` every 10 s while the client has a PC. At most one per 5 s is processed; the rest are dropped
  silently.
- The hub keeps the last report per connection for the admin live snapshot and debug logs. No IP addresses: the
  candidate type only.
- `stats.watch {on: true}` makes the server send `stats` (`ServerStats`) to this connection every 2 s until
  `{on: false}` or the connection closes. It is used by the "Stats for nerds" overlay.
- **The hub reads the server's stats for every connection with media, watched or not** (S40). While a connection
  publishes a share in its room, has stated a desired subscription there, or has `stats.watch` on, its actor calls
  `MediaPeer.Stats()` every 2 s. The result is kept with the connection's room membership, where `Hub.Snapshot`
  takes each share's published layers and sums its egress (`LiveShare`, §15.2: only a connection's actor may call
  its peer, so the snapshot can't ask the SFU itself), and it goes to the client as `stats` only while that client
  watches. The reads go on while the connection is detached (its media may still flow) and stop, with the kept
  result forgotten, once the connection has no media in the room and doesn't watch. Outside a room there is nothing
  to read: `stats.watch {on: true}` is answered `ok` there and the messages start with the next `room.join`.

```json
{"type": "stats", "data": {"intervalMs": 10000,
  "pcs": [{"pc": "sub", "gen": 1, "state": "connected", "rttMs": 38, "candidateType": "prflx", "transport": "udp"}],
  "inbound": [{"shareId": "s_q7m2x9c4v8b1n5k3", "kind": "video", "bitrate": 7400000, "packetsLost": 12,
    "jitterBufferMs": 41, "fps": 59.8, "width": 1920, "height": 1080, "freezeCount": 0,
    "decoder": "ExternalDecoder", "hwDecoder": true, "codec": "h264/6400"}]}}
{"type": "stats.watch", "id": "20", "data": {"on": true}}
{"type": "stats", "data": {"downlinkEstimate": 24000000,
  "subs": [{"shareId": "s_q7m2x9c4v8b1n5k3", "kind": "video", "layer": "high", "bitrate": 7600000,
            "lossPct": 0.2, "dropped": 0}],
  "layers": []}}
```

```go
type ClientStats struct {
	IntervalMs int             `json:"intervalMs"`
	PCs        []PCStats       `json:"pcs"`
	Inbound    []InboundStats  `json:"inbound,omitempty"`
	Outbound   []OutboundStats `json:"outbound,omitempty"`
}

type PCStats struct {
	PC              PCKind `json:"pc"`
	Gen             uint32 `json:"gen"`
	State           string `json:"state"` // RTCPeerConnectionState
	RTTMs           int    `json:"rttMs,omitempty"`
	OutgoingBitrate int64  `json:"outgoingBitrate,omitempty"` // availableOutgoingBitrate
	CandidateType   string `json:"candidateType,omitempty"`   // local side of the selected pair: host|srflx|prflx|relay
	Transport       string `json:"transport,omitempty"`       // udp|tcp
}

type InboundStats struct {
	ShareID        string    `json:"shareId"`
	Kind           TrackKind `json:"kind"`
	Bitrate        int64     `json:"bitrate"`
	PacketsLost    int64     `json:"packetsLost"`
	JitterBufferMs int       `json:"jitterBufferMs,omitempty"`
	FPS            float64   `json:"fps,omitempty"`
	Width          int       `json:"width,omitempty"`
	Height         int       `json:"height,omitempty"`
	FreezeCount    int       `json:"freezeCount,omitempty"`
	Decoder        string    `json:"decoder,omitempty"`
	HWDecoder      bool      `json:"hwDecoder,omitempty"`
	Codec          CodecKey  `json:"codec,omitempty"`
	// Cumulative counters since the track started (getStats values); the hub turns deltas into the
	// isshoni_client_* metrics of §18 for the M1 exit test (06 §12.3).
	FramesDecoded    int64 `json:"framesDecoded,omitempty"`
	FramesDropped    int64 `json:"framesDropped,omitempty"`
	FreezeDurationMs int64 `json:"freezeDurationMs,omitempty"`  // totalFreezesDuration
	ConcealedSamples int64 `json:"concealedSamples,omitempty"`  // audio
	TotalSamples     int64 `json:"totalSamples,omitempty"`      // audio: totalSamplesReceived
}

type OutboundStats struct {
	ShareID           string    `json:"shareId"`
	Kind              TrackKind `json:"kind"`
	RID               string    `json:"rid,omitempty"`
	Bitrate           int64     `json:"bitrate"`
	FPS               float64   `json:"fps,omitempty"`
	Width             int       `json:"width,omitempty"`
	Height            int       `json:"height,omitempty"`
	Encoder           string    `json:"encoder,omitempty"`
	HWEncoder         bool      `json:"hwEncoder,omitempty"`
	QualityLimitation string    `json:"qualityLimitation,omitempty"` // none|cpu|bandwidth|other
}

type StatsWatch struct {
	On bool `json:"on"`
}

// ServerStats: values come from MediaPeer.Stats() (02).
type ServerStats struct {
	DownlinkEstimate int64              `json:"downlinkEstimate,omitempty"` // bit/s for this connection's sub PC
	Subs             []ServerSubStats   `json:"subs"`
	Layers           []ServerLayerStats `json:"layers"` // this connection's own published layers
}

type ServerSubStats struct {
	ShareID string     `json:"shareId"`
	Kind    TrackKind  `json:"kind"`
	Layer   VideoLayer `json:"layer,omitempty"` // video only
	Bitrate int64      `json:"bitrate"`
	LossPct float64    `json:"lossPct"`
	Dropped int64      `json:"dropped"` // packets dropped by the DownTrack queue
}

type ServerLayerStats struct {
	ShareID string    `json:"shareId"`
	Kind    TrackKind `json:"kind"`
	RID     string    `json:"rid,omitempty"`
	Bitrate int64     `json:"bitrate"`
	LossPct float64   `json:"lossPct"`
}
```

### 8.12 `invalidate`, `server.shutdown` (M1)

`invalidate` tells the SPA which REST resources (03) changed, so it can refetch them (TanStack Query invalidation).
Example uses: the room list and "Create room" appear when an admin creates a second room; the admin's approval queue
badge updates live.

```json
{"type": "invalidate", "data": {"topics": ["rooms"]}}
{"type": "server.shutdown", "data": {"reason": "restart", "reconnectInMs": 1840}}
```

```go
type Invalidate struct {
	Topics []Topic `json:"topics"`
}

type Topic string

const (
	TopicRooms          Topic = "rooms"           // room list                    → everyone
	TopicMe             Topic = "me"              // own account (name, admin)    → that user
	TopicDevices        Topic = "devices"         // the user's sessions/devices  → that user
	TopicAdminUsers     Topic = "admin.users"     // admins only
	TopicAdminInvites   Topic = "admin.invites"   // admins only
	TopicAdminApprovals Topic = "admin.approvals" // admins only
	TopicAdminSettings  Topic = "admin.settings"  // admins only
)

type ServerShutdown struct {
	Reason        ShutdownReason `json:"reason"`
	ReconnectInMs int            `json:"reconnectInMs"` // per connection, uniform random in [500, 3000]
}

type ShutdownReason string

const (
	ShutdownReasonRestart ShutdownReason = "restart" // M1 always sends this (SIGTERM can't tell restart from stop)
	ShutdownReasonStop    ShutdownReason = "stop"
)
```

The exact REST paths per topic are 03's; the SPA maps topics to its query keys (05).

### 8.13 `ok`, `error`, and fallbacks (M1)

```json
{"type": "error", "re": "5", "data": {"code": "share_limit", "retryable": false, "scope": "request",
  "params": {"limit": 4, "per": "user"}}}
{"type": "error", "data": {"code": "sdp_invalid", "retryable": false, "scope": "pc", "pc": "sub", "gen": 2, "neg": 3}}
{"type": "error", "data": {"code": "session_revoked", "retryable": false, "scope": "session"}}
```

```go
type Error struct {
	Code         ErrorCode      `json:"code"`
	Retryable    bool           `json:"retryable"`
	Scope        ErrorScope     `json:"scope"`
	RetryAfterMs int            `json:"retryAfterMs,omitempty"`
	ShareID      string         `json:"shareId,omitempty"`
	RoomID       string         `json:"roomId,omitempty"`
	PC           PCKind         `json:"pc,omitempty"`
	Gen          uint32         `json:"gen,omitempty"`
	Neg          uint32         `json:"neg,omitempty"`
	Params       map[string]any `json:"params,omitempty"` // machine-readable values for i18n interpolation; never prose
}

type ErrorScope string

const (
	ErrorScopeRequest      ErrorScope = "request"      // only the request `re` failed (never used for notifications)
	ErrorScopeSubscription ErrorScope = "subscription" // reserved: no M1 code uses it; subscribe.status reports subscriptions
	ErrorScopeShare        ErrorScope = "share"        // one of the user's shares failed or can't be published
	ErrorScopePC           ErrorScope = "pc"           // negotiation (pc, gen, neg) failed
	ErrorScopeRoom         ErrorScope = "room"         // the connection is no longer in the room
	ErrorScopeConnection   ErrorScope = "connection"   // the server closes the socket next; retryable = reconnect helps
	ErrorScopeSession      ErrorScope = "session"      // credentials are gone; go to the login page
)
```

`ErrorCode` constants are listed in §12. Client fallbacks for unknown values from a newer server:

| Field | Unknown value is treated as |
|---|---|
| `ShareKind` | `screen` |
| `Preset` | `auto` |
| `ShareStatus` | `live` |
| `VideoLayer` (in status, watchers) | `high` |
| `ParticipantStatus`, `ConnectionStatus` | `present`, `online` |
| `ClientKind`, `Role` (in snapshots) | `web`, `full` |
| `RoomEventKind`, `Topic` | ignored |
| `ErrorCode` | generic by `scope` and `retryable` (§12.3) |
| `StatusReason`, `HintReason`, `EndReason` | shown without a specific reason |
| Message `type` | ignored (client); `unknown_type` if it had an `id` (server) |

### 8.14 `agent.send`, `agent.recv` (M1 transport, payloads later M2/M4)

The same-user relay (plan): the web page drives the Linux agent, and the web page hands off to the desktop app. Only
the transport is defined in v1. The server never parses `payload`.

```json
{"type": "agent.send", "id": "30", "data": {"toRole": "agent", "kind": "share.request",
  "payload": {"preset": "game"}}}
{"type": "ok", "re": "30", "data": {"delivered": 1}}
{"type": "agent.recv", "data": {"from": "c_k3v9q2m7xw4pa8d1", "kind": "share.request", "payload": {"preset": "game"}}}
```

```go
type AgentSend struct {
	To      string          `json:"to,omitempty"`     // a connectionId of the same user, or
	ToRole  Role            `json:"toRole,omitempty"` // all the user's connections with this role (exactly one of To/ToRole)
	Kind    string          `json:"kind"`             // [a-z.]{1,32}; registry in 04/M2 docs
	Payload json.RawMessage `json:"payload"`          // any JSON value but null, <= 16 KiB; opaque to the server
}

type AgentSendResult struct {
	Delivered int `json:"delivered"`
}

type AgentRecv struct {
	From    string          `json:"from"` // the sender's connectionId
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"` // the agent.send's payload: never null
}

// UserConnections (later M2, feature user.connections): all of the user's connections, sent to each of them on change.
type UserConnections struct {
	Connections []OwnConnection `json:"connections"`
}

type OwnConnection struct {
	ConnectionInfo
	OS      ClientOS `json:"os"`
	Version string   `json:"version"`
	RoomID  string   `json:"roomId,omitempty"`
}
```

Rules:
- The server delivers only to connections of the **same user**. An unknown target, or one of another user, gets
  `agent_target_not_found`, so other users' connection ids are never confirmed.
- 10 messages per second per connection, refused ones included (below).
- Reserved `kind` names: `share.request`, `share.status` (M4); `playback.stop` ("Now watching in the isshoni app",
  M2); `exclusions.get`, `exclusions.set` (M2).
- The plan's `share.request` signaling message is this relay kind, not a top-level message type: only the user's
  own connections ever see it.

**As README S51 built it** (`signal/relay.go`). The rules above stand; these are the ones they left open.

- **Who is a target.** With `to`, the one connection of that id; with `toRole`, every connection of the user with
  that role. In both cases a target is a connection that
  - belongs to the sender's user, whatever its session or device;
  - is **not the sender**: a connection never relays to itself, neither by its own id nor by its own role;
  - has the **`agent.relay` feature** active (§6.2): a client that didn't ask for it doesn't know `agent.recv`;
  - is **online**: it has a socket that still takes messages. A detached connection (§4.2) is no target, because
    what the hub sends it is dropped and never replayed (§10.5), and the sender had better hear that nobody got
    the message.
- **`ok` always has `delivered >= 1`.** Without a target the reply is `agent_target_not_found`, for every reason
  alike: an unknown id, another user's connection, the sender's own id, a connection without the feature, one
  that is detached, a role that none of the user's other connections has. So the relay never confirms that
  another user's connection id exists, and never answers `ok {delivered: 0}`.
- **`delivered` is a count, not an acknowledgment.** It counts the connections whose actor was handed the message
  while their socket was open. A socket can still fail before the message is written, and a connection can detach
  before its actor gets to it. Kinds that need an answer define one (`share.request` and `share.status`, M4).
  - The message is encoded once and posted to each target's actor, which queues it on its socket in order with
    that connection's other messages. The sender's actor never waits for a target's: a target that is busy or
    stuck costs the sender nothing, and two connections may relay to each other at once.
  - A target whose actor's inbox is full is not handed the message and is not counted. It is closed for good as
    `slow_connection`, without grace, like any connection whose inbox overflows (§15.2).
- **The payload is relayed as the same JSON value.** The server never parses it: `agent.recv.payload` is the value
  that came in, byte for byte but for the white space between its tokens, which is dropped. In particular the hub
  does not escape it the way `encoding/json` escapes text for a web page (`<`, `>`, `&`, U+2028 and U+2029 as six
  bytes each), so an `agent.recv` is never more than its payload plus an envelope of about a hundred bytes: what a
  sender is charged for (16 KiB, 10 per second) is the most each target is sent, and `agent.recv` stays far below
  the 64 KiB of a message. `from` is the sender's `connectionId`, which a target answers with an `agent.send {to}`
  of its own. The payload is never logged (§17); the debug line has the `kind` and the count.
- **Order of the checks, and what costs a token.**
  1. The frame is at most 64 KiB (`message_too_large`) and the connection has the `agent.relay` feature
     (`feature_disabled`). Every role may send (§6.3). Neither refusal costs a token of the `agent.send` bucket
     (the global message rate of §13 counts every frame).
  2. The `agent.send` bucket (§13): 10 per second per connection, else `rate_limited` with `retryAfterMs`. It is
     charged here, **whatever becomes of the message afterwards**: a send that is then refused costs its token
     like one that is delivered. So a connection also gets at most ten answers per second about connection ids.
  3. The payload of the request (`protocol.AgentSend.Validate`): exactly one of `to` and `toRole`, a known role,
     the form of `kind`, and a `payload` (`bad_request` with the field; a missing `payload` is `{field: "payload",
     reason: "required"}`). A `payload` over 16 KiB is `message_too_large` in scope `request`, not `bad_request`.
  4. The targets. Validation comes first, so an oversized message to nobody is `message_too_large`.

**`payload: null` is `bad_request`** (decided after group 6; README F03 implements it). As S51 merged it, a `null`
payload passed step 3, being a JSON value of four bytes, and was relayed as `agent.recv {payload: null}`. That is
the one way a client could make the hub send a `null`, which nothing on this wire does (§5: absent means `{}`, and
a test asserts that no hub output holds one), and it reads the same as a payload that was left out, which is
refused. So `payload` must be a JSON value other than `null`: `agent.send {payload: null}` gets `error{bad_request,
scope: request}` with `params {field: "payload", reason: "required"}`, the answer for a missing one, reaches
nobody, and costs its token like every refused send. That is the one thing the server reads of a payload. A
`null` inside one (`{"a": null}`, `[null]`) stays the sender's business, like everything else in it, and a kind
with nothing to say sends `{}`.

## 9. WebRTC negotiation rules (M1)

These rules bind the web client (05), the Go test and load-test clients, the SFU (02) and later native clients.

1. **Fixed offerer.** The client always offers on `pub` and the server always offers on `sub`. There is no glare
   handling.
   - A client message that goes the wrong way is refused by the hub, before the SFU sees it (S40): `pc.offer{pc:
     sub}`, `pc.answer{pc: pub}` and `pc.restart{pc: pub}` get `error{bad_request, scope: pc, pc, gen, neg}` with
     `params {field: "pc", reason: "invalid"}`. `pc.ice` goes either way, and `pc.close{pc: sub}` is ignored
     (rule 10).
   - The hub checks every `pc.*` message in this order: the payload (`bad_request` with the field), the role
     (`forbidden` when the role may not use that PC, §6.3), the direction (above), for `pc.restart` its rate limit
     (§13), then the room (`not_in_room`); a pub offer with a new `gen` meets its rate limit after that. `pc.*`
     messages are notifications, so each of these errors has scope `pc` with the message's `pc`, `gen` and `neg`,
     never scope `request`.
2. **Generations.**
   - `gen` starts at 1 per PC kind per connection. The side that creates PCs increments it when it replaces a PC:
     the client for `pub`, the server for `sub`.
   - An offer with a higher `gen` replaces the receiver's PC: close the old one, create a new one.
   - Messages with a lower `gen` are dropped. `pc.offer` for `pub` with an old `gen` gets
     `error{stale_negotiation, scope: pc, pc, gen, neg}`.
   - After `welcome{resumed: false}` everything resets: all PCs are gone, and `gen` starts at 1 again.
   - The server-side `gen`/`neg` bookkeeping is 02's (the SFU) for both PC kinds. The hub keeps no PC counters and
     checks only role and ownership.
3. **Negotiations.** Within a `gen`, the offerer numbers offers `neg = 1, 2, …` and has at most one outstanding.
   - Changes made while an offer is outstanding are folded into one follow-up offer (S4 `negotiateSub`). The server
     debounces sub offers by 50 ms.
   - The answer echoes `neg`. The offerer ignores answers whose `neg` isn't its outstanding one.
   - The answerer handles a repeated `neg` by resending its stored last answer (safe replay after a resume). The
     client ignores a sub offer with a lower `neg`. The server answers a pub offer with a lower `neg` with
     `error{stale_negotiation, scope: pc, pc, gen, neg}`, which the client ignores.
4. **Track mapping.**
   - Every offer lists `tracks` for all m-sections currently carrying a share. Both sides use `tracks`, not msid, to
     map m-sections to shares; the SFU still sets the msid stream id to the `shareId` for debugging.
   - The pub offer's `tracks` name shares published by this connection. TrackRefs of a share that isn't a
     `starting`/`live`/`stalled` share of this connection (for example one that ended in a race) are ignored: the
     server still answers, and those m-sections carry no share. A malformed `tracks` array (duplicate mids, > 8
     entries, bad `kind`) gets `error{bad_request, scope: pc, pc, gen, neg, params.field: "tracks"}`; `pc.offer` is a
     notification, so its errors are never scope `request`.
   - A share has exactly one video m-section with 0–2 rids from {`f`, `q`} (none = a single `f` layer), and at most
     one audio m-section.
   - The sub offer's mapping can change between offers, because the SFU reuses inactive transceivers (02). The client
     re-maps on every offer.
5. **Candidates.**
   - Clients trickle their candidates with `pc.ice`, and may send an end-of-candidates marker.
   - The server puts all of its candidates in its SDP: host candidates with the public address, UDP 7882, TCP 443
     (ICE-TCP through the 443 mux, not in `tls.mode=off`) and TCP 7882. It waits for gathering (instant with muxes,
     capped at 2 s) and never trickles (02). `MediaSink.ICE` exists for later use only.
   - Both sides buffer candidates that arrive before the remote description of the same `gen` (S4). The server keeps
     at most 64 remote candidates per PC per `gen` in total, buffered or applied; later ones are dropped (the early
     ones are the host candidates).
6. **Web answers.** The web client adds `stereo=1;sprop-stereo=1` to the Opus `fmtp` of its sub answer (S4:
   receivers must ask for stereo themselves).
7. **Publishing codec.** The client offers every H.264 profile it can encode (packetization-mode 1) plus RTX, with
   `ShareParams.codec` (or the latest `quality.hint.codec`) first. It offers no VP8/VP9/AV1 in v1. If the offer has no
   usable H.264, the hub sends `error{codec_not_supported, scope: share, shareId}` and then ends the share with
   `stopped`: the error goes first, so the client knows why before it sees the share end (S40; §15.4).
8. **Failures while applying.**
   - An SDP that can't be applied gets `error{sdp_invalid, scope: pc, pc, gen, neg}`. The PC's owner rebuilds it once
     (`gen + 1`, or `pc.restart{rebuild}` for sub).
   - A second `sdp_invalid` within 60 s on the same PC shows a "can't connect media" error with a Reload button
     (client-side).
9. **At most 2 PCs per connection** (plan guard), by construction: one per kind, replaced by `gen`.
10. **Closing.**
    - The client sends `pc.close` when it closes its pub PC on purpose (last share stopped, leaving the room). The
      hub ends, with `stopped`, the shares of this connection that a pub offer bound, then closes the PC at the SFU;
      a `starting` share that no offer has bound yet stays, under its 30 s start timeout (§8.7).
    - The sub PC closes server-side on `room.leave` or a `room.join` elsewhere; the client closes its local sub PC
      without sending anything. The server ignores `pc.close{pc: sub}` (reserved).
    - The server never asks to rebuild a pub PC that carries no live share. `room.leave` closes both PCs server-side.
      - **The server enforces this, in the hub** (decided after group 6; README F03 implements it).
        The request in question is `pc.restart{pc: pub, mode: rebuild, reason: failed}`, which is made of the
        SFU's `PCStateEvent{PCPub, failed}` (§15.4). `sfuplane` passes every such event on as a `RestartRequest`;
        the hub's `MediaSink` decides on the connection's actor, in order with everything that starts or ends its
        shares, because the share lifecycle is the hub's. As group 6 merged it, every such event became a request,
        also for a pub PC whose connection publishes nothing any more: the last share was stopped or timed out, and
        the PC failed before the client's `pc.close` reached the server, or the client never sent one. That
        client would build and negotiate a PeerConnection that carries nothing.
      - The rule: the request goes out only while the failed PC's connection has a share that has not ended,
        one that is `starting`, `live` or `stalled` (to the SFU: pending, or live with a stalled one included).
        A `starting` share waits for the new PC's first keyframe and a `stalled` one for its media to come back,
        each under its 30 s timeout (§4.4), so both need the rebuild. Without such a share nothing is sent. What
        counts is the moment the actor gets to the request, not the moment the PC failed.
      - A share that starts later needs no request: its pub offer comes with a `gen` the client chooses, and a
        client whose pub PC has failed offers `gen + 1` (§10.4).

## 10. Reconnect and recovery (M1)

### 10.1 Signaling client state machine (web and Go clients)

```
              start()
   ┌──────────────────────► connecting ──socket open──► handshaking ──welcome──► ready
   │                            │                            │                      │
   │                    socket error/close          error / close / 10 s      close / ping timeout /
   │                            ▼                            ▼                      │ fatal error
   │   backoff timer ◄───── backoff ◄───────────────────────────────────────────────┘
   └──────────────────────────  │
                                │ error scope session / connection+!retryable / 4401 4403 4409 4426
                                ▼
                            stopped(reason)   (only a new start() or a reload leaves it)
```

- `stopped` reasons and client actions:
  - `unauthenticated`, `session_revoked`: go to the login page;
  - `account_disabled`, `too_many_connections`: show the notice;
  - `protocol_unsupported`, `client_outdated`: show the update screen;
  - `replaced`: stop silently, this socket was superseded;
  - `bad_message`: show "Something went wrong" with Reload.
- While not `ready`, requests fail at once with the local error `connection_lost` (§12.4). The desired subscription
  set and local shares are kept and resynced on `ready` (§10.5).

### 10.2 Backoff

- Delay for attempt n (n = 0 for the first retry after a drop): `min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2)`, clamped to
  **[0.5 s, 10 s]**. That gives about 0.5, 1, 2, 4, 8, 10, 10, … s.
- `n` resets only after the connection has been `ready` for 10 s, so a crash-looping server keeps the backoff high.
- These events skip the current wait once: `online`, page becomes visible, or the user taps Retry (except a
  rate-limit wait, below).
- After `error{rate_limited, scope: connection}` (or close 4429 without a preceding error), the next delay is
  `max(30 s, retryAfterMs)`. `online`, visibility and `retryNow()` don't shorten it.
- After `server.shutdown`, the first delay is exactly `reconnectInMs`, then the normal sequence.
- UI: "Reconnecting…" appears after 2 s in `backoff`, so short blips show nothing. After 30 s it reads "Can't reach
  the server, retrying".

### 10.3 Resume token and grace

- Format: `"r1." + base64url(connIdRaw[10] ‖ nonce[16] ‖ HMAC-SHA256(resumeKey, "isshoni-resume-v1" ‖ connIdRaw ‖ nonce)[:16])`.
  - `resumeKey` is the 32-byte key `keys.resume` in `secrets.json` (04, `config.KeyResume`). `isshoni admin
    rotate-secrets` rotates it and restarts the server (04 §5.3), which invalidates all tokens.
  - The HMAC lets the hub drop forged tokens without a lookup, and tells "valid but from a previous process" apart
    from garbage (both answered `resumed: false`).
- The hub stores `sha256(token)` for the connection's **current** token and rotates it on every `welcome`. An older
  token of the same connection is not accepted.
- The token lives in client memory only, never in `localStorage`: a page reload starts a new connection on purpose.
- Resume succeeds when:
  - the HMAC is valid;
  - the connection exists in this process (ready or detached);
  - the token hash matches;
  - the authenticated identity has the same user and the same session (cookie) or device (bearer) as the
    connection;
  - the `hello` has the connection's `role` and negotiates its protocol version.

  Otherwise `welcome.resumed = false`. That is not an error.
- A resumed connection keeps its client info, features and rate-limit buckets: the resuming `hello`'s `client` and
  `features` are ignored. Its `caps` replace the connection's, since they can have changed while the client was away
  (§11.7); when they differ, the hub calls `MediaPeer.SetCaps` before `Resync()`.
- If the connection's old socket is still attached (half-open), the hub sends it `error{replaced}`, closes it with
  `4409`, then attaches the new socket.
- **Grace**: 30 s (`limits.graceMs`) from detachment. The Connection, its Participant slot, its shares (status
  unchanged while their media flows), its subscriptions and both PCs are kept. When grace expires, the connection is
  closed (§4.2). A detached connection still counts toward the user's 16 connections and shows as `reconnecting` in
  `Hub.Snapshot`; `CloseConnections` and `Shutdown` close it at once, without waiting for the grace.
- Resuming from another IP address (Wi-Fi to LTE) is allowed.

### 10.4 PeerConnection recovery

The client drives the recovery of both of its PCs, from the states it observes. Each 3 s timer is cancelled if the
PC recovers first.

**Pub** (the client offers):

| Condition | Client does |
|---|---|
| ICE `disconnected` for **3 s** | ICE restart: new offer with fresh ICE credentials, same `gen`, `neg + 1` |
| ICE restart not `connected` within **15 s**, PC `failed`, or `pc.restart{pc: pub, mode: rebuild}` received | Rebuild: new PC, `gen + 1`, re-add everything |
| After a resumed `welcome`, the pub PC isn't `connected` | ICE restart |

**Sub** (the server offers):

| Condition | Client does |
|---|---|
| ICE `disconnected` for **3 s** | Sends `pc.restart{pc: sub, mode: ice, reason: disconnected}`; the server answers with an ICE-restart offer (same `gen`, `neg + 1`) |
| ICE restart not `connected` within **15 s** of the ICE-restart offer, or PC `failed` | Sends `pc.restart{pc: sub, mode: rebuild, reason: failed}`; the server offers a new PC (`gen + 1`) |
| After a resumed `welcome` | Nothing: the server's `Resync()` ICE-restarts a sub PC that isn't connected, and that offer counts as the client's restart (below) |

**A sub offer with a new `ice-ufrag` is the sub ICE restart, whoever asked for it**: the client's
`pc.restart{sub, ice}` or the server's `Resync()`. On such an offer the client cancels its 3 s timer and starts the
15 s rebuild timer from that offer, not from its own request. The server runs one sub ICE restart at a time: a
`pc.restart{sub, ice}` that arrives while one for this `gen` is queued, or less than 5 s after its offer with neither
`connected` nor `failed` since, sends no new offer (02 §5.3). This is the usual case after a network switch: the
client's sub PC has been `disconnected` for 3 s when the socket resumes, so its request sent on `ready` and
`Resync()` both ask, and one offer serves both.

**Server**: it starts exactly three actions on its own:
1. `pc.restart{pc: pub, gen, mode: rebuild, reason: failed}` when the current pub PC reaches `failed`, or when a new
   pub PC misses its 10 s handshake (02); only while that connection has a share that has not ended (§9 rule 10);
2. inside `Resync()` (§10.5), an ICE-restart offer for a sub PC that isn't connected;
3. SFU-internal sub rebuilds for codec recovery (§11.7, 02 §8.5).

It never sends `pc.restart{mode: ice}` and never rebuilds a sub PC because of its own ICE state.

Limits:
- The client does at most one ICE restart per PC per 5 s and one rebuild per PC per 10 s. The spacing applies to the
  client's own actions and to its `pc.restart` requests; it does not bind the server's three actions above.
- A `pc.restart` whose `gen` is older than the offerer's current one is ignored.
- While signaling isn't `ready`, PC state changes are only recorded. Acting needs the socket; the resumed-`welcome`
  rows run on `ready`.
- Rebuilding `pub` keeps the same local `MediaStreamTrack`s and the same `shareId`s. Viewers see the share go
  `stalled`, then `live` again.
- After 5 rebuilds of the same PC without reaching `connected` (about 1 min), the UI shows "Can't reach the server's
  media port", links to the connection test (05), and keeps retrying every 30 s.
- ICE restarts also try the TCP 443 candidates, so a network that blocks UDP moves to ICE-TCP by itself.

### 10.5 Resync after `welcome`

**Resumed** (`resumed: true`):
1. Server: `room.state` (if in a room), then `MediaPeer.Resync()`. That re-sends the pending sub offer (same `neg`),
   re-emits `subscribe.status` and `quality.hint`, and ICE-restarts non-connected sub PCs.
2. Client:
   - if `welcome.roomId` is absent or differs from the room it wants, sends `room.join` for that room;
   - re-sends its pending pub offer if it had one (same `neg`);
   - applies the resumed-`welcome` rows of §10.4 (ICE-restart a pub PC that isn't connected; nothing for sub);
   - sends one `subscribe.update` with its full desired set;
   - retries requests that failed with `connection_lost` (`share.start` with the same `ref`, `share.stop`);
   - reconciles its shares against `room.state`: a local live share whose `shareId` is missing on the server is
     re-published with `replaces` (§10.6); a server share with this `connectionId` that the client no longer has is
     stopped with `share.stop`. This re-publish applies only here. A share that the server ends while the socket is
     `ready` is not re-published; the client stops it (05 §13.1).

**Not resumed** (`resumed: false`; server restarted, grace expired or token rotated):
1. The client discards all PCs and resets `gen`.
2. It sends `room.join` with the last room id: memory first (the room the app currently wants: 05's desired room,
   also set by a join made before the first welcome), then `localStorage` `isshoni.lastRoomId`, else
   `defaultRoomId`.
3. It re-publishes every local share whose capture is still alive: `share.start{replaces: oldShareId, ref: new}`, then
   a new pub PC `gen 1`.
4. It sends its desired subscriptions, mapped to the new share ids (§10.6).

### 10.6 Keeping a share across restarts (`replaces`)

- The web sharer keeps its `getDisplayMedia` tracks alive for **60 s** after losing the server, so the user doesn't
  have to pick again. After that it stops them and shows "Sharing stopped: the server was unreachable". Native apps
  keep their encoder for 30 s (plan, M2).
- On re-publish, `share.start.replaces` carries the old `shareId`. The server copies it into `ShareInfo.replaces` and
  `RoomEvent.replaces`, and skips Web Push.
- Viewers move focus, tile position and audio to the new share only if their last snapshot had the replaced share
  with the **same `userId`**. The server can't verify old ids after a restart, so clients check.

## 11. Sequences

Notation: `→` client to server, `←` server to client, `══` media. Ids are shortened (`s_a` = Alex's share).

### 11.1 First join (web viewer)

```
Bea's browser                           Hub (signal)                               SFU
 │ GET /ws  Cookie, Origin ─────────────►│ origin ok; cookie → Identity(u_bea)
 │◄──────────────── 101 ─────────────────│ read limit 64 KiB; 10 s hello timer
 │ hello{1..1, role full, caps} ────────►│ version 1, no resume token
 │◄ welcome{c_b, resumeToken, defaultRoomId lounge, limits}   read limit → 256 KiB
 │ room.join{lounge} id 2 ────────────►│ GetRoom ✓; Participant(u_bea) added
 │                                       │ NewPeer(c_b, lounge, caps, sink) ──────►│
 │◄ ok re 2 {room: Lounge}               │
 │◄ room.state{rev 7, [alex, bea], [s_a live]}
 │                                       │→ others: room.event participant.joined(bea); room.state rev 8
 │ subscribe.update{s_a high on} id 3 ──►│ peer.Subscribe ─────────────────────────►│ sub PC gen 1 + 2 tracks
 │◄ ok re 3 {ignored: []}                │◄──────────── sink.Offer(sub,1,1,tracks) ──│
 │◄ pc.offer{sub,1,1, tracks: 0→s_a video, 1→s_a audio}
 │ pc.answer{sub,1,1} (+stereo=1) ──────►│ peer.HandleAnswer ──────────────────────►│
 │ pc.ice ×n ──────────────────────────►│ peer.AddICE ────────────────────────────►│
 │══════════ ICE (UDP 7882, else TCP 443) + DTLS ════════════════════════════════════│
 │                                       │               sub connected → DownTracks start → PLI → keyframe
 │◄═══════════════════ s_a: layer f + Opus ══════════════════════════════════════════│
 │◄ room.state{rev 9, s_a.watchers += bea(high,on)} (≤ 200 ms, to everyone)
UI: video autoplays muted; "Tap to unmute" unmutes the element locally (no message)
```

### 11.2 Start a share (Chrome/Edge)

```
Alex's browser                          Hub                                         SFU
 getDisplayMedia() → video (+ audio) track; displaySurface → kind; audio track contentHint music
 │ share.start{screen, auto, audio, ref} id 5 ►│ role ✓, in room ✓, limits ✓
 │                                       │ peer.CreateShare(s_a, meta) ────────────►│ codec from room safe set
 │◄ ok re 5 {s_a, codec h264/6400, encodings f,q (wire order, high first), audioBitrate 128000}
 │◄ room.state (s_a starting) to all; viewers show no tile yet
 video track contentHint from the preset
 addTransceiver(video, sendEncodings q,f: complete, ascending; rid, active, maxBitrate, maxFramerate,
                scaleResolutionDownBy) · setCodecPreferences(6400 first, other H.264, RTX)
 addTransceiver(audio, sendEncodings [{maxBitrate: 128000}]) · setCodecPreferences(Opus)
 video sender setParameters: degradationPreference only (try/catch)
 │ pc.offer{pub,1,1, tracks: 0→s_a video, 1→s_a audio} ►│ peer.HandleOffer ────►│ answer: 6400 first,
 │◄ pc.answer{pub,1,1}                   │◄──────────────────────────────────────────│ Opus maxaveragebitrate
 │ pc.ice ×n ───────────────────────────►│
 │══════════ RTP f, q, audio ════════════════════════════════════════════════════════►│ first keyframe
 │                                       │◄──── sink.ShareMedia(s_a, live, layers, codec) ──│
 │                                       │ s_a live; PushNotifier.ShareStarted(absent users)
 │◄ room.state (s_a live) + room.event share.started to all
viewers: newest live share → focus: subscribe.update{[s_prev low off, s_a high on]}
 │◄ room.state (s_a.watchers: 3) → "3 watching"
```

### 11.3 Focus change (video layers)

```
Bea                                     Hub                                         SFU
 clicks tile s_c while s_a is focused
 │ subscribe.update{[s_a low off, s_c high on]} id 17 ►│ merge wants; watchers change
 │                                       │ peer.Subscribe ──────────────────────────►│ s_a DownTrack: target q
 │◄ ok re 17                             │                     PLI q (≤ 1 per 500 ms per layer)
 │══ s_a: keeps sending f until q's keyframe (SPS), then q; seq/ts continuous (munger) ══│
 │══ s_c: switches q → f on f's next keyframe ═══════════════════════════════════════│
 │◄ room.state (watchers updated, ≤ 200 ms)
 no SDP renegotiation: both subscriptions already have transceivers
 if the server later has too little bandwidth for f on s_c:
 │◄ subscribe.status{s_c video low, requestedVideo high, reason bandwidth} → "Reduced quality" badge
```

S4 measured about 52 ms median and 106 ms max for a Chrome switch.

### 11.4 Audio follows focus

```
Bea (focused s_a: high+on)            Hub / SFU
 Carl starts sharing → room.state (s_c live) + room.event share.started
 auto-focus newest (focus mode auto, 05 §12.2):
 │ subscribe.update{[s_a low off, s_c high on]} ─► one atomic update: s_a audio DownTrack stops
 │                                               forwarding, s_c audio DownTrack starts; no overlap. If s_c
 │                                               had no subscription yet, its audio starts after the sub
 │                                               renegotiation (50 ms debounce + one round trip)
 │══ s_c Opus only ══
 speaker button on s_a's tile (audio moves, focus stays):
 │ subscribe.update{[s_c high off, s_a low on]} ─►
 │══ s_a Opus only ══
 tab hidden for > 10 s (05 policy): subscribe.update{[s_c off off, s_a off on]} → audio keeps playing, video costs 0
 visible again: previous wants restored
```

The client also keeps every `<video>`/`<audio>` element without audio `on` muted locally (defense in depth). Audio that
is `off` costs no bandwidth because the server doesn't forward it.

### 11.5 Network blip

**A. WebSocket drops, media fine** (mobile carrier NAT timeout, proxy reset):

```
t=0      socket closes (1006), or a ping gets no pong within 10 s (3 s for an immediate ping, §3.4)
         → client: backoff 0.5 s
         Hub: connection detached; room.state: Bea "reconnecting" (≤ 200 ms). Media keeps flowing.
t≈0.5 s  → hello{resumeToken}   ← welcome{resumed: true, roomId}   ← room.state
         Hub: peer.Resync() → pending offers / statuses / hints re-sent
         → subscribe.update (full desired set)   PCs connected → nothing else
         room.state: Bea "present". No participant.left/joined events.
```

**B. UDP black-holed for 5 s** (plan integration case):

```
t=0      UDP lost both ways; the WebSocket (TCP) is fine
t≈2–5 s  browser ICE → disconnected (Pion: disconnected after its timeout, 02)
         Hub: pub PC not connected ≥ 2 s → s_a stalled → viewers: last frame + "connection unstable"
t+3 s    client (offerer, pub): ICE restart offer → pc.answer
         client (sub): pc.restart{sub, ice, disconnected} → server: ICE restart offer (sub, same gen, neg+1)
t=5 s    UDP back → ICE checks succeed (or TCP 443 pairs win if UDP stays blocked)
t≈5.5 s  connected → keyframe requests → s_a live; subscriptions resume on keyframes
```

**C. Wi-Fi → LTE** (both break): A and B together. Socket and ICE fail, the client resumes the socket first (skip-wait
on `online`), then the resumed-`welcome` rows of §10.4 run: the client ICE-restarts its pub PC, and the server's
`Resync()` ICE-restarts the sub PC. The client's sub 3 s timer has usually expired by then, so it also sends
`pc.restart{sub, ice}` on `ready`; the server sends no second offer for it, and the client counts the `Resync()` offer
(new `ice-ufrag`) as its restart (§10.4). When the new path isn't up within 15 s of the restart offers, the client
rebuilds the pub PC and sends `pc.restart{sub, rebuild, failed}` for the sub PC. The server sends no
`pc.restart{mode: ice}` and doesn't rebuild the sub PC on its own ICE state.

### 11.6 Server restart

```
old process                    clients (Alex sharing s_a, Bea watching)                new process
 SIGTERM → Hub.Shutdown
 ← server.shutdown{restart, reconnectInMs: 500..3000}
 close 1012 (all sockets); PCs closed
                                UI: "Server restarting…" (no error); Alex keeps capture tracks (≤ 60 s);
                                PC failures ignored while not ready
                                wait reconnectInMs, then backoff 0.5 → 10 s
                                                                                   starts (typically 2–10 s)
                                → hello{resumeToken(old)} ────────────────────────► unknown connection
                                ← welcome{resumed: false} ◄─────────────────────────
                                discard PCs, gen := 1
                                → room.join{lastRoomId} ──────────────────────────► ok + room.state
 Alex:                          → share.start{replaces: s_a, ref: r2} ────────────► ok{s_a2, ...}
                                → pc.offer{pub,1,1, tracks→s_a2} ─────────────────► answer; first keyframe
                                ← room.state (s_a2 live, replaces s_a) + room.event share.started{replaces}
                                  (no Web Push)
 Bea:                           sees s_a2.replaces == s_a (same userId as in her last snapshot)
                                → subscribe.update{[s_a2 high on]} (focus and audio carried over)
```

### 11.7 Codec not supported yet: fresh Firefox (S4)

```
Dan's fresh Firefox                     Hub                                    SFU
 caps: RTCRtpReceiver.getCapabilities('video') has no H.264 yet (OpenH264 still downloading)
 │ hello{caps.decode: ["opus"]} ─────────►│
 │◄ welcome · room.join · room.state (s_a live, codec h264/6400)
 │ subscribe.update{s_a high on} ─────────►│ peer.Subscribe ───────────────────────►│ viewer can't decode:
 │                                        │                                        │ adds audio only
 │◄ pc.offer{sub,1,1, tracks: 0→s_a audio}  (answer, ICE) → Dan hears s_a
 │◄ subscribe.status{s_a video off, requestedVideo high, reason codec}  (the first one with reason codec
 │    also shows the toast; there is no separate error)
 UI tile: "Your browser is still getting its video decoder, retrying…" (audio plays)
 client: every 5 s re-read capabilities (the only client-side retry)
 server (02 §8.5): if caps list H.264 but the answer still rejects video (Firefox lists H.264 before it can use it),
   rebuild the sub PC every 20 s, at most 9 times (3 min); same status each time
 …OpenH264 installed → caps now include h264/42e0, h264/4200
 │ caps.update{decode: [h264/42e0, h264/4200, opus]} ►│ SetCaps ───────────────►│ room safe set → {42e0, 4200}
 │                                        │◄── sink.QualityHint(s_a, codec 42e0) ──│
 Alex ← quality.hint{s_a, reason codec, codec h264/42e0} → setCodecPreferences(42e0 first), re-offer pub
        → server answers with 42e0 first → Chrome switches encoder (IDR); room.state s_a.codec = h264/42e0
 │◄ pc.offer{sub, gen 2, neg 1, tracks: video+audio}   (server rebuilt Dan's sub PC: a fresh PC is sure to
 │   see the new decoder)
 │ pc.answer{sub,2,1} → video decodes; subscribe.status{s_a video high} (reason cleared)
 after 3 min without success: tile shows "Reload the page" (a reload gives fresh caps and a fresh PC)
```

Policy details belong to 02:
- A viewer with no H.264 at all is left out of the room's safe set; profile switching can't help it.
- 02 decides whether the room drops to 42e0 for everyone. S4 finding 4: Chrome on macOS then encodes in software.

## 12. Errors and close codes (M1)

### 12.1 Error codes

`type ErrorCode string` with one constant per row (`ErrorCodeBadMessage = "bad_message"`, …). Codes are stable
forever: a code is never renamed or reused with another meaning. 05 maps each to `errors.<code>` in `en.json`.

| Code | Scope | Retryable | Sent when | Client action | Close |
|---|---|---|---|---|---|
| `bad_message` | connection | no | frame isn't a JSON object, or the envelope is invalid | stop; "Something went wrong" + Reload | 4400 |
| `hello_required` | connection | no | first message isn't `hello` | stop (bug) | 4400 |
| `hello_timeout` | connection | yes | no `hello` within 10 s | backoff | 4408 |
| `idle_timeout` | connection | yes | no frame (message or pong) received for 45 s | backoff | 4408 |
| `protocol_unsupported` | connection | no | no common protocol version; `params {serverMin, serverMax, serverVersion}` | update screen ("Ask your admin to update" if the server is older, else "Update the app") | 4426 |
| `client_outdated` | connection | no | native client below `minClientVersion` | "Update the app" + "Open in browser" | 4426 |
| `unauthenticated` | session | no | no valid cookie and no valid bearer | login page | 4401 |
| `session_revoked` | session | no | every 03 revocation reason except `account_disabled`: logged out (here or everywhere), session revoked or expired, password changed or reset, account deleted, device revoked (M2) | login page with notice | 4401 |
| `account_disabled` | session | no | admin disabled the account | notice | 4403 |
| `too_many_connections` | connection | no | more than 16 connections for this user | notice: "Close other isshoni tabs" | 4429 |
| `rate_limited` | request | yes | per-type or global rate limit; `retryAfterMs` | retry after the delay | — |
| `rate_limited` | pc | yes | PC creation limit (02: 10 client-caused PC creations per PC kind per minute), with `pc`, `gen`, `retryAfterMs` | retry the rebuild after the delay | — |
| `rate_limited` | connection | yes | global bucket empty for 10 s | backoff (at least 30 s) | 4429 |
| `slow_connection` | connection | yes | send queue overflow | backoff | 4503 |
| `replaced` | connection | no | the same connection resumed on another socket | stop silently | 4409 |
| `server_shutdown` | connection | yes | the hub is stopping (after `server.shutdown`) | wait `reconnectInMs` | 1012 |
| `internal` | request or connection; pc for a `pc.*` notification; from the SFU with the scope of what failed: request, pc, share or subscription (§15.4) | yes; **no** when the SFU says a retry can't help (§15.4: a call that broke its contract, a method that isn't implemented yet) | unexpected server error; `params {ref}` is an 8-char id also written to the server log, so a user can report it | retry once / backoff; no retry when `retryable` is false | 1011 if connection |
| `bad_request` | request; pc for `pc.*` notifications; connection for a second `hello` | no | payload invalid; `params {field, reason}` with reason `required`, `invalid`, `too_long`, `too_many` or `duplicate` | generic error (a bug) | 4400 if connection |
| `unknown_type` | request | no | a request type the server doesn't know | generic error | — |
| `message_too_large` | request | no | a non-SDP message over 64 KiB after `hello`; an `agent.send` whose `payload` is over 16 KiB (§8.14) | generic error | — |
| `forbidden` | request | no | role, ownership or `CanJoin` check failed | generic "not allowed" | — |
| `feature_disabled` | request | no | a message behind a feature that isn't active | hide the feature | — |
| `not_in_room` | request | no | share or PC message without a room | rejoin, then retry | — |
| `room_not_found` | request | no | unknown, deleted or malformed room id | go to `defaultRoomId` | — |
| `room_full` | request | yes | admin soft limit; `params {limit}` | "Room is full" | — |
| `kicked` | room | no | *later*: an admin removed the user from the room (reserved code) | leave the room UI; "You were removed from {room}" | — |
| `kicked` | request | yes | *later*: `room.join` during the rejoin block; `retryAfterMs` (reserved code) | show the remaining time | — |
| `room_closed` | room | no | an admin deleted the room | go to `defaultRoomId`, toast | — |
| `share_not_found` | request | no | `share.update` of an unknown share | refresh from `room.state` | — |
| `share_limit` | request | no | `params {limit, per: "user"\|"room"}` (per user: the constant 4; per room: 03's `maxSharesPerRoom`) | "You're already sharing" / "Room has too many shares" | — |
| `codec_not_supported` | share | no | publisher offered no usable H.264 | "This browser can't share in a format friends can watch; use Chrome/Edge or the desktop app" | — |
| `sdp_invalid` | pc | no | SDP failed to apply (`pc`, `gen`, `neg`) | rebuild that PC once (§9 rule 8) | — |
| `stale_negotiation` | pc | no | pub offer with an older `gen`, or an older `neg` in the current `gen` (`pc`, `gen`, `neg`) | ignore (the client already moved on) | — |
| `agent_target_not_found` | request | no | no same-user connection matches: none of the user's other connections that is online and has `agent.relay` (§8.14); the same answer for another user's connection id | "Your helper isn't running" (M2/M4) | — |

Scope `request` is used only for messages that have an `id`. An error caused by a `pc.*` notification always has
scope `pc` with `pc`, `gen` and `neg`, whichever row its code comes from.

Shared with 03's REST table (one `errors.<code>` namespace): `bad_request`, `unauthenticated`, `forbidden`,
`rate_limited`, `internal`, `account_disabled`, `room_not_found`, `server_shutdown`. They must mean the same thing in
both. A new code must not reuse a REST code with another meaning (test in `internal/protocol/api`). Payloads differ:
`retryAfterMs` here vs REST `retryAfter` (s); `params.ref` here vs REST `requestId`.

### 12.2 WebSocket close codes

| Code | Meaning | Client without a preceding `error` |
|---|---|---|
| 1000 | Normal closure (client logout or page unload; the server skips grace) | — |
| 1001 | Going away (the browser's own close on unload, Go clients, server stop) | backoff |
| 1003 | Binary frame | stop (bug) |
| 1006 | Abnormal (no close frame: network) | backoff |
| 1009 | Message too big (coder/websocket) | backoff, log |
| 1011 | Internal error | backoff |
| 1012 | Service restart | backoff |
| 4400 | Protocol violation | stop |
| 4401 | Unauthenticated / session revoked | login page |
| 4403 | Forbidden / account disabled | stop |
| 4408 | Hello or idle timeout | backoff |
| 4409 | Replaced by a resume on another socket | stop silently |
| 4426 | Version unsupported / client outdated | update screen |
| 4429 | Rate limited or too many connections | backoff (≥ 30 s) |
| 4503 | Slow connection (send queue full) | backoff |

The preceding `error` (scope `connection` or `session`) decides the client action. The close code is the fallback.

### 12.3 Generic handling of unknown codes

- `request`: reject the pending promise; show a generic error only if the action was user-initiated.
- `subscription`, `share`, `pc`: log; the matching status message drives the UI.
- `room`: leave the room UI and go to `defaultRoomId`.
- `connection`: `retryable` → backoff, else stop with a generic screen and Reload.
- `session`: login page.

### 12.4 Client-local errors (TypeScript and Go client only, never on the wire)

`connection_lost` (the socket went away before a reply), `request_timeout` (no reply in 10 s), `not_ready` (request
made while `stopped`).

## 13. Limits, timeouts and rate limits (M1)

| Item | Value | Where |
|---|---|---|
| Hello timeout | 10 s | server |
| Request timeout (client side) | 10 s | clients |
| Client ping interval / pong timeout; server ping frame interval; server idle timeout | 15 s / 10 s; 15 s; 45 s (any frame, incl. pongs) | §3.4 |
| Grace | 30 s | §10.3 |
| ICE disconnected → restart; restart → rebuild; min spacing (client) | 3 s; 15 s; 5 s (restart), 10 s (rebuild) | §10.4 |
| Share `starting` / `stalled` timeout | 30 s / 30 s | §4.4 |
| `stalled` threshold (pub PC not connected) | 2 s | §4.4 |
| `room.state` coalescing | ≤ 1 per 200 ms per room | §8.5 |
| Sub offer debounce | 50 ms | §9 |
| Kick rejoin block | later (reserved) | §8.4 |
| Revalidation (03 `Touch`) | at connect, then every 5 min | §3.2 |
| Backoff | 0.5 s → 10 s, ×2, ±20 %, reset after 10 s ready | §10.2 |
| `server.shutdown` spread | 500–3000 ms | §8.12 |
| Web sharer keeps capture without a server | 60 s | §10.6 |
| Read limit | 64 KiB before `welcome`; 256 KiB after, for `pc.offer`/`pc.answer` only | §3.3 |
| Send queue | 512 messages or 8 MiB | §3.3 |
| Pre-auth upgrades | 20 per IP per minute (IPv6 per /64); 500 concurrent server-wide | §3.1 |
| Connections per user | 16 | §3.1 |
| Shares per user (all rooms) | 4 (a constant in `signal.DefaultConfig`, not a config key). An abuse guard: GeForce allows 8 NVENC sessions, about 4 two-layer shares (plan) | §8.7 |
| Room soft limits | participants, shares: 0 = none (03 settings `maxParticipantsPerRoom`, `maxSharesPerRoom`, read through `Deps.Policy`) | §8.7 |
| Global message rate | 20/s refill, burst 100; 512 KiB/s refill, burst 1 MiB | server |
| Per type | `room.join` 10/min · `share.start` 10/min · `pc.restart` 12/min per PC · pub offers with a new `gen` 6/min · `caps.update` 12/min · `stats` 1 per 5 s (excess dropped silently) · `agent.send` 10/s | server |
| Field sizes | `id`/`ref` ≤ 32; `label` ≤ 40 code points; room/user ids ≤ 64; `subs` ≤ 64; `tracks` ≤ 8 (pub offers); pub offer SDP ≤ 64 KiB and ≤ 8 m-lines (02 §12); sub answer ≤ 256 KiB; candidate ≤ 512 B; buffered candidates ≤ 64 per PC per gen (later ones dropped); `agent.send.payload` ≤ 16 KiB (above it `message_too_large`, not `bad_request`); `stats` ≤ 16 KiB; `features`, `caps.decode` ≤ 32 entries | `protocol` validation |

The per-type limits are token buckets per connection that start full: `n` per minute or per second is a burst of
`n` that refills evenly, so the 11th `agent.send` in one instant is refused with `retryAfterMs: 100`, and a steady
ten per second is never refused. The buckets of `room.join`, `share.start`, `caps.update`, `stats` and `agent.send`
are charged after the checks that refuse a message outright (its size, an inactive feature, the role) and **before
the handler reads the payload**: the token is spent whatever the handler then answers. For `agent.send` that means
a send refused as `bad_request`, as `message_too_large` for its payload, or with `agent_target_not_found` costs a
token like a delivered one (README S51; §8.14). `agent.recv` costs its receiver nothing: the limits are on what a
client sends. The two `pc.*` limits are charged where those messages are routed, in the order of §9 rule 1.

Why 256 KiB for SDP: a Chrome sub answer is about 2.7 KB per video m-section when the offer lists 5 H.264 profiles
plus RTX, and about 0.6 KB per audio m-section. That is about 3.3 KB per watched share, so 64 KiB would stop a viewer at
about 19 shares, which is a numeric limit on people in disguise. The pre-auth limit, the guard that matters, stays
64 KiB. 02 should also keep sub offers lean: only the profiles in use, and reused inactive transceivers.

## 14. Versioning and compatibility (M1 setup, used from the first release on)

### 14.1 Rules within a version (additive only)

Allowed:
- new optional fields whose zero value means the old behavior;
- new message types behind a feature;
- new server→client enum values that have a client fallback (§8.13);
- new error codes;
- new features.

Forbidden:
- renaming or removing fields;
- changing a field's type, unit or meaning;
- making an optional field required;
- new client→server enum values without a feature;
- reusing an error code with another meaning.

### 14.2 A new protocol version (later)

- Only for a breaking change. `internal/protocol` always holds the newest version.
- The previous version's changed types move to `internal/protocol/v<N-1>` with `ToCurrent`/`FromCurrent` converters.
  The hub applies them at the connection boundary for connections that negotiated N−1.
- `MinVersion = Version - 1`. Release notes flag protocol bumps (06).

### 14.3 Golden fixtures

- `internal/protocol/testdata/v1/*.json`: one full envelope per file, named `<type>[.<variant>].json`.
  - Replies are named `ok.<request type>[.<variant>].json`, so the test knows the result type.
  - SDPs are short placeholders: fixtures test JSON shape, not SDP.
- Minimum set (M1):

  ```
  hello.web.json  hello.resume.json  hello.bearer.json  welcome.json  welcome.resumed.json
  ping.json  pong.json  room.join.json  ok.room.join.json  room.leave.json  room.state.json  room.state.admin.json
  room.event.share.started.json  room.event.share.stopped.json  room.event.participant.left.json
  share.start.json  ok.share.start.json  share.update.json  share.stop.json
  pc.offer.pub.json  pc.answer.pub.json  pc.offer.sub.json  pc.answer.sub.json  pc.ice.json  pc.ice.end.json
  pc.restart.json  pc.close.json  subscribe.update.json  ok.subscribe.update.json  subscribe.status.json
  quality.hint.codec.json  quality.hint.viewers.json  caps.update.json  stats.client.json  stats.watch.json
  stats.server.json  invalidate.json  server.shutdown.json  error.request.json  error.pc.json  error.share.codec.json
  error.session.json  error.bad_request.json  agent.send.json  ok.agent.send.json  agent.recv.json
  ```
- **Compat snapshots** are taken before tagging, in the release-prep PR. The release job cannot commit them: `main`
  requires a PR and `ci-ok`, and Actions may not open PRs (06 §8.6).
  - `task release:compat VERSION=<version>` (06 §7.2; `<version>` is the tag without its `v`, e.g. `0.2.0`) copies
    `testdata/v1` to `testdata/compat/<version>/` and deletes all but the two newest final-release snapshots.
    Prereleases (`-rc.N`) get no snapshot. The PR is merged before the tag is pushed (06 §9.1 "Before tagging").
  - For a non-prerelease tag, release.yml's `build` job first checks that `testdata/compat/<version>/` exists and is
    identical to `testdata/v1`, and fails before goreleaser runs if not (06 §9.3).
  - Tests decode every compat fixture with the current code. They assert that decoding succeeds, and that
    re-encoding keeps every key present in the fixture with an equal value. That enforces "additive only"
    mechanically.

### 14.4 TypeScript generation and drift check

- `tygo.yaml` (repo root):

  ```yaml
  packages:
    - path: "github.com/MoonWX/isshoni/internal/protocol"
      output_path: "web/src/protocol/types.gen.ts"
      enum_style: "union"
      type_mappings:
        time.Time: "string"
        json.RawMessage: "unknown"
      frontmatter: "// Source: internal/protocol. Regenerate with \"task gen\".\n"
    - path: "github.com/MoonWX/isshoni/internal/protocol/api"   # REST DTOs and error codes (03, plus 04's files)
      output_path: "web/src/protocol/api.gen.ts"
      enum_style: "union"
      type_mappings:
        time.Time: "string"
        json.RawMessage: "unknown"
      frontmatter: "// Source: internal/protocol/api. Regenerate with \"task gen\".\n"
  ```

  tygo writes its own `// Code generated by tygo. DO NOT EDIT.` first line, so the frontmatter only names the source.

  Checked with tygo v0.2.21 on a scratch package:
  - defined string types with a `<Type><Value>` const group become `export type X = typeof XA | typeof XB`;
  - `omitempty`/`omitzero` fields become optional (`?`);
  - `uint32` becomes `number`;
  - a Go `any` becomes a TypeScript `any`, so every struct field that holds one has a `tstype` tag naming a checked
    type: `tstype:"{ [key: string]: unknown }"` on the `Params` maps, and `tstype:"unknown"` on `Spec.Payload` and
    `Spec.Result` (tygo emits the Go-only registry `Spec` too, although it is never on the wire). tygo reads the tag
    up to its first comma as the type and the rest as options (`,extends`, `,required`), so the type is written
    without a comma (an index signature, not `Record<string, unknown>`). The generated files contain no `any`;
    `TestNoTSAny` checks the tags of `internal/protocol` and `internal/protocol/api`.

  Enum const groups need at least 2 members and names prefixed by the type name; anything else stays a plain
  `string` in TS. `CodecKey` uses single-line consts on purpose: it must stay an open `string`.
- Registry generator: `internal/protocol/gen/tsregistry` (package main) writes `web/src/protocol/registry.gen.ts` from
  `protocol.Registry` (§15.1): `ClientRequests`, `ClientNotifications`, `ServerMessages`, `ServerEnvelope`, and runtime
  arrays of message types.
- Task: `task gen` runs `.bin/tygo generate` (tygo v0.2.21, pinned in 06's separate `tools/go.mod` and built into
  `.bin/` by `task tools`) and `go run ./internal/protocol/gen/tsregistry -o web/src/protocol/registry.gen.ts`.
  - The separate tools module keeps tygo's 2022 `golang.org/x/tools` out of the server's module graph (the reason
    this doc first proposed `go run pkg@version`).
  - tygo is MIT-licensed and build-time only.
- CI drift check (06's `protocol` job): `task gen:check`, which runs `task gen` and fails when
  `git status --porcelain -- 'web/src/protocol/*.gen.ts'` prints anything (hand-written files in that directory are
  not checked).

## 15. Go packages

### 15.1 `internal/protocol` (M1, stdlib only)

Files:
- `doc.go`: versions;
- `envelope.go`: `Envelope`, `MessageType` consts, encode/decode;
- `hello.go`, `room.go`, `share.go`, `pc.go`, `subscribe.go`, `quality.go`, `stats.go`, `notify.go`, `agent.go`,
  `errors.go`: the types above;
- `codec.go`: `CodecKey` helpers;
- `secret.go`: `Secret`, `SDP`;
- `validate.go`;
- `registry.go`.

No dependency on Pion or any server package: `internal/client` and the TS generator both use it.

```go
// MessageType constants: MessageTypeHello = "hello", MessageTypeWelcome = "welcome", MessageTypeOK = "ok",
// MessageTypeError = "error", MessageTypePing, MessageTypePong, MessageTypeRoomJoin = "room.join", ... one per §7 row.
type MessageType string

// ParseEnvelope parses one frame. Errors (-> bad_message): not a JSON object, missing/empty type,
// id/re longer than 32 bytes or with characters outside [A-Za-z0-9_-].
func ParseEnvelope(b []byte) (Envelope, error)

// Decode unmarshals e.Data into T (unknown fields ignored; absent data decodes as the zero value)
// and runs T's Validate method if it has one.
func Decode[T any](e Envelope) (T, error)

// Marshal encodes one message; data may be nil. Slices in data must be non-nil (tests enforce).
func Marshal(t MessageType, id, re string, data any) ([]byte, error)

// NewError builds an Error with the retryable flag from the catalog (§12.1) for code and scope.
func NewError(code ErrorCode, scope ErrorScope) Error

// Error implements the error interface, so MediaPeer and hub code can return a *protocol.Error.
func (e *Error) Error() string

// FieldError is returned by Validate methods; the hub maps it to bad_request with params {field, reason}.
type FieldError struct {
	Field  string // JSON path, e.g. "subs[3].video"
	Reason string // required|invalid|too_long|too_many|duplicate
}

func (e *FieldError) Error() string

// Validate methods exist on every client->server payload: Hello, RoomJoin, ShareStart, ShareUpdate, ShareStop,
// PCOffer, PCAnswer, PCICE, PCRestart, PCClose, SubscribeUpdate, CapsUpdate, ClientStats, StatsWatch, AgentSend.

// ParseH264CodecKey maps an H.264 fmtp line to its CodecKey ("" if not packetization-mode=1 or no profile-level-id).
func ParseH264CodecKey(fmtp string) CodecKey

const CodecOpus CodecKey = "opus"
const CodecH264ConstrainedBaseline CodecKey = "h264/42e0"
const CodecH264Baseline CodecKey = "h264/4200"
const CodecH264Main CodecKey = "h264/4d00"
const CodecH264ConstrainedHigh CodecKey = "h264/640c"
const CodecH264High CodecKey = "h264/6400"

type Direction uint8

const (
	DirClientToServer Direction = 1 << iota
	DirServerToClient
)

type MsgKind uint8

const (
	KindRequest MsgKind = iota + 1
	KindReply
	KindNotification
)

// Spec describes one message type (and direction); Registry is the single list used by the hub's dispatcher,
// the tests (every Spec has a fixture) and the TS generator. The tstype tags keep any out of types.gen.ts (§14.4).
type Spec struct {
	Type      MessageType
	Dir       Direction
	Kind      MsgKind
	Payload   any         `tstype:"unknown"` // zero value of the payload type, e.g. RoomJoin{}
	Reply     MessageType // requests: MessageTypeOK, or MessageTypeWelcome for hello
	Result    any         `tstype:"unknown"` // requests: zero value of the reply payload (Empty{} if none)
	Roles     []Role      // client->server: allowed roles; nil = all
	Feature   Feature     // "" = baseline
	Since     int         // protocol version that introduced it
	Milestone string      // "M1", "M2", ... (docs)
}

var Registry []Spec

func Lookup(t MessageType, d Direction) (Spec, bool)
```

### 15.2 `internal/server/signal` (M1)

Files:
- `hub.go`: `Hub`, `New`, `Config`, `Deps`;
- `upgrade.go`;
- `conn.go`: the connection actor and its reader and writer;
- `dispatch.go`;
- `room.go`: participants, snapshots, coalescing;
- `share.go`;
- `relay.go`: the same-user relay, `agent.send` and `agent.recv` (§8.14; README S51);
- `resume.go`;
- `ratelimit.go`;
- `stats.go`: `stats` and `stats.watch` in both directions (§8.11);
- `metrics.go`: the hub's series (§18); `semver.go`: the comparison behind `Policy.MinClientVersion` (§6.1);
- `interfaces.go`;
- `snapshot.go`;
- `signaltest/`: a fake `MediaPlane`, `Authenticator` and `RoomDirectory` for tests (also used by 02 and 04).

**The server before the SFU is wired** (README S54). The first wiring slice did not put `signaltest`'s fake
`MediaPlane` into `serve`, which would have made a test package part of the binary. `internal/server/wire.go`
has its own placeholder, `noMedia`, until README S59 passes `sfuplane`'s `Plane` instead. With it signaling is complete
(connections join rooms, see each other and resume) and everything that needs media is refused:

| `MediaPeer` method | `noMedia` |
|---|---|
| `CreateShare`, `UpdateShare` | `error{feature_disabled, scope: request}`: `share.start` and `share.update` fail |
| `HandleOffer`, `HandleAnswer`, `AddICE`, `Restart` | `error{feature_disabled, scope: pc}` with the message's `pc`, `gen` and `neg` |
| `Subscribe` | every share is `ignored`: without media no share exists |
| `EndShare`, `ClosePC`, `SetCaps`, `Resync`, `Close` | nothing |
| `Stats` | an empty `ServerStats` (`subs: []`, `layers: []`) |

It keeps no state. `feature_disabled` is the catalog's "hide the feature" (§12.1), which is what a client should do
with sharing on such a server. No release runs this way: the placeholder exists only between README S54 and S59.

**Concurrency model:**
- Each connection is one actor goroutine. It owns the connection state and handles, in order: parsed client messages
  (from the reader goroutine), MediaSink events, timers and control calls (revoke, kick).
- The writer goroutine drains the send queue.
- Room state is guarded by a per-room mutex, held only for short sections. Lock order: hub → room → connection.
- **No lock is held while calling a `MediaPeer`.**
- `MediaSink` methods never block: they enqueue to the actor. If the actor's 1024-slot inbox overflows, the connection
  is closed as `slow_connection` (`4503`) for good, without grace: the dropped call is lost (a share's media state,
  the close of its room), so a resume would bring back a connection in a wrong state. Its shares end with
  `disconnected`, and no resume token resumes it meanwhile.
  - A revocation is never lost this way: `CloseConnections` marks the connection before it posts to the inbox, and
    the actor applies the mark when its socket ends or a post was dropped.
  - The reader goroutine waits for room in the inbox instead of dropping (a flooding client slows itself down).
  - `Shutdown` reaches the actors through a channel they all select on, not through the inbox, so a detached
    connection can't hold it up for its grace. It is therefore not ordered after calls already queued in an inbox.

```go
package signal

type Config struct {
	PublicOrigin      string              // 04's Site.Origin, e.g. "https://watch.example.com"
	AllowedOrigins    []string            // extra exact origins: none in M1 (no config key); M2 adds Wails origins in code
	ServerVersion     string              // internal/version.Version() (04)
	ResumeKey         []byte              // 32 bytes, secrets.json keys.resume (04 config.KeyResume)
	ICEServers        []protocol.ICEServer // M1: nil (sent as [])
	Grace             time.Duration       // 30s
	HelloTimeout      time.Duration       // 10s
	IdleTimeout       time.Duration       // 45s
	PingInterval      time.Duration       // 15s (advertised; also the hub's WebSocket ping-frame interval, §3.4)
	StateCoalesce     time.Duration       // 200ms
	ShareStartTimeout time.Duration       // 30s
	StalledTimeout    time.Duration       // 30s
	RevalidateEvery   time.Duration       // 5m (03 Touch; also refreshes the session's last-seen time)
	Limits            Limits
}

// Limits are guards (constants), not admin policy. Only PreAuthPerIPPerMinute has a config key
// (04: limits.ws_handshakes_per_ip_per_minute).
type Limits struct {
	MaxSharesPerUser      int   // 4
	MaxConnectionsPerUser int   // 16
	PreAuthPerIPPerMinute int   // 20
	MaxPreAuthConns       int   // 500
	MessagesPerSecond     int   // 20
	MessageBurst          int   // 100
	BytesPerSecond        int   // 512 << 10
	BytesBurst            int   // 1 << 20
	SendQueueMessages     int   // 512
	SendQueueBytes        int   // 8 << 20
}

// DefaultConfig returns the values in the comments above; tests shorten the durations.
func DefaultConfig() Config

type Deps struct {
	Auth     Authenticator
	Rooms    RoomDirectory
	Media    MediaPlane
	Policy   func() Policy                  // admin soft limits and client floor, read on every use (wiring: 03 settings)
	Push     PushNotifier                   // optional (nil: no Web Push)
	ClientIP func(*http.Request) netip.Addr // 04 httpapi.ClientIP: honors trusted proxies
	Log      *slog.Logger
	Metrics  prometheus.Registerer          // optional (nil: no metrics); 04's ops.Metrics.Registerer()
}

// Policy is admin policy from 03's SettingsCache (pinnable by 04's config keys). Changes apply to the next
// room.join / share.start / hello; nothing already running is torn down.
type Policy struct {
	MinClientVersion    string // "" = none (setting minClientVersion)
	MaxRoomParticipants int    // 0 = none (setting maxParticipantsPerRoom)
	MaxRoomShares       int    // 0 = none (setting maxSharesPerRoom)
	MaxVideoBitrate     int64  // bit/s per share, 0 = preset default (setting maxShareBitrateKbps × 1000)
}

func New(cfg Config, deps Deps) (*Hub, error)

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) // mounted at /ws by 04
func (h *Hub) Ready() bool                                      // for /readyz (04)
// Shutdown sends server.shutdown{reason, reconnectInMs} and error{server_shutdown, scope connection, retryable}
// to every connection, closes the sockets with 1012 and returns when they are closed or ctx ends (§11.6).
// 04 maps its own reasons: stop → stop; restart and restore → restart.
func (h *Hub) Shutdown(ctx context.Context, reason protocol.ShutdownReason) error

// Called through the wiring adapters (04 §6.6) by 03's REST handlers and admin actions:
func (h *Hub) Notify(t Target, topics ...protocol.Topic)
// CloseConnections closes every matching connection with error{code, scope session} and 4401 (4403 for
// account_disabled). code is ErrorCodeSessionRevoked or ErrorCodeAccountDisabled. Returns the number closed.
// The wiring implements 03's auth.ConnCloser with it (reason → code table in §15.4).
func (h *Hub) CloseConnections(sel ConnSelector, code protocol.ErrorCode) int
func (h *Hub) UpdateUser(userID, name string, admin bool) // rename or role change: refresh room.state names, Identity
// CloseRoom → room_closed (scope room); clients rejoin defaultRoomId. It also records roomID as closed for the
// process lifetime; a room.join that passed GetRoom before the delete gets room_not_found.
func (h *Hub) CloseRoom(roomID string)
func (h *Hub) Snapshot() LiveSnapshot                     // presence counts (03), admin dashboard (04)
// later: func (h *Hub) Kick(roomID, userID string) error  // → kicked (scope room) + rejoin block

// ConnSelector selects connections by user, session or device (same fields as 03's auth.ConnSelector).
// UserID is required: an empty UserID matches nothing (ERROR log, returns 0). The other fields narrow within that user.
type ConnSelector struct {
	UserID          string
	SessionID       string // "" = any
	DeviceID        string // "" = any
	ExceptSessionID string // keep this one ("sign out other browsers", password change)
}

// Target selects connections for Notify.
type Target struct {
	UserID string // "" = not by user
	RoomID string // "" = not by room
	Admins bool   // all connections of admin users
	All    bool   // every connection
}

type Identity struct {
	UserID    string
	Name      string
	Admin     bool
	SessionID string // cookie sessions
	DeviceID  string // bearer (native apps)
}

// Authenticator is implemented by a wiring adapter over 03's auth.Service (04 §6.6):
// AuthenticateRequest → auth.AuthenticateCookie(r) (session cookie only; Authorization headers are ignored on /ws),
// Revalidate → auth.Touch + a user re-read, AuthenticateBearer → M2 device tokens (M1: always ErrInvalid).
type Authenticator interface {
	// AuthenticateRequest reads the session cookie. ErrNoCredentials if absent; ErrInvalid if bad or expired;
	// other errors are transient (the hub answers 503).
	AuthenticateRequest(r *http.Request) (Identity, error)
	// AuthenticateBearer validates a device access token (device flow, 03, M2).
	AuthenticateBearer(ctx context.Context, token protocol.Secret) (Identity, error)
	// Revalidate reports whether the session/device behind id is still valid and marks it as seen (03 Touch);
	// the result can update Name/Admin. Called at connect and every RevalidateEvery.
	// Returns ErrInvalid only when the session/device is gone or expired or the user is not active (the hub then
	// closes with session_revoked). Any other error is transient: the hub keeps the connection and retries at the
	// next RevalidateEvery.
	Revalidate(ctx context.Context, id Identity, ip netip.Addr) (Identity, error)
}

var (
	ErrNoCredentials = errors.New("signal: no credentials")
	ErrInvalid       = errors.New("signal: invalid credentials")
	ErrNotFound      = errors.New("signal: not found")
)

// RoomDirectory is implemented by a wiring adapter over 03's store (RoomByID; DefaultRoomID = store.DefaultRoomID).
type RoomDirectory interface {
	GetRoom(ctx context.Context, roomID string) (protocol.RoomInfo, error) // ErrNotFound
	DefaultRoomID(ctx context.Context) (string, error)
	CanJoin(ctx context.Context, id Identity, roomID string) error // M1: always nil; room locks later
}

// PushNotifier is implemented by internal/server/push (04), through a wiring adapter. Non-blocking.
// The hub calls it once per share, on starting → live, unless the share has `replaces`.
type PushNotifier interface {
	ShareStarted(ctx context.Context, ev PushShareStarted)
}

type PushShareStarted struct {
	RoomID   string
	// RoomName is read with GetRoom when the event is built; the hub never caches room names, so renames (03 §8)
	// need no hook. When that read fails, ShareStarted is not called at all (§8.6).
	RoomName string
	ShareID          string
	UserID, UserName string
	PresentUserIDs   []string  // participants in the room now (present or reconnecting): 04 skips them
	At               time.Time
}

// MediaPlane is the SFU as seen from signaling. Implemented by internal/server/sfuplane over 02's *sfu.SFU (§15.4).
type MediaPlane interface {
	// NewPeer creates the media side of one connection in one room. sink gets that peer's events.
	NewPeer(p PeerParams, sink MediaSink) (MediaPeer, error)
}

type PeerParams struct {
	ConnectionID string
	UserID       string
	RoomID       string
	Role         protocol.Role
	Caps         protocol.Caps
	Client       protocol.ClientInfo
}

// MediaPeer is called only from the connection's actor goroutine, never concurrently for one peer.
type MediaPeer interface {
	CreateShare(shareID string, meta protocol.ShareStart) (protocol.ShareParams, error)
	UpdateShare(shareID string, meta protocol.ShareUpdate) (protocol.ShareParams, error)
	EndShare(shareID string, reason protocol.EndReason)
	HandleOffer(o protocol.PCOffer) (protocol.PCAnswer, error)  // pub PC; errors are *protocol.Error (scope pc; §15.4)
	HandleAnswer(a protocol.PCAnswer) error                     // sub PC
	AddICE(c protocol.PCICE) error
	Restart(r protocol.PCRestart) error                         // sub: ICE restart or rebuild (client asked)
	ClosePC(c protocol.PCClose) error                           // pub only (the hub ignores pc.close{sub})
	Subscribe(wants []protocol.SubscriptionWant) (ignored []string, err error)
	SetCaps(protocol.Caps)
	Resync()                // §10.5: re-emit pending sub offer, statuses, hints; ICE-restart non-connected sub PC
	Stats() protocol.ServerStats
	Close()                 // closes both PCs; the hub has already ended the peer's shares
}

// MediaSink receives a peer's events. Implementations never block.
type MediaSink interface {
	Offer(o protocol.PCOffer)                   // sub PC offers
	ICE(c protocol.PCICE)
	// RestartRequest asks the client to rebuild its pub PC. The peer reports every failed pub PC; the hub passes
	// the request on only while the connection publishes a share (§9 rule 10).
	RestartRequest(r protocol.PCRestart)
	SubscriptionStatus(s []protocol.SubscriptionStatus)
	QualityHint(h protocol.QualityHint)
	ShareMedia(shareID string, ev ShareMediaEvent)
	Error(e protocol.Error)                     // scope pc or share
}

type ShareMediaEvent struct {
	Kind   ShareMediaKind
	Layers []protocol.VideoLayer // layers currently received
	Codec  protocol.CodecKey
	Audio  bool                  // an audio track is being received
}

type ShareMediaKind uint8

const (
	ShareMediaLive    ShareMediaKind = iota + 1 // first keyframe, or recovered after stalled
	ShareMediaStalled                           // pub PC not connected for 2 s, or its tracks are gone (PC rebuilt)
	ShareMediaChanged                           // layers/codec/audio changed while live
	ShareMediaGone                              // reserved: the hub itself ends a share whose tracks a pub offer of
	                                            // any gen no longer lists (§8.7); the SFU never emits it in M1
)

// LiveSnapshot feeds the admin dashboard (03/04 expose it over REST).
type LiveSnapshot struct {
	Rooms []LiveRoom
}

type LiveRoom struct {
	ID, Name     string
	Participants []LiveParticipant
	Shares       []LiveShare
}

type LiveParticipant struct {
	UserID, Name string
	Connections  []LiveConnection
}

type LiveConnection struct {
	ID        string
	Kind      protocol.ClientKind
	Role      protocol.Role
	OS        protocol.ClientOS
	Browser   string
	Version   string
	Status    protocol.ConnectionStatus
	Since     time.Time
	LastStats *protocol.ClientStats
}

type LiveShare struct {
	Info          protocol.ShareInfo
	Layers        []protocol.ServerLayerStats
	EgressBitrate int64 // sum over its subscribers (from MediaPeer.Stats)
}
```

### 15.3 `internal/client/signal` (M1: tests and `isshoni-loadtest`; M2+: native apps)

Pure Go on coder/websocket; imports only `internal/protocol` and the stdlib, so it stays gomobile-friendly.
coder/websocket is pure Go; this refines the plan's "x/net" note. It implements §10.1–§10.3 and §10.5 (signaling part
only). PC handling stays with the caller.

```go
package signal

type Options struct {
	URL         string                                    // "wss://example.com/ws" (ws: against a dev server)
	Header      http.Header                               // on every upgrade: Cookie for kind tool; empty for bearer
	Token       func(ctx context.Context) (protocol.Secret, error) // bearer (M2+); nil = cookie auth
	Client      protocol.ClientInfo
	Role        protocol.Role
	Caps        protocol.Caps                             // the first hello's; then those of the last caps.update sent
	Features    []protocol.Feature
	HTTPClient  *http.Client                              // nil = http.DefaultClient
	Logger      *slog.Logger                              // nil discards the log
	NoReconnect bool                                      // tests: stop where the client would back off
}

type State uint8

const (
	StateConnecting State = iota + 1
	StateHandshaking
	StateReady
	StateBackoff
	StateStopped
)

func (s State) String() string // the names of §10.1: "connecting" … "stopped"

type StateChange struct {
	State   State
	Resumed bool            // StateReady: welcome.resumed
	Err     *protocol.Error // StateBackoff, StateStopped: why, when the server said so (its error, or the one its
	                        // close code stands for, §12.2); nil for a plain drop, a timeout of the client's and Close
	Delay   time.Duration   // StateBackoff: the wait before the next attempt
}

// The client-local errors of §12.4. Request and Send return them wrapped: test with errors.Is.
var (
	ErrConnectionLost = errors.New("signal: connection lost")   // connection_lost
	ErrRequestTimeout = errors.New("signal: request timed out") // request_timeout
	ErrNotReady       = errors.New("signal: client stopped")    // not_ready
)

// Timings (§3.4, §10.1–§10.2, §13), exported so that callers and tests name them.
const (
	RequestTimeout      = 10 * time.Second
	ConnectTimeout      = 10 * time.Second // one attempt's bearer token call and upgrade
	HandshakeTimeout    = 10 * time.Second
	DefaultPingInterval = 15 * time.Second // when welcome.limits.pingIntervalMs is missing
	PongTimeout         = 10 * time.Second
	ProbeTimeout        = 3 * time.Second
	BackoffMin          = 500 * time.Millisecond
	BackoffMax          = 10 * time.Second
	BackoffReset        = 10 * time.Second
	RateLimitMinWait    = 30 * time.Second
)

type Client struct{ /* … */ } // safe for concurrent use

// Dial connects, sends hello, and returns after the first welcome; reconnects in the background afterwards.
func Dial(ctx context.Context, o Options) (*Client, error)
func (c *Client) Welcome() protocol.Welcome
func (c *Client) Request(ctx context.Context, t protocol.MessageType, data, result any) error // *protocol.Error on error
func (c *Client) Send(t protocol.MessageType, data any) error
func (c *Client) Events() <-chan protocol.Envelope  // server notifications, in order; must be received (below)
func (c *Client) States() <-chan StateChange       // every state change, in order; never blocks the client
func (c *Client) Probe()                            // an immediate ping with the 3 s timeout (§3.4)
func (c *Client) RetryNow()                         // skip the current backoff wait once (§10.2)
func (c *Client) Close() error                      // close 1000: the server skips grace
```

**The client as README S39 built it.** The block above is the package's whole exported surface. What the first
version of this section left open:

- **`Dial` retries.** A first attempt that fails (nothing listens yet, the upgrade is refused, no `welcome` within
  10 s) is retried with the backoff of §10.2 like any later one, until `ctx` ends; `Dial` then stops the client and
  returns `ctx`'s error together with the last attempt's. When the server answers with an error that reconnecting
  can't fix (scope `session`; scope `connection` or the answer to `hello`, unless `retryable`: `unauthenticated`,
  `protocol_unsupported`, `too_many_connections`, …), `Dial` returns at once with an error that wraps that
  `*protocol.Error` (`signal: dial: …`): read it with `errors.As`, a type assertion does not find it. With
  `Options.NoReconnect` the first failure of any kind is returned, wrapped the same way. `ctx` bounds `Dial` only:
  the client keeps `ctx`'s values and outlives its cancellation, until `Close`.
- **`Options.Token`** is called before every `hello`, the resuming ones too, with a context that ends after
  `ConnectTimeout`: the place to refresh an access token that is about to expire (§3.2). An error from it fails that
  attempt like a failed connect. The token goes nowhere but into `hello.auth`. An `unauthenticated` answer stops the
  client like every error in scope `session`; §3.2's "refresh once and retry" is the app's, which dials again as a
  new connection. The M1 server answers every bearer `hello` with `unauthenticated`.
- **`Options.Caps` are the first `hello`'s only.** `Send` checks a `caps.update` the way the server does (one the
  server would refuse is a `*protocol.FieldError` and is not sent) and keeps its caps: they go into every later
  `hello`, because a resume replaces the connection's caps with the `hello`'s (§10.3). The options are copied at
  `Dial`.
- **`Request`** takes requests only (a notification type is an error: no reply would come), `Send` notifications
  only, and neither takes `hello`. A type this build doesn't know is sent: a newer server may know it. The error is
  the server's `*protocol.Error` itself, not wrapped (unlike `Dial`'s), or it wraps a sentinel: `ErrConnectionLost`
  while the client is connecting or backing off, or when the socket went away before the reply;
  `ErrRequestTimeout` after `RequestTimeout`; `ErrNotReady` once the client is stopped (only a new `Dial` helps);
  or `ctx`'s error, which ends the wait and never the socket. Nothing is queued while the client is not ready, and
  nothing is retried: after `ErrConnectionLost` or `ErrRequestTimeout` the caller can't tell whether the server
  acted, and applies §10.5 after the next `welcome`.
- **`Events` must be received: a caller that doesn't drain it stops the client.** The channel carries every server
  notification in the order it arrived (`room.state`, `room.event`, `pc.*`, `subscribe.status`, `quality.hint`,
  `stats`, `invalidate`, `server.shutdown`, `agent.recv`, and the `error` messages that answer no request: scopes
  `share`, `pc` and `room`, and scopes this build doesn't know). It holds 256 of them. When it is full the client
  stops reading its socket, replies and pongs included, like any slow consumer: requests time out, and once a ping
  has gone `PongTimeout` without its pong being read, the client drops the socket and resumes on a new one. What
  the old socket had not handed over is lost. A caller that doesn't care about notifications drains the channel in
  a goroutine. Replies never come here, nor do errors in scope `connection` or `session` (`States` reports those),
  nor message types this build doesn't know (§8.13).
- **`States` never blocks the client.** It holds 64 changes; when nobody receives, the oldest are dropped. When
  `Dial` returns, the changes up to the first `ready` are waiting in it. One guarantee ties the two channels: when a
  `welcome` does not resume the connection (`StateReady` with `Resumed` false after the first one), the
  notifications of the old connection that nobody has received yet are dropped before that state is reported, so a
  notification received after it belongs to the new connection, never to PCs and shares that are gone.
- **`Probe` and `RetryNow`** are for what the caller knows and the client doesn't. `Probe` sends a ping at once with
  `ProbeTimeout`; without a pong the client drops the socket and reconnects (a caller whose PC became `disconnected`
  calls it, §3.4). It does nothing unless the client is `ready`. `RetryNow` skips the current backoff wait once (the
  network is back, or a user asked, §10.2); it does nothing unless the client is in `backoff`, and it never shortens
  a rate-limit wait.
- **Backoff as built** (§10.2): after `rate_limited` in scope `connection`, or close `4429`, the wait is
  `max(RateLimitMinWait, retryAfterMs)`; after `server.shutdown` the next wait is exactly `reconnectInMs`, and the
  sequence then goes on from where it was. A wait the server dictates is capped at 5 min, and a
  `limits.pingIntervalMs` below 1 s is read as 1 s. A close code without a preceding error is mapped like the web
  client does (§12.2): `1003` and `4400` → `bad_message`, `4401` → `unauthenticated`, `4403` → `forbidden`, `4409` →
  `replaced`, `4426` → `protocol_unsupported` (all stop), `4429` → `rate_limited`; every other code backs off.
- **`Close`** closes the socket with `1000`, so the server ends the connection without its grace period (§4.2), and
  returns when the client's goroutine has ended and both channels are closed; pending requests fail with
  `ErrConnectionLost`. Its error is non-nil when the server did not answer the close handshake (coder/websocket
  allows 5 s to send the close frame and 5 s for the answer): the server then keeps the connection for its grace
  period. Without a socket (connecting, backing off, stopped) it returns at once. Later calls return the same result.
- The client reads server messages of up to 1 MiB and logs no token, SDP or payload.

### 15.4 `internal/server/sfuplane` (M1): `MediaPlane` over 02's SFU

02 keeps its own Go types and never imports `signal` or `protocol` (02 §2). This package is the only place that knows
both. It is mapping and nothing else, and table tests cover every row below (README S50): `plane.go` (the `Plane`,
its peers and the `RoomEvents`), `peer.go` (the calls and the events), `convert.go` (the values) and `errors.go`
(the error table). The tests read the SFU's declared error codes, end reasons, subscription reasons, profiles and
event types from its source, so a new one without a row here fails them.

```go
package sfuplane // imports internal/protocol, internal/server/signal, internal/server/sfu

// New returns the MediaPlane for the hub and the RoomEvents for the SFU. The wiring (04 §6.6) passes
// events as sfu.Deps.Events before calling sfu.New, then calls Bind with the SFU.
func New(log *slog.Logger) (*Plane, sfu.RoomEvents)
func (p *Plane) Bind(s *sfu.SFU)
func (p *Plane) NewPeer(pp signal.PeerParams, sink signal.MediaSink) (signal.MediaPeer, error)
```

`NewPeer` calls `SFU.Join(sfu.JoinParams{Room: pp.RoomID, Participant: pp.UserID, User: pp.UserID, Conn:
pp.ConnectionID, Role, Client, Decode, Signaler: peer})`. The peer implements `sfu.Signaler` by converting to
`MediaSink` calls. It lives as long as the Connection (resumes included), because the hub keeps the sink per
Connection.

**Calls** (`MediaPeer` → `*sfu.Conn`; every call gets a 5 s context):

| `signal.MediaPeer` | `*sfu.Conn` | Notes |
|---|---|---|
| `CreateShare(id, meta)` | `StartShare(ctx, StartShareParams{ID: id, Preset, Audio, Source: meta.Kind})` → `sfu.ShareParams` | converted to `protocol.ShareParams` (`Codec = "h264/" + Profile`, `Layer` from the rid) |
| `UpdateShare(id, u)` | `UpdateShare(ctx, id, ShareUpdate{Preset})` → `sfu.ShareParams` | label changes never reach the SFU |
| `EndShare(id, reason)` | `StopShare(ctx, id, reason)` | the hub always calls the publishing connection's peer |
| `HandleOffer(o)` | `HandleOffer(ctx, PCPub, o.Gen, o.Neg, o.SDP, tracks)` → answer SDP | synchronous (gathering is instant with muxes, capped at 2 s); a higher `gen` replaces the pub PC; a lower `gen`, or a lower `neg` in the current `gen`, returns `sfu.stale_offer`. Before the call the hub drops TrackRefs of shares that aren't `starting`/`live`/`stalled` shares of this connection (§9 rule 4) and ends bound shares that `tracks` omit (§8.7) |
| `HandleAnswer(a)` | `HandleAnswer(ctx, PCSub, a.Gen, a.Neg, a.SDP)` | a stale `gen`/`neg` is dropped silently |
| `AddICE(c)` | `AddICECandidate(ctx, c.PC, c.Gen, init)` | end-of-candidates is ignored |
| `Restart(r)` | `mode: ice` → `RestartICE(ctx, PCSub, r.Gen)`; `mode: rebuild` → `ResetPC(ctx, PCSub, r.Gen)` | pub restarts are client offers; a lower `gen` than the current one does nothing and returns nil; `RestartICE` also returns nil with no new offer while a sub ICE restart is under way (§10.4, 02 §5.3) |
| `ClosePC(c)` | `ClosePC(ctx, c.PC, c.Gen)` | the hub calls it only for `pub` and has already ended the shares of the closed pub PC; a lower `gen` does nothing and returns nil |
| `Subscribe(wants)` | `UpdateSubscriptions(ctx, items)` | items failing with `sfu.share_not_found` become `ignored`; other per-item errors: below |
| `SetCaps(c)` | `SetDecodeCaps(ctx, DecodeCaps{H264: profiles})` | `"h264/6400"` → `"6400"`; non-H.264 keys dropped |
| `Resync()` | `Resync()` | |
| `Stats()` | `Stats()` → `protocol.ServerStats` | `subs`: one entry per track forwarded now (the video with the layer of its rid, and the audio); `layers`: every attached video layer of the connection's own shares, and their audio |
| `Close()` | `Close(protocol.EndReasonLeft)` | |

**Events** (SFU → `MediaSink`):

| SFU | `MediaSink` |
|---|---|
| `Signaler.SendOffer(PCSub, gen, neg, sdp, tracks)` | `Offer(PCOffer{pc: sub, gen, neg, sdp, tracks})` |
| `SubscriptionStateEvent` | `SubscriptionStatus` (reason map below) |
| `CodecPolicyEvent{Share, Profile}` (one per share this connection publishes) | `QualityHint{shareId, reason: codec, codec: "h264/"+Profile}` with no encodings (a profile switch doesn't change them) |
| `QualityHintEvent{Share, Reason, MaxBitrate, Encodings}` | `QualityHint{shareId, reason: ev.Reason, maxBitrate, encodings converted 1:1}`; `Reason` is `admin` or `viewers`, and `Encodings` is the full current `f`/`q` list with the cap and pause state applied by the SFU |
| `PCStateEvent{PCPub, Gen, failed}` | `RestartRequest{pc: pub, gen: ev.Gen, mode: rebuild, reason: failed}` (the SFU emits PCStateEvents only for the current PC of each kind). `sfuplane` passes every such event on. From README F03 on the hub's sink drops the request unless the connection has a share that has not ended (§9 rule 10, decided after group 6) |
| `PCStateEvent{PCSub, …}` | nothing: the client drives sub restarts (§10.4); the SFU ICE-restarts in `Resync()` and rebuilds sub PCs itself only for codec retries |
| `ErrorEvent` | `Error` (code map below) with the event's scope: `pc.pub`/`pc.sub` → scope `pc` with that `pc` (an event has no `gen` or `neg`); `share`/`subscription` → that scope with `shareId` = `Error.Share` |
| `RoomEvents.ShareUpdated` | `ShareMedia` on the publishing connection's sink: `pending→live` or `stalled→live` = `Live`; `→stalled` = `Stalled`; a layer attached or ended, the profile changed, or audio changed while live = `Changed`. `Layers` is built from `ShareInfo.Layers` (every layer whose track is attached, paused ones included), ignoring `LayerInfo.Active` |
| `RoomEvents.ShareEnded`, `CodecPolicyChanged` | ignored: the hub ends shares itself; per-connection `CodecPolicyEvent`s cover publishers |

The SFU computes the content of every hint; `sfuplane` only converts types and keeps no encoding or share-list state.
So a failed pub PC is passed on whatever it carried: whether a share still needs it is the hub's to say (§9 rule 10).
On `Resync()` the SFU re-sends one `CodecPolicyEvent` per share and the last `QualityHintEvent` per share, if one was
sent. The one thing `sfuplane` remembers is the state the SFU last reported for each share (`ShareUpdated` carries
the share as it is now, not what changed): that tells `Live` from `Changed`, and is forgotten on `ShareEnded`.

**Values**: `ShareKind` screen/window/tab ↔ `SourceScreen/Window/Tab`; `VideoLayer` high/low/off ↔
`QualityHigh/Low/Off`; `AudioState` on ↔ `true`; `EndReason` strings are identical in both packages; `Role`,
`ClientKind` and `PCKind` map by name. The SFU has no `tool` client kind: `tool`, and any kind the server doesn't
know, becomes `ClientWeb`, as the hub treats them (§8.2).

**Subscription reasons** (`sfu.SubReason` → `protocol.StatusReason`): `""` while forwarded < requested → `waiting`;
`bandwidth`, `server_limit` → `bandwidth`; `codec_mismatch`, `decoder_unavailable`, `decoder_failed` → `codec`;
`no_preview_layer`, `no_layer` → `unavailable`. A status that forwards what was requested has no reason (§8.9).

**Error codes** (`sfu.*` never goes on the wire):

Errors from the six PC methods (`HandleOffer`, `HandleAnswer`, `AddICECandidate`, `RestartICE`, `ResetPC`, `ClosePC`)
go out with scope `pc` plus the call's `pc`, `gen` and `neg`, keeping the mapped code below. The one exception is
`sfu.no_h264`. Wherever `sfu.Error.RetryAfter` is set (`pc_rate_limited`, `busy`, `probe_limit`), `retryAfterMs` =
`Error.RetryAfter`.

| `sfu` code | Wire |
|---|---|
| `sfu.role_forbidden`, `sfu.not_owner` | `forbidden` (scope request; pc from the PC methods) |
| `sfu.bad_pc`, `sfu.pc_limit` | `bad_request`, `params {field: "pc", reason: "invalid"}` for `bad_pc` and `{field: "pc", reason: "too_many"}` for `pc_limit` (scope request; pc from the PC methods) |
| `sfu.unknown_track` | `bad_request`, `params {field: "tracks", reason: "invalid"}` (scope pc). Only for a malformed binding (02 §6.3); an m-section of a share that ended in a race is answered `a=inactive`, with no error (§9 rule 4) |
| `sfu.too_many_subscriptions` | `bad_request`, `params {field: "subs", reason: "too_many"}` |
| `sfu.bad_sdp`, `sfu.bad_rid` | `sdp_invalid` (scope `pc`, with `pc`, `gen`, `neg`) |
| `sfu.stale_offer` | `stale_negotiation` (scope `pc`, with `pc`, `gen`, `neg`); the client ignores it |
| `sfu.no_h264` | `codec_not_supported` (scope `share`, `shareId` = `Error.Share`); the hub then ends that share with `stopped` |
| `sfu.pc_rate_limited` | `rate_limited` (scope `pc`, `retryAfterMs` = `Error.RetryAfter`) |
| `sfu.share_not_found` | `share_not_found` (or `ignored` in `subscribe.update`) |
| `sfu.too_many_shares` | `share_limit`, `params {limit: 4, per: "user"}` (the SFU's guard of 4 per participant, 02 §12) |
| `sfu.busy`, `sfu.internal` | `internal` (scope request, or pc from the PC methods) with `params {ref}`: `sfuplane` makes the 8-char id and logs it with the SFU's error (§12.1). `retryable` = `Error.Retryable`, not the catalog's flag: yes for `sfu.busy` and for an unexpected Pion error; **no** for the two kinds of `sfu.internal` that a second try can't help (02 §6.3): a call that breaks the SFU's contract (a bug in the hub or in `sfuplane`, never something a client sent: the hub validates every payload first) and a method that a later slice implements. Logged at error, `sfu.busy` at warn. An `sfu.*` code that `sfuplane` has no row for is mapped the same way |
| `sfu.closed` | `not_in_room` on `CreateShare`/`UpdateShare`/`Subscribe`, and on `HandleOffer` (scope `pc`), whose client waits for an answer; nothing on the other notification paths; logged at debug |
| `sfu.stale_answer` | nothing is sent; logged at debug |
| `sfu.probe_limit` | not signaling: 04's `/api/v1/conntest` answers 429 `rate_limited` |

In `subscribe.update`, a per-item error other than `sfu.share_not_found` fails the whole request with the mapped
code; the items before it stay applied.

**03 revocation reasons** (the wiring's `auth.ConnCloser` adapter): `account_disabled` → `ErrorCodeAccountDisabled`;
every other reason (`logged_out`, `session_revoked`, `password_changed`, `password_reset`, `account_deleted`,
`device_revoked`) → `ErrorCodeSessionRevoked`. Session expiry is not pushed; `Revalidate` finds it (§3.2).

## 16. TypeScript: `web/src/protocol/` (M1)

| File | Content | Owner |
|---|---|---|
| `types.gen.ts` | tygo output | generated |
| `api.gen.ts` | tygo output of `internal/protocol/api` (03's and 04's REST DTOs, §14.4) | generated |
| `registry.gen.ts` | message maps and type arrays | generated |
| `codecs.ts` | `detectCaps(): Caps` (getCapabilities → `CodecKey`, H.264 packetization-mode=1 only), `h264Key(fmtp)` | this doc |
| `signal-client.ts` | `SignalClient` (§10.1–§10.3, §10.5 signaling part) | this doc |
| `errors.ts` | `ProtocolError` (`code`, `scope`, `retryable`, `params`, `local`, plus `retryAfterMs`, `shareId`, `roomId`, `pc`, `gen`, `neg`, `closeCode`; `fromWire`, `local`, `fromCloseCode`) | this doc |
| `index.ts` | re-exports of this doc's files | this doc |
| `rest.ts`, `queryKeys.ts`, `invalidate.ts` | REST client, query keys, `invalidate` topic mapping | 05 |

PC management (`RTCPeerConnection`, simulcast, `setCodecPreferences`, stereo munging) lives in 05's
`src/platform`, following §9 and §10.4.

```ts
export type SignalState = 'connecting' | 'handshaking' | 'ready' | 'backoff' | 'stopped';

export interface SignalClientOptions {
  url: string;                                // new URL('/ws', location.href) with ws(s):
  client: ClientInfo;
  role: Role;
  caps: () => Caps;                           // re-read on every (re)connect
  features?: Feature[];
  // after every welcome (the first after start() included, resumed or not). state is already 'ready'
  // (request/notify work); onState('ready') listeners fire right after it returns; a returned promise is not
  // awaited, its rejection is logged
  onResync?: (w: Welcome) => void | Promise<void>;
  buildVersion?: string;                      // vs welcome.serverVersion; default BUILD_VERSION (tests set it)
}

export interface SignalStateInfo {
  resumed?: boolean;       // ready
  staleBuild?: boolean;    // ready
  error?: ProtocolError;   // backoff, stopped: the server's error, or the one its close code stands for
  shutdown?: ServerShutdown; // set from server.shutdown until ready
  delayMs?: number;        // backoff: the wait before the next attempt
  rateLimited?: boolean;   // backoff: a rate-limit wait that retryNow() doesn't shorten (05 disables Retry)
}

export class SignalClient {
  constructor(opts: SignalClientOptions);
  start(): void;
  stop(): void;       // closes with 1000; the server skips grace
  probe(): void;      // immediate ping with a 3 s pong timeout (§3.4); no-op unless ready
  readonly state: SignalState;
  readonly welcome: Welcome | undefined;
  // every client request but hello, which the client sends itself
  request<K extends Exclude<keyof ClientRequests, 'hello'>>(type: K, data: ClientRequests[K]['data'],
    opts?: { timeoutMs?: number }): Promise<ClientRequests[K]['result']>;  // rejects with ProtocolError
  notify<K extends keyof ClientNotifications>(type: K, data: ClientNotifications[K]): boolean; // false if not ready
  on<K extends keyof ServerMessages>(type: K,
    fn: (data: ServerMessages[K], env: ServerEnvelope<K>) => void): () => void;
  onState(fn: (s: SignalState, info: SignalStateInfo) => void): () => void;
  retryNow(): void;   // skip the current backoff wait, except a rate-limit wait (§10.2); the UI's Retry button
}
```

Behavior (normative):
- `ping`/`pong` per §3.4, and backoff with skip-wait per §10.2. The resume token is kept in memory.
- The client pings immediately on `visibilitychange → visible` and `online` by itself; 05 calls `probe()` when a PC
  becomes `disconnected`.
- On `welcome{serverVersion}` it reports `staleBuild: true` only when `serverVersion != BUILD_VERSION` and neither is
  a dev build (prerelease starts with `dev`, 04 §15). It reports it with the `ready` state, and 05 reloads (§6.1).
- It closes with `1000` on `pagehide` (browsers accept only 1000 or 3000–4999 in `WebSocket.close`; not
  `beforeunload`, which breaks the bfcache on some browsers), so the server skips grace. On `pageshow` with
  `persisted: true` it calls `start()` again (a new connection, `resumed: false`).
- `request()` never retries. It rejects with `ProtocolError`, and callers (05) apply §12.1's client action.
- An error of scope `connection` or `session` acts at once (§12.2: the error decides), without waiting for the close
  frame. A close without a preceding error becomes the error its code stands for (`ProtocolError.fromCloseCode`, with
  `closeCode` set and `local` false), so 05's handling by code gives §12.2's client action: 1003 and 4400 →
  `bad_message`; 4401 → `unauthenticated` (scope `session`); 4403 → `forbidden`; 4409 → `replaced`; 4426 →
  `protocol_unsupported` (empty params); 4429 → `rate_limited` (retryable, at least 30 s). Every other code, unknown
  ones included, is a plain backoff with no error.
- `connecting` has a 10 s connect timeout. When the client drops a socket itself to reconnect (a timeout, a
  connection error), it closes with `ReconnectCloseCode` 4000, so the server keeps the grace period and the next
  `hello` can resume.

## 17. Security and privacy (M1)

- **Cross-site WebSocket hijacking**: an exact Origin allowlist (step 2) plus SameSite=Lax cookies. A missing Origin
  is allowed only because it proves a non-browser client, which needs the cookie or token itself.
- **Pre-auth surface**: 64 KiB read limit, 10 s hello deadline, 20 per IP per minute, 500 global. Unknown resume
  tokens are rejected by HMAC before any map lookup.
- **Authorization**:
  - shares can only be changed by their user;
  - pub tracks must map to shares published by the same connection;
  - subscriptions only reach shares in the connection's room;
  - the relay only reaches the same user's connections;
  - roles gate message types.
- **ICE**: with full ICE, the server sends connectivity checks to whatever addresses clients put in `pc.ice`, so a
  client could make it probe internal networks. 02 keeps full ICE (S4 tested it) and drops remote candidates with
  loopback, link-local, multicast, unspecified or (unless the server itself uses a private address, 04's LAN case)
  private and CGNAT addresses (02 §7.3). The server still learns every client address it needs as a peer-reflexive
  candidate from incoming checks. The protocol carries candidates either way, so switching to ICE-lite later needs no
  change.
- **Logging** (README "Logging", 04 §10: no access logs):
  - `Secret` and `SDP` redact themselves (`slog.LogValuer`, `fmt.Formatter`);
  - the hub logs message types, codes and ids, never payloads, SDP, tokens or candidate addresses;
  - connect, resume and close lines carry `conn_id` and `user_id` only, never the client IP, so they do not add up
    to a per-user IP history;
  - `remote_ip` appears only on security events, logged at `warn`: a pre-auth `429` (step 4) and an Origin `403`
    (step 2), each at most once per client IP (IPv6: /64) per minute so a flood cannot flood the log. No `info`
    line carries a client IP, which keeps S90's log canary true;
  - the durable IP record is 03's (last IP per session, and the audit log with 30-day pruning). Security-event lines
    live as long as journald or Docker log rotation keeps them; 03 §11 says so for the privacy page.
- **Privacy** (plan):
  - no window titles (labels are user-typed, default empty);
  - no OS or version shown to other users;
  - no IPs in stats;
  - watchers include admins, and nothing can subscribe invisibly.

## 18. Observability (M1)

- **slog attributes**: README's names `conn_id`, `user_id`, `room_id`, `share_id`, plus `pc`, `gen`, `code`.
  `remote_ip` only on the §17 security events.
  - `info`: connect, detach (with the socket's close code), resume (`resumed=true|false`), close (with the last
    socket's code and the end reason), share start/end (with reason), kick, revocation. None of them carries
    `remote_ip`.
  - `debug`: every message type in and out; a resume token that resumed nothing, with the reason and never the
    token.
- **Prometheus** (only when `Deps.Metrics` is set, 04):
  - `isshoni_ws_connections{kind,role}` gauge: connections, detached ones included;
  - `isshoni_ws_messages_total{type,dir}`;
  - `isshoni_ws_errors_total{code}`;
  - `isshoni_ws_resume_total{result="resumed|not_resumed"}`;
  - `isshoni_ws_close_total{code}`: closed sockets, so a resumed connection counts once per socket (`4409` for a
    replaced one);
  - `isshoni_rooms`, `isshoni_participants`, `isshoni_shares{status}` (01 owns these three; 02 and 04 don't
    register them);
  - from client `stats` deltas (§8.11), for the exit test: `isshoni_client_frames_decoded_total`,
    `isshoni_client_frames_dropped_total`, `isshoni_client_freeze_seconds_total`,
    `isshoni_client_packets_lost_total{kind}`, `isshoni_client_audio_samples_total` and
    `isshoni_client_audio_concealed_samples_total`. No per-user labels.

## 19. Test plan

**Unit (Go, `go test -race`)**, in `internal/protocol`:
- **Round-trip**: every fixture in `testdata/v1` is decoded strictly (`DisallowUnknownFields`, so fixture typos
  fail) into its registered type, re-encoded, and compared as JSON trees.
- **Coverage**: every `Registry` entry has at least one fixture.
- **No `null`**: a reflection walk marshals a fully populated zero value of every payload and asserts no `null`
  arrays.
- **Tolerance**: extra unknown fields at every level decode fine. Unknown `type` → `Lookup` false.
- **Compat**: every `testdata/compat/*` fixture decodes, and re-encoding keeps each fixture key and value (§14.3).
- **Validation tables** per payload: label rules (length, Cc/Cf, trimming), id charset, `subs` duplicates and
  limits, tracks, candidate size, enum values.
- **Codecs**: `ParseH264CodecKey` for `42e01f`, `42001f`, `4d001f`, `640c1f`, `64001f`, `640034` (Chrome High →
  `h264/6400`), mode-0 lines (→ ""), mixed case.
- **Secrets**: `Secret`/`SDP` never appear via `%v`, `%+v`, `%#v` or slog (text and JSON handlers); JSON marshal is
  verbatim.
- **Fuzzing**: `FuzzParseEnvelope` and `FuzzDecode` (all registry types): no panics, no allocation above 4× the input.

In `internal/server/signal`, with `signaltest` fakes and `httptest`:
- **Upgrade** (the Origin matrix moved here from 03): bad Origin → 403; Origin that differs only in host case or an
  explicit default port → OK; missing Origin + cookie → OK (non-browser); an M2 Wails origin with a cookie → 403;
  21st pre-auth upgrade per IP per minute → 429; 21 pre-auth upgrades within a minute from different addresses in
  one IPv6 /64 → the 21st gets 429; a 17th cookie upgrade of one user → accepted, and its `hello` is answered
  `error{too_many_connections}` + 4429 unless it resumes one of the user's connections (the cap never blocks a
  resume); `AuthenticateRequest` failing with a non-credential error → 503; shutting down → 503.
- **Handshake**:
  - non-hello first → `hello_required` + 4400;
  - no hello in `HelloTimeout` → 4408;
  - protocol 2..2 → `protocol_unsupported` + 4426 with params;
  - bad cookie and no bearer → 4401;
  - cookie and `hello.auth` both → `bad_request`;
  - a 65 KiB message before welcome → 1009;
  - a 200 KiB `pc.answer` (sub) after welcome → accepted by the hub; a 70 KiB `stats` → `message_too_large`.
- **Rate limits**: a burst of 101 messages → `rate_limited` with `retryAfterMs`; continuous flood → 4429.
  `share.start` 11×/min → `rate_limited`. `stats` twice within 5 s → the second is dropped silently.
  - After the 4429 the connection is `detached` for the grace: `room.state` shows it `reconnecting`, and its shares
    stay. A `hello` with its resume token within the grace resumes it (`resumed: true`). If it goes on flooding, it
    is closed with 4429 again at its first refused message, not 10 s later: the buckets and the flood's start
    survive the resume (§4.2).
  - Group 4's tests stop at the close code (`TestRateLimitFlood`), and nothing resumes after a flood. The next
    slice that changes `internal/server/signal` adds this case.
- **Roles**: a table test of every client message × every role against §6.3.
- **Rooms**:
  - two connections of one user → one participant with two connections; watchers merged (max video, any audio);
  - the owner is never in its own share's watchers;
  - `room.state` coalescing: 50 changes in 100 ms → at most 2 snapshots, the last with the final state; `rev`
    increases;
  - byte-identical snapshots to all recipients;
  - `room.event`s not sent on join or resume;
  - `CloseRoom` → `room_closed` (scope room) to that room's connections only;
  - a `room.join` whose `GetRoom` returns before `CloseRoom` but attaches after it → `room_not_found`, no in-memory
    room left;
  - `room_full` when `Policy().MaxRoomParticipants` is reached; a raised limit applies to the next join;
  - `Notify` targets (user, admins, room, all).
- **Shares**:
  - limits per user across two rooms and per room;
  - the same `ref` → same `shareId`;
  - `starting → live → stalled → live`, `starting` timeout and `stalled` timeout → `media_timeout` events;
  - `share.stop` is idempotent; another user's → `forbidden`; same user from another connection → OK;
  - `pc.close` ends shares; a pub offer (any `gen`) without a bound share's tracks ends it with `stopped` before the
    offer is applied; a `starting` share that was never bound is left alone;
  - `replaces` is copied to state and event, and Push is not called.
- **Resume**:
  - socket killed → detached; within grace → `resumed: true`, same `connectionId`, shares unchanged, participant
    `reconnecting` then `present`, no leave/join events, `Resync` called once;
  - after grace → `participant.left{disconnected}` and `share.stopped{disconnected}`;
  - an old token after rotation → `resumed: false`;
  - another user's token → `resumed: false`;
  - resume while the old socket is still attached → old socket gets `replaced` + 4409;
  - client close 1000 or 1001 → no grace; 4000 keeps it;
  - a resume at the per-user cap succeeds; a `hello` with another role → `resumed: false`;
  - after an inbox overflow, attached or detached: no grace, no resume, shares end with `disconnected`.
- **Revocation**: `CloseConnections({UserID, SessionID})` → `session_revoked` + 4401 only on connections of that
  session; `{UserID, ExceptSessionID}` keeps the excepted session; a selector with empty `UserID` closes nothing;
  `account_disabled` → 4403; `Revalidate` returning `ErrInvalid` on the 5 min tick (a shortened interval in tests) →
  `session_revoked`; `Revalidate` returning a non-`ErrInvalid` error (fake: context deadline) keeps the connection
  open and is retried at the next tick.
- **Heartbeat**: a client that sends no messages but answers the hub's ping frames stays open past `IdleTimeout`; one
  that answers nothing gets `idle_timeout` + 4408.
- **Shutdown**: `server.shutdown` with the given reason and `reconnectInMs` in [500, 3000] to every connection, then
  `error{server_shutdown}` and 1012, and `Shutdown` returns before its ctx deadline.
- **Adapter** (`sfuplane`): table tests for every row of §15.4 (calls, events, values, reasons, error codes) against a
  fake `*sfu.Conn` recorder.
- **Slow consumer**: a client that never reads → `slow_connection` / 4503 and no hub goroutine blocked (with
  `goleak`); the connection is then detached and can resume (§3.3).
- **Leaks**: `goleak.VerifyNone` in every test package.

**Integration (Go, in-process server, real SFU from 02 through `sfuplane`, Pion clients via
`internal/client/signal`)**, in package `internal/server/itest`. These extend the S4 harness: 02's `fake.Source` and
`publish.Publisher` (synthetic H.264 with SPS on keyframes, plus Opus), and 02's `sfutest.Viewer` subscribers.
1. **Join and watch**: media arrives, the first packet forwarded is an SPS (DTLS-ready gate, S4 finding 1).
2. **Layers**: `subscribe.update high → low → high` switches on a keyframe, with continuous seq/ts.
3. **Audio-follows-focus**: with two shares and focus on A, only A's audio DownTrack sends RTP. After one
   `subscribe.update`, only B's does, with no instant where both or neither run (sampled every 20 ms).
4. **WebSocket killed mid-share** (close the TCP conn): RTP keeps flowing through the outage (no gap > 200 ms), the
   resume succeeds, and there are no extra renegotiations.
5. **UDP black-holed for 5 s** (a packet filter in the test's UDP mux wrapper): the share goes `stalled`, an ICE
   restart happens, and media resumes within 3 s after unblocking; the viewer's subscription stays; the share id is
   unchanged.
6. **Server restart** (stop hub and SFU, start new ones on the same ports): the publisher re-publishes with
   `replaces`, the subscriber refocuses on the new share id, and media flows within 5 s of the new server being up.
7. **Codec-blocked viewer**: a subscriber with `caps.decode = ["opus"]` gets audio and `subscribe.status{codec}`.
   After `caps.update` with `h264/42e0`: the publisher gets `quality.hint{codec: h264/42e0}`, the viewer's sub PC is
   rebuilt (gen 2), and video decodes. The client never sends `pc.restart` in this flow.
8. **Pub PC rebuild**: the publisher's pub PC is closed abruptly, then `gen 2` is offered with the same share id.
   Viewers get video again without renegotiation.
9. **10 × 10** (from S4): all 100 video subscriptions get the right layer, and audio flows only on focused shares.

**TypeScript (Vitest)**, `signal-client.test.ts` with a fake WebSocket:
- the backoff sequence and jitter bounds, including the reset rule;
- skip-wait on `online`;
- ping timeout → reconnect;
- the resume token is sent and rotated;
- requests reject with `connection_lost` on drop and `request_timeout` after 10 s;
- close-code/error → state mapping (§12.2);
- stale-build detection;
- `server.shutdown` delay.

`codecs.test.ts`: `h264Key` for the same fmtp table as Go. `registry.test.ts`: every fixture's `type` is in the
generated arrays; this imports the Go fixtures with `import.meta.glob`, so one fixture set serves both languages.

**E2E (Playwright, Google Chrome, owned with 05/06)**:
- **Watch**: the sharer tab (canvas + tone source, S4 finding 7) and the viewer tab assert `framesDecoded > 0` and
  audio energy.
- **Reconnect**: `context.setOffline(true)` for 5 s on the viewer, then back online. It asserts "Reconnecting…"
  appeared, playback continues, and the same `connectionId` is in the debug overlay (`resumed`).
- **Restart**: restart the server process during a share; the viewer shows the replaced share within 15 s, focused,
  with audio.
- **Focus**: two sharers; clicking the second tile moves audio; axe checks pass.

Manual (M1 exit): Firefox fresh profile on Windows (§11.7); iOS Safari PWA and Android Chrome watching, with a Wi-Fi
to LTE switch; 5 friends for 2 h.

## 20. Interfaces other docs rely on

- **Wire**:
  - endpoint `GET /ws`;
  - message types (§7) and payloads (§8);
  - error codes and scopes (§12.1);
  - close codes (§12.2);
  - limits (§13);
  - RIDs `f`/`q` ↔ layers `high`/`low`;
  - `CodecKey` format `h264/<4 hex>` | `opus`.
- **`internal/protocol`**:
  - `Version`, `MinVersion`, `Envelope`, `Empty`, `Secret`, `SDP`, `CodecKey`, `ParseH264CodecKey`, `CodecH264*`,
    `CodecOpus`;
  - `MessageType*`, `Registry`, `Spec`, `Lookup`, `ParseEnvelope`, `Decode`, `Marshal`, `NewError`, `FieldError`;
  - all payload types in §8: `Hello`, `HelloAuth`, `ClientInfo`, `Caps`, `Welcome`, `UserInfo`, `Limits`,
    `ICEServer`, `Ping`, `Pong`, `RoomJoin`, `RoomJoinResult`, `RoomInfo`, `RoomState`, `ParticipantInfo`,
    `ConnectionInfo`, `ShareInfo`, `Watcher`, `RoomEvent`, `ShareStart`, `ShareParams`, `Encoding`, `ShareUpdate`,
    `ShareStop`, `PCOffer`, `PCAnswer`, `TrackRef`, `PCICE`, `ICECandidate`, `PCRestart`, `PCClose`,
    `SubscribeUpdate`, `SubscriptionWant`, `SubscribeResult`, `SubscribeStatus`, `SubscriptionStatus`,
    `QualityHint`, `CapsUpdate`, `ClientStats` (+ `PCStats`, `InboundStats`, `OutboundStats`), `StatsWatch`,
    `ServerStats` (+ `ServerSubStats`, `ServerLayerStats`), `Invalidate`, `ServerShutdown`, `Error`, `AgentSend`,
    `AgentSendResult`, `AgentRecv`, `UserConnections`, `OwnConnection`;
  - enums: `Role`, `ClientKind`, `ClientOS`, `Feature`, `AuthScheme`, `TrackKind`, `PCKind`, `VideoLayer`,
    `AudioState`, `ShareKind`, `Preset`, `ShareStatus`, `ParticipantStatus`, `ConnectionStatus`, `RoomEventKind`,
    `EndReason`, `StatusReason`, `HintReason`, `RestartMode`, `RestartReason`, `Topic`, `ShutdownReason`,
    `ErrorScope`, `ErrorCode`;
  - `RIDHigh`, `RIDLow`.
- **`internal/server/signal`**:
  - `Hub`, `New`, `Config`, `DefaultConfig`, `Limits`, `Deps`, `Policy`;
  - `Hub.ServeHTTP`, `Ready`, `Shutdown(ctx, reason)`, `Notify`, `Target`, `CloseConnections`, `ConnSelector`,
    `UpdateUser`, `CloseRoom`, `Snapshot`, `LiveSnapshot` (+ `LiveRoom`, `LiveParticipant`, `LiveConnection`,
    `LiveShare`); later: `Kick`;
  - `Identity`, `Authenticator`, `ErrNoCredentials`, `ErrInvalid`, `ErrNotFound`, `RoomDirectory`, `PushNotifier`,
    `PushShareStarted`;
  - `MediaPlane`, `PeerParams`, `MediaPeer`, `MediaSink`, `ShareMediaEvent`, `ShareMediaKind`;
  - package `signaltest` (fakes).
- **`internal/server/sfuplane`**: `New`, `Plane` (`Bind`, `NewPeer`) and the mapping tables of §15.4.
- **`internal/client/signal`**: `Dial`, `Options`, `Client`, `State*`, `StateChange`.
- **`web/src/protocol/`**: `types.gen.ts`, `api.gen.ts` (03's and 04's REST DTOs), `registry.gen.ts` (`ClientRequests`,
  `ClientNotifications`, `ServerMessages`, `ServerEnvelope`), `SignalClient` (incl. `retryNow`, `probe`,
  `info.shutdown`), `SignalState`, `SignalClientOptions`, `ProtocolError`, `detectCaps`, `h264Key`.
- **Config consumed** (04 owns every key; `ISSHONI_*` overrides per 04): only the guard
  `limits.ws_handshakes_per_ip_per_minute` (20). Admin policy comes from 03's settings through `Deps.Policy`:
  `minClientVersion`, `maxParticipantsPerRoom`, `maxSharesPerRoom`, `maxShareBitrateKbps` (pinnable by 04's policy
  keys `clients.min_version`, `limits.max_participants_per_room`, `limits.max_shares_per_room`,
  `limits.max_bitrate_kbps`). Grace (30 s), shares per user (4) and connections per user (16) are constants.
- **Secrets consumed**: `keys.resume` (32 random bytes in `secrets.json`, 04 `config.KeyResume`).
- **Tasks and CI**: `task gen`, `task gen:check`, `task release:compat VERSION=<version>` (06 `Taskfile.yml`); the
  `testdata/compat/<version>/` snapshot is committed in the release-prep PR and checked by 06's release job (§14.3).

## 21. Depends on

**02-sfu.md**:
- Exposes the `*sfu.SFU`/`*sfu.Conn` API that `sfuplane` (§15.4) maps to `MediaPlane`/`MediaPeer`/`MediaSink`,
  with this doc's counters: `HandleOffer`/`HandleAnswer`/`AddICECandidate` take `gen` and `neg`, pub offers carry the
  `tracks` binding, sub offers return one, share ids come from the hub (`StartShareParams.ID`), and `HandleOffer`
  returns the answer synchronously. It follows §9 and §10.4 on the server side: `gen`/`neg` bookkeeping (the SFU is
  its only owner, for both PC kinds), candidate buffering, the 50 ms offer debounce, stored last answer for replay,
  and `Resync`.
- `RestartICE`, `ResetPC` and `ClosePC` also take `gen`; a lower one does nothing and returns nil. `HandleOffer` with a
  lower `gen`, or a lower `neg` in the current `gen`, returns `sfu.stale_offer`.
- Carries the fields `sfuplane` maps (§15.4): `sfu.Error.Share` and `RetryAfter`, `PCStateEvent.Gen` (emitted only
  for the current PC of each kind), `CodecPolicyEvent.Share` (one per published share), and
  `QualityHintEvent.Reason`/`Encodings` (the full `f`/`q` list with the cap and pause state applied).
- Reports media state through `RoomEvents.ShareUpdated`, which `sfuplane` turns into `ShareMediaEvent`s:
  - `Live` on the first keyframe (and after recovering);
  - `Stalled` after the pub PC has been not connected for 2 s, or while a rebuilt pub PC has no tracks yet;
  - `Changed`.
  The SFU never ends a share on its own; the hub owns the timeouts (§4.4).
- Re-binds a rebuilt pub PC's tracks to existing shares by `tracks` (`shareId`), so viewers keep their DownTracks.
- Keeps the DTLS-ready gate and the keyframe request on connect (S4 finding 1), and uses `ReadSimulcastRTCP` (S4
  finding 2).
- Owns the codec safe set from `Caps`:
  - viewers with no H.264 are left out;
  - it decides when to emit `quality.hint{codec}`;
  - codec-blocked subscriptions get audio only and `StatusReason codec`;
  - after `SetCaps` gains H.264 it rebuilds the sub PC; while an answer keeps rejecting video it rebuilds every 20 s,
    at most 9 times (the only codec retry loop; clients send no `pc.restart` for codecs).
- Owns `ShareParams`/`Encoding` numbers per preset, and the Opus `maxaveragebitrate` per share in the pub answer
  (per m-section).
- Keeps sub offers lean (only the profiles in use, reused inactive transceivers) so SDP stays well under 256 KiB.
- Keeps full ICE with a remote-candidate filter (§17, 02 §7.3).
- Sets the msid stream id to the `shareId` (debugging aid only), puts all server candidates in its SDP, never trickles,
  and supplies `Stats()`.
- Its load test uses `internal/client/signal` with `kind: tool` and cookie auth, and needs one account per simulated
  user (the per-user connection limit is 16).

**03-accounts-and-store.md** (all through the wiring adapters of 04 §6.6):
- `auth.AuthenticateCookie(r)` (cookie only, ignores Authorization) and `auth.Touch` back `Authenticator`; `store`
  backs `RoomDirectory`.
- Revocation paths call `auth.ConnCloser.CloseConnections(sel, reason)`, which maps to `Hub.CloseConnections`
  (reason table in §15.4).
- `httpapi.Signal` hooks map to the hub: `UserChanged` → `Hub.UpdateUser`, `RoomDeleted` → `Hub.CloseRoom`,
  `Notify` → `Hub.Notify`, `RoomPresence`/`OnlineUserIDs` → derived from `Hub.Snapshot()`. 03 lists which REST change
  emits which topic (03 §12.5). There is no kick endpoint in M1.
- User and room ids are opaque strings ≤ 64 chars `[A-Za-z0-9_-]`; the user `name` is the username in M1.
- Provides `POST /api/v1/auth/login` returning the session cookie (used by the load test and Go tests).
- The settings behind `Deps.Policy` (`minClientVersion`, `maxParticipantsPerRoom`, `maxSharesPerRoom`,
  `maxShareBitrateKbps`).

**04-server-platform.md**:
- Mounts `Hub` at `/ws` on the TLS listener (443 mux) and on the plain listener in `tls.mode=off`.
- Supplies `PublicOrigin` (`Site.Origin`), `ClientIP` (trusted proxies), `ServerVersion`, `ResumeKey`
  (`secrets.json` `keys.resume`; `admin rotate-secrets` restarts the server), the guard key
  `limits.ws_handshakes_per_ip_per_minute`, the logger with redaction, and optionally Metrics.
- Calls `Hub.Shutdown(ctx, reason)` on SIGTERM before closing the SFU, and includes `Hub.Ready()` in `/readyz`.
- Implements `PushNotifier` in `internal/server/push` (recipient selection, skipping `PresentUserIDs`, dedup, rate
  limits), adapted by the wiring.
- Owns the wiring (`internal/server`, 04 §6.6) that builds `sfuplane`, the SFU and every adapter above.
- Owns the browser connection-test endpoint (`POST /api/v1/conntest`, REST, on top of 02's `SFU.Probe`); it is not a
  signaling feature.

**05-web-client.md**:
- Uses `SignalClient` and follows:
  - §9 for PC handling (stereo munging, `setCodecPreferences` order, `tracks` mapping, `pc.close`);
  - §10.4 and §10.5 for recovery;
  - §10.6 for the 60 s capture hold and `replaces` focus carry-over;
  - §6.1 for stale-build reload;
  - §8.9 for audio-follows-focus and batched updates;
  - §11.7 for the Firefox codec wait UI (caps polling and `caps.update` only; no client retries).
- Maps every `ErrorCode` to `errors.<code>` in `en.json`, and renders statuses and fallbacks per §8.13.
- Uses role `full` or `viewer` per §6.3, and maps `invalidate` topics to TanStack Query keys.
- Reports the §8.11 `InboundStats` counters in its `stats` notifications.

**06-deploy-and-ci.md**:
- `tools/go.mod` pins tygo v0.2.21; `task gen` / `task gen:check` as in §14.4.
- CI runs the protocol drift check, `go test -race` (including fixtures, compat and fuzz seeds), and the Vitest
  protocol tests.
- `task release:compat VERSION=<version>` copies the fixtures into `internal/protocol/testdata/compat/<version>/` and
  keeps the last two final releases; it runs in the release-prep PR and is on the 06 §9.1 "Before tagging" list. For a
  non-prerelease tag, release.yml's `build` job fails if that snapshot is missing or differs from `testdata/v1`.
- Playwright e2e covers the reconnect and restart scenarios in §19.

## 22. Implementation slices

Each slice is independently testable and lands behind green CI. Estimates assume a solo developer.

| # | Slice | Scope | Acceptance | Depends on |
|---|---|---|---|---|
| P1 | Protocol types | `internal/protocol`: all §8 types and enums, `Envelope`, `ParseEnvelope`, `Decode`, `Marshal`, `Secret`/`SDP`, `CodecKey`, `Validate` methods, `Registry`, the §14.3 fixture set | `go test -race ./internal/protocol/...` green: round-trip, coverage, no-null, unknown-field tolerance, validation tables, codec table, secret redaction; fuzz targets run 30 s without findings | — |
| P2 | TS generation | `tygo.yaml` (both packages), `internal/protocol/gen/tsregistry`, `web/src/protocol/{types,api,registry}.gen.ts`, `task gen`/`gen:check`, `registry.test.ts`, `codecs.ts` + tests | `task gen` is idempotent; `npm run typecheck` and Vitest green; renaming a Go field without regenerating fails `task gen:check` | P1, 03 DTOs |
| P3 | Hub skeleton | `internal/server/signal`: upgrade checks (Origin, pre-auth limits, per-user cap), hello/welcome with version and features, auth via `Authenticator`, read limits, ping/pong, idle timeout, rate limits, error/close mapping, send queue, `Shutdown(ctx, reason)`, `Ready`, `signaltest` fakes | Upgrade (incl. the Origin matrix), handshake, rate-limit, slow-consumer and shutdown tests of §19 green; goleak clean | P1 |
| P4 | Rooms and participants | `room.join/leave`, Participant merge, coalesced byte-identical `room.state`, `room.event`, watchers from desired subscriptions (fake media), `Notify`, `CloseRoom`, `UpdateUser`, `CloseConnections` and revalidation, `Policy`, `Snapshot` | Room, notify and revocation tests of §19 green | P3 |
| P5 | Resume and grace | Resume tokens (HMAC, rotation), detached state, grace timer, `replaced`, deliberate-close fast path, `Resync` hook | Resume tests of §19 green, including "no leave/join events within grace" | P4 |
| P6 | Go signaling client | `internal/client/signal`: dial, hello, backoff, resume, requests, events, states | Tests against the P5 hub: reconnect with resume after a killed socket; backoff bounds; `Close` → server skips grace | P5 |
| P7 | Shares and negotiation plumbing (fake media) | `share.start/update/stop` (limits, `ref`, `replaces`, state machine and timeouts), `pc.*` routing with role and ownership checks (gen/neg are checked by 02), `subscribe.update`/status, `quality.hint`, `caps.update`, `stats`/`stats.watch` forwarding, Push call | Share and role-matrix tests of §19 green against the fake `MediaPlane` | P5 |
| P8 | Real SFU wiring | `internal/server/sfuplane` (§15.4) with its table tests; in-process integration harness `internal/server/itest` with 02's fake publisher and `sfutest.Viewer` on P6 | Adapter table tests green; integration cases 1–3 and 9 of §19 green | P6, P7, 02's SFU slices 6–7, 04 wiring v1 |
| P9 | Recovery end to end | ICE restart and rebuild paths, `Resync`, server restart with `replaces`, codec-blocked viewer flow | Integration cases 4–8 of §19 green; no goroutine leaks | P8 |
| P10 | TS SignalClient | `web/src/protocol/signal-client.ts`, `errors.ts`, `index.ts` | Vitest cases of §19 green; the SPA (05) connects to a `task dev` server and reaches `ready` | P2, P5 |
| P11 | E2E | Playwright watch, reconnect, restart and focus scenarios (with 05/06) | All four scenarios green in CI on Google Chrome | P9, P10, 05 viewer/sharer |
| P12 | Same-user relay (cuttable) | `agent.send`/`agent.recv`, feature `agent.relay`, limits | Two connections of one user exchange messages; another user's target → `agent_target_not_found`; payload 17 KiB → `message_too_large`; 11/s → `rate_limited` | P7 |
| P13 | Compat snapshot | The compat test (§14.3); the `release:compat` task body and the release-job check are 06's (README S75) | `task release:compat VERSION=0.0.1` on a scratch branch copies the fixtures and prunes to two; the release check fails on a missing or stale snapshot; removing a field from a type fails the compat test | P1, 06 release job |

## 23. Decisions taken at integration (formerly open questions)

1. **Read limit for SDP**: accepted. 64 KiB before `welcome` and for every non-SDP message; `pc.offer`/`pc.answer`
   up to 256 KiB after login. A deliberate, documented refinement of the plan's 64 KiB guard (README).
2. **What "watching" counts**: anyone receiving a share's video at any layer or its audio. It matches the plan's "no
   hidden viewers"; the snapshot carries each watcher's layer, so the UI may add "(1 focused)".
3. **Kick**: not in M1. Admins disable an account (03) for a lasting block; `kicked` codes stay reserved.
4. **Web sharer during a server outage**: keep the capture for 60 s (the desktop encoder's 30 s is M2's choice).

Decided after group 5 (engineering calls the owner delegated; README §6):

5. **Everyone in the room sees who is an admin**: `ParticipantInfo` gains the additive field `admin` (§8.5; README
   F02 adds it to the type, the hub and the people panel, 05 §11.2). Until then `room.state` says who is in the
   room but not who is an admin, and the web client shows the badge on the user's own row only.
6. **A `share.update` from another connection does not reach the publishing client in M1** (§8.7): `room.state`
   and the SFU get the new preset, the publishing connection gets no `quality.hint`, and `HintReasonPreset` is not
   emitted. It arrives with the M2 desktop handoff.

Decided after group 6 (engineering calls the owner delegated; README §6; README F03 implements both):

7. **An `agent.send` with `payload: null` is `bad_request`** (§8.14), like one without a payload. The hub then never
   relays a `null`, which nothing on this wire sends (§5).
8. **The server asks for no pub PC rebuild that §9 rule 10 forbids**: the hub's `MediaSink` drops the request of a
   failed pub PC whose connection has no share that has not ended; `sfuplane` still reports every failed pub PC
   (§9 rule 10, §15.4).
