package share

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The controller's synctest suite (03 §22.1's share row): the held pick, Start's order and waits, Stop, every row of
// both mapping tables through the controller, pause, preset, Share sound, StopOther, the events.

// TestPickHeldForPickHold: the window is shown and owns the picker; the pick is held for PickHold, so a Start at
// 299 s works.
func TestPickHeldForPickHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		p := e.pick(t)
		if len(p.PickID) != 16 || !validHex(p.PickID) || p.Kind != protocol.ShareKindWindow || p.Label != "Minecraft" ||
			!p.Border || !slices.Equal(p.Confirm, []Sheet{SheetFirstShare}) {
			t.Fatalf("Picked %+v", p)
		}
		e.log.order(t, "window.Show(true)", "engine.Pick(owner=0x1234)")
		time.Sleep(299 * time.Second)
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); err != nil {
			t.Fatalf("Start at 299 s: %v", err)
		}
		if e.log.count("engine.Release") != 0 {
			t.Errorf("a started pick was released: %q", e.log.get())
		}
	})
}

func validHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}

// settle waits out one coalescing gap and lets the delivery that came due run (inside a synctest bubble).
func settle() {
	time.Sleep(StateEvery)
	synctest.Wait()
}

// TestPickExpires: an unused pick is released at PickHold, and a Start at 301 s is pick_expired.
func TestPickExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		p := e.pick(t)
		time.Sleep(PickHold - time.Second)
		if e.log.count("engine.Release") != 0 {
			t.Fatal("released before PickHold")
		}
		time.Sleep(2 * time.Second)
		if e.log.count("engine.Release(pick:1)") != 1 {
			t.Fatalf("not released at PickHold: %q", e.log.get())
		}
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
		if !errors.Is(err, ErrPickExpired) {
			t.Fatalf("Start at 301 s: %v", err)
		}
		if e.log.count("core.StartShare") != 0 || e.log.count("audio.OnShareStarted") != 0 {
			t.Errorf("an expired pick started: %q", e.log.get())
		}
	})
}

// TestPickHoldOption: Options.PickHold replaces the default.
func TestPickHoldOption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &callLog{}
		eng := &fakeEngine{log: log}
		c := New(Options{Engine: eng, PickHold: 120 * time.Second})
		defer c.Close()
		p, err := c.Pick(context.Background())
		if err != nil || p == nil {
			t.Fatal(p, err)
		}
		time.Sleep(121 * time.Second)
		if log.count("engine.Release(pick:1)") != 1 {
			t.Fatalf("not released at 120 s: %q", log.get())
		}
	})
}

// TestOnePickAtATime: a new Pick releases the pick held before; only the newest pickId starts.
func TestOnePickAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		first := e.pick(t)
		second := e.pick(t)
		if first.PickID == second.PickID {
			t.Fatal("the same pickId twice")
		}
		e.log.order(t, "engine.Pick(owner=0x1234)", "engine.Release(pick:1)", "engine.Pick(owner=0x1234)")
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: first.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrPickExpired) {
			t.Fatalf("Start with the replaced pick: %v", err)
		}
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: second.PickID, RoomID: "lounge"}); err != nil {
			t.Fatalf("Start with the new pick: %v", err)
		}
		if got := e.client.current().Snapshot(); got.RoomID != "lounge" {
			t.Errorf("snapshot %+v", got)
		}
		if r := e.client.started[0]; r.Source.ID != "pick:2" {
			t.Errorf("started %+v", r)
		}
	})
}

// TestPickCancelled: a closed picker is null, nothing is held, and the picker's view goes away.
func TestPickCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.eng.set(func(e *fakeEngine) { e.err = &media.Error{Code: media.CodeCancelled, Op: "pick"} })
		p, err := e.ctl.Pick(context.Background())
		if p != nil || err != nil {
			t.Fatalf("Pick = %+v, %v", p, err)
		}
		synctest.Wait()
		settle()
		if got := e.rec.states(); !slices.Equal(got, []string{"picking", "none"}) {
			t.Errorf("states %q", got)
		}
		if e.ctl.Current() != nil {
			t.Error("a view after a cancelled pick")
		}
	})
}

// TestPickWhilePicking: a second Pick while the picker is open is engine busy (pick_busy) and doesn't touch the
// open picker; ending the picker's ctx cancels it.
func TestPickWhilePicking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.eng.set(func(e *fakeEngine) { e.block = true })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			p, err := e.ctl.Pick(ctx)
			if p != nil {
				err = errors.New("a pick")
			}
			done <- err
		}()
		synctest.Wait()
		if v := e.ctl.Current(); v == nil || v.State != StatePicking {
			t.Fatalf("view while picking %+v", v)
		}
		_, err := e.ctl.Pick(context.Background())
		var me *media.Error
		if !errors.As(err, &me) || me.Code != media.CodeBusy || me.Reason != "pick_busy" {
			t.Fatalf("second Pick: %v", err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("cancelled Pick: %v", err)
		}
	})
}

// TestPickSelfWindow: a pick of an isshoni window is the engine's invalid/self_window; nothing is held.
func TestPickSelfWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.eng.set(func(e *fakeEngine) {
			e.err = &media.Error{Code: media.CodeInvalid, Op: "pick", Reason: "self_window"}
		})
		p, err := e.ctl.Pick(context.Background())
		var me *media.Error
		if p != nil || !errors.As(err, &me) || me.Code != media.CodeInvalid || me.Reason != "self_window" {
			t.Fatalf("Pick = %+v, %v", p, err)
		}
		e.ctl.mu.Lock()
		held := e.ctl.pick
		e.ctl.mu.Unlock()
		if held != nil {
			t.Error("a self pick is held")
		}
		if e.log.count("engine.Release") != 0 {
			t.Error("the controller released a pick the engine already released")
		}
	})
}

