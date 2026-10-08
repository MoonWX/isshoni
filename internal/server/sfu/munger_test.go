package sfu

import (
	"math"
	"testing"
	"time"
)

// Test times are monotonic nanoseconds, far from zero like the SFU's.
const (
	t0         = int64(time.Hour)
	tick       = int64(10 * time.Millisecond)
	oneSecond  = int64(time.Second)
	videoClock = 90000
	audioClock = 48000
)

// vp returns a High-profile video packet of layer l.
func vp(l *Layer, seq uint16, ts uint32, key bool) *packet {
	return &packet{layer: l, seq: seq, ts: ts, profile: ProfileHigh, keyStart: key}
}

// cb returns a Constrained Baseline video packet of layer l.
func cb(l *Layer, seq uint16, ts uint32, key bool) *packet {
	return &packet{layer: l, seq: seq, ts: ts, profile: ProfileConstrainedBaseline, keyStart: key}
}

// ap returns an audio packet of layer l (audio packets always start a "keyframe").
func ap(l *Layer, seq uint16, ts uint32) *packet {
	return &packet{layer: l, seq: seq, ts: ts, keyStart: true}
}

// pad returns a padding-only packet of layer l.
func pad(l *Layer, seq uint16) *packet {
	return &packet{layer: l, seq: seq, profile: ProfileHigh, padding: true}
}

// fwd is a packet the munger forwarded, with its own seq and ts.
type fwd struct {
	p   *packet
	seq uint16
	ts  uint32
	v   verdict
}

// feed runs packets through the munger, 10 ms apart from at, and returns the forwarded ones.
func feed(m *munger, at int64, pkts ...*packet) []fwd {
	var out []fwd
	for i, p := range pkts {
		seq, ts, v := m.process(p, at+int64(i)*tick)
		if v.forwarded() {
			out = append(out, fwd{p, seq, ts, v})
		}
	}
	return out
}

// assertContinuous checks that forwarded packets have consecutive own seqs and timestamps that never go back.
func assertContinuous(t *testing.T, out []fwd) {
	t.Helper()
	for i := 1; i < len(out); i++ {
		if out[i].seq != out[i-1].seq+1 {
			t.Fatalf("seq gap at %d: %d -> %d", i, out[i-1].seq, out[i].seq)
		}
		if int32(out[i].ts-out[i-1].ts) < 0 {
			t.Fatalf("timestamp went backwards at %d: %d -> %d", i, out[i-1].ts, out[i].ts)
		}
	}
}

// assertLookup checks that a NACK for each forwarded own seq maps back to the packet's Layer and upstream seq.
func assertLookup(t *testing.T, m *munger, now int64, out []fwd) {
	t.Helper()
	for _, o := range out {
		e, ok := m.lookup(o.seq, now)
		if !ok {
			t.Fatalf("lookup(%d) failed", o.seq)
		}
		if e.layer != o.p.layer || o.seq-e.seqOff != o.p.seq || o.p.ts+e.tsOff != o.ts {
			t.Fatalf("lookup(%d) maps to layer %v seq %d ts %d, want layer %v seq %d ts %d", o.seq, e.layer.slot,
				o.seq-e.seqOff, o.p.ts+e.tsOff, o.p.layer.slot, o.p.seq, o.ts)
		}
	}
}

// videoMunger returns a video munger targeting slot.
func videoMunger(slot Slot) *munger {
	m := newMunger(videoClock)
	m.setTarget(slot, true)
	return m
}

