package logx

import (
	"log/slog"
	"sync"
	"time"
)

// lineLimiter lets at most limit lines through per window and counts the rest. It uses fixed windows that start with
// the first line after the previous window ended. It says when a window drops its first line, and again when it
// drops a line of a higher level than every line it dropped before, so that the notice has the level of the lines
// it stands for. It reports the count of a window's suppressed lines once, with the first line of a later window,
// at the highest level among them. It starts no goroutines and no timers.
type lineLimiter struct {
	limit  int
	window time.Duration

	mu         sync.Mutex
	start      time.Time  // start of the current window; zero before the first line
	n          int        // lines let through in the current window
	suppressed int        // lines refused in the current window
	top        slog.Level // the highest level of the lines refused in the current window, while suppressed > 0
}

func newLineLimiter(limit int, window time.Duration) *lineLimiter {
	return &lineLimiter{limit: limit, window: window}
}

// verdict is lineLimiter.allow's answer for one line.
type verdict struct {
	ok bool // the line may be logged

	// reported is the number of lines an earlier window suppressed, > 0 only on the first call of a new window
	// (whether or not that call is ok). The caller logs it once, at reportedLevel: the highest level among them.
	reported      int
	reportedLevel slog.Level

	// quietUntil is the end of the current window when this call is the window's first refused line, or a refused
	// line of a higher level than every one refused before it in the window, and zero otherwise. The caller says
	// then, at this line's level, that further lines are dropped until quietUntil.
	quietUntil time.Time
}

// allow decides whether a line of the given level may be logged at now.
func (l *lineLimiter) allow(now time.Time, level slog.Level) verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	var v verdict
	if l.start.IsZero() || now.Sub(l.start) >= l.window {
		if l.suppressed > 0 {
			v.reported, v.reportedLevel = l.suppressed, l.top
		}
		l.start, l.n, l.suppressed = now, 0, 0
	}
	if l.n < l.limit {
		l.n++
		v.ok = true
		return v
	}
	l.suppressed++
	if l.suppressed == 1 || level > l.top {
		l.top = level
		v.quietUntil = l.start.Add(l.window)
	}
	return v
}
