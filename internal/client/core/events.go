package core

import (
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// EventKind is what an Event reports (docs/m2/01-client-core.md §11.10).
type EventKind uint8

// The event kinds.
const (
	EventStatus        EventKind = iota + 1 // Status changed (connection state, connection id, stop)
	EventShare                              // a ShareSnapshot field other than a live counter changed (coalesced to ≤ 4 per second)
	EventNotice                             // a toast: Code "encoder_fallback" {encoder}, …
	EventPlaybackState                      // a web tab reported isshoni playback (relay playback.state, §11.11)
	EventResync                             // the subscriber missed events: read Status()
)

// String returns the kind's name, for logs and test failures.
func (k EventKind) String() string {
	switch k {
	case EventStatus:
		return "status"
	case EventShare:
		return "share"
	case EventNotice:
		return "notice"
	case EventPlaybackState:
		return "playback-state"
	case EventResync:
		return "resync"
	}
	return "EventKind(" + strconv.Itoa(int(k)) + ")"
}

// Event is one event for the app. Exactly the field of its Kind is set (none for EventResync); each Event has its
// own copies, so a subscriber may keep them.
type Event struct {
	Kind     EventKind
	Status   *Status
	Share    *ShareSnapshot
	Notice   *Notice
	Playback *PlaybackState
}

// Status is the Go connection's state.
type Status struct {
	Conn          string     // "connecting" | "online" | "reconnecting" | "stopped"
	ConnectionID  string     // the Go connection's id: "" until the first welcome; changes after a reconnect that isn't resumed
	Stop          *StopError // when stopped by the server or by Auth (nil after Close or the end of Run's ctx)
	ServerVersion string
	Features      []protocol.Feature
	Share         *ShareSnapshot // the active share's snapshot, nil when none
}

// The values of Status.Conn.
const (
	ConnConnecting   = "connecting"
	ConnOnline       = "online"
	ConnReconnecting = "reconnecting"
	ConnStopped      = "stopped"
)

// PlaybackState is a web tab's report that it plays isshoni audio (relay playback.state, §11.11): 03 forwards it to
// 05's Service.SetViewerBrowser(From, Browser, OS, Playing).
type PlaybackState struct {
	From    string // the sender's connectionId (agent.recv.from)
	Playing bool
	Browser string // "chrome" | "edge" | "firefox" | "other" (05 §5.4 families)
	OS      protocol.ClientOS
}

// Notice is a toast for the app; 03 maps Code to its text (the core never formats text).
type Notice struct {
	Code   string
	Params map[string]string
}

// The notice codes.
const (
	// NoticeEncoderFallback: the engine switched a layer to another encoder (EventEncoderChanged); Params
	// {"layer", "encoder"}.
	NoticeEncoderFallback = "encoder_fallback"
)

// subscriberDepth is how many events a subscriber's channel holds (§13).
const subscriberDepth = 64

// subscribers fans events out to the channels of Subscribe. A subscriber that falls behind loses its oldest events
// and gets an EventResync in their place; publishing never blocks.
type subscribers struct {
	mu   sync.Mutex // guards subs, and serializes the sends to each channel
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch chan Event
}

// add registers a subscriber and returns its channel and the function that ends it (and closes the channel).
func (h *subscribers) add() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, subscriberDepth)}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[*subscriber]struct{}{}
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			close(s.ch)
			h.mu.Unlock()
		})
	}
}

// publish hands an event to every subscriber. mk returns a fresh copy for each, so subscribers never share memory.
func (h *subscribers) publish(mk func() Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.send(mk())
	}
}

// send queues ev without blocking. When the channel is full, the two oldest events go, and an EventResync and ev
// take their places: the subscriber reads Status again and goes on from ev. Only one publish runs at a time (h.mu),
// so the receiver alone can take events meanwhile, which only makes room.
func (s *subscriber) send(ev Event) {
	select {
	case s.ch <- ev:
		return
	default:
	}
	for range 2 {
		select {
		case <-s.ch:
		default:
		}
	}
	select {
	case s.ch <- Event{Kind: EventResync}:
	default:
	}
	select {
	case s.ch <- ev:
	default:
	}
}

// copyStatus returns a deep copy of st.
func copyStatus(st Status) *Status {
	st.Features = slices.Clone(st.Features)
	if st.Stop != nil {
		stop := *st.Stop
		st.Stop = &stop
	}
	if st.Share != nil {
		st.Share = st.Share.clone()
	}
	return &st
}

// sameStatus reports whether a and b are the same for EventStatus: the connection state, its id, the stop, the
// server's version and the features. The share has events of its own.
func sameStatus(a, b Status) bool {
	return a.Conn == b.Conn && a.ConnectionID == b.ConnectionID && a.ServerVersion == b.ServerVersion &&
		slices.Equal(a.Features, b.Features) && reflect.DeepEqual(a.Stop, b.Stop)
}

// shareEventEvery is the coalescing interval of EventShare: at most 4 per second (§11.10).
const shareEventEvery = 250 * time.Millisecond

// coalescer limits a share's EventShare to one per shareEventEvery. A change inside the interval is emitted at its
// end, with the snapshot as it is then. Emits never overlap (emitMu), and none follows flush's: an emit takes its
// snapshot and then publishes it, so without that a timer's emit holding an older snapshot could publish after the
// share's last one (ended).
type coalescer struct {
	emitMu sync.Mutex  // held around every emit; taken before mu, never inside it
	mu     sync.Mutex  // guards the fields below
	last   time.Time   // the last emit
	timer  *time.Timer // a pending emit, or nil
	// stopped is set by flush, before its emit: an emit that has not begun by then is dropped.
	stopped bool
	emit    func()
}

// changed asks for an emit: now when the last one is old enough, else at the end of the interval.
func (k *coalescer) changed() {
	k.mu.Lock()
	if k.stopped || k.timer != nil {
		k.mu.Unlock()
		return
	}
	now := time.Now()
	if wait := k.last.Add(shareEventEvery).Sub(now); wait > 0 {
		k.timer = time.AfterFunc(wait, k.fire)
		k.mu.Unlock()
		return
	}
	k.last = now
	k.mu.Unlock()
	k.emitUnlessStopped()
}

func (k *coalescer) fire() {
	k.mu.Lock()
	k.timer = nil
	if k.stopped {
		k.mu.Unlock()
		return
	}
	k.last = time.Now()
	k.mu.Unlock()
	k.emitUnlessStopped()
}

// emitUnlessStopped emits unless flush has stopped the coalescer meanwhile: flush's emit is then the last.
func (k *coalescer) emitUnlessStopped() {
	k.emitMu.Lock()
	defer k.emitMu.Unlock()
	k.mu.Lock()
	stopped := k.stopped
	k.mu.Unlock()
	if !stopped {
		k.emit()
	}
}

// flush emits a pending change at once and stops the coalescer: the share's last event (ended) goes out without
// waiting, after any emit in progress, and nothing follows it.
func (k *coalescer) flush() {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return
	}
	k.stopped = true
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	k.mu.Unlock()
	k.emitMu.Lock()
	defer k.emitMu.Unlock()
	k.emit()
}
