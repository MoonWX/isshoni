package fake

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// Session is a running fake capture and encode (docs/m2/01-client-core.md §6.2): a Source behind media.Session's
// controls, with the test controls Fail, Emit, Calls and Origin. Next is for one goroutine at a time; every other
// method may be called from any goroutine, also while Next blocks.
//
// The session's clock starts at Start: CaptureNS 0 is Origin, Now is the time since then, and both move with the
// Source's 200 ms lag cap. Packet.CaptureNS equals the CaptureNS of the packet's Marker, so Origin plus a Marker's
// CaptureNS is the frame's capture instant on the wall clock (01 §12.4).
//
// A re-initialization (a Reconfigure that changes the profile, the preset or a layer's size or frame rate, and a
// SetBitrate of a re-init encoder) gives each affected layer a new encoder on the same timeline: a new Source layer
// whose frame indices, capture times and flash instants go on from the old one's, starting with a keyframe that is
// marked Reinit and preceded by the layer's EventLayer. Audio and the other layers go on untouched.
type Session struct {
	eng        *Engine
	mode       Mode
	encoder    media.EncoderInfo // the encoder the session started on (Probe)
	size       [2]int            // the source's size
	borderless bool
	flashEvery time.Duration
	seed       uint64
	hasAudio   bool
	tapPCM     bool
	startedAt  time.Time

	mu           sync.Mutex // guards the fields below
	src          *Source
	profile      string
	preset       string
	layers       []*layerState
	audioBitrate int
	audioMuted   bool // AudioController.SetAudioMuted: the pause mute
	audioOff     bool // AudioController.SetAudioMode(AudioOff): "Share sound" off
	audioStarted bool // the first audio packet went out (it is the Reinit one)
	paused       bool
	events       []media.Event   // returned by Next before any media
	held         *media.Packet   // a Reinit packet whose EventLayer Next just returned
	pcm          []int64         // capture times of the PCM chunks to come (TapPCM)
	pending      map[string]bool // layers to re-initialize at Next's next pull, or nil
	failErr      error           // what Next returns next (Fail), then the session stops
	pullCancel   context.CancelFunc
	done         bool      // stopped: by Stop, Engine.Close or a Fail that Next returned
	endedAt      time.Time // when it stopped
	stopCalled   bool
	calls        []Call
}

// layerState is one video layer of a Session.
type layerState struct {
	index         int
	cfg           media.LayerConfig // current: MaxPixels, MaxFPS and Bitrate change with Reconfigure and SetBitrate
	width, height int
	enc           media.EncoderInfo // the encoder running the layer: Name, HW, LiveBitrate
	active        bool
	needKey       bool // deliver nothing of the layer before a keyframe (resume, re-init)
	reinit        bool // the next delivered packet is the Reinit keyframe of a new encoder
	eventDue      bool // and its EventLayer goes out right before it
	lastForced    time.Time
	deferredKey   bool // a keyframe request waits for the 250 ms mark (04 §6.4)
	in, out       frameWindow
}

// Call is one control call that a Session accepted, with its time, for rate-control and pause assertions. Calls that
// returned an error are not recorded.
type Call struct {
	At time.Time
	// Op is "keyframe" (RequestKeyframe), "bitrate" (SetBitrate; Layer is media.LayerAudio for the Opus target),
	// "active" (SetLayerActive), "paused" (SetPaused), "reconfigure" (Reconfigure), "audio_mode"
	// (AudioController.SetAudioMode), "audio_muted" (AudioController.SetAudioMuted), "exclusions"
	// (AudioController.SetExclusions) or "stop" (the first Stop).
	Op     string
	Layer  string
	Bps    int
	On     bool            // "active": active; "paused": paused; "audio_muted": muted (as Muted)
	Reconf media.Reconfig  // "reconfigure": the argument
	Mode   media.AudioMode // "audio_mode": the mode (05 §13.3)
	Muted  bool            // "audio_muted": muted (05 §13.3)
}

var _ media.Session = (*Session)(nil)

