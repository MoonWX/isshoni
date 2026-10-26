package share

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// callLog records the calls of every fake in one order, so a test checks what came before what.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

func (l *callLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

// count is how many calls start with prefix.
func (l *callLog) count(prefix string) int {
	n := 0
	for _, c := range l.get() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// order checks that a call starting with each of want occurs in the log, in this order (other calls may come
// between).
func (l *callLog) order(t *testing.T, want ...string) {
	t.Helper()
	calls := l.get()
	i := 0
	for _, c := range calls {
		if i < len(want) && strings.HasPrefix(c, want[i]) {
			i++
		}
	}
	if i < len(want) {
		t.Fatalf("calls %q: want %q in this order", calls, want)
	}
}

// fakeEngine is the engine's picker. Each pick gets a new "pick:<n>" id, like 01's fake engine.
type fakeEngine struct {
	log *callLog

	mu    sync.Mutex
	src   media.Source
	err   error
	block bool // Pick waits for its ctx
	n     int
}

func (e *fakeEngine) Pick(ctx context.Context, req media.PickRequest) (media.Source, error) {
	e.log.add("engine.Pick(owner=%#x)", req.OwnerWindow)
	e.mu.Lock()
	src, err, block := e.src, e.err, e.block
	if err == nil && !block {
		e.n++
		src.ID = fmt.Sprintf("pick:%d", e.n)
	}
	e.mu.Unlock()
	if block {
		<-ctx.Done()
		return media.Source{}, fmt.Errorf("%w: %w", &media.Error{Code: media.CodeCancelled, Op: "pick"}, ctx.Err())
	}
	if err != nil {
		return media.Source{}, err
	}
	return src, nil
}

func (e *fakeEngine) Release(id string) { e.log.add("engine.Release(%s)", id) }

func (e *fakeEngine) set(fn func(e *fakeEngine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

// fakeClient is C2's core client: its status, its subscribers and one share at a time.
type fakeClient struct {
	log *callLog

	mu           sync.Mutex
	status       core.Status
	subs         map[int]chan core.Event
	nextSub      int
	startErr     error
	startBlock   bool          // StartShare waits for its ctx
	startGate    chan struct{} // StartShare waits for it to close, ignoring its ctx, then succeeds
	started      []core.ShareRequest
	share        *fakeShare
	stopOtherErr error
}

func newFakeClient(log *callLog) *fakeClient {
	return &fakeClient{log: log, subs: map[int]chan core.Event{}, status: core.Status{Conn: core.ConnOnline,
		ConnectionID: "c_k3v9q2m7xw4pa8d1", Features: []protocol.Feature{protocol.FeatureSharePause}}}
}

func (c *fakeClient) Status() core.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Features = slices.Clone(st.Features)
	return st
}

func (c *fakeClient) Subscribe() (<-chan core.Event, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	ch := make(chan core.Event, 64)
	c.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs, id)
			c.mu.Unlock()
			close(ch)
		})
	}
}

// emit sends ev to every subscriber; like the core, it never blocks.
func (c *fakeClient) emit(ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (c *fakeClient) subscribers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}

func (c *fakeClient) StartShare(ctx context.Context, r core.ShareRequest) (Share, error) {
	c.log.add("core.StartShare(room=%s,source=%s,preset=%s,audio=%v,label=%q)", r.RoomID, r.Source.ID, r.Preset,
		r.Audio, r.Label)
	c.mu.Lock()
	c.started = append(c.started, r)
	err, block, gate := c.startErr, c.startBlock, c.startGate
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	s := &fakeShare{c: c, done: make(chan struct{}), snap: core.ShareSnapshot{ID: "s_q7m2k9x4w3bd8c1a",
		RoomID: r.RoomID, Kind: kindOf(r.Source.Kind), Preset: r.Preset, State: core.StateStarting, Audio: r.Audio}}
	c.mu.Lock()
	c.share = s
	c.mu.Unlock()
	return s, nil
}

func (c *fakeClient) StopOther(_ context.Context, roomID, shareID string) error {
	c.log.add("core.StopOther(%s,%s)", roomID, shareID)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopOtherErr
}

