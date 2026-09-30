package api

// This file holds 04's Web Push payload (04 §14.3). A payload carries data, not English text: the service worker
// renders every type from en.json (05) and always shows a notification. Payloads stay under 1 KB.

// PushPayloadVersion is PushPayload.V.
const PushPayloadVersion = 1

// PushType is PushPayload.Type.
type PushType string

const (
	PushTypeShareStarted PushType = "share.started" // a share went live (not a replacement); TTL 600 s, urgency high
	PushTypeAdminAlert   PushType = "admin.alert"   // an admin alert; TTL 86400 s, urgency normal
	PushTypeTest         PushType = "push.test"     // POST /api/v1/push/test; TTL 60 s, urgency high
)

// AdminAlertKind is the kind of an admin alert (03 §7.11, plus 04's transfer_threshold). The service worker renders
// it from push.adminAlert.<kind>.
type AdminAlertKind string

const (
	AdminAlertKindAdminGranted            AdminAlertKind = "admin_granted"
	AdminAlertKindAdminRevoked            AdminAlertKind = "admin_revoked"
	AdminAlertKindAdminPasswordReset      AdminAlertKind = "admin_password_reset"
	AdminAlertKindRegistrationModeChanged AdminAlertKind = "registration_mode_changed"
	AdminAlertKindSignupPending           AdminAlertKind = "signup_pending"
	AdminAlertKindSecretsRotated          AdminAlertKind = "secrets_rotated"
	AdminAlertKindTransferThreshold       AdminAlertKind = "transfer_threshold"   // Target is "80" or "100"
	AdminAlertKindRefreshTokenReused      AdminAlertKind = "refresh_token_reused" // later (M2)
)

// PushPayload is the JSON a push message carries. The fields after URL depend on Type.
type PushPayload struct {
	V    int      `json:"v"` // PushPayloadVersion
	Type PushType `json:"type"`
	TS   int64    `json:"ts"`  // unix milliseconds
	Tag  string   `json:"tag"` // a newer notification with the same tag replaces the older one
	URL  string   `json:"url"` // an SPA route, e.g. "/r/lounge?focus=s_q7m2x9c4v8b1n5k3"

	// share.started
	Room    *NameRef `json:"room,omitempty"`
	User    *NameRef `json:"user,omitempty"` // the sharer; Name is the username
	ShareID string   `json:"shareId,omitempty"`

	// admin.alert
	Kind   AdminAlertKind `json:"kind,omitempty"`
	Actor  string         `json:"actor,omitempty"`  // a username, "cli" or "system"
	Target string         `json:"target,omitempty"` // a username, a threshold, or absent
}

// NameRef is {id, name}: PushPayload.Room and PushPayload.User.
type NameRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