// Timing rules of the native engine that the fake follows (04 §6.4, §9.4, §10.3).
const (
	gopFirst         = 4 * time.Second // layer 0 ("f")
	gopOther         = 2 * time.Second
	forcedKeySpacing = 250 * time.Millisecond
	statsWindow      = 2 * time.Second
	pcmChunkDuration = 10 * time.Millisecond
	pcmChunkFrames   = opusSampleRate / int(time.Second/pcmChunkDuration) // 480 stereo frames
	// pcmChunkBytes is the size of a KindPCM packet: 10 ms of 48 kHz stereo float32 (media.KindPCM).
	pcmChunkBytes = pcmChunkFrames * 2 * 4
)

// newSession starts a Session for a checked plan; the clock starts now.
func newSession(e *Engine, p plan) (*Session, error) {
	s := &Session{
		eng: e, mode: e.mode, encoder: p.encoder, size: p.size, borderless: p.borderless,
		flashEvery: e.flashEvery, seed: e.seed, hasAudio: p.audio != nil, profile: p.profile, preset: p.preset,
	}
	if p.audio != nil {
		s.tapPCM, s.audioBitrate = p.audio.TapPCM, p.audio.BitrateBPS
	}
	for i, lp := range p.layers {
		s.layers = append(s.layers, &layerState{
			index: i, cfg: lp.cfg, width: lp.width, height: lp.height, enc: p.encoder,
			active: !lp.cfg.Inactive, needKey: true, reinit: !lp.cfg.Inactive,
		})
	}
	src, err := s.newSource(s.profile, s.videoLayers(), s.hasAudio)
	if err != nil {
		return nil, errorf(media.CodeInvalid, "start", "", "%v", err)
	}
	s.startedAt = time.Now()
	src.started, src.origin = true, s.startedAt
	s.src = src
	// One EventLayer per layer, then media (01 §6.2). A layer's first packet is its Reinit keyframe; one that starts
	// inactive resumes later with a plain keyframe.
	for _, l := range s.layers {
		s.events = append(s.events, s.layerEvent(l))
	}
	return s, nil
}

// videoLayers returns the Source layers of the current layer states.
func (s *Session) videoLayers() []VideoLayer {
	out := make([]VideoLayer, 0, len(s.layers))
	for _, l := range s.layers {
		out = append(out, VideoLayer{RID: l.cfg.RID, Width: l.width, Height: l.height, FPS: l.cfg.MaxFPS,
			Bitrate: l.cfg.Bitrate})
	}
	return out
}

// newSource makes a Source for the session's mode with the engine's GOPs: f 4 s, other layers 2 s (04 §9.4).
func (s *Session) newSource(profile string, layers []VideoLayer, audio bool) (*Source, error) {
	cfg := Config{Mode: s.mode, Layers: layers, Audio: audio, FlashEvery: s.flashEvery, Seed: s.seed}
	if len(layers) > 0 {
		cfg.Profile = profile
	}
	src, err := New(cfg)
	if err != nil {
		return nil, err
	}
	i := 0
	for _, st := range src.streams {
		if st.kind != Video {
			continue
		}
		gop := gopOther
		if i == 0 {
			gop = gopFirst
		}
		st.gopFrames = max(int((gop*time.Duration(st.fps)+time.Second/2)/time.Second), 1)
		i++
	}
	return src, nil
}

// layerEvent is a layer's EventLayer: its size, rate and encoder, the session's profile and border.
func (s *Session) layerEvent(l *layerState) media.Event {
	return media.Event{
		Code: media.EventLayer, Layer: l.cfg.RID, Width: l.width, Height: l.height, FPS: l.cfg.MaxFPS,
		Encoder: l.enc.Name, HW: l.enc.HW, Profile: s.profile, LiveBitrate: l.enc.LiveBitrate,
		NoOSIndicator: s.borderless,
	}
}

