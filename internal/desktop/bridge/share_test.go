package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The Share service over the share controller (03 §6.2, §6.3, §10.5).

const testPickID = "0123456789abcdef"

// liveView is 03 §6.3's example share.
func liveView() share.View {
	return share.View{
		State: share.StateLive, ShareID: "s_q7m2k9x4w3bd8c1a", ConnectionID: "c_k3v9q2m7xw4pa8d1", RoomID: "lounge",
		Kind: protocol.ShareKindWindow, Label: "Minecraft", Preset: protocol.PresetGame, WithAudio: true, AudioOn: true,
		Pill: true, Watching: 3, LiveSince: time.Date(2026, 10, 21, 14, 0, 0, 0, time.FixedZone("CEST", 7200)),
		Encoder: "nvenc", HW: true,
		Layers: []share.Layer{
			{RID: "f", Width: 1920, Height: 1080, FPS: 59.6, TargetBps: 8_000_000, SentBps: 7_712_400,
				CaptureToSendP50: 14 * time.Millisecond, CaptureToSendP95: 22200 * time.Microsecond},
			{RID: "q", Width: 640, Height: 360, FPS: 15, TargetBps: 300_000, SentBps: 296_000,
				CaptureToSendP50: 12 * time.Millisecond, CaptureToSendP95: 19 * time.Millisecond},
		},
	}
}