// S4 port: TestMungerSwitchesOnlyOnKeyframe, with Layers instead of rids.
func TestMungerSwitchesOnlyOnKeyframe(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)

	// Layer f starts mid-GOP: nothing forwarded until its keyframe.
	out := feed(m, t0,
		vp(f, 100, 1000, false), vp(q, 500, 7000, true),
		vp(f, 101, 4000, true), vp(f, 102, 4000, false), vp(q, 501, 13000, false),
		vp(f, 103, 7000, false))
	if len(out) != 3 || out[0].p.layer != f || !out[0].p.keyStart || out[0].v != verdictNewEpoch {
		t.Fatalf("want 3 packets starting at f's keyframe, got %+v", out)
	}

	// Switch to q: keep forwarding f until q has a keyframe, then only q.
	m.setTarget(SlotQ, true)
	out2 := feed(m, t0+oneSecond,
		vp(q, 502, 16000, false), vp(f, 104, 10000, false),
		vp(q, 503, 19000, true), vp(f, 105, 13000, false), vp(q, 504, 22000, false))
	if len(out2) != 3 || out2[0].p.layer != f || out2[1].p.layer != q || !out2[1].p.keyStart || out2[2].p.layer != q {
		t.Fatalf("unexpected switch sequence %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
	if m.switches != 2 {
		t.Fatalf("want 2 switches, got %d", m.switches)
	}
}

// S4 port: TestMungerPauseResumeKeepsContinuity. M1: a resume on the same Layer keeps the timestamp offset.
func TestMungerPauseResumeKeepsContinuity(t *testing.T) {
	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	out := feed(m, t0, vp(f, 10, 100, true), vp(f, 11, 3100, false))

	m.setTarget(SlotF, false)
	if got := feed(m, t0, vp(f, 12, 6100, true)); len(got) != 0 {
		t.Fatalf("paused munger forwarded %+v", got)
	}

	m.setTarget(SlotF, true)
	out2 := feed(m, t0+2*oneSecond,
		vp(f, 13, 9100, false), // not a keyframe: must wait
		vp(f, 14, 12100, true), vp(f, 15, 15100, false))
	if len(out2) != 2 || !out2[0].p.keyStart {
		t.Fatalf("resume should start at a keyframe, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
	if tsOff := out[0].ts - 100; out2[0].ts != 12100+tsOff {
		t.Fatalf("resume on the same layer: ts %d, want upstream ts + the old offset %d", out2[0].ts, 12100+tsOff)
	}
}

// restart ends forwarding without a pause (the DownTrack calls it when a packet never left the server): the output
// goes on with the next keyframe of the target, right after the last own seq and with the layer's timestamp offset,
// and packets from before the restart are not replayed.
func TestMungerRestart(t *testing.T) {
	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	if _, ok := m.current(); ok {
		t.Fatal("a munger that forwards nothing has a current slot")
	}
	out := feed(m, t0, vp(f, 10, 100, true), vp(f, 11, 3100, false))
	if slot, ok := m.current(); !ok || slot != SlotF {
		t.Fatalf("current = %v, %v; want f", slot, ok)
	}

	m.restart()
	if _, ok := m.current(); ok || !m.active || !m.waitingForKeyframe() {
		t.Fatalf("after restart: current %v, active %v, waiting %v; want an active munger that waits for a keyframe", ok,
			m.active, m.waitingForKeyframe())
	}
	_, _, v := m.process(vp(f, 12, 6100, false), t0+3*tick)
	if v != verdictWaitKeyframe {
		t.Fatalf("a delta packet after restart: verdict %d, want it to wait for a keyframe", v)
	}
	if _, _, v := m.process(vp(f, 11, 3100, false), t0+3*tick); v != verdictDrop {
		t.Fatalf("a packet from before the restart: verdict %d, want it dropped", v)
	}
	out2 := feed(m, t0+4*tick, vp(f, 13, 9100, true), vp(f, 14, 12100, false))
	if len(out2) != 2 || out2[0].v != verdictNewEpoch {
		t.Fatalf("after restart the next keyframe starts an epoch, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
	if tsOff := out[0].ts - 100; out2[0].ts != 9100+tsOff {
		t.Fatalf("restart on the same layer: ts %d, want upstream ts + the old offset %d", out2[0].ts, 9100+tsOff)
	}
	m.setTarget(SlotF, false)
	if _, ok := m.current(); ok {
		t.Fatal("a paused munger has a current slot")
	}
}

// unforwardable is how the munger hears of a packet the DownTrack can't forward for lack of a payload type, so that
// process never sees it. A newer packet of the Layer forwarded now ends the epoch: the Layer moved on to a profile
// the viewer can't take. Without that, a flip back to the old profile would go on in the old epoch across a gap,
// and a NACK for the gap would map to the cached packets of the other profile (02 §9.4, "Further rules").
func TestMungerUnforwardable(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	if m.unforwardable(cb(f, 9, 0, true)) {
		t.Fatal("a munger that forwards nothing ended an epoch")
	}
	out := feed(m, t0, vp(f, 10, 100, true), vp(f, 11, 3100, false), vp(f, 12, 6100, false))
	epochs := m.n

	// Nothing ends for a late packet, for a repeat of the newest one, or for a packet of another Layer.
	for name, p := range map[string]*packet{
		"late": cb(f, 11, 3100, false), "repeated": cb(f, 12, 6100, false), "of q": cb(q, 500, 0, true),
	} {
		if m.unforwardable(p) {
			t.Fatalf("a %s packet ended the epoch", name)
		}
	}
	if slot, ok := m.current(); !ok || slot != SlotF {
		t.Fatalf("current = %v, %v; want f still forwarded", slot, ok)
	}

	// The Layer goes on in a profile without a payload type: three packets the munger only hears of.
	if !m.unforwardable(cb(f, 13, 9100, true)) {
		t.Fatal("a newer packet of the current Layer did not end the epoch")
	}
	if m.unforwardable(cb(f, 14, 9100, false)) || m.unforwardable(cb(f, 15, 12100, false)) {
		t.Fatal("an epoch ended twice")
	}
	if _, ok := m.current(); ok || !m.active || !m.waitingForKeyframe() || m.n != epochs {
		t.Fatalf("after the Layer moved on: current %v, active %v, waiting %v, %d epochs; want an active munger that "+
			"waits for a keyframe", ok, m.active, m.waitingForKeyframe(), m.n)
	}
	// Back in the old profile. A delta packet must not go on from before the gap: own seqs lastS+1 … would stand for
	// upstream 13–15, which the cache holds in the other profile.
	if _, _, v := m.process(vp(f, 16, 15100, false), t0+7*tick); v != verdictWaitKeyframe {
		t.Fatalf("a delta packet back in the old profile: verdict %d, want it to wait for a keyframe", v)
	}
	if _, _, v := m.process(vp(f, 12, 6100, false), t0+7*tick); v != verdictDrop {
		t.Fatalf("a packet from before the Layer moved on: verdict %d, want it dropped", v)
	}
	out2 := feed(m, t0+8*tick, vp(f, 17, 18100, true), vp(f, 18, 21100, false))
	if len(out2) != 2 || out2[0].v != verdictNewEpoch {
		t.Fatalf("the next keyframe in the old profile starts an epoch, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
	// The own seqs sent map back to what was sent, and none of them to a packet of the other profile.
	assertLookup(t, m, t0+10*tick, append(out, out2...))

	// Paused, the munger has nothing to end.
	m.setTarget(SlotF, false)
	if m.unforwardable(cb(f, 20, 27100, true)) {
		t.Fatal("a paused munger ended an epoch")
	}
}

// S4 port: TestMungerDropsReorderedPacketsFromBeforeSwitch.
func TestMungerDropsReorderedPacketsFromBeforeSwitch(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	out := feed(m, t0, vp(f, 10, 100, true), vp(f, 11, 100, false))
	m.setTarget(SlotQ, true)
	out2 := feed(m, t0,
		vp(q, 200, 5000, true),
		vp(q, 199, 2000, false), // arrived late, predates the switch point
		vp(q, 201, 5000, false))
	if len(out2) != 2 {
		t.Fatalf("late pre-switch packet must be dropped, got %+v", out2)
	}
	assertContinuous(t, append(out, out2...))
}

// S4 port: TestMungerAudioAlwaysForwardsWhenActive.
func TestMungerAudioAlwaysForwardsWhenActive(t *testing.T) {
	a := newLayer(SlotAudio, audio)
	m := newMunger(audioClock)
	m.setTarget(SlotAudio, true)
	out := feed(m, t0, ap(a, 1, 960), ap(a, 2, 1920), ap(a, 3, 2880))
	if len(out) != 3 {
		t.Fatalf("want 3 audio packets, got %d", len(out))
	}
	assertContinuous(t, out)
}

func TestMungerVerdicts(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := newMunger(videoClock)
	step := func(name string, p *packet, want verdict) {
		t.Helper()
		if _, _, v := m.process(p, t0); v != want {
			t.Fatalf("%s: verdict %d, want %d", name, v, want)
		}
	}
	step("paused: even a keyframe is dropped", vp(f, 1, 0, true), verdictDrop)
	if m.waitingForKeyframe() {
		t.Fatal("a paused munger waits for nothing")
	}
	if !m.setTarget(SlotF, true) || m.setTarget(SlotF, true) {
		t.Fatal("setTarget must report exactly the first change")
	}
	if !m.waitingForKeyframe() {
		t.Fatal("an active munger that forwards nothing waits for a keyframe")
	}
	step("another slot", vp(q, 1, 0, true), verdictDrop)
	step("the target without a keyframe", vp(f, 2, 0, false), verdictWaitKeyframe)
	step("the target's keyframe", vp(f, 3, 0, true), verdictNewEpoch)
	if m.waitingForKeyframe() {
		t.Fatal("forwarding the target: not waiting")
	}
	step("in the epoch", vp(f, 4, 0, false), verdictForward)
	step("a keyframe in the epoch starts no epoch", vp(f, 5, 3000, true), verdictForward)

	if !m.setTarget(SlotQ, true) || !m.waitingForKeyframe() {
		t.Fatal("a new target waits for its keyframe")
	}
	step("the new target without a keyframe", vp(q, 2, 0, false), verdictWaitKeyframe)
	step("the old layer keeps flowing", vp(f, 6, 3000, false), verdictForward)

	if !m.setTarget(SlotQ, false) || m.waitingForKeyframe() {
		t.Fatal("a pause is a change, and a paused munger waits for nothing")
	}
	step("paused", vp(f, 7, 3000, false), verdictDrop)
	if m.setTarget(SlotF, false) {
		t.Fatal("another target while paused changes nothing")
	}

	for v, want := range map[verdict]bool{verdictDrop: false, verdictWaitKeyframe: false, verdictForward: true,
		verdictNewEpoch: true} {
		if v.forwarded() != want {
			t.Errorf("verdict %d forwarded: %v", v, !want)
		}
	}
}

func TestMungerStartsAtRandomValues(t *testing.T) {
	seqs, tss := map[uint16]bool{}, map[uint32]bool{}
	for range 8 {
		m := newMunger(videoClock)
		seqs[m.seq0], tss[m.ts0] = true, true
	}
	if len(seqs) < 2 || len(tss) < 2 {
		t.Fatalf("8 mungers start at %d seqs and %d timestamps", len(seqs), len(tss))
	}

	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	m.seq0, m.ts0 = 4321, 987654321
	out := feed(m, t0, vp(f, 100, 5, true), vp(f, 101, 3005, false))
	if out[0].seq != 4321 || out[0].ts != 987654321 || out[1].seq != 4322 || out[1].ts != 987657321 {
		t.Fatalf("first packets %+v", out)
	}
}

func TestMungerWraparound(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	m.seq0, m.ts0 = 65534, math.MaxUint32-2999 // the output wraps too

	// Upstream seq and ts both wrap within the f epoch.
	var pkts []*packet
	for i := range 8 {
		pkts = append(pkts, vp(f, uint16(65532+i), 0xffffe000+uint32(i/2)*3000, i == 0))
	}
	out := feed(m, t0, pkts...)
	if len(out) != 8 {
		t.Fatalf("forwarded %d of 8", len(out))
	}
	// Switch to q, whose seq wraps right at its keyframe.
	m.setTarget(SlotQ, true)
	out = append(out, feed(m, t0+oneSecond, vp(q, 65535, 0xfffff000, true), vp(q, 0, 0xfffff000, false),
		vp(q, 1, 0xfffff000+3000, false), vp(f, 4, 12000, false))...)
	if len(out) != 11 || out[8].p.layer != q {
		t.Fatalf("switch across the wrap: %+v", out[8:])
	}
	assertContinuous(t, out)
	if out[0].seq != 65534 || out[2].seq != 0 || out[0].ts != math.MaxUint32-2999 || out[2].ts != 0 {
		t.Fatalf("the output doesn't wrap as expected: %+v", out[:3])
	}
	assertLookup(t, m, t0+oneSecond, out)

	sr := &srInfo{rtp: 0xfffff000 - 45000}
	if rtp, ok := m.translateSR(q, sr); !ok || rtp != out[8].ts-45000 {
		t.Fatalf("translateSR across the wrap: %d %v, want %d", rtp, ok, out[8].ts-45000)
	}
}

func TestMungerLookupAcrossSwitches(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	now := t0
	var out []fwd
	send := func(p *packet) {
		p.payload, p.arrival = []byte{byte(p.seq)}, now
		p.layer.cache.insert(p)
		if seq, ts, v := m.process(p, now); v.forwarded() {
			out = append(out, fwd{p, seq, ts, v})
		}
		now += tick
	}
	fs, qs := uint16(1000), uint16(5000)
	nextF := func(key bool) { send(vp(f, fs, uint32(fs)*3000, key)); fs++ }
	nextQ := func(key bool) { send(vp(q, qs, uint32(qs)*3000, key)); qs++ }
	// Three switches after the start: f → q → f → q, each on a keyframe, with both layers flowing all the time.
	targets := []Slot{SlotF, SlotQ, SlotF, SlotQ}
	for i, slot := range targets {
		m.setTarget(slot, true)
		for k := range 5 {
			key := k == 2
			nextF(key && slot == SlotF)
			nextQ(key && slot == SlotQ)
		}
		if m.switches != i+1 {
			t.Fatalf("after target %d: %d switches", i, m.switches)
		}
	}
	assertContinuous(t, out)
	assertLookup(t, m, now, out)

	// A NACK is served from the packet's own layer cache.
	for _, o := range out {
		e, _ := m.lookup(o.seq, now)
		if got := e.layer.cache.get(o.seq-e.seqOff, now); got != o.p {
			t.Fatalf("own seq %d: the cache returns %+v, want %+v", o.seq, got, o.p)
		}
	}

	if _, ok := m.lookup(m.lastS+1, now); ok {
		t.Error("lookup of a seq not sent yet")
	}
	if _, ok := m.lookup(m.lastS-maxLateSeq-1, now); ok {
		t.Error("lookup of a seq more than maxLateSeq back")
	}
	// Once the current epoch is more than 2 s old, earlier epochs are out of reach; its own seqs are not.
	later := m.newest().at + int64(nackLookback) + 1
	for _, o := range out {
		_, ok := m.lookup(o.seq, later)
		if cur := int16(o.seq-m.newest().startS) >= 0; ok != cur {
			t.Fatalf("lookup(%d) 2 s after the last switch: %v, want %v", o.seq, ok, cur)
		}
	}
}

func TestMungerPaddingClosesGaps(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	out := feed(m, t0, vp(f, 10, 0, true), vp(f, 11, 0, false))
	m.skipPadding(pad(f, 12), t0)
	m.skipPadding(pad(f, 13), t0) // a run of padding extends one epoch
	if m.n != 2 {
		t.Fatalf("%d epochs after a padding run, want 2", m.n)
	}
	out = append(out, feed(m, t0, vp(f, 14, 3000, false), vp(f, 15, 3000, false))...)
	m.skipPadding(pad(f, 16), t0) // padding after media: another epoch
	out = append(out, feed(m, t0, vp(f, 17, 6000, false))...)
	if m.n != 3 {
		t.Fatalf("%d epochs, want 3", m.n)
	}
	assertContinuous(t, out)

	// Out-of-order padding is ignored: the gap stays, and a NACK for it maps to the padding seq, which the cache
	// never holds.
	gap := feed(m, t0, vp(f, 18, 9000, false), vp(f, 20, 9000, false))
	m.skipPadding(pad(f, 19), t0)
	if m.n != 3 || gap[1].seq != gap[0].seq+2 {
		t.Fatalf("out-of-order padding: %d epochs, seqs %d %d", m.n, gap[0].seq, gap[1].seq)
	}
	if e, ok := m.lookup(gap[0].seq+1, t0); !ok || gap[0].seq+1-e.seqOff != 19 {
		t.Fatalf("the gap maps to upstream %d (%v), want 19", gap[0].seq+1-e.seqOff, ok)
	}
	// Padding of another layer, from the future, or while paused changes nothing.
	m.skipPadding(pad(q, 21), t0)
	m.skipPadding(pad(f, 22), t0)
	m.setTarget(SlotF, false)
	m.skipPadding(pad(f, 21), t0)
	if m.n != 3 || m.lastU != 20 {
		t.Fatalf("ignored padding changed the munger: %d epochs, lastU %d", m.n, m.lastU)
	}
	assertLookup(t, m, t0, append(out, gap...))
}

func TestMungerLatePackets(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)

	t.Run("across a padding epoch: mapped into the gap", func(t *testing.T) {
		m := videoMunger(SlotF)
		out := feed(m, t0, vp(f, 10, 0, true), vp(f, 11, 0, false), vp(f, 13, 3000, false)) // 12 is late
		m.skipPadding(pad(f, 14), t0)
		out = append(out, feed(m, t0, vp(f, 15, 6000, false))...)
		lastS, lastTS := m.lastS, m.lastTS
		seq, ts, v := m.process(vp(f, 12, 0, false), t0)
		if v != verdictForward || seq != out[0].seq+2 || ts != out[0].ts {
			t.Fatalf("late packet across padding: seq %d ts %d verdict %d, want seq %d", seq, ts, v, out[0].seq+2)
		}
		if m.lastS != lastS || m.lastTS != lastTS {
			t.Fatal("a late packet moved lastS or lastTS")
		}
	})

	t.Run("across a layer switch: dropped", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 10, 0, true), vp(f, 12, 0, false))
		m.setTarget(SlotQ, true)
		feed(m, t0, vp(q, 300, 0, true), vp(q, 302, 0, false))
		for _, p := range []*packet{vp(f, 11, 0, false), vp(q, 299, 0, false)} {
			if _, _, v := m.process(p, t0); v != verdictDrop {
				t.Fatalf("late %v packet across a switch: verdict %d", p.layer.slot, v)
			}
		}
		if _, _, v := m.process(vp(q, 301, 0, false), t0); v != verdictForward {
			t.Fatal("a reordered packet within the epoch must be forwarded")
		}
	})

	t.Run("colliding with a resume epoch: dropped", func(t *testing.T) {
		m := videoMunger(SlotF)
		out := feed(m, t0, vp(f, 10, 0, true), vp(f, 12, 3000, false)) // 11 is late
		m.setTarget(SlotF, false)
		m.setTarget(SlotF, true)
		out = append(out, feed(m, t0+oneSecond, vp(f, 14, 93000, true))...) // 13 was never forwarded
		if out[2].seq != out[1].seq+1 {
			t.Fatalf("resume: %+v", out)
		}
		if seq, _, v := m.process(vp(f, 11, 0, false), t0); v != verdictForward || seq != out[0].seq+1 {
			t.Fatalf("late 11: seq %d verdict %d, want %d", seq, v, out[0].seq+1)
		}
		// 13 would map to the resume epoch's first own seq.
		if _, _, v := m.process(vp(f, 13, 6000, false), t0); v != verdictDrop {
			t.Fatalf("late 13: verdict %d, want a drop", v)
		}
	})

	t.Run("from before a pause: dropped", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 10, 0, true), vp(f, 12, 3000, false)) // 11 is late
		m.setTarget(SlotF, false)
		m.setTarget(SlotF, true)
		// Even a keyframe from before the pause doesn't restart the output: it would go back in time.
		if _, _, v := m.process(vp(f, 11, 3000, true), t0+oneSecond); v != verdictDrop || m.n != 1 {
			t.Fatalf("late packet after a resume: verdict %d, %d epochs", v, m.n)
		}
		if _, _, v := m.process(vp(f, 13, 6000, false), t0+oneSecond); v != verdictWaitKeyframe {
			t.Fatalf("the next packet after a resume: verdict %d", v)
		}
		if _, _, v := m.process(vp(f, 14, 9000, true), t0+oneSecond); v != verdictNewEpoch {
			t.Fatalf("the next keyframe after a resume: verdict %d", v)
		}
	})

	t.Run("a long pause: seqs aren't compared", func(t *testing.T) {
		// After more than nackLookback without output, the upstream seq may have wrapped past lastU: a keyframe that
		// looks behind it starts an epoch as usual, with or without a new profile.
		const behind = 1<<16 + 10 - 100 // 100 behind seq 10
		for _, p := range []*packet{vp(f, behind, 900000, true), cb(f, behind, 900000, true), vp(f, 10, 900000, true)} {
			m := videoMunger(SlotF)
			feed(m, t0, vp(f, 10, 0, true))
			m.setTarget(SlotF, false)
			m.setTarget(SlotF, true)
			if _, _, v := m.process(p, t0+int64(nackLookback)+tick); v != verdictNewEpoch || m.newest().profile != p.profile {
				t.Fatalf("seq %d (%s) after a long pause: verdict %d", p.seq, p.profile, v)
			}
		}
	})

	t.Run("media with the seq of in-order padding: dropped", func(t *testing.T) {
		m := videoMunger(SlotF)
		out := feed(m, t0, vp(f, 10, 0, true), vp(f, 11, 0, false))
		m.skipPadding(pad(f, 12), t0)
		epoch := *m.newest()
		// A publisher that reuses the padding's seq for media: own seq 11+1 would be sent twice.
		if _, _, v := m.process(vp(f, 12, 3000, false), t0); v != verdictDrop || *m.newest() != epoch {
			t.Fatalf("media with the padding's seq: verdict %d, epoch %+v, was %+v", v, *m.newest(), epoch)
		}
		out = append(out, feed(m, t0, vp(f, 13, 3000, false))...)
		assertContinuous(t, out)
		// After the next packet it is a late one, which collides with the padding epoch's first own seq.
		if _, _, v := m.process(vp(f, 12, 3000, false), t0); v != verdictDrop {
			t.Fatalf("late media with the padding's seq: verdict %d", v)
		}
		// The same for a repeat of the newest seq right after an epoch starts.
		m.setTarget(SlotQ, true)
		feed(m, t0, vp(q, 300, 0, true))
		if _, _, v := m.process(vp(q, 300, 0, false), t0); v != verdictDrop {
			t.Fatalf("a repeat of the epoch's first seq: verdict %d", v)
		}
	})

	t.Run("more than maxLateSeq behind: dropped", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 0, 0, true))
		for s := 3; s <= maxLateSeq+2; s++ { // 1 and 2 are late
			m.process(vp(f, uint16(s), 0, false), t0)
		}
		if _, _, v := m.process(vp(f, 1, 0, false), t0); v != verdictDrop {
			t.Fatalf("maxLateSeq+1 behind: verdict %d", v)
		}
		if seq, _, v := m.process(vp(f, 2, 0, false), t0); v != verdictForward || seq != m.seq0+2 {
			t.Fatalf("maxLateSeq behind: verdict %d seq %d", v, seq)
		}
	})
}