// Next implements media.Session: the queued events (the EventLayers of Start and Emit's), then media in capture order
// per stream, without the packets of an inactive layer, without video while paused, and, after a resume or a
// re-init, a layer's media only from its next keyframe on. It returns ErrStopped after Stop, a Fail's error once (the
// session is then stopped), and ctx's error as soon as ctx ends.
func (s *Session) Next(ctx context.Context) (media.Packet, error) {
	for {
		s.mu.Lock()
		if s.done {
			s.mu.Unlock()
			return media.Packet{}, media.ErrStopped
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return media.Packet{}, err
		}
		if err := s.failErr; err != nil {
			s.endLocked()
			s.mu.Unlock()
			return media.Packet{}, err
		}
		now := time.Now()
		if p, ok := s.queuedLocked(now); ok {
			s.mu.Unlock()
			return p, nil
		}
		if s.pending != nil {
			if err := s.swapLocked(); err != nil {
				s.failErr = errorf(media.CodeInternal, "next", "", "re-init: %v", err)
				s.mu.Unlock()
				continue
			}
		}
		// Wake up for a deferred keyframe request or a PCM chunk even when no packet comes before them.
		deadline := s.applyDeferredKeysLocked(now)
		if len(s.pcm) > 0 {
			if due := s.src.Origin().Add(time.Duration(s.pcm[0])); deadline.IsZero() || due.Before(deadline) {
				deadline = due
			}
		}
		var pctx context.Context
		var cancel context.CancelFunc
		if deadline.IsZero() {
			pctx, cancel = context.WithCancel(ctx)
		} else {
			pctx, cancel = context.WithDeadline(ctx, deadline)
		}
		s.pullCancel = cancel // a control call that changes what Next returns cancels the pull (kickLocked)
		src := s.src
		s.mu.Unlock()

		// The Source returns ctx's error without making a packet when ctx ends while it waits, so a cancelled pull
		// loses nothing.
		pkt, err := src.Next(pctx)

		s.mu.Lock()
		s.pullCancel = nil
		cancel()
		if err != nil {
			// Kicked, a deadline, or the Source closed by Stop: look again. ctx's own end is returned at the top.
			s.mu.Unlock()
			continue
		}
		out, ok := s.deliverLocked(pkt, time.Now())
		s.mu.Unlock()
		if ok {
			return out, nil
		}
	}
}

// queuedLocked returns what Next returns before pulling: an event, a held Reinit packet, or a PCM chunk that is due.
func (s *Session) queuedLocked(now time.Time) (media.Packet, bool) {
	switch {
	case len(s.events) > 0:
		ev := s.events[0]
		s.events = s.events[1:]
		return media.Packet{Kind: media.KindEvent, Event: &ev}, true
	case s.held != nil:
		p := *s.held
		s.held = nil
		return p, true
	case len(s.pcm) > 0 && !now.Before(s.src.Origin().Add(time.Duration(s.pcm[0]))):
		capture := s.pcm[0]
		s.pcm = s.pcm[1:]
		return media.Packet{Kind: media.KindPCM, CaptureNS: capture, Data: pcmChunk(capture, s.audioSilentLocked())}, true
	}
	return media.Packet{}, false
}

// deliverLocked applies the session's state to a packet of the Source. ok is false when the packet is dropped.
func (s *Session) deliverLocked(p Packet, now time.Time) (media.Packet, bool) {
	switch p.Kind {
	case media.KindVideo:
		l := s.layerLocked(p.Layer)
		if l == nil {
			return media.Packet{}, false
		}
		l.in.add(now, 0)
		if s.paused || !l.active || (l.needKey && !p.Keyframe) {
			return media.Packet{}, false
		}
		l.needKey = false
		l.out.add(now, len(p.Data))
		if l.reinit {
			l.reinit, p.Reinit = false, true
			if l.eventDue {
				l.eventDue = false
				s.held = &p
				ev := s.layerEvent(l)
				return media.Packet{Kind: media.KindEvent, Event: &ev}, true
			}
		}
		return p, true
	case media.KindAudio:
		if !s.audioStarted {
			s.audioStarted, p.Reinit = true, true // 05 §10.4: the first packet after Start
		}
		if s.audioSilentLocked() {
			p.Data = silenced(p.Data)
		}
		if s.tapPCM {
			s.pcm = append(s.pcm, p.CaptureNS, p.CaptureNS+int64(pcmChunkDuration))
		}
		return p, true
	}
	return media.Packet{}, false
}

