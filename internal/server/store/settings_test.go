package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// adminActor is an admin changing settings in the UI.
var adminActor = Actor{Kind: ActorUser, UserID: "k3m9p2qxw7ht", Name: "Alex", IP: "192.0.2.10"}

// patch builds an Update patch from name/value pairs; each value is marshalled, except raw JSON given as
// json.RawMessage, which goes in as it is.
func patch(t *testing.T, kv ...any) map[string]json.RawMessage {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatalf("patch: odd number of arguments %v", kv)
	}
	p := map[string]json.RawMessage{}
	for pair := range slices.Chunk(kv, 2) {
		name, value := pair[0].(string), pair[len(pair)-1]
		if raw, ok := value.(json.RawMessage); ok {
			p[name] = raw
			continue
		}
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		p[name] = b
	}
	return p
}

// settingsRows returns the settings table as key → JSON value.
func settingsRows(t *testing.T, db *DB) map[string]string {
	t.Helper()
	rows := map[string]string{}
	mustRead(t, db, func(q *Q) error {
		return q.queryAll("test", `SELECT key, value FROM settings`, nil, func(r scanner) error {
			var k, v string
			if err := r.Scan(&k, &v); err != nil {
				return err
			}
			rows[k] = v
			return nil
		})
	})
	return rows
}

// settingsAudit returns the settings.changed rows, newest first.
func settingsAudit(t *testing.T, db *DB) []AuditEntry {
	t.Helper()
	var out []AuditEntry
	mustRead(t, db, func(q *Q) error {
		var err error
		out, err = q.ListAudit(AuditQuery{ActionPrefix: "settings."})
		return err
	})
	return out
}

// detailJSON re-marshals an audit detail for comparison (json.Marshal sorts map keys).
func detailJSON(t *testing.T, d map[string]any) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// apiError asserts that err is an *api.Error with the given code and returns it.
func apiError(t *testing.T, err error, code string) *api.Error {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("error = %v, want *api.Error %s", err, code)
	}
	return ae
}

// changeRecorder records OnChange calls.
type changeRecorder struct {
	mu    sync.Mutex
	calls [][2]Settings
}

func (r *changeRecorder) fn(prev, next Settings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]Settings{prev, next})
}