// TestPickWithoutEngine: no engine, no picker: unsupported_os.
func TestPickWithoutEngine(t *testing.T) {
	c := New(Options{})
	defer c.Close()
	if _, err := c.Pick(context.Background()); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Fatalf("Pick: %v", err)
	}
}

// TestReleaseOnBackOut: Release drops the held pick and releases it in the engine; a later Start is pick_expired;
// another id changes nothing.
func TestReleaseOnBackOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		p := e.pick(t)
		e.ctl.Release("0123456789abcdef")
		if e.log.count("engine.Release") != 0 {
			t.Fatal("another id released the pick")
		}
		e.ctl.Release(p.PickID)
		if e.log.count("engine.Release(pick:1)") != 1 {
			t.Fatalf("calls %q", e.log.get())
		}
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrPickExpired) {
			t.Fatalf("Start after Release: %v", err)
		}
		time.Sleep(PickHold + time.Second)
		if e.log.count("engine.Release") != 1 {
			t.Errorf("released twice: %q", e.log.get())
		}
	})
}

// TestSheets: the first-share sheet once per install, the DND tip for the first screen share; a started share
// records them as shown.
func TestSheets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.eng.set(func(e *fakeEngine) { e.src.Kind, e.src.PID = media.SourceScreen, 0 })
		p := e.pick(t)
		if !slices.Equal(p.Confirm, []Sheet{SheetFirstShare, SheetDND}) || p.Kind != protocol.ShareKindScreen {
			t.Fatalf("Picked %+v", p)
		}
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); err != nil {
			t.Fatal(err)
		}
		tips := e.set.Get().Tips
		if !tips.FirstShareShown || !tips.DNDShown {
			t.Errorf("tips %+v", tips)
		}
		e.ctl.Stop(context.Background(), CauseUser)
		if p := e.pick(t); len(p.Confirm) != 0 {
			t.Errorf("sheets again: %v", p.Confirm)
		}
	})
}

// TestStartOrder: C2 ready, then 05's OnShareStarted(pid, withAudio), then 01's StartShare with the pick, the
// preset, audio and Label "", then the server-stats wish (off) on the share's client; the controller sends no room or
// share message of its own (its Client has none: the join before share.start is 01's, itests D1 and D2). The preset
// becomes serverPrefs[origin].preset.
func TestStartOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		p := e.pick(t)
		e.log.reset()
		v, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge",
			Preset: protocol.PresetGame, WithAudio: true})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"audio.OnShareStarted(4120,true)",
			`core.StartShare(room=lounge,source=pick:1,preset=game,audio=true,label="")`,
			"core.WatchServerStats(false)",
		}
		if got := e.log.get(); !slices.Equal(got, want) {
			t.Fatalf("calls %q, want %q", got, want)
		}
		if v.State != StateStarting || v.RoomID != "lounge" || v.Kind != protocol.ShareKindWindow ||
			v.Label != "Minecraft" || v.Preset != protocol.PresetGame || !v.WithAudio || !v.AudioOn || !v.Border ||
			v.Pill || v.ShareID != "s_q7m2k9x4w3bd8c1a" || v.ConnectionID != "c_k3v9q2m7xw4pa8d1" {
			t.Errorf("view %+v", v)
		}
		if got := e.set.Get().ServerPrefs[testOrigin].Preset; got != protocol.PresetGame {
			t.Errorf("serverPrefs preset %q", got)
		}
		if roomID, live := e.ctl.Live(); !live || roomID != "lounge" {
			t.Errorf("Live = %q, %v", roomID, live)
		}
		// One share at a time.
		if _, err := e.ctl.Pick(context.Background()); !errors.Is(err, ErrAlreadySharing) {
			t.Errorf("Pick while sharing: %v", err)
		}
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrAlreadySharing) {
			t.Errorf("Start while sharing: %v", err)
		}
	})
}

// TestStartShareFails: OnShareStopped follows the OnShareStarted; the view is failed with the error; the pick stays
// held, so the SPA may start it again.
func TestStartShareFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.client.set(func(c *fakeClient) { c.startErr = &protocol.Error{Code: protocol.ErrorCodeRoomFull} })
		p := e.pick(t)
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge", WithAudio: true})
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeRoomFull {
			t.Fatalf("Start: %v", err)
		}
		e.log.order(t, "audio.OnShareStarted(4120,true)", "core.StartShare", "audio.OnShareStopped")
		v := e.ctl.Current()
		if v == nil || v.State != StateFailed || v.EndReason != EndError || !errors.As(v.Err, &pe) {
			t.Fatalf("view %+v", v)
		}
		if _, live := e.ctl.Live(); live {
			t.Error("live after a failed start")
		}
		if e.client.subscribers() != 0 {
			t.Error("still subscribed after a failed start")
		}
		e.client.set(func(c *fakeClient) { c.startErr = nil })
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); err != nil {
			t.Fatalf("Start again with the kept pick: %v", err)
		}
	})
}

