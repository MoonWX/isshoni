package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Shares (docs/m2/01-client-core.md D12, §11.4): one at a time, each with an actor goroutine that owns its Publisher,
// its ratectl.Controller, the engine session and the pub PC's negotiation; the connection actor posts the server's
// messages to it.

// The engine profile keys (01 §8.6).
const (
	highProfile = publish.HighProfile
	cbProfile   = publish.ConstrainedBaselineProfile
)

// ShareRequest is what StartShare starts.
type ShareRequest struct {
	RoomID string
	Source media.Source    // a picked source (03: Engine.Pick); Engine.Start consumes it
	Preset protocol.Preset // "" = auto
	Audio  bool            // share.start{audio}; false: Config.Audio = nil
	Label  string          // user-typed only; "" from M2's UI (plan: labels default to Screen or Window)
}

// ShareState is a share's state (§11.4).
type ShareState string

// The share states.
const (
	StateJoining      ShareState = "joining"
	StateStarting     ShareState = "starting"
	StateLive         ShareState = "live"
	StateReconnecting ShareState = "reconnecting"
	StateUnreachable  ShareState = "unreachable"
	StateEnded        ShareState = "ended"
)

// ShareSnapshot is a share as the app sees it (§11.9). Live counters are marked; they never fire EventShare.
type ShareSnapshot struct {
	ID            string // the current shareId (changes on republish)
	RoomID        string
	Kind          protocol.ShareKind
	Preset        protocol.Preset
	State         ShareState
	Paused        bool
	Audio         bool // the share has audio
	AudioSentBps  int  // live counter
	NoOSIndicator bool // no OS border (borderless WGC or DD, from EventLayer): 03 must show the Sharing pill
	Minimized     bool // the shared window is minimized (EventSourceMinimized)
	Watchers      []protocol.Watcher
	Encoder       string // f's encoder ("nvenc", …)
	HW            bool   // f's
	Layers        []LayerSnapshot
	Upload        ratectl.UploadHint
	Overloaded    bool                        // ratectl.CPU
	Server        []protocol.ServerLayerStats // with WatchServerStats(true)
	End           *EndInfo
}

// LayerSnapshot is one video layer of a ShareSnapshot.
type LayerSnapshot struct {
	RID           string
	Width, Height int
	FPS           float64 // live counter (Publisher)
	Active        bool
	Encoder       string // this layer's encoder (layers may differ, 04 §9.7)
	HW            bool
	Profile       string
	Limit         string // "none" | "cpu" | "gpu" | "source"
	TargetBps     int
	SentBps       int                  // live counter
	CaptureToSend publish.LatencyStats // live counter
	EngineLatency publish.LatencyStats // live counter: capture → engine ring (Session.Stats LatencyP50/P95; Max 0)
	FPSIn         float64              // live counter: captured frames per second (Session.Stats)
}

// EndInfo says why a share ended (§11.9's table).
type EndInfo struct {
	Reason    string             // "user" | "source-gone" | "server" | "server-unreachable" | "unlinked" | "error"
	Media     media.ErrorCode    // engine failures
	Protocol  protocol.ErrorCode // server error replies
	Server    protocol.EndReason // server-ended shares
	ByThisApp bool               // true when this core sent the share.stop that ended it
}

// The values of EndInfo.Reason.
const (
	EndUser              = "user"
	EndSourceGone        = "source-gone"
	EndServer            = "server"
	EndServerUnreachable = "server-unreachable"
	EndUnlinked          = "unlinked"
	EndError             = "error"
)

// clone returns a deep copy of s.
func (s *ShareSnapshot) clone() *ShareSnapshot {
	c := *s
	c.Watchers = slices.Clone(s.Watchers)
	c.Layers = slices.Clone(s.Layers)
	c.Server = slices.Clone(s.Server)
	if s.End != nil {
		e := *s.End
		c.End = &e
	}
	return &c
}

// layer returns the snapshot's layer rid, or nil.
func (s *ShareSnapshot) layer(rid string) *LayerSnapshot {
	for i := range s.Layers {
		if s.Layers[i].RID == rid {
			return &s.Layers[i]
		}
	}
	return nil
}

