package signal

import (
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// LiveSnapshot feeds the admin dashboard (03/04 expose it over REST).
type LiveSnapshot struct {
	Rooms []LiveRoom
}

// LiveRoom is one room with at least one participant. Name is the one the room's latest room.join read with GetRoom
// (Hub.Snapshot): it lags a rename, so readers that show it prefer the store's current name.
type LiveRoom struct {
	ID, Name     string
	Participants []LiveParticipant
	Shares       []LiveShare
}

// LiveParticipant is one user in a room, with all of its connections there.
type LiveParticipant struct {
	UserID, Name string
	Connections  []LiveConnection
}

// LiveConnection is one connection of a participant. OS and version are for admins only (01 §8.5, privacy).
type LiveConnection struct {
	ID        string
	Kind      protocol.ClientKind
	Role      protocol.Role
	OS        protocol.ClientOS
	Browser   string
	Version   string
	Status    protocol.ConnectionStatus
	Since     time.Time
	LastStats *protocol.ClientStats
}

// LiveShare is one share with its published layers.
type LiveShare struct {
	Info          protocol.ShareInfo
	Layers        []protocol.ServerLayerStats
	EgressBitrate int64 // sum over its subscribers (from MediaPeer.Stats)
}
