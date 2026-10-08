package sfu

import (
	"cmp"
	"slices"
	"sync"
)

// Room is one room's media state, in memory only (02 §5.1): its participants, their Conns and their shares. The SFU
// creates it on the first Join and removes it with its last Conn; a Conn's shares end with it, so an empty room has
// no shares either.
type Room struct {
	id RoomID

	mu           sync.Mutex // guards the fields below, and every Participant's maps
	participants map[ParticipantID]*Participant
	conns        map[ConnID]*Conn
	shares       map[ShareID]*Share
	// policy is the room's codec policy (02 §8.3): ProfileHigh until README S69 adds the viewer set, the switch to
	// ProfileConstrainedBaseline and its hysteresis.
	policy ProfileKey
}

func newRoom(id RoomID) *Room {
	return &Room{
		id:           id,
		participants: map[ParticipantID]*Participant{},
		conns:        map[ConnID]*Conn{},
		shares:       map[ShareID]*Share{},
		policy:       ProfileHigh,
	}
}

// codecPolicy returns the room's codec policy.
func (r *Room) codecPolicy() ProfileKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy
}

// shareInfos returns the room's shares, sorted by StartedAt (then by id, so the order is stable).
func (r *Room) shareInfos() []ShareInfo {
	r.mu.Lock()
	shares := make([]*Share, 0, len(r.shares))
	for _, sh := range r.shares {
		shares = append(shares, sh)
	}
	r.mu.Unlock()
	infos := make([]ShareInfo, 0, len(shares))
	for _, sh := range shares {
		if info, ok := sh.info(); ok {
			infos = append(infos, info)
		}
	}
	sortShareInfos(infos)
	return infos
}

func sortShareInfos(infos []ShareInfo) {
	slices.SortFunc(infos, func(a, b ShareInfo) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}
