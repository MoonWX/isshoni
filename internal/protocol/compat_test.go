package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The compat test (01 §14.3, §19). Before each final release, `task release:compat VERSION=X.Y.Z` copies testdata/v1
// to testdata/compat/X.Y.Z and keeps the two newest such snapshots (06 §7.2); the release-prep PR commits them, and
// release.yml refuses to release without one that equals testdata/v1 (06 §9.3). From then on every fixture of every
// snapshot must still decode with the current code, and encode again with each of its keys and values: a field may
// be added within a protocol version, but not removed, renamed or given another type (01 §14.1).

// compatDir holds the snapshots, one directory per final release.
const compatDir = "testdata/compat"

// releaseVersion is a final release's version, as release.yml accepts it for a tag (06 §9.1): X.Y.Z, no leading
// zeros, no prerelease. Prereleases get no snapshot.
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// TestCompatFixtures decodes every fixture of every snapshot in testdata/compat with the current code.
func TestCompatFixtures(t *testing.T) {
	snapshots := compatSnapshots(t)
	if len(snapshots) == 0 {
		t.Skip("no compat snapshots yet: task release:compat makes one in the release-prep PR of each final release")
	}
	for _, dir := range snapshots {
		files := snapshotFiles(t, dir)
		if len(files) == 0 {
			t.Errorf("%s holds no fixtures: run task release:compat VERSION=%s again", dir, filepath.Base(dir))
		}
		for _, name := range files {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			fixtureName := strings.TrimSuffix(name, ".json")
			t.Run(filepath.Base(dir)+"/"+fixtureName, func(t *testing.T) {
				if err := checkCompat(fixtureName, raw); err != nil {
					t.Error(err)
				}
			})
		}
	}
}

// compatSnapshots returns the snapshot directories in testdata/compat (none if it does not exist). Each entry must
// be what task release:compat makes: a directory named after a final release, not a link to one (a link to
// testdata/v1 would follow every change and check nothing). Hidden entries such as .DS_Store are skipped.
func compatSnapshots(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(compatDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case !e.IsDir() || !releaseVersion.MatchString(e.Name()):
			t.Errorf("%s/%s is not a snapshot: task release:compat makes one directory per final release X.Y.Z",
				compatDir, e.Name())
		default:
			dirs = append(dirs, filepath.Join(compatDir, e.Name()))
		}
	}
	return dirs
}

// snapshotFiles returns the fixture files of a snapshot directory: regular *.json files, as task release:compat
// copies them. Anything else is reported, hidden files aside.
func snapshotFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json"):
			t.Errorf("%s/%s is not a fixture: a snapshot is a copy of the *.json files of %s", dir, e.Name(), fixtureDir)
		default:
			names = append(names, e.Name())
		}
	}
	return names
}

// checkCompat decodes one frozen fixture (named <type>[.<variant>] like the files in testdata/v1) along the
// production path: ParseEnvelope, then DecodeInto with its limits and Validate, into the type the Registry names
// today. It then encodes the value with Marshal and returns a *compatLoss if the encoding lacks a key of the fixture
// or holds another value under it. New keys in the encoding are fine: that is an additive change.
func checkCompat(name string, raw []byte) error {
	env, err := ParseEnvelope(raw)
	if err != nil {
		return fmt.Errorf("ParseEnvelope: %w", err)
	}
	zero, _, err := fixturePayload(fixture{name: name, raw: raw, env: env})
	if err != nil {
		return err
	}
	v, err := decodeLenient(env, zero)
	if err != nil {
		return fmt.Errorf("decode into %T: %w", zero, err)
	}
	out, err := Marshal(env.Type, env.ID, env.Re, v.Interface())
	if err != nil {
		return fmt.Errorf("Marshal: %w", err)
	}
	want, err := parseEnvelopeTree(raw)
	if err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	got, err := parseEnvelopeTree(out)
	if err != nil {
		return fmt.Errorf("encoding: %w", err)
	}
	if p := jsonSubset(want, got, "$"); p != "" {
		return &compatLoss{path: p, fixture: raw, encoded: out}
	}
	return nil
}

// compatLoss is checkCompat's error for a fixture value that the current code does not encode again.
type compatLoss struct {
	path    string // the first such value, as jsonSubset reports it
	fixture []byte
	encoded []byte
}

func (e *compatLoss) Error() string {
	return fmt.Sprintf("re-encoding lost or changed %s, which breaks clients of that release (01 §14.1: additive "+
		"changes only)\nfixture: %s\nencoded: %s", e.path, e.fixture, e.encoded)
}

