package fake

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// Kind is a packet's media kind.
type Kind uint8

// The media kinds. The zero Kind is invalid.
const (
	Video Kind = iota + 1
	Audio
)

func (k Kind) String() string {
	switch k {
	case Video:
		return "video"
	case Audio:
		return "audio"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// Packet is one access unit or one audio packet. It mirrors the native engine's C ABI packet {kind, layer, keyframe,
// capture_ts_ns, annexb|opus, reinit} (docs/PLAN.md); a fake never re-initializes, so there is no reinit field.
type Packet struct {
	Kind      Kind
	Layer     string // "f" | "q" (a VideoLayer's RID) | "" (audio)
	Keyframe  bool   // video: an IDR access unit with SPS and PPS in front
	CaptureNS int64  // monotonic capture time in ns; 0 is the first packet the Source returned
	Data      []byte // Annex B access unit (with emulation prevention) or one Opus packet; owned by the caller
	Flash     bool   // this frame shows the sync flash
	Beep      bool   // this audio packet starts the sync beep
}

// Mode picks how video is made.
type Mode uint8

// The modes. Synthetic is the zero value, so it is the default.
const (
	// Synthetic makes codec-shaped H.264 that the SFU forwards like real video but no decoder can show.
	Synthetic Mode = iota
	// Decodable makes Constrained Baseline H.264 of I_PCM and P_Skip macroblocks that browsers decode: a test
	// picture with a moving box, a frame counter and the flash square (decodable.go).
	Decodable
)

// VideoLayer is one simulcast layer of the fake video.
type VideoLayer struct {
	RID                string // one letter or digit: it is one byte of the Marker ("f", "q"; "h" only for guard tests)
	Width, Height, FPS int    // even width and height; FPS 1–240
	// Bitrate is the average bitrate over a GOP in bps: a positive setting in Synthetic, which SetBitrate changes.
	// Decodable ignores it (its rate follows from the picture); DefaultDecodableLayers fills in the approximate rate.
	Bitrate int
}

// Config configures a Source. The zero value is valid: Synthetic, High profile, the default layers, 3 s GOPs, no
// audio, a flash every second, seed 0.
type Config struct {
	Mode Mode
	// Profile is the H.264 profile key of the SPS: Synthetic "6400" (default) or "42e0" (to exercise the codec
	// policy), or another profile the SFU knows ("4200", "4d00", "640c"). Decodable is always "42e0" ("" or
	// "42e0" here; anything else is an error).
	Profile string
	// Layers are the video layers, in the order the publisher sends them. nil means DefaultLayers() (Synthetic) or
	// DefaultDecodableLayers() (Decodable); an empty, non-nil slice means no video (then Audio must be on).
	Layers []VideoLayer
	GOP    time.Duration // default 3 s; a keyframe request starts a new GOP
	// Audio adds one Opus packet every 20 ms from the committed asset (opus.go): a 440 Hz tone with a 1 kHz beep in
	// the first 100 ms of every second, looped.
	Audio bool
	// FlashEvery puts one flash frame on every layer and one beep in the audio at the same capture instant, every
	// FlashEvery (default 1 s; negative: none). The instants are k·FlashEvery for k = 0, 1, 2, …; the flash frame is
	// the first frame at or after each instant, so flash and beep share their capture time exactly when FlashEvery is
	// a multiple of 20 ms and of every layer's frame interval (as with the defaults). The flags are what the Markers
	// say; the asset's audible beep comes every second, so it matches the Beep flags when FlashEvery is 1 s.
	FlashEvery time.Duration
	Seed       uint64 // Synthetic's filler bytes and frame-size jitter are a function of the seed alone
}

// DefaultLayers returns the layers of a Synthetic Config without Layers: f 1920×1080@60 at 8 Mbps and q 640×360@15
// at 0.3 Mbps, the Auto preset's encodings (02 §8.6).
func DefaultLayers() []VideoLayer {
	return []VideoLayer{
		{RID: "f", Width: 1920, Height: 1080, FPS: 60, Bitrate: 8_000_000},
		{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 300_000},
	}
}

// DefaultDecodableLayers returns the layers of a Decodable Config without Layers: f 640×360@30 and q 320×180@15
// (02 §15.1). Their Bitrate is the approximate average with the default GOP and flashes, which the I_PCM IDR
// frames and the moving box dominate; Decodable does not use it.
func DefaultDecodableLayers() []VideoLayer {
	return []VideoLayer{
		{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: decodableRateF},
		{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: decodableRateQ},
	}
}

// Defaults and limits.
const (
	DefaultGOP        = 3 * time.Second
	DefaultFlashEvery = time.Second
	DefaultProfile    = "6400"
	// DecodableProfile is the SPS profile of Decodable: Constrained Baseline.
	DecodableProfile = "42e0"
	// AudioPacketDuration is the duration of one audio packet (48 kHz, 960 samples).
	AudioPacketDuration = 20 * time.Millisecond
	maxLayers           = 4
	maxFPS              = 240
	// maxLag is the most media Next hands out at once to a consumer that fell behind; see Next.
	maxLag = 200 * time.Millisecond
)

var (
	// ErrClosed is returned by Next after Close.
	ErrClosed = errors.New("fake: source closed")
	// ErrInvalidConfig is wrapped by every New error about the Config.
	ErrInvalidConfig = errors.New("fake: invalid config")
)

// Source makes the fake media. It is pull-based like the native engine: Next blocks until the next packet is due in
// real time and returns packets in capture order. Next is for one consumer goroutine; RequestKeyframe, SetBitrate and
// Close may be called from any goroutine. New starts nothing: the clock starts at the first Next.
type Source struct {
	flashEvery int64 // ns; 0 = no flashes
	streams    []*stream
	closed     chan struct{}
	closeOnce  sync.Once
	timer      *time.Timer // Next only

	mu      sync.Mutex // guards streams' state, started and origin
	started bool
	origin  time.Time // the first Next, moved forward by every skip (see Next)
}

// stream is one video layer or the audio.
type stream struct {
	kind      Kind
	rid       string
	fps       int
	gopFrames int
	bitrate   int    // bps, SetBitrate
	index     uint32 // of the next frame or audio packet
	next      int64  // capture ns of the next frame or audio packet
	sinceKey  int    // frames since the last keyframe (video)
	forceKey  bool
	nextFlash int64
	jitter    float64 // the last odd delta frame's jitter, which the next even one mirrors (Synthetic video)
	rng       *rand.Rand
	sps, pps  []byte       // NAL units with header and emulation prevention (video)
	scratch   []byte       // RBSP buffer, reused (Synthetic video)
	dec       *decodable   // the encoder (Decodable video)
	opus      []opusPacket // the asset's loop (audio)
}

// New checks cfg, applies its defaults and returns a Source.
func New(cfg Config) (*Source, error) {
	switch cfg.Mode {
	case Synthetic:
		if cfg.Profile == "" {
			cfg.Profile = DefaultProfile
		}
		if cfg.Layers == nil {
			cfg.Layers = DefaultLayers()
		}
	case Decodable:
		if cfg.Profile == "" {
			cfg.Profile = DecodableProfile
		}
		if cfg.Profile != DecodableProfile {
			return nil, fmt.Errorf("%w: Profile %q: Decodable is always %q", ErrInvalidConfig, cfg.Profile,
				DecodableProfile)
		}
		if cfg.Layers == nil {
			cfg.Layers = DefaultDecodableLayers()
		}
	default:
		return nil, fmt.Errorf("%w: unknown Mode %d", ErrInvalidConfig, cfg.Mode)
	}
	prof, err := parseProfile(cfg.Profile)
	if err != nil {
		return nil, err
	}
	if len(cfg.Layers) > maxLayers {
		return nil, fmt.Errorf("%w: %d layers, at most %d", ErrInvalidConfig, len(cfg.Layers), maxLayers)
	}
	if len(cfg.Layers) == 0 && !cfg.Audio {
		return nil, fmt.Errorf("%w: no video layers and no audio", ErrInvalidConfig)
	}
	if cfg.GOP == 0 {
		cfg.GOP = DefaultGOP
	}
	if cfg.GOP < 0 {
		return nil, fmt.Errorf("%w: negative GOP", ErrInvalidConfig)
	}
	if cfg.FlashEvery == 0 {
		cfg.FlashEvery = DefaultFlashEvery
	}
	s := &Source{closed: make(chan struct{})}
	if cfg.FlashEvery > 0 {
		s.flashEvery = int64(cfg.FlashEvery)
	}
	seen := map[string]bool{}
	for i, l := range cfg.Layers {
		if err := checkLayer(l, cfg.Mode); err != nil {
			return nil, err
		}
		if seen[l.RID] {
			return nil, fmt.Errorf("%w: layer %q twice", ErrInvalidConfig, l.RID)
		}
		seen[l.RID] = true
		sps, err := buildSPS(prof, l)
		if err != nil {
			return nil, err
		}
		gop := int((cfg.GOP*time.Duration(l.FPS) + time.Second/2) / time.Second)
		st := &stream{
			kind: Video, rid: l.RID, fps: l.FPS, gopFrames: max(gop, 1), bitrate: l.Bitrate,
			rng: newRand(cfg.Seed, uint64(i)), sps: sps, pps: buildPPS(),
		}
		if cfg.Mode == Decodable {
			st.dec = newDecodable(l)
		}
		s.streams = append(s.streams, st)
	}
	if cfg.Audio {
		loop, err := audioLoop()
		if err != nil {
			return nil, fmt.Errorf("fake: the Opus asset: %w", err)
		}
		s.streams = append(s.streams, &stream{kind: Audio, opus: loop})
	}
	return s, nil
}

// newRand returns the generator of one stream: filler bytes and frame-size jitter must be a function of the seed.
func newRand(seed, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, stream)) //nolint:gosec // G404: deterministic test media, not security
}

