package sfutest

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// listenUDP opens a UDP socket on a free loopback port.
func listenUDP(t *testing.T) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
}

func TestRTPHeader(t *testing.T) {
	rtpPkt := []byte{0x80, 0xe0, 0x12, 0x34, 0, 0, 0, 1, 0xde, 0xad, 0xbe, 0xef}
	if ssrc, seq, pt, ok := RTPHeader(rtpPkt); !ok || ssrc != 0xdeadbeef || seq != 0x1234 || pt != 96 {
		t.Errorf("RTP: %x %d %d %v", ssrc, seq, pt, ok)
	}
	for name, b := range map[string][]byte{
		"RTCP SR": {0x80, 200, 0, 6, 0, 0, 0, 1, 0, 0, 0, 0},
		"PLI":     {0x81, 206, 0, 2, 0, 0, 0, 1, 0, 0, 0, 2},
		"STUN":    {0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42, 0, 0, 0, 0},
		"DTLS":    {22, 0xfe, 0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"short":   {0x80, 96, 0, 1},
	} {
		if _, _, _, ok := RTPHeader(b); ok {
			t.Errorf("%s parsed as RTP", name)
		}
	}
}

// TestFaultConn drops and black-holes datagrams between two sockets.
func TestFaultConn(t *testing.T) {
	a, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaultConn(a)
	defer fc.Close()
	b, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	recv := func(pc net.PacketConn) string {
		t.Helper()
		_ = pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 64)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return ""
		}
		return string(buf[:n])
	}
	send := func(msg string) {
		if _, err := fc.WriteTo([]byte(msg), b.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	fc.SetDrop(func(dir Direction, _ net.Addr, p []byte) bool { return dir == Outgoing && string(p) == "drop" })
	send("drop")
	send("keep")
	if got := recv(b); got != "keep" {
		t.Errorf("got %q, want keep", got)
	}
	fc.SetDrop(nil)
	fc.BlackHole(b.LocalAddr(), true)
	send("lost")
	if _, err := b.WriteTo([]byte("in"), fc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if got := recv(b) + recv(fc); got != "" {
		t.Errorf("black hole let %q through", got)
	}
	fc.BlackHole(b.LocalAddr(), false)
	send("back")
	if got := recv(b); got != "back" {
		t.Errorf("got %q after the black hole", got)
	}
	if n := fc.Dropped(); n != 3 {
		t.Errorf("dropped %d, want 3", n)
	}
}

// TestLoopbackNACK loses chosen RTP packets on the publisher's socket once each: the viewer's NACKs get them resent
// by the publisher's NACK responder, so every gap is recovered and nothing is finally lost. Then 500 ms of black hole:
// media resumes.
func TestLoopbackNACK(t *testing.T) {
	conn, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaultConn(conn)
	mux := webrtc.NewICEUDPMux(nil, fc)
	t.Cleanup(func() { _ = mux.Close() }) // after the publisher's cleanup, which runs first
	var (
		mu    sync.Mutex
		lost  = map[[2]uint32]bool{}
		viewr atomic.Value
	)
	fc.SetDrop(func(dir Direction, peer net.Addr, b []byte) bool {
		ssrc, seq, _, ok := RTPHeader(b)
		if dir != Outgoing || !ok {
			return false
		}
		viewr.Store(peer)
		key := [2]uint32{ssrc, uint32(seq)}
		mu.Lock()
		defer mu.Unlock()
		if seq%40 != 7 || lost[key] {
			return false
		}
		lost[key] = true
		return true
	})
	se := LoopbackSettings()
	se.SetICEUDPMux(mux)
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 1_000_000}},
		GOP:    10 * time.Second, Audio: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopbackWith(t, src, publish.Options{Layers: []string{"f"}, Audio: true}, se)
	video := track(t, v, webrtc.RTPCodecTypeVideo, "")
	audio := track(t, v, webrtc.RTPCodecTypeAudio, "")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, r := range []*Recorder{video, audio} {
		if err := r.Wait(ctx, func(r *Recorder) bool { return r.Stats().Packets > 200 }); err != nil {
			t.Fatal(err)
		}
	}
	fc.SetDrop(nil)
	time.Sleep(FinalLossAfter + 200*time.Millisecond)
	for _, r := range []*Recorder{video, audio} {
		s := r.Stats()
		t.Logf("%s: %d packets, %d gaps, %d recovered, %d lost", r.Kind(), s.Packets, s.Gaps, s.Recovered, s.Lost)
		if s.Gaps == 0 || s.Recovered != s.Gaps || s.Lost != 0 || s.Duplicates != 0 {
			t.Errorf("%s: %+v", r.Kind(), s)
		}
		if err := CheckContinuous(r.Packets()); err != nil {
			t.Errorf("%s: %v", r.Kind(), err)
		}
	}
	nacks := 0
	for _, ts := range pub.Stats().Tracks {
		nacks += ts.NACKs
	}
	if nacks == 0 {
		t.Error("the publisher counted no NACKs")
	}

	// Black hole: nothing arrives, then media resumes.
	peer, _ := viewr.Load().(net.Addr)
	if peer == nil {
		t.Fatal("no viewer address seen")
	}
	fc.BlackHole(peer, true)
	time.Sleep(100 * time.Millisecond) // packets in flight
	before := video.Stats().Packets
	time.Sleep(400 * time.Millisecond)
	if n := video.Stats().Packets; n != before {
		t.Errorf("%d packets arrived through the black hole", n-before)
	}
	fc.BlackHole(peer, false)
	if err := video.Wait(ctx, func(r *Recorder) bool { return r.Stats().Packets > before+30 }); err != nil {
		t.Fatalf("media didn't resume: %v", err)
	}
}
