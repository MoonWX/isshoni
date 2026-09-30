package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// The MarshalJSON methods below enforce two wire rules of the package (doc.go) for every encoder, not only for the
// producers that remember them:
//   - timestamps are RFC 3339 UTC strings with exactly three fractional digits ("2026-10-01T12:00:00.000Z", 03 §3.3,
//     the same as 01's timestamps and JavaScript's Date.toISOString), whatever the location and precision of the
//     time.Time. They are fixed width, so they also sort as text;
//   - lists are always present: a nil slice encodes as [] and a nil AuditEntry.Detail as {}, never as null.
//
// Each method converts the value to a local type without methods (the same fields and tags), fills in nil
// collections and overrides each time.Time field with an msTime field of the same JSON name, so encoding stays with
// encoding/json. The overriding timestamp fields come last in the encoded object (JSON objects are unordered; the
// goldens show the order). Decoding is unchanged: time.Time accepts any RFC 3339 form.
//
// Every DTO with a time.Time field, or a slice or map field without omitempty, needs a method here:
// TestTimeFieldsHaveMarshalers and TestNoNullZeroValues fail on a type that is missing one. No DTO is embedded in
// another (TestJSONNamesCamelCase), so none of these methods is promoted to an outer type.

// marshal encodes v like json.Marshal but without HTML escaping. The encoder that called MarshalJSON escapes the
// result again when its own setting says so, so the output is the same as if the method did not exist: json.Marshal
// escapes "&", an Encoder with SetEscapeHTML(false) keeps it.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// msTime encodes a time.Time as RFC 3339 UTC with exactly three fractional digits (truncated, not rounded).
type msTime time.Time

const msTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// errTimeRange is returned for a timestamp that RFC 3339 cannot hold, as time.Time.MarshalJSON does.
var errTimeRange = errors.New("api: time year outside of range [0,9999]")

func (t msTime) MarshalJSON() ([]byte, error) {
	tt := time.Time(t).UTC()
	if y := tt.Year(); y < 0 || y > 9999 {
		return nil, errTimeRange
	}
	b := make([]byte, 0, len(msTimeLayout)+2)
	b = append(b, '"')
	b = tt.AppendFormat(b, msTimeLayout)
	return append(b, '"'), nil
}

// IsZero makes omitzero fields behave as they do with time.Time: a zero time in any location is absent.
func (t msTime) IsZero() bool { return time.Time(t).IsZero() }

// ---- types.go ----

func (u User) MarshalJSON() ([]byte, error) {
	type plain User
	return marshal(struct {
		plain
		CreatedAt msTime `json:"createdAt,omitzero"`
	}{plain(u), msTime(u.CreatedAt)})
}

func (i Info) MarshalJSON() ([]byte, error) {
	type plain Info
	p := plain(i)
	p.Features = nonNil(p.Features)
	return marshal(p)
}

func (i InviteInfo) MarshalJSON() ([]byte, error) {
	type plain InviteInfo
	return marshal(struct {
		plain
		ExpiresAt msTime `json:"expiresAt"`
	}{plain(i), msTime(i.ExpiresAt)})
}

func (s SessionInfo) MarshalJSON() ([]byte, error) {
	type plain SessionInfo
	return marshal(struct {
		plain
		CreatedAt  msTime `json:"createdAt"`
		ExpiresAt  msTime `json:"expiresAt,omitzero"`
		LastSeenAt msTime `json:"lastSeenAt,omitzero"`
	}{plain(s), msTime(s.CreatedAt), msTime(s.ExpiresAt), msTime(s.LastSeenAt)})
}

func (d DeviceInfo) MarshalJSON() ([]byte, error) {
	type plain DeviceInfo
	return marshal(struct {
		plain
		CreatedAt  msTime `json:"createdAt,omitzero"`
		LastSeenAt msTime `json:"lastSeenAt,omitzero"`
	}{plain(d), msTime(d.CreatedAt), msTime(d.LastSeenAt)})
}

