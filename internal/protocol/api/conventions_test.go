package api

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestJSONNamesCamelCase walks every DTO type by reflection (the golden cases reach every exported struct, see
// TestGoldenCoversEveryDTO) and checks each field's JSON name.
func TestJSONNamesCamelCase(t *testing.T) {
	pkg := reflect.TypeOf(Error{}).PkgPath()
	checked := 0
	for _, c := range goldenCases() {
		walkTypes(reflect.TypeOf(c.v), func(rt reflect.Type) {
			if rt.Kind() != reflect.Struct || rt.PkgPath() != pkg {
				return
			}
			names := map[string]string{}
			for i := range rt.NumField() {
				f := rt.Field(i)
				where := rt.Name() + "." + f.Name
				if !f.IsExported() {
					t.Errorf("%s: unexported field in a DTO", where)
					continue
				}
				if f.Anonymous {
					t.Errorf("%s: embedded field in a DTO; name it", where)
				}
				tag, ok := f.Tag.Lookup("json")
				if !ok {
					t.Errorf("%s has no json tag (it would be sent as %q)", where, f.Name)
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !camelCase.MatchString(name) {
					t.Errorf("%s: JSON name %q is not camelCase", where, name)
				}
				for o := range strings.SplitSeq(opts, ",") {
					if o != "" && o != "omitempty" && o != "omitzero" {
						t.Errorf("%s: unexpected json option %q", where, o)
					}
				}
				if prev, dup := names[name]; dup {
					t.Errorf("%s and %s.%s share the JSON name %q", where, rt.Name(), prev, name)
				}
				names[name] = f.Name
				checked++
			}
		})
	}
	if checked == 0 {
		t.Fatal("checked no fields")
	}
}

// TestStandardLibraryOnly: the package imports only the standard library (03 §2.1), so tygo, the Go client and every
// server package can use it. Standard library paths have no dot in their first element.
func TestStandardLibraryOnly(t *testing.T) {
	for _, p := range importPaths(t, parseSources(t, ".")) {
		first, _, _ := strings.Cut(p, "/")
		if strings.Contains(first, ".") {
			t.Errorf("imports %q: only the standard library is allowed", p)
		}
	}
}

var enumValue = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)

// TestEnumConstants checks every typed string constant: named <Type><Value> (tygo's union rule), a lowercase value,
// and unique within its type.
func TestEnumConstants(t *testing.T) {
	seen := map[string]map[string]string{}
	count := map[string]int{}
	for _, c := range stringConsts(t, parseSources(t, ".")) {
		if c.Type == "" {
			continue
		}
		count[c.Type]++
		if !strings.HasPrefix(c.Name, c.Type) || len(c.Name) == len(c.Type) {
			t.Errorf("%s: a %s constant must be named %s<Value>", c.Name, c.Type, c.Type)
		}
		if !enumValue.MatchString(c.Value) {
			t.Errorf("%s = %q: enum values are lowercase", c.Name, c.Value)
		}
		if seen[c.Type] == nil {
			seen[c.Type] = map[string]string{}
		}
		if prev, dup := seen[c.Type][c.Value]; dup {
			t.Errorf("%s and %s both have the value %q", prev, c.Name, c.Value)
		}
		seen[c.Type][c.Value] = c.Name
	}
	for typ, n := range count {
		if n < 2 {
			t.Errorf("%s has %d constant; tygo needs at least 2 for a union", typ, n)
		}
	}
}

// TestValueLists checks that CloudProviders and NATKinds list every declared constant, in declaration order: 05's
// fix texts, 06's site anchors, netx and doctor rely on them being complete.
func TestValueLists(t *testing.T) {
	var providers []CloudProvider
	var nats []NATKind
	for _, c := range stringConsts(t, parseSources(t, ".")) {
		switch c.Type {
		case "CloudProvider":
			providers = append(providers, CloudProvider(c.Value))
		case "NATKind":
			nats = append(nats, NATKind(c.Value))
		}
	}
	if got := CloudProviders(); !slices.Equal(got, providers) {
		t.Errorf("CloudProviders() = %v\nwant %v", got, providers)
	}
	if got := NATKinds(); !slices.Equal(got, nats) {
		t.Errorf("NATKinds() = %v\nwant %v", got, nats)
	}
	if len(providers) != 13 || len(nats) != 6 {
		t.Errorf("got %d providers and %d NAT kinds, want 13 (04 §13.3) and 6 (04 §7.4)", len(providers), len(nats))
	}
}

func TestWireTime(t *testing.T) {
	if !WireTime(time.Time{}).IsZero() {
		t.Error("WireTime(zero) is not zero")
	}
	in := time.Date(2026, 10, 1, 14, 0, 0, 123_456_789, time.FixedZone("CEST", 2*60*60))
	got := WireTime(in)
	if want := time.Date(2026, 10, 1, 12, 0, 0, 123_000_000, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("WireTime = %v, want %v in UTC", got, want)
	}
	b, err := json.Marshal(struct {
		At time.Time `json:"at"`
	}{got})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"at":"2026-10-01T12:00:00.123Z"}` {
		t.Errorf("encoded %s", b)
	}
}

// TestUnknownFieldsIgnored: a newer peer's extra fields decode without error (03 §12.1: additive changes only).
func TestUnknownFieldsIgnored(t *testing.T) {
	var got LoginRequest
	in := `{"username": "Alex", "password": "correct horse battery", "rememberMe": true, "extra": {"a": [1]}}`
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	if got != (LoginRequest{Username: "Alex", Password: "correct horse battery"}) {
		t.Errorf("got %+v", got)
	}
}