// srSwitch sets up the SR-aligned switch scenario: layer f forwards 30 frames at 30 fps starting at its SR, then the
// target moves to q. f's SR is at NTP n0, RTP 1_000_000; q's SR (half a second later on the same capture clock) at
// RTP 5_000_000. The returned time is 1 s after f's SR.
func srSwitch(t *testing.T) (m *munger, f, q *Layer, now int64) {
	t.Helper()
	f, q = newLayer(SlotF, video), newLayer(SlotQ, video)
	m = videoMunger(SlotF)
	m.seq0, m.ts0 = 1000, 50000
	const n0 = uint64(3_900_000_000) << 32
	f.lastSR.Store(&srInfo{ntp: n0, rtp: 1_000_000, arrival: t0})
	q.lastSR.Store(&srInfo{ntp: n0 + 1<<31, rtp: 5_000_000, arrival: t0})
	for k := range 30 {
		_, ts, v := m.process(vp(f, uint16(100+k), 1_000_000+uint32(k)*3000, k == 0), t0+int64(k)*oneSecond/30)
		if !v.forwarded() || ts != 50000+uint32(k)*3000 {
			t.Fatalf("frame %d: ts %d verdict %d", k, ts, v)
		}
	}
	m.setTarget(SlotQ, true)
	return m, f, q, t0 + oneSecond
}

