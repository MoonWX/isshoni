package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Hints, the codec switch, presets and pause (docs/m2/01-client-core.md D13–D15, §11.7's reconcile, §11.8):
//
//   - quality.hint for the share's current id: codec → §8.6 (the Publisher puts that profile first and offers again;
//     its answer has the send loop switch the engine with Session.Reconfigure{Profile}); encodings and maxBitrate →
//     ratectl.Hint, whose actions apply in order (apply). Hints for other ids are ignored, and the same values again
//     change nothing (a resume sends them again, M1 01 §8.10).
//   - ratectl.Tick every 500 ms while the share is live and not paused: the ramp and the guards.
//   - The codec switch's check (§8.6): an EventLayer of the share's main layer (f, or the first layer of a share
//     without one) after the Publisher's ProfileSource.SetProfile that reports another profile than the one asked
//     for, or a SetProfile that failed, has the core offer again with the profile the session runs. The ask is kept
//     per layer and remembers how many engine events the pump had read, so neither another layer's EventLayer (layers
//     may run different encoders, 04 §9.7) nor one of an earlier re-init answers it; and a profile the session
//     refused has the core offer again at most once per refusedHold, until a new codec hint, so the cycle answer →
//     refusal → re-offer doesn't repeat when the room's policy needs a profile the session can't switch to.
//   - Presets (D13): SetPreset, and the reconcile of a preset another connection changed: share.update{preset}, the
//     reply's ShareParams to ratectl.Params and Session.Reconfigure (the preset, and a layer's frame rate or size).
//   - Pause (D14): SetPaused, and the reconcile of a pause or resume another connection made. A pause drops media at
//     the pump first, then pauses (Session.SetPaused) and mutes (AudioController.SetAudioMuted) the engine, then sends
//     share.update{paused: true}; a resume unmutes first, un-pauses, asks for a keyframe per active layer and forwards
//     again (video from those keyframes), then sends share.update{paused: false}.
//   - After a republish (README WA38's recovery hands it over): the new ShareParams go to ratectl, the server's codec
//     policy goes first in the new Publisher's offers, and a paused share sends share.update{paused: true} again. The
//     preset goes again in the republish's share.start itself.
//
// room.state is the server's view, and the reconcile follows it, with one exception: a share.update of the core's
// own that the room.state has yet to show. The hub coalesces room.states (the one with the change may follow the
// reply by its coalescing interval), and the signaling client hands replies and notifications over on separate
// paths, so a room.state sent before the change can reach the share's actor after the reply. Until a room.state
// shows what the core asked for, one that disagrees is taken for an older one (for intentWindow after the reply), and
// a share.update{paused} that has no ok reply (signaling went down, the server refused it, a republish made a new
// share) is owed: the share's actor sends it again (resendPause: at most every resendEvery, and at its first look
// after a welcome) whatever room.state says, which is sent only when something changes. A pause is a safety
// control, and a resume the server never heard of would leave the SFU swallowing new viewers' keyframe requests.
// The share.update{paused} requests go out one at a time (pauseState.sending), each with the newest intent, so they
// reach the server in the order of the local transitions.

// The timers of this file.
const (
	// updateTimeout bounds a share.update the core sends from the share's actor.
	updateTimeout = 10 * time.Second
	// intentWindow: after the server answered a share.update of the core's own, a room.state that disagrees with it
	// within this long was sent before the update (see above). One after it is another connection's change.
	intentWindow = 5 * time.Second
	// resendEvery: a share.update{paused} the server never answered goes again at most this often.
	resendEvery = 2 * time.Second
	// engineCallTimeout bounds the AudioController calls of a pause or resume.
	engineCallTimeout = 2 * time.Second
	// refusedHold: after the session refused a profile (Session.Reconfigure), the core offers again with the profile
	// it runs; the same profile refused again within this long, without a codec hint between, offers nothing more.
	refusedHold = 10 * time.Second
)

// errEnded is what SetPaused and SetPreset return once the share has ended.
var errEnded = errors.New("core: the share has ended")

// serverView is what the newest room.state of the share's current id says of its pause.
type serverView uint8

const (
	viewUnknown serverView = iota // no room.state of the current id with status live or paused yet
	viewLive
	viewPaused
)

// viewOf is the serverView that shows paused.
func viewOf(paused bool) serverView {
	if paused {
		return viewPaused
	}
	return viewLive
}

// pauseState is a share's pause (D14). Its mutex serializes the transitions of SetPaused, which runs on its caller's
// goroutine so that the pump drops media at once whatever the actor is doing, with the reconcile's on the actor.
type pauseState struct {
	mu     sync.Mutex
	paused bool       // the pump drops media; the engine is paused and muted
	server serverView // the newest room.state of the share's current id
	// A pause or resume of the core's own that room.state has yet to show (pending): want is its value; owed says
	// share.update{paused: want} has no ok reply yet (signaling went down, the server refused it, or a republish made
	// a new share), so the actor sends it again (at most every resendEvery, triedAt); sentAt is when the ok reply
	// came.
	pending bool
	want    bool
	owed    bool
	sentAt  time.Time
	triedAt time.Time

	// sending is the one slot of share.update{paused}: held across each request (SetPaused's, the actor's resend), so
	// the requests reach the server one at a time, in the order of the local transitions. newShare makes it.
	sending chan struct{}
}

// presetState is a share's preset (D13). The actor's alone.
type presetState struct {
	applied protocol.Preset // the preset whose ShareParams apply; ShareSnapshot.Preset
	server  protocol.Preset // the newest room.state's, "" before one
	// want is a SetPreset the server answered (at wantAt) that room.state has yet to show; "" for none.
	want   protocol.Preset
	wantAt time.Time
}

// profileAsk is the engine profile the Publisher's send loop last asked the session for (consumer.SetProfile), per
// video layer, until the actor sees that layer's EventLayer of the re-init. after is how many engine events the pump
// had read when it was asked: an EventLayer the pump read before then is of an earlier re-init (a preset, a bitrate
// re-init, a resize), not of this one. The send loop sets it, the actor takes it.
type profileAsk struct {
	mu    sync.Mutex
	after uint64
	asked map[string]string // rid → the engine profile asked for
}

// set records that every layer of layers was asked for profile once the pump had read after events.
func (a *profileAsk) set(layers []string, profile string, after uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.after = after
	a.asked = make(map[string]string, len(layers))
	for _, l := range layers {
		a.asked[l] = profile
	}
}

// take returns the profile layer was asked for and forgets it, for an EventLayer of layer that was the pump's seq-th
// event: "" when the layer wasn't asked for, or the event is older than the ask.
func (a *profileAsk) take(layer string, seq uint64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq <= a.after {
		return ""
	}
	p := a.asked[layer]
	delete(a.asked, layer)
	return p
}

// clear forgets every layer's ask: the session refused it.
func (a *profileAsk) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = nil
}

