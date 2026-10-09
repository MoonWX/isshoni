package sfu_test

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of repair and sync (README S63): integration 4, 5, 6 and 19 of 02 §17, through
// sfutest.Harness with real PeerConnections, fake media, and clients whose UDP a FaultConn loses packets on. The
// unit tests of the same code are rtx_test.go's.

// sentMedia is a publish.Source that keeps what the Publisher pulled from a fake.Source: the originals that a test
// compares a viewer's packets with.
type sentMedia struct {
	*fake.Source
	mu   sync.Mutex
	pkts []fake.Packet
}

func (s *sentMedia) Next(ctx context.Context) (fake.Packet, error) {
	p, err := s.Source.Next(ctx)
	if err == nil {
		s.mu.Lock()
		s.pkts = append(s.pkts, p)
		s.mu.Unlock()
	}
	return p, err
}

// payloads returns the hash of every RTP payload the Publisher made of what it pulled so far: for a video layer by
// the capture time of the frame, in packet order, and for audio (layer "") by the packet's capture time. A Publisher
// packetizes each layer with a payloader of its own, from the layer's first frame on, and so does this.
func (s *sentMedia) payloads(layer string) map[int64][]uint64 {
	s.mu.Lock()
	pkts := slices.Clone(s.pkts)
	s.mu.Unlock()
	out := map[int64][]uint64{}
	var pay codecs.H264Payloader
	for _, p := range pkts {
		switch {
		case p.Kind == fake.Audio && layer == "":
			out[p.CaptureNS] = []uint64{sfutest.PayloadHash(p.Data)}
		case p.Kind == fake.Video && p.Layer == layer && layer != "":
			for _, payload := range pay.Payload(publish.PayloadMTU, p.Data) {
				out[p.CaptureNS] = append(out[p.CaptureNS], sfutest.PayloadHash(payload))
			}
		}
	}
	return out
}

// repaired returns a viewer's packets as its decoder gets them after repairs: without the second copies of sequence
// numbers it already had, in the order of their sequence numbers (a retransmission arrives after newer packets).
func repaired(pkts []sfutest.Packet) []sfutest.Packet {
	return sfutest.InSeqOrder(slices.DeleteFunc(slices.Clone(pkts), func(p sfutest.Packet) bool { return p.Dup }))
}

// forwarded returns what a viewer got on a track's media stream, in the order of the sequence numbers: the packets
// the SFU forwarded, without the retransmissions that the viewer's own NACKs brought on the RTX stream. Whether a
// packet was the first copy of its sequence number to arrive says nothing here (Pion may hand a retransmission over
// before the forwarded packet it repeats), so the check that follows finds a sequence number that was forwarded
// twice by itself.
func forwarded(pkts []sfutest.Packet) []sfutest.Packet {
	out := slices.DeleteFunc(slices.Clone(pkts), func(p sfutest.Packet) bool { return p.RTX })
	for i := range out {
		out[i].Dup = false
	}
	return sfutest.InSeqOrder(out)
}

// checkOriginals checks that every whole frame among a viewer's video packets has exactly the payloads the
// publisher sent for it, in order; it returns the number of frames compared. want is sentMedia.payloads of the
// layer.
func checkOriginals(t *testing.T, what string, pkts []sfutest.Packet, want map[int64][]uint64) (frames int) {
	t.Helper()
	type frame struct {
		capture int64
		marked  bool
		pkts    []sfutest.Packet
	}
	byTS := map[uint32]*frame{}
	var order []uint32
	for _, p := range sfutest.WithoutPartialTail(repaired(pkts)) {
		f := byTS[p.TS]
		if f == nil {
			f = &frame{}
			byTS[p.TS] = f
			order = append(order, p.TS)
		}
		if p.HasMark {
			f.capture, f.marked = p.Mark.CaptureNS, true
		}
		f.pkts = append(f.pkts, p)
	}
	for _, ts := range order {
		f := byTS[ts]
		if !f.marked {
			t.Errorf("%s: the frame with timestamp %d has no marker", what, ts)
			continue
		}
		got := make([]uint64, len(f.pkts))
		for i, p := range f.pkts {
			got[i] = p.Hash
		}
		if !slices.Equal(got, want[f.capture]) {
			t.Errorf("%s: the frame captured at %v arrived as %d packets that aren't the %d the publisher sent", what,
				time.Duration(f.capture), len(got), len(want[f.capture]))
			continue
		}
		frames++
	}
	return frames
}

