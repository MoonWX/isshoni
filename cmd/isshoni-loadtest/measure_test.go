package main

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Unit tests of the command line's layer syntax and of the analysis: percentiles, CPU, the PLI windows, the rates,
// loss totals, layer switches and the A/V pairs' NTP mapping.

func TestParseLayer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want fake.VideoLayer
		err  string
	}{
		{in: "1920x1080@60:8M", want: fake.VideoLayer{RID: "f", Width: 1920, Height: 1080, FPS: 60, Bitrate: 8_000_000}},
		{in: "640x360@15:300k", want: fake.VideoLayer{RID: "f", Width: 640, Height: 360, FPS: 15, Bitrate: 300_000}},
		{in: "1280x720@30:2.5M", want: fake.VideoLayer{RID: "f", Width: 1280, Height: 720, FPS: 30, Bitrate: 2_500_000}},
		{in: "320x180@15:250000", want: fake.VideoLayer{RID: "f", Width: 320, Height: 180, FPS: 15, Bitrate: 250_000}},
		{in: "320x180@15:64K", want: fake.VideoLayer{RID: "f", Width: 320, Height: 180, FPS: 15, Bitrate: 64_000}},
		{in: "640x360@30", want: fake.VideoLayer{RID: "f", Width: 640, Height: 360, FPS: 30}},
		{in: "640x360", err: "want WxH@FPS:BITRATE"},
		{in: "640@30", err: "want WxH@FPS:BITRATE"},
		{in: "641x360@30", err: "width"},
		{in: "640x0@30", err: "height"},
		{in: "640x360@0", err: "frame rate"},
		{in: "640x360@241", err: "frame rate"},
		{in: "640x360@30:fast", err: "bitrate"},
		{in: "640x360@30:999", err: "bitrate"},
		{in: "640x360@30:2G", err: "bitrate"},
	} {
		got, err := parseLayer("f", tc.in)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("parseLayer(%q) = %+v, %v; want an error about %s", tc.in, got, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("parseLayer(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

// FuzzParseLayer: whatever parseLayer accepts is a valid fake layer, and formatLayer writes it so that parseLayer
// reads it back the same.
func FuzzParseLayer(f *testing.F) {
	for _, s := range []string{"1920x1080@60:8M", "640x360@15:300k", "1280x720@30:2.5M", "2x2@1:1k", "640x360@30", "x@:", "1e3x2@3:4M"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l, err := parseLayer("f", s)
		if err != nil {
			return
		}
		if l.Width <= 0 || l.Width%2 != 0 || l.Height <= 0 || l.Height%2 != 0 || l.FPS < 1 || l.FPS > 240 || l.Bitrate < 0 {
			t.Fatalf("parseLayer(%q) = %+v: not a valid layer", s, l)
		}
		if l.Bitrate == 0 {
			return
		}
		back, err := parseLayer("f", formatLayer(l))
		if err != nil || back != l {
			t.Fatalf("parseLayer(formatLayer(%+v)) = %+v, %v (formatted %q)", l, back, err, formatLayer(l))
		}
	})
}

func TestParseLayersDecodable(t *testing.T) {
	t.Parallel()
	layers, err := parseLayers(defaultFull, defaultPreview, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if def := fake.DefaultDecodableLayers(); layers[0] != def[0] || layers[1] != def[1] {
		t.Errorf("decodable defaults = %+v, want fake's %+v", layers, def)
	}
	layers, err = parseLayers("1280x720@30:9M", defaultPreview, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if l := layers[0]; l.Width != 1280 || l.Height != 720 || l.FPS != 30 || l.Bitrate == 9_000_000 {
		t.Errorf("decodable -full 1280x720@30:9M = %+v: the size and rate count, the bitrate doesn't", l)
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()
	xs := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}
	for _, tc := range []struct{ p, want float64 }{{50, 5}, {95, 10}, {99, 10}, {10, 1}, {0, 1}, {100, 10}} {
		if got := percentile(xs, tc.p); got != tc.want {
			t.Errorf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("percentile of nothing is not 0")
	}
}

func TestCPUStats(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	// 4 cores: 2 CPU-seconds in 5 s is 10 %, then 6 in 5 s is 30 %, then 4 is 20 %.
	samples := []dashSample{
		{generated: at(0), cpuSeconds: 100, numCPU: 4},
		{generated: at(5), cpuSeconds: 102, numCPU: 4},
		{generated: at(10), cpuSeconds: 108, numCPU: 4},
		{generated: at(15), cpuSeconds: 112, numCPU: 4},
	}
	avg, p95, peak, ok := cpuStats(samples)
	if !ok || math.Abs(avg-20) > 1e-9 || p95 != 30 || peak != 30 {
		t.Errorf("cpuStats = %v, %v, %v, %v; want 20, 30, 30, true", avg, p95, peak, ok)
	}
	if _, _, _, ok := cpuStats(samples[:1]); ok {
		t.Error("cpuStats of one sample is ok")
	}
}

func TestRatesAndExpectedEgress(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	f, q, a := trackKey{"s1", "f"}, trackKey{"s1", "q"}, trackKey{"s1", ""}
	samples := []pubSample{
		{at: t0, tracks: map[trackKey]trackCounts{f: {octets: 0}, q: {octets: 0}, a: {octets: 0}}},
		{at: t0.Add(2 * time.Second), tracks: map[trackKey]trackCounts{f: {octets: 1_000_000}, q: {octets: 50_000}, a: {octets: 4_000}}},
	}
	r := rates(samples)
	if r[f] != 4e6 || r[q] != 2e5 || r[a] != 16e3 {
		t.Fatalf("rates = %v", r)
	}
	cfg, err := parseFlags([]string{"-server", "http://127.0.0.1:1", "-admin", "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lt := &loadTest{cfg: cfg}
	s := dashSample{egressBps: 9e6, counts: map[string]wantCount{"s1": {high: 2, low: 1, audio: 2}}}
	if got := lt.expectedEgress(s, r); got != 2*4e6+2e5+2*16e3 {
		t.Errorf("expectedEgress = %v", got)
	}
	got, want := lt.egress([]dashSample{s, s}, r)
	if got != 9e6 || want != 2*4e6+2e5+2*16e3 {
		t.Errorf("egress = %v, %v", got, want)
	}
}

func TestPLIStats(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	f, q, a := trackKey{"s1", "f"}, trackKey{"s1", "q"}, trackKey{"s1", ""}
	// One sample a second for 30 s. Layer f gets a request every 5 s, and a burst of 8 at 12 s; q gets one at 3 s.
	var samples []pubSample
	fCount, qCount := 0, 0
	for s := 0; s <= 30; s++ {
		if s > 0 && s%5 == 0 {
			fCount++
		}
		if s == 12 {
			fCount += 8
		}
		if s == 3 {
			qCount++
		}
		samples = append(samples, pubSample{at: t0.Add(time.Duration(s) * time.Second), tracks: map[trackKey]trackCounts{
			f: {plis: fCount}, q: {plis: qCount}, a: {plis: 0},
		}})
	}
	got := pliStats(samples, 10*time.Second)
	if len(got) != 2 || got[0].key != f || got[1].key != q {
		t.Fatalf("pliStats = %+v, want f and q (no audio)", got)
	}
	// f: 6 + 8 = 14 in 30 s; the worst 10 s window, (5 s, 15 s] or so, has the burst and two more.
	if got[0].count != 14 || math.Abs(got[0].perSecond-14.0/30) > 1e-9 || got[0].maxInWin != 10 {
		t.Errorf("f = %+v, want 14 requests, 14/30 per second, 10 in the worst 10 s", got[0])
	}
	if got[1].count != 1 || got[1].maxInWin != 1 {
		t.Errorf("q = %+v, want 1 request", got[1])
	}
}

func TestLossSince(t *testing.T) {
	t.Parallel()
	r1, r2 := &sfutest.Recorder{}, &sfutest.Recorder{}
	marks := map[*sfutest.Recorder]sfutest.RecorderStats{r1: {Packets: 100, Expected: 100, Gaps: 2, Lost: 1, Bytes: 1000}}
	now := map[*sfutest.Recorder]sfutest.RecorderStats{
		r1: {Packets: 1100, Expected: 1100, Gaps: 12, Lost: 3, Duplicates: 1, Bytes: 5000},
		r2: {Packets: 1000, Expected: 1000, Gaps: 0, Lost: 0, Bytes: 3000}, // came after the mark
	}
	got := lossSince(now, marks)
	if got != (lossTotals{expected: 2000, gaps: 10, lost: 2, dups: 1, tracks: 2, bytes: 7000}) {
		t.Errorf("lossSince = %+v", got)
	}
	if math.Abs(got.pct(got.gaps)-0.5) > 1e-9 || math.Abs(got.pct(got.lost)-0.1) > 1e-9 {
		t.Errorf("pct = %v, %v", got.pct(got.gaps), got.pct(got.lost))
	}
}

// track builds packets of fake video in arrival order: frames of a layer, each a keyframe (an SPS packet in front of
// its slice) or a delta frame, a packet per slice, 10 ms apart.
type trackBuilder struct {
	t0   time.Time
	seq  uint16
	ts   uint32
	n    int
	pkts []sfutest.Packet
}

func (b *trackBuilder) frame(rid string, key bool) {
	at := b.t0.Add(time.Duration(b.n) * 10 * time.Millisecond)
	b.n++
	b.ts += 3000
	if key {
		b.pkts = append(b.pkts, sfutest.Packet{Seq: b.seq, TS: b.ts, Arrival: at, KeyStart: true})
		b.seq++
	}
	b.pkts = append(b.pkts, sfutest.Packet{
		Seq: b.seq, TS: b.ts, Arrival: at, Marker: true, HasMark: true, Mark: fake.Marker{RID: rid, Keyframe: key},
	})
	b.seq++
}

// switchTrack builds a track of 7 q frames and then 2 f frames, the first a keyframe at 70 ms, with the sequence
// numbers wrapping on the way; between the last q frame and the f keyframe, between runs first.
func switchTrack(t0 time.Time, between func(b *trackBuilder)) *trackBuilder {
	b := &trackBuilder{t0: t0, seq: 65530}
	b.frame("q", true)
	for range 6 {
		b.frame("q", false)
	}
	if between != nil {
		between(b)
	}
	b.frame("f", true)
	b.frame("f", false)
	return b
}

func TestFindSwitch(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	b := switchTrack(t0, nil)
	sentAt := t0.Add(35 * time.Millisecond) // after frame 3

	s, state := findSwitch(b.pkts, "f", sentAt)
	if state != switchFound || s.latency != 35*time.Millisecond || !s.keyframe || s.lostStart || s.seqGap || s.tsBack {
		t.Errorf("findSwitch = %+v, %v; want found after 35 ms, on a keyframe, continuous", s, state)
	}
	if want := b.pkts[len(b.pkts)-3].Seq; s.start != want || s.seq != want+1 {
		t.Errorf("findSwitch = %+v: the frame starts at seq %d (its SPS), its slice at %d", s, want, want+1)
	}
	if _, state := findSwitch(b.pkts[:len(b.pkts)-3], "f", sentAt); state != switchPending {
		t.Errorf("before the first f packet: %v, want pending", state)
	}
	// A track that was on f before the update has nothing to switch.
	if _, state := findSwitch(b.pkts, "f", t0.Add(80*time.Millisecond)); state != switchNotNeeded {
		t.Errorf("a track on f already: %v, want not needed", state)
	}
	// The SPS packet in front of the keyframe lost: the start can't be checked.
	var lost []sfutest.Packet
	keyTS := b.pkts[len(b.pkts)-2].TS // the f keyframe's slice; the last packet is the delta frame after it
	for _, p := range b.pkts {
		if !p.KeyStart || p.TS != keyTS {
			lost = append(lost, p)
		}
	}
	if s, state := findSwitch(lost, "f", sentAt); state != switchFound || s.keyframe || !s.lostStart || s.seqGap {
		t.Errorf("without its SPS packet: %+v, %v; want found, lost start", s, state)
	}
	// A switch onto a delta frame.
	d := &trackBuilder{t0: t0}
	d.frame("q", true)
	d.frame("q", false)
	d.frame("f", false)
	if s, state := findSwitch(d.pkts, "f", t0); state != switchFound || s.keyframe || s.lostStart || s.seqGap {
		t.Errorf("onto a delta frame: %+v, %v; want found, not a keyframe", s, state)
	}
}

// TestCheckSwitch: the second look at a switch, once repairs have had their chance, finds what the first one could
// not tell: a gap in the sequence numbers at the layer change, an SPS packet that a repair brought late, and a
// timestamp that does not move forward.
func TestCheckSwitch(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	sentAt := t0.Add(35 * time.Millisecond)
	find := func(t *testing.T, pkts []sfutest.Packet) switchSample {
		t.Helper()
		s, state := findSwitch(pkts, "f", sentAt)
		if state != switchFound {
			t.Fatalf("findSwitch: %v, want found", state)
		}
		return s
	}

	t.Run("seq gap at the layer change", func(t *testing.T) {
		t.Parallel()
		b := switchTrack(t0, func(b *trackBuilder) { b.seq++ }) // the munger skipped a number
		s := find(t, b.pkts)
		if !checkSwitch(b.pkts, &s) || !s.keyframe || s.lostStart || !s.seqGap || s.tsBack {
			t.Errorf("checkSwitch = %+v; want a keyframe with a seq gap in front", s)
		}
	})

	t.Run("SPS repaired after the slice", func(t *testing.T) {
		t.Parallel()
		b := switchTrack(t0, nil)
		// The f keyframe's SPS packet is lost on the way and comes again through RTX after the delta frame.
		i := len(b.pkts) - 3
		sps := b.pkts[i]
		sps.RTX, sps.Arrival = true, b.pkts[len(b.pkts)-1].Arrival.Add(30*time.Millisecond)
		pkts := append(slices.Delete(slices.Clone(b.pkts), i, i+1), sps)
		early := find(t, pkts[:len(pkts)-1])
		if early.keyframe || !early.lostStart {
			t.Errorf("first look, before the repair: %+v; want a lost start", early)
		}
		s := early
		if !checkSwitch(pkts, &s) || !s.keyframe || s.lostStart || s.seqGap || s.tsBack {
			t.Errorf("checkSwitch after the repair = %+v; want a keyframe on its SPS, continuous", s)
		}
	})

	for _, tc := range []struct {
		name string
		back uint32
	}{{"timestamp of the frame before", 3000}, {"timestamp before the frame before", 6000}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := switchTrack(t0, func(b *trackBuilder) { b.ts -= tc.back })
			s := find(t, b.pkts)
			if !checkSwitch(b.pkts, &s) || !s.keyframe || s.seqGap || !s.tsBack {
				t.Errorf("checkSwitch = %+v; want a keyframe whose timestamp goes back", s)
			}
		})
	}

	t.Run("frame no longer kept", func(t *testing.T) {
		t.Parallel()
		b := switchTrack(t0, nil)
		s := find(t, b.pkts)
		before := s
		if checkSwitch(b.pkts[:5], &s) || s != before {
			t.Errorf("checkSwitch without the frame = %+v; want false, the sample as it was", s)
		}
	})
}

func TestTimestampsForward(t *testing.T) {
	t.Parallel()
	pkts := []sfutest.Packet{{Seq: 65535, TS: 4294967000}, {Seq: 0, TS: 100}, {Seq: 2, TS: 300}, {Seq: 1, TS: 200}}
	if err := timestampsForward(pkts); err != nil {
		t.Errorf("a wrapping, reordered stream: %v", err)
	}
	pkts = append(pkts, sfutest.Packet{Seq: 3, TS: 250})
	if err := timestampsForward(pkts); err == nil {
		t.Error("a timestamp that goes back passed")
	}
}

func TestMapNTP(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	// NTP 2.5 s at RTP 90000, then NTP 3.5 s at RTP 180000 (90 kHz).
	srs := []sfutest.SenderReport{
		{Arrival: t0, NTP: 2<<32 | 1<<31, RTP: 90000},
		{Arrival: t0.Add(time.Second), NTP: 3<<32 | 1<<31, RTP: 180000},
	}
	for _, tc := range []struct {
		ts   uint32
		at   time.Time
		want time.Duration
	}{
		{ts: 99000, at: t0.Add(100 * time.Millisecond), want: 2600 * time.Millisecond},
		{ts: 180000 + 45000, at: t0.Add(1500 * time.Millisecond), want: 4 * time.Second},
		{ts: 81000, at: t0.Add(-time.Second), want: 2400 * time.Millisecond}, // before every SR: the first one
	} {
		got, ok := mapNTP(srs, tc.ts, tc.at, videoClock)
		if !ok || time.Duration(got) != tc.want {
			t.Errorf("mapNTP(%d) = %v, %v; want %v", tc.ts, time.Duration(got), ok, tc.want)
		}
	}
	if _, ok := mapNTP(nil, 0, t0, videoClock); ok {
		t.Error("mapNTP without sender reports is ok")
	}
}

func TestCheckList(t *testing.T) {
	t.Parallel()
	var l checkList
	l.add("measured, limit kept", true, "1", true, true, "< 2")
	l.add("measured, limit broken", true, "3", true, false, "< 2")
	l.add("reported only", true, "3", false, false, "< 2")
	l.add("nothing to measure", false, "", true, false, "< 2")
	want := []checkReport{
		{Name: "measured, limit kept", Value: "1", Limit: "< 2", Checked: true, Pass: true},
		{Name: "measured, limit broken", Value: "3", Limit: "< 2", Checked: true, Pass: false},
		{Name: "reported only", Value: "3", Checked: false, Pass: true},
		{Name: "nothing to measure", Value: "nothing to measure", Limit: "< 2", Checked: false, Pass: true},
	}
	for i := range want {
		if l.list[i] != want[i] {
			t.Errorf("check %d = %+v, want %+v", i, l.list[i], want[i])
		}
	}
}