// TestStartShareFailsPickGone: when the engine says the pick is gone (source_gone, pick_expired), it is dropped and
// released; 01's ErrBusy is already_sharing.
func TestStartShareFailsPickGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.client.set(func(c *fakeClient) {
			c.startErr = &media.Error{Code: media.CodeSourceGone, Op: "start", Reason: "pick_expired"}
		})
		p := e.pick(t)
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
		var me *media.Error
		if !errors.As(err, &me) || me.Reason != "pick_expired" {
			t.Fatalf("Start: %v", err)
		}
		if e.log.count("engine.Release(pick:1)") != 1 {
			t.Errorf("the gone pick wasn't released: %q", e.log.get())
		}
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrPickExpired) {
			t.Errorf("Start again: %v", err)
		}
		e.client.set(func(c *fakeClient) { c.startErr = core.ErrBusy })
		p = e.pick(t)
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrAlreadySharing) {
			t.Errorf("Start with the core busy: %v", err)
		}
		e.client.set(func(c *fakeClient) { c.startErr = core.ErrNotReady })
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(
			err, ErrPublisherOffline) {
			t.Errorf("Start with the core offline: %v", err)
		}
	})
}

// TestPublisherOfflineAt10s: C2 not online within 10 s is publisher_offline, exactly then; nothing else ran, and the
// pick stays held.
func TestPublisherOfflineAt10s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.pub.set(func(p *fakePublisher) { p.block = true })
		p := e.pick(t)
		start := time.Now()
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
		if !errors.Is(err, ErrPublisherOffline) {
			t.Fatalf("Start: %v", err)
		}
		if waited := time.Since(start); waited != ReadyTimeout {
			t.Errorf("waited %v, want %v", waited, ReadyTimeout)
		}
		if e.log.count("audio.") != 0 || e.log.count("core.") != 0 {
			t.Errorf("calls %q", e.log.get())
		}
		e.pub.set(func(p *fakePublisher) { p.block = false })
		if _, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); err != nil {
			t.Fatalf("Start once C2 is online: %v", err)
		}
	})
}

// TestStartShareTimeout: StartShare gets its own 10 s after the wait for C2.
func TestStartShareTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.client.set(func(c *fakeClient) { c.startBlock = true })
		p := e.pick(t)
		start := time.Now()
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start: %v", err)
		}
		if waited := time.Since(start); waited != StartShareTimeout {
			t.Errorf("waited %v, want %v", waited, StartShareTimeout)
		}
		e.log.order(t, "audio.OnShareStarted(4120,false)", "core.StartShare", "audio.OnShareStopped")
	})
}

// TestStartNotLinked: an active server that isn't linked fails at once with 02's ErrNotLinked (not_linked).
func TestStartNotLinked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.pub.set(func(p *fakePublisher) { p.err = deviceauth.ErrNotLinked })
		p := e.pick(t)
		start := time.Now()
		_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
		if !errors.Is(err, deviceauth.ErrNotLinked) || time.Since(start) != 0 {
			t.Fatalf("Start: %v after %v", err, time.Since(start))
		}
	})
}

// TestStartWithoutPublisher: no C2 at all is publisher_offline.
func TestStartWithoutPublisher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &callLog{}
		c := New(Options{Engine: &fakeEngine{log: log}})
		defer c.Close()
		p, _ := c.Pick(context.Background())
		if _, err := c.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"}); !errors.Is(err,
			ErrPublisherOffline) {
			t.Fatalf("Start: %v", err)
		}
		if _, err := c.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge",
			Preset: "fast"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Start with an unknown preset: %v", err)
		}
	})
}

// TestStopOrder: Stop calls 01's Share.Stop, then 05's OnShareStopped, and returns after both; the view is ended with
// endReason user. Stop is idempotent.
func TestStopOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		goLive(s)
		synctest.Wait()
		e.log.reset()
		e.ctl.Stop(context.Background(), CauseUser)
		if got, want := e.log.get(), []string{"core.Share.Stop", "audio.OnShareStopped"}; !slices.Equal(got, want) {
			t.Fatalf("calls %q, want %q", got, want)
		}
		v := e.ctl.Current()
		if v == nil || v.State != StateEnded || v.EndReason != EndUser || v.Err != nil || v.Pill {
			t.Fatalf("view %+v", v)
		}
		if _, live := e.ctl.Live(); live {
			t.Error("live after Stop")
		}
		e.ctl.Stop(context.Background(), CauseTray)
		if e.log.count("core.Share.Stop") != 1 || e.log.count("audio.OnShareStopped") != 1 {
			t.Errorf("a second Stop did more: %q", e.log.get())
		}
		synctest.Wait()
		if e.client.subscribers() != 0 {
			t.Error("still subscribed after the end")
		}
		e.ctl.Stop(context.Background(), CauseUser) // no share: nothing
	})
}

// TestStopCauses: the controller's cause is the end reason of a share 01 ends with reason user.
func TestStopCauses(t *testing.T) {
	for cause, want := range map[Cause]EndReason{CauseUser: EndUser, CauseHotkey: EndHotkey, CauseTray: EndTray,
		CauseSystem: EndSystem, "": EndUser} {
		t.Run(string(want)+"/"+string(cause), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				goLive(e.start(t, "lounge", false))
				e.ctl.Stop(context.Background(), cause)
				if v := e.ctl.Current(); v == nil || v.EndReason != want {
					t.Fatalf("view %+v", v)
				}
			})
		})
	}
}

