package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// hints.go against fakeServer (docs/m2/01-client-core.md §11.7's reconcile, §11.8): the hints' layer actions,
// ratectl's Ticks, pause and resume (the app's own, and one from room.state, with the share.update that room.state
// has yet to show), presets (SetPreset, and one from room.state), and a republish that sends the pause again and
// keeps the preset. The codec switch needs a server that answers offers: the itests' D7 (desktop_hints_test.go).

// pauseServer is a fake server that offers share.pause.
func pauseServer(t *testing.T) *fakeServer {
	fs := newFakeServer(t)
	fs.features = []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureSharePause}
	return fs
}

// sessionOf returns the engine's running session.
func sessionOf(t *testing.T, e *testEnv) *fake.Session {
	t.Helper()
	sess := e.engine.Session()
	if sess == nil {
		t.Fatal("no engine session")
	}
	return sess
}

// callsSince returns the session's calls after the first n whose op is one of ops ("" ops: all but keyframes).
func callsSince(sess *fake.Session, n int, ops ...string) []fake.Call {
	var out []fake.Call
	for _, c := range sess.Calls()[n:] {
		if len(ops) == 0 && c.Op != "keyframe" || slices.Contains(ops, c.Op) {
			out = append(out, c)
		}
	}
	return out
}

// callList renders calls as "op layer bps on …", for comparisons and failures.
func callList(calls []fake.Call) string {
	var out []string
	for _, c := range calls {
		switch c.Op {
		case "bitrate":
			out = append(out, fmt.Sprintf("bitrate %s %d", c.Layer, c.Bps))
		case "active":
			out = append(out, fmt.Sprintf("active %s %t", c.Layer, c.On))
		case "paused", "audio_muted":
			out = append(out, fmt.Sprintf("%s %t", c.Op, c.On))
		case "reconfigure":
			out = append(out, fmt.Sprintf("reconfigure %+v", c.Reconf))
		default:
			out = append(out, c.Op)
		}
	}
	return strings.Join(out, ", ")
}

// waitCalls waits until the session's calls after the first n (filtered by ops) render as want.
func waitCalls(t *testing.T, sess *fake.Session, n int, want string, ops ...string) {
	t.Helper()
	eventually(t, func() bool { return callList(callsSince(sess, n, ops...)) == want }, func() string {
		return fmt.Sprintf("the engine's calls are %q, want %q", callList(callsSince(sess, n, ops...)), want)
	})
}

// updates returns the share.update requests the server got.
func updates(fs *fakeServer) []protocol.ShareUpdate {
	return received[protocol.ShareUpdate](fs, protocol.MessageTypeShareUpdate)
}

