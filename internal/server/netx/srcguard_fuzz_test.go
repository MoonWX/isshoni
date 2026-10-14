package netx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"

	"github.com/MoonWX/isshoni/internal/logx"
)

// This file fuzzes the parsing of the ICE source guard (srcguard.go, 04 §7.8, 02 §12): stunUfrag and admitUDP, which
// read every UDP packet below Pion's UDP mux, and firstFrameGate, which reads the first RFC 4571 frame of every ICE-TCP
// connection before Pion's TCP mux. Both parse bytes from anyone who can reach the media ports, before any check of
// MESSAGE-INTEGRITY. The targets check that they never panic or read out of bounds, that their allocation stays
// bounded, and that the ufrag they count a source for is the one Pion's muxes route the same bytes by
// (pionUfrag, which TestPionRoutesLikeReference pins to the real muxes). stunUfrag walks the attribute list instead
// of decoding the message as Pion does, so FuzzStunUfragDecode also holds it to the decoding version (decodedUfrag),
// result for result.

// Allocation bounds of the targets.
//
// stunUfrag allocates nothing, whatever the input: FuzzStunUfrag checks it for every input with testing.AllocsPerRun,
// and TestStunUfragAllocations for the longest attribute lists a STUN header's length allows, which mutation rarely
// grows an input to. (stun.Message.Decode, which decodedUfrag and Pion's muxes use, keeps a 32-byte RawAttribute for
// each attribute, which can be as short as 4 bytes, in a slice that append grows: up to 43 bytes allocated per input
// byte for a message of nothing but empty attributes.)
//
// firstFrameGate copies the frame it reads ahead, so its bound has a part that grows with the bytes it took from the
// connection, which must be at most 2 + maxFirstFrame whatever length the frame's header claims: the copy, rounded up
// to the allocator's size class. The constant covers what doesn't grow: the copy of a stream that ends inside the
// frame, made at the size the header claims, the guard's map entry for a new source, an IPv6 zone that netip interns.
const (
	frameAllocPerByte = 2
	frameAllocSlack   = 4 << 10
)

// allocatedBytes returns the bytes allocated on the heap while fn runs.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// pionUfrag is the ufrag by which Pion's UDP mux routes a packet from a source address it doesn't know yet, as
// UDPMuxDefault.connWorker computes it (pion/ice v4.4.7): a STUN message that decodes and has a USERNAME goes to the
// connection of the USERNAME's part before the first colon; ok is false for anything else, which the mux routes by
// its source address only. Pion's TCP mux (TCPMuxDefault.handleConn) reads a connection's first frame the same way
// and takes it only when it is also a Binding message of at most 512 bytes (pionTCPTakes). This is the reference the
// guard must agree with: the source it counts is charged to the ufrag whose connection Pion hands the packet to.
func pionUfrag(b []byte) (ufrag string, ok bool) {
	if !stun.IsMessage(b) {
		return "", false
	}
	msg := &stun.Message{Raw: append([]byte{}, b...)}
	if err := msg.Decode(); err != nil {
		return "", false
	}
	attr, err := msg.Get(stun.AttrUsername)
	if err != nil {
		return "", false
	}
	return strings.Split(string(attr), ":")[0], true
}

// pionTCPFirstFrameMax is the buffer Pion's TCP mux reads a connection's first frame into (TCPMuxDefault.handleConn).
const pionTCPFirstFrameMax = 512

// pionTCPTakes reports whether Pion's TCP mux gives a connection whose first frame carries frame to a ufrag (the
// one pionUfrag returns), rather than closing it.
func pionTCPTakes(frame []byte) bool {
	if len(frame) > pionTCPFirstFrameMax {
		return false
	}
	msg := &stun.Message{Raw: bytes.Clone(frame)}
	if msg.Decode() != nil || msg.Type.Method != stun.MethodBinding {
		return false
	}
	_, err := msg.Get(stun.AttrUsername)
	return err == nil
}

// decodedUfrag is stunUfrag as it was before it walked the attribute list itself: the message decoded with
// stun.Message.Decode and its USERNAME read with Message.Get, as Pion's muxes do, and the part before the first colon,
// aliasing b. FuzzStunUfragDecode holds stunUfrag to it.
func decodedUfrag(b []byte) (ufrag []byte, ok bool) {
	m := stun.Message{Raw: b}
	if err := m.Decode(); err != nil {
		return nil, false
	}
	u, err := m.Get(stun.AttrUsername)
	if err != nil {
		return nil, false
	}
	if i := bytes.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return u, true
}

// guardSeed is one packet for the guard's fuzz targets, with what Pion's muxes do with it.
type guardSeed struct {
	name  string
	raw   []byte
	ufrag string // the ufrag in its USERNAME, by which Pion routes it when udp or tcp is set
	udp   bool   // the UDP mux routes it by ufrag (pionUfrag), so the guard counts its source
	tcp   bool   // the TCP mux takes a connection with it as the first frame (pionTCPTakes)
}

// seedPwd is the ICE password the seeds' MESSAGE-INTEGRITY is computed with.
const seedPwd = "serverpasswordserverpwd"

// seedTxID makes the seeds the same bytes in every run.
var seedTxID = stun.NewTransactionIDSetter([stun.TransactionIDSize]byte{
	'i', 's', 's', 'h', 'o', 'n', 'i', '-', 'f', 'u', 'z', 'z',
})

