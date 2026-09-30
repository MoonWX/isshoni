package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const (
	secretValue = "r1.S3CR3T-resume-token"
	sdpValue    = "v=0\r\no=- 1 1 IN IP4 198.51.100.7\r\ns=-\r\nt=0 0\r\na=candidate:1 1 udp 1 198.51.100.7 7882 typ host\r\n"
)

// leaks reports whether out contains any recognizable part of the secret or the SDP.
func leaks(out string) bool {
	for _, s := range []string{"S3CR3T", "198.51.100.7", "candidate", "IN IP4"} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// TestSecretNeverFormatted: Secret and SDP never appear through fmt, with any verb, directly or inside structs,
// slices, maps and pointers (01 §17, §19).
func TestSecretNeverFormatted(t *testing.T) {
	s, sdp := Secret(secretValue), SDP(sdpValue)
	auth := &HelloAuth{Scheme: AuthSchemeBearer, Token: s}
	values := map[string]any{
		"secret":         s,
		"secret pointer": &s,
		"sdp":            sdp,
		"auth":           auth,
		"auth value":     *auth,
		"hello":          Hello{ResumeToken: s, Auth: auth},
		"hello pointer":  &Hello{ResumeToken: s, Auth: auth},
		"welcome":        Welcome{ResumeToken: s, ICEServers: []ICEServer{{URLs: []string{"turn:x"}, Credential: s}}},
		"offer":          PCOffer{PC: PCKindPub, SDP: sdp},
		"answer pointer": &PCAnswer{SDP: sdp},
		"slice":          []Secret{s, s},
		"map":            map[string]SDP{"offer": sdp},
		"any slice":      []any{s, sdp},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s", "%-8v"}
	for name, v := range values {
		for _, verb := range verbs {
			if out := fmt.Sprintf(verb, v); leaks(out) {
				t.Errorf("%s with %s leaks: %s", name, verb, out)
			}
		}
		for _, out := range []string{fmt.Sprint(v), fmt.Sprintln(v), fmt.Errorf("wrapped: %v", v).Error()} {
			if leaks(out) {
				t.Errorf("%s leaks through Sprint/Errorf: %s", name, out)
			}
		}
	}
	if got := fmt.Sprint(s); got != "[redacted]" {
		t.Errorf("Secret prints %q", got)
	}
	if got, want := fmt.Sprintf("%v", sdp), fmt.Sprintf("[sdp %d B]", len(sdpValue)); got != want {
		t.Errorf("SDP prints %q, want %q", got, want)
	}
	if s.String() != "[redacted]" || s.GoString() != "[redacted]" || !strings.HasPrefix(sdp.GoString(), "[sdp ") {
		t.Error("String/GoString not redacted")
	}
}

// TestSecretNeverLogged: slog's text and JSON handlers redact Secret and SDP attributes (LogValuer), in groups too.
func TestSecretNeverLogged(t *testing.T) {
	s, sdp := Secret(secretValue), SDP(sdpValue)
	for _, h := range []struct {
		name string
		new  func(*bytes.Buffer) slog.Handler
	}{
		{"text", func(b *bytes.Buffer) slog.Handler {
			return slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})
		}},
		{"json", func(b *bytes.Buffer) slog.Handler {
			return slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})
		}},
	} {
		var buf bytes.Buffer
		log := slog.New(h.new(&buf))
		log.Info("resume", "token", s, "sdp", sdp)
		log.Debug("attrs", slog.Any("token", s), slog.Any("sdp", sdp), slog.Group("pc", slog.Any("offer", sdp)))
		log.With("token", s).Warn("with", "auth", &HelloAuth{Scheme: AuthSchemeBearer, Token: s})
		log.Error("err", "err", fmt.Errorf("bad token %v", s))
		out := buf.String()
		if leaks(out) {
			t.Errorf("%s handler leaks:\n%s", h.name, out)
		}
		if !strings.Contains(out, "[redacted]") || !strings.Contains(out, fmt.Sprintf("[sdp %d B]", len(sdpValue))) {
			t.Errorf("%s handler: redaction markers missing:\n%s", h.name, out)
		}
	}
	// The text handler formats structs with %+v, which calls Format on the fields.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("hello", "msg", Hello{ResumeToken: s})
	if leaks(buf.String()) {
		t.Errorf("text handler leaks a struct field: %s", buf.String())
	}
}

// TestSecretJSONVerbatim: on the wire a Secret and an SDP are plain strings.
func TestSecretJSONVerbatim(t *testing.T) {
	b, err := json.Marshal(Hello{ResumeToken: secretValue, Auth: &HelloAuth{Scheme: AuthSchemeBearer, Token: "isa_tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"resumeToken":"`+secretValue+`"`) || !strings.Contains(string(b), `"token":"isa_tok"`) {
		t.Errorf("secrets not verbatim: %s", b)
	}
	b, err = json.Marshal(PCAnswer{SDP: SDP(sdpValue)})
	if err != nil {
		t.Fatal(err)
	}
	var back PCAnswer
	if err := json.Unmarshal(b, &back); err != nil || back.SDP.Reveal() != sdpValue {
		t.Errorf("SDP round trip: %q, %v", back.SDP.Reveal(), err)
	}
	if Secret(secretValue).Reveal() != secretValue {
		t.Error("Reveal")
	}
	// Errors built from protocol types never include params or payloads.
	pe := NewError(ErrorCodeBadRequest, ErrorScopeRequest)
	pe.Params = map[string]any{"field": secretValue}
	if err := error(&pe); strings.Contains(err.Error(), "S3CR3T") {
		t.Errorf("Error() includes params: %v", err)
	}
	if !errors.Is(fmt.Errorf("x: %w", &pe), &pe) {
		t.Error("errors.Is on the same *Error")
	}
}
