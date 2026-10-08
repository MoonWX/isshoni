// Package signal is the signaling hub (docs/m1/01-protocol.md §15.2): the server side of protocol v1 at GET /ws.
// It upgrades WebSockets, runs the hello handshake, and keeps one actor goroutine per connection that handles the
// connection's messages in order.
//
// The hub reaches the rest of the server only through the interfaces in interfaces.go (Deps): 03's authentication
// and room directory, 02's SFU (through 01's sfuplane), 04's Web Push, admin policy, the client IP and metrics. The
// wiring (04 §6.6) adapts each one; package signaltest has fakes for tests. The hub never imports config, store,
// auth or sfu.
//
// # What exists so far (README slices S11, S19 and S28; 01 slices P3 to P5)
//
//   - upgrade.go: the six upgrade checks of 01 §3.1 (shutdown, the exact Origin allowlist, cookie authentication,
//     pre-auth limits per IP key and server-wide, 16 connections per user, Accept) and the hello handshake of
//     01 §8.2 (validation, cookie or bearer authentication, revalidation, version negotiation, the client floor,
//     then a resume or a new connection);
//   - conn.go: the socket (reader, writer with a bounded send queue, pinger) and the connection actor with its idle
//     timeout, periodic revalidation, revocation and shutdown; a connection outlives its sockets, and leaves its
//     room when it closes;
//   - resume.go: resume and grace (01 §4.2, §10.3, §10.5): the resume token format and its rotation on every
//     welcome; a connection that loses its socket stays detached for Config.Grace, shown as reconnecting, unless the
//     client left on purpose or the hub closed it for good; a hello with its token moves it to a new socket
//     (replacing a half-open one with error{replaced} and 4409) and is answered with welcome{resumed: true},
//     room.state and MediaPeer.Resync;
//   - dispatch.go: message handling: bad_message, the size limits, unknown types, roles, features, the per-type
//     rate limits, ping/pong, room.join and room.leave, caps and stats bookkeeping, and not_in_room outside a room;
//   - room.go: rooms and participants (01 §4.1, §8.4–8.6): a participant merges a user's connections in a room;
//     room.state snapshots are coalesced (at most one broadcast per StateCoalesce per room) and encoded once per
//     broadcast; room.events; watchers from the desired subscriptions; the room_full policy; the MediaPeer of each
//     room membership and its MediaSink;
//   - ratelimit.go: token buckets, the global message and byte limits with the flood rule, per-type limits;
//   - metrics.go: the Prometheus series of 01 §18;
//   - hub.go: Config, Deps, Policy, New, Ready, Shutdown, Notify, CloseConnections, UpdateUser, CloseRoom, Snapshot.
//
// Later slices fill in the rest of the declared interface: shares, subscriptions, negotiation and the other
// MediaPlane calls (S40), and the same-user relay (S51). Until then those requests get error{internal} with a ref,
// and the log says what is not implemented yet.
//
// # Concurrency
//
// Each connection is one actor goroutine that owns the connection's state, from its first welcome through every
// socket and every grace until it closes; other goroutines reach it through its inbox (post never blocks; a full
// inbox closes the connection's socket as slow_connection). Each socket has a reader (the ServeHTTP goroutine), a
// writer that drains the send queue, and a pinger. A reader whose hello resumes a connection hands its socket to
// that connection's actor and waits for the answer. The hub lock guards the maps of sockets, connections and rooms;
// each room's lock guards its participants, shares and snapshot state. No lock is held while calling a dependency (a
// MediaPeer included), writing to a socket, or sending on a channel that can block: a room change collects its
// messages and posts them to the connections' actors after unlocking. A scheduled room.state broadcast runs on its
// timer's goroutine, counted like the others. Lock order: hub → room → connection.
package signal