func (r *changeRecorder) take() [][2]Settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// TestSettingsRegistry: the registry has every Settings field in struct order, with its JSON name and the dotted
// key of 03 §9 (04's policy keys among them), and it mirrors api.Settings.
func TestSettingsRegistry(t *testing.T) {
	t.Parallel()
	wantKeys := map[string]string{
		"serverName":             "server.name",
		"registrationMode":       "registration.mode",
		"inviteDefaultTtlHours":  "invites.default_ttl_hours",
		"inviteDefaultMaxUses":   "invites.default_max_uses",
		"membersCanInvite":       "invites.members_can_create",
		"maxParticipantsPerRoom": "limits.max_participants_per_room",
		"maxSharesPerRoom":       "limits.max_shares_per_room",
		"maxShareBitrateKbps":    "limits.max_bitrate_kbps",
		"transferAlertGb":        "limits.transfer_alert_gb",
		"updateCheck":            "updates.release_check",
		"minClientVersion":       "clients.min_version",
		"setupWizardDone":        "setup.wizard_done",
	}
	st := reflect.TypeFor[Settings]()
	if st.NumField() != len(settingFields) || len(settingFields) != len(wantKeys) {
		t.Fatalf("Settings has %d fields, the registry %d, the table %d", st.NumField(), len(settingFields),
			len(wantKeys))
	}
	if len(settingByName) != len(settingFields) || len(settingByKey) != len(settingFields) {
		t.Fatal("duplicate name or key in the registry")
	}
	for i, f := range settingFields {
		sf := st.Field(i)
		if tag := sf.Tag.Get("json"); tag != f.name {
			t.Errorf("field %d: registry name %q, struct %s has json %q", i, f.name, sf.Name, tag)
		}
		if wantKeys[f.name] != f.key {
			t.Errorf("%s: key %q, want %q", f.name, f.key, wantKeys[f.name])
		}
		// get and set reach exactly this struct field.
		var s Settings
		v := f.get(&s)
		one := map[reflect.Kind]any{reflect.String: "x", reflect.Int: 7, reflect.Bool: true}[sf.Type.Kind()]
		nv := reflect.ValueOf(one).Convert(sf.Type).Interface()
		f.set(&s, nv)
		if got := reflect.ValueOf(s).Field(i).Interface(); got != nv || f.get(&s) != nv || v == nv {
			t.Errorf("%s: get/set do not reach %s", f.name, sf.Name)
		}
	}

	// api.Settings (03 §12.4) has the same JSON shape.
	b, err := json.Marshal(defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var mirror api.Settings
	if err := dec.Decode(&mirror); err != nil {
		t.Fatalf("store.Settings JSON does not decode into api.Settings: %v", err)
	}
	if b2, _ := json.Marshal(mirror); !bytes.Equal(b, b2) {
		t.Errorf("api.Settings JSON = %s, want %s", b2, b)
	}
}

func TestDefaultRoomNameKey(t *testing.T) {
	t.Parallel()
	key, err := precis.Nickname.CompareKey(defaultRoomName)
	if err != nil || key != defaultRoomNameKey {
		t.Errorf("Nickname.CompareKey(%q) = %q, %v; the constant is %q", defaultRoomName, key, err, defaultRoomNameKey)
	}
}

// TestSettingsDefaults: the defaults of the 03 §9 table, a new DB serves them and stores nothing, and each default
// passes its own field's checks.
func TestSettingsDefaults(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	c := db.Settings()
	want := Settings{
		ServerName:             "",
		RegistrationMode:       ModeInvite,
		InviteDefaultTTLHours:  168,
		InviteDefaultMaxUses:   10,
		MembersCanInvite:       false,
		MaxParticipantsPerRoom: 0,
		MaxSharesPerRoom:       0,
		MaxShareBitrateKbps:    0,
		TransferAlertGB:        0,
		UpdateCheck:            true,
		MinClientVersion:       "",
		SetupWizardDone:        false,
	}
	if got := c.Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
	if got := c.Get(); got != want {
		t.Errorf("Get() = %+v, want the defaults", got)
	}
	if got := c.Locked(); got != nil {
		t.Errorf("Locked() = %v, want nil", got)
	}
	if rows := settingsRows(t, db); len(rows) != 0 {
		t.Errorf("a new DB stores settings: %v", rows)
	}
	d := c.Defaults()
	for _, f := range settingFields {
		raw, _ := json.Marshal(f.get(&d))
		if v, code := f.decode(raw); code != "" || v != f.get(&d) {
			t.Errorf("default of %s (%s) fails its checks: %v %q", f.name, raw, v, code)
		}
	}
	// Defaults is a copy.
	d.RegistrationMode = ModeClosed
	if c.Defaults().RegistrationMode != ModeInvite {
		t.Error("Defaults() shares memory")
	}
}

// TestSettingsValidation: every field's type and range (03 §9), through Update and through Pin, which run the same
// checks.
func TestSettingsValidation(t *testing.T) {
	t.Parallel()
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	cases := []struct {
		field string
		raw   string
		want  any    // the stored value when code is ""
		code  string // api.Field*
	}{
		{"serverName", `"Alex's server"`, "Alex's server", ""},
		{"serverName", `"  Alex's   server  "`, "Alex's server", ""},
		{"serverName", "\"\u3000Ａｌｅｘ\u00a0place\"", "Alex place", ""}, // non-ASCII spaces and NFKC
		{"serverName", `"🎬 Movie night"`, "🎬 Movie night", ""},
		{"serverName", `""`, "", ""},
		{"serverName", "\"  \u00a0 \"", "", ""},
		{"serverName", q(strings.Repeat("a", 64)), strings.Repeat("a", 64), ""},
		{"serverName", q(strings.Repeat("太", 64)), strings.Repeat("太", 64), ""},
		{"serverName", q(strings.Repeat("a", 32) + "      " + strings.Repeat("b", 31)), strings.Repeat("a", 32) + " " +
			strings.Repeat("b", 31), ""}, // 64 runes once the spaces collapse
		{"serverName", q(strings.Repeat("a", 65)), nil, api.FieldTooLong},
		{"serverName", q(strings.Repeat("太", 65)), nil, api.FieldTooLong},
		{"serverName", `"bell\u0007"`, nil, api.FieldInvalid},
		{"serverName", `"tab\there"`, nil, api.FieldInvalid},
		{"serverName", "\"a\u200bb\"", nil, api.FieldInvalid}, // zero width space
		{"serverName", `42`, nil, api.FieldInvalid},
		{"serverName", `null`, nil, api.FieldInvalid},
		// encoding/json would turn invalid UTF-8 and lone surrogates into U+FFFD, which PRECIS Nickname allows.
		{"serverName", "\"ok\xffname\"", nil, api.FieldInvalid},
		{"serverName", "\"ok\xff\xfename\"", nil, api.FieldInvalid},
		{"serverName", `"a\udc00b"`, nil, api.FieldInvalid},
		{"serverName", `"a\ud83db"`, nil, api.FieldInvalid},
		{"serverName", `"a�b"`, nil, api.FieldInvalid},
		{"serverName", "\"a�b\"", nil, api.FieldInvalid},
		{"serverName", `"🎬 ok"`, "🎬 ok", ""}, // a valid surrogate pair

		{"registrationMode", `"approval"`, ModeApproval, ""},
		{"registrationMode", `"closed"`, ModeClosed, ""},
		{"registrationMode", `"invite"`, ModeInvite, ""},
		{"registrationMode", `"open"`, nil, api.FieldInvalid},
		{"registrationMode", `"Invite"`, nil, api.FieldInvalid},
		{"registrationMode", `""`, nil, api.FieldInvalid},
		{"registrationMode", `1`, nil, api.FieldInvalid},

		{"inviteDefaultTtlHours", `1`, 1, ""},
		{"inviteDefaultTtlHours", ` 720 `, 720, ""},
		{"inviteDefaultTtlHours", `0`, nil, api.FieldOutOfRange},
		{"inviteDefaultTtlHours", `721`, nil, api.FieldOutOfRange},
		{"inviteDefaultTtlHours", `-1`, nil, api.FieldOutOfRange},
		{"inviteDefaultTtlHours", `99999999999999999999999`, nil, api.FieldOutOfRange},
		{"inviteDefaultTtlHours", `"24"`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `24.5`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `24.0`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `2e1`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `true`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `[24]`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `{"h":24}`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `24 25`, nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", `null`, nil, api.FieldInvalid},

		{"inviteDefaultMaxUses", `1`, 1, ""},
		{"inviteDefaultMaxUses", `1000`, 1000, ""},
		{"inviteDefaultMaxUses", `0`, nil, api.FieldOutOfRange},
		{"inviteDefaultMaxUses", `1001`, nil, api.FieldOutOfRange},

		{"membersCanInvite", `true`, true, ""},
		{"membersCanInvite", `false`, false, ""},
		{"membersCanInvite", `"true"`, nil, api.FieldInvalid},
		{"membersCanInvite", `1`, nil, api.FieldInvalid},
		{"membersCanInvite", `null`, nil, api.FieldInvalid},

		{"maxParticipantsPerRoom", `0`, 0, ""},
		{"maxParticipantsPerRoom", `10000`, 10000, ""},
		{"maxParticipantsPerRoom", `10001`, nil, api.FieldOutOfRange},
		{"maxParticipantsPerRoom", `-1`, nil, api.FieldOutOfRange},

		{"maxSharesPerRoom", `0`, 0, ""},
		{"maxSharesPerRoom", `1000`, 1000, ""},
		{"maxSharesPerRoom", `1001`, nil, api.FieldOutOfRange},

		{"maxShareBitrateKbps", `0`, 0, ""},
		{"maxShareBitrateKbps", `500`, 500, ""},
		{"maxShareBitrateKbps", `100000`, 100000, ""},
		{"maxShareBitrateKbps", `1`, nil, api.FieldOutOfRange},
		{"maxShareBitrateKbps", `499`, nil, api.FieldOutOfRange},
		{"maxShareBitrateKbps", `100001`, nil, api.FieldOutOfRange},
		{"maxShareBitrateKbps", `-500`, nil, api.FieldOutOfRange},

		{"transferAlertGb", `0`, 0, ""},
		{"transferAlertGb", `1000000`, 1000000, ""},
		{"transferAlertGb", `1000001`, nil, api.FieldOutOfRange},

		{"updateCheck", `false`, false, ""},
		{"updateCheck", `"no"`, nil, api.FieldInvalid},

		{"minClientVersion", `"0.3.0"`, "0.3.0", ""},
		{"minClientVersion", `"1.2.3-rc.1+build.5"`, "1.2.3-rc.1+build.5", ""},
		{"minClientVersion", `""`, "", ""},
		{"minClientVersion", `"v0.3.0"`, nil, api.FieldInvalid},
		{"minClientVersion", `"0.3"`, nil, api.FieldInvalid},
		{"minClientVersion", `"01.2.3"`, nil, api.FieldInvalid},
		{"minClientVersion", `"1.2.3-01"`, nil, api.FieldInvalid},
		{"minClientVersion", `3`, nil, api.FieldInvalid},
		{"minClientVersion", "\"1.2.3-a\xff\"", nil, api.FieldInvalid},
		{"minClientVersion", `"1.2.3-a\udc00"`, nil, api.FieldInvalid},

		{"setupWizardDone", `true`, true, ""},
		{"setupWizardDone", `0`, nil, api.FieldInvalid},
	}
	ctx := context.Background()
	updDB := newEnv(t).open(nil)
	pinDB := newEnv(t).open(nil)
	for _, tc := range cases {
		name := tc.field + "=" + tc.raw
		f := settingFields[settingByName[tc.field]]
		before := updDB.Settings().Get()
		got, err := updDB.Settings().Update(ctx, patch(t, tc.field, json.RawMessage(tc.raw)), adminActor)
		pinErr := pinDB.Settings().Pin(tc.field, json.RawMessage(tc.raw))
		if tc.code == "" {
			if err != nil || pinErr != nil {
				t.Errorf("%s: Update = %v, Pin = %v", name, err, pinErr)
				continue
			}
			if v := f.get(&got); v != tc.want {
				t.Errorf("%s: Update stored %#v, want %#v", name, v, tc.want)
			}
			if pinned := pinDB.Settings().Get(); f.get(&pinned) != tc.want {
				t.Errorf("%s: Pin set %#v, want %#v", name, f.get(&pinned), tc.want)
			}
			continue
		}
		for what, e := range map[string]error{"Update": err, "Pin": pinErr} {
			var ae *api.Error
			if !errors.As(e, &ae) || ae.Code != api.CodeValidationFailed ||
				!maps.Equal(ae.Fields, map[string]string{tc.field: tc.code}) {
				t.Errorf("%s: %s = %#v, want validation_failed {%s: %s}", name, what, e, tc.field, tc.code)
			}
		}
		if after := updDB.Settings().Get(); after != before {
			t.Errorf("%s: a rejected Update changed the settings", name)
		}
	}

	// Several bad fields are reported together, and a patch with one bad field writes nothing.
	db := newEnv(t).open(nil)
	_, err := db.Settings().Update(ctx, patch(t, "maxSharesPerRoom", 4, "inviteDefaultMaxUses", 0,
		"registrationMode", "open", "minClientVersion", "latest"), adminActor)
	ae := apiError(t, err, api.CodeValidationFailed)
	wantFields := map[string]string{"inviteDefaultMaxUses": api.FieldOutOfRange, "registrationMode": api.FieldInvalid,
		"minClientVersion": api.FieldInvalid}
	if !maps.Equal(ae.Fields, wantFields) || ae.Params != nil {
		t.Errorf("fields = %v, want %v", ae.Fields, wantFields)
	}
	if api.StatusOf(ae.Code) != 422 {
		t.Errorf("status %d", api.StatusOf(ae.Code))
	}
	if db.Settings().Get() != defaultSettings() || len(settingsRows(t, db)) != 0 || len(settingsAudit(t, db)) != 0 {
		t.Error("a rejected patch wrote something")
	}
}

// TestSettingsUpdate: stored rows, the audit row in the same write, no-op patches, unknown names, and reloading.
func TestSettingsUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	c := db.Settings()
	ctx := context.Background()

	got, err := c.Update(ctx, patch(t, "registrationMode", "approval", "maxSharesPerRoom", 4), adminActor)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultSettings()
	want.RegistrationMode, want.MaxSharesPerRoom = ModeApproval, 4
	if got != want || c.Get() != want {
		t.Fatalf("Update = %+v, Get = %+v, want %+v", got, c.Get(), want)
	}
	rows := settingsRows(t, db)
	if !maps.Equal(rows, map[string]string{"registration.mode": `"approval"`, "limits.max_shares_per_room": "4"}) {
		t.Errorf("rows = %v", rows)
	}
	var by string
	var at int64
	mustRead(t, db, func(q *Q) error {
		return q.queryOne("test", `SELECT updated_by, updated_at FROM settings WHERE key = 'registration.mode'`, nil,
			&by, &at)
	})
	if by != string(adminActor.UserID) || at != e.clock.Now().UnixMilli() {
		t.Errorf("updated_by, updated_at = %q, %d", by, at)
	}
	audit := settingsAudit(t, db)
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit))
	}
	a := audit[0]
	if a.Action != "settings.changed" || a.Outcome != "ok" || a.Actor != adminActor || a.TargetKind != "settings" ||
		a.TargetID != "" || !a.At.Equal(e.clock.Now()) {
		t.Errorf("audit row = %+v", a)
	}
	wantDetail := `{"changes":{"maxSharesPerRoom":{"from":0,"to":4},"registrationMode":{"from":"invite","to":"approval"}}}`
	if got := detailJSON(t, a.Detail); got != wantDetail {
		t.Errorf("detail = %s, want %s", got, wantDetail)
	}

	// Only the fields that change are written and audited; the registrationMode change is a security event, the
	// next one isn't.
	e.clock.Advance(time.Second)
	if _, err := c.Update(ctx, patch(t, "registrationMode", "approval", "maxSharesPerRoom", 0, "updateCheck", false),
		CLIActor); err != nil {
		t.Fatal(err)
	}
	rows = settingsRows(t, db)
	if !maps.Equal(rows, map[string]string{"registration.mode": `"approval"`, "updates.release_check": "false"}) {
		t.Errorf("after reset to the default, rows = %v", rows) // the default deletes its row
	}
	audit = settingsAudit(t, db)
	if len(audit) != 2 || audit[0].Actor != CLIActor ||
		detailJSON(t, audit[0].Detail) != `{"changes":{"maxSharesPerRoom":{"from":4,"to":0},"updateCheck":{"from":true,"to":false}}}` {
		t.Errorf("second audit row = %+v", audit[0])
	}
	mustRead(t, db, func(q *Q) error {
		return q.queryOne("test", `SELECT updated_by FROM settings WHERE key = 'updates.release_check'`, nil, &by)
	})
	if by != "cli" {
		t.Errorf("updated_by = %q, want cli", by)
	}
	var sec []string
	mustRead(t, db, func(q *Q) error {
		evs, err := q.SecurityEvents(e.clock.Now().Add(-24*time.Hour), 0)
		for _, ev := range evs {
			sec = append(sec, detailJSON(t, ev.Detail))
		}
		return err
	})
	if len(sec) != 1 || !strings.Contains(sec[0], "registrationMode") {
		t.Errorf("security events = %v, want only the registrationMode change", sec)
	}

	// A patch that changes nothing, an empty patch and unknown names (JSON names are case-sensitive) write nothing.
	now := c.Get()
	for _, p := range []map[string]json.RawMessage{
		patch(t, "registrationMode", "approval", "updateCheck", false),
		nil,
		{},
		patch(t, "bogus", 1, "ServerName", "x", "server.name", "x"),
	} {
		got, err := c.Update(ctx, p, adminActor)
		if err != nil || got != now {
			t.Errorf("Update(%v) = %+v, %v", p, got, err)
		}
	}
	if n := len(settingsAudit(t, db)); n != 2 {
		t.Errorf("audit rows = %d after no-op patches, want 2", n)
	}

	// The next start loads the stored values.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 1), adminActor); !errors.Is(err, errClosed) {
		t.Errorf("Update after Close = %v", err)
	}
	db2 := e.open(nil)
	if got := db2.Settings().Get(); got != now {
		t.Errorf("after reopening Get = %+v, want %+v", got, now)
	}
}

