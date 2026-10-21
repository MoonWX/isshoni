package deeplink_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
)

// maxFuzzBytes bounds the fuzzers' inputs. The fuzzer minimizes every new input it finds, which is quadratic in its
// length: one of 2 KiB takes the whole -fuzzminimizetime (60 s), during which nothing else runs. 640 bytes holds a
// 253-byte host, a port, a 64-character room id and a tail; TestLength and links.json cover the 2048-byte limit.
const maxFuzzBytes = 640

// FuzzParse: Parse never panics; an error is exactly one of ErrInvalid and ErrUnsupported with a zero Link; an
// accepted link is canonical (its Origin is NormalizeOrigin's fixed point, its host ASCII, its room id valid) and
// rebuilds with Build to an equal Link; and a query or fragment appended to any input changes nothing (02 §12.1,
// §16.1). Seeds: the links of testdata/links.json (without their padding) and testdata/fuzz/FuzzParse.
func FuzzParse(f *testing.F) {
	for _, c := range loadLinks(f).Parse {
		f.Add(c.Link)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > maxFuzzBytes {
			return
		}
		l, err := deeplink.Parse(raw)
		if err != nil {
			if kind := errKind(err); kind != "invalid" && kind != "unsupported" {
				t.Fatalf("Parse(%q) error %v wraps neither or both sentinels", raw, err)
			}
			if l != (deeplink.Link{}) {
				t.Fatalf("Parse(%q) returned %+v with its error", raw, l)
			}
		} else {
			checkAccepted(t, raw, l)
		}

		// Nothing after the first "?" or "#" is read.
		l2, err2 := deeplink.Parse(raw + "?token=isa_x&code=WDJB-MJHT#/r/other")
		if l2 != l || errKind(err2) != errKind(err) {
			t.Fatalf("Parse(%q) = %+v, %v; with a tail %+v, %v", raw, l, err, l2, err2)
		}
	})
}

func checkAccepted(t *testing.T, raw string, l deeplink.Link) {
	t.Helper()
	if len(raw) > deeplink.MaxBytes || !utf8.ValidString(raw) {
		t.Fatalf("Parse accepted %d bytes (valid UTF-8 %v)", len(raw), utf8.ValidString(raw))
	}
	if l.Origin != "https://"+l.Host || l.Host == "" || l.HostUnicode == "" {
		t.Fatalf("Parse(%q) = %+v: Origin, Host and HostUnicode disagree", raw, l)
	}
	for i := range len(l.Host) {
		if c := l.Host[i]; c >= utf8.RuneSelf || (c >= 'A' && c <= 'Z') {
			t.Fatalf("Parse(%q).Host = %q is not lower-case ASCII", raw, l.Host)
		}
	}
	if (l.HostUnicode != l.Host) != (strings.HasPrefix(l.Host, "xn--") || strings.Contains(l.Host, ".xn--")) {
		t.Fatalf("Parse(%q) = %+v: HostUnicode differs from Host exactly when Host has an xn-- label", raw, l)
	}
	if l.RoomID != "" && !deeplink.ValidRoomID(l.RoomID) {
		t.Fatalf("Parse(%q).RoomID = %q breaks the room-id rule", raw, l.RoomID)
	}
	if o, err := deeplink.NormalizeOrigin(l.Origin, false); err != nil || o != l.Origin {
		t.Fatalf("NormalizeOrigin(%q) = %q, %v; want it unchanged", l.Origin, o, err)
	}
	built := deeplink.Build(l.Origin, l.RoomID)
	again, err := deeplink.Parse(built)
	if err != nil || again != l {
		t.Fatalf("Parse(%q) = %+v, but Parse(Build = %q) = %+v, %v", raw, l, built, again, err)
	}
	if deeplink.Build(again.Origin, again.RoomID) != built {
		t.Fatalf("Build is not stable for %q", built)
	}
}

// FuzzNormalizeOrigin: NormalizeOrigin never panics; it refuses with ErrInvalidOrigin or returns a fixed point that
// is an https origin (or, only when allowed, an http loopback one) that Build and Parse agree with.
func FuzzNormalizeOrigin(f *testing.F) {
	for _, s := range []string{
		"watch.example.com", "https://watch.example.com:443/", "http://localhost:8080", "http://[::1]", "2001:db8::1",
		"bücher.example", "https://user@watch.example.com", "https://watch.example.com/r/lounge", "127.1",
		"javascript:alert(1)", "https:watch.example.com", " https://watch.example.com ", "[fe80::1%25en0]",
	} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, raw string, loopback bool) {
		if len(raw) > maxFuzzBytes {
			return
		}
		o, err := deeplink.NormalizeOrigin(raw, loopback)
		if err != nil {
			if !errors.Is(err, deeplink.ErrInvalidOrigin) || o != "" {
				t.Fatalf("NormalizeOrigin(%q, %v) = %q, %v", raw, loopback, o, err)
			}
			return
		}
		if again, err := deeplink.NormalizeOrigin(o, loopback); err != nil || again != o {
			t.Fatalf("NormalizeOrigin(%q) = %q, but NormalizeOrigin(%q) = %q, %v", raw, o, o, again, err)
		}
		host, isHTTPS := strings.CutPrefix(o, "https://")
		if !isHTTPS {
			h, isHTTP := strings.CutPrefix(o, "http://")
			hostOnly := h
			if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
				hostOnly = h[:i]
			}
			if !isHTTP || !loopback || (hostOnly != "localhost" && hostOnly != "127.0.0.1" && hostOnly != "[::1]") {
				t.Fatalf("NormalizeOrigin(%q, %v) = %q: not https, nor http to a loopback host when allowed", raw, loopback, o)
			}
			return
		}
		if strings.ContainsAny(host, "/?#@") || host == "" {
			t.Fatalf("NormalizeOrigin(%q) = %q has a path, query, fragment or userinfo", raw, o)
		}
		l, err := deeplink.Parse(deeplink.Build(o, ""))
		if err != nil || l.Origin != o || l.Host != host || l.RoomID != "" {
			t.Fatalf("Parse(Build(%q)) = %+v, %v", o, l, err)
		}
	})
}
