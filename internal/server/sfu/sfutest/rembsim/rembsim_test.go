package rembsim

import (
	"math"
	"testing"
	"time"
)

// TestModel checks the rules of 02 §17 against values worked out by hand, on a link stepped every 100 ms: the start
// at the received rate, growth by 8 % a second, the fall to 0.85 × the received rate on overuse (at most once per
// 200 ms, and never up), a REMB with the first estimate, every second and at once on a fall of 3 %, and the ceiling of
// 1.5 × the received rate + 10 kbit/s that keeps a viewer which receives little from reporting much.
func TestModel(t *testing.T) {
	const ms = time.Millisecond
	grow := func(est float64, d time.Duration) float64 { return est * math.Pow(1.08, d.Seconds()) }
	type row struct {
		at                  time.Duration
		forwarded, received float64 // bits in the 100 ms before at
		est                 float64 // 0: don't check
		send                bool
	}
	var rows []row
	// 1 Mbit/s, all of it arriving: no estimate before 0.5 s of it has arrived.
	for i := range 5 {
		rows = append(rows, row{at: time.Duration(i) * 100 * ms, forwarded: 100_000, received: 100_000})
	}
	// The estimate starts at the received rate, 500 kbit in 0.5 s, and goes out at once.
	rows = append(rows, row{at: 500 * ms, forwarded: 100_000, received: 100_000, est: 1_000_000, send: true})
	// It grows by 8 % a second; the next REMB a second after the first.
	for i := 6; i <= 14; i++ {
		at := time.Duration(i) * 100 * ms
		rows = append(rows, row{at: at, forwarded: 100_000, received: 100_000, est: grow(1_000_000, at-500*ms)})
	}
	rows = append(rows, row{at: 1500 * ms, forwarded: 100_000, received: 100_000, est: 1_080_000, send: true})
	// Overuse: 3 Mbit/s forwarded on a 1 Mbit/s link. The estimate falls to 0.85 × 1 Mbit/s, and a REMB goes at once.
	rows = append(rows, row{at: 1600 * ms, forwarded: 300_000, received: 100_000, est: 850_000, send: true})
	// Overuse goes on: within 200 ms nothing more; then 0.85 × the same rate is no lower, so nothing either.
	rows = append(rows, row{at: 1700 * ms, forwarded: 300_000, received: 100_000, est: 850_000})
	rows = append(rows, row{at: 1800 * ms, forwarded: 300_000, received: 100_000, est: 850_000})
	// The link narrows to 0.5 Mbit/s: the last 0.5 s brought 100+100+100+100+50 kbit, 900 kbit/s, so 765 kbit/s, a
	// fall of 10 % since the last REMB.
	rows = append(rows, row{at: 1900 * ms, forwarded: 300_000, received: 50_000, est: 765_000, send: true})
	// The sender slows down to what the link carries: no overuse, so the estimate grows again, under 1.5 × the
	// received rate + 10 kbit/s (at 2.0 s that is 1.5 × 800 kbit/s + 10 kbit/s, far above).
	est := grow(765_000, 100*ms)
	rows = append(rows, row{at: 2000 * ms, forwarded: 50_000, received: 50_000, est: est})
	est = grow(est, 100*ms) // the ceiling is 1.5 × 700 + 10 kbit/s
	rows = append(rows, row{at: 2100 * ms, forwarded: 50_000, received: 50_000, est: est})
	est = grow(est, 100*ms) // 1.5 × 600 + 10 kbit/s
	rows = append(rows, row{at: 2200 * ms, forwarded: 50_000, received: 50_000, est: est})
	// From here the ceiling, 1.5 × 500 kbit/s + 10 kbit/s = 760 kbit/s, is below the estimate: it stays where it is,
	// neither growing nor falling, and goes out once a second.
	for i := 23; i <= 40; i++ {
		rows = append(rows, row{at: time.Duration(i) * 100 * ms, forwarded: 50_000, received: 50_000, est: est,
			send: i == 29 || i == 39})
	}

	var m Model
	for _, r := range rows {
		got, send := m.Step(r.at, r.forwarded, r.received)
		if r.est != 0 && math.Abs(got-r.est) > 1 {
			t.Errorf("at %v: estimate %.0f, want %.0f", r.at, got, r.est)
		}
		if send != r.send {
			t.Errorf("at %v: REMB sent %v, want %v (estimate %.0f)", r.at, send, r.send, got)
		}
	}
	if got := m.Received(); math.Abs(got-500_000) > 1 {
		t.Errorf("received rate at the end %.0f, want 500000", got)
	}
}

// TestModelCeiling: a viewer that receives 200 kbit/s reports at most 1.5 × that + 10 kbit/s, however long its link
// has room, and its estimate never falls without overuse, however little it receives after.
func TestModelCeiling(t *testing.T) {
	var m Model
	at := time.Duration(0)
	for range 600 { // a minute at 200 kbit/s
		m.Step(at, 20_000, 20_000)
		at += 100 * time.Millisecond
	}
	if got := m.Estimate(); math.Abs(got-310_000) > 1 {
		t.Fatalf("estimate after a minute at 200 kbit/s = %.0f, want 310000", got)
	}
	for range 100 { // then 10 s at 20 kbit/s
		m.Step(at, 2_000, 2_000)
		at += 100 * time.Millisecond
	}
	if got := m.Estimate(); math.Abs(got-310_000) > 1 {
		t.Fatalf("estimate after 10 s at 20 kbit/s = %.0f, want it to stay at 310000", got)
	}
	// And it grows only as far as what arrives allows: 8 Mbit/s from now on lifts the ceiling to 12.01 Mbit/s, which
	// 8 % a second takes a long time to reach.
	for range 300 {
		m.Step(at, 800_000, 800_000)
		at += 100 * time.Millisecond
	}
	want := 310_000 * math.Pow(1.08, 30)
	if got := m.Estimate(); math.Abs(got-want) > want*1e-9 {
		t.Fatalf("estimate after 30 s at 8 Mbit/s = %.0f, want %.0f", got, want)
	}
}
