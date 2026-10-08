package signaltest

import (
	"slices"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// FakeSDP is the session description of the fake's answers.
const FakeSDP protocol.SDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"

// ShareParamsFor returns the fake's ShareParams for a share: constrained baseline H.264, the f and q layers of the
// auto preset in wire order, and 128 kbit/s Opus.
func ShareParamsFor(shareID string) protocol.ShareParams {
	return protocol.ShareParams{
		ShareID: shareID,
		Codec:   protocol.CodecH264ConstrainedBaseline,
		Encodings: []protocol.Encoding{
			{RID: protocol.RIDHigh, Layer: protocol.VideoLayerHigh, Active: true, MaxBitrate: 6_000_000,
				MaxFramerate: 60, MaxPixels: 1920 * 1080},
			{RID: protocol.RIDLow, Layer: protocol.VideoLayerLow, Active: true, MaxBitrate: 300_000,
				MaxFramerate: 15, MaxPixels: 640 * 360},
		},
		AudioBitrate: 128_000,
	}
}

// Media is a fake signal.MediaPlane: NewPeer returns a *Peer that records its calls.
type Media struct {
	mu         sync.Mutex
	peers      []*Peer
	newPeerErr error
}

// NewMedia returns a Media without peers.
func NewMedia() *Media { return &Media{} }

// NewPeer implements signal.MediaPlane.
func (m *Media) NewPeer(p signal.PeerParams, sink signal.MediaSink) (signal.MediaPeer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.newPeerErr != nil {
		return nil, m.newPeerErr
	}
	peer := &Peer{Params: p, sink: sink, fail: make(map[string]error), hooks: make(map[string]func())}
	m.peers = append(m.peers, peer)
	return peer, nil
}

// FailNewPeer makes NewPeer return err; nil restores the normal behavior.
func (m *Media) FailNewPeer(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newPeerErr = err
}

// Peers returns every peer created so far, in order.
func (m *Media) Peers() []*Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.peers)
}

// Peer returns the newest peer of a connection, or nil.
func (m *Media) Peer(connID string) *Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.peers) - 1; i >= 0; i-- {
		if m.peers[i].Params.ConnectionID == connID {
			return m.peers[i]
		}
	}
	return nil
}

// Call is one recorded MediaPeer call: the method name and its arguments.
type Call struct {
	Method string
	Args   []any
}

// Peer is a fake signal.MediaPeer. It records every call. By default CreateShare and UpdateShare return
// ShareParamsFor(shareID), HandleOffer answers with FakeSDP and the offer's pc, gen and neg, Subscribe ignores no
// share, Stats is empty, and everything else succeeds. Fail makes a method return an error, SetShareParams and
// SetStats set what the share methods and Stats return; tests emit the peer's events through Sink, and OnCall lets
// them act from inside a method (OnResync: from inside Resync, where a real peer re-emits its events).
//
// The hub reads Stats every 2 s while the connection has media in its room or watches the server's stats, so Calls
// has a "Stats" entry for each of those: CallsTo and MediaCalls leave them aside.
type Peer struct {
	Params signal.PeerParams
	sink   signal.MediaSink

	mu          sync.Mutex
	calls       []Call
	fail        map[string]error
	hooks       map[string]func()
	ignored     []string
	stats       protocol.ServerStats
	shareParams func(shareID string) protocol.ShareParams
	closed      bool
}

// Sink returns the hub's MediaSink for this peer.
func (p *Peer) Sink() signal.MediaSink { return p.sink }

// Calls returns the recorded calls, in order.
func (p *Peer) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// CallsTo returns the recorded calls of one method, in order.
func (p *Peer) CallsTo(method string) []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Call
	for _, c := range p.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// MediaCalls returns the recorded calls without the periodic Stats reads, in order.
func (p *Peer) MediaCalls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Call
	for _, c := range p.calls {
		if c.Method != "Stats" {
			out = append(out, c)
		}
	}
	return out
}

// Closed reports whether Close was called.
func (p *Peer) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Fail makes the named method (for example "HandleOffer") return err; nil restores the normal behavior. Methods
// without an error result ignore it.
func (p *Peer) Fail(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.fail, method)
		return
	}
	p.fail[method] = err
}

// SetIgnored sets the share ids that Subscribe reports as ignored.
func (p *Peer) SetIgnored(shareIDs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ignored = slices.Clone(shareIDs)
}

