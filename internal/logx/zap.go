package logx

import (
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// NewZapBridge returns a zap logger that writes to l, for certmagic (04 §8.2). Set component on l (component=tls).
//
// Levels map to slog (debug, info, warn; error and above become error). Fields become attributes in their order; an
// error field keeps zap's key unless it is the default "error", which becomes "err". A named logger adds the
// attribute logger. The safety net of New applies to the fields like to any attribute.
func NewZapBridge(l *slog.Logger) *zap.Logger {
	return zap.New(&zapCore{l: l})
}

// zapCore is a zapcore.Core that forwards entries to slog.
type zapCore struct {
	l *slog.Logger
}

func (c *zapCore) Enabled(level zapcore.Level) bool {
	return c.l.Enabled(noCtx, slogLevelOfZap(level))
}

func (c *zapCore) With(fields []zapcore.Field) zapcore.Core {
	if len(fields) == 0 {
		return c
	}
	attrs := zapAttrs(fields)
	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	return &zapCore{l: c.l.With(args...)}
}

func (c *zapCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *zapCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	r := slog.NewRecord(e.Time, slogLevelOfZap(e.Level), e.Message, 0)
	if e.LoggerName != "" {
		r.AddAttrs(slog.String("logger", e.LoggerName))
	}
	r.AddAttrs(zapAttrs(fields)...)
	return c.l.Handler().Handle(noCtx, r)
}

func (c *zapCore) Sync() error { return nil }

func slogLevelOfZap(level zapcore.Level) slog.Level {
	switch {
	case level <= zapcore.DebugLevel:
		return slog.LevelDebug
	case level == zapcore.InfoLevel:
		return slog.LevelInfo
	case level == zapcore.WarnLevel:
		return slog.LevelWarn
	default: // error, dpanic, panic, fatal
		return slog.LevelError
	}
}

// zapAttrs converts zap fields to slog attributes, in field order. zap's own encoder does the conversion, so every
// field type (objects, arrays, errors, namespaces) comes out as zap would encode it.
func zapAttrs(fields []zapcore.Field) []slog.Attr {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		v, ok := enc.Fields[f.Key]
		if !ok || seen[f.Key] {
			continue // a field inside a namespace, a skipped field, or a repeated key
		}
		seen[f.Key] = true
		key := f.Key
		if f.Type == zapcore.ErrorType && key == "error" {
			key = "err"
		}
		attrs = append(attrs, slog.Any(key, v))
	}
	return attrs
}
