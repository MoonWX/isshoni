package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
)

const fixtureDir = "testdata"

// payloadKinds are the kinds that have a payload type in M2, with the type's zero value and its Decode function.
var payloadKinds = map[string]struct {
	zero   any
	decode func(json.RawMessage) (any, error)
}{
	KindPlaybackStop:  {PlaybackStop{}, func(p json.RawMessage) (any, error) { return DecodePlaybackStop(p) }},
	KindPlaybackState: {PlaybackState{}, func(p json.RawMessage) (any, error) { return DecodePlaybackState(p) }},
}

// fixture is one golden file: a full agent.send or agent.recv envelope, named <message type>.<relay kind>.json.
type fixture struct {
	name string
	raw  []byte
	env  protocol.Envelope
}

func loadFixtures(t testing.TB) []fixture {
	t.Helper()
	fsys := os.DirFS(fixtureDir)
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
		env, err := protocol.ParseEnvelope(raw)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, fixture{name: strings.TrimSuffix(p, ".json"), raw: raw, env: env})
	}
	return out
}

// relayed returns the kind and payload of an agent.send or agent.recv, decoded along the production path
// (protocol.Decode: limits, then agent.send's Validate), and a function that encodes the message again with another
// payload.
func relayed(t *testing.T, env protocol.Envelope) (kind string, payload json.RawMessage, encode func(json.RawMessage) []byte) {
	t.Helper()
	marshal := func(v any) []byte {
		out, err := protocol.Marshal(env.Type, env.ID, env.Re, v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return out
	}
	switch env.Type {
	case protocol.MessageTypeAgentSend:
		s, err := protocol.Decode[protocol.AgentSend](env)
		if err != nil {
			t.Fatalf("agent.send: %v", err)
		}
		return s.Kind, s.Payload, func(p json.RawMessage) []byte { s.Payload = p; return marshal(s) }
	case protocol.MessageTypeAgentRecv:
		r, err := protocol.Decode[protocol.AgentRecv](env)
		if err != nil {
			t.Fatalf("agent.recv: %v", err)
		}
		return r.Kind, r.Payload, func(p json.RawMessage) []byte { r.Payload = p; return marshal(r) }
	}
	t.Fatalf("type %q: want agent.send or agent.recv", env.Type)
	return "", nil, nil
}

// TestFixturesRoundTrip (M2 01 §14): every fixture is an agent.send or agent.recv that protocol decodes, its payload
// decodes strictly (no unknown field) into its kind's type and through the kind's Decode function to the same value,
// and the message encodes again to the fixture.
func TestFixturesRoundTrip(t *testing.T) {
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			kind, payload, encode := relayed(t, f.env)
			if want := string(f.env.Type) + "." + kind; f.name != want {
				t.Errorf("named %s.json, want %s.json", f.name, want)
			}
			k, ok := payloadKinds[kind]
			if !ok {
				t.Fatalf("kind %q has no payload type", kind)
			}
			strict := reflect.New(reflect.TypeOf(k.zero))
			dec := json.NewDecoder(bytes.NewReader(payload))
			dec.DisallowUnknownFields()
			if err := dec.Decode(strict.Interface()); err != nil {
				t.Fatalf("strict decode into %T: %v", k.zero, err)
			}
			got, err := k.decode(payload)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, strict.Elem().Interface()) {
				t.Errorf("Decode gave %#v, the strict decode %#v", got, strict.Elem().Interface())
			}
			again, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if len(again) > protocol.MaxAgentPayloadBytes {
				t.Errorf("payload of %d bytes, over the relay's %d", len(again), protocol.MaxAgentPayloadBytes)
			}
			out := encode(again)
			if want, got := jsonTree(t, f.raw), jsonTree(t, out); !reflect.DeepEqual(want, got) {
				t.Errorf("round trip differs\nfixture: %s\nencoded: %s", mustJSON(t, want), out)
			}
			if bytes.Contains(again, []byte("null")) {
				t.Errorf("encoded payload holds null: %s", again)
			}
		})
	}
}

