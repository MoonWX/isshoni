package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// goldenSchema is a verbatim copy of the SQL block in docs/m1/03-accounts-and-store.md §5 (schema version 1).
const goldenSchema = "testdata/schema_v1.sql"

// canonicalSQL removes -- comments, collapses whitespace and drops the trailing semicolon. SQLite stores each
// CREATE statement's own text (from CREATE up to the semicolon, comments included), so two statements compare equal
// exactly when they differ only in comments and layout.
func canonicalSQL(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(strings.Join(strings.Fields(b.String()), " "), ";")
}

// splitStatements splits a schema file into canonical statements.
func splitStatements(s string) []string {
	var out []string
	var cur strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			out = append(out, canonicalSQL(cur.String()))
			cur.Reset()
		}
	}
	if rest := canonicalSQL(cur.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// schemaDump is what `sqlite3 isshoni.db .schema` shows, in creation order and canonical form, without the
// migrator's own schema_migrations table and SQLite's internal objects.
func schemaDump(t *testing.T, db *DB) []string {
	t.Helper()
	var out []string
	err := db.Read(context.Background(), func(q *Q) error {
		rows, err := q.tx.QueryContext(q.ctx, `SELECT sql FROM sqlite_master
			WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND tbl_name <> 'schema_migrations' ORDER BY rowid`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, canonicalSQL(s))
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSchemaMatchesGolden: the schema of a new database is exactly 03 §5.
func TestSchemaMatchesGolden(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(goldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	want := splitStatements(string(golden))
	db := newEnv(t).open(embeddedMigrations()[:1])
	got := schemaDump(t, db)
	if len(want) < 20 {
		t.Fatalf("golden has only %d statements", len(want))
	}
	for i := range max(len(got), len(want)) {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Errorf("statement %d:\n got: %s\nwant: %s", i+1, g, w)
		}
	}
}

// TestMigration0001IsFrozen: the first migration is the golden copy, byte for byte. A released migration never
// changes; a schema change is a new migration file.
func TestMigration0001IsFrozen(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(goldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	if m := embeddedMigrations()[0]; m.sql != string(golden) {
		t.Errorf("migrations/%s.sql differs from %s", m.name, goldenSchema)
	}
}

// TestGoldenMatchesSpec: the golden file is the SQL block of 03 §5. Skipped outside a repository checkout.
func TestGoldenMatchesSpec(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "m1", "03-accounts-and-store.md"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("docs/m1 is not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	i := strings.Index(s, "\n## 5. Schema")
	if i < 0 {
		t.Fatal("03 has no §5 Schema heading")
	}
	s = s[i:]
	start := strings.Index(s, "\n```sql\n")
	if start < 0 {
		t.Fatal("03 §5 has no sql block")
	}
	s = s[start+len("\n```sql\n"):]
	end := strings.Index(s, "\n```\n")
	if end < 0 {
		t.Fatal("03 §5's sql block does not end")
	}
	block := s[:end+1]
	golden, err := os.ReadFile(goldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(block), golden) {
		t.Errorf("%s differs from the SQL block of docs/m1/03-accounts-and-store.md §5", goldenSchema)
	}
}

// fkWithoutIndex lists the foreign key child columns of 03 §5 that have no index. It is empty: every child column
// is indexed (03 §15), so deleting a parent row never scans a child table. A new exception needs a reason here.
var fkWithoutIndex []string

// TestSchemaStrictAndIndexedForeignKeys: every table is STRICT, and every foreign key child column is the first
// column of an index (except the documented list above).
func TestSchemaStrictAndIndexedForeignKeys(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	var tables []string
	var notStrict, unindexed []string
	err := db.Read(context.Background(), func(q *Q) error {
		err := queryRows(q, `SELECT name, strict FROM pragma_table_list
			WHERE schema = 'main' AND type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, nil,
			func(rows *sql.Rows) error {
				var name string
				var strict int
				if err := rows.Scan(&name, &strict); err != nil {
					return err
				}
				tables = append(tables, name)
				if strict != 1 {
					notStrict = append(notStrict, name)
				}
				return nil
			})
		if err != nil {
			return err
		}
		for _, table := range tables {
			// The first column of every index of the table.
			leading := map[string]bool{}
			err := queryRows(q, `SELECT ii.name FROM pragma_index_list(?) AS il
				JOIN pragma_index_info(il.name) AS ii WHERE ii.seqno = 0`, []any{table},
				func(rows *sql.Rows) error {
					var col string
					if err := rows.Scan(&col); err != nil {
						return err
					}
					leading[col] = true
					return nil
				})
			if err != nil {
				return err
			}
			err = queryRows(q, `SELECT "from" FROM pragma_foreign_key_list(?)`, []any{table},
				func(rows *sql.Rows) error {
					var col string
					if err := rows.Scan(&col); err != nil {
						return err
					}
					if !leading[col] {
						unindexed = append(unindexed, table+"."+col)
					}
					return nil
				})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 15 {
		t.Fatalf("only %d tables: %v", len(tables), tables)
	}
	if len(notStrict) > 0 {
		t.Errorf("tables that are not STRICT: %v", notStrict)
	}
	slices.Sort(unindexed)
	if !slices.Equal(unindexed, fkWithoutIndex) {
		t.Errorf("foreign key columns without an index: %v\nwant exactly the documented %v", unindexed, fkWithoutIndex)
	}
}

// queryRows runs a query in q and calls scan for each row.
func queryRows(q *Q, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.tx.QueryContext(q.ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// constraintErr asserts that err is an SQLite constraint violation.
func constraintErr(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: succeeded, want a constraint error", what)
		return
	}
	if sqliteCode(err) != sqlite3.SQLITE_CONSTRAINT {
		t.Errorf("%s: %v, want a constraint error", what, err)
	}
}

func TestConstraintsForeignKeys(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	for _, s := range []string{
		`INSERT INTO users (id, username, username_key, role, status, created_via, created_at, updated_at)
		 VALUES ('admin', 'Alex', 'alex', 'admin', 'active', 'setup', 0, 0)`,
		`INSERT INTO invites (id, token_hash, created_by, created_at, expires_at, max_uses, revoked_by)
		 VALUES ('inv', x'01', 'admin', 0, 1, 10, 'admin')`,
		`INSERT INTO users (id, username, username_key, created_via, invite_id, approved_by, created_at, updated_at)
		 VALUES ('sam', 'Sam', 'sam', 'invite', 'inv', 'admin', 0, 0)`,
		`INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at, last_ip,
		   idle_expires_at, expires_at) VALUES ('ses', 'admin', x'01', 'Chrome on Windows', 0, 0, 0, '', 1, 1)`,
		`INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at, last_seen_at,
		   last_ip) VALUES ('dev', 'admin', 'Alex-PC', 'desktop', 'windows', '0.1.0', 'device_flow', 0, 0, '')`,
		`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at) VALUES (x'01', 'dev', 'access', 0, 1)`,
		`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		   request_ip, created_at, expires_at, user_id) VALUES (x'01', x'02', 'desktop', 'n', 'linux', '0.1.0', '', 0, 1, 'admin')`,
		`INSERT INTO push_subscriptions (id, user_id, session_id, endpoint, p256dh, auth_secret, name, created_at)
		 VALUES ('p1', 'admin', 'ses', 'https://push.example/1', 'k', 'a', 'Chrome', 0)`,
		`INSERT INTO push_subscriptions (id, user_id, device_id, endpoint, p256dh, auth_secret, name, created_at)
		 VALUES ('p2', 'admin', 'dev', 'https://push.example/2', 'k', 'a', 'App', 0)`,
		`INSERT INTO push_preferences (user_id, updated_at) VALUES ('admin', 0)`,
		`INSERT INTO password_resets (token_hash, user_id, created_by, created_at, expires_at) VALUES (x'01', 'sam', 'admin', 0, 1)`,
		`INSERT INTO rooms (id, name, name_key, created_by, created_at, updated_at) VALUES ('r1', 'Games', 'games', 'admin', 0, 0)`,
	} {
		mustExecW(t, db, s)
	}

	// A reference to a missing row fails.
	constraintErr(t, "session of a missing user", execW(t, db,
		`INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at, last_ip,
		   idle_expires_at, expires_at) VALUES ('x', 'nobody', x'09', 'n', 0, 0, 0, '', 1, 1)`))

	// Deleting a device cascades to its tokens and push subscriptions.
	mustExecW(t, db, "DELETE FROM devices WHERE id = 'dev'")
	for query, want := range map[string]int{
		"SELECT count(*) FROM device_tokens":                      0,
		"SELECT count(*) FROM push_subscriptions WHERE id = 'p2'": 0,
		"SELECT count(*) FROM push_subscriptions WHERE id = 'p1'": 1,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after deleting the device, %s = %d, want %d", query, n, want)
		}
	}
	mustExecW(t, db, `INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at,
		last_seen_at, last_ip) VALUES ('dev', 'admin', 'Alex-PC', 'desktop', 'windows', '0.1.0', 'password', 0, 0, '')`)

	// Deleting a user cascades to sessions, devices, push subscriptions and preferences, and device codes, and sets
	// the references in invites, users, resets and rooms to NULL.
	mustExecW(t, db, "DELETE FROM users WHERE id = 'admin'")
	for query, want := range map[string]int{
		"SELECT count(*) FROM sessions":                                                0,
		"SELECT count(*) FROM devices":                                                 0,
		"SELECT count(*) FROM push_subscriptions":                                      0,
		"SELECT count(*) FROM push_preferences":                                        0,
		"SELECT count(*) FROM device_codes":                                            0,
		"SELECT count(*) FROM invites WHERE created_by IS NULL AND revoked_by IS NULL": 1,
		"SELECT count(*) FROM users WHERE id = 'sam' AND approved_by IS NULL":          1,
		"SELECT count(*) FROM password_resets WHERE created_by IS NULL":                1,
		"SELECT count(*) FROM rooms WHERE id = 'r1' AND created_by IS NULL":            1,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after deleting the user, %s = %d, want %d", query, n, want)
		}
	}
	// Deleting an invite keeps its users (invite_id → NULL); deleting a user deletes its reset link.
	mustExecW(t, db, "DELETE FROM invites WHERE id = 'inv'")
	if n := queryInt(t, db, "SELECT count(*) FROM users WHERE id = 'sam' AND invite_id IS NULL"); n != 1 {
		t.Error("deleting an invite did not clear users.invite_id")
	}
	mustExecW(t, db, "DELETE FROM users WHERE id = 'sam'")
	if n := queryInt(t, db, "SELECT count(*) FROM password_resets"); n != 0 {
		t.Error("deleting a user kept its password reset")
	}
}

func TestConstraintsChecks(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	mustExecW(t, db, `INSERT INTO users (id, username, username_key, created_via, created_at, updated_at)
		VALUES ('u', 'Alex', 'alex', 'setup', 0, 0)`)
	if n := queryInt(t, db, "SELECT count(*) FROM users WHERE role = 'user' AND status = 'active'"); n != 1 {
		t.Error("users defaults are not role=user, status=active")
	}
	mustExecW(t, db, `INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at,
		last_ip, idle_expires_at, expires_at) VALUES ('s', 'u', x'01', 'n', 0, 0, 0, '', 1, 1)`)
	mustExecW(t, db, `INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at,
		last_seen_at, last_ip) VALUES ('d', 'u', 'n', 'mobile', 'ios', '0.1.0', 'password', 0, 0, '')`)
	mustExecW(t, db, `INSERT INTO invites (id, token_hash, created_at, expires_at, max_uses, uses)
		VALUES ('i', x'01', 0, 1, 10, 10)`)

	cases := map[string]string{ //nolint:gosec // SQL fixtures, not credentials
		"role":             "UPDATE users SET role = 'root'",
		"status":           "UPDATE users SET status = 'banned'",
		"created_via":      "UPDATE users SET created_via = 'magic'",
		"username_key":     `INSERT INTO users (id, username, username_key, created_via, created_at, updated_at) VALUES ('u2', 'ALEX', 'alex', 'signup', 0, 0)`,
		"uses ≤ max_uses":  "UPDATE invites SET uses = 11",
		"uses ≥ 0":         "UPDATE invites SET uses = -1",
		"max_uses ≥ 1":     "UPDATE invites SET max_uses = 0, uses = 0",
		"max_uses ≤ 1000":  "UPDATE invites SET max_uses = 1001",
		"invite hash":      "INSERT INTO invites (id, token_hash, created_at, expires_at, max_uses) VALUES ('i2', x'01', 0, 1, 1)",
		"push neither":     `INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth_secret, name, created_at) VALUES ('p', 'u', 'https://e/1', 'k', 'a', 'n', 0)`,
		"push both":        `INSERT INTO push_subscriptions (id, user_id, session_id, device_id, endpoint, p256dh, auth_secret, name, created_at) VALUES ('p', 'u', 's', 'd', 'https://e/2', 'k', 'a', 'n', 0)`,
		"push preference":  "INSERT INTO push_preferences (user_id, share_started, updated_at) VALUES ('u', 'some', 0)",
		"admin alerts":     "INSERT INTO push_preferences (user_id, admin_alerts, updated_at) VALUES ('u', 2, 0)",
		"client_kind":      "UPDATE devices SET client_kind = 'tv'",
		"os":               "UPDATE devices SET os = 'plan9'",
		"linked_via":       "UPDATE devices SET linked_via = 'magic'",
		"token kind":       "INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at) VALUES (x'01', 'd', 'bearer', 0, 1)",
		"code status":      `INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version, request_ip, created_at, expires_at, status) VALUES (x'01', x'02', 'desktop', 'n', 'linux', 'v', '', 0, 1, 'maybe')`,
		"audit outcome":    "INSERT INTO audit_log (at, action, outcome, actor_kind) VALUES (0, 'x', 'meh', 'cli')",
		"audit actor_kind": "INSERT INTO audit_log (at, action, actor_kind) VALUES (0, 'x', 'robot')",
		"is_default":       "UPDATE rooms SET is_default = 2",
		"strict types":     "UPDATE users SET created_at = 'yesterday'",
		"not null":         "UPDATE users SET username = NULL",
		"session hash":     `INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at, last_ip, idle_expires_at, expires_at) VALUES ('s2', 'u', x'01', 'n', 0, 0, 0, '', 1, 1)`,
		"reset per user":   "INSERT INTO password_resets (token_hash, user_id, created_at, expires_at) VALUES (x'01', 'u', 0, 1), (x'02', 'u', 0, 1)",
	}
	for name, s := range cases {
		constraintErr(t, name, execW(t, db, s))
	}
	// The valid edge values pass.
	mustExecW(t, db, "UPDATE invites SET max_uses = 1000, uses = 0")
	mustExecW(t, db, "UPDATE invites SET max_uses = 1, uses = 1")
	mustExecW(t, db, `INSERT INTO push_subscriptions (id, user_id, session_id, endpoint, p256dh, auth_secret, name,
		created_at) VALUES ('p', 'u', 's', 'https://e/3', 'k', 'a', 'n', 0)`)
	constraintErr(t, "push endpoint", execW(t, db, `INSERT INTO push_subscriptions (id, user_id, device_id, endpoint,
		p256dh, auth_secret, name, created_at) VALUES ('p2', 'u', 'd', 'https://e/3', 'k', 'a', 'n', 0)`))
}

func TestConstraintsOneDefaultRoom(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	constraintErr(t, "second default room", execW(t, db,
		"INSERT INTO rooms (id, name, name_key, is_default, created_at, updated_at) VALUES ('r2', 'Other', 'other', 1, 0, 0)"))
	constraintErr(t, "room name_key", execW(t, db,
		"INSERT INTO rooms (id, name, name_key, created_at, updated_at) VALUES ('r2', 'LOUNGE', 'lounge', 0, 0)"))
	// Many non-default rooms are fine.
	mustExecW(t, db, "INSERT INTO rooms (id, name, name_key, created_at, updated_at) VALUES ('r2', 'A', 'a', 0, 0)")
	mustExecW(t, db, "INSERT INTO rooms (id, name, name_key, created_at, updated_at) VALUES ('r3', 'B', 'b', 0, 0)")
	if n := queryInt(t, db, "SELECT count(*) FROM rooms WHERE is_default = 1"); n != 1 {
		t.Errorf("default rooms = %d", n)
	}
}

func TestAuditIDIsAutoincrement(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	mustExecW(t, db, "INSERT INTO audit_log (at, action, actor_kind) VALUES (0, 'a', 'cli')")
	mustExecW(t, db, "INSERT INTO audit_log (at, action, actor_kind) VALUES (0, 'b', 'cli')")
	mustExecW(t, db, "DELETE FROM audit_log WHERE action = 'b'")
	mustExecW(t, db, "INSERT INTO audit_log (at, action, actor_kind) VALUES (0, 'c', 'cli')")
	// AUTOINCREMENT never reuses an id, so the pagination cursor stays stable across pruning.
	if n := queryInt(t, db, "SELECT id FROM audit_log WHERE action = 'c'"); n != 3 {
		t.Errorf("id after a delete = %d, want 3", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM audit_log WHERE detail = '{}' AND outcome = 'ok' AND ip = ''"); n != 2 {
		t.Error("audit_log defaults")
	}
}
