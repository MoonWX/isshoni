package protocol

import "slices"

// Direction is the direction of a message. It is a bit set so Lookup can ask for either.
type Direction uint8

const (
	DirClientToServer Direction = 1
	DirServerToClient Direction = 2
)

// MsgKind says whether a message is a request, a reply or a notification (01 §7).
type MsgKind uint8

const (
	KindRequest      MsgKind = 1 // has an id and gets exactly one reply
	KindReply        MsgKind = 2 // welcome, ok and error (an error without re is a notification)
	KindNotification MsgKind = 3 // no id, no reply
)

// Spec describes one message type (and direction); Registry is the single list used by the hub's dispatcher,
// the tests (every Spec has a fixture) and the TS generator.
type Spec struct {
	Type      MessageType
	Dir       Direction
	Kind      MsgKind
	Payload   any         // zero value of the payload type, e.g. RoomJoin{}; nil for ok (the request's Result applies)
	Reply     MessageType // requests: MessageTypeOK, or MessageTypeWelcome for hello
	Result    any         // requests: zero value of the reply payload (Empty{} if none)
	Roles     []Role      // client->server: allowed roles; nil = all (for pc.* the PC kind decides: Role.AllowsPC)
	Feature   Feature     // "" = baseline
	Since     int         // protocol version that introduced it
	Milestone string      // "M1", "M2", ... (docs)
}

// Allows reports whether role r may send this client->server message; Roles nil means every known role. For pc.*
// messages use Allowed, which also checks the PC kind.
func (s Spec) Allows(r Role) bool {
	if s.Roles == nil {
		return r.Valid()
	}
	return slices.Contains(s.Roles, r)
}

var (
	rolesPublish   = []Role{RoleFull, RolePublisher, RoleAgent}
	rolesSubscribe = []Role{RoleFull, RoleViewer}
)

