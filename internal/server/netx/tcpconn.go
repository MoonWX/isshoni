package netx

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxICEConnsPerIP is the number of ICE-TCP connections one IPKey may hold open across 443 and 7882/tcp
// (04 §7.8).
const DefaultMaxICEConnsPerIP = 64

// connLimiter counts open connections per IPKey. One limiter holds the ICE-TCP count for both 443 (PortMux) and
// 7882/tcp (Transport), so a client can't get 64 on each (04 §7.3).
type connLimiter struct {
	max int

	// refused counts the connections closed because their IPKey was at the limit or could not be keyed. Transport's
	// 7882/tcp listener adds to it, and PortMux.Stats reports it in Limited (04 §7.3).
	refused atomic.Uint64

	mu     sync.Mutex
	counts map[netip.Prefix]int
}

func newConnLimiter(maxPerKey int) *connLimiter {
	if maxPerKey <= 0 {
		maxPerKey = DefaultMaxICEConnsPerIP
	}
	return &connLimiter{max: maxPerKey, counts: map[netip.Prefix]int{}}
}

// acquire takes one slot for key and reports whether it was free.
func (l *connLimiter) acquire(key netip.Prefix) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= l.max {
		return false
	}
	l.counts[key]++
	return true
}

// release gives back one slot of key.
func (l *connLimiter) release(key netip.Prefix) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := l.counts[key]; n > 1 {
		l.counts[key] = n - 1
	} else {
		delete(l.counts, key)
	}
}

// open returns the number of connections key holds (for tests).
func (l *connLimiter) open(key netip.Prefix) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[key]
}

// remoteKey returns the IPKey of a connection's remote address, or false when it has none.
func remoteKey(c net.Conn) (netip.Prefix, bool) {
	var ap netip.AddrPort
	switch a := c.RemoteAddr().(type) {
	case *net.TCPAddr:
		ap = a.AddrPort()
	default:
		p, err := netip.ParseAddrPort(a.String())
		if err != nil {
			return netip.Prefix{}, false
		}
		ap = p
	}
	key := IPKey(ap.Addr())
	return key, key.IsValid()
}

// Accept retry after a transient error (EMFILE and the like): as net/http does, from 5 ms doubling up to 1 s.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// iceListener wraps an ICE-TCP listener for ice.TCPMuxDefault. On 7882/tcp it allows at most limiter.max open
// connections per IPKey (the excess is closed at once and counted in limiter.refused), and every accepted connection
// counts its bytes on PathMediaTCP. With a nil limiter and a nil counter (PortMux's 443 ICE sub-listener, which PortMux
// already limits and counts) it only tracks the connections for Close.
//
// Accept never returns a transient error: pion's TCPMuxDefault stops accepting for good on the first error, so
// transient errors are retried here and only a closed listener ends Accept.
//
// Close also closes every connection it handed out that is still open. pion's TCPMuxDefault.Close closes the
// listener and then waits for its per-connection goroutines, and one that still waits for a client's first STUN
// request would otherwise hold shutdown for up to FirstStunBindTimeout (10 s).
type iceListener struct {
	net.Listener
	limiter *connLimiter // nil: no limit
	counter *TransferCounter
	log     *slog.Logger

	mu        sync.Mutex
	live      map[*countingConn]struct{} // handed out and not closed yet; nil once the listener is closed
	closeOnce sync.Once
	closed    chan struct{}
}

func newICEListener(ln net.Listener, limiter *connLimiter, counter *TransferCounter, log *slog.Logger) *iceListener {
	return &iceListener{
		Listener: ln, limiter: limiter, counter: counter, log: log,
		live: map[*countingConn]struct{}{}, closed: make(chan struct{}),
	}
}

// track wraps an admitted connection; it returns nil (and closes c) when the listener was closed meanwhile.
func (l *iceListener) track(c net.Conn, key netip.Prefix) net.Conn {
	var cc *countingConn
	cc = newCountingConn(c, l.counter, PathMediaTCP, func() {
		if l.limiter != nil {
			l.limiter.release(key)
		}
		l.mu.Lock()
		delete(l.live, cc)
		l.mu.Unlock()
	})
	l.mu.Lock()
	open := l.live != nil
	if open {
		l.live[cc] = struct{}{}
	}
	l.mu.Unlock()
	if !open {
		_ = cc.Close()
		return nil
	}
	return cc
}

func (l *iceListener) Accept() (net.Conn, error) {
	var delay time.Duration
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			select {
			case <-l.closed:
				return nil, net.ErrClosed
			default:
			}
			delay = min(max(2*delay, acceptRetryMin), acceptRetryMax)
			l.log.Warn("ICE-TCP accept failed, retrying", "err", err, "retry_in", delay)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-l.closed:
				t.Stop()
				return nil, net.ErrClosed
			}
			continue
		}
		delay = 0
		var key netip.Prefix
		if l.limiter != nil {
			k, ok := remoteKey(c)
			if !ok || !l.limiter.acquire(k) {
				l.limiter.refused.Add(1)
				_ = c.Close()
				l.log.Debug("ICE-TCP connection over the per-IP limit closed", "max", l.limiter.max)
				continue
			}
			key = k
		}
		if cc := l.track(c, key); cc != nil {
			return cc, nil
		}
		return nil, net.ErrClosed
	}
}

// handedOut returns the number of connections handed out and not closed yet (for tests).
func (l *iceListener) handedOut() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.live)
}

// Close closes the listener and every connection still open; later calls do nothing and return nil (pion's
// TCPMuxDefault.Close closes it, and a failed NewTransport may close it before a mux exists).
func (l *iceListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.Listener.Close()
		l.mu.Lock()
		live := l.live
		l.live = nil
		l.mu.Unlock()
		for c := range live {
			_ = c.Close()
		}
	})
	return err
}
