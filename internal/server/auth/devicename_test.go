package auth

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// TestNormalizeDeviceName is M2 02 §5.6's rule: PRECIS Nickname (the profile of room names), 1–64 code points, and
// the field codes required, too_long and invalid.
func TestNormalizeDeviceName(t *testing.T) {
	for _, tc := range []struct {
		in, want, code string
	}{
		{in: "DESKTOP-7F3K2LQ", want: "DESKTOP-7F3K2LQ"},
		{in: "  Gaming   PC  ", want: "Gaming PC"},
		{in: "Gaming\u00a0PC", want: "Gaming PC"},      // a no-break space becomes U+0020
		{in: "\uff27\uff41\uff4d\uff45", want: "Game"}, // full-width letters, NFKC
		{in: "Alex's \u7b14\u8bb0\u672c", want: "Alex's \u7b14\u8bb0\u672c"},
		{in: "\U0001F3AE Rig", want: "\U0001F3AE Rig"},
		{in: "Windows PC", want: "Windows PC"},
		{in: strings.Repeat("\u00e9", 64), want: strings.Repeat("\u00e9", 64)},
		{in: "", code: FieldRequired},
		{in: "    ", code: FieldRequired},
		{in: "\t\n", code: FieldRequired},
		{in: "\u3000\u00a0", code: FieldRequired}, // only spaces of other kinds
		{in: strings.Repeat("\u00e9", 65), code: FieldTooLong},
		{in: strings.Repeat("x", 65), code: FieldTooLong},
		{in: "a\nb", code: FieldInvalid},
		{in: "a\x00b", code: FieldInvalid},
		{in: "a\u200bb", code: FieldInvalid}, // a zero-width space
		{in: "\xff\xfe", code: FieldInvalid},
		{in: "PC \ufffd", code: FieldInvalid},
		{in: "\u2764\ufe0f", code: FieldInvalid}, // the profile refuses U+FE0F, as for room names
	} {
		got, err := NormalizeDeviceName(tc.in)
		if tc.code != "" {
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != "deviceName" || fe.Code != tc.code || got != "" {
				t.Errorf("NormalizeDeviceName(%q) = %q, %v; want the field code %s", tc.in, got, err, tc.code)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeDeviceName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
		if again, err := NormalizeDeviceName(got); err != nil || again != got {
			t.Errorf("NormalizeDeviceName(%q) = %q, %v; a stored name must normalize unchanged", got, again, err)
		}
	}
}

// FuzzNormalizeDeviceName: no input panics; a refusal is one of the three field codes; an accepted name has 1–64
// code points of valid UTF-8 and normalizes to itself.
func FuzzNormalizeDeviceName(f *testing.F) {
	for _, s := range []string{"DESKTOP-7F3K2LQ", "  Gaming   PC  ", "", "\u3000", "a\nb", "\xff", "\u2764\ufe0f",
		strings.Repeat("\u00e9", 65), "\uff27\uff41\uff4d\uff45"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := NormalizeDeviceName(in)
		if err != nil {
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != fieldDeviceName ||
				(fe.Code != FieldRequired && fe.Code != FieldTooLong && fe.Code != FieldInvalid) {
				t.Fatalf("NormalizeDeviceName(%q): %v", in, err)
			}
			return
		}
		if n := utf8.RuneCountInString(got); !utf8.ValidString(got) || n < 1 || n > deviceNameMaxRunes {
			t.Fatalf("NormalizeDeviceName(%q) = %q (%d code points)", in, got, n)
		}
		if again, err := NormalizeDeviceName(got); err != nil || again != got {
			t.Fatalf("NormalizeDeviceName(%q) = %q, %v; want %q again", got, again, err, got)
		}
	})
}

// TestCheckAppVersion is M2 02 §5.2's rule: 1–32 bytes of [0-9A-Za-z.+-].
func TestCheckAppVersion(t *testing.T) {
	for _, tc := range []struct{ in, code string }{
		{"0.2.0", ""},
		{"0.0.0-dev+3f9c2a1", ""},
		{"1.0.0-rc.1+build.7", ""},
		{strings.Repeat("9", 32), ""},
		{"", FieldRequired},
		{strings.Repeat("9", 33), FieldTooLong},
		{"0.2.0 ", FieldInvalid},
		{"v0.2_0", FieldInvalid},
		{"0.2.\u00e9", FieldInvalid},
		{"0.2.0\n", FieldInvalid},
	} {
		if got := checkAppVersion(tc.in); got != tc.code {
			t.Errorf("checkAppVersion(%q) = %q, want %q", tc.in, got, tc.code)
		}
	}
}

// TestIPKey48 is device-code-ip48's key (M2 02 §5.5, §16.1): IPv4 → false, IPv4-mapped → false, a zone dropped.
func TestIPKey48(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // "" = false
	}{
		{"2001:db8:aa:1::7", "2001:db8:aa::/48"},
		{"2001:db8:aa:ffff:1:2:3:4", "2001:db8:aa::/48"},
		{"2001:db8:bb::1", "2001:db8:bb::/48"},
		{"fe80::1%eth0", "fe80::/48"},
		{"::1", "::/48"},
		{"203.0.113.7", ""},
		{"::ffff:203.0.113.7", ""},
	} {
		got, ok := IPKey48(netip.MustParseAddr(tc.in))
		switch {
		case tc.want == "" && (ok || got.IsValid()):
			t.Errorf("IPKey48(%s) = %v, %v; want false", tc.in, got, ok)
		case tc.want != "" && (!ok || got != netip.MustParsePrefix(tc.want) || got.Addr().Zone() != ""):
			t.Errorf("IPKey48(%s) = %v, %v; want %s", tc.in, got, ok, tc.want)
		}
	}
	if got, ok := IPKey48(netip.Addr{}); ok || got.IsValid() {
		t.Errorf("IPKey48(zero Addr) = %v, %v; want false", got, ok)
	}
}

// TestNetworkMatch is LookupUserCode's network comparison (M2 02 §5.3, §16.1).
func TestNetworkMatch(t *testing.T) {
	for _, tc := range []struct {
		app, browser string
		want         api.NetworkMatch
	}{
		{"203.0.113.7", "203.0.113.7", api.NetworkMatchSame},
		{"203.0.113.7", "198.51.100.9", api.NetworkMatchDifferent},
		{"2001:db8:1:2::7", "2001:db8:1:2::99", api.NetworkMatchSame}, // one /64
		{"2001:db8:1:2::7", "2001:db8:1:3::7", api.NetworkMatchDifferent},
		{"2001:db8:cc::7", "203.0.113.7", api.NetworkMatchUnknown}, // started over IPv6, looked up over IPv4
		{"203.0.113.7", "2001:db8:cc::7", api.NetworkMatchUnknown}, // and the reverse
		{"203.0.113.7", "::ffff:203.0.113.7", api.NetworkMatchSame},
		{"203.0.113.7", "", api.NetworkMatchUnknown},
		{"", "203.0.113.7", api.NetworkMatchUnknown},
	} {
		var browser netip.Addr
		if tc.browser != "" {
			browser = netip.MustParseAddr(tc.browser)
		}
		if got := networkMatch(tc.app, browser); got != tc.want {
			t.Errorf("networkMatch(%q, %q) = %s, want %s", tc.app, tc.browser, got, tc.want)
		}
	}
}
