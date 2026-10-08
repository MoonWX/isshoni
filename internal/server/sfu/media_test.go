package sfu

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The media path's unit tests (README S41): the Layer's read loop and rates (02 §9.1), the DownTrack's writer and
// RTCP handling (02 §9.3), keyframe requests and their throttle (02 §9.7), and a share's ShareUpdated calls
// (02 §5.3, §6.2). They build Layers and DownTracks by hand on a real SFU, feed them packets and look at what comes
// out; the test with real PeerConnections and media is TestBasicForwarding (forwarding_test.go).

// rtcpRecorder is a Layer's pub PC for a test: it records the RTCP the SFU writes to the publisher.
type rtcpRecorder struct {
	mu   sync.Mutex
	pkts []rtcp.Packet
	err  error // what WriteRTCP returns
}

func (r *rtcpRecorder) WriteRTCP(pkts []rtcp.Packet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.pkts = append(r.pkts, pkts...)
	return nil
}

// forget drops what was recorded so far.
func (r *rtcpRecorder) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pkts = nil
}

// plis returns the PLIs written so far.
func (r *rtcpRecorder) plis() []rtcp.PictureLossIndication {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rtcp.PictureLossIndication
	for _, p := range r.pkts {
		if pli, ok := p.(*rtcp.PictureLossIndication); ok {
			out = append(out, *pli)
		}
	}
	return out
}

// eventLog is a RoomEvents that records the calls of every kind in one list, as "updated s_1 live f+audio" and
// "ended s_1 stopped", so that a test sees their order.
type eventLog struct {
	mu    sync.Mutex
	calls []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, s)
}

func describe(s ShareInfo) string {
	parts := layerNames(s)
	if s.Audio {
		parts = append(parts, "audio")
	}
	return fmt.Sprintf("%s %s %s", s.ID, s.State, strings.Join(parts, "+"))
}

func layerNames(s ShareInfo) []string {
	var out []string
	for _, l := range s.Layers {
		out = append(out, l.RID)
	}
	return out
}

func (e *eventLog) ShareUpdated(_ RoomID, s ShareInfo) { e.add("updated " + describe(s)) }
func (e *eventLog) ShareEnded(_ RoomID, s ShareInfo, r EndReason) {
	e.add(fmt.Sprintf("ended %s %s", s.ID, r))
}
func (e *eventLog) CodecPolicyChanged(RoomID, ProfileKey) {}

// take returns the calls recorded since the last take.
func (e *eventLog) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.calls
	e.calls = nil
	return out
}

// newTicklessSFU builds an SFU whose ticker never starts: the test makes its reports (reportShares) and its
// once-a-second work (everySecond) by hand, with its own clock.
func newTicklessSFU(t *testing.T) (*SFU, *eventLog) {
	t.Helper()
	ev := &eventLog{}
	s, err := New(Config{Transport: loopbackTransport(t, false)}, Deps{Events: ev, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.ticking = true // as if the ticker ran, and had already stopped
	s.mu.Unlock()
	close(s.tickDone)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("SFU.Close: %v", err)
		}
	})
	return s, ev
}

// testShare starts a share on a new publisher Conn and returns it.
func testShare(t *testing.T, s *SFU, id ShareID) *Share {
	t.Helper()
	pub, _ := join(t, s, "lounge", UserID("pub-"+id), ConnID("c-"+id), RoleFull)
	startShare(t, pub, id)
	return s.lookupShare(id)
}

// testLayer attaches a Layer to sh as a pub PC's track would, with a recorder in place of the pub PC.
func testLayer(t *testing.T, sh *Share, slot Slot, ssrc uint32) (*Layer, *rtcpRecorder) {
	t.Helper()
	kind := webrtc.RTPCodecTypeVideo
	if slot == SlotAudio {
		kind = webrtc.RTPCodecTypeAudio
	}
	rec := &rtcpRecorder{}
	l := newLayer(slot, kind)
	l.share, l.ssrc, l.pubPC, l.rtcpSSRC = sh, ssrc, rec, 0x5f5f5f5f
	if _, ok := sh.attach(l); !ok {
		t.Fatalf("share %s took no layer", sh.id)
	}
	return l, rec
}

// captureWriter is a DownTrack's webrtc.TrackLocalWriter for a test: it records what the writer sends, and can stand
// in for a Pion that isn't ready to send (taken counts down the packets it takes without sending) or that fails.
type captureWriter struct {
	hdrs     []rtp.Header
	payloads [][]byte
	taken    int
	err      error
}

func (w *captureWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.taken > 0 {
		w.taken--
		return 0, nil
	}
	w.hdrs = append(w.hdrs, h.Clone())
	w.payloads = append(w.payloads, payload)
	return h.MarshalSize() + len(payload), nil
}

func (w *captureWriter) Write(b []byte) (int, error) {
	return 0, errors.New("the SFU writes with WriteRTP")
}

// Payload types of the test viewer, as Chrome and sfutest.Viewer have them.
var chromePTs = map[ProfileKey]uint8{"42e0": 96, "4200": 98, "4d00": 100, "640c": 102, "6400": 104}

// testDownTrack makes a DownTrack of viewer for sh, on the share's fan-out list and bound as Pion would bind it: to
// a sub PC whose DTLS-ready gate is open, with SSRC 0x7000 (video) or 0x7001 (audio).
func testDownTrack(t *testing.T, viewer *Conn, sh *Share, kind webrtc.RTPCodecType) (*DownTrack, *captureWriter) {
	t.Helper()
	sub := &Subscription{conn: viewer, share: sh}
	d := newDownTrack(sub, kind)
	if !sh.addDownTrack(d) {
		t.Fatalf("share %s took no DownTrack", sh.id)
	}
	pc := &subPC{gen: 1}
	pc.ready.Store(true)
	w := &captureWriter{}
	b := &binding{pc: pc, writer: w, ssrc: 0x7000, ptFor: chromePTs, absSendTimeID: 3}
	if kind == webrtc.RTPCodecTypeAudio {
		b.ssrc, b.ptFor, b.absSendTimeID = 0x7001, map[ProfileKey]uint8{"": 111}, 0
	}
	d.binding.Store(b)
	return d, w
}

// drain runs the DownTrack's writer over everything queued, as its goroutine would.
func drain(d *DownTrack, w *rtpWriteState) {
	for {
		select {
		case p := <-d.queue:
			d.write(w, p)
		default:
			return
		}
	}
}

// Payloads: the start of a keyframe (a STAP-A with the SPS of 640x360 Constrained Baseline and a PPS), and a slice
// of a delta frame.
func keyPayload(t *testing.T) []byte {
	return stapA(mustHex(t, realSPS[4].hex), mustHex(t, ppsHex))
}

var deltaPayload = []byte{0x41, 0x9a, 0x00, 0x01}

