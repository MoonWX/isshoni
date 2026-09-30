// Package signal is the signaling hub (docs/m1/01-protocol.md §15.2): the server side of protocol v1 at GET /ws.
// It upgrades WebSockets, runs the hello handshake, and keeps one actor goroutine per connection that handles the
// connection's messages in order.
//
// The hub reaches the rest of the server only through the interfaces in interfaces.go (Deps): 03's authentication
// and room directory, 02's SFU (through 01's sfuplane), 04's Web Push, admin policy, the client IP and metrics. The
// wiring (04 §6.6) adapts each one; package signaltest has fakes for tests. The hub never imports config, store,
// auth or sfu.
//
// # What exists so far (README slice S11, 01 slice P3)
//
//   - upgrade.go: the six upgrade checks of 01 §3.1 (shutdown, the exact Origin allowlist, cookie authentication,
//     pre-auth limits per IP key and server-wide, 16 connections per user, Accept) and the hello handshake of
//     01 §8.2 (validation, cookie or bearer authentication, revalidation, version negotiation, the client floor);
//   - conn.go: the socket (reader, writer with a bounded send queue, pinger) and the connection actor with its idle
//     timeout, periodic revalidation, revocation and shutdown;
//   - dispatch.go: message handling: bad_message, the size limits, unknown types, roles, features, the per-type
//     rate limits, ping/pong, caps and stats bookkeeping, and not_in_room outside a room;
//   - ratelimit.go: token buckets, the global message and byte limits with the flood rule, per-type limits;
//   - resume.go: the resume token format (01 §10.3); metrics.go: the Prometheus series of 01 §18;
//   - hub.go: Config, Deps, Policy, New, Ready, Shutdown, Notify, CloseConnections, UpdateUser, CloseRoom, Snapshot.
//
// Later slices fill in the rest of the declared interface: rooms, participants, room.state and room.event (S19),
// resume and grace (S28), shares, negotiation and the MediaPlane calls (S40), and the same-user relay (S51). Until
// then those requests get error{internal} with a ref, and the log says what is not implemented yet.
//
// # Concurrency
//
// Each connection is one actor goroutine that owns the connection's state; other goroutines reach it through its
// inbox (post never blocks; a full inbox closes the connection as slow_connection). Each socket has a reader (the
// ServeHTTP goroutine), a writer that drains the send queue, and a pinger. The hub lock guards the maps of sockets
// and connections and is never held while calling a dependency, writing to a socket, or sending on a channel that
// can block. Lock order: hub → room → connection.
package signal
