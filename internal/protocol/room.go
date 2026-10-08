package protocol

import "time"

// RoomJoin asks to join a room (01 §8.4). A malformed roomId gets room_not_found, like 03's REST 404.
type RoomJoin struct {
	RoomID string `json:"roomId"`
}

// RoomJoinResult is the ok payload of room.join. A room.state for the room follows immediately.
type RoomJoinResult struct {
	Room RoomInfo `json:"room"`
}

// RoomInfo identifies a room.
type RoomInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RoomState is a full snapshot of one room (01 §8.5), coalesced to at most one per 200 ms per room. Every connection
// in the room gets byte-identical snapshots.
type RoomState struct {
	RoomID       string            `json:"roomId"`
	Rev          uint64            `json:"rev"`          // grows with every change in this server process; clients reset on every welcome
	Participants []ParticipantInfo `json:"participants"` // sorted by joinedAt, then userId
	Shares       []ShareInfo       `json:"shares"`       // sorted by startedAt, then id
}

// ParticipantInfo merges all connections of one user in one room.
type ParticipantInfo struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	// Admin says that the user is an admin (03's role), for the people panel's badge. It follows a role change like
	// Name follows a rename. Absent means a member, as in welcome.user; a server from before the field never sends
	// it (additive, 01 §14.1).
	Admin       bool              `json:"admin,omitempty"`
	Status      ParticipantStatus `json:"status"`
	JoinedAt    time.Time         `json:"joinedAt"`
	Connections []ConnectionInfo  `json:"connections"`
}

// ParticipantStatus is present while at least one connection is ready. Clients treat unknown values as present.
type ParticipantStatus string

const (
	ParticipantStatusPresent      ParticipantStatus = "present"
	ParticipantStatusReconnecting ParticipantStatus = "reconnecting"
)

// Valid reports whether s is a known status.
func (s ParticipantStatus) Valid() bool {
	return s == ParticipantStatusPresent || s == ParticipantStatusReconnecting
}

// ConnectionInfo is what everyone in the room sees about a connection. OS and version are deliberately absent
// (privacy); admins get them from the live snapshot (01 §15.2), the user's own devices from user.connections (M2).
//
// OwnConnection embeds it, so it must never get a MarshalJSON method (the embedding would hide OwnConnection's own
// fields).
type ConnectionInfo struct {
	ID     string           `json:"id"`
	Kind   ClientKind       `json:"kind"`
	Role   Role             `json:"role"`
	Status ConnectionStatus `json:"status"`
}

// ConnectionStatus is online while the connection's socket is attached. Clients treat unknown values as online.
type ConnectionStatus string

const (
	ConnectionStatusOnline       ConnectionStatus = "online"
	ConnectionStatusReconnecting ConnectionStatus = "reconnecting"
)

// Valid reports whether s is a known status.
func (s ConnectionStatus) Valid() bool {
	return s == ConnectionStatusOnline || s == ConnectionStatusReconnecting
}

// ShareInfo describes one share in room.state.
type ShareInfo struct {
	ID           string       `json:"id"`
	UserID       string       `json:"userId"`
	ConnectionID string       `json:"connectionId"` // the publishing connection
	Kind         ShareKind    `json:"kind"`
	Label        string       `json:"label,omitempty"` // absent: viewers render t('share.label.<kind>')
	Preset       Preset       `json:"preset"`
	Audio        bool         `json:"audio"` // declared at start; after live: whether an audio track arrived
	Status       ShareStatus  `json:"status"`
	Layers       []VideoLayer `json:"layers"`          // layers whose tracks arrived (paused layers included), e.g. ["high","low"]; [] while starting
	Codec        CodecKey     `json:"codec,omitempty"` // H.264 profile currently published
	StartedAt    time.Time    `json:"startedAt"`
	Replaces     string       `json:"replaces,omitempty"` // re-publish after a restart/rebuild (§10.6)
	Watchers     []Watcher    `json:"watchers"`           // excludes the owner (§4.1); "N watching" is len(watchers)
}

// Watcher is one participant receiving a share's video at any layer or its audio (01 §4.1), from the desired
// subscriptions, merged over the participant's connections.
type Watcher struct {
	UserID string     `json:"userId"`
	Video  VideoLayer `json:"video"`
	Audio  AudioState `json:"audio"`
}

// RoomEvent announces a discrete change for toasts and aria-live (01 §8.6). Events are never part of a snapshot and
// are not sent on join or resume; the UI renders from room.state.
type RoomEvent struct {
	Kind     RoomEventKind `json:"kind"`
	RoomID   string        `json:"roomId"`
	UserID   string        `json:"userId"`
	Name     string        `json:"name"` // so a toast works after the person left the snapshot
	ShareID  string        `json:"shareId,omitempty"`
	Replaces string        `json:"replaces,omitempty"` // share.started of a re-publish: clients suppress the toast
	Reason   EndReason     `json:"reason,omitempty"`   // share.stopped, participant.left
	At       time.Time     `json:"at"`
}

// RoomEventKind is the kind of a room.event. Clients ignore unknown kinds.
type RoomEventKind string

const (
	RoomEventKindParticipantJoined RoomEventKind = "participant.joined"
	RoomEventKindParticipantLeft   RoomEventKind = "participant.left"
	RoomEventKindShareStarted      RoomEventKind = "share.started" // on starting → live, once per share
	RoomEventKindShareStopped      RoomEventKind = "share.stopped"
)

// Valid reports whether k is a known kind.
func (k RoomEventKind) Valid() bool {
	switch k {
	case RoomEventKindParticipantJoined, RoomEventKindParticipantLeft, RoomEventKindShareStarted,
		RoomEventKindShareStopped:
		return true
	}
	return false
}

// EndReason says why a share ended or a participant left. The strings are identical in 02's sfu package. Clients
// show unknown reasons without a specific text.
type EndReason string

const (
	EndReasonStopped        EndReason = "stopped"         // share.stop, pc.close, or tracks removed by the owner
	EndReasonLeft           EndReason = "left"            // room.leave / room.join elsewhere / deliberate close / closed by revocation (03)
	EndReasonDisconnected   EndReason = "disconnected"    // grace expired
	EndReasonMediaTimeout   EndReason = "media_timeout"   // starting or stalled for 30 s
	EndReasonKicked         EndReason = "kicked"          // reserved: admin kick is later
	EndReasonRoomClosed     EndReason = "room_closed"     // an admin deleted the room
	EndReasonServerShutdown EndReason = "server_shutdown" // shares only; never seen by clients of the new process
)

// Valid reports whether r is a known reason.
func (r EndReason) Valid() bool {
	switch r {
	case EndReasonStopped, EndReasonLeft, EndReasonDisconnected, EndReasonMediaTimeout, EndReasonKicked,
		EndReasonRoomClosed, EndReasonServerShutdown:
		return true
	}
	return false
}
