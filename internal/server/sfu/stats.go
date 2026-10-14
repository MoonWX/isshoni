package sfu

import (
	"cmp"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// This file declares what the SFU reports about itself (02 §6.2, §13) and builds the reports: ShareInfo for the hub,
// ConnStats for 01's stats message, Snapshot for the admin dashboard and Metrics for 04's Prometheus adapter.
//
// None of them asks a Conn's actor, so none of them waits: they read what the media path and the actors keep in
// atomics, and take each lock of doc.go's order alone and briefly.
//   - Layers measure their ingress (size, frame rate, bitrate, loss) and DownTracks their egress (bitrate, the loss
//     their viewer reports, retransmissions, drops). The ticker turns their counts into 2 s moving averages once a
//     second (SFU.everySecond).
//   - A Conn's PeerConnections are its views (pcView): the PC's state and selected candidate pair come from Pion,
//     which keeps both in atomics, labeled by 02 §7.1 (apis.via); the round-trip time is what the PC's RTCP readers
//     last read from the selected pair's statistics (samplePairRTT), every 5 s.
//   - A subscription's request, and the reason its client last heard, are what its Conn's actor last published
//     (Subscription.shown); what its DownTracks forward is theirs.
//   - The counters of Metrics are the SFU's totals since New (counters.go), which keep what Layers and DownTracks
//     that are gone have counted; its gauges are read from the object model when Metrics is called.

// ShareInfo describes a share (02 §6.2). Ended shares are never listed.
type ShareInfo struct {
	ID          ShareID
	Room        RoomID
	Participant ParticipantID
	User        UserID
	Conn        ConnID
	Source      SourceKind
	Preset      Preset
	State       ShareState
	Profile     ProfileKey  // current H.264 profile of the video ("" until known)
	Layers      []LayerInfo // sorted f, h, q; every layer whose track is attached, paused ones included (02 §10.1)
	Audio       bool        // an audio track is attached
	// Viewers are the participants a DownTrack forwards this share to now, sorted (debug and tests). room.state
	// watchers come from the hub (01 §4.1) and the dashboard's counts from ShareSnapshot.Viewers.
	Viewers   []ParticipantID
	StartedAt time.Time
	LiveAt    time.Time // zero until the first keyframe
}

// LayerInfo describes one attached video layer of a share.
type LayerInfo struct {
	RID           string  // "f" | "q"; a video m-section without rids is one "f" layer
	Width, Height int     // from the last SPS
	FPS           float64 // measured
	Bitrate       int     // bps, 2 s EWMA
	LossPct       float64 // ingress loss over the last 2 s
	Active        bool    // packets in the last 2 s
}

// ConnStats is one Conn's view of its media (02 §13). Signal sends it as 01's stats message every 2 s while the
// client asks for it.
type ConnStats struct {
	PCs              []PCStats           // pub first, then sub; only the PCs that exist
	Shares           []OwnShareStats     // the shares this Conn publishes, by StartedAt
	Subscriptions    []SubscriptionStats // by share id
	DownlinkEstimate int64               // bit/s for the sub PC, from the downlink allocator (02 §10.2); 0 while unknown
}

// PCStats describes one PeerConnection of a Conn.
type PCStats struct {
	Kind      PCKind
	Gen       uint32
	State     string        // a webrtc.PeerConnectionState string
	Transport string        // "udp" | "tcp443" | "tcp7882" | "" while not connected (02 §7.1)
	RTT       time.Duration // of the selected candidate pair
}

// OwnShareStats describes the ingress of one share the Conn publishes.
type OwnShareStats struct {
	Share        ShareID
	State        ShareState
	Layers       []LayerInfo // the video layers, as in ShareInfo
	Audio        bool        // an audio track is attached
	AudioBitrate int         // bps, 2 s EWMA
	AudioLossPct float64
}

// SubscriptionStats describes one subscription of the Conn.
type SubscriptionStats struct {
	Share          ShareID
	Requested      Quality
	Forwarded      Quality
	Reason         SubReason
	Layer          string     // rid of the video layer forwarded now; "" when none
	Profile        ProfileKey // H.264 profile of the forwarded video; "" when none
	AudioRequested bool
	AudioForwarded bool
	Video          DownTrackStats
	Audio          DownTrackStats
}

// DownTrackStats are the egress numbers of one DownTrack.
type DownTrackStats struct {
	Bitrate     int     // bps, 2 s EWMA
	LossPct     float64 // from the viewer's receiver reports
	NACKsServed uint64
	Drops       uint64 // packets dropped by the DownTrack's queues
}

// Snapshot is the SFU's state for the admin dashboard (02 §13). Probe PCs are never counted.
type Snapshot struct {
	At     time.Time
	Rooms  []RoomSnapshot // by room id
	Totals SnapshotTotals
}

// RoomSnapshot is one room of a Snapshot.
type RoomSnapshot struct {
	Room   RoomID
	Shares []ShareSnapshot // by StartedAt
	Conns  []ConnSummary   // by ConnID
}

// ShareSnapshot is one share of a Snapshot.
type ShareSnapshot struct {
	Info                  ShareInfo
	IngressBps, EgressBps int64 // RTP level, 2 s EWMA
	Viewers               ViewerCounts
}

// ViewerCounts counts the DownTracks forwarding a share now, by forwarded quality; Audio counts the audio DownTracks
// forwarding.
type ViewerCounts struct {
	High, Low, Audio int
}

// ConnSummary is one Conn of a Snapshot: the sub PC's selected transport and RTT, falling back to the pub PC.
type ConnSummary struct {
	Conn      ConnID
	User      UserID
	Transport string        // "udp" | "tcp443" | "tcp7882" | "" (02 §7.1)
	RTT       time.Duration // selected candidate pair
}

// SnapshotTotals are the server-wide numbers of a Snapshot.
type SnapshotTotals struct {
	IngressBps, EgressBps int64          // RTP level; EgressBps is the dashboard's media.egressBps (02 §15.2)
	DownTracks            int            // every DownTrack, forwarding or not
	PCsByState            map[string]int // pub+sub PCs by webrtc.PeerConnectionState string
	PCsByTransport        map[string]int // connected pub+sub PCs by the label of 02 §7.1
}

// Metrics is a copy of the SFU's counters and gauges (02 §13). 04 maps them to Prometheus as isshoni_sfu_*; the map
// keys are the label values of the table in 02 §13, and every listed value is present, zero or not. Probe PCs are
// never counted.
type Metrics struct {
	Conns             map[string]int64            // isshoni_sfu_conns{role}
	PeerConnections   map[string]map[string]int64 // isshoni_sfu_peerconnections{kind}{state}
	SelectedTransport map[string]int64            // isshoni_sfu_selected_transport{transport}
	Shares            map[string]int64            // isshoni_sfu_shares{state}
	DownTracks        map[string]map[string]int64 // isshoni_sfu_downtracks{kind}{forwarding}
	IngressBytes      map[string]uint64           // isshoni_sfu_ingress_bytes_total{kind}
	IngressPackets    map[string]uint64           // isshoni_sfu_ingress_packets_total{kind}
	IngressDuplicates uint64                      // isshoni_sfu_ingress_duplicates_total
	EgressBytes       map[string]uint64           // isshoni_sfu_egress_bytes_total{kind}
	EgressPackets     map[string]uint64           // isshoni_sfu_egress_packets_total{kind}
	NACKReceived      uint64
	NACKServed        uint64
	NACKMissed        uint64
	RTXSkipped        uint64
	PLISent           uint64
	PLIThrottled      uint64
	KeyframesReceived uint64
	LayerSwitches     uint64
	Downgrades        map[string]uint64 // isshoni_sfu_downgrades_total{reason}
	QueueDrops        map[string]uint64 // isshoni_sfu_queue_drops_total{kind}
	HandshakeTimeouts map[string]uint64 // isshoni_sfu_handshake_timeouts_total{kind}
	PacketCacheBytes  int64
}

// Label values of 02 §13 that have no type of their own.
const (
	kindVideo = "video"
	kindAudio = "audio"
	kindRTX   = "rtx"
)

// pcStates are the label values of isshoni_sfu_peerconnections{state}, and the keys of SnapshotTotals.PCsByState.
var pcStates = [...]webrtc.PeerConnectionState{
	webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting, webrtc.PeerConnectionStateConnected,
	webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed,
}

// transports are the labels of 02 §7.1: the label values of isshoni_sfu_selected_transport, and the keys of
// SnapshotTotals.PCsByTransport.
var transports = [...]string{string(ProbeUDP), string(ProbeTCP443), string(ProbeTCP7882)}

// newMetrics returns a Metrics with every label value of 02 §13 present and zero.
func newMetrics() Metrics {
	m := Metrics{
		Conns:             map[string]int64{},
		PeerConnections:   map[string]map[string]int64{},
		SelectedTransport: map[string]int64{},
		Shares:            map[string]int64{},
		DownTracks:        map[string]map[string]int64{},
		IngressBytes:      map[string]uint64{},
		IngressPackets:    map[string]uint64{},
		EgressBytes:       map[string]uint64{},
		EgressPackets:     map[string]uint64{},
		Downgrades:        map[string]uint64{},
		QueueDrops:        map[string]uint64{},
		HandshakeTimeouts: map[string]uint64{},
	}
	for _, r := range []Role{RoleFull, RoleViewer, RolePublisher, RoleAgent} {
		m.Conns[r.String()] = 0
	}
	for _, k := range []PCKind{PCPub, PCSub} {
		states := map[string]int64{}
		for _, st := range pcStates {
			states[st.String()] = 0
		}
		m.PeerConnections[k.String()] = states
		m.HandshakeTimeouts[k.String()] = 0
	}
	for _, t := range transports {
		m.SelectedTransport[t] = 0
	}
	for _, st := range []ShareState{SharePending, ShareLive, ShareStalled} {
		m.Shares[st.String()] = 0
	}
	for _, kind := range []string{kindVideo, kindAudio} {
		m.DownTracks[kind] = map[string]int64{
			QualityHigh.String(): 0, QualityLow.String(): 0, QualityOff.String(): 0,
		}
		m.IngressBytes[kind], m.IngressPackets[kind] = 0, 0
	}
	for _, kind := range kindLabels {
		m.EgressBytes[kind], m.EgressPackets[kind], m.QueueDrops[kind] = 0, 0, 0
	}
	for _, r := range downgradeReasons {
		m.Downgrades[string(r)] = 0
	}
	return m
}

// ---- what the reports read of PeerConnections, shares and DownTracks ----

// pcView is what the reports read of one PeerConnection of a Conn (Conn.views), from any goroutine: its kind and gen,
// the PeerConnection, whose connection state and selected candidate pair Pion keeps safe to read from anywhere, and
// its round-trip time as the PC's RTCP readers last read it (subPC.rtt, pubPC.rtt). The fields never change.
type pcView struct {
	kind PCKind
	gen  uint32
	pc   *webrtc.PeerConnection
	rtt  *atomic.Int64
}

// stats returns the PC's PCStats now. The transport and the round-trip time are the selected pair's, and only while
// the PC is connected: before, there is no pair, and after, what was measured is no longer so.
func (v *pcView) stats(a *apis) PCStats {
	state := v.pc.ConnectionState()
	st := PCStats{Kind: v.kind, Gen: v.gen, State: state.String()}
	if state == webrtc.PeerConnectionStateConnected {
		st.Transport = a.selectedVia(v.pc)
		st.RTT = time.Duration(v.rtt.Load())
	}
	return st
}

// selectedVia returns the label of 02 §7.1 of pc's selected candidate pair, from its local candidate (apis.via): ""
// while it has none. Pion keeps the pair in an atomic, so this neither waits for the ICE agent nor asks a closed one.
func (a *apis) selectedVia(pc *webrtc.PeerConnection) string {
	sctp := pc.SCTP()
	if sctp == nil {
		return ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil {
		return ""
	}
	return a.via(*pair.Local)
}

// pcStats returns the Conn's PeerConnections as Stats reports them: pub first, then sub, only those that exist.
func (c *Conn) pcStats() []PCStats {
	var out []PCStats
	for _, kind := range [...]PCKind{PCPub, PCSub} {
		if v := c.views[kind].Load(); v != nil {
			out = append(out, v.stats(c.sfu.apis))
		}
	}
	return out
}

// summary returns the Conn's ConnSummary for Snapshot (02 §13): the sub PC's selected transport and round-trip time,
// and for each of them the pub PC's when the sub PC has none (it isn't connected, or hasn't been measured yet).
func (c *Conn) summary(pcs []PCStats) ConnSummary {
	s := ConnSummary{Conn: c.id, User: c.user}
	for _, kind := range [...]PCKind{PCSub, PCPub} {
		for _, p := range pcs {
			if p.Kind != kind {
				continue
			}
			if s.Transport == "" {
				s.Transport = p.Transport
			}
			if s.RTT == 0 {
				s.RTT = p.RTT
			}
		}
	}
	return s
}

// sortedShares returns the room's shares that haven't ended, sorted by StartedAt (then by id).
func (r *Room) sortedShares() []*Share {
	r.mu.Lock()
	shares := make([]*Share, 0, len(r.shares))
	for _, sh := range r.shares {
		shares = append(shares, sh)
	}
	r.mu.Unlock()
	sortShares(shares)
	return shares
}

// sortedConns returns the room's Conns, sorted by id.
func (r *Room) sortedConns() []*Conn {
	r.mu.Lock()
	conns := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	slices.SortFunc(conns, func(a, b *Conn) int { return cmp.Compare(a.id, b.id) })
	return conns
}

// ingressBps returns what the share's publisher sends now, video and audio, in bit/s of RTP payload: the sum of its
// attached layers' bitrates (2 s moving averages, Layer.tick).
func (s *Share) ingressBps() int64 {
	var bps int64
	for _, l := range s.attached() {
		bps += l.stats.bitrate.Load()
	}
	return bps
}

// allDownTracks returns the share's video DownTracks, then its audio ones (kindIdxVideo, kindIdxAudio); the caller
// must not change the lists.
func (s *Share) allDownTracks() [kindIdxRTX][]*DownTrack {
	return [kindIdxRTX][]*DownTrack{s.downTracks(webrtc.RTPCodecTypeVideo), s.downTracks(webrtc.RTPCodecTypeAudio)}
}

// tickEgress turns the byte counts of the share's DownTracks into their bitrates; the ticker calls it once a second,
// after the layers' tick (SFU.everySecond).
func (s *Share) tickEgress(now int64) {
	for _, list := range s.allDownTracks() {
		for _, dt := range list {
			dt.tick(now)
		}
	}
}

// tick turns what the DownTrack has sent, media and retransmissions, into its bitrate: a 2 s moving average, like a
// Layer's (Layer.tick). Only the ticker calls it. The first tick only takes the baseline; the first measured second
// is the average so far.
func (d *DownTrack) tick(now int64) {
	st := &d.stats
	bytes := st.bytes.Load() + st.rtxBytes.Load()
	prevAt, prevBytes := st.tickAt, st.tickBytes
	st.tickAt, st.tickBytes = now, bytes
	if st.ticks == 0 {
		st.ticks = 1
		return
	}
	dt := time.Duration(now - prevAt)
	if dt <= 0 {
		return
	}
	weight := 1 - math.Exp(-float64(dt)/float64(rateWindow))
	if st.ticks == 1 {
		st.ticks, weight = 2, 1
	}
	old := float64(st.bitrate.Load())
	sample := float64(bytes-prevBytes) * 8 / dt.Seconds()
	st.bitrate.Store(int64(math.Round(old + weight*(sample-old))))
}

// forwardingNow returns the slot of the layer the DownTrack forwards now and the profile of its stream ("" for
// audio); ok is false while it forwards nothing.
func (d *DownTrack) forwardingNow() (slot Slot, profile ProfileKey, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if slot, ok = d.m.current(); ok {
		profile = d.m.newest().profile
	}
	return slot, profile, ok
}

// egress returns the DownTrack's DownTrackStats: its bitrate, the loss its viewer reports, the retransmissions it
// sent and what both of its queues dropped.
func (d *DownTrack) egress() DownTrackStats {
	st := &d.stats
	return DownTrackStats{
		Bitrate:     int(st.bitrate.Load()),
		LossPct:     math.Float64frombits(st.lossPct.Load()),
		NACKsServed: st.nackServed.Load(),
		Drops:       st.drops.Load() + st.rtxDrops.Load(),
	}
}

// stats returns the subscription's SubscriptionStats: the request and the reason as the client last heard them
// (shown), and what its DownTracks forward and count now.
func (s *Subscription) stats() SubscriptionStats {
	st := SubscriptionStats{Share: s.share.id, Video: s.video.egress(), Audio: s.audio.egress()}
	if shown := s.shown.Load(); shown != nil {
		st.Requested, st.AudioRequested, st.Reason = shown.requested, shown.audio, shown.reason
	}
	if slot, profile, ok := s.video.forwardingNow(); ok {
		st.Forwarded, st.Layer, st.Profile = slot.quality(), slot.String(), profile
	}
	_, _, st.AudioForwarded = s.audio.forwardingNow()
	return st
}

// ---- the three reports ----

// Snapshot returns the SFU's state for the admin dashboard (02 §13): the rooms by id, each with its shares by
// StartedAt (ShareInfo with the layers' ingress, the share's ingress and egress rates and its viewers by forwarded
// quality) and its Conns by id (the selected transport and round-trip time); and the server's totals. Rates are 2 s
// moving averages of RTP payload, up to a second old; a share's egress is what its DownTracks send, retransmissions
// included. Probe PCs are counted nowhere. Every state and every transport label is in the totals' maps, zero or not.
func (s *SFU) Snapshot() Snapshot {
	snap := Snapshot{
		At:     time.Now(),
		Totals: SnapshotTotals{PCsByState: map[string]int{}, PCsByTransport: map[string]int{}},
	}
	for _, st := range pcStates {
		snap.Totals.PCsByState[st.String()] = 0
	}
	for _, t := range transports {
		snap.Totals.PCsByTransport[t] = 0
	}
	s.mu.Lock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.mu.Unlock()
	slices.SortFunc(rooms, func(a, b *Room) int { return cmp.Compare(a.id, b.id) })
	for _, r := range rooms {
		rs := RoomSnapshot{Room: r.id}
		for _, sh := range r.sortedShares() {
			if ss, downTracks, ok := sh.snapshot(); ok {
				snap.Totals.IngressBps += ss.IngressBps
				snap.Totals.EgressBps += ss.EgressBps
				snap.Totals.DownTracks += downTracks
				rs.Shares = append(rs.Shares, ss)
			}
		}
		for _, c := range r.sortedConns() {
			pcs := c.pcStats()
			for _, p := range pcs {
				if _, ok := snap.Totals.PCsByState[p.State]; ok {
					snap.Totals.PCsByState[p.State]++
				}
				if _, ok := snap.Totals.PCsByTransport[p.Transport]; ok {
					snap.Totals.PCsByTransport[p.Transport]++
				}
			}
			rs.Conns = append(rs.Conns, c.summary(pcs))
		}
		snap.Rooms = append(snap.Rooms, rs)
	}
	return snap
}

// snapshot returns the share's ShareSnapshot and how many DownTracks it has, forwarding or not; ok is false once it
// has ended.
func (s *Share) snapshot() (ss ShareSnapshot, downTracks int, ok bool) {
	info, ok := s.info()
	if !ok {
		return ShareSnapshot{}, 0, false
	}
	ss = ShareSnapshot{Info: info, IngressBps: s.ingressBps()}
	for kind, list := range s.allDownTracks() {
		downTracks += len(list)
		for _, dt := range list {
			ss.EgressBps += dt.stats.bitrate.Load()
			slot, _, ok := dt.forwardingNow()
			switch {
			case !ok:
			case kind == kindIdxAudio:
				ss.Viewers.Audio++
			case slot.quality() == QualityHigh:
				ss.Viewers.High++
			default:
				ss.Viewers.Low++
			}
		}
	}
	return ss, downTracks, true
}

// Metrics returns the counters and gauges of 02 §13 with every label value present. The counters are the SFU's
// totals since New (counters.go); the gauges are read from the Conns, their PeerConnections, the shares, their layers
// and DownTracks now. Probe PCs are never counted.
func (s *SFU) Metrics() Metrics {
	m := newMetrics()
	t := &s.counters
	for i, kind := range kindLabels {
		if i < len(t.ingressBytes) {
			m.IngressBytes[kind], m.IngressPackets[kind] = t.ingressBytes[i].Load(), t.ingressPackets[i].Load()
		}
		m.EgressBytes[kind], m.EgressPackets[kind] = t.egressBytes[i].Load(), t.egressPackets[i].Load()
		m.QueueDrops[kind] = t.queueDrops[i].Load()
	}
	m.IngressDuplicates = t.ingressDuplicates.Load()
	m.NACKReceived, m.NACKServed = t.nackReceived.Load(), t.nackServed.Load()
	m.NACKMissed, m.RTXSkipped = t.nackMissed.Load(), t.rtxSkipped.Load()
	m.PLISent, m.PLIThrottled = t.pliSent.Load(), t.pliThrottled.Load()
	m.KeyframesReceived, m.LayerSwitches = t.keyframesReceived.Load(), t.layerSwitches.Load()
	for i, r := range downgradeReasons {
		m.Downgrades[string(r)] = t.downgrades[i].Load()
	}
	for _, kind := range []PCKind{PCPub, PCSub} {
		m.HandshakeTimeouts[kind.String()] = t.handshakeTimeouts[kind].Load()
	}

	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	shares := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		shares = append(shares, sh)
	}
	s.mu.Unlock()
	for _, c := range conns {
		m.Conns[c.role.String()]++
		for _, p := range c.pcStats() {
			if states := m.PeerConnections[p.Kind.String()]; states != nil {
				if _, ok := states[p.State]; ok {
					states[p.State]++
				}
			}
			if _, ok := m.SelectedTransport[p.Transport]; ok {
				m.SelectedTransport[p.Transport]++
			}
		}
	}
	for _, sh := range shares {
		st, ok := sh.liveState()
		if !ok {
			continue
		}
		m.Shares[st.String()]++
		for _, l := range sh.attached() {
			m.PacketCacheBytes += l.cache.bytesHeld()
		}
		for kind, list := range sh.allDownTracks() {
			forwarding := m.DownTracks[kindLabels[kind]]
			for _, dt := range list {
				q := QualityOff
				if slot, _, ok := dt.forwardingNow(); ok {
					q = slot.quality() // the audio slot's is high
				}
				forwarding[q.String()]++
			}
		}
	}
	return m
}

// Stats returns the Conn's media view (02 §13): its PeerConnections, the shares it publishes with their layers and
// audio, its subscriptions with what they forward and count, by share id, and the downlink estimate of its allocator
// (02 §10.2): the budget it holds the viewer to while it is limited, else the viewer's latest REMB while that is
// fresh, else 0. It reads atomics and takes each lock briefly, never the Conn's actor, so signal's actor can ask for
// it every 2 s without waiting.
func (c *Conn) Stats() ConnStats {
	st := ConnStats{PCs: c.pcStats(), DownlinkEstimate: c.alloc.estimate.Load()}
	now := monoNow()
	for _, sh := range c.room.sortedShares() {
		info, ok := sh.info()
		if !ok {
			continue
		}
		if info.Conn == c.id {
			own := OwnShareStats{Share: info.ID, State: info.State, Layers: info.Layers, Audio: info.Audio}
			if l := sh.layer(SlotAudio); l != nil {
				li := l.info(now)
				own.AudioBitrate, own.AudioLossPct = li.Bitrate, li.LossPct
			}
			st.Shares = append(st.Shares, own)
		}
		for _, dt := range sh.downTracks(webrtc.RTPCodecTypeVideo) {
			if dt.sub.conn == c {
				st.Subscriptions = append(st.Subscriptions, dt.sub.stats())
			}
		}
	}
	slices.SortFunc(st.Subscriptions, func(a, b SubscriptionStats) int { return cmp.Compare(a.Share, b.Share) })
	return st
}
