package core

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
)

// The media pump (docs/m2/01-client-core.md D5, §11.5): the only caller of Session.Next. It hands media to the
// current Publisher through bounded queues, events and the fatal error to the share's actor, and it throttles the
// keyframe requests that reach the engine.

// The pump's limits (§13).
const (
	videoQueue   = 6                      // frames per video layer: 100 ms at 60 fps
	audioQueue   = 10                     // Opus packets: 200 ms
	keyThrottle  = 300 * time.Millisecond // keyframe requests to the engine per layer
	pumpOutDepth = 64                     // events waiting for the share's actor
)

// pumpMsg is what the pump tells the share's actor: an engine event (the seq-th the pump read, from 1), or the
// error that ended the session.
type pumpMsg struct {
	ev  *media.Event
	seq uint64
	err error
}

// pump drains one engine session. It lives exactly as long as the session.
type pump struct {
	sess   media.Session
	log    *slog.Logger
	out    chan pumpMsg
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed when the goroutine has ended
	events atomic.Uint64 // the engine events read so far: a pumpMsg's seq, and profileAsk's after

	mu       sync.Mutex // guards the fields below
	wake     chan struct{}
	consumer *consumer       // the attached one, or nil
	queue    []media.Packet  // the queued media of every stream, in arrival order
	counts   map[string]int  // queued packets per stream (streamKey)
	needKey  map[string]bool // video layers whose frames are dropped until a keyframe
	layers   []string        // the video layers, in session order
	active   map[string]bool // the video layers that are active (only those get keyframe requests)
	paused   bool            // D14: media is dropped, keyframe requests are owed (hints.go turns it on)
	halted   bool            // the share is ending: media is dropped and nothing goes to the engine
	stopping bool            // stop called: the session's end is the core's own
	keyLast  map[string]time.Time
	keyOwed  map[string]bool
	keyTimer map[string]*time.Timer
}

