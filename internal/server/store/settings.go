package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Settings are the runtime settings an admin changes in the UI (03 §9). Only non-default values are stored, as JSON
// under the dotted keys in the comments. The dotted names are also the TOML keys that pin 04's policy settings
// (04 §4.3, §4.6): registration.mode, clients.min_version, limits.max_participants_per_room,
// limits.max_shares_per_room, limits.max_bitrate_kbps, limits.transfer_alert_gb and updates.release_check.
// Server-wide Web Push on/off is 04's config key push.enabled; per-user notification choices are push preferences,
// not settings.
type Settings struct {
	ServerName             string           `json:"serverName"`             // server.name ("" → host of Primary origin)
	RegistrationMode       RegistrationMode `json:"registrationMode"`       // registration.mode
	InviteDefaultTTLHours  int              `json:"inviteDefaultTtlHours"`  // invites.default_ttl_hours
	InviteDefaultMaxUses   int              `json:"inviteDefaultMaxUses"`   // invites.default_max_uses
	MembersCanInvite       bool             `json:"membersCanInvite"`       // invites.members_can_create
	MaxParticipantsPerRoom int              `json:"maxParticipantsPerRoom"` // limits.max_participants_per_room
	MaxSharesPerRoom       int              `json:"maxSharesPerRoom"`       // limits.max_shares_per_room
	MaxShareBitrateKbps    int              `json:"maxShareBitrateKbps"`    // limits.max_bitrate_kbps
	TransferAlertGB        int              `json:"transferAlertGb"`        // limits.transfer_alert_gb
	UpdateCheck            bool             `json:"updateCheck"`            // updates.release_check
	MinClientVersion       string           `json:"minClientVersion"`       // clients.min_version
	SetupWizardDone        bool             `json:"setupWizardDone"`        // setup.wizard_done
}

