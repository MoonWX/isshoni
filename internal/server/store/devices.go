package store

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The device flow's rows (M2 02 §3, §4): RFC 8628 device codes, the linked devices, and their access and refresh
// tokens. Every code and token is stored only as an HMAC-SHA-256 hash; auth computes the hashes, the store compares
// bytes. auth also validates every field (names, kinds, versions) before it reaches the store; the CHECK
// constraints of 03 §5 are the backstop.

// Device is a native app linked with the device flow or the in-app password fallback (M2 02 §5.4, §14). A device
// lives while it has an unspent, unexpired refresh token; the janitor deletes it otherwise (Prune, M2 02 §6.7).
type Device struct {
	ID                   DeviceID
	UserID               UserID
	Name, ClientKind, OS string // Name is user content (never logged); ClientKind desktop|mobile; OS as devices.os
	AppVersion           string
	LinkedVia            string // device_flow | password
	CreatedAt            time.Time
	LastSeenAt           time.Time
	LastIP               string
}

// DeviceToken is an access or refresh token of a device (M2 02 §3, §6.2). A refresh token is spent once exchanged,
// or once a grace re-issue superseded it; a spent row stays until its own ExpiresAt, so that a replay is caught as
// reuse.
type DeviceToken struct {
	TokenHash  []byte
	DeviceID   DeviceID
	Kind       string // "access" | "refresh"
	CreatedAt  time.Time
	ExpiresAt  time.Time
	SpentAt    time.Time // refresh only: when it was exchanged or superseded; zero = unspent
	ReplacedBy []byte    // refresh only: hash of the refresh token it was exchanged for (or superseded by)
	// IssuedBy is the hash of the refresh token whose exchange issued this token (access or refresh); nil for the
	// pair a device-code or password grant issued (0002_device_flow).
	IssuedBy []byte
	// ExchangeHash is set on a spent refresh token: the HMAC of the exchangeId of the request that spent it; nil when
	// the client sent none, and for a token a grace path superseded (0002_device_flow).
	ExchangeHash []byte
}

// DeviceCode is an RFC 8628 authorization in progress (M2 02 §5). It is looked up by the hash of its device code
// (the app's polls) or of its normalized user code (the approval page).
type DeviceCode struct {
	DeviceCodeHash, UserCodeHash           []byte
	ClientKind, DeviceName, OS, AppVersion string
	RequestIP                              string
	CreatedAt, ExpiresAt                   time.Time
	Interval                               time.Duration // whole seconds (interval_s)
	LastPollAt                             time.Time     // zero = never polled
	Status                                 string        // pending|approved|denied|consumed
	UserID                                 UserID        // the approver; "" until decided
	DecidedAt                              time.Time
}

// defaultPollInterval is device_codes.interval_s's default (M2 02 §5.2: interval 5).
const defaultPollInterval = 5 * time.Second

// deviceCols are the columns scanned by deviceScan, in its order.
const deviceCols = `id, user_id, name, client_kind, os, app_version, linked_via, created_at, last_seen_at, last_ip`

type deviceScan struct {
	d             Device
	id, user      string
	created, seen int64
}

func (s *deviceScan) dest() []any {
	return []any{&s.id, &s.user, &s.d.Name, &s.d.ClientKind, &s.d.OS, &s.d.AppVersion, &s.d.LinkedVia, &s.created,
		&s.seen, &s.d.LastIP}
}

func (s *deviceScan) device() Device {
	out := s.d
	out.ID = DeviceID(s.id)
	out.UserID = UserID(s.user)
	out.CreatedAt = fromMS(s.created)
	out.LastSeenAt = fromMS(s.seen)
	return out
}

// scanDevices collects the rows of a deviceCols query.
func scanDevices(out *[]Device) func(scanner) error {
	return func(r scanner) error {
		var d deviceScan
		if err := r.Scan(d.dest()...); err != nil {
			return err
		}
		*out = append(*out, d.device())
		return nil
	}
}

// deviceTokenCols are the columns scanned by deviceTokenScan, in its order.
//
//nolint:gosec // G101 false positive: column names, not a credential.
const deviceTokenCols = `token_hash, device_id, kind, created_at, expires_at, spent_at, replaced_by, issued_by,
	exchange_hash`

