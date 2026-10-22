package signal_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// user.connections (M2 01 §10.4, slice WA27): the full list of a user's connections goes to those of them with the
// feature after their welcome and whenever one of them opens, closes, detaches, re-attaches, changes room or has its
// device name changed by a Revalidate; at most one per 200 ms per user, and never to another user's connections.

// coalesce is the least time between two user.connections of one user.
const coalesce = 200 * time.Millisecond

// connsHello is a web client's hello (role full) with the feature user.connections.
func connsHello() protocol.Hello {
	h := signaltest.DefaultHello()
	h.Features = []protocol.Feature{protocol.FeatureUserConnections}
	return h
}

// connsConn connects with connsHello, checks that the welcome turns the feature on and returns the client and its
// connection id. The list that follows the welcome is left queued.
func (e *env) connsConn(cookie string) (*signaltest.Client, string) {
	e.t.Helper()
	c, w := e.connect(cookie, connsHello())
	if !slices.Equal(w.Features, []protocol.Feature{protocol.FeatureUserConnections}) {
		e.t.Fatalf("welcome features %v, want user.connections", w.Features)
	}
	return c, w.ConnectionID
}

// expectConns reads the next message, a user.connections notification, and returns its list. The list must be
// sorted by connection id.
func expectConns(t *testing.T, c *signaltest.Client) []protocol.OwnConnection {
	t.Helper()
	env := expectType(t, c, protocol.MessageTypeUserConnections)
	if env.ID != "" || env.Re != "" {
		t.Fatalf("user.connections id %q re %q, want a notification", env.ID, env.Re)
	}
	v, err := protocol.Decode[protocol.UserConnections](env)
	if err != nil {
		t.Fatalf("decode user.connections: %v", err)
	}
	if !slices.IsSortedFunc(v.Connections, func(a, b protocol.OwnConnection) int { return strings.Compare(a.ID, b.ID) }) {
		t.Errorf("user.connections not sorted by id: %+v", v.Connections)
	}
	return v.Connections
}

// onlyConns lets everything settle and reads what c has queued, which must be exactly one user.connections, and
// returns its list.
func onlyConns(t *testing.T, c *signaltest.Client) []protocol.OwnConnection {
	t.Helper()
	settle()
	list := expectConns(t, c)
	expectOpen(t, c)
	return list
}

// webConn is what user.connections shows about a connection of DefaultHello's web client.
func webConn(id string, status protocol.ConnectionStatus, roomID string) protocol.OwnConnection {
	return protocol.OwnConnection{
		ConnectionInfo: protocol.ConnectionInfo{ID: id, Kind: protocol.ClientKindWeb, Role: protocol.RoleFull,
			Status: status},
		OS: protocol.ClientOSMacOS, Version: "0.1.0", RoomID: roomID,
	}
}

// sortedConns returns the list sorted by connection id, as the hub sends it.
func sortedConns(list ...protocol.OwnConnection) []protocol.OwnConnection {
	return slices.SortedFunc(slices.Values(list), func(a, b protocol.OwnConnection) int { return strings.Compare(a.ID, b.ID) })
}

// expectList checks a list against want (in any order; the list is sorted by id).
func expectList(t *testing.T, what string, got []protocol.OwnConnection, want ...protocol.OwnConnection) {
	t.Helper()
	if jsonOf(t, got) != jsonOf(t, sortedConns(want...)) {
		t.Errorf("%s: user.connections %s, want %s", what, jsonOf(t, got), jsonOf(t, sortedConns(want...)))
	}
}

