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

	// awaitsKeyframe is true while the share is pending or stalled: until the keyframe that makes it live, its video
	// layers ask the publisher for one whenever another packet arrives (Layer.handleRTP). The layers' read loops read
	// it without a lock.
	awaitsKeyframe atomic.Bool

	// audioBps is the Opus bitrate of the share's preset (presetAudioBitrate), set with the preset. The pub answer
	// and the NACK path read it without a lock (audioTarget, maxBitrate).
	audioBps atomic.Int64

	// The fan-out lists: the DownTracks of every subscription to this share, by kind. They are copied on write (under
	// mu), so the media path reads them without a lock (02 §5.4).
	video, audio atomic.Pointer[[]*DownTrack]

	// paused holds the slot bits of the layers the sharer was told to pause (02 §11): up.paused, for the goroutines
	// other than the publishing Conn's actor. A keyframe request for a paused layer does nothing
	// (Layer.requestKeyframe), and a viewer's new request for high has the actor look at once (askedForHigh).
	paused atomic.Uint32
	// up is the share's uplink control (02 §11, uplink.go). It belongs to the publishing Conn's actor, like the rest of
	// what that Conn publishes; addShare sets it there.
	up shareUplink
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

// shareParams returns what a sharer encodes for a preset (02 §8.6): the encodings for the share's cap in bit/s
// (0 for none) and the layers paused now (slot bits), the audio bitrate of the preset, and profile, the room's codec
// policy.
func shareParams(preset Preset, capBps int, paused uint32, profile ProfileKey) ShareParams {
	return ShareParams{
		Profile:      profile,
		Encodings:    presetEncodings(preset, capBps, paused),
		AudioBitrate: presetAudioBitrate(preset),
	}
}

// presetEncodings returns the encodings of a preset (02 §8.6): the full layer f, then the preview layer q. The cap
// (capBps, 0 for none) lowers f's bitrate and never touches q; each layer whose slot bit is in paused is inactive
// (02 §11), the others active. StartShare, UpdateShare and the quality hints all build their lists here.
func presetEncodings(preset Preset, capBps int, paused uint32) []EncodingParams {
	full := EncodingParams{
		RID: ridFull, Active: paused&(1<<SlotF) == 0, MaxBitrate: fullLayerMax(capBps), MaxFramerate: fullFramerate,
		MaxPixels: fullMaxPixels,
	}
	if preset == PresetText {
		full.MaxFramerate = textFramerate
	}
	preview := EncodingParams{
		RID: ridPreview, Active: paused&(1<<SlotQ) == 0, MaxBitrate: previewMaxBitrate,
		MaxFramerate: previewFramerate, MaxPixels: previewMaxPixels,
	}
	return []EncodingParams{full, preview}
}

// fullLayerMax is the full layer's MaxBitrate under a share's cap in bit/s (02 §8.6): fullMaxBitrate, or the cap
// when that is set and lower.
func fullLayerMax(capBps int) int {
	if capBps > 0 && capBps < fullMaxBitrate {
		return capBps
	}
	return fullMaxBitrate
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
	return int(s.audioBps.Load())
}

// maxBitrate returns what a layer of the share in slot is meant to carry at most, in bit/s (02 §8.6): for video the
// encoding's MaxBitrate after the admin cap (the middle layer of M5 counts as the full one), for audio the preset's
// Opus bitrate. The retransmission budget of the layer's DownTracks follows it (02 §9.5). A viewer's NACK path asks
// for it with every NACK, so it takes no lock and makes nothing: each number is an atomic or a constant.
func (s *Share) maxBitrate(slot Slot) int64 {
	switch slot {
	case SlotAudio:
		return s.audioBps.Load()
	case SlotQ:
		return previewMaxBitrate
	default:
		return s.conn.sfu.fullMaxBps.Load()
	}
}

// setPreset changes the preset; false when the share has ended.
func (s *Share) setPreset(p Preset) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.preset = p
	s.audioBps.Store(int64(presetAudioBitrate(p)))
	return true
}