// Share is the active share of a Client. Its methods are safe for concurrent use.
type Share struct {
	c      *Client
	req    ShareRequest
	log    *slog.Logger
	abort  context.CancelFunc // ends StartShare while it runs
	inbox  mailbox
	ends   chan endRequest
	done   chan struct{} // closed when the share has ended
	run    chan struct{} // closed when the actor runs
	events coalescer

	// Set by StartShare before the actor runs; the actor's alone afterwards.
	params protocol.ShareParams
	probe  media.Probe
	sess   media.Session
	ctl    *ratectl.Controller
	pump   *pump
	ctx    context.Context // the actor's: ends when end asks the actor to end the share, or when the share ends
	cancel context.CancelFunc
	connID string // the connection the server's share is on: share.start's (and a republish's); "" when unknown
	// The actor's alone.
	drv     pubDriver
	cons    *consumer
	seen    map[string]bool // the layers whose first EventLayer arrived
	limits  map[string]string
	inState bool // the share (its current id) was in a room.state
	live    bool // the share has been live (§11.4 step 5); it stays so across a republish
	rec     recovery
	// hints.go: the profile the session runs (its newest EventLayer's), the one the Publisher puts first ("" = the
	// one the session runs: a codec hint's, or the server's policy after a republish), and the preset.
	running     string
	sendProfile string
	ps          presetState

	// hints.go, safe for concurrent use: the pause (its own mutex), and the profile the Publisher's send loop asked
	// the session for.
	pz  pauseState
	ask profileAsk

	mu   sync.Mutex // guards the fields below
	snap ShareSnapshot
	pub  *publish.Publisher
}

// endRequest asks the actor to end the share.
type endRequest struct {
	info  EndInfo
	ctx   context.Context // bounds the share.stop and room.leave requests
	stop  bool            // send share.stop
	leave bool            // room.leave afterwards
}

// StartShare joins r.RoomID, starts the share on the server and then on the engine (§11.4 steps 1–3), and returns
// once Engine.Start succeeded; the pub PC and the live state follow asynchronously (Snapshot().State). It returns
// ErrNotReady at once when Status().Conn != "online" (03 waits up to 10 s for EventStatus online itself), ErrBusy
// while a share exists, a *protocol.Error for room and share errors, and a *media.Error for engine start errors.
// Cancelling ctx before it returns aborts the start: share.stop if share.start succeeded, engine Stop, room.leave,
// and no share remains. 03 gives it a 10 s ctx after its own 10 s wait (03 §6.2), so both fit its 20 s cap.
func (c *Client) StartShare(ctx context.Context, r ShareRequest) (*Share, error) {
	if !c.isOnline() {
		return nil, ErrNotReady
	}
	if r.Preset == "" {
		r.Preset = protocol.PresetAuto
	}
	switch {
	case r.RoomID == "":
		return nil, errors.New("core: start share: no room")
	case r.Source.ID == "":
		return nil, errors.New("core: start share: no source")
	case !r.Preset.Valid():
		return nil, fmt.Errorf("core: start share: unknown preset %q", r.Preset)
	}
	if !c.startOp() {
		return nil, ErrNotReady
	}
	defer c.ops.Done()
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	defer context.AfterFunc(c.life, cancel)()

	s := newShare(c, r, cancel)
	c.mu.Lock()
	if c.share != nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	c.share = s
	c.mu.Unlock()

	c.roomMu.Lock()
	err := s.start(sctx)
	c.roomMu.Unlock()
	if err != nil {
		if c.life.Err() != nil && ctx.Err() == nil {
			err = ErrClosed // Close or the end of Run cut the start short
		}
		return nil, err
	}
	s.takeSignal()
	go s.actor()
	return s, nil
}

// startTimeout bounds StartShare's steps: the server's start timeout (01 §4.4), which also ends a share that is not
// live by then.
const startTimeout = 30 * time.Second

// newShare returns a share in state joining.
func newShare(c *Client, r ShareRequest, abort context.CancelFunc) *Share {
	s := &Share{
		c: c, req: r, log: c.log, abort: abort,
		ends: make(chan endRequest), done: make(chan struct{}), run: make(chan struct{}),
		seen: map[string]bool{}, limits: map[string]string{}, ps: presetState{applied: r.Preset},
		snap: ShareSnapshot{RoomID: r.RoomID, Kind: shareKind(r.Source.Kind), Preset: r.Preset, State: StateJoining,
			Audio: r.Audio},
	}
	s.inbox.wake = make(chan struct{}, 1)
	s.events.emit = s.emit
	return s
}

// shareKind maps a source kind to share.start's kind.
func shareKind(k media.SourceKind) protocol.ShareKind {
	if k == media.SourceWindow {
		return protocol.ShareKindWindow
	}
	return protocol.ShareKindScreen
}

