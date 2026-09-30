package protocol

// Invalidate tells the SPA which REST resources (03) changed, so it can refetch them (01 §8.12).
type Invalidate struct {
	Topics []Topic `json:"topics"`
}

// Topic names a group of REST resources. The exact REST paths per topic are 03's; the SPA maps topics to its query
// keys (05). Clients ignore unknown topics.
type Topic string

const (
	TopicRooms          Topic = "rooms"           // room list                    → everyone
	TopicMe             Topic = "me"              // own account (name, admin)    → that user
	TopicDevices        Topic = "devices"         // the user's sessions/devices  → that user
	TopicAdminUsers     Topic = "admin.users"     // admins only
	TopicAdminInvites   Topic = "admin.invites"   // admins only
	TopicAdminApprovals Topic = "admin.approvals" // admins only
	TopicAdminSettings  Topic = "admin.settings"  // admins only
)

// Valid reports whether t is a known topic.
func (t Topic) Valid() bool {
	switch t {
	case TopicRooms, TopicMe, TopicDevices, TopicAdminUsers, TopicAdminInvites, TopicAdminApprovals,
		TopicAdminSettings:
		return true
	}
	return false
}

// ServerShutdown is sent to every connection before the server stops; error{server_shutdown} and close 1012 follow.
type ServerShutdown struct {
	Reason        ShutdownReason `json:"reason"`
	ReconnectInMs int            `json:"reconnectInMs"` // per connection, uniform random in [500, 3000]
}

// ShutdownReason says whether the server expects to come back.
type ShutdownReason string

const (
	ShutdownReasonRestart ShutdownReason = "restart" // M1 always sends this (SIGTERM can't tell restart from stop)
	ShutdownReasonStop    ShutdownReason = "stop"
)

// Valid reports whether r is a known reason.
func (r ShutdownReason) Valid() bool { return r == ShutdownReasonRestart || r == ShutdownReasonStop }
