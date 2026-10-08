package signal

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// A normal drop (Close) sends close code 1000 on every socket that lives, also on one whose reader has ended
// already: only that makes the server skip the grace period (01 §4.2). A reader ends on a live socket when drop
// halts it while it waits for a caller that receives no more, and whether it has ended by the time drop looks is a
// race that TestCloseWithFullEvents loses only now and then. Here the reader has always ended.
func TestDropTellsALiveSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		over bool
		want websocket.StatusCode // what the server sees; -1: no close frame
	}{
		{"the reader ended on a live socket", false, websocket.StatusNormalClosure},
		{"the socket is over", true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				closed := make(chan websocket.StatusCode, 1)
				srv := signaltest.StartServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
					if err != nil {
						return
					}
					go func() { // the socket outlives its upgrade request
						_, _, err := ws.Read(context.Background())
						closed <- websocket.CloseStatus(err)
						_ = ws.CloseNow()
					}()
				}))
				defer srv.Close()

				c := newClient(context.Background(), Options{URL: srv.URL("/ws"), HTTPClient: srv.Net.HTTPClient()})
				defer c.cancel()
				ws, err := c.upgrade(c.life)
				if err != nil {
					t.Fatal(err)
				}
				s := newSocket(c, ws, "1")
				s.over.Store(tc.over)
				close(s.done) // the reader has ended
				if err := c.drop(s, true); err != nil {
					t.Errorf("drop: %v", err)
				}
				if got := <-closed; got != tc.want {
					t.Errorf("the server saw close code %d, want %d", got, tc.want)
				}
			})
		})
	}
}
