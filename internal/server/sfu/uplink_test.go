package sfu

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The unit tests of publisher-side control (02 §11, README S88): the M1 uplink policy on its fake clock, the quality
// hints and the keyframe gate of a paused layer, the REMB of the admin's cap, and the jitter a layer measures. The
// tests with real PeerConnections are pausing_test.go's.

// hints returns the QualityHintEvents the Signaler got, in order.
func (r *recSignaler) hints() []QualityHintEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []QualityHintEvent
	for _, ev := range r.events {
		if h, ok := ev.(QualityHintEvent); ok {
			out = append(out, h)
		}
	}
	return out
}

// waitHints waits until the Signaler has got n QualityHintEvents and returns them.
func (r *recSignaler) waitHints(t *testing.T, n int) []QualityHintEvent {
	t.Helper()
	eventuallyTrue(t, func() bool { return len(r.hints()) >= n }, fmt.Sprintf("%d quality hints have come", n))
	return r.hints()
}

// TestM1UplinkPolicy is the policy of 02 §11 on a fake clock: the admin's cap always, and with layer pausing the full
// layer off once nobody has asked for high for 10 s (not a nanosecond before), on again with the first request, and
// never off while the share has no preview layer.
func TestM1UplinkPolicy(t *testing.T) {
	s, _ := newTicklessSFU(t)
	var clock int64
	now := func() int64 { return clock }
	advance := func(d time.Duration) { clock += int64(d) }
	both := ShareUplinkStats{Client: ClientWeb, Layers: []LayerUplinkStats{{RID: "f"}, {RID: "q"}}}
	focused := both
	focused.HighRequests = 1
	fOnly := ShareUplinkStats{Client: ClientWeb, Layers: []LayerUplinkStats{{RID: "f"}}}
	tick := func(p UplinkPolicy, st ShareUplinkStats, want UplinkDecision) {
		t.Helper()
		if got := p.Tick(st); !reflect.DeepEqual(got, want) {
			t.Fatalf("Tick(%+v) = %+v, want %+v", st, got, want)
		}
	}
	off := map[string]bool{"f": false}
	on := map[string]bool{"f": true}

	// sfu.pause_unwatched_layers = false: the cap, and nothing paused however long nobody watches.
	var p UplinkPolicy = newM1Uplink(s, now)
	tick(p, both, UplinkDecision{})
	advance(time.Hour)
	tick(p, both, UplinkDecision{})
	s.SetLimits(Limits{MaxShareKbps: 2500})
	tick(p, both, UplinkDecision{MaxBitrate: 2_500_000})
	s.SetLimits(Limits{})

	s.pauseUnwatched = true // before any Join: no actor reads it yet
	p = newM1Uplink(s, now)
	advance(pauseAfter - 1)
	tick(p, both, UplinkDecision{})
	advance(1)
	tick(p, both, UplinkDecision{Active: off})
	tick(p, both, UplinkDecision{}) // no change
	tick(p, focused, UplinkDecision{Active: on})
	tick(p, focused, UplinkDecision{})
	// The 10 s count from the last look that found a request for high.
	advance(pauseAfter - 1)
	tick(p, both, UplinkDecision{})
	advance(1)
	s.SetLimits(Limits{MaxShareKbps: 3000})
	tick(p, both, UplinkDecision{MaxBitrate: 3_000_000, Active: off})
	s.SetLimits(Limits{})

	// A share without its preview layer keeps its full layer: it is all the share's viewers could get.
	p = newM1Uplink(s, now)
	advance(time.Hour)
	tick(p, fOnly, UplinkDecision{})
	tick(p, both, UplinkDecision{Active: off}) // the preview layer arrives: the hour without a request counts
	tick(p, fOnly, UplinkDecision{})           // and the full layer stays off when it goes again
	focusedFOnly := fOnly
	focusedFOnly.HighRequests = 2
	tick(p, focusedFOnly, UplinkDecision{Active: on})
}

