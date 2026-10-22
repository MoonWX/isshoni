package fake

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// EngineConfig configures an Engine (docs/m2/01-client-core.md §6.2). The zero value is valid: Synthetic video of a
// 1920×1080 screen, the default probe, one listed monitor and a picker that picks a window at once.
type EngineConfig struct {
	Mode Mode // Synthetic (default) or Decodable
	// SourceSize is the fake screen, default 1920×1080 (both sides must be positive, else the default applies). A
	// session's layers are scaled from its source's size (a picked or listed Source's Width and Height, which default
	// to SourceSize) by their MaxPixels: aspect ratio kept, even sides, never upscaled.
	SourceSize [2]int
	// Probe is what Probe returns. Each zero part takes its default: OS {runtime.GOOS, Supported}; one encoder {Name
	// "fake", Adapter −1, Tested, LiveBitrate, Profiles ["6400", "42e0"] (Decodable: ["42e0"])}; Capture {Picker,
	// Borderless, MaxFPS 60}; Audio media.AudioCapsFromFlags(0xF) (all four bits, Backend "process-loopback"). A
	// session's layers run on the first Tested encoder (or the one Config.Encoder names); with LiveBitrate false,
	// every SetBitrate that changes a layer's target re-initializes the layer (a Reinit keyframe), so tests see what a
	// re-init encoder costs.
	Probe media.Probe
	// Sources is what Sources returns, the listed sources: default one monitor "mon:fake" of SourceSize with Capture
	// "test". Starting one needs Config.Listed.
	Sources []media.Source
	// PickResult is what Pick returns, under a fresh single-use "pick:<n>" ID each time; nil = a window "Fake window"
	// of SourceSize with Capture "wgc" and OSBorder.
	PickResult *media.Source
	Cancel     bool // Pick returns ErrCancelled: the user closed the picker
	// PickDelay is how long the fake user takes to pick (or, with Cancel, to close the picker); default at once. A
	// PickDelay longer than the request's Timeout makes Pick time out (ErrCancelled), and a ctx that ends first ends
	// Pick: how tests exercise the picker's timeout and ctx.
	PickDelay time.Duration
	// AudioScenario names the script of the Engine's media.AudioController (05 §13.3, audio_ctl.go): "normal" (the
	// default for ""), "degraded", "degrade-live", "old-windows", "no-device", "fast-path", "shared-mic" or "many".
	// With any other name every AudioController call is CodeInvalid. The desktop app's dev build reads it from
	// ISSHONI_FAKE_AUDIO.
	AudioScenario string
	Seed          uint64        // Synthetic's filler bytes and frame-size jitter (Config.Seed)
	FlashEvery    time.Duration // as Config.FlashEvery: flash frames and beeps at k·FlashEvery
}

// Engine is the fake media engine (docs/m2/01-client-core.md §6.2): media.Engine on top of Source, and media.AudioController
// (audio_ctl.go). It enforces the native engine's source rules, so code under test takes the app's real path: a
// pick is single use, expires 5 min after the pick, at most 4 unused picks are kept (a fifth evicts the oldest), a
// failed Start keeps it, and a listed source needs Config.Listed. It runs one Session at a time. Its methods are safe
// for concurrent use.
//
// It is what the app runs with on macOS and Linux in M2 (cmd/isshoni-desktop's dev and e2e builds) and the engine of
// every test that needs no hardware; no release build links it.
type Engine struct {
	mode       Mode
	sourceSize [2]int
	probe      media.Probe
	sources    []media.Source
	pickResult media.Source
	cancel     bool
	pickDelay  time.Duration
	seed       uint64
	flashEvery time.Duration

	done chan struct{} // closed by Close: ends a Pick that waits out PickDelay, and a SoundCheck

	audio audioState // the AudioController's script and state (audio_ctl.go); its own mutex

	mu       sync.Mutex // guards the fields below
	closed   bool
	picking  bool
	lastPick int
	picks    []pick // unused picks, oldest first
	sess     *Session
}

// pick is one unused pick: its Source (with its ID) and when it expires.
type pick struct {
	src     media.Source
	expires time.Time
}

var _ media.Engine = (*Engine)(nil)

