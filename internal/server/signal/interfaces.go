package signal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// This file declares every interface the hub depends on (01 §15.2). The wiring (04 §6.6) implements them with
// adapters over 03's auth and store, 04's push service and 01's sfuplane; package signaltest has fakes of each.

// Identity is who a connection authenticated as (01 §3.2). UserID, SessionID and DeviceID are fixed for the
// connection's lifetime; Name and Admin follow UpdateUser and Revalidate.
type Identity struct {
	UserID    string
	Name      string // the username in M1; never logged
	Admin     bool
	SessionID string // cookie sessions
	DeviceID  string // bearer (native apps, M2)
}

// LogValue implements slog.LogValuer: the user id and the admin flag only. Usernames are never logged (README
// "Logging"), and the session and device ids stay out of the log as well.
func (id Identity) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user_id", id.UserID), slog.Bool("admin", id.Admin))
}

// Authenticator is implemented by a wiring adapter over 03's auth.Service (04 §6.6):
// AuthenticateRequest → auth.AuthenticateCookie(r) (session cookie only; Authorization headers are ignored on /ws),
// Revalidate → auth.Touch + a user re-read, AuthenticateBearer → M2 device tokens (M1: always ErrInvalid).
type Authenticator interface {
	// AuthenticateRequest reads the session cookie. ErrNoCredentials if absent; ErrInvalid if bad or expired;
	// other errors are transient (the hub answers 503).
	AuthenticateRequest(r *http.Request) (Identity, error)
	// AuthenticateBearer validates a device access token (device flow, 03, M2).
	AuthenticateBearer(ctx context.Context, token protocol.Secret) (Identity, error)
	// Revalidate reports whether the session/device behind id is still valid and marks it as seen (03 Touch);
	// the result can update Name/Admin. Called at connect and every RevalidateEvery.
	// Returns ErrInvalid only when the session/device is gone or expired or the user is not active (the hub then
	// closes with session_revoked). Any other error is transient: the hub keeps the connection and retries at the
	// next RevalidateEvery.
	Revalidate(ctx context.Context, id Identity, ip netip.Addr) (Identity, error)
}

// Errors of the dependency interfaces. Adapters return them (wrapped or not); the hub tests with errors.Is.
var (
	ErrNoCredentials = errors.New("signal: no credentials")
	ErrInvalid       = errors.New("signal: invalid credentials")
	ErrNotFound      = errors.New("signal: not found")
)

// RoomDirectory is implemented by a wiring adapter over 03's store (RoomByID; DefaultRoomID = store.DefaultRoomID).
type RoomDirectory interface {
	GetRoom(ctx context.Context, roomID string) (protocol.RoomInfo, error) // ErrNotFound
	DefaultRoomID(ctx context.Context) (string, error)
	CanJoin(ctx context.Context, id Identity, roomID string) error // M1: always nil; room locks later
}

// PushNotifier is implemented by internal/server/push (04), through a wiring adapter. Non-blocking.
// The hub calls it once per share, on starting → live, unless the share has `replaces`.
type PushNotifier interface {
	ShareStarted(ctx context.Context, ev PushShareStarted)
}

// PushShareStarted is the event behind the "Alex started sharing" Web Push notification (04 §14).
type PushShareStarted struct {
	RoomID string
	// RoomName is read with GetRoom when the event is built; the hub never caches room names, so renames (03 §8)
	// need no hook.
	RoomName         string
	ShareID          string
	UserID, UserName string
	PresentUserIDs   []string // participants in the room now (present or reconnecting): 04 skips them
	At               time.Time
}

// MediaPlane is the SFU as seen from signaling. Implemented by internal/server/sfuplane over 02's *sfu.SFU (§15.4).
type MediaPlane interface {
	// NewPeer creates the media side of one connection in one room. sink gets that peer's events.
	NewPeer(p PeerParams, sink MediaSink) (MediaPeer, error)
}

// PeerParams describes the connection a MediaPeer serves.
type PeerParams struct {
	ConnectionID string
	UserID       string
	RoomID       string
	Role         protocol.Role
	Caps         protocol.Caps
	Client       protocol.ClientInfo
}

// MediaPeer is called only from the connection's actor goroutine, never concurrently for one peer.
type MediaPeer interface {
	CreateShare(shareID string, meta protocol.ShareStart) (protocol.ShareParams, error)
	UpdateShare(shareID string, meta protocol.ShareUpdate) (protocol.ShareParams, error)
	EndShare(shareID string, reason protocol.EndReason)
	HandleOffer(o protocol.PCOffer) (protocol.PCAnswer, error) // pub PC; errors are *protocol.Error (scope pc; §15.4)
	HandleAnswer(a protocol.PCAnswer) error                    // sub PC
	AddICE(c protocol.PCICE) error
	Restart(r protocol.PCRestart) error // sub: ICE restart or rebuild (client asked)
	ClosePC(c protocol.PCClose) error   // pub only (the hub ignores pc.close{sub})
	Subscribe(wants []protocol.SubscriptionWant) (ignored []string, err error)
	SetCaps(protocol.Caps)
	Resync() // §10.5: re-emit pending sub offer, statuses, hints; ICE-restart non-connected sub PC
	Stats() protocol.ServerStats
	Close() // closes both PCs; the hub has already ended the peer's shares
}

// MediaSink receives a peer's events. Implementations never block, and they copy whatever they keep of a call's
// arguments before returning, so the caller may reuse its slices, maps and pointers once a call returns.
type MediaSink interface {
	Offer(o protocol.PCOffer) // sub PC offers
	ICE(c protocol.PCICE)
	RestartRequest(r protocol.PCRestart) // ask the client to restart/rebuild its pub PC
	SubscriptionStatus(s []protocol.SubscriptionStatus)
	QualityHint(h protocol.QualityHint)
	ShareMedia(shareID string, ev ShareMediaEvent)
	Error(e protocol.Error) // scope pc or share
}

// ShareMediaEvent is a media fact about one share, reported by the SFU (01 §4.4). The hub owns the share lifecycle
// and turns these facts into status changes and timeouts.
type ShareMediaEvent struct {
	Kind   ShareMediaKind
	Layers []protocol.VideoLayer // layers currently received
	Codec  protocol.CodecKey
	Audio  bool // an audio track is being received
}

// ShareMediaKind says what happened to a share's media.
type ShareMediaKind uint8

const (
	ShareMediaLive    ShareMediaKind = iota + 1 // first keyframe, or recovered after stalled
	ShareMediaStalled                           // pub PC not connected for 2 s, or its tracks are gone (PC rebuilt)
	ShareMediaChanged                           // layers/codec/audio changed while live
	ShareMediaGone                              // reserved: the hub itself ends a share whose tracks a pub offer of
	// any gen no longer lists (§8.7); the SFU never emits it in M1
)

// String returns the kind's name, for logs.
func (k ShareMediaKind) String() string {
	switch k {
	case ShareMediaLive:
		return "live"
	case ShareMediaStalled:
		return "stalled"
	case ShareMediaChanged:
		return "changed"
	case ShareMediaGone:
		return "gone"
	}
	return "unknown"
}
