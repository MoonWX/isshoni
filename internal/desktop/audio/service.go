package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// The service's times (05 §14.3, §14.4, §5.9).
const (
	rulesDebounce = 50 * time.Millisecond  // a rule change reaches SetExclusions this long after it, at the latest
	pollLevels    = 100 * time.Millisecond // AudioLevels while a level meter is on screen (audio.levels at 10 Hz)
	pollIdle      = 250 * time.Millisecond // AudioLevels otherwise, only to see ReportSeq change
	minReportGap  = 250 * time.Millisecond // audio.report from engine reports at most 4 times a second
	viewerRecheck = 30 * time.Second       // a viewer family with no active session is re-checked this long
	alertRepeat   = 60 * time.Second       // an error banner that comes back alerts again only after this
	callTimeout   = 5 * time.Second        // the bound of an engine call made by a method without a ctx
	maxNames      = 1024                   // display names remembered for AppRules
)

// Service is the desktop audio policy service (05 §14). Its methods are safe for concurrent use. New starts nothing;
// Run polls the engine and applies rule changes (debounced) until its ctx ends or Close, so the app runs it for the
// service's whole life; Close stops the engine's watcher.
//
// Methods without a context bound their engine calls to 5 s. Events go to the Subscribe callbacks in the order they
// happened, from the goroutine that caused them (Run's, or a method's caller), never under the service's locks: a
// callback may call the service.
type Service struct {
	eng      media.AudioController
	st       SettingsStore
	self     SelfInfo
	log      *slog.Logger
	ducking0 func() duckingPref // readDucking; tests replace it

	base    context.Context // ends with Close (and with it Run); the engine calls of methods without a ctx derive from it
	cancel  context.CancelFunc
	wake    chan struct{} // tells Run to re-plan; 1-buffered
	running atomic.Bool

	// opMu serializes the engine calls that change the watcher, the rules and the mode, so the engine gets them in
	// the order they were decided. Never taken under mu.
	opMu sync.Mutex
	// setMu serializes settings changes: read, save to the store, then apply. Never taken under mu.
	setMu sync.Mutex

	mu        sync.Mutex
	closed    bool
	settings  Settings // normalized
	sharedPID uint32
	sheet     bool // Watch(true)
	watcherOn bool // WatchAudio succeeded and no UnwatchAudio followed
	live      bool // OnShareStarted … OnShareStopped
	withAudio bool // the live share has an audio track
	soundOn   bool // Share sound
	paused    bool // SetPaused, during the live share
	// modePending: SetShareSound came before the share's audio session ran (ErrAudioNotRunning); the next live
	// report applies it.
	modePending bool
	levelsCount int
	visible     bool
	ducking     duckingPref // read when the watcher starts

	// The viewer rule (05 §5.9).
	viewers   map[string]viewerTab     // the playing tabs by connection id
	families  map[string]*viewerFamily // the families with a playing tab on Windows
	overrides map[string]bool          // exe → the per-run viewer override ("Include" on a viewer row)

	// The rules: dirty since dirtySince; applied is what the engine has.
	dirty      bool
	dirtySince time.Time
	applied    media.Exclusions

	// Polling.
	report       media.AudioReport
	haveReport   bool
	seenSeq      uint32 // the Seq of report
	fetchPending bool   // ReportSeq changed (or the watcher started) and the report wasn't fetched yet
	lastPoll     time.Time
	lastFetch    time.Time
	lastPollErr  string // the last poll error logged, so a failing engine is logged once per error

	names    map[string]string // app id → display name seen in this run (AppRules)
	prompts  []Prompt          // pending, oldest first
	prompted map[string]bool   // app ids prompted in this run (once per app id per run)
	promptN  int

	fh        FriendsHear
	levelsSeq uint64
	alerted   map[string]time.Time // banner code → its last audio.alert in this share

	subs       map[int]func(Event)
	subN       int
	outbox     []Event
	delivering bool
}

// viewerTab is one tab that plays the user's isshoni viewer (05 §5.9 step 2).
type viewerTab struct{ family, os string }

// viewerFamily is a browser family kept out for a viewer: the exes found with an active session, and until when the
// reports are re-checked for more.
type viewerFamily struct {
	exes         []string
	recheckUntil time.Time
}