// defaultSettings is the single source of the policy defaults (03 §9 table). Every limit defaults to 0 (off).
func defaultSettings() Settings {
	return Settings{
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
}

// Value limits of 03 §9 that are not ranges in the registry.
const (
	serverNameMaxRunes = 64
	// auditTextRunes is how long a text value stays in a settings.changed row that would pass the 1 KiB detail limit
	// (the first of auditTextCuts).
	auditTextRunes = 32
)

// semverRE matches a SemVer 2.0.0 version without a leading v, like 04's check of clients.min_version.
var semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(-((0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$`)

// settingField is one field of Settings: its JSON name (the name in REST, Locked and setting_locked), its dotted DB
// key, how a JSON value is checked, and how the field is read and written. Values are the field's Go type (string,
// int, bool or RegistrationMode), so they compare with ==.
type settingField struct {
	name   string
	key    string
	decode func(raw []byte) (v any, code string) // the typed, normalized value, or an api.Field* code
	get    func(*Settings) any
	set    func(*Settings, any)
}

// field builds a registry entry for a Settings field of type T.
func field[T comparable](name, key string, decode func([]byte) (T, string), ptr func(*Settings) *T) settingField {
	return settingField{
		name: name,
		key:  key,
		decode: func(raw []byte) (any, string) {
			v, code := decode(raw)
			if code != "" {
				return nil, code
			}
			return v, ""
		},
		get: func(s *Settings) any { return *ptr(s) },
		set: func(s *Settings, v any) { *ptr(s) = v.(T) },
	}
}

// settingFields is the registry: every Settings field in struct order with its checks (03 §9 table). A test checks
// it against the struct.
var settingFields = []settingField{
	field("serverName", "server.name", decodeServerName,
		func(s *Settings) *string { return &s.ServerName }),
	field("registrationMode", "registration.mode", decodeRegistrationMode,
		func(s *Settings) *RegistrationMode { return &s.RegistrationMode }),
	field("inviteDefaultTtlHours", "invites.default_ttl_hours", intRange(1, 720, false),
		func(s *Settings) *int { return &s.InviteDefaultTTLHours }),
	field("inviteDefaultMaxUses", "invites.default_max_uses", intRange(1, 1000, false),
		func(s *Settings) *int { return &s.InviteDefaultMaxUses }),
	field("membersCanInvite", "invites.members_can_create", decodeBool,
		func(s *Settings) *bool { return &s.MembersCanInvite }),
	field("maxParticipantsPerRoom", "limits.max_participants_per_room", intRange(0, 10000, false),
		func(s *Settings) *int { return &s.MaxParticipantsPerRoom }),
	field("maxSharesPerRoom", "limits.max_shares_per_room", intRange(0, 1000, false),
		func(s *Settings) *int { return &s.MaxSharesPerRoom }),
	field("maxShareBitrateKbps", "limits.max_bitrate_kbps", intRange(500, 100000, true),
		func(s *Settings) *int { return &s.MaxShareBitrateKbps }),
	field("transferAlertGb", "limits.transfer_alert_gb", intRange(0, 1000000, false),
		func(s *Settings) *int { return &s.TransferAlertGB }),
	field("updateCheck", "updates.release_check", decodeBool,
		func(s *Settings) *bool { return &s.UpdateCheck }),
	field("minClientVersion", "clients.min_version", decodeMinClientVersion,
		func(s *Settings) *string { return &s.MinClientVersion }),
	field("setupWizardDone", "setup.wizard_done", decodeBool,
		func(s *Settings) *bool { return &s.SetupWizardDone }),
}

// settingByName and settingByKey index settingFields by JSON name and by DB key.
var settingByName, settingByKey = func() (byName, byKey map[string]int) {
	byName, byKey = map[string]int{}, map[string]int{}
	for i, f := range settingFields {
		byName[f.name], byKey[f.key] = i, i
	}
	return byName, byKey
}()

// jsonValue parses raw as exactly one JSON value, keeping numbers as json.Number. It fails for malformed JSON,
// trailing data and null.
func jsonValue(raw []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || v == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

func decodeString(raw []byte) (string, bool) {
	v, _ := jsonValue(raw)
	s, ok := v.(string)
	return s, ok
}

func decodeBool(raw []byte) (bool, string) {
	v, _ := jsonValue(raw)
	b, ok := v.(bool)
	if !ok {
		return false, api.FieldInvalid
	}
	return b, ""
}

// intRange accepts a JSON integer in lo..hi, or 0 when zeroOK. A fraction, an exponent or another type is invalid;
// an integer outside the range (however large) is out_of_range.
func intRange(lo, hi int, zeroOK bool) func([]byte) (int, string) {
	return func(raw []byte) (int, string) {
		v, _ := jsonValue(raw)
		n, ok := v.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			return 0, api.FieldInvalid
		}
		// Atoi parses straight to int, so there is no narrowing conversion: a value too large for int
		// fails here, like any other value outside lo..hi.
		i, err := strconv.Atoi(string(n))
		switch {
		case err != nil: // a valid JSON integer fails only by overflowing
			return 0, api.FieldOutOfRange
		case i == 0 && zeroOK:
			return 0, ""
		case i < lo || i > hi:
			return 0, api.FieldOutOfRange
		}
		return i, ""
	}
}

func decodeRegistrationMode(raw []byte) (RegistrationMode, string) {
	s, ok := decodeString(raw)
	switch m := RegistrationMode(s); {
	case ok && (m == ModeInvite || m == ModeApproval || m == ModeClosed):
		return m, ""
	default:
		return "", api.FieldInvalid
	}
}

func decodeMinClientVersion(raw []byte) (string, string) {
	s, ok := decodeString(raw)
	if !ok || (s != "" && !semverRE.MatchString(s)) {
		return "", api.FieldInvalid
	}
	return s, ""
}

// decodeServerName checks a serverName value. encoding/json silently turns invalid UTF-8 and lone surrogate escapes
// (\udc00) into U+FFFD, which PRECIS Nickname allows (it is a symbol), so both are rejected here first: PRECIS works
// only on valid UTF-8 (RFC 8264), and a server name has no use for U+FFFD.
func decodeServerName(raw []byte) (string, string) {
	if !utf8.Valid(raw) {
		return "", api.FieldInvalid
	}
	s, ok := decodeString(raw)
	if !ok || strings.ContainsRune(s, utf8.RuneError) {
		return "", api.FieldInvalid
	}
	return normalizeServerName(s)
}

// normalizeServerName applies the PRECIS Nickname profile (RFC 8266: non-ASCII spaces become ASCII, leading and
// trailing spaces go, inner runs collapse to one, NFKC), like room names (03 §8). Only spaces, or nothing, is "" (the
// default: the host of the primary origin). Disallowed runes (controls, for example) are invalid, and more than 64
// runes after normalization is too_long. s is valid UTF-8 without U+FFFD (decodeServerName).
func normalizeServerName(s string) (string, string) {
	if strings.TrimFunc(s, func(r rune) bool { return unicode.Is(unicode.Zs, r) }) == "" {
		return "", ""
	}
	out, err := precis.Nickname.String(s)
	if err != nil {
		return "", api.FieldInvalid
	}
	if again, err := precis.Nickname.String(out); err != nil || again != out {
		return "", api.FieldInvalid
	}
	if utf8.RuneCountInString(out) > serverNameMaxRunes {
		return "", api.FieldTooLong
	}
	return out, ""
}

// SettingsCache holds the current settings for lock-free reads (03 §9). DB.Settings returns it; Open loads it from
// the settings table.
//
// The effective settings are the defaults, overlaid with the stored (non-default) values, overlaid with the values
// that config pins. A pinned field keeps its DB value, which applies again once config stops pinning it (04 §4.6).
type SettingsCache struct {
	db  *DB
	cur atomic.Pointer[settingsView]

	// mu serializes the swaps of cur (after a committed write and in Pin) and the OnChange calls they make, so
	// callbacks see the changes in order.
	mu      sync.Mutex
	stored  Settings    // defaults overlaid with the DB rows, as of the write numbered applied
	pinned  map[int]any // settingFields index → value pinned by config
	applied uint64
	// seq numbers the settings writes. It is taken inside the write transaction, and the one writer runs those one
	// after another, so the numbers follow the commit order: apply skips a snapshot older than one it already has.
	seq atomic.Uint64

	cbMu   sync.Mutex
	cbs    []settingsCallback
	nextCB uint64

	// failAfterAudit is a test hook: an error it returns fails a settings write right after the audit row.
	failAfterAudit func(q *Q) error
}

// settingsView is one immutable snapshot for the lock-free readers.
type settingsView struct {
	settings Settings
	locked   []string // JSON names of the pinned fields, in field order
}

type settingsCallback struct {
	id uint64
	fn func(prev, next Settings)
}

// settingValue is a checked value of settingFields[i].
type settingValue struct {
	i int
	v any
}

// settingsCommit is what a settings write leaves for the cache once it has committed.
type settingsCommit struct {
	seq    uint64
	stored Settings
}

// settingChange is one field's entry in the settings.changed audit detail {changes:{field:{from,to}}}.
type settingChange struct {
	From any `json:"from"`
	To   any `json:"to"`
}

func newSettingsCache(db *DB) *SettingsCache {
	c := &SettingsCache{db: db, stored: defaultSettings()}
	c.cur.Store(&settingsView{settings: c.stored})
	return c
}

// load reads the stored settings (Open). A row with an unknown key or a value that fails validation is skipped with
// a WARN, so that field keeps its default; the row itself stays until an Update of that field replaces it, or
// removes it when the value sent is the default.
func (c *SettingsCache) load(ctx context.Context) error {
	var (
		s   Settings
		bad []badSetting
	)
	err := c.db.Read(ctx, func(q *Q) error {
		var err error
		s, bad, err = q.storedSettings()
		return err
	})
	if err != nil {
		return err
	}
	for _, b := range bad {
		c.db.log.Warn("ignoring a stored setting; the default applies", "key", b.key, "reason", b.reason)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = s
	c.publishLocked()
	return nil
}

// Get returns a snapshot of the current settings without locking.
func (c *SettingsCache) Get() Settings { return c.cur.Load().settings }

// Defaults returns the default settings, the single source of policy defaults (04's config registry repeats them for
// documentation only).
func (c *SettingsCache) Defaults() Settings { return defaultSettings() }

// Locked returns the JSON names of the fields that config pins, in field order (nil when none is pinned).
func (c *SettingsCache) Locked() []string { return slices.Clone(c.cur.Load().locked) }

// Pin forces a field (by JSON name) to value, as a TOML policy key does (04 §4.6). 04 calls it after Open and before
// serving, for every policy key that config sets. value is a Go value that marshals to the field's JSON type (a
// string, bool or integer; a json.RawMessage works too), and it goes through the same checks as Update: a value they
// reject returns *api.Error{Code: validation_failed, Fields: {field: code}}, which 04 reports as a config error. An
// unknown field is a plain error.
//
// A pinned field is listed by Locked, and Update refuses it with 409 setting_locked. Pinning never writes the DB. A
// Pin that changes the effective settings runs the OnChange callbacks. Pinning a field again replaces the value.
func (c *SettingsCache) Pin(field string, value any) error {
	i, ok := settingByName[field]
	if !ok {
		return fmt.Errorf("store: SettingsCache.Pin: unknown setting %q", field)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fieldErrors(map[string]string{field: api.FieldInvalid})
	}
	v, code := settingFields[i].decode(raw)
	if code != "" {
		return fieldErrors(map[string]string{field: code})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinned == nil {
		c.pinned = map[int]any{}
	}
	c.pinned[i] = v
	c.publishLocked()
	return nil
}

// Update applies a patch (JSON name → JSON value; PATCH is a merge, so only the fields present change) and returns
// the new settings. Unknown names are ignored, like every unknown request field (03 §12.1).
//
// Every field is checked before anything is written. A pinned field in the patch returns
// *api.Error{Code: setting_locked, Params: {field: <json name>}} (409); otherwise invalid values return
// *api.Error{Code: validation_failed, Fields: {<json name>: <field code>}} (422) for every bad field. null is invalid
// (send the default value to reset a field). Values are normalized (serverName: PRECIS Nickname).
//
// The fields whose value differs are written in one Write together with the settings.changed audit row
// ({changes: {field: {from, to}}}, actor a, target kind "settings"). A value equal to the default deletes its row,
// so only non-default values are stored. After the commit the cache swaps and the OnChange callbacks run. A patch
// that changes nothing writes nothing, except that a field's row that load skipped as invalid is replaced (or
// removed, for the default) without an audit row, since the effective value stays.
func (c *SettingsCache) Update(ctx context.Context, patch map[string]json.RawMessage, a Actor) (Settings, error) {
	vals, err := c.prepare(patch)
	if err != nil {
		return Settings{}, err
	}
	if len(vals) == 0 {
		return c.Get(), nil
	}
	var sc settingsCommit
	err = c.db.Write(ctx, func(q *Q) error {
		var err error
		sc, err = c.writeTx(q, vals, a)
		return err
	})
	if err != nil {
		return Settings{}, err
	}
	return c.apply(sc), nil
}

// UpdateTx is Update inside the caller's Write, for flows that change a setting together with other rows
// (setup/complete sets server.name in its own Write, 03 §7.8). It checks and writes exactly like Update, including
// the settings.changed audit row, and fails inside Read.
//
// The cache does not change until the caller runs apply, which it does only after its Write has committed (never
// after a failed one): apply swaps the cache, runs the OnChange callbacks and returns the new settings. A cache
// whose apply was skipped catches up at the next settings write.
func (c *SettingsCache) UpdateTx(q *Q, patch map[string]json.RawMessage, a Actor) (apply func() Settings, err error) {
	if !q.writable {
		return nil, errReadOnlyTx
	}
	vals, err := c.prepare(patch)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return c.Get, nil
	}
	sc, err := c.writeTx(q, vals, a)
	if err != nil {
		return nil, err
	}
	return func() Settings { return c.apply(sc) }, nil
}

// OnChange registers fn to run after every change of the effective settings (a committed Update or UpdateTx, or a
// Pin) with the settings before and after it. Callbacks run synchronously in registration order, on the goroutine
// that made the change, one change after another; concurrent writes whose commits race may reach them as one
// combined change. fn must be quick and must not call Pin, Update or an UpdateTx apply (Get, Locked and Defaults are
// fine). cancel removes fn; a change that is already running may still call it once.
func (c *SettingsCache) OnChange(fn func(prev, next Settings)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.nextCB++
	id := c.nextCB
	c.cbs = append(c.cbs, settingsCallback{id: id, fn: fn})
	return func() {
		c.cbMu.Lock()
		defer c.cbMu.Unlock()
		c.cbs = slices.DeleteFunc(c.cbs, func(cb settingsCallback) bool { return cb.id == id })
	}
}

// prepare checks a patch: a pinned field is setting_locked (before any value check), and every bad value is listed
// in one validation_failed. It returns the decoded values in field order.
func (c *SettingsCache) prepare(patch map[string]json.RawMessage) ([]settingValue, error) {
	locked := c.cur.Load().locked
	var (
		vals []settingValue
		bad  map[string]string
	)
	for i := range settingFields {
		f := &settingFields[i]
		raw, ok := patch[f.name]
		if !ok {
			continue
		}
		if slices.Contains(locked, f.name) {
			return nil, &api.Error{Code: api.CodeSettingLocked, Params: map[string]any{api.ParamField: f.name}}
		}
		v, code := f.decode(raw)
		if code != "" {
			if bad == nil {
				bad = map[string]string{}
			}
			bad[f.name] = code
			continue
		}
		vals = append(vals, settingValue{i: i, v: v})
	}
	if bad != nil {
		return nil, fieldErrors(bad)
	}
	return vals, nil
}

// writeTx writes checked values in q: it reads the stored settings in the same transaction, writes the rows of the
// fields that change and the audit row, and returns the new stored settings for apply.
func (c *SettingsCache) writeTx(q *Q, vals []settingValue, a Actor) (settingsCommit, error) {
	prev, bad, err := q.storedSettings()
	if err != nil {
		return settingsCommit{}, err
	}
	badKeys := make(map[string]bool, len(bad))
	for _, b := range bad {
		badKeys[b.key] = true
	}
	next := prev
	now := normMS(q.now())
	by := updatedBy(a)
	changes := map[string]settingChange{}
	for _, sv := range vals {
		f := &settingFields[sv.i]
		from := f.get(&prev)
		if from == sv.v {
			// The effective value stays, so there is no audit entry. A row that load skipped (its value is invalid,
			// so from is the default) is still replaced or removed, or it would stay, and be logged at every Open,
			// until the field changed to another value and back.
			if badKeys[f.key] {
				if err := q.writeSetting(f, sv.v, now, by); err != nil {
					return settingsCommit{}, err
				}
			}
			continue
		}
		f.set(&next, sv.v)
		changes[f.name] = settingChange{From: from, To: sv.v}
		if err := q.writeSetting(f, sv.v, now, by); err != nil {
			return settingsCommit{}, err
		}
	}
	if len(changes) > 0 {
		err := q.AppendAudit(AuditEntry{At: now, Action: "settings.changed", Actor: a, TargetKind: "settings",
			Detail: settingsChangedDetail(changes)})
		if err != nil {
			return settingsCommit{}, err
		}
		if c.failAfterAudit != nil {
			if err := c.failAfterAudit(q); err != nil {
				return settingsCommit{}, err
			}
		}
	}
	return settingsCommit{seq: c.seq.Add(1), stored: next}, nil
}

// writeSetting stores v as field f's row, or deletes the row when v is the default, so only non-default values are
// stored.
func (q *Q) writeSetting(f *settingField, v any, now time.Time, by string) error {
	defaults := defaultSettings()
	if v == f.get(&defaults) {
		_, err := q.execCount("delete setting", `DELETE FROM settings WHERE key = ?`, f.key)
		return err
	}
	value, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode setting %s: %w", f.key, err)
	}
	_, err = q.execCount("store setting", `INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value,
		updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		f.key, string(value), unixMS(now), by)
	return err
}

// apply puts a committed write's stored settings into the cache, unless a newer write is already there.
func (c *SettingsCache) apply(sc settingsCommit) Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sc.seq > c.applied {
		c.applied = sc.seq
		c.stored = sc.stored
		c.publishLocked()
	}
	return c.Get()
}

// publishLocked computes the effective settings from stored and pinned, swaps the view and, when the settings
// changed, runs the callbacks. c.mu is held.
func (c *SettingsCache) publishLocked() {
	next := c.stored
	var locked []string
	for i := range settingFields {
		if v, ok := c.pinned[i]; ok {
			settingFields[i].set(&next, v)
			locked = append(locked, settingFields[i].name)
		}
	}
	prev := c.cur.Load().settings
	c.cur.Store(&settingsView{settings: next, locked: locked})
	if prev == next {
		return
	}
	c.cbMu.Lock()
	cbs := slices.Clone(c.cbs)
	c.cbMu.Unlock()
	for _, cb := range cbs {
		cb.fn(prev, next)
	}
}

// badSetting is a settings row that storedSettings skipped.
type badSetting struct {
	key    string
	reason string // "unknown key" or the field code of the value
}

// storedSettings reads the settings table over the defaults. Rows with an unknown key or an invalid value are
// skipped and returned in bad.
func (q *Q) storedSettings() (s Settings, bad []badSetting, err error) {
	s = defaultSettings()
	err = q.queryAll("read settings", `SELECT key, value FROM settings ORDER BY key`, nil, func(r scanner) error {
		var key, value string
		if err := r.Scan(&key, &value); err != nil {
			return err
		}
		i, ok := settingByKey[key]
		if !ok {
			bad = append(bad, badSetting{key: key, reason: "unknown key"})
			return nil
		}
		v, code := settingFields[i].decode([]byte(value))
		if code != "" {
			bad = append(bad, badSetting{key: key, reason: code})
			return nil
		}
		settingFields[i].set(&s, v)
		return nil
	})
	if err != nil {
		return Settings{}, nil, err
	}
	return s, bad, nil
}

// updatedBy is the settings.updated_by value: the user ID, or the actor kind ("cli", "system").
func updatedBy(a Actor) string {
	if a.Kind == ActorUser && a.UserID != "" {
		return string(a.UserID)
	}
	return string(a.Kind)
}

// auditTextCuts are the lengths, in runes, that settingsChangedDetail tries in turn for text values; at 0 a text
// value that is not empty becomes "…".
var auditTextCuts = []int{auditTextRunes, 8, 0}

// settingsChangedDetail is the settings.changed audit detail. When it would pass the 1 KiB limit (a long server
// name among many changes), text values are cut to 32 runes, then to 8 and last to "…", until it fits, and
// "truncated": true is added. The row so always names every changed field, since encodeDetail would otherwise drop
// the whole changes key (the security-event query looks for changes.registrationMode). json.Marshal writes <, > and
// & as 6-byte escapes, which is why a rune count alone is not enough.
func settingsChangedDetail(changes map[string]settingChange) map[string]any {
	d := map[string]any{"changes": changes}
	if b, err := json.Marshal(d); err == nil && len(b) <= maxAuditDetail {
		return d
	}
	for _, n := range auditTextCuts {
		short := make(map[string]settingChange, len(changes))
		for name, ch := range changes {
			short[name] = settingChange{From: shortenText(ch.From, n), To: shortenText(ch.To, n)}
		}
		d = map[string]any{"changes": short, "truncated": true}
		if b, err := json.Marshal(d); err == nil && len(b) <= maxAuditDetail {
			break
		}
	}
	return d
}

// shortenText cuts a string value to n runes plus "…"; other values stay.
func shortenText(v any, n int) any {
	s, ok := v.(string)
	if !ok || utf8.RuneCountInString(s) <= n {
		return v
	}
	return string([]rune(s)[:n]) + "…"
}

// fieldErrors is the validation_failed error with its field codes.
func fieldErrors(fields map[string]string) error {
	return &api.Error{Code: api.CodeValidationFailed, Fields: fields}
}
