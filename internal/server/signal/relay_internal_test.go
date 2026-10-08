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

// checkRelayEncoding relays an agent.send frame's message as encodeAgentRecv does and checks what a target gets: a
// frame that parses, from the sender, with the kind, a payload of the same JSON value, and at most six times the
// payload's size plus the envelope. It reports whether the frame was a valid agent.send at all.
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
	out, err := encodeAgentRecv(relayFrom, v.Kind, v.Payload)
	if err != nil {
		t.Fatalf("a valid agent.send does not encode: %v\n%s", err, frame)
	}
	empty, err := encodeAgentRecv(relayFrom, v.Kind, json.RawMessage(`0`))
	if err != nil {
		t.Fatal(err)
	}
	if limit := len(empty) - 1 + 6*len(v.Payload); len(out) > limit {
		t.Fatalf("a payload of %d bytes makes a message of %d, over the limit of %d", len(v.Payload), len(out), limit)
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
	return true
}

// The largest message the relay sends: a 16 KiB payload of characters that encoding/json escapes with six bytes
// each stays under 100 KiB, and a payload without them goes out no larger than it came in.
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
		{"html", text(protocol.MaxAgentPayloadBytes, "<"), 100 << 10},
		{"line separators", text(protocol.MaxAgentPayloadBytes, "\u2028"), 2*protocol.MaxAgentPayloadBytes + 128},
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
