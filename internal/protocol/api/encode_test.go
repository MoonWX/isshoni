package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	timeType      = reflect.TypeFor[time.Time]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
)

// dtoTypes returns every struct type of the package that the golden cases reach, sorted by name;
// TestGoldenCoversEveryDTO makes sure that is every exported struct type.
func dtoTypes() []reflect.Type {
	pkg := reflect.TypeFor[Error]().PkgPath()
	seen := map[reflect.Type]bool{}
	var out []reflect.Type
	for _, c := range goldenCases() {
		walkTypes(reflect.TypeOf(c.v), func(rt reflect.Type) {
			if rt.Kind() == reflect.Struct && rt.PkgPath() == pkg && rt.Name() != "" && !seen[rt] {
				seen[rt] = true
				out = append(out, rt)
			}
		})
	}
	slices.SortFunc(out, func(a, b reflect.Type) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// TestTimeFieldsHaveMarshalers: every DTO with a time.Time field, or with a slice or map field that may not be
// omitted, has a value-receiver MarshalJSON (encode.go), so the 03 §3.3 timestamp form and the no-null rule hold for
// every encoder and for values as well as pointers.
func TestTimeFieldsHaveMarshalers(t *testing.T) {
	checked := 0
	for _, rt := range dtoTypes() {
		var needs []string
		for i := range rt.NumField() {
			f := rt.Field(i)
			omit := strings.Contains(f.Tag.Get("json"), ",omit")
			switch {
			case f.Type == timeType:
				needs = append(needs, f.Name+" (time.Time)")
			case (f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Map) && !omit:
				needs = append(needs, f.Name+" (never null)")
			}
		}
		if len(needs) == 0 {
			continue
		}
		checked++
		if !rt.Implements(marshalerType) {
			t.Errorf("%s needs a value-receiver MarshalJSON in encode.go for %s", rt.Name(), strings.Join(needs, ", "))
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d types", checked)
	}
}

var wireTime = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// TestGoldenTimestampsUTCMilliseconds: every string in the golden files that parses as RFC 3339 has the one wire form
// of 03 §3.3, "2026-10-01T12:00:00.000Z", and is exactly what WireTime encodes for it.
func TestGoldenTimestampsUTCMilliseconds(t *testing.T) {
	n := 0
	for _, c := range goldenCases() {
		walkJSON(t, c.name, func(path string, v any) {
			s, ok := v.(string)
			if !ok {
				return
			}
			parsed, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return
			}
			n++
			if !wireTime.MatchString(s) {
				t.Errorf("%s: %s = %q is not RFC 3339 UTC with milliseconds", c.name, path, s)
			}
			if b, err := json.Marshal(WireTime(parsed)); err != nil || string(b) != `"`+s+`"` {
				t.Errorf("%s: %s = %q, but WireTime encodes it as %s (%v)", c.name, path, s, b, err)
			}
		})
	}
	if n < 50 {
		t.Fatalf("found only %d timestamps in the golden files", n)
	}
}

// TestTimestampsAreUTCMilliseconds: a timestamp in any location and with any precision encodes as UTC with exactly
// three fractional digits (truncated), a zero optional timestamp stays absent in any location, and decoding accepts
// any RFC 3339 form.
func TestTimestampsAreUTCMilliseconds(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := time.Date(2026, 10, 4, 3, 22, 5, 114_999_999, loc) // 2026-10-03T19:22:05.114999999Z
	cases := []struct {
		v    any
		want string
	}{
		{ResetLink{ExpiresAt: at}, `"expiresAt":"2026-10-03T19:22:05.114Z"`},
		{InviteInfo{ExpiresAt: at}, `"expiresAt":"2026-10-03T19:22:05.114Z"`},
		{AuditEntry{At: at}, `"at":"2026-10-03T19:22:05.114Z"`},
		{&AuditEntry{At: at}, `"at":"2026-10-03T19:22:05.114Z"`},
		{User{CreatedAt: at}, `"createdAt":"2026-10-03T19:22:05.114Z"`},
		{TLSInfo{NotAfter: at}, `"notAfter":"2026-10-03T19:22:05.114Z"`},
		{Room{CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, `"createdAt":"2026-10-01T12:00:00.000Z"`},
		{Room{}, `"createdAt":"0001-01-01T00:00:00.000Z"`}, // a required timestamp is never absent
		// Nested: the outer MarshalJSON leaves the inner ones in charge.
		{OpsDashboard{Doctor: &DoctorSummary{RanAt: at}}, `"ranAt":"2026-10-03T19:22:05.114Z"`},
		{RoomResponse{Room: Room{CreatedAt: at}}, `"createdAt":"2026-10-03T19:22:05.114Z"`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%T: %v", c.v, err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%T encodes %s, want it to contain %s", c.v, b, c.want)
		}
	}

	// omitzero: a zero time is absent whatever its location (time.Time{}.Local() is not the zero struct).
	for _, zero := range []time.Time{{}, time.Time{}.Local(), time.Time{}.In(loc)} {
		b, err := json.Marshal(SessionInfo{ExpiresAt: zero, LastSeenAt: zero})
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "expiresAt") || strings.Contains(s, "lastSeenAt") {
			t.Errorf("zero optional timestamps in %v are present: %s", zero.Location(), s)
		}
	}

	if _, err := json.Marshal(InviteInfo{ExpiresAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Error("year 10000 encoded without an error")
	}

	var got InviteInfo
	if err := json.Unmarshal([]byte(`{"expiresAt":"2026-10-04T03:22:05.114999999+08:00"}`), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(at) {
		t.Errorf("decoded %v, want %v", got.ExpiresAt, at)
	}
}

// findNull returns the path of the first null in v, or "".
func findNull(v any, path string) string {
	switch x := v.(type) {
	case nil:
		return path
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if p := findNull(x[k], path+"."+k); p != "" {
				return p
			}
		}
	case []any:
		for i, e := range x {
			if p := findNull(e, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

// populate fills v for the populated no-null test: every slice gets one element and every pointer a value, each filled
// recursively; maps stay nil and leaves keep their zero values.
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
		s := reflect.MakeSlice(v.Type(), 1, 1)
		populate(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		populate(p.Elem(), depth+1)
		v.Set(p)
	}
}

// TestNoNullZeroValues: the zero value of every DTO encodes without a single null, and so does a populated one (one
// element per slice, every pointer set), with nil slices and maps inside: a producer that forgets to allocate an
// empty list still sends [] (doc.go). The one exception is AuditPage.nextBefore, null on the last page.
func TestNoNullZeroValues(t *testing.T) {
	check := func(what string, rt reflect.Type, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s %s: %v", what, rt.Name(), err)
		}
		var tree any
		if err := json.Unmarshal(b, &tree); err != nil {
			t.Fatalf("%s %s: %v", what, rt.Name(), err)
		}
		p := findNull(tree, "")
		if p == ".nextBefore" && rt == reflect.TypeFor[AuditPage]() {
			return
		}
		if p != "" {
			t.Errorf("%s %s encodes null at %s: %s", what, rt.Name(), p, b)
		}
	}
	for _, rt := range dtoTypes() {
		zero := reflect.New(rt)
		check("zero", rt, zero.Elem().Interface())
		check("zero pointer to", rt, zero.Interface())
		full := reflect.New(rt)
		populate(full.Elem(), 0)
		check("populated", rt, full.Elem().Interface())
	}

	// The cases the review found: an empty approval queue, an empty invite list and an audit row without detail.
	for _, c := range []struct {
		v    any
		want string
	}{
		{ApprovalsResponse{}, `{"pending":[]}`},
		{InvitesResponse{}, `{"invites":[]}`},
		{AuditEntry{}, `"detail":{}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%T encodes %s, want %s", c.v, b, c.want)
		}
	}
}

// TestMarshalKeepsCallersEscaping: the MarshalJSON methods don't change HTML escaping; the calling encoder decides, as
// if the methods did not exist (json.Marshal escapes "&", an Encoder with SetEscapeHTML(false) keeps it).
func TestMarshalKeepsCallersEscaping(t *testing.T) {
	r := DoctorReport{Checks: []DoctorCheck{{Fix: "sudo tee x && sudo sysctl --system"}}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`x \u0026\u0026 sudo`)) {
		t.Errorf("json.Marshal did not escape: %s", b)
	}
	if got := encodeGolden(t, r); !bytes.Contains(got, []byte(`x && sudo`)) {
		t.Errorf("SetEscapeHTML(false) escaped: %s", got)
	}
}
