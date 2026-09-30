package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// minimumFixtures is the fixture set 01 §14.3 requires for M1.
var minimumFixtures = strings.Fields(`
	hello.web hello.resume hello.bearer welcome welcome.resumed
	ping pong room.join ok.room.join room.leave room.state
	room.event.share.started room.event.share.stopped room.event.participant.left
	share.start ok.share.start share.update share.stop
	pc.offer.pub pc.answer.pub pc.offer.sub pc.answer.sub pc.ice pc.ice.end
	pc.restart pc.close subscribe.update ok.subscribe.update subscribe.status
	quality.hint.codec quality.hint.viewers caps.update stats.client stats.watch
	stats.server invalidate server.shutdown error.request error.pc error.share.codec
	error.session agent.send ok.agent.send agent.recv`)

func TestMinimumFixtureSet(t *testing.T) {
	for _, name := range minimumFixtures {
		if _, err := os.Stat(filepath.Join(fixtureDir, name+".json")); err != nil {
			t.Errorf("fixture %s.json of the 01 §14.3 minimum set: %v", name, err)
		}
	}
}

// TestFixturesRoundTrip decodes every fixture strictly into its registered type, re-encodes it with Marshal and
// compares the JSON trees (01 §19). It also runs the production decode path (DecodeInto: limits, Validate), which
// must accept every fixture and give the same value.
func TestFixturesRoundTrip(t *testing.T) {
	fixtures := loadFixtures(t, fixtureDir)
	if len(fixtures) < len(minimumFixtures) {
		t.Fatalf("found %d fixtures in %s, want at least %d", len(fixtures), fixtureDir, len(minimumFixtures))
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			zero, _, err := fixturePayload(f)
			if err != nil {
				t.Fatal(err)
			}
			strict, err := decodeStrict(f.env.Data, zero)
			if err != nil {
				t.Fatalf("strict decode into %T: %v", zero, err)
			}
			lenient, err := decodeLenient(f.env, zero)
			if err != nil {
				t.Fatalf("DecodeInto %T: %v", zero, err)
			}
			if !reflect.DeepEqual(strict.Interface(), lenient.Interface()) {
				t.Errorf("DecodeInto changed the value:\nstrict  %#v\nlenient %#v", strict.Interface(), lenient.Interface())
			}
			out, err := Marshal(f.env.Type, f.env.ID, f.env.Re, strict.Interface())
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if want, got := envelopeTree(t, f.raw), envelopeTree(t, out); !reflect.DeepEqual(want, got) {
				t.Errorf("round trip differs\nfixture: %s\nencoded: %s", mustJSON(t, want), mustJSON(t, got))
			}
			if p := findNull(jsonTree(t, out), "$"); p != "" {
				t.Errorf("encoded message has null at %s: %s", p, out)
			}
		})
	}
}

// TestRegistryCoverage: every Registry entry has at least one fixture.
func TestRegistryCoverage(t *testing.T) {
	covered := make([]bool, len(Registry))
	for _, f := range loadFixtures(t, fixtureDir) {
		_, covers, err := fixturePayload(f)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, i := range covers {
			covered[i] = true
		}
	}
	for i, ok := range covered {
		if !ok {
			s := Registry[i]
			t.Errorf("Registry entry %s (dir %d) has no fixture in %s", s.Type, s.Dir, fixtureDir)
		}
	}
}

// TestFixturesHaveNoNull: no golden fixture contains null (arrays are always present, 01 §5).
func TestFixturesHaveNoNull(t *testing.T) {
	for _, f := range loadFixtures(t, fixtureDir) {
		if p := findNull(jsonTree(t, f.raw), "$"); p != "" {
			t.Errorf("%s: null at %s", f.name, p)
		}
	}
}

// TestFixtureEnvelopes checks the envelope rules of the fixtures themselves: requests have an id, replies a re,
// notifications neither (an error is a reply exactly when it has a re), and each error's retryable flag matches the
// catalog.
func TestFixtureEnvelopes(t *testing.T) {
	for _, f := range loadFixtures(t, fixtureDir) {
		var kinds []MsgKind
		for _, s := range Registry {
			if s.Type == f.env.Type {
				kinds = append(kinds, s.Kind)
			}
		}
		switch kinds[0] {
		case KindRequest:
			if f.env.ID == "" || f.env.Re != "" {
				t.Errorf("%s: a request needs an id and no re", f.name)
			}
		case KindReply:
			if f.env.ID != "" || (f.env.Re == "" && f.env.Type != MessageTypeError) {
				t.Errorf("%s: a reply needs a re and no id", f.name)
			}
		case KindNotification:
			if f.env.ID != "" || f.env.Re != "" {
				t.Errorf("%s: a notification has neither id nor re", f.name)
			}
		}
		if f.env.Type != MessageTypeError {
			continue
		}
		e, err := Decode[Error](f.env)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if want := NewError(e.Code, e.Scope).Retryable; e.Retryable != want {
			t.Errorf("%s: retryable = %v, the catalog says %v", f.name, e.Retryable, want)
		}
		if e.Scope == ErrorScopeRequest && f.env.Re == "" {
			t.Errorf("%s: scope request needs a re (01 §12.1)", f.name)
		}
	}
}