// TestUplinkHints: a share's sharer hears where its uplink stands only when that changes (02 §11): f paused 10 s
// after the share started without a request for high, with the reason viewers and the full encodings; resumed at once,
// without waiting for the ticker, when a viewer asks for high, and asked for a keyframe then; a cap change with the
// reason admin; both in one hint, with the reason viewers, when they change together. A paused layer's keyframe
// requests do nothing, so that the one after the resume finds the throttle unspent. A viewer that the downlink
// allocator holds at the preview layer still counts as asking for high. UpdateShare and Resync tell the sharer the
// same state.
func TestUplinkHints(t *testing.T) {
	s, _ := newTicklessSFU(t)
	s.pauseUnwatched = true
	ctx := testCtx(t)
	pub, pubSig := join(t, s, "lounge", "alice", "c-pub", RoleFull)
	params := startShare(t, pub, "s_1")
	sh := s.lookupShare("s_1")
	f, fPub := testLayer(t, sh, SlotF, 0xf00)
	q, _ := testLayer(t, sh, SlotQ, 0xa00)
	viewer, _ := join(t, s, "lounge", "bob", "c-view", RoleViewer)
	request := func(want Quality) {
		t.Helper()
		errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: want, Audio: true}})
		if err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions(%v) = %v, %v", want, errs, err)
		}
	}
	look := func() {
		t.Helper()
		if err := pub.CheckUplink(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hint := func(reason string, capBps int, paused uint32) QualityHintEvent {
		return QualityHintEvent{
			Share: "s_1", Reason: reason, MaxBitrate: capBps, Encodings: presetEncodings(PresetAuto, capBps, paused),
		}
	}
	request(QualityLow)

	// Nothing to tell for 10 s without a request for high. (The real clock goes on too: TestM1UplinkPolicy has the
	// exact 10 s.)
	s.AdvanceUplinkClock(pauseAfter - time.Second)
	look()
	if n := len(pubSig.hints()); n != 0 {
		t.Fatalf("%d hints before 10 s had passed", n)
	}
	// Then f goes off: the whole list, q on, no cap.
	s.AdvanceUplinkClock(time.Second)
	look()
	paused := hint(HintReasonViewers, 0, 1<<SlotF)
	if got := pubSig.hints(); len(got) != 1 || !reflect.DeepEqual(got[0], paused) {
		t.Fatalf("hints = %+v, want %+v", got, paused)
	}
	if want := slices.Clone(params.Encodings); !reflect.DeepEqual(paused.Encodings[1], want[1]) ||
		paused.Encodings[0].Active || paused.Encodings[0].MaxBitrate != want[0].MaxBitrate {
		t.Errorf("the hint's encodings %+v are not StartShare's %+v with f inactive", paused.Encodings, want)
	}
	if sh.paused.Load() != 1<<SlotF {
		t.Errorf("Share.paused = %#b", sh.paused.Load())
	}
	look()
	if n := len(pubSig.hints()); n != 1 {
		t.Errorf("%d hints after a look that changed nothing", n)
	}

	// A keyframe request for the paused layer writes nothing and spends no throttle; q's go out as before.
	if f.requestKeyframe(monoNow()) || len(fPub.plis()) != 0 || f.stats.pliThrottled.Load() != 0 {
		t.Errorf("a keyframe request for the paused layer: %d PLIs, %d throttled", len(fPub.plis()),
			f.stats.pliThrottled.Load())
	}
	if !q.requestKeyframe(monoNow()) {
		t.Error("no PLI for the preview layer")
	}

	// UpdateShare tells the sharer the same: f inactive.
	p, err := pub.UpdateShare(ctx, "s_1", ShareUpdate{})
	if err != nil || !reflect.DeepEqual(p.Encodings, paused.Encodings) {
		t.Errorf("UpdateShare while f is paused = %+v, %v; want the encodings %+v", p, err, paused.Encodings)
	}
	// Resync: the last hint again.
	pub.Resync()
	if got := pubSig.waitHints(t, 2); !reflect.DeepEqual(got[1], paused) {
		t.Errorf("Resync sent %+v, want the last hint %+v", got[1], paused)
	}

	// The viewer asks for high: f resumes at once, without a look of the ticker's, and its keyframe is asked for.
	request(QualityHigh)
	resumed := hint(HintReasonViewers, 0, 0)
	if got := pubSig.waitHints(t, 3); !reflect.DeepEqual(got[2], resumed) {
		t.Errorf("the hint after the request for high = %+v, want %+v", got[2], resumed)
	}
	eventuallyTrue(t, func() bool { return len(fPub.plis()) == 1 }, "the resumed layer's PLI has gone out")
	if pli := fPub.plis()[0]; pli.MediaSSRC != 0xf00 {
		t.Errorf("PLI = %+v", pli)
	}
	if sh.paused.Load() != 0 {
		t.Errorf("Share.paused = %#b after the resume", sh.paused.Load())
	}

	// The downlink allocator holds the viewer at the preview layer: its request is still high, and f stays on.
	err = viewer.do(ctx, func(context.Context) error {
		viewer.capSubscription(viewer.subs["s_1"], QualityLow, SubReasonBandwidth)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.AdvanceUplinkClock(time.Hour)
	look()
	if n := len(pubSig.hints()); n != 3 {
		t.Errorf("%d hints while a capped viewer asks for high, want 3: f must keep flowing", n)
	}

	// The admin's cap changes: the publishing Conn looks at once, and the reason is admin, f on with the cap.
	s.SetLimits(Limits{MaxShareKbps: 2500})
	if got := pubSig.waitHints(t, 4); !reflect.DeepEqual(got[3], hint(HintReasonAdmin, 2_500_000, 0)) {
		t.Errorf("the hint after the cap changed = %+v", got[3])
	}
	// Back to low, 10 s pass, and the cap changes in the same look: one hint, with the reason viewers.
	request(QualityLow)
	look()
	s.AdvanceUplinkClock(pauseAfter)
	s.SetLimits(Limits{MaxShareKbps: 3000})
	settle(pub)
	look()
	got := pubSig.hints()
	if want := hint(HintReasonViewers, 3_000_000, 1<<SlotF); len(got) != 5 || !reflect.DeepEqual(got[4], want) {
		t.Errorf("hints = %+v, want the fifth and last to be %+v", got, want)
	}
	if st, ok, err := pub.Uplink(ctx, "s_1"); err != nil || !ok ||
		!reflect.DeepEqual(st, UplinkState{MaxBitrate: 3_000_000, Paused: []string{"f"}, Reason: HintReasonViewers}) {
		t.Errorf("Uplink = %+v, %v, %v", st, ok, err)
	}

	// An ended share tells nothing more.
	if err := pub.StopShare(ctx, "s_1", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	s.SetLimits(Limits{})
	look()
	pub.Resync()
	settle(pub)
	if n := len(pubSig.hints()); n != 5 {
		t.Errorf("%d hints after the share ended, want 5", n)
	}
}

// settle waits until the Conn's actor has done the work posted to it so far (its event queue is in order).
func settle(c *Conn) {
	done := make(chan struct{})
	c.post(func() { close(done) })
	<-done
}

// fixedPolicy is an UplinkPolicy that always decides the same.
type fixedPolicy UplinkDecision

func (p fixedPolicy) Tick(ShareUplinkStats) UplinkDecision { return UplinkDecision(p) }

// layerOn attaches a Layer to a share as testLayer does, writing its RTCP to to.
func layerOn(t *testing.T, sh *Share, slot Slot, ssrc uint32, to rtcpWriter) *Layer {
	t.Helper()
	sh.conn.pubUp.Store(true)
	kind := webrtc.RTPCodecTypeVideo
	if slot == SlotAudio {
		kind = webrtc.RTPCodecTypeAudio
	}
	l := newLayer(slot, kind)
	l.share, l.ssrc, l.pubPC, l.rtcpSSRC = sh, ssrc, to, 0x5f5f5f5f
	if _, ok := sh.attach(l); !ok {
		t.Fatalf("share %s took no layer", sh.id)
	}
	return l
}

// rembs returns the REMBs written so far.
func (r *rtcpRecorder) rembs() []rtcp.ReceiverEstimatedMaximumBitrate {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rtcp.ReceiverEstimatedMaximumBitrate
	for _, p := range r.pkts {
		if remb, ok := p.(*rtcp.ReceiverEstimatedMaximumBitrate); ok {
			out = append(out, *remb)
		}
	}
	return out
}

// TestUplinkREMB: while the shares of a pub PC have a cap, every look writes one REMB on it: the sum of their caps,
// naming the SSRCs of their video layers (paused ones too, audio not), sorted. None while one of its shares has no
// cap, or while the pub PC is away. Once the cap goes, the PC gets three REMBs of no limit, one per look, then none;
// a new pub PC is owed none.
func TestUplinkREMB(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-pub", RoleFull)
	startShare(t, pub, "s_1")
	startShare(t, pub, "s_2")
	s1, s2 := s.lookupShare("s_1"), s.lookupShare("s_2")
	pc := &rtcpRecorder{}
	layerOn(t, s1, SlotF, 0x30, pc)
	layerOn(t, s1, SlotQ, 0x10, pc)
	layerOn(t, s2, SlotF, 0x20, pc)
	layerOn(t, s2, SlotQ, 0x40, pc)
	layerOn(t, s2, SlotAudio, 0x50, pc)
	look := func(n int) {
		t.Helper()
		for range n {
			if err := pub.CheckUplink(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	capped := func(bps float32) rtcp.ReceiverEstimatedMaximumBitrate {
		return rtcp.ReceiverEstimatedMaximumBitrate{
			SenderSSRC: 0x5f5f5f5f, Bitrate: bps, SSRCs: []uint32{0x10, 0x20, 0x30, 0x40},
		}
	}
	all := func(rembs []rtcp.ReceiverEstimatedMaximumBitrate, want rtcp.ReceiverEstimatedMaximumBitrate) bool {
		return !slices.ContainsFunc(rembs, func(r rtcp.ReceiverEstimatedMaximumBitrate) bool {
			return !reflect.DeepEqual(r, want)
		})
	}

	setLimits := func(kbps int) { // with the look it makes the publishing Conn take at once
		s.SetLimits(Limits{MaxShareKbps: kbps})
		settle(pub)
	}
	setPolicy := func(sh *Share, p UplinkPolicy) {
		t.Helper()
		if err := pub.do(ctx, func(context.Context) error {
			sh.up.policy = p
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	look(2)
	if n := len(pc.rembs()); n != 0 {
		t.Fatalf("%d REMBs without a cap", n)
	}
	setLimits(2500)
	look(2)
	if rembs := pc.rembs(); len(rembs) != 3 || !all(rembs, capped(5_000_000)) {
		t.Fatalf("REMBs = %+v, want one per look of %+v", rembs, capped(5_000_000))
	}
	// The pub PC away: nothing.
	pc.forget()
	pub.pubUp.Store(false)
	look(2)
	if n := len(pc.rembs()); n != 0 {
		t.Errorf("%d REMBs while the pub PC is away", n)
	}
	pub.pubUp.Store(true)
	look(1)
	if rembs := pc.rembs(); len(rembs) != 1 || !all(rembs, capped(5_000_000)) {
		t.Errorf("REMBs once the pub PC is back = %+v", rembs)
	}

	// The cap goes: three REMBs of no limit, one per look, and nothing after.
	pc.forget()
	setLimits(0)
	look(rembLifts + 1)
	if rembs := pc.rembs(); len(rembs) != rembLifts || !all(rembs, capped(rembUncapped)) {
		t.Errorf("REMBs after the cap went = %+v, want %d of %+v", rembs, rembLifts, capped(rembUncapped))
	}

	// A share whose policy sets no cap: no REMB for the PC, though the other share has one.
	pc.forget()
	setLimits(1000)
	if rembs := pc.rembs(); len(rembs) != 1 || !all(rembs, capped(2_000_000)) {
		t.Errorf("REMBs under a cap of 1000 kbit/s = %+v", rembs)
	}
	pc.forget()
	setPolicy(s2, fixedPolicy{})
	look(rembLifts + 1)
	if rembs := pc.rembs(); len(rembs) != rembLifts || !all(rembs, capped(rembUncapped)) {
		t.Errorf("REMBs once s_2 has no cap = %+v, want %d of %+v", rembs, rembLifts, capped(rembUncapped))
	}

	// A new pub PC (the shares' layers on another writer) has had no REMB, and owes none of no limit. Its first REMB
	// names its own layers.
	setPolicy(s2, newM1Uplink(s, s.uplinkNow))
	look(1)
	pc.forget()
	next := &rtcpRecorder{}
	for _, sh := range []*Share{s1, s2} {
		for _, l := range sh.attached() {
			sh.detach(l)
		}
	}
	layerOn(t, s1, SlotF, 0x31, next)
	layerOn(t, s2, SlotF, 0x21, next)
	look(1)
	if rembs := next.rembs(); len(rembs) != 1 || rembs[0].Bitrate != 2_000_000 ||
		!slices.Equal(rembs[0].SSRCs, []uint32{0x21, 0x31}) {
		t.Errorf("REMBs on the new pub PC = %+v", rembs)
	}
	setLimits(0)
	look(rembLifts)
	if rembs := next.rembs(); len(rembs) != 1+rembLifts || len(pc.rembs()) != 0 {
		t.Errorf("REMBs on the new pub PC after its cap went = %+v (on the old one %+v), want its own %d lifts", rembs,
			pc.rembs(), rembLifts)
	}
}

// TestLayerJitter is the interarrival jitter of RFC 3550 §6.4.1 that a layer measures for its uplink policy: 0 for
// packets that arrive as they were sent; |D| for a delay that alternates by D; packets that come late or after a
// pause of more than 10 s change nothing.
func TestLayerJitter(t *testing.T) {
	s, _ := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	audio, _ := testLayer(t, sh, SlotAudio, 0x50)
	video, _ := testLayer(t, sh, SlotF, 0xf0)
	seq, ts := uint16(65000), uint32(4_294_000_000) // both wrap
	at := t0
	send := func(l *Layer, delay time.Duration) {
		pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, PayloadType: 111}, Payload: []byte{1, 2}}
		profile := ProfileKey("")
		if l.kind == webrtc.RTPCodecTypeVideo {
			pkt.Payload, profile = []byte{0x41, 1}, ProfileHigh
		}
		l.handleRTP(pkt, profile, at+int64(delay))
	}
	step := func(ticks uint32, every time.Duration) {
		seq++
		ts += ticks
		at += int64(every)
	}

	for range 100 {
		send(audio, 0)
		step(960, 20*time.Millisecond)
	}
	if j := audio.jitterNow(); j != 0 {
		t.Errorf("jitter of a steady stream = %v", j)
	}
	for i := range 300 {
		send(audio, time.Duration(i%2)*5*time.Millisecond)
		step(960, 20*time.Millisecond)
	}
	if j := audio.jitterNow(); j < 4900*time.Microsecond || j > 5*time.Millisecond {
		t.Errorf("jitter of a delay alternating by 5 ms = %v, want 5 ms", j)
	}
	before := audio.stats.jitter.Load()
	// A late packet (an older sequence number) is no sample.
	seq -= 5
	send(audio, 300*time.Millisecond)
	seq += 5
	// After 11 s without a packet the next one only starts the estimate again.
	at += int64(11 * time.Second)
	ts += 960 * 50 // a timeline that went on 1 s, not 11
	send(audio, 0)
	if after := audio.stats.jitter.Load(); after != before {
		t.Errorf("the jitter estimate went from %d to %d with a late packet and one after a pause", before, after)
	}

	// Video counts in 90 kHz ticks: frames every 33 ms (3000 ticks), each 2 ms late or early in turn.
	for i := range 300 {
		send(video, time.Duration(i%2)*2*time.Millisecond)
		step(3000, time.Second/30)
	}
	if j := video.jitterNow(); j < 1900*time.Microsecond || j > 2100*time.Microsecond {
		t.Errorf("video jitter = %v, want 2 ms", j)
	}
	st := sh.uplinkStats(ClientWeb)
	if len(st.Layers) != 1 || st.Layers[0].RID != "f" || st.Layers[0].Jitter != video.jitterNow() {
		t.Errorf("uplink stats = %+v", st)
	}
}