// rawAttr adds an attribute as it is, without the checks of stun's typed setters (a USERNAME of over 513 bytes).
type rawAttr struct {
	typ stun.AttrType
	val []byte
}

func (a rawAttr) AddTo(m *stun.Message) error {
	m.Add(a.typ, a.val)
	return nil
}

// seedSTUN builds a STUN message with the seeds' transaction ID.
func seedSTUN(tb testing.TB, setters ...stun.Setter) []byte {
	tb.Helper()
	m, err := stun.Build(append([]stun.Setter{seedTxID}, setters...)...)
	if err != nil {
		tb.Fatal(err)
	}
	return m.Raw
}

// paddedRequest is a Binding request with USERNAME "srv:cli" and a SOFTWARE attribute that makes it size bytes long
// (a multiple of 4, at least 36).
func paddedRequest(tb testing.TB, size int) []byte {
	tb.Helper()
	const fixed = 20 + 4 + 8 + 4 // header, USERNAME "srv:cli" padded to 8, SOFTWARE's header
	return seedSTUN(tb, stun.BindingRequest, stun.NewUsername("srv:cli"),
		rawAttr{stun.AttrSoftware, bytes.Repeat([]byte{'s'}, size-fixed)})
}

// emptyAttributes is a Binding request of n empty attributes and then USERNAME "srv:cli" (n+1 attributes in all), the
// message that costs stun's decoder the most per byte.
func emptyAttributes(tb testing.TB, n int) []byte {
	tb.Helper()
	setters := []stun.Setter{stun.BindingRequest}
	for range n {
		setters = append(setters, rawAttr{0x8030, nil})
	}
	return seedSTUN(tb, append(setters, stun.NewUsername("srv:cli"))...)
}

// guardSeeds returns the packets both targets start from: a connectivity check as browsers and Pion send it, STUN
// without a USERNAME, every way a USERNAME's ufrag can be cut, the attribute orders that stun's decoder treats
// specially, broken headers and attributes, the sizes around Pion's and the guard's frame limits, and media.
func guardSeeds(tb testing.TB) []guardSeed {
	tb.Helper()
	request := seedSTUN(tb, stun.BindingRequest, stun.NewUsername("srvufrag:cliufrag"), ice.PriorityAttr(0x6e0001ff),
		ice.AttrControlling(0x1234567890), ice.UseCandidate(), stun.NewShortTermIntegrity(seedPwd), stun.Fingerprint)
	user := func(username string) []byte {
		return seedSTUN(tb, stun.BindingRequest, rawAttr{stun.AttrUsername, []byte(username)})
	}
	patched := func(b []byte, fn func(b []byte)) []byte {
		b = bytes.Clone(b)
		fn(b)
		return b
	}
	rtp := []byte{0x80, 0x60, 0x00, 0x01, 0x01, 0x02, 0x03, 0x04, 0x0a, 0x0b, 0x0c, 0x0d, 0xde, 0xad, 0xbe, 0xef,
		0x01, 0x02, 0x03, 0x04}
	return []guardSeed{
		{"binding-request", request, "srvufrag", true, true},
		{"binding-request-without-username", seedSTUN(tb, stun.BindingRequest, stun.Fingerprint), "", false, false},
		{"binding-success", seedSTUN(tb, stun.BindingSuccess,
			&stun.XORMappedAddress{IP: net.ParseIP("203.0.113.7"), Port: 5000}, stun.NewShortTermIntegrity(seedPwd),
			stun.Fingerprint), "", false, false},
		{"username-without-colon", user("srvufrag"), "srvufrag", true, true},
		{"username-empty", user(""), "", true, true},
		{"username-colon-first", user(":cliufrag"), "", true, true},
		{"username-several-colons", user("a:b:c"), "a", true, true},
		{"username-not-utf8", user("\xff\x00srv:\xfe"), "\xff\x00srv", true, true},
		{"username-long", user(strings.Repeat("u", 600) + ":peer"), strings.Repeat("u", 600), true, false},
		{"two-usernames", seedSTUN(tb, stun.BindingRequest, rawAttr{stun.AttrUsername, []byte("first:x")},
			rawAttr{stun.AttrUsername, []byte("second:y")}), "first", true, true},
		// stun's decoder drops what follows MESSAGE-INTEGRITY (but not what follows FINGERPRINT alone).
		{"username-after-integrity", seedSTUN(tb, stun.BindingRequest, stun.NewShortTermIntegrity(seedPwd),
			stun.NewUsername("srv:cli")), "srv", false, false},
		{"username-after-fingerprint", seedSTUN(tb, stun.BindingRequest, stun.Fingerprint,
			stun.NewUsername("srv:cli")), "srv", true, true},
		{"indication", seedSTUN(tb, stun.NewType(stun.MethodBinding, stun.ClassIndication),
			stun.NewUsername("srv:cli")), "srv", true, true},
		{"allocate-request", seedSTUN(tb, stun.NewType(stun.MethodAllocate, stun.ClassRequest),
			stun.NewUsername("srv:cli")), "srv", true, false},
		{"top-type-bits-set", patched(user("srv:cli"), func(b []byte) { b[0] |= 0xc0 }), "srv", true, true},
		{"trailing-bytes", append(user("srv:cli"), "junk!"...), "srv", true, true},
		{"truncated-header", request[:19], "srvufrag", false, false},
		{"truncated-header-short", request[:4], "srvufrag", false, false},
		{"empty", []byte{}, "", false, false},
		{"truncated-body", request[:len(request)-8], "srvufrag", false, false},
		{"bad-cookie", patched(request, func(b []byte) { b[4] ^= 0xff }), "srvufrag", false, false},
		{"username-past-end", patched(user("srv:cli"), func(b []byte) { binary.BigEndian.PutUint16(b[22:], 64) }),
			"srv", false, false},
		// A broken attribute after a good USERNAME: the whole message fails to decode, and Pion routes nothing.
		{"username-then-broken-attribute", patched(seedSTUN(tb, stun.BindingRequest, stun.NewUsername("srv:cli"),
			rawAttr{stun.AttrSoftware, []byte("x")}), func(b []byte) { binary.BigEndian.PutUint16(b[34:], 200) }),
			"srv", false, false},
		{"many-empty-attributes", emptyAttributes(tb, 1000), "srv", true, false},
		{"pion-first-frame-max", paddedRequest(tb, pionTCPFirstFrameMax), "srv", true, true},
		{"pion-first-frame-max-plus-4", paddedRequest(tb, pionTCPFirstFrameMax+4), "srv", true, false},
		{"guard-first-frame-max", paddedRequest(tb, maxFirstFrame), "srv", true, false},
		{"guard-first-frame-max-plus-4", paddedRequest(tb, maxFirstFrame+4), "srv", true, false},
		{"rtp", rtp, "", false, false},
		{"rtp-with-cookie", patched(rtp, func(b []byte) { binary.BigEndian.PutUint32(b[4:], 0x2112a442) }), "", false,
			false},
		{"dtls-client-hello", []byte{0x16, 0xfe, 0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x00, 0x10, 0x01, 0x00, 0x00, 0x0c,
			0x00, 0x00}, "", false, false},
	}
}

