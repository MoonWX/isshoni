package sfu

import (
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// This file holds the SFU's totals for Metrics (02 §13). Layers and DownTracks count what they do in their own
// counters, which feed their rates and the Conns' stats, and come and go with tracks and subscriptions. A Prometheus
// counter must never go back, so every counter of 02 §13 that they feed is also counted, at the same moment, into
// one total per SFU: a tally is a Layer's or a DownTrack's counter with the total of the same thing beside it.
// Metrics reads the totals; the gauges of 02 §13 are read from the object model when Metrics is called (stats.go).

// The kind label values of 02 §13 as indexes into the totals: the media kinds of ingress, egress and the DownTrack
// queues, and the retransmissions of egress and of the RTX queue.
const (
	kindIdxVideo = iota
	kindIdxAudio
	kindIdxRTX
	kindIdxCount
)

// kindLabels are the label values of the indexes above.
var kindLabels = [kindIdxCount]string{kindVideo, kindAudio, kindRTX}

// mediaIdx returns the index of a track kind: audio, or video for everything else.
func mediaIdx(kind webrtc.RTPCodecType) int {
	if kind == webrtc.RTPCodecTypeAudio {
		return kindIdxAudio
	}
	return kindIdxVideo
}

// downgradeReasons are the label values of isshoni_sfu_downgrades_total: the non-empty SubReasons, in the order of
// 02 §13. server_limit has no source in M1 and stays 0.
var downgradeReasons = [...]SubReason{
	SubReasonBandwidth, SubReasonServerLimit, SubReasonCodecMismatch, SubReasonDecoderUnavailable,
	SubReasonDecoderFailed, SubReasonNoPreviewLayer, SubReasonNoLayer,
}

// counters are the SFU's totals since New (02 §13): what every Layer and DownTrack counted, those that are gone
// included, and what the Conns' actors count themselves (downgrades, handshake timeouts). Everything is an atomic:
// the media path adds to them without a lock, and Metrics reads them the same way.
type counters struct {
	ingressBytes, ingressPackets [kindIdxRTX]atomic.Uint64   // by kindIdxVideo, kindIdxAudio
	ingressDuplicates            atomic.Uint64               // packets the cache already held (02 §9.1)
	egressBytes, egressPackets   [kindIdxCount]atomic.Uint64 // video and audio as forwarded; rtx every retransmission
	queueDrops                   [kindIdxCount]atomic.Uint64 // by DownTrack queue: video, audio, rtx
	nackReceived, nackServed     atomic.Uint64
	nackMissed, rtxSkipped       atomic.Uint64
	pliSent, pliThrottled        atomic.Uint64
	keyframesReceived            atomic.Uint64
	layerSwitches                atomic.Uint64
	downgrades                   [len(downgradeReasons)]atomic.Uint64
	handshakeTimeouts            [PCSub + 1]atomic.Uint64 // new PeerConnections not connected in time, by PCKind
}

// downgrade counts a subscription that was told it gets less than it asked for, for a reason (Conn.tell).
func (c *counters) downgrade(r SubReason) {
	for i, reason := range downgradeReasons {
		if reason == r {
			c.downgrades[i].Add(1)
			return
		}
	}
}

// tally is one counter of a Layer or a DownTrack that also counts into the SFU's total of the same thing: the
// object's own count feeds its rates and its Conn's stats, the total feeds Metrics and keeps what objects that are
// gone have counted. total is set before the Layer or DownTrack is used (countInto) and never changes; nil counts
// nothing more, as for the Layers and DownTracks that unit tests build without a share or a Conn.
type tally struct {
	n     atomic.Uint64
	total *atomic.Uint64
}

// Add adds d to the counter and to its total, and returns the counter's new value.
func (t *tally) Add(d uint64) uint64 {
	if t.total != nil {
		t.total.Add(d)
	}
	return t.n.Add(d)
}

// Load returns the counter's own value.
func (t *tally) Load() uint64 { return t.n.Load() }

// countInto makes the Layer's counters count into the SFU's totals as well. Share.attach calls it before the Layer
// is the share's, so before its read loop or anybody else counts a packet or a keyframe request on it.
func (l *Layer) countInto(c *counters) {
	i := mediaIdx(l.kind)
	st := &l.stats
	st.packets.total, st.bytes.total = &c.ingressPackets[i], &c.ingressBytes[i]
	st.duplicates.total = &c.ingressDuplicates
	st.keyframes.total = &c.keyframesReceived
	st.pliSent.total, st.pliThrottled.total = &c.pliSent, &c.pliThrottled
}

// countInto makes the DownTrack's counters count into the SFU's totals as well. newDownTrack calls it, before the
// DownTrack is on a fan-out list or its goroutines run.
func (d *DownTrack) countInto(c *counters) {
	i := mediaIdx(d.kind)
	st := &d.stats
	st.packets.total, st.bytes.total, st.drops.total = &c.egressPackets[i], &c.egressBytes[i], &c.queueDrops[i]
	st.rtxPackets.total, st.rtxBytes.total = &c.egressPackets[kindIdxRTX], &c.egressBytes[kindIdxRTX]
	st.rtxDrops.total = &c.queueDrops[kindIdxRTX]
	st.nackReceived.total, st.nackServed.total = &c.nackReceived, &c.nackServed
	st.nackMissed.total, st.rtxSkipped.total = &c.nackMissed, &c.rtxSkipped
	d.totals = c
}