// attach makes l the share's Layer for its slot and returns the Layer it replaces, if any (a track whose SSRC
// changed). ok is false when the share has ended: nothing attaches to an ended share. The share's DownTracks choose
// their layer again before attach returns (retargetLocked), so a caller that attaches a track before it starts to
// read it loses none of its packets for any viewer. From here on the Layer counts into the SFU's totals (02 §13).
func (s *Share) attach(l *Layer) (replaced *Layer, ok bool) {
	l.countInto(&s.conn.sfu.counters)
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil, false
	}
	replaced = s.layers[l.slot]
	s.layers[l.slot] = l
	s.changedLocked()
	moved, _ := s.retargetLocked(l.kind, nil)
	s.mu.Unlock()
	for _, sub := range moved {
		sub.changed()
	}
	return replaced, true
}

// detach removes l from the share if it is still the Layer of its slot. The DownTracks that wanted it fall back to
// what 02 §10.1 gives them without it, or pause. For a viewer who was getting l, the stream is over at once, and
// goes on with a keyframe of the layer it falls back to: the publisher is asked for one here (02 §9.7), not only
// when that layer's next packet comes, which on a still screen can take a second.
//
// A live share that loses its last video layer is stalled at once (02 §5.3): its pub PC closed, or a new gen
// replaced it. The tracks of the next pub PC attach to the same share, and their first keyframe makes it live again.
func (s *Share) detach(l *Layer) {
	s.mu.Lock()
	var (
		moved   []*Subscription
		ask     []*Layer
		stalled bool
	)
	if s.layers[l.slot] == l {
		s.layers[l.slot] = nil
		s.changedLocked()
		moved, ask = s.retargetLocked(l.kind, l)
		if l.kind == webrtc.RTPCodecTypeVideo && !s.hasVideoLocked() {
			stalled = s.stallLocked()
		}
	}
	s.mu.Unlock()
	now := monoNow()
	for _, fallback := range ask {
		fallback.requestKeyframe(now)
	}
	for _, sub := range moved {
		sub.changed()
	}
	if stalled {
		s.conn.sfu.shareStateChanged(s, ShareStalled, "its video tracks have ended")
	}
}

// hasVideoLocked reports whether a video layer is attached. s.mu is held.
func (s *Share) hasVideoLocked() bool {
	return s.layers[SlotF] != nil || s.layers[SlotH] != nil || s.layers[SlotQ] != nil
}

// stall makes a live share stalled (02 §5.3): its media has stopped, because its pub PC hasn't been connected for
// 2 s (the ticker), has failed (Conn.stallShares), or is gone with every video track (detach). A share that is
// pending, stalled already or ended stays as it is. The hub hears of it at once, and ends a share that stays
// stalled for 30 s; the SFU only waits for the keyframe that makes the share live again.
func (s *Share) stall(cause string) {
	s.mu.Lock()
	stalled := s.stallLocked()
	s.mu.Unlock()
	if stalled {
		s.conn.sfu.shareStateChanged(s, ShareStalled, cause)
	}
}

// stallLocked is stall with s.mu held, without telling anybody: it reports whether the state changed.
func (s *Share) stallLocked() bool {
	if s.ended || s.state != ShareLive {
		return false
	}
	s.state = ShareStalled
	s.awaitsKeyframe.Store(true)
	s.stateChangedLocked()
	return true
}

// presentLocked returns the slot bits of the layers attached now: what 02 §10.1 chooses among. A layer is there from
// the moment its track arrives until the track ends, whether or not packets flow on it. s.mu is held.
func (s *Share) presentLocked() uint32 {
	var bits uint32
	for slot, l := range s.layers {
		if l != nil {
			bits |= 1 << slot
		}
	}
	return bits
}

// present returns the slot bits of the layers attached now.
func (s *Share) present() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.presentLocked()
}