func rtpPkt(seq uint16, ts uint32, marker bool, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, Marker: marker, PayloadType: 123, SequenceNumber: seq, Timestamp: ts, SSRC: 0xf00,
			CSRC: []uint32{1, 2},
		},
		Payload: payload,
	}
}

// TestLayerHandleRTP: the RTP loop of 02 §9.1. A packet is copied once and goes to the DownTracks that want its
// layer; duplicates, packets a cache capacity late and video in a payload type the SFU doesn't forward go nowhere; a
// padding-only packet goes out as a marker and is never cached; the first packet that starts a keyframe makes the
// pending share live, and until then every other packet asks the publisher for one.
func TestLayerHandleRTP(t *testing.T) {
	s, ev := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, pubF := testLayer(t, sh, SlotF, 0xf00)
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio
	wantsF, _ := testDownTrack(t, viewer, sh, video)
	wantsQ, _ := testDownTrack(t, viewer, sh, video)
	paused, _ := testDownTrack(t, viewer, sh, video)
	wantsA, _ := testDownTrack(t, viewer, sh, audio)
	for _, d := range []*DownTrack{wantsF, wantsQ, paused, wantsA} {
		d.binding.Store(nil) // not negotiated yet: this test is about what reaches the queues
	}
	wantsF.setTarget(SlotF, true)
	wantsQ.setTarget(SlotQ, true)
	wantsA.setTarget(SlotAudio, true)
	queued := func(d *DownTrack) []*packet {
		var out []*packet
		for {
			select {
			case p := <-d.queue:
				out = append(out, p)
			default:
				return out
			}
		}
	}
	now := t0

	// A delta frame's packet on a pending share: forwarded like any packet, and the publisher is asked for the keyframe
	// the share waits for, with the layer's SSRC and the pub PC's RTCP SSRC. Asking again within 500 ms is throttled.
	buf := slices.Clone(deltaPayload)
	f.handleRTP(rtpPkt(100, 1000, false, buf), ProfileHigh, now)
	buf[0] = 0xff // the reader reuses its buffer
	got := queued(wantsF)
	if len(got) != 1 {
		t.Fatalf("the DownTrack that wants f got %d packets, want 1", len(got))
	}
	want := packet{layer: f, seq: 100, ts: 1000, pt: 123, profile: ProfileHigh, payload: deltaPayload, arrival: now}
	if p := got[0]; p.layer != want.layer || p.seq != want.seq || p.ts != want.ts || p.marker || p.pt != want.pt ||
		p.profile != want.profile || p.keyStart || p.padding || !slices.Equal(p.payload, want.payload) || p.arrival != now ||
		cap(p.payload) != len(p.payload) {
		t.Errorf("queued packet = %+v, want an exact-size copy of the packet", p)
	}
	if n := len(queued(wantsQ)) + len(queued(paused)) + len(queued(wantsA)); n != 0 {
		t.Errorf("%d packets of f reached DownTracks that don't want f", n)
	}
	wantPLI := []rtcp.PictureLossIndication{{SenderSSRC: 0x5f5f5f5f, MediaSSRC: 0xf00}}
	if plis := pubF.plis(); !slices.Equal(plis, wantPLI) {
		t.Errorf("PLIs for a pending share's delta packet = %+v, want %+v", plis, wantPLI)
	}
	f.handleRTP(rtpPkt(101, 1000, true, deltaPayload), ProfileHigh, now+int64(pliInterval)-1)
	if n := len(pubF.plis()); n != 1 || f.stats.pliThrottled.Load() != 1 {
		t.Errorf("%d PLIs, %d throttled after a second delta packet within 500 ms", n, f.stats.pliThrottled.Load())
	}
	if p := queued(wantsF); len(p) != 1 || !p[0].marker {
		t.Errorf("the packet with the marker bit: %+v", p)
	}
	if info, _ := sh.info(); info.State != SharePending || info.Profile != "" || info.Layers[0].Width != 0 {
		t.Errorf("the share before a keyframe = %+v", info)
	}

	// The packet that starts a keyframe: the share is live, in the packet's profile, and the layer has its size.
	now += oneSecond
	f.handleRTP(rtpPkt(102, 4000, false, keyPayload(t)), ProfileHigh, now)
	if p := queued(wantsF); len(p) != 1 || !p[0].keyStart {
		t.Errorf("the keyframe start: %+v", p)
	}
	info, _ := sh.info()
	if info.State != ShareLive || info.LiveAt.IsZero() || info.Profile != ProfileHigh || !info.Audio ||
		!slices.Equal(layerNames(info), []string{"f", "q"}) || info.Layers[0].Width != 640 || info.Layers[0].Height != 360 ||
		!info.Layers[0].Active || info.Layers[1].Active {
		t.Errorf("the share after its first keyframe = %+v", info)
	}
	select {
	case <-s.kick:
	default:
		t.Error("the ticker was not woken for the state change")
	}
	s.reportShares(now)
	if calls := ev.take(); !slices.Equal(calls, []string{"updated s_1 live f+q+audio"}) {
		t.Errorf("room events = %v", calls)
	}
	// A live share asks for nothing more.
	f.handleRTP(rtpPkt(103, 4000, true, deltaPayload), ProfileHigh, now+oneSecond)
	if n := len(pubF.plis()); n != 1 {
		t.Errorf("%d PLIs after the share went live, want still 1", n)
	}
	queued(wantsF)

	// A copy of a packet the cache holds (Chrome's redundant RTX), a packet a cache capacity late, and video in a
	// payload type that is no H.264 go nowhere.
	f.handleRTP(rtpPkt(102, 4000, false, keyPayload(t)), ProfileHigh, now)
	f.handleRTP(rtpPkt(1<<16+103-cacheVideoFloor, 4000, false, deltaPayload), ProfileHigh, now)
	f.handleRTP(rtpPkt(104, 7000, false, deltaPayload), "", now)
	if p := queued(wantsF); len(p) != 0 {
		t.Errorf("%d packets forwarded, want none: a duplicate, a too-late one and one with a foreign payload type", len(p))
	}
	if d, late, bad := f.stats.duplicates.Load(), f.stats.tooLate.Load(), f.stats.badPT.Load(); d != 1 || late != 1 || bad != 1 {
		t.Errorf("counted %d duplicates, %d too late, %d with a bad payload type; want 1 each", d, late, bad)
	}
	if f.cache.get(104, now) != nil {
		t.Error("the packet with a foreign payload type was cached")
	}
	// A padding-only packet goes out as a marker and is never cached.
	f.handleRTP(rtpPkt(104, 7000, false, nil), ProfileHigh, now)
	if p := queued(wantsF); len(p) != 1 || !p[0].padding || p[0].seq != 104 || p[0].payload != nil || p[0].keyStart {
		t.Errorf("padding marker = %+v", p)
	}
	if f.cache.get(104, now) != nil || f.cache.get(103, now) == nil {
		t.Error("the cache holds the padding packet, or lost the media packet before it")
	}
	if got, want := f.stats.packets.Load(), uint64(8); got != want {
		t.Errorf("the layer counted %d packets, want %d: every one it read", got, want)
	}
	if frames, keys := f.stats.frames.Load(), f.stats.keyframes.Load(); frames != 2 || keys != 1 {
		t.Errorf("%d frames, %d keyframes; want 2 timestamps and 1 keyframe", frames, keys)
	}

	// The other layers reach only their own DownTracks. Audio always counts as a keyframe start and asks for none.
	q.handleRTP(rtpPkt(7, 90, false, keyPayload(t)), ProfileHigh, now)
	a.handleRTP(rtpPkt(9, 960, false, []byte{1, 2, 3}), "", now)
	if p := queued(wantsQ); len(p) != 1 || p[0].layer != q {
		t.Errorf("q's packet: %+v", p)
	}
	if p := queued(wantsA); len(p) != 1 || p[0].layer != a || !p[0].keyStart || p[0].profile != "" {
		t.Errorf("the audio packet: %+v", p)
	}
	if n := len(queued(wantsF)) + len(queued(paused)); n != 0 {
		t.Errorf("%d packets of q and audio reached other DownTracks", n)
	}
	if a.requestKeyframe(now) || len(pubQ.plis()) != 0 {
		t.Error("a keyframe request for audio, or for a layer nobody waits for")
	}

	// A full queue drops, and counts, instead of holding up the layer.
	for i := range videoQueueLen + 5 {
		f.handleRTP(rtpPkt(uint16(200+i), 9000, false, deltaPayload), ProfileHigh, now)
	}
	if n, drops := len(queued(wantsF)), wantsF.stats.drops.Load(); n != videoQueueLen || drops != 5 {
		t.Errorf("%d packets queued and %d dropped, want %d and 5", n, drops, videoQueueLen)
	}

	// A keyframe on a Layer that is no longer attached changes nothing: its track has ended or was replaced.
	pending := testShare(t, s, "s_2")
	gone, _ := testLayer(t, pending, SlotF, 0xf01)
	pending.detach(gone)
	gone.handleRTP(rtpPkt(1, 1, false, keyPayload(t)), ProfileHigh, now)
	if info, _ := pending.info(); info.State != SharePending {
		t.Errorf("a detached layer's keyframe made the share %s", info.State)
	}
	// Sender reports: the layer keeps the latest one of its own SSRC.
	f.handleSR(&rtcp.SenderReport{SSRC: 0xbad, NTPTime: 1, RTPTime: 2}, now)
	if f.lastSR.Load() != nil {
		t.Error("a sender report of another SSRC was kept")
	}
	f.handleSR(&rtcp.SenderReport{SSRC: 0xf00, NTPTime: 77 << 32, RTPTime: 4000}, now)
	if sr := f.lastSR.Load(); sr == nil || *sr != (srInfo{ntp: 77 << 32, rtp: 4000, arrival: now}) {
		t.Errorf("lastSR = %+v", sr)
	}
}

