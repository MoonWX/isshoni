package fake

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// testLayers are the share's two layers at the Auto preset: f 1080p60 at 4 Mbit/s, q 360p15 at 300 kbit/s.
func testLayers() []media.LayerConfig {
	return []media.LayerConfig{
		{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: 4_000_000},
		{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000},
	}
}

// testConfig starts the listed monitor with the two layers and audio.
func testConfig() media.Config {
	return media.Config{
		Source: ListedMonitorID, Listed: true, Profile: "6400", Preset: "auto", Layers: testLayers(),
		Audio: &media.AudioConfig{}, Borderless: true,
	}
}

// mustStart starts a session and stops it when the test ends.
func mustStart(t *testing.T, e *Engine, cfg media.Config) *Session {
	t.Helper()
	s, err := e.Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s.(*Session)
}

// codeOf returns the media.ErrorCode of err, or "".
func codeOf(err error) media.ErrorCode {
	if me, ok := errors.AsType[*media.Error](err); ok {
		return me.Code
	}
	return ""
}

// reasonOf returns the Reason of err's media.Error, or "".
func reasonOf(err error) string {
	if me, ok := errors.AsType[*media.Error](err); ok {
		return me.Reason
	}
	return ""
}

func TestEngineDefaults(t *testing.T) {
	e := NewEngine(EngineConfig{})
	defer e.Close()
	if got := e.Info(); got != (media.Info{Name: "fake", ABI: media.ABIVersion}) {
		t.Errorf("Info = %+v", got)
	}
	p, err := e.Probe(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := media.Probe{
		OS: media.OSInfo{Name: runtime.GOOS, Supported: true},
		Encoders: []media.EncoderInfo{{
			Name: "fake", Adapter: -1, Tested: true, LiveBitrate: true, Profiles: []string{"6400", "42e0"},
		}},
		Capture: media.CaptureCaps{Picker: true, Borderless: true, MaxFPS: 60},
		Audio: media.AudioCaps{ProcessLoopback: true, EndpointLoopback: true, SessionEvents: true, MicDetection: true,
			Backend: "process-loopback"},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("Probe = %+v\nwant %+v", p, want)
	}
	// The probe is a copy.
	p.Encoders[0].Profiles[0] = "x"
	if p2, _ := e.Probe(t.Context(), true); p2.Encoders[0].Profiles[0] != "6400" {
		t.Error("changing the returned Probe changed the engine's")
	}
	srcs, err := e.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantSrc := []media.Source{{ID: "mon:fake", Kind: media.SourceScreen, Name: "Fake monitor", Width: 1920,
		Height: 1080, MonitorID: "mon:fake", Capture: "test"}}
	if !reflect.DeepEqual(srcs, wantSrc) {
		t.Errorf("Sources = %+v", srcs)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Probe(ctx, false); !errors.Is(err, context.Canceled) {
		t.Errorf("Probe with a cancelled ctx: %v", err)
	}
	if _, err := e.Sources(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Sources with a cancelled ctx: %v", err)
	}

	d := NewEngine(EngineConfig{Mode: Decodable, SourceSize: [2]int{640, 360}})
	defer d.Close()
	if p, _ := d.Probe(t.Context(), false); !reflect.DeepEqual(p.Encoders[0].Profiles, []string{"42e0"}) {
		t.Errorf("Decodable profiles = %v", p.Encoders[0].Profiles)
	}
	if srcs, _ := d.Sources(t.Context()); srcs[0].Width != 640 || srcs[0].Height != 360 {
		t.Errorf("Decodable monitor = %+v", srcs[0])
	}

	// Each zero part of a Probe takes its default.
	part := NewEngine(EngineConfig{Probe: media.Probe{Encoders: []media.EncoderInfo{{Name: "mf-sw", Tested: true}}}})
	defer part.Close()
	p, _ = part.Probe(t.Context(), false)
	if p.OS != want.OS || p.Capture != want.Capture || p.Audio != want.Audio || len(p.Encoders) != 1 ||
		p.Encoders[0].Name != "mf-sw" {
		t.Errorf("partial Probe = %+v", p)
	}
	none := NewEngine(EngineConfig{Probe: media.Probe{Audio: media.AudioCapsFromFlags(0)}})
	defer none.Close()
	if p, _ := none.Probe(t.Context(), false); p.Audio.Backend != "none" {
		t.Errorf("Probe.Audio = %+v, want the configured caps without audio", p.Audio)
	}
}

func TestEngineClose(t *testing.T) {
	e := NewEngine(EngineConfig{})
	s := mustStart(t, e, testConfig())
	if e.Session() != s {
		t.Fatal("Session() is not the running session")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(t.Context()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Next after Engine.Close: %v", err)
	}
	if e.Session() != nil {
		t.Error("Session() after Close")
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := e.Probe(t.Context(), false); !errors.Is(err, media.ErrStopped) || reasonOf(err) != "closed" {
		t.Errorf("Probe after Close: %v", err)
	}
	if _, err := e.Sources(t.Context()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Sources after Close: %v", err)
	}
	if _, err := e.Pick(t.Context(), media.PickRequest{}); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Pick after Close: %v", err)
	}
	if _, err := e.Start(t.Context(), testConfig()); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Start after Close: %v", err)
	}
	if e.Info().Name != "fake" {
		t.Error("Info after Close")
	}
}

// pickConfig starts src with the two layers.
func pickConfig(id string) media.Config {
	cfg := testConfig()
	cfg.Source, cfg.Listed = id, false
	return cfg
}

// TestPicks: picks are single use, a failed Start keeps them, Release forgets them, they expire 5 min after the pick
// and a fifth unused pick evicts the oldest (04 §7.2); a listed source needs Config.Listed.
func TestPicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(EngineConfig{})
		defer e.Close()
		pick := func() media.Source {
			t.Helper()
			src, err := e.Pick(t.Context(), media.PickRequest{})
			if err != nil {
				t.Fatalf("Pick: %v", err)
			}
			return src
		}
		start := func(cfg media.Config) error {
			t.Helper()
			s, err := e.Start(t.Context(), cfg)
			if err == nil {
				_ = s.Stop()
			}
			return err
		}
		wantGone := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, media.ErrSourceGone) || reasonOf(err) != "pick_expired" {
				t.Errorf("%s: %v, want source_gone (pick_expired)", what, err)
			}
		}

		src := pick()
		want := media.Source{ID: "pick:1", Kind: media.SourceWindow, Name: "Fake window", Width: 1920, Height: 1080,
			Capture: "wgc", OSBorder: true}
		if src != want {
			t.Errorf("Pick = %+v, want %+v", src, want)
		}
		if err := start(pickConfig(src.ID)); err != nil {
			t.Fatalf("Start(pick:1): %v", err)
		}
		wantGone("a second Start of pick:1", start(pickConfig(src.ID)))

		// A failed Start keeps the pick.
		src = pick()
		bad := pickConfig(src.ID)
		bad.Profile = "640c"
		if err := start(bad); codeOf(err) != media.CodeInvalid {
			t.Fatalf("Start with profile 640c: %v", err)
		}
		if err := start(pickConfig(src.ID)); err != nil {
			t.Errorf("Start after a failed Start: %v", err)
		}

		// Release.
		src = pick()
		e.Release(src.ID)
		e.Release("pick:999") // no-op
		e.Release(ListedMonitorID)
		wantGone("Start after Release", start(pickConfig(src.ID)))
		wantGone("Start of an unknown pick", start(pickConfig("pick:999")))

		// Expiry, counted from the pick.
		early, late := pick(), pick()
		time.Sleep(pickLifetime - time.Second)
		if err := start(pickConfig(early.ID)); err != nil {
			t.Errorf("Start 1 s before the pick expires: %v", err)
		}
		time.Sleep(time.Second)
		wantGone("Start 5 min after the pick", start(pickConfig(late.ID)))

		// At most 4 unused picks: a fifth evicts the oldest.
		var ids []string
		for range 5 {
			ids = append(ids, pick().ID)
		}
		wantGone("Start of an evicted pick", start(pickConfig(ids[0])))
		for _, id := range ids[1:] {
			if err := start(pickConfig(id)); err != nil {
				t.Errorf("Start(%s): %v", id, err)
			}
		}

		// Listed sources.
		cfg := testConfig()
		cfg.Listed = false
		if err := start(cfg); codeOf(err) != media.CodeInvalid {
			t.Errorf("Start of a listed source without Listed: %v, want invalid", err)
		}
		cfg.Listed = true
		for range 2 {
			if err := start(cfg); err != nil {
				t.Errorf("Start of a listed source: %v", err)
			}
		}
		cfg.Source = "mon:other"
		if err := start(cfg); !errors.Is(err, media.ErrSourceGone) {
			t.Errorf("Start of an unknown source: %v, want source_gone", err)
		}
		// Listed accepts a pick too.
		p := pickConfig(pick().ID)
		p.Listed = true
		if err := start(p); err != nil {
			t.Errorf("Start of a pick with Listed: %v", err)
		}
	})
}

