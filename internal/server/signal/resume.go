package signal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"slices"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Resume and grace (01 §4.2, §10.3, §10.5).
//
// A connection outlives its socket. When the socket is lost (the network, the idle timeout, a full send queue) the
// connection is detached: it keeps its room membership, its shares, its subscriptions and its MediaPeer, room.state
// shows it as reconnecting, and what the hub would send it is dropped. A hello with the connection's resume token,
// from the same user and session or device, moves the connection to the hello's socket: welcome{resumed: true}, then
// room.state and MediaPeer.Resync. Nobody in the room sees a participant.left or participant.joined for it. After
// Config.Grace without a resume the connection closes, with disconnected.
//
// There is no grace when the client left on purpose (close 1000 or 1001), and none when the hub closed the
// connection for good: a revocation, a shutdown, an error after which the client stops, or a full inbox, after
// which the connection has missed something the hub can't send again (endsWith).
//
// Resume tokens:
//
//	"r1." + base64url(connIdRaw[10] ‖ nonce[16] ‖ HMAC-SHA256(resumeKey, "isshoni-resume-v1" ‖ connIdRaw ‖ nonce)[:16])
//
// The HMAC lets the hub drop forged tokens without a lookup, and tells "valid but from a previous process" apart from
// garbage (both are answered resumed: false, which is not an error). The key is Config.ResumeKey: rotating it
// (isshoni admin rotate-secrets, which restarts the server) invalidates every token. A connection keeps the SHA-256
// of its current token and rotates it on every welcome. A token is never a credential: the socket that presents it
// has authenticated already.
const (
	resumeTokenPrefix = "r1."
	resumeHMACLabel   = "isshoni-resume-v1"
	resumeNonceLen    = 16
	resumeMACLen      = 16
	resumeTokenRawLen = connIDRawLen + resumeNonceLen + resumeMACLen
)

// mintResumeToken returns a new resume token for the connection whose id has the random bytes raw, and the token's
// SHA-256, which the connection keeps as its current token.
func mintResumeToken(key []byte, raw [connIDRawLen]byte) (protocol.Secret, [sha256.Size]byte) {
	var nonce [resumeNonceLen]byte
	_, _ = rand.Read(nonce[:]) // crypto/rand.Read never fails (it crashes the program instead)
	b := make([]byte, 0, resumeTokenRawLen)
	b = append(b, raw[:]...)
	b = append(b, nonce[:]...)
	b = append(b, resumeMAC(key, raw[:], nonce[:])...)
	tok := resumeTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return protocol.Secret(tok), sha256.Sum256([]byte(tok))
}

// parseResumeToken checks a token's HMAC under key and returns the random bytes of the connection id it names. ok
// is false for anything that is not a token of this key: garbage, a forgery, or a token from before a key rotation.
func parseResumeToken(key []byte, tok string) (raw [connIDRawLen]byte, ok bool) {
	rest, found := strings.CutPrefix(tok, resumeTokenPrefix)
	if !found || len(rest) != base64.RawURLEncoding.EncodedLen(resumeTokenRawLen) {
		return raw, false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(b) != resumeTokenRawLen {
		return raw, false
	}
	id, nonce, mac := b[:connIDRawLen], b[connIDRawLen:connIDRawLen+resumeNonceLen], b[connIDRawLen+resumeNonceLen:]
	if !hmac.Equal(mac, resumeMAC(key, id, nonce)) {
		return raw, false
	}
	copy(raw[:], id)
	return raw, true
}

// resumeMAC is the token's truncated HMAC.
func resumeMAC(key, id, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(resumeHMACLabel))
	m.Write(id)
	m.Write(nonce)
	return m.Sum(nil)[:resumeMACLen]
}

