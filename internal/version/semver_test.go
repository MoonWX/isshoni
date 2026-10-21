package version

import (
	"errors"
	"slices"
	"testing"
)

// TestBelow: the hub's minClientVersion table (moved with the code from internal/server/signal, whose
// TestVersionBelow it was), then the cases M2 02 §13.2 adds.
func TestBelow(t *testing.T) {
	for _, tc := range []struct {
		v, floor  string
		below, ok bool
	}{
		// The hub's table, unchanged.
		{"0.1.0", "0.2.0", true, true},
		{"0.2.0", "0.2.0", false, true},
		{"v0.2.1", "0.2.0", false, true},
		{"0.10.0", "0.9.0", false, true},
		{"1.0.0", "0.99.99", false, true},
		{"0.2.0-rc.1", "0.2.0", true, true},
		{"0.2.0-rc.2", "0.2.0-rc.10", true, true},
		{"0.2.0-alpha", "0.2.0-1", false, true},
		{"0.2.0-rc", "0.2.0-rc.1", true, true},
		{"0.2.0+build.5", "0.2.0", false, true},
		{"dev", "0.2.0", true, true},
		{"", "0.2.0", true, true},
		{"01.2.0", "0.2.0", true, true},
		{"0.2", "0.2.0", true, true},
		{"0.2.0-01", "0.2.0", true, true},
		{"0.2.0", "latest", false, false},

		// 02 §13.2: a dev build is below every real floor; a floor that isn't SemVer is no floor.
		{"0.0.0-dev+1a2b3c4d5e6f", "0.1.0", true, true},
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", "0.0.1", true, true},
		{"0.0.0-dev", "0.0.0-dev", false, true},
		{"0.2.0", "", false, false},
		{"0.2.0", "v0.2.0", false, true},
		{"0.2.0", "0.2.0+meta", false, true},
		{"0.2.0", "0.2.0-", false, false},
		{"0.1.9", "v0.2.0-rc.1", true, true},
		{"0.2.0-rc.1", "0.2.0-rc.1+ci.7", false, true},
	} {
		below, ok := Below(tc.v, tc.floor)
		if below != tc.below || ok != tc.ok {
			t.Errorf("Below(%q, %q) = %v, %v; want %v, %v", tc.v, tc.floor, below, ok, tc.below, tc.ok)
		}
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want SemVer
	}{
		{"0.0.0", SemVer{}},
		{"1.2.3", SemVer{Major: 1, Minor: 2, Patch: 3}},
		{"v1.2.3", SemVer{Major: 1, Minor: 2, Patch: 3}},
		{"10.20.30", SemVer{Major: 10, Minor: 20, Patch: 30}},
		{"1.0.0-alpha", SemVer{Major: 1, Pre: []string{"alpha"}}},
		{"1.0.0-alpha.1", SemVer{Major: 1, Pre: []string{"alpha", "1"}}},
		{"1.0.0-0.3.7", SemVer{Major: 1, Pre: []string{"0", "3", "7"}}},
		{"1.0.0-x.7.z.92", SemVer{Major: 1, Pre: []string{"x", "7", "z", "92"}}},
		{"1.0.0-x-y-z.--", SemVer{Major: 1, Pre: []string{"x-y-z", "--"}}},
		{"1.0.0-alpha+001", SemVer{Major: 1, Pre: []string{"alpha"}}},
		{"1.0.0+20130313144700", SemVer{Major: 1}},
		{"1.0.0-beta+exp.sha.5114f85", SemVer{Major: 1, Pre: []string{"beta"}}},
		{"1.0.0+21AF26D3----117B344092BD", SemVer{Major: 1}},
		{"0.0.0-dev+1a2b3c4d5e6f", SemVer{Pre: []string{"dev"}}},
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", SemVer{Pre: []string{"dev"}}},
		{"0.0.0-ci.42", SemVer{Pre: []string{"ci", "42"}}},
		{"0.2.0-rc.1", SemVer{Minor: 2, Pre: []string{"rc", "1"}}},
		{"18446744073709551615.0.0", SemVer{Major: 1<<64 - 1}},
	} {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if got.Major != tc.want.Major || got.Minor != tc.want.Minor || got.Patch != tc.want.Patch ||
			!slices.Equal(got.Pre, tc.want.Pre) {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{
		"", "v", "1", "1.2", "1.2.3.4", "1.2.3-", "1.2.3+", "1.2.3-+", "1.2.3-.", "1.2.3-a..b", "1.2.3+a..b",
		"01.2.3", "1.02.3", "1.2.03", "1.2.3-01", "1.2.3-a.01", "1.2.3-ä", "1.2.3+ä", "1.2.3 ", " 1.2.3",
		"1.2.3-rc_1", "V1.2.3", "vv1.2.3", "-1.2.3", "1.-2.3", "+1.2.3", "1.2.3++a", "a.b.c", "1.2.c",
		"18446744073709551616.0.0", "latest", "dev",
	} {
		if v, err := Parse(in); !errors.Is(err, ErrNotSemVer) {
			t.Errorf("Parse(%q) = %+v, %v; want ErrNotSemVer", in, v, err)
		}
	}
}

// TestCompare checks semver.org §11's precedence chain, every pair both ways.
func TestCompare(t *testing.T) {
	chain := []string{
		"0.0.0-dev+1a2b3c4d5e6f", "0.0.0", "0.1.0", "0.9.0", "0.10.0",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11",
		"1.0.0-rc.1", "1.0.0", "2.0.0", "2.1.0", "2.1.1", "v10.0.0",
	}
	for i, a := range chain {
		for j, b := range chain {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			got, err := Compare(a, b)
			if err != nil || got != want {
				t.Errorf("Compare(%q, %q) = %d, %v; want %d", a, b, got, err, want)
			}
		}
	}

	for _, eq := range [][2]string{
		{"1.0.0", "v1.0.0"},
		{"1.0.0+a", "1.0.0+b"},
		{"1.0.0-rc.1+build.1", "1.0.0-rc.1"},
	} {
		if got, err := Compare(eq[0], eq[1]); err != nil || got != 0 {
			t.Errorf("Compare(%q, %q) = %d, %v; want 0", eq[0], eq[1], got, err)
		}
	}

	for _, bad := range [][2]string{{"1.0.0", "latest"}, {"dev", "1.0.0"}, {"", ""}} {
		if got, err := Compare(bad[0], bad[1]); !errors.Is(err, ErrNotSemVer) || got != 0 {
			t.Errorf("Compare(%q, %q) = %d, %v; want 0 and ErrNotSemVer", bad[0], bad[1], got, err)
		}
	}
}

// FuzzCompare: Parse never panics, Compare is antisymmetric and reflexive on SemVer input, and Below agrees with
// Compare whenever both are SemVer.
func FuzzCompare(f *testing.F) {
	for _, s := range [][2]string{
		{"1.0.0", "1.0.0-rc.1"}, {"v0.2.0", "0.2.0+x"}, {"0.0.0-dev+abc", "0.1.0"}, {"1.0.0-a.1", "1.0.0-a.b"},
		{"01.0.0", "1.0.0"}, {"", "latest"}, {"1.0.0-99999999999999999999", "1.0.0-100000000000000000000"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		va, errA := Parse(a)
		vb, errB := Parse(b)
		if errA == nil && va.Compare(va) != 0 {
			t.Fatalf("%q compares unequal to itself", a)
		}
		ab, err := Compare(a, b)
		if (err == nil) != (errA == nil && errB == nil) {
			t.Fatalf("Compare(%q, %q) error %v, Parse errors %v, %v", a, b, err, errA, errB)
		}
		below, ok := Below(a, b)
		if ok != (errB == nil) {
			t.Fatalf("Below(%q, %q) ok = %v with Parse(floor) error %v", a, b, ok, errB)
		}
		if err != nil {
			if errA != nil && errB == nil && !below {
				t.Fatalf("Below(%q, %q): a version that isn't SemVer must count as below", a, b)
			}
			return
		}
		if ba, _ := Compare(b, a); ba != -ab {
			t.Fatalf("Compare(%q, %q) = %d but Compare(%q, %q) = %d", a, b, ab, b, a, ba)
		}
		if ab != va.Compare(vb) {
			t.Fatalf("Compare(%q, %q) = %d, SemVer.Compare = %d", a, b, ab, va.Compare(vb))
		}
		if below != (ab < 0) {
			t.Fatalf("Below(%q, %q) = %v, Compare = %d", a, b, below, ab)
		}
	})
}
