package itest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// TestShutdownWithMedia: a server that stops while a share is being watched takes its parts down in the order of
// 04 §6.4: the hub says goodbye on every socket and ends the shares, the SFU closes the PeerConnections, and the
// Transport closes the sockets under them. Nothing has to be cut off, the media stops, and nothing is left behind:
// the package's leak check runs after this test like after every other. The order of the three steps itself is
// internal/server's TestShutdownOrder; here they run with live PeerConnections on both sides.
func TestShutdownWithMedia(t *testing.T) {
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(9, sfutest.LoopbackSettings())
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 15)
	waitPackets(t, audio, 25)

	begin := time.Now()
	w.srv.Stop(t) // fails the test when the shutdown had to use force, or took longer than shutdown_timeout
	took := time.Since(begin)
	// The budgets of the hub's and the SFU's steps are 2 s and 1 s (04 §6.4); a shutdown that needed them would
	// have waited for something.
	if took > 3*time.Second {
		t.Errorf("the shutdown took %v with one share and one viewer", took)
	}

	// Both clients were told before their sockets closed: the server is coming back, try again in a moment.
	for _, c := range []*client{alice, bob} {
		eventually(t, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return len(c.goodbye) > 0
		}, func() string { return c.name + " got no server.shutdown" })
		c.mu.Lock()
		bye := slices.Clone(c.goodbye)
		c.mu.Unlock()
		if len(bye) != 1 || bye[0].Reason != protocol.ShutdownReasonRestart || bye[0].ReconnectInMs < 500 || bye[0].ReconnectInMs > 3000 {
			t.Errorf("%s: server.shutdown = %+v, want one that announces a restart", c.name, bye)
		}
	}
	// The media has stopped: the server's sockets are closed.
	time.Sleep(100 * time.Millisecond) // what was on its way has arrived
	v, a := video.Stats().Packets, audio.Stats().Packets
	time.Sleep(300 * time.Millisecond)
	if v2, a2 := video.Stats().Packets, audio.Stats().Packets; v2 != v || a2 != a {
		t.Errorf("the viewer got %d video and %d audio packets after the server had stopped", v2-v, a2-a)
	}
	// What the viewer had got until then is whole.
	checkVideo(t, "video", video.Packets())

	// The server's own account of it: the shutdown ran to its end, nothing was forced, and nothing along the way
	// was an error, or worth a warning of the server's own. (Pion's warnings, which carry a scope, are not: a
	// PeerConnection that closes under a running stream makes some.)
	var complete bool
	for line := range strings.SplitSeq(strings.TrimSpace(w.srv.Logs()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if level, pion := rec["level"], rec["scope"] != nil; level == "ERROR" || (level == "WARN" && !pion) {
			t.Errorf("the server logged %v", rec)
		}
		complete = complete || rec["msg"] == "shutdown complete"
	}
	if !complete {
		t.Errorf("the server's log has no \"shutdown complete\":\n%s", w.srv.Logs())
	}
}
