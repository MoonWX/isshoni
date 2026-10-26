package core

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// stats.go against fakeServer (docs/m2/01-client-core.md §11.9): the stats reports (valid, every interval, what they
// carry), the quality limitation, the engine's stats in the snapshot without EventShare for the live counters
// (§14's core row), and WatchServerStats (stats.watch on each new connection, ShareSnapshot.Server). The reports'
// numbers on a connected pub PC, and the hub's metrics, are the itests' (internal/server/itest,
// desktop_relay_test.go).

// statsTiming is fastTiming with the stats' intervals shortened too.
func statsTiming() func(*Options) {
	return fastTiming(func(t *timing) {
		t.engineStats = 50 * time.Millisecond
		t.statsReport = 300 * time.Millisecond
	})
}

// statsReports decodes the stats reports the server received, in order, as the hub does: one it would refuse
// (ClientStats.Validate) fails the test.
func statsReports(t *testing.T, fs *fakeServer) []protocol.ClientStats {
	t.Helper()
	fs.mu.Lock()
	var envs []protocol.Envelope
	for _, env := range fs.got {
		if env.Type == protocol.MessageTypeStats {
			envs = append(envs, env)
		}
	}
	fs.mu.Unlock()
	var out []protocol.ClientStats
	for _, env := range envs {
		cs, err := protocol.Decode[protocol.ClientStats](env)
		if err != nil {
			t.Fatalf("a stats report the hub would refuse: %v: %s", err, env.Data)
		}
		out = append(out, cs)
	}
	return out
}

// waitReports waits until the server has received at least n stats reports and returns them.
func waitReports(t *testing.T, fs *fakeServer, n int) []protocol.ClientStats {
	t.Helper()
	var got []protocol.ClientStats
	eventually(t, func() bool {
		got = statsReports(t, fs)
		return len(got) >= n
	}, func() string { return fmt.Sprintf("the server got %d stats reports, want %d", len(got), n) })
	return got
}

// outbound returns a report's entry of a kind and rid, failing the test when it has none.
func outbound(t *testing.T, cs protocol.ClientStats, kind protocol.TrackKind, rid string) protocol.OutboundStats {
	t.Helper()
	i := slices.IndexFunc(cs.Outbound, func(o protocol.OutboundStats) bool { return o.Kind == kind && o.RID == rid })
	if i < 0 {
		t.Fatalf("the report %+v has no %s %q entry", cs, kind, rid)
	}
	return cs.Outbound[i]
}

// TestStatsReports: once the share's pub PC exists, a stats report goes every interval, valid for the hub: the pub
// PC (gen, state; no candidate while ICE hasn't connected), an outbound entry per video layer with the size and
// encoder of its EventLayer and for the audio, intervalMs the time since the report before.
func TestStatsReports(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: statsTiming()})
	startedShare(t, e)
	rs := waitReports(t, fs, 3)
	for i, cs := range rs {
		if len(cs.PCs) != 1 {
			t.Fatalf("report %d: pcs %+v, want the pub PC alone", i, cs.PCs)
		}
		pc := cs.PCs[0]
		if pc.PC != protocol.PCKindPub || pc.Gen != 1 || (pc.State != "new" && pc.State != "connecting") ||
			pc.CandidateType != "" || pc.Transport != "" || pc.RTTMs != 0 {
			t.Errorf("report %d: pc %+v, want the pub PC of gen 1, not connected (the fake server answers no offer)",
				i, pc)
		}
		if len(cs.Inbound) != 0 || len(cs.Outbound) != 3 {
			t.Errorf("report %d: inbound %+v, outbound %+v, want f, q and the audio out", i, cs.Inbound, cs.Outbound)
		}
		for _, rid := range []string{"f", "q"} {
			o := outbound(t, cs, protocol.TrackKindVideo, rid)
			if o.ShareID != "s_1" || o.Width != 640 || o.Height != 360 || o.Encoder != "fake" || o.HWEncoder ||
				o.QualityLimitation != qualityNone || o.Bitrate != 0 {
				t.Errorf("report %d: %s %+v, want s_1's 640×360 on the fake encoder, no limitation, nothing sent", i,
					rid, o)
			}
		}
		if a := outbound(t, cs, protocol.TrackKindAudio, ""); a.ShareID != "s_1" || a.Encoder != "" ||
			a.QualityLimitation != "" {
			t.Errorf("report %d: audio %+v, want s_1's audio with its bitrate only", i, a)
		}
	}
	if rs[0].IntervalMs != 300 {
		t.Errorf("the first report's intervalMs = %d, want the interval, 300", rs[0].IntervalMs)
	}
	for i, cs := range rs[1:] {
		if cs.IntervalMs < 300 {
			t.Errorf("report %d: intervalMs %d, want the time since the report before (at least 300)", i+1,
				cs.IntervalMs)
		}
	}
}

