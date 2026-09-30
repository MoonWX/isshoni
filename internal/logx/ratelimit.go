package logx

import (
	"sync"
	"time"
)

// lineLimiter lets at most limit lines through per window and counts the rest. It uses fixed windows that start with
// the first line after the previous window ended. It says when a window drops its first line, and it reports the count
// of a window's suppressed lines once, with the first line of a later window. It starts no goroutines and no timers.
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

// verdict is lineLimiter.allow's answer for one line.
type verdict struct {
	ok bool // the line may be logged

	// reported is the number of lines an earlier window suppressed, > 0 only on the first call of a new window
	// (whether or not that call is ok). The caller logs it once.
	reported int

	// quietUntil is the end of the current window when this call is the window's first refused line, and zero
	// otherwise. The caller says once that further lines are dropped until then.
	quietUntil time.Time
}

// allow decides whether a line may be logged at now.
func (l *lineLimiter) allow(now time.Time) verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	var v verdict
	if l.start.IsZero() || now.Sub(l.start) >= l.window {
		v.reported, l.suppressed = l.suppressed, 0
		l.start, l.n = now, 0
	}
	if l.n < l.limit {
		l.n++
		v.ok = true
		return v
	}
	l.suppressed++
	if l.suppressed == 1 {
		v.quietUntil = l.start.Add(l.window)
	}
	return v
}
