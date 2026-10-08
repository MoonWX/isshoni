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
// (slow_connection), no hub call blocks meanwhile, and the other connections are not affected (01 §3.3, §19). The
// connection then enters grace like after any other drop, and the client can resume it.
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
		// The socket is gone; the connection is detached and keeps its slot.
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 2 || socks != 1 || slots != 1 {
			t.Errorf("conns %d, sockets %d, slots %d; want 2, 1, 1", conns, socks, slots)
		}
		if e.logs.count("connection detached", sw.ConnectionID, "code=4503") != 1 {
			t.Errorf("no detach line with 4503:\n%s", e.logs)
		}
		if got := e.metric("isshoni_ws_close_total", "code", "4503"); got != 1 {
			t.Errorf("isshoni_ws_close_total{code=4503} %v, want 1", got)
		}
		ping(t, other)

		// slow_connection is retryable: the client reconnects and resumes.
		h := signaltest.DefaultHello()
		h.ResumeToken = sw.ResumeToken
		back, w := e.connect(cookie, h)
		if !w.Resumed || w.ConnectionID != sw.ConnectionID {
			t.Errorf("resumed %v as %s, want the slow connection %s", w.Resumed, w.ConnectionID, sw.ConnectionID)
		}
		ping(t, back)
	})
}

// A client that never reads at all is closed when its queue overflows, and the hub's goroutines for it end: the
// socket's when the stuck write times out, the connection's when nothing has resumed it within the grace.
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
		// The writer is stuck in a write; its 10 s write timeout ends it, and the socket goes away without the client
		// ever reading or closing. (When the queue overflows before the writer has written anything, the close frame
		// is the one frame that the client's pending read still takes, and the socket goes at once: either way.) The
		// connection stays for its grace.
		synctest.Wait()
		if conns, _, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || slots != 1 {
			t.Fatalf("conns %d, slots %d once the queue has overflowed; want 1, 1", conns, slots)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 0 || slots != 1 {
			t.Errorf("conns %d, sockets %d, slots %d after the write timeout; want 1, 0, 1 (detached)", conns, socks, slots)
		}
		time.Sleep(grace)
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns+socks+slots != 0 {
			t.Errorf("conns %d, sockets %d, slots %d after the grace; want none", conns, socks, slots)
		}
	})
}
