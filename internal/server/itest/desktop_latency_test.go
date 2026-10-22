package itest

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Case D16 of docs/m2/01-client-core.md §14: the Go pipeline's latency proxy of §12.4 item 1, which runs on every PR
// (ubuntu, macOS and Windows; under -race except on Windows). It catches a pump, pacer or SFU change that holds
// frames back; the glass-to-glass number itself is the owner's-PC run of §12.2.

// The load and the bounds of D16 (§12.4 item 1).
const (
	latencyWindow  = 20 * time.Second
	latencyP95     = 60 * time.Millisecond
	latencyMax     = 200 * time.Millisecond
	latencyFPS     = 60
	latencyBitrate = 8_000_000 // the Auto preset's full layer at its ceiling: the end of the ramp (§9.3)
	// latencyMinRate is the least payload bitrate of the full layer that the window must carry for the result to be
	// at "8 Mbit/s": the fake's frame sizes average the target over whole GOPs, and the window holds five of them.
	latencyMinRate = 7_000_000
)

// TestDesktopLatency is D16: fake.Engine (Synthetic, 1080p60, the default 4 s GOP of the full layer) → the client
// core → the in-process server → an sfutest.Viewer, all on loopback. Once the full layer runs at 8 Mbit/s, every
// frame of it captured during the next 20 s is timed from its capture instant (the session's Origin plus its
// Marker's CaptureNS) to the arrival of its last packet at the viewer: p95 < 60 ms and max < 200 ms. Every frame of
// the window must arrive whole, so a frame the pipeline drops can't make the numbers better. The values are logged
// with the core's own capture-to-send figures, pass or fail.
func TestDesktopLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("the 20 s latency window runs outside -short")
	}
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), desktopOptions{engine: fake.EngineConfig{
		SourceSize: [2]int{1920, 1080}, Seed: 43, // Synthetic, the default probe: a live-bitrate encoder
	}})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)

	sh := app.start(defaultRoom(bob))
	snap := app.waitState(sh, core.StateLive)
	liveAt, start := time.Now(), layerOf(snap, "f").TargetBps // the ramp's first step is RampDelay after live
	sess := app.engine.Session()
	if sess == nil {
		t.Fatal("no engine session while the share is live")
	}
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, true)) // the full layer, with the audio, as a viewer's stage
	video := track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", latencyFPS)
	bob.waitStatus(want(snap.ID, true))
	f := layerOf(sh.Snapshot(), "f")
	if f.Width != 1920 || f.Height != 1080 || !f.Active {
		t.Fatalf("the full layer is %+v, want 1920×1080 and active", f)
	}
	ramped := fullAt8Mbps(t, sh, sess, liveAt, start)
	f = layerOf(sh.Snapshot(), "f")

	// The window. The session's clock is the clock of the Markers' CaptureNS; its Origin moves only when the fake's
	// lag cap skips, which needs the pump 200 ms behind (fake.Source.Next). The capture instants are taken against
	// the Origin of the window's start, so a skip can only make the latencies larger, and fails the test on its own.
	origin, from := sess.Origin(), sess.Now()
	time.Sleep(latencyWindow)
	until := sess.Now()
	after := sh.Snapshot()
	// Every frame captured before until has arrived once a later one has (loopback, and the viewer asks for what it
	// misses); waiting for that one bounds the wait without a timeout of the test's own.
	eventually(t, func() bool {
		return slices.ContainsFunc(sfutest.Frames(sfutest.WithoutPartialTail(video.Packets())), func(fr sfutest.Frame) bool {
			return fr.HasMark && fr.Mark.RID == "f" && fr.Mark.CaptureNS >= until
		})
	}, func() string { return "a frame of the full layer captured after the window" })
	if moved := sess.Origin().Sub(origin); moved != 0 {
		t.Errorf("the engine's clock skipped %v during the window: the pump fell more than 200 ms behind", moved)
	}

	var (
		lat            []time.Duration
		keyLat         []time.Duration
		indices        []uint32
		bytes, partial int
	)
	for _, fr := range sfutest.Frames(video.Packets()) {
		if !fr.HasMark || fr.Mark.RID != "f" || fr.Mark.CaptureNS < from || fr.Mark.CaptureNS >= until {
			continue
		}
		indices = append(indices, fr.Mark.Frame)
		bytes += fr.Bytes
		if fr.Marks != 1 || fr.MarkerBits != 1 || !fr.MarkerOnLast || int(fr.LastSeq-fr.FirstSeq)+1 != fr.Packets {
			partial++
			continue
		}
		d := fr.Last.Sub(origin.Add(time.Duration(fr.Mark.CaptureNS)))
		lat = append(lat, d)
		if fr.Mark.Keyframe {
			keyLat = append(keyLat, d)
		}
	}
	span := time.Duration(until - from)
	rate := float64(bytes) * 8 / span.Seconds()
	slices.Sort(indices)
	all, keys := summarizeLatency(lat), summarizeLatency(keyLat)
	fa := layerOf(after, "f")
	t.Logf("D16 (%s/%s, race %s): %d frames of the full layer over %v at %.2f Mbit/s (the core's target %d bit/s, "+
		"ramped %t): "+
		"last-packet latency p50 %v, p95 %v, max %v, min %v; %d keyframes, max %v; "+
		"the core's capture-to-send p50 %v, p95 %v, max %v; sent %d bit/s at %.1f fps",
		runtime.GOOS, runtime.GOARCH, raceBuild(), all.n, span.Round(time.Millisecond), rate/1e6, f.TargetBps,
		ramped, all.p50, all.p95, all.max, all.min, keys.n, keys.max,
		fa.CaptureToSend.P50, fa.CaptureToSend.P95, fa.CaptureToSend.Max, fa.SentBps, fa.FPS)

	// What was measured is what D16 says: 60 frames a second at 8 Mbit/s, every one of them whole.
	wantFrames := int(span * latencyFPS / time.Second)
	if n := len(indices); n < wantFrames-2 || n > wantFrames+2 {
		t.Errorf("%d frames of the full layer in %v, want %d ± 2 (60 fps)", n, span, wantFrames)
	}
	if n := len(indices); n > 0 {
		if missing := int(indices[n-1]-indices[0]) + 1 - n; missing != 0 {
			t.Errorf("%d frames of the full layer captured in the window never reached the viewer", missing)
		}
	}
	if partial != 0 {
		t.Errorf("%d frames of the full layer arrived incomplete", partial)
	}
	if rate < latencyMinRate {
		t.Errorf("the full layer carried %.2f Mbit/s in the window, want at least %.2f: not the 8 Mbit/s load",
			rate/1e6, float64(latencyMinRate)/1e6)
	}
	if all.n == 0 {
		t.Fatal("no whole frame of the full layer in the window")
	}
	// A clock check: Next hands out a frame no earlier than its due time, on the clock the viewer stamps arrivals
	// with, so a latency below zero means the test reads the wrong Origin.
	if all.min < 0 {
		t.Errorf("a latency of %v: the capture instants are not on the viewer's clock", all.min)
	}
	if all.p95 >= latencyP95 {
		t.Errorf("last-packet latency p95 %v, want under %v", all.p95, latencyP95)
	}
	if all.max >= latencyMax {
		t.Errorf("last-packet latency max %v, want under %v", all.max, latencyMax)
	}
	bob.check()
}

