package main

import (
	"encoding/binary"
	"io"
)

// An Opus-in-Ogg writer (RFC 3533, RFC 7845) for capture's audio.ogg: the engine's 20 ms Opus packets of 48 kHz
// stereo, one logical stream. Players (ffplay, mpv, VLC) open the file as it is.

const (
	oggOpusRate        = 48000
	oggPacketSamples   = 960 // 20 ms at 48 kHz
	oggPreSkip         = 312 // libopus's encoder delay at 48 kHz (RFC 7845 §4.2 recommends the encoder's lookahead)
	oggPacketsPerPage  = 50  // about one second per page
	oggMaxSegments     = 255
	oggHeaderTypeBOS   = 0x02
	oggHeaderTypeEOS   = 0x04
	oggStreamSerialNum = 0x69737368 // "issh"
)

var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

// oggCRC is Ogg's CRC-32: polynomial 0x04C11DB7, no reflection, initial value and final XOR 0.
func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}

type oggWriter struct {
	w       io.Writer
	seq     uint32
	granule int64
	packets [][]byte
	err     error
}

// newOggOpus writes the two header pages (OpusHead, OpusTags) and returns the writer.
func newOggOpus(w io.Writer, channels uint8) *oggWriter {
	o := &oggWriter{w: w}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = channels
	binary.LittleEndian.PutUint16(head[10:], oggPreSkip)
	binary.LittleEndian.PutUint32(head[12:], oggOpusRate)
	// output gain 0, channel mapping family 0
	o.page([][]byte{head}, 0, oggHeaderTypeBOS)
	vendor := "isshoni-enginetest"
	tags := make([]byte, 8+4+len(vendor)+4)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], uint32(len(vendor))) //nolint:gosec // G115: a constant string
	copy(tags[12:], vendor)
	// no user comments
	o.page([][]byte{tags}, 0, 0)
	// RFC 7845 §4: the granule position counts every decoded sample, the pre-skip ones included (the playable length
	// is granule - pre-skip), so it starts at 0 and a page after N packets carries N × 960.
	return o
}

// WritePacket queues one 20 ms Opus packet; a page goes out every oggPacketsPerPage packets.
func (o *oggWriter) WritePacket(p []byte) error {
	if o.err != nil {
		return o.err
	}
	o.packets = append(o.packets, append([]byte(nil), p...))
	o.granule += oggPacketSamples
	if len(o.packets) >= oggPacketsPerPage || o.segments() > oggMaxSegments-8 {
		o.page(o.packets, o.granule, 0)
		o.packets = o.packets[:0]
	}
	return o.err
}

// Close writes the last page with the end-of-stream flag.
func (o *oggWriter) Close() error {
	if o.err != nil {
		return o.err
	}
	o.page(o.packets, o.granule, oggHeaderTypeEOS)
	o.packets = nil
	return o.err
}

func (o *oggWriter) segments() int {
	n := 0
	for _, p := range o.packets {
		n += len(p)/255 + 1
	}
	return n
}

// page writes one page holding whole packets (each laced as len/255 segments of 255 and one shorter one).
func (o *oggWriter) page(packets [][]byte, granule int64, headerType byte) {
	if o.err != nil {
		return
	}
	var lacing []byte
	size := 0
	for _, p := range packets {
		for n := len(p); ; n -= 255 {
			if n >= 255 {
				lacing = append(lacing, 255)
				continue
			}
			lacing = append(lacing, byte(n)) //nolint:gosec // G115: n < 255 here
			break
		}
		size += len(p)
	}
	if len(lacing) > oggMaxSegments {
		o.err = io.ErrShortBuffer // a packet too large for one page; the engine's are at most ~1.3 KB
		return
	}
	b := make([]byte, 27+len(lacing), 27+len(lacing)+size)
	copy(b, "OggS")
	b[4] = 0 // version
	b[5] = headerType
	binary.LittleEndian.PutUint64(b[6:], uint64(granule)) //nolint:gosec // G115: granules are >= 0
	binary.LittleEndian.PutUint32(b[14:], oggStreamSerialNum)
	binary.LittleEndian.PutUint32(b[18:], o.seq)
	b[26] = byte(len(lacing)) //nolint:gosec // G115: at most oggMaxSegments, checked above
	copy(b[27:], lacing)
	for _, p := range packets {
		b = append(b, p...)
	}
	binary.LittleEndian.PutUint32(b[22:], oggCRC(b))
	o.seq++
	_, o.err = o.w.Write(b)
}
