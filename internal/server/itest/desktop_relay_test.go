package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/relay"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Case D18 of docs/m2/01-client-core.md §14 and the rest of its slice C14 (README WA55): a web tab's playback.state
// reaches the desktop app as EventPlaybackState, and nothing else of the relay does; the core's stats reports reach
// the hub every 10 s and pass its checks; WatchServerStats brings the server's view of the share into its snapshot;
// the engine's stats are in it too.

// relayTab is a web tab of a member's that talks to the user's other connections over the relay: a full-role
// connection with agent.relay, in no room, as rooms/handoff.ts uses one (03 §12.2). It only drains its
// notifications.
type relayTab struct {
	t    *testing.T
	sig  *signal.Client
	done chan struct{}
}

// relayTab opens a relay tab for m. It closes when the test ends.
func (w *world) relayTab(m *member) *relayTab {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	sig, err := signal.Dial(ctx, signal.Options{
		URL:        w.srv.WSURL,
		HTTPClient: &http.Client{Jar: m.http.Jar, Transport: w.srv.Client.Transport},
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindWeb, Version: "0.2.0", OS: protocol.ClientOSWindows},
		Role:       protocol.RoleFull,
		Caps:       caps,
		Features:   []protocol.Feature{protocol.FeatureAgentRelay},
	})
	if err != nil {
		w.t.Fatalf("%s's tab: dial: %v", m.name, err)
	}
	tab := &relayTab{t: w.t, sig: sig, done: make(chan struct{})}
	go func() {
		defer close(tab.done)
		for range sig.Events() { // a signaling client whose Events nobody drains stops reading its socket
		}
	}()
	w.t.Cleanup(func() {
		_ = sig.Close()
		<-tab.done
	})
	return tab
}

// send relays payload of kind to connection to and fails the test unless the server delivered it to exactly one
// connection: it never parses a payload, so an invalid one is delivered too (M1 01 §8.14).
func (tab *relayTab) send(to, kind, payload string) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	var res protocol.AgentSendResult
	err := tab.sig.Request(ctx, protocol.MessageTypeAgentSend,
		protocol.AgentSend{To: to, Kind: kind, Payload: json.RawMessage(payload)}, &res)
	if err != nil || res.Delivered != 1 {
		tab.t.Fatalf("the tab's agent.send of %s: delivered %d, %v; want delivered to the app", kind, res.Delivered, err)
	}
}

// playbackEvents returns the app's EventPlaybackState reports so far, in order.
func (d *desktopApp) playbackEvents() []core.PlaybackState {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.PlaybackState
	for _, ev := range d.events {
		if ev.Kind == core.EventPlaybackState && ev.Playback != nil {
			out = append(out, *ev.Playback)
		}
	}
	return out
}

// TestDesktopPlaybackState is D18: a full-role connection of the user sends agent.send{to: Status.ConnectionID,
// kind: playback.state, payload} → EventPlaybackState with From = that connection; a payload that doesn't decode, a
// browser outside the four families and the other kinds are ignored, and the core's log quotes none of them.
func TestDesktopPlaybackState(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	app := w.desktop(alice, desktopOptions{})
	tab := w.relayTab(alice)
	to, from := app.core.Status().ConnectionID, tab.sig.Welcome().ConnectionID

	// What the core ignores, sent first: the hub relays one connection's messages in order, so the first event the
	// app reports is the valid report's.
	const marker = "mrk4z"
	tab.send(to, relay.KindPlaybackState, `{"playing": true, "browser": "safari", "os": "windows"}`)
	tab.send(to, relay.KindPlaybackState, `{"playing": "yes", "browser": "chrome", "os": "`+marker+`"}`)
	tab.send(to, relay.KindPlaybackState, `"`+marker+`"`)
	tab.send(to, relay.KindPlaybackStop, `{"v": 1, "roomId": "`+marker+`"}`)
	tab.send(to, relay.KindShareRequest, `{"preset": "game"}`)
	tab.send(to, relay.KindPlaybackState, `{"playing": true, "browser": "chrome", "os": "windows", "tab": "`+marker+`"}`)
	want := []core.PlaybackState{{From: from, Playing: true, Browser: relay.BrowserChrome, OS: protocol.ClientOSWindows}}
	eventually(t, func() bool { return len(app.playbackEvents()) == len(want) },
		func() string {
			return fmt.Sprintf("the app's playback reports %+v, want %+v", app.playbackEvents(), want)
		})
	tab.send(to, relay.KindPlaybackState, `{"playing": false, "browser": "edge", "os": "windows"}`)
	want = append(want, core.PlaybackState{From: from, Browser: relay.BrowserEdge, OS: protocol.ClientOSWindows})
	eventually(t, func() bool { return len(app.playbackEvents()) == len(want) },
		func() string {
			return fmt.Sprintf("the app's playback reports %+v, want %+v", app.playbackEvents(), want)
		})
	time.Sleep(200 * time.Millisecond)
	if got := app.playbackEvents(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the app's playback reports %+v, want %+v", got, want)
	}
	if logs := app.logs.String(); strings.Contains(logs, marker) || strings.Contains(logs, "safari") {
		t.Errorf("the core's log quotes a relay payload:\n%s", logs)
	}
}

