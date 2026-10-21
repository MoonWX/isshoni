package media_test

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// An engine implements Engine and AudioController (01 §5.1): the two interfaces must not declare one method name
// twice, which this declaration checks at compile time.
type engineWithAudio interface {
	media.Engine
	media.AudioController
}

var _ engineWithAudio = nil

func TestConstants(t *testing.T) {
	if media.ABIVersion != 1 {
		t.Errorf("ABIVersion = %d, want 1 (IM_ABI_MAJOR)", media.ABIVersion)
	}
	if media.LayerAudio != "audio" {
		t.Errorf("LayerAudio = %q", media.LayerAudio)
	}
	if media.SourceScreen != "screen" || media.SourceWindow != "window" {
		t.Errorf("source kinds %q, %q: they must be protocol.ShareKind's strings", media.SourceScreen,
			media.SourceWindow)
	}
}

func TestKind(t *testing.T) {
	for _, tc := range []struct {
		k    media.Kind
		n    uint8
		want string
	}{
		{media.KindVideo, 1, "video"},
		{media.KindAudio, 2, "audio"},
		{media.KindEvent, 3, "event"},
		{media.KindPCM, 4, "pcm"},
		{0, 0, "Kind(0)"},
		{9, 9, "Kind(9)"},
	} {
		if uint8(tc.k) != tc.n || tc.k.String() != tc.want {
			t.Errorf("Kind %d = %q, want %d = %q", uint8(tc.k), tc.k.String(), tc.n, tc.want)
		}
	}
}

// TestEventCodes pins the Go-only event numbers of 01 §5.2 (2–5 are reserved).
func TestEventCodes(t *testing.T) {
	for code, want := range map[media.EventCode]uint8{
		media.EventLayer:              1,
		media.EventLimit:              6,
		media.EventSourceResized:      7,
		media.EventSourceMinimized:    8,
		media.EventSourceRestored:     9,
		media.EventCaptureInterrupted: 10,
		media.EventCaptureResumed:     11,
		media.EventEncoderChanged:     12,
		media.EventDeviceReset:        13,
	} {
		if uint8(code) != want {
			t.Errorf("event code %d, want %d", code, want)
		}
	}
}
