package winos

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"slices"
	"strings"
)

// Action Center (03 §15): a toast that is no longer relevant is removed from it, so its buttons can't be pressed
// after their question was answered another way (the tray, the panel, a later stop, the share's end; README WA68's
// R3). The app's toast book already ignores such a press (its nonce is settled); removing the toast is what the user
// sees.
//
// The shell shows its toasts through Wails' notifications service, which at beta.34 gives them no tag, and Windows
// removes a single toast from Action Center only by its tag and group (ToastNotificationHistory.Remove). So
// RemoveDeliveredToasts removes a tagged toast by its tag and, when an untagged toast is to go, clears the app's
// toasts (ClearWithId) and delivers the others again straight into Action Center, without a banner or a sound
// (SuppressPopup), with their own XML (so their buttons still activate the app through Wails' activator, which is
// registered per AppUserModelID) and a tag of their own, so the next removal is a single one. Which toast is which
// comes from its launch attribute: Wails' payload carries the app.Notification's ID and nonce (decodeWailsLaunch).

// DeliveredToast is one of an app's toasts in Action Center (ToastNotificationHistory.GetHistory).
type DeliveredToast struct {
	Tag    string // "" for a toast shown without one (Wails' at beta.34)
	Group  string
	Launch string // the <toast> element's launch attribute
	// ID and Nonce are the app.Notification's (cmd/isshoni-desktop's NotificationOptions.ID and its data's "nonce"),
	// decoded from Wails' launch payload; "" when Launch isn't one.
	ID    string
	Nonce string
}

// Toast is a toast for ShowToast: its XML (the toast schema's <toast> document), its tag and group in Action Center,
// and SuppressPopup to place it there without a banner.
type Toast struct {
	XML           string
	Tag           string // at most 64 characters (16 before Windows 10 1703)
	Group         string
	SuppressPopup bool
}

// RemoveToasts removes the app's toasts (AppUserModelID InstanceID) whose app.Notification ID is one of ids from
// Action Center, and reports how many went. A toast that isn't there (dismissed, or never shown) is no error.
// The app calls it when a toast's question was answered another way (toastBook.dropKind); off Windows it does
// nothing.
func (h *Hooks) RemoveToasts(ids ...string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return h.removeToasts(InstanceID, func(t DeliveredToast) bool { return t.ID != "" && slices.Contains(ids, t.ID) })
}

// wailsLaunch is Wails' launch payload at beta.34 (pkg/services/notifications, NotificationPayload): base64 of this
// JSON.
type wailsLaunch struct {
	Action  string `json:"action"`
	Payload struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	} `json:"payload"`
}

// nonceKey is the key of the Wails toast data that carries app.Notification.Nonce (cmd/isshoni-desktop's).
const nonceKey = "nonce"

// decodeWailsLaunch is the app.Notification ID and nonce in Wails' launch payload; "" and "" for anything else.
func decodeWailsLaunch(launch string) (id, nonce string) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(launch))
	if err != nil {
		return "", ""
	}
	var p wailsLaunch
	if json.Unmarshal(b, &p) != nil {
		return "", ""
	}
	nonce, _ = p.Payload.Data[nonceKey].(string)
	return p.Payload.ID, nonce
}

// toastLaunch is the launch attribute of a toast's XML ("" when it has none or isn't a toast document).
func toastLaunch(doc string) string {
	var t struct {
		XMLName xml.Name `xml:"toast"`
		Launch  string   `xml:"launch,attr"`
	}
	if xml.Unmarshal([]byte(doc), &t) != nil {
		return ""
	}
	return t.Launch
}

// deliveredToast builds a DeliveredToast from what Action Center holds.
func deliveredToast(tag, group, doc string) DeliveredToast {
	t := DeliveredToast{Tag: tag, Group: group, Launch: toastLaunch(doc)}
	t.ID, t.Nonce = decodeWailsLaunch(t.Launch)
	return t
}