// TestPickResult: PickResult is returned under fresh IDs.
func TestPickResult(t *testing.T) {
	res := media.Source{ID: "ignored", Kind: media.SourceScreen, Name: "A monitor", Width: 2560, Height: 1440,
		MonitorID: "mon:2", Capture: "dd"}
	e := NewEngine(EngineConfig{PickResult: &res})
	defer e.Close()
	for i := 1; i <= 2; i++ {
		src, err := e.Pick(t.Context(), media.PickRequest{})
		if err != nil {
			t.Fatal(err)
		}
		want := res
		want.ID = "pick:" + string(rune('0'+i))
		if src != want {
			t.Errorf("Pick %d = %+v, want %+v", i, src, want)
		}
	}
	// The session runs at the picked source's size.
	src, _ := e.Pick(t.Context(), media.PickRequest{})
	s := mustStart(t, e, pickConfig(src.ID))
	p, err := s.Next(t.Context())
	if err != nil || p.Kind != media.KindEvent || p.Event.Width != 1920 || p.Event.Height != 1080 {
		t.Fatalf("first packet %+v (%v), want f's EventLayer at 1920x1080", p, err)
	}
	src2, _ := e.Pick(t.Context(), media.PickRequest{})
	_ = s.Stop()
	cfg := pickConfig(src2.ID)
	cfg.Layers = []media.LayerConfig{{RID: "f", MaxPixels: 4000 * 4000, MaxFPS: 30, Bitrate: 8_000_000}}
	s = mustStart(t, e, cfg)
	if p, _ := s.Next(t.Context()); p.Event.Width != 2560 || p.Event.Height != 1440 {
		t.Errorf("EventLayer %+v, want the pick's 2560x1440", p.Event)
	}
}

