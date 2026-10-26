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
//   - pubdriver.go: the pub PC's negotiation (01 §9) and its recovery (§11.6): ICE restarts, rebuilds, unreachable;
//   - recovery.go: reconnects (§11.7): the 30 s hold while signaling is down, the resume, the republish on a new
//     connection, the reconcile with room.state;
//   - events.go: Status, the events for the app, Subscribe's channels and EventShare's coalescing;
//   - hints.go: the server's quality hints (the codec switch, layer pausing, caps) and the rate controller's Ticks,
//     presets (Share.SetPreset, and one set from another connection), pause (Share.SetPaused, and one set from
//     another connection), and what a republish owes them;
//   - stats.go: the engine's Stats poll into the snapshot, the stats reports to the server every 10 s, and the
//     server's view of the share (WatchServerStats, stats.watch);
//   - relay.go: the same-user relay, of which the core consumes playback.state only (EventPlaybackState).
//
// Bearer auth's integration tests come with README WA73.
//
// The core never stores a token: 02's manager owns the credential. Nothing it logs holds a token, an SDP, an ICE
// candidate, a window title, a source or app name, or a relay payload (§11.12). It has no Wails dependency (D18) and
// builds for every OS; non-test files import only sibling client packages, internal/protocol, internal/media's root,
// internal/version and internal/logx from this repository (01 D2).
package core
