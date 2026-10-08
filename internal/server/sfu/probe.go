package sfu

import (
	"context"
	"time"
)

// ProbeResult is the outcome of one connection-test probe PC (02 §7.6).
type ProbeResult struct {
	Transport ProbeTransport
	Connected bool
	Selected  string        // "udp 203.0.113.5:7882" (server side of the nominated pair)
	RTT       time.Duration // from the candidate-pair stats
	Err       string        // "timeout" | "closed"
}

// Probe answers a connection-test offer on one ICE transport (02 §7.6): offerSDP is a browser offer with one data
// channel and no media; the answer is complete and holds only candidates of the requested transport; the probe PC
// echoes every data-channel message and lives at most 20 s. res gets one value, when the probe connects or times
// out; it has capacity 1, so callers may ignore it. The errors are sfu.transport_disabled when the transport has no
// mux and sfu.probe_limit when 20 probes already run.
//
// README S76 implements it on the probe APIs that New already builds (api.go); until then it returns an error that
// wraps ErrNotImplemented.
func (s *SFU) Probe(ctx context.Context, user UserID, t ProbeTransport, offerSDP string,
) (answerSDP string, res <-chan ProbeResult, err error) {
	return "", nil, errNotImplemented("SFU.Probe", "S76")
}
