package signal

import (
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Stats (01 §8.11), in both directions.
//
// Client → server: the hub keeps the last stats report of each connection for the live snapshot, and turns the
// growth of its cumulative inbound counters into the isshoni_client_* metrics (01 §18), which the M1 exit test reads.
//
// Server → client: while a connection has media in its room (it publishes a share, or has stated a desired
// subscription) or watches the server's stats (stats.watch), its actor reads MediaPeer.Stats every statsInterval. The
// result is kept on the connection's room membership, where Snapshot reads the published layers and the egress of
// each share, and it goes to the client as a stats message while the client watches.

// statsInterval is how often a watching connection gets the server's stats (01 §8.11).
const statsInterval = 2 * time.Second

// maxInboundKeys bounds the inbound tracks whose counters a connection remembers between two stats reports: twice
// the entries of one report (protocol.ClientStats.Validate).
const maxInboundKeys = 4 * protocol.MaxSubs

// inboundKey names one inbound track of a client's stats report.
type inboundKey struct {
	shareID string
	kind    protocol.TrackKind
}

// inboundCounters are the cumulative counters of one inbound track, as its last report had them.
type inboundCounters struct {
	framesDecoded, framesDropped, freezeMs, packetsLost, concealedSamples, totalSamples int64
}

// counterDelta is the growth of a cumulative counter from prev to cur, never negative. A counter that went down has
// started again from zero (the track was rebuilt), so all of cur is new.
func counterDelta(prev, cur int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return max(0, cur)
}

// lossDelta is counterDelta for packetsLost, which may legitimately go down a little (RTCP counts duplicates as
// negative loss): a decrease adds nothing.
func lossDelta(prev, cur int64) int64 {
	return max(0, cur-prev)
}

// countClientStats adds the growth of the report's cumulative inbound counters since the connection's previous
// report to the isshoni_client_* metrics (01 §8.11, §18). A track's first report counts in full: its counters started
// with the track. Reports only count while the connection is in a room: outside one it has no PeerConnection.
func (c *conn) countClientStats(v *protocol.ClientStats) {
	m := c.h.metrics
	if m == nil || c.room == nil || len(v.Inbound) == 0 {
		return
	}
	if c.inbound == nil {
		c.inbound = make(map[inboundKey]inboundCounters)
	}
	seen := make(map[inboundKey]struct{}, len(v.Inbound))
	for _, in := range v.Inbound {
		k := inboundKey{shareID: in.ShareID, kind: in.Kind}
		seen[k] = struct{}{}
		prev := c.inbound[k] // of the last report, or of the track's previous entry in this one: nothing counts twice
		cur := inboundCounters{
			framesDecoded:    in.FramesDecoded,
			framesDropped:    in.FramesDropped,
			freezeMs:         in.FreezeDurationMs,
			packetsLost:      in.PacketsLost,
			concealedSamples: in.ConcealedSamples,
			totalSamples:     in.TotalSamples,
		}
		c.inbound[k] = cur
		m.clientInbound(in.Kind, inboundCounters{
			framesDecoded:    counterDelta(prev.framesDecoded, cur.framesDecoded),
			framesDropped:    counterDelta(prev.framesDropped, cur.framesDropped),
			freezeMs:         counterDelta(prev.freezeMs, cur.freezeMs),
			packetsLost:      lossDelta(prev.packetsLost, cur.packetsLost),
			concealedSamples: counterDelta(prev.concealedSamples, cur.concealedSamples),
			totalSamples:     counterDelta(prev.totalSamples, cur.totalSamples),
		})
	}
	if len(c.inbound) > maxInboundKeys {
		for k := range c.inbound {
			if _, ok := seen[k]; !ok {
				delete(c.inbound, k)
			}
		}
	}
}

// armStats starts the connection's stats timer unless it runs already. The timer stops by itself once the connection
// neither has media in its room nor watches (statsTick).
func (c *conn) armStats() {
	if c.statsOn || c.room == nil {
		return
	}
	c.statsOn = true
	c.statsTimer.Reset(statsInterval)
}

// stopStats stops the stats timer: the connection has left its room.
func (c *conn) stopStats() {
	c.statsOn = false
	c.statsTimer.Stop()
}

// statsTick reads the MediaPeer's stats, keeps them for Snapshot and, while the client watches, sends them as a
// stats message (01 §8.11). It runs every statsInterval while the connection has media in its room or watches; when
// neither holds any more, it forgets the kept stats and the timer stops. A detached connection keeps reading (its
// media may flow on); the message is dropped, like everything else while it has no socket.
func (c *conn) statsTick() {
	c.statsOn = false
	r := c.room
	if r == nil {
		return
	}
	r.mu.Lock()
	m := r.memberLocked(c)
	need := c.statsWatch
	if !need && m != nil {
		need = len(m.subs) > 0
		for _, s := range r.shares {
			need = need || s.info.ConnectionID == c.id
		}
	}
	if !need && m != nil {
		m.media = nil
	}
	r.mu.Unlock()
	if !need {
		return
	}
	st := c.peer.Stats()
	// The kept copy is the hub's own: the peer may reuse its slices, and Snapshot reads them under the room's lock.
	kept := st
	kept.Subs, kept.Layers = slices.Clone(st.Subs), slices.Clone(st.Layers)
	r.mu.Lock()
	if m := r.memberLocked(c); m != nil {
		m.media = &kept
	}
	r.mu.Unlock()
	if c.statsWatch {
		c.send(protocol.MessageTypeStats, "", st)
	}
	c.armStats()
}
