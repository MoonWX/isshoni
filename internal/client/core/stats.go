package core

import (
	"log/slog"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Stats (docs/m2/01-client-core.md §11.9, M1 01 §8.11), three flows:
//
//   - The engine's view: while a share is live, its actor calls Session.Stats every timing.engineStats (1 s) and
//     copies each layer's encoder, HW flag and limit into its LayerSnapshot (EventShare when one changes), next to
//     the live counters FPSIn and EngineLatency (capture → engine ring, P50 and P95; Max stays 0), which fire no
//     event (§11.10).
//   - The stats report to the server: every timing.statsReport (10 s) while the share's pub PC exists and signaling
//     is ready, a protocol.ClientStats with the pub PC (gen, its RTCPeerConnectionState, the RTT from the SFU's
//     receiver reports, the selected pair's local candidate type and transport) and an outbound entry per video
//     layer and for the audio (bitrate and frame rate from the Publisher; size, encoder and HW flag from the
//     snapshot; the quality limitation). The report is checked with ClientStats.Validate, the hub's own check,
//     before it goes. It carries encoder names and numbers only: no source or app name, no address (§11.12).
//   - The server's view (WatchServerStats, the app's debug overlay): stats.watch{on} on the Go connection, sent
//     again on a new connection (a resumed one keeps it, M1 01 §8.11), and the server's stats of the share's
//     layers (every 2 s while the connection is in the share's room) kept in ShareSnapshot.Server, a live counter.

// statsState is a share's side of the stats. The actor's alone.
type statsState struct {
	engineAt time.Time // the next Session.Stats poll; zero: at the first look of a live share
	reportAt time.Time // the next stats report; zero until the share has a pub PC
	lastAt   time.Time // when the last report went out; zero before the first
}

// statsTick is the actor's look at the stats (tick, every timing.pcPoll): the engine's stats while the share is
// live, a stats report when one is due, and the connection's stats.watch.
func (s *Share) statsTick(now time.Time) {
	t := s.c.timing
	if s.live && !now.Before(s.st.engineAt) {
		s.st.engineAt = now.Add(t.engineStats)
		s.pollEngine()
	}
	if s.rec.online {
		s.c.syncWatch()
	}
	pub := s.publisher()
	if pub == nil || s.rec.republish {
		return // no pub PC (yet, or while a rebuild or a republish replaces it): nothing to report
	}
	if s.st.reportAt.IsZero() {
		s.st.reportAt = now.Add(t.statsReport) // the first report comes one interval after the pub PC
		return
	}
	if now.Before(s.st.reportAt) || !s.rec.online {
		return // a report due while signaling is down goes once it is ready
	}
	interval := t.statsReport
	if !s.st.lastAt.IsZero() {
		interval = now.Sub(s.st.lastAt)
	}
	s.st.reportAt, s.st.lastAt = now.Add(t.statsReport), now
	s.report(pub, interval)
}

// pollEngine copies the engine's stats into the snapshot (§11.9): the encoder, its HW flag and the limit of each
// layer (and f's encoder as the share's), and the live counters FPSIn and EngineLatency, which fire no EventShare.
// An engine that leaves a field empty keeps what EventLayer and EventLimit said.
func (s *Share) pollEngine() {
	st, err := s.sess.Stats()
	if err != nil {
		s.log.Debug("engine stats", logx.Err(err))
		return
	}
	s.mu.Lock()
	for _, l := range st.Layers {
		if ls := s.snap.layer(l.RID); ls != nil {
			ls.FPSIn = l.FPSIn
			ls.EngineLatency = publish.LatencyStats{P50: l.LatencyP50, P95: l.LatencyP95}
		}
	}
	s.mu.Unlock()
	s.update(func(snap *ShareSnapshot) {
		for _, l := range st.Layers {
			ls := snap.layer(l.RID)
			if ls == nil {
				continue
			}
			if l.Encoder != "" {
				ls.Encoder, ls.HW = l.Encoder, l.HW
				if l.RID == protocol.RIDHigh {
					snap.Encoder, snap.HW = l.Encoder, l.HW
				}
			}
			if l.Limit != "" {
				ls.Limit = l.Limit
			}
		}
	})
}

// report sends one stats report for the share's pub PC, which covers interval. A report that the hub would refuse
// (ClientStats.Validate) is logged and not sent: it would only earn an error.
func (s *Share) report(pub *publish.Publisher, interval time.Duration) {
	cs := s.clientStats(pub, interval)
	if err := cs.Validate(); err != nil {
		s.log.Warn("stats report not sent", logx.Err(err))
		return
	}
	if err := s.c.send(protocol.MessageTypeStats, cs); err != nil {
		s.log.Debug("stats report", logx.Err(err))
		return
	}
	pc := cs.PCs[0]
	s.log.Debug("stats report sent", slog.Uint64("gen", uint64(pc.Gen)), slog.String("state", pc.State),
		slog.Int("rtt_ms", pc.RTTMs), slog.String("candidate_type", pc.CandidateType),
		slog.String("transport", pc.Transport), slog.Int("outbound", len(cs.Outbound)))
}

// clientStats builds the stats report of the share's pub PC (§11.9).
func (s *Share) clientStats(pub *publish.Publisher, interval time.Duration) protocol.ClientStats {
	pcs := pub.PC()
	ps := protocol.PCStats{PC: protocol.PCKindPub, Gen: s.drv.gen, State: pcs.ConnectionState().String()}
	if cand, transport, ok := selectedCandidate(pcs); ok {
		ps.CandidateType, ps.Transport = cand, transport
	}
	tracks := pub.Stats().Tracks
	ps.RTTMs = rttMs(tracks)

	s.mu.Lock()
	snap := s.snap.clone()
	s.mu.Unlock()
	shareID := s.params.ShareID
	var out []protocol.OutboundStats
	for _, ts := range tracks {
		if ts.Kind == webrtc.RTPCodecTypeAudio {
			out = append(out, protocol.OutboundStats{ShareID: shareID, Kind: protocol.TrackKindAudio,
				Bitrate: int64(max(0, ts.Bitrate))})
			continue
		}
		o := protocol.OutboundStats{ShareID: shareID, Kind: protocol.TrackKindVideo, RID: ts.Layer,
			Bitrate: int64(max(0, ts.Bitrate)), FPS: max(0, ts.FPS),
			QualityLimitation: qualityLimitation(snap.Upload.Limited, "")}
		if l := snap.layer(ts.Layer); l != nil {
			o.Width, o.Height, o.Encoder, o.HWEncoder = max(0, l.Width), max(0, l.Height), l.Encoder, l.HW
			o.QualityLimitation = qualityLimitation(snap.Upload.Limited, l.Limit)
		}
		out = append(out, o)
	}
	return protocol.ClientStats{
		IntervalMs: int(max(0, interval.Round(time.Millisecond)/time.Millisecond)),
		PCs:        []protocol.PCStats{ps},
		Outbound:   out,
	}
}

// The values of OutboundStats.QualityLimitation (M1 01 §8.11: none|cpu|bandwidth|other).
const (
	qualityNone      = "none"
	qualityCPU       = "cpu"
	qualityBandwidth = "bandwidth"
	qualityOther     = "other"
)

// qualityLimitation is a video layer's qualityLimitation (§11.9): bandwidth while the upload hint holds (ratectl's
// UploadHint.Limited), cpu while the layer's limit is cpu, other while it is gpu (the protocol has no gpu), else none.
func qualityLimitation(uploadLimited bool, limit string) string {
	switch {
	case uploadLimited:
		return qualityBandwidth
	case limit == "cpu":
		return qualityCPU
	case limit == "gpu":
		return qualityOther
	}
	return qualityNone
}

// rttMs is the pub PC's RTT in whole milliseconds, rounded up so that a known one is never 0: the first track's
// that has one (the video layers come first), 0 while no receiver report has given one.
func rttMs(tracks []publish.TrackStats) int {
	for _, ts := range tracks {
		if ts.RTT > 0 {
			return int((ts.RTT + time.Millisecond - 1) / time.Millisecond)
		}
	}
	return 0
}

// selectedCandidate returns the local side of the PC's selected candidate pair: its type (host, srflx, prflx,
// relay) and transport (udp, tcp). ok is false while no pair is selected. Never the address.
func selectedCandidate(pc *webrtc.PeerConnection) (typ, transport string, ok bool) {
	for _, tr := range pc.GetTransceivers() {
		s := tr.Sender()
		if s == nil {
			continue
		}
		dtls := s.Transport()
		if dtls == nil || dtls.ICETransport() == nil {
			continue
		}
		pair, err := dtls.ICETransport().GetSelectedCandidatePair()
		if err != nil || pair == nil || pair.Local == nil {
			return "", "", false
		}
		return pair.Local.Typ.String(), pair.Local.Protocol.String(), true
	}
	return "", "", false
}

// ---- the server's stats (stats.watch) ----

// statsWatch is the Go connection's stats.watch: what WatchServerStats wants, and what the server has. A new
// connection starts without a watch; a resumed one keeps it (M1 01 §8.11). Client.mu guards it.
type statsWatch struct {
	want    bool      // WatchServerStats's last call
	on      bool      // the watch the server has for connection conn, as far as the core knows
	conn    string    // the connection on is about; "" before the first stats.watch went through
	syncing bool      // a goroutine is sending stats.watch (syncWatch)
	retryAt time.Time // after a failed stats.watch, the shares' looks try again from then on
}

// watchRetry: a stats.watch that failed while signaling stayed ready (a refusal) goes again at most this often.
const watchRetry = 2 * time.Second

// WatchServerStats turns stats.watch on or off (the app's debug overlay); the server's view of this connection's
// layers then arrives in ShareSnapshot.Server, a live counter (every 2 s while a share is in its room, M1 01 §8.11).
// It returns at once: the request goes out in the background while signaling is ready, and again on a new
// connection, which starts without the watch. Off clears ShareSnapshot.Server at once.
func (c *Client) WatchServerStats(on bool) {
	c.mu.Lock()
	c.watch.want, c.watch.retryAt = on, time.Time{}
	sh := c.share
	c.mu.Unlock()
	if !on && sh != nil {
		sh.setServerStats(nil)
	}
	c.syncWatch()
}

// syncWatch sends stats.watch until the server's watch on the current connection is the wanted one, unless that
// runs already or signaling isn't ready: WatchServerStats calls it, and a share's actor at each look, so a new
// connection gets the watch again once ready. One request at a time, each with the newest wish, so they reach the
// server in order. A request that fails ends the run; the next call tries again, at the earliest watchRetry later
// (a new wish at once). The run is one of the Client's operations: Close and the end of Run wait for it, and their
// end cuts its request short.
func (c *Client) syncWatch() {
	c.mu.Lock()
	start := !c.watch.syncing && c.watchDueLocked()
	if start {
		c.watch.syncing = true
	}
	c.mu.Unlock()
	if !start {
		return
	}
	if !c.startOp() {
		c.mu.Lock()
		c.watch.syncing = false
		c.mu.Unlock()
		return
	}
	go func() {
		defer c.ops.Done()
		for {
			c.mu.Lock()
			if !c.watchDueLocked() {
				c.watch.syncing = false
				c.mu.Unlock()
				return
			}
			want, conn := c.watch.want, c.connID
			c.mu.Unlock()
			err := c.request(c.life, protocol.MessageTypeStatsWatch, protocol.StatsWatch{On: want}, nil)
			c.mu.Lock()
			if err != nil {
				c.watch.syncing = false
				if c.watch.want == want {
					c.watch.retryAt = time.Now().Add(watchRetry)
				}
				c.mu.Unlock()
				c.log.Debug("stats.watch", slog.Bool("on", want), logx.Err(err))
				return
			}
			c.watch.on, c.watch.conn = want, conn
			c.mu.Unlock()
		}
	}()
}

// watchDueLocked reports whether signaling is ready, no failed stats.watch holds the next one back, and the
// server's watch on the connection differs from the wanted one. c.mu is held.
func (c *Client) watchDueLocked() bool {
	if !c.sigReady || c.sig == nil || c.life.Err() != nil || time.Now().Before(c.watch.retryAt) {
		return false
	}
	has := c.watch.on && c.watch.conn == c.connID
	return has != c.watch.want
}

// onServerStats takes the server's stats notification (stats.watch): the layers of the active share's current id
// go to its snapshot while the watch is wanted. Another share's layers (an id before a republish) are dropped. A
// WatchServerStats(false) that ran meanwhile has cleared the snapshot before they were set, so they are cleared
// again.
func (c *Client) onServerStats(st protocol.ServerStats) {
	want := func() (bool, *Share) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.watch.want, c.share
	}
	on, sh := want()
	if !on || sh == nil {
		return
	}
	sh.setServerStats(st.Layers)
	if on, _ := want(); !on {
		sh.setServerStats(nil)
	}
}

// setServerStats sets ShareSnapshot.Server to the layers of the share's current id: a live counter, so no
// EventShare (§11.10). nil clears it.
func (s *Share) setServerStats(layers []protocol.ServerLayerStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap.State == StateEnded {
		return
	}
	var mine []protocol.ServerLayerStats
	for _, l := range layers {
		if l.ShareID == s.snap.ID {
			mine = append(mine, l)
		}
	}
	s.snap.Server = mine
}
