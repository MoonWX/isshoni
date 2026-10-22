package fake

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// mustNext returns s's next packet.
func mustNext(t *testing.T, s media.Session) media.Packet {
	t.Helper()
	p, err := s.Next(t.Context())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return p
}

// pullFor returns s's packets for d of (fake) time.
func pullFor(t *testing.T, s media.Session, d time.Duration) []media.Packet {
	t.Helper()
	var out []media.Packet
	for start := time.Now(); time.Since(start) < d; {
		out = append(out, mustNext(t, s))
	}
	return out
}

// skipEvents pulls the EventLayers of Start.
func skipEvents(t *testing.T, s media.Session, n int) []media.Event {
	t.Helper()
	var evs []media.Event
	for range n {
		p := mustNext(t, s)
		if p.Kind != media.KindEvent {
			t.Fatalf("packet %+v, want one of the %d events of Start", p, n)
		}
		evs = append(evs, *p.Event)
	}
	return evs
}

// ofLayer returns the video packets of a layer.
func ofLayer(pkts []media.Packet, rid string) []media.Packet {
	var out []media.Packet
	for _, p := range pkts {
		if p.Kind == media.KindVideo && p.Layer == rid {
			out = append(out, p)
		}
	}
	return out
}

func ofKind(pkts []media.Packet, k media.Kind) []media.Packet {
	var out []media.Packet
	for _, p := range pkts {
		if p.Kind == k {
			out = append(out, p)
		}
	}
	return out
}

// spsOf parses the SPS of a keyframe.
func spsOf(t *testing.T, p media.Packet) testSPS {
	t.Helper()
	if !p.Keyframe {
		t.Fatalf("%q packet at %d ns is not a keyframe", p.Layer, p.CaptureNS)
	}
	return readSPS(t, splitAnnexB(t, p.Data)[0])
}

// TestStartEvents: one EventLayer per layer, then media; each layer starts with a Reinit keyframe, audio with a Reinit
// packet (01 §6.2, 05 §10.4).
func TestStartEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		evs := skipEvents(t, s, 2)
		want := []media.Event{
			{Code: media.EventLayer, Layer: "f", Width: 1920, Height: 1080, FPS: 60, Encoder: "fake", Profile: "6400",
				LiveBitrate: true, NoOSIndicator: true},
			{Code: media.EventLayer, Layer: "q", Width: 640, Height: 360, FPS: 15, Encoder: "fake", Profile: "6400",
				LiveBitrate: true, NoOSIndicator: true},
		}
		if !reflect.DeepEqual(evs, want) {
			t.Errorf("events %+v\nwant %+v", evs, want)
		}
		pkts := pullFor(t, s, time.Second)
		if len(ofKind(pkts, media.KindEvent)) != 0 {
			t.Error("more events after Start's")
		}
		for _, rid := range []string{"f", "q"} {
			v := ofLayer(pkts, rid)
			if !v[0].Keyframe || !v[0].Reinit || v[0].CaptureNS != 0 {
				t.Errorf("layer %s: first packet %+v, want a Reinit keyframe at 0", rid, v[0])
			}
			for _, p := range v[1:] {
				if p.Reinit {
					t.Errorf("layer %s: Reinit at %d ns", rid, p.CaptureNS)
				}
			}
			if sps := spsOf(t, v[0]); sps.profileKey() != "6400" {
				t.Errorf("layer %s: SPS profile %s", rid, sps.profileKey())
			}
		}
		a := ofKind(pkts, media.KindAudio)
		if !a[0].Reinit || a[1].Reinit {
			t.Errorf("audio Reinit flags %v, %v, want the first packet only", a[0].Reinit, a[1].Reinit)
		}

		// Borderless false: NoOSIndicator false.
		_ = s.Stop()
		cfg := testConfig()
		cfg.Borderless = false
		s = mustStart(t, e, cfg)
		if evs := skipEvents(t, s, 2); evs[0].NoOSIndicator || evs[1].NoOSIndicator {
			t.Error("NoOSIndicator without Borderless")
		}
	})
}

// TestCBOnlyEncoder: a session whose encoder lacks the requested profile runs the encoder's (04 §9.6).
func TestCBOnlyEncoder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{Probe: media.Probe{Encoders: []media.EncoderInfo{
			{Name: "openh264", Adapter: -1, Tested: true, LiveBitrate: true, Profiles: []string{"42e0"}},
		}}})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		evs := skipEvents(t, s, 2)
		if evs[0].Profile != "42e0" || evs[0].Encoder != "openh264" {
			t.Errorf("EventLayer %+v, want profile 42e0 on openh264", evs[0])
		}
		if sps := spsOf(t, ofLayer(pullFor(t, s, 100*time.Millisecond), "f")[0]); sps.profileKey() != "42e0" {
			t.Errorf("SPS profile %s, want 42e0", sps.profileKey())
		}
	})
}

// TestTimeline: the session's clock starts at Start; packets come when due, with CaptureNS equal to their Markers'
// and to Now() at arrival, and Origin plus CaptureNS is the arrival instant (01 §6.1, §12.4).
func TestTimeline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		t0 := time.Now()
		s := mustStart(t, e, testConfig())
		if !s.Origin().Equal(t0) || s.Now() != 0 {
			t.Errorf("Origin %v, Now %d at Start, want %v and 0", s.Origin(), s.Now(), t0)
		}
		skipEvents(t, s, 2)
		for range 500 {
			p := mustNext(t, s)
			if now := s.Now(); now != p.CaptureNS {
				t.Fatalf("Now() %d at the arrival of a packet captured at %d", now, p.CaptureNS)
			}
			if at := s.Origin().Add(time.Duration(p.CaptureNS)); !at.Equal(time.Now()) {
				t.Fatalf("Origin+CaptureNS %v, arrival %v", at, time.Now())
			}
			if m := packetMarker(t, p); m.CaptureNS != p.CaptureNS {
				t.Fatalf("%s %q: Marker CaptureNS %d, packet %d", p.Kind, p.Layer, m.CaptureNS, p.CaptureNS)
			}
		}
	})
}