// Registry lists every message type per direction (01 §7). "stats" has one entry per direction with different
// payloads; pc.offer, pc.answer, pc.ice and pc.restart have one entry per direction with the same payload.
var Registry = []Spec{
	// client -> server
	{Type: MessageTypeHello, Dir: DirClientToServer, Kind: KindRequest, Payload: Hello{}, Reply: MessageTypeWelcome, Result: Welcome{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePing, Dir: DirClientToServer, Kind: KindNotification, Payload: Ping{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeRoomJoin, Dir: DirClientToServer, Kind: KindRequest, Payload: RoomJoin{}, Reply: MessageTypeOK, Result: RoomJoinResult{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeRoomLeave, Dir: DirClientToServer, Kind: KindRequest, Payload: Empty{}, Reply: MessageTypeOK, Result: Empty{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeShareStart, Dir: DirClientToServer, Kind: KindRequest, Payload: ShareStart{}, Reply: MessageTypeOK, Result: ShareParams{}, Roles: rolesPublish, Since: 1, Milestone: "M1"},
	{Type: MessageTypeShareUpdate, Dir: DirClientToServer, Kind: KindRequest, Payload: ShareUpdate{}, Reply: MessageTypeOK, Result: ShareParams{}, Roles: rolesPublish, Since: 1, Milestone: "M1"},
	{Type: MessageTypeShareStop, Dir: DirClientToServer, Kind: KindRequest, Payload: ShareStop{}, Reply: MessageTypeOK, Result: Empty{}, Roles: rolesPublish, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCOffer, Dir: DirClientToServer, Kind: KindNotification, Payload: PCOffer{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCAnswer, Dir: DirClientToServer, Kind: KindNotification, Payload: PCAnswer{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCICE, Dir: DirClientToServer, Kind: KindNotification, Payload: PCICE{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCRestart, Dir: DirClientToServer, Kind: KindNotification, Payload: PCRestart{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCClose, Dir: DirClientToServer, Kind: KindNotification, Payload: PCClose{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeSubscribeUpdate, Dir: DirClientToServer, Kind: KindRequest, Payload: SubscribeUpdate{}, Reply: MessageTypeOK, Result: SubscribeResult{}, Roles: rolesSubscribe, Since: 1, Milestone: "M1"},
	{Type: MessageTypeCapsUpdate, Dir: DirClientToServer, Kind: KindNotification, Payload: CapsUpdate{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeStats, Dir: DirClientToServer, Kind: KindNotification, Payload: ClientStats{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeStatsWatch, Dir: DirClientToServer, Kind: KindRequest, Payload: StatsWatch{}, Reply: MessageTypeOK, Result: Empty{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeAgentSend, Dir: DirClientToServer, Kind: KindRequest, Payload: AgentSend{}, Reply: MessageTypeOK, Result: AgentSendResult{}, Feature: FeatureAgentRelay, Since: 1, Milestone: "M1"},

	// server -> client
	{Type: MessageTypeWelcome, Dir: DirServerToClient, Kind: KindReply, Payload: Welcome{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeOK, Dir: DirServerToClient, Kind: KindReply, Payload: nil, Since: 1, Milestone: "M1"},
	{Type: MessageTypeError, Dir: DirServerToClient, Kind: KindReply, Payload: Error{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePong, Dir: DirServerToClient, Kind: KindNotification, Payload: Pong{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeRoomState, Dir: DirServerToClient, Kind: KindNotification, Payload: RoomState{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeRoomEvent, Dir: DirServerToClient, Kind: KindNotification, Payload: RoomEvent{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCOffer, Dir: DirServerToClient, Kind: KindNotification, Payload: PCOffer{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCAnswer, Dir: DirServerToClient, Kind: KindNotification, Payload: PCAnswer{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCICE, Dir: DirServerToClient, Kind: KindNotification, Payload: PCICE{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypePCRestart, Dir: DirServerToClient, Kind: KindNotification, Payload: PCRestart{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeSubscribeStatus, Dir: DirServerToClient, Kind: KindNotification, Payload: SubscribeStatus{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeQualityHint, Dir: DirServerToClient, Kind: KindNotification, Payload: QualityHint{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeStats, Dir: DirServerToClient, Kind: KindNotification, Payload: ServerStats{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeInvalidate, Dir: DirServerToClient, Kind: KindNotification, Payload: Invalidate{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeServerShutdown, Dir: DirServerToClient, Kind: KindNotification, Payload: ServerShutdown{}, Since: 1, Milestone: "M1"},
	{Type: MessageTypeAgentRecv, Dir: DirServerToClient, Kind: KindNotification, Payload: AgentRecv{}, Feature: FeatureAgentRelay, Since: 1, Milestone: "M1"},
	{Type: MessageTypeUserConnections, Dir: DirServerToClient, Kind: KindNotification, Payload: UserConnections{}, Feature: FeatureUserConnections, Since: 1, Milestone: "M2"},
}

var registryIndex = indexRegistry(Registry)

func indexRegistry(specs []Spec) map[MessageType][]int {
	idx := make(map[MessageType][]int, len(specs))
	for i, s := range specs {
		idx[s.Type] = append(idx[s.Type], i)
	}
	return idx
}

// Lookup returns the Spec of message type t in direction d (DirClientToServer, DirServerToClient, or both to take
// the first match). ok is false for unknown types: the hub answers unknown_type if the message had an id and
// ignores it otherwise; clients ignore it.
func Lookup(t MessageType, d Direction) (Spec, bool) {
	for _, i := range registryIndex[t] {
		if Registry[i].Dir&d != 0 {
			return Registry[i], true
		}
	}
	return Spec{}, false
}

// Allowed reports whether role r may send the client->server message t (01 §6.3). pc is the PeerConnection a pc.*
// message is about (its pc field); it is ignored for other types. Unknown types are not allowed.
func Allowed(r Role, t MessageType, pc PCKind) bool {
	s, ok := Lookup(t, DirClientToServer)
	if !ok || !s.Allows(r) {
		return false
	}
	switch t {
	case MessageTypePCOffer, MessageTypePCAnswer, MessageTypePCICE, MessageTypePCRestart, MessageTypePCClose:
		return r.AllowsPC(pc)
	}
	return true
}

// MaxMessageSize returns the largest frame the server accepts for message type t after welcome (01 §3.3, §13):
// MaxSDPBytes for pc.offer and pc.answer, MaxStatsBytes for stats, MaxMessageBytes for everything else.
// Before welcome every frame is limited to MaxMessageBytes.
func MaxMessageSize(t MessageType) int {
	switch t {
	case MessageTypePCOffer, MessageTypePCAnswer:
		return MaxSDPBytes
	case MessageTypeStats:
		return MaxStatsBytes
	}
	return MaxMessageBytes
}