// TestCompatCatchesBreakingChanges shows that checkCompat enforces "additive only" (01 §14.1) at every object and
// value of every fixture, taking testdata/v1 as the snapshot (it is the one the next release freezes):
//
//   - a removed field. checkCompat sees only the snapshot and what the current types make of it, so removing a field
//     from a type and adding a key that no type has to the snapshot look the same to it: the snapshot holds a key
//     that the decode drops. An added key stands in for every field of every type, at every level, and the check
//     must report exactly that key;
//   - a renamed field: a key of the snapshot under another name. The check must fail;
//   - a changed type: a value of another JSON kind (a string for a number, an object for an array, ...). The check
//     must fail, at the decode or at the comparison.
//
// Opaque values are not fields of a type and are exempt: agent.send's and agent.recv's payload, and an error's
// params. They keep what they are given (TestUnknownFieldTolerance), and the test checks that an added key survives
// there, so the exemption cannot cover more than these values.
func TestCompatCatchesBreakingChanges(t *testing.T) {
	const added = "zzRemovedField"
	opaqueRoots := map[MessageType]string{
		MessageTypeAgentSend: "$.data.payload",
		MessageTypeAgentRecv: "$.data.payload",
		MessageTypeError:     "$.data.params",
	}
	var removed, renamed, retyped int
	for _, f := range loadFixtures(t, fixtureDir) {
		t.Run(f.name, func(t *testing.T) {
			if err := checkCompat(f.name, f.raw); err != nil {
				t.Fatalf("the fixture as it is: %v", err)
			}
			root := opaqueRoots[f.env.Type]
			opaque := func(path string) bool {
				return root != "" && (path == root || strings.HasPrefix(path, root+".") || strings.HasPrefix(path, root+"["))
			}
			var nodes []jsonNode
			collectJSONNodes(jsonTree(t, f.raw), "$", nil, &nodes)
			for _, n := range nodes {
				obj, isObj := n.value.(map[string]any)
				if isObj {
					mutated := editJSON(t, f.raw, n.steps, func(v any) any {
						v.(map[string]any)[added] = json.Number("1")
						return v
					})
					err := checkCompat(f.name, mutated)
					if opaque(n.path) {
						if err != nil {
							t.Errorf("a key added to the opaque value %s did not survive: %v", n.path, err)
						}
						continue
					}
					var loss *compatLoss
					if !errors.As(err, &loss) || loss.path != n.path+"."+added {
						t.Errorf("a field removed from the type at %s: got %v, want the loss of %s.%s",
							n.path, err, n.path, added)
					}
					removed++
				}
				if opaque(n.path) {
					continue
				}
				if isObj {
					for _, k := range slices.Sorted(maps.Keys(obj)) {
						mutated := editJSON(t, f.raw, n.steps, func(v any) any {
							m := v.(map[string]any)
							m[k+"Renamed"] = m[k]
							delete(m, k)
							return v
						})
						if checkCompat(f.name, mutated) == nil {
							t.Errorf("the field %s.%s renamed to %sRenamed passes the compat check", n.path, k, k)
						}
						renamed++
					}
				}
				if len(n.steps) == 0 {
					continue
				}
				other := otherJSONKind(n.value)
				mutated := editJSON(t, f.raw, n.steps, func(any) any { return other })
				if checkCompat(f.name, mutated) == nil {
					t.Errorf("%s as %s instead of %s passes the compat check", n.path, mustJSON(t, other),
						mustJSON(t, n.value))
				}
				retyped++
			}
		})
	}
	t.Logf("simulated %d removed, %d renamed and %d retyped fields", removed, renamed, retyped)
	if removed == 0 || renamed == 0 || retyped == 0 {
		t.Error("the fixtures gave nothing to change")
	}
}

// jsonNode is one value of a JSON tree: its path in jsonSubset's notation, and the steps that lead to it from the
// root (a string for an object member, an int for an array element).
type jsonNode struct {
	path  string
	steps []any
	value any
}

// collectJSONNodes appends v and every value inside it to out, parents first, object members in key order.
func collectJSONNodes(v any, path string, steps []any, out *[]jsonNode) {
	*out = append(*out, jsonNode{path: path, steps: slices.Clone(steps), value: v})
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			collectJSONNodes(v[k], path+"."+k, append(steps, k), out)
		}
	case []any:
		for i, e := range v {
			collectJSONNodes(e, fmt.Sprintf("%s[%d]", path, i), append(steps, i), out)
		}
	}
}

// editJSON returns raw with the value at steps replaced by edit's result. edit gets a fresh copy of the value.
func editJSON(t *testing.T, raw []byte, steps []any, edit func(any) any) []byte {
	t.Helper()
	var at func(v any, steps []any) any
	at = func(v any, steps []any) any {
		if len(steps) == 0 {
			return edit(v)
		}
		switch s := steps[0].(type) {
		case string:
			m := v.(map[string]any)
			m[s] = at(m[s], steps[1:])
		case int:
			a := v.([]any)
			a[s] = at(a[s], steps[1:])
		}
		return v
	}
	return mustJSON(t, at(jsonTree(t, raw), steps))
}

// otherJSONKind returns a value of another JSON kind than v.
func otherJSONKind(v any) any {
	switch v.(type) {
	case string:
		return json.Number("7")
	case json.Number:
		return "7"
	case bool:
		return "true"
	case []any:
		return map[string]any{}
	default: // an object (the fixtures hold no null)
		return []any{}
	}
}

// jsonSubset reports the path of the first value of want (a decoded JSON tree) that got lacks or holds differently,
// or "" if got contains all of want. Objects may gain keys at every level, including inside array elements (a new
// field of ShareInfo appears in every element of room.state's shares); arrays keep their length and order. Members
// are compared in key order, so the path reported is always the same.
func jsonSubset(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			gv, ok := g[k]
			if !ok {
				return path + "." + k
			}
			if p := jsonSubset(w[k], gv, path+"."+k); p != "" {
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
		{`{"b":1,"a":1,"c":1}`, `{}`, "$.a"},
	} {
		if p := jsonSubset(jsonTree(t, []byte(c.want)), jsonTree(t, []byte(c.got)), "$"); p != c.path {
			t.Errorf("jsonSubset(%s, %s) = %q, want %q", c.want, c.got, p, c.path)
		}
	}
}

// TestReleaseVersion: the snapshot names the test accepts are the versions release.yml tags and task
// release:compat writes.
func TestReleaseVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"0.0.1": true, "0.1.0": true, "1.2.3": true, "10.20.30": true,
		"": false, "1.2": false, "1.2.3.4": false, "01.2.3": false, "1.02.3": false, "1.2.03": false,
		"1.2.3-rc.1": false, "0.0.0-dev+0123456789ab": false, "v1.2.3": false, "1..3": false, " 1.2.3": false,
	} {
		if got := releaseVersion.MatchString(v); got != want {
			t.Errorf("releaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}
