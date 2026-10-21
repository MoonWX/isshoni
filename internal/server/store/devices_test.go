package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

// ---- fixtures of the device flow (M2 02 §4) ----

// newCode creates a pending device code with CreateDeviceCode: created at the env clock, valid for 10 minutes, with
// opts applied. It returns the code as the store keeps it.
func (e *testEnv) newCode(db *DB, deviceCode, userCode string, opts ...func(*DeviceCode)) DeviceCode {
	e.t.Helper()
	now := e.clock.Now()
	c := DeviceCode{DeviceCodeHash: []byte(deviceCode), UserCodeHash: []byte(userCode), ClientKind: "desktop",
		DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.2.0", RequestIP: "203.0.113.7", CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute)}
	for _, o := range opts {
		o(&c)
	}
	mustWrite(e.t, db, func(q *Q) error { return q.CreateDeviceCode(c) })
	return e.code(db, deviceCode)
}

// code reads a device code by its device-code hash.
func (e *testEnv) code(db *DB, deviceCode string) DeviceCode {
	e.t.Helper()
	var c DeviceCode
	mustRead(e.t, db, func(q *Q) (err error) {
		c, err = q.DeviceCodeByHash([]byte(deviceCode))
		return err
	})
	return c
}

// linkDevice creates a Windows desktop device of u with CreateDevice, created and last seen at at.
func (e *testEnv) linkDevice(db *DB, u UserID, name string, at time.Time) Device {
	e.t.Helper()
	d := Device{UserID: u, Name: name, ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
		LinkedVia: "device_flow", CreatedAt: at, LastIP: "203.0.113.7"}
	mustWrite(e.t, db, func(q *Q) error { return q.CreateDevice(&d) })
	return d
}

// newToken inserts a token of the device: kind access or refresh, hash h, created at the env clock and living for
// life, with opts applied.
func (e *testEnv) newToken(db *DB, d DeviceID, kind, h string, life time.Duration, opts ...func(*DeviceToken)) DeviceToken {
	e.t.Helper()
	now := e.clock.Now()
	tok := DeviceToken{TokenHash: []byte(h), DeviceID: d, Kind: kind, CreatedAt: now, ExpiresAt: now.Add(life)}
	for _, o := range opts {
		o(&tok)
	}
	mustWrite(e.t, db, func(q *Q) error { return q.InsertDeviceToken(tok) })
	return tok
}

// token reads a token row with SQL, whatever its kind and expiry (DeviceTokenByHash filters both).
func tokenRow(t *testing.T, db *DB, h string) (DeviceToken, bool) {
	t.Helper()
	var out DeviceToken
	found := true
	mustRead(t, db, func(q *Q) error {
		var s deviceTokenScan
		err := q.queryOne("token row", `SELECT `+deviceTokenCols+` FROM device_tokens WHERE token_hash = ?`,
			[]any{[]byte(h)}, s.dest()...)
		if errors.Is(err, ErrNotFound) {
			found = false
			return nil
		}
		out = s.token()
		return err
	})
	return out, found
}

// tokenHashes lists the hashes of the device's tokens of kind, sorted.
func tokenHashes(t *testing.T, db *DB, d DeviceID, kind string) []string {
	t.Helper()
	var out []string
	mustRead(t, db, func(q *Q) error {
		return q.queryAll("token hashes", `SELECT token_hash FROM device_tokens WHERE device_id = ? AND kind = ?`,
			[]any{string(d), kind}, func(r scanner) error {
				var h []byte
				if err := r.Scan(&h); err != nil {
					return err
				}
				out = append(out, string(h))
				return nil
			})
	})
	slices.Sort(out)
	return out
}

// ---- migration 0002 ----

// TestMigration0002FromSchema1: a schema-1 file with device rows (as a 0.1.x server could hold them) migrates to
// version 2 with every row intact, after a pre-1-*.db backup; the two columns and the index exist, and the M2
// methods read and change the migrated rows (M2 02 §4.1, §16.1).
func TestMigration0002FromSchema1(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	t0 := e.clock.Now()
	ms := t0.UnixMilli()
	v1, err := Open(ctx, e.opts(embeddedMigrations()[:1]))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`INSERT INTO users (id, username, username_key, password_hash, role, status, created_via, created_at,
		   updated_at) VALUES ('alex', 'Alex', 'alex', 'x', 'admin', 'active', 'setup', ?1, ?1)`,
		`INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at, last_seen_at,
		   last_ip) VALUES ('dev1', 'alex', 'Gaming PC', 'desktop', 'windows', '0.1.9', 'device_flow', ?1, ?1 + 5000,
		   '203.0.113.7')`,
		`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at, spent_at, replaced_by)
		   VALUES (x'a1', 'dev1', 'access', ?1, ?1 + 900000, NULL, NULL),
		          (x'b1', 'dev1', 'refresh', ?1, ?1 + 7776000000, ?1 + 1000, x'b2'),
		          (x'b2', 'dev1', 'refresh', ?1 + 1000, ?1 + 7777000000, NULL, NULL)`,
		`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		   request_ip, created_at, expires_at, interval_s, last_poll_at, status, user_id, decided_at)
		   VALUES (x'c1', x'd1', 'desktop', 'DESKTOP-7F3K2LQ', 'windows', '0.1.9', '203.0.113.7', ?1, ?1 + 600000, 10,
		   ?1 + 2000, 'approved', 'alex', ?1 + 3000)`,
		`INSERT INTO push_subscriptions (id, user_id, device_id, endpoint, p256dh, auth_secret, name, created_at)
		   VALUES ('p1', 'alex', 'dev1', 'https://push.example/1', 'k', 'a', 'App', ?1)`,
	} {
		mustExecW(t, v1, s, ms)
	}
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(time.Minute)
	db := e.open(nil)
	if db.SchemaVersion() != 2 || LatestSchemaVersion() != 2 {
		t.Fatalf("schema %d (latest %d) after the migration, want 2", db.SchemaVersion(), LatestSchemaVersion())
	}
	if recorded, userVersion := fileVersions(t, e.dbPath()); recorded != 2 || userVersion != 2 {
		t.Errorf("DB at %d (user_version %d), want 2", recorded, userVersion)
	}

	// The pre-migration backup is the schema-1 file, device rows included.
	wantBackup := "pre-1-" + e.clock.Now().UTC().Format(backupTimeFormat) + ".db"
	if b := e.backups(); !slices.Equal(b, []string{wantBackup}) {
		t.Fatalf("backups = %v, want [%s]", b, wantBackup)
	}
	backupPath := filepath.Join(e.backupDir(), wantBackup)
	if recorded, userVersion := fileVersions(t, backupPath); recorded != 1 || userVersion != 1 {
		t.Errorf("backup at %d (user_version %d), want 1", recorded, userVersion)
	}
	bk, err := sql.Open("sqlite", sqliteURI(backupPath, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bk.Close() }()
	var backupTokens int
	if err := bk.QueryRowContext(ctx, "SELECT count(*) FROM device_tokens").Scan(&backupTokens); err != nil ||
		backupTokens != 3 {
		t.Errorf("the backup has %d device tokens (%v), want 3", backupTokens, err)
	}

	// The two columns (nullable BLOBs without default) and the index.
	type col struct {
		name, typ string
		notNull   int
		dflt      sql.NullString
	}
	var cols []col
	mustRead(t, db, func(q *Q) error {
		return q.queryAll("table info", `SELECT name, type, "notnull", dflt_value FROM pragma_table_info('device_tokens')
			WHERE name IN ('issued_by', 'exchange_hash') ORDER BY cid`, nil, func(r scanner) error {
			var c col
			if err := r.Scan(&c.name, &c.typ, &c.notNull, &c.dflt); err != nil {
				return err
			}
			cols = append(cols, c)
			return nil
		})
	})
	if want := []col{{name: "issued_by", typ: "BLOB"}, {name: "exchange_hash", typ: "BLOB"}}; !slices.Equal(cols, want) {
		t.Errorf("new columns = %+v, want %+v", cols, want)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM pragma_index_list('device_tokens') il
		JOIN pragma_index_info(il.name) ii WHERE il.name = 'device_tokens_issued_by' AND ii.name = 'issued_by'
		AND ii.seqno = 0`); n != 1 {
		t.Error("index device_tokens_issued_by on device_tokens(issued_by) is missing")
	}

	// Every row is intact, and the new columns are NULL.
	for query, want := range map[string]int{
		"SELECT count(*) FROM users":              1,
		"SELECT count(*) FROM devices":            1,
		"SELECT count(*) FROM device_tokens":      3,
		"SELECT count(*) FROM device_codes":       1,
		"SELECT count(*) FROM push_subscriptions": 1,
		"SELECT count(*) FROM device_tokens WHERE issued_by IS NULL AND exchange_hash IS NULL": 3,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after the migration, %s = %d, want %d", query, n, want)
		}
	}
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	wantDevice := Device{ID: "dev1", UserID: "alex", Name: "Gaming PC", ClientKind: "desktop", OS: "windows",
		AppVersion: "0.1.9", LinkedVia: "device_flow", CreatedAt: t0, LastSeenAt: at(5 * time.Second),
		LastIP: "203.0.113.7"}
	wantSpent := DeviceToken{TokenHash: []byte{0xb1}, DeviceID: "dev1", Kind: "refresh", CreatedAt: t0,
		ExpiresAt: at(90 * 24 * time.Hour), SpentAt: at(time.Second), ReplacedBy: []byte{0xb2}}
	wantCode := DeviceCode{DeviceCodeHash: []byte{0xc1}, UserCodeHash: []byte{0xd1}, ClientKind: "desktop",
		DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.1.9", RequestIP: "203.0.113.7", CreatedAt: t0,
		ExpiresAt: at(10 * time.Minute), Interval: 10 * time.Second, LastPollAt: at(2 * time.Second),
		Status: "approved", UserID: "alex", DecidedAt: at(3 * time.Second)}
	mustRead(t, db, func(q *Q) error {
		tok, d, u, err := q.DeviceTokenByHash([]byte{0xb1}, "refresh", e.clock.Now())
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(tok, wantSpent) || d != wantDevice || u.ID != "alex" || u.Role != RoleAdmin {
			t.Errorf("migrated token:\n got %+v %+v %s\nwant %+v %+v", tok, d, u.ID, wantSpent, wantDevice)
		}
		c, err := q.DeviceCodeByUserCode([]byte{0xd1})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(c, wantCode) {
			t.Errorf("migrated code:\n got %+v\nwant %+v", c, wantCode)
		}
		return nil
	})

	// The migrated chain rotates with the M2 methods.
	mustWrite(t, db, func(q *Q) error {
		if err := q.SpendRefreshToken([]byte{0xb2}, []byte{0xb3}, []byte("x"), e.clock.Now()); err != nil {
			return err
		}
		return q.InsertDeviceToken(DeviceToken{TokenHash: []byte{0xb3}, DeviceID: "dev1", Kind: "refresh",
			ExpiresAt: e.clock.Now().Add(time.Hour), IssuedBy: []byte{0xb2}})
	})
	if tok, _ := tokenRow(t, db, "\xb2"); string(tok.ExchangeHash) != "x" || string(tok.ReplacedBy) != "\xb3" {
		t.Errorf("spent migrated token = %+v", tok)
	}
}

// ---- device codes ----

func TestCreateDeviceCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")

	// Defaults: the store's clock, interval 5 s, status pending, nothing polled or decided.
	mustWrite(t, db, func(q *Q) error {
		return q.CreateDeviceCode(DeviceCode{DeviceCodeHash: []byte("dc1"), UserCodeHash: []byte("uc1"),
			ClientKind: "desktop", DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.0.0-dev+abc",
			RequestIP: "2001:db8::7", ExpiresAt: t0.Add(10 * time.Minute)})
	})
	want := DeviceCode{DeviceCodeHash: []byte("dc1"), UserCodeHash: []byte("uc1"), ClientKind: "desktop",
		DeviceName: "DESKTOP-7F3K2LQ", OS: "windows", AppVersion: "0.0.0-dev+abc", RequestIP: "2001:db8::7",
		CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute), Interval: 5 * time.Second, Status: "pending"}
	mustRead(t, db, func(q *Q) error {
		byHash, err := q.DeviceCodeByHash([]byte("dc1"))
		if err != nil {
			return err
		}
		byUser, err := q.DeviceCodeByUserCode([]byte("uc1"))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(byHash, want) || !reflect.DeepEqual(byUser, want) {
			t.Errorf("DeviceCodeByHash = %+v\nDeviceCodeByUserCode = %+v\nwant %+v", byHash, byUser, want)
		}
		// The two lookups never mix up their columns, and unknown or empty hashes are not found.
		for name, err := range map[string]error{
			"by hash, a user code":      second(q.DeviceCodeByHash([]byte("uc1"))),
			"by user code, a dev code":  second(q.DeviceCodeByUserCode([]byte("dc1"))),
			"by hash, unknown":          second(q.DeviceCodeByHash([]byte("nope"))),
			"by hash, empty":            second(q.DeviceCodeByHash(nil)),
			"by user code, empty":       second(q.DeviceCodeByUserCode([]byte{})),
			"by user code, unknown":     second(q.DeviceCodeByUserCode([]byte("nope"))),
			"by hash, case of the hash": second(q.DeviceCodeByHash([]byte("DC1"))),
		} {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		return nil
	})

	// Every field as given (CreatedAt rounded to milliseconds).
	full := e.newCode(db, "dc2", "uc2", func(c *DeviceCode) {
		c.CreatedAt = t0.Add(1500 * time.Microsecond)
		c.Interval, c.LastPollAt = 7*time.Second, t0.Add(time.Second)
		c.Status, c.UserID, c.DecidedAt = "approved", alex.ID, t0.Add(2*time.Second)
	})
	if !full.CreatedAt.Equal(t0.Add(time.Millisecond)) || full.Interval != 7*time.Second || full.Status != "approved" ||
		full.UserID != alex.ID || !full.DecidedAt.Equal(t0.Add(2*time.Second)) || !full.LastPollAt.Equal(t0.Add(time.Second)) {
		t.Errorf("code with every field = %+v", full)
	}

	// A user-code collision is a ConflictError; a device-code collision is not.
	err := db.Write(context.Background(), func(q *Q) error {
		return q.CreateDeviceCode(DeviceCode{DeviceCodeHash: []byte("dc3"), UserCodeHash: []byte("uc1"),
			ClientKind: "desktop", OS: "windows", ExpiresAt: t0.Add(time.Minute)})
	})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Column != "user_code_hash" || !errors.Is(err, ErrConflict) {
		t.Errorf("user-code collision = %v, want *ConflictError{user_code_hash}", err)
	}
	err = db.Write(context.Background(), func(q *Q) error {
		return q.CreateDeviceCode(DeviceCode{DeviceCodeHash: []byte("dc1"), UserCodeHash: []byte("uc9"),
			ClientKind: "desktop", OS: "windows", ExpiresAt: t0.Add(time.Minute)})
	})
	if err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("device-code collision = %v, want a plain error", err)
	}

	// Missing fields, a sub-second interval and a status the CHECK refuses fail before or at the insert.
	base := DeviceCode{DeviceCodeHash: []byte("dc4"), UserCodeHash: []byte("uc4"), ClientKind: "desktop",
		OS: "windows", ExpiresAt: t0.Add(time.Minute)}
	for name, mod := range map[string]func(*DeviceCode){
		"no device code hash": func(c *DeviceCode) { c.DeviceCodeHash = nil },
		"no user code hash":   func(c *DeviceCode) { c.UserCodeHash = nil },
		"no expiry":           func(c *DeviceCode) { c.ExpiresAt = time.Time{} },
		"interval 500 ms":     func(c *DeviceCode) { c.Interval = 500 * time.Millisecond },
		"negative interval":   func(c *DeviceCode) { c.Interval = -time.Second },
		"status":              func(c *DeviceCode) { c.Status = "maybe" },
		"client kind":         func(c *DeviceCode) { c.ClientKind = "tv" },
		"unknown approver":    func(c *DeviceCode) { c.UserID = "nobody" },
	} {
		c := base
		mod(&c)
		if err := db.Write(context.Background(), func(q *Q) error { return q.CreateDeviceCode(c) }); err == nil {
			t.Errorf("%s: CreateDeviceCode succeeded", name)
		}
	}
	if n := queryInt(t, db, "SELECT count(*) FROM device_codes"); n != 2 {
		t.Errorf("device codes = %d, want 2", n)
	}
}

// second returns the error of a two-value call.
func second[T any](_ T, err error) error { return err }

// TestCountPendingDeviceCodes: pending codes with expires_at > now; a code at its expiry no longer counts.
func TestCountPendingDeviceCodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	exp := t0.Add(10 * time.Minute)
	e.newCode(db, "dc1", "uc1")
	e.newCode(db, "dc2", "uc2", func(c *DeviceCode) { c.ExpiresAt = exp.Add(time.Minute) })
	for i, status := range []string{"approved", "denied", "consumed"} {
		e.newCode(db, "dcx"+itoa(i), "ucx"+itoa(i), func(c *DeviceCode) {
			c.Status, c.UserID, c.DecidedAt = status, alex.ID, t0
		})
	}
	for at, want := range map[time.Time]int{
		t0:                         2,
		exp.Add(-time.Millisecond): 2,
		exp:                        1,
		exp.Add(time.Minute):       0,
	} {
		mustRead(t, db, func(q *Q) error {
			n, err := q.CountPendingDeviceCodes(at)
			if err != nil || n != want {
				t.Errorf("CountPendingDeviceCodes(+%v) = %d, %v, want %d", at.Sub(t0), n, err, want)
			}
			return nil
		})
	}
}

// TestDecideDeviceCode: only a pending, unexpired code is decided; a name replaces the app's, "" keeps it.
func TestDecideDeviceCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	c := e.newCode(db, "dc1", "uc1")
	decide := func(uc, status string, u UserID, name string, at time.Time) error {
		return db.Write(context.Background(), func(q *Q) error {
			return q.DecideDeviceCode([]byte(uc), status, u, name, at)
		})
	}

	// At its expiry the code can no longer be decided; a millisecond before, it can.
	if err := decide("uc1", "approved", alex.ID, "", c.ExpiresAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("decide at expires_at = %v, want ErrNotFound", err)
	}
	at := c.ExpiresAt.Add(-time.Millisecond)
	if err := decide("uc1", "approved", alex.ID, "", at); err != nil {
		t.Fatal(err)
	}
	got := e.code(db, "dc1")
	if got.Status != "approved" || got.UserID != alex.ID || !got.DecidedAt.Equal(at) ||
		got.DeviceName != "DESKTOP-7F3K2LQ" {
		t.Errorf("approved code = %+v (the empty name keeps the app's)", got)
	}
	// Decided already: neither the same nor the other answer changes it.
	for _, status := range []string{"approved", "denied"} {
		if err := decide("uc1", status, alex.ID, "Other", t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("deciding a decided code (%s) = %v, want ErrNotFound", status, err)
		}
	}
	if got2 := e.code(db, "dc1"); !reflect.DeepEqual(got2, got) {
		t.Errorf("a refused decide changed the code: %+v", got2)
	}

	// Deny with a name: the approver's name replaces the app's.
	e.newCode(db, "dc2", "uc2")
	if err := decide("uc2", "denied", alex.ID, "Gaming PC", t0); err != nil {
		t.Fatal(err)
	}
	if got := e.code(db, "dc2"); got.Status != "denied" || got.DeviceName != "Gaming PC" || got.UserID != alex.ID {
		t.Errorf("denied code = %+v", got)
	}

	// Unknown codes, other statuses and a missing approver.
	e.newCode(db, "dc3", "uc3")
	for name, err := range map[string]error{
		"unknown user code": decide("nope", "approved", alex.ID, "", t0),
		"by device code":    decide("dc3", "approved", alex.ID, "", t0),
		"empty hash":        decide("", "approved", alex.ID, "", t0),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
	for name, err := range map[string]error{
		"status pending":   decide("uc3", "pending", alex.ID, "", t0),
		"status consumed":  decide("uc3", "consumed", alex.ID, "", t0),
		"no approver":      decide("uc3", "approved", "", "", t0),
		"unknown approver": decide("uc3", "approved", "nobody", "", t0),
	} {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want an error other than ErrNotFound", name, err)
		}
	}
	if got := e.code(db, "dc3"); got.Status != "pending" {
		t.Errorf("a refused decide changed the code: %+v", got)
	}
}

// TestDecideDeviceCodeParallel: two users decide one code at once; exactly one wins (M2 02 §5.3).
func TestDecideDeviceCodeParallel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	users := []User{e.newUser(db, "Alex"), e.newUser(db, "Sam")}
	e.newCode(db, "dc1", "uc1")
	errs := make([]error, len(users))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Go(func() {
			<-start
			errs[i] = db.Write(context.Background(), func(q *Q) error {
				return q.DecideDeviceCode([]byte("uc1"), "approved", u.ID, "", e.clock.Now())
			})
		})
	}
	close(start)
	wg.Wait()
	winner := slices.IndexFunc(errs, func(err error) bool { return err == nil })
	if winner < 0 || !errors.Is(errs[1-winner], ErrNotFound) {
		t.Fatalf("parallel decides = %v, want one nil and one ErrNotFound", errs)
	}
	if got := e.code(db, "dc1"); got.UserID != users[winner].ID {
		t.Errorf("the code's approver is %s, want the winner %s", got.UserID, users[winner].ID)
	}
}

// TestPollAndConsumeDeviceCode: PollDeviceCode records every poll in any status; ConsumeDeviceCode turns an
// approved, unexpired code into consumed exactly once.
func TestPollAndConsumeDeviceCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	c := e.newCode(db, "dc1", "uc1")
	write := func(fn func(q *Q) error) error { return db.Write(context.Background(), fn) }

	if err := write(func(q *Q) error { return q.PollDeviceCode([]byte("dc1"), t0.Add(time.Second), 10*time.Second) }); err != nil {
		t.Fatal(err)
	}
	if got := e.code(db, "dc1"); !got.LastPollAt.Equal(t0.Add(time.Second)) || got.Interval != 10*time.Second ||
		got.Status != "pending" {
		t.Errorf("polled code = %+v", got)
	}
	// Whole seconds are stored; under a second is refused, and an unknown code is not found.
	if err := write(func(q *Q) error { return q.PollDeviceCode([]byte("dc1"), t0, 15500*time.Millisecond) }); err != nil {
		t.Fatal(err)
	}
	if got := e.code(db, "dc1"); got.Interval != 15*time.Second {
		t.Errorf("interval 15.5 s stored as %v, want 15s", got.Interval)
	}
	if err := write(func(q *Q) error { return q.PollDeviceCode([]byte("dc1"), t0, 0) }); err == nil {
		t.Error("PollDeviceCode with interval 0 succeeded")
	}
	if err := write(func(q *Q) error { return q.PollDeviceCode([]byte("nope"), t0, time.Second) }); !errors.Is(err, ErrNotFound) {
		t.Errorf("PollDeviceCode(unknown) = %v, want ErrNotFound", err)
	}

	consume := func(h string, at time.Time) error {
		return write(func(q *Q) error { return q.ConsumeDeviceCode([]byte(h), at) })
	}
	if err := consume("dc1", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("consuming a pending code = %v, want ErrNotFound", err)
	}
	mustWrite(t, db, func(q *Q) error { return q.DecideDeviceCode([]byte("uc1"), "approved", alex.ID, "", t0) })
	at := c.ExpiresAt.Add(-time.Millisecond)
	if err := consume("dc1", at); err != nil {
		t.Fatal(err)
	}
	if got := e.code(db, "dc1"); got.Status != "consumed" || !got.LastPollAt.Equal(at) || got.UserID != alex.ID {
		t.Errorf("consumed code = %+v", got)
	}
	if err := consume("dc1", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("consuming twice = %v, want ErrNotFound", err)
	}
	// An approved code at its expiry, a denied code and an unknown one are not consumed.
	e.newCode(db, "dc2", "uc2")
	e.newCode(db, "dc3", "uc3")
	mustWrite(t, db, func(q *Q) error {
		if err := q.DecideDeviceCode([]byte("uc2"), "approved", alex.ID, "", t0); err != nil {
			return err
		}
		return q.DecideDeviceCode([]byte("uc3"), "denied", alex.ID, "", t0)
	})
	for name, err := range map[string]error{
		"approved, at expires_at": consume("dc2", c.ExpiresAt),
		"denied":                  consume("dc3", t0),
		"unknown":                 consume("nope", t0),
		"by user code":            consume("uc2", t0),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
	if got := e.code(db, "dc2"); got.Status != "approved" {
		t.Errorf("an expired approved code was consumed: %+v", got)
	}
}

// ---- devices ----

func TestCreateDeviceAndDeviceByID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })

	// Defaults: CreatedAt is the store's clock, LastSeenAt is CreatedAt.
	d := Device{UserID: alex.ID, Name: "Gaming PC", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
		LinkedVia: "password", LastIP: "2001:db8::7"}
	mustWrite(t, db, func(q *Q) error { return q.CreateDevice(&d) })
	if len(d.ID) != 12 || !d.CreatedAt.Equal(t0) || !d.LastSeenAt.Equal(t0) {
		t.Errorf("created device = %+v", d)
	}
	mustRead(t, db, func(q *Q) error {
		got, u, err := q.DeviceByID(d.ID)
		if err != nil {
			return err
		}
		if got != d || u.ID != alex.ID || u.Username != "Alex" || u.Role != RoleAdmin {
			t.Errorf("DeviceByID = %+v, %+v\nwant %+v", got, u, d)
		}
		if list, err := q.ListDevices(alex.ID); err != nil || !slices.Equal(list, []Device{d}) {
			t.Errorf("ListDevices = %+v, %v", list, err)
		}
		if _, _, err := q.DeviceByID("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeviceByID(unknown) = %v, want ErrNotFound", err)
		}
		return nil
	})

	// Times as given, rounded to milliseconds.
	d2 := Device{UserID: alex.ID, Name: "Laptop", ClientKind: "mobile", OS: "android", AppVersion: "1",
		LinkedVia: "device_flow", CreatedAt: t0.Add(1500 * time.Microsecond), LastSeenAt: t0.Add(time.Hour)}
	mustWrite(t, db, func(q *Q) error { return q.CreateDevice(&d2) })
	if !d2.CreatedAt.Equal(t0.Add(time.Millisecond)) || !d2.LastSeenAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("device with times = %+v", d2)
	}

	// The CHECKs and the foreign key refuse bad rows.
	for name, mod := range map[string]func(*Device){
		"client kind":  func(d *Device) { d.ClientKind = "tv" },
		"os":           func(d *Device) { d.OS = "plan9" },
		"linked via":   func(d *Device) { d.LinkedVia = "magic" },
		"unknown user": func(d *Device) { d.UserID = "nobody" },
	} {
		bad := Device{UserID: alex.ID, Name: "x", ClientKind: "desktop", OS: "linux", AppVersion: "1",
			LinkedVia: "device_flow"}
		mod(&bad)
		if err := db.Write(context.Background(), func(q *Q) error { return q.CreateDevice(&bad) }); err == nil {
			t.Errorf("%s: CreateDevice succeeded", name)
		}
	}
	// A colliding ID is retried once with another one.
	e2 := newEnv(t)
	o := e2.opts(nil)
	o.newID = idSequence("dup000000000", "dup000000000", "dup000000000", "fresh0000000")
	db2, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	sam := e2.newUser(db2, "Sam") // takes the first ID
	first := Device{UserID: sam.ID, Name: "a", ClientKind: "desktop", OS: "linux", AppVersion: "1",
		LinkedVia: "device_flow"}
	retried := first
	mustWrite(t, db2, func(q *Q) error {
		if err := q.CreateDevice(&first); err != nil {
			return err
		}
		return q.CreateDevice(&retried)
	})
	if first.ID != "dup000000000" || retried.ID != "fresh0000000" {
		t.Errorf("device IDs = %s, %s, want the retry to take a fresh ID", first.ID, retried.ID)
	}
}

func TestRenameAndTouchDevice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	sam := e.newUser(db, "Sam")
	d := e.linkDevice(db, alex.ID, "DESKTOP-7F3K2LQ", t0)

	mustWrite(t, db, func(q *Q) error {
		got, err := q.RenameDevice(alex.ID, d.ID, "Gaming PC")
		if err != nil {
			return err
		}
		want := d
		want.Name = "Gaming PC"
		if got != want {
			t.Errorf("RenameDevice = %+v, want %+v", got, want)
		}
		if _, err := q.RenameDevice(sam.ID, d.ID, "Stolen"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RenameDevice of another user's device = %v, want ErrNotFound", err)
		}
		if _, err := q.RenameDevice(alex.ID, "nope", "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RenameDevice(unknown) = %v, want ErrNotFound", err)
		}
		return nil
	})

	device := func() Device {
		var out Device
		mustRead(t, db, func(q *Q) (err error) {
			out, _, err = q.DeviceByID(d.ID)
			return err
		})
		return out
	}
	if got := device(); got.Name != "Gaming PC" {
		t.Errorf("name after the refused renames = %q", got.Name)
	}
	touch := func(ip, version string, at time.Time) error {
		return db.Write(context.Background(), func(q *Q) error { return q.TouchDevice(d.ID, ip, version, at) })
	}
	if err := touch("198.51.100.4", "0.2.1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := device(); !got.LastSeenAt.Equal(t0.Add(time.Hour)) || got.LastIP != "198.51.100.4" ||
		got.AppVersion != "0.2.1" || !got.CreatedAt.Equal(t0) {
		t.Errorf("touched device = %+v", got)
	}
	// "" keeps the stored IP and version.
	if err := touch("", "", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := device(); !got.LastSeenAt.Equal(t0.Add(2*time.Hour)) || got.LastIP != "198.51.100.4" ||
		got.AppVersion != "0.2.1" {
		t.Errorf("device touched with empty values = %+v", got)
	}
	if err := db.Write(context.Background(), func(q *Q) error {
		return q.TouchDevice("nope", "", "", t0)
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchDevice(unknown) = %v, want ErrNotFound", err)
	}
}

// TestTrimDevices: TrimDevices(u, 19) evicts the least recently seen devices beyond 19 (ties: the oldest created)
// with their tokens and push subscriptions, and returns them, least recently seen first (M2 02 §6.7).
func TestTrimDevices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	sam := e.newUser(db, "Sam")
	var devs []Device
	for i := range 21 {
		devs = append(devs, e.linkDevice(db, alex.ID, "PC "+itoa(i), t0.Add(time.Duration(i)*time.Minute)))
	}
	// Device 1 was seen as recently as device 0, but created later: device 0 goes first.
	mustWrite(t, db, func(q *Q) error { return q.TouchDevice(devs[1].ID, "", "", t0) })
	devs[1].LastSeenAt = t0
	samDev := e.linkDevice(db, sam.ID, "Sam's PC", t0.Add(-time.Hour))
	e.newToken(db, devs[0].ID, "refresh", "r0", time.Hour)
	e.newToken(db, devs[1].ID, "access", "a1", time.Hour)
	mustExecW(t, db, `INSERT INTO push_subscriptions (id, user_id, device_id, endpoint, p256dh, auth_secret, name,
		created_at) VALUES ('p0', ?, ?, 'https://push.example/0', 'k', 'a', 'App', 0)`, string(alex.ID),
		string(devs[0].ID))

	var trimmed []Device
	mustWrite(t, db, func(q *Q) (err error) {
		trimmed, err = q.TrimDevices(alex.ID, 19)
		return err
	})
	if want := devs[:2]; !slices.Equal(trimmed, want) {
		t.Errorf("TrimDevices(19) =\n %+v\nwant\n %+v", trimmed, want)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM devices WHERE user_id = '" + string(alex.ID) + "'": 19,
		"SELECT count(*) FROM devices WHERE id = '" + string(samDev.ID) + "'":    1,
		"SELECT count(*) FROM device_tokens":                                     0,
		"SELECT count(*) FROM push_subscriptions":                                0,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after TrimDevices, %s = %d, want %d", query, n, want)
		}
	}
	mustWrite(t, db, func(q *Q) error {
		if again, err := q.TrimDevices(alex.ID, 19); err != nil || len(again) != 0 {
			t.Errorf("a second TrimDevices(19) = %+v, %v, want none", again, err)
		}
		// A negative keep is 0: every device of the user goes.
		all, err := q.TrimDevices(sam.ID, -1)
		if err != nil || !slices.Equal(all, []Device{samDev}) {
			t.Errorf("TrimDevices(sam, -1) = %+v, %v", all, err)
		}
		return nil
	})
}

// ---- device tokens ----

func TestDeviceTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	d := e.linkDevice(db, alex.ID, "Gaming PC", t0)
	access := e.newToken(db, d.ID, "access", "a1", 15*time.Minute, func(tok *DeviceToken) {
		tok.IssuedBy = []byte("r0")
	})
	spent := e.newToken(db, d.ID, "refresh", "r0", 90*24*time.Hour, func(tok *DeviceToken) {
		tok.CreatedAt = t0.Add(1500 * time.Microsecond) // rounded to the millisecond
		tok.SpentAt, tok.ReplacedBy, tok.ExchangeHash = t0.Add(time.Minute), []byte("r1"), []byte("x0")
	})
	spent.CreatedAt = t0.Add(time.Millisecond)

	get := func(h, kind string, at time.Time) (DeviceToken, Device, User, error) {
		var tok DeviceToken
		var dev Device
		var u User
		err := db.Read(context.Background(), func(q *Q) (err error) {
			tok, dev, u, err = q.DeviceTokenByHash([]byte(h), kind, at)
			return err
		})
		return tok, dev, u, err
	}
	tok, dev, u, err := get("a1", "access", t0)
	if err != nil || !reflect.DeepEqual(tok, access) || dev != d || u.ID != alex.ID {
		t.Errorf("DeviceTokenByHash(access) = %+v %+v %s %v\nwant %+v", tok, dev, u.ID, err, access)
	}
	// Spent tokens are returned too (reuse detection reads them).
	if tok, _, _, err := get("r0", "refresh", t0.Add(time.Hour)); err != nil || !reflect.DeepEqual(tok, spent) {
		t.Errorf("DeviceTokenByHash(spent refresh) = %+v, %v\nwant %+v", tok, err, spent)
	}
	// Unexpired means expires_at > now: at its expiry a token is gone.
	if _, _, _, err := get("a1", "access", access.ExpiresAt.Add(-time.Millisecond)); err != nil {
		t.Errorf("a millisecond before its expiry: %v", err)
	}
	for name, err := range map[string]error{
		"at expires_at": fourth(get("a1", "access", access.ExpiresAt)),
		"wrong kind":    fourth(get("a1", "refresh", t0)),
		"unknown":       fourth(get("zz", "access", t0)),
		"empty hash":    fourth(get("", "access", t0)),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}

	// Required fields, the CHECK on kind, the foreign key and a duplicate hash.
	for name, tok := range map[string]DeviceToken{
		"no hash":       {DeviceID: d.ID, Kind: "access", ExpiresAt: t0},
		"no device":     {TokenHash: []byte("n1"), Kind: "access", ExpiresAt: t0},
		"no kind":       {TokenHash: []byte("n2"), DeviceID: d.ID, ExpiresAt: t0},
		"no expiry":     {TokenHash: []byte("n3"), DeviceID: d.ID, Kind: "access"},
		"bad kind":      {TokenHash: []byte("n4"), DeviceID: d.ID, Kind: "bearer", ExpiresAt: t0},
		"unknown dev":   {TokenHash: []byte("n5"), DeviceID: "nope", Kind: "access", ExpiresAt: t0},
		"existing hash": {TokenHash: []byte("a1"), DeviceID: d.ID, Kind: "access", ExpiresAt: t0},
	} {
		if err := db.Write(context.Background(), func(q *Q) error { return q.InsertDeviceToken(tok) }); err == nil {
			t.Errorf("%s: InsertDeviceToken succeeded", name)
		}
	}
}

// fourth returns the error of a four-value call.
func fourth[A, B, C any](_ A, _ B, _ C, err error) error { return err }

// TestSpendRefreshToken: a refresh token is spent once; a second spend, an access token or an unknown hash gives
// ErrConflict.
func TestSpendRefreshToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	d := e.linkDevice(db, e.newUser(db, "Alex").ID, "PC", t0)
	e.newToken(db, d.ID, "refresh", "r0", time.Hour)
	e.newToken(db, d.ID, "refresh", "s0", time.Hour)
	e.newToken(db, d.ID, "access", "a0", time.Hour)
	spend := func(h, replacedBy string, exchange []byte, at time.Time) error {
		return db.Write(context.Background(), func(q *Q) error {
			return q.SpendRefreshToken([]byte(h), []byte(replacedBy), exchange, at)
		})
	}

	if err := spend("r0", "r1", []byte("x1"), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if tok, _ := tokenRow(t, db, "r0"); !tok.SpentAt.Equal(t0.Add(time.Second)) || string(tok.ReplacedBy) != "r1" ||
		string(tok.ExchangeHash) != "x1" {
		t.Errorf("spent token = %+v", tok)
	}
	// Without an exchangeId the column stays NULL.
	if err := spend("s0", "s1", nil, t0); err != nil {
		t.Fatal(err)
	}
	if tok, _ := tokenRow(t, db, "s0"); tok.ExchangeHash != nil || string(tok.ReplacedBy) != "s1" {
		t.Errorf("token spent without an exchange id = %+v", tok)
	}
	for name, err := range map[string]error{
		"twice":         spend("r0", "r2", []byte("x2"), t0.Add(2*time.Second)),
		"access token":  spend("a0", "r2", nil, t0),
		"unknown token": spend("zz", "r2", nil, t0),
	} {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v, want ErrConflict", name, err)
		}
		var ce *ConflictError
		if errors.As(err, &ce) {
			t.Errorf("%s: %v is a ConflictError; no UNIQUE column was involved", name, err)
		}
	}
	if tok, _ := tokenRow(t, db, "r0"); string(tok.ReplacedBy) != "r1" || !tok.SpentAt.Equal(t0.Add(time.Second)) {
		t.Errorf("a refused spend changed the token: %+v", tok)
	}
	if tok, _ := tokenRow(t, db, "a0"); !tok.SpentAt.IsZero() {
		t.Errorf("an access token was spent: %+v", tok)
	}
	if err := spend("s0", "", nil, t0); err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("spend without replacedBy = %v, want a plain error", err)
	}
}

// TestSpendRefreshTokenParallel: 25 requests spend one refresh token at once; exactly one wins and the others get
// ErrConflict (M2 02 §4.2: spent in parallel).
func TestSpendRefreshTokenParallel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	d := e.linkDevice(db, e.newUser(db, "Alex").ID, "PC", e.clock.Now())
	e.newToken(db, d.ID, "refresh", "r0", time.Hour)
	const n = 25
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = db.Write(context.Background(), func(q *Q) error {
				return q.SpendRefreshToken([]byte("r0"), []byte("child"+itoa(i)), nil, e.clock.Now())
			})
		})
	}
	close(start)
	wg.Wait()
	winners := 0
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			winner = i
		case !errors.Is(err, ErrConflict):
			t.Errorf("request %d: %v, want nil or ErrConflict", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d requests spent the token, want exactly 1: %v", winners, errs)
	}
	if tok, _ := tokenRow(t, db, "r0"); string(tok.ReplacedBy) != "child"+itoa(winner) {
		t.Errorf("replaced_by = %q, want the winner's child%d", tok.ReplacedBy, winner)
	}
}

// TestSupersedeAndSetReplacedBy: the grace path's two writes (M2 02 §6.2).
func TestSupersedeAndSetReplacedBy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	d := e.linkDevice(db, e.newUser(db, "Alex").ID, "PC", t0)
	// P was spent for C (with an exchange id); C is unspent. The fixture gives C an exchange hash so that the NULL
	// written by SupersedeRefreshToken shows.
	e.newToken(db, d.ID, "refresh", "P", time.Hour, func(tok *DeviceToken) {
		tok.SpentAt, tok.ReplacedBy, tok.ExchangeHash = t0, []byte("C"), []byte("x")
	})
	e.newToken(db, d.ID, "refresh", "C", time.Hour, func(tok *DeviceToken) {
		tok.IssuedBy, tok.ExchangeHash = []byte("P"), []byte("stale")
	})
	write := func(fn func(q *Q) error) error { return db.Write(context.Background(), fn) }

	if err := write(func(q *Q) error { return q.SupersedeRefreshToken([]byte("C"), []byte("C2"), t0.Add(time.Minute)) }); err != nil {
		t.Fatal(err)
	}
	c, ok := tokenRow(t, db, "C")
	if !ok || !c.SpentAt.Equal(t0.Add(time.Minute)) || string(c.ReplacedBy) != "C2" || c.ExchangeHash != nil ||
		string(c.IssuedBy) != "P" {
		t.Errorf("superseded child = %+v (found %v); want it kept as a spent row", c, ok)
	}
	for name, err := range map[string]error{
		"a spent token":  write(func(q *Q) error { return q.SupersedeRefreshToken([]byte("C"), []byte("C3"), t0) }),
		"an unknown one": write(func(q *Q) error { return q.SupersedeRefreshToken([]byte("zz"), []byte("C3"), t0) }),
	} {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("SupersedeRefreshToken on %s = %v, want ErrConflict", name, err)
		}
	}
	if err := write(func(q *Q) error { return q.SupersedeRefreshToken([]byte("C"), nil, t0) }); err == nil ||
		errors.Is(err, ErrConflict) {
		t.Errorf("SupersedeRefreshToken without replacedBy = %v, want a plain error", err)
	}

	if err := write(func(q *Q) error { return q.SetReplacedBy([]byte("P"), []byte("C2")) }); err != nil {
		t.Fatal(err)
	}
	if p, _ := tokenRow(t, db, "P"); string(p.ReplacedBy) != "C2" || string(p.ExchangeHash) != "x" || !p.SpentAt.Equal(t0) {
		t.Errorf("parent after SetReplacedBy = %+v", p)
	}
	e.newToken(db, d.ID, "refresh", "U", time.Hour)
	e.newToken(db, d.ID, "access", "A", time.Hour)
	for name, h := range map[string]string{"an unspent token": "U", "an access token": "A", "an unknown one": "zz"} {
		if err := write(func(q *Q) error { return q.SetReplacedBy([]byte(h), []byte("C2")) }); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetReplacedBy on %s = %v, want ErrNotFound", name, err)
		}
	}
}

// TestDeleteAccessTokensIssuedBy: only the access tokens of one exchange go; the issuer's refresh rows stay.
func TestDeleteAccessTokensIssuedBy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	d := e.linkDevice(db, e.newUser(db, "Alex").ID, "PC", t0)
	issued := func(by string) func(*DeviceToken) {
		return func(tok *DeviceToken) { tok.IssuedBy = []byte(by) }
	}
	e.newToken(db, d.ID, "refresh", "P", time.Hour, func(tok *DeviceToken) { tok.SpentAt = t0 })
	e.newToken(db, d.ID, "access", "aP1", time.Hour, issued("P"))
	e.newToken(db, d.ID, "access", "aP2", time.Hour, issued("P"))
	e.newToken(db, d.ID, "refresh", "C", time.Hour, issued("P"))
	e.newToken(db, d.ID, "access", "aQ", time.Hour, issued("Q"))
	e.newToken(db, d.ID, "access", "a0", time.Hour) // the device-code grant's pair: issued_by NULL
	mustWrite(t, db, func(q *Q) error {
		if n, err := q.DeleteAccessTokensIssuedBy([]byte("P")); err != nil || n != 2 {
			t.Errorf("DeleteAccessTokensIssuedBy(P) = %d, %v, want 2", n, err)
		}
		if n, err := q.DeleteAccessTokensIssuedBy([]byte("P")); err != nil || n != 0 {
			t.Errorf("a second DeleteAccessTokensIssuedBy(P) = %d, %v, want 0", n, err)
		}
		if n, err := q.DeleteAccessTokensIssuedBy(nil); err != nil || n != 0 {
			t.Errorf("DeleteAccessTokensIssuedBy(nil) = %d, %v, want 0 (the NULL pair stays)", n, err)
		}
		return nil
	})
	if got := tokenHashes(t, db, d.ID, "access"); !slices.Equal(got, []string{"a0", "aQ"}) {
		t.Errorf("access tokens left = %v", got)
	}
	if got := tokenHashes(t, db, d.ID, "refresh"); !slices.Equal(got, []string{"C", "P"}) {
		t.Errorf("refresh tokens left = %v, want the issuer and its refresh child", got)
	}
}

// TestTrimAccessTokens: TrimAccessTokens(device, 5) keeps the 5 newest access tokens of that device only.
func TestTrimAccessTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	d := e.linkDevice(db, alex.ID, "PC", t0)
	other := e.linkDevice(db, alex.ID, "Laptop", t0)
	for i := range 7 {
		e.newToken(db, d.ID, "access", "a"+itoa(i), 15*time.Minute, func(tok *DeviceToken) {
			tok.CreatedAt = t0.Add(time.Duration(i) * time.Second)
			tok.ExpiresAt = tok.CreatedAt.Add(15 * time.Minute)
		})
	}
	e.newToken(db, d.ID, "refresh", "r0", time.Hour, func(tok *DeviceToken) { tok.CreatedAt = t0.Add(-time.Hour) })
	e.newToken(db, d.ID, "refresh", "r1", time.Hour)
	for i := range 3 {
		e.newToken(db, other.ID, "access", "o"+itoa(i), time.Hour)
	}
	mustWrite(t, db, func(q *Q) error {
		if n, err := q.TrimAccessTokens(d.ID, 5); err != nil || n != 2 {
			t.Errorf("TrimAccessTokens(5) = %d, %v, want 2", n, err)
		}
		if n, err := q.TrimAccessTokens(d.ID, 5); err != nil || n != 0 {
			t.Errorf("a second TrimAccessTokens(5) = %d, %v, want 0", n, err)
		}
		return nil
	})
	if got := tokenHashes(t, db, d.ID, "access"); !slices.Equal(got, []string{"a2", "a3", "a4", "a5", "a6"}) {
		t.Errorf("access tokens left = %v, want the 5 newest", got)
	}
	if got := tokenHashes(t, db, d.ID, "refresh"); !slices.Equal(got, []string{"r0", "r1"}) {
		t.Errorf("refresh tokens left = %v", got)
	}
	if got := tokenHashes(t, db, other.ID, "access"); len(got) != 3 {
		t.Errorf("the other device's access tokens = %v", got)
	}
	mustWrite(t, db, func(q *Q) error {
		if n, err := q.TrimAccessTokens(d.ID, 0); err != nil || n != 5 {
			t.Errorf("TrimAccessTokens(0) = %d, %v, want 5", n, err)
		}
		return nil
	})
}

// TestDeviceWritesInsideRead: every device write method fails inside Read and changes nothing.
func TestDeviceWritesInsideRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	alex := e.newUser(db, "Alex")
	d := e.linkDevice(db, alex.ID, "PC", now)
	e.newToken(db, d.ID, "refresh", "r0", time.Hour)
	e.newCode(db, "dc1", "uc1")
	before := queryInt(t, db, `SELECT (SELECT count(*) FROM devices) + (SELECT count(*) FROM device_tokens) +
		(SELECT count(*) FROM device_codes)`)
	mustRead(t, db, func(q *Q) error {
		calls := map[string]func() error{
			"CreateDeviceCode": func() error {
				return q.CreateDeviceCode(DeviceCode{DeviceCodeHash: []byte("x"), UserCodeHash: []byte("y"),
					ClientKind: "desktop", OS: "windows", ExpiresAt: now})
			},
			"DecideDeviceCode":  func() error { return q.DecideDeviceCode([]byte("uc1"), "approved", alex.ID, "", now) },
			"PollDeviceCode":    func() error { return q.PollDeviceCode([]byte("dc1"), now, time.Second) },
			"ConsumeDeviceCode": func() error { return q.ConsumeDeviceCode([]byte("dc1"), now) },
			"CreateDevice": func() error {
				return q.CreateDevice(&Device{UserID: alex.ID, Name: "x", ClientKind: "desktop", OS: "linux",
					AppVersion: "1", LinkedVia: "device_flow"})
			},
			"RenameDevice": func() error { _, err := q.RenameDevice(alex.ID, d.ID, "y"); return err },
			"TrimDevices":  func() error { _, err := q.TrimDevices(alex.ID, 0); return err },
			"TouchDevice":  func() error { return q.TouchDevice(d.ID, "", "", now) },
			"InsertDeviceToken": func() error {
				return q.InsertDeviceToken(DeviceToken{TokenHash: []byte("t"), DeviceID: d.ID, Kind: "access",
					ExpiresAt: now})
			},
			"SpendRefreshToken":          func() error { return q.SpendRefreshToken([]byte("r0"), []byte("r1"), nil, now) },
			"SetReplacedBy":              func() error { return q.SetReplacedBy([]byte("r0"), []byte("r1")) },
			"SupersedeRefreshToken":      func() error { return q.SupersedeRefreshToken([]byte("r0"), []byte("r1"), now) },
			"DeleteAccessTokensIssuedBy": func() error { _, err := q.DeleteAccessTokensIssuedBy([]byte("r0")); return err },
			"TrimAccessTokens":           func() error { _, err := q.TrimAccessTokens(d.ID, 0); return err },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, errReadOnlyTx) {
				t.Errorf("%s inside Read = %v, want errReadOnlyTx", name, err)
			}
		}
		return nil
	})
	after := queryInt(t, db, `SELECT (SELECT count(*) FROM devices) + (SELECT count(*) FROM device_tokens) +
		(SELECT count(*) FROM device_codes)`)
	if after != before {
		t.Errorf("rows %d after writes inside Read, want %d", after, before)
	}
	if got := e.code(db, "dc1"); got.Status != "pending" || !got.LastPollAt.IsZero() {
		t.Errorf("the code changed inside Read: %+v", got)
	}
	if tok, _ := tokenRow(t, db, "r0"); !tok.SpentAt.IsZero() {
		t.Errorf("the token changed inside Read: %+v", tok)
	}
}