func (r SessionsResponse) MarshalJSON() ([]byte, error) {
	type plain SessionsResponse
	p := plain(r)
	p.Sessions = nonNil(p.Sessions)
	return marshal(p)
}

func (r DevicesResponse) MarshalJSON() ([]byte, error) {
	type plain DevicesResponse
	p := plain(r)
	p.Devices = nonNil(p.Devices)
	return marshal(p)
}

func (r Rooms) MarshalJSON() ([]byte, error) {
	type plain Rooms
	p := plain(r)
	p.Rooms = nonNil(p.Rooms)
	return marshal(p)
}

func (r Room) MarshalJSON() ([]byte, error) {
	type plain Room
	return marshal(struct {
		plain
		CreatedAt msTime `json:"createdAt"`
	}{plain(r), msTime(r.CreatedAt)})
}

func (i Invite) MarshalJSON() ([]byte, error) {
	type plain Invite
	p := plain(i)
	p.RedeemedBy = nonNil(p.RedeemedBy)
	return marshal(struct {
		plain
		CreatedAt msTime `json:"createdAt"`
		ExpiresAt msTime `json:"expiresAt"`
	}{p, msTime(i.CreatedAt), msTime(i.ExpiresAt)})
}

func (r InvitesResponse) MarshalJSON() ([]byte, error) {
	type plain InvitesResponse
	p := plain(r)
	p.Invites = nonNil(p.Invites)
	return marshal(p)
}

func (u AdminUser) MarshalJSON() ([]byte, error) {
	type plain AdminUser
	return marshal(struct {
		plain
		CreatedAt   msTime `json:"createdAt"`
		LastLoginAt msTime `json:"lastLoginAt,omitzero"`
		LastSeenAt  msTime `json:"lastSeenAt,omitzero"`
	}{plain(u), msTime(u.CreatedAt), msTime(u.LastLoginAt), msTime(u.LastSeenAt)})
}

func (r AdminUsersResponse) MarshalJSON() ([]byte, error) {
	type plain AdminUsersResponse
	p := plain(r)
	p.Users = nonNil(p.Users)
	return marshal(p)
}

func (l ResetLink) MarshalJSON() ([]byte, error) {
	type plain ResetLink
	return marshal(struct {
		plain
		ExpiresAt msTime `json:"expiresAt"`
	}{plain(l), msTime(l.ExpiresAt)})
}

func (u PendingUser) MarshalJSON() ([]byte, error) {
	type plain PendingUser
	return marshal(struct {
		plain
		RequestedAt msTime `json:"requestedAt"`
	}{plain(u), msTime(u.RequestedAt)})
}

func (r ApprovalsResponse) MarshalJSON() ([]byte, error) {
	type plain ApprovalsResponse
	p := plain(r)
	p.Pending = nonNil(p.Pending)
	return marshal(p)
}

func (r SettingsResponse) MarshalJSON() ([]byte, error) {
	type plain SettingsResponse
	p := plain(r)
	p.Locked = nonNil(p.Locked)
	return marshal(p)
}

func (e AuditEntry) MarshalJSON() ([]byte, error) {
	type plain AuditEntry
	p := plain(e)
	if p.Detail == nil {
		p.Detail = map[string]any{}
	}
	return marshal(struct {
		plain
		At msTime `json:"at"`
	}{p, msTime(e.At)})
}

func (a AuditPage) MarshalJSON() ([]byte, error) {
	type plain AuditPage
	p := plain(a)
	p.Entries = nonNil(p.Entries)
	return marshal(p)
}

func (d DashboardAccounts) MarshalJSON() ([]byte, error) {
	type plain DashboardAccounts
	p := plain(d)
	p.SecurityEvents = nonNil(p.SecurityEvents)
	return marshal(p)
}

// ---- ops.go ----

