package signal_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// A client that stops reading: its send queue overflows, the hub sends nothing more and closes with 4503
// (slow_connection), no hub call blocks meanwhile, and the other connections are not affected (01 §3.3, §19).
func TestSlowConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		slow, sw := e.connect(cookie, signaltest.DefaultHello())
		otherCookie, _ := e.user(false)
		other, _ := e.connect(otherCookie, signaltest.DefaultHello())

		slow.Pause()
		// 600 invalidates for the slow user: more than the 512-message queue. Every Notify returns at once (a
		// blocked call would deadlock the bubble and fail the test).
		for range 600 {
			e.hub.Notify(signal.Target{UserID: id.UserID}, protocol.TopicMe)
		}
		synctest.Wait()

		// Meanwhile the hub serves everyone else.
		ping(t, other)
		e.hub.Notify(signal.Target{All: true}, protocol.TopicRooms)
		expectType(t, other, protocol.MessageTypeInvalidate)

		// The slow client reads again: at most the message in flight when the queue overflowed, then 4503.
		slow.Resume()
		expectClose(t, slow, protocol.CloseCodeSlowConnection)
		got := 0
		for {
			env, err := slow.Recv(ctxT(t))
			if err != nil {
				break
			}
			if env.Type != protocol.MessageTypeInvalidate {
				t.Errorf("unexpected %s", env.Type)
			}
			got++
		}
		if got > 3 {
			t.Errorf("%d invalidates reached the slow client, want at most the ones in flight", got)
		}
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 1 || slots != 0 {
			t.Errorf("conns %d, sockets %d, slots %d; want 1, 1, 0", conns, socks, slots)
		}
		if e.logs.count("connection closed", sw.ConnectionID, "code=4503") != 1 {
			t.Errorf("no close line with 4503:\n%s", e.logs)
		}
		if got := e.metric("isshoni_ws_close_total", "code", "4503"); got != 1 {
			t.Errorf("isshoni_ws_close_total{code=4503} %v, want 1", got)
		}
		ping(t, other)
	})
}

// A client that never reads at all is closed when its queue overflows, and the hub's goroutines for it end.
func TestSlowConsumerNeverReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		c.Pause()
		for range 1000 {
			e.hub.Notify(signal.Target{UserID: id.UserID}, protocol.TopicMe)
		}
		// The writer is stuck in a write; its 10 s write timeout ends it, and the socket and the connection go away
		// without the client ever reading or closing.
		synctest.Wait()
		if conns, _, _, _ := signal.Counts(e.hub, id.UserID); conns != 1 {
			t.Fatalf("%d connections while the write is pending, want 1", conns)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns+socks+slots != 0 {
			t.Errorf("conns %d, sockets %d, slots %d after the write timeout; want none", conns, socks, slots)
		}
	})
}
