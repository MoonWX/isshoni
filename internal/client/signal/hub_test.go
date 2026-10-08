package signal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	hub "github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The client against the real hub (README S39, 01 §22 P6), in process, with signaltest's fakes behind it: resume
// after a killed socket, backoff within its bounds across a server restart, and Close, which makes the server skip
// the grace period. The scripted server of client_test.go covers what a healthy hub never does.

// grace is the hub's resume grace (30 s).
var grace = hub.DefaultConfig().Grace

// hubEnv is a hub that clients reach through two in-memory networks: Sever on one of them kills the clients'
// sockets and leaves the observers' alone. restart replaces the hub behind both, as a server restart does.
type hubEnv struct {
	t    *testing.T
	auth *signaltest.Auth // the accounts and sessions outlive a restart, as the database does
	cur  atomic.Pointer[backend]
	srv  *signaltest.Server // the clients' network
	obs  *signaltest.Server // the observers' network
	hc   *http.Client

	mu      sync.Mutex
	users   int
	clients []*signal.Client
	raw     []*signaltest.Client
}

// backend is one server process: a hub and its media plane.
type backend struct {
	hub   *hub.Hub
	media *signaltest.Media
}

func newHubEnv(t *testing.T) *hubEnv {
	t.Helper()
	e := &hubEnv{t: t, auth: signaltest.NewAuth()}
	e.start()
	e.srv = signaltest.StartServer(e)
	e.obs = signaltest.StartServer(e)
	e.hc = e.srv.Net.HTTPClient()
	return e
}

func (e *hubEnv) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.cur.Load().hub.ServeHTTP(w, r) }

func (e *hubEnv) hub() *hub.Hub { return e.cur.Load().hub }

// start starts a new server process behind the networks.
func (e *hubEnv) start() {
	e.t.Helper()
	cfg := hub.DefaultConfig()
	cfg.PublicOrigin = "https://watch.example.com"
	cfg.ServerVersion = "0.1.0"
	cfg.ResumeKey = bytes.Repeat([]byte{7}, 32)
	b := &backend{media: signaltest.NewMedia()}
	h, err := hub.New(cfg, hub.Deps{
		Auth:     e.auth,
		Rooms:    signaltest.NewRooms(),
		Media:    b.media,
		ClientIP: signaltest.ClientIP,
	})
	if err != nil {
		e.t.Fatalf("hub.New: %v", err)
	}
	b.hub = h
	e.cur.Store(b)
}

// stop shuts the current server process down; until start, upgrades get 503.
func (e *hubEnv) stop(reason protocol.ShutdownReason) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.hub().Shutdown(ctx, reason); err != nil {
		e.t.Errorf("Shutdown: %v", err)
	}
}

// close closes every client, shuts the hub down and stops the servers.
func (e *hubEnv) close() {
	e.mu.Lock()
	clients, raw := e.clients, e.raw
	e.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
	for _, c := range raw {
		c.Close()
	}
	e.stop(protocol.ShutdownReasonStop)
	e.srv.Close()
	e.obs.Close()
}

// user registers a new user with one cookie session.
func (e *hubEnv) user() (cookie string, id hub.Identity) {
	e.mu.Lock()
	e.users++
	n := e.users
	e.mu.Unlock()
	id = hub.Identity{UserID: fmt.Sprintf("user%02d", n), Name: fmt.Sprintf("name%02d", n),
		SessionID: fmt.Sprintf("session%02d", n)}
	cookie = fmt.Sprintf("cookie%02d", n)
	e.auth.AddSession(cookie, id)
	return cookie, id
}

// options are the options of a client that authenticates with the session cookie.
func (e *hubEnv) options(cookie string) signal.Options {
	o := options(e.srv.URL("/ws"), e.hc)
	if cookie != "" {
		o.Header = signaltest.CookieHeader(cookie)
	}
	return o
}

// dial connects a client; the env closes it at the end.
func (e *hubEnv) dial(o signal.Options) *signal.Client {
	e.t.Helper()
	c, err := signal.Dial(ctxT(e.t), o)
	if err != nil {
		e.t.Fatalf("Dial: %v", err)
	}
	e.mu.Lock()
	e.clients = append(e.clients, c)
	e.mu.Unlock()
	return c
}

