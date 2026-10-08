package fake

import (
	_ "embed" // the Opus asset
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The fake audio is a committed Ogg Opus file, testdata/tone.opus (02 §15.1): two seconds of a periodic tone that
// testdata/gen-opus.sh encoded once with opusenc (libopus, BSD) at 64 kbit/s in 20 ms packets. There is no pure-Go
// Opus encoder, so the file is committed and CI never needs opus-tools. The Source loops its second second (packets
// 50–99): a 440 Hz bed with whole cycles per second, so the loop is phase-continuous, plus a 1 kHz beep in the first
// 100 ms. The encoder state of those packets has a whole period behind it, so the loop point is seamless, and the
// generator shifted the tone by the encoder's pre-skip, so the beep starts exactly at loop packet 0.
//
//go:embed testdata/tone.opus
var toneOpus []byte

const (
	assetLoopStart   = 50  // the loop's first audio packet, counted from 0 after the OpusHead and OpusTags packets
	assetLoopPackets = 50  // one second of 20 ms packets
	assetPreSkip     = 312 // libopus's pre-skip at 48 kHz; gen-opus.sh shifts the tone by it
	opusMaxFrame     = 1275
	opusSampleRate   = 48000
	// audioPacketSamples is AudioPacketDuration at 48 kHz.
	audioPacketSamples = opusSampleRate / int(time.Second/AudioPacketDuration)
)

// errOpus is wrapped by every error about an Opus packet or the Ogg Opus asset.
var errOpus = errors.New("fake: bad Opus data")

// audioLoop returns the loop of the committed asset, parsed once.
var audioLoop = sync.OnceValues(func() ([]opusPacket, error) {
	return loadAsset(toneOpus)
})

// loadAsset reads an Ogg Opus file (RFC 7845) and returns its loop: packets assetLoopStart onwards, one second of
// 20 ms packets.
func loadAsset(file []byte) ([]opusPacket, error) {
	pkts, _, err := readOgg(file)
	if err != nil {
		return nil, err
	}
	if len(pkts) < 2 {
		return nil, fmt.Errorf("%w: no OpusHead and OpusTags", errOpus)
	}
	if _, err := parseOpusHead(pkts[0]); err != nil {
		return nil, err
	}
	if len(pkts[1]) < 8 || string(pkts[1][:8]) != "OpusTags" {
		return nil, fmt.Errorf("%w: the second packet is not OpusTags", errOpus)
	}
	audio := pkts[2:]
	if len(audio) < assetLoopStart+assetLoopPackets {
		return nil, fmt.Errorf("%w: %d audio packets, want at least %d", errOpus, len(audio),
			assetLoopStart+assetLoopPackets)
	}
	loop := make([]opusPacket, 0, assetLoopPackets)
	for i, raw := range audio[assetLoopStart : assetLoopStart+assetLoopPackets] {
		p, err := parseOpus(raw)
		if err != nil {
			return nil, fmt.Errorf("audio packet %d: %w", assetLoopStart+i, err)
		}
		if d := p.samples(); d != audioPacketSamples {
			return nil, fmt.Errorf("%w: audio packet %d lasts %d samples", errOpus, assetLoopStart+i, d)
		}
		loop = append(loop, p)
	}
	return loop, nil
}

// opusHead is the identification header of an Ogg Opus stream (RFC 7845 §5.1).
type opusHead struct {
	version, channels uint8
	preSkip           uint16
	inputRate         uint32
	gain              int16
	mapping           uint8
}

// parseOpusHead reads an OpusHead packet with channel mapping family 0 (mono or stereo).
func parseOpusHead(b []byte) (opusHead, error) {
	if len(b) < 19 || string(b[:8]) != "OpusHead" {
		return opusHead{}, fmt.Errorf("%w: the first packet is not OpusHead", errOpus)
	}
	h := opusHead{
		version: b[8], channels: b[9], preSkip: binary.LittleEndian.Uint16(b[10:]),
		inputRate: binary.LittleEndian.Uint32(b[12:]), gain: int16(binary.LittleEndian.Uint16(b[16:])), mapping: b[18],
	}
	if h.version>>4 != 0 || h.mapping != 0 || h.channels < 1 || h.channels > 2 || len(b) != 19 {
		return opusHead{}, fmt.Errorf("%w: OpusHead version %d, mapping family %d, %d channels, %d bytes", errOpus,
			h.version, h.mapping, h.channels, len(b))
	}
	return h, nil
}

// opusPacket is one Opus packet split into its frames (RFC 6716 §3). The frames alias the packet.
type opusPacket struct {
	toc     byte
	frames  [][]byte
	padding int // bytes of padding (code 3)
}

// frameSamples returns the duration of one frame of a TOC byte's configuration at 48 kHz (RFC 6716 §3.1).
func frameSamples(toc byte) int {
	config := toc >> 3
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		return [4]int{480, 960, 1920, 2880}[config&3]
	case config < 16: // Hybrid: 10, 20 ms
		return [2]int{480, 960}[config&1]
	default: // CELT: 2.5, 5, 10, 20 ms
		return [4]int{120, 240, 480, 960}[config&3]
	}
}