// TestStatsNotWhileOffline: a report that falls due while signaling is down waits for the welcome; none goes before.
func TestStatsNotWhileOffline(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: statsTiming()})
	startedShare(t, e)
	waitReports(t, fs, 1)
	refuseHellos(fs)
	fs.kill()
	eventually(t, func() bool { return e.c.Status().Conn != ConnOnline },
		func() string { return fmt.Sprintf("status %+v, want it offline", e.c.Status()) })
	n := len(statsReports(t, fs))
	time.Sleep(700 * time.Millisecond) // two intervals
	if got := len(statsReports(t, fs)); got != n {
		t.Fatalf("%d stats reports went out while signaling was down", got-n)
	}
	fs.setHello(nil) // resumes
	waitReports(t, fs, n+1)
}

// TestQualityLimitation: §11.9's mapping, and a layer's EventLimit in the next report (the share isn't live, so the
// engine's stats poll doesn't run).
func TestQualityLimitation(t *testing.T) {
	for _, tc := range []struct {
		upload bool
		limit  string
		want   string
	}{
		{false, "", qualityNone}, {false, "none", qualityNone}, {false, "source", qualityNone},
		{false, "cpu", qualityCPU}, {false, "gpu", qualityOther},
		{true, "none", qualityBandwidth}, {true, "cpu", qualityBandwidth}, {true, "gpu", qualityBandwidth},
	} {
		if got := qualityLimitation(tc.upload, tc.limit); got != tc.want {
			t.Errorf("qualityLimitation(%t, %q) = %q, want %q", tc.upload, tc.limit, got, tc.want)
		}
	}

	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: statsTiming()})
	startedShare(t, e)
	sess := sessionOf(t, e)
	for _, limit := range []struct{ engine, report string }{{"cpu", qualityCPU}, {"gpu", qualityOther},
		{"none", qualityNone}} {
		sess.Emit(media.Event{Code: media.EventLimit, Layer: "f", Limit: limit.engine})
		eventually(t, func() bool { return fullOf(e.c.ActiveShare()).Limit == limit.engine },
			func() string { return fmt.Sprintf("the snapshot's f limit, want %s", limit.engine) })
		n := len(statsReports(t, fs))
		rs := waitReports(t, fs, n+1)
		last := rs[len(rs)-1]
		f, q := outbound(t, last, protocol.TrackKindVideo, "f"), outbound(t, last, protocol.TrackKindVideo, "q")
		if f.QualityLimitation != limit.report || q.QualityLimitation != qualityNone {
			t.Errorf("limit %s: f %q, q %q, want %q and none", limit.engine, f.QualityLimitation, q.QualityLimitation,
				limit.report)
		}
	}
}

// withoutLiveCounters is a snapshot with its live counters zeroed: what EventShare compares (§11.10).
func withoutLiveCounters(s ShareSnapshot) ShareSnapshot {
	c := *s.clone()
	c.AudioSentBps, c.Server = 0, nil
	for i := range c.Layers {
		l := &c.Layers[i]
		l.SentBps, l.FPS, l.FPSIn = 0, 0, 0
		l.CaptureToSend, l.EngineLatency = publish.LatencyStats{}, publish.LatencyStats{}
	}
	return c
}

// TestEngineStatsInSnapshot: while the share is live, the engine's stats reach the snapshot: the layers' limit and
// encoder, and FPSIn, a live counter that fires no EventShare: every EventShare differs from the one before in
// something else (the ramp's targets change meanwhile).
func TestEngineStatsInSnapshot(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: statsTiming()})
	events, unsub := e.c.Subscribe()
	defer unsub()
	sh := startedShare(t, e)
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	e.waitState(sh, StateLive)
	eventually(t, func() bool {
		f := fullOf(sh)
		return f.FPSIn > 0 && f.Limit == "none" && f.Encoder == "fake"
	}, func() string {
		return fmt.Sprintf("the full layer %+v, want the engine's FPSIn and limit", *fullOf(sh))
	})
	time.Sleep(time.Second) // 20 polls, each with a new FPSIn
	var shares []ShareSnapshot
	for drained := false; !drained; {
		select {
		case ev := <-events:
			if ev.Kind == EventShare {
				shares = append(shares, *ev.Share)
			}
		default:
			drained = true
		}
	}
	for i := 1; i < len(shares); i++ {
		if reflect.DeepEqual(withoutLiveCounters(shares[i-1]), withoutLiveCounters(shares[i])) {
			t.Errorf("EventShare %d differs from the one before only in live counters: %+v", i, shares[i])
		}
	}
}