// observer connects a raw protocol client over the observers' network and joins it to the room: it sees what the
// other participants see of the client under test.
func (e *hubEnv) observer(cookie string, hello protocol.Hello, roomID string) (*signaltest.Client, protocol.Welcome) {
	e.t.Helper()
	c, err := signaltest.Dial(context.Background(), e.obs.Net.HTTPClient(), e.obs.URL("/ws"),
		signaltest.DialOptions{Header: signaltest.CookieHeader(cookie)})
	if err != nil {
		e.t.Fatalf("observer: dial: %v", err)
	}
	e.mu.Lock()
	e.raw = append(e.raw, c)
	e.mu.Unlock()
	w, err := c.Hello(ctxT(e.t), hello)
	if err != nil {
		e.t.Fatalf("observer: hello: %v", err)
	}
	if roomID != "" {
		if err := c.Send(protocol.MessageTypeRoomJoin, c.NextID(), protocol.RoomJoin{RoomID: roomID}); err != nil {
			e.t.Fatalf("observer: room.join: %v", err)
		}
	}
	return c, w
}

// settle lets the hub send what it has: longer than the 200 ms room.state coalescing, shorter than the first
// backoff delay (0.5 s).
func settle() {
	time.Sleep(300 * time.Millisecond)
	synctest.Wait()
}

// seen is what an observer received: the room events in order and the room.state snapshots in order.
type seen struct {
	events []protocol.RoomEvent
	states []protocol.RoomState
}

// drain reads everything an observer has queued.
func drain(t *testing.T, c *signaltest.Client) seen {
	t.Helper()
	var s seen
	for c.Pending() > 0 {
		env, err := c.Recv(ctxT(t))
		if err != nil {
			t.Fatalf("observer: recv: %v", err)
		}
		switch env.Type {
		case protocol.MessageTypeRoomEvent:
			ev, err := protocol.Decode[protocol.RoomEvent](env)
			if err != nil {
				t.Fatal(err)
			}
			s.events = append(s.events, ev)
		case protocol.MessageTypeRoomState:
			st, err := protocol.Decode[protocol.RoomState](env)
			if err != nil {
				t.Fatal(err)
			}
			s.states = append(s.states, st)
		case protocol.MessageTypeOK:
		default:
			t.Fatalf("observer: got %s %s, want room traffic", env.Type, env.Data)
		}
	}
	return s
}

// statusOf returns the status of userID in the last snapshot, "" when the user is not in it.
func (s seen) statusOf(t *testing.T, userID string) protocol.ParticipantStatus {
	t.Helper()
	if len(s.states) == 0 {
		t.Fatalf("no room.state, want one with user %s", userID)
	}
	for _, p := range s.states[len(s.states)-1].Participants {
		if p.UserID == userID {
			return p.Status
		}
	}
	return ""
}

// join sends room.join through the client and returns the room.state that follows its ok.
func join(t *testing.T, c *signal.Client, roomID string) protocol.RoomState {
	t.Helper()
	var res protocol.RoomJoinResult
	if err := c.Request(ctxT(t), protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: roomID}, &res); err != nil {
		t.Fatalf("room.join: %v", err)
	}
	if res.Room.ID != roomID {
		t.Fatalf("room.join result %+v, want room %s", res, roomID)
	}
	return roomState(t, c)
}

// roomState reads the next notification, a room.state.
func roomState(t *testing.T, c *signal.Client) protocol.RoomState {
	t.Helper()
	st, err := protocol.Decode[protocol.RoomState](expectEvent(t, c, protocol.MessageTypeRoomState))
	if err != nil {
		t.Fatalf("room.state: %v", err)
	}
	return st
}

// connections returns the hub's connections of userID, from its snapshot.
func (e *hubEnv) connections(userID string) []hub.LiveConnection {
	var out []hub.LiveConnection
	for _, r := range e.hub().Snapshot().Rooms {
		for _, p := range r.Participants {
			if p.UserID == userID {
				out = append(out, p.Connections...)
			}
		}
	}
	return out
}

