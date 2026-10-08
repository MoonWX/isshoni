package sfu

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// 02 §17's throttle_test: a publisher gets at most one PLI per 500 ms and layer, whoever asks and however many do
// (02 §9.7; S4 switched in 42–106 ms with that).

// TestKeyframeThrottle: a hundred concurrent keyframe requests for one layer make one PLI, and the next one goes out
// 500 ms later, not before.
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

// TestKeyframeThrottlePerLayer: the throttle is each layer's own. Twenty viewers who switch to the preview layer in
// the same moment, and whose DownTracks then all wait for its keyframe and ask again with every packet (02 §9.7),
// cost the publisher one PLI for that layer per 500 ms; and a request for the full layer right after goes out all the
// same, because the preview layer's PLI doesn't throttle it.
func TestKeyframeThrottlePerLayer(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, pubF := testLayer(t, sh, SlotF, 0xf00)
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	sh.keyframe(f, ProfileHigh) // live: the layers' own read loops ask for nothing
	const viewers = 20
	dts := make([]*DownTrack, viewers)
	states := make([]rtpWriteState, viewers)
	for i := range dts {
		dts[i], _ = testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
		dts[i].binding.Load().writer = discardWriter{}
	}

	start := time.Now()
	var wg sync.WaitGroup
	for _, d := range dts {
		wg.Go(func() { d.setWant(QualityLow) })
	}
	wg.Wait()
	// Each of them now waits for q's keyframe: every packet of q that isn't one asks again.
	for seq := range uint16(5) {
		q.handleRTP(rtpPkt(seq, 3000*uint32(seq), true, deltaPayload), ProfileHigh, monoNow())
		for i, d := range dts {
			drain(d, &states[i])
		}
	}
	asked := q.stats.pliSent.Load() + q.stats.pliThrottled.Load()
	// One PLI, or one more per 500 ms that this took on a machine that is that slow.
	most := 1 + int(time.Since(start)/pliInterval)
	if n := len(pubQ.plis()); n < 1 || n > most || asked < viewers*6 {
		t.Errorf("%d viewers switching to q made %d requests and %d PLIs, want %d requests or more and 1 PLI (at most %d "+
			"in the time it took)", viewers, asked, n, viewers*6, most)
	}
	for _, pli := range pubQ.plis() {
		if pli.MediaSSRC != 0xa00 {
			t.Errorf("PLI for q names SSRC %#x", pli.MediaSSRC)
		}
	}
	if n := len(pubF.plis()); n != 0 {
		t.Errorf("%d PLIs for f, which nobody asked for", n)
	}
	// The full layer has its own 500 ms.
	dts[0].setWant(QualityHigh)
	if plis := pubF.plis(); len(plis) != 1 || plis[0].MediaSSRC != 0xf00 {
		t.Errorf("PLIs for f right after q's = %+v, want one: the layers are throttled apart", plis)
	}
}