func TestMungerSRAlignedSwitch(t *testing.T) {
	m, _, q, now := srSwitch(t)
	lastS := m.lastS
	// q's keyframe was captured 1 s after f's SR: 0.5 s after q's own SR. On f's timeline that is 1_090_000, which the
	// f epoch sent as 50000 + 90000.
	seq, ts, v := m.process(vp(q, 7000, 5_045_000, true), now)
	if v != verdictNewEpoch || seq != lastS+1 || ts != 140000 {
		t.Fatalf("SR-aligned switch: seq %d ts %d verdict %d, want seq %d ts 140000", seq, ts, v, lastS+1)
	}
	// The new layer's SR translates to the same output timeline: 0.5 s after f's SR.
	if rtp, ok := m.translateSR(q, q.lastSR.Load()); !ok || rtp != 95000 {
		t.Fatalf("translateSR: %d %v, want 95000", rtp, ok)
	}
}

func TestMungerSRAlignedFallbacks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f, q *Layer, now int64)
	}{
		{"no SR on the new layer", func(_, q *Layer, _ int64) { q.lastSR.Store(nil) }},
		{"no SR on the old layer", func(f, _ *Layer, _ int64) { f.lastSR.Store(nil) }},
		{"stale SR on the old layer", func(f, _ *Layer, now int64) {
			sr := *f.lastSR.Load()
			sr.arrival = now - int64(srMaxAge) - 1
			f.lastSR.Store(&sr)
		}},
		{"stale SR on the new layer", func(_, q *Layer, now int64) {
			sr := *q.lastSR.Load()
			sr.arrival = now - int64(srMaxAge) - 1
			q.lastSR.Store(&sr)
		}},
		// The monotonic guard: the SRs put q's keyframe 2 s before f's last frame.
		{"backwards", func(_, q *Layer, _ int64) {
			sr := *q.lastSR.Load()
			sr.ntp -= 3 << 32
			q.lastSR.Store(&sr)
		}},
		{"equal to the last timestamp", func(_, q *Layer, _ int64) {
			sr := *q.lastSR.Load()
			sr.rtp += 3000 // puts the keyframe on f's last frame (1_087_000 on f's timeline)
			q.lastSR.Store(&sr)
		}},
		{"further ahead than the elapsed time plus 1 s", func(_, q *Layer, _ int64) {
			sr := *q.lastSR.Load()
			sr.ntp += 30 << 32
			q.lastSR.Store(&sr)
		}},
		{"an absurd NTP difference", func(_, q *Layer, _ int64) {
			sr := *q.lastSR.Load()
			sr.ntp += 1 << 62
			q.lastSR.Store(&sr)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, f, q, now := srSwitch(t)
			c.setup(f, q, now)
			want := m.lastTS + uint32((now-m.lastAt)*videoClock/oneSecond) // the wall clock
			if _, ts, v := m.process(vp(q, 7000, 5_045_000, true), now); v != verdictNewEpoch || ts != want {
				t.Fatalf("ts %d verdict %d, want the wall-clock ts %d", ts, v, want)
			}
		})
	}
	// After more than maxWallJumpNs without output, SR alignment is off (its bound would overflow), and the wall
	// clock steps by maxWallJump.
	t.Run("a long pause", func(t *testing.T) {
		m, _, q, now := srSwitch(t)
		m.lastAt = now - maxWallJumpNs - 1
		last := m.lastTS
		if _, ts, _ := m.process(vp(q, 7000, 5_045_000, true), now); ts != last+maxWallJump {
			t.Fatalf("ts %d after a pause longer than maxWallJumpNs, want %d", ts, last+maxWallJump)
		}
	})
}

