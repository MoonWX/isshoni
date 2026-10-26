package audio

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestPollingRates is 05 §14.4 under synctest: no polling when nothing is watched, AudioLevels every 250 ms while
// watching with no level meter, every 100 ms (and audio.levels at 10 Hz) with one, and the meter count never below 0.
func TestPollingRates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()
		polls := func() int { return len(ctl.ops("levels")) }

		// Idle: no call at all, with or without a meter.
		settle(10 * time.Second)
		s.WatchLevels(true)
		settle(time.Second)
		if n := len(ctl.ops()); n != 0 {
			t.Fatalf("%d engine calls while idle", n)
		}
		s.WatchLevels(false)
		s.WatchLevels(false) // the count stays at 0 …
		s.WatchLevels(false)

		// Watching, no meter: a poll at once, then every 250 ms; no audio.levels.
		if err := s.Watch(true); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := polls(); n != 1 {
			t.Fatalf("%d polls when the watch started", n)
		}
		settle(time.Second)
		if n := polls(); n != 5 {
			t.Errorf("%d polls in the first second, want 1 + 4", n)
		}
		if n := len(named(rec.take(), EventLevels)); n != 0 {
			t.Errorf("%d audio.levels without a meter", n)
		}

		// One meter (… so one true makes it 1): every 100 ms, audio.levels at 10 Hz.
		s.WatchLevels(true)
		settle(time.Second)
		if n := polls(); n != 15 {
			t.Errorf("%d polls after a second with a meter, want 5 + 10", n)
		}
		levels := named(rec.take(), EventLevels)
		if len(levels) != 10 {
			t.Fatalf("%d audio.levels in a second", len(levels))
		}
		for i, ev := range levels {
			if lv := ev.Payload.(Levels); lv.Seq != uint64(i+1) || lv.Apps == nil {
				t.Errorf("levels %d: %+v", i, lv)
			}
		}
		// Two meters, then both gone: back to 250 ms.
		s.WatchLevels(true)
		s.WatchLevels(false)
		s.WatchLevels(false)
		settle(time.Second)
		if n := polls(); n != 19 {
			t.Errorf("%d polls after a second without a meter, want 15 + 4", n)
		}

		// The report: fetched when ReportSeq changes, not on every poll.
		if n := len(ctl.ops("report")); n != 1 {
			t.Errorf("%d reports fetched without a change", n)
		}
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{app("a.exe", "A", verdictHeard, "")} })
		settle(time.Second)
		if n := len(ctl.ops("report")); n != 2 {
			t.Errorf("%d reports fetched after one change", n)
		}

		// Not watching: no polling.
		if err := s.Watch(false); err != nil {
			t.Fatal(err)
		}
		before := len(ctl.ops())
		settle(10 * time.Second)
		if n := len(ctl.ops()) - before; n != 0 {
			t.Errorf("%d engine calls after the watch ended", n)
		}

		// Live without the sheet: polling again, from the watcher's start. A shared window (a rule change, which
		// wakes Run before the watcher starts) doesn't add a poll.
		before = polls()
		s.OnShareStarted(77, false)
		synctest.Wait()
		if n := polls() - before; n != 1 {
			t.Fatalf("%d polls when the share started", n)
		}
		settle(time.Second)
		if n := polls() - before; n != 5 {
			t.Errorf("%d polls in a live second, want 1 + 4", n)
		}
		s.OnShareStopped()
		stop()
	})
}

// TestReportRate: engine reports become audio.report at most 4 times a second, however often ReportSeq changes.
func TestReportRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		s := New(ctl, newStore(DefaultSettings()), testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()
		s.WatchLevels(true) // 100 ms polls
		_ = s.Watch(true)
		settle(time.Second)
		rec.take()
		start := time.Now()
		for i := range 40 { // a visible change every 50 ms for 2 s
			ctl.set(func(r *media.AudioReport) {
				r.Apps = []media.AudioApp{app("a.exe", fmt.Sprintf("A %d", i), verdictHeard, "")}
			})
			settle(50 * time.Millisecond)
		}
		settle(time.Second)
		var last time.Time
		n := 0
		for _, c := range ctl.ops("report") {
			if c.at.Before(start) {
				continue
			}
			if !last.IsZero() && c.at.Sub(last) < minReportGap {
				t.Errorf("reports fetched %v apart", c.at.Sub(last))
			}
			last = c.at
			n++
		}
		reports := len(named(rec.take(), EventReport))
		if n < 8 || n > 13 || reports != n {
			t.Errorf("%d reports fetched and %d audio.report in 3 s", n, reports)
		}
		stop()
	})
}
