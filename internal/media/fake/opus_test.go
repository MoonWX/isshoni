package fake

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// TestOpusAsset: tone.opus is a well-formed Ogg Opus file (RFC 7845) whose audio packets are all valid Opus
// (RFC 6716 §3.4, R1–R7) of 20 ms, with the pre-skip gen-opus.sh shifted the tone by, and granule positions that
// count exactly the two seconds of input.
func TestOpusAsset(t *testing.T) {
	pkts, pages, err := readOgg(toneOpus)
	if err != nil {
		t.Fatal(err)
	}
	head, err := parseOpusHead(pkts[0])
	if err != nil {
		t.Fatal(err)
	}
	if head != (opusHead{version: 1, channels: 2, preSkip: assetPreSkip, inputRate: 48000}) {
		t.Fatalf("OpusHead %+v", head)
	}
	if pages[0].ends != 1 || pages[0].granule != 0 {
		t.Errorf("the OpusHead page holds %d packets, granule %d", pages[0].ends, pages[0].granule)
	}
	checkOpusTags(t, pkts[1])

	audio := pkts[2:]
	samples := 0
	configs := map[byte]int{}
	for i, raw := range audio {
		p, err := parseOpus(raw)
		if err != nil {
			t.Fatalf("audio packet %d: %v", i, err)
		}
		if p.samples() != audioPacketSamples {
			t.Fatalf("audio packet %d: %d samples", i, p.samples())
		}
		configs[p.toc>>3]++
		samples += p.samples()
	}
	t.Logf("%d bytes, %d pages, %d audio packets, TOC configs %v", len(toneOpus), len(pages), len(audio), configs)
	if len(audio) < assetLoopStart+assetLoopPackets {
		t.Fatalf("%d audio packets", len(audio))
	}
	// Granule positions (RFC 7845 §4): the samples of the packets that end on each page, pre-skip included; the
	// last page's trims the end, so that it minus the pre-skip is the input's length.
	done := 0
	for i, pg := range pages {
		if i < 2 {
			continue
		}
		done += pg.ends
		switch want := int64(done * audioPacketSamples); {
		case i < len(pages)-1 && pg.granule != want:
			t.Errorf("page %d: granule %d, want %d", i, pg.granule, want)
		case i == len(pages)-1 && pg.granule != assetPreSkip+2*opusSampleRate:
			t.Errorf("last page: granule %d, want %d (pre-skip and 2 s)", pg.granule, assetPreSkip+2*opusSampleRate)
		}
	}
	if done != len(audio) || samples < assetPreSkip+2*opusSampleRate {
		t.Errorf("%d packets end on audio pages, %d samples", done, samples)
	}

	loop, err := audioLoop()
	if err != nil {
		t.Fatal(err)
	}
	if len(loop) != assetLoopPackets {
		t.Fatalf("a loop of %d packets", len(loop))
	}
	for i, p := range loop {
		if q, err := parseOpus(audio[assetLoopStart+i]); err != nil || q.toc != p.toc || !equalFrames(q.frames, p.frames) {
			t.Fatalf("loop packet %d is not asset packet %d", i, assetLoopStart+i)
		}
	}
}

// checkOpusTags checks the comment header (RFC 7845 §5.2).
func checkOpusTags(t *testing.T, b []byte) {
	t.Helper()
	if len(b) < 16 || string(b[:8]) != "OpusTags" {
		t.Fatalf("OpusTags %q", b[:min(len(b), 8)])
	}
	rest := b[8:]
	field := func() []byte {
		if len(rest) < 4 || int(binary.LittleEndian.Uint32(rest)) > len(rest)-4 {
			t.Fatal("OpusTags runs out")
		}
		n := int(binary.LittleEndian.Uint32(rest))
		f := rest[4 : 4+n]
		rest = rest[4+n:]
		return f
	}
	vendor := field()
	if len(rest) < 4 {
		t.Fatal("OpusTags without a comment count")
	}
	n := int(binary.LittleEndian.Uint32(rest))
	rest = rest[4:]
	var comments []string
	for range n {
		comments = append(comments, string(field()))
	}
	t.Logf("vendor %q, comments %q", vendor, comments)
	if len(rest) != 0 {
		t.Errorf("%d bytes after the comments", len(rest))
	}
}