// TestSettingsServerName: the normalized value is what is stored, audited and loaded.
func TestSettingsServerName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	got, err := db.Settings().Update(context.Background(), patch(t, "serverName", " Ａｌｅｘ's   server "), adminActor)
	if err != nil || got.ServerName != "Alex's server" {
		t.Fatalf("Update = %q, %v", got.ServerName, err)
	}
	if rows := settingsRows(t, db); rows["server.name"] != `"Alex's server"` {
		t.Errorf("rows = %v", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := e.open(nil).Settings().Get().ServerName; got != "Alex's server" {
		t.Errorf("loaded %q", got)
	}
}

// TestSettingsPin: Pin locks a field (setting_locked), validates like Update, never writes the DB, and the DB
// value applies again without the pin.
func TestSettingsPin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	c := db.Settings()
	ctx := context.Background()
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 4, "updateCheck", false), adminActor); err != nil {
		t.Fatal(err)
	}
	var rec changeRecorder
	defer c.OnChange(rec.fn)()

	if err := c.Pin("updateCheck", true); err != nil {
		t.Fatal(err)
	}
	if !c.Get().UpdateCheck || !slices.Equal(c.Locked(), []string{"updateCheck"}) {
		t.Errorf("after Pin: UpdateCheck = %v, Locked = %v", c.Get().UpdateCheck, c.Locked())
	}
	if calls := rec.take(); len(calls) != 1 || calls[0][0].UpdateCheck || !calls[0][1].UpdateCheck {
		t.Errorf("OnChange calls = %+v, want one false → true", calls)
	}
	// The values config hands over: typed strings, other integer types, raw JSON; strings are normalized.
	pins := []struct {
		field string
		value any
	}{
		{"registrationMode", ModeClosed},
		{"maxShareBitrateKbps", int64(2500)},
		{"transferAlertGb", uint16(500)},
		{"minClientVersion", json.RawMessage(`"0.3.0"`)},
		{"serverName", "  Our   place "},
		{"maxParticipantsPerRoom", 0}, // equal to the value in force: pinned, but no change
	}
	for _, p := range pins {
		if err := c.Pin(p.field, p.value); err != nil {
			t.Fatalf("Pin(%s, %v) = %v", p.field, p.value, err)
		}
	}
	want := defaultSettings()
	want.RegistrationMode, want.MaxShareBitrateKbps, want.TransferAlertGB = ModeClosed, 2500, 500
	want.MinClientVersion, want.ServerName, want.MaxSharesPerRoom, want.UpdateCheck = "0.3.0", "Our place", 4, true
	if got := c.Get(); got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	wantLocked := []string{"serverName", "registrationMode", "maxParticipantsPerRoom", "maxShareBitrateKbps",
		"transferAlertGb", "updateCheck", "minClientVersion"}
	if got := c.Locked(); !slices.Equal(got, wantLocked) {
		t.Errorf("Locked = %v, want %v (field order)", got, wantLocked)
	}
	if calls := rec.take(); len(calls) != 5 {
		t.Errorf("OnChange ran %d times for 6 pins (one without a change), want 5", len(calls))
	}
	// Locked returns a copy.
	c.Locked()[0] = "x"
	if c.Locked()[0] != "serverName" {
		t.Error("Locked shares memory")
	}
	// Pinning the same value again changes nothing.
	if err := c.Pin("updateCheck", true); err != nil || len(rec.take()) != 0 {
		t.Errorf("re-Pin: %v", err)
	}

	// Update refuses pinned fields with 409, before checking values, and writes nothing.
	rowsBefore, auditBefore := settingsRows(t, db), len(settingsAudit(t, db))
	for _, p := range []map[string]json.RawMessage{
		patch(t, "updateCheck", false),
		patch(t, "updateCheck", true), // even the pinned value itself
		patch(t, "maxSharesPerRoom", 5, "registrationMode", "approval"),
		patch(t, "maxSharesPerRoom", "bad", "serverName", "x"),
	} {
		_, err := c.Update(ctx, p, adminActor)
		ae := apiError(t, err, api.CodeSettingLocked)
		field, _ := ae.Params[api.ParamField].(string)
		if !slices.Contains(wantLocked, field) || ae.Fields != nil || api.StatusOf(ae.Code) != 409 {
			t.Errorf("Update(%v): %+v", p, ae)
		}
		if _, ok := p[field]; !ok {
			t.Errorf("setting_locked names %q, not in the patch", field)
		}
	}
	if !maps.Equal(settingsRows(t, db), rowsBefore) || len(settingsAudit(t, db)) != auditBefore ||
		len(rec.take()) != 0 || c.Get() != want {
		t.Error("a refused patch changed something")
	}
	// Other fields still change.
	got, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 6), adminActor)
	want.MaxSharesPerRoom = 6
	if err != nil || got != want || len(rec.take()) != 1 {
		t.Errorf("Update of an unpinned field = %+v, %v", got, err)
	}

	// A pin that fails validation reports the field code and changes nothing.
	for _, p := range []struct {
		field string
		value any
		code  string
	}{
		{"maxShareBitrateKbps", 100, api.FieldOutOfRange},
		{"maxSharesPerRoom", -1, api.FieldOutOfRange},
		{"registrationMode", "open", api.FieldInvalid},
		{"updateCheck", "yes", api.FieldInvalid},
		{"updateCheck", nil, api.FieldInvalid},
		{"inviteDefaultTtlHours", 1.5, api.FieldInvalid},
		{"minClientVersion", "v1", api.FieldInvalid},
		{"serverName", strings.Repeat("x", 65), api.FieldTooLong},
		{"setupWizardDone", make(chan int), api.FieldInvalid}, // does not even marshal
	} {
		err := c.Pin(p.field, p.value)
		ae := apiError(t, err, api.CodeValidationFailed)
		if !maps.Equal(ae.Fields, map[string]string{p.field: p.code}) {
			t.Errorf("Pin(%s, %v) fields = %v, want %s", p.field, p.value, ae.Fields, p.code)
		}
	}
	if err := c.Pin("noSuchSetting", 1); err == nil || api.IsCode(err, api.CodeValidationFailed) ||
		!strings.Contains(err.Error(), "noSuchSetting") {
		t.Errorf("Pin(unknown) = %v, want a plain error naming it", err)
	}
	if c.Get() != want || !slices.Equal(c.Locked(), wantLocked) || len(rec.take()) != 0 {
		t.Error("a failed Pin changed the settings")
	}

	// Pinning never writes the DB: without the pins, the next start serves the DB values.
	rows := settingsRows(t, db)
	if !maps.Equal(rows, map[string]string{"limits.max_shares_per_room": "6", "updates.release_check": "false"}) {
		t.Errorf("rows = %v", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c2 := e.open(nil).Settings()
	want2 := defaultSettings()
	want2.MaxSharesPerRoom, want2.UpdateCheck = 6, false
	if got := c2.Get(); got != want2 || c2.Locked() != nil {
		t.Errorf("unpinned restart: Get = %+v, Locked = %v", got, c2.Locked())
	}
}

// TestSettingsOnChange: every callback runs once per change with the old and new settings, in registration order,
// and never for a write that changes nothing or fails; cancel removes one.
func TestSettingsOnChange(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	c := db.Settings()
	ctx := context.Background()
	var (
		mu    sync.Mutex
		order []string
		pairs [][2]Settings
	)
	record := func(name string) func(prev, next Settings) {
		return func(prev, next Settings) {
			// Readers work inside a callback and already see the new settings.
			_ = c.Locked()
			if c.Get() != next {
				t.Errorf("%s: Get inside the callback = %+v, want %+v", name, c.Get(), next)
			}
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			pairs = append(pairs, [2]Settings{prev, next})
		}
	}
	take := func() ([]string, [][2]Settings) {
		mu.Lock()
		defer mu.Unlock()
		o, p := order, pairs
		order, pairs = nil, nil
		return o, p
	}
	cancelA := c.OnChange(record("a"))
	cancelB := c.OnChange(record("b"))
	defer cancelB()
	c.OnChange(nil)() // a nil callback is ignored

	old := c.Get()
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 3, "membersCanInvite", true), adminActor); err != nil {
		t.Fatal(err)
	}
	want := old
	want.MaxSharesPerRoom, want.MembersCanInvite = 3, true
	o, p := take()
	if !slices.Equal(o, []string{"a", "b"}) || p[0] != [2]Settings{old, want} || p[1] != p[0] {
		t.Fatalf("calls %v %+v, want a then b with (old, new)", o, p)
	}

	// No call for a no-op, a rejected or a locked patch.
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 3), adminActor); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 3000), adminActor); err == nil {
		t.Fatal("out of range accepted")
	}
	if err := c.Pin("setupWizardDone", false); err != nil { // the value in force: no change
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, patch(t, "setupWizardDone", true), adminActor); err == nil {
		t.Fatal("locked field accepted")
	}
	if o, _ := take(); len(o) != 0 {
		t.Fatalf("calls without a change: %v", o)
	}

	// After cancel only b runs; cancel twice is fine.
	cancelA()
	cancelA()
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 5), adminActor); err != nil {
		t.Fatal(err)
	}
	if o, p := take(); !slices.Equal(o, []string{"b"}) || p[0][0].MaxSharesPerRoom != 3 || p[0][1].MaxSharesPerRoom != 5 {
		t.Errorf("after cancel: %v %+v", o, p)
	}
}

