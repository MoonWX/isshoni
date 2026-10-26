package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/relay"
)

// relay.go against fakeServer: docs/m2/01-client-core.md §11.11's core rules 1–3 (§14's core row).

// relayMsg sends an agent.recv from connection from, as the hub relays one.
func relayMsg(fs *fakeServer, from, kind, payload string) {
	fs.send(protocol.MessageTypeAgentRecv, protocol.AgentRecv{From: from, Kind: kind, Payload: json.RawMessage(payload)})
}

// TestPlaybackState: a playback.state that decodes becomes EventPlaybackState with the sender's connection id
// (rule 1), unknown fields ignored; one that doesn't decode, or names a browser outside the four families, is
// ignored, and so are playback.stop (rule 2) and every other kind (rule 3). The core's log names the kinds it
// ignored, never a payload.
func TestPlaybackState(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{})
	e.run()
	e.waitOnline()
	events, unsub := e.c.Subscribe()
	defer unsub()

	const marker = "mrk7q"
	for _, m := range []struct{ kind, payload string }{
		{relay.KindPlaybackState, `{"playing": true, "browser": "safari", "os": "windows"}`}, // not a family
		{relay.KindPlaybackState, `{"playing": true, "browser": "` + marker + `", "os": "windows"}`},
		{relay.KindPlaybackState, `{"playing": true, "os": "windows"}`},                       // no browser
		{relay.KindPlaybackState, `{"playing": "yes", "browser": "chrome", "os": "windows"}`}, // the wrong type
		{relay.KindPlaybackState, `"` + marker + `"`},                                         // not an object
		{relay.KindPlaybackState, `[{"playing": true, "browser": "chrome", "os": "windows"}]`},
		{relay.KindPlaybackStop, `{"v": 1, "roomId": "lounge"}`},
		{relay.KindShareRequest, `{"preset": "game", "x": "` + marker + `"}`},
		{relay.KindExclusionsGet, `{}`},
		{"later.kind", `{"playing": true, "browser": "chrome", "os": "windows"}`},
	} {
		relayMsg(fs, "c_9", m.kind, m.payload)
	}
	// The valid ones, after the rest on the same socket: the first event is theirs.
	relayMsg(fs, "c_9", relay.KindPlaybackState,
		`{"playing": true, "browser": "chrome", "os": "windows", "v": 1, "tab": "`+marker+`"}`)
	relayMsg(fs, "c_8", relay.KindPlaybackState, `{"playing": false, "browser": "other", "os": "macos"}`)

	isPlayback := func(ev Event) bool { return ev.Kind == EventPlaybackState }
	for _, want := range []PlaybackState{
		{From: "c_9", Playing: true, Browser: relay.BrowserChrome, OS: protocol.ClientOSWindows},
		{From: "c_8", Playing: false, Browser: relay.BrowserOther, OS: protocol.ClientOSMacOS},
	} {
		ev := nextEvent(t, events, isPlayback)
		if ev.Playback == nil || *ev.Playback != want || ev.Status != nil || ev.Share != nil || ev.Notice != nil {
			t.Errorf("event %+v (playback %+v), want EventPlaybackState %+v alone", ev, ev.Playback, want)
		}
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case ev := <-events:
		t.Errorf("an event after the two valid reports: %+v", ev)
	default:
	}
	logs := e.logs.String()
	if strings.Contains(logs, marker) || strings.Contains(logs, "safari") {
		t.Errorf("the core's log quotes a relay payload:\n%s", logs)
	}
	for _, kind := range []string{relay.KindPlaybackState, relay.KindPlaybackStop, relay.KindShareRequest,
		relay.KindExclusionsGet, "later.kind"} {
		if !strings.Contains(logs, "kind="+kind) {
			t.Errorf("the core's log has no line about the ignored %s", kind)
		}
	}
}