// updateList renders share.update requests as "id paused=… preset=…".
func updateList(us []protocol.ShareUpdate) string {
	var out []string
	for _, u := range us {
		s := u.ShareID
		if u.Paused != nil {
			s += fmt.Sprintf(" paused=%t", *u.Paused)
		}
		if u.Preset != "" {
			s += " preset=" + string(u.Preset)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// waitUpdates waits until the server's share.update requests render as want.
func waitUpdates(t *testing.T, fs *fakeServer, want string) {
	t.Helper()
	eventually(t, func() bool { return updateList(updates(fs)) == want }, func() string {
		return fmt.Sprintf("the server got the share.update requests %q, want %q", updateList(updates(fs)), want)
	})
}

// roomState sends a room.state of the lounge that lists the shares, as the hub does.
func roomState(fs *fakeServer, rev uint64, shares ...protocol.ShareInfo) {
	fs.send(protocol.MessageTypeRoomState, protocol.RoomState{RoomID: "lounge", Rev: rev,
		Participants: []protocol.ParticipantInfo{}, Shares: shares})
}

// shareAs is the core's share s_1 on c_1 as a room.state lists it, with a status and a preset.
func shareAs(status protocol.ShareStatus, preset protocol.Preset) protocol.ShareInfo {
	return protocol.ShareInfo{ID: "s_1", UserID: "u_alice", ConnectionID: "c_1", Kind: protocol.ShareKindWindow,
		Preset: preset, Status: status}
}

// fullOf returns the share's full layer as its snapshot has it, nil when it has none.
func fullOf(sh *Share) *LayerSnapshot {
	snap := sh.Snapshot()
	return snap.layer("f")
}

// settle gives the actor time to take what was sent (a room.state, a hint) when the test asserts that nothing
// happened.
func settle() { time.Sleep(300 * time.Millisecond) }

// TestHintLayerActions: a hint's encodings turn the full layer off (Session.SetLayerActive) and on again with its
// target set in the same step; the snapshot follows. A hint for another share changes nothing.
func TestHintLayerActions(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	enc := fakeParams("s_1").Encodings
	enc[0].Active = false
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonViewers,
		Encodings: enc})
	waitCalls(t, sess, n, "active f false")
	eventually(t, func() bool { f := fullOf(sh); return f != nil && !f.Active },
		func() string { return fmt.Sprintf("the share is %+v, want f inactive", sh.Snapshot()) })

	// Another share's hint: nothing.
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_9", Reason: protocol.HintReasonViewers,
		Encodings: fakeParams("s_9").Encodings})
	settle()
	waitCalls(t, sess, n, "active f false")

	enc[0].Active = true
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonViewers,
		Encodings: enc})
	want := fmt.Sprintf("active f false, bitrate f %d, active f true", ratectl.DefaultStartBps)
	waitCalls(t, sess, n, want)
	eventually(t, func() bool {
		f := fullOf(sh)
		return f != nil && f.Active && f.TargetBps == ratectl.DefaultStartBps
	}, func() string { return fmt.Sprintf("the share is %+v, want f active at its start", sh.Snapshot()) })

	// The same hint again (a resume sends them again) changes nothing.
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonViewers,
		Encodings: enc})
	settle()
	waitCalls(t, sess, n, want)
}

// TestRampTicks: once room.state shows the share live, ratectl's Ticks run, and its first ramp step reaches the
// engine RampDelay later; the snapshot's target follows. A congestion hint lowers the full layer at once.
func TestRampTicks(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	live := time.Now()
	const step = 4_600_000 // ⌈4 Mbit/s × 1.15⌉
	waitCalls(t, sess, n, fmt.Sprintf("bitrate f %d", step), "bitrate")
	if took := time.Since(live); took < ratectl.RampDelay-100*time.Millisecond {
		t.Errorf("the first ramp step came %v after live, want about %v", took, ratectl.RampDelay)
	}
	eventually(t, func() bool { f := fullOf(sh); return f != nil && f.TargetBps == step },
		func() string { return fmt.Sprintf("the share is %+v, want f at %d", sh.Snapshot(), step) })

	m := len(sess.Calls())
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonCongestion,
		Encodings: fakeParams("s_1").Encodings, MaxBitrate: 2_000_000})
	// The cap of 2 Mbit/s splits as q 300 kbit/s (its ceiling) and f 1.7 Mbit/s; the change waits for f's 2 s guard.
	waitCalls(t, sess, m, "bitrate f 1700000", "bitrate")
}

// TestSetPaused: a pause drops media at the pump, then pauses and mutes the engine, then sends share.update{paused:
// true}; a resume unmutes first, then un-pauses, then sends share.update{paused: false}. The snapshot follows.
func TestSetPaused(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	events, unsub := e.c.Subscribe()
	defer unsub()
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	if err := sh.SetPaused(ctxFor(t), true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	sh.pump.mu.Lock()
	dropping := sh.pump.paused
	sh.pump.mu.Unlock()
	if !dropping {
		t.Error("the pump forwards media after SetPaused(true) returned")
	}
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	waitUpdates(t, fs, "s_1 paused=true")
	if snap := sh.Snapshot(); !snap.Paused {
		t.Errorf("the share is %+v, want it paused", snap)
	}
	nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventShare && ev.Share.Paused })

	// Keyframe requests are swallowed while paused.
	m := len(sess.Calls())
	sh.pump.requestKey("f") // a PLI through the Publisher's Source
	settle()
	if calls := callsSince(sess, m, "keyframe"); len(calls) != 0 {
		t.Errorf("keyframe requests reached the paused engine: %s", callList(calls))
	}

	if err := sh.SetPaused(ctxFor(t), false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	waitCalls(t, sess, n, "paused true, audio_muted true, audio_muted false, paused false", "paused", "audio_muted")
	waitUpdates(t, fs, "s_1 paused=true, s_1 paused=false")
	if snap := sh.Snapshot(); snap.Paused {
		t.Errorf("the share is %+v, want it resumed", snap)
	}
	// The resume asks for a keyframe per active layer.
	eventually(t, func() bool { return len(callsSince(sess, m, "keyframe")) >= 2 }, func() string {
		return fmt.Sprintf("keyframe requests after the resume: %s, want one per layer", callList(callsSince(sess, m, "keyframe")))
	})
}