// TestPickCancelTimeoutCtx: the picker's cancel, timeout and ctx (01 §5.1, 04 §7.2).
func TestPickCancelTimeoutCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pickAfter := func(cfg EngineConfig, req media.PickRequest, ctx context.Context) (time.Duration, error) {
			t.Helper()
			e := NewEngine(cfg)
			defer e.Close()
			t0 := time.Now()
			_, err := e.Pick(ctx, req)
			return time.Since(t0), err
		}
		if d, err := pickAfter(EngineConfig{Cancel: true}, media.PickRequest{}, t.Context()); !errors.Is(err,
			media.ErrCancelled) || d != 0 {
			t.Errorf("Cancel: %v after %v, want cancelled at once", err, d)
		}
		if d, err := pickAfter(EngineConfig{Cancel: true, PickDelay: 3 * time.Second}, media.PickRequest{},
			t.Context()); !errors.Is(err, media.ErrCancelled) || d != 3*time.Second {
			t.Errorf("Cancel after 3 s: %v after %v", err, d)
		}
		if d, err := pickAfter(EngineConfig{PickDelay: 3 * time.Second}, media.PickRequest{}, t.Context()); err != nil ||
			d != 3*time.Second {
			t.Errorf("a pick after 3 s: %v after %v", err, d)
		}
		if d, err := pickAfter(EngineConfig{PickDelay: 10 * time.Second}, media.PickRequest{Timeout: 2 * time.Second},
			t.Context()); !errors.Is(err, media.ErrCancelled) || d != 2*time.Second {
			t.Errorf("timeout 2 s: %v after %v, want cancelled after 2 s", err, d)
		}
		if d, err := pickAfter(EngineConfig{PickDelay: time.Hour}, media.PickRequest{}, t.Context()); !errors.Is(err,
			media.ErrCancelled) || d != defaultPickTimeout {
			t.Errorf("default timeout: %v after %v, want cancelled after %v", err, d, defaultPickTimeout)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		d, err := pickAfter(EngineConfig{PickDelay: 10 * time.Second}, media.PickRequest{}, ctx)
		if !errors.Is(err, media.ErrCancelled) || !errors.Is(err, context.DeadlineExceeded) || d != time.Second {
			t.Errorf("ctx ends after 1 s: %v after %v, want cancelled and DeadlineExceeded after 1 s", err, d)
		}
		ctx, cancel = context.WithCancel(t.Context())
		cancel()
		if _, err := pickAfter(EngineConfig{}, media.PickRequest{}, ctx); !errors.Is(err, media.ErrCancelled) ||
			!errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled ctx: %v", err)
		}
		for _, timeout := range []time.Duration{-time.Second, maxPickTimeout + time.Millisecond} {
			if _, err := pickAfter(EngineConfig{}, media.PickRequest{Timeout: timeout}, t.Context()); codeOf(err) !=
				media.CodeInvalid {
				t.Errorf("Timeout %v: %v, want invalid", timeout, err)
			}
		}
		if _, err := pickAfter(EngineConfig{}, media.PickRequest{Timeout: maxPickTimeout}, t.Context()); err != nil {
			t.Errorf("Timeout %v: %v", maxPickTimeout, err)
		}

		// One pick at a time.
		e := NewEngine(EngineConfig{PickDelay: 10 * time.Second})
		defer e.Close()
		done := make(chan error)
		go func() {
			_, err := e.Pick(t.Context(), media.PickRequest{})
			done <- err
		}()
		synctest.Wait()
		if _, err := e.Pick(t.Context(), media.PickRequest{}); !errors.Is(err, media.ErrBusy) {
			t.Errorf("a concurrent Pick: %v, want busy", err)
		}
		if err := <-done; err != nil {
			t.Errorf("the first Pick: %v", err)
		}
		if src, err := e.Pick(t.Context(), media.PickRequest{}); err != nil || src.ID != "pick:2" {
			t.Errorf("a Pick after it: %+v, %v", src, err)
		}
	})
}