// swapLocked re-initializes the pending layers: a new Source with new streams for them, on the old one's timeline,
// and the old streams of the other layers and of the audio moved over as they are. Only Next calls it, so the old
// Source is not in Next.
func (s *Session) swapLocked() error {
	affected := s.pending
	s.pending = nil
	nsrc, err := s.newSource(s.profile, s.videoLayers(), false)
	if err != nil {
		return err
	}
	old := s.src
	old.mu.Lock()
	for i, ns := range nsrc.streams {
		prev := old.video(ns.rid)
		if !affected[ns.rid] {
			nsrc.streams[i] = prev
			continue
		}
		// The new encoder's frames go on where the old one's stopped: the same frame indices and flash instants, at
		// the new rate from the old stream's next capture instant, starting with a keyframe.
		ns.index, ns.baseIndex, ns.base, ns.next = prev.index, prev.index, prev.next, prev.next
		ns.nextFlash, ns.forceKey = prev.nextFlash, true
	}
	for _, st := range old.streams {
		if st.kind == Audio {
			nsrc.streams = append(nsrc.streams, st)
		}
	}
	nsrc.started, nsrc.origin = old.started, old.origin
	old.mu.Unlock()
	_ = old.Close()
	s.src = nsrc
	for _, l := range s.layers {
		if affected[l.cfg.RID] {
			l.needKey, l.reinit, l.eventDue = true, true, true
		}
	}
	return nil
}

// reinitLocked schedules a re-initialization of the layers for Next's next pull.
func (s *Session) reinitLocked(rids ...string) {
	if len(rids) == 0 {
		return
	}
	if s.pending == nil {
		s.pending = map[string]bool{}
	}
	for _, rid := range rids {
		s.pending[rid] = true
	}
	s.kickLocked()
}

// kickLocked makes a Next that waits for the Source look at the session again.
func (s *Session) kickLocked() {
	if s.pullCancel != nil {
		s.pullCancel()
	}
}

// requestKeyLocked asks the Source for a keyframe of l, at most one per 250 ms (04 §6.4): a request inside that
// window waits for its end.
func (s *Session) requestKeyLocked(l *layerState, now time.Time) {
	if !l.lastForced.IsZero() && now.Sub(l.lastForced) < forcedKeySpacing {
		l.deferredKey = true
		s.kickLocked()
		return
	}
	s.src.RequestKeyframe(l.cfg.RID)
	l.lastForced, l.deferredKey = now, false
}

