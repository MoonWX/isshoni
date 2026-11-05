package itest

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The client core's bearer authentication against the test's server (docs/m2/01-client-core.md §11.3, C15, README
// WA73), with a device linked through the real deviceauth.Manager (desktop_helpers_test.go's linkApp): a refused
// hello makes the core ask the manager for a new token once and connect anew, which publishes the share again; and a
// device removed on the Devices page ends the share, tells the manager, which unlinks, and stops the core unlinked.

// accessTokenLife is the server's access token lifetime (M2 02 §3: 900 s).
const accessTokenLife = 900 * time.Second

// TestDesktopForcedRefresh: alice's app shares and bob watches. The server's clock moves past the life of the app's
// access token, which the manager, on the real clock, still takes for fresh; then the app's socket is cut. Its
// resume hello carries that token and is answered unauthenticated, so the core asks the manager once for a forced
// refresh and dials anew with the new token: a new connection (not resumed), on which the share is published again
// with replaces, and bob's focus follows it. The manager stays linked, SessionEnded is never called, and the local
// share and its engine session go on.
func TestDesktopForcedRefresh(t *testing.T) {
	const within = 5 * time.Second
	noLeaks(t)
	clock := &testClock{}
	w := newWorld(t, servertest.Options{Deps: server.Deps{Now: clock.now}})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	app := w.recoveringDesktop(w.member("alice"), appEngine(73).engine, nil)
	sh, snap, _, _ := watched(t, app, bob, viewer)
	before := app.core.Status().ConnectionID
	old, _ := bob.shareIn(snap.ID)
	sess := app.engine.Session()
	tokens, forced := app.auth.counts()
	if tokens == 0 || forced != 0 {
		t.Fatalf("before the cut the core got %d tokens with %d forced refreshes, want some and none forced", tokens, forced)
	}
	dialed := app.dialed()

	// ---- the token expires on the server, the socket goes ----
	clock.advance(accessTokenLife + time.Minute)
	cut := time.Now()
	app.cutSocket()

	// ---- one forced refresh, a new connection, the share published again ----
	var next string
	eventually(t, func() bool {
		next = sh.Snapshot().ID
		return next != snap.ID && next != "" && app.core.Status().Conn == core.ConnOnline
	}, func() string {
		return fmt.Sprintf("the share is %+v and the core %+v, want a new share id online", sh.Snapshot(), app.core.Status())
	})
	t.Logf("the core published the share again %v after the cut", time.Since(cut).Round(time.Millisecond))
	if _, forced := app.auth.counts(); forced != 1 {
		t.Errorf("the core asked for %d forced refreshes, want one", forced)
	}
	if calls := app.auth.endedCalls(); len(calls) != 0 {
		t.Errorf("the core called SessionEnded %d times (first %s), want never: the forced refresh worked", len(calls),
			calls[0].code)
	}
	if n := app.dialed() - dialed; n < 2 {
		t.Errorf("the core dialed %d sockets after the cut, want the refused resume and the new connection", n)
	}
	st := app.core.Status()
	if st.ConnectionID == before || st.ConnectionID == "" || st.Stop != nil {
		t.Errorf("the core's status after the refresh is %+v, want online with a new connection id (it was %s)", st, before)
	}
	if ms := app.auth.mgr.State(); ms.Status != deviceauth.StatusLinked || ms.Notice != deviceauth.NoticeNone {
		t.Errorf("the manager is %s/%q after the forced refresh, want linked without a notice", ms.Status, ms.Notice)
	}
	info := bob.waitLive(next)
	if info.Replaces != snap.ID || info.ConnectionID != st.ConnectionID || info.UserID != old.UserID {
		t.Errorf("the new share is %+v, want the core's on its new connection %s, replacing %s", info, st.ConnectionID,
			snap.ID)
	}
	// The refused resume left the old connection detached, so the hub keeps its share (stalled) until the grace ends
	// (30 s, M1 01 §4.2); no other share may be there.
	for _, s := range bob.roomState().Shares {
		if s.ID != next && (s.ID != snap.ID || s.Status == protocol.ShareStatusLive) {
			t.Errorf("the room has the share %+v besides the new one, want at most the old one, not live", s)
		}
	}

	// ---- bob watches the new share; the local share is the same ----
	video := track(t, viewer, next, webrtc.RTPCodecTypeVideo)
	if flowing := video.Stats().First.Sub(cut); flowing > within {
		t.Errorf("bob got the new share's video %v after the cut, want within %v", flowing.Round(time.Millisecond), within)
	}
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, track(t, viewer, next, webrtc.RTPCodecTypeAudio), 50)
	bob.waitStatus(want(next, true))
	app.waitState(sh, core.StateLive)
	select {
	case <-sh.Done():
		t.Errorf("the share ended: %+v", sh.Snapshot().End)
	default:
	}
	if app.core.ActiveShare() != sh || app.engine.Session() != sess {
		t.Error("the active share or the engine session changed: a republish keeps both")
	}
	bob.check()
}

