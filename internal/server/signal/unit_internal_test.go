package signal

import (
	"crypto/sha256"
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

func TestNormalizeOrigin(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		httpOnly bool
	}{
		{"https://Watch.Example.com", "https://watch.example.com", true},
		{"HTTPS://watch.example.com:443", "https://watch.example.com", true},
		{"http://watch.example.com:80", "http://watch.example.com", true},
		{"http://watch.example.com:443", "http://watch.example.com:443", true},
		{"https://watch.example.com:08443", "https://watch.example.com:8443", true},
		{"https://[2001:DB8::1]", "https://[2001:db8::1]", true},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443", true},
		{"https://192.0.2.10:8443", "https://192.0.2.10:8443", true},
		{"wails://wails.localhost", "wails://wails.localhost", false},
		{"wails://wails.localhost", "", true},
		{"https://watch.example.com/", "", false},
		{"https://watch.example.com?x", "", false},
		{"https://user@watch.example.com", "", false},
		{"https://watch.example.com:0", "", false},
		{"https://watch.example.com:65536", "", false},
		{"https://bücher.example", "", false},
		{"https://[fe80::1%25eth0]", "", false},
		{"null", "", false},
		{"A://::", "", false},        // an unbracketed IPv6 host (found by FuzzNormalizeOrigin)
		{"https://::1", "", false},   // the same
		{"https://a%20b", "", false}, // not a host name
		{"", "", false},
		{"watch.example.com", "", false},
	} {
		got, err := normalizeOrigin(tc.in, tc.httpOnly)
		if tc.want == "" {
			if err == nil {
				t.Errorf("normalizeOrigin(%q, %v) = %q, want an error", tc.in, tc.httpOnly, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeOrigin(%q, %v) = %q, %v; want %q", tc.in, tc.httpOnly, got, err, tc.want)
		}
	}
}

func TestOriginsClassify(t *testing.T) {
	o, err := newOrigins("https://watch.example.com:443", []string{"wails://wails.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		values []string
		want   originKind
	}{
		{nil, originAbsent},
		{[]string{"https://watch.example.com"}, originPublic},
		{[]string{"https://WATCH.example.com:443"}, originPublic},
		{[]string{"wails://WAILS.localhost"}, originAllowed},
		{[]string{"https://evil.example.com"}, originBad},
		{[]string{"https://watch.example.com", "https://watch.example.com"}, originBad},
		{[]string{"null"}, originBad},
	} {
		if got := o.classify(tc.values); got != tc.want {
			t.Errorf("classify(%q) = %d, want %d", tc.values, got, tc.want)
		}
	}
}

func TestResumeToken(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	other := []byte(strings.Repeat("o", 32))
	_, raw := newConnID()
	tok, hash := mintResumeToken(key, raw)
	s := tok.Reveal()
	if !strings.HasPrefix(s, "r1.") || len(s) != 3+56 {
		t.Fatalf("token %q", s)
	}
	if hash != sha256.Sum256([]byte(s)) {
		t.Error("hash is not the token's SHA-256")
	}
	if got, ok := parseResumeToken(key, s); !ok || got != raw {
		t.Errorf("parse: %v, %v", got, ok)
	}
	tok2, _ := mintResumeToken(key, raw)
	if tok2 == tok {
		t.Error("two tokens of one connection are equal (no nonce)")
	}
	if _, ok := parseResumeToken(other, s); ok {
		t.Error("a token of another key (a previous process) parsed")
	}
	for _, bad := range []string{"", "r1.", "r2." + s[3:], s[:len(s)-1], s + "A", "r1." + strings.Repeat("A", 56)} {
		if _, ok := parseResumeToken(key, bad); ok {
			t.Errorf("parse(%q) ok", bad)
		}
	}
	// Flipping any byte of the payload breaks the HMAC.
	b := []byte(s)
	b[10] ^= 0x01
	if b[10] == '-' || b[10] == '_' || (b[10] >= '0' && b[10] <= '9') || (b[10] >= 'a' && b[10] <= 'z') ||
		(b[10] >= 'A' && b[10] <= 'Z') {
		if _, ok := parseResumeToken(key, string(b)); ok {
			t.Error("a tampered token parsed")
		}
	}
	if id := connIDFromRaw(raw); !strings.HasPrefix(id, "c_") || len(id) != 18 {
		t.Errorf("connection id %q", id)
	}
	if ref := newRef(); len(ref) != 8 {
		t.Errorf("ref %q", ref)
	}
}

func TestVersionBelow(t *testing.T) {
	for _, tc := range []struct {
		v, floor  string
		below, ok bool
	}{
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
	} {
		below, ok := versionBelow(tc.v, tc.floor)
		if below != tc.below || ok != tc.ok {
			t.Errorf("versionBelow(%q, %q) = %v, %v; want %v, %v", tc.v, tc.floor, below, ok, tc.below, tc.ok)
		}
	}
}

func TestSendQueue(t *testing.T) {
	var q sendQueue
	q.init(3, 10)
	for _, m := range []string{"aaa", "bbb", "ccc"} {
		if r := q.push([]byte(m)); r != pushed {
			t.Fatalf("push %s: %v", m, r)
		}
	}
	if r := q.push([]byte("d")); r != pushFull {
		t.Errorf("4th message: %v, want full", r)
	}
	// take hands the writer one message at a time; it leaves the queue's count at once.
	m, fin := q.take()
	if string(m) != "aaa" || fin.set {
		t.Fatalf("take: %q, fin %+v", m, fin)
	}
	if r := q.push([]byte("x")); r != pushed {
		t.Errorf("push after take: %v, want pushed", r)
	}
	if r := q.push([]byte("y")); r != pushFull {
		t.Errorf("4th queued message: %v, want full", r)
	}
	if r := q.push([]byte("0123456789x")); r != pushFull {
		t.Errorf("11 bytes: %v, want full", r)
	}
	if !q.finish(4400, "bad", []byte("err")) || q.finish(4401, "") {
		t.Error("finish: the first call must win")
	}
	if r := q.push([]byte("late")); r != pushGone {
		t.Errorf("push after finish: %v, want gone", r)
	}
	// The close request comes with the last message, after every one queued before it.
	var got []string
	for {
		m, fin := q.take()
		if m != nil {
			got = append(got, string(m))
		}
		if fin.set {
			if fin.code != 4400 || m == nil {
				t.Errorf("fin %+v with %q", fin, m)
			}
			break
		}
		if m == nil {
			t.Fatal("queue empty before the close request")
		}
	}
	if !slices.Equal(got, []string{"bbb", "ccc", "x", "err"}) {
		t.Errorf("take order %q", got)
	}
	if m, fin := q.take(); m != nil || !fin.set {
		t.Errorf("take after the last message: %q, %+v", m, fin)
	}
	if q.abort(websocket.StatusCode(protocol.CloseCodeSlowConnection), "") {
		t.Error("abort after finish")
	}

	var a sendQueue
	a.init(10, 100)
	a.push([]byte("x"))
	if !a.abort(4503, "slow_connection") || !a.aborted() || a.closeCode() != 4503 {
		t.Error("abort")
	}
	if m, fin := a.take(); m != nil || fin.code != 4503 {
		t.Errorf("after abort: %q, %+v", m, fin)
	}
}

// Every client→server message of the registry has a handler (hello and pc.* have their own paths).
func TestHandlersCoverRegistry(t *testing.T) {
	for _, s := range protocol.Registry {
		if s.Dir != protocol.DirClientToServer || s.Type == protocol.MessageTypeHello || isPC(s.Type) {
			continue
		}
		if _, ok := handlers[s.Type]; !ok {
			t.Errorf("no handler for %s", s.Type)
		}
	}
}

func TestShareMediaKindString(t *testing.T) {
	for k, want := range map[ShareMediaKind]string{
		ShareMediaLive: "live", ShareMediaStalled: "stalled", ShareMediaChanged: "changed", ShareMediaGone: "gone", 0: "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("%d: %q, want %q", k, got, want)
		}
	}
}
