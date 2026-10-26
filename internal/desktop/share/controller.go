package share

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Controller is the share controller (03 §10.1). Its methods are safe for concurrent use.
type Controller struct {
	o    Options
	hold time.Duration
	log  *slog.Logger

	emitMu  sync.Mutex // one delivery at a time, so subscribers see the events in order
	subsMu  sync.Mutex
	subs    map[uint64]func(Event)
	nextSub uint64

	wg sync.WaitGroup // the shares' watchers and the stops Start makes

	mu         sync.Mutex // guards the fields below
	closed     bool
	picking    bool
	pick       *held
	cur        *active // the share being started or running; nil when none
	last       *View   // shown while cur is nil: the picker's view, or the last share's final one
	watchStats bool
	stateTimer *time.Timer // a coalesced share.state that is due
	lastState  time.Time   // when the last share.state went out
}

// held is the pick the controller holds (03 §10.1): one at a time, PickHold from the pick.
type held struct {
	id      string // the controller's pickId (16 hex), never the engine's source id
	src     media.Source
	confirm []Sheet
	expires time.Time
	timer   *time.Timer // releases it at expires
	inUse   bool        // a Start is using it
}

// active is the share being started or running.
type active struct {
	src       media.Source
	req       StartRequest
	sheets    []Sheet            // the pick's one-time sheets, recorded as shown once the share started
	cancel    context.CancelFunc // ends the start in progress
	cause     Cause              // the controller's own Stop's; "" until then
	audioOn   bool               // Share sound
	liveSince time.Time
	// reconnSince is when 01's state last turned reconnecting (zero while it isn't); reconnTimer refreshes the view
	// once that has lasted ReconnectingAfter.
	reconnSince time.Time
	reconnTimer *time.Timer
	client      Client // set once StartShare succeeded
	share       Share
	origin      string
	view        View          // the latest
	done        chan struct{} // closed once the share's end ran its local steps, or the start failed
}

// New returns a Controller. It starts nothing.
func New(o Options) *Controller {
	hold := o.PickHold
	if hold <= 0 {
		hold = PickHold
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Controller{o: o, hold: hold, log: log.With("component", "share"), subs: map[uint64]func(Event){}}
}

// Pick opens the OS picker owned by the main window, which is shown and focused first (03 §10.1), and holds the
// picked source under a new pickId for PickHold; a new Pick releases the pick held before. It returns nil for a
// picker the user closed (or that timed out: media.ErrCancelled), ErrAlreadySharing while a share is starting or
// running, and the engine's *media.Error otherwise: invalid with Reason self_window for an isshoni window (the
// engine has released it, and nothing is held), busy with pick_busy while the picker is already open,
// unsupported_os without an engine.
func (c *Controller) Pick(ctx context.Context) (*Picked, error) {
	if c.o.Engine == nil {
		return nil, &media.Error{Code: media.CodeUnsupportedOS, Op: "pick"}
	}
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return nil, ErrClosed
	case c.cur != nil:
		c.mu.Unlock()
		return nil, ErrAlreadySharing
	case c.picking: // as the engine answers (04 §5.7), without touching the picker that is open
		c.mu.Unlock()
		return nil, &media.Error{Code: media.CodeBusy, Op: "pick", Reason: "pick_busy"}
	}
	old := c.takePickLocked()
	c.picking = true
	c.last = &View{State: StatePicking}
	c.mu.Unlock()
	if old != nil {
		c.o.Engine.Release(old.src.ID)
	}
	c.stateChanged()

	var owner uintptr
	if w := c.o.Window; w != nil {
		w.ShowWindow(true)
		owner = w.MainWindowHandle()
	}
	src, err := c.o.Engine.Pick(ctx, media.PickRequest{OwnerWindow: owner})

	c.mu.Lock()
	c.picking = false
	if c.last != nil && c.last.State == StatePicking {
		c.last = nil
	}
	if err != nil {
		c.mu.Unlock()
		c.stateChanged()
		if errors.Is(err, media.ErrCancelled) {
			return nil, nil
		}
		return nil, err
	}
	if c.closed || c.cur != nil {
		c.mu.Unlock()
		c.o.Engine.Release(src.ID)
		return nil, ErrClosed
	}
	h := &held{id: newPickID(), src: src, confirm: c.sheets(src), expires: time.Now().Add(c.hold)}
	h.timer = time.AfterFunc(c.hold, func() { c.expire(h) })
	c.pick = h
	c.mu.Unlock()
	c.stateChanged()
	c.log.Info("share: source picked", "kind", string(kindOf(src.Kind)))
	return &Picked{PickID: h.id, Kind: kindOf(src.Kind), Label: src.Name, Border: src.OSBorder,
		Confirm: slices.Clone(h.confirm)}, nil
}

