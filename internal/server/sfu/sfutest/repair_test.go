package sfutest

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// The Publisher's two test options (README S63), against a Viewer without an SFU between them: what the SFU's
// integration tests 6 and 19 (02 §17) send it is what these tests see arrive.

// TestLoopbackRedundantRTX: a Publisher with RedundantRTX repairs like a browser. Its second copies arrive on the
// RTX stream of each simulcast layer, which the receiver finds by the repaired-rtp-stream-id extension; packets lost
// on the wire come back on the same stream when the Viewer asks, audio as the packet again; and the Viewer, which
// knows what came back through RTX, asks for nothing twice.
func TestLoopbackRedundantRTX(t *testing.T) {
	conn, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaultConn(conn)
	mux := webrtc.NewICEUDPMux(nil, fc)
	t.Cleanup(func() { _ = mux.Close() }) // after the publisher's cleanup, which runs first
	// The publisher's wire loses video packets that no second copy covers (those without the marker bit; its video
	// payload type is 96, RTX 97) and audio packets (111), each once: but only once every track has begun, since a
	// receiver doesn't miss what came before the first packet it got.
	var (
		mu   sync.Mutex
		lost = map[[2]uint32]bool{}
	)
	drop := func(dir Direction, _ net.Addr, b []byte) bool {
		ssrc, seq, pt, ok := RTPHeader(b)
		if dir != Outgoing || !ok || seq%16 != 5 || (pt != 111 && (pt != 96 || b[1]&0x80 != 0)) {
			return false
		}
		key := [2]uint32{ssrc, uint32(seq)}
		mu.Lock()
		defer mu.Unlock()
		if lost[key] {
			return false
		}
		lost[key] = true
		return true
	}
	se := LoopbackSettings()
	se.SetICEUDPMux(mux)
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 1_000_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 300_000},
		},
		GOP: 10 * time.Second, Audio: true, Seed: 63,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopbackWith(t, src, publish.Options{Audio: true, RedundantRTX: true}, se)
	recs := map[string]*Recorder{
		"f": track(t, v, webrtc.RTPCodecTypeVideo, "f"),
		"q": track(t, v, webrtc.RTPCodecTypeVideo, "q"),
		"":  track(t, v, webrtc.RTPCodecTypeAudio, ""),
	}
	fc.SetDrop(drop)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for rid, r := range recs {
		err := r.Wait(ctx, func(r *Recorder) bool {
			if rid == "" {
				return r.Stats().Packets >= 300 // six seconds of audio
			}
			return len(Frames(r.Packets())) >= 90 // three seconds of f, six of q
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	fc.SetDrop(nil)
	time.Sleep(FinalLossAfter + 200*time.Millisecond)
	dropped := map[uint32]int{} // by SSRC
	mu.Lock()
	for key := range lost {
		dropped[key[0]]++
	}
	mu.Unlock()
	for _, ts := range pub.Stats().Tracks {
		r := recs[ts.RID]
		st := r.Stats()
		wire := dropped[ts.SSRC]
		t.Logf("%s %q: %d lost on the wire; publisher %d redundant, %d retransmitted, %d NACKs; viewer %d gaps, %d recovered "+
			"(%d by RTX), %d lost, %d RTX packets, %d duplicates, %d NACKs sent", ts.Kind, ts.RID, wire, ts.Redundant,
			ts.Retransmitted, ts.NACKs, st.Gaps, st.Recovered, st.RecoveredRTX, st.Lost, st.RTX, st.Duplicates, st.NACKs)
		// A gap is a packet lost on the wire or one that Pion handed over late: it reads a track's RTX stream ahead
		// of the track, so a second copy can overtake packets that left before it. Every gap closes.
		if wire < 5 || st.Gaps < wire || st.Recovered != st.Gaps || st.Lost != 0 {
			t.Errorf("%q: %d packets lost on the wire, %d gaps, %d recovered, %d lost for good; want loss, and all of it "+
				"repaired", ts.RID, wire, st.Gaps, st.Recovered, st.Lost)
		}
		// The Viewer asked for what the wire lost, hardly ever twice (the repair is back before it would ask again),
		// and the Publisher resent whatever it was asked for.
		if st.NACKs < wire || st.NACKs > wire+wire/2+2 || int(ts.Retransmitted) != st.NACKs {
			t.Errorf("%q: the viewer sent NACKs for %d sequence numbers and the publisher resent %d, for %d lost packets",
				ts.RID, st.NACKs, ts.Retransmitted, wire)
		}
		// One copy of every sequence number, in sequence order: the stream as a decoder gets it. And never two copies
		// on the media stream: of video, the other one is on the RTX stream, whichever arrived first.
		var whole []Packet
		media := map[uint16]int{}
		for _, p := range InSeqOrder(r.Packets()) {
			if !p.Dup {
				whole = append(whole, p)
			}
			if !p.RTX {
				media[p.Seq]++
			}
		}
		if err := CheckContinuous(whole); err != nil {
			t.Errorf("%q: %v", ts.RID, err)
		}
		if ts.Kind == webrtc.RTPCodecTypeAudio {
			// Audio has no RTX stream: a repair is the packet again, and there are no second copies.
			if st.RTX != 0 || ts.Redundant != 0 || st.Duplicates != st.NACKs-wire {
				t.Errorf("audio: %d RTX packets, %d second copies, %d duplicates; want plain retransmissions only", st.RTX,
					ts.Redundant, st.Duplicates)
			}
			continue
		}
		for seq, n := range media {
			if n > 1 {
				t.Fatalf("%q: seq %d came %d times on the media stream", ts.RID, seq, n)
			}
		}
		if st.RecoveredRTX < wire || ts.Redundant < 60 {
			t.Errorf("%q: %d gaps closed by RTX for %d lost packets, %d second copies sent", ts.RID, st.RecoveredRTX, wire,
				ts.Redundant)
		}
		// The second copies arrive as duplicates, all of them: the two counts are taken a moment apart while the
		// media flows, so they may differ by the copies of that moment.
		if got, sent := uint64(st.Duplicates), ts.Redundant; got+5 < sent || got > sent+uint64(st.NACKs-wire)+5 {
			t.Errorf("%q: the viewer got %d duplicates for %d second copies", ts.RID, got, sent)
		}
		if err := CheckVideoMarkers(WithoutPartialTail(whole)); err != nil {
			t.Errorf("%q: %v", ts.RID, err)
		}
	}
}

// TestLoopbackPadding: a Publisher with Padding sends padding-only packets on its video's media stream. They arrive
// as packets without payload, in sequence, so the receiver sees no gap; the SFU must not pass them on (02 §9.4).
func TestLoopbackPadding(t *testing.T) {
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 600_000}}, GOP: time.Second, Audio: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopback(t, src, publish.Options{Layers: []string{"f"}, Audio: true, Padding: 3})
	r := track(t, v, webrtc.RTPCodecTypeVideo, "")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := r.Wait(ctx, func(r *Recorder) bool { return len(Frames(r.Packets())) >= 60 }); err != nil {
		t.Fatal(err)
	}
	pkts := r.Packets()
	sent := pub.Stats().Tracks[0]
	empty := 0
	for i, p := range pkts {
		if p.Size != 0 {
			continue
		}
		empty++
		// Padding follows a frame's last packet, with the frame's timestamp, and has no marker bit.
		if i == 0 || p.Marker || p.TS != pkts[i-1].TS || p.Seq != pkts[i-1].Seq+1 {
			t.Fatalf("padding packet %d = %+v after %+v", i, p, pkts[max(i-1, 0)])
		}
	}
	// Two after every third frame.
	frames := len(Frames(pkts))
	if want := 2 * (frames / 3); empty < want-2 || empty > want+2 || sent.Padding < uint64(empty) {
		t.Errorf("the viewer got %d padding packets in %d frames, want about %d; the publisher sent %d", empty, frames, want,
			sent.Padding)
	}
	if err := CheckContinuous(pkts); err != nil {
		t.Error(err)
	}
	if st := r.Stats(); st.Gaps != 0 || st.NACKs != 0 || st.Duplicates != 0 {
		t.Errorf("stats %+v, want no gap and no NACK", st)
	}
}
