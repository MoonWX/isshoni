package logx

import (
	"sync"
	"time"
)

// lineLimiter lets at most limit lines through per window and counts the rest. It uses fixed windows that start with
// the first line after the previous window ended, and it reports the count of a window's suppressed lines once, with
// the first line of a later window. It starts no goroutines and no timers.
type lineLimiter struct {
	limit  int
	window time.Duration

	mu         sync.Mutex
	start      time.Time // start of the current window; zero before the first line
	n          int       // lines let through in the current window
	suppressed int       // lines refused in the current window
}

func newLineLimiter(limit int, window time.Duration) *lineLimiter {
	return &lineLimiter{limit: limit, window: window}
}

// allow reports whether a line may be logged at now, and how many lines were suppressed since the last report (> 0
// only on the first call of a new window, whether or not that call is allowed).
func (l *lineLimiter) allow(now time.Time) (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() || now.Sub(l.start) >= l.window {
		suppressed, l.suppressed = l.suppressed, 0
		l.start, l.n = now, 0
	}
	if l.n < l.limit {
		l.n++
		return true, suppressed
	}
	l.suppressed++
	return false, suppressed
}
