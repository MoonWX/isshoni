package main

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// What the run measures (02 §15.2), after the warm-up:
//   - Server data, from GET /api/v1/admin/dashboard every 5 s and once before the clients joined (the baseline
//     RSS): process CPU ÷ (wall × NumCPU) on average and per sample interval; (RSS − baseline RSS) ÷ DownTracks; and
//     the SFU's RTP-level egress (media.egressBps, not transfer.egressBps), against what the subscriptions should
//     take: the publishers' measured payload rate of each layer they watch, and of the audio.
//   - Client data: each subscriber track's loss before NACK/RTX (gaps) and after (still missing after 1 s), from
//     sfutest's Recorders; the keyframe requests (PLI, FIR) that reach each publisher layer, from the Publishers'
//     counters every second; per focus move, the time from the subscribe.update to the first packet of the new
//     layer, which must start a keyframe on its SPS and, once repairs have had their chance, follow the packet in
//     front of it with the next sequence number and a later timestamp (seq continuous per track); and the offset of
//     every flash frame from its beep at the viewers, both mapped to NTP through the sender reports the SFU
//     forwarded.

// The kinds of a track.
const (
	videoKind = webrtc.RTPCodecTypeVideo
	audioKind = webrtc.RTPCodecTypeAudio
)

// Clock rates of the RTP timestamps.
const (
	videoClock = 90000
	audioClock = 48000
)

// pliWindow is the window of the PLI storm check: never more than limits.pliPer10s requests in any 10 s.
const pliWindow = 10 * time.Second

// switchPoll is how often a switch measurement looks at the Recorder.
const switchPoll = 20 * time.Millisecond

// switchSettle is how long a switch measurement waits after the new layer's first packet before it checks the
// switch's start: the Viewer asks for a missing packet again until it has been missing for FinalLossAfter, and the
// last retransmission may still be on its way then. What is missing after it stays missing.
const switchSettle = sfutest.FinalLossAfter + 200*time.Millisecond

// dashSample is one reading of the dashboard.
type dashSample struct {
	at         time.Time // when the tool read it
	generated  time.Time // the dashboard's generatedAt
	cpuSeconds float64
	rss        int64
	numCPU     int
	egressBps  int64
	downTracks int
	// counts are the subscribers' desired subscriptions at that time, by share: what the egress should be.
	counts map[string]wantCount
}

// wantCount counts the subscriptions of one share by what they take.
type wantCount struct{ high, low, audio int }

// trackKey names a publisher's sent stream: a share's video layer, or its audio (layer "").
type trackKey struct{ share, layer string }

// trackCounts are a sent stream's counters at a sample.
type trackCounts struct {
	octets uint64 // payload bytes of the media stream
	plis   int    // PLIs and FIRs received
}

// pubSample is one reading of every Publisher's counters.
type pubSample struct {
	at     time.Time
	tracks map[trackKey]trackCounts
}

// switchSample is one measured layer switch.
type switchSample struct {
	latency time.Duration
	// The new layer's first frame: its timestamp, the packet with its marker (its slice's start), and its first
	// packet as checkSwitch found it (the SPS in front of the slice, or the slice).
	ts         uint32
	seq, start uint16
	keyMarked  bool // the marker says the frame is a keyframe
	keyframe   bool // the new layer starts on a keyframe with its SPS in front
	lostStart  bool // the SPS in front never arrived, so the start could not be checked (the loss check counts it)
	// verified: the switch was checked again once repairs had had their chance (switchSettle), so seqGap is final.
	// A switch that the run's end cut short keeps what its first look found.
	verified bool
	seqGap   bool // the sequence number in front of the new layer's first packet never arrived (verified only)
	tsBack   bool // the new layer's first frame does not come after the packet in front of it (02 §9.4)
}

// avKey identifies one flash frame at one subscriber.
type avKey struct {
	sub          int
	share, layer string
	frame        uint32
}

// meter collects the measurements of a run.
type meter struct {
	lt *loadTest

	mu          sync.Mutex
	baselineRSS int64
	warmEnd     time.Time // set by start, before the drivers run
	dash        []dashSample
	dashErrs    int
	pub         []pubSample
	warmMarks   map[*sfutest.Recorder]sfutest.RecorderStats
	switches    []switchSample
	av          []time.Duration // |flash − beep|
	avSeen      map[avKey]bool
	continuity  []string // timestamps that went back
}