// sheets lists the one-time sheets due before a share of src (03 §10.2): first-share once per install, dnd for the
// first share of kind screen.
func (c *Controller) sheets(src media.Source) []Sheet {
	out := []Sheet{}
	if c.o.Settings == nil {
		return out
	}
	tips := c.o.Settings.Get().Tips
	if !tips.FirstShareShown {
		out = append(out, SheetFirstShare)
	}
	if kindOf(src.Kind) == protocol.ShareKindScreen && !tips.DNDShown {
		out = append(out, SheetDND)
	}
	return out
}

// newPickID returns 16 random hex characters.
func newPickID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// takePickLocked drops the held pick and returns it (nil when none), for the caller to release outside the lock.
func (c *Controller) takePickLocked() *held {
	h := c.pick
	if h == nil {
		return nil
	}
	c.pick = nil
	h.timer.Stop()
	return h
}

// expire releases a pick that reached PickHold without a Start.
func (c *Controller) expire(h *held) {
	c.mu.Lock()
	if c.pick != h || h.inUse {
		c.mu.Unlock()
		return
	}
	c.pick = nil
	c.mu.Unlock()
	c.o.Engine.Release(h.src.ID)
	c.log.Debug("share: the pick expired unused")
}

// Release drops the held pick pickID: the user backed out (the first-share sheet's Cancel, the share sheet closed).
// Any other id, and a pick a Start is using, are left alone.
func (c *Controller) Release(pickID string) {
	c.mu.Lock()
	h := c.pick
	if h == nil || h.id != pickID || h.inUse {
		c.mu.Unlock()
		return
	}
	c.takePickLocked()
	c.mu.Unlock()
	c.o.Engine.Release(h.src.ID)
}

