package fake

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/mediatest"
)

// listed is the default listed monitor of an Engine, the source of the conformance runs (01 §6.4).
var listed = media.Source{ID: ListedMonitorID, Kind: media.SourceScreen}

// TestConformanceSynthetic: the fake in its default mode passes every check of mediatest.Conformance (01 §6.4).
func TestConformanceSynthetic(t *testing.T) {
	t.Parallel()
	mediatest.Conformance(t, func(*testing.T) media.Engine { return NewEngine(EngineConfig{}) }, listed,
		mediatest.Options{Listed: true})
}

// TestConformanceDecodable: Decodable passes every check but 5 (its bitrate follows the picture, not the target),
// at the app's dev and e2e configuration (640×360, 03 §22.4), with the test tone as its audio.
func TestConformanceDecodable(t *testing.T) {
	t.Parallel()
	if testing.CoverMode() != "" {
		// Conformance's checks run in real time, and decodable.go renders every sample in Go: with coverage counters
		// (atomic under -race) a CI runner made the first packets 0.7–1.4 s after Start and 15 audio packets a
		// second where 50 are due. windows-go and desktop-macos run this test without coverage.
		t.Skip("real-time conformance checks; the jobs without coverage run them")
	}
	mediatest.Conformance(t, func(*testing.T) media.Engine {
		return NewEngine(EngineConfig{Mode: Decodable, SourceSize: [2]int{640, 360}})
	}, listed, mediatest.Options{Listed: true, TestTone: true, SkipBitrate: true})
}