// endsWith decides what becomes of the connection now that its socket has closed with code (byHub: the hub chose
// the code, otherwise the client did or the network failed). ends false means the connection stays, detached, for
// Config.Grace. Otherwise it closes at once, with reason (01 §4.2):
//
//   - revoke or shutdown has marked the connection: left or server_shutdown, whatever the code;
//   - the client closed with 1000 or 1001: it left on purpose (the web client sends 1000 on logout and page unload,
//     browsers 1001 on unload, Go clients 1001), so a reloaded sharer's frozen share disappears at once: left. Any
//     other code from the client keeps the grace (the web client drops a socket it wants to replace with 4000);
//   - the hub closed the socket after an error that makes the client stop (01 §12.2), so nothing will resume the
//     connection: a protocol violation (4400: bad_message, a second hello) or a binary frame (1003): disconnected.
//     4401 and 4403 are revocations that reached the socket before the connection was marked: left;
//   - the connection's full inbox dropped a post (conn.post, which closed the socket with 4503): the connection has
//     missed something that the hub can't send again, and a resume would bring back a connection in a wrong state:
//     disconnected (01 §15.2).
//
// The hub's other closes are retryable for the client, which reconnects and resumes: the idle timeout (4408), a
// full send queue (4503), a flood (4429) and an internal error (1011). So are a message over the read limit (1009)
// and a socket that just went away (1006).
func (c *conn) endsWith(code int, byHub bool) (reason protocol.EndReason, ends bool) {
	cc := protocol.CloseCode(code)
	switch {
	case c.endReason != "":
		return c.endReason, true
	case !byHub && (cc == protocol.CloseCodeNormal || cc == protocol.CloseCodeGoingAway):
		return protocol.EndReasonLeft, true
	case byHub && (cc == protocol.CloseCodeProtocolViolation || cc == protocol.CloseCodeUnsupportedData):
		return protocol.EndReasonDisconnected, true
	case byHub && (cc == protocol.CloseCodeUnauthenticated || cc == protocol.CloseCodeForbidden):
		return protocol.EndReasonLeft, true
	case c.lost.Load():
		return protocol.EndReasonDisconnected, true
	}
	return "", false
}

// startGrace leaves the connection detached after its socket was lost (01 §4.2, §10.3): the connection, its
// participant slot, its shares, its subscriptions and its MediaPeer are kept for Config.Grace, and room.state shows
// the connection as reconnecting (its participant too, when all of the user's connections in the room are). Media
// keeps flowing if the PeerConnections are healthy: a WebSocket drop is not a media drop.
func (c *conn) startGrace() {
	c.grace.Reset(c.h.cfg.Grace)
	if c.room != nil {
		c.room.setDetached(c, true)
	}
	c.h.log.Info("connection detached", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.Int("code", c.lastCode))
}

// graceExpired closes a connection that nothing resumed within Config.Grace: its shares end with disconnected, and
// so does its participant with its last connection (01 §10.3). A resume stops the timer, and a stopped timer
// delivers nothing, so the connection is still detached here.
func (c *conn) graceExpired() {
	c.close(protocol.EndReasonDisconnected)
}

// resumeRequest is a hello with a resume token, once its socket has authenticated and the hello has passed the
// version and client checks (01 §8.2 steps 1–4).
type resumeRequest struct {
	sock        *socket
	re          string            // the hello's request id: welcome answers it
	hash        [sha256.Size]byte // SHA-256 of the hello's token
	id          Identity          // who the socket authenticated as, revalidated: the name and admin flag are current
	role        protocol.Role
	version     int // the protocol version the hello negotiated
	caps        protocol.Caps
	defaultRoom string
	pol         Policy
}

// resume tries to resume a connection for a hello with a resume token (01 §8.2 step 5, §10.3). It returns the
// connection, which has taken req.sock and sent welcome{resumed: true}, or nil when the token resumes nothing: the
// hello then opens a new connection, which is not an error. Resume succeeds when
//
//   - the token's HMAC is valid under Config.ResumeKey (so a forged token, or one from before a key rotation, costs
//     no lookup);
//   - the connection it names exists in this process, ready or detached;
//   - the socket authenticated as the connection's user, with the same session (cookie) or device (bearer): the
//     token is not a credential;
//   - the token is the connection's current one (conn.resume, on the actor, which owns the hash).
//
// It runs on the socket's reader and waits for the connection's actor. Why a token resumed nothing is logged at
// DEBUG, without the token.
func (h *Hub) resume(tok protocol.Secret, req resumeRequest) *conn {
	c, why := h.tryResume(tok, &req)
	if why != "" {
		h.log.Debug("resume token not resumed", slog.String("user_id", req.id.UserID), slog.String("reason", why))
		return nil
	}
	return c
}

// Why a resume token resumed nothing, for the DEBUG log line.
const (
	noResumeToken    = "invalid_token"      // not a token of this key: garbage, a forgery, or from before a key rotation
	noResumeConn     = "unknown_connection" // closed (grace expired, the client left), or from a previous process
	noResumeIdentity = "other_identity"     // another user, session or device than the connection's
	noResumeRotated  = "rotated_token"      // an older token of the connection
	noResumeClient   = "other_client"       // another role or protocol version than the connection's
	noResumeClosing  = "closing"            // the connection or the hub is closing for good
)

