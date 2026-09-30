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

// ListDevices returns a user's linked devices.
func (q *Q) ListDevices(u UserID) ([]Device, error) { return nil, notImplemented("ListDevices") }

// DeleteDevice deletes one of the user's devices and reports whether it existed.
func (q *Q) DeleteDevice(u UserID, id DeviceID) (bool, error) {
	return false, notImplemented("DeleteDevice")
}

// DeleteDevices deletes all of the user's devices and returns their IDs.
func (q *Q) DeleteDevices(u UserID) ([]DeviceID, error) { return nil, notImplemented("DeleteDevices") }

// DeleteDeviceCodesOf deletes the device codes the user approved or denied.
func (q *Q) DeleteDeviceCodesOf(u UserID) error { return notImplemented("DeleteDeviceCodesOf") }

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