// start runs §11.4's steps 1–3. On an error the steps done are undone (share.stop, room.leave) and the share is
// over: it is no longer the Client's, and its Done is closed. c.roomMu is held.
func (s *Share) start(ctx context.Context) (err error) {
	c := s.c
	var shareID string
	joined := false // a room.join went out: whatever its reply, the abort leaves
	defer func() {
		if err == nil {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if shareID != "" {
			if serr := c.request(cctx, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: shareID}, nil); serr != nil {
				s.log.Debug("share.stop of an aborted start", logx.Err(serr))
			}
		}
		if lerr := c.leaveRoom(cctx, joined); lerr != nil {
			s.log.Debug("room.leave of an aborted start", logx.Err(lerr))
		}
		s.over(startEnd(err))
	}()

	// Steps 1 and 2. A request that signaling's drop cut short goes again once it is ready (§11.7): on the same
	// connection (resumed) share.start keeps its ref, so the server answers with the share it may have made; a new
	// connection has neither the room nor the share, and the start begins again with a new ref.
	var params protocol.ShareParams
	ref := newRef()
	for {
		seq, conn := c.readySeqNow(), c.sigConnID()
		// 1. room.join, unless the connection is in the room already.
		if c.roomID() != s.req.RoomID {
			joined = true
			if err := c.join(ctx, s.req.RoomID); err != nil {
				var perr *protocol.Error
				if errors.As(err, &perr) {
					joined = false // the server refused it: the connection is in no room
				}
				if lostSignaling(err) && c.waitReady(ctx, seq) == nil {
					continue
				}
				return requestErr(ctx, err)
			}
		}
		// 2. share.start.
		s.update(func(snap *ShareSnapshot) { snap.State = StateStarting })
		start := protocol.ShareStart{Kind: shareKind(s.req.Source.Kind), Label: s.req.Label, Preset: s.req.Preset,
			Audio: s.req.Audio, Ref: ref}
		if err := c.request(ctx, protocol.MessageTypeShareStart, start, &params); err != nil {
			if lostSignaling(err) && c.waitReady(ctx, seq) == nil {
				if c.sigConnID() != conn {
					ref = newRef()
				}
				continue
			}
			return requestErr(ctx, err)
		}
		if c.sigConnID() == conn {
			s.connID = conn // else unknown: the actor publishes the share again on the connection it finds
		}
		break
	}
	shareID = params.ShareID
	s.params = params
	s.update(func(snap *ShareSnapshot) {
		snap.ID = params.ShareID
		for _, e := range params.Encodings {
			snap.Layers = append(snap.Layers, LayerSnapshot{RID: e.RID, Active: e.Active})
		}
	})
	// 3. Engine.Start.
	probe, err := c.o.Engine.Probe(ctx, false)
	if err != nil {
		return err
	}
	s.probe = probe
	cfg := s.config()
	sess, err := c.o.Engine.Start(ctx, cfg)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		_ = sess.Stop()
		return ctx.Err()
	}
	s.sess = sess
	var layers []string
	active := map[string]bool{}
	for _, l := range cfg.Layers {
		layers = append(layers, l.RID)
		active[l.RID] = !l.Inactive
	}
	s.pump = newPump(sess, layers, active, s.log)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.pump.run()
	s.log.Info("share started", slog.String("share_id", params.ShareID), slog.String("profile", cfg.Profile))
	return nil
}

// requestErr is a failed request's error for StartShare: the server's *protocol.Error as it is, ctx's error when
// ctx ended.
func requestErr(ctx context.Context, err error) error {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return perr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// startEnd is the EndInfo of a start that failed with err.
func startEnd(err error) EndInfo {
	var perr *protocol.Error
	var merr *media.Error
	switch {
	case errors.As(err, &perr):
		return EndInfo{Reason: EndError, Protocol: perr.Code}
	case errors.As(err, &merr):
		return engineEnd(merr)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return EndInfo{Reason: EndUser, ByThisApp: true}
	}
	return EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}
}

// engineEnd is the EndInfo of an engine failure (§5.2's table): source-gone for source_gone, else error.
func engineEnd(err error) EndInfo {
	var merr *media.Error
	if !errors.As(err, &merr) {
		return EndInfo{Reason: EndError, Media: media.CodeInternal}
	}
	if merr.Code == media.CodeSourceGone {
		return EndInfo{Reason: EndSourceGone, Media: media.CodeSourceGone}
	}
	return EndInfo{Reason: EndError, Media: merr.Code}
}

