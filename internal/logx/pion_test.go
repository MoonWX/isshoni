package logx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

func TestPionLevels(t *testing.T) {
	var buf bytes.Buffer
	pl := NewPionLoggerFactory(newJSON(&buf, slog.LevelDebug).With("component", "sfu")).NewLogger("ice")
	pl.Trace("trace line")
	pl.Tracef("trace %d", 1)
	pl.Debug("debug line")
	pl.Debugf("debug %d", 2)
	pl.Info("info line")
	pl.Infof("info %d", 3)
	pl.Warn("warn line")
	pl.Warnf("warn %d", 4)
	pl.Error("error line")
	pl.Errorf("error %d", 5)

	want := []struct{ level, msg string }{
		{"DEBUG", "trace line"}, {"DEBUG", "trace 1"},
		{"DEBUG", "debug line"}, {"DEBUG", "debug 2"},
		{"INFO", "info line"}, {"INFO", "info 3"},
		{"WARN", "warn line"}, {"WARN", "warn 4"},
		{"ERROR", "error line"}, {"ERROR", "error 5"},
	}
	got := jsonLines(t, &buf)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), buf.String())
	}
	for i, w := range want {
		if got[i]["level"] != w.level || got[i]["msg"] != w.msg || got[i]["scope"] != "ice" || got[i]["component"] != "sfu" {
			t.Errorf("line %d = %v, want level %s msg %q scope ice component sfu", i, got[i], w.level, w.msg)
		}
	}
}

// Pion's default is warn: below it, Pion logs only while the logger is at debug.
func TestPionDefaultsToWarn(t *testing.T) {
	for _, tt := range []struct {
		level slog.Level
		want  []string
	}{
		{slog.LevelDebug, []string{"trace", "debug", "info", "warn", "error"}},
		{slog.LevelInfo, []string{"warn", "error"}},
		{slog.LevelWarn, []string{"warn", "error"}},
		{slog.LevelError, []string{"error"}},
	} {
		var buf bytes.Buffer
		pl := NewPionLoggerFactory(newJSON(&buf, tt.level)).NewLogger("pc")
		pl.Tracef("%s", "trace")
		pl.Debug("debug")
		pl.Infof("%s", "info")
		pl.Warn("warn")
		pl.Errorf("%s", "error")
		var got []string
		for _, l := range jsonLines(t, &buf) {
			got = append(got, l["msg"].(string))
		}
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("logger at %v: pion lines %v, want %v", tt.level, got, tt.want)
		}
	}
}

func TestPionLevelFollowsLevelVar(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	pl := NewPionLoggerFactory(New(Options{Level: lv, Format: "json", Out: &buf})).NewLogger("dtls")
	pl.Info("hidden")
	lv.Set(slog.LevelDebug)
	pl.Info("shown")
	if got := jsonLines(t, &buf); len(got) != 1 || got[0]["msg"] != "shown" {
		t.Errorf("lines = %v, want only the line after switching to debug", got)
	}
}

func TestPionDropsSDP(t *testing.T) {
	dropped := []string{
		"v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-\r\na=group:BUNDLE 0 1\r\n",
		"a=ice-ufrag:abcd",
		"remote description: a=fingerprint:sha-256 AB:CD",
		"Adding remote candidate: candidate:1 1 udp 2130706431 192.0.2.10 50000 typ host",
		"candidate:842163049 1 udp 1677729535 203.0.113.7 3478 typ srflx",
		"unhandled line\na=ice-pwd:secret",
	}
	kept := []string{
		"ICE connection state changed: connected",
		"failed to handshake: context deadline exceeded",
		"metadata=1 area=2", // "a=" inside a word is not an SDP line
		"Failed to accept RTCP stream is already closed",
	}
	var buf bytes.Buffer
	pl := NewPionLoggerFactory(newJSON(&buf, slog.LevelDebug)).NewLogger("pc")
	for _, m := range dropped {
		pl.Warn(m)
		pl.Warnf("%s", m)
		pl.Debug(m)
	}
	if buf.Len() != 0 {
		t.Fatalf("SDP-like lines were logged:\n%s", buf.String())
	}
	for _, m := range kept {
		pl.Warn(m)
	}
	got := jsonLines(t, &buf)
	if len(got) != len(kept) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(kept), buf.String())
	}
	for i, m := range kept {
		if got[i]["msg"] != m {
			t.Errorf("line %d = %q, want %q", i, got[i]["msg"], m)
		}
	}
}

func TestPionRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		f := NewPionLoggerFactory(newJSON(&buf, slog.LevelInfo))
		ice := f.NewLogger("ice")
		ice2 := f.NewLogger("ice") // Pion makes a logger per PeerConnection: the limit is per scope
		dtls := f.NewLogger("dtls")
		windowEnd := time.Now().Add(pionWindow)

		for i := range 15 {
			ice.Warnf("ice %d", i)
			ice2.Warnf("ice2 %d", i)
		}
		dtls.Warn("dtls still logs")
		ice.Warn("a=candidate-like lines are dropped and not counted")

		// 20 ice lines, the notice right after them (the 21st line), then dtls.
		lines := jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow+2 {
			t.Fatalf("got %d lines in the first minute, want %d (20 ice + 1 notice + 1 dtls):\n%s", len(lines), pionLinesPerWindow+2, buf.String())
		}
		checkNotice(t, lines[pionLinesPerWindow], "WARN", windowEnd)
		if lines[pionLinesPerWindow+1]["msg"] != "dtls still logs" {
			t.Errorf("last line = %v, want the dtls line", lines[pionLinesPerWindow+1])
		}

		time.Sleep(pionWindow - time.Second)
		buf.Reset()
		ice.Warn("still suppressed")
		if buf.Len() != 0 {
			t.Fatalf("logged before the minute was over (the notice comes only once per minute):\n%s", buf.String())
		}

		time.Sleep(time.Second)
		ice.Warn("next minute")
		lines = jsonLines(t, &buf)
		if len(lines) != 2 {
			t.Fatalf("got %d lines after the minute, want the suppressed count and the line:\n%s", len(lines), buf.String())
		}
		// 30 ice lines, 20 logged, 10 suppressed, plus "still suppressed".
		if lines[0]["msg"] != "11 messages suppressed" || lines[0]["level"] != "WARN" || lines[0]["scope"] != "ice" || lines[0]["suppressed"] != 11.0 {
			t.Errorf("suppressed line = %v", lines[0])
		}
		if lines[1]["msg"] != "next minute" {
			t.Errorf("line = %v, want the new line", lines[1])
		}

		// The second minute started with "next minute": 19 more fit, the next two are dropped with one notice.
		buf.Reset()
		windowEnd = time.Now().Add(pionWindow)
		for i := range pionLinesPerWindow - 1 {
			ice.Warn(fmt.Sprint("more ", i))
		}
		ice.Warn("over the limit again")
		ice2.Warn("and again")
		lines = jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow {
			t.Fatalf("second minute: %d lines, want %d (19 + 1 notice):\n%s", len(lines), pionLinesPerWindow, buf.String())
		}
		checkNotice(t, lines[pionLinesPerWindow-1], "WARN", windowEnd)
	})
}

// checkNotice checks the line a scope logs when it goes over its limit: "further messages suppressed" for scope ice,
// at level, until the end of the minute.
func checkNotice(t *testing.T, line map[string]any, level string, windowEnd time.Time) {
	t.Helper()
	if line["msg"] != "further messages suppressed" || line["level"] != level || line["scope"] != "ice" {
		t.Errorf("notice = %v, want a %s line \"further messages suppressed\" for scope ice", line, level)
	}
	until, err := time.Parse(time.RFC3339Nano, fmt.Sprint(line["suppressed_until"]))
	if err != nil || !until.Equal(windowEnd) {
		t.Errorf("notice suppressed_until = %v, want %v (the end of the minute)", line["suppressed_until"], windowEnd)
	}
}

