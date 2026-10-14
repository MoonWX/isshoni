package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRedactMessage: an ICE password in the text of a message is replaced, in the forms that Pion and SDP write
// it; other text stays as it is.
func TestRedactMessage(t *testing.T) {
	tests := []struct{ in, want string }{
		{`Started agent: isControlling? false, remoteUfrag: "NebKSTlgEbivgZgz", remotePwd: "OJMptkvWKLDWnqdfKgPQQVQOHIsJxdcu"`,
			`Started agent: isControlling? false, remoteUfrag: "NebKSTlgEbivgZgz", remotePwd: "[redacted]"`},
		{"answer a=ice-pwd:asd88fgpdd777uzjYhagZg\r\n", "answer a=ice-pwd:\"[redacted]\"\r\n"},
		{"localPwd=abcdefghij, next", `localPwd="[redacted]", next`},
		{`{"pwd": "secretvalue"}`, `{"pwd": "[redacted]"}`},
		{"the password-change throttle", "the password-change throttle"},
		{"nothing to see", "nothing to see"},
		{"a pwd without a value", "a pwd without a value"},
	}
	for _, tt := range tests {
		if got := redactMessage(tt.in); got != tt.want {
			t.Errorf("redactMessage(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The server's logger redacts the messages of every logger derived from it, and keeps their attributes.
func TestSafeLogger(t *testing.T) {
	var out bytes.Buffer
	log := newSafeLogger(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if again := newSafeLogger(log); again != log {
		t.Error("newSafeLogger wrapped a safe logger twice")
	}
	log.With(slog.String("component", "sfu")).WithGroup("g").Debug(`remotePwd: "hunter2hunter2"`, slog.String("scope", "ice"))
	got := out.String()
	if strings.Contains(got, "hunter2") || !strings.Contains(got, `remotePwd: \"[redacted]\"`) ||
		!strings.Contains(got, `"component":"sfu"`) || !strings.Contains(got, `"g":{"scope":"ice"}`) {
		t.Errorf("the record = %s", got)
	}
}
