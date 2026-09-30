package protocol

import (
	"reflect"
	"testing"
)

func TestRegistryShape(t *testing.T) {
	seen := map[MessageType]Direction{}
	for _, s := range Registry {
		if s.Dir != DirClientToServer && s.Dir != DirServerToClient {
			t.Errorf("%s: Dir %d is not exactly one direction", s.Type, s.Dir)
		}
		if seen[s.Type]&s.Dir != 0 {
			t.Errorf("%s: two entries for direction %d", s.Type, s.Dir)
		}
		seen[s.Type] |= s.Dir
		if s.Since != 1 || s.Since > Version {
			t.Errorf("%s: Since %d", s.Type, s.Since)
		}
		if s.Milestone == "" {
			t.Errorf("%s: no milestone", s.Type)
		}
		if s.Feature != "" && !s.Feature.Valid() {
			t.Errorf("%s: unknown feature %q", s.Type, s.Feature)
		}
		if s.Payload == nil && s.Type != MessageTypeOK {
			t.Errorf("%s: no payload", s.Type)
		}
		switch s.Kind {
		case KindRequest:
			if s.Dir != DirClientToServer {
				t.Errorf("%s: only clients send requests", s.Type)
			}
			wantReply := MessageTypeOK
			if s.Type == MessageTypeHello {
				wantReply = MessageTypeWelcome
			}
			if s.Reply != wantReply || s.Result == nil {
				t.Errorf("%s: reply %q, result %v", s.Type, s.Reply, s.Result)
			}
		case KindReply:
			if s.Dir != DirServerToClient || s.Reply != "" || s.Result != nil {
				t.Errorf("%s: a reply goes server to client and has no reply itself", s.Type)
			}
		case KindNotification:
			if s.Reply != "" || s.Result != nil {
				t.Errorf("%s: a notification has no reply", s.Type)
			}
		default:
			t.Errorf("%s: kind %d", s.Type, s.Kind)
		}
		if s.Dir == DirServerToClient && s.Roles != nil {
			t.Errorf("%s: roles only apply to client messages", s.Type)
		}
		for _, r := range s.Roles {
			if !r.Valid() {
				t.Errorf("%s: unknown role %q", s.Type, r)
			}
		}
	}
	// The hello result is Welcome, and the welcome entry carries the same type.
	h, _ := Lookup(MessageTypeHello, DirClientToServer)
	w, _ := Lookup(MessageTypeWelcome, DirServerToClient)
	if reflect.TypeOf(h.Result) != reflect.TypeOf(w.Payload) {
		t.Error("hello's Result is not welcome's Payload")
	}
}

func TestLookup(t *testing.T) {
	for _, c := range []struct {
		t    MessageType
		d    Direction
		want any
	}{
		{MessageTypeStats, DirClientToServer, ClientStats{}},
		{MessageTypeStats, DirServerToClient, ServerStats{}},
		{MessageTypePCOffer, DirClientToServer, PCOffer{}},
		{MessageTypePCOffer, DirServerToClient, PCOffer{}},
		{MessageTypeHello, DirClientToServer, Hello{}},
		{MessageTypeRoomState, DirServerToClient, RoomState{}},
		{MessageTypeRoomState, DirClientToServer | DirServerToClient, RoomState{}},
	} {
		s, ok := Lookup(c.t, c.d)
		if !ok || !reflect.DeepEqual(s.Payload, c.want) {
			t.Errorf("Lookup(%s, %d) = %T, %v; want %T", c.t, c.d, s.Payload, ok, c.want)
		}
	}
	for _, c := range []struct {
		t MessageType
		d Direction
	}{
		{MessageTypeHello, DirServerToClient},
		{MessageTypeRoomState, DirClientToServer},
		{MessageTypeWelcome, DirClientToServer},
		{"room.teleport", DirClientToServer},
		{"", DirClientToServer},
		{MessageTypeHello, 0},
	} {
		if _, ok := Lookup(c.t, c.d); ok {
			t.Errorf("Lookup(%q, %d) succeeded", c.t, c.d)
		}
	}
}