// TestUnknownFieldTolerance adds unknown keys (with nested objects, arrays and nulls) to every object of every
// fixture, envelope included. Decoding must succeed and give the same message; unknown keys survive only inside
// opaque values (relay payloads, error params), and stripping them restores the fixture (01 §5, §19).
func TestUnknownFieldTolerance(t *testing.T) {
	const key = "zzUnknownField"
	unknown := map[string]any{"a": []any{1, map[string]any{"b": nil}, "c"}, "d": map[string]any{"e": true}}
	var inject func(v any) any
	inject = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				v[k] = inject(e)
			}
			v[key] = unknown
		case []any:
			for i, e := range v {
				v[i] = inject(e)
			}
		}
		return v
	}
	var strip func(v any) any
	strip = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			delete(v, key)
			for k, e := range v {
				v[k] = strip(e)
			}
		case []any:
			for i, e := range v {
				v[i] = strip(e)
			}
		}
		return v
	}
	for _, f := range loadFixtures(t, fixtureDir) {
		t.Run(f.name, func(t *testing.T) {
			zero, _, err := fixturePayload(f)
			if err != nil {
				t.Fatal(err)
			}
			tree := jsonTree(t, f.raw)
			noisy := mustJSON(t, inject(tree))
			env, err := ParseEnvelope(noisy)
			if err != nil {
				t.Fatalf("ParseEnvelope with unknown fields: %v", err)
			}
			v, err := decodeLenient(env, zero)
			if err != nil {
				t.Fatalf("DecodeInto with unknown fields: %v\n%s", err, noisy)
			}
			out, err := Marshal(env.Type, env.ID, env.Re, v.Interface())
			if err != nil {
				t.Fatal(err)
			}
			if want, got := envelopeTree(t, f.raw), strip(envelopeTree(t, out)); !reflect.DeepEqual(want, got) {
				t.Errorf("unknown fields changed the message\nfixture: %s\ndecoded: %s", mustJSON(t, want), mustJSON(t, got))
			}
		})
	}
	if _, ok := Lookup("room.teleport", DirClientToServer|DirServerToClient); ok {
		t.Error("Lookup of an unknown type succeeded")
	}
}

// TestCompatFixtures decodes every frozen fixture of earlier releases (testdata/compat/<version>, 01 §14.3) with the
// current code. Decoding must succeed, and re-encoding must keep every key of the fixture with an equal value, which
// enforces "additive only": removing or renaming a field, or changing its type, fails here.
func TestCompatFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "compat", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Skip("no compat snapshots yet (task release:compat creates them before a release)")
	}
	for _, dir := range dirs {
		for _, f := range loadFixtures(t, dir) {
			t.Run(filepath.Base(dir)+"/"+f.name, func(t *testing.T) {
				zero, _, err := fixturePayload(f)
				if err != nil {
					t.Fatal(err)
				}
				v, err := decodeLenient(f.env, zero)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				out, err := Marshal(f.env.Type, f.env.ID, f.env.Re, v.Interface())
				if err != nil {
					t.Fatal(err)
				}
				if p := jsonSubset(envelopeTree(t, f.raw), envelopeTree(t, out), "$"); p != "" {
					t.Errorf("re-encoding lost or changed %s\nfixture: %s\nencoded: %s", p, f.raw, out)
				}
			})
		}
	}
}

// jsonSubset reports the path of the first value of want (a decoded JSON tree) that got lacks or holds differently,
// or "" if got contains all of want. Objects may gain keys at every level, including inside array elements (a new
// field of ShareInfo appears in every element of room.state's shares); arrays keep their length and order.
func jsonSubset(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return path + "." + k
			}
			if p := jsonSubset(wv, gv, path+"."+k); p != "" {
				return p
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return path
		}
		for i := range w {
			if p := jsonSubset(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
		return ""
	}
	if !reflect.DeepEqual(want, got) {
		return path
	}
	return ""
}

func TestJSONSubset(t *testing.T) {
	for _, c := range []struct {
		want, got, path string
	}{
		{`{"a":1}`, `{"a":1}`, ""},
		{`{"a":1}`, `{"a":1,"b":[]}`, ""},
		{`{"shares":[{"id":"s1"},{"id":"s2"}]}`, `{"shares":[{"id":"s1","tags":[]},{"id":"s2","tags":[]}]}`, ""},
		{`{"a":[[{"x":1}]]}`, `{"a":[[{"x":1,"y":2}]]}`, ""},
		{`{"a":null}`, `{"a":null}`, ""},
		{`{"a":null}`, `{}`, "$.a"},
		{`{"a":1}`, `{}`, "$.a"},
		{`{"a":1}`, `{"a":"1"}`, "$.a"},
		{`{"a":{"b":1}}`, `{"a":[]}`, "$.a"},
		{`{"a":[1,2]}`, `{"a":[1,2,3]}`, "$.a"},
		{`{"a":[1,2]}`, `{"a":[2,1]}`, "$.a[0]"},
		{`{"a":[{"id":"s1"},{"id":"s2"}]}`, `{"a":[{"id":"s1"},{"id":"s3","x":1}]}`, "$.a[1].id"},
		{`{"a":[{"id":"s1"}]}`, `{"a":[{"x":1}]}`, "$.a[0].id"},
	} {
		if p := jsonSubset(jsonTree(t, []byte(c.want)), jsonTree(t, []byte(c.got)), "$"); p != c.path {
			t.Errorf("jsonSubset(%s, %s) = %q, want %q", c.want, c.got, p, c.path)
		}
	}
}

// TestFixturesAreFormatted keeps the fixtures readable and diff-friendly: indented with spaces, one object per
// file, ending in a newline.
func TestFixturesAreFormatted(t *testing.T) {
	for _, f := range loadFixtures(t, fixtureDir) {
		if !json.Valid(f.raw) || !strings.HasSuffix(string(f.raw), "}\n") || strings.Contains(string(f.raw), "\t") {
			t.Errorf("%s: want JSON indented with spaces and ending in a newline", f.name)
		}
	}
}