// config returns the engine's start config from the share's params and probe (§11.4 step 3), and makes the share's
// rate controller, whose first targets it uses.
func (s *Share) config() media.Config {
	c := s.c
	enc := firstEncoder(s.probe, c.o.Encoder)
	aspect := 0.0
	if s.req.Source.Width > 0 && s.req.Source.Height > 0 {
		aspect = float64(s.req.Source.Width) / float64(s.req.Source.Height)
	}
	s.ctl = ratectl.New(ratectl.Config{Params: s.params, SourceAspect: aspect})
	targets := map[string]int{}
	for _, a := range s.ctl.Initial() {
		targets[a.Layer] = a.Bps
	}
	cfg := media.Config{
		Source:     s.req.Source.ID,
		Listed:     c.o.allowListed,
		Encoder:    c.o.Encoder,
		Profile:    engineProfile(s.params.Codec, enc),
		Preset:     string(s.req.Preset),
		Borderless: s.probe.Capture.Borderless,
	}
	for _, e := range s.params.Encodings {
		cfg.Layers = append(cfg.Layers, media.LayerConfig{
			RID: e.RID, MaxPixels: e.MaxPixels, MaxFPS: e.MaxFramerate, Bitrate: targets[e.RID], Inactive: !e.Active,
		})
	}
	if s.req.Audio {
		ac := media.AudioConfig{Mode: media.AudioExcludeApps}
		if c.o.AudioConfig != nil {
			ac = c.o.AudioConfig()
		}
		ac.BitrateBPS = targets[media.LayerAudio] // 0 when the params carry none: the engine's default
		cfg.Audio = &ac
	}
	s.update(func(snap *ShareSnapshot) {
		for i := range snap.Layers {
			snap.Layers[i].TargetBps = targets[snap.Layers[i].RID]
		}
	})
	return cfg
}

// firstEncoder returns the encoder a session will run on: the first available one in the probe, or the one forced.
func firstEncoder(p media.Probe, forced string) media.EncoderInfo {
	for _, e := range p.Encoders {
		if e.Tested && (forced == "" || e.Name == forced) {
			return e
		}
	}
	return media.EncoderInfo{}
}

// engineProfile is Config.Profile: the params' codec as an engine profile (6400 or 42e0) when the encoder lists it,
// else the encoder's first profile (the SFU then answers with what the offer has, M1 02 §8.4).
func engineProfile(codec protocol.CodecKey, enc media.EncoderInfo) string {
	p := publish.EngineProfile(codec.H264Profile())
	if p != highProfile && p != cbProfile {
		p = highProfile
	}
	if len(enc.Profiles) > 0 && !slices.Contains(enc.Profiles, p) {
		return enc.Profiles[0]
	}
	return p
}

// newRef returns a share.start ref: 8 random base32 characters (§11.4 step 2).
func newRef() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	for i := range b {
		b[i] = alphabet[b[i]%32]
	}
	return string(b[:])
}

// Snapshot returns the share as it is now, with the live counters of its Publisher.
func (s *Share) Snapshot() ShareSnapshot {
	s.mu.Lock()
	snap := s.snap.clone()
	pub := s.pub
	s.mu.Unlock()
	if pub == nil || snap.State == StateEnded {
		return *snap
	}
	for _, ts := range pub.Stats().Tracks {
		if ts.Kind == webrtc.RTPCodecTypeAudio {
			snap.AudioSentBps = ts.Bitrate
			continue
		}
		if l := snap.layer(ts.Layer); l != nil {
			l.SentBps, l.FPS, l.CaptureToSend = ts.Bitrate, ts.FPS, ts.CaptureToSend
		}
	}
	return *snap
}

// Stop ends the share (§11.4): share.stop (the engine stops while its reply is awaited), pc.close, the Publisher
// closed, room.leave. ctx bounds the requests; the local part always runs. It returns when the share has ended, with
// ctx's error when ctx ended before the requests did.
func (s *Share) Stop(ctx context.Context) error {
	s.end(ctx, endRequest{info: EndInfo{Reason: EndUser, ByThisApp: true}, stop: true, leave: true})
	return ctx.Err()
}

// Done is closed when the share has ended; Snapshot().End says why.
func (s *Share) Done() <-chan struct{} { return s.done }

// end has the actor end the share and waits until it has ended. A start in progress is aborted instead. The actor's
// ctx ends first, so blocking work it is doing (an offer's ICE gathering, up to offerTimeout) returns at once and the
// request is taken within the bound of Close and Stop; finish's requests use req.ctx.
func (s *Share) end(ctx context.Context, req endRequest) {
	s.abort() // a start in progress ends; a running share isn't affected
	req.ctx = ctx
	select {
	case <-s.run:
		s.cancel() // set before the actor ran (start), so reading it after run closed is race-free
		select {
		case s.ends <- req:
		case <-s.done:
		}
	case <-s.done:
	}
	<-s.done
}

// update changes the snapshot and fires EventShare (coalesced) when a field that isn't a live counter changed.
func (s *Share) update(change func(*ShareSnapshot)) {
	s.mu.Lock()
	before := s.snap.clone()
	change(&s.snap)
	changed := !reflect.DeepEqual(before, s.snap.clone())
	s.mu.Unlock()
	if changed {
		s.events.changed()
	}
}

