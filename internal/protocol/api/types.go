package api

import "time"

// This file holds 03's REST DTOs (03 §12.3–§12.4). Each type names the endpoints that use it; the numbers are the
// rows of 03's endpoint table.

// ---- shared enums ----

// Role is a user's role (03 §7.11).
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// UserStatus is a user's account status. RegisterResponse uses active and pending.
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusPending  UserStatus = "pending"
	UserStatusDisabled UserStatus = "disabled"
)

// RegistrationMode is the setting registration.mode (03 §7.9).
type RegistrationMode string

const (
	RegistrationModeInvite   RegistrationMode = "invite"
	RegistrationModeApproval RegistrationMode = "approval"
	RegistrationModeClosed   RegistrationMode = "closed"
)

// CreatedVia says how an account was created (AdminUser.CreatedVia).
type CreatedVia string

const (
	CreatedViaSetup  CreatedVia = "setup"
	CreatedViaInvite CreatedVia = "invite"
	CreatedViaSignup CreatedVia = "signup"
	CreatedViaCLI    CreatedVia = "cli" // reserved (03 §5)
)

// InviteState is an invite's state; inactive invites stay listed for 30 days.
type InviteState string

const (
	InviteStateActive  InviteState = "active"
	InviteStateExpired InviteState = "expired"
	InviteStateUsedUp  InviteState = "used_up"
	InviteStateRevoked InviteState = "revoked"
)

// ShareStartedPref is the push preference shareStarted (03 §12.4.6).
type ShareStartedPref string

const (
	ShareStartedPrefAll ShareStartedPref = "all"
	ShareStartedPrefOff ShareStartedPref = "off"
)

// AuditOutcome is an audit row's outcome (03 §5).
type AuditOutcome string

const (
	AuditOutcomeOK     AuditOutcome = "ok"
	AuditOutcomeDenied AuditOutcome = "denied"
)

// ---- shared shapes ----

// User is the public identity shape {id, username, role} that other docs reuse (login, register, setup and reset
// replies, Me). CreatedAt is set only in Me.
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// UserRef names a user: an invite's creator and redeemers, AdminUser.InvitedBy.
type UserRef struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Empty is the body of requests and replies without fields: {}.
type Empty struct{}

// UserResponse is {user}: login (#2), setup/complete (#8), reset/complete (#10).
type UserResponse struct {
	User User `json:"user"`
}

// ---- #1 info ----

// Info feature strings (Info.Features). The set is open: clients ignore unknown values.
const (
	InfoFeaturePush          = "push"          // present exactly when Info.Push is present
	InfoFeaturePasswordReset = "passwordReset" // M1
	InfoFeatureDeviceFlow    = "deviceFlow"    // later (M2)
)

// Info is GET /api/v1/info (#1, 03 §12.4.1).
type Info struct {
	Server           InfoServer       `json:"server"`
	Protocol         InfoProtocol     `json:"protocol"`         // from internal/protocol (01)
	MinClientVersion string           `json:"minClientVersion"` // "" = no floor
	Registration     RegistrationMode `json:"registration"`
	SetupRequired    bool             `json:"setupRequired"`
	Features         []string         `json:"features"` // InfoFeature* values
	AccountRules     AccountRules     `json:"accountRules"`
	Push             *InfoPush        `json:"push,omitempty"` // absent when push is unavailable
}

// InfoServer is Info.Server.
type InfoServer struct {
	Name      string `json:"name"` // the setting serverName, or else the host of the primary origin
	Version   string `json:"version"`
	PublicURL string `json:"publicUrl"`
}

// InfoProtocol is Info.Protocol: the signaling protocol versions this server speaks.
type InfoProtocol struct {
	Current int `json:"current"`
	Min     int `json:"min"`
}

