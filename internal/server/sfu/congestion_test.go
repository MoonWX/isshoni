package sfu_test

import (
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of M2's native uplink estimator (01 §10.1, C09) and of the publish PCs' SRTP replay window
// (01 D7, §8.4), through sfutest.Harness with real PeerConnections: a Go publisher of the desktop kind sends fake
// media from a UDP socket of its own, behind a bottleneck on the SFU's socket (FaultConn.SetInboundLink). The unit
// tests are uplink_native_test.go's.

// nativeSource returns fake media of f at fBps (640×360, 30 fps) and, when qBps isn't 0, q at qBps (320×180,
// 15 fps), with a GOP far longer than a test and no audio.
func nativeSource(t *testing.T, fBps, qBps int) *fake.Source {
	t.Helper()
	layers := []fake.VideoLayer{{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: fBps}}
	if qBps > 0 {
		layers = append(layers, fake.VideoLayer{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: qBps})
	}
	src, err := fake.New(fake.Config{Layers: layers, GOP: time.Minute, Seed: 28})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// nativePublisher starts a share on a new desktop Conn with the publisher role (the M2 core's, 01 D12) and publishes
// src's video layers on it from a UDP socket of its own, which it returns: the SFU's FaultConn sees the publisher at
// that socket's address, and a test can write to the SFU from it. It returns once the share is live with every
// layer.
func nativePublisher(t *testing.T, h *sfutest.Harness, share sfu.ShareID, src *fake.Source, layers ...string,
) (*sfutest.DirectSignaler, *publish.Publisher, net.PacketConn) {
	t.Helper()
	ctx := testCtx(t)
	c, sig, err := h.Join(sfu.JoinParams{
		Room: "lounge", Participant: "alice", User: "alice", Conn: "c-desktop", Role: sfu.RolePublisher,
		Client: sfu.ClientDesktop,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := sfu.StartShareParams{ID: share, Preset: sfu.PresetAuto, Source: sfu.SourceScreen}
	if _, err := c.StartShare(ctx, start); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	sock, err := lc.ListenPacket(ctx, "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := webrtc.NewICEUDPMux(nil, sock)
	t.Cleanup(func() { _ = mux.Close() }) // after the Publisher's cleanup, which runs first
	se := sfutest.LoopbackSettings()
	se.SetICEUDPMux(mux)
	pub, err := publish.New(publish.Options{Source: src, Settings: se, Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
	})
	if _, err := sfutest.Publish(ctx, c, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := max(len(layers), 1)
	if layers == nil {
		want = 2
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive && len(i.Layers) == want })
	return sig, pub, sock
}

// layerRate returns the bitrate the SFU measures for a layer of a share, 0 when it has none.
func layerRate(h *sfutest.Harness, share sfu.ShareID, rid string) int {
	info, _ := h.SFU.Share(share)
	for _, l := range info.Layers {
		if l.RID == rid {
			return l.Bitrate
		}
	}
	return 0
}

// congestionHint returns the first QualityHintEvent with the reason congestion, and whether there is one.
func congestionHint(sig *sfutest.DirectSignaler) (sfu.QualityHintEvent, bool) {
	for _, h := range hintsOf(sig) {
		if h.Reason == sfu.HintReasonCongestion {
			return h, true
		}
	}
	return sfu.QualityHintEvent{}, false
}

// wantCongestionHint fails unless h caps the share below capacity with the encodings of the Auto preset (no admin's
// cap) and both layers on.
func wantCongestionHint(t *testing.T, h sfu.QualityHintEvent, share sfu.ShareID, capacity int) {
	t.Helper()
	want := []sfu.EncodingParams{
		{RID: "f", Active: true, MaxBitrate: 8_000_000, MaxFramerate: 60, MaxPixels: 1920 * 1080},
		{RID: "q", Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 640 * 360},
	}
	if h.Share != share || h.MaxBitrate < 500_000 || h.MaxBitrate >= capacity || !slices.Equal(h.Encodings, want) {
		t.Errorf("congestion hint = %+v, want a cap between 500 kbit/s and %d bit/s with the encodings %+v", h,
			capacity, want)
	}
}

// waitTick waits until the SFU's ticker has just done a second (sfu.SFU.Ticks) and returns that moment.
func waitTick(t *testing.T, h *sfutest.Harness) time.Time {
	t.Helper()
	before := h.SFU.Ticks()
	deadline := time.Now().Add(5 * time.Second)
	for h.SFU.Ticks() == before {
		if time.Now().After(deadline) {
			t.Fatal("the SFU's ticker did no second in 5 s")
		}
		time.Sleep(time.Millisecond)
	}
	return time.Now()
}

// TestNativeUplinkPolicer: a Go publisher of the desktop kind sending about 4 Mbit/s gets a congestion hint within
// 6 s of a 3 Mbit/s policer appearing on its uplink (01 §14 C09, the SFU half of D9): the loss before repair backs
// the estimate off to 80 % of what got through.
func TestNativeUplinkPolicer(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{FaultyUDP: true})
	const share = sfu.ShareID("s_policed")
	sig, _, sock := nativePublisher(t, h, share, nativeSource(t, 3_700_000, 300_000))
	eventually(t, func() bool { return layerRate(h, share, "f") > 3_000_000 },
		func() string { return fmt.Sprintf("the full layer arrives at %d bit/s", layerRate(h, share, "f")) })
	if hints := hintsOf(sig); len(hints) != 0 {
		t.Fatalf("hints before the uplink was limited: %+v", hints)
	}
	before := layerRate(h, share, "f") + layerRate(h, share, "q")

	link := sfutest.NewLink(3_000_000)
	h.Wire.SetInboundLink(sock.LocalAddr(), link)
	limited := time.Now()
	var hint sfu.QualityHintEvent
	eventually(t, func() bool {
		var ok bool
		hint, ok = congestionHint(sig)
		return ok
	}, func() string { return fmt.Sprintf("no congestion hint; the hints are %+v", hintsOf(sig)) })
	took := time.Since(limited)
	_, _, lost := link.Counts()
	t.Logf("arriving at %d bit/s, behind a 3 Mbit/s policer: a congestion hint of %d bit/s after %v (%d datagrams "+
		"lost at the policer)", before, hint.MaxBitrate, took, lost)
	if took > 6*time.Second {
		t.Errorf("the congestion hint came %v after the uplink was limited, want within 6 s", took)
	}
	wantCongestionHint(t, hint, share, 3_000_000)
}

// queueWireMax is the most the queue test's publisher may send into the link, in bit/s of datagrams: about 4 Mbit/s
// into 3 grows the queue by a third of a second per second, so the 1 s buffer overflows about 3 s after the start.
const queueWireMax = 4_050_000

// TestNativeUplinkQueue: the same publisher sending about 4 Mbit/s on the wire behind a queueing link of 3 Mbit/s with
// a 1 s buffer, like a home router's (01 §14 C09, the SFU half of D19): the queueing delay backs the estimate off
// before the buffer overflows, so the congestion hint comes before any datagram is lost at the queue, whatever the
// ticker's phase. The estimator needs two consecutive ticks at 40 ms or more, on the minimum of their last 500 ms, so
// its worst case after a step is about 0.5 s + 40 ms ÷ the queue's growth per second + 2 s: 2.62 s here, against the
// overflow at about 3 s. The queue starts at two phases of the ticker, about 150 and about 420 ms after a tick.
func TestNativeUplinkQueue(t *testing.T) {
	for _, phase := range []time.Duration{150 * time.Millisecond, 420 * time.Millisecond} {
		t.Run(fmt.Sprintf("%v after a tick", phase), func(t *testing.T) { nativeQueueCase(t, phase) })
	}
}

// nativeQueueCase is TestNativeUplinkQueue with the queue starting phase after one of the SFU's ticks.
func nativeQueueCase(t *testing.T, phase time.Duration) {
	h := newHarness(t, sfutest.HarnessOptions{FaultyUDP: true})
	const share = sfu.ShareID("s_queued")
	sig, _, sock := nativePublisher(t, h, share, nativeSource(t, 3_500_000, 300_000))
	eventually(t, func() bool { return layerRate(h, share, "f") > 3_000_000 },
		func() string { return fmt.Sprintf("the full layer arrives at %d bit/s", layerRate(h, share, "f")) })
	before := layerRate(h, share, "f") + layerRate(h, share, "q")

	time.Sleep(time.Until(waitTick(t, h).Add(phase)))
	link := sfutest.NewQueueLink(3_000_000, time.Second)
	h.Wire.SetInboundLink(sock.LocalAddr(), link)
	limited := time.Now()
	var (
		hint    sfu.QualityHintEvent
		got     bool
		lost    int64
		queued  time.Duration
		deepest time.Duration
		wire    = -1 // bit/s into the link over its first second
	)
	lastDelay := time.Duration(-2)
	for deadline := limited.Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		queued = link.Queued()
		deepest = max(deepest, queued)
		if since := time.Since(limited); wire < 0 && since >= time.Second {
			sent, _, _ := link.Counts()
			wire = int(float64(sent) * 8 / since.Seconds())
			t.Logf("%v after the start: %d bit/s into the link", since.Round(time.Millisecond), wire)
		}
		if st, ok := h.SFU.UplinkStats(share); ok && st.Layers[0].QueueDelay != lastDelay {
			lastDelay = st.Layers[0].QueueDelay // a tick
			t.Logf("%v after the start: the full layer's QueueDelay %v at %d bit/s; %v queued now",
				time.Since(limited).Round(time.Millisecond), lastDelay, st.Layers[0].Bitrate,
				queued.Round(time.Millisecond))
		}
		if hint, got = congestionHint(sig); got {
			break
		}
		if _, _, lost = link.Counts(); lost > 0 {
			break
		}
	}
	_, _, lostAtHint := link.Counts()
	took := time.Since(limited)
	t.Logf("arriving at %d bit/s, behind a 3 Mbit/s queue of 1 s from %v after a tick: hint %v after %v with %v "+
		"queued (%d datagrams lost)", before, phase, got, took, queued, lostAtHint)
	if wire > queueWireMax {
		t.Errorf("%d bit/s went into the link in its first second, want at most %d", wire, queueWireMax)
	}
	switch {
	case !got && lost > 0:
		t.Fatalf("%d datagrams were lost at the queue (%v deep) before any congestion hint", lost, deepest)
	case !got:
		t.Fatalf("no congestion hint within 8 s; the queue reached %v", deepest)
	case lostAtHint > 0:
		t.Errorf("%d datagrams were lost at the queue by the time the congestion hint came", lostAtHint)
	}
	wantCongestionHint(t, hint, share, 3_000_000)
}

// TestPubSRTPReplayWindow: the publish PCs accept a plain retransmission far behind the newest packet (01 D7,
// §8.4; group 2's WA16-R1). A native publisher answers NACKs from a history of 4096 packets per SSRC on the media
// SSRC, so its retransmissions reach the SFU with their original sequence numbers; Pion's default SRTP replay
// window of 64 would drop one that comes after 64 newer packets. On the SFU's socket every copy of every 50th packet
// of the stream is lost and the last copy (a retransmission once the publisher has answered the SFU's NACK) kept;
// once the stream has gone on for over 4096 packets, the publisher stops and the test writes kept copies to the SFU
// from the publisher's socket, as late retransmissions. The ones about 100 and 4000 packets behind the newest reach
// the share's layer; one more than 4096 behind is a replay, which SRTP still drops.
func TestPubSRTPReplayWindow(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{FaultyUDP: true})
	const share = sfu.ShareID("s_replay")
	_, pub, sock := nativePublisher(t, h, share, nativeSource(t, 10_000_000, 0), "f")
	var ssrc uint32
	for _, tr := range pub.Tracks() {
		if tr.Layer == "f" {
			ssrc = tr.SSRC
		}
	}
	var (
		mu       sync.Mutex
		first    int // the extended number of the first packet seen; -1 before it
		high     int // the extended number of the newest packet seen
		kept     = map[int][]byte{}
		released = map[uint16]bool{}
	)
	first = -1
	h.Wire.SetDrop(func(dir sfutest.Direction, _ net.Addr, b []byte) bool {
		s, seq, _, ok := sfutest.RTPHeader(b)
		if dir != sfutest.Incoming || !ok || s != ssrc {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		ext := int(seq)
		if first < 0 {
			first, high = ext, ext
		} else {
			ext = high + int(int16(seq-uint16(high)))
			high = max(high, ext)
		}
		if ext <= first || seq%50 != 0 || released[seq] {
			return false
		}
		kept[ext] = append([]byte(nil), b...) // the newest copy: a retransmission once one came
		return true
	})
	newest := func() (from, to int) {
		mu.Lock()
		defer mu.Unlock()
		return first, high
	}
	eventually(t, func() bool { from, to := newest(); return from >= 0 && to-from > 4400 },
		func() string { from, to := newest(); return fmt.Sprintf("%d packets after the first", to-from) })
	if err := pub.SetActive("f", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // what is still on its way
	// top is the newest packet the SFU saw before the publisher stopped.
	_, top := newest()
	ingress := func() uint64 { return h.SFU.Ingress(share)["f"].Packets }
	stopped := ingress()
	time.Sleep(200 * time.Millisecond)
	if n := ingress(); n != stopped {
		t.Fatalf("the full layer still gets packets after the publisher stopped it (%d → %d)", stopped, n)
	}

	// pick returns a kept copy between lo and hi packets behind the newest, and its distance.
	pick := func(lo, hi int) ([]byte, int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for ext, b := range kept {
			if d := top - ext; d >= lo && d <= hi {
				_, seq, _, _ := sfutest.RTPHeader(b)
				released[seq] = true
				return b, d
			}
		}
		t.Fatalf("no kept packet %d to %d behind the newest", lo, hi)
		return nil, 0
	}
	inject := func(b []byte) {
		t.Helper()
		if _, err := sock.WriteTo(b, h.Wire.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		lo, hi int
		accept bool
	}{
		{4150, 4300, false}, // beyond the window: a replay
		{3900, 4050, true},
		{100, 200, true},
	} {
		b, behind := pick(tc.lo, tc.hi)
		before := ingress()
		inject(b)
		if tc.accept {
			eventually(t, func() bool { return ingress() == before+1 },
				func() string {
					return fmt.Sprintf("a retransmission %d packets behind the newest didn't reach the layer", behind)
				})
		} else {
			time.Sleep(500 * time.Millisecond)
			if n := ingress(); n != before {
				t.Errorf("a packet %d behind the newest reached the layer (%d → %d): no replay protection", behind,
					before, n)
			}
		}
		t.Logf("a retransmission %d packets behind the newest: accepted %v", behind, ingress() == before+1)
	}
}