// TestServerEndRunsLocalStepsOnce: a share the server or the core ended runs the same local steps, once.
func TestServerEndRunsLocalStepsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		goLive(s)
		synctest.Wait()
		s.end(core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonStopped})
		synctest.Wait()
		if n := e.log.count("audio.OnShareStopped"); n != 1 {
			t.Fatalf("OnShareStopped %d times", n)
		}
		v := e.ctl.Current()
		if v == nil || v.State != StateEnded || v.EndReason != EndElsewhere {
			t.Fatalf("view %+v", v)
		}
		e.ctl.Stop(context.Background(), CauseUser)
		if e.log.count("audio.OnShareStopped") != 1 || e.log.count("core.Share.Stop") != 0 {
			t.Errorf("Stop after the end: %q", e.log.get())
		}
		settle()
		if lv := e.rec.lastView(); lv == nil || lv.EndReason != EndElsewhere {
			t.Errorf("last share.state %+v", lv)
		}
	})
}

// TestEndReasonTable is 03 §10.1's end-reason table through the controller: 01's EndInfo → state and endReason, a
// failed share's error as engine.<code> with 01's detail token as its Reason (the bridge's params.reason: a live
// share that ends with encoder_lost and device_removed gets 03 §10.5's "Your graphics driver restarted" row, not the
// code's) or the protocol code.
func TestEndReasonTable(t *testing.T) {
	for _, c := range []struct {
		name   string
		info   core.EndInfo
		state  State
		reason EndReason
		media  media.ErrorCode
		token  string // the media error's Reason
		wire   protocol.ErrorCode
	}{
		{"user", core.EndInfo{Reason: core.EndUser, ByThisApp: true}, StateEnded, EndUser, "", "", ""},
		{"source-gone", core.EndInfo{Reason: core.EndSourceGone, Media: media.CodeSourceGone}, StateEnded,
			EndSourceGone, "", "", ""},
		{"elsewhere stopped", core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonStopped}, StateEnded,
			EndElsewhere, "", "", ""},
		{"elsewhere left", core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonLeft}, StateEnded,
			EndElsewhere, "", "", ""},
		{"stopped by this app", core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonStopped,
			ByThisApp: true}, StateEnded, EndServer, "", "", ""},
		{"media_timeout", core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonMediaTimeout}, StateEnded,
			EndServer, "", "", ""},
		{"room_closed", core.EndInfo{Reason: core.EndServer, Server: protocol.EndReasonRoomClosed}, StateEnded,
			EndServer, "", "", ""},
		{"server-unreachable", core.EndInfo{Reason: core.EndServerUnreachable}, StateEnded, EndServerUnreachable, "",
			"", ""},
		{"unlinked", core.EndInfo{Reason: core.EndUnlinked, Protocol: protocol.ErrorCodeSessionRevoked}, StateEnded,
			EndUnlinked, "", "", ""},
		{"engine error", core.EndInfo{Reason: core.EndError, Media: media.CodeEncoderLost}, StateFailed, EndError,
			media.CodeEncoderLost, "", ""},
		{"device removed", core.EndInfo{Reason: core.EndError, Media: media.CodeEncoderLost,
			MediaReason: "device_removed"}, StateFailed, EndError, media.CodeEncoderLost, "device_removed", ""},
		{"protocol error", core.EndInfo{Reason: core.EndError, Protocol: protocol.ErrorCodeReplaced}, StateFailed,
			EndError, "", "", protocol.ErrorCodeReplaced},
		{"error without a code", core.EndInfo{Reason: core.EndError}, StateFailed, EndError, media.CodeInternal, "",
			""},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				s := e.start(t, "lounge", true)
				goLive(s)
				s.end(c.info)
				synctest.Wait()
				v := e.ctl.Current()
				if v == nil || v.State != c.state || v.EndReason != c.reason {
					t.Fatalf("view %+v", v)
				}
				var (
					me *media.Error
					pe *protocol.Error
				)
				switch {
				case c.media != "":
					if !errors.As(v.Err, &me) || me.Code != c.media || me.Reason != c.token {
						t.Errorf("error %v, want media %s with reason %q", v.Err, c.media, c.token)
					}
				case c.wire != "":
					if !errors.As(v.Err, &pe) || pe.Code != c.wire {
						t.Errorf("error %v, want wire %s", v.Err, c.wire)
					}
				default:
					if v.Err != nil {
						t.Errorf("error %v", v.Err)
					}
				}
				if e.log.count("audio.OnShareStopped") != 1 {
					t.Errorf("local steps: %q", e.log.get())
				}
			})
		})
	}
}

