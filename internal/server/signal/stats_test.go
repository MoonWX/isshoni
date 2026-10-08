package signal_test

import (
	"bytes"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Stats (01 §8.11, §18) against the fake MediaPlane: stats.watch and the server's stats every 2 s, the MediaPeer's
// stats in the live snapshot, and the client metrics from the counters of the clients' reports.

// statsCalls returns how often the MediaPeer's Stats was read.
func statsCalls(p *signaltest.Peer) int { return len(p.CallsTo("Stats")) }

// stats.watch (01 §8.11): while on, the connection gets the stats of its MediaPeer every 2 s; {on: false}, leaving
// the room and closing stop them. A connection without media in its room that doesn't watch costs no Stats call.
func TestStatsWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		watch := func(on bool) {
			t.Helper()
			id := request(t, c, protocol.MessageTypeStatsWatch, protocol.StatsWatch{On: on})
			if env := expectType(t, c, protocol.MessageTypeOK); env.Re != id || env.Data != nil {
				t.Fatalf("stats.watch reply re %q %s", env.Re, env.Data)
			}
		}
		// expectStats reads the next message, a stats message, and returns the frame and the payload.
		expectStats := func() ([]byte, protocol.ServerStats) {
			t.Helper()
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeStats || f.Envelope.Re != "" {
				t.Fatalf("got %s (%v), want stats", f.Envelope.Type, err)
			}
			st, err := protocol.Decode[protocol.ServerStats](f.Envelope)
			if err != nil {
				t.Fatalf("decode stats: %v", err)
			}
			return f.Raw, st
		}

		// Watching outside a room: ok, and nothing to send.
		watch(true)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		expectOpen(t, c)
		// The stats start with the room: the first 2 s after the join.
		join(t, c, "lounge")
		joined := time.Now()
		peer := e.media.Peer(w.ConnectionID)
		raw, st := expectStats()
		if d := time.Since(joined); d != 2*time.Second {
			t.Errorf("the first stats came after %v, want 2 s", d)
		}
		if !bytes.Contains(raw, []byte(`"subs":[]`)) || !bytes.Contains(raw, []byte(`"layers":[]`)) || st.DownlinkEstimate != 0 {
			t.Errorf("stats %s, want empty lists, never null", raw)
		}
		want := protocol.ServerStats{
			DownlinkEstimate: 24_000_000,
			Subs: []protocol.ServerSubStats{{ShareID: "s_q7m2x9c4v8b1n5k3", Kind: protocol.TrackKindVideo, Layer: high,
				Bitrate: 7_600_000, LossPct: 0.2}},
			Layers: []protocol.ServerLayerStats{{ShareID: "s_z1x2c3v4b5n6m7k8", Kind: protocol.TrackKindVideo,
				RID: protocol.RIDHigh, Bitrate: 6_000_000}},
		}
		peer.SetStats(want)
		for i := range 3 {
			if _, st := expectStats(); jsonOf(t, st) != jsonOf(t, want) {
				t.Errorf("stats %s, want the MediaPeer's %s", jsonOf(t, st), jsonOf(t, want))
			}
			if d := time.Since(joined); d != time.Duration(i+2)*2*time.Second {
				t.Errorf("stats %d came after %v, want every 2 s", i+2, d)
			}
		}
		if n := statsCalls(peer); n != 4 {
			t.Errorf("%d Stats calls, want 4", n)
		}

		// Off: no more messages, and no more Stats calls for a connection without media.
		watch(false)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		expectOpen(t, c)
		if n := statsCalls(peer); n != 4 {
			t.Errorf("%d Stats calls after stats.watch{on: false}, want still 4", n)
		}
		// On again; the stats follow the connection to its next room, 2 s after it joined, and stop when it leaves.
		watch(true)
		watch(true) // twice is once
		expectStats()
		time.Sleep(1500 * time.Millisecond)
		join(t, c, room1.ID)
		joined = time.Now()
		peer2 := e.media.Peer(w.ConnectionID)
		before := statsCalls(peer)
		expectStats()
		if d := time.Since(joined); d != 2*time.Second {
			t.Errorf("the first stats in the next room came after %v, want 2 s", d)
		}
		if statsCalls(peer) != before || statsCalls(peer2) != 1 {
			t.Errorf("Stats calls: %d at the old MediaPeer (want %d), %d at the new one (want 1)", statsCalls(peer), before,
				statsCalls(peer2))
		}
		leave(t, c)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		expectOpen(t, c)
		if n := statsCalls(peer2); n != 1 {
			t.Errorf("%d Stats calls after room.leave, want 1", n)
		}
	})
}

