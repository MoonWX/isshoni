package itest

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Slice WF3 (docs/m2/01-client-core.md §10.3, group 5's WA46-R2): a share that the client core pauses before it is
// live sends no frame, so it can't go live before its resume. While a starting share is paused the hub suspends its
// 30 s start timeout and room.state shows it paused; the resume starts a fresh start timeout, and the first keyframe
// after it brings the share live. The two ways a share gets there: paused right after StartShare, and paused across a
// server restart (the core publishes it again with share.start{replaces} and share.update{paused: true}, README
// WA38's hand-over).

// pausedStay is how long these cases keep a starting share paused: past the hub's start timeout (ShareStartTimeout,
// 30 s, 01 §4.4), with room for its timer and the room.state coalescing.
const pausedStay = 30*time.Second + 5*time.Second

// shareLogged reports whether a line of the server's log has all of words.
func shareLogged(w *world, words ...string) bool {
	for line := range strings.SplitSeq(w.srv.Logs(), "\n") {
		all := true
		for _, word := range words {
			all = all && strings.Contains(line, word)
		}
		if all {
			return true
		}
	}
	return false
}

// checkPausedStarting checks a share that the core paused before it went live, once it has stayed paused past the
// start timeout: the viewer's room.state still has it, paused; no room.state ever showed it live and no room.event
// named it (neither share.started nor share.stopped); the server logged no end of it (media_timeout or any other);
// and the core still has it, paused, live as the server shows it.
func checkPausedStarting(t *testing.T, w *world, sh *core.Share, viewer *client, id string) {
	t.Helper()
	info, ok := viewer.shareIn(id)
	if !ok || info.Status != protocol.ShareStatusPaused {
		t.Fatalf("after %v paused the share %s in room.state is %+v (there: %t), want it paused", pausedStay, id, info, ok)
	}
	for _, st := range viewer.roomStates() {
		for _, s := range st.Shares {
			if s.ID == id && s.Status != protocol.ShareStatusStarting && s.Status != protocol.ShareStatusPaused {
				t.Errorf("room.state rev %d shows the share %s %s before its resume, want starting or paused", st.Rev,
					id, s.Status)
			}
		}
	}
	for _, ev := range viewer.roomEvents() {
		if ev.ShareID == id {
			t.Errorf("room.event %+v before the resume, want none for the paused share", ev)
		}
	}
	if shareLogged(w, "share ended", id) {
		t.Errorf("the server ended the paused share %s:\n%s", id, w.srv.Logs())
	}
	select {
	case <-sh.Done():
		t.Fatalf("the core's share ended while paused: %+v", sh.Snapshot())
	default:
	}
	eventually(t, func() bool {
		s := sh.Snapshot()
		return s.ID == id && s.Paused && s.State == core.StateLive
	}, func() string {
		return fmt.Sprintf("the core's share is %+v, want %s paused, and live as the server shows it", sh.Snapshot(), id)
	})
}

// resumeToLive resumes the paused share and checks that it goes live: the viewer's room.state shows it live with both
// layers, room.event share.started names it, the core's share is live and not paused, and the viewer, subscribed to
// it while it was paused, gets its video from a keyframe.
func resumeToLive(t *testing.T, sh *core.Share, viewer *client, view *sfutest.Viewer, id string) {
	t.Helper()
	if err := sh.SetPaused(waitCtx(t), false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	viewer.waitLive(id)
	eventually(t, func() bool {
		for _, ev := range viewer.roomEvents() {
			if ev.Kind == protocol.RoomEventKindShareStarted && ev.ShareID == id {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the room.events %+v name no share.started of %s", viewer.roomEvents(), id)
	})
	eventually(t, func() bool {
		s := sh.Snapshot()
		return s.ID == id && !s.Paused && s.State == core.StateLive
	}, func() string {
		return fmt.Sprintf("the core's share is %+v, want %s live and resumed", sh.Snapshot(), id)
	})
	video := track(t, view, id, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 15)
	if frames := sfutest.Frames(video.Packets()); !frames[0].KeyStart {
		t.Errorf("the viewer's first frame of %s is %+v, want a keyframe", id, frames[0])
	}
}

// TestDesktopPausedBeforeLive: the core pauses its share right after StartShare, before the share's pub PC has
// carried a frame. The server shows the share paused and keeps it past the 30 s start timeout; a viewer subscribes to
// it meanwhile. The resume brings it live, and the viewer gets its video.
func TestDesktopPausedBeforeLive(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(77))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)

	sh := app.start(defaultRoom(bob))
	if err := sh.SetPaused(waitCtx(t), true); err != nil {
		t.Fatalf("SetPaused(true) right after StartShare: %v", err)
	}
	pausedAt := time.Now()
	id := sh.Snapshot().ID
	waitShareStatus(bob, id, protocol.ShareStatusPaused)
	eventually(t, func() bool { return shareLogged(w, "share paused", id) },
		func() string { return "the server's log has no pause of the share" })
	bob.subscribe(want(id, true))

	time.Sleep(time.Until(pausedAt.Add(pausedStay)))
	checkPausedStarting(t, w, sh, bob, id)
	if hasTrack(bobView, id, webrtc.RTPCodecTypeVideo) {
		t.Error("the viewer got video of the share before its resume")
	}

	resumeToLive(t, sh, bob, bobView, id)
	bob.check()
}

// TestDesktopPausedAcrossRestart: the server restarts while the core's live share is paused. The core publishes it
// again (share.start{replaces}) and sends share.update{paused: true} for the new share, which sends no frame: the
// server shows it paused and keeps it past the 30 s start timeout, while the viewer's subscription has moved to it by
// replaces. The resume brings it live, and the viewer gets its video.
func TestDesktopPausedAcrossRestart(t *testing.T) {
	noLeaks(t)
	udp, tcp := freePort(t, "udp"), freePort(t, "tcp")
	w := newWorld(t, servertest.Options{Flags: []string{
		"--listen.ice-udp=127.0.0.1:" + strconv.Itoa(udp), "--listen.ice-tcp=127.0.0.1:" + strconv.Itoa(tcp),
	}})
	app := w.desktop(w.member("alice"), appEngine(78))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, _, _ := watchedShare(t, app, bob, bobView)
	if err := sh.SetPaused(waitCtx(t), true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	waitShareStatus(bob, snap.ID, protocol.ShareStatusPaused)

	// ---- the restart: the share is published again, paused ----
	w.srv.Restart(t)
	var next string
	eventually(t, func() bool {
		next = sh.Snapshot().ID
		return next != snap.ID && next != ""
	}, func() string { return fmt.Sprintf("the core's share is %+v, want a new id", sh.Snapshot()) })
	var info protocol.ShareInfo
	eventually(t, func() bool {
		var ok bool
		info, ok = bob.shareIn(next)
		return ok
	}, func() string {
		return fmt.Sprintf("bob's room.state %+v, want the new share %s", bob.roomState(), next)
	})
	republishedAt := time.Now() // no earlier than its share.start
	if info.Replaces != snap.ID {
		t.Errorf("the new share is %+v, want it to replace %s", info, snap.ID)
	}
	waitShareStatus(bob, next, protocol.ShareStatusPaused)
	eventually(t, func() bool { return shareLogged(w, "share paused", next) },
		func() string { return "the server's log has no pause of the new share" })

	time.Sleep(time.Until(republishedAt.Add(pausedStay)))
	checkPausedStarting(t, w, sh, bob, next)

	resumeToLive(t, sh, bob, bobView, next)
	bob.check()
}