func newMeter(lt *loadTest) *meter {
	return &meter{lt: lt, avSeen: map[avKey]bool{}}
}

// setBaseline keeps the dashboard read before the clients joined.
func (m *meter) setBaseline(d api.OpsDashboard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baselineRSS = d.Server.Process.RSSBytes
}

// start sets the time the warm-up ends.
func (m *meter) start(warmEnd time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warmEnd = warmEnd
}

// inWindow reports whether t is after the warm-up.
func (m *meter) inWindow(t time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !t.Before(m.warmEnd)
}

// sampleDashboard reads the dashboard every dashboardEvery until ctx ends. It also collects the A/V pairs of each
// subscriber's focused share, before the Recorders drop their oldest packets.
func (m *meter) sampleDashboard(ctx context.Context) {
	tick := time.NewTicker(m.lt.cfg.dashboardEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.readDashboard(ctx)
		for _, c := range m.lt.subs {
			c.mu.Lock()
			focus := c.focus
			c.mu.Unlock()
			m.collectAV(c, m.lt.shareID(focus))
		}
	}
}

// readDashboard takes one dashboard sample. A failed read is counted: more failures than samples fail the run (report).
func (m *meter) readDashboard(ctx context.Context) {
	d, err := m.lt.dashboard(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.lt.log.Warn("reading the dashboard", "err", err)
			m.mu.Lock()
			m.dashErrs++
			m.mu.Unlock()
		}
		return
	}
	s := dashSample{
		at: time.Now(), generated: d.GeneratedAt, cpuSeconds: d.Server.Process.CPUSeconds,
		rss: d.Server.Process.RSSBytes, numCPU: d.Server.Process.NumCPU, egressBps: d.Media.EgressBps,
		downTracks: d.Media.DownTracks, counts: m.lt.wantCounts(),
	}
	m.mu.Lock()
	m.dash = append(m.dash, s)
	m.mu.Unlock()
}

// wantCounts counts the subscribers' desired subscriptions by share.
func (lt *loadTest) wantCounts() map[string]wantCount {
	out := map[string]wantCount{}
	for _, c := range lt.subs {
		c.mu.Lock()
		for id, w := range c.wants {
			n := out[id]
			switch w.Video {
			case protocol.VideoLayerHigh:
				n.high++
			case protocol.VideoLayerLow:
				n.low++
			}
			if w.Audio == protocol.AudioStateOn {
				n.audio++
			}
			out[id] = n
		}
		c.mu.Unlock()
	}
	return out
}

// samplePublishers reads the Publishers' counters every statsEvery until ctx ends.
func (m *meter) samplePublishers(ctx context.Context) {
	tick := time.NewTicker(m.lt.cfg.statsEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.readPublishers()
	}
}

// readPublishers takes one sample of every Publisher's counters.
func (m *meter) readPublishers() {
	s := pubSample{at: time.Now(), tracks: map[trackKey]trackCounts{}}
	for _, c := range m.lt.pubs {
		for _, ts := range c.share.pub.Stats().Tracks {
			k := trackKey{share: c.share.id, layer: ts.Layer}
			s.tracks[k] = trackCounts{octets: ts.Octets, plis: ts.PLIs + ts.FIRs}
		}
	}
	m.mu.Lock()
	m.pub = append(m.pub, s)
	m.mu.Unlock()
}

// recorderStats returns the counters of every subscriber's Recorders now.
func (m *meter) recorderStats() map[*sfutest.Recorder]sfutest.RecorderStats {
	out := map[*sfutest.Recorder]sfutest.RecorderStats{}
	for _, c := range m.lt.subs {
		for _, r := range c.viewer.Recorders() {
			out[r] = r.Stats()
		}
	}
	return out
}

// markWarm marks the end of the warm-up: the Recorders' counters, and a sample of the server and the Publishers.
func (m *meter) markWarm(ctx context.Context) {
	marks := m.recorderStats()
	m.mu.Lock()
	m.warmMarks = marks
	m.mu.Unlock()
	m.readPublishers()
	m.readDashboard(ctx)
}

