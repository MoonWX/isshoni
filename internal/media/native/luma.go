package native

import (
	"encoding/binary"
	"image"
)

// This file is README WF16's (04 §5.8, §12.3, §14 avsync): the luma tap's Go side. An IM_TEST_HOOKS DLL (the
// win-x64-ci build) started with IM_START_TEST_LUMA_TAP emits an IM_PACKET_DEBUG packet per captured frame the
// session's video thread takes: the mean luma of a fixed region of the frame (the centered box of up to 256 pixels a
// side) with the frame's capture time. WithLumaTap asks for it and Next hands each reading to the option's function
// instead of returning it (Next never returns a debug packet). The code is pure Go: the DLL exists only on Windows,
// but the start flag, the decoding and the delivery run and are tested everywhere.
//
// The constants mirror native/windows/src/session/luma_sample.h (TestLumaConstantsMatchTheEngine reads it): the start
// flag is a bit of im_start_config.flags that the public header doesn't define, as only test builds know it (04 §5.8).

const (
	startTestLumaTap = 0x80000000 // IM_START_TEST_LUMA_TAP (luma_sample.h's kStartFlag)
	lumaMagic        = 0x414D554C // "LUMA" in byte order
	lumaVersion      = 1
	lumaPayloadBytes = 48
	lumaMeanScale    = 65535
)

// LumaSample is one reading of the luma tap (04 §5.8).
type LumaSample struct {
	// CaptureNS is the frame's capture time on the session's clock (capture_ts_ns − epoch_ns, as Packet.CaptureNS).
	CaptureNS int64
	// Mean is the region's mean BT.709 luma of the frame's 8-bit gamma-encoded pixels, full range: 0 black, 1 white.
	Mean float64
	// Source is the frame's content size in pixels, Region the part of it the mean is over.
	Source image.Point
	Region image.Rectangle
	// Missed counts the captured frames since the previous reading that have none (replaced in the engine's frame
	// slots before its video thread took them, or its readback queue was full).
	Missed int
}

// WithLumaTap asks StartWith or StartExtra for the luma tap of an IM_TEST_HOOKS DLL (04 §5.8): the session's Next
// calls fn with every reading, in capture order, on the goroutine that called Next and before Next returns its next
// packet; fn must not block for long or call the session. Readings that arrive malformed or before the session's epoch
// are dropped with a warn log, like any other packet over its limits. A DLL built without test hooks (every shipped
// one) has no tap: the start is then media.CodeDependency without a DLL call. A nil fn turns the tap off again. Only
// isshoni-enginetest's avsync uses it (04 §14).
func WithLumaTap(fn func(LumaSample)) StartOption {
	return func(o *startOptions) { o.luma = fn }
}

// LumaTapOf is the luma-tap function that opts set (nil when none does), with the last WithLumaTap winning as in
// StartWith. It lets callers that build options test what they pass, and fakes of StartWith deliver readings as Next
// would.
func LumaTapOf(opts ...StartOption) func(LumaSample) {
	return collectOptions(opts).luma
}

// decodeLuma reads a debug packet's payload (04 §5.8's table, little endian): ok is false unless it is 48 bytes with
// the magic, version 1 and a mean of at most 65535. The capture time it returns is the engine clock's.
func decodeLuma(b []byte) (s LumaSample, captureTSNS int64, ok bool) {
	if len(b) != lumaPayloadBytes {
		return LumaSample{}, 0, false
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	if u32(0) != lumaMagic || u32(4) != lumaVersion || u32(16) > lumaMeanScale {
		return LumaSample{}, 0, false
	}
	x, y, w, h := int(u32(28)), int(u32(32)), int(u32(36)), int(u32(40))
	s = LumaSample{
		Mean:   float64(u32(16)) / lumaMeanScale,
		Source: image.Pt(int(u32(20)), int(u32(24))),
		Region: image.Rect(x, y, x+w, y+h),
		Missed: int(u32(44)),
	}
	return s, int64(binary.LittleEndian.Uint64(b[8:])), true // int64 as two's complement
}

// debug handles an IM_PACKET_DEBUG packet: the luma tap's reading for the session's WithLumaTap function, or nothing
// when the session has none (an IM_TEST_HOOKS DLL emits them only when asked). It never delivers a packet.
func (s *session) debug(pk *pulled) (bool, error) {
	if s.luma == nil {
		return false, nil
	}
	r, ts, ok := decodeLuma(pk.data)
	switch {
	case !ok:
		s.dropped("a debug packet that is not a luma reading", pk, -1)
		return false, nil
	case ts != pk.p.captureTSNS:
		s.dropped("a luma reading whose capture time is not its packet's", pk, -1)
		return false, nil
	case s.captureNS(pk) < 0:
		s.dropped("a luma reading captured before the session's epoch", pk, -1)
		return false, nil
	}
	r.CaptureNS = s.captureNS(pk)
	s.luma(r)
	return false, nil
}
