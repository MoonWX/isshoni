package ops

import (
	"log/slog"
	"sync"
	"time"
)

// LogLevelNames are the log levels of log.level and of `isshoni admin log-level`, least severe first (04 §4.3).
var LogLevelNames = []string{"debug", "info", "warn", "error"}

// DefaultLogLevelFor is how long a level set through the admin socket lasts when the request names no duration
// (04 §3.1: --for 30m).
const DefaultLogLevelFor = 30 * time.Minute

// ParseLogLevel returns the slog level of one of LogLevelNames; ok is false for anything else.
func ParseLogLevel(name string) (level slog.Level, ok bool) {
	switch name {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// LogLevel changes the process's log level at runtime (04 §10). The level of the config (log.level) is the base;
// the admin socket's POST /v1/log-level puts another level on top of it for a while (Override), after which the
// base is back by itself. All methods are safe for concurrent use.
//
// The slog.LevelVar is the one the process logger was built with (logx.Options.Level): cmd/isshoni creates it, and
// the wiring hands it to NewLogLevel.
type LogLevel struct {
	v   *slog.LevelVar
	log *slog.Logger

	mu     sync.Mutex
	base   slog.Level
	timer  *time.Timer // non-nil while an override is in effect
	until  time.Time   // when it ends
	gen    uint64      // counts the overrides, so that a timer that fires late ends only its own
	closed bool
}

// NewLogLevel returns a LogLevel for v; v's current level is the base. log gets a line when an override ends; nil
// means slog.Default().
func NewLogLevel(v *slog.LevelVar, log *slog.Logger) *LogLevel {
	if v == nil {
		panic("ops: NewLogLevel: nil LevelVar")
	}
	if log == nil {
		log = slog.Default()
	}
	return &LogLevel{v: v, log: log.With(slog.String("component", "admin")), base: v.Level()}
}

// Override sets the level to level for d, then back to the base. An override that is still in effect is replaced,
// and its time with it. A d of zero or less only ends the override that is in effect.
func (l *LogLevel) Override(level slog.Level, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.stopTimerLocked()
	if d <= 0 {
		l.v.Set(l.base)
		return
	}
	l.v.Set(level)
	l.until = time.Now().Add(d)
	gen := l.gen
	l.timer = time.AfterFunc(d, func() { l.expire(gen) })
}

// expire ends override number gen, unless it was replaced or ended since its timer fired.
func (l *LogLevel) expire(gen uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen || l.timer == nil || l.closed {
		return
	}
	l.timer, l.until = nil, time.Time{}
	// Before the switch: with a base of warn or error the line would not be written after it.
	l.log.Info("the log level set through the admin socket has run out; back to the configured level",
		slog.String("level", l.base.String()))
	l.v.Set(l.base)
}

// SetBase changes the base level: the level of the config after a reload (SIGHUP, 04 §6.5). It applies at once
// unless an override is in effect; then it is the level the override falls back to.
func (l *LogLevel) SetBase(level slog.Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base = level
	if l.timer == nil && !l.closed {
		l.v.Set(level)
	}
}

// Level returns the level in effect and, while an override is, when it ends (the zero time otherwise).
func (l *LogLevel) Level() (level slog.Level, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.v.Level(), l.until
}

// Close ends an override that is in effect and stops its timer. The LogLevel does nothing after that.
func (l *LogLevel) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.stopTimerLocked()
	l.v.Set(l.base)
}

// stopTimerLocked ends the override in effect, if any, without touching the level, and starts a new generation.
func (l *LogLevel) stopTimerLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer, l.until = nil, time.Time{}
	}
	l.gen++
}