// TestSettingsWriteIsAtomic: the settings rows and the audit row commit together; a failure injected after the
// audit insert leaves no settings change, no audit row, the cache as it was and no OnChange call.
func TestSettingsWriteIsAtomic(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	c := db.Settings()
	var rec changeRecorder
	defer c.OnChange(rec.fn)()
	injected := errors.New("injected")
	c.failAfterAudit = func(q *Q) error {
		// The audit row and the settings row are in the transaction at this point.
		rows, err := q.ListAudit(AuditQuery{ActionPrefix: "settings."})
		if err != nil || len(rows) != 1 {
			t.Errorf("audit rows inside the write = %d, %v", len(rows), err)
		}
		var n int
		if err := q.queryOne("test", `SELECT count(*) FROM settings`, nil, &n); err != nil || n != 2 {
			t.Errorf("settings rows inside the write = %d, %v", n, err)
		}
		return injected
	}
	p := patch(t, "registrationMode", "closed", "inviteDefaultMaxUses", 3)
	if _, err := c.Update(context.Background(), p, adminActor); !errors.Is(err, injected) {
		t.Fatalf("Update = %v, want the injected error", err)
	}
	if len(settingsRows(t, db)) != 0 || len(settingsAudit(t, db)) != 0 || c.Get() != defaultSettings() ||
		len(rec.take()) != 0 {
		t.Fatal("the failed write left a change behind")
	}
	c.failAfterAudit = nil
	if _, err := c.Update(context.Background(), p, adminActor); err != nil {
		t.Fatal(err)
	}
	if len(settingsRows(t, db)) != 2 || len(settingsAudit(t, db)) != 1 || len(rec.take()) != 1 {
		t.Error("the retried write is incomplete")
	}
}

