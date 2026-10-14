// Package sfutest holds the SFU's test tools (docs/m1/02-sfu.md §15.1, §17), used by the SFU tests, 01's
// integration tests (internal/server/itest) and isshoni-loadtest.
//
// Viewer is a Pion subscriber like a browser: H.264 (the five profiles of 02 §8.1 unless told otherwise) with RTX,
// Opus, NACKs and receiver reports. It answers offers (the SFU's sub offers, or a publish.Publisher's
// offer in the loopback tests) and records each track in a Recorder: sequence numbers, timestamps, arrival times,
// a hash of each payload, the fake media's markers (layer, frame, flash, beep), keyframes, gaps, packets recovered
// by RTX, the NACKs it sent, the final loss after 1 s, and the sender reports. The checks of the S4 spike work on
// those records (CheckContinuous, CheckStartsOnSPS), plus the marker checks, the GOP bitrate and Viewer.AVOffset
// (flash against beep, both mapped to NTP through the last sender reports). WithoutPartialTail and WithoutCutFrames
// leave out the frames that may be short by design: the newest one of a running stream, and the old layer's last one
// at a layer switch; InSeqOrder puts a stream with repairs in the order a decoder gets it in. REMBs are fixed values
// a test sends with Viewer.SendREMB. Viewer.Rebuild gives it a new PeerConnection, as a client does for a sub PC of
// a new gen; an offer with new ICE credentials restarts ICE on the one it has. ViewerOptions.H264 and NoH264 make
// it a Firefox, with Constrained Baseline and Baseline only or before its decoder download, and Viewer.SetH264
// installs a decoder for its next PeerConnections, as a browser that fetched OpenH264 uses it on a new
// PeerConnection (02 §8.5).
//
// ProfileSource is a publish.ProfileSource of fake.Sources, one per profile, on one timeline: what a sharer's
// encoder does when the room's codec policy changes the profile (02 §8.3).
//
// The Viewer's NACKs are its own, from what its Recorders miss, timed about as a browser's (NACKAfter, NACKEvery,
// NACKTries), and they stop when the packet arrives, on the track or through RTX. Pion's NACK generator doesn't: it
// never sees what Pion's receiver recovered from an RTX stream, and asks for it again every interval for as long as
// its window holds the sequence number.
//
// FaultConn wraps a UDP socket to lose chosen datagrams or black-hole a peer. It goes under the Harness's SFU
// (HarnessOptions.FaultyUDP, through 04's netx.TransportOptions.PacketConns), where its filter tells the clients
// apart by their addresses, or under a Go client through webrtc.NewICEUDPMux, which is how the recovery tests take a
// client's network away.
//
// Harness is a real sfu.SFU on a loopback netx.Transport (UDP, and on request the ICE-TCP listener and 04's 443
// multiplexer, all on ephemeral ports of 127.0.0.1), without a hub: each joined Conn gets a DirectSignaler, which
// records the Conn's sub offers and events and can answer the offers with a Viewer (WaitAnswered tells when the Conn
// has an answer), and a RoomEventLog records the SFU's RoomEvents with the time of each call. The DirectSignaler
// answers as a client does (01 §9): an offer of a new gen on a new PeerConnection, a repeated one with the answer
// it gave before. SetDown stands in for a WebSocket that is down: what the Conn sends meanwhile is lost, and the
// test calls Conn.Resync when it is back. Publish runs a publish.Publisher's side of a pub negotiation, the first
// one or a re-offer, with its tracks bound to a share (Bindings); PublishICERestart, the re-offer that restarts ICE.
// The SFU's in-process integration tests (02 §17) are written on these.
//
// Later slices add the rest of the package from 02 §4 and §17 with the first test that needs each part: REMB from
// the rembsim model and a FaultConn rate limit (README S84).
package sfutest