// TestGOP: f has a keyframe every 4 s, q every 2 s (04 §9.4).
func TestGOP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pkts := pullFor(t, s, 8*time.Second+time.Millisecond)
		for rid, want := range map[string][]int64{"f": {0, 4e9, 8e9}, "q": {0, 2e9, 4e9, 6e9, 8e9}} {
			var keys []int64
			for _, p := range ofLayer(pkts, rid) {
				if p.Keyframe {
					keys = append(keys, p.CaptureNS)
				}
			}
			if !reflect.DeepEqual(keys, want) {
				t.Errorf("layer %s: keyframes at %v, want %v", rid, keys, want)
			}
		}
	})
}

// TestKeyframeSpacing: a keyframe request comes out on the next frame, or at the 250 ms mark after the last forced
// keyframe (01 §5.3, 04 §6.4).
func TestKeyframeSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 500*time.Millisecond)
		t0 := s.Now()
		s.RequestKeyframe("f")
		s.RequestKeyframe("x") // ignored
		f := ofLayer(pullFor(t, s, 100*time.Millisecond), "f")
		if !f[0].Keyframe || f[0].Reinit {
			t.Fatalf("first f frame after RequestKeyframe: %+v", f[0])
		}
		s.RequestKeyframe("f") // 100 ms later: waits for the 250 ms mark
		f = ofLayer(pullFor(t, s, 300*time.Millisecond), "f")
		var keys []int64
		for _, p := range f {
			if p.Keyframe {
				keys = append(keys, p.CaptureNS-t0)
			}
		}
		frame := int64(time.Second / 60)
		if len(keys) != 1 || keys[0] < int64(forcedKeySpacing) || keys[0] > int64(forcedKeySpacing)+2*frame {
			t.Errorf("keyframes %v ns after the first request, want one at the 250 ms mark", keys)
		}
	})
}

// TestSetBitrate: the checks, the live path and the Opus target.
func TestSetBitrate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		for _, tc := range []struct {
			layer string
			bps   int
		}{
			{"f", 0}, {"q", 0}, {"f", 99_999}, {"f", 50_000_001}, {"q", 49_999}, {"q", 2_000_001}, {"h", 1_000_000},
			{media.LayerAudio, 0}, {media.LayerAudio, 31_999}, {media.LayerAudio, 510_001},
		} {
			if err := s.SetBitrate(tc.layer, tc.bps); codeOf(err) != media.CodeInvalid {
				t.Errorf("SetBitrate(%q, %d) = %v, want invalid", tc.layer, tc.bps, err)
			}
		}
		if len(s.Calls()) != 0 {
			t.Errorf("refused calls were recorded: %+v", s.Calls())
		}
		skipEvents(t, s, 2)
		pullFor(t, s, 100*time.Millisecond)
		if err := s.SetBitrate("f", 1_000_000); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBitrate(media.LayerAudio, 64_000); err != nil {
			t.Fatal(err)
		}
		s.RequestKeyframe("f") // start a GOP, so 4 s are one GOP
		pkts := pullFor(t, s, 4*time.Second)
		if n := len(ofKind(pkts, media.KindEvent)); n != 0 {
			t.Errorf("%d events after a live SetBitrate", n)
		}
		var bytes int
		for _, p := range ofLayer(pkts, "f") {
			if p.Reinit {
				t.Errorf("Reinit at %d ns after a live SetBitrate", p.CaptureNS)
			}
			bytes += len(p.Data)
		}
		if bps := float64(bytes) * 8 / 4; math.Abs(bps/1e6-1) > 0.05 {
			t.Errorf("f at %.0f bit/s after SetBitrate(f, 1 Mbit/s)", bps)
		}
		st, _ := s.Stats()
		if st.Layers[0].TargetBps != 1_000_000 {
			t.Errorf("TargetBps = %d", st.Layers[0].TargetBps)
		}

		// Without audio, the Opus target is invalid.
		_ = s.Stop()
		cfg := testConfig()
		cfg.Audio = nil
		s = mustStart(t, e, cfg)
		if err := s.SetBitrate(media.LayerAudio, 64_000); codeOf(err) != media.CodeInvalid {
			t.Errorf("SetBitrate(audio) without audio: %v", err)
		}
	})
}

