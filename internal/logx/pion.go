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
//   - Each scope (ice, dtls, pc, …) may log at most 20 lines per minute. The first line over the limit logs
//     "further messages suppressed" (with suppressed_until, the end of the minute) at that line's level, and so
//     does a later line of the same minute whose level is higher than that of every line dropped before it: a
//     burst of debug lines says so at debug, and a warning that is dropped says so at warn. The dropped lines are
//     counted, and the count is logged as "N messages suppressed" at the highest level among them, with the
//     scope's next line after the minute.
//   - A line that looks like SDP or an ICE candidate (an "a=" attribute or "candidate:") is dropped, so no session
//     description, ICE credential or address reaches the log.
//   - An ICE password in the text of any other line is redacted before the line is written, at every level: a named
//     one (remotePwd: "…") and the password of a ufrag:password pair (redactCredentials). Pion's ICE agent logs the
//     remote password at debug when it starts its checks.
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
	v := p.lim.allow(time.Now(), level)
	if v.reported > 0 {
		p.l.Log(noCtx, v.reportedLevel, fmt.Sprintf("%d messages suppressed", v.reported), "suppressed", v.reported)
	}
	if v.ok {
		p.l.Log(noCtx, level, redactCredentials(msg))
	}
	if !v.quietUntil.IsZero() { // a burst followed by silence would otherwise never say that lines were dropped
		p.l.Log(noCtx, level, "further messages suppressed", "suppressed_until", v.quietUntil)
	}
}

// sdpAttrRE matches an SDP attribute line ("a=…") at the start of a line or after whitespace.
var sdpAttrRE = regexp.MustCompile(`(?:^|\s)a=`)

// looksLikeSDP reports whether msg contains SDP attribute lines or an ICE candidate.
func looksLikeSDP(msg string) bool {
	return strings.Contains(msg, "candidate:") || sdpAttrRE.MatchString(msg)
}

// ICE passwords in the text of Pion's lines (04 §10, README F20). Pion's ICE agent writes the remote password at
// debug when it starts its connectivity checks (pion/ice v4.4.7 agent.go):
//
//	Started agent: isControlling? false, remoteUfrag: "NebKSTlgEbivgZgz", remotePwd: "OJMptkvWKLDWnqdfKgPQQVQOHIsJxdcu"
//
// The password is SDP's a=ice-pwd: it authenticates the PeerConnection's connectivity checks, and 04 §10 says it is
// never logged. A ufrag is no secret (every connectivity check carries it in the clear), so it stays.

// namedPasswordRE matches a named password and its value: a name that ends in pwd, password, passwd or credential,
// in any case and after any letters, digits and _ (remotePwd, localPwd, icePwd, pwd and the pwd of ice-pwd, the
// Password field of a struct that %+v prints, a TURN server's Credential), then ":" or "=" with optional spaces
// around it (a JSON key's closing quote, escaped or not, may come first), then the value, group 1: quoted as %q
// writes it ("…" with Go's escapes), or in '…' or `…`, or else everything up to the next space, comma, semicolon,
// & or closing bracket.
var namedPasswordRE = regexp.MustCompile(`(?i)[a-z0-9_]*(?:pwd|passw(?:or)?d|credential)s?\\?"?\s*[:=]\s*` +
	`("(?:[^"\\]|\\.)*"|'[^']*'|` + "`[^`]*`" + `|[^\s,;&)\]}]+)`)

// The lengths that make a ufrag:password pair (RFC 8445 §5.3, RFC 8839 §5.4): a ufrag has at least 4 ICE
// characters, a password at least 22 (128 bits of randomness).
const (
	minUfragLen = 4
	minPwdLen   = 22
)

// redactCredentials returns msg with the ICE passwords in it replaced by [redacted]:
//
//   - a named password (namedPasswordRE): its value becomes [redacted], in the quotes it had, so remotePwd: "…"
//     becomes remotePwd: "[redacted]";
//   - a ufrag:password pair: in a run of ICE characters (letters, digits, + and /) and colons, a part of at least 22
//     characters that follows a part of at least 4 becomes [redacted], so ufrag:password becomes ufrag:[redacted].
//     The parts of IP addresses with ports, fingerprints and times are shorter. A URL whose host has no dot and is
//     at least 20 characters long looks like a pair after its scheme and is redacted as well: a safety net errs on
//     that side.
//
// Pion's other lines come back as they are. The bridge redacts only the lines its limiter lets through.
func redactCredentials(msg string) string {
	return redactPairs(redactNamedPasswords(msg))
}

// redactNamedPasswords replaces the values that namedPasswordRE finds.
func redactNamedPasswords(msg string) string {
	found := namedPasswordRE.FindAllStringSubmatchIndex(msg, -1)
	if found == nil {
		return msg
	}
	var b strings.Builder
	last := 0
	for _, m := range found {
		start, end := m[2], m[3] // the value
		b.WriteString(msg[last:start])
		switch q := msg[start]; q {
		case '"', '\'', '`':
			b.WriteByte(q)
			b.WriteString(redacted)
			b.WriteByte(q)
		default:
			b.WriteString(redacted)
		}
		last = end
	}
	b.WriteString(msg[last:])
	return b.String()
}

// redactPairs replaces the password of every ufrag:password pair in msg (redactCredentials).
func redactPairs(msg string) string {
	if !strings.Contains(msg, ":") {
		return msg
	}
	var b strings.Builder
	last := 0 // msg[:last] is in b; a redacted part follows a colon, so last > 0 once one was redacted
	for i := 0; i < len(msg); {
		if !iceChar(msg[i]) && msg[i] != ':' {
			i++
			continue
		}
		// msg[i:j] is a run of ICE characters and colons, which separate its parts.
		j := i
		for j < len(msg) && (iceChar(msg[j]) || msg[j] == ':') {
			j++
		}
		partStart, prevLen := i, 0
		for k := i; k <= j; k++ {
			if k < j && msg[k] != ':' {
				continue
			}
			// msg[partStart:k] is a part; prevLen is the length of the part before it.
			if partStart > i && prevLen >= minUfragLen && k-partStart >= minPwdLen {
				b.WriteString(msg[last:partStart])
				b.WriteString(redacted)
				last = k
			}
			prevLen, partStart = k-partStart, k+1
		}
		i = j
	}
	if last == 0 {
		return msg
	}
	b.WriteString(msg[last:])
	return b.String()
}

// iceChar reports whether c is an ICE character (RFC 8445 §5.3: ALPHA / DIGIT / "+" / "/").
func iceChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '/'
}
