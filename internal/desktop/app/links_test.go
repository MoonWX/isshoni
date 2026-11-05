package app

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Group 16's follow-ups to 03 §13.2's table (README WA83-R2, WA83-R4).

// TestConfirmedSwitchOpensRoom is 03 §13.2's "server known, not active, live share" row to its end (WA83-R2): the
// link asks; the page's Open stops the share and calls SwitchServer, which makes the server active with one reload
// and keeps the link's room pending, so the reloaded page opens it (DesktopLinks' first pull). A link to the
// server's own last room keeps {roomId: ""}. A switch to another server than the question's drops the question.
func TestConfirmedSwitchOpensRoom(t *testing.T) {
	origins := []string{"https://a.example.com", "https://b.example.com", "https://c.example.com"}
	t.Run("the link's room", func(t *testing.T) {
		e := newEnv(t, origins, nil)
		live := true
		e.app.sharing = func() (string, bool) { return "lounge", live }
		e.app.HandleLink(link(t, "isshoni://b.example.com/r/movies"))
		if got := lastEvent(t, e.shell); got.Switch == nil || got.Switch.RoomID != "movies" {
			t.Fatalf("deeplink.open = %+v", got)
		}
		// Open: Share.Stop first (SwitchServer refuses while the share is live), then SwitchServer.
		if err := e.app.SwitchServer("https://b.example.com"); !errors.Is(err, ErrBusy) {
			t.Fatalf("SwitchServer while live = %v, want ErrBusy", err)
		}
		live = false
		if err := e.app.SwitchServer("https://B.example.com:443"); err != nil {
			t.Fatal(err)
		}
		if e.srv.Active().State().Origin != "https://b.example.com" || e.shell.count("reload") != 1 {
			t.Errorf("active %s, %d reloads", e.srv.Active().State().Origin, e.shell.count("reload"))
		}
		p := e.app.PendingLink()
		if p == nil || p.RoomID != "movies" || p.Switch != nil || p.Live != nil || p.Add != nil {
			t.Fatalf("PendingLink after the switch = %+v, want {roomId: movies}", p)
		}
		// The reloaded page opens it and says so.
		e.app.LinkHandled()
		if p := e.app.PendingLink(); p != nil {
			t.Errorf("PendingLink after LinkHandled = %+v", p)
		}
	})
	t.Run("the server's last room", func(t *testing.T) {
		e := newEnv(t, origins, nil)
		live := true
		e.app.sharing = func() (string, bool) { return "lounge", live }
		e.app.HandleLink(link(t, "isshoni://b.example.com/"))
		live = false
		if err := e.app.SwitchServer("https://b.example.com"); err != nil {
			t.Fatal(err)
		}
		if p := e.app.PendingLink(); p == nil || p.RoomID != "" || p.Switch != nil {
			t.Fatalf("PendingLink = %+v, want {roomId: \"\"}", p)
		}
	})
	t.Run("another server", func(t *testing.T) {
		e := newEnv(t, origins, nil)
		live := true
		e.app.sharing = func() (string, bool) { return "lounge", live }
		e.app.HandleLink(link(t, "isshoni://b.example.com/r/movies"))
		live = false
		// The server switcher, not the question's Open.
		if err := e.app.SwitchServer("https://c.example.com"); err != nil {
			t.Fatal(err)
		}
		if p := e.app.PendingLink(); p != nil {
			t.Fatalf("PendingLink = %+v, want none", p)
		}
	})
}

// TestTooManyServersShownOnce (WA83-R4): {error: too_many_servers} stays pending for the page that shows it (the
// setup page reads it with PendingLink), is gone once the page says it was shown (LinkHandled), and never outlives a
// reload: removing a server (what the notice asks) reloads the window, and the reloaded page finds nothing pending.
func TestTooManyServersShownOnce(t *testing.T) {
	var origins []string
	for i := range MaxServers {
		origins = append(origins, fmt.Sprintf("https://s%d.example.com", i))
	}
	t.Run("shown", func(t *testing.T) {
		e := newEnv(t, origins, nil)
		e.app.HandleLink(link(t, "isshoni://new.example.com/"))
		// DesktopLinks pulls it and goes to the setup page, which pulls it again and shows it.
		for range 2 {
			if p := e.app.PendingLink(); p == nil || p.Error != "too_many_servers" {
				t.Fatalf("PendingLink = %+v", p)
			}
		}
		e.app.LinkHandled()
		if p := e.app.PendingLink(); p != nil {
			t.Errorf("PendingLink after it was shown = %+v", p)
		}
	})
	t.Run("a reload", func(t *testing.T) {
		e := newEnv(t, origins, nil)
		e.app.HandleLink(link(t, "isshoni://new.example.com/"))
		if err := e.app.RemoveServer(t.Context(), "https://s0.example.com"); err != nil {
			t.Fatal(err)
		}
		if e.shell.count("reload") != 1 {
			t.Fatalf("%d reloads", e.shell.count("reload"))
		}
		if p := e.app.PendingLink(); p != nil {
			t.Errorf("PendingLink after the reload = %+v", p)
		}
		// A room link for the next page is no notice: a reload keeps it (the switch's own pending link).
		e.clock.Advance(time.Second)
		e.app.HandleLink(link(t, "isshoni://s2.example.com/r/den"))
		if p := e.app.PendingLink(); p == nil || p.RoomID != "den" {
			t.Errorf("PendingLink after a switch = %+v", p)
		}
	})
}
