package audio

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// testSelf is the SelfInfo of the tests: a made-up pid and folder (the repository is public: no real paths).
var testSelf = SelfInfo{PID: 4242, WebViewDataDir: `C:\isshoni-test\WebView2`}

// memStore is a SettingsStore in memory that counts its saves and can fail them.
type memStore struct {
	mu    sync.Mutex
	s     Settings
	saves int
	fail  error
}

func newStore(s Settings) *memStore { return &memStore{s: s} }

func (m *memStore) AudioSettings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.clone()
}

func (m *memStore) SaveAudioSettings(s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.s = s.clone()
	m.saves++
	return nil
}

func (m *memStore) get() (Settings, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.clone(), m.saves
}

// recorder records a service's events.
type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func record(s *Service) *recorder {
	r := &recorder{}
	s.Subscribe(func(ev Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.evs = append(r.evs, ev)
	})
	return r
}

// take returns the events recorded since the last take.
func (r *recorder) take() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := r.evs
	r.evs = nil
	return evs
}

// named returns the events of evs with name.
func named(evs []Event, name string) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

// runService starts s.Run and returns a stop that closes s and waits for Run; tests call it before they return
// (inside a synctest bubble, the bubble must be empty when the test function ends).
func runService(t *testing.T, s *Service) (stop func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = s.Close()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
}

// ctlCall is one call a stubCtl received.
type ctlCall struct {
	op   string
	ex   media.Exclusions // watch, exclusions
	mode media.AudioMode  // mode
	pid  uint32           // soundcheck
	at   time.Time
}

// stubCtl is a media.AudioController with a report set by the test, for the cases no fake scenario makes.
type stubCtl struct {
	mu     sync.Mutex
	report media.AudioReport
	levels media.AudioLevels
	calls  []ctlCall
	errs   map[string]error // op → the error of every such call
	sound  media.SoundCheckResult
}

var _ media.AudioController = (*stubCtl)(nil)

func newStub() *stubCtl {
	return &stubCtl{errs: map[string]error{}, report: media.AudioReport{V: 1, Seq: 1, Mode: media.AudioExcludeApps,
		Engine: "include-set", State: "ok", Probe: media.AudioProbeResult{Result: "ok", OSBuild: 26100}}}
}

// set replaces the report's content (its Seq goes up) with what edit makes of it.
func (c *stubCtl) set(edit func(r *media.AudioReport)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := c.report.Seq
	edit(&c.report)
	c.report.Seq = seq + 1
}

func (c *stubCtl) fail(op string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs[op] = err
}

func (c *stubCtl) rec(cl ctlCall) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl.at = time.Now()
	cl.ex = cloneExclusions(cl.ex)
	c.calls = append(c.calls, cl)
	return c.errs[cl.op]
}

// ops returns the calls of the given ops (all with none).
func (c *stubCtl) ops(ops ...string) []ctlCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlCall
	for _, cl := range c.calls {
		if len(ops) == 0 || slices.Contains(ops, cl.op) {
			out = append(out, cl)
		}
	}
	return out
}

// lastEx returns the rules of the last watch or exclusions call.
func (c *stubCtl) lastEx(t *testing.T) media.Exclusions {
	t.Helper()
	calls := c.ops("watch", "exclusions")
	if len(calls) == 0 {
		t.Fatal("no rules reached the engine")
	}
	return calls[len(calls)-1].ex
}

func (c *stubCtl) WatchAudio(_ context.Context, ex media.Exclusions) error {
	return c.rec(ctlCall{op: "watch", ex: ex})
}
func (c *stubCtl) UnwatchAudio(context.Context) error { return c.rec(ctlCall{op: "unwatch"}) }
func (c *stubCtl) SetExclusions(_ context.Context, ex media.Exclusions) error {
	return c.rec(ctlCall{op: "exclusions", ex: ex})
}
func (c *stubCtl) SetAudioMode(_ context.Context, m media.AudioMode) error {
	return c.rec(ctlCall{op: "mode", mode: m})
}
func (c *stubCtl) SetAudioMuted(context.Context, bool) error { return c.rec(ctlCall{op: "muted"}) }

