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

// ParseVideoMarker returns the Marker of a synthetic slice: body is the slice NAL unit after its one-byte header, as
// sent (with emulation prevention). Only its first bytes are read, so the start of a fragmented NAL unit (the payload
// of the first FU-A packet after its two FU bytes) is enough. ok is false when body holds no Marker.
func ParseVideoMarker(body []byte) (m Marker, ok bool) {
	var raw [MarkerSize]byte
	n, zeros := 0, 0
	for _, c := range body {
		if n == MarkerSize {
			break
		}
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		raw[n] = c
		n++
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	if n < MarkerSize {
		return Marker{}, false
	}
	return decodeMarker(raw[:])
}

// Opus packet framing of the synthetic audio (RFC 6716 §3): code 3 with one CBR frame and padding, and the Marker at
// the start of the padding. Decoders ignore padding, so the packets stay valid Opus. The TOC is config 31 (CELT-only,
// fullband, 20 ms), stereo.
const (
	opusTOC        = 31<<3 | 1<<2 | 3 // config 31, s = 1, c = 3
	opusCountByte  = 0x40 | 1         // v = 0 (CBR), p = 1 (padding), M = 1 frame
	opusHeaderSize = 3                // TOC, frame count, one padding-length byte
)

// syntheticOpus makes one AudioPacketSize-byte Opus packet whose frame is seeded filler and whose padding is m.
func syntheticOpus(m Marker, rng *rand.Rand) []byte {
	b := make([]byte, 0, AudioPacketSize)
	b = append(b, opusTOC, opusCountByte, MarkerSize)
	b = appendFiller(b, AudioPacketSize-opusHeaderSize-MarkerSize, rng)
	return appendMarker(b, m)
}

// ParseAudioMarker returns the Marker in the padding of a synthetic Opus packet. ok is false for any other packet,
// including valid Opus without the Marker.
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