// Start starts a share of the held pick r.PickID in r.RoomID (03 §10.1), in order: C2 online (at most ReadyTimeout,
// else ErrPublisherOffline; not linked: 02's ErrNotLinked at once) → 05's OnShareStarted(src.PID, withAudio) → 01's
// StartShare(RoomID, Source, Preset, Audio, Label "") with StartShareTimeout, which joins the room itself. It returns
// the view at starting (live comes as an EventState). Errors: ErrPickExpired for a pickId it doesn't hold,
// ErrAlreadySharing, ErrPublisherOffline, 02's errors, and StartShare's (*protocol.Error such as room_not_found,
// room_full, forbidden, share_limit; *media.Error from the engine's start). On an error 05's OnShareStopped follows
// the OnShareStarted, the view is failed, and the pick stays held for another Start, unless the engine said it is
// gone (source_gone, pick_expired). A Stop during the start aborts it (01 undoes its steps) and ends the share with
// the Stop's cause.
func (c *Controller) Start(ctx context.Context, r StartRequest) (View, error) {
	if r.Preset == "" {
		r.Preset = protocol.PresetAuto
	}
	if !r.Preset.Valid() || r.RoomID == "" {
		return View{}, ErrInvalid
	}
	c.mu.Lock()
	h := c.pick
	switch {
	case c.closed:
		c.mu.Unlock()
		return View{}, ErrClosed
	case c.cur != nil:
		c.mu.Unlock()
		return View{}, ErrAlreadySharing
	case h == nil || h.id != r.PickID:
		c.mu.Unlock()
		return View{}, ErrPickExpired
	case !time.Now().Before(h.expires):
		c.takePickLocked()
		c.mu.Unlock()
		c.o.Engine.Release(h.src.ID)
		return View{}, ErrPickExpired
	}
	h.inUse = true
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := &active{src: h.src, req: r, sheets: h.confirm, cancel: cancel, audioOn: r.WithAudio,
		done: make(chan struct{})}
	a.view = c.viewLocked(a, nil, "")
	c.cur, c.last = a, nil
	c.mu.Unlock()
	c.stateChanged()

	client, origin, err := c.ready(sctx)
	if err != nil {
		return c.startFailed(a, h, err)
	}
	if au := c.o.Audio; au != nil {
		au.OnShareStarted(pidOf(a.src.PID), r.WithAudio) // the shared app is never auto-excluded (PLAN)
	}
	events, unsubscribe := client.Subscribe() // before StartShare, so no EventShare of the new share is missed
	stctx, stcancel := context.WithTimeout(sctx, StartShareTimeout)
	sh, err := client.StartShare(stctx, core.ShareRequest{RoomID: r.RoomID, Source: a.src, Preset: r.Preset,
		Audio: r.WithAudio, Label: ""})
	stcancel()
	if err != nil {
		unsubscribe()
		if au := c.o.Audio; au != nil {
			au.OnShareStopped()
		}
		return c.startFailed(a, h, startError(err))
	}

	snap := sh.Snapshot()
	conn := client.Status().ConnectionID
	c.mu.Lock()
	if c.pick == h {
		c.pick = nil // Engine.Start consumed it
		h.timer.Stop()
	}
	a.client, a.share, a.origin = client, sh, origin
	if c.closed { // Close came while StartShare ran: this share ends at once, and Close waits for no watcher of it
		c.mu.Unlock()
		c.abandon(a, unsubscribe)
		return View{}, ErrClosed
	}
	stopNow := a.cause != ""
	watch := c.watchStats
	a.view = c.viewLocked(a, &snap, conn)
	v := *cloneView(a.view)
	c.wg.Add(1) // under c.mu, which Close holds when it sets closed: no Add after Close's Wait began
	go c.watch(a, events, unsubscribe)
	if stopNow { // a Stop came while StartShare returned: it waits for this share's end
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CloseTimeout) // outlives Start's ctx
			defer cancel()
			_ = sh.Stop(ctx)
		}()
	}
	c.mu.Unlock()
	if watch {
		client.WatchServerStats(true)
	}
	c.remember(origin, r.Preset, a.sheets)
	c.stateChanged()
	c.log.Info("share: started", "share_id", snap.ID, "kind", string(v.Kind), "preset", string(r.Preset),
		"audio", r.WithAudio)
	return v, nil
}

// pidOf is a source's PID for 05 (0 for a monitor or an unknown process).
func pidOf(pid int) uint32 {
	if pid <= 0 || pid > math.MaxUint32 {
		return 0
	}
	return uint32(pid) //nolint:gosec // checked above
}

// abandon stops a share whose start finished after Close, and runs its end's local steps here.
func (c *Controller) abandon(a *active, unsubscribe func()) {
	ctx, cancel := context.WithTimeout(context.Background(), CloseTimeout)
	defer cancel()
	_ = a.share.Stop(ctx)
	unsubscribe() // the core never blocks on a subscriber: nothing to drain
	if au := c.o.Audio; au != nil {
		au.OnShareStopped()
	}
	c.mu.Lock()
	if c.cur == a {
		c.cur = nil
	}
	close(a.done)
	c.mu.Unlock()
}