// TestStartChecks: Start checks the config as the native engine does.
func TestStartChecks(t *testing.T) {
	type mod func(*media.Config)
	layer := func(i int, f func(*media.LayerConfig)) mod {
		return func(c *media.Config) { f(&c.Layers[i]) }
	}
	for _, tc := range []struct {
		name   string
		engine EngineConfig
		mod    mod
		want   media.ErrorCode
		reason string
	}{
		{name: "default"},
		{name: "no audio", mod: func(c *media.Config) { c.Audio = nil }},
		{name: "one layer", mod: func(c *media.Config) { c.Layers = c.Layers[:1] }},
		{name: "audio only", mod: func(c *media.Config) { c.Source, c.Layers = "", nil }},
		{name: "nothing", mod: func(c *media.Config) { c.Source, c.Layers, c.Audio = "", nil, nil }, want: media.CodeInvalid},
		{name: "layers without a source", mod: func(c *media.Config) { c.Source = "" }, want: media.CodeInvalid},
		{name: "a source without layers", mod: func(c *media.Config) { c.Layers = nil }, want: media.CodeInvalid},
		{name: "three layers", mod: func(c *media.Config) {
			c.Layers = append(c.Layers, media.LayerConfig{RID: "h", MaxPixels: 1000, MaxFPS: 1, Bitrate: 100_000})
		}, want: media.CodeInvalid},
		{name: "duplicate rid", mod: layer(1, func(l *media.LayerConfig) { l.RID = "f" }), want: media.CodeInvalid},
		{name: "long rid", mod: layer(0, func(l *media.LayerConfig) { l.RID = "ff" }), want: media.CodeInvalid},
		{name: "MaxPixels 0", mod: layer(0, func(l *media.LayerConfig) { l.MaxPixels = 0 }), want: media.CodeInvalid},
		{name: "MaxPixels 3", mod: layer(0, func(l *media.LayerConfig) { l.MaxPixels = 3 }), want: media.CodeInvalid},
		{name: "MaxFPS 0", mod: layer(0, func(l *media.LayerConfig) { l.MaxFPS = 0 }), want: media.CodeInvalid},
		{name: "MaxFPS 241", mod: layer(0, func(l *media.LayerConfig) { l.MaxFPS = 241 }), want: media.CodeInvalid},
		{name: "MaxFPS 240", mod: layer(1, func(l *media.LayerConfig) { l.MaxFPS = 240 })},
		{name: "f at 100 kbit/s", mod: layer(0, func(l *media.LayerConfig) { l.Bitrate = 100_000 })},
		{name: "f at 50 Mbit/s", mod: layer(0, func(l *media.LayerConfig) { l.Bitrate = 50_000_000 })},
		{name: "f below 100 kbit/s", mod: layer(0, func(l *media.LayerConfig) { l.Bitrate = 99_999 }), want: media.CodeInvalid},
		{name: "f above 50 Mbit/s", mod: layer(0, func(l *media.LayerConfig) { l.Bitrate = 50_000_001 }), want: media.CodeInvalid},
		{name: "q at 50 kbit/s", mod: layer(1, func(l *media.LayerConfig) { l.Bitrate = 50_000 })},
		{name: "q at 2 Mbit/s", mod: layer(1, func(l *media.LayerConfig) { l.Bitrate = 2_000_000 })},
		{name: "q below 50 kbit/s", mod: layer(1, func(l *media.LayerConfig) { l.Bitrate = 49_999 }), want: media.CodeInvalid},
		{name: "q above 2 Mbit/s", mod: layer(1, func(l *media.LayerConfig) { l.Bitrate = 2_000_001 }), want: media.CodeInvalid},
		{name: "bitrate 0", mod: layer(0, func(l *media.LayerConfig) { l.Bitrate = 0 }), want: media.CodeInvalid},
		{name: "profile 42e0", mod: func(c *media.Config) { c.Profile = "42e0" }},
		{name: "no profile", mod: func(c *media.Config) { c.Profile = "" }, want: media.CodeInvalid},
		{name: "profile 640c", mod: func(c *media.Config) { c.Profile = "640c" }, want: media.CodeInvalid},
		{name: "profile 4d00", mod: func(c *media.Config) { c.Profile = "4d00" }, want: media.CodeInvalid},
		{name: "decodable 6400", engine: EngineConfig{Mode: Decodable}, want: media.CodeInvalid},
		{name: "decodable 42e0", engine: EngineConfig{Mode: Decodable, SourceSize: [2]int{640, 360}},
			mod: func(c *media.Config) { c.Profile = "42e0" }},
		{name: "unknown mode", engine: EngineConfig{Mode: 9}, want: media.CodeInvalid},
		{name: "no preset", mod: func(c *media.Config) { c.Preset = "" }},
		{name: "preset text", mod: func(c *media.Config) { c.Preset = "text" }},
		{name: "unknown preset", mod: func(c *media.Config) { c.Preset = "fast" }, want: media.CodeInvalid},
		{name: "encoder fake", mod: func(c *media.Config) { c.Encoder = "fake" }},
		{name: "encoder nvenc", mod: func(c *media.Config) { c.Encoder = "nvenc" }, want: media.CodeNoEncoder,
			reason: "forced_failed"},
		{name: "unknown encoder", mod: func(c *media.Config) { c.Encoder = "x264" }, want: media.CodeInvalid},
		{name: "no tested encoder", engine: EngineConfig{Probe: media.Probe{Encoders: []media.EncoderInfo{
			{Name: "nvenc", Status: media.CodeDependency, Reason: "driver_too_old"},
		}}}, want: media.CodeNoEncoder, reason: "open_failed"},
		{name: "audio exclude-apps", mod: func(c *media.Config) { c.Audio.Mode = media.AudioExcludeApps }},
		{name: "audio test tone", mod: func(c *media.Config) { c.Audio.Mode = media.AudioTestTone }},
		{name: "audio everything", mod: func(c *media.Config) { c.Audio.Mode = media.AudioEverything },
			want: media.CodeInvalid},
		{name: "audio off", mod: func(c *media.Config) { c.Audio.Mode = media.AudioOff }, want: media.CodeInvalid},
		{name: "audio at 32 kbit/s", mod: func(c *media.Config) { c.Audio.BitrateBPS = 32_000 }},
		{name: "audio at 510 kbit/s", mod: func(c *media.Config) { c.Audio.BitrateBPS = 510_000 }},
		{name: "audio below 32 kbit/s", mod: func(c *media.Config) { c.Audio.BitrateBPS = 31_999 },
			want: media.CodeInvalid},
		{name: "audio above 510 kbit/s", mod: func(c *media.Config) { c.Audio.BitrateBPS = 510_001 },
			want: media.CodeInvalid},
		{name: "513 rules", mod: func(c *media.Config) { c.Audio.Exclusions.Rules = make([]media.Rule, 513) },
			want: media.CodeInvalid},
		{name: "above level 6.2", engine: EngineConfig{SourceSize: [2]int{16384, 8704}},
			mod: layer(0, func(l *media.LayerConfig) { l.MaxPixels = 16384 * 8704 }), want: media.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(tc.engine)
			defer e.Close()
			cfg := testConfig()
			if tc.mod != nil {
				tc.mod(&cfg)
			}
			s, err := e.Start(t.Context(), cfg)
			if codeOf(err) != tc.want || (tc.want == "") != (err == nil) {
				t.Fatalf("Start = %v, want %q", err, tc.want)
			}
			if err != nil {
				if me, _ := errors.AsType[*media.Error](err); me.Op != "start" || me.Reason != tc.reason {
					t.Errorf("Start = %#v, want Op start and Reason %q", me, tc.reason)
				}
				if e.Session() != nil {
					t.Error("a failed Start left a session")
				}
				return
			}
			_ = s.Stop()
		})
	}
}