// Defaults and the native engine's limits (04 §6.4, §7.2) that the fake enforces.
const (
	// DefaultSourceWidth and DefaultSourceHeight are the size of the fake screen without EngineConfig.SourceSize.
	DefaultSourceWidth  = 1920
	DefaultSourceHeight = 1080
	// ListedMonitorID is the ID of the default listed monitor.
	ListedMonitorID = "mon:fake"
	// EncoderName is the default probe's encoder (the native IM_ENC_FAKE's token).
	EncoderName = "fake"

	pickPrefix         = "pick:"
	pickLifetime       = 5 * time.Minute
	maxUnusedPicks     = 4
	defaultPickTimeout = 120 * time.Second
	maxPickTimeout     = 600 * time.Second
	maxSessionLayers   = 2 // M2's layers per session (04 §6.4)
	maxRules           = 512
)

// encoderTokens are the probe tokens Config.Encoder may name (04 §5.6).
var encoderTokens = []string{"nvenc", "qsv", "amf", "mf-hw", "mf-sw", "openh264", "fake"}

// NewEngine returns an Engine. It starts nothing.
func NewEngine(cfg EngineConfig) *Engine {
	e := &Engine{
		mode: cfg.Mode, sourceSize: cfg.SourceSize, cancel: cfg.Cancel, pickDelay: cfg.PickDelay,
		seed: cfg.Seed, flashEvery: cfg.FlashEvery, done: make(chan struct{}),
	}
	e.audio.init(cfg.AudioScenario)
	if e.sourceSize[0] <= 0 || e.sourceSize[1] <= 0 {
		e.sourceSize = [2]int{DefaultSourceWidth, DefaultSourceHeight}
	}
	e.probe = withProbeDefaults(cfg.Probe, cfg.Mode)
	if cfg.Sources != nil {
		e.sources = slices.Clone(cfg.Sources)
	} else {
		e.sources = []media.Source{{
			ID: ListedMonitorID, Kind: media.SourceScreen, Name: "Fake monitor",
			Width: e.sourceSize[0], Height: e.sourceSize[1], MonitorID: ListedMonitorID, Capture: "test",
		}}
	}
	if cfg.PickResult != nil {
		e.pickResult = *cfg.PickResult
	} else {
		e.pickResult = media.Source{
			Kind: media.SourceWindow, Name: "Fake window", Width: e.sourceSize[0], Height: e.sourceSize[1],
			Capture: "wgc", OSBorder: true,
		}
	}
	return e
}

// withProbeDefaults fills each zero part of p with the default probe's.
func withProbeDefaults(p media.Probe, mode Mode) media.Probe {
	p = cloneProbe(p)
	if p.OS == (media.OSInfo{}) {
		p.OS = media.OSInfo{Name: runtime.GOOS, Supported: true}
	}
	if p.Encoders == nil {
		profiles := []string{"6400", DecodableProfile}
		if mode == Decodable {
			profiles = []string{DecodableProfile}
		}
		p.Encoders = []media.EncoderInfo{{
			Name: EncoderName, Adapter: -1, Tested: true, LiveBitrate: true, Profiles: profiles,
		}}
	}
	if p.Capture == (media.CaptureCaps{}) {
		p.Capture = media.CaptureCaps{Picker: true, Borderless: true, MaxFPS: 60}
	}
	if p.Audio == (media.AudioCaps{}) {
		p.Audio = media.AudioCapsFromFlags(0xF)
	}
	return p
}

// cloneProbe returns a deep copy of p.
func cloneProbe(p media.Probe) media.Probe {
	if p.Encoders != nil {
		p.Encoders = slices.Clone(p.Encoders)
		for i := range p.Encoders {
			p.Encoders[i].Profiles = slices.Clone(p.Encoders[i].Profiles)
		}
	}
	return p
}

// Info implements media.Engine: {Name "fake", ABI media.ABIVersion}.
func (e *Engine) Info() media.Info {
	return media.Info{Name: "fake", ABI: media.ABIVersion}
}