// finish takes the last samples and the last A/V pairs, and checks every track's timestamps.
func (m *meter) finish(ctx context.Context) {
	m.readPublishers()
	m.readDashboard(ctx)
	for _, c := range m.lt.subs {
		c.mu.Lock()
		focus := c.focus
		c.mu.Unlock()
		m.collectAV(c, m.lt.shareID(focus))
		for _, r := range c.viewer.Recorders() {
			if err := timestampsForward(r.Packets()); err != nil {
				m.mu.Lock()
				m.continuity = append(m.continuity, fmt.Sprintf("%s: %s of %s: %v", c.acct.name, r.Kind(), r.StreamID(), err))
				m.mu.Unlock()
			}
		}
	}
}

// measureSwitch waits until a subscriber's video of a share has switched to layer rid after a subscribe.update sent
// at sentAt, and keeps the switch when it began after the warm-up. A track that was on that layer already (the SFU
// had moved it for bandwidth) has nothing to switch. No switch by deadline is a problem of the run.
//
// A kept switch is checked once more after switchSettle, when every repair the Viewer asked for has come or never
// will: the new layer starts on its SPS, and the track's sequence numbers and timestamps run on across the switch
// (02 §15.2 "seq continuous per track", §9.4).
func (m *meter) measureSwitch(ctx context.Context, c *client, shareID, rid string, sentAt, deadline time.Time) {
	for {
		if r := c.recorder(shareID, videoKind); r != nil {
			s, state := findSwitch(r.Packets(), rid, sentAt)
			switch state {
			case switchFound:
				if !m.inWindow(sentAt) {
					return
				}
				if sleep(ctx, switchSettle) == nil {
					s.verified = checkSwitch(r.Packets(), &s)
				}
				m.mu.Lock()
				m.switches = append(m.switches, s)
				m.mu.Unlock()
				if !s.keyframe && !s.lostStart {
					c.problem("the switch of %s to layer %s did not start on a keyframe with its SPS", shareID, rid)
				}
				if s.verified && s.seqGap {
					c.log.Warn("a sequence number is missing in front of a switch", "share_id", shareID, "layer", rid,
						"missing_seq", s.start-1, "first_seq", s.start)
				}
				if s.tsBack {
					c.log.Warn("the timestamp goes back at a switch", "share_id", shareID, "layer", rid, "seq", s.seq, "ts", s.ts)
				}
				return
			case switchNotNeeded:
				return
			}
		}
		if !time.Now().Before(deadline) {
			if m.inWindow(sentAt) {
				c.problem("the video of %s did not switch to layer %s within %v", shareID, rid, deadline.Sub(sentAt))
			}
			return
		}
		if sleep(ctx, switchPoll) != nil {
			return
		}
	}
}

// What findSwitch found.
type switchState uint8

const (
	switchPending   switchState = iota // no packet of the layer since sentAt yet
	switchFound                        // the first packet of the layer since sentAt
	switchNotNeeded                    // the track was on the layer before sentAt
)

// findSwitch finds the first frame of layer rid in a track's packets (in arrival order) that arrived at or after
// sentAt, and takes a first look at how it starts (checkSwitch).
func findSwitch(pkts []sfutest.Packet, rid string, sentAt time.Time) (switchSample, switchState) {
	i := sort.Search(len(pkts), func(i int) bool { return !pkts[i].Arrival.Before(sentAt) })
	for k := i - 1; k >= 0; k-- {
		if p := pkts[k]; p.HasMark && !p.Dup {
			if p.Mark.RID == rid {
				return switchSample{}, switchNotNeeded
			}
			break
		}
	}
	first := -1
	for k := i; k < len(pkts); k++ {
		if p := pkts[k]; p.HasMark && !p.Dup && p.Mark.RID == rid {
			first = k
			break
		}
	}
	if first < 0 {
		return switchSample{}, switchPending
	}
	p := pkts[first]
	s := switchSample{latency: p.Arrival.Sub(sentAt), ts: p.TS, seq: p.Seq, keyMarked: p.Mark.Keyframe}
	checkSwitch(pkts, &s)
	return s, switchFound
}