// ready waits for C2 (03 §9):ErrPublisherOffline when it isn't online within ReadyTimeout or by ctx's deadline.
func (c *Controller) ready(ctx context.Context) (Client, string, error) {
	if c.o.Publisher == nil {
		return nil, "", ErrPublisherOffline
	}
	rctx, cancel := context.WithTimeout(ctx, ReadyTimeout)
	defer cancel()
	client, origin, err := c.o.Publisher.Ready(rctx)
	switch {
	case err == nil:
		return client, origin, nil
	case errors.Is(err, context.DeadlineExceeded):
		return nil, "", fmt.Errorf("%w: %w", ErrPublisherOffline, err)
	}
	return nil, "", err
}

// startError maps StartShare's own errors to the controller's: a share the core already has is ErrAlreadySharing,
// a core that isn't online (or closed meanwhile: a server switch) ErrPublisherOffline.
func startError(err error) error {
	switch {
	case errors.Is(err, core.ErrBusy):
		return fmt.Errorf("%w: %w", ErrAlreadySharing, err)
	case errors.Is(err, core.ErrNotReady), errors.Is(err, core.ErrClosed):
		return fmt.Errorf("%w: %w", ErrPublisherOffline, err)
	}
	return err
}

// startFailed ends a start that didn't reach StartShare's success: failed with err, or ended with the cause of a
// Stop that aborted it. The pick stays held for another Start unless the engine said it is gone or a Stop came.
func (c *Controller) startFailed(a *active, h *held, err error) (View, error) {
	c.mu.Lock()
	stopped := a.cause != ""
	if c.cur == a {
		c.cur = nil
	}
	var release *held
	if c.pick == h {
		h.inUse = false
		h.timer.Stop()
		if stopped || errors.Is(err, media.ErrSourceGone) || !time.Now().Before(h.expires) {
			c.pick = nil
			release = h
		} else {
			h.timer = time.AfterFunc(time.Until(h.expires), func() { c.expire(h) })
		}
	}
	v := a.view
	if stopped {
		v.State, v.EndReason, v.Err = StateEnded, causeReason(a.cause), nil
	} else {
		v.State, v.EndReason, v.Err = StateFailed, EndError, err
	}
	a.view = v
	c.last = cloneView(v)
	close(a.done)
	c.mu.Unlock()
	if release != nil {
		c.o.Engine.Release(release.src.ID)
	}
	c.stateChanged()
	c.log.Info("share: start failed", "err", redact(err), "stopped", stopped)
	return View{}, err
}

// remember records what a started share settles (03 §10.1, §10.2): serverPrefs[origin].preset, and the one-time
// sheets that were shown before it.
func (c *Controller) remember(origin string, preset protocol.Preset, shown []Sheet) {
	if c.o.Settings == nil {
		return
	}
	err := c.o.Settings.Update(func(s *settings.Settings) {
		if origin != "" {
			p := s.ServerPrefs[origin]
			p.Preset = preset
			s.ServerPrefs[origin] = p
		}
		for _, sh := range shown {
			switch sh {
			case SheetFirstShare:
				s.Tips.FirstShareShown = true
			case SheetDND:
				s.Tips.DNDShown = true
			}
		}
	})
	if err != nil {
		c.log.Warn("share: the preset and tips weren't saved", "err", err)
	}
}

// watch follows a running share until it ends: its snapshots and C2's status (EventShare, EventStatus), 01's
// notices, the stats every StatsEvery while live, then the end's local steps.
func (c *Controller) watch(a *active, events <-chan core.Event, unsubscribe func()) {
	defer c.wg.Done()
	defer unsubscribe()
	stats := time.NewTicker(StatsEvery)
	defer stats.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch ev.Kind {
			case core.EventNotice:
				c.notice(ev.Notice)
			case core.EventShare, core.EventStatus, core.EventResync:
				c.refresh(a)
			}
		case <-stats.C:
			c.stats(a)
		case <-a.share.Done():
			c.ended(a)
			return
		}
	}
}

