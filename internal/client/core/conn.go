package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The connection (docs/m2/01-client-core.md §11.1, §11.3): one signaling client of role publisher at a time, its
// stops, and the connection actor that drains its notifications and owns the room state.

// helloFeatures are the features the Go connection asks for (§11.1).
var helloFeatures = []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureSharePause}

// helloCaps returns the hello's caps from the engine's probe (§11.1): decode nothing; encode H.264 High (with its
// Constrained High alias) when an available encoder encodes High, Constrained Baseline when one encodes it, and Opus;
// simulcast and display capture. When the probe fails, every H.264 key is listed: the share's start reports the
// engine's error.
func (c *Client) helloCaps(ctx context.Context) protocol.Caps {
	high, cb := true, true
	if probe, err := c.o.Engine.Probe(ctx, false); err != nil {
		c.log.Warn("engine probe failed", logx.Err(err))
	} else {
		high, cb = false, false
		for _, e := range probe.Encoders {
			if !e.Tested {
				continue
			}
			high = high || slices.Contains(e.Profiles, highProfile)
			cb = cb || slices.Contains(e.Profiles, cbProfile)
		}
	}
	encode := []protocol.CodecKey{}
	if high {
		encode = append(encode, protocol.CodecH264High, protocol.CodecH264ConstrainedHigh)
	}
	if cb {
		encode = append(encode, protocol.CodecH264ConstrainedBaseline)
	}
	encode = append(encode, protocol.CodecOpus)
	return protocol.Caps{Decode: []protocol.CodecKey{}, Encode: encode, Simulcast: true, DisplayCapture: true}
}

// connect runs connections until ctx ends (nil) or a terminal stop (§11.3). It dials anew after a forced token
// refresh: the new connection's welcome is not resumed. ended is the code terminate reports to Auth.SessionEnded, ""
// for none.
func (c *Client) connect(ctx context.Context, caps protocol.Caps) (stop *StopError, ended protocol.ErrorCode) {
	forced := false // the next hello carries the token of a forced refresh
	first := true
	for n := 0; ctx.Err() == nil; {
		if first {
			c.setConn(ConnConnecting)
		} else {
			c.signalDown() // a signaling client that stopped (a forced refresh's new dial): the share's hold runs
			c.setConn(ConnReconnecting)
		}
		first = false
		sig, err := c.dial(ctx, caps)
		authErr := c.takeAuthErr()
		var perr *protocol.Error
		switch {
		case authErr != nil:
			if sig != nil {
				_ = sig.Close()
			}
			return authErr, ""
		case err == nil:
			n = 0
			var se *StopError
			if perr, se = c.serve(ctx, sig); se != nil {
				return se, ""
			}
			if perr == nil {
				return nil, "" // ctx ended: the signaling client stays open for shutdown's share.stop
			}
			c.dropSignaling()
			forced = false // a welcome came since the forced refresh
		case !errors.As(err, &perr):
			// Dial ends only with ctx's error or a server error that reconnecting can't fix; anything else is a bug
			// somewhere, so wait and try again rather than give up (the loop ends when ctx has).
			if ctx.Err() == nil {
				c.log.Warn("signaling dial failed", logx.Err(err))
				sleep(ctx, backoffDelay(n))
				n++
			}
			continue
		}
		se, again, ended := c.onStop(ctx, perr, forced)
		if !again {
			return se, ended
		}
		forced = true
	}
	return nil, ""
}

// dial opens a signaling client and returns after its first welcome (signal.Dial retries with its backoff until
// then). A terminal Auth error of a token call ends the dial (token).
func (c *Client) dial(ctx context.Context, caps protocol.Caps) (*signal.Client, error) {
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.dialCancel = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.dialCancel = nil
		c.mu.Unlock()
	}()
	o := signal.Options{
		URL:        c.wsURL,
		Header:     c.o.Header,
		Client:     c.o.Client,
		Role:       protocol.RolePublisher,
		Caps:       caps,
		Features:   helloFeatures,
		HTTPClient: c.o.HTTPClient,
		Logger:     c.log,
	}
	if c.o.Auth != nil {
		o.Token = c.token
	}
	return signal.Dial(dctx, o)
}