// TestReinitEncoder: with a re-init encoder every SetBitrate that changes a layer's target re-initializes it: its
// EventLayer, then a Reinit keyframe on the same timeline; the other layers and audio go on (01 §6.2).
func TestReinitEncoder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{Probe: media.Probe{Encoders: []media.EncoderInfo{
			{Name: "amf", Adapter: 0, HW: true, Tested: true, Profiles: []string{"6400", "42e0"}},
		}}})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		if evs := skipEvents(t, s, 2); evs[0].LiveBitrate || evs[0].Encoder != "amf" || !evs[0].HW {
			t.Errorf("EventLayer %+v, want amf without live bitrate", evs[0])
		}
		before := pullFor(t, s, time.Second)
		lastF := ofLayer(before, "f")
		last := packetMarker(t, lastF[len(lastF)-1])
		if err := s.SetBitrate("f", 2_000_000); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		i := 0
		for pkts[i].Kind != media.KindEvent {
			if pkts[i].Layer == "f" {
				t.Fatalf("an f packet before the re-init's EventLayer: %+v", pkts[i])
			}
			i++
		}
		if ev := pkts[i].Event; ev.Code != media.EventLayer || ev.Layer != "f" || ev.Encoder != "amf" {
			t.Fatalf("event %+v, want f's EventLayer", ev)
		}
		next := pkts[i+1]
		if next.Kind != media.KindVideo || next.Layer != "f" || !next.Keyframe || !next.Reinit {
			t.Fatalf("after the EventLayer: %+v, want f's Reinit keyframe", next)
		}
		m := packetMarker(t, next)
		if want := int64(last.Frame+1) * int64(time.Second) / 60; m.Frame != last.Frame+1 || next.CaptureNS != want {
			t.Errorf("the re-init's first frame is %d at %d ns, want %d at %d ns", m.Frame, next.CaptureNS,
				last.Frame+1, want)
		}
		if len(ofKind(pkts, media.KindEvent)) != 1 {
			t.Errorf("%d events, want f's EventLayer only", len(ofKind(pkts, media.KindEvent)))
		}
		for _, p := range append(ofLayer(pkts, "q"), ofKind(pkts, media.KindAudio)...) {
			if p.Reinit {
				t.Errorf("%s %q: Reinit at %d ns", p.Kind, p.Layer, p.CaptureNS)
			}
		}
		// The same target again changes nothing.
		if err := s.SetBitrate("f", 2_000_000); err != nil {
			t.Fatal(err)
		}
		if n := len(ofKind(pullFor(t, s, 500*time.Millisecond), media.KindEvent)); n != 0 {
			t.Errorf("%d events after SetBitrate with the same target", n)
		}
	})
}

// TestSetLayerActive: an inactive layer sends nothing; a resumed one starts with a keyframe that is not a Reinit
// (01 §6.2).
func TestSetLayerActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 300*time.Millisecond)
		if err := s.SetLayerActive("h", false); codeOf(err) != media.CodeInvalid {
			t.Errorf("SetLayerActive(h): %v", err)
		}
		if err := s.SetLayerActive("f", false); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		if n := len(ofLayer(pkts, "f")); n != 0 {
			t.Errorf("%d f packets while inactive", n)
		}
		if q, a := len(ofLayer(pkts, "q")), len(ofKind(pkts, media.KindAudio)); q < 14 || a < 49 {
			t.Errorf("%d q and %d audio packets while f was inactive", q, a)
		}
		on := s.Now()
		if err := s.SetLayerActive("f", true); err != nil {
			t.Fatal(err)
		}
		pkts = pullFor(t, s, 500*time.Millisecond)
		f := ofLayer(pkts, "f")
		if len(f) == 0 || !f[0].Keyframe || f[0].Reinit {
			t.Fatalf("f resumed with %+v, want a keyframe that is not a Reinit", f)
		}
		if d := f[0].CaptureNS - on; d > int64(time.Second/60) {
			t.Errorf("f resumed %v after SetLayerActive(f, true), want its next frame", time.Duration(d))
		}
		if n := len(ofKind(pkts, media.KindEvent)); n != 0 {
			t.Errorf("%d events at the resume", n)
		}
		if err := s.SetLayerActive("f", true); err != nil { // already active
			t.Fatal(err)
		}
		if n := len(ofKind(pullFor(t, s, 100*time.Millisecond), media.KindEvent)); n != 0 {
			t.Errorf("%d events", n)
		}
	})
}

// TestInactiveAtStart: a layer that starts inactive sends nothing until it is resumed, with a plain keyframe.
func TestInactiveAtStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		cfg := testConfig()
		cfg.Layers[1].Inactive = true
		s := mustStart(t, e, cfg)
		if evs := skipEvents(t, s, 2); evs[1].Layer != "q" {
			t.Errorf("events %+v, want an EventLayer for the inactive q too", evs)
		}
		if n := len(ofLayer(pullFor(t, s, time.Second), "q")); n != 0 {
			t.Errorf("%d q packets while inactive", n)
		}
		if err := s.SetLayerActive("q", true); err != nil {
			t.Fatal(err)
		}
		q := ofLayer(pullFor(t, s, 500*time.Millisecond), "q")
		if len(q) == 0 || !q[0].Keyframe || q[0].Reinit {
			t.Fatalf("q resumed with %+v, want a keyframe that is not a Reinit", q)
		}
	})
}

// TestSetPaused: paused, Next returns no video while audio goes on; un-paused, video resumes (01 §6.2).
func TestSetPaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 300*time.Millisecond)
		if err := s.SetPaused(true); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		if n := len(ofKind(pkts, media.KindVideo)); n != 0 {
			t.Errorf("%d video packets while paused", n)
		}
		if n := len(ofKind(pkts, media.KindAudio)); n < 49 {
			t.Errorf("%d audio packets while paused, want audio to go on", n)
		}
		resumed := s.Now()
		if err := s.SetPaused(false); err != nil {
			t.Fatal(err)
		}
		pkts = pullFor(t, s, 100*time.Millisecond)
		f := ofLayer(pkts, "f")
		if len(f) < 5 || f[0].CaptureNS-resumed > int64(time.Second/60) {
			t.Errorf("%d f frames in the 100 ms after SetPaused(false), the first %v after it", len(f),
				time.Duration(f[0].CaptureNS-resumed))
		}
		for _, p := range f {
			if p.Reinit {
				t.Errorf("Reinit at %d ns after the pause", p.CaptureNS)
			}
		}
	})
}