// longSeeds are FuzzStunUfrag's seeds beyond guardSeeds: the longest attribute lists, of 14593 empty attributes and
// a USERNAME (where stun's decoder grows its slice for the last time, its worst case per byte) and of 16381 (the
// most that fit next to the USERNAME). Not in guardSeeds, whose packets must fit Pion's 8 KiB reads
// (TestPionRoutesLikeReference).
func longSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	return [][]byte{emptyAttributes(tb, 14592), emptyAttributes(tb, 16380)}
}

// stunUfragAllocRuns is the number of calls over which testing.AllocsPerRun averages stunUfrag's allocations. An
// allocation of stunUfrag happens in every call, so the average shows it; what other goroutines allocate in the
// meantime has to add up to this many before it does.
const stunUfragAllocRuns = 10

// FuzzStunUfrag: for any packet, stunUfrag returns the ufrag Pion's UDP mux routes it by (nothing when the mux
// doesn't route it by ufrag), as a part of the packet, without changing it and without allocating; and admitUDP
// counts the packet's source for that ufrag exactly when the mux routes it by ufrag.
func FuzzStunUfrag(f *testing.F) {
	for _, s := range guardSeeds(f) {
		f.Add(s.raw)
	}
	for _, b := range longSeeds(f) {
		f.Add(b)
	}
	first := netip.MustParseAddrPort("192.0.2.1:1000")
	second := netip.MustParseAddrPort("[2001:db8::1]:1000")
	f.Fuzz(func(t *testing.T, b []byte) {
		in := bytes.Clone(b)
		want, wantOK := pionUfrag(b)

		got, ok := stunUfrag(b)
		if allocs := testing.AllocsPerRun(stunUfragAllocRuns, func() { _, _ = stunUfrag(b) }); allocs != 0 {
			t.Fatalf("stunUfrag allocated %v times per call for %d input bytes", allocs, len(b))
		}
		if !bytes.Equal(b, in) {
			t.Fatal("stunUfrag changed its input")
		}
		if ok != wantOK || string(got) != want {
			t.Fatalf("stunUfrag = %q, %v; Pion's mux routes by %q, %v", got, ok, want, wantOK)
		}
		if ok {
			// The ufrag aliases the packet and lies inside it.
			off := cap(b) - cap(got)
			if off < 0 || off+len(got) > len(b) || !bytes.Equal(b[off:off+len(got)], got) {
				t.Fatalf("the ufrag is not inside the packet (offset %d, len %d of %d)", off, len(got), len(b))
			}
		}

		// admitUDP: with a limit of one source, a second source is refused exactly when Pion routes the packet by
		// the ufrag; the counted source passes again.
		g := newSourceGuard(1, nil)
		if wantOK {
			g.open(want)
		}
		if !g.admitUDP(b, first) {
			t.Fatal("admitUDP refused the first source")
		}
		if refused := !g.admitUDP(b, second); refused != wantOK {
			t.Fatalf("admitUDP refused a second source: %v; Pion routes the packet by ufrag: %v", refused, wantOK)
		}
		if !g.admitUDP(b, first) {
			t.Fatal("admitUDP refused the source it counted")
		}
		wantRefused := uint64(0)
		if wantOK {
			wantRefused = 1
			if g.sources(want) != 1 {
				t.Fatalf("the guard counts %d sources for the packet's ufrag, want 1", g.sources(want))
			}
		}
		if g.refused.Load() != wantRefused {
			t.Fatalf("refused %d packets, want %d", g.refused.Load(), wantRefused)
		}
	})
}