// statsIn is how many stats reports the hub has received (isshoni_ws_messages_total counts each message as it
// arrives, before its checks).
func statsIn(samples map[string]float64) float64 {
	return samples[`isshoni_ws_messages_total{dir="in",type="stats"}`]
}

// TestDesktopStats: while the app shares, the engine's stats reach the snapshot (the full layer's frame rate in and
// its limit), a stats report reaches the hub every 10 s and passes the hub's checks (no bad_request), the core's log
// shows its pub PC connected over a host candidate and UDP, and WatchServerStats(true) brings the server's view of
// the share's layers into ShareSnapshot.Server, which WatchServerStats(false) clears.
func TestDesktopStats(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{Config: withMetrics})
	app := w.desktop(w.member("alice"), appEngine(5))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	app.core.WatchServerStats(true) // in no room: the server's stats start with the share's room.join

	sh := app.start(defaultRoom(bob))
	snap := app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, true))
	waitFrames(t, track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo), 0, "f", 30)

	// The engine's stats (every 1 s while live).
	eventually(t, func() bool {
		f := layerOf(sh.Snapshot(), "f")
		return f.FPSIn > 0 && f.Limit == "none"
	}, func() string {
		return fmt.Sprintf("the full layer %+v, want the engine's frame rate in and limit", layerOf(sh.Snapshot(), "f"))
	})

	// The server's stats of the share's layers (every 2 s).
	eventually(t, func() bool {
		for _, l := range sh.Snapshot().Server {
			if l.ShareID == snap.ID && l.Kind == protocol.TrackKindVideo && l.RID == "f" && l.Bitrate > 0 {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("ShareSnapshot.Server = %+v, want the server's view of %s's full layer", sh.Snapshot().Server,
			snap.ID)
	})
	for _, l := range sh.Snapshot().Server {
		if l.ShareID != snap.ID {
			t.Errorf("ShareSnapshot.Server has a layer of another share: %+v", l)
		}
	}

	// The stats reports: two of them, 10 s apart (the hub drops one less than 5 s after the last, M1 01 §8.11).
	var samples map[string]float64
	var first time.Time
	deadline := time.Now().Add(2 * waitTimeout)
	for {
		samples = scrape(t, w)
		n := statsIn(samples)
		if n >= 1 && first.IsZero() {
			first = time.Now()
		}
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hub has %v stats reports from the app, want 2 within %v", n, 2*waitTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
	gap := time.Since(first)
	t.Logf("stats reports: the second %v after the first", gap.Round(10*time.Millisecond))
	// 10 s apart: the core times the next report from the last one's send. The scrapes see each report up to their
	// spacing (and a slow scrape) late, so the bounds leave room.
	if gap < 8*time.Second || gap > 20*time.Second {
		t.Errorf("the second stats report came %v after the first, want 10 s", gap)
	}
	if n := samples[`isshoni_ws_errors_total{code="bad_request"}`]; n != 0 {
		t.Errorf("the hub refused %v messages with bad_request, want none", n)
	}
	var sent []string
	for line := range strings.SplitSeq(app.logs.String(), "\n") {
		if strings.Contains(line, `msg="stats report sent"`) {
			sent = append(sent, line)
		}
	}
	if len(sent) < 2 {
		t.Fatalf("the core's log has %d stats reports, want at least the 2 the hub got", len(sent))
	}
	t.Logf("the last report: %s", sent[len(sent)-1])
	for _, field := range []string{"state=connected", "candidate_type=host", "transport=udp", "outbound=3"} {
		if !strings.Contains(sent[len(sent)-1], field) {
			t.Errorf("the last stats report (%s) lacks %s", sent[len(sent)-1], field)
		}
	}

	// Off: cleared at once, and nothing more arrives.
	app.core.WatchServerStats(false)
	if got := sh.Snapshot().Server; got != nil {
		t.Errorf("ShareSnapshot.Server right after WatchServerStats(false) = %+v, want none", got)
	}
	eventually(t, func() bool {
		samples = scrape(t, w)
		return samples[`isshoni_ws_messages_total{dir="in",type="stats.watch"}`] == 2
	}, func() string {
		return fmt.Sprintf("the hub's stats.watch count %v, want on and off",
			samples[`isshoni_ws_messages_total{dir="in",type="stats.watch"}`])
	})
	time.Sleep(3 * time.Second) // more than the server's 2 s
	if got := sh.Snapshot().Server; got != nil {
		t.Errorf("ShareSnapshot.Server 3 s after WatchServerStats(false) = %+v, want none", got)
	}
	bob.check()
}