// TestLayerTick: the once-a-second stats of 02 §9.1. Bitrate, packet rate and frame rate are measured over each
// second and averaged over about two; loss is what the last two seconds of the sequence space are still missing,
// so a gap that a retransmission fills in time is no loss; a layer is active for 2 s after a packet; and its packet
// cache grows with the packet rate.
func TestLayerTick(t *testing.T) {
	s, _ := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	l, _ := testLayer(t, sh, SlotF, 0xf00)
	payload := make([]byte, 1000)
	payload[0] = 0x41
	seq, ts := uint16(65000), uint32(0) // the sequence numbers wrap during the test
	// second feeds one second of video: 30 frames of 3 packets, without the packets skip names; it returns their
	// sequence numbers.
	second := func(now int64, skip func(i int) bool) (skipped []uint16) {
		for i := range 90 {
			if i%3 == 0 {
				ts += 3000
			}
			if skip != nil && skip(i) {
				skipped = append(skipped, seq)
			} else {
				l.handleRTP(rtpPkt(seq, ts, i%3 == 2, payload), ProfileHigh, now+int64(i)*int64(time.Second)/90)
			}
			seq++
		}
		return skipped
	}
	check := func(when string, bitrate int, fps, loss float64) {
		t.Helper()
		info, _ := sh.info()
		got := info.Layers[0]
		if got.Bitrate != bitrate || math.Abs(got.FPS-fps) > 0.01 || math.Abs(got.LossPct-loss) > 0.01 {
			t.Errorf("%s: %d bit/s, %.2f fps, %.2f%% loss; want %d, %.2f, %.2f%%", when, got.Bitrate, got.FPS, got.LossPct,
				bitrate, fps, loss)
		}
	}

	now := t0
	if l.active(now) {
		t.Error("a layer without a packet is active")
	}
	s.everySecond(now) // the baseline
	second(now, nil)
	check("before the first measured second", 0, 0, 0)
	now += oneSecond
	s.everySecond(now)
	check("one clean second", 720_000, 30, 0) // 90 packets of 1000 bytes
	if last := l.lastRTP.Load(); !l.active(now) || !l.active(last+int64(activeAfter)) || l.active(last+int64(activeAfter)+1) {
		t.Error("a layer is active for exactly 2 s after its last packet")
	}

	// A second in which every tenth packet is lost: 9 of the 180 sequence numbers of the last two seconds.
	lost := second(now, func(i int) bool { return i%10 == 4 })
	now += oneSecond
	s.everySecond(now)
	check("a second with loss", 691_670, 30, 5) // 720000 + (1 − e^−½)·(648000 − 720000)
	// The publisher retransmits them: they arrive late, fill their places, and the loss is gone.
	for _, lostSeq := range lost {
		l.handleRTP(rtpPkt(lostSeq, ts, false, payload), ProfileHigh, now)
	}
	second(now, nil)
	now += oneSecond
	s.everySecond(now)
	if info, _ := sh.info(); info.Layers[0].LossPct != 0 {
		t.Errorf("loss after the gaps were filled = %.2f%%, want 0", info.Layers[0].LossPct)
	}
	if d, late := l.stats.duplicates.Load(), l.stats.tooLate.Load(); d != 0 || late != 0 {
		t.Errorf("%d duplicates and %d too-late packets among the retransmissions", d, late)
	}

	// The stream stops: the rates decay.
	before, _ := sh.info()
	now += oneSecond
	s.everySecond(now)
	after, _ := sh.info()
	if after.Layers[0].Bitrate >= before.Layers[0].Bitrate*7/10 || after.Layers[0].Bitrate == 0 || after.Layers[0].FPS >= 21 {
		t.Errorf("rates one silent second later: %+v, before %+v; want them about 40%% lower", after.Layers[0], before.Layers[0])
	}

	// The packet cache follows the packet rate: a second at 3000 packets per second grows it past its floor at once.
	if c := l.cache.capacity(); c != cacheVideoFloor {
		t.Fatalf("cache capacity %d at 90 packets per second, want the floor %d", c, cacheVideoFloor)
	}
	for i := range 3000 {
		l.handleRTP(rtpPkt(seq, ts, false, payload[:10]), ProfileHigh, now+int64(i))
		seq++
	}
	now += oneSecond
	s.everySecond(now)
	if c := l.cache.capacity(); c <= cacheVideoFloor {
		t.Errorf("cache capacity %d after a second at 3000 packets per second, want it grown", c)
	}
}