// refresh rebuilds a's view from its share's snapshot and C2's status.
func (c *Controller) refresh(a *active) {
	snap := a.share.Snapshot()
	conn := a.client.Status().ConnectionID
	c.mu.Lock()
	if c.cur != a {
		c.mu.Unlock()
		return
	}
	a.view = c.viewLocked(a, &snap, conn)
	c.mu.Unlock()
	c.stateChanged()
}

// ended runs the local steps of every share end once (03 §10.1), also of one the server or the core made: 05's
// OnShareStopped (then, with their slices, the hotkeys unregistered, the pill hidden, keep-awake off and the tray
// idle), and the final view.
func (c *Controller) ended(a *active) {
	snap := a.share.Snapshot()
	conn := a.client.Status().ConnectionID
	if au := c.o.Audio; au != nil {
		au.OnShareStopped()
	}
	c.mu.Lock()
	a.view = c.viewLocked(a, &snap, conn) // ended: also stops a reconnecting timer
	if c.cur == a {
		c.cur = nil
	}
	c.last = cloneView(a.view)
	reason := a.view.EndReason
	close(a.done)
	c.mu.Unlock()
	c.stateChanged()
	c.log.Info("share: ended", "share_id", snap.ID, "end_reason", string(reason))
}

// notice passes 01's EventNotice on (03 §10.5): encoder_fallback becomes share.notice; audio_rebuilt is logged only
// (05's banners show the audio state).
func (c *Controller) notice(n *core.Notice) {
	if n == nil {
		return
	}
	if n.Code == "audio_rebuilt" {
		c.log.Info("share: the audio capture was rebuilt")
		return
	}
	params := make(map[string]string, len(n.Params))
	for k, v := range n.Params {
		params[k] = v
	}
	c.publish(Event{Kind: EventNotice, Notice: &Notice{Code: n.Code, Params: params}})
}

// stats sends a share.stats sample while the share is live (or paused).
func (c *Controller) stats(a *active) {
	c.mu.Lock()
	live := c.cur == a && (a.view.State == StateLive || a.view.State == StatePaused)
	c.mu.Unlock()
	if !live {
		return
	}
	snap := a.share.Snapshot()
	c.publish(Event{Kind: EventStats, Stats: statsOf(&snap)})
}

// viewLocked builds a's view: the controller's fields, then 01's snapshot (nil while starting) through mapping.go's
// tables. It stamps liveSince the first time it sees live, and keeps it across a republish (replaces), whose new
// share id is the same share to the user. A reconnecting shorter than ReconnectingAfter keeps the state shown before.
func (c *Controller) viewLocked(a *active, snap *core.ShareSnapshot, connID string) View {
	shown := a.view.State
	v := View{
		State: StateStarting, ConnectionID: connID, RoomID: a.req.RoomID, Kind: kindOf(a.src.Kind),
		Label: a.src.Name, Preset: a.req.Preset, WithAudio: a.req.WithAudio, AudioOn: a.req.WithAudio && a.audioOn,
		Border: a.src.OSBorder,
	}
	if snap != nil {
		v.ShareID = snap.ID
		if snap.RoomID != "" {
			v.RoomID = snap.RoomID
		}
		if snap.Preset != "" {
			v.Preset = snap.Preset
		}
		v.State = stateOf(snap)
		if snap.State == core.StateLive && a.liveSince.IsZero() {
			a.liveSince = time.Now()
		}
		if !a.liveSince.IsZero() {
			v.Border = !snap.NoOSIndicator // the engine's report replaces the pick's once the share is live
		}
		v.Minimized = snap.Minimized
		v.Watching = len(snap.Watchers)
		v.Encoder, v.HW = snap.Encoder, snap.HW
		v.Hint = hintOf(snap)
		v.Layers = layersOf(snap.Layers)
		if c.watchStats {
			v.Server = slices.Clone(snap.Server)
		}
		if snap.State == core.StateEnded {
			v.State, v.EndReason, v.Err = endOf(snap.End, a.cause)
		}
		v.State = c.debounceLocked(a, v.State, shown)
	}
	v.LiveSince = a.liveSince
	if a.cause != "" && !v.State.final() {
		v.State = StateStopping
	}
	v.Pill = v.State.active() && !v.Border
	return v
}