func (c *stubCtl) AudioReport(context.Context) (media.AudioReport, error) {
	if err := c.rec(ctlCall{op: "report"}); err != nil {
		return media.AudioReport{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.report
	r.Apps = slices.Clone(r.Apps)
	r.Sessions = slices.Clone(r.Sessions)
	return r, nil
}

func (c *stubCtl) AudioLevels(context.Context) (media.AudioLevels, error) {
	if err := c.rec(ctlCall{op: "levels"}); err != nil {
		return media.AudioLevels{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lv := c.levels
	lv.ReportSeq = c.report.Seq
	lv.Apps = slices.Clone(lv.Apps)
	return lv, nil
}

func (c *stubCtl) SoundCheck(ctx context.Context, pid uint32, timeout time.Duration) (media.SoundCheckResult, error) {
	if err := c.rec(ctlCall{op: "soundcheck", pid: pid}); err != nil {
		return media.SoundCheckResult{}, err
	}
	if timeout != soundCheckTimeout {
		return media.SoundCheckResult{}, errors.New("unexpected timeout")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sound, nil
}

// app is a report row: an exe app ("exe:" + exe) with its verdict and reason.
func app(exe, name, verdict, reason string) media.AudioApp {
	return media.AudioApp{ID: "exe:" + exe, Name: name, Exe: exe, Verdict: verdict, Reason: reason,
		Warnings: []string{}}
}

// session is an active session of exe.
func session(exe string, pid uint32, state string) media.AudioSession {
	return media.AudioSession{PID: pid, Exe: exe, AppID: "exe:" + exe, Flow: "render", State: state}
}

// settle passes d of the bubble's fake time and lets the service's Run loop finish what that woke (synctest only).
func settle(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// rowByID returns the FriendsHear row with id from any group, and its group's name.
func rowByID(fh FriendsHear, id string) (AppRow, string) {
	for _, g := range []struct {
		name string
		rows []AppRow
	}{{"heard", fh.Heard}, {"keptOut", fh.KeptOut}, {"notCaptured", fh.NotCaptured}} {
		for _, r := range g.rows {
			if r.ID == id {
				return r, g.name
			}
		}
	}
	return AppRow{}, ""
}

// hasRule reports whether ex has a rule of kind and source on value.
func hasRule(ex media.Exclusions, kind media.RuleKind, src media.RuleSource, value string) bool {
	return slices.ContainsFunc(ex.Rules, func(r media.Rule) bool {
		return r.Kind == kind && r.Source == src && r.Value == value
	})
}

// banner returns fh's banner with code.
func banner(fh FriendsHear, code string) (Banner, bool) {
	i := slices.IndexFunc(fh.Banners, func(b Banner) bool { return b.Code == code })
	if i < 0 {
		return Banner{}, false
	}
	return fh.Banners[i], true
}

// newFake returns the fake engine with an audio scenario of 05 §13.3.
func newFake(scenario string) *fake.Engine {
	return fake.NewEngine(fake.EngineConfig{AudioScenario: scenario})
}

// startShare starts a fake session with audio under the service's AudioConfig, as 01's core does for a share with
// sound right after OnShareStarted (the listed monitor stands in for the pick).
func startShare(t *testing.T, eng *fake.Engine, s *Service) *fake.Session {
	t.Helper()
	ac := s.AudioConfig()
	sess, err := eng.Start(t.Context(), media.Config{
		Source: fake.ListedMonitorID, Listed: true, Profile: "6400", Preset: "auto", Borderless: true,
		Layers: []media.LayerConfig{{RID: "f", MaxPixels: 1280 * 720, MaxFPS: 30, Bitrate: 2_000_000}},
		Audio:  &ac,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sess.(*fake.Session)
}

// audioCalls returns the session's audio_mode and audio_muted calls, without their times.
func audioCalls(sess *fake.Session) []fake.Call {
	var out []fake.Call
	for _, c := range sess.Calls() {
		if c.Op == "audio_mode" || c.Op == "audio_muted" {
			c.At = time.Time{}
			out = append(out, c)
		}
	}
	return out
}