// TestRoleMatrix is 01 §6.3 as a table: every client message (per PC kind for pc.*) × every role.
func TestRoleMatrix(t *testing.T) {
	type row struct {
		t  MessageType
		pc PCKind
	}
	all := []Role{RoleFull, RoleViewer, RolePublisher, RoleAgent}
	publish := []Role{RoleFull, RolePublisher, RoleAgent}
	subscribe := []Role{RoleFull, RoleViewer}
	matrix := map[row][]Role{
		{MessageTypeHello, ""}:           all,
		{MessageTypeRoomJoin, ""}:        all,
		{MessageTypeRoomLeave, ""}:       all,
		{MessageTypePing, ""}:            all,
		{MessageTypeCapsUpdate, ""}:      all,
		{MessageTypeStats, ""}:           all,
		{MessageTypeStatsWatch, ""}:      all,
		{MessageTypeShareStart, ""}:      publish,
		{MessageTypeShareUpdate, ""}:     publish,
		{MessageTypeShareStop, ""}:       publish,
		{MessageTypeSubscribeUpdate, ""}: subscribe,
		{MessageTypeAgentSend, ""}:       all, // behind feature agent.relay
	}
	for _, t := range []MessageType{MessageTypePCOffer, MessageTypePCAnswer, MessageTypePCICE, MessageTypePCRestart, MessageTypePCClose} {
		matrix[row{t, PCKindPub}] = publish
		matrix[row{t, PCKindSub}] = subscribe
		matrix[row{t, ""}] = nil // a pc.* message without a valid pc is allowed for nobody
		matrix[row{t, "data"}] = nil
	}
	covered := map[MessageType]bool{}
	for r, allowed := range matrix {
		covered[r.t] = true
		for _, role := range all {
			want := false
			for _, a := range allowed {
				want = want || a == role
			}
			if got := Allowed(role, r.t, r.pc); got != want {
				t.Errorf("Allowed(%s, %s, %q) = %v, want %v", role, r.t, r.pc, got, want)
			}
		}
		if Allowed("admin", r.t, r.pc) {
			t.Errorf("unknown role allowed for %s", r.t)
		}
	}
	for _, s := range Registry {
		if s.Dir == DirClientToServer && !covered[s.Type] {
			t.Errorf("%s is missing from the role matrix", s.Type)
		}
	}
	for _, typ := range []MessageType{MessageTypeRoomState, "room.teleport"} {
		if Allowed(RoleFull, typ, "") {
			t.Errorf("%s allowed from a client", typ)
		}
	}
}

func TestRoleHelpers(t *testing.T) {
	for _, c := range []struct {
		r              Role
		publish, watch bool
	}{
		{RoleFull, true, true}, {RoleViewer, false, true}, {RolePublisher, true, false}, {RoleAgent, true, false},
		{"", false, false}, {"admin", false, false},
	} {
		if c.r.CanPublish() != c.publish || c.r.CanSubscribe() != c.watch {
			t.Errorf("%q: publish %v, subscribe %v", c.r, c.r.CanPublish(), c.r.CanSubscribe())
		}
		if c.r.AllowsPC(PCKindPub) != c.publish || c.r.AllowsPC(PCKindSub) != c.watch || c.r.AllowsPC("x") {
			t.Errorf("%q: AllowsPC mismatch", c.r)
		}
	}
}

func TestMaxMessageSize(t *testing.T) {
	for typ, want := range map[MessageType]int{
		MessageTypePCOffer: 256 << 10, MessageTypePCAnswer: 256 << 10, MessageTypeStats: 16 << 10,
		MessageTypePCICE: 64 << 10, MessageTypeHello: 64 << 10, MessageTypeAgentSend: 64 << 10, "unknown": 64 << 10,
	} {
		if got := MaxMessageSize(typ); got != want {
			t.Errorf("MaxMessageSize(%s) = %d, want %d", typ, got, want)
		}
	}
}