// TestSetPausedWithoutFeature: without the server's share.pause only the local part happens, and room.state's live
// doesn't undo it.
func TestSetPausedWithoutFeature(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	if err := sh.SetPaused(ctxFor(t), true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	settle()
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	if us := updates(fs); len(us) != 0 {
		t.Errorf("the server without share.pause got %s", updateList(us))
	}
	if !sh.Snapshot().Paused {
		t.Error("the share isn't paused")
	}
}

// TestReconcilePause: a pause and a resume from another connection (room.state's status) run the same local steps,
// without a share.update. A pause of the app's own that room.state has yet to show isn't undone by an older
// room.state; once room.state showed it, a later live is another connection's resume.
func TestReconcilePause(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	roomState(fs, 4, shareAs(protocol.ShareStatusPaused, protocol.PresetAuto))
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	eventually(t, func() bool { return sh.Snapshot().Paused },
		func() string { return fmt.Sprintf("the share is %+v, want it paused", sh.Snapshot()) })
	roomState(fs, 5, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	waitCalls(t, sess, n, "paused true, audio_muted true, audio_muted false, paused false", "paused", "audio_muted")
	if us := updates(fs); len(us) != 0 {
		t.Errorf("the core sent %s for a pause from room.state, want nothing", updateList(us))
	}

	// The app's own pause, then a room.state from before it.
	m := len(sess.Calls())
	if err := sh.SetPaused(ctxFor(t), true); err != nil {
		t.Fatal(err)
	}
	waitUpdates(t, fs, "s_1 paused=true")
	roomState(fs, 5, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	settle()
	waitCalls(t, sess, m, "paused true, audio_muted true", "paused", "audio_muted")
	// The one with the pause, then another connection's resume.
	roomState(fs, 6, shareAs(protocol.ShareStatusPaused, protocol.PresetAuto))
	roomState(fs, 7, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	waitCalls(t, sess, m, "paused true, audio_muted true, audio_muted false, paused false", "paused", "audio_muted")
	if us := updates(fs); len(us) != 1 {
		t.Errorf("the server got %s, want the app's pause alone", updateList(us))
	}
}

// TestPauseOwed: a share.update{paused} the server refused goes again when room.state still shows the share live
// (at most every resendEvery), and the share stays paused meanwhile.
func TestPauseOwed(t *testing.T) {
	fs := pauseServer(t)
	fs.rejectOnce = map[protocol.MessageType]protocol.Error{
		protocol.MessageTypeShareUpdate: protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeRequest),
	}
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	err := sh.SetPaused(ctxFor(t), true)
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorCodeInternal {
		t.Errorf("SetPaused(true) = %v, want the server's internal", err)
	}
	if !sh.Snapshot().Paused {
		t.Error("the refused share.update undid the local pause")
	}
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	settle()
	waitUpdates(t, fs, "s_1 paused=true") // within resendEvery: not yet
	time.Sleep(resendEvery)
	roomState(fs, 4, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	waitUpdates(t, fs, "s_1 paused=true, s_1 paused=true")
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	if info, _ := fs.shareInfo("s_1"); info.Status != protocol.ShareStatusPaused {
		t.Errorf("the server's share is %+v, want it paused", info)
	}
}

// TestSetPreset: share.update{preset}, then the reply's ShareParams: Session.Reconfigure with the preset and Text's
// frame rate on f. An older room.state doesn't undo it; a later preset from another connection is fetched with
// share.update and applied (Movie's Opus target).
func TestSetPreset(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	if err := sh.SetPreset(ctxFor(t), protocol.PresetText); err != nil {
		t.Fatalf("SetPreset(text): %v", err)
	}
	waitUpdates(t, fs, "s_1 preset=text")
	const text = "reconfigure {Profile: Preset:text Layers:[{RID:f MaxPixels:0 MaxFPS:15 Bitrate:0 Inactive:false}]}"
	waitCalls(t, sess, n, text, "reconfigure")
	if snap := sh.Snapshot(); snap.Preset != protocol.PresetText {
		t.Errorf("the share is %+v, want preset text", snap)
	}
	if err := sh.SetPreset(ctxFor(t), protocol.PresetText); err != nil {
		t.Fatalf("SetPreset(text) again: %v", err)
	}

	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto)) // from before the update
	settle()
	waitUpdates(t, fs, "s_1 preset=text")
	roomState(fs, 4, shareAs(protocol.ShareStatusLive, protocol.PresetText))
	roomState(fs, 5, shareAs(protocol.ShareStatusLive, protocol.PresetMovie)) // a web tab's
	waitUpdates(t, fs, "s_1 preset=text, s_1 preset=movie")
	waitCalls(t, sess, n, text+", reconfigure {Profile: Preset:movie Layers:[{RID:f MaxPixels:0 MaxFPS:30 Bitrate:0 "+
		"Inactive:false}]}", "reconfigure")
	if !slices.ContainsFunc(callsSince(sess, n, "bitrate"), func(c fake.Call) bool {
		return c.Layer == "audio" && c.Bps == 256_000
	}) {
		t.Errorf("the engine's bitrate calls are %q, want Movie's Opus target", callList(callsSince(sess, n, "bitrate")))
	}
	eventually(t, func() bool { return sh.Snapshot().Preset == protocol.PresetMovie },
		func() string { return fmt.Sprintf("the share is %+v, want preset movie", sh.Snapshot()) })

	if err := sh.SetPreset(ctxFor(t), "bogus"); err == nil {
		t.Error("SetPreset(bogus) = nil")
	}
}

// TestRepublishResendsPauseAndPreset: a share published again on a new connection keeps its preset (share.start
// carries it) and sends share.update{paused: true} again for the new id; the engine stays paused.
func TestRepublishResendsPauseAndPreset(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	if err := sh.SetPreset(ctxFor(t), protocol.PresetText); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetPaused(ctxFor(t), true); err != nil {
		t.Fatal(err)
	}
	waitUpdates(t, fs, "s_1 preset=text, s_1 paused=true")
	n := len(sess.Calls())

	fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} })
	fs.kill()
	waitUpdates(t, fs, "s_1 preset=text, s_1 paused=true, s_2 paused=true")
	starts := received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)
	if len(starts) != 2 || starts[1].Replaces != "s_1" || starts[1].Preset != protocol.PresetText {
		t.Errorf("share.start requests %+v, want the republish with replaces s_1 and preset text", starts)
	}
	if info, _ := fs.shareInfo("s_2"); info.Status != protocol.ShareStatusPaused || info.Preset != protocol.PresetText {
		t.Errorf("the server's new share is %+v, want it paused, preset text", info)
	}
	settle()
	if calls := callsSince(sess, n, "paused", "audio_muted", "reconfigure"); len(calls) != 0 {
		t.Errorf("the republish changed the engine: %s", callList(calls))
	}
	if snap := sh.Snapshot(); snap.ID != "s_2" || !snap.Paused || snap.Preset != protocol.PresetText {
		t.Errorf("the share is %+v, want s_2, paused, preset text", snap)
	}
	// The new share's room.state (paused) changes nothing; a resume goes to the new id.
	roomState(fs, 9, protocol.ShareInfo{ID: "s_2", UserID: "u_alice", ConnectionID: "c_2",
		Kind: protocol.ShareKindWindow, Preset: protocol.PresetText, Status: protocol.ShareStatusPaused})
	if err := sh.SetPaused(ctxFor(t), false); err != nil {
		t.Fatal(err)
	}
	waitUpdates(t, fs, "s_1 preset=text, s_1 paused=true, s_2 paused=true, s_2 paused=false")
	waitCalls(t, sess, n, "audio_muted false, paused false", "paused", "audio_muted")
}

