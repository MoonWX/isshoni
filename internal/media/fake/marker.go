package fake

import (
	"encoding/binary"
	"math/rand/v2"
)

// Marker is what every fake frame and audio packet says about itself (02 §15.1). Viewers read it back to check
// layers, frame continuity, keyframes and A/V sync (sfutest.Viewer).
type Marker struct {
	RID       string // the layer; "" for audio
	Keyframe  bool
	Flash     bool
	Beep      bool
	Frame     uint32 // index of the frame in its layer, or of the audio packet, from 0
	CaptureNS int64  // Packet.CaptureNS
}

// MarkerSize is the size of an encoded Marker: magic "ISHN" (4), rid (1, 0 for audio), flags (1), frame index
// (uint32) and capture ns (uint64), big endian.
const MarkerSize = 18

// Marker flag bits.
const (
	flagKeyframe = 1 << iota
	flagFlash
	flagBeep
)

var markerMagic = [4]byte{'I', 'S', 'H', 'N'}

// appendMarker appends m's 18 bytes to b.
func appendMarker(b []byte, m Marker) []byte {
	var rid byte
	if m.RID != "" {
		rid = m.RID[0]
	}
	var flags byte
	if m.Keyframe {
		flags |= flagKeyframe
	}
	if m.Flash {
		flags |= flagFlash
	}
	if m.Beep {
		flags |= flagBeep
	}
	b = append(b, markerMagic[:]...)
	b = append(b, rid, flags)
	b = binary.BigEndian.AppendUint32(b, m.Frame)
	return binary.BigEndian.AppendUint64(b, uint64(m.CaptureNS))
}

// decodeMarker reads a Marker from its 18 raw bytes.
func decodeMarker(b []byte) (Marker, bool) {
	if len(b) < MarkerSize || [4]byte(b[:4]) != markerMagic {
		return Marker{}, false
	}
	m := Marker{
		Keyframe:  b[5]&flagKeyframe != 0,
		Flash:     b[5]&flagFlash != 0,
		Beep:      b[5]&flagBeep != 0,
		Frame:     binary.BigEndian.Uint32(b[6:10]),
		CaptureNS: int64(binary.BigEndian.Uint64(b[10:18])),
	}
	if b[4] != 0 {
		m.RID = string(b[4:5])
	}
	return m, true
}

// ParseVideoMarker returns the Marker of a fake slice: body is the slice NAL unit after its one-byte header, as sent
// (with emulation prevention). Only its first bytes are read, so the start of a fragmented NAL unit (the payload of
// the first FU-A packet after its two FU bytes) is enough. It reads both modes: a synthetic slice starts with the
// Marker, a decodable one carries it in the first luma samples of its first macroblock. ok is false when body holds
// no Marker.
func ParseVideoMarker(body []byte) (m Marker, ok bool) {
	var buf [markerPrefix]byte
	rbsp := unescapePrefix(buf[:0], body)
	if m, ok := decodeMarker(rbsp); ok {
		return m, true
	}
	return parseDecodableMarker(rbsp)
}

// markerPrefix is how much RBSP ParseVideoMarker reads: enough for a decodable slice header and macroblock 0's
// mb_type (at most 9 bytes with a 16-bit idr_pic_id), then its 36 marker samples.
const markerPrefix = 64

// unescapePrefix appends body's RBSP (emulation prevention removed) to dst, up to dst's capacity.
func unescapePrefix(dst, body []byte) []byte {
	zeros := 0
	for _, c := range body {
		if len(dst) == cap(dst) {
			break
		}
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// opusWithMarker returns one packet of the Opus asset as a code 3 packet whose padding is m (RFC 6716 §3.2.5).
// Decoders ignore padding, so the audio is the asset's.
func opusWithMarker(p opusPacket, m Marker) []byte {
	var pad [MarkerSize]byte
	return appendOpusPadded(make([]byte, 0, 4+len(p.frames)*2+p.bytes()+MarkerSize), p, appendMarker(pad[:0], m))
}

// ParseAudioMarker returns the Marker at the start of a fake Opus packet's padding. ok is false for any other
// packet, including valid Opus without the Marker.
func ParseAudioMarker(opus []byte) (m Marker, ok bool) {
	if len(opus) < 2 || opus[0]&3 != 3 || opus[1]&0x40 == 0 || opus[1]&0x3f == 0 {
		return Marker{}, false
	}
	// Padding length (RFC 6716 §3.2.5): each 255 adds 254 bytes and another length byte follows.
	pad, i := 0, 2
	for {
		if i >= len(opus) {
			return Marker{}, false
		}
		c := int(opus[i])
		i++
		if c < 255 {
			pad += c
			break
		}
		pad += 254
	}
	if pad < MarkerSize || pad > len(opus)-i {
		return Marker{}, false
	}
	return decodeMarker(opus[len(opus)-pad:])
}

// appendFiller appends n seeded bytes to b.
func appendFiller(b []byte, n int, rng *rand.Rand) []byte {
	for n >= 8 {
		b = binary.LittleEndian.AppendUint64(b, rng.Uint64())
		n -= 8
	}
	if n > 0 {
		var tail [8]byte
		binary.LittleEndian.PutUint64(tail[:], rng.Uint64())
		b = append(b, tail[:n]...)
	}
	return b
}
