package publish

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestCaptureToSend: with a Clock Source, a video frame's CaptureToSend runs from its capture to the moment its last
// packet left: its age when the Source handed it out, plus its time in the pacer. A 300 KB keyframe from a full
// bucket at 24 Mbit/s adds about 62 ms. Without Pacing it is the age alone, and without a Clock it stays zero (§8.7).
func TestCaptureToSend(t *testing.T) {
	const age = 10 * time.Millisecond
	script := func() []scripted {
		var s []scripted
		for i := range 60 { // 1 s at 60 fps
			s = append(s, scripted{at: time.Duration(i) * time.Second / 60, age: age, pkt: videoFrame("f", 10_000, i == 0)})
		}
		return append(s, scripted{at: time.Second, age: age, pkt: videoFrame("f", 300_000, true)})
	}
	for _, tc := range []struct {
		name          string
		pacing, clock bool
		max           [2]time.Duration
	}{
		{"paced", true, true, [2]time.Duration{70 * time.Millisecond, 75 * time.Millisecond}},
		{"unpaced", false, true, [2]time.Duration{age, age}},
		{"no clock", true, false, [2]time.Duration{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var src Source = newScriptSource(script())
				if !tc.clock {
					src = plainSource{src}
				}
				b := newBench(src, []string{"f"}, false, tc.pacing)
				b.p.SetPacingTargets(map[string]int{"f": 8_000_000})
				b.run(t.Context())
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()
				b.stop()
				got := b.trackStats("f").CaptureToSend
				if !tc.clock {
					if got != (LatencyStats{}) {
						t.Errorf("CaptureToSend without a Clock = %+v, want zero", got)
					}
					return
				}
				if got.Samples != 61 || got.P50 != age || got.P95 != age {
					t.Errorf("CaptureToSend = %+v, want 61 samples, p50 and p95 %v", got, age)
				}
				if got.Max < tc.max[0] || got.Max > tc.max[1] {
					t.Errorf("CaptureToSend.Max = %v, want %v–%v", got.Max, tc.max[0], tc.max[1])
				}
				// Ten seconds on, the window has forgotten them.
				time.Sleep(10 * time.Second)
				if got := b.trackStats("f").CaptureToSend; got != (LatencyStats{}) {
					t.Errorf("CaptureToSend 10 s later = %+v, want an empty window", got)
				}
			})
		})
	}
}

// TestBitrateFPS: Bitrate is the payload and RTP header bits of the media packets over the last 2 s, FPS the frames
// over the last 2 s, for video and audio.
func TestBitrateFPS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var script []scripted
		for i := range 180 { // 3 s at 60 fps, and audio every 20 ms
			script = append(script, scripted{at: time.Duration(i) * time.Second / 60, pkt: videoFrame("f", 10_000, i == 0)})
		}
		for i := range 150 {
			script = append(script, scripted{at: time.Duration(i) * 20 * time.Millisecond, pkt: audioPacket()})
		}
		b := newBench(newScriptSource(script), []string{"f"}, true, true)
		b.run(t.Context())
		time.Sleep(2990 * time.Millisecond) // the last frame is due at 2983 ms, the last audio at 2980 ms
		synctest.Wait()
		f, a := b.trackStats("f"), b.trackStats("")
		b.stop()
		// A 10 000-byte access unit is 9 packets (8 of 1200 payload bytes and the FU-A headers in them), each with a
		// 20-byte header (the mid and rid extensions).
		var frameBits int
		pkts := b.writers["f"].all()
		for _, p := range pkts[len(pkts)-9:] {
			frameBits += 8 * (p.hdr.MarshalSize() + p.n)
		}
		if f.FPS != 60 || f.Bitrate != 60*frameBits {
			t.Errorf("video: %.1f fps at %d bit/s, want 60 fps at %d", f.FPS, f.Bitrate, 60*frameBits)
		}
		ap := b.writers[""].all()[0]
		audioBits := 8 * (ap.hdr.MarshalSize() + ap.n)
		if a.FPS != 50 || a.Bitrate != 50*audioBits {
			t.Errorf("audio: %.1f packets/s at %d bit/s, want 50 at %d", a.FPS, a.Bitrate, 50*audioBits)
		}
	})
}