// TestKeyframeThrottle is 02 §17's throttle_test: a hundred concurrent keyframe requests for one layer make one PLI,
// and the next one goes out 500 ms later, not before (02 §9.7).
func TestKeyframeThrottle(t *testing.T) {
	s, _ := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	l, pub := testLayer(t, sh, SlotF, 0xf00)
	now := t0

	var wg sync.WaitGroup
	sent := make(chan bool, 100)
	for range 100 {
		wg.Go(func() { sent <- l.requestKeyframe(now) })
	}
	wg.Wait()
	close(sent)
	wrote := 0
	for ok := range sent {
		if ok {
			wrote++
		}
	}
	if n := len(pub.plis()); n != 1 || wrote != 1 || l.stats.pliSent.Load() != 1 || l.stats.pliThrottled.Load() != 99 {
		t.Fatalf("100 concurrent requests: %d PLIs written, %d callers told so, %d counted sent and %d throttled; want 1, "+
			"1, 1 and 99", n, wrote, l.stats.pliSent.Load(), l.stats.pliThrottled.Load())
	}
	if l.requestKeyframe(now+int64(pliInterval)-1) || len(pub.plis()) != 1 {
		t.Error("a second PLI before 500 ms had passed")
	}
	if !l.requestKeyframe(now+int64(pliInterval)) || len(pub.plis()) != 2 {
		t.Fatal("no PLI 500 ms after the last one")
	}
	if got, want := pub.plis()[1], (rtcp.PictureLossIndication{SenderSSRC: 0x5f5f5f5f, MediaSSRC: 0xf00}); got != want {
		t.Errorf("PLI = %+v, want %+v", got, want)
	}

	// Share.requestKeyframe finds the layer of a slot: nothing for a slot without one.
	sh.requestKeyframe(SlotQ)
	if n := len(pub.plis()); n != 2 {
		t.Errorf("%d PLIs after a request for a layer the share doesn't have", n)
	}
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	sh.requestKeyframe(SlotQ)
	if plis := pubQ.plis(); len(plis) != 1 || plis[0].MediaSSRC != 0xa00 {
		t.Errorf("PLIs for q = %+v", plis)
	}
	// A pub PC that can't send (not connected, closing): the request is spent, and whoever waits asks again in 500 ms.
	pubQ.err = errors.New("not connected")
	later := monoNow() + int64(pliInterval)
	if q.requestKeyframe(later) || q.stats.pliSent.Load() != 1 {
		t.Error("a PLI the pub PC refused counts as sent")
	}
	pubQ.err = nil
	if q.requestKeyframe(later+1) || !q.requestKeyframe(later+int64(pliInterval)) {
		t.Error("after a refused PLI the next request must wait its 500 ms, and then go out")
	}
}