// samples returns the packet's duration at 48 kHz.
func (p opusPacket) samples() int { return len(p.frames) * frameSamples(p.toc) }

// bytes returns the size of the packet's frames.
func (p opusPacket) bytes() int {
	n := 0
	for _, f := range p.frames {
		n += len(f)
	}
	return n
}

// parseOpus splits an Opus packet into its frames and checks the framing requirements R1–R7 of RFC 6716 §3.4.
func parseOpus(b []byte) (opusPacket, error) {
	fail := func(format string, a ...any) (opusPacket, error) {
		return opusPacket{}, fmt.Errorf("%w: %s", errOpus, fmt.Sprintf(format, a...))
	}
	if len(b) < 1 {
		return fail("empty packet (R1)")
	}
	p := opusPacket{toc: b[0]}
	rest := b[1:]
	switch b[0] & 3 {
	case 0:
		p.frames = [][]byte{rest}
	case 1:
		if len(rest)%2 != 0 {
			return fail("code 1 packet with an odd payload (R3)")
		}
		p.frames = [][]byte{rest[:len(rest)/2], rest[len(rest)/2:]}
	case 2:
		n, used, ok := opusFrameLength(rest)
		if !ok || n > len(rest)-used {
			return fail("code 2 packet with a bad first frame length (R4)")
		}
		p.frames = [][]byte{rest[used : used+n], rest[used+n:]}
	case 3:
		if len(rest) < 1 {
			return fail("code 3 packet without a frame count (R6)")
		}
		vbr, padded, count := rest[0]&0x80 != 0, rest[0]&0x40 != 0, int(rest[0]&0x3f)
		rest = rest[1:]
		if count == 0 || count*frameSamples(p.toc) > 120*opusSampleRate/1000 {
			return fail("code 3 packet with %d frames (R5)", count)
		}
		if padded {
			for {
				if len(rest) == 0 {
					return fail("padding length runs out (R6, R7)")
				}
				c := int(rest[0])
				rest = rest[1:]
				if c < 255 {
					p.padding += c
					break
				}
				p.padding += 254
			}
			if p.padding > len(rest) {
				return fail("padding longer than the packet (R6, R7)")
			}
			rest = rest[:len(rest)-p.padding]
		}
		if !vbr {
			if len(rest)%count != 0 {
				return fail("CBR code 3 packet whose frames don't divide evenly (R6)")
			}
			n := len(rest) / count
			for i := range count {
				p.frames = append(p.frames, rest[i*n:(i+1)*n])
			}
			break
		}
		lengths := make([]int, count-1)
		for i := range lengths {
			n, used, ok := opusFrameLength(rest)
			if !ok {
				return fail("VBR frame length %d runs out (R7)", i)
			}
			lengths[i] = n
			rest = rest[used:]
		}
		for _, n := range lengths {
			if n > len(rest) {
				return fail("VBR frames longer than the packet (R7)")
			}
			p.frames = append(p.frames, rest[:n])
			rest = rest[n:]
		}
		p.frames = append(p.frames, rest)
	}
	for _, f := range p.frames {
		if len(f) > opusMaxFrame {
			return fail("a %d-byte frame (R2)", len(f))
		}
	}
	return p, nil
}

// opusFrameLength reads a frame length (RFC 6716 §3.2.1): one byte below 252, else two bytes.
func opusFrameLength(b []byte) (n, used int, ok bool) {
	switch {
	case len(b) == 0:
		return 0, 0, false
	case b[0] < 252:
		return int(b[0]), 1, true
	case len(b) < 2:
		return 0, 0, false
	}
	return 4*int(b[1]) + int(b[0]), 2, true
}