// The acceptance case "resume after a killed socket" (01 §10.3, §11.5 A): the network under the client's socket
// fails. The client backs off for the first delay, says hello with its resume token and is ready again on the same
// connection: its room, and for everybody else its participant, are as before.
func TestResumeAfterKilledSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookieA, a := e.user()
		cookieO, _ := e.user()

		c := e.dial(e.options(cookieA))
		if sc := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady); sc.Resumed {
			t.Errorf("first ready %+v, want resumed false", sc)
		}
		first := c.Welcome()
		if first.ConnectionID == "" || first.ResumeToken == "" || first.Resumed || first.User.ID != a.UserID {
			t.Fatalf("first welcome %+v", first)
		}
		join(t, c, "lounge")
		obs, _ := e.observer(cookieO, signaltest.DefaultHello(), "lounge")
		settle()
		if got := drain(t, obs).statusOf(t, a.UserID); got != protocol.ParticipantStatusPresent {
			t.Fatalf("the observer sees A as %q before the drop, want present", got)
		}
		// The observer joined: the event, then the room's new state.
		if ev, err := protocol.Decode[protocol.RoomEvent](expectEvent(t, c, protocol.MessageTypeRoomEvent)); err != nil ||
			ev.Kind != protocol.RoomEventKindParticipantJoined {
			t.Errorf("room.event %+v, %v; want participant.joined", ev, err)
		}
		if st := roomState(t, c); len(st.Participants) != 2 {
			t.Errorf("room.state %+v, want two participants", st)
		}
		noEvents(t, c)
		peer := e.cur.Load().media.Peer(first.ConnectionID)

		// The socket is killed.
		e.srv.Net.Sever()
		back := expectStates(t, c, signal.StateBackoff)
		down := time.Now()
		if back.Err != nil {
			t.Errorf("backoff after a killed socket carries %v, want no error", back.Err)
		}
		inBackoffBounds(t, 0, back.Delay)
		// Not ready: requests and notifications fail at once (01 §10.1).
		if err := c.Request(ctxT(t), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Request while backing off: %v, want ErrConnectionLost", err)
		}
		if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Send while backing off: %v, want ErrConnectionLost", err)
		}
		// The server keeps the connection, detached; the others see it reconnecting.
		settle()
		during := drain(t, obs)
		if got := during.statusOf(t, a.UserID); got != protocol.ParticipantStatusReconnecting {
			t.Errorf("the observer sees A as %q during the drop, want reconnecting", got)
		}
		if conns := e.connections(a.UserID); len(conns) != 1 || conns[0].ID != first.ConnectionID ||
			conns[0].Status != protocol.ConnectionStatusReconnecting {
			t.Errorf("the hub's connections of A during the drop: %+v, want the first one, reconnecting", conns)
		}

		// The client resumes by itself after the delay.
		if sc := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady); !sc.Resumed {
			t.Fatalf("ready after the drop %+v, want resumed", sc)
		}
		if got := time.Since(down); got != back.Delay {
			t.Errorf("ready again %v after the drop, want the backoff delay %v", got, back.Delay)
		}
		w := c.Welcome()
		if !w.Resumed || w.ConnectionID != first.ConnectionID || w.RoomID != "lounge" {
			t.Errorf("welcome after the drop: resumed %v, connection %s, room %q; want resumed, %s, lounge",
				w.Resumed, w.ConnectionID, w.RoomID, first.ConnectionID)
		}
		if w.ResumeToken == "" || w.ResumeToken == first.ResumeToken {
			t.Error("the resume token was not rotated")
		}
		// A resumed welcome is followed by the room's state (01 §10.5), and the peer is resynced once.
		if st := roomState(t, c); st.RoomID != "lounge" {
			t.Errorf("room.state after the resume is of room %q", st.RoomID)
		}
		settle()
		if n := len(peer.CallsTo("Resync")); n != 1 {
			t.Errorf("Resync was called %d times, want once", n)
		}
		after := drain(t, obs)
		if got := after.statusOf(t, a.UserID); got != protocol.ParticipantStatusPresent {
			t.Errorf("the observer sees A as %q after the resume, want present", got)
		}
		if evs := append(during.events, after.events...); len(evs) != 0 {
			t.Errorf("the observer got room events %+v, want none: nobody left or joined", evs)
		}
		if conns := e.connections(a.UserID); len(conns) != 1 || conns[0].ID != first.ConnectionID ||
			conns[0].Status != protocol.ConnectionStatusOnline {
			t.Errorf("the hub's connections of A after the resume: %+v, want the first one, online", conns)
		}
		// The connection works again.
		if err := c.Request(ctxT(t), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); err != nil {
			t.Errorf("room.leave after the resume: %v", err)
		}
		noStates(t, c)
	})
}