// token is the signaling client's Token: Auth.AccessToken(ctx, false), or once the token of a forced refresh. An
// error that says the device is no longer linked, or that the app must be updated, stops the connection: it ends a
// dial in progress, and the connection actor closes a running client (serve).
func (c *Client) token(ctx context.Context) (protocol.Secret, error) {
	c.mu.Lock()
	if f := c.forced; f != nil {
		c.forced = nil
		c.mu.Unlock()
		return *f, nil
	}
	c.mu.Unlock()
	tok, err := c.o.Auth.AccessToken(ctx, false)
	if err == nil {
		return tok, nil
	}
	if se := authStopFor(err); se != nil {
		c.mu.Lock()
		if c.authErr == nil {
			c.authErr = se
		}
		cancel := c.dialCancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		select {
		case c.authStop <- se:
		default:
		}
	}
	return "", fmt.Errorf("core: access token: %w", err)
}

// takeAuthErr returns the terminal Auth error of the dial that just ended, if any.
func (c *Client) takeAuthErr() *StopError {
	c.mu.Lock()
	defer c.mu.Unlock()
	se := c.authErr
	c.authErr = nil
	return se
}

// authStopFor returns the stop an Auth error means: unlinked for RelinkRequired, update_required for UpdateRequired
// (§11.3); nil for a transient error.
func authStopFor(err error) *StopError {
	switch {
	case chainHas(err, func(e error) bool {
		r, ok := e.(interface{ RelinkRequired() bool })
		return ok && r.RelinkRequired()
	}):
		return &StopError{Reason: StopUnlinked}
	case chainHas(err, func(e error) bool {
		u, ok := e.(interface{ UpdateRequired() bool })
		return ok && u.UpdateRequired()
	}):
		return &StopError{Reason: StopUpdateRequired}
	}
	return nil
}

// chainHas reports whether an error in err's tree (Unwrap() error and Unwrap() []error) satisfies match.
func chainHas(err error, match func(error) bool) bool {
	for err != nil {
		if match(err) {
			return true
		}
		switch u := err.(type) { //nolint:errorlint // walking the tree by hand: errors.As stops at the first match
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				if chainHas(e, match) {
					return true
				}
			}
			return false
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		default:
			return false
		}
	}
	return false
}

// onStop applies §11.3's table to the server's error that stopped the signaling client. again means: dial anew,
// with the token of the forced refresh that onStop made (unauthenticated, the first time). afterForced says the
// stopped hello carried such a token. ended is the code for Auth.SessionEnded ("" for none), which terminate reports
// once the share has ended: capture stops at once, not after the manager's refresh (up to 15 s).
func (c *Client) onStop(ctx context.Context, perr *protocol.Error, afterForced bool) (se *StopError, again bool,
	ended protocol.ErrorCode) {
	stop := func(r StopReason) *StopError { return &StopError{Reason: r, Code: perr.Code, Err: perr} }
	switch perr.Code {
	case protocol.ErrorCodeUnauthenticated:
		switch {
		case c.o.Auth == nil:
			return stop(StopUnlinked), false, ""
		case afterForced:
			return stop(StopUnlinked), false, perr.Code
		}
		se, again := c.forceRefresh(ctx)
		return se, again, ""
	case protocol.ErrorCodeSessionRevoked:
		return stop(StopUnlinked), false, perr.Code
	case protocol.ErrorCodeAccountDisabled:
		return stop(StopDisabled), false, perr.Code
	case protocol.ErrorCodeProtocolUnsupported, protocol.ErrorCodeClientOutdated:
		return stop(StopUpdateRequired), false, ""
	case protocol.ErrorCodeTooManyConnections:
		return stop(StopTooMany), false, ""
	case protocol.ErrorCodeReplaced:
		return stop(StopReplaced), false, ""
	}
	// bad_message, and any other code that reconnecting can't fix (forbidden for a 4403 close without an error):
	// a bug on one side.
	return stop(StopProtocol), false, ""
}

