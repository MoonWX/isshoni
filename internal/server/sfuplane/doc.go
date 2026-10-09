// Package sfuplane is the adapter between the signaling hub and the SFU (docs/m1/01-protocol.md §15.4): it
// implements signal.MediaPlane on top of 02's *sfu.SFU. The SFU keeps its own Go types and never imports signal or
// protocol, and the hub never imports sfu, so this is the only package that knows both sides. It translates and
// keeps no policy: what a share encodes, which layer a viewer gets, when a hint goes out and what it says are all
// the SFU's.
//
// The wiring (04 §6.6) builds it in three steps, because the SFU needs its RoomEvents before it exists:
//
//	plane, events := sfuplane.New(log)
//	s, err := sfu.New(cfg, sfu.Deps{Events: events, Logger: log})
//	plane.Bind(s)
//
// and passes plane as signal.Deps.Media.
//
// # The three directions
//
//   - Calls (peer.go): the hub calls a signal.MediaPeer, one per connection and room, and each method becomes one
//     call of that connection's *sfu.Conn under a 5 s context, with the arguments converted.
//   - Events (peer.go, plane.go): the SFU calls the peer as its sfu.Signaler (sub offers, subscription states,
//     hints, PC states, errors) and the Plane's sfu.RoomEvents (a share's media facts); each becomes a
//     signal.MediaSink call. A share's facts go to the sink of the connection that publishes it.
//   - Values and errors (convert.go, errors.go): every enum maps by name, never by number; an H.264 profile key gets
//     its "h264/" prefix; the SFU's finer subscription reasons become the wire's four; and every sfu.* error code
//     becomes a protocol.ErrorCode with the scope and fields of the call it came from. No sfu.* code, SDP, candidate
//     or log message ever reaches a client.
//
// # Errors
//
// A MediaPeer method returns a *protocol.Error, which the hub sends as it is. Two SFU errors send nothing where
// nothing waits for an answer: sfu.stale_answer, and sfu.closed on the notification paths. An SFU error with no wire
// code of its own (sfu.internal, sfu.busy, a code this package doesn't know) becomes internal with a new 8-character
// params.ref, and the same ref is logged with the SFU's error, so a user's report leads to the log line. Its
// retryable flag is the SFU's: a call that broke the SFU's contract, or reached a method that isn't implemented yet,
// can't succeed on a second try.
//
// # What exists so far (README slice S50; 01 slice P8, the adapter)
//
// All of 01 §15.4. The peer calls every method of *sfu.Conn as the table says, also the one that a later slice of
// the SFU implements: SetDecodeCaps (README S69). Until then the SFU answers it with an sfu.internal that is not
// retryable, so a caps.update gets only a log line (02 §6.3); nothing here changes when it lands, as nothing did
// when RestartICE, ResetPC, ClosePC and Resync did (README S57). The integration tests on the hub, this adapter and
// the SFU together are internal/server/itest's (README S59, S74).
//
// # Concurrency
//
// The hub calls a MediaPeer only from its connection's actor. The SFU calls the Signaler from the Conn's actor and
// the RoomEvents from its ticker and from whoever ends a share, the latter with the share's report lock held. So
// nothing here blocks and nothing here calls the SFU from inside one of the SFU's calls: an event is converted and
// handed to the MediaSink, which only queues it for the hub's actor. The Plane's one lock guards its table of peers
// and each share's last reported state; it is never held while calling the SFU or a MediaSink.
package sfuplane
