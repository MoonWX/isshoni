# M2 design 02: Device authorization and device tokens

Part of the M2 ("Windows app") design set, `docs/m2/01` to `06`. `docs/PLAN.md` is the source of truth above this
document. The M1 specs (`docs/m1/01` to `06`, with `docs/m1/README.md` §6) record what exists. This document owns:
- the **RFC 8628 device authorization flow** on the server: the `/api/v1/device/*` endpoints, the approval page
  `/link?code=…`, user codes, lifetimes, polling, throttles and audit rows;
- **device tokens**: short-lived access tokens, rotating refresh tokens with reuse detection, hashing at rest, bearer
  authentication on REST and in `hello`, CORS for the desktop asset origins, revocation, device expiry, last-seen data;
- the **Go client side**: `internal/client/deviceauth` (linking, refresh and rotation, the server list),
  `internal/client/credstore` (Windows Credential Manager; macOS Keychain later), `internal/client/deeplink`
  (`isshoni://` links) and `internal/client/compat` (version checks, "Update the app" vs "Ask your admin");
- the plan's **in-app username/password fallback** on the server and in Go (§14: `POST /api/v1/device/password`, M2).

**Milestone marking.** Every item is **M2** unless it is tagged *later (M3)*, *later (M4)*, *later (M5)*, *later (M6)*
or *pending* (the native mobile apps). A later item only needs room now.

**What this replaces.** 03 (`docs/m1/03-accounts-and-store.md`) designed the device flow as "later (M2)": §3.2 (token
table), §7.3 (`lookup` bucket), §7.5 (bearer and CORS), §7.6 (WebSocket bearer), §7.7 (device rows), §7.12 (the
flow), §10 (device audit rows), §11 (privacy rows), §12.2–§12.4.7 (codes, endpoints, JSON) and §14 (limits). This
document is the M2 design of all of them. Where it changes 03's sketch it says so and why; 03's M1 behavior stays as
built. Slice DA12 writes a pointer to this document into those 03 sections.

