package publish

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media"
)

// The M2 tests' helpers (docs/m2/01-client-core.md §14's publish row): a scripted Source on a clock, a writer that
// records when each packet left, and a Publisher without a PeerConnection that runs its send loop and pacer as Start
// would, so that testing/synctest can drive them on its fake clock.

// scripted is one packet of a scriptSource: due at the given time after the source's start, and age old then.
type scripted struct {
	at  time.Duration
	age time.Duration
	pkt media.Packet
}

// scriptSource hands out its script in order, each packet when it is due; then Next blocks until ctx ends. It is a
// Clock: its time is the time since its start, and a packet's CaptureNS is its due time less its age. onNext, when
// set, runs at every call of Next, before it waits.
type scriptSource struct {
	start  time.Time
	onNext func()

	mu        sync.Mutex
	script    []scripted
	next      int
	keyframes []string
}

func newScriptSource(script []scripted) *scriptSource {
	s := &scriptSource{start: time.Now(), script: slices.Clone(script)}
	slices.SortStableFunc(s.script, func(a, b scripted) int { return int(a.at - b.at) })
	return s
}

func (s *scriptSource) Now() int64 { return int64(time.Since(s.start)) }

func (s *scriptSource) Next(ctx context.Context) (media.Packet, error) {
	if s.onNext != nil {
		s.onNext()
	}
	s.mu.Lock()
	if s.next == len(s.script) {
		s.mu.Unlock()
		<-ctx.Done()
		return media.Packet{}, ctx.Err()
	}
	item := s.script[s.next]
	s.next++
	s.mu.Unlock()
	if wait := time.Until(s.start.Add(item.at)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return media.Packet{}, ctx.Err()
		case <-timer.C:
		}
	}
	pkt := item.pkt
	pkt.CaptureNS = int64(item.at - item.age)
	return pkt, nil
}

func (s *scriptSource) RequestKeyframe(layer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyframes = append(s.keyframes, layer)
}

// taken returns how many packets Next has handed out.
func (s *scriptSource) taken() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// plainSource hides a Source's Clock.
type plainSource struct{ Source }

// annexB returns an access unit of about size bytes: a 4-byte start code, an SPS NAL unit with profile_idc idc when
// idc isn't 0, then one slice NAL unit (IDR when key) of bytes that are never 0, so no start code hides in them.
func annexB(size int, key bool, idc byte) []byte {
	var au []byte
	if idc != 0 {
		au = append(au, 0, 0, 0, 1, 0x67, idc, 0x00, 0x1f, 0xac)
	}
	slice := byte(0x41)
	if key {
		slice = 0x65
	}
	au = append(au, 0, 0, 0, 1, slice)
	for len(au) < size {
		au = append(au, byte(len(au)%250)+1)
	}
	return au
}

// videoFrame returns a video packet of layer, of about size bytes.
func videoFrame(layer string, size int, key bool) media.Packet {
	return media.Packet{Kind: media.KindVideo, Layer: layer, Keyframe: key, Data: annexB(size, key, 0)}
}

// audioPacket returns an Opus packet.
func audioPacket() media.Packet {
	return media.Packet{Kind: media.KindAudio, Data: append([]byte{0xfc}, make([]byte, 160)...)}
}

// written is one packet a clockWriter recorded.
type written struct {
	at  time.Time
	hdr rtp.Header
	n   int // payload bytes
}

// clockWriter is a track's webrtc.TrackLocalWriter that records each packet with the time it was written.
type clockWriter struct {
	mu   sync.Mutex
	pkts []written
}

func (w *clockWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pkts = append(w.pkts, written{at: time.Now(), hdr: h.Clone(), n: len(payload)})
	return h.MarshalSize() + len(payload), nil
}

func (w *clockWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *clockWriter) all() []written {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.pkts)
}

// markers returns the write times of the packets with the marker bit: the last packet of each video frame.
func (w *clockWriter) markers() []time.Time {
	var out []time.Time
	for _, p := range w.all() {
		if p.hdr.Marker {
			out = append(out, p.at)
		}
	}
	return out
}

// bench is a Publisher without a PeerConnection: its tracks are bound to clockWriters, and run starts its send loop
// (pump) and, with pacing, its pacer, as Start does once the PeerConnection is connected.
type bench struct {
	p       *Publisher
	writers map[string]*clockWriter // by layer; "" is the audio
	cancel  context.CancelFunc
	done    chan struct{}
}

// newBench builds the Publisher for src: one video track per layer, simulcast when there are two or more, the
// audio with audio, each bound with the mid and rid extensions, in profile "6400" (payload type 96).
func newBench(src Source, layers []string, audio, pacing bool) *bench {
	p := &Publisher{src: src, log: slog.New(slog.DiscardHandler), h264: h264Table{DefaultProfile}, encoding: DefaultProfile}
	p.setSending(pacing)
	b := &bench{p: p, writers: map[string]*clockWriter{}}
	bind := func(t *track, ssrc uint32) {
		w := &clockWriter{}
		b.writers[t.layer] = w
		t.b = &binding{w: w, ssrc: ssrc, pt: videoPT, midID: 1, ridID: 2}
		t.stats.SSRC = ssrc
		t.midExt = []byte("0")
	}
	for i, l := range layers {
		rid := l
		if len(layers) == 1 {
			rid = ""
		}
		t := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", rid, l, DefaultProfile)
		bind(t, uint32(10+i))
		p.video = append(p.video, t)
	}
	if audio {
		p.audio = newTrack(webrtc.RTPCodecTypeAudio, "audio", "s", "", "", "")
		bind(p.audio, 99)
		p.audio.b.pt = opusPT
	}
	return b
}

// run starts the send loop and the pacer under ctx.
func (b *bench) run(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.done = make(chan struct{})
	n := 1
	if b.p.pacer != nil {
		n = 2
	}
	ended := make(chan struct{}, n)
	go func() { b.p.pump(ctx); ended <- struct{}{} }()
	if b.p.pacer != nil {
		go func() { b.p.pacer.run(ctx); ended <- struct{}{} }()
	}
	go func() {
		for range n {
			<-ended
		}
		close(b.done)
	}()
}

// stop ends the send loop and the pacer and waits for them.
func (b *bench) stop() {
	b.cancel()
	<-b.done
}

// trackStats returns the Stats of a layer's track ("" for the audio).
func (b *bench) trackStats(layer string) TrackStats {
	for _, ts := range b.p.Stats().Tracks {
		if ts.Layer == layer {
			return ts
		}
	}
	panic("no track for layer " + layer)
}