// applyDeferredKeysLocked passes on the deferred keyframe requests that are due and returns when the next one is
// (zero: none).
func (s *Session) applyDeferredKeysLocked(now time.Time) time.Time {
	var next time.Time
	for _, l := range s.layers {
		if !l.deferredKey {
			continue
		}
		due := l.lastForced.Add(forcedKeySpacing)
		if !now.Before(due) {
			s.requestKeyLocked(l, now)
			continue
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}

// Now implements media.Session: the time since Origin, on the clock of Packet.CaptureNS.
func (s *Session) Now() int64 {
	s.mu.Lock()
	src := s.src
	s.mu.Unlock()
	return src.Now()
}

// Origin returns the wall-clock instant of CaptureNS 0 (the Source's Origin, which the lag cap moves forward): Origin
// plus a Marker's CaptureNS is when that frame was captured (01 §12.4).
func (s *Session) Origin() time.Time {
	s.mu.Lock()
	src := s.src
	s.mu.Unlock()
	return src.Origin()
}

// RequestKeyframe implements media.Session: the layer's next or second-next frame is a keyframe, or, within 250 ms
// of the layer's last forced keyframe, its first frame after the 250 ms mark. Unknown layers are ignored.
func (s *Session) RequestKeyframe(layer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.layerLocked(layer)
	if s.done || l == nil {
		return
	}
	s.requestKeyLocked(l, time.Now())
	s.recordLocked(Call{Op: "keyframe", Layer: layer})
}

// SetBitrate implements media.Session. A video layer's target must be in 04 §6.4's range (layer 0 100 kbit/s–
// 50 Mbit/s, others 50 kbit/s–2 Mbit/s), the Opus target (media.LayerAudio) in 32–510 kbit/s and only with audio;
// anything else, 0 included, is CodeInvalid. With a live encoder (the layer's LiveBitrate) Synthetic video follows the
// new target from the next frame on (Decodable has no rate control); with a re-init encoder every change re-initializes
// the layer: its EventLayer, then a Reinit keyframe. The same target again changes nothing.
func (s *Session) SetBitrate(layer string, bps int) error {
	const op = "set_bitrate"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return errStopped(op)
	}
	if layer == media.LayerAudio {
		switch {
		case !s.hasAudio:
			return errorf(media.CodeInvalid, op, "", "the session has no audio")
		case bps < minAudioBitrate || bps > maxAudioBitrate:
			return errorf(media.CodeInvalid, op, "", "audio bitrate %d not in %d–%d", bps, minAudioBitrate,
				maxAudioBitrate)
		}
		s.audioBitrate = bps
		s.recordLocked(Call{Op: "bitrate", Layer: layer, Bps: bps})
		return nil
	}
	l := s.layerLocked(layer)
	if l == nil {
		return errorf(media.CodeInvalid, op, "", "unknown layer %q", layer)
	}
	if err := checkBitrate(op, l.index, layer, bps); err != nil {
		return err
	}
	if bps != l.cfg.Bitrate {
		l.cfg.Bitrate = bps
		if l.enc.LiveBitrate {
			s.src.SetBitrate(layer, bps)
		} else {
			s.reinitLocked(layer)
		}
	}
	s.recordLocked(Call{Op: "bitrate", Layer: layer, Bps: bps})
	return nil
}

// SetLayerActive implements media.Session: an inactive layer sends nothing; a resumed one starts with a keyframe that
// is not a Reinit. An unknown layer is CodeInvalid.
func (s *Session) SetLayerActive(layer string, active bool) error {
	const op = "set_layer_active"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return errStopped(op)
	}
	l := s.layerLocked(layer)
	if l == nil {
		return errorf(media.CodeInvalid, op, "", "unknown layer %q", layer)
	}
	if active && !l.active {
		l.active, l.needKey = true, true
		s.requestKeyLocked(l, time.Now())
	}
	if !active {
		l.active = false
	}
	s.recordLocked(Call{Op: "active", Layer: layer, On: active})
	return nil
}

// SetPaused implements media.Session: while paused Next returns no video (audio goes on, muted only through
// AudioController.SetAudioMuted, 01 D14); un-paused, video resumes where it is (the core asks for keyframes), and a
// layer that still waits for a keyframe (a resume or a re-init during the pause) gets one.
func (s *Session) SetPaused(paused bool) error {
	const op = "set_paused"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return errStopped(op)
	}
	s.paused = paused
	if !paused {
		now := time.Now()
		for _, l := range s.layers {
			if l.active && l.needKey {
				s.requestKeyLocked(l, now)
			}
		}
	}
	s.recordLocked(Call{Op: "paused", On: paused})
	return nil
}