// checkSwitch checks how the new layer's first frame of a switch starts, in a track's packets: on a keyframe (its
// marker says so) whose SPS came right in front of its slice (02 §9.4, the STAP-A of publish.Publisher), right
// behind the packet before it (the sequence numbers run on), and after it (its timestamp is later: two frames never
// share one). When the packet in front of a keyframe's slice never arrived, the start can't be told from a gap:
// that is loss, which the loss check counts. It returns false, and leaves s as it was, when the frame's marked packet
// is no longer among the packets.
func checkSwitch(pkts []sfutest.Packet, s *switchSample) bool {
	bySeq := make(map[uint16]sfutest.Packet, len(pkts))
	for _, p := range pkts {
		if !p.Dup {
			bySeq[p.Seq] = p
		}
	}
	if p, ok := bySeq[s.seq]; !ok || p.TS != s.ts {
		return false
	}
	start := s.seq
	if p, ok := bySeq[start-1]; ok && p.TS == s.ts && p.KeyStart {
		start--
	}
	first := bySeq[start]
	prev, havePrev := bySeq[start-1]
	s.start = start
	s.keyframe = s.keyMarked && first.KeyStart
	s.lostStart = s.keyMarked && !first.KeyStart && !havePrev
	s.seqGap = !havePrev && !s.lostStart
	s.tsBack = havePrev && int32(s.ts-prev.TS) <= 0 //nolint:gosec // G115: RTP timestamps wrap; the difference is read as signed
	return true
}

// hasLayer reports whether a track has a whole frame's start of layer rid.
func hasLayer(pkts []sfutest.Packet, rid string) bool {
	for i := len(pkts) - 1; i >= 0; i-- {
		if pkts[i].HasMark && pkts[i].Mark.RID == rid {
			return true
		}
	}
	return false
}

// timestampsForward checks that a track's timestamps never go back in sequence-number order: a layer switch keeps
// the publisher's timeline (02 §9.4).
func timestampsForward(pkts []sfutest.Packet) error {
	ordered := sfutest.InSeqOrder(pkts)
	for i := 1; i < len(ordered); i++ {
		a, b := ordered[i-1], ordered[i]
		if b.Dup || a.Seq == b.Seq {
			continue
		}
		if int32(b.TS-a.TS) < 0 { //nolint:gosec // G115: RTP timestamps wrap; the difference is read as signed
			return fmt.Errorf("the timestamp goes back at seq %d: %d → %d", b.Seq, a.TS, b.TS)
		}
	}
	return nil
}

// collectAV adds the A/V pairs of a share at a subscriber that are new and came after the warm-up.
func (m *meter) collectAV(c *client, shareID string) {
	audio := c.recorder(shareID, audioKind)
	if audio == nil {
		return
	}
	var video []*sfutest.Recorder
	for _, r := range c.viewer.Recorders() {
		if r.Kind() == videoKind && r.StreamID() == shareID {
			video = append(video, r)
		}
	}
	m.mu.Lock()
	after := m.warmEnd
	m.mu.Unlock()
	pairs := avPairs(video, audio, after)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pairs {
		k := avKey{sub: c.index, share: shareID, layer: p.layer, frame: p.frame}
		if m.avSeen[k] {
			continue
		}
		m.avSeen[k] = true
		m.av = append(m.av, p.offset.Abs())
	}
}

// avPair is one flash frame and its beep.
type avPair struct {
	layer  string
	frame  uint32
	offset time.Duration // flash − beep
}

// avPairs pairs every flash frame of a share's video tracks that arrived after `after` with the beep of the same
// capture instant on its audio track, both mapped to NTP time through the last sender report their track got before
// them (the first one, early on), as sfutest.Viewer.AVOffset does; and returns flash − beep for each pair. Unlike
// AVOffset it takes the audio track to use (a subscription's audio can come back on a new track) and leaves out the
// warm-up.
func avPairs(video []*sfutest.Recorder, audio *sfutest.Recorder, after time.Time) []avPair {
	aSRs := audio.SRs()
	beeps := map[int64]int64{} // capture ns → NTP ns
	for _, p := range audio.Packets() {
		if !p.HasMark || !p.Mark.Beep || p.Dup {
			continue
		}
		if ntp, ok := mapNTP(aSRs, p.TS, p.Arrival, audioClock); ok {
			beeps[p.Mark.CaptureNS] = ntp
		}
	}
	var out []avPair
	for _, r := range video {
		srs := r.SRs()
		for _, f := range sfutest.Frames(r.Packets()) {
			if !f.HasMark || !f.Mark.Flash || f.First.Before(after) {
				continue
			}
			beep, ok := beeps[f.Mark.CaptureNS]
			if !ok {
				continue
			}
			if flash, ok := mapNTP(srs, f.TS, f.First, videoClock); ok {
				out = append(out, avPair{layer: f.Mark.RID, frame: f.Mark.Frame, offset: time.Duration(flash - beep)})
			}
		}
	}
	return out
}

