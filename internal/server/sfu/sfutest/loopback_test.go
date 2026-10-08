package sfutest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// These are README S13's acceptance tests (02 §20 slice 4): a publish.Publisher sending fake.Source media straight
// to a Viewer over loopback, without the SFU.

// loopback connects a Publisher of src to a new Viewer and starts it.
func loopback(t *testing.T, src *fake.Source, po publish.Options) (*publish.Publisher, *Viewer) {
	t.Helper()
	return loopbackWith(t, src, po, LoopbackSettings())
}

// loopbackWith is loopback with the Publisher's Pion settings.
func loopbackWith(t *testing.T, src *fake.Source, po publish.Options, se webrtc.SettingEngine) (
	*publish.Publisher, *Viewer,
) {
	t.Helper()
	po.Source = src
	po.Settings = se
	pub, err := publish.New(po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Error(err)
		}
		_ = src.Close()
	})
	v, err := NewViewer(ViewerOptions{Settings: LoopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	offer, err := pub.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := v.Answer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pub, v
}

// track waits for the Viewer's track of a kind and rid.
func track(t *testing.T, v *Viewer, kind webrtc.RTPCodecType, rid string) *Recorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r, err := v.WaitRecorder(ctx, func(r *Recorder) bool { return r.Kind() == kind && r.RID() == rid })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// keyframes counts the frames with a keyframe marker.
func keyframes(r *Recorder) int {
	n := 0
	for _, f := range Frames(r.Packets()) {
		if f.HasMark && f.Mark.Keyframe {
			n++
		}
	}
	return n
}

// TestLoopbackMedia: the viewer sees each layer's configured bitrate within ±5%, every marker intact, continuous
// streams that start on an SPS, a sender report every second whose NTP and RTP times are the capture time and
// timestamp of a frame, and flash and beep in sync.
func TestLoopbackMedia(t *testing.T) {
	layers := fake.DefaultLayers() // f 1080p60 at 8 Mbps, q 360p15 at 0.3 Mbps
	src, err := fake.New(fake.Config{Layers: layers, GOP: time.Second, Audio: true, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopback(t, src, publish.Options{Audio: true, StreamID: "share"})
	recs := map[string]*Recorder{
		"f": track(t, v, webrtc.RTPCodecTypeVideo, "f"),
		"q": track(t, v, webrtc.RTPCodecTypeVideo, "q"),
		"":  track(t, v, webrtc.RTPCodecTypeAudio, ""),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, rid := range []string{"f", "q"} {
		// Six keyframes: five whole GOPs to measure, and five SRs.
		if err := recs[rid].Wait(ctx, func(r *Recorder) bool { return keyframes(r) >= 6 }); err != nil {
			t.Fatal(err)
		}
	}
	// Let the last repairs land before counting the final loss.
	time.Sleep(FinalLossAfter + 100*time.Millisecond)

	ps := pub.Stats()
	if ps.Dropped != 0 {
		t.Errorf("publisher dropped %d packets", ps.Dropped)
	}
	for _, ts := range ps.Tracks {
		if ts.WriteErrors != 0 || ts.SRs < 5 {
			t.Errorf("publisher track %s/%q: %d write errors, %d SRs", ts.Kind, ts.RID, ts.WriteErrors, ts.SRs)
		}
	}
	for rid, r := range recs {
		pkts := r.Packets()
		st := r.Stats()
		t.Logf("%s %q: %d packets, %d gaps, %d recovered, %d lost, %d SRs", r.Kind(), rid, st.Packets, st.Gaps,
			st.Recovered, st.Lost, st.SRs)
		if st.Lost != 0 || st.Duplicates != 0 {
			t.Errorf("%q: %d lost, %d duplicates", rid, st.Lost, st.Duplicates)
		}
		if r.StreamID() != "share" || r.MID() == "" {
			t.Errorf("%q: stream %q, mid %q", rid, r.StreamID(), r.MID())
		}
		if err := CheckContinuous(pkts); err != nil {
			t.Errorf("%q: %v", rid, err)
		}
		checkSRs(t, rid, r)
		if r.Kind() == webrtc.RTPCodecTypeAudio {
			if err := CheckAudioMarkers(pkts); err != nil {
				t.Errorf("audio: %v", err)
			}
			continue
		}
		if err := CheckStartsOnSPS(pkts); err != nil {
			t.Errorf("%q: %v", rid, err)
		}
		pkts = WithoutPartialTail(pkts)
		if err := CheckVideoMarkers(pkts); err != nil {
			t.Errorf("%q: %v", rid, err)
		}
		frames := Frames(pkts)
		for _, f := range frames {
			if f.Mark.RID != rid {
				t.Fatalf("track %q carries a frame of layer %q", rid, f.Mark.RID)
			}
		}
		bps, err := GOPBitrate(frames)
		if err != nil {
			t.Fatal(err)
		}
		want := layers[0].Bitrate
		if rid == "q" {
			want = layers[1].Bitrate
		}
		t.Logf("%q: %.0f bps over whole GOPs, configured %d", rid, bps, want)
		if math.Abs(bps/float64(want)-1) > 0.05 {
			t.Errorf("%q: %.0f bps, want %d ± 5%%", rid, bps, want)
		}
	}

	samples, err := v.AVOffset("share")
	if err != nil {
		t.Fatal(err)
	}
	layersSeen := map[string]int{}
	for _, s := range samples {
		layersSeen[s.Layer]++
		if s.Offset < -time.Millisecond || s.Offset > time.Millisecond {
			t.Errorf("A/V offset %v at %s frame %d", s.Offset, s.Layer, s.Frame)
		}
	}
	if layersSeen["f"] < 3 || layersSeen["q"] < 3 {
		t.Errorf("flash/beep pairs per layer: %v", layersSeen)
	}
}

// checkSRs checks a track's sender reports: one per second (each within ±250 ms of the last, ±5% on average), each
// describing a frame the viewer received (RTP = its timestamp), with NTP times that advance exactly like those
// frames' capture times.
func checkSRs(t *testing.T, rid string, r *Recorder) {
	t.Helper()
	srs := r.SRs()
	if len(srs) < 5 {
		t.Errorf("%q: %d SRs, want at least 5", rid, len(srs))
		return
	}
	capture := map[uint32]int64{} // RTP timestamp → capture ns
	for _, p := range r.Packets() {
		if p.HasMark {
			capture[p.TS] = p.Mark.CaptureNS
		}
	}
	for i := 1; i < len(srs); i++ {
		a, b := srs[i-1], srs[i]
		if gap := b.Arrival.Sub(a.Arrival); gap < 750*time.Millisecond || gap > 1250*time.Millisecond {
			t.Errorf("%q: SR %d arrived %v after the previous one", rid, i, gap)
		}
		if b.Packets <= a.Packets || b.Octets <= a.Octets {
			t.Errorf("%q: SR %d counts %d/%d after %d/%d", rid, i, b.Packets, b.Octets, a.Packets, a.Octets)
		}
		ca, okA := capture[a.RTP]
		cb, okB := capture[b.RTP]
		if !okA || !okB {
			t.Errorf("%q: SR %d names RTP times %d, %d of no received frame", rid, i, a.RTP, b.RTP)
			continue
		}
		ntp := ntpNanos(b.NTP) - ntpNanos(a.NTP)
		if d := ntp - (cb - ca); d < -1000 || d > 1000 {
			t.Errorf("%q: SR %d: NTP advanced %d ns, capture time %d ns", rid, i, ntp, cb-ca)
		}
	}
	avg := srs[len(srs)-1].Arrival.Sub(srs[0].Arrival) / time.Duration(len(srs)-1)
	if avg < 950*time.Millisecond || avg > 1050*time.Millisecond {
		t.Errorf("%q: one SR every %v on average, want 1 s", rid, avg)
	}
	if d := time.Since(srs[len(srs)-1].Time()); d < -100*time.Millisecond || d > 3*time.Second {
		t.Errorf("%q: the last SR's NTP time is %v old, want the wall clock", rid, d)
	}
}

// TestLoopbackKeyframeOnPLI: a PLI from the viewer gets a keyframe within one frame, on each layer.
func TestLoopbackKeyframeOnPLI(t *testing.T) {
	layers := []fake.VideoLayer{
		{RID: "f", Width: 1280, Height: 720, FPS: 30, Bitrate: 2_000_000},
		{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 300_000},
	}
	src, err := fake.New(fake.Config{Layers: layers, GOP: time.Minute, Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopback(t, src, publish.Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, rid := range []string{"f", "q"} {
		r := track(t, v, webrtc.RTPCodecTypeVideo, rid)
		if err := r.Wait(ctx, func(r *Recorder) bool { return len(Frames(r.Packets())) >= 5 }); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			sent := time.Now()
			if err := v.RequestKeyframe(r); err != nil {
				t.Fatal(err)
			}
			var after []Frame
			err := r.Wait(ctx, func(r *Recorder) bool {
				after = after[:0]
				for _, f := range Frames(r.Packets()) {
					if f.First.After(sent) {
						after = append(after, f)
					}
				}
				for _, f := range after {
					if f.Mark.Keyframe {
						return true
					}
				}
				return false
			})
			if err != nil {
				t.Fatalf("%q PLI %d: %v", rid, i, err)
			}
			for n, f := range after {
				if !f.Mark.Keyframe {
					continue
				}
				t.Logf("%q PLI %d: keyframe %v after the PLI, %d frames before it", rid, i, f.First.Sub(sent), n)
				if n > 1 {
					t.Errorf("%q PLI %d: %d frames before the keyframe, want at most 1", rid, i, n)
				}
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if err := CheckStartsOnSPS(r.Packets()); err != nil {
			t.Error(err)
		}
	}
	for _, ts := range pub.Stats().Tracks {
		if ts.PLIs != 3 || ts.Keyframes != 4 {
			t.Errorf("publisher %q: %d PLIs, %d keyframes; want 3 and 4", ts.RID, ts.PLIs, ts.Keyframes)
		}
	}
}

// TestLoopbackSingleLayer: one layer goes out without a rid and arrives as a plain track; packets of layers the
// publisher doesn't send are dropped and counted.
func TestLoopbackSingleLayer(t *testing.T) {
	src, err := fake.New(fake.Config{Layers: []fake.VideoLayer{
		{RID: "f", Width: 320, Height: 180, FPS: 30, Bitrate: 200_000},
		{RID: "q", Width: 160, Height: 90, FPS: 15, Bitrate: 100_000},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pub, v := loopback(t, src, publish.Options{Layers: []string{"q"}, Profile: "6400"})
	r := track(t, v, webrtc.RTPCodecTypeVideo, "")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := r.Wait(ctx, func(r *Recorder) bool { return len(Frames(r.Packets())) >= 10 }); err != nil {
		t.Fatal(err)
	}
	pkts := r.Packets()
	for _, check := range []func([]Packet) error{CheckContinuous, CheckStartsOnSPS, CheckVideoMarkers} {
		if err := check(pkts); err != nil {
			t.Error(err)
		}
	}
	if f := Frames(pkts)[0]; f.Mark.RID != "q" {
		t.Errorf("frame of layer %q", f.Mark.RID)
	}
	if pub.Stats().Dropped == 0 {
		t.Error("layer f was not dropped")
	}
	if ti := pub.Tracks(); len(ti) != 1 || ti[0].RID != "" || ti[0].Layer != "q" || ti[0].SSRC != r.SSRC() {
		t.Errorf("publisher tracks %+v, viewer SSRC %d", ti, r.SSRC())
	}
}