// AccountRules are the form rules the SPA shows and checks before submitting.
type AccountRules struct {
	UsernameMinLength int `json:"usernameMinLength"`
	UsernameMaxLength int `json:"usernameMaxLength"`
	PasswordMinLength int `json:"passwordMinLength"`
	PasswordMaxLength int `json:"passwordMaxLength"`
}

// InfoPush is Info.Push.
type InfoPush struct {
	VAPIDPublicKey string `json:"vapidPublicKey"` // base64url, uncompressed P-256 point
}

// ---- #2–#10 auth ----

// LoginRequest is POST /api/v1/auth/login (#2); the reply is UserResponse.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegisterRequest is POST /api/v1/auth/register (#5).
type RegisterRequest struct {
	InviteToken string `json:"inviteToken,omitempty"` // absent: a public sign-up (approval mode only)
	Username    string `json:"username"`
	Password    string `json:"password"`
}

// RegisterResponse is the reply to #5: 201 {status: "active", user} with the cookie, or 202 {status: "pending"}.
type RegisterResponse struct {
	Status UserStatus `json:"status"`
	User   *User      `json:"user,omitempty"`
}

// TokenRequest is the {token} body of auth/invite/check (#6), auth/setup/check (#7) and auth/reset/check (#9).
type TokenRequest struct {
	Token string `json:"token"`
}

// InviteInfo is the reply to POST /api/v1/auth/invite/check (#6).
type InviteInfo struct {
	ServerName string `json:"serverName"`
	// InvitedBy is the creator's username. It is absent when the invite was created from the CLI or its creator was
	// deleted (invites.created_by is NULL, 03 §5), like Invite.CreatedBy.
	InvitedBy string    `json:"invitedBy,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsesLeft  int       `json:"usesLeft"`
}

// SetupCompleteRequest is POST /api/v1/auth/setup/complete (#8); the reply is UserResponse.
type SetupCompleteRequest struct {
	Token      string `json:"token"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	ServerName string `json:"serverName,omitempty"`
}

// ResetCheckResponse is the reply to POST /api/v1/auth/reset/check (#9).
type ResetCheckResponse struct {
	Username string `json:"username"`
}

// ResetCompleteRequest is POST /api/v1/auth/reset/complete (#10); the reply is UserResponse.
type ResetCompleteRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// ---- #11–#18 me ----

// Me is GET /api/v1/me (#11, 03 §12.4.3).
type Me struct {
	User        User         `json:"user"`
	Session     *SessionInfo `json:"session,omitempty"` // the caller's web session (always set in M1)
	Device      *DeviceInfo  `json:"device,omitempty"`  // later (M2): a bearer principal gets its device instead
	Permissions Permissions  `json:"permissions"`
	Badges      *Badges      `json:"badges,omitempty"` // admins only
}

// Permissions is Me.Permissions.
type Permissions struct {
	Admin         bool `json:"admin"`
	CreateInvites bool `json:"createInvites"` // admins, or members while membersCanInvite is on
}

// Badges is Me.Badges: counts for the admin navigation.
type Badges struct {
	PendingApprovals int `json:"pendingApprovals"`
}

// SessionInfo is one web session: Me.Session (with expiresAt) and the items of GET /api/v1/me/sessions (with
// lastSeenAt and lastIp).
type SessionInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"` // from the User-Agent, e.g. "Chrome on Windows"
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt,omitzero"`
	LastSeenAt time.Time `json:"lastSeenAt,omitzero"`
	LastIP     string    `json:"lastIp,omitempty"`
	Current    bool      `json:"current"` // this request's session
}

