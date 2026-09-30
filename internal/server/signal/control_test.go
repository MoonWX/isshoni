package signal_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// CloseConnections (01 §3.2, §15.2): by user and session, keeping an excepted session, never without a user id;
// account_disabled closes with 4403. Sockets still in their handshake are closed too.
func TestCloseConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieA2 := e.session(a, "sessionA2")
		cookieB, bid := e.user(false)
		a1, _ := e.connect(cookieA, signaltest.DefaultHello())
		a1b, _ := e.connect(cookieA, signaltest.DefaultHello())
		a2, _ := e.connect(cookieA2, signaltest.DefaultHello())
		b, _ := e.connect(cookieB, signaltest.DefaultHello())

		if n := e.hub.CloseConnections(signal.ConnSelector{}, protocol.ErrorCodeSessionRevoked); n != 0 {
			t.Errorf("empty selector closed %d", n)
		}
		if e.logs.count("level=ERROR", "CloseConnections without a user id") != 1 {
			t.Error("no ERROR line for the empty selector")
		}

		n := e.hub.CloseConnections(signal.ConnSelector{UserID: a.UserID, SessionID: a.SessionID},
			protocol.ErrorCodeSessionRevoked)
		if n != 2 {
			t.Errorf("closed %d, want 2", n)
		}
		expectFail(t, a1, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)
		expectFail(t, a1b, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)
		ping(t, a2)
		ping(t, b)
		synctest.Wait()

		// "Sign out other browsers": the excepted session stays.
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: a.UserID, ExceptSessionID: "sessionA2"},
			protocol.ErrorCodeSessionRevoked); n != 0 {
			t.Errorf("closed %d, want 0", n)
		}
		ping(t, a2)

		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: a.UserID}, protocol.ErrorCodeAccountDisabled); n != 1 {
			t.Errorf("closed %d, want 1", n)
		}
		expectFail(t, a2, protocol.ErrorCodeAccountDisabled, protocol.ErrorScopeSession)

		// A code other than the two revocation codes is sent as session_revoked.
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: bid.UserID}, protocol.ErrorCodeForbidden); n != 1 {
			t.Errorf("closed %d, want 1", n)
		}
		expectFail(t, b, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)

		// A socket in its handshake.
		cookieC, c := e.user(false)
		pending := e.mustDial(headers(cookieC, testOrigin, ""))
		synctest.Wait()
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: c.UserID}, protocol.ErrorCodeSessionRevoked); n != 1 {
			t.Errorf("closed %d handshaking sockets, want 1", n)
		}
		expectFail(t, pending, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)
		if e.logs.count("level=INFO", "revocation", "user_id="+a.UserID) != 3 {
			t.Errorf("revocation log lines:\n%s", e.logs)
		}
	})
}

// Revalidation every RevalidateEvery (01 §3.2): a transient error keeps the connection and is retried at the next
// tick; ErrInvalid closes it with session_revoked; a changed role is applied.
func TestRevalidateTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		ca, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		if n := e.auth.Revalidations(); n != 2 {
			t.Fatalf("%d revalidations at connect, want 2", n)
		}

		e.auth.FailRevalidate(errors.New("context deadline exceeded"))
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := e.auth.Revalidations(); n != 4 {
			t.Errorf("%d revalidations after one tick, want 4", n)
		}
		expectOpen(t, ca)
		if e.logs.count("level=WARN", "msg=revalidate", "user_id="+a.UserID) != 1 {
			t.Errorf("no WARN line:\n%s", e.logs)
		}

		e.auth.FailRevalidate(nil)
		e.auth.Revoke(a.SessionID)
		e.auth.SetUser(b.UserID, "promoted", true)
		time.Sleep(5 * time.Minute)
		expectFail(t, ca, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)

		// b is an admin now: admin topics reach it.
		synctest.Wait()
		e.hub.Notify(signal.Target{Admins: true}, protocol.TopicAdminUsers)
		expectInvalidate(t, cb, protocol.TopicAdminUsers)
	})
}

