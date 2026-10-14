package sfu

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// Room is one room's media state, in memory only (02 §5.1): its participants, their Conns and their shares, and the
// room's codec policy. The SFU creates it on the first Join and removes it with its last Conn; a Conn's shares end
// with it, so an empty room has no shares either.
type Room struct {
	id RoomID

	// policyMu orders the room's codec policy changes and what they are reported with (SFU.updateCodecPolicy): one
	// evaluation at a time, and its CodecPolicyChanged and CodecPolicyEvents out before the next one starts, so every
	// publisher hears of the changes in the order they happened. It is taken before mu and never while another lock
	// of doc.go's order is held; the reports it is held across never block.
	policyMu sync.Mutex

	mu           sync.Mutex // guards the fields below, every Participant's maps and every Conn's decode caps
	participants map[ParticipantID]*Participant
	conns        map[ConnID]*Conn
	shares       map[ShareID]*Share
	// policy is the room's codec policy with its hysteresis (02 §8.3).
	policy codecPolicy
}

func newRoom(id RoomID) *Room {
	return &Room{
		id:           id,
		participants: map[ParticipantID]*Participant{},
		conns:        map[ConnID]*Conn{},
		shares:       map[ShareID]*Share{},
		policy:       codecPolicy{profile: ProfileHigh},
	}
}

// codecPolicy returns the room's codec policy: ProfileHigh or ProfileConstrainedBaseline.
func (r *Room) codecPolicy() ProfileKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.profile
}

// wantedPolicyLocked returns the codec policy that the room's viewers need now (02 §8.3): the viewer set is the Conns
// with a role that subscribes (full or viewer) whose decode caps list some H.264. A Conn without any is waiting for
// a decoder (02 §8.5): no profile can help it, so it doesn't count. r.mu is held.
func (r *Room) wantedPolicyLocked() ProfileKey {
	viewers := make([][]ProfileKey, 0, len(r.conns))
	for _, c := range r.conns {
		if c.role.canSubscribe() && len(c.decode.H264) > 0 {
			viewers = append(viewers, c.decode.H264)
		}
	}
	return policyFor(viewers)
}

// codecPolicy is a room's H.264 profile policy (02 §8.3): ProfileHigh while every viewer can receive High, else
// ProfileConstrainedBaseline, with a hysteresis on the way back up.
type codecPolicy struct {
	profile ProfileKey // ProfileHigh or ProfileConstrainedBaseline
	// clear: the policy is cb and the room has had no viewer that needs it since clearSince (monotonic ns).
	clear      bool
	clearSince int64
}

// next moves the policy towards want, what the viewers need at now, and reports whether the policy changed. A viewer
// that needs Constrained Baseline gets it at once (high → cb): with High it would see nothing. The way back
// (cb → high) waits until the room has gone hold without such a viewer, so a Firefox user who reconnects doesn't
// make every sharer switch twice. Times are monotonic ns; a clock that runs back counts as no time passed.
func (p *codecPolicy) next(want ProfileKey, now int64, hold time.Duration) bool {
	switch {
	case want != ProfileHigh:
		p.clear = false
		if p.profile == ProfileConstrainedBaseline {
			return false
		}
		p.profile = ProfileConstrainedBaseline
		return true
	case p.profile == ProfileHigh:
		return false
	case !p.clear:
		p.clear, p.clearSince = true, now
	}
	if time.Duration(now-p.clearSince) < hold {
		return false
	}
	p.profile, p.clear = ProfileHigh, false
	return true
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