// FuzzStunUfragDecode is the differential target: for any packet, stunUfrag returns exactly what decodedUfrag
// returns, nothing or the same part of the packet (sameAsDecoded). It tries every packet also behind a STUN header
// whose length covers it (behindHeader), which takes nearly every input past the header checks into the attribute
// walk. Its seeds, which every go test run replays, are every packet in FuzzStunUfrag's and FuzzFirstFrameGate's
// corpora (stunCorpus) and walkSeeds, each also without its 20-byte header, as an attribute list to put behind one.
func FuzzStunUfragDecode(f *testing.F) {
	seeds := stunCorpus(f)
	for _, s := range walkSeeds(f) {
		seeds = append(seeds, s.raw)
	}
	var attrLists [][]byte
	for _, b := range seeds {
		if len(b) > stunHeaderSize {
			attrLists = append(attrLists, b[stunHeaderSize:])
		}
	}
	for _, b := range distinct(append(seeds, attrLists...)) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sameAsDecoded(t, b)
		sameAsDecoded(t, behindHeader(b))
	})
}

// sameAsDecoded fails t unless stunUfrag returns for b what decodedUfrag returns: both not ok and nil, or both ok and
// the same bytes at the same place in b. Both results alias b and end where b's capacity ends, so the same length
// and capacity are the same place.
func sameAsDecoded(t *testing.T, b []byte) {
	t.Helper()
	got, ok := stunUfrag(b)
	want, wantOK := decodedUfrag(b)
	if ok != wantOK || (got == nil) != (want == nil) || len(got) != len(want) || cap(got) != cap(want) {
		t.Fatalf("stunUfrag returns %s; the decoding version returns %s (of %d bytes)", result(b, got, ok),
			result(b, want, wantOK), len(b))
	}
}

// result describes what stunUfrag or decodedUfrag returned for b.
func result(b, ufrag []byte, ok bool) string {
	if ufrag == nil {
		return fmt.Sprintf("nil, %v", ok)
	}
	return fmt.Sprintf("%.40q at %d, %v", ufrag, cap(b)-cap(ufrag), ok)
}

// stunMagicCookie is the STUN header's magic cookie (RFC 8489 §5).
const stunMagicCookie = 0x2112a442

// rawMessage is a Binding request's header whose length field says size, then the parts as they are, whatever size
// says.
func rawMessage(size int, parts ...[]byte) []byte {
	m := binary.BigEndian.AppendUint16(nil, stun.BindingRequest.Value())
	m = binary.BigEndian.AppendUint16(m, uint16(size&0xffff))
	m = binary.BigEndian.AppendUint32(m, stunMagicCookie)
	m = append(m, "isshoni-walk"...) // the transaction ID
	for _, p := range parts {
		m = append(m, p...)
	}
	return m
}

// behindHeader returns b behind a Binding request's header whose length is len(b) (in 16 bits: bytes past 65535
// trail the message).
func behindHeader(b []byte) []byte {
	return rawMessage(len(b), b)
}

// attrOf is an attribute's header whose type is typ and whose length field says n, then value as it is.
func attrOf(typ stun.AttrType, n int, value string) []byte {
	a := binary.BigEndian.AppendUint16(nil, uint16(typ))
	a = binary.BigEndian.AppendUint16(a, uint16(n&0xffff))
	return append(a, value...)
}

// walkSeed is a message whose attribute list stunUfrag must take or refuse as stun's decoder does, and the result.
type walkSeed struct {
	name  string
	raw   []byte
	ufrag string
	ok    bool
}

// walkSeeds returns the messages that test the attribute walk at its edges: a length field that cuts an attribute
// or its padding, a broken attribute after a good USERNAME, the integrity attributes after which the decoder drops a
// USERNAME, and the longest values.
func walkSeeds(tb testing.TB) []walkSeed {
	tb.Helper()
	attr := func(typ stun.AttrType, value string) []byte { // padded to a multiple of 4
		return attrOf(typ, len(value), value+strings.Repeat("\x00", -len(value)&3))
	}
	user := func(username string) []byte { return attr(stun.AttrUsername, username) }
	zeros := func(n int) string { return strings.Repeat("\x00", n) }
	msg := func(attrs ...[]byte) []byte { return rawMessage(len(bytes.Join(attrs, nil)), attrs...) }
	integrity := attr(stun.AttrMessageIntegrity, zeros(20))
	integrity256 := attr(stun.AttrMessageIntegritySHA256, zeros(32))
	fingerprint := attr(stun.AttrFingerprint, zeros(4))
	long := strings.Repeat("u", 0xfff7)
	return []walkSeed{
		{"no-attributes", msg(), "", false},
		{"username", msg(user("srv:cli")), "srv", true},
		{"length-cuts-an-attribute-header", rawMessage(14, user("srv:cli"), []byte{0x80, 0x22}), "", false},
		{"length-cuts-the-padding", rawMessage(11, user("srv:cli")), "", false},
		{"length-cuts-the-padding-after-username", rawMessage(17, user("srv:cli"), attr(stun.AttrSoftware, "x")), "",
			false},
		{"padding-missing", rawMessage(11, attrOf(stun.AttrUsername, 7, "srv:cli")), "", false},
		{"value-past-length", rawMessage(8, user("srv:cli")), "", false},
		{"attribute-longer-than-message", append(rawMessage(12, attrOf(stun.AttrUsername, 9, "srv:cli")), 0, 0, 0, 0),
			"", false},
		{"length-zero", rawMessage(0, user("srv:cli")), "", false},
		{"bytes-past-length", append(msg(user("srv:cli")), user("other:cli")...), "srv", true},
		{"username-after-integrity-sha256", msg(integrity256, user("srv:cli")), "", false},
		{"username-after-both-integrities", msg(integrity, integrity256, user("srv:cli")), "", false},
		{"username-after-sha256-then-integrity", msg(integrity256, integrity, user("srv:cli")), "", false},
		{"username-after-fingerprint-then-integrity", msg(fingerprint, integrity, user("srv:cli")), "", false},
		{"usernames-around-integrity", msg(user("first:x"), integrity, user("second:y")), "first", true},
		{"broken-attribute-after-integrity", msg(user("srv:cli"), integrity, attrOf(stun.AttrSoftware, 200, "abcd")),
			"", false},
		{"empty-username-first", msg(user(""), user("srv:cli")), "", true},
		{"compat-xor-mapped-address-first", msg(attr(0x8020, zeros(8)), user("srv:cli")), "srv", true},
		{"longest-username", msg(user(long + ":")), long, true},
		{"username-longer-than-a-message", rawMessage(0xffff, attrOf(stun.AttrUsername, 0xffff, long+"uuuu")), "",
			false},
	}
}

