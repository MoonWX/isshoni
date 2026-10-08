package signal

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Message directions, the dir label of isshoni_ws_messages_total.
const (
	dirIn  = "in"
	dirOut = "out"
)

// metrics are the hub's Prometheus series (01 §18). A nil *metrics (no Deps.Metrics) records nothing.
type metrics struct {
	connections *prometheus.GaugeVec   // isshoni_ws_connections{kind,role}
	messages    *prometheus.CounterVec // isshoni_ws_messages_total{type,dir}
	errors      *prometheus.CounterVec // isshoni_ws_errors_total{code}
	resume      *prometheus.CounterVec // isshoni_ws_resume_total{result}
	closes      *prometheus.CounterVec // isshoni_ws_close_total{code}

	rooms        prometheus.Gauge     // isshoni_rooms
	participants prometheus.Gauge     // isshoni_participants
	shares       *prometheus.GaugeVec // isshoni_shares{status}

	clientFramesDecoded     prometheus.Counter
	clientFramesDropped     prometheus.Counter
	clientFreezeSeconds     prometheus.Counter
	clientPacketsLost       *prometheus.CounterVec // {kind}
	clientAudioSamples      prometheus.Counter
	clientAudioConcealedSmp prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		return nil, nil // no registerer: no metrics
	}
	m := &metrics{
		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "isshoni_ws_connections", Help: "Open signaling connections, including detached ones within their resume grace.",
		}, []string{"kind", "role"}),
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_ws_messages_total", Help: "Signaling messages received (dir=in) and queued (dir=out).",
		}, []string{"type", "dir"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_ws_errors_total", Help: "Signaling error messages sent, by code.",
		}, []string{"code"}),
		resume: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_ws_resume_total", Help: "Hellos with a resume token, by result.",
		}, []string{"result"}),
		closes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_ws_close_total", Help: "Closed WebSockets of signaling connections, by close code.",
		}, []string{"code"}),
		rooms: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "isshoni_rooms", Help: "Rooms with at least one participant.",
		}),
		participants: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "isshoni_participants", Help: "Participants over all rooms.",
		}),
		shares: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "isshoni_shares", Help: "Shares, by status.",
		}, []string{"status"}),
		clientFramesDecoded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "isshoni_client_frames_decoded_total", Help: "Video frames decoded by viewers (client stats).",
		}),
		clientFramesDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "isshoni_client_frames_dropped_total", Help: "Video frames dropped by viewers (client stats).",
		}),
		clientFreezeSeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "isshoni_client_freeze_seconds_total", Help: "Video freeze time seen by viewers (client stats).",
		}),
		clientPacketsLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "isshoni_client_packets_lost_total", Help: "Packets lost on viewers' inbound tracks (client stats).",
		}, []string{"kind"}),
		clientAudioSamples: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "isshoni_client_audio_samples_total", Help: "Audio samples received by viewers (client stats).",
		}),
		clientAudioConcealedSmp: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "isshoni_client_audio_concealed_samples_total", Help: "Audio samples concealed by viewers (client stats).",
		}),
	}
	cs := []prometheus.Collector{
		m.connections, m.messages, m.errors, m.resume, m.closes, m.rooms, m.participants, m.shares,
		m.clientFramesDecoded, m.clientFramesDropped, m.clientFreezeSeconds, m.clientPacketsLost,
		m.clientAudioSamples, m.clientAudioConcealedSmp,
	}
	var errs []error
	for i, c := range cs {
		if err := reg.Register(c); err != nil {
			for _, done := range cs[:i] {
				reg.Unregister(done)
			}
			errs = append(errs, err)
			break
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("signal: register metrics: %w", err)
	}
	return m, nil
}

// kindLabel bounds the kind label to the known client kinds.
func kindLabel(k protocol.ClientKind) string {
	if k.Valid() {
		return string(k)
	}
	return "other"
}

// typeLabel bounds the type label to the registry's message types.
func typeLabel(t protocol.MessageType) string {
	if _, ok := protocol.Lookup(t, protocol.DirClientToServer|protocol.DirServerToClient); ok {
		return string(t)
	}
	return "unknown"
}

func (m *metrics) connOpened(k protocol.ClientKind, r protocol.Role) {
	if m != nil {
		m.connections.WithLabelValues(kindLabel(k), string(r)).Inc()
	}
}

// connClosed counts a connection out of isshoni_ws_connections: when it closes, not when it loses a socket.
func (m *metrics) connClosed(k protocol.ClientKind, r protocol.Role) {
	if m != nil {
		m.connections.WithLabelValues(kindLabel(k), string(r)).Dec()
	}
}

// socketClosed counts one closed WebSocket of a connection in isshoni_ws_close_total. A connection that resumes
// has several over its lifetime, the replaced ones (4409) included.
func (m *metrics) socketClosed(code int) {
	if m != nil {
		m.closes.WithLabelValues(strconv.Itoa(code)).Inc()
	}
}

func (m *metrics) message(t protocol.MessageType, dir string) {
	if m != nil {
		m.messages.WithLabelValues(typeLabel(t), dir).Inc()
	}
}

func (m *metrics) errorSent(code protocol.ErrorCode) {
	if m != nil {
		label := string(code)
		if !code.Valid() {
			label = "unknown"
		}
		m.errors.WithLabelValues(label).Inc()
	}
}

// roomCounts sets isshoni_rooms and isshoni_participants.
func (m *metrics) roomCounts(rooms, participants int) {
	if m != nil {
		m.rooms.Set(float64(rooms))
		m.participants.Set(float64(participants))
	}
}

// shareStatus moves one share from status from to status to in isshoni_shares{status}; "" stands for no share: a
// new one (from) or one that has ended (to).
func (m *metrics) shareStatus(from, to protocol.ShareStatus) {
	if m == nil || from == to {
		return
	}
	if from != "" {
		m.shares.WithLabelValues(string(from)).Dec()
	}
	if to != "" {
		m.shares.WithLabelValues(string(to)).Inc()
	}
}

// clientInbound adds what one inbound track of a client's stats report gained since the previous report to the
// isshoni_client_* counters (01 §8.11). kind is a known track kind (protocol.ClientStats.Validate).
func (m *metrics) clientInbound(kind protocol.TrackKind, d inboundCounters) {
	if m == nil {
		return
	}
	// A counter panics on a negative value. The deltas are never negative; a report must not be able to change that.
	add := func(c prometheus.Counter, v float64) {
		if v > 0 {
			c.Add(v)
		}
	}
	add(m.clientFramesDecoded, float64(d.framesDecoded))
	add(m.clientFramesDropped, float64(d.framesDropped))
	add(m.clientFreezeSeconds, float64(d.freezeMs)/1000)
	add(m.clientPacketsLost.WithLabelValues(string(kind)), float64(d.packetsLost))
	add(m.clientAudioSamples, float64(d.totalSamples))
	add(m.clientAudioConcealedSmp, float64(d.concealedSamples))
}

func (m *metrics) resumeResult(resumed bool) {
	if m != nil {
		result := "not_resumed"
		if resumed {
			result = "resumed"
		}
		m.resume.WithLabelValues(result).Inc()
	}
}
