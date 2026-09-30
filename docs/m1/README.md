# M1 design: overview and implementation plan

M1 is "Watch together": the isshoni server and the web client. This page is the short version of the six M1 specs
(about 13,000 lines together). It says what M1 delivers, how the parts fit, the rules every slice follows, and the
order in which the work gets built. `docs/PLAN.md` stays the source of truth for product decisions; the six specs are
the source of truth for their own parts. Where this page and a spec disagree, the spec wins and this page gets fixed.

## 1. What M1 delivers

From the plan's roadmap:
- Accounts: the first admin through a one-time setup link, invite links, an optional approval queue, the Lounge room
  (admins can add rooms).
- Signaling with reconnect and resume; an SFU with two simulcast layers and audio-follows-focus.
- A web viewer on every current browser, including iPhone/iPad Safari and Android Chrome; an installable PWA with Web
  Push ("Alex started sharing"); a Chrome/Edge sharer (window + its audio).
- TLS modes `auto`, `ip`, `manual` and `off`; ICE-TCP on 443 next to HTTPS.
- `install.sh` and `compose.yaml`; a setup wizard with a real connection test; `doctor`; ops endpoints, the admin
  dashboard, the admin socket, backup and restore.
- A project site with the install script, privacy and code-signing pages.

Not in M1: native apps (M2+), mobile apps, TURN, E2EE, recording (never). M1 only leaves room for the native apps:
the `publisher`/`agent` roles, the bearer field in `hello`, the device-flow tables, and the SPA's Platform adapter.

**Exit criterion**: 5 friends finish a 2-hour session on a small VPS with no manual fixes, and iPhone and Android can
watch. The run book and the pass criteria are in [06 §12](06-deploy-and-ci.md).

### The six specs

- [**01-protocol.md**](01-protocol.md): signaling protocol v1. One WebSocket per client at `GET /ws`, JSON envelopes
  `{type, id, re, data}`, `hello`/`welcome` with version, features, role and codec caps. Room state goes out as
  coalesced `room.state` snapshots plus `room.event`s; subscriptions are explicit (`subscribe.update`), which is how
  audio-follows-focus works. Two PeerConnections per client (pub offered by the client, sub by the server), guarded by
  `gen`/`neg` counters. Reconnect is state resync with a 30 s resume grace, never message replay. Owns
  `internal/protocol` (generated into TypeScript by tygo, golden fixtures, N−1 compatibility), the hub
  `internal/server/signal`, the Go client, the `sfuplane` adapter, the media integration tests and the TS `SignalClient`.
- [**02-sfu.md**](02-sfu.md): `internal/server/sfu` on Pion v4. Rooms, participants and connections (one actor
  goroutine per connection), shares with two simulcast layers (`f`, `q`), keyframe-aligned layer switches, one packet
  cache per layer for NACK/RTX, forwarded sender reports for A/V sync, a Galene-style downlink allocator, a per-room
  H.264 profile policy (High, or Constrained Baseline when Firefox watches), the connection-test probe PCs, guards and
  metrics. Also the fake media source, the Go publisher and test viewer, and `isshoni-loadtest` with its load gate.
- [**03-accounts-and-store.md**](03-accounts-and-store.md): SQLite through modernc (one writer, WAL, forward-only
  migrations with a backup first, refusal of a newer schema), PRECIS usernames, argon2 passwords, web sessions (30 days
  idle, rotated daily), HMAC-hashed setup, invite and reset tokens, registration modes and the approval queue, roles and
  the last-admin rule, rooms, runtime settings in the DB (pinnable from TOML), the audit log, push subscriptions, and
  every `/api/v1` endpoint with one error envelope. The device-flow tables ship now; its endpoints are M2.
- [**04-server-platform.md**](04-server-platform.md): `cmd/isshoni` and its exit codes, config (TOML, env, flags; one
  key registry), `secrets.json` and the data directory, startup, readiness and shutdown, the 443 first-byte
  multiplexer and ICE transports (`netx`), public-IP detection, TLS through certmagic, the HTTP router, security headers
  and SPA serving, logging, ops (health, metrics, transfer accounting, dashboard, release check, admin socket, backup
  and restore), `doctor`, the Web Push sender, `servertest`, and the wiring that joins all packages.
- [**05-web-client.md**](05-web-client.md): the React SPA embedded in the binary. Boot and error screens, auth pages,
  the room with the viewer (tile grid, focus, fullscreen, keyboard, audio follows focus, tap to unmute), the
  Chrome/Edge sharer (presets, two layers, the whole-screen audio warning), recovery rules, the setup wizard with the
  connection test, account and admin pages, the PWA with a hand-written service worker and Web Push, i18n and
  accessibility. A Platform adapter lets the M2/M3 desktop apps reuse the same SPA.
- [**06-deploy-and-ci.md**](06-deploy-and-ci.md): the deployment contract (ports 80, 443, 7882 UDP+TCP; paths; the
  `isshoni` user), a thin `install.sh` over a signed tarball, the systemd unit, sysctl and firewall files, deb/rpm, the
  distroless Docker image and compose files, the contributor workflow (mise, `Taskfile.yml`, npm), GitHub Actions with
  one required check `ci-ok`, the license gate, the signed release pipeline, the VitePress site, nightly VM tests and
  the exit-test run book.

## 2. Architecture

```
 Browsers (SPA from web/, PWA)            isshoni serve  (one Go binary, CGO_ENABLED=0)
                                     :80    tlsmgr: ACME HTTP-01, redirect to HTTPS
 HTTPS: SPA, REST /api/v1, WSS /ws ─► :443  netx.PortMux ─┬─ TLS ─► httpapi router ─┬─ SPA (web.Dist)
 ICE-TCP (first byte ≠ 0x16) ───────►                     │                          ├─ /api/v1 ─► auth ─► store (SQLite)
                                                          │                          ├─ /ws ─► signal.Hub ─► sfuplane ─► sfu
                                                          │                          └─ ops routes: dashboard, doctor, conntest
                                                          └─ ICE ──────────────────┐
 ICE UDP and ICE-TCP ───────────────► :7882 netx.Transport (UDP mux, TCP mux) ◄────┴── sfu (pub, sub and probe PCs)
                                     unix   /run/isshoni/admin.sock ─► ops admin API ◄── isshoni CLI (setup-url, admin …)
                                     out    push.Service ─► browser push services (VAPID)
                                     :9469  /metrics on 127.0.0.1 (off by default)
 internal/server (wire.go) builds every part and holds every adapter between 01, 02, 03, ops and push.
```