// New returns a Service over eng with the settings of st (05 §14.1, read once, normalized) and the app's SelfInfo.
// The zero Settings (nil AppRules and DismissedTips, which neither UnmarshalJSON nor DefaultSettings gives) is a
// store without an "audio" section: New reads it as DefaultSettings(), since 05 owns the defaults (03 §14.2).
// eng and st must not be nil. It starts nothing: Run polls, Close stops. log nil discards; the service logs with
// component=audio.
func New(eng media.AudioController, st SettingsStore, self SelfInfo, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	settings := st.AudioSettings()
	if settings.AppRules == nil && settings.DismissedTips == nil {
		settings = DefaultSettings()
	}
	base, cancel := context.WithCancel(context.Background())
	s := &Service{
		eng: eng, st: st, self: self, log: log.With("component", "audio"), ducking0: readDucking,
		base: base, cancel: cancel, wake: make(chan struct{}, 1),
		settings: settings.normalized(),
		viewers:  map[string]viewerTab{}, families: map[string]*viewerFamily{}, overrides: map[string]bool{},
		names: map[string]string{}, prompted: map[string]bool{}, subs: map[int]func(Event){},
	}
	s.fh = buildFriendsHear(s.fhInputLocked())
	s.fh.Seq = 1
	return s
}

// Run is the polling loop (05 §14.4) and applies the debounced rule changes (05 §14.3). While the watcher runs (the
// share sheet watches) or a share is live it reads AudioLevels every 100 ms with a level meter on screen and every
// 250 ms otherwise, fetches the report when its ReportSeq changes, and sends the changed rules; with nothing watched
// and no share it does neither. It returns nil when ctx ends or Close is called. Run can be called once.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("audio: Run called more than once")
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(s.base, stop)()
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		t.Stop()
		var due <-chan time.Time
		if d, ok := s.nextDue(time.Now()); ok {
			t.Reset(max(d, 0))
			due = t.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-due:
		}
		s.work(ctx)
	}
}

// nextDue is how long Run sleeps before its next work: the debounced rules or the next poll.
func (s *Service) nextDue(now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due time.Time
	if s.dirty && s.activeLocked() {
		due = s.dirtySince.Add(rulesDebounce)
	}
	if s.activeLocked() {
		next := s.lastPoll.Add(s.intervalLocked())
		if due.IsZero() || next.Before(due) {
			due = next
		}
	}
	if due.IsZero() {
		return 0, false
	}
	return due.Sub(now), true
}

// work applies the rules and polls when they are due.
func (s *Service) work(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	applyDue := s.dirty && s.activeLocked() && !now.Before(s.dirtySince.Add(rulesDebounce))
	pollDue := s.activeLocked() && !now.Before(s.lastPoll.Add(s.intervalLocked()))
	s.mu.Unlock()
	if applyDue {
		s.applyRules(ctx)
	}
	if pollDue {
		s.poll(ctx)
	}
}

// activeLocked reports whether the service polls (05 §14.4) and sends rule changes to the engine (05 §14.3): the
// watcher runs or a share is live. A live share counts without the watcher too (WatchAudio failed): the engine
// reports for a running share, and im_set_exclusions replaces its rules, or stores them for the share's start
// (05 §4.3).
func (s *Service) activeLocked() bool { return (s.watcherOn || s.live) && !s.closed }

// intervalLocked is the polling interval for the levels watch count (05 §14.4).
func (s *Service) intervalLocked() time.Duration {
	if s.levelsCount > 0 {
		return pollLevels
	}
	return pollIdle
}

// Close stops the engine's watcher (UnwatchAudio) and ends Run and the engine calls in flight. Afterwards Watch(true),
// SoundCheck and SetShareSound return ErrClosed and the rules reach the engine no more. Idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	defer s.cancel()

	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	on := s.watcherOn
	s.watcherOn = false
	s.mu.Unlock()
	if !on {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.base, callTimeout)
	defer cancel()
	if err := s.eng.UnwatchAudio(ctx); err != nil {
		s.log.Warn("audio watcher stop failed", errAttr(err))
		return fmt.Errorf("audio: unwatch: %w", err)
	}
	s.log.Info("audio watcher stopped")
	return nil
}