// Reconfigure implements media.Session. Zero fields keep their value. A Profile ("6400" or "42e0"; Decodable only
// "42e0") re-initializes every layer, also when it is the current one, and a session whose encoder lacks the profile
// runs the encoder's (04 §9.6: its EventLayers tell). A changed Preset re-initializes every layer; the same one again
// changes nothing. A layer (by RID; an unknown one is CodeInvalid) with MaxPixels or MaxFPS set re-initializes at
// its new size or rate; a Bitrate alone is SetBitrate's. Audio and capture go on. An invalid Reconfig changes nothing.
func (s *Session) Reconfigure(r media.Reconfig) error {
	const op = "reconfigure"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return errStopped(op)
	}
	profile, preset := s.profile, s.preset
	var err error
	if r.Profile != "" {
		if profile, err = s.eng.profileFor(op, s.encoder, r.Profile); err != nil {
			return err
		}
	}
	if r.Preset != "" {
		if preset, err = checkPreset(op, r.Preset); err != nil {
			return err
		}
	}
	type change struct {
		cfg           media.LayerConfig
		width, height int
		structural    bool // size or rate: a re-init
	}
	changes := map[*layerState]*change{}
	for _, rl := range r.Layers {
		l := s.layerLocked(rl.RID)
		if l == nil {
			return errorf(media.CodeInvalid, op, "", "unknown layer %q", rl.RID)
		}
		c := changes[l]
		if c == nil {
			c = &change{cfg: l.cfg, width: l.width, height: l.height}
			changes[l] = c
		}
		switch {
		case rl.MaxPixels < 0 || rl.MaxFPS < 0 || rl.Bitrate < 0:
			return errorf(media.CodeInvalid, op, "", "layer %q: negative value", rl.RID)
		case rl.MaxFPS > maxFPS:
			return errorf(media.CodeInvalid, op, "", "layer %q: MaxFPS %d not in 1–%d", rl.RID, rl.MaxFPS, maxFPS)
		}
		if rl.MaxPixels > 0 {
			w, h, ok := scaleToFit(s.size[0], s.size[1], rl.MaxPixels)
			if !ok {
				return errorf(media.CodeInvalid, op, "", "layer %q: MaxPixels %d leaves no picture", rl.RID,
					rl.MaxPixels)
			}
			c.cfg.MaxPixels, c.width, c.height, c.structural = rl.MaxPixels, w, h, true
		}
		if rl.MaxFPS > 0 {
			c.cfg.MaxFPS, c.structural = rl.MaxFPS, true
		}
		if rl.Bitrate > 0 {
			if err := checkBitrate(op, l.index, rl.RID, rl.Bitrate); err != nil {
				return err
			}
			c.cfg.Bitrate = rl.Bitrate
		}
	}
	structural := r.Profile != ""
	for _, c := range changes {
		structural = structural || c.structural
	}
	if structural && len(s.layers) > 0 {
		// Check that the Source takes the new layers (their H.264 levels) before changing anything.
		trial := make([]VideoLayer, len(s.layers))
		for i, l := range s.layers {
			vl := VideoLayer{RID: l.cfg.RID, Width: l.width, Height: l.height, FPS: l.cfg.MaxFPS, Bitrate: l.cfg.Bitrate}
			if c := changes[l]; c != nil {
				vl.Width, vl.Height, vl.FPS, vl.Bitrate = c.width, c.height, c.cfg.MaxFPS, c.cfg.Bitrate
			}
			trial[i] = vl
		}
		t, err := s.newSource(profile, trial, false)
		if err != nil {
			return errorf(media.CodeInvalid, op, "", "%v", err)
		}
		_ = t.Close()
	}

	all := r.Profile != "" || preset != s.preset
	s.profile, s.preset = profile, preset
	var affected []string
	for _, l := range s.layers {
		c := changes[l]
		if c != nil {
			bitrateChanged := c.cfg.Bitrate != l.cfg.Bitrate
			l.cfg, l.width, l.height = c.cfg, c.width, c.height
			if bitrateChanged && l.enc.LiveBitrate && !c.structural && !all {
				s.src.SetBitrate(l.cfg.RID, l.cfg.Bitrate)
			}
			if c.structural || (bitrateChanged && !l.enc.LiveBitrate) {
				affected = append(affected, l.cfg.RID)
				continue
			}
		}
		if all {
			affected = append(affected, l.cfg.RID)
		}
	}
	s.reinitLocked(affected...)
	rc := r
	rc.Layers = slices.Clone(r.Layers)
	s.recordLocked(Call{Op: "reconfigure", Reconf: rc})
	return nil
}

