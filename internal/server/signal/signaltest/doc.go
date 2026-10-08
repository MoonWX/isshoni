// Package signaltest has fakes of the hub's dependencies (signal.Deps, docs/m1/01-protocol.md §15.2) for tests: Auth
// (signal.Authenticator), Rooms (signal.RoomDirectory), Media and Peer (signal.MediaPlane and signal.MediaPeer),
// Push (signal.PushNotifier), Policy (Deps.Policy) and ClientIP (Deps.ClientIP). 02 and 04 use them too, and so does
// the first server wiring before the real SFU is wired.
//
// It also has what hub tests need around the fakes: PipeNet and Server, an in-memory network and HTTP server for
// testing/synctest bubbles (time is fake there, so tests run the real 10 s, 45 s and 30 s timeouts instantly), and
// Client, a raw protocol client that queues every message it receives. Client.Close drops one socket without a close
// frame and PipeNet.Sever drops them all, which is how tests start a connection's resume grace.
//
// Every fake is safe for concurrent use.
package signaltest