// A second drop resumes with the token of the second welcome: the client always sends its newest token, and the
// hub accepts no older one (01 §10.3).
func TestResumeTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, _ := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		id := c.Welcome().ConnectionID
		tokens := map[protocol.Secret]bool{c.Welcome().ResumeToken: true}
		for i := range 3 {
			e.srv.Net.Sever()
			expectBackoff(t, c)
			if sc := expectStates(t, c, signal.StateHandshaking, signal.StateReady); !sc.Resumed {
				t.Fatalf("drop %d: ready %+v, want resumed", i, sc)
			}
			w := c.Welcome()
			if w.ConnectionID != id || tokens[w.ResumeToken] {
				t.Fatalf("drop %d: connection %s (want %s), token seen before: %v", i, w.ConnectionID, id,
					tokens[w.ResumeToken])
			}
			tokens[w.ResumeToken] = true
		}
	})
}

// The acceptance case "Close makes the server skip the grace period" (01 §4.2): Close sends close code 1000, and
// the hub ends the connection at once, where a killed socket would have kept it for 30 s.
func TestCloseSkipsGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookieA, a := e.user()
		cookieO, _ := e.user()
		c := e.dial(e.options(cookieA))
		join(t, c, "lounge")
		obs, _ := e.observer(cookieO, signaltest.DefaultHello(), "lounge")
		settle()
		drain(t, obs)
		if conns := e.connections(a.UserID); len(conns) != 1 {
			t.Fatalf("the hub's connections of A: %+v, want one", conns)
		}

		start := time.Now()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		synctest.Wait()
		// No time has passed, and A is gone: no grace.
		if got := time.Since(start); got != 0 {
			t.Errorf("Close and the hub's reaction took %v of the %v grace, want none", got, grace)
		}
		if conns := e.connections(a.UserID); len(conns) != 0 {
			t.Errorf("the hub still has A's connections after Close: %+v", conns)
		}
		left := drain(t, obs)
		if len(left.events) != 1 || left.events[0].Kind != protocol.RoomEventKindParticipantLeft ||
			left.events[0].UserID != a.UserID || left.events[0].Reason != protocol.EndReasonLeft {
			t.Errorf("the observer got %+v, want participant.left{A, left}", left.events)
		}
		settle()
		if got := drain(t, obs).statusOf(t, a.UserID); got != "" {
			t.Errorf("the observer still sees A as %q", got)
		}

		// The client is stopped for good.
		rest := expectStopped(t, c)
		if len(rest) == 0 || rest[len(rest)-1] != (signal.StateChange{State: signal.StateStopped}) {
			t.Errorf("state changes %+v, want a last one stopped without an error", rest)
		}
		if err := c.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})
}

// The contrast to TestCloseSkipsGrace: a client whose socket is killed and that does not come back (here its
// reconnects are refused) keeps its place for the whole grace period.
func TestKilledSocketKeepsGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, a := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		join(t, c, "lounge")
		id := c.Welcome().ConnectionID

		e.auth.FailRequests(errors.New("the database is away")) // every upgrade now gets 503
		e.srv.Net.Sever()
		time.Sleep(grace - time.Second)
		synctest.Wait()
		if conns := e.connections(a.UserID); len(conns) != 1 || conns[0].ID != id ||
			conns[0].Status != protocol.ConnectionStatusReconnecting {
			t.Errorf("the hub's connections of A just before the grace ends: %+v, want it reconnecting", conns)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if conns := e.connections(a.UserID); len(conns) != 0 {
			t.Errorf("the hub's connections of A after the grace: %+v, want none", conns)
		}

		// The server is back: the token resumes nothing any more, which is not an error (01 §10.3).
		e.auth.FailRequests(nil)
		for {
			if sc := nextState(t, c); sc.State == signal.StateReady {
				if sc.Resumed {
					t.Errorf("ready after the grace %+v, want a new connection", sc)
				}
				break
			}
		}
		if w := c.Welcome(); w.Resumed || w.ConnectionID == id || w.RoomID != "" {
			t.Errorf("welcome after the grace: resumed %v, connection %s (old %s), room %q", w.Resumed,
				w.ConnectionID, id, w.RoomID)
		}
	})
}