// TestStateTable is 03 §10.1's state table through the controller, with liveSince stamped at the first live and
// kept across a republish (replaces: a new share id), the border from the engine once live, and the pill.
func TestStateTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		check := func(want State) *View {
			t.Helper()
			synctest.Wait()
			v := e.ctl.Current()
			if v == nil || v.State != want {
				t.Fatalf("view %+v, want %s", v, want)
			}
			return v
		}
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateJoining })
		if v := check(StateStarting); !v.LiveSince.IsZero() {
			t.Error("liveSince before live")
		}
		time.Sleep(time.Second)
		s.update(func(snap *core.ShareSnapshot) {
			snap.State, snap.NoOSIndicator = core.StateLive, true
			snap.Upload = ratectl.UploadHint{Limited: true, ApproxHeight: 720}
		})
		v := check(StateLive)
		liveSince := v.LiveSince
		if !liveSince.Equal(time.Now()) || v.Border || !v.Pill || v.Hint != (Hint{Kind: HintUploadLimited,
			ApproxHeight: 720}) {
			t.Fatalf("live view %+v", v)
		}
		s.update(func(snap *core.ShareSnapshot) { snap.Paused, snap.Upload = true, ratectl.UploadHint{} })
		check(StatePaused)
		time.Sleep(5 * time.Second)
		s.update(func(snap *core.ShareSnapshot) {
			snap.State, snap.Paused, snap.Overloaded = core.StateReconnecting,
				false, true
		})
		if v := check(StatePaused); v.Hint.Kind != HintCPULimited { // reconnecting shows after ReconnectingAfter
			t.Errorf("hint %+v", v.Hint)
		}
		time.Sleep(ReconnectingAfter)
		check(StateReconnecting)
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateUnreachable })
		check(StateUnreachable)
		s.update(func(snap *core.ShareSnapshot) {
			snap.State, snap.ID, snap.Minimized, snap.Overloaded = core.StateLive, "s_newid00000000001", true, false
		})
		v = check(StateLive)
		if v.ShareID != "s_newid00000000001" || !v.LiveSince.Equal(liveSince) || !v.Minimized {
			t.Errorf("after replaces %+v (liveSince %v)", v, liveSince)
		}
		goLive(s)
		v = check(StateLive)
		if v.Watching != 1 || v.Encoder != "nvenc" || !v.HW || len(v.Layers) != 2 || v.Layers[0].SentBps != 7_712_000 ||
			v.Server != nil {
			t.Errorf("details %+v", v)
		}
	})
}

// TestReconnectingDebounced: a reconnecting that 01 cures within ReconnectingAfter (a resume, an ICE restart) never
// shows; a longer one shows once it has lasted ReconnectingAfter, also without a further snapshot; unreachable shows
// at once.
func TestReconnectingDebounced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		goLive(s)
		synctest.Wait()
		time.Sleep(time.Second)
		e.rec.reset()
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateReconnecting })
		synctest.Wait()
		time.Sleep(ReconnectingAfter - 500*time.Millisecond)
		if v := e.ctl.Current(); v.State != StateLive {
			t.Fatalf("a short drop shows %+v", v)
		}
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateLive })
		synctest.Wait()
		time.Sleep(5 * time.Second)
		if got := e.rec.states(); slices.Contains(got, string(StateReconnecting)) {
			t.Fatalf("states %q", got)
		}

		start := time.Now()
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateReconnecting })
		synctest.Wait()
		for e.ctl.Current().State != StateReconnecting {
			time.Sleep(10 * time.Millisecond)
			if time.Since(start) > time.Minute {
				t.Fatal("never reconnecting")
			}
		}
		if waited := time.Since(start); waited < ReconnectingAfter || waited > ReconnectingAfter+20*time.Millisecond {
			t.Errorf("reconnecting after %v", waited)
		}
		settle()
		if lv := e.rec.lastView(); lv == nil || lv.State != StateReconnecting {
			t.Errorf("last share.state %+v", lv)
		}
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateUnreachable })
		synctest.Wait()
		if v := e.ctl.Current(); v.State != StateUnreachable {
			t.Errorf("unreachable %+v", v)
		}
		s.update(func(snap *core.ShareSnapshot) { snap.State = core.StateReconnecting })
		synctest.Wait()
		if v := e.ctl.Current(); v.State != StateUnreachable {
			t.Errorf("unreachable → short reconnecting shows %+v", v)
		}
		s.end(core.EndInfo{Reason: core.EndServerUnreachable})
		synctest.Wait()
		time.Sleep(ReconnectingAfter)
		if v := e.ctl.Current(); v.State != StateEnded || v.EndReason != EndServerUnreachable {
			t.Errorf("ended %+v", v)
		}
	})
}

// TestStateOf is the state table as a function, the ended rows included.
func TestStateOf(t *testing.T) {
	for _, c := range []struct {
		snap core.ShareSnapshot
		want State
	}{
		{core.ShareSnapshot{State: core.StateJoining}, StateStarting},
		{core.ShareSnapshot{State: core.StateStarting}, StateStarting},
		{core.ShareSnapshot{State: core.StateStarting, Paused: true}, StateStarting},
		{core.ShareSnapshot{State: core.StateLive}, StateLive},
		{core.ShareSnapshot{State: core.StateLive, Paused: true}, StatePaused},
		{core.ShareSnapshot{State: core.StateReconnecting}, StateReconnecting},
		{core.ShareSnapshot{State: core.StateUnreachable}, StateUnreachable},
		{core.ShareSnapshot{State: core.StateEnded, End: &core.EndInfo{Reason: core.EndUser}}, StateEnded},
		{core.ShareSnapshot{State: core.StateEnded, End: &core.EndInfo{Reason: core.EndError}}, StateFailed},
	} {
		if got := stateOf(&c.snap); got != c.want {
			t.Errorf("stateOf(%+v) = %s, want %s", c.snap, got, c.want)
		}
	}
}