// mediaLoss returns a FaultConn filter for the SFU's socket that loses media packets on their way to one viewer,
// each sequence number of each stream once: the video packets (a viewer's H.264 payload types are the even ones
// from 96 to 104) whose sequence number modulo 40 is 7, or 20, 21 or 22, and the audio packets (payload type 111)
// where it is 3 modulo 10. Retransmissions on the RTX stream (the odd payload types) always pass. It also returns a
// function that reports how many packets of each kind were lost, and how many went out on the RTX stream.
func mediaLoss(viewer net.Addr) (drop func(sfutest.Direction, net.Addr, []byte) bool, counts func() (video, audio, rtx int)) {
	var (
		mu                sync.Mutex
		lost              = map[[2]uint32]bool{}
		video, audio, rtx int
	)
	to := viewer.String()
	drop = func(dir sfutest.Direction, peer net.Addr, b []byte) bool {
		ssrc, seq, pt, ok := sfutest.RTPHeader(b)
		if dir != sfutest.Outgoing || !ok || peer.String() != to {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		isVideo := pt >= 96 && pt <= 104 && pt%2 == 0
		switch {
		case pt >= 97 && pt <= 105 && pt%2 == 1:
			rtx++
			return false
		case isVideo && (seq%40 == 7 || (seq%40 >= 20 && seq%40 <= 22)):
		case pt == 111 && seq%10 == 3:
		default:
			return false
		}
		key := [2]uint32{ssrc, uint32(seq)}
		if lost[key] {
			return false // the second time it is the plain retransmission
		}
		lost[key] = true
		if isVideo {
			video++
		} else {
			audio++
		}
		return true
	}
	return drop, func() (int, int, int) {
		mu.Lock()
		defer mu.Unlock()
		return video, audio, rtx
	}
}

// subscription returns a Conn's subscription to a share as the SFU has it.
func subscription(t *testing.T, c *sfu.Conn, share sfu.ShareID) sfu.SubscriptionState {
	t.Helper()
	subs, err := c.Subscriptions(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range subs {
		if s.Share == share {
			return s
		}
	}
	t.Fatalf("no subscription to %s", share)
	return sfu.SubscriptionState{}
}

// TestNACKFromCache is integration 4 of 02 §17. Packets of the SFU's to one viewer are lost on the way, video and
// audio, single ones and three in a row. The viewer's NACKs are answered from the packet cache: video on the RTX
// stream, audio as the packet again. Every gap closes, nothing is finally lost, what arrives is byte for byte what
// the publisher sent, and the cache never misses. A second viewer of the same share notices nothing.
func TestNACKFromCache(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{FaultyUDP: true})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_nack")
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	sent := &sentMedia{Source: newLongGOPSource(t)}
	pub, err := publish.New(publish.Options{Source: sent, Settings: sfutest.LoopbackSettings(), Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)

	// Two viewers of the full layer. One has a socket of its own, so that the SFU's socket can tell its packets
	// from the other's by their destination.
	se, viewerSocket := faultySettings(t)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, se)
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
	otherConn, otherSig := join(t, h, "carol", "c-other", sfu.RoleViewer)
	other := newViewer(t, sfutest.LoopbackSettings())
	otherSig.Attach(otherConn, other)
	subscribe(t, otherConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	otherVideo := streamTrack(t, other, share, webrtc.RTPCodecTypeVideo)
	// Both streams of the first viewer have begun (a receiver doesn't miss what came before the first packet it
	// got): from here on the SFU's socket loses media on its way to that viewer, for five seconds, about 30 video and
	// 25 audio packets.
	drop, lostSoFar := mediaLoss(viewerSocket.LocalAddr())
	h.Wire.SetDrop(drop)
	if err := video.Wait(ctx, func(r *sfutest.Recorder) bool { return len(sfutest.Frames(r.Packets())) >= 150 }); err != nil {
		t.Fatalf("%v (answer errors %v)", err, viewSig.Errs())
	}
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 250 }); err != nil {
		t.Fatal(err)
	}
	h.Wire.SetDrop(nil)
	lostVideo, lostAudio, _ := lostSoFar()
	// The last repairs arrive, and a second later the Recorders would count what is still missing as lost.
	time.Sleep(sfutest.FinalLossAfter + 300*time.Millisecond)

	vs, as := video.Stats(), audio.Stats()
	_, _, rtxArrived := lostSoFar()
	sub := subscription(t, viewConn, share)
	t.Logf("lost on the wire: %d video, %d audio; viewer video %+v; audio %+v; RTX packets arrived %d; SFU video %+v, audio %+v",
		lostVideo, lostAudio, vs, as, rtxArrived, sub.VideoRepair, sub.AudioRepair)
	if lostVideo < 10 || lostAudio < 10 {
		t.Fatalf("the wire lost %d video and %d audio packets, want at least 10 of each for the test to mean anything",
			lostVideo, lostAudio)
	}

	// Video: every gap closed through the RTX stream.
	if vs.Gaps < lostVideo || vs.Recovered != vs.Gaps || vs.RecoveredRTX != vs.Gaps || vs.Lost != 0 {
		t.Errorf("video: %d gaps for %d lost packets, %d recovered, %d of them by RTX, %d lost for good; want every gap "+
			"closed by RTX", vs.Gaps, lostVideo, vs.Recovered, vs.RecoveredRTX, vs.Lost)
	}
	if rtxArrived < lostVideo {
		t.Errorf("%d packets went out on the RTX stream for %d lost video packets", rtxArrived, lostVideo)
	}
	vp := video.Packets()
	for _, p := range vp {
		// A second copy can only be a retransmission that a repeated NACK asked for: nothing is forwarded twice.
		if p.Dup && !p.RTX {
			t.Fatalf("video seq %d arrived twice on the media stream", p.Seq)
		}
		if p.PT != 104 {
			t.Fatalf("video packet %+v: Pion hands RTX packets over as packets of the media payload type 104", p)
		}
	}
	checkVideo(t, "video", repaired(vp))
	want := sent.payloads("f")
	if n := checkOriginals(t, "video", vp, want); n < 140 {
		t.Errorf("%d video frames compared with the publisher's, want the 150 the viewer waited for", n)
	}

	// Audio: every gap closed by the packet itself, on the media stream.
	if as.Gaps < lostAudio || as.Recovered != as.Gaps || as.RecoveredRTX != 0 || as.RTX != 0 || as.Lost != 0 {
		t.Errorf("audio: %d gaps for %d lost packets, %d recovered, %d packets by RTX, %d lost for good; want every gap "+
			"closed by a plain retransmission", as.Gaps, lostAudio, as.Recovered, as.RTX, as.Lost)
	}
	ap := repaired(audio.Packets())
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	wantAudio := sent.payloads("")
	for _, p := range ap {
		if !p.HasMark || !slices.Equal(wantAudio[p.Mark.CaptureNS], []uint64{p.Hash}) {
			t.Fatalf("audio seq %d (marker %+v) isn't the packet the publisher sent", p.Seq, p.Mark)
		}
	}

	// The SFU: every NACKed sequence number was served from the cache, and the cache never missed.
	for kind, st := range map[string]struct {
		repair sfu.RepairState
		lost   int
	}{"video": {sub.VideoRepair, lostVideo}, "audio": {sub.AudioRepair, lostAudio}} {
		if st.repair.Missed != 0 || st.repair.Dropped != 0 {
			t.Errorf("%s: the cache missed %d NACKed packets and the RTX queue dropped %d, want none", kind,
				st.repair.Missed, st.repair.Dropped)
		}
		if int(st.repair.Served) < st.lost || st.repair.NACKs < st.repair.Served {
			t.Errorf("%s: %d sequence numbers NACKed and %d served for %d lost packets", kind, st.repair.NACKs,
				st.repair.Served, st.lost)
		}
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offer: %v", errs)
	}
	// How soon a repeated NACK is answered again goes by the viewer's round-trip time, which the SFU reads from the
	// sub PC's candidate pair when the viewer's RTCP comes in: on loopback, well under the 20 ms that count anyway.
	if pc := pcOf(t, viewConn, sfu.PCSub); pc.RTT <= 0 || pc.RTT > 100*time.Millisecond {
		t.Errorf("the sub PC's round-trip time is %v, want what ICE measured on loopback", pc.RTT)
	}

	// The other viewer of the layer: nothing lost, nothing asked for, nothing sent twice.
	otherSub := subscription(t, otherConn, share)
	if st := otherVideo.Stats(); st.Gaps != 0 || st.NACKs != 0 || st.Duplicates != 0 || st.RTX != 0 ||
		otherSub.VideoRepair.NACKs != 0 || otherSub.AudioRepair.NACKs != 0 {
		t.Errorf("the viewer whose packets all arrive: %+v, NACKs at the SFU %d and %d; want no gap and no repair",
			st, otherSub.VideoRepair.NACKs, otherSub.AudioRepair.NACKs)
	}
	checkVideo(t, "the other viewer's video", otherVideo.Packets())
}

