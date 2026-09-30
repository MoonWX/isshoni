package logx

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
)

// Pion bridge limits (04 §10).
const (
	pionLinesPerWindow = 20
	pionWindow         = time.Minute
)

// NewPionLoggerFactory returns a Pion logger factory that writes to l, for webrtc.SettingEngine.LoggerFactory.
//
//   - Levels: trace and debug become slog debug; info, warn and error keep their level.
//   - Pion's default is warn: its trace, debug and info lines are logged only while l is enabled for debug (config
//     log.level = "debug", or the admin socket's log-level). Pion is chatty below warn.
//   - Each scope (ice, dtls, pc, …) may log at most 20 lines per minute. The rest are dropped and counted, and the
//     count is logged as "N messages suppressed" (at warn) with the scope's next line after the minute.
//   - A line that looks like SDP or an ICE candidate (an "a=" attribute or "candidate:") is dropped, so no session
//     description, ICE credential or address reaches the log.
//
// Lines carry the attribute scope. Share one factory between all SettingEngines so the limit holds per scope for the
// whole process; set component on l (for example component=sfu).
func NewPionLoggerFactory(l *slog.Logger) logging.LoggerFactory {
	return &pionFactory{l: l, limiters: map[string]*lineLimiter{}}
}

type pionFactory struct {
	l *slog.Logger

	mu       sync.Mutex
	limiters map[string]*lineLimiter // by scope; Pion has a fixed, small set of scopes
}

func (f *pionFactory) NewLogger(scope string) logging.LeveledLogger {
	f.mu.Lock()
	lim, ok := f.limiters[scope]
	if !ok {
		lim = newLineLimiter(pionLinesPerWindow, pionWindow)
		f.limiters[scope] = lim
	}
	f.mu.Unlock()
	return &pionLogger{l: f.l.With("scope", scope), lim: lim}
}

type pionLogger struct {
	l   *slog.Logger
	lim *lineLimiter
}

func (p *pionLogger) Trace(msg string)                  { p.log(slog.LevelDebug, msg) }
func (p *pionLogger) Tracef(format string, args ...any) { p.logf(slog.LevelDebug, format, args...) }
func (p *pionLogger) Debug(msg string)                  { p.log(slog.LevelDebug, msg) }
func (p *pionLogger) Debugf(format string, args ...any) { p.logf(slog.LevelDebug, format, args...) }
func (p *pionLogger) Info(msg string)                   { p.log(slog.LevelInfo, msg) }
func (p *pionLogger) Infof(format string, args ...any)  { p.logf(slog.LevelInfo, format, args...) }
func (p *pionLogger) Warn(msg string)                   { p.log(slog.LevelWarn, msg) }
func (p *pionLogger) Warnf(format string, args ...any)  { p.logf(slog.LevelWarn, format, args...) }
func (p *pionLogger) Error(msg string)                  { p.log(slog.LevelError, msg) }
func (p *pionLogger) Errorf(format string, args ...any) { p.logf(slog.LevelError, format, args...) }

// enabled applies Pion's warn default on top of the logger's own level.
func (p *pionLogger) enabled(level slog.Level) bool {
	if level < slog.LevelWarn && !p.l.Enabled(noCtx, slog.LevelDebug) {
		return false
	}
	return p.l.Enabled(noCtx, level)
}

func (p *pionLogger) logf(level slog.Level, format string, args ...any) {
	if !p.enabled(level) {
		return // don't format lines nobody sees
	}
	p.write(level, fmt.Sprintf(format, args...))
}

func (p *pionLogger) log(level slog.Level, msg string) {
	if !p.enabled(level) {
		return
	}
	p.write(level, msg)
}

func (p *pionLogger) write(level slog.Level, msg string) {
	if looksLikeSDP(msg) {
		return
	}
	ok, suppressed := p.lim.allow(time.Now())
	if suppressed > 0 {
		p.l.Log(noCtx, slog.LevelWarn, fmt.Sprintf("%d messages suppressed", suppressed), "suppressed", suppressed)
	}
	if ok {
		p.l.Log(noCtx, level, msg)
	}
}

// sdpAttrRE matches an SDP attribute line ("a=…") at the start of a line or after whitespace.
var sdpAttrRE = regexp.MustCompile(`(?:^|\s)a=`)

// looksLikeSDP reports whether msg contains SDP attribute lines or an ICE candidate.
func looksLikeSDP(msg string) bool {
	return strings.Contains(msg, "candidate:") || sdpAttrRE.MatchString(msg)
}
