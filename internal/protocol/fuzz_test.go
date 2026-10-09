package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// allocatedBytes returns the bytes allocated on the heap while fn runs.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// allocatedWithin is allocatedBytes for a bound: when fn allocated more than limit, it runs fn once more and returns
// that second measurement. The first run of an input may also pay for what encoding/json keeps between calls (its
// pooled buffers, which a fuzz worker that starts with this input has not grown yet, and which a GC empties); that
// is not the decode's own cost, and it depends on which inputs the process saw before. The second run, right
// after, measures the decode alone, so a real excess fails both. fn must start from fresh values each time.
func allocatedWithin(limit uint64, fn func()) uint64 {
	n := allocatedBytes(fn)
	if n > limit {
		n = allocatedBytes(fn)
	}
	return n
}

// Allocation bounds of the fuzz targets (01 §19: "no allocation above 4× the input"). The constant covers what
// does not grow with the input: encoding/json's decode state and, for Decode, the slices that the array limits
// allow (about 54 KB for a ClientStats with every list at its limit and every element empty).
const (
	envelopeAllocSlack = 8 << 10
	decodeAllocSlack   = 64 << 10
	// clientDecodeRatio bounds server->client payloads, which only clients decode (from their own server) and whose
	// arrays have no count limit: encoding/json allocates up to ~250 bytes per "{}," element there (ShareInfo,
	// grown by doubling). The bound still catches anything superlinear.
	clientDecodeRatio = 1024
)

// decodeTarget is one payload type of the Registry for FuzzDecode.
type decodeTarget struct {
	typ           reflect.Type
	msgType       MessageType
	serverDecoded bool // the payload of a client->server message: the hub decodes it from untrusted input
}

// decodeTargets returns every payload and result type of the Registry, sorted by type name so fuzz seeds keep their
// meaning when the Registry is reordered.
func decodeTargets() []decodeTarget {
	byType := map[reflect.Type]*decodeTarget{}
	for _, s := range Registry {
		for j, v := range []any{s.Payload, s.Result} {
			if v == nil {
				continue
			}
			typ := reflect.TypeOf(v)
			tg, ok := byType[typ]
			if !ok {
				tg = &decodeTarget{typ: typ, msgType: s.Type}
				byType[typ] = tg
			}
			if j == 0 && s.Dir == DirClientToServer {
				tg.serverDecoded = true
			}
		}
	}
	var out []decodeTarget
	for _, tg := range byType {
		out = append(out, *tg)
	}
	slices.SortFunc(out, func(a, b decodeTarget) int { return strings.Compare(a.typ.Name(), b.typ.Name()) })
	return out
}

func targetIndex(targets []decodeTarget, zero any) uint8 {
	for i, tg := range targets {
		if tg.typ == reflect.TypeOf(zero) {
			return uint8(i)
		}
	}
	panic("no decode target for " + reflect.TypeOf(zero).String())
}

func FuzzParseEnvelope(f *testing.F) {
	for _, fx := range loadFixtures(f, fixtureDir) {
		f.Add(fx.raw)
	}
	_, _ = ParseEnvelope([]byte(`{"type":"x","id":"1","data":{}}`)) // warm encoding/json's type cache
	f.Fuzz(func(t *testing.T, b []byte) {
		var e Envelope
		var err error
		limit := 4*uint64(len(b)) + envelopeAllocSlack
		if n := allocatedWithin(limit, func() { e, err = ParseEnvelope(b) }); n > limit {
			t.Fatalf("ParseEnvelope allocated %d bytes for %d input bytes (limit %d)", n, len(b), limit)
		}
		if err != nil {
			if !errors.Is(err, ErrBadMessage) {
				t.Fatalf("error %v does not wrap ErrBadMessage", err)
			}
			return
		}
		switch {
		case e.Type == "" || len(e.Type) > maxTypeLen:
			t.Fatalf("accepted type %q", e.Type)
		case e.ID != "" && !validToken(e.ID, MaxIDLen), e.Re != "" && !validToken(e.Re, MaxIDLen):
			t.Fatalf("accepted id %q / re %q", e.ID, e.Re)
		case len(e.Data) > 0 && e.Data[0] != '{':
			t.Fatalf("accepted data %s", e.Data)
		}
		// What Marshal writes, ParseEnvelope reads back unchanged.
		out, err := Marshal(e.Type, e.ID, e.Re, e.Data)
		if err != nil {
			t.Fatalf("Marshal of a parsed envelope: %v", err)
		}
		e2, err := ParseEnvelope(out)
		if err != nil {
			t.Fatalf("re-parse %s: %v", out, err)
		}
		if e2.Type != e.Type || e2.ID != e.ID || e2.Re != e.Re || !sameJSON(t, e.Data, e2.Data) {
			t.Fatalf("round trip changed the envelope: %+v -> %+v", e, e2)
		}
	})
}

// sameJSON reports whether a and b hold the same JSON value (both empty counts as the same).
func sameJSON(t *testing.T, a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return reflect.DeepEqual(jsonTree(t, a), jsonTree(t, b))
}

