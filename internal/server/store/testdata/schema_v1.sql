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