// forceRefresh asks Auth once for a new token after an unauthenticated (§11.3). A transient error is retried with
// 01 §10.2's backoff until ctx ends.
func (c *Client) forceRefresh(ctx context.Context) (*StopError, bool) {
	for n := 0; ; n++ {
		tctx, cancel := context.WithTimeout(ctx, signal.ConnectTimeout)
		tok, err := c.o.Auth.AccessToken(tctx, true)
		cancel()
		if err == nil {
			c.mu.Lock()
			c.forced = &tok
			c.mu.Unlock()
			return nil, true
		}
		if se := authStopFor(err); se != nil {
			return se, false
		}
		if ctx.Err() == nil {
			c.log.Info("access token refresh failed, retrying", logx.Err(err))
		}
		if !sleep(ctx, backoffDelay(n)) {
			return nil, false // Run's ctx ended
		}
	}
}

// sessionEnded calls Auth.SessionEnded, bounded at 15 s, before the stop is reported (§11.3). Without Auth (cookie
// auth) or without a code there is nobody to tell.
func (c *Client) sessionEnded(code protocol.ErrorCode) {
	if c.o.Auth == nil || code == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionEndedTimeout)
	defer cancel()
	c.o.Auth.SessionEnded(ctx, code)
}

// serve is the connection actor: it drains the signaling client's notifications and states until the client stops
// (the server's error is returned), a token call stops it (the StopError), or ctx ends (both nil; the client stays
// open). Long work never runs here: a share's actor does it.
func (c *Client) serve(ctx context.Context, sig *signal.Client) (*protocol.Error, *StopError) {
	c.mu.Lock()
	c.sig = sig
	c.mu.Unlock()
	events, states := sig.Events(), sig.States()
	for {
		select {
		case env, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			c.handle(env)
		case sc, ok := <-states:
			if !ok {
				return &protocol.Error{Code: protocol.ErrorCodeBadMessage}, nil // never: States reports the stop first
			}
			if sc.State == signal.StateStopped {
				if sc.Err == nil {
					// Only Close stops a client without an error, and nobody but shutdown closes it.
					return &protocol.Error{Code: protocol.ErrorCodeBadMessage}, nil
				}
				return sc.Err, nil
			}
			c.onState(sig, sc)
		case se := <-c.authStop:
			c.dropSignaling()
			return nil, se
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// dropSignaling closes the current signaling client, if any, and forgets it.
func (c *Client) dropSignaling() {
	c.mu.Lock()
	sig := c.sig
	c.sig = nil
	c.mu.Unlock()
	if sig != nil {
		if err := sig.Close(); err != nil {
			c.log.Debug("signaling close", logx.Err(err))
		}
	}
}

// onState maps the signaling client's states to Status.Conn (§11.3) and takes the welcome's values. A welcome that
// is not the connection's first and not resumed starts a new connection: the server forgot the room and the share.
// Leaving ready and coming back are the share's too (§11.7): its hold, its resume or its republish.
func (c *Client) onState(sig *signal.Client, sc signal.StateChange) {
	switch sc.State {
	case signal.StateConnecting, signal.StateHandshaking:
		c.signalDown()
		c.setConn(ConnConnecting)
	case signal.StateBackoff:
		c.signalDown()
		c.setConn(ConnReconnecting)
	case signal.StateReady:
		w := sig.Welcome()
		c.mu.Lock()
		reset := c.welcomed && !sc.Resumed
		c.welcomed = true
		switch {
		case reset:
			c.room.reset("")
		case sc.Resumed:
			if w.RoomID != c.room.id {
				c.room.reset(w.RoomID)
			}
		}
		noShare := c.share == nil
		room, seq := c.room.id, c.room.seq
		c.mu.Unlock()
		c.setStatus(func(st *Status) {
			st.Conn = ConnOnline
			st.ConnectionID = w.ConnectionID
			st.ServerVersion = w.ServerVersion
			st.Features = slices.Clone(w.Features)
		})
		c.signalReady(w.ConnectionID, sc.Resumed)
		if sc.Resumed && noShare && room != "" {
			c.leaveAfterResume(room, seq)
		}
	}
}

// handle routes one notification: the room state to the room view and the share, the share's messages to its actor.
func (c *Client) handle(env protocol.Envelope) {
	switch env.Type {
	case protocol.MessageTypeRoomState:
		if st, ok := decode[protocol.RoomState](c, env); ok {
			c.onRoomState(st)
		}
	case protocol.MessageTypeRoomEvent:
		if ev, ok := decode[protocol.RoomEvent](c, env); ok {
			c.toShare(msgRoomEvent{ev})
		}
	case protocol.MessageTypePCAnswer:
		if a, ok := decode[protocol.PCAnswer](c, env); ok && a.PC == protocol.PCKindPub {
			c.toShare(msgAnswer{a})
		}
	case protocol.MessageTypePCRestart:
		if r, ok := decode[protocol.PCRestart](c, env); ok && r.PC == protocol.PCKindPub {
			c.toShare(msgRestart{r})
		}
	case protocol.MessageTypeQualityHint:
		if h, ok := decode[protocol.QualityHint](c, env); ok {
			c.toShare(msgHint{h})
		}
	case protocol.MessageTypeError:
		if e, ok := decode[protocol.Error](c, env); ok {
			c.onError(e)
		}
	case protocol.MessageTypeAgentRecv:
		if r, ok := decode[protocol.AgentRecv](c, env); ok {
			c.onRelay(r)
		}
	case protocol.MessageTypeStats:
		if st, ok := decode[protocol.ServerStats](c, env); ok {
			c.onServerStats(st)
		}
	case protocol.MessageTypeServerShutdown:
		c.log.Info("the server is shutting down")
	default:
		c.log.Debug("notification ignored", slog.String("type", string(env.Type)))
	}
}

// decode decodes a notification's payload; a malformed one is logged with its type only and dropped.
func decode[T any](c *Client, env protocol.Envelope) (T, bool) {
	v, err := protocol.Decode[T](env)
	if err != nil {
		c.log.Debug("malformed notification dropped", slog.String("type", string(env.Type)))
		return v, false
	}
	return v, true
}

// onError handles an error notification: scope room takes the connection out of its room; scopes share, room and
// pc go to the share.
func (c *Client) onError(e protocol.Error) {
	switch e.Scope {
	case protocol.ErrorScopeRoom:
		c.mu.Lock()
		if e.RoomID == "" || e.RoomID == c.room.id {
			c.room.reset("")
		}
		c.mu.Unlock()
		c.toShare(msgError{e})
	case protocol.ErrorScopeShare, protocol.ErrorScopePC:
		c.toShare(msgError{e})
	default:
		c.log.Debug("error notification", slog.String("code", string(e.Code)), slog.String("scope", string(e.Scope)))
	}
}

// toShare posts a message to the active share's actor, if there is one.
func (c *Client) toShare(m any) {
	c.mu.Lock()
	sh := c.share
	c.mu.Unlock()
	if sh != nil {
		sh.post(m)
	}
}

// request sends a request on the current signaling client. Without one it fails like a client that is not ready.
func (c *Client) request(ctx context.Context, t protocol.MessageType, data, result any) error {
	c.mu.Lock()
	sig := c.sig
	c.mu.Unlock()
	if sig == nil {
		return fmt.Errorf("core: %s: %w", t, signal.ErrConnectionLost)
	}
	return sig.Request(ctx, t, data, result)
}

// send sends a notification on the current signaling client.
func (c *Client) send(t protocol.MessageType, data any) error {
	c.mu.Lock()
	sig := c.sig
	c.mu.Unlock()
	if sig == nil {
		return fmt.Errorf("core: %s: %w", t, signal.ErrConnectionLost)
	}
	return sig.Send(t, data)
}

// shutdown ends the connection after ctx ended or Close (§11.2): StartShare and StopOther calls in flight end, the
// live share ends (share.stop, at most 3 s for its reply), and the signaling client closes with 1000, so the server
// ends anything left at once.
func (c *Client) shutdown() {
	c.waitOps()
	if sh := c.ActiveShare(); sh != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		sh.end(ctx, endRequest{info: EndInfo{Reason: EndUser, ByThisApp: true}, stop: true})
		cancel()
	}
	c.dropSignaling()
	c.setConn(ConnStopped)
}

// terminate ends the connection after a terminal stop: the share ends locally (the signaling client is stopped),
// then Auth.SessionEnded hears of ended (when set; the share's capture has stopped by then), and Status reports the
// stop.
func (c *Client) terminate(stop *StopError, ended protocol.ErrorCode) {
	c.waitOps()
	if sh := c.ActiveShare(); sh != nil {
		info := EndInfo{Reason: EndError, Protocol: stop.Code}
		if stop.Reason == StopUnlinked || stop.Reason == StopDisabled {
			info.Reason = EndUnlinked
		}
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		sh.end(ctx, endRequest{info: info})
		cancel()
	}
	c.sessionEnded(ended)
	c.dropSignaling()
	c.setStatus(func(st *Status) {
		st.Conn = ConnStopped
		st.Stop = stop
	})
}

// waitOps waits for the StartShare and StopOther calls in flight, after life has ended. Taking c.mu first orders
// the wait after every call that saw life still running (startOp), so no Add races the Wait.
func (c *Client) waitOps() {
	c.mu.Lock()
	c.mu.Unlock() //nolint:staticcheck // SA2001: an empty critical section on purpose (see above)
	c.ops.Wait()
}

// startOp registers a StartShare or StopOther call; false once the Client is closing.
func (c *Client) startOp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.life.Err() != nil {
		return false
	}
	c.ops.Add(1)
	return true
}

// roomView is the connection's room as the connection actor knows it.
type roomView struct {
	id      string             // the room the connection is in (or is joining); "" for none
	state   protocol.RoomState // the newest room.state of id since it was set; zero before the first
	seq     uint64             // the room.states of id received since it was set
	changed chan struct{}      // closed and replaced at every change
}

// reset sets the room and forgets its state.
func (r *roomView) reset(id string) {
	r.id, r.state, r.seq = id, protocol.RoomState{}, 0
	close(r.changed)
	r.changed = make(chan struct{})
}

// onRoomState keeps a room.state of the connection's room and hands it to the share. A room.state of another room
// (one the connection has just left) is dropped.
func (c *Client) onRoomState(st protocol.RoomState) {
	c.mu.Lock()
	if st.RoomID != c.room.id {
		c.mu.Unlock()
		return
	}
	c.room.state, c.room.seq = st, c.room.seq+1
	close(c.room.changed)
	c.room.changed = make(chan struct{})
	sh := c.share
	c.mu.Unlock()
	if sh != nil {
		sh.post(msgRoomState{st})
	}
}

// roomID returns the room the connection is in.
func (c *Client) roomID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.room.id
}