type deviceTokenScan struct {
	t                DeviceToken
	device           string
	created, expires int64
	spent            sql.NullInt64
}

func (s *deviceTokenScan) dest() []any {
	return []any{&s.t.TokenHash, &s.device, &s.t.Kind, &s.created, &s.expires, &s.spent, &s.t.ReplacedBy,
		&s.t.IssuedBy, &s.t.ExchangeHash}
}

func (s *deviceTokenScan) token() DeviceToken {
	out := s.t
	out.DeviceID = DeviceID(s.device)
	out.CreatedAt = fromMS(s.created)
	out.ExpiresAt = fromMS(s.expires)
	out.SpentAt = fromNullMS(s.spent)
	return out
}

// deviceCodeCols are the columns scanned by deviceCodeScan, in its order.
const deviceCodeCols = `device_code_hash, user_code_hash, client_kind, device_name, os, app_version, request_ip,
	created_at, expires_at, interval_s, last_poll_at, status, user_id, decided_at`

type deviceCodeScan struct {
	c                          DeviceCode
	created, expires, interval int64
	lastPoll, decided          sql.NullInt64
	user                       sql.NullString
}

func (s *deviceCodeScan) dest() []any {
	return []any{&s.c.DeviceCodeHash, &s.c.UserCodeHash, &s.c.ClientKind, &s.c.DeviceName, &s.c.OS, &s.c.AppVersion,
		&s.c.RequestIP, &s.created, &s.expires, &s.interval, &s.lastPoll, &s.c.Status, &s.user, &s.decided}
}

func (s *deviceCodeScan) code() DeviceCode {
	out := s.c
	out.CreatedAt = fromMS(s.created)
	out.ExpiresAt = fromMS(s.expires)
	out.Interval = time.Duration(s.interval) * time.Second
	out.LastPollAt = fromNullMS(s.lastPoll)
	out.UserID = UserID(s.user.String)
	out.DecidedAt = fromNullMS(s.decided)
	return out
}

// intervalSeconds converts a poll interval to interval_s: whole seconds, at least 1.
func intervalSeconds(method string, d time.Duration) (int64, error) {
	s := int64(d / time.Second)
	if s < 1 {
		return 0, fmt.Errorf("store: %s: the poll interval %v is shorter than 1 s", method, d)
	}
	return s, nil
}

// refreshConflict is the ErrConflict of SpendRefreshToken and SupersedeRefreshToken: the token is spent already
// (a parallel request won), or there is no such refresh token.
func refreshConflict(method string) error {
	return fmt.Errorf("store: %s: the refresh token is spent or gone: %w", method, ErrConflict)
}

// ---- device codes ----

// CreateDeviceCode inserts a device code. DeviceCodeHash, UserCodeHash and ExpiresAt are required; a zero CreatedAt
// is the store's clock, a zero Interval is 5 s (whole seconds are stored), and an empty Status is "pending", the
// only status a new code has in the device flow (M2 02 §5.2). A user-code collision returns
// *ConflictError{"user_code_hash"}: the caller draws a new code (M2 02 §3). The device-code hash is the primary key
// and never collides in practice; a collision is a plain error, never a ConflictError, so it can't make the caller
// redraw the user code forever.
func (q *Q) CreateDeviceCode(dc DeviceCode) error {
	const method = "CreateDeviceCode"
	if err := requireBytes(method, "DeviceCodeHash", dc.DeviceCodeHash); err != nil {
		return err
	}
	if err := requireBytes(method, "UserCodeHash", dc.UserCodeHash); err != nil {
		return err
	}
	if err := requireTime(method, "ExpiresAt", dc.ExpiresAt); err != nil {
		return err
	}
	if dc.Interval == 0 {
		dc.Interval = defaultPollInterval
	}
	interval, err := intervalSeconds(method, dc.Interval)
	if err != nil {
		return err
	}
	if dc.Status == "" {
		dc.Status = "pending"
	}
	_, err = q.exec(`INSERT INTO device_codes (`+deviceCodeCols+`) VALUES (`+placeholders(14)+`)`,
		dc.DeviceCodeHash, dc.UserCodeHash, dc.ClientKind, dc.DeviceName, dc.OS, dc.AppVersion, dc.RequestIP,
		unixMS(q.orNow(dc.CreatedAt)), unixMS(dc.ExpiresAt), interval, msOrNull(dc.LastPollAt), dc.Status,
		strOrNull(string(dc.UserID)), msOrNull(dc.DecidedAt))
	if err != nil {
		if uniqueViolation(err) == "device_codes.user_code_hash" {
			return &ConflictError{Column: "user_code_hash"}
		}
		return fmt.Errorf("store: create device code: %w", err)
	}
	return nil
}

