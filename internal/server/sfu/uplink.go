package sfu

import (
	"math"
	"slices"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Publisher-side control (02 §11): what the SFU tells a sharer about its uplink. Each share has an UplinkPolicy, which
// the publishing Conn's actor asks once a second (the ticker wakes it: Conn.uplinkTick), when the admin's cap changes
// (SetLimits wakes it too), and at once when a viewer asks for high, so that a paused layer resumes without waiting
// (Share.askedForHigh). What the policy decides reaches the sharer two ways: a QualityHintEvent whenever the share's
// cap or its pause state changes, whose encodings the web sharer applies with setParameters (05), and, while the
// shares of a pub PC have a cap, a REMB of it on that PC every second, which Chrome takes as the upper bound of its
// own congestion control.

// UplinkPolicy decides a share's uplink (02 §11): the cap on what its sharer sends, and which of its layers the
// sharer sends at all. The SFU keeps one per share, and calls Tick on the publishing Conn's actor, never two at once:
// once a second, and in between when something may have changed what the policy decides (the admin's cap, a viewer's
// new request for high). M1 has one policy for every publisher (the admin's cap and layer pausing, m1Uplink); M2's
// native publishers, the desktop and mobile clients, get an estimator of their uplink on top of it (nativeUplink,
// M2 01 §10.1). Every other kind (web, and the tools, which signal maps to web) keeps m1Uplink.
type UplinkPolicy interface {
	Tick(s ShareUplinkStats) UplinkDecision
}

// ShareUplinkStats is what an UplinkPolicy decides by.
type ShareUplinkStats struct {
	Client ClientKind // the publishing Conn's client
	// HighRequests counts the share's viewers whose request is high, as they asked (a focused share): what the full
	// layer is there for. It is the request before the downlink allocator's cap (02 §10.2), so a viewer held at the
	// preview layer for its bandwidth keeps the full layer flowing, and its trial upgrades try a layer that is there.
	HighRequests int
	Layers       []LayerUplinkStats // the video layers attached now, sorted f, h, q; paused ones included
	// gen is the ticker's second whose layer stats these are (SFU.uplinkGen). The policy is also asked between the
	// ticker's seconds (askedForHigh, SetLimits), on the same layer stats; a policy that counts consecutive seconds
	// (nativeUplink) counts a generation once.
	gen uint64
}

// LayerUplinkStats are the numbers of one video layer of a share, as it arrives.
type LayerUplinkStats struct {
	RID     string
	Bitrate int           // bps, 2 s moving average (LayerInfo.Bitrate)
	LossPct float64       // loss over the last 2 s, in percent (LayerInfo.LossPct), after repair
	Jitter  time.Duration // interarrival jitter (RFC 3550 §6.4.1)
	Viewers int           // video DownTracks that forward the layer now or wait for its keyframe
	// RawLossPct is the share of sequence numbers that were missing when a later one arrived, over the last 2 s, in
	// percent, before any retransmission (M2 01 §10.1): the Layer's RTP loop counts each gap at the new highest
	// sequence number, and padding counts as received. LossPct hides the congestion that NACKs repair.
	RawLossPct float64
	// QueueDelay is the layer's queueing-delay trend (M2 01 §10.1, delayTrend): the minimum one-way delay of the
	// last 500 ms above its slowly rising baseline; −1 when unknown.
	QueueDelay time.Duration
	// arrived is what arrived of the layer in the ticker's last second alone, in bit/s: the rate nativeUplink's loss
	// row backs off from (M2 01 §10.1). Bitrate's 2 s average still carries some of what the sharer sent before the
	// bottleneck began losing it.
	arrived int
}

// UplinkDecision is what an UplinkPolicy decided.
type UplinkDecision struct {
	MaxBitrate int             // bps; 0 = none
	Active     map[string]bool // per rid; nil = no change
	// Reason is HintReasonCongestion when MaxBitrate is the native estimator's estimate, which binds below the
	// admin's cap (M2 01 §10.1); otherwise "", MaxBitrate is the admin's cap, and the M1 rule chooses the hint's
	// reason (lookAtUplink).
	Reason string
}

// The reasons of a QualityHintEvent (02 §11): the cap changed, or which layers the share's viewers need did. M2 adds
// HintReasonCongestion (uplink_native.go).
const (
	HintReasonAdmin   = "admin"
	HintReasonViewers = "viewers"
)

// m1Uplink is the M1 policy of 02 §11, one per share and the same for every publisher. The cap is the admin's
// (Limits.MaxShareKbps). With sfu.pause_unwatched_layers on, the full layer is paused once nobody has asked for high
// for 10 s, and resumes as soon as somebody does. The 10 s count from the share's start, or from the first look that
// found no request for high after one that found some: the last request ended between those two looks, a second
// apart at most, so the layer is paused 10 to 11 s after it ended, never before. Nothing else is ever paused: the
// preview layer is what every thumbnail shows, and the full layer of a share that has no preview layer is the only
// video it has.
type m1Uplink struct {
	sfu *SFU
	now func() int64 // the clock: SFU.uplinkNow
	// wantedAt is when the 10 s began: the share's start, or the first look that found no request for high after
	// one that found some (watched). While watched, it is the last look that found one.
	wantedAt int64
	watched  bool
	paused   bool
}

func (p *m1Uplink) Tick(s ShareUplinkStats) UplinkDecision {
	d := UplinkDecision{MaxBitrate: p.sfu.currentLimits().shareCap()}
	if !p.sfu.pauseUnwatched {
		return d
	}
	now := p.now()
	if s.HighRequests > 0 {
		p.wantedAt, p.watched = now, true
		if p.paused {
			p.paused = false
			d.Active = map[string]bool{ridFull: true}
		}
		return d
	}
	if p.watched {
		p.wantedAt, p.watched = now, false // the request for high ended since the last look
	}
	if !p.paused && now-p.wantedAt >= int64(pauseAfter) && slices.ContainsFunc(s.Layers, isPreview) {
		p.paused = true
		d.Active = map[string]bool{ridFull: false}
	}
	return d
}

func isPreview(l LayerUplinkStats) bool { return l.RID == ridPreview }

// uplinkNow is the uplink policies' clock: monoNow, moved on in tests (uplinkSkew).
func (s *SFU) uplinkNow() int64 { return monoNow() + s.uplinkSkew.Load() }

// shareUplink is a share's uplink control as the publishing Conn's actor keeps it (02 §11).
type shareUplink struct {
	policy UplinkPolicy
	// capBps is the cap the sharer was told last, in bit/s (0 for none): by the ShareParams of StartShare, or by a
	// hint. UpdateShare's don't count, as they may go to another of the user's connections (01 §8.7). paused holds the
	// slot bits of the layers it was told to pause (Share.paused is the copy for other goroutines).
	capBps int
	paused uint32
	// adminBps is the admin's cap that the encodings the sharer was told last are under (M2 01 §10.1: the encodings
	// carry the admin's cap, MaxBitrate the dynamic total). Under m1Uplink it is always capBps.
	adminBps int
	// reason is the reason of the last hint, "" before the first: Resync tells the sharer again what the hints told
	// it. hintAt is when the last hint went out, on the uplink policies' clock: a congestion hint keeps
	// nativeHintMinGap from it.
	reason string
	hintAt int64
}

// newShareUplink returns the uplink control of a new share sh whose ShareParams carry capBps, for the kind of its
// publishing Conn: the native estimator for the desktop and mobile clients (M2 01 §10.1), M1's policy for every
// other kind.
func newShareUplink(s *SFU, sh *Share, client ClientKind, capBps int) shareUplink {
	u := shareUplink{capBps: capBps, adminBps: capBps}
	switch client {
	case ClientDesktop, ClientMobile:
		u.policy = newNativeUplink(s, sh, s.uplinkNow)
	default:
		u.policy = newM1Uplink(s, s.uplinkNow)
	}
	return u
}

// newM1Uplink returns the M1 policy of a share that starts now, on a clock.
func newM1Uplink(s *SFU, now func() int64) *m1Uplink {
	return &m1Uplink{sfu: s, now: now, wantedAt: now()}
}

// hint returns the QualityHintEvent that tells the sharer of share id where its uplink stands: the cap, and the
// encodings of its preset in the shape StartShare returns them, under the admin's cap and with the paused layers
// inactive.
func (u *shareUplink) hint(id ShareID, preset Preset) QualityHintEvent {
	return QualityHintEvent{
		Share: id, Reason: u.reason, MaxBitrate: u.capBps, Encodings: presetEncodings(preset, u.adminBps, u.paused),
	}
}

// congestionDue reports whether a cap that only the native estimator moved goes to the sharer now (M2 01 §10.1, the
// plan's hysteresis): when it differs from the cap the sharer was told by nativeHintMinChange of that (or one of the
// two is none), and nativeHintMinGap has passed since the share's last hint. A change that isn't due waits: the next
// look compares the cap it decides with the told one again.
func (u *shareUplink) congestionDue(capBps int, now int64) bool {
	if u.reason != "" && now-u.hintAt < int64(nativeHintMinGap) {
		return false
	}
	told := u.capBps
	if (told == 0) != (capBps == 0) {
		return true
	}
	// The threshold in whole bit/s, so that a change of exactly 15 % counts whatever the float rounding.
	return math.Abs(float64(capBps-told)) >= math.Round(nativeHintMinChange*float64(told))
}

// uplinkStats returns what the share's uplink policy decides by. It reads the viewers' requests as their actors
// published them (Subscription.requested), the layers' rates, raw loss and queueing delay of the ticker's last second
// and their jitter (atomics), and the share's layer set under Share.mu, a leaf. The generation comes first: the ticker
// adds one only after the layers' stats are in place, so stats read after it are at least that second's.
func (s *Share) uplinkStats(client ClientKind) ShareUplinkStats {
	st := ShareUplinkStats{Client: client, gen: s.conn.sfu.uplinkGen.Load()}
	viewers := s.downTracks(webrtc.RTPCodecTypeVideo)
	for _, dt := range viewers {
		if dt.sub.requested() == QualityHigh {
			st.HighRequests++
		}
	}
	layers := s.layerSet()
	for _, slot := range [...]Slot{SlotF, SlotH, SlotQ} {
		l := layers[slot]
		if l == nil {
			continue
		}
		ls := LayerUplinkStats{
			RID: slot.String(), Bitrate: int(l.stats.bitrate.Load()),
			LossPct: math.Float64frombits(l.stats.lossPct.Load()), Jitter: l.jitterNow(),
			RawLossPct: math.Float64frombits(l.stats.rawLossPct.Load()),
			QueueDelay: time.Duration(l.stats.queueDelay.Load()), arrived: int(l.stats.arrived.Load()),
		}
		for _, dt := range viewers {
			if dt.interest.Load()&(1<<slot) != 0 {
				ls.Viewers++
			}
		}
		st.Layers = append(st.Layers, ls)
	}
	return st
}

// allocRate returns the rate in bit/s at which the downlink allocators weigh the share's layer l (02 §10.2): its
// measured rate, unless that says nothing about what the layer brings a viewer who gets it. A video layer the sharer
// was told to pause measures 0 within seconds, and one it was told to resume less than rateWindow ago measures less
// than it sends while its 2 s moving average catches up: such a layer weighs at least its maximum
// (Share.maxBitrate). So a request for high made while the viewer's allocator is limited is capped unless the full
// layer's maximum fits, rather than served at a layer measured at 0 bps; the request still counts for the uplink
// policy (ShareUplinkStats.HighRequests), so the layer resumes, and the next trial upgrade tests a layer that flows
// (02 §11). The sharer's own pause of the whole share (Share.sharerPaused, M2 01 §10.3) counts as a pause of every
// video layer, and its end as their resume (setSharerPaused stores resumedAt). It reads atomics only.
func (s *Share) allocRate(l *Layer) int64 {
	rate := l.stats.bitrate.Load()
	if l.slot >= SlotAudio {
		return rate
	}
	if s.paused.Load()&(1<<l.slot) != 0 || s.sharerPaused.Load() {
		return max(rate, s.maxBitrate(l.slot))
	}
	if at := s.resumedAt[l.slot].Load(); at != 0 && s.conn.sfu.uplinkNow()-at < int64(rateWindow) {
		return max(rate, s.maxBitrate(l.slot))
	}
	return rate
}

// askedForHigh runs on a viewer's actor when its request for the share has become high (UpdateSubscriptions), after
// the actor has published the request (Subscription.publish). The publishing Conn's actor then looks at the share's
// uplink at once, so a paused full layer resumes as soon as somebody asks for it (02 §11), not at the ticker's next
// second. It does so whenever layers may be paused, not only while Share.paused says so: a look that is deciding to
// pause right now, on the requests from before this one, stores the bit only after it has decided, and the look
// posted here, which sees this request, comes after it.
func (s *Share) askedForHigh() {
	if s.paused.Load() == 0 && !s.conn.sfu.pauseUnwatched {
		return
	}
	pub := s.conn
	pub.post(func() { pub.lookAtUplink(s) })
}

// wakeUplink has the Conn's actor look at the uplink of the shares it publishes (uplinkTick), unless a look is
// queued already. It never blocks: the ticker calls it once a second, SetLimits when the cap may have changed.
func (c *Conn) wakeUplink() {
	if c.upQueued.CompareAndSwap(false, true) {
		c.post(c.uplinkTick)
	}
}

// uplinkTick runs on the actor: each share the Conn publishes gets its policy's look (lookAtUplink), and then the
// pub PC its REMB (sendREMB).
func (c *Conn) uplinkTick() {
	c.upQueued.Store(false)
	shares := c.ownShares()
	for _, sh := range shares {
		c.lookAtUplink(sh)
	}
	c.sendREMB(shares)
}

// ownShares returns the shares the Conn publishes that haven't ended, sorted by StartedAt.
func (c *Conn) ownShares() []*Share {
	c.room.mu.Lock()
	var shares []*Share
	for _, sh := range c.part.shares {
		if sh.conn == c {
			shares = append(shares, sh)
		}
	}
	c.room.mu.Unlock()
	sortShares(shares)
	return shares
}

// lookAtUplink asks the share's uplink policy (02 §11) and tells the sharer what changed: a QualityHintEvent when its
// cap or its pause state did, with the reason viewers when the pause state did (that wins over a cap that changed
// with it), admin when the admin's cap did, and congestion when only the native estimator moved the cap (M2 01
// §10.1). Each hint carries both, whole: MaxBitrate the decision's cap, the encodings the admin's. Pause and admin
// changes go out at once, as in M1; a congestion change only when it is due (shareUplink.congestionDue: 15 % and
// 2 s). A layer that resumes gets a keyframe request once the hint is out, for the viewers who wait for it: a sharer
// that sends the layer again starts it with a keyframe of its own accord, and one that never stopped it (a client
// that applies no encodings) is asked here. The time of the resume goes to Share.resumedAt, for the downlink
// allocators (allocRate). It runs on the actor; a share that has ended gets nothing.
func (c *Conn) lookAtUplink(sh *Share) {
	up := &sh.up
	if sh.conn != c || up.policy == nil {
		return
	}
	preset, live := sh.currentPreset()
	if !live {
		return
	}
	d := up.policy.Tick(sh.uplinkStats(c.client))
	paused := up.paused
	for rid, active := range d.Active {
		slot, ok := slotFor(webrtc.RTPCodecTypeVideo, rid)
		if !ok || rid == "" {
			continue // "h" until M5, and "", which names no layer of a simulcast share
		}
		if active {
			paused &^= 1 << slot
		} else {
			paused |= 1 << slot
		}
	}
	capBps := max(d.MaxBitrate, 0)
	adminBps := capBps // without an estimate that binds, the decision's cap is the admin's
	if d.Reason == HintReasonCongestion {
		adminBps = c.sfu.currentLimits().shareCap()
	}
	pausing := paused != up.paused
	adminChanged := adminBps != up.adminBps
	if !pausing && !adminChanged && capBps == up.capBps {
		return
	}
	now := c.sfu.uplinkNow()
	reason := HintReasonAdmin
	switch {
	case pausing:
		reason = HintReasonViewers
	case !adminChanged: // only the estimate moved the cap
		if !up.congestionDue(capBps, now) {
			return
		}
		reason = HintReasonCongestion
	}
	resumed := up.paused &^ paused
	up.paused, up.capBps, up.adminBps, up.reason, up.hintAt = paused, capBps, adminBps, reason, now
	if resumed != 0 {
		// Before the paused bits go, so the downlink allocator never sees a resumed layer as neither (allocRate).
		at := max(now, 1)
		for slot := range SlotAudio {
			if resumed&(1<<slot) != 0 {
				sh.resumedAt[slot].Store(at)
			}
		}
	}
	sh.paused.Store(paused)
	c.log.Debug("quality hint", "share_id", string(sh.id), "reason", up.reason, "max_bitrate", capBps,
		"paused", pausedRIDs(paused))
	c.sig.SendEvent(up.hint(sh.id, preset))
	if resumed == 0 {
		return
	}
	mono := monoNow()
	for slot := range SlotAudio {
		if resumed&(1<<slot) == 0 {
			continue
		}
		if l := sh.layer(slot); l != nil {
			l.requestKeyframe(mono)
		}
	}
}

// pausedRIDs returns the rids of the slot bits in paused, for a log line.
func pausedRIDs(paused uint32) []string {
	out := []string{}
	for _, slot := range [...]Slot{SlotF, SlotH, SlotQ} {
		if paused&(1<<slot) != 0 {
			out = append(out, slot.String())
		}
	}
	return out
}

// resendHints tells the sharer again where the uplink of each share it publishes stands, if it was ever told by a
// hint (Resync): the hint as it would be now, with the reason of the last one, so a sharer whose WebSocket dropped
// it ends up with the current cap and pause state, and the encodings of the current preset. It runs on the actor.
func (c *Conn) resendHints() {
	for _, sh := range c.ownShares() {
		if sh.up.reason == "" {
			continue
		}
		if preset, live := sh.currentPreset(); live {
			c.sig.SendEvent(sh.up.hint(sh.id, preset))
		}
	}
}

// rembState is what the Conn's actor keeps of the REMBs it wrote (sendREMB): the pub PC it wrote them to, and how
// many REMBs of rembUncapped that PC is still to get since its cap went.
type rembState struct {
	to    rtcpWriter
	lifts int
}

// sendREMB writes the REMB of the admin's cap on the pub PC (02 §11): the sum of the caps of the shares the PC
// carries, and the SSRCs of their video layers. Chrome takes a REMB's bitrate as the upper bound of everything the PC
// sends, whichever SSRCs it names, so a pub PC with several shares gets one REMB for all of them, and none while one
// of them has no cap, which a REMB can't say. A browser keeps the last REMB as its limit until the next one, so once
// the cap is gone the PC gets rembLifts REMBs of rembUncapped, one a second. Nothing is written while the actor has
// the pub PC as away: a write would fail. It runs on the actor, with the Conn's shares (ownShares).
func (c *Conn) sendREMB(shares []*Share) {
	if !c.pubUp.Load() {
		return
	}
	var (
		to     rtcpWriter
		sender uint32
		ssrcs  []uint32
		total  int
		capped = true
	)
	for _, sh := range shares {
		video := false
		for _, l := range sh.attached() {
			if l.kind != webrtc.RTPCodecTypeVideo || l.pubPC == nil {
				continue
			}
			if to == nil {
				to, sender = l.pubPC, l.rtcpSSRC
			}
			if l.pubPC == to { // the attached layers are all the current pub PC's
				ssrcs, video = append(ssrcs, l.ssrc), true
			}
		}
		if video {
			total += sh.up.capBps
			capped = capped && sh.up.capBps > 0
		}
	}
	if to == nil {
		return
	}
	if to != c.remb.to {
		c.remb = rembState{to: to} // a new pub PC has no limit from the old one's REMBs
	}
	bitrate := total
	switch {
	case capped:
		c.remb.lifts = rembLifts
	case c.remb.lifts > 0:
		c.remb.lifts--
		bitrate = rembUncapped
	default:
		return
	}
	slices.Sort(ssrcs)
	remb := &rtcp.ReceiverEstimatedMaximumBitrate{SenderSSRC: sender, Bitrate: float32(bitrate), SSRCs: ssrcs}
	if err := to.WriteRTCP([]rtcp.Packet{remb}); err != nil {
		c.log.Debug("REMB not written", "err", err)
	}
}