// TestDownTrackWrite: the writer of 02 §9.3. Nothing is written, and the munger not even asked, before the viewer
// negotiated the track and its sub PC's DTLS is up; then the stream starts on a keyframe start, which the DownTrack
// has the publisher asked for; every packet goes out under a fresh header.
func TestDownTrackWrite(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, pub := testLayer(t, sh, SlotF, 0xf00)
	sh.keyframe(f, ProfileHigh) // live: the layer's own loop asks for nothing
	d, w := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	b := d.binding.Load()
	var state rtpWriteState
	now := t0
	seq, ts := uint16(500), uint32(90000)
	// send feeds one packet through the layer and the writer and returns what the writer has written so far.
	send := func(payload []byte, profile ProfileKey, marker bool) int {
		t.Helper()
		now += tick
		f.handleRTP(rtpPkt(seq, ts, marker, payload), profile, now)
		seq++
		ts += 3000
		drain(d, &state)
		return len(w.hdrs)
	}
	key := keyPayload(t)

	// Paused: no interest, so nothing even reaches the queue.
	if n := send(key, ProfileHigh, false); n != 0 || len(d.queue) != 0 || d.interest.Load() != 0 {
		t.Fatalf("a paused DownTrack: %d written, %d queued, interest %b", n, len(d.queue), d.interest.Load())
	}

	// The DTLS-ready gate is closed (S4 finding 1): packets are dropped before the munger sees them, keyframes
	// included, and nobody is asked for a keyframe the viewer couldn't get.
	b.pc.ready.Store(false)
	d.setTarget(SlotF, true)
	if d.interest.Load() != 1<<SlotF {
		t.Fatalf("interest = %b, want the bit of f", d.interest.Load())
	}
	send(deltaPayload, ProfileHigh, false)
	if n := send(key, ProfileHigh, false); n != 0 || d.m.n != 0 || d.m.started || len(pub.plis()) != 0 || d.forwarding.Load() {
		t.Fatalf("with the gate closed: %d written, %d epochs, %d PLIs, forwarding %v; want nothing at all", n, d.m.n,
			len(pub.plis()), d.forwarding.Load())
	}
	// Not bound at all (before the answer, after Unbind): the same.
	d.binding.Store(nil)
	if n := send(key, ProfileHigh, false); n != 0 || d.m.n != 0 {
		t.Fatalf("unbound: %d written, %d epochs", n, d.m.n)
	}
	// A viewer that negotiated no H.264 (02 §8.5): the same.
	d.binding.Store(&binding{pc: b.pc, writer: w, unsupported: true})
	b.pc.ready.Store(true)
	if n := send(key, ProfileHigh, false); n != 0 || d.m.n != 0 {
		t.Fatalf("unsupported binding: %d written, %d epochs", n, d.m.n)
	}
	d.binding.Store(b)

	// The gate is open. A delta packet can't start the stream: the DownTrack waits, and has the publisher asked for
	// a keyframe, once per 500 ms however many packets wait.
	send(deltaPayload, ProfileHigh, false)
	if n := send(deltaPayload, ProfileHigh, false); n != 0 || len(pub.plis()) != 1 || d.forwarding.Load() {
		t.Fatalf("waiting for a keyframe: %d written, %d PLIs, forwarding %v; want 0, 1, false", n, len(pub.plis()),
			d.forwarding.Load())
	}
	// In a profile the viewer has no payload type for (a codec policy change under way): dropped before the munger.
	b.ptFor = map[ProfileKey]uint8{"42e0": 96}
	if n := send(key, ProfileHigh, false); n != 0 || d.m.n != 0 || d.stats.noCodec.Load() != 1 {
		t.Fatalf("no payload type for the profile: %d written, %d epochs, %d counted", n, d.m.n, d.stats.noCodec.Load())
	}
	b.ptFor = chromePTs

	// The keyframe start: the first packet the viewer gets. The header is fresh: the viewer's payload type for the
	// profile and its SSRC, the munger's random first seq and ts, the publisher's marker bit, and abs-send-time as
	// the only extension; nothing else of the publisher's header (its payload type 123, its CSRCs, its SSRC).
	if n := send(key, ProfileHigh, true); n != 1 {
		t.Fatalf("the keyframe start: %d packets written, want 1", n)
	}
	h := w.hdrs[0]
	if h.Version != 2 || h.Padding || !h.Marker || h.PayloadType != 104 || h.SSRC != 0x7000 || len(h.CSRC) != 0 ||
		h.SequenceNumber != d.m.seq0 || h.Timestamp != d.m.ts0 {
		t.Errorf("first header = %+v, want PT 104, SSRC 0x7000, seq %d, ts %d", h, d.m.seq0, d.m.ts0)
	}
	if ids := h.GetExtensionIDs(); !slices.Equal(ids, []uint8{3}) || len(h.GetExtension(3)) != 3 ||
		h.ExtensionProfile != rtp.ExtensionProfileOneByte {
		t.Errorf("extensions = %v (profile %#x), want only abs-send-time with id 3", ids, h.ExtensionProfile)
	}
	if &w.payloads[0][0] != &f.cache.get(seq-1, now).payload[0] {
		t.Error("the payload written is a copy, want the one slice that the cache and every viewer share")
	}
	if !d.forwarding.Load() || d.stats.switches.Load() != 1 || d.stats.packets.Load() != 1 ||
		d.stats.bytes.Load() != uint64(len(key)) {
		t.Errorf("after the first packet: forwarding %v, %d epochs, %d packets, %d bytes", d.forwarding.Load(),
			d.stats.switches.Load(), d.stats.packets.Load(), d.stats.bytes.Load())
	}
	if !slices.Equal(sh.viewers(), []ParticipantID{"bob"}) {
		t.Errorf("the share's viewers = %v, want bob", sh.viewers())
	}
	// What follows continues it: seq + 1, the timestamp 3000 ticks on, other payload types by profile.
	send(deltaPayload, ProfileHigh, false)
	send(deltaPayload, ProfileHigh, true)
	for i, want := range []struct {
		seq    uint16
		ts     uint32
		marker bool
	}{{d.m.seq0 + 1, d.m.ts0 + 3000, false}, {d.m.seq0 + 2, d.m.ts0 + 6000, true}} {
		if h := w.hdrs[i+1]; h.SequenceNumber != want.seq || h.Timestamp != want.ts || h.Marker != want.marker || h.PayloadType != 104 {
			t.Errorf("header %d = seq %d, ts %d, marker %v, PT %d; want %+v", i+1, h.SequenceNumber, h.Timestamp, h.Marker,
				h.PayloadType, want)
		}
	}
	if len(pub.plis()) != 1 {
		t.Errorf("%d PLIs once the stream runs, want still 1", len(pub.plis()))
	}
	// A padding-only packet upstream leaves no gap downstream.
	send(nil, ProfileHigh, false)
	if n := send(deltaPayload, ProfileHigh, false); n != 4 || w.hdrs[3].SequenceNumber != d.m.seq0+3 {
		t.Errorf("after upstream padding: %d written, seq %d; want 4 and %d", n, w.hdrs[n-1].SequenceNumber, d.m.seq0+3)
	}
	// Without abs-send-time negotiated the header has no extension at all.
	b.absSendTimeID = 0
	if n := send(deltaPayload, ProfileHigh, false); w.hdrs[n-1].Extension || len(w.hdrs[n-1].GetExtensionIDs()) != 0 {
		t.Errorf("header without abs-send-time negotiated: %+v", w.hdrs[n-1])
	}

	// The viewer's PLI and FIR for this track's SSRC become a keyframe request for the layer forwarded now;
	// feedback that names another SSRC, or no keyframe, does nothing.
	now += oneSecond
	forget := func() { pub.forget(); f.lastPLI.Store(-int64(pliInterval)) } // as if the last PLI were long ago
	forget()
	d.handleRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: 0x9999}, &rtcp.ReceiverReport{}})
	d.handleRTCP([]rtcp.Packet{&rtcp.FullIntraRequest{FIR: []rtcp.FIREntry{{SSRC: 0x9999}}}})
	if n := len(pub.plis()); n != 0 {
		t.Errorf("%d PLIs for feedback about another SSRC", n)
	}
	d.handleRTCP([]rtcp.Packet{&rtcp.ReceiverReport{}, &rtcp.PictureLossIndication{MediaSSRC: 0x7000}})
	if n := len(pub.plis()); n != 1 || d.stats.keyRequests.Load() != 1 {
		t.Errorf("%d PLIs upstream, %d counted, after the viewer's PLI", n, d.stats.keyRequests.Load())
	}
	forget()
	d.handleRTCP([]rtcp.Packet{&rtcp.FullIntraRequest{FIR: []rtcp.FIREntry{{SSRC: 1}, {SSRC: 0x7000}}}})
	if plis := pub.plis(); len(plis) != 1 || plis[0].MediaSSRC != 0xf00 {
		t.Errorf("PLIs upstream after the viewer's FIR = %+v, want one for the layer", plis)
	}

	// Pause: at once, and nothing is asked for.
	forget()
	written := len(w.hdrs)
	d.setTarget(SlotF, false)
	if n := send(key, ProfileHigh, false); n != written || d.interest.Load() != 0 || d.forwarding.Load() || len(sh.viewers()) != 0 {
		t.Errorf("paused: %d more written, interest %b, forwarding %v", n-written, d.interest.Load(), d.forwarding.Load())
	}
	d.handleRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: 0x7000}})
	if n := len(pub.plis()); n != 0 {
		t.Errorf("%d PLIs for a paused DownTrack", n)
	}
	// Resume: the publisher is asked at once (the viewer can receive), and the stream goes on with the keyframe,
	// right after the last seq.
	d.setTarget(SlotF, true)
	if n := len(pub.plis()); n != 1 {
		t.Errorf("%d PLIs on resume, want 1", n)
	}
	last := w.hdrs[written-1].SequenceNumber
	send(deltaPayload, ProfileHigh, false)
	if n := send(key, ProfileHigh, false); n != written+1 || w.hdrs[written].SequenceNumber != last+1 {
		t.Errorf("after the resume: %d more written, seq %d; want 1 and %d", n-written, w.hdrs[n-1].SequenceNumber, last+1)
	}

	// A write that fails (the sender stopped) is counted and changes nothing else.
	w.err = errors.New("closed")
	written = len(w.hdrs)
	if n := send(deltaPayload, ProfileHigh, false); n != written || d.stats.writeErrors.Load() != 1 || !d.forwarding.Load() {
		t.Errorf("a failed write: %d more written, %d errors counted", n-written, d.stats.writeErrors.Load())
	}
	// Unbind: the track is no longer forwarded.
	if err := d.Unbind(nil); err != nil || d.forwarding.Load() {
		t.Errorf("Unbind: %v, forwarding %v", err, d.forwarding.Load())
	}
}