// TestPausedReinit: a re-init during a pause comes out at the resume: the EventLayer, then the Reinit keyframe.
func TestPausedReinit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 300*time.Millisecond)
		if err := s.SetPaused(true); err != nil {
			t.Fatal(err)
		}
		if err := s.Reconfigure(media.Reconfig{Profile: "42e0"}); err != nil {
			t.Fatal(err)
		}
		if n := len(ofKind(pullFor(t, s, time.Second), media.KindVideo)); n != 0 {
			t.Errorf("%d video packets while paused", n)
		}
		if err := s.SetPaused(false); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, 500*time.Millisecond)
		for _, rid := range []string{"f", "q"} {
			ev := -1
			for i, p := range pkts {
				if p.Kind == media.KindEvent && p.Event.Layer == rid {
					ev = i
					break
				}
				if p.Kind == media.KindVideo && p.Layer == rid {
					t.Fatalf("layer %s: %+v before its EventLayer", rid, p)
				}
			}
			if ev < 0 || pkts[ev].Event.Profile != "42e0" {
				t.Fatalf("layer %s: no EventLayer with 42e0 after the pause", rid)
			}
			p := pkts[ev+1]
			if p.Layer != rid || !p.Reinit || spsOf(t, p).profileKey() != "42e0" {
				t.Errorf("layer %s: after its EventLayer %+v, want a CB Reinit keyframe", rid, p)
			}
		}
	})
}

// reinitOf finds a layer's EventLayer and the Reinit keyframe right after it.
func reinitOf(t *testing.T, pkts []media.Packet, rid string) (media.Event, media.Packet, int) {
	t.Helper()
	for i, p := range pkts {
		switch {
		case p.Kind == media.KindEvent && p.Event.Code == media.EventLayer && p.Event.Layer == rid:
			if i+1 == len(pkts) {
				t.Fatalf("layer %s: nothing after its EventLayer", rid)
			}
			n := pkts[i+1]
			if n.Kind != media.KindVideo || n.Layer != rid || !n.Keyframe || !n.Reinit {
				t.Fatalf("layer %s: %+v after its EventLayer, want a Reinit keyframe", rid, n)
			}
			return *p.Event, n, i
		case p.Kind == media.KindVideo && p.Layer == rid && p.Reinit:
			t.Fatalf("layer %s: a Reinit packet without an EventLayer", rid)
		}
	}
	t.Fatalf("layer %s: no re-init", rid)
	return media.Event{}, media.Packet{}, 0
}

// TestReconfigureProfile: Reconfigure{Profile} gives every layer a new encoder in that profile on the same timeline:
// an EventLayer, then a Reinit keyframe whose SPS has the new profile, the frames going on with the next index and
// capture time; audio goes on (01 §6.2).
func TestReconfigureProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		before := pullFor(t, s, 1100*time.Millisecond)
		if err := s.Reconfigure(media.Reconfig{Profile: "640c"}); codeOf(err) != media.CodeInvalid {
			t.Errorf("Reconfigure{640c}: %v", err)
		}
		if err := s.Reconfigure(media.Reconfig{Profile: "42e0"}); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		for _, rid := range []string{"f", "q"} {
			ev, p, _ := reinitOf(t, pkts, rid)
			if ev.Profile != "42e0" {
				t.Errorf("layer %s: EventLayer profile %s", rid, ev.Profile)
			}
			if sps := spsOf(t, p); sps.profileKey() != "42e0" {
				t.Errorf("layer %s: SPS profile %s, want 42e0", rid, sps.profileKey())
			}
			old := ofLayer(before, rid)
			last, first := packetMarker(t, old[len(old)-1]), packetMarker(t, p)
			// Frame n of a layer at fps is captured at n/fps s.
			want := int64(last.Frame+1) * int64(time.Second) / map[string]int64{"f": 60, "q": 15}[rid]
			if first.Frame != last.Frame+1 || p.CaptureNS != want {
				t.Errorf("layer %s: the new encoder's first frame is %d at %d ns, want %d at %d", rid, first.Frame,
					p.CaptureNS, last.Frame+1, want)
			}
		}
		for _, p := range ofKind(pkts, media.KindAudio) {
			if p.Reinit {
				t.Error("audio re-initialized")
			}
		}
		// And back to High.
		if err := s.Reconfigure(media.Reconfig{Profile: "6400"}); err != nil {
			t.Fatal(err)
		}
		pkts = pullFor(t, s, time.Second)
		if _, p, _ := reinitOf(t, pkts, "f"); spsOf(t, p).profileKey() != "6400" {
			t.Error("not back to High")
		}

		// Decodable is Constrained Baseline only; the same profile again still re-initializes.
		d := NewEngine(EngineConfig{Mode: Decodable, SourceSize: [2]int{640, 360}})
		defer d.Close()
		cfg := testConfig()
		cfg.Profile = "42e0"
		ds := mustStart(t, d, cfg)
		skipEvents(t, ds, 2)
		pullFor(t, ds, 100*time.Millisecond)
		if err := ds.Reconfigure(media.Reconfig{Profile: "6400"}); codeOf(err) != media.CodeInvalid {
			t.Errorf("Decodable Reconfigure{6400}: %v", err)
		}
		if err := ds.Reconfigure(media.Reconfig{Profile: "42e0"}); err != nil {
			t.Fatal(err)
		}
		pkts = pullFor(t, ds, 500*time.Millisecond)
		for _, rid := range []string{"f", "q"} {
			reinitOf(t, pkts, rid)
		}
	})
}