func TestMungerResumeSameLayerKeepsTSOffset(t *testing.T) {
	t.Run("video", func(t *testing.T) {
		f := newLayer(SlotF, video)
		m := videoMunger(SlotF)
		out := feed(m, t0, vp(f, 10, 1000, true), vp(f, 11, 4000, false))
		m.setTarget(SlotF, false)
		m.setTarget(SlotF, true)
		// The keyframe after 2.1 s carries its media time; the offset stays, whatever the wall clock says.
		out = append(out, feed(m, t0+5*oneSecond, vp(f, 70, 1000+189000, true))...)
		assertContinuous(t, out)
		if tsOff := out[0].ts - 1000; out[2].ts != 190000+tsOff {
			t.Fatalf("ts %d, want %d", out[2].ts, 190000+tsOff)
		}
	})
	t.Run("audio after off", func(t *testing.T) {
		a := newLayer(SlotAudio, audio)
		m := newMunger(audioClock)
		m.setTarget(SlotAudio, true)
		out := feed(m, t0, ap(a, 1, 960), ap(a, 2, 1920))
		m.setTarget(SlotAudio, false)
		feed(m, t0, ap(a, 3, 2880))
		m.setTarget(SlotAudio, true)
		out = append(out, feed(m, t0+3*oneSecond, ap(a, 200, 960*200))...)
		assertContinuous(t, out)
		if tsOff := out[0].ts - 960; out[2].ts != 960*200+tsOff || m.n != 2 || m.switches != 1 {
			t.Fatalf("ts %d, want %d (%d epochs, %d switches)", out[2].ts, 960*200+tsOff, m.n, m.switches)
		}
	})
	t.Run("the monotonic guard", func(t *testing.T) {
		// A resume whose timestamp is behind the last one (the publisher reset its clock on the same track) falls
		// back to the wall clock.
		f := newLayer(SlotF, video)
		m := videoMunger(SlotF)
		out := feed(m, t0, vp(f, 10, 900000, true), vp(f, 11, 903000, false))
		m.setTarget(SlotF, false)
		m.setTarget(SlotF, true)
		now := t0 + 2*oneSecond
		want := m.lastTS + uint32((now-m.lastAt)*videoClock/oneSecond)
		if _, ts, _ := m.process(vp(f, 12, 1000, true), now); ts != want || int32(ts-out[1].ts) <= 0 {
			t.Fatalf("ts %d, want the wall-clock ts %d", ts, want)
		}
	})
}

