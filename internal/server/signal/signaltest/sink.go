package signaltest

import (
	"maps"
	"slices"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// The fakes implement the hub's interfaces.
var (
	_ signal.Authenticator = (*Auth)(nil)
	_ signal.RoomDirectory = (*Rooms)(nil)
	_ signal.MediaPlane    = (*Media)(nil)
	_ signal.MediaPeer     = (*Peer)(nil)
	_ signal.MediaSink     = (*Sink)(nil)
	_ signal.PushNotifier  = (*Push)(nil)
)

// Event is one recorded MediaSink call: the method name and its argument (for ShareMedia, a ShareMediaCall).
type Event struct {
	Method string
	Arg    any
}

// ShareMediaCall is the argument of a recorded ShareMedia event.
type ShareMediaCall struct {
	ShareID string
	Event   signal.ShareMediaEvent
}

// Sink is a fake signal.MediaSink that records every event, for tests of MediaPlane implementations (sfuplane). As the
// MediaSink contract says, it copies the slices, maps and pointers of each argument, so a recorded event keeps the
// value of its call when the caller reuses them.
type Sink struct {
	mu     sync.Mutex
	events []Event
}

func (s *Sink) record(method string, arg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{Method: method, Arg: arg})
}

// Events returns the recorded events, in order.
func (s *Sink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// Offer implements signal.MediaSink.
func (s *Sink) Offer(o protocol.PCOffer) {
	o.Tracks = slices.Clone(o.Tracks)
	s.record("Offer", o)
}

// ICE implements signal.MediaSink.
func (s *Sink) ICE(c protocol.PCICE) {
	if c.Candidate != nil {
		cand := *c.Candidate
		cand.SDPMid, cand.SDPMLineIndex = clonePtr(cand.SDPMid), clonePtr(cand.SDPMLineIndex)
		cand.UsernameFragment = clonePtr(cand.UsernameFragment)
		c.Candidate = &cand
	}
	s.record("ICE", c)
}

// RestartRequest implements signal.MediaSink.
func (s *Sink) RestartRequest(r protocol.PCRestart) { s.record("RestartRequest", r) }

// SubscriptionStatus implements signal.MediaSink.
func (s *Sink) SubscriptionStatus(st []protocol.SubscriptionStatus) {
	s.record("SubscriptionStatus", slices.Clone(st))
}

// QualityHint implements signal.MediaSink.
func (s *Sink) QualityHint(h protocol.QualityHint) {
	h.Encodings = slices.Clone(h.Encodings)
	s.record("QualityHint", h)
}

// ShareMedia implements signal.MediaSink.
func (s *Sink) ShareMedia(shareID string, ev signal.ShareMediaEvent) {
	ev.Layers = slices.Clone(ev.Layers)
	s.record("ShareMedia", ShareMediaCall{ShareID: shareID, Event: ev})
}

// Error implements signal.MediaSink.
func (s *Sink) Error(e protocol.Error) {
	e.Params = maps.Clone(e.Params)
	s.record("Error", e)
}

// clonePtr returns a pointer to a copy of *p, or nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
