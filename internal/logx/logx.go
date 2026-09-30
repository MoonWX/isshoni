// Package logx builds the process logger (log/slog) and keeps secrets out of it (04 §10).
//
//   - New writes text on a terminal and JSON lines otherwise (systemd's journal and Docker both get JSON).
//   - Secret and SecretBytes redact themselves in logs, fmt output and JSON.
//   - A safety net replaces the values of attributes with sensitive keys (password, token, sdp, …).
//   - NewPionLoggerFactory and NewZapBridge route Pion's and certmagic's logs into the same logger, with Pion's
//     rate-limited and stripped of SDP.
//
// Attribute names used across the server: component (tls, netx, push, sfu, signal, store, …), user_id, room_id,
// share_id, conn_id, remote_ip (security events only), route and err (see Err). Usernames, share labels, SDP, ICE
// candidates, tokens, passwords, cookies and full push endpoints (only push_host) are never logged.
package logx

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
)

// Log formats for Options.Format (config key log.format), besides "auto".
const (
	formatText = "text"
	formatJSON = "json"
)

// Options configures New.
type Options struct {
	// Level is the minimum level; the admin socket changes it at runtime. nil means a fixed info level.
	Level *slog.LevelVar
	// Format is "auto" (text when Out is a terminal, otherwise JSON), "text" or "json". "" and unknown values mean
	// "auto"; config validation rejects unknown values before they get here.
	Format string
	// Out is where log lines go. nil means os.Stderr.
	Out io.Writer
}

// New returns a logger with the level, format and redaction rules of 04 §10.
func New(opts Options) *slog.Logger {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	var level slog.Leveler = slog.LevelInfo
	if opts.Level != nil {
		level = opts.Level
	}
	ho := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	if useJSON(opts.Format, out) {
		return slog.New(slog.NewJSONHandler(out, ho))
	}
	return slog.New(slog.NewTextHandler(out, ho))
}

// Err returns the attribute for an error: slog.Any("err", err).
func Err(err error) slog.Attr { return slog.Any("err", err) }

func useJSON(format string, out io.Writer) bool {
	switch format {
	case formatJSON:
		return true
	case formatText:
		return false
	default: // "auto", "" and unknown values
		return !isTerminal(out)
	}
}

// isTerminal reports whether w is a terminal: a file (anything with Stat, such as *os.File) on a character device.
// That also counts /dev/null, where the format doesn't matter.
func isTerminal(w io.Writer) bool {
	s, ok := w.(interface{ Stat() (fs.FileInfo, error) })
	if !ok {
		return false
	}
	fi, err := s.Stat()
	return err == nil && fi.Mode()&fs.ModeCharDevice != 0
}

// sensitiveKeys are attribute keys whose values are always replaced, whatever their type (04 §10). Keys ending in
// "_token" are sensitive too. Matching ignores case.
var sensitiveKeys = []string{
	"password", "token", "secret", "authorization", "cookie",
	"sdp", "offer", "answer", "candidate", "endpoint",
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
	return slices.Contains(sensitiveKeys, key) || strings.HasSuffix(key, "_token")
}

// redactAttr is the handlers' ReplaceAttr safety net: an attribute with a sensitive key, or inside a group with a
// sensitive name (slog.Group("offer", …), Logger.WithGroup("cookie")), is logged as "[redacted]".
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	if sensitiveKey(a.Key) || slices.ContainsFunc(groups, sensitiveKey) {
		return slog.String(a.Key, redacted)
	}
	return a
}

// noCtx is the context of log calls made by the Pion and zap bridges, whose callers pass none.
var noCtx = context.Background()