func TestMungerNewLayerInstanceIsNewEpoch(t *testing.T) {
	// A publisher rebuild attaches a new Layer with the same rid: its packets wait for its keyframe, and the old
	// instance flows until then. After that the old instance is another Layer of the target slot: not forwarded.
	f1, f2 := newLayer(SlotF, video), newLayer(SlotF, video)
	m := videoMunger(SlotF)
	var got []verdict
	var out []fwd
	for _, p := range []*packet{vp(f1, 10, 0, true), vp(f1, 11, 3000, false), vp(f2, 500, 70000, false),
		vp(f1, 12, 6000, false), vp(f2, 501, 73000, true), vp(f1, 13, 9000, false), vp(f2, 502, 76000, false)} {
		seq, ts, v := m.process(p, t0)
		got = append(got, v)
		if v.forwarded() {
			out = append(out, fwd{p, seq, ts, v})
		}
	}
	want := []verdict{verdictNewEpoch, verdictForward, verdictWaitKeyframe, verdictForward, verdictNewEpoch,
		verdictWaitKeyframe, verdictForward}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
	}
	assertContinuous(t, out)
	if m.switches != 2 || m.n != 2 {
		t.Fatalf("%d switches, %d epochs", m.switches, m.n)
	}
}

func TestMungerProfileChangeWaitsForKeyframe(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	t.Run("the target layer", func(t *testing.T) {
		m := videoMunger(SlotF)
		var got []verdict
		var out []fwd
		for _, p := range []*packet{vp(f, 10, 0, true), vp(f, 11, 3000, false),
			cb(f, 12, 6000, false), // the policy changed: CB from here on
			vp(f, 13, 6000, false), // a High packet after that can't resume the old epoch
			cb(f, 14, 9000, true), cb(f, 15, 9000, false)} {
			seq, ts, v := m.process(p, t0)
			got = append(got, v)
			if v.forwarded() {
				out = append(out, fwd{p, seq, ts, v})
			}
		}
		want := []verdict{verdictNewEpoch, verdictForward, verdictWaitKeyframe, verdictWaitKeyframe, verdictNewEpoch,
			verdictForward}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("verdicts %v, want %v", got, want)
			}
		}
		assertContinuous(t, out)
		if tsOff := out[0].ts; out[2].ts != 9000+tsOff || m.newest().profile != ProfileConstrainedBaseline {
			t.Fatalf("the profile epoch on the same layer keeps tsOff: ts %d, want %d", out[2].ts, 9000+tsOff)
		}
	})
	t.Run("a late packet with the old profile", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 10, 0, true), vp(f, 12, 3000, false), cb(f, 13, 6000, true)) // 11 is late
		// Neither a keyframe request (the CB epoch is running) nor a switch back to High.
		if _, _, v := m.process(vp(f, 11, 3000, false), t0); v != verdictDrop || !m.forwarding {
			t.Fatalf("late old-profile packet: verdict %d, forwarding %v", v, m.forwarding)
		}
		// The SPS of the last High keyframe, recovered by RTX after the CB epoch started.
		if _, _, v := m.process(vp(f, 11, 3000, true), t0); v != verdictDrop || m.n != 2 ||
			m.newest().profile != ProfileConstrainedBaseline {
			t.Fatalf("late old-profile keyframe: verdict %d, %d epochs, profile %s", v, m.n, m.newest().profile)
		}
		if _, _, v := m.process(cb(f, 14, 6000, false), t0); v != verdictForward {
			t.Fatalf("the CB epoch goes on: verdict %d", v)
		}
	})
	t.Run("a late packet with the old profile after the change ended forwarding", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 10, 0, true), vp(f, 12, 3000, false))
		if _, _, v := m.process(cb(f, 13, 6000, false), t0); v != verdictWaitKeyframe || m.forwarding { // 11 is late
			t.Fatalf("the new profile's first packet: verdict %d, forwarding %v", v, m.forwarding)
		}
		for _, p := range []*packet{vp(f, 11, 3000, true), vp(f, 12, 3000, false)} {
			if _, _, v := m.process(p, t0); v != verdictDrop || m.n != 1 || m.forwarding {
				t.Fatalf("late old-profile packet %d: verdict %d, %d epochs", p.seq, v, m.n)
			}
		}
		if _, _, v := m.process(cb(f, 14, 9000, true), t0); v != verdictNewEpoch || m.newest().profile != ProfileConstrainedBaseline {
			t.Fatalf("the CB keyframe: verdict %d", v)
		}
	})
	t.Run("the old layer during a switch", func(t *testing.T) {
		m := videoMunger(SlotF)
		feed(m, t0, vp(f, 10, 0, true))
		m.setTarget(SlotQ, true)
		if _, _, v := m.process(cb(f, 11, 3000, false), t0); v != verdictDrop || !m.waitingForKeyframe() {
			t.Fatalf("a new profile on the old layer: verdict %d", v)
		}
		if _, _, v := m.process(cb(q, 50, 3000, true), t0); v != verdictNewEpoch {
			t.Fatalf("the target's keyframe: verdict %d", v)
		}
	})
}

func TestMungerTranslateSR(t *testing.T) {
	f, q := newLayer(SlotF, video), newLayer(SlotQ, video)
	m := videoMunger(SlotF)
	sr := &srInfo{rtp: 5000}
	if _, ok := m.translateSR(f, sr); ok {
		t.Fatal("translateSR before the first epoch")
	}
	out := feed(m, t0, vp(f, 1, 8000, true))
	if rtp, ok := m.translateSR(f, sr); !ok || rtp != out[0].ts-3000 {
		t.Fatalf("translateSR: %d %v, want %d", rtp, ok, out[0].ts-3000)
	}
	if _, ok := m.translateSR(q, sr); ok {
		t.Fatal("translateSR of a layer that isn't forwarded")
	}
	if _, ok := m.translateSR(f, nil); ok {
		t.Fatal("translateSR without an SR")
	}
	m.setTarget(SlotQ, true) // f still flows
	if _, ok := m.translateSR(f, sr); !ok {
		t.Fatal("translateSR while the old layer still flows")
	}
	m.setTarget(SlotQ, false)
	if _, ok := m.translateSR(f, sr); ok {
		t.Fatal("translateSR while paused")
	}
}