// TestSettingsUpdateTx: the in-transaction variant for setup/complete (03 §7.8).
func TestSettingsUpdateTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	c := db.Settings()
	ctx := context.Background()
	var rec changeRecorder
	defer c.OnChange(rec.fn)()

	// Committed together with another row; the cache changes on apply.
	var apply func() Settings
	mustWrite(t, db, func(q *Q) error {
		u := User{Username: "Alex", UsernameKey: "alex", PasswordHash: testPHC, Role: RoleAdmin, CreatedVia: "setup"}
		if err := q.CreateUser(&u); err != nil {
			return err
		}
		var err error
		apply, err = c.UpdateTx(q, patch(t, "serverName", "Alex's server"), Actor{Kind: ActorUser, UserID: u.ID, Name: "Alex"})
		if c.Get().ServerName != "" {
			t.Error("the cache changed before the commit")
		}
		return err
	})
	if got := apply(); got.ServerName != "Alex's server" || c.Get() != got || len(rec.take()) != 1 {
		t.Errorf("apply = %+v", got)
	}
	if len(settingsAudit(t, db)) != 1 {
		t.Error("no settings.changed row")
	}

	// A rolled-back Write leaves nothing (its caller never calls apply).
	boom := errors.New("boom")
	err := db.Write(ctx, func(q *Q) error {
		if _, err := c.UpdateTx(q, patch(t, "serverName", "Other"), adminActor); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || settingsRows(t, db)["server.name"] != `"Alex's server"` || len(settingsAudit(t, db)) != 1 {
		t.Errorf("rolled back: %v, rows %v", err, settingsRows(t, db))
	}

	// Validation and locked errors come back inside the Write; a Read refuses the write.
	if err := c.Pin("setupWizardDone", false); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, db, func(q *Q) error {
		_, err := c.UpdateTx(q, patch(t, "maxSharesPerRoom", -1), adminActor)
		_ = apiError(t, err, api.CodeValidationFailed)
		_, err = c.UpdateTx(q, patch(t, "setupWizardDone", true), adminActor)
		_ = apiError(t, err, api.CodeSettingLocked)
		return nil
	})
	mustRead(t, db, func(q *Q) error {
		if _, err := c.UpdateTx(q, patch(t, "maxSharesPerRoom", 1), adminActor); !errors.Is(err, errReadOnlyTx) {
			t.Errorf("UpdateTx in Read = %v", err)
		}
		return nil
	})
	// An empty patch writes nothing and apply returns the settings in force.
	mustWrite(t, db, func(q *Q) error {
		apply, err = c.UpdateTx(q, nil, adminActor)
		return err
	})
	if apply() != c.Get() || len(settingsAudit(t, db)) != 1 {
		t.Error("empty UpdateTx wrote something")
	}

	// A skipped apply leaves the cache behind until the next settings write, which catches up.
	mustWrite(t, db, func(q *Q) error {
		_, err := c.UpdateTx(q, patch(t, "maxSharesPerRoom", 7), adminActor)
		return err
	})
	if c.Get().MaxSharesPerRoom != 0 {
		t.Error("the cache changed without apply")
	}
	got, err := c.Update(ctx, patch(t, "membersCanInvite", true), adminActor)
	if err != nil || got.MaxSharesPerRoom != 7 || !got.MembersCanInvite || c.Get() != got {
		t.Errorf("catch-up: %+v, %v", got, err)
	}
	// The late apply of an older write never rolls the cache back.
	mustWrite(t, db, func(q *Q) error {
		apply, err = c.UpdateTx(q, patch(t, "maxSharesPerRoom", 8), adminActor)
		return err
	})
	if _, err := c.Update(ctx, patch(t, "maxSharesPerRoom", 9), adminActor); err != nil {
		t.Fatal(err)
	}
	if got := apply(); got.MaxSharesPerRoom != 9 || c.Get().MaxSharesPerRoom != 9 {
		t.Errorf("late apply rolled back to %d", c.Get().MaxSharesPerRoom)
	}
}