// A slow Revalidate (03's store busy) runs off the connection's actor: messages are answered at once while it runs,
// its result is applied when it arrives, and a call that outlasts its 10 s budget is a transient error.
func TestRevalidateSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, a := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())

		release := e.auth.HoldRevalidate()
		e.auth.SetUser(a.UserID, "promoted", true)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := e.auth.Revalidations(); n != 2 {
			t.Fatalf("%d revalidations after one tick, want 2", n)
		}
		start := time.Now()
		ping(t, c)
		if d := time.Since(start); d != 0 {
			t.Errorf("pong after %v while Revalidate runs, want at once", d)
		}
		release()
		synctest.Wait()
		e.hub.Notify(signal.Target{Admins: true}, protocol.TopicAdminUsers) // a is an admin now
		expectInvalidate(t, c, protocol.TopicAdminUsers)

		release = e.auth.HoldRevalidate()
		defer release()
		time.Sleep(5*time.Minute + 11*time.Second) // the next tick, and past the call's 10 s
		synctest.Wait()
		if e.logs.count("level=WARN", "msg=revalidate", "user_id="+a.UserID, "deadline exceeded") != 1 {
			t.Errorf("no WARN line for the timed-out call:\n%s", e.logs)
		}
		expectOpen(t, c)
		ping(t, c)
	})
}

// expectInvalidate reads invalidate{topics}; nothing else may be queued before the next pong.
func expectInvalidate(t *testing.T, c *signaltest.Client, topics ...protocol.Topic) {
	t.Helper()
	inv, err := protocol.Decode[protocol.Invalidate](expectType(t, c, protocol.MessageTypeInvalidate))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inv.Topics, topics) {
		t.Errorf("topics %v, want %v", inv.Topics, topics)
	}
	ping(t, c)
}

// Notify targets (01 §19): all, a user, admins; admin topics reach admins only; a room target matches nothing
// before rooms exist.
func TestNotify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(true)
		cookieB, b := e.user(false)
		cookieC, c := e.user(false)
		ca, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb1, _ := e.connect(cookieB, signaltest.DefaultHello())
		cb2, _ := e.connect(cookieB, signaltest.DefaultHello())
		cc, _ := e.connect(cookieC, signaltest.DefaultHello())
		all := []*signaltest.Client{ca, cb1, cb2, cc}

		e.hub.Notify(signal.Target{All: true}, protocol.TopicRooms)
		for _, cl := range all {
			expectInvalidate(t, cl, protocol.TopicRooms)
		}

		e.hub.Notify(signal.Target{UserID: b.UserID}, protocol.TopicMe, protocol.TopicDevices)
		expectInvalidate(t, cb1, protocol.TopicMe, protocol.TopicDevices)
		expectInvalidate(t, cb2, protocol.TopicMe, protocol.TopicDevices)
		ping(t, ca)
		ping(t, cc)

		e.hub.Notify(signal.Target{Admins: true}, protocol.TopicAdminApprovals)
		expectInvalidate(t, ca, protocol.TopicAdminApprovals)

		e.hub.Notify(signal.Target{All: true}, protocol.TopicRooms, protocol.TopicAdminUsers)
		expectInvalidate(t, ca, protocol.TopicRooms, protocol.TopicAdminUsers)
		for _, cl := range []*signaltest.Client{cb1, cb2, cc} {
			expectInvalidate(t, cl, protocol.TopicRooms)
		}

		e.hub.Notify(signal.Target{UserID: c.UserID}, protocol.TopicAdminSettings) // not an admin
		e.hub.Notify(signal.Target{RoomID: "lounge"}, protocol.TopicRooms)         // nobody is in a room yet
		e.hub.Notify(signal.Target{All: true})                                     // no topics
		for _, cl := range all {
			ping(t, cl)
		}

		// UpdateUser: c becomes an admin.
		e.hub.UpdateUser(c.UserID, "carol", true)
		synctest.Wait()
		e.hub.Notify(signal.Target{Admins: true}, protocol.TopicAdminInvites)
		expectInvalidate(t, ca, protocol.TopicAdminInvites)
		expectInvalidate(t, cc, protocol.TopicAdminInvites)
		ping(t, cb1)
	})
}

// CloseRoom and Snapshot before rooms exist (README S19): the closed id is recorded, the snapshot is empty.
func TestCloseRoomAndSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		e.hub.CloseRoom("room1")
		if snap := e.hub.Snapshot(); snap.Rooms == nil || len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v, want no rooms", snap)
		}
		if e.logs.count("room closed", "room_id=room1") != 1 {
			t.Error("no room closed line")
		}
		ping(t, c)
	})
}
