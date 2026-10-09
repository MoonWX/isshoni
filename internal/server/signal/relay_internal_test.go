package signal

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// relayFrom is a connection id for the encoding tests.
const relayFrom = "c_k3v9q2m7xw4pa8d1"

// relayFrame is the agent.send frame of a message by role with the kind and the payload's JSON text.
func relayFrame(kind string, payload []byte) []byte {
	return []byte(`{"type":"agent.send","id":"30","data":{"toRole":"agent","kind":` + strconv.Quote(kind) +
		`,"payload":` + string(payload) + `}}`)
}

// jsonValue decodes JSON text into a value, with numbers kept as their text.
func jsonValue(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}

// htmlEscaped reports whether encoding/json's escaping for HTML would change the JSON text: it has a <, a > or a &,
// or a U+2028 or U+2029.
func htmlEscaped(b []byte) bool {
	return bytes.ContainsAny(b, "<>&\u2028\u2029")
}

// checkRelayEncoding relays an agent.send frame's message as encodeAgentRecv does and checks what a target gets: a
// frame that parses, from the sender, with the kind and the payload. The payload is the one that came in, byte for
// byte but for the white space between its tokens, so the message is never larger than the payload plus the
// envelope; and unless the payload has a character that encoding/json escapes for HTML, the message is the very one
// that protocol.Marshal makes. It reports whether the frame was a valid agent.send at all.
func checkRelayEncoding(t *testing.T, frame []byte) bool {
	t.Helper()
	env, err := protocol.ParseEnvelope(frame)
	if err != nil || env.Type != protocol.MessageTypeAgentSend {
		return false
	}
	v, err := protocol.Decode[protocol.AgentSend](env)
	if err != nil {
		return false
	}
	// null is no payload (01 §8.14): the protocol's checks stop it, so the relay never hands one on.
	if bytes.Equal(bytes.TrimSpace(v.Payload), []byte("null")) {
		t.Fatalf("an agent.send with a null for its payload is valid:\n%s", frame)
	}
	out, err := encodeAgentRecv(relayFrom, v.Kind, v.Payload)
	if err != nil {
		t.Fatalf("a valid agent.send does not encode: %v\n%s", err, frame)
	}
	empty, err := encodeAgentRecv(relayFrom, v.Kind, json.RawMessage(`0`))
	if err != nil {
		t.Fatal(err)
	}
	if limit := len(empty) - 1 + len(v.Payload); len(out) > limit {
		t.Fatalf("a payload of %d bytes makes a message of %d, over the limit of %d", len(v.Payload), len(out), limit)
	}
	if !htmlEscaped(v.Payload) {
		same, err := protocol.Marshal(protocol.MessageTypeAgentRecv, "", "",
			protocol.AgentRecv{From: relayFrom, Kind: v.Kind, Payload: v.Payload})
		if err != nil || !bytes.Equal(out, same) {
			t.Fatalf("relayed as %s, but protocol.Marshal makes %s (%v)", out, same, err)
		}
	}
	got, err := protocol.ParseEnvelope(out)
	if err != nil || got.Type != protocol.MessageTypeAgentRecv || got.ID != "" || got.Re != "" {
		t.Fatalf("the relayed message %s does not parse as an agent.recv notification: %v", out, err)
	}
	r, err := protocol.Decode[protocol.AgentRecv](got)
	if err != nil || r.From != relayFrom || r.Kind != v.Kind {
		t.Fatalf("relayed as %s (%v), want from %s and kind %s", out, err, relayFrom, v.Kind)
	}
	want, err := jsonValue(v.Payload)
	if err != nil {
		t.Fatalf("payload %s: %v", v.Payload, err)
	}
	have, err := jsonValue(r.Payload)
	if err != nil || !reflect.DeepEqual(have, want) {
		t.Fatalf("payload %s was relayed as %s (%v)", v.Payload, r.Payload, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, v.Payload); err != nil || !bytes.Equal(r.Payload, compact.Bytes()) {
		t.Fatalf("payload %s was relayed as %s, want it unchanged but for its white space (%v)", v.Payload, r.Payload, err)
	}
	return true
}

// The relay puts the payload into the agent.recv itself (encodeAgentRecv), and the message must not drift from the
// protocol's: for a payload that HTML escaping leaves alone it is protocol.Marshal's, byte for byte, and the spec's
// example (01 §8.14). With a character that Marshal would escape, the two differ in that character's form only.
func TestRelayEncodeMatchesProtocol(t *testing.T) {
	marshal := func(kind, payload string) []byte {
		t.Helper()
		b, err := protocol.Marshal(protocol.MessageTypeAgentRecv, "", "",
			protocol.AgentRecv{From: relayFrom, Kind: kind, Payload: json.RawMessage(payload)})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct{ kind, payload string }{
		{"share.request", `{"preset":"game"}`},
		{"share.status", `{}`},
		{"playback.stop", `[1,2.5,-3e2,1E400,true,false,null,"x"]`},
		{"exclusions.set", ` { "spaced" : [ 1 , 2 ] ,` + "\n\t" + `"type" : "ok" , "from" : "c_0000000000000000" } `},
		{"exclusions.get", `{"escaped":"\u003c\u2028\ud800\"\\","text":"héllo 日本語 🎬"}`},
		{"x", `"text"`},
		{"x", `0`},
		// Not a payload that the relay ever gets (AgentSend.Validate refuses it): this is the encoder alone, whose
		// placeholder is this very text.
		{"x", `null`},
	} {
		if htmlEscaped([]byte(tc.payload)) {
			t.Fatalf("payload %s has a character that is escaped for HTML", tc.payload)
		}
		got, err := encodeAgentRecv(relayFrom, tc.kind, json.RawMessage(tc.payload))
		if want := marshal(tc.kind, tc.payload); err != nil || !bytes.Equal(got, want) {
			t.Errorf("payload %s: encoded as %s (%v), protocol.Marshal makes %s", tc.payload, got, err, want)
		}
	}
	const example = `{"type":"agent.recv","data":{"from":"c_k3v9q2m7xw4pa8d1","kind":"share.request","payload":{"preset":"game"}}}`
	if got, err := encodeAgentRecv(relayFrom, "share.request", json.RawMessage(`{"preset":"game"}`)); err != nil ||
		string(got) != example {
		t.Errorf("encoded as %s (%v), want the spec's example %s", got, err, example)
	}

	// With characters that Marshal escapes, the relay's message has them as they came in. Marshal's is the same JSON
	// value in more bytes: five more for each <, > and &, three more for each U+2028 and U+2029. (Should Marshal stop
	// escaping, encodeAgentRecv can go back to it.)
	const html = "{\"html\":\"<b>a&b</b>\",\"sep\":\"\u2028\u2029\"}"
	got, err := encodeAgentRecv(relayFrom, "exclusions.set", json.RawMessage(html))
	if err != nil || !bytes.HasSuffix(got, []byte(`"payload":`+html+`}}`)) {
		t.Errorf("encoded as %s (%v), want the payload %s as it is", got, err, html)
	}
	escaped := marshal("exclusions.set", html)
	if want := len(got) + 5*5 + 2*3; len(escaped) != want {
		t.Errorf("protocol.Marshal makes %d bytes, want %d (the relay's %d and the escapes): %s", len(escaped), want,
			len(got), escaped)
	}
	a, errA := jsonValue(got)
	b, errB := jsonValue(escaped)
	if errA != nil || errB != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("%s and %s are not the same JSON value (%v, %v)", got, escaped, errA, errB)
	}
}

// The largest message the relay sends is the largest payload, 16 KiB, plus the envelope, whatever the payload is
// made of: the characters that encoding/json would escape for HTML with six bytes each go out as they came in. So a
// target is sent what the sender was charged for, and agent.recv stays far under the 64 KiB of a message
// (protocol.MaxMessageBytes).
func TestRelayEncodeSize(t *testing.T) {
	text := func(n int, c string) []byte { // a JSON string of n bytes
		return []byte(`"` + strings.Repeat(c, (n-2)/len(c)) + `"`)
	}
	for _, tc := range []struct {
		name    string
		payload []byte
		max     int
	}{
		{"plain", text(protocol.MaxAgentPayloadBytes, "x"), protocol.MaxAgentPayloadBytes + 128},
		{"spaced", []byte(`[` + strings.Repeat(" 1 ,\n", 3000) + `2]`), protocol.MaxAgentPayloadBytes},
		{"html", text(protocol.MaxAgentPayloadBytes, "<"), protocol.MaxAgentPayloadBytes + 128},
		{"line separators", text(protocol.MaxAgentPayloadBytes, "\u2028"), protocol.MaxAgentPayloadBytes + 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.payload) > protocol.MaxAgentPayloadBytes {
				t.Fatalf("the payload has %d bytes", len(tc.payload))
			}
			if !checkRelayEncoding(t, relayFrame("share.request", tc.payload)) {
				t.Fatal("not a valid agent.send")
			}
			out, err := encodeAgentRecv(relayFrom, "share.request", tc.payload)
			if err != nil || len(out) > tc.max {
				t.Errorf("a message of %d bytes (%v), want at most %d", len(out), err, tc.max)
			}
		})
	}
}