// TestShareViewJSON: the controller's View as 03 §6.3's ShareView JSON, byte for byte.
func TestShareViewJSON(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	s := newTestServices(core, &fakeServers{}, nil, &buf)
	v := liveView()
	core.shares.set(func(f *fakeShares) { f.current = &v })
	b, err := json.Marshal(s.Share.Current())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"live","shareId":"s_q7m2k9x4w3bd8c1a","connectionId":"c_k3v9q2m7xw4pa8d1","roomId":"lounge",` +
		`"kind":"window","label":"Minecraft","preset":"game","withAudio":true,"audioOn":true,"border":false,` +
		`"pill":true,"minimized":false,"watching":3,"liveSince":"2026-10-21T12:00:00.000Z","hint":null,` +
		`"encoder":"nvenc","hw":true,"layers":[{"rid":"f","width":1920,"height":1080,"fps":60,"targetKbps":8000,` +
		`"sentKbps":7712,"captureToSendMs":{"p50":14,"p95":22}},{"rid":"q","width":640,"height":360,"fps":15,` +
		`"targetKbps":300,"sentKbps":296,"captureToSendMs":{"p50":12,"p95":19}}],"server":null,"endReason":null,` +
		`"error":null}`
	if string(b) != want {
		t.Fatalf("ShareView\n got %s\nwant %s", b, want)
	}

	// A failed share: endReason error and 03 §6.4's {code, params}; a hint; server stats while watched.
	v = share.View{State: share.StateFailed, RoomID: "lounge", Kind: protocol.ShareKindScreen, Layers: nil,
		Hint:      share.Hint{Kind: share.HintUploadLimited, ApproxHeight: 720},
		Server:    []protocol.ServerLayerStats{{ShareID: "s_1", Kind: protocol.TrackKindVideo, RID: "f", Bitrate: 7000}},
		EndReason: share.EndError, Err: &media.Error{Code: media.CodeEncoderLost, Op: "share"}}
	core.shares.set(func(f *fakeShares) { f.current = &v })
	got := s.Share.Current()
	if got.State != "failed" || *got.EndReason != "error" || got.Error.Code != string(codes.CodeEngineEncoderLost) ||
		got.Error.Params["reason"] != "" || got.Hint.Kind != "upload-limited" || got.Hint.ApproxHeight != 720 ||
		got.LiveSince != nil || got.Layers == nil {
		t.Fatalf("failed view %+v", got)
	}
	b, _ = json.Marshal(got.Server)
	if string(b) != `{"layers":[{"shareId":"s_1","kind":"video","rid":"f","bitrate":7000,"lossPct":0}]}` {
		t.Errorf("server %s", b)
	}
	v = share.View{State: share.StateFailed, EndReason: share.EndError, Err: &protocol.Error{
		Code: protocol.ErrorCodeShareLimit, Params: map[string]any{"limit": 2}}}
	core.shares.set(func(f *fakeShares) { f.current = &v })
	if got := s.Share.Current(); got.Error.Code != "share_limit" || got.Error.Params["limit"] != 2 {
		t.Errorf("wire error %+v", got.Error)
	}
	core.shares.set(func(f *fakeShares) { f.current = nil })
	if s.Share.Current() != nil {
		t.Error("a view without a share")
	}
}

// TestSharePick: the picked source as Picked; null for a cancelled picker; the engine's errors as engine.<code>.
func TestSharePick(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	s := newTestServices(core, &fakeServers{}, nil, &buf)
	core.shares.set(func(f *fakeShares) {
		f.picked = &share.Picked{PickID: testPickID, Kind: protocol.ShareKindScreen, Label: "DELL U2723QE",
			Border: true, Confirm: []share.Sheet{share.SheetFirstShare, share.SheetDND}}
	})
	p, err := s.Share.Pick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"pickId":"0123456789abcdef","kind":"screen","label":"DELL U2723QE","border":true,`+
		`"confirm":["first-share","dnd"]}` {
		t.Fatalf("Picked %s", b)
	}
	core.shares.set(func(f *fakeShares) { f.picked = &share.Picked{PickID: testPickID, Kind: "window"} })
	if p, _ := s.Share.Pick(context.Background()); p == nil || p.Confirm == nil {
		t.Errorf("no sheets must be [], not null: %+v", p)
	}
	core.shares.set(func(f *fakeShares) { f.picked = nil })
	if p, err := s.Share.Pick(context.Background()); p != nil || err != nil {
		t.Errorf("cancelled: %+v, %v", p, err)
	}
	core.shares.set(func(f *fakeShares) {
		f.err = &media.Error{Code: media.CodeInvalid, Op: "pick", Reason: "self_window"}
	})
	_, err = s.Share.Pick(context.Background())
	if err == nil || err.Error() != `{"code":"engine.invalid","params":{"reason":"self_window"}}` {
		t.Errorf("self pick: %v", err)
	}
	core.shares.set(func(f *fakeShares) { f.err = share.ErrAlreadySharing })
	if _, err := s.Share.Pick(context.Background()); codeOf(t, err) != codes.CodeAlreadySharing {
		t.Errorf("Pick while sharing: %v", err)
	}
	s.Share.Release(testPickID)
	if calls := core.shares.recorded(); calls[len(calls)-1] != "Release "+testPickID {
		t.Errorf("calls %q", calls)
	}
}