// TestPauseOrder: Share.SetPaused, then 05's SetPaused once it succeeded; feature_disabled without share.pause;
// not_sharing without a share; a refusal skips 05.
func TestPauseOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		if err := e.ctl.SetPaused(context.Background(), true); !errors.Is(err, ErrNotSharing) {
			t.Fatalf("SetPaused without a share: %v", err)
		}
		s := e.start(t, "lounge", true)
		goLive(s)
		e.log.reset()
		if err := e.ctl.SetPaused(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if got, want := e.log.get(), []string{"core.Share.SetPaused(true)", "audio.SetPaused(true)"}; !slices.Equal(got,
			want) {
			t.Fatalf("calls %q, want %q", got, want)
		}
		if v := e.ctl.Current(); v.State != StatePaused {
			t.Errorf("view %+v", v)
		}
		e.log.reset()
		s.mu.Lock()
		s.pausedErr = &protocol.Error{Code: protocol.ErrorCodeForbidden}
		s.mu.Unlock()
		var pe *protocol.Error
		if err := e.ctl.SetPaused(context.Background(), false); !errors.As(err, &pe) {
			t.Fatalf("refused SetPaused: %v", err)
		}
		if e.log.count("audio.SetPaused") != 0 {
			t.Errorf("05 paused after a refusal: %q", e.log.get())
		}
		e.client.set(func(c *fakeClient) { c.status.Features = nil })
		e.log.reset()
		if err := e.ctl.SetPaused(context.Background(), false); !errors.Is(err, ErrFeatureDisabled) {
			t.Fatalf("SetPaused without share.pause: %v", err)
		}
		if len(e.log.get()) != 0 {
			t.Errorf("calls without the feature: %q", e.log.get())
		}
		s.end(core.EndInfo{Reason: core.EndSourceGone})
		synctest.Wait()
		if err := e.ctl.SetPaused(context.Background(), false); !errors.Is(err, ErrNotSharing) {
			t.Errorf("SetPaused after the end: %v", err)
		}
	})
}

// TestSetAudioEnabled calls only 05's SetShareSound, and is a no-op for a share started without sound.
func TestSetAudioEnabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		if err := e.ctl.SetAudioEnabled(false); !errors.Is(err, ErrNotSharing) {
			t.Fatalf("without a share: %v", err)
		}
		goLive(e.start(t, "lounge", true))
		e.log.reset()
		if err := e.ctl.SetAudioEnabled(false); err != nil {
			t.Fatal(err)
		}
		if got := e.log.get(); !slices.Equal(got, []string{"audio.SetShareSound(false)"}) {
			t.Fatalf("calls %q", got)
		}
		if v := e.ctl.Current(); v.AudioOn || !v.WithAudio {
			t.Errorf("view %+v", v)
		}
		e.audio.soundErr = &media.Error{Code: media.CodeAudioNotRunning}
		if err := e.ctl.SetAudioEnabled(true); !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("05's error: %v", err)
		}
		e.audio.soundErr = nil
		e.ctl.Stop(context.Background(), CauseUser)

		goLive(e.start(t, "lounge", false))
		e.log.reset()
		if err := e.ctl.SetAudioEnabled(true); err != nil || len(e.log.get()) != 0 {
			t.Errorf("a share without sound: %v %q", err, e.log.get())
		}
	})
}

// TestSetPreset: 01's Share.SetPreset (C2's share.update), remembered as serverPrefs[origin].preset.
func TestSetPreset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", false)
		goLive(s)
		if err := e.ctl.SetPreset(context.Background(), protocol.PresetMovie); err != nil {
			t.Fatal(err)
		}
		if e.log.count("core.Share.SetPreset(movie)") != 1 {
			t.Errorf("calls %q", e.log.get())
		}
		if v := e.ctl.Current(); v.Preset != protocol.PresetMovie {
			t.Errorf("view %+v", v)
		}
		if got := e.set.Get().ServerPrefs[testOrigin].Preset; got != protocol.PresetMovie {
			t.Errorf("serverPrefs preset %q", got)
		}
		if err := e.ctl.SetPreset(context.Background(), "fast"); !errors.Is(err, ErrInvalid) {
			t.Errorf("unknown preset: %v", err)
		}
		s.mu.Lock()
		s.presetErr = core.ErrNotReady
		s.mu.Unlock()
		if err := e.ctl.SetPreset(context.Background(), protocol.PresetText); !errors.Is(err, ErrPublisherOffline) {
			t.Errorf("signaling down: %v", err)
		}
	})
}

// TestStopOther: C2 online, then 01's StopOther; busy while C2 shares elsewhere; publisher_offline at 10 s.
func TestStopOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		if err := e.ctl.StopOther(context.Background(), "lounge", "s_other0000000001"); err != nil {
			t.Fatal(err)
		}
		if got := e.log.get(); !slices.Equal(got, []string{"core.StopOther(lounge,s_other0000000001)"}) {
			t.Fatalf("calls %q", got)
		}
		e.client.set(func(c *fakeClient) { c.stopOtherErr = core.ErrBusy })
		if err := e.ctl.StopOther(context.Background(), "den", "s_other0000000001"); !errors.Is(err, ErrBusy) {
			t.Errorf("C2 in another room: %v", err)
		}
		e.client.set(func(c *fakeClient) { c.stopOtherErr = &protocol.Error{Code: protocol.ErrorCodeForbidden} })
		var pe *protocol.Error
		if err := e.ctl.StopOther(context.Background(), "lounge", "s_other0000000001"); !errors.As(err, &pe) {
			t.Errorf("a refusal: %v", err)
		}
		e.pub.set(func(p *fakePublisher) { p.block = true })
		start := time.Now()
		if err := e.ctl.StopOther(context.Background(), "lounge", "s_other0000000001"); !errors.Is(err,
			ErrPublisherOffline) || time.Since(start) != ReadyTimeout {
			t.Errorf("C2 offline: %v after %v", err, time.Since(start))
		}
	})
}

