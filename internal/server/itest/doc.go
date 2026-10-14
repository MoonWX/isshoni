// Package itest holds the media integration tests of the whole server (docs/m1/01-protocol.md §19 "Integration",
// docs/m1/04-server-platform.md §17): a real server from servertest, with accounts, the signaling hub, the SFU and
// the ICE Transport as `isshoni serve` wires them, and Go clients that use it the way a browser does.
//
// A client here is three of the project's own parts put together (harness_test.go):
//   - an account: registered over REST with an invite, its session cookie in a jar of its own;
//   - internal/client/signal on /ws with that cookie: hello, room.join, share.start, subscribe.update and the
//     pc.* messages of 01 §9. The harness drains the client's Events for as long as it runs: a signaling client
//     whose notifications nobody receives stops reading its socket (01 §15.3);
//   - Pion PeerConnections on loopback: internal/client/publish with internal/media/fake as the pub PC, and an
//     sfutest.Viewer as the sub PC, which answers the server's sub offers and records what arrives.
//
// A client recovers by itself, as the web client does (client_test.go): it resumes after a socket drops, restarts
// ICE and rebuilds its PeerConnections when 01 §10.4 says so, follows codec hints, and after a server restart joins
// its room again, publishes its share again with replaces and moves its subscriptions to the new shares (01 §10.5,
// §10.6). A test can cut a client's WebSocket under it, and take a client's UDP away (sfutest.FaultConn under its
// ICE UDP mux).
//
// What the tests assert is what a client can observe: the messages on its socket and the RTP on its
// PeerConnections. The SFU's own tests (internal/server/sfu) look inside the SFU for the same cases without a hub;
// these go through every layer between a browser and it.
//
// Cases 1–3 and 9 of 01 §19 and the ICE-TCP checks of 04 §17 are here (README S59), and the recovery cases 4–8
// (README S74, recovery_test.go). README S85 added what the metrics endpoint says about them: the ICE-TCP-via-443
// test counts both PeerConnections in isshoni_sfu_selected_transport{transport="tcp443"}, and metrics_test.go
// reads the series of 06 §12.3 (the exit test's) from a session with media, client stats and a resume.
package itest