// debounceLocked is the state to show for state (stateOf's) when shown was shown before: reconnecting only once it
// has lasted ReconnectingAfter, until then the state shown before (live, paused, starting, unreachable). A timer
// refreshes the view when the gap is over.
func (c *Controller) debounceLocked(a *active, state, shown State) State {
	if state != StateReconnecting {
		a.reconnSince = time.Time{}
		if a.reconnTimer != nil {
			a.reconnTimer.Stop()
			a.reconnTimer = nil
		}
		return state
	}
	now := time.Now()
	if a.reconnSince.IsZero() {
		a.reconnSince = now
	}
	left := ReconnectingAfter - now.Sub(a.reconnSince)
	if left <= 0 || shown == StateReconnecting || shown == StateStopping || shown.final() || shown == "" {
		return StateReconnecting
	}
	if a.reconnTimer == nil {
		a.reconnTimer = time.AfterFunc(left, func() {
			c.mu.Lock()
			a.reconnTimer = nil
			c.mu.Unlock()
			c.refresh(a)
		})
	}
	return shown
}

// Current returns the share's view: the share being started or running, the picker's while it is open, else the
// last share's final view (ended or failed) until the next Pick; nil before any.
func (c *Controller) Current() *View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentLocked()
}

func (c *Controller) currentLocked() *View {
	switch {
	case c.cur != nil:
		return cloneView(c.cur.view)
	case c.last != nil:
		return cloneView(*c.last)
	}
	return nil
}

// Live reports a share being started or running, and its room (03 §13.2's live share, §11.6's "while live"): the
// app's Live.
func (c *Controller) Live() (roomID string, live bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return "", false
	}
	return c.cur.view.RoomID, true
}

// Stop stops the share, idempotent and ordered (03 §10.1): 01's Share.Stop (share.stop, pc.close, the Publisher
// closed, the engine stopped, room.leave) and then the end's local steps (05's OnShareStopped, …). cause becomes
// the end reason ("" is user). A share still starting is aborted. ctx bounds the server's replies; the local part
// always runs, and Stop returns once it ran or ctx ended.
func (c *Controller) Stop(ctx context.Context, cause Cause) {
	if cause == "" {
		cause = CauseUser
	}
	c.mu.Lock()
	a := c.cur
	if a == nil {
		c.mu.Unlock()
		return
	}
	if a.cause == "" {
		a.cause = cause
	}
	sh := a.share
	if sh == nil {
		a.cancel() // the start in progress ends; 01 undoes what it did
	}
	if !a.view.State.final() {
		a.view.State = StateStopping
		a.view.Pill = false
	}
	c.mu.Unlock()
	c.stateChanged()
	if sh != nil {
		_ = sh.Stop(ctx) // returns once the share ended; its error is ctx's
	}
	select {
	case <-a.done:
	case <-ctx.Done():
	}
}

// running returns the running share (StartShare succeeded, its end not run yet), or ErrNotSharing.
func (c *Controller) running() (*active, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || c.cur.share == nil {
		return nil, ErrNotSharing
	}
	return c.cur, nil
}

// shareError maps an error of a running share's call: the share ended meanwhile is ErrNotSharing, signaling down
// ErrPublisherOffline.
func shareError(a *active, err error) error {
	select {
	case <-a.share.Done():
		return fmt.Errorf("%w: %w", ErrNotSharing, err)
	default:
	}
	if errors.Is(err, core.ErrNotReady) {
		return fmt.Errorf("%w: %w", ErrPublisherOffline, err)
	}
	return err
}