func TestMungerWallTicks(t *testing.T) {
	v, a := newMunger(videoClock), newMunger(audioClock)
	cases := []struct {
		m       *munger
		elapsed int64
		want    uint32
	}{
		{v, -oneSecond, 1},
		{v, 0, 1},
		{v, 1, 1},
		{v, oneSecond, 90000},
		{a, 20 * int64(time.Millisecond), 960},
		{v, int64(time.Hour), 324_000_000},
		{v, maxWallJumpNs, maxWallJump},
		{v, 10 * int64(time.Hour), maxWallJump},
		{v, math.MaxInt64, maxWallJump},
	}
	for _, c := range cases {
		if got := c.m.wallTicks(c.elapsed); got != c.want {
			t.Errorf("wallTicks(%d) at %d Hz = %d, want %d", c.elapsed, c.m.clock, got, c.want)
		}
	}
}

func TestMungerLongEpoch(t *testing.T) {
	// 40000 packets in one epoch, across the upstream wrap: the epoch's start trails its newest packet by at most
	// maxEpochSpan, so reordered packets, padding and NACKs still work with int16 arithmetic.
	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	const n = 40000
	const late = n - 5
	first := uint16(60000)
	var out []fwd
	for i := range n {
		if i == late {
			continue
		}
		p := vp(f, first+uint16(i), uint32(i/3)*3000, i == 0)
		if seq, ts, v := m.process(p, t0); v.forwarded() {
			out = append(out, fwd{p, seq, ts, v})
		} else {
			t.Fatalf("packet %d not forwarded", i)
		}
	}
	if span := m.lastS - m.newest().startS; span != maxEpochSpan {
		t.Fatalf("epoch span %d, want %d", span, maxEpochSpan)
	}
	seq, _, v := m.process(vp(f, first+late, uint32(late/3)*3000, false), t0)
	if v != verdictForward || seq != m.seq0+late {
		t.Fatalf("reordered packet late in a long epoch: seq %d verdict %d, want %d", seq, v, m.seq0+late)
	}
	assertLookup(t, m, t0, out[len(out)-maxLateSeq:])
	if _, ok := m.lookup(m.lastS-maxLateSeq-1, t0); ok {
		t.Fatal("lookup beyond maxLateSeq")
	}
	m.skipPadding(pad(f, first+n), t0)
	if seq, _, _ := m.process(vp(f, first+n+1, n*1000, false), t0); seq != m.seq0+n {
		t.Fatalf("after padding: seq %d, want %d", seq, m.seq0+n)
	}
}

func TestMungerEpochRing(t *testing.T) {
	// 70 padding epochs: the ring keeps the newest 64.
	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	var out []fwd
	s := uint16(100)
	for i := range 70 {
		out = append(out, feed(m, t0, vp(f, s, uint32(i)*3000, i == 0))...)
		m.skipPadding(pad(f, s+1), t0)
		s += 2
	}
	out = append(out, feed(m, t0, vp(f, s, 70*3000, false))...)
	if m.n != 71 {
		t.Fatalf("%d epochs", m.n)
	}
	assertContinuous(t, out)
	assertLookup(t, m, t0, out[len(out)-63:])
	if _, ok := m.lookup(out[0].seq, t0); ok {
		t.Fatal("lookup through an epoch that left the ring")
	}
	// A late packet from before every epoch in the ring is dropped.
	if _, _, v := m.process(vp(f, 99, 0, false), t0); v != verdictDrop {
		t.Fatalf("late packet older than the ring: verdict %d", v)
	}
}

func TestSlotString(t *testing.T) {
	for s, want := range map[Slot]string{SlotQ: "q", SlotH: "h", SlotF: "f", SlotAudio: "audio", 9: "slot?"} {
		if got := s.String(); got != want {
			t.Errorf("Slot(%d) = %q, want %q", s, got, want)
		}
	}
}

func BenchmarkMungerProcess(b *testing.B) {
	f := newLayer(SlotF, video)
	m := videoMunger(SlotF)
	p := vp(f, 0, 0, true)
	for b.Loop() {
		m.process(p, t0)
		p.seq++
		p.keyStart = false
	}
}

// fuzzReader hands out the fuzz input byte by byte; an exhausted input reads as zeros.
type fuzzReader struct{ data []byte }

func (r *fuzzReader) byte() byte {
	if len(r.data) == 0 {
		return 0
	}
	b := r.data[0]
	r.data = r.data[1:]
	return b
}

func (r *fuzzReader) u16() uint16 { return uint16(r.byte())<<8 | uint16(r.byte()) }
func (r *fuzzReader) u32() uint32 { return uint32(r.u16())<<16 | uint32(r.u16()) }

// fuzzSource is one upstream Layer in FuzzMunger: consecutive seqs, and timestamps that move per frame on a capture
// clock shared by every layer, as a real publisher's do.
type fuzzSource struct {
	layer   *Layer
	seq     uint16
	tsBase  uint32 // RTP ts at capture time 0
	frame   int64  // capture time of the current frame, in ticks
	profile ProfileKey
	held    []*packet // media packets lost upstream (or reusing a padding seq), delivered later (loose mode)
}

func (s *fuzzSource) next(key bool) *packet {
	p := &packet{layer: s.layer, seq: s.seq, ts: s.tsBase + uint32(s.frame), profile: s.profile, keyStart: key}
	s.seq++
	return p
}