// TestDesktopDeviceRevoked: alice's app shares and bob watches; alice removes the app's device on the Devices page.
// The server closes the app's socket with session_revoked; the core ends the share (unlinked, with the code), then
// calls SessionEnded(session_revoked), whose refresh gets invalid_grant, so the manager deletes the credential and is
// unlinked with signed_out; and only then does Run return StopUnlinked, which Status reports. The core asked for no
// refresh of its own, and the share is gone from the room.
func TestDesktopDeviceRevoked(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	app := w.desktop(alice, appEngine(74))
	sh := app.start(defaultRoom(bob))
	snap := app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)
	app.auth.setBefore(func(protocol.ErrorCode) { // 01 §11.3: the share has ended when SessionEnded is called
		select {
		case <-sh.Done():
		default:
			t.Error("SessionEnded was called before the share ended")
		}
	})

	// ---- the removal ----
	device := app.auth.mgr.State().Device
	if device == nil {
		t.Fatal("the app's manager has no device")
	}
	rest(t, w.srv, restCall{method: http.MethodDelete, path: "/api/v1/me/devices/" + device.ID,
		cookie: sessionCookie(t, w.srv, alice.http.Jar)}).expect(t, "removing the device", http.StatusNoContent)

	// ---- Run stops unlinked, after SessionEnded ----
	err := app.waitRun()
	var se *core.StopError
	if !errors.As(err, &se) || se.Reason != core.StopUnlinked || se.Code != protocol.ErrorCodeSessionRevoked ||
		se.Err == nil || se.Err.Code != protocol.ErrorCodeSessionRevoked {
		t.Fatalf("Run = %v, want a StopError unlinked with session_revoked", err)
	}
	calls := app.auth.endedCalls() // SessionEnded is synchronous: it has returned before Run did
	if len(calls) != 1 || calls[0].code != protocol.ErrorCodeSessionRevoked {
		t.Fatalf("the core's SessionEnded calls by Run's return: %+v, want one with session_revoked", calls)
	}
	if s := calls[0].state; s.Status != deviceauth.StatusUnlinked || s.Notice != deviceauth.NoticeSignedOut || s.Device != nil {
		t.Errorf("SessionEnded left the manager %s/%q with device %v, want unlinked/signed_out", s.Status, s.Notice,
			s.Device != nil)
	}
	if end := sh.Snapshot().End; end == nil || *end != (core.EndInfo{Reason: core.EndUnlinked,
		Protocol: protocol.ErrorCodeSessionRevoked}) {
		t.Errorf("the share ended with %+v, want unlinked with session_revoked", end)
	}
	if st := app.core.Status(); st.Conn != core.ConnStopped || st.Stop == nil || st.Stop.Reason != core.StopUnlinked ||
		st.Share != nil {
		t.Errorf("the core's status is %+v, want stopped unlinked without a share", st)
	}
	eventually(t, func() bool { // the subscriber's goroutine may get it after Run returned
		return slices.ContainsFunc(app.statusEvents(), func(st core.Status) bool {
			return st.Stop != nil && st.Stop.Reason == core.StopUnlinked
		})
	}, func() string { return fmt.Sprintf("EventStatus %+v, want one with the stop", app.statusEvents()) })
	if _, forced := app.auth.counts(); forced != 0 {
		t.Errorf("the core asked for %d forced refreshes, want none: session_revoked is no refresh of the core's", forced)
	}
	if app.engine.Session() != nil {
		t.Error("the engine's session still runs after the stop")
	}

	// ---- the manager is unlinked; the room has no share ----
	if s := app.auth.mgr.State(); s.Status != deviceauth.StatusUnlinked || s.Notice != deviceauth.NoticeSignedOut {
		t.Errorf("the manager is %s/%q, want unlinked/signed_out", s.Status, s.Notice)
	}
	if _, err := app.auth.mgr.AccessToken(waitCtx(t), false); !errors.Is(err, deviceauth.ErrNotLinked) ||
		!relinkRequired(err) {
		t.Errorf("AccessToken after the stop: %v, want ErrNotLinked with RelinkRequired() true", err)
	}
	eventually(t, func() bool {
		_, shared := bob.shareIn(snap.ID)
		return !shared
	}, func() string { return fmt.Sprintf("bob's room.state %+v, want the app's share gone", bob.roomState()) })
	bob.check()
}
