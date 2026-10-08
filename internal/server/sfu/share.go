package sfu

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Share is one stream a participant shares: up to two simulcast video layers and an audio layer from exactly one
// Conn's pub PC, fanned out to the DownTracks of its subscribers (02 §5.1). The hub creates its id, owns its
// lifecycle and both 30 s timeouts; the SFU reports media facts (pending, live, stalled) and never ends a share by
// itself (02 §5.3). A Share survives a rebuild of its pub PC: the new PC's tracks attach to it as new Layers.
type Share struct {
	id        ShareID
	room      *Room
	part      *Participant
	conn      *Conn // the source Conn
	source    SourceKind
	startedAt time.Time

	// notify orders the share's RoomEvents calls: the ticker holds it while it reports ShareUpdated, and SFU.endShare
	// while it ends the share and reports ShareEnded, so nothing is ever reported about a share after its end. It is
	// the one lock held across a RoomEvents call (which never blocks); it is taken before mu, and never while mu or
	// another lock of doc.go's order is held.
	notify sync.Mutex

	mu      sync.Mutex // guards the fields below
	preset  Preset
	state   ShareState
	ended   bool
	liveAt  time.Time
	profile ProfileKey
	layers  [SlotAudio + 1]*Layer // by Slot; nil while no track is attached
	// What the ticker still has to report with ShareUpdated (02 §6.2): updates are the share as it was at each state
	// change, oldest first, none of which is ever merged away; dirty says that a layer, the profile or the audio
	// changed since the last report while the share was live; notified is when the last report went out (monoNow),
	// for the 250 ms debounce of those changes.
	updates  []ShareInfo
	dirty    bool
	notified int64

	// awaitsKeyframe is true while the share is pending: until its first keyframe, its video layers ask the publisher
	// for one whenever another packet arrives (Layer.handleRTP). The layers' read loops read it without a lock.
	awaitsKeyframe atomic.Bool

	// The fan-out lists: the DownTracks of every subscription to this share, by kind. They are copied on write (under
	// mu), so the media path reads them without a lock (02 §5.4).
	video, audio atomic.Pointer[[]*DownTrack]
}

// Encodings per preset (02 §8.6).
const (
	fullMaxBitrate    = 8_000_000
	fullMaxPixels     = 1920 * 1080 // 2 073 600
	fullFramerate     = 60
	textFramerate     = 30
	previewMaxBitrate = 300_000
	previewMaxPixels  = 640 * 360 // 230 400
	previewFramerate  = 15
	audioBitrate      = 128_000
	audioBitrateMovie = 256_000
)

// shareParams returns what a sharer encodes for a preset (02 §8.6): the full layer f, then the preview layer q, both
// active. The admin cap (Limits.MaxShareKbps) lowers f's bitrate and never touches q. profile is the room's codec
// policy.
func shareParams(preset Preset, limits Limits, profile ProfileKey) ShareParams {
	full := EncodingParams{
		RID: ridFull, Active: true, MaxBitrate: fullMaxBitrate, MaxFramerate: fullFramerate, MaxPixels: fullMaxPixels,
	}
	if preset == PresetText {
		full.MaxFramerate = textFramerate
	}
	if limit := limits.MaxShareKbps; limit > 0 && limit < fullMaxBitrate/1000 {
		full.MaxBitrate = limit * 1000
	}
	preview := EncodingParams{
		RID: ridPreview, Active: true, MaxBitrate: previewMaxBitrate, MaxFramerate: previewFramerate,
		MaxPixels: previewMaxPixels,
	}
	return ShareParams{
		Profile:      profile,
		Encodings:    []EncodingParams{full, preview},
		AudioBitrate: presetAudioBitrate(preset),
	}
}

// presetAudioBitrate is the Opus target of a preset, which the pub answer carries as maxaveragebitrate (02 §8.4).
func presetAudioBitrate(preset Preset) int {
	if preset == PresetMovie {
		return audioBitrateMovie
	}
	return audioBitrate
}

