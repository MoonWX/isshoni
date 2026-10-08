package push

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The payloads of 04 §14.3. A payload carries data, never English text: the service worker renders every type
// from en.json and always shows a notification (iOS revokes a subscription after silent pushes, so there is no
// silent type).

// Per-type delivery options (04 §14.3).
const (
	ttlShareStarted = 600 * time.Second   // a stream that started ten minutes ago is old news
	ttlAdminAlert   = 86400 * time.Second // security events wait a day for an offline device
	ttlTest         = 60 * time.Second
)

const (
	// maxPayloadBytes: payloads stay under 1 KB. webpush-go pads every message to one 4096-byte record, which
	// also hides its length from the push service.
	maxPayloadBytes = 1024
	// maxNameRunes cuts a name in a payload. Usernames have at most 32 characters (03) and room names are short,
	// so this only bounds what a caller that skipped those rules could send.
	maxNameRunes = 64
)

// sharePayload is the share.started payload: "Alex started sharing in Lounge", a link that focuses the share, and
// a tag per (room, sharer) so that a newer notification replaces the older one on the device.
func sharePayload(ev ShareStarted, ts time.Time) ([]byte, error) {
	q := url.Values{}
	q.Set("focus", ev.ShareID)
	return encodePayload(api.PushPayload{
		V:       api.PushPayloadVersion,
		Type:    api.PushTypeShareStarted,
		TS:      ts.UnixMilli(),
		Tag:     "share:" + ev.RoomID + ":" + ev.UserID,
		URL:     "/r/" + url.PathEscape(ev.RoomID) + "?" + q.Encode(),
		Room:    &api.NameRef{ID: ev.RoomID, Name: cutName(ev.RoomName)},
		User:    &api.NameRef{ID: ev.UserID, Name: cutName(ev.UserName)},
		ShareID: ev.ShareID,
	})
}

// alertPayload is the admin.alert payload. The link opens the admin page where the alert can be acted on.
func alertPayload(a AdminAlert, ts time.Time) ([]byte, error) {
	return encodePayload(api.PushPayload{
		V:      api.PushPayloadVersion,
		Type:   api.PushTypeAdminAlert,
		TS:     ts.UnixMilli(),
		Tag:    "admin:" + a.Kind,
		URL:    alertURL(api.AdminAlertKind(a.Kind)),
		Kind:   api.AdminAlertKind(a.Kind),
		Actor:  cutName(a.Actor),
		Target: cutName(a.Target),
	})
}

// testPayload is the push.test payload (POST /api/v1/push/test).
func testPayload(ts time.Time) ([]byte, error) {
	return encodePayload(api.PushPayload{
		V:    api.PushPayloadVersion,
		Type: api.PushTypeTest,
		TS:   ts.UnixMilli(),
		Tag:  "test",
		URL:  "/account/notifications",
	})
}

// alertURL is the SPA route (05 §5) an admin alert links to.
func alertURL(kind api.AdminAlertKind) string {
	switch kind {
	case api.AdminAlertKindSignupPending:
		return "/admin/approvals"
	case api.AdminAlertKindAdminGranted, api.AdminAlertKindAdminRevoked, api.AdminAlertKindAdminPasswordReset:
		return "/admin/users"
	case api.AdminAlertKindRegistrationModeChanged:
		return "/admin/settings"
	case api.AdminAlertKindSecretsRotated:
		return "/admin/audit"
	default: // transfer_threshold, and kinds a later version adds: the dashboard
		return "/admin"
	}
}

func encodePayload(p api.PushPayload) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("push: encoding a %s payload: %w", p.Type, err)
	}
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("push: a %s payload is %d bytes, over the limit of %d", p.Type, len(b), maxPayloadBytes)
	}
	return b, nil
}

// cutName cuts s to maxNameRunes characters.
func cutName(s string) string {
	if utf8.RuneCountInString(s) <= maxNameRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxNameRunes {
			return s[:i]
		}
		n++
	}
	return s
}
