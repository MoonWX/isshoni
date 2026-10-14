package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The pacer's tests (README S84; 02 §9.8, §17 pacer_test): its rate and depth, the three cases of 02 §17 on the
// bucket alone in simulated time (exact), the same three through a DownTrack's writer goroutine in real time (within
// the 10 % of 02 §17), and what goes through it: retransmissions do, audio doesn't.

// TestPacerRate: max(2.5 × the layer's MaxBitrate after the admin cap, 3 × its measured bitrate, 2 Mbit/s), and a
// bucket of 20 ms of that, at least 16 KiB.
func TestPacerRate(t *testing.T) {
	s, _ := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	q, _ := testLayer(t, sh, SlotQ, 0xa00)
	for _, tc := range []struct {
		what     string
		l        *Layer
		measured int64
		capKbps  int
		want     int64
	}{
		{"an idle 8 Mbit/s full layer: its maximum counts", f, 100_000, 0, 20_000_000},
		{"a full layer that sends 8 Mbit/s", f, 8_000_000, 0, 24_000_000},
		{"a full layer under an admin cap of 4 Mbit/s", f, 1_000_000, 4000, 10_000_000},
		{"the preview layer: the floor", q, 300_000, 0, 2_000_000},
		{"a preview layer that sends far more than it should", q, 1_000_000, 0, 3_000_000},
	} {
		s.SetLimits(Limits{MaxShareKbps: tc.capKbps})
		tc.l.stats.bitrate.Store(tc.measured)
		if got := pacerRate(tc.l); got != tc.want {
			t.Errorf("%s: rate %d, want %d", tc.what, got, tc.want)
		}
	}
	if got := pacerRate(nil); got != pacerMinRate {
		t.Errorf("rate without a layer = %d", got)
	}
	for rate, want := range map[int64]int64{20_000_000: 400_000, 24_000_000: 480_000, 2_000_000: 16 * 1024 * 8} {
		if got := pacerDepth(rate); got != want {
			t.Errorf("depth at %d bit/s = %d bits, want %d", rate, got, want)
		}
	}
}

// departures runs a writer over packets of sizes (bytes) that arrive at the given times, through p at rate, and
// returns when each one leaves: the writer takes a packet once it has arrived and the one before has left, and the
// pacer may hold it.
func departures(p *pacer, rate int64, arrivals []int64, sizes []int) []int64 {
	out := make([]int64, len(arrivals))
	now := int64(0)
	for i := range arrivals {
		now = max(now, arrivals[i])
		now += int64(p.delay(int64(sizes[i])*8, rate, now))
		out[i] = now
	}
	return out
}

// burst returns n packets of size bytes that all arrive at at.
func burst(n, size int, at int64) (arrivals []int64, sizes []int) {
	for range n {
		arrivals, sizes = append(arrivals, at), append(sizes, size)
	}
	return arrivals, sizes
}

// within reports whether got is want ± pct %.
func within(got, want time.Duration, pct int64) bool {
	d := got - want
	return d*100 <= want*time.Duration(pct) && -d*100 <= want*time.Duration(pct)
}