// TestDownTrackUnsentStart: Pion can take a packet and send nothing, without an error: a sub PC reports connected a
// moment before its SRTP session is usable (S4 finding 1), and a sender's write path opens a moment after Bind. The
// viewer never saw that packet, so the stream must not go on from it: it starts again with the next keyframe.
func TestDownTrackUnsentStart(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, pub := testLayer(t, sh, SlotF, 0xf00)
	sh.keyframe(f, ProfileHigh)
	d, w := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	d.setTarget(SlotF, true) // asks for a keyframe: the viewer can receive
	pub.forget()
	f.lastPLI.Store(-int64(pliInterval))
	var state rtpWriteState
	now, seq := t0, uint16(10)
	send := func(payload []byte) {
		now += tick
		f.handleRTP(rtpPkt(seq, uint32(seq)*3000, false, payload), ProfileHigh, now)
		seq++
		drain(d, &state)
	}

	w.taken = 1
	send(keyPayload(t)) // taken, never sent
	send(deltaPayload)  // without the restart these would reach a viewer that has no keyframe to decode them with
	send(deltaPayload)
	if len(w.hdrs) != 0 || d.stats.unsent.Load() != 1 || d.forwarding.Load() {
		t.Fatalf("after a keyframe start that Pion took without sending: %d written, %d counted, forwarding %v", len(w.hdrs),
			d.stats.unsent.Load(), d.forwarding.Load())
	}
	if n := len(pub.plis()); n != 1 {
		t.Errorf("%d PLIs for the lost start, want 1", n)
	}
	send(keyPayload(t))
	send(deltaPayload)
	if len(w.hdrs) != 2 || w.hdrs[0].SequenceNumber != d.m.seq0+1 || w.hdrs[1].SequenceNumber != d.m.seq0+2 {
		t.Fatalf("after the next keyframe: %d written (%+v), want the stream to start there", len(w.hdrs), w.hdrs)
	}
	if !isKeyframeStart(w.payloads[0]) || !d.forwarding.Load() {
		t.Error("the first packet the viewer got doesn't start a keyframe")
	}
}

// TestDownTrackSwitch: a new target keeps the old layer flowing until the new one's keyframe arrives (02 §10.1). The
// interest mask holds both layers until then, the new layer's publisher is asked for a keyframe, and the switch
// leaves no gap in the viewer's sequence numbers.
func TestDownTrackSwitch(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, pubF := testLayer(t, sh, SlotF, 0xf00)
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	sh.keyframe(f, ProfileHigh)
	d, w := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	var state rtpWriteState
	now := t0
	seqs := map[*Layer]uint16{f: 100, q: 7000}
	send := func(l *Layer, payload []byte) {
		now += tick
		l.handleRTP(rtpPkt(seqs[l], uint32(now/int64(time.Millisecond))*90, false, payload), ProfileHigh, now)
		seqs[l]++
		drain(d, &state)
	}

	d.setTarget(SlotF, true)
	send(q, keyPayload(t)) // not wanted: never queued
	send(f, keyPayload(t))
	send(f, deltaPayload)
	if len(w.hdrs) != 2 || len(pubF.plis()) != 1 || len(pubQ.plis()) != 0 {
		t.Fatalf("on f: %d written, %d PLIs for f, %d for q", len(w.hdrs), len(pubF.plis()), len(pubQ.plis()))
	}

	d.setTarget(SlotQ, true)
	if d.interest.Load() != 1<<SlotF|1<<SlotQ {
		t.Fatalf("interest during the switch = %b, want f and q", d.interest.Load())
	}
	if n := len(pubQ.plis()); n != 1 {
		t.Errorf("%d PLIs for q after the target change, want 1", n)
	}
	send(q, deltaPayload) // q before its keyframe: waits
	send(f, deltaPayload) // f keeps flowing meanwhile
	if len(w.hdrs) != 3 || w.payloads[2][0] != deltaPayload[0] {
		t.Fatalf("while waiting for q's keyframe: %d written, want f's packet", len(w.hdrs))
	}
	send(q, keyPayload(t)) // the switch
	if d.interest.Load() != 1<<SlotQ {
		t.Errorf("interest after the switch = %b, want only q", d.interest.Load())
	}
	send(f, deltaPayload) // no longer wanted
	send(q, deltaPayload)
	if len(w.hdrs) != 5 || d.stats.switches.Load() != 2 {
		t.Fatalf("after the switch: %d written, %d epochs; want 5 and 2", len(w.hdrs), d.stats.switches.Load())
	}
	for i := 1; i < len(w.hdrs); i++ {
		if w.hdrs[i].SequenceNumber != w.hdrs[i-1].SequenceNumber+1 || int32(w.hdrs[i].Timestamp-w.hdrs[i-1].Timestamp) <= 0 {
			t.Errorf("packet %d: seq %d after %d, ts %d after %d; want one continuous stream", i, w.hdrs[i].SequenceNumber,
				w.hdrs[i-1].SequenceNumber, w.hdrs[i].Timestamp, w.hdrs[i-1].Timestamp)
		}
	}
	if !isKeyframeStart(w.payloads[3]) {
		t.Error("the first packet of the new layer doesn't start a keyframe")
	}
}

// TestDownTrackWriterGoroutine: run forwards what is queued until stop, which is idempotent and fine for a DownTrack
// whose writer never ran; a subscription's detach stops both of its writers.
func TestDownTrackWriterGoroutine(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	d, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeAudio)
	d.sub.audio, d.sub.video = d, newDownTrack(d.sub, webrtc.RTPCodecTypeVideo)
	d.sub.reqAudio = true
	d.sub.apply()
	if d.interest.Load() != 1<<SlotAudio || d.sub.video.interest.Load() != 0 {
		t.Fatalf("interest of an audio-only subscription: audio %b, video %b", d.interest.Load(), d.sub.video.interest.Load())
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(nil)
	}()
	for i := range 50 {
		a.handleRTP(rtpPkt(uint16(i), uint32(i)*960, false, []byte{byte(i)}), "", t0+int64(i)*tick)
	}
	deadline := time.Now().Add(5 * time.Second)
	for d.stats.packets.Load() < 50 {
		if time.Now().After(deadline) {
			t.Fatalf("the writer forwarded %d of 50 packets", d.stats.packets.Load())
		}
		time.Sleep(time.Millisecond)
	}
	d.sub.detach()
	d.sub.detach()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer did not stop")
	}
	if v, au, _ := s.FanOut("s_1"); v != 0 || au != 0 {
		t.Errorf("FanOut after detach = %d, %d", v, au)
	}
	if d.kind != webrtc.RTPCodecTypeAudio || d.m.clock != audioClockRate || cap(d.queue) != audioQueueLen ||
		cap(d.sub.video.queue) != videoQueueLen || d.sub.video.m.clock != videoClockRate {
		t.Error("queue sizes or clock rates of the DownTracks")
	}
}