**The other M2 documents**, referenced by file name: [01-client-core.md](01-client-core.md) (the Go client core, the
publisher/agent connection, `internal/client` rules), [03-desktop-app.md](03-desktop-app.md) (Wails shell, the
bundled SPA's `DesktopPlatform`, screens, first-run wizard, tray), [04-windows-engine.md](04-windows-engine.md),
[05-windows-audio.md](05-windows-audio.md), [06-windows-release.md](06-windows-release.md) (installer, protocol
registration, updater, release).

---

## 1. Key decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | RFC 8628 with 03's conventions: JSON bodies with camelCase names, the one REST error envelope (03 §12.2), no `client_id` | Only first-party clients use it (03 §7.12); one error shape for the SPA and the app |
| 2 | Approval happens only in a browser with a **cookie session**, on the server's own `/link` page. A device token can never approve a code | A stolen device token can't link more devices; the approver reads the request on the server's page, not in an app |
| 3 | User code: 8 letters from `BCDFGHJKLMNPQRSTVWXZ`, shown `XXXX-XXXX` (≈34.6 bits), valid 10 min | RFC 8628 §6.1's alphabet: no vowels (no words), no digits (no 0/O or 1/I mix-ups). With per-user lookup throttles a guess is hopeless (§5.5) |
| 4 | The approval page shows the code to compare with the app, the app and its version, an editable device name, the request time and IP, and whether the request came from **the browser's own network**; every answer except a match (`different` or `unknown`) requires an "I started this myself" tick | RFC 8628 §5.4 (remote phishing): a friend can tell a code they started from one somebody sent them. A phisher can force `unknown` by picking the other address family (§5.3), so only a match skips the tick |
| 5 | Access tokens live 15 min; refresh tokens 90 days from issue and rotate on every use. Both are stored only as HMAC-SHA-256 hashes (03 §3.2). Access tokens are rows, not signed blobs | Revocation is immediate (rows are deleted), at the cost of one indexed read per uncached request, the same as web sessions (03 §7.4) |
| 6 | A spent refresh token presented again **revokes the device**, except an idempotent retry: the same `exchangeId` (the client's retry of one refresh), or within 120 s and only once, and in both cases only while the pair it was exchanged for is unused. A grace re-issue **keeps the child it replaces as a spent row** (only that child's access token is deleted), so whoever presents that child later trips reuse detection | Two holders of one token (theft) are caught, also inside the 120 s window: a client that takes a grace path never received the child it replaces, so only a second holder can ever present it. An app whose refresh answer was lost on a flaky network is not signed out. 03's 30 s grace was shorter than the client's own retry series (§11.4) |
| 7 | The refresh token never leaves Go. The desktop SPA gets 15-min access tokens from Go through a binding and keeps them in memory only | A bug in the webview can leak at most a short-lived token; the long-lived secret stays in the OS credential store |
| 8 | Windows Credential Manager through a small wrapper over `advapi32` `CredWriteW`/`CredReadW`/`CredDeleteW`/`CredEnumerateW` (x/sys/windows), generic credentials, `CRED_PERSIST_LOCAL_MACHINE`, target `isshoni:v1:<origin>` | No new dependency (x/sys is in `go.mod`); `LOCAL_MACHINE` never roams a device's token to another PC; one credential per server |
| 9 | The desktop asset origins (`http://wails.localhost` on Windows, `wails://localhost` on macOS, plus three spare spellings) are accepted for CORS and on `/ws` **only with bearer tokens**. Requests from them never use cookies | Every Wails app on a PC has the same origin, so the origin is no identity; the token is. CORS gives such a page nothing it couldn't do with `net/http` |
| 10 | Five self-service routes stay **cookie-only**: change password, delete account, "sign out other browsers", push subscribe and push test; plus the device approval routes and **every admin route** (`/api/v1/admin/*`: #29–#43, #43 being M1 04's dashboard, plus M1 04's doctor and bandwidth). The invite routes (#21–#23) keep taking a bearer | The app handles a password only in the linking fallback (decision 12), never for account changes; the desktop SPA opens the account and admin pages in the system browser (03-desktop-app D7). A device's refresh token is readable by any process running as that Windows user (§11.5), so it must not reach server administration; the desktop invite dialog needs the invite routes |
| 11 | No absolute device lifetime: a device lives while it refreshes at least once in 90 days; the app refreshes at start and daily while running | Relinking a desktop app every few months is friction without a matching threat; the Devices page, rotation and reuse detection cover theft |
| 12 | The plan's in-app username/password fallback is **M2** (§14): `POST /api/v1/device/password` (#48) with login's order and throttles, creating a device with `linked_via = 'password'`. The device flow stays the default path; 03-desktop-app offers the form as "Sign in with username and password instead" | Plan "Privacy and trust model" ("The fallback is an in-app username/password form"); M1 03 §12.3 marks #48 M2. It covers a friend with no logged-in browser at hand, and lets the desktop e2e link the real app without a browser |
| 13 | `isshoni://<host>[:port]/[r/<roomId>]` means `https://<host>[:port]`, room `<roomId>`. Query and fragment are never read | Mirrors the web URL `https://<host>/r/<roomId>`; there is no field a credential could travel in; a link only adds or opens a server after a confirmation and never starts a share |
| 14 | One decision function (`compat.FromInfo`, `compat.FromError`) turns `/info`, `protocol_unsupported` and `client_outdated` into "Update the app" or "Ask your admin to update the server" | One table, tested once, for the pre-link check and the `hello` errors (plan "Versioning and compatibility") |

---

## 2. Scope and ownership

**Owned here** (each implementation slice in §19 lists its exact paths):
- `internal/protocol/desktop.go`: `DesktopAssetOriginWindows`, `DesktopOrigins()` (*later (M3)*:
  `DesktopAssetOriginMacOS`).
- `internal/protocol/api/device.go`: the device-flow DTOs, `DevicePasswordRequest`, `NormalizeUserCode`,
  `FormatUserCode`.
- `internal/version/semver.go`: SemVer parsing and comparison, exported (moved from `internal/server/signal/semver.go`).
- Server: `internal/server/store` (migration `0002_device_flow.sql`, the device methods, the device rule of the
  janitor), `internal/server/auth` (`deviceflow.go`, `devicepassword.go`, `bearer.go`, `refresh.go`, the new buckets,
  bearer cases of `session.go` and `selfservice.go`, `Principal.DeviceName`), `internal/server/httpapi` (`device.go`,
  the CORS and credential steps of the `/api/v1` chain, `/me`, `/me/devices`), the hub's pre-auth refund and
  `Identity.DeviceName` in `internal/server/signal`, the bearer adapter and origins in `internal/server/wire.go`, and
  `internal/server/itest/device_test.go` with `device_helpers_test.go`.
- Web: `web/src/link/` (the approval page), the device rows of `web/src/account/DevicesPage.tsx`,
  `web/src/lib/appLink.ts`, the `link` i18n namespace, `web/e2e/link.spec.ts`.
- Client: `internal/client/deviceauth`, `internal/client/credstore`, `internal/client/deeplink`,
  `internal/client/compat`.
- Docs: the server-side device rows of `docs/privacy.md`, a "Linking the app" section (`#desktop-linking`) in
  `docs/troubleshooting.md`.

**Not owned here**: every desktop screen (the "Link this app" screen with its QR code, the password form, the "Add
server" modal, the update screens, first-run wizard step 1, the tray) and the Wails binding mechanics are
03-desktop-app's, built on the state model and service of §11.7; `DesktopPlatform` (`apiFetch`, `signaling()`) is
03-desktop-app's and follows the rules of §11.8; the `isshoni` URL-protocol registration, the uninstaller and its
command line, the updater behind "Update the app", every CI job and the `/privacy#desktop` section are
06-windows-release's; the Go signaling client, the client core and the depguard rule are 01-client-core's.

### 2.1 Package layout

```
internal/protocol/desktop.go              DesktopAssetOriginWindows, DesktopOrigins() (stdlib only)
internal/protocol/api/device.go           DTOs + user-code helpers (stdlib only; tygo → web/src/protocol/api.gen.ts)
internal/version/semver.go                Parse, Compare, Below (moved from internal/server/signal)
internal/server/store/
  migrations/0002_device_flow.sql
  devices.go                              device codes, devices, device tokens (§4.2)
  prune.go                                + the devices rule (§6.7)
internal/server/auth/
  deviceflow.go                           StartDeviceFlow, LookupUserCode, DecideUserCode, the device_code grant
  devicepassword.go                       PasswordDeviceLogin (§14)
  refresh.go                              the refresh_token grant: rotation, reuse detection, grace
  bearer.go                               AuthenticateBearerToken, the bearer cache, Touch for devices
  devicename.go                           NormalizeDeviceName
  limiter.go                              + device-code-ip, device-code-ip48 (IPKey48), device-token-ip, device-lookup
internal/server/httpapi/
  device.go                               /api/v1/device/*, PATCH /me/devices/{id}
  api.go                                  + CORS step, credential step, Access variants (§6.4)
internal/server/itest/
  device_test.go                          the cases of §16.2
  device_helpers_test.go                  linkDevice, linkedManager (§16.2), shared with 01-client-core's itests
internal/client/
  deviceauth/  client.go manager.go servers.go state.go errors.go compat_server_test.go
  credstore/   credstore.go wincred_windows.go file_other.go memory.go
  deeplink/    deeplink.go origin.go testdata/links.json
  compat/      compat.go
web/src/link/  DeviceLinkPage.tsx DeviceLinkPage.module.css useDeviceLink.ts userCode.ts index.ts
web/src/lib/appLink.ts
```

### 2.2 Dependencies

No new module. `golang.org/x/sys/windows` (in `go.mod`) for Credential Manager; `golang.org/x/net/idna` (in `go.mod`)
for host names in deep links; the rest is the standard library. 03-desktop-app renders the QR code of
`verificationUriComplete` in the SPA (M1's uqr-based `QrCode` component, `web/src/setup/QrCode.tsx`); no binding
returns an image.

**Wails at the time of writing.** The newest Wails v3 release on 2026-10-21 is **v3.0.0-beta.34** (2026-10-20,
pre-release; there is no v3 GA). At that tag the asset server's base URL is `http://wails.localhost` on Windows and
`wails://localhost` on macOS (`v3/internal/assetserver`). 03-desktop-app pins the exact version; §6.4 keeps the
server's list independent of that pin.

---

## 3. Tokens and codes

All random values come from `crypto/rand`; all are base64url without padding unless the row says otherwise. Every hash
is HMAC-SHA-256 with the `session` key of `secrets.json` (03 §3.2), so `isshoni admin rotate-secrets` invalidates
exactly these (03 §4.6: a changed `session` key deletes every `devices`, `device_tokens` and `device_codes` row, and
desktop apps relink).

| Value | Random | Shape | Travels in | Stored as | Lifetime |
|---|---|---|---|---|---|
| Device code | 32 bytes | 43 chars | `/device/code` reply → `/device/token` body (Go only) | `device_codes.device_code_hash` | 600 s; row pruned 1 h after expiry |
| User code | 8 symbols of 20 (34.6 bits) | `WDJB-MJHT`; normalized `WDJBMJHT` | app screen, QR code, `/link?code=`, `/device/lookup` and `/device/decide` bodies | `device_codes.user_code_hash` (of the normalized form) | the code's row |
| Access token | 32 bytes | `isa_` + 43 | `Authorization: Bearer` (REST), `hello.auth` (WebSocket) | `device_tokens` kind `access` | 900 s |
| Refresh token | 32 bytes | `isr_` + 43 | `/device/token` bodies only, sent by Go only | `device_tokens` kind `refresh` | 90 d from issue; spent on use, or superseded by a grace re-issue (§6.2); a spent row stays until its own expiry (reuse detection) |
| Exchange id | 16 bytes | 22 chars | `/device/token` refresh body | `device_tokens.exchange_hash` of the row it spent | that row's |

**User codes.**
- Generation: rejection sampling over random bytes: a byte `b ≥ 240` is dropped, otherwise the symbol is
  `alphabet[b % 20]` with `alphabet = "BCDFGHJKLMNPQRSTVWXZ"`; 8 symbols. A collision with an existing row's
  `user_code_hash` (UNIQUE) retries with a new code, at most 3 times, then `internal`.
- `api.NormalizeUserCode(s string) (string, bool)`: drop ASCII spaces, tabs and `-`, map ASCII `a`–`z` to upper case;
  the result must be exactly 8 letters of the alphabet. `api.FormatUserCode` inserts the `-` after 4. The TypeScript
  copy is `web/src/link/userCode.ts`; both test suites use these cases:

| Input | Result |
|---|---|
| `WDJB-MJHT`, `wdjb mjht`, ` wdjbmjht `, `WDJB-MJHT-` | `WDJBMJHT` |
| `WDJB-MJHA` (A is a vowel), `WDJB-MJH` (7), `WDJB-MJHTX` (9), `WDJB–MJHT` (en dash), `ＷＤＪＢＭＪＨＴ` (full width), `` | invalid |

**Token shape checks** run before any hashing or database access (03's rule for every token): an access token is
exactly `isa_` plus 43 canonical base64url characters decoding to 32 bytes, a refresh token the same with `isr_`, a
device code 43 such characters, an exchange id 22 decoding to 16 bytes. Anything else is refused as §5 and §6 say.

**Redaction.** Every DTO field that carries a device code, token or exchange id is redacted in `fmt` and `slog` by its
type's redact method (`internal/protocol/api/secret.go`; `TestSecretFieldsRedacted` fails otherwise). The user code is
not a secret on its own but is never logged either. The device name is user content and is never logged (like share
labels, README §4 Logging).

---

## 4. Schema and store

### 4.1 Migration `0002_device_flow.sql` (schema version 2)

The device tables shipped in `0001_init.sql` (03 §5). M2 adds two nullable columns and one index; every row and query
of M1 stays valid.

```sql
-- 0002_device_flow.sql (M2): refresh-token rotation (docs/m2/02-device-auth.md §4, §6.2).

-- Hash of the refresh token whose exchange issued this token (access or refresh). NULL for the pair a device-code
-- grant issued. The grace paths of §6.2 delete the access token of one exchange by it, and check by it that a child
-- came from its parent's own exchange (a superseded child's successor has another issued_by).
ALTER TABLE device_tokens ADD COLUMN issued_by BLOB;

-- Refresh tokens, once spent: HMAC of the exchangeId the client sent with the request that spent it. NULL when the
-- client sent none. A replay with the same exchangeId is the client's own retry (§6.2).
ALTER TABLE device_tokens ADD COLUMN exchange_hash BLOB;

CREATE INDEX device_tokens_issued_by ON device_tokens(issued_by);
```

- `issued_by` is no foreign key (its parent row may be pruned first), so 03 §15's FK-index rule doesn't cover it; it
  gets an index anyway for the grace paths' delete.
- Release notes flag the schema bump (06-windows-release.md §11.8); a 0.1.x server refuses the new file and names the
  pre-migration backup (03 §4.5). If another M2 document also needs a migration, the slice that merges first takes
  `0002` and the next `0003` (the contiguity test, 03 §15).

### 4.2 Store Go API (M2)

The M1 stubs of 03 §6 get their bodies; two signatures change (marked); the rest is new. `DeviceToken` gains
`IssuedBy, ExchangeHash []byte`. Every method works inside `Write` as 03 §6 says; the caller writes the audit rows.

```go
package store

// ---- device codes ----
// CreateDeviceCode inserts a pending code. *ConflictError{"user_code_hash"} on a user-code collision (the caller
// draws a new code, §3); the device-code hash is a primary key and never collides in practice.
func (q *Q) CreateDeviceCode(dc DeviceCode) error
func (q *Q) DeviceCodeByHash(h []byte) (DeviceCode, error)     // by device_code_hash, any status; ErrNotFound
func (q *Q) DeviceCodeByUserCode(h []byte) (DeviceCode, error) // by user_code_hash, any status; ErrNotFound
// CountPendingDeviceCodes counts status 'pending' rows with expires_at > now (the server-wide cap, §5.5).
func (q *Q) CountPendingDeviceCodes(now time.Time) (int, error)
// DecideDeviceCode (CHANGED: takes name): UPDATE … SET status = ?, user_id = ?, decided_at = now,
// device_name = COALESCE(NULLIF(name, ''), device_name) WHERE user_code_hash = ? AND status = 'pending'
// AND expires_at > now. 0 rows → ErrNotFound.
func (q *Q) DecideDeviceCode(h []byte, status string, u UserID, name string, now time.Time) error
// PollDeviceCode sets last_poll_at = now and interval_s = interval (whole seconds).
func (q *Q) PollDeviceCode(h []byte, now time.Time, interval time.Duration) error
// ConsumeDeviceCode: status 'approved' → 'consumed'. 0 rows → ErrNotFound.
func (q *Q) ConsumeDeviceCode(h []byte, now time.Time) error

// ---- devices ----
func (q *Q) CreateDevice(d *Device) error                     // sets d.ID (NewID)
func (q *Q) DeviceByID(id DeviceID) (Device, User, error)     // with its user; ErrNotFound
func (q *Q) RenameDevice(u UserID, id DeviceID, name string) (Device, error) // ErrNotFound if not u's
// TrimDevices deletes the user's least recently seen devices beyond keep and returns them (for audit names).
func (q *Q) TrimDevices(u UserID, keep int) ([]Device, error)
func (q *Q) TouchDevice(id DeviceID, ip, appVersion string, now time.Time) error // appVersion "" = keep
// ListDevices, DeleteDevice, DeleteDevices, DeleteDeviceCodesOf, DeleteAllDeviceCodes: as built in M1.

// ---- device tokens ----
func (q *Q) InsertDeviceToken(t DeviceToken) error
// DeviceTokenByHash returns an unexpired token (expires_at > now) of the kind, spent or not, with its device and the
// device's user. ErrNotFound otherwise. The caller checks the user's status.
func (q *Q) DeviceTokenByHash(h []byte, kind string, now time.Time) (DeviceToken, Device, User, error)
// SpendRefreshToken (CHANGED: takes exchangeHash): UPDATE … SET spent_at = now, replaced_by = ?, exchange_hash = ?
// WHERE token_hash = ? AND kind = 'refresh' AND spent_at IS NULL. 0 rows → ErrConflict (spent in parallel).
func (q *Q) SpendRefreshToken(h, replacedBy, exchangeHash []byte, now time.Time) error
func (q *Q) SetReplacedBy(h, replacedBy []byte) error                // the grace path's new child (§6.2)
// SupersedeRefreshToken marks the child a grace path replaces as spent instead of deleting it (§6.2), so whoever
// presents it later is caught as reuse: UPDATE … SET spent_at = now, replaced_by = ?, exchange_hash = NULL
// WHERE token_hash = ? AND kind = 'refresh' AND spent_at IS NULL. 0 rows → ErrConflict (spent in parallel).
// The row stays until its own expires_at, like every spent refresh token.
func (q *Q) SupersedeRefreshToken(h, replacedBy []byte, now time.Time) error
// DeleteAccessTokensIssuedBy deletes the access tokens one exchange issued (kind = 'access' AND issued_by = h);
// refresh tokens are never deleted by it.
func (q *Q) DeleteAccessTokensIssuedBy(h []byte) (int, error)
func (q *Q) TrimAccessTokens(id DeviceID, keep int) (int, error)     // newest `keep` live access tokens stay
```

`Prune` (03 §4.7) gains the devices rule of §6.7 and `PruneStats.Devices`. `Stats.Devices` and `UserRow.Devices`/
`LastSeenAt` (03 §6) already count devices.

---

## 5. Server: the device authorization flow

### 5.1 Sequence

```
 desktop app (Go)                 isshoni server                         friend's browser (logged in)
 POST /device/code ────────────►  pending code, 600 s
 ◄── deviceCode, userCode, URIs
 opens verificationUriComplete ─────────────────────────────────────────► /link?code=WDJB-MJHT
 shows code + QR                                                          POST /device/lookup  (cookie)
 every 5 s: POST /device/token ─►  authorization_pending                  "Link isshoni for Windows on Gaming PC?"
                                                                          POST /device/decide approve (cookie)
                                   approved
 POST /device/token ───────────►  device + token pair, code consumed
 ◄── accessToken, refreshToken
 saves refresh token in Credential Manager, then hello{auth: bearer}
```

### 5.2 Start: `POST /api/v1/device/code` (Public)

Request (`api.DeviceCodeRequest`):

```json
{"clientKind": "desktop", "deviceName": "DESKTOP-7F3K2LQ", "os": "windows", "appVersion": "0.2.0"}
```

Order, every step before the next:
1. `auth-ip` (03 §7.3: 20, then 1 per 15 s) → 429 `rate_limited`.
2. `device-code-ip` (10, then 1 per 6 min), then, for an IPv6 client only, `device-code-ip48` (keyed by the
   client address's /48, `auth.IPKey48`: 30, then 1 per 2 min) → 429 `rate_limited`. An IPv4 client skips the second
   bucket (its /32 key is already one address).
3. Validation → 422 `validation_failed` with these field codes:
   - `clientKind`: `desktop` or `mobile` (01 `ClientKind`; the Linux agent sends `desktop`), else `invalid`;
   - `os`: `windows`, `macos`, `linux`, `ios` or `android` (the `devices.os` CHECK), else `invalid`;
   - `deviceName`: `auth.NormalizeDeviceName` (§5.6): `required`, `too_long` or `invalid`;
   - `appVersion`: 1–32 bytes of `[0-9A-Za-z.+-]`, else `required`/`too_long`/`invalid` (not necessarily SemVer: dev
     builds are `0.0.0-dev+<commit>`).
4. Server-wide cap: 5,000 pending, unexpired codes (`CountPendingDeviceCodes`) → 503 `server_busy` with
   `Retry-After: 60`. A row is about 200 bytes, so the cap bounds the table at about 1 MB; it is a backstop for
   memory and disk, and the per-prefix buckets of step 2 keep one holder of many IPv6 /64s from reaching it (§5.5).
5. One `Write`: `CreateDeviceCode` with `request_ip` = `ClientIP`, `expires_at = now + 600 s`, `interval_s = 5`.

Response 200 (`api.DeviceCodeResponse`):

```json
{"deviceCode": "EXAMPLEdeviceCODE0123456789abcdefghijklmnopq",
 "userCode": "WDJB-MJHT",
 "verificationUri": "https://watch.example.com/link",
 "verificationUriComplete": "https://watch.example.com/link?code=WDJB-MJHT",
 "expiresIn": 600, "interval": 5}
```

The URIs come from `auth.Origins.Primary` (04's `Site.Origin`). No audit row: the request is anonymous and frequent,
and a throttled address writes `auth.throttled {scope: "ip"}` as in M1.

### 5.3 Approval: `POST /api/v1/device/lookup` and `POST /api/v1/device/decide` (cookie sessions only)

Both have the access `UserSession` (§6.4): a bearer gets 403 `forbidden` with `params {credential: "session"}`.

**Lookup** (`api.DeviceLookupRequest {userCode}`), order:
1. `NormalizeUserCode` fails → 422 `validation_failed {userCode: required|invalid}`; costs no token.
2. `device-lookup` (key: user ID; 10, then 1 per min) takes a token **before** the read, like 03's per-username buckets,
   so parallel guesses all see the bucket as it is. Empty → 429 `rate_limited` with `retryAfter`.
3. Read `DeviceCodeByUserCode`. Unknown, not `pending`, or `expires_at ≤ now` → 404 `device_code_invalid`; the token
   is kept (a wrong code). Found → the token goes back (refund).

Response 200 (`api.DeviceLookup`):

```json
{"userCode": "WDJB-MJHT", "clientKind": "desktop", "os": "windows", "deviceName": "DESKTOP-7F3K2LQ",
 "appVersion": "0.2.0", "requestIp": "203.0.113.7", "network": "same",
 "requestedAt": "2026-11-02T19:04:05.123Z", "expiresAt": "2026-11-02T19:14:05.123Z"}
```

`network` compares `auth.IPKey(request_ip)` with `auth.IPKey(ClientIP(this request))` (IPv4 /32, IPv6 /64, 03 §7.3;
an IPv4-mapped IPv6 address counts as IPv4): `same` when equal, `different` when both are of one family and differ,
`unknown` when the families differ. `unknown` alone is no sign of phishing (a dual-stack PC can reach the server over
IPv6 from the app and over IPv4 from the browser), but a remote phisher can force it: they start the flow over the
family the victim's browser isn't using (`curl -6` against a dual-stack server when the victim's browser is
IPv4-only, or the reverse). So the page treats every value except `same` as needing the "I started this myself" tick
(§10.1), with softer text for `unknown`. `same` is no proof either: people behind one Wi-Fi router or one
carrier-grade NAT share an address, so the page always asks the friend to compare the code.

**Decide** (`api.DeviceDecideRequest {userCode, approve, deviceName?}`), order: step 1 above, then `deviceName` when
present (§5.6, 422 `{deviceName: …}`), step 2, then one `Write`:
- `DecideDeviceCode(h, approve ? "approved" : "denied", p.UserID, name, now)`. The caller is checked again in the same
  `Write` (an active user whose session still exists, 03's `loadSelf` rule).
- 0 rows: when the code is already decided **by this user with the same decision**, the answer is 204 again (a double
  click, a retried request); otherwise 404 `device_code_invalid` (the `device-lookup` token is kept).
- Audit `device.approved` or `device.denied` (§8). 204, and the `device-lookup` token goes back (also for the
  idempotent repeat).

Two users deciding one code at once: the `WHERE status = 'pending'` makes the first `Write` win; the second gets 404.
Approval never creates the device: the device and its tokens appear at the app's next poll (§5.4), so an approval
nobody collects leaves nothing behind but the code row, which expires.

### 5.4 Polling: `POST /api/v1/device/token` with `grantType: device_code` (Public)

```json
{"grantType": "device_code", "deviceCode": "EXAMPLEdeviceCODE0123456789abcdefghijklmnopq"}
```

Order: `device-token-ip` (60, then 1 per s; 429 `rate_limited`); `grantType` known, else 422
`validation_failed {grantType: invalid}`; the device code's shape (§3), else 400 `invalid_grant`. Then one `Write` that
**decides the answer and commits** (the poll bookkeeping must survive an error answer, so the function returns nil
and the handler sends the decided error after the commit):

| State of the code at the poll | Answer | Writes |
|---|---|---|
| unknown hash (never issued, pruned) | 400 `invalid_grant` | — |
| `consumed` | 400 `invalid_grant` | — |
| `now ≥ expires_at` (any other status) | 400 `expired_token` | — |
| `last_poll_at` set and `now − last_poll_at < interval_s − 1 s` | 400 `slow_down` with `params {interval}` | `interval_s = min(interval_s + 5, 60)`, `last_poll_at = now` |
| `pending` | 400 `authorization_pending` | `last_poll_at = now` |
| `denied` | 400 `access_denied` | `last_poll_at = now` |
| `approved`, but the approver is no longer `active` | 400 `access_denied` | `last_poll_at = now` |
| `approved` | 200 token pair (below) | device, tokens, `consumed`, audit |

- The 1 s tolerance absorbs network jitter between two polls sent exactly `interval` apart; RFC 8628 §3.5 has the
  client add 5 s per `slow_down`, and `params.interval` says the new value outright.
- **Success**, in the same `Write`: `TrimDevices(user, 19)` (the cap of 20, §6.7) with one `device.revoked {via:
  "cap"}` row per evicted device; `CreateDevice` (name, kind, os and version from the code, `linked_via =
  'device_flow'`, `last_seen_at = now`, `last_ip` = this poll's IP); one access and one refresh token with
  `issued_by = NULL`; `ConsumeDeviceCode`; audit `device.linked`. After the commit: notify `{UserID}: devices`
  (03 §12.5) and close the evicted devices' connections (`device_revoked`).

Response 200 (`api.DeviceTokenResponse`, the same shape for both grants):

```json
{"accessToken": "isa_EXAMPLEaccessTOKEN0123456789abcdefghijklmnop", "tokenType": "Bearer", "expiresIn": 900,
 "refreshToken": "isr_EXAMPLErefreshTOKEN0123456789abcdefghijklmno", "refreshExpiresIn": 7776000,
 "device": {"id": "m4q8x2c6v0bn", "name": "Gaming PC"},
 "user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin"}}
```

### 5.5 Abuse guards

| Bucket | Key | Burst | Refill | Consumed by | Checked |
|---|---|---|---|---|---|
| `auth-ip` (03 §7.3, M1) | client IP key | 20 | 1 per 15 s | every `/device/code` and `/device/password` | first |
| `device-code-ip` | client IP key | 10 | 1 per 6 min | every `/device/code` | after `auth-ip` |
| `device-code-ip48` | the IPv6 client's /48 (`auth.IPKey48`); IPv4 clients skip it | 30 | 1 per 2 min | every `/device/code` from IPv6 | after `device-code-ip` |
| `device-token-ip` | client IP key | 60 | 1 per s | every `/device/token` (both grants) | first |
| `device-lookup` (03's `lookup`, re-keyed) | user ID | 10 | 1 per min | `/device/lookup` and `/device/decide` with a wrong code; taken before the read, refunded when the code is found | after the shape check |
| pending-code cap | server | 5,000 pending, unexpired codes | — | `/device/code` | last, before the insert |

- 03 keyed `lookup` by session ID; a user can hold 50 sessions (03 §7.4), so the user is the right key.
- **Why a /48 bucket**: the per-/64 keys alone let one holder of many /64s fill the server-wide cap and block every
  new link with 503 (a tunnel broker hands out a /48, 65,536 /64s, for free). A /48 is the usual allocation of one
  site or broker account. With 30 and then 1 per 2 min, one /48 creates at most 35 codes in its first 10 minutes (a
  code's lifetime) and 5 per 10 minutes after that, so filling the cap once takes about 143 /48s and keeping it full
  about 1,000. `auth.IPKey48(a netip.Addr) (netip.Prefix, bool)` unmaps first and returns
  false for IPv4 (beside M1's `IPKey`, `limiter.go`).
- `/device/password` (§14) takes exactly login's buckets in login's order (`auth-ip`, `auth-user-ip`, `auth-user`,
  `auth-hash`, then the hash semaphore, 03 §7.2–§7.3) and none of this table's device buckets: a password guess there
  costs what one at `/auth/login` costs, so the fallback opens no cheaper path to an account.
- Every bucket follows 03 §7.3: in memory, 100,000 keys per map, a blocked attempt answers 429 with `retryAfter` and
  `Retry-After`, does no hashing and no database access, and logs one `warn` line with `remote_ip` at most once per IP
  key per minute. The first refusal of an episode writes `auth.throttled`: scope `ip` for `device-code-ip` and
  `device-token-ip` (key: the IP) and for `device-code-ip48` (key: the /48 prefix), scope `device_lookup` for
  `device-lookup` (actor: the user, no key).
- **Guessing user codes is hopeless.** With at most 5,000 pending codes among 20⁸ ≈ 2.6 × 10¹⁰, one guess hits with
  probability ≤ 2 × 10⁻⁷. A user gets 10 guesses and then 1 per minute, 1,450 in the first day, so a whole day of one
  account's guessing finds a code with probability ≤ 3 × 10⁻⁴, and what it would find is somebody else's app, which
  then gets linked to the guesser's own account (the app would show that account). Device codes (256 bits) and refresh
  tokens (256 bits) can't be guessed; their buckets only bound load.
- **Remote phishing** (RFC 8628 §5.4): somebody starts a flow and sends a friend the `/link?code=` link. The page
  (§10.1) shows the code to compare with the app, the app and its version, the request's IP and age, and a warning plus
  a required "I started this myself" check whenever `network` is not `same` (`different`, or `unknown` with softer
  text: a phisher can force `unknown`, §5.3). `device.linked` is a security event on the admin dashboard, and the
  friend's Devices page lists the device. Codes live 10 minutes.

### 5.6 Device names

`auth.NormalizeDeviceName(in string) (string, error)`: trim spaces; `precis.Nickname` (the profile of room names,
03 §8); 1–64 code points. Errors are field codes: empty → `required`, over 64 → `too_long`, refused by PRECIS →
`invalid`. The app sends `os.Hostname()` cut to 64 code points, or `Windows PC` when that is empty; the approver may
replace it at `decide`, and the Devices page renames it later (`PATCH /me/devices/{id}`, §7.2). Names need not be
unique.

---

## 6. Server: device tokens

### 6.1 Bearer authentication on REST

- **Header grammar**: exactly one `Authorization` header whose value is `Bearer` (any case), one space, and an access
  token of the shape in §3. Anything else on a User or Admin route (another scheme, two headers, a refresh token, a
  wrong length) is 401 `invalid_token`, refused before hashing.
- **Lookup**: the bearer cache, else `DeviceTokenByHash(h, "access", now)`. Not found, expired, or a user who is not
  `active` → 401 `invalid_token`. Every 401 `invalid_token` carries `WWW-Authenticate: Bearer error="invalid_token"`.
- **Principal**: `auth.Principal{UserID, Username, Role, Method: MethodBearer, DeviceID, DeviceName: device.Name,
  ClientKind: device.ClientKind}`. `DeviceName` (new in M2) is set only for a bearer principal, from the device row.
- **Bearer cache**: token hash → (device, user, token expiry), TTL `min(30 s, token expiry − now)`. Every path that
  deletes a device or changes a user (03 §7.4's invalidation list) also forgets that device's or user's entries,
  synchronously, before `ConnCloser` runs.
- **Use**: `last_seen_at` and `last_ip` of the device are written at most once per 5 min per device (an in-memory
  timestamp, best effort, like sessions, 03 §7.4). `MaybeRotate` does nothing for a bearer principal (as built).
- A bearer request **ignores cookies completely**: the `Cookie` header is not read, and the cross-origin half of 03
  §7.5's CSRF check is skipped (there is no ambient credential to forge). The `Content-Type: application/json` rule
  still applies to unsafe methods.

### 6.2 Refresh: `POST /api/v1/device/token` with `grantType: refresh_token`

```json
{"grantType": "refresh_token", "refreshToken": "isr_EXAMPLErefreshTOKEN0123456789abcdefghijklmno",
 "exchangeId": "q9V3xT0bLm2Rk7aZ4cWfYg", "appVersion": "0.2.1"}
```

`exchangeId` is optional (16 random bytes the client draws per logical refresh and reuses for that refresh's retries,
memory only); `appVersion` is optional and, when valid (§5.2's rule), updates the device row, so the Devices page shows
what runs now. A malformed `exchangeId` is 422 `validation_failed {exchangeId: invalid}`; a malformed refresh token is
400 `invalid_grant`.

The new pair is generated before the `Write` (no `crypto/rand` inside it). Then, in one `Write`, with `P` the presented
token, `T` its `spent_at`, `C` the token its `replaced_by` names and `X` the HMAC of the request's `exchangeId`:

"`C` is P's own child" below means: `C` exists, is unspent, and `C.issued_by = P` (it came from P's own exchange). A
child that a grace path superseded is spent, and its successor has `issued_by` = the grand-parent, so a superseded
row never qualifies as a parent for a grace path. `X` "equals" `P.exchange_hash` only when both are set: a request
without `exchangeId` never takes the retry row.

| Case | Action | Answer |
|---|---|---|
| `P` unknown or expired | — | 400 `invalid_grant` |
| `P` unspent | `SpendRefreshToken(P, new refresh, X)`; insert the new access and refresh tokens with `issued_by = P`; `TrimAccessTokens(device, 5)`; `TouchDevice(ip, appVersion)` | 200 pair |
| `P` spent, `C` is P's own child, and `X` equals `P.exchange_hash` | the client's retry of the same refresh (**grace**): `DeleteAccessTokensIssuedBy(P)` (C's access token); `SupersedeRefreshToken(C, new refresh)` (C stays as a spent row until its own expiry, `exchange_hash` NULL); insert a new pair with `issued_by = P`; `SetReplacedBy(P, new refresh)` | 200 pair |
| `P` spent, `C` is P's own child, `now − T ≤ 120 s`, and `C.created_at = T` (no grace re-issue yet) | the answer was lost and the client restarted (**grace**): same as the row above; the new pair's `created_at` is `now ≠ T`, so a second replay falls through | 200 pair |
| any other spent `P` (its child was used, the window passed, a second replay, another `exchangeId` after the window, or `P` is a child that a grace path superseded) | **reuse**: `DeleteDevice` (cascades every token and the device's push subscriptions); audit `device.refresh_reused` | 400 `invalid_grant` with `params {reason: "reused"}` |

- **Why a grace path keeps C**: the client that takes a grace path never received C (its answer was lost), so it
  can't present C; only a second holder can. Example: the app rotates `P → C`; a process that copied `P` from
  Credential Manager (§11.5) replays it 100 s later and gets a pair, and C is superseded. When the app presents C at
  its next refresh, C is spent and its successor is not its own child, so the device is revoked with
  `device.refresh_reused` and the admin alert; the thief's pair dies with the device. The mirror case (the thief
  rotated first, the app replays `P` within 120 s) ends the same way when the thief presents its superseded child.
  Deleting C instead would make that presentation an unknown token: a plain `invalid_grant`, no alarm, and a live
  chain left with the other holder.
- After a committed reuse: the bearer cache forgets the device, `ConnCloser.CloseConnections({UserID, DeviceID},
  ReasonDeviceRevoked)` (01 sends `session_revoked`, 4401), the admin alert `refresh_token_reused` (03 §7.11; actor
  `system`, target the username), and `{UserID}: devices`.
- A user who is not `active` has no devices (03 §7.7 deletes them on disable and reset), so the table needs no status
  row; the code checks it anyway and answers `invalid_grant`.
- Why the exchange id: the client retries one refresh with the same `exchangeId` for as long as it runs (§11.4), so a
  network that drops answers for minutes never signs the app out, while an attacker, who never sees the id (memory
  only, sent only over TLS), can't use the retry path. The 120 s path covers a client restarted right after the
  server committed; a restart later than that after a lost answer is the one case that revokes an honest app, which
  then relinks with one browser approval.
- Rationale for revoking the whole device on reuse (RFC 9700 §4.14.2, "refresh token rotation"): one of the two
  holders is not the app, and the server can't tell which.

### 6.3 The WebSocket bearer path

01 built the shapes (01 §3.1, §3.2, §8.2; `upgrade.go` as merged). M2 changes:
1. **Origins**: `wire.go` sets `site.ExtraOrigins = protocol.DesktopOrigins()` right after `config.NewSite` (the slot
   M1 built for this: `config.Site.ExtraOrigins`, "later (M2) the Wails asset origins, added in code"). The hub gets
   it through the line M1 already has, `hubCfg.AllowedOrigins = s.site.ExtraOrigins` (`wire.go`, as built), and the
   REST API through `httpapi.Deps.DesktopOrigins = s.site.ExtraOrigins` (§6.4). `DesktopOrigins` returns a new slice on
   every call, so nothing else holds the site's copy; `config` keeps importing no protocol package. As built, an
   allowed origin is accepted only for a socket without a valid cookie, which then authenticates with `hello.auth`; a
   valid cookie from such an origin gets 403. The Go connection sends no `Origin` and takes the non-browser path.
2. **The bearer adapter** (`internal/server/wire.go`): `AuthenticateBearer(ctx, token)` calls
   `auth.AuthenticateBearerToken(ctx, token.Reveal())`; `*api.Error` `invalid_token` or `unauthenticated` →
   `signal.ErrInvalid` (the hub answers `unauthenticated`, 4401); other errors pass through (the hub answers
   `internal`). The `Identity` is `{UserID, Name: Username, Admin: Role == admin, DeviceID, DeviceName}`
   (`identityOf` copies the principal's `DeviceName`). `Admin` is set for a bearer identity exactly as for a cookie
   one (presence's admin flag and the admin-only `invalidate` topics, as built); only the REST admin routes refuse a
   bearer (§6.4).
   - **`signal.Identity.DeviceName string`** (new in M2, bearer identities only; "" for cookie sessions): the device's
     current name, which the hub puts into 01-client-core's `OwnConnection.DeviceName` (its §10.4). `LogValue` stays
     as built (user id and admin flag only), so the name is never logged.
3. **Revalidation**: `Revalidate` for an identity with a `DeviceID` already builds a bearer principal (as built);
   M2's `auth.Touch` checks that the device exists and its user is `active` (cache or one read), records the use
   (§6.1), and returns the principal with the device row's current `DeviceName` (§6.8: `Touch` now returns
   `(Principal, error)`). `Revalidate` copies it into the identity; a `DeviceName` that changed is applied like a
   changed `Name` (M1 01 §3.2), so a rename through `PATCH /me/devices/{id}` reaches open connections within the
   5-min revalidation. The access token's expiry doesn't matter after `hello` (01 §3.2): a connection lives until its
   device is revoked.
4. **Pre-auth refund**: a socket without a cookie takes a token of the hub's per-IP pre-auth bucket at the upgrade
   (20 per IP key per minute, 01 §3.1 step 4). M2 gives it back when its `hello` authenticates with a bearer
   (`keyedLimiter.refund`, beside the existing `h.authenticated(s)`), so the bucket counts only sockets that never
   authenticate. Without it, ten friends with the app behind one NAT (two bearer sockets each) would hit the limit
   when a server restart reconnects them all; M1 exempted cookie upgrades for the same reason.
5. The hub does not compare `hello.client` with the device row. `client.kind` is `desktop` for both the app's Go
   connection and its SPA (03-desktop-app), so 01's `minClientVersion` floor applies to both; the device's app version
   on the Devices page comes from refresh requests.

### 6.4 Credentials per route and CORS

**`internal/protocol/desktop.go`** (stdlib only):

```go
// DesktopAssetOriginWindows is the origin the desktop app's bundled SPA has on Windows (Wails v3's asset server in
// WebView2). 03-desktop-app passes it as its asset handler's Options.Origin and checks it against the pinned Wails.
const DesktopAssetOriginWindows = "http://wails.localhost"

// later (M3): const DesktopAssetOriginMacOS = "wails://localhost"

// DesktopOrigins returns a new slice of every origin a desktop app's SPA may have; DesktopOrigins()[0] is
// DesktopAssetOriginWindows.
func DesktopOrigins() []string
```

**`protocol.DesktopOrigins()`** returns, in this order: `DesktopAssetOriginWindows` (`http://wails.localhost`, Wails
v3, Windows), `wails://localhost` (Wails v3, macOS, M3; the value of the later `DesktopAssetOriginMacOS`), and
`https://wails.localhost`, `wails://wails`, `wails://wails.localhost` (spellings of other Wails builds, 03 §7.6). The
origin is no credential, so a spare entry costs nothing, and it keeps an app whose Wails bump changed the spelling
working against the previous server release (N−1). 03-desktop-app's test asserts that its pinned Wails serves the
bundled SPA from `DesktopAssetOriginWindows`; a release that needs a new spelling adds it here one release before any
app uses it. The server reads the list only through `Site.ExtraOrigins` (§6.3 item 1).

**Access levels** (`httpapi.Access`, M1 `Public`, `User`, `Admin`) gain two values; `User` now takes either
credential, and `Admin` stays cookie-only:

| Access | Credential | The other credential gets |
|---|---|---|
| `Public` | none read: the `Authorization` header is ignored (DesktopPlatform sends it on every call while it has a token, `/info` included, and none while unlinked, §11.8), cookies only matter to the handler's own logic, the CSRF check applies as in M1 | — |
| `User` | session cookie or bearer | — |
| `Admin` | session cookie only, of an admin | bearer (even an admin's): 403 `forbidden` with `params {credential: "session"}` |
| `UserSession` (new) | session cookie only | bearer: 403 `forbidden` with `params {credential: "session"}` |
| `UserDevice` (new) | bearer only | cookie: 403 `forbidden` with `params {credential: "device"}` |

`Admin` refuses a bearer because no desktop screen calls an admin route (03-desktop-app D7 opens `/admin/*` in the
system browser), while a linked admin PC holds a 90-day sliding refresh token that any process running as that
Windows user can read (§11.5). Accepting it would turn a stolen desktop credential into server administration
(deleting users, changing settings, reading the audit log with IPs) without the browser and its cookie ever being
involved. This changes M1 03 §7.12's sketch ("works on every user and admin endpoint"); DA12's write-back says "every
user endpoint and the WebSocket; admin routes cookie-only". A later client that needs admin power over a bearer
brings scoped tokens (*later*).

**The `/api/v1` chain** (03 §12.1 as built in `httpapi/api.go`) becomes, outermost first:

0. **CORS** (new, first so that every answer of the chain carries it): when `Origin` is one of `Deps.DesktopOrigins`
   (`wire.go` passes `s.site.ExtraOrigins`, §6.3 item 1):
   - a preflight (`OPTIONS` with `Access-Control-Request-Method`) is answered here and goes no further: when the method
     is one of `GET, POST, PUT, PATCH, DELETE` and `Access-Control-Request-Headers` lists only `authorization` and
     `content-type` (any case, any order), 204 with `Access-Control-Allow-Origin: <origin>`,
     `Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE`, `Access-Control-Allow-Headers: Authorization,
     Content-Type`, `Access-Control-Max-Age: 600`; otherwise 204 without them (the webview then refuses the request);
   - any other request: the `Cookie` header is removed (such a request never uses a cookie, even one a webview might
     hold), and the response gets `Access-Control-Allow-Origin: <origin>` and `Access-Control-Expose-Headers:
     Retry-After, WWW-Authenticate, X-Request-Id`;
   - every response to such an origin carries `Vary: Origin`; **never** `Access-Control-Allow-Credentials`.
   A request from any other origin passes untouched; its preflight reaches the route lookup and gets M1's 405, as now.
   (PUT is added to 03 §7.5's list for `PUT /push/preferences`.) Answers of 04's global chain (421, the shutdown 503, a
   recovered 500) carry no CORS headers; the SPA sees a network error and retries, which is what it does for those.
1. Body limit; 2. no-store; 3. route lookup: as built.
4. **Credentials** (replaces M1's `rejectAuthorization`): `Public` routes skip to step 5 without reading the header. On
   any other route, a request **with** an `Authorization` header takes the bearer path: §6.1 (401 `invalid_token`), no
   CSRF step, the principal is the device; a `UserSession` or `Admin` route then answers 403 `{credential:
   "session"}` as in the table (after the token checked out, so a bad token is still 401). A request **without** it
   takes M1's cookie path (steps 5–7), and a `UserDevice` route answers 403 after the cookie authenticated.
5. CSRF (cookie path and `Public` routes); 6. authenticate (cookie path); 7. rotate; 8. access (`Admin`: 403
   `forbidden` for a non-admin cookie session; a bearer never gets this far on an `Admin` route); 9. the handler.

Why skipping CSRF on the header's presence is safe: a cross-site page can send `Authorization` only after a preflight,
and only the desktop origins get one; the bearer path then reads no cookie, so no ambient credential is ever used.

**Route by route** (03 §12.3 numbers; every route not listed keeps its M1 access: a `User` route takes both
credentials, an `Admin` route only the cookie):

| # | Route | M2 access | Why |
|---|---|---|---|
| 4 | `POST /auth/logout-everywhere` | `User` | From the app it signs out everything, the app included; no cookie is cleared for a bearer |
| 12 | `POST /me/password` | `UserSession` | Account changes happen in the browser, never in the app (decision 10) |
| 13 | `POST /me/delete` | `UserSession` | Same |
| 14 | `GET /me/sessions` | `User` | For a bearer no entry is `current`, and M1's "own session must be live" check is skipped |
| 16 | `POST /me/sessions/revoke-others` | `UserSession` | "Other than this browser" has no meaning for a device |
| 17 | `GET /me/devices` | `User` | The caller's device is `current: true`; M1's "own session must exist" check becomes "own device must exist" for a bearer |
| 18 | `DELETE /me/devices/{id}` | `User` | A bearer may remove its own device: that is signing out (`device.revoked {via: "app"}`) |
| 21–23 | `GET /invites`, `POST /invites`, `DELETE /invites/{id}` | `User` (as built; the handler checks admin, `membersCanInvite` or the creator) | The desktop invite dialog uses them (03-desktop-app); an admin's bearer may create and revoke invites, which is what any member with `membersCanInvite` can do |
| 24, 28 | `POST /push/subscriptions`, `POST /push/test` | `UserSession` | A subscription belongs to a web session (03 §12.4.6); native push is *later* |
| 29–43, and M1 04's `GET\|POST /admin/doctor`, `GET /admin/bandwidth` | every `/api/v1/admin/*` route (#43 is M1 04's dashboard) | `Admin`, cookie only | Decision 10; the paragraph under the access table |
| 46, 47 | `POST /device/lookup`, `/device/decide` | `UserSession` | Decision 2 |
| 48 | `POST /device/password` | `Public` | The linking fallback (§14); sent by Go, never by the SPA, so it needs no CORS |
| 49 | `POST /device/revoke` | `UserDevice` | The app's own sign-out |
| 52 | `PATCH /me/devices/{id}` | `User` | Rename (§7.2) |

`Handle(pattern, access, h)` accepts the two new values; 04's routes keep theirs: `/conntest` `User` (both credentials;
the app itself runs M1's connection test in the browser, 03-desktop-app), the dashboard, doctor and bandwidth `Admin`
(cookie-only, like every admin route).

### 6.5 Revocation

03 §7.7's table holds; its two "later (M2)" rows (revoke device, refresh-token reuse) are built as written. M2 adds:

| Event | Web sessions | Devices and tokens | Push subscriptions | Live connections closed (reason) |
|---|---|---|---|---|
| The app's own sign-out (`POST /device/revoke`, or `DELETE /me/devices/{own id}` with its bearer) | — | that device | that device's | that device's (`device_revoked`) |
| Device cap eviction (the 21st device) | — | the evicted one | its | its (`device_revoked`) |
| Device expiry (janitor, §6.7) | — | devices without a live refresh token | theirs | none pushed: the hub's 5-min `Revalidate` finds the device gone and closes with `session_revoked` |

- Every revocation commits first, then forgets the user's or device's cache entries (session and bearer caches), then
  calls `ConnCloser` (03 §7.7's order).
- **Password change, admin password reset, admin sign-out, admin disable, account deletion, logout everywhere** already
  delete every device (03 §7.7, as built); their `ConnSelector{UserID}` closes the devices' connections too.
- "Revoke device" leaves the device codes the user decided (03 §7.7, as built); every row that deletes all devices
  also runs `DeleteDeviceCodesOf` (as built).
- **Admins revoking one device of another user** is *later (M6)*: an admin signs a user out everywhere or disables the
  account (03 §7.11); a per-device admin action adds a route and a page for a case friends don't have yet.
- The app's reaction to each of these is §11.3 and §11.4: the core stops (01-client-core §11.3), 03-desktop-app
  calls `SessionEnded`, its refresh gets `invalid_grant` → "Link this app" with a notice.

### 6.6 Session-key rotation, restore and other server events

- `isshoni admin rotate-secrets` with a new `session` key: every device, token and code is deleted at the next start
  (03 §4.6). Apps get `invalid_grant` and return to "Link this app".
- Restoring an older backup (04 §12.4): the database holds older refresh tokens than the apps, so their tokens are
  unknown (`invalid_grant`, no reuse detection) and they relink.
- A server restart changes nothing: codes, devices and tokens are rows.

### 6.7 Device cap, expiry and the janitor

- **Cap**: 20 devices per user (03 §7.12). Creating the 21st evicts the least recently seen one (§5.4).
- **Expiry**: a device is live while it has an unspent refresh token with `expires_at > now`. The janitor's hourly
  `Prune` (03 §4.7) gains, before its token rule:

```sql
DELETE FROM devices
 WHERE created_at < :now - 3600000
   AND NOT EXISTS (SELECT 1 FROM device_tokens t
                    WHERE t.device_id = devices.id AND t.kind = 'refresh'
                      AND t.spent_at IS NULL AND t.expires_at > :now)
RETURNING id, user_id, name
```

  with one `device.expired` audit row (actor `system`) per device, in the same transaction, and `PruneStats.Devices`.
  The hour keeps the rule clear of a device being created (its first pair is inserted in the same `Write` anyway). As
  in M1 the janitor pushes nothing: the hub's `Revalidate` closes a connection whose device went (§6.5).
- Token rows: access tokens are trimmed to the newest 5 per device at every insert and pruned at expiry; spent refresh
  tokens stay until their own expiry (03 §4.7, as designed), which is what lets a replay weeks later still be caught.
  The app refreshes only when it needs a token with less than 120 s left (§11.4), so an app used 4 hours a day
  refreshes about 16 times a day and its spent chain is about 1,500 rows (a few hundred KB) after 90 days.

### 6.8 Auth Go API (M2)

"CHANGED" is relative to 03 §7.13's sketch; M1's code declares only `AuthenticateBearerToken` and `DecideUserCode`, as
not-implemented stubs (README §3, "Carried to later milestones").

```go
package auth

// ---- device flow ----
func (s *Service) StartDeviceFlow(ctx context.Context, in api.DeviceCodeRequest, m ReqMeta) (api.DeviceCodeResponse, error)
// LookupUserCode (CHANGED: takes m, for the network comparison of §5.3).
func (s *Service) LookupUserCode(ctx context.Context, p Principal, userCode string, m ReqMeta) (api.DeviceLookup, error)
// DecideUserCode (CHANGED: takes the request, for deviceName).
func (s *Service) DecideUserCode(ctx context.Context, p Principal, in api.DeviceDecideRequest, m ReqMeta) error
// DeviceToken serves both grants (§5.4, §6.2).
func (s *Service) DeviceToken(ctx context.Context, in api.DeviceTokenRequest, m ReqMeta) (api.DeviceTokenResponse, error)
// PasswordDeviceLogin is POST /device/password (§14): login's order and buckets, then the device_code grant's
// success Write with linked_via = 'password'. Errors as Login's, plus 422 for the device fields.
func (s *Service) PasswordDeviceLogin(ctx context.Context, in api.DevicePasswordRequest, m ReqMeta) (api.DeviceTokenResponse, error)

// ---- bearer ----
// AuthenticateBearerToken checks an access token (hello.auth, and the REST bearer path through Authenticate). The
// principal carries DeviceName from the device row.
// Errors: *api.Error{invalid_token}; any other failure wraps *api.Error{internal}.
func (s *Service) AuthenticateBearerToken(ctx context.Context, token string) (Principal, error)

// Principal (M1) gains:
//	DeviceName string // Method == MethodBearer (M2): the device's name; "" for sessions; never logged

// ---- devices ----
func (s *Service) RenameDevice(ctx context.Context, p Principal, id store.DeviceID, name string, m ReqMeta) (store.Device, error)
func (s *Service) RevokeOwnDevice(ctx context.Context, p Principal, m ReqMeta) error // POST /device/revoke

// ---- changed for bearer principals ----
// Authenticate: an Authorization header → the bearer path (§6.1), the cookie is not read; else the cookie (M1).
// Touch (CHANGED: returns the principal): func (s *Service) Touch(ctx context.Context, p Principal, ip netip.Addr)
// (Principal, error). A session principal comes back unchanged; a bearer principal checks its device row and user
// (§6.3) and comes back with the row's current DeviceName. wire.go's toucher interface and Revalidate follow.
// RevokeDevice: allowed for a bearer; its own ID is the app's sign-out.
// LogoutEverywhere: allowed for a bearer; deletes every session and device, its own included.
// ChangePassword, DeleteSelf, RevokeOtherSessions keep refusing a bearer (the routes are UserSession, §6.4).
// The admin methods (03 §7.11, M1 04's ops routes) are reached only by cookie principals: their routes are Admin,
// which refuses a bearer (§6.4). They need no bearer check of their own.

func NormalizeDeviceName(in string) (string, error) // §5.6; field codes as errors
```

`ReasonDeviceRevoked` (as built) is used for every device row of §6.5; `AdminAlert` kind `refresh_token_reused`
(03 §7.11) is raised after a committed reuse. New limiter rates in `limiter.go`:

```go
var (
	rateDeviceCodeIP   = rate{Burst: 10, Every: 6 * time.Minute}
	rateDeviceCodeIP48 = rate{Burst: 30, Every: 2 * time.Minute} // IPv6 /48 (§5.5)
	rateDeviceTokenIP  = rate{Burst: 60, Every: time.Second}
	rateDeviceLookup   = rate{Burst: 10, Every: time.Minute}
)

// IPKey48 is device-code-ip48's key: the /48 of an IPv6 address (IPv4-mapped addresses are unmapped first, zone
// dropped); false for IPv4 and the zero Addr, which skip the bucket.
func IPKey48(a netip.Addr) (netip.Prefix, bool)

const maxPendingDeviceCodes = 5000 // §5.2 step 4
```

---

## 7. REST API

### 7.1 Endpoint table (03 §12.3, M2 rows)

Common errors are 03's (`bad_request`, `payload_too_large`, `unsupported_media_type`, `csrf_failed`, `rate_limited`,
`internal`; `unauthenticated` for `User`/`Admin`), plus 401 `invalid_token` on every non-`Public` route for a bearer,
and 403 `forbidden {credential: session}` on every `Admin` route (`/api/v1/admin/*`) for a valid bearer (§6.4).
Bodies are limited to 16 KiB under `/api/v1/device/` (as built).

| # | Method and path | Access | Success | Endpoint-specific errors |
|---|---|---|---|---|
| 44 | `POST /api/v1/device/code` | Public | 200 `DeviceCodeResponse` | 429 `rate_limited` (`auth-ip`, `device-code-ip`, `device-code-ip48`), 422 `validation_failed {clientKind\|os\|deviceName\|appVersion}`, 503 `server_busy` (pending-code cap of 5,000) |
| 45 | `POST /api/v1/device/token` | Public | 200 `DeviceTokenResponse` | 400 `authorization_pending`, `slow_down {interval}`, `access_denied`, `expired_token`, `invalid_grant {reason?}`; 422 `validation_failed {grantType\|exchangeId\|appVersion}` |
| 46 | `POST /api/v1/device/lookup` | UserSession | 200 `DeviceLookup` | 403 `forbidden {credential: session}`, 404 `device_code_invalid`, 422 `{userCode}` |
| 47 | `POST /api/v1/device/decide` | UserSession | 204 | as #46, plus 422 `{deviceName}` |
| 48 | `POST /api/v1/device/password` | Public | 200 `DeviceTokenResponse` | as #2 (§14): 401 `invalid_credentials`, 403 `account_pending`/`account_disabled`, 422 `validation_failed {username\|password\|clientKind\|os\|deviceName\|appVersion}`, 429 `rate_limited`, 503 `server_busy` |
| 49 | `POST /api/v1/device/revoke` | UserDevice | 204 | 403 `forbidden {credential: device}` |
| 52 | `PATCH /api/v1/me/devices/{id}` | User | 200 `{device}` | 404 `not_found` (not the caller's), 422 `{name}` |

The changed access of M1 routes is §6.4's table. `GET /api/v1/info` lists `deviceFlow` in `features` on every M2
server (the constant `api.InfoFeatureDeviceFlow` exists since M1):

```json
{"server": {"name": "Alex's server", "version": "0.2.0", "publicUrl": "https://watch.example.com"},
 "protocol": {"current": 1, "min": 1}, "minClientVersion": "", "features": ["push", "passwordReset", "deviceFlow"], "…": "…"}
```

### 7.2 Requests and responses (beyond §5 and §6.2)

`POST /api/v1/device/revoke` `{}` with `Authorization: Bearer isa_…` → 204. The device, its tokens and its push
subscriptions go; audit `device.revoked {via: "app"}`; its connections close (`device_revoked`); `{UserID}: devices`.

`PATCH /api/v1/me/devices/{id}` `{"name": "Gaming PC"}` → `200 {"device": DeviceInfo}`. Audit `device.renamed {from,
to}`; `{UserID}: devices`. A name equal to the current one changes nothing and writes nothing.

`GET /api/v1/me` for a bearer (03 §12.4.3 planned it) has `device` instead of `session`:

```json
{"user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin", "createdAt": "2026-10-01T12:00:00.000Z"},
 "device": {"id": "m4q8x2c6v0bn", "name": "Gaming PC", "clientKind": "desktop", "os": "windows",
            "appVersion": "0.2.0", "createdAt": "2026-11-02T19:05:01.004Z", "current": true},
 "permissions": {"admin": true, "createInvites": true}, "badges": {"pendingApprovals": 0}}
```

`GET /api/v1/me/devices` items gain `"current": true` for the caller's own device (bearer only; always `false` for a
cookie caller):

```json
{"devices": [{"id": "m4q8x2c6v0bn", "name": "Gaming PC", "clientKind": "desktop", "os": "windows",
              "appVersion": "0.2.1", "createdAt": "…", "lastSeenAt": "…", "lastIp": "203.0.113.7", "current": false}]}
```

A CORS preflight from the desktop SPA:

```
OPTIONS /api/v1/rooms HTTP/1.1
Origin: http://wails.localhost
Access-Control-Request-Method: GET
Access-Control-Request-Headers: authorization

HTTP/1.1 204 No Content
Access-Control-Allow-Origin: http://wails.localhost
Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE
Access-Control-Allow-Headers: Authorization, Content-Type
Access-Control-Max-Age: 600
Vary: Origin
Cache-Control: no-store
```

### 7.3 DTOs (`internal/protocol/api/device.go`)

```go
package api

type DeviceCodeRequest struct {
	ClientKind string `json:"clientKind"` // desktop | mobile
	DeviceName string `json:"deviceName"`
	OS         string `json:"os"`         // windows | macos | linux | ios | android
	AppVersion string `json:"appVersion"`
}

type DeviceCodeResponse struct {
	DeviceCode              string `json:"deviceCode"` // redacted in fmt/slog
	UserCode                string `json:"userCode"`   // "WDJB-MJHT"
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"` // seconds
	Interval                int    `json:"interval"`  // seconds
}

type GrantType string

const (
	GrantTypeDeviceCode   GrantType = "device_code"
	GrantTypeRefreshToken GrantType = "refresh_token"
)

type DeviceTokenRequest struct {
	GrantType    GrantType `json:"grantType"`
	DeviceCode   string    `json:"deviceCode,omitempty"`   // device_code grant; redacted
	RefreshToken string    `json:"refreshToken,omitempty"` // refresh_token grant; redacted
	ExchangeID   string    `json:"exchangeId,omitempty"`   // refresh_token grant, optional; redacted
	AppVersion   string    `json:"appVersion,omitempty"`   // refresh_token grant, optional
}

type DeviceTokenResponse struct {
	AccessToken      string    `json:"accessToken"`  // redacted
	TokenType        string    `json:"tokenType"`    // "Bearer"
	ExpiresIn        int       `json:"expiresIn"`    // 900
	RefreshToken     string    `json:"refreshToken"` // redacted
	RefreshExpiresIn int       `json:"refreshExpiresIn"` // 7776000
	Device           DeviceRef `json:"device"`
	User             User      `json:"user"`
}

type DeviceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DeviceLookupRequest struct {
	UserCode string `json:"userCode"`
}

type NetworkMatch string

const (
	NetworkSame      NetworkMatch = "same"
	NetworkDifferent NetworkMatch = "different"
	NetworkUnknown   NetworkMatch = "unknown"
)

type DeviceLookup struct {
	UserCode    string       `json:"userCode"` // formatted
	ClientKind  string       `json:"clientKind"`
	OS          string       `json:"os"`
	DeviceName  string       `json:"deviceName"`
	AppVersion  string       `json:"appVersion"`
	RequestIP   string       `json:"requestIp"`
	Network     NetworkMatch `json:"network"`
	RequestedAt time.Time    `json:"requestedAt"`
	ExpiresAt   time.Time    `json:"expiresAt"`
}

type DeviceDecideRequest struct {
	UserCode   string  `json:"userCode"`
	Approve    bool    `json:"approve"`
	DeviceName *string `json:"deviceName,omitempty"` // absent = keep the app's name
}

// DevicePasswordRequest is POST /device/password (#48, §14); M1 03 §12.4.7's sketch.
type DevicePasswordRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`   // redacted in fmt/slog
	ClientKind string `json:"clientKind"` // as DeviceCodeRequest
	DeviceName string `json:"deviceName"`
	OS         string `json:"os"`
	AppVersion string `json:"appVersion"`
}

type RenameDeviceRequest struct {
	Name string `json:"name"`
}

type DeviceResponse struct {
	Device DeviceInfo `json:"device"`
}

// DeviceInfo (M1) gains:
//	Current bool `json:"current"` // the caller's own device (bearer); M2

func NormalizeUserCode(s string) (string, bool) // §3
func FormatUserCode(normalized string) string   // "WDJBMJHT" → "WDJB-MJHT"
```

Golden JSON for each type goes in `internal/protocol/api/testdata/`; `task gen` regenerates
`web/src/protocol/api.gen.ts`.

---

## 8. Audit, alerts, notifications and logs

**Audit rows** (03 §10; written in the same transaction as the change; Sec = security event on the dashboard):

| Action | Actor | Target | Detail | IP column | Sec |
|---|---|---|---|---|---|
| `device.approved` | user | device (no id; name = the device name) | `{clientKind, os}` | the approver's | |
| `device.denied` | user | device (no id; name) | `{clientKind, os}` | the approver's | |
| `device.linked` | user (the approver; for `password`, the account) | device | `{clientKind, os, via: "device_flow" \| "password"}` | the app's (the poll, or the password request) | ✓ |
| `device.renamed` | user | device | `{from, to}` | the caller's | |
| `device.revoked` | user (the owner, on the Devices page or in the app) or system (`cap`) | device | `{via: "devices_page" \| "app" \| "cap"}` (M1 wrote `{}`; `via` is additive) | the caller's; none for `cap` | |
| `device.refresh_reused` | system | device | `{}` | the presenter's | ✓ |
| `device.expired` | system | device | `{}` | — | |
| `auth.throttled` | anonymous, or the user for `device_lookup` | — | `{scope: "ip", key}` or `{scope: "device_lookup"}` | the client's | ✓ |
| `auth.login_failed` (M1) | anonymous | user (only if it exists) | as M1, also for a failed `/device/password` | the client's | as M1 |

`TestAuditActionsInSource` and `TestAuditCoverage` (03 §10, README S70) gain these rows: every action above is produced
by a real request (or the janitor) and found in `GET /api/v1/admin/audit` with its kinds and IP.

**Admin alert**: `refresh_token_reused` (03 §7.11), after the commit, to every admin with `adminAlerts` on.

**Notifications** (`invalidate` topics, 03 §12.5): `{UserID}: devices` after a device is linked, renamed or revoked
(any `via`), and after a reuse. Approve and deny notify nothing (the approval page has the answer).

**Logs** (README §4 Logging): `info` "device linked" with `user_id`, `device_id` and `via`; `warn` "refresh token
reused, device revoked" with `user_id`, `device_id` and `remote_ip` (a security event); throttle lines as in §5.5.
Never: tokens, device codes, user codes, exchange ids, device names, passwords, request bodies.

---

## 9. Privacy

The plan's list ("sessions and devices: token hashes, name, last seen, last IP") holds. 03 §11's two "later (M2)" rows
become:

| Data | Table | Why | Kept until |
|---|---|---|---|
| Per linked app: the name (the computer's name unless the friend changed it), app kind, OS, app version, created and last-seen times, **last IP**, token hashes | `devices`, `device_tokens` | Native apps; the Devices page | Revoked; 90 days without a refresh (+ ≤ 1 h); tokens at their expiry |
| Pending app links: the same, plus the requesting IP | `device_codes` | The approval page | ≤ 70 min |

- Only the friend sees a device's name, last-seen time and last IP (Devices page); admins see a count per user and
  the audit log (30 days), as in M1.
- **On the PC**: the refresh token, device ID and username in Windows Credential Manager; the server list (origins,
  server names, usernames, device IDs) in `servers.json` (§11.6). No access token or password is written to disk.
- `docs/privacy.md`: slice DA12 adds only the server-side device rows of the table above. The "On the PC" facts go to
  06-windows-release's `#desktop` section (its R14), which also removes "which come later"; DA12 hands them over and
  edits neither.

---

## 10. Web: the approval page and the Devices page

### 10.1 `/link` (DeviceLinkPage)

- **Route**: `web/src/app/router.tsx` replaces `{ path: 'link', Component: NotFound }` with
  `{ path: 'link', ...page('link', 'DeviceLinkPage') }`, still under `RequireAuth`, so a signed-out browser goes to
  `/login?next=%2Flink%3Fcode%3DWDJB-MJHT` and comes back after logging in. The folder `web/src/link/` exports
  `DeviceLinkPage` from `index.ts` and imports its lazy namespace `i18n/lazy/link`.
- **Reading the code**: `?code=` is normalized with `normalizeUserCode` and removed from the address bar with
  `history.replaceState` (it stays in component state), so it doesn't linger in history.
- **The desktop SPA** never shows this page (03-desktop-app leaves `/link` out of its routes): approval needs a cookie
  session, which the app doesn't have.

| State | Shown | Actions |
|---|---|---|
| `enter` (no or malformed code) | "Enter the code shown in the isshoni app" | code field (one input, `autocapitalize="characters"`, `-` inserted after 4 letters; Continue is enabled at 8 valid letters) |
| `loading` | spinner | — |
| `confirm` (`network: same`) | heading "Link the isshoni app?"; right under it, in every `confirm` variant, the code in large monospace with "Make sure it matches the code in the app"; "isshoni for Windows 0.2.0"; **Name for this computer** (prefilled, editable, §5.6); "Requested 1 minute ago from 203.0.113.7 (this browser's network). People on the same Wi-Fi or mobile carrier can share one address, so compare the code too."; "The app will act as **Alex** on Alex's server: watch, share and change your settings"; "Signed in as Alex. Not you? Sign out" | **Link**, **Don't link** |
| `confirm` with `network: different` | as above without "(this browser's network) …", plus a warning: "This request came from a different network than this browser. Only continue if you started it yourself, for example on another computer or on mobile data." and a checkbox **I started this myself**, required before Link is enabled; Link reads "Link anyway" | same |
| `confirm` with `network: unknown` | as above without "(this browser's network) …", plus a softer note: "isshoni couldn't check whether this request came from your network. Only continue if you started it yourself." and the same required checkbox **I started this myself**; Link keeps its label | same |
| `linked` | "Linked. Go back to the app; it continues by itself." | — |
| `denied` | "Not linked. The app will show that you said no." | — |
| `invalid` (404 `device_code_invalid`) | "This code is wrong, was already used or has expired. Start again in the app." | code field |
| `rate_limited` (429) | "Too many wrong codes. Try again in {minutes} min." | — |
| `error` | 03's generic error with the request id | Try again |

- **Which variant**: only `same` skips the checkbox; any other value, including one the page doesn't know, shows the
  `unknown` note at least and requires the checkbox (the check fails closed).
- **Accessibility**: the heading takes focus on each state change; the result states are announced (`aria-live
  polite`); the code is read letter by letter (`aria-label="W D J B, M J H T"`); the checkbox is a labeled native
  input, and the warning or note is its `aria-describedby`.
- **i18n**: namespace `link` in `web/src/i18n/lazy/link.en.json` (the shared-files rule of README §5: only a lazy page
  folder uses it); `errors.device_code_invalid` and `errors.invalid_token` in `en.json`. App names come from
  `link.app.<clientKind>.<os>` ("isshoni for Windows", "isshoni for macOS", "isshoni Linux helper").
- No GET ever approves: approval is the Link button's POST, under 04's `frame-ancestors 'none'` and 03's CSRF check.

### 10.2 Devices page

- `web/src/account/DevicesPage.tsx` (05 §15; M1 lists browsers and an always empty app list): each app row shows the
  name, `link.app.<kind>.<os>` with the version, "Linked {date}", "Last seen {relative} from {ip}", **Rename** (an
  inline field, `PATCH`) and **Remove** (a confirmation: "{name} will be signed out and has to be linked again"). Only
  when `current` is true does the row show a **This app** badge and Remove read "This app will be signed out".
- The page runs only in a browser with a cookie session in M2: 03-desktop-app opens `/account`, `/account/devices` and
  `/account/notifications` in the system browser (`RequireBrowser`), and the desktop SPA calls `/me/devices` not at
  all. So `current` is always false in M2, and there is no desktop variant of AccountPage or DevicesPage. `/me.device`
  and `DeviceInfo.current` (§7.2) stay as room for clients that show these pages with a bearer (*later*: the mobile
  apps, M4's agent).
- `web/src/lib/appLink.ts`: `appLinkFor(serverOrigin: string, roomId?: string): string` builds the deep link of §12
  for 03-desktop-app's and 05's "Open in the app" buttons.

---

## 11. Client side (Go)

### 11.1 Packages and import rules

- `internal/client/deviceauth` imports the standard library, `internal/protocol`, `internal/protocol/api`,
  `internal/version`, `internal/client/credstore`, `internal/client/compat` and `internal/client/deeplink`. It stays
  gomobile-friendly (no cgo, no OS calls): the pending mobile apps can reuse it with their own `credstore.Store`.
- `internal/client/credstore` imports the standard library and, in `wincred_windows.go` only,
  `golang.org/x/sys/windows`. This is allowed by 01-client-core's D2 rule (the depguard note of M1 06 §8.2 put in
  force: an in-repo allowlist plus the `internal/client/...` siblings; third-party modules governed by the license
  gate), so no exception is needed.
- `internal/client/deeplink` imports the standard library and `golang.org/x/net/idna`.
- `internal/client/compat` imports the standard library, `internal/protocol`, `internal/protocol/api` and
  `internal/version`.
- `internal/version` is a leaf (04 §2) that M2's client may import (04 §2 already says "needed by … `internal/client`
  and the desktop app later (M2)").

### 11.2 `deviceauth.Client`: the REST calls

```go
package deviceauth

// AppInfo describes this app to the server. 03-desktop-app fills it once at start.
type AppInfo struct {
	Version    string             // version.Version()
	ClientKind protocol.ClientKind // ClientKindDesktop
	OS         protocol.ClientOS   // ClientOSWindows
	DeviceName string             // os.Hostname() cut to 64 code points, or "Windows PC"
}

// Client talks to one server's /api/v1. Safe for concurrent use.
type Client struct{ /* … */ }

// NewClient takes an origin from deeplink.NormalizeOrigin (https; http only for loopback with
// Options.AllowInsecureLoopback).
func NewClient(origin string, app AppInfo, o ClientOptions) (*Client, error)

type ClientOptions struct {
	HTTP                  *http.Client // nil = DefaultHTTPClient()
	AllowInsecureLoopback bool         // dev builds: http://localhost, 127.0.0.1, [::1]
}

// DefaultHTTPClient: Timeout 15 s, TLS ≥ 1.2, the system roots, ProxyFromEnvironment, and CheckRedirect returning
// http.ErrUseLastResponse: a 307/308 would re-send a token body to another location.
func DefaultHTTPClient() *http.Client

func (c *Client) Info(ctx context.Context) (api.Info, error)
// StartLink is POST /device/code. ErrBadResponse unless the answer passes the verification-URI check below; on success
// VerificationURI and VerificationURIComplete are replaced by Go's canonical strings.
func (c *Client) StartLink(ctx context.Context) (api.DeviceCodeResponse, error)
func (c *Client) Poll(ctx context.Context, deviceCode string) (api.DeviceTokenResponse, error)
// PasswordLink is POST /device/password (§14) with AppInfo's kind, OS, version and device name.
func (c *Client) PasswordLink(ctx context.Context, username string, password protocol.Secret) (api.DeviceTokenResponse, error)
func (c *Client) Refresh(ctx context.Context, refreshToken, exchangeID string) (api.DeviceTokenResponse, error)
func (c *Client) Revoke(ctx context.Context, accessToken string) error

var (
	ErrNetwork     = errors.New("deviceauth: network error")
	ErrBadResponse = errors.New("deviceauth: unexpected response")
)
```

- Every request sends `User-Agent: isshoni-desktop/<version>` (the same string 06-windows-release §8.4 sends to
  GitHub and documents on `/privacy#desktop`; the server learns the OS from `/device/code` and `hello.client`),
  `Content-Type: application/json`, and no `Origin` (03 §7.5: a request without browser headers passes the CSRF
  check).
- A non-2xx answer with the envelope returns the `*api.Error` (with `RetryAfter`); a transport failure is
  `fmt.Errorf("%w: %w", ErrNetwork, cause)`, so `errors.Is(err, ErrNetwork)` holds and `errors.As` still reaches a
  `*tls.CertificateVerificationError` and the `x509` error types in `cause` (03-desktop-app's probe
  tells `tls_untrusted` from other network errors this way); an unexpected body wraps `ErrBadResponse`. Response
  bodies are read up to 64 KiB.
- **Verification URIs are checked** before `StartLink` returns, because 03-desktop-app opens `verificationUriComplete`
  with the shell, draws it as the QR code and copies it with [Copy link], and a malicious or compromised server must
  not be able to hand the app `file:`, `search-ms:`, `ms-*:`, a UNC path or a look-alike site. Each of
  `verificationUri` and `verificationUriComplete` must parse with `url.Parse` and have: scheme `https` (`http` only for
  the loopback hosts `AllowInsecureLoopback` allows); no `Opaque` part, no userinfo, no fragment (and no `#` in the
  raw string); `deeplink.NormalizeOrigin(scheme + "://" + host, AllowInsecureLoopback)` equal to the client's origin
  (normalized components, not raw strings: `Site.Origin` and the adopted `publicUrl` may differ in case or a default
  `:443`); path exactly `/link`. `verificationUri` has no query (`RawQuery == ""`, no trailing `?`);
  `verificationUriComplete`'s `RawQuery` is exactly `code=` + `api.FormatUserCode(n)`, where `n` is
  `api.NormalizeUserCode(userCode)`, which must succeed. `deviceCode` must have §3's shape, and `interval` and
  `expiresIn` must be positive. Any failure → `ErrBadResponse` (03-desktop-app shows its generic link error), and
  nothing is opened. On success the `LinkState` (§11.3) carries canonical strings that Go builds itself,
  `origin + "/link"` and `origin + "/link?code=" + FormatUserCode(n)`, never the server's raw strings.
- Go reads no Windows system proxy settings (WinINET); `HTTPS_PROXY` works. A known M2 limit (WebView2 does use the
  system proxy, so on a PC that needs it the server can open in the browser or the app's viewer while Go's requests
  fail), explained in `docs/troubleshooting.md#desktop-linking` with the `HTTPS_PROXY` workaround; 03-desktop-app's
  `server_unreachable` and `publisher_offline` texts link there ("Opens in your browser but not here? See why").
  *Later (M5)* may read the system proxy.

### 11.3 `deviceauth.Manager`: one server's link

```go
type Status string

const (
	StatusUnlinked     Status = "unlinked"     // no credential: show "Link this app"
	StatusLinking      Status = "linking"      // a code is shown; polling
	StatusLinked       Status = "linked"
	StatusOffline      Status = "offline"      // linked, but the server can't be reached now
	StatusIncompatible Status = "incompatible" // compat says update_app or update_server
)

// Notice says why the state changed when the user didn't do it. Shown once, then cleared by the UI.
type Notice string

const (
	NoticeNone            Notice = ""
	NoticeDenied          Notice = "denied"           // access_denied
	NoticeCodeExpired     Notice = "code_expired"     // expired_token / invalid_grant while linking
	NoticeSignedOut       Notice = "signed_out"       // invalid_grant at a refresh; device revoked or expired
	NoticeRefreshReused   Notice = "refresh_reused"   // invalid_grant {reason: reused}
	NoticeAccountDisabled Notice = "account_disabled" // SessionEnded(account_disabled), then invalid_grant
	NoticeSaveFailed      Notice = "save_failed"      // credstore write failed; linked for this run only
)

type LinkState struct {
	UserCode                string    // "WDJB-MJHT" (FormatUserCode of the checked code)
	VerificationURI         string    // origin + "/link", built by Go after §11.2's check
	VerificationURIComplete string    // origin + "/link?code=" + UserCode; also the QR code's content (03-desktop-app draws it)
	ExpiresAt               time.Time
}

type State struct {
	Origin     string
	ServerName string          // from /info
	Status     Status
	Link       *LinkState      // StatusLinking
	User       *api.User       // linked, offline
	Device     *api.DeviceRef  // linked, offline
	Notice     Notice
	Compat     compat.Check    // StatusIncompatible (and the last check otherwise)
}

type ManagerOptions struct {
	Client  *Client
	Store   credstore.Store
	OnState func(State)       // called on every change, never while holding a lock
	Clock   func() time.Time  // monotonic-safe; tests inject
	Rand    io.Reader         // exchange ids; nil = crypto/rand
	Logger  *slog.Logger
}

func NewManager(o ManagerOptions) *Manager
// Start loads the credential and refreshes once: linked, unlinked, offline or incompatible. It never blocks longer
// than ctx; a network failure leaves StatusOffline and retries in the background (§11.4).
func (m *Manager) Start(ctx context.Context) error
// Link checks compat (/info) and starts a flow. It returns once StatusLinking is reached, or with an error; ctx
// bounds only the /info and /device/code requests. Polling then runs on the manager's own context (not the caller's:
// a Wails binding's ctx ends with the JS call, 03-desktop-app W11) until linked, denied or expired, CancelLink, or
// Close.
func (m *Manager) Link(ctx context.Context) error
func (m *Manager) CancelLink()
// LinkWithPassword is the fallback of §14: /info → compat.FromInfo (not ok → StatusIncompatible, ErrIncompatible),
// then POST /device/password; a 200 takes the steps of a poll's success (Linking, step 4). It returns when linked or
// with the error: *api.Error (invalid_credentials, account_pending, account_disabled, rate_limited with RetryAfter,
// server_busy, validation_failed), ErrNetwork or ErrIncompatible; the state stays unlinked on an error and a running
// device-flow link is cancelled first. The password is passed on once, never stored, retried or logged.
func (m *Manager) LinkWithPassword(ctx context.Context, username string, password protocol.Secret) error
// AccessToken returns a token with at least 120 s left, refreshing when needed (single flight). It satisfies
// 01-client-core's core.Auth. forceRefresh=true means the server refused the token the caller had: the cached token
// is dropped and refreshed (single flight). At most one forced refresh per 10 s: a forced call within 10 s of the last
// forced refresh gets the token that refresh issued, without a new request.
// Errors: ErrNotLinked (RelinkRequired), ErrIncompatible (UpdateRequired), ErrOffline (transient; wraps the
// network error), ctx's error.
func (m *Manager) AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)
// AccessTokenWithExpiry is AccessToken plus its expiry, for the desktop SPA (§11.7). Same forceRefresh rules.
func (m *Manager) AccessTokenWithExpiry(ctx context.Context, forceRefresh bool) (protocol.Secret, time.Time, error)
// SessionEnded reports that a connection was stopped with unauthenticated (a second one, after the core's forced
// refresh), session_revoked or account_disabled; any other code is ignored and State() returned. It is synchronous:
// it makes one refresh attempt, bounded by ctx, and returns the resulting state. invalid_grant → unlinked with the
// notice of §11.4. Success → the manager stays linked: the Go core has already stopped (01-client-core §11.3), and
// 03-desktop-app starts a new core client when State().Status is linked. Concurrent calls (C2's adapter and the
// SPA's binding) share one attempt (single flight). A network failure → offline, as in §11.4. Already unlinked (the
// core's forced refresh got invalid_grant): no request, the unlinked state is returned.
func (m *Manager) SessionEnded(ctx context.Context, code protocol.ErrorCode) State
// HelloRefused reports a hello refused with client_outdated or protocol_unsupported (either connection: 03's C2
// adapter, and the SPA through the binding); other codes and nil are ignored. It runs compat.FromError(e,
// compat.Default()): a decided check that is not OK → SetIncompatible; an undecided one (protocol_unsupported
// without params.serverMax, §13.2) → Recheck on the manager's own context.
func (m *Manager) HelloRefused(e *protocol.Error)
// SetIncompatible enters StatusIncompatible with c and keeps the credential; a check whose Verdict is OK is ignored.
func (m *Manager) SetIncompatible(c compat.Check)
// Recheck re-runs /info → compat.FromInfo. Verdict ok: the state Start would reach (linked after one refresh when a
// credential exists, else unlinked). Otherwise StatusIncompatible with the new check. A network failure returns the
// ErrNetwork error and leaves the state as it was.
func (m *Manager) Recheck(ctx context.Context) (State, error)
// Unlink signs this app out: POST /device/revoke (10 s, best effort), then deletes the credential. It returns
// ErrRevokeNotConfirmed (still unlinked locally) when the server couldn't be reached.
func (m *Manager) Unlink(ctx context.Context) error
func (m *Manager) State() State
func (m *Manager) Close() error // stops polling and timers; waits for its goroutines
```

**Errors** (`errors.go`). 01-client-core's core decides "unlinked" only from an `Auth` error whose chain has a
`RelinkRequired() bool` method returning true, and treats every other error as transient (its §11.2); these values
give it that answer:

```go
// ErrNotLinked: no credential exists, or this call's refresh got invalid_grant (the credential is deleted before
// the call returns). AccessToken returns an error wrapping it (errors.Is(err, ErrNotLinked)).
var ErrNotLinked error = notLinked{}

type notLinked struct{}

func (notLinked) Error() string        { return "deviceauth: not linked" }
func (notLinked) RelinkRequired() bool { return true }

// ErrIncompatible: the manager is in StatusIncompatible (compat says update_app or update_server). The signal client
// never reaches hello while Token fails, so it never sees client_outdated itself; this method says it instead.
var ErrIncompatible error = incompatible{}

type incompatible struct{}

func (incompatible) Error() string        { return "deviceauth: incompatible server" }
func (incompatible) UpdateRequired() bool { return true }

// ErrOffline: transient (has neither method). AccessToken wraps the network error:
// fmt.Errorf("%w: %w", ErrOffline, cause).
var ErrOffline = errors.New("deviceauth: server unreachable")

var (
	ErrRevokeNotConfirmed = errors.New("deviceauth: revoke not confirmed")
	ErrTooManyServers     = errors.New("deviceauth: too many servers")
)
```

**State machine.**

```
           Start: no credential            Link()                    200 tokens, credential saved
   ┌───────────────────────────► unlinked ────────► linking ───────────────────────────────► linked
   │                               ▲  ▲               │ access_denied (denied) / expired_token,   │  ▲
   │                               │  │               │ invalid_grant (code_expired) / CancelLink  │  │ refresh ok
   │                               │  └───────────────┘                                            ▼  │
   │  invalid_grant at a refresh   │ Unlink()                                       network error  offline
   │  (signed_out, refresh_reused, │                                                (keeps the credential,
   │   account_disabled)           │                                                 retries in background)
   └───────────────────────────────┴──── linked / offline
 LinkWithPassword(): unlinked → linked on its 200 (no linking state); an error leaves unlinked.
 SessionEnded(): a refresh attempt from linked/offline: invalid_grant → unlinked (notice), ok → linked.
 incompatible: entered from Link(), LinkWithPassword(), Start() or Recheck() (compat.FromInfo), from HelloRefused()
 (compat.FromError) or SetIncompatible(); left by Recheck (03-desktop-app's "Try again", and every 10 min) when the
 verdict is ok again.
```

**Linking** (`Link`):
1. `Info` → `compat.FromInfo`; not `ok` → `StatusIncompatible` and return `ErrIncompatible`.
2. `StartLink` (which checks both verification URIs, §11.2; `ErrBadResponse` otherwise, state unchanged) →
   `StatusLinking` with the code and Go's canonical URIs, and `Link` returns nil. 03-desktop-app opens
   `VerificationURIComplete` in the system browser right away (the click on "Link this app" is the user gesture)
   through `Shell.OpenURL`, which applies `App.OpenExternal`'s rule itself (a second, independent check), and shows
   the code, the URL, a QR code and "Copy link".
3. On the manager's own context, poll every `interval` (at least 5 s): `authorization_pending` → go on; `slow_down`
   → `interval = params.interval` (or + 5 s); `access_denied` → unlinked, `denied`; `expired_token` or
   `invalid_grant` → unlinked, `code_expired` ("Get a new code"; no automatic restart, so an abandoned app doesn't
   loop); a network error, 5xx or 429 → keep polling with the delay of §11.4 until `ExpiresAt`. `CancelLink` ends
   the loop (unlinked, no notice); `Close` ends it with the manager.
4. 200: write the credential (§11.5) **first**, then `servers.json`, then `StatusLinked`. A failed credential write is
   retried once; if it fails again the app stays linked for this run with `save_failed` ("Couldn't save the sign-in to
   Windows Credential Manager. You'll have to link again after restarting the app.").

### 11.4 Refresh and rotation

- **When**: in `AccessToken` when the cached token has less than 120 s left or none is cached (after `Start`), or
  when `forceRefresh` is set (at most once per 10 s, §11.3); in `SessionEnded` and `Recheck`; and at least once every
  24 h while the manager runs, so the 90-day window keeps sliding for an app that sits in the tray with a live
  WebSocket and makes no REST calls.
- **Single flight**: one refresh at a time per server; concurrent callers wait for it (bounded by their `ctx`). The Go
  publisher connection and the SPA share the manager, so two refreshes of one token never race from one app.
- **Exchange id**: drawn for each logical refresh (16 bytes) and kept in memory until that refresh succeeds or ends in
  `invalid_grant`; every retry of it sends the same id (§6.2).
- **Order on success**: write the new refresh token to the credential store, then update `servers.json` (username,
  role and device name from the response, when they changed), then replace the cached access token, then return it.
  The window in which an honest app can lose a committed rotation is the time between the server's commit and the
  credential write: one round trip plus a few milliseconds.
- **Retries**: on `ErrNetwork`, 5xx or `server_busy`: 1, 2, 4, 8, 16, 30 s, then every 30 s, with ±20 % jitter, same
  exchange id; on 429: `retryAfter`. While the cached access token still has more than 10 s left, `AccessToken`
  returns it during the retries; after that it returns `ErrOffline` and the state is `offline`.
- **`invalid_grant`**: delete the credential, clear the entry's user and device in `servers.json` (the server stays in
  the list), drop tokens, `StatusUnlinked` with `refresh_reused` when `params.reason` says so, else `signed_out` (or
  `account_disabled` when the `SessionEnded` code said so). The call whose refresh got it (and every caller waiting on
  that single flight) returns an error wrapping `ErrNotLinked`, after the credential is deleted.

### 11.5 `credstore`: Windows Credential Manager

```go
package credstore

type Credential struct {
	RefreshToken string    // redacted in fmt/slog
	DeviceID     string
	UserName     string    // display only (Credential Manager's "User name" column)
	SavedAt      time.Time
}

type Store interface {
	Get(origin string) (Credential, error) // ErrNotFound
	Put(origin string, c Credential) error
	Delete(origin string) error            // nil when it doesn't exist
	List() ([]string, error)               // origins with a credential
}

var (
	ErrNotFound    = errors.New("credstore: not found")
	ErrUnavailable = errors.New("credstore: unavailable") // no user profile (ERROR_NO_SUCH_LOGON_SESSION)
)

type Options struct {
	Prefix string // target prefix; default "isshoni:v1:"; tests use "isshoni-test:<random>:"
	Dir    string // file store only (non-Windows, M2): os.UserConfigDir()/isshoni-dev
}

func Open(o Options) (Store, error) // Windows: Credential Manager; elsewhere in M2: the file store
func NewMemory() Store              // tests
```

**Windows** (`wincred_windows.go`, ~150 lines over `windows.NewLazySystemDLL("advapi32.dll")`):
- `CredWriteW` with a `CREDENTIALW` of `Type = CRED_TYPE_GENERIC` (1), `Persist = CRED_PERSIST_LOCAL_MACHINE` (2),
  `TargetName = Prefix + origin` (e.g. `isshoni:v1:https://watch.example.com`), `UserName` = the username,
  `Comment = "isshoni desktop app sign-in"`, `CredentialBlob` = UTF-8 JSON
  `{"v":1,"refreshToken":"isr_…","deviceId":"…","savedAt":"2026-11-02T19:05:01.004Z"}` (about 130 bytes; the API
  allows 2,560, checked before the call).
- `CredReadW(target, CRED_TYPE_GENERIC, 0, &p)`: the blob is copied out, then `CredFree(p)`; `ERROR_NOT_FOUND` (1168)
  → `ErrNotFound`; a blob that doesn't parse or has another `v` → `ErrNotFound` plus a WARN (the app relinks).
- `CredDeleteW`; `ERROR_NOT_FOUND` → nil.
- `CredEnumerateW(Prefix + "*", 0, …)` for `List`; `ERROR_NOT_FOUND` → empty.
- `ERROR_NO_SUCH_LOGON_SESSION` (1312), which a process without a loaded profile gets → `ErrUnavailable`.
- Every other Win32 error is wrapped with its number (`credstore: CredWriteW: 0x…`), never with the blob.
- Threat model, stated in `docs/security.md`: any process running as the same Windows user can read the credential
  (DPAPI binds it to the user's logon), as it can read browser cookies; `LOCAL_MACHINE` keeps it off other PCs a
  roaming profile reaches.

**Other OSes in M2** (`file_other.go`): `credentials.json` in `Options.Dir`, mode 0600, the same JSON per origin,
written atomically (temp file and rename). It exists for development and CI builds of the desktop app on macOS and
Linux; the app logs one WARN at start ("plaintext credential store, development only"). *Later (M3)*: macOS Keychain
(cgo, `SecItemAdd`/`SecItemCopyMatching`, `kSecClassGenericPassword`, service `io.isshoni.desktop`, account = origin,
`kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`), replacing the file store on darwin. *Later (M4)*: the Linux agent
uses the Secret Service over D-Bus (godbus, already planned for the portal) with this file store as the headless
fallback. *Pending*: mobile apps bring their own `Store` (Keychain, Keystore).

### 11.6 `deviceauth.Servers`: several servers and `servers.json`

```go
type ServersOptions struct {
	Dir                   string // the data root, %LOCALAPPDATA%\isshoni on Windows (03-desktop-app §14.1 passes it)
	Store                 credstore.Store
	App                   AppInfo
	HTTP                  *http.Client
	AllowInsecureLoopback bool
	OnState               func(State) // per server; State.Origin says which
	Clock                 func() time.Time
	Logger                *slog.Logger
}

type ServerEntry struct {
	Origin  string         `json:"origin"` // the server's own public origin (Add), also the credential key
	Name    string         `json:"name"`   // server name from /info
	AddedAt time.Time      `json:"addedAt"`
	User    *api.User      `json:"user,omitempty"`
	Device  *api.DeviceRef `json:"device,omitempty"`
}

// AddResult is what Add did. Origin is the entry's origin; RequestedOrigin is what the user typed or the link
// named, normalized. When their hosts differ, 03-desktop-app shows "This server calls itself {host}".
type AddResult struct {
	Manager         *Manager
	Origin          string
	RequestedOrigin string
	Existing        bool // Origin was already in the list: that entry became active, nothing was added
}

func OpenServers(o ServersOptions) (*Servers, error)
func (s *Servers) List() []ServerEntry
func (s *Servers) Has(origin string) bool
func (s *Servers) Active() *Manager // nil when no server was added
// Add runs after the user confirmed (§12.3); the entry becomes active.
func (s *Servers) Add(ctx context.Context, origin string) (AddResult, error)
func (s *Servers) SetActive(origin string) error
func (s *Servers) Remove(ctx context.Context, origin string) error // Unlink, then drop the entry
func (s *Servers) ForgetAll(ctx context.Context) error             // --forget-credentials, --purge-data (§11.7)
func (s *Servers) Close() error
```

**`Add`**: normalizes `origin` (`deeplink.NormalizeOrigin`), calls `Info`, then stores
`deeplink.NormalizeOrigin(info.Server.PublicURL, AllowInsecureLoopback)` as the entry's origin and credential key,
not the origin as typed. The server's Host, Origin and CSRF checks use its public origin (M1 04 §4.4), so the SPA's
REST and WebSocket calls to an alternate name would be refused or answered 421. A `publicUrl` that doesn't normalize
is `ErrBadResponse`. When the adopted origin is already in the list, that entry becomes active and `Existing` is true;
no second entry or credential is made.

`servers.json` (mode 0600 on Unix; on Windows it inherits the user profile's ACL; written atomically; at most 20
servers, `Add` beyond that answers `ErrTooManyServers`):

```json
{"version": 1, "active": "https://watch.example.com",
 "servers": [{"origin": "https://watch.example.com", "name": "Alex's server", "addedAt": "2026-11-02T19:03:58.211Z",
              "user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin"},
              "device": {"id": "m4q8x2c6v0bn", "name": "Gaming PC"}}]}
```

- One server is active at a time in M2: the window, the Go publisher connection and the SPA's viewer connection use
  it. Switching servers while sharing asks first (03-desktop-app); each server has its own device row and its own
  credential. A server entry without a credential is listed and shows "Link this app".
- An unknown `version` or a file that doesn't parse is renamed to `servers.json.bad` and the app starts with an empty
  list (WARN); credentials are not touched, so re-adding a server reuses its credential.
- The last room is not kept here: the SPA keeps it in its prefixed Web Storage (03-desktop-app §14.3).

### 11.7 What the desktop app exposes (contract with 03-desktop-app)

03-desktop-app wraps `Servers` and the active `Manager` in a Wails service, `AuthService`, with these methods and JSON
shapes (field names as below; 03-desktop-app owns the binding code and the screens):

| Method | Result | Notes |
|---|---|---|
| `State()` | `AuthState` | the active server's state |
| `Link()`, `CancelLink()` | — | §11.3; `Link` returns once the code is shown, polling goes on in Go |
| `LinkWithPassword(username, password)` | `AuthState` | `Manager.LinkWithPassword` on the active server with `AppInfo`'s device name; 03-desktop-app binds it as `Auth.LinkWithPassword`; rejects with the `*api.Error` code (`invalid_credentials`, `rate_limited` with `retryAfter`, …) |
| `AccessToken(origin, force)` | `{token, expiresAt}` or `null` (not linked, or `origin` isn't the active server's) | `Manager.AccessTokenWithExpiry(ctx, force)` of the active `Manager`, **only when `origin` equals that `Manager`'s origin** (read once, so the token is always `origin`'s own even if the active server changes during the call); any other origin gets `null` without touching a `Manager`, plus one debug line with no origin in it (03 §5.8). `force: true` after the server refused the token the SPA had (there is no separate "token rejected" call, and no `TokenRejected`); `expiresAt` in ms since the epoch; the token goes to the SPA's memory only |
| `SessionEnded(origin, code)` | `AuthState` | `Manager.SessionEnded` (single flight with C2's call) when `origin` is the active server's, else rejects `not_found` with a debug line: the SPA redials C1 when the result is `linked` and shows relink otherwise |
| `HelloRefused(origin, {code, params})` | `AuthState` | `Manager.HelloRefused` for C1's `client_outdated` and `protocol_unsupported`, with the same origin rule as `SessionEnded` |
| `Unlink()` | `{confirmed: bool}` | `false`: the server wasn't reached; the UI says "Remove {device} on your Devices page to finish" |
| `Servers()` | `ServerEntry[]` | |
| `AddServer(origin)` | `{origin, requestedOrigin, existing}` | `Servers.Add`, only after the confirmation of §12.3; the server becomes active and 03 reloads the window at `/desktop/setup?link=1`, so the page, its `bridge.js` and every token call name the new server before its link step (03 §8.2) |
| `SwitchServer(origin)`, `RemoveServer(origin)` | — | every change of the active server reloads the window once (also a switch to a known server that isn't linked), so no loaded page outlives its server (03 §5.8) |
| `Recheck()` | `AuthState` | `Manager.Recheck`: re-runs `compat.FromInfo` |

`AuthState` (the Wails event `isshoni:auth.state`, following 03-desktop-app §6.3's prefix rule, carries the same object
on every change):

```json
{"origin": "https://watch.example.com", "serverName": "Alex's server", "status": "linking",
 "link": {"userCode": "WDJB-MJHT", "verificationUri": "https://watch.example.com/link",
          "verificationUriComplete": "https://watch.example.com/link?code=WDJB-MJHT", "expiresAt": 1762110845123},
 "user": null, "device": null, "notice": "",
 "compat": {"verdict": "ok", "reason": "", "serverVersion": "0.2.0", "minClientVersion": ""}}
```

- **Uninstall**: the uninstaller (06-windows-release §6.8, which owns the command line) runs
  `isshoni.exe --forget-credentials` on every uninstall and `isshoni.exe --purge-data` instead on a `/PURGE` one; both
  call `Servers.ForgetAll` (06's "DeleteAll") with one 5 s budget:
  1. list every credential under the prefix (`credstore` `List`), whether or not `servers.json` names it;
  2. for all of them **concurrently, under one shared 5 s deadline**: refresh, then `POST /device/revoke` with the new
     access token; a failure of either is ignored;
  3. delete every credential under the prefix whatever the revoke results, clear `user` and `device` of every entry in
     `servers.json`, and exit 0.
  The whole call takes about 6 s whatever the number of servers, inside the 30 s the uninstaller gives it.
  `--purge-data` then deletes the data root (`%LOCALAPPDATA%\isshoni`), `servers.json` included (06's rule);
  `--forget-credentials` leaves it. 03-desktop-app parses both flags.
- **Logs and diagnostics** (03-desktop-app's log file, M5's "Copy diagnostics") contain no token, code or exchange id;
  `State` has no secret, so it may be logged.

### 11.8 Rules for `DesktopPlatform` (03-desktop-app implements them)

- **The page's own origin in every call**: `DesktopPlatform` passes its `serverOrigin` (the server the page was built
  for, from `bridge.js`) to `AccessToken`, `SessionEnded` and `HelloRefused`, and ignores an `isshoni:auth.state` whose
  `origin` isn't its `serverOrigin`. A page that outlives a switch therefore never receives another server's token.
- `apiFetch(path, init)`: get `AccessToken(serverOrigin, false)`. With a token: send `Authorization: Bearer <token>`
  with `mode: 'cors'`, `credentials: 'omit'`; on 401 `invalid_token` or `unauthenticated` call
  `AccessToken(serverOrigin, true)` and retry **once**; a second 401 goes to the SPA (05 §6.2 clears `['me']`). When
  `AccessToken` is `null` (not linked): send the request **without** `Authorization` instead of rejecting locally.
  `Public` routes answer normally (so `GET /api/v1/info` at boot works on an active but unlinked server, 03-desktop-app
  §7.2 step 4, and the app shows relink rather than the Offline screen); `User` routes get the server's own 401
  `unauthenticated`, with no retry.
- `signaling()`: `{url: new URL('/ws', serverOrigin) with wss:, auth: HelloAuthProvider}` (03-desktop-app §7.1's
  `({forceRefresh}) => Promise<HelloAuth | undefined>`). The provider runs before every `hello` (01 §3.2; a resume
  needs one too): `forceRefresh: false` → `AccessToken(serverOrigin, false)`, `forceRefresh: true` (the SPA's signal
  client after an `unauthenticated`) → `AccessToken(serverOrigin, true)`; the result is `{scheme: 'bearer', token}`,
  or `undefined` when the token is `null` (so a hello to a server that isn't active carries no token). A second
  `unauthenticated` stops the connection and leaves the state to `isshoni:auth.state`: the SPA calls
  `SessionEnded(serverOrigin, 'unauthenticated')`. On `session_revoked` or `account_disabled` on a live socket the SPA
  stops (01 §12.1) and calls `SessionEnded(serverOrigin, code)`; it redials C1 when the result's status is `linked`
  and shows relink otherwise. On `client_outdated` or `protocol_unsupported` at `hello` it calls
  `HelloRefused(serverOrigin, error)` and shows §13.3's screen from the returned state.
- `client`: `{kind: 'desktop', version: <app version>, os: 'windows'}`; the SPA's stale-build reload (05 §16.4) is off
  under `DesktopPlatform` (the bundled SPA is the app's version, not the server's); `versionActions()` returns
  `updateApp` and `openInBrowser` (§13.3; `openInBrowser` opens the SPA's current room).
- Logging out from the SPA's account menu calls `Unlink()`, never `POST /auth/logout`.

---

## 12. `isshoni://` deep links

### 12.1 Format

```
isshoni://<host>[:<port>]/                  → server https://<host>[:<port>], its default room
isshoni://<host>[:<port>]/r/<roomId>        → that server, room <roomId>
```

- `<host>` is a DNS name (IDN allowed, converted with `idna.Lookup.ToASCII`) or an IP literal (IPv6 in brackets);
  lower-cased; no userinfo. `<port>` 1–65535; `:443` is dropped. The server is always `https://` (the plan: every TLS
  mode ends with a publicly trusted certificate). Dev builds add servers on `http://localhost` through the app's
  manual field, never through a link.
- `<roomId>`: 1–64 of `[A-Za-z0-9_-]` (01 §4.1); a trailing `/` is accepted.
- **Nothing else is read.** A query and a fragment are dropped unread (one debug line says so, without their content),
  so no version of a link can carry a credential, a code or a "start sharing" instruction into the app. Any other path
  is `ErrUnsupported` ("This link isn't supported by this version of the app").
- At most 2048 bytes, valid UTF-8, no spaces or control characters.
- The web page builds it with `appLinkFor(serverOrigin, roomId)` (§10.2); Go with `deeplink.Build(origin, roomID)`.

```go
package deeplink

type Link struct {
	Origin      string // "https://watch.example.com"
	Host        string // ASCII host[:port], as in Origin
	HostUnicode string // the Unicode form when Host has xn-- labels; else equal to Host
	RoomID      string // "" = the server's default room
}

var ErrUnsupported = errors.New("deeplink: unsupported link")

func Parse(raw string) (Link, error)
func Build(origin, roomID string) string
// NormalizeOrigin turns what a user typed or a link named into "https://host[:port]": the host as for links, no path
// other than "/", no query, fragment or userinfo; http only for a loopback host when allowInsecureLoopback.
func NormalizeOrigin(raw string, allowInsecureLoopback bool) (string, error)
```

`testdata/links.json` holds the cases both Go and TypeScript tests use (`appLinkFor` builds the valid ones), among
them: `isshoni://watch.example.com/r/lounge` (ok), `ISSHONI://Watch.Example.com:443/r/lounge/` (ok, normalized),
`isshoni://[2001:db8::1]:8443/` (ok), `isshoni://xn--bcher-kva.example/` (ok; `HostUnicode` `bücher.example`),
`isshoni://watch.example.com/r/lounge?token=x#t` (ok, query dropped), `isshoni://user@watch.example.com/` (refused),
`isshoni:watch.example.com` (refused, no `//`), `isshoni://watch.example.com/admin` (unsupported),
`isshoni://watch.example.com/r/` (unsupported), `isshoni://watch.example.com:0/` (refused).

### 12.2 Launch and forwarding

- 06-windows-release registers the scheme under `HKCU\Software\Classes\isshoni` (`URL Protocol` = `""`) with the
  command `"<install dir>\isshoni.exe" --open-url "%1"` (06-windows-release §2.3, which writes the registry).
- With `--open-url`, the app takes exactly one value after the flag as the link, which must be an `isshoni:` URL, and
  honors no other flag in that launch, so a link that smuggles a quote and flags (`isshoni://x" --flag`) can only ever
  be a malformed link.
- A second instance forwards the link to the running one and exits (Wails single-instance, 03-desktop-app).

### 12.3 Behavior

| Situation | What the app does |
|---|---|
| The server is in the list and linked | Make it active and open the room (for `/`, the SPA's own last room, 03-desktop-app §14.3) as a viewer. **Never starts a share.** If the app is sharing on another server or room, it asks "You're sharing in {room}. Open {target}? Your share will stop." first; a link never stops a share silently either |
| The server is in the list, not linked | Make it active, show "Link this app"; open the room after linking |
| The server is unknown | A modal, before any network request: **"Add server {host}?"** with the host large (and `HostUnicode (Host)` when they differ, against look-alike names), "Only add servers you trust. The server's admin can see what you share." [Add] [Cancel]. On Add: `GET /info` (10 s); not an isshoni server or incompatible → say so (§13) and add nothing; else show the server name and the trust model in one sentence ("Media is encrypted in transit but the server can see your streams; isshoni has no telemetry"), then `Servers.Add` (which adopts the server's own `publicUrl`, §11.6; "This server calls itself {host}" when it differs) and "Link this app" |
| A link arrives while a confirmation is open | Ignored (a debug line). A newer link must not swap the host under the friend's cursor |
| The room doesn't exist | The join gets `room_not_found` and the app goes to the server's default room (01 §12.1) |
| More than 20 servers | "You've added 20 servers. Remove one first." |

Contacting the server only after the friend said yes keeps a link from working as a tracking pixel.

---

## 13. Version compatibility for native clients

### 13.1 Where the app checks

1. **Before linking and at every start** (`Manager.Link`, `Manager.LinkWithPassword`, `Manager.Start`):
   `GET /api/v1/info` → `compat.FromInfo(info, compat.Default())`.
2. **At every `hello`** (both connections): `error{protocol_unsupported, params {serverMin, serverMax,
   serverVersion}}` or `error{client_outdated}` (01 §6.1, §12.1; close 4426). 03-desktop-app passes the core's
   `StopError.Err` (01-client-core §11.2: the server's `*protocol.Error` with its params) to
   `Manager.HelloRefused`, which runs `compat.FromError(err, compat.Default())` and calls `Manager.SetIncompatible`
   (or `Recheck` when `FromError` can't decide, §13.2); the SPA's hello error goes the same way through the
   `HelloRefused` binding (§11.7), and the SPA shows the screen through `platform.versionActions()`.
   `AccessToken` then answers `ErrIncompatible` (`UpdateRequired`), so the core doesn't redial into the same refusal.
3. **Every 10 min while incompatible, and on "Try again"**: `Recheck`.

### 13.2 The decision

```go
package compat

type Verdict string

const (
	OK           Verdict = "ok"
	UpdateApp    Verdict = "update_app"
	UpdateServer Verdict = "update_server"
)

type Reason string // "" (ok), "protocol_too_new", "protocol_too_old", "no_device_flow", "min_client_version"

type Check struct {
	Verdict          Verdict
	Reason           Reason
	ServerVersion    string
	MinClientVersion string
}

type App struct {
	Version               string
	Protocol, MinProtocol int
}

func Default() App // version.Version(), protocol.Version, protocol.MinVersion
func FromInfo(info api.Info, app App) Check
// FromError: false (undecided) for any other code, a nil error, and protocol_unsupported without params.serverMax.
func FromError(e *protocol.Error, app App) (Check, bool)
```

`FromInfo`, first match wins:

| # | Condition | Verdict, reason |
|---|---|---|
| 1 | `info.Protocol.Current < app.MinProtocol` | `update_server`, `protocol_too_old` |
| 2 | `info.Protocol.Min > app.Protocol` | `update_app`, `protocol_too_new` |
| 3 | `deviceFlow` not in `info.Features` (a 0.1.x server) | `update_server`, `no_device_flow` |
| 4 | `info.MinClientVersion != ""` and `version.Below(app.Version, info.MinClientVersion)` | `update_app`, `min_client_version` |
| 5 | otherwise | `ok` |

`FromError`: `client_outdated` → `update_app`, `min_client_version`; `protocol_unsupported` with `params.serverMax`
present and above 0 → `update_server`, `protocol_too_old` when `params.serverMax < app.MinProtocol`, else
`update_app`, `protocol_too_new`; **`protocol_unsupported` with `params.serverMax` absent or 0 → `(Check{}, false)`**;
any other code or a nil error → `(Check{}, false)`.

Why the undecided case: when the server closes with 4426 without sending an error message first, M1's
`errorForClose` (`internal/client/signal/errors.go`, as built) maps the close to `protocol_unsupported` with empty
params, even when the real cause was `client_outdated`. Reading `serverMax` as 0 would answer `update_server`
wrongly. The caller then runs `Manager.Recheck` (`/info` → `FromInfo`), which also catches a `minClientVersion` floor
correctly.

`version.Below(v, floor)` is the hub's rule (moved from `internal/server/signal/semver.go`, so client and server agree):
SemVer 2.0 precedence; a version that isn't SemVer counts as below; a floor that isn't SemVer can't be enforced
(`ok = false`, treated as no floor). A dev build (`0.0.0-dev+<commit>`) is below every floor, which only matters on a
server whose admin set one.

```go
package version // internal/version/semver.go (moved from internal/server/signal/semver.go, behavior unchanged)

type SemVer struct {
	Major, Minor, Patch uint64
	Pre                 []string // build metadata is dropped: it has no precedence
}

func Parse(s string) (SemVer, error)          // SemVer 2.0; a leading "v" is accepted
func Compare(a, b string) (int, error)         // -1, 0, +1 by precedence; an error when either isn't SemVer
func Below(v, floor string) (below, ok bool)   // the hub's versionBelow, exported
```

### 13.3 The screens (03-desktop-app draws them)

- **`update_app`**: "This server needs a newer isshoni app. You have {app version}; the server runs {server
  version}." **Update now** and **Open in browser**. Update now calls 06-windows-release's `Updater.CheckNow` (its
  §8.9) and acts on `Status.State`:

  | `State` | Shown |
  |---|---|
  | `ready` | **Restart and update** (`Updater.Apply`, relaunch) |
  | `downloading` | progress (`Status.Progress`) |
  | `up_to_date` | "No newer version is available yet" |
  | `off` | "Update checks are turned off", or "Update checks are turned off by your administrator" when `PolicyOff` |
  | `disabled` | "This build doesn't update itself" |
  | `error` | the `update.error.<code>` text (06-windows-release §8.10) |

  Every state except `ready` and `downloading` also shows **Get it from your server's Download page**, which opens
  `origin + "/download"` in the system browser (`platform.openExternal`), never in the webview: the bundled SPA's own
  `/download` shows no download buttons under `DesktopPlatform` (06-windows-release §7.5).
- **`update_server`**: "This server runs isshoni {server version}, which is too old for this app. Ask your admin to
  update it." **Open in browser**.
- **Open in browser** opens `origin + "/r/" + <the SPA's current room>`, or `origin + "/"` when there is none, with the
  system browser; the web app there always matches the server (plan). 03-desktop-app's
  `versionActions().openInBrowser` implements it (and includes the room).

### 13.4 N and N−1

- The server and all apps speak protocol N and N−1 (01 §6.1, §14); M2 keeps protocol 1 and adds features
  (01-client-core). REST changes within `/api/v1` are additive; an app uses an endpoint newer than the previous release
  only when `/info.features` (or `welcome.features`) lists the feature that brought it. `deviceFlow` brings every
  route of §7.1.
- Tokens, the JSON of §7.3 and the credential blob (`"v": 1`) are stable across releases; a change adds fields.
- CI: the test is 02's, 06's nightly `compat-servers` job runs it. `TestCompatServerDeviceFlow`
  (`internal/client/deviceauth/compat_server_test.go`) skips unless `ISSHONI_COMPAT_SERVER_URL` is set, and reads 06's
  `ISSHONI_COMPAT_ADMIN_FILE` (JSON `{origin, username, password}`). If `/info` lists `deviceFlow`, it logs in with
  `POST /api/v1/auth/login` (cookie), runs `Manager.Link` with a memory store, approves through `/device/lookup` and
  `/device/decide` with the cookie, then calls `AccessToken(ctx, true)` once (one refresh) and `Unlink` (revoke),
  asserting `linked`, a new access token after the refresh, and `Unlink` returning nil (the revoke was confirmed).
  Otherwise (a 0.1.x server) it asserts `compat.FromInfo` = `update_server`/`no_device_flow`. Its name matches 06's
  `-run TestCompatServer`.

---

## 14. The in-app password fallback: `POST /api/v1/device/password` (M2)

The plan names "an in-app username/password form" as the device flow's fallback ("Privacy and trust model"), and
M1 03 §12.3 marks route #48 M2. The device flow stays the default path (journey 5: the friend links with a browser
approval); 03-desktop-app offers the form on the link screen as "Sign in with username and password instead", with
WebView2's autofill off for it (03-desktop-app's note).

**Request** (`api.DevicePasswordRequest`, M1 03 §12.4.7's sketch; Public; body ≤ 16 KiB like every `/device/` route):

```json
{"username": "Alex", "password": "…", "clientKind": "desktop", "deviceName": "DESKTOP-7F3K2LQ", "os": "windows",
 "appVersion": "0.2.0"}
```

**Order** (`auth.PasswordDeviceLogin`, `devicepassword.go`): exactly `auth.Login`'s order and buckets as built in
`login.go` (03 §7.2–§7.3), every step before the next:
1. `auth-ip` → 429 `rate_limited`.
2. Validation, every field before answering → 422 `validation_failed`: `username` and `password` as login
   (`required`, `too_long`, `invalid`); `clientKind`, `os`, `deviceName`, `appVersion` as §5.2 step 3.
3. The per-username buckets (`auth-user-ip`, `auth-user`, with login's known-IP rule), reserved before the hash as
   login reserves them → 429 `rate_limited`.
4. `auth-hash` → 503 `server_busy` with `retryAfter`; then the hash under the semaphore (a full queue → 503
   `server_busy`). An unknown username, or an account without a password, hashes the dummy value.
5. Wrong password → 401 `invalid_credentials` and M1's `auth.login_failed` row; a correct password for a pending or
   disabled account → 403 `account_pending` / `account_disabled` (with the same row). Login's rehash rule applies.
6. One `Write` that re-reads the user (password hash unchanged and status `active`, else as step 5, like login's
   `Write`) and then runs the device_code grant's success steps (§5.4): `TrimDevices(user, 19)` with its `cap` rows,
   `CreateDevice` with `linked_via = 'password'` and name, kind, OS and version from the request, `last_seen_at = now`,
   `last_ip` = this request's IP, a token pair with `issued_by = NULL`, audit `device.linked {clientKind, os, via:
   "password"}`. No web session is created. After the commit: `{UserID}: devices` and the evicted devices'
   connections closed (`device_revoked`).

**Response**: 200 `DeviceTokenResponse` (§5.4's shape). Errors are #2's: 401 `invalid_credentials`, 403
`account_pending`/`account_disabled`, 422, 429, 503 `server_busy`.

**Client side**: `deviceauth.Client.PasswordLink(ctx, username, password)` (§11.2), `Manager.LinkWithPassword(ctx,
username, password)` (§11.3, which also checks compat first), and the `AuthService` row `LinkWithPassword(username,
password)` (§11.7), which 03-desktop-app binds as `Auth.LinkWithPassword` (on the active server, with `AppInfo`'s
device name). The password crosses the binding once as a `protocol.Secret`, is sent once over TLS, and is never stored,
retried or logged.

Why it is safe enough: a guess here costs exactly what one at `/auth/login` costs (same buckets, same hash), so it
opens no cheaper path to an account, and the new device shows on the Devices page and as a security event like any
other linked device.

---

## 15. Limits and timeouts

| Item | Value |
|---|---|
| Device code | 600 s; row pruned 1 h after expiry |
| Poll interval | 5 s; `slow_down` + 5 s, at most 60 s; 1 s tolerance |
| User code | 8 letters of 20 (34.6 bits); 3 tries on a collision |
| Access token | 900 s; at most 5 live per device; bearer cache 30 s |
| Refresh token | 90 d from issue; rotated on every use; retry grace: same `exchangeId` (any time before expiry) or 120 s once; a grace re-issue keeps the replaced child as a spent row |
| Devices | 20 per user (LRU eviction); expire after 90 d without a refresh (+ ≤ 1 h) |
| Device name | 1–64 code points, PRECIS Nickname |
| App version | 1–32 bytes of `[0-9A-Za-z.+-]` |
| Throttles | `device-code-ip` 10 + 1/6 min; `device-code-ip48` 30 + 1/2 min per IPv6 /48; `device-token-ip` 60 + 1/s; `device-lookup` 10 + 1/min per user; `auth-ip` as M1; 5,000 pending codes server-wide; `/device/password`: login's buckets and hash semaphore (§14) |
| Device last-seen write | at most once per 5 min per device |
| CORS preflight cache | 600 s |
| Client HTTP | 15 s per request; no redirects; bodies ≤ 64 KiB |
| Client refresh | when < 120 s left; at least daily; retries 1, 2, 4, 8, 16, 30 s then every 30 s (±20 %) |
| Client forced refresh | at most once per 10 s (`AccessToken(ctx, true)`); a forced call within 10 s of the last forced refresh gets that refresh's token |
| `SessionEnded` | one refresh attempt, bounded by the caller's ctx; concurrent calls share it |
| Unlink | 10 s for the revoke call; uninstall (`--forget-credentials` on every uninstall, `--purge-data` on `/PURGE`): one 5 s budget for all servers, concurrent; credentials deleted regardless |
| Servers | 20 per app |
| Deep link | ≤ 2048 bytes; room id 1–64 `[A-Za-z0-9_-]` |
| Compat recheck | every 10 min while incompatible |

---

## 16. Test plan

Everything automated runs on the Mac (`task test`, `task e2e`) and in GitHub Actions; the column says where else.
Time-dependent code uses `testing/synctest` or the injected clocks (README §4 Testing). Fuzz targets run 30–60 s
before their slice's PR; their seeds live in `testdata/fuzz`.

### 16.1 Unit (Go)

| Package | Asserted | Where |
|---|---|---|
| `internal/protocol/api` | golden JSON of every §7.3 DTO; redaction of `deviceCode`, `accessToken`, `refreshToken`, `exchangeId` and `DevicePasswordRequest.password` (`TestSecretFieldsRedacted`); `NormalizeUserCode` and `FormatUserCode` with §3's table; `FuzzNormalizeUserCode` (never panics, output always 8 alphabet letters or invalid) | Mac, CI ubuntu |
| `internal/protocol` | `DesktopOrigins` is the §6.4 list, exact strings, no trailing slash; `DesktopOrigins()[0] == DesktopAssetOriginWindows`; two calls return distinct slices | Mac, CI ubuntu |
| `internal/version` | the hub's SemVer tests moved with the code (precedence, pre-releases, invalid input); `Below` table | Mac, CI ubuntu |
| `internal/server/store` | 0002 migrates a schema-1 file with device rows intact (`pre-1-*.db` written); the two columns and the index exist; `DecideDeviceCode` only from pending and unexpired; `SpendRefreshToken` twice → `ErrConflict`; `SupersedeRefreshToken` sets `spent_at`, `replaced_by` and a NULL `exchange_hash`, and on a spent row → `ErrConflict`; `DeleteAccessTokensIssuedBy` deletes only access rows (the issuer's refresh rows stay); `DeleteAccessTokensIssuedBy`, `TrimAccessTokens(5)`, `TrimDevices(19)` return what they removed; prune: a device with only spent or expired refresh tokens and `created_at` older than 1 h goes with a `device.expired` row, a younger one or one with a live token stays; boundaries exact (a row at its limit stays) | Mac, CI ubuntu |
| `internal/server/auth` (device flow) | table test of §5.4 under a fake clock: pending → `authorization_pending`; a poll at 3.9 s → `slow_down {interval: 10}`, at 4.0 s with interval 5 → pending; repeated early polls cap at 60; deny → `access_denied`; approve → 200 once, then `invalid_grant`; expiry → `expired_token`, after prune → `invalid_grant`; approver disabled between approve and poll → `access_denied`; decide idempotent for the same user and decision, 404 otherwise; parallel decides by two users: exactly one 204; the 21st device evicts the least recently seen with a `cap` row and a `ConnCloser` call; the pending cap at 5,000 → `server_busy`; `device-code-ip48`: requests from 31 different /64s of one /48 → the 31st gets 429 while one from another /48 gets 200, an IPv4 client never takes its token, `IPKey48` table (IPv4 → false, IPv4-mapped → false, a zone dropped); `LookupUserCode`'s `network`: same IPv4 → `same`, two IPv4 → `different`, two addresses of one IPv6 /64 → `same`, a code started over IPv6 looked up over IPv4 (and the reverse) → `unknown`, IPv4-mapped IPv6 vs IPv4 of one address → `same`; every bucket: order, refusal without hashing or reads, refund of `device-lookup` on a found code, `auth.throttled` once per episode; **password link** (§14): login's bucket order and refusals (a counting hasher: a throttled request hashes nothing; an unknown user hashes the dummy), 401 with `auth.login_failed`, 403 pending/disabled only after a correct password, 422 for each device field, success creates a device with `linked_via = 'password'`, `device.linked {via: "password"}`, no session row, and the 21st device evicts as in the device flow; a password changed between the hash and the `Write` → 401 | Mac, CI ubuntu |
| `internal/server/auth` (tokens) | refresh rotates (old spent, `replaced_by` set, new pair `issued_by` old); **reuse**: replay after the child was used → device and tokens gone, `device.refresh_reused`, alert raised, `ConnCloser{UserID, DeviceID}` called after the commit, cache forgotten; **grace**: replay with the same `exchangeId` at +10 min while the child is unused → new pair, the old child's access token gone, the old child kept as a spent row (`replaced_by` = the new refresh, `exchange_hash` NULL); replay without id at +100 s → new pair once, a second replay at +110 s → reuse; replay at +121 s without id → reuse; replay with another id at +10 min → reuse (inside 120 s another id takes the restart row, since a restarted client draws a new id); **grace, then the superseded child**: replay without id at +100 s, then present the old child C → 400 `invalid_grant {reason: reused}`, device and tokens gone, `device.refresh_reused`, alert raised (once with C presented at +101 s, inside the 120 s window, and once at +15 min); the same after a same-`exchangeId` retry; a request without `exchangeId` never matches a NULL `exchange_hash`; bearer: wrong shapes → `invalid_token` without a DB read (counting fake), expired → `invalid_token`, cache hit within 30 s, a revoke invalidates the cache before `ConnCloser` runs; `Touch` for a bearer: device gone → `unauthenticated`, use recorded at most once per 5 min, the returned principal carries the device's current `DeviceName` (also from `AuthenticateBearerToken`); a session principal comes back unchanged; `MaybeRotate` no-op; `LogoutEverywhere` and `RevokeDevice` for a bearer; `NormalizeDeviceName` table; `FuzzDeviceToken` (random bodies never panic, never return 5xx) | Mac, CI ubuntu |
| `internal/server/httpapi` | the §7.1 endpoint table (statuses and codes) through the real chain with a real `auth.Service`; the **credential matrix**: each of `Public`, `User`, `Admin`, `UserSession`, `UserDevice` × {none, cookie, bearer, bad bearer, cookie + bearer}, where `Admin` with an admin user's valid bearer → 403 `forbidden {credential: session}`, with a bad bearer → 401 `invalid_token`, with the admin's cookie → as M1; #29–#42 are registered `Admin`; #21–#23 accept an admin's bearer; a bearer request with a cross-site `Origin` and no CSRF headers passes, a cookie one doesn't; the **CORS matrix**: preflights from each desktop origin (allowed and refused methods and headers), from another origin (405 as M1), a desktop-origin request's `Cookie` header ignored, `Vary: Origin` everywhere, never `Allow-Credentials`, CORS headers on 401/413/429 answers; `/me` and `/me/devices` for a bearer (`device`, `current`); `/info` lists `deviceFlow`; `TestAuditCoverage` and `TestAuditActionsInSource` with §8's rows | Mac, CI ubuntu |
| `internal/server/signal` | a socket from `http://wails.localhost` with `hello.auth` is accepted; with a valid cookie it gets 403 (as built); a successful bearer `hello` refunds the per-IP pre-auth token (21 bearer sockets of two users from one IP, opened one after another within a minute, all succeed; 21 sockets whose `hello` fails get 429 on the 21st); `Identity.DeviceName` never appears in `LogValue` output; a `Revalidate` result whose `DeviceName` changed is applied like a changed `Name` (M1 01 §3.2); `wire.go`'s `AuthenticateBearer` and `Revalidate` copy `DeviceName` (the wiring test of `internal/server`) | Mac, CI ubuntu |
| `internal/client/deviceauth` | against an `httptest` server with scripted answers: the poll loop (pending, `slow_down` with and without `params.interval`, denied, expired, network errors until `ExpiresAt`); the credential is written before `linked` and before a refreshed token is returned (an ordering fake); `save_failed`; single flight (100 concurrent `AccessToken(ctx, false)` calls → one refresh); retries keep the `exchangeId` and stop on `invalid_grant`; `AccessToken(ctx, true)` refreshes even with a fresh cached token, a second forced call within 10 s returns that refresh's token without a request, one after 10 s refreshes again; **errors**: no credential → `errors.Is(err, ErrNotLinked)` and the chain has `RelinkRequired() == true`; the call whose refresh gets `invalid_grant` returns the same, after the store is empty; `ErrIncompatible` has `UpdateRequired() == true`; `ErrOffline` has neither method and unwraps to its network cause; `*deviceauth.Manager` satisfies `core.Auth` (a compile-time assertion in a test file); `Link` returns once `linking` is reached and polling continues after its ctx is cancelled, until `CancelLink` or `Close`; `LinkWithPassword`: 200 → credential then `linked`, 401 → stays `unlinked` with the `*api.Error`, compat refusal → `incompatible`; `SessionEnded` (each of its three codes) → refresh → `invalid_grant` → unlinked with `signed_out` or `account_disabled`, → success → stays `linked`, two concurrent calls → one refresh, another code → no request; `HelloRefused` with `client_outdated` → `incompatible`/`update_app`, with `protocol_unsupported` and no `serverMax` → a `/info` request (Recheck) decides; **verification URIs** (§11.2): `StartLink` → `ErrBadResponse`, the state unchanged and nothing to open, when either URI is `file:///C:/x`, `ms-settings:display`, `search-ms:query=x`, `javascript:alert(1)`, a UNC path (`\\host\share\x` and `file://host/share/x`), `https` on another host, `http` on a non-loopback host, carries userinfo or a fragment, has the path `/link/` or `/Link`, an extra query parameter (`code=…&next=…`), or a code that differs from `userCode`; accepted with an upper-case host or an explicit `:443`, and `LinkState` then holds Go's canonical strings, not the server's; `Recheck` back to `linked`; daily refresh under `synctest`; `Unlink` offline → `ErrRevokeNotConfirmed` and the credential deleted; redirects are not followed; the `User-Agent` is `isshoni-desktop/<version>`; a TLS failure is `ErrNetwork` with a reachable `*tls.CertificateVerificationError`; `Servers.Add` stores `info.server.publicUrl` normalized (a server reached as `alt.example` that calls itself `watch.example.com` → entry and credential key `https://watch.example.com`, `RequestedOrigin` kept), an adopted origin already listed → `Existing` and no second entry; `ForgetAll` with 20 servers, half of them hanging: returns within 6 s, every credential gone, users and devices cleared; `servers.json` round trip, the 20 cap, a corrupt file renamed `.bad` | Mac, CI ubuntu, CI windows (06 windows-go) |
| `internal/client/credstore` | file and memory stores: put/get/delete/list, 0600, atomic replace, a corrupt file; the Windows store with a random test prefix: put/get/overwrite/delete/list, the 2,560-byte check, a missing target → `ErrNotFound`; `ERROR_NO_SUCH_LOGON_SESSION` → `t.Skip` with a message (the manual check M-DA-1 covers it) | Mac, CI ubuntu (file); CI windows (06 windows-go; Credential Manager) |
| `internal/client/deeplink` | `testdata/links.json`; `NormalizeOrigin` table (paths, ports, userinfo, IDN, loopback http only when allowed); `FuzzParse` (never panics; an accepted link always rebuilds with `Build` to an equal `Link`) | Mac, CI ubuntu |
| `internal/client/compat` | §13.2's table, both functions, dev versions, a floor that isn't SemVer; `FromError` for `protocol_unsupported` with `serverMax` absent and with 0 → `(Check{}, false)` (M1's `errorForClose` case), with `serverMax` 1 and `MinProtocol` 1 → `update_app`/`protocol_too_new`; `client_outdated` → `update_app`/`min_client_version`; nil and other codes → `false` | Mac, CI ubuntu |

Windows: 06's Windows Go test job (`windows-go`, windows-2025) runs `go test -race ./internal/client/...` (MinGW) and
then `CGO_ENABLED=0 go test ./internal/client/...` (the shipped mode) on every PR that touches them; 06's `lint-go`
cross-lints with `GOOS=windows`. This document adds no CI step of its own.

### 16.2 Integration (Go, in-process server)

`internal/server/itest/device_test.go`, on `servertest.Start` with the real wiring.

**Helpers** (package `itest`, `internal/server/itest/device_helpers_test.go`; 01-client-core's desktop itests and its
slice C15 use them). They live in `itest`, not `servertest`, so that `servertest` never imports client packages:

```go
type linkedDevice struct {
	DeviceID        string
	Access, Refresh protocol.Secret
}

// linkDevice links a device for the user behind cookie with raw HTTP: POST /device/code, POST /device/decide with
// the cookie (approve, deviceName name), POST /device/token (device_code grant). Fails t on any other answer. (DA6)
func linkDevice(t *testing.T, srv *servertest.Server, cookie, name string) linkedDevice

// linkedManager returns a started deviceauth.Manager (memory credential store) that is already linked for the user
// behind cookie, through Manager.Link and the cookie's lookup and decide. It satisfies core.Auth. Closed by
// t.Cleanup. (DA11)
func linkedManager(t *testing.T, srv *servertest.Server, cookie string) *deviceauth.Manager
```

**Cases**:
1. A user logs in (cookie). A `deviceauth.Manager` with a memory store calls `Link`, which returns at `linking`; a
   goroutine plays the browser: `lookup` (sees `network: same`), `decide {approve, deviceName: "Test PC"}`. The
   manager reaches `linked` within one poll interval (shortened through the server's clock hook); the device is listed
   by `GET /me/devices`.
2. 01's Go signal client dials with `Options.Token = func(ctx context.Context) (protocol.Secret, error) { return
   mgr.AccessToken(ctx, false) }`, `role: publisher`, `kind: desktop`; a second client dials as the "SPA" (`viewer`,
   the same token, `Origin: http://wails.localhost`); both join the Lounge and `room.state` shows **one** participant.
   Once 01-client-core's C08 has merged, `user.connections` lists both with `deviceName` "Test PC" (01 §10.4); until
   then this assertion is skipped with a note.
3. `DELETE /me/devices/{id}` with the cookie: both sockets get `error{session_revoked}` and close 4401 within 100 ms;
   `SessionEnded(ctx, session_revoked)` → refresh → `invalid_grant` → returns `unlinked` with `signed_out`; the memory
   store is empty; `AccessToken(ctx, false)` then returns an error with `RelinkRequired() == true`.
4. Relink, then present a spent refresh token from outside the manager after the manager refreshed: the device is
   revoked, the manager's next refresh gets `invalid_grant {reason: reused}` → `refresh_reused`.
5. An admin sets `minClientVersion` above the app's version: the next dial gets `client_outdated`;
   `Manager.HelloRefused` with that error → `incompatible` with `update_app`/`min_client_version`, and
   `AccessToken` returns an error with `UpdateRequired() == true`.
6. `LinkWithPassword` with the user's password → `linked`, the device is listed with `linkedVia` `password` (store
   read) and `device.linked {via: "password"}` is in the audit log; a wrong password → 401 `invalid_credentials`, state
   `unlinked`.
7. `PATCH /me/devices/{id}` renames the device; after the hub's 5-min revalidation (advanced through the test clock)
   `user.connections` shows the new `deviceName` (skipped with a note, like case 2, until 01's C08 has merged).
8. `goleak` clean (checked over the whole file, cases 9–12 included).
9. **Grace replay, then the superseded child** (raw HTTP, `linkDevice`): refresh `P → C` with an `exchangeId`; at
   +100 s (test clock) replay `P` without one → 200 with a new pair; then present `C` → 400 `invalid_grant {reason:
   reused}`; `GET /me/devices` (cookie) no longer lists the device; the audit log has `device.refresh_reused`; the
   admin alert is raised (`srv.Logs()` has the "admin alert" WARN line with `kind=refresh_token_reused`, M1's
   `alert` helper); the new pair's access token is refused (401 `invalid_token`).
10. **Admin routes refuse a bearer**: the admin user links a device; with its bearer, `PATCH /api/v1/admin/settings`
    → 403 `forbidden {credential: session}` and the setting is unchanged; every `/api/v1/admin/*` route of the real
    wiring (#29–#43 and M1 04's doctor and bandwidth) answers the same; `POST /api/v1/invites` → 201; a WebSocket
    `hello` with the same token → `welcome` with `user.admin: true` (the WebSocket admin identity is unchanged).
11. **The /48 bucket**: `POST /device/code` from 60 different /64s inside `2001:db8:aa::/48` (`X-Forwarded-For`
    through the harness's trusted proxy) → the first 30 get 200, the rest 429 `rate_limited`; a request from
    `2001:db8:bb::1` and one from `203.0.113.9` still get 200.
12. **Network comparison**: a code started from `2001:db8:cc::7` and looked up by the cookie from `203.0.113.7` →
    `network: unknown`; started and looked up from `203.0.113.7` → `same`; from `203.0.113.7` and `198.51.100.9` →
    `different` (the checkbox each value needs is §16.3's Vitest).

### 16.3 Web

- **Vitest** (`web/src/link/*.test.tsx`, MSW): every state of §10.1; `?code=` normalized and removed from the URL;
  `network: different` and `network: unknown` (the app over IPv6, the browser over IPv4) each show their own text and
  keep Link disabled until the checkbox is ticked; `network: same` (one IPv4 address) shows the shared-address line
  and no checkbox; an unknown `network` value requires the checkbox; double click sends one decide; 404 → `invalid` with the code field;
  429 shows the wait; `normalizeUserCode` with §3's table; DevicesPage app rows (rename, remove confirmation, and the
  **This app** badge and wording only when `current` is true); `appLinkFor` with `links.json`'s valid cases. Where:
  Mac, CI ubuntu.
- **The origin rule of §11.7–§11.8** (03 owns the code; these cases are the contract): a Go test of 03's `Auth`
  binding over two `Servers` entries, A active and B linked, where `AccessToken(B, false)` and `AccessToken(B, true)`
  return `null` without a refresh or a request to either server, and `SessionEnded(B, …)` and `HelloRefused(B, …)`
  reject `not_found` without calling a `Manager`; a Vitest case of `DesktopPlatform` that passes `serverOrigin` on
  every call and ignores an `auth.state` for another origin; and 03's `desktop-link.spec` second-server case: no
  request or hello to A ever carries a token B issued (03 §22, README WA71). Where: Mac, CI ubuntu.
- **Playwright** `web/e2e/link.spec.ts` (Chrome, the e2e server): the test starts a flow with Node's `fetch`
  (`POST /device/code`), opens `verificationUriComplete` signed in as a fixture user, checks the code on the page,
  renames and links; Node's poll gets 200 with an `isa_` token; a Node WebSocket with `hello.auth` gets `welcome`; the
  Devices page lists the device; Remove closes Node's socket with 4401. A second case: signed out → `/login?next=…` →
  back on the confirm card. `a11y.spec` gains `/link`'s states. Where: Mac, CI ubuntu.

### 16.4 Manual (the owner's Windows PC; needs 03-desktop-app's and 06-windows-release's builds)

Recorded in the PR of slice DA13; they don't block merges (README §4 Testing).
- **M-DA-1 (Win11)**: install, first run, "Link this app": the browser opens `/link?code=…`, the code matches the
  app's, Link → the app shows "Linked" within 10 s. Control Panel → Credential Manager → Windows Credentials → Generic
  Credentials lists `isshoni:v1:https://<server>` with your username.
- **M-DA-2**: restart Windows; the app starts linked without any prompt; the Devices page shows the device "last seen
  just now".
- **M-DA-3**: unlink, then link again by scanning the QR code with the phone (signed in on the phone's browser).
- **M-DA-4**: on the phone, Devices → Remove the PC: within 5 s the app shows "Link this app" with the signed-out
  notice; the Credential Manager entry is gone.
- **M-DA-5**: post `isshoni://<server>/r/lounge` in a Discord message; click it with the app closed and again with it
  running: the first time "Add server <host>?" appears (after removing the server), later the Lounge opens; no share
  starts.
- **M-DA-6 (Win10 22H2)**: M-DA-1 and M-DA-2.
- **M-DA-7**: uninstall keeping the data (every uninstall runs `isshoni.exe --forget-credentials`), then reinstall,
  link and uninstall with "remove my data" (06's `/PURGE`, which runs `isshoni.exe --purge-data`): each time the
  Credential Manager entry is gone and the device disappears from the Devices page. Repeat with the network cable
  unplugged: the uninstaller still finishes within its 30 s and the entry is gone (the device then stays on the Devices
  page until removed there or expired).
- **M-DA-8**: with the app linked and a room open, set the admin's `minClientVersion` above the app's version: the app
  shows "Update the app"; "Open in browser" opens that room in the default browser; "Update now" shows one of §13.3's
  states (on a build without update keys: "This build doesn't update itself" and the Download-page link, which opens
  in the default browser, not in the app).
- **M-DA-9**: unlink, then "Sign in with username and password instead": a wrong password shows the error, the right
  one links; the Devices page lists the PC and the admin's audit log shows `device.linked` with `via: password`.

---

## 17. Interfaces other docs rely on

- **Wire and REST**: the endpoints, access levels and errors of §7.1 and §6.4; the JSON of §5, §6.2 and §7.2; the DTOs
  of §7.3 (`api.DeviceCodeRequest`, `DeviceCodeResponse`, `GrantType`, `DeviceTokenRequest`, `DeviceTokenResponse`,
  `DeviceRef`, `DeviceLookupRequest`, `DeviceLookup`, `NetworkMatch`, `DeviceDecideRequest`, `DevicePasswordRequest`,
  `RenameDeviceRequest`, `DeviceResponse`, `DeviceInfo.Current`, `NormalizeUserCode`, `FormatUserCode`);
  `POST /api/v1/device/password` (#48, §14); `/info.features` `deviceFlow`; `Authorization: Bearer isa_…` on REST and
  `hello.auth` on `/ws`; `401 invalid_token` with `WWW-Authenticate`; the CORS headers of §6.4.
- **`protocol.DesktopAssetOriginWindows = "http://wails.localhost"`** and **`protocol.DesktopOrigins() []string`**
  (§6.4; `DesktopOrigins()[0] == DesktopAssetOriginWindows`): 03-desktop-app's asset handler uses the constant as its
  `Options.Origin` and its W1 test pins the pinned Wails' origin against it. *Later (M3)*: `DesktopAssetOriginMacOS`.
- **`version.Parse(s) (SemVer, error)`, `version.Compare(a, b string) (int, error)`, `version.Below(v, floor string) (below, ok bool)`**
  in `internal/version` (§13.2): available to 06-windows-release's "no downgrades" check and 01-client-core.
- **Server Go**: `auth.(*Service)` methods of §6.8 (with `PasswordDeviceLogin` and `Touch` returning the principal);
  `auth.Principal.DeviceName` and `signal.Identity.DeviceName` (bearer only, never logged; copied by `wire.go`'s
  `AuthenticateBearer` and `Revalidate`; 01-client-core's `OwnConnection.DeviceName` reads it);
  `httpapi.UserSession`, `httpapi.UserDevice`, and `httpapi.Admin` refusing every bearer (403 `forbidden
  {credential: session}`; 04's admin routes inherit it); `httpapi.Deps.DesktopOrigins []string`; `site.ExtraOrigins =
  protocol.DesktopOrigins()` in `wire.go`, feeding `signal.Config.AllowedOrigins` (as built) and
  `httpapi.Deps.DesktopOrigins`; the store methods of §4.2; migration `0002_device_flow.sql`; `PruneStats.Devices`.
- **Client Go** (for 01-client-core and 03-desktop-app):
  - `deviceauth.Manager.AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)`: satisfies
    01-client-core's `core.Auth`; `forceRefresh` at most once per 10 s. For 01's signal client:
    `Options.Token = func(ctx context.Context) (protocol.Secret, error) { return mgr.AccessToken(ctx, false) }`;
  - its errors: `ErrNotLinked` (its type has `RelinkRequired() bool` returning true; returned when no credential
    exists or this call's refresh got `invalid_grant`), `ErrIncompatible` (`UpdateRequired() bool` returning true),
    `ErrOffline` (neither method: transient), `ErrRevokeNotConfirmed`, `ErrTooManyServers`;
  - `Manager.SessionEnded(ctx, code) State` (synchronous, single flight; `unauthenticated`, `session_revoked`,
    `account_disabled`), `Manager.HelloRefused(*protocol.Error)`, `Manager.SetIncompatible(compat.Check)`,
    `Manager.Recheck(ctx) (State, error)`, `Manager.Link(ctx)` (returns at `linking`; polls on the manager's own
    context), `Manager.LinkWithPassword(ctx, username, password)`, `State()` and `OnState` (§11.3);
  - `deviceauth.Servers` (§11.6) with `Add` returning `AddResult` (the server's own `publicUrl` adopted), and the
    `AuthService` contract with `AuthState` JSON and the `isshoni:auth.state` event (§11.7), including the rows
    `AccessToken(origin, force)`, `SessionEnded(origin, code)`, `HelloRefused(origin, error)` (each answering only
    for the active server's origin) and `LinkWithPassword(username, password)`;
    `Servers.ForgetAll` behind `isshoni.exe --forget-credentials` (every uninstall) and `--purge-data` (purge) (06's
    "DeleteAll");
  - `deviceauth.AppInfo`, `deviceauth.Client` (with `PasswordLink`), `DefaultHTTPClient`, `ErrNetwork` (wrapping the
    transport cause with `%w: %w`), `ErrBadResponse` (also for verification URIs that fail §11.2's check);
  - `LinkState.VerificationURI` and `VerificationURIComplete` are always Go's canonical `origin + "/link"` and
    `origin + "/link?code=" + UserCode` (§11.2), safe to open, draw as a QR code and copy; 03-desktop-app still opens
    them through a `Shell.OpenURL` that validates on its own;
  - `credstore.Store`, `credstore.Open`, `credstore.NewMemory`, `ErrNotFound`, `ErrUnavailable` (M3 adds the Keychain
    implementation behind the same interface);
  - `deeplink.Parse`, `deeplink.Build`, `deeplink.NormalizeOrigin`, `deeplink.Link`, `ErrUnsupported`; the command
    line `isshoni.exe --open-url <url>` (06-windows-release §2.3); the behavior table of §12.3;
  - `compat.FromInfo`, `compat.FromError` (undecided `false` for `protocol_unsupported` without `serverMax`),
    `compat.Default`, `compat.Check` with its verdicts and reasons (§13.2).
- **Test helpers** (package `itest`, §16.2): `linkDevice(t, srv *servertest.Server, cookie, name string) linkedDevice`
  (DA6) and `linkedManager(t, srv *servertest.Server, cookie string) *deviceauth.Manager` (DA11), for 01-client-core's
  desktop itests and its slice C15; `TestCompatServerDeviceFlow` for 06's `compat-servers` job (§13.4).
- **Web**: the `/link` route and `DeviceLinkPage` (only `network: same` skips the "I started this myself" tick);
  `appLinkFor(serverOrigin, roomId?)` in `web/src/lib/appLink.ts`; the `link` i18n namespace;
  `/troubleshooting#desktop-linking` (DA12), which 03-desktop-app links from its `server_unreachable` and
  `publisher_offline` texts. The SPA tells the desktop by `platform.kind === 'desktop'` (03-desktop-app), not by
  `me.device`; `/me.device` and `DeviceInfo.current` are room for later bearer clients.
- **Rules for `DesktopPlatform`** (§11.8): bearer on every `apiFetch` that has a token, and no `Authorization` header
  when `AccessToken` is `null` (no local rejection); one retry with `AccessToken(serverOrigin, true)` after a 401
  `invalid_token` or `unauthenticated` on a request that carried a token; `signaling()` returns
  `{url, auth: HelloAuthProvider}` whose provider calls `AccessToken(serverOrigin, forceRefresh)` before every `hello`;
  `SessionEnded` and `HelloRefused` through the binding with `serverOrigin`; `auth.state` for another origin ignored;
  logout through `Unlink()`; no stale-build reload; `versionActions()` with `updateApp` and `openInBrowser` (the current
  room).

---

## 18. Depends on

**docs/m2/01-client-core.md**
- The Go signaling client keeps `Options.Token` as 01 §15.3 built it (called before every `hello`); the core's
  `core.Auth` is `AccessToken(ctx, forceRefresh bool)`, which `*deviceauth.Manager` implements. The core calls
  `AccessToken(ctx, true)` once after `unauthenticated`, then dials again (its §11.3), and decides "unlinked" from an
  error with `RelinkRequired() == true`. It treats an `Auth` error with `UpdateRequired() == true` like
  `client_outdated` (stop `update_required`), so it doesn't back off forever while the manager is incompatible.
- `StopError` carries the server's `*protocol.Error` (with its params) as `Err`, which 03-desktop-app passes to
  `Manager.HelloRefused` (§13.1).
- The depguard rule (its D2) lets `internal/client` packages import each other, `internal/version` and
  `internal/protocol/api` (§11.1); third-party modules (`x/sys/windows`, `x/net/idna`) stay with the license gate.
- Both of the app's connections send `client.kind: desktop`; the SPA's connection uses role `viewer`, the Go one
  `publisher` (01 §6.3).
- Its desktop itests and slice C15 use `linkDevice` and `linkedManager` from package `itest` (§16.2), so they live in
  that package.

**docs/m2/03-desktop-app.md**
- The `AuthService` binding and the `isshoni:auth.state` event of §11.7 (its adapter maps `AccessToken(origin, force)`
  onto the active `Manager` only when the origins match, reloads the window on every change of the active server, and
  binds `Link`, `CancelLink` and `LinkWithPassword` as `Auth.Link`, `Auth.CancelLink` and `Auth.LinkWithPassword`);
  the "Link this app" screen (code, URL, the QR code of `verificationUriComplete` rendered in the SPA with M1's
  uqr-based `QrCode` component, "Copy link", "Get a new code", the notices of §11.3, and `ErrBadResponse` from `Link`
  shown as its generic link error), the password form ("Sign in with username and password instead") over
  `LinkWithPassword`, the "Add server" modal and its rules (§12.3; "This server calls itself {host}" from
  `AddResult`), the update screens (§13.3, over 06's updater states), first-run wizard step 1 = `Manager.Link`.
- C2's adapter calls `Manager.HelloRefused` on `client_outdated` and `protocol_unsupported` and
  `Manager.SessionEnded` on `unauthenticated`, `session_revoked` and `account_disabled`, and starts a new core client
  when `State().Status` is `linked` afterwards.
- `DesktopPlatform` follows §11.8; the desktop router leaves out `/link`; `versionActions().openInBrowser` opens
  `origin + "/r/" + <the current room>` (or `origin + "/"`), changed from `serverOrigin` only.
- Wails single-instance forwarding of `--open-url <url>`, and parsing the `--forget-credentials` and `--purge-data`
  flags (06-windows-release owns the command line; §11.7, §12.2); the app's data root (`%LOCALAPPDATA%\isshoni`)
  passed as `ServersOptions.Dir`.
- 03's CSP allows `connect-src` to the active server's https and wss origin (03 §5.3); 02 needs nothing more, because
  the SPA calls only the active server and Go makes every other request. In dev and e2e builds with an http loopback
  server (`AllowInsecureLoopback`), 03 uses `http://` and `ws://` for that host.
- Its asset handler uses `protocol.DesktopAssetOriginWindows`, and its W1 test pins the pinned Wails' asset origin
  against it.
- `Shell.OpenURL` applies `App.OpenExternal`'s rule itself (absolute `https:`; `http:` loopback only in dev and e2e
  builds; anything else refused), so no caller, the link step included, can hand a raw server string to the shell
  (§11.2, §11.3 step 2).
- Its `server_unreachable` (probe, §8.1 there) and `publisher_offline` texts gain one line, "Opens in your browser
  but not here? See why", linking `/troubleshooting#desktop-linking` (the proxy limit of §11.2).
- No desktop screen calls an `/api/v1/admin/*` route with a bearer (its D7); those routes refuse one (§6.4).

**docs/m2/06-windows-release.md**
- The NSIS installer registers `isshoni://` with `--open-url "%1"` (§12.2), and the uninstaller runs
  `isshoni.exe --forget-credentials` (every uninstall) or `--purge-data` (`/PURGE`), both calling `Servers.ForgetAll`
  (which 06 calls "DeleteAll"), before removing files.
- The updater backs "Update now" (§13.3) with `CheckNow`, `Status` (`State`, `PolicyOff`, `ErrorCode`, `Progress`)
  and `Apply`; it compares versions with `internal/version`.
- Release notes flag schema version 2 (06-windows-release.md §11.8, §4.1 here).
- Its Windows Go test job (`windows-go`, windows-2025) runs `go test -race ./internal/client/...` (MinGW), then
  `CGO_ENABLED=0`, per PR, and `lint-go` cross-lints `GOOS=windows`; its nightly `compat-servers` job runs
  `TestCompatServerDeviceFlow` (§13.4) with `ISSHONI_COMPAT_SERVER_URL` and `ISSHONI_COMPAT_ADMIN_FILE` (`{origin,
  username, password}`).
- Its R14: the `/privacy#desktop` section takes §9's "On the PC" facts (and drops "which come later"), and
  `docsanchors_test.go` gains `/troubleshooting#desktop-linking`.

**M1 as built** (`docs/m1/`): 01's hub (`AllowedOrigins`, the bearer path at `hello`, `Revalidate`, `CloseConnections`
with `DeviceID`, close codes 4401/4403/4426), 01 §15.3's Go client; 03's store, tables (`devices`, `device_tokens`,
`device_codes` from `0001_init.sql`), `auth.Service`, limiter, revocation table, audit log, settings
(`minClientVersion`) and REST chain, and `auth.Login`'s order and buckets (`login.go`); 04's `Site.Origin`,
`Site.ExtraOrigins`, `ClientIP` and wiring (`wire.go` adapters, already shaped for devices); 05's router slot `/link`,
the Platform adapter with its `DesktopPlatform` slot and `versionActions`, DevicesPage and the uqr `QrCode`
component; M1 06's CI, license gate and site.

---

## 19. Implementation slices

Ordered; each is independently testable and lands with its tests. "Touches" lists paths (README §5's rules: a Go
package means its directory). Days are rough. DA1–DA7 are server and web and can start at once; DA8–DA11 need only
DA1 and can run beside them; DA13 waits for 03-desktop-app's and 06-windows-release's builds.

| Id | Slice | Touches | Acceptance | Needs | Verified | Days |
|---|---|---|---|---|---|---|
| DA1 | **Shared names**: `api` device DTOs (with `DevicePasswordRequest`) with redaction and golden JSON, `NormalizeUserCode`/`FormatUserCode`, `DeviceInfo.Current`; `protocol.DesktopAssetOriginWindows` and `protocol.DesktopOrigins`; `internal/version/semver.go` exported and the hub switched to it (its `semver.go` removed); `task gen` | `internal/protocol/api`, `internal/protocol`, `internal/version`, `internal/server/signal` (semver only), `web/src/protocol/*.gen.ts` | §16.1 rows for `api`, `protocol`, `version`; the hub's tests unchanged and green; tygo drift check green | — | Mac, CI ubuntu | 1 |
| DA2 | **Store**: migration 0002, every §4.2 method, the devices prune rule, `PruneStats.Devices` | `internal/server/store` | §16.1 store row; the migration test from a schema-1 file with rows; 03 §15's schema tests green | DA1 | Mac, CI ubuntu | 1.5 |
| DA3 | **Device flow in auth**: `StartDeviceFlow`, `LookupUserCode`, `DecideUserCode`, the `device_code` grant of `DeviceToken`, `PasswordDeviceLogin` (§14, sharing the grant's success `Write`), `NormalizeDeviceName`, the four buckets (`device-code-ip48` with `IPKey48` included), the 5,000 pending-code cap, the device cap, audit rows `device.approved/denied/linked` (both `via`), `auth.throttled` scopes | `internal/server/auth` | §16.1 device-flow row, password-link cases included (fake clock, counting hasher/store fakes) | DA2 | Mac, CI ubuntu | 2.5 |
| DA4 | **Tokens in auth**: `AuthenticateBearerToken` and `Authenticate`'s bearer path with the cache, `Principal.DeviceName`, the `refresh_token` grant (rotation, reuse, grace with the superseded child kept as a spent row), `Touch` for devices (returning the principal), `RenameDevice`, `RevokeOwnDevice`, bearer cases of `RevokeDevice`/`LogoutEverywhere`, `device.expired` in the janitor log, the `refresh_token_reused` alert | `internal/server/auth` | §16.1 tokens row; the M1 revocation matrix tests still green with device rows present | DA3 | Mac, CI ubuntu | 2 |
| DA5 | **REST**: `device.go` routes (#44–#49, #52; #48 `/device/password` included), `Access` `UserSession`/`UserDevice`, `Admin` refusing a bearer, the credential step, the CORS step, `Deps.DesktopOrigins`, the §6.4 access changes, `/me` and `/me/devices` for bearers, `PATCH /me/devices/{id}`, `/info` `deviceFlow`, notify topics, audit coverage | `internal/server/httpapi` | §16.1 httpapi row (endpoint table, credential and CORS matrices, audit coverage) | DA4 | Mac, CI ubuntu | 2 |
| DA6 | **Hub and wiring**: the pre-auth refund; `signal.Identity.DeviceName`; `wire.go`: `site.ExtraOrigins = protocol.DesktopOrigins()` after `config.NewSite` (the hub's `AllowedOrigins` line stays as built) and `httpapi.Deps.DesktopOrigins = s.site.ExtraOrigins`, `AuthenticateBearer` → `AuthenticateBearerToken`, `identityOf` and `Revalidate` copying `DeviceName` (a change applied like a changed `Name`); `itest/device_helpers_test.go` with `linkDevice`; `itest/device_test.go` cases 1–3 and 5 with `linkDevice` and raw HTTP in place of the manager (the manager comes in DA11), and cases 9–12 (raw HTTP only) | `internal/server/signal`, `internal/server` (wire.go), `internal/server/itest` | §16.1 signal row; §16.2 cases 1–3, 5 through `linkDevice`, and 9–12; `goleak` clean | DA5 | Mac, CI ubuntu | 1 |
| DA7 | **Web**: `web/src/link/`, the router line, DevicesPage app rows, `lib/appLink.ts`, i18n (`lazy/link.en.json`, two `errors.*` keys), Vitest, `web/e2e/link.spec.ts`, `a11y.spec` states | `web/src/link`, `web/src/app/router.tsx` (one line), `web/src/account`, `web/src/lib/appLink.ts`, `web/src/i18n`, `web/e2e` | §16.3; `task lint typecheck test e2e` green; initial JS stays under 05's 200 KB budget (the page is lazy) | DA6 | Mac, CI ubuntu | 2 |
| DA8 | **Client compat and deep links**: `internal/client/compat` (with the undecided `FromError` case), `internal/client/deeplink` with `links.json` and the fuzz target | `internal/client/compat`, `internal/client/deeplink` | §16.1 compat and deeplink rows; 06's `lint-go` (`GOOS=windows`) green | DA1 | Mac, CI ubuntu | 1 |
| DA9 | **Credential store**: interface, memory, file (non-Windows), Windows Credential Manager | `internal/client/credstore` | §16.1 credstore row on ubuntu and on 06's `windows-go` job | — | Mac, CI ubuntu, CI windows (06 windows-go) | 1.5 |
| DA10 | **deviceauth**: `Client` (with `PasswordLink`, `ErrNetwork` wrapping), `Manager` (link with its own polling context, `LinkWithPassword`, refresh, rotation, retries, `AccessToken(ctx, forceRefresh)`, the errors with `RelinkRequired`/`UpdateRequired`, `SessionEnded`, `HelloRefused`, `SetIncompatible`, `Recheck`, `Unlink`, daily refresh), `Servers` and `servers.json` (`Add` adopting `publicUrl`), concurrent `ForgetAll` | `internal/client/deviceauth` | §16.1 deviceauth row (httptest server, memory store, synctest) | DA8, DA9 | Mac, CI ubuntu, CI windows (06 windows-go) | 3 |
| DA11 | **End to end**: `itest/device_helpers_test.go` gains `linkedManager`; `itest/device_test.go` cases 1–8 with the real `Manager` and 01's Go signal client; `internal/client/deviceauth/compat_server_test.go` (`TestCompatServerDeviceFlow`, §13.4) | `internal/server/itest`, `internal/client/deviceauth` (test file) | §16.2 complete; `goleak` clean; `TestCompatServerDeviceFlow` skips without `ISSHONI_COMPAT_SERVER_URL` and passes against a locally started current server with an admin file | DA6, DA10 | Mac, CI ubuntu; nightly `compat-servers` (06) | 1.5 |
| DA12 | **Docs and write-back**: `docs/privacy.md` server-side device rows only (§9's table; the "On the PC" facts and the "which come later" edit are 06's R14 in `#desktop`), `docs/security.md` credential threat model, `docs/troubleshooting.md` "Linking the app" with the fixed anchor `#desktop-linking` (the proxy limit, worded for a friend who sees the server open in the browser but the app say "Can't reach the server": the browser and the app's window use the Windows proxy settings, the app's own connection doesn't yet, and setting `HTTPS_PROXY` for the user is the workaround; "code expired", "signed out", "wrong password", Credential Manager; 06 R14 adds the anchor to `docsanchors_test.go` so the app may link to it), pointers from the 03 sections named at the top to this document, and in M1 03 §7.12 the bearer line becomes "works on every user endpoint and on the WebSocket; admin routes are cookie-only (02 §6.4)" | `docs/privacy.md`, `docs/security.md`, `docs/troubleshooting.md`, `docs/m1/03-accounts-and-store.md` | `task site:build` green; the privacy page's device rows match §9 | DA5 | Mac, CI ubuntu | 0.5 |
| DA13 | **Manual checks** M-DA-1 to M-DA-9 on Win11 and Win10, recorded in the PR | — | every check passes or has an issue filed | DA11, DA12, 03-desktop-app's first-run wizard, password form and `AuthService`, 06-windows-release's installer and updater | owner's PC | 0.5 |

About 20 days of work; DA1–DA7 (server and web, ~12 days) and DA8–DA10 (client, ~5.5 days) can proceed in parallel.

---

## 20. Decisions left to the owner

None open. The in-app username/password fallback follows the plan ("Privacy and trust model") and M1 03 §12.3 and is
built in M2 (§14, decision 12). Should the owner prefer to defer it after all, 03-desktop-app drops
`Auth.LinkWithPassword`, the wizard's "Sign in with username and password instead", its autofill note and the
password cases of its e2e specs (its CDP spec then approves through `/device/lookup` and `/device/decide` with a
cookie session), and this document drops #48, `PasswordDeviceLogin`, `PasswordLink`, `LinkWithPassword` and their
tests.