// stunCorpus returns every packet in FuzzStunUfrag's and FuzzFirstFrameGate's corpora once: their seeds and the
// inputs committed under testdata/fuzz, and of each FuzzFirstFrameGate stream also its first frame, which is what the
// gate gives stunUfrag.
func stunCorpus(tb testing.TB) [][]byte {
	tb.Helper()
	seeds := guardSeeds(tb)
	packets := longSeeds(tb)
	var streams [][]byte
	for _, s := range seeds {
		packets = append(packets, s.raw)
		streams = append(streams, gateStream(s.raw))
	}
	streams = append(streams, gateEdgeStreams(seeds[0].raw)...)
	packets = append(packets, committedInputs(tb, "FuzzStunUfrag")...)
	streams = append(streams, committedInputs(tb, "FuzzFirstFrameGate")...)
	for _, s := range streams {
		packets = append(packets, s)
		if len(s) >= 2 {
			if size := int(binary.BigEndian.Uint16(s)); len(s) >= 2+size {
				packets = append(packets, s[2:2+size])
			}
		}
	}
	return distinct(packets)
}

// committedInputs returns the first argument, a []byte, of every input committed under testdata/fuzz/<target>. It
// fails tb when there is none, or when an input isn't in the format go test writes ("go test fuzz v1", then one line
// per argument).
func committedInputs(tb testing.TB, target string) [][]byte {
	tb.Helper()
	dir := filepath.Join("testdata", "fuzz", target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatal(err)
	}
	var inputs [][]byte
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			tb.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) < 2 || strings.TrimSpace(lines[0]) != "go test fuzz v1" {
			tb.Fatalf("%s/%s is not a fuzz input", dir, e.Name())
		}
		arg := strings.TrimSpace(lines[1])
		quoted, isBytes := strings.CutPrefix(arg, "[]byte(")
		quoted, closed := strings.CutSuffix(quoted, ")")
		b, err := strconv.Unquote(quoted)
		if !isBytes || !closed || err != nil {
			tb.Fatalf("%s/%s: the first argument is not a []byte: %.40q", dir, e.Name(), arg)
		}
		inputs = append(inputs, []byte(b))
	}
	if len(inputs) == 0 {
		tb.Fatalf("no inputs committed under %s", dir)
	}
	return inputs
}

// distinct returns bs without repetitions, in their order.
func distinct(bs [][]byte) [][]byte {
	seen := make(map[string]bool, len(bs))
	var out [][]byte
	for _, b := range bs {
		if !seen[string(b)] {
			seen[string(b)] = true
			out = append(out, b)
		}
	}
	return out
}

// TestStunUfragWalk: stunUfrag, and the decoding version alike, take or refuse each of walkSeeds with the table's
// ufrag, so the table also pins stun's decoder.
func TestStunUfragWalk(t *testing.T) {
	for _, s := range walkSeeds(t) {
		for _, fn := range []struct {
			name  string
			ufrag func([]byte) ([]byte, bool)
		}{{"stunUfrag", stunUfrag}, {"decodedUfrag", decodedUfrag}} {
			if got, ok := fn.ufrag(s.raw); ok != s.ok || string(got) != s.ufrag {
				t.Errorf("%s: %s = %.20q, %v; want %.20q, %v", s.name, fn.name, got, ok, s.ufrag, s.ok)
			}
		}
	}
}