// After its welcome a connection with the feature gets the list of its user's connections, itself included, and
// nothing about other users. A connection without the feature gets nothing, but is on the list.
func TestUserConnectionsWelcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)

		a1, id1 := e.connsConn(cookieA)
		f, err := a1.RecvFrame(ctxT(t))
		if err != nil || f.Envelope.Type != protocol.MessageTypeUserConnections {
			t.Fatalf("got %s, %v after the welcome; want user.connections", f.Envelope.Type, err)
		}
		want := `{"connections":[{"id":"` + id1 + `","kind":"web","role":"full","status":"online","os":"macos","version":"0.1.0"}]}`
		if string(f.Envelope.Data) != want {
			t.Errorf("user.connections %s, want %s", f.Envelope.Data, want)
		}
		synctest.Wait()
		expectOpen(t, a1)

		// A second connection without the feature: it gets nothing, a1 gets both, a coalescing interval after its
		// first list.
		start := time.Now()
		a2, w2 := e.connect(e.session(a, "sessionA2"), signaltest.DefaultHello())
		list := expectConns(t, a1)
		if d := time.Since(start); d != coalesce {
			t.Errorf("the second list came after %v, want %v", d, coalesce)
		}
		expectList(t, "two connections", list, webConn(id1, online, ""), webConn(w2.ConnectionID, online, ""))

		// Another user with the feature: its own list only, and A's connections hear nothing about it.
		b1, idB := e.connsConn(cookieB)
		expectList(t, "user B", expectConns(t, b1), webConn(idB, online, ""))
		settle()
		for _, c := range []*signaltest.Client{a1, a2, b1} {
			expectOpen(t, c)
		}
		if got := e.metric("isshoni_ws_messages_total", "type", "user.connections", "dir", "out"); got != 3 {
			t.Errorf("isshoni_ws_messages_total{user.connections, out} = %v, want 3", got)
		}
		if e.logs.count("level=DEBUG", "msg=\"user connections\"", "user_id="+a.UserID, "connections=2",
			"recipients=1") != 1 {
			t.Errorf("no debug line for A's second list:\n%s", e.logs)
		}
	})
}

// Every change of a user's connections reaches the user's connections with the feature: one opens, changes room
// (room.join, a join elsewhere, room.leave, CloseRoom), detaches, re-attaches and closes.
func TestUserConnectionsChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, a := e.user(false)
		cookieA2 := e.session(a, "sessionA2")
		a1, id1 := e.connsConn(cookieA)
		expectConns(t, a1)
		self := webConn(id1, online, "")

		a2, w2 := e.connect(cookieA2, signaltest.DefaultHello())
		id2 := w2.ConnectionID
		expectList(t, "open", onlyConns(t, a1), self, webConn(id2, online, ""))

		join(t, a2, "lounge")
		expectList(t, "room.join", onlyConns(t, a1), self, webConn(id2, online, "lounge"))
		join(t, a2, room1.ID)
		expectList(t, "join elsewhere", onlyConns(t, a1), self, webConn(id2, online, room1.ID))
		leave(t, a2)
		expectList(t, "room.leave", onlyConns(t, a1), self, webConn(id2, online, ""))

		// The recipient's own room is on the list too.
		join(t, a1, "lounge")
		self = webConn(id1, online, "lounge")
		expectList(t, "own room.join", onlyConns(t, a1), self, webConn(id2, online, ""))

		join(t, a2, room1.ID)
		expectList(t, "join again", onlyConns(t, a1), self, webConn(id2, online, room1.ID))
		e.hub.CloseRoom(room1.ID)
		expectError(t, a2, protocol.ErrorCodeRoomClosed, protocol.ErrorScopeRoom)
		expectList(t, "CloseRoom", onlyConns(t, a1), self, webConn(id2, online, ""))
		join(t, a2, "lounge")
		expectList(t, "join a1's room", drainConns(t, a1), self, webConn(id2, online, "lounge"))

		// The socket drops: reconnecting, room kept. A resume within the grace: online again.
		a2.Close()
		expectList(t, "detach", drainConns(t, a1), self, webConn(id2, reconnecting, "lounge"))
		r2, rw := e.resume(cookieA2, w2.ResumeToken)
		if !rw.Resumed {
			t.Fatal("not resumed")
		}
		expectList(t, "re-attach", drainConns(t, a1), self, webConn(id2, online, "lounge"))

		// A deliberate close: off the list at once.
		if err := r2.CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		expectList(t, "close", drainConns(t, a1), self)

		// The grace expires for a connection that never comes back.
		a3, w3 := e.connect(cookieA2, signaltest.DefaultHello())
		expectList(t, "open again", drainConns(t, a1), self, webConn(w3.ConnectionID, online, ""))
		a3.Close()
		expectList(t, "detach again", drainConns(t, a1), self, webConn(w3.ConnectionID, reconnecting, ""))
		time.Sleep(grace)
		expectList(t, "grace expired", drainConns(t, a1), self)
	})
}