// The acceptance case "backoff within its bounds", across a server restart (01 §10.2, §11.6): the first wait after
// server.shutdown is exactly reconnectInMs, then every delay is min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2) within
// [0.5 s, 10 s] while the server is down, and the new process answers resumed: false.
func TestBackoffWhileServerRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, a := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		join(t, c, "lounge")
		first := c.Welcome()

		e.stop(protocol.ShutdownReasonRestart)
		sd, err := protocol.Decode[protocol.ServerShutdown](expectEvent(t, c, protocol.MessageTypeServerShutdown))
		if err != nil || sd.Reason != protocol.ShutdownReasonRestart || sd.ReconnectInMs < 500 || sd.ReconnectInMs > 3000 {
			t.Fatalf("server.shutdown %+v, %v", sd, err)
		}
		back := expectBackoff(t, c)
		if back.Err == nil || back.Err.Code != protocol.ErrorCodeServerShutdown {
			t.Errorf("backoff after the shutdown carries %v, want server_shutdown", back.Err)
		}
		if want := time.Duration(sd.ReconnectInMs) * time.Millisecond; back.Delay != want {
			t.Errorf("first delay after server.shutdown is %v, want exactly reconnectInMs = %v", back.Delay, want)
		}

		// The old process answers 503 until the new one is up: the normal sequence, from its start.
		var total time.Duration
		for n := range 9 {
			sc := expectBackoff(t, c)
			if sc.Err != nil {
				t.Errorf("backoff %d carries %v, want no error for a refused upgrade", n, sc.Err)
			}
			inBackoffBounds(t, n, sc.Delay)
			total += sc.Delay
		}
		// 0.5 + 1 + 2 + 4 + 8 + 10 + 10 + 10 + 10 = 55.5 s, ± 20 %.
		if total < 44*time.Second || total > 67*time.Second {
			t.Errorf("nine delays add up to %v, want about 55.5 s", total)
		}

		// The attempt that is out gets its 503 from the old process; the next one reaches the new one.
		synctest.Wait()
		e.start()
		sc := expectBackoff(t, c)
		inBackoffBounds(t, 9, sc.Delay)
		if sc = expectStates(t, c, signal.StateHandshaking, signal.StateReady); sc.Resumed {
			t.Errorf("ready on the new process %+v, want resumed false", sc)
		}
		w := c.Welcome()
		if w.Resumed || w.ConnectionID == first.ConnectionID || w.RoomID != "" || w.User.ID != a.UserID {
			t.Errorf("welcome of the new process: resumed %v, connection %s (old %s), room %q, user %s",
				w.Resumed, w.ConnectionID, first.ConnectionID, w.RoomID, w.User.ID)
		}
		noEvents(t, c)
		// 01 §10.5, not resumed: the caller joins again.
		if st := join(t, c, "lounge"); len(st.Participants) != 1 {
			t.Errorf("room.state on the new process: %+v", st)
		}
	})
}

// The backoff sequence starts over only after the connection has been ready for 10 s (01 §10.2): a server that
// drops its clients right after the welcome keeps the delays growing.
func TestBackoffResetAgainstHub(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, _ := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)

		// Dropped right after each welcome: the delays double.
		for n := range 4 {
			e.srv.Net.Sever()
			inBackoffBounds(t, n, expectBackoff(t, c).Delay)
			expectStates(t, c, signal.StateHandshaking, signal.StateReady)
			time.Sleep(signal.BackoffReset - time.Second) // not long enough
		}
		// Ready for 10 s: the next drop starts at 0.5 s again.
		time.Sleep(time.Second)
		e.srv.Net.Sever()
		inBackoffBounds(t, 0, expectBackoff(t, c).Delay)
		expectStates(t, c, signal.StateHandshaking, signal.StateReady)
	})
}

// An idle client stays connected: its pings (01 §3.4) and the pongs keep both sides content far beyond the hub's
// 45 s idle timeout and the client's own 10 s timeouts, and the connection still works afterwards.
func TestIdleConnectionStaysUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, _ := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		id := c.Welcome().ConnectionID

		time.Sleep(10 * time.Minute)
		noStates(t, c)
		if got := c.Welcome().ConnectionID; got != id {
			t.Errorf("connection %s after 10 idle minutes, want %s", got, id)
		}
		st := join(t, c, "lounge")
		if len(st.Participants) != 1 || len(st.Participants[0].Connections) != 1 ||
			st.Participants[0].Connections[0].ID != id {
			t.Errorf("room.state after 10 idle minutes: %+v", st)
		}
	})
}

