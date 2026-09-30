package fake

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestNewConfig(t *testing.T) {
	good := VideoLayer{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 1_000_000}
	with := func(f func(*VideoLayer)) []VideoLayer {
		l := good
		f(&l)
		return []VideoLayer{l}
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"zero value", Config{}, nil},
		{"42e0", Config{Profile: "42e0"}, nil},
		{"all known profiles", Config{Profile: "4d00"}, nil},
		{"audio only", Config{Layers: []VideoLayer{}, Audio: true}, nil},
		{"nothing", Config{Layers: []VideoLayer{}}, ErrInvalidConfig},
		{"decodable", Config{Mode: Decodable}, ErrNotImplemented},
		{"unknown mode", Config{Mode: 7}, ErrInvalidConfig},
		{"upper-case profile", Config{Profile: "42E0"}, ErrInvalidConfig},
		{"short profile", Config{Profile: "640"}, ErrInvalidConfig},
		{"not hex", Config{Profile: "64zz"}, ErrInvalidConfig},
		{"profile idc 88", Config{Profile: "5800"}, ErrInvalidConfig},
		{"long rid", Config{Layers: with(func(l *VideoLayer) { l.RID = "ff" })}, ErrInvalidConfig},
		{"empty rid", Config{Layers: with(func(l *VideoLayer) { l.RID = "" })}, ErrInvalidConfig},
		{"odd width", Config{Layers: with(func(l *VideoLayer) { l.Width = 641 })}, ErrInvalidConfig},
		{"zero height", Config{Layers: with(func(l *VideoLayer) { l.Height = 0 })}, ErrInvalidConfig},
		{"zero fps", Config{Layers: with(func(l *VideoLayer) { l.FPS = 0 })}, ErrInvalidConfig},
		{"fps 241", Config{Layers: with(func(l *VideoLayer) { l.FPS = 241 })}, ErrInvalidConfig},
		{"zero bitrate", Config{Layers: with(func(l *VideoLayer) { l.Bitrate = 0 })}, ErrInvalidConfig},
		{"above level 6.2", Config{Layers: with(func(l *VideoLayer) { l.Width, l.Height = 16384, 8704 })}, ErrInvalidConfig},
		{"duplicate rid", Config{Layers: []VideoLayer{good, good}}, ErrInvalidConfig},
		{"five layers", Config{Layers: []VideoLayer{
			good, {RID: "q", Width: 2, Height: 2, FPS: 1, Bitrate: 1}, {RID: "h", Width: 2, Height: 2, FPS: 1, Bitrate: 1},
			{RID: "x", Width: 2, Height: 2, FPS: 1, Bitrate: 1}, {RID: "y", Width: 2, Height: 2, FPS: 1, Bitrate: 1},
		}}, ErrInvalidConfig},
		{"negative GOP", Config{GOP: -time.Second}, ErrInvalidConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.cfg)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
			if s != nil {
				_ = s.Close()
			}
		})
	}
}

// collect pulls packets from s for d of (fake) time.
func collect(t *testing.T, s *Source, d time.Duration) []Packet {
	t.Helper()
	var out []Packet
	for {
		p, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if p.CaptureNS >= int64(d) {
			return out
		}
		out = append(out, p)
	}
}