// drainConns lets everything settle and reads what c has queued, which may include room traffic but must include
// exactly one user.connections (one change, one list), and returns its list.
func drainConns(t *testing.T, c *signaltest.Client) []protocol.OwnConnection {
	t.Helper()
	settle()
	var lists [][]protocol.OwnConnection
	for c.Pending() > 0 {
		env := recv(t, c)
		switch env.Type {
		case protocol.MessageTypeRoomState, protocol.MessageTypeRoomEvent:
		case protocol.MessageTypeUserConnections:
			v, err := protocol.Decode[protocol.UserConnections](env)
			if err != nil {
				t.Fatal(err)
			}
			lists = append(lists, v.Connections)
		default:
			t.Fatalf("got %s %s, want room traffic or user.connections", env.Type, env.Data)
		}
	}
	if len(lists) != 1 {
		t.Fatalf("%d user.connections %s, want one", len(lists), jsonOf(t, lists))
	}
	return lists[0]
}

// A connection with the feature that detaches gets nothing meanwhile; after its resumed welcome (and its room.state)
// it gets the current list, with itself online.
func TestUserConnectionsResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		a1, w1 := e.connect(cookieA, connsHello())
		expectConns(t, a1)
		join(t, a1, "lounge")
		expectConns(t, a1)
		a2, w2 := e.connect(e.session(a, "sessionA2"), signaltest.DefaultHello())
		expectList(t, "two connections", onlyConns(t, a1), webConn(w1.ConnectionID, online, "lounge"),
			webConn(w2.ConnectionID, online, ""))

		// a1's socket drops, then a2 closes: the list of that change is not sent to a1, nor replayed.
		a1.Close()
		settle()
		if err := a2.CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		settle()

		h := connsHello()
		h.ResumeToken = w1.ResumeToken
		r1 := e.mustDial(headers(cookieA, testOrigin, ""))
		w, err := r1.Hello(ctxT(t), h)
		if err != nil || !w.Resumed || w.ConnectionID != w1.ConnectionID {
			t.Fatalf("resume: %v, resumed %v", err, w.Resumed)
		}
		expectState(t, r1, "lounge")
		expectList(t, "after the resume", expectConns(t, r1), webConn(w1.ConnectionID, online, "lounge"))
		settle()
		expectOpen(t, r1)
	})
}

// A list encoded while the connection was detached, which reaches its actor behind the resume, is not sent after
// the resumed welcome: it shows the connection reconnecting. The next list, with it online, is.
func TestUserConnectionsResumeSkipsDetachedList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		a1, w1 := e.connect(cookieA, connsHello())
		expectConns(t, a1)
		a2, w2 := e.connect(e.session(a, "sessionA2"), signaltest.DefaultHello())
		expectList(t, "two connections", onlyConns(t, a1), webConn(w1.ConnectionID, online, ""),
			webConn(w2.ConnectionID, online, ""))
		a1.Close()
		settle()
		time.Sleep(time.Second) // the next change goes out at once

		// a1's actor is busy while the resume is queued; a2's room change is encoded with a1 still detached and
		// reaches a1's inbox behind the resume.
		release := signal.Stall(e.hub, w1.ConnectionID)
		if release == nil {
			t.Fatal("no connection to hold")
		}
		defer release()
		h := connsHello()
		h.ResumeToken = w1.ResumeToken
		r1 := e.mustDial(headers(cookieA, testOrigin, ""))
		type hello struct {
			w   protocol.Welcome
			err error
		}
		done, ctx := make(chan hello, 1), ctxT(t)
		go func() {
			w, err := r1.Hello(ctx, h)
			done <- hello{w, err}
		}()
		settle()
		join(t, a2, "lounge")
		release()
		if res := <-done; res.err != nil || !res.w.Resumed || res.w.ConnectionID != w1.ConnectionID {
			t.Fatalf("resume: %v, resumed %v", res.err, res.w.Resumed)
		}
		expectList(t, "after the resume", expectConns(t, r1), webConn(w1.ConnectionID, online, ""),
			webConn(w2.ConnectionID, online, "lounge"))
		settle()
		expectOpen(t, r1)
	})
}

