package sfu

// A ring validated by sequence number is also the design of Galene's packetcache (02 §14). No Galene code is copied
// here, so it needs no notice.

import (
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Packet cache sizing (02 §9.2).
const (
	cacheMaxSlots    = 8192                    // capacity limit
	cacheVideoFloor  = 1024                    // a static screen's first big frame must outlive the NACKs' RTT
	cacheAudioFloor  = 128                     // 2.5 s at 50 pps
	cacheWindow      = time.Second             // capacity ≈ this much at the measured packet rate
	cacheMaxAge      = 1500 * time.Millisecond // get ignores older packets
	cacheShrinkAfter = 10 * time.Second        // how long the wanted capacity must stay at a quarter or less
)

// cacheResult is the outcome of packetCache.insert.
type cacheResult uint8

const (
	// cacheInserted: the packet is cached; the Layer fans it out.
	cacheInserted cacheResult = iota
	// cacheDuplicate: the cache already holds that seq (Chrome's redundant RTX copies, 02 §9.1). Counted as an
	// ingress duplicate and dropped without fan-out; the cached packet stays as it is.
	cacheDuplicate
	// cacheTooLate: the packet is a capacity or more behind the highest seq. Counted as a too-late drop.
	cacheTooLate
)

// cacheEntry is one ring slot. ext is the unwrapped seq of p, so a slot is valid only for exactly that seq, never
// for one a multiple of 65536 away.
type cacheEntry struct {
	ext int64
	p   *packet // nil: empty
}

// packetCache holds the recent packets of one Layer for NACKs from every viewer (02 §9.2): a ring indexed by the
// unwrapped seq modulo the capacity, a power of two. Writes come from the Layer's RTP goroutine and the ticker
// (resize), reads from DownTrack RTCP goroutines. Payloads are shared, never copied per viewer.
type packetCache struct {
	mu      sync.RWMutex
	floor   int          // smallest capacity: cacheVideoFloor or cacheAudioFloor
	entries []cacheEntry // len is the capacity
	highest int64        // unwrapped seq of the highest packet inserted; valid once started
	started bool

	// Shrink hysteresis: since shrinkSince the wanted capacity has stayed at a quarter of the current one or less;
	// shrinkTo is the largest wanted capacity in that time.
	shrinking   bool
	shrinkSince int64
	shrinkTo    int

	bytes atomic.Int64 // payload bytes held (isshoni_sfu_packet_cache_bytes); read without the lock
}

// newPacketCache returns an empty cache at the floor capacity for a layer of the given kind.
func newPacketCache(kind webrtc.RTPCodecType) *packetCache {
	floor := cacheVideoFloor
	if kind == webrtc.RTPCodecTypeAudio {
		floor = cacheAudioFloor
	}
	return &packetCache{floor: floor, entries: make([]cacheEntry, floor)}
}

// cacheCapacity returns the capacity for a layer's packet rate: about 1 s of packets rounded up to a power of two,
// at least floor and at most cacheMaxSlots (02 §9.2).
func cacheCapacity(pps float64, floor int) int {
	want := math.Ceil(pps * cacheWindow.Seconds())
	switch {
	case math.IsNaN(want) || want <= float64(floor):
		return floor
	case want >= cacheMaxSlots:
		return cacheMaxSlots
	}
	return 1 << bits.Len(uint(want)-1)
}

// insert caches a packet (never a padding-only one). It returns cacheTooLate for a packet a capacity or more behind
// the highest seq (int16 arithmetic), cacheDuplicate when the slot already holds that seq, and otherwise overwrites
// the slot: out-of-order and late (publisher RTX) packets land in their empty slots.
func (c *packetCache) insert(p *packet) cacheResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		c.started, c.highest = true, int64(p.seq)
	}
	ext := c.ext(p.seq)
	if ext <= c.highest-int64(len(c.entries)) {
		return cacheTooLate
	}
	e := &c.entries[ext&int64(len(c.entries)-1)]
	if e.p != nil {
		if e.ext == ext {
			return cacheDuplicate
		}
		c.bytes.Add(-int64(len(e.p.payload)))
	}
	e.ext, e.p = ext, p
	c.bytes.Add(int64(len(p.payload)))
	c.highest = max(c.highest, ext)
	return cacheInserted
}

// get returns the cached packet with upstream seq seq, or nil when the cache doesn't hold it or it arrived more
// than 1.5 s before now.
func (c *packetCache) get(seq uint16, now int64) *packet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.started {
		return nil
	}
	ext := c.ext(seq)
	if ext > c.highest || ext <= c.highest-int64(len(c.entries)) {
		return nil
	}
	e := c.entries[ext&int64(len(c.entries)-1)]
	if e.p == nil || e.ext != ext || now-e.p.arrival > int64(cacheMaxAge) {
		return nil
	}
	return e.p
}

// resize adapts the capacity to the layer's measured packet rate; the ticker calls it every second (02 §9.2). It
// grows at once when the wanted capacity is larger, and shrinks only once the wanted capacity has stayed at a quarter
// of the current one or less for 10 s, to the largest wanted in that time (never below the floor). A resize keeps
// the packets still in the new window.
func (c *packetCache) resize(pps float64, now int64) {
	want := cacheCapacity(pps, c.floor)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch capacity := len(c.entries); {
	case want > capacity:
		c.shrinking = false
		c.setCapacity(want)
	case want*4 > capacity:
		c.shrinking = false
	case !c.shrinking:
		c.shrinking, c.shrinkSince, c.shrinkTo = true, now, want
	default:
		c.shrinkTo = max(c.shrinkTo, want)
		if now-c.shrinkSince >= int64(cacheShrinkAfter) {
			c.shrinking = false
			c.setCapacity(c.shrinkTo)
		}
	}
}

// capacity returns the number of slots.
func (c *packetCache) capacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// bytesHeld returns the payload bytes the cache holds, for isshoni_sfu_packet_cache_bytes.
func (c *packetCache) bytesHeld() int64 { return c.bytes.Load() }

// setCapacity replaces the ring with one of n slots (a power of two) that keeps the packets within both windows.
// The caller holds mu.
func (c *packetCache) setCapacity(n int) {
	entries := make([]cacheEntry, n)
	keep := c.highest - int64(min(n, len(c.entries)))
	for _, e := range c.entries {
		if e.p == nil {
			continue
		}
		if e.ext <= keep {
			c.bytes.Add(-int64(len(e.p.payload)))
			continue
		}
		entries[e.ext&int64(n-1)] = e
	}
	c.entries = entries
}

// ext unwraps seq to the int64 nearest the highest seq (within ±32768). The caller holds mu and has started.
func (c *packetCache) ext(seq uint16) int64 {
	return c.highest + int64(int16(seq-uint16(c.highest)))
}
