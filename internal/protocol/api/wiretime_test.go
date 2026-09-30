package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	utcPlus8  = time.FixedZone("UTC+8", 8*3600)
	utcMinus5 = time.FixedZone("UTC-5", -5*3600)
)

// TestWireTimeMarshal: any location and precision encodes as UTC with exactly three fractional digits (truncated) and
// a Z suffix, 24 characters between the quotes; a year outside 0–9999 (in UTC) is an error.
func TestWireTimeMarshal(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole second in UTC", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), `"2026-09-30T12:00:00.000Z"`},
		{"milliseconds", time.Date(2026, 10, 3, 19, 22, 5, 114_000_000, time.UTC), `"2026-10-03T19:22:05.114Z"`},
		{"one millisecond", time.Date(2026, 10, 3, 19, 22, 5, 1_000_000, time.UTC), `"2026-10-03T19:22:05.001Z"`},
		{"truncated, not rounded", time.Date(2026, 10, 4, 3, 22, 5, 114_999_999, utcPlus8), `"2026-10-03T19:22:05.114Z"`},
		{"below a millisecond", time.Date(2026, 10, 3, 19, 22, 5, 999_999, time.UTC), `"2026-10-03T19:22:05.000Z"`},
		{"west of UTC, into the next year", time.Date(2026, 12, 31, 22, 30, 0, 999_999_999, utcMinus5),
			`"2027-01-01T03:30:00.999Z"`},
		{"zero", time.Time{}, `"0001-01-01T00:00:00.000Z"`},
		{"zero in another location", time.Time{}.In(utcPlus8), `"0001-01-01T00:00:00.000Z"`},
		{"year 0", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), `"0000-01-01T00:00:00.000Z"`},
		{"last millisecond of 9999", time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
			`"9999-12-31T23:59:59.999Z"`},
		{"year 10000 locally, 9999 in UTC", time.Date(10000, 1, 1, 2, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)),
			`"9999-12-31T21:00:00.000Z"`},
	}
	for _, c := range cases {
		b, err := json.Marshal(WireTime(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(b) != c.want {
			t.Errorf("%s: %v encodes as %s, want %s", c.name, c.in, b, c.want)
		}
		if s := strings.Trim(string(b), `"`); !wireTime.MatchString(s) || len(s) != 24 {
			t.Errorf("%s: %s is not the fixed 24-character form", c.name, b)
		}
		if got, want := WireTime(c.in).String(), strings.Trim(c.want, `"`); got != want {
			t.Errorf("%s: String() = %q, want %q", c.name, got, want)
		}
		// The pointer encodes the same (the method has a value receiver).
		w := WireTime(c.in)
		if pb, err := json.Marshal(&w); err != nil || string(pb) != c.want {
			t.Errorf("%s: pointer encodes as %s, %v", c.name, pb, err)
		}
	}

	for _, bad := range []time.Time{
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(-1, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 0, 0, 0, utcMinus5), // 10000-01-01T04:00:00Z
	} {
		if b, err := json.Marshal(WireTime(bad)); err == nil {
			t.Errorf("%v encoded as %s, want an error", bad, b)
		}
		if WireTime(bad).String() == "" {
			t.Errorf("%v: String() is empty", bad)
		}
	}
}

// TestWireTimeFields: as a struct field, WireTime keeps the fixed form, and omitzero drops a zero time in any
// location, as it does for time.Time.
func TestWireTimeFields(t *testing.T) {
	type doc struct {
		At      WireTime `json:"at"`
		Expires WireTime `json:"expiresAt,omitzero"`
	}
	for _, zero := range []time.Time{{}, time.Time{}.Local(), time.Time{}.In(utcPlus8)} {
		b, err := json.Marshal(doc{At: WireTime(zero), Expires: WireTime(zero)})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"at":"0001-01-01T00:00:00.000Z"}`; string(b) != want {
			t.Errorf("zero in %v: got %s, want %s", zero.Location(), b, want)
		}
	}
	at := time.Date(2026, 9, 30, 20, 0, 0, 123_456_789, utcPlus8)
	b, err := json.Marshal(doc{At: WireTime(at), Expires: WireTime(at.Add(24 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"at":"2026-09-30T12:00:00.123Z","expiresAt":"2026-10-01T12:00:00.123Z"}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

// truncatedUTC is what a reader gets back for t: t in UTC, cut to whole milliseconds, built from its fields rather
// than with time.Truncate.
func truncatedUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), u.Second(),
		u.Nanosecond()/int(time.Millisecond)*int(time.Millisecond), time.UTC)
}

// checkRoundTrip encodes t as a WireTime and decodes the result into both a WireTime and a time.Time (what DTO fields
// hold): both equal t cut to the millisecond, and the decoded WireTime encodes to the same bytes.
func checkRoundTrip(t *testing.T, in time.Time) {
	t.Helper()
	b, err := json.Marshal(WireTime(in))
	if err != nil {
		t.Fatalf("marshal %v: %v", in, err)
	}
	want := truncatedUTC(in)
	var w WireTime
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("unmarshal %s into WireTime: %v", b, err)
	}
	if !time.Time(w).Equal(want) {
		t.Errorf("%v → %s → %v, want %v", in, b, time.Time(w), want)
	}
	again, err := json.Marshal(w)
	if err != nil || string(again) != string(b) {
		t.Errorf("%s re-encodes as %s, %v", b, again, err)
	}
	var tt time.Time
	if err := json.Unmarshal(b, &tt); err != nil {
		t.Fatalf("unmarshal %s into time.Time: %v", b, err)
	}
	if !tt.Equal(want) {
		t.Errorf("%s decodes into time.Time as %v, want %v", b, tt, want)
	}
}

// TestWireTimeRoundTrip: encode, decode and encode again; readers accept any RFC 3339 form and refuse anything else.
func TestWireTimeRoundTrip(t *testing.T) {
	for _, in := range []time.Time{
		time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 4, 3, 22, 5, 114_999_999, utcPlus8),
		time.Date(2026, 12, 31, 22, 30, 0, 999_999_999, utcMinus5),
		time.Date(2026, 10, 3, 19, 22, 5, 999_999, time.UTC),
		time.Date(2026, 10, 3, 19, 22, 5, 0, time.Local),
		time.Unix(1790712000, 123_456_789),
		{},
		time.Date(0, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
	} {
		checkRoundTrip(t, in)
	}

	// A DTO field round-trips the same way.
	at := time.Date(2026, 10, 4, 3, 22, 5, 114_999_999, utcPlus8)
	b, err := json.Marshal(Room{ID: "lounge", CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	var r Room
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if !r.CreatedAt.Equal(truncatedUTC(at)) {
		t.Errorf("Room.CreatedAt: %v → %s → %v", at, b, r.CreatedAt)
	}

	// Readers accept any RFC 3339 form and keep the value as sent; encoding gives the fixed form.
	for in, want := range map[string]string{
		`"2026-10-01T12:00:00Z"`:                `"2026-10-01T12:00:00.000Z"`,
		`"2026-10-01T14:00:00.5+02:00"`:         `"2026-10-01T12:00:00.500Z"`,
		`"2026-10-01T12:00:00.123456789Z"`:      `"2026-10-01T12:00:00.123Z"`,
		`"2026-10-01T07:00:00.000999-05:00"`:    `"2026-10-01T12:00:00.000Z"`,
		`"2026-10-01T12:00:00.000Z"`:            `"2026-10-01T12:00:00.000Z"`,
		`"2026-10-04T03:22:05.114999999+08:00"`: `"2026-10-03T19:22:05.114Z"`,
	} {
		var w WireTime
		if err := json.Unmarshal([]byte(in), &w); err != nil {
			t.Errorf("unmarshal %s: %v", in, err)
			continue
		}
		var tt time.Time
		if err := json.Unmarshal([]byte(in), &tt); err != nil {
			t.Fatal(err)
		}
		if !time.Time(w).Equal(tt) || time.Time(w).Nanosecond() != tt.Nanosecond() {
			t.Errorf("%s decodes as %v, want %v as sent", in, time.Time(w), tt)
		}
		if got, err := json.Marshal(w); err != nil || string(got) != want {
			t.Errorf("%s re-encodes as %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{`"2026-10-01 12:00:00Z"`, `"2026-10-01"`, `"2026-10-01T12:00:00"`, `""`, `"now"`,
		`1790712000`, `true`, `{}`} {
		var w WireTime
		if err := json.Unmarshal([]byte(bad), &w); err == nil {
			t.Errorf("unmarshal %s: no error, got %v", bad, time.Time(w))
		}
	}

	// null leaves the value as it was, as with time.Time.
	w := WireTime(at)
	if err := json.Unmarshal([]byte(`null`), &w); err != nil || !time.Time(w).Equal(at) {
		t.Errorf("null: %v, value %v", err, time.Time(w))
	}
}

// TestWireTimeMatchesProtocolFixtures: 01's golden fixtures (internal/protocol/testdata/v1, frozen per release)
// hold the signaling form of timestamps. Every one of them already has WireTime's form, and decoding and re-encoding
// it through WireTime gives the same string, so REST and signaling timestamps are one format (03 §3.3, 01 §5). The
// fixtures are read as files: this package's tests don't depend on the protocol package's Go API.
func TestWireTimeMatchesProtocolFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "v1", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var root any
		if err := json.Unmarshal(b, &root); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		walkValue(root, "", func(path string, v any) {
			s, ok := v.(string)
			if !ok {
				return
			}
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				return
			}
			n++
			if !wireTime.MatchString(s) {
				t.Errorf("%s %s = %q: 01's form differs from WireTime's", filepath.Base(f), path, s)
			}
			q, _ := json.Marshal(s)
			var w WireTime
			if err := json.Unmarshal(q, &w); err != nil {
				t.Errorf("%s %s: %v", filepath.Base(f), path, err)
				return
			}
			if got, err := json.Marshal(w); err != nil || string(got) != string(q) {
				t.Errorf("%s %s: WireTime re-encodes %s as %s, %v", filepath.Base(f), path, q, got, err)
			}
		})
	}
	if n < 5 {
		t.Fatalf("found %d timestamps in %d protocol fixtures; want at least 5 (did internal/protocol/testdata/v1 move?)",
			n, len(files))
	}
}

// walkValue calls fn for v and every value inside it, with its path.
func walkValue(v any, path string, fn func(path string, v any)) {
	fn(path, v)
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			walkValue(e, path+"."+k, fn)
		}
	case []any:
		for i, e := range x {
			walkValue(e, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

// FuzzWireTimeRoundTrip: for any instant and zone offset, encoding either fails (a year outside 0–9999 in UTC) or
// gives the fixed form, which decodes to the instant cut to the millisecond and encodes to the same bytes again.
func FuzzWireTimeRoundTrip(f *testing.F) {
	f.Add(int64(1790712000), int64(123_456_789), int32(0))
	f.Add(int64(1790712000), int64(999_999_999), int32(8*3600))
	f.Add(int64(1798756200), int64(999_999), int32(-5*3600))
	f.Add(int64(-62135596800), int64(0), int32(0))           // year 1, the zero time
	f.Add(int64(-62167219200), int64(1), int32(0))           // year 0
	f.Add(int64(253402300799), int64(999_999_999), int32(0)) // the last second of 9999
	f.Add(int64(253402300800), int64(0), int32(0))           // year 10000
	f.Add(int64(-62167219201), int64(0), int32(-3600))       // year -1
	f.Fuzz(func(t *testing.T, sec, nsec int64, offset int32) {
		in := time.Unix(sec, nsec).In(time.FixedZone("fuzz", int(offset%(26*3600))))
		b, err := json.Marshal(WireTime(in))
		if y := in.UTC().Year(); y < 0 || y > 9999 {
			if err == nil {
				t.Fatalf("%v (UTC year %d) encoded as %s", in, y, b)
			}
			return
		}
		if err != nil {
			t.Fatalf("marshal %v: %v", in, err)
		}
		if s := string(b); len(s) != 26 || !wireTime.MatchString(s[1:len(s)-1]) {
			t.Fatalf("%v encoded as %s, not the fixed form", in, b)
		}
		checkRoundTrip(t, in)
	})
}