// Probe implements media.Engine: EngineConfig.Probe with its defaults (a copy). refresh changes nothing.
func (e *Engine) Probe(ctx context.Context, refresh bool) (media.Probe, error) {
	if err := ctx.Err(); err != nil {
		return media.Probe{}, err
	}
	if err := e.checkOpen("probe"); err != nil {
		return media.Probe{}, err
	}
	return cloneProbe(e.probe), nil
}

// Sources implements media.Engine: EngineConfig.Sources (a copy).
func (e *Engine) Sources(ctx context.Context) ([]media.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen("sources"); err != nil {
		return nil, err
	}
	return slices.Clone(e.sources), nil
}

// Pick implements media.Engine: after EngineConfig.PickDelay it returns EngineConfig.PickResult under a fresh
// "pick:<n>" ID, or ErrCancelled with EngineConfig.Cancel. When req.Timeout (0 = 120 s, at most 600 s) passes first it
// returns ErrCancelled, as the native picker does when it times out. When ctx ends first it returns an error that
// matches both media.ErrCancelled (the native binding's im_cancel_pick) and ctx's error; when the Engine is closed
// first it returns a stopped error (Reason "closed") at once. One pick runs at a time: a second concurrent Pick is
// ErrBusy with Reason "pick_busy" (IM_ERR_LIMIT, 04 §5.7: the picker is already open).
func (e *Engine) Pick(ctx context.Context, req media.PickRequest) (media.Source, error) {
	const op = "pick"
	timeout := req.Timeout
	switch {
	case timeout < 0 || timeout > maxPickTimeout:
		return media.Source{}, errorf(media.CodeInvalid, op, "", "timeout %v not in 0–%v", timeout, maxPickTimeout)
	case timeout == 0:
		timeout = defaultPickTimeout
	}
	e.mu.Lock()
	switch {
	case e.closed:
		e.mu.Unlock()
		return media.Source{}, errClosed(op)
	case e.picking:
		e.mu.Unlock()
		return media.Source{}, errorf(media.CodeBusy, op, "pick_busy", "another pick is running")
	}
	e.picking = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.picking = false
		e.mu.Unlock()
	}()

	if err := ctx.Err(); err != nil {
		return media.Source{}, pickCtxError(err)
	}
	if e.pickDelay > 0 {
		t := time.NewTimer(min(e.pickDelay, timeout))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return media.Source{}, pickCtxError(ctx.Err())
		case <-e.done:
			t.Stop()
			return media.Source{}, errClosed(op)
		}
		if e.pickDelay > timeout {
			return media.Source{}, errorf(media.CodeCancelled, op, "", "the picker timed out after %v", timeout)
		}
	}
	if e.cancel {
		return media.Source{}, &media.Error{Code: media.CodeCancelled, Op: op}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return media.Source{}, errClosed(op)
	}
	now := time.Now()
	e.prunePicksLocked(now)
	if len(e.picks) == maxUnusedPicks {
		e.picks = e.picks[1:] // a fifth pick evicts the oldest (04 §7.2)
	}
	e.lastPick++
	src := e.pickResult
	src.ID = fmt.Sprintf("%s%d", pickPrefix, e.lastPick)
	e.picks = append(e.picks, pick{src: src, expires: now.Add(pickLifetime)})
	return src, nil
}

// pickCtxError is Pick's error when ctx ended: cancelled, as the binding's im_cancel_pick answers, and ctx's error.
func pickCtxError(ctxErr error) error {
	return fmt.Errorf("%w: %w", &media.Error{Code: media.CodeCancelled, Op: "pick"}, ctxErr)
}

// Release implements media.Engine: it forgets an unused pick. Other ids are a no-op.
func (e *Engine) Release(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.picks = slices.DeleteFunc(e.picks, func(p pick) bool { return p.src.ID == id })
}

// prunePicksLocked forgets the expired picks.
func (e *Engine) prunePicksLocked(now time.Time) {
	e.picks = slices.DeleteFunc(e.picks, func(p pick) bool { return !now.Before(p.expires) })
}