// TestRTT: the RTT from a crafted receiver report, against the sender report it quotes (LSR) and its DLSR; when two
// sender reports carry the same NTP time (no frame between them), the one that gives a non-negative RTT; a report
// without an LSR, about another SSRC or quoting an unknown SR changes nothing.
func TestRTT(t *testing.T) {
	tr := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "f", "f", "6400")
	tr.stats.SSRC = 42
	t0 := time.Now()
	const ntp = uint64(0xe1234567_89abcdef)
	lsrOf := func(ntp uint64) uint32 { return uint32(ntp >> 16) } // the middle 32 bits
	lsr := lsrOf(ntp)
	dlsr := func(d time.Duration) uint32 { return uint32(d * 65536 / time.Second) }
	tr.wroteSR(&rtcp.SenderReport{NTPTime: ntp}, t0)
	rr := func(r rtcp.ReceptionReport, at time.Time) time.Duration {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.onReceptionReport(r, at)
		return tr.stats.RTT
	}
	if got := rr(rtcp.ReceptionReport{SSRC: 42, LastSenderReport: lsr, Delay: dlsr(500 * time.Millisecond)},
		t0.Add(540*time.Millisecond)); got < 39*time.Millisecond || got > 41*time.Millisecond {
		t.Errorf("RTT = %v, want 40 ms", got)
	}
	// The same NTP time a second later: a report quoting it gives the RTT against the later SR, one that arrived
	// before that SR could be quoted gives it against the first.
	tr.wroteSR(&rtcp.SenderReport{NTPTime: ntp}, t0.Add(time.Second))
	if got := rr(rtcp.ReceptionReport{SSRC: 42, LastSenderReport: lsr, Delay: dlsr(100 * time.Millisecond)},
		t0.Add(1130*time.Millisecond)); got < 29*time.Millisecond || got > 31*time.Millisecond {
		t.Errorf("RTT against the second SR = %v, want 30 ms", got)
	}
	if got := rr(rtcp.ReceptionReport{SSRC: 42, LastSenderReport: lsr, Delay: dlsr(950 * time.Millisecond)},
		t0.Add(1010*time.Millisecond)); got < 59*time.Millisecond || got > 61*time.Millisecond {
		t.Errorf("RTT against the first SR = %v, want 60 ms", got)
	}
	for _, r := range []rtcp.ReceptionReport{
		{SSRC: 42, LastSenderReport: 0, Delay: 0},
		{SSRC: 42, LastSenderReport: lsr + 1},
		{SSRC: 42, LastSenderReport: lsr, Delay: dlsr(5 * time.Second)}, // quotes an SR newer than any written
	} {
		if got := rr(r, t0.Add(1200*time.Millisecond)); got < 59*time.Millisecond || got > 61*time.Millisecond {
			t.Errorf("report %+v changed the RTT to %v", r, got)
		}
	}
	// Through onRTCP, as the RTCP reader hands them over: a receiver report, and the reports of a sender report.
	tr.wroteSR(&rtcp.SenderReport{NTPTime: ntp + 1<<32}, time.Now().Add(-80*time.Millisecond))
	tr.onRTCP(&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{
		{SSRC: 7, LastSenderReport: lsrOf(ntp + 1<<32)},
		{SSRC: 42, LastSenderReport: lsrOf(ntp + 1<<32), Delay: dlsr(50 * time.Millisecond)},
	}})
	if got := tr.stats.RTT; got < 29*time.Millisecond || got > 40*time.Millisecond {
		t.Errorf("RTT from a receiver report = %v, want about 30 ms", got)
	}
	tr.wroteSR(&rtcp.SenderReport{NTPTime: ntp + 2<<32}, time.Now().Add(-20*time.Millisecond))
	tr.onRTCP(&rtcp.SenderReport{Reports: []rtcp.ReceptionReport{
		{SSRC: 42, LastSenderReport: lsrOf(ntp + 2<<32), Delay: dlsr(10 * time.Millisecond)},
	}})
	if got := tr.stats.RTT; got < 9*time.Millisecond || got > 20*time.Millisecond {
		t.Errorf("RTT from a sender report's reception report = %v, want about 10 ms", got)
	}
	if tr.stats.SRs != 4 {
		t.Errorf("%d SRs counted, want 4", tr.stats.SRs)
	}
	// The track remembers its last srHistory SRs.
	for i := range 2 * srHistory {
		tr.wroteSR(&rtcp.SenderReport{NTPTime: uint64(i) << 32}, t0)
	}
	if len(tr.srs) != srHistory || tr.srs[0].lsr != lsrOf(uint64(srHistory)<<32) {
		t.Errorf("remembered SRs %+v", tr.srs)
	}
}

// TestDroppedKinds: packets of no track, PCM, events and video of a layer the Publisher doesn't send, are dropped and
// counted; nothing is written for them (§8.1).
func TestDroppedKinds(t *testing.T) {
	b := newBench(newScriptSource(nil), []string{"f"}, true, true)
	ev := &media.Event{Code: media.EventLayer, Layer: "f"}
	for _, pkt := range []media.Packet{
		{Kind: media.KindPCM, Data: make([]byte, 3840)},
		{Kind: media.KindEvent, Event: ev},
		{Kind: media.KindVideo, Layer: "q", Keyframe: true, Data: annexB(500, true, 0x64)},
		{Kind: 0, Data: []byte{1}},
	} {
		b.p.send(pkt)
	}
	if got := b.p.Stats().Dropped; got != 4 {
		t.Errorf("Dropped = %d, want 4", got)
	}
	if len(b.writers["f"].all()) != 0 || len(b.writers[""].all()) != 0 {
		t.Error("a dropped packet was written")
	}
	b.p.pacer.mu.Lock()
	queued := len(b.p.pacer.queue)
	b.p.pacer.mu.Unlock()
	if queued != 0 {
		t.Errorf("%d frames queued for the pacer", queued)
	}
}

func TestLatencyRank(t *testing.T) {
	w := newWindow(latencyWindow)
	now := time.Unix(100, 0)
	for i := 1; i <= 100; i++ {
		w.add(now, int64(time.Duration(i)*time.Millisecond))
	}
	if got := w.latency(now); got.P50 != 50*time.Millisecond || got.P95 != 95*time.Millisecond ||
		got.Max != 100*time.Millisecond || got.Samples != 100 {
		t.Errorf("latency of 1..100 ms = %+v", got)
	}
	w = newWindow(latencyWindow)
	w.add(now, int64(7*time.Millisecond))
	if got := w.latency(now); got.P50 != 7*time.Millisecond || got.P95 != 7*time.Millisecond || got.Samples != 1 {
		t.Errorf("latency of one sample = %+v", got)
	}
	// The window drops what is older than its span, and keeps at most maxSamples.
	for i := range maxSamples + 10 {
		w.add(now.Add(time.Duration(i)*time.Microsecond), 1)
	}
	if _, n := w.sum(now.Add(time.Second)); n != maxSamples {
		t.Errorf("%d samples kept, want %d", n, maxSamples)
	}
	if _, n := w.sum(now.Add(latencyWindow + time.Second)); n != 0 {
		t.Errorf("%d samples kept after the span", n)
	}
}
