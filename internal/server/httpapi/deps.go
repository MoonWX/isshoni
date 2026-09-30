package httpapi

import (
	"context"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// The interfaces through which 03's API reaches 01's hub, 04's push service and 04's build information (03 §12.5).
// httpapi never imports signal or push (04 §2): the wiring (internal/server/wire.go, 04 §6.6) adapts them.

// Signal is implemented by the wiring over 01's hub (04 §6.6). Deps.Signal may be nil (tests): no presence, and the
// hooks do nothing.
type Signal interface {
	// RoomPresence returns the participants and shares of every room with someone in it, from Hub.Snapshot().
	RoomPresence() map[store.RoomID]RoomPresence
	// OnlineUserIDs returns the users with at least one connection, from Hub.Snapshot().
	OnlineUserIDs() map[store.UserID]struct{}
	// RoomDeleted is Hub.CloseRoom: room_closed to that room's connections.
	RoomDeleted(id store.RoomID)
	// UserChanged is Hub.UpdateUser: refresh the user's name and role on its connections.
	UserChanged(id store.UserID, username string, admin bool)
	// Notify is Hub.Notify: the target's SPAs refetch the topics (01 §8.12). REST handlers call it after the commit,
	// following the topic table of 03 §12.5.
	Notify(t NotifyTarget, topics ...protocol.Topic)
}

// RoomPresence is a room's live counts (the live object of a room list, api.RoomPresence).
type RoomPresence struct{ Participants, Shares int }

// NotifyTarget mirrors 01's signal.Target: the connections a Notify reaches.
type NotifyTarget struct {
	UserID store.UserID // "" = not by user
	Admins bool         // every admin's connections
	All    bool         // every connection
}

// Push is implemented by the wiring's adapter over 04's push service (it converts store.PushSubscription to
// push.Subscription). Deps.Push is nil when push is off (push.enabled = false, 04 §6.6): the push endpoints then
// answer 503 push_unavailable and GET /info omits push.
type Push interface {
	// VAPIDPublicKey is the server's VAPID public key: base64url, an uncompressed P-256 point.
	VAPIDPublicKey() string
	// ValidateEndpoint checks a subscription endpoint (every URL and host rule of 03 §12.4.6, including the DNS
	// check): nil, or *api.Error{push_endpoint_rejected} with params.reason.
	ValidateEndpoint(ctx context.Context, endpoint string) error
	// SendTest sends the test notification to the given subscriptions.
	SendTest(ctx context.Context, subs []store.PushSubscription) error
}

// InfoSource is implemented by 04 over internal/version and internal/protocol (04 §6.6). Deps.Info may be nil: the
// API then reads the same two packages itself.
type InfoSource interface {
	ServerVersion() string        // SemVer of this build
	Protocol() (current, min int) // from internal/protocol (01)
}

// nopSignal is the Signal of a nil Deps.Signal: nobody is online and the hooks do nothing.
type nopSignal struct{}

func (nopSignal) RoomPresence() map[store.RoomID]RoomPresence { return map[store.RoomID]RoomPresence{} }
func (nopSignal) OnlineUserIDs() map[store.UserID]struct{}    { return map[store.UserID]struct{}{} }
func (nopSignal) RoomDeleted(store.RoomID)                    {}
func (nopSignal) UserChanged(store.UserID, string, bool)      {}
func (nopSignal) Notify(NotifyTarget, ...protocol.Topic)      {}

// buildInfo is the InfoSource of a nil Deps.Info: this binary's version and protocol range.
type buildInfo struct{}

func (buildInfo) ServerVersion() string { return version.Version() }

func (buildInfo) Protocol() (current, minimum int) { return protocol.Version, protocol.MinVersion }