// TestPionRateLimitLevels: the limiter's own lines have the level of the lines they stand for (README F20). A burst
// of Pion's debug lines says at debug that lines are dropped, never at warn; a dropped warning says so at warn; the
// count has the highest level among the dropped lines.
func TestPionRateLimitLevels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		ice := NewPionLoggerFactory(newJSON(&buf, slog.LevelDebug)).NewLogger("ice")
		windowEnd := time.Now().Add(pionWindow)

		for i := range pionLinesPerWindow + 5 {
			ice.Debugf("debug %d", i)
		}
		lines := jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow+1 {
			t.Fatalf("got %d lines, want %d (20 debug lines + 1 notice):\n%s", len(lines), pionLinesPerWindow+1, buf.String())
		}
		for i, line := range lines {
			if line["level"] != "DEBUG" {
				t.Errorf("line %d = %v, want a debug line: only debug lines were dropped", i, line)
			}
		}
		checkNotice(t, lines[pionLinesPerWindow], "DEBUG", windowEnd)

		// A warning dropped in the same minute says so at warn, once; lines of lower levels after it say nothing.
		buf.Reset()
		ice.Warn("a dropped warning")
		ice.Warnf("%s", "another dropped warning")
		ice.Info("a dropped info line")
		ice.Debug("a dropped debug line")
		lines = jsonLines(t, &buf)
		if len(lines) != 1 {
			t.Fatalf("got %d lines for the dropped warnings, want 1 notice:\n%s", len(lines), buf.String())
		}
		checkNotice(t, lines[0], "WARN", windowEnd)

		// The next minute's first line brings the count (5 + 2 + 1 + 1), at warn: warnings were among them.
		time.Sleep(pionWindow)
		buf.Reset()
		ice.Debug("next minute")
		lines = jsonLines(t, &buf)
		if len(lines) != 2 || lines[0]["msg"] != "9 messages suppressed" || lines[0]["level"] != "WARN" ||
			lines[0]["suppressed"] != 9.0 || lines[1]["msg"] != "next minute" || lines[1]["level"] != "DEBUG" {
			t.Fatalf("after the first minute, want the count at warn and the line at debug:\n%s", buf.String())
		}

		// A minute that drops only debug lines reports them at debug.
		windowEnd = time.Now().Add(pionWindow)
		buf.Reset()
		for i := range pionLinesPerWindow { // 19 fit after "next minute"
			ice.Debugf("more %d", i)
		}
		lines = jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow {
			t.Fatalf("second minute: %d lines, want %d (19 + 1 notice):\n%s", len(lines), pionLinesPerWindow, buf.String())
		}
		checkNotice(t, lines[pionLinesPerWindow-1], "DEBUG", windowEnd)
		time.Sleep(pionWindow)
		buf.Reset()
		ice.Warn("third minute")
		lines = jsonLines(t, &buf)
		if len(lines) != 2 || lines[0]["msg"] != "1 messages suppressed" || lines[0]["level"] != "DEBUG" ||
			lines[1]["msg"] != "third minute" || lines[1]["level"] != "WARN" {
			t.Fatalf("after the second minute, want the count at debug and the line at warn:\n%s", buf.String())
		}

		// At info, Pion's debug lines are not logged, so they use up nothing and drop nothing.
		buf.Reset()
		dtls := NewPionLoggerFactory(newJSON(&buf, slog.LevelInfo)).NewLogger("dtls")
		for i := range 3 * pionLinesPerWindow {
			dtls.Debugf("hidden %d", i)
		}
		for i := range pionLinesPerWindow {
			dtls.Warnf("warning %d", i)
		}
		lines = jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow {
			t.Fatalf("at info: %d lines, want the %d warnings and nothing else:\n%s", len(lines), pionLinesPerWindow, buf.String())
		}
		for _, line := range lines {
			if line["level"] != "WARN" || !strings.HasPrefix(line["msg"].(string), "warning ") {
				t.Errorf("at info: line %v, want only the warnings", line)
			}
		}
	})
}

