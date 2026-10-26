package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// hints.go against fakeServer (docs/m2/01-client-core.md §11.7's reconcile, §11.8): the hints' layer actions,
// ratectl's Ticks, pause and resume (the app's own, and one from room.state, with the share.update that room.state
// has yet to show, sent again by the actor when it has no ok reply, and concurrent calls kept in order), presets
// (SetPreset, and one from room.state), a republish that sends the pause again and keeps the preset, and the codec
// switch's check (per layer, older events, refusals). The codec switch itself needs a server that answers offers:
// the itests' D7 (desktop_hints_test.go).

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

// The full layer's targets of TestRateTickClock: the ramp's first step (⌈4 Mbit/s × 1.15⌉), then what congestion
// caps of 2 and 1 Mbit/s leave it beside the preview's 300 kbit/s ceiling, and a cap of 500 kbit/s beside the
// preview's floor of 150 kbit/s.
const (
	rampStepF = 4_600_000
	cap2MF    = 1_700_000
	cap1MF    = 700_000
	cap500kF  = 350_000
)

// congestionCap sends a congestion hint for s_1 that caps its video at bps.
func congestionCap(fs *fakeServer, bps int64) {
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonCongestion,
		Encodings: fakeParams("s_1").Encodings, MaxBitrate: bps})
}

