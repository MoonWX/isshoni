package push

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"testing"
	"time"
)

func TestDedupWindow(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := newDedup(10 * time.Minute)
	key := shareDedupKey("lounge", "alex")
	if d.seen(key, t0) {
		t.Error("a new key was seen")
	}
	d.mark(key, t0)
	for _, after := range []time.Duration{0, time.Second, 10*time.Minute - 1} {
		if !d.seen(key, t0.Add(after)) {
			t.Errorf("not seen %v after", after)
		}
	}
	for _, after := range []time.Duration{10 * time.Minute, time.Hour} {
		if d.seen(key, t0.Add(after)) {
			t.Errorf("still seen %v after", after)
		}
	}
	// The key is the pair: "a" + "bc" and "ab" + "c" are different sharers in different rooms.
	if shareDedupKey("a", "bc") == shareDedupKey("ab", "c") {
		t.Error("the dedup key is ambiguous")
	}
	if d.seen(shareDedupKey("lounge", "carol"), t0) || d.seen(shareDedupKey("movies", "alex"), t0) {
		t.Error("another sharer or room shares the window")
	}
}

func TestDedupSweep(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := newDedup(10 * time.Minute)
	for i := range limitSweepSize {
		d.mark(fmt.Sprint("old", i), t0)
	}
	d.mark("recent", t0.Add(5*time.Minute))
	if len(d.last) != limitSweepSize+1 {
		t.Fatalf("%d keys, want %d: nothing is over yet", len(d.last), limitSweepSize+1)
	}
	// The map is full: the next mark forgets the windows that are over.
	d.mark("new", t0.Add(10*time.Minute))
	if len(d.last) != 2 || !d.seen("recent", t0.Add(10*time.Minute)) || !d.seen("new", t0.Add(10*time.Minute)) {
		t.Errorf("%d keys after the sweep, want recent and new", len(d.last))
	}
}

func TestBuckets(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	b := newBuckets(3, 10*time.Minute)
	take := func(after time.Duration) bool { return b.take("bob", t0.Add(after)) }
	for i := range 3 {
		if !take(0) {
			t.Fatalf("token %d of the burst refused", i+1)
		}
	}
	if take(0) || take(10*time.Minute-1) {
		t.Error("a 4th token before the refill")
	}
	if !take(10 * time.Minute) {
		t.Error("no token after 10 minutes")
	}
	if take(10*time.Minute) || take(20*time.Minute-1) {
		t.Error("the refill gave more than one token")
	}
	// Odd intervals add up exactly: 3 + 7 minutes is one token, not 0.9999.
	if !take(20*time.Minute) || take(23*time.Minute) || !take(30*time.Minute) {
		t.Error("partial refills do not add up to whole tokens")
	}
	// A clock that steps back takes nothing away and gives nothing.
	if take(25 * time.Minute) {
		t.Error("a token appeared when the clock stepped back")
	}
	// A long pause refills to the burst, never past it.
	for i := range 3 {
		if !take(30 * 24 * time.Hour) {
			t.Fatalf("token %d after a long pause refused", i+1)
		}
	}
	if take(30 * 24 * time.Hour) {
		t.Error("more than the burst after a long pause")
	}
	// Recipients don't share a bucket.
	if !b.take("carol", t0) {
		t.Error("carol has no token")
	}
}

func TestBucketsSweep(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	b := newBuckets(3, 10*time.Minute)
	for i := range limitSweepSize - 1 {
		b.take(fmt.Sprint("user", i), t0)
	}
	for range 3 {
		b.take("busy", t0.Add(25*time.Minute)) // empty at the time of the sweep
	}
	if len(b.m) != limitSweepSize {
		t.Fatalf("%d buckets, want %d", len(b.m), limitSweepSize)
	}
	// The map is full: a new recipient forgets the buckets that are full again (a missing bucket is a full one).
	b.take("new", t0.Add(30*time.Minute))
	if len(b.m) != 2 {
		t.Errorf("%d buckets after the sweep, want busy and new", len(b.m))
	}
	if b.take("busy", t0.Add(30*time.Minute)) {
		t.Error("the sweep refilled a bucket that was not full")
	}
}

func TestShareTopic(t *testing.T) {
	topic := shareTopic("lounge", "k3m9p2qxw7ht")
	sum := sha256.Sum256([]byte("lounge/k3m9p2qxw7ht"))
	if want := "s" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]; topic != want {
		t.Errorf("topic %q, want %q", topic, want)
	}
	// RFC 8030 §5.4: at most 32 characters of the URL-safe base64 alphabet.
	if !regexp.MustCompile(`^s[A-Za-z0-9_-]{22}$`).MatchString(topic) {
		t.Errorf("topic %q is not 23 URL-safe characters", topic)
	}
	if topic != shareTopic("lounge", "k3m9p2qxw7ht") {
		t.Error("the topic is not stable")
	}
	if topic == shareTopic("lounge", "q7m2x9c4v8b1") || topic == shareTopic("movies", "k3m9p2qxw7ht") {
		t.Error("another sharer or room has the same topic")
	}
}
