package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/version"
)

// A simulated client is one friend in the default room, as a browser would be (01 §9, §11): a signaling connection
// with the account's session cookie, and the PeerConnections of what the friend does there. A publisher shares the
// fake media through a publish.Publisher on its pub PC; a subscriber watches through an sfutest.Viewer, which answers
// the server's sub offers and records every track.
//
// It keeps to the protocol where a load test meets it: sub offers are answered by their gen and neg (01 §9), a
// resumed connection sends its whole desired subscription set again (01 §10.5), and every notification is received,
// since a signaling client whose events nobody reads stops reading its socket (01 §15.3). It does not recover from
// what a healthy run never sees (a connection the server no longer had, a pub PC the server asks to rebuild, a
// server shutdown): each is a problem of the run, which fails it.

// Timeouts of a client's own steps.
const (
	dialTimeout   = 30 * time.Second
	answerTimeout = 30 * time.Second
	// recorderKeep bounds the packets each of a Viewer's Recorders keeps: about 10 s of an 8 Mbps layer, which is
	// what a switch and the A/V pairs are read from, without holding a whole run per track in memory (02 §15.2's
	// 10×10 has 110 tracks). The counters cover every packet.
	recorderKeep = 1 << 13
)

// caps are the clients' capabilities: what publish.Publisher sends and sfutest.Viewer decodes.
var caps = protocol.Caps{
	Decode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
	Encode:         []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264High, protocol.CodecOpus},
	Simulcast:      true,
	DisplayCapture: true,
}

// client is one simulated friend.
type client struct {
	lt    *loadTest
	acct  *account
	index int // among the publishers, or among the subscribers
	log   *slog.Logger
	sig   *signal.Client
	alive sync.WaitGroup // the pump and the state watcher: they end when sig has stopped

	// A publisher's share (nil for a subscriber), a subscriber's Viewer (nil for a publisher).
	share  *share
	viewer *sfutest.Viewer

	answers chan protocol.PCAnswer // the server's answers to the pub offer
	pcErrs  chan protocol.Error    // error notifications about the pub offer

	subMu sync.Mutex // one sub offer at a time
	sub   subPC

	mu      sync.Mutex // guards the fields below
	closing bool
	focus   int // a subscriber's focused publisher (scenario.go)
	room    protocol.RoomState
	status  map[string]protocol.SubscriptionStatus // the last subscribe.status entry by share
	wants   map[string]protocol.SubscriptionWant   // the desired subscriptions by share (01 §8.9)
}

// subPC is what the client knows of its sub PeerConnection: the gen and neg of the last offer it answered, and the
// answer it sent, which a repeat of that offer gets again (01 §9 rules 2 and 3).
type subPC struct {
	gen, neg uint32
	answer   string
}

// share is a publisher's share: the hub's id, the fake media and the Publisher that sends it.
type share struct {
	id  string
	src *fake.Source
	pub *publish.Publisher
}

// connect opens the account's signaling connection with role and joins the default room. A subscriber gets its
// Viewer here, before its first subscription asks the server for a sub offer.
func (lt *loadTest) connect(ctx context.Context, a *account, role protocol.Role, index int) (*client, error) {
	c := &client{
		lt: lt, acct: a, index: index, log: lt.log.With("client", a.name),
		answers: make(chan protocol.PCAnswer, 4), pcErrs: make(chan protocol.Error, 4),
		status: map[string]protocol.SubscriptionStatus{}, wants: map[string]protocol.SubscriptionWant{},
	}
	if role == protocol.RoleViewer {
		v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: lt.settings(), KeepPackets: recorderKeep, Logger: c.log})
		if err != nil {
			return nil, fmt.Errorf("%s: the Viewer: %w", a.name, err)
		}
		c.viewer = v
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	sig, err := signal.Dial(dctx, signal.Options{
		URL:        lt.wsURL,
		HTTPClient: a.rest.hc, // its jar has the session cookie
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: version.Version(), OS: clientOS()},
		Role:       role,
		Caps:       caps,
		Logger:     c.log,
	})
	if err != nil {
		c.closeMedia()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, setupErrorf("%s: connecting to %s: %w", a.name, lt.wsURL, err)
	}
	c.sig = sig
	c.alive.Go(c.pump)
	c.alive.Go(c.watchStates)
	room := sig.Welcome().DefaultRoomID
	var joined protocol.RoomJoinResult
	if err := c.request(ctx, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room}, &joined); err != nil {
		c.close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, setupErrorf("%s: joining the room %s: %w", a.name, room, err)
	}
	return c, nil
}