func (c *fakeClient) WatchServerStats(on bool) { c.log.add("core.WatchServerStats(%v)", on) }

func (c *fakeClient) set(fn func(c *fakeClient)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

func (c *fakeClient) current() *fakeShare {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.share
}

// fakeShare is one core share. update and end change its snapshot and fire EventShare, as the core does.
type fakeShare struct {
	c *fakeClient

	mu        sync.Mutex
	snap      core.ShareSnapshot
	done      chan struct{}
	ended     bool
	pausedErr error
	presetErr error
}

func (s *fakeShare) Snapshot() core.ShareSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.Watchers = slices.Clone(s.snap.Watchers)
	snap.Layers = slices.Clone(s.snap.Layers)
	snap.Server = slices.Clone(s.snap.Server)
	if s.snap.End != nil {
		e := *s.snap.End
		snap.End = &e
	}
	return snap
}

func (s *fakeShare) update(fn func(*core.ShareSnapshot)) {
	s.mu.Lock()
	fn(&s.snap)
	snap := s.snap
	s.mu.Unlock()
	s.c.emit(core.Event{Kind: core.EventShare, Share: &snap})
}

// end ends the share with info (once), as the core's over does: State ended, End, Done closed, a last EventShare.
func (s *fakeShare) end(info core.EndInfo) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.snap.State, s.snap.End = core.StateEnded, &info
	snap := s.snap
	s.mu.Unlock()
	close(s.done)
	s.c.emit(core.Event{Kind: core.EventShare, Share: &snap})
}

func (s *fakeShare) SetPreset(_ context.Context, p protocol.Preset) error {
	s.c.log.add("core.Share.SetPreset(%s)", p)
	s.mu.Lock()
	err := s.presetErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.update(func(snap *core.ShareSnapshot) { snap.Preset = p })
	return nil
}

func (s *fakeShare) SetPaused(_ context.Context, paused bool) error {
	s.c.log.add("core.Share.SetPaused(%v)", paused)
	s.mu.Lock()
	err := s.pausedErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.update(func(snap *core.ShareSnapshot) { snap.Paused = paused })
	return nil
}

func (s *fakeShare) Stop(ctx context.Context) error {
	s.c.log.add("core.Share.Stop")
	s.end(core.EndInfo{Reason: core.EndUser, ByThisApp: true})
	return ctx.Err()
}

func (s *fakeShare) Done() <-chan struct{} { return s.done }

// fakePublisher is C2's session: Ready answers with the client, an error, or waits for ctx (C2 never online).
type fakePublisher struct {
	mu     sync.Mutex
	client *fakeClient
	origin string
	err    error
	block  bool
	calls  int
}

func (p *fakePublisher) Ready(ctx context.Context) (Client, string, error) {
	p.mu.Lock()
	p.calls++
	client, origin, err, block := p.client, p.origin, p.err, p.block
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, "", err
	}
	return client, origin, nil
}

func (p *fakePublisher) set(fn func(p *fakePublisher)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

// fakeAudio is 05's audio policy.
type fakeAudio struct {
	log      *callLog
	soundErr error
}

func (a *fakeAudio) OnShareStarted(pid uint32, withAudio bool) {
	a.log.add("audio.OnShareStarted(%d,%v)", pid, withAudio)
}
func (a *fakeAudio) OnShareStopped()       { a.log.add("audio.OnShareStopped") }
func (a *fakeAudio) SetPaused(paused bool) { a.log.add("audio.SetPaused(%v)", paused) }
func (a *fakeAudio) SetShareSound(on bool) error {
	a.log.add("audio.SetShareSound(%v)", on)
	return a.soundErr
}

// fakeWindow is the main window.
type fakeWindow struct{ log *callLog }

func (w *fakeWindow) ShowWindow(focus bool)     { w.log.add("window.Show(%v)", focus) }
func (w *fakeWindow) MainWindowHandle() uintptr { return 0x1234 }

// fakeSettings is settings.json in memory.
type fakeSettings struct {
	mu sync.Mutex
	s  settings.Settings
}

func (f *fakeSettings) Get() settings.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.s
	s.ServerPrefs = make(map[string]settings.ServerPrefs, len(f.s.ServerPrefs))
	for k, v := range f.s.ServerPrefs {
		s.ServerPrefs[k] = v
	}
	return s
}

