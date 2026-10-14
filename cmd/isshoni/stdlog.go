package main

import (
	"context"
	"fmt"
	stdlog "log"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The standard log package in serve (README S90). The server logs through log/slog only (04 §10), and so do the
// bridges of logx for Pion and certmagic's zap logger. A few lines of certmagic's bypass zap and go to the standard
// log package: a panic in one of its workers or challenge servers, an error of its challenge servers, an OCSP staple
// it can't delete. Without a route they would reach the journal as plain text among the JSON lines, at no level.

const (
	// stdLogComponent is the component of the lines that come through the standard log package. certmagic is the
	// one library known to write there; the bridge can't tell who wrote a line, so it does not name one.
	stdLogComponent = "stdlog"
	// stdLogLimit lines per stdLogWindow get through, so that a library that writes in a loop (an accept that fails
	// over and over) can't flood the log; the rest are counted and reported once.
	stdLogLimit  = 20
	stdLogWindow = time.Minute
)

// routeStdLog sends what the standard log package writes into l, one record per line, from now until the returned
// function puts the previous output, flags and prefix back. serve calls it once its logger is built.
func routeStdLog(l *slog.Logger) (restore func()) {
	out, flags, prefix := stdlog.Writer(), stdlog.Flags(), stdlog.Prefix()
	stdlog.SetOutput(newStdLogWriter(l))
	stdlog.SetFlags(0) // the record has its own time
	stdlog.SetPrefix("")
	return func() {
		stdlog.SetOutput(out)
		stdlog.SetFlags(flags)
		stdlog.SetPrefix(prefix)
	}
}

// stdLogWriter turns the standard log package's lines into records of a slog.Logger. The log package writes each
// message with one Write call. The level comes from the prefix that certmagic and many other libraries use:
// "[ERROR]" and a "panic:" line are errors, "[WARNING]" or "[WARN]" warnings, "[INFO]" and "[DEBUG]" info and debug;
// a line without one is a warning, because a library that writes there without a level is rarely chatty. Like logx's
// Pion bridge it drops a line that looks like SDP or holds an ICE candidate (04 §10), and lets stdLogLimit lines
// through per minute.
type stdLogWriter struct {
	log *slog.Logger
	now func() time.Time

	mu         sync.Mutex
	start      time.Time // of the current window
	n          int       // lines let through in it
	suppressed int       // lines dropped in it
}

func newStdLogWriter(l *slog.Logger) *stdLogWriter {
	return &stdLogWriter{log: l.With(slog.String("component", stdLogComponent)), now: time.Now}
}

// stdLogLevels are the prefixes that set a line's level, and the level.
var stdLogLevels = []struct {
	prefix string
	level  slog.Level
}{
	{"[ERROR] ", slog.LevelError},
	{"[WARNING] ", slog.LevelWarn},
	{"[WARN] ", slog.LevelWarn},
	{"[INFO] ", slog.LevelInfo},
	{"[DEBUG] ", slog.LevelDebug},
}

func (w *stdLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	level := slog.LevelWarn
	for _, l := range stdLogLevels {
		if rest, ok := strings.CutPrefix(msg, l.prefix); ok {
			msg, level = rest, l.level
			break
		}
	}
	if strings.HasPrefix(msg, "panic: ") {
		level = slog.LevelError
	}
	ctx := context.Background()
	if !w.log.Enabled(ctx, level) {
		return len(p), nil
	}
	if looksLikeSDP(msg) {
		w.log.LogAttrs(ctx, slog.LevelDebug, "a line of the standard log was dropped: it looks like SDP or an ICE candidate")
		return len(p), nil
	}
	ok, reported := w.allow()
	if reported > 0 {
		w.log.LogAttrs(ctx, slog.LevelWarn, fmt.Sprintf("%d lines of the standard log were suppressed", reported))
	}
	if ok {
		w.log.LogAttrs(ctx, level, msg)
	}
	return len(p), nil
}

// allow decides whether a line gets through, and returns the count of the lines that the window before dropped
// when this line opens a new window.
func (w *stdLogWriter) allow() (ok bool, reported int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.start.IsZero() || now.Sub(w.start) >= stdLogWindow {
		reported, w.suppressed = w.suppressed, 0
		w.start, w.n = now, 0
	}
	if w.n < stdLogLimit {
		w.n++
		return true, reported
	}
	w.suppressed++
	return false, reported
}

// looksLikeSDP reports whether a line holds an SDP attribute line or an ICE candidate (logx's Pion bridge drops
// the same).
func looksLikeSDP(msg string) bool {
	return strings.HasPrefix(msg, "a=") || strings.Contains(msg, "\na=") || strings.Contains(msg, "candidate:") ||
		strings.Contains(msg, "v=0\r\n") || strings.Contains(msg, "v=0\n")
}