// SetPaused pauses or resumes the share (03 §11.3): 01's Share.SetPaused (the pump drops media at once, then the
// engine freezes and mutes, then share.update{paused}), then, once it succeeded, 05's SetPaused (its extra mute).
// ErrNotSharing without a running share; ErrFeatureDisabled when the server lacks share.pause (nothing changes);
// 01's errors (a *protocol.Error refusal, ctx's) otherwise.
func (c *Controller) SetPaused(ctx context.Context, paused bool) error {
	a, err := c.running()
	if err != nil {
		return err
	}
	if !slices.Contains(a.client.Status().Features, protocol.FeatureSharePause) {
		return ErrFeatureDisabled
	}
	if err := a.share.SetPaused(ctx, paused); err != nil {
		return shareError(a, err)
	}
	if au := c.o.Audio; au != nil {
		au.SetPaused(paused)
	}
	c.refresh(a)
	return nil
}

// SetPreset changes the running share's preset (03 §10.6): 01's Share.SetPreset sends share.update{preset} on C2.
// A preset that succeeded becomes serverPrefs[origin].preset. ErrNotSharing, ErrInvalid for an unknown preset,
// ErrPublisherOffline while C2's signaling is down, and the server's *protocol.Error.
func (c *Controller) SetPreset(ctx context.Context, p protocol.Preset) error {
	if p == "" {
		p = protocol.PresetAuto
	}
	if !p.Valid() {
		return ErrInvalid
	}
	a, err := c.running()
	if err != nil {
		return err
	}
	if err := a.share.SetPreset(ctx, p); err != nil {
		return shareError(a, err)
	}
	c.mu.Lock()
	a.req.Preset = p
	c.mu.Unlock()
	c.remember(a.origin, p, nil)
	c.refresh(a)
	return nil
}

// SetAudioEnabled is the running share's Share sound switch (03 §10.3): only 05's SetShareSound, no wire message. A
// share started without sound has no switch: nothing happens. ErrNotSharing without a running share; 05's error.
func (c *Controller) SetAudioEnabled(on bool) error {
	a, err := c.running()
	if err != nil {
		return err
	}
	if !a.req.WithAudio {
		return nil
	}
	if au := c.o.Audio; au != nil {
		if err := au.SetShareSound(on); err != nil {
			return err
		}
	}
	c.mu.Lock()
	a.audioOn = on
	c.mu.Unlock()
	c.refresh(a)
	return nil
}

// StopOther stops another connection's share of this user through C2 (01's Client.StopOther; the SPA's viewer
// connection may not, 01 §6.3): C2 online within ReadyTimeout (else ErrPublisherOffline), then share.stop. It
// returns once room.state no longer lists the share; ErrBusy while C2 shares in another room; the server's
// *protocol.Error.
func (c *Controller) StopOther(ctx context.Context, roomID, shareID string) error {
	client, _, err := c.ready(ctx)
	if err != nil {
		return err
	}
	err = client.StopOther(ctx, roomID, shareID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrBusy):
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case errors.Is(err, core.ErrNotReady), errors.Is(err, core.ErrClosed):
		return fmt.Errorf("%w: %w", ErrPublisherOffline, err)
	}
	return err
}