// msgProfileFailed: the session refused the profile the send loop asked for (consumer.SetProfile).
type msgProfileFailed struct{ profile string }

// msgCall runs fn on the actor and sends its result to done (one slot).
type msgCall struct {
	fn   func() error
	done chan error
}

// call runs fn on the share's actor and returns its error: errEnded when the share ends first, ctx's error when ctx
// ends first (fn may then still run).
func (s *Share) call(ctx context.Context, fn func() error) error {
	select {
	case <-s.done:
		return errEnded
	default:
	}
	m := msgCall{fn: fn, done: make(chan error, 1)}
	s.post(m)
	select {
	case err := <-m.done:
		return err
	case <-s.done:
		return errEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- quality hints and rate control ----

// onHint applies a quality.hint for the share's current id (§11.8): its codec (§8.6), then its encodings and cap
// through ratectl.Hint. A hint for another id is ignored. The EndInfo is set when the codec switch's offer failed and
// the share must end.
func (s *Share) onHint(h protocol.QualityHint) *EndInfo {
	if h.ShareID != s.params.ShareID {
		return nil
	}
	s.log.Debug("quality hint", slog.String("reason", string(h.Reason)), slog.String("codec", string(h.Codec)),
		slog.Int64("max_bitrate", h.MaxBitrate), slog.Int("encodings", len(h.Encodings)))
	if h.Codec != "" {
		if info := s.onCodecHint(h.Codec); info != nil {
			return info
		}
	}
	now := time.Now()
	s.apply(s.ctl.Hint(h, now))
	s.syncRates(now)
	return nil
}

// rateTick is ratectl's Tick (every TickEvery) while the share is live and not paused (§11.8). The upload hint is
// read even when no Tick runs: it clears 10 s after its last Tick.
func (s *Share) rateTick(now time.Time) {
	if s.ctx.Err() != nil {
		return
	}
	if s.live && s.state() == StateLive && !s.isPaused() {
		s.apply(s.ctl.Tick(now))
	}
	s.syncRates(now)
}

// apply carries ratectl's actions out in order (§11.8): OpActive turns the layer on or off at the pump, in the
// engine (Session.SetLayerActive; a resume sets the layer's target first, in the same step) and in the Publisher;
// OpBitrate and OpAudioBitrate are Session.SetBitrate. The Publisher then paces for the new targets. An engine call
// that fails is logged: the share goes on with what the engine has, and a session that ended reports it through the
// pump.
func (s *Share) apply(acts []ratectl.Action) {
	if len(acts) == 0 {
		return
	}
	pub := s.publisher()
	for _, a := range acts {
		s.log.Debug("rate action", slog.String("op", a.Op.String()), slog.String("layer", a.Layer),
			slog.Int("bps", a.Bps), slog.Bool("active", a.Active), slog.Bool("reinit", a.Reinit))
		switch a.Op {
		case ratectl.OpActive:
			s.pump.setActive(a.Layer, a.Active)
			if a.Active && a.Bps > 0 {
				s.setBitrate(a.Layer, a.Bps)
			}
			if err := s.sess.SetLayerActive(a.Layer, a.Active); err != nil {
				s.log.Warn("engine layer active", slog.String("layer", a.Layer), slog.Bool("active", a.Active),
					logx.Err(err))
			}
			if pub != nil {
				if err := pub.SetActive(a.Layer, a.Active); err != nil {
					s.log.Warn("publisher layer active", slog.String("layer", a.Layer), logx.Err(err))
				}
			}
		case ratectl.OpBitrate, ratectl.OpAudioBitrate:
			s.setBitrate(a.Layer, a.Bps)
		}
	}
	if pub != nil {
		pub.SetPacingTargets(s.videoTargets())
	}
}

// setBitrate is Session.SetBitrate, logged when it fails.
func (s *Share) setBitrate(layer string, bps int) {
	if err := s.sess.SetBitrate(layer, bps); err != nil {
		s.log.Warn("engine bitrate", slog.String("layer", layer), slog.Int("bps", bps), logx.Err(err))
	}
}

// syncRates copies the rate controller's view into the snapshot: each layer's target and whether it is active, the
// upload hint and the CPU hint.
func (s *Share) syncRates(now time.Time) {
	st := s.ctl.State()
	upload, cpu := s.ctl.Upload(now), s.ctl.CPU(now)
	s.update(func(snap *ShareSnapshot) {
		for i := range snap.Layers {
			l := &snap.Layers[i]
			if bps, ok := st.Applied[l.RID]; ok {
				l.TargetBps = bps
			}
			if on, ok := st.Active[l.RID]; ok {
				l.Active = on
			}
		}
		snap.Upload, snap.Overloaded = upload, cpu
	})
}

// state returns the snapshot's state.
func (s *Share) state() ShareState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.State
}

// ---- the codec switch (§8.6) ----

// sentProfile is the profile the Publisher puts first: a codec hint's or the actual one, else what the session runs.
func (s *Share) sentProfile() string {
	if s.sendProfile != "" {
		return s.sendProfile
	}
	return s.running
}

// mainLayer is the layer whose EventLayer says which profile the session runs: f, or the first layer of a share
// without one. Layers may run different encoders (04 §9.7); the Publisher's one profile is f's.
func (s *Share) mainLayer() string {
	for _, e := range s.params.Encodings {
		if e.RID == protocol.RIDHigh {
			return e.RID
		}
	}
	if len(s.params.Encodings) > 0 {
		return s.params.Encodings[0].RID
	}
	return protocol.RIDHigh
}

// onCodecHint runs M1's path for quality.hint{codec} (§8.6): the Publisher puts the profile first and offers again
// (same gen, neg + 1); its answer has the send loop switch the engine. A hint naming the profile already sent needs
// nothing. Without a Publisher (before the first EventLayer, during a rebuild or a republish) the next one offers it
// first. The EndInfo is set when the offer failed and the share must end.
func (s *Share) onCodecHint(codec protocol.CodecKey) *EndInfo {
	key := codec.H264Profile()
	if key == "" || key == s.sentProfile() {
		return nil
	}
	pub := s.publisher()
	if pub != nil {
		if err := pub.SetProfile(key); err != nil {
			s.log.Warn("codec hint not applied", slog.String("codec", string(codec)), logx.Err(err))
			return nil
		}
	}
	s.log.Info("codec switch", slog.String("share_id", s.params.ShareID), slog.String("profile", key))
	s.sendProfile = key
	s.refusedProfile = "" // a new switch: a profile the session refused before may be asked for again
	if pub == nil {
		return nil
	}
	return s.reoffer()
}

// onLayerProfile follows the profile the session runs, from the main layer's EventLayers (any layer's until the main
// one has reported), and checks the switch the send loop asked for (consumer.SetProfile): when the main layer's
// EventLayer of that re-init (seq is its place among the engine events the pump read) reports another profile than
// the one asked for (a CB-only encoder asked for High stays CB, 04 §9.6), the core offers again with the profile the
// session runs. Another layer that stays in its profile only logs: the Publisher sends f's. The EndInfo is set when
// the offer failed and the share must end.
func (s *Share) onLayerProfile(ev media.Event, seq uint64) *EndInfo {
	main := ev.Layer == s.mainLayer()
	if ev.Profile != "" && (main || s.running == "") {
		s.running = ev.Profile
	}
	asked := s.ask.take(ev.Layer, seq)
	if asked == "" || ev.Profile == "" || publish.EngineProfile(asked) == ev.Profile {
		return nil
	}
	if !main {
		s.log.Debug("a layer runs another profile than asked for", slog.String("layer", ev.Layer),
			slog.String("asked", asked), slog.String("profile", ev.Profile))
		return nil
	}
	s.log.Info("the session runs another profile than asked for", slog.String("asked", asked),
		slog.String("profile", ev.Profile))
	return s.offerActual(ev.Profile)
}

// onProfileFailed: Session.Reconfigure refused the profile the send loop asked for, so the session goes on in the
// one it runs, and the core offers again with that one (§8.6). The same profile refused again within refusedHold,
// without a codec hint between, offers nothing more: the room's policy may need a profile that the session's encoder
// lacks while another encoder lists it, and each answer would ask for it again. The EndInfo is set when the offer
// failed and the share must end.
func (s *Share) onProfileFailed(m msgProfileFailed) *EndInfo {
	s.ask.clear()
	now := time.Now()
	if m.profile == s.refusedProfile && now.Sub(s.refusedProfileAt) < refusedHold {
		s.log.Info("the session refused the profile again", slog.String("asked", m.profile),
			slog.String("profile", s.running))
		return nil
	}
	s.refusedProfile, s.refusedProfileAt = m.profile, now
	s.log.Info("the session did not switch profiles", slog.String("asked", m.profile),
		slog.String("profile", s.running))
	if s.running == "" {
		return nil
	}
	return s.offerActual(s.running)
}

// offerActual puts the profile the session runs first and offers again. The EndInfo is set when the offer failed and
// the share must end.
func (s *Share) offerActual(profile string) *EndInfo {
	s.sendProfile = profile
	pub := s.publisher()
	if pub == nil {
		return nil
	}
	if err := pub.SetProfile(profile); err != nil {
		s.log.Warn("publisher profile", slog.String("profile", profile), logx.Err(err))
		return nil
	}
	s.log.Info("offering the profile the session runs", slog.String("profile", profile))
	return s.reoffer()
}

// reoffer asks for an offer on the current PC (same gen, neg + 1): now, or once the outstanding offer has its answer
// and signaling is ready (01 §9 rule 3: changes fold into one follow-up). Any offer carries the change, an ICE
// restart's included. The EndInfo is set when the offer failed and the share must end, as for every other offer.
func (s *Share) reoffer() *EndInfo {
	s.drv.reoffer = true
	return s.maybeReoffer()
}

// maybeReoffer makes the owed re-offer when it can go now; the EndInfo is set when the offer failed and the share
// must end.
func (s *Share) maybeReoffer() *EndInfo {
	if !s.drv.reoffer || s.drv.pending != nil || !s.rec.online || s.rec.republish || s.publisher() == nil ||
		s.ctx.Err() != nil {
		return nil
	}
	return s.offerOrEnd(false)
}

// ---- presets (D13) ----

// SetPreset changes the share's preset (§11.7): share.update{preset}, then the reply's ShareParams to the rate
// controller and Session.Reconfigure (the preset, and Text's lower frame rate on f). The preset already applied needs
// nothing. It returns the request's error (the preset is then unchanged), ErrNotReady while signaling is down, and an
// error once the share has ended.
func (s *Share) SetPreset(ctx context.Context, p protocol.Preset) error {
	if p == "" {
		p = protocol.PresetAuto
	}
	if !p.Valid() {
		return fmt.Errorf("core: Share.SetPreset: unknown preset %q", p)
	}
	err := s.call(ctx, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == s.ps.applied {
			return nil
		}
		if !s.rec.online {
			return ErrNotReady
		}
		params, err := s.updateShare(ctx, protocol.ShareUpdate{ShareID: s.params.ShareID, Preset: p})
		if err != nil {
			return err
		}
		s.applyPreset(p, params)
		if s.ps.server != p {
			s.ps.want, s.ps.wantAt = p, time.Now()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("core: Share.SetPreset: %w", err)
	}
	return nil
}

// updateShare sends a share.update from the actor and returns the reply's ShareParams. ctx and the actor's ctx bound
// it, and so does updateTimeout.
func (s *Share) updateShare(ctx context.Context, u protocol.ShareUpdate) (protocol.ShareParams, error) {
	rctx, cancel := context.WithTimeout(s.ctx, updateTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	logUpdate(s.log, u)
	var params protocol.ShareParams
	err := s.c.request(rctx, protocol.MessageTypeShareUpdate, u, &params)
	return params, err
}

// logUpdate logs a share.update the core sends (debug): its share and what it changes.
func logUpdate(log *slog.Logger, u protocol.ShareUpdate) {
	attrs := []any{slog.String("share_id", u.ShareID)}
	if u.Preset != "" {
		attrs = append(attrs, slog.String("preset", string(u.Preset)))
	}
	if u.Paused != nil {
		attrs = append(attrs, slog.Bool("paused", *u.Paused))
	}
	log.Debug("share update", attrs...)
}

// applyPreset applies a share.update reply for preset p: its encodings and Opus target go to the rate controller
// (whose actions apply), then Session.Reconfigure gets the preset and each layer whose frame rate or pixel budget
// changed (Text runs f at 30 fps, M1 02 §8.6).
func (s *Share) applyPreset(p protocol.Preset, params protocol.ShareParams) {
	now := time.Now()
	rc := media.Reconfig{Preset: string(p)}
	for _, e := range params.Encodings {
		i := slices.IndexFunc(s.params.Encodings, func(o protocol.Encoding) bool { return o.RID == e.RID })
		if i < 0 {
			continue
		}
		old, l := s.params.Encodings[i], media.LayerConfig{RID: e.RID}
		if e.MaxFramerate > 0 && e.MaxFramerate != old.MaxFramerate {
			l.MaxFPS = e.MaxFramerate
		}
		if e.MaxPixels > 0 && e.MaxPixels != old.MaxPixels {
			l.MaxPixels = e.MaxPixels
		}
		if l.MaxFPS > 0 || l.MaxPixels > 0 {
			rc.Layers = append(rc.Layers, l)
		}
	}
	if len(params.Encodings) > 0 {
		s.params.Encodings = slices.Clone(params.Encodings)
	}
	if params.AudioBitrate > 0 {
		s.params.AudioBitrate = params.AudioBitrate
	}
	s.apply(s.ctl.Params(params, now))
	if err := s.sess.Reconfigure(rc); err != nil {
		s.log.Warn("engine preset", slog.String("preset", string(p)), logx.Err(err))
	}
	s.ps.applied = p
	s.update(func(snap *ShareSnapshot) { snap.Preset = p })
	s.syncRates(now)
	s.log.Info("share preset", slog.String("share_id", s.params.ShareID), slog.String("preset", string(p)))
}

// reconcilePreset follows the preset of the share's own ShareInfo (D13): a preset that differs from the one applied
// was changed from another connection, and the core fetches its ShareParams with an idempotent share.update{preset}.
// A SetPreset that room.state has yet to show keeps the room.states that disagree from undoing it, for intentWindow.
func (s *Share) reconcilePreset(p protocol.Preset) {
	if p == "" {
		return
	}
	s.ps.server = p
	if w := s.ps.want; w != "" {
		if p == w || time.Since(s.ps.wantAt) < intentWindow {
			if p == w {
				s.ps.want = ""
			}
			return
		}
		s.ps.want = ""
	}
	if p == s.ps.applied || !p.Valid() {
		return
	}
	s.log.Info("the share's preset changed elsewhere", slog.String("share_id", s.params.ShareID),
		slog.String("preset", string(p)))
	params, err := s.updateShare(context.Background(), protocol.ShareUpdate{ShareID: s.params.ShareID, Preset: p})
	if err != nil {
		s.log.Info("share update for the preset", logx.Err(err))
		return
	}
	s.applyPreset(p, params)
}

// ---- pause (D14) ----

// SetPaused pauses or resumes the share (D14, §11.8). A pause drops video and audio at the pump at once (it covers
// the up to one frame interval before the engine applies it), then pauses (Session.SetPaused) and mutes
// (AudioController.SetAudioMuted) the engine, then sends share.update{paused: true}; keyframe requests stay
// swallowed. A resume unmutes the engine (10 ms fade-in) and un-pauses it, asks for a keyframe per active layer,
// forwards media again (video from those keyframes), then sends share.update{paused: false}. Without the server's
// share.pause feature only the local part happens (viewers see a frozen frame without the "Paused" label). "Share
// sound" off is 05's Service.SetShareSound(false).
//
// The local part always happens. A share.update that has no ok reply (signaling's drop cut it short, the server
// refused it, ctx ended first) is owed: the share's actor sends it again, at most every resendEvery and at its first
// look after signaling is back, until the server answers it ok or its room.state shows the pause. SetPaused returns
// nil when signaling's drop cut the update short; it returns the server's refusal (the local part stays) and ctx's
// error. It returns an error, and changes nothing, once the share has ended. Calls from several goroutines (03's
// hotkey and the pill) reach the server in the order of their local transitions: one share.update{paused} is on its
// way at a time, and a call whose transition a later one undid sends nothing (the later one sends its own).
func (s *Share) SetPaused(ctx context.Context, paused bool) error {
	select {
	case <-s.done:
		return fmt.Errorf("core: Share.SetPaused: %w", errEnded)
	default:
	}
	send := s.c.hasFeature(protocol.FeatureSharePause)
	s.pz.mu.Lock()
	s.pauseLocked(paused, false)
	if send && s.pz.server != viewOf(paused) {
		s.pz.pending, s.pz.want, s.pz.owed, s.pz.triedAt = true, paused, true, time.Now()
	} else {
		s.pz.pending = false
	}
	s.pz.mu.Unlock()
	if !send {
		return nil
	}
	err := s.lockSend(ctx)
	if err == nil {
		err = s.sendPausedLocked(ctx, paused)
		s.unlockSend()
	}
	switch {
	case err == nil, lostSignaling(err) && ctx.Err() == nil:
		return nil // owed: the share's actor sends it again once signaling is back
	case ctx.Err() != nil:
		return ctx.Err()
	}
	s.log.Info("share update for the pause", logx.Err(err))
	return fmt.Errorf("core: Share.SetPaused: %w", err)
}

// lockSend takes the one slot of share.update{paused} (pauseState.sending), waiting for the update on its way, or
// returns ctx's error.
func (s *Share) lockSend(ctx context.Context) error {
	select {
	case s.pz.sending <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryLockSend takes the slot if it is free.
func (s *Share) tryLockSend() bool {
	select {
	case s.pz.sending <- struct{}{}:
		return true
	default:
		return false
	}
}

// unlockSend gives the slot back.
func (s *Share) unlockSend() { <-s.pz.sending }

// sendPausedLocked is SetPaused's share.update{paused} once it holds the slot. A call whose transition a later call
// undid meanwhile sends nothing: the later one sends its own, after it.
func (s *Share) sendPausedLocked(ctx context.Context, paused bool) error {
	s.pz.mu.Lock()
	current := s.pz.paused == paused
	s.pz.mu.Unlock()
	if !current {
		return nil
	}
	return s.putPaused(ctx, s.snapID(), paused)
}

// putPaused sends share.update{paused} for share id, bounded by ctx and updateTimeout, and records an ok reply for
// the pause the core wants the server to show (for the share's current id: an older id's reply says nothing of the
// new share). The caller holds the slot (lockSend).
func (s *Share) putPaused(ctx context.Context, id string, paused bool) error {
	rctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	u := protocol.ShareUpdate{ShareID: id, Paused: &paused}
	logUpdate(s.log, u)
	var params protocol.ShareParams
	err := s.c.request(rctx, protocol.MessageTypeShareUpdate, u, &params)
	if err == nil {
		s.pz.mu.Lock()
		if s.pz.pending && s.pz.want == paused && s.pz.owed && id == s.snapID() {
			s.pz.owed, s.pz.sentAt = false, time.Now()
		}
		s.pz.mu.Unlock()
	}
	return err
}

// resendPause sends an owed share.update{paused} again (the actor's regular look, tick): at most every resendEvery,
// whatever room.state says, while signaling is ready and no republish is due (that one owes the update for the new
// id). When another update is on its way (SetPaused holds the slot), the next look sees what that one left owed: the
// actor never waits for the slot.
func (s *Share) resendPause(now time.Time) {
	if !s.rec.online || s.rec.republish || s.ctx.Err() != nil || !s.tryLockSend() {
		return
	}
	defer s.unlockSend()
	s.pz.mu.Lock()
	due := s.pz.pending && s.pz.owed && now.Sub(s.pz.triedAt) >= resendEvery
	want := s.pz.want
	if due {
		s.pz.triedAt = now
	}
	s.pz.mu.Unlock()
	if !due {
		return
	}
	if err := s.putPaused(s.ctx, s.params.ShareID, want); err != nil {
		s.log.Info("share update for the pause, again", logx.Err(err))
	}
}

// pauseOwedNow has an owed share.update{paused} go at the actor's next look: signaling is back.
func (s *Share) pauseOwedNow() {
	s.pz.mu.Lock()
	defer s.pz.mu.Unlock()
	if s.pz.owed {
		s.pz.triedAt = time.Time{}
	}
}

// pauseLocked runs the local part of a pause or a resume (D14) when the share isn't in that state yet; remote says
// that room.state asked for it (another connection of the user), for the log. s.pz.mu is held.
func (s *Share) pauseLocked(paused, remote bool) {
	if s.pz.paused == paused {
		return
	}
	ac, audio := s.c.o.Engine.(media.AudioController)
	audio = audio && s.req.Audio
	ctx, cancel := context.WithTimeout(context.Background(), engineCallTimeout)
	defer cancel()
	if paused {
		s.pump.setPaused(true) // from this moment nothing more leaves the PC
		if err := s.sess.SetPaused(true); err != nil {
			s.log.Debug("engine pause", logx.Err(err))
		}
		if audio {
			if err := ac.SetAudioMuted(ctx, true); err != nil {
				s.log.Debug("engine mute", logx.Err(err))
			}
		}
	} else {
		if audio {
			if err := ac.SetAudioMuted(ctx, false); err != nil {
				s.log.Debug("engine unmute", logx.Err(err))
			}
		}
		if err := s.sess.SetPaused(false); err != nil {
			s.log.Debug("engine resume", logx.Err(err))
		}
		s.pump.setPaused(false) // a keyframe request per active layer, and media flows again from them
	}
	s.pz.paused = paused
	s.update(func(snap *ShareSnapshot) { snap.Paused = paused })
	msg := "share resumed"
	if paused {
		msg = "share paused"
	}
	s.log.Info(msg, slog.String("share_id", s.snapID()), slog.Bool("elsewhere", remote))
}

// isPaused reports whether the share is paused.
func (s *Share) isPaused() bool {
	s.pz.mu.Lock()
	defer s.pz.mu.Unlock()
	return s.pz.paused
}

// snapID returns the share's current id from its snapshot (any goroutine).
func (s *Share) snapID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.ID
}

// reconcilePause follows the status of the share's own ShareInfo (§11.7, D14): paused while the share isn't pauses it
// locally, live while it is paused resumes it, the same way as SetPaused but without another share.update. A pause
// or resume of the core's own that room.state has yet to show keeps it from that: within intentWindow of the ok reply
// the room.state is an older one; a share.update that has no ok reply is owed, and the actor sends it again
// (resendPause). Other statuses (starting, stalled) say nothing of the pause. Without the server's share.pause
// feature the server never shows a pause, and the share's pause is the app's alone.
func (s *Share) reconcilePause(status protocol.ShareStatus) {
	var view serverView
	switch status {
	case protocol.ShareStatusLive:
		view = viewLive
	case protocol.ShareStatusPaused:
		view = viewPaused
	default:
		return
	}
	if !s.c.hasFeature(protocol.FeatureSharePause) {
		return
	}
	s.pz.mu.Lock()
	s.pz.server = view
	if s.pz.pending {
		switch {
		case viewOf(s.pz.want) == view:
			s.pz.pending = false
			s.pz.mu.Unlock()
			return
		case s.pz.owed:
			s.pz.mu.Unlock()
			return // the server hasn't heard of it yet: the actor sends it again (resendPause)
		case time.Since(s.pz.sentAt) < intentWindow:
			s.pz.mu.Unlock()
			return // a room.state from before the update
		}
		s.pz.pending = false // another connection changed it since
	}
	s.pauseLocked(view == viewPaused, true)
	s.pz.mu.Unlock()
}

// reconcileHints is the reconcile of hints.go for the share's own ShareInfo in a room.state: its pause and its
// preset (§11.7).
func (s *Share) reconcileHints(info protocol.ShareInfo) {
	s.reconcilePause(info.Status)
	s.reconcilePreset(info.Preset)
}

// ---- republish ----

// republished takes a republish's ShareParams (§11.7, README WA38's hand-over): the new share's encodings go to the
// rate controller (its layers are all active until the server says otherwise), the room's codec policy goes first in
// the new Publisher's offers, the server's views of the old id are forgotten, and a paused share owes
// share.update{paused: true} for the new id, which goes now (or, while a SetPaused's update is on its way, at the
// actor's next look): the server's new share doesn't have it. The preset went in share.start. s.params is the new
// share's.
func (s *Share) republished(params protocol.ShareParams) {
	now := time.Now()
	s.apply(s.ctl.Params(params, now))
	s.syncRates(now)
	if p := engineProfile(params.Codec, firstEncoder(s.probe, s.c.o.Encoder)); p != s.sentProfile() {
		s.sendProfile = p
	}
	s.ps.server, s.ps.want = "", ""
	feature := s.c.hasFeature(protocol.FeatureSharePause)
	s.pz.mu.Lock()
	s.pz.server = viewUnknown
	if s.pz.paused && feature {
		s.pz.pending, s.pz.want, s.pz.owed, s.pz.triedAt = true, true, true, time.Time{}
	} else {
		s.pz.pending = false
	}
	s.pz.mu.Unlock()
	s.resendPause(now)
}
