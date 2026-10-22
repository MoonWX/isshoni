package deeplink_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// linksFile is testdata/links.json (its "about" field explains the format).
type linksFile struct {
	Parse []parseCase `json:"parse"`
	Build []buildCase `json:"build"`
}

type parseCase struct {
	Name        string `json:"name"`
	Link        string `json:"link"`
	PadTo       int    `json:"padTo"`
	OK          bool   `json:"ok"`
	Error       string `json:"error"` // "invalid" or "unsupported" when !OK
	Origin      string `json:"origin"`
	Host        string `json:"host"`
	HostUnicode string `json:"hostUnicode"`
	RoomID      string `json:"roomId"`
	Built       string `json:"built"`
}

// raw is the link as Parse sees it: padded with 'a' to PadTo bytes.
func (c parseCase) raw() string {
	if c.PadTo > len(c.Link) {
		return c.Link + strings.Repeat("a", c.PadTo-len(c.Link))
	}
	return c.Link
}

func (c parseCase) want() deeplink.Link {
	return deeplink.Link{Origin: c.Origin, Host: c.Host, HostUnicode: c.HostUnicode, RoomID: c.RoomID}
}

type buildCase struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
	RoomID string `json:"roomId"`
	Link   string `json:"link"`
}

func loadLinks(t testing.TB) linksFile {
	t.Helper()
	b, err := os.ReadFile("testdata/links.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var f struct {
		About []string `json:"about"`
		linksFile
	}
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("testdata/links.json: %v", err)
	}
	if len(f.Parse) == 0 || len(f.Build) == 0 {
		t.Fatal("testdata/links.json has no cases")
	}
	return f.linksFile
}

// errKind names the sentinel err wraps: "invalid", "unsupported", "" for nil, or "?" for anything else.
func errKind(err error) string {
	inv, uns := errors.Is(err, deeplink.ErrInvalid), errors.Is(err, deeplink.ErrUnsupported)
	switch {
	case err == nil:
		return ""
	case inv && !uns:
		return "invalid"
	case uns && !inv:
		return "unsupported"
	}
	return "?"
}

