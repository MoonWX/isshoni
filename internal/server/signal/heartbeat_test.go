package signal_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// A client that sends no messages but answers the hub's ping frames stays open past IdleTimeout (01 §3.4, §19).
func TestHeartbeatPongsKeepOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		expectOpen(t, c)
		ping(t, c)
	})
}

// One that answers nothing gets idle_timeout and 4408 after IdleTimeout (45 s).
func TestHeartbeatIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, err := e.dial(signaltest.DialOptions{Header: headers(cookie, testOrigin, ""), NoPong: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(45*time.Second - time.Millisecond)
		synctest.Wait()
		expectOpen(t, c)
		time.Sleep(time.Millisecond)
		pe := expectFail(t, c, protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection)
		if !pe.Retryable {
			t.Error("idle_timeout is not retryable")
		}
	})
}

// Messages count as activity too: a client that ignores ping frames but sends a ping message every 30 s stays open.
func TestHeartbeatMessagesKeepOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, err := e.dial(signaltest.DialOptions{Header: headers(cookie, testOrigin, ""), NoPong: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
			t.Fatal(err)
		}
		for range 6 {
			time.Sleep(30 * time.Second)
			ping(t, c)
		}
		expectOpen(t, c)
	})
}