// TestShareStart: arguments checked first (invalid_argument with the field), then the controller's Start within
// 20 s; its errors as 03 §6.4's codes.
func TestShareStart(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	s := newTestServices(core, &fakeServers{}, nil, &buf)
	for _, c := range []struct {
		req   StartRequest
		field string
	}{
		{StartRequest{PickID: "p_1", RoomID: "lounge"}, "pickId"},
		{StartRequest{PickID: "0123456789ABCDEF", RoomID: "lounge"}, "pickId"},
		{StartRequest{PickID: testPickID, RoomID: ""}, "roomId"},
		{StartRequest{PickID: testPickID, RoomID: "../admin"}, "roomId"},
		{StartRequest{PickID: testPickID, RoomID: "lounge", Preset: "fast"}, "preset"},
	} {
		_, err := s.Share.Start(context.Background(), c.req)
		var be *Error
		if !errors.As(err, &be) || be.Code != codes.CodeInvalidArgument || be.Params["field"] != c.field {
			t.Errorf("Start(%+v): %v", c.req, err)
		}
	}
	if calls := core.shares.recorded(); len(calls) != 0 {
		t.Fatalf("the controller was called: %q", calls)
	}

	v := liveView()
	v.State = share.StateStarting
	core.shares.set(func(f *fakeShares) { f.view = v })
	got, err := s.Share.Start(context.Background(), StartRequest{PickID: testPickID, RoomID: "lounge",
		Preset: "movie", WithAudio: true})
	if err != nil || got.State != "starting" || got.Label != "Minecraft" {
		t.Fatalf("Start = %+v, %v", got, err)
	}
	core.shares.mu.Lock()
	req, left := core.shares.startReq, core.shares.startCtx
	core.shares.mu.Unlock()
	if req != (share.StartRequest{PickID: testPickID, RoomID: "lounge", Preset: protocol.PresetMovie,
		WithAudio: true}) || left <= 19*time.Second || left > startTimeout {
		t.Errorf("controller got %+v with %v left", req, left)
	}
	if _, err := s.Share.Start(context.Background(), StartRequest{PickID: testPickID, RoomID: "lounge"}); err != nil {
		t.Errorf("Start with the default preset: %v", err)
	}

	for _, c := range []struct {
		err    error
		want   string
		params map[string]any
	}{
		{share.ErrPickExpired, "pick_expired", map[string]any{"reason": "pick_expired"}},
		{&media.Error{Code: media.CodeSourceGone, Op: "start", Reason: "pick_expired"}, "engine.source_gone",
			map[string]any{"reason": "pick_expired"}},
		{share.ErrAlreadySharing, "already_sharing", nil},
		{fmt.Errorf("%w: %w", share.ErrPublisherOffline, context.DeadlineExceeded), "publisher_offline", nil},
		{fmt.Errorf("session: %w", deviceauth.ErrNotLinked), "not_linked", nil},
		{fmt.Errorf("session: %w", deviceauth.ErrIncompatible), "incompatible_server", nil},
		{&protocol.Error{Code: protocol.ErrorCodeRoomFull}, "room_full", nil},
		{&protocol.Error{Code: protocol.ErrorCodeRoomNotFound}, "room_not_found", nil},
		{&media.Error{Code: media.CodePermissionDenied, Op: "start"}, "engine.permission_denied",
			map[string]any{"reason": ""}},
	} {
		core.shares.set(func(f *fakeShares) { f.err = c.err })
		_, err := s.Share.Start(context.Background(), StartRequest{PickID: testPickID, RoomID: "lounge"})
		var be *Error
		if !errors.As(err, &be) || string(be.Code) != c.want {
			t.Errorf("%v → %v, want %s", c.err, err, c.want)
			continue
		}
		if c.params != nil {
			want, _ := json.Marshal(c.params)
			have, _ := json.Marshal(be.Params)
			if string(want) != string(have) {
				t.Errorf("%v → params %s, want %s", c.err, have, want)
			}
		}
	}
	if buf.Len() != 0 {
		t.Errorf("known errors were logged: %s", buf.String())
	}
}

