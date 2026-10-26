package main

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// These tests cover the portable capture (capture.go): report sessions map to fixture roles by pid only, the event
// ring every report repeats is kept once and only from the case's own watcher start, other apps heard above
// −60 dBFS are found from the levels, and tap gaps are zero-padded and told apart from a new timestamp grid.

const capOffset = 5_000_000_000 // engine clock − session clock in these tests

// pcmChunk is a KindPCM packet at captureNS (session clock) whose samples all hold v.
func pcmChunk(captureNS int64, v float32) media.Packet {
	data := make([]byte, pcmChunkBytes)
	for i := 0; i < len(data); i += 4 {
		binary.LittleEndian.PutUint32(data[i:], math.Float32bits(v))
	}
	return media.Packet{Kind: media.KindPCM, CaptureNS: captureNS, Data: data}
}

// newTestCapture is a capture whose recording starts at session time 1 s (engine 6 s) and whose watcher started at
// engine 5.5 s.
func newTestCapture() *capture {
	c := newCapture(capOffset, capOffset+500_000_000)
	c.packet(pcmChunk(1_000_000_000, 0))
	return c
}

// engineAt is the engine clock of recording time t in a test capture.
func engineAt(t float64) int64 { return capOffset + 1_000_000_000 + int64(t*1e9) }

// TestCaptureMapsSessionsByPID is 05 §18.3's "per fixture pid": a fixture's sessions are its only by pid, so helpers
// without an app row of their own, fixtures that share an exe name, and real processes with a fixture's exe name are
// all told apart.
func TestCaptureMapsSessionsByPID(t *testing.T) {
	c := newTestCapture()
	r := media.AudioReport{
		QPCNanos: engineAt(2), Engine: "include-set",
		Apps: []media.AudioApp{
			{ID: "discord", Exe: "discord.exe", Verdict: "kept_out", Reason: "builtin"},
			{ID: "exe:msedge.exe", Exe: "msedge.exe", Verdict: "kept_out", Reason: "viewer"},
		},
		Sessions: []media.AudioSession{
			// The helper's session carries the voice app's id; no row has its exe.
			{PID: 101, Exe: "audiohelper.exe", AppID: "discord", Flow: "render", State: "active",
				Verdict: "kept_out", Reason: "builtin"},
			// Two fixtures with the same exe name.
			{PID: 201, Exe: "msedge.exe", AppID: "exe:msedge.exe", Flow: "render", State: "active",
				Verdict: "kept_out", Reason: "viewer"},
			{PID: 202, Exe: "msedge.exe", AppID: "exe:msedge.exe", Flow: "render", State: "inactive",
				Verdict: "kept_out", Reason: "viewer"},
			// A real Edge, not a fixture.
			{PID: 999, Exe: "msedge.exe", AppID: "exe:msedge.exe", Flow: "render", State: "active",
				Verdict: "kept_out", Reason: "viewer"},
			// A real voice app, kept out: named on the console for the fast-path cases.
			{PID: 777, Exe: "teamspeak.exe", AppID: "exe:teamspeak.exe", Flow: "render", State: "inactive",
				Verdict: "kept_out", Reason: "builtin"},
		},
	}
	pids := map[uint32]string{101: "audiohelper", 201: "isshoniTab", 202: "otherTab"}
	c.sample(r, fhState{banners: []string{"exclusion-unavailable"}}, nil, pids)

	if got := len(c.roles); got != 3 {
		t.Fatalf("%d roles sampled, want 3 (the real Edge and TeamSpeak are no fixture's)", got)
	}
	h := c.roles["audiohelper"].samples[0]
	if !h.active || h.verdict != "kept_out" || h.reason != "builtin" || !h.row || h.rowVerdict != "kept_out" {
		t.Fatalf("the helper's sample %+v, want its session's verdict and the discord row", h)
	}
	if h.t != 2 {
		t.Fatalf("the sample's time %.3f, want 2 (the report's own engine-clock time)", h.t)
	}
	if !c.roles["isshoniTab"].samples[0].active || c.roles["otherTab"].samples[0].active {
		t.Fatal("two fixtures with one exe name got each other's session state")
	}
	if c.roleApp["audiohelper"] != "discord" {
		t.Fatalf("the helper's app id %q, want discord", c.roleApp["audiohelper"])
	}
	if got := c.keptOutOthers(); !slices.Equal(got, []string{"msedge.exe", "teamspeak.exe"}) {
		t.Fatalf("kept-out non-fixtures %v", got)
	}
	if len(c.bannerAt) != 1 || c.bannerAt[0].codes[0] != "exclusion-unavailable" || c.engineAt[0].value != "include-set" {
		t.Fatalf("banner and engine samples %+v %+v", c.bannerAt, c.engineAt)
	}
}