// clientOS is the hello's client.os for this machine.
func clientOS() protocol.ClientOS {
	switch runtime.GOOS {
	case "linux":
		return protocol.ClientOSLinux
	case "darwin":
		return protocol.ClientOSMacOS
	case "windows":
		return protocol.ClientOSWindows
	}
	return protocol.ClientOSOther
}

// settings returns the Pion settings of the clients' PeerConnections: no mDNS candidates, Pion's logs through
// the tool's logger, and, against a server on this machine, loopback UDP only, which is where it listens.
func (lt *loadTest) settings() webrtc.SettingEngine {
	var se webrtc.SettingEngine
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.LoggerFactory = lt.pionLog
	if lt.loopback {
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
		se.SetIncludeLoopbackCandidate(true)
		se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	}
	return se
}

// request sends a request and decodes its reply into result.
func (c *client) request(ctx context.Context, typ protocol.MessageType, data, result any) error {
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	if err := c.sig.Request(ctx, typ, data, result); err != nil {
		return fmt.Errorf("%s: %w", typ, err)
	}
	return nil
}

// problem records something that went wrong for this client, unless the client is closing.
func (c *client) problem(format string, args ...any) {
	c.mu.Lock()
	closing := c.closing
	c.mu.Unlock()
	if !closing {
		c.lt.problem(c.acct.name + ": " + fmt.Sprintf(format, args...))
	}
}

// pump receives the client's notifications until the signaling client has stopped.
func (c *client) pump() {
	for env := range c.sig.Events() {
		if err := c.handle(env); err != nil {
			c.problem("%s: %v", env.Type, err)
		}
	}
}

func (c *client) handle(env protocol.Envelope) error {
	switch env.Type {
	case protocol.MessageTypeRoomState:
		st, err := protocol.Decode[protocol.RoomState](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.room = st
		c.mu.Unlock()
	case protocol.MessageTypeSubscribeStatus:
		st, err := protocol.Decode[protocol.SubscribeStatus](env)
		if err != nil {
			return err
		}
		c.mu.Lock()
		for _, s := range st.Subs {
			c.status[s.ShareID] = s
		}
		c.mu.Unlock()
	case protocol.MessageTypePCAnswer:
		a, err := protocol.Decode[protocol.PCAnswer](env)
		if err != nil {
			return err
		}
		if a.PC == protocol.PCKindPub {
			select {
			case c.answers <- a:
			default: // nobody waits for it: the answer to an offer the client gave up on
			}
		}
	case protocol.MessageTypePCOffer:
		o, err := protocol.Decode[protocol.PCOffer](env)
		if err != nil {
			return err
		}
		return c.answerSubOffer(o)
	case protocol.MessageTypePCRestart:
		r, err := protocol.Decode[protocol.PCRestart](env)
		if err != nil {
			return err
		}
		c.problem("the server asked to %s the %s PC (gen %d, reason %s); a load test client does not recover",
			r.Mode, r.PC, r.Gen, r.Reason)
	case protocol.MessageTypeQualityHint:
		h, err := protocol.Decode[protocol.QualityHint](env)
		if err != nil {
			return err
		}
		// No rate control in M1 (02 §15.1), and the fake source has one profile: a hint changes nothing.
		c.log.Debug("quality.hint ignored", "share_id", h.ShareID, "reason", h.Reason, "codec", h.Codec)
	case protocol.MessageTypeError:
		e, err := protocol.Decode[protocol.Error](env)
		if err != nil {
			return err
		}
		if e.Scope == protocol.ErrorScopePC && e.PC == protocol.PCKindPub {
			select {
			case c.pcErrs <- e:
			default:
			}
		}
		c.problem("the server sent error %s (scope %s, share %q, pc %q)", e.Code, e.Scope, e.ShareID, e.PC)
	case protocol.MessageTypeServerShutdown:
		c.problem("the server is shutting down")
	}
	return nil
}

// watchStates follows the signaling client after its first welcome (Dial's). A resumed connection sends its desired
// subscriptions again (01 §10.5); a connection that went away for good, or a welcome that did not resume it, is a
// problem: the server lost the client's state, and this client does not start over.
func (c *client) watchStates() {
	welcomed := false
	for sc := range c.sig.States() {
		if !welcomed {
			welcomed = sc.State == signal.StateReady
			continue
		}
		switch {
		case sc.State == signal.StateStopped:
			c.problem("the signaling connection stopped (%v)", sc.Err)
		case sc.State == signal.StateBackoff:
			c.log.Warn("the signaling connection dropped", "err", sc.Err, "retry_in", sc.Delay.String())
		case sc.State == signal.StateReady && !sc.Resumed:
			c.problem("the server did not resume the signaling connection")
		case sc.State == signal.StateReady:
			c.lt.resumed.Add(1)
			c.log.Info("the signaling connection resumed")
			c.mu.Lock()
			wants := slices.Collect(maps.Values(c.wants))
			c.mu.Unlock()
			if len(wants) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
				var res protocol.SubscribeResult
				err := c.sig.Request(ctx, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}, &res)
				cancel()
				if err != nil {
					c.problem("the desired subscriptions after a resume: %v", err)
				}
			}
		}
	}
}