// TestShareCalls: Stop (user, never fails), StopOther, SetPaused, SetPreset, SetAudioEnabled and Stats over the
// controller, with their argument checks and error codes.
func TestShareCalls(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	s := newTestServices(core, &fakeServers{}, nil, &buf)
	ctx := context.Background()
	if err := s.Share.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if core.shares.stopCause != share.CauseUser {
		t.Errorf("Stop's cause %q", core.shares.stopCause)
	}
	for _, c := range []struct{ room, share, field string }{
		{"", "s_1", "roomId"}, {"lounge", "", "shareId"}, {"lounge", "s 1", "shareId"},
	} {
		err := s.Share.StopOther(ctx, c.room, c.share)
		var be *Error
		if !errors.As(err, &be) || be.Code != codes.CodeInvalidArgument || be.Params["field"] != c.field {
			t.Errorf("StopOther(%q, %q): %v", c.room, c.share, err)
		}
	}
	if err := s.Share.StopOther(ctx, "lounge", "s_q7m2k9x4w3bd8c1a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Share.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Share.SetPreset(ctx, "text"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "fast"} {
		if err := s.Share.SetPreset(ctx, p); codeOf(t, err) != codes.CodeInvalidArgument {
			t.Errorf("SetPreset(%q): %v", p, err)
		}
	}
	if err := s.Share.SetAudioEnabled(false); err != nil {
		t.Fatal(err)
	}
	want := []string{"Stop", "StopOther lounge s_q7m2k9x4w3bd8c1a", "SetPaused true", "SetPreset text",
		"SetAudioEnabled false"}
	if got := core.shares.recorded(); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}

	for _, c := range []struct {
		err  error
		want codes.Code
	}{
		{share.ErrFeatureDisabled, codes.CodeFeatureDisabled},
		{share.ErrNotSharing, codes.CodeNotSharing},
		{fmt.Errorf("%w: %w", share.ErrNotSharing, &protocol.Error{Code: protocol.ErrorCodeShareNotFound}),
			codes.CodeNotSharing},
		{share.ErrBusy, codes.CodeBusy},
		{share.ErrPublisherOffline, codes.CodePublisherOffline},
		{share.ErrInvalid, codes.CodeInvalidArgument},
	} {
		core.shares.set(func(f *fakeShares) { f.err = c.err })
		for name, call := range map[string]func() error{
			"StopOther":       func() error { return s.Share.StopOther(ctx, "lounge", "s_1") },
			"SetPaused":       func() error { return s.Share.SetPaused(ctx, false) },
			"SetPreset":       func() error { return s.Share.SetPreset(ctx, "auto") },
			"SetAudioEnabled": func() error { return s.Share.SetAudioEnabled(true) },
		} {
			if got := codeOf(t, call()); got != c.want {
				t.Errorf("%s with %v: %s, want %s", name, c.err, got, c.want)
			}
		}
	}
	core.shares.set(func(f *fakeShares) { f.err = share.ErrFeatureDisabled })
	if err := s.Share.SetPaused(ctx, true); err.Error() != `{"code":"feature_disabled","params":{"feature":"share.pause"}}` {
		t.Errorf("feature_disabled: %s", err)
	}
	if err := s.Share.Stop(ctx); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	if s.Share.Stats() != nil {
		t.Error("stats without a share")
	}
	core.shares.set(func(f *fakeShares) {
		f.stats = &share.Stats{AudioSentBps: 127_900, Layers: []share.StatsLayer{{RID: "f", Width: 1920, Height: 1080,
			FPS: 30.4, SentBps: 4_000_999, Encoder: "qsv", HW: true, Limit: share.LimitBandwidth}}}
	})
	b, _ := json.Marshal(s.Share.Stats())
	if string(b) != `{"layers":[{"rid":"f","width":1920,"height":1080,"fps":30,"kbps":4000,"encoder":"qsv",`+
		`"hw":true,"limit":"bandwidth"}],"audioKbps":127}` {
		t.Errorf("Stats %s", b)
	}
}

