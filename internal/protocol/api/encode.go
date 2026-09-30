package api

import (
	"bytes"
	"encoding/json"
)

// The MarshalJSON methods below enforce two wire rules of the package (doc.go) for every encoder, not only for the
// producers that remember them:
//   - timestamps are WireTime's form (wiretime.go): RFC 3339 UTC strings with exactly three fractional digits and a
//     Z suffix ("2026-10-01T12:00:00.000Z", 03 §3.3, the same as 01's timestamps and JavaScript's
//     Date.toISOString), whatever the location and precision of the time.Time. They are fixed width, so they also
//     sort as text;
//   - lists are always present: a nil slice encodes as [] and a nil AuditEntry.Detail as {}, never as null.
//
// Each method converts the value to a local type without methods (the same fields and tags), fills in nil
// collections and overrides each time.Time field with a WireTime field of the same JSON name, so encoding stays with
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

// ---- types.go ----

func (u User) MarshalJSON() ([]byte, error) {
	type plain User
	return marshal(struct {
		plain
		CreatedAt WireTime `json:"createdAt,omitzero"`
	}{plain(u), WireTime(u.CreatedAt)})
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
		ExpiresAt WireTime `json:"expiresAt"`
	}{plain(i), WireTime(i.ExpiresAt)})
}

func (s SessionInfo) MarshalJSON() ([]byte, error) {
	type plain SessionInfo
	return marshal(struct {
		plain
		CreatedAt  WireTime `json:"createdAt"`
		ExpiresAt  WireTime `json:"expiresAt,omitzero"`
		LastSeenAt WireTime `json:"lastSeenAt,omitzero"`
	}{plain(s), WireTime(s.CreatedAt), WireTime(s.ExpiresAt), WireTime(s.LastSeenAt)})
}

func (d DeviceInfo) MarshalJSON() ([]byte, error) {
	type plain DeviceInfo
	return marshal(struct {
		plain
		CreatedAt  WireTime `json:"createdAt,omitzero"`
		LastSeenAt WireTime `json:"lastSeenAt,omitzero"`
	}{plain(d), WireTime(d.CreatedAt), WireTime(d.LastSeenAt)})
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
		CreatedAt WireTime `json:"createdAt"`
	}{plain(r), WireTime(r.CreatedAt)})
}

func (i Invite) MarshalJSON() ([]byte, error) {
	type plain Invite
	p := plain(i)
	p.RedeemedBy = nonNil(p.RedeemedBy)
	return marshal(struct {
		plain
		CreatedAt WireTime `json:"createdAt"`
		ExpiresAt WireTime `json:"expiresAt"`
	}{p, WireTime(i.CreatedAt), WireTime(i.ExpiresAt)})
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
		CreatedAt   WireTime `json:"createdAt"`
		LastLoginAt WireTime `json:"lastLoginAt,omitzero"`
		LastSeenAt  WireTime `json:"lastSeenAt,omitzero"`
	}{plain(u), WireTime(u.CreatedAt), WireTime(u.LastLoginAt), WireTime(u.LastSeenAt)})
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
		ExpiresAt WireTime `json:"expiresAt"`
	}{plain(l), WireTime(l.ExpiresAt)})
}

func (u PendingUser) MarshalJSON() ([]byte, error) {
	type plain PendingUser
	return marshal(struct {
		plain
		RequestedAt WireTime `json:"requestedAt"`
	}{plain(u), WireTime(u.RequestedAt)})
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
		At WireTime `json:"at"`
	}{p, WireTime(e.At)})
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
		GeneratedAt WireTime `json:"generatedAt"`
	}{p, WireTime(d.GeneratedAt)})
}

func (s ServerInfo) MarshalJSON() ([]byte, error) {
	type plain ServerInfo
	p := plain(s)
	p.Advertised = nonNil(p.Advertised)
	return marshal(struct {
		plain
		StartedAt WireTime `json:"startedAt"`
	}{p, WireTime(s.StartedAt)})
}

func (t TLSInfo) MarshalJSON() ([]byte, error) {
	type plain TLSInfo
	p := plain(t)
	p.Names = nonNil(p.Names)
	return marshal(struct {
		plain
		NotBefore   WireTime `json:"notBefore,omitzero"`
		NotAfter    WireTime `json:"notAfter,omitzero"`
		NextRenewal WireTime `json:"nextRenewal,omitzero"`
		LastErrorAt WireTime `json:"lastErrorAt,omitzero"`
	}{p, WireTime(t.NotBefore), WireTime(t.NotAfter), WireTime(t.NextRenewal), WireTime(t.LastErrorAt)})
}

func (u UpdateInfo) MarshalJSON() ([]byte, error) {
	type plain UpdateInfo
	return marshal(struct {
		plain
		CheckedAt WireTime `json:"checkedAt"`
	}{plain(u), WireTime(u.CheckedAt)})
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
		ConnectedAt WireTime `json:"connectedAt"`
	}{plain(c), WireTime(c.ConnectedAt)})
}

func (s ShareLive) MarshalJSON() ([]byte, error) {
	type plain ShareLive
	p := plain(s)
	p.Layers = nonNil(p.Layers)
	return marshal(struct {
		plain
		StartedAt WireTime `json:"startedAt"`
	}{p, WireTime(s.StartedAt)})
}

func (d DoctorSummary) MarshalJSON() ([]byte, error) {
	type plain DoctorSummary
	return marshal(struct {
		plain
		RanAt WireTime `json:"ranAt"`
	}{plain(d), WireTime(d.RanAt)})
}

func (s ServerStatus) MarshalJSON() ([]byte, error) {
	type plain ServerStatus
	p := plain(s)
	p.Advertised = nonNil(p.Advertised)
	p.Listeners = nonNil(p.Listeners)
	return marshal(struct {
		plain
		StartedAt WireTime `json:"startedAt"`
	}{p, WireTime(s.StartedAt)})
}

// ---- doctor.go ----

func (r DoctorReport) MarshalJSON() ([]byte, error) {
	type plain DoctorReport
	p := plain(r)
	p.Checks = nonNil(p.Checks)
	return marshal(struct {
		plain
		RanAt WireTime `json:"ranAt"`
	}{p, WireTime(r.RanAt)})
}

// ---- conntest.go ----

func (s ConnTestServerInfo) MarshalJSON() ([]byte, error) {
	type plain ConnTestServerInfo
	p := plain(s)
	p.TCPPorts = nonNil(p.TCPPorts)
	return marshal(p)
}