// TestEveryPayloadKindHasFixtures: each kind with a payload type has a fixture of its agent.send and its agent.recv.
func TestEveryPayloadKindHasFixtures(t *testing.T) {
	have := map[string]bool{}
	for _, f := range loadFixtures(t) {
		have[f.name] = true
	}
	for kind := range payloadKinds {
		for _, typ := range []protocol.MessageType{protocol.MessageTypeAgentSend, protocol.MessageTypeAgentRecv} {
			if name := string(typ) + "." + kind; !have[name] {
				t.Errorf("no fixture %s/%s.json", fixtureDir, name)
			}
		}
	}
}

// TestPlaybackStopWire pins playback.stop's payload as 03 §12.2 and 01 §11.11 give it: {v: 1, roomId}.
func TestPlaybackStopWire(t *testing.T) {
	got := string(mustJSON(t, PlaybackStop{V: PlaybackStopVersion, RoomID: "lounge"}))
	if want := `{"v":1,"roomId":"lounge"}`; got != want {
		t.Errorf("PlaybackStop encodes as %s, want %s", got, want)
	}
	got = string(mustJSON(t, PlaybackState{Playing: true, Browser: BrowserFirefox, OS: protocol.ClientOSWindows}))
	if want := `{"playing":true,"browser":"firefox","os":"windows"}`; got != want {
		t.Errorf("PlaybackState encodes as %s, want %s", got, want)
	}
	// Absent fields are written, not omitted: false and "" are values a receiver reads.
	got = string(mustJSON(t, PlaybackState{}))
	if want := `{"playing":false,"browser":"","os":""}`; got != want {
		t.Errorf("zero PlaybackState encodes as %s, want %s", got, want)
	}
}

// parseSource parses the package's non-test sources.
func parseSource(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := fs.Glob(os.DirFS("."), "*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// constGroups returns the package's exported constants, by const declaration, in source order: name → value
// (string constants unquoted).
func constGroups(t *testing.T) [][][2]string {
	t.Helper()
	var groups [][][2]string
	for _, f := range parseSource(t) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			var group [][2]string
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !name.IsExported() || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok {
						t.Fatalf("%s is not a literal", name.Name)
					}
					value := lit.Value
					if lit.Kind == token.STRING {
						value, _ = strconv.Unquote(lit.Value)
					}
					group = append(group, [2]string{name.Name, value})
				}
			}
			groups = append(groups, group)
		}
	}
	return groups
}

// prefixed returns the constants whose names start with prefix, from all groups, as name → value.
func prefixed(t *testing.T, prefix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, g := range constGroups(t) {
		for _, c := range g {
			if strings.HasPrefix(c[0], prefix) {
				out[c[0]] = c[1]
			}
		}
	}
	return out
}

// TestKinds (M2 01 §11.11): the kinds are exactly M2's two and the four reserved ones, under their M1 names (01
// §8.14), distinct, and each is a kind agent.send accepts ([a-z.]{1,32}).
func TestKinds(t *testing.T) {
	want := map[string]string{
		"KindPlaybackStop":  "playback.stop",
		"KindPlaybackState": "playback.state",
		"KindShareRequest":  "share.request",  // reserved, M4
		"KindShareStatus":   "share.status",   // reserved, M4
		"KindExclusionsGet": "exclusions.get", // reserved, M4's agent spec
		"KindExclusionsSet": "exclusions.set", // reserved, M4's agent spec
	}
	got := prefixed(t, "Kind")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kind constants %v\nwant           %v", got, want)
	}
	seen := map[string]string{}
	for name, kind := range got {
		if other, dup := seen[kind]; dup {
			t.Errorf("%s and %s are both %q", name, other, kind)
		}
		seen[kind] = name
		send := protocol.AgentSend{To: "c_k3v9q2m7xw4pa8d1", Kind: kind, Payload: json.RawMessage(`{}`)}
		if err := send.Validate(); err != nil {
			t.Errorf("%s = %q: agent.send refuses it: %v", name, kind, err)
		}
	}
	// Only playback.stop and playback.state have payloads in M2; the reserved kinds have none (01 D16).
	var withPayload []string
	for kind := range payloadKinds {
		withPayload = append(withPayload, kind)
	}
	slices.Sort(withPayload)
	if want := []string{KindPlaybackState, KindPlaybackStop}; !slices.Equal(withPayload, want) {
		t.Errorf("kinds with a payload type %v, want %v", withPayload, want)
	}
}