func FuzzDecode(f *testing.F) {
	targets := decodeTargets()
	// Warm encoding/json's per-type caches, so the first input a worker measures doesn't pay for them.
	for _, tg := range targets {
		_ = DecodeInto(Envelope{Data: json.RawMessage(`{}`)}, reflect.New(tg.typ).Interface())
		_, _ = json.Marshal(reflect.New(tg.typ).Elem().Interface())
	}
	for _, fx := range loadFixtures(f, fixtureDir) {
		if zero, _, err := fixturePayload(fx); err == nil && len(fx.env.Data) > 0 {
			f.Add(targetIndex(targets, zero), []byte(fx.env.Data))
		}
	}
	dense := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("{},", n), ",") + "]" }
	f.Add(targetIndex(targets, SubscribeUpdate{}), []byte(`{"subs":`+dense(MaxSubs+1)+`}`))
	f.Add(targetIndex(targets, ClientStats{}), []byte(`{"pcs":`+dense(4)+`,"inbound":`+dense(128)+`,"outbound":`+dense(16)+`}`))
	f.Add(targetIndex(targets, Hello{}), []byte(`{"feat`+jsonEsc("0075")+`res":[`+strings.Repeat(`"x",`, 40)+`"x"]}`))
	f.Add(targetIndex(targets, RoomState{}), []byte(`{"shares":`+dense(200)+`,"participants":[{"connections":`+dense(50)+`}]}`))
	f.Add(targetIndex(targets, AgentSend{}), []byte(`{"to":"c_1","kind":"a.b","payload":`+strings.Repeat("[", 30)+strings.Repeat("]", 30)+`}`))
	f.Add(targetIndex(targets, AgentSend{}), []byte(`{"toRole":"agent","kind":"a.b","payload":null}`))
	f.Add(targetIndex(targets, AgentSend{}), []byte(`{"toRole":"agent","kind":"a.b","payload":{"a":null},"payload": null }`))
	f.Add(targetIndex(targets, AgentSend{}), []byte(`{"toRole":"agent","kind":"a.b","payload":null,"payload":[null]}`))
	f.Add(targetIndex(targets, ShareStart{}), []byte(`{"kind":"tab","label":"`+jsonEsc("202e")+` `+jsonEsc("0041")+` ","preset":"game","ref":"r"}`))
	f.Add(targetIndex(targets, Error{}), []byte(`{"code":"x","params":{"a":[1,[2,{"b":null}]],"c":1e308}}`))
	f.Fuzz(func(t *testing.T, idx uint8, data []byte) {
		tg := targets[int(idx)%len(targets)]
		var v reflect.Value
		var err error
		limit := 4*uint64(len(data)) + decodeAllocSlack
		if !tg.serverDecoded {
			limit = clientDecodeRatio*uint64(len(data)) + decodeAllocSlack
		}
		n := allocatedWithin(limit, func() {
			v = reflect.New(tg.typ) // a new value each time: decoding into a filled one reuses its slices
			err = DecodeInto(Envelope{Type: tg.msgType, Data: data}, v.Interface())
		})
		if n > limit {
			t.Fatalf("decoding %v allocated %d bytes for %d input bytes (limit %d)", tg.typ, n, len(data), limit)
		}
		if err != nil {
			var fe *FieldError
			var pe *Error
			if !errors.As(err, &fe) && !errors.As(err, &pe) {
				t.Fatalf("decoding %v: error %v (%T) is neither a *FieldError nor a *Error", tg.typ, err, err)
			}
			return
		}
		// An agent.send that passed has a payload for the relay to hand on: some JSON value, and not null (01 §8.14).
		if a, ok := v.Interface().(*AgentSend); ok {
			if !json.Valid(a.Payload) || bytes.Equal(bytes.TrimSpace(a.Payload), []byte("null")) {
				t.Fatalf("accepted an agent.send with the payload %q", a.Payload)
			}
		}
		// A decoded payload encodes, and its encoding is canonical: decoding and encoding it again gives the same
		// bytes (and passes validation again).
		b1, err := json.Marshal(v.Interface())
		if errors.Is(err, errTimeRange) {
			return // a time that parses but lies outside [0000, 9999] in UTC
		}
		if err != nil {
			t.Fatalf("encoding a decoded %v: %v", tg.typ, err)
		}
		v2 := reflect.New(tg.typ)
		if err := DecodeInto(Envelope{Type: tg.msgType, Data: b1}, v2.Interface()); err != nil {
			t.Fatalf("decoding the re-encoded %v %s: %v", tg.typ, b1, err)
		}
		b2, err := json.Marshal(v2.Interface())
		if err != nil || !bytes.Equal(b1, b2) {
			t.Fatalf("%v encoding is not canonical (%v):\n%s\n%s", tg.typ, err, b1, b2)
		}
		if p := findNull(jsonTree(t, b1), "$"); p != "" && !strings.Contains(p, ".params") && !strings.Contains(p, ".payload") {
			t.Fatalf("%v encodes null at %s: %s", tg.typ, p, b1)
		}
	})
}

// FuzzUnescapeKey checks the scanner's allocation-free key unescaping against encoding/json.
func FuzzUnescapeKey(f *testing.F) {
	for _, s := range []string{
		`subs`, `SUBS`, "su" + jsonEsc("0062") + "s", jsonEsc("0073") + jsonEsc("0075") + jsonEsc("0062") + jsonEsc("0073"),
		jsonEsc("d83d") + jsonEsc("de00"), jsonEsc("d800"), jsonEsc("d800") + jsonEsc("0061"), jsonEsc("dc00") + jsonEsc("d800"),
		`\"\\\/\b\f\n\r\t`, `\x`, `\` + "u12", "\xff\xfe",
		string(rune(0x017f)) + "ubs", string(rune(0x212a)), jsonEsc("017f") + "ubs", strings.Repeat(jsonEsc("0061"), 20),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var buf [64]byte
		got, ok := unescapeKey(raw, buf[:])
		var want string
		if err := json.Unmarshal(append(append([]byte{'"'}, raw...), '"'), &want); err != nil {
			return // not a valid string body: encoding/json rejects the whole message
		}
		if !ok {
			if len(want) <= len(buf)-utf8.UTFMax {
				t.Fatalf("unescapeKey(%q) failed; encoding/json gives %q", raw, want)
			}
			return
		}
		if string(got) != want {
			t.Fatalf("unescapeKey(%q) = %q; encoding/json gives %q", raw, got, want)
		}
	})
}
