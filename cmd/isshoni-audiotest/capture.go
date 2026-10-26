package main

import (
	"math"
	"slices"
	"sort"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is the portable half of a case's capture (docs/m2/05-windows-audio.md §18.6): it builds the tap timeline
// from the session's packets and turns the 250 ms report samples into per-role tracks, keyed by fixture pid. The
// Windows driver feeds it packets, reports, levels and the fixtures' pids and times; everything it decides (which
// session is which fixture, which events are this case's, which gaps lost audio) is tested on every OS.

// The tap layout: one PCM-tap chunk is 10 ms of 48 kHz stereo float32 (05 §10.2, media.KindPCM).
const (
	pcmChunkFrames = 480
	pcmChunkBytes  = pcmChunkFrames * 2 * 4
	pcmChunkNS     = 10_000_000
)

// foreignPeakDB is the peak above which a non-fixture app that is heard counts as "another app was heard"
// (05 §18.6).
const foreignPeakDB = -60.0

// foreignSample is one report sample in which a non-fixture app was heard above foreignPeakDB.
type foreignSample struct {
	t   float64
	app string // redacted: always otherRedacted
}

// tapGap is a jump in the PCM tap's capture times: at is the capture time (session clock) of the chunk after it,
// missing the time it skipped beyond one chunk.
type tapGap struct {
	at, missing int64
}

// opusPkt is one captured Opus packet: its capture time (session clock), bytes and reinit flag.
type opusPkt struct {
	captureNS int64
	data      []byte
	reinit    bool
}

// eventKey identifies one engine event; every report repeats the last 300, so the capture keeps each once.
type eventKey struct {
	tNS  int64
	code string
	app  string
}

// capture accumulates one case's tap and report samples and builds its recording.
type capture struct {
	offset  int64 // engine clock − session clock (05 §13.2)
	watchNS int64 // the engine clock when the case's watcher started: events before it belong to earlier cases

	left, right []float64
	tap0, last  int64 // the first and the latest tap chunk's capture time (session clock)
	haveTap     bool
	gaps        []tapGap
	opus        []opusPkt

	roles     map[string]*roleTrack
	roleApp   map[string]string // role → its sessions' app id (the latest seen)
	engineAt  []engineSample
	bannerAt  []bannerSample
	events    []recEvent
	seen      map[eventKey]bool
	counters  map[string]int
	foreign   []foreignSample
	keptOther map[string]bool // exe names of kept-out sessions that are no fixture's (console only, never reported)
}

func newCapture(offset, watchNS int64) *capture {
	return &capture{
		offset: offset, watchNS: watchNS,
		roles: map[string]*roleTrack{}, roleApp: map[string]string{}, seen: map[eventKey]bool{},
		counters: map[string]int{}, keptOther: map[string]bool{},
	}
}

// packet adds one session packet: a PCM-tap chunk extends the timeline (a jump in capture times is zero-padded, so
// a sample's index stays its capture time), an Opus packet is kept for case 21 and for telling a new timestamp grid
// from lost audio (05 §10.4: the Opus packet after a new grid carries Reinit).
func (c *capture) packet(pkt media.Packet) {
	switch pkt.Kind {
	case media.KindPCM:
		if len(pkt.Data) != pcmChunkBytes {
			return
		}
		if !c.haveTap {
			c.tap0, c.haveTap = pkt.CaptureNS, true
		} else if jump := pkt.CaptureNS - c.last - pcmChunkNS; jump > pcmChunkNS/10 {
			c.gaps = append(c.gaps, tapGap{at: pkt.CaptureNS, missing: jump})
			pad := int(jump * sampleRate / 1e9)
			c.left = append(c.left, make([]float64, pad)...)
			c.right = append(c.right, make([]float64, pad)...)
		}
		c.last = pkt.CaptureNS
		l, r := decodePCM(pkt.Data)
		c.left = append(c.left, l...)
		c.right = append(c.right, r...)
	case media.KindAudio:
		c.opus = append(c.opus, opusPkt{captureNS: pkt.CaptureNS, data: append([]byte(nil), pkt.Data...),
			reinit: pkt.Reinit})
	}
}

// now is the time since the recording started (the first tap chunk), in seconds.
func (c *capture) now() float64 {
	if !c.haveTap {
		return 0
	}
	return float64(c.last-c.tap0) / 1e9
}

// engineToTap maps an engine-clock time to seconds since the recording start (05 §13.2's offset).
func (c *capture) engineToTap(engineNS int64) float64 {
	if !c.haveTap {
		return 0
	}
	return float64(engineNS-c.offset-c.tap0) / 1e9
}

// sample adds one report sample (05 §18.6: every 250 ms). pids maps the fixtures' pids to their roles (the harness's
// own pid to selfRole when it plays); a session is a fixture's only by its pid. banners are FriendsHear's banner
// codes; levels, if not nil, are the app levels of the same moment (another app heard above −60 dBFS). A sample
// before the first tap chunk is dropped.
func (c *capture) sample(r media.AudioReport, banners []string, levels *media.AudioLevels, pids map[uint32]string) {
	if !c.haveTap {
		return // times are relative to the first tap chunk: nothing can be placed before it
	}
	t := c.now()
	if r.QPCNanos != 0 {
		t = c.engineToTap(r.QPCNanos)
	}
	c.engineAt = append(c.engineAt, engineSample{t: t, value: r.Engine})
	c.bannerAt = append(c.bannerAt, bannerSample{t: t, codes: slices.Clone(banners)})

	rows := make(map[string]media.AudioApp, len(r.Apps))
	for _, a := range r.Apps {
		rows[a.ID] = a
	}
	states := map[string]*roleSampleState{}
	for _, s := range r.Sessions {
		role, ok := pids[s.PID]
		if !ok {
			if s.Verdict == controlKeptOut && s.Reason != "isshoni" {
				c.keptOther[s.Exe] = true
			}
			continue
		}
		if s.Flow != "" && s.Flow != "render" {
			continue // a fixture's microphone session (WA62b's mic cases) says nothing about its playback
		}
		c.roleApp[role] = s.AppID
		st := states[role]
		active := s.State == "active"
		if st != nil && st.active && !active {
			continue // keep the active session of a role that has several
		}
		st = &roleSampleState{t: t, active: active, verdict: s.Verdict, reason: s.Reason, note: s.Note}
		if row, ok := rows[s.AppID]; ok {
			st.row, st.rowVerdict, st.rowReason, st.rowDetail = true, row.Verdict, row.Reason, row.Detail
		}
		states[role] = st
	}
	for role, st := range states {
		tr := c.roles[role]
		if tr == nil {
			tr = &roleTrack{}
			c.roles[role] = tr
		}
		tr.samples = append(tr.samples, *st)
	}

	for _, ev := range r.Events {
		key := eventKey{tNS: ev.TNanos, code: ev.Code, app: ev.App}
		if ev.TNanos < c.watchNS || c.seen[key] {
			continue
		}
		c.seen[key] = true
		c.events = append(c.events, recEvent{t: c.engineToTap(ev.TNanos), code: ev.Code, app: ev.App})
	}
	for k, v := range reportCounters(r) {
		c.counters[k] = v
	}

	if levels != nil {
		fixtureApps := map[string]bool{}
		for _, app := range c.roleApp {
			fixtureApps[app] = true
		}
		for _, l := range levels.Apps {
			if fixtureApps[l.AppID] || l.AppID == "isshoni" || float64(l.PeakDB) <= foreignPeakDB {
				continue
			}
			if row, ok := rows[l.AppID]; ok && row.Verdict == controlHeard {
				c.foreign = append(c.foreign, foreignSample{t: t, app: otherRedacted})
			}
		}
	}
}

// keptOutOthers are the exe names of kept-out sessions that belong to no fixture, sorted (console only).
func (c *capture) keptOutOthers() []string {
	out := make([]string, 0, len(c.keptOther))
	for exe := range c.keptOther {
		out = append(out, exe)
	}
	sort.Strings(out)
	return out
}

// captureEnd is what the driver knows about the case beyond the samples: the fixtures' logged times on the engine
// clock (05 §18.3), and the premises it checked.
type captureEnd struct {
	dur         float64 // the planned recording length (s)
	plan        map[string]planSpan
	reparented  bool             // every reparented fixture really was started under explorer.exe
	silenceFail bool             // the silence pre-check failed (05 §18.4)
	keptOutPre  bool             // a non-fixture process was kept out before the fixtures started (05 §18.4)
	firstRender map[string]int64 // role → firstRenderNs
	start       map[string]int64 // role → the process's start (its {"startNs"} line)
	exit        map[string]int64 // role → when the harness saw the process exit
	setStamp    int64            // case 15's T_set: the first recorder stamp after SetAppDecision
	setStampOK  bool
}

// gridSlackNS is how far an Opus Reinit packet's time may lie from a tap jump for the jump to count as a new
// timestamp grid (05 §10.4): the packet starts at the new grid's first chunk minus the encoder's lookahead.
const gridSlackNS = 40_000_000

// lostGapMs is the largest jump in tap capture times that no new grid explains: lost audio (05 §18.6). A recording
// that stops short of dur counts its missing tail as lost too.
func (c *capture) lostGapMs(dur float64) int {
	var reinits []int64
	for i, p := range c.opus {
		if i > 0 && p.reinit {
			reinits = append(reinits, p.captureNS)
		}
	}
	lost := int64(0)
	for _, g := range c.gaps {
		newGrid := false
		for _, r := range reinits {
			if r >= g.at-gridSlackNS && r <= g.at+gridSlackNS/2 {
				newGrid = true
				break
			}
		}
		if !newGrid && g.missing > lost {
			lost = g.missing
		}
	}
	if short := int64(math.Round((dur - float64(len(c.left))/sampleRate) * 1e9)); short > lost {
		lost = short
	}
	return int(lost / 1_000_000)
}

// finish builds the recording for the portable verdict.
func (c *capture) finish(cs caseSpec, in captureEnd) recording {
	rc := recording{
		left: c.left, right: c.right, recordDur: in.dur, tap0NS: c.tap0,
		reparented: in.reparented, silenceFail: in.silenceFail,
		keptOutOther: in.keptOutPre || len(c.keptOther) > 0,
		lostGapMs:    c.lostGapMs(in.dur), foreign: c.foreign, roles: c.roles, roleApp: c.roleApp,
		roleStart: map[string]float64{}, engineAt: c.engineAt, bannerAt: c.bannerAt, events: c.events,
		firstRender: map[string]float64{}, counters: c.counters, opus: c.opus,
	}
	for role, ns := range in.firstRender {
		rc.firstRender[role] = c.engineToTap(ns)
	}
	for role, ns := range in.start {
		rc.roleStart[role] = c.engineToTap(ns)
	}
	if in.setStampOK {
		rc.setStamp, rc.setStampOK = c.engineToTap(in.setStamp), true
	}
	// The positive controls (05 §18.6): every role the case checks must appear active, and must not exit before its
	// planned end (or the end of the recording).
	for _, role := range checkedRoles(cs) {
		tr := c.roles[role]
		if tr == nil {
			tr = &roleTrack{}
			c.roles[role] = tr
		}
		tr.neverActive = !anyActive(tr)
		exitNS, exited := in.exit[role]
		span, planned := in.plan[role]
		if !exited || !planned {
			continue
		}
		exitT := c.engineToTap(exitNS)
		start := span.start
		if s, ok := rc.roleStart[role]; ok {
			start = s
		}
		if exitT < math.Min(start+span.life, in.dur)-exitSlackSec {
			tr.exitedEarly = true
		}
	}
	return rc
}

// exitSlackSec is how much earlier than planned a fixture may exit before it counts as exited early.
const exitSlackSec = 0.25

func anyActive(tr *roleTrack) bool {
	for _, s := range tr.samples {
		if s.active {
			return true
		}
	}
	return false
}

// decodePCM splits a 3840-byte tap chunk into left and right float64 (interleaved little-endian float32).
func decodePCM(data []byte) (left, right []float64) {
	left = make([]float64, pcmChunkFrames)
	right = make([]float64, pcmChunkFrames)
	for i := range pcmChunkFrames {
		left[i] = float64(math.Float32frombits(le32(data[i*8:])))
		right[i] = float64(math.Float32frombits(le32(data[i*8+4:])))
	}
	return left, right
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// reportCounters is the mixer counters the verdict reads (fastpathLateCuts, …).
func reportCounters(r media.AudioReport) map[string]int {
	return map[string]int{
		"underruns":        r.Counters.Underruns,
		"trims":            r.Counters.Trims,
		"fastpathExits":    r.Counters.FastpathExits,
		"fastpathLateCuts": r.Counters.FastpathLateCuts,
		"mixerResyncs":     r.Counters.MixerResyncs,
	}
}
