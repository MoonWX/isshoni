package sfutest

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// keyframeSource is a publish.Source that records the keyframes the Publisher asks for.
type keyframeSource struct {
	*fake.Source
	mu    sync.Mutex
	asked []string
}

func (s *keyframeSource) RequestKeyframe(layer string) {
	s.mu.Lock()
	s.asked = append(s.asked, layer)
	s.mu.Unlock()
	s.Source.RequestKeyframe(layer)
}

func (s *keyframeSource) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// TestApplyHint: a hint's encodings turn the Publisher's layers off and on by rid (a layer turned on again asks the
// Source for its keyframe); an encoding of a layer the Publisher doesn't send is passed over, and a codec hint changes
// nothing.
func TestApplyHint(t *testing.T) {
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Minute, Audio: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rec := &keyframeSource{Source: src}
	pub, err := publish.New(publish.Options{Source: rec, Settings: LoopbackSettings(), Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	hint := func(f, q bool) sfu.QualityHintEvent {
		return sfu.QualityHintEvent{Share: "s_1", Reason: sfu.HintReasonViewers, Encodings: []sfu.EncodingParams{
			{RID: "f", Active: f, MaxBitrate: 8_000_000}, {RID: "q", Active: q, MaxBitrate: 300_000},
			{RID: "h", Active: true}, // M5's middle layer: this Publisher doesn't send it
		}}
	}

	if err := ApplyHint(pub, hint(false, true)); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests(); len(got) != 0 {
		t.Errorf("turning f off asked for keyframes %v", got)
	}
	if err := ApplyHint(pub, sfu.QualityHintEvent{Share: "s_1", Reason: sfu.HintReasonAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyHint(pub, hint(true, true)); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests(); !slices.Equal(got, []string{"f"}) {
		t.Errorf("turning f on again asked for keyframes %v, want [f]", got)
	}
	// q was on all along, and f is on now: the same hint again asks for nothing.
	if err := ApplyHint(pub, hint(true, true)); err != nil || len(rec.requests()) != 1 {
		t.Errorf("a hint that changes nothing: %v, keyframe requests %v", err, rec.requests())
	}
}
