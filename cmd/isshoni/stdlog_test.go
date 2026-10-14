package main

import (
	"bytes"
	"encoding/json"
	stdlog "log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// stdLogRecords parses the JSON lines of a logx logger.
func stdLogRecords(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// What a library writes to the standard log package while serve runs becomes a record of serve's logger: JSON
// lines with a level from the line's prefix and the component stdlog, and the log package's own settings come back
// afterwards.
func TestRouteStdLog(t *testing.T) {
	var out bytes.Buffer
	level := new(slog.LevelVar)
	l := logx.New(logx.Options{Level: level, Format: "json", Out: &out})
	before := stdlog.Writer()
	restore := routeStdLog(l)
	stdlog.Printf("[ERROR] key auth HTTP server: %v", "accept failed")
	stdlog.Print("[WARNING] Unable to delete invalid OCSP staple file")
	stdlog.Print("panic: certificate worker: boom\ngoroutine 1 [running]:")
	stdlog.Print("something without a level")
	stdlog.Print("[INFO] a library's info line")
	stdlog.Print("[DEBUG] not at info")
	restore()
	if stdlog.Writer() != before {
		t.Error("the standard log's output was not put back")
	}

	recs := stdLogRecords(t, out.String())
	want := []struct{ level, msg string }{
		{"ERROR", "key auth HTTP server: accept failed"},
		{"WARN", "Unable to delete invalid OCSP staple file"},
		{"ERROR", "panic: certificate worker: boom\ngoroutine 1 [running]:"},
		{"WARN", "something without a level"},
		{"INFO", "a library's info line"},
	}
	if len(recs) != len(want) {
		t.Fatalf("%d records, want %d:\n%s", len(recs), len(want), out.String())
	}
	for i, w := range want {
		if recs[i]["level"] != w.level || recs[i]["msg"] != w.msg || recs[i]["component"] != stdLogComponent {
			t.Errorf("record %d = %v, want %s %q with component %s", i, recs[i], w.level, w.msg, stdLogComponent)
		}
	}
}

// The bridge keeps the rules of logx's Pion bridge: nothing that looks like SDP or a candidate, and no flood.
func TestStdLogWriterLimits(t *testing.T) {
	var out bytes.Buffer
	level := new(slog.LevelVar)
	w := newStdLogWriter(logx.New(logx.Options{Level: level, Format: "json", Out: &out}))
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	for _, line := range []string{
		"[ERROR] answer: v=0\no=- 1 2 IN IP4 127.0.0.1\na=ice-ufrag:abcd\n",
		"[WARNING] a=fingerprint:sha-256 00:11",
		"[ERROR] remote candidate:1 1 udp 2130706431 192.0.2.7 50000 typ host",
	} {
		_, _ = w.Write([]byte(line + "\n"))
	}
	if out.Len() != 0 {
		t.Errorf("SDP-like lines reached the log at info:\n%s", out.String())
	}

	for i := range stdLogLimit + 5 {
		_, _ = w.Write([]byte("[ERROR] the same error again " + string(rune('a'+i%26)) + "\n"))
	}
	if n := len(stdLogRecords(t, out.String())); n != stdLogLimit {
		t.Errorf("%d records in one minute, want %d", n, stdLogLimit)
	}
	now = now.Add(stdLogWindow)
	_, _ = w.Write([]byte("[ERROR] a minute later\n"))
	recs := stdLogRecords(t, out.String())
	if len(recs) != stdLogLimit+2 || recs[stdLogLimit]["msg"] != "5 lines of the standard log were suppressed" ||
		recs[stdLogLimit+1]["msg"] != "a minute later" {
		t.Errorf("after the window: %v", recs[stdLogLimit:])
	}
}
