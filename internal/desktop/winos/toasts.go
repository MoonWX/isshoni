package winos

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
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
//
// The clear path can lose a toast that Wails delivers during it (RemoveToasts' contract). The lasting fix is for the
// shell to show the app's toasts with a tag equal to their app.Notification ID (cmd/isshoni-desktop building Wails'
// XML and calling ShowToast); then every removal is a single tagged one and the clear path goes away.

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
	Group         string // a tagged toast needs one: an unpackaged app removes a toast by its tag and group
	SuppressPopup bool
}

// RemoveToasts removes the app's toasts (AppUserModelID InstanceID) whose app.Notification ID is one of ids from
// Action Center, and reports how many went. A toast that isn't there (dismissed, or never shown) is no error.
// The app calls it when a toast's question was answered another way (the IDs toastBook.dropKind drops); off Windows
// it does nothing.
//
// Windows removes only the caller's own toasts: the process's AppUserModelID must be InstanceID
// (SetAppUserModelID, which cmd/isshoni-desktop calls first thing), or Windows may reject the removal (E_INVALIDARG).
//
// Contract for callers: callers must serialize RemoveToasts with Shell.Notify and must not call it under App.mu.
// Removing an untagged toast clears the app's toasts and delivers the others again (removeDelivered); a toast the
// shell shows through Wails meanwhile doesn't take this package's lock and may be lost, so the removal must finish
// before the next Notify starts (one ordered toast goroutine or queue). It makes several WinRT round trips, which is
// why it must not run with App.mu held.
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

// deliveredItem is one toast in Action Center with its XML.
type deliveredItem struct {
	DeliveredToast
	xml string
}

// toastStore is one app's Action Center as removeDelivered uses it (actionCenter on Windows; a fake in the tests).
type toastStore interface {
	list() ([]deliveredItem, error)
	remove(tag, group string) error // ToastNotificationHistory.Remove(tag, group, appID)
	clear() error                   // ToastNotificationHistory.ClearWithId(appID)
	show(t Toast) error
}

// removeDelivered removes the store's toasts that match and reports how many went: a tagged one by its tag and
// group; when an untagged one is among them, every toast of the app is cleared and the ones that don't match are
// delivered again with their own XML, without a banner, each with a tag of its own (keepTag) and a group (keepGroup
// when it had none) so that the next removal of one is a single one.
//
// The store is listed again just before the clear, and what is kept comes from that second list: a toast delivered
// since the first list is kept (or removed, when it matches), and one the user dismissed meanwhile isn't brought
// back. The window between the second list and the clear stays; RemoveToasts' contract closes it for the app's own
// toasts. The count includes untagged toasts only once the clear succeeded; when a kept toast can't be delivered
// again it is lost, and the error says how many were.
func removeDelivered(c toastStore, match func(DeliveredToast) bool) (int, error) {
	items, err := c.list()
	if err != nil {
		return 0, err
	}
	removed := 0
	untagged := false
	for _, it := range items {
		switch {
		case !match(it.DeliveredToast):
		case it.Tag == "":
			untagged = true
		default:
			if err := c.remove(it.Tag, it.Group); err != nil {
				return removed, err
			}
			removed++
		}
	}
	if !untagged {
		return removed, nil
	}
	items, err = c.list()
	if err != nil {
		return removed, err
	}
	var keep []deliveredItem
	pendingUntagged := 0
	for _, it := range items {
		if match(it.DeliveredToast) {
			pendingUntagged++ // the clear takes it, tagged or not
		} else {
			keep = append(keep, it)
		}
	}
	if pendingUntagged == 0 { // dismissed meanwhile
		return removed, nil
	}
	if err := c.clear(); err != nil {
		return removed, err
	}
	removed += pendingUntagged
	var errs []error
	for _, it := range keep {
		tag, group := it.Tag, it.Group
		if tag == "" {
			tag = keepTag(it.xml)
		}
		if group == "" {
			group = keepGroup
		}
		if err := c.show(Toast{XML: it.xml, Tag: tag, Group: group, SuppressPopup: true}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("winos: %d of %d kept toasts lost: %w", len(errs), len(keep), errors.Join(errs...))
	}
	return removed, nil
}

// keepTag is the tag a toast delivered again gets: 16 hex characters of its XML's SHA-256 (16 is the tag's limit
// before Windows 10 1703).
func keepTag(doc string) string {
	sum := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(sum[:8])
}

// keepGroup is the group a toast delivered again gets when it had none (Wails' toasts): an unpackaged app can't
// remove a tagged toast without a group (the Windows Community Toolkit's ToastNotifierCompat gives every tagged toast
// one for this reason).
const keepGroup = "isshoni"