// The live snapshot (01 §15.2): LiveShare.Layers are the published layers of the share from its publishing
// connection's MediaPeer.Stats, and EgressBitrate is the sum over the share's subscribers. The hub reads the stats
// of the connections that have media in the room, watched or not, and only theirs.
func TestSnapshotMedia(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		cookieC, _ := e.user(false)
		cookieD, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		cc, wc := e.connect(cookieC, signaltest.DefaultHello())
		cd, wd := e.connect(cookieD, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{ca, cb, cc, cd} {
			join(t, c, "lounge")
		}
		s1 := startShare(t, ca, screen("r1")).ShareID
		time.Sleep(time.Second)
		s2 := startShare(t, ca, screen("r2")).ShareID
		goLive(e, wa.ConnectionID, s1)
		goLive(e, wa.ConnectionID, s2)
		subscribe(t, cb, want(s1, high, audioOn), want(s2, low, audioOff))
		subscribe(t, cc, want(s1, low, audioOff))
		peerA, peerB, peerC, peerD := e.media.Peer(wa.ConnectionID), e.media.Peer(wb.ConnectionID),
			e.media.Peer(wc.ConnectionID), e.media.Peer(wd.ConnectionID)

		video, audio := protocol.TrackKindVideo, protocol.TrackKindAudio
		layers := []protocol.ServerLayerStats{
			{ShareID: s1, Kind: video, RID: protocol.RIDHigh, Bitrate: 6_000_000, LossPct: 0.5},
			{ShareID: s1, Kind: video, RID: protocol.RIDLow, Bitrate: 300_000},
			{ShareID: s2, Kind: video, RID: protocol.RIDHigh, Bitrate: 2_000_000},
			{ShareID: s1, Kind: audio, Bitrate: 128_000},
		}
		peerA.SetStats(protocol.ServerStats{Layers: layers})
		peerB.SetStats(protocol.ServerStats{Subs: []protocol.ServerSubStats{
			{ShareID: s1, Kind: video, Layer: high, Bitrate: 5_900_000},
			{ShareID: s1, Kind: audio, Bitrate: 120_000},
			{ShareID: s2, Kind: video, Layer: low, Bitrate: 290_000},
		}})
		peerC.SetStats(protocol.ServerStats{
			Subs: []protocol.ServerSubStats{{ShareID: s1, Kind: video, Layer: low, Bitrate: 280_000}},
			// A layer that C claims for A's share is not A's: only the publishing connection's layers count.
			Layers: []protocol.ServerLayerStats{{ShareID: s1, Kind: video, RID: protocol.RIDHigh, Bitrate: 1}},
		})
		peerD.SetStats(protocol.ServerStats{Subs: []protocol.ServerSubStats{{ShareID: s1, Kind: video, Layer: high, Bitrate: 1}}})
		time.Sleep(3 * time.Second)
		synctest.Wait()

		shares := e.hub.Snapshot().Rooms[0].Shares
		if len(shares) != 2 || shares[0].Info.ID != s1 || shares[1].Info.ID != s2 {
			t.Fatalf("snapshot shares %+v", shares)
		}
		if !slices.Equal(shares[0].Layers, []protocol.ServerLayerStats{layers[0], layers[1], layers[3]}) ||
			!slices.Equal(shares[1].Layers, []protocol.ServerLayerStats{layers[2]}) {
			t.Errorf("layers %+v and %+v, want each share's own from its publisher", shares[0].Layers, shares[1].Layers)
		}
		if shares[0].EgressBitrate != 5_900_000+120_000+280_000 || shares[1].EgressBitrate != 290_000 {
			t.Errorf("egress %d and %d, want the sums over the subscribers", shares[0].EgressBitrate, shares[1].EgressBitrate)
		}
		// D has no media in the room and doesn't watch: its MediaPeer is never asked.
		if n := statsCalls(peerD); n != 0 {
			t.Errorf("%d Stats calls for a connection without media, want none", n)
		}
		if statsCalls(peerA) == 0 || statsCalls(peerB) == 0 || statsCalls(peerC) == 0 {
			t.Errorf("Stats calls %d, %d, %d; want some for the publisher and both subscribers", statsCalls(peerA),
				statsCalls(peerB), statsCalls(peerC))
		}
		// Nobody watches: no stats messages.
		for _, c := range []*signaltest.Client{ca, cb, cc, cd} {
			for c.Pending() > 0 {
				if env := recv(t, c); env.Type == protocol.MessageTypeStats {
					t.Fatalf("a stats message without stats.watch: %s", env.Data)
				}
			}
		}

		// The kept stats are the hub's own: the fake's later values replace them 2 s later, not before.
		peerB.SetStats(protocol.ServerStats{})
		synctest.Wait()
		if got := e.hub.Snapshot().Rooms[0].Shares[0].EgressBitrate; got != 5_900_000+120_000+280_000 {
			t.Errorf("egress %d right after the MediaPeer's stats changed, want the kept ones", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := e.hub.Snapshot().Rooms[0].Shares[0].EgressBitrate; got != 280_000 {
			t.Errorf("egress %d, want C's alone", got)
		}
		// A share that ended is gone from the snapshot, and a connection whose media is gone is no longer asked.
		stopShare(t, ca, s1)
		stopShare(t, ca, s2)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if shares := e.hub.Snapshot().Rooms[0].Shares; len(shares) != 0 {
			t.Errorf("snapshot shares %+v, want none", shares)
		}
		na, nb := statsCalls(peerA), statsCalls(peerB)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if statsCalls(peerA) != na || statsCalls(peerB) != nb {
			t.Errorf("Stats calls went on after the shares ended: %d → %d, %d → %d", na, statsCalls(peerA), nb, statsCalls(peerB))
		}
		// The stats that the hub kept went with the media: a share that takes the old id (the hook can; ids are
		// random) starts with nothing.
		signal.AddShare(e.hub, "lounge", liveShare(s1, a.UserID, wa.ConnectionID))
		if got := e.hub.Snapshot().Rooms[0].Shares; len(got) != 1 || len(got[0].Layers) != 0 || got[0].EgressBitrate != 0 {
			t.Errorf("snapshot shares %+v, want no layers and no egress kept from the ended share", got)
		}

		// A subscribe.update that the MediaPeer refuses leaves the stated wants in the room, and the SFU keeps what
		// it applied before the failing item (01 §8.9). So the hub reads that connection's stats as well, also when
		// the refused request was the connection's first subscribe.update.
		tooMany := protocol.NewError(protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
		peerD.Fail("Subscribe", &tooMany)
		refused(t, cd, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
			Subs: []protocol.SubscriptionWant{want(s1, high, audioOn)}}, protocol.ErrorCodeBadRequest)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := statsCalls(peerD); n != 1 {
			t.Errorf("%d Stats calls for a connection whose first subscribe.update was refused, want 1 within 2 s", n)
		}
		if got := e.hub.Snapshot().Rooms[0].Shares[0].EgressBitrate; got != 1 {
			t.Errorf("egress %d, want what D's MediaPeer reports for the share", got)
		}
	})
}

// The client metrics (01 §8.11, §18): the hub adds the growth of each inbound track's cumulative counters between
// two reports of a connection. A track's first report counts in full; a counter that went down started again;
// packetsLost may go down without that.
func TestClientStatsMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		const s1, s2 = "s_q7m2x9c4v8b1n5k3", "s_z1x2c3v4b5n6m7k8"
		video, audio := protocol.TrackKindVideo, protocol.TrackKindAudio
		report := func(in ...protocol.InboundStats) {
			t.Helper()
			time.Sleep(5 * time.Second) // at most one report per 5 s is processed
			notify(t, c, protocol.MessageTypeStats, protocol.ClientStats{IntervalMs: 10000, Inbound: in})
			ping(t, c)
		}
		// check compares the six client metrics.
		check := func(what string, decoded, dropped, freeze, lostVideo, lostAudio, samples, concealed float64) {
			t.Helper()
			got := []float64{
				e.metric("isshoni_client_frames_decoded_total"), e.metric("isshoni_client_frames_dropped_total"),
				e.metric("isshoni_client_freeze_seconds_total"),
				max(0, e.metric("isshoni_client_packets_lost_total", "kind", "video")),
				max(0, e.metric("isshoni_client_packets_lost_total", "kind", "audio")),
				e.metric("isshoni_client_audio_samples_total"), e.metric("isshoni_client_audio_concealed_samples_total"),
			}
			if want := []float64{decoded, dropped, freeze, lostVideo, lostAudio, samples, concealed}; !slices.Equal(got, want) {
				t.Errorf("%s: client metrics %v, want %v (frames decoded, dropped, freeze s, lost video, lost audio, samples, concealed)",
					what, got, want)
			}
		}

		// Outside a room a report is kept for the snapshot but counts nothing.
		report(protocol.InboundStats{ShareID: s1, Kind: video, FramesDecoded: 999})
		check("outside a room", 0, 0, 0, 0, 0, 0, 0)
		if got := signal.LastStats(e.hub, w.ConnectionID); got == nil || len(got.Inbound) != 1 {
			t.Errorf("kept report %+v", got)
		}
		join(t, c, "lounge")

		v1 := protocol.InboundStats{ShareID: s1, Kind: video, FramesDecoded: 600, FramesDropped: 3, FreezeDurationMs: 1500, PacketsLost: 12}
		a1 := protocol.InboundStats{ShareID: s1, Kind: audio, TotalSamples: 480_000, ConcealedSamples: 960, PacketsLost: 2}
		report(v1, a1)
		check("first report", 600, 3, 1.5, 12, 2, 480_000, 960)

		// The counters are cumulative: only their growth counts. A second share's tracks start from zero.
		v1.FramesDecoded, v1.FramesDropped, v1.FreezeDurationMs, v1.PacketsLost = 1200, 3, 2000, 10 // loss went down
		a1.TotalSamples, a1.ConcealedSamples, a1.PacketsLost = 960_000, 1000, 5
		v2 := protocol.InboundStats{ShareID: s2, Kind: video, FramesDecoded: 50, PacketsLost: -1}
		report(v1, a1, v2, v2) // a track listed twice counts once
		check("second report", 1200+50, 3, 2, 12, 5, 960_000, 1000)

		// The sub PC was rebuilt: the counters start again, and count from zero. packetsLost follows without adding.
		v1.FramesDecoded, v1.FramesDropped, v1.FreezeDurationMs, v1.PacketsLost = 30, 1, 100, 13
		report(v1, a1)
		check("after a rebuild", 1250+30, 3+1, 2+0.1, 12+3, 5, 960_000, 1000)
		// A report within 5 s of the last one is dropped: nothing counts twice.
		v1.FramesDecoded = 90
		notify(t, c, protocol.MessageTypeStats, protocol.ClientStats{IntervalMs: 10000, Inbound: []protocol.InboundStats{v1}})
		ping(t, c)
		check("a report too soon", 1280, 4, 2.1, 15, 5, 960_000, 1000)

		// Leaving the room closes the PeerConnections: in the next room every track is new, and counts in full even
		// when its counters are above the old track's.
		leave(t, c)
		join(t, c, "lounge")
		report(protocol.InboundStats{ShareID: s1, Kind: video, FramesDecoded: 50, PacketsLost: 20})
		check("after rejoining", 1280+50, 4, 2.1, 15+20, 5, 960_000, 1000)
	})
}
