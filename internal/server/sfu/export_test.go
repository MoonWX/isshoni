package sfu

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// This file gives the tests in package sfu_test (the ones that use sfutest.Harness, which imports this package) a
// look at the state a Conn's actor owns.

// PubTrackState describes one incoming track of a Conn's pub PC.
type PubTrackState struct {
	MID, RID string
	Kind     webrtc.RTPCodecType
	Share    ShareID // "" while the track feeds no share
	Packets  uint64  // RTP packets read so far
}

// PubTracks returns the tracks Pion has delivered on the Conn's current pub PC, sorted by mid and rid.
func (c *Conn) PubTracks(ctx context.Context) ([]PubTrackState, error) {
	var out []PubTrackState
	err := c.do(ctx, func(context.Context) error {
		if c.pub == nil {
			return nil
		}
		for key, t := range c.pub.tracks {
			st := PubTrackState{MID: key.mid, RID: key.rid, Kind: t.kind, Packets: t.packets.Load()}
			if l := t.layer.Load(); l != nil {
				st.Share = l.share.id
			}
			out = append(out, st)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b PubTrackState) int {
		return strings.Compare(a.MID+"/"+a.RID, b.MID+"/"+b.RID)
	})
	return out, err
}

// PubStuck reports whether the Conn's current pub PC is stuck: an offer failed inside Pion after Pion had taken it,
// so the PC takes no other one. It is false without a pub PC.
func (c *Conn) PubStuck(ctx context.Context) (bool, error) {
	var stuck bool
	err := c.do(ctx, func(context.Context) error {
		stuck = c.pub != nil && c.pub.stuck
		return nil
	})
	return stuck, err
}

// SubscriptionState describes one subscription of a Conn.
type SubscriptionState struct {
	Share ShareID
	Video Quality
	Audio bool
	// Forwarded, AudioForwarded and Reason are what the subscription gets now, as a SubscriptionStateEvent says it;
	// Layer is the rid of the video layer forwarded, "" when none is.
	Forwarded      Quality
	AudioForwarded bool
	Reason         SubReason
	Layer          string
	// VideoBound and AudioBound report whether Pion has bound the DownTrack to a negotiated sender.
	VideoBound, AudioBound bool
	// VideoPTs maps the H.264 profiles the viewer can receive to its payload types (the video binding's ptFor).
	VideoPTs map[ProfileKey]uint8
	// VideoSent and AudioSent count the RTP packets written to the viewer; VideoDrops and AudioDrops those a full
	// DownTrack queue dropped.
	VideoSent, AudioSent, VideoDrops, AudioDrops uint64
	// VideoRepair and AudioRepair are what the DownTracks did about the viewer's NACKs, and the sender reports they
	// forwarded.
	VideoRepair, AudioRepair RepairState
}

// RepairState are a DownTrack's repair counters (02 §9.5): of the sequence numbers the viewer's NACKs asked for
// (NACKs), how many were retransmitted (Served), were no longer in a cache (Missed), were skipped (asked for again
// too soon or too often, or over budget) or met a full RTX queue (Dropped). SRs counts the sender reports forwarded
// to the viewer (02 §9.6).
type RepairState struct {
	NACKs, Served, Missed, Skipped, Dropped uint64
	SRs                                     uint64
}

func (d *DownTrack) repairState() RepairState {
	return RepairState{
		NACKs: d.stats.nackReceived.Load(), Served: d.stats.nackServed.Load(), Missed: d.stats.nackMissed.Load(),
		Skipped: d.stats.rtxSkipped.Load(), Dropped: d.stats.rtxDrops.Load(), SRs: d.stats.srs.Load(),
	}
}

// LayerState are the ingress counters of one attached layer of a share.
type LayerState struct {
	// Packets counts the RTP packets read; Duplicates the copies of a packet the cache already held (the publisher's
	// redundant RTX, 02 §9.1: isshoni_sfu_ingress_duplicates_total), which were not forwarded; TooLate the packets a
	// cache capacity or more behind.
	Packets, Duplicates, TooLate uint64
}

// Ingress returns the counters of the layers attached to a share now, by rid ("audio" for the audio layer); nil for
// an unknown or ended share.
func (s *SFU) Ingress(id ShareID) map[string]LayerState {
	sh := s.lookupShare(id)
	if sh == nil {
		return nil
	}
	out := map[string]LayerState{}
	for _, l := range sh.attached() {
		out[l.slot.String()] = LayerState{
			Packets: l.stats.packets.Load(), Duplicates: l.stats.duplicates.Load(), TooLate: l.stats.tooLate.Load(),
		}
	}
	return out
}

// Subscriptions returns the Conn's subscriptions, sorted by share id.
func (c *Conn) Subscriptions(ctx context.Context) ([]SubscriptionState, error) {
	var out []SubscriptionState
	err := c.do(ctx, func(context.Context) error {
		for id, sub := range c.subs {
			st := SubscriptionState{Share: id, Video: sub.reqVideo, Audio: sub.reqAudio}
			now := sub.state()
			st.Forwarded, st.AudioForwarded, st.Reason = now.forwarded, now.audio, now.reason
			sub.video.mu.Lock()
			if slot, ok := sub.video.m.current(); ok {
				st.Layer = slot.String()
			}
			sub.video.mu.Unlock()
			if b := sub.video.binding.Load(); b != nil {
				st.VideoBound, st.VideoPTs = true, b.ptFor
			}
			st.AudioBound = sub.audio.binding.Load() != nil
			st.VideoSent, st.VideoDrops = sub.video.stats.packets.Load(), sub.video.stats.drops.Load()
			st.AudioSent, st.AudioDrops = sub.audio.stats.packets.Load(), sub.audio.stats.drops.Load()
			st.VideoRepair, st.AudioRepair = sub.video.repairState(), sub.audio.repairState()
			out = append(out, st)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b SubscriptionState) int { return strings.Compare(string(a.Share), string(b.Share)) })
	return out, err
}

// FanOut returns how many video and audio DownTracks a share fans out to; ok is false for an unknown or ended share.
func (s *SFU) FanOut(id ShareID) (video, audio int, ok bool) {
	sh := s.lookupShare(id)
	if sh == nil {
		return 0, 0, false
	}
	return len(sh.downTracks(webrtc.RTPCodecTypeVideo)), len(sh.downTracks(webrtc.RTPCodecTypeAudio)), true
}

// PCState describes one of a Conn's two PeerConnections as its actor has it.
type PCState struct {
	Exists bool   // the Conn has a PC of the kind (a closed sub PC exists; a closed pub PC is gone)
	Gen    uint32 // of that PC; for pub without a PC, the highest gen accepted
	// State is the PeerConnection's connection state as Pion has it now ("" without a PC).
	State string
	// Neg is the neg of the last offer: the last one answered on the pub PC, the last one sent on the sub PC. Closed
	// and Offering are the sub PC's negotiation state: closed for good, an offer outstanding.
	Neg              uint32
	Closed, Offering bool
	// HandshakeTimer and GraceTimer report whether the PC's handshake timeout and its grace after failed are running.
	HandshakeTimer, GraceTimer bool
	// RTT is the sub PC's round-trip time as its DownTracks' RTCP readers last read it from the selected candidate
	// pair (02 §9.3); 0 while unknown, and for the pub PC.
	RTT time.Duration
}

// PC returns the state of the Conn's PeerConnection of a kind.
func (c *Conn) PC(ctx context.Context, kind PCKind) (PCState, error) {
	var st PCState
	err := c.do(ctx, func(context.Context) error {
		switch {
		case kind == PCPub && c.pub != nil:
			p := c.pub
			st = PCState{
				Exists: true, Gen: p.gen, State: p.pc.ConnectionState().String(), Neg: p.neg,
				HandshakeTimer: p.handshake.pending(), GraceTimer: p.grace.pending(),
			}
		case kind == PCPub:
			st.Gen = c.pubGen
		case kind == PCSub && c.sub != nil:
			s := c.sub
			st = PCState{
				Exists: true, Gen: s.gen, State: s.pc.ConnectionState().String(), Closed: s.closed, Offering: s.offering,
				Neg: s.neg, HandshakeTimer: s.handshake.pending(), GraceTimer: s.grace.pending(),
				RTT: time.Duration(s.rtt.Load()),
			}
		}
		return nil
	})
	return st, err
}

// Timings are the durations of PeerConnection recovery that a test may shorten (config.go): the handshake timeout
// (10 s), the grace after failed (30 s), the sub offer re-send (15 s), the ICE restart spacing (5 s), the window of
// the PC creation limit (1 min), the stalled threshold (2 s) and the time between two codec rebuilds for a viewer
// whose answers refuse H.264 (20 s); and the life of a connection-test probe (20 s).
type Timings struct {
	Handshake, Grace, OfferResend, ICERestart, PCWindow, Stalled, CodecRetry, ProbeLife time.Duration
}

// SetTimings replaces the durations of t that aren't zero. Call it before the SFU's first Join or Probe.
func (s *SFU) SetTimings(t Timings) {
	for _, d := range []struct {
		to   *time.Duration
		from time.Duration
	}{
		{&s.timing.handshake, t.Handshake}, {&s.timing.grace, t.Grace}, {&s.timing.offerResend, t.OfferResend},
		{&s.timing.iceRestart, t.ICERestart}, {&s.timing.pcWindow, t.PCWindow}, {&s.timing.stalled, t.Stalled},
		{&s.timing.codecRetry, t.CodecRetry}, {&s.timing.probeLife, t.ProbeLife},
	} {
		if d.from != 0 {
			*d.to = d.from
		}
	}
}

// DefaultTimings returns the durations an SFU uses unless a test shortens them: the numbers of 02 §12.
func DefaultTimings() Timings {
	d := defaultTimings()
	return Timings{
		Handshake: d.handshake, Grace: d.grace, OfferResend: d.offerResend, ICERestart: d.iceRestart,
		PCWindow: d.pcWindow, Stalled: d.stalled, CodecRetry: d.codecRetry, ProbeLife: d.probeLife,
	}
}

// MaxProbes is how many connection-test probes one server runs at once (02 §7.6).
const MaxProbes = maxProbes

// Probes returns how many connection-test probes hold a slot, and how many probe goroutines still run (the replaced
// ones that are still closing included).
func (s *SFU) Probes() (slots, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.probes), len(s.probing)
}

// HasProbe reports whether a probe of the user and transport holds a slot.
func (s *SFU) HasProbe(user UserID, t ProbeTransport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes[probeKey{user: user, t: t}] != nil
}

// MaxPCCreations is the number of PeerConnections of one kind a client may make a Conn create within a minute.
const MaxPCCreations = maxPCCreations

// CodecRetries is how many codec rebuilds the SFU makes for a viewer whose answers refuse H.264 (02 §8.5), and
// CodecPolicyHold how long a room's codec policy stays cb after the last viewer that needed it (02 §8.3).
const (
	CodecRetries    = codecRetries
	CodecPolicyHold = codecPolicyHold
)

// AdvanceCodecClock moves the clock of the SFU's codec policies on by d: the fake clock of 02 §17, with which a test
// sees a policy's hold pass without waiting for it. The ticker looks at the policies once a second;
// CheckCodecPolicies looks at once.
func (s *SFU) AdvanceCodecClock(d time.Duration) { s.clockSkew.Add(int64(d)) }

// CheckCodecPolicies brings every room's codec policy up to date now, as the ticker does once a second.
func (s *SFU) CheckCodecPolicies() {
	s.mu.Lock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.mu.Unlock()
	for _, r := range rooms {
		s.updateCodecPolicy(r)
	}
}

// DecoderState describes what a Conn knows of its viewer's H.264 decoders (02 §8.5).
type DecoderState struct {
	H264     bool // the viewer's caps list some H.264
	Refused  bool // an answer of the viewer refused H.264: its video waits for a codec rebuild
	Rebuilds int  // codec rebuilds since the caps last changed or the viewer last took H.264
	Failed   bool // all of them done, and refused still: decoder_failed
	Retry    bool // the next codec rebuild is set
}

// Decoders returns the Conn's DecoderState.
func (c *Conn) Decoders(ctx context.Context) (DecoderState, error) {
	var st DecoderState
	err := c.do(ctx, func(context.Context) error {
		st = DecoderState{
			H264: c.decodesH264(), Refused: c.codec.refused, Rebuilds: c.codec.retries, Failed: c.codec.failed,
			Retry: c.codec.retry.pending(),
		}
		return nil
	})
	return st, err
}

// SetMediaICETimeouts replaces the ICE timeouts of the pub and sub PCs of every SFU built from now on, and returns
// the function that puts the real ones back (02 §7.3: 5 s to disconnected, 15 s more to failed). A test that needs a
// PC to fail can't wait that long.
func SetMediaICETimeouts(disconnected, failed, keepalive time.Duration) (restore func()) {
	old := mediaICETimeouts
	mediaICETimeouts = iceTimeouts{disconnected: disconnected, failed: failed, keepalive: keepalive}
	return func() { mediaICETimeouts = old }
}

// DownlinkState describes a Conn's downlink allocator (02 §10.2).
type DownlinkState struct {
	State   string        // "normal", "limited" or "probing"
	Budget  int64         // bit/s; what the high requests are fitted into while limited
	Backoff time.Duration // before the next trial upgrade
	Cause   string        // why it capped last: "remb", "loss" or "drops"
}

// Downlink returns the state of the Conn's downlink allocator.
func (c *Conn) Downlink(ctx context.Context) (DownlinkState, error) {
	var st DownlinkState
	err := c.do(ctx, func(context.Context) error {
		a := &c.alloc
		st = DownlinkState{State: a.state.String(), Budget: a.budget, Backoff: a.backoff, Cause: a.cause}
		return nil
	})
	return st, err
}

// The downlink allocator's first backoff and the length of a trial upgrade (02 §10.2).
const (
	BackoffStart = backoffStart
	ProbeTime    = probeTime
)

// PauseAfter is how long a share's full layer goes without a request for high before it is paused (02 §11), and
// REMBLifts and REMBUncapped are the REMBs of no limit that a pub PC gets once its cap is gone.
const (
	PauseAfter   = pauseAfter
	REMBLifts    = rembLifts
	REMBUncapped = rembUncapped
)

// AdvanceUplinkClock moves the clock of the SFU's uplink policies on by d: the fake clock with which a test sees the
// 10 s of layer pausing pass without waiting for them (02 §11). The ticker has every publishing Conn look at its
// uplink once a second; CheckUplink looks at once.
func (s *SFU) AdvanceUplinkClock(d time.Duration) { s.uplinkSkew.Add(int64(d)) }

// CheckUplink has the Conn look at the uplink of the shares it publishes now, as the ticker has it do once a second:
// each share's policy with its hints, then the pub PC's REMB.
func (c *Conn) CheckUplink(ctx context.Context) error {
	return c.do(ctx, func(context.Context) error {
		c.uplinkTick()
		return nil
	})
}

// UplinkStats returns what a share's uplink policy decides by as of the ticker's last second (M2 01 §10.1: with each
// layer's raw loss and queueing delay); false for an unknown or ended share.
func (s *SFU) UplinkStats(id ShareID) (ShareUplinkStats, bool) {
	sh := s.lookupShare(id)
	if sh == nil {
		return ShareUplinkStats{}, false
	}
	return sh.uplinkStats(sh.conn.client), true
}

// UplinkState is a share's uplink control as its publishing Conn's actor has it (02 §11).
type UplinkState struct {
	MaxBitrate int      // the cap the sharer was told last, bit/s
	Paused     []string // the rids of the layers it was told to pause
	Reason     string   // of the last hint; "" before the first
}

// Uplink returns the uplink control of a share the Conn publishes; false for another Conn's share or one that ended.
func (c *Conn) Uplink(ctx context.Context, id ShareID) (UplinkState, bool, error) {
	var (
		st UplinkState
		ok bool
	)
	err := c.do(ctx, func(context.Context) error {
		for _, sh := range c.ownShares() {
			if sh.id == id {
				st, ok = UplinkState{MaxBitrate: sh.up.capBps, Paused: pausedRIDs(sh.up.paused), Reason: sh.up.reason}, true
			}
		}
		return nil
	})
	return st, ok, err
}