// Changes within the coalescing interval go out as one list, at the last list + 200 ms, with every change; lists of
// one user are never closer than 200 ms.
func TestUserConnectionsCoalesced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, a := e.user(false)

		// Record when each list reaches a1.
		type stamped struct {
			at   time.Time
			list []protocol.OwnConnection
		}
		var mu sync.Mutex
		var rec []stamped
		lists := func() []stamped {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(rec)
		}
		a1, id1 := e.connsConn(cookieA)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				env, err := a1.Recv(ctx)
				if err != nil {
					return
				}
				if env.Type == protocol.MessageTypeUserConnections {
					v, _ := protocol.Decode[protocol.UserConnections](env)
					mu.Lock()
					rec = append(rec, stamped{time.Now(), v.Connections})
					mu.Unlock()
				}
			}
		}()
		t0 := time.Now()
		synctest.Wait()

		// Within the interval: two connections open and one joins a room. They go out together, one interval after
		// the first list.
		a2, w2 := e.connect(e.session(a, "sessionA2"), signaltest.DefaultHello())
		a3, w3 := e.connect(e.session(a, "sessionA3"), signaltest.DefaultHello())
		join(t, a3, "lounge")
		time.Sleep(coalesce)
		synctest.Wait()
		if l := lists(); len(l) != 2 {
			t.Fatalf("%d lists after the burst, want the first and the coalesced one", len(l))
		}

		// Then a change every 50 ms for 1 s: a2 joins and leaves a room (room.join has a budget of 10 per minute).
		for i := range 20 {
			time.Sleep(50 * time.Millisecond)
			if i%2 == 0 {
				join(t, a2, room1.ID)
			} else {
				leave(t, a2)
			}
		}
		final := []protocol.OwnConnection{webConn(id1, online, ""), webConn(w2.ConnectionID, online, ""),
			webConn(w3.ConnectionID, online, "lounge")}
		settle()
		cancel()
		<-done

		got := lists()
		if len(got) < 3 {
			t.Fatalf("%d lists, want the first, the coalesced one and the later ones", len(got))
		}
		if !got[0].at.Equal(t0) || len(got[0].list) != 1 {
			t.Errorf("first list at %v with %d connections, want at the welcome with a1 alone", got[0].at.Sub(t0),
				len(got[0].list))
		}
		if got[1].at.Sub(t0) != coalesce {
			t.Errorf("second list after %v, want %v", got[1].at.Sub(t0), coalesce)
		}
		expectList(t, "coalesced", got[1].list, webConn(id1, online, ""), webConn(w2.ConnectionID, online, ""),
			webConn(w3.ConnectionID, online, "lounge"))
		for i := 1; i < len(got); i++ {
			if d := got[i].at.Sub(got[i-1].at); d < coalesce {
				t.Errorf("lists %d and %d %v apart, want at least %v", i-1, i, d, coalesce)
			}
		}
		// 20 changes over 1 s: at most one list per interval, and the last one has the final state.
		if n := len(got) - 2; n > 6 {
			t.Errorf("%d lists for 20 changes over 1 s, want at most 6", n)
		}
		expectList(t, "final", got[len(got)-1].list, final...)
	})
}

// deviceAuth is signaltest.Auth with device names: AuthenticateBearer and Revalidate report a bearer identity's
// current device name, as the wiring copies auth.Principal.DeviceName (M2 slice DA6).
type deviceAuth struct {
	*signaltest.Auth
	mu    sync.Mutex
	names map[string]string // device id → name
}

func (a *deviceAuth) rename(deviceID, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[deviceID] = name
}

func (a *deviceAuth) withName(id signal.Identity) signal.Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, ok := a.names[id.DeviceID]; ok && id.DeviceID != "" {
		id.DeviceName = n
	}
	return id
}

func (a *deviceAuth) AuthenticateBearer(ctx context.Context, tok protocol.Secret) (signal.Identity, error) {
	id, err := a.Auth.AuthenticateBearer(ctx, tok)
	if err != nil {
		return id, err
	}
	return a.withName(id), nil
}

func (a *deviceAuth) Revalidate(ctx context.Context, id signal.Identity, ip netip.Addr) (signal.Identity, error) {
	nid, err := a.Auth.Revalidate(ctx, id, ip)
	if err != nil {
		return nid, err
	}
	return a.withName(nid), nil
}