func TestLinksJSON(t *testing.T) {
	f := loadLinks(t)
	names := map[string]bool{}
	for _, c := range f.Parse {
		t.Run(c.Name, func(t *testing.T) {
			if names[c.Name] {
				t.Fatalf("duplicate case name %q", c.Name)
			}
			names[c.Name] = true
			raw := c.raw()
			if c.PadTo != 0 && len(raw) != c.PadTo {
				t.Fatalf("padded link has %d bytes, want %d", len(raw), c.PadTo)
			}
			got, err := deeplink.Parse(raw)
			if !c.OK {
				if c.Error != "invalid" && c.Error != "unsupported" {
					t.Fatalf("case error %q is neither invalid nor unsupported", c.Error)
				}
				if kind := errKind(err); kind != c.Error {
					t.Fatalf("Parse = %+v, %v (%q); want a %s error", got, err, kind, c.Error)
				}
				if got != (deeplink.Link{}) {
					t.Errorf("Parse returned %+v with its error, want a zero Link", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != c.want() {
				t.Fatalf("Parse = %+v, want %+v", got, c.want())
			}
			if b := deeplink.Build(got.Origin, got.RoomID); b != c.Built {
				t.Errorf("Build(%q, %q) = %q, want %q", got.Origin, got.RoomID, b, c.Built)
			}
			again, err := deeplink.Parse(c.Built)
			if err != nil || again != got {
				t.Errorf("Parse(built %q) = %+v, %v; want %+v", c.Built, again, err, got)
			}
		})
	}
	for _, c := range f.Build {
		t.Run("build/"+c.Name, func(t *testing.T) {
			if got := deeplink.Build(c.Origin, c.RoomID); got != c.Link {
				t.Fatalf("Build(%q, %q) = %q, want %q", c.Origin, c.RoomID, got, c.Link)
			}
			if c.Link == "" {
				return
			}
			l, err := deeplink.Parse(c.Link)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.Link, err)
			}
			wantRoom := c.RoomID
			if !deeplink.ValidRoomID(wantRoom) {
				wantRoom = ""
			}
			if l.RoomID != wantRoom {
				t.Errorf("Parse(%q).RoomID = %q, want %q", c.Link, l.RoomID, wantRoom)
			}
		})
	}
}

// TestLinksJSONCoverage keeps the shared file covering what the acceptance names: the spec's examples, the 2048-byte
// boundary on both sides, both error kinds, and room ids at the rule's edges.
func TestLinksJSONCoverage(t *testing.T) {
	f := loadLinks(t)
	byLink := map[string]parseCase{}
	var at2048, over2048, room64, room65 bool
	for _, c := range f.Parse {
		byLink[c.Link] = c
		switch raw := c.raw(); {
		case len(raw) == deeplink.MaxBytes && c.OK:
			at2048 = true
		case len(raw) == deeplink.MaxBytes+1 && c.Error == "invalid":
			over2048 = true
		}
		if c.OK && len(c.RoomID) == deeplink.MaxRoomIDLen {
			room64 = true
		}
		if strings.Contains(c.Link, "/r/") && c.Error == "unsupported" && len(c.Link)-strings.Index(c.Link, "/r/")-3 == 65 {
			room65 = true
		}
	}
	if !at2048 || !over2048 || !room64 || !room65 {
		t.Errorf("links.json lacks a boundary case: 2048 bytes ok %v, 2049 invalid %v, 64-char room %v, 65-char room %v",
			at2048, over2048, room64, room65)
	}
	// 02 §12.1's list, with the outcome it gives.
	for link, want := range map[string]string{
		"isshoni://watch.example.com/r/lounge":           "ok",
		"ISSHONI://Watch.Example.com:443/r/lounge/":      "ok",
		"isshoni://[2001:db8::1]:8443/":                  "ok",
		"isshoni://xn--bcher-kva.example/":               "ok",
		"isshoni://watch.example.com/r/lounge?token=x#t": "ok",
		"isshoni://user@watch.example.com/":              "invalid",
		"isshoni:watch.example.com":                      "invalid",
		"isshoni://watch.example.com/admin":              "unsupported",
		"isshoni://watch.example.com/r/":                 "unsupported",
		"isshoni://watch.example.com:0/":                 "invalid",
	} {
		c, found := byLink[link]
		got := c.Error
		if c.OK {
			got = "ok"
		}
		if !found || got != want {
			t.Errorf("links.json: %q is %q (present %v), want %q", link, got, found, want)
		}
	}
	if c := byLink["isshoni://xn--bcher-kva.example/"]; c.HostUnicode != "bücher.example" {
		t.Errorf("HostUnicode of the IDN example = %q, want bücher.example", c.HostUnicode)
	}
}

// hostileTails are queries and fragments that would matter if anything read them.
var hostileTails = []string{
	"?",
	"#",
	"?token=isa_0123456789abcdef",
	"#access_token=isa_0123456789abcdef",
	"?code=WDJB-MJHT&start=share&role=publisher",
	"?roomId=other#/r/other",
	"?u=user@evil.example:8443",
	"?next=isshoni://evil.example/r/x",
	"?x=%00%0d%0a%22%20--flag",
	"#frag?query=1",
	"?a=1#b?c#d",
	"?" + strings.Repeat("q", 1500),
}

// TestQueryAndFragmentNeverRead: for every accepted link of links.json without a query or fragment, adding any of
// hostileTails changes nothing, and the one debug line names neither their content nor the link.
func TestQueryAndFragmentNeverRead(t *testing.T) {
	logs := captureLogs(t)
	n := 0
	for _, c := range loadLinks(t).Parse {
		if !c.OK || strings.ContainsAny(c.raw(), "?#") {
			continue
		}
		n++
		logs.reset()
		base, err := deeplink.Parse(c.raw())
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.raw(), err)
		}
		if got := logs.all(); len(got) != 0 {
			t.Errorf("Parse(%q) without a query logged %q", c.raw(), got)
		}
		for _, tail := range hostileTails {
			raw := c.raw() + tail
			logs.reset()
			got, err := deeplink.Parse(raw)
			if err != nil || got != base {
				t.Errorf("Parse(%q) = %+v, %v; want %+v as without %q", raw, got, err, base, tail)
			}
			lines := logs.all()
			if len(lines) != 1 {
				t.Fatalf("Parse(%q) logged %d lines, want 1: %q", raw, len(lines), lines)
			}
			if strings.Contains(lines[0], "isa_") || strings.Contains(lines[0], "WDJB") ||
				strings.Contains(lines[0], "evil") || strings.Contains(lines[0], base.Host) {
				t.Errorf("the debug line names the link or its tail: %q", lines[0])
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d accepted links without a tail in links.json", n)
	}
}

// TestLength: 2048 bytes is the limit for any link, multi-byte characters counted in bytes.
func TestLength(t *testing.T) {
	base := "isshoni://watch.example.com/r/lounge"
	pad := func(n int, fill string) string {
		s := base + "?"
		for len(s)+len(fill) <= n {
			s += fill
		}
		return s + strings.Repeat("a", n-len(s))
	}
	tests := []struct {
		raw  string
		kind string
	}{
		{pad(2048, "a"), ""},
		{pad(2049, "a"), "invalid"},
		{pad(2048, "é"), ""},
		{pad(2049, "é"), "invalid"},
		{pad(4096, "a"), "invalid"},
		{"isshoni://" + strings.Repeat("a", 2039), "invalid"},
	}
	for _, tt := range tests {
		_, err := deeplink.Parse(tt.raw)
		if errKind(err) != tt.kind {
			t.Errorf("Parse(%d bytes) = %v, want kind %q", len(tt.raw), err, tt.kind)
		}
	}
}

// TestInvalidUTF8 covers what links.json can't hold.
func TestInvalidUTF8(t *testing.T) {
	for _, raw := range []string{
		"isshoni://wa\xfftch.example.com/",
		"isshoni://watch.example.com/r/lounge?\xff",
		"isshoni://watch.example.com/r/lounge#\xc3",
		"isshoni://watch.example.com/r/\xed\xa0\x80", // an encoded surrogate
		"\xffisshoni://watch.example.com/",
	} {
		if _, err := deeplink.Parse(raw); errKind(err) != "invalid" {
			t.Errorf("Parse(%q) = %v, want invalid", raw, err)
		}
	}
}

// TestErrorsNameNoPartOfTheLink: errors may be logged, so they carry the rule, never the link's content.
func TestErrorsNameNoPartOfTheLink(t *testing.T) {
	for _, raw := range []string{
		"isshoni://SECRET:SECRET@watch.example.com/",
		"isshoni://watch.example.com/SECRET",
		"isshoni://watch.example.com/r/SECRET.x?SECRET",
		"isshoni://watch.example.com/r/lounge?SECRET x",
		"isshoni://SECRET_x.example/",
		"isshoni://watch.example.com:SECRET/",
		"SECRET://watch.example.com/",
		"isshoni:SECRET",
	} {
		_, err := deeplink.Parse(raw)
		if err == nil {
			t.Fatalf("Parse(%q) accepted", raw)
		}
		if strings.Contains(strings.ToLower(err.Error()), "secret") || strings.Contains(err.Error(), "watch") {
			t.Errorf("Parse(%q) error %q names part of the link", raw, err)
		}
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		origin, room, want string
	}{
		{"https://watch.example.com", "lounge", "isshoni://watch.example.com/r/lounge"},
		{"watch.example.com", "", "isshoni://watch.example.com/"},          // NormalizeOrigin's scheme-less form
		{"https://bücher.example", "", "isshoni://xn--bcher-kva.example/"}, // the ASCII form
		{"https://watch.example.com/r/lounge", "", ""},                     // an origin has no path
		{"https://watch.example.com?x", "", ""},                            // nor a query
		{"https://user@watch.example.com", "", ""},                         // nor userinfo
		{"http://127.0.0.1:8080", "", ""},                                  // http, even for loopback
		{"https://[::1]:8443", "a", "isshoni://[::1]:8443/r/a"},            // https loopback is a server like any
		{"isshoni://watch.example.com/", "", ""},                           // a link is not an origin
		{"https://watch.example.com", strings.Repeat("r", 64), "isshoni://watch.example.com/r/" + strings.Repeat("r", 64)},
	}
	for _, tt := range tests {
		if got := deeplink.Build(tt.origin, tt.room); got != tt.want {
			t.Errorf("Build(%q, %q) = %q, want %q", tt.origin, tt.room, got, tt.want)
		}
	}
}

func TestValidRoomID(t *testing.T) {
	if deeplink.MaxRoomIDLen != protocol.MaxOpaqueIDLen {
		t.Fatalf("MaxRoomIDLen = %d, protocol.MaxOpaqueIDLen = %d", deeplink.MaxRoomIDLen, protocol.MaxOpaqueIDLen)
	}
	for id, want := range map[string]bool{
		"lounge":                  true,
		"k3m9p2qxw7ht":            true,
		"a":                       true,
		"A_b-9":                   true,
		"-":                       true,
		"_":                       true,
		strings.Repeat("x", 64):   true,
		"":                        false,
		strings.Repeat("x", 65):   false,
		"lounge.x":                false,
		"lounge/x":                false,
		"lounge x":                false,
		"lounge%20":               false,
		"ラウンジ":                    false,
		"lounge\x00":              false,
		"\ufb01":                  false, // a ligature isn't "fi"
		"\u212a":                  false, // the Kelvin sign isn't "K"
		"lounge?start=share":      false,
		"lounge#x":                false,
		"lounge:8443":             false,
		strings.Repeat("é", 32):   false,
		strings.Repeat("ab", 100): false,
	} {
		if got := deeplink.ValidRoomID(id); got != want {
			t.Errorf("ValidRoomID(%q) = %v, want %v", id, got, want)
		}
	}
}

// logSink records every log line's message and attributes as text.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = nil
}

func (s *logSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

type sinkHandler struct {
	s     *logSink
	attrs []slog.Attr
}

func (h sinkHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h sinkHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	for _, a := range h.attrs {
		b.WriteString(" " + a.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.String())
		return true
	})
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.lines = append(h.s.lines, b.String())
	return nil
}

func (h sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sinkHandler{s: h.s, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h sinkHandler) WithGroup(string) slog.Handler { return h }

// captureLogs points slog's default logger at a sink for the test (so tests using it don't run in parallel).
func captureLogs(t *testing.T) *logSink {
	t.Helper()
	prev := slog.Default()
	s := &logSink{}
	slog.SetDefault(slog.New(sinkHandler{s: s}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return s
}

// TestDebugLineLevel: the dropped-query line is a debug line.
func TestDebugLineLevel(t *testing.T) {
	logs := captureLogs(t)
	if _, err := deeplink.Parse("isshoni://watch.example.com/r/lounge?token=x"); err != nil {
		t.Fatal(err)
	}
	lines := logs.all()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "DEBUG ") || !strings.Contains(lines[0], "component=deeplink") {
		t.Fatalf("logged %q, want one DEBUG line with component=deeplink", lines)
	}
}