func (f *fakeSettings) Update(fn func(*settings.Settings)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := f.s
	next.ServerPrefs = make(map[string]settings.ServerPrefs, len(f.s.ServerPrefs))
	for k, v := range f.s.ServerPrefs {
		next.ServerPrefs[k] = v
	}
	fn(&next)
	f.s = next
	return nil
}

// recorder keeps every event the controller delivers.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func record(c *Controller) (*recorder, func()) {
	r := &recorder{}
	cancel := c.Subscribe(func(ev Event) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	})
	return r, cancel
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// kind returns the events of kind k.
func (r *recorder) kind(k EventKind) []Event {
	var out []Event
	for _, ev := range r.all() {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// lastView is the last share.state's view (nil for none or no event).
func (r *recorder) lastView() *View {
	states := r.kind(EventState)
	if len(states) == 0 {
		return nil
	}
	return states[len(states)-1].View
}

// states lists the states of the share.state events in order ("none" for a nil view), without repeats.
func (r *recorder) states() []string {
	var out []string
	for _, ev := range r.kind(EventState) {
		s := "none"
		if ev.View != nil {
			s = string(ev.View.State)
		}
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// env is a controller over fakes.
type env struct {
	log    *callLog
	eng    *fakeEngine
	client *fakeClient
	pub    *fakePublisher
	audio  *fakeAudio
	set    *fakeSettings
	ctl    *Controller
	rec    *recorder
}

const testOrigin = "https://watch.example.com"

func newEnv(t *testing.T) *env {
	t.Helper()
	log := &callLog{}
	e := &env{
		log: log,
		eng: &fakeEngine{log: log, src: media.Source{Kind: media.SourceWindow, Name: "Minecraft", Width: 1920,
			Height: 1080, PID: 4120, Capture: "wgc", OSBorder: true}},
		client: newFakeClient(log),
		audio:  &fakeAudio{log: log},
		set:    &fakeSettings{s: settings.Defaults()},
	}
	e.pub = &fakePublisher{client: e.client, origin: testOrigin}
	e.ctl = New(Options{Engine: e.eng, Publisher: e.pub, Audio: e.audio, Window: &fakeWindow{log: log},
		Settings: e.set, Log: slog.New(slog.DiscardHandler)})
	var cancel func()
	e.rec, cancel = record(e.ctl)
	t.Cleanup(func() {
		cancel()
		e.ctl.Close()
	})
	return e
}

// pick picks a source and returns its pickId.
func (e *env) pick(t *testing.T) *Picked {
	t.Helper()
	p, err := e.ctl.Pick(context.Background())
	if err != nil || p == nil {
		t.Fatalf("Pick = %+v, %v", p, err)
	}
	return p
}

// start picks and starts a share in room, and returns the core's fake share.
func (e *env) start(t *testing.T, room string, withAudio bool) *fakeShare {
	t.Helper()
	p := e.pick(t)
	if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: room,
		Preset: protocol.PresetGame, WithAudio: withAudio}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return e.client.current()
}

// goLive makes the share live with a watcher and both layers.
func goLive(s *fakeShare) {
	s.update(func(snap *core.ShareSnapshot) {
		snap.State = core.StateLive
		snap.Watchers = []protocol.Watcher{{UserID: "u_2"}}
		snap.Encoder, snap.HW = "nvenc", true
		snap.Layers = []core.LayerSnapshot{
			{RID: "f", Width: 1920, Height: 1080, FPS: 59.6, Active: true, Encoder: "nvenc", HW: true,
				TargetBps: 8_000_000, SentBps: 7_712_000},
			{RID: "q", Width: 640, Height: 360, FPS: 15, Active: true, Encoder: "nvenc", HW: true, TargetBps: 300_000,
				SentBps: 296_000},
		}
		snap.AudioSentBps = 128_000
	})
}