// answerSubOffer answers a sub offer with the client's Viewer as 01 §9 says: an older offer is dropped, the repeat
// of the offer last answered gets the same answer, and an offer of a new gen gets a new PeerConnection.
func (c *client) answerSubOffer(o protocol.PCOffer) error {
	if o.PC != protocol.PCKindSub {
		return fmt.Errorf("an offer for the %s PC: the server offers on sub only", o.PC)
	}
	if c.viewer == nil {
		return errors.New("a sub offer to a publisher, which subscribes to nothing")
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()
	cur := c.sub
	switch {
	case o.Gen < cur.gen, o.Gen == cur.gen && o.Neg < cur.neg:
		return nil
	case o.Gen == cur.gen && o.Neg == cur.neg:
		return c.sig.Send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(cur.answer)})
	case o.Gen > cur.gen && cur.gen != 0:
		if err := c.viewer.Rebuild(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
	defer cancel()
	answer, err := c.viewer.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP.Reveal()})
	if err != nil {
		return err
	}
	c.sub = subPC{gen: o.Gen, neg: o.Neg, answer: answer.SDP}
	return c.sig.Send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(answer.SDP)})
}

// publish starts the client's share as a browser does (01 §11.2): share.start, a pub offer whose tracks name the
// share, the server's answer, and then the fake media, which runs until media ends. seed makes each share's
// synthetic video its own.
func (c *client) publish(ctx, media context.Context, seed uint64) error {
	var params protocol.ShareParams
	start := protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true, Ref: "lt"}
	if err := c.request(ctx, protocol.MessageTypeShareStart, start, &params); err != nil {
		return fmt.Errorf("%s: %w", c.acct.name, err)
	}
	cfg := c.lt.cfg
	mode := fake.Synthetic
	if cfg.decodable {
		mode = fake.Decodable
	}
	src, err := fake.New(fake.Config{
		Mode: mode, Profile: cfg.profile, Layers: slices.Clone(cfg.layers), GOP: cfg.gop, Audio: true, Seed: seed,
	})
	if err != nil {
		return fmt.Errorf("%s: the fake media: %w", c.acct.name, err)
	}
	rids := make([]string, len(cfg.layers))
	for i, l := range cfg.layers {
		rids[i] = l.RID
	}
	pub, err := publish.New(publish.Options{
		Source: src, Settings: c.lt.settings(), Layers: rids, Profile: cfg.profile, Audio: true, Logger: c.log,
	})
	if err != nil {
		_ = src.Close()
		return fmt.Errorf("%s: the Publisher: %w", c.acct.name, err)
	}
	c.mu.Lock()
	c.share = &share{id: params.ShareID, src: src, pub: pub}
	c.mu.Unlock()

	octx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	offer, err := pub.Offer(octx)
	if err != nil {
		return fmt.Errorf("%s: the pub offer: %w", c.acct.name, err)
	}
	o := protocol.PCOffer{PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: protocol.SDP(offer.SDP), Tracks: tracks(pub, params.ShareID)}
	if err := c.sig.Send(protocol.MessageTypePCOffer, o); err != nil {
		return fmt.Errorf("%s: sending the pub offer: %w", c.acct.name, err)
	}
	for {
		select {
		case a := <-c.answers:
			if a.Gen != o.Gen || a.Neg != o.Neg {
				continue
			}
			if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.SDP.Reveal()}); err != nil {
				return fmt.Errorf("%s: the Publisher refused the server's answer: %w", c.acct.name, err)
			}
			if err := pub.Start(media); err != nil {
				return fmt.Errorf("%s: %w", c.acct.name, err)
			}
			return nil
		case e := <-c.pcErrs:
			if e.Gen == o.Gen && e.Neg == o.Neg {
				return fmt.Errorf("%s: the server refused the pub offer: %s", c.acct.name, e.Code)
			}
		case <-octx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: no answer to the pub offer within %v", c.acct.name, answerTimeout)
		}
	}
}