// Stats returns the running share's stats sample, nil unless it is live or paused.
func (c *Controller) Stats() *Stats {
	c.mu.Lock()
	a := c.cur
	if a == nil || a.share == nil || (a.view.State != StateLive && a.view.State != StatePaused) {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	snap := a.share.Snapshot()
	return statsOf(&snap)
}

// WatchServerStats turns 01's server stats on or off (the Details disclosure, 03 §10.3; the bridge counts the
// disclosures). The wish holds for the running share and for every later one; View.Server is set only while on.
func (c *Controller) WatchServerStats(on bool) {
	c.mu.Lock()
	c.watchStats = on
	a := c.cur
	c.mu.Unlock()
	if a == nil || a.share == nil {
		return
	}
	a.client.WatchServerStats(on)
	c.refresh(a)
}

// Subscribe calls fn with every Event until cancel is called. fn runs on the controller's goroutines, one call at
// a time and in order; it must not block for long, and may call only Current, Live and Stats of the controller (the
// others may deliver an event themselves) and never Subscribe's cancel.
func (c *Controller) Subscribe(fn func(Event)) (cancel func()) {
	c.subsMu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = fn
	c.subsMu.Unlock()
	return func() {
		c.subsMu.Lock()
		delete(c.subs, id)
		c.subsMu.Unlock()
	}
}

// stateChanged sends the current view as an EventState, at most every StateEvery: a change within StateEvery of the
// last event goes out when the gap is over, together with every change made meanwhile.
func (c *Controller) stateChanged() {
	c.mu.Lock()
	if c.stateTimer != nil {
		c.mu.Unlock()
		return
	}
	if wait := StateEvery - time.Since(c.lastState); !c.lastState.IsZero() && wait > 0 {
		c.stateTimer = time.AfterFunc(wait, c.fireState)
		c.mu.Unlock()
		return
	}
	c.lastState = time.Now()
	c.mu.Unlock()
	c.deliverState()
}

// fireState is the coalesced share.state that came due.
func (c *Controller) fireState() {
	c.mu.Lock()
	c.stateTimer = nil
	c.lastState = time.Now()
	c.mu.Unlock()
	c.deliverState()
}

// deliverState sends the view as it is at the delivery.
func (c *Controller) deliverState() {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.mu.Lock()
	v := c.currentLocked()
	c.mu.Unlock()
	c.deliver(Event{Kind: EventState, View: v})
}

// publish sends an event at once.
func (c *Controller) publish(ev Event) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.deliver(ev)
}

// deliver calls every subscriber; c.emitMu is held.
func (c *Controller) deliver(ev Event) {
	c.subsMu.Lock()
	fns := make([]func(Event), 0, len(c.subs))
	for _, fn := range c.subs {
		fns = append(fns, fn)
	}
	c.subsMu.Unlock()
	for _, fn := range fns {
		fn(copyEvent(ev))
	}
}

// copyEvent gives each subscriber its own copies.
func copyEvent(ev Event) Event {
	if ev.View != nil {
		ev.View = cloneView(*ev.View)
	}
	if ev.Stats != nil {
		s := *ev.Stats
		s.Layers = slices.Clone(s.Layers)
		ev.Stats = &s
	}
	if ev.Notice != nil {
		n := *ev.Notice
		n.Params = make(map[string]string, len(ev.Notice.Params))
		for k, v := range ev.Notice.Params {
			n.Params[k] = v
		}
		ev.Notice = &n
	}
	return ev
}

// Close ends the controller at the app's end: the held pick is released, a share still running is stopped
// (CloseTimeout), and Close returns once the shares' watchers have returned. Pick and Start fail with ErrClosed
// afterwards; a Start that was past its wait ends its share at once.
func (c *Controller) Close() {
	c.mu.Lock()
	c.closed = true
	h := c.takePickLocked()
	c.mu.Unlock()
	if h != nil {
		c.o.Engine.Release(h.src.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), CloseTimeout)
	defer cancel()
	c.Stop(ctx, CauseUser)
	c.wg.Wait()
	c.mu.Lock()
	if c.stateTimer != nil {
		c.stateTimer.Stop()
		c.stateTimer = nil
	}
	c.mu.Unlock()
}

// redact is an error for the log: a media error's Redacted form (no detail), a protocol error's code, else the
// name of a known error, else "error". No error text that could hold a name, a path or a server's answer reaches the
// log (03 §17).
func redact(err error) string {
	var (
		me *media.Error
		pe *protocol.Error
	)
	switch {
	case errors.As(err, &me):
		return me.Redacted()
	case errors.As(err, &pe):
		return string(pe.Code)
	}
	for _, known := range []error{ErrPublisherOffline, ErrAlreadySharing, ErrPickExpired, ErrClosed, ErrInvalid,
		deviceauth.ErrNotLinked, deviceauth.ErrIncompatible, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "error"
}