// DeviceCodeByHash returns the device code with this device-code hash, in any status (expired ones the janitor has
// not pruned yet included), or ErrNotFound.
func (q *Q) DeviceCodeByHash(h []byte) (DeviceCode, error) {
	return q.deviceCodeBy("device code by hash",
		`SELECT `+deviceCodeCols+` FROM device_codes WHERE device_code_hash = ?`, h)
}

// DeviceCodeByUserCode returns the device code with this user-code hash (of the normalized code), in any status,
// or ErrNotFound.
func (q *Q) DeviceCodeByUserCode(h []byte) (DeviceCode, error) {
	return q.deviceCodeBy("device code by user code",
		`SELECT `+deviceCodeCols+` FROM device_codes WHERE user_code_hash = ?`, h)
}

// deviceCodeBy reads one device code with a query that selects deviceCodeCols by one hash.
func (q *Q) deviceCodeBy(what, query string, h []byte) (DeviceCode, error) {
	if len(h) == 0 {
		return DeviceCode{}, ErrNotFound
	}
	var s deviceCodeScan
	if err := q.queryOne(what, query, []any{h}, s.dest()...); err != nil {
		return DeviceCode{}, err
	}
	return s.code(), nil
}

// CountPendingDeviceCodes counts the codes with status pending and expires_at > now (the server-wide cap of
// M2 02 §5.5).
func (q *Q) CountPendingDeviceCodes(now time.Time) (int, error) {
	var n int
	err := q.queryOne("count pending device codes",
		`SELECT count(*) FROM device_codes WHERE status = 'pending' AND expires_at > ?`, []any{unixMS(now)}, &n)
	return n, err
}

// DecideDeviceCode approves or denies the pending, unexpired code with this user-code hash (M2 02 §5.3): status is
// "approved" or "denied", u the approver, decided_at now, and a non-empty name replaces the device name the app
// sent. ErrNotFound when no code with this hash is pending with expires_at > now (unknown, decided already, or
// expired), so of two users deciding one code at once only the first succeeds.
func (q *Q) DecideDeviceCode(h []byte, status string, u UserID, name string, now time.Time) error {
	if status != "approved" && status != "denied" {
		return fmt.Errorf("store: DecideDeviceCode: status %q is neither approved nor denied", status)
	}
	if u == "" {
		return errors.New("store: DecideDeviceCode: the approver is required")
	}
	if len(h) == 0 {
		return ErrNotFound
	}
	ms := unixMS(now)
	return q.execOne("decide device code", `UPDATE device_codes SET status = ?, user_id = ?, decided_at = ?,
		device_name = COALESCE(NULLIF(?, ''), device_name)
		WHERE user_code_hash = ? AND status = 'pending' AND expires_at > ?`,
		status, string(u), ms, name, h, ms)
}

// PollDeviceCode records a poll of the code with this device-code hash: last_poll_at = now and interval_s =
// interval in whole seconds (at least 1 s; M2 02 §5.4's slow_down raises it). Any status. ErrNotFound for an unknown
// hash.
func (q *Q) PollDeviceCode(h []byte, now time.Time, interval time.Duration) error {
	s, err := intervalSeconds("PollDeviceCode", interval)
	if err != nil {
		return err
	}
	return q.execOne("poll device code", `UPDATE device_codes SET last_poll_at = ?, interval_s = ?
		WHERE device_code_hash = ?`, unixMS(now), s, h)
}

