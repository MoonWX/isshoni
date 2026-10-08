package ops

import (
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestParseLogLevel(t *testing.T) {
	want := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	for _, name := range LogLevelNames {
		if got, ok := ParseLogLevel(name); !ok || got != want[name] {
			t.Errorf("ParseLogLevel(%q) = %v, %v", name, got, ok)
		}
	}
	if len(LogLevelNames) != len(want) {
		t.Errorf("LogLevelNames = %v", LogLevelNames)
	}
	for _, bad := range []string{"", "DEBUG", "warning", "trace", "info+2", " info"} {
		if _, ok := ParseLogLevel(bad); ok {
			t.Errorf("ParseLogLevel(%q) succeeded", bad)
		}
	}
}

// An override lasts for its duration and then gives way to the configured level (04 §3.1: "--for 30m, then
// back").
func TestLogLevelOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log, logs := testLogger()
		v := new(slog.LevelVar)
		v.Set(slog.LevelWarn) // log.level = "warn"
		l := NewLogLevel(v, log)
		defer l.Close()

		if level, until := l.Level(); level != slog.LevelWarn || !until.IsZero() {
			t.Fatalf("Level() = %v, %v before any override", level, until)
		}
		start := time.Now()
		l.Override(slog.LevelDebug, 30*time.Minute)
		if level, until := l.Level(); level != slog.LevelDebug || !until.Equal(start.Add(30*time.Minute)) {
			t.Errorf("Level() = %v until %v, want debug until start+30m", level, until)
		}
		time.Sleep(30*time.Minute - time.Second)
		synctest.Wait()
		if v.Level() != slog.LevelDebug {
			t.Errorf("level %v one second before the end, want debug", v.Level())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if level, until := l.Level(); level != slog.LevelWarn || !until.IsZero() {
			t.Errorf("Level() = %v, %v after the override ran out, want warn", level, until)
		}
		// The line names the level that is back as log_level. "level" is the record's own severity, and a second
		// member of that name would hide it from a JSON parser that keeps the last one.
		line := logs.String()
		if strings.Count(line, "\n") != 1 || !strings.Contains(line, "back to the configured level") {
			t.Fatalf("want one log line for the end of the override:\n%s", line)
		}
		if n := strings.Count(line, `"level":`); n != 1 || !strings.Contains(line, `"level":"INFO"`) || !strings.Contains(line, `"log_level":"warn"`) {
			t.Errorf("the line has %d \"level\" members, want one (INFO) and log_level=warn:\n%s", n, line)
		}
	})
}

// A second override replaces the first one and its time; the first timer ends nothing.
func TestLogLevelOverrideReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v := new(slog.LevelVar)
		l := NewLogLevel(v, slog.New(slog.DiscardHandler))
		defer l.Close()

		l.Override(slog.LevelDebug, time.Minute)
		time.Sleep(30 * time.Second)
		l.Override(slog.LevelError, time.Hour)
		time.Sleep(31 * time.Second) // past the first override's end
		synctest.Wait()
		if v.Level() != slog.LevelError {
			t.Errorf("level %v after the first override's time, want the second override's error", v.Level())
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if v.Level() != slog.LevelInfo {
			t.Errorf("level %v after the second override, want info", v.Level())
		}

		// A duration of zero ends the override in effect.
		l.Override(slog.LevelDebug, time.Hour)
		l.Override(slog.LevelError, 0)
		if level, until := l.Level(); level != slog.LevelInfo || !until.IsZero() {
			t.Errorf("Level() = %v, %v after Override(_, 0), want info and no end", level, until)
		}
	})
}

// A reload of log.level (SIGHUP) changes the base: at once without an override, and as what an override falls
// back to.
func TestLogLevelSetBase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v := new(slog.LevelVar)
		l := NewLogLevel(v, slog.New(slog.DiscardHandler))
		defer l.Close()

		l.SetBase(slog.LevelWarn)
		if v.Level() != slog.LevelWarn {
			t.Errorf("level %v after SetBase(warn), want warn", v.Level())
		}
		l.Override(slog.LevelDebug, time.Minute)
		l.SetBase(slog.LevelError)
		if v.Level() != slog.LevelDebug {
			t.Errorf("SetBase during an override changed the level to %v", v.Level())
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if v.Level() != slog.LevelError {
			t.Errorf("level %v after the override, want the new base error", v.Level())
		}
	})
}

// Close ends the override and stops its timer (goleak checks that nothing is left); afterwards nothing changes the
// level.
func TestLogLevelClose(t *testing.T) {
	v := new(slog.LevelVar)
	l := NewLogLevel(v, slog.New(slog.DiscardHandler))
	l.Override(slog.LevelDebug, time.Hour)
	l.Close()
	l.Close()
	if v.Level() != slog.LevelInfo {
		t.Errorf("level %v after Close, want the base info", v.Level())
	}
	l.Override(slog.LevelDebug, time.Hour)
	l.SetBase(slog.LevelError)
	if level, until := l.Level(); level != slog.LevelInfo || !until.IsZero() {
		t.Errorf("Level() = %v, %v: a closed LogLevel changed the level", level, until)
	}
	defer func() {
		if recover() == nil {
			t.Error("NewLogLevel(nil, nil) did not panic")
		}
	}()
	NewLogLevel(nil, nil)
}