// TestShareEvents: the controller's events go out as isshoni:share.state (ShareView, null for none),
// isshoni:share.stats and isshoni:share.notice; Close stops forwarding.
func TestShareEvents(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	type sent struct {
		name string
		data any
	}
	var (
		mu  sync.Mutex
		got []sent
	)
	s, err := New(Deps{App: core, Servers: &fakeServers{}, Log: testLog(&buf), Emit: func(name string, data any) {
		mu.Lock()
		got = append(got, sent{name, data})
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	v := liveView()
	core.shares.send(share.Event{Kind: share.EventState, View: &v})
	core.shares.send(share.Event{Kind: share.EventState})
	core.shares.send(share.Event{Kind: share.EventStats, Stats: &share.Stats{Layers: []share.StatsLayer{{RID: "q",
		Limit: share.LimitNone}}}})
	core.shares.send(share.Event{Kind: share.EventNotice, Notice: &share.Notice{Code: "encoder_fallback",
		Params: map[string]string{"layer": "f", "encoder": "mf-hw"}}})
	s.Close()
	core.shares.send(share.Event{Kind: share.EventState})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("%d events: %+v", len(got), got)
	}
	names := []string{got[0].name, got[1].name, got[2].name, got[3].name}
	if !slices.Equal(names, []string{"isshoni:share.state", "isshoni:share.state", "isshoni:share.stats",
		"isshoni:share.notice"}) {
		t.Fatalf("names %q", names)
	}
	if sv, ok := got[0].data.(ShareView); !ok || sv.State != "live" || sv.Watching != 3 {
		t.Errorf("state %+v", got[0].data)
	}
	if b, _ := json.Marshal(got[1].data); string(b) != "null" {
		t.Errorf("no share: %s", b)
	}
	if b, _ := json.Marshal(got[2].data); string(b) != `{"layers":[{"rid":"q","width":0,"height":0,"fps":0,`+
		`"kbps":0,"encoder":"","hw":false,"limit":"none"}],"audioKbps":0}` {
		t.Errorf("stats %s", b)
	}
	if b, _ := json.Marshal(got[3].data); string(b) != `{"code":"encoder_fallback","params":{"encoder":"mf-hw",`+
		`"layer":"f"}}` {
		t.Errorf("notice %s", b)
	}
}

// TestEngineErrorRows is 03 §10.5's Go side: each reason and code row reaches the SPA as engine.<code> with
// params.reason (the SPA picks the reason row first), the controller's own pick_expired with the same reason, and
// cancelled never surfaces.
func TestEngineErrorRows(t *testing.T) {
	var buf bytes.Buffer
	log := testLog(&buf)
	for _, c := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{share.ErrPickExpired, codes.CodePickExpired, "pick_expired"},
		{&media.Error{Code: media.CodeSourceGone, Reason: "pick_expired"}, codes.CodeEngineSourceGone, "pick_expired"},
		{&media.Error{Code: media.CodeInvalid, Reason: "self_window"}, codes.CodeEngineInvalid, "self_window"},
		{&media.Error{Code: media.CodeDependency, Reason: "driver_too_old"}, codes.CodeEngineDependency,
			"driver_too_old"},
		{&media.Error{Code: media.CodeDependency, Reason: "no_media_foundation"}, codes.CodeEngineDependency,
			"no_media_foundation"},
		{&media.Error{Code: media.CodeDependency, Reason: "blocked_by_policy"}, codes.CodeEngineDependency,
			"blocked_by_policy"},
		{&media.Error{Code: media.CodeDependency, Reason: "missing"}, codes.CodeEngineDependency, "missing"},
		{&media.Error{Code: media.CodeDependency, Reason: "version"}, codes.CodeEngineDependency, "version"},
		{&media.Error{Code: media.CodeDependency, Reason: "abi"}, codes.CodeEngineDependency, "abi"},
		{&media.Error{Code: media.CodeEncoderLost, Reason: "device_removed"}, codes.CodeEngineEncoderLost,
			"device_removed"},
		{&media.Error{Code: media.CodePermissionDenied}, codes.CodeEnginePermissionDenied, ""},
		{&media.Error{Code: media.CodeEncoderLost}, codes.CodeEngineEncoderLost, ""},
		{&media.Error{Code: media.CodeSourceGone}, codes.CodeEngineSourceGone, ""},
		{&media.Error{Code: media.CodeUnsupportedOS}, codes.CodeEngineUnsupportedOS, ""},
		{&media.Error{Code: media.CodeNoEncoder}, codes.CodeEngineNoEncoder, ""},
		{&media.Error{Code: media.CodeInternal}, codes.CodeEngineInternal, ""},
		{&media.Error{Code: media.CodeBusy, Reason: "pick_busy"}, codes.CodeEngineBusy, "pick_busy"},
		{&media.Error{Code: media.CodeCancelled}, codes.CodeEngineInternal, ""},
	} {
		var be *Error
		if !errors.As(mapError(c.err, log), &be) || be.Code != c.code || be.Params["reason"] != c.reason {
			t.Errorf("%v → %+v, want %s (%s)", c.err, be, c.code, c.reason)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("logged: %s", buf.String())
	}
}