| Path | Spec | What |
|---|---|---|
| `cmd/isshoni` | 04 | CLI: `serve`, `setup-url`, `doctor`, `healthcheck`, `admin …`, `config …`, `version` |
| `cmd/isshoni-loadtest` | 02 | load test on the Go client and publisher |
| `internal/version`, `internal/logx` | 04 | build info; slog setup, `Secret`, Pion and certmagic log bridges |
| `internal/protocol` (+ `gen/tsregistry`, `testdata/`) | 01 | wire types, registry, fixtures; stdlib only |
| `internal/protocol/api` | 03 (04 adds ops DTOs) | REST DTOs, error codes and statuses; stdlib only |
| `internal/client/signal` | 01 | Go signaling client (tests, load test; native apps from M2) |
| `internal/client/publish`, `internal/media/fake` | 02 | Go RTP publisher; fake test-pattern and tone media |
| `internal/server` | 04 | lifecycle and wiring (`wire.go`) |
| `internal/server/{config,netx,tlsmgr,ops,ops/doctor,push,servertest}` | 04 | config and secrets; 443 mux, ICE transports, public IP; TLS; health, metrics, dashboard, admin socket, backup; doctor; Web Push sender; in-process test server |
| `internal/server/httpapi` | 04 (router) + 03 (`API`, handlers) | HTTP router, middleware, JSON helpers, SPA; `/api/v1` |
| `internal/server/{store,auth}` | 03 | SQLite, migrations, settings; passwords, sessions, invites, roles |
| `internal/server/{signal,signal/signaltest,sfuplane,itest}` | 01 | hub; fakes; adapter to the SFU; media integration tests |
| `internal/server/sfu`, `sfu/sfutest` | 02 | SFU; harness, Go viewer, fault injection |
| `web/` (`web/src/protocol/` is 01's) | 05 | SPA, `embed.go`, Playwright e2e |
| `deploy/`, `.github/`, `tools/`, `docs/` site, root files | 06 | install, packaging, Docker, CI, release, site |

Which package may import which is one table, 04 §2; it is also the source of the `depguard` rules.

## 3. Integration decisions

The specs were written in parallel and then reconciled. The main outcomes:

**Changes against the plan**
- npm replaces pnpm (Node 26 no longer ships corepack), with one lockfile each in `web/` and `docs/` (06 D12).
- The Docker binary lives at `/usr/local/bin/isshoni`, data at `/var/lib/isshoni` (a required volume; without it the
  server exits 78), `/run/isshoni` is a tmpfs (06 D5–D6, 04 §5.1). Stricter than the plan's "doctor fails".
- `pc.offer`/`pc.answer` may be up to 256 KiB after `hello`; every other message keeps the plan's 64 KiB (01 D11).
- New packages not in the plan's layout: `netx`, `logx`, `version`, `servertest`, `sfuplane`, `itest`,
  `internal/protocol/api`, plus `internal/media/fake` and an M1 subset of `internal/client/publish`.

**Who owns what**
- 01 owns the wire. 02's Go API reaches it only through 01's `sfuplane`. The hub owns share ids, share limits, the
  share lifecycle and both 30 s timeouts; the SFU owns `gen`/`neg` for both PC kinds and only reports media facts.
- The WebSocket Origin check and pre-auth limits are in the hub; 03 only checks credentials (cookie in M1; the bearer
  path answers `unauthenticated` until M2).
- One REST error envelope and code table (03 §12.2) for 03 and 04; JSON is camelCase everywhere.
- Push subscription REST and tables are 03's; the sender and its triggers are 04's. The admin dashboard is one 04
  endpoint with 03's `accounts` part inside. The connection test is 04's `POST /api/v1/conntest` on 02's probe PCs.
- CLI names, flags and exit codes are 04's; every adapter between 01, 02, 03, ops and push is in `wire.go` (04 §6.6).

**Behavior**
- No maintenance mode: a state that a restart can't fix exits 78, and recovery is offline (`admin restore --offline`).
  `rotate-secrets` restarts the server. No `sd_notify` (the unit is `Type=exec`).
- Firefox without H.264: only the server retries (it rebuilds the sub PC every 20 s, at most 9 times); the client
  only sends `caps.update`.
- "Watching" means receiving a share's video at any layer or its audio. No kick in M1: admins disable the account.
- The web sharer keeps its capture for 60 s during a server outage. No Pause in the web sharer (M2).
- Layer pausing is on by default (`sfu.pause_unwatched_layers`). Any signed-in user may run the connection test;
  only admins see the provider fix text. Non-admin invites sit behind `membersCanInvite`, default off.
- The transfer allowance is not a wizard step (admins set it in Settings). "X started streaming" shows the name and
  the room, and each user can switch it off.
- Site at `https://moonwx.github.io/isshoni/`; `PLAN.md` and `docs/m1/` are not published there. `compose.yaml` uses
  `:latest` during 0.x.

**Decided in this plan**
- Slices were re-cut to 0.5–3 days and ordered for parallel work: 02 slice 6 is split in two, resilience comes before
  repair, and the probe and observability come before downlink adaptation; 03's slices are regrouped; 04 S4 is split
  into the router (S17) and the server (S31), which also brings `ops/health.go`; 01 P8 is split into the adapter (S50)
  and the harness with the wiring (S59); 05's large slices are split, and the browser e2e specs run in their own
  slices once the server is wired (S66, S72).
- `web/embed.go` and `web/dist/.gitkeep` come with the web scaffold (S09), not the repo skeleton, so group 1 stays
  disjoint. The TCP 443 candidate test of 02 slice 5 moves to S29. The load-test `smoke` runs as a Go test inside
  `test-go`, so it needs no extra CI job.

## 4. Conventions

### Errors
- **Wire**: `error{code, scope, retryable, params}` with a stable snake_case `ErrorCode` (01 §12.1) and the close codes
  of 01 §12.2. Codes are never renamed or reused. Clients handle unknown codes by `scope` and `retryable`.
- **REST**: one envelope `{"error": {code, fields?, params?, retryAfter?, requestId?}}` and one code table with HTTP
  statuses (`api.StatusOf`, 03 §12.2); 04 adds rows to the same table.
- **No English on the wire.** The SPA maps codes to `errors.<code>` and `fieldErrors.<field>.<code>` in `en.json`.
  `internal` errors carry an 8-character `ref` that is also logged.
- **Go**: each package has its own sentinel or typed errors (`store.ErrNotFound`, `store.ErrNeedsOperator`,
  `sfu.Error` with `sfu.*` codes, `signal.ErrInvalid`). Only adapters (`sfuplane`, `wire.go`) translate them into wire
  codes. Wrap with `%w`, test with `errors.Is`/`As` (`errorlint` is on).
- **Process exit codes** are 04 §3.2: 78 means "a restart can't fix this" (systemd stops retrying), 2 is usage.

### Context, goroutines and shutdown
- Every function that blocks or does I/O takes `ctx context.Context` first. HTTP handlers use `r.Context()`.
  `context.Background()` appears only in `main`, in top-level goroutines and in tests.
- Long-lived parts have an explicit life: `New` starts nothing, then `Run(ctx)` or `Start`, then `Close()` or
  `Shutdown(ctx)`. Every goroutine has one owner that waits for it. `SFU.Close` returns within 1 s; the whole server
  stops within 10 s (the unit allows 20 s).
- Shutdown order (04 §6.4): readiness off, then the hub (`server.shutdown`, close 1012), the SFU, the Transport,
  the HTTP servers and the 443 mux, and last push, the admin socket and the store.
- Pion callbacks never block: they post to the connection's actor (02 §5.4). No lock is held across a Pion call, a
  network write or a channel send that can block.
- Timers and tickers stop on close. Time-dependent code is tested with `testing/synctest`, or with the clock hooks
  that 02 and 03 define.

### Logging
- Only `log/slog` through `internal/logx`: text on a TTY, JSON lines otherwise. `fmt.Print*` and `log.*` are
  forbidden in `internal/server` (`forbidigo`).
- Attribute names: `component`, `user_id`, `room_id`, `share_id`, `conn_id`, `remote_ip`, `route`, `err`.
- Never logged: SDP, ICE candidates, tokens, passwords, cookies, usernames, share labels, full push endpoints (only
  `push_host`). Secrets travel as `logx.Secret`. `remote_ip` appears only on security events; there are no access logs.

### Config
- `config` is imported only where 04 §2 allows it (`cmd/isshoni`, the wiring, `httpapi`, `tlsmgr`, `ops`, `push`).
  `netx`, `store`, `auth`, `signal` and `sfu` take plain option structs that the wiring fills.
- One key registry (`config/keys.go`); S15 registers every M1 key, including 01's and 02's. Env names are
  `ISSHONI_` + the key path; any other `ISSHONI_*` name must be on 04 §4.2's reserved list.
- Settings an admin changes at runtime live in the DB (`store.SettingsCache`); a TOML policy key can pin one.
  Secrets come only from `config.SecretStore`.

### Testing
- `task test:go` runs `CGO_ENABLED=1 go test -race -count=1 ./...` (the race detector needs cgo; the shipped binary
  is `CGO_ENABLED=0`). `-short` skips only the slow cases (10×10); CI runs everything.
- `goleak` in every package that starts goroutines. Golden files live in each package's `testdata/`; protocol fixtures
  are frozen per release (01 §14.3). Fuzz seeds live in `testdata/fuzz`, so `go test` replays them; a slice runs its
  fuzzers for 30–60 s before its PR.
- One harness per kind: `servertest.Start` for an in-process server (04), `internal/server/itest` with `sfutest` and
  the Go client for media (01, 02), `testscript` for the CLI (04).
- Web: Vitest with jsdom (setup file `web/src/test/setup.ts`), MSW for REST, the fake RTCPeerConnection in
  `web/src/test/` and the fake WebSocket in `web/src/protocol/testing/`; Playwright on Google Chrome, headful under
  xvfb in CI. Each spec file belongs to one slice.
- Everything automated runs on the Mac (`task test`, `task e2e`) and in GitHub Actions; Linux-only checks (systemd,
  distro containers) run in CI. Acceptance items marked "Manual:" are recorded in the PR and don't block the merge;
  the exit test collects them (05 §19.4).

### Naming
- Go module `github.com/MoonWX/isshoni`; a package's name is the last element of its path.
- Wire message types are dotted lower case (`room.join`); JSON fields camelCase; error codes snake_case.
- IDs are 12-character lowercase Crockford base32 (`store.NewID`); the default room is `lounge`.
- Config keys are dotted snake_case (`limits.max_bitrate_kbps` → `ISSHONI_LIMITS_MAX_BITRATE_KBPS`); metrics are
  `isshoni_*` with `_total` on counters; i18n keys follow 05 §16.5.
- TypeScript: components `PascalCase.tsx` with a sibling `*.module.css`, other modules `camelCase.ts`; generated files
  end in `.gen.ts` and are never edited by hand.

### Interfaces first
The first slice of a package declares its whole public interface as the spec gives it, with fakes, even when later
slices fill in the behavior (unfinished methods return a clear "not implemented" error). S11 does this for the hub's
`Deps`, S24 for `httpapi.Signal`, `Push` and `InfoSource`, S29 for the SFU API, S27 for the web `Platform`. Adapters
and fakes then never chase a moving interface.

### Branches and PRs
- `main` is the default branch; its ruleset requires a pull request and the `ci-ok` check (06 §8.6).
- M1 work lands on `m1/server-web` first. Each slice has its own branch, `m1/sNN-<short-name>`, created from the tip
  of `m1/server-web` when its group starts, and its own git worktree.
- Each slice opens a PR into `m1/server-web`; CI runs on it. The integrator merges a group's PRs in ID order, then
  runs `task lint test build e2e` on the result before the next group starts.
- `m1/server-web` goes into `main` by PR at the end of each group (and at the end of M1), with `ci-ok` green.
- Every commit is signed (SSH) under the owner's identity; PRs are merged in the GitHub web UI. Nobody changes
  signing or Git settings, force-pushes `main` or `m1/server-web`, or rewrites merged history.
- Commit subjects start with the slice id: `S27: Web app shell: platform, REST, boot`.

## 5. Implementation plan

### How the plan works
- 94 slices in 15 groups. A group starts when the previous one is merged into `m1/server-web` and green.
- Slices in one group touch **disjoint paths** ("Touches"), so separate agents can build them at the same time in
  separate worktrees. A Go package means its own directory only, not its subpackages; other paths are prefixes. A
  slice changes only its listed paths and the shared files below; if it needs more, it stops and asks the integrator.
- "Needs" lists the slices that must be merged first; they are always in earlier groups.
- Acceptance must pass in the slice's PR. A check that needs another slice of the same group (for example a Go build
  before S01's `go.mod` is merged) is run by the integrator right after the group merges.
- Days are rough estimates for one developer. They add up to about 195 days. Built group by group with parallel
  agents, the longest slice of each group adds up to about 41 working days, in line with the plan's ≈ 8 weeks;
  review and integration come on top.
- The critical path runs through `netx` and the SFU: S05 → S16 → S26 → S29 → S41 → S52 → S57 → S63 → S69 → S74 →
  S81 → S86 → S93 → S94. Milestones: accounts, REST and signaling in `serve` after group 6 (S54); the SFU in `serve` after group 7
  (S59); the first browser "watch together" after group 8 (S66); a signed release candidate after group 9 (S73).
- Cuttable if M1 runs late: S51 (same-user relay) and S88 (uplink control and layer pausing). See the questions.
  A cut slice is removed from every Needs list. If S88 is cut, S92 runs without layer pausing.

### Shared files and their owners

| File | Owner | Rule for every other slice |
|---|---|---|
| `go.mod`, `go.sum` | S01 | A slice may `go get` the modules it needs (versions below) and commit the change. On a conflict the integrator takes the base files, re-runs each slice's `go get`, then `go mod tidy` and `task lint:go` |
| `tools/go.mod` | S01 | pins tygo v0.2.21, go-licenses v2, govulncheck; our tools go in `tools/<name>/` (S18, S67, S94) |
| `Taskfile.yml` | S01 | S01 writes every task of 06 §7.2 up front (tasks without inputs yet exit 0 with a note). Later slices may only change the body of a task they own (`test:sh`, `deploy:test`, `docker:smoke`, `release:*`, `site:*`) |
| `.golangci.yml`, `.gitignore`, `.tool-versions`, `tygo.yaml` | S01 | integrator only |
| `.github/workflows/ci.yml` | S02 | only the slices that list it (S18, S44, S61, S68, S73, S78, S82), never two in one group |
| `web/package.json`, `web/package-lock.json` | S09 | S09 installs every package of 05 §2 at once; S18 edits the `build` script; new packages go through the integrator |
| `internal/server/config/keys.go` | S15 | integrator only |
| `internal/protocol`, `internal/protocol/api` | S03, S04 | additive changes only, called out in the PR; the integrator re-runs `task gen` |
| `web/src/protocol/*.gen.ts` | S10 (generated) | never edited or merged by hand: after each merge run `task gen` and commit the output |
| `web/src/i18n/en.json` | S09 | each slice adds keys under its own namespaces only; a conflict is resolved by keeping both sides; `check:i18n` must pass (S09 writes `web/scripts/check-i18n.mjs`; S27, S37 and S93 add its later rules) |
| `web/src/app/router.tsx` | S27 | S27 declares every route of 05 §5, each lazily loading its folder; page slices fill only their folder |
| `internal/server/wire.go` | S54 | only the slices that list `internal/server` |

**Go modules** (every one must pass the license gate of 06 §8.3). The first slice to need a module adds it at the
current release; later slices keep that version.

| Module | First needed by |
|---|---|
| `github.com/pion/webrtc/v4` v4.2.x (S4 used v4.2.22) with `pion/rtp`, `rtcp`, `sdp/v3`, `interceptor`; `pion/ice/v4` | S06; S16 |
| `modernc.org/sqlite` | S07 |
| `golang.org/x/text`, `golang.org/x/crypto` | S08 |
| `github.com/coder/websocket` | S11 |
| `github.com/pelletier/go-toml/v2` | S15 |
| `github.com/caddyserver/certmagic` (brings zap and acmez) | S44 |
| `github.com/SherClockHolmes/webpush-go` | S32 |
| `github.com/prometheus/client_golang` | S55 |
| `github.com/skip2/go-qrcode` | S43 |
| `go.uber.org/goleak`, `github.com/rogpeppe/go-internal` (tests only); `golang.org/x/{sys,mod,time}` | first user |

### Slices

"Grp" is the parallel group. "Spec" gives the source slice and sections (for example `02 slice 7` or `05 W6`).

| ID | Grp | Slice | Spec | Touches | Needs | Days | Acceptance |
|---|---|---|---|---|---|---|---|
| S01 | 1 | Repo skeleton and dev loop | 06 S1; 06 §7.1–7.3 | root: `go.mod`, `go.sum`, `Taskfile.yml`, `.tool-versions`, `.golangci.yml`, `.gitignore`, `tygo.yaml`, `LICENSE`, `NOTICE`, `CONTRIBUTING.md`; `tools/`, `deploy/dev/` | – | 1 | Fresh clone: `mise install && task setup` succeed; `task lint:go lint:pins test:go` pass on the empty module; `task --list` shows every task of 06 §7.2 (tasks whose inputs do not exist yet exit 0 with a note); `.bin/tygo` is v0.2.21; `task build:go` stops with a clear message while `web/dist/index.html` is missing |
| S02 | 1 | CI core | 06 S2; 06 §8.1–8.2, §8.6 | `.github/workflows/ci.yml`, `.github/dependabot.yml` | – | 1.5 | `actionlint` clean; once group 1 is merged, a PR with a gofmt error turns `ci-ok` red and a clean PR is green in < 12 min; jobs whose inputs are missing are skipped and count as success. Owner: 06 §8.6 steps 1–3 and 6–7 |
| S03 | 1 | Protocol types and fixtures | 01 P1; 01 §5, §8, §14.3, §15.1 | `internal/protocol` | – | 3 | `go test -race ./internal/protocol/...`: round-trip, registry coverage, no-null, unknown-field tolerance, validation tables, codec table and secret redaction pass; each fuzz target runs 30 s clean; `go list -deps` shows only the standard library |
| S04 | 1 | REST and ops DTOs, error table | 03 §12.2, §12.4, §16; 04 §2, §9.4, §18 | `internal/protocol/api` | – | 1 | Golden JSON round-trip for every DTO; every error code has a status in `api.StatusOf`; a reflection test finds only camelCase JSON names; standard library imports only |
| S05 | 1 | CLI skeleton, version, logx | 04 S1; 04 §3, §10, §15 | `cmd/isshoni`, `internal/version`, `internal/logx` | – | 1.5 | A build with `-ldflags -X…` makes `isshoni version --json` show the injected values and `--short` print the version; logx tests (Secret redacted for every verb, ReplaceAttr keys, `auto` format); testscript: usage text and exit 2 on a bad flag |
| S06 | 1 | SFU codec and H.264 helpers | 02 slice 1; 02 §8.1–8.2 | `internal/server/sfu` | – | 2.5 | `codec_test`, `h264_test`, `sdpcheck_test` pass; each fuzzer runs 60 s clean; golden SDP fragments match 02 §8.1 |
| S07 | 1 | Store foundation | 03 slice 1; 03 §4–5 | `internal/server/store` | – | 2 | Store tests for migrations, pre-migration backups, newer-schema refusal (`ErrNeedsOperator`) and constraints pass; a new DB contains Lounge; the schema dump matches a golden copy of 03 §5 |
| S08 | 1 | Auth primitives and CSRF wrapper | 03 slices 3 (origin.go) and 4; 03 §3.2, §7.1–7.3, §7.5 | `internal/server/auth` | – | 1.5 | Unit tests for keyed hashes, PRECIS usernames, the common-password list, argon2 (PHC, semaphore, dummy hash), the limiter (burst, refill, one key per IPv6 /64) and the CSRF wrapper; fuzz targets run 30 s clean |
| S09 | 1 | Web scaffold, build and embed | 05 W1; 05 §2–3, §16.5 (first rule), §17 | `web/` root files (package.json, lockfile, .npmrc, index.html, vite/ts/eslint/prettier configs), `web/build/`, `web/public/`, `web/scripts/{check-size,check-i18n}.mjs`, `web/src/{main.tsx,i18n/,types/}`, `web/src/test/setup.ts`, `web/src/ui/{tokens,global}.css`, `web/embed.go`, `web/dist/.gitkeep` | – | 2.5 | `npm ci && npm run lint && npm run typecheck && npm test -- --run && npm run build && npm run check:size && npm run check:i18n` pass (the Vitest setup file `src/test/setup.ts` loads); a JSX literal string fails lint; a `t('…')` literal key missing from `en.json` fails `check:i18n` (the script's only rule so far); `dist/` has `version.json` and `.br`/`.gz` siblings; `go build ./web` works with only `dist/.gitkeep`; the PR notes the confirmed React Router 8 API names |
| S10 | 2 | TS generation | 01 P2; 01 §14.4, §16 | `internal/protocol/gen/tsregistry`, `web/src/protocol/{types,api,registry}.gen.ts`, `web/src/protocol/codecs.ts`, `web/src/protocol/{registry,codecs}.test.ts` | S01, S03, S04, S09 | 1.5 | `task gen` is idempotent; `npm run typecheck` and the registry and codec Vitest files pass; renaming a Go field without regenerating fails `task gen:check` |
| S11 | 2 | Signal hub skeleton | 01 P3; 01 §3, §6, §12–13, §15.2 | `internal/server/signal`, `internal/server/signal/signaltest` | S03, S05 | 3 | Upgrade (Origin matrix, pre-auth limits, 16 connections per user), handshake, rate-limit, slow-consumer and shutdown tests of 01 §19 pass; every `Deps` interface is declared in full with fakes in `signaltest`; goleak clean |
| S12 | 2 | Munger and packet cache | 02 slices 2–3; 02 §9.2, §9.4 | `internal/server/sfu` | S06 | 2.5 | `munger_test` (with the S4 ports) and `packetcache_test` pass under `-race`; `FuzzMunger` runs 60 s clean; munger coverage ≥ 90%; insert+get benchmark < 100 ns |
| S13 | 2 | Fake media, Go publisher, Go viewer | 02 slice 4; 02 §15.1 | `internal/media/fake`, `internal/client/publish`, `internal/server/sfu/sfutest` | S06 | 2.5 | Pion-to-Pion loopback without the SFU: the viewer sees the configured bitrate ±5%, markers intact, an SR every 1 s, and a keyframe within one frame of a PLI |
| S14 | 2 | Store queries and pruning | 03 slice 2; 03 §6 | `internal/server/store` | S04, S07 | 2 | The remaining store unit tests pass, including concurrent `UseInvite` and prune boundaries |
| S15 | 2 | Config | 04 S2; 04 §4 | `internal/server/config`, `cmd/isshoni` | S05 | 2.5 | Config tests of 04 §17 pass; every M1 key (04 §4.3, incl. 01's guard, `sfu.pause_unwatched_layers` and the policy keys) is registered in `keys.go`; `config example` round-trips; `config init` refuses to overwrite (exit 7); errors show file:line and a fix |
| S16 | 2 | ICE transports and public IP | 04 S5; 04 §7.1, §7.3–7.6 | `internal/server/netx` | S04, S05 | 3 | netx unit tests pass; a Pion PeerConnection pair connects through `Transport` over UDP and over TCP 7882 on loopback; `TransferCounter` counts both |
| S17 | 2 | HTTP router, middleware, JSON helpers, SPA handler | 04 S4 (router); 04 §9 | `internal/server/httpapi` | S04, S05 | 2 | Router tests: middleware order of 04 §9.3, `ClientIP` with trusted proxies, `WriteError` emits 03's envelope, security headers of 04 §9.6, SPA fallback and caching (hashed assets immutable, `index.html` no-cache, `.br`/`.gz` served) |
| S18 | 2 | License gate and notices | 06 S3; 06 §8.3–8.4 | `tools/notices/`, `web/scripts/licenses.mjs`, `web/licenses.overrides.json`, `web/package.json` (build script), `deploy/notices/`, `.github/workflows/ci.yml` | S01, S02, S09 | 1.5 | A GPL-3.0 fixture dependency fails `task licenses`; `THIRD_PARTY_NOTICES` lists every module in `go version -m bin/isshoni`, the Go runtime and every prod npm package; `npm run build` writes `dist/licenses.txt` |
| S19 | 3 | Rooms and participants | 01 P4; 01 §4, §8.4–8.6, §8.12 | `internal/server/signal`, `internal/server/signal/signaltest` | S11 | 2.5 | Room, notify and revocation tests of 01 §19 pass: coalesced byte-identical `room.state` (at most one per 200 ms), `room.event`, `Notify`, `CloseRoom`, `UpdateUser`, `CloseConnections` |
| S20 | 3 | TS SignalClient | 01 P10; 01 §10, §16 | `web/src/protocol/{signal-client,errors,index}.ts`, `web/src/protocol/signal-client.test.ts`, `web/src/protocol/testing/` | S10 | 2.5 | Vitest cases of 01 §19 pass with the fake socket in `web/src/protocol/testing/` (later web slices reuse it): handshake, backoff, resume, `retryNow`, `staleBuild`, `probe`, `info.shutdown`, unknown error codes handled by scope and `retryable` |
| S21 | 3 | SFU on netx transports | 02 slice 5; 02 §7.1–7.3 | `internal/server/sfu` | S12, S16 | 1.5 | `api_test` on a loopback `netx.Transport`: UDP 7882 and TCP 7882 candidates, no trickle, the remote-candidate filter table (the TCP 443 case lands in S29) |
| S22 | 3 | Decodable fake media | 02 slice 15 (fake); 02 §15.1 | `internal/media/fake` | S13 | 1.5 | `fake` tests: SPS/PPS and slice headers parse with the SFU parser; macroblock count and I_PCM alignment checked by a bit reader; the Opus asset packets are valid |
| S23 | 3 | Settings cache | 03 slice 5 (cache); 03 §9 | `internal/server/store` | S14 | 0.5 | `SettingsCache` tests: typed validation, `Pin` locks a field, `OnChange` fires once per change, defaults |
| S24 | 3 | REST API chain and /info | 03 slice 3 (HTTP); 03 §7.5, §12.1–12.3 | `internal/server/httpapi` | S04, S08, S17 | 1 | CSRF and Content-Type matrix tests pass; `GET /api/v1/info` returns the 03 §12.4.1 shape with fake sources; every response has `no-store`; unknown `/api/v1/` paths get JSON 404/405; `httpapi.Signal`, `Push` and `InfoSource` are declared in full |
| S25 | 3 | Data dir and secrets | 04 S3; 04 §5.1–5.2 | `internal/server/config` | S15 | 1.5 | Secrets tests (create, load, permissions, rotate); the container data-volume check exits 78 with a fake mountinfo and passes with `ISSHONI_ALLOW_EPHEMERAL_DATA=1` |
| S26 | 3 | 443 multiplexer | 04 S6; 04 §7.2 | `internal/server/netx` | S16 | 2 | portmux tests pass (TLS, RFC 4571, plain-HTTP hint, garbage, slow client, per-IP limits, `Close` unblocks both `Accept`s); goleak clean |
| S27 | 3 | Web app shell: platform, REST, boot | 05 W2; 05 §4–6, §8, §16.5 (error-code rule), §19.1 (i18n) | `web/src/{platform,app,lib,ui,test}/`, `web/src/protocol/{rest,queryKeys,invalidate}.ts`, `web/scripts/check-i18n.mjs` | S09, S10 | 3 | Unit tests for `ApiError` (both envelopes) and the topic → query-key map; MSW tests for each boot branch (Offline, NeedsHttps, NotSetUp, Unsupported, Fatal); `role` is `viewer` with a mobile UA; `router.tsx` has every 05 §5 route, each lazily loading its folder; the whole `Platform` interface is declared, with stub `displayMedia.ts`, `pwa.ts` and `push.ts` and a stub `UpdatePill` mounted in the shell (S35, S38 and S77 fill them in); every `ErrorCode` and api `Code…` constant has an `errors.<code>` key in `en.json`, `check:i18n` fails when one is missing, and the i18n unit test of 05 §19.1 passes |
| S28 | 4 | Resume and grace | 01 P5; 01 §10.1–10.3, §10.5 | `internal/server/signal`, `internal/server/signal/signaltest` | S19 | 2 | Resume tests of 01 §19 pass, including no leave/join events within the 30 s grace, `replaced` on a second socket, the deliberate-close fast path and resume-key rotation |
| S29 | 4 | SFU core: model, Conn actor, PCs | 02 slice 6 (part); 02 §5–6 | `internal/server/sfu`, `internal/server/sfu/sfutest` | S13, S21, S26 | 3 | The whole 02 §6 API exists (methods of later slices return a not-implemented error); through `sfutest.Harness` a publisher and a viewer negotiate pub and sub PCs with `gen`/`neg` and `tracks`; a stale offer → `sfu.stale_offer`; the sub offer has a passive TCP 443 candidate via `netx.ListenPortMux` on 127.0.0.1:0; goleak clean |
| S30 | 4 | Sessions, login and setup | 03 slices 6–7; 03 §7.4, §7.7 (logout rows), §7.8 | `internal/server/auth`, `internal/server/httpapi` | S14, S24 | 2 | Login, rotation and throttle integration tests pass (a hasher counter proves no hashing while throttled); logout and the login-with-an-old-cookie row call `ConnCloser` with that session and the 03 §7.7 reason (fake `ConnCloser`); setup tests pass, including the parallel-complete race |
| S31 | 4 | `serve` in off mode, health, servertest | 04 S4 (server); 04 §6.1–6.4, §11.1 | `internal/server`, `internal/server/servertest`, `internal/server/ops` (health.go) | S09, S17, S25 | 2 | `servertest.Start` is ready in < 1 s; `/healthz` and `/readyz` follow 04 §11.1; a test `fs.FS` SPA and its fallback are served; with only `dist/.gitkeep` the "web app is not built" page shows; `Shutdown` returns in time |
| S32 | 4 | Web Push sender | 04 S12; 04 §14 | `internal/server/push` | S04, S25 | 2.5 | push tests pass: the payload decrypts at a fake push service, SSRF guard table (private addresses, redirects), dedup and rate limits, pruning on 404/410, VAPID fingerprint check |
| S33 | 4 | Auth pages and setup step 1 | 05 W3, W10 (step 1); 05 §14.1 (step 1), §15.1; 03 §7.8 | `web/src/auth/`, `web/src/setup/SetupPage*` | S27 | 2.5 | Component tests (MSW) for each error code on each form; the fragment token is removed before the first request (fetch spy); a logout in tab A redirects tab B; `SetupPage` reads the `/setup` fragment token, calls `setup/check`, and its create-admin form calls `setup/complete`, then navigates to `/` (S87 switches this to `/admin/welcome?step=2`) |
| S34 | 4 | Signaling and room session controller | 05 W4, W5 (controller); 05 §7, §11.1 | `web/src/rooms/` | S20, S27 | 2 | Vitest with a fake socket: the 05 §7.1 states ("Reconnecting…" after 2 s, recovery), stale-build reload exactly once, `RoomSession` join/leave/resync, `room.event` announcements |
| S35 | 4 | Sharer capture and picker | 05 W6 (capture); 05 §13.1–13.3, §13.8 | `web/src/share/`, `web/src/platform/browser/{displayMedia,classify,fakeDisplay}.ts` (`displayMedia.ts` replaces S27's stub) | S27 | 2.5 | Unit tests for `classify` and the `pick()` fallbacks; component tests for ShareSheet and ScreenAudioWarning ("Share without sound", "Pick something else"); the fake-display seam works only on localhost |
| S36 | 4 | Viewer core: subscribe and stage | 05 W7 (core); 05 §9, §10.1–10.5, §12.1 | `web/src/viewer/` | S20, S27 | 3 | Unit tests: `SubscriberPC` maps by `tracks`, adds `stereo=1`, handles `pc.close`, sends `pc.restart{ice}` after 3 s disconnected and `{rebuild}` on failed; component tests for Stage, Tile, ViewerLayout |
| S37 | 4 | Connection-test client | 05 W10 (client); 05 §14.2, §16.5 (provider and NAT rule) | `web/src/conntest/`, `web/scripts/check-i18n.mjs` | S27 | 2 | Unit tests (fake RTCPeerConnection) for the three probes, `ProbeVerdict` and `ConnTestResult`; every provider id of 04 §13.3 has fix text; `codes.json` lists the `ct-` codes; `en.json` has every `fix.firewall.<provider>` and `conntest.nat.<nat>` text, and `check:i18n` fails when a `CloudProvider` or `NATKind` constant has none |
| S38 | 4 | PWA shell: service worker, manifest, update pill | 05 W11 (PWA); 05 §16.1–16.2, §16.4 | `web/src/sw/`, `web/build/sw-plugin.ts`, `web/public/{manifest.webmanifest,icons/}`, `web/src/app/UpdatePill.tsx`, `web/src/platform/browser/pwa.ts` (both replace S27's stubs) | S27 | 2.5 | SW unit tests (`routes.ts` fetch strategy, versioned shell list); the build emits root `sw.js` and `manifest.webmanifest`; the update pill appears when `version.json` changes |
| S39 | 5 | Go signaling client | 01 P6; 01 §10.1–10.2, §15.3 | `internal/client/signal` | S28 | 2 | Against the in-process hub: resume after a killed socket; backoff within its bounds; `Close` makes the server skip the grace period; goleak clean |
| S40 | 5 | Shares and negotiation plumbing (fake media) | 01 P7; 01 §4.4, §8.7–8.11 | `internal/server/signal`, `internal/server/signal/signaltest` | S28 | 3 | Share and role-matrix tests of 01 §19 pass against the fake `MediaPlane`: limits, `ref`, `replaces`, both 30 s share timeouts, `pc.*` routing and ownership, `subscribe.update`/status, `quality.hint`, `caps.update`, stats forwarding, one `ShareStarted` push call per live share |
| S41 | 5 | SFU media path, one layer | 02 slice 6 (part); 02 §9.1, §9.3, §9.7 | `internal/server/sfu`, `internal/server/sfu/sfutest` | S29 | 2.5 | Integration 1 (`TestBasicForwarding`): the first video packet is an SPS, audio flows, the share goes pending → live; `Close` returns within 1 s and leaves no goroutines |
| S42 | 5 | Settings endpoints, invites, registration, approvals | 03 slices 5 (REST) and 8; 03 §7.9, §9 | `internal/server/auth`, `internal/server/httpapi` | S23, S30 | 2 | `GET/PATCH /admin/settings` tests (pinned fields refused); invite, approval and closed-mode integration tests; `signup_pending` reaches a fake alerter |
| S43 | 5 | Admin socket and CLI basics | 04 S8; 04 §12.1–12.2, §12.5 | `internal/server/ops`, `cmd/isshoni` | S31 | 2.5 | testscript tests against an in-process socket with fake auth: `healthcheck` exits 0/1 only, `setup-url --json/--qr/--wait` and exit 7 after setup, `users list --json`, `invite create`, `log-level --for`; peer-credential checks pass on macOS and Linux |
| S44 | 5 | TLS manager and TLS modes | 04 S7; 04 §8 | `internal/server/tlsmgr`, `internal/server`, `internal/server/servertest`, `.github/workflows/ci.yml` | S26, S31 | 3 | A Pebble + challtestsrv CI job issues a domain and an IP certificate; in `servertest` with TLS, WSS (to a stub `/ws` handler; the hub is wired in S54), HTTP/2 and ICE-TCP (a raw RFC 4571 STUN frame; media over 443 is tested in S59) share one port; manual-mode reload tests pass. Manual: a Let's Encrypt staging IP certificate on a VPS |
| S45 | 5 | Room page and shell | 05 W5 (UI); 05 §11.2 | `web/src/rooms/` | S34 | 2 | Component tests: header, people panel, room switcher only when `showRoomList`, `InRoomBar`, root redirect to `defaultRoomId` or `isshoni.lastRoomId` |
| S46 | 5 | Publisher PC and share.start | 05 W6 (publish); 05 §13.4–13.7 | `web/src/share/` | S20, S35 | 2.5 | Unit tests with a fake RTCPeerConnection: `share.start` then a pub offer with `tracks` and `gen`/`neg`; encodings from `ShareParams` with `scaleResolutionDownBy = max(1, sqrt(w·h/maxPixels))`; codec order; presets; stop and browser-stop send `share.stop` |
| S47 | 5 | Viewer focus, audio, watchers, stats core | 05 W7 (rest); 05 §10.3, §10.7, §12.2–12.3 | `web/src/viewer/`, `web/src/lib/stats/` | S36 | 2.5 | Unit tests: auto-focus of the newest share and `replaces` carry-over, audio only for the focused share, batched `subscribe.update`, TapToStart, watchers popover; `window.__isshoni.stats()` returns per-tile stats |
| S48 | 5 | Account pages | 05 W11 (account); 05 §15.2 | `web/src/account/` | S33 | 1.5 | Component tests (MSW): session list and revoke, revoke others, password change, delete account, devices list (empty in M1) |
| S49 | 5 | Admin pages: users, approvals, invites, rooms, settings, audit | 05 W12 (part); 05 §15.3 | `web/src/admin/` | S33 | 2.5 | Component tests (MSW) for users, approvals, invites, rooms, settings (pinned fields read-only) and audit paging |
| S50 | 6 | sfuplane adapter | 01 P8 (adapter); 01 §15.4 | `internal/server/sfuplane` | S29, S40 | 1.5 | Table tests for every mapping of 01 §15.4 (errors, events, reasons, codec keys) pass against the real `*sfu.SFU` API |
| S51 | 6 | Same-user relay (cuttable) | 01 P12; 01 §8.14 | `internal/server/signal` | S40 | 1 | Two connections of one user exchange messages; another user as target → `agent_target_not_found`; a 17 KiB payload → `message_too_large`; 11 messages/s → `rate_limited` |
| S52 | 6 | Simulcast and layer selection | 02 slice 7; 02 §8.6, §10.1 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S41 | 3 | Integration 2, 3, 10 and 17 (10×10, outside `-short`) and `throttle_test` pass |
| S53 | 6 | Rooms and push subscriptions | 03 slices 11–12; 03 §8, §12.4.6 | `internal/server/httpapi` | S30 | 1.5 | Rooms tests (`showRoomList`, admin CRUD, Signal hooks, presence counts) and push tests with a fake `Push` (upsert, validation, test send, preferences round-trip, `ListPushSubscriptions(Pref)`) pass |
| S54 | 6 | Wiring v1: hub, store, auth, REST | 04 W1, 03 slice 14; 04 §6.6 | `internal/server` | S23, S28, S30, S31, S43 | 2 | Adapter table tests pass; in `servertest` (off mode): log in over REST, `/ws` joins Lounge, `POST /api/v1/auth/logout` closes that session's socket (`session_revoked` on the wire) in < 100 ms; `isshoni setup-url --json` returns a token that completes setup |
| S55 | 6 | Ops data: metrics, transfer, release check | 04 S10; 04 §11.2–11.3, §11.5 | `internal/server/ops` | S16, S43 | 2 | Metrics scrape test on 127.0.0.1; month rollover and alert-once tests under a fake clock; release-feed parser tests |
| S56 | 6 | Layers, fullscreen, keyboard, mobile | 05 W8; 05 §12.4–12.8 | `web/src/viewer/` | S47 | 2.5 | `layerPolicy` tests (focused high, thumbnails low, off-screen and hidden-tab video off after 10 s); keyboard map tests; fullscreen, PiP and wake lock feature-detected (PiP hidden on iOS) |
| S57 | 7 | Resilience and guards | 02 slice 9; 02 §6.5, §12 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S52 | 3 | Integration 7, 8, 9, 11, 14 and 15 pass |
| S58 | 7 | Self-service and revocation | 03 slice 9; 03 §7.7 | `internal/server/auth`, `internal/server/httpapi` | S30 | 1.5 | The revocation matrix passes with a fake `ConnCloser`: every M1 row of 03 §7.7, including logout-everywhere, closes the right connections. Optional: an in-process logout-everywhere check in an external test package (`httpapi_test` with `servertest`), which only imports `internal/server` and does not change it |
| S59 | 7 | Wiring v2: SFU in `serve`, itest | 04 W2, 01 P8 (harness); 01 §19 cases 1–3, 9 | `internal/server`, `internal/server/itest` | S13, S39, S44, S50, S52, S54 | 2.5 | In `internal/server/itest` against `servertest` with the real SFU, cases 1–3 and 9 of 01 §19 pass; ICE-TCP via 443 (04 §17): with `servertest.Options{TLS: true}`, `listen.ice_udp = ""` and `listen.ice_tcp = ""`, a tcp4-only Go publisher shares, an `sfutest.Viewer` decodes its media, and WSS signaling runs on the same port; both Go clients' selected candidate pairs are TCP with the TLS listener's port as the remote port; shutdown order is hub, SFU, Transport; goleak clean |
| S60 | 7 | doctor | 04 S11; 04 §13 | `internal/server/ops/doctor`, `internal/server/ops`, `cmd/isshoni` | S43, S44, S55 | 3 | doctor tests pass (provider table, bandwidth numbers of 04 §13.4); `--list-checks` prints the ids of 04 §13.2; exit 0/5; the typed doctor and bandwidth functions return the JSON of 04 §13.5. Manual: `doctor --json` all ok on a VPS |
| S61 | 7 | e2e harness and CI job | 06 S4; 05 §19.3; 06 §8.2 (e2e job) | `web/playwright.config.ts`, `web/e2e/{global-setup,fixtures,stats}.ts`, `web/e2e/smoke.spec.ts`, `.github/workflows/ci.yml` | S02, S33, S54 | 1.5 | `task e2e` locally and the CI `e2e` job run `smoke.spec` green (global setup starts `ISSHONI_BIN` with `deploy/dev/isshoni.e2e.toml`, completes setup through `setup-url --json`, logs in); a forced failure uploads the trace, video and `server.log` |
| S62 | 7 | systemd unit, sysctl, firewall, packaging files | 06 S5; 06 §4.5–4.7, §5 | `deploy/systemd/`, `deploy/sysctl/`, `deploy/firewall/`, `deploy/packaging/` | S44, S54 | 1 | `systemd-analyze verify` passes in the existing `lint-deploy` job. Manual: in a Debian systemd container or VM (podman is fine), a hand-placed binary starts as `isshoni`, binds 443 with a test cert, `CapEff` 0x400; the `systemd-analyze security` score is recorded. The automated start check is C1 (S67 locally, S78 in CI) and nightly V1; S62 adds no script and no CI change |
| S63 | 8 | Repair and sync | 02 slice 8; 02 §9.4–9.6 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S57 | 2 | Integration 4, 5 (flash-to-beep offset ≤ 5 ms) and 6, and `rtx_test` pass |
| S64 | 8 | Admin users and password reset | 03 slice 10; 03 §7.10–7.11 | `internal/server/auth`, `internal/server/httpapi` | S58 | 1.5 | Roles and reset integration tests: last-admin and self rules, rename/role/status with the password requirement, reset link issue and complete, admin alerts |
| S65 | 8 | Backup, restore, secret rotation | 04 S9; 04 §5.3, §12.3–12.4, §12.6 | `internal/server/ops`, `cmd/isshoni`, `internal/server` | S14, S43, S54 | 2.5 | Backup/restore round-trip and crash-recovery tests; a newer-schema DB makes `serve` exit 78 and an offline restore of the pre-migration DB brings it back; `rotate-secrets` restarts and invalidates exactly the rotated tokens |
| S66 | 8 | Browser e2e: invite, presence, share, watch | 05 W5–W7 e2e, 01 P11; 05 §19.3 | `web/e2e/{invite,rooms,warning,watch}.spec.ts`, `web/e2e/tone.html`; fixes in `web/src/{rooms,share,viewer}/` | S22, S33, S42, S45, S46, S47, S53, S59, S61 | 2.5 | In CI on Chrome: `invite.spec`, `rooms.spec` (two browsers see each other within 1 s; closing a tab removes presence at once; cutting the network keeps it through the grace period), `warning.spec` and `watch.spec` pass; tone-tab capture works under xvfb |
| S67 | 8 | install.sh core | 06 S6; 06 §4.1–4.10, §4.12, §11.1 | `deploy/install.sh`, `deploy/test/`, `tools/relserve/` | S15, S43, S60, S62 | 3 | install.sh unit tests pass under dash, `bash --posix` and busybox; scenarios C1 and C6 pass in a Debian 12 container against a local test release from `tools/relserve` |
| S68 | 8 | Docker image and compose | 06 S8; 06 §6, §11.3 | `deploy/docker/`, `deploy/compose.yaml`, `deploy/compose.host.yaml`, `.github/workflows/ci.yml` | S25, S43, S54 | 1.5 | `task docker:smoke` (06 §11.3) passes on amd64 in CI and on arm64 on the Mac; a start without a data volume exits 78 with the fix text |
| S69 | 9 | Codec policy and Firefox | 02 slice 10; 02 §8.3–8.5 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S63 | 2.5 | Integration 12 and 13, `policy_test` and `sdpcheck_test` pass, including the 20 s × 9 rebuild guard |
| S70 | 9 | Audit, janitor, dashboard accounts | 03 slice 13; 03 §4.7, §10 | `internal/server/auth`, `internal/server/httpapi` | S42, S53, S64 | 1 | The audit coverage test (every M1 action), the `accounts` golden JSON and the prune schedule under a fake clock pass |
| S71 | 9 | Push wiring | 04 S12b; 04 §6.6, §14.3 | `internal/server` | S32, S42, S53, S59 | 1 | In `servertest`: a share going live reaches the fake push service for an absent user only; `signup_pending` reaches admins; with `push.enabled = false` REST answers `push_unavailable` |
| S72 | 9 | Browser e2e: focus, unmute, layers | 05 W7–W8 e2e, 01 P11; 05 §19.3 | `web/e2e/{focus-audio,unmute,layers}.spec.ts`; fixes in `web/src/{viewer,rooms}/` | S56, S63, S66 | 2 | `focus-audio.spec`, `unmute.spec` and `layers.spec` pass in CI; the A/V offset of the tone/flash check is ≤ 45 ms. Manual: M-IOS-1 and M-AND-1 viewing |
| S73 | 9 | goreleaser and release pipeline | 06 S9; 06 §9 | `.goreleaser.yaml`, `.github/workflows/release.yml`, `deploy/keys/`, `.github/workflows/ci.yml` | S18, S62, S67, S68 | 2.5 | `release.yml` has no `site` job yet (S82 adds it); `goreleaser-check` asserts the asset list of 06 §3; the release dry run passes; tag `v0.1.0-rc.1` → draft → owner approval → signed → verified → published without moving `:latest` or the latest release. Manual: `install.sh` from that rc on a VPS |
| S74 | 10 | Recovery end to end (Go) | 01 P9; 01 §10.4–10.6, §19 cases 4–8 | `internal/server/itest`, `internal/server/signal`, `internal/server/sfuplane` | S57, S59, S69 | 2.5 | Cases 4–8 of 01 §19 pass in `internal/server/itest`: socket killed mid-share, UDP black-holed 5 s, server restart with `replaces`, codec-blocked viewer, pub PC rebuild; goleak clean |
| S75 | 10 | Protocol compat snapshot | 01 P13; 01 §14.3 | `internal/protocol`, `.github/workflows/release.yml` | S73 | 0.5 | A fake release copies the fixtures into `testdata/compat/<version>/` and keeps the last two; removing a field from a type fails the compat test |
| S76 | 10 | Connection-test probe | 02 slice 14; 02 §7.6 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S69 | 1.5 | Integration 18: per-transport answers, echo channel, one probe per user and transport, `sfu.probe_limit` on the 21st, `sfu.transport_disabled` without a mux |
| S77 | 10 | Web Push flow | 05 W11 (push); 05 §16.3 | `web/src/account/`, `web/src/platform/browser/push.ts` (replaces S27's stub), `web/src/sw/push.ts`, `web/e2e/pwa.spec.ts` | S38, S48, S61, S71 | 2 | `pwa.spec` passes (manifest parses, SW activates, offline navigation shows the Offline screen); SW tests render every push type from `en.json` and never stay silent. Manual: M-IOS-2 and M-AND-1 push |
| S78 | 10 | install.sh lifecycle and distro matrix | 06 S7; 06 §4.11, §4.13, §11.2 | `deploy/install.sh`, `deploy/test/`, `.github/workflows/ci.yml` | S65, S67, S73 | 2.5 | Scenarios C1–C12 pass on all six container distros in CI (upgrade, repair, downgrade, uninstall, purge, firewall offer, port check) |
| S79 | 11 | SFU observability | 02 slice 12; 02 §13 | `internal/server/sfu` | S76 | 1.5 | After integration 2 a test reads every metric of 02 §13 (non-zero where expected; metrics of later slices are registered now); a log-capture test finds no SDP or IPs at info; `Snapshot` golden test |
| S80 | 11 | Connection-test endpoint | 04 S13 (server); 04 §7.7 | `internal/server/ops`, `internal/server` | S43, S59, S76 | 1 | In `servertest` (off mode) `POST /api/v1/conntest` returns answers that connect for `udp` and `tcp7882` and `transport_disabled` for `tcp443`; a second probe of the same user and transport replaces the first |
| S81 | 11 | Web recovery: resync, re-publish, capture hold | 05 W9 (part), 01 P11; 05 §9; 01 §10.4–10.6 | `web/src/rooms/`, `web/src/share/`, `web/e2e/reconnect.spec.ts` | S66, S74 | 2.5 | `reconnect.spec`: (a) a dropped socket resumes with the same connection id and no new sub offer; (b) 5 s offline shows the banner after 2 s, then clears; (c) after a server restart the viewer decodes the re-published share within 15 s, focused, with audio |
| S82 | 11 | Project site | 06 S10; 06 §10 | `docs/` site pages (not `PLAN.md`, `m1/`), `docs/package.json`, `.github/workflows/{site,ci,release}.yml` | S37, S60, S73 | 2.5 | `vitepress build` passes; `release.yml` gains the `site` job (`uses: ./.github/workflows/site.yml`) and `actionlint` stays clean; anchor tests cover every doctor id, `ct-` code and provider id; `site.yml` checks that the served `install.sh` equals the latest release asset (sha256); no third-party requests |
| S83 | 11 | Nightly and VM tests | 06 S11; 06 §8.5 | `.github/workflows/nightly.yml` | S68, S73, S78 | 2 | One green nightly with every row: VM matrix with Pebble, multi-arch smoke, cross-OS Go tests, govulncheck, goreleaser snapshot, link check (KVM rows may be skipped with the reason and run once by hand in a local VM) |
| S84 | 12 | Downlink adaptation | 02 slice 11; 02 §9.8, §10.2 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S79 | 2.5 | `allocator_test`, `pacer_test` and integration 16 pass |
| S85 | 12 | Dashboard, metrics and ops routes wiring | 04 S10b; 04 §6.6, §11.2, §11.4 | `internal/server/ops`, `internal/server`, `internal/server/itest` | S55, S59, S60, S70, S79, S80 | 2 | Golden dashboard JSON with fake sources; a failing source does not fail the response; `/metrics` has the SFU and hub series of 06 §12.3; in S59's ICE-TCP-via-443 test, `isshoni_sfu_selected_transport{transport="tcp443"}` counts both PCs; dashboard, doctor and bandwidth routes answer 403 to non-admins |
| S86 | 12 | Codec wait and quality hints | 05 W9 (part); 05 §10.6, §13.6; 01 §11.7 | `web/src/viewer/`, `web/src/share/`, `web/e2e/hint.spec.ts` | S69, S72, S81 | 2 | Vitest: without H.264 the client polls caps and sends only `caps.update`, never `pc.restart`; `quality.hint` encodings set `active`/`maxBitrate`; `hint.spec`: a `quality.hint{codec}` makes the sharer re-offer and viewers keep decoding. Manual: M-FF-1, M-WIFI |
| S87 | 12 | Setup wizard steps 2–3 | 05 W10 (wizard steps 2–3), 04 S13 (UI); 05 §14.1 | `web/src/setup/`, `web/e2e/setup.spec.ts` | S33, S37, S42, S61, S80 | 2.5 | `WelcomePage` steps 2–3; S33's `SetupPage` now navigates to `/admin/welcome?step=2`; `setup.spec`: admin created; UDP ✓ and TCP 7882 ✓ with the TCP 443 row hidden (off mode); invite link and QR; `setupWizardDone` set; with `listen.ice_udp = ""` the UDP row shows ✗ with the "UDP turned off" text; a non-admin sees no fix text. Manual: a VPS with UDP 7882 blocked shows that provider's text |
| S88 | 13 | Uplink control and layer pausing (cuttable) | 02 slice 13; 02 §11 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S84 | 1.5 | With `MaxShareKbps` set the pub PC gets a REMB every 1 s; an unwatched layer gets a pause hint after 10 s and resumes with a PLI; `sfu.pause_unwatched_layers = false` turns it off |
| S89 | 13 | Load test tool | 02 slice 15 (tool); 02 §15.2 | `cmd/isshoni-loadtest`, `web/e2e/decodable.spec.ts` (not `sfutest` or `publish`, which S88 owns in this group; M1 has no publisher rate control) | S22, S39, S42, S59, S61, S64, S85 | 2.5 | A Go test runs `-scenario smoke` against an in-process server and exits 0 with the JSON report (so it runs in `test-go`); users come from an invite and are deleted afterwards; an unreachable server gives the documented exit code; `decodable.spec` runs `isshoni-loadtest -scenario smoke -decodable -publishers 1 -subscribers 0` (or a `-publish-only` flag) against the e2e server fixture, and a Chrome viewer shows `framesDecoded > 0` |
| S90 | 13 | Server hardening | 04 S14; 04 §6.4–6.5, §17 | `internal/server`, `cmd/isshoni`, `web/e2e/tcp-only.spec.ts` | S65, S66, S68, S71, S85 | 2 | SIGHUP reload test; shutdown test (`server.shutdown`, close 1012, REST 503 `server_shutdown`, `Run` returns in time); a log canary finds no secrets, SDP or client IPs at info; `tcp-only.spec` (UDP blocked, off mode, so TCP 7882) plays over TCP; S59's ICE-TCP-via-443 itest (04 §17) still passes with the hardened server; doctor reports ok in the Docker bridge and host setups |
| S91 | 13 | Admin dashboard, doctor, bandwidth; admin e2e | 05 W12 (part); 05 §15.3 | `web/src/admin/`, `web/e2e/admin.spec.ts` | S49, S61, S64, S70, S85 | 2 | `admin.spec` (create and revoke an invite, approve a pending sign-up, doctor page renders); the dashboard polls every 2 s while visible and stops when hidden |
| S92 | 14 | Load gate on a VPS | 02 slice 16; 02 §15.2 | `internal/server/sfu` | S88, S89 | 2 | Manual (VPS): `-scenario focus` against a 4 vCPU VPS, run from a second VM, meets every 02 §15.2 criterion; the `all-high` knee goes into the release-notes draft |
| S93 | 14 | Web hardening: overlay, a11y, i18n check, versions | 05 W13; 05 §16.4–16.6 | `web/src/{app,viewer,share}/`, `web/scripts/check-i18n.mjs`, `web/e2e/{a11y,version}.spec.ts` | S72, S77, S86, S87, S91 | 3 | `a11y.spec` and `version.spec` pass; a test shows `check:i18n` fails when a key is removed, and the script warns about unused keys (its earlier rules came in S09, S27 and S37); the debug overlay (`stats.watch`) shows per-tile stats with no IPs; `stats` notifications raise the hub's client metrics. Manual: the upload hint under DevTools throttling |
| S94 | 15 | M1 exit test | 06 S12, 05 W14; 06 §12; 05 §19.4 | `tools/exittest/`, `docs/m1/exit-test/` | S51, S75, S82, S83, S90, S92, S93 | 1.5 | `tools/exittest` tests pass; the 05 §19.4 device matrix is filled in; on an rc every pass criterion of 06 §12.3 is met (5 friends, 2 hours, no manual fixes, iPhone and Android watch); `v0.1.0` is tagged from that commit |

## 6. Questions for the owner

1. **Minimum password length.** The specs use 8 characters plus a common-password blocklist and throttles. NIST
   (SP 800-63B-4) recommends 15 when the password is the only factor, which is safer but slower for friends signing up
   on a phone. Keep 8, or pick 10, 12 or 15? (03 §19; only `CheckPassword` and `/info.accountRules` change.)
2. **Auto-focus after a manual pick.** The plan focuses the newest share automatically. Proposed: once a friend has
   clicked a tile, a new share only shows an "Alex started sharing [Watch]" toast instead of taking over the stage and
   the sound, until the picked share ends. OK, or always jump to the newest share? (05 §24)
3. **What to cut if M1 runs late.** Proposed order: first S51 (same-user relay; no M1 client uses it, M2 needs it),
   then S88 (uplink cap and layer pausing; moves to M5 with no protocol change). A cut slice is removed from every
   Needs list. If S88 is cut, S92 runs without layer pausing. OK?
4. **Test servers.** The load gate (S92) needs a 4 vCPU VPS plus a second VM for about a day; the manual TLS, doctor,
   connection-test and release checks (S44, S60, S87, S73) need a small VPS with a domain, which then hosts the exit
   test. OK to rent them?

### Owner actions (not decisions)
- The one-time GitHub setup of 06 §8.6 (default branch `main`, rulesets, the `release` environment, Pages), before
  group 1 merges.
- Create the release signing key and add it to the `release` environment (06 §9.4) before S73.
- Finish the S4 Firefox and Safari viewer check from M0 before S69 (codec policy) lands.
- Run the manual device matrix (05 §19.4) on iPhone, iPad and Android, and gather four friends for the exit test.