func TestLineLimiter(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l := newLineLimiter(3, time.Minute)
	check := func(what string, now time.Time, want verdict) {
		t.Helper()
		if got := l.allow(now, slog.LevelWarn); got != want {
			t.Fatalf("%s: allow = %+v, want %+v", what, got, want)
		}
	}
	for i := range 3 {
		check(fmt.Sprint("line ", i), start.Add(time.Duration(i)*time.Second), verdict{ok: true})
	}
	check("first extra line", start.Add(10*time.Second), verdict{quietUntil: start.Add(time.Minute)})
	check("second extra line", start.Add(20*time.Second), verdict{})
	check("new window", start.Add(time.Minute), verdict{ok: true, reported: 2, reportedLevel: slog.LevelWarn})
	check("count reported once", start.Add(time.Minute+time.Second), verdict{ok: true})
	// A quiet period longer than a window: nothing suppressed, a fresh window.
	check("after an hour", start.Add(time.Hour), verdict{ok: true})
	// Each window has its own notice: the first refused line of this one names this window's end.
	check("hour +1s", start.Add(time.Hour+time.Second), verdict{ok: true})
	check("hour +2s", start.Add(time.Hour+2*time.Second), verdict{ok: true})
	check("hour +3s", start.Add(time.Hour+3*time.Second), verdict{quietUntil: start.Add(time.Hour + time.Minute)})
}

// TestLineLimiterLevels: the notice comes with the first refused line and with every refused line of a higher level
// than all those before it in the window; the count has the highest level of the window's refused lines.
func TestLineLimiterLevels(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	end, end2 := start.Add(time.Minute), start.Add(2*time.Minute)
	l := newLineLimiter(1, time.Minute)
	check := func(what string, now time.Time, level slog.Level, want verdict) {
		t.Helper()
		if got := l.allow(now, level); got != want {
			t.Fatalf("%s: allow = %+v, want %+v", what, got, want)
		}
	}
	at := func(d time.Duration) time.Time { return start.Add(d) }
	check("the window's line", at(0), slog.LevelDebug, verdict{ok: true})
	check("a debug line refused", at(1*time.Second), slog.LevelDebug, verdict{quietUntil: end})
	check("another debug line", at(2*time.Second), slog.LevelDebug, verdict{})
	check("an info line: higher", at(3*time.Second), slog.LevelInfo, verdict{quietUntil: end})
	check("debug after info", at(4*time.Second), slog.LevelDebug, verdict{})
	check("info again", at(5*time.Second), slog.LevelInfo, verdict{})
	check("an error: higher", at(6*time.Second), slog.LevelError, verdict{quietUntil: end})
	check("a warning below it", at(7*time.Second), slog.LevelWarn, verdict{})
	check("new window", at(time.Minute), slog.LevelDebug, verdict{ok: true, reported: 7, reportedLevel: slog.LevelError})
	check("only debug refused", at(time.Minute+time.Second), slog.LevelDebug, verdict{quietUntil: end2})
	check("next window", at(2*time.Minute), slog.LevelWarn, verdict{ok: true, reported: 1, reportedLevel: slog.LevelDebug})
}

// ICE credentials in the shapes that Pion and the browsers generate (README F20): the passwords never reach the log,
// the ufrags stay.
//
//nolint:gosec // G101: made-up ICE credentials, what the redaction must find
const (
	testUfrag = "NebKSTlgEbivgZgz"                 // pion/ice's own ufrags: 16 letters
	testPwd   = "OJMptkvWKLDWnqdfKgPQQVQOHIsJxdcu" // pion/ice's own passwords: 32 letters
	chromePwd = "4Xq+/Zp0aT7sLmN2bV9cR1eW"         // Chrome's: 24 ICE characters, + and / among them
	foxPwd    = "9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e" // Firefox's: 32 hex digits
)

// pionStartedAgent is the line pion/ice v4.4.7 logs at debug when an agent starts its connectivity checks
// (agent.go, startConnectivityChecks), with its format string as Pion writes it.
func pionStartedAgent(isControlling bool, remoteUfrag, remotePwd string) string {
	return fmt.Sprintf("Started agent: isControlling? %t, remoteUfrag: %q, remotePwd: %q", isControlling, remoteUfrag, remotePwd)
}