// newPump returns a pump for sess with its video layers (in session order) and which of them are active. run
// starts it.
func newPump(sess media.Session, layers []string, active map[string]bool, log *slog.Logger) *pump {
	p := &pump{
		sess: sess, log: log, out: make(chan pumpMsg, pumpOutDepth), done: make(chan struct{}),
		wake: make(chan struct{}), counts: map[string]int{}, needKey: map[string]bool{},
		layers: slices.Clone(layers), active: map[string]bool{},
		keyLast: map[string]time.Time{}, keyOwed: map[string]bool{}, keyTimer: map[string]*time.Timer{},
	}
	for _, l := range layers {
		p.active[l] = active[l]
		p.needKey[l] = true
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	return p
}

// streamKey names a packet's stream for the queue's bounds.
func streamKey(pkt media.Packet) string {
	if pkt.Kind == media.KindVideo {
		return "v:" + pkt.Layer
	}
	return "a"
}

// run calls Next until the session ends. A fatal error, or an end the core did not ask for, goes to the actor.
func (p *pump) run() {
	defer close(p.done)
	for {
		pkt, err := p.sess.Next(p.ctx)
		if err != nil {
			p.mu.Lock()
			ours := p.stopping
			p.mu.Unlock()
			if ours || p.ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			p.tell(pumpMsg{err: err})
			return
		}
		switch pkt.Kind {
		case media.KindEvent:
			if pkt.Event != nil {
				p.tell(pumpMsg{ev: pkt.Event, seq: p.events.Add(1)})
			}
		case media.KindVideo, media.KindAudio:
			p.push(pkt)
		default: // KindPCM: the core never asks for it
		}
	}
}

// tell hands a message to the actor; it waits while the actor is busy, until the pump stops.
func (p *pump) tell(m pumpMsg) {
	select {
	case p.out <- m:
	case <-p.ctx.Done():
	}
}

// push queues a media packet for the consumer, or drops it (§11.5): while paused or halted, without a consumer, and
// a video layer's frames until its keyframe. A full video queue drops the layer's oldest frame and its frames up to
// the next keyframe, which is requested; a full audio queue drops its oldest packet.
func (p *pump) push(pkt media.Packet) {
	video := pkt.Kind == media.KindVideo
	var askKey string
	p.mu.Lock()
	switch {
	case p.halted:
	case p.paused || p.consumer == nil:
		if video {
			p.needKey[pkt.Layer] = true
		}
	case video && p.needKey[pkt.Layer] && !pkt.Keyframe:
	default:
		key := streamKey(pkt)
		if video {
			if p.counts[key] >= videoQueue && !p.dropLayerLocked(pkt.Layer) && !pkt.Keyframe {
				askKey = pkt.Layer // the layer waits for a keyframe (needKey), and this frame goes too
				break
			}
			p.needKey[pkt.Layer] = false
		} else if p.counts[key] >= audioQueue {
			p.removeLocked(slices.IndexFunc(p.queue, func(q media.Packet) bool { return q.Kind == media.KindAudio }))
		}
		p.queue = append(p.queue, pkt)
		p.counts[key]++
		p.wakeLocked()
	}
	p.mu.Unlock()
	if askKey != "" {
		p.requestKey(askKey)
	}
}

// dropLayerLocked drops a full layer's oldest queued frame and its queued frames up to its next keyframe. It
// reports whether a queued keyframe is left; when none is, the layer waits for a keyframe (needKey) and the caller
// asks for one.
func (p *pump) dropLayerLocked(layer string) (keyLeft bool) {
	first := true
	for {
		i := slices.IndexFunc(p.queue, func(q media.Packet) bool { return q.Kind == media.KindVideo && q.Layer == layer })
		if i < 0 {
			p.needKey[layer] = true
			return false
		}
		if !first && p.queue[i].Keyframe {
			return true
		}
		first = false
		p.removeLocked(i)
	}
}

// removeLocked removes the queued packet at i (nothing for i < 0).
func (p *pump) removeLocked(i int) {
	if i < 0 {
		return
	}
	p.counts[streamKey(p.queue[i])]--
	p.queue = slices.Delete(p.queue, i, i+1)
}

func (p *pump) wakeLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

// attach makes k the consumer: the queue starts empty, every video layer waits for a keyframe, and every active
// layer gets a keyframe request (§11.5).
func (p *pump) attach(k *consumer) {
	p.mu.Lock()
	if k.detached {
		p.mu.Unlock()
		return
	}
	p.consumer = k
	p.queue, p.counts = nil, map[string]int{}
	for _, l := range p.layers {
		p.needKey[l] = true
	}
	ask := p.activeLocked()
	p.mu.Unlock()
	for _, l := range ask {
		p.requestKey(l)
	}
}

// detach ends k: its Next returns errDetached, and the media queued for it is dropped.
func (p *pump) detach(k *consumer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.detached = true
	if p.consumer == k {
		p.consumer = nil
		p.queue, p.counts = nil, map[string]int{}
	}
	p.wakeLocked()
}

// activeLocked returns the active video layers in session order.
func (p *pump) activeLocked() []string {
	var out []string
	for _, l := range p.layers {
		if p.active[l] {
			out = append(out, l)
		}
	}
	return out
}

// setActive records whether a video layer is active (01 §11.8's OpActive): only active layers get the keyframe
// requests of an attach or a resume. A layer turned on again is forwarded from its first keyframe (the engine starts
// it with one).
func (p *pump) setActive(layer string, active bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if was, ok := p.active[layer]; ok {
		p.active[layer] = active
		if active && !was {
			p.needKey[layer] = true
		}
	}
}

// setPaused drops media from now on (D14), or forwards again: audio at once, and each active video layer from the
// keyframe it is asked for.
func (p *pump) setPaused(paused bool) {
	p.mu.Lock()
	p.paused = paused
	var ask []string
	if paused {
		p.queue, p.counts = nil, map[string]int{}
	} else {
		for _, l := range p.layers {
			p.needKey[l] = true
		}
		ask = p.activeLocked()
		for _, l := range ask {
			p.keyOwed[l] = false
			delete(p.keyLast, l) // the resume's request is not throttled
		}
	}
	p.mu.Unlock()
	for _, l := range ask {
		p.requestKey(l)
	}
}

// requestKey passes a keyframe request on to the engine, at most one per layer per keyThrottle (§8.3): a request
// within the interval is owed and goes out at its end. While paused it is only owed (the resume asks anyway).
func (p *pump) requestKey(layer string) {
	p.mu.Lock()
	if _, ok := p.active[layer]; !ok || p.halted {
		p.mu.Unlock()
		return
	}
	if p.paused {
		p.keyOwed[layer] = true
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if last, ok := p.keyLast[layer]; ok && now.Sub(last) < keyThrottle {
		p.keyOwed[layer] = true
		if p.keyTimer[layer] == nil {
			p.keyTimer[layer] = time.AfterFunc(last.Add(keyThrottle).Sub(now), func() { p.owed(layer) })
		}
		p.mu.Unlock()
		return
	}
	p.keyLast[layer], p.keyOwed[layer] = now, false
	p.mu.Unlock()
	p.sess.RequestKeyframe(layer)
}

// owed sends a keyframe request that waited for the throttle.
func (p *pump) owed(layer string) {
	p.mu.Lock()
	delete(p.keyTimer, layer)
	owed := p.keyOwed[layer] && !p.paused && !p.halted
	p.mu.Unlock()
	if owed {
		p.requestKey(layer)
	}
}

// halt drops every media packet from now on: nothing more leaves the PC while the share ends.
func (p *pump) halt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.halted = true
	p.queue, p.counts = nil, map[string]int{}
	for l, t := range p.keyTimer {
		t.Stop()
		delete(p.keyTimer, l)
	}
	p.wakeLocked()
}

// stop stops the session (it unblocks Next within 200 ms) and waits for the pump's goroutine.
func (p *pump) stop() {
	p.halt()
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	if err := p.sess.Stop(); err != nil {
		p.log.Debug("engine session stop", logx.Err(err))
	}
	p.cancel()
	<-p.done
}

// errDetached ends the Next of a consumer whose Publisher was replaced or closed.
var errDetached = errors.New("core: the media pump detached this consumer")

// consumer is a Publisher's end of the pump: its publish.Source, publish.Clock (Session.Now) and
// publish.ProfileSource (SetProfile → Session.Reconfigure{Profile}). It attaches at its first Next, which the
// Publisher's send loop makes once its PeerConnection is connected, so the first frames it gets start on fresh
// keyframes.
type consumer struct {
	p        *pump
	once     sync.Once
	detached bool // guarded by p.mu
	// ask gets the profile of every SetProfile for each video layer before the session re-initializes, and failed
	// hears of a SetProfile the session refused: the share's check of the codec switch (hints.go). Either may be nil.
	ask    *profileAsk
	failed func(profile string)
}

var (
	_ publish.Source        = (*consumer)(nil)
	_ publish.Clock         = (*consumer)(nil)
	_ publish.ProfileSource = (*consumer)(nil)
)

// Next implements publish.Source.
func (k *consumer) Next(ctx context.Context) (media.Packet, error) {
	k.once.Do(func() { k.p.attach(k) })
	p := k.p
	for {
		p.mu.Lock()
		switch {
		case k.detached:
			p.mu.Unlock()
			return media.Packet{}, errDetached
		case p.consumer == k && len(p.queue) > 0:
			pkt := p.queue[0]
			p.queue = slices.Delete(p.queue, 0, 1)
			p.counts[streamKey(pkt)]--
			p.mu.Unlock()
			return pkt, nil
		}
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return media.Packet{}, ctx.Err()
		}
	}
}

// RequestKeyframe implements publish.Source: a PLI or FIR of the SFU, throttled by the pump.
func (k *consumer) RequestKeyframe(layer string) { k.p.requestKey(layer) }

// Now implements publish.Clock: the engine's clock (D6).
func (k *consumer) Now() int64 { return k.p.sess.Now() }

// SetProfile implements publish.ProfileSource: the engine re-initializes its encoders in profile (§8.6). The share
// hears of the profile asked for before the call, for every video layer and with the count of the events read so
// far, so each layer's EventLayer of the re-init finds it (and none read before the call does), and of a refusal.
func (k *consumer) SetProfile(profile string) error {
	if k.ask != nil {
		k.ask.set(k.p.layers, profile, k.p.events.Load())
	}
	err := k.p.sess.Reconfigure(media.Reconfig{Profile: profile})
	if err != nil && k.failed != nil {
		k.failed(profile)
	}
	return err
}