// fCall waits for the engine's SetBitrate of the full layer to bps among the session's calls after the first n, and
// returns it.
func fCall(t *testing.T, sess *fake.Session, n, bps int) fake.Call {
	t.Helper()
	var got fake.Call
	eventually(t, func() bool {
		for _, c := range callsSince(sess, n, "bitrate") {
			if c.Layer == "f" && c.Bps == bps {
				got = c
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the engine's calls are %q, want bitrate f %d", callList(callsSince(sess, n, "bitrate")),
			bps)
	})
	return got
}

// checkSpacing fails the test when a change reached the engine less than ratectl's interval for a live encoder
// (MinInterval) after the one before it, and when the full layer's changes after the first n calls aren't want.
func checkSpacing(t *testing.T, sess *fake.Session, n int, want string, calls ...fake.Call) {
	t.Helper()
	for i := 1; i < len(calls); i++ {
		if gap := calls[i].At.Sub(calls[i-1].At); gap < ratectl.MinInterval {
			t.Errorf("f's change to %d bit/s reached the engine %v after its change to %d bit/s, want at least %v",
				calls[i].Bps, gap, calls[i-1].Bps, ratectl.MinInterval)
		}
	}
	var full []fake.Call
	for _, c := range callsSince(sess, n, "bitrate") {
		if c.Layer == "f" {
			full = append(full, c)
		}
	}
	if got := callList(full); got != want {
		t.Errorf("the full layer's changes are %q, want %q", got, want)
	}
}

// TestRateTickClock: ratectl and the engine see one clock (share.go's actor), so no change of a layer reaches the
// engine sooner than ratectl's interval after the one before it (fake.Call.At). Times are from the ramp's first step,
// which comes on a Tick, so the next Ticks fall due about every TickEvery from there:
//
//   - at 2.25 s a congestion hint lowers f at once (the step is 2 s old), and a second one wants f lower again, which
//     waits for f's guard until 4.25 s, so for the Tick due at 4.5 s;
//   - the actor is busy (on a call of its own) from 4.25 s to 5.25 s and takes that Tick 0.75 s late, and the engine
//     takes another 0.4 s for its change;
//   - a third hint wants f lower again, which waits for f's guard from that change.
//
// With the timer's value (when the Tick was due) as ratectl's now, ratectl dates the change 1.15 s before the engine
// has it, and the next one reached the engine 0.85–1.35 s after it. With the clock read but on a ticker's fixed
// schedule, the next Tick after the guard's 2 s came 1.85 s after the engine call. The timer re-armed after the Tick's
// engine calls puts it 2 s after them.
//
// The second hint's change must be dated between the Ticks (2.1–2.4 s), and the busy call must start before the Tick
// due at 4.5 s. When the test runs late for either, the run is tried again (three runs at most); the spacing is
// checked in every run.
func TestRateTickClock(t *testing.T) {
	for run := 1; ; run++ {
		if rateClockRun(t) {
			return
		}
		t.Logf("run %d: the hint or the busy call came late", run)
		if run == 3 {
			t.Skip("the hint or the busy call came late in every run: no Tick was taken late")
		}
	}
}

// rateClockRun is a run of TestRateTickClock: it reports whether the run went as planned (the first hint's change
// came between two Ticks, and the actor was busy when the Tick that let the second one through fell due).
func rateClockRun(t *testing.T) bool {
	t.Helper()
	slow := &slowEngine{}
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: func(o *Options) {
		fastTiming(nil)(o)
		slow.Engine = o.Engine.(*fake.Engine)
		o.Engine = slow
	}})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	roomState(fs, 3, shareAs(protocol.ShareStatusLive, protocol.PresetAuto))
	step := fCall(t, sess, n, rampStepF)
	at := func(d time.Duration) time.Time { return step.At.Add(d) }
	time.Sleep(time.Until(at(2250 * time.Millisecond)))
	congestionCap(fs, 2_000_000)
	first := fCall(t, sess, n, cap2MF)
	congestionCap(fs, 1_000_000)
	slow.holdNext("f", 400*time.Millisecond)
	time.Sleep(time.Until(at(4250 * time.Millisecond)))
	busyUntil := at(5250 * time.Millisecond)
	if err := sh.call(ctxFor(t), func() error {
		time.Sleep(time.Until(busyUntil))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	second := fCall(t, sess, n, cap1MF)
	congestionCap(fs, 500_000)
	third := fCall(t, sess, n, cap500kF)
	checkSpacing(t, sess, n, fmt.Sprintf("bitrate f %d, bitrate f %d, bitrate f %d, bitrate f %d", rampStepF,
		cap2MF, cap1MF, cap500kF), step, first, second, third)
	if err := sh.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	between := first.At.After(at(2100*time.Millisecond)) && first.At.Before(at(2400*time.Millisecond))
	return between && !second.At.Before(busyUntil)
}

// slowEngine is a fake.Engine whose sessions can hold a layer's next SetBitrate for a while before the engine gets
// it (fake.Call.At is when it does), as an engine call that takes that long.
type slowEngine struct {
	*fake.Engine
	mu    sync.Mutex
	layer string
	hold  time.Duration
}

// holdNext holds the next SetBitrate of layer for d.
func (e *slowEngine) holdNext(layer string, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.layer, e.hold = layer, d
}

// take returns how long to hold a SetBitrate of layer, once.
func (e *slowEngine) take(layer string) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if layer != e.layer {
		return 0
	}
	d := e.hold
	e.layer, e.hold = "", 0
	return d
}

func (e *slowEngine) Start(ctx context.Context, cfg media.Config) (media.Session, error) {
	sess, err := e.Engine.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &slowSession{Session: sess, e: e}, nil
}

// slowSession is a session of slowEngine.
type slowSession struct {
	media.Session
	e *slowEngine
}

