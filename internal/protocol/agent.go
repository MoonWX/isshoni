package protocol

import "encoding/json"

// AgentSend relays an opaque payload to other connections of the same user (01 §8.14, feature agent.relay). Only the
// transport is defined in v1; the server never parses Payload.
//
// Reserved kinds: share.request, share.status (M4); playback.stop, exclusions.get, exclusions.set (M2).
type AgentSend struct {
	To      string          `json:"to,omitempty"`     // a connectionId of the same user, or
	ToRole  Role            `json:"toRole,omitempty"` // all the user's connections with this role (exactly one of To/ToRole)
	Kind    string          `json:"kind"`             // [a-z.]{1,32}; registry in 04/M2 docs
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

// UserConnections (later M2, feature user.connections): all of the user's connections, sent to each of them on change.
type UserConnections struct {
	Connections []OwnConnection `json:"connections"`
}

// OwnConnection is one of the user's own connections, with the details other users never see. The embedded
// ConnectionInfo's fields are inlined in JSON; the tstype tag makes tygo emit "extends ConnectionInfo" to match.
type OwnConnection struct {
	ConnectionInfo `tstype:",extends"`
	OS             ClientOS `json:"os"`
	Version        string   `json:"version"`
	RoomID         string   `json:"roomId,omitempty"`
}