// emit publishes the share's snapshot as an EventShare.
func (s *Share) emit() {
	snap := s.Snapshot()
	s.c.events.publish(func() Event { return Event{Kind: EventShare, Share: snap.clone()} })
}

// over marks the share ended with info, releases it from the Client and closes Done.
func (s *Share) over(info EndInfo) {
	s.c.mu.Lock()
	if s.c.share == s {
		s.c.share = nil
	}
	s.c.mu.Unlock()
	s.mu.Lock()
	s.snap.State, s.snap.End = StateEnded, &info
	s.mu.Unlock()
	close(s.done)
	s.events.flush()
	s.log.Info("share ended", slog.String("share_id", s.params.ShareID), slog.String("reason", info.Reason),
		slog.String("media", string(info.Media)), slog.String("protocol", string(info.Protocol)),
		slog.String("server", string(info.Server)))
}

// The messages the connection actor posts to the share's actor.
type (
	msgRoomState struct{ st protocol.RoomState }
	msgRoomEvent struct{ ev protocol.RoomEvent }
	msgAnswer    struct{ a protocol.PCAnswer }
	msgRestart   struct{ r protocol.PCRestart }
	msgHint      struct{ h protocol.QualityHint }
	msgError     struct{ e protocol.Error }
	msgResend    struct{ gen, neg uint32 }
	// msgWelcome: signaling is ready (seq counts the readies), on connection connID, resumed or a new connection,
	// in the welcome's room (a resumed welcome names the room the connection is in, "" for none).
	msgWelcome struct {
		seq     uint64
		connID  string
		resumed bool
		roomID  string
	}
	// msgOffline: signaling left the ready of seq at at: the share's hold starts (§11.7).
	msgOffline struct {
		seq uint64
		at  time.Time
	}
)

// post hands a message to the actor; it never blocks. Messages to a share that has ended are dropped.
func (s *Share) post(m any) {
	select {
	case <-s.done:
		return
	default:
	}
	s.inbox.post(m)
}

// mailbox is the actor's unbounded inbox, so the connection actor never waits for a share.
type mailbox struct {
	mu   sync.Mutex
	msgs []any
	wake chan struct{} // one slot
}

func (m *mailbox) post(v any) {
	m.mu.Lock()
	m.msgs = append(m.msgs, v)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *mailbox) take() []any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.msgs
	m.msgs = nil
	return out
}

// actor is the share's goroutine: engine events and the session's end from the pump, the server's messages, the
// pub PC's looks (pubdriver.go), the hold (recovery.go), the rate controller's Ticks (hints.go), and the requests
// to end the share.
func (s *Share) actor() {
	close(s.run)
	tick := time.NewTicker(s.c.timing.pcPoll)
	defer tick.Stop()
	rate := time.NewTicker(ratectl.TickEvery)
	defer rate.Stop()
	if req := s.begin(); req != nil {
		s.finish(*req)
		return
	}
	for {
		select {
		case m := <-s.pump.out:
			if m.err != nil {
				s.log.Info("engine session ended", logx.Err(m.err))
				s.finish(endRequest{info: engineEnd(m.err), stop: true, leave: true})
				return
			}
			if info := s.onEngineEvent(*m.ev); info != nil {
				s.finish(endRequest{info: *info, stop: true, leave: true})
				return
			}
		case <-s.inbox.wake:
			for _, m := range s.inbox.take() {
				if req := s.onMessage(m); req != nil {
					s.finish(*req)
					return
				}
			}
		case <-s.rec.holdC():
			if req := s.holdOver(); req != nil {
				s.finish(*req)
				return
			}
		case now := <-tick.C:
			if req := s.tick(now); req != nil {
				s.finish(*req)
				return
			}
		case now := <-rate.C:
			s.rateTick(now)
		case req := <-s.ends:
			s.finish(req)
			return
		}
	}
}

// onMessage handles a message of the connection actor; a request to end the share when it ends it.
func (s *Share) onMessage(m any) *endRequest {
	switch m := m.(type) {
	case msgRoomState:
		return s.onRoomState(m.st)
	case msgRoomEvent:
		ev := m.ev
		if ev.Kind == protocol.RoomEventKindShareStopped && ev.ShareID != "" && ev.ShareID == s.params.ShareID {
			return &endRequest{info: EndInfo{Reason: EndServer, Server: ev.Reason}, leave: true}
		}
	case msgAnswer:
		if info := s.onAnswer(m.a); info != nil {
			return &endRequest{info: *info, stop: true, leave: true}
		}
		if info := s.maybeReoffer(); info != nil {
			return &endRequest{info: *info, stop: true, leave: true}
		}
	case msgCall:
		m.done <- m.fn()
	case msgProfileFailed:
		s.onProfileFailed(m)
	case msgError:
		return s.onError(m.e)
	case msgHint:
		s.onHint(m.h)
	case msgRestart:
		if info := s.onRestart(m.r); info != nil {
			return &endRequest{info: *info, stop: true, leave: true}
		}
	case msgResend:
		s.resend(m.gen, m.neg)
	case msgWelcome:
		return s.onWelcome(m)
	case msgOffline:
		s.onOffline(m)
	}
	return nil
}