// appendOpusFrameLength appends a frame length n ≤ 1275.
func appendOpusFrameLength(b []byte, n int) []byte {
	if n < 252 {
		return append(b, byte(n))
	}
	first := 252 + (n-252)&3
	return append(b, byte(first), byte((n-first)/4))
}

// appendOpusPadded appends p's frames as a code 3 packet (RFC 6716 §3.2.5) with pad as its padding: CBR when the
// frames are the same size, else VBR. Decoders ignore padding, so the audio is p's.
func appendOpusPadded(b []byte, p opusPacket, pad []byte) []byte {
	vbr := false
	for _, f := range p.frames[1:] {
		vbr = vbr || len(f) != len(p.frames[0])
	}
	count := byte(0x40 | len(p.frames)) // p = 1
	if vbr {
		count |= 0x80
	}
	b = append(b, p.toc|3, count)
	n := len(pad)
	for n >= 255 {
		b = append(b, 255)
		n -= 254
	}
	b = append(b, byte(n))
	if vbr {
		for _, f := range p.frames[:len(p.frames)-1] {
			b = appendOpusFrameLength(b, len(f))
		}
	}
	for _, f := range p.frames {
		b = append(b, f...)
	}
	return append(b, pad...)
}

// oggPage is what readOgg reports about each page.
type oggPage struct {
	flags   byte // 1 continued packet, 2 beginning of stream, 4 end of stream
	granule int64
	serial  uint32
	seq     uint32
	ends    int // packets that end on this page
}

// readOgg reads a single-stream Ogg file (RFC 3533) and returns its packets and pages. It checks the capture
// pattern, version, CRC, serial number, page sequence, the beginning- and end-of-stream flags and the continuation
// of packets across pages.
func readOgg(b []byte) (pkts [][]byte, pages []oggPage, err error) {
	fail := func(format string, a ...any) ([][]byte, []oggPage, error) {
		return nil, nil, fmt.Errorf("%w: Ogg page %d: %s", errOpus, len(pages), fmt.Sprintf(format, a...))
	}
	var partial []byte // a packet that continues on the next page
	open := false
	for len(b) > 0 {
		if len(b) < 27 || string(b[:4]) != "OggS" || b[4] != 0 {
			return fail("no Ogg page header")
		}
		pg := oggPage{
			flags: b[5], granule: int64(binary.LittleEndian.Uint64(b[6:])), serial: binary.LittleEndian.Uint32(b[14:]),
			seq: binary.LittleEndian.Uint32(b[18:]),
		}
		nseg := int(b[26])
		if len(b) < 27+nseg {
			return fail("segment table runs out")
		}
		size := 27 + nseg
		for _, l := range b[27 : 27+nseg] {
			size += int(l)
		}
		if len(b) < size {
			return fail("page body runs out")
		}
		page := b[:size]
		if got, want := oggCRC(page), binary.LittleEndian.Uint32(b[22:]); got != want {
			return fail("CRC %08x, want %08x", got, want)
		}
		switch {
		case pg.flags&^7 != 0:
			return fail("flags %#x", pg.flags)
		case (pg.flags&2 != 0) != (len(pages) == 0):
			return fail("beginning-of-stream flag %v", pg.flags&2 != 0)
		case len(pages) > 0 && (pg.serial != pages[0].serial || pg.seq != pages[len(pages)-1].seq+1):
			return fail("serial %d, sequence %d", pg.serial, pg.seq)
		case len(pages) > 0 && pages[len(pages)-1].flags&4 != 0:
			return fail("a page after the end of the stream")
		case (pg.flags&1 != 0) != open:
			return fail("continuation flag %v, but a packet is open: %v", pg.flags&1 != 0, open)
		}
		body := page[27+nseg:]
		for _, l := range b[27 : 27+nseg] {
			partial = append(partial, body[:l]...)
			body = body[l:]
			open = l == 255
			if !open {
				pkts = append(pkts, partial)
				partial = nil
				pg.ends++
			}
		}
		pages = append(pages, pg)
		b = b[size:]
	}
	if len(pages) == 0 || pages[len(pages)-1].flags&4 == 0 || open {
		return fail("no end of stream")
	}
	return pkts, pages, nil
}

// oggCRCTable is the CRC-32 of Ogg (RFC 3533 §6): polynomial 0x04c11db7, MSB first, initial value 0, no final XOR.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

// oggCRC returns the CRC of a page with its CRC field taken as zero.
func oggCRC(page []byte) uint32 {
	var crc uint32
	for i, c := range page {
		if i >= 22 && i < 26 {
			c = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}