// discardWriter is a TrackLocalWriter that sends nowhere and keeps nothing.
type discardWriter struct{}

func (discardWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	return 12 + len(payload), nil
}
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }

// TestForwardingAllocations: an incoming packet costs its two allocations (the packet and the exact-size copy of its
// payload) however many viewers get it, and forwarding it to a viewer costs none (02 §9.1, §9.2).
func TestForwardingAllocations(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	sh.keyframe(f, ProfileHigh)
	var dts []*DownTrack
	for range 3 {
		d, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
		d.binding.Load().writer = discardWriter{}
		d.setTarget(SlotF, true)
		dts = append(dts, d)
	}
	var state rtpWriteState
	seq, now := uint16(0), t0
	pkt := rtpPkt(0, 0, false, keyPayload(t))
	payload := make([]byte, 1100)
	payload[0] = 0x41
	step := func() {
		pkt.SequenceNumber, pkt.Timestamp = seq, uint32(seq)*3000
		f.handleRTP(pkt, ProfileHigh, now)
		pkt.Payload = payload
		seq++
		now += tick
	}
	step() // the keyframe start: every DownTrack starts forwarding
	for _, d := range dts {
		drain(d, &state)
	}
	if n := testing.AllocsPerRun(200, step); n > 2 {
		t.Errorf("an incoming packet for 3 viewers allocates %v times, want 2: the packet and its payload", n)
	}
	queued := len(dts[0].queue)
	if queued < 200 {
		t.Fatalf("%d packets queued, want the 200 just read", queued)
	}
	if n := testing.AllocsPerRun(queued-1, func() { dts[0].write(&state, <-dts[0].queue) }); n != 0 {
		t.Errorf("forwarding a packet to a viewer allocates %v times, want 0", n)
	}
	if got := dts[0].stats.packets.Load(); got < 200 {
		t.Errorf("the DownTrack wrote %d packets", got)
	}
}

// TestAbsSendTime: the extension's 24 bits are those of Pion's own encoding.
func TestAbsSendTime(t *testing.T) {
	for _, at := range []time.Time{
		time.Unix(0, 0), time.Unix(63, 999_999_999), time.Unix(64, 1), time.Unix(1_800_000_000, 123_456_789),
		time.Date(2026, 10, 8, 12, 34, 56, 789_000_000, time.UTC),
	} {
		want, err := rtp.NewAbsSendTimeExtension(at).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		var got [3]byte
		putAbsSendTime(&got, at)
		if !slices.Equal(got[:], want) {
			t.Errorf("abs-send-time of %v = %x, want %x", at, got, want)
		}
	}
}

// TestShareUpdates: what the ticker reports with ShareUpdated (02 §5.3, §6.2). A state change is reported at once
// and never merged with another; a layer, profile or audio change is reported only while the share is live, at most
// once per 250 ms; ShareEnded comes after every ShareUpdated of the share, and nothing after it.
func TestShareUpdates(t *testing.T) {
	s, ev := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	now := t0
	report := func(wait time.Duration) []string {
		now += int64(wait)
		s.reportShares(now)
		return ev.take()
	}

	// Pending: tracks arrive, nothing is reported.
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	if calls := report(time.Second); len(calls) != 0 {
		t.Errorf("room events for a pending share: %v", calls)
	}
	// Live: at once, with what the share has by then.
	if !sh.keyframe(f, ProfileHigh) || sh.keyframe(f, ProfileHigh) {
		t.Error("the first keyframe makes the share live, the second doesn't again")
	}
	if calls := report(0); !slices.Equal(calls, []string{"updated s_1 live f"}) {
		t.Errorf("room events = %v", calls)
	}
	// Changes while live are debounced: two of them within 250 ms of the last report make one report, 250 ms after
	// it, with the share as it is then.
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	if calls := report(100 * time.Millisecond); len(calls) != 0 {
		t.Errorf("room events 100 ms after the last report: %v", calls)
	}
	testLayer(t, sh, SlotQ, 0xa00)
	if calls := report(149 * time.Millisecond); len(calls) != 0 {
		t.Errorf("room events 249 ms after the last report: %v", calls)
	}
	if calls := report(time.Millisecond); !slices.Equal(calls, []string{"updated s_1 live f+q+audio"}) {
		t.Errorf("room events = %v", calls)
	}
	if calls := report(time.Second); len(calls) != 0 {
		t.Errorf("room events without a change: %v", calls)
	}
	// Long after the last report a change is reported by the next tick. A track that ends is such a change, a
	// profile change too; the same profile again is none.
	sh.detach(a)
	if calls := report(time.Millisecond); !slices.Equal(calls, []string{"updated s_1 live f+q"}) {
		t.Errorf("room events = %v", calls)
	}
	sh.keyframe(f, ProfileConstrainedBaseline)
	sh.keyframe(f, ProfileConstrainedBaseline)
	if calls := report(tickInterval); !slices.Equal(calls, []string{"updated s_1 live f+q"}) || sh.currentProfile() != ProfileConstrainedBaseline {
		t.Errorf("room events after a profile change = %v, profile %s", calls, sh.currentProfile())
	}
	if calls := report(tickInterval); len(calls) != 0 {
		t.Errorf("room events = %v", calls)
	}

	// State changes are never merged away: two between two reports are both reported, in order, each with the share
	// as it was. (Stalled is README S57's; the queue doesn't care which states it carries.)
	sh.mu.Lock()
	sh.state = ShareStalled
	sh.stateChangedLocked()
	sh.layers[SlotQ] = nil
	sh.state = ShareLive
	sh.stateChangedLocked()
	sh.mu.Unlock()
	if calls := report(time.Millisecond); !slices.Equal(calls, []string{"updated s_1 stalled f+q", "updated s_1 live f"}) {
		t.Errorf("room events = %v", calls)
	}

	// A change right behind a state change, before the ticker has reported it: the state change goes out with the
	// share as it was, and the change waits its 250 ms.
	third := testShare(t, s, "s_3")
	h, _ := testLayer(t, third, SlotF, 0xf03)
	third.keyframe(h, ProfileHigh)
	testLayer(t, third, SlotAudio, 0xac)
	if calls := report(0); !slices.Equal(calls, []string{"updated s_3 live f"}) {
		t.Errorf("room events = %v", calls)
	}
	if calls := report(tickInterval - time.Millisecond); len(calls) != 0 {
		t.Errorf("room events 249 ms after the state change: %v", calls)
	}
	if calls := report(time.Millisecond); !slices.Equal(calls, []string{"updated s_3 live f+audio"}) {
		t.Errorf("room events = %v", calls)
	}

	// The end: a state change the ticker hadn't reported yet is reported first, a debounced change is dropped, and
	// nothing follows ShareEnded, whatever arrives late.
	second := testShare(t, s, "s_2")
	g, _ := testLayer(t, second, SlotF, 0xf01)
	second.keyframe(g, ProfileHigh)
	testLayer(t, second, SlotAudio, 0xab)
	if err := s.StopShare("s_2", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	if calls := ev.take(); !slices.Equal(calls, []string{"updated s_2 live f", "ended s_2 stopped"}) {
		t.Errorf("room events at the end = %v", calls)
	}
	second.keyframe(g, ProfileConstrainedBaseline)
	second.detach(g)
	if calls := report(time.Second); len(calls) != 0 {
		t.Errorf("room events after ShareEnded: %v", calls)
	}
	if second.awaitsKeyframe.Load() || sh.awaitsKeyframe.Load() {
		t.Error("an ended or live share still asks for keyframes")
	}
}

// TestTicker: the SFU's ticker starts with the first Join, reports a state change at once and a later change within
// a few ticks, and stops with Close. (That it measures the layers once a second shows in TestBasicForwarding.)
func TestTicker(t *testing.T) {
	tr := loopbackTransport(t, false)
	ev := &eventLog{}
	s, err := New(Config{Transport: tr}, Deps{Events: ev})
	if err != nil {
		t.Fatal(err)
	}
	if s.ticking {
		t.Error("New started the ticker")
	}
	sh := testShare(t, s, "s_1")
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	wait := func(want ...string) {
		t.Helper()
		var got []string
		deadline := time.Now().Add(5 * time.Second)
		for len(got) < len(want) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			got = append(got, ev.take()...)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("room events = %v, want %v", got, want)
		}
	}
	start := time.Now()
	f.handleRTP(rtpPkt(1, 1, false, keyPayload(t)), ProfileHigh, monoNow())
	wait("updated s_1 live f")
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("the state change was reported after %v, want at once, not at the next tick", took)
	}
	testLayer(t, sh, SlotAudio, 0xaa)
	wait("updated s_1 live f+audio")
	if took := time.Since(start); took < tickInterval {
		t.Errorf("the audio change was reported %v after the state change, want at least %v later", took, tickInterval)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.tickDone:
	default:
		t.Error("the ticker runs after Close")
	}
	wait("ended s_1 server_shutdown")
}

