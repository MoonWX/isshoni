package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	nested := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	for _, c := range []struct {
		name, in string
		ok       bool
	}{
		{"minimal", `{"type":"ping"}`, true},
		{"request", `{"type":"room.join","id":"2","data":{"roomId":"lounge"}}`, true},
		{"reply", `{"type":"ok","re":"2"}`, true},
		{"whitespace", " \n{ \"type\" : \"ping\" , \"data\" : { } }\n", true},
		{"unknown envelope fields", `{"type":"ping","v":2,"meta":{"a":[1]}}`, true},
		{"unknown type is not a parse error", `{"type":"room.teleport","id":"1"}`, true},
		{"id 32 chars", `{"type":"x","id":"` + strings.Repeat("a", 32) + `"}`, true},
		{"id charset", `{"type":"x","id":"aZ09_-"}`, true},
		{"data null is absent", `{"type":"ping","data":null}`, true},
		{"data nested 30 deep", `{"type":"x","data":{"a":` + nested(30) + `}}`, true},

		{"empty input", ``, false},
		{"not JSON", `hello`, false},
		{"truncated", `{"type":"ping"`, false},
		{"array", `[{"type":"ping"}]`, false},
		{"string", `"ping"`, false},
		{"number", `1`, false},
		{"null", `null`, false},
		{"type missing", `{"id":"1"}`, false},
		{"type empty", `{"type":""}`, false},
		{"type not a string", `{"type":1}`, false},
		{"type 65 bytes", `{"type":"` + strings.Repeat("a", 65) + `"}`, false},
		{"id 33 chars", `{"type":"x","id":"` + strings.Repeat("a", 33) + `"}`, false},
		{"id with a dot", `{"type":"x","id":"1.2"}`, false},
		{"id with a space", `{"type":"x","id":"1 2"}`, false},
		{"id not a string", `{"type":"x","id":1}`, false},
		{"re invalid", `{"type":"ok","re":"a/b"}`, false},
		{"data array", `{"type":"x","data":[]}`, false},
		{"data string", `{"type":"x","data":"{}"}`, false},
		{"data number", `{"type":"x","data":0}`, false},
		{"data nested 31 deep", `{"type":"x","data":{"a":` + nested(31) + `}}`, false},
		{"trailing garbage", `{"type":"ping"} x`, false},
		{"invalid UTF-8 in type", "{\"type\":\"p\xffing\"}", false},
		{"invalid UTF-8 in an unknown field", "{\"type\":\"ping\",\"x\":\"\xf3\xf3\"}", false},
		{"invalid UTF-8 in data", "{\"type\":\"x\",\"data\":{\"label\":\"\xc0\"}}", false},
		{"valid non-ASCII", `{"type":"x","data":{"label":"Filmabend 🎬 ſ"}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, err := ParseEnvelope([]byte(c.in))
			if c.ok != (err == nil) {
				t.Fatalf("ParseEnvelope(%q) = %+v, %v; want ok=%v", c.in, e, err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrBadMessage) {
				t.Errorf("error %v does not wrap ErrBadMessage", err)
			}
			if err == nil && string(e.Data) == "null" {
				t.Error("data: null was not normalized to absent")
			}
		})
	}
}

func TestParseEnvelopeFields(t *testing.T) {
	e, err := ParseEnvelope([]byte(`{"type":"share.start","id":"7","data":{"kind":"screen"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != MessageTypeShareStart || e.ID != "7" || e.Re != "" || string(e.Data) != `{"kind":"screen"}` {
		t.Errorf("got %+v (data %s)", e, e.Data)
	}
	// The envelope owns its data: changing the input afterwards must not change it.
	in := []byte(`{"type":"x","data":{"a":1}}`)
	e, err = ParseEnvelope(in)
	if err != nil {
		t.Fatal(err)
	}
	copy(in[len(in)-6:], "{\"b\":2")
	if string(e.Data) != `{"a":1}` {
		t.Errorf("data aliases the input: %s", e.Data)
	}
}

func TestMarshal(t *testing.T) {
	for _, c := range []struct {
		name string
		t    MessageType
		id   string
		re   string
		data any
		want string
	}{
		{"no data", MessageTypeOK, "", "9", nil, `{"type":"ok","re":"9"}`},
		{"empty payload", MessageTypeRoomLeave, "9", "", Empty{}, `{"type":"room.leave","id":"9","data":{}}`},
		{"typed nil pointer", MessageTypeOK, "", "3", (*RoomJoinResult)(nil), `{"type":"ok","re":"3"}`},
		{"nil raw message", MessageTypeOK, "", "3", json.RawMessage(nil), `{"type":"ok","re":"3"}`},
		{"payload", MessageTypeRoomJoin, "2", "", RoomJoin{RoomID: "lounge"}, `{"type":"room.join","id":"2","data":{"roomId":"lounge"}}`},
		{"pointer payload", MessageTypeRoomJoin, "2", "", &RoomJoin{RoomID: "lounge"}, `{"type":"room.join","id":"2","data":{"roomId":"lounge"}}`},
		{"nil slices become []", MessageTypeSubscribeStatus, "", "", SubscribeStatus{}, `{"type":"subscribe.status","data":{"subs":[]}}`},
		{"raw payload", MessageTypeOK, "", "4", json.RawMessage(` {"delivered": 1} `), `{"type":"ok","re":"4","data":{"delivered":1}}`},
		{"secrets are verbatim on the wire", MessageTypeHello, "1", "", HelloAuth{Scheme: AuthSchemeBearer, Token: "isa_t0k"},
			`{"type":"hello","id":"1","data":{"scheme":"bearer","token":"isa_t0k"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Marshal(c.t, c.id, c.re, c.data)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if _, err := ParseEnvelope(got); err != nil {
				t.Errorf("output does not parse: %v", err)
			}
		})
	}
	if _, err := Marshal("", "", "", nil); err == nil {
		t.Error("empty type accepted")
	}
	if _, err := Marshal(MessageTypeOK, "", "1", map[string]any{"c": make(chan int)}); err == nil {
		t.Error("unencodable payload accepted")
	}
	if _, err := Marshal(MessageTypeOK, "", "1", json.RawMessage(`{"a":`)); err == nil {
		t.Error("invalid raw payload accepted")
	}
}

func TestMarshalParseDecodeRoundTrip(t *testing.T) {
	label := "Slides"
	in := ShareUpdate{ShareID: "s_q7m2x9c4v8b1n5k3", Label: &label, Preset: PresetText}
	b, err := Marshal(MessageTypeShareUpdate, "11", "", in)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode[ShareUpdate](e)
	if err != nil {
		t.Fatal(err)
	}
	if out.ShareID != in.ShareID || *out.Label != label || out.Preset != in.Preset || e.ID != "11" {
		t.Errorf("got %+v", out)
	}
}

func TestNegotiate(t *testing.T) {
	for _, c := range []struct {
		clientMax, clientMin, want int
		ok                         bool
	}{
		{1, 1, 1, true},
		{2, 1, 1, true}, // a newer client that still speaks 1
		{3, 2, 1, false},
		{1, 0, 1, true},
		{0, 0, 0, false}, // protocol missing
		{-1, -5, -1, false},
	} {
		got, ok := Negotiate(c.clientMax, c.clientMin)
		if got != c.want || ok != c.ok {
			t.Errorf("Negotiate(%d, %d) = %d, %v; want %d, %v", c.clientMax, c.clientMin, got, ok, c.want, c.ok)
		}
	}
	if Version < MinVersion || MinVersion < 1 {
		t.Errorf("Version %d, MinVersion %d", Version, MinVersion)
	}
}
