# M1 design 03: Accounts, store and REST API

Part of the M1 design set (`docs/m1/01` to `06`). `docs/PLAN.md` is the source of truth above this document.
This document covers:
- `internal/server/store`: SQLite through modernc.org/sqlite;
- `internal/server/auth`: passwords, sessions, invites, setup, roles, and the device flow for native apps;
- `internal/server/httpapi`: every REST endpoint under `/api/v1`;
- `internal/protocol/api`: the REST DTOs and error codes, generated to TypeScript by tygo.

**Milestone marking.** Every item in this document is **M1** unless it is tagged *later (Mx)*. A later item only
needs room now (a table, a reserved field, an extension point), and M1 code does not implement it.

**Integration status.** Reconciled with 01, 02, 04, 05 and 06 by the integrator (`README.md`, "Integration
decisions"). The main changes: the WebSocket Origin check and pre-auth limits moved to 01's hub; the hub is reached
through wiring adapters (04 §6.6); one REST error envelope and code table (§12.2) for 03 and 04; push subscription
REST and preferences are here, the sender is 04's; the admin dashboard endpoint is 04's, with an `accounts` part
from here.

---

## 1. Key decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | All token hashes are **HMAC-SHA-256** with the `session` or `invite` key from `secrets.json` | A copied DB or backup yields no usable token. Rotating a key invalidates exactly the tokens it protects (the plan's `rotate-secrets`) |
| 2 | Random 12-character public IDs everywhere; the default room is the fixed ID `lounge` | No user counting or enumeration through IDs. Links (`/r/lounge`, `isshoni://`) survive renames and restores |
| 3 | One writer connection with `BEGIN IMMEDIATE`, plus a small read-only pool; WAL, `synchronous=NORMAL` | SQLite has one writer. Queueing in Go replaces `SQLITE_BUSY`. NORMAL avoids an fsync per write and cannot corrupt the DB |
| 4 | Passwords are hashed **before** the write transaction opens | A 50 ms argon2 hash never holds the writer lock |
| 5 | CSRF protection is Go's `net/http.CrossOriginProtection` plus a mandatory `Content-Type: application/json` | Stdlib and token-free. Forms can't send JSON, and cross-origin JSON needs a preflight, which the server never grants |
| 6 | Unicode usernames with PRECIS (RFC 8265), 2–32 characters, `_ - .` allowed | Friend groups write their names in their own scripts. PRECIS gives a standard, stable comparison key |
| 7 | Sessions last 30 days idle and 180 days at most. The token rotates every 24 h, and the old token stays valid for 60 s | A phone PWA opened on weekends stays logged in. Rotation limits a stolen cookie. The grace period stops parallel requests from logging the user out |
| 8 | Throttles live in memory (token buckets) and run before any hashing or DB access (the one exception is a read of the user's known IPs while that account's bucket is empty, §7.3) | An attacker cannot restart the server. Persisting each failure would turn an attack into DB writes |
| 9 | Admin password reset issues a **one-time reset link**, clears the password and revokes everything | The admin never learns the password. The same path (via the CLI) recovers a sole admin who is locked out |
| 10 | Pending sign-ups are ordinary `users` rows with `status='pending'`, and that is the approval queue | One place for usernames and uniqueness. Rejecting a sign-up frees its name |
| 11 | Settings that an admin can change in the UI live in the DB (`settings`), with typed validation and a live cache. TOML stays for infrastructure, and a TOML key can **pin** a setting | Registration mode and limits change at runtime without a restart. Config can still force a value |
| 12 | Device-flow tables ship in migration 0001; the endpoints ship in M2 | M1's revocation paths (password reset, logout everywhere) already delete devices, so that code is written and tested once |
| 13 | REST DTOs go in `internal/protocol/api` (a new subpackage) | The M2 desktop client (`internal/client`) imports only `internal/protocol`, and tygo generates the SPA's types from the same structs |

---

## 2. Scope and ownership

This document owns the SQLite file and schema (including the tables 04 needs: push preferences and transfer
counters), migrations and pre-migration backups, passwords, web sessions, CSRF/Origin rules for REST,
setup/invite/reset tokens, registration modes and the approval queue, roles and admin alerts, the device flow design,
rooms (persistence and admin CRUD), settings storage, the audit log, push *subscription* and *preference* storage and
endpoints, the REST error envelope and code table (shared with 04), and all of `/api/v1` except the routes listed in
§12.6. The WebSocket handshake's Origin check and pre-auth limits are 01's (01 §3.1); this doc only validates the
credentials.

It does not own the following, and uses them through interfaces (§16, §17):
- config parsing, `secrets.json`, TLS, the 443 mux, listeners, global security headers, the Web Push *sender*, doctor,
  and the admin socket CLI (docs/m1/04-server-platform.md);
- signaling, presence and the WebSocket protocol (docs/m1/01-protocol.md);
- the SFU and live stats (docs/m1/02-sfu.md);
- all UI (docs/m1/05-web-client.md);
- packaging (docs/m1/06-deploy-and-ci.md).

### 2.1 Package layout

```
internal/protocol/api/            REST DTOs + error codes; imports only the stdlib (tygo → TS)
  types.go  errors.go  device.go (later: M2)  testdata/*.json (golden JSON)
internal/server/store/
  store.go        Open, Options, DB, Read/Write, Ping, QuickCheck, Stats, BackupTo, Close, BackupFile
  migrate.go      migrator, pre-migration backups, newer-schema refusal, LatestSchemaVersion, InspectFile
  migrations/0001_init.sql
  ids.go  errors.go  meta.go
  users.go sessions.go devices.go invites.go setup.go resets.go rooms.go push.go audit.go prune.go
  settings.go     Settings struct, registry, SettingsCache
internal/server/auth/
  service.go      Service, Options, Principal, Actor helpers
  tokens.go       random tokens, keyed hashes, key fingerprints
  username.go  password.go  argon2.go  limiter.go  useragent.go
  session.go      cookie, rotation, cache, Authenticate, AuthenticateCookie, Touch
  origin.go       CSRF wrapper (REST only; the WebSocket Origin check is 01's)
  setup.go  register.go  invite.go  reset.go  users.go  alerts.go  janitor.go
  deviceflow.go   later (M2)
  data/common-passwords.txt.gz
internal/server/httpapi/
  api.go          API, Deps, Access, Handle, the /api/v1 middleware chain (04's router, helpers and global chain
                  live in the same package: router.go, middleware.go, errors.go, json.go, spa.go, headers.go)
  info.go  auth.go  me.go  rooms.go  invites.go  push.go (subscriptions and preferences)
  admin_users.go  admin_rooms.go  admin_settings.go  admin_audit.go  accounts.go (dashboard accounts part)
  device.go       later (M2)
```

**Import rules** (the single table is 04 §2, which is also the `depguard` source; these are 03's rows):
- `store` imports only `internal/protocol/api` (plus the stdlib and modernc).
- `auth` imports `store` and `internal/protocol/api`, never `config` or `httpapi`.
- `httpapi` may import `config`, `logx`, `store`, `auth`, `internal/protocol` and `internal/protocol/api`, and never
  `ops`, `push`, `netx`, `tlsmgr`, `signal`, `sfu` or `sfuplane`. What it needs from them comes through small
  interfaces declared in httpapi (`Signal`, `Push`, `InfoSource`, and the router's metrics interface, 04).
- Only `internal/server` (the wiring), `cmd/isshoni` and `servertest` combine these packages with 04's.

### 2.2 New dependencies (all permissive)

- `golang.org/x/text` (BSD-3-Clause): `secure/precis` for usernames, passwords and room names.
- A **common-password list**: SecLists `10k-most-common` (MIT), filtered to entries of 8 characters or more, lowercased,
  gzipped and embedded (about 25 KB). The attribution goes into `THIRD_PARTY_NOTICES` (06).
- Nothing else. argon2 comes from `golang.org/x/crypto/argon2`. CSRF uses the stdlib. The limiter is about 80 lines
  of our own code.

---

## 3. Conventions

### 3.1 IDs

- `store.NewID()` returns 12 characters from the lowercase Crockford alphabet `0123456789abcdefghjkmnpqrstvwxyz`
  (60 bits from crypto/rand). An INSERT that hits a UNIQUE collision retries once with a new ID.
- Users, sessions, devices, invites, rooms and push subscriptions all use these IDs. The default room is `lounge`.
- Integer rowids never leave the DB. The one exception is `audit_log.id`, which is a pagination cursor.

### 3.2 Secrets and tokens

All tokens come from crypto/rand and are encoded as base64url without padding.

| Token | Random bytes | Shape | Travels in | Stored as | Lifetime |
|---|---|---|---|---|---|
| Session | 32 | 43 chars | Cookie | HMAC(`session` key) | 30 d idle, 180 d max; rotated every 24 h |
| Setup | 32 | 43 chars | `/setup#<t>` fragment → POST body | HMAC(`invite` key) | 24 h, single use |
| Invite | 24 | 32 chars | `/invite#<t>` fragment → POST body | HMAC(`invite` key) | Default 7 d and 10 uses |
| Password reset | 32 | 43 chars | `/reset#<t>` fragment → POST body | HMAC(`invite` key) | 24 h, single use |
| Device code *(later: M2)* | 32 | 43 chars | POST body | HMAC(`session` key) | 10 min |
| User code *(later: M2)* | 8 of 20 letters (≈34.6 bits) | `WDJB-MJHT` | `/link?code=`, app screen | HMAC(`session` key) | 10 min |
| Access token *(later: M2)* | 32 | `isa_` + 43 | `Authorization: Bearer` | HMAC(`session` key) | 15 min |
| Refresh token *(later: M2)* | 32 | `isr_` + 43 | POST body | HMAC(`session` key) | 90 d sliding, rotated on every use |

- Hash = HMAC-SHA-256(key, token bytes as ASCII) → a 32-byte BLOB. Lookups use equality on a UNIQUE index.
- The keys come from 04's `secrets.json`: `session` (32 bytes) and `invite` (32 bytes), passed as `auth.Keys`. The
  resume key belongs to 01 and the VAPID keys to 04.
- The `isa_`/`isr_` prefixes help secret scanners and redaction. 04's `Secret` type redacts every token anyway, and no
  token is ever logged.

### 3.3 Time, IPs, text

- **DB**: INTEGER unix milliseconds, UTC. **JSON**: RFC 3339 UTC strings with milliseconds
  (`2026-10-01T12:00:00.000Z`). Durations in JSON are integer seconds, with the unit implied by the name (`expiresIn`,
  `retryAfter`).
- **IPs** come only from `ClientIP(r)` (04), which honours `X-Forwarded-For` only from trusted proxy CIDRs. They are
  stored as `netip.Addr.String()`, with IPv4-mapped addresses unmapped.
- **Text columns** hold valid UTF-8, NFC-normalized by the PRECIS profiles below. Control characters are rejected.

---

## 4. Store: SQLite

### 4.1 Files, DSN and pragmas

- **Files**:
  - The DB is `<data_dir>/isshoni.db`, plus `-wal` and `-shm`, all mode 0600. `data_dir` comes from 04:
    `/var/lib/isshoni` under systemd, the data volume in Docker.
  - Backups go in `<data_dir>/backups/`: the directory is 0700 and each file 0600.
- **WAL check**: `Open` checks that `PRAGMA journal_mode=WAL` returns `wal`. Otherwise it fails with
  `store: WAL mode unavailable at <path> (network filesystem?)`.
- **Driver** `modernc.org/sqlite`, driver name `sqlite`. The pragmas go in the DSN, so every pooled connection gets
  them:

```
writer: file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)
        &_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)&_pragma=journal_size_limit(67108864)&_txlock=immediate
reader: file:<path>?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)&_txlock=deferred
```

### 4.2 Connections and transactions

- **Writer pool**: `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, no connection lifetime limit. Every write goes through
  it.
- **Reader pool**: `SetMaxOpenConns(min(4, NumCPU))`, with `query_only`.
- `db.Write(ctx, fn)` runs `BEGIN IMMEDIATE` on the writer. It commits if `fn` returns nil and rolls back otherwise.
- `db.Read(ctx, fn)` runs a deferred transaction on a reader, so `fn` sees one consistent snapshot.
- **Rules**:
  - no password hashing, network I/O or hub calls inside `Write`;
  - hooks (kicks, alerts, room events) run **after** the commit;
  - a `Write` that takes longer than 250 ms logs a WARN with the caller name.

### 4.3 Migrations

- **Files**: `internal/server/store/migrations/NNNN_snake_name.sql`, embedded with `//go:embed migrations/*.sql`.
  - The version is `NNNN`: it starts at 1 and must be contiguous (a unit test checks this).
  - For data rewrites that SQL can't express, a version may also have a Go step in
    `var goSteps = map[int]func(ctx context.Context, tx *sql.Tx) error{}`. It runs after that version's SQL, in the
    same transaction.
- **Bookkeeping** (created by the migrator itself, before any migration runs):

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     INTEGER PRIMARY KEY,
  name        TEXT    NOT NULL,     -- file name without extension, e.g. "0001_init"
  applied_at  INTEGER NOT NULL,     -- unix ms
  app_version TEXT    NOT NULL      -- isshoni SemVer that applied it
) STRICT;
```

- The current version is `MAX(version)`, or 0 for a new file. After each migration the migrator also sets
  `PRAGMA user_version = <version>`, so the version is visible to the `sqlite3` CLI.
- **Forward-only**: there are no down migrations. Each version runs as follows:
  1. `PRAGMA foreign_keys=OFF` (outside the transaction);
  2. `BEGIN IMMEDIATE`;
  3. the SQL, then the Go step if there is one;
  4. `PRAGMA foreign_key_check`: any row aborts;
  5. INSERT into `schema_migrations`;
  6. `COMMIT`;
  7. `PRAGMA foreign_keys=ON`.

  This follows SQLite's table-rebuild procedure: rebuilding a table needs foreign keys off, and the check keeps the
  step safe.
- **History mismatch**: if a version in `schema_migrations` has a different name from the embedded file with that
  number, the server refuses to start with `store: schema history mismatch at version N (DB has "x", binary has "y")`.
  This catches a DB from a fork or a dev build.
- **`Open` sequence**:
  1. create `backups/`;
  2. open the writer and check WAL;
  3. create `schema_migrations`;
  4. read the current version;
  5. if it is newer than this binary, return `*SchemaTooNewError` (§4.5);
  6. if it is older, take the backup (§4.4), then migrate;
  7. upsert the meta keys `created_at` (if missing) and `last_app_version`;
  8. `EnsureDefaultRoom`;
  9. open the reader pool;
  10. run `PRAGMA optimize=0x10002`.
- **Errors a restart can't fix**: the store exports
  `var ErrNeedsOperator = errors.New("store: needs operator action")`. `Open` returns each of these wrapped with it
  (`fmt.Errorf("…: %w", ErrNeedsOperator)`), keeping the message shown above:
  - the schema history mismatch;
  - a failed migration step (its SQL or its Go step);
  - a failed `PRAGMA foreign_key_check`;
  - `SQLITE_CORRUPT` or `SQLITE_NOTADB`, at open or during a migration;
  - WAL mode unavailable (§4.1);
  - not enough free space for the pre-migration backup (§4.4);
  - a schema newer than the binary: `*SchemaTooNewError` has `Unwrap() error` returning `ErrNeedsOperator` (§4.5).

  04 exits with 78 exactly when `errors.Is(err, store.ErrNeedsOperator)`, and with 1 for any other `Open` error. In
  both cases it prints this doc's message.

### 4.4 Pre-migration backups

- A backup is taken only when the current version is ≥ 1 and below the binary's version. A new DB needs none.
- `VACUUM INTO ?` writes to `backups/pre-<ver>-<UTC yyyymmddThhmmssZ>.db`, for example
  `backups/pre-3-20261014T021500Z.db`. `<ver>` is the **schema version before migrating**, which is exactly the
  version an older binary looks for (§4.5).
- **Free space**: before the backup, `Open` checks that free space is at least 2 × (DB + WAL size). Otherwise it fails
  with `store: not enough free disk space for the pre-migration backup (need X MB, have Y MB)`, and nothing changes.
- **Rotation**: after a successful backup, `Open` deletes `pre-*.db` files beyond the newest 5, ordered by the
  timestamp in the name. It always keeps the newest file for each distinct `<ver>`, even when that file is older than
  the newest 5. The reason is a restart loop: Docker's `restart: unless-stopped` (06) restarts forever after exit 78,
  and every start that finds an older schema writes one more backup. Example: an upgrade from schema 3 commits
  migrations 4 and 5 and then fails at 6. Each restart then writes another `pre-5-*.db`, and without this rule the
  only `pre-3-*.db`, the file the old binary needs (§4.5), would be gone after 5 restarts. The folder holds at most 5
  files plus one per schema version the server has ever migrated from, which is a handful of small DB copies.
- `db.BackupTo(ctx, path)` also uses `VACUUM INTO`, for `isshoni admin backup` (04).
- **File-level functions** (§6), for 04's offline backup, its restore validation of archives and bare `.db` files,
  and offline doctor's schema check. None of them migrates, creates `backups/` or writes `meta`. `InspectFile` and
  `BackupFile` open the source read-only (`mode=ro`); `LatestSchemaVersion` opens nothing (it reads the embedded
  migrations). `ops` may not import `store`, so `cmd/isshoni` and the wiring pass them to 04 as functions.
  - `LatestSchemaVersion()` is the highest schema version this binary knows.
  - `InspectFile(ctx, dbPath, backupDir)` returns `FileInfo`: the schema version, `meta.last_app_version`, whether the
    migration history matches the embedded files (`HistoryOK`), the `PRAGMA integrity_check` result (`Integrity`,
    nil = ok) and, for a newer schema, the same `*SchemaTooNewError` that `Open` would return (`backupDir` is only
    read, to fill its `Backup`).
  - `BackupFile(ctx, src, dst)` opens `src` read-only and runs `VACUUM INTO dst`; it fails if `dst` exists.
- **Close**: `(*DB).Close` checkpoints with `PRAGMA wal_checkpoint(TRUNCATE)` (§6), so the `.db` file alone is
  complete after a clean shutdown.
- Restore is 04's (04 §12.4): the running server validates, shuts down, closes the store and swaps the files, then
  re-execs; `--offline` does the same while the server is stopped.

### 4.5 Refusing a newer schema

```go
// Returned by store.Open when the file's schema is newer than this binary.
type SchemaTooNewError struct {
	DBVersion      int    // schema version found in the file
	BinaryVersion  int    // highest version this binary knows
	LastAppVersion string // meta.last_app_version, e.g. "0.6.1"
	Backup         string // newest backups/pre-<BinaryVersion>-*.db, or "" if there is none
}

func (e *SchemaTooNewError) Error() string
func (e *SchemaTooNewError) Unwrap() error // ErrNeedsOperator (§4.3)
```

`Error()` gives the user one actionable message: the problem, the versions and the backup file, with no command. For
example:

> database schema 5 is newer than this isshoni build supports (4); it was last used by isshoni 0.6.1. Install
> isshoni 0.6.1 or newer, or restore the database from before the upgrade:
> /var/lib/isshoni/backups/pre-4-20261014T021500Z.db (changes made after that backup are lost)

With no matching backup, the second half becomes "no backup for schema 4 was found in <dir>". 04 prints the message,
appends the exact `isshoni admin restore --offline` command for systemd or Docker, and exits with code 78 (EX_CONFIG;
systemd's `RestartPreventExitStatus=78` stops the restart loop, 06). Offline doctor's `schema` check shows the same
text through the same 04 formatter. 04's `admin restore` accepts such a `.db` file and restores only the database
(04 §12.4). There are no "compatible newer schema" exceptions: the plan says refuse, and release notes flag every
migration (06).

### 4.6 Key fingerprints (rotate-secrets)

- The meta keys `session_key_fp` and `invite_key_fp` hold the hex of the first 8 bytes of SHA-256(key).
- `auth.New` compares them with the current keys:
  - a changed `session` key deletes every `sessions`, `devices`, `device_tokens` and `device_codes` row;
  - a changed `invite` key deletes every `invites`, `setup_tokens` and `password_resets` row.
- Both cases write an audit row `secrets.rotated {keys:[...]}` and raise the admin alert `secrets_rotated`.
- Rationale: rows hashed with the old key can never match again, and deleting them keeps the Devices and Invites pages
  honest.
- This startup check is the only purge path: `isshoni admin rotate-secrets` writes the new keys and restarts the
  server (04 §5.3); there are no live rotation hooks. 04's push service does the same for VAPID with the meta key
  `vapid_key_fp`, in `push.New`, which the wiring runs before `auth.New` and before serving.

### 4.7 Janitor and pruning

`auth.(*Service).RunJanitor(ctx)` runs at startup and then every hour. 04 starts it with `serve`. Each run does
`db.Prune(ctx, now)` and `PRAGMA wal_checkpoint(PASSIVE)`, plus `PRAGMA optimize` once a day.

| Rows | Deleted when |
|---|---|
| `sessions` | `idle_expires_at < now` (idle expiry is never later than absolute expiry). Push subscriptions go with them (cascade) |
| `setup_tokens`, `password_resets` | `expires_at < now` |
| `invites` | inactive (expired, used up or revoked) for more than 30 days |
| `users` with `status='pending'` | `created_at < now − 14 d`. Audit `user.signup_expired` |
| `device_codes` *(later: M2)* | `expires_at < now − 1 h` |
| `device_tokens` *(later: M2)* | `expires_at < now`. Spent refresh tokens are kept until their own expiry, for reuse detection |
| `audit_log` | `at < now − 30 d` |

---

## 5. Schema: migration `0001_init.sql` (schema version 1)

```sql
-- Key/value facts about this database: created_at, last_app_version, session_key_fp, invite_key_fp, and 04's
-- internal ops state: vapid_key_fp, ops.release_check, ops.doctor_last, ops.transfer_alert_sent (JSON values).
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE users (
  id                  TEXT PRIMARY KEY,                 -- store.NewID()
  username            TEXT NOT NULL,                    -- display form (case preserved, PRECIS-enforced)
  username_key        TEXT NOT NULL UNIQUE,             -- PRECIS UsernameCaseMapped compare key
  password_hash       TEXT,                             -- PHC argon2id; NULL = admin reset pending
  role                TEXT NOT NULL DEFAULT 'user'   CHECK (role   IN ('admin','user')),
  status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','pending','disabled')),
  created_via         TEXT NOT NULL CHECK (created_via IN ('setup','invite','signup','cli')), -- 'cli' reserved, later
  invite_id           TEXT REFERENCES invites(id) ON DELETE SET NULL,
  approved_by         TEXT REFERENCES users(id)   ON DELETE SET NULL,
  approved_at         INTEGER,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  password_changed_at INTEGER,
  last_login_at       INTEGER
) STRICT;
CREATE INDEX users_status_created ON users(status, created_at);
CREATE INDEX users_invite         ON users(invite_id);

CREATE TABLE sessions (                                  -- web sessions (cookie)
  id               TEXT PRIMARY KEY,                     -- stable across rotations
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash       BLOB NOT NULL UNIQUE,
  prev_token_hash  BLOB UNIQUE,                          -- previous token, valid until prev_valid_until
  prev_valid_until INTEGER,
  name             TEXT NOT NULL,                        -- "Chrome on Windows" (raw User-Agent is never stored)
  created_at       INTEGER NOT NULL,
  rotated_at       INTEGER NOT NULL,
  last_seen_at     INTEGER NOT NULL,
  last_ip          TEXT NOT NULL,
  idle_expires_at  INTEGER NOT NULL,                     -- min(last use + 30 d, expires_at)
  expires_at       INTEGER NOT NULL                      -- created_at + 180 d
) STRICT;
CREATE INDEX sessions_user ON sessions(user_id, last_seen_at);
CREATE INDEX sessions_idle ON sessions(idle_expires_at);

-- Native apps linked with the device flow. Table: M1 (so revocation paths cover it); flow: later (M2).
CREATE TABLE devices (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,                            -- from the app, e.g. "Alex-PC" (≤ 64 chars)
  client_kind  TEXT NOT NULL CHECK (client_kind IN ('desktop','mobile')),
                                                         -- 01 ClientKind; the Linux agent is desktop + os linux
                                                         -- (role agent is a hello field)
  os           TEXT NOT NULL CHECK (os IN ('windows','macos','linux','ios','android')),
  app_version  TEXT NOT NULL,
  linked_via   TEXT NOT NULL CHECK (linked_via IN ('device_flow','password')),
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_ip      TEXT NOT NULL
) STRICT;
CREATE INDEX devices_user ON devices(user_id);

CREATE TABLE device_tokens (                             -- later (M2)
  token_hash  BLOB PRIMARY KEY,
  device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('access','refresh')),
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  spent_at    INTEGER,                                   -- refresh only: when it was exchanged
  replaced_by BLOB                                       -- refresh only: hash of the refresh token it was exchanged for
) STRICT;
CREATE INDEX device_tokens_device ON device_tokens(device_id, kind);
CREATE INDEX device_tokens_expiry ON device_tokens(expires_at);

CREATE TABLE device_codes (                              -- RFC 8628 pending authorizations; later (M2)
  device_code_hash BLOB PRIMARY KEY,
  user_code_hash   BLOB NOT NULL UNIQUE,
  client_kind      TEXT NOT NULL CHECK (client_kind IN ('desktop','mobile')),
                                                         -- 01 ClientKind; the Linux agent is desktop + os linux
                                                         -- (role agent is a hello field)
  device_name      TEXT NOT NULL,
  os               TEXT NOT NULL,
  app_version      TEXT NOT NULL,
  request_ip       TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  interval_s       INTEGER NOT NULL DEFAULT 5,
  last_poll_at     INTEGER,
  status           TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','denied','consumed')),
  user_id          TEXT REFERENCES users(id) ON DELETE CASCADE,   -- approver
  decided_at       INTEGER
) STRICT;
CREATE INDEX device_codes_expiry ON device_codes(expires_at);
CREATE INDEX device_codes_user   ON device_codes(user_id);

CREATE TABLE invites (
  id          TEXT PRIMARY KEY,
  token_hash  BLOB NOT NULL UNIQUE,
  note        TEXT NOT NULL DEFAULT '',                  -- e.g. "for Sam", ≤ 64 chars
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,   -- NULL: created from the CLI or creator deleted
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  max_uses    INTEGER NOT NULL CHECK (max_uses BETWEEN 1 AND 1000),
  uses        INTEGER NOT NULL DEFAULT 0,
  revoked_at  INTEGER,
  revoked_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  CHECK (uses BETWEEN 0 AND max_uses)
) STRICT;
CREATE INDEX invites_created_by ON invites(created_by);
CREATE INDEX invites_expiry     ON invites(expires_at);

CREATE TABLE setup_tokens (                              -- at most one row: issuing a new token deletes the others
  token_hash BLOB PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) STRICT;

CREATE TABLE password_resets (                           -- one live link per user
  token_hash BLOB PRIMARY KEY,
  user_id    TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  created_by TEXT REFERENCES users(id) ON DELETE SET NULL,     -- NULL = CLI
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) STRICT;

CREATE TABLE rooms (
  id         TEXT PRIMARY KEY,                           -- 'lounge' for the default room
  name       TEXT NOT NULL,                              -- PRECIS Nickname, 1–40 chars
  name_key   TEXT NOT NULL UNIQUE,                       -- Nickname compare key (case-insensitive)
  is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
  created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX rooms_one_default ON rooms(is_default) WHERE is_default = 1;
-- The Lounge row is inserted by store.EnsureDefaultRoom at every Open (idempotent), not by this file.

CREATE TABLE push_subscriptions (
  id              TEXT PRIMARY KEY,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id      TEXT REFERENCES sessions(id) ON DELETE CASCADE,  -- web: dies with its session
  device_id       TEXT REFERENCES devices(id)  ON DELETE CASCADE,  -- later (native apps)
  endpoint        TEXT NOT NULL UNIQUE,                  -- https URL, ≤ 2048 bytes
  p256dh          TEXT NOT NULL,                         -- base64url, 65-byte uncompressed P-256 point
  auth_secret     TEXT NOT NULL,                         -- base64url, 16 bytes
  name            TEXT NOT NULL,                         -- "Safari on iPhone"
  created_at      INTEGER NOT NULL,
  last_success_at INTEGER,
  failures        INTEGER NOT NULL DEFAULT 0,
  CHECK ((session_id IS NULL) <> (device_id IS NULL))
) STRICT;
CREATE INDEX push_user    ON push_subscriptions(user_id);
CREATE INDEX push_session ON push_subscriptions(session_id);
CREATE INDEX push_device  ON push_subscriptions(device_id);

CREATE TABLE push_preferences (                          -- per user; a missing row means the defaults
  user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  share_started TEXT NOT NULL DEFAULT 'all' CHECK (share_started IN ('all','off')),
  admin_alerts  INTEGER NOT NULL DEFAULT 1 CHECK (admin_alerts IN (0,1)),
  updated_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE transfer_months (                           -- 04's socket-level transfer accounting (04 §11.3)
  month         TEXT PRIMARY KEY,                        -- '2026-09' (UTC)
  egress_bytes  INTEGER NOT NULL DEFAULT 0,
  ingress_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE settings (                                  -- only non-default values are stored
  key        TEXT PRIMARY KEY,                           -- dotted key, see §9
  value      TEXT NOT NULL,                              -- JSON
  updated_at INTEGER NOT NULL,
  updated_by TEXT NOT NULL                               -- user ID or 'cli' (no FK: survives user deletion)
) STRICT;

CREATE TABLE audit_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  at          INTEGER NOT NULL,
  action      TEXT NOT NULL,                             -- catalog in §10
  outcome     TEXT NOT NULL DEFAULT 'ok' CHECK (outcome IN ('ok','denied')),
  actor_kind  TEXT NOT NULL CHECK (actor_kind IN ('user','cli','system','anonymous')),
  actor_id    TEXT,                                      -- no FK: rows outlive users
  actor_name  TEXT NOT NULL DEFAULT '',                  -- snapshot
  target_kind TEXT NOT NULL DEFAULT '',                  -- user|session|device|invite|room|settings|setup
  target_id   TEXT NOT NULL DEFAULT '',
  target_name TEXT NOT NULL DEFAULT '',                  -- snapshot
  ip          TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '{}'                 -- JSON object, ≤ 1 KiB, never secrets
) STRICT;
CREATE INDEX audit_at     ON audit_log(at);
CREATE INDEX audit_actor  ON audit_log(actor_id, at);
CREATE INDEX audit_target ON audit_log(target_id, at);
CREATE INDEX audit_action ON audit_log(action, at);
```

Notes:
- `users` and `invites` reference each other. SQLite accepts a reference to a table that is created later, and the
  checks happen per statement.
- **Login throttles are not persisted** (decision 8). The approval queue is the set of `users.status='pending'` rows.
  Admin alerts are derived from the audit log (§7.11).
- Later migrations that only add a nullable column or a table are additive. A later (M2) feature that needs one ships
  as `0002_…`.

---

## 6. Store Go API

```go
package store

// ---- identifiers and enums ----
type (
	UserID    string
	SessionID string
	DeviceID  string
	InviteID  string
	RoomID    string
	PushSubID string
)

const DefaultRoomID RoomID = "lounge"

func NewID() string // 12 chars, lowercase Crockford base32, crypto/rand

type Role string             // RoleAdmin = "admin", RoleUser = "user"
type UserStatus string       // StatusActive = "active", StatusPending = "pending", StatusDisabled = "disabled"
type RegistrationMode string // ModeInvite = "invite", ModeApproval = "approval", ModeClosed = "closed"
type ActorKind string        // ActorUser = "user", ActorCLI = "cli", ActorSystem = "system", ActorAnonymous = "anonymous"

// Actor is who did something, as recorded in the audit log.
type Actor struct {
	Kind   ActorKind
	UserID UserID // "" unless Kind == ActorUser
	Name   string // username snapshot, "cli", "system", or "" for anonymous
	IP     string // "" for cli/system
}

var CLIActor = Actor{Kind: ActorCLI, Name: "cli"}
var SystemActor = Actor{Kind: ActorSystem, Name: "system"}

// ---- errors ----
var (
	ErrNotFound       = errors.New("store: not found")
	ErrConflict       = errors.New("store: unique constraint")
	ErrInviteUnusable = errors.New("store: invite expired, used up or revoked")
	ErrDefaultRoom    = errors.New("store: the default room cannot be deleted")
	ErrNeedsOperator  = errors.New("store: needs operator action") // wraps every Open error a restart can't fix (§4.3)
)

type ConflictError struct{ Column string } // "username_key" | "name_key" | "endpoint"
func (e *ConflictError) Error() string
func (e *ConflictError) Unwrap() error // ErrConflict

// ---- rows ----
type User struct {
	ID                UserID
	Username          string
	UsernameKey       string
	PasswordHash      string // "" = reset pending
	Role              Role
	Status            UserStatus
	CreatedVia        string // setup | invite | signup | cli
	InviteID          InviteID
	ApprovedBy        UserID
	ApprovedAt        time.Time // zero = never
	CreatedAt         time.Time
	UpdatedAt         time.Time
	PasswordChangedAt time.Time
	LastLoginAt       time.Time
}

type UserRow struct { // admin list: User plus derived fields
	User
	InvitedBy  *UserRef // creator of the invite this user redeemed
	LastSeenAt time.Time // max over sessions and devices
	Sessions   int
	Devices    int
	ResetPending bool
	SignupIP   string // pending users only: IP of the user.signup_requested audit row
}

type UserRef struct {
	ID       UserID
	Username string
}

type Session struct {
	ID             SessionID
	UserID         UserID
	TokenHash      []byte
	PrevTokenHash  []byte
	PrevValidUntil time.Time
	Name           string
	CreatedAt      time.Time
	RotatedAt      time.Time
	LastSeenAt     time.Time
	LastIP         string
	IdleExpiresAt  time.Time
	ExpiresAt      time.Time
}

type Device struct {
	ID                   DeviceID
	UserID               UserID
	Name, ClientKind, OS string
	AppVersion           string
	LinkedVia            string
	CreatedAt            time.Time
	LastSeenAt           time.Time
	LastIP               string
}

type DeviceToken struct { // later (M2)
	TokenHash  []byte
	DeviceID   DeviceID
	Kind       string // "access" | "refresh"
	CreatedAt  time.Time
	ExpiresAt  time.Time
	SpentAt    time.Time
	ReplacedBy []byte
}

type DeviceCode struct { // later (M2)
	DeviceCodeHash, UserCodeHash         []byte
	ClientKind, DeviceName, OS, AppVersion string
	RequestIP                            string
	CreatedAt, ExpiresAt                 time.Time
	Interval                             time.Duration
	LastPollAt                           time.Time
	Status                               string // pending|approved|denied|consumed
	UserID                               UserID
	DecidedAt                            time.Time
}

type Invite struct {
	ID         InviteID
	TokenHash  []byte
	Note       string
	CreatedBy  *UserRef
	CreatedAt  time.Time
	ExpiresAt  time.Time
	MaxUses    int
	Uses       int
	RevokedAt  time.Time
	RevokedBy  UserID
	RedeemedBy []UserRef // filled by ListInvites
}

func (i Invite) State(now time.Time) string // "active" | "expired" | "used_up" | "revoked"

type PasswordReset struct {
	TokenHash []byte
	UserID    UserID
	CreatedBy UserID
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Room struct {
	ID        RoomID
	Name      string
	NameKey   string
	IsDefault bool
	CreatedBy UserID
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PushSubscription struct {
	ID            PushSubID
	UserID        UserID
	SessionID     SessionID
	DeviceID      DeviceID
	Endpoint      string
	P256dh        string
	Auth          string
	Name          string
	CreatedAt     time.Time
	LastSuccessAt time.Time
	Failures      int
}

type AuditEntry struct {
	ID         int64
	At         time.Time
	Action     string
	Outcome    string // "ok" | "denied"
	Actor      Actor
	TargetKind string
	TargetID   string
	TargetName string
	Detail     map[string]any // marshalled ≤ 1 KiB; larger → truncated with "truncated": true
}

// ---- database ----
type Options struct {
	Path       string           // <data_dir>/isshoni.db
	BackupDir  string           // <data_dir>/backups
	AppVersion string           // SemVer of this build
	Readers    int              // default min(4, NumCPU)
	Clock      func() time.Time // default time.Now
	Logger     *slog.Logger
}

func Open(ctx context.Context, o Options) (*DB, error) // migrates; may return *SchemaTooNewError

// Close rejects new Read/Write calls, waits for running transactions, closes the reader pool, runs
// PRAGMA wal_checkpoint(TRUNCATE) on the writer, then closes it. Idempotent. 04 calls it after stopping the janitor
// and the push queue.
func (db *DB) Close() error
func (db *DB) Read(ctx context.Context, fn func(q *Q) error) error  // reader pool, snapshot
func (db *DB) Write(ctx context.Context, fn func(q *Q) error) error // writer, BEGIN IMMEDIATE
func (db *DB) Ping(ctx context.Context) error                       // /readyz: SELECT 1 on writer and a reader
func (db *DB) QuickCheck(ctx context.Context) error                 // doctor: PRAGMA quick_check
func (db *DB) SchemaVersion() int
func (db *DB) Stats(ctx context.Context) (Stats, error)
func (db *DB) BackupTo(ctx context.Context, path string) error      // VACUUM INTO; fails if path exists
func (db *DB) Prune(ctx context.Context, now time.Time) (PruneStats, error)
func (db *DB) Settings() *SettingsCache                              // §9

type Stats struct {
	SchemaVersion   int
	DBBytes         int64
	WALBytes        int64
	Backups         []string // file names, newest first
	Users, Pending  int
	Sessions        int
	Devices         int
	ActiveInvites   int
	Rooms           int
	PushSubs        int
	AuditRows       int
}

type PruneStats struct{ Sessions, Tokens, Invites, Pending, DeviceCodes, DeviceTokens, Audit int }

// ---- file-level (§4.4): never migrate, never create backups/, never write meta ----
func LatestSchemaVersion() int // highest version in the embedded migrations; opens nothing
// InspectFile opens dbPath read-only (mode=ro). backupDir is only read, to fill TooNew.Backup.
func InspectFile(ctx context.Context, dbPath, backupDir string) (FileInfo, error)
// BackupFile opens src read-only (mode=ro) and runs VACUUM INTO dst; fails if dst exists.
func BackupFile(ctx context.Context, src, dst string) error

type FileInfo struct {
	SchemaVersion  int
	LastAppVersion string             // meta.last_app_version
	HistoryOK      bool               // schema_migrations names match the embedded files
	Integrity      error              // PRAGMA integrity_check; nil = ok
	TooNew         *SchemaTooNewError // non-nil when SchemaVersion > LatestSchemaVersion()
}
```

`*Q` wraps the transaction. Read methods work in both `Read` and `Write`. Write methods fail inside `Read`, which a
test covers. Every mutating method that the API exposes takes the audit entry in the same call, so the audit row and
the change commit together (`q.AppendAudit` is also public for composite flows).

```go
// users
func (q *Q) CreateUser(u *User) error                                // sets ID; *ConflictError{"username_key"}
func (q *Q) UserByID(id UserID) (User, error)
func (q *Q) UserByUsernameKey(key string) (User, error)
func (q *Q) ListUsers(status UserStatus /* "" = all */) ([]UserRow, error) // ordered by username_key
func (q *Q) RenameUser(id UserID, display, key string, now time.Time) error
func (q *Q) SetRole(id UserID, r Role, now time.Time) error
func (q *Q) SetStatus(id UserID, s UserStatus, now time.Time) error
func (q *Q) Approve(id UserID, by UserID, now time.Time) error       // pending → active; ErrNotFound if not pending
func (q *Q) SetPasswordHash(id UserID, phc string /* "" = NULL */, now time.Time) error
func (q *Q) TouchLogin(id UserID, now time.Time) error
func (q *Q) DeleteUser(id UserID) error
func (q *Q) CountActiveAdmins() (int, error)
func (q *Q) AnyAdmin() (bool, error)                                 // any status
func (q *Q) CountPending() (int, error)
func (q *Q) DeletePending() ([]UserID, error)                        // reject all (§7.9): every pending row
// KnownIPs returns the last_ip of each live session of that user (§7.3); none for an unknown name.
func (q *Q) KnownIPs(usernameKey string, now time.Time) ([]string, error)

// sessions
func (q *Q) CreateSession(s *Session) error                          // sets ID
func (q *Q) SessionByTokenHash(h []byte, now time.Time) (Session, User, error) // token_hash, or prev within grace; unexpired
func (q *Q) RotateSession(id SessionID, newHash []byte, prevValidUntil, now time.Time) error
func (q *Q) TouchSession(id SessionID, ip string, now, idleExpires time.Time) error
func (q *Q) ListSessions(u UserID) ([]Session, error)                // newest last_seen first
func (q *Q) DeleteSession(u UserID, id SessionID) (bool, error)
func (q *Q) DeleteSessions(u UserID, except SessionID) ([]SessionID, error)
func (q *Q) TrimSessions(u UserID, keep int) ([]SessionID, error)   // deletes least recently seen beyond keep

// devices (list/delete: M1; the rest: later M2)
func (q *Q) ListDevices(u UserID) ([]Device, error)
func (q *Q) DeleteDevice(u UserID, id DeviceID) (bool, error)
func (q *Q) DeleteDevices(u UserID) ([]DeviceID, error)
func (q *Q) DeleteDeviceCodesOf(u UserID) error
func (q *Q) CreateDeviceCode(dc DeviceCode) error                                  // later (M2)
func (q *Q) DeviceCodeByHash(h []byte) (DeviceCode, error)                         // later (M2)
func (q *Q) DeviceCodeByUserCode(h []byte) (DeviceCode, error)                     // later (M2)
func (q *Q) DecideDeviceCode(h []byte, status string, u UserID, now time.Time) error // later (M2)
func (q *Q) PollDeviceCode(h []byte, now time.Time, interval time.Duration) error  // later (M2)
func (q *Q) CreateDevice(d *Device) error                                          // later (M2)
func (q *Q) InsertDeviceToken(t DeviceToken) error                                 // later (M2)
func (q *Q) DeviceTokenByHash(h []byte, kind string, now time.Time) (DeviceToken, Device, User, error) // later (M2)
func (q *Q) SpendRefreshToken(h, replacedBy []byte, now time.Time) error           // later (M2)
func (q *Q) TouchDevice(id DeviceID, ip, appVersion string, now time.Time) error   // later (M2)

// setup tokens
func (q *Q) ReplaceSetupToken(h []byte, now, expires time.Time) error // deletes all others
func (q *Q) SetupTokenValid(h []byte, now time.Time) (bool, error)
func (q *Q) DeleteSetupTokens() error

// invites
func (q *Q) CreateInvite(inv *Invite) error
func (q *Q) InviteByTokenHash(h []byte) (Invite, error)
func (q *Q) InviteByID(id InviteID) (Invite, error)
func (q *Q) ListInvites(createdBy UserID /* "" = all */, includeInactive bool) ([]Invite, error)
func (q *Q) CountActiveInvites(createdBy UserID /* "" = all */, now time.Time) (int, error)
func (q *Q) RevokeInvite(id InviteID, by UserID, now time.Time) error
// UseInvite: UPDATE … SET uses = uses + 1 WHERE id = ? AND revoked_at IS NULL AND expires_at > now AND uses < max_uses.
// 0 rows → ErrInviteUnusable. Callers run it in the same Write as CreateUser.
func (q *Q) UseInvite(id InviteID, now time.Time) error

// password resets
func (q *Q) ReplacePasswordReset(r PasswordReset) error              // one per user
func (q *Q) PasswordResetByHash(h []byte, now time.Time) (PasswordReset, error)
func (q *Q) DeletePasswordReset(u UserID) error

// rooms
func (q *Q) EnsureDefaultRoom(now time.Time) error                   // inserts ('lounge','Lounge') if no default room
func (q *Q) ListRooms() ([]Room, error)                              // default first, then created_at
func (q *Q) RoomByID(id RoomID) (Room, error)
func (q *Q) CountRooms() (int, error)
func (q *Q) CreateRoom(r *Room) error                                // *ConflictError{"name_key"}
func (q *Q) RenameRoom(id RoomID, name, key string, now time.Time) error
func (q *Q) DeleteRoom(id RoomID) error                              // ErrDefaultRoom

// push subscriptions
// UpsertPushSubscription: on endpoint conflict it rebinds the row to s.UserID/s.SessionID and replaces the keys.
func (q *Q) UpsertPushSubscription(s *PushSubscription) (created bool, err error)
func (q *Q) ListPushSubscriptions(f PushFilter) ([]PushSubscription, error)
func (q *Q) DeletePushSubscription(u UserID, id PushSubID) (bool, error)
func (q *Q) DeletePushSubscriptionByEndpoint(u UserID, endpoint string) (bool, error)
func (q *Q) DeletePushSubscriptionByID(id PushSubID) error          // sender (04) on 404/410/401/403
func (q *Q) DeleteAllPushSubscriptions() (int, error)               // 04: VAPID key rotated (vapid_key_fp changed)
func (q *Q) PrunePushSubscriptions(minFailures int, noSuccessSince time.Time) (int, error) // 04's daily job
func (q *Q) RecordPushResult(id PushSubID, ok bool, now time.Time) error // failures++ or reset + last_success_at
func (q *Q) TrimPushSubscriptions(u UserID, keep int) error         // oldest beyond keep
func (q *Q) PushPreferences(u UserID) (PushPreferences, error)      // defaults when no row
func (q *Q) SetPushPreferences(u UserID, p PushPreferences, now time.Time) error

type PushFilter struct {
	UserIDs        []UserID // empty = all users
	ExcludeUserIDs []UserID
	AdminsOnly     bool
	Pref           string   // "" | "share_started" | "admin_alerts": keep only users whose preference allows it
	SessionID      SessionID // "" = any; set = only that session's subscriptions (push test)
	// Only users with status 'active' are ever returned.
}

type PushPreferences struct {
	ShareStarted string // "all" | "off"
	AdminAlerts  bool
}

// transfer accounting (04)
func (q *Q) AddTransfer(month string, egress, ingress int64, now time.Time) error // upsert += deltas
func (q *Q) TransferMonth(month string) (egress, ingress int64, err error)

// audit
func (q *Q) AppendAudit(e AuditEntry) error
func (q *Q) ListAudit(f AuditQuery) ([]AuditEntry, error)
func (q *Q) SecurityEvents(since time.Time, limit int) ([]AuditEntry, error) // actions flagged in §10

type AuditQuery struct {
	Before       int64  // id cursor; 0 = newest
	Limit        int    // 1..200, default 50
	ActionPrefix string // e.g. "user." ; "" = all
	ActorID      UserID
	TargetID     string
}

// meta
func (q *Q) GetMeta(key string) (string, error)
func (q *Q) SetMeta(key, value string) error
```

---

## 7. Auth

### 7.1 Usernames

`auth.NormalizeUsername(in) (display, key string, err error)` applies these rules in order. The field codes come
from §12.2.

1. The input may be at most 128 bytes → otherwise `too_long`.
2. `display = precis.UsernameCasePreserved.String(strings.TrimSpace(in))`. This maps fullwidth to halfwidth, applies
   NFC, and rejects spaces, controls, symbols and emoji → otherwise `invalid`.
3. The display form has 2–32 runes → otherwise `too_short` or `too_long`.
4. Every rune is a letter (L\*), a mark (M\*), a decimal digit (Nd), `_`, `-` or `.`. The first and last runes are a
   letter or digit, and two punctuation runes never touch → otherwise `invalid`.
5. `key = precis.UsernameCaseMapped.CompareKey(display)`. Uniqueness, login and the reserved check all use the key.
6. The reserved keys `isshoni`, `system`, `everyone` and `here` → `reserved`. `admin` is allowed, because the first
   admin often picks it.

- Usernames are shown as typed, with case preserved. There are no display names in M1 (*later*).
- An admin can rename any user (§12). Users can't rename themselves (*later*).
- Mixed-script confusable checks come *later (M6 hardening)*. Admins see every new user in the approval queue or the
  audit log.
- Examples: `Ａｌｅｘ` becomes the display `Alex` with key `alex`; `太郎`, `Ёжик` and `sam_k.99` are valid; `a`
  (too short), `-sam` and `sam..k` are invalid.

### 7.2 Passwords and hashing

`auth.CheckPassword(pw, usernameKey) (normalized string, err error)` applies these rules in order:
1. The input may be at most 1024 bytes → otherwise `too_long`, before any other work.
2. `precis.OpaqueString.String(pw)`: NFC, and non-ASCII spaces become U+0020. Controls are rejected → `invalid`.
3. 8–128 runes → otherwise `too_short` or `too_long`.
4. The lowercased password must not be in the embedded common list → otherwise `too_common`.
5. It must not equal the username key after case folding → otherwise `same_as_username`.

There are no composition rules, no expiry and no forced periodic changes. Pasting and password managers stay allowed
(05). The minimum of 8 is lower than NIST SP 800-63B-4's 15 for single-factor passwords. Online guessing is capped at
about 30 per hour per account after a first burst of 30, however many addresses the attacker uses (the `auth-user`
bucket, §7.3), the common list removes weak choices, and argon2id makes a stolen DB expensive.
This is the first open question in §19. `/api/v1/info` serves the limits, so changing them touches only the server.

**Hashing** (`internal/server/auth/argon2.go`):
- **Parameters**: argon2id with m = 19456 KiB (19 MiB), t = 2, p = 1, a 16-byte salt and a 32-byte key.
- **Encoding** (PHC): `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`, both in standard base64 without padding.
- **Verify** parses the parameters from the stored string. If they differ from the current `ArgonParams`, a successful
  login rehashes the password. The new hash is computed outside the transaction and written in the same `Write` as the
  login's other updates.
- **Semaphore**: at most `max(2, runtime.NumCPU()/2)` hashes run at once. Anonymous hashes must first pass the
  global `auth-hash` budget (§7.3), which caps their rate; the semaphore caps only how many run at once.
  - At most 32 callers may wait; the 33rd gets `server_busy` at once.
  - A caller that waits more than 10 s gets `server_busy` (503, `Retry-After: 5`).
  - Peak hashing memory is N × 19 MiB, which is 38 MiB on a 1–3 vCPU VPS.
- **Dummy hash**: at startup the server hashes 32 random bytes with the current parameters. Unknown usernames, and
  users whose hash is NULL, verify against it, so they cost the same CPU and time as a real user.
- Hashes are compared with `subtle.ConstantTimeCompare`.

### 7.3 Throttles

The throttles are in-memory token buckets (decision 8), in `auth/limiter.go`. Every per-IP key is `auth.IPKey`
(§7.13): an IPv4 address as its /32, an IPv6 address as its /64. It is exported so that 04's wiring test pins
`netx.IPKey` to the same result (04 §7.2, §17).

| Bucket | Key | Burst | Refill | Consumed by | Checked |
|---|---|---|---|---|---|
| `auth-ip` | client IP | 20 | 1 per 15 s | Every attempt at login, register, setup/check+complete, reset/check+complete, invite/check, device/code and device/password *(later: M2)* | First, before any work |
| `auth-user-ip` | username key (existing or not) + client IP | 5 | 1 per 2 min | Failed password checks | Before hashing, after `auth-ip`. A hard block |
| `auth-user` | username key (existing or not) | 30 | 1 per 2 min | Failed password checks, from any IP | After `auth-user-ip`. When it is empty, the user's known IPs still pass (below) |
| `register-ip` | client IP | 5 | 1 per 12 min | Sign-up requests without an invite (approval mode). Invite registrations are limited by the invite's own `maxUses` instead (the load test, 02, registers many users from one IP) | Before hashing, after `auth-ip` |
| `auth-hash` | one for the whole server | 20 | 5 per s | Every public request that reaches the hash: login (the dummy hash too), register, setup/complete, reset/complete, device/password *(later: M2)* | Last: after all of the buckets above, before the semaphore (§7.2) |
| `lookup` *(later: M2)* | session ID | 10 | 1 per min | Failed user-code lookups | Before lookup |
| `push-test` | user ID | 1 | 1 per 10 s | `POST /push/test` | Before sending |

- A blocked attempt gets 429 `rate_limited` with `retryAfter` and a `Retry-After` header. It does no hashing, and no
  DB access other than the known-IP read below.
- **Two buckets per username**: `auth-user-ip` stops one address from guessing, and only that address. A stranger who
  fails 5 times from address A blocks A, not the friend's correct password from address B. `auth-user` caps guessing
  across all addresses at about 30 per hour (§7.2). A stranger with enough addresses can empty it; from then on only
  the user's known IPs get through until it refills.
- **Known IPs**: when `auth-user` is empty, the login still goes on if the client IP (IPv4) or its /64 (IPv6) matches
  the `last_ip` of one of that user's live sessions (not past `idle_expires_at` or `expires_at`; M2 adds linked
  devices). That address is still limited by its own `auth-user-ip` bucket. The set comes from `KnownIPs` (§6), one
  indexed read on the reader pool, done only while the bucket is empty, so the normal path stays free of DB access
  (decision 8). An unknown username has no sessions and stays blocked. A friend who logs in from a new place while
  the account is under attack (for example a new iPhone Home Screen app on mobile data, §7.4) waits for the refill.
  A signed long-lived device cookie that would let such a browser through comes *later (M6 hardening)*.
- A successful login refills that username's `auth-user-ip` bucket for that IP. It doesn't touch `auth-user`, so a
  friend's login never hands an attacker a fresh budget.
- **`auth-hash`** caps the total rate of anonymous argon2 hashes, which the per-IP and per-username buckets can't: a
  stranger with many addresses (IPv6 /64s are cheap) could otherwise keep every hash slot busy with random usernames,
  and the SFU would compete for CPU. 5 hashes per second at about 50 ms each use a quarter of one core, about 1/8 of a
  2 vCPU VPS. Keeping the bucket empty takes about 75 addresses, each spending all of its `auth-ip` refill. When it is
  empty, the request gets 503 `server_busy` with `Retry-After` (the seconds until the next token, at least 1) and
  does no hashing. The trade-off: under such an attack, new logins, sign-ups and resets wait. Existing sessions,
  WebSockets and media are unaffected, because nothing on those paths hashes. Password checks by a logged-in user
  (changing the password, deleting the account, granting admin) don't use this bucket.
- Each map holds at most 100,000 keys. When it is full, full buckets (idle keys) are evicted first, then the least
  recently used ones.
- The plan's "20 pre-auth WebSocket handshakes per IP per minute" is enforced by 01's hub at the upgrade (01 §3.1;
  04 key `limits.ws_handshakes_per_ip_per_minute`). Cookie-authenticated upgrades aren't counted there, so 10 friends
  behind one NAT who reconnect after a server restart are never blocked.
- **Audit**:
  - The first block of a key writes one `auth.throttled {scope, key}` row. Later blocks in the same episode are not
    logged. The scope is `ip` for `auth-ip` and `register-ip` (the key is the IP), `username` for `auth-user` (a row
    only if that user exists; the key is the username) and `hash` for `auth-hash` (no key).
  - `auth-user-ip` blocks write no `auth.throttled` row. The `auth.login_failed` rows before them already show the
    address (and the account, if it exists), and one row per pair would let a stranger with many addresses flood the
    log.
  - `auth.login_failed` rows have a global cap of 600 per hour. Beyond it, one `auth.throttled {scope:"global"}` row
    is written per hour.
- **Log**: a blocked attempt logs one `warn` line with `remote_ip` (no username), at most once per `IPKey` per
  minute, like 01's pre-auth `429` (01 §3.1). No `info` line carries a client IP (§11).

### 7.4 Web sessions

**Cookie** (set by `SetSessionCookie`):

```
Set-Cookie: __Host-isshoni_session=<43-char token>; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax
```

- **Name**:
  - `__Host-isshoni_session` whenever the public origin is https. That covers the `auto`, `ip` and `manual` TLS modes,
    and `off` behind an HTTPS proxy.
  - `isshoni_session` without `Secure` only for a plain-http public origin (`task dev` on `http://localhost`).
  - The `__Host-` prefix pins the cookie to exactly this host: no Domain, Path=/, Secure.
- **SameSite=Lax, not Strict**: invite links and notification links opened from other apps must arrive logged in.
- **Lifetime**:
  - `expires_at = created + 180 d`, and `idle_expires_at = min(now + 30 d, expires_at)`, which moves forward on use.
  - `Max-Age` is the remaining idle time, refreshed on every rotation.
- **Rotation**:
  - When `now − rotated_at ≥ 24 h`, the next **REST** request gets a new token, through the httpapi middleware's
    `MaybeRotate`. WebSocket upgrades never rotate.
  - `prev_token_hash` keeps the old hash and `prev_valid_until = now + 60 s`, so parallel requests and other tabs keep
    working.
  - The session ID stays the same, and so do the push subscriptions tied to it. The SPA calls `GET /api/v1/me` at
    start, which guarantees a rotation at least daily for an active user.
- **Login** always issues a new token, so session fixation is impossible. A login or registration that arrives with a
  session cookie deletes that old session first.
- **Touch**:
  - `last_seen_at`, `last_ip` and `idle_expires_at` are written at most once per 5 min per session; an in-memory
    timestamp decides.
  - It happens on REST use, and through `Touch` from the signal hub (01's `Authenticator.Revalidate`, adapted by the
    wiring) at WebSocket connect and every 5 min while connected. A 2-hour session therefore never idles out.
  - On every call, `Touch` checks from the cache or a read that the session exists, is unexpired and belongs to an
    `active` user, and returns `*api.Error{unauthenticated}` otherwise. (M2: for a bearer principal it checks the
    device row; the access token's expiry does not matter after `hello`, 01 §3.2.) The
    `last_seen_at`/`idle_expires_at` write is best effort and throttled to once per 5 min: a write error is logged
    and never returned.
- **Cache**:
  - An in-memory map from token hash to (session, user), with a 30 s TTL.
  - Every revoke, role, status and rename path invalidates it synchronously, by session or by user. There is only one
    server process.
  - A user whose status isn't `active` is always treated as unauthenticated.
- **Cap**: 50 sessions per user. Creating the 51st deletes the least recently seen one.
- **Name**: `auth.DescribeUserAgent(ua)` returns labels like "Chrome on Windows", "Edge on macOS", "Safari on iPhone",
  "Firefox on Linux" or "Samsung Internet on Android", and "Browser" as a fallback. The raw User-Agent is never stored.
- Note for 05: iOS Home Screen web apps have their own cookie jar, so a friend logs in once more inside the installed
  app.

### 7.5 CSRF and Origin checks (REST)

- **Cross-origin check**: every unsafe method (POST, PUT, PATCH, DELETE) under `/api/v1` passes through
  `http.CrossOriginProtection`, which Go has had since 1.25.
  - With `Sec-Fetch-Site`, only `same-origin` and `none` pass.
  - Without it, the Origin host must match `Host`.
  - A request with neither header comes from a non-browser client and passes.
  - Every origin in `Origins.Public` is added with `AddTrustedOrigin`.
  - A denied request gets 403 `csrf_failed` through `SetDenyHandler`.
- **Content type**: every unsafe request must also send `Content-Type: application/json` (a charset is optional), even
  with an empty body. Otherwise the server answers 415 `unsupported_media_type`. HTML forms can't send this type, and
  a cross-origin fetch with it needs a preflight, which the server never grants to a browser origin.
- **Safe methods**: GET and HEAD never change state. The only writes a GET may cause are session bookkeeping
  (touch, §7.4 rotation), which gives a cross-site caller nothing. Code review checks this, and a test (§15) covers it.
- **M1 has no bearer tokens**: any `Authorization` header gets 401 `unauthenticated`.
- *Later (M2)*: a request with `Authorization: Bearer isa_…` **ignores cookies completely** and skips the cross-origin
  check, since there is no ambient credential to forge.
- *Later (M2)*: CORS for the Wails asset origins (listed in §7.6), for bearer requests only:
  - `Access-Control-Allow-Origin: <origin>`, `Access-Control-Allow-Headers: Authorization, Content-Type`,
    `Access-Control-Allow-Methods: GET, POST, PATCH, DELETE`, `Access-Control-Max-Age: 600`;
  - never `Access-Control-Allow-Credentials`.
  
  M1 sends no CORS headers at all.

### 7.6 WebSocket authentication

01's hub owns the upgrade: the exact Origin allowlist, the pre-auth rate limit, the per-user connection cap and
`websocket.Accept` (01 §3.1). This doc supplies only credential checks, through a small wiring adapter that
implements 01's `signal.Authenticator` (04 §6.6):

1. **Cookie** (M1): `AuthenticateRequest(r)` → `(*Service).AuthenticateCookie(r)`, which reads only the session
   cookie and ignores any `Authorization` header (the REST rules of §7.5 do not apply to /ws). `ErrNoCookie` →
   `signal.ErrNoCredentials`; `unauthenticated` (an unknown, expired or non-`active` session) → `signal.ErrInvalid`;
   other errors pass through (the hub answers 503). WebSocket upgrades never rotate the session token (§7.4).
2. **Revalidation**: `Revalidate(ctx, id, ip)` → `(*Service).Touch(ctx, p, ip)` plus a user re-read (username, role).
   The hub calls it at connect and every 5 min. The adapter maps `*api.Error{unauthenticated}` to `signal.ErrInvalid`
   (the hub closes with `session_revoked`) and passes any other error through unchanged. The hub keeps the
   connection on those and retries in 5 min (01 §3.2).
3. *Later (M2)*: bearer access tokens arrive in `hello.auth` (01 D2), never in a header or subprotocol. The adapter's
   `AuthenticateBearer` calls `(*Service).AuthenticateBearerToken(ctx, token)`. The Wails asset origins
   (`wails://wails`, `wails://wails.localhost`, `http://wails.localhost`, `https://wails.localhost`, confirmed against
   the pinned Wails v3 beta in M2) are accepted by the hub only for bearer connections, never with cookies.

### 7.7 Revocation

Every row below commits first. Then the session cache is invalidated, and only then does
`ConnCloser.CloseConnections(sel, reason)` run (the wiring implements it with 01's `Hub.CloseConnections`, mapping
`account_disabled` to `account_disabled` and every other reason to `session_revoked`, 01 §15.4). No code path
deletes a session row without this call, except two that need none: the janitor's prune of expired sessions (see
below the table) and the startup purge after a `session` key change (no connection exists yet).

| Event | Web sessions | Devices and tokens *(later: M2)* | Push subscriptions | Pending reset link | Live connections closed (reason) |
|---|---|---|---|---|---|
| Logout | This one | — | This session's | — | This session (`logged_out`) |
| Revoke one session | That one | — | That session's | — | That session (`session_revoked`) |
| Login, registration, setup or reset completion that arrives with an existing session cookie | That old session | — | That session's | — | That session (`session_revoked`) |
| Session cap eviction (the 51st session) | The evicted ones (`TrimSessions` result) | — | Theirs | — | Those sessions (`session_revoked`) |
| "Sign out other browsers" | All but the current one | — | Theirs | — | Those sessions (`session_revoked`) |
| "Log out everywhere" | All | All | All | — | All of the user's (`logged_out`) |
| Change own password | All but the current one (rotated) | All | Theirs | Deleted | Others (`password_changed`) |
| Admin: password reset | All | All | All | New link issued | All (`password_reset`) |
| Admin: sign out everywhere | All | All | All | — | All (`session_revoked`) |
| Admin: disable user | All | All | All | Deleted | All (`account_disabled`) |
| Delete user (admin or self) | Cascade | Cascade | Cascade | Cascade | All (`account_deleted`) |
| Role change or rename | — | — | — | — | None. The hub gets `Signal.UserChanged(id, username, admin)` (REST path. A CLI `set-role` reaches open connections at the hub's next Revalidate, ≤ 5 min.) |
| Revoke device *(later: M2)* | — | That device | That device's | — | That device (`device_revoked`) |
| Refresh-token reuse *(later: M2)* | — | That device | That device's | — | That device (`device_revoked`) |
| `session` key rotated | All users' | All users' | Cascade | — | Server restart |

Session expiry (idle or absolute) is not pushed. A live connection's expired session is found by the hub's 5-min
Revalidate (§7.6).

Pending device codes that the user approved are deleted along with the devices (`DeleteDeviceCodesOf`). 01 maps each
reason to its `error{code, retryable:false}` message and close code (01 §12.1, §15.4).

### 7.8 Setup token

- `SetupAvailable(ctx)` is true while **no admin row exists**, whatever its status. 04's SPA handler serves `/setup`
  only while it is true, and answers **404** afterwards (§12.6).
- `IssueSetupToken(ctx, actor)`:
  - with an admin present, it returns `api.Error{Code: "setup_unavailable"}`;
  - otherwise, in one `Write`, it deletes all setup tokens, inserts the new hash with `expires_at = now + 24 h`, and
    audits `setup.token_issued`;
  - it returns `Link{URL: Origins.Primary + "/setup#" + token, ExpiresAt}`.
  - Only the newest link works. 04's `isshoni setup-url` (through the admin socket) calls it and prints the URL and
    QR code. For an existing admin, the CLI instead suggests `isshoni admin users reset-password <name>`.
- **At startup with no admin**, the server logs this line at INFO (04 §6.1 step 10), without the token (no token
  ever reaches the logs): "Finish setup: run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni
  setup-url`)".
- **Flow**: the SPA reads the token from `location.hash` and removes the fragment with `history.replaceState` (05).
  1. `POST /api/v1/auth/setup/check {token}` → 204. It lets the page show "link expired" before the form is filled
     in.
  2. `POST /api/v1/auth/setup/complete {token, username, password, serverName?}`. The steps are: throttle, validate,
     hash (outside the transaction), then one `Write`:
     - check the token hash is unexpired and **no admin exists**;
     - create the user (`role=admin`, `created_via=setup`) and a session;
     - delete all setup tokens and set `server.name` if it was given;
     - `EnsureDefaultRoom`, and audit `setup.completed`.
     
     The response is 201 with the session cookie. A race between two tabs leaves exactly one admin: the loser gets 404
     `setup_unavailable`.
- The wizard's next steps (connection test, first invite) run as that logged-in admin. The SPA sets
  `setupWizardDone` when the admin finishes the wizard; step progress lives in the URL (05 §14.1).

### 7.9 Registration modes, invites and the approval queue

**Modes** (setting `registration.mode`, default `invite`; there is no unmoderated open mode):

| Mode | Invite links | Public sign-up (`/signup`) | Result of a sign-up |
|---|---|---|---|
| `invite` (default) | Work | Off → 403 `invite_required` | — |
| `approval` | Work, and pre-approve (account active at once) | On | A `pending` user; an admin approves or rejects |
| `closed` | **Refused** → 403 `registration_closed`. Creating one also gets 403 | Off → 403 `registration_closed` | — |

`closed` is a freeze switch: it differs from `invite` only in refusing existing links.

**Invites**:
- **Who creates them**: admins. Members can too when `invites.members_can_create` is true (default false). Members see
  and revoke only their own invites, and may have at most 10 active → 409 `limit_reached {limit:"member_invites"}`.
- **Limits**: `expiresInHours` is 1–720 (default `invites.default_ttl_hours` = 168) and `maxUses` is 1–1000 (default
  `invites.default_max_uses` = 10). At most 100 active invites per server → 409 `limit_reached {limit:"invites"}`.
- **Link**: `Origins.Primary + "/invite#" + token`. It is returned **once**, at creation. Only the hash is kept, so a
  lost link means creating a new one, which the admin UI says ("Copy it now").
- **Check**: `POST /api/v1/auth/invite/check {token}` returns `{serverName, invitedBy, expiresAt, usesLeft}`, or an
  error that says why the link doesn't work: `invite_invalid` (404), or `invite_expired`, `invite_used_up` or
  `invite_revoked` (410). Only token holders learn this, so the detail is safe to give.
- **Revoke**: `DELETE /api/v1/invites/{id}` sets `revoked_at`. Accounts already created stay.

**Register**: `POST /api/v1/auth/register {inviteToken?, username, password}`. The checks run in this order:
1. the `auth-ip` bucket, then (only without an invite) `register-ip`;
2. the mode;
3. with `inviteToken`: the invite's state (without using it yet);
4. field validation (422);
5. the `auth-hash` budget (§7.3), then the hash (semaphore, outside the transaction);
6. one `Write`:
   - with an invite: `UseInvite` (atomic), `CreateUser(status=active, created_via=invite, invite_id)`,
     `CreateSession`, and audit `user.registered {inviteId}` → **201** with the cookie, and the SPA lands in Lounge;
   - without an invite (approval mode only): at most 50 pending users (else 409
     `limit_reached {limit:"pending_signups"}`), `CreateUser(status=pending, created_via=signup)`, and audit
     `user.signup_requested` with the IP → **202 `{status:"pending"}`**, with no cookie. After the commit comes the
     admin alert `signup_pending`, at most one per 10 min.

A username that is already taken, including by a pending user, gets 409 `username_taken`. That check comes after the
invite is validated, so usernames can't be enumerated without an invite. In approval mode anyone can probe names, at
5 per hour per IP. That is accepted, and noted in §11.

**Approval queue**:
- `GET /api/v1/admin/approvals` lists pending users: username, request time and sign-up IP (from the audit row).
- **Approve**: `status=active`, `approved_by` and `approved_at` are set, and `user.approved` is audited. The user can
  log in from then on; before that, login answers 403 `account_pending`, but only after a correct password.
- **Reject**: the row is deleted, which frees the username, and `user.signup_rejected` is audited.
- **Reject all**: the same endpoint with `all` in place of the ID and the body `{"all": true}` deletes every row that
  is pending when the transaction runs. Both are required, so a stray request can't empty the queue: `all` without
  that body is looked up as an ID and gets 404. It writes one
  `user.signup_rejected {all: true, count}` row with no target and answers 200 `{rejected: n}`. The admin page's
  "Reject all" button (05) is for a flood of fake sign-ups (§11). A real sign-up that arrived a moment before is
  rejected too; its username is free again, so that friend just signs up once more.
- **Expiry**: pending rows expire after 14 days (janitor).
- Pending users get no session in M1, so there is no "you were approved" push. Their page says "An admin will review
  your request. Try logging in later."

### 7.10 Admin-issued password reset

- **Issue**: `POST /api/v1/admin/users/{id}/password-reset {currentPassword?}`, or the CLI
  `isshoni admin users reset-password <name>` for a sole admin who is locked out. One `Write`:
  - sets `password_hash = NULL`;
  - deletes all sessions, devices, approved device codes and push subscriptions of the target;
  - replaces the user's reset row with a new token hash (24 h);
  - audits `user.password_reset_issued`.
  
  After the commit, the target's connections are closed (`password_reset`) and the admin alert `admin_password_reset`
  goes out if the target is an admin. The response is `{url: Origins.Primary + "/reset#" + token, expiresAt}`, which
  the admin sends to the friend privately.
- **Rules**:
  - resetting **another admin** requires the acting admin's `currentPassword`, or 403 `wrong_password`, since the
    reset would hand over that admin account;
  - an admin can't reset themselves this way (409 `self_action_forbidden`) and uses `/me/password` instead;
  - the CLI is exempt from both.
- **Complete**:
  1. `POST /api/v1/auth/reset/check {token}` → `{username}`.
  2. `POST /api/v1/auth/reset/complete {token, password}`: validate, hash, then one `Write` that sets the hash and
     `password_changed_at`, deletes the reset row, creates a session and audits `user.password_reset_completed`. The
     response is 200 with the cookie.
- **Login while the hash is NULL** answers `invalid_credentials`, with no enumeration. The login page always shows
  "Forgot your password? Ask an admin for a reset link."

### 7.11 Roles, the last-admin rule and admin alerts

- There are two roles, `admin` and `user`. Only admins reach `/api/v1/admin/*`; everyone else gets 403 `forbidden`.
  The role is checked on every request (the cache is invalidated on change).
- **Last admin**: at least one **active** admin must always exist. Demoting, disabling or deleting the last one gets
  409 `last_admin`, including self-deletion.
- **Self actions**:
  - an admin can't disable or delete themselves through admin endpoints (409 `self_action_forbidden`);
  - self-deletion goes through `POST /me/delete`;
  - demoting oneself is allowed while another active admin exists.
- **Granting admin** requires the acting admin's `currentPassword` (403 `wrong_password`), so a stolen admin cookie
  alone can't mint a second admin. The CLI is exempt.
- **Admin alerts** (the plan's "admin transfer alerts"). `auth` calls `AdminAlerter.AdminAlert(ctx, alert)` after the
  commit. 04 sends it as a Web Push payload `{type: "admin.alert", kind, actor, target}` to every admin whose
  `adminAlerts` preference is on, including the actor and the target; 05's service worker renders it from the i18n
  key `push.adminAlert.<kind>`. The alert also produces a WARN log line with no secrets, and it appears in the
  dashboard's **Security events** list (the last 7 days of security-flagged audit rows, §10).

| Kind | Trigger |
|---|---|
| `admin_granted` | A user was made admin (by an admin or the CLI) |
| `admin_revoked` | An admin was demoted |
| `admin_password_reset` | A reset link was issued for an admin |
| `registration_mode_changed` | `registration.mode` changed |
| `signup_pending` | A sign-up request arrived (at most one alert per 10 min) |
| `secrets_rotated` | Key fingerprints changed at startup (§4.6). After a session-key rotation no push subscription survives, so this alert shows only as a security event and log line |
| `transfer_threshold` | Month-to-date egress crossed 80 % or 100 % of `transferAlertGb` (raised by 04 §11.3 through the same `AdminAlerter`; `Target` = `"80"` or `"100"`) |
| `refresh_token_reused` *(later: M2)* | A spent refresh token was replayed; that device was revoked |

### 7.12 Native apps: device flow and tokens (later: M2)

The endpoints are listed in §12.4.7. RFC 8628 applies, with our JSON conventions (camelCase versions of the RFC field
names) and our error envelope. Rationale: only first-party clients use it.

- **Start**: `POST /api/v1/device/code {clientKind, deviceName, os, appVersion}` (public; `auth-ip`, plus at most
  10 per hour per IP). It returns `deviceCode` (32 bytes), `userCode` (8 letters from `BCDFGHJKLMNPQRSTVWXZ`, shown
  as `XXXX-XXXX`), `verificationUri` (`<primary>/link`), `verificationUriComplete` (`<primary>/link?code=WDJB-MJHT`),
  `expiresIn: 600` and `interval: 5`. User codes are normalized to upper case with `-` and spaces removed, then
  hashed. `clientKind` is `desktop` or `mobile` (01 `ClientKind`; the Linux agent sends `desktop`); anything else →
  422 `validation_failed {clientKind: invalid}`.
- **Approve** (in the browser, as the logged-in user):
  - `/link?code=…` calls `POST /device/lookup {userCode}`, which shows "Link *isshoni for Windows* on *Alex-PC*?",
    plus the requesting IP and the request time;
  - `POST /device/decide {userCode, approve}` records the choice;
  - both accept **only cookie sessions**, and the `lookup` bucket applies.
- **Poll**: `POST /device/token {grantType:"device_code", deviceCode}` answers `authorization_pending`, `slow_down`
  (when a client polls faster than `interval`; each one adds 5 s), `access_denied` or `expired_token`. On success it
  creates the device and a token pair, marks the code `consumed`, and audits `device.linked`.
- **Tokens**:
  - access tokens (`isa_`) last 15 min;
  - refresh tokens (`isr_`) last 90 days from issue, and every refresh issues a new pair;
  - both are stored only as hashes (`device_tokens`); the app keeps them in the OS credential store (plan).
- **Reuse detection**: presenting a spent refresh token revokes the whole device (the device is its token family),
  audits `device.refresh_reused`, raises the alert, and answers `invalid_grant`.
  - **Grace**: if the replay comes within 30 s of the exchange (the client lost the response), the child token from
    the first exchange (`replaced_by`) is deleted and a fresh pair is issued once.
- **Password fallback** (the plan's in-app form): `POST /device/password {username, password, clientKind, …}`. It has
  the same throttles and rules as login, and creates a device with `linked_via=password`.
- **Use and revocation**:
  - `Authorization: Bearer isa_…` works on every user and admin endpoint and on the WebSocket (§7.6);
  - expired or unknown tokens get 401 `invalid_token` with `WWW-Authenticate: Bearer error="invalid_token"`, so the
    app knows to refresh rather than relink;
  - on the WebSocket the token travels only in `hello.auth` and failure is always `unauthenticated` (01 §3.2). The
    app refreshes before every `hello`, and on `unauthenticated` refreshes once and retries before relinking;
  - `POST /device/revoke` (bearer) is the app's own "Sign out", and the Devices page (M1) revokes any device.
- **Caps**: 20 devices per user; the 21st evicts the least recently seen one.

### 7.13 Auth Go API

```go
package auth

type Keys struct{ Session, Invite []byte } // 32 bytes each, from secrets.json (04)

type Origins struct {
	Primary string   // 04's Site.Origin, e.g. "https://watch.example.com"; builds setup/invite/reset/link URLs
	Public  []string // origins trusted by the REST CSRF check: [Site.Origin] in M1 (04's Host check admits no other
	                 // host). Dev needs nothing extra: task dev sets public_url to the Vite origin (06 §7.3)
}

type Options struct {
	Keys     Keys
	Origins  func() Origins                  // 04; fixed for the process lifetime (config changes need a restart,
	                                         // 04 §4). 01's WebSocket allowlist (Config.PublicOrigin) uses the same
	                                         // Site.Origin
	ClientIP func(*http.Request) netip.Addr  // 04; trusted-proxy aware
	Conns    ConnCloser                      // wiring adapter over 01's Hub.CloseConnections; nil = no-op only in unit
	                                         // tests and offline CLI commands (admin-socket commands run in the
	                                         // server and use the real adapter)
	Alerts   AdminAlerter                    // 04's push; nil = alerts are only logged
	Argon    ArgonParams                     // zero = DefaultArgon
	Hashes   HashBudget                      // the auth-hash bucket (§7.3); zero = DefaultHashBudget; tests only
	Clock    func() time.Time
	Logger   *slog.Logger
}

func New(ctx context.Context, db *store.DB, o Options) (*Service, error) // key fingerprints (§4.6), dummy hash

type ArgonParams struct {
	MemoryKiB, Time uint32
	Threads         uint8
	SaltLen, KeyLen uint32
}

var DefaultArgon = ArgonParams{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

type HashBudget struct{ Burst, PerSecond int }

var DefaultHashBudget = HashBudget{Burst: 20, PerSecond: 5}

type Method uint8

const (
	MethodSession Method = 1
	MethodBearer  Method = 2 // later (M2)
)

// Principal is the authenticated caller of a REST request or WebSocket.
type Principal struct {
	UserID     store.UserID
	Username   string
	Role       store.Role
	Method     Method
	SessionID  store.SessionID // Method == MethodSession
	DeviceID   store.DeviceID  // Method == MethodBearer (M2)
	ClientKind string          // "web" for sessions; the device's kind ("desktop"|"mobile", 01 ClientKind) for bearer
}

func (p Principal) IsAdmin() bool
func ActorOf(p Principal, ip netip.Addr) store.Actor

// IPKey is the limiter's key for a client IP (§7.3): an IPv4 address (IPv4-mapped IPv6 is unmapped first) as its /32,
// an IPv6 address as its /64. 04's wiring test pins netx.IPKey to it (04 §17).
func IPKey(a netip.Addr) netip.Prefix

// Implemented by the wiring over 01's Hub.CloseConnections (04 §6.6). Selects connections by user, session or device.
type ConnSelector struct {
	UserID          store.UserID    // required, never empty (01 closes nothing otherwise)
	SessionID       store.SessionID // "" = any
	DeviceID        store.DeviceID  // "" = any
	ExceptSessionID store.SessionID // keep this one (password change, "sign out other browsers")
}

type ConnCloser interface {
	CloseConnections(sel ConnSelector, reason string) int // returns connections closed
}

// Reason codes passed to CloseConnections.
const (
	ReasonLoggedOut       = "logged_out"
	ReasonSessionRevoked  = "session_revoked"
	ReasonPasswordChanged = "password_changed"
	ReasonPasswordReset   = "password_reset"
	ReasonAccountDisabled = "account_disabled"
	ReasonAccountDeleted  = "account_deleted"
	ReasonDeviceRevoked   = "device_revoked" // later (M2)
)

// Implemented by the Web Push service (04).
type AdminAlert struct {
	Kind   string // §7.11
	Actor  string // username, "cli" or "system"
	Target string // username, or "" when there is no target
	At     time.Time
}

type AdminAlerter interface {
	AdminAlert(ctx context.Context, a AdminAlert)
}

type ReqMeta struct {
	IP        netip.Addr
	UserAgent string // only turned into a session name
}

type Link struct {
	URL       string
	ExpiresAt time.Time
}

type LoginResult struct {
	User    store.User
	Session store.Session
	Token   string // raw cookie value; httpapi sets the cookie
}

// ---- HTTP integration ----
func (s *Service) Authenticate(r *http.Request) (Principal, error)       // cookie (M1) or bearer (M2); REST only
func (s *Service) AuthenticateCookie(r *http.Request) (Principal, error) // /ws only: session cookie, never rotates,
                                                                         // ignores Authorization
// Touch is the hub's Revalidate: validates session/device + user every call; write throttled and best effort; only
// unauthenticated means gone (§7.4, §7.6).
func (s *Service) Touch(ctx context.Context, p Principal, ip netip.Addr) error
func (s *Service) MaybeRotate(w http.ResponseWriter, p Principal)    // REST middleware (§7.4)
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, idleExpires time.Time)
func (s *Service) ClearSessionCookie(w http.ResponseWriter)
func (s *Service) CSRF(next http.Handler) http.Handler               // §7.5

var ErrNoCookie = errors.New("auth: no session cookie") // AuthenticateCookie: the request has no session cookie

// ---- setup ----
func (s *Service) SetupAvailable(ctx context.Context) (bool, error)
func (s *Service) IssueSetupToken(ctx context.Context, a store.Actor) (Link, error)
func (s *Service) CheckSetupToken(ctx context.Context, token string, m ReqMeta) error
func (s *Service) CompleteSetup(ctx context.Context, in SetupInput, m ReqMeta) (LoginResult, error)

type SetupInput struct{ Token, Username, Password, ServerName string }

// ---- login, registration ----
func (s *Service) Login(ctx context.Context, username, password string, m ReqMeta) (LoginResult, error)
func (s *Service) Logout(ctx context.Context, p Principal, m ReqMeta) error
func (s *Service) LogoutEverywhere(ctx context.Context, p Principal, m ReqMeta) error
func (s *Service) CheckInvite(ctx context.Context, token string, m ReqMeta) (InviteInfo, error)
func (s *Service) Register(ctx context.Context, in RegisterInput, m ReqMeta) (RegisterResult, error)

type RegisterInput struct{ InviteToken, Username, Password string }

type RegisterResult struct {
	Pending bool
	Login   *LoginResult // nil when Pending
}

type InviteInfo struct {
	ServerName, InvitedBy string
	ExpiresAt             time.Time
	UsesLeft              int
}

// ---- self-service ----
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, m ReqMeta) (LoginResult, error)
func (s *Service) DeleteSelf(ctx context.Context, p Principal, password string, m ReqMeta) error
func (s *Service) RevokeSession(ctx context.Context, p Principal, id store.SessionID, m ReqMeta) error
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal, m ReqMeta) (int, error)
func (s *Service) RevokeDevice(ctx context.Context, p Principal, id store.DeviceID, m ReqMeta) error

// ---- invites ----
func (s *Service) CreateInvite(ctx context.Context, a store.Actor, in InviteInput) (store.Invite, Link, error)
func (s *Service) RevokeInvite(ctx context.Context, a store.Actor, id store.InviteID) error

type InviteInput struct {
	Note           string
	ExpiresInHours int // 0 = setting default
	MaxUses        int // 0 = setting default
}

// ---- admin (also used by the admin socket with store.CLIActor) ----
func (s *Service) Approve(ctx context.Context, a store.Actor, id store.UserID) (store.User, error)
func (s *Service) Reject(ctx context.Context, a store.Actor, id store.UserID) error
func (s *Service) RejectAll(ctx context.Context, a store.Actor) (int, error) // §7.9; one audit row with the count
func (s *Service) UpdateUser(ctx context.Context, a store.Actor, id store.UserID, ch UserChange) (store.User, error)
func (s *Service) DeleteUser(ctx context.Context, a store.Actor, id store.UserID) error
func (s *Service) SignOutUser(ctx context.Context, a store.Actor, id store.UserID) (sessions, devices int, err error)
func (s *Service) IssuePasswordReset(ctx context.Context, a store.Actor, id store.UserID, actorPassword string) (Link, error)
func (s *Service) CheckPasswordReset(ctx context.Context, token string, m ReqMeta) (username string, err error)
func (s *Service) CompletePasswordReset(ctx context.Context, token, password string, m ReqMeta) (LoginResult, error)
func (s *Service) UserByUsername(ctx context.Context, username string) (store.User, error) // CLI lookups

type UserChange struct {
	Username      *string
	Role          *store.Role
	Status        *store.UserStatus // "active" | "disabled" only
	ActorPassword string            // required when granting admin (not for the CLI)
}

// ---- maintenance ----
func (s *Service) RunJanitor(ctx context.Context) // blocks until ctx is done (§4.7)

// ---- rules (pure functions, exported for tests, CLI and parity checks) ----
func NormalizeUsername(in string) (display, key string, err error)
func CheckPassword(pw, usernameKey string) (normalized string, err error)
func DescribeUserAgent(ua string) string

// ---- later (M2) ----
func (s *Service) AuthenticateBearerToken(ctx context.Context, token string) (Principal, error) // hello.auth (01)
func (s *Service) StartDeviceFlow(ctx context.Context, in api.DeviceCodeRequest, m ReqMeta) (api.DeviceCodeResponse, error)
func (s *Service) DeviceToken(ctx context.Context, in api.DeviceTokenRequest, m ReqMeta) (api.DeviceTokenResponse, error)
func (s *Service) LookupUserCode(ctx context.Context, p Principal, userCode string) (api.DeviceLookup, error)
func (s *Service) DecideUserCode(ctx context.Context, p Principal, userCode string, approve bool, m ReqMeta) error
func (s *Service) PasswordDeviceLogin(ctx context.Context, in api.DevicePasswordRequest, m ReqMeta) (api.DeviceTokenResponse, error)
```

All service errors are `*api.Error` (§12.2) carrying a stable code, so httpapi maps them to status codes in one table.
The one exception is `ErrNoCookie`, which only the /ws adapter sees (§7.6); `AuthenticateCookie` returns
`*api.Error{unauthenticated}` for an invalid, expired or non-`active` session and passes any other error through
as-is.

---

## 8. Rooms

- **Lounge**: `store.EnsureDefaultRoom` runs at every `Open` and inserts `('lounge', 'Lounge', is_default=1)` if no
  default room exists.
  - The default room can be renamed but **never deleted** (409 `room_is_default`), so there is always a room to land
    in.
  - It stays the default: changing the default room comes *later*.
- **Names**: `precis.Nickname.String` trims and collapses spaces and applies NFKC. Emoji are allowed, so
  "🎬 Movie night" is a valid name.
  - 1–40 runes → otherwise `too_short` or `too_long`; disallowed runes → `invalid`.
  - `name_key = precis.Nickname.CompareKey(name)` is unique, so names are case-insensitive (409 `room_name_taken`).
- **Who**: admins create, rename and delete rooms. Room creation by members comes *later*, as a setting.
- **Cap**: 200 rooms (abuse guard) → 409 `limit_reached {limit:"rooms"}`.
- **UX rule** (plan): `GET /api/v1/rooms` returns `showRoomList = (room count > 1)`.
  - While it is false, the SPA hides the room list and the in-room "Create room" link (shown to admins only; it opens
    Admin → Rooms), and admins create the second room under Admin → Rooms.
  - Deleting down to one room hides the list again.
- **Hooks into the signal hub (01)** through `httpapi.Signal` (§12.5), called after the commit:
  - `Notify(NotifyTarget{All: true}, protocol.TopicRooms)` on create, rename and delete, so clients refresh their room
    list or name;
  - `RoomDeleted(id)` on delete. The hub ends that room's shares (`room_closed`) and sends its connections
    `error{room_closed, scope: room}`; clients rejoin `defaultRoomId` themselves (01 §12.1).
  - A rename needs no hub hook: the hub reads names through `RoomDirectory.GetRoom` when it uses them (01 §15.2).
- **Room IDs** are stable across renames. They appear in SPA URLs (`/r/<id>`, 05) and in `isshoni://` deep links
  (plan). Room ids are never reused after a delete (random ids; `lounge` can't be deleted). 01's hub relies on this.
  An unknown or deleted room in `/r/<id>` is reported by 01's `room.join` error `room_not_found`; the SPA then goes
  to `defaultRoomId` (05).
- **Live counts** in room lists come from `Signal.RoomPresence()` (01). They are never stored.
- Later: per-room locks (plan: Later), room order, invites into a specific room, and an i18n default name.

---

## 9. Settings

Only non-default values are stored in `settings`, as JSON under dotted keys. `db.Settings()` returns the cache:

```go
package store

type Settings struct {
	ServerName              string           `json:"serverName"`              // server.name ("" → host of Primary origin)
	RegistrationMode        RegistrationMode `json:"registrationMode"`        // registration.mode
	InviteDefaultTTLHours   int              `json:"inviteDefaultTtlHours"`   // invites.default_ttl_hours
	InviteDefaultMaxUses    int              `json:"inviteDefaultMaxUses"`    // invites.default_max_uses
	MembersCanInvite        bool             `json:"membersCanInvite"`        // invites.members_can_create
	MaxParticipantsPerRoom  int              `json:"maxParticipantsPerRoom"`  // limits.max_participants_per_room
	MaxSharesPerRoom        int              `json:"maxSharesPerRoom"`        // limits.max_shares_per_room
	MaxShareBitrateKbps     int              `json:"maxShareBitrateKbps"`     // limits.max_bitrate_kbps
	TransferAlertGB         int              `json:"transferAlertGb"`         // limits.transfer_alert_gb
	UpdateCheck             bool             `json:"updateCheck"`             // updates.release_check
	MinClientVersion        string           `json:"minClientVersion"`        // clients.min_version
	SetupWizardDone         bool             `json:"setupWizardDone"`         // setup.wizard_done
}
// The dotted names are the DB keys and, for 04's policy keys, the TOML keys that pin them (04 §4.3, §4.6):
// registration.mode, clients.min_version, limits.max_participants_per_room, limits.max_shares_per_room,
// limits.max_bitrate_kbps, limits.transfer_alert_gb, updates.release_check. Server-wide Web Push on/off is 04's
// config key push.enabled; per-user notification choices are push preferences (§12.4.6), not settings.

type SettingsCache struct{ /* atomic.Pointer[Settings] */ }

func (c *SettingsCache) Get() Settings      // lock-free snapshot
func (c *SettingsCache) Defaults() Settings
func (c *SettingsCache) Locked() []string   // JSON names pinned by config
func (c *SettingsCache) Pin(field string, value any) error // 04, before serving: a TOML key forces a value
func (c *SettingsCache) Update(ctx context.Context, patch map[string]json.RawMessage, a Actor) (Settings, error)
func (c *SettingsCache) OnChange(fn func(old, new Settings)) (cancel func())
```

`Update` validates every field before writing. Errors come back as `*api.Error{Code:"validation_failed",
Fields:{...}}`, and a pinned field gets 409 `setting_locked`. The new values and the `settings.changed` audit row
(with `{changes:{field:{from,to}}}`) are written in one `Write`. After the commit, the cache swaps and `OnChange`
callbacks run.

`Pin` runs the same validation as `Update`. On failure it returns
`*api.Error{validation_failed, Fields:{<json name>: <field code>}}`, which 04 reports as a config error naming the TOML
key, its source and the field code (exit 78). `Defaults()` is the single source of policy defaults: 04's config
registry repeats them only for documentation and `config example`, and a wiring test checks that they match.

| Field | Type, range | Default | Read by (enforced in) |
|---|---|---|---|
| `serverName` | string, 0–64 chars, PRECIS Nickname | `""` | 03 (`/info`, invite check), 05 (titles, via `/info`) |
| `registrationMode` | `invite` \| `approval` \| `closed` | `invite` | 03. A change raises an alert |
| `inviteDefaultTtlHours` | int 1–720 | 168 | 03 |
| `inviteDefaultMaxUses` | int 1–1000 | 10 | 03 |
| `membersCanInvite` | bool | false | 03 |
| `maxParticipantsPerRoom` | int 0–10000 (0 = no limit) | 0 | 01 (room.join) |
| `maxSharesPerRoom` | int 0–1000 (0 = no limit) | 0 | 01 (share.start) |
| `maxShareBitrateKbps` | int 0 or 500–100000 (0 = preset default) | 0 | 02 (REMB cap, `quality.hint`, `ShareParams`), 01 (`welcome.limits`) |
| `transferAlertGb` | int 0–1000000 (0 = off; 1 GB = 10⁹ bytes) | 0 | 04 (transfer alerts) |
| `updateCheck` | bool | true | 04 (daily release check) |
| `minClientVersion` | "" or SemVer | `""` | 01 (`hello` reply), 03 (`/info`) |
| `setupWizardDone` | bool | false | 05 (dashboard setup checklist) |

The "no numeric limits" rule from the plan holds: every limit defaults to 0, meaning off, and is an optional admin
soft limit.

---

## 10. Audit log

- Rows are written in the **same transaction** as the change they describe (store §6). The exceptions are failed
  logins and throttle events, which have no other write.
- Rows are pruned after **30 days**. An admin-configurable retention comes *later*.
- The actor and target names are snapshots, so a row stays readable after the user is deleted.
- Security events (column **Sec**) feed the dashboard's Security events list.

| Action | Actor | Target | Detail | Sec |
|---|---|---|---|---|
| `setup.token_issued` | cli | — | `{}` | |
| `setup.completed` | the new admin | user | `{}` | ✓ |
| `auth.login` | user | session | `{}` | |
| `auth.login_failed` | anonymous | user (only if it exists) | `{reason: wrong_password\|unknown_user\|account_pending\|account_disabled}` | |
| `auth.throttled` | anonymous | — | `{scope: ip\|username\|hash\|global, key}` (§7.3) | ✓ |
| `auth.logout` | user | session | `{}` | |
| `auth.logout_everywhere` | user | user | `{sessions, devices}` | |
| `auth.password_changed` | user | user | `{}` | |
| `session.revoked` | user | session | `{name}` | |
| `user.registered` | the new user | user | `{inviteId}` | |
| `user.signup_requested` | anonymous (name = username) | user | `{}` (the IP column holds the sign-up IP) | |
| `user.approved` / `user.signup_rejected` | admin | user (none for reject all) | `{}` / `{}`, or `{all: true, count}` for reject all | |
| `user.signup_expired` | system | user | `{}` | |
| `user.renamed` | admin/cli | user | `{from, to}` | |
| `user.role_changed` | admin/cli | user | `{from, to}` | ✓ |
| `user.disabled` / `user.enabled` | admin/cli | user | `{}` | ✓ / |
| `user.deleted` | admin or the user themselves | user | `{self: bool}` | ✓ |
| `user.signed_out` | admin | user | `{sessions, devices}` | |
| `user.password_reset_issued` | admin/cli | user | `{}` | ✓ |
| `user.password_reset_completed` | user | user | `{}` | |
| `invite.created` | user/cli | invite | `{maxUses, expiresAt, note}` | |
| `invite.revoked` | user | invite | `{}` | |
| `room.created` / `room.renamed` / `room.deleted` | admin | room | `{name}` / `{from, to}` / `{name}` | |
| `settings.changed` | admin/cli | settings | `{changes:{field:{from,to}}}` | ✓ if `registrationMode` changed |
| `secrets.rotated` | system | — | `{keys:[...]}` | ✓ |
| `device.linked` *(later: M2)* | user | device | `{clientKind, os, via}` | |
| `device.revoked` *(later: M2)* | user/admin | device | `{}` | |
| `device.refresh_reused` *(later: M2)* | system | device | `{}` | ✓ |

What is never stored in `detail`: tokens, password material, User-Agent strings, the usernames tried for unknown
accounts (a typo there is often the password), and SDP.

---

## 11. Privacy: exactly what the server stores

This section backs the plan's privacy statement. The project site's privacy page (06) and the login page text (05)
are written from this table.

| Data | Table | Why | Kept until |
|---|---|---|---|
| Username as typed, plus its comparison key | `users` | Login, display | Account deleted (pending sign-ups: 14 d) |
| Password hash (argon2id) | `users` | Login | Account deleted, or cleared by an admin reset |
| Role, status, creation and approval times, approver, invite used, last login time | `users` | Admin pages | Account deleted |
| Per web session: token hash, browser label ("Chrome on Windows"), created and last-seen times, **last IP** | `sessions` | Staying logged in; the Devices page; letting the user's own addresses past a login throttle under attack (§7.3) | Logout, revoke, 30 d idle or 180 d total (removed within 1 h) |
| Per linked app *(later: M2)*: name, app kind, OS, app version, created and last-seen times, last IP, token hashes | `devices`, `device_tokens` | Native apps | Revoked; tokens at expiry |
| Pending app links *(later: M2)*: the same, plus the requesting IP | `device_codes` | The approval screen | ≤ 70 min |
| Invites: token hash, note, creator, limits, use count | `invites` | Invite links | 30 d after the invite stops working |
| Setup and reset link hashes | `setup_tokens`, `password_resets` | One-time links | Used or expired (+ ≤ 1 h) |
| Rooms: name, creator | `rooms` | Rooms | Deleted |
| Push subscriptions: endpoint URL, public keys, browser label, delivery counters | `push_subscriptions` | Notifications | Unsubscribe, the session ends, or the endpoint goes away |
| Push preferences: "tell me when someone shares" (all/off), admin alerts on/off | `push_preferences` | Notifications | Account deleted |
| Server transfer totals per month (bytes in and out, no per-user data) | `transfer_months` | Admin dashboard, transfer alerts (04) | Kept (a few bytes per month) |
| Settings and who last changed them | `settings` | Admin settings | Changed back to the default |
| Audit log: action, actor and target names, **IP**, small detail | `audit_log` | Security review | 30 d |
| Pre-migration backups: full copies of all of the above | `backups/` | Recovery from a failed upgrade | The newest 5 are kept, plus the newest one for each older schema version (§4.4), so deleted accounts can live on in them until they rotate out |

**Never stored**:
- passwords, and any raw token, cookie or code;
- raw User-Agent strings;
- the usernames tried for unknown accounts;
- SDP, ICE candidates, media and stats history;
- anything the plan keeps on the device (window titles, app lists, exclusions, "What friends hear");
- IP history beyond the last IP per session or device, the 30-day audit log and the security-event log lines below.

HTTP access logs are off (04). Security-event log lines (a pre-auth WebSocket `429` and an Origin `403`, 01 §3.1;
login-limit hits, §7.3) may contain the client IP. They are logged at `warn`, at most once per IP per minute, and
are kept as long as journald or Docker log rotation keeps them, not pruned at 30 days like the audit log.

**Memory only**: throttle buckets (IP, username key, or both; at most 100,000 keys per bucket) and the 30-second
session cache.

**Known trade-offs** of `approval` mode; invite mode (the default) has neither:
- Anyone who knows the server URL can check whether a username exists, at 5 tries per hour per IP.
- A stranger can fill the 50-slot pending queue from about 10 addresses (`register-ip` allows 5 sign-ups per
  address at once). Real sign-ups then get 409 `limit_reached` until an admin clears the queue with "Reject all"
  (§7.9) or the rows expire after 14 days. Invite links keep working meanwhile, and switching to invite mode stops
  new sign-ups.

---

## 12. REST API `/api/v1`

### 12.1 Conventions

- **Base**: `/api/v1`. 04 mounts `httpapi.API` at `/api/v1/`. Changes within v1 are additive only: clients ignore
  unknown fields, and the server ignores unknown request fields (no `DisallowUnknownFields`).
- **Encoding**: requests and responses are JSON, and response field names are camelCase. Responses send
  `Content-Type: application/json; charset=utf-8`. A 204 has no body.
- **Headers on every `/api/v1` response**, errors included: `Cache-Control: no-store`. It covers the plan's "auth
  responses", and applies everywhere because every response is per-user. `X-Content-Type-Options: nosniff` comes from
  04's global middleware.
- **Body limits**: auth endpoints (`/auth/*`, `/device/*`) accept 16 KiB and everything else 64 KiB → otherwise 413
  `payload_too_large`. Malformed JSON, or trailing data after the object, gets 400 `bad_request`.
- **Unsafe methods** need `Content-Type: application/json` (§7.5) and pass the cross-origin check.
- **Access levels**:
  - *Public*: no auth needed, but the cross-origin check still applies to unsafe methods;
  - *User*: an active user (cookie in M1; cookie or bearer in M2);
  - *Admin*: an active admin.
- **PATCH** is a JSON merge: only the fields present change.
- **Rate-limited responses** send both `Retry-After` and `error.retryAfter` (seconds).
- **Middleware order** (outermost first): 04's global chain (recover → request ID → read deadline →
  transfer/metrics → shutdown gate → Host check → real IP → security headers, 04 §9.3), then this API chain: body
  limit → no-store → CSRF (unsafe methods) → authenticate (skipped for Public) → `MaybeRotate` → access check →
  handler.

### 12.2 Errors

The envelope carries codes only and never English text (plan). It is the **only** REST error shape: 04's routes
(`/api/v1/conntest`, push sender errors, admin dashboard, doctor, bandwidth) and 04's global middleware use it too. The
SPA maps `code` to the i18n key `errors.<code>` and each field code to `fieldErrors.<field>.<code>`.

```json
HTTP/1.1 422 Unprocessable Entity
Cache-Control: no-store
Content-Type: application/json; charset=utf-8

{"error": {"code": "validation_failed",
           "fields": {"username": "invalid", "password": "too_common"}}}
```

```json
HTTP/1.1 429 Too Many Requests
Retry-After: 42

{"error": {"code": "rate_limited", "retryAfter": 42}}
```

```go
package api // internal/protocol/api

// Error is both the wire body and the Go error value used by store, auth, httpapi and 04's handlers.
type Error struct {
	Code       string            `json:"code"`
	Fields     map[string]string `json:"fields,omitempty"`     // field name → field code
	Params     map[string]any    `json:"params,omitempty"`     // e.g. {"limit": "rooms"}; never prose
	RetryAfter int               `json:"retryAfter,omitempty"` // seconds; also sent as Retry-After
	RequestID  string            `json:"requestId,omitempty"`  // 04's request ID, set on 500 internal (for bug reports)
}

func (e *Error) Error() string // "api: " + Code
func StatusOf(code string) int // the table below; unknown codes → 500

type ErrorResponse struct {
	Error Error `json:"error"`
}
```

**Codes** (Go constants `api.Code…` in `internal/protocol/api/errors.go`; the status map is `api.StatusOf`, used by
`httpapi.WriteError`). Every code is M1 unless tagged. Rows marked (04) belong to 04's routes and middleware.

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed JSON (including trailing data), or a bad path or query parameter |
| `validation_failed` | 422 | See `fields` |
| `unsupported_media_type` | 415 | Unsafe request without `Content-Type: application/json` |
| `payload_too_large` | 413 | Body over the limit |
| `method_not_allowed` | 405 | Known `/api/v1` path, wrong method; the `Allow` header lists the allowed ones |
| `unauthenticated` | 401 | No valid session (or bearer) |
| `invalid_credentials` | 401 | Wrong username or password (the same answer for unknown users) |
| `invalid_token` *(later: M2)* | 401 | Bearer access token expired or unknown: refresh |
| `wrong_password` | 403 | Re-authentication failed (`currentPassword`) |
| `forbidden` | 403 | Not allowed for this role |
| `csrf_failed` | 403 | Cross-origin unsafe request |
| `account_pending` | 403 | Correct password, but the sign-up awaits approval |
| `account_disabled` | 403 | Correct password, but the account is disabled |
| `registration_closed` | 403 | Mode `closed` |
| `invite_required` | 403 | Mode `invite` and no invite token |
| `not_found` | 404 | Generic |
| `user_not_found` / `room_not_found` | 404 | |
| `invite_invalid` | 404 | Unknown invite token |
| `invite_expired` / `invite_used_up` / `invite_revoked` | 410 | |
| `setup_unavailable` | 404 | An admin already exists |
| `setup_token_invalid` | 404 | Unknown, replaced or expired setup token |
| `reset_token_invalid` | 404 | Unknown, used or expired reset token |
| `username_taken` / `room_name_taken` | 409 | |
| `last_admin` | 409 | Would leave no active admin |
| `self_action_forbidden` | 409 | Use the self-service endpoint instead |
| `room_is_default` | 409 | Lounge can't be deleted |
| `limit_reached` | 409 | `params.limit`: `rooms`, `invites`, `member_invites`, `pending_signups` |
| `setting_locked` | 409 | `params.field`, pinned by config |
| `push_endpoint_rejected` | 422 | `params.reason`: `not_https`, `bad_port`, `userinfo`, `ip_literal`, `private_address`, `unresolvable`, `too_long` or `bad_keys` (`too_long` and `bad_keys` come from the handler, the rest from 04's `ValidateEndpoint`, §12.4.6) |
| `push_unavailable` | 503 | Push is off (`push.enabled=false`, 04) |
| `rate_limited` | 429 | `retryAfter` |
| `server_busy` | 503 | Hash queue full, or the server-wide `auth-hash` budget is empty (§7.3); `retryAfter` |
| `internal` | 500 | Logged with `requestId` (04), never with details on the wire |
| `not_found` (03 inside `/api/v1`, 04 elsewhere under `/api/`) | 404 | Unknown `/api/…` route |
| `bad_sdp` (04) | 400 | `/conntest`: the offer isn't a data-channel-only SDP |
| `transport_disabled` (04) | 409 | `/conntest`: `tcp443` in `tls.mode=off` |
| `doctor_busy` (04) | 429 | A doctor run is in progress or ran less than 10 s ago; `retryAfter` |
| `not_ready` (04) | 503 | The media plane isn't ready yet |
| `server_shutdown` (04) | 503 | The server is stopping or restarting; `Retry-After: 5` (same code as 01's WebSocket error) |
| `backup_invalid` (04, admin socket only) | 400 | The archive or DB file failed validation |
| `backup_newer` (04, admin socket only) | 409 | The backup's schema is newer than this binary |
| `restore_in_progress` (04, admin socket only) | 409 | A restore is already running |
| `insufficient_storage` (04, admin socket only) | 507 | Not enough free disk space for the backup or restore |
| `authorization_pending`, `slow_down`, `access_denied`, `expired_token`, `invalid_grant` *(later: M2)* | 400 | RFC 8628 and OAuth semantics |
| `device_code_invalid` *(later: M2)* | 404 | Unknown or expired user code |

**Field codes**: `required`, `too_short`, `too_long`, `invalid`, `reserved`, `too_common`, `same_as_username`,
`out_of_range`, `not_allowed`.

**Codes shared with 01's WebSocket errors** (01 §12.1): `bad_request`, `unauthenticated`, `forbidden`,
`account_disabled`, `room_not_found`, `rate_limited`, `internal` and `server_shutdown`. Each side keeps its own wire
shape and unit rule (REST: `retryAfter` in seconds and `requestId`; WebSocket: `retryAfterMs` and `params.ref`). A
code that appears in both tables must mean the same thing, and its `errors.<code>` text must work for both: the SPA
interpolates a wait time normalized to seconds and one normalized reference id. `internal/protocol/api/errors_test.go`
(a test file only, which imports `internal/protocol`) asserts that the set of codes present in both `api.Code*` and
`protocol.ErrorCode*` equals this list.

### 12.3 Endpoint table

Common errors are not repeated per row: `bad_request`, `payload_too_large`, `unsupported_media_type`, `csrf_failed`,
`rate_limited`, `internal`, and (for User and Admin rows) `unauthenticated`, plus `forbidden` for Admin rows.

| # | Method and path | Access | Success | Endpoint-specific errors | M |
|---|---|---|---|---|---|
| 1 | `GET /api/v1/info` | Public | 200 `Info` | — | M1 |
| 2 | `POST /api/v1/auth/login` | Public | 200 `{user}` + cookie | 401 `invalid_credentials`, 403 `account_pending`/`account_disabled`, 422, 503 `server_busy` | M1 |
| 3 | `POST /api/v1/auth/logout` | Public | 204, cookie cleared | — (idempotent) | M1 |
| 4 | `POST /api/v1/auth/logout-everywhere` | User | 204, cookie cleared | — | M1 |
| 5 | `POST /api/v1/auth/register` | Public | 201 `{status:"active", user}` + cookie · 202 `{status:"pending"}` | 403 `registration_closed`/`invite_required`, 404 `invite_invalid`, 410 `invite_*`, 409 `username_taken`/`limit_reached`, 422, 503 | M1 |
| 6 | `POST /api/v1/auth/invite/check` | Public | 200 `InviteInfo` | 403 `registration_closed`, 404 `invite_invalid`, 410 `invite_*` | M1 |
| 7 | `POST /api/v1/auth/setup/check` | Public | 204 | 404 `setup_unavailable`/`setup_token_invalid` | M1 |
| 8 | `POST /api/v1/auth/setup/complete` | Public | 201 `{user}` + cookie | 404 `setup_unavailable`/`setup_token_invalid`, 422, 503 | M1 |
| 9 | `POST /api/v1/auth/reset/check` | Public | 200 `{username}` | 404 `reset_token_invalid` | M1 |
| 10 | `POST /api/v1/auth/reset/complete` | Public | 200 `{user}` + cookie | 404 `reset_token_invalid`, 422, 503 | M1 |
| 11 | `GET /api/v1/me` | User | 200 `Me` (may rotate the cookie) | — | M1 |
| 12 | `POST /api/v1/me/password` | User | 200 `{}` + rotated cookie | 403 `wrong_password`, 422, 503 | M1 |
| 13 | `POST /api/v1/me/delete` | User | 204, cookie cleared | 403 `wrong_password`, 409 `last_admin` | M1 |
| 14 | `GET /api/v1/me/sessions` | User | 200 `{sessions}` | — | M1 |
| 15 | `DELETE /api/v1/me/sessions/{id}` | User | 204 | 404 `not_found` | M1 |
| 16 | `POST /api/v1/me/sessions/revoke-others` | User | 200 `{revoked}` | — | M1 |
| 17 | `GET /api/v1/me/devices` | User | 200 `{devices}` (empty until M2) | — | M1 |
| 18 | `DELETE /api/v1/me/devices/{id}` | User | 204 | 404 `not_found` | M1 |
| 19 | `GET /api/v1/rooms` | User | 200 `Rooms` | — | M1 |
| 21 | `GET /api/v1/invites` | User | 200 `{invites}` (admin: all; member: own) | 403 `forbidden` (member without permission) | M1 |
| 22 | `POST /api/v1/invites` | Admin, or User if `membersCanInvite` | 201 `{invite, url}` | 403 `forbidden`/`registration_closed`, 409 `limit_reached`, 422 | M1 |
| 23 | `DELETE /api/v1/invites/{id}` | Admin, or the creator | 204 | 404 `not_found` | M1 |
| 24 | `POST /api/v1/push/subscriptions` | User | 201 `{id}` (new) · 200 `{id}` (existing) | 422 `push_endpoint_rejected`, 503 `push_unavailable` | M1 |
| 27 | `POST /api/v1/push/unsubscribe` | User | 204 (idempotent, by endpoint) | — | M1 |
| 28 | `POST /api/v1/push/test` | User | 202 (this session's subscriptions) | 404 `not_found` (none), 429 (1 per 10 s), 503 `push_unavailable` | M1 |
| 29 | `GET /api/v1/admin/users` | Admin | 200 `{users}` (`?status=active\|pending\|disabled`) | — | M1 |
| 30 | `PATCH /api/v1/admin/users/{id}` | Admin | 200 `{user}` | 404 `user_not_found`, 409 `username_taken`/`last_admin`/`self_action_forbidden`, 403 `wrong_password`, 422 | M1 |
| 31 | `DELETE /api/v1/admin/users/{id}` | Admin | 204 | 404, 409 `last_admin`/`self_action_forbidden` | M1 |
| 32 | `POST /api/v1/admin/users/{id}/password-reset` | Admin | 201 `{url, expiresAt}` | 404, 403 `wrong_password`, 409 `self_action_forbidden` | M1 |
| 33 | `POST /api/v1/admin/users/{id}/sign-out` | Admin | 200 `{sessions, devices}` | 404 | M1 |
| 34 | `GET /api/v1/admin/approvals` | Admin | 200 `{pending}` | — | M1 |
| 35 | `POST /api/v1/admin/approvals/{id}/approve` | Admin | 200 `{user}` | 404 `user_not_found` (not pending) | M1 |
| 36 | `POST /api/v1/admin/approvals/{id}/reject` | Admin | 204 · with `{id}` = `all` and `{"all": true}`: 200 `{rejected}` (§7.9) | 404 (also `all` without that body) | M1 |
| 37 | `POST /api/v1/admin/rooms` | Admin | 201 `{room}` | 409 `room_name_taken`/`limit_reached`, 422 | M1 |
| 38 | `PATCH /api/v1/admin/rooms/{id}` | Admin | 200 `{room}` | 404 `room_not_found`, 409 `room_name_taken`, 422 | M1 |
| 39 | `DELETE /api/v1/admin/rooms/{id}` | Admin | 204 | 404, 409 `room_is_default` | M1 |
| 40 | `GET /api/v1/admin/settings` | Admin | 200 `{settings, defaults, locked}` | — | M1 |
| 41 | `PATCH /api/v1/admin/settings` | Admin | 200 `{settings, defaults, locked}` | 409 `setting_locked`, 422 | M1 |
| 42 | `GET /api/v1/admin/audit` | Admin | 200 `{entries, nextBefore}` | 400 (bad query) | M1 |
| 43 | `GET /api/v1/admin/dashboard` | Admin | owned by 04 (§11.4 there); this doc supplies its `accounts` part (§12.4.8) | — | M1 |
| 44 | `POST /api/v1/device/code` | Public | 200 | 422 | M2 |
| 45 | `POST /api/v1/device/token` | Public | 200 token pair | 400 `authorization_pending`/`slow_down`/`access_denied`/`expired_token`/`invalid_grant` | M2 |
| 46 | `POST /api/v1/device/lookup` | User (cookie only) | 200 | 404 `device_code_invalid` | M2 |
| 47 | `POST /api/v1/device/decide` | User (cookie only) | 204 | 404 `device_code_invalid` | M2 |
| 48 | `POST /api/v1/device/password` | Public | 200 token pair | as #2 | M2 |
| 49 | `POST /api/v1/device/revoke` | User (bearer only) | 204 | — | M2 |
| 50 | `GET /api/v1/push/preferences` | User | 200 `PushPreferences` | — | M1 |
| 51 | `PUT /api/v1/push/preferences` | User | 200 `PushPreferences` | 422 | M1 |

### 12.4 Requests and responses

Examples use `watch.example.com` and documentation IPs. IDs are illustrative.

#### 12.4.1 Info

`GET /api/v1/info`

```json
{
  "server": {"name": "Alex's server", "version": "0.1.0", "publicUrl": "https://watch.example.com"},
  "protocol": {"current": 1, "min": 1},
  "minClientVersion": "0.1.0",
  "registration": "invite",
  "setupRequired": false,
  "features": ["push", "passwordReset"],
  "accountRules": {"usernameMinLength": 2, "usernameMaxLength": 32,
                   "passwordMinLength": 8, "passwordMaxLength": 128},
  "push": {"vapidPublicKey": "BEXAMPLE-vapid-public-key-87-chars-base64url…"}
}
```

- `server.name` is the setting, or else the host of the primary origin.
- `protocol` comes from `internal/protocol` (01).
- `minClientVersion` is the setting (`""` = no floor; pinnable by 04's `clients.min_version`).
- `features` holds opaque capability strings. M1 has `passwordReset`, plus `push` exactly when the `push` object is
  present (push enabled and a VAPID key loaded); M2 adds `deviceFlow`. Clients ignore unknown values.
- `push` is omitted when push is unavailable.
- `setupRequired` lets `/` show "This server isn't set up yet. On the server run `sudo isshoni setup-url` (Docker:
  `docker compose exec isshoni isshoni setup-url`)" (05 §4, NotSetUp).

#### 12.4.2 Auth

`POST /api/v1/auth/login` `{"username": "Alex", "password": "correct horse battery"}` →

```json
200  Set-Cookie: __Host-isshoni_session=…; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax
{"user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin"}}
```

This `user` object (`api.User`: `{id, username, role}`) is the public identity shape that other docs reuse.

`POST /api/v1/auth/register`:

```json
{"inviteToken": "EXAMPLEinviteTOKEN0123456789abcd", "username": "太郎", "password": "a long enough passphrase"}
→ 201 {"status": "active", "user": {"id": "b8f2n4r6t0vz", "username": "太郎", "role": "user"}}   + cookie

{"username": "sam_k", "password": "another long passphrase"}          (approval mode, no invite)
→ 202 {"status": "pending"}
```

`POST /api/v1/auth/invite/check` `{"token": "EXAMPLEinviteTOKEN0123456789abcd"}` →
`200 {"serverName": "Alex's server", "invitedBy": "Alex", "expiresAt": "2026-10-08T12:00:00.000Z", "usesLeft": 9}`.
The invalid cases: `410 {"error":{"code":"invite_used_up"}}`, and the other codes in §12.2.

`POST /api/v1/auth/setup/check` `{"token": "…"}` → 204.

`POST /api/v1/auth/setup/complete` → 201 with `{user}` and the cookie:

```json
{"token": "…", "username": "Alex", "password": "…", "serverName": "Alex's server"}
```

`POST /api/v1/auth/reset/check` `{"token": "…"}` → `200 {"username": "Sam"}`.

`POST /api/v1/auth/reset/complete` `{"token": "…", "password": "…"}` → 200 with `{user}` and the cookie.

`POST /api/v1/auth/logout` `{}` → 204, with `Set-Cookie: __Host-isshoni_session=; Path=/; Max-Age=0; HttpOnly;
Secure; SameSite=Lax`. It deletes this session and its push subscriptions. The SPA also calls
`PushSubscription.unsubscribe()` locally.

#### 12.4.3 Me

`GET /api/v1/me` →

```json
{
  "user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin", "createdAt": "2026-10-01T12:00:00.000Z"},
  "session": {"id": "q1w2e3r4t5y6", "name": "Chrome on Windows",
              "createdAt": "2026-10-01T12:00:00.000Z", "expiresAt": "2027-03-30T12:00:00.000Z"},
  "permissions": {"admin": true, "createInvites": true},
  "badges": {"pendingApprovals": 1}
}
```

`badges` appears for admins only. *Later (M2)*: a bearer principal gets `"device": {id, name, clientKind}` instead
of `session`.

`POST /api/v1/me/password` `{"currentPassword": "…", "newPassword": "…"}` → `200 {}`. It rotates this session's
cookie and applies the revocation row in §7.7.

`POST /api/v1/me/delete` `{"password": "…"}` → 204.

`GET /api/v1/me/sessions` →

```json
{"sessions": [
  {"id": "q1w2e3r4t5y6", "name": "Chrome on Windows", "createdAt": "…", "lastSeenAt": "…",
   "lastIp": "203.0.113.7", "current": true},
  {"id": "z9x8c7v6b5n4", "name": "Safari on iPhone", "createdAt": "…", "lastSeenAt": "…",
   "lastIp": "198.51.100.23", "current": false}
]}
```

`GET /api/v1/me/devices` →

```json
{"devices": [{"id": "…", "name": "Alex-PC", "clientKind": "desktop", "os": "windows",
              "appVersion": "0.2.0", "createdAt": "…", "lastSeenAt": "…", "lastIp": "203.0.113.7"}]}
```

`POST /api/v1/me/sessions/revoke-others` `{}` → `200 {"revoked": 2}`.

#### 12.4.4 Rooms

`GET /api/v1/rooms` →

```json
{
  "defaultRoomId": "lounge",
  "showRoomList": true,
  "rooms": [
    {"id": "lounge", "name": "Lounge", "isDefault": true, "createdAt": "…",
     "live": {"participants": 3, "shares": 1}},
    {"id": "p4t7w2m9k1qs", "name": "🎬 Movie night", "isDefault": false, "createdAt": "…",
     "live": {"participants": 0, "shares": 0}}
  ]
}
```

`POST /api/v1/admin/rooms` `{"name": "🎬 Movie night"}` → `201 {"room": {…}}`.

`PATCH /api/v1/admin/rooms/{id}` `{"name": "Games"}` → `200 {"room": {…}}`.

`DELETE /api/v1/admin/rooms/{id}` → 204.

#### 12.4.5 Invites

`POST /api/v1/invites` `{"note": "for Sam", "expiresInHours": 168, "maxUses": 10}` (every field optional) →

```json
201 {
  "invite": {"id": "h6j8k0m2n4p6", "note": "for Sam",
             "createdBy": {"id": "k3m9p2qxw7ht", "username": "Alex"},
             "createdAt": "2026-10-01T12:00:00.000Z", "expiresAt": "2026-10-08T12:00:00.000Z",
             "maxUses": 10, "uses": 0, "state": "active", "redeemedBy": []},
  "url": "https://watch.example.com/invite#EXAMPLEinviteTOKEN0123456789abcd"
}
```

`GET /api/v1/invites?state=active|all` (default `active`) → `{"invites": [Invite…]}`. Inactive invites stay listed
for 30 days.

`DELETE /api/v1/invites/{id}` → 204.

#### 12.4.6 Push subscriptions (storage and API here; the sender is in 04)

`POST /api/v1/push/subscriptions`. This is the browser's `PushSubscription.toJSON()`, trimmed:

```json
{"endpoint": "https://fcm.googleapis.com/fcm/send/dx1…", "keys": {"p256dh": "BNcRd…", "auth": "tBHItJI5svbpez7KI4CCXg"}}
```

**Validation**, in this order; each failure is 422 `push_endpoint_rejected` with that `params.reason`:
1. the endpoint is at most 2048 bytes, else `too_long`;
2. `p256dh` decodes to 65 bytes starting with `0x04` and `auth` to 16 bytes, else `bad_keys`;
3. then `Push.ValidateEndpoint` (04) checks the scheme (`https` only, `not_https`), the port (443, `bad_port`), no
   userinfo (`userinfo`), a DNS name rather than an IP literal (`ip_literal`), and that the name resolves
   (`unresolvable`) only to public addresses (`private_address`, the plan's rule). 04's sender re-checks at dial time
   against DNS rebinding.

The handler checks only the body shape; every URL and host rule is 04's.

**Storage**: the row is upserted by endpoint and bound to the current session, with a name from the User-Agent. Each
user keeps at most 10 subscriptions; the oldest is evicted. The SPA calls this **on every app start** once permission
is granted. That keeps the binding current after re-logins and makes the call idempotent.

The VAPID public key comes from `GET /api/v1/info` (`push.vapidPublicKey`, §12.4.1); there is no separate config
endpoint. The SPA re-subscribes when the key differs from its subscription's `applicationServerKey` (after
`rotate-secrets`, 04).

**Other endpoints**:
- `POST /api/v1/push/unsubscribe` `{"endpoint": "…"}` → 204.
- `POST /api/v1/push/test` → 202. It sends a `push.test` notification to **this session's** subscriptions (this
  browser) through `Push.SendTest`. At most 1 per 10 s per user (`push-test` bucket).
- `GET /api/v1/push/preferences` → `{"shareStarted": "all", "adminAlerts": true}`; `PUT` with the same shape → 200.
  `shareStarted` is `all` or `off`; `adminAlerts` matters for admins only. 04's sender reads them through
  `PushFilter.Pref`.

#### 12.4.7 Device flow (later: M2)

```json
POST /api/v1/device/code
{"clientKind": "desktop", "deviceName": "Alex-PC", "os": "windows", "appVersion": "0.2.0"}
→ 200 {"deviceCode": "…43 chars…", "userCode": "WDJB-MJHT",
       "verificationUri": "https://watch.example.com/link",
       "verificationUriComplete": "https://watch.example.com/link?code=WDJB-MJHT",
       "expiresIn": 600, "interval": 5}

POST /api/v1/device/token
{"grantType": "device_code", "deviceCode": "…"}          or  {"grantType": "refresh_token", "refreshToken": "isr_…"}
→ 200 {"accessToken": "isa_…", "tokenType": "Bearer", "expiresIn": 900,
       "refreshToken": "isr_…", "refreshExpiresIn": 7776000,
       "device": {"id": "…", "name": "Alex-PC"},
       "user": {"id": "k3m9p2qxw7ht", "username": "Alex", "role": "admin"}}
→ 400 {"error": {"code": "authorization_pending"}}

POST /api/v1/device/lookup   {"userCode": "wdjb mjht"}
→ 200 {"clientKind": "desktop", "deviceName": "Alex-PC", "os": "windows", "appVersion": "0.2.0",
       "requestIp": "203.0.113.7", "requestedAt": "…", "expiresAt": "…"}

POST /api/v1/device/decide   {"userCode": "WDJB-MJHT", "approve": true}   → 204
POST /api/v1/device/password {"username": "Alex", "password": "…", "clientKind": "desktop",
                              "deviceName": "Alex-PC", "os": "windows", "appVersion": "0.2.0"}  → 200 (as token)
POST /api/v1/device/revoke   {}   (Authorization: Bearer isa_…)   → 204
```

#### 12.4.8 Admin

`GET /api/v1/admin/users` →

```json
{"users": [
  {"id": "b8f2n4r6t0vz", "username": "Sam", "role": "user", "status": "active", "createdVia": "invite",
   "createdAt": "…", "lastLoginAt": "…", "lastSeenAt": "…",
   "invitedBy": {"id": "k3m9p2qxw7ht", "username": "Alex"},
   "sessions": 2, "devices": 0, "online": true, "resetPending": false}
]}
```

`online` comes from `Signal.OnlineUserIDs()` (01). The list has no IPs: admins see IPs in the audit log only.

`PATCH /api/v1/admin/users/{id}`. Any subset of the fields below; `currentPassword` is needed only when `role`
becomes `admin`:

```json
{"username": "Samuel", "role": "admin", "status": "disabled", "currentPassword": "…"}
```

→ `200 {"user": AdminUser}`. `status` accepts only `active` or `disabled`, and only for users who aren't pending:
pending users go through the approval endpoints (otherwise 422 with `fields.status = "not_allowed"`).

`POST /api/v1/admin/users/{id}/password-reset` `{"currentPassword": "…"}` (needed only if the target is an admin) →
`201 {"url": "https://watch.example.com/reset#…", "expiresAt": "…"}`.

`POST /api/v1/admin/users/{id}/sign-out` `{}` → `200 {"sessions": 2, "devices": 0}`.

`GET /api/v1/admin/approvals` →
`{"pending": [{"id": "…", "username": "sam_k", "requestedAt": "…", "ip": "198.51.100.23"}]}`.

`POST /api/v1/admin/approvals/{id}/reject` `{}` → 204. `POST /api/v1/admin/approvals/all/reject` `{"all": true}` →
`200 {"rejected": 37}`.

`GET /api/v1/admin/settings` →
`{"settings": {Settings…}, "defaults": {Settings…}, "locked": ["updateCheck"]}`.

`PATCH /api/v1/admin/settings` `{"registrationMode": "approval", "maxSharesPerRoom": 4}` → 200, same shape as GET.

`GET /api/v1/admin/audit?before=4812&limit=50&action=user.&actor=k3m9p2qxw7ht&target=b8f2n4r6t0vz` →

```json
{"entries": [
  {"id": 4811, "at": "2026-10-03T19:22:05.114Z", "action": "user.role_changed", "outcome": "ok",
   "actor": {"kind": "user", "id": "k3m9p2qxw7ht", "name": "Alex"},
   "target": {"kind": "user", "id": "b8f2n4r6t0vz", "name": "Sam"},
   "ip": "203.0.113.7", "detail": {"from": "user", "to": "admin"}}
 ],
 "nextBefore": 4811}
```

`nextBefore` is null on the last page.

`GET /api/v1/admin/dashboard` is 04's endpoint (04 §11.4). This doc supplies its `accounts` object through
`(*API).DashboardAccounts(ctx)` (§12.5), which 04's wiring calls:

```json
"accounts": {
  "users": {"active": 6, "pending": 1, "disabled": 0, "admins": 1},
  "invites": {"active": 2},
  "sessions": {"active": 9},
  "devices": {"linked": 0},
  "registrationMode": "invite",
  "securityEvents": [ {AuditEntry…} ]
}
```

`securityEvents` holds up to 20 security-flagged audit rows from the last 7 days.

### 12.5 httpapi Go API (for the integrator and the other docs)

```go
package httpapi

type Access uint8

const (
	Public Access = iota
	User
	Admin
)

type Deps struct {
	DB       *store.DB
	Auth     *auth.Service
	Signal   Signal                         // wiring adapter over 01's hub; nil-safe (tests): no presence, no-op hooks
	Push     Push                           // wiring adapter over 04's push service; nil → push_unavailable
	Info     InfoSource                     // 04
	ClientIP func(*http.Request) netip.Addr // 04's httpapi.ClientIP
	Clock    func() time.Time
	Logger   *slog.Logger
}

func New(d Deps) *API
// ServeHTTP is mounted by 04's router at /api/v1/. After the no-store step it answers every unmatched /api/v1 path
// with 404 not_found and every method mismatch with 405 method_not_allowed plus an Allow header, through
// WriteError. This covers routes added through Handle. 04's router handles only /api/ paths outside /api/v1/.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request)
// Handle registers a route owned by another doc behind the same /api/v1 chain (auth, CSRF, body limit, no-store),
// e.g. a.Handle("POST /api/v1/admin/doctor", httpapi.Admin, h). Patterns use Go 1.22+ ServeMux syntax.
// 04's conntest, dashboard, doctor and bandwidth routes are registered this way by 04's wiring.
func (a *API) Handle(pattern string, access Access, h http.Handler)
// DashboardAccounts builds the "accounts" object of 04's admin dashboard (§12.4.8).
func (a *API) DashboardAccounts(ctx context.Context) (api.DashboardAccounts, error)

func PrincipalFrom(ctx context.Context) (auth.Principal, bool)

// The JSON helpers are defined once, in 04's files of this package (errors.go, json.go), and used by 03 and 04:
//   WriteJSON(w, status int, v any)
//   WriteError(w, r, err error)                         // *api.Error → api.StatusOf(code); other → 500 internal + requestId
//   DecodeJSON(w, r, dst any, maxBytes int64) error      // → *api.Error{bad_request|payload_too_large|unsupported_media_type}

// Implemented by the wiring over 01's hub (04 §6.6).
type Signal interface {
	RoomPresence() map[store.RoomID]RoomPresence         // from Hub.Snapshot()
	OnlineUserIDs() map[store.UserID]struct{}            // from Hub.Snapshot()
	RoomDeleted(id store.RoomID)                         // Hub.CloseRoom: room_closed to that room
	UserChanged(id store.UserID, username string, admin bool) // Hub.UpdateUser: refresh names and roles
	Notify(t NotifyTarget, topics ...protocol.Topic)     // Hub.Notify: the SPA refetches (01 §8.12)
}

type RoomPresence struct{ Participants, Shares int }

// NotifyTarget mirrors 01's signal.Target.
type NotifyTarget struct {
	UserID store.UserID // "" = not by user
	Admins bool         // every admin's connections
	All    bool         // every connection
}

// Implemented by the wiring's adapter over 04's push service (it converts store.PushSubscription to
// push.Subscription).
type Push interface {
	VAPIDPublicKey() string                                     // base64url, uncompressed P-256
	ValidateEndpoint(ctx context.Context, endpoint string) error // nil or *api.Error{push_endpoint_rejected}
	SendTest(ctx context.Context, subs []store.PushSubscription) error
}

// Implemented by 04.
type InfoSource interface {
	ServerVersion() string        // SemVer of this build
	Protocol() (current, min int) // from internal/protocol (01)
}
```

**Which change notifies which topic** (after the commit; 05 maps topics to query keys):

| Change | `Notify` target and topics |
|---|---|
| Room created, renamed or deleted | `{All}`: `rooms` |
| User renamed or role changed | `{UserID: target}`: `me`; `{Admins}`: `admin.users` |
| User disabled, enabled, deleted, signed out, reset link issued | `{Admins}`: `admin.users` |
| Sign-up requested, approved or rejected | `{Admins}`: `admin.approvals`, `admin.users` |
| Invite created, revoked or used by a registration | `{Admins}`: `admin.invites`; `{UserID: creator}`: `admin.invites` when a member created it |
| Settings changed | `{Admins}`: `admin.settings` |
| `membersCanInvite` changed | `{All}`: `me` (members' `permissions.createInvites`) |
| A session created or revoked; a device linked or revoked (M2) | `{UserID}`: `devices` |

Only REST handlers notify. Janitor expiry and admin-CLI changes are picked up at the SPA's next refetch.

The DTOs in `internal/protocol/api` (tygo → TS; 01's tygo config gains this package) are:
- `Info`, `AccountRules`;
- `User`, `Me`, `SessionInfo`, `DeviceInfo`;
- `LoginRequest`, `RegisterRequest`, `RegisterResponse`, `TokenRequest` (the `{token}` bodies), `InviteInfo`;
- `SetupCompleteRequest`, `ResetCompleteRequest`, `ChangePasswordRequest`, `DeleteSelfRequest`;
- `Room`, `RoomPresence` (the `live` object in room lists), `Rooms`;
- `Invite`, `CreateInviteRequest`, `CreateInviteResponse`;
- `PushSubscribeRequest`, `PushPreferences`;
- `AdminUser`, `PatchUserRequest`, `PendingUser`, `RejectRequest` (`{all}`), `RejectAllResponse`, `ResetLink`;
- `SettingsResponse` (with `Settings` mirrored from store), `AuditEntry`, `AuditPage`, `DashboardAccounts`;
- `Error`, `ErrorResponse`, the `Code*` constants and `StatusOf`;
- 04's REST and push DTOs live in the same package (04 §2): the dashboard, doctor, bandwidth, connection-test and
  push-payload types;
- *later (M2)*: `DeviceCodeRequest`, `DeviceCodeResponse`, `DeviceTokenRequest`, `DeviceTokenResponse`,
  `DeviceLookup`, `DeviceDecideRequest`, `DevicePasswordRequest`.

Golden JSON for each DTO goes in `internal/protocol/api/testdata/`.

### 12.6 Routes registered by other documents

| Route | Owner | Access / notes |
|---|---|---|
| `GET /ws` | 01 | The hub does its own Origin and pre-auth checks; credentials through the wiring's `Authenticator` over `auth.AuthenticateCookie` and `Touch` (§7.6) |
| `GET /healthz`, `GET /readyz` | 04 | Public; readyz uses `db.Ping` |
| `POST /api/v1/conntest` | 04 (on 02's probe) | `API.Handle` with `User`: any signed-in user (05 shows fix text to admins only) |
| `GET /api/v1/admin/dashboard` | 04 | `API.Handle` with `Admin`; the `accounts` part comes from `DashboardAccounts` |
| `GET\|POST /api/v1/admin/doctor`, `GET /api/v1/admin/bandwidth` | 04 | `API.Handle` with `Admin` |
| `/metrics` on `127.0.0.1:9469` | 04 | Separate listener |
| SPA routes (every route in 05 §5), `/download`, install scripts | 04, 05, 06 | `/setup` must answer **404** when `auth.SetupAvailable` is false. Every other SPA route is always served |

**Admin socket (04's CLI, 04 §3.1 and §12.2) → functions here**:

| CLI command | Calls |
|---|---|
| `isshoni setup-url` | `SetupAvailable`, then `IssueSetupToken(CLIActor)` |
| `isshoni admin users list` | `db.Read → ListUsers` |
| `isshoni admin users reset-password <name>` | `UserByUsername`, then `IssuePasswordReset(CLIActor, id, "")` |
| `isshoni admin users set-role <name> admin\|user` | `UpdateUser(CLIActor, id, UserChange{Role})` (last-admin rule applies; no password needed) |
| `isshoni admin users disable\|enable <name>` | `UpdateUser(CLIActor, id, UserChange{Status})` |
| `isshoni admin invite create [--uses N] [--ttl DUR]` | `CreateInvite(CLIActor, …)` |
| `isshoni admin backup` | `db.BackupTo` (plus secrets and certs, 04) |

Settings are changed in the admin UI or pinned in the config file; there is no settings CLI in M1.

---

## 13. Security headers (split with 04)

This doc owns two headers; 04 owns every global header and its exact values (04 §9.6: CSP, `Referrer-Policy:
no-referrer`, `nosniff`, `frame-ancestors 'none'`, HSTS rules, COOP, CORP, `Permissions-Policy` with
`display-capture=(self)`, `X-Robots-Tag`).

| Header | Owner | Value |
|---|---|---|
| `Cache-Control: no-store` | **03**, on every `/api/v1` response | — |
| `Set-Cookie` attributes | **03** | §7.4 |

The `/setup`, `/invite`, `/reset` and `/link` pages load nothing from third parties, which 04's CSP enforces, and the
SPA removes the fragment right after reading it (05).

---

## 14. Limits and timeouts

| Item | Value |
|---|---|
| argon2id | m = 19 MiB, t = 2, p = 1; concurrency `max(2, NumCPU/2)`; 32 waiters; 10 s wait → `server_busy`; anonymous hashes: burst 20, 5 per s server-wide (`auth-hash`) → `server_busy` |
| Session | 32-byte token; 30 d idle; 180 d max; rotation 24 h; old-token grace 60 s; touch every 5 min at most; cache 30 s; 50 per user |
| Setup token | 24 h, single use, one live at a time |
| Invite | 1–720 h (default 168); 1–1000 uses (default 10); 100 active per server; 10 active per member |
| Password reset link | 24 h, single use, one per user |
| Pending sign-ups | 50 at most; expire after 14 d |
| Rooms | 200 at most; names 1–40 chars |
| Push subscriptions | 10 per user; endpoint ≤ 2048 bytes |
| Throttles | §7.3; 100,000 keys per bucket map |
| Request bodies | 16 KiB (auth and device), 64 KiB (other) |
| Audit | 30 d retention; detail ≤ 1 KiB; `login_failed` capped at 600 per hour |
| Device flow *(later: M2)* | Code 10 min; poll interval 5 s (+5 s per `slow_down`); access 15 min; refresh 90 d; reuse grace 30 s; 20 devices per user |
| SQLite | busy_timeout 5000 ms; slow-write warning 250 ms; pre-migration backups: the newest 5, plus the newest per schema version |
| Push preferences | defaults `shareStarted: all`, `adminAlerts: true` |

---

## 15. Test plan

**Unit: store** (`go test -race`; real SQLite in `t.TempDir()`; injected clock):
- A new DB migrates to the latest version, and reopening it is a no-op. `PRAGMA user_version` matches.
- Migration files are contiguous from 1 with unique names. Every table is STRICT, and every FK child column has an
  index (a test queries `sqlite_master`).
- **Backups**:
  - Using a fake migration list (v1 → v2 → … → v8), each upgrade from ≥ 1 writes `backups/pre-<old>-<ts>.db`. Only
    the newest 5 remain, plus the newest file of each older `<old>`, and each backup opens and has the old version.
  - **Restart loop**: a DB at v3 and a fake list whose v6 fails. Eight `Open` calls in a row (the injected clock moves
    1 s per call) each return `ErrNeedsOperator`. The first writes `pre-3-*.db` and each later one a `pre-5-*.db`;
    afterwards the `pre-3-*.db` file is still there, next to exactly 5 `pre-5-*.db` files.
  - A new DB writes no backup.
  - Low free space (an injected statfs) fails before any change.
- **Newer schema**: a DB at v9 against a binary that knows v8 gives a `SchemaTooNewError` with `Backup` set to the
  newest `pre-8-*.db`, and a DB at v9 with no backup gives `Backup == ""`. A renamed migration gives the history
  mismatch error.
- **Needs operator**: each case of §4.3 satisfies `errors.Is(err, ErrNeedsOperator)`; a cancelled context does not.
- **File-level functions**: `InspectFile` reports the version, history, integrity and `TooNew` of fixture files
  (current, newer, renamed migration, corrupted page); `InspectFile` and `BackupFile` leave the source byte-identical
  and create no `backups/`; a `BackupFile` copy opens with the same version.
- **Constraints**:
  - FKs are enforced: deleting a user cascades to sessions, devices and push subscriptions, and sets invites'
    `created_by` to NULL.
  - CHECKs hold: `role`, `status`, `uses ≤ max_uses`, and the push `session_id`/`device_id` XOR.
  - Only one default room can exist.
- `UseInvite` under concurrency: 25 goroutines redeem a 10-use invite, and exactly 10 succeed.
- `SessionByTokenHash` accepts the previous hash until `prev_valid_until` and rejects it 1 ms later. It rejects
  sessions past `idle_expires_at` or `expires_at`.
- `Prune`: each rule in §4.7 is checked at `t−1ms` and `t+1ms`.
- A write inside `Read` fails.
- `SettingsCache`: defaults, validation per field, `Pin` → `setting_locked`, `OnChange` receives old and new values,
  and the audit row is written in the same transaction (a failure injected after the audit insert leaves no settings
  change).

**Unit: auth**:
- **PHC**: encoding round-trips and parses foreign parameters. `Verify` works for right and wrong passwords, and the
  rehash flag is set when the parameters differ.
- **Semaphore**: with N = 2, the in-flight maximum never exceeds 2. The 33rd waiter gets `server_busy` at once, and a
  wait over 10 s (fake clock) also gets `server_busy`.
- **Dummy hash**: a login for an unknown user calls the hasher exactly once. A counting hasher asserts this, not
  timing.
- **Username table tests**: fullwidth input, case, `太郎`, Cyrillic, emoji rejected, edge punctuation, length in
  runes, reserved names, and the 128-byte limit. `FuzzNormalizeUsername`: output is idempotent and the key is stable.
- **Password rules**: the length counts runes, OpaqueString handles non-ASCII spaces, a common password is caught
  regardless of case, and a password equal to the username is rejected.
- **Limiter**: burst and refill (fake clock); IPv6 addresses in one /64 share a key (`IPKey` table: IPv4,
  IPv4-mapped IPv6, two addresses in one /64, neighbouring /64s); the map cap evicts full buckets
  first; a successful login refills that address's `auth-user-ip` bucket and leaves `auth-user` as it was.
- **Hash budget**: fake clock and the counting hasher; 1000 logins for random usernames from 1000 random /64s, spread
  over 10 simulated seconds, hash at most 20 + 5 × 10 = 70 times. The others get 503 `server_busy` with
  `Retry-After` ≥ 1 and no hash.
- **Cookies**: the attribute matrix (https: `__Host-`, Secure, HttpOnly, Lax, Path=/, Max-Age; http dev: no prefix and
  no Secure).
- **Rotation**: after 24 h a REST request rotates, a WebSocket upgrade never does, and the old token works for 60 s.
- **CSRF matrix**: `Sec-Fetch-Site` values (`same-origin`, `none`, `same-site`, `cross-site`) × Origin
  (match, mismatch, absent) × method × Content-Type.
- **WebSocket credentials** (the Origin matrix is 01's test now): `AuthenticateCookie` on an upgrade request with a
  valid cookie returns the principal and never rotates, and ignores an `Authorization` header; no cookie →
  `ErrNoCookie`; a disabled or pending user → unauthenticated; `Touch` on a revoked or expired session →
  unauthenticated; a failing last-seen write → nil.
- **Revocation**: for every row of §7.7, a fake `ConnCloser` records the exact selector and reason, and the session
  cache is invalidated.
- **Key fingerprints**: changing the `session` key deletes sessions and device tables; changing the `invite` key
  deletes invites and setup/reset tokens; both write an audit row.
- `DescribeUserAgent` table tests with about 15 real UA strings: Chrome, Edge, Firefox, Safari on macOS/iPhone/iPad,
  Samsung Internet, Android WebView.

**Integration: httpapi** (`httptest.Server`, real store and auth, fake Signal/Push/ConnCloser/AdminAlerter; `goleak`):
- **Setup**: the CLI path issues a token; `setup/check` → 204; `complete` → 201 with the cookie; a second `complete`
  → 404 `setup_unavailable`; `SetupAvailable` becomes false; issuing again → `setup_unavailable`. Two parallel
  `complete` calls leave exactly one admin.
- **Invite mode**: create an invite (the URL has a fragment and the DB holds no plaintext token); check; register 10
  users; the 11th → 410 `invite_used_up`; revoked → 410 `invite_revoked`; expired (clock) → 410 `invite_expired`;
  register without an invite → 403 `invite_required`; a taken username → 409 without using the invite.
- **Approval mode**: register without an invite → 202; login → 403 `account_pending`, and a wrong password → 401; the
  admin approves → login 200; rejecting frees the username; the 51st pending sign-up → 409 `limit_reached`; a pending
  user older than 14 d is pruned; the `signup_pending` alert is coalesced to one per 10 min. Reject all with 3 pending
  → 200 `{rejected: 3}`, the queue is empty, and one `user.signup_rejected {all: true, count: 3}` row is written;
  `all` without the `{"all": true}` body → 404 and nothing is deleted.
- **Closed mode**: invite check, register and invite creation → 403 `registration_closed`.
- **Login throttles** (these tests set `Options.Hashes` large, except where `auth-hash` is emptied):
  - 5 wrong passwords for one username from address A, then the 6th from A → 429 with `Retry-After`, and the hasher
    counter doesn't move;
  - the correct password from address B then → 200: a lockout from A doesn't block B;
  - 5 more addresses with 5 wrong passwords each empty `auth-user`. The correct password from a new address C → 429
    without a hash; from B, which is the `last_ip` of a live session → 200; and from another address in the /64 of a
    live session's IPv6 `last_ip` → 200;
  - the IP bucket blocks the 21st attempt;
  - an unknown username costs exactly one hash;
  - with the `auth-hash` bucket empty, a login → 503 `server_busy` with `Retry-After`, and
    the hasher counter doesn't move;
  - the audit holds `auth.login_failed` rows without unknown usernames, one `auth.throttled` row per scope hit
    (`username` once for the emptied `auth-user`), and none for `auth-user-ip`.
- **Sessions**: the list marks `current`; revoking one → that cookie gets 401; revoke-others keeps the current one;
  logout-everywhere clears the cookie and deletes the user's devices; the 51st session evicts the least recently seen.
- **Passwords**: changing the password keeps the current session and kills the others. An admin reset makes the old
  cookie get 401, makes the old password get 401 `invalid_credentials`, and the reset link logs the user in; the link
  can't be used twice. Resetting another admin without `currentPassword` → 403 `wrong_password`.
- **Roles**: demoting, disabling or deleting the last admin → 409 `last_admin`. Granting admin without the password
  → 403, and with it → 200 plus an `admin_granted` alert. A demoted admin gets 403 on the next admin call (cache
  invalidated).
- **Rooms**: Lounge exists after the first start; `showRoomList` is false with 1 room and true with 2; a
  case-insensitive duplicate name → 409; deleting Lounge → 409; deleting another room calls `Signal.RoomDeleted`; the
  201st room → 409.
- **Push**: an endpoint over 2048 bytes → 422 `too_long`; bad keys → 422 `bad_keys`; a fake `ValidateEndpoint`
  rejection → 422 with its reason; a valid subscription → 201, and the same endpoint again → 200; the 11th
  subscription evicts the oldest; logout deletes the session's subscriptions.
- **Settings**: a bad value → 422 with `fields`; a pinned field → 409; `OnChange` fires; a registration-mode change
  raises an alert and a security event.
- **Audit**: every action in §10 that M1 can trigger is produced by at least one test, with the IP from `ClientIP`.
  Pagination with `before` is stable. Rows older than 30 d are pruned.
- **Cross-cutting**:
  - every `/api/v1` response, including 4xx and 5xx, has `Cache-Control: no-store`;
  - error bodies match the `api.Error` golden JSON;
  - a cross-site POST with a cookie → 403 `csrf_failed`;
  - a POST without JSON Content-Type → 415;
  - an `Authorization` header in M1 → 401;
  - **no GET changes the DB**: run every GET route and compare `PRAGMA data_version` and table checksums before and
    after (the touch and rotation writes are excluded by using a fresh session inside the test window).
- **DTO golden fixtures** in `internal/protocol/api/testdata` round-trip. The tygo drift check (01's CI job) covers
  the TS output.

**E2E**: 03 owns no browser specs. Its flows run in 05's Playwright specs (05 §19.3):
- `setup.spec`: `isshoni setup-url` → the wizard creates the admin → the connection-test step → the invite link;
- `invite.spec`: a second browser context opens the invite, signs up and lands in Lounge;
- `admin.spec`: the approval mode round trip (a pending sign-up → the admin approves);
- `a11y.spec`: axe on the login, invite and setup pages, and the reset page too (a one-line addition in 05).

The rest is covered below the browser: the admin reset link, logout and "log out everywhere" by the integration tests
above; "a logout in tab A redirects tab B" by S33's component test; and mobile sign-up by 05's manual checks M-IOS-1
(invite → sign-up on an iPhone) and M-AND-1.

---

## 16. Interfaces other docs rely on

- **Packages**: `internal/server/store`, `internal/server/auth`, `internal/server/httpapi`, and
  `internal/protocol/api` (a new subpackage owned by 03, added to 01's tygo config).
- **IDs and constants**: `store.UserID`, `SessionID`, `DeviceID`, `InviteID`, `RoomID`, `PushSubID` (12-character
  strings); `store.DefaultRoomID = "lounge"`; `store.Role` (`admin`|`user`); `store.RegistrationMode`.
- **Store**:
  - `store.Open(ctx, store.Options{Path, BackupDir, AppVersion, Readers, Clock, Logger}) (*store.DB, error)`;
  - `*store.SchemaTooNewError{DBVersion, BinaryVersion, LastAppVersion, Backup}` and `store.ErrNeedsOperator` (04:
    exit 78 exactly when `errors.Is(err, store.ErrNeedsOperator)`);
  - `store.LatestSchemaVersion`, `store.InspectFile`, `store.FileInfo` and `store.BackupFile` (04: offline backup,
    restore validation and offline doctor);
  - `(*DB).Read`, `Write`, `Ping`, `QuickCheck`, `SchemaVersion`, `Stats`, `BackupTo`, `Prune`, `Settings`, `Close`;
  - `(*Q).ListUsers` and `store.UserRow` (04: admin socket `GET /v1/users`, fields id, username, role, status,
    createdAt, lastSeenAt);
  - `(*Q).RoomByID`, `ListRooms` (01: room.join validation);
  - `(*Q).ListPushSubscriptions(PushFilter)`, `RecordPushResult`, `DeletePushSubscriptionByID`,
    `DeleteAllPushSubscriptions`, `PrunePushSubscriptions`, `PushPreferences` (04: sender);
  - `(*Q).AddTransfer`, `TransferMonth`, `GetMeta`, `SetMeta` (04: transfer accounting and ops state);
  - `store.Room`, `store.PushSubscription`, `store.PushPreferences`, `store.Actor`, `store.CLIActor`.
- **Settings**: `store.Settings` with its JSON field names; `(*SettingsCache).Get`, `Defaults`, `OnChange`, `Pin`,
  `Update`, `Locked`. Consumers: `MaxParticipantsPerRoom`, `MaxSharesPerRoom`, `MaxShareBitrateKbps` and
  `MinClientVersion` (01, through `Deps.Policy`); `MaxShareBitrateKbps` (02, through `SetLimits`); `UpdateCheck`,
  `TransferAlertGB` (04, through the wiring's `ops.Policy`); `SetupWizardDone` (05).
- **Auth**:
  - `auth.New(ctx, db, auth.Options{Keys, Origins, ClientIP, Conns, Alerts, Argon, Clock, Logger})`;
  - `auth.Keys{Session, Invite}` and `auth.Origins{Primary, Public}`;
  - `auth.Principal{UserID, Username, Role, Method, SessionID, DeviceID, ClientKind}`, `auth.MethodSession`,
    `auth.MethodBearer`;
  - `(*Service).Authenticate(r)`, `(*Service).AuthenticateCookie(r)` and `auth.ErrNoCookie`, `Touch(ctx, p, ip)`,
    `SetupAvailable(ctx)`, `IssueSetupToken(ctx, actor)`, `IssuePasswordReset`, `UpdateUser`, `CreateInvite`,
    `UserByUsername`, `RunJanitor(ctx)`;
  - `auth.ConnCloser{CloseConnections(ConnSelector, reason) int}`, `auth.ConnSelector`, and the `auth.Reason*` codes;
  - `auth.IPKey(netip.Addr) netip.Prefix`, the limiter's per-IP key (04: the wiring test pins `netx.IPKey` to it);
  - `auth.AdminAlerter{AdminAlert(ctx, AdminAlert)}`, `auth.AdminAlert{Kind, Actor, Target, At}`, and the alert kinds
    in §7.11.
- **httpapi**:
  - `httpapi.New(httpapi.Deps{…}) *API`, `(*API).Handle(pattern, Access, handler)`, `(*API).DashboardAccounts`, and
    `httpapi.Public`/`User`/`Admin`;
  - `httpapi.PrincipalFrom` (the JSON helpers are 04's, in the same package);
  - `httpapi.Signal`, `httpapi.NotifyTarget`, `httpapi.RoomPresence`, `httpapi.Push`, `httpapi.InfoSource`, and the
    topic table of §12.5.
- **Wire contract**:
  - every endpoint in §12.3 and its JSON in §12.4;
  - the error envelope `{"error":{code,fields?,params?,retryAfter?,requestId?}}` (the only REST error shape, 04 uses
    it too), the codes and statuses in §12.2 (`api.StatusOf`), and `api.User{id, username, role}`;
  - the cookie name `__Host-isshoni_session` (`isshoni_session` for http dev);
  - the CSRF rule (JSON Content-Type on unsafe methods);
  - the fragment URLs `/setup#t`, `/invite#t`, `/reset#t`, and `/link?code=` (M2).

---

## 17. Depends on

**docs/m1/01-protocol.md**
- The hub's `CloseConnections(sel, code)`, `UpdateUser`, `CloseRoom`, `Notify` and `Snapshot`, which 04's wiring
  adapts to `auth.ConnCloser` and `httpapi.Signal` (01 §15.2, §15.4). The hub closes connections with
  `error{session_revoked|account_disabled, retryable: false}`; clients of a deleted room rejoin the default room.
- The hub owns the WebSocket Origin check, the pre-auth limits and `websocket.Accept`; it calls the wiring's
  `Authenticator` (over `AuthenticateCookie` and `Touch`) at connect and every 5 min.
- *Later (M2)*: bearer tokens arrive in `hello.auth` only (01 D2).
- 01 reads `MinClientVersion`, `MaxParticipantsPerRoom`, `MaxSharesPerRoom` and `MaxShareBitrateKbps` through the
  wiring's `Deps.Policy`, and uses string user and room ids.
- 01's `tygo.yaml` includes `internal/protocol/api`, generated to `web/src/protocol/api.gen.ts`.

**docs/m1/02-sfu.md**
- 02 reads `MaxShareBitrateKbps` (pushed by the wiring with `SetLimits`). Live media data for the dashboard goes to
  04 directly, not through this doc.

**docs/m1/04-server-platform.md**
- Config: `data_dir`; `Site.Origin` (the only public origin: `Origins.Primary` and `Origins.Public`); trusted proxies
  → `ClientIP`.
- `secrets.json` provides 32-byte `session` and `invite` keys; `rotate-secrets` replaces them and restarts the server,
  so §4.6's startup check purges the affected rows.
- Startup: `store.Open` runs before the listeners. 04 prints this doc's message and exits with 78 exactly when
  `errors.Is(err, store.ErrNeedsOperator)`, and with 1 otherwise; `admin restore --offline` accepts a pre-migration
  `.db` file. Offline backup, restore validation and offline doctor use `BackupFile`, `InspectFile` and
  `LatestSchemaVersion`, which `cmd/isshoni` and the wiring pass to 04 as functions.
- Wiring (04 §6.6):
  - `auth.New` runs after `push.New` (its VAPID check) and gets the push service (adapted) as `AdminAlerter`, and
    `RunJanitor` runs under `serve`;
  - the adapters for `auth.ConnCloser`, `httpapi.Signal` and 01's `Authenticator`/`RoomDirectory`/`Policy`;
  - `httpapi.API` is mounted at `/api/v1/` behind 04's global chain, and 04's REST routes register through
    `API.Handle`;
  - `/readyz` uses `db.Ping`;
  - the SPA handler returns 404 for `/setup` when `SetupAvailable` is false;
  - the global security headers (04 §9.6) and the JSON helpers (`WriteJSON`, `WriteError`, `DecodeJSON`).
- The wiring's adapter over 04's push service implements `httpapi.Push` (it converts `store.PushSubscription` to
  `push.Subscription`): the VAPID key, `ValidateEndpoint` (every URL and host rule of §12.4.6, including the DNS
  check and private-address refusal) and `SendTest`. The push service, adapted, is also `auth.AdminAlerter`. It
  sends "X started streaming" using `ListPushSubscriptions` with `Pref: "share_started"`, and deletes
  subscriptions on 404/410/401/403.
- 04 provides an `InfoSource` and owns the admin dashboard endpoint, which embeds `DashboardAccounts`.
- Doctor uses `QuickCheck` and `Stats`. The admin socket commands map as in §12.6. Backup and restore cover the DB,
  `secrets.json` and certs together, because tokens are only valid with the matching keys.
- Settings pinned from TOML (04's policy keys) go through `SettingsCache.Pin` before serving.
- `task dev` sets `public_url` to the Vite origin, so no extra trusted origin is needed.

**docs/m1/05-web-client.md**
- The SPA sends `Content-Type: application/json` on every unsafe request and treats 401 `unauthenticated` as "go to
  login".
- It maps error codes to `errors.<code>` and `fieldErrors.<field>.<code>`, and takes the form rules from
  `/info.accountRules`.
- It reads fragment tokens and removes them with `history.replaceState`.
- It calls `GET /api/v1/me` at start (daily rotation) and upserts the push subscription at start; the VAPID key comes
  from `/info`.
- Pages: the pages in 05 §5, including `/setup`, `/invite`, `/signup`, `/pending`, `/reset`, `/login`, `/account/*`
  and `/admin/*`, plus `/link` in M2. The admin nav badge comes from `me.badges`.

**docs/m1/06-deploy-and-ci.md**
- The license gate allows `golang.org/x/text` (BSD) and the embedded SecLists subset (MIT, listed in
  `deploy/notices/extra.txt` so it reaches `THIRD_PARTY_NOTICES`).
- The privacy page is written from §11.
- Release notes flag every schema version bump.
- install.sh prints the setup link through `isshoni setup-url`.

---

## 18. Implementation slices

Each slice is independently testable and merges on green `go test -race`. Rough total: about 2.5 of M1's 8 weeks.
The integrated plan (`README.md`, ids `Sxx`) orders these with the other docs; slice 14 became part of 04's wiring
slice.

1. **Store foundation**: `store.Open` (DSN, pools, WAL check), the migrator (bookkeeping, forward-only, FK
   procedure, history check), pre-migration backups and rotation, `SchemaTooNewError`, `0001_init.sql`, `meta`,
   `EnsureDefaultRoom`, `Ping`, `QuickCheck`, `Stats`, `BackupTo`, `Close`, `ErrNeedsOperator`, and the file-level
   `LatestSchemaVersion`, `InspectFile` and `BackupFile`.
   *Acceptance*: the store unit tests for migrations, backups, newer-schema refusal and constraints pass; a new DB has
   Lounge; `sqlite3 isshoni.db .schema` matches §5.
2. **Store queries and pruning**: every M1 `*Q` method in §6 and `Prune`.
   *Acceptance*: the remaining store unit tests, including concurrent `UseInvite` and prune boundaries.
3. **API DTOs and the HTTP skeleton**: `internal/protocol/api` (DTOs, `Error`, codes, `StatusOf`, golden JSON, tygo
   wiring); on top of 04's router and JSON helpers: `httpapi.New` with Access levels; the /api/v1 chain (body limits,
   no-store, the CSRF wrapper and Content-Type rule); `GET /info` with fake InfoSource and Push.
   *Acceptance*: the golden tests pass; the CSRF and Content-Type matrix tests pass; `/info` returns the §12.4.1 shape;
   every response has `no-store`.
4. **Auth primitives**: token generation and keyed hashes, key fingerprints and purge, `NormalizeUsername`,
   `CheckPassword` with the embedded list, the argon2 hasher (PHC, semaphore, dummy), the limiter (with `auth-hash`),
   and `DescribeUserAgent`.
   *Acceptance*: the auth unit tests for these parts, including fuzz targets running for 30 s in CI.
5. **Settings**: `SettingsCache` (typed struct, validation, `Pin`, `OnChange`, audit), plus
   `GET/PATCH /admin/settings` (behind a temporary admin fixture).
   *Acceptance*: the settings unit and integration tests.
   *Depends on*: 2, 3.
6. **Sessions and login**: `Authenticate`, `AuthenticateCookie`, the cookie helpers, `MaybeRotate`, the cache,
   `Touch`; `login`, `logout` and `me`, with throttles and audit.
   *Acceptance*: the login, rotation and throttle integration tests; the hasher counter proves there is no hashing
   while throttled; logout and the login-with-an-old-cookie row call `ConnCloser` with that session and the §7.7
   reason (fake `ConnCloser`).
   *Depends on*: 3, 4.
7. **Setup**: `SetupAvailable`, `IssueSetupToken` (exported for 04's CLI), and `setup/check` and `setup/complete`.
   *Acceptance*: the setup integration tests, including the parallel-complete race.
   *Depends on*: 6.
8. **Invites and registration**: invite create, list, revoke and check; `register` in all three modes; the approval
   queue endpoints; the `signup_pending` alert (fake alerter).
   *Acceptance*: the invite, approval and closed-mode integration tests.
   *Depends on*: 5, 6.
9. **Self-service and revocation**: session list and revoke, revoke-others, logout-everywhere, password change,
   `me/delete`, device list and delete (empty), and `ConnCloser` calls for every row of §7.7 that M1 can trigger.
   *Acceptance*: the revocation matrix tests with a fake `ConnCloser`.
   *Depends on*: 6.
10. **Admin users**: list (with `Signal.OnlineUserIDs`), PATCH (rename, role, status, with the password
    requirement), delete, password reset issue and complete, sign-out, the last-admin and self rules, and admin
    alerts.
    *Acceptance*: the roles and reset integration tests.
    *Depends on*: 9.
11. **Rooms**: user and admin room endpoints, `showRoomList`, the Signal hooks and presence counts.
    *Acceptance*: the rooms integration tests.
    *Depends on*: 6.
12. **Push subscriptions and preferences**: subscribe (upsert and validation), unsubscribe, test (this session),
    `GET/PUT /push/preferences`.
    *Acceptance*: the push integration tests with a fake `Push`; preferences round-trip and filter
    `ListPushSubscriptions(Pref)`.
    *Depends on*: 6.
13. **Audit, janitor, dashboard accounts**: `GET /admin/audit` with pagination, `DashboardAccounts` (the `accounts`
    object of 04's dashboard), and `RunJanitor`.
    *Acceptance*: the audit coverage test (every M1 action), the `accounts` golden JSON, and the prune schedule under a
    fake clock.
    *Depends on*: 8–12.
14. **WebSocket credentials** (done inside 04's wiring slice): the `Authenticator`, `ConnCloser` and `Signal`
    adapters over this package.
    *Acceptance*: an in-process test in which 01's hub closes a connection within 100 ms of `POST /api/v1/auth/logout`
    (the §7.7 `logged_out` reason, sent as `session_revoked` on the wire).
    *Depends on*: 6, and 01's hub.
15. ***Later (M2)*: device flow and bearer tokens**: `device/*` endpoints, bearer `Authenticate`, the WebSocket
    bearer path, refresh rotation with reuse detection and grace, Wails CORS, and `me` for devices.
    *Acceptance*: RFC 8628 state tests (pending, slow_down, denied, expired, consumed), reuse revoking the device, the
    grace path, and the bearer paths through CSRF and CORS.
    *Depends on*: all of the above.

---

## 19. Owner decisions

1. **Minimum password length: 8 characters** (owner, 2026-09-30), with the common-password blocklist and the
   throttles in §7. NIST SP 800-63B-4 recommends 15 for a single factor; the owner chose 8 so friends can sign up on
   phones. Changing it later only touches `CheckPassword` and `/info.accountRules`.

Decided at integration: **invites from non-admins** stay behind the setting `membersCanInvite`, default **off**, in
line with the plan's "an admin plus invite links".
