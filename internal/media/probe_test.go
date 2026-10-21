package media_test

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestAudioCapsFromFlags decodes all 16 values of the four IM_AUDIO_* bits (05 §11.1), with and without unknown
// higher bits: one field per bit, and Backend "process-loopback" whenever 0x1 is set, "endpoint" with only 0x2 of the
// loopback bits, else "none".
func TestAudioCapsFromFlags(t *testing.T) {
	for flags := range uint32(16) {
		want := media.AudioCaps{
			ProcessLoopback:  flags&0x1 != 0,
			EndpointLoopback: flags&0x2 != 0,
			SessionEvents:    flags&0x4 != 0,
			MicDetection:     flags&0x8 != 0,
			Backend:          "none",
		}
		switch {
		case flags&0x1 != 0:
			want.Backend = "process-loopback"
		case flags&0x2 != 0:
			want.Backend = "endpoint"
		}
		for _, high := range []uint32{0, 0x10, 0x80000000, 0xfffffff0} {
			if got := media.AudioCapsFromFlags(flags | high); got != want {
				t.Errorf("AudioCapsFromFlags(%#x) = %+v, want %+v", flags|high, got, want)
			}
		}
	}
	// The spec's named cases.
	for _, tc := range []struct {
		flags   uint32
		backend string
	}{
		{0xf, "process-loopback"}, {0x1, "process-loopback"}, {0x3, "process-loopback"}, {0x2, "endpoint"},
		{0xe, "endpoint"}, {0x0, "none"}, {0xc, "none"},
	} {
		if got := media.AudioCapsFromFlags(tc.flags).Backend; got != tc.backend {
			t.Errorf("AudioCapsFromFlags(%#x).Backend = %q, want %q", tc.flags, got, tc.backend)
		}
	}
}
