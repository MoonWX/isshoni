package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// The media pump's rules (docs/m2/01-client-core.md §11.5, §8.3) on a scripted session.

// scriptSession is a media.Session whose Next hands out what the test feeds it, and which records the keyframe
// requests and reconfigurations it gets.
type scriptSession struct {
	feed chan media.Packet
	fail chan error
	stop chan struct{}
	once sync.Once

	mu       sync.Mutex
	keys     []string
	keyTimes []time.Time
	reconfs  []media.Reconfig
	stopped  bool
}

func newScriptSession() *scriptSession {
	return &scriptSession{feed: make(chan media.Packet), fail: make(chan error, 1), stop: make(chan struct{})}
}

func (s *scriptSession) Next(ctx context.Context) (media.Packet, error) {
	select {
	case p := <-s.feed:
		return p, nil
	case err := <-s.fail:
		return media.Packet{}, err
	case <-s.stop:
		return media.Packet{}, media.ErrStopped
	case <-ctx.Done():
		return media.Packet{}, ctx.Err()
	}
}

func (s *scriptSession) Now() int64 { return 42 }

func (s *scriptSession) RequestKeyframe(layer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, layer)
	s.keyTimes = append(s.keyTimes, time.Now())
}

func (s *scriptSession) SetBitrate(string, int) error      { return nil }
func (s *scriptSession) SetLayerActive(string, bool) error { return nil }
func (s *scriptSession) SetPaused(bool) error              { return nil }
func (s *scriptSession) Stats() (media.EngineStats, error) { return media.EngineStats{}, nil }
func (s *scriptSession) Reconfigure(r media.Reconfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconfs = append(s.reconfs, r)
	return nil
}

func (s *scriptSession) Stop() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.stop)
	})
	return nil
}

func (s *scriptSession) keyRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

// pumpEnv is a running pump on a scripted session with layers f and q, both active.
type pumpEnv struct {
	t    *testing.T
	sess *scriptSession
	p    *pump
	n    int64 // capture times of the packets fed
}

func newPumpEnv(t *testing.T) *pumpEnv {
	t.Helper()
	sess := newScriptSession()
	p := newPump(sess, []string{"f", "q"}, map[string]bool{"f": true, "q": true}, slog.New(slog.DiscardHandler))
	go p.run()
	t.Cleanup(p.stop)
	return &pumpEnv{t: t, sess: sess, p: p}
}

// video feeds a frame of layer; key makes it a keyframe. It returns the frame's capture time.
func (e *pumpEnv) video(layer string, key bool) int64 {
	e.n++
	e.feed(media.Packet{Kind: media.KindVideo, Layer: layer, Keyframe: key, CaptureNS: e.n, Data: []byte{1}})
	return e.n
}

// audio feeds an Opus packet and returns its capture time.
func (e *pumpEnv) audio() int64 {
	e.n++
	e.feed(media.Packet{Kind: media.KindAudio, CaptureNS: e.n, Data: []byte{1}})
	return e.n
}

func (e *pumpEnv) feed(p media.Packet) {
	e.t.Helper()
	select {
	case e.sess.feed <- p:
	case <-time.After(waitTimeout):
		e.t.Fatal("the pump does not pull")
	}
	e.settle()
}

// settle waits until the pump has handled what was fed: the next send can only happen once Next is called again.
func (e *pumpEnv) settle() {
	e.t.Helper()
	select {
	case e.sess.feed <- media.Packet{Kind: media.KindPCM, Data: make([]byte, 3840)}: // dropped by the pump
	case <-time.After(waitTimeout):
		e.t.Fatal("the pump does not pull")
	}
}

// queued returns the capture times queued for the consumer, in order.
func (e *pumpEnv) queued() []int64 {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	var out []int64
	for _, q := range e.p.queue {
		out = append(out, q.CaptureNS)
	}
	return out
}

// attach attaches a consumer, as its Publisher's first Next would.
func (e *pumpEnv) attach() *consumer {
	k := &consumer{p: e.p}
	k.once.Do(func() { e.p.attach(k) })
	return k
}

func TestPumpDropsWithoutConsumer(t *testing.T) {
	e := newPumpEnv(t)
	e.video("f", true)
	e.audio()
	if q := e.queued(); len(q) != 0 {
		t.Fatalf("queued %v without a consumer, want nothing", q)
	}
	k := e.attach()
	if keys := e.sess.keyRequests(); !slices.Equal(keys, []string{"f", "q"}) {
		t.Errorf("keyframe requests at attach = %v, want f and q", keys)
	}
	e.video("f", false) // not a keyframe: dropped
	a := e.audio()
	kf := e.video("f", true)
	p := e.video("f", false)
	if q := e.queued(); !slices.Equal(q, []int64{a, kf, p}) {
		t.Errorf("queued %v, want the audio, then f from its keyframe: %v", q, []int64{a, kf, p})
	}
	ctx := ctxFor(t)
	for _, want := range []int64{a, kf, p} {
		pkt, err := k.Next(ctx)
		if err != nil || pkt.CaptureNS != want {
			t.Fatalf("Next = %+v, %v; want capture %d", pkt, err, want)
		}
	}
	e.p.detach(k)
	if _, err := k.Next(ctx); !errors.Is(err, errDetached) {
		t.Errorf("Next after detach = %v, want errDetached", err)
	}
	if k.Now() != 42 {
		t.Errorf("Now = %d, want the session's", k.Now())
	}
	if err := k.SetProfile("42e0"); err != nil || len(e.sess.reconfs) != 1 || e.sess.reconfs[0].Profile != "42e0" {
		t.Errorf("SetProfile: %v, reconfigurations %+v; want Reconfigure{Profile: 42e0}", err, e.sess.reconfs)
	}
}