// info returns the share's ShareInfo and whether the share still exists.
func (s *Share) info() (ShareInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked(), !s.ended
}

// infoLocked builds the ShareInfo; s.mu is held. The layers' numbers and the viewers come from atomics, so no other
// lock is taken.
func (s *Share) infoLocked() ShareInfo {
	info := ShareInfo{
		ID: s.id, Room: s.room.id, Participant: s.part.id, User: s.part.user, Conn: s.conn.id,
		Source: s.source, Preset: s.preset, State: s.state, Profile: s.profile,
		Audio:     s.layers[SlotAudio] != nil,
		Viewers:   s.viewers(),
		StartedAt: s.startedAt, LiveAt: s.liveAt,
	}
	now := monoNow()
	for _, slot := range [...]Slot{SlotF, SlotH, SlotQ} {
		if l := s.layers[slot]; l != nil {
			info.Layers = append(info.Layers, l.info(now))
		}
	}
	return info
}

// viewers returns the participants a DownTrack of this share forwards to now, sorted and without repeats
// (ShareInfo.Viewers).
func (s *Share) viewers() []ParticipantID {
	var out []ParticipantID
	for _, list := range [...]*atomic.Pointer[[]*DownTrack]{&s.video, &s.audio} {
		dts := list.Load()
		if dts == nil {
			continue
		}
		for _, dt := range *dts {
			if id := dt.sub.conn.part.id; dt.forwarding.Load() && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// liveState returns the share's state, and false once it has ended.
func (s *Share) liveState() (ShareState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, !s.ended
}

// currentProfile returns the H.264 profile of the share's video, "" until it is known.
func (s *Share) currentProfile() ProfileKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profile
}

// audioTarget returns the Opus bitrate the share's preset asks for now.
func (s *Share) audioTarget() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return presetAudioBitrate(s.preset)
}

// setPreset changes the preset; false when the share has ended.
func (s *Share) setPreset(p Preset) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.preset = p
	return true
}

// attach makes l the share's Layer for its slot and returns the Layer it replaces, if any (a track whose SSRC
// changed). ok is false when the share has ended: nothing attaches to an ended share.
func (s *Share) attach(l *Layer) (replaced *Layer, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, false
	}
	replaced = s.layers[l.slot]
	s.layers[l.slot] = l
	s.changedLocked()
	return replaced, true
}

// detach removes l from the share if it is still the Layer of its slot.
func (s *Share) detach(l *Layer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layers[l.slot] == l {
		s.layers[l.slot] = nil
		s.changedLocked()
	}
}

// layer returns the Layer attached for a slot, or nil.
func (s *Share) layer(slot Slot) *Layer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.layers[slot]
}

// attached returns the Layers attached now.
func (s *Share) attached() []*Layer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Layer
	for _, l := range s.layers {
		if l != nil {
			out = append(out, l)
		}
	}
	return out
}

// requestKeyframe asks the publisher for a keyframe on the layer attached for slot, at most once per 500 ms and
// layer (02 §9.7, Layer.requestKeyframe). Without a layer there it does nothing: the layer's first packets ask.
func (s *Share) requestKeyframe(slot Slot) {
	if l := s.layer(slot); l != nil {
		l.requestKeyframe(monoNow())
	}
}

// keyframe notes that a packet starting a keyframe arrived on video layer l, in the given profile (the layer's read
// loop calls it). The first one makes a pending share live (02 §5.3); it reports whether this one did. The profile
// of the newest keyframe is the share's. A Layer that was detached meanwhile changes nothing.
func (s *Share) keyframe(l *Layer, profile ProfileKey) (wentLive bool) {
	s.mu.Lock()
	if s.ended || s.layers[l.slot] != l {
		s.mu.Unlock()
		return false
	}
	if s.profile != profile {
		s.profile = profile
		s.changedLocked()
	}
	// SharePending is the only state a keyframe ends in this slice; README S57 adds stalled, which the first keyframe
	// of a reconnected pub PC ends the same way.
	if s.state == SharePending {
		s.state, s.liveAt = ShareLive, time.Now()
		s.awaitsKeyframe.Store(false)
		s.stateChangedLocked()
		wentLive = true
	}
	s.mu.Unlock()
	if wentLive {
		s.conn.sfu.shareLive(s)
	}
	return wentLive
}

