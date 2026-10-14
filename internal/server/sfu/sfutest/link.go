package sfutest

import (
	"sync"
	"time"
)

// LinkBuffer is how much of its capacity a Link holds: a burst up to that size passes, what comes faster than the
// capacity beyond it is lost, as at the bottleneck of a downlink whose buffer is full.
const LinkBuffer = 100 * time.Millisecond

// Link is the bottleneck of one client's downlink, with a capacity the test scripts (02 §17, integration 16). It sits
// on the SFU's FaultConn (FaultConn.SetLink, with HarnessOptions.FaultyUDP), in front of the client's address: every
// datagram the SFU sends that client takes its size from the Link's buffer, which fills at the capacity. An RTP
// datagram that finds too little there is lost; the rest (STUN, DTLS, RTCP) always passes, so ICE and the reports
// go on as they would beside a congested media stream, but it takes its share of the capacity.
//
// A Viewer whose ViewerOptions.REMB is the Link sends REMBs from rembsim's model of it: what the SFU forwarded and
// what of it passed, which is what Chrome's estimator measures on a real link.
type Link struct {
	mu       sync.Mutex
	capacity int64 // bit/s; 0: no limit
	tokens   int64 // bits in the buffer
	at       time.Time
	// The RTP datagrams: the bytes the SFU sent through the Link and the bytes that passed, and how many were lost.
	sent, passed uint64
	lost         int64
}

// NewLink returns a Link of capacity bit/s, 0 for none.
func NewLink(capacity int64) *Link {
	return &Link{capacity: capacity}
}

// SetCapacity changes the capacity (bit/s, 0 for none) from now on.
func (l *Link) SetCapacity(capacity int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

// Counts returns the bytes of the RTP datagrams the SFU sent through the Link so far, the bytes of those that passed,
// and the number of RTP datagrams lost.
func (l *Link) Counts() (sent, passed uint64, lost int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent, l.passed, l.lost
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

// pass reports whether a datagram of n bytes gets through now; rtp says it is RTP (or SRTP), which the Link may lose.
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