// Requests against the hub: the ok's payload is decoded into the result, the hub's error comes back as a
// *protocol.Error, notifications arrive in Events in order, and Send delivers a notification.
func TestRequestsAgainstHub(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, a := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		w := c.Welcome()
		if w.Protocol != protocol.Version || w.DefaultRoomID != "lounge" || w.User.Name != a.Name ||
			w.Limits.PingIntervalMs != 15000 {
			t.Errorf("welcome %+v", w)
		}

		err := c.Request(ctxT(t), protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "nowhere"}, nil)
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeRoomNotFound || pe.Scope != protocol.ErrorScopeRequest {
			t.Fatalf("room.join of an unknown room: %v, want room_not_found", err)
		}
		noEvents(t, c)

		st := join(t, c, w.DefaultRoomID)
		if len(st.Participants) != 1 || st.Participants[0].UserID != a.UserID {
			t.Errorf("room.state %+v, want A alone", st)
		}
		// A request with a payload the hub rejects, and one without a result.
		err = c.Request(ctxT(t), protocol.MessageTypeShareStop, protocol.ShareStop{}, nil)
		if !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeBadRequest || pe.Params["field"] != "shareId" {
			t.Errorf("share.stop without a share id: %v, want bad_request for shareId", err)
		}
		if err := c.Request(ctxT(t), protocol.MessageTypeStatsWatch, protocol.StatsWatch{}, nil); err != nil {
			t.Errorf("stats.watch: %v", err)
		}
		// A notification: the hub keeps the connection's last stats report.
		if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); err != nil {
			t.Errorf("Send stats: %v", err)
		}
		synctest.Wait()
		if conns := e.connections(a.UserID); len(conns) != 1 || conns[0].LastStats == nil ||
			conns[0].Kind != protocol.ClientKindTool || conns[0].Role != protocol.RoleFull {
			t.Errorf("the hub's connections of A: %+v, want one tool connection with a stats report", conns)
		}
		// The client sends hello itself.
		if err := c.Request(ctxT(t), protocol.MessageTypeHello, protocol.Hello{}, nil); err == nil {
			t.Error("Request(hello) succeeded")
		}
		if err := c.Send(protocol.MessageTypeHello, protocol.Hello{}); err == nil {
			t.Error("Send(hello) succeeded")
		}
		// A request given to Send and a notification given to Request never leave the client: the hub ends the
		// connection over a request without an id (bad_message, 4400), and it never answers a notification.
		start := time.Now()
		if err := c.Send(protocol.MessageTypeRoomLeave, protocol.Empty{}); err == nil || !strings.Contains(err.Error(), "use Request") {
			t.Errorf("Send(room.leave) = %v, want an error that names Request", err)
		}
		err = c.Request(ctxT(t), protocol.MessageTypeStats, protocol.ClientStats{}, nil)
		if err == nil || !strings.Contains(err.Error(), "use Send") {
			t.Errorf("Request(stats) = %v, want an error that names Send", err)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("the two calls took %v, want no wait for a reply", got)
		}
		// A type this build doesn't know may be a newer server's and goes out either way (01 §8.13): this hub answers
		// such a request with unknown_type and ignores such a notification.
		err = c.Request(ctxT(t), "from.the.future", protocol.Empty{}, nil)
		if !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeUnknownType {
			t.Errorf("a request of an unknown type: %v, want unknown_type", err)
		}
		if err := c.Send("from.the.future", protocol.Empty{}); err != nil {
			t.Errorf("a notification of an unknown type: %v", err)
		}
		// None of it touched the connection: it is still in its room, on its first socket.
		synctest.Wait()
		if conns := e.connections(a.UserID); len(conns) != 1 || conns[0].ID != w.ConnectionID ||
			conns[0].Status != protocol.ConnectionStatusOnline {
			t.Errorf("the hub's connections of A: %+v, want the first one, online", conns)
		}
		noStates(t, c)
	})
}

