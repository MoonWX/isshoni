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
// Later slices add the rest of the package from 02 §4 and §17 with the first test that needs each part: Harness and
// DirectSignaler with the SFU's API (README S29), REMB from the rembsim model and a FaultConn rate limit (S84).
package sfutest
