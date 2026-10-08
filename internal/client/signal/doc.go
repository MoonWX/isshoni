// Package signal is the Go signaling client (docs/m1/01-protocol.md §15.3): one connection to an isshoni server's
// /ws endpoint. In M1 the tests and isshoni-loadtest use it; from M2 the native apps are built on it. It is pure Go
// on coder/websocket and imports nothing else but internal/protocol and the standard library, so it stays
// gomobile-friendly.
//
// A Client owns the WebSocket side of the protocol:
//   - the handshake (01 §8.2): hello with the protocol versions, the client info, the role, the caps and, when
//     there is one, the resume token; cookie auth through Options.Header or a bearer token through Options.Token;
//   - the state machine of 01 §10.1 (connecting, handshaking, ready, backoff, stopped) and the backoff of §10.2;
//   - the resume token of §10.3, kept in memory and sent with every hello after the first welcome;
//   - the application-level heartbeat of §3.4 (ping every limits.pingIntervalMs, a 10 s pong timeout);
//   - requests and their replies, notifications, and the error and close-code mapping of §12.
//
// What to do after a welcome (01 §10.5) is the caller's: States reports every welcome as StateReady with Resumed,
// and Welcome has the room the server resumed the connection into. A caller that was not resumed joins its room
// again, publishes its shares again with replaces and sends its subscriptions; a resumed one sends its full
// subscription set and re-sends a pending pub offer. PeerConnections are the caller's too (01 §9, §10.4): the client
// only carries their pc.* messages. Probe and RetryNow are for what such a caller knows and the client doesn't: a
// PeerConnection that became disconnected, a network that is back (01 §3.4, §10.2).
//
// # Use
//
//	c, err := signal.Dial(ctx, signal.Options{
//		URL:    "wss://watch.example.com/ws",
//		Header: http.Header{"Cookie": {cookie}},
//		Client: protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: version, OS: protocol.ClientOSLinux},
//		Role:   protocol.RoleFull,
//		Caps:   protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264High, protocol.CodecOpus}},
//	})
//	if err != nil {
//		return err
//	}
//	defer c.Close()
//
//	var joined protocol.RoomJoinResult
//	err = c.Request(ctx, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: c.Welcome().DefaultRoomID}, &joined)
//	for env := range c.Events() {
//		switch env.Type {
//		case protocol.MessageTypeRoomState:
//			state, err := protocol.Decode[protocol.RoomState](env)
//			…
//		}
//	}
//
// # Channels
//
// Events and States are the client's two outputs, and both are closed once the client has stopped.
//
// Events must be received. It carries every server notification in the order the server sent it, and it holds at
// most 256 of them: when it is full the client stops reading its socket, replies and pongs included, as any slow
// consumer of a connection would, until the caller receives again. Requests then time out, and once a ping has gone
// 10 s without its pong being read, the client drops the socket and resumes on a new one; what the old socket had
// not handed over is lost, like everything a server sends while a client is away. A caller that does not care
// about notifications drains the channel in a goroutine.
//
// States never blocks the client. It holds 64 changes; when nobody receives, the oldest are dropped.
//
// The two channels are independent, with one guarantee between them: when a welcome does not resume the connection
// (StateReady with Resumed false after the first one: the server restarted, or the grace period ran out), the
// notifications of the old connection that nobody has received yet are dropped before that state is reported. A
// notification received after such a StateReady therefore belongs to the new connection, never to PeerConnections
// and shares that are gone.
//
// # Errors
//
// Request returns the server's error as a *protocol.Error. The client-local errors of 01 §12.4 are ErrConnectionLost,
// ErrRequestTimeout and ErrNotReady. An error in scope connection or session, a close code that stands for one, and
// every error that answers hello decide what the client does next, backoff or stopped (01 §10.1, §12.2); States
// reports them with the state.
//
// Nothing this package logs holds a token, an SDP or a message payload.
package signal