// TestShareFollowsSPARoom: a share is in the room the SPA started it in; the SPA's switch to another room stops it
// (01's Share.Stop leaves the room), and the next share starts in the new room. The controller itself never joins
// or leaves.
func TestShareFollowsSPARoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		goLive(e.start(t, "lounge", true))
		if roomID, live := e.ctl.Live(); roomID != "lounge" || !live {
			t.Fatalf("Live = %q, %v", roomID, live)
		}
		// "Stop sharing and switch to den?" → yes: the SPA stops the share before C1 joins den.
		e.ctl.Stop(context.Background(), CauseUser)
		if _, live := e.ctl.Live(); live {
			t.Fatal("live after the switch")
		}
		goLive(e.start(t, "den", true))
		if roomID, _ := e.ctl.Live(); roomID != "den" {
			t.Errorf("Live room %q", roomID)
		}
		if v := e.ctl.Current(); v.RoomID != "den" {
			t.Errorf("view room %q", v.RoomID)
		}
		var rooms []string
		for _, c := range e.log.get() {
			if strings.Contains(c, "join") || strings.Contains(c, "leave") {
				t.Errorf("a room message of the controller's own: %s", c)
			}
			if strings.HasPrefix(c, "core.StartShare") {
				rooms = append(rooms, c[len("core.StartShare(room="):strings.Index(c, ",")])
			}
		}
		if !slices.Equal(rooms, []string{"lounge", "den"}) {
			t.Errorf("StartShare rooms %q", rooms)
		}
	})
}

// TestStopDuringStart: a Stop while Start waits for C2 aborts it; the share ends with the Stop's cause and the pick
// is released.
func TestStopDuringStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.pub.set(func(p *fakePublisher) { p.block = true })
		p := e.pick(t)
		done := make(chan error, 1)
		go func() {
			_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge"})
			done <- err
		}()
		synctest.Wait()
		if v := e.ctl.Current(); v == nil || v.State != StateStarting {
			t.Fatalf("view %+v", v)
		}
		e.ctl.Stop(context.Background(), CauseHotkey)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Start: %v", err)
		}
		v := e.ctl.Current()
		if v == nil || v.State != StateEnded || v.EndReason != EndHotkey || v.Err != nil {
			t.Fatalf("view %+v", v)
		}
		if e.log.count("engine.Release(pick:1)") != 1 || e.log.count("audio.") != 0 {
			t.Errorf("calls %q", e.log.get())
		}
	})
}

// TestStopWhileStartShareReturns: a Stop that comes while StartShare is about to succeed (too late for its ctx)
// stops the share StartShare returns, and returns once that share's end ran.
func TestStopWhileStartShareReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		gate := make(chan struct{})
		e.client.set(func(c *fakeClient) { c.startGate = gate })
		p := e.pick(t)
		started := make(chan error, 1)
		go func() {
			_, err := e.ctl.Start(context.Background(), StartRequest{PickID: p.PickID, RoomID: "lounge",
				WithAudio: true})
			started <- err
		}()
		synctest.Wait() // StartShare runs and ignores its ctx from here on
		stopped := make(chan struct{})
		go func() {
			e.ctl.Stop(context.Background(), CauseTray)
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned before the share's end")
		default:
		}
		close(gate)
		if err := <-started; err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-stopped
		v := e.ctl.Current()
		if v == nil || v.State != StateEnded || v.EndReason != EndTray {
			t.Fatalf("view %+v", v)
		}
		e.log.order(t, "core.StartShare", "core.Share.Stop", "audio.OnShareStopped")
	})
}

// TestStateEventsCoalesced: share.state goes out at most every StateEvery, the latest view last.
func TestStateEventsCoalesced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		goLive(s)
		synctest.Wait()
		time.Sleep(time.Second)
		e.rec.reset()
		for i := range 50 {
			s.update(func(snap *core.ShareSnapshot) { snap.Watchers = make([]protocol.Watcher, i+1) })
			synctest.Wait()
			time.Sleep(20 * time.Millisecond)
		}
		settle()
		states := e.rec.kind(EventState)
		if len(states) > 11 || len(states) < 9 {
			t.Errorf("%d share.state events in 1 s", len(states))
		}
		if v := states[len(states)-1].View; v == nil || v.Watching != 50 {
			t.Errorf("last view %+v", v)
		}
	})
}

// TestStatsEvery2s: share.stats every StatsEvery while live (and paused), none while starting; Stats() alike.
func TestStatsEvery2s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		time.Sleep(3 * StatsEvery)
		if n := len(e.rec.kind(EventStats)); n != 0 || e.ctl.Stats() != nil {
			t.Fatalf("%d stats while starting", n)
		}
		goLive(s)
		synctest.Wait()
		time.Sleep(5*StatsEvery + time.Millisecond)
		stats := e.rec.kind(EventStats)
		if len(stats) != 5 {
			t.Fatalf("%d stats in 10 s", len(stats))
		}
		st := stats[0].Stats
		if len(st.Layers) != 2 || st.Layers[0].SentBps != 7_712_000 || st.Layers[0].Encoder != "nvenc" ||
			st.Layers[0].Limit != LimitNone || st.AudioSentBps != 128_000 {
			t.Errorf("stats %+v", st)
		}
		s.update(func(snap *core.ShareSnapshot) { snap.Upload.Limited = true })
		synctest.Wait()
		if st := e.ctl.Stats(); st == nil || st.Layers[1].Limit != LimitBandwidth {
			t.Errorf("Stats %+v", st)
		}
	})
}

