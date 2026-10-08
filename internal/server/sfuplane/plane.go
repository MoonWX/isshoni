package sfuplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// conn is what a peer calls of its *sfu.Conn (02 §6.1). *sfu.Conn is its only implementation outside the tests,
// which record the calls with a fake.
type conn interface {
	HandleOffer(ctx context.Context, pc sfu.PCKind, gen, neg uint32, sdp string, tracks []sfu.TrackBinding) (string, error)
	HandleAnswer(ctx context.Context, pc sfu.PCKind, gen, neg uint32, sdp string) error
	AddICECandidate(ctx context.Context, pc sfu.PCKind, gen uint32, cand webrtc.ICECandidateInit) error
	RestartICE(ctx context.Context, pc sfu.PCKind, gen uint32) error
	ResetPC(ctx context.Context, pc sfu.PCKind, gen uint32) error
	ClosePC(ctx context.Context, pc sfu.PCKind, gen uint32) error
	StartShare(ctx context.Context, p sfu.StartShareParams) (sfu.ShareParams, error)
	UpdateShare(ctx context.Context, id sfu.ShareID, u sfu.ShareUpdate) (sfu.ShareParams, error)
	StopShare(ctx context.Context, id sfu.ShareID, r sfu.EndReason) error
	UpdateSubscriptions(ctx context.Context, items []sfu.SubscriptionUpdate) ([]error, error)
	SetDecodeCaps(ctx context.Context, caps sfu.DecodeCaps) error
	Resync()
	Stats() sfu.ConnStats
	Close(r sfu.EndReason)
}

var (
	_ conn              = (*sfu.Conn)(nil)
	_ signal.MediaPlane = (*Plane)(nil)
	_ sfu.RoomEvents    = roomEvents{}
)

// Plane is the SFU as the hub sees it: signal.MediaPlane over 02's *sfu.SFU (01 §15.4). New returns it together with
// the RoomEvents that the SFU needs when it is built; Bind then gives the Plane its SFU. It is safe for concurrent
// use.
type Plane struct {
	log *slog.Logger

	mu sync.Mutex // guards the fields below and every peer's media; never held while calling the SFU or a MediaSink
	// join is the bound SFU's Join; nil until Bind.
	join func(sfu.JoinParams) (conn, error)
	// peers are the peers whose Conn is joined, by its id: where the RoomEvents of a share find the MediaSink of the
	// connection that publishes it.
	peers map[sfu.ConnID]*peer
}

// New returns the MediaPlane for the hub and the RoomEvents for the SFU. The wiring (04 §6.6) passes the events as
// sfu.Deps.Events to sfu.New and then calls Bind with the SFU. log gets component=sfuplane; nil discards the logs.
func New(log *slog.Logger) (*Plane, sfu.RoomEvents) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Plane{log: log.With("component", "sfuplane"), peers: map[sfu.ConnID]*peer{}}
	return p, roomEvents{p}
}

// Bind gives the Plane the SFU that was built with its RoomEvents. The wiring calls it once, before the hub serves
// its first connection; until then NewPeer fails.
func (p *Plane) Bind(s *sfu.SFU) {
	var join func(sfu.JoinParams) (conn, error)
	if s != nil {
		join = func(jp sfu.JoinParams) (conn, error) {
			c, err := s.Join(jp)
			if err != nil {
				return nil, err
			}
			return c, nil
		}
	}
	p.mu.Lock()
	p.join = join
	p.mu.Unlock()
}

// errNotBound is NewPeer's error before Bind.
var errNotBound = errors.New("sfuplane: not bound to an SFU")

