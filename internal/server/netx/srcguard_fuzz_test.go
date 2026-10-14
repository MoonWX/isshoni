package netx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
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
// (pionUfrag, which TestPionRoutesLikeReference pins to the real muxes).

// Allocation bounds of the targets: a part that grows with the bytes parsed, and a constant for what doesn't (the
// error values that stun's decoder formats, the guard's map entry for a new source, the size classes the allocator
// rounds up to).
//
// stunUfrag decodes the message with stun.Message.Decode, as Pion's muxes do. Decode keeps a 32-byte RawAttribute
// for each attribute, which can be as short as 4 bytes, in a slice that append grows, and the slices it outgrew are
// garbage too. A message of nothing but empty attributes is the worst case: it measured up to 43 bytes per input
// byte (at about 14600 attributes; a STUN header's length ends the count at 16380). That is Pion's own cost for every
// STUN message its muxes route by ufrag and its agent reads, and the guard pays it once more per packet. Linear, and
// the input is bounded: Pion reads UDP into 8 KiB, and the gate reads at most maxFirstFrame. firstFrameGate also
// copies the frame it reads ahead, so its bound counts the bytes it took from the connection, which must be at most
// 2 + maxFirstFrame whatever length the frame's header claims.
const (
	stunAllocPerByte  = 48
	stunAllocSlack    = 2 << 10
	frameAllocPerByte = stunAllocPerByte + 2
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
	manyEmpty := []stun.Setter{stun.BindingRequest}
	for range 1000 {
		manyEmpty = append(manyEmpty, rawAttr{0x8030, nil})
	}
	manyEmpty = append(manyEmpty, stun.NewUsername("srv:cli"))
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
		{"many-empty-attributes", seedSTUN(tb, manyEmpty...), "srv", true, false},
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

// FuzzStunUfrag: for any packet, stunUfrag returns the ufrag Pion's UDP mux routes it by (nothing when the mux
// doesn't route it by ufrag), as a part of the packet, without changing it and within its allocation bound; and
// admitUDP counts the packet's source for that ufrag exactly when the mux routes it by ufrag.
func FuzzStunUfrag(f *testing.F) {
	for _, s := range guardSeeds(f) {
		f.Add(s.raw)
	}
	first := netip.MustParseAddrPort("192.0.2.1:1000")
	second := netip.MustParseAddrPort("[2001:db8::1]:1000")
	f.Fuzz(func(t *testing.T, b []byte) {
		in := bytes.Clone(b)
		want, wantOK := pionUfrag(b)

		var got []byte
		var ok bool
		limit := stunAllocPerByte*uint64(len(b)) + stunAllocSlack
		n := allocatedBytes(func() { got, ok = stunUfrag(b) })
		if n > limit { // once more, without what a first call in this process may pay for
			n = allocatedBytes(func() { got, ok = stunUfrag(b) })
		}
		if n > limit {
			t.Fatalf("stunUfrag allocated %d bytes for %d input bytes (limit %d)", n, len(b), limit)
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

// FuzzFirstFrameGate: whatever a connection sends, and however the bytes arrive and are read, the gate takes no
// more than the first frame from the connection and allocates within its bound. It refuses the connection when the
// frame is longer than maxFirstFrame, or when Pion would route the frame by a known ufrag that has no slot left for
// the remote address, and fails it when the stream ends inside the first frame. Otherwise the reader gets the
// stream byte for byte, and the guard has counted the remote address for the frame's ufrag. The arguments: the bytes
// the peer sent, the mode (gateMode), and the most bytes one Read of the connection returns and of the gate asks
// for, minus one.
func FuzzFirstFrameGate(f *testing.F) {
	tail := frameOf([]byte("the next frame"))
	seeds := guardSeeds(f)
	var remote, chunk uint8 // the second seed of each packet cycles through the remotes and small chunks
	for _, s := range seeds {
		stream := append(frameOf(s.raw), tail...)
		f.Add(stream, gateMode(true, true, 0), uint8(255), uint8(255))
		f.Add(stream, gateMode(true, remote%2 == 0, remote), chunk, uint8(1))
		if remote++; int(remote) == len(gateRemotes) {
			remote = 0
		}
		chunk = (chunk + 1) % 5
	}
	request := frameOf(seeds[0].raw)
	for _, stream := range [][]byte{
		{},           // closed at once
		{0x00},       // a truncated header
		{0x00, 0x14}, // a header, then nothing
		append([]byte{0x05, 0xdd}, make([]byte, maxFirstFrame+1)...), // one byte over the limit
		append([]byte{0xff, 0xff}, request...),                       // the longest header there is
		append(frameOf(make([]byte, maxFirstFrame)), tail...),        // the longest first frame the gate reads
		append(frameOf(nil), tail...),                                // an empty first frame
		request[:30],                                                 // a truncated frame
	} {
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
