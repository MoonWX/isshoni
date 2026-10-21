package publish

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestPacingRate: max(3 × Σ targets of the active video layers, 6 Mbit/s), a bucket of 40 ms at that rate that
// starts full (at the rate set before the first packet) and then keeps its tokens up to a new depth; audio, unknown
// layers and negative targets count 0; an inactive layer leaves the sum; a Publisher without Pacing has no pacer and
// SetPacingTargets changes nothing (§8.5).
func TestPacingRate(t *testing.T) {
	b := newBench(newScriptSource(nil), []string{"f", "q"}, true, true)
	p, pc := b.p, b.p.pacer
	state := func() (int64, float64) {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		return pc.rate, pc.tokens
	}
	if rate, tokens := state(); rate != MinPacingRate || tokens != MinPacingRate*0.04 {
		t.Fatalf("without targets: rate %d, tokens %.0f; want the floor and a full bucket", rate, tokens)
	}
	p.SetPacingTargets(map[string]int{"f": 8_000_000, "q": 400_000, media.LayerAudio: 128_000, "h": 9_000_000})
	if rate, _ := state(); rate != 25_200_000 {
		t.Errorf("f 8 Mbit/s and q 400 kbit/s pace at %d bit/s, want 25.2 Mbit/s", rate)
	}
	if _, tokens := state(); tokens != 25_200_000*0.04 {
		t.Errorf("before the first packet the bucket holds %.0f bits, want it full at the new rate", tokens)
	}
	if !pc.take(t.Context(), time.NewTimer(time.Hour), 1000) {
		t.Fatal("no tokens for a packet in a full bucket")
	}
	p.SetPacingTargets(map[string]int{"f": 16_000_000, "q": 400_000})
	if _, tokens := state(); tokens > 25_200_000*0.04 {
		t.Errorf("after a packet, a higher rate filled the bucket at once: %.0f bits", tokens)
	}
	p.SetPacingTargets(map[string]int{"f": 8_000_000, "q": 400_000})
	if err := p.SetActive("f", false); err != nil {
		t.Fatal(err)
	}
	if rate, _ := state(); rate != MinPacingRate {
		t.Errorf("with f off the rate is %d, want the floor (3 × 400 kbit/s is below it)", rate)
	}
	if err := p.SetActive("f", true); err != nil {
		t.Fatal(err)
	}
	p.SetPacingTargets(map[string]int{"f": 3_000_000, "q": -5})
	if rate, _ := state(); rate != 9_000_000 {
		t.Errorf("f 3 Mbit/s with q negative paces at %d, want 9 Mbit/s", rate)
	}
	p.SetPacingTargets(map[string]int{"f": 1_000_000})
	if rate, tokens := state(); rate != MinPacingRate || tokens > MinPacingRate*0.04 {
		t.Errorf("rate %d with %.0f tokens; want the floor and at most its bucket", rate, tokens)
	}

	plain := newBench(newScriptSource(nil), []string{"f"}, false, false)
	plain.p.SetPacingTargets(map[string]int{"f": 8_000_000})
	if plain.p.pacer != nil {
		t.Error("a Publisher without Pacing has a pacer")
	}
}

// TestPacerKeyframe: at 24 Mbit/s (an 8 Mbit/s full layer) a 300 KB keyframe's last packet leaves 55–70 ms after it
// was taken from a full bucket, 95–110 ms after it from an empty one (a 120 KB keyframe went first), and the audio
// written meanwhile is not delayed by more than 2 ms: it never waits behind video (§8.5, §14). Every packet leaves.
func TestPacerKeyframe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   int // bytes of a keyframe that goes first
		min, max time.Duration
	}{
		{"full bucket", 0, 55 * time.Millisecond, 70 * time.Millisecond},
		{"empty bucket", 120_000, 95 * time.Millisecond, 110 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var script []scripted
				if tc.before > 0 {
					script = append(script, scripted{pkt: videoFrame("f", tc.before, true)})
				}
				script = append(script, scripted{pkt: videoFrame("f", 300_000, true)})
				for i := range 11 {
					script = append(script, scripted{at: time.Duration(i) * 20 * time.Millisecond, pkt: audioPacket()})
				}
				src := newScriptSource(script)
				b := newBench(src, []string{"f"}, true, true)
				b.p.SetPacingTargets(map[string]int{"f": 8_000_000})
				start := time.Now()
				b.run(t.Context())
				time.Sleep(500 * time.Millisecond)
				synctest.Wait()
				b.stop()

				marks := b.writers["f"].markers()
				if want := 1 + min(tc.before, 1); len(marks) != want {
					t.Fatalf("%d frames left, want %d", len(marks), want)
				}
				if took := marks[len(marks)-1].Sub(start); took < tc.min || took > tc.max {
					t.Errorf("the 300 KB keyframe's last packet left after %v, want %v–%v", took, tc.min, tc.max)
				} else {
					t.Logf("the 300 KB keyframe's last packet left after %v", took)
				}
				audio := b.writers[""].all()
				if len(audio) != 11 {
					t.Fatalf("%d audio packets written, want 11", len(audio))
				}
				for i, a := range audio {
					due := start.Add(time.Duration(i) * 20 * time.Millisecond)
					if late := a.at.Sub(due); late > 2*time.Millisecond {
						t.Errorf("audio packet %d left %v late", i, late)
					}
				}
				f := b.trackStats("f")
				var sent uint64
				for _, w := range b.writers["f"].all() {
					sent += uint64(w.n)
				}
				if f.Frames != len(marks) || f.Packets != uint64(len(b.writers["f"].all())) || f.Octets != sent ||
					b.p.Stats().Dropped != 0 {
					t.Errorf("stats %+v, dropped %d; want every packet counted once and nothing dropped", f,
						b.p.Stats().Dropped)
				}
			})
		})
	}
}