// OnCall sets a function that the named method (for example "CreateShare") calls once it has recorded the call,
// before it returns; nil removes it. Like every MediaPeer method it runs on the connection's actor, which holds no
// lock then: a test can change the hub's state in the middle of a request there.
func (p *Peer) OnCall(method string, f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f == nil {
		delete(p.hooks, method)
		return
	}
	p.hooks[method] = f
}

func (p *Peer) record(method string, args ...any) error {
	p.mu.Lock()
	p.calls = append(p.calls, Call{Method: method, Args: args})
	err, hook := p.fail[method], p.hooks[method]
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

// SetShareParams sets the function whose result CreateShare and UpdateShare return from now on; nil restores
// ShareParamsFor.
func (p *Peer) SetShareParams(f func(shareID string) protocol.ShareParams) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shareParams = f
}

func (p *Peer) paramsFor(shareID string) protocol.ShareParams {
	p.mu.Lock()
	f := p.shareParams
	p.mu.Unlock()
	if f == nil {
		return ShareParamsFor(shareID)
	}
	return f(shareID)
}

// CreateShare implements signal.MediaPeer.
func (p *Peer) CreateShare(shareID string, meta protocol.ShareStart) (protocol.ShareParams, error) {
	if err := p.record("CreateShare", shareID, meta); err != nil {
		return protocol.ShareParams{}, err
	}
	return p.paramsFor(shareID), nil
}

// UpdateShare implements signal.MediaPeer. The recorded update has its own copy of the label.
func (p *Peer) UpdateShare(shareID string, meta protocol.ShareUpdate) (protocol.ShareParams, error) {
	meta.Label = clonePtr(meta.Label)
	if err := p.record("UpdateShare", shareID, meta); err != nil {
		return protocol.ShareParams{}, err
	}
	return p.paramsFor(shareID), nil
}

// EndShare implements signal.MediaPeer.
func (p *Peer) EndShare(shareID string, reason protocol.EndReason) {
	_ = p.record("EndShare", shareID, reason)
}

// HandleOffer implements signal.MediaPeer.
func (p *Peer) HandleOffer(o protocol.PCOffer) (protocol.PCAnswer, error) {
	o.Tracks = slices.Clone(o.Tracks)
	if err := p.record("HandleOffer", o); err != nil {
		return protocol.PCAnswer{}, err
	}
	return protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: FakeSDP}, nil
}

// HandleAnswer implements signal.MediaPeer.
func (p *Peer) HandleAnswer(a protocol.PCAnswer) error { return p.record("HandleAnswer", a) }

// AddICE implements signal.MediaPeer.
func (p *Peer) AddICE(c protocol.PCICE) error { return p.record("AddICE", c) }

// Restart implements signal.MediaPeer.
func (p *Peer) Restart(r protocol.PCRestart) error { return p.record("Restart", r) }

// ClosePC implements signal.MediaPeer.
func (p *Peer) ClosePC(c protocol.PCClose) error { return p.record("ClosePC", c) }

// Subscribe implements signal.MediaPeer.
func (p *Peer) Subscribe(wants []protocol.SubscriptionWant) ([]string, error) {
	if err := p.record("Subscribe", slices.Clone(wants)); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ignored), nil
}

// SetCaps implements signal.MediaPeer.
func (p *Peer) SetCaps(c protocol.Caps) { _ = p.record("SetCaps", c) }

// OnResync sets a function that Resync calls after recording the call; nil removes it (OnCall for "Resync"). A real
// peer re-emits its pending sub offer, its subscription statuses and its quality hints there (01 §10.5): a test does
// the same through Sink.
func (p *Peer) OnResync(f func()) { p.OnCall("Resync", f) }

// Resync implements signal.MediaPeer.
func (p *Peer) Resync() { _ = p.record("Resync") }

// SetStats sets what Stats returns from now on.
func (p *Peer) SetStats(st protocol.ServerStats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st.Subs, st.Layers = slices.Clone(st.Subs), slices.Clone(st.Layers)
	p.stats = st
}

// Stats implements signal.MediaPeer: the stats of SetStats, empty by default. The slices are never nil.
func (p *Peer) Stats() protocol.ServerStats {
	_ = p.record("Stats")
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats
	st.Subs = append([]protocol.ServerSubStats{}, st.Subs...)
	st.Layers = append([]protocol.ServerLayerStats{}, st.Layers...)
	return st
}

// Close implements signal.MediaPeer.
func (p *Peer) Close() {
	_ = p.record("Close")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}