// FuzzRelayEncode: whatever payload an agent.send gets past the protocol's checks, the relay encodes it, and the
// agent.recv parses on the receiving side with the same value and within the size limit (checkRelayEncoding).
func FuzzRelayEncode(f *testing.F) {
	for _, s := range []struct{ kind, payload string }{
		{"share.request", `{"preset":"game"}`},
		{"share.status", `{}`},
		{"playback.stop", `[1,2.5,-3e2,1E400,true,false,null,"x"]`},
		{"exclusions.set", `{"html":"<b>a&b</b>","escaped":"\u003c\u2028\ud800\"\\","text":"héllo 日本語 🎬"}`},
		{"exclusions.get", ` { "a" : [ 1 , { "a" : 2 , "a" : 3 } ] } `},
		{"x", `"\u2028\u2029"`},
		{"x", "\"\u2028\u2029\""},
		{"x", `0`},
		{"x", `null`},
		{"x", ` null `},
		{"x", `[null]`},
		{"x", `{"preset":"game"},"payload":null`},
		{"x", strings.Repeat("[", 29) + strings.Repeat("]", 29)},
		{"x", `1},"to":"c_0000000000000000","x":{"y":2`},
		{"Bad kind", `{}`},
		{"x", `{"unterminated":`},
		{"x", ""},
	} {
		f.Add(s.kind, []byte(s.payload))
	}
	f.Fuzz(func(t *testing.T, kind string, payload []byte) {
		checkRelayEncoding(t, relayFrame(kind, payload))
	})
}
