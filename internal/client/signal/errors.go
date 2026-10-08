package signal

import (
	"errors"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// The client-local errors of 01 §12.4: made by the client, never on the wire. Request and Send return them wrapped;
// test with errors.Is.
var (
	// ErrConnectionLost (connection_lost): the client is connecting or backing off, or the socket went away before
	// the reply. After a lost reply the request may or may not have reached the server; nothing retries it.
	ErrConnectionLost = errors.New("signal: connection lost")
	// ErrRequestTimeout (request_timeout): no reply within RequestTimeout. A late reply is dropped.
	ErrRequestTimeout = errors.New("signal: request timed out")
	// ErrNotReady (not_ready): the client is stopped, by Close or by the server (01 §10.1). Only a new Dial helps.
	ErrNotReady = errors.New("signal: client stopped")
)

// Why the client itself ended a socket or an attempt. They reach callers only inside Dial's error and the log.
var (
	errClosing          = errors.New("signal: closed")
	errHandshakeTimeout = errors.New("signal: no welcome within the handshake timeout")
	errPongTimeout      = errors.New("signal: no pong within the timeout")
	errBadWelcome       = errors.New("signal: malformed welcome")
	errLost             = errors.New("signal: the connection ended")
)

// errorForClose returns the error that a close code stands for when the server closed without sending one first
// (01 §12.2, the column "Client without a preceding error"), or nil when the client just backs off: 1000, 1001, 1006,
// 1009, 1011, 1012, 4408, 4503 and every code this build doesn't know. The codes are the web client's
// (ProtocolError.fromCloseCode), so both clients report the same thing:
//
//   - 1003, 4400 → bad_message: stop (a bug on one side);
//   - 4401 → unauthenticated, scope session: stop, sign in again;
//   - 4403 → forbidden: stop;
//   - 4409 → replaced: stop silently, this socket was superseded;
//   - 4426 → protocol_unsupported: stop, update (the params are empty: the server's versions are unknown);
//   - 4429 → rate_limited, retryable: back off for at least RateLimitMinWait.
func errorForClose(code websocket.StatusCode) *protocol.Error {
	var e protocol.Error
	switch protocol.CloseCode(code) {
	case protocol.CloseCodeUnsupportedData, protocol.CloseCodeProtocolViolation:
		e = protocol.NewError(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection)
	case protocol.CloseCodeUnauthenticated:
		e = protocol.NewError(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession)
	case protocol.CloseCodeForbidden:
		e = protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeConnection)
	case protocol.CloseCodeReplaced:
		e = protocol.NewError(protocol.ErrorCodeReplaced, protocol.ErrorScopeConnection)
	case protocol.CloseCodeVersionUnsupported:
		e = protocol.NewError(protocol.ErrorCodeProtocolUnsupported, protocol.ErrorScopeConnection)
	case protocol.CloseCodeRateLimited:
		e = protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeConnection)
	default:
		return nil
	}
	return &e
}

// decodeError reads the payload of an error message. It tolerates a malformed one (a server bug), like the web
// client: a missing code reads as internal, a missing scope as request, and fields of the wrong type keep their zero
// value. Codes and scopes this build doesn't know are kept as they are (01 §12.3).
func decodeError(env protocol.Envelope) *protocol.Error {
	e, _ := protocol.Decode[protocol.Error](env) //nolint:errcheck // a partly decoded error is still an error
	if e.Code == "" {
		e.Code = protocol.ErrorCodeInternal
	}
	if e.Scope == "" {
		e.Scope = protocol.ErrorScopeRequest
	}
	return &e
}