// Start implements media.Engine. It checks cfg as the native engine does (unknown values, ranges and the source rules
// are CodeInvalid, a consumed, expired, evicted or unknown pick is source_gone with Reason "pick_expired", a second
// session is ErrBusy with Reason "session_limit" (IM_ERR_LIMIT, 04 §5.7), a forced encoder the probe lacks is
// no_encoder) and starts a Session: one Source layer per cfg.Layers entry, audio when cfg.Audio is set. A successful
// Start consumes a picked source; a failed one keeps it. A session with audio replaces the AudioController's rule set
// with cfg.Audio.Exclusions, as im_start does with its exclusions (05 §4.2), and runs its watcher while it lives.
func (e *Engine) Start(ctx context.Context, cfg media.Config) (media.Session, error) {
	// A session with audio holds the AudioController's lock from before the session exists until the controller
	// knows it, so no SetAudioMuted or SetAudioMode finds the session (Engine.Session) before the report follows it.
	audio := cfg.Audio != nil && e.audio.sc != nil
	if audio {
		e.audio.mu.Lock()
		defer e.audio.mu.Unlock()
	}
	s, err := e.start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if audio {
		e.audioSessionStartedLocked(s, cfg.Audio.Exclusions)
	}
	return s, nil
}

// start is Start's checks and new Session, under e.mu. Start holds audioState.mu around it for a session with audio
// (audio_ctl.go's lock order: audioState.mu, then Engine.mu).
func (e *Engine) start(ctx context.Context, cfg media.Config) (*Session, error) {
	const op = "start"
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
		return nil, errClosed(op)
	case e.sess != nil:
		return nil, errorf(media.CodeBusy, op, "session_limit", "a session is running (the fake runs one at a time)")
	}
	plan, err := e.planLocked(cfg)
	if err != nil {
		return nil, err
	}
	s, err := newSession(e, plan)
	if err != nil {
		return nil, err
	}
	if plan.pickID != "" {
		e.picks = slices.DeleteFunc(e.picks, func(p pick) bool { return p.src.ID == plan.pickID })
	}
	e.sess = s
	return s, nil
}

// Close implements media.Engine: it stops the running Session and ends a Pick that waits. Afterwards Probe, Sources,
// Pick and Start return a stopped error (Reason "closed"). Idempotent; it always returns nil.
func (e *Engine) Close() error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.done)
	}
	e.picks = nil
	s := e.sess
	e.mu.Unlock()
	if s != nil {
		_ = s.Stop()
	}
	return nil
}

// Session returns the running session, or nil: a test's handle on its controls (Fail, Emit, Calls).
func (e *Engine) Session() *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess
}

// ended forgets s when it is the running session.
func (e *Engine) ended(s *Session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == s {
		e.sess = nil
	}
}

func (e *Engine) checkOpen(op string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errClosed(op)
	}
	return nil
}

// plan is a checked media.Config: what newSession starts.
type plan struct {
	pickID     string // the pick Start consumes, or ""
	size       [2]int // the source's size
	encoder    media.EncoderInfo
	profile    string // the profile the session runs
	preset     string
	layers     []layerPlan
	audio      *media.AudioConfig // nil = no audio; BitrateBPS filled in
	borderless bool
}

// layerPlan is one checked layer: its config and the scaled size.
type layerPlan struct {
	cfg           media.LayerConfig
	width, height int
}