func (d OpsDashboard) MarshalJSON() ([]byte, error) {
	type plain OpsDashboard
	p := plain(d)
	p.Rooms = nonNil(p.Rooms)
	p.Clients = nonNil(p.Clients)
	p.Alerts = nonNil(p.Alerts)
	return marshal(struct {
		plain
		GeneratedAt msTime `json:"generatedAt"`
	}{p, msTime(d.GeneratedAt)})
}

func (s ServerInfo) MarshalJSON() ([]byte, error) {
	type plain ServerInfo
	p := plain(s)
	p.Advertised = nonNil(p.Advertised)
	return marshal(struct {
		plain
		StartedAt msTime `json:"startedAt"`
	}{p, msTime(s.StartedAt)})
}

func (t TLSInfo) MarshalJSON() ([]byte, error) {
	type plain TLSInfo
	p := plain(t)
	p.Names = nonNil(p.Names)
	return marshal(struct {
		plain
		NotBefore   msTime `json:"notBefore,omitzero"`
		NotAfter    msTime `json:"notAfter,omitzero"`
		NextRenewal msTime `json:"nextRenewal,omitzero"`
		LastErrorAt msTime `json:"lastErrorAt,omitzero"`
	}{p, msTime(t.NotBefore), msTime(t.NotAfter), msTime(t.NextRenewal), msTime(t.LastErrorAt)})
}

func (u UpdateInfo) MarshalJSON() ([]byte, error) {
	type plain UpdateInfo
	return marshal(struct {
		plain
		CheckedAt msTime `json:"checkedAt"`
	}{plain(u), msTime(u.CheckedAt)})
}

func (r RoomLive) MarshalJSON() ([]byte, error) {
	type plain RoomLive
	p := plain(r)
	p.Participants = nonNil(p.Participants)
	p.Shares = nonNil(p.Shares)
	return marshal(p)
}

func (pl ParticipantLive) MarshalJSON() ([]byte, error) {
	type plain ParticipantLive
	p := plain(pl)
	p.Connections = nonNil(p.Connections)
	p.Watching = nonNil(p.Watching)
	return marshal(p)
}

func (c ConnectionLive) MarshalJSON() ([]byte, error) {
	type plain ConnectionLive
	return marshal(struct {
		plain
		ConnectedAt msTime `json:"connectedAt"`
	}{plain(c), msTime(c.ConnectedAt)})
}

func (s ShareLive) MarshalJSON() ([]byte, error) {
	type plain ShareLive
	p := plain(s)
	p.Layers = nonNil(p.Layers)
	return marshal(struct {
		plain
		StartedAt msTime `json:"startedAt"`
	}{p, msTime(s.StartedAt)})
}

func (d DoctorSummary) MarshalJSON() ([]byte, error) {
	type plain DoctorSummary
	return marshal(struct {
		plain
		RanAt msTime `json:"ranAt"`
	}{plain(d), msTime(d.RanAt)})
}

func (s ServerStatus) MarshalJSON() ([]byte, error) {
	type plain ServerStatus
	p := plain(s)
	p.Advertised = nonNil(p.Advertised)
	p.Listeners = nonNil(p.Listeners)
	return marshal(struct {
		plain
		StartedAt msTime `json:"startedAt"`
	}{p, msTime(s.StartedAt)})
}

// ---- doctor.go ----

func (r DoctorReport) MarshalJSON() ([]byte, error) {
	type plain DoctorReport
	p := plain(r)
	p.Checks = nonNil(p.Checks)
	return marshal(struct {
		plain
		RanAt msTime `json:"ranAt"`
	}{p, msTime(r.RanAt)})
}

// ---- conntest.go ----

func (s ConnTestServerInfo) MarshalJSON() ([]byte, error) {
	type plain ConnTestServerInfo
	p := plain(s)
	p.TCPPorts = nonNil(p.TCPPorts)
	return marshal(p)
}