// stateChangedLocked queues a ShareUpdated for a state change, with the share as it is now; s.mu is held. A state
// change is reported at once and never merged with another (02 §6.2). It also covers every change made before it.
func (s *Share) stateChangedLocked() {
	s.updates = append(s.updates, s.infoLocked())
	s.dirty = false
}

// changedLocked notes that a layer, the profile or the audio changed; s.mu is held. That is reported only while the
// share is live, and debounced to one ShareUpdated per 250 ms (02 §5.3, §6.2).
func (s *Share) changedLocked() {
	if s.state == ShareLive {
		s.dirty = true
	}
}

// takeUpdates returns what the ticker reports with ShareUpdated now, oldest first: every queued state change, or
// else, when the share changed otherwise and its last report is at least 250 ms old, the share as it is. So a change
// that follows a state change closely waits its 250 ms like any other. An ended share reports nothing more. The
// caller holds s.notify.
func (s *Share) takeUpdates(now int64) []ShareInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.updates
	s.updates = nil
	switch {
	case s.ended:
		return nil
	case len(out) > 0:
	case s.dirty && now-s.notified >= int64(tickInterval):
		out = append(out, s.infoLocked())
		s.dirty = false
	default:
		return nil
	}
	s.notified = now
	return out
}

// end marks the share ended, detaches its layers and empties its fan-out lists. It returns the last ShareInfo, the
// state changes the ticker hadn't reported yet (they are still reported, before the end: none is ever merged away)
// and the Conns that subscribed to the share, without repeats. Only SFU.endShare calls it, once, after taking the
// share out of the registry and holding s.notify.
func (s *Share) end() (info ShareInfo, unreported []ShareInfo, subscribers []*Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info = s.infoLocked()
	unreported = s.updates
	s.ended = true
	s.layers = [SlotAudio + 1]*Layer{}
	s.updates, s.dirty = nil, false
	s.awaitsKeyframe.Store(false)
	for _, list := range []*atomic.Pointer[[]*DownTrack]{&s.video, &s.audio} {
		if dts := list.Swap(nil); dts != nil {
			for _, dt := range *dts {
				if !slices.Contains(subscribers, dt.sub.conn) {
					subscribers = append(subscribers, dt.sub.conn)
				}
			}
		}
	}
	return info, unreported, subscribers
}

// fanOut returns the share's fan-out list for a kind.
func (s *Share) fanOut(kind webrtc.RTPCodecType) *atomic.Pointer[[]*DownTrack] {
	if kind == webrtc.RTPCodecTypeAudio {
		return &s.audio
	}
	return &s.video
}

// addDownTrack puts dt on the share's fan-out list of its kind; false when the share has ended.
func (s *Share) addDownTrack(dt *DownTrack) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	list := s.fanOut(dt.kind)
	var next []*DownTrack
	if cur := list.Load(); cur != nil {
		next = slices.Clone(*cur)
	}
	next = append(next, dt)
	list.Store(&next)
	return true
}

// removeDownTrack takes dt off the share's fan-out list.
func (s *Share) removeDownTrack(dt *DownTrack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.fanOut(dt.kind)
	cur := list.Load()
	if cur == nil || !slices.Contains(*cur, dt) {
		return
	}
	next := slices.DeleteFunc(slices.Clone(*cur), func(d *DownTrack) bool { return d == dt })
	list.Store(&next)
}

// downTracks returns the current fan-out list of a kind; the caller must not change it.
func (s *Share) downTracks(kind webrtc.RTPCodecType) []*DownTrack {
	if cur := s.fanOut(kind).Load(); cur != nil {
		return *cur
	}
	return nil
}