// tracks are the TrackRefs of a pub offer: one per m-section of the Publisher (its simulcast encodings share the
// video's mid), all of them the share's.
func tracks(pub *publish.Publisher, shareID string) []protocol.TrackRef {
	var refs []protocol.TrackRef
	for _, tr := range pub.Tracks() {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: shareID, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// shareLive reports whether the newest room.state shows the client's share live with every layer and its audio:
// the SFU has every simulcast layer and has seen the first keyframe.
func (c *client) shareLive() (protocol.ShareInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.share == nil {
		return protocol.ShareInfo{}, false
	}
	for _, s := range c.room.Shares {
		if s.ID == c.share.id {
			return s, s.Status == protocol.ShareStatusLive && len(s.Layers) == len(c.lt.cfg.layers) && s.Audio
		}
	}
	return protocol.ShareInfo{}, false
}

// subscribe sends subscriptions in one subscribe.update, which the SFU applies in one step, and merges them into the
// client's desired set. It returns when the server has taken them.
func (c *client) subscribe(ctx context.Context, wants ...protocol.SubscriptionWant) error {
	c.mu.Lock()
	for _, w := range wants {
		c.wants[w.ShareID] = w
	}
	c.mu.Unlock()
	var res protocol.SubscribeResult
	if err := c.request(ctx, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}, &res); err != nil {
		return err
	}
	if len(res.Ignored) > 0 {
		return fmt.Errorf("subscribe.update ignored the shares %s", strings.Join(res.Ignored, ", "))
	}
	return nil
}

// recorder returns the newest Recorder of a kind in a share's stream: the SFU sets the msid stream id to the share
// id (01 §9 rule 4). nil before the track's first packet.
func (c *client) recorder(shareID string, kind webrtc.RTPCodecType) *sfutest.Recorder {
	recs := c.viewer.Recorders()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind() == kind && recs[i].StreamID() == shareID {
			return recs[i]
		}
	}
	return nil
}

// close ends the signaling connection, waits for the client's goroutines, then closes its PeerConnections.
func (c *client) close() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	if c.sig != nil {
		if err := c.sig.Close(); err != nil {
			c.log.Debug("closing the signaling connection", "err", err)
		}
		c.alive.Wait()
	}
	c.closeMedia()
}

// closeMedia closes the Publisher and its Source, or the Viewer.
func (c *client) closeMedia() {
	c.mu.Lock()
	sh := c.share
	c.mu.Unlock()
	if sh != nil {
		if err := sh.pub.Close(); err != nil {
			c.log.Debug("closing the Publisher", "err", err)
		}
		_ = sh.src.Close()
	}
	if c.viewer != nil {
		if err := c.viewer.Close(); err != nil {
			c.log.Debug("closing the Viewer", "err", err)
		}
	}
}
