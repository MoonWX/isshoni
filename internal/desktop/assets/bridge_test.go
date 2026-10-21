package assets

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	bridgePrefix = "window.__ISSHONI_DESKTOP__=Object.freeze("
	bridgeSuffix = ");"
)

// TestBridgeJSExample is §5.2's example, byte for byte.
func TestBridgeJSExample(t *testing.T) {
	got, err := BridgeJS(BridgeConfig{
		V: 1, Transport: "wails",
		App:       AppFacts{Version: "0.2.0", OS: "windows", OSBuild: 26100, Arch: "amd64"},
		Server:    &ServerFacts{ID: "k3m9p2qx", Origin: "https://watch.example.com", Name: "Friends"},
		ServerIDs: []string{"k3m9p2qx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `window.__ISSHONI_DESKTOP__=Object.freeze({"v":1,"transport":"wails","app":{"version":"0.2.0","os":"windows",` +
		`"osBuild":26100,"arch":"amd64"},"server":{"id":"k3m9p2qx","origin":"https://watch.example.com","name":"Friends"},` +
		`"serverIds":["k3m9p2qx"],"dev":false});`
	if string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestBridgeJSEscapes(t *testing.T) {
	name := "</script><script>alert(1)</script> &    <!-- -->"
	got, err := BridgeJS(BridgeConfig{V: 1, Server: &ServerFacts{Name: name, Origin: "https://a.example"}})
	if err != nil {
		t.Fatal(err)
	}
	checkBridgeJS(t, got)
	c := decodeBridgeJS(t, got)
	if c.Server == nil || c.Server.Name != name {
		t.Fatalf("round trip: %+v", c.Server)
	}
}

// checkBridgeJS checks that body is one statement whose JSON can't end the script, start a comment or break the line.
func checkBridgeJS(t *testing.T, body []byte) {
	t.Helper()
	s := string(body)
	if !strings.HasPrefix(s, bridgePrefix) || !strings.HasSuffix(s, bridgeSuffix) {
		t.Fatalf("not a bridge.js body: %q", s)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, bridgePrefix), bridgeSuffix)
	if i := strings.IndexAny(inner, "<>&  \n\r"); i >= 0 {
		t.Fatalf("raw %q in the JSON: %q", inner[i:i+1], inner)
	}
}

func decodeBridgeJS(t *testing.T, body []byte) BridgeConfig {
	t.Helper()
	inner := strings.TrimSuffix(strings.TrimPrefix(string(body), bridgePrefix), bridgeSuffix)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inner), &raw); err != nil {
		t.Fatalf("bridge.js JSON: %v", err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"app", "dev", "server", "serverIds", "transport", "v"}; !slices.Equal(keys, want) {
		t.Fatalf("bridge.js keys %v, want %v", keys, want)
	}
	if string(raw["serverIds"]) == "null" {
		t.Fatal("serverIds is null, want an array")
	}
	var c BridgeConfig
	dec := json.NewDecoder(strings.NewReader(inner))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("bridge.js JSON: %v", err)
	}
	return c
}

// TestBridgeConfigHoldsNoSecret checks the shape of bridge.js: only the fields of §5.1, and none that could carry a
// token, a password or another secret (§5.2, §5.8).
func TestBridgeConfigHoldsNoSecret(t *testing.T) {
	want := map[string][]string{
		"BridgeConfig": {"v", "transport", "app", "server", "serverIds", "dev"},
		"AppFacts":     {"version", "os", "osBuild", "arch"},
		"ServerFacts":  {"id", "origin", "name"},
	}
	for _, v := range []any{BridgeConfig{}, AppFacts{}, ServerFacts{}} {
		typ := reflect.TypeOf(v)
		var names []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			names = append(names, tag)
			lower := strings.ToLower(f.Name + " " + tag)
			for _, bad := range []string{"token", "secret", "password", "refresh", "cookie", "key"} {
				if strings.Contains(lower, bad) {
					t.Errorf("%s.%s looks like a secret", typ.Name(), f.Name)
				}
			}
			switch f.Type.Kind() {
			case reflect.String, reflect.Int, reflect.Bool, reflect.Struct, reflect.Pointer, reflect.Slice:
			default:
				t.Errorf("%s.%s has kind %s", typ.Name(), f.Name, f.Type.Kind())
			}
		}
		if !slices.Equal(names, want[typ.Name()]) {
			t.Errorf("%s fields %v, want %v", typ.Name(), names, want[typ.Name()])
		}
	}
}

// FuzzBridgeJS feeds arbitrary server names, origins and ids through bridge.js: the body stays one safe statement,
// the JSON round-trips, and it holds the fields of §5.1 only.
func FuzzBridgeJS(f *testing.F) {
	for _, s := range []string{"Friends", "</script>", "<!--", "a b", "&amp;", "\x00", "\"};alert(1);//",
		"isa_0123456789", "\xff\xfe"} {
		f.Add(s, "https://watch.example.com", "k3m9p2qx")
	}
	f.Fuzz(func(t *testing.T, name, origin, id string) {
		body, err := BridgeJS(BridgeConfig{
			V: 1, Transport: "wails", App: AppFacts{Version: "0.2.0", OS: "windows"},
			Server:    &ServerFacts{ID: id, Origin: origin, Name: name},
			ServerIDs: []string{id},
		})
		if err != nil {
			t.Fatal(err)
		}
		checkBridgeJS(t, body)
		c := decodeBridgeJS(t, body)
		if utf8.ValidString(name) && c.Server.Name != name {
			t.Fatalf("name %q came back as %q", name, c.Server.Name)
		}
		if utf8.ValidString(origin) && c.Server.Origin != origin {
			t.Fatalf("origin %q came back as %q", origin, c.Server.Origin)
		}
	})
}
