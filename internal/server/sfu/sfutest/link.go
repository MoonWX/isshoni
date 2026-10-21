package sfutest

import (
	"sync"
	"time"
)

// LinkBuffer is how much of its capacity a Link holds: a burst up to that size passes, what comes faster than the
// capacity beyond it is lost, as at the bottleneck of a downlink whose buffer is full.
const LinkBuffer = 100 * time.Millisecond

// Link is a bottleneck with a capacity the test scripts (02 §17, integration 16). It comes in two kinds.
//
// A policer (NewLink) is a token bucket of LinkBuffer of its capacity with no queue: every datagram takes its size
// from the bucket, which fills at the capacity, and an RTP datagram that finds too little there is lost; the rest
// (STUN, DTLS, RTCP) always passes, so ICE and the reports go on as they would beside a congested media stream, but
// it takes its share of the capacity. It sits on the SFU's FaultConn in front of a client's address, for what the SFU
// sends that client (FaultConn.SetLink, with HarnessOptions.FaultyUDP: the client's downlink), or for what comes
// from it (FaultConn.SetInboundLink: its uplink).
//
// A queueing Link (NewQueueLink) is a FIFO delay line, like a home router's uplink buffer (M2 01 §10.1): each datagram
// departs at max(now, the previous departure) + its size × 8 / capacity, and an RTP datagram whose wait would exceed
// the Link's limit is lost (drop-tail in time); STUN, DTLS and RTCP queue like the rest but are never lost. It works
// only for what comes from a peer (FaultConn.SetInboundLink): a policer can't build a deep queue, so bufferbloat can't
// be tested on one.
//
// A Viewer whose ViewerOptions.REMB is a policer Link sends REMBs from rembsim's model of it: what the SFU forwarded
// and what of it passed, which is what Chrome's estimator measures on a real link.
type Link struct {
	mu       sync.Mutex
	capacity int64 // bit/s; 0: no limit
	// The policer's bucket: the bits in it and when it was last filled.
	tokens int64
	at     time.Time
	// The queueing Link's delay line: queue marks the kind, limit is the longest wait an RTP datagram may have, and
	// depart the departure of the last datagram queued.
	queue  bool
	limit  time.Duration
	depart time.Time
	// The RTP datagrams: the bytes sent through the Link and the bytes that passed, and how many were lost.
	sent, passed uint64
	lost         int64
}

// NewLink returns a policer Link of capacity bit/s, 0 for none.
func NewLink(capacity int64) *Link {
	return &Link{capacity: capacity}
}

// NewQueueLink returns a queueing Link of capacity bit/s (0 for none) that loses an RTP datagram whose wait would
// exceed limit.
func NewQueueLink(capacity int64, limit time.Duration) *Link {
	return &Link{capacity: capacity, queue: true, limit: limit}
}

// SetCapacity changes the capacity (bit/s, 0 for none) from now on. A queueing Link's datagrams that are queued
// already keep their departure times.
func (l *Link) SetCapacity(capacity int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queue {
		l.capacity = capacity
		return
	}
	l.fill(time.Now())
	l.capacity = capacity
	l.tokens = min(l.tokens, l.depth())
}

// Capacity returns the capacity in bit/s, 0 for none.
func (l *Link) Capacity() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.capacity
}

// Counts returns the bytes of the RTP datagrams sent through the Link so far, the bytes of those that passed, and
// the number of RTP datagrams lost.
func (l *Link) Counts() (sent, passed uint64, lost int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent, l.passed, l.lost
}

// Queued returns a queueing Link's current wait: how long a datagram that came now would wait before it starts to
// leave, the queue's depth in time. It is 0 for a policer, and for a queueing Link whose queue is empty.
func (l *Link) Queued() time.Duration {
	return l.queuedAt(time.Now())
}

// queuedAt is Queued at a given time.
func (l *Link) queuedAt(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.queue || !l.depart.After(now) {
		return 0
	}
	return l.depart.Sub(now)
}

// depth is the buffer's size in bits: LinkBuffer of the capacity, at least one datagram. l.mu is held.
func (l *Link) depth() int64 {
	return max(l.capacity*int64(LinkBuffer)/int64(time.Second), 1500*8)
}

// fill adds what the capacity brought since the last fill. A time before the last fill changes nothing. l.mu is held.
func (l *Link) fill(now time.Time) {
	switch {
	case l.at.IsZero():
	case now.After(l.at):
		elapsed := min(now.Sub(l.at), time.Second) // the buffer is full long before
		l.tokens = min(l.tokens+int64(elapsed)*l.capacity/int64(time.Second), l.depth())
	default:
		return
	}
	l.at = now
}

// pass reports whether a datagram of n bytes gets through a policer now; rtp says it is RTP (or SRTP), which the
// Link may lose.
func (l *Link) pass(n int, rtp bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rtp {
		l.sent += uint64(n)
	}
	if l.capacity <= 0 {
		l.at = time.Time{}
		if rtp {
			l.passed += uint64(n)
		}
		return true
	}
	if l.at.IsZero() {
		l.tokens = l.depth()
	}
	l.fill(now)
	bits := int64(n) * 8
	switch {
	case !rtp:
		l.tokens -= bits // it passes, and takes its share
		return true
	case l.tokens < bits:
		l.lost++
		return false
	}
	l.tokens -= bits
	l.passed += uint64(n)
	return true
}

// enqueue puts a datagram of n bytes that came at now into a queueing Link's delay line and returns when it departs;
// ok is false for an RTP datagram (rtp) whose wait would exceed the limit, which is lost and takes no place in the
// line. Without a capacity every datagram departs at once.
func (l *Link) enqueue(n int, rtp bool, now time.Time) (due time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rtp {
		l.sent += uint64(n)
	}
	if l.capacity <= 0 {
		if rtp {
			l.passed += uint64(n)
		}
		return now, true
	}
	start := now
	if l.depart.After(now) {
		start = l.depart
	}
	due = start.Add(time.Duration(int64(n) * 8 * int64(time.Second) / l.capacity))
	if rtp && due.Sub(now) > l.limit {
		l.lost++
		return time.Time{}, false
	}
	l.depart = due
	if rtp {
		l.passed += uint64(n)
	}
	return due, true
}