// mapNTP maps an RTP timestamp that arrived at `at` to NTP nanoseconds through the last sender report before it, or
// the first one when none came before.
func mapNTP(srs []sfutest.SenderReport, ts uint32, at time.Time, clock int64) (int64, bool) {
	if len(srs) == 0 {
		return 0, false
	}
	sr := srs[0]
	for _, s := range srs {
		if s.Arrival.After(at) {
			break
		}
		sr = s
	}
	//nolint:gosec // G115: RTP timestamps wrap; the difference is read as signed
	return ntpNanos(sr.NTP) + int64(int32(ts-sr.RTP))*int64(time.Second)/clock, true
}

// ntpNanos converts a 32.32 NTP time to nanoseconds since 1900.
func ntpNanos(ntp uint64) int64 {
	//nolint:gosec // G115: NTP seconds fit an int64 of nanoseconds until 2192
	return int64(ntp>>32)*int64(time.Second) + int64((ntp&0xffffffff)*uint64(time.Second)>>32)
}

// ---- results ----

// windowDash returns the dashboard samples taken in [from, to], without repeats of one snapshot (it is cached for
// 1 s).
func (m *meter) windowDash(from, to time.Time) []dashSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []dashSample
	for _, s := range m.dash {
		if s.at.Before(from) || s.at.After(to) {
			continue
		}
		if n := len(out); n > 0 && !s.generated.After(out[n-1].generated) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// windowPub returns the Publisher samples taken in [from, to].
func (m *meter) windowPub(from, to time.Time) []pubSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pubSample
	for _, s := range m.pub {
		if !s.at.Before(from) && !s.at.After(to) {
			out = append(out, s)
		}
	}
	return out
}

// cpuStats returns the server's CPU in percent of all its cores: the average over the samples, and the 95th
// percentile and maximum of the intervals between them. ok is false with fewer than two samples.
func cpuStats(samples []dashSample) (avg, p95, peak float64, ok bool) {
	var intervals []float64
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		if pct, ok := cpuPct(a, b); ok {
			intervals = append(intervals, pct)
		}
	}
	if len(intervals) == 0 {
		return 0, 0, 0, false
	}
	avg, _ = cpuPct(samples[0], samples[len(samples)-1])
	return avg, percentile(intervals, 95), slices.Max(intervals), true
}

// cpuPct is the CPU between two samples in percent of all cores.
func cpuPct(a, b dashSample) (float64, bool) {
	wall := b.generated.Sub(a.generated).Seconds()
	if wall <= 0 || b.numCPU <= 0 {
		return 0, false
	}
	return (b.cpuSeconds - a.cpuSeconds) / (wall * float64(b.numCPU)) * 100, true
}

// rates returns each sent stream's payload rate in bit/s between the first and the last sample.
func rates(samples []pubSample) map[trackKey]float64 {
	out := map[trackKey]float64{}
	if len(samples) < 2 {
		return out
	}
	a, b := samples[0], samples[len(samples)-1]
	secs := b.at.Sub(a.at).Seconds()
	if secs <= 0 {
		return out
	}
	for k, end := range b.tracks {
		out[k] = float64(end.octets-a.tracks[k].octets) * 8 / secs
	}
	return out
}

// expectedEgress is what a sample's subscriptions take, at the given rates: each share's full layer, preview and
// audio, as often as they are watched.
func (lt *loadTest) expectedEgress(s dashSample, r map[trackKey]float64) float64 {
	f, q := lt.cfg.layers[0].RID, lt.cfg.layers[1].RID
	var bps float64
	for id, n := range s.counts {
		bps += float64(n.high)*r[trackKey{id, f}] + float64(n.low)*r[trackKey{id, q}] + float64(n.audio)*r[trackKey{id, ""}]
	}
	return bps
}

// egress returns the mean media.egressBps of the samples and the mean expected egress, in bit/s.
func (lt *loadTest) egress(samples []dashSample, r map[trackKey]float64) (got, want float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	for _, s := range samples {
		got += float64(s.egressBps)
		want += lt.expectedEgress(s, r)
	}
	n := float64(len(samples))
	return got / n, want / n
}