// A caps.update outlasts a resume (01 §8.10, §10.3, §11.7): the hello that resumes carries the caps the client sent
// last, not the ones Dial got, so the hub does not put the connection back to those.
func TestCapsUpdateOutlastsResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, _ := e.user()
		o := e.options(cookie)
		atDial := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecOpus}} // no H.264 yet
		o.Caps = atDial
		c := e.dial(o)
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		join(t, c, "lounge")
		peer := e.cur.Load().media.Peer(c.Welcome().ConnectionID)
		if peer == nil || !slices.Equal(peer.Params.Caps.Decode, atDial.Decode) {
			t.Fatalf("the connection's peer %+v, want one with the caps of Dial", peer)
		}

		// H.264 became available.
		now := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecOpus}}
		if err := c.Send(protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: now}); err != nil {
			t.Fatalf("Send caps.update: %v", err)
		}
		synctest.Wait()
		for range 2 {
			e.srv.Net.Sever()
			expectBackoff(t, c)
			if sc := expectStates(t, c, signal.StateHandshaking, signal.StateReady); !sc.Resumed {
				t.Fatalf("ready after the drop %+v, want resumed", sc)
			}
			settle()
		}
		if n := len(peer.CallsTo("Resync")); n != 2 {
			t.Errorf("Resync was called %d times, want once per resume", n)
		}
		for _, call := range peer.CallsTo("SetCaps") {
			if got, ok := call.Args[0].(protocol.Caps); !ok || !slices.Equal(got.Decode, now.Decode) {
				t.Errorf("the hub set the peer's caps to %+v, want only ever %+v", call.Args[0], now)
			}
		}
	})
}

// Errors that reconnecting can't fix end Dial at once, with the hub's error (01 §10.1).
func TestDialStopsOnHubErrors(t *testing.T) {
	t.Run("unauthenticated", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newHubEnv(t)
			defer e.close()
			for name, cookie := range map[string]string{"no cookie": "", "unknown cookie": "nobody"} {
				start := time.Now()
				c, err := signal.Dial(ctxT(t), e.options(cookie))
				var pe *protocol.Error
				if c != nil || !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeUnauthenticated ||
					pe.Scope != protocol.ErrorScopeSession {
					t.Errorf("%s: Dial = %v, %v; want unauthenticated", name, c, err)
				}
				if got := time.Since(start); got != 0 {
					t.Errorf("%s: Dial took %v, want no retry", name, got)
				}
			}
		})
	})
	t.Run("too many connections", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newHubEnv(t)
			defer e.close()
			cookie, _ := e.user()
			for range hub.DefaultConfig().Limits.MaxConnectionsPerUser {
				e.dial(e.options(cookie))
			}
			c, err := signal.Dial(ctxT(t), e.options(cookie))
			var pe *protocol.Error
			if c != nil || !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeTooManyConnections {
				t.Errorf("17th Dial = %v, %v; want too_many_connections", c, err)
			}
		})
	})
	// An upgrade that the server refuses is a failed attempt like any other: the client can't tell a wrong Origin
	// from a proxy that is not ready, and keeps trying until Dial's context ends.
	t.Run("refused upgrade", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newHubEnv(t)
			defer e.close()
			cookie, _ := e.user()
			o := e.options(cookie)
			o.Header.Set("Origin", "https://evil.example.com")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			c, err := signal.Dial(ctx, o)
			if c != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Dial = %v, %v; want the context's deadline", c, err)
			}
			if got := time.Since(start); got != 5*time.Second {
				t.Errorf("Dial returned after %v, want 5 s", got)
			}
			if want := "upgrade refused with 403"; !strings.Contains(err.Error(), want) {
				t.Errorf("Dial error %q does not name the last attempt (%q)", err, want)
			}

			o.NoReconnect = true
			start = time.Now()
			c, err = signal.Dial(ctxT(t), o)
			if c != nil || err == nil || !strings.Contains(err.Error(), "upgrade refused with 403") {
				t.Errorf("Dial with NoReconnect = %v, %v; want the refusal", c, err)
			}
			if got := time.Since(start); got != 0 {
				t.Errorf("Dial with NoReconnect took %v, want no retry", got)
			}
		})
	})
}

// A revocation (01 §3.2) stops the client: session_revoked in scope session, and no reconnect.
func TestRevocationStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, a := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		join(t, c, "lounge")

		if n := e.hub().CloseConnections(hub.ConnSelector{UserID: a.UserID}, protocol.ErrorCodeSessionRevoked); n != 1 {
			t.Fatalf("CloseConnections closed %d connections, want 1", n)
		}
		sc := expectStates(t, c, signal.StateStopped)
		if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeSessionRevoked || sc.Err.Scope != protocol.ErrorScopeSession {
			t.Errorf("stopped with %v, want session_revoked", sc.Err)
		}
		if rest := expectStopped(t, c); len(rest) != 0 {
			t.Errorf("state changes after stopped: %+v", rest)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if conns := e.connections(a.UserID); len(conns) != 0 {
			t.Errorf("the client came back after the revocation: %+v", conns)
		}
		if err := c.Close(); err != nil {
			t.Errorf("Close after the server stopped the client: %v", err)
		}
	})
}

