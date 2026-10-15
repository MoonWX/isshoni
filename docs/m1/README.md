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

**Carried to later milestones**
- `internal/client/publish` imports `internal/media/fake` (for its packet type), which the M2 depguard rule for
  `internal/client` (06 §8.2: only Pion, `x/net` and `internal/protocol`) forbids. Before that rule goes live, move
  the packet type out of `fake` (for example into `publish`) or add a depguard exception.
- The four M2 device-flow methods of 03 §7.13 (`StartDeviceFlow`, `DeviceToken`, `LookupUserCode`,
  `PasswordDeviceLogin`) are not declared in M1, because their `internal/protocol/api` DTOs don't exist yet. 03
  slice 15 (M2) adds the DTOs and the methods together; S24 declares every other method.

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
  `context.Background()` appears only in `main`, in top-level goroutines and in tests. A call whose signature has
  no context bounds its own work instead and says so in its spec: `auth.Service.MaybeRotate` writes under a 5 s
  timeout (03 §7.4).
- Long-lived parts have an explicit life: `New` starts nothing, then `Run(ctx)` or `Start`, then `Close()` or
  `Shutdown(ctx)`. Every goroutine has one owner that waits for it. `SFU.Close` returns within 1 s; the whole server
  stops within 10 s, a forced shutdown within 11 s (04 §6.4; the unit allows 20 s).
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
  xvfb in CI (with a window manager and one worker there; headless locally, 05 §19.3). Each spec file belongs to
  one slice.
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
`Deps`, S24 for `httpapi.Signal`, `Push` and `InfoSource` and for the `auth.Service` surface of 03 §7.13, S29 for the
SFU API, S27 for the web `Platform`. Adapters and fakes then never chase a moving interface.

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
- 94 slices in 15 groups, plus follow-up slices (S00, S00B, S00C, S00D, S00E, S00F, S00G, S00H, S00I, S00J, S00K, S00L,
  S00M: spec write-backs, with small code fixes in S00 and S00B; W00, F02, F03, F04, F05, F10, F11, F12, F13, F14,
  F15, F16, F17, F18, F19, F20, F21 and F22: code or files that a group's integration or review found missing; F06,
  F07, F08 and F09: the fixes of the security review of `main` before the first release candidate, §6 "Decided
  after group 10") and dependency slices (D01) that the integrator adds to a group. A group starts when the previous
  one is merged into `m1/server-web` and green.
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
  S81 → S86 → S93 → S94 → S94-day. Milestones: accounts, REST and signaling in `serve` after group 6 (S54); the SFU
  in `serve` after group 7 (S59); the first browser "watch together" after group 8 (S66); a signed release candidate
  after group 9 (S73).
- Cuttable if M1 runs late: S51 (same-user relay) and S88 (uplink control and layer pausing). See the questions.
  A cut slice is removed from every Needs list. If S88 is cut, S92 runs without layer pausing. S88 was not cut: it
  merged in group 13, with layer pausing on by default (§6 "Decided after group 13" (3)), so S92 runs with it.

### Shared files and their owners

| File | Owner | Rule for every other slice |
|---|---|---|
| `go.mod`, `go.sum` | S01 | A slice may `go get` the modules it needs (versions below) and commit the change. On a conflict the integrator takes the base files, re-runs each slice's `go get`, then `go mod tidy` and `task lint:go` |
| `tools/go.mod` | S01 | pins tygo v0.2.21, go-licenses v2, govulncheck, and Task, golangci-lint and shfmt at the `.tool-versions` versions (`task lint:pins`); our tools go in `tools/<name>/` (S18, S67, S94) |
| `Taskfile.yml` | S01 | S01 writes every task of 06 §7.2 up front (tasks without inputs yet exit 0 with a note). Later slices may only change the body of a task they own (`test:sh`, `deploy:test`, `docker:smoke`, `release:*`, `site:*`) |
| `.golangci.yml`, `.gitignore`, `.tool-versions`, `tygo.yaml` | S01 | integrator only |
| `.github/workflows/ci.yml` | S02 | only the slices that list it (S18, S44, S61, S68, S73, S78, S82), never two in one group |
| `web/package.json`, `web/package-lock.json` | S09 | S09 installs every package of 05 §2 at once; S18 edits the `build` script; new packages go through the integrator |
| `internal/server/config/keys.go` | S15 | integrator only |
| `internal/protocol`, `internal/protocol/api` | S03, S04 | additive changes only, called out in the PR; the integrator re-runs `task gen` |
| `web/src/protocol/*.gen.ts` | S10 (generated) | never edited or merged by hand: after each merge run `task gen` and commit the output |
| `web/src/i18n/en.json`, `web/src/i18n/lazy/<ns>.en.json` | S09; W00 (the lazy files) | each slice adds keys under its own namespaces only, in the file that holds the namespace: `account`, `admin` and `setup` are in `lazy/<ns>.en.json` and load with the page folders that use them, everything else is in `en.json` (05 §16.5). A new namespace that only lazy page folders use gets its own lazy file (`doctor`, README S91's, stayed in `en.json`: §6 "Decided after group 13" (1)). A conflict is resolved by keeping both sides; `check:i18n` must pass (S09 writes `web/scripts/check-i18n.mjs`; S27, S37, W00 and S93 add its later rules), and it names the file when a key of a lazy namespace was added to `en.json` |
| `web/src/app/router.tsx` | S27 | S27 declares every route of 05 §5, each lazily loading its folder; page slices fill only their folder: its pages are named exports of `<folder>/index.ts`, which the router finds with `import.meta.glob` (05 §5 "Page folders") |
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
| `go.uber.org/zap` v1.28.0 and `github.com/pion/logging` v0.2.4 (`logx`'s certmagic and Pion log bridges) | S05 |
| `github.com/caddyserver/certmagic` v0.25.6 (04 §8.2 names v0.25.4 as the floor; brings `mholt/acmez/v3` v3.1.7; zap is already there from S05) | S44 |
| `github.com/SherClockHolmes/webpush-go` | S32 |
| `github.com/prometheus/client_golang` v1.24.1 (the hub's `Deps.Metrics` is a `prometheus.Registerer`) | S11 |
| `github.com/skip2/go-qrcode` | S43 |
| `go.uber.org/goleak`, `github.com/rogpeppe/go-internal` (tests only); `golang.org/x/{sys,mod,time}` | first user |
| `golang.org/x/term` v0.47.0 (BSD-3-Clause; `isshoni-loadtest`'s no-echo password prompt, not linked into `bin/isshoni`) | S89 |

### Slices

"Grp" is the parallel group. "Spec" gives the source slice and sections (for example `02 slice 7` or `05 W6`).

| ID | Grp | Slice | Spec | Touches | Needs | Days | Acceptance |
|---|---|---|---|---|---|---|---|
| S00 | 2 | Group 1 follow-ups: spec amendments, REST time format, Dependabot target | the group 1 integrator notes; §6 "Decided after group 1" | `docs/m1/*.md` (not 03 §5, which S14 edits), `internal/protocol/api`, `.github/dependabot.yml` | – | 0.5 | The amendments are in the specs; `api.WireTime` encodes `2026-09-30T12:00:00.000Z` (three fractional digits, `Z`) like `internal/protocol`, with a round-trip test and a fuzz target; `go test -race ./internal/protocol/...` passes; every Dependabot ecosystem targets `m1/server-web` |
| S00B | 4 | Group 3 follow-ups: spec write-backs, protocol TS tag, netx IPv6 loopback | the group 3 integrator notes | `docs/m1/*.md`, `internal/protocol` (struct tags only, then `task gen`), `internal/server/netx` | – | 0.5 | The write-backs are in the specs (05 §3–§6, §8: S27's contracts; 03 §9: S23's choices; 04 §5.1, §5.2, §20: S25's; 01 §14.4: the `tstype` rule); `types.gen.ts` has no `any` for the registry `Spec.Payload` and `Spec.Result` fields (`tstype:"unknown"`) and `task gen:check` is clean; with `network.include_loopback` and `network.ipv6` both on, `Transport.Advertised` has no TCP entry on `::1`, which Pion never gathers (unit tests, one against a real Pion offer; 04 §7.6); `go test -race ./internal/protocol/... ./internal/server/netx/...` and `npm run typecheck` pass |
| S00C | 5 | Group 4 follow-ups: spec write-backs | the group 4 integrator notes | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 (S29: §5.3 a PC closed by the client's DTLS `close_notify`; §6.1 enums from 1, `CloseRoom` closes the Conns, `StartShare`/`UpdateShare` with the §8.6 numbers; §6.3 `Error.Unwrap`, `ErrNotImplemented`, contract violations as a non-retryable `sfu.internal`; §9.3 no transceiver reuse before S52; §13 `ConnStats`); 03 (S30: §7.4 `MaybeRotate`'s own 5 s context; §7.8 the setup token is checked before the hash; §12.4.2 a login answers 401 for a username no account can have; §7.13 unexpected failures wrap `*api.Error{internal}`); 04 §14 (S32: `Options.Resolver`, `OwnAddrs`, `Metrics`, `ErrDisabled`, `DeriveSubject` and `New`'s subject rule, which is stricter than `config`'s, the result table and `ValidateEndpoint` as built, `push.test` outside the recipient bucket); 05 (S33 §15.1; S34 §7, §7.1, §11.1; S35 §6.1, §13.1, §13.6, §19.1, §19.3 with the Chrome flag finding; S36 §10.1, §12.2; S37 §14.2); 01 §4.2 (a `4429` flood close keeps the 30 s grace; §19 names the test that is still missing); 05 §15.1, §16.3 (S48 writes the in-app browser banner into `web/src/auth/` and shows it on the invite and login pages; S77 shows it on the room page) |
| S00D | 6 | Group 5 follow-ups: spec write-backs and recorded decisions | the group 5 integrator notes; §6 "Decided after group 5" | `docs/m1/*.md` (not 01 §8.5, which F02 edits, nor 05 §5, §16.5 and §17.3, which W00 edits) | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 01 §15.3 (S39: `Dial`'s retry rule, `RetryNow` and `Probe`, the three sentinel errors, `Options.Token`, and `Events` backpressure: a caller must drain `Events()` or the client stops reading its socket); 01 §8.6, §8.7, §8.11, §9 (S40: `pc.close{pub}` ends only the shares a pub offer bound; `bad_request` for a `pc.*` message of the wrong direction; a `share.update` from another connection waits up to 10 s for the publishing one, without blocking the requester; `MediaPeer.Stats` is read for every connection with media; no push without the room's name); 02 §5.4, §9.3, §9.7 (S41: the DTLS-ready gate follows the sub PC's state; a restart after `WriteRTP` returned `(0, nil)` only while nothing has left yet; a pending share requests its own first keyframe; what came early from S63; the `Share.notify` lock exception); 03 §7.3, §7.9, §7.13, §9, §12.5 (S42: `register-ip` before the mode check; the register order as built, the invite note rules, `RejectAll` on an empty queue; `UpdateSettings` and `SettingsChange`; `admin.users` on a registration with an invite); 04 §3.2, §3.3, §12.1, §12.2, §12.5 (S43: the codes that exit 7, the bare link on stdout off a terminal, the 2-module QR quiet zone, 503 `server_shutdown` during a shutdown); 04 §6, §8.2–§8.4, §8.7, §18 (S44: the added `tlsmgr.Options` fields, `Manager.Shutdown`, certmagic v0.25.6 where the spec named v0.25.4, the three manual-mode codes, the narrowed hint rows, `servertest`'s `Roots` and `TLSDomain`); 05 §11.2 (S45: "Invite friends" as a link), §13 (S46), §15.2 (S48), §15.3 (S49). The four decisions of §6 "Decided after group 5" are recorded where the specs discussed them (01 §8.7, §8.10, §23; 03 §7.3, §19; 04 §6.2, §6.3, §7.4, §13.2 and its decisions list; 05 §11.2, §13.5, §24), and this table has rows for W00 and F02 |
| D01 | 4 | Major dependency bumps: ESLint 10 and TypeScript 7 (Dependabot PRs #3, #7, #8) | 05 §2; 06 §8.1 | `web/package.json`, `web/package-lock.json`, `web/eslint.config.*`, `web/tsconfig*.json`, `.github/dependabot.yml` | – | 0.5 | Each major either lands with `npm ci`, lint, typecheck, tests, build and the check scripts green, or is deferred with a Dependabot ignore rule that names the blocking package and its peer range. Outcome: both deferred. ESLint 10 waits for eslint-plugin-jsx-a11y (peer `eslint` up to `^9`); TypeScript 7 waits for typescript-eslint (peer `typescript <6.1.0`) and needs `web/scripts/check-i18n.mjs` ported off the compiler API that typescript 7 no longer exports |
| W00 | 6 | Bundle diet: a lazy share publisher, and texts per lazy page folder | the group 5 integrator notes (initial JS was 194.6 KB of the 200 KB budget); 05 §3, §5, §16.5, §17.3 | `web/src/i18n/` (with `i18n/lazy/`), `web/scripts/check-i18n.mjs`, the `index.ts` of the lazy page folders (`web/src/{account,admin,download,setup}/`), `web/src/platform/browser/displayMedia.ts` and its test, in `web/src/app/`: `router.tsx`, `boot.tsx` and `layouts/RootLayout.tsx` (the room as a lazy page folder, from the review) with `router.test.tsx` and `boot.test.tsx`, and the chunk tests (`chunks.node.test.ts`, `lazyNamespaces.test.tsx`), `docs/m1/05-web-client.md` (§5, §16.5, §17.3 only) | – | 1 | The initial JS has room again under the budget of 05 §17.3 (158.4 KB gzip, from 194.6 KB; 158.9 KB with all of group 6 and its integration): the room is a lazy page folder like the others, and boot asks for the first page's folder just before `GET /api/v1/info`, so the room is not slower for it; `share/`'s publisher is no longer in the initial chunks (`BrowserSharing` and `PublisherPC` load with the first pick of something to share, `BrowserShare` with the room's media chunk), and the texts of the `account`, `admin` and `setup` namespaces are in `i18n/lazy/<ns>.en.json` and load with the chunks of the page folders that use them (the clause that the `conntest` and `fix` namespaces load lazily too was dropped after group 6: the size target is met without it, at 158.9 KB of the 200 KB budget, so `account`, `admin` and `setup` are the only lazy namespaces and `conntest` and `fix` stay in `en.json`; moving them would need files of `web/src/conntest/`, 05 §16.5); `app/chunks.node.test.ts` fails when the publisher, a lazy namespace or a lazy page is in the initial chunks again; `npm run lint`, `typecheck`, `test -- --run`, `build`, `check:size` and `check:i18n` pass; `check:i18n` still fails on a `t('…')` key that no file has, and now also on a lazy namespace's key that could be asked for before its chunk is loaded and on a lazy namespace that is in the main bundle; the PR states the initial JS size before and after |
| F02 | 6 | Admin badge for everyone: `ParticipantInfo.admin` | §6 "Decided after group 5" (1); 01 §8.5; 05 §11.2 | `internal/protocol` (additive: the field and a fixture for it, then `task gen`), `internal/server/signal`, `web/src/protocol/types.gen.ts` (generated), `web/src/rooms/` (the people panel and the room page's lines for it), `docs/m1/01-protocol.md` (§8.5 only) | – | 0.5 | `room.state` has `admin: true` on every participant who is an admin and leaves the field out for members; it is additive (01 §14.1): the fixtures that existed are unchanged, a new one holds the field, and `task gen:check` is clean; a role change reaches the room like a rename (`UpdateUser`, or a `Revalidate` result) with a new snapshot; the people panel shows the admin badge on every admin's row and none for a snapshot without the field; `go test -race ./internal/protocol/... ./internal/server/signal/...` and the people panel's component tests pass |
| S00E | 7 | Group 6 follow-ups: spec write-backs and recorded decisions | the group 6 integrator notes; §6 "Decided after group 6" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 01 §8.14, §12.1, §13 (S51: the sender is never a target; a target has `agent.relay` and is online; `ok` always has `delivered >= 1`, which is a count and not an acknowledgment; the payload is relayed as the same JSON value; the `agent.send` bucket is charged for refused sends too), §12.1 `internal` (S50: not retryable when the SFU says a retry can't help; the scopes an SFU error brings), §15.2 (`relay.go`; S54's placeholder media plane `noMedia`), §15.4 (S50's files in place of a line count); 02 §5.4, §6.2, §10.1 (S52: the layer choice under `Share.mu` then `DownTrack.mu`; `SubscriptionStateEvent`'s `Forwarded` and `Reason`, at once for what the client asked and at most once per 250 ms and subscription for what the media path did; a forwarded layer that ends), §9.3 (the sub PC's own list of spare transceivers, which decides when Pion's `AddTrack` may reuse one; as merged after S52's review, not the `SetSender` of its first version), §17 (`throttle_test.go`, `subscription_test.go`, `sfutest.WithoutCutFrames`); 03 §7.3 (S53: the `push-test` bucket is in `httpapi`, the 404 comes before it, a test that sent nothing gives its token back), §8 (the room name rules in order, the emoji the profile refuses, the admin rule again inside the transaction, rename as a merge), §12.3 rows 24, 28, 38 and 51, §12.4.6 (the keys: 65 bytes starting `0x04` and 16 bytes; rebinding; the session checked again), §12.5; 04 §6.2 (S54: the `db` check keeps its answer for 1 s), §6.4 (the store's close has one more second of its own; the admin socket closes in step 6), §6.6 (what wiring v1 built and what is still a stand-in), §7.4 (the periodic detection as wired); 04 §11.2, §11.3, §11.5 (S55: the `Metrics`, `Transfer` and `ReleaseCheck` APIs; the flush at a month's end and the months owed; the alert thresholds, their `meta` value and severities; the redirect rule, the exact `User-Agent`, the limit on the wire and the format of `ops.release_check`); 05 §3 (S56's viewer files, W00's `i18n/lazy/`), §12.2 (the 5 s `?focus=` wait counts from the room's first `room.state`), §12.4–§12.8 (S56: the policy's inputs as measured, fullscreen in three modes, PiP, the wake lock, the keyboard rules, the frozen tile, the iOS hint, the media session), §19.1 (S56's tests and W00's in `router.test.tsx`, `boot.test.tsx` and `chunks.node.test.ts`). The decisions of §6 "Decided after group 6" are recorded where the specs discussed them (04 §3.2, §6.2, §6.3, §6.5, §7.4 and its decisions list; 01 §8.14, §9 rule 10, §10.4, §15.2, §15.4, §23), the open question on PiP is marked as open in 05 §12.5 and §24 without a change to the rule, and this table has rows for S00E and F03 |
| F03 | 7 | Protocol and hub guards: null relay payload, no rebuild for an idle pub PC | §6 "Decided after group 6" (2), (3); 01 §8.14, §9 rule 10, §15.2, §15.4 | `internal/protocol` (the validation of `agent.send`, its field comments and a fixture, then `task gen`), `internal/server/signal`, `internal/server/sfuplane`, `web/src/protocol/types.gen.ts` (generated) | – | 0.5 | An `agent.send` with `payload: null` gets `bad_request` with `params {field: "payload", reason: "required"}`, like one without a payload: nothing is relayed, so no `agent.recv` has a `null` for its payload, and a `null` inside a payload still passes; a failed pub PC whose connection has no share that is `starting`, `live` or `stalled` gets no `pc.restart{pub, rebuild}` (the hub's `MediaSink` drops the request, which `sfuplane` still makes of every failed pub PC), and one with such a share gets it as before, tested from the SFU's `PCStateEvent` to the wire; `go test -race ./internal/protocol/... ./internal/server/signal/... ./internal/server/sfuplane/...` pass and `task gen:check` is clean |
| S00F | 8 | Group 7 follow-ups: spec write-backs and recorded decisions | the group 7 integrator notes; §6 "Decided after group 7" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 §5.3, §6.1, §6.3, §7.3, §12, §17, §20 (S57: the handshake timer starts with the PC's first answer; `sfu.pc_limit` only for an offer with a `PCKind` above `PCSub`; `sfu.pc_rate_limited` also from `RestartICE` of a closed sub PC, and as an `ErrorEvent` for `Resync()` and a subscription change; the 64 remote candidates counted as Pion keeps candidates, across trickle and remote descriptions, with the pub PC's candidates its own; what the PC creation limit counts; the timing of integration 7; "What README S57 built"); 03 §7.3, §7.7, §7.13, §12.3 rows 12 and 13, §12.4.3 (S58: the password a logged-in user types again counts in `auth-user-ip` and `auth-user`, and the sentence about an unused `push-test` bucket in `auth/limiter.go` is gone with the bucket; a password change rotates the caller's session with the 60 s of 03 §7.4; `ConnSelector.ExceptSessionID` is the password change's, while "sign out other browsers" names each session; the order of the errors of `POST /me/password` and `/me/delete`; the session list); 04 §6, §6.1 steps 5, 6 and 10, §6.2, §6.3, §6.4, §6.6, §7.4, §18 and 01 §19 case 3 (S59: the media sockets in the startup, the ready line, the `media` check's texts, a server that waits for its address starts without media sockets while every other one ends the start with a runtime error, the two budgets of shutdown step 4, the SFU's row out of "Not wired yet", `Addrs.ICEUDP` and `ICETCP`, and the tolerance of the audio-follows-focus case); 04 §12.2, §13.1, §13.2, §13.5 (S60: the added `Env` fields and `DBInfo.TooNewMessage`, `ok` for a check with nothing to check and `skip` only for one that could not run, the result and fix codes, `AdminDoctorRequest.bandwidth`, `GET /api/v1/admin/doctor` answering 404 before the first report, the admin socket's `POST /v1/doctor` waiting its turn instead of `doctor_busy`); 05 §19.3 and 06 §8.2 (S61: openbox under Xvfb, one worker and one retry in CI, Playwright's ffmpeg for the videos, the null sink, the harness's own address per account request so the auth throttles don't couple the specs, the `e2e` job also on deploy changes); 06 §4.9, §4.11, §5 (S62: the package scripts as built, firewalld's reload before `--add-service`, and the service taken out of every zone, policy and rich rule before its file is removed); 01 §9 rule 10, §10.4, §15.2, §15.4 (F03: the hub's `MediaSink`, not `sfuplane`, drops the rebuild request, which passes for any share the connection publishes). The four decisions of §6 "Decided after group 7" are recorded where the specs discussed them (04 §3.1, §4.1, §6.2, §6.5, §7.4, §13.2, §16 and its decisions list; 06 §4.5, §10.2, §17; 01 §5, §19, §23), the two open owner questions are marked as open without a change to their rules (PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19), this table has rows for S00F and F04, and S78's row carries what the write-backs to 06 §4.9 and §4.11 ask of install.sh |
| F04 | 8 | Doctor on a development server; the envelope parser's allocation bound | §6 "Decided after group 7" (3), (4); 04 §13.2; 01 §5, §19 | `internal/server/ops/doctor`, `internal/protocol` (the parser, `Decode`'s way to `encoding/json`, their tests and fuzz seeds; no wire type changes, so `task gen` leaves no diff) | – | 0.5 | With `network.include_loopback` set, no STUN server configured and a site that is not the server's address (off mode, or a domain), doctor's `public_ip` check is `skip` where it was `fail` for a server without a public address, with the code `public_ip.loopback` and a text of its own, so a full run on the dev and e2e configs counts no failure for it (tests where the check stays `fail`: each of the two keys alone, and both keys in ip mode, where the site is the address); `ParseEnvelope` allocates no more than 4× its input plus the constant of `fuzz_test.go` for every input, and `Decode` no more than 4× plus its constant for every payload of a client message: the input that fails on the group 7 tree is a seed in `testdata/fuzz/FuzzParseEnvelope`; deterministic tests, not only fuzz seeds, give `ParseEnvelope` a frame with a long unknown member name, escaped and plain, of 64 KiB and of 256 KiB, and want it accepted and within the bound, and give `Decode` a payload with such a name at those sizes (256 KiB for `pc.offer` and `pc.answer`) and want the result it has without that member, within the bound (01 §19 has the measured cases); `FuzzParseEnvelope` and `FuzzDecode` run 60 s clean; the bound is not loosened (the factor stays 4× and neither constant grows); and what the parser and `Decode` accept and refuse is as before (the round-trip, tolerance and validation tests of 01 §19 pass unchanged); `go test -race ./internal/protocol/... ./internal/server/ops/... ./internal/server/` pass, the last with no change to its tests (`TestServerWithoutASite` wants `fail` and `public_ip.none` with both keys set in ip mode) |
| S00G | 9 | Group 8 follow-ups: spec write-backs and recorded decisions | the group 8 integrator notes; §6 "Decided after group 8" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 04 §13.2 (F04: the `skip` code `public_ip.loopback` and the rule as built, which looks for no address at all) and 01 §5, §13, §15.1, §19 (F04: the parser's own scanner, its fixed errors that wrap `ErrBadMessage`, `Decode` checking the syntax first, the 1024-byte limit on an object key with its exemption for the payload of `agent.send` and `agent.recv`, the tests and seeds); for the catalog entry `doctor.public_ip.loopback` a note in 05 §15.3 and S91's row, since the Doctor page doesn't exist yet; 02 §4, §8.1, §9.3, §9.4, §9.5, §15.1, §17, §20 (S63: the pub interceptor chain [NACK filter, NACK generator capped at 10, RR, twcc] and one `webrtc.API` per pub PC; the DownTrack's second queue, NACK reader and RTT; the `lastTS + 1` rule; the steps of repair in their order, the cap of 256, what is counted, and why the publish side needs more than Pion's generator; the Viewer's own NACKs, `Options.Padding`, `RedundantRTX`; tests 4 and 19; `InSeqOrder` in the stream checks and the itest harness's retry of a 503 `server_busy`, from the integration's "tests that hold on a loaded Linux machine"), and S63's row names integration 19; 03 §7.10, §12.2, §12.3 #9 and #10, §12.4.8, §12.5 (S64: 403 `account_pending` and `account_disabled` for a live reset link of an account that is not active; the admin endpoints as built; a `PATCH` notifies by the fields present; a reset completion notifies `admin.users`); 04 §3.1, §3.2, §6.2, §6.4, §6.5, §12.2–§12.4, §12.6 (S65: the SIGHUP line, the re-exec through `os.Executable` with `/proc/self/exe`, a stop during the restart, exit 75 only without a new process; the socket's backup, restore and rotation; the manifest; the swap at the end of the shutdown; the offline commands' order) and 06 §4.5 (`systemctl reload` is harmless until S90); 05 §12.1, §19.3 (S66: `setOffline` does not close a WebSocket; `--mute-audio`; the tone tab at 1280×658 and 30 fps; `rooms.spec` in the table; the 40 s budget for the high layer; the low window); 06 §4.2, §4.6, §4.9, §4.10, §11.2 (S67, which built the two firewalld rules of §4.9, not S78; S78's row says so); 06 §6.1, §6.4, §8.1, §11.3 (S68: two directory COPYs with `--chmod`, the binary's `--chmod=0755`, Docker Desktop's forwarding from 192.168.65.1, the files in `deploy/docker/test/`, the project `isshoni-smoke`, the ACME override of the part without `.env`). The two decisions of §6 "Decided after group 8" are recorded where the specs discuss them (01 §5, §13, §19, §23; 06 §6.1, §9.2, §17), S73's row carries the pinned `# syntax=` line, the two open owner questions stay marked as open (PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19), and this table has a row for S00G |
| S00H | 10 | Group 9 follow-ups: spec write-backs and recorded decisions | the group 9 integrator notes; §6 "Decided after group 9" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 §4, §5.4, §6.3, §8.4 step 3, §8.5, §10.1, §15.1, §17, §20 (S69: `Room.policyMu` next to `Share.notify`; only `Probe` left unimplemented, until README S76; the codec filter set on every offer and chosen from the first offer's codecs, `pubPC.offered`, an offer without an allowed profile answered with what it has; `Bind`'s unsupported binding drives the guard and a rejected m-line gets a zero codec, Pion fixes a sub PC's video codecs with its first video answer, `checkSubAnswer` only logged, the rebuild rules of `SetDecodeCaps` with equal caps a no-op, a stale empty sub PC and a closed sub PC; the decoders' cap `Subscription.codec` and `codec_mismatch` as a derived reason; the test tools, the tests and `Timings.CodecRetry`; "What README S69 built"); 03 §4.7, §10, §12.4.8, §15 (S70: `device.revoked` has an M1 writer, no CLI path for `user.renamed` or `settings.changed`, the two target variants; the `GET /admin/audit` query rules and `nextBefore`; the `DashboardAccounts` count rules; the janitor's runs and the tests); 04 §4.5, §6, §6.4, §6.6, §14.1, §17, §18 (S71: the `push` shutdown step between `tls` and the admin socket, its drain from the end of step 5; `Deps.PushSender`; "What README S71 wired" with the push row out of "Not wired yet" and `push.Options.Metrics` in it; a bare `push.subject` as a config error, exit 78; `servertest.PushService` and `PushHost`, the push tests); 05 §10.6, §12.1, §12.3, §12.4, §12.7, §19.3 (S72: a share without sound never takes the sound, `FocusState.soundDue` and the settle step; the list of tiles stays at its start; the three specs on the fake display in CI, unmute's `--autoplay-policy` and its CDP reader without trace snapshots, every Playwright page script a user activation); 06 §3, §4.9, §5, §6.1, §7.1, §7.2, §8.2, §9.2–§9.5 (S73: package file names with `-` for `~` and the SBOMs in `checksums.txt`; the packages never reload firewalld; the `# syntax=` line bumped by hand; `tools/goreleaser` and the tasks `release:goreleaser`, `release:assets`, `release:keys` and `release:snapshot`; `goreleaser-check` also on deploy changes; `dockers_v2` images in a snapshot and `extra_files` with their paths, the header's macOS and Windows line; release.yml's jobs as built, with `publish`'s `!cancelled() && needs.verify.result == 'success'` and the condition S82's `site` job needs; the root `README.md` of every archive, which F05 adds). The four decisions of §6 "Decided after group 9" are recorded where the specs discuss them (06 §3, §4.9, §5, §6.1, §7.1, §9.2, §9.5, §17; 02 §8.5; 05 §10.6); the new owner question (the sound when the focused share has none) is marked open in 05 §12.3 and §24 without a change to the rule, next to the two that stay open (PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19); S82's and S85's rows carry what the write-backs ask of them; this table has rows for S00H and F05. For the integrator, code comments outside `docs/`: the "README S72" citations in `web/src/viewer/autoFocus.ts`, `ViewerLayout.tsx` (and `ViewerLayout.module.css`, the unit tests, and the e2e specs `focus-audio.spec.ts` and `layers.spec.ts`) point at 05 §12.3 and §12.1 now; and once F05 lands, the comment on release.yml's build step "The repository has the README.md that every archive carries" drops its sentence about `.goreleaser.yaml`'s pattern (release.yml belongs to S75 in this group) |
| F05 | 10 | Root README.md: the project's front page and the release key fingerprints section | the group 9 integrator notes (a release blocker: release.yml refuses a release without it); 06 §3, §9.4, §9.5, §10.1; `docs/PLAN.md` (Context, Decisions, User journeys) | `README.md` (new, repository root), `.goreleaser.yaml` (only the README archive entry and its comment), `deploy/keys/README.md` (only the paragraph that says the root `README.md` is missing) | – | 0.5 | `README.md` exists at the root, under about 120 lines of plain English: what isshoni is and the problem it solves (self-hosted screen sharing with system audio minus voice apps, an SFU in one Go binary, browser viewers including phones); its status, stated honestly (pre-release; M1, the server and web client, is being built; no release yet; the desktop apps are later milestones); what v0.1.0 will ship (06 §9.5); install pointers marked as available from v0.1.0 (the `install.sh` one-liner at the stable URL of 06 §10.1, and Docker Compose; the site is not live before README S82); a "Release signing keys" section (the heading that `deploy/keys/README.md` names; release.yml checks only that the file exists) that says the fingerprints appear there before the first release and how to verify a download (06 §4.3, §9.4), with no invented fingerprint; the license (Apache-2.0) and links to `CONTRIBUTING.md`, `docs/PLAN.md` and `docs/m1/README.md`; no badges or claims that are not true today; `.goreleaser.yaml` archives it as `src: README.md` (goreleaser then fails without it) and `task release:snapshot` passes with `README.md` in every archive; the markdown renders cleanly |
| S00I | 11 | Group 10 follow-ups: spec write-backs, the security review, recorded decisions | the group 10 integrator and review notes; the security review of `main` at f7053dc; §6 "Decided after group 10" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 01 §10.4, §11.5 B, §19 cases 5, 6 and 8 (S74: case 5 cuts only the sharer's UDP and its outage lasts until the share is stalled, about 7.4 s, and why; §11.5 B's viewer half waits for README F10; a Go client treats a sub PC that the server closed as disconnected, which 02 §7.3 says too; case 6's `replaces` check; the continuity checks after `FinalLossAfter` in cases 5 and 8), 01 §14.3 (S75: what counts as a compat snapshot); 02 §5.4, §6.3, §7.3, §7.6, §12, §18 (S76: the probe goroutine; `sfu.bad_sdp` from `Probe` and `probe_limit`'s `RetryAfter` of at least 1 s; a probe offer's candidates capped at 64 like a PC's; one data channel per probe PC, no echo while 1 MiB waits, the 64 KiB offer with one `m=application`, what `timeout` and `closed` mean; `ProbeErrTimeout` and `ProbeErrClosed`); 05 §8, §11.2, §15.2, §16.3, §19.2, §19.3, §19.4 (S77: the re-post at every `status()`, as its review fixed it; `support()` reading `/info` itself, kept 5 minutes and waited for at most 3 s; `enable()` resolving when the permission isn't granted, `PushSetupError`; `onNotificationOpen` with the full-navigation fallback; the visible-time rule, the card in the empty state, the first-launch sheet, the `prefsStore` dismissal ids, the lazy `RoomPushSlots` chunk; the tests; the push rows of the manual matrix); 06 §4.2, §4.11, §4.13, §8.2, §11.1, §11.2 and D8 (S78: the "not implemented" bullet gone; uninstall as built: the file prefilter, `isshoni.xml` kept while a zone or policy file still names the service, the admin's drop-ins left in place, a failed stop is exit 1 with nothing removed, `--no-firewall` honoured; snapshot releases in place of S67's build from the working tree, the `-b`, `-r` and `-o` flags and `IMAGE_NAME`, V3 in containers, the `distro-release` job, Docker on CI, the scenario order), and S78's row says C1–C13; 06 §9.1, §10.3 (F05's review: a "before tagging" step for the root `README.md`, whose "no release yet" and "not created yet" must not ship in an archive; the cross-check of its fingerprints is README F11's); 03 §6, §12.3 rows 12, 24 and 28, §12.4.6, 04 §14.2–§14.3 (F08: `DeleteSessionPushSubscriptionsExcept`, the `password-change` 429, one subscription per session, the `push.test` lane in `Options` and `Run`); 04 §7.3 and 02 §7.3 (F06: the source guard in how netx builds its muxes, and where 02 points to it). The decisions of §6 "Decided after group 10" are recorded where the specs discuss them (01 §10.4, §11.5 B, §19, §23; 02 §12 and its decisions list; 03 §19; 04 §12.3 and its decisions list; 06 §4.13, §9.3, §17), the owner questions stay open (the sound when the focused share has none, 05 §12.3 and §24; PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19), and this table has rows for S00I, F06, F07 (waiting for the owner), F08, F10 and F11 (group 11) and F09 (group 12) |
| F06 | 11 | ICE source-address cap per ufrag in netx (review R3) | §6 "Decided after group 10" (1); 02 §12 (the 64-candidate cap, "Pion has no cap of its own"); 04 §7.8 | `internal/server/netx`; `docs/m1/02-sfu.md` (the §12 table row only), `docs/m1/04-server-platform.md` (§7.8 only) | – | 1 | A netx test with a real pion/ice agent behind the UDP mux: 1000 Binding requests with a valid USERNAME and MESSAGE-INTEGRITY from 1000 distinct source ports leave at most 64 peer-reflexive remote candidates, and from the 65th source on there is no Binding response and no new address-map entry; the same over the ICE-TCP mux (7882, and 443 when shared); one ufrag reached through two local sockets shares one count; after `RemoveConnByUfrag`, or an ICE restart with a new ufrag, the count starts over (and, from its review, an ICE restart during gathering leaves no ufrag behind); a normal client with host, srflx and IPv6 candidates connects unchanged; the existing netx tests, S59's ICE-TCP-via-443 itest and the SFU integration tests stay green; 02 §12 and 04 §7.8 list the guard and its number; `go test -race ./internal/server/netx/... ./internal/server/itest/`. Built, reviewed and fixed ahead of the group, from `m1/server-web` at ccaf8b0, and merged in it like the others |
| F07 | 11 | Bound pub-PC tracks under SSRC churn (review R1): **waiting for the owner** | §6 "Decided after group 10" (1); 02 §12; the `onPubTrack` description in 02 (§10.1) | `internal/server/sfu` (`pubpc.go`, `pubnack.go`, a new constant in `config.go`, the `PCStateEvent` reason), `internal/server/sfu/sfutest` (a raw-Pion publisher that sends crafted SRTP); `docs/m1/02-sfu.md` (the §12 table row and the `onPubTrack` description only) | – | 1.5 | An sfu integration test with a raw Pion publisher on a real pub PC: (i) a one-m-section offer with no `a=ssrc` or `a=rid`, then one SRTP packet on each of 1000 fresh SSRCs: no reader pair starts for a receiver whose transceiver has no mid; (ii) a simulcast m-section (`a=rid f`, mid and rid header extensions) with the same 1000 SSRCs; in (i) and (ii), once the pub PC's `OnTrack` deliveries pass the `config.go` cap the pub PC is dropped with `PCStateEvent{failed, <new reason>}`, `len(p.tracks)`, the NACK-filter size and the reader count never exceed the cap, the Conn and its sub PC stay up, and goleak is clean after Close; (iii) one legitimate SSRC change on rid `f`, then packets on both SSRCs: the viewer receives every packet of the new SSRC (no second reader pair discarding half) and the old SSRC has left the NACK filter; the existing sfu tests and integrations 1–19 stay green; 02 §12 lists the cap, and the comment in `pubpc.go` no longer claims that Pion stops the old receiver; `go test -race ./internal/server/sfu/...` 3 times in a row. **Waiting for the owner; not merged in group 11.** The production change is on its branch (a cap of 32 deliveries that a pub PC holds, the PC dropped past it with the reason `too_many_tracks`; a transceiver without a mid stopped at once; a track that Pion delivers again with another SSRC superseded), but the tests (i)–(iii) and the crafted-SRTP publisher they need are not written: the run stopped before that part, and how they get written is the owner's call. Its review also left open that a track gives its place back on any read error, not only on `io.EOF` (F07-2). After it merges, a write-back adds `too_many_tracks` to 02 §5.3 and §6.2 and to `sfu/doc.go`, and rewrites 02 §12's "Until it lands, nothing in this table bounds them" and this row. README F06 left it one option, not built: a per-PC cap on the SRTP and SRTCP read streams that Pion keeps for every undeclared SSRC until DTLS closes (a `SettingEngine.BufferFactory` in the SFU, 02 §12; netx below the mux sees no PCs). It shares `internal/server/sfu` with S79, S84, S88 and S92, so it merges first in the group it lands in. **Still waiting for the owner after groups 11 to 14; not in group 12, 13, 14 or 15** (§6 "Decided after group 11" (7), "Decided after group 12" (11), "Decided after group 13" (13), "Decided after group 14" (12)) |
| F08 | 11 | Member abuse caps: password change and `push.test` (review R2, R6) | §6 "Decided after group 10" (1); 03 §7.3 (the throttle table), 03 §12.4.6 (push storage); 04 §14.4–§14.5 | `internal/server/auth` (`selfservice.go` and the limiter wiring), `internal/server/push` (`service.go`, `sender.go`), `internal/server/store` (`push.go`: one subscription per session), `internal/server/httpapi` (`push.go` only if the handler changes); `docs/m1/03-accounts-and-store.md` (the §7.3 table and the push storage bullet), `docs/m1/04-server-platform.md` (§14.4–§14.5) | – | 1.5 | Auth: with a counting hasher, a user's 4th successful `POST /me/password` within 10 minutes is 429 `rate_limited` with `retryAfter` and runs no hash; 20 parallel requests with the correct password and one cookie run at most 2 × burst hashes; a wrong `currentPassword` still costs `auth-user-ip` and `auth-user` as before, and a 503 `server_busy` refunds the new bucket; 03 §7.3 has the `password-change` row. Push: a second `POST /push/subscriptions` from the same session with another endpoint leaves one row for that session (the newer one), and the user cap of 10 still holds across sessions; with a fake Sender that blocks each send to user A's endpoint until its timeout, user A calling `/push/test` every 10 s for 2 minutes delays no `share.started` or `admin.alert` job of user B by more than 1 s, because test jobs never run on the shared workers; a test job is attempted once; the existing auth, push, store and httpapi tests stay green; `go test -race` for the touched packages. Built, reviewed and fixed ahead of the group, from `m1/server-web` at ccaf8b0, and merged in it like the others; the group's integration gives `internal/server`'s `TestShareStartedPush` two sessions for Sam's two devices, since a session now keeps one subscription |
| F10 | 11 | Sub PC ICE restart survives a viewer's UDP outage (S74-R1) | §6 "Decided after group 10" (3); 01 §11.5 B, §19 case 5; 02 §7.3, §9.7 | `internal/server/sfu` (`api.go`'s `settingEngine`, `layer.go`'s `requestKeyframe`, their tests), `internal/server/itest` (only `TestUDPBlackHoleViewer`'s `t.Skip` and what it needs); `docs/m1/02-sfu.md` (§7.3 and §9.7 only) | S74 | 0.5 | (1) The SFU's sub PCs keep checking a candidate pair long enough that an ICE restart requested during the viewer's outage connects once the network is back (Pion's default of 7 binding requests gives up after about 1.5 s): `SetICEMaxBindingRequests` derived from the failed timeout, with a unit test that reads the setting engine's value, and the slice says whether the publish API gets it too and why (02 §7.3); (2) `Layer.requestKeyframe` returns false without touching `lastPLI` while the share's pub PC is down (`share.conn.pubUp` is false), with a unit test; (3) itest `TestUDPBlackHoleViewer` runs (its `t.Skip` removed) and passes 10 times in a row with `-race` on macOS and 3 times in a Linux container with `--cpus 4`; (4) 02 §7.3 and §9.7 describe both; the existing sfu tests and integrations stay green; `go test -race ./internal/server/sfu/... ./internal/server/itest/`. It merges before S79, which works in `internal/server/sfu` in the same group |
| F11 | 11 | `repocheck keys` cross-checks README.md's release key fingerprints (F05-R5) | 06 §9.4, §10.3 (the key pins are cross-checked) | `tools/repocheck` (the keys check and its tests) | F05 | 0.5 | When `deploy/keys/allowed_signers` exists and is not empty, `repocheck keys` (`task lint:keys` and release.yml's use of it) fails unless the root `README.md`'s "## Release signing keys" section lists the SHA256 fingerprint of every key in the file (as `ssh-keygen -lf` prints it, computed in Go) and no row still says the key is not created; it still skips cleanly while `allowed_signers` does not exist; a table test covers no file (skip), all fingerprints present (ok), one missing, an extra stale fingerprint, a "not created yet" row left, and a README without the section, with throwaway keys made in the test; `go -C tools test -race ./repocheck/...` |
| F09 | 12 | Release verify without a write token, publish re-verifies; the backup's default name never writes into an existing file (review R4, R5) | §6 "Decided after group 10" (1); 06 §4.13, §9.3; 04 §12.3 | `.github/workflows/release.yml` (`sign`'s artifact upload, `verify`, `publish`; optionally `build`'s buildx driver image), `cmd/isshoni` (`backup.go` and its test); `docs/m1/06-deploy-and-ci.md` (§4.13, §9.3 only), `docs/m1/04-server-platform.md` (§12.3 only) | – | 1 | Release: `actionlint` is clean; `verify` has `contents: read` (plus `packages: read` and `attestations: read`) and no job-level `GH_TOKEN`, and takes the signed files from an artifact that `sign` uploads; its matrix bases are pinned by digest, with a comment saying how they are bumped, and the distro container starts with `podman run -d --systemd=always`, with no `--privileged`; S78 found that a rootless podman container reaches the runner's `relserve` another way than Docker's host-gateway setup (06 §4.13), so F09 makes `relserve` reachable from it (for example as `host.containers.internal`; `verify` already starts `relserve` on every address) and the release dry run shows it; `publish` downloads the draft before `--draft=false` and fails unless `ssh-keygen -Y verify` against `deploy/keys/allowed_signers`, `sha256sum -c checksums.txt` and the unlisted-asset check all pass, and a script test on fixture directories shows that an added asset and a changed `install.sh` each fail it; the release dry run passes. Backup (`cmd/isshoni` tests): without `--out`, a FIFO already at the default name gives exit 7 and its reader gets no bytes; an explicit `--out` FIFO owned by the caller is streamed as today; an explicit `--out` FIFO whose owner is neither the euid nor root gives exit 7 (through an injectable owner check); `--out -` is unchanged; 04 §12.3 states the rule. In group 12 because S82 touches `release.yml` in group 11 and S90 touches `cmd/isshoni` in group 13 |
| S00J | 12 | Group 11 follow-ups: spec write-backs and recorded decisions | the group 11 integrator and review notes; §6 "Decided after group 11" | `docs/m1/*.md` (not 06 §4.13 and §9.3 or 04 §12.3, which F09 edits in this group) | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 §4, §9.3, §13, §20 (S79: `counters.go` and `pionlog.go`; the receiver reports read for `DownTrackStats.LossPct`, the DownTrack's bitrate and tallies; "What README S79 settled": reports that never wait, rates with retransmissions, transport and RTT only while connected and the pub PC's RTT, every label in `Snapshot`'s totals, totals per SFU, what a layer switch and a downgrade are, the log levels and Pion's info lines and lines with an address at debug, the tests; "What README S79 built"), 02 §17 integration 7 (F10: the restart may now be asked for during the outage), 02 §6.3 (S80 wired `Probe`), 02 §12 (F06's note on Pion's read streams per undeclared SSRC, an option for F07) and 02's decisions list; 04 §7.3 and §10 (netx's Pion info lines at debug, the group 11 integration), 04 §7.7 and §17 (S80: 422 `validation_failed` for a missing or unknown transport, `sfu.bad_sdp` → 400 and `sfu.closed` → 503 `not_ready`, what the limit counts, the context error of a request that ended, the server object, the tests) and 04's decisions list; 05 §10.7, §11.1, §13.1, §13.6, §13.7, §19.3 and §24 (S81: `dropSocket()` still throws until README F12; "`ShareRecovery` as README S81 built it": `resync(kept)` does nothing, the same-`ref` retry in `BrowserSharing.start` and `republish()`, a kept re-publish on a new pub PC, the reconcile that spares a re-publish's copy; the capture hold and its `unreachable` end with [Test my connection]; `PublisherPC.republish` and `forgetServer`; the `link` fixture and `reconnect.spec` (a)–(d)), 01 §19 (the reconnect test's link); 06 §8.1, §8.2, §10.1, §10.3, §10.4, §10.5, §13 (S82: `site.yml`'s triggers and why it has no top-level `concurrency`, the `site` job, `srcExclude` and `check-site.mjs`, the release fetch's checks and the post-build re-check, the Pages-on guard and the `check` job, `TestDocsAnchors` as built), 06 §10.3 (F11's checks beyond its acceptance), 06 §8.5, §11.2 (S83: the VM parts, the harness inline until README F13, `goreleaser-full --skip=sign`, `go-cross-os`, `site-links`; V4 for IPv4 and IPv6, V5 in domain mode only, V6 reading the recorded value) and 06 §17. The decisions of §6 "Decided after group 11" are recorded where the specs discuss them (02 §7.3, §13 and its decisions list; 04 §7.3, §7.7 and its decisions list; 05 §11.1, §13.6, §24; 06 §8.5, §10.5, §17); F07 stays waiting for the owner; the owner questions stay open (the sound when the focused share has none, 05 §12.3 and §24; PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19); this table has rows for S00J, F12, F13 and F14. For the integrator, after F09 and F13 merge: 06 §9.3's `site` job still says "not there yet" (S82 built it: it calls `site.yml` after `publish`, with `pages: write` and `id-token: write`, and fails once the release is out while Pages is off), and 06 §4.13's VM bullets still name `deploy/test/vm/` and a hand run in UTM/Tart, which want F13's layout (06 §8.5 has S83's) |
| F12 | 12 | `window.__isshoni.dropSocket()` works (S81-R3) | 05 §10.7 (the debug handle), §19.3; the group 11 integrator notes (S81-R3) | `web/src/protocol/signal-client.ts` (a public `dropSocket` hook only), `web/src/lib/stats/debugHandle.ts`, `web/src/rooms/connectStats.ts` | S81 | 0.5 | `SignalClient` has a public way to drop its socket the way a network cut does (the client then resumes as after any drop); `window.__isshoni.dropSocket()` calls it instead of throwing `NotImplementedError`; Vitest covers both (the hook drops and the client resumes with the same connection id; the debug handle calls through); the debug handle stays unavailable unless `sessionStorage['isshoni.debug']` is `1`, and exposes nothing beyond 05 §10.7; `reconnect.spec` keeps the `link` fixture; `npm run lint`, `typecheck`, Vitest and `check:size` pass |
| F13 | 12 | The nightly's VM harness moves out of `nightly.yml` (S83-F6) | 06 §4.13, §8.5, §11.2; §6 "Decided after group 11" (5) | `deploy/test/vm/` (new), `deploy/test/distro-test.sh` (only a guard so that it can be sourced), `.github/workflows/nightly.yml` (the `distro-vm` job's steps only), `Taskfile.yml` (only the new scripts in `lint:sh`'s list, if it has an explicit one) | S83 | 1 | The `distro-vm` job runs a script under `deploy/test/vm/` instead of inline bash that sources `distro-test.sh` without its last line; `distro-test.sh` can be sourced (a guard skips its main when sourced) and still runs as before when executed (the container matrix and `test:sh` unchanged); the new scripts pass shellcheck and shfmt through `task lint:sh`; `actionlint` is clean; the `scenario_v3_*` functions of README S78 are reused, not copied; the KVM rows still skip cleanly without `/dev/kvm`; the layout is recorded for the next write-back (06 §4.13, §8.5). Its first real run is the nightly on `main` after the group's PR merges |
| F14 | 12 | Fuzz targets for netx's STUN ufrag and first-frame parsing (F06) | 02 §12 (the peer-reflexive source cap); 04 §7.3, §7.8; §4 "Testing" (seeds in `testdata/fuzz`); the group 11 integrator notes (two temporary 20 s fuzzers found nothing) | `internal/server/netx` (fuzz tests and their seed corpus only; a code fix only if a fuzzer finds a real bug, then with a regression test) | F06 | 0.5 | Committed fuzz targets cover `stunUfrag` (and `admitUDP`'s parsing path) and `firstFrameGate`'s frame reading in `internal/server/netx/srcguard.go`: no panic, no out-of-bounds read, bounded allocation, and for valid STUN the ufrag they return is the one Pion's mux routes by; the seed corpora include valid Binding requests with and without USERNAME, truncated headers and frames over the size limit; each target runs 60 s clean with `-fuzzminimizetime 2s`; `go test -race ./internal/server/netx/...` |
| S00K | 13 | Group 12 follow-ups: spec write-backs and recorded decisions | the group 12 integrator and review notes; §6 "Decided after group 12" | `docs/m1/*.md` (not 06 §4.13 and §9.3, which F16 and F15 edit in this group) | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 §4, §9.3, §9.5, §9.8, §10.1, §10.2, §11, §12, §13, §15.1, §17 and §20 (S84: the REMBs and receiver reports that reach the allocator; retransmissions paced through the media bucket, the RTX queue first, audio unpaced; the loss input as the link lost it: the SFU's own skipped sequence numbers taken out, a report more than 2 s after the last only a new start, evidence only from intervals that began after a decision, a verdict on at least 20 packets; the guards on the viewer's RTCP; `DownlinkEstimate`; no cap on a share without a smaller layer; every cap decided again when a request changes while limited; the ticker's looks up to 1 s late; `sfutest.Link` and `ViewerOptions.REMB`, `rembsim` judged per 50 ms step; integration 4 at 5 % loss; integration 16's trial failing on REMB; the base layer pausing's "requested" left for S88 as `Subscription.reqVideo`); 04 §4.5, §6, §6.1, §6.4, §6.6, §7.8, §9.3, §11.2, §11.3, §11.4, §11.5, §13.1, §17 and §18 (S85: an invalid `updates.release_url` a config error, exit 78, redacted by `ops.RedactURL`; `Addrs.Metrics` and the ready line's `listen_metrics`; the shutdown step `transfer`; the "Not wired yet" table gone; "What README S85 wired"; `POST /api/v1/admin/doctor`'s 3-minute read deadline; `sfuCollector` without panics; `listen.http`'s bytes counted as web; the alerts' severities and `params`, `dashboard.source_failed`'s source names, `clients[].outdated`, a failed source being an error, a panic or a cooperative 1 s timeout; and §17's netx rows with F06's guard tests, F14's fuzz targets and `TestPionRoutesLikeReference`); 05 §3, §10.6, §10.7, §11.2, §14.1, §15.3, §19.1 and §19.3 (S86: `codecWait.ts`, `caps.update` at most every 5 s, the 30 s gap rule, the "can't play" banner only once a codec status exists, `CodecBanner`, no toast, the debug state's `codecWait` and per-share `codec`, a §19.1 row for `viewer/codecWait.ts` and a §19.3 row for `hint.spec`; S87: the invite card's texts in the lazy `setup` namespace, "Done" opening `/r/lounge`, the account menu's card left for F17; S85's alerts and source names for README S91's page); 01 §11.7 (S86: no toast); 06 §7.1, §7.2, §8.5, §11.2, §13 and §17 (F09-R5: the image digests bumped by hand with the tool pins; F13-R6: `test:sh` runs `vm-test.sh -h`; F13: the `distro-vm` job's first real run is the nightly on `main`, with an owner action to look at it; F09's departures and what it left for F15 and F16). 04's decision 9 and 06 §17 "Decided after group 10" no longer call F09 future work, and §6 "Decided after group 10" (1) says it merged. The decisions of §6 "Decided after group 12" are recorded where the specs discuss them (02 §9.5, §9.8, §10.2, §17 and its decisions list; 04 §4.5, §6.4, §11.3, §11.4 and its decisions list; 05 §10.6, §14.1, §24; 06 §7.1, §17), with the Touches extensions group 12 accepted; F07 stays waiting for the owner; the owner questions stay open (the sound when the focused share has none, 05 §12.3 and §24; PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19); this table has rows for S00K, F15, F16, F17 and F18. For the integrator: `.github/dependabot.yml`'s "Not covered here" comment already names the digests of 06 §7.1 (the group 12 integration's commit), so F09-R5 needs no further code change |
| F15 | 13 | The release's draft check becomes a script with a committed test (F09-R1) | 06 §9.3; the group 12 review notes (F09-R1, F09-R3); §6 "Decided after group 12" (10) | `.github/workflows/release.yml` (the `publish` job's checkout and draft-check steps only: the sparse checkout adds `deploy/release`, review F15-R3), `deploy/release/` (new: `check-draft.sh`), `deploy/test/` (a new test file for it), `Taskfile.yml` (`test:sh`'s body only, to run the new test); `docs/m1/06-deploy-and-ci.md` (§9.3 only) | F09 | 0.5 | `publish` runs `deploy/release/check-draft.sh` instead of the inline step, with the same checks (`ssh-keygen -Y verify` against `deploy/keys/allowed_signers`, `sha256sum -c checksums.txt`, no unlisted asset, hidden files included); a shell test on fixture directories (throwaway keys made in the test) shows ok for a good draft and failure for an added asset, a changed `install.sh`, a hidden file, a bad signature and a missing checksums line; `task test:sh` runs it under dash and `bash --posix`; shellcheck, shfmt and actionlint clean; `sign`'s own asset loop gets the same `dotglob`/`nullglob` strictness (F09-R3) |
| F16 | 13 | `distro-test.sh`'s podman path boots systemd's sandbox (F09-R4) | 06 §4.13, §11.2; the group 12 review notes (F09-R2, F09-R4); §6 "Decided after group 12" (10) | `deploy/test/distro-test.sh` (the podman run line and its comment), `docs/m1/06-deploy-and-ci.md` (§4.13 only) | F09 | 0.5 | With `CONTAINER_ENGINE=podman`, `distro-test.sh` starts the container with what systemd needs to set up the unit's sandbox (`--cap-add SYS_ADMIN`, and the cgroup and user-session requirements documented), so C1 passes instead of exiting 78 on "not a mounted volume", consistent with `release.yml`'s `verify`; the Docker path is unchanged; 06 §4.13's sentence about the missing capability is replaced by the rule as built; where podman isn't available, the Docker path and a shellcheck/shfmt pass are tested and the report says so; the containers and images it creates are named `isshoni-*` and removed afterwards, and no other is touched |
| F17 | 13 | Web follow-ups from group 12: "Invite friends" opens the invite card; typed debug state | 05 §10.7, §11.2, §14.1, §19.3; the group 12 integrator notes (S86, S87 carry-overs); §6 "Decided after group 12" (10) | `web/src/rooms/` (`AccountMenu.tsx`, a small invite dialog, `index.ts`'s lazy import, `connectViewer.ts`'s signal type), `web/e2e/stats.ts` (`DebugState`/`DebugShare` types), `web/e2e/hint.spec.ts` (only to use the typed fields) | S86, S87 | 1 | The account menu's "Invite friends" opens a dialog that creates an invite (`POST /api/v1/invites {}`) and renders S87's `InviteLinkCard`, for users allowed to invite (03's rule), instead of linking to `/admin/invites`; the `setup` i18n namespace is loaded before use (`check:i18n` passes); `web/e2e/stats.ts` declares `viewerDebugState`'s `codecWait` and per-share `codec`, and `hint.spec` reads them without `Reflect.get`; `connectViewer.ts`'s signal type includes `notify`; Vitest for the dialog; `npm run lint`, `typecheck`, Vitest, `check:size` and e2e pass. As built (§6 "Decided after group 13" (10), (11); 05 §11.2): members also get "My invites", nobody gets "Invite friends" while registration is closed, and a refusal that no retry ends has no "Try again"; the card's "once" text for members is README F19's |
| F18 | 13 | netx reads the STUN ufrag without decoding the whole message (F14 note) | 02 §12; 04 §7.8; the group 12 integrator notes (F14: about 43 bytes allocated per input byte); §6 "Decided after group 12" (10) | `internal/server/netx` (`srcguard.go`'s `stunUfrag` and its tests) | F14 | 0.5 | `stunUfrag` finds the USERNAME attribute by scanning the attribute list without `stun.Message.Decode`'s allocations (allocation-free or a constant small bound, shown by `testing.AllocsPerRun`); its result is identical to the decoding version for every input in F14's fuzz corpus and in a differential fuzz target run 60 s; `TestPionRoutesLikeReference` still passes; `go test -race ./internal/server/netx/...` |
| S00L | 14 | Group 13 follow-ups: spec write-backs and recorded decisions | the group 13 integrator and review notes; §6 "Decided after group 13" | `docs/m1/*.md` | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 02 §4, §5.4, §6.1, §6.2, §8.6, §9.1, §9.7, §10.2, §11, §15.1, §17, §18, §20 and its decisions list, with 01 §8.7, §8.10 and §10.5 (S88: one REMB per pub PC with the sum of its shares' caps and none while a share has no cap; three REMBs of 1 Gbit/s once the cap goes; a hint on every change of the cap or the pause state, `admin` also when `f`'s encodings don't change and for a pending share; keyframe requests for a paused layer dropped without starting the throttle; no pausing without a `q` layer; the 10 s from `StartShare` or from the first look that found the last request for `high` gone; a resume on any new request for `high`, decoder state aside, posted at once; the allocator weighing a paused or just resumed layer at its maximum; jitter per frame; `UpdateShare`'s encodings with the current cap and pause state and not taken as told; `Resync`'s hint rebuilt from the current cap, pause state and preset; `HintReasonAdmin` and `HintReasonViewers`; the uplink types as built; `Publisher.SetActive`, `sfutest.ApplyHint` and the uplink clock of `export_test.go`; the new tests; "What README S88 built"); 02 §4, §15.2, §17, §18 and §20, with 05 §19.3 and §21 (S89: `client.go`; `-v` and exit 130; media that never came is exit 1; `-duration` with the warm-up; teardown's stray accounts; the expected egress from the publishers' measured rates and `receivedMbps`; the memory check skipped while `rssBytes` is 0; the switch check after repairs; `smoke`'s limits; all-high's knee; the report's fields; `LOADTEST_BIN` and `decodable.spec`'s row; the tests); 04 §3.1, §4.1, §6 (`Server.Reload`), §6.5, §7.8, §8.3, §8.4, §8.5, §9.3, §10, §17, §18, §19 and its decisions list, with 06 §4.5 and §14 (S90: SIGHUP reloads, with `Reload`'s rules; `limits.conns_per_ip` in off mode except for trusted proxies; the 503s given on purpose at debug; the redaction net; the `stdlog` bridge; `TestReexecDeletedBinary`; the new tests and the canary's allowances; 04 §6.1 step 6 checked: S00K had already fixed its `metrics.enabled` warning); 05 §15.3, §16.5, §19.1, §19.2, §19.3 and §24 (S91: the `admin.dashboard`/`admin.doctor` keys, the `doctor.part` fragments, the context and plural variants, fix steps as a sentence plus a command, the freshness line, the alert texts, the certificate's last error, failed sources not shown as zero values, the names of wire values, the connection-test card without a landmark, the tests, `admin.spec`'s fourth test; F04's stale note on the catalog); 05 §10.6, §10.7, §11.2, §14.1, §19.2 and §24 (F17: `InviteDialog`, "My invites" for members, the closed-mode rule with `/info` read again when the menu opens, the entry order, no "Try again" after `registration_closed` or `forbidden`, the `room.menu.myInvites` and `room.invite.*` keys, `rooms/InviteDialog.test.tsx`; `connectViewer.ts`'s signal and the typed debug state); 04 §7.8 and §17 (F18: `stunUfrag` an allocation-free walk held to `Decode`, `FuzzStunUfragDecode`, `TestStunUfragWalk`, `TestStunUfragAllocations`, the gate's new bound); 06 §7.2 and §13 (F15: written by the group 13 integration, checked against `Taskfile.yml`). The decisions of §6 "Decided after group 13" are recorded where the specs discuss them (02 §11 and its decisions list; 04 §13.2, §17 and its decisions list; 05 §15.3, §16.5, §24; 06 §12.1, §16, §17); F07 stays waiting for the owner; the owner questions stay open (the sound when the focused share has none, 05 §12.3 and §24; PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19); this table has rows for S00L, F19 and F20, and S94's row is split in two |
| F19 | 14 | The invite card's text fits a member's "My invites" (F17-R2) | 05 §11.2, §14.1; the group 13 review notes (F17-R2); §6 "Decided after group 13" (12) | `web/src/setup/InviteLinkCard.tsx` (an optional prop for its "once" text) and its test, `web/src/rooms/` (only the invite dialog's use of it), `web/src/i18n/en.json` (only the new key) | F17 | 0.5 | `InviteLinkCard` takes an optional key for its "once" text; the setup wizard keeps "under Admin → Invites", and the rooms invite dialog of a member says where a member finds the invite again ("My invites"); Vitest for both; `check:i18n`, lint and typecheck pass |
| F20 | 14 | Log hygiene: Pion's ICE password redacted; no shutdown WARN from Transfer's first tick | 04 §10, §11.3, §17 (the log canary); PLAN privacy; S90's blocked items (group 13); §6 "Decided after group 13" (12) | `internal/logx` (the Pion log bridge and its rate limiter, with tests), `internal/server/ops` (Transfer's first tick only, with a test) | S90 | 0.5 | The Pion bridge redacts ICE passwords before writing, at every level: the value of `remotePwd`, `localPwd`, `ice-pwd`/`icePwd` or a bare `pwd` after `:` or `=`, quoted or not, in any case (the forms `internal/server/logsafety.go`'s `icePassword` matches; pion/ice logs `remotePwd` at debug in its `Started agent` line), with a test that feeds Pion's own message formats, `Started agent` among them; the rate limiter's "further messages suppressed" line is logged at the level of the suppressed lines (not WARN for debug lines), with a test; Transfer's first tick does not log a WARN when the server stops right after starting (a cancelled context is a normal stop), with a test; S90's log canary still passes; `go test -race ./internal/logx/... ./internal/server/...` |
| S00M | 15 | Group 14 follow-ups: spec write-backs and recorded decisions | the group 14 integrator and review notes; §6 "Decided after group 14" | `docs/m1/*.md` (not 04 §13.2 and 06 §11.3, which F21 edits in this group) | – | 0.5 | The write-backs are in the specs and match the code as merged, with no code change: 04 §10, §11.3, §17 and its decisions list, with 02 §13 (F20: the Pion bridge redacting named passwords and the password of a ufrag:password pair at every level, the pair rule's lean toward redacting, `internal/server/logsafety.go` as a second layer; the limiter's notice at the dropped line's level and again for a higher one, the count at the highest, one budget per scope across levels; a tick that the stop cuts short logs at debug and skips the alert check, `loadSent` and `rememberSent` quiet for a cancelled context, a store timeout still a warning; `TestRedactCredentials`, `TestPionRedactsAtEveryLevel`, `TestPionRedactsICEAgentPassword`, `TestPionRateLimitLevels`, `TestLineLimiterLevels`, `TestTransferStopInFirstTick` and logx's goleak); 05 §11.2, §14.1 and §19.2 (F19: `InviteLinkCardProps.onceKey`, default `setup.inviteCard.once`; `InviteDialogProps.me` from `AccountMenu`; `room.invite.once` ("My invites") for members, among the `room.invite.*` keys); 05 §3, §10.6, §10.7, §12.7, §16.5, §16.6, §19.1, §19.2, §19.3 and §24 (S93 with the group 14 integration's wiring: `attachViewerStats` in `viewer/statsLink.ts`, `viewer.debug` in `debugStats.ts`, `DebugOverlay`/`DebugPanel` with `?debug=1` for one page, `share/stats.ts` as the pub PC's source, `stats`'s `intervalMs` as the time since the report before; the stale W13 sentences of §10.7 and §12.7 gone; the F17-R3 carry-over done; rule 4 of `check:i18n` with its use rules, `READ_WHOLE = ['push']` and its warnings on stderr; the announcements; the `a11y.spec` row with both colour schemes, the overlay and the settle wait, and the `version.spec` row as §16.4 has it; the new test files); 06 §7.4, §12.3, §16 and §17 (S94: `run.csv`'s row kinds and columns, `cpus` and `host_cpu_pct`, the `<out>-metrics/` scrapes, the flags `-unit`, `-pid-file`, `-nic`, `-metrics-dir`, `-proc`, `-interval` and `-scrape-interval`, the `Udp6` counters, USER_HZ 100, the summary's verdict rules, the journal read by the JSON level, `systemd-run --collect`; a Linux container's `go -C tools test ./...` and the darwin `.bin/go-licenses`); 05 §19.3 and 04 §17 (the PR #22 test fixes merged after group 13: `hint.spec`'s start profile, `TestSourceGuardTCPAgent`'s distinct sources). The sentences that still described F19 and F20 as planned are in the past tense (04 §10 and its decision 16; §6 "Decided after group 13" (8), (12)). The decisions of §6 "Decided after group 14" are recorded where the specs discuss them (04's decisions list; 05 §24; 06 §17), and §6 keeps S00L's numbering of "Decided after group 13"; F07 stays waiting for the owner; the owner questions stay open (the sound when the focused share has none, 05 §12.3 and §24; PiP on Android Chrome, 05 §12.5 and §24; room names and the emoji that need U+FE0F or a joiner, 03 §8 and §19); this table has rows for S00M and F21, and S93's row records the integration's extension of its Touches |
| F21 | 15 | Doctor asserted per code in CI's Linux docker-smoke; two doctor rules reviewed | 04 §13.2; 06 §6, §11.3; §6 "Decided after group 13" (2), "Decided after group 14" (11) | `deploy/docker/test/` (the smoke script's doctor step), `internal/server/ops/doctor` (the `public_ip` rules and their tests); `docs/m1/04-server-platform.md` (§13.2 only), `docs/m1/06-deploy-and-ci.md` (§11.3 only) | S90 | 1 | `docker-smoke` runs `isshoni doctor --json` inside the container in both the bridge and the host setup and asserts each check by its code, with an explicit allow-list for what a CI runner can't satisfy, each entry named and justified in the script; on the developer's Mac the same step runs under Docker Desktop with that environment's allow-list and passes; the two `public_ip` rules are decided and tested: (a) a Docker bridge is not reported as symmetric NAT when the published ports don't depend on the outbound NAT kind (or 04 §13.2 says why it stays), (b) a configured literal `127.0.0.1`, or any loopback address, is not taken for a public address; table tests for both; `go test -race ./internal/server/ops/...`; shellcheck and shfmt clean; every container and image it creates is named `isshoni-*` or `s<NN>-*` and removed afterwards, and no other is touched. CI's `docker-smoke` job changes only if the step needs a new input (`.github/workflows/ci.yml` is not in the Touches: the report says so, and the integrator applies it). The integrator runs `task docker:smoke` on the Mac after the merge |
| F22 | 16 | Dashboard and connection test don't call a Docker bridge "symmetric NAT" (F21 follow-up) | 04 §7.4, §7.7, §11.4, §13.2 (F21's rule (a), the param `dockerBridge`); 05 §14.2, §15.3 | `internal/protocol/api` (an additive field next to the dashboard's and the connection test's `nat`), `internal/server/ops` and `internal/server` (where they are filled), `web/src/admin/dashboardText.ts`, `web/src/conntest/fixText.ts` and their tests, `web/src/i18n/en.json` (new keys only), `web/src/protocol/*.gen.ts` (regenerated); `docs/m1/04-server-platform.md` (§7.4, §7.7, §11.4 only), `docs/m1/05-web-client.md` (§14.1 and §15.3 in the plan; the slice edited §14.2, where `ConnTestResult` and the NAT fix texts are, instead of §14.1). The group 16 integration recorded three one-line extensions the slice needed: `web/src/conntest/runConnTest.ts` (the result's `server.dockerBridge`, copied only when the server sent `true`), `web/src/admin/DashboardPage.tsx` (`natName` takes the server object) and the key `admin.dashboard.natKind.symmetric_dockerBridge` in `web/src/i18n/lazy/admin.en.json`, the file of the `admin` namespace | F21 | 1 | Where F21's doctor rule lets a container's bridge NAT pass (`dockerBridge`), the dashboard and the connection test say the same: an additive `dockerBridge: true` next to `nat` in the dashboard and conntest JSON, filled from the same detection (the wiring runs doctor's `public_ip` once, `dockerBridgeNAT`); the web shows a bridge note instead of "symmetric NAT" or the cgnat fix text for it, and unchanged text otherwise; the protocol change is additive (golden fixtures and the compat test pass; `task gen` leaves no diff after regeneration); Go tests for the field's filling, Vitest for both texts; `go test -race ./internal/server/... ./internal/protocol/...`; npm lint, typecheck, Vitest, `check:i18n`, `check:size` pass. Not in it: `isshoni admin status` still prints the raw NAT kind (`api.ServerStatus` has no flag) |
| S01 | 1 | Repo skeleton and dev loop | 06 S1; 06 §7.1–7.3 | root: `go.mod`, `go.sum`, `Taskfile.yml`, `.tool-versions`, `.golangci.yml`, `.gitignore`, `tygo.yaml`, `LICENSE`, `NOTICE`, `CONTRIBUTING.md`; `tools/`, `deploy/dev/` | – | 1 | Fresh clone: `mise install && task setup` succeed; `task lint:go lint:pins test:go` pass on the empty module; `task --list` shows every task of 06 §7.2 (tasks whose inputs do not exist yet exit 0 with a note); `.bin/tygo` is v0.2.21; `task build:go` stops with a clear message while `web/dist/index.html` is missing |
| S02 | 1 | CI core | 06 S2; 06 §8.1–8.2, §8.6 | `.github/workflows/ci.yml`, `.github/dependabot.yml` | – | 1.5 | `actionlint` clean; once group 1 is merged, a PR with a gofmt error turns `ci-ok` red and a clean PR is green in < 12 min; jobs whose inputs are missing are skipped and count as success. Owner: 06 §8.6 steps 1–3 and 6–7 |
| S03 | 1 | Protocol types and fixtures | 01 P1; 01 §5, §8, §14.3, §15.1 | `internal/protocol` | – | 3 | `go test -race ./internal/protocol/...`: round-trip, registry coverage, no-null, unknown-field tolerance, validation tables, codec table and secret redaction pass; each fuzz target runs 30 s clean; `go list -deps` shows only the standard library |
| S04 | 1 | REST and ops DTOs, error table | 03 §12.2, §12.4, §16; 04 §2, §9.4, §18 | `internal/protocol/api` | – | 1 | Golden JSON round-trip for every DTO; every error code has a status in `api.StatusOf`; a reflection test finds only camelCase JSON names; standard library imports only |
| S05 | 1 | CLI skeleton, version, logx | 04 S1; 04 §3, §10, §15 | `cmd/isshoni`, `internal/version`, `internal/logx` | – | 1.5 | A build with `-ldflags -X…` makes `isshoni version --json` show the injected values and `--short` print the version; logx tests (Secret redacted for every fmt verb except `%p` and `%w`, which fmt prints raw (vet rejects only `%w`; 04 §10), ReplaceAttr keys, `auto` format); testscript: usage text and exit 2 on a bad flag |
| S06 | 1 | SFU codec and H.264 helpers | 02 slice 1; 02 §8.1–8.2 | `internal/server/sfu` | – | 2.5 | `codec_test`, `h264_test`, `sdpcheck_test` pass; each fuzzer runs 60 s clean; golden SDP fragments match 02 §8.1 |
| S07 | 1 | Store foundation | 03 slice 1; 03 §4–5 | `internal/server/store` | – | 2 | Store tests for migrations, pre-migration backups, newer-schema refusal (`ErrNeedsOperator`) and constraints pass; a new DB contains Lounge; the schema dump matches a golden copy of 03 §5 |
| S08 | 1 | Auth primitives and CSRF wrapper | 03 slices 3 (origin.go) and 4; 03 §3.2, §7.1–7.3, §7.5 | `internal/server/auth` | – | 1.5 | Unit tests for keyed hashes, PRECIS usernames, the common-password list, argon2 (PHC, semaphore, dummy hash), the limiter (burst, refill, one key per IPv6 /64) and the CSRF wrapper; fuzz targets run 30 s clean |
| S09 | 1 | Web scaffold, build and embed | 05 W1; 05 §2–3, §16.5 (first rule), §17 | `web/` root files (package.json, lockfile, .npmrc, index.html, vite/ts/eslint/prettier configs), `web/build/`, `web/public/`, `web/scripts/{check-size,check-i18n}.mjs`, `web/src/{main.tsx,i18n/,types/}`, `web/src/test/setup.ts`, `web/src/ui/{tokens,global}.css`, `web/embed.go`, `web/dist/.gitkeep` | – | 2.5 | `npm ci && npm run lint && npm run typecheck && npm test -- --run && npm run build && npm run check:size && npm run check:i18n` pass (the Vitest setup file `src/test/setup.ts` loads); a JSX literal string fails lint; a `t('…')` literal key missing from `en.json` fails `check:i18n` (the script's only rule so far); `dist/` has `version.json` and `.br`/`.gz` siblings; `go build ./web` works with only `dist/.gitkeep`; the PR notes the confirmed React Router 8 API names |
| S10 | 2 | TS generation | 01 P2; 01 §14.4, §16 | `internal/protocol/gen/tsregistry`, `web/src/protocol/{types,api,registry}.gen.ts`, `web/src/protocol/codecs.ts`, `web/src/protocol/{registry,codecs}.test.ts` | S01, S03, S04, S09 | 1.5 | `task gen` is idempotent; `npm run typecheck` and the registry and codec Vitest files pass; renaming a Go field without regenerating fails `task gen:check` |
| S11 | 2 | Signal hub skeleton | 01 P3; 01 §3, §6, §12–13, §15.2 | `internal/server/signal`, `internal/server/signal/signaltest` | S03, S05 | 3 | Upgrade (Origin matrix, pre-auth limits, 16 connections per user), handshake, rate-limit, slow-consumer and shutdown tests of 01 §19 pass; every `Deps` interface is declared in full with fakes in `signaltest`; goleak clean |
| S12 | 2 | Munger and packet cache | 02 slices 2–3; 02 §9.2, §9.4; 02 §8.4 step 1 (m-sections without a share, S00) | `internal/server/sfu` | S06 | 2.5 | `munger_test` (with the S4 ports) and `packetcache_test` pass under `-race`; `FuzzMunger` runs 60 s clean; munger coverage ≥ 90%; insert+get benchmark < 100 ns; `sdpcheck_test`: a sending m-section without a binding, or bound to a share that isn't this Conn's, carries no share and is no error (no `sfu.unknown_track`, 02 §6.3) |
| S13 | 2 | Fake media, Go publisher, Go viewer | 02 slice 4; 02 §15.1 | `internal/media/fake`, `internal/client/publish`, `internal/server/sfu/sfutest` | S06 | 2.5 | Pion-to-Pion loopback without the SFU: the viewer sees the configured bitrate ±5%, markers intact, an SR every 1 s, and a keyframe within one frame of a PLI |
| S14 | 2 | Store queries and pruning | 03 slice 2; 03 §6 | `internal/server/store` | S04, S07 | 2 | The remaining store unit tests pass, including concurrent `UseInvite` and prune boundaries |
| S15 | 2 | Config | 04 S2; 04 §4; 04 §15 (`BuildInfo.Protocol`, `Schema`) | `internal/server/config`, `cmd/isshoni` | S03, S05, S07 | 2.5 | Config tests of 04 §17 pass; every M1 key (04 §4.3, incl. 01's guard, `sfu.pause_unwatched_layers` and the policy keys) is registered in `keys.go`; `config example` round-trips; `config init` refuses to overwrite (exit 7); errors show file:line and a fix; `version --json` reports `protocol.Version` and `store.LatestSchemaVersion()` (a test checks both; S05's handoff) |
| S16 | 2 | ICE transports and public IP | 04 S5; 04 §7.1, §7.3–7.6 | `internal/server/netx` | S04, S05 | 3 | netx unit tests pass; a Pion PeerConnection pair connects through `Transport` over UDP and over TCP 7882 on loopback; `TransferCounter` counts both |
| S17 | 2 | HTTP router, middleware, JSON helpers, SPA handler | 04 S4 (router); 04 §9 | `internal/server/httpapi` | S04, S05 | 2 | Router tests: middleware order of 04 §9.3, `ClientIP` with trusted proxies, `WriteError` emits 03's envelope, security headers of 04 §9.6, SPA fallback and caching (hashed assets immutable, `index.html` no-cache, `.br`/`.gz` served) |
| S18 | 2 | License gate and notices | 06 S3; 06 §8.3–8.4 | `tools/notices/`, `web/scripts/licenses.mjs`, `web/licenses.overrides.json`, `web/package.json` (build script), `deploy/notices/`, `.github/workflows/ci.yml` | S01, S02, S09 | 1.5 | A GPL-3.0 fixture dependency fails `task licenses`; `THIRD_PARTY_NOTICES` lists every module in `go version -m bin/isshoni`, the Go runtime and every prod npm package; `npm run build` writes `dist/licenses.txt` |
| S19 | 3 | Rooms and participants | 01 P4; 01 §4, §8.4–8.6, §8.12 | `internal/server/signal`, `internal/server/signal/signaltest` | S11 | 2.5 | Room, notify and revocation tests of 01 §19 pass: coalesced byte-identical `room.state` (at most one per 200 ms), `room.event`, `Notify`, `CloseRoom`, `UpdateUser`, `CloseConnections` |
| S20 | 3 | TS SignalClient | 01 P10; 01 §10, §16 | `web/src/protocol/{signal-client,errors,index}.ts`, `web/src/protocol/signal-client.test.ts`, `web/src/protocol/testing/` | S10 | 2.5 | Vitest cases of 01 §19 pass with the fake socket in `web/src/protocol/testing/` (later web slices reuse it): handshake, backoff, resume, `retryNow`, `staleBuild`, `probe`, `info.shutdown`, unknown error codes handled by scope and `retryable` |
| S21 | 3 | SFU on netx transports | 02 slice 5; 02 §7.1–7.3 | `internal/server/sfu` | S12, S16 | 1.5 | `api_test` on a loopback `netx.Transport`: UDP 7882 and TCP 7882 candidates, no trickle, the remote-candidate filter table (the TCP 443 case lands in S29) |
| S22 | 3 | Decodable fake media | 02 slice 15 (fake); 02 §15.1 | `internal/media/fake` | S13 | 1.5 | `fake` tests: SPS/PPS and slice headers parse with the SFU parser; macroblock count and I_PCM alignment checked by a bit reader; the Opus asset packets are valid |
| S23 | 3 | Settings cache | 03 slice 5 (cache); 03 §9 | `internal/server/store` | S14 | 0.5 | `SettingsCache` tests: typed validation, `Pin` locks a field, `OnChange` fires once per change, defaults |
| S24 | 3 | REST API chain and /info | 03 slice 3 (HTTP); 03 §7.5, §7.13 (declarations), §12.1–12.3 | `internal/server/httpapi`, `internal/server/auth/service.go` | S04, S07, S08, S17 | 1 | CSRF and Content-Type matrix tests pass; `GET /api/v1/info` returns the 03 §12.4.1 shape with fake sources; every response has `no-store`; unknown `/api/v1/` paths get JSON 404/405; `httpapi.Signal`, `Push` and `InfoSource` are declared in full; `auth/service.go` declares the full 03 §7.13 auth surface (`Service` with every method but the four M2 device-flow ones of §3 "Carried to later milestones", `Options`, `New`, `Principal`, `ActorOf`, `ConnSelector`, `ConnCloser`, `LoginResult`, `RegisterResult`, `UserChange`), the bodies returning an error that names the method and wraps `api.NewError(api.CodeInternal)` (`fmt.Errorf("auth: Login not implemented: %w", api.NewError(api.CodeInternal))`: api has no not_implemented code, and api is outside S24's Touches; S30 and the later auth slices fill them in) |
| S25 | 3 | Data dir and secrets | 04 S3; 04 §5.1–5.2 | `internal/server/config` | S15 | 1.5 | Secrets tests (create, load, permissions, rotate); the container data-volume check exits 78 with a fake mountinfo and passes with `ISSHONI_ALLOW_EPHEMERAL_DATA=1` |
| S26 | 3 | 443 multiplexer | 04 S6; 04 §7.2 | `internal/server/netx` | S16 | 2 | portmux tests pass (TLS, RFC 4571, plain-HTTP hint, garbage, slow client, per-IP limits, `Close` unblocks both `Accept`s); goleak clean |
| S27 | 3 | Web app shell: platform, REST, boot | 05 W2; 05 §4–6, §8, §16.5 (error-code rule), §19.1 (i18n) | `web/src/{platform,app,lib,ui,test}/`, `web/src/main.tsx`, `web/src/protocol/{rest,queryKeys,invalidate}.ts`, `web/scripts/check-i18n.mjs` | S09, S10 | 3 | Unit tests for `ApiError` (both envelopes) and the topic → query-key map; MSW tests for each boot branch (Offline, NeedsHttps, NotSetUp, Unsupported, Fatal); `role` is `viewer` with a mobile UA; `router.tsx` has every 05 §5 route, each lazily loading its folder; the whole `Platform` interface is declared, with stub `displayMedia.ts`, `pwa.ts` and `push.ts` and a stub `UpdatePill` mounted in the shell (S35, S38 and S77 fill them in); every `ErrorCode` and api `Code…` constant has an `errors.<code>` key in `en.json`, `check:i18n` fails when one is missing, and the i18n unit test of 05 §19.1 passes |
| S28 | 4 | Resume and grace | 01 P5; 01 §10.1–10.3, §10.5 | `internal/server/signal`, `internal/server/signal/signaltest` | S19 | 2 | Resume tests of 01 §19 pass, including no leave/join events within the 30 s grace, `replaced` on a second socket, the deliberate-close fast path and resume-key rotation |
| S29 | 4 | SFU core: model, Conn actor, PCs | 02 slice 6 (part); 02 §5–6 | `internal/server/sfu`, `internal/server/sfu/sfutest` | S13, S21, S26 | 3 | The whole 02 §6 API exists (methods of later slices return a not-implemented error); through `sfutest.Harness` a publisher and a viewer negotiate pub and sub PCs with `gen`/`neg` and `tracks`; a stale offer → `sfu.stale_offer`; a pub offer that still binds a share stopped while it was in flight is answered, with that share's m-sections `a=inactive` (02 §8.4); the sub offer has a passive TCP 443 candidate via `netx.ListenPortMux` on 127.0.0.1:0; `TestProbeAPIsIPv4Only` runs (is not skipped) on a dual-stack loopback: its guard expects one IPv6 entry in `Advertised`, UDP on `::1`, since S00B took the TCP entry on `::1` out (04 §7.6); goleak clean |
| S30 | 4 | Sessions, login and setup | 03 slices 6–7; 03 §4.6 (key-rotation purge in `New`, the `secrets.rotated` audit row, the `secrets_rotated` alert), §7.4, §7.7 (logout rows), §7.8 | `internal/server/auth`, `internal/server/httpapi` | S14, S24 | 2 | The 03 §15 key-fingerprint tests pass (a changed `session` key deletes the sessions and device tables, a changed `invite` key deletes invites and setup/reset tokens, both write the audit row and raise the alert through a fake `AdminAlerter`); login, rotation and throttle integration tests pass (a hasher counter proves no hashing while throttled); logout and the login-with-an-old-cookie row call `ConnCloser` with that session and the 03 §7.7 reason (fake `ConnCloser`); setup tests pass, including the parallel-complete race |
| S31 | 4 | `serve` in off mode, health, servertest | 04 S4 (server); 04 §6.1–6.4, §11.1 | `internal/server`, `internal/server/servertest`, `internal/server/ops` (health.go) | S09, S17, S25 | 2 | `servertest.Start` is ready in < 1 s; `/healthz` and `/readyz` follow 04 §11.1; a test `fs.FS` SPA and its fallback are served; with only `dist/.gitkeep` the "web app is not built" page shows; `Shutdown` returns in time |
| S32 | 4 | Web Push sender | 04 S12; 04 §14 | `internal/server/push` | S04, S25 | 2.5 | push tests pass: the payload decrypts at a fake push service, SSRF guard table (private addresses, redirects), dedup and rate limits, pruning on 404/410, VAPID fingerprint check |
| S33 | 4 | Auth pages and setup step 1 | 05 W3, W10 (step 1); 05 §14.1 (step 1), §15.1; 03 §7.8 | `web/src/auth/`, `web/src/setup/SetupPage*`, `web/src/setup/index.ts` (exports `SetupPage`; S87 adds `WelcomePage`) | S27 | 2.5 | Component tests (MSW) for each error code on each form; the fragment token is removed before the first request (fetch spy); a logout in tab A redirects tab B; `SetupPage` reads the `/setup` fragment token, calls `setup/check`, and its create-admin form calls `setup/complete`, then navigates to `/` (S87 switches this to `/admin/welcome?step=2`) |
| S34 | 4 | Signaling and room session controller | 05 W4, W5 (controller); 05 §7, §11.1 | `web/src/rooms/` | S20, S27 | 2 | Vitest with a fake socket: the 05 §7.1 states ("Reconnecting…" after 2 s, recovery), stale-build reload exactly once, `RoomSession` join/leave/resync, `room.event` announcements |
| S35 | 4 | Sharer capture and picker | 05 W6 (capture); 05 §13.1–13.3, §13.8 | `web/src/share/`, `web/src/platform/browser/{displayMedia,classify,fakeDisplay}.ts` (`displayMedia.ts` replaces S27's stub) | S27 | 2.5 | Unit tests for `classify` and the `pick()` fallbacks; component tests for ShareSheet and ScreenAudioWarning ("Share without sound", "Pick something else"); the fake-display seam works only on localhost |
| S36 | 4 | Viewer core: subscribe and stage | 05 W7 (core); 05 §9, §10.1–10.5, §12.1 | `web/src/viewer/` | S20, S27 | 3 | Unit tests: `SubscriberPC` maps by `tracks`, adds `stereo=1`, handles `pc.close`, sends `pc.restart{ice}` after 3 s disconnected and `{rebuild}` on failed; component tests for Stage, Tile, ViewerLayout |
| S37 | 4 | Connection-test client | 05 W10 (client); 05 §14.2, §16.5 (provider and NAT rule) | `web/src/conntest/`, `web/scripts/check-i18n.mjs` | S27 | 2 | Unit tests (fake RTCPeerConnection) for the three probes, `ProbeVerdict` and `ConnTestResult`; every provider id of 04 §13.3 has fix text; `codes.json` lists the `ct-` codes; `en.json` has every `fix.firewall.<provider>` and `conntest.nat.<nat>` text, and `check:i18n` fails when a `CloudProvider` or `NATKind` constant has none |
| S38 | 4 | PWA shell: service worker, manifest, update pill | 05 W11 (PWA); 05 §16.1–16.2, §16.4 | `web/src/sw/`, `web/build/sw-plugin.ts`, `web/public/{manifest.webmanifest,icons/}`, `web/src/app/UpdatePill.tsx`, `web/src/platform/browser/pwa.ts` (both replace S27's stubs) | S27 | 2.5 | SW unit tests (`routes.ts` fetch strategy, versioned shell list); the build emits root `sw.js` and `manifest.webmanifest`; the update pill appears when `version.json` changes |
| S39 | 5 | Go signaling client | 01 P6; 01 §10.1–10.2, §15.3 | `internal/client/signal` | S28 | 2 | Against the in-process hub: resume after a killed socket; backoff within its bounds; `Close` makes the server skip the grace period; goleak clean |
| S40 | 5 | Shares and negotiation plumbing (fake media) | 01 P7; 01 §4.4, §8.7–8.11 | `internal/server/signal`, `internal/server/signal/signaltest` | S28 | 3 | Share and role-matrix tests of 01 §19 pass against the fake `MediaPlane`: limits, `ref`, `replaces`, both 30 s share timeouts, `pc.*` routing and ownership, `subscribe.update`/status, `quality.hint`, `caps.update`, stats forwarding, one `ShareStarted` push call per live share |
| S41 | 5 | SFU media path, one layer | 02 slice 6 (part); 02 §9.1, §9.3, §9.7 | `internal/server/sfu`, `internal/server/sfu/sfutest` | S29 | 2.5 | Integration 1 (`TestBasicForwarding`): the first video packet is an SPS, audio flows, the share goes pending → live; `Close` returns within 1 s and leaves no goroutines |
| S42 | 5 | Settings endpoints, invites, registration, approvals | 03 slices 5 (REST) and 8; 03 §7.9, §9 | `internal/server/auth`, `internal/server/httpapi` | S23, S30 | 2 | `GET/PATCH /admin/settings` tests (pinned fields refused); invite, approval and closed-mode integration tests; `signup_pending` reaches a fake alerter |
| S43 | 5 | Admin socket and CLI basics | 04 S8; 04 §12.1–12.2, §12.5 | `internal/server/ops`, `cmd/isshoni` | S31 | 2.5 | testscript tests against an in-process socket with fake auth: `healthcheck` exits 0/1 only, `setup-url --json/--qr/--wait` and exit 7 after setup, `users list --json`, `invite create`, `log-level --for`; peer-credential checks pass on macOS and Linux; `isshoni serve` runs the server (no slice before owned this): `runServe` builds the logger, calls `server.New` and `Run` (S31) with SIGTERM added to the context, and maps the result as 04 §6 says (nil and `ErrShutdownForced` → 0, `NeedsOperator` → 78, `ErrRestartRequested` → 75 until S65 re-execs, anything else → 1); a test starts it in off mode, gets `/healthz` 200 and stops it with exit 0 |
| S44 | 5 | TLS manager and TLS modes | 04 S7; 04 §8 | `internal/server/tlsmgr`, `internal/server`, `internal/server/servertest`, `.github/workflows/ci.yml` | S26, S31 | 3 | A Pebble + challtestsrv CI job issues a domain and an IP certificate; in `servertest` with TLS, WSS (to a stub `/ws` handler; the hub is wired in S54), HTTP/2 and ICE-TCP (a raw RFC 4571 STUN frame; media over 443 is tested in S59) share one port; manual-mode reload tests pass. Manual: a Let's Encrypt staging IP certificate on a VPS |
| S45 | 5 | Room page and shell | 05 W5 (UI); 05 §11.2 | `web/src/rooms/` | S34 | 2 | Component tests: header, people panel, room switcher only when `showRoomList`, `InRoomBar`, root redirect to `defaultRoomId` or `isshoni.lastRoomId` |
| S46 | 5 | Publisher PC and share.start | 05 W6 (publish); 05 §13.4–13.7 | `web/src/share/` | S20, S35 | 2.5 | Unit tests with a fake RTCPeerConnection: `share.start` then a pub offer with `tracks` and `gen`/`neg`; encodings from `ShareParams` with `scaleResolutionDownBy = max(1, sqrt(w·h/maxPixels))`; codec order; presets; stop and browser-stop send `share.stop` |
| S47 | 5 | Viewer focus, audio, watchers, stats core | 05 W7 (rest); 05 §10.3, §10.7, §12.2–12.3 | `web/src/viewer/`, `web/src/lib/stats/` | S36 | 2.5 | Unit tests: auto-focus of the newest share and `replaces` carry-over, audio only for the focused share, batched `subscribe.update`, TapToStart, watchers popover; `window.__isshoni.stats()` returns per-tile stats |
| S48 | 5 | Account pages and the download placeholder | 05 W11 (account, download); 05 §15.2, §15.4, §16.3 (the in-app browser banner) | `web/src/account/`, `web/src/download/` (`DownloadPage`, exported from `index.ts`), `web/src/auth/` (only the in-app browser banner: `InAppBrowserBanner.tsx` and `inAppBanner.ts` with their tests, and the lines that show it on the invite and login pages, 05 §15.1, §16.3) | S33 | 1.5 | Component tests (MSW): session list and revoke, revoke others, password change, delete account, devices list (empty in M1); `/download` renders the 05 §15.4 placeholder, not `PageUnavailable`; the invite and login pages show `InAppBrowserBanner` when `platform.inAppBrowser()` returns a name and nothing when it returns `null` (component tests with `createTestPlatform`); Copy link on the invite page gives `<origin>/invite#<token>` with the page's token (`auth/fragmentToken.ts`), and `location.href` on the login page; a dismissal is kept for the tab (`isshoni.inAppBannerDismissed`) |
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
| S59 | 7 | Wiring v2: SFU in `serve`, itest | 04 W2, 01 P8 (harness); 01 §19 cases 1–3, 9 | `internal/server`, `internal/server/itest` | S13, S39, S44, S50, S52, S54 | 2.5 | In `internal/server/itest` against `servertest` with the real SFU, cases 1–3 and 9 of 01 §19 pass; ICE-TCP via 443 (04 §17): with `servertest.Options{TLS: true}`, `listen.ice_udp = ""` and `listen.ice_tcp = ""`, a tcp4-only Go publisher shares, an `sfutest.Viewer` decodes its media, and WSS signaling runs on the same port; both Go clients' selected candidate pairs are TCP with the TLS listener's port as the remote port; shutdown order is hub, SFU, Transport; goleak clean; an ip-mode server that started not ready because no public address was found asks for its own restart when the periodic detection finds one (`Run` returns `ErrRestartRequested`), and in every other state a changed address is only logged (§6 "Decided after group 6" (1); 04 §6.2, §7.4) |
| S60 | 7 | doctor | 04 S11; 04 §13 | `internal/server/ops/doctor`, `internal/server/ops`, `cmd/isshoni` | S43, S44, S55 | 3 | doctor tests pass (provider table, bandwidth numbers of 04 §13.4); `--list-checks` prints the ids of 04 §13.2; exit 0/5; the typed doctor and bandwidth functions return the JSON of 04 §13.5. Manual: `doctor --json` all ok on a VPS |
| S61 | 7 | e2e harness and CI job | 06 S4; 05 §19.3; 06 §8.2 (e2e job) | `web/playwright.config.ts`, `web/e2e/{global-setup,fixtures,stats}.ts`, `web/e2e/smoke.spec.ts`, `.github/workflows/ci.yml` | S02, S33, S54 | 1.5 | `task e2e` locally and the CI `e2e` job run `smoke.spec` green (global setup finds `ISSHONI_BIN`; each worker's fixture starts it with `deploy/dev/isshoni.e2e.toml` on ports, a data directory and an admin socket of its own, completes setup through `setup-url --json` and logs in, as 05 §19.3 and 06 §7.3 have it); a forced failure uploads the trace, video and `server.log` |
| S62 | 7 | systemd unit, sysctl, firewall, packaging files | 06 S5; 06 §4.5–4.7, §5 | `deploy/systemd/`, `deploy/sysctl/`, `deploy/firewall/`, `deploy/packaging/` | S44, S54 | 1 | `systemd-analyze verify` passes in the existing `lint-deploy` job. Manual: in a Debian systemd container or VM (podman is fine), a hand-placed binary starts as `isshoni`, binds 443 with a test cert, `CapEff` 0x400; the `systemd-analyze security` score is recorded. The automated start check is C1 (S67 locally, S78 in CI) and nightly V1; S62 adds no script and no CI change |
| S63 | 8 | Repair and sync | 02 slice 8; 02 §9.4–9.6 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S57 | 2 | Integration 4, 5 (flash-to-beep offset ≤ 5 ms), 6 and 19 (`TestRTXDuplicates`, with the test option `publish.Options.RedundantRTX`, 02 §15.1), and `rtx_test` pass |
| S64 | 8 | Admin users and password reset | 03 slice 10; 03 §7.10–7.11 | `internal/server/auth`, `internal/server/httpapi` | S58 | 1.5 | Roles and reset integration tests: last-admin and self rules, rename/role/status with the password requirement, reset link issue and complete, admin alerts |
| S65 | 8 | Backup, restore, secret rotation | 04 S9; 04 §5.3, §12.3–12.4, §12.6 | `internal/server/ops`, `cmd/isshoni`, `internal/server` | S14, S43, S54 | 2.5 | Backup/restore round-trip and crash-recovery tests; a newer-schema DB makes `serve` exit 78 and an offline restore of the pre-migration DB brings it back; `rotate-secrets` restarts and invalidates exactly the rotated tokens; `serve` ignores SIGHUP until README S90 builds the reload: a running server that gets the signal goes on serving, and stops with exit 0 on the SIGTERM after it (§6 "Decided after group 7" (1); 04 §6.5, 06 §4.5); a server that waits for its public address looks for it every 30 s, and every other server every 10 minutes as before (§6 "Decided after group 7" (2); 04 §6.2, §7.4) |
| S66 | 8 | Browser e2e: invite, presence, share, watch | 05 W5–W7 e2e, 01 P11; 05 §19.3 | `web/e2e/{invite,rooms,warning,watch}.spec.ts`, `web/e2e/tone.html`; fixes in `web/src/{rooms,share,viewer}/` | S22, S33, S42, S45, S46, S47, S53, S59, S61 | 2.5 | In CI on Chrome: `invite.spec`, `rooms.spec` (two browsers see each other within 1 s; closing a tab removes presence at once; cutting the network keeps it through the grace period), `warning.spec` and `watch.spec` pass; tone-tab capture works under xvfb |
| S67 | 8 | install.sh core | 06 S6; 06 §4.1–4.10, §4.12, §11.1 | `deploy/install.sh`, `deploy/test/`, `tools/relserve/` | S15, S43, S60, S62 | 3 | install.sh unit tests pass under dash, `bash --posix` and busybox; scenarios C1 and C6 pass in a Debian 12 container against a local test release from `tools/relserve` |
| S68 | 8 | Docker image and compose | 06 S8; 06 §6, §11.3 | `deploy/docker/`, `deploy/compose.yaml`, `deploy/compose.host.yaml`, `.github/workflows/ci.yml`, `.github/dependabot.yml` (the docker ecosystem entry) | S25, S43, S54 | 1.5 | `task docker:smoke` (06 §11.3) passes on amd64 in CI and on arm64 on the Mac; a start without a data volume exits 78 with the fix text; `.github/dependabot.yml` has the `docker` ecosystem again for `/deploy/docker` (target-branch as the other entries; it was removed until the Dockerfile existed, because Dependabot fails on a directory without one) |
| S69 | 9 | Codec policy and Firefox | 02 slice 10; 02 §8.3–8.5 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S63 | 2.5 | Integration 12 and 13, `policy_test` and `sdpcheck_test` pass, including the 20 s × 9 rebuild guard |
| S70 | 9 | Audit, janitor, dashboard accounts | 03 slice 13; 03 §4.7, §10 | `internal/server/auth`, `internal/server/httpapi` | S42, S53, S64 | 1 | The audit coverage test (every M1 action), the `accounts` golden JSON and the prune schedule under a fake clock pass |
| S71 | 9 | Push wiring | 04 S12b; 04 §6.6, §14.3 | `internal/server` | S32, S42, S53, S59 | 1 | In `servertest`: a share going live reaches the fake push service for an absent user only; `signup_pending` reaches admins; with `push.enabled = false` REST answers `push_unavailable` |
| S72 | 9 | Browser e2e: focus, unmute, layers | 05 W7–W8 e2e, 01 P11; 05 §19.3 | `web/e2e/{focus-audio,unmute,layers}.spec.ts`; fixes in `web/src/{viewer,rooms}/` | S56, S63, S66 | 2 | `focus-audio.spec`, `unmute.spec` and `layers.spec` pass in CI. Manual: M-IOS-1 and M-AND-1 viewing |
| S73 | 9 | goreleaser and release pipeline | 06 S9; 06 §9; §6 "Decided after group 8" (2) | `.goreleaser.yaml`, `.github/workflows/release.yml`, `deploy/keys/`, `.github/workflows/ci.yml`, `deploy/docker/Dockerfile` (the `# syntax=` line and a comment just below it, never above: 06 §6.1) | S18, S62, S67, S68 | 2.5 | The Dockerfile's `# syntax=` line is pinned by digest like its `FROM` line, and a comment just below it says how that digest is kept current (Dependabot's `docker` ecosystem if S73 confirms that it updates the directive, otherwise by hand with the monthly bump of the tool pins, 06 §7.1) (06 §6.1, §9.2); `release.yml` has no `site` job yet (S82 adds it); `goreleaser-check` asserts the asset list of 06 §3; the release dry run passes; tag `v0.1.0-rc.1` → draft → owner approval → signed → verified → published without moving `:latest` or the latest release. Manual: `install.sh` from that rc on a VPS |
| S74 | 10 | Recovery end to end (Go) | 01 P9; 01 §10.4–10.6, §19 cases 4–8 | `internal/server/itest`, `internal/server/signal`, `internal/server/sfuplane` | S57, S59, S69 | 2.5 | Cases 4–8 of 01 §19 pass in `internal/server/itest`: socket killed mid-share, UDP black-holed 5 s, server restart with `replaces`, codec-blocked viewer, pub PC rebuild; goleak clean |
| S75 | 10 | Protocol compat snapshot | 01 P13; 01 §14.3 | `internal/protocol`, `Taskfile.yml` (the `release:compat` task body only), `.github/workflows/release.yml` | S73 | 0.5 | `task release:compat VERSION=0.0.1` on a scratch branch copies the fixtures into `testdata/compat/<version>/` and prunes to the two newest; the release-job check fails on a missing or stale snapshot; removing a field from a type fails the compat test |
| S76 | 10 | Connection-test probe | 02 slice 14; 02 §7.6 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S69 | 1.5 | Integration 18: per-transport answers, echo channel, one probe per user and transport, `sfu.probe_limit` on the 21st, `sfu.transport_disabled` without a mux |
| S77 | 10 | Web Push flow | 05 W11 (push); 05 §16.3 | `web/src/account/`, `web/src/platform/browser/push.ts` (replaces S27's stub), `web/src/sw/push.ts`, `web/e2e/pwa.spec.ts`, `web/src/rooms/` (only the lines that mount W11's pieces on the room page: S48's in-app browser banner, and this slice's notifications card and Home Screen sheet; 05 §11.2, §16.3) | S38, S45, S48, S61, S71 | 2 | `pwa.spec` passes (manifest parses, SW activates, offline navigation shows the Offline screen); SW tests render every push type from `en.json` and never stay silent; component tests (05 §19.2): the room page shows `InAppBrowserBanner` when `platform.inAppBrowser()` returns a name, and the Home Screen sheet stays hidden until the banner is dismissed. Manual: M-IOS-2 and M-AND-1 push |
| S78 | 10 | install.sh lifecycle and distro matrix | 06 S7; 06 §4.9 (the firewalld offer), §4.11, §4.13, §11.2 | `deploy/install.sh`, `deploy/test/`, `.github/workflows/ci.yml` | S65, S67, S73 | 2.5 | Scenarios C1–C13 pass on all six container distros in CI (upgrade, repair, downgrade, uninstall, purge, firewall offer, port check; C13, the re-run after exit 5, is 06's too, and all 13 are built); the firewalld offer reloads before `--add-service` and asks systemd before `firewall-cmd` (06 §4.9: README S67 built both, so this slice keeps them and adds their tests); `--uninstall` takes the service out of every zone, policy and rich rule before it removes the XML, and firewalld still loads its permanent configuration afterwards (06 §4.11); both are covered by the unit tests of 06 §11.1 with stubbed `systemctl` and `firewall-cmd`, and scenario V3 has them for the nightly's real firewalld (06 §11.2), and as S78 built it also in privileged containers on `fedora-44` and `ubuntu-24.04`, on every PR that touches deploy (06 §11.2) |
| S79 | 11 | SFU observability | 02 slice 12; 02 §13 | `internal/server/sfu` | S76 | 1.5 | After integration 2 a test reads every metric of 02 §13 (non-zero where expected; metrics of later slices are registered now); a log-capture test finds no SDP or IPs at info; `Snapshot` golden test |
| S80 | 11 | Connection-test endpoint | 04 S13 (server); 04 §7.7 | `internal/server/ops`, `internal/server` | S43, S59, S76 | 1 | In `servertest` (off mode) `POST /api/v1/conntest` returns answers that connect for `udp` and `tcp7882` and `transport_disabled` for `tcp443`; a second probe of the same user and transport replaces the first |
| S81 | 11 | Web recovery: resync, re-publish, capture hold | 05 W9 (part), 01 P11; 05 §9; 01 §10.4–10.6 | `web/src/rooms/`, `web/src/share/`, `web/e2e/reconnect.spec.ts` | S66, S74 | 2.5 | `reconnect.spec`: (a) a dropped socket resumes with the same connection id and no new sub offer; (b) 5 s offline shows the banner after 2 s, then clears; (c) after a server restart the viewer decodes the re-published share within 15 s, focused, with audio |
| S82 | 11 | Project site | 06 S10; 06 §10 | `docs/` site pages (not `PLAN.md`, `m1/`), `docs/package.json`, `internal/server/ops/doctor/docsanchors_test.go`, `.github/workflows/{site,ci,release}.yml` (in `ci.yml` the `site` job, with its Go step) | S37, S60, S73 | 2.5 | `vitepress build` passes; `release.yml` gains the `site` job (`uses: ./.github/workflows/site.yml`, after `publish`, with `if: ${{ !cancelled() && needs.publish.result == 'success' }}`: on a tag `sign-dry` is skipped, and a plain condition would skip the job, 06 §9.3) and `actionlint` stays clean; `TestDocsAnchors` covers every doctor id, `ct-` code and provider id and runs in `test-go` and the `site` job; `site.yml` checks that the served `install.sh` equals the latest release asset (sha256); no third-party requests |
| S83 | 11 | Nightly and VM tests | 06 S11; 06 §8.5 | `.github/workflows/nightly.yml` | S68, S73, S78 | 2 | One green nightly with every row: VM matrix with Pebble, multi-arch smoke, cross-OS Go tests, govulncheck, goreleaser snapshot, link check (KVM rows may be skipped with the reason and run once by hand in a local VM) |
| S84 | 12 | Downlink adaptation | 02 slice 11; 02 §9.8, §10.2 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S79 | 2.5 | `allocator_test`, `pacer_test` and integration 16 pass |
| S85 | 12 | Dashboard, metrics and ops routes wiring | 04 S10b; 04 §6.6, §11.2, §11.4 | `internal/server/ops`, `internal/server`, `internal/server/itest` | S55, S59, S60, S70, S79, S80 | 2 | Golden dashboard JSON with fake sources; a failing source does not fail the response; `/metrics` has the SFU and hub series of 06 §12.3, and `isshoni_push_sent_total` once `push.Options.Metrics` is `ops.Metrics.Registerer()` (04 §6.6 "Not wired yet"); in S59's ICE-TCP-via-443 test, `isshoni_sfu_selected_transport{transport="tcp443"}` counts both PCs; dashboard, doctor and bandwidth routes answer 403 to non-admins. As built (04 §6.6 "What README S85 wired"): `POST /api/v1/admin/doctor` raises its own read deadline to 3 minutes, since a doctor run takes up to 2 and the router's 30 s read deadline would end the request's context (04 §9.3) |
| S86 | 12 | Codec wait and quality hints | 05 W9 (part); 05 §10.6, §13.6; 01 §11.7 | `web/src/viewer/`, `web/src/share/`, `web/e2e/hint.spec.ts` | S69, S72, S81 | 2 | Vitest: without H.264 the client polls caps and sends only `caps.update`, never `pc.restart`; `quality.hint` encodings set `active`/`maxBitrate`; `hint.spec`: a `quality.hint{codec}` makes the sharer re-offer and viewers keep decoding. Manual: M-FF-1, M-WIFI |
| S87 | 12 | Setup wizard steps 2–3 | 05 W10 (wizard steps 2–3), 04 S13 (UI); 05 §14.1 | `web/src/setup/`, `web/e2e/setup.spec.ts` | S33, S37, S42, S61, S80 | 2.5 | `WelcomePage` steps 2–3; S33's `SetupPage` now navigates to `/admin/welcome?step=2`; `setup.spec`: admin created; UDP ✓ and TCP 7882 ✓ with the TCP 443 row hidden (off mode); invite link and QR; `setupWizardDone` set; with `listen.ice_udp = ""` the UDP row shows ✗ with the "UDP turned off" text; a non-admin sees no fix text. Manual: a VPS with UDP 7882 blocked shows that provider's text |
| S88 | 13 | Uplink control and layer pausing (cuttable) | 02 slice 13; 02 §11 | `internal/server/sfu`, `internal/server/sfu/sfutest`, `internal/client/publish` | S84 | 1.5 | With `MaxShareKbps` set the pub PC gets a REMB every 1 s; an unwatched layer gets a pause hint after 10 s and resumes with a PLI; `sfu.pause_unwatched_layers = false` turns it off. Merged in group 13 and not cut, with layer pausing on by default (§6 "Decided after group 13" (3), (5)): one REMB per pub PC with the sum of its shares' caps, three REMBs of 1 Gbit/s once the cap goes, `UpdateShare` and `Resync` with the current cap and pause state, a paused layer weighed at its maximum by the downlink allocator (02 §11 "As README S88 built it") |
| S89 | 13 | Load test tool | 02 slice 15 (tool); 02 §15.2 | `cmd/isshoni-loadtest`, `web/e2e/decodable.spec.ts` (not `sfutest` or `publish`, which S88 owns in this group; M1 has no publisher rate control) | S22, S39, S42, S59, S61, S64, S85 | 2.5 | A Go test runs `-scenario smoke` against an in-process server and exits 0 with the JSON report (so it runs in `test-go`); users come from an invite and are deleted afterwards; an unreachable server gives the documented exit code; `decodable.spec` runs `isshoni-loadtest -scenario smoke -decodable -publishers 1 -subscribers 0` (or a `-publish-only` flag) against the e2e server fixture, and a Chrome viewer shows `framesDecoded > 0`. As built (§6 "Decided after group 13" (6), 02 §15.2): `-v`, exit 130 for an interrupt, media that never came is exit 1, `-duration` with the warm-up, the expected egress from the publishers' measured rates, the memory check skipped while the server reports no RSS, `smoke`'s relaxed limits and all-high's knee; `decodable.spec` takes the tool from `LOADTEST_BIN`, which CI's `e2e` job sets to the `build` job's binary (06 §8.2), and otherwise builds it with the Go on the `PATH` (05 §19.3) |
| S90 | 13 | Server hardening | 04 S14; 04 §6.4–6.5, §17 | `internal/server`, `cmd/isshoni`, `web/e2e/tcp-only.spec.ts` | S65, S66, S68, S71, S85 | 2 | SIGHUP reload test; shutdown test (`server.shutdown`, close 1012, REST 503 `server_shutdown`, `Run` returns in time); a log canary finds no secrets, SDP or client IPs at info; `tcp-only.spec` (UDP blocked, off mode, so TCP 7882) plays over TCP; S59's ICE-TCP-via-443 itest (04 §17) still passes with the hardened server; doctor reports ok in the Docker bridge and host setups. Merged in group 13 with every item but the last met; the last is **Manual** since then, on a Linux host with a real address with the exit test (§6 "Decided after group 13" (2); 06 §12.1 pre-flight 5): Docker Desktop and the smoke overrides can't give an all-ok report. As built: §6 "Decided after group 13" (7), (8) |
| S91 | 13 | Admin dashboard, doctor, bandwidth; admin e2e | 05 W12 (part); 05 §15.3 | `web/src/admin/`, `web/e2e/admin.spec.ts` | S49, S61, S64, S70, S85 | 2 | `admin.spec` (create and revoke an invite, approve a pending sign-up, doctor page renders); the dashboard polls every 2 s while visible and stops when hidden; the catalog has `doctor.<code>` for every code of 04 §13.2, `doctor.public_ip.loopback` (README F04, a `skip` without params or fix, which an e2e server reports) and `doctor.udp_buffers.unknown` among them (05 §15.3). As built (§6 "Decided after group 13" (1), (9); 05 §15.3): the `doctor` namespace stayed in `en.json`, and `admin.spec` has a fourth test, the dashboard polling a real server |
| S92 | 14 | Load gate on a VPS | 02 slice 16; 02 §15.2 | `internal/server/sfu` | S88, S89 | 2 | Manual (VPS): `-scenario focus` against a 4 vCPU VPS, run from a second VM, meets every 02 §15.2 criterion; the `all-high` knee goes into the release-notes draft. **Waits for the owner's OK for the two machines** (§6 decision 4; "Owner actions"): not in group 14's parallel run of F19, F20, S00L, S93 and S94's code, nor in group 15's of F21 and S00M (§6 "Decided after group 14" (12)). S88 merged with layer pausing on by default, so S92 runs with it |
| S93 | 14 | Web hardening: overlay, a11y, i18n check, versions | 05 W13; 05 §16.4–16.6 | `web/src/{app,viewer,share}/`, `web/scripts/check-i18n.mjs`, `web/e2e/{a11y,version}.spec.ts`; the group 14 integration extended them, for review S93-R2, to `web/src/rooms/connectStats.ts` and `web/src/rooms/media.ts` (the wiring of the collector's pub source, `attachViewerStats` and the `stats` reports, which S93 wrote and verified as a patch) and the overlay step of `a11y.spec`, which needs that wiring (§6 "Decided after group 14" (10)) | S72, S77, S86, S87, S91 | 3 | `a11y.spec` and `version.spec` pass; a test shows `check:i18n` fails when a key is removed, and the script warns about unused keys (its earlier rules came in S09, S27 and S37); the debug overlay (`stats.watch`) shows per-tile stats with no IPs; `stats` notifications raise the hub's client metrics. Manual: the upload hint under DevTools throttling |
| S94 | 14 | M1 exit test: the collector tool and the run book (code part; split after group 13, §6 "Decided after group 13" (4)) | 06 S12, 05 W14; 06 §12; 05 §19.4 | `tools/exittest/`, `docs/m1/exit-test/` | S90 (the rest of S94's needs gate the test day, the row below, not this code) | 1 | `tools/exittest` builds with the Go standard library only and its tests pass: `collect` samples every 10 s what 06 §12.3 lists (the isshoni PID re-resolved each sample, with a restart row when it changes; the metrics scraped from a fake `/metrics` server in the test), writes `run.csv`, survives the server restarting and the metrics endpoint going away for a while, and stops cleanly on SIGTERM; any summarize or report subcommand 06 §12.3 asks for works on a recorded fixture; `docs/m1/exit-test/` holds the run book as 06 §12 describes it (setup, pre-flight, timeline, measurements, the survey, the pass criteria) ready for the owner, without real names, IPs or domains; `go -C tools test -race ./exittest/...` (or the module 06 names); lint clean. Not in this slice: the device matrix, the test day and the `v0.1.0` tag (the next row) |
| S94-day | 15 | M1 exit test: the session, the device matrix and `v0.1.0` (the owner's; split from S94 after group 13) | 06 §12; 05 §19.4 | the run's results in `docs/m1/exit-test/` | S51, S75, S82, S83, S90, S92, S93, S94 | 0.5 + the session | Manual (the owner): the 05 §19.4 device matrix is filled in; on an rc, after S92's load gate, every pass criterion of 06 §12.3 is met (5 friends, 2 hours, no manual fixes, iPhone and Android watch), with 06 §12.1's pre-flight, its doctor check in Docker included (§6 "Decided after group 13" (2)); `v0.1.0` is tagged from that commit. Not in group 15's run of F21 and S00M: it follows S92, which waits for the owner's OK for its two machines (§6 "Decided after group 14" (12)) |

## 6. Owner decisions (2026-09-30)

The design's open questions were answered by the owner:
1. **Minimum password length: 8 characters**, with the common-password blocklist and throttles (03 §19).
2. **Auto-focus after a manual pick: toast.** Once a friend has clicked a tile, a new share only shows an "Alex
   started sharing [Watch]" toast; the stage and the sound stay on the picked share until it ends. With no manual
   pick, the newest share is focused as before (05 §24).
3. **Cut order if M1 runs late: S51 first, then S88.** A cut slice is removed from every Needs list; if S88 is cut,
   S92 runs without layer pausing.
4. **Test servers: OK to rent when needed.** Before S44/S60/S73/S87 (small VPS with a domain, which later hosts the
   exit test) and S92 (4 vCPU VPS plus a second VM for about a day), the owner is told exactly what to rent and for how
   long, and creates them.

### Decided after group 1 (technical choices the owner delegated)

1. **REST timestamps use the signaling form.** Every timestamp in REST, ops and admin-socket JSON is RFC 3339 UTC
   with exactly three fractional digits and a `Z` suffix, `2026-10-01T12:00:00.000Z`, like 01's (01 §5, 03 §3.3,
   04 §12.2). `api.WireTime` encodes it.
2. **A share that ends while its pub offer is in flight** (a conflict between S29 and S57): 01 §9 rule 4 and §15.4
   win. The hub drops the ended share's TrackRefs and the server still answers. The SFU answers every sending
   m-section without a binding, or bound to a share that isn't the Conn's, with `a=inactive` and ignores it; that is
   no error (02 §6.3, §8.4). `sfu.unknown_track` stays only for malformed bindings (a mid bound twice, the wrong kind,
   a share's second video or audio m-section).
3. **Every foreign-key child column has an index.** The four `ON DELETE SET NULL` references to users that had none
   (`invites.revoked_by`, `password_resets.created_by`, `rooms.created_by`, `users.approved_by`) get one; S14 adds
   them to 03 §5 and `0001_init.sql`, so 03 §15's rule holds with no exemption.

### Decided after group 5 (engineering calls the owner delegated)

Four places where what group 5 built and what the specs said differed, or where the specs left a choice. S00D
wrote each into the spec that discusses it.

1. **Everyone in the room sees who is an admin.** 05 §11.2 gives every participant in the people panel an admin
   badge, but `room.state` only said who is in the room, so S45 could show it on the user's own row alone.
   `ParticipantInfo` gains the additive field `admin` (01 §8.5), which the hub fills from the user's role, and the
   panel shows the badge on every admin. Slice F02 implements it. The badge is a label, not a permission.
2. **A `share.update` from another connection of the user does not reach the publishing client in M1.** The label
   and the preset change in `room.state` and in the SFU, but the hub pushes no new encodings to the connection that
   publishes the share: `HintReasonPreset` is not emitted (01 §8.7, §8.10; 05 §13.5). The M1 web client changes a
   preset only from the page that publishes, which applies the reply itself. The push arrives with the M2 desktop
   handoff, where the publishing connection and the controls are two connections of one user.
3. **The sign-up throttle answers before the registration mode: keep.** In the `invite` and `closed` modes a
   `register` request without an invite takes its `register-ip` token first, so an address that has used its tokens
   up gets 429 `rate_limited` where the mode alone would answer 403 (03 §7.3, §7.9). Throttles come before any
   other work everywhere, and the handler keeps one order for all three modes.
4. **An ip-mode server that finds no public address starts and is not ready; it does not exit 78: keep.** The cause
   is often transient at boot (the network or STUN isn't there yet), and exit 78 is for what a restart can't fix
   (04 §3.2); here a restart once the network is up is the fix. The server stays up: port 80 answers 503 with the
   waiting page, and the readiness checks `public_ip` and `tls` and `isshoni doctor` say what is missing and how to
   fix it (04 §6.2, §6.3, §7.4, §8.2, §8.3, §13.2). It does not recover by itself: the site, the Host check, the
   certificate's name and the ICE rewrite rules are fixed at startup, so an address that the 10-minute detection
   finds later (wired by S54) is only logged, and the operator restarts isshoni (04 §6.2). systemd does not restart
   a running server, and Docker's `HEALTHCHECK` passes meanwhile because it checks liveness. A literal `public_ip`
   that is not a public address stays a config error with exit 78.

### Decided after group 6 (engineering calls the owner delegated)

Three places where what group 6 built and what the specs said differed, or where a rule had no code behind it yet,
and one question that is the owner's. S00E wrote each into the spec that discusses it.

1. **An ip-mode server that started without a public address restarts itself when it finds one.** This amends
   point 4 above, whose "it does not recover by itself" left a server that had only started too early waiting
   for a person. The running process still can't take a site, but a new one can: when the periodic detection
   (every 10 minutes, wired by S54) finds an address on a server that started not ready because none was found,
   the server shuts down gracefully and `Run` returns `ErrRestartRequested`. That is exit 75 until S65 re-execs,
   and systemd and Docker restart a process that exits 75; the new process detects the address at startup, takes
   its site and orders its certificate. In every other state a changed address is still only logged with "restart
   isshoni to apply": a server that has a site is not restarted because an address moved (04 §3.2, §6.2, §6.5,
   §7.4). Slice S59 implements it.
2. **An `agent.send` with `payload: null` is `bad_request`.** As S51 merged it, a `null` payload passed validation
   and was relayed, the one way a client could make the hub send a `null`, which nothing on the wire does
   (01 §5). It is refused like a missing payload (01 §8.14). Slice F03 implements it.
3. **01 §9 rule 10 is enforced, in the hub.** "The server never asks to rebuild a pub PC that carries no live
   share" (the rule's words then; it now reads "whose connection publishes no share", in whatever status the
   share is) had no code behind it: every failed pub PC got a `pc.restart{rebuild}`. Now a failed pub PC whose
   connection has no share that is live or pending (in the hub's words: `starting`, `live` or `stalled`) gets
   none. `sfuplane` keeps passing every failed pub PC on; the hub's sink drops the request (the adapter keeps no
   share list, 01 §15.4), on the connection's actor, in order with whatever starts or ends its shares (01 §9
   rule 10, §10.4, §15.2, §15.4). Slice F03 implements it.
4. **Open, for the owner: picture-in-picture on Android Chrome.** 05 §12.5 says "desktop only" and hides PiP on
   iOS. S56 built it by capability: the button is there wherever the browser reports PiP and the device is not
   iOS, which includes Android Chrome. Until the owner answers, the rule is unchanged and the code stays as built
   (05 §12.5, §24).

### Decided after group 7 (engineering calls the owner delegated)

Four places where what group 7 built and what the specs said differed, or where the integration found a gap, and
two questions that are the owner's. S00F wrote each into the spec that discusses it.

1. **`serve` ignores SIGHUP until the reload exists.** The unit has `ExecReload=/bin/kill -HUP $MAINPID`, and the
   reload itself (manual TLS files and `log.level`) is S90's. As group 7 merged it, `serve` handles SIGTERM and
   SIGINT only, so a SIGHUP ends the process, and systemd, which takes an end by SIGHUP for a clean exit, does
   not start it again: `systemctl reload isshoni` would stop the server. Until S90, `serve` ignores the signal,
   so `systemctl reload isshoni` neither reloads nor stops the server; the unit keeps its line (04 §3.1, §6.5;
   06 §4.5). Slice S65 implements it. S90 replaced the ignoring with the reload in group 13 ("Decided after group
   13" (7)).
2. **A server that waits for its public address looks for it every 30 s.** This amends point 1 of "Decided after
   group 6", where the detection that finds the address looks "every 10 minutes": a server that started a few
   seconds before its network would be not ready for up to ten minutes, longer than install.sh waits (180 s,
   06 §4.10). The periodic detection looks every 30 s on exactly the server that started without the public
   address that is its site, for as long as it waits, and every 10 minutes on every other server, as before
   (04 §6.2, §7.4). The privacy facts change with it: while `public_ip` is `auto`, such a server asks the STUN
   servers every 30 s for as long as it waits, and the outbound table and the `/privacy` page say so (04 §7.4,
   §16; 06 §10.2). Slice S65 implements it.
3. **Doctor's `public_ip` check is `skip`, not `fail`, on a development server.** That is a server with all
   three of: `network.include_loopback` set, no STUN server configured, and a site that is not the server's
   address (off mode, or a domain). `deploy/dev/isshoni.dev.toml` and `isshoni.e2e.toml` are such configs, both
   in off mode. Such a server has no public address by design and is ready without one. Since the group 7
   integration every server runs doctor on itself 20 s after its start, so every `task dev`, and every e2e
   server that lived that long, logged "doctor found problems" for it. On such a server the check is `skip`,
   with the code `public_ip.loopback` and a text that says why. It stays `fail` with only one of the two keys
   set, and, whatever the keys say, where the site is the address (ip mode, and manual mode without a domain):
   that server is not ready until it has an address, and doctor is where its operator reads why (04 §6.2,
   §13.2). Slice F04 implements it.
4. **`ParseEnvelope` meets the 4× allocation bound of 01 §19 for every input.** A fuzz run on the merged tree
   finds, within seconds, frames for which the parser allocates more than four times the input plus the test's
   constant, and measurements find well-formed ones among them, which the parser accepts and has to go on
   accepting: the cause is a long object member name, in valid JSON or not. `Decode` has the same rule and the
   same gap, for a payload from about 28 KB on. The seeds in the repository have no such input, so CI is green.
   The bound is what keeps the cost of a frame in proportion to its size, so it stays, and the parser changes,
   with `Decode`'s way to the JSON library (01 §5, §19). Slice F04 implements it.
5. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 4 of "Decided after
   group 6": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and
   until the owner answers neither changes (05 §12.5, §24).
6. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** 03 §8 says emoji
   are allowed in room names; the PRECIS profile it names refuses the variation selector and a joiner between
   emoji, so the red heart, the keycaps and the profession and family sequences are `invalid`, while an emoji of
   one code point, one with a skin tone and a flag pass. Keep the profile, or let those sequences in: until the
   owner answers, the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 8 (engineering calls the owner delegated)

Two places where what group 8 built went past what the specs said, or where its review left a gap, and the two
questions that are still the owner's. S00G wrote each into the spec that discusses it.

1. **F04's syntax check in `Decode` and the 1024-byte limit on an object key stay**, with the exemption for the
   payload of `agent.send` and `agent.recv`. Point 4 of "Decided after group 7" asked the parser and `Decode` to
   meet the 4× allocation bound of 01 §19 for every input. F04 met it in `ParseEnvelope` with a scanner of its
   own, which needs no limit, and found two more inputs that break the bound in `Decode`: a payload that breaks
   off after a long key, whose `encoding/json` error costs 8 to 14 times its size, and a valid payload with one
   long key that its type doesn't know, which `encoding/json` looks up through a folded copy at up to 5.2 times
   the key. So `Decode` checks a payload's syntax before `encoding/json` reads it, which changes no result, and
   refuses a key of more than 1024 bytes (`bad_request`, `data`/`invalid`), which is the one change to what the
   server accepts: such a payload was decoded with the key ignored. The names of the protocol are under 32
   bytes. The keys inside the relay's payload are the sender's and are kept as bytes, so they are not limited;
   the envelope's keys aren't either, and a frame with a long one is no `bad_message` (01 §5, §13, §19, §23).
2. **The Dockerfile's `# syntax=` line is pinned by digest, like its `FROM` line**, so no build, the release's
   included, pulls a floating Dockerfile frontend from Docker Hub. BuildKit fetches the frontend that the line
   names before it reads the file, and `docker/dockerfile:1` is a tag: the one input of the image that no digest
   fixes. Slice S73 implements it, in the same group as S00G, and says in a comment below the line how the digest
   is kept current (06 §6.1, §9.2, §17).
3. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 5 of "Decided after group
   7": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until
   the owner answers neither changes (05 §12.5, §24).
4. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
   point 6 of "Decided after group 7": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
   allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 9 (engineering calls the owner delegated)

Four places where what group 9 built went past what the specs said, or where its review left a choice, a new
question that is the owner's, and the two that are still the owner's. S00H wrote each into the spec that discusses
it.

1. **Release package file names use `-` where the package's version has `~`.** nfpm names a prerelease's packages
   after their version, `0.1.0~rc.1`, which dpkg and rpm order before `0.1.0`. GitHub renames a release asset with a
   `~` in its name (to `.`), so the draft of `v0.1.0-rc.1` would hold `isshoni_0.1.0.rc.1_amd64.deb` while
   `checksums.txt` lists the name with `~`, and the `sign` job would fail after the owner's approval. The file names
   replace `~` with `-` (`isshoni_0.1.0-rc.1_amd64.deb`, the release's own version), and the package's version keeps
   the `~`, so a package manager still orders an rc before its release (06 §3, §9.2, §17). Slice S73 implements it.
2. **The deb and rpm packages never reload firewalld on install**; only install.sh's firewall offer does. A reload
   drops every runtime-only rule the admin has, and a package asks nobody: its `postinstall.sh` prints the reload
   before `--add-service=isshoni` for the admin to run, and its `preremove.sh` reloads only after it took the
   service out of a zone or policy (06 §4.9, §5, §17). The packages of S62 and S73 work this way.
3. **The Dockerfile's `# syntax=` digest and the goreleaser pin are bumped by hand**, with the monthly bump of the
   tool pins in `.tool-versions`. Dependabot reads neither: its `docker` ecosystem reads only `FROM` lines, and
   goreleaser is built from a module of its own, `tools/goreleaser/go.mod`, which stays out of Dependabot's `gomod`
   directories because its pin follows `.tool-versions` (06 §6.1, §7.1, §17). The comment below the Dockerfile's
   line and `.github/dependabot.yml` say so. This settles what point 2 of "Decided after group 8" left to S73.
4. **A known M1 limit: after `decoder_failed`, a viewer's codec retries start again only on a caps change or a
   reload.** The server rebuilds a viewer's sub PC every 20 s, at most 9 times, while its caps list H.264 and its
   answers refuse it, and then reports `decoder_failed`. A `caps.update` with the same caps starts nothing, and a
   client sends one only when its capabilities change. So a Firefox whose first `hello` already listed H.264 and
   whose OpenH264 download takes longer than those 3 minutes stays `decoder_failed` until a reload, which 05's
   "Reload the page" hint asks for at the same 3 minutes. The release notes list it among the known limits
   (02 §8.5; 05 §10.6; 06 §9.5, §17).
5. **Open, for the owner: the sound when the focused share has none.** S72 made a share without sound never the
   audible one: focus on such a share, automatic or picked, leaves nothing from the stage heard ("audio follows
   focus" taken literally). Before that fix the page was silent too, with a Mute button that muted nothing. The
   alternative is to keep the previous share's sound playing until a share with sound is focused. It is not
   blocking. Until the owner answers, the rule is unchanged and the code stays as S72 built it (05 §12.3, §24).
6. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 3 of "Decided after group
   8": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until the
   owner answers neither changes (05 §12.5, §24).
7. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
   point 4 of "Decided after group 8": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
   allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 10 (engineering calls the owner delegated)

Three engineering calls the owner delegated, one of whose fixes waits for the owner, and the three questions that
are still the owner's. S00I wrote each into the spec that discusses it.

1. **A security review before the first release candidate, and a fix slice for each finding.** A read-only review
   of `main` at f7053dc covered seven attack surfaces (auth, signaling, the SFU, HTTP and ops, push, the web
   client, deploy and release), and every finding was checked by three independent reviewers: six were confirmed
   (three medium, three low) and five refuted.
   - **R1** (medium): a publishing member with a custom DTLS-SRTP client can grow a pub PC's tracks, reader
     goroutines and Pion's receive state without a bound by sending one SRTP packet per fresh SSRC. Slice F07 caps
     the deliveries a pub PC holds (02 §12). **It waits for the owner**: its change is built on its branch, but its
     tests, which need a raw publisher that sends crafted SRTP, are not written, and how they get written is the
     owner's call. It is not merged in group 11.
   - **R2** (medium): a member can loop `POST /me/password` with their own password and keep every hash slot busy,
     until anonymous logins get 503 `server_busy`. Slice F08 adds a `password-change` bucket per user (03 §7.3).
   - **R3** (medium): peer-reflexive candidates bypass the 64-candidate cap, so a member with a PC's ICE
     credentials can grow its remote candidates and pairs from ever new source addresses. Slice F06 lets at most
     64 source addresses per local ICE ufrag reach Pion (02 §12; 04 §7.3, §7.8).
   - **R4** (low): release.yml's `verify` job holds a write token while it runs a privileged container from
     Docker Hub bases that no digest pins, and `publish` doesn't check the draft again. Slice F09, in group 12
     (06 §4.13, §9.3, §17); turning on GitHub's immutable releases is the owner's to consider ("Owner actions").
     F09 merged in group 12 ("Decided after group 12" (8)); its last check is the release dry run on GitHub.
   - **R5** (low): `admin backup` without `--out` writes the archive into a FIFO that another local user put at
     the predictable default name. Slice F09 (04 §12.3), merged in group 12.
   - **R6** (low): one member's `push.test` calls to endpoints that never answer can hold the four shared push
     workers and delay everyone's notifications by about a minute. Slice F08 gives `push.test` a worker of its
     own, attempts a test once, and keeps one subscription per web session (03 §12.4.6; 04 §14.4–§14.5).

   F06 and F08 were built, reviewed and fixed ahead of group 11 and merge in it. F09 is group 12's: no other slice
   touches `release.yml` or `cmd/isshoni` there (S82 has the first in group 11, S90 the second in group 13). R2
   and R6 were gaps in the specs, where the code followed them, and F08 closes them in the specs too; R4 and R5 are
   deviations from 06 §4.13 and 04 §12.3, which F09 brings the code back to. Besides the sections named above, the
   findings are in 02's, 03's and 04's decisions lists and 06 §17.
2. **Case 5 of 01 §19 cuts only the sharer's UDP, and its outage lasts until the share is stalled** (about 7.4 s),
   not exactly 5 s. With 02's timers a 5 s outage can't make a share stalled: the SFU sees its pub PC not
   connected only after Pion's 5 s without a packet, or once the client's ICE restart arrives (2 s plus 3 s), and
   marks the share stalled 2 s later on its once-a-second ticker. S74 built the case that way, and 01 §11.5 B has
   the times as measured in a note below its drawing (01 §11.5 B, §19, §23).
3. **The SFU's sub PCs keep checking candidate pairs for the length of the failed timeout**, so an ICE restart
   that a viewer asks for during its own outage connects once the network is back. With Pion's default the SFU,
   the controlling agent of a sub PC, gives up on each pair after 7 unanswered checks (about 1.5 s), so such a
   viewer came back only through the 15 s rebuild; S74 found it and committed the viewer's case of its test
   skipped. Slice F10 builds it, together with keyframe requests that wait while the share's pub PC is away, so
   that the 500 ms throttle doesn't swallow the request of the pub PC's own reconnect (01 §10.4, §11.5 B, §23;
   02 §7.3, §9.7 and its decisions list).
4. **Open, for the owner: the sound when the focused share has none.** Unchanged from point 5 of "Decided after
   group 9": a share without sound is never the audible one, and the alternative is to keep the previous share's
   sound playing until a share with sound is focused. Until the owner answers, the rule is unchanged and the code
   stays as S72 built it (05 §12.3, §24).
5. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 6 of "Decided after group
   9": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until the
   owner answers neither changes (05 §12.5, §24).
6. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
   point 7 of "Decided after group 9": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
   allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 11 (engineering calls the owner delegated)

Six engineering calls that group 11's slices and its integration made where the specs left a choice or the code
went past them, F07 still waiting for the owner, and the three questions that are still the owner's. S00J wrote
each into the spec that discusses it.

1. **The binding-request limit of README F10 is set for the pub and the sub APIs alike; the probe APIs keep
   Pion's 7.** Only the controlling agent needs it, which the SFU is on every sub PC: a controlled agent revives a
   given-up pair through its triggered checks, as the sharer's pub PC did with Pion's 7 in S74's test. But Pion's
   role follows the negotiation (an `a=ice-lite` offer, a 487 role conflict); on a controlled agent the setting
   only makes the SFU's own checks of a silent pair go on longer, at the same pace; and the pub and sub APIs keep
   one `SettingEngine`. The probe APIs set Pion's 7 back: a probe gains nothing from more, and with the 40 of its
   8 s failed timeout each of up to 64 candidates of a probe offer, which any signed-in user may post 12 times a
   minute, would get 41 checks instead of 8, about five times the outbound STUN that S76 bounded (02 §7.3 and
   its decision 3).
2. **Pion's info lines are debug lines, in the SFU and in netx.** README S79 gave the SFU a Pion logger on top of
   logx's bridge (`pionlog.go`): Pion's info lines, which repeat without ids what the SFU logs itself, and its
   warnings and errors that hold an IP address are logged at debug. The group 11 integration did the same for
   netx's mux logger, whose info lines ("Listening TCP on <ip>:<port>", "Added connection: tcp remote …") named
   addresses at info whenever the log level was debug; its warnings were debug lines already. So at info and above
   no line of Pion's names an address, and the SFU's log capture test checks every component (02 §13 and its
   decision 5; 04 §7.3, §10 and its decision 10).
3. **The project site deploys only once GitHub Pages is on.** `site.yml` asks GitHub whether Pages is on (with the
   Source "GitHub Actions"; one that builds from a branch fails the run) and deploys only then. Before the first
   release a run without Pages stays green, with the site built and checked and a warning, so `main` doesn't go
   red over the owner's one-time setting; once a release exists, a run on `main` or a release tag that did not
   deploy fails in its `check` job, release.yml's call included, because the stable install URL would serve
   nothing or an old installer (06 §10.5, §17; "Owner actions" below).
4. **A share published again on a kept connection goes onto a new pub PC.** When the first `room.state` after a
   resumed `welcome` no longer lists the page's share (it ended while the socket was down), the page publishes it
   again with `replaces` (01 §10.5) on a new pub PC of the next `gen`, which the server takes in place of the one
   it kept, like a rebuild (01 §9 rule 2), with the connection's other shares on it. S81 first put the share's new
   transceivers on the kept PC with the old ones stopped; Chrome's re-offer then rejects the BUNDLE group's first
   m-section and moves the group to a new transport, which the SFU's PC didn't follow (it went back to
   `connecting` and failed 20 s later, `reconnect.spec` (d)) (05 §11.1, §13.6, §24).
5. **The nightly departs from 06 twice, the first time only until README F13.** (a) Its VM harness is inline in
   the `distro-vm` job, not a script in `deploy/test/vm/` (06 §4.13): S83 could change only `nightly.yml`. It
   sources `deploy/test/distro-test.sh` without its last line and relies on the library's names, so a rename there
   fails only the nightly (loudly: the step checks the last line first), and the harness can't be run by hand
   before a release without being taken out of the YAML. README F13 (group 12) moves it to `deploy/test/vm/` with
   a guard in `distro-test.sh` and `lint:sh` over it. (b) `goreleaser-full` doesn't sign (`--skip=sign`): cosign's
   keyless signature would need an OIDC token in a job that also runs npm, which release.yml keeps apart, and a
   public transparency-log entry every night; release.yml's dry run covers the signing, and the asset check takes
   an empty stand-in for the cosign bundle (06 §8.5, §11.2, §17).
6. **The connection test answers a missing or unknown transport with 422 `validation_failed`** (`fields.transport`
   `required` or `invalid`), 03's rule for a bad body field, which 04 §7.7's list of errors lacked; it maps 02's
   `sfu.bad_sdp` to 400 `bad_sdp` and `sfu.closed` to 503 `not_ready`; and only the requests that get past the
   transport checks count toward its 12 a minute. The SPA never sends an unknown transport (04 §7.7 and its
   decision 11).
7. **Still waiting for the owner: README F07** (R1 of the security review, point 1 of "Decided after group 10").
   Unchanged: its change is on its branch, its tests (a raw publisher that sends crafted SRTP) are not written, and
   how they get written is the owner's call. It is not in group 12. README F06 added one option for it, not built:
   a per-PC cap on the SRTP and SRTCP read streams that Pion keeps for every undeclared SSRC (02 §12).
8. **Open, for the owner: the sound when the focused share has none.** Unchanged from point 4 of "Decided after
   group 10": a share without sound is never the audible one, and the alternative is to keep the previous share's
   sound playing until a share with sound is focused. Until the owner answers, the rule is unchanged and the code
   stays as S72 built it (05 §12.3, §24).
9. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 5 of "Decided after group
   10": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until the
   owner answers neither changes (05 §12.5, §24).
10. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
    point 6 of "Decided after group 10": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
    allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 12 (engineering calls the owner delegated)

Eight engineering calls that group 12's slices and its integration made where the specs left a choice or the code
went past them, what the group did outside its listed paths, four follow-up slices, F07 still waiting for the owner,
and the three questions that are still the owner's. S00K wrote each into the spec that discusses it.

1. **Retransmissions are paced, ahead of new media** (README S84; the question S63 left open). A video
   retransmission goes through its DownTrack's token bucket like new media, and the writer serves the RTX queue
   first: a repair takes its tokens before new media does and never waits behind a frame, and a large frame that a
   viewer asks for again leaves spread like the frame itself, not as a second burst on top of the one that was lost.
   The retransmission budget bounds how much of the rate repair may use; the pacer, how fast any of it leaves.
   Audio and its retransmissions are never paced (02 §9.5, §9.8 and its decision 6).
2. **The downlink allocator reads loss as the link lost it** (README S84). From a viewer's receiver reports it
   takes out the own sequence numbers the SFU never sent (an upstream gap, a packet a queue dropped); a report more
   than 2 s after the last of its stream covers an outage of the viewer's network, not congestion, and only starts
   the count again; a report is evidence about the allocator's last decision only when all of its interval came
   after it; and a verdict needs 20 packets. Reports less than 200 ms apart are folded together and at most 1024
   are kept, so a viewer that floods them costs no more than one that reports five times a second, and a REMB past
   100 Gbit/s is ignored (both from S84's review). Since receivers count the packets that RTX brought back as lost,
   10 % of the video lost and repaired is congestion, so integration 4 (`TestNACKFromCache`) now loses 5 % (02 §9.3,
   §10.2, §12, §17 and its decision 7).
3. **Two rules of the allocation, and what integration 16 shows** (README S84). A share without a smaller layer
   (single-layer, or only its preview layer) is never capped: a cap would leave its viewer with no video. While the
   allocator is limited, any change to a subscription's request decides every cap again, newest `high` first, which
   can also give an older request its full layer back; the design only said that a new `high` is served in full
   only if it fits. And with `rembsim` as 02 §17 specifies it, integration 16's first trial upgrade fails on a
   falling REMB within about 250 ms, before a receiver report shows the loss that 02 §10.2 expected it to fail on;
   either fails a trial, and the test accepts both and logs which (02 §10.2, §17 and its decision 8).
4. **The dashboard's own alerts and its sources** (README S85). `dashboard.source_failed` is `warn` with
   `{source}`, whose values `server`, `process`, `rooms`, `media`, `transfer`, `update`, `doctor` and `accounts` are
   wire values that 05 renders; `public_ip.changed` is `warn` with `{family, from?, to}`, one per address family;
   `tls.renewal_failing` is `{code, notAfter}`, `warn` while the certificate is still served and `error` once it
   has expired or isn't, in auto and ip modes only; `doctor.fail` is `error` with `{fail, checks}`. A source fails on
   an error, a panic, or a second that ran out, and that second is cooperative: it reaches the source only through
   its context. A client is `outdated` below the server's own version too, and a share's `codec` is `""` while the
   SFU doesn't know its profile (04 §11.4 and its decision 12; 05 §15.3 for README S91).
5. **Three choices of the ops wiring** (README S85). An `updates.release_url` that the release check would refuse
   is a config error, exit 78, with its user info redacted (`ops.RedactURL`), not a start failure; the transfer
   counters' last flush is a shutdown step of its own, `transfer`, right after `tls` inside step 6's 2 s; and
   `listen.http`'s bytes count as `web`, through a counting listener in `internal/server`. With them the ready line
   names the metrics listener (`listen_metrics`), and `POST /api/v1/admin/doctor` raises its read deadline to
   3 minutes, because a doctor run takes up to 2 (04 §4.5, §6.1, §6.4, §9.3, §11.3 and its decision 13).
6. **The codec wait is the page's, and keeps its 3 minutes across a gap under 30 s** (README S86). A status that
   flips while the server rebuilds can't put off the "Reload the page" hint for ever. There is no toast: the tile
   and the stage say it where the video would be, without promising sound (a thumbnail's audio is off, a share may
   have none), and a room banner asks for the reload after 3 minutes, with a text of its own for a browser that
   isn't Firefox, or says that a browser without H.264 can't play isshoni video, once a subscription has the reason
   `codec`. `caps.update` goes out at most once per 5 s (05 §10.6, §24; 01 §11.7).
7. **"Done, go to the Lounge" opens `/r/lounge`** (README S87's review), as its label says, and not `/`, whose root
   redirect opens the last room: an admin who came back to the wizard from another room landed there. **The invite
   card's texts stay in the lazy `setup` namespace**, and a folder that shows the card imports `i18n/lazy/setup` in
   its entry module (the card did it itself at first, which left `check:i18n`'s own test unable to fail) (05 §11.2,
   §14.1, §24).
8. **Review R4 and R5 are fixed by README F09, with three departures from 06 that 06 now has.** `verify`'s rootless
   container also takes `--cap-add SYS_ADMIN` (its own, inside its user namespace: systemd needs it for the unit's
   sandbox) and `--network slirp4netns`, `relserve` listens on every address, and a step first makes sure that the
   runner's user has a systemd user session; `sign` checks the draft as strictly as `publish` and hands `verify`
   the files it signed as the artifact `release-signed`. The digests F09 pins, the `verify` bases and the BuildKit
   image, are bumped by hand with the tool pins, like the Pebble images that F13 moved into `vm-test.sh` (06 §4.13,
   §7.1, §9.3, §17; 04 §12.3 and its decision 9). F09's last acceptance item, the release dry run on GitHub, waits
   for the push ("Owner actions").
9. **What group 12 did outside its listed paths, accepted at the merge.** README F12 added one line to
   `web/src/rooms/runtime.ts` (`signal,` in `connectStats({...})`: the debug handle has no other way to the tab's
   `SignalClient`; 05 §10.7). README S86 added its `viewer.*` keys to `web/src/i18n/en.json`, as the shared-files rule
   of §5 allows. README F13 changed two comments of `nightly.yml` beside its `distro-vm` steps, which pointed at the
   inline harness it replaced. README S84's `internal/server/sfu/sfutest/rembsim` is a subpackage, outside `sfutest`'s
   directory, and 02 §4 asks for it. The integration itself fixed `cmd/isshoni/serve_test.go` (S85 removed the warning
   `TestServeLogger` checked), `web/src/auth/fragmentToken.test.tsx` (setup now goes to `/admin/welcome`),
   `Taskfile.yml`'s `test:sh` (it runs `vm-test.sh -h`, 06 §7.2), and comments in `ci.yml` and `.github/dependabot.yml`;
   and it wrote F12's and F13's spec text (01 §16, 05 §10.7, §19.3; 06 §4.13, §8.5, §9.3, §17).
10. **Four follow-up slices in group 13**, for what group 12's reviews found outside the paths of the slice they
    reviewed: README F15, `publish`'s draft check as `deploy/release/check-draft.sh` with a committed test under
    `task test:sh` (F09-R1: F09's fixture runs were one-off); README F16, `distro-test.sh`'s podman path with what
    systemd needs for its sandbox (F09-R4); README F17, the account menu's "Invite friends" opening S87's invite card
    in a dialog, and the e2e debug state typed (S87's and S86's carry-overs); README F18, `stunUfrag` reading the
    ufrag without decoding the whole STUN message, which F14 measured at about 43 bytes allocated per input byte.
11. **Still waiting for the owner: README F07** (R1 of the security review, point 1 of "Decided after group 10").
    Unchanged: its change is on its branch, its tests (a raw publisher that sends crafted SRTP) are not written, and
    how they get written is the owner's call. It is not in group 13.
12. **Open, for the owner: the sound when the focused share has none.** Unchanged from point 8 of "Decided after
    group 11": a share without sound is never the audible one, and the alternative is to keep the previous share's
    sound playing until a share with sound is focused. Until the owner answers, the rule is unchanged and the code
    stays as S72 built it (05 §12.3, §24).
13. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 9 of "Decided after group
    11": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until the
    owner answers neither changes (05 §12.5, §24).
14. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
    point 10 of "Decided after group 11": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
    allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 13 (engineering calls the owner delegated)

Ten engineering calls that group 13's slices and its integration made where the specs left a choice or the code
went past them, what the group did outside its listed paths, two follow-up slices, F07 still waiting for the owner,
and the three questions that are still the owner's. S00L wrote each into the spec that discusses it.

1. **Doctor's catalog stays in `en.json`** (README S91; review S91-6, decided at the integration). Only `admin/` uses
   the `doctor` namespace, so the shared-files rule of §5 would give it a lazy file of its own, but the move needs
   `lazy/doctor.ts` and changes to the two tests that pin the lazy namespaces to `account`, `admin` and `setup`,
   outside S91's paths, and 05 §16.5 allows a namespace that could be lazy to stay in `en.json`. The initial JS is
   168.5 of 200 KB with it (about 6 KB of it doctor's). Making it lazy later is a five-file change outside `admin/`
   (`lazy/doctor.en.json`, `lazy/doctor.ts`, the import in `admin/index.ts`, and the pinned lists in
   `i18n/index.test.tsx` and `check-i18n-lazy.node.test.ts`) (05 §15.3, §16.5, §24).
2. **"doctor reports ok in the Docker bridge and host setups" becomes a manual check** (README S90's last acceptance
   item, which failed on this Mac for reasons of the environment): it runs on a Linux host with a real public
   address, with the exit test (06 §12.1 pre-flight 5; README S94-day (the run book is S94's)). Docker Desktop's
   user-space NAT makes `public_ip` symmetric for every config and its fake-IP DNS answers for the test domain, its
   VM's clock and UDP buffers warn, and on a Linux runner the smoke overrides' test domain resolves nowhere; every
   check of the server itself was `ok` or `info`. A later slice that owns `deploy/docker/test` and
   `internal/server/ops/doctor` may run `isshoni doctor --json` in CI's Linux docker-smoke and assert per code, and
   review two rules of `public_ip`: it calls a Docker bridge "symmetric" although published ports don't depend on
   the outbound NAT kind, and it takes a configured literal `127.0.0.1` for the public address (04 §13.2, §17;
   06 §17). That later slice is README F21, in group 15 ("Decided after group 14" (11)).
3. **Layer pausing is on by default** (README S88, cuttable and not cut). Every Go test and the whole e2e suite ran
   green with it in the group 13 integration; `sfu.pause_unwatched_layers = false` turns it off (02 §11 and its
   decision 9).
4. **README S94 is split.** Its code, `tools/exittest` and the run book in `docs/m1/exit-test/`, is built in group 14;
   the exit test itself, 05 §19.4's device matrix and the `v0.1.0` tag (the row `S94-day`) stay with the owner, after
   README S92's load gate on a VPS (06 §16, §17).
5. **The admin's cap and layer pausing as README S88 built them** (02 §6.1, §6.2, §8.6, §9.1, §9.7, §10.2, §11; 01
   §8.7, §8.10). The cap is **one REMB per pub PC**, the sum of its shares' caps naming all their video SSRCs, and
   none while one of them has no cap, because Chrome applies a REMB's bitrate to everything the PC sends; once the
   cap is gone the PC gets three REMBs of 1 Gbit/s, never one of 0. A hint goes out whenever the cap or the pause
   state changes (`admin` also when `f`'s encodings stay the same, and for a pending share). The full layer is paused
   10 s after the last request for `high` ended (counted from `StartShare`, or from the first look that found it
   gone), never for a share without a preview layer, and resumes at once on any new request for `high`, also from a
   viewer whose decoders can't take the video; keyframe requests for a paused layer do nothing, so the PLI after the
   resume goes out; the downlink allocator weighs a paused or just resumed layer at its maximum (review S88-R1).
   `UpdateShare`'s encodings carry the current cap and pause state, and `Resync` rebuilds the last hint from the
   current cap, pause state and preset; `UpdateShare`'s are not taken as told to the sharer (review S88-R4).
6. **`isshoni-loadtest` as README S89 built it** (02 §15.2, §17; 05 §19.3; 06 §8.2). It adds `-v` and exit 130 (an
   interrupt; the accounts are still deleted, and a second signal quits at once); media that never came is a
   failure, exit 1, not a setup error; `-duration` includes the warm-up; the expected egress comes from the
   publishers' measured rates, with `receivedMbps` as a check from the other end; the memory check is skipped while
   the dashboard reports `rssBytes` 0; seq and ts continuity are checked at every switch once repairs had their
   chance; `smoke`'s limits are relaxed (loss < 2 %, egress 50–200 %, PLI ≤ 1/s and ≤ 20 in 10 s, switch p95 < 3 s,
   A/V p99 ≤ 20 ms; CPU and memory reported only); all-high needs a subscriber and stops at a knee of unrecovered
   loss over 2 % or average CPU over 90 %, or when nothing is left to raise. The group 13 integration gave CI's e2e
   job the tool from the `build` job as `LOADTEST_BIN` (`CGO_ENABLED=0 go build -trimpath`), so `decodable.spec`
   doesn't build it with an unpinned Go; it is no release artifact.
7. **The reload as README S90 built it** (04 §3.1, §4.1, §6, §6.5, §8.4; 06 §4.5). SIGHUP loads the config again
   from `serve`'s flags, its environment and the file (§4.1's precedence holds), reads the manual TLS files again and
   applies `log.level` (a level set with `admin log-level --for` stays until its time is up); every other changed
   key is named in one WARN line for a restart, never with its value. A config with errors changes nothing and names
   no key (review F2); the certificate files are read without the lifecycle lock, so a hung read never holds up a
   shutdown (review F3). `Server.Reload` is a new public method.
8. **The server's hardening besides the reload** (README S90; 04 §7.8, §8.5, §9.3, §10, §17). `limits.conns_per_ip`
   applies to off mode's `listen.http` too, except for peers inside `network.trusted_proxies`; the 503s the server
   gives on purpose (a polled `/readyz` or `/healthz` while not ready, the shutdown gate's) are debug lines; a safety
   net in `internal/server` redacts an ICE password in the text of a library's message (Pion's ICE agent logs
   `remotePwd` at debug), which README F20 then did in logx's Pion bridge in group 14, leaving the net as a second
   layer ("Decided after group 14" (1)); Go's `log` package is routed into the
   server's log as the component `stdlog`. The log canary allows Pion's own warnings that name no address (two come
   with every PeerConnection, at info as well, so an operator sees them in the journal) and holds them to the same
   rules, allows client addresses at debug, and at info and above allows a client's IP only as the `remote_ip` of a
   security event (review F4, F5).
9. **The Dashboard and Doctor pages as README S91 built them** (05 §15.3). The pages' texts are `admin.dashboard.*`
   and `admin.doctor.*` in the lazy `admin` namespace; doctor's catalog is `doctor.<code>`, `doctor.fix.<fixCode>`
   and the fragments `doctor.part.*`, with the CLI's variants as contexts and plurals of the code's key; a fix is a
   list of steps, each a sentence and maybe a command to copy. A section whose source failed says "Not available",
   not zero values; the certificate's last error shows doctor's fix for its code; `tls.renewal_failing` tells an
   expired certificate from one no longer served; wire values have names; the connection test's card is no second
   landmark of its name. `admin.spec` has a fourth test, the dashboard polling a real server.
10. **The account menu's invite entries as README F17 built them** (05 §11.2, §14.1). Members with
    `createInvites` get "My invites" (a link to `/admin/invites`), which they would otherwise have lost when "Invite
    friends" became a dialog; nobody gets "Invite friends" while registration is closed, which the menu reads from
    `/info` when it opens (members get no `admin.settings` invalidation); after 403 `registration_closed` or
    `forbidden` the dialog offers no "Try again", and marks `['info']` or `['me']` stale.
11. **What group 13 did outside its listed paths, accepted at the merge.** README F15 renamed `publish`'s checkout
    step and added `deploy/release` to its sparse checkout (without it the script isn't there; F15-R3, and its row
    says so now) and edited `test:sh`'s `desc` in `Taskfile.yml`. README F17 added its `room.menu.myInvites` and
    `room.invite.*` keys to `en.json` (its own namespace, as the shared-files rule allows), the "My invites" entry,
    `aria-haspopup="dialog"` on "Test my connection", and changed `rooms/RoomHeader.test.tsx`, whose tests expected
    the old link. README F18 committed five seed files under `testdata/fuzz/FuzzStunUfragDecode` and lowered
    `FuzzFirstFrameGate`'s allocation bound in its test file. README S89 added `golang.org/x/term` v0.47.0
    (BSD-3-Clause; only the tool links it) and the file `client.go`. README S91 put the `doctor` namespace into
    `en.json` (decision 1). The integration itself changed `.github/workflows/ci.yml` (the loadtest artifact, S89-R2;
    the distro job's comment on what `distro-test.sh`'s podman path leaves to its caller, F16-R4), 06 §7.2, §8.2, §13
    and §17, and this page (F15's Touches, the module table).
12. **Two follow-up slices in group 14**, for what group 13's reviews found outside the paths of the slice they
    reviewed: README F19, an optional key for `InviteLinkCard`'s "once" text, so a member's invite dialog points to
    "My invites" and not to "Admin → Invites" (F17-R2); README F20, logx's Pion bridge redacting ICE passwords
    itself and logging its "suppressed" line at the level of the suppressed lines, and `ops.Transfer`'s first tick
    logging no WARN when a server stops right after it started (S90's blocked items). Both merged in group 14
    ("Decided after group 14" (1)–(3)). F17-R3 (`ViewerSignal` as `Pick<SignalClient, 'on' | 'notify'>`,
    `canSend` and its debug branch gone) is a carry-over for README S93, which owns `web/src/viewer/` in group 14;
    S93 did it (05 §10.6). Done in group 15: decision 2's doctor check in CI (README F21). Not scheduled yet:
    `sign` and `sign-dry` calling `check-draft.sh` in a mode without the signature check (F15's note);
    `task e2e` building `isshoni-loadtest` and passing `LOADTEST_BIN` (S89's option; locally the spec builds the
    tool itself).
13. **Still waiting for the owner: README F07** (R1 of the security review, point 1 of "Decided after group 10").
    Unchanged: its change is on its branch, its tests (a raw publisher that sends crafted SRTP) are not written, and
    how they get written is the owner's call. It is not in group 14.
14. **Open, for the owner: the sound when the focused share has none.** Unchanged from point 12 of "Decided after
    group 12": a share without sound is never the audible one, and the alternative is to keep the previous share's
    sound playing until a share with sound is focused. Until the owner answers, the rule is unchanged and the code
    stays as S72 built it (05 §12.3, §24).
15. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 13 of "Decided after group
    12": the rule says "desktop only", the code offers PiP wherever the browser reports it outside iOS, and until the
    owner answers neither changes (05 §12.5, §24).
16. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
    point 14 of "Decided after group 12": the PRECIS profile that 03 §8 names refuses them, the rule says emoji are
    allowed, and until the owner answers the rule is unchanged and the code stays as S53 built it (03 §8, §19).

### Decided after group 14 (engineering calls the owner delegated)

Nine engineering calls that group 14's slices and its integration made where the specs left a choice or the code
went past them, what the group did outside its listed paths, one follow-up slice, F07 and the load gate still
waiting for the owner, and the three questions that are still the owner's. S00M wrote each into the spec that
discusses it. The numbering of "Decided after group 13" above stays as S00L set it, because rows refer to it.

1. **logx's Pion bridge redacts ICE passwords itself, and its limiter's lines have the level of the lines they
   stand for** (README F20; 04 §10, §17 and its decision 18; 02 §13). In every line it writes, at every level, the
   bridge replaces a named password (a name ending in `pwd`, `password`, `passwd` or `credential`, or their
   plural, then `:` or `=`) and, in a ufrag:password pair, a part of 22 or more ICE characters after one of 4 or
   more. The pair rule leans toward redacting: after a scheme of 4 or more letters, a URL's `//`, host and path,
   up to the next colon or the first dot or other character that is not an ICE character, are one part, redacted
   when it has 22 or more characters (`http://localhost/api/v1/rooms/abc` becomes `http:[redacted]`). README S90's
   net in `internal/server/logsafety.go` stays as a second layer (the integration rewrote its header comment). The
   "further messages suppressed" notice comes at the level of the line that was dropped, and once more in the same
   minute for a dropped line of a higher level; "N messages suppressed" comes at the highest dropped level. A
   design point stays as it is: the limiter has one budget per scope across levels, so at `log.level = debug`
   Pion's debug lines can use it up and a warning is dropped, which the notice then announces at warn. logx's tests
   now run a real pion/ice agent, so the package has `goleak.VerifyTestMain` (review F20-R2).
2. **A transfer tick that the stop cuts short is a normal stop** (README F20; 04 §11.3 and its decision 19). A
   cancelled context logs one debug line and skips the alert check, and `loadSent` and `rememberSent`, which the
   alert check calls and which the same race made warn, stay quiet for it as well; `Run`'s last flush stores the
   bytes. A store call that times out while the server runs (`context.DeadlineExceeded`) still warns. The debug
   line avoids the prefix "transfer accounting:", on which `internal/server`'s `TestTransferAccounting` fails.
3. **The invite card's "once" text as README F19 built it** (05 §11.2, §14.1, §24). `InviteLinkCard` takes an
   optional `onceKey`, default `setup.inviteCard.once` ("under Admin → Invites"); `InviteDialog` takes `me` from
   `AccountMenu` as a required prop, so the compiler finds a caller that leaves it out and the dialog makes no
   `['me']` query of its own, and passes `room.invite.once` ("…under My invites in your account menu…") when the
   user is no admin. `check:i18n`'s rule 1 doesn't check a key given as a prop, so the tests assert the member's
   full text; its rule 4 counts the key's string literal as a use (review F19-R1).
4. **The stats' consumers and the debug overlay as README S93 built them** (05 §10.7, §12.7; reviews S93-R2,
   S93-R4). `attachViewerStats` (`viewer/statsLink.ts`) runs the tiles' freeze watch, the `stats` notification every
   10 s (its `intervalMs` the time since the report before) and the overlay's feed with `stats.watch`; `viewer.debug`
   (`debugStats.ts`) holds the overlay's state; `share/stats.ts` lists the page's pub PC as the collector's second
   source, through a module-level set that `PublisherPC` joins while it has a PC, because the publisher is in a chunk
   of its own. `?debug=1` shows the overlay on that page only and leaves `prefsStore.debug` alone, so a shared link
   doesn't leave the 1 s sampling and the server's `stats.watch` on for every later visit; only `Shift+D` and the
   overlay's close button change the preference. With the wiring the hub's `isshoni_client_*` metrics rise (the
   integration saw `isshoni_client_frames_decoded_total` at 224 in a temporary run), which the exit test reads.
5. **`check:i18n`'s unused-key warnings as README S93 built them** (05 §16.5, §24; review S93-R3). A message
   counts as used through a literal key, a plural base, the key written out as a string anywhere, the written head
   of a key built at run time, what rules 2 and 3 require, `$t()` nesting and `READ_WHOLE = ['push']`. A string
   that ends in a dot covers keys only below a namespace (a lone `'room.'` is the audit page's action group), and a
   template head that is a namespace alone covers all of it, with a note that the rule can't check there. The
   warnings and their count go to stderr and never fail the check; the summary line stays as it was, since a test
   of the connection test's texts reads its end. The two keys it warns about, `common.tagline` and
   `common.loading` (S09's scaffold), are left to the `common` namespace's owner.
6. **The announcements and the two e2e specs as README S93 built them** (05 §16.6, §19.3). "Now listening to …" is
   polite and only for a share that was already there, because a new share's `room.event` is announced at the same
   moment; "You're live" (when a share goes from starting to live, not when it is live again after a reconnect) and
   "Sharing stopped" (when a share that went live ends without a toast or an alert of its own) are assertive.
   `a11y.spec` checks every page in the light and the dark scheme, under reduced motion and after the computed colours
   stood still, and checks the debug overlay. `version.spec` follows 05 §16.4, not the old §19.3 row: after the one
   reload the page runs on with a warning, and VersionMismatch comes through `protocol_unsupported`, including a
   Reload that didn't help (S93's note).
7. **The exit test's collector and summary as README S94 built them** (06 §12.3, §16, §17). `cpu_pct` is in percent
   of all CPUs, and `run.csv` has the machine's CPU beside it (`cpus`, `host_cpu_pct`), because "Server CPU" can be
   read either way; the verdict uses isshoni's own. The UDP counters add `/proc/net/snmp6`'s `Udp6*`; CPU ticks use
   USER_HZ 100 (no `sysconf` without cgo); each scrape is saved whole under `<out>-metrics/` and its series become
   a `scrape` row, so `summarize` reads one file. The flags beyond 06's command line keep its defaults: `-unit`,
   `-pid-file`, `-nic`, `-metrics-dir`, `-proc`, `-interval`, `-scrape-interval`. Where the pass criteria are not
   numeric the summary applies its own rules: T+0 is `-t0` or 60 minutes before the first restart; 2 minutes of
   slack at each end of the session; a restart within 5 minutes of T+60 is the scripted one and a second is a
   failure; goroutines within 10 % of the T+85 baseline; viewing quality fails only at 3 % frozen, every device
   together; ICE-TCP fails only without any `tcp443` PeerConnection; recovery stays manual, with the outage as a
   range. A row passes only on what was recorded (reviews S94-F2 to F5).
8. **The run book's journal and files** (06 §12.3; reviews S94-F1, F6, F7, F8). journald stores isshoni's JSON
   lines at one priority, so the excerpt filters on the JSON `level`, and `-p warning` only gives systemd's own lines
   about the unit. The collector runs under `systemd-run --collect`, so a failed start can be run again under the
   same name. The binary is built into the ignored `bin/`, and the recording is copied into the ignored `.dev/` or
   outside the checkout. The results template's T+0 row asks only C and D for their times.
9. **A Linux container over a checkout set up on a Mac runs the root module's tests** (06 §7.4; the group 14
   integration). `.bin/` holds darwin binaries there, so `go -C tools test ./...` fails `tools/notices`'
   `TestGoLicensesGate` with "exec format error"; `go -C tools test ./exittest/` and the other `tools/` packages
   run. CI builds `.bin/` on Linux.
10. **What group 14 did outside its listed paths, accepted at the merge.** README F19 changed one line of
    `rooms/AccountMenu.tsx` (`me={me}` on the dialog) and added an assertion to `rooms/RoomHeader.test.tsx`, both
    part of the dialog's use of the card. README F20 made `ops.Transfer`'s `loadSent` and `rememberSent` quiet for
    a stop, besides the tick. README S93 could not wire its stats into `web/src/rooms/` and wrote the patch instead
    (review S93-R2); the integration applied it unchanged as an extension of S93's Touches
    (`rooms/connectStats.ts` gives the collector `[...viewerStatsSources(viewer), ...shareStatsSources()]` and runs
    `attachViewerStats` with the tab's client, `rooms/media.ts` exports it in the place of `attachFreezeWatch`) with
    S93's overlay step of `a11y.spec`, and corrected two comments: `web/src/lib/stats/index.ts`'s wiring note and
    `internal/server/logsafety.go`'s header. After group 13's integration, the fixes of PR #22 (tests that held only
    on macOS: `TestSourceGuardTCPAgent`'s distinct sources and `hint.spec`'s start profile on CI's Linux Chrome)
    were merged into group 13 and then into group 14 (04 §17, 05 §19.3).
11. **One follow-up slice in group 15: README F21**, decision (2) of "Decided after group 13": `isshoni doctor
    --json` asserted per code in CI's Linux `docker-smoke` and under Docker Desktop on the Mac, with an allow-list
    for what each environment can't satisfy, and the two `public_ip` rules reviewed (a Docker bridge called
    symmetric; a literal loopback `public_ip` taken for a public address). The manual check on a Linux host with a
    real address stays in the exit test's pre-flight (06 §12.1 item 5). Still not scheduled: `sign` and `sign-dry`
    calling `check-draft.sh` without the signature check (F15's note); `task e2e` building `isshoni-loadtest` and
    passing `LOADTEST_BIN` (S89's option). Two stale comments wait for the next slice that edits their file:
    `web/e2e/tcp-only.spec.ts` still says the page's stats have only the viewer's PCs (S93 added the pub PC), and
    `redactCredentials` in `internal/logx/pion.go` describes the pair rule's URL case by a dotless host of at
    least 20 characters, where 04 §10 has what the code does (review S00M-R1, R2).
12. **Still waiting for the owner: README F07** (R1 of the security review, point 1 of "Decided after group 10"),
    unchanged and not in group 15; **README S92**, the load gate, which needs the owner's OK for its two machines;
    and **README S94-day**, the exit test, the device matrix and `v0.1.0`, after S92 ("Owner actions").
13. **Open, for the owner: the sound when the focused share has none.** Unchanged from point 14 of "Decided after
    group 13"; the rule and the code stay as they are (05 §12.3, §24).
14. **Open, for the owner: picture-in-picture on Android Chrome.** Unchanged from point 15 of "Decided after group
    13"; neither the rule nor the code changes until the owner answers (05 §12.5, §24).
15. **Open, for the owner: room names and the emoji that need U+FE0F or a zero-width joiner.** Unchanged from
    point 16 of "Decided after group 13"; the rule and the code stay as they are (03 §8, §19).

### Owner actions (not decisions)
- The one-time GitHub setup of 06 §8.6 (default branch `main`, rulesets, the `release` environment, Pages), before
  group 1 merges.
- Create the release signing key and add it to the `release` environment (06 §9.4) before S73.
- Finish the S4 Firefox and Safari viewer check from M0 before S69 (codec policy) lands.
- Run the manual device matrix (05 §19.4) on iPhone, iPad and Android, and gather four friends for the exit test.
  Since group 13 that is the row `S94-day` (§6 "Decided after group 13" (4)): after README S92's load gate, on an rc,
  with 06 §12.1's pre-flight, whose fifth item is doctor in the Docker bridge and host setups on a Linux host with a
  real address (§6 "Decided after group 13" (2)); then tag `v0.1.0`.
- Decide how slice F07's tests get written (a raw publisher that sends crafted SRTP; §6 "Decided after group 10"
  (1)), so that F07 can merge.
- Consider turning on GitHub's immutable releases (06 §8.6, §9.3; the security review's R4, §6 "Decided after group
  10" (1)), which the one-time setup doesn't include.
- Turn on GitHub Pages (06 §8.6 step 5: Source "GitHub Actions"; the `github-pages` environment for `main` and tags
  `v*`) before the first release: until then `site.yml` builds and checks the site but doesn't deploy it, and once
  a release exists its `check` job fails until the site is deployed (§6 "Decided after group 11" (3)). Then look
  at the first `site.yml` run: whether the workflow token's `pages: read` may read `GET /repos/{repo}/pages` is not
  verified yet (06 §10.5).
- Once `m1/server-web` with group 12 is pushed, run Actions → Release → Run workflow with `dry_run` on: it is README
  F09's last acceptance item (§6 "Decided after group 12" (8)). Its `verify` log should say `systemd: running` and
  that the container reaches `relserve` at `https://host.containers.internal:<port>`, for both bases; the risk is a
  hosted runner whose rootless podman gets no delegated cgroup (06 §4.13).
- Once `m1/server-web` reaches `main`, look at the first `distro-vm` nightly (README F13, 06 §8.5): the VM runs of
  debian-13, ubuntu-22.04 and ubuntu-26.04 were never run by hand, nor any run on x86_64 with KVM, which is how the
  nightly runs every row, so that run is their first.
- Say yes to README S92's two machines (§6 decision 4: a 4 vCPU VPS and a second VM, for about a day) when M1 is
  ready for its load gate; S92 is not in group 14's or group 15's run until then, and the exit test comes after it.
- Once group 13 is pushed (after the group 12 PR merges): its first CI run is the first run of the `e2e` job's
  `isshoni-loadtest` artifact (06 §8.2). Two more paths have not run on GitHub yet: `publish`'s
  `deploy/release/check-draft.sh` first runs at the first real tag (a dry run skips `publish`), and
  `distro-test.sh`'s podman path has run only in README F16's stand-in containers (CI uses Docker).
- The manual checks of group 12, with the steps in the slices' reports: M-FF-1 (a fresh Firefox profile gets
  OpenH264 and the video without a reload) and M-WIFI (a phone viewer from Wi-Fi to LTE and back) of 05 §19.4, and
  a VPS with UDP 7882 blocked showing that provider's fix text in the wizard's connection test (05 §14.2).
- Push group 14 only after the group 13 PR has merged. Its first CI run is the first linux/amd64 run of README
  S94's `TestRealProc` (`tools/exittest`; it has passed on linux/arm64 only).
- README S93's manual check, with the steps in its report: the upload hint under throttling (05 §13.7). Share a
  window with motion at Auto, throttle the upload to about 1–1.5 Mbit/s, expect "Your upload allows about …" within
  about 6 s and expect it to clear within about 10 s after the throttling stops; use the Network Link Conditioner if
  DevTools' throttling doesn't reach WebRTC.