// onRoomState follows the share in room.state (§11.4 step 5, §11.7). A room.state that comes while signaling is
// down is kept for the reconcile after the welcome; the first one after a resumed welcome is reconciled with the
// server's view of a share it may have lost (reconcile).
func (s *Share) onRoomState(st protocol.RoomState) *endRequest {
	if !s.rec.online {
		s.rec.offline = &st
		return nil
	}
	resync := s.rec.resync
	s.rec.resync = false
	return s.reconcile(st, resync)
}

// reconcile follows the share in a room.state: it goes live when the server says so (the SFU saw its first
// keyframe, §11.4 step 5), a republished one is back once its new id is live, its watchers go to the snapshot, and
// the connection's shares that the core doesn't have are stopped (§11.7). A share that vanished was ended by the
// server; after a resumed welcome (resync) the server lost it instead, and it is republished.
func (s *Share) reconcile(st protocol.RoomState, resync bool) *endRequest {
	i := slices.IndexFunc(st.Shares, func(si protocol.ShareInfo) bool { return si.ID == s.params.ShareID })
	if i < 0 {
		switch {
		case s.rec.republish:
			// Its republish is due: the share it had is gone, and the new one isn't there yet.
		case resync:
			s.log.Info("the server lost the share: publishing it again", slog.String("share_id", s.params.ShareID))
			s.rec.republish = true
			return s.republish()
		case s.inState:
			// Gone without a room.event naming it (that one ends the share first, with its reason).
			return &endRequest{info: EndInfo{Reason: EndServer}, leave: true}
		}
		return nil
	}
	s.inState = true
	info := st.Shares[i]
	if info.Status == protocol.ShareStatusLive || info.Status == protocol.ShareStatusPaused {
		if !s.live {
			s.live = true
			s.ctl.Live(time.Now())
			s.log.Info("share live", slog.String("share_id", s.params.ShareID))
		}
		s.rec.republishing = false
	}
	s.update(func(snap *ShareSnapshot) { snap.Watchers = slices.Clone(info.Watchers) })
	s.syncState()
	s.reconcileHints(info)
	s.stopOrphans(st)
	return nil
}

// onError handles an error notification about the share, its room or its pub PC.
func (s *Share) onError(e protocol.Error) *endRequest {
	switch e.Scope {
	case protocol.ErrorScopeShare:
		if e.ShareID == "" || e.ShareID == s.params.ShareID {
			return &endRequest{info: EndInfo{Reason: EndError, Protocol: e.Code}, stop: true, leave: true}
		}
	case protocol.ErrorScopeRoom:
		if e.RoomID == "" || e.RoomID == s.req.RoomID {
			reason := protocol.EndReasonLeft
			switch e.Code {
			case protocol.ErrorCodeRoomClosed:
				reason = protocol.EndReasonRoomClosed
			case protocol.ErrorCodeKicked:
				reason = protocol.EndReasonKicked
			}
			return &endRequest{info: EndInfo{Reason: EndServer, Server: reason}}
		}
	case protocol.ErrorScopePC:
		if info := s.onPCError(e); info != nil {
			return &endRequest{info: *info, stop: true, leave: true}
		}
	}
	return nil
}