// firstVideoProfile returns the H.264 profile key of the first payload type of an SDP's video m-section ("" when it
// has none).
func firstVideoProfile(sdp string) string {
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	pt := ""
	for _, l := range lines {
		if f := strings.Fields(l); len(f) > 3 && f[0] == "m=video" {
			pt = f[3]
			break
		}
	}
	if pt == "" {
		return ""
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "a=fmtp:"+pt+" ") {
			continue
		}
		if _, after, ok := strings.Cut(l, "profile-level-id="); ok && len(after) >= 4 {
			return after[:4]
		}
	}
	return ""
}

// TestCodecHintKeptAcrossRebuild: a codec hint puts its profile first for the next offer, which waits for the
// pending one (01 §9 rule 3); a rebuild's new Publisher offers it first (README WA38's hand-over: startPublisher
// keeps the codec-hint profile). The same hint again needs nothing.
func TestCodecHintKeptAcrossRebuild(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	startedShare(t, e)
	first := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)[0]
	if p := firstVideoProfile(first.SDP.Reveal()); p != "6400" {
		t.Fatalf("the first offer puts %q first, want 6400", p)
	}
	hint := protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonCodec, Codec: protocol.CodecH264ConstrainedBaseline}
	fs.send(protocol.MessageTypeQualityHint, hint)
	fs.send(protocol.MessageTypeQualityHint, hint)
	settle()
	waitOffers(t, fs, "1/1") // the re-offer waits for the first offer's answer
	// The server can't apply the first offer: a rebuild, whose new Publisher offers CB first.
	e1 := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
	e1.PC, e1.Gen, e1.Neg = protocol.PCKindPub, 1, 1
	fs.send(protocol.MessageTypeError, e1)
	waitOffers(t, fs, "1/1 2/1")
	rebuilt := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)[1]
	if p := firstVideoProfile(rebuilt.SDP.Reveal()); p != "42e0" {
		t.Errorf("the rebuilt PC's offer puts %q first, want 42e0 (the codec hint's)", p)
	}
	settle()
	waitOffers(t, fs, "1/1 2/1") // nothing more: the new offer carries the change
}

