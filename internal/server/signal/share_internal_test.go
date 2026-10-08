package signal

import (
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Share ids are "s_" + 16 lowercase Crockford base32 characters from 80 random bits (01 §4.1), so they pass the
// protocol's id rules and never repeat.
func TestNewShareID(t *testing.T) {
	re := regexp.MustCompile(`^s_[0-9a-hjkmnp-tv-z]{16}$`)
	seen := make(map[string]bool)
	for range 1000 {
		id := newShareID()
		if !re.MatchString(id) {
			t.Fatalf("share id %q", id)
		}
		if seen[id] {
			t.Fatalf("share id %q twice", id)
		}
		seen[id] = true
	}
	stop := protocol.ShareStop{ShareID: newShareID()}
	if err := stop.Validate(); err != nil {
		t.Errorf("a share id fails the protocol's validation: %v", err)
	}
}

// What a media event says the SFU receives (01 §8.5): the layers and the audio follow the event, the codec stays the
// last one known when the event has none.
func TestShareApplyMedia(t *testing.T) {
	high, low := protocol.VideoLayerHigh, protocol.VideoLayerLow
	s := &share{info: protocol.ShareInfo{Audio: true}}
	for _, tc := range []struct {
		name    string
		ev      ShareMediaEvent
		changed bool
		layers  []protocol.VideoLayer
		codec   protocol.CodecKey
		audio   bool
	}{
		{"first keyframe", ShareMediaEvent{Layers: []protocol.VideoLayer{high}, Codec: protocol.CodecH264High, Audio: true},
			true, []protocol.VideoLayer{high}, protocol.CodecH264High, true},
		{"the same again", ShareMediaEvent{Layers: []protocol.VideoLayer{high}, Codec: protocol.CodecH264High, Audio: true},
			false, []protocol.VideoLayer{high}, protocol.CodecH264High, true},
		{"a second layer, no codec", ShareMediaEvent{Layers: []protocol.VideoLayer{high, low}, Audio: true},
			true, []protocol.VideoLayer{high, low}, protocol.CodecH264High, true},
		{"another profile", ShareMediaEvent{Layers: []protocol.VideoLayer{high, low}, Codec: protocol.CodecH264ConstrainedBaseline, Audio: true},
			true, []protocol.VideoLayer{high, low}, protocol.CodecH264ConstrainedBaseline, true},
		{"the audio track is gone", ShareMediaEvent{Layers: []protocol.VideoLayer{high, low}},
			true, []protocol.VideoLayer{high, low}, protocol.CodecH264ConstrainedBaseline, false},
		{"the tracks are gone", ShareMediaEvent{}, true, nil, protocol.CodecH264ConstrainedBaseline, false},
		{"still gone", ShareMediaEvent{Layers: []protocol.VideoLayer{}}, false, nil, protocol.CodecH264ConstrainedBaseline, false},
	} {
		if got := s.applyMedia(tc.ev); got != tc.changed {
			t.Errorf("%s: changed %v, want %v", tc.name, got, tc.changed)
		}
		if !slices.Equal(s.info.Layers, tc.layers) || s.info.Codec != tc.codec || s.info.Audio != tc.audio {
			t.Errorf("%s: layers %v, codec %q, audio %v; want %v, %q, %v", tc.name, s.info.Layers, s.info.Codec,
				s.info.Audio, tc.layers, tc.codec, tc.audio)
		}
	}
	// Whatever the layers are, room.state and the snapshot show a list, never null.
	if got := s.shareInfo(nil); got.Layers == nil || got.Watchers == nil {
		t.Errorf("shareInfo %+v, want empty lists", got)
	}
}

// The growth of a client's cumulative counters between two reports (01 §8.11).
func TestCounterDelta(t *testing.T) {
	for _, tc := range []struct{ prev, cur, want, loss int64 }{
		{0, 0, 0, 0},
		{0, 600, 600, 600}, // a track's first report counts in full
		{600, 1200, 600, 600},
		{1200, 1200, 0, 0},
		{1200, 30, 30, 0}, // the counter started again; packetsLost just went down
		{10, -2, 0, 0},    // packetsLost may be negative (RTCP counts duplicates)
		{-2, 3, 5, 5},
	} {
		if got := counterDelta(tc.prev, tc.cur); got != tc.want {
			t.Errorf("counterDelta(%d, %d) = %d, want %d", tc.prev, tc.cur, got, tc.want)
		}
		if got := lossDelta(tc.prev, tc.cur); got != tc.loss {
			t.Errorf("lossDelta(%d, %d) = %d, want %d", tc.prev, tc.cur, got, tc.loss)
		}
	}
}

// A connection remembers the counters of a bounded number of inbound tracks, whatever share ids its reports name:
// when there are too many, the tracks that the current report doesn't list are forgotten.
func TestClientStatsBounded(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	c := &conn{h: &Hub{metrics: m}, room: &room{}}
	report := func(first, n int) *protocol.ClientStats {
		st := &protocol.ClientStats{}
		for i := first; i < first+n; i++ {
			st.Inbound = append(st.Inbound, protocol.InboundStats{ShareID: fmt.Sprintf("s_%016d", i),
				Kind: protocol.TrackKindVideo, FramesDecoded: 1})
		}
		return st
	}
	const perReport = 2 * protocol.MaxSubs // the most entries that one report may have
	for i := range 10 {
		st := report(i*perReport, perReport)
		c.countClientStats(st)
		if len(c.inbound) > maxInboundKeys {
			t.Fatalf("report %d: %d inbound tracks remembered, want at most %d", i, len(c.inbound), maxInboundKeys)
		}
		for _, in := range st.Inbound {
			if _, ok := c.inbound[inboundKey{shareID: in.ShareID, kind: in.Kind}]; !ok {
				t.Fatalf("report %d: track %s of the current report was forgotten", i, in.ShareID)
			}
		}
	}
	// Two reports' worth fit: a track that skips one report is still known, and doesn't count again.
	c.inbound = nil
	c.countClientStats(report(0, perReport))
	c.countClientStats(report(perReport, perReport))
	if len(c.inbound) != 2*perReport {
		t.Errorf("%d inbound tracks remembered, want both reports' %d", len(c.inbound), 2*perReport)
	}
}

// The metrics take any delta without a panic (a Prometheus counter panics on a negative value), and a hub without
// metrics takes them too.
func TestClientInboundMetrics(t *testing.T) {
	var none *metrics
	none.clientInbound(protocol.TrackKindVideo, inboundCounters{framesDecoded: 1})
	none.shareStatus("", protocol.ShareStatusStarting)
	reg := prometheus.NewRegistry()
	m, err := newMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	m.clientInbound(protocol.TrackKindAudio, inboundCounters{framesDecoded: -1, framesDropped: -1, freezeMs: -1,
		packetsLost: -1, concealedSamples: -1, totalSamples: -1})
	m.shareStatus(protocol.ShareStatusLive, protocol.ShareStatusLive)
	m.shareStatus("", protocol.ShareStatusStarting)
	m.shareStatus(protocol.ShareStatusStarting, protocol.ShareStatusLive)
	m.shareStatus(protocol.ShareStatusLive, "")
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, metric := range mf.GetMetric() {
			if v := metric.GetGauge().GetValue() + metric.GetCounter().GetValue(); v != 0 {
				t.Errorf("%s %v = %v, want 0: no delta was positive, and the share came and went", mf.GetName(),
					metric.GetLabel(), v)
			}
		}
	}
}