// newDeviceEnv is newEnv with deviceAuth as the Authenticator and the Wails origin allowed.
func newDeviceEnv(t *testing.T) (*env, *deviceAuth) {
	t.Helper()
	cfg := signal.DefaultConfig()
	cfg.PublicOrigin = testOrigin
	cfg.ServerVersion = serverVersion
	cfg.ResumeKey = bytes.Repeat([]byte{7}, 32)
	withWails(&cfg)
	e := &env{
		t:      t,
		auth:   signaltest.NewAuth(),
		rooms:  signaltest.NewRooms(),
		media:  signaltest.NewMedia(),
		push:   &signaltest.Push{},
		policy: &signaltest.Policy{},
		reg:    prometheus.NewRegistry(),
		logs:   &syncBuffer{},
	}
	da := &deviceAuth{Auth: e.auth, names: make(map[string]string)}
	hub, err := signal.New(cfg, signal.Deps{
		Auth:     da,
		Rooms:    e.rooms,
		Media:    e.media,
		Policy:   e.policy.Get,
		Push:     e.push,
		ClientIP: signaltest.ClientIP,
		Log:      slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:  e.reg,
	})
	if err != nil {
		t.Fatalf("signal.New: %v", err)
	}
	e.hub = hub
	e.srv = signaltest.StartServer(hub)
	e.hc = e.srv.Net.HTTPClient()
	return e, da
}

// A bearer connection's device name is on the list (M2 01 §10.4, 02 §6.3). A Revalidate that changes it sends a new
// list; one that changes nothing sends none. The name is never logged.
func TestUserConnectionsDeviceName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, da := newDeviceEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		dev := a
		dev.SessionID, dev.DeviceID, dev.DeviceName = "", "device01", "stale name"
		e.auth.AddBearer("token01", dev)
		da.rename("device01", "Gaming PC")

		// The desktop app's publisher connection (01 §11.1), with the feature for this test.
		h := signaltest.DefaultHello()
		h.Client = protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: "0.2.0", OS: protocol.ClientOSWindows}
		h.Role = protocol.RolePublisher
		h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: "token01"}
		h.Features = []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureUserConnections,
			protocol.FeatureSharePause}
		d := e.mustDial(headers("", wailsOrigin, ""))
		wd, err := d.Hello(ctxT(t), h)
		if err != nil {
			t.Fatalf("bearer hello: %v", err)
		}
		desktop := func(name string) protocol.OwnConnection {
			return protocol.OwnConnection{
				ConnectionInfo: protocol.ConnectionInfo{ID: wd.ConnectionID, Kind: protocol.ClientKindDesktop,
					Role: protocol.RolePublisher, Status: online},
				OS: protocol.ClientOSWindows, Version: "0.2.0", DeviceName: name,
			}
		}
		expectList(t, "desktop", expectConns(t, d), desktop("Gaming PC"))

		start := time.Now()
		a1, id1 := e.connsConn(cookieA)
		self := webConn(id1, online, "")
		expectList(t, "web tab", onlyConns(t, a1), self, desktop("Gaming PC"))
		expectList(t, "the desktop's view", onlyConns(t, d), self, desktop("Gaming PC"))

		// The device is renamed: the next revalidation of the desktop's connection reports it.
		da.rename("device01", "Living room PC")
		time.Sleep(signal.DefaultConfig().RevalidateEvery - time.Since(start) + time.Millisecond)
		synctest.Wait()
		for _, c := range []*signaltest.Client{a1, d} {
			expectList(t, "renamed", onlyConns(t, c), self, desktop("Living room PC"))
		}

		// A revalidation that changes nothing sends nothing.
		time.Sleep(signal.DefaultConfig().RevalidateEvery)
		settle()
		expectOpen(t, a1)
		expectOpen(t, d)
		if n := e.auth.Revalidations(); n < 6 {
			t.Errorf("%d Revalidate calls, want at least 6 (two connects, two ticks each)", n)
		}

		// A resume's handshake picks up a rename too: the list after its welcome has the new name.
		da.rename("device01", "Office PC")
		d.Close()
		expectList(t, "detached", onlyConns(t, a1), self, func() protocol.OwnConnection {
			o := desktop("Living room PC")
			o.Status = reconnecting
			return o
		}())
		hr := h
		hr.ResumeToken = wd.ResumeToken
		dr := e.mustDial(headers("", wailsOrigin, ""))
		if w, err := dr.Hello(ctxT(t), hr); err != nil || !w.Resumed {
			t.Fatalf("bearer resume: %v, resumed %v", err, w.Resumed)
		}
		expectList(t, "resumed", onlyConns(t, dr), self, desktop("Office PC"))
		expectList(t, "resumed", onlyConns(t, a1), self, desktop("Office PC"))

		for _, name := range []string{"Gaming PC", "Living room PC", "Office PC", "stale name"} {
			if e.logs.count(name) != 0 {
				t.Errorf("a log line has the device name %q:\n%s", name, e.logs)
			}
		}
	})
}