// TestCaptureEventsOnceAndFresh: every report repeats the last 300 events; the capture keeps each event once, and
// drops the ones from before the case's watcher started (an earlier case's).
func TestCaptureEventsOnceAndFresh(t *testing.T) {
	c := newTestCapture()
	stale := media.AudioEvent{TNanos: capOffset, Code: "session_created", App: "discord"} // before the watcher
	ev1 := media.AudioEvent{TNanos: engineAt(0.2), Code: "session_created", App: "exe:game.exe"}
	ev2 := media.AudioEvent{TNanos: engineAt(3.01), Code: "session_created", App: "discord"}
	ev3 := media.AudioEvent{TNanos: engineAt(3.02), Code: "fastpath_exit"}
	c.sample(media.AudioReport{Events: []media.AudioEvent{stale, ev1}}, fhState{}, nil, nil)
	c.sample(media.AudioReport{Events: []media.AudioEvent{stale, ev1, ev2}}, fhState{}, nil, nil)
	c.sample(media.AudioReport{Events: []media.AudioEvent{stale, ev1, ev2, ev3}}, fhState{}, nil, nil)
	if len(c.events) != 3 {
		t.Fatalf("%d events, want 3 (each once, the stale one dropped): %+v", len(c.events), c.events)
	}
	if e := c.events[1]; e.code != "session_created" || e.app != "discord" || math.Abs(e.t-3.01) > 1e-9 {
		t.Fatalf("event %+v, want discord's session_created at 3.01 s", e)
	}
}

// TestCaptureForeign: an app that is no fixture's, heard, with a peak above −60 dBFS is "another app heard"; a
// quieter one, a kept-out one, a fixture's and isshoni's are not (05 §18.6).
func TestCaptureForeign(t *testing.T) {
	c := newTestCapture()
	r := media.AudioReport{
		Apps: []media.AudioApp{
			{ID: "exe:game.exe", Verdict: "heard"}, {ID: "exe:spotify.exe", Verdict: "heard"},
			{ID: "exe:quiet.exe", Verdict: "heard"}, {ID: "discord", Verdict: "kept_out"},
		},
		Sessions: []media.AudioSession{{PID: 1, AppID: "exe:game.exe", Flow: "render", State: "active"}},
	}
	levels := &media.AudioLevels{Apps: []media.AudioLevel{
		{AppID: "exe:game.exe", PeakDB: -18}, {AppID: "isshoni", PeakDB: -20}, {AppID: "discord", PeakDB: -18},
		{AppID: "exe:quiet.exe", PeakDB: -65},
	}}
	pids := map[uint32]string{1: "game"}
	c.sample(r, fhState{}, levels, pids)
	if len(c.foreign) != 0 {
		t.Fatalf("foreign %+v, want none", c.foreign)
	}
	levels.Apps = append(levels.Apps, media.AudioLevel{AppID: "exe:spotify.exe", PeakDB: -30})
	c.sample(r, fhState{}, levels, pids)
	if len(c.foreign) != 1 || c.foreign[0].app != otherRedacted {
		t.Fatalf("foreign %+v, want one redacted entry", c.foreign)
	}
}

