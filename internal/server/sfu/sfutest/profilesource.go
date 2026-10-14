package sfutest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// ProfileSource is a publish.ProfileSource made of fake.Sources, for the codec policy tests (02 §8.3): a fake.Source
// makes one H.264 profile, so a switch to another profile makes a new one with NewSource, as an encoder that
// re-initializes in the new profile would, and the media goes on in it. The new Source's packets come on the old
// one's timeline: their capture times go on from the moment of the switch, so a Publisher's RTP timestamps stay
// continuous. The new Source starts with a keyframe of every layer.
type ProfileSource struct {
	newSource func(profile string) (*fake.Source, error)

	mu      sync.Mutex // guards the fields below
	cur     *fake.Source
	profile string
	closed  bool
	// start is when the ProfileSource returned its first packet; off is where the current Source's capture times
	// start on that timeline, and fresh says that it hasn't returned a packet yet (off is set with its first one).
	start    time.Time
	started  bool
	off      int64
	fresh    bool
	switches int
}

var _ publish.ProfileSource = (*ProfileSource)(nil)

// NewProfileSource makes the Source for profile with newSource and returns a ProfileSource that starts with it.
func NewProfileSource(profile string, newSource func(profile string) (*fake.Source, error)) (*ProfileSource, error) {
	src, err := newSource(profile)
	if err != nil {
		return nil, err
	}
	return &ProfileSource{newSource: newSource, cur: src, profile: profile, fresh: true}, nil
}

// Next implements publish.Source: the current Source's next packet, on the ProfileSource's timeline. A packet of a
// Source that a switch replaced while Next waited for it is dropped.
func (s *ProfileSource) Next(ctx context.Context) (fake.Packet, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return fake.Packet{}, fake.ErrClosed
		}
		src := s.cur
		s.mu.Unlock()
		pkt, err := src.Next(ctx)
		s.mu.Lock()
		switch {
		case s.cur != src: // switched meanwhile: the old Source is closed, and what it made is not wanted
			s.mu.Unlock()
			continue
		case err != nil:
			s.mu.Unlock()
			return fake.Packet{}, err
		}
		if !s.started {
			s.started, s.start = true, time.Now()
		}
		if s.fresh {
			s.fresh, s.off = false, int64(time.Since(s.start))-pkt.CaptureNS
		}
		pkt.CaptureNS += s.off
		s.mu.Unlock()
		return pkt, nil
	}
}

// RequestKeyframe implements publish.Source.
func (s *ProfileSource) RequestKeyframe(layer string) {
	s.mu.Lock()
	src := s.cur
	s.mu.Unlock()
	src.RequestKeyframe(layer)
}

// SetProfile implements publish.ProfileSource: from now on the media is in profile, made by a new Source. The same
// profile again changes nothing.
func (s *ProfileSource) SetProfile(profile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return errors.New("sfutest: SetProfile on a closed ProfileSource")
	case profile == s.profile:
		return nil
	}
	src, err := s.newSource(profile)
	if err != nil {
		return err
	}
	old := s.cur
	s.cur, s.profile, s.fresh = src, profile, true
	s.switches++
	return old.Close()
}

// Profile returns the profile the media is in now, and how often it switched.
func (s *ProfileSource) Profile() (profile string, switches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profile, s.switches
}

// Close closes the current Source. It is idempotent.
func (s *ProfileSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.cur.Close()
}