// TestAudioMarker: every fake audio packet is a valid Opus packet with the asset's frames and the Marker in its
// padding; the whole loop plays in order, packet 0 at every flash instant.
func TestAudioMarker(t *testing.T) {
	loop, err := audioLoop()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range loop {
		m := Marker{Beep: i == 0, Frame: uint32(i) + 50, CaptureNS: int64(i+50) * int64(AudioPacketDuration)}
		pkt := opusWithMarker(p, m)
		got, err := parseOpus(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if got.toc != p.toc|3 || got.padding != MarkerSize || !equalFrames(got.frames, p.frames) {
			t.Fatalf("packet %d: TOC %#x, padding %d, %d frames", i, got.toc, got.padding, len(got.frames))
		}
		if pm, ok := ParseAudioMarker(pkt); !ok || pm != m {
			t.Fatalf("packet %d: ParseAudioMarker = %+v %v, want %+v", i, pm, ok, m)
		}
	}

	// A long padding (length bytes 255 255 2: 254 + 254 + 2): the Marker is read at its start, not its end.
	m := Marker{Beep: true, Frame: 50, CaptureNS: int64(time.Second)}
	pad := append(appendMarker(nil, m), make([]byte, 510-MarkerSize)...)
	long := appendOpusPadded(nil, loop[1], pad)
	if !bytes.Contains(long[:6], []byte{255, 255, 2}) {
		t.Fatalf("long padding header %x", long[:6])
	}
	if got, ok := ParseAudioMarker(long); !ok || got != m {
		t.Errorf("long padding: %+v %v", got, ok)
	}
	pad = append(make([]byte, 510-MarkerSize), appendMarker(nil, m)...)
	if got, ok := ParseAudioMarker(appendOpusPadded(nil, loop[1], pad)); ok {
		t.Errorf("a marker at the end of the padding parsed: %+v", got)
	}

	for _, bad := range [][]byte{
		nil, {0xfc}, {0xfc, 1, 2, 3}, // code 0
		{0xff, 0x01, 18},  // no padding flag
		{0xff, 0x40, 18},  // zero frames
		{0xff, 0x41, 17},  // padding shorter than a marker
		{0xff, 0x41, 255}, // runs out
		append([]byte{0xff, 0x41, 200}, make([]byte, 50)...), // padding longer than the packet
	} {
		if m, ok := ParseAudioMarker(bad); ok {
			t.Errorf("ParseAudioMarker(%x) = %+v, want no marker", bad, m)
		}
	}
}

func equalFrames(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestOpusFraming checks parseOpus against each framing requirement of RFC 6716 §3.4 and appendOpusPadded's
// packets: CBR and VBR code 3, two-byte frame lengths, padding of 254 bytes and more.
func TestOpusFraming(t *testing.T) {
	frame := func(n int) []byte { return bytes.Repeat([]byte{0xa5}, n) }
	celt20 := byte(31<<3 | 1<<2) // CELT FB 20 ms, stereo
	celt2 := byte(28<<3 | 1<<2)  // CELT FB 2.5 ms
	silk60 := byte(3 << 3)       // SILK NB 60 ms
	for _, tc := range []struct {
		name   string
		pkt    []byte
		frames []int
	}{
		{"code 0", append([]byte{celt20}, frame(100)...), []int{100}},
		{"code 0 empty frame (DTX)", []byte{celt20}, []int{0}},
		{"code 1", append([]byte{celt20 | 1}, frame(40)...), []int{20, 20}},
		{"code 2", append([]byte{celt20 | 2, 10}, frame(30)...), []int{10, 20}},
		{"code 2 two-byte length", append([]byte{celt20 | 2, 253, 10}, frame(300)...), []int{293, 7}},
		{"code 3 CBR", append([]byte{celt2 | 3, 3}, frame(30)...), []int{10, 10, 10}},
		{"code 3 VBR padded", append([]byte{celt2 | 3, 0xc2, 2, 5}, frame(12)...), []int{5, 5}},
		{"code 3 120 ms", append([]byte{silk60 | 3, 2}, frame(8)...), []int{4, 4}},
	} {
		p, err := parseOpus(tc.pkt)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var sizes []int
		for _, f := range p.frames {
			sizes = append(sizes, len(f))
		}
		if len(sizes) != len(tc.frames) {
			t.Fatalf("%s: frames %v, want %v", tc.name, sizes, tc.frames)
		}
		for i := range sizes {
			if sizes[i] != tc.frames[i] {
				t.Fatalf("%s: frames %v, want %v", tc.name, sizes, tc.frames)
			}
		}
	}
	for _, tc := range []struct {
		name string
		pkt  []byte
	}{
		{"R1 empty", nil},
		{"R2 code 0 frame of 1276", append([]byte{celt20}, frame(1276)...)},
		{"R3 code 1 odd", append([]byte{celt20 | 1}, frame(41)...)},
		{"R4 code 2 no length", []byte{celt20 | 2}},
		{"R4 code 2 length past the end", append([]byte{celt20 | 2, 50}, frame(40)...)},
		{"R4 code 2 two-byte length cut", []byte{celt20 | 2, 252}},
		{"R5 code 3 no frames", []byte{celt20 | 3, 0}},
		{"R5 code 3 over 120 ms", append([]byte{silk60 | 3, 3}, frame(9)...)},
		{"R5 code 3 49 × 2.5 ms", append([]byte{celt2 | 3, 49}, frame(49)...)},
		{"R6 code 3 no count", []byte{celt20 | 3}},
		{"R6 CBR uneven", append([]byte{celt20 | 3, 2}, frame(9)...)},
		{"R6 padding past the end", append([]byte{celt20 | 3, 0x41, 20}, frame(10)...)},
		{"R6 padding length runs out", []byte{celt20 | 3, 0x41, 255}},
		{"R7 VBR lengths past the end", append([]byte{celt2 | 3, 0x82, 30}, frame(10)...)},
		{"R7 VBR length cut", []byte{celt2 | 3, 0x82, 253}},
		{"R2 VBR last frame of 1276", append([]byte{celt20 | 3, 0x82, 1}, frame(1277)...)},
	} {
		if _, err := parseOpus(tc.pkt); !errors.Is(err, errOpus) {
			t.Errorf("%s: parseOpus = %v, want errOpus", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name   string
		frames []int
		pad    int
	}{
		{"one frame", []int{160}, MarkerSize},
		{"CBR", []int{40, 40, 40}, 3},
		{"VBR", []int{300, 1275, 0, 7}, MarkerSize},
		{"254 padding", []int{10}, 254},
		{"255 padding", []int{10}, 255},
		{"600 padding", []int{10, 11}, 600},
	} {
		p := opusPacket{toc: celt2}
		for _, n := range tc.frames {
			p.frames = append(p.frames, frame(n))
		}
		pkt := appendOpusPadded(nil, p, bytes.Repeat([]byte{7}, tc.pad))
		got, err := parseOpus(pkt)
		if err != nil || got.toc != celt2|3 || got.padding != tc.pad || !equalFrames(got.frames, p.frames) {
			t.Errorf("%s: %v, TOC %#x, padding %d, frames equal %v", tc.name, err, got.toc, got.padding,
				equalFrames(got.frames, p.frames))
		}
	}
}

// TestOggReader: the reader rejects a damaged file.
func TestOggReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"CRC", func(b []byte) []byte { b[100] ^= 1; return b }},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }},
		{"no end of stream", func(b []byte) []byte { return b[:bytes.LastIndex(b, []byte("OggS"))] }},
		{"garbage after the end", func(b []byte) []byte { return append(b, 'x') }},
		{"a page twice", func(b []byte) []byte {
			second := bytes.Index(b[4:], []byte("OggS")) + 4
			third := bytes.Index(b[second+4:], []byte("OggS")) + second + 4
			return append(b[:third:third], b[second:]...)
		}},
	} {
		b := tc.edit(bytes.Clone(toneOpus))
		if _, _, err := readOgg(b); !errors.Is(err, errOpus) {
			t.Errorf("%s: readOgg = %v, want errOpus", tc.name, err)
		}
		if _, err := loadAsset(b); err == nil {
			t.Errorf("%s: loadAsset accepted it", tc.name)
		}
	}
	// A valid Ogg stream without enough audio.
	short := oggFile(t, [][]byte{opusHeadPacket(), []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"), {0xfc, 1}})
	if _, err := loadAsset(short); !errors.Is(err, errOpus) {
		t.Errorf("loadAsset(3 packets) = %v", err)
	}
	pkts, pages, err := readOgg(short)
	if err != nil || len(pkts) != 3 || len(pages) != 1 {
		t.Errorf("readOgg of a one-page file: %d packets, %d pages, %v", len(pkts), len(pages), err)
	}
}

func opusHeadPacket() []byte {
	h := []byte("OpusHead\x01\x02")
	h = binary.LittleEndian.AppendUint16(h, assetPreSkip)
	h = binary.LittleEndian.AppendUint32(h, 48000)
	return append(h, 0, 0, 0)
}

// oggFile writes packets (each under 255 bytes) as one Ogg page with the BOS and EOS flags.
func oggFile(t *testing.T, pkts [][]byte) []byte {
	t.Helper()
	b := []byte("OggS\x00\x06")
	b = binary.LittleEndian.AppendUint64(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0) // CRC
	b = append(b, byte(len(pkts)))
	for _, p := range pkts {
		if len(p) >= 255 {
			t.Fatal("packet too long for oggFile")
		}
		b = append(b, byte(len(p)))
	}
	for _, p := range pkts {
		b = append(b, p...)
	}
	binary.LittleEndian.PutUint32(b[22:], oggCRC(b))
	return b
}

// TestSourceAudioLoops: the Source plays the loop in order and starts it again every second, the Beep packets at
// loop packet 0.
func TestSourceAudioLoops(t *testing.T) {
	s, err := New(Config{Layers: []VideoLayer{}, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loop, err := audioLoop()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2*assetLoopPackets + 1 {
		st := s.streams[0]
		p := st.audioPacket(s.flashEvery)
		got, err := parseOpus(p.Data)
		if err != nil || !equalFrames(got.frames, loop[i%assetLoopPackets].frames) {
			t.Fatalf("packet %d is not loop packet %d: %v", i, i%assetLoopPackets, err)
		}
		if p.Beep != (i%assetLoopPackets == 0) || p.CaptureNS != int64(i)*int64(AudioPacketDuration) {
			t.Fatalf("packet %d: beep %v at %v", i, p.Beep, time.Duration(p.CaptureNS))
		}
	}
}

func FuzzParseOpus(f *testing.F) {
	loop, err := audioLoop()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(opusWithMarker(loop[0], Marker{Frame: 1}))
	f.Add([]byte{0xe3, 0x82, 253, 1, 5})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		p, err := parseOpus(pkt)
		if err != nil {
			return
		}
		if n := p.samples(); n < 120 || n > 120*opusSampleRate/1000 {
			t.Fatalf("%d samples", n)
		}
		// Any valid packet carries a Marker after re-framing, with the same frames.
		m := Marker{Beep: true, Frame: 9, CaptureNS: 180_000_000}
		again := opusWithMarker(p, m)
		q, err := parseOpus(again)
		if err != nil || !equalFrames(q.frames, p.frames) || q.toc != p.toc|3 {
			t.Fatalf("re-framed: %v", err)
		}
		if got, ok := ParseAudioMarker(again); !ok || got != m {
			t.Fatalf("marker %+v %v", got, ok)
		}
	})
}

func FuzzReadOgg(f *testing.F) {
	f.Add(toneOpus[:200])
	f.Add([]byte("OggS\x00\x06\x00\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		pkts, pages, err := readOgg(b)
		if err != nil {
			return
		}
		n := 0
		for _, p := range pkts {
			n += len(p)
		}
		if n > len(b) || len(pages) == 0 {
			t.Fatalf("%d packet bytes from %d bytes, %d pages", n, len(b), len(pages))
		}
	})
}