// TestCaptureGaps: a jump in tap capture times is zero-padded (a sample's index stays its capture time) and counts
// as lost audio, unless an Opus Reinit packet marks a new grid there (05 §10.4); a recording that stops short of its
// length loses its tail.
func TestCaptureGaps(t *testing.T) {
	build := func(withReinit bool) *capture {
		c := newCapture(0, 0)
		c.packet(media.Packet{Kind: media.KindAudio, CaptureNS: 0, Reinit: true, Data: []byte{1}})
		for i := range int64(100) {
			c.packet(pcmChunk(i*pcmChunkNS, 0.1))
		}
		// 50 ms of capture time skipped: the next chunk is at 1.05 s instead of 1.00 s.
		if withReinit {
			c.packet(media.Packet{Kind: media.KindAudio, CaptureNS: 1_050_000_000 - 6_500_000, Reinit: true,
				Data: []byte{1}})
		}
		for i := range int64(100) {
			c.packet(pcmChunk(1_050_000_000+i*pcmChunkNS, 0.2))
		}
		return c
	}
	c := build(false)
	if got := len(c.left); got != int(2.05*sampleRate) {
		t.Fatalf("timeline %d samples, want %d (padded)", got, int(2.05*sampleRate))
	}
	if c.left[int(1.02*sampleRate)] != 0 || c.left[int(1.06*sampleRate)] != float64(float32(0.2)) {
		t.Fatal("the gap is not zero-padded at its capture time")
	}
	if got := c.lostGapMs(2.05); got != 50 {
		t.Fatalf("lost %d ms, want 50", got)
	}
	if got := build(true).lostGapMs(2.05); got != 0 {
		t.Fatalf("a new grid lost %d ms, want 0", got)
	}
	if got := build(true).lostGapMs(2.5); got != 450 {
		t.Fatalf("a recording 450 ms short lost %d ms, want 450", got)
	}
}

// TestCaptureFinishControls: finish marks a checked role that never had an active session, and one whose process
// exited before its planned end, and maps the logged times to the recording's timeline.
func TestCaptureFinishControls(t *testing.T) {
	cs, _ := caseByName("builtin-voice")
	c := newTestCapture()
	for i := range int64(800) {
		c.packet(pcmChunk(1_000_000_000+(i+1)*pcmChunkNS, 0))
	}
	c.sample(media.AudioReport{QPCNanos: engineAt(2), Sessions: []media.AudioSession{
		{PID: 1, AppID: "exe:game.exe", Flow: "render", State: "active", Verdict: "heard"},
		{PID: 2, AppID: "discord", Flow: "render", State: "inactive", Verdict: "kept_out"},
	}}, fhState{}, nil, map[uint32]string{1: "game", 2: "voice"})
	rec := c.finish(cs, captureEnd{
		dur: 8, plan: planSpans(cs, 8), reparented: true,
		firstRender: map[string]int64{"game": engineAt(-0.95)},
		start:       map[string]int64{"game": engineAt(-1)},
		exit:        map[string]int64{"game": engineAt(4)},
		setStamp:    engineAt(4.05), setStampOK: true,
	})
	if !rec.roles["game"].exitedEarly {
		t.Fatal("the game exited at 4 s of a planned 10 s: want exitedEarly")
	}
	if !rec.roles["voice"].neverActive {
		t.Fatal("the voice app was never active: want neverActive")
	}
	if math.Abs(rec.firstRender["game"]+0.95) > 1e-9 || math.Abs(rec.roleStart["game"]+1) > 1e-9 ||
		math.Abs(rec.setStamp-4.05) > 1e-9 {
		t.Fatalf("times %v %v %v not on the recording's timeline", rec.firstRender, rec.roleStart, rec.setStamp)
	}
	if rec.lostGapMs != 0 {
		t.Fatalf("lost %d ms", rec.lostGapMs)
	}
	if cr := evaluate(cs, rec, gateConfig{FastPath: true}); cr.Result != resultInconclusive {
		t.Fatalf("want INCONCLUSIVE, got %s", cr.Result)
	}
	// Exiting at the end of the recording is no early exit.
	c2 := newTestCapture()
	for i := range int64(800) {
		c2.packet(pcmChunk(1_000_000_000+(i+1)*pcmChunkNS, 0))
	}
	rec2 := c2.finish(cs, captureEnd{dur: 8, plan: planSpans(cs, 8), exit: map[string]int64{"game": engineAt(8.1)}})
	if rec2.roles["game"].exitedEarly {
		t.Fatal("an exit after the recording ended counted as early")
	}
}
