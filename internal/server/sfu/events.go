package sfu

// This file is the SFU → signal half of the interface (02 §6.2). 01's sfuplane implements both interfaces and is
// the only code that turns these calls into wire messages (01 §15.4).

// Signaler carries what the SFU sends to one Conn's client. 01's sfuplane peer implements it, one per Conn, and it
// stays the same across WebSocket resumes.
//
// Implementations must not block (enqueue or drop) and must not call the SFU synchronously: the Conn's actor makes
// these calls. While the WebSocket is down signal may drop them; Conn.Resync re-sends what matters. Pub answers are
// not here: Conn.HandleOffer returns them.
type Signaler interface {
	// SendOffer sends a sub offer (PCSub in M1) with its tracks binding: one entry per m-section that carries a share.
	SendOffer(pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding)
	SendEvent(ev Event)
}

// RoomEvents carries the media facts of a room's shares to the hub (02 §5.3). Implementations must not block and
// must not call the SFU synchronously; calls come from Conn actors, from the SFU's ticker and from the callers of
// StopShare, CloseRoom, Conn.Close and SFU.Close.
type RoomEvents interface {
	// ShareUpdated reports a state change, or a layer, profile or audio change while live. It is debounced to at most
	// one per share per 250 ms; state changes are never merged away.
	ShareUpdated(room RoomID, s ShareInfo)
	// ShareEnded reports that a share was removed, once per share, with the reason the caller gave.
	ShareEnded(room RoomID, s ShareInfo, r EndReason)
	CodecPolicyChanged(room RoomID, p ProfileKey)
}

// Event is something the SFU tells one Conn's client through Signaler.SendEvent: SubscriptionStateEvent,
// CodecPolicyEvent, QualityHintEvent, PCStateEvent or ErrorEvent.
type Event interface{ isEvent() }

// SubscriptionStateEvent goes to the subscriber, only when Forwarded, Audio or Reason changes: not for a request
// alone, and not for a new subscription that forwards nothing yet without a reason (02 §10.1). A switch to another
// layer is reported when that layer's keyframe has arrived, a pause at once.
type SubscriptionStateEvent struct {
	Share     ShareID
	Requested Quality // the client's latest request
	// Forwarded is what the SFU actually sends: high for the full layer, low for the preview layer, off for none.
	// For the moment a switch down takes (the old layer flows until the new one's keyframe), it is above Requested.
	Forwarded Quality
	Audio     bool // audio actually forwarded: the client asked for it and the share's audio flows
	// Reason says why Forwarded < Requested. It is "" when they are equal, or while Forwarded < Requested only
	// because the sub PC isn't connected yet or the target layer's keyframe hasn't arrived (01 maps that to waiting).
	// A request for high that gets the preview layer because the share has no fuller one is Forwarded low with
	// SubReasonNoLayer, as is one that gets nothing because the share has no video layer yet.
	Reason SubReason
}

// CodecPolicyEvent goes to the Conn that publishes Share: re-offer with this profile first (02 §8.3).
type CodecPolicyEvent struct {
	Share   ShareID
	Profile ProfileKey
}

// QualityHintEvent goes to the publishing Conn (02 §11).
type QualityHintEvent struct {
	Share      ShareID
	Reason     string // "admin" | "viewers"
	MaxBitrate int    // bps; 0 = no cap. M1: the admin cap. M2: the native uplink estimator
	// Encodings is the full current list, in the shape StartShare returns, with the admin cap and the pause state
	// applied.
	Encodings []EncodingParams
}

// PCStateEvent reports a state of the Conn's current PC of a kind; never of one a higher gen already replaced.
type PCStateEvent struct {
	PC     PCKind
	Gen    uint32 // generation of the PC this state belongs to
	State  string // "connected" "disconnected" "failed" "closed"
	Reason string // "" | "handshake_timeout" | "pc_grace_expired"
}

// ErrorEvent reports a failure that no call returns, for example a sub offer that couldn't be created.
type ErrorEvent struct {
	Err   *Error
	Scope string // "pc.pub" | "pc.sub" | "share" | "subscription"; sfuplane maps it to 01's scope and pc fields
}

// ErrorEvent scopes.
const (
	ScopePCPub        = "pc.pub"
	ScopePCSub        = "pc.sub"
	ScopeShare        = "share"
	ScopeSubscription = "subscription"
)

func (SubscriptionStateEvent) isEvent() {}
func (CodecPolicyEvent) isEvent()       {}
func (QualityHintEvent) isEvent()       {}
func (PCStateEvent) isEvent()           {}
func (ErrorEvent) isEvent()             {}

// noRoomEvents drops every event: the RoomEvents of an SFU built without Deps.Events.
type noRoomEvents struct{}

func (noRoomEvents) ShareUpdated(RoomID, ShareInfo)          {}
func (noRoomEvents) ShareEnded(RoomID, ShareInfo, EndReason) {}
func (noRoomEvents) CodecPolicyChanged(RoomID, ProfileKey)   {}