// Stats implements media.Session from the session's own counters: per layer the frames the Source made (FPSIn) and
// that Next returned (FPSOut) and the returned bitrate, over the last 2 s; the target; Limit "none"; no latencies
// and no drops.
func (s *Session) Stats() (media.EngineStats, error) {
	const op = "stats"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return media.EngineStats{}, errStopped(op)
	}
	now := time.Now()
	st := media.EngineStats{Layers: make([]media.EngineLayerStats, 0, len(s.layers))}
	for _, l := range s.layers {
		in, _ := l.in.sum(now)
		out, bytes := l.out.sum(now)
		st.Layers = append(st.Layers, media.EngineLayerStats{
			RID: l.cfg.RID, Encoder: l.enc.Name, HW: l.enc.HW,
			FPSIn: float64(in) / statsWindow.Seconds(), FPSOut: float64(out) / statsWindow.Seconds(),
			BitrateBps: int(float64(bytes) * 8 / statsWindow.Seconds()), TargetBps: l.cfg.Bitrate, Limit: "none",
		})
	}
	return st, nil
}

// Stop implements media.Session: it ends the session and unblocks Next at once. Idempotent; it always returns nil.
func (s *Session) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopCalled {
		s.stopCalled = true
		s.recordLocked(Call{Op: "stop"})
	}
	s.endLocked()
	return nil
}

// endLocked stops the session and frees the engine for the next Start.
func (s *Session) endLocked() {
	if s.done {
		return
	}
	s.done, s.endedAt = true, time.Now()
	s.pending, s.events, s.held, s.pcm = nil, nil, nil, nil
	_ = s.src.Close()
	s.kickLocked()
	s.eng.ended(s)
}

// Fail makes the next Next return err, as if the engine ended the session with it (an ENDED event); the session is
// then stopped. A Next that waits returns at once. nil ends the session cleanly: Next returns media.ErrStopped. A
// stopped session ignores it, and so does one that already failed.
func (s *Session) Fail(err *media.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.failErr != nil {
		return
	}
	if err == nil {
		s.failErr = media.ErrStopped
	} else {
		s.failErr = err
	}
	s.kickLocked()
}

// Emit makes Next return ev (as a KindEvent packet) before any more media; a Next that waits returns it at once. An
// EventLayer that names an Encoder also makes it the layer's encoder: its Name, HW and LiveBitrate are what Stats,
// later EventLayers and SetBitrate's re-init rule use (an engine's fallback to another encoder, 04 §9.7). A stopped
// session ignores it.
func (s *Session) Emit(ev media.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	if l := s.layerLocked(ev.Layer); ev.Code == media.EventLayer && ev.Encoder != "" && l != nil {
		l.enc = media.EncoderInfo{Name: ev.Encoder, HW: ev.HW, LiveBitrate: ev.LiveBitrate}
	}
	s.events = append(s.events, ev)
	s.kickLocked()
}

// Calls returns every control call the session accepted, in order, with its time.
func (s *Session) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.calls)
	for i := range out {
		out[i].Reconf.Layers = slices.Clone(out[i].Reconf.Layers)
	}
	return out
}

func (s *Session) recordLocked(c Call) {
	c.At = time.Now()
	s.calls = append(s.calls, c)
}

func (s *Session) layerLocked(rid string) *layerState {
	for _, l := range s.layers {
		if l.cfg.RID == rid {
			return l
		}
	}
	return nil
}

// setAudioMuted is AudioController.SetAudioMuted's part in the session: muted, the audio packets carry Opus silence
// (and PCM chunks zeros) instead of the tone, with their Markers. It needs a running session with audio.
func (s *Session) setAudioMuted(muted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || !s.hasAudio {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: "audio_set_muted"}
	}
	s.audioMuted = muted
	s.recordLocked(Call{Op: "audio_muted", On: muted, Muted: muted})
	return nil
}

// setAudioMode is AudioController.SetAudioMode's part in the session, for a mode the caller checked: with
// media.AudioOff ("Share sound" off) the audio packets carry Opus silence as when muted, since the native mix is
// silent while either flag is set (05 §4.3); media.AudioExcludeApps turns the sound back on. Each flag is cleared
// only by its own call. It needs a running session with audio.
func (s *Session) setAudioMode(m media.AudioMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || !s.hasAudio {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: "audio_set_mode"}
	}
	s.audioOff = m == media.AudioOff
	s.recordLocked(Call{Op: "audio_mode", Mode: m})
	return nil
}