func (h *Hub) tryResume(tok protocol.Secret, req *resumeRequest) (*conn, string) {
	raw, ok := parseResumeToken(h.cfg.ResumeKey, tok.Reveal())
	if !ok {
		return nil, noResumeToken
	}
	h.mu.Lock()
	c := h.conns[connIDFromRaw(raw)]
	closing := h.closing
	h.mu.Unlock()
	switch {
	case c == nil:
		return nil, noResumeConn
	case closing:
		return nil, noResumeClosing
	case c.userID != req.id.UserID || c.sessionID != req.id.SessionID || c.deviceID != req.id.DeviceID:
		return nil, noResumeIdentity
	}
	req.hash = sha256.Sum256([]byte(tok.Reveal()))
	res := make(chan string, 1)
	if !c.postWait(func() { res <- c.resume(req) }) {
		return nil, noResumeConn // it closed meanwhile
	}
	why := noResumeConn
	select {
	case why = <-res:
	case <-c.done:
		// The actor has ended. It ran the request first or never will; a connection that took the socket can't
		// close before the socket ends, which this goroutine reports.
		select {
		case why = <-res:
		default:
		}
	}
	return c, why
}

// resume moves the connection to the socket of a hello with its resume token, on the actor. It returns "", or why
// it refused. It refuses a token that is not the connection's current one: an older token of the same connection,
// which a welcome has rotated since. It also refuses when the connection is closing for good (revoked, shutting
// down, or its inbox dropped a post), and when the hello asks for another role or negotiated another protocol
// version than the connection has: both are fixed for a connection's lifetime, so such a hello is another client and
// opens a connection of its own.
//
// Otherwise (01 §10.3, §10.5):
//
//  1. a socket that is still attached (half-open: the client has given it up, the hub has not noticed yet) gets
//     error{replaced} and is closed with 4409;
//  2. the connection takes the new socket; in its room it is online again, which room.state shows to everyone;
//  3. welcome{resumed: true, roomId, a new token}, then, in a room, room.state and MediaPeer.Resync: the peer
//     sends its pending sub offer, its subscription statuses and quality hints again and restarts ICE on a sub
//     PeerConnection that is not connected.
//
// What the hub sent while the connection was detached is not replayed. The connection keeps its client info,
// features and rate-limit buckets. The hello's caps replace the connection's: they are the client's current ones,
// which can have changed while it was away (a codec that became available, 01 §11.7).
func (c *conn) resume(req *resumeRequest) string {
	switch {
	case c.closed, c.endReason != "", c.revoked.Load() != nil, c.lost.Load():
		return noResumeClosing
	case subtle.ConstantTimeCompare(req.hash[:], c.tokenHash[:]) != 1:
		return noResumeRotated
	case req.role != c.role || req.version != c.version:
		return noResumeClient
	}
	if old := c.sock; old != nil {
		old.fail(protocol.NewError(protocol.ErrorCodeReplaced, protocol.ErrorScopeConnection), "")
	}
	c.grace.Stop()
	c.h.adopt(req.sock, c)
	if id := c.identity(); id.Name != req.id.Name || id.Admin != req.id.Admin {
		c.setUser(req.id.Name, req.id.Admin)
	}
	if !capsEqual(c.caps, req.caps) {
		c.caps = req.caps
		if c.peer != nil {
			c.peer.SetCaps(req.caps)
		}
	}
	if c.room != nil {
		c.room.setDetached(c, false)
	}
	c.attach(req.sock, req.re, req.defaultRoom, req.pol, true)
	if c.room != nil {
		c.sendStateNow()
		c.peer.Resync()
	}
	return ""
}

// capsEqual reports whether two capability sets are the same.
func capsEqual(a, b protocol.Caps) bool {
	return slices.Equal(a.Decode, b.Decode) && slices.Equal(a.Encode, b.Encode) &&
		a.Simulcast == b.Simulcast && a.DisplayCapture == b.DisplayCapture
}

// adopt records that socket s serves connection c from now on, which resumed on it. The socket gives back what it
// held for a connection of its own (its per-user slot, or its place at the cap): c has had its slot since it
// opened.
func (h *Hub) adopt(s *socket, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.conn.Store(c)
	h.releaseSocketLocked(s)
}