// checkLayer validates one VideoLayer (the level check is buildSPS's).
func checkLayer(l VideoLayer, mode Mode) error {
	if len(l.RID) != 1 || !isAlnum(l.RID[0]) {
		return fmt.Errorf("%w: layer RID %q is not one letter or digit", ErrInvalidConfig, l.RID)
	}
	if l.Width <= 0 || l.Height <= 0 || l.Width%2 != 0 || l.Height%2 != 0 {
		return fmt.Errorf("%w: layer %q: size %dx%d is not positive and even", ErrInvalidConfig, l.RID, l.Width, l.Height)
	}
	if l.FPS < 1 || l.FPS > maxFPS {
		return fmt.Errorf("%w: layer %q: FPS %d not in 1–%d", ErrInvalidConfig, l.RID, l.FPS, maxFPS)
	}
	if l.Bitrate <= 0 && mode == Synthetic {
		return fmt.Errorf("%w: layer %q: Bitrate %d is not positive", ErrInvalidConfig, l.RID, l.Bitrate)
	}
	return nil
}

func isAlnum(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// Next returns the next packet once it is due: at the Source's first Next plus its capture time, plus any time the
// clock has skipped. A consumer that falls behind gets the overdue packets at once, in order, but never more than
// maxLag (200 ms) of media: when the next packet is more than maxLag overdue, the clock skips the excess, so every
// stream resumes maxLag behind the current time. Like a capture engine, the Source does not pile up media for a
// consumer that stopped pulling (a pub PC rebuild); unlike one, it drops no frame: indices and capture times stay
// continuous, and CaptureNS falls behind the time since the first Next by the skipped time. Next returns ctx.Err()
// when ctx has ended or ends before the packet is due, and ErrClosed after Close.
func (s *Source) Next(ctx context.Context) (Packet, error) {
	select {
	case <-s.closed:
		return Packet{}, ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return Packet{}, err
	}
	s.mu.Lock()
	now := time.Now()
	if !s.started {
		s.started, s.origin = true, now
	}
	st := s.streams[0]
	for _, o := range s.streams[1:] {
		if o.next < st.next {
			st = o
		}
	}
	due := s.origin.Add(time.Duration(st.next))
	if lag := now.Sub(due); lag > maxLag {
		s.origin = s.origin.Add(lag - maxLag)
		due = now.Add(-maxLag)
	}
	s.mu.Unlock()

	if d := time.Until(due); d > 0 {
		if s.timer == nil {
			s.timer = time.NewTimer(d)
		} else {
			s.timer.Reset(d)
		}
		select {
		case <-s.timer.C:
		case <-ctx.Done():
			s.timer.Stop()
			return Packet{}, ctx.Err()
		case <-s.closed:
			s.timer.Stop()
			return Packet{}, ErrClosed
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if st.kind == Audio {
		return st.audioPacket(s.flashEvery), nil
	}
	return st.videoFrame(s.flashEvery), nil
}

// RequestKeyframe makes the next frame of layer a keyframe and starts a new GOP there, like an encoder on PLI. An
// unknown layer is ignored.
func (s *Source) RequestKeyframe(layer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.video(layer); st != nil {
		st.forceKey = true
	}
}

// SetBitrate changes a Synthetic layer's average bitrate from its next frame on. An unknown layer and bps ≤ 0 are
// ignored, and so is every call in Decodable, which has no rate control.
func (s *Source) SetBitrate(layer string, bps int) {
	if bps <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.video(layer); st != nil {
		st.bitrate = bps
	}
}

// Close makes Next return ErrClosed, also a Next that is waiting. It is idempotent and always returns nil.
func (s *Source) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *Source) video(rid string) *stream {
	for _, st := range s.streams {
		if st.kind == Video && st.rid == rid {
			return st
		}
	}
	return nil
}

// flash reports whether the frame or packet at capture time t is the first one at or after a flash instant, and
// moves nextFlash past t.
func (st *stream) flash(t, every int64) bool {
	if every <= 0 || t < st.nextFlash {
		return false
	}
	for st.nextFlash <= t {
		st.nextFlash += every
	}
	return true
}

// videoFrame makes the stream's next access unit and advances the stream.
func (st *stream) videoFrame(flashEvery int64) Packet {
	key := st.index == 0 || st.forceKey || st.sinceKey >= st.gopFrames
	capture := st.next
	flash := st.flash(capture, flashEvery)
	m := Marker{RID: st.rid, Keyframe: key, Flash: flash, Frame: st.index, CaptureNS: capture}
	var data []byte
	if st.dec != nil {
		data = st.dec.accessUnit(key, m, capture, st.sps, st.pps)
	} else {
		data = st.syntheticAU(key, m, st.frameSize(key))
	}
	if key {
		st.sinceKey, st.forceKey = 1, false
	} else {
		st.sinceKey++
	}
	st.index++
	st.next = int64(st.index) * int64(time.Second) / int64(st.fps)
	return Packet{Kind: Video, Layer: st.rid, Keyframe: key, CaptureNS: capture, Data: data, Flash: flash}
}

// frameSize is the access unit size in bytes (02 §15.1): over a GOP of n frames, P = n·avg/(n − 1 + 8) and K = 8·P,
// so the GOP averages avg. Delta frames get ±25% jitter, mirrored between the two frames of each pair so that the
// average holds exactly over every pair.
func (st *stream) frameSize(key bool) int {
	avg := float64(st.bitrate) / 8 / float64(st.fps)
	n := float64(st.gopFrames)
	p := n * avg / (n + 7)
	if key {
		return int(8*p + 0.5)
	}
	var j float64
	if st.sinceKey%2 == 1 {
		st.jitter = (st.rng.Float64() - 0.5) / 2
		j = st.jitter
	} else {
		j = -st.jitter
	}
	return int(p*(1+j) + 0.5)
}

// audioPacket makes the next audio packet, the asset loop's next packet with the Marker in its padding, and
// advances the stream.
func (st *stream) audioPacket(flashEvery int64) Packet {
	capture := st.next
	beep := st.flash(capture, flashEvery)
	m := Marker{Beep: beep, Frame: st.index, CaptureNS: capture}
	data := opusWithMarker(st.opus[st.index%uint32(len(st.opus))], m)
	st.index++
	st.next = int64(st.index) * int64(AudioPacketDuration)
	return Packet{Kind: Audio, CaptureNS: capture, Data: data, Beep: beep}
}
