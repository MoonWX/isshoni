// Package mediatest is the conformance suite of the media contract (docs/m2/01-client-core.md §6.4): Conformance
// asserts, against any media.Engine, the rules of §5.3 that every engine follows. It is a test helper, imported by
// _test files only: the fake engine runs it in CI on every OS, and the Windows engine on windows-2025 with the
// win-x64-ci DLL and on the owner's PC runner per probed encoder.
package mediatest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// Options says how Conformance starts the engine's sessions.
type Options struct {
	// Listed says that the source is a listed one (mon:…, test:pattern): Config.Listed is set, and check 8 starts the
	// same source again.
	Listed bool
	// TestTone asks for 04's test tone (no audio device needed) as Config.Audio = &media.AudioConfig{Mode:
	// media.AudioTestTone} (05 §13.1, the one carrier); otherwise the sessions capture audio as a share does.
	TestTone bool
	// Encoder is Config.Encoder: "" = automatic; "fake" = IM_ENC_FAKE (the win-x64-ci build).
	Encoder string
	// SkipBitrate skips check 5 with a message, for an engine whose bitrate does not follow its target: the fake
	// engine in Decodable mode (01 §6.4).
	SkipBitrate bool
}

// The bounds of 01 §5.3, §6.4 and §13.
const (
	firstPacketBound = 500 * time.Millisecond // a video layer's first packet after Start
	mediaWindow      = 5 * time.Second        // check 3
	minFPSChecked    = 5                      // check 3 skips a layer below this rate (a static desktop)
	keyframeSpacing  = 250 * time.Millisecond // 04 §6.4's forced-IDR spacing
	reinitBound      = 11 * time.Second       // a re-init encoder's deferred re-init (04 §9.5)
	bitrateWindow    = 4 * time.Second
	bitrateTarget    = 1_000_000
	inactiveGrace    = 100 * time.Millisecond // packets the engine had already made when the layer went inactive
	inactiveWindow   = time.Second
	resumeBound      = 500 * time.Millisecond
	reconfigureBound = 3 * time.Second // re-inits are spaced ≥ 1 s apart (04 §6.4)
	stopBound        = 200 * time.Millisecond
	nowTolerance     = 50 * time.Millisecond
	startTimeout     = 10 * time.Second
	readerBuffer     = 8192
)

// layers are the two layers of check 2: 1080p60 at 4 Mbit/s and 360p15 at 300 kbit/s.
func layers() []media.LayerConfig {
	return []media.LayerConfig{
		{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: 4_000_000},
		{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000},
	}
}

// Conformance runs the checks of 01 §6.4 against the engine newEngine returns (called once; the engine is closed when
// the test ends) with src, a source that needs no picker:
//
//  1. Info().ABI is media.ABIVersion; Probe reports a Tested encoder.
//  2. Start with two layers and audio: each layer's first packet is a Reinit keyframe within 500 ms, after an
//     EventLayer whose size fits the layer's budget and whose Profile is set.
//  3. 5 s of media: CaptureNS monotonic per stream; video frame intervals within ±50 % of 1/MaxFPS on average
//     (skipped for a layer below 5 fps); audio every 20 ms ± 5 ms on average; Annex B video.
//  4. RequestKeyframe("f"), 250 ms after f's last keyframe: one of f's next 2 frames is a keyframe.
//  5. SetBitrate("f", 1 Mbit/s): after the Reinit keyframe when f's encoder is not live, f's bitrate over 4 s is
//     within 50–150 % of the target (Options.SkipBitrate skips it).
//  6. SetLayerActive("f", false): no f packets for 1 s while q and audio go on; true: f resumes within 500 ms with a
//     keyframe that is not a Reinit.
//  7. Reconfigure{Profile: "42e0"}: f's and q's next Reinit keyframes carry a Constrained Baseline SPS (profile_idc 66,
//     constraint_set1), after EventLayers that report "42e0".
//  8. Stop while Next blocks: ErrStopped within 200 ms, also from a later Next; with Options.Listed the same source
//     starts again.
//  9. A second Start with audio while the session with audio runs is ErrBusy, and the session goes on.
//  10. Session.Now() is within 50 ms of a video packet's CaptureNS plus the time since it arrived; an audio packet's
//     CaptureNS is never after Now().
//
// Check 9 runs before check 8, while the first session still runs, and check 10 right after check 3. Checks 1 and 2
// stop the suite when they fail; the others run on regardless. The suite runs in real time, about 15 s.
func Conformance(t *testing.T, newEngine func(t *testing.T) media.Engine, src media.Source, o Options) {
	t.Helper()
	eng := newEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	r := &run{eng: eng, src: src, o: o}
	t.Cleanup(r.stop) // before the engine's Close: cleanups run last first
	for _, c := range []struct {
		name  string
		f     func(*testing.T)
		fatal bool
	}{
		{"1-info-probe", r.checkInfo, true},
		{"2-start", r.checkStart, true},
		{"3-media", r.checkMedia, false},
		{"10-now", r.checkNow, false},
		{"4-keyframe", r.checkKeyframe, false},
		{"5-bitrate", r.checkBitrate, false},
		{"6-layer-active", r.checkLayerActive, false},
		{"7-profile", r.checkProfile, false},
		{"9-busy", r.checkBusy, false},
		{"8-stop", r.checkStop, false},
	} {
		if !t.Run(c.name, c.f) && c.fatal {
			return
		}
	}
}