// TestSyntheticBitrate is the 02 §17 fake test: every layer averages its bitrate within ±5% over 30 s, frames come
// at the layer's rate, keyframes once per GOP, and flash frames and beeps share one capture instant every second.
func TestSyntheticBitrate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(Config{Audio: true, Seed: 42})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		start := time.Now()
		pkts := collect(t, s, 30*time.Second)
		if got := time.Since(start); got < 30*time.Second-20*time.Millisecond {
			t.Errorf("30 s of media took %v: not paced in real time", got)
		}
		var lastCapture int64
		for i, p := range pkts {
			if p.CaptureNS < lastCapture {
				t.Fatalf("packet %d goes back in capture time: %d < %d", i, p.CaptureNS, lastCapture)
			}
			lastCapture = p.CaptureNS
		}
		for _, l := range DefaultLayers() {
			var bytes, frames, keys int
			var flashes []int64
			for _, p := range pkts {
				if p.Kind != Video || p.Layer != l.RID {
					continue
				}
				bytes += len(p.Data)
				frames++
				if p.Keyframe {
					keys++
					if frames%(3*l.FPS) != 1 {
						t.Errorf("layer %s: keyframe at frame %d, want one every %d frames", l.RID, frames-1, 3*l.FPS)
					}
				}
				if p.Flash {
					flashes = append(flashes, p.CaptureNS)
				}
			}
			bps := float64(bytes) * 8 / 30
			t.Logf("layer %s: %d frames, %d keyframes, %.0f bps (want %d)", l.RID, frames, keys, bps, l.Bitrate)
			if math.Abs(bps/float64(l.Bitrate)-1) > 0.05 {
				t.Errorf("layer %s: %.0f bps, want %d ± 5%%", l.RID, bps, l.Bitrate)
			}
			if frames != 30*l.FPS || keys != 10 {
				t.Errorf("layer %s: %d frames and %d keyframes, want %d and 10", l.RID, frames, keys, 30*l.FPS)
			}
			if len(flashes) != 30 {
				t.Fatalf("layer %s: %d flash frames, want 30", l.RID, len(flashes))
			}
			for k, c := range flashes {
				if c != int64(k)*int64(time.Second) {
					t.Errorf("layer %s: flash %d at %d ns, want %d", l.RID, k, c, int64(k)*int64(time.Second))
				}
			}
		}
		var audio, beeps int
		for _, p := range pkts {
			if p.Kind != Audio {
				continue
			}
			if m, ok := ParseAudioMarker(p.Data); !ok || m.Frame != uint32(audio) || m.Beep != p.Beep ||
				m.CaptureNS != p.CaptureNS || len(p.Data) != AudioPacketSize {
				t.Fatalf("audio packet %d: marker %+v %v, packet %+v", audio, m, ok, p)
			}
			if p.CaptureNS != int64(audio)*int64(AudioPacketDuration) {
				t.Fatalf("audio packet %d at %d ns", audio, p.CaptureNS)
			}
			if p.Beep {
				if p.CaptureNS%int64(time.Second) != 0 {
					t.Errorf("beep at %d ns, want whole seconds", p.CaptureNS)
				}
				beeps++
			}
			audio++
		}
		if audio != 1500 || beeps != 30 {
			t.Errorf("%d audio packets with %d beeps, want 1500 and 30", audio, beeps)
		}
	})
}

// TestSyntheticAU checks the access units: NAL unit order and headers, Markers, emulation prevention and the SPS.
func TestSyntheticAU(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		layers := []VideoLayer{
			{RID: "f", Width: 1920, Height: 1080, FPS: 60, Bitrate: 8_000_000},
			{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 300_000},
			{RID: "h", Width: 1280, Height: 720, FPS: 30, Bitrate: 2_000_000},
		}
		for _, prof := range []string{"6400", "42e0"} {
			s, err := New(Config{Profile: prof, Layers: layers, GOP: time.Second, Seed: 7})
			if err != nil {
				t.Fatal(err)
			}
			next := map[string]uint32{}
			for _, p := range collect(t, s, 2*time.Second) {
				nals := splitAnnexB(t, p.Data)
				m, ok := ParseVideoMarker(nals[len(nals)-1][1:])
				if !ok {
					t.Fatalf("%s %s: no marker in %x", prof, p.Layer, nals[len(nals)-1][:24])
				}
				want := Marker{RID: p.Layer, Keyframe: p.Keyframe, Flash: p.Flash, Frame: next[p.Layer],
					CaptureNS: p.CaptureNS}
				if m != want {
					t.Fatalf("%s: marker %+v, want %+v", prof, m, want)
				}
				next[p.Layer]++
				if !p.Keyframe {
					if len(nals) != 1 || nals[0][0] != nalNonIDR {
						t.Fatalf("delta frame NAL units %v", nalTypes(nals))
					}
					continue
				}
				if len(nals) != 3 || nals[0][0] != nalSPS || nals[1][0] != nalPPS || nals[2][0] != nalIDR {
					t.Fatalf("keyframe NAL units %v", nalTypes(nals))
				}
				sps := readSPS(t, nals[0])
				var l VideoLayer
				for _, c := range layers {
					if c.RID == p.Layer {
						l = c
					}
				}
				if got := sps.profileKey(); got != prof || sps.width != l.Width || sps.height != l.Height {
					t.Fatalf("%s: SPS %+v, want %s %dx%d", p.Layer, sps, prof, l.Width, l.Height)
				}
				if lvl, _ := levelFor((l.Width+15)/16, (l.Height+15)/16, l.FPS); sps.level != lvl {
					t.Errorf("%s: level %d, want %d", p.Layer, sps.level, lvl)
				}
			}
			_ = s.Close()
		}
	})
}

