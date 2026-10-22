// Package core is the desktop app's Go client core (docs/m2/01-client-core.md §11): the app's own signaling
// connection to its linked server (role publisher, client kind desktop), its shares, the media pump between the
// engine and the publisher, and the events the app shows.
//
// The app runs two connections to its server, authenticated as the same device, so the server sees one user and,
// in a room, one Participant (§11.1): the SPA's viewer connection follows the window's room, and this package's
// publisher connection joins a room only while it shares there (D12), or briefly to stop another share of the user
// (Client.StopOther).
//
// A Client runs once (Run) and owns one share at a time (Client.StartShare):
//   - core.go: Options, Auth (02's *deviceauth.Manager), the Client, Run and Close;
//   - conn.go: the connection: signal.Dial with the hello of §11.1, the stops of §11.3 (one forced token refresh
//     after unauthenticated, Auth.SessionEnded on a session stop), and the connection actor that drains the
//     signaling client and owns the room state;
//   - share.go: StartShare's steps (room.join, share.start, Engine.Start), StopOther, and the share's actor, which
//     owns the engine session, the Publisher, the rate controller and the pub PC; Share.Stop and the share's end;
//   - pump.go: the media pump, the only caller of Session.Next (D5): bounded queues to the Publisher, whole-frame
//     drops with a keyframe request, keyframe requests throttled to one per layer per 300 ms;
//   - pubdriver.go: the pub PC's negotiation (01 §9);
//   - events.go: Status, the events for the app, Subscribe's channels and EventShare's coalescing;
//   - hints.go, relay.go, stats.go: hints and pause, the relay, and stats, which later slices of docs/m2/README.md
//     fill in (README WA46 and WA55); recovery (§11.6, §11.7) comes with README WA38, and bearer auth's integration
//     tests with WA73.
//
// The core never stores a token: 02's manager owns the credential. Nothing it logs holds a token, an SDP, an ICE
// candidate, a window title, a source or app name, or a relay payload (§11.12). It has no Wails dependency (D18) and
// builds for every OS; non-test files import only sibling client packages, internal/protocol, internal/media's root,
// internal/version and internal/logx from this repository (01 D2).
package core