// TestKeyframeRequests: who asks a publisher for a keyframe (02 §9.7), on a real sub PC and without any media, so
// that only the events themselves can have asked: the sub PC reaching connected (S4 finding 1), a DownTrack bound on
// a PC that is already connected, and a new target. A pause, a subscription the viewer can't receive yet and an
// audio track ask for nothing.
func TestKeyframeRequests(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	one, two := testShare(t, s, "s_1"), testShare(t, s, "s_2")
	f1, pubF1 := testLayer(t, one, SlotF, 0xf01)
	q1, pubQ1 := testLayer(t, one, SlotQ, 0xa01)
	_, pubA1 := testLayer(t, one, SlotAudio, 0xaa1)
	f2, pubF2 := testLayer(t, two, SlotF, 0xf02)
	asked := func(l *Layer) uint64 { return l.stats.pliSent.Load() + l.stats.pliThrottled.Load() }
	subscribe := func(u SubscriptionUpdate) {
		t.Helper()
		errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{u})
		if err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions(%+v) = %v, %v", u, errs, err)
		}
	}
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	answer := func(neg uint32) {
		t.Helper()
		offer := sig.waitOffers(t, int(neg))[neg-1]
		if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.sdp}); err != nil {
			t.Fatal(err)
		}
		ans, err := client.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := viewer.HandleAnswer(ctx, PCSub, 1, neg, completeDescription(t, client, ans)); err != nil {
			t.Fatal(err)
		}
	}
	waitPLI := func(what string, rec *rtcpRecorder, ssrc uint32) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for len(rec.plis()) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("no PLI %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // anything that would ask twice has done so by now
		if plis := rec.plis(); len(plis) != 1 || plis[0].MediaSSRC != ssrc {
			t.Errorf("PLIs %s = %+v, want one for SSRC %#x", what, plis, ssrc)
		}
	}

	// A subscription, negotiated: nothing is asked while the viewer can't receive. Then the PC connects: one PLI for
	// the layer the viewer asked for, although no packet ever arrived that could have waited for a keyframe.
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityHigh, Audio: true})
	if n := asked(f1); n != 0 {
		t.Errorf("%d keyframe requests at the subscription, before the viewer can receive", n)
	}
	answer(1)
	sig.waitPCStates(t, "sub/1:connected")
	waitPLI("when the sub PC connected", pubF1, 0xf01)
	if n := len(pubQ1.plis()) + len(pubA1.plis()) + len(pubF2.plis()); n != 0 {
		t.Errorf("%d PLIs for layers the viewer doesn't get", n)
	}

	// A second subscription on the connected PC: its DownTrack asks when Pion binds it.
	subscribe(SubscriptionUpdate{Share: "s_2", Video: QualityHigh})
	if n := asked(f2); n != 0 {
		t.Errorf("%d keyframe requests for a DownTrack that isn't negotiated yet", n)
	}
	answer(2)
	waitPLI("when the DownTrack was bound on a connected PC", pubF2, 0xf02)

	// A new target asks for the new layer's keyframe at once; a pause asks for nothing; a resume asks again.
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityLow, Audio: true})
	if plis := pubQ1.plis(); len(plis) != 1 || plis[0].MediaSSRC != 0xa01 {
		t.Errorf("PLIs for q after the viewer asked for low = %+v", plis)
	}
	before := asked(f1) + asked(q1)
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityOff, Audio: true})
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityOff})
	if n := asked(f1) + asked(q1) - before; n != 0 {
		t.Errorf("%d keyframe requests for a pause", n)
	}
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityHigh})
	if n := asked(f1) + asked(q1) - before; n != 1 {
		t.Errorf("%d keyframe requests for a resume, want 1 (sent, or throttled when the last PLI is under 500 ms old)", n)
	}
	subscribe(SubscriptionUpdate{Share: "s_1", Video: QualityHigh}) // the same again changes nothing
	if n := asked(f1) + asked(q1) - before; n != 1 {
		t.Errorf("%d keyframe requests after an unchanged subscription, want still 1", n)
	}
	if n := len(pubA1.plis()); n != 0 {
		t.Errorf("%d PLIs for audio", n)
	}
	if n := len(sig.sent()); n != 2 {
		t.Errorf("%d sub offers, want 2: changing quality never renegotiates", n)
	}
}