func TestPumpQueueBounds(t *testing.T) {
	e := newPumpEnv(t)
	e.attach()
	time.Sleep(keyThrottle) // the attach's requests are out of the throttle's way
	first := e.video("f", true)
	for range videoQueue - 1 {
		e.video("f", false)
	}
	if q := e.queued(); len(q) != videoQueue || q[0] != first {
		t.Fatalf("queued %v, want %d frames from the keyframe", q, videoQueue)
	}
	// A seventh frame: the oldest goes, and with it every frame up to the next keyframe; none is queued, so the
	// layer waits for one, which is requested.
	before := len(e.sess.keyRequests())
	e.video("f", false)
	if q := e.queued(); len(q) != 0 {
		t.Errorf("queued %v after the overflow, want the layer dropped up to its next keyframe", q)
	}
	if keys := e.sess.keyRequests()[before:]; !slices.Equal(keys, []string{"f"}) {
		t.Errorf("keyframe requests after the overflow = %v, want one for f", keys)
	}
	e.video("f", false) // still waiting
	kf := e.video("f", true)
	if q := e.queued(); !slices.Equal(q, []int64{kf}) {
		t.Errorf("queued %v, want the new keyframe alone", q)
	}

	// Audio drops its oldest packet.
	var audio []int64
	for range audioQueue + 2 {
		audio = append(audio, e.audio())
	}
	var queuedAudio []int64
	e.p.mu.Lock()
	for _, p := range e.p.queue {
		if p.Kind == media.KindAudio {
			queuedAudio = append(queuedAudio, p.CaptureNS)
		}
	}
	e.p.mu.Unlock()
	if !slices.Equal(queuedAudio, audio[2:]) {
		t.Errorf("queued audio %v, want the newest %d: %v", queuedAudio, audioQueue, audio[2:])
	}
}

func TestPumpKeyframeThrottle(t *testing.T) {
	e := newPumpEnv(t)
	e.p.requestKey("f")
	e.p.requestKey("f") // within 300 ms: owed
	e.p.requestKey("f") // still one owed
	e.p.requestKey("q")
	e.p.requestKey("x") // not a layer of the session: ignored
	eventually(t, func() bool { return len(e.sess.keyRequests()) == 3 },
		func() string {
			return fmt.Sprintf("keyframe requests %v, want f, q, then the owed f", e.sess.keyRequests())
		})
	e.sess.mu.Lock()
	keys, times := slices.Clone(e.sess.keys), slices.Clone(e.sess.keyTimes)
	e.sess.mu.Unlock()
	if !slices.Equal(keys, []string{"f", "q", "f"}) {
		t.Errorf("keyframe requests %v, want f, q, f", keys)
	}
	if gap := times[2].Sub(times[0]); gap < keyThrottle-5*time.Millisecond {
		t.Errorf("the owed request went out %v after the first, want %v", gap, keyThrottle)
	}
	time.Sleep(2 * keyThrottle)
	if n := len(e.sess.keyRequests()); n != 3 {
		t.Errorf("%d keyframe requests, want 3", n)
	}
}

func TestPumpPausedAndActive(t *testing.T) {
	e := newPumpEnv(t)
	e.p.setActive("q", false)
	e.attach()
	if keys := e.sess.keyRequests(); !slices.Equal(keys, []string{"f"}) {
		t.Errorf("keyframe requests at attach = %v, want only the active f", keys)
	}
	e.video("f", true)
	e.p.setPaused(true)
	if q := e.queued(); len(q) != 0 {
		t.Errorf("queued %v after the pause, want nothing", q)
	}
	e.video("f", true)
	e.audio()
	e.p.requestKey("f") // swallowed while paused: owed
	if q := e.queued(); len(q) != 0 || len(e.sess.keyRequests()) != 1 {
		t.Errorf("while paused: queued %v, keyframe requests %v; want nothing new", q, e.sess.keyRequests())
	}
	e.p.setPaused(false)
	if keys := e.sess.keyRequests(); !slices.Equal(keys, []string{"f", "f"}) {
		t.Errorf("keyframe requests after the resume = %v, want one more for f, not throttled", keys)
	}
	a := e.audio()
	e.video("f", false) // video waits for the keyframe; audio goes at once
	kf := e.video("f", true)
	if q := e.queued(); !slices.Equal(q, []int64{a, kf}) {
		t.Errorf("queued %v after the resume, want the audio and f from its keyframe", q)
	}
}

func TestPumpFatalAndStop(t *testing.T) {
	e := newPumpEnv(t)
	ev := media.Event{Code: media.EventLayer, Layer: "f", Profile: "6400"}
	e.feed(media.Packet{Kind: media.KindEvent, Event: &ev})
	select {
	case m := <-e.p.out:
		if m.ev == nil || m.ev.Layer != "f" {
			t.Errorf("the pump told %+v, want the EventLayer", m)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no event from the pump")
	}
	e.sess.fail <- &media.Error{Code: media.CodeSourceGone, Op: "next"}
	select {
	case m := <-e.p.out:
		if !errors.Is(m.err, media.ErrSourceGone) {
			t.Errorf("the pump told %+v, want the fatal source_gone", m)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no fatal error from the pump")
	}
	select {
	case <-e.p.done:
	case <-time.After(waitTimeout):
		t.Fatal("the pump goes on after a fatal error")
	}

	// The core's own stop is no fatal error.
	s := newPumpEnv(t)
	s.p.stop()
	select {
	case m := <-s.p.out:
		t.Errorf("the pump told %+v after the core stopped it", m)
	default:
	}
	if !s.sess.stopped {
		t.Error("stop did not stop the session")
	}
}