// syncSource returns fake media for the A/V sync test: the two layers and audio of the other tests, with a flash and
// a beep every 200 ms (a multiple of the audio packet's 20 ms and of both layers' frame intervals, so they share
// their capture instants exactly), and keyframes only on request.
func syncSource(t *testing.T) *fake.Source {
	t.Helper()
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Minute, Audio: true, FlashEvery: 200 * time.Millisecond, Seed: 63,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// TestSRTranslationAndAVOffset is integration 5 of 02 §17: the acceptance check of the plan's A/V sync target as far
// as the SFU is concerned (02 §9.6). A viewer switches layers ten times and turns the share's audio off and on five
// times. Flash and beep, which the publisher captured at the same instant, stay within 5 ms of each other on the
// viewer's clocks, which it has from the sender reports the SFU forwards: the publisher's NTP times, with RTP times
// on the viewer's own timelines.
func TestSRTranslationAndAVOffset(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := ctxFor(t, 90*time.Second)
	const share = sfu.ShareID("s_sync")
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	pub := newPublisher(t, syncSource(t), sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	want := func(q sfu.Quality, sound bool) sfu.SubscriptionUpdate {
		return sfu.SubscriptionUpdate{Share: share, Video: q, Audio: sound}
	}
	subscribe(t, viewConn, want(sfu.QualityHigh, true))
	video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	// Each stream has had two sender reports: the first has reached the SFU for certain, and from here on a switch
	// finds a report on both of its layers.
	eventually(t, func() bool {
		return layerStats(t, pub, "f").SRs >= 2 && layerStats(t, pub, "q").SRs >= 2 && layerStats(t, pub, "").SRs >= 2
	}, func() string { return fmt.Sprintf("the publisher's stats are %+v", pub.Stats()) })

	// Ten switches, f → q → f → …; with every second one the audio goes off, and comes back on once the switch is done.
	sound := func(n int) {
		t.Helper()
		before := audio.Stats().Packets
		if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= before+n }); err != nil {
			t.Fatalf("the audio doesn't flow: %v", err)
		}
	}
	for i := range 10 {
		quality, rid, frames := sfu.QualityLow, "q", 8
		if i%2 == 1 {
			quality, rid, frames = sfu.QualityHigh, "f", 16
		}
		mark := len(video.Packets())
		subscribe(t, viewConn, want(quality, i%2 == 0))
		waitFrames(t, video, mark, rid, frames)
		if i%2 == 1 {
			// Half a second without the audio, at least: the PLI throttle (500 ms per layer) doesn't keep the next
			// switch waiting either way.
			time.Sleep(200 * time.Millisecond)
			subscribe(t, viewConn, want(quality, true))
		}
		sound(15)
	}
	// One more second on the last layer, with a sender report of each stream in it.
	mark := len(video.Packets())
	waitFrames(t, video, mark, "f", 30)
	sound(50)

	// ---- what the viewer got ----
	vp := checkVideo(t, "video", video.Packets())
	if runs := layerRuns(vp); runs != "f q f q f q f q f q f" {
		t.Errorf("the viewer got the layers %q, want eleven runs for ten switches", runs)
	}
	ap := audio.Packets()
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if pauses := checkAudioRuns(t, ap); pauses != 5 {
		t.Errorf("the audio has %d pauses, want 5", pauses)
	}
	if vs, as := video.Stats(), audio.Stats(); vs.Gaps != 0 || as.Gaps != 0 {
		t.Errorf("gaps on loopback: video %d, audio %d", vs.Gaps, as.Gaps)
	}

	// Across every switch the timeline stays the capture timeline, to a fraction of a frame: the timestamps are
	// aligned through the two layers' sender reports, not by when the keyframe happened to arrive (02 §9.4).
	frames := sfutest.Frames(vp)
	worst := time.Duration(0)
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		if !a.HasMark || !b.HasMark || a.Mark.RID == b.Mark.RID {
			continue
		}
		d := time.Duration(int32(b.TS-a.TS))*time.Second/90000 - time.Duration(b.Mark.CaptureNS-a.Mark.CaptureNS)
		worst = max(worst, d.Abs())
		if d.Abs() > time.Millisecond {
			t.Errorf("switch %s → %s at seq %d: the timestamp is %v off the capture time", a.Mark.RID, b.Mark.RID, b.FirstSeq, d)
		}
	}

	// Flash and beep.
	samples, err := viewer.AVOffset(string(share))
	if err != nil {
		t.Fatal(err)
	}
	perLayer := map[string]int{}
	offset := time.Duration(0)
	for _, s := range samples {
		perLayer[s.Layer]++
		offset = max(offset, s.Offset.Abs())
		if s.Offset.Abs() > 5*time.Millisecond {
			t.Errorf("A/V offset %v at frame %d of layer %s, want at most 5 ms", s.Offset, s.Frame, s.Layer)
		}
	}
	if perLayer["f"] < 10 || perLayer["q"] < 10 {
		t.Errorf("flash and beep pairs per layer: %v, want at least 10 on each", perLayer)
	}

	// The sender reports. Each one names an instant of the publisher's capture clock (NTP) and the timestamp that
	// instant has on the track. The publisher reports the last frame it sent, so where the viewer got that frame, the
	// report must name exactly the frame's timestamp, and the same clock for every track and layer: the NTP time
	// minus the frame's capture time is one constant.
	type ref struct{ capture int64 }
	var (
		origin  int64
		matched = map[webrtc.RTPCodecType]int{}
	)
	for _, r := range []*sfutest.Recorder{video, audio} {
		clock := int64(90000)
		if r.Kind() == webrtc.RTPCodecTypeAudio {
			clock = 48000
		}
		capture := map[uint32]ref{}
		for _, p := range r.Packets() {
			if p.HasMark {
				capture[p.TS] = ref{p.Mark.CaptureNS}
			}
		}
		srs := r.SRs()
		for i, sr := range srs {
			if sr.Packets == 0 || sr.Octets == 0 {
				t.Errorf("%s SR %d counts %d packets and %d bytes", r.Kind(), i, sr.Packets, sr.Octets)
			}
			if i > 0 {
				// Report to report, the RTP time advances as the NTP time does: the viewer's timeline has no step. A
				// video switch may add a tick (11 µs), when the new layer's keyframe has the capture time of a frame
				// that was forwarded already.
				prev := srs[i-1]
				ntp := sr.Time().Sub(prev.Time())
				rtp := time.Duration(int32(sr.RTP-prev.RTP)) * time.Second / time.Duration(clock)
				if d := (rtp - ntp).Abs(); d > 200*time.Microsecond {
					t.Errorf("%s SR %d: the RTP time advances by %v and the NTP time by %v", r.Kind(), i, rtp, ntp)
				}
			}
			f, ok := capture[sr.RTP]
			if !ok {
				continue // it reports a frame from before the viewer's stream, or its current layer, began
			}
			matched[r.Kind()]++
			at := sr.Time().UnixNano() - f.capture
			if origin == 0 {
				origin = at
			}
			if d := time.Duration(at - origin).Abs(); d > 20*time.Microsecond {
				t.Errorf("%s SR %d puts the frame captured at %v on a clock %v off the other reports'", r.Kind(), i,
					time.Duration(f.capture), d)
			}
		}
		// A report a second, and one more right after every start of an epoch.
		if r.Kind() == webrtc.RTPCodecTypeVideo && len(srs) < 15 || len(srs) < 8 {
			t.Errorf("%d %s sender reports, want one a second and one with every switch or resume", len(srs), r.Kind())
		}
	}
	if matched[webrtc.RTPCodecTypeVideo] < 3 || matched[webrtc.RTPCodecTypeAudio] < 3 {
		t.Errorf("sender reports that name a frame the viewer got: %v, want at least 3 per track", matched)
	}
	gotVideo, gotAudio := len(video.SRs()), len(audio.SRs()) // before the SFU's counts: the reports keep coming
	sub := subscription(t, viewConn, share)
	if int(sub.VideoRepair.SRs) < gotVideo || int(sub.AudioRepair.SRs) < gotAudio {
		t.Errorf("the SFU counts %d video and %d audio sender reports, the viewer got %d and %d", sub.VideoRepair.SRs,
			sub.AudioRepair.SRs, gotVideo, gotAudio)
	}
	t.Logf("%d flash and beep pairs (%v), worst offset %v; worst timestamp error at a switch %v; sender reports: %d video, "+
		"%d audio, of which %v name a frame the viewer got", len(samples), perLayer, offset, worst, len(video.SRs()),
		len(audio.SRs()), matched)
	if n := len(viewSig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want 1", n)
	}
}

