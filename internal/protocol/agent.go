package protocol

import "encoding/json"

// AgentSend relays an opaque payload to other connections of the same user (01 §8.14, feature agent.relay). Only the
// transport is defined in v1; the server never parses Payload.
//
// The kinds and their payloads are registered in internal/protocol/relay (M2 01 §11.11): playback.stop and
// playback.state (M2); share.request and share.status (reserved, M4); exclusions.get and exclusions.set (reserved,
// M4's agent spec declares their payloads). Receivers ignore kinds they don't know.
type AgentSend struct {
	To      string          `json:"to,omitempty"`     // a connectionId of the same user, or
	ToRole  Role            `json:"toRole,omitempty"` // all the user's connections with this role (exactly one of To/ToRole)
	Kind    string          `json:"kind"`             // [a-z.]{1,32}; registry: internal/protocol/relay
	Payload json.RawMessage `json:"payload"`          // any JSON value but null, <= 16 KiB; opaque to the server
}

// AgentSendResult is the ok payload of agent.send.
type AgentSendResult struct {
	Delivered int `json:"delivered"`
}

// AgentRecv delivers a relayed payload.
type AgentRecv struct {
	From    string          `json:"from"` // the sender's connectionId
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"` // the agent.send's payload: never null
}

// UserConnections (M2, feature user.connections): all of the user's connections, sorted by connection id. The hub
// sends it to each of the user's online connections that has the feature active (M2 01 §10.4): after its welcome,
// and whenever one of the user's connections opens, closes, detaches, re-attaches or changes room, or a
// revalidation changed a connection's DeviceName; at most one per 200 ms per user. Only the user's own connections
// ever see it (01 §17: OS and version stay private).
type UserConnections struct {
	Connections []OwnConnection `json:"connections"`
}

// OwnConnection is one of the user's own connections, with the details other users never see. The embedded
// ConnectionInfo's fields are inlined in JSON; the tstype tag makes tygo emit "extends ConnectionInfo" to match.
// The list holds every connection of the user, the recipient included; a detached one has status reconnecting.
//
// DeviceName (M2) is the name the user gave the device of a bearer connection (02's signal.Identity.DeviceName),
// absent for cookie connections. It may lag a rename by up to 5 minutes (the hub's revalidation interval). It is
// user content: never logged.
type OwnConnection struct {
	ConnectionInfo `tstype:",extends"`
	OS             ClientOS `json:"os"`
	Version        string   `json:"version"`
	RoomID         string   `json:"roomId,omitempty"`
	DeviceName     string   `json:"deviceName,omitempty"` // M2: the device's name for a bearer connection; may lag a rename by <= 5 min
}
