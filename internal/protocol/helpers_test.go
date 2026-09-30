package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const fixtureDir = "testdata/v1"

// fixture is one golden file: a full envelope, named <type>[.<variant>].json (replies: ok.<request type>[.<variant>]).
type fixture struct {
	name string // file name without .json
	raw  []byte
	env  Envelope
}

func loadFixtures(t testing.TB, dir string) []fixture {
	t.Helper()
	fsys := os.DirFS(dir)
	paths, err := fs.Glob(fsys, "*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	for _, p := range paths {
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Fatal(err)
		}
		env, err := ParseEnvelope(raw)
		if err != nil {
			t.Fatalf("%s/%s: ParseEnvelope: %v", dir, p, err)
		}
		out = append(out, fixture{name: strings.TrimSuffix(p, ".json"), raw: raw, env: env})
	}
	return out
}

// fixturePayload returns the zero value of the payload type a fixture holds and the Registry indexes it covers.
//
// Replies are named ok.<request type>[.<variant>] and hold that request's Result; they cover the ok entry. Other
// fixtures start with their type and cover every entry of that type with the same payload type (pc.* has one per
// direction); where the directions differ (stats), the variant "client" or "server" picks the entry.
func fixturePayload(f fixture) (any, []int, error) {
	var covers []int
	for i, s := range Registry {
		if s.Type == f.env.Type {
			covers = append(covers, i)
		}
	}
	if len(covers) == 0 {
		return nil, nil, fmt.Errorf("type %q is not in the Registry", f.env.Type)
	}
	if f.env.Type == MessageTypeOK {
		var req Spec
		for _, s := range Registry {
			if s.Dir == DirClientToServer && s.Kind == KindRequest && s.Reply == MessageTypeOK &&
				(f.name == "ok."+string(s.Type) || strings.HasPrefix(f.name, "ok."+string(s.Type)+".")) &&
				len(s.Type) > len(req.Type) {
				req = s
			}
		}
		if req.Type == "" {
			return nil, nil, fmt.Errorf("%s: no request type in the name (want ok.<request type>[.<variant>])", f.name)
		}
		return req.Result, covers, nil
	}
	if f.name != string(f.env.Type) && !strings.HasPrefix(f.name, string(f.env.Type)+".") {
		return nil, nil, fmt.Errorf("%s: name does not start with its type %q", f.name, f.env.Type)
	}
	first := reflect.TypeOf(Registry[covers[0]].Payload)
	same := true
	for _, i := range covers {
		same = same && reflect.TypeOf(Registry[i].Payload) == first
	}
	if same {
		return Registry[covers[0]].Payload, covers, nil
	}
	variant, _, _ := strings.Cut(strings.TrimPrefix(f.name, string(f.env.Type)+"."), ".")
	dir := map[string]Direction{"client": DirClientToServer, "server": DirServerToClient}[variant]
	for _, i := range covers {
		if Registry[i].Dir == dir {
			return Registry[i].Payload, []int{i}, nil
		}
	}
	return nil, nil, fmt.Errorf("%s: type %q differs per direction; name it %s.client or %s.server",
		f.name, f.env.Type, f.env.Type, f.env.Type)
}

// decodeStrict decodes data into a new value of zero's type, rejecting unknown fields (so fixture typos fail).
func decodeStrict(data json.RawMessage, zero any) (reflect.Value, error) {
	v := reflect.New(reflect.TypeOf(zero))
	if len(data) == 0 {
		return v.Elem(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v.Interface()); err != nil {
		return reflect.Value{}, err
	}
	if dec.More() {
		return reflect.Value{}, fmt.Errorf("trailing data")
	}
	return v.Elem(), nil
}

// decodeLenient runs DecodeInto (the production path: limits, unknown fields ignored, Validate) into a new value of
// zero's type.
func decodeLenient(e Envelope, zero any) (reflect.Value, error) {
	v := reflect.New(reflect.TypeOf(zero))
	if err := DecodeInto(e, v.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return v.Elem(), nil
}

// jsonTree decodes b into a generic tree. Envelopes without data get data: {} ("absent means {}").
func jsonTree(t testing.TB, b []byte) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return v
}

func envelopeTree(t testing.TB, b []byte) any {
	t.Helper()
	v := jsonTree(t, b)
	if m, ok := v.(map[string]any); ok {
		if _, has := m["data"]; !has {
			m["data"] = map[string]any{}
		}
	}
	return v
}

// findNull returns the path of the first null in a JSON tree, or "".
func findNull(v any, path string) string {
	switch v := v.(type) {
	case nil:
		return path
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if p := findNull(v[k], path+"."+k); p != "" {
				return p
			}
		}
	case []any:
		for i, e := range v {
			if p := findNull(e, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

// jsonEsc returns the JSON escape \uXXXX for hex. It is spelled without the escape itself, so no editor or
// tool can turn it into the character it names and quietly change what a test exercises.
func jsonEsc(hex string) string { return `\` + "u" + hex }

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