// ConsumeDeviceCode turns the approved code with this device-code hash into consumed: the poll that collects the
// approval (M2 02 §5.4), which also records last_poll_at = now. Only an approved code with expires_at > now is
// consumed; otherwise (unknown, pending, denied, consumed already, or expired) ErrNotFound, so a code yields one
// token pair at most.
func (q *Q) ConsumeDeviceCode(h []byte, now time.Time) error {
	ms := unixMS(now)
	return q.execOne("consume device code", `UPDATE device_codes SET status = 'consumed', last_poll_at = ?
		WHERE device_code_hash = ? AND status = 'approved' AND expires_at > ?`, ms, h, ms)
}

// ---- devices ----

// ListDevices returns a user's linked devices, the most recently seen first.
func (q *Q) ListDevices(u UserID) ([]Device, error) {
	var out []Device
	err := q.queryAll("list devices", `SELECT `+deviceCols+` FROM devices WHERE user_id = ?
		ORDER BY last_seen_at DESC, created_at DESC, rowid DESC`, []any{string(u)}, scanDevices(&out))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateDevice inserts d and sets d.ID (NewID). A zero CreatedAt is the store's clock and a zero LastSeenAt is
// CreatedAt; d is updated with these values. ClientKind, OS and LinkedVia must satisfy the CHECKs of devices. The
// caller enforces the per-user cap with TrimDevices first (M2 02 §6.7).
func (q *Q) CreateDevice(d *Device) error {
	d.CreatedAt = q.orNow(d.CreatedAt)
	if d.LastSeenAt.IsZero() {
		d.LastSeenAt = d.CreatedAt
	}
	d.LastSeenAt = normMS(d.LastSeenAt)
	id, err := q.withNewID("devices", func(id string) error {
		_, err := q.exec(`INSERT INTO devices (`+deviceCols+`) VALUES (`+placeholders(10)+`)`,
			id, string(d.UserID), d.Name, d.ClientKind, d.OS, d.AppVersion, d.LinkedVia, unixMS(d.CreatedAt),
			unixMS(d.LastSeenAt), d.LastIP)
		return err
	})
	if err != nil {
		return conflictOr("create device", err)
	}
	d.ID = DeviceID(id)
	return nil
}

// DeviceByID returns a device with its user, or ErrNotFound. The user's status is not checked here.
func (q *Q) DeviceByID(id DeviceID) (Device, User, error) {
	var ds deviceScan
	var us userScan
	err := q.queryOne("device by id", `SELECT `+prefixCols("d", deviceCols)+`, `+prefixCols("u", userCols)+`
		FROM devices d JOIN users u ON u.id = d.user_id WHERE d.id = ?`,
		[]any{string(id)}, append(ds.dest(), us.dest()...)...)
	if err != nil {
		return Device{}, User{}, err
	}
	return ds.device(), us.user(), nil
}

// RenameDevice sets the name of one of u's devices and returns the device as it is now. ErrNotFound when the device
// does not exist or is not u's.
func (q *Q) RenameDevice(u UserID, id DeviceID, name string) (Device, error) {
	var out []Device
	err := q.writeReturning("rename device", `UPDATE devices SET name = ? WHERE id = ? AND user_id = ?
		RETURNING `+deviceCols, []any{name, string(id), string(u)}, scanDevices(&out))
	if err != nil {
		return Device{}, err
	}
	if len(out) == 0 {
		return Device{}, ErrNotFound
	}
	return out[0], nil
}

// TrimDevices keeps the user's keep most recently seen devices (ties: the newest created) and deletes the rest with
// their tokens and push subscriptions (M2 02 §6.7: TrimDevices(u, 19) right before CreateDevice keeps the cap of 20,
// so the 21st link evicts the least recently seen device). It returns the deleted devices, least recently seen first,
// for their device.revoked {via: "cap"} audit rows; none when the user has at most keep devices.
func (q *Q) TrimDevices(u UserID, keep int) ([]Device, error) {
	var out []Device
	err := q.writeReturning("trim devices", `DELETE FROM devices WHERE id IN (SELECT id FROM devices WHERE user_id = ?
			ORDER BY last_seen_at DESC, created_at DESC, rowid DESC LIMIT -1 OFFSET ?) RETURNING `+deviceCols,
		[]any{string(u), max(keep, 0)}, scanDevices(&out))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Device) int {
		return cmp.Or(a.LastSeenAt.Compare(b.LastSeenAt), a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// TouchDevice records a use of the device (M2 02 §6.1, §6.2): last_seen_at = now, last_ip = ip and app_version =
// appVersion, where "" keeps the stored value of either. ErrNotFound for an unknown device.
func (q *Q) TouchDevice(id DeviceID, ip, appVersion string, now time.Time) error {
	return q.execOne("touch device", `UPDATE devices SET last_seen_at = ?,
		last_ip = CASE WHEN ? = '' THEN last_ip ELSE ? END,
		app_version = CASE WHEN ? = '' THEN app_version ELSE ? END
		WHERE id = ?`, unixMS(now), ip, ip, appVersion, appVersion, string(id))
}

// DeleteDevice deletes one of the user's devices and reports whether it existed. Its tokens and push subscriptions
// go with it.
func (q *Q) DeleteDevice(u UserID, id DeviceID) (bool, error) {
	n, err := q.execCount("delete device", `DELETE FROM devices WHERE id = ? AND user_id = ?`, string(id), string(u))
	return n > 0, err
}

// DeleteDevices deletes all of the user's devices and returns their IDs, sorted.
func (q *Q) DeleteDevices(u UserID) ([]DeviceID, error) {
	return deleteReturningIDs[DeviceID](q, "delete devices", `DELETE FROM devices WHERE user_id = ? RETURNING id`,
		string(u))
}

// DeleteDeviceCodesOf deletes the device codes the user approved or denied (03 §7.7: they go with the devices).
func (q *Q) DeleteDeviceCodesOf(u UserID) error {
	_, err := q.execCount("delete device codes", `DELETE FROM device_codes WHERE user_id = ?`, string(u))
	return err
}

// DeleteAllDeviceCodes deletes every device code, the ones nobody decided included (03 §4.6: the session key
// changed, so no code can turn into a token that still works), and returns how many there were.
func (q *Q) DeleteAllDeviceCodes() (int, error) {
	return q.execCount("delete all device codes", `DELETE FROM device_codes`)
}

// ---- device tokens ----

// InsertDeviceToken inserts a device token. TokenHash, DeviceID, Kind and ExpiresAt are required; a zero CreatedAt
// is the store's clock. A zero SpentAt and empty ReplacedBy, IssuedBy and ExchangeHash are NULL. The caller trims
// the device's access tokens with TrimAccessTokens (M2 02 §6.7).
func (q *Q) InsertDeviceToken(t DeviceToken) error {
	const method = "InsertDeviceToken"
	if err := requireBytes(method, "TokenHash", t.TokenHash); err != nil {
		return err
	}
	if t.DeviceID == "" || t.Kind == "" {
		return fmt.Errorf("store: %s: DeviceID and Kind are required", method)
	}
	if err := requireTime(method, "ExpiresAt", t.ExpiresAt); err != nil {
		return err
	}
	_, err := q.exec(`INSERT INTO device_tokens (`+deviceTokenCols+`) VALUES (`+placeholders(9)+`)`,
		t.TokenHash, string(t.DeviceID), t.Kind, unixMS(q.orNow(t.CreatedAt)), unixMS(t.ExpiresAt),
		msOrNull(t.SpentAt), bytesOrNull(t.ReplacedBy), bytesOrNull(t.IssuedBy), bytesOrNull(t.ExchangeHash))
	if err != nil {
		return conflictOr("insert device token", err)
	}
	return nil
}

// DeviceTokenByHash returns the token of this kind with this hash while it is unexpired (expires_at > now), spent
// or not, with its device and the device's user. ErrNotFound otherwise. The caller checks the user's status.
func (q *Q) DeviceTokenByHash(h []byte, kind string, now time.Time) (DeviceToken, Device, User, error) {
	if len(h) == 0 {
		return DeviceToken{}, Device{}, User{}, ErrNotFound
	}
	var ts deviceTokenScan
	var ds deviceScan
	var us userScan
	dest := append(append(ts.dest(), ds.dest()...), us.dest()...)
	err := q.queryOne("device token by hash", `SELECT `+prefixCols("t", deviceTokenCols)+`, `+
		prefixCols("d", deviceCols)+`, `+prefixCols("u", userCols)+`
		FROM device_tokens t JOIN devices d ON d.id = t.device_id JOIN users u ON u.id = d.user_id
		WHERE t.token_hash = ? AND t.kind = ? AND t.expires_at > ?`,
		[]any{h, kind, unixMS(now)}, dest...)
	if err != nil {
		return DeviceToken{}, Device{}, User{}, err
	}
	return ts.token(), ds.device(), us.user(), nil
}

// SpendRefreshToken marks an unspent refresh token as exchanged (M2 02 §6.2): spent_at = now, replaced_by =
// replacedBy (the new refresh token's hash, required) and exchange_hash = exchangeHash (nil when the client sent no
// exchangeId). When no unspent refresh token has this hash, it returns an error that wraps ErrConflict: a parallel
// request spent it first, and the caller treats its request as a replay.
func (q *Q) SpendRefreshToken(h, replacedBy, exchangeHash []byte, now time.Time) error {
	const method = "SpendRefreshToken"
	if err := requireBytes(method, "replacedBy", replacedBy); err != nil {
		return err
	}
	n, err := q.execCount("spend refresh token", `UPDATE device_tokens SET spent_at = ?, replaced_by = ?,
		exchange_hash = ? WHERE token_hash = ? AND kind = 'refresh' AND spent_at IS NULL`,
		unixMS(now), replacedBy, bytesOrNull(exchangeHash), h)
	if err != nil {
		return err
	}
	if n == 0 {
		return refreshConflict(method)
	}
	return nil
}

// SetReplacedBy points a spent refresh token at the new child a grace path issued in place of its old one
// (M2 02 §6.2). ErrNotFound when no spent refresh token has this hash.
func (q *Q) SetReplacedBy(h, replacedBy []byte) error {
	if err := requireBytes("SetReplacedBy", "replacedBy", replacedBy); err != nil {
		return err
	}
	return q.execOne("set replaced_by", `UPDATE device_tokens SET replaced_by = ?
		WHERE token_hash = ? AND kind = 'refresh' AND spent_at IS NOT NULL`, replacedBy, h)
}

// SupersedeRefreshToken marks the child a grace path replaces as spent instead of deleting it (M2 02 §6.2), so that
// whoever presents it later is caught as reuse: spent_at = now, replaced_by = replacedBy (required) and
// exchange_hash = NULL. The row stays until its own expires_at, like every spent refresh token. When no unspent
// refresh token has this hash, it returns an error that wraps ErrConflict (spent in parallel).
func (q *Q) SupersedeRefreshToken(h, replacedBy []byte, now time.Time) error {
	const method = "SupersedeRefreshToken"
	if err := requireBytes(method, "replacedBy", replacedBy); err != nil {
		return err
	}
	n, err := q.execCount("supersede refresh token", `UPDATE device_tokens SET spent_at = ?, replaced_by = ?,
		exchange_hash = NULL WHERE token_hash = ? AND kind = 'refresh' AND spent_at IS NULL`,
		unixMS(now), replacedBy, h)
	if err != nil {
		return err
	}
	if n == 0 {
		return refreshConflict(method)
	}
	return nil
}

// DeleteAccessTokensIssuedBy deletes the access tokens that the exchange of the refresh token h issued (kind access
// and issued_by = h) and returns how many there were. Refresh tokens are never deleted by it, and an empty h (the
// pairs of a device-code or password grant, issued_by NULL) deletes nothing.
func (q *Q) DeleteAccessTokensIssuedBy(h []byte) (int, error) {
	if len(h) == 0 {
		return 0, nil
	}
	return q.execCount("delete access tokens issued by", `DELETE FROM device_tokens
		WHERE kind = 'access' AND issued_by = ?`, h)
}

// TrimAccessTokens keeps the device's keep newest access tokens (by created_at, then expires_at) and deletes the
// rest, returning how many it deleted (M2 02 §6.7: TrimAccessTokens(device, 5) at every refresh). Refresh tokens are
// never deleted by it.
func (q *Q) TrimAccessTokens(id DeviceID, keep int) (int, error) {
	return q.execCount("trim access tokens", `DELETE FROM device_tokens WHERE rowid IN (SELECT rowid FROM device_tokens
			WHERE device_id = ? AND kind = 'access' ORDER BY created_at DESC, expires_at DESC, rowid DESC
			LIMIT -1 OFFSET ?)`, string(id), max(keep, 0))
}
