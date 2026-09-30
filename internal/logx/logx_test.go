package logx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonLines decodes the JSON log lines in buf.
func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func newJSON(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	lv := new(slog.LevelVar)
	lv.Set(level)
	return New(Options{Level: lv, Format: "json", Out: buf})
}

// ttyBuffer is a bytes.Buffer that claims to be a terminal, as an *os.File on a character device would.
type ttyBuffer struct{ bytes.Buffer }

func (*ttyBuffer) Stat() (fs.FileInfo, error) { return charDevice{}, nil }

type charDevice struct{}

func (charDevice) Name() string       { return "tty" }
func (charDevice) Size() int64        { return 0 }
func (charDevice) Mode() fs.FileMode  { return fs.ModeDevice | fs.ModeCharDevice | 0o620 }
func (charDevice) ModTime() time.Time { return time.Time{} }
func (charDevice) IsDir() bool        { return false }
func (charDevice) Sys() any           { return nil }

func isJSONLine(s string) bool {
	return strings.HasPrefix(s, "{") && json.Valid([]byte(strings.TrimSpace(s)))
}

func isTextLine(s string) bool { return strings.HasPrefix(s, "time=") }

func TestFormatAuto(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()
	regular, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = regular.Close() }()

	tests := []struct {
		name     string
		out      io.Writer
		wantJSON bool
	}{
		{"terminal", &ttyBuffer{}, false},
		{"pipe (systemd journal, docker without -t)", pw, true},
		{"regular file", regular, true},
		{"plain writer", &bytes.Buffer{}, true},
	}
	for _, tt := range tests {
		for _, format := range []string{"auto", "", "unknown"} {
			if got := useJSON(format, tt.out); got != tt.wantJSON {
				t.Errorf("%s, format %q: JSON = %v, want %v", tt.name, format, got, tt.wantJSON)
			}
		}
		if !useJSON("json", tt.out) {
			t.Errorf("%s: format json gave text", tt.name)
		}
		if useJSON("text", tt.out) {
			t.Errorf("%s: format text gave JSON", tt.name)
		}
	}
}

func TestNewWritesTheDetectedFormat(t *testing.T) {
	tty := &ttyBuffer{}
	New(Options{Out: tty}).Info("hello", "k", "v")
	if got := tty.String(); !isTextLine(got) || !strings.Contains(got, "msg=hello k=v") {
		t.Errorf("terminal output = %q, want a text line", got)
	}

	var buf bytes.Buffer
	New(Options{Out: &buf}).Info("hello", "k", "v")
	if got := buf.String(); !isJSONLine(got) {
		t.Errorf("non-terminal output = %q, want a JSON line", got)
	}

	tty.Reset()
	New(Options{Out: tty, Format: "json"}).Info("hello")
	if got := tty.String(); !isJSONLine(got) {
		t.Errorf("format json on a terminal = %q, want a JSON line", got)
	}

	buf.Reset()
	New(Options{Out: &buf, Format: "text"}).Info("hello")
	if got := buf.String(); !isTextLine(got) {
		t.Errorf("format text on a pipe = %q, want a text line", got)
	}
}

func TestNewLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Out: &buf, Format: "json"}) // nil Level: info
	l.Debug("hidden")
	l.Info("shown")
	if got := jsonLines(t, &buf); len(got) != 1 || got[0]["msg"] != "shown" {
		t.Errorf("default level: lines = %v, want only the info line", got)
	}

	buf.Reset()
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	l = New(Options{Level: lv, Out: &buf, Format: "json"})
	l.Info("hidden")
	lv.Set(slog.LevelDebug) // the admin socket's log-level
	l.Debug("shown")
	if got := jsonLines(t, &buf); len(got) != 1 || got[0]["msg"] != "shown" || got[0]["level"] != "DEBUG" {
		t.Errorf("level change: lines = %v, want only the debug line after the change", got)
	}
}

func TestNewDefaultsToStderr(t *testing.T) {
	l := New(Options{})
	if !l.Enabled(t.Context(), slog.LevelInfo) || l.Enabled(t.Context(), slog.LevelDebug) {
		t.Error("New(Options{}) is not at info level")
	}
}