func (s *slowSession) SetBitrate(layer string, bps int) error {
	if d := s.e.take(layer); d > 0 {
		time.Sleep(d)
	}
	return s.Session.SetBitrate(layer, bps)
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

// within waits until cond holds, for at most d.
func within(t *testing.T, d time.Duration, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", d, what())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serverStatus waits until the fake server's share s_1 has status, for at most d.
func serverStatus(t *testing.T, fs *fakeServer, d time.Duration, status protocol.ShareStatus) {
	t.Helper()
	within(t, d, func() bool {
		info, ok := fs.shareInfo("s_1")
		return ok && info.Status == status
	}, func() string {
		info, _ := fs.shareInfo("s_1")
		return fmt.Sprintf("the server's share is %+v, want it %s", info, status)
	})
}

// waitServerView waits until the share's actor has taken a room.state that shows v, and has no update of its own
// that room.state has yet to show.
func waitServerView(t *testing.T, sh *Share, v serverView) {
	t.Helper()
	view := func() (serverView, bool) {
		sh.pz.mu.Lock()
		defer sh.pz.mu.Unlock()
		return sh.pz.server, sh.pz.pending
	}
	eventually(t, func() bool { got, pending := view(); return got == v && !pending }, func() string {
		got, pending := view()
		return fmt.Sprintf("the share's view of the server is %d (pending %t), want %d", got, pending, v)
	})
}

// TestPauseOwed: a share.update{paused} the server refused goes again from the share's actor at most every
// resendEvery, with no room.state to prompt it (room.state comes only when something changes); the share stays
// paused meanwhile.
func TestPauseOwed(t *testing.T) {
	fs := pauseServer(t)
	fs.rejectOnce = map[protocol.MessageType]protocol.Error{
		protocol.MessageTypeShareUpdate: protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeRequest),
	}
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	n := len(sess.Calls())
	called := time.Now()
	err := sh.SetPaused(ctxFor(t), true)
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorCodeInternal {
		t.Errorf("SetPaused(true) = %v, want the server's internal", err)
	}
	if !sh.Snapshot().Paused {
		t.Error("the refused share.update undid the local pause")
	}
	settle()
	waitUpdates(t, fs, "s_1 paused=true") // within resendEvery: not yet
	serverStatus(t, fs, resendEvery+time.Second-time.Since(called), protocol.ShareStatusPaused)
	waitUpdates(t, fs, "s_1 paused=true, s_1 paused=true")
	waitCalls(t, sess, n, "paused true, audio_muted true", "paused", "audio_muted")
	settle()
	waitUpdates(t, fs, "s_1 paused=true, s_1 paused=true") // answered: nothing more
}

// TestPauseOwedAcrossDrop: a pause, and a resume, made while signaling is down reach the server from the share's
// actor at its first look after signaling is back (within a second here), with no room.state from the test: the
// resume's room.state, which still shows the server's old view, neither undoes them nor has to prompt the update.
func TestPauseOwedAcrossDrop(t *testing.T) {
	for _, pause := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%t", pause), func(t *testing.T) {
			fs := pauseServer(t)
			gate := make(chan struct{})
			var once sync.Once
			open := func() { once.Do(func() { close(gate) }) }
			t.Cleanup(open) // before the server's close: a hello it holds back gets its answer
			e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
			sh := startedShare(t, e)
			sess := sessionOf(t, e)
			from, to := protocol.ShareStatusLive, protocol.ShareStatusPaused
			if !pause { // the resume of a share that is paused, and that the server shows paused
				if err := sh.SetPaused(ctxFor(t), true); err != nil {
					t.Fatal(err)
				}
				waitUpdates(t, fs, "s_1 paused=true")
				from, to = to, from
			}
			roomState(fs, 3, shareAs(from, protocol.PresetAuto))
			waitServerView(t, sh, viewOf(!pause))
			n, m := len(sess.Calls()), len(updates(fs))

			// Signaling goes down, and stays down until the test lets the next hello through.
			fs.setHello(func(_ int, h protocol.Hello) fakeAnswer {
				<-gate
				return fakeAnswer{resumed: h.ResumeToken != ""}
			})
			fs.kill()
			eventually(t, func() bool { return sh.Snapshot().State == StateReconnecting },
				func() string { return fmt.Sprintf("the share is %+v, want reconnecting", sh.Snapshot()) })
			if err := sh.SetPaused(ctxFor(t), pause); err != nil {
				t.Fatalf("SetPaused(%t) while signaling is down = %v, want nil (owed)", pause, err)
			}
			open()
			e.waitOnline()
			serverStatus(t, fs, resendEvery+time.Second, to)
			settle()
			if got := updateList(updates(fs)[m:]); got != fmt.Sprintf("s_1 paused=%t", pause) {
				t.Errorf("the server got %q after the drop, want the owed update once", got)
			}
			if snap := sh.Snapshot(); snap.Paused != pause || snap.ID != "s_1" {
				t.Errorf("the share is %+v, want s_1 with paused %t", snap, pause)
			}
			want := "paused true, audio_muted true"
			if !pause {
				want = "audio_muted false, paused false"
			}
			waitCalls(t, sess, n, want, "paused", "audio_muted") // the resume's room.state undid nothing
		})
	}
}

