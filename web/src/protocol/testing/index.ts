// Test support for the signaling layer (01 §19, 05 §19.1): the fake WebSocket, a scriptable fake server on top of it,
// and wire fixtures. Only tests import this folder.
export { FakeWebSocket, type ClientMessage, type FakeWebSocketPeer } from './fakeWebSocket';
export { FakeSignalServer, type FakeSignalServerOptions, type HandlerReply } from './fakeSignalServer';
export { makeError, makeWelcome, testLimits } from './fixtures';
