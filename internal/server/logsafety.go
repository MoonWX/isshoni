package server

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// The server's last safety net for its log (README S90; 04 §10). logx replaces the attributes whose names say they
// hold a secret, and its Pion bridge drops the lines that hold SDP or a candidate; what neither can see is a
// credential inside the text of a library's message. The log canary (TestLogCanary) found one: Pion's ICE agent
// writes the remote ICE password at debug level when it starts ("Started agent: isControlling? false, remoteUfrag:
// \"…\", remotePwd: \"…\"", pion/ice v4.4.7 agent.go), through the SFU's and netx's Pion loggers. The password is
// SDP's a=ice-pwd, which 04 §10 says is never logged, and it authenticates the PeerConnection's connectivity
// checks.
//
// New wraps the logger it is given in safeHandler, so that every part the server builds logs through it.

// icePassword matches an ICE password in a message: the name (remotePwd, localPwd, ice-pwd, pwd, in any case) and
// its value, quoted or not.
var icePassword = regexp.MustCompile(`(?i)\b((?:remote|local|ice-?)?pwd)(\\?"?\s*[:=]\s*)(\\?"[^"\\]*\\?"|[^\s,;)]+)`)

// redactMessage replaces the values of the ICE passwords in a message by [redacted].
func redactMessage(msg string) string {
	if !strings.Contains(strings.ToLower(msg), "pwd") {
		return msg
	}
	return icePassword.ReplaceAllString(msg, `${1}${2}"[redacted]"`)
}

// safeHandler redacts the message of every record (redactMessage) before the handler under it writes it.
type safeHandler struct{ slog.Handler }

// newSafeLogger returns log with safeHandler under it, or log itself when it has one already.
func newSafeLogger(log *slog.Logger) *slog.Logger {
	if _, ok := log.Handler().(*safeHandler); ok {
		return log
	}
	return slog.New(&safeHandler{log.Handler()})
}

func (h *safeHandler) Handle(ctx context.Context, r slog.Record) error {
	if msg := redactMessage(r.Message); msg != r.Message {
		safe := slog.NewRecord(r.Time, r.Level, msg, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			safe.AddAttrs(a)
			return true
		})
		r = safe
	}
	return h.Handler.Handle(ctx, r)
}

func (h *safeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &safeHandler{h.Handler.WithAttrs(attrs)}
}

func (h *safeHandler) WithGroup(name string) slog.Handler {
	return &safeHandler{h.Handler.WithGroup(name)}
}
