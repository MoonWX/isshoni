package store

import "time"

// Device is a native app linked with the device flow. The table ships in M1 so the revocation paths cover it; the
// flow itself is later (M2).
type Device struct {
	ID                   DeviceID
	UserID               UserID
	Name, ClientKind, OS string
	AppVersion           string
	LinkedVia            string // device_flow | password
	CreatedAt            time.Time
	LastSeenAt           time.Time
	LastIP               string
}

// DeviceToken is an access or refresh token of a device. Later (M2).
type DeviceToken struct {
	TokenHash  []byte
	DeviceID   DeviceID
	Kind       string // "access" | "refresh"
	CreatedAt  time.Time
	ExpiresAt  time.Time
	SpentAt    time.Time
	ReplacedBy []byte
}

// DeviceCode is a pending RFC 8628 authorization. Later (M2).
type DeviceCode struct {
	DeviceCodeHash, UserCodeHash           []byte
	ClientKind, DeviceName, OS, AppVersion string
	RequestIP                              string
	CreatedAt, ExpiresAt                   time.Time
	Interval                               time.Duration
	LastPollAt                             time.Time
	Status                                 string // pending|approved|denied|consumed
	UserID                                 UserID
	DecidedAt                              time.Time
}

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

// ListDevices returns a user's linked devices, the most recently seen first (empty in M1: linking is M2).
func (q *Q) ListDevices(u UserID) ([]Device, error) {
	var out []Device
	err := q.queryAll("list devices", `SELECT `+deviceCols+` FROM devices WHERE user_id = ?
		ORDER BY last_seen_at DESC, created_at DESC, rowid DESC`, []any{string(u)}, func(r scanner) error {
		var d deviceScan
		if err := r.Scan(d.dest()...); err != nil {
			return err
		}
		out = append(out, d.device())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

// CreateDeviceCode inserts a pending device code. Later (M2).
func (q *Q) CreateDeviceCode(dc DeviceCode) error { return notImplemented("CreateDeviceCode") }

// DeviceCodeByHash finds a device code by its device-code hash. Later (M2).
func (q *Q) DeviceCodeByHash(h []byte) (DeviceCode, error) {
	return DeviceCode{}, notImplemented("DeviceCodeByHash")
}

// DeviceCodeByUserCode finds a device code by its user-code hash. Later (M2).
func (q *Q) DeviceCodeByUserCode(h []byte) (DeviceCode, error) {
	return DeviceCode{}, notImplemented("DeviceCodeByUserCode")
}

// DecideDeviceCode approves or denies a device code. Later (M2).
func (q *Q) DecideDeviceCode(h []byte, status string, u UserID, now time.Time) error {
	return notImplemented("DecideDeviceCode")
}

// PollDeviceCode records a poll of a device code. Later (M2).
func (q *Q) PollDeviceCode(h []byte, now time.Time, interval time.Duration) error {
	return notImplemented("PollDeviceCode")
}

// CreateDevice inserts d and sets d.ID. Later (M2).
func (q *Q) CreateDevice(d *Device) error { return notImplemented("CreateDevice") }

// InsertDeviceToken inserts a device token. Later (M2).
func (q *Q) InsertDeviceToken(t DeviceToken) error { return notImplemented("InsertDeviceToken") }

// DeviceTokenByHash finds an unexpired device token with its device and user. Later (M2).
func (q *Q) DeviceTokenByHash(h []byte, kind string, now time.Time) (DeviceToken, Device, User, error) {
	return DeviceToken{}, Device{}, User{}, notImplemented("DeviceTokenByHash")
}

// SpendRefreshToken marks a refresh token as exchanged for replacedBy. Later (M2).
func (q *Q) SpendRefreshToken(h, replacedBy []byte, now time.Time) error {
	return notImplemented("SpendRefreshToken")
}

// TouchDevice records a use of the device. Later (M2).
func (q *Q) TouchDevice(id DeviceID, ip, appVersion string, now time.Time) error {
	return notImplemented("TouchDevice")
}