// TestReconfigureLayers: a layer's new rate or size re-initializes that layer only, on the same timeline.
func TestReconfigureLayers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		before := ofLayer(pullFor(t, s, 500*time.Millisecond), "f")
		last := packetMarker(t, before[len(before)-1])
		for _, bad := range []media.Reconfig{
			{Layers: []media.LayerConfig{{RID: "h", MaxFPS: 30}}},
			{Layers: []media.LayerConfig{{RID: "f", MaxFPS: -1}}},
			{Layers: []media.LayerConfig{{RID: "f", MaxFPS: 241}}},
			{Layers: []media.LayerConfig{{RID: "f", MaxPixels: 3}}},
			{Layers: []media.LayerConfig{{RID: "q", Bitrate: 2_000_001}}},
			{Preset: "fast"},
		} {
			if err := s.Reconfigure(bad); codeOf(err) != media.CodeInvalid {
				t.Errorf("Reconfigure(%+v): %v, want invalid", bad, err)
			}
		}
		if err := s.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "f", MaxFPS: 30}}}); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		ev, p, _ := reinitOf(t, pkts, "f")
		if ev.FPS != 30 || ev.Width != 1920 {
			t.Errorf("EventLayer %+v, want f at 1920x1080@30", ev)
		}
		f := ofLayer(pkts, "f")
		want := int64(last.Frame+1) * int64(time.Second) / 60
		if m := packetMarker(t, p); m.Frame != last.Frame+1 || p.CaptureNS != want {
			t.Errorf("first frame at 30 fps: %d at %d ns, want %d at %d", m.Frame, p.CaptureNS, last.Frame+1, want)
		}
		for i := 1; i < len(f); i++ {
			if d := f[i].CaptureNS - f[i-1].CaptureNS; d < int64(time.Second)/30-1 || d > int64(time.Second)/30+1 {
				t.Fatalf("f frames %d ns apart at 30 fps", d)
			}
			if packetMarker(t, f[i]).Frame != packetMarker(t, f[i-1]).Frame+1 {
				t.Fatal("f's frame indices jump")
			}
		}
		for _, q := range ofLayer(pkts, "q") {
			if q.Reinit {
				t.Error("q re-initialized")
			}
		}
		// A new size; a Bitrate alone with a live encoder changes nothing else.
		if err := s.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "q", MaxPixels: 320 * 180},
			{RID: "f", Bitrate: 6_000_000}}}); err != nil {
			t.Fatal(err)
		}
		pkts = pullFor(t, s, time.Second)
		ev, p, _ = reinitOf(t, pkts, "q")
		if sps := spsOf(t, p); ev.Width != 320 || ev.Height != 180 || sps.width != 320 || sps.height != 180 {
			t.Errorf("q after Reconfigure: EventLayer %dx%d, SPS %dx%d, want 320x180", ev.Width, ev.Height,
				sps.width, sps.height)
		}
		if n := len(ofKind(pkts, media.KindEvent)); n != 1 {
			t.Errorf("%d events, want q's only", n)
		}
		st, _ := s.Stats()
		if st.Layers[0].TargetBps != 6_000_000 {
			t.Errorf("f's target %d", st.Layers[0].TargetBps)
		}
		// A size above H.264 level 6.2 is refused and changes nothing.
		big := NewEngine(EngineConfig{SourceSize: [2]int{8192, 8192}})
		defer big.Close()
		bs := mustStart(t, big, testConfig())
		if err := bs.Reconfigure(media.Reconfig{Layers: []media.LayerConfig{{RID: "f", MaxPixels: 8192 * 8192}}}); codeOf(err) != media.CodeInvalid {
			t.Errorf("Reconfigure above level 6.2: %v", err)
		}
		skipEvents(t, bs, 2)
		if n := len(ofKind(pullFor(t, bs, 200*time.Millisecond), media.KindEvent)); n != 0 {
			t.Errorf("%d events after a refused Reconfigure", n)
		}
	})
}

// TestReconfigurePreset: a new preset re-initializes every layer; the same one again changes nothing.
func TestReconfigurePreset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 100*time.Millisecond)
		if err := s.Reconfigure(media.Reconfig{Preset: "text"}); err != nil {
			t.Fatal(err)
		}
		pkts := pullFor(t, s, time.Second)
		reinitOf(t, pkts, "f")
		reinitOf(t, pkts, "q")
		if err := s.Reconfigure(media.Reconfig{Preset: "text"}); err != nil {
			t.Fatal(err)
		}
		if n := len(ofKind(pullFor(t, s, 500*time.Millisecond), media.KindEvent)); n != 0 {
			t.Errorf("%d events after the same preset again", n)
		}
	})
}

