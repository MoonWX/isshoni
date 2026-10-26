package main

import (
	"context"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// TestRecorderStamps checks the SetExclusions recorder (05 §18.1): it stamps every SetExclusions call with the
// engine clock just before forwarding it, so cases 13 and 15 can measure a rule change from the exact moment it
// reached the engine. It uses the fake engine, so it runs on every OS.
func TestRecorderStamps(t *testing.T) {
	eng := fake.NewEngine(fake.EngineConfig{})
	t.Cleanup(func() { _ = eng.Close() })

	var tick int64
	clock := func() int64 { tick += 1_000_000; return tick } // +1 ms per call
	rec := newRecorder(eng, clock)

	if _, ok := rec.lastStamp(); ok {
		t.Fatal("no stamp expected before any SetExclusions")
	}
	// The recorder forwards to the wrapped engine and records a stamp each time.
	for i := range 3 {
		if err := rec.SetExclusions(context.Background(), media.Exclusions{}); err != nil {
			t.Fatalf("SetExclusions %d: %v", i, err)
		}
	}
	if n := rec.stampCount(); n != 3 {
		t.Fatalf("stamp count %d, want 3", n)
	}
	last, ok := rec.lastStamp()
	if !ok || last != 3_000_000 {
		t.Fatalf("last stamp %d (ok=%v), want 3000000", last, ok)
	}
	// Case 15 reads the count before its SetAppDecision and takes the next stamp as T_set (05 §18.5).
	if s, ok := rec.stampAt(1); !ok || s != 2_000_000 {
		t.Fatalf("stamp 1 is %d (ok=%v), want 2000000", s, ok)
	}
	if _, ok := rec.stampAt(3); ok {
		t.Fatal("a stamp that wasn't recorded yet exists")
	}

	// Other AudioController methods still reach the engine (the recorder embeds it).
	if _, err := rec.AudioReport(context.Background()); err != nil {
		t.Fatalf("AudioReport through the recorder: %v", err)
	}
}