// isClosed reports whether Close was called.
func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// callCtx is the context of an engine call made by a method without one.
func (s *Service) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.base, callTimeout)
}

// kickLocked tells Run to re-plan.
func (s *Service) kickLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Subscribe registers fn for the service's events (05 §14.4): EventReport, EventLevels, EventPrompt, EventAlert. The
// returned cancel removes it; an event already being delivered may still reach fn.
func (s *Service) Subscribe(fn func(Event)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subN++
	id := s.subN
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// queueLocked queues ev for the subscribers; deliver sends it.
func (s *Service) queueLocked(ev Event) { s.outbox = append(s.outbox, ev) }

// deliver sends the queued events to the subscribers, in order, without holding mu. One caller delivers at a time;
// events queued meanwhile (also by a callback) go out with the same loop.
func (s *Service) deliver() {
	s.mu.Lock()
	if s.delivering {
		s.mu.Unlock()
		return
	}
	s.delivering = true
	for len(s.outbox) > 0 {
		evs := s.outbox
		s.outbox = nil
		subs := make([]func(Event), 0, len(s.subs))
		for _, id := range slices.Sorted(maps.Keys(s.subs)) {
			subs = append(subs, s.subs[id])
		}
		s.mu.Unlock()
		for _, ev := range evs {
			for _, fn := range subs {
				fn(ev)
			}
		}
		s.mu.Lock()
	}
	s.delivering = false
	s.mu.Unlock()
}

// markDirtyLocked records a change of the rules' inputs: Run applies it within 50 ms.
func (s *Service) markDirtyLocked(now time.Time) {
	if !s.dirty {
		s.dirty, s.dirtySince = true, now
	}
	s.kickLocked()
}

// exclusionsLocked builds the current rule set (05 §14.3).
func (s *Service) exclusionsLocked() media.Exclusions {
	return buildExclusions(ruleInput{self: s.self, settings: s.settings, sharedPID: s.sharedPID,
		viewerExes: s.viewerExesLocked()})
}

// applyRules sends a changed rule set to the engine (SetExclusions), unless neither the watcher runs nor a share is
// live: then the rules go with the next WatchAudio or AudioConfig.
func (s *Service) applyRules(ctx context.Context) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if !s.dirty || !s.activeLocked() {
		s.mu.Unlock()
		return
	}
	ex := s.exclusionsLocked()
	s.dirty = false
	if sameExclusions(ex, s.applied) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := s.eng.SetExclusions(cctx, ex); err != nil {
		s.log.Warn("audio rules not applied", errAttr(err))
		return
	}
	s.mu.Lock()
	s.applied = ex
	s.mu.Unlock()
	s.log.Debug("audio rules applied", "rules", len(ex.Rules))
}