// DeviceInfo is one linked app: the items of GET /api/v1/me/devices (#17, empty until M2) and, later (M2), Me.Device
// with only id, name and clientKind.
type DeviceInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	ClientKind string    `json:"clientKind"` // 01's ClientKind: desktop | mobile
	OS         string    `json:"os,omitempty"`
	AppVersion string    `json:"appVersion,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	LastSeenAt time.Time `json:"lastSeenAt,omitzero"`
	LastIP     string    `json:"lastIp,omitempty"`
}

// ChangePasswordRequest is POST /api/v1/me/password (#12); the reply is Empty.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// DeleteSelfRequest is POST /api/v1/me/delete (#13).
type DeleteSelfRequest struct {
	Password string `json:"password"`
}

// SessionsResponse is GET /api/v1/me/sessions (#14).
type SessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// RevokeOthersResponse is the reply to POST /api/v1/me/sessions/revoke-others (#16).
type RevokeOthersResponse struct {
	Revoked int `json:"revoked"`
}

// DevicesResponse is GET /api/v1/me/devices (#17).
type DevicesResponse struct {
	Devices []DeviceInfo `json:"devices"`
}

// ---- #19, #37–#39 rooms ----

// Rooms is GET /api/v1/rooms (#19, 03 §12.4.4).
type Rooms struct {
	DefaultRoomID string `json:"defaultRoomId"`
	ShowRoomList  bool   `json:"showRoomList"` // more than one room exists
	Rooms         []Room `json:"rooms"`
}

// Room is one room. Live counts come from the signaling hub and are never stored.
type Room struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	IsDefault bool         `json:"isDefault"`
	CreatedAt time.Time    `json:"createdAt"`
	Live      RoomPresence `json:"live"`
}

// RoomPresence is Room.Live.
type RoomPresence struct {
	Participants int `json:"participants"`
	Shares       int `json:"shares"`
}

// CreateRoomRequest is POST /api/v1/admin/rooms (#37); the reply is RoomResponse.
type CreateRoomRequest struct {
	Name string `json:"name"`
}

// PatchRoomRequest is PATCH /api/v1/admin/rooms/{id} (#38), a JSON merge; the reply is RoomResponse.
type PatchRoomRequest struct {
	Name *string `json:"name,omitempty"`
}

// RoomResponse is {room}: the reply to #37 and #38.
type RoomResponse struct {
	Room Room `json:"room"`
}

// ---- #21–#23 invites ----

// Invite is one invite link's metadata (03 §12.4.5). The token itself is returned only once, in
// CreateInviteResponse.URL.
type Invite struct {
	ID         string      `json:"id"`
	Note       string      `json:"note"`
	CreatedBy  *UserRef    `json:"createdBy,omitempty"` // absent when created by the CLI or the creator was deleted
	CreatedAt  time.Time   `json:"createdAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	MaxUses    int         `json:"maxUses"`
	Uses       int         `json:"uses"`
	State      InviteState `json:"state"`
	RedeemedBy []UserRef   `json:"redeemedBy"`
}

// CreateInviteRequest is POST /api/v1/invites (#22). Every field is optional; 0 means the setting's default.
type CreateInviteRequest struct {
	Note           string `json:"note,omitempty"`
	ExpiresInHours int    `json:"expiresInHours,omitempty"` // 1–720
	MaxUses        int    `json:"maxUses,omitempty"`        // 1–1000
}

// CreateInviteResponse is the reply to #22.
type CreateInviteResponse struct {
	Invite Invite `json:"invite"`
	URL    string `json:"url"` // <primary origin>/invite#<token>
}

// InvitesResponse is GET /api/v1/invites?state=active|all (#21).
type InvitesResponse struct {
	Invites []Invite `json:"invites"`
}

// ---- #24–#28, #50–#51 push subscriptions and preferences ----

// PushSubscribeRequest is POST /api/v1/push/subscriptions (#24): the browser's PushSubscription.toJSON(), trimmed.
type PushSubscribeRequest struct {
	Endpoint string   `json:"endpoint"`
	Keys     PushKeys `json:"keys"`
}

// PushKeys is PushSubscribeRequest.Keys (base64url).
type PushKeys struct {
	P256dh string `json:"p256dh"` // 65 bytes, starting with 0x04
	Auth   string `json:"auth"`   // 16 bytes
}