// TestBrowserFamilies: the families are 05 §5.4's three plus other, and PlaybackState.Validate accepts exactly them.
func TestBrowserFamilies(t *testing.T) {
	want := map[string]string{
		"BrowserChrome": "chrome", "BrowserEdge": "edge", "BrowserFirefox": "firefox", "BrowserOther": "other",
	}
	got := prefixed(t, "Browser")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("browser constants %v, want %v", got, want)
	}
	for _, b := range got {
		if err := (PlaybackState{Browser: b}).Validate(); err != nil {
			t.Errorf("browser %q refused: %v", b, err)
		}
	}
	// M1's client.browser has safari too: a tab sends it as other (05 treats both alike, 05 §5.9 step 3).
	for _, b := range []string{"safari", "Chrome", "chromium", "edge ", "zz_not_a_family"} {
		if err := (PlaybackState{Browser: b}).Validate(); err == nil {
			t.Errorf("browser %q accepted", b)
		}
	}
}

// TestTSUnions: tygo (enum_style union) turns each const group of 2 or more exported constants that share a
// capitalized name prefix into a TS union type named after it. relay.gen.ts has exactly two, on purpose: Kind and
// Browser (the type of PlaybackState.browser there, by its tstype tag).
func TestTSUnions(t *testing.T) {
	var unions []string
	for _, g := range constGroups(t) {
		if len(g) < 2 {
			continue
		}
		prefix := ""
		for n := len(g[0][0]); n > 0 && prefix == ""; n-- {
			if !slices.ContainsFunc(g[1:], func(c [2]string) bool { return !strings.HasPrefix(c[0], g[0][0][:n]) }) {
				prefix = g[0][0][:n]
			}
		}
		if prefix != "" {
			unions = append(unions, prefix)
		}
	}
	slices.Sort(unions)
	if want := []string{"Browser", "Kind"}; !slices.Equal(unions, want) {
		t.Errorf("tygo would emit the TS unions %v, want %v", unions, want)
	}
	sf, _ := reflect.TypeFor[PlaybackState]().FieldByName("Browser")
	if tag := sf.Tag.Get("tstype"); tag != "Browser" {
		t.Errorf("PlaybackState.Browser has tstype %q, want Browser", tag)
	}
}

// TestImports: the package imports only the standard library and internal/protocol (01 §4; depguard's
// protocol-relay rule checks the same in lint).
func TestImports(t *testing.T) {
	for _, f := range parseSource(t) {
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != "github.com/MoonWX/isshoni/internal/protocol" {
				t.Errorf("imports %s: only the standard library and internal/protocol are allowed", path)
			}
		}
	}
}

// errField checks that err is a refusal of the Decode functions and returns its field and reason.
func errField(t *testing.T, err error) string {
	t.Helper()
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("error %v does not wrap ErrInvalidPayload", err)
	}
	var fe *protocol.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v holds no *protocol.FieldError", err)
	}
	return fe.Field + ":" + fe.Reason
}

type decodeCase struct {
	name, payload, want string // want: "" or field:reason
}

func runDecode(t *testing.T, decode func(json.RawMessage) (any, error), cases []decodeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decode(json.RawMessage(c.payload))
			got := ""
			if err != nil {
				got = errField(t, err)
			}
			if got != c.want {
				t.Errorf("payload %s: got %q, want %q", c.payload, got, c.want)
			}
		})
	}
}

// notObjects are payloads agent.send accepts that are no object: each kind refuses them as a whole.
var notObjects = []string{`null`, `[]`, `[{"v":1}]`, `"playback"`, `1`, `true`, ``, ` `}