func TestRedactCredentials(t *testing.T) {
	iceParams := webrtc.ICEParameters{UsernameFragment: testUfrag, Password: testPwd}
	iceParamsJSON, err := json.Marshal(iceParams) //nolint:gosec // G117: a made-up password, what the redaction must find
	if err != nil {
		t.Fatal(err)
	}
	turn := webrtc.ICEServer{
		URLs: []string{"turn:turn.example.com:3478?transport=udp"}, Username: "friend", Credential: testPwd,
		CredentialType: webrtc.ICECredentialTypePassword,
	}
	redactedTests := []struct{ in, want string }{
		// Named passwords.
		{pionStartedAgent(false, testUfrag, testPwd),
			`Started agent: isControlling? false, remoteUfrag: "NebKSTlgEbivgZgz", remotePwd: "[redacted]"`},
		{pionStartedAgent(true, "abcd", chromePwd),
			`Started agent: isControlling? true, remoteUfrag: "abcd", remotePwd: "[redacted]"`},
		{pionStartedAgent(false, "3f2a91c0", `quote" and backslash\ in it, "remotePwd: x"`), // %q escapes them
			`Started agent: isControlling? false, remoteUfrag: "3f2a91c0", remotePwd: "[redacted]"`},
		{fmt.Sprintf("%+v", iceParams), "{UsernameFragment:NebKSTlgEbivgZgz Password:[redacted] ICELite:false}"},
		{string(iceParamsJSON), `{"usernameFragment":"NebKSTlgEbivgZgz","password":"[redacted]","iceLite":false}`},
		{fmt.Sprintf("%+v", turn),
			"{URLs:[turn:turn.example.com:3478?transport=udp] Username:friend Credential:[redacted] CredentialType:password}"},
		{"unexpected attribute ice-pwd:" + chromePwd + " in answer", "unexpected attribute ice-pwd:[redacted] in answer"},
		{"localPwd=" + foxPwd + ", next", "localPwd=[redacted], next"},
		{`{\"pwd\": \"` + testPwd + `\"}`, `{\"pwd\": [redacted]}`},
		{"icePwd: '" + testPwd + "' and LocalPwd=`" + foxPwd + "`", "icePwd: '[redacted]' and LocalPwd=`[redacted]`"},
		// ufrag:password pairs, whatever their names.
		{"credentials " + testUfrag + ":" + testPwd + " set", "credentials NebKSTlgEbivgZgz:[redacted] set"},
		{"pair abcd:" + chromePwd + ".", "pair abcd:[redacted]."},
		{`"3f2a91c0:` + foxPwd + `"`, `"3f2a91c0:[redacted]"`},
		{"ice:abcd:" + chromePwd + ":" + foxPwd, "ice:abcd:[redacted]:[redacted]"},
		{testUfrag + ":" + testPwd, "NebKSTlgEbivgZgz:[redacted]"},
	}
	for _, tt := range redactedTests {
		got := redactCredentials(tt.in)
		if got != tt.want {
			t.Errorf("redactCredentials(%q)\n = %q\nwant %q", tt.in, got, tt.want)
		}
		for _, secret := range []string{testPwd, chromePwd, foxPwd} {
			if strings.Contains(got, secret) {
				t.Errorf("redactCredentials(%q) = %q, still holds a password", tt.in, got)
			}
		}
	}

	// Pion's lines without a password, and what looks a little like a pair, come back as they are.
	kept := []string{
		"Failed to ping without candidate pairs. Connection is not possible yet.",
		"Setting new connection state: Checking",
		"Failed to accept RTP stream is already closed",
		"Incoming unhandled RTCP ssrc(3735928559), OnTrack will not be fired. Incoming unhandled RTP ssrc(42)",
		"GetConn by ufrag: " + testUfrag,
		"Ufrag: " + testUfrag,
		"remote pwd is empty",
		"local password must be at least 22 ice-chars for 128 bits of randomness",
		"Failed to gather relay candidates: password is empty",
		"the password-change throttle",
		"Discard request with wrong username from (192.0.2.10:50000), username mismatch expected(616263643a4e65624b) actual(65666768)",
		"STUN username abcd:" + testUfrag, // remote:local ufrags are 16 characters at most from browsers and Pion
		"Added connection: tcp remote [2001:db8:85a3::8a2e:370:7334]:50000 to local 192.0.2.1:7882",
		"turn:turn.example.com:3478?transport=udp",
		"fingerprint sha-256 AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89",
		"at 12:34:56.789, scope:ice",
		"https://example.com/a/very/long/path/without/a/password",
		"",
	}
	for _, msg := range kept {
		if got := redactCredentials(msg); got != msg {
			t.Errorf("redactCredentials(%q) = %q, want it unchanged", msg, got)
		}
	}
}