// TestStunUfragAllocations: stunUfrag allocates nothing for guardSeeds, walkSeeds and longSeeds, which hold the
// longest attribute lists and the longest USERNAME a STUN header's length allows, nor for an 8 KiB packet of empty
// attributes (Pion's UDP read size), for which the decoding version allocates about 190 KB. Nor does admitUDP for a
// packet it judges by its ufrag, whether it admits the source or drops the packet, or for one it lets through
// unjudged.
func TestStunUfragAllocations(t *testing.T) {
	type input struct {
		name string
		raw  []byte
	}
	udpRead := emptyAttributes(t, 2040)
	if len(udpRead) != 8192 {
		t.Fatalf("the 8 KiB packet has %d bytes", len(udpRead))
	}
	inputs := []input{{"8-KiB-of-empty-attributes", udpRead}}
	for i, b := range longSeeds(t) {
		inputs = append(inputs, input{"long-seed-" + strconv.Itoa(i), b})
	}
	for _, s := range guardSeeds(t) {
		inputs = append(inputs, input{s.name, s.raw})
	}
	for _, s := range walkSeeds(t) {
		inputs = append(inputs, input{"walk/" + s.name, s.raw})
	}
	for _, in := range inputs {
		if allocs := testing.AllocsPerRun(100, func() { _, _ = stunUfrag(in.raw) }); allocs != 0 {
			t.Errorf("%s: stunUfrag allocated %v times per call", in.name, allocs)
		}
	}

	g := newSourceGuard(1, nil)
	g.open("srv")
	counted, other := netip.MustParseAddrPort("192.0.2.1:1000"), netip.MustParseAddrPort("[2001:db8::1]:1000")
	if !g.admitUDP(udpRead, counted) {
		t.Fatal("the first source was refused")
	}
	for _, tc := range []struct {
		name  string
		raw   []byte
		from  netip.AddrPort
		admit bool
	}{
		{"counted source", udpRead, counted, true},
		{"source over the limit", udpRead, other, false},
		{"unknown ufrag", seedSTUN(t, stun.BindingRequest, stun.NewUsername("other:cli")), other, true},
		{"no USERNAME", seedSTUN(t, stun.BindingSuccess), other, true},
		{"not STUN", []byte{0x80, 0x60, 0x00, 0x01, 1, 2, 3, 4, 5, 6, 7, 8}, other, true},
	} {
		if got := g.admitUDP(tc.raw, tc.from); got != tc.admit {
			t.Errorf("%s: admitUDP = %v, want %v", tc.name, got, tc.admit)
		}
		if allocs := testing.AllocsPerRun(100, func() { _ = g.admitUDP(tc.raw, tc.from) }); allocs != 0 {
			t.Errorf("%s: admitUDP allocated %v times per call", tc.name, allocs)
		}
	}
}

// streamConn is the server end of an ICE-TCP connection whose peer sent stream and closed: each Read returns at
// most chunk bytes, then io.EOF. firstFrameGate uses only its Read and RemoteAddr.
type streamConn struct {
	net.Conn
	stream []byte
	read   int // bytes of stream read
	chunk  int
	remote net.Addr
}

func (c *streamConn) Read(b []byte) (int, error) {
	rest := c.stream[c.read:]
	if len(rest) == 0 {
		return 0, io.EOF
	}
	n := copy(b, rest[:min(len(rest), c.chunk)])
	c.read += n
	return n, nil
}

func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

// taken returns the part of the stream read so far.
func (c *streamConn) taken() []byte { return c.stream[:c.read] }

