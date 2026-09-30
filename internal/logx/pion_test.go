package logx

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"
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

		for i := range 15 {
			ice.Warnf("ice %d", i)
			ice2.Warnf("ice2 %d", i)
		}
		dtls.Warn("dtls still logs")
		ice.Warn("a=candidate-like lines are dropped and not counted")

		lines := jsonLines(t, &buf)
		if len(lines) != pionLinesPerWindow+1 {
			t.Fatalf("got %d lines in the first minute, want %d (20 ice + 1 dtls):\n%s", len(lines), pionLinesPerWindow+1, buf.String())
		}

		time.Sleep(pionWindow - time.Second)
		buf.Reset()
		ice.Warn("still suppressed")
		if buf.Len() != 0 {
			t.Fatalf("logged before the minute was over:\n%s", buf.String())
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

		buf.Reset()
		for i := range pionLinesPerWindow - 1 {
			ice.Warn(fmt.Sprint("more ", i))
		}
		ice.Warn("over the limit again")
		if n := len(jsonLines(t, &buf)); n != pionLinesPerWindow-1 {
			t.Errorf("second minute: %d lines, want %d", n, pionLinesPerWindow-1)
		}
	})
}

func TestLineLimiter(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l := newLineLimiter(3, time.Minute)
	for i := range 3 {
		if ok, s := l.allow(start.Add(time.Duration(i) * time.Second)); !ok || s != 0 {
			t.Fatalf("line %d: allow = %v, %d; want true, 0", i, ok, s)
		}
	}
	for i := range 2 {
		if ok, s := l.allow(start.Add(10 * time.Second)); ok || s != 0 {
			t.Fatalf("extra line %d: allow = %v, %d; want false, 0", i, ok, s)
		}
	}
	if ok, s := l.allow(start.Add(time.Minute)); !ok || s != 2 {
		t.Fatalf("new window: allow = %v, %d; want true, 2", ok, s)
	}
	if ok, s := l.allow(start.Add(time.Minute + time.Second)); !ok || s != 0 {
		t.Fatalf("count reported once: allow = %v, %d; want true, 0", ok, s)
	}
	// A quiet period longer than a window: nothing suppressed, a fresh window.
	if ok, s := l.allow(start.Add(time.Hour)); !ok || s != 0 {
		t.Fatalf("after an hour: allow = %v, %d; want true, 0", ok, s)
	}
}
