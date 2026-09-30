package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

const (
	secretValue = "r1.S3CR3T-resume-token"
	sdpValue    = "v=0\r\no=- 1 1 IN IP4 198.51.100.7\r\ns=-\r\nt=0 0\r\na=candidate:1 1 udp 1 198.51.100.7 7882 typ host\r\n"
)

// leaks reports whether out contains any recognizable part of the secret, the SDP or a candidate.
func leaks(out string) bool {
	for _, s := range []string{"S3CR3T", "198.51.100.7", "a=candidate", "typ host", "IN IP4"} {
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

// TestPayloadsNeverLogged: payload structs that hold a Secret, an SDP or a candidate log a redacted summary through
// both handlers. slog resolves LogValuer only for the attribute's own value, and the JSON handler encodes any other
// struct with encoding/json, so without their own LogValue the nested fields would appear verbatim.
func TestPayloadsNeverLogged(t *testing.T) {
	s, sdp := Secret(secretValue), SDP(sdpValue)
	candidate := "candidate:1 1 udp 2122260223 198.51.100.7 7882 typ host"
	mid := "0"
	hello := Hello{Protocol: 1, MinProtocol: 1, Features: []Feature{FeatureAgentRelay},
		Client: ClientInfo{Kind: ClientKindTool, Version: "0.1.0", OS: ClientOSLinux}, Role: RoleFull,
		ResumeToken: s, Auth: &HelloAuth{Scheme: AuthSchemeBearer, Token: "isa_" + s}}
	welcome := Welcome{Protocol: 1, ConnectionID: "c_k3v9q2m7xw4pa8d1", ResumeToken: s, Resumed: true, RoomID: "lounge",
		ICEServers: []ICEServer{{URLs: []string{"turn:turn.example.org"}, Username: "u_S3CR3T", Credential: s}},
		User:       UserInfo{ID: "u_1", Name: "S3CR3T-username"}}
	tracks := []TrackRef{{MID: "0", ShareID: "s_1", Kind: TrackKindVideo}, {MID: "1", ShareID: "s_1", Kind: TrackKindAudio}}
	for _, c := range []struct {
		name  string
		value any
		want  []string // substrings of the JSON handler's output
	}{
		{"hello", hello, []string{`"protocol":1`, `"role":"full"`, `"kind":"tool"`, `"resumeToken":"[redacted]"`,
			`"auth":{"scheme":"bearer"}`, `"features":1`}},
		{"hello pointer", &hello, []string{`"role":"full"`}},
		{"hello without secrets", Hello{Role: RoleViewer}, []string{`"role":"viewer"`}},
		{"welcome", welcome, []string{`"connectionId":"c_k3v9q2m7xw4pa8d1"`, `"resumed":true`, `"roomId":"lounge"`,
			`"iceServers":1`}},
		{"welcome pointer", &welcome, []string{`"resumed":true`}},
		{"ice server", welcome.ICEServers[0], []string{`"urls":["turn:turn.example.org"]`}},
		{"pub offer", PCOffer{PC: PCKindPub, Gen: 2, Neg: 3, SDP: sdp, Tracks: tracks},
			[]string{`"pc":"pub"`, `"gen":2`, `"neg":3`, fmt.Sprintf(`"sdp":"[sdp %d B]"`, len(sdpValue)), `"tracks":2`}},
		{"sub offer pointer", &PCOffer{PC: PCKindSub, Gen: 1, Neg: 1, SDP: sdp}, []string{`"pc":"sub"`, `"tracks":0`}},
		{"answer", PCAnswer{PC: PCKindSub, Gen: 1, Neg: 4, SDP: sdp}, []string{`"neg":4`, `"sdp":"[sdp `}},
		{"answer pointer", &PCAnswer{PC: PCKindPub, Gen: 1, Neg: 1, SDP: sdp}, []string{`"pc":"pub"`}},
		{"candidate", PCICE{PC: PCKindPub, Gen: 1, Candidate: &ICECandidate{Candidate: candidate, SDPMid: &mid}},
			[]string{`"pc":"pub"`, `"gen":1`, `"candidate":true`}},
		{"end of candidates", &PCICE{PC: PCKindPub, Gen: 1}, []string{`"candidate":false`}},
	} {
		var jsonBuf, textBuf bytes.Buffer
		slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("x", "payload", c.value)
		slog.New(slog.NewTextHandler(&textBuf, nil)).Info("x", slog.Any("payload", c.value), slog.Group("g", "v", c.value))
		for name, out := range map[string]string{"json": jsonBuf.String(), "text": textBuf.String()} {
			if leaks(out) || strings.Contains(out, "isa_") || strings.Contains(out, "LogValue panicked") {
				t.Errorf("%s: %s handler leaks:\n%s", c.name, name, out)
			}
		}
		for _, w := range c.want {
			if !strings.Contains(jsonBuf.String(), w) {
				t.Errorf("%s: JSON output lacks %s:\n%s", c.name, w, jsonBuf.String())
			}
		}
	}
}

// TestSecretHoldersLogRedacted: every Registry struct that holds a Secret or an SDP, directly or through its fields,
// implements slog.LogValuer on its value type, so a new payload with a token cannot reach the JSON handler verbatim.
func TestSecretHoldersLogRedacted(t *testing.T) {
	secretTypes := map[reflect.Type]bool{reflect.TypeFor[Secret](): true, reflect.TypeFor[SDP](): true}
	logValuer := reflect.TypeFor[slog.LogValuer]()
	memo := map[reflect.Type]bool{}
	var holds func(reflect.Type) bool
	holds = func(typ reflect.Type) bool {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			return holds(typ.Elem())
		case reflect.Struct:
			if v, ok := memo[typ]; ok {
				return v
			}
			memo[typ] = false // a cycle adds nothing
			for i := range typ.NumField() {
				if holds(typ.Field(i).Type) {
					memo[typ] = true
				}
			}
			return memo[typ]
		}
		return secretTypes[typ]
	}
	found := 0
	for _, typ := range reachableStructs() {
		if !holds(typ) {
			continue
		}
		found++
		if !typ.Implements(logValuer) {
			t.Errorf("%v holds a Secret or an SDP but has no value-receiver LogValue method", typ)
		}
	}
	if found < 6 { // Hello, HelloAuth, Welcome, ICEServer, PCOffer, PCAnswer
		t.Errorf("found only %d structs holding secrets; the walk is broken", found)
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