// TestPaddingNoGaps is integration 6 of 02 §17: a publisher that pads its video on the media stream, as a browser
// without RTX does, sends packets that take sequence numbers and carry nothing. The SFU forwards none of them and
// closes the gaps: its viewers see continuous sequence numbers, on the layer they watch and across a switch, and
// never send a NACK.
func TestPaddingNoGaps(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_pad")
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	pub, err := publish.New(publish.Options{
		Source: newLongGOPSource(t), Settings: sfutest.LoopbackSettings(), Audio: true, Padding: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)

	type client struct {
		conn   *sfu.Conn
		sig    *sfutest.DirectSignaler
		viewer *sfutest.Viewer
		video  *sfutest.Recorder
	}
	watch := func(user sfu.UserID, q sfu.Quality) *client {
		t.Helper()
		c := &client{viewer: newViewer(t, sfutest.LoopbackSettings())}
		c.conn, c.sig = join(t, h, user, sfu.ConnID("c-"+user), sfu.RoleViewer)
		c.sig.Attach(c.conn, c.viewer)
		subscribe(t, c.conn, sfu.SubscriptionUpdate{Share: share, Video: q, Audio: true})
		c.video = streamTrack(t, c.viewer, share, webrtc.RTPCodecTypeVideo)
		return c
	}
	bob, carol := watch("bob", sfu.QualityHigh), watch("carol", sfu.QualityLow)
	waitFrames(t, bob.video, 0, "f", 60)
	waitFrames(t, carol.video, 0, "q", 30)
	// Bob goes to the preview layer and back: the gaps close in every epoch.
	mark := len(bob.video.Packets())
	subscribe(t, bob.conn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, bob.video, mark, "q", 20)
	mark = len(bob.video.Packets())
	subscribe(t, bob.conn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	waitFrames(t, bob.video, mark, "f", 45)

	// The publisher did pad, and the padding reached the SFU: it read more packets of each layer than the layer has
	// media packets, even by the publisher's later count.
	tracks, err := pubConn.PubTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range []string{"f", "q"} {
		st := layerStats(t, pub, rid)
		i := slices.IndexFunc(tracks, func(tr sfu.PubTrackState) bool { return tr.RID == rid })
		if i < 0 {
			t.Fatalf("the SFU has no track for layer %s: %+v", rid, tracks)
		}
		if st.Padding < 20 || tracks[i].Packets <= st.Packets {
			t.Errorf("layer %s: the publisher sent %d media and %d padding packets, the SFU read %d; want padding, and all "+
				"of it read", rid, st.Packets, st.Padding, tracks[i].Packets)
		}
	}

	for name, c := range map[string]*client{"bob": bob, "carol": carol} {
		pkts := checkVideo(t, name+"'s video", c.video.Packets())
		for _, p := range pkts {
			if p.Size == 0 || p.Dup || p.RTX {
				t.Fatalf("%s got video packet %+v, want media only, once", name, p)
			}
		}
		st := c.video.Stats()
		if st.Gaps != 0 || st.Lost != 0 || st.NACKs != 0 {
			t.Errorf("%s's video: %d gaps, %d lost, %d NACKs sent; want none", name, st.Gaps, st.Lost, st.NACKs)
		}
		sub := subscription(t, c.conn, share)
		if sub.VideoRepair.NACKs != 0 || sub.AudioRepair.NACKs != 0 || sub.VideoDrops != 0 {
			t.Errorf("%s: the SFU got NACKs for %d video and %d audio packets and dropped %d; want none", name,
				sub.VideoRepair.NACKs, sub.AudioRepair.NACKs, sub.VideoDrops)
		}
		if errs := c.sig.Errs(); len(errs) != 0 {
			t.Errorf("%s: answering the sub offer: %v", name, errs)
		}
	}
	if runs := layerRuns(sfutest.WithoutPartialTail(bob.video.Packets())); runs != "f q f" {
		t.Errorf("bob got the layers %q, want f, q and f again", runs)
	}
	t.Logf("publisher padding: f %d, q %d; bob %d packets, carol %d", layerStats(t, pub, "f").Padding,
		layerStats(t, pub, "q").Padding, bob.video.Stats().Packets, carol.video.Stats().Packets)
}

// TestRTXDuplicates is integration 19 of 02 §17. The publisher resends packets it already delivered on its RTX
// stream, as Chrome does for padding and probes. The SFU drops and counts those copies (02 §9.1): each viewer gets
// every sequence number once and has nothing to ask for. Then the publisher's link loses packets that no second copy
// covers: the SFU's NACKs get them resent on the same RTX stream, and those reach every viewer, once.
func TestRTXDuplicates(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_rtx")
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	se, wire := faultySettings(t)
	pub, err := publish.New(publish.Options{Source: newLongGOPSource(t), Settings: se, Audio: true, RedundantRTX: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)

	type client struct {
		conn   *sfu.Conn
		viewer *sfutest.Viewer
		video  *sfutest.Recorder
		rid    string
	}
	watch := func(user sfu.UserID, q sfu.Quality, rid string) *client {
		t.Helper()
		c := &client{viewer: newViewer(t, sfutest.LoopbackSettings()), rid: rid}
		conn, sig := join(t, h, user, sfu.ConnID("c-"+user), sfu.RoleViewer)
		c.conn = conn
		sig.Attach(conn, c.viewer)
		subscribe(t, conn, sfu.SubscriptionUpdate{Share: share, Video: q, Audio: true})
		c.video = streamTrack(t, c.viewer, share, webrtc.RTPCodecTypeVideo)
		return c
	}
	viewers := map[string]*client{"bob": watch("bob", sfu.QualityHigh, "f"), "carol": watch("carol", sfu.QualityLow, "q")}
	redundant := func(layer string) uint64 { return layerStats(t, pub, layer).Redundant }

	// ---- second copies of delivered packets ----
	waitFrames(t, viewers["bob"].video, 0, "f", 90)
	waitFrames(t, viewers["carol"].video, 0, "q", 45)
	copies := map[string]uint64{"f": redundant("f"), "q": redundant("q")}
	if copies["f"] < 60 || copies["q"] < 30 {
		t.Fatalf("the publisher sent %v second copies, want one per frame after its first frames", copies)
	}
	// Every one of them reached the SFU, which dropped it as a duplicate.
	var ingress map[string]sfu.LayerState
	eventually(t, func() bool {
		ingress = h.SFU.Ingress(share)
		return ingress["f"].Duplicates >= copies["f"] && ingress["q"].Duplicates >= copies["q"]
	}, func() string { return fmt.Sprintf("the SFU counts %+v, the publisher sent %v copies", ingress, copies) })
	for name, c := range viewers {
		// Every sequence number once, in whole frames: none of the copies was forwarded.
		checkVideo(t, name+"'s video", forwarded(c.video.Packets()))
		// And the copies give the viewer nothing to ask for. (Pion reads a track's RTX stream ahead of the track, so
		// the SFU can get a second copy before the packets that left the publisher just before it, and then forwards
		// it first: a packet that arrives late for that closes its gap at once, and is all a NACK may be about.)
		st := c.video.Stats()
		sub := subscription(t, c.conn, share)
		if st.Lost != 0 || st.Recovered != st.Gaps || st.NACKs > st.Gaps || int(sub.VideoRepair.NACKs) > st.NACKs {
			t.Errorf("%s's video with a publisher that sends second copies: %d gaps, %d recovered, %d lost, %d NACKs sent "+
				"(%d at the SFU); want nothing lost and nothing asked for", name, st.Gaps, st.Recovered, st.Lost, st.NACKs,
				sub.VideoRepair.NACKs)
		}
		t.Logf("%s before the loss: %d packets, %d out of order, %d NACKs", name, st.Packets, st.Gaps, st.NACKs)
	}
	if st := layerStats(t, pub, "f"); st.NACKs != 0 || st.Retransmitted != 0 {
		t.Errorf("the SFU sent the publisher %d NACKs on a loss-free link (%d packets resent)", st.NACKs, st.Retransmitted)
	}

	// ---- a packet lost on the publisher's link, recovered by RTX ----
	// The publisher's video packets are payload type 96, its RTX packets 97. Second copies cover the last packet of a
	// frame (the marker bit), so the wire loses others: only a NACK brings those back. (One in eight of them, by the
	// sequence number without its last bit: in a stream of two-packet frames they are all odd, or all even.)
	var (
		mu   sync.Mutex
		lost = map[[2]uint32]bool{}
	)
	wire.SetDrop(func(dir sfutest.Direction, _ net.Addr, b []byte) bool {
		ssrc, seq, pt, ok := sfutest.RTPHeader(b)
		if dir != sfutest.Outgoing || !ok || pt != 96 || b[1]&0x80 != 0 || (seq>>1)%8 != 3 {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		lost[[2]uint32{ssrc, uint32(seq)}] = true
		return true
	})
	lostSoFar := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(lost)
	}
	eventually(t, func() bool { return lostSoFar() >= 20 }, func() string {
		return fmt.Sprintf("the publisher's wire lost %d packets", lostSoFar())
	})
	wire.SetDrop(nil)
	dropped := lostSoFar()
	time.Sleep(sfutest.FinalLossAfter + 300*time.Millisecond)

	// Only a NACK brings such a packet back, so each was asked for; and none more than ten times, although Pion's
	// NACK generator never learns that a packet came back on the RTX stream and would ask for it for as long as its
	// window holds the sequence number. This publisher resends on every NACK, as a browser nearly does.
	resent := int(layerStats(t, pub, "f").Retransmitted + layerStats(t, pub, "q").Retransmitted)
	if st := layerStats(t, pub, "f"); st.NACKs == 0 || resent < dropped || resent > 10*dropped {
		t.Errorf("the publisher got %d NACKs on f and resent %d packets for %d lost ones; want each asked for, at most "+
			"10 times", st.NACKs, resent, dropped)
	}
	for name, c := range viewers {
		st := c.video.Stats()
		if st.Lost != 0 || st.Recovered != st.Gaps {
			t.Errorf("%s's video after loss on the publisher's link: %d gaps, %d recovered, %d lost; want every packet "+
				"there in the end", name, st.Gaps, st.Recovered, st.Lost)
		}
		// What the SFU forwards arrives once. A viewer that asked the SFU for a packet in the moment it came in
		// may get it again as a retransmission, on the RTX stream: that copy is the answer to its own NACK.
		pkts := checkVideo(t, name+"'s video after the loss", forwarded(c.video.Packets()))
		for _, f := range sfutest.Frames(pkts) {
			if !f.HasMark || f.Mark.RID != c.rid {
				t.Fatalf("%s got a frame of layer %q", name, f.Mark.RID)
			}
		}
	}
	if bob := viewers["bob"].video.Stats(); bob.Gaps == 0 {
		t.Errorf("bob saw no gap while the publisher's link lost %d packets: the test lost nothing of the layer he watches", dropped)
	}
	after := h.SFU.Ingress(share)
	if after["f"].Duplicates <= ingress["f"].Duplicates {
		t.Errorf("the SFU counts %d duplicates on f after the loss, %d before; want the copies still counted",
			after["f"].Duplicates, ingress["f"].Duplicates)
	}
	t.Logf("second copies sent: f %d, q %d; duplicates dropped by the SFU: %+v; lost on the publisher's link: %d; "+
		"publisher f: %+v", redundant("f"), redundant("q"), after, dropped, layerStats(t, pub, "f"))
}