// TestFail: the next Next returns Fail's error, also a Next that waits, and the session stops.
func TestFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 100*time.Millisecond)
		res := make(chan error)
		go func() {
			for {
				if _, err := s.Next(t.Context()); err != nil {
					res <- err
					return
				}
			}
		}()
		synctest.Wait()
		t0 := time.Now()
		s.Fail(&media.Error{Code: media.CodeSourceGone, Op: "next"})
		s.Fail(&media.Error{Code: media.CodeEncoderLost}) // the first one wins
		err := <-res
		if !errors.Is(err, media.ErrSourceGone) || time.Since(t0) != 0 {
			t.Errorf("Next after Fail: %v after %v, want source_gone at once", err, time.Since(t0))
		}
		if _, err := s.Next(t.Context()); !errors.Is(err, media.ErrStopped) {
			t.Errorf("Next after the failure: %v", err)
		}
		if e.Session() != nil {
			t.Error("the failed session still runs")
		}
		if err := s.SetPaused(true); !errors.Is(err, media.ErrStopped) {
			t.Errorf("SetPaused after the failure: %v", err)
		}
		if err := s.Stop(); err != nil {
			t.Errorf("Stop after the failure: %v", err)
		}
		// Fail(nil) ends the session cleanly; Fail after Stop is ignored.
		s = mustStart(t, e, testConfig())
		s.Fail(nil)
		if _, err := s.Next(t.Context()); !errors.Is(err, media.ErrStopped) {
			t.Errorf("Next after Fail(nil): %v", err)
		}
		s = mustStart(t, e, testConfig())
		_ = s.Stop()
		s.Fail(&media.Error{Code: media.CodeInternal})
		if _, err := s.Next(t.Context()); !errors.Is(err, media.ErrStopped) {
			t.Errorf("Next after Stop and Fail: %v", err)
		}
	})
}

// TestEmit: emitted events come before any more media, also to a Next that waits; an EventLayer that names an
// encoder becomes the layer's.
func TestEmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 100*time.Millisecond)
		changed := media.Event{Code: media.EventEncoderChanged, Layer: "f", Encoder: "mf-sw"}
		layer := media.Event{Code: media.EventLayer, Layer: "f", Width: 1920, Height: 1080, FPS: 60, Encoder: "mf-sw",
			Profile: "6400"}
		res := make(chan media.Packet)
		go func() { // takes the media that is due, then waits in Next until the event
			for {
				p, err := s.Next(t.Context())
				if err != nil || p.Kind == media.KindEvent {
					res <- p
					return
				}
			}
		}()
		synctest.Wait()
		t0 := time.Now()
		s.Emit(changed)
		s.Emit(layer)
		if p := <-res; p.Kind != media.KindEvent || *p.Event != changed || time.Since(t0) != 0 {
			t.Errorf("Next after Emit: %+v after %v, want the event at once", p, time.Since(t0))
		}
		if p := mustNext(t, s); p.Kind != media.KindEvent || *p.Event != layer {
			t.Errorf("second packet %+v, want the EventLayer", p)
		}
		st, _ := s.Stats()
		if st.Layers[0].Encoder != "mf-sw" || st.Layers[1].Encoder != "fake" {
			t.Errorf("Stats encoders %q, %q", st.Layers[0].Encoder, st.Layers[1].Encoder)
		}
		// mf-sw re-initializes on a bitrate change.
		if err := s.SetBitrate("f", 2_000_000); err != nil {
			t.Fatal(err)
		}
		ev, _, _ := reinitOf(t, pullFor(t, s, 500*time.Millisecond), "f")
		if ev.Encoder != "mf-sw" || ev.LiveBitrate {
			t.Errorf("the re-init's EventLayer %+v, want mf-sw without live bitrate", ev)
		}
		_ = s.Stop()
		s.Emit(changed) // ignored
	})
}

// TestCalls: the accepted control calls in order (01 §6.2).
func TestCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		s := mustStart(t, e, testConfig())
		t0 := time.Now()
		s.RequestKeyframe("f")
		s.RequestKeyframe("x")
		time.Sleep(time.Second)
		_ = s.SetBitrate("q", 500_000)
		_ = s.SetBitrate("f", 0)
		_ = s.SetBitrate(media.LayerAudio, 96_000)
		_ = s.SetLayerActive("f", false)
		_ = s.SetPaused(true)
		rc := media.Reconfig{Preset: "game", Layers: []media.LayerConfig{{RID: "f", MaxFPS: 30}}}
		_ = s.Reconfigure(rc)
		rc.Layers[0].MaxFPS = 1 // the recorded call is a copy
		_ = e.SetAudioMuted(ctx, true)
		_ = e.SetAudioMode(ctx, media.AudioEverything) // invalid: not recorded
		_ = e.SetAudioMode(ctx, media.AudioOff)
		_ = e.SetExclusions(ctx, media.Exclusions{KeepOutMicUsers: true})
		_ = s.Stop()
		_ = s.Stop()
		t1 := t0.Add(time.Second)
		want := []Call{
			{At: t0, Op: "keyframe", Layer: "f"},
			{At: t1, Op: "bitrate", Layer: "q", Bps: 500_000},
			{At: t1, Op: "bitrate", Layer: media.LayerAudio, Bps: 96_000},
			{At: t1, Op: "active", Layer: "f"},
			{At: t1, Op: "paused", On: true},
			{At: t1, Op: "reconfigure", Reconf: media.Reconfig{Preset: "game",
				Layers: []media.LayerConfig{{RID: "f", MaxFPS: 30}}}},
			{At: t1, Op: "audio_muted", On: true, Muted: true},
			{At: t1, Op: "audio_mode", Mode: media.AudioOff},
			{At: t1, Op: "exclusions"},
			{At: t1, Op: "stop"},
		}
		if got := s.Calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("Calls:\n%+v\nwant\n%+v", got, want)
		}
		calls := s.Calls()
		calls[5].Reconf.Layers[0].RID = "x"
		if s.Calls()[5].Reconf.Layers[0].RID != "f" {
			t.Error("Calls returned the session's own slice")
		}
	})
}

