package sfu

import (
	"cmp"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"
)

// This file declares what the SFU reports about itself (02 §6.2, §13): ShareInfo for the hub, ConnStats for 01's
// stats message, Snapshot for the admin dashboard and Metrics for 04's Prometheus adapter. ShareInfo is complete: the
// media path measures each layer (size, frame rate, bitrate, loss, activity) and knows a share's profile and its
// viewers. Snapshot, ConnStats and Metrics hold what the object model knows: rooms, Conns, and shares with their
// ShareInfo. Their rates, viewer counts, transports, RTTs and counters stay zero until the observability slice
// (README S79) sums up what the Layers and DownTracks count; the types don't change then.

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
	DownlinkEstimate int64               // bit/s for the sub PC, from the downlink allocator; 0 while unknown
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

// newMetrics returns a Metrics with every label value of 02 §13 present and zero.
func newMetrics() Metrics {
	m := Metrics{
		Conns:             map[string]int64{},
		PeerConnections:   map[string]map[string]int64{},
		SelectedTransport: map[string]int64{string(ProbeUDP): 0, string(ProbeTCP443): 0, string(ProbeTCP7882): 0},
		Shares:            map[string]int64{},
		DownTracks:        map[string]map[string]int64{},
		IngressBytes:      map[string]uint64{kindVideo: 0, kindAudio: 0},
		IngressPackets:    map[string]uint64{kindVideo: 0, kindAudio: 0},
		EgressBytes:       map[string]uint64{kindVideo: 0, kindAudio: 0, kindRTX: 0},
		EgressPackets:     map[string]uint64{kindVideo: 0, kindAudio: 0, kindRTX: 0},
		Downgrades:        map[string]uint64{},
		QueueDrops:        map[string]uint64{kindVideo: 0, kindAudio: 0, kindRTX: 0},
		HandshakeTimeouts: map[string]uint64{},
	}
	for _, r := range []Role{RoleFull, RoleViewer, RolePublisher, RoleAgent} {
		m.Conns[r.String()] = 0
	}
	for _, k := range []PCKind{PCPub, PCSub} {
		states := map[string]int64{}
		for _, st := range []webrtc.PeerConnectionState{
			webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting, webrtc.PeerConnectionStateConnected,
			webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed,
		} {
			states[st.String()] = 0
		}
		m.PeerConnections[k.String()] = states
		m.HandshakeTimeouts[k.String()] = 0
	}
	for _, st := range []ShareState{SharePending, ShareLive, ShareStalled} {
		m.Shares[st.String()] = 0
	}
	for _, kind := range []string{kindVideo, kindAudio} {
		m.DownTracks[kind] = map[string]int64{
			QualityHigh.String(): 0, QualityLow.String(): 0, QualityOff.String(): 0,
		}
	}
	for _, r := range []SubReason{
		SubReasonBandwidth, SubReasonServerLimit, SubReasonCodecMismatch, SubReasonDecoderUnavailable,
		SubReasonDecoderFailed, SubReasonNoPreviewLayer, SubReasonNoLayer,
	} {
		m.Downgrades[string(r)] = 0
	}
	return m
}

// Snapshot returns the rooms with their shares and Conns (02 §13). In this slice it holds what the object model
// knows; the rates, viewer counts, transports, RTTs and totals are README S79's.
func (s *SFU) Snapshot() Snapshot {
	snap := Snapshot{
		At:     time.Now(),
		Totals: SnapshotTotals{PCsByState: map[string]int{}, PCsByTransport: map[string]int{}},
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
		for _, info := range r.shareInfos() {
			rs.Shares = append(rs.Shares, ShareSnapshot{Info: info})
		}
		r.mu.Lock()
		for _, c := range r.conns {
			rs.Conns = append(rs.Conns, ConnSummary{Conn: c.id, User: c.user})
		}
		r.mu.Unlock()
		slices.SortFunc(rs.Conns, func(a, b ConnSummary) int { return cmp.Compare(a.Conn, b.Conn) })
		snap.Rooms = append(snap.Rooms, rs)
	}
	return snap
}

// Metrics returns the counters and gauges of 02 §13 with every label value present. Only the Conn and share gauges
// count so far; README S79 fills in the rest from what the Layers and DownTracks count.
func (s *SFU) Metrics() Metrics {
	m := newMetrics()
	s.mu.Lock()
	for _, c := range s.conns {
		m.Conns[c.role.String()]++
	}
	shares := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		shares = append(shares, sh)
	}
	s.mu.Unlock()
	for _, sh := range shares {
		if st, ok := sh.liveState(); ok {
			m.Shares[st.String()]++
		}
	}
	return m
}

// Stats returns the Conn's media view (02 §13). It lists the shares the Conn publishes, with what the SFU measures
// of their video layers; PCs, subscriptions, the audio numbers and the downlink estimate are README S79's.
func (c *Conn) Stats() ConnStats {
	var st ConnStats
	for _, info := range c.room.shareInfos() {
		if info.Conn != c.id {
			continue
		}
		st.Shares = append(st.Shares, OwnShareStats{
			Share: info.ID, State: info.State, Layers: info.Layers, Audio: info.Audio,
		})
	}
	return st
}
