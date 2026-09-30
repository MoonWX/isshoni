package sfu

import (
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cpkt returns a packet with a payload of size bytes that arrived at at.
func cpkt(seq uint16, at int64, size int) *packet {
	return &packet{seq: seq, arrival: at, payload: make([]byte, size)}
}

// mustInsert inserts a packet and fails the test unless the cache takes it.
func mustInsert(t *testing.T, c *packetCache, p *packet) {
	t.Helper()
	if r := c.insert(p); r != cacheInserted {
		t.Fatalf("insert(%d): result %d", p.seq, r)
	}
}

func TestCacheCapacity(t *testing.T) {
	cases := []struct {
		pps         float64
		floor, want int
	}{
		{0, cacheVideoFloor, 1024},
		{-5, cacheVideoFloor, 1024},
		{math.NaN(), cacheVideoFloor, 1024},
		{1, cacheVideoFloor, 1024},   // a static screen: the floor
		{900, cacheVideoFloor, 1024}, // an 8 Mbps full layer
		{1024, cacheVideoFloor, 1024},
		{1024.5, cacheVideoFloor, 2048},
		{1025, cacheVideoFloor, 2048},
		{3000, cacheVideoFloor, 4096},
		{8192, cacheVideoFloor, 8192},
		{20000, cacheVideoFloor, 8192},
		{math.Inf(1), cacheVideoFloor, 8192},
		{50, cacheAudioFloor, 128}, // Opus at 20 ms: 2.5 s
		{128, cacheAudioFloor, 128},
		{129, cacheAudioFloor, 256},
	}
	for _, c := range cases {
		if got := cacheCapacity(c.pps, c.floor); got != c.want {
			t.Errorf("cacheCapacity(%v, %d) = %d, want %d", c.pps, c.floor, got, c.want)
		}
	}
	if v, a := newPacketCache(video), newPacketCache(audio); v.capacity() != 1024 || a.capacity() != 128 {
		t.Errorf("floors: video %d, audio %d", v.capacity(), a.capacity())
	}
}

func TestPacketCacheResize(t *testing.T) {
	sec := func(n int) int64 { return t0 + int64(n)*oneSecond }

	t.Run("grows at once, shrinks after 10 s at a quarter", func(t *testing.T) {
		c := newPacketCache(video)
		c.resize(3000, sec(0))
		if c.capacity() != 4096 {
			t.Fatalf("grow: %d", c.capacity())
		}
		for i := 1; i <= 11; i++ {
			c.resize(900, sec(i)) // wants 1024, a quarter of 4096
			if want := map[bool]int{true: 1024, false: 4096}[i == 11]; c.capacity() != want {
				t.Fatalf("after %d s at a quarter: capacity %d, want %d", i-1, c.capacity(), want)
			}
		}
	})

	t.Run("a wanted size above a quarter restarts the wait", func(t *testing.T) {
		c := newPacketCache(video)
		c.resize(3000, sec(0))
		c.resize(900, sec(1))
		c.resize(2000, sec(6)) // wants 2048: more than a quarter of 4096
		c.resize(900, sec(7))
		c.resize(900, sec(16))
		if c.capacity() != 4096 {
			t.Fatalf("shrank after 9 s: %d", c.capacity())
		}
		c.resize(900, sec(17))
		if c.capacity() != 1024 {
			t.Fatalf("no shrink after 10 s: %d", c.capacity())
		}
	})

	t.Run("shrinks to the largest size wanted in the 10 s", func(t *testing.T) {
		c := newPacketCache(video)
		c.resize(8000, sec(0))
		c.resize(10, sec(1))
		c.resize(1500, sec(5)) // 2048: a quarter of 8192
		c.resize(10, sec(11))
		if c.capacity() != 2048 {
			t.Fatalf("capacity %d, want 2048", c.capacity())
		}
	})

	t.Run("never below the floor", func(t *testing.T) {
		c := newPacketCache(audio)
		for i := range 30 {
			c.resize(0, sec(i))
		}
		if c.capacity() != cacheAudioFloor {
			t.Fatalf("capacity %d", c.capacity())
		}
	})

	t.Run("keeps the packets in the window", func(t *testing.T) {
		c := newPacketCache(video)
		for s := range 1024 {
			mustInsert(t, c, cpkt(uint16(s), t0, 10))
		}
		c.resize(3000, t0) // 4096: every packet stays
		for s := range 1024 {
			if c.get(uint16(s), t0) == nil {
				t.Fatalf("grow lost %d", s)
			}
		}
		for s := 1024; s < 3000; s++ {
			mustInsert(t, c, cpkt(uint16(s), t0, 10))
		}
		if c.get(0, t0) == nil || c.bytesHeld() != 30000 {
			t.Fatalf("the grown window: get(0) nil, or %d bytes", c.bytesHeld())
		}
		c.resize(10, t0)
		c.resize(10, t0+int64(cacheShrinkAfter)) // back to 1024: the newest 1024 stay
		if c.capacity() != 1024 || c.bytesHeld() != 10240 {
			t.Fatalf("shrink: capacity %d, %d bytes", c.capacity(), c.bytesHeld())
		}
		for s := range 3000 {
			if got := c.get(uint16(s), t0) != nil; got != (s >= 3000-1024) {
				t.Fatalf("after the shrink, get(%d): %v", s, got)
			}
		}
	})
}

func TestPacketCacheInsertGet(t *testing.T) {
	c := newPacketCache(video)
	if c.get(10, t0) != nil {
		t.Fatal("get before the first insert")
	}
	// Out of order: every packet lands in its slot.
	p10, p12, p11 := cpkt(10, t0, 100), cpkt(12, t0, 100), cpkt(11, t0, 100)
	for _, p := range []*packet{p10, p12, p11} {
		mustInsert(t, c, p)
	}
	for _, p := range []*packet{p10, p11, p12} {
		if c.get(p.seq, t0) != p {
			t.Fatalf("get(%d)", p.seq)
		}
	}
	if c.get(13, t0) != nil || c.get(9, t0) != nil {
		t.Fatal("get of a seq never inserted")
	}
	// A duplicate (a redundant RTX copy) is refused and leaves the slot as it is.
	if r := c.insert(cpkt(11, t0+tick, 100)); r != cacheDuplicate || c.get(11, t0) != p11 {
		t.Fatalf("duplicate: result %d", r)
	}
	if c.bytesHeld() != 300 {
		t.Fatalf("%d bytes", c.bytesHeld())
	}
	// A late packet within the capacity fills its empty slot; one a capacity behind is refused.
	for s := uint16(13); s <= 2000; s++ {
		mustInsert(t, c, cpkt(s, t0, 1))
	}
	if r := c.insert(cpkt(2000-1024, t0, 1)); r != cacheTooLate {
		t.Fatalf("a capacity behind: result %d", r)
	}
	if r := c.insert(cpkt(2000-1023, t0, 1)); r != cacheDuplicate {
		t.Fatalf("still cached: result %d", r)
	}
	if c.get(10, t0) != nil || c.get(2000-1024, t0) != nil || c.get(2000-1023, t0) == nil {
		t.Fatal("the window is the last 1024 seqs")
	}
	if c.bytesHeld() != 1024 {
		t.Fatalf("%d bytes after overwrites", c.bytesHeld())
	}
}

func TestPacketCacheLateFillsGap(t *testing.T) {
	c := newPacketCache(video)
	for _, s := range []uint16{100, 101, 103, 104} {
		mustInsert(t, c, cpkt(s, t0, 1))
	}
	late := cpkt(102, t0+tick, 1) // recovered by the publisher's RTX
	mustInsert(t, c, late)
	if c.get(102, t0+tick) != late {
		t.Fatal("the late packet isn't served")
	}
}

func TestPacketCacheWraparound(t *testing.T) {
	c := newPacketCache(video)
	var pkts []*packet
	for i := range 12 {
		p := cpkt(uint16(65530+i), t0, 1) // 65530 … 65535, 0 … 5
		pkts = append(pkts, p)
		if i != 5 { // 65535 arrives late, after the wrap
			mustInsert(t, c, p)
		}
	}
	mustInsert(t, c, pkts[5])
	for _, p := range pkts {
		if c.get(p.seq, t0) != p {
			t.Fatalf("get(%d) across the wrap", p.seq)
		}
	}
	if r := c.insert(cpkt(65535, t0, 1)); r != cacheDuplicate {
		t.Fatalf("duplicate across the wrap: result %d", r)
	}
}

func TestPacketCacheStaleSlotsAfterJumps(t *testing.T) {
	// Jumps leave old packets in their slots. They must never answer for the same seq 65536 later, as a hit or as a
	// duplicate.
	c := newPacketCache(video)
	for s := uint16(100); s <= 110; s++ {
		mustInsert(t, c, cpkt(s, t0, 1))
	}
	for _, s := range []uint16{30000, 60000} {
		mustInsert(t, c, cpkt(s, t0, 1))
	}
	if c.get(105, t0) != nil {
		t.Fatal("a packet 60000 seqs back is served")
	}
	for s := uint16(100); s <= 110; s++ { // the next cycle, without 105
		if s != 105 {
			mustInsert(t, c, cpkt(s, t0, 1))
		}
	}
	if c.get(105, t0) != nil {
		t.Fatal("the slot's packet from the last cycle answers for 105")
	}
	p := cpkt(105, t0, 1)
	mustInsert(t, c, p)
	if c.get(105, t0) != p {
		t.Fatal("get(105) after the insert")
	}
	if c.bytesHeld() != 13 { // 100 … 110, 30000, 60000: 105 of the last cycle was overwritten
		t.Fatalf("%d bytes", c.bytesHeld())
	}
}

func TestPacketCacheExpiry(t *testing.T) {
	c := newPacketCache(audio)
	p := cpkt(7, t0, 1)
	mustInsert(t, c, p)
	if c.get(7, t0+int64(cacheMaxAge)) != p {
		t.Fatal("a packet exactly 1.5 s old must be served")
	}
	if c.get(7, t0+int64(cacheMaxAge)+1) != nil {
		t.Fatal("a packet older than 1.5 s is served")
	}
}

func TestPacketCacheIdleScreenBurst(t *testing.T) {
	// 10 s of a static screen at 1 fps (3 packets per frame, so the measured rate is 3 pps), then a 300-packet frame
	// at 20 Mbps (about 0.5 ms per 1200-byte packet). A viewer's NACK for the frame's first packet, one RTT (200 ms)
	// later, is still served: a ring sized by the packet rate alone would have 128 slots.
	c := newPacketCache(video)
	seq, now := uint16(40000), t0
	for range 10 {
		for range 3 {
			mustInsert(t, c, cpkt(seq, now, 1200))
			seq++
		}
		now += oneSecond
		c.resize(3, now)
	}
	first := seq
	for k := range 300 {
		mustInsert(t, c, cpkt(seq, now+int64(k)*int64(480*time.Microsecond), 1200))
		seq++
	}
	c.resize(300, now+oneSecond)
	if c.capacity() != cacheVideoFloor || c.get(first, now+int64(200*time.Millisecond)) == nil {
		t.Fatalf("capacity %d; the frame's first packet isn't served 200 ms later", c.capacity())
	}
}

func TestPacketCacheConcurrent(t *testing.T) {
	// One writer (the Layer's RTP loop), the ticker resizing, and NACK readers, under -race.
	c := newPacketCache(video)
	var highest atomic.Int32
	highest.Store(-1)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		for s := range 50000 {
			if r := c.insert(cpkt(uint16(s), t0, 1)); r != cacheInserted {
				t.Errorf("insert(%d): result %d", s, r)
				return
			}
			highest.Store(int32(s))
		}
	})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			c.resize(float64(i%5000), t0+int64(i)*int64(cacheShrinkAfter)/4)
		}
	})
	for range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test data
			for {
				select {
				case <-done:
					return
				default:
				}
				h := highest.Load()
				if h < 0 {
					continue
				}
				seq := uint16(h - int32(rng.IntN(1024)))
				if p := c.get(seq, t0); p != nil && p.seq != seq {
					t.Errorf("get(%d) returned %d", seq, p.seq)
					return
				}
				_ = c.bytesHeld()
			}
		})
	}
	wg.Wait()
	if c.get(49999, t0) == nil {
		t.Fatal("the newest packet is missing")
	}
}

// BenchmarkPacketCacheInsertGet measures one insert (the Layer's RTP loop) plus one get (a NACK) on a 1024-slot
// cache. 02 §17 asks for less than 100 ns.
func BenchmarkPacketCacheInsertGet(b *testing.B) {
	c := newPacketCache(video)
	pkts := make([]*packet, 1<<16)
	for i := range pkts {
		pkts[i] = cpkt(uint16(i), t0, 1200)
	}
	i := 0
	for b.Loop() {
		c.insert(pkts[i&0xffff])
		if c.get(uint16(i-100), t0) == nil && i >= 100 {
			b.Fatal("miss")
		}
		i++
	}
}