// TestAudioMuted: muted, the session's audio is Opus silence with the tone's Markers; SetAudioMuted and SetAudioMode
// need a running session with audio (05 §13.1).
func TestAudioMuted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		ctx := t.Context()
		if err := e.SetAudioMuted(ctx, true); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMuted without a session: %v", err)
		}
		if err := e.SetAudioMode(ctx, media.AudioOff); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMode without a session: %v", err)
		}
		cfg := testConfig()
		cfg.Audio = nil
		s := mustStart(t, e, cfg)
		if err := e.SetAudioMuted(ctx, true); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMuted in a session without audio: %v", err)
		}
		if err := e.SetAudioMode(ctx, media.AudioOff); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("SetAudioMode in a session without audio: %v", err)
		}
		_ = s.Stop()

		s = mustStart(t, e, testConfig())
		for m, want := range map[media.AudioMode]media.ErrorCode{
			media.AudioOff: "", media.AudioExcludeApps: "", media.AudioEverything: media.CodeInvalid,
			media.AudioTestTone: media.CodeInvalid, "": media.CodeInvalid,
		} {
			if err := e.SetAudioMode(ctx, m); codeOf(err) != want {
				t.Errorf("SetAudioMode(%q) = %v, want %q", m, err, want)
			}
		}
		if err := e.SetAudioMode(ctx, media.AudioExcludeApps); err != nil { // sound on, whatever the map's order
			t.Fatal(err)
		}
		skipEvents(t, s, 2)
		tone := ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)
		if err := e.SetAudioMuted(ctx, true); err != nil {
			t.Fatal(err)
		}
		muted := ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)
		silence := silenceOpus().frames[0]
		for i, p := range muted {
			op, err := parseOpus(p.Data)
			if err != nil || len(op.frames) != 1 || !bytes.Equal(op.frames[0], silence) || op.samples() != audioPacketSamples {
				t.Fatalf("muted packet %d: %v, %+v", i, err, op)
			}
			if m := packetMarker(t, p); m.CaptureNS != p.CaptureNS || m.Beep {
				t.Errorf("muted packet %d: Marker %+v", i, m)
			}
		}
		if packetMarker(t, muted[0]).Frame != packetMarker(t, tone[len(tone)-1]).Frame+1 {
			t.Error("the Markers' packet indices jump at the mute")
		}
		if err := e.SetAudioMuted(ctx, false); err != nil {
			t.Fatal(err)
		}
		unmuted := ofKind(pullFor(t, s, 200*time.Millisecond), media.KindAudio)
		if op, _ := parseOpus(unmuted[0].Data); bytes.Equal(op.frames[0], silence) {
			t.Error("still silent after SetAudioMuted(false)")
		}
	})
}

// TestAudioOnlyPCM: a session without a Source and layers runs audio only; with TapPCM it also returns 10 ms PCM
// chunks of the tone (01 §6.2).
func TestAudioOnlyPCM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, media.Config{Audio: &media.AudioConfig{Mode: media.AudioTestTone, TapPCM: true}})
		s.RequestKeyframe("f") // ignored
		if err := s.SetBitrate("f", 1_000_000); codeOf(err) != media.CodeInvalid {
			t.Errorf("SetBitrate(f) on an audio-only session: %v", err)
		}
		if err := s.Reconfigure(media.Reconfig{Profile: "42e0"}); err != nil {
			t.Errorf("Reconfigure{Profile} on an audio-only session: %v", err)
		}
		var pcm, audio []media.Packet
		for start := time.Now(); time.Since(start) < time.Second; {
			p := mustNext(t, s)
			if s.Now() != p.CaptureNS {
				t.Fatalf("%s at %d ns came at %d", p.Kind, p.CaptureNS, s.Now())
			}
			if p.CaptureNS >= int64(time.Second) {
				break
			}
			switch p.Kind {
			case media.KindPCM:
				pcm = append(pcm, p)
			case media.KindAudio:
				audio = append(audio, p)
			default:
				t.Fatalf("a %s packet in an audio-only session", p.Kind)
			}
		}
		if len(audio) != 50 || len(pcm) != 100 || !audio[0].Reinit {
			t.Fatalf("%d audio and %d PCM packets in 1 s (first Reinit %v), want 50 and 100", len(audio), len(pcm),
				audio[0].Reinit)
		}
		for i, p := range pcm {
			if p.CaptureNS != int64(i)*int64(10*time.Millisecond) || len(p.Data) != pcmChunkBytes || p.Layer != "" {
				t.Fatalf("PCM chunk %d: %d bytes at %d ns", i, len(p.Data), p.CaptureNS)
			}
		}
		sample := func(p media.Packet, frame, ch int) float32 {
			return math.Float32frombits(binary.LittleEndian.Uint32(p.Data[8*frame+4*ch:]))
		}
		// The beep at 1 kHz in the first 100 ms: a quarter period is 12 samples; then the 440 Hz tone.
		if l, r := sample(pcm[0], 12, 0), sample(pcm[0], 12, 1); math.Abs(float64(l)-0.25) > 1e-6 || l != r {
			t.Errorf("PCM sample 12: %v, %v, want 0.25 on both channels", l, r)
		}
		want := float32(0.25 * math.Sin(2*math.Pi*440*4801/48000))
		if got := sample(pcm[10], 1, 0); math.Abs(float64(got-want)) > 1e-6 {
			t.Errorf("PCM sample 4801: %v, want %v", got, want)
		}
		if err := e.SetAudioMuted(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		for _, p := range pullFor(t, s, 100*time.Millisecond) {
			if p.Kind == media.KindPCM && !bytes.Equal(p.Data, make([]byte, pcmChunkBytes)) {
				t.Fatal("a muted PCM chunk is not silent")
			}
		}
		st, err := s.Stats()
		if err != nil || len(st.Layers) != 0 {
			t.Errorf("Stats of an audio-only session: %+v, %v", st, err)
		}
	})
}