// syncWatcher starts the engine's watcher when the sheet watches or a share is live, and stops it otherwise.
func (s *Service) syncWatcher() error {
	defer s.deliver()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	want := (s.sheet || s.live) && !s.closed
	if want == s.watcherOn {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := s.callCtx()
	defer cancel()
	if !want {
		s.watcherOn = false
		s.mu.Unlock()
		if err := s.eng.UnwatchAudio(ctx); err != nil {
			s.log.Warn("audio watcher stop failed", errAttr(err))
			return fmt.Errorf("audio: unwatch: %w", err)
		}
		s.log.Info("audio watcher stopped")
		return nil
	}
	ex := s.exclusionsLocked()
	s.mu.Unlock()
	if err := s.eng.WatchAudio(ctx, ex); err != nil {
		s.log.Warn("audio watcher start failed", errAttr(err))
		return fmt.Errorf("audio: watch: %w", err)
	}
	duck := s.ducking0()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watcherOn, s.applied, s.ducking = true, ex, duck
	s.dirty = !sameExclusions(s.exclusionsLocked(), ex)
	s.fetchPending, s.lastPoll, s.lastFetch = true, time.Time{}, time.Time{}
	for _, f := range s.families { // the reports start now: re-check them for the viewers' browsers
		f.recheckUntil = now.Add(viewerRecheck)
	}
	s.refreshLocked(now)
	s.kickLocked()
	s.log.Info("audio watcher started")
	return nil
}

// poll is one polling step (05 §14.4): the levels, and the report when its seq changed.
func (s *Service) poll(ctx context.Context) {
	defer s.deliver()
	s.mu.Lock()
	s.lastPoll = time.Now()
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	lv, err := s.eng.AudioLevels(cctx)
	s.mu.Lock()
	if !s.activeLocked() {
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.pollErrLocked(err)
		s.mu.Unlock()
		return
	}
	if s.levelsCount > 0 {
		s.levelsSeq++
		s.queueLocked(Event{Name: EventLevels, Payload: levelsOf(lv, s.levelsSeq)})
	}
	if !s.haveReport || lv.ReportSeq != s.seenSeq {
		s.fetchPending = true
	}
	now := time.Now()
	fetch := s.fetchPending && (s.lastFetch.IsZero() || now.Sub(s.lastFetch) >= minReportGap)
	if fetch {
		s.lastFetch = now
	}
	s.mu.Unlock()
	if !fetch {
		return
	}
	r, err := s.eng.AudioReport(cctx)
	s.mu.Lock()
	if !s.activeLocked() {
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.pollErrLocked(err)
		s.mu.Unlock()
		return
	}
	s.lastPollErr = ""
	pending := s.onReportLocked(r, time.Now())
	s.mu.Unlock()
	if pending {
		s.applyPendingMode()
	}
}

// pollErrLocked logs a polling error once per kind of error.
func (s *Service) pollErrLocked(err error) {
	a := errAttr(err)
	if a.Value.String() == s.lastPollErr {
		return
	}
	s.lastPollErr = a.Value.String()
	s.log.Debug("audio poll failed", a)
}

// onReportLocked takes a new engine report: the names for AppRules, the viewers' browsers, the prompts, then
// FriendsHear. It reports whether a Share sound change waits for the live session.
func (s *Service) onReportLocked(r media.AudioReport, now time.Time) bool {
	s.report, s.haveReport, s.seenSeq, s.fetchPending = r, true, r.Seq, false
	for _, a := range r.Apps {
		if a.Name != "" && (len(s.names) < maxNames || s.names[a.ID] != "") {
			s.names[a.ID] = a.Name
		}
	}
	s.recheckViewersLocked(now)
	newPrompts := s.updatePromptsLocked(now)
	before := s.fh.Seq
	s.refreshLocked(now)
	for _, p := range newPrompts {
		s.queueLocked(Event{Name: EventPrompt, Payload: p})
	}
	if s.fh.Seq != before {
		s.log.Debug("audio report", "engine", r.Engine, "state", r.State, "probe", r.Probe.Result, "live", r.Live,
			"heard", len(s.fh.Heard), "keptOut", len(s.fh.KeptOut), "notCaptured", len(s.fh.NotCaptured),
			"prompts", len(s.prompts), "banners", bannerCodes(s.fh.Banners))
	}
	return s.modePending && r.Live && s.live && s.withAudio
}

// bannerCodes lists the banners' codes for the log.
func bannerCodes(bs []Banner) string {
	codes := make([]string, len(bs))
	for i, b := range bs {
		codes[i] = b.Code
	}
	return strings.Join(codes, ",")
}

// updatePromptsLocked applies 05 §14.6 to a new report: a prompt for each app id kept out with reason mic and
// decision auto, once per app id per run; a pending prompt whose app no longer is kept out for its mic is gone. It
// returns the new prompts.
func (s *Service) updatePromptsLocked(now time.Time) []Prompt {
	s.prompts = slices.DeleteFunc(s.prompts, func(p Prompt) bool { return !s.micRowLocked(p.AppID) })
	if !s.settings.KeepOutMicUsers {
		s.prompts = nil
		return nil
	}
	viewer := s.viewerExesLocked()
	var added []Prompt
	for _, a := range s.report.Apps {
		exe, _ := exeOf(a.ID)
		if a.Verdict != verdictKeptOut || a.Reason != reasonMic || s.prompted[a.ID] || !validAppID(a.ID) ||
			lockedApp(a.ID) || s.settings.decision(a.ID) != DecisionAuto || slices.Contains(viewer, exe) {
			continue // a viewer rule will keep that browser out as a viewer: no question about its mic
		}
		s.prompted[a.ID] = true
		s.promptN++
		p := Prompt{ID: fmt.Sprintf("p%d", s.promptN), AppID: a.ID, Name: a.Name, Kind: PromptMic,
			Since: now.UnixMilli()}
		if a.Browser {
			p.Kind = PromptBrowserMic
		}
		s.prompts = append(s.prompts, p)
		added = append(added, p)
		s.log.Debug("audio prompt", "kind", p.Kind)
	}
	return added
}

// micRowLocked reports whether the report keeps the app id out for its mic.
func (s *Service) micRowLocked(id string) bool {
	return slices.ContainsFunc(s.report.Apps, func(a media.AudioApp) bool {
		return a.ID == id && a.Verdict == verdictKeptOut && a.Reason == reasonMic
	})
}

// fhInputLocked is FriendsHear's input now.
func (s *Service) fhInputLocked() fhInput {
	return fhInput{
		report: s.report, haveReport: s.haveReport, live: s.live, withAudio: s.withAudio, soundOn: s.soundOn,
		paused: s.paused, settings: s.settings, prompts: s.prompts, unknownViewer: s.unknownViewerLocked(),
		ducking: s.ducking,
	}
}

// refreshLocked rebuilds FriendsHear; a change goes up its Seq and is an audio.report, and an error banner that
// appeared during the live share is an audio.alert (05 §14.4).
func (s *Service) refreshLocked(now time.Time) {
	prev := s.fh
	fh := buildFriendsHear(s.fhInputLocked())
	fh.Seq = prev.Seq
	if reflect.DeepEqual(fh, prev) {
		return
	}
	fh.Seq++
	s.fh = fh
	s.queueLocked(Event{Name: EventReport, Payload: cloneFriendsHear(fh)})
	if bannerCodes(prev.Banners) != bannerCodes(fh.Banners) {
		s.log.Info("audio banners", "banners", bannerCodes(fh.Banners), "live", fh.Live)
	}
	if !fh.Live {
		return
	}
	for _, b := range fh.Banners {
		if !isErrorBanner(b) {
			continue
		}
		if prev.Live && slices.ContainsFunc(prev.Banners, func(p Banner) bool { return p.Code == b.Code }) {
			continue // not new
		}
		if last, ok := s.alerted[b.Code]; ok && now.Sub(last) < alertRepeat {
			continue
		}
		if s.alerted == nil {
			s.alerted = map[string]time.Time{}
		}
		s.alerted[b.Code] = now
		s.queueLocked(Event{Name: EventAlert, Payload: cloneBanner(b)})
		s.log.Info("audio alert", "banner", b.Code)
	}
}

// cloneFriendsHear returns a deep copy of fh.
func cloneFriendsHear(fh FriendsHear) FriendsHear {
	fh.Heard = slices.Clone(fh.Heard)
	fh.KeptOut = slices.Clone(fh.KeptOut)
	fh.NotCaptured = slices.Clone(fh.NotCaptured)
	fh.Prompts = slices.Clone(fh.Prompts)
	fh.Banners = slices.Clone(fh.Banners)
	for i := range fh.Banners {
		fh.Banners[i] = cloneBanner(fh.Banners[i])
	}
	fh.Tips = slices.Clone(fh.Tips)
	fh.VoiceApps = slices.Clone(fh.VoiceApps)
	return fh
}

// cloneBanner returns a copy of b that shares no map with it.
func cloneBanner(b Banner) Banner {
	b.Params = maps.Clone(b.Params)
	return b
}

// applyPendingMode sends a Share sound change that came before the share's audio session ran.
func (s *Service) applyPendingMode() {
	defer s.deliver()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if !s.modePending || !s.live || !s.withAudio || s.closed {
		s.modePending = false
		s.mu.Unlock()
		return
	}
	mode := modeFor(s.soundOn)
	s.mu.Unlock()
	ctx, cancel := s.callCtx()
	defer cancel()
	err := s.eng.SetAudioMode(ctx, mode)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errors.Is(err, media.ErrAudioNotRunning):
		return // still not running: the next live report tries again
	case err != nil:
		s.log.Warn("audio sound mode not applied", errAttr(err))
	}
	s.modePending = false
}

// modeFor is the engine mode of Share sound on or off (05 §6.5).
func modeFor(soundOn bool) media.AudioMode {
	if soundOn {
		return media.AudioExcludeApps
	}
	return media.AudioOff
}