func TestLevels(t *testing.T) {
	for _, tc := range []struct {
		w, h, fps int
		want      uint8
	}{
		{1920, 1080, 60, 42}, {1920, 1080, 30, 40}, {640, 360, 15, 22}, {1280, 720, 30, 31}, {1280, 720, 60, 32},
		{3840, 2160, 60, 52}, {320, 180, 15, 12}, {7680, 4320, 60, 61},
	} {
		if got, ok := levelFor((tc.w+15)/16, (tc.h+15)/16, tc.fps); !ok || got != tc.want {
			t.Errorf("%dx%d@%d: level %d %v, want %d", tc.w, tc.h, tc.fps, got, ok, tc.want)
		}
	}
}

// TestMarkerEscaping: markers full of zeros survive emulation prevention (02 §17 "markers survive emulation
// prevention"), and the escaped slice never contains a start code or a stray 00 00 0x.
func TestMarkerEscaping(t *testing.T) {
	for _, m := range []Marker{
		{RID: "f"},
		{RID: "q", Keyframe: true, Frame: 0x00000100, CaptureNS: 0x0000000300000001},
		{RID: "f", Flash: true, Frame: 0x03000003, CaptureNS: 0x0000000000000000},
		{RID: "9", Beep: true, Frame: math.MaxUint32, CaptureNS: math.MaxInt64},
		{Frame: 0x00000002, CaptureNS: 0x0000000200000000},
	} {
		rbsp := append(appendMarker(nil, m), 0, 0, 0, 0, 1, 0, 0, 2, 0, 0, 3, rbspStopBit)
		nal := appendEscaped([]byte{nalNonIDR}, rbsp)
		checkEscaped(t, nal)
		got, ok := ParseVideoMarker(nal[1:])
		if !ok || got != m {
			t.Errorf("ParseVideoMarker(%x) = %+v %v, want %+v", nal, got, ok, m)
		}
		// Only the first bytes of a fragmented slice are needed.
		if got, ok := ParseVideoMarker(nal[1:min(len(nal), 1+MarkerSize+9)]); !ok || got != m {
			t.Errorf("prefix: %+v %v, want %+v", got, ok, m)
		}
	}
	if _, ok := ParseVideoMarker(make([]byte, MarkerSize)); ok {
		t.Error("zeros parsed as a marker")
	}
	if _, ok := ParseVideoMarker([]byte("ISHN")); ok {
		t.Error("short input parsed as a marker")
	}
}

func TestAudioMarker(t *testing.T) {
	m := Marker{Beep: true, Frame: 50, CaptureNS: int64(time.Second)}
	pkt := syntheticOpus(m, newTestRand())
	if pkt[0] != 0xff || pkt[1] != 0x41 || pkt[2] != MarkerSize || len(pkt) != AudioPacketSize {
		t.Fatalf("header %x, len %d", pkt[:3], len(pkt))
	}
	if got, ok := ParseAudioMarker(pkt); !ok || got != m {
		t.Fatalf("ParseAudioMarker = %+v %v, want %+v", got, ok, m)
	}
	// A long padding: 255 means 254 bytes plus another length byte.
	long := []byte{opusTOC, opusCountByte, 255, 255, 2}
	long = append(long, make([]byte, 7)...) // the frame
	long = append(long, make([]byte, 254+254+2-MarkerSize)...)
	long = appendMarker(long, m)
	if got, ok := ParseAudioMarker(long); ok {
		t.Errorf("marker at the end of the padding parsed: %+v", got)
	}
	long = []byte{opusTOC, opusCountByte, 255, 255, 2}
	long = append(long, make([]byte, 7)...)
	long = appendMarker(long, m)
	long = append(long, make([]byte, 254+254+2-MarkerSize)...)
	if got, ok := ParseAudioMarker(long); !ok || got != m {
		t.Errorf("long padding: %+v %v", got, ok)
	}
	for _, bad := range [][]byte{
		nil, {0xfc}, {0xfc, 1, 2, 3}, // code 0
		{0xff, 0x01, 18},  // no padding flag
		{0xff, 0x40, 18},  // zero frames
		{0xff, 0x41, 17},  // padding shorter than a marker
		{0xff, 0x41, 255}, // runs out
		append([]byte{0xff, 0x41, 200}, make([]byte, 50)...), // padding longer than the packet
	} {
		if m, ok := ParseAudioMarker(bad); ok {
			t.Errorf("ParseAudioMarker(%x) = %+v, want no marker", bad, m)
		}
	}
}