// run is one Conformance run: the engine, the running session and what the suite has seen of it.
type run struct {
	eng        media.Engine
	src        media.Source
	o          Options
	profile    string // the profile the sessions start in
	borderless bool
	cfg        media.Config

	sess    media.Session
	rd      *reader
	started time.Time              // when Start returned
	info    map[string]media.Event // the latest EventLayer per layer
	last    map[streamKey]int64    // the latest CaptureNS per stream
	lastKey map[string]int64       // the latest keyframe's CaptureNS per layer
	video   *rec                   // the latest video packet
	errs    map[string]struct{}    // reported invariant failures, once each
}

type streamKey struct {
	kind  media.Kind
	layer string
}

// rec is one return of Session.Next, as the reader saw it.
type rec struct {
	pkt media.Packet
	err error
	at  time.Time // when Next returned
	now int64     // Session.Now() right after Next returned
}

// reader is the one goroutine that calls a session's Next.
type reader struct {
	ch     chan rec
	done   chan struct{}
	cancel context.CancelFunc
}

func startReader(s media.Session) *reader {
	ctx, cancel := context.WithCancel(context.Background())
	r := &reader{ch: make(chan rec, readerBuffer), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(r.done)
		defer close(r.ch)
		for {
			p, err := s.Next(ctx)
			rc := rec{pkt: p, err: err, at: time.Now(), now: s.Now()}
			if ctx.Err() != nil {
				return
			}
			select {
			case r.ch <- rc:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

// stop stops the session and its reader.
func (r *run) stop() {
	if r.sess != nil {
		_ = r.sess.Stop()
	}
	if r.rd != nil {
		r.rd.cancel()
		for range r.rd.ch { // drain until the reader is gone
		}
		<-r.rd.done
		r.rd = nil
	}
}

// start starts a session with r.cfg and its reader.
func (r *run) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	s, err := r.eng.Start(ctx, r.cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.sess, r.started = s, time.Now()
	r.rd = startReader(s)
	r.info, r.last, r.lastKey = map[string]media.Event{}, map[streamKey]int64{}, map[string]int64{}
	r.errs, r.video = map[string]struct{}{}, nil
}

// until consumes the session's packets, checking each one's invariants, until f returns true for one (true) or d
// passes (false). Next's errors end the test.
func (r *run) until(t *testing.T, d time.Duration, f func(rc rec) bool) bool {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case rc, ok := <-r.rd.ch:
			switch {
			case !ok:
				t.Fatal("the session's reader is gone")
			case rc.err != nil:
				t.Fatalf("Next: %v", rc.err)
			}
			r.observe(t, rc)
			if f != nil && f(rc) {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

// drain consumes the packets the reader has queued, without waiting.
func (r *run) drain(t *testing.T) {
	t.Helper()
	for {
		select {
		case rc, ok := <-r.rd.ch:
			switch {
			case !ok:
				t.Fatal("the session's reader is gone")
			case rc.err != nil:
				t.Fatalf("Next: %v", rc.err)
			}
			r.observe(t, rc)
		default:
			return
		}
	}
}

// observe checks what holds for every packet (01 §5.1, §5.3) and keeps what later checks need.
func (r *run) observe(t *testing.T, rc rec) {
	t.Helper()
	p := rc.pkt
	switch p.Kind {
	case media.KindEvent:
		switch {
		case p.Event == nil:
			r.errorOnce(t, "event-nil", "an event packet without an Event")
			return
		case len(p.Data) != 0:
			r.errorOnce(t, "event-data", "an event packet with %d bytes of Data", len(p.Data))
		}
		if p.Event.Code == media.EventLayer {
			if !r.hasLayer(p.Event.Layer) {
				r.errorOnce(t, "event-layer", "an EventLayer of unknown layer %q", p.Event.Layer)
			}
			r.info[p.Event.Layer] = *p.Event
		}
		return
	case media.KindVideo:
		switch {
		case !r.hasLayer(p.Layer):
			r.errorOnce(t, "video-layer", "a video packet of unknown layer %q", p.Layer)
		case !hasStartCode(p.Data):
			r.errorOnce(t, "annexb-"+p.Layer, "layer %q: video Data at %d ns does not start with an Annex B start code",
				p.Layer, p.CaptureNS)
		case p.Reinit && !p.Keyframe:
			r.errorOnce(t, "reinit-"+p.Layer, "layer %q: a Reinit packet at %d ns that is not a keyframe", p.Layer,
				p.CaptureNS)
		}
		if p.Keyframe {
			r.lastKey[p.Layer] = p.CaptureNS
		}
		r.video = &rc
	case media.KindAudio:
		switch {
		case p.Layer != "" || p.Keyframe || len(p.Data) == 0:
			r.errorOnce(t, "audio", "an audio packet with Layer %q, Keyframe %v and %d bytes", p.Layer, p.Keyframe,
				len(p.Data))
		case p.CaptureNS > rc.now: // check 10's audio rule
			r.errorOnce(t, "audio-now", "an audio packet captured at %d ns, after Now() %d ns", p.CaptureNS, rc.now)
		}
	case media.KindPCM:
		r.errorOnce(t, "pcm", "a PCM packet (%d bytes) without AudioConfig.TapPCM", len(p.Data))
	default:
		r.errorOnce(t, "kind", "a packet of %v", p.Kind)
		return
	}
	k := streamKey{p.Kind, p.Layer}
	if last, ok := r.last[k]; ok && p.CaptureNS < last {
		r.errorOnce(t, "order-"+p.Kind.String()+p.Layer, "%v %q: CaptureNS goes back from %d to %d ns", p.Kind, p.Layer,
			last, p.CaptureNS)
	}
	if p.CaptureNS < 0 {
		r.errorOnce(t, "negative", "%v %q: negative CaptureNS %d", p.Kind, p.Layer, p.CaptureNS)
	}
	r.last[k] = p.CaptureNS
}

// errorOnce reports an invariant failure once per run and key, so a broken stream doesn't print a line per packet.
func (r *run) errorOnce(t *testing.T, key, format string, args ...any) {
	t.Helper()
	if _, done := r.errs[key]; done {
		return
	}
	r.errs[key] = struct{}{}
	t.Errorf(format, args...)
}

func (r *run) hasLayer(rid string) bool {
	return slices.ContainsFunc(r.cfg.Layers, func(l media.LayerConfig) bool { return l.RID == rid })
}

func (r *run) audio() *media.AudioConfig {
	if r.o.TestTone {
		return &media.AudioConfig{Mode: media.AudioTestTone}
	}
	return &media.AudioConfig{}
}

func (r *run) needSession(t *testing.T) {
	t.Helper()
	if r.sess == nil || r.rd == nil {
		t.Skip("no session (an earlier check failed)")
	}
}

// checkInfo is check 1. It also picks the profile the sessions start in: High when a usable encoder lists it.
func (r *run) checkInfo(t *testing.T) {
	info := r.eng.Info()
	if info.ABI != media.ABIVersion {
		t.Errorf("Info().ABI = %d, want %d", info.ABI, media.ABIVersion)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	p, err := r.eng.Probe(ctx, false)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var usable []media.EncoderInfo
	for _, e := range p.Encoders {
		if e.Tested {
			usable = append(usable, e)
		}
	}
	if len(usable) == 0 {
		t.Fatalf("Probe reports no Tested encoder among %d", len(p.Encoders))
	}
	if r.o.Encoder != "" {
		if i := slices.IndexFunc(usable, func(e media.EncoderInfo) bool { return e.Name == r.o.Encoder }); i >= 0 {
			usable = usable[i : i+1]
		}
	}
	r.profile = "42e0"
	if slices.ContainsFunc(usable, func(e media.EncoderInfo) bool { return slices.Contains(e.Profiles, "6400") }) {
		r.profile = "6400"
	}
	r.borderless = p.Capture.Borderless
	t.Logf("engine %q, ABI %d, build %q: %d encoders, starting in profile %s", info.Name, info.ABI, info.Build,
		len(p.Encoders), r.profile)
}

// checkStart is check 2.
func (r *run) checkStart(t *testing.T) {
	r.cfg = media.Config{
		Source: r.src.ID, Listed: r.o.Listed, Encoder: r.o.Encoder, Profile: r.profile, Preset: "auto",
		Layers: layers(), Audio: r.audio(), Borderless: r.borderless,
	}
	r.start(t)
	first := map[string]rec{}
	events := map[string]int{} // EventLayers before the layer's first packet
	r.until(t, 2*time.Second, func(rc rec) bool {
		p := rc.pkt
		switch {
		case p.Kind == media.KindEvent && p.Event.Code == media.EventLayer:
			if _, ok := first[p.Event.Layer]; !ok {
				events[p.Event.Layer]++
			}
		case p.Kind == media.KindVideo:
			if _, ok := first[p.Layer]; !ok {
				first[p.Layer] = rc
			}
		}
		return len(first) == len(r.cfg.Layers)
	})
	for _, l := range r.cfg.Layers {
		rc, ok := first[l.RID]
		if !ok {
			t.Errorf("layer %q: no packet within 2 s of Start", l.RID)
			continue
		}
		if d := rc.at.Sub(r.started); d > firstPacketBound {
			t.Errorf("layer %q: first packet %v after Start, want ≤ %v", l.RID, d, firstPacketBound)
		}
		if !rc.pkt.Keyframe || !rc.pkt.Reinit {
			t.Errorf("layer %q: first packet has Keyframe %v and Reinit %v, want both", l.RID, rc.pkt.Keyframe,
				rc.pkt.Reinit)
		}
		ev, ok := r.info[l.RID]
		if events[l.RID] == 0 || !ok {
			t.Errorf("layer %q: no EventLayer before its first packet", l.RID)
			continue
		}
		switch {
		case ev.Width <= 0 || ev.Height <= 0 || ev.Width*ev.Height > l.MaxPixels:
			t.Errorf("layer %q: EventLayer size %dx%d, want within %d pixels", l.RID, ev.Width, ev.Height, l.MaxPixels)
		case ev.FPS < 1 || ev.FPS > l.MaxFPS:
			t.Errorf("layer %q: EventLayer FPS %d, want 1–%d", l.RID, ev.FPS, l.MaxFPS)
		case ev.Profile != "6400" && ev.Profile != "42e0":
			t.Errorf("layer %q: EventLayer Profile %q, want 6400 or 42e0", l.RID, ev.Profile)
		}
		t.Logf("layer %s: %dx%d@%d, encoder %q (HW %v, live bitrate %v), profile %s; first packet after %v", l.RID,
			ev.Width, ev.Height, ev.FPS, ev.Encoder, ev.HW, ev.LiveBitrate, ev.Profile, rc.at.Sub(r.started))
	}
	if t.Failed() {
		t.FailNow()
	}
}

// checkMedia is check 3.
func (r *run) checkMedia(t *testing.T) {
	r.needSession(t)
	type acc struct {
		n           int
		first, last int64
	}
	accs := map[streamKey]*acc{}
	r.until(t, mediaWindow, func(rc rec) bool {
		p := rc.pkt
		if p.Kind != media.KindVideo && p.Kind != media.KindAudio {
			return false
		}
		k := streamKey{p.Kind, p.Layer}
		a := accs[k]
		if a == nil {
			a = &acc{first: p.CaptureNS}
			accs[k] = a
		}
		a.n++
		a.last = p.CaptureNS
		return false
	})
	for _, l := range r.cfg.Layers {
		a := accs[streamKey{media.KindVideo, l.RID}]
		if a == nil || a.n < 2 {
			t.Errorf("layer %q: fewer than 2 frames in %v", l.RID, mediaWindow)
			continue
		}
		span := time.Duration(a.last - a.first)
		fps := float64(a.n-1) / span.Seconds()
		if fps < minFPSChecked {
			t.Logf("layer %s: %.1f fps, below %d fps (static content?): frame interval check skipped", l.RID, fps,
				minFPSChecked)
			continue
		}
		avg, want := span/time.Duration(a.n-1), time.Second/time.Duration(l.MaxFPS)
		if avg < want/2 || avg > want*3/2 {
			t.Errorf("layer %q: average frame interval %v, want %v ± 50 %%", l.RID, avg, want)
		}
		t.Logf("layer %s: %d frames, %.1f fps", l.RID, a.n, fps)
	}
	if r.cfg.Audio != nil {
		a := accs[streamKey{media.KindAudio, ""}]
		if a == nil || a.n < int(mediaWindow/(25*time.Millisecond))-10 {
			n := 0
			if a != nil {
				n = a.n
			}
			t.Errorf("%d audio packets in %v, want one every 20 ms", n, mediaWindow)
			return
		}
		avg := time.Duration(a.last-a.first) / time.Duration(a.n-1)
		if avg < 15*time.Millisecond || avg > 25*time.Millisecond {
			t.Errorf("audio: average packet interval %v, want 20 ms ± 5 ms", avg)
		}
		t.Logf("audio: %d packets, every %v", a.n, avg)
	}
}

// checkNow is check 10's video rule (observe checks the audio rule on every packet).
func (r *run) checkNow(t *testing.T) {
	r.needSession(t)
	var n int
	r.until(t, time.Second, func(rc rec) bool {
		if rc.pkt.Kind != media.KindVideo {
			return false
		}
		n++
		if d := time.Duration(rc.now - rc.pkt.CaptureNS); d < -nowTolerance || d > nowTolerance {
			r.errorOnce(t, "now-arrival", "layer %q: Now() at arrival is %v from CaptureNS, want within %v",
				rc.pkt.Layer, d, nowTolerance)
		}
		return false
	})
	if n == 0 {
		t.Fatal("no video packet in 1 s")
	}
	for range 5 {
		time.Sleep(50 * time.Millisecond)
		r.drain(t)
		v := r.video
		now := r.sess.Now()
		want := v.pkt.CaptureNS + int64(time.Since(v.at))
		if d := time.Duration(now - want); d < -nowTolerance || d > nowTolerance {
			t.Errorf("Now() is %v from the latest video packet's CaptureNS plus the time since it arrived, want within %v",
				d, nowTolerance)
		}
	}
}

// checkKeyframe is check 4.
func (r *run) checkKeyframe(t *testing.T) {
	r.needSession(t)
	quiet := func() bool {
		k, ok := r.lastKey["f"]
		return ok && time.Duration(r.sess.Now()-k) >= keyframeSpacing+30*time.Millisecond
	}
	if !quiet() && !r.until(t, 2*time.Second, func(rec) bool { return quiet() }) && !quiet() {
		t.Fatal("no keyframe on f to wait from")
	}
	asked := time.Now()
	r.sess.RequestKeyframe("f")
	frames, key := 0, false
	r.until(t, 2*time.Second, func(rc rec) bool {
		if rc.pkt.Kind != media.KindVideo || rc.pkt.Layer != "f" || !rc.at.After(asked) {
			return false
		}
		frames++
		key = rc.pkt.Keyframe
		return key || frames == 2
	})
	if !key {
		t.Errorf("no keyframe among f's %d frames after RequestKeyframe, want one within 2 frames", frames)
	}
}

// checkBitrate is check 5.
func (r *run) checkBitrate(t *testing.T) {
	r.needSession(t)
	if r.o.SkipBitrate {
		t.Skip("skipped: this engine's bitrate does not follow its target (Options.SkipBitrate)")
	}
	live := r.info["f"].LiveBitrate
	set := time.Now()
	if err := r.sess.SetBitrate("f", bitrateTarget); err != nil {
		t.Fatalf("SetBitrate(f, %d): %v", bitrateTarget, err)
	}
	start, bytes := set, 0
	if !live {
		var reinit rec
		if !r.until(t, reinitBound, func(rc rec) bool {
			if rc.pkt.Kind == media.KindVideo && rc.pkt.Layer == "f" && rc.pkt.Reinit && rc.at.After(set) {
				reinit = rc
				return true
			}
			return false
		}) {
			t.Fatalf("f's encoder is not live, and no Reinit keyframe came within %v of SetBitrate", reinitBound)
		}
		start, bytes = reinit.at, len(reinit.pkt.Data)
		t.Logf("f re-initialized %v after SetBitrate", reinit.at.Sub(set))
	}
	end := start.Add(bitrateWindow)
	r.until(t, time.Until(end)+100*time.Millisecond, func(rc rec) bool {
		if rc.at.After(end) {
			return true
		}
		if rc.pkt.Kind == media.KindVideo && rc.pkt.Layer == "f" && rc.at.After(start) {
			bytes += len(rc.pkt.Data)
		}
		return false
	})
	bps := float64(bytes) * 8 / bitrateWindow.Seconds()
	if bps < bitrateTarget/2 || bps > bitrateTarget*3/2 {
		t.Errorf("f: %.0f bit/s over %v after SetBitrate(%d), want 50–150 %% of the target", bps, bitrateWindow,
			bitrateTarget)
	}
	t.Logf("f: %.0f bit/s over %v (target %d, live %v)", bps, bitrateWindow, bitrateTarget, live)
}

// checkLayerActive is check 6.
func (r *run) checkLayerActive(t *testing.T) {
	r.needSession(t)
	off := time.Now()
	if err := r.sess.SetLayerActive("f", false); err != nil {
		t.Fatalf("SetLayerActive(f, false): %v", err)
	}
	from := off.Add(inactiveGrace)
	end := from.Add(inactiveWindow)
	var f, q, audio int
	r.until(t, time.Until(end)+100*time.Millisecond, func(rc rec) bool {
		if rc.at.After(end) {
			return true
		}
		if rc.at.Before(from) {
			return false
		}
		switch {
		case rc.pkt.Kind == media.KindVideo && rc.pkt.Layer == "f":
			f++
		case rc.pkt.Kind == media.KindVideo:
			q++
		case rc.pkt.Kind == media.KindAudio:
			audio++
		}
		return false
	})
	if f > 0 {
		t.Errorf("%d f packets in the %v after SetLayerActive(f, false), want none", f, inactiveWindow)
	}
	if q < 3 {
		t.Errorf("%d q packets while f was inactive, want q to go on", q)
	}
	if r.cfg.Audio != nil && audio < 25 {
		t.Errorf("%d audio packets in %v while f was inactive, want audio to go on", audio, inactiveWindow)
	}
	on := time.Now()
	if err := r.sess.SetLayerActive("f", true); err != nil {
		t.Fatalf("SetLayerActive(f, true): %v", err)
	}
	var first rec
	if !r.until(t, 2*resumeBound, func(rc rec) bool {
		if rc.pkt.Kind == media.KindVideo && rc.pkt.Layer == "f" && rc.at.After(on) {
			first = rc
			return true
		}
		return false
	}) {
		t.Fatalf("f did not resume within %v of SetLayerActive(f, true)", 2*resumeBound)
	}
	if d := first.at.Sub(on); d > resumeBound {
		t.Errorf("f resumed %v after SetLayerActive(f, true), want ≤ %v", d, resumeBound)
	}
	if !first.pkt.Keyframe || first.pkt.Reinit {
		t.Errorf("f resumed with Keyframe %v and Reinit %v, want a keyframe that is not a Reinit", first.pkt.Keyframe,
			first.pkt.Reinit)
	}
}

// checkProfile is check 7.
func (r *run) checkProfile(t *testing.T) {
	r.needSession(t)
	asked := time.Now()
	if err := r.sess.Reconfigure(media.Reconfig{Profile: "42e0"}); err != nil {
		t.Fatalf("Reconfigure{Profile: 42e0}: %v", err)
	}
	events := map[string]int{} // EventLayers after Reconfigure, before the layer's Reinit keyframe
	done := map[string]bool{}
	ok := r.until(t, reconfigureBound, func(rc rec) bool {
		p := rc.pkt
		if !rc.at.After(asked) {
			return false
		}
		switch {
		case p.Kind == media.KindEvent && p.Event.Code == media.EventLayer:
			if p.Event.Profile != "42e0" {
				t.Errorf("layer %q: EventLayer after Reconfigure reports profile %q, want 42e0", p.Event.Layer,
					p.Event.Profile)
			}
			if !done[p.Event.Layer] {
				events[p.Event.Layer]++
			}
		case p.Kind == media.KindVideo && p.Reinit && !done[p.Layer]:
			done[p.Layer] = true
			idc, constraints, found := spsProfile(p.Data)
			switch {
			case !p.Keyframe || !found:
				t.Errorf("layer %q: the Reinit packet after Reconfigure has Keyframe %v and SPS %v", p.Layer,
					p.Keyframe, found)
			case idc != 66 || constraints&0x40 == 0:
				t.Errorf("layer %q: SPS after Reconfigure has profile_idc %d and constraint flags %#02x, want 66 with "+
					"constraint_set1", p.Layer, idc, constraints)
			}
			if events[p.Layer] == 0 {
				t.Errorf("layer %q: no EventLayer before the Reinit keyframe after Reconfigure", p.Layer)
			}
		}
		return len(done) == len(r.cfg.Layers)
	})
	if !ok {
		var missing []string
		for _, l := range r.cfg.Layers {
			if !done[l.RID] {
				missing = append(missing, l.RID)
			}
		}
		t.Errorf("no Reinit keyframe on %v within %v of Reconfigure", missing, reconfigureBound)
	}
}

// checkBusy is check 9: a second session with audio while one runs.
func (r *run) checkBusy(t *testing.T) {
	r.needSession(t)
	cfg := r.cfg
	if !r.o.Listed { // a picked source would be consumed: ask for an audio-only session instead
		cfg = media.Config{Audio: r.audio()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	s, err := r.eng.Start(ctx, cfg)
	if err == nil {
		_ = s.Stop()
		t.Fatal("a second Start with audio succeeded while a session with audio runs")
	}
	if !errors.Is(err, media.ErrBusy) {
		t.Errorf("second Start: %v, want ErrBusy", err)
	}
	if !r.until(t, time.Second, func(rc rec) bool { return rc.pkt.Kind != media.KindEvent }) {
		t.Error("the running session sent nothing in the second after the refused Start")
	}
}

// checkStop is check 8.
func (r *run) checkStop(t *testing.T) {
	r.needSession(t)
	r.drain(t)
	stopped := time.Now()
	if err := r.sess.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	var end rec
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
wait:
	for {
		select {
		case rc, ok := <-r.rd.ch:
			if !ok {
				t.Fatal("the session's reader is gone")
			}
			if rc.err != nil {
				end = rc
				break wait
			}
			r.observe(t, rc)
		case <-timer.C:
			t.Fatal("Next did not return within 2 s of Stop")
		}
	}
	if !errors.Is(end.err, media.ErrStopped) {
		t.Errorf("Next after Stop: %v, want ErrStopped", end.err)
	}
	if d := end.at.Sub(stopped); d > stopBound {
		t.Errorf("Next returned %v after Stop, want ≤ %v", d, stopBound)
	}
	<-r.rd.done
	r.rd = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.sess.Next(ctx); !errors.Is(err, media.ErrStopped) {
		t.Errorf("a later Next: %v, want ErrStopped", err)
	}
	if err := r.sess.Stop(); err != nil {
		t.Errorf("a second Stop: %v, want nil", err)
	}
	if !r.o.Listed {
		t.Log("not starting again: the source is not listed (a picked source is single use)")
		return
	}
	r.start(t)
	if !r.until(t, 2*firstPacketBound, func(rc rec) bool { return rc.pkt.Kind == media.KindVideo }) {
		t.Errorf("the listed source started again, but no video came within %v", 2*firstPacketBound)
	}
}

// hasStartCode reports whether b starts with an Annex B start code (00 00 01 or 00 00 00 01).
func hasStartCode(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0, 0, 1}) || bytes.HasPrefix(b, []byte{0, 0, 0, 1})
}

// nalUnits splits an Annex B access unit into its NAL units, without their start codes (and without the zero byte
// of a 4-byte start code).
func nalUnits(au []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(au); {
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			i++
			continue
		}
		if start >= 0 {
			end := i
			for end > start && au[end-1] == 0 {
				end--
			}
			out = append(out, au[start:end])
		}
		i += 3
		start = i
	}
	if start >= 0 && start < len(au) {
		out = append(out, au[start:])
	}
	return out
}

// spsProfile returns profile_idc and the constraint flags byte of an access unit's first SPS.
func spsProfile(au []byte) (idc, constraints byte, ok bool) {
	for _, n := range nalUnits(au) {
		if len(n) < 3 || n[0]&0x1f != 7 {
			continue
		}
		rbsp := unescape(n[1:min(len(n), 16)])
		if len(rbsp) >= 2 {
			return rbsp[0], rbsp[1], true
		}
	}
	return 0, 0, false
}

// unescape removes H.264's emulation prevention bytes (7.4.1).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