// slowUpdateLog holds each "share update" record for up to 2 ms before its handler takes it, outside the handler's
// lock: the core logs a share.update just before it sends it, so concurrent calls get the time to overtake each
// other between their local transition and their request.
type slowUpdateLog struct{ slog.Handler }

func (h slowUpdateLog) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "share update" {
		time.Sleep(rand.N(2 * time.Millisecond)) //nolint:gosec // G404: test timing jitter, not security
	}
	return h.Handler.Handle(ctx, r)
}

func (h slowUpdateLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return slowUpdateLog{h.Handler.WithAttrs(attrs)}
}

func (h slowUpdateLog) WithGroup(name string) slog.Handler {
	return slowUpdateLog{h.Handler.WithGroup(name)}
}

// TestPauseConcurrent: SetPaused from two goroutines at once (03's hotkey and the pill) leaves the server with the
// pause the core has: the share.update requests reach it in the order of the local transitions.
func TestPauseConcurrent(t *testing.T) {
	fs := pauseServer(t)
	e := newEnv(t, fs, envOptions{tweak: func(o *Options) {
		fastTiming(nil)(o)
		o.Logger = slog.New(slowUpdateLog{o.Logger.Handler()})
	}})
	sh := startedShare(t, e)
	ctx := ctxFor(t)
	check := func() {
		t.Helper()
		paused := sh.Snapshot().Paused
		if info, _ := fs.shareInfo("s_1"); (info.Status == protocol.ShareStatusPaused) != paused {
			t.Errorf("the server's share is %+v while the core's is paused %t", info, paused)
		}
	}
	for range 5 {
		var wg sync.WaitGroup
		for _, p := range []bool{true, false} {
			wg.Go(func() {
				for range 20 {
					if err := sh.SetPaused(ctx, p); err != nil {
						t.Errorf("SetPaused(%t): %v", p, err)
						return
					}
				}
			})
		}
		wg.Wait()
		check()
	}
	settle()
	check() // and nothing sent later changes it
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

// askProfile records a SetProfile of profile for every layer, as the send loop's consumer.SetProfile does before
// Session.Reconfigure, once the engine events of the session's start have reached the actor.
func askProfile(t *testing.T, sh *Share, profile string) {
	t.Helper()
	eventually(t, func() bool {
		snap := sh.Snapshot()
		return len(snap.Layers) > 0 && !slices.ContainsFunc(snap.Layers, func(l LayerSnapshot) bool { return l.Profile == "" })
	}, func() string { return fmt.Sprintf("the share is %+v, want every layer's EventLayer", sh.Snapshot()) })
	sh.ask.set(sh.pump.layers, profile, sh.pump.events.Load())
}

// emitLayer has the session report a layer's EventLayer with profile.
func emitLayer(sess *fake.Session, rid, profile string) {
	sess.Emit(media.Event{Code: media.EventLayer, Layer: rid, Width: 640, Height: 360, FPS: 30,
		Encoder: fake.EncoderName, Profile: profile, LiveBitrate: true, NoOSIndicator: true})
}

// reoffersOf counts the core's re-offers with the profile the session runs.
func reoffersOf(e *testEnv) int {
	return strings.Count(e.logs.String(), `msg="offering the profile the session runs"`)
}

// TestCodecActualProfile: when the EventLayer after the send loop's SetProfile reports another profile than the one
// asked for, or the session refuses the profile, the core puts the profile the session runs first again (§8.6). The
// re-offer waits for the pending offer, as every change does.
func TestCodecActualProfile(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	askProfile(t, sh, "42e0")
	emitLayer(sess, "f", "6400")
	eventually(t, func() bool {
		return strings.Contains(e.logs.String(), `msg="the session runs another profile than asked for" asked=42e0 profile=6400`)
	}, func() string { return "the core never noticed that the session runs another profile than asked for" })
	sh.post(msgProfileFailed{profile: "42e0"})
	eventually(t, func() bool {
		return strings.Contains(e.logs.String(), `msg="the session did not switch profiles" asked=42e0 profile=6400`)
	}, func() string { return "the core never noticed the refused profile" })
	waitOffers(t, fs, "1/1")
}

// TestCodecCheckPerLayer: the switch's check is the main layer's. While f switches to the profile asked for, an
// EventLayer of q that stays in the old one (its encoder lacks the profile, 04 §9.7) neither answers f's ask nor
// re-offers, and the profile the session runs stays f's though q reports last.
func TestCodecCheckPerLayer(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sess := sessionOf(t, e)
	askProfile(t, sh, "42e0")
	emitLayer(sess, "q", "6400")
	emitLayer(sess, "f", "42e0")
	emitLayer(sess, "q", "6400")
	eventually(t, func() bool {
		snap := sh.Snapshot()
		return snap.layer("f").Profile == "42e0" && snap.layer("q").Profile == "6400"
	}, func() string { return fmt.Sprintf("the share is %+v, want f in 42e0 and q in 6400", sh.Snapshot()) })
	settle()
	if logs := e.logs.String(); strings.Contains(logs, "the session runs another profile than asked for") ||
		reoffersOf(e) != 0 {
		t.Error("q's EventLayer in the old profile had the core offer f's old profile again")
	}
	// A refusal now offers the profile f runs.
	sh.post(msgProfileFailed{profile: "6400"})
	eventually(t, func() bool {
		return strings.Contains(e.logs.String(), `msg="the session did not switch profiles" asked=6400 profile=42e0`)
	}, func() string { return "the profile the session runs isn't f's" })
}

// TestProfileRefusedTwice: the session refusing the same profile twice in a row has the core offer the profile it
// runs once, so the cycle answer → refusal → re-offer doesn't repeat; a new codec hint lets a refusal offer again.
func TestProfileRefusedTwice(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	sh.post(msgProfileFailed{profile: "42e0"})
	sh.post(msgProfileFailed{profile: "42e0"})
	eventually(t, func() bool { return strings.Contains(e.logs.String(), `msg="the session refused the profile again"`) },
		func() string { return "the second refusal never reached the core" })
	if n := reoffersOf(e); n != 1 {
		t.Errorf("two refusals of 42e0 made %d re-offers, want 1", n)
	}
	fs.send(protocol.MessageTypeQualityHint, protocol.QualityHint{ShareID: "s_1", Reason: protocol.HintReasonCodec,
		Codec: protocol.CodecH264ConstrainedBaseline})
	eventually(t, func() bool { return strings.Contains(e.logs.String(), `msg="codec switch"`) },
		func() string { return "the codec hint never reached the core" })
	sh.post(msgProfileFailed{profile: "42e0"})
	eventually(t, func() bool { return reoffersOf(e) == 2 },
		func() string {
			return fmt.Sprintf("%d re-offers after the codec hint's refusal, want 2", reoffersOf(e))
		})
}

// TestProfileAskStale: an ask answers only each layer's first EventLayer that the pump read after it; one read
// before is of an earlier re-init. A refusal forgets it.
func TestProfileAskStale(t *testing.T) {
	var a profileAsk
	a.set([]string{"f", "q"}, "42e0", 5)
	for _, c := range []struct {
		layer string
		seq   uint64
		want  string
	}{{"f", 5, ""}, {"f", 6, "42e0"}, {"f", 7, ""}, {"h", 8, ""}, {"q", 9, "42e0"}} {
		if got := a.take(c.layer, c.seq); got != c.want {
			t.Errorf("take(%s, %d) = %q, want %q", c.layer, c.seq, got, c.want)
		}
	}
	a.set([]string{"f"}, "6400", 10)
	a.clear()
	if got := a.take("f", 11); got != "" {
		t.Errorf("take after clear = %q, want nothing", got)
	}
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
