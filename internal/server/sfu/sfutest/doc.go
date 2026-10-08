// Package sfutest holds the SFU's test tools (docs/m1/02-sfu.md §15.1, §17), used by the SFU tests, 01's
// integration tests (internal/server/itest) and isshoni-loadtest.
//
// Viewer is a Pion subscriber like a browser: H.264 (the five profiles of 02 §8.1 unless told otherwise) with RTX,
// Opus, a NACK generator and receiver reports. It answers offers (the SFU's sub offers, or a publish.Publisher's
// offer in the loopback tests) and records each track in a Recorder: sequence numbers, timestamps, arrival times,
// the fake media's markers (layer, frame, flash, beep), keyframes, gaps, packets recovered by RTX, the final loss
// after 1 s, and the sender reports. The checks of the S4 spike work on those records (CheckContinuous,
// CheckStartsOnSPS), plus the marker checks, the GOP bitrate and Viewer.AVOffset (flash against beep, both mapped to
// NTP through the last sender reports). REMBs are fixed values a test sends with Viewer.SendREMB.
//
// FaultConn wraps a UDP socket to lose chosen datagrams or black-hole a peer; tests inject it as the SFU's socket
// through 04's netx.TransportOptions.PacketConns, or under a Go client through webrtc.NewICEUDPMux.
//
// Harness is a real sfu.SFU on a loopback netx.Transport (UDP, and on request the ICE-TCP listener and 04's 443
// multiplexer, all on ephemeral ports of 127.0.0.1), without a hub: each joined Conn gets a DirectSignaler, which
// records the Conn's sub offers and events and can answer the offers with a Viewer, and a RoomEventLog records the
// SFU's RoomEvents with the time of each call. Publish runs a publish.Publisher's side of a pub negotiation, with
// its tracks bound to a share (Bindings). The SFU's in-process integration tests (02 §17) are written on these.
//
// Later slices add the rest of the package from 02 §4 and §17 with the first test that needs each part: a FaultConn
// under the Harness's UDP socket (README S57, S63), REMB from the rembsim model and a FaultConn rate limit (S84).
package sfutest
