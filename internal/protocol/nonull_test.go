package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

// registryTypes returns every payload and result type of the Registry.
func registryTypes() []reflect.Type {
	seen := map[reflect.Type]bool{}
	var out []reflect.Type
	for _, s := range Registry {
		for _, v := range []any{s.Payload, s.Result} {
			if v == nil {
				continue
			}
			if t := reflect.TypeOf(v); !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// reachableStructs returns every struct type reachable from the Registry through fields, slices, pointers and maps
// (time.Time excluded).
func reachableStructs() []reflect.Type {
	seen := map[reflect.Type]bool{}
	var out []reflect.Type
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
		case reflect.Struct:
			if t == timeType || seen[t] {
				return
			}
			seen[t] = true
			out = append(out, t)
			for i := range t.NumField() {
				walk(t.Field(i).Type)
			}
		}
	}
	for _, t := range registryTypes() {
		walk(t)
	}
	return out
}

// populate fills v the way the no-null test needs: every slice gets one element, every pointer a value, each filled
// recursively; maps and relay payloads stay nil (they are omitempty or encode as {}), and leaves keep their zero
// values.
func populate(v reflect.Value, depth int) {
	if depth > 10 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				populate(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		populate(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		populate(p.Elem(), depth+1)
		v.Set(p)
	}
}

// TestNoNullZeroValues: the zero value of every Registry payload and result encodes through Marshal without a single
// null, and so does the zero value of every struct type reachable from them (01 §5: arrays are always present). A
// type with a non-omitempty slice needs a MarshalJSON method in encode.go to pass.
func TestNoNullZeroValues(t *testing.T) {
	for _, s := range Registry {
		for _, v := range []any{s.Payload, s.Result} {
			if v == nil {
				continue
			}
			out, err := Marshal(s.Type, "1", "", v)
			if err != nil {
				t.Fatalf("%s %T: %v", s.Type, v, err)
			}
			if p := findNull(jsonTree(t, out), "$"); p != "" {
				t.Errorf("%s: zero %T encodes null at %s: %s", s.Type, v, p, out)
			}
		}
	}
	for _, typ := range reachableStructs() {
		out, err := json.Marshal(reflect.New(typ).Elem().Interface())
		if err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		if p := findNull(jsonTree(t, out), "$"); p != "" {
			t.Errorf("zero %v encodes null at %s: %s", typ, p, out)
		}
	}
}

// TestNoNullPopulated: a fully populated zero value of every payload (one element per slice, every pointer set,
// recursively) encodes without null (01 §19 "No null").
func TestNoNullPopulated(t *testing.T) {
	for _, typ := range registryTypes() {
		v := reflect.New(typ).Elem()
		populate(v, 0)
		out, err := Marshal(MessageTypeOK, "", "1", v.Interface())
		if err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		if p := findNull(jsonTree(t, out), "$"); p != "" {
			t.Errorf("populated %v encodes null at %s: %s", typ, p, out)
		}
	}
}

// TestTimestampsAreUTCMilliseconds: every timestamp encodes as RFC 3339 UTC with exactly three fractional digits,
// whatever the location and precision of the time.Time (01 §5).
func TestTimestampsAreUTCMilliseconds(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*3600)
	at := time.Date(2026, 10, 13, 4, 2, 31, 20_123_456, loc) // 2026-10-12T19:02:31.020123456Z
	const want = "2026-10-12T19:02:31.020Z"
	cases := map[string]any{
		"welcome":     Welcome{ServerTime: at},
		"participant": ParticipantInfo{JoinedAt: at},
		"share":       ShareInfo{StartedAt: at},
		"event":       RoomEvent{At: at},
	}
	keys := map[string]string{"welcome": "serverTime", "participant": "joinedAt", "share": "startedAt", "event": "at"}
	for name, v := range cases {
		var m map[string]any
		if err := json.Unmarshal(mustJSON(t, v), &m); err != nil {
			t.Fatal(err)
		}
		if got := m[keys[name]]; got != want {
			t.Errorf("%s: %s = %v, want %s", name, keys[name], got, want)
		}
	}
	whole := RoomEvent{At: time.Date(2026, 10, 12, 19, 0, 1, 0, time.UTC)}
	if got := string(mustJSON(t, whole)); !strings.Contains(got, `"at":"2026-10-12T19:00:01.000Z"`) {
		t.Errorf("whole second: %s", got)
	}
	if _, err := json.Marshal(RoomEvent{At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Error("year 10000 encoded without an error")
	}
	// Decoding accepts any RFC 3339 form.
	var e RoomEvent
	if err := json.Unmarshal([]byte(`{"at":"2026-10-13T04:02:31.020123456+09:00"}`), &e); err != nil || !e.At.Equal(at) {
		t.Errorf("decode: %v, %v", e.At, err)
	}
}

// TestNilRelayPayloadEncodesAsObject: a relay message built without a payload sends {} ("absent means {}").
func TestNilRelayPayloadEncodesAsObject(t *testing.T) {
	for _, v := range []any{AgentSend{ToRole: RoleAgent, Kind: "share.request"}, AgentRecv{From: "c_1", Kind: "x"}} {
		if got := string(mustJSON(t, v)); !strings.Contains(got, `"payload":{}`) {
			t.Errorf("%T: %s", v, got)
		}
	}
	raw := json.RawMessage(`{"preset":"game"}`)
	if got := string(mustJSON(t, AgentSend{Payload: raw})); !strings.Contains(got, `"payload":{"preset":"game"}`) {
		t.Errorf("payload not verbatim: %s", got)
	}
}