// When the same connection resumes on another socket, the old socket gets replaced and 4409, and its client stops
// silently instead of fighting for the connection (01 §10.1, §10.3).
func TestReplacedStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		cookie, _ := e.user()
		c := e.dial(e.options(cookie))
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		w := c.Welcome()

		hello := signaltest.DefaultHello()
		hello.ResumeToken = w.ResumeToken
		_, w2 := e.observer(cookie, hello, "")
		if !w2.Resumed || w2.ConnectionID != w.ConnectionID {
			t.Fatalf("the second socket's welcome: resumed %v, connection %s; want it to resume %s", w2.Resumed,
				w2.ConnectionID, w.ConnectionID)
		}
		sc := expectStates(t, c, signal.StateStopped)
		if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeReplaced {
			t.Errorf("stopped with %v, want replaced", sc.Err)
		}
		expectStopped(t, c)
	})
}

// Bearer auth (01 §3.2; native apps, M2+): the token goes into hello.auth and nowhere else, Token is called before
// every hello, and a resume needs the same device.
func TestBearerToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		e.auth.AddBearer("access-1", hub.Identity{UserID: "user77", Name: "name77", DeviceID: "device77"})
		e.auth.AddBearer("access-2", hub.Identity{UserID: "user77", Name: "name77", DeviceID: "device77"})
		var calls atomic.Int32
		o := e.options("")
		o.Client.Kind = protocol.ClientKindDesktop
		o.Role = protocol.RolePublisher
		o.Token = func(context.Context) (protocol.Secret, error) {
			return protocol.Secret(fmt.Sprintf("access-%d", calls.Add(1))), nil
		}
		c := e.dial(o)
		w := c.Welcome()
		if w.User.ID != "user77" || calls.Load() != 1 {
			t.Fatalf("welcome for user %s after %d token calls, want user77 after 1", w.User.ID, calls.Load())
		}
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)

		// A refreshed token of the same device resumes the connection.
		e.srv.Net.Sever()
		expectBackoff(t, c)
		if sc := expectStates(t, c, signal.StateHandshaking, signal.StateReady); !sc.Resumed {
			t.Errorf("ready after the drop %+v, want resumed", sc)
		}
		if calls.Load() != 2 || c.Welcome().ConnectionID != w.ConnectionID {
			t.Errorf("%d token calls, connection %s; want 2 calls and %s", calls.Load(), c.Welcome().ConnectionID,
				w.ConnectionID)
		}

		// A token the server no longer takes: unauthenticated, and the client stops.
		e.srv.Net.Sever()
		sc := expectStates(t, c, signal.StateBackoff, signal.StateConnecting, signal.StateHandshaking, signal.StateStopped)
		if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeUnauthenticated {
			t.Errorf("stopped with %v, want unauthenticated", sc.Err)
		}
		expectStopped(t, c)
	})
}

// A Token that fails is a failed attempt: the client backs off and asks again.
func TestBearerTokenError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHubEnv(t)
		defer e.close()
		e.auth.AddBearer("access", hub.Identity{UserID: "user77", Name: "name77", DeviceID: "device77"})
		var calls atomic.Int32
		o := e.options("")
		o.Token = func(context.Context) (protocol.Secret, error) {
			if calls.Add(1) < 3 {
				return "", errors.New("the token endpoint is away")
			}
			return "access", nil
		}
		start := time.Now()
		c := e.dial(o)
		expectStates(t, c, signal.StateConnecting, signal.StateBackoff, signal.StateConnecting, signal.StateBackoff,
			signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		if got := time.Since(start); calls.Load() != 3 || got < 1200*time.Millisecond || got > 1800*time.Millisecond {
			t.Errorf("ready after %d token calls and %v, want 3 calls and two delays (about 1.5 s)", calls.Load(), got)
		}

		o.NoReconnect = true
		calls.Store(0)
		if c, err := signal.Dial(ctxT(t), o); c != nil || err == nil ||
			!strings.Contains(err.Error(), "the token endpoint is away") {
			t.Errorf("Dial with NoReconnect and a failing Token = %v, %v", c, err)
		}
	})
}