func TestRequestKeyframeAndBitrate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(Config{Layers: []VideoLayer{{RID: "q", Width: 640, Height: 360, FPS: 10, Bitrate: 100_000}},
			GOP: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		next := func() Packet {
			p, err := s.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		if p := next(); !p.Keyframe {
			t.Fatal("first frame is not a keyframe")
		}
		for range 5 {
			if next().Keyframe {
				t.Fatal("unexpected keyframe")
			}
		}
		s.RequestKeyframe("q")
		s.RequestKeyframe("nope") // ignored
		if !next().Keyframe {
			t.Fatal("no keyframe after RequestKeyframe")
		}
		// The GOP restarts: the next periodic keyframe is 100 frames later.
		for i := 1; i < 100; i++ {
			if next().Keyframe {
				t.Fatalf("keyframe %d frames after the forced one", i)
			}
		}
		if !next().Keyframe {
			t.Fatal("no periodic keyframe after a GOP")
		}
		var before int
		for range 20 {
			before += len(next().Data)
		}
		s.SetBitrate("q", 400_000)
		s.SetBitrate("q", -1) // ignored
		var after int
		for range 20 {
			after += len(next().Data)
		}
		if r := float64(after) / float64(before); r < 3.5 || r > 4.5 {
			t.Errorf("4× bitrate gave %d → %d bytes per 20 frames", before, after)
		}
	})
}

func TestNextStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(Config{Layers: []VideoLayer{{RID: "f", Width: 64, Height: 64, FPS: 1, Bitrate: 10_000}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(t.Context()); err != nil { // frame 0 is due at once
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Next = %v, want the deadline", err)
		}
		done := make(chan error)
		go func() {
			_, err := s.Next(t.Context())
			done <- err
		}()
		synctest.Wait()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrClosed) {
			t.Fatalf("waiting Next = %v, want ErrClosed", err)
		}
		if _, err := s.Next(t.Context()); !errors.Is(err, ErrClosed) {
			t.Fatalf("Next after Close = %v", err)
		}
		_ = s.Close()
	})
}

func TestDeterministic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := func(seed uint64) []Packet {
			s, err := New(Config{Audio: true, Seed: seed, GOP: 500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			return collect(t, s, time.Second)
		}
		a, b, c := run(1), run(1), run(2)
		if len(a) != len(b) {
			t.Fatalf("%d vs %d packets", len(a), len(b))
		}
		same := true
		for i := range a {
			if !bytes.Equal(a[i].Data, b[i].Data) {
				t.Fatalf("packet %d differs with the same seed", i)
			}
			same = same && bytes.Equal(a[i].Data, c[i].Data)
		}
		if same {
			t.Error("different seeds made the same packets")
		}
	})
}

func FuzzParseVideoMarker(f *testing.F) {
	f.Add(appendEscaped(nil, appendMarker(nil, Marker{RID: "f", Frame: 1, CaptureNS: 2})))
	f.Add([]byte("ISHN\x00\x00\x03\x00\x00\x03"))
	f.Fuzz(func(t *testing.T, body []byte) {
		m, ok := ParseVideoMarker(body)
		if !ok {
			return
		}
		// A parsed marker re-encodes and parses to itself.
		again, ok := ParseVideoMarker(appendEscaped(nil, appendMarker(nil, m)))
		if !ok || again != m {
			t.Fatalf("%+v re-parsed as %+v %v", m, again, ok)
		}
	})
}

func FuzzParseAudioMarker(f *testing.F) {
	f.Add(syntheticOpus(Marker{Beep: true, Frame: 3, CaptureNS: 60_000_000}, newTestRand()))
	f.Add([]byte{0xff, 0x41, 0xff, 0xff, 0x05})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		if m, ok := ParseAudioMarker(pkt); ok && m.RID != "" && len(m.RID) != 1 {
			t.Fatalf("RID %q", m.RID)
		}
	})
}