// stringAddr is a remote address that isn't a *net.TCPAddr, like a wrapped connection's.
type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// gateRemotes are the remote addresses FuzzFirstFrameGate's connections come from. counted: the guard can key the
// address (tcpSource), so it counts it and may refuse it; one it can't key it lets through.
var gateRemotes = []struct {
	addr    net.Addr
	counted bool
}{
	{net.TCPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.7:10000")), true},
	{net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[::ffff:203.0.113.7]:10000")), true},
	{&net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 10000, Zone: "eth0"}, true},
	{net.TCPAddrFromAddrPort(netip.MustParseAddrPort("[2001:db8::7]:443")), true},
	{stringAddr("203.0.113.9:4000"), true},
	{stringAddr("pipe"), false},
	{nil, false},
	{(*net.TCPAddr)(nil), false},
	{&net.TCPAddr{}, false},
}

// gateFiller is the source that fills a ufrag's one slot in FuzzFirstFrameGate (none of gateRemotes).
var gateFiller = iceSource{tcp: true, addr: netip.MustParseAddrPort("198.51.100.1:1")}

// gateMode is FuzzFirstFrameGate's mode byte: bit 0 makes the first frame's ufrag (as Pion routes it) known to the
// guard, bit 1 fills its one slot with another source, and the bits above pick the remote address.
func gateMode(known, full bool, remote uint8) uint8 {
	m := remote << 2
	if known {
		m |= 1
	}
	if full {
		m |= 2
	}
	return m
}

// gateStream is the stream FuzzFirstFrameGate starts from for one of guardSeeds: the packet as the first frame, then
// another frame.
func gateStream(raw []byte) []byte {
	return append(frameOf(raw), frameOf([]byte("the next frame"))...)
}

// gateEdgeStreams are FuzzFirstFrameGate's other streams, the edges of the first frame's framing; request is
// guardSeeds' Binding request.
func gateEdgeStreams(request []byte) [][]byte {
	framed := frameOf(request)
	tail := frameOf([]byte("the next frame"))
	return [][]byte{
		{},           // closed at once
		{0x00},       // a truncated header
		{0x00, 0x14}, // a header, then nothing
		append([]byte{0x05, 0xdd}, make([]byte, maxFirstFrame+1)...), // one byte over the limit
		append([]byte{0xff, 0xff}, framed...),                        // the longest header there is
		append(frameOf(make([]byte, maxFirstFrame)), tail...),        // the longest first frame the gate reads
		append(frameOf(nil), tail...),                                // an empty first frame
		framed[:30],                                                  // a truncated frame
	}
}

// FuzzFirstFrameGate: whatever a connection sends, and however the bytes arrive and are read, the gate takes no
// more than the first frame from the connection and allocates within its bound. It refuses the connection when the
// frame is longer than maxFirstFrame, or when Pion would route the frame by a known ufrag that has no slot left for
// the remote address, and fails it when the stream ends inside the first frame. Otherwise the reader gets the
// stream byte for byte, and the guard has counted the remote address for the frame's ufrag. The arguments: the bytes
// the peer sent, the mode (gateMode), and the most bytes one Read of the connection returns and of the gate asks
// for, minus one.
func FuzzFirstFrameGate(f *testing.F) {
	seeds := guardSeeds(f)
	var remote, chunk uint8 // the second seed of each packet cycles through the remotes and small chunks
	for _, s := range seeds {
		stream := gateStream(s.raw)
		f.Add(stream, gateMode(true, true, 0), uint8(255), uint8(255))
		f.Add(stream, gateMode(true, remote%2 == 0, remote), chunk, uint8(1))
		if remote++; int(remote) == len(gateRemotes) {
			remote = 0
		}
		chunk = (chunk + 1) % 5
	}
	for _, stream := range gateEdgeStreams(seeds[0].raw) {
		f.Add(stream, gateMode(true, true, 0), uint8(255), uint8(255))
		f.Add(stream, gateMode(false, false, 1), uint8(0), uint8(0))
	}
	f.Fuzz(func(t *testing.T, stream []byte, mode, chunk, readSize uint8) {
		remote := gateRemotes[int(mode>>2)%len(gateRemotes)]
		known, full := mode&1 != 0, mode&2 != 0

		// The stream's first frame (RFC 4571: a 2-byte length, then the bytes), and what the gate must do with it.
		var size int
		var frame []byte
		var ufrag string
		var routed bool
		if len(stream) >= 2 {
			size = int(binary.BigEndian.Uint16(stream))
			if size <= maxFirstFrame && len(stream) >= 2+size {
				frame = stream[2 : 2+size]
				ufrag, routed = pionUfrag(frame)
			}
		}
		tooLarge := size > maxFirstFrame
		refuse := routed && known && full && remote.counted

		// Two connections with the same guard state: the second is read only to measure the allocation again.
		newGate := func() (*firstFrameGate, *streamConn, *sourceGuard) {
			g := newSourceGuard(1, nil)
			if routed && known {
				g.open(ufrag)
				if full && !g.admit([]byte(ufrag), gateFiller) {
					t.Fatal("the filler source was refused")
				}
			}
			c := &streamConn{stream: stream, chunk: int(chunk) + 1, remote: remote.addr}
			return newFirstFrameGate(c, g), c, g
		}
		gate, conn, g := newGate()
		twin, _, _ := newGate()
		buf, twinBuf := make([]byte, int(readSize)+1), make([]byte, int(readSize)+1)
		var n int
		var err error
		alloc := allocatedBytes(func() { n, err = gate.Read(buf) })
		if limit := frameAllocPerByte*uint64(len(conn.taken())) + frameAllocSlack; alloc > limit {
			// Once more, without what a first call in this process may pay for.
			if alloc = allocatedBytes(func() { _, _ = twin.Read(twinBuf) }); alloc > limit {
				t.Fatalf("the first Read allocated %d bytes after taking %d (limit %d)", alloc, conn.read, limit)
			}
		}

		switch {
		case len(stream) < 2 || (!tooLarge && frame == nil):
			if n != 0 || !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("a stream that ends in the first frame: Read = %d, %v", n, err)
			}
			if conn.read != len(stream) {
				t.Fatalf("took %d bytes of a stream of %d", conn.read, len(stream))
			}
			return
		case tooLarge:
			if n != 0 || !errors.Is(err, errFirstFrameTooLarge) {
				t.Fatalf("a first frame of %d bytes: Read = %d, %v", size, n, err)
			}
			if conn.read != 2 {
				t.Fatalf("took %d bytes of a first frame that is too large, want its header only", conn.read)
			}
			return
		case refuse:
			if n != 0 || !errors.Is(err, errICESourceLimit) {
				t.Fatalf("a source over the limit: Read = %d, %v", n, err)
			}
			if conn.read != 2+size {
				t.Fatalf("took %d bytes, want the first frame's %d", conn.read, 2+size)
			}
			if g.refused.Load() != 1 || g.sources(ufrag) != 1 {
				t.Fatalf("refused %d, %d sources counted; want 1 and the filler", g.refused.Load(), g.sources(ufrag))
			}
			return
		}

		// Admitted: the reader gets the stream unchanged, and the gate took only the first frame ahead.
		if err != nil || n == 0 {
			t.Fatalf("the first Read = %d, %v", n, err)
		}
		if conn.read != 2+size {
			t.Fatalf("the first Read took %d bytes, want the first frame's %d", conn.read, 2+size)
		}
		got := bytes.Clone(buf[:n])
		for {
			n, err := gate.Read(buf)
			if n > len(buf) || n == 0 && err == nil {
				t.Fatalf("Read = %d, %v into %d bytes", n, err, len(buf))
			}
			got = append(got, buf[:n]...)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("Read: %v", err)
				}
				break
			}
		}
		if !bytes.Equal(got, stream) {
			t.Fatalf("read %q, want %q", got, stream)
		}
		if routed && known {
			want := 0 // the filler, or the remote address when the guard can key it
			if full || remote.counted {
				want = 1
			}
			if g.sources(ufrag) != want {
				t.Fatalf("the guard counts %d sources for the frame's ufrag, want %d", g.sources(ufrag), want)
			}
		} else if g.known() != 0 {
			t.Fatalf("the guard knows %d ufrags", g.known())
		}
		if g.refused.Load() != 0 {
			t.Fatalf("refused %d", g.refused.Load())
		}
	})
}