// ---- the Conn's share calls (02 §6.1) ----

// StartShare creates a share in the pending state under the id the hub chose, and returns how the sharer encodes it
// (02 §8.6). It returns sfu.role_forbidden for a role that can't publish and sfu.too_many_shares for a participant's
// fifth share.
func (c *Conn) StartShare(ctx context.Context, p StartShareParams) (ShareParams, error) {
	var params ShareParams
	err := c.do(ctx, func(context.Context) error {
		var err error
		params, err = c.startShare(p)
		return err
	})
	if err != nil {
		return ShareParams{}, err
	}
	return params, nil
}

func (c *Conn) startShare(p StartShareParams) (ShareParams, error) {
	switch {
	case !c.role.canPublish():
		return ShareParams{}, newError(CodeRoleForbidden, "the role can't publish")
	case p.ID == "":
		return ShareParams{}, errCaller("StartShare: no share id")
	case !p.Preset.valid():
		return ShareParams{}, errCaller("StartShare: unknown preset")
	}
	sh, err := c.sfu.addShare(c, p)
	if err != nil {
		return ShareParams{}, err
	}
	c.log.Info("share started", "share_id", string(sh.id), "preset", p.Preset.String(), "source", p.Source.String(),
		"audio", p.Audio)
	return shareParams(p.Preset, c.sfu.currentLimits(), c.room.codecPolicy()), nil
}

// UpdateShare changes a share this Conn publishes and returns its new ShareParams: the encodings apply at once, the
// audio bitrate with the next publish offer's answer. It returns sfu.share_not_found for an unknown or ended share
// and sfu.not_owner for another Conn's.
func (c *Conn) UpdateShare(ctx context.Context, id ShareID, u ShareUpdate) (ShareParams, error) {
	var params ShareParams
	err := c.do(ctx, func(context.Context) error {
		var err error
		params, err = c.updateShare(id, u)
		return err
	})
	if err != nil {
		return ShareParams{}, err
	}
	return params, nil
}

func (c *Conn) updateShare(id ShareID, u ShareUpdate) (ShareParams, error) {
	sh, err := c.ownShare(id)
	if err != nil {
		return ShareParams{}, err
	}
	if u.Preset != nil {
		if !u.Preset.valid() {
			return ShareParams{}, errCaller("UpdateShare: unknown preset")
		}
		if !sh.setPreset(*u.Preset) {
			return ShareParams{}, newError(CodeShareNotFound, "the share ended")
		}
	}
	info, ok := sh.info()
	if !ok {
		return ShareParams{}, newError(CodeShareNotFound, "the share ended")
	}
	return shareParams(info.Preset, c.sfu.currentLimits(), c.room.codecPolicy()), nil
}

// StopShare ends a share this Conn publishes with the hub's reason: RoomEvents.ShareEnded fires, the subscribers'
// m-sections go inactive, and the next pub offer's m-sections for it are answered a=inactive. It returns
// sfu.share_not_found for an unknown or ended share and sfu.not_owner for another Conn's.
func (c *Conn) StopShare(ctx context.Context, id ShareID, r EndReason) error {
	return c.do(ctx, func(context.Context) error {
		sh, err := c.ownShare(id)
		if err != nil {
			return err
		}
		if !c.sfu.endShare(sh, r) {
			return newError(CodeShareNotFound, "the share ended")
		}
		c.detachShare(sh) // at once, not only when the post arrives: the next command may be this share's pub offer
		return nil
	})
}

// ownShare returns a share that hasn't ended and that this Conn publishes.
func (c *Conn) ownShare(id ShareID) (*Share, error) {
	sh := c.sfu.lookupShare(id)
	switch {
	case sh == nil:
		return nil, newError(CodeShareNotFound, "no such share")
	case sh.conn != c:
		return nil, newError(CodeNotOwner, "the share belongs to another connection")
	}
	return sh, nil
}