// PushSubscribeResponse is the reply to #24: 201 when new, 200 when the endpoint was known.
type PushSubscribeResponse struct {
	ID string `json:"id"`
}

// PushUnsubscribeRequest is POST /api/v1/push/unsubscribe (#27).
type PushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// PushPreferences is GET and PUT /api/v1/push/preferences (#50, #51).
type PushPreferences struct {
	ShareStarted ShareStartedPref `json:"shareStarted"` // default all
	AdminAlerts  bool             `json:"adminAlerts"`  // default true; matters for admins only
}

// ---- #29–#36 admin users and approvals ----

// AdminUser is one row of GET /api/v1/admin/users (#29) and the reply of #30 and #35. It has no IPs.
type AdminUser struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Role         Role       `json:"role"`
	Status       UserStatus `json:"status"`
	CreatedVia   CreatedVia `json:"createdVia"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastLoginAt  time.Time  `json:"lastLoginAt,omitzero"`
	LastSeenAt   time.Time  `json:"lastSeenAt,omitzero"` // latest over sessions and devices
	InvitedBy    *UserRef   `json:"invitedBy,omitempty"` // the creator of the invite this user redeemed
	Sessions     int        `json:"sessions"`
	Devices      int        `json:"devices"`
	Online       bool       `json:"online"` // from the signaling hub
	ResetPending bool       `json:"resetPending"`
}

// AdminUsersResponse is GET /api/v1/admin/users?status=active|pending|disabled (#29).
type AdminUsersResponse struct {
	Users []AdminUser `json:"users"`
}

// AdminUserResponse is {user}: the reply to PATCH /api/v1/admin/users/{id} (#30) and to approve (#35).
type AdminUserResponse struct {
	User AdminUser `json:"user"`
}

// PatchUserRequest is PATCH /api/v1/admin/users/{id} (#30), a JSON merge. CurrentPassword is needed only when Role
// becomes admin; Status accepts only active or disabled.
type PatchUserRequest struct {
	Username        *string     `json:"username,omitempty"`
	Role            *Role       `json:"role,omitempty"`
	Status          *UserStatus `json:"status,omitempty"`
	CurrentPassword string      `json:"currentPassword,omitempty"`
}

// PasswordResetRequest is POST /api/v1/admin/users/{id}/password-reset (#32); CurrentPassword is needed only when
// the target is an admin. The reply is ResetLink.
type PasswordResetRequest struct {
	CurrentPassword string `json:"currentPassword,omitempty"`
}

// ResetLink is the reply to #32: a one-time password-reset link.
type ResetLink struct {
	URL       string    `json:"url"` // <primary origin>/reset#<token>
	ExpiresAt time.Time `json:"expiresAt"`
}

// SignOutResponse is the reply to POST /api/v1/admin/users/{id}/sign-out (#33).
type SignOutResponse struct {
	Sessions int `json:"sessions"`
	Devices  int `json:"devices"`
}

// PendingUser is one sign-up in the approval queue.
type PendingUser struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	RequestedAt time.Time `json:"requestedAt"`
	IP          string    `json:"ip"` // the sign-up IP, from the audit row
}

// ApprovalsResponse is GET /api/v1/admin/approvals (#34).
type ApprovalsResponse struct {
	Pending []PendingUser `json:"pending"`
}

// RejectRequest is the body of POST /api/v1/admin/approvals/{id}/reject (#36). With {id} = "all", All must be true.
type RejectRequest struct {
	All bool `json:"all,omitempty"`
}

// RejectAllResponse is the reply to #36 in its "all" form.
type RejectAllResponse struct {
	Rejected int `json:"rejected"`
}

// ---- #40–#41 settings ----

// Settings mirrors store.Settings (03 §9): the runtime settings with their JSON names, which are also the names in
// SettingsResponse.Locked and in setting_locked's params.field.
type Settings struct {
	ServerName             string           `json:"serverName"`
	RegistrationMode       RegistrationMode `json:"registrationMode"`
	InviteDefaultTTLHours  int              `json:"inviteDefaultTtlHours"`
	InviteDefaultMaxUses   int              `json:"inviteDefaultMaxUses"`
	MembersCanInvite       bool             `json:"membersCanInvite"`
	MaxParticipantsPerRoom int              `json:"maxParticipantsPerRoom"` // 0 = no limit
	MaxSharesPerRoom       int              `json:"maxSharesPerRoom"`       // 0 = no limit
	MaxShareBitrateKbps    int              `json:"maxShareBitrateKbps"`    // 0 = preset default
	TransferAlertGB        int              `json:"transferAlertGb"`        // 0 = off; 1 GB = 10⁹ bytes
	UpdateCheck            bool             `json:"updateCheck"`
	MinClientVersion       string           `json:"minClientVersion"` // "" or SemVer
	SetupWizardDone        bool             `json:"setupWizardDone"`
}

// SettingsResponse is GET and PATCH /api/v1/admin/settings (#40, #41). The PATCH body is a partial Settings.
type SettingsResponse struct {
	Settings Settings `json:"settings"`
	Defaults Settings `json:"defaults"`
	Locked   []string `json:"locked"` // JSON names pinned by config
}

// ---- #42 audit, #43 dashboard accounts ----

// AuditEntry is one audit-log row (03 §10). Names are snapshots taken when the row was written.
type AuditEntry struct {
	ID      int64          `json:"id"` // the pagination cursor
	At      time.Time      `json:"at"`
	Action  string         `json:"action"` // e.g. "user.role_changed" (03 §10)
	Outcome AuditOutcome   `json:"outcome"`
	Actor   AuditRef       `json:"actor"`
	Target  *AuditRef      `json:"target,omitempty"` // absent when the action has no target
	IP      string         `json:"ip,omitempty"`
	Detail  map[string]any `json:"detail"` // small, never secrets; {} when empty
}

// AuditRef is an audit row's actor or target. Actor kinds: user, cli, system, anonymous. Target kinds: user,
// session, device, invite, room, settings, setup.
type AuditRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// AuditPage is GET /api/v1/admin/audit?before&limit&action&actor&target (#42). NextBefore is always present and null
// on the last page; its tstype tag makes tygo emit `nextBefore: number | null` instead of an optional field.
type AuditPage struct {
	Entries    []AuditEntry `json:"entries"`
	NextBefore *int64       `json:"nextBefore" tstype:"number | null,required"` // null on the last page
}

// DashboardAccounts is the accounts part of 04's admin dashboard (03 §12.4.8), built by httpapi's
// (*API).DashboardAccounts.
type DashboardAccounts struct {
	Users            DashboardUserCounts    `json:"users"`
	Invites          DashboardInviteCounts  `json:"invites"`
	Sessions         DashboardSessionCounts `json:"sessions"`
	Devices          DashboardDeviceCounts  `json:"devices"`
	RegistrationMode RegistrationMode       `json:"registrationMode"`
	SecurityEvents   []AuditEntry           `json:"securityEvents"` // ≤ 20 security-flagged rows of the last 7 days
}

// DashboardUserCounts is DashboardAccounts.Users.
type DashboardUserCounts struct {
	Active   int `json:"active"`
	Pending  int `json:"pending"`
	Disabled int `json:"disabled"`
	Admins   int `json:"admins"`
}

// DashboardInviteCounts is DashboardAccounts.Invites.
type DashboardInviteCounts struct {
	Active int `json:"active"`
}

// DashboardSessionCounts is DashboardAccounts.Sessions.
type DashboardSessionCounts struct {
	Active int `json:"active"`
}

// DashboardDeviceCounts is DashboardAccounts.Devices.
type DashboardDeviceCounts struct {
	Linked int `json:"linked"`
}
