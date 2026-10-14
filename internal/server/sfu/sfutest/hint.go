package sfutest

import (
	"errors"
	"slices"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// ApplyHint does with a quality hint's encodings what a browser sharer does with setParameters (01 §8.10): each
// layer of pub that an encoding names by its rid sends, or stops, as the encoding's Active flag says
// (publish.Publisher.SetActive). An encoding of a layer that pub doesn't send is passed over, as a browser's sender
// has no encoding to set for it. The Go Publisher has no rate control in M1, so the bitrates, frame rates and sizes
// change nothing, and a hint without encodings (a codec hint) changes nothing at all.
func ApplyHint(pub *publish.Publisher, ev sfu.QualityHintEvent) error {
	var errs []error
	for _, e := range ev.Encodings {
		if !slices.ContainsFunc(pub.Tracks(), func(t publish.TrackInfo) bool { return t.Layer == e.RID }) {
			continue
		}
		errs = append(errs, pub.SetActive(e.RID, e.Active))
	}
	return errors.Join(errs...)
}