// TestPacerBackpressure: while more than 300 ms of video at the pacing rate waits, the send loop doesn't call the
// Source's Next, and nothing is dropped: every frame leaves, in order, at the pacing rate.
func TestPacerBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const frames, size = 8, 100_000
		var script []scripted
		for range frames {
			script = append(script, scripted{pkt: videoFrame("f", size, true)})
		}
		src := newScriptSource(script)
		b := newBench(src, []string{"f"}, false, true) // no targets: the 6 Mbit/s floor
		limit := float64(MinPacingRate) * PacingBacklog.Seconds() / 8
		over := 0
		src.onNext = func() {
			b.p.pacer.mu.Lock()
			defer b.p.pacer.mu.Unlock()
			if float64(b.p.pacer.queued) > limit {
				over++
			}
		}
		start := time.Now()
		b.run(t.Context())
		synctest.Wait()
		if n := src.taken(); n >= frames || n < 2 {
			t.Errorf("the send loop took %d of %d frames at once, want it to stop at the 300 ms backlog", n, frames)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		b.stop()

		if over > 0 {
			t.Errorf("Next was called %d times with more than 300 ms queued", over)
		}
		marks := b.writers["f"].markers()
		if len(marks) != frames || src.taken() != frames {
			t.Fatalf("%d frames left, %d taken; want all %d", len(marks), src.taken(), frames)
		}
		var bytes int
		for _, w := range b.writers["f"].all() {
			bytes += w.hdr.MarshalSize() + w.n
		}
		// From a full bucket (40 ms), the rest at 6 Mbit/s.
		want := time.Duration(float64(bytes*8)/MinPacingRate*float64(time.Second)) - PacingDepth
		if took := marks[frames-1].Sub(start); took < want-time.Millisecond || took > want+5*time.Millisecond {
			t.Errorf("the last frame left after %v, want %v at the pacing rate", took, want)
		}
		if st := b.trackStats("f"); st.Frames != frames || b.p.Stats().Dropped != 0 {
			t.Errorf("stats %+v, dropped %d", st, b.p.Stats().Dropped)
		}
		pkts := b.writers["f"].all()
		for i := 1; i < len(pkts); i++ {
			if pkts[i].hdr.SequenceNumber != pkts[i-1].hdr.SequenceNumber+1 {
				t.Fatalf("packet %d has seq %d after %d: out of order or missing", i, pkts[i].hdr.SequenceNumber,
					pkts[i-1].hdr.SequenceNumber)
			}
		}
	})
}

// TestPacerSimulcast: one bucket for all layers, in the order the frames were taken; a rate raised while a frame
// waits applies at once.
func TestPacerSimulcast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newScriptSource([]scripted{
			{pkt: videoFrame("f", 300_000, true)},
			{pkt: videoFrame("q", 30_000, true)},
		})
		b := newBench(src, []string{"f", "q"}, false, true)
		start := time.Now()
		b.run(t.Context())
		time.Sleep(10 * time.Millisecond)
		b.p.SetPacingTargets(map[string]int{"f": 8_000_000, "q": 400_000}) // 25.2 Mbit/s from 10 ms on
		time.Sleep(time.Second)
		synctest.Wait()
		b.stop()
		f, q := b.writers["f"].markers(), b.writers["q"].markers()
		if len(f) != 1 || len(q) != 1 || !q[0].After(f[0]) {
			t.Fatalf("f's frame left at %v, q's at %v: want q's after f's", f, q)
		}
		// 30 KB at once, 10 ms at 6 Mbit/s (7.5 KB), the rest (about 270 KB of f's) at 25.2 Mbit/s: about 96 ms.
		if took := f[0].Sub(start); took < 85*time.Millisecond || took > 105*time.Millisecond {
			t.Errorf("f's frame left after %v, want about 96 ms", took)
		}
	})
}