// TestStop: Stop unblocks Next at once; Next after Stop returns ErrStopped; the controls return stopped.
func TestStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		mustNext(t, s)
		res := make(chan error)
		go func() {
			for {
				if _, err := s.Next(t.Context()); err != nil {
					res <- err
					return
				}
			}
		}()
		synctest.Wait()
		t0 := time.Now()
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := <-res; !errors.Is(err, media.ErrStopped) || time.Since(t0) != 0 {
			t.Errorf("a waiting Next after Stop: %v after %v", err, time.Since(t0))
		}
		if _, err := s.Next(t.Context()); !errors.Is(err, media.ErrStopped) {
			t.Errorf("Next after Stop: %v", err)
		}
		if err := s.Stop(); err != nil {
			t.Errorf("second Stop: %v", err)
		}
		for name, err := range map[string]error{
			"SetBitrate":     s.SetBitrate("f", 1_000_000),
			"SetLayerActive": s.SetLayerActive("f", false),
			"SetPaused":      s.SetPaused(true),
			"Reconfigure":    s.Reconfigure(media.Reconfig{Profile: "42e0"}),
		} {
			if !errors.Is(err, media.ErrStopped) {
				t.Errorf("%s after Stop: %v", name, err)
			}
		}
		s.RequestKeyframe("f")
		if _, err := s.Stats(); !errors.Is(err, media.ErrStopped) {
			t.Errorf("Stats after Stop: %v", err)
		}
		if e.Session() != nil {
			t.Error("Session() after Stop")
		}
		if n := len(s.Calls()); n != 1 {
			t.Errorf("%d calls, want the stop only", n)
		}
	})
}

// TestNextCtx: Next returns ctx's error as soon as ctx ends, and the session goes on.
func TestNextCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		cfg := testConfig()
		cfg.Audio = nil
		cfg.Layers = cfg.Layers[:1]
		cfg.Layers[0].MaxFPS = 1
		s := mustStart(t, e, cfg)
		skipEvents(t, s, 1)
		mustNext(t, s) // frame 0; frame 1 is due in 1 s
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		t0 := time.Now()
		if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) != 100*time.Millisecond {
			t.Errorf("Next with a 100 ms ctx: %v after %v", err, time.Since(t0))
		}
		if p := mustNext(t, s); p.CaptureNS != int64(time.Second) {
			t.Errorf("the frame after the cancelled Next: %+v", p)
		}
		done, cancel2 := context.WithCancel(t.Context())
		cancel2()
		if _, err := s.Next(done); !errors.Is(err, context.Canceled) {
			t.Errorf("Next with a cancelled ctx: %v", err)
		}
	})
}

// TestStats: the session's own numbers over the last 2 s.
func TestStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		s := mustStart(t, e, testConfig())
		skipEvents(t, s, 2)
		pullFor(t, s, 4*time.Second+time.Millisecond)
		st, err := s.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Layers) != 2 {
			t.Fatalf("Stats %+v", st)
		}
		for i, want := range []struct {
			rid    string
			fps    float64
			target int
		}{{"f", 60, 4_000_000}, {"q", 15, 300_000}} {
			l := st.Layers[i]
			if l.RID != want.rid || l.Encoder != "fake" || l.HW || l.Limit != "none" || l.TargetBps != want.target ||
				math.Abs(l.FPSOut-want.fps) > 1 || math.Abs(l.FPSIn-want.fps) > 1 || l.Drops != 0 {
				t.Errorf("layer %d: %+v", i, l)
			}
			if r := float64(l.BitrateBps) / float64(want.target); r < 0.5 || r > 1.5 {
				t.Errorf("layer %s: %d bit/s, target %d", want.rid, l.BitrateBps, want.target)
			}
		}
		if err := s.SetPaused(true); err != nil {
			t.Fatal(err)
		}
		pullFor(t, s, 2100*time.Millisecond)
		st, _ = s.Stats()
		if l := st.Layers[0]; l.FPSOut != 0 || math.Abs(l.FPSIn-60) > 1 || l.BitrateBps != 0 {
			t.Errorf("paused: %+v, want frames in but none out", l)
		}
	})
}

// TestSourceNowOrigin: the Source's clock and its origin, which the 200 ms lag cap moves (01 §6.1).
func TestSourceNowOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(Config{Layers: []VideoLayer{}, Audio: true})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.Now() != 0 || !s.Origin().IsZero() {
			t.Errorf("before the first Next: Now %d, Origin %v", s.Now(), s.Origin())
		}
		time.Sleep(time.Second)
		t0 := time.Now()
		if _, err := s.Next(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !s.Origin().Equal(t0) || s.Now() != 0 {
			t.Errorf("after the first Next: Origin %v, Now %d, want %v and 0", s.Origin(), s.Now(), t0)
		}
		time.Sleep(time.Second) // packet 1 is now 980 ms overdue: the clock skips 780 ms
		p, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		skip := 780 * time.Millisecond
		if !s.Origin().Equal(t0.Add(skip)) || s.Now() != int64(time.Second-skip) || p.CaptureNS != int64(20*time.Millisecond) {
			t.Errorf("after the skip: Origin +%v, Now %d, packet at %d; want +%v, %d, 20 ms", s.Origin().Sub(t0),
				s.Now(), p.CaptureNS, skip, time.Second-skip)
		}
	})
}
