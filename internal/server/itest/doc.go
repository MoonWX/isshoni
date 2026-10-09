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
// What the tests assert is what a client can observe: the messages on its socket and the RTP on its
// PeerConnections. The SFU's own tests (internal/server/sfu) look inside the SFU for the same cases without a hub;
// these go through every layer between a browser and it.
//
// Cases 1–3 and 9 of 01 §19 and the ICE-TCP checks of 04 §17 are here (README S59). The recovery cases 4–8 come
// with README S74, and the metric assertion of the ICE-TCP test with README S85.
package itest