// retargetLocked has the share's DownTracks of a kind choose their layer again, after a layer of that kind came or
// went (02 §10.1): a viewer who asked for high gets the full layer as soon as it is there, and the preview layer for
// as long as it is the only one. gone is the Layer that went, nil when one came: what a DownTrack was forwarding of
// it is over (a stream doesn't go on from a track that has ended), so its viewer gets nothing until the keyframe of
// the layer it has now.
//
// It returns the subscriptions for which something changed; the caller tells their Conns once it has released s.mu
// (Subscription.changed), which never blocks. After a layer went it also returns the layers to ask for a keyframe,
// each once: those that a DownTrack now waits for and whose viewer could get it (02 §9.7, a target change). A layer
// that came needs none: its track's first packet starts a keyframe. s.mu is held, and each DownTrack's lock is taken
// under it, the order of doc.go.
func (s *Share) retargetLocked(kind webrtc.RTPCodecType, gone *Layer) (moved []*Subscription, ask []*Layer) {
	present := s.presentLocked()
	for _, dt := range s.downTracks(kind) {
		dt.mu.Lock()
		over := gone != nil && dt.m.forwards(gone)
		if over {
			dt.m.restart()
		}
		slot, changed := dt.retargetLocked(present)
		waits := dt.m.waitingForKeyframe()
		dt.mu.Unlock()
		if !changed && !over {
			continue
		}
		moved = append(moved, dt.sub)
		if gone != nil && waits && kind == webrtc.RTPCodecTypeVideo && dt.binding.Load().sendable() {
			if l := s.layers[slot]; !slices.Contains(ask, l) {
				ask = append(ask, l)
			}
		}
	}
	return moved, ask
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
// loop calls it). The first one makes a pending share live, and a stalled share is live again with the first one
// that arrives while its pub PC is connected (02 §5.3); it reports whether this one made the share live. The profile
// of the newest keyframe is the share's. A Layer that was detached meanwhile changes nothing.
//
// "Connected" is what the publishing Conn's actor has seen of the PC (Conn.pubUp), the same view the stalled check
// goes by, so the two can't disagree and flap. A keyframe that overtakes the actor's look at a PC that has just
// reconnected isn't lost: the actor asks every layer of the PC for a keyframe when it sees the PC connected, and
// until one makes the share live, each further packet asks again (Layer.handleRTP).
//
// A new profile (the first keyframe, or the sharer's switch after a codec policy change, 02 §8.3) is news for every
// video subscriber: whether it has a payload type for the profile decides codec_mismatch, which its Conn's actor
// works out when it looks at the subscription (Subscription.changed).
func (s *Share) keyframe(l *Layer, profile ProfileKey) (wentLive bool) {
	s.mu.Lock()
	if s.ended || s.layers[l.slot] != l {
		s.mu.Unlock()
		return false
	}
	var viewers []*DownTrack
	if s.profile != profile {
		s.profile = profile
		s.changedLocked()
		viewers = s.downTracks(webrtc.RTPCodecTypeVideo)
	}
	switch {
	case s.state == SharePending:
		s.state, s.liveAt = ShareLive, time.Now()
		wentLive = true
	case s.state == ShareStalled && s.conn.pubUp.Load():
		s.state = ShareLive // liveAt stays: it is when the share first went live
		wentLive = true
	}
	if wentLive {
		s.awaitsKeyframe.Store(false)
		s.stateChangedLocked()
	}
	s.mu.Unlock()
	if wentLive {
		s.conn.sfu.shareStateChanged(s, ShareLive, "")
	}
	for _, dt := range viewers {
		dt.sub.changed()
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
	return shareParams(p.Preset, sh.up.capBps, 0, c.room.codecPolicy()), nil
}

// UpdateShare changes a share this Conn publishes and returns its new ShareParams: the encodings apply at once, the
// audio bitrate with the next publish offer's answer. The encodings are the share's as they stand: under the admin's
// cap as it is now, and with the layers paused that the sharer was told to pause (02 §11), so a sharer that applies
// them pauses or resumes nothing. It returns sfu.share_not_found for an unknown or ended share and sfu.not_owner for
// another Conn's.
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
	preset, ok := sh.currentPreset()
	if !ok {
		return ShareParams{}, newError(CodeShareNotFound, "the share ended")
	}
	// The sharer applies the cap it gets here: the uplink control has nothing more to tell it about the cap (a hint
	// with the reason admin follows only a change after this one).
	sh.up.capBps = c.sfu.currentLimits().shareCap()
	return shareParams(preset, sh.up.capBps, sh.up.paused, c.room.codecPolicy()), nil
}

// currentPreset returns the share's preset, and false once the share has ended.
func (s *Share) currentPreset() (Preset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preset, !s.ended
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