func TestDecodePlaybackStop(t *testing.T) {
	cases := []decodeCase{
		{"valid", `{"v":1,"roomId":"lounge"}`, ""},
		{"key order and spaces", ` { "roomId" : "r_9-x" , "v" : 1 } `, ""},
		{"unknown fields ignored", `{"v":1,"roomId":"lounge","from":"x","extra":{"a":[1,null]}}`, ""},
		{"keys match like encoding/json", `{"V":1,"RoomID":"lounge"}`, ""},
		{"v missing", `{"roomId":"lounge"}`, "v:required"},
		{"v 0", `{"v":0,"roomId":"lounge"}`, "v:required"},
		{"v 2 (a newer sender)", `{"v":2,"roomId":"lounge"}`, "v:invalid"},
		{"v negative", `{"v":-1,"roomId":"lounge"}`, "v:invalid"},
		{"v a string", `{"v":"1","roomId":"lounge"}`, "v:invalid"},
		{"v a fraction", `{"v":1.5,"roomId":"lounge"}`, "v:invalid"},
		{"a refused key is named as the field", `{"V":"1","roomId":"lounge"}`, "v:invalid"},
		{"a folded key is named as the field", `{"v":1,"ROOMID":true}`, "roomId:invalid"},
		{"room missing", `{"v":1}`, "roomId:required"},
		{"room empty", `{"v":1,"roomId":""}`, "roomId:required"},
		{"room too long", `{"v":1,"roomId":"` + strings.Repeat("r", protocol.MaxOpaqueIDLen+1) + `"}`, "roomId:too_long"},
		{"room longest", `{"v":1,"roomId":"` + strings.Repeat("r", protocol.MaxOpaqueIDLen) + `"}`, ""},
		{"room charset", `{"v":1,"roomId":"lou nge"}`, "roomId:invalid"},
		{"room a number", `{"v":1,"roomId":7}`, "roomId:invalid"},
		{"not valid JSON", `{"v":1,`, "payload:invalid"},
		{"deeper than protocol allows", `{"v":1,"roomId":"lounge","x":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`, "payload:invalid"},
	}
	for _, p := range notObjects {
		cases = append(cases, decodeCase{"not an object " + p, p, "payload:invalid"})
	}
	runDecode(t, func(p json.RawMessage) (any, error) { return DecodePlaybackStop(p) }, cases)

	got, err := DecodePlaybackStop(json.RawMessage(`{"v":1,"roomId":"lounge"}`))
	if err != nil || got != (PlaybackStop{V: 1, RoomID: "lounge"}) {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDecodePlaybackState(t *testing.T) {
	cases := []decodeCase{
		{"valid", `{"playing":true,"browser":"chrome","os":"windows"}`, ""},
		{"edge", `{"playing":true,"browser":"edge","os":"windows"}`, ""},
		{"firefox stopped", `{"playing":false,"browser":"firefox","os":"windows"}`, ""},
		{"other on macos", `{"playing":true,"browser":"other","os":"macos"}`, ""},
		{"unknown os accepted", `{"playing":true,"browser":"chrome","os":"plan9"}`, ""},
		{"os missing", `{"playing":true,"browser":"chrome"}`, ""},
		{"playing missing is false", `{"browser":"chrome","os":"windows"}`, ""},
		{"unknown fields ignored", `{"v":1,"playing":true,"browser":"chrome","os":"windows","tab":{"id":3}}`, ""},
		{"keys match like encoding/json", `{"PLAYING":true,"Browser":"chrome","Os":"windows"}`, ""},
		{"browser missing", `{"playing":true,"os":"windows"}`, "browser:required"},
		{"browser empty", `{"playing":true,"browser":"","os":"windows"}`, "browser:required"},
		{"safari is sent as other", `{"playing":true,"browser":"safari","os":"macos"}`, "browser:invalid"},
		{"browser case", `{"playing":true,"browser":"Chrome","os":"windows"}`, "browser:invalid"},
		{"browser a number", `{"playing":true,"browser":1,"os":"windows"}`, "browser:invalid"},
		{"playing a string", `{"playing":"yes","browser":"chrome","os":"windows"}`, "playing:invalid"},
		{"playing a number", `{"playing":1,"browser":"chrome","os":"windows"}`, "playing:invalid"},
		{"a refused key is named as the field", `{"PLAYING":"x","browser":"chrome"}`, "playing:invalid"},
		{"os a number", `{"playing":true,"browser":"chrome","os":10}`, "os:invalid"},
		{"not valid JSON", `{"playing":true`, "payload:invalid"},
		{"a key longer than protocol allows", `{"` + strings.Repeat("k", 2000) + `":1,"browser":"chrome"}`, "payload:invalid"},
	}
	for _, p := range notObjects {
		cases = append(cases, decodeCase{"not an object " + p, p, "payload:invalid"})
	}
	runDecode(t, func(p json.RawMessage) (any, error) { return DecodePlaybackState(p) }, cases)

	got, err := DecodePlaybackState(json.RawMessage(`{"playing":true,"browser":"edge","os":"windows"}`))
	if want := (PlaybackState{Playing: true, Browser: BrowserEdge, OS: protocol.ClientOSWindows}); err != nil || got != want {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
}

// knownFields are the only fields an error of the Decode functions names: relay payloads are never logged (01
// §11.12), so no part of a payload reaches an error message.
var knownFields = []string{"payload", "v", "roomId", "playing", "browser", "os"}

// checkRefusal checks that err is a refusal whose message is exactly what the package writes: the kind and a known
// field and reason, nothing taken from the payload.
func checkRefusal(t *testing.T, kind string, err error) {
	t.Helper()
	var fe *protocol.FieldError
	if !errors.Is(err, ErrInvalidPayload) || !errors.As(err, &fe) {
		t.Fatalf("%s: error %v is no refusal of this package", kind, err)
	}
	if !slices.Contains(knownFields, fe.Field) {
		t.Fatalf("%s: error names the field %q", kind, fe.Field)
	}
	if want := fmt.Sprintf("relay: invalid payload: %s: protocol: field %s: %s", kind, fe.Field, fe.Reason); err.Error() != want {
		t.Fatalf("%s: error %q, want %q", kind, err.Error(), want)
	}
}

func TestErrorsHoldNoPayload(t *testing.T) {
	const marker = "zzSecretMarker"
	for _, p := range []string{
		`{"v":"` + marker + `","roomId":"lounge"}`,
		`{"v":1,"roomId":"` + marker + ` "}`,
		`{"v":1,"roomId":"lounge",` + marker + `}`,
		`"` + marker + `"`,
		`{"v":12345678901234567890123,"roomId":"lounge"}`,
	} {
		_, err := DecodePlaybackStop(json.RawMessage(p))
		checkRefusal(t, KindPlaybackStop, err)
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "123456") {
			t.Errorf("error %q holds a value of the payload %s", err, p)
		}
	}
	for _, p := range []string{
		`{"playing":"` + marker + `","browser":"chrome"}`,
		`{"playing":true,"browser":"` + marker + `"}`,
		`{"playing":true,"browser":"chrome","os":["` + marker + `"]}`,
		`[` + marker + `]`,
	} {
		_, err := DecodePlaybackState(json.RawMessage(p))
		checkRefusal(t, KindPlaybackState, err)
		if strings.Contains(err.Error(), marker) {
			t.Errorf("error %q holds a value of the payload %s", err, p)
		}
	}
}

// FuzzDecode: whatever a sender relays, the Decode functions don't panic, refuse with a message that holds nothing
// of the payload, and what they accept is valid and survives an encode and a decode unchanged.
func FuzzDecode(f *testing.F) {
	for _, fx := range loadFixtures(f) {
		var m struct {
			Data struct {
				Payload json.RawMessage `json:"payload"`
			} `json:"data"`
		}
		if err := json.Unmarshal(fx.raw, &m); err != nil {
			f.Fatal(err)
		}
		f.Add([]byte(m.Data.Payload))
	}
	for _, p := range notObjects {
		f.Add([]byte(p))
	}
	f.Add([]byte(`{"v":2,"roomId":"lounge","playing":true,"browser":"safari","os":"macos"}`))
	f.Add([]byte(`{"V":1,"ROOMID":"lounge","Playing":true,"BROWSER":"edge"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		check := func(kind string, v any, err error, again func(json.RawMessage) (any, error)) {
			if err != nil {
				checkRefusal(t, kind, err)
				return
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("%s: encode %#v: %v", kind, v, err)
			}
			w, err := again(b)
			if err != nil || !reflect.DeepEqual(v, w) {
				t.Fatalf("%s: %#v encodes as %s, which decodes to %#v, %v", kind, v, b, w, err)
			}
		}
		stop, err := DecodePlaybackStop(data)
		check(KindPlaybackStop, stop, err, func(p json.RawMessage) (any, error) { return DecodePlaybackStop(p) })
		state, err := DecodePlaybackState(data)
		check(KindPlaybackState, state, err, func(p json.RawMessage) (any, error) { return DecodePlaybackState(p) })
	})
}

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

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