// pliLayer is the keyframe requests of one sent layer.
type pliLayer struct {
	key       trackKey
	count     int
	perSecond float64
	maxInWin  int
}

// pliStats returns the PLIs and FIRs each video layer received in the samples' span: their number, their rate, and
// the most in any window of length win.
func pliStats(samples []pubSample, win time.Duration) []pliLayer {
	if len(samples) < 2 {
		return nil
	}
	first, last := samples[0], samples[len(samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	var out []pliLayer
	for k, end := range last.tracks {
		if k.layer == "" {
			continue // audio has no keyframes
		}
		l := pliLayer{key: k, count: end.plis - first.tracks[k].plis}
		if secs > 0 {
			l.perSecond = float64(l.count) / secs
		}
		j := 0
		for i := range samples {
			for j+1 <= i && !samples[j+1].at.After(samples[i].at.Add(-win)) {
				j++
			}
			l.maxInWin = max(l.maxInWin, samples[i].tracks[k].plis-samples[j].tracks[k].plis)
		}
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b pliLayer) int {
		return cmp.Or(cmp.Compare(a.key.share, b.key.share), cmp.Compare(a.key.layer, b.key.layer))
	})
	return out
}

// lossTotals sums the Recorders' counters since the marks: the packets expected, the gaps found before NACK/RTX, the
// packets still missing after 1 s, the duplicates, and the payload bytes received.
type lossTotals struct {
	expected, gaps, lost, dups, tracks, bytes int
}

func lossSince(now, marks map[*sfutest.Recorder]sfutest.RecorderStats) lossTotals {
	var t lossTotals
	for r, s := range now {
		m := marks[r] // zero for a track that came after the mark
		t.expected += s.Expected - m.Expected
		t.gaps += s.Gaps - m.Gaps
		t.lost += s.Lost - m.Lost
		t.dups += s.Duplicates - m.Duplicates
		t.bytes += s.Bytes - m.Bytes
		if s.Packets > m.Packets {
			t.tracks++
		}
	}
	return t
}

// pct is a share of the expected packets in percent.
func (t lossTotals) pct(n int) float64 {
	if t.expected <= 0 {
		return 0
	}
	return float64(n) / float64(t.expected) * 100
}

// step measures one all-high step: what was raised, egress, CPU and loss over [from, to].
func (m *meter) step(from, to time.Time, marks map[*sfutest.Recorder]sfutest.RecorderStats) stepReport {
	lt := m.lt
	dash := m.windowDash(from, to)
	got, want := lt.egress(dash, rates(m.windowPub(from, to)))
	loss := lossSince(m.recorderStats(), marks)
	st := stepReport{
		Seconds: round(to.Sub(from).Seconds(), 1), EgressMbps: round(got/1e6, 2), ExpectedEgressMbps: round(want/1e6, 2),
		LossRaw: round(loss.pct(loss.gaps), 3), LossFinal: round(loss.pct(loss.lost), 3),
	}
	for _, n := range lt.wantCounts() {
		st.High += n.high
	}
	if avg, _, peak, ok := cpuStats(dash); ok {
		st.CPUAvg, st.CPUMax = round(avg, 1), round(peak, 1)
	}
	return st
}

// report fills the report's measurements and checks from what the run collected.
func (m *meter) report(rep *report) {
	lt := m.lt
	cfg := lt.cfg
	lim := cfg.scenario.limits
	m.mu.Lock()
	warmEnd, baseline, dashErrs := m.warmEnd, m.baselineRSS, m.dashErrs
	marks := m.warmMarks
	switches := slices.Clone(m.switches)
	av := slices.Clone(m.av)
	continuity := slices.Clone(m.continuity)
	m.mu.Unlock()
	end := time.Now()
	rep.MeasuredSeconds = round(end.Sub(warmEnd).Seconds(), 1)
	var checks checkList

	// Server.
	dash := m.windowDash(warmEnd, end)
	rep.Server.Samples = len(dash)
	rep.Server.BaselineRSS = baseline
	if dashErrs > len(dash) {
		rep.Failures = append(rep.Failures, fmt.Sprintf("the dashboard failed %d times, with %d good samples", dashErrs, len(dash)))
	}
	avg, p95, peak, cpuOK := cpuStats(dash)
	rep.Server.CPU = cpuReport{Avg: round(avg, 1), P95: round(p95, 1), Max: round(peak, 1)}
	checks.add("cpu avg", cpuOK, fmt.Sprintf("%.1f%%", avg), lim.cpuAvgPct > 0, avg < lim.cpuAvgPct, fmt.Sprintf("< %.0f%%", lim.cpuAvgPct))
	checks.add("cpu sample", cpuOK, fmt.Sprintf("%.1f%%", peak), lim.cpuSamplePct > 0, peak < lim.cpuSamplePct,
		fmt.Sprintf("< %.0f%% in any %v", lim.cpuSamplePct, cfg.dashboardEvery))
	perDT, dts := 0.0, 0
	for _, s := range dash {
		rep.Server.PeakRSS = max(rep.Server.PeakRSS, s.rss)
		dts = max(dts, s.downTracks)
		if s.downTracks > 0 {
			perDT = max(perDT, float64(s.rss-baseline)/float64(s.downTracks))
		}
	}
	rep.Server.DownTracks, rep.Server.RSSPerDownTrack = dts, math.Round(perDT)
	// A server that can't read its own RSS reports 0 (the dashboard's process collector has none on some platforms,
	// such as a macOS build without cgo): then there is nothing to measure.
	checks.add("memory", dts > 0 && rep.Server.PeakRSS > 0, fmt.Sprintf("%.2f MB per DownTrack", perDT/1e6), lim.rssPerDownTrack > 0,
		perDT < lim.rssPerDownTrack, fmt.Sprintf("< %.0f MB", lim.rssPerDownTrack/1e6))
	got, want := lt.egress(dash, rates(m.windowPub(warmEnd, end)))
	rep.Server.EgressMbps, rep.Server.ExpectedEgressMbps = round(got/1e6, 2), round(want/1e6, 2)
	ratio := 0.0
	if want > 0 {
		ratio = got / want
	}
	checks.add("egress", want > 0 && len(lt.subs) > 0, fmt.Sprintf("%.2f Mbps, %.0f%% of %.2f Mbps", got/1e6, ratio*100, want/1e6),
		lim.egressHigh > 0, ratio >= lim.egressLow && ratio <= lim.egressHigh,
		fmt.Sprintf("%.0f%%–%.0f%% of the expected", lim.egressLow*100, lim.egressHigh*100))

	// PLIs.
	plis := pliStats(m.windowPub(warmEnd, end), pliWindow)
	worstRate, worstWin := 0.0, 0
	for _, l := range plis {
		rep.PLI.Total += l.count
		worstRate, worstWin = max(worstRate, l.perSecond), max(worstWin, l.maxInWin)
		rep.PLI.Layers = append(rep.PLI.Layers, pliLayerReport{
			Share: l.key.share, Layer: l.key.layer, Count: l.count, PerSecond: round(l.perSecond, 3), MaxIn10s: l.maxInWin,
		})
	}
	rep.PLI.MaxPerSecond, rep.PLI.MaxIn10s = round(worstRate, 3), worstWin
	checks.add("pli rate", len(plis) > 0, fmt.Sprintf("%.3f/s on the worst layer", worstRate), lim.pliPerSecond > 0,
		worstRate <= lim.pliPerSecond, fmt.Sprintf("≤ %g/s per layer", lim.pliPerSecond))
	checks.add("pli storms", len(plis) > 0, fmt.Sprintf("%d in 10 s on the worst layer", worstWin), lim.pliPer10s > 0,
		worstWin <= lim.pliPer10s, fmt.Sprintf("≤ %d in any 10 s", lim.pliPer10s))

	// The subscribers' checks: none without subscribers (02 §15.2: a browser is the viewer then).
	if len(lt.subs) > 0 {
		loss := lossSince(m.recorderStats(), marks)
		if secs := end.Sub(warmEnd).Seconds(); secs > 0 {
			rep.Server.ReceivedMbps = round(float64(loss.bytes)*8/secs/1e6, 2)
		}
		rep.Loss = lossReport{
			Raw: round(loss.pct(loss.gaps), 3), Final: round(loss.pct(loss.lost), 3), ExpectedPackets: loss.expected,
			Tracks: loss.tracks, Duplicates: loss.dups,
		}
		checks.add("loss", loss.expected > 0, fmt.Sprintf("%.3f%% of %d packets (%.3f%% before NACK/RTX)",
			rep.Loss.Final, loss.expected, rep.Loss.Raw), lim.lossFinalPct > 0, rep.Loss.Final < lim.lossFinalPct,
			fmt.Sprintf("< %g%%", lim.lossFinalPct))
		if loss.expected == 0 {
			rep.Failures = append(rep.Failures, "the subscribers received nothing after the warm-up")
		}

		lat := make([]float64, 0, len(switches))
		for _, s := range switches {
			lat = append(lat, ms(s.latency))
			switch {
			case s.lostStart:
				rep.SwitchMs.LostStart++
			case !s.keyframe:
				rep.SwitchMs.WithoutKeyframe++
			}
			if s.verified {
				rep.SwitchMs.Verified++
				if s.seqGap {
					rep.SwitchMs.SeqGaps++
				}
			}
			if s.tsBack {
				rep.SwitchMs.TSBack++
			}
		}
		rep.SwitchMs.Count = len(lat)
		if len(lat) > 0 {
			rep.SwitchMs.P50, rep.SwitchMs.P95 = round(percentile(lat, 50), 1), round(percentile(lat, 95), 1)
			rep.SwitchMs.Max = round(slices.Max(lat), 1)
		}
		checks.add("switch latency", len(lat) > 0, fmt.Sprintf("p95 %.0f ms of %d switches", rep.SwitchMs.P95, len(lat)),
			lim.switchP95 > 0, rep.SwitchMs.P95 < ms(lim.switchP95), fmt.Sprintf("p95 < %v", lim.switchP95))
		checks.add("switch starts", len(lat) > 0, fmt.Sprintf("%d without a keyframe and its SPS", rep.SwitchMs.WithoutKeyframe),
			lim.switchP95 > 0, rep.SwitchMs.WithoutKeyframe == 0, "every switch on an SPS")
		checks.add("continuity", rep.SwitchMs.Verified > 0 || rep.SwitchMs.TSBack > 0,
			fmt.Sprintf("%d seq gaps and %d timestamps back at %d switches checked after repairs", rep.SwitchMs.SeqGaps,
				rep.SwitchMs.TSBack, rep.SwitchMs.Verified),
			lim.switchP95 > 0, rep.SwitchMs.SeqGaps == 0 && rep.SwitchMs.TSBack == 0, "seq and ts continuous at every switch")
		for _, c := range continuity {
			rep.Failures = append(rep.Failures, "continuity: "+c)
		}

		avms := make([]float64, len(av))
		for i, d := range av {
			avms[i] = ms(d)
		}
		rep.AVOffsetMs.Count = len(avms)
		if len(avms) > 0 {
			rep.AVOffsetMs.P50, rep.AVOffsetMs.P99 = round(percentile(avms, 50), 2), round(percentile(avms, 99), 2)
			rep.AVOffsetMs.Max = round(slices.Max(avms), 2)
		}
		checks.add("a/v", len(avms) > 0, fmt.Sprintf("p99 %.2f ms of %d pairs", rep.AVOffsetMs.P99, len(avms)), lim.avP99 > 0,
			rep.AVOffsetMs.P99 <= ms(lim.avP99), fmt.Sprintf("p99 ≤ %v", lim.avP99))
		if lim.avP99 > 0 && len(avms) == 0 {
			rep.Failures = append(rep.Failures, "a/v: no flash and beep pair with sender reports at any viewer")
		}
	}
	rep.Checks = checks.list
	for _, c := range checks.list {
		if c.Checked && !c.Pass {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: %s, limit %s", c.Name, c.Value, c.Limit))
		}
	}
}

// checkList collects the checks of a report.
type checkList struct{ list []checkReport }

// add adds a check. have says whether there was anything to measure; without it the check is skipped and reported
// as such. checked says whether the scenario has a limit for it; pass is the verdict against that limit.
func (l *checkList) add(name string, have bool, value string, checked, pass bool, limit string) {
	c := checkReport{Name: name, Value: value, Checked: checked && have, Pass: pass || !checked || !have}
	if checked {
		c.Limit = limit
	}
	if !have {
		c.Value = "nothing to measure"
	}
	l.list = append(l.list, c)
}

// percentile returns the p-th percentile of xs by the nearest-rank method.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[min(max(rank, 1), len(s))-1]
}

// ms returns a duration in milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// round rounds x to n decimals.
func round(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