// TestCodecActualProfile: when the EventLayer after the send loop's SetProfile reports another profile than the one
// asked for, or the session refuses the profile, the core puts the profile the session runs first again (§8.6). The
// re-offer waits for the pending offer, as every change does.
func TestCodecActualProfile(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	sh.ask.set("42e0") // as the send loop does before Session.Reconfigure (consumer.SetProfile)
	sess.Emit(media.Event{Code: media.EventLayer, Layer: "f", Width: 640, Height: 360, FPS: 30,
		Encoder: fake.EncoderName, Profile: "6400", LiveBitrate: true, NoOSIndicator: true})
	eventually(t, func() bool {
		return strings.Contains(e.logs.String(), `msg="the session runs another profile than asked for" asked=42e0 profile=6400`)
	}, func() string { return "the core never noticed that the session runs another profile than asked for" })
	sh.post(msgProfileFailed{profile: "42e0"})
	eventually(t, func() bool {
		return strings.Contains(e.logs.String(), `msg="the session did not switch profiles" asked=42e0 profile=6400`)
	}, func() string { return "the core never noticed the refused profile" })
	waitOffers(t, fs, "1/1")
}

// TestPauseAfterEnd: SetPaused and SetPreset on an ended share are errors and change nothing.
func TestPauseAfterEnd(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	if err := sh.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	if err := sh.SetPaused(ctxFor(t), true); !errors.Is(err, errEnded) {
		t.Errorf("SetPaused after the end = %v, want errEnded", err)
	}
	if err := sh.SetPreset(ctxFor(t), protocol.PresetMovie); !errors.Is(err, errEnded) {
		t.Errorf("SetPreset after the end = %v, want errEnded", err)
	}
	if us := updates(fs); len(us) != 0 {
		t.Errorf("the server got %s after the end", updateList(us))
	}
}