// The bridge redacts before it writes, at every level and through every method, and the line keeps its level.
func TestPionRedactsAtEveryLevel(t *testing.T) {
	var buf bytes.Buffer
	pl := NewPionLoggerFactory(newJSON(&buf, slog.LevelDebug)).NewLogger("ice")
	line := pionStartedAgent(false, testUfrag, testPwd)
	format, args := "Started agent: isControlling? %t, remoteUfrag: %q, remotePwd: %q", []any{false, testUfrag, testPwd}
	pl.Trace(line)
	pl.Tracef(format, args...)
	pl.Debug(line)
	pl.Debugf(format, args...)
	pl.Info(line)
	pl.Infof(format, args...)
	pl.Warn(line)
	pl.Warnf(format, args...)
	pl.Error(line)
	pl.Errorf(format, args...)
	pl.Warn("pair " + testUfrag + ":" + testPwd)

	if strings.Contains(buf.String(), testPwd) {
		t.Fatalf("the password was logged:\n%s", buf.String())
	}
	got := jsonLines(t, &buf)
	wantLevels := []string{"DEBUG", "DEBUG", "DEBUG", "DEBUG", "INFO", "INFO", "WARN", "WARN", "ERROR", "ERROR", "WARN"}
	if len(got) != len(wantLevels) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(wantLevels), buf.String())
	}
	want := `Started agent: isControlling? false, remoteUfrag: "NebKSTlgEbivgZgz", remotePwd: "[redacted]"`
	for i, l := range got[:len(got)-1] {
		if l["msg"] != want || l["level"] != wantLevels[i] || l["scope"] != "ice" {
			t.Errorf("line %d = %v, want %s %q", i, l, wantLevels[i], want)
		}
	}
	if last := got[len(got)-1]; last["msg"] != "pair NebKSTlgEbivgZgz:[redacted]" {
		t.Errorf("pair line = %v", last)
	}
}

// lockedBuffer is a bytes.Buffer for a logger that Pion's goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestPionRedactsICEAgentPassword runs pion/ice's own agent on the bridge at debug, as the SFU and netx do (README
// F20, the S90 log canary's finding): the agent names the remote password when its checks start, and the log holds
// neither password. A pion/ice that stops logging the line fails here, so the test can be looked at again.
func TestPionRedactsICEAgentPassword(t *testing.T) {
	out := new(lockedBuffer)
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	f := NewPionLoggerFactory(New(Options{Level: level, Format: "json", Out: out}).With("component", "sfu"))
	const localUfrag, remoteUfrag = "LocalQuillwren", "RemoteJunoberry"
	agent, err := ice.NewAgentWithOptions(
		ice.WithLoggerFactory(f),
		ice.WithMulticastDNSMode(ice.MulticastDNSModeDisabled),
		ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}),
		ice.WithLocalCredentials(localUfrag, foxPwd),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.StartDial(remoteUfrag, chromePwd); err != nil {
		t.Fatal(err)
	}
	if err := agent.GracefulClose(); err != nil {
		t.Fatal(err)
	}

	logs := out.String()
	for _, secret := range []string{chromePwd, foxPwd, jsonEscaped(chromePwd)} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds an ICE password (%s):\n%s", secret, logs)
		}
	}
	want := `Started agent: isControlling? true, remoteUfrag: \"RemoteJunoberry\", remotePwd: \"[redacted]\"`
	if !strings.Contains(logs, want) {
		t.Errorf("no line %s in pion/ice's log:\n%s", want, logs)
	}
}

// jsonEscaped is s as it appears inside a JSON string.
func jsonEscaped(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