// FuzzMunger drives a munger with random interleavings of three video layers (f, q, and a rebuilt f), keyframes,
// padding (and media that reuses a padding seq, which the cache can't catch), target changes, pauses, profile
// changes, sender reports (consistent or not) and time steps, and checks the forwarded stream (02 §17):
//   - every own seq is forwarded at most once, and a NACK lookup of it maps back to the same Layer and upstream seq;
//   - an epoch starts only on a keyframe of the target slot, right after the highest own seq, with a timestamp after
//     the highest own ts; nothing is forwarded while paused, and other forwarded packets are of the current epoch's
//     Layer and profile;
//   - in strict mode (every packet delivered in order), the output seq is continuous and the output ts never goes back;
//   - translateSR shifts an SR of the forwarded layer by the current epoch's offset.
//
// Loose mode holds packets back and delivers them late, as upstream loss followed by publisher RTX does.
func FuzzMunger(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0x10, 0x20, 0, 0, 0, 1, 0, 0, 2, 0, 3, 5, 7, 0, 0, 9, 1, 1, 8, 20})
	f.Add([]byte{1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xf0, 0, 0, 0, 0, 0, 0, 5, 0, 5, 0, 6, 0, 6, 1, 7, 1,
		0, 1, 3, 1, 0, 1, 9, 0, 1, 10, 0, 2, 0, 0, 0})
	f.Add([]byte{0, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 9, 0, 1, 9, 1, 1, 0, 0, 0x80, 3, 0, 40, 7, 1, 1, 1, 0, 1, 1, 0x80, 8, 20, 4, 0, 0, 0,
		11, 7, 2, 7, 0, 10, 0, 0, 2, 0x80, 12, 2, 4})
	// Media 11, padding 12, media 12 on f (strict), then the same with the reuse held and delivered late (loose).
	for _, mode := range []byte{0, 1} {
		seed := append([]byte{mode}, make([]byte, 24)...)
		seed[7], seed[8] = 0, 10 // f starts at seq 10
		// f: key 10, 11, padding 12 with media 12 (held in loose mode), 13, then the held one.
		seed = append(seed, 0, 0, 0, 0x30, 5, 0xa2, 0, 0x30, 7, 0)
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		strict := r.byte()&1 == 0
		m := newMunger(videoClock)
		m.seq0, m.ts0 = r.u16(), r.u32()
		srcs := []*fuzzSource{
			{layer: newLayer(SlotF, video)}, {layer: newLayer(SlotQ, video)}, {layer: newLayer(SlotF, video)},
		}
		for _, s := range srcs {
			s.seq, s.tsBase, s.profile = r.u16(), r.u32(), ProfileHigh
		}
		const n0 = uint64(3_900_000_000) << 32
		now := t0
		m.setTarget(SlotF, true)

		type sent struct {
			layer *Layer
			seq   uint16
		}
		byOwn := map[int64]sent{} // unwrapped own seq → upstream packet
		var hiS uint16            // highest own seq forwarded
		var hiExt int64           // its unwrapped value
		var hiTS uint32           // highest own ts forwarded
		var lastInOrderTS uint32  // strict mode: ts of the latest in-order packet
		started := false
		var cur *Layer // the Layer of the latest epoch
		var curProfile ProfileKey
		var curTSOff uint32
		packets := 0

		deliver := func(p *packet) {
			packets++
			active := m.active
			target := m.target
			seq, ts, v := m.process(p, now)
			if !v.forwarded() {
				return
			}
			if !active {
				t.Fatalf("forwarded while paused: %+v", p)
			}
			ext := hiExt + int64(int16(seq-hiS))
			if started {
				if prev, dup := byOwn[ext]; dup {
					t.Fatalf("own seq %d forwarded twice: upstream %d and %d", seq, prev.seq, p.seq)
				}
			}
			newHigh := !started || ext > hiExt
			if v == verdictNewEpoch {
				if !p.keyStart || p.layer.slot != target {
					t.Fatalf("epoch started by %+v (target %v)", p, target)
				}
				if started && (ext != hiExt+1 || int32(ts-hiTS) <= 0) {
					t.Fatalf("new epoch at seq %d ts %d after seq %d ts %d", seq, ts, hiS, hiTS)
				}
				cur, curProfile, curTSOff = p.layer, p.profile, ts-p.ts
			} else if p.layer != cur || p.profile != curProfile {
				t.Fatalf("forwarded %+v outside the current epoch", p)
			}
			if strict {
				if !newHigh || (started && ext != hiExt+1) {
					t.Fatalf("strict: seq %d after %d", seq, hiS)
				}
				if started && v == verdictForward && int32(ts-lastInOrderTS) < 0 {
					t.Fatalf("strict: ts %d after %d", ts, lastInOrderTS)
				}
				lastInOrderTS = ts
			}
			byOwn[ext] = sent{p.layer, p.seq}
			if newHigh {
				hiS, hiExt = seq, ext
			}
			if !started || int32(ts-hiTS) > 0 {
				hiTS = ts
			}
			started = true
			e, ok := m.lookup(seq, now)
			if newHigh && !ok {
				t.Fatalf("lookup of the newest own seq %d failed", seq)
			}
			if ok && (e.layer != p.layer || seq-e.seqOff != p.seq) {
				t.Fatalf("lookup(%d) maps to upstream %d, forwarded %d", seq, seq-e.seqOff, p.seq)
			}
		}

		for ops := 0; ops < 2048 && len(r.data) > 0 && packets < 20000; ops++ {
			op, arg := r.byte()%13, r.byte()
			s := srcs[int(arg)%len(srcs)]
			switch op {
			case 0, 1, 2: // deliver the next media packet
				deliver(s.next(arg&0x30 == 0))
			case 3: // a burst of in-order packets of one frame
				for range int(arg) % 64 {
					deliver(s.next(false))
				}
			case 4: // a new frame
				s.frame += int64(arg%64) * 500
			case 5: // padding
				p := s.next(false)
				p.padding = true
				m.skipPadding(p, now)
				if arg&0x80 != 0 {
					// A non-compliant publisher reuses the padding's seq for media: right away, or later in loose mode.
					// The cache never holds padding, so the munger gets it.
					reuse := &packet{layer: s.layer, seq: p.seq, ts: p.ts, profile: s.profile, keyStart: arg&0x40 != 0}
					if strict || arg&0x20 == 0 {
						deliver(reuse)
					} else {
						s.held = append(s.held, reuse)
					}
				}
			case 6: // upstream loss: the packet arrives later (loose), or now (strict)
				p := s.next(arg&0x40 == 0)
				if strict {
					deliver(p)
				} else {
					s.held = append(s.held, p)
				}
			case 7: // a held packet arrives
				if len(s.held) > 0 {
					i := int(arg) % len(s.held)
					p := s.held[i]
					s.held = append(s.held[:i], s.held[i+1:]...)
					deliver(p)
				}
			case 8: // the viewer's target
				switch arg % 3 {
				case 0:
					m.setTarget(SlotF, true)
				case 1:
					m.setTarget(SlotQ, true)
				default:
					m.setTarget(SlotF, false)
				}
			case 9: // time passes
				switch {
				case arg == 0xff:
					now += int64(srMaxAge) + oneSecond
				case arg >= 0xf0:
					now += int64(nackLookback) + oneSecond
				default:
					now += int64(arg) * int64(time.Millisecond)
				}
			case 10: // a sender report: consistent with the shared capture clock, or garbage
				sr := &srInfo{
					ntp: n0 + uint64(s.frame)<<32/videoClock, rtp: s.tsBase + uint32(s.frame), arrival: now,
				}
				if arg&0x80 != 0 {
					sr.ntp, sr.rtp = uint64(r.u32())<<24, r.u32()
				}
				s.layer.lastSR.Store(sr)
			case 11: // a profile change on this layer
				if s.profile == ProfileHigh {
					s.profile = ProfileConstrainedBaseline
				} else {
					s.profile = ProfileHigh
				}
			case 12: // NACK lookups and SR translation
				if started {
					own := hiS - uint16(arg)
					if e, ok := m.lookup(own, now); ok {
						if sp, sent := byOwn[hiExt-int64(arg)]; sent && (sp.layer != e.layer || own-e.seqOff != sp.seq) {
							t.Fatalf("lookup(%d) maps to upstream %d, forwarded %d", own, own-e.seqOff, sp.seq)
						}
					}
				}
				sr := &srInfo{rtp: r.u32()}
				rtp, ok := m.translateSR(s.layer, sr)
				if ok != (m.active && m.forwarding && s.layer == cur) || ok && rtp != sr.rtp+curTSOff {
					t.Fatalf("translateSR: %d %v", rtp, ok)
				}
			}
		}
	})
}
