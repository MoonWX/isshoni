package native

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink is a JSON slog handler's output, safe for the drain goroutine and the test.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns the logged records, decoded.
func (s *logSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// waitFor polls the sink until a record with msg is there.
func (s *logSink) waitFor(t *testing.T, msg string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, r := range s.records(t) {
			if r["msg"] == msg {
				return r
			}
		}
	}
	t.Fatalf("no log record %q in %v", msg, s.records(t))
	return nil
}

func newSinkLogger(level slog.Level) (*logSink, *slog.Logger) {
	s := &logSink{}
	return s, slog.New(slog.NewJSONHandler(s, &slog.HandlerOptions{Level: level}))
}

// TestEngineRecord: one im_read_logs record (04 §5.6) becomes a slog record with the engine's time, level, message,
// its component as "area", its attrs and the drop count.
func TestEngineRecord(t *testing.T) {
	rec, ok := engineRecord([]byte(`{"t":"2026-10-21T12:00:00.123Z","level":"warn","component":"encode",` +
		`"msg":"encoder opened","attrs":{"w":1920,"layer":"f","fps":59.5,"hw":true,"dims":[1,2]},"dropped":3}`))
	if !ok {
		t.Fatal("not parsed")
	}
	if want := time.Date(2026, 10, 21, 12, 0, 0, 123e6, time.UTC); !rec.Time.Equal(want) {
		t.Errorf("time %v, want %v", rec.Time, want)
	}
	if rec.Level != slog.LevelWarn || rec.Message != "encoder opened" {
		t.Errorf("level %v, message %q", rec.Level, rec.Message)
	}
	attrs := map[string]any{}
	var keys []string
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		keys = append(keys, a.Key)
		return true
	})
	if got := strings.Join(keys, ","); got != "area,dims,fps,hw,layer,w,dropped" {
		t.Errorf("attr order %s", got)
	}
	if attrs["area"] != "encode" || attrs["w"] != int64(1920) || attrs["fps"] != 59.5 || attrs["hw"] != true ||
		attrs["layer"] != "f" || attrs["dropped"] != int64(3) {
		t.Errorf("attrs %v", attrs)
	}

	for level, want := range map[string]slog.Level{
		"error": slog.LevelError, "warn": slog.LevelWarn, "info": slog.LevelInfo, "debug": slog.LevelDebug,
		"verbose": slog.LevelInfo,
	} {
		if rec, ok := engineRecord([]byte(`{"level":"` + level + `","msg":"m"}`)); !ok || rec.Level != want {
			t.Errorf("level %q: %v, %v", level, rec.Level, ok)
		}
	}
	// A record without a readable time is logged now.
	if rec, ok := engineRecord([]byte(`{"t":"yesterday","msg":"m"}`)); !ok || time.Since(rec.Time) > time.Minute {
		t.Errorf("bad time: %v, %v", rec.Time, ok)
	}
	if _, ok := engineRecord([]byte(`not json`)); ok {
		t.Error("garbage parsed")
	}

	// The ring's unsigned attrs (Attr::Kind::kUint) keep every bit above math.MaxInt64.
	rec, ok = engineRecord([]byte(`{"msg":"m","attrs":{"u":18446744073709551615,"i":-9223372036854775808,` +
		`"big":18446744073709551616}}`))
	if !ok {
		t.Fatal("not parsed")
	}
	attrs = map[string]any{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	if attrs["u"] != uint64(math.MaxUint64) || attrs["i"] != int64(math.MinInt64) || attrs["big"] != float64(1<<64) {
		t.Errorf("attrs %#v", attrs)
	}
}

// TestLogDrain: the drain moves the engine's records into the logger, with component=engine, honoring the logger's
// level; an unreadable line is reported by its size only.
func TestLogDrain(t *testing.T) {
	f := newFakeCaller(t)
	sink, log := newSinkLogger(slog.LevelInfo)
	e := openFake(t, f, Options{Logger: log})
	f.queueLogs(
		`{"t":"2026-10-21T12:00:00.000Z","level":"info","component":"encode","msg":"encoder opened",`+
			`"attrs":{"layer":"f"}}`+"\n"+
			`{"t":"2026-10-21T12:00:00.001Z","level":"debug","component":"encode","msg":"debug detail"}`+"\n"+
			"\n"+
			`{"oops`+"\n",
		`{"t":"2026-10-21T12:00:01.000Z","level":"error","component":"capture","msg":"capture failed","dropped":2}`,
	)
	e.kickLogs()
	r := sink.waitFor(t, "capture failed")
	if r["component"] != "engine" || r["area"] != "capture" || r["dropped"] != float64(2) || r["level"] != "ERROR" {
		t.Errorf("record %v", r)
	}
	r = sink.waitFor(t, "encoder opened")
	if r["component"] != "engine" || r["area"] != "encode" || r["layer"] != "f" ||
		r["time"] != "2026-10-21T12:00:00Z" {
		t.Errorf("record %v", r)
	}
	r = sink.waitFor(t, "engine log record unreadable")
	if r["bytes"] != float64(len(`{"oops`)) || r["level"] != "WARN" {
		t.Errorf("record %v", r)
	}
	for _, r := range sink.records(t) {
		if r["msg"] == "debug detail" {
			t.Error("a debug record passed an info logger")
		}
	}
	// The engine-opened line of Open itself.
	if r := sink.waitFor(t, "engine opened"); r["abi"] != "1.0" || r["build"] != "0.2.0-test" {
		t.Errorf("record %v", r)
	}
}

// TestLogDrainAfterError: an error has the drain run at once instead of within a second (04 §12.3).
func TestLogDrainAfterError(t *testing.T) {
	f := newFakeCaller(t)
	sink, log := newSinkLogger(slog.LevelInfo)
	e := openFake(t, f, Options{Logger: log})
	f.queueLogs(`{"level":"error","component":"engine","msg":"clock read failed"}` + "\n")
	f.set(procClock, fakeFails(imErrInternal, "QueryPerformanceCounter failed"))
	start := time.Now()
	if _, err := e.Clock(); err == nil {
		t.Fatal("no error")
	}
	sink.waitFor(t, "clock read failed")
	if d := time.Since(start); d >= logDrainInterval {
		t.Errorf("drained after %v", d)
	}
}

// TestLogReadFailure: a failing im_read_logs is logged at warn at most once a minute, not every second.
func TestLogReadFailure(t *testing.T) {
	f := newFakeCaller(t)
	f.set(procReadLogs, fakeFails(imErrInternal, "ring broken"))
	sink, log := newSinkLogger(slog.LevelInfo)
	e := openFake(t, f, Options{Logger: log})
	for range 3 {
		before := f.count(procReadLogs)
		e.kickLogs()
		for deadline := time.Now().Add(5 * time.Second); f.count(procReadLogs) == before; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the drain did not run")
			}
		}
	}
	n := 0
	for _, r := range sink.records(t) {
		if r["msg"] == "engine logs unreadable" {
			n++
			if !strings.Contains(r["err"].(string), "internal") {
				t.Errorf("record %v", r)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d warnings, want 1", n)
	}
}