// NewPeer implements signal.MediaPlane: it joins the connection to its room in the SFU (SFU.Join) and returns the
// peer that drives the resulting Conn. A user is one participant of a room, so the user id is also the SFU's
// participant id. The peer is the Conn's Signaler and turns what the SFU sends into calls of sink; it lives as long
// as the connection is in the room, WebSocket resumes included, since the hub keeps the sink per connection.
//
// Its error is a plain one, which the hub answers with error{internal}: the Plane isn't bound, the role is unknown,
// or the SFU refused the Join (it is closed, or the call broke its contract, 02 §6.3).
func (p *Plane) NewPeer(pp signal.PeerParams, sink signal.MediaSink) (signal.MediaPeer, error) {
	p.mu.Lock()
	join := p.join
	p.mu.Unlock()
	switch {
	case join == nil:
		return nil, errNotBound
	case sink == nil:
		return nil, errors.New("sfuplane: no MediaSink")
	}
	role, ok := sfuRole(pp.Role)
	if !ok {
		return nil, fmt.Errorf("sfuplane: unknown role %q", pp.Role)
	}
	pr := &peer{
		plane: p,
		id:    sfu.ConnID(pp.ConnectionID),
		sink:  sink,
		log:   p.log.With("conn_id", pp.ConnectionID, "room_id", pp.RoomID, "user_id", pp.UserID),
		media: map[sfu.ShareID]sfu.ShareState{},
	}
	c, err := join(sfu.JoinParams{
		Room:        sfu.RoomID(pp.RoomID),
		Participant: sfu.ParticipantID(pp.UserID),
		User:        sfu.UserID(pp.UserID),
		Conn:        pr.id,
		Role:        role,
		Client:      sfuClientKind(pp.Client.Kind),
		Decode:      decodeCaps(pp.Caps),
		Signaler:    pr,
	})
	if err != nil {
		return nil, fmt.Errorf("sfuplane: join: %w", err)
	}
	// The Conn's actor may already call the peer as its Signaler; that path never reads pr.conn. The hub calls the
	// peer's MediaPeer methods only after NewPeer has returned.
	pr.conn = c
	p.mu.Lock()
	p.peers[pr.id] = pr
	p.mu.Unlock()
	return pr, nil
}

// forget takes a closed peer out of the table, unless a newer peer of the same connection (in its next room) has
// taken its place.
func (p *Plane) forget(pr *peer) {
	p.mu.Lock()
	if p.peers[pr.id] == pr {
		delete(p.peers, pr.id)
	}
	p.mu.Unlock()
}

// roomEvents is the Plane as the SFU's RoomEvents (02 §6.2). The SFU calls it from its ticker and from whoever ends
// a share, with the share's report lock held: it never blocks and never calls the SFU.
type roomEvents struct{ p *Plane }

// ShareUpdated implements sfu.RoomEvents: a share's media fact becomes a ShareMedia call on the sink of the
// connection that publishes it (01 §15.4). The SFU reports the share as it is now, so the Plane remembers each
// share's last reported state to tell what happened:
//
//   - live after pending or stalled is Live (the first keyframe, or recovered);
//   - stalled is Stalled;
//   - live after live is Changed: a layer attached or ended, the profile changed or the audio changed.
//
// The SFU makes these calls for one share one at a time, in order, and nothing after its ShareEnded.
func (e roomEvents) ShareUpdated(_ sfu.RoomID, s sfu.ShareInfo) {
	p := e.p
	p.mu.Lock()
	pr := p.peers[s.Conn]
	prev := sfu.SharePending // a share is pending until its first report
	if pr != nil {
		if st, reported := pr.media[s.ID]; reported {
			prev = st
		}
		pr.media[s.ID] = s.State
	}
	p.mu.Unlock()
	if pr == nil {
		// The publishing connection's peer has closed: the share is ending with it.
		p.log.Debug("share update dropped: no peer", "share_id", string(s.ID), "conn_id", string(s.Conn))
		return
	}
	var kind signal.ShareMediaKind
	switch {
	case s.State == sfu.ShareLive && prev == sfu.ShareLive:
		kind = signal.ShareMediaChanged
	case s.State == sfu.ShareLive:
		kind = signal.ShareMediaLive
	case s.State == sfu.ShareStalled:
		kind = signal.ShareMediaStalled
	default:
		// Pending: the hub's share is starting and learns nothing from it.
		pr.log.Debug("share update dropped", "share_id", string(s.ID), "state", s.State.String())
		return
	}
	codec, ok := codecKey(s.Profile)
	if !ok {
		pr.log.Error("share with a malformed H.264 profile", "share_id", string(s.ID))
	}
	pr.sink.ShareMedia(string(s.ID), signal.ShareMediaEvent{
		Kind:   kind,
		Layers: wireLayers(s.Layers),
		Codec:  codec,
		Audio:  s.Audio,
	})
}

// ShareEnded implements sfu.RoomEvents. The hub ends shares itself, so nothing is reported; the Plane only forgets
// the share's state.
func (e roomEvents) ShareEnded(_ sfu.RoomID, s sfu.ShareInfo, _ sfu.EndReason) {
	p := e.p
	p.mu.Lock()
	if pr := p.peers[s.Conn]; pr != nil {
		delete(pr.media, s.ID)
	}
	p.mu.Unlock()
}

// CodecPolicyChanged implements sfu.RoomEvents. It is ignored: each publisher gets a CodecPolicyEvent per share
// through its Signaler, and that is what becomes a quality.hint.
func (roomEvents) CodecPolicyChanged(sfu.RoomID, sfu.ProfileKey) {}