// planLocked checks cfg against the native engine's rules (01 §5.1, 04 §6.4, §7.2).
func (e *Engine) planLocked(cfg media.Config) (plan, error) {
	const op = "start"
	p := plan{borderless: cfg.Borderless}
	// The source.
	switch {
	case cfg.Source == "" && (len(cfg.Layers) > 0 || cfg.Audio == nil):
		return p, errorf(media.CodeInvalid, op, "", "no Source: only an audio-only session (no layers, Audio set)")
	case cfg.Source != "" && len(cfg.Layers) == 0:
		return p, errorf(media.CodeInvalid, op, "", "a session with a Source needs a layer")
	case cfg.Source != "":
		src, err := e.resolveLocked(cfg.Source, cfg.Listed)
		if err != nil {
			return p, err
		}
		p.size = e.sourceSize
		if src.Width > 0 && src.Height > 0 {
			p.size = [2]int{src.Width, src.Height}
		}
		if strings.HasPrefix(src.ID, pickPrefix) {
			p.pickID = src.ID
		}
	}
	// Audio.
	if cfg.Audio != nil {
		a, err := checkAudio(op, *cfg.Audio)
		if err != nil {
			return p, err
		}
		p.audio = &a
	}
	if len(cfg.Layers) == 0 {
		return p, nil
	}
	// Video: encoder, profile, preset, layers.
	if len(cfg.Layers) > maxSessionLayers {
		return p, errorf(media.CodeInvalid, op, "", "%d layers, at most %d", len(cfg.Layers), maxSessionLayers)
	}
	enc, err := e.encoderFor(op, cfg.Encoder)
	if err != nil {
		return p, err
	}
	p.encoder = enc
	if p.profile, err = e.profileFor(op, enc, cfg.Profile); err != nil {
		return p, err
	}
	if p.preset, err = checkPreset(op, cfg.Preset); err != nil {
		return p, err
	}
	seen := map[string]bool{}
	for i, l := range cfg.Layers {
		if seen[l.RID] {
			return p, errorf(media.CodeInvalid, op, "", "layer %q twice", l.RID)
		}
		seen[l.RID] = true
		lp, err := checkEngineLayer(op, i, l, p.size)
		if err != nil {
			return p, err
		}
		p.layers = append(p.layers, lp)
	}
	return p, nil
}

// resolveLocked applies the source rules to id (04 §7.2).
func (e *Engine) resolveLocked(id string, listed bool) (media.Source, error) {
	const op = "start"
	for _, s := range e.sources {
		if s.ID == id {
			if !listed {
				return media.Source{}, errorf(media.CodeInvalid, op, "", "a listed source needs Config.Listed")
			}
			return s, nil
		}
	}
	if strings.HasPrefix(id, pickPrefix) {
		e.prunePicksLocked(time.Now())
		for _, p := range e.picks {
			if p.src.ID == id {
				return p.src, nil
			}
		}
		return media.Source{}, errorf(media.CodeSourceGone, op, "pick_expired",
			"the pick was consumed, expired, evicted or released")
	}
	return media.Source{}, errorf(media.CodeSourceGone, op, "", "unknown source")
}

// encoderFor returns the encoder a session runs on: the first Tested one, or the Tested one name forces.
func (e *Engine) encoderFor(op, name string) (media.EncoderInfo, error) {
	if name != "" && !slices.Contains(encoderTokens, name) {
		return media.EncoderInfo{}, errorf(media.CodeInvalid, op, "", "unknown encoder %q", name)
	}
	for _, enc := range e.probe.Encoders {
		if enc.Tested && (name == "" || enc.Name == name) {
			return enc, nil
		}
	}
	if name != "" {
		return media.EncoderInfo{}, errorf(media.CodeNoEncoder, op, "forced_failed", "encoder %q is not available", name)
	}
	return media.EncoderInfo{}, errorf(media.CodeNoEncoder, op, "open_failed", "no tested encoder in the probe")
}

// profileFor checks a requested profile key and returns the one the session runs: the request when the encoder
// lists it, else the encoder's first profile (04 §9.6: a session the encoders can't run in High runs CB, and its
// EventLayers say so). Decodable is always Constrained Baseline and refuses "6400".
func (e *Engine) profileFor(op string, enc media.EncoderInfo, key string) (string, error) {
	switch key {
	case "6400", DecodableProfile:
	default:
		return "", errorf(media.CodeInvalid, op, "", "profile %q is not 6400 or 42e0", key)
	}
	if e.mode == Decodable && key != DecodableProfile {
		return "", errorf(media.CodeInvalid, op, "", "Decodable is always %s", DecodableProfile)
	}
	if len(enc.Profiles) == 0 || slices.Contains(enc.Profiles, key) {
		return key, nil
	}
	return enc.Profiles[0], nil
}

// checkPreset checks a preset; "" is "auto".
func checkPreset(op, preset string) (string, error) {
	switch preset {
	case "":
		return "auto", nil
	case "auto", "game", "movie", "text":
		return preset, nil
	}
	return "", errorf(media.CodeInvalid, op, "", "unknown preset %q", preset)
}