func TestErr(t *testing.T) {
	var buf bytes.Buffer
	newJSON(&buf, slog.LevelInfo).Error("failed", Err(errors.New("boom")))
	got := jsonLines(t, &buf)
	if len(got) != 1 || got[0]["err"] != "boom" {
		t.Errorf("lines = %v, want err=boom", got)
	}
	if a := Err(nil); a.Key != "err" {
		t.Errorf("Err(nil).Key = %q", a.Key)
	}
}

// The ReplaceAttr safety net of 04 §10.
func TestRedactKeys(t *testing.T) {
	const raw = "s3cr3t-value"
	type payload struct{ Value string }
	values := map[string]any{
		"string": raw,
		"bytes":  []byte(raw),
		"struct": payload{raw},
		"error":  errors.New(raw),
		"int":    1234567,
	}
	keys := []string{
		"password", "token", "secret", "authorization", "cookie",
		"sdp", "offer", "answer", "candidate", "endpoint",
		"resume_token", "invite_token", "Authorization", "TOKEN", "Setup_Token",
	}
	for _, format := range []string{"json", "text"} {
		for _, key := range keys {
			for kind, v := range values {
				var buf bytes.Buffer
				New(Options{Out: &buf, Format: format}).Info("x", key, v)
				got := buf.String()
				if strings.Contains(got, raw) || strings.Contains(got, "1234567") {
					t.Errorf("%s, key %q, %s value: leaked: %s", format, key, kind, got)
				}
				if !strings.Contains(got, redacted) {
					t.Errorf("%s, key %q, %s value: no %s: %s", format, key, kind, redacted, got)
				}
			}
		}
	}
}

func TestRedactKeepsOtherKeys(t *testing.T) {
	var buf bytes.Buffer
	newJSON(&buf, slog.LevelInfo).Info("x",
		"user_id", "u1", "room_id", "lounge", "token_count", 3, "tokens", 2, "passwords_checked", 1, "push_host", "fcm.googleapis.com")
	got := jsonLines(t, &buf)[0]
	want := map[string]any{
		"user_id": "u1", "room_id": "lounge", "token_count": 3.0, "tokens": 2.0, "passwords_checked": 1.0,
		"push_host": "fcm.googleapis.com",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestRedactGroupsAndWith(t *testing.T) {
	const raw = "v=0 s3cr3t"
	var buf bytes.Buffer
	l := newJSON(&buf, slog.LevelInfo)
	l.Info("group named offer", slog.Group("offer", "type", "offer", "body", raw))
	l.Info("nested", slog.Group("req", slog.Group("headers", slog.String("authorization", raw)), "route", "/ws"))
	l.WithGroup("cookie").Info("with group", "value", raw)
	l.With("password", raw).Info("with attrs")
	l.With(slog.Group("secret", "a", raw)).Info("with group attr")
	out := buf.String()
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("leaked:\n%s", out)
	}
	lines := jsonLines(t, &buf)
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5", len(lines))
	}
	offer := lines[0]["offer"].(map[string]any)
	if offer["type"] != redacted || offer["body"] != redacted {
		t.Errorf("offer group = %v, want every member redacted", offer)
	}
	req := lines[1]["req"].(map[string]any)
	if req["route"] != "/ws" {
		t.Errorf("req.route = %v, want it kept", req["route"])
	}
}

func TestSecretInHandlers(t *testing.T) {
	const raw = "hunter2-secret"
	type creds struct {
		User string
		Pass Secret
		Key  SecretBytes
	}
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		l := New(Options{Out: &buf, Format: format})
		l.Info("attrs", "value", Secret(raw), "bytes", SecretBytes(raw))
		l.Info("struct", "creds", creds{User: "u1", Pass: Secret(raw), Key: SecretBytes(raw)})
		l.Info("pointer", "p", new(Secret(raw)))
		l.Info("slice", "list", []Secret{Secret(raw), Secret(raw)})
		l.Info(Secret(raw).String())
		out := buf.String()
		if strings.Contains(out, "hunter2") {
			t.Errorf("%s handler leaked:\n%s", format, out)
		}
		if n := strings.Count(out, redacted); n < 8 {
			t.Errorf("%s handler: %d redactions, want at least 8:\n%s", format, n, out)
		}
	}
}
