package publish

import (
	"errors"
	"slices"
	"testing"
)

// TestSetActive: a layer that SetActive turns off sends nothing and writes no sender report, and the other layer goes
// on; turned on again, it asks the Source for a keyframe and sends nothing before one, as a browser's encoder that
// starts an encoding again (02 §11). Turning a layer to the state it has asks for nothing.
func TestSetActive(t *testing.T) {
	rec := &profileSource{Source: newSource(t)}
	p, err := New(Options{Source: rec})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	writers := map[string]*recWriter{}
	for _, tr := range p.video {
		writers[tr.layer] = &recWriter{}
		tr.b = &binding{w: writers[tr.layer], ssrc: uint32(len(writers)), pt: videoPT}
	}
	sendFrame := func(layer string, n int, key bool) {
		pkt := frame(n)
		pkt.Layer, pkt.Keyframe = layer, key
		p.send(pkt)
	}
	written := func(layer string) int {
		w := writers[layer]
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.hdrs)
	}
	stats := func(layer string) TrackStats {
		for _, ts := range p.Stats().Tracks {
			if ts.Layer == layer {
				return ts
			}
		}
		t.Fatalf("no track for layer %q", layer)
		return TrackStats{}
	}
	srOf := func(layer string) bool {
		for _, tr := range p.video {
			if tr.layer == layer {
				_, ok := tr.senderReport()
				return ok
			}
		}
		return false
	}

	sendFrame("f", 0, true)
	sendFrame("q", 0, true)
	if written("f") != 3 || written("q") != 3 || !srOf("f") {
		t.Fatalf("before any SetActive: f wrote %d packets, q %d; want a frame (3) each, and an SR", written("f"),
			written("q"))
	}

	if err := p.SetActive("f", true); err != nil || len(rec.keyframes) != 0 {
		t.Fatalf("turning on a layer that is on: %v, keyframe requests %v", err, rec.keyframes)
	}
	if err := p.SetActive("f", false); err != nil {
		t.Fatal(err)
	}
	if err := p.SetActive("f", false); err != nil || len(rec.keyframes) != 0 {
		t.Fatalf("turning off: %v, keyframe requests %v; want none", err, rec.keyframes)
	}
	for n := 1; n <= 3; n++ {
		sendFrame("f", n, n == 2)
		sendFrame("q", n, false)
	}
	if written("f") != 3 || written("q") != 12 {
		t.Errorf("while f is off: f wrote %d packets, q %d; want 3 (as before) and 12", written("f"), written("q"))
	}
	if st := stats("f"); st.Paused != 3 || st.Frames != 1 || p.Stats().Dropped != 0 {
		t.Errorf("f's stats while off = %+v, dropped %d; want 3 paused, 1 frame, nothing dropped", st, p.Stats().Dropped)
	}
	if srOf("f") || !srOf("q") {
		t.Error("a sender report for the layer that is off, or none for the other one")
	}

	// On again: a keyframe is asked for, and nothing goes out before it.
	if err := p.SetActive("f", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.keyframes, []string{"f"}) {
		t.Errorf("keyframe requests after turning f on: %v, want [f]", rec.keyframes)
	}
	sendFrame("f", 4, false)
	if written("f") != 3 || stats("f").Paused != 4 {
		t.Errorf("a delta frame after turning f on went out (%d packets written)", written("f"))
	}
	sendFrame("f", 5, true)
	sendFrame("f", 6, false)
	if written("f") != 9 || stats("f").Keyframes != 2 || !srOf("f") {
		t.Errorf("after f's keyframe: %d packets written, stats %+v; want the keyframe and the frame after it",
			written("f"), stats("f"))
	}
	if stats("f").Paused != 4 {
		t.Errorf("Paused = %d, want 4", stats("f").Paused)
	}

	if err := p.SetActive("h", false); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("SetActive of a layer the Publisher doesn't send = %v", err)
	}
	audioOnly, err := New(Options{Source: newSource(t), Layers: []string{}, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer audioOnly.Close()
	if err := audioOnly.SetActive("", false); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("SetActive without video = %v", err)
	}
}