// onEngineEvent applies an engine event (§11.5). The first EventLayer sets the profile the Publisher sends in and
// starts the pub PC.
func (s *Share) onEngineEvent(ev media.Event) *EndInfo {
	now := time.Now()
	switch ev.Code {
	case media.EventLayer:
		first := len(s.seen) == 0
		if !s.seen[ev.Layer] {
			s.seen[ev.Layer] = true
		}
		s.ctl.SetLive(ev.Layer, ev.LiveBitrate, now)
		if ev.Layer == protocol.RIDHigh {
			s.ctl.SetTargetHeight(ev.Height)
		}
		s.update(func(snap *ShareSnapshot) {
			snap.NoOSIndicator = ev.NoOSIndicator
			if l := snap.layer(ev.Layer); l != nil {
				l.Width, l.Height, l.Encoder, l.HW, l.Profile = ev.Width, ev.Height, ev.Encoder, ev.HW, ev.Profile
			}
			if ev.Layer == protocol.RIDHigh {
				snap.Encoder, snap.HW = ev.Encoder, ev.HW
			}
		})
		// The profile the session runs (the first EventLayer's is the Publisher's), and the codec switch's check.
		s.onLayerProfile(ev)
		if first && !s.rec.republish { // a republish that is due starts the Publisher itself
			if err := s.startPublisher(); err != nil {
				if s.ctx.Err() != nil {
					// end cut the offer short: the share ends with the EndInfo it asks for, which the actor takes next.
					return nil
				}
				s.log.Warn("pub PC start failed", logx.Err(err))
				return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}
			}
		}
	case media.EventLimit:
		s.limits[ev.Layer] = ev.Limit
		s.ctl.SetLimit(shareLimit(s.limits), now)
		s.update(func(snap *ShareSnapshot) {
			if l := snap.layer(ev.Layer); l != nil {
				l.Limit = ev.Limit
			}
			snap.Overloaded = s.ctl.CPU(now)
		})
	case media.EventEncoderChanged:
		s.update(func(snap *ShareSnapshot) {
			if l := snap.layer(ev.Layer); l != nil {
				l.Encoder = ev.Encoder
			}
			if ev.Layer == protocol.RIDHigh {
				snap.Encoder = ev.Encoder
			}
		})
		s.c.events.publish(func() Event {
			return Event{Kind: EventNotice, Notice: &Notice{Code: NoticeEncoderFallback,
				Params: map[string]string{"layer": ev.Layer, "encoder": ev.Encoder}}}
		})
	case media.EventSourceMinimized, media.EventSourceRestored:
		s.update(func(snap *ShareSnapshot) { snap.Minimized = ev.Code == media.EventSourceMinimized })
	default:
		// EventSourceResized, EventCaptureInterrupted/Resumed, EventDeviceReset: logged; new sizes reach the snapshot
		// with the EventLayer of a re-init. Reserved and unknown codes are dropped.
		s.log.Debug("engine event", slog.Int("code", int(ev.Code)), slog.String("layer", ev.Layer))
	}
	return nil
}

// shareLimit is ratectl.SetLimit's share-wide value: cpu or gpu while any layer's last EventLimit is, else none.
func shareLimit(limits map[string]string) string {
	out := "none"
	for _, l := range limits {
		switch l {
		case "cpu":
			return "cpu"
		case "gpu":
			out = "gpu"
		}
	}
	return out
}

// startPublisher makes the share's Publisher (§11.4 step 4): its layers are the rids of the share's encodings (the
// ones the rate controller has off are turned off), its profile the one the session runs, the other profiles of §8.6
// offered after it, with a codec hint's profile (or the server's policy after a republish) put first, the video paced
// at the applied targets; then the first offer goes out.
func (s *Share) startPublisher() error {
	var available []string
	for _, e := range s.probe.Encoders {
		if e.Tested {
			available = append(available, e.Profiles...)
		}
	}
	profile := s.running
	if profile != highProfile && profile != cbProfile {
		profile = engineProfile(s.params.Codec, firstEncoder(s.probe, s.c.o.Encoder))
	}
	var layers []string
	for _, e := range s.params.Encodings {
		layers = append(layers, e.RID)
	}
	s.cons = &consumer{p: s.pump, ask: &s.ask, failed: func(p string) { s.post(msgProfileFailed{profile: p}) }}
	pub, err := publish.New(publish.Options{
		Source:     s.cons,
		Settings:   s.c.settings,
		ICEServers: s.c.iceServers(),
		Layers:     layers,
		Profile:    profile,
		Profiles:   publish.OfferProfiles(profile, available),
		Audio:      s.req.Audio,
		Logger:     s.log,
		Pacing:     true,
	})
	if err != nil {
		return err
	}
	active := s.ctl.State().Active
	for _, rid := range layers {
		if on, ok := active[rid]; ok && !on {
			_ = pub.SetActive(rid, false)
		}
	}
	if want := s.sendProfile; want != "" && want != profile {
		if err := pub.SetProfile(want); err != nil {
			s.log.Info("publisher profile", slog.String("profile", want), logx.Err(err))
			s.sendProfile = ""
		}
	}
	pub.SetPacingTargets(s.videoTargets())
	s.mu.Lock()
	s.pub = pub
	s.mu.Unlock()
	return s.offer(false)
}

// videoTargets returns the applied bitrate of every video layer, for Publisher.SetPacingTargets.
func (s *Share) videoTargets() map[string]int {
	targets := map[string]int{}
	for rid, bps := range s.ctl.State().Applied {
		if rid != media.LayerAudio {
			targets[rid] = bps
		}
	}
	return targets
}