// audioSilentLocked reports whether the session's audio is silence: muted or sound off.
func (s *Session) audioSilentLocked() bool {
	return s.audioMuted || s.audioOff
}

// audioInfo is what the AudioController's report reads from a session with audio.
type audioInfo struct {
	start, end         time.Time // end is zero while it runs
	paused, muted, off bool
}

// liveAt reports whether the session ran at t.
func (i audioInfo) liveAt(t time.Time) bool {
	return !t.Before(i.start) && (i.end.IsZero() || t.Before(i.end))
}

// audioInfo returns the session's audioInfo.
func (s *Session) audioInfo() audioInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return audioInfo{start: s.startedAt, end: s.endedAt, paused: s.paused, muted: s.audioMuted, off: s.audioOff}
}

// recordExclusions records an AudioController.SetExclusions call made while the session runs.
func (s *Session) recordExclusions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.recordLocked(Call{Op: "exclusions"})
	}
}

func errStopped(op string) *media.Error {
	return &media.Error{Code: media.CodeStopped, Op: op}
}

// frameWindow counts frames and bytes over the last statsWindow.
type frameWindow struct {
	t []time.Time
	n []int
}

func (w *frameWindow) add(now time.Time, bytes int) {
	w.trim(now)
	w.t = append(w.t, now)
	w.n = append(w.n, bytes)
}

func (w *frameWindow) trim(now time.Time) {
	i := 0
	for i < len(w.t) && now.Sub(w.t[i]) > statsWindow {
		i++
	}
	if i > 0 {
		w.t, w.n = slices.Delete(w.t, 0, i), slices.Delete(w.n, 0, i)
	}
}

// sum returns the frames and bytes of the window.
func (w *frameWindow) sum(now time.Time) (frames, bytes int) {
	w.trim(now)
	for _, n := range w.n {
		bytes += n
	}
	return len(w.t), bytes
}

// silenceOpus is a 20 ms Opus frame of digital silence (CELT, fullband), with the stereo flag of the asset.
var silenceOpus = sync.OnceValue(func() opusPacket {
	var stereo byte
	if loop, err := audioLoop(); err == nil && len(loop) > 0 {
		stereo = loop[0].toc & 0x04
	}
	p, err := parseOpus([]byte{0xf8 | stereo, 0xff, 0xfe})
	if err != nil {
		panic("fake: the Opus silence frame: " + err.Error())
	}
	return p
})

// silenced returns a muted audio packet: Opus silence with the packet's Marker (its beep flag cleared, since nothing
// beeps) in the padding, so viewers' marker and continuity checks go on.
func silenced(opus []byte) []byte {
	m, _ := ParseAudioMarker(opus)
	m.Beep = false
	return opusWithMarker(silenceOpus(), m)
}

// pcmChunk returns the KindPCM chunk captured at capture: 10 ms of the tone as interleaved little-endian float32
// stereo at 48 kHz, a 440 Hz tone with a 1 kHz beep in the first 100 ms of every second like the Opus asset; zeros
// when muted.
func pcmChunk(capture int64, muted bool) []byte {
	b := make([]byte, pcmChunkBytes)
	if muted {
		return b
	}
	n0 := capture * opusSampleRate / int64(time.Second)
	for i := range pcmChunkFrames {
		n := (n0 + int64(i)) % opusSampleRate // both tones have whole cycles per second
		f := 440.0
		if n < opusSampleRate/10 {
			f = 1000
		}
		v := math.Float32bits(float32(0.25 * math.Sin(2*math.Pi*f*float64(n)/opusSampleRate)))
		binary.LittleEndian.PutUint32(b[8*i:], v)
		binary.LittleEndian.PutUint32(b[8*i+4:], v)
	}
	return b
}