// TestPacerSchedule runs the three cases of 02 §17 on the bucket alone, in simulated time: a 150 KB burst at
// 24 Mbit/s takes 50 ms; a 1 fps idle stream followed by a 300-packet frame that arrives at 20 Mbit/s adds less than
// 20 ms to any of its packets; the same frame injected at once leaves at 20 Mbit/s, once the bucket's 20 ms of it
// have gone.
func TestPacerSchedule(t *testing.T) {
	const packet = 1200
	// (1) 150 KB at 24 Mbit/s (an 8 Mbit/s full layer that sends 8 Mbit/s): 50 ms when the bucket is empty to begin
	// with, as right after another burst; a full bucket lets its 60 KB go at once and the other 90 KB take 30 ms.
	p := pacer{started: true, at: t0}
	arr, sizes := burst(150_000/packet, packet, t0)
	dep := departures(&p, 24_000_000, arr, sizes)
	if took := time.Duration(dep[len(dep)-1] - t0); !within(took, 50*time.Millisecond, 1) {
		t.Errorf("150 KB at 24 Mbit/s from an empty bucket took %v, want 50 ms", took)
	}
	p = pacer{}
	dep = departures(&p, 24_000_000, arr, sizes)
	if took := time.Duration(dep[len(dep)-1] - t0); !within(took, 30*time.Millisecond, 1) {
		t.Errorf("150 KB at 24 Mbit/s from a full bucket took %v, want 30 ms", took)
	}
	if dep[0] != t0 || dep[49] != t0 || dep[50] == t0 {
		t.Errorf("from a full bucket packets 0, 49 and 50 left at %v, %v, %v; want 60 KB at once", time.Duration(dep[0]-t0),
			time.Duration(dep[49]-t0), time.Duration(dep[50]-t0))
	}

	// (2) A still screen at 1 fps for 10 s, then a 300-packet frame that arrives at 20 Mbit/s (an 8 Mbit/s full
	// layer measured at its idle rate is paced at 20 Mbit/s): nothing waits for long.
	idle := func() (*pacer, int64) {
		p := &pacer{}
		at := t0
		for range 10 {
			departures(p, 20_000_000, []int64{at, at}, []int{1000, 1000})
			at += oneSecond
		}
		return p, at
	}
	p2, start := idle()
	arr, sizes = nil, nil
	for i := range 300 {
		arr = append(arr, start+int64(i)*packet*8*int64(time.Second)/20_000_000)
		sizes = append(sizes, packet)
	}
	dep = departures(p2, 20_000_000, arr, sizes)
	var worst time.Duration
	for i := range dep {
		worst = max(worst, time.Duration(dep[i]-arr[i]))
	}
	if worst >= 20*time.Millisecond {
		t.Errorf("a frame arriving at 20 Mbit/s after an idle second waited up to %v, want less than 20 ms", worst)
	}

	// (3) The same frame at once: the bucket's 50 KB go at once, the rest at 20 Mbit/s.
	p3, start := idle()
	arr, sizes = burst(300, packet, start)
	dep = departures(p3, 20_000_000, arr, sizes)
	first := 0
	for dep[first] == start {
		first++
	}
	if first != 400_000/(packet*8) {
		t.Errorf("%d packets left at once, want the bucket's 400 kbit: 41", first)
	}
	rate := float64(len(dep)-1-first) * packet * 8 / time.Duration(dep[len(dep)-1]-dep[first]).Seconds()
	if rate < 19_800_000 || rate > 20_200_000 {
		t.Errorf("the frame left at %.0f bit/s, want 20 Mbit/s", rate)
	}

	// A lower rate than before lowers what the bucket holds: after a full layer at 24 Mbit/s, the preview layer's
	// 2 Mbit/s holds 16 KiB.
	p4 := pacer{}
	p4.delay(0, 24_000_000, t0)
	if wait := p4.delay(16*1024*8, 2_000_000, t0); wait != 0 {
		t.Errorf("16 KiB at 2 Mbit/s from a full bucket waits %v", wait)
	}
	if wait := p4.delay(1000*8, 2_000_000, t0); wait != 4*time.Millisecond {
		t.Errorf("1000 bytes more at 2 Mbit/s wait %v, want 4 ms", wait)
	}
}

// timedWriter is a DownTrack's webrtc.TrackLocalWriter that notes when each packet was written, and its size.
type timedWriter struct {
	mu    sync.Mutex
	at    []time.Time
	sizes []int
}

func (w *timedWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.at = append(w.at, time.Now())
	w.sizes = append(w.sizes, len(payload))
	return h.MarshalSize() + len(payload), nil
}

func (w *timedWriter) Write(b []byte) (int, error) { return len(b), nil }

// wait waits until n packets were written and returns their times.
func (w *timedWriter) wait(t *testing.T, n int) []time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.mu.Lock()
		got := len(w.at)
		at := append([]time.Time(nil), w.at...)
		w.mu.Unlock()
		if got >= n {
			return at
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d packets written, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// pacedRig is a full layer and a viewer's video DownTrack that forwards it, with its writer goroutine running and a
// timedWriter in place of the sub PC's sender.
type pacedRig struct {
	t   *testing.T
	f   *Layer
	d   *DownTrack
	w   *timedWriter
	seq uint16
	ts  uint32
}

func newPacedRig(t *testing.T) *pacedRig {
	t.Helper()
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	d, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	r := &pacedRig{t: t, f: f, d: d, w: &timedWriter{}, seq: 1000, ts: 90000}
	d.binding.Load().writer = r.w
	d.setWant(QualityHigh)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(nil)
	}()
	t.Cleanup(func() {
		d.stop()
		<-done
	})
	return r
}

// frame hands the layer one frame of n packets of size bytes (a keyframe start first when key), as its track's
// reader would, all at once.
func (r *pacedRig) frame(n, size int, key bool) {
	payload := make([]byte, size)
	payload[0] = 0x41
	for i := range n {
		p := payload
		if key && i == 0 {
			p = keyPayload(r.t)
		}
		r.f.handleRTP(rtpPkt(r.seq, r.ts, i == n-1, p), ProfileHigh, monoNow())
		r.seq++
	}
	r.ts += 3000
}