// fullAt8Mbps brings the share's full layer to D16's 8 Mbit/s and says whether the core's ramp did it. The ramp
// takes the layer there from ratectl's start of 4 Mbit/s in 12 s (§9.3); its Ticks are README WA46's, and a core
// without them keeps the layer at its first target (start, its target at live). When the target hasn't moved by the
// time of the ramp's second step plus 2 s, the test sets the engine's full layer to 8 Mbit/s itself, through the
// session control the ramp uses, so the load is D16's either way. The core's pacer then still paces for the first
// targets (3 × 4.3 Mbit/s rather than 3 × 8.3), which drains a frame more slowly than a ramped share's would: that
// makes the bounds harder to meet, not easier.
func fullAt8Mbps(t *testing.T, sh *core.Share, sess *fake.Session, liveAt time.Time, start int) (ramped bool) {
	t.Helper()
	for probe := liveAt.Add(ratectl.RampDelay + 2*ratectl.RampEvery + 2*time.Second); time.Now().Before(probe); {
		if layerOf(sh.Snapshot(), "f").TargetBps != start {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f := layerOf(sh.Snapshot(), "f"); f.TargetBps == start && start < latencyBitrate {
		t.Logf("the core's full layer stays at its first target, %d bit/s (no ramp): the test sets the engine's to "+
			"%d bit/s", start, latencyBitrate)
		if err := sess.SetBitrate("f", latencyBitrate); err != nil {
			t.Fatalf("SetBitrate(f, %d): %v", latencyBitrate, err)
		}
		return false
	}
	var f core.LayerSnapshot
	eventually(t, func() bool {
		f = layerOf(sh.Snapshot(), "f")
		return f.TargetBps >= latencyBitrate
	}, func() string {
		return fmt.Sprintf("the full layer is %+v, want it at %d bit/s: the ramp did not end", f, latencyBitrate)
	})
	return true
}

// latencySummary is the distribution of a set of latencies; the percentiles are nearest-rank.
type latencySummary struct {
	n                  int
	min, p50, p95, max time.Duration
}

// summarizeLatency summarizes latencies (in any order); a summary of none is all zero.
func summarizeLatency(lat []time.Duration) latencySummary {
	if len(lat) == 0 {
		return latencySummary{}
	}
	s := slices.Sorted(slices.Values(lat))
	rank := func(p int) time.Duration { return s[max((len(s)*p+99)/100, 1)-1] } // ⌈n·p/100⌉, counted from 1
	return latencySummary{n: len(s), min: s[0], p50: rank(50), p95: rank(95), max: s[len(s)-1]}
}

// raceBuild says whether the test binary was built with the race detector ("true" or "false"), for the log.
func raceBuild() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value
		}
	}
	return "false"
}