// Bitrate ranges (04 §6.4).
const (
	minBitrateFirst = 100_000
	maxBitrateFirst = 50_000_000
	minBitrateOther = 50_000
	maxBitrateOther = 2_000_000
	minAudioBitrate = 32_000
	maxAudioBitrate = 510_000
	// defaultAudioBitrate is AudioConfig.BitrateBPS 0.
	defaultAudioBitrate = 128_000
)

// checkBitrate checks a video layer's target: layer 0 ("f") 100 kbit/s–50 Mbit/s, others 50 kbit/s–2 Mbit/s.
func checkBitrate(op string, index int, rid string, bps int) error {
	lo, hi := minBitrateFirst, maxBitrateFirst
	if index > 0 {
		lo, hi = minBitrateOther, maxBitrateOther
	}
	if bps < lo || bps > hi {
		return errorf(media.CodeInvalid, op, "", "layer %q: bitrate %d not in %d–%d", rid, bps, lo, hi)
	}
	return nil
}

// checkEngineLayer checks layer index of a media.Config and scales the source to its budget.
func checkEngineLayer(op string, index int, l media.LayerConfig, size [2]int) (layerPlan, error) {
	if len(l.RID) != 1 || !isAlnum(l.RID[0]) {
		return layerPlan{}, errorf(media.CodeInvalid, op, "", "layer RID %q is not one letter or digit", l.RID)
	}
	if l.MaxPixels <= 0 {
		return layerPlan{}, errorf(media.CodeInvalid, op, "", "layer %q: MaxPixels %d", l.RID, l.MaxPixels)
	}
	if l.MaxFPS < 1 || l.MaxFPS > maxFPS {
		return layerPlan{}, errorf(media.CodeInvalid, op, "", "layer %q: MaxFPS %d not in 1–%d", l.RID, l.MaxFPS, maxFPS)
	}
	if err := checkBitrate(op, index, l.RID, l.Bitrate); err != nil {
		return layerPlan{}, err
	}
	w, h, ok := scaleToFit(size[0], size[1], l.MaxPixels)
	if !ok {
		return layerPlan{}, errorf(media.CodeInvalid, op, "", "layer %q: MaxPixels %d leaves no picture", l.RID, l.MaxPixels)
	}
	return layerPlan{cfg: l, width: w, height: h}, nil
}

// scaleToFit returns w×h scaled to at most maxPixels pixels: aspect ratio kept, even sides, never upscaled. ok is
// false when a side would be below 2.
func scaleToFit(w, h, maxPixels int) (sw, sh int, ok bool) {
	sw, sh = w&^1, h&^1
	if w*h > maxPixels {
		s := math.Sqrt(float64(maxPixels) / float64(w*h))
		for sw = int(float64(w)*s+1e-9) &^ 1; sw >= 2; sw -= 2 {
			if sh = (sw * h / w) &^ 1; sw*sh <= maxPixels {
				break
			}
		}
	}
	return sw, sh, sw >= 2 && sh >= 2
}

// checkAudio checks an AudioConfig as 05 §13.1's mode mapping does and fills in the default bitrate.
func checkAudio(op string, a media.AudioConfig) (media.AudioConfig, error) {
	switch a.Mode {
	case "", media.AudioExcludeApps, media.AudioTestTone:
	default:
		return a, errorf(media.CodeInvalid, op, "", "audio mode %q at start", a.Mode)
	}
	switch {
	case a.BitrateBPS == 0:
		a.BitrateBPS = defaultAudioBitrate
	case a.BitrateBPS < minAudioBitrate || a.BitrateBPS > maxAudioBitrate:
		return a, errorf(media.CodeInvalid, op, "", "audio bitrate %d not in %d–%d", a.BitrateBPS,
			minAudioBitrate, maxAudioBitrate)
	}
	if err := checkExclusions(op, a.Exclusions); err != nil {
		return a, err
	}
	return a, nil
}

// errorf returns an engine error. detail is for logs only: it never names a source.
func errorf(code media.ErrorCode, op, reason, format string, args ...any) *media.Error {
	return &media.Error{Code: code, Op: op, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// errClosed is the error of a call on a closed Engine.
func errClosed(op string) *media.Error {
	return &media.Error{Code: media.CodeStopped, Op: op, Reason: "closed"}
}