// TestPacerWriter runs the three cases of 02 §17 through a DownTrack's writer goroutine in real time, each within
// 10 %: a 150 KB burst at 24 Mbit/s takes 50 ms; a frame of 300 packets arriving at 20 Mbit/s after a still second
// adds less than 20 ms; the same frame injected at once leaves at 20 Mbit/s.
func TestPacerWriter(t *testing.T) {
	const packet = 1200
	r := newPacedRig(t)

	// (1) The layer sends 8 Mbit/s: 24 Mbit/s. A first burst of 60 KB empties the bucket; the 150 KB after it take
	// 50 ms.
	r.f.stats.bitrate.Store(8_000_000)
	r.frame(50, packet, true)
	r.frame(150_000/packet, packet, false)
	at := r.w.wait(t, 50+125)
	if took := at[174].Sub(at[49]); !within(took, 50*time.Millisecond, 10) {
		t.Errorf("150 KB at 24 Mbit/s took %v, want 50 ms ± 10 %%", took)
	}

	// (2) A still screen: an idle frame a second, measured at 100 kbit/s, so 20 Mbit/s from the full layer's
	// maximum. Then a frame of 300 packets arrives at 20 Mbit/s.
	r.f.stats.bitrate.Store(100_000)
	time.Sleep(time.Second)
	r.frame(1, 1000, false)
	time.Sleep(time.Second)
	mark := 50 + 125 + 1
	gap := time.Duration(packet * 8 * int64(time.Second) / 20_000_000)
	start := time.Now()
	handed := make([]time.Time, 300)
	for i := range 300 {
		if d := time.Until(start.Add(time.Duration(i) * gap)); d > 0 {
			time.Sleep(d)
		}
		handed[i] = time.Now()
		r.frame(1, packet, false)
	}
	at = r.w.wait(t, mark+300)
	var worst time.Duration
	for i := range 300 {
		worst = max(worst, at[mark+i].Sub(handed[i]))
	}
	t.Logf("a frame arriving at 20 Mbit/s in %v waited up to %v", handed[299].Sub(handed[0]), worst)
	if worst >= 20*time.Millisecond {
		t.Errorf("a frame arriving at 20 Mbit/s after a still second waited up to %v, want less than 20 ms", worst)
	}

	// (3) The same frame at once, after a still second: it leaves at 20 Mbit/s once the bucket's 50 KB have gone.
	time.Sleep(time.Second)
	mark += 300
	r.frame(300, packet, false)
	at = r.w.wait(t, mark+300)
	from, to := at[mark+150], at[mark+299]
	rate := float64(149*packet*8) / to.Sub(from).Seconds()
	t.Logf("the frame injected at once: the first %v at once, then %.0f bit/s", at[mark+40].Sub(at[mark]), rate)
	if rate < 18_000_000 || rate > 22_000_000 {
		t.Errorf("a frame injected at once left at %.0f bit/s, want 20 Mbit/s ± 10 %%", rate)
	}
}

// TestPacerRTX: a video retransmission goes through the pacer like media, in the writer's order, which serves
// retransmissions first (02 §9.8, the decision of README S84): a large frame asked for again leaves spread like the
// frame itself. Audio, retransmissions included, is never paced.
func TestPacerRTX(t *testing.T) {
	r := newRTXRig(t)
	r.pace = 100 * tick / 1000 // the frame's packets arrive 1 ms apart
	seq0, _ := r.start(0)
	big := make([]byte, 1100)
	big[0] = 0x41
	for range 100 {
		r.send(big, false)
	}
	time.Sleep(100 * time.Millisecond) // the bucket is full again
	seqs := make([]uint16, 100)
	for i := range seqs {
		seqs[i] = seq0 + 1 + uint16(i)
	}
	start := time.Now()
	if n := r.nack(seqs...); n != 100 {
		t.Fatalf("%d retransmissions for 100 NACKed packets", n)
	}
	// 100 × 1102 bytes at 20 Mbit/s, of which the bucket's 400 kbit go at once: 24 ms at least.
	took := time.Since(start)
	if took < 22*time.Millisecond {
		t.Errorf("100 retransmissions of 1100 bytes took %v, want them paced: at least 24 ms", took)
	}
	t.Logf("100 retransmissions of 1100 bytes took %v", took)

	// Audio: never paced, and the bucket isn't even started.
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_2")
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	d, w := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeAudio)
	d.setWant(QualityHigh)
	payload := make([]byte, 1200)
	var state rtpWriteState
	audioStart := time.Now()
	for i := range 200 {
		a.handleRTP(rtpPkt(uint16(i), uint32(i)*960, false, payload), "", monoNow())
		drain(d, &state)
	}
	if len(w.hdrs) != 200 || d.pacer.started || time.Since(audioStart) > 200*time.Millisecond {
		t.Errorf("audio: %d packets written in %v, pacer started %v; want 200 at once, unpaced", len(w.hdrs),
			time.Since(audioStart), d.pacer.started)
	}
}