// TestNotices: 01's encoder_fallback becomes a share.notice; audio_rebuilt is logged only.
func TestNotices(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		goLive(e.start(t, "lounge", true))
		synctest.Wait()
		e.client.emit(core.Event{Kind: core.EventNotice, Notice: &core.Notice{Code: core.NoticeEncoderFallback,
			Params: map[string]string{"layer": "f", "encoder": "mf-hw"}}})
		e.client.emit(core.Event{Kind: core.EventNotice, Notice: &core.Notice{Code: "audio_rebuilt"}})
		synctest.Wait()
		notices := e.rec.kind(EventNotice)
		if len(notices) != 1 || notices[0].Notice.Code != "encoder_fallback" ||
			notices[0].Notice.Params["encoder"] != "mf-hw" {
			t.Fatalf("notices %+v", notices)
		}
	})
}

// TestWatchServerStats: the wish reaches the running share's client and every later one; View.Server only while on.
func TestWatchServerStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		s := e.start(t, "lounge", true)
		s.update(func(snap *core.ShareSnapshot) {
			snap.State = core.StateLive
			snap.Server = []protocol.ServerLayerStats{{ShareID: "s_q7m2k9x4w3bd8c1a", Kind: protocol.TrackKindVideo,
				RID: "f", Bitrate: 7_000_000}}
		})
		synctest.Wait()
		if v := e.ctl.Current(); v.Server != nil {
			t.Fatal("server stats while off")
		}
		e.ctl.WatchServerStats(true)
		if v := e.ctl.Current(); len(v.Server) != 1 {
			t.Fatalf("view %+v", v)
		}
		e.ctl.Stop(context.Background(), CauseUser)
		e.log.reset()
		e.start(t, "lounge", false)
		if e.log.count("core.WatchServerStats(true)") != 1 {
			t.Errorf("a later share's client: %q", e.log.get())
		}
		e.ctl.WatchServerStats(false)
		if e.log.count("core.WatchServerStats(false)") != 1 {
			t.Errorf("calls %q", e.log.get())
		}
	})
}

// TestWatchServerStatsBetweenShares: 01's switch is sticky on the client, so a share's end turns it off there, and
// a Details disclosure closed between shares (the bridge's off reaching a controller with no share) leaves the next
// share's client off.
func TestWatchServerStatsBetweenShares(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		goLive(e.start(t, "lounge", true))
		synctest.Wait()
		e.ctl.WatchServerStats(true)
		e.log.reset()
		e.ctl.Stop(context.Background(), CauseUser)
		e.log.order(t, "core.Share.Stop", "core.WatchServerStats(false)")
		if e.log.count("core.WatchServerStats(true)") != 0 {
			t.Errorf("stop: %q", e.log.get())
		}

		e.ctl.WatchServerStats(false) // the disclosure unmounts after the share ended
		e.log.reset()
		goLive(e.start(t, "lounge", false))
		synctest.Wait()
		if e.log.count("core.WatchServerStats(false)") != 1 || e.log.count("core.WatchServerStats(true)") != 0 {
			t.Errorf("the next share's client: %q", e.log.get())
		}
		if v := e.ctl.Current(); v.Server != nil {
			t.Errorf("server stats with the wish off: %+v", v.Server)
		}

		// Without the wish a share's end makes no call.
		e.log.reset()
		e.ctl.Stop(context.Background(), CauseUser)
		if n := e.log.count("core.WatchServerStats("); n != 0 {
			t.Errorf("end without the wish: %q", e.log.get())
		}
	})
}

// TestClose: the held pick is released, a running share stopped, and Pick and Start refuse afterwards.
func TestClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		goLive(e.start(t, "lounge", true))
		synctest.Wait()
		e.ctl.Close()
		e.log.order(t, "core.Share.Stop", "audio.OnShareStopped")
		if _, err := e.ctl.Pick(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("Pick after Close: %v", err)
		}

		e2 := newEnv(t)
		e2.pick(t)
		e2.ctl.Close()
		if e2.log.count("engine.Release(pick:1)") != 1 {
			t.Errorf("held pick at Close: %q", e2.log.get())
		}
	})
}

// TestFromCore: the adapter keeps a nil share out of the interface.
func TestFromCore(t *testing.T) {
	c := FromCore(nilCore{})
	s, err := c.StartShare(context.Background(), core.ShareRequest{})
	if s != nil || !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("StartShare = %v, %v", s, err)
	}
}

type nilCore struct{}

func (nilCore) Status() core.Status                    { return core.Status{} }
func (nilCore) Subscribe() (<-chan core.Event, func()) { return nil, func() {} }
func (nilCore) StartShare(context.Context, core.ShareRequest) (*core.Share, error) {
	return nil, core.ErrNotReady
}
func (nilCore) StopOther(context.Context, string, string) error { return nil }
func (nilCore) WatchServerStats(bool)                           {}

// TestRedact: no error text reaches the log beyond codes and known names.
func TestRedact(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&media.Error{Code: media.CodeDependency, Op: "start", Reason: "abi", Detail: `C:\Users\alex\x.dll`},
			"start: dependency (abi)"},
		{&protocol.Error{Code: protocol.ErrorCodeRoomFull, RoomID: "alexs-room"}, "room_full"},
		{deviceauth.ErrNotLinked, deviceauth.ErrNotLinked.Error()},
		{errors.New("GET https://watch.example.com: Bearer isa_x"), "error"},
	} {
		if got := redact(c.err); got != c.want {
			t.Errorf("redact(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