func TestStartBusyAndCtx(t *testing.T) {
	e := NewEngine(EngineConfig{})
	defer e.Close()
	s := mustStart(t, e, testConfig())
	audioOnly := media.Config{Audio: &media.AudioConfig{}}
	if _, err := e.Start(t.Context(), audioOnly); !errors.Is(err, media.ErrBusy) {
		t.Errorf("a second Start: %v, want busy", err)
	}
	_ = s.Stop()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Start(ctx, testConfig()); !errors.Is(err, context.Canceled) {
		t.Errorf("Start with a cancelled ctx: %v", err)
	}
	s2 := mustStart(t, e, audioOnly)
	if e.Session() != s2 {
		t.Error("Session() is not the new session")
	}
}

func TestScaleToFit(t *testing.T) {
	for _, tc := range []struct {
		w, h, budget int
		ww, wh       int
		ok           bool
	}{
		{1920, 1080, 1920 * 1080, 1920, 1080, true},
		{1920, 1080, 4000 * 4000, 1920, 1080, true}, // never upscaled
		{1920, 1080, 1280 * 720, 1280, 720, true},
		{1920, 1080, 640 * 360, 640, 360, true},
		{1920, 1080, 960 * 540, 960, 540, true},
		{1920, 1080, 1_000_000, 1332, 748, true},
		{2560, 1440, 1920 * 1080, 1920, 1080, true},
		{1366, 768, 640 * 360, 640, 358, true},
		{1919, 1079, 1920 * 1080, 1918, 1078, true}, // even sides
		{1920, 1080, 3, 0, 0, false},
		{100, 100, 4, 2, 2, true},
	} {
		w, h, ok := scaleToFit(tc.w, tc.h, tc.budget)
		if ok != tc.ok || ok && (w != tc.ww || h != tc.wh) {
			t.Errorf("scaleToFit(%d, %d, %d) = %d, %d, %v, want %d, %d, %v", tc.w, tc.h, tc.budget, w, h, ok, tc.ww,
				tc.wh, tc.ok)
		}
		if ok && (w%2 != 0 || h%2 != 0 || w*h > tc.budget || w > tc.w || h > tc.h) {
			t.Errorf("scaleToFit(%d, %d, %d) = %dx%d breaks a rule", tc.w, tc.h, tc.budget, w, h)
		}
	}
}