// join makes the connection join room id (§11.4 step 1). The room view takes id before the request goes out, so the
// room.state that follows the reply is kept whichever goroutine runs first; a failed join puts the old one back.
func (c *Client) join(ctx context.Context, id string) error {
	c.mu.Lock()
	prev := c.room.id
	c.room.reset(id)
	c.mu.Unlock()
	var res protocol.RoomJoinResult
	if err := c.request(ctx, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: id}, &res); err != nil {
		c.mu.Lock()
		if c.room.id == id {
			c.room.reset(prev)
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// leave takes the connection out of its room, if it is in one.
func (c *Client) leave(ctx context.Context) error {
	return c.leaveRoom(ctx, false)
}

// leaveRoom sends room.leave when the connection is in a room, or always with force (a join whose reply was lost
// may have put it in one; room.leave outside a room is ok, 01 §8.4).
func (c *Client) leaveRoom(ctx context.Context, force bool) error {
	id := c.roomID()
	if id == "" && !force {
		return nil
	}
	if err := c.request(ctx, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	if c.room.id == id {
		c.room.reset("")
	}
	c.mu.Unlock()
	return nil
}

// waitRoom waits until cond holds for the connection's room view: the view's room, its newest state and the number
// of states since the room was set. It ends with ctx, and when the connection leaves room id.
func (c *Client) waitRoom(ctx context.Context, id string, cond func(st protocol.RoomState, seq uint64) bool) error {
	for {
		c.mu.Lock()
		cur, st, seq, changed := c.room.id, c.room.state, c.room.seq, c.room.changed
		c.mu.Unlock()
		if cur != id {
			return fmt.Errorf("core: no longer in room %q", id)
		}
		if cond(st, seq) {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// The backoff of 01 §10.2, for the retries the core makes itself (a forced token refresh).
const (
	backoffMin       = 500 * time.Millisecond
	backoffMax       = 10 * time.Second
	backoffDoublings = 5
)

// backoffDelay returns min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to [0.5 s, 10 s].
func backoffDelay(n int) time.Duration {
	base := backoffMax
	if n < backoffDoublings {
		base = min(backoffMax, backoffMin<<max(n, 0))
	}
	d := time.Duration(float64(base) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: retry jitter, not a secret
	return min(backoffMax, max(backoffMin, d))
}

// sleep waits for d; false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