// TestSettingsLoadSkipsBadRows: Open keeps the default for a row with an unknown key or an invalid value, logs a
// WARN naming the key, and leaves the row alone until an Update of that field replaces or removes it.
func TestSettingsLoadSkipsBadRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	for _, kv := range [][2]string{
		{"registration.mode", `"approval"`},
		{"server.name", `"Our place"`},
		{"limits.max_shares_per_room", `99999`},
		{"invites.default_ttl_hours", `not json`},
		{"future.setting", `1`},
	} {
		mustExecW(t, db, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, 0, 'cli')`, kv[0], kv[1])
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	o := e.opts(nil)
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	db, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	want := defaultSettings()
	want.RegistrationMode, want.ServerName = ModeApproval, "Our place"
	if got := db.Settings().Get(); got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	out := logs.String()
	for _, key := range []string{"limits.max_shares_per_room", "invites.default_ttl_hours", "future.setting"} {
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "key="+key) {
			t.Errorf("no WARN for %s in:\n%s", key, out)
		}
	}
	if strings.Contains(out, "Our place") {
		t.Error("a stored value reached the log")
	}
	if n := len(settingsRows(t, db)); n != 5 {
		t.Errorf("rows = %d, want the 5 left alone", n)
	}
	// Sending the default for a field with a bad row removes the row (it would otherwise stay, and be logged at every
	// Open), without an audit row or an OnChange call: the effective value stays the default.
	var rec changeRecorder
	db.Settings().OnChange(rec.fn)
	got, err := db.Settings().Update(context.Background(), patch(t, "maxSharesPerRoom", 0), adminActor)
	if err != nil || got != want {
		t.Fatalf("Update(maxSharesPerRoom 0) = %+v, %v", got, err)
	}
	rows := settingsRows(t, db)
	if _, ok := rows["limits.max_shares_per_room"]; ok || len(rows) != 4 {
		t.Errorf("rows = %v, want the bad limits.max_shares_per_room row gone", rows)
	}
	if n := len(settingsAudit(t, db)); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
	if calls := rec.take(); len(calls) != 0 {
		t.Errorf("OnChange ran %d times, want 0", len(calls))
	}

	// It works for several fields in one patch, whatever made the row bad (a wrong type, malformed JSON)...
	mustExecW(t, db, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, 0, 'cli')`,
		"invites.default_max_uses", `"ten"`)
	if _, err := db.Settings().Update(context.Background(), patch(t, "inviteDefaultMaxUses", 10,
		"inviteDefaultTtlHours", 168), adminActor); err != nil {
		t.Fatal(err)
	}
	rows = settingsRows(t, db)
	if _, ok := rows["invites.default_max_uses"]; ok {
		t.Errorf("rows = %v, want invites.default_max_uses gone", rows)
	}
	if _, ok := rows["invites.default_ttl_hours"]; ok {
		t.Errorf("rows = %v, want invites.default_ttl_hours gone", rows)
	}
	// ...and a field whose valid row already holds the value, or that has no row, is left alone.
	if _, err := db.Settings().Update(context.Background(), patch(t, "serverName", "Our place", "updateCheck", true),
		adminActor); err != nil {
		t.Fatal(err)
	}
	if rows = settingsRows(t, db); !maps.Equal(rows, map[string]string{"registration.mode": `"approval"`,
		"server.name": `"Our place"`, "future.setting": "1"}) {
		t.Errorf("rows = %v", rows)
	}
	var at int64
	mustRead(t, db, func(q *Q) error {
		return q.queryOne("test", `SELECT updated_at FROM settings WHERE key = 'server.name'`, nil, &at)
	})
	if at != 0 {
		t.Errorf("server.name updated_at = %d, want the row untouched", at)
	}
	if n := len(settingsAudit(t, db)); n != 0 || len(rec.take()) != 0 {
		t.Errorf("repairs wrote %d audit rows or ran OnChange", n)
	}

	// The next Open logs a WARN only for the unknown key left.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs2 lockedBuffer
	o = e.opts(nil)
	o.Logger = slog.New(slog.NewTextHandler(&logs2, nil))
	db, err = Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	out = logs2.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "key=future.setting") {
		t.Errorf("after the repairs, want one WARN for future.setting, got:\n%s", out)
	}
	if got := db.Settings().Get(); got != want {
		t.Errorf("after reopening Get = %+v, want %+v", got, want)
	}

	// An update to another value replaces a bad row; its old value counts as the default.
	mustExecW(t, db, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, 0, 'cli')`,
		"limits.max_shares_per_room", `99999`)
	if _, err := db.Settings().Update(context.Background(), patch(t, "maxSharesPerRoom", 5), adminActor); err != nil {
		t.Fatal(err)
	}
	if got := detailJSON(t, settingsAudit(t, db)[0].Detail); got != `{"changes":{"maxSharesPerRoom":{"from":0,"to":5}}}` {
		t.Errorf("detail = %s", got)
	}
	if got := settingsRows(t, db)["limits.max_shares_per_room"]; got != "5" {
		t.Errorf("limits.max_shares_per_room = %s, want 5", got)
	}
}

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSettingsAuditDetailLimit: a settings.changed row that would pass 1 KiB shortens long text values and still
// names every changed field, even in the worst case of HTML-escaped text.
func TestSettingsAuditDetailLimit(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	long := strings.Repeat("🎬", 64)
	version := "1.2.3-" + strings.Repeat("a", 300)
	p := patch(t, "serverName", long, "registrationMode", "approval", "inviteDefaultTtlHours", 719,
		"inviteDefaultMaxUses", 999, "membersCanInvite", true, "maxParticipantsPerRoom", 9999, "maxSharesPerRoom", 999,
		"maxShareBitrateKbps", 99999, "transferAlertGb", 999999, "updateCheck", false, "minClientVersion", version,
		"setupWizardDone", true)
	got, err := db.Settings().Update(context.Background(), p, adminActor)
	if err != nil || got.ServerName != long || got.MinClientVersion != version {
		t.Fatalf("Update = %+v, %v", got, err)
	}
	d := settingsAudit(t, db)[0].Detail
	if d["truncated"] != true {
		t.Errorf("detail not marked truncated: %v", d)
	}
	changes, _ := d["changes"].(map[string]any)
	if len(changes) != len(settingFields) {
		t.Fatalf("changes has %d fields, want %d", len(changes), len(settingFields))
	}
	sn, _ := changes["serverName"].(map[string]any)
	if sn["to"] != strings.Repeat("🎬", auditTextRunes)+"…" {
		t.Errorf("serverName.to = %v", sn["to"])
	}
	if b, _ := json.Marshal(d); len(b) > maxAuditDetail {
		t.Errorf("detail is %d bytes", len(b))
	}
	var sec int
	mustRead(t, db, func(q *Q) error {
		evs, err := q.SecurityEvents(longAgo, 0)
		sec = len(evs)
		return err
	})
	if sec != 1 {
		t.Errorf("security events = %d, want the registrationMode change", sec)
	}

	// The worst case: all 12 fields change, from and to at their longest. json.Marshal writes each '<' and '>' as a
	// 6-byte escape, so 32 runes of them are 192 bytes; the detail still fits and names every field.
	db = newEnv(t).open(nil)
	version = "1.2.3-" + strings.Repeat("a", 100)
	p = patch(t, "serverName", strings.Repeat("<", 64), "registrationMode", "approval", "inviteDefaultTtlHours", 720,
		"inviteDefaultMaxUses", 1000, "membersCanInvite", true, "maxParticipantsPerRoom", 10000,
		"maxSharesPerRoom", 1000, "maxShareBitrateKbps", 100000, "transferAlertGb", 1000000, "updateCheck", false,
		"minClientVersion", version, "setupWizardDone", true)
	if _, err := db.Settings().Update(context.Background(), p, adminActor); err != nil {
		t.Fatal(err)
	}
	p = patch(t, "serverName", strings.Repeat(">", 64), "registrationMode", "closed", "inviteDefaultTtlHours", 100,
		"inviteDefaultMaxUses", 100, "membersCanInvite", false, "maxParticipantsPerRoom", 1000,
		"maxSharesPerRoom", 100, "maxShareBitrateKbps", 10000, "transferAlertGb", 100000, "updateCheck", true,
		"minClientVersion", "1.2.3-"+strings.Repeat("b", 100), "setupWizardDone", false)
	if _, err := db.Settings().Update(context.Background(), p, adminActor); err != nil {
		t.Fatal(err)
	}
	var raw string
	mustRead(t, db, func(q *Q) error {
		return q.queryOne("test", `SELECT detail FROM audit_log WHERE action = 'settings.changed' ORDER BY id DESC
			LIMIT 1`, nil, &raw)
	})
	if len(raw) > maxAuditDetail {
		t.Errorf("stored detail is %d bytes, want ≤ %d", len(raw), maxAuditDetail)
	}
	d = settingsAudit(t, db)[0].Detail
	changes, _ = d["changes"].(map[string]any)
	if d["truncated"] != true || len(changes) != len(settingFields) {
		t.Fatalf("detail = %s, want every field and truncated", raw)
	}
	if rm, _ := changes["registrationMode"].(map[string]any); rm["from"] != "approval" || rm["to"] != "closed" {
		t.Errorf("registrationMode change = %v", rm)
	}
	mustRead(t, db, func(q *Q) error {
		evs, err := q.SecurityEvents(longAgo, 0)
		sec = len(evs)
		return err
	})
	if sec != 2 {
		t.Errorf("security events = %d, want both registrationMode changes", sec)
	}

	// settingsChangedDetail itself: with every field at its longest escaped text, the detail fits, and a short enough
	// change stays whole.
	all := map[string]settingChange{}
	for _, f := range settingFields {
		switch f.get(&Settings{}).(type) {
		case string:
			all[f.name] = settingChange{From: strings.Repeat("<", 200), To: strings.Repeat("&", 200)}
		case int:
			all[f.name] = settingChange{From: -1000000, To: 1000000}
		case RegistrationMode:
			all[f.name] = settingChange{From: ModeApproval, To: ModeApproval}
		case bool:
			all[f.name] = settingChange{From: false, To: false}
		default:
			t.Fatalf("%s: no worst case for %T", f.name, f.get(&Settings{}))
		}
	}
	if b, _ := json.Marshal(settingsChangedDetail(all)); len(b) > maxAuditDetail {
		t.Errorf("worst-case detail is %d bytes: %s", len(b), b)
	}
	small := map[string]settingChange{"serverName": {From: "a", To: "b"}}
	if got := settingsChangedDetail(small); got["truncated"] != nil {
		t.Errorf("a small detail was shortened: %v", got)
	}
}

// longAgo is a time before every test clock.
var longAgo = newTestClock().Now().AddDate(-1, 0, 0)

// TestSettingsConcurrentUpdates: concurrent writers (race detector), lock-free readers, and callbacks that see a
// consistent chain of changes ending in what the DB holds.
func TestSettingsConcurrentUpdates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	c := db.Settings()
	ctx := context.Background()
	var (
		mu    sync.Mutex
		chain [][2]Settings
	)
	defer c.OnChange(func(prev, next Settings) {
		mu.Lock()
		defer mu.Unlock()
		chain = append(chain, [2]Settings{prev, next})
	})()
	fields := []string{"maxParticipantsPerRoom", "maxSharesPerRoom", "transferAlertGb", "inviteDefaultMaxUses"}
	const perWriter = 15
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.Get()
				_ = c.Locked()
			}
		}
	})
	var writers sync.WaitGroup
	for g, name := range fields {
		writers.Go(func() {
			for k := 1; k <= perWriter; k++ {
				if _, err := c.Update(ctx, patch(t, name, g*100+k), adminActor); err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		})
	}
	writers.Wait()
	close(stop)
	wg.Wait()

	final := c.Get()
	for g, name := range fields {
		if v := settingFields[settingByName[name]].get(&final); v != g*100+perWriter {
			t.Errorf("%s = %v, want %d", name, v, g*100+perWriter)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chain) == 0 || len(chain) > len(fields)*perWriter {
		t.Fatalf("%d callbacks for %d writes", len(chain), len(fields)*perWriter)
	}
	if chain[0][0] != defaultSettings() || chain[len(chain)-1][1] != final {
		t.Error("the callback chain does not run from the defaults to the final settings")
	}
	for i := 1; i < len(chain); i++ {
		if chain[i][0] != chain[i-1][1] {
			t.Fatalf("callback %d starts from %+v, the previous one ended at %+v", i, chain[i][0], chain[i-1][1])
		}
	}
	var audits int
	mustRead(t, db, func(q *Q) error {
		return q.queryOne("test", `SELECT count(*) FROM audit_log WHERE action = 'settings.changed'`, nil, &audits)
	})
	if audits != len(fields)*perWriter {
		t.Errorf("audit rows = %d, want %d", audits, len(fields)*perWriter)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := e.open(nil).Settings().Get(); got != final {
		t.Errorf("the DB holds %+v, the cache had %+v", got, final)
	}
}

// FuzzSettingValue: no JSON input makes a field's check panic, rejections carry a known field code, and an
// accepted value passes again unchanged (normalization is stable).
func FuzzSettingValue(f *testing.F) {
	for i, s := range []string{`"Alex's server"`, `"approval"`, `168`, `10`, `true`, `0`, `4`, `2500`, `1000`, `false`,
		`"0.3.0"`, `true`, "\" Ａ\u3000b \"", `1e3`, `null`, `"1.2.3-rc.1"`, `99999999999999999999`, `"\u0007"`} {
		f.Add(uint8(i), []byte(s))
	}
	f.Add(uint8(0), []byte("\"ok\xffname\""))
	f.Add(uint8(0), []byte(`"a\udc00b"`))
	codes := []string{api.FieldInvalid, api.FieldOutOfRange, api.FieldTooLong}
	f.Fuzz(func(t *testing.T, which uint8, raw []byte) {
		fd := settingFields[int(which)%len(settingFields)]
		v, code := fd.decode(raw)
		if code != "" {
			if !slices.Contains(codes, code) || v != nil {
				t.Fatalf("%s(%q) = %v, %q", fd.name, raw, v, code)
			}
			return
		}
		if s, ok := v.(string); ok && (!utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError)) {
			t.Fatalf("%s(%q) = %q, which is not valid UTF-8 or has U+FFFD", fd.name, raw, s)
		}
		again, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		v2, code2 := fd.decode(again)
		if code2 != "" || v2 != v {
			t.Fatalf("%s(%q) = %#v, but %s gives %#v, %q", fd.name, raw, v, again, v2, code2)
		}
		var s Settings
		fd.set(&s, v) // the value has the field's type
		if fmt.Sprint(fd.get(&s)) != fmt.Sprint(v) {
			t.Fatal("set/get")
		}
	})
}