// TestPionRoutesLikeReference pins the targets' reference to Pion: every seed goes through a real UDP mux and a real
// TCP mux, each with a connection for the seed's ufrag, and reaches that connection exactly when pionUfrag (and
// pionTCPTakes for TCP) says so, and the seeds' table agrees. So when an update of pion/ice changes how its muxes
// route, this test fails before the fuzz targets compare the guard with a stale copy.
func TestPionRoutesLikeReference(t *testing.T) {
	pionLog := logx.NewPionLoggerFactory(slog.New(slog.DiscardHandler)).NewLogger("ice")
	seeds := guardSeeds(t)
	for _, s := range seeds {
		ufrag, ok := pionUfrag(s.raw)
		if ok != s.udp || ok && ufrag != s.ufrag || pionTCPTakes(s.raw) != s.tcp || s.tcp && !s.udp {
			t.Errorf("%s: pionUfrag = %q, %v and pionTCPTakes = %v; the table says %q, %v, %v", s.name, ufrag, ok,
				pionTCPTakes(s.raw), s.ufrag, s.udp, s.tcp)
		}
	}

	// A new mux for each seed: Pion removes a closed connection's ufrag asynchronously, which could take the next
	// seed's connection for the same ufrag with it.
	for _, s := range seeds {
		t.Run("udp/"+s.name, func(t *testing.T) {
			pc := newMemPacketConn("192.0.2.10:3478")
			mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{Logger: pionLog, UDPConn: pc})
			defer func() { _ = mux.Close() }()
			conn, err := mux.GetConn(s.ufrag, pc.LocalAddr())
			if err != nil {
				t.Fatal(err)
			}
			// The seed, then a marker that the mux routes to the connection, each from an address it doesn't know.
			// The mux handles packets in order, so the connection reads the seed first exactly when it was routed
			// there (the mux drops STUN it can't route by ufrag without calling OnUnhandledPacket).
			marker := seedSTUN(t, stun.BindingRequest, rawAttr{stun.AttrUsername, []byte(s.ufrag + ":marker")})
			pc.in <- memPacket{b: s.raw, from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.7:20000"))}
			pc.in <- memPacket{b: marker, from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.7:20001"))}
			want, what := marker, "the marker (the seed unrouted)"
			if s.udp {
				want, what = s.raw, "the seed"
			}
			if got, err := readPacket(conn, 5*time.Second); err != nil || !bytes.Equal(got, want) {
				t.Errorf("the connection of %q read %q, %v; want %s", s.ufrag, got, err, what)
			}
		})
	}

	local := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.10:7882"))
	remote := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.7:20000"))
	for _, s := range seeds {
		t.Run("tcp/"+s.name, func(t *testing.T) {
			ln := newFakeListener()
			mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: ln, Logger: pionLog, ReadBufferSize: 8,
				FirstStunBindTimeout: 5 * time.Second, AliveDurationForConnFromStun: 5 * time.Second})
			defer func() { _ = mux.Close() }()
			conn, err := mux.GetConnByUfrag(s.ufrag, false, local.IP)
			if err != nil {
				t.Fatal(err)
			}
			server, client := net.Pipe()
			ln.conns <- tcpPipe{Conn: server, local: local, remote: remote}
			wrote := make(chan struct{})
			defer func() {
				_ = client.Close()
				<-wrote
			}()
			go func() {
				defer close(wrote)
				_, _ = client.Write(frameOf(s.raw)) // fails once Pion closes the connection
			}()
			if s.tcp {
				if got, err := readPacket(conn, 5*time.Second); err != nil || !bytes.Equal(got, s.raw) {
					t.Errorf("the connection of %q read %d bytes, %v; want the frame", s.ufrag, len(got), err)
				}
				return
			}
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Errorf("read %d bytes, %v; want the connection closed without an answer", n, err)
			}
			_ = conn.Close()
		})
	}
}

// readPacket reads one packet from c, or fails after wait (a muxed connection has no read deadline). It closes c.
func readPacket(c net.PacketConn, wait time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 8192)
		n, _, err := c.ReadFrom(buf)
		done <- result{buf[:n], err}
	}()
	select {
	case r := <-done:
		_ = c.Close()
		return r.b, r.err
	case <-time.After(wait):
		_ = c.Close()
		<-done
		return nil, errors.New("no packet")
	}
}

// tcpPipe is one end of a net.Pipe with TCP addresses, as Pion's TCP mux needs them.
type tcpPipe struct {
	net.Conn
	local, remote net.Addr
}

func (c tcpPipe) LocalAddr() net.Addr  { return c.local }
func (c tcpPipe) RemoteAddr() net.Addr { return c.remote }