// finish ends the share (§11.4 "Stop"): media stops leaving at once; share.stop goes out and the engine stops
// while its reply is awaited (capture ends within one frame interval, §11.2, not after the reply's wait of up to
// ctx's end); then pc.close, the Publisher closed, room.leave when the connection has no other share. On the wire
// the order stays share.stop → pc.close → room.leave. Requests that fail are logged: the local part always runs.
func (s *Share) finish(req endRequest) {
	ctx := req.ctx
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
	}
	s.rec.stopHold()
	s.pump.halt()
	var stopped chan error // share.stop's result
	if req.stop {
		stopped = make(chan error, 1)
		go func() {
			stopped <- s.c.request(ctx, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: s.params.ShareID}, nil)
		}()
	}
	s.pump.stop() // capture ends now; the halted pump puts nothing on the wire
	if stopped != nil {
		if err := <-stopped; err != nil {
			s.log.Debug("share.stop", logx.Err(err))
		}
	}
	s.closePublisher(true) // its consumer's detach is safe on a stopped pump
	s.cancel()
	if req.leave {
		s.c.roomMu.Lock()
		if s.c.roomID() == s.req.RoomID {
			if err := s.c.leave(ctx); err != nil {
				s.log.Debug("room.leave", logx.Err(err))
			}
		}
		s.c.roomMu.Unlock()
	}
	s.over(req.info)
}

// closePublisher closes the share's Publisher, after pc.close when tell (the server then drops the pub PC at once),
// and forgets it: a rebuild or a republish makes the next one.
func (s *Share) closePublisher(tell bool) {
	s.mu.Lock()
	pub := s.pub
	s.pub = nil
	s.mu.Unlock()
	if pub == nil {
		return
	}
	if tell && s.drv.gen > 0 {
		if err := s.c.send(protocol.MessageTypePCClose, protocol.PCClose{PC: protocol.PCKindPub, Gen: s.drv.gen}); err != nil {
			s.log.Debug("pc.close", logx.Err(err))
		}
	}
	if err := pub.Close(); err != nil {
		s.log.Debug("publisher close", logx.Err(err))
	}
	if s.cons != nil {
		s.pump.detach(s.cons)
		s.cons = nil
	}
}

// iceServers returns the welcome's ICE servers as Pion's (M1 sends none).
func (c *Client) iceServers() []webrtc.ICEServer {
	c.mu.Lock()
	sig := c.sig
	c.mu.Unlock()
	if sig == nil {
		return nil
	}
	var out []webrtc.ICEServer
	for _, s := range sig.Welcome().ICEServers {
		out = append(out, webrtc.ICEServer{URLs: slices.Clone(s.URLs), Username: s.Username,
			Credential: s.Credential.Reveal()})
	}
	return out
}

// StopOther stops another share of this user (the SPA's role viewer may not, 01 §6.3): (1) if the Go connection is
// in roomID, it sends share.stop; (2) if it is in no room, it joins roomID (the same Participant as the SPA, so
// presence doesn't change), sends share.stop, then leaves; (3) if it is in another room, ErrBusy. It returns nil
// only once room.state no longer lists shareID: share.stop answers ok for unknown ids, and M1's server looks the
// share up only in the caller's current room (internal/server/signal/share.go, onShareStop with requireRoom).
func (c *Client) StopOther(ctx context.Context, roomID, shareID string) error {
	if !c.isOnline() {
		return ErrNotReady
	}
	if roomID == "" || shareID == "" {
		return errors.New("core: stop other: no room or share")
	}
	if !c.startOp() {
		return ErrNotReady
	}
	defer c.ops.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.life, cancel)()

	c.roomMu.Lock()
	defer c.roomMu.Unlock()
	switch cur := c.roomID(); cur {
	case roomID:
	case "":
		joined := true // a room.join went out: whatever its reply, StopOther leaves (as an aborted start does)
		defer func() {
			lctx, lcancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer lcancel()
			if err := c.leaveRoom(lctx, joined); err != nil {
				c.log.Debug("room.leave after stopping another share", logx.Err(err))
			}
		}()
		if err := c.join(ctx, roomID); err != nil {
			var perr *protocol.Error
			if errors.As(err, &perr) {
				joined = false // the server refused it: the connection is in no room
			}
			return requestErr(ctx, err)
		}
		// The room.state that follows the join tells what the room has.
		if err := c.waitRoom(ctx, roomID, func(_ protocol.RoomState, seq uint64) bool { return seq > 0 }); err != nil {
			return err
		}
	default:
		return ErrBusy
	}
	if err := c.request(ctx, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: shareID}, nil); err != nil {
		return requestErr(ctx, err)
	}
	return c.waitRoom(ctx, roomID, func(st protocol.RoomState, seq uint64) bool {
		return seq > 0 && !slices.ContainsFunc(st.Shares, func(s protocol.ShareInfo) bool { return s.ID == shareID })
	})
}