// waitState waits until the share is in state.
func (e *testEnv) waitState(sh *Share, state ShareState) {
	e.t.Helper()
	eventually(e.t, func() bool { return sh.Snapshot().State == state },
		func() string { return fmt.Sprintf("the share is %+v, want %s", sh.Snapshot(), state) })
}

// watches renders the stats.watch requests the server received: "on off …".
func watches(fs *fakeServer) string {
	var out []string
	for _, w := range received[protocol.StatsWatch](fs, protocol.MessageTypeStatsWatch) {
		if w.On {
			out = append(out, "on")
		} else {
			out = append(out, "off")
		}
	}
	return fmt.Sprint(out)
}

// waitWatches waits until the server's stats.watch requests render as want ("on off").
func waitWatches(t *testing.T, fs *fakeServer, want string) {
	t.Helper()
	want = "[" + want + "]"
	eventually(t, func() bool { return watches(fs) == want },
		func() string {
			return fmt.Sprintf("the server got the stats.watch requests %s, want %s", watches(fs), want)
		})
}

// TestWatchServerStats: a wish made before Run goes once online; the server's stats of the share's current id reach
// ShareSnapshot.Server, without EventShare, and those of another id don't; off clears it at once and later stats are
// dropped; a resumed connection keeps the watch, a new one is asked again.
func TestWatchServerStats(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: statsTiming()})
	e.c.WatchServerStats(true)
	events, unsub := e.c.Subscribe()
	defer unsub()
	sh := startedShare(t, e)
	waitWatches(t, fs, "on")

	mine := protocol.ServerLayerStats{ShareID: "s_1", Kind: protocol.TrackKindVideo, RID: "f", Bitrate: 2_000_000,
		LossPct: 0.5}
	settle()
	for drained := false; !drained; {
		select {
		case <-events:
		default:
			drained = true
		}
	}
	fs.send(protocol.MessageTypeStats, protocol.ServerStats{Layers: []protocol.ServerLayerStats{mine,
		{ShareID: "s_9", Kind: protocol.TrackKindAudio, Bitrate: 64_000}}})
	eventually(t, func() bool { return len(sh.Snapshot().Server) > 0 },
		func() string { return "the server's stats are not in the snapshot" })
	if got := sh.Snapshot().Server; !slices.Equal(got, []protocol.ServerLayerStats{mine}) {
		t.Errorf("ShareSnapshot.Server = %+v, want the share's layer alone: %+v", got, mine)
	}
	settle()
	select {
	case ev := <-events:
		t.Errorf("the server's stats fired %s: %+v", ev.Kind, ev)
	default:
	}

	e.c.WatchServerStats(false)
	if got := sh.Snapshot().Server; got != nil {
		t.Errorf("ShareSnapshot.Server after WatchServerStats(false) = %+v, want none", got)
	}
	waitWatches(t, fs, "on off")
	fs.send(protocol.MessageTypeStats, protocol.ServerStats{Layers: []protocol.ServerLayerStats{mine}})
	settle()
	if got := sh.Snapshot().Server; got != nil {
		t.Errorf("ShareSnapshot.Server after stats that came when off = %+v, want none", got)
	}

	e.c.WatchServerStats(true)
	waitWatches(t, fs, "on off on")
	fs.kill() // resumed: the server keeps the connection's watch
	eventually(t, func() bool { return fs.helloCount() == 2 && e.c.Status().Conn == ConnOnline },
		func() string { return "no resume" })
	settle()
	if got := watches(fs); got != "[on off on]" {
		t.Errorf("after a resume the server got the stats.watch requests %s, want no new one", got)
	}
	fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{} }) // a new connection
	fs.kill()
	waitWatches(t, fs, "on off on on")
}
