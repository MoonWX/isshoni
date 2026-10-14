package sfu

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/logx"
)

// The observability slice's unit tests (README S79, 02 §13): Snapshot against a golden file, and Metrics and
// ConnStats with exact numbers, on an SFU whose media path a test feeds by hand; the totals that outlive the Layers
// and DownTracks that counted them; and what the SFU makes of Pion's log lines. The same reports with real
// PeerConnections and media are observability_test.go's.

// testSubscription subscribes viewer to sh as UpdateSubscriptions would, but without the Conn's actor and sub PC: a
// video and an audio DownTrack on the share's fan-out lists, bound as Pion would bind them (SSRC 0x7000 and 0x7001,
// Chrome's payload types) to a sub PC whose DTLS-ready gate is open, and the request applied and published for Stats.
// The test runs the writers (drain).
func testSubscription(t *testing.T, viewer *Conn, sh *Share, video Quality, audio bool) *Subscription {
	t.Helper()
	sub := &Subscription{conn: viewer, share: sh, reqVideo: video, reqAudio: audio}
	sub.video = newDownTrack(sub, webrtc.RTPCodecTypeVideo)
	sub.audio = newDownTrack(sub, webrtc.RTPCodecTypeAudio)
	pc := &subPC{gen: 1}
	pc.ready.Store(true)
	for _, d := range sub.tracks() {
		if !sh.addDownTrack(d) {
			t.Fatalf("share %s took no DownTrack", sh.id)
		}
		b := &binding{id: fakeTrackContext{}.ID(), pc: pc, writer: &captureWriter{}, ssrc: 0x7000, ptFor: chromePTs}
		if d.kind == webrtc.RTPCodecTypeAudio {
			b.ssrc, b.ptFor = 0x7001, map[ProfileKey]uint8{"": 111}
		}
		d.binding.Store(b)
	}
	sub.apply()
	sub.publish()
	return sub
}

// mediaRig is one live share with the three layers of a simulcast sharer with audio, which a test feeds a second of
// media at a time: f at 30 frames a second of three 1000-byte packets, q at 15 frames of one 500-byte packet, and 50
// audio packets of 100 bytes. Each layer's first packet starts a keyframe.
type mediaRig struct {
	t        *testing.T
	f, q, a  *Layer
	seq      map[*Layer]uint16
	ts       map[*Layer]uint32
	key      []byte
	writers  map[*DownTrack]*rtpWriteState
	fPackets int // packets fed so far, by layer
	qPackets int
	aPackets int
}

func newMediaRig(t *testing.T, sh *Share) *mediaRig {
	t.Helper()
	r := &mediaRig{t: t, seq: map[*Layer]uint16{}, ts: map[*Layer]uint32{}, key: keyPayload(t), writers: map[*DownTrack]*rtpWriteState{}}
	r.f, _ = testLayer(t, sh, SlotF, 0xf00)
	r.q, _ = testLayer(t, sh, SlotQ, 0xf01)
	r.a, _ = testLayer(t, sh, SlotAudio, 0xa00)
	return r
}

// payload returns n bytes of a delta slice, or the keyframe start for a layer's first packet.
func (r *mediaRig) payload(l *Layer, n int) []byte {
	if _, started := r.seq[l]; !started {
		return r.key
	}
	p := make([]byte, n)
	p[0] = 0x41
	return p
}

// send feeds one packet of l at now; a new frame moves the timestamp on by step.
func (r *mediaRig) send(l *Layer, n int, newFrame, marker bool, step uint32, now int64) {
	p := r.payload(l, n)
	if newFrame {
		r.ts[l] += step
	}
	seq := r.seq[l]
	profile := ProfileHigh
	if l.kind == webrtc.RTPCodecTypeAudio {
		profile = ""
	}
	l.handleRTP(rtpPkt(1000+seq, r.ts[l], marker, p), profile, now)
	r.seq[l] = seq + 1
}

// second feeds one second of media from now, and then runs the writers of the given DownTracks over what they got.
func (r *mediaRig) second(now int64, dts ...*DownTrack) {
	for i := range 90 {
		r.send(r.f, 1000, i%3 == 0, i%3 == 2, 3000, now+int64(i)*oneSecond/90)
		r.fPackets++
	}
	for i := range 15 {
		r.send(r.q, 500, true, true, 6000, now+int64(i)*oneSecond/15)
		r.qPackets++
	}
	for i := range 50 {
		r.send(r.a, 100, true, false, 960, now+int64(i)*oneSecond/50)
		r.aPackets++
	}
	for _, d := range dts {
		w := r.writers[d]
		if w == nil {
			w = &rtpWriteState{}
			r.writers[d] = w
		}
		drain(d, w)
	}
}

// TestSnapshotGolden: Snapshot of 02 §13 on two rooms: in the lounge a live share with f, q and audio whose two
// viewers get high with audio and low without; in the den a share that stays pending, and a viewer that asked for
// it (no_layer, with a sub PC that has no answer yet). The rates come from two seconds of media and three ticks of the
// clock the test runs, so the numbers are exact: testdata/golden/snapshot.json (go test -run Golden -update). Metrics
// and ConnStats are checked against the same numbers.
func TestSnapshotGolden(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	alice, _ := join(t, s, "lounge", "alice", "c-alice", RoleFull)
	bob, _ := join(t, s, "lounge", "bob", "c-bob", RoleViewer)
	carol, _ := join(t, s, "lounge", "carol", "c-carol", RoleViewer)
	erin, _ := join(t, s, "den", "erin", "c-erin", RolePublisher)
	dave, daveSig := join(t, s, "den", "dave", "c-dave", RoleViewer)
	startShare(t, alice, "s_lounge")
	startShare(t, erin, "s_den")
	sh := s.lookupShare("s_lounge")
	rig := newMediaRig(t, sh)
	bobSub := testSubscription(t, bob, sh, QualityHigh, true)
	carolSub := testSubscription(t, carol, sh, QualityLow, false)
	errs, err := dave.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_den", Video: QualityHigh, Audio: true}})
	if err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	dts := []*DownTrack{bobSub.video, bobSub.audio, carolSub.video, carolSub.audio}
	s.everySecond(t0) // the baseline
	rig.second(t0, dts...)
	s.everySecond(t0 + oneSecond)
	rig.second(t0+oneSecond, dts...)
	s.everySecond(t0 + 2*oneSecond)
	// bob's player reports a quarter of the video lost.
	bobSub.video.handleRTCP([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 0x7000, FractionLost: 64}}}})

	// ---- Snapshot, against the golden file ----
	snap := s.Snapshot()
	fixed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	norm := func(tm *time.Time) {
		if !tm.IsZero() {
			*tm = fixed
		}
	}
	if snap.At.IsZero() {
		t.Error("Snapshot.At is zero")
	}
	norm(&snap.At)
	for i := range snap.Rooms {
		for j := range snap.Rooms[i].Shares {
			norm(&snap.Rooms[i].Shares[j].Info.StartedAt)
			norm(&snap.Rooms[i].Shares[j].Info.LiveAt)
		}
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "snapshot.json", string(b)+"\n")

	// ---- Metrics: the totals are what the Layers and DownTracks counted, and the gauges what there is now ----
	m := s.Metrics()
	fBytes := uint64(len(rig.key) + (rig.fPackets-1)*1000)
	qBytes := uint64(len(rig.key) + (rig.qPackets-1)*500)
	aBytes := uint64(len(rig.key) + (rig.aPackets-1)*100)
	check := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Metrics.%s = %v, want %v", name, got, want)
		}
	}
	states := func(set map[string]int64) map[string]int64 {
		out := map[string]int64{"new": 0, "connecting": 0, "connected": 0, "disconnected": 0, "failed": 0, "closed": 0}
		for k, v := range set {
			out[k] = v
		}
		return out
	}
	check("Conns", m.Conns, map[string]int64{"full": 1, "viewer": 3, "publisher": 1, "agent": 0})
	check("PeerConnections", m.PeerConnections, map[string]map[string]int64{
		"pub": states(nil), "sub": states(map[string]int64{"new": 1}), // dave's: offered, never answered
	})
	check("SelectedTransport", m.SelectedTransport, map[string]int64{"udp": 0, "tcp443": 0, "tcp7882": 0})
	check("Shares", m.Shares, map[string]int64{"pending": 1, "live": 1, "stalled": 0})
	check("DownTracks", m.DownTracks, map[string]map[string]int64{
		"video": {"high": 1, "low": 1, "off": 1}, // bob, carol, dave
		"audio": {"high": 1, "low": 0, "off": 2},
	})
	check("IngressPackets", m.IngressPackets, map[string]uint64{
		"video": uint64(rig.fPackets + rig.qPackets), "audio": uint64(rig.aPackets),
	})
	check("IngressBytes", m.IngressBytes, map[string]uint64{"video": fBytes + qBytes, "audio": aBytes})
	check("EgressPackets", m.EgressPackets, map[string]uint64{
		"video": uint64(rig.fPackets + rig.qPackets), "audio": uint64(rig.aPackets), "rtx": 0,
	})
	check("EgressBytes", m.EgressBytes, map[string]uint64{"video": fBytes + qBytes, "audio": aBytes, "rtx": 0})
	check("KeyframesReceived", m.KeyframesReceived, uint64(2))
	check("PLISent", m.PLISent, rig.f.stats.pliSent.Load()+rig.q.stats.pliSent.Load())
	check("PLIThrottled", m.PLIThrottled, rig.f.stats.pliThrottled.Load()+rig.q.stats.pliThrottled.Load())
	check("Downgrades", m.Downgrades, map[string]uint64{
		"bandwidth": 0, "server_limit": 0, "codec_mismatch": 0, "decoder_unavailable": 0, "decoder_failed": 0,
		"no_preview_layer": 0, "no_layer": 1, // dave's
	})
	check("PacketCacheBytes", m.PacketCacheBytes, int64(fBytes+qBytes+aBytes))
	if m.LayerSwitches != 0 || m.IngressDuplicates != 0 || m.NACKReceived != 0 || m.QueueDrops["video"] != 0 {
		t.Errorf("Metrics = %+v, want no switch, duplicate, NACK or drop", m)
	}

	// ---- ConnStats ----
	wantSub := SubscriptionStats{
		Share: "s_lounge", Requested: QualityHigh, Forwarded: QualityHigh, Layer: "f", Profile: ProfileHigh,
		AudioRequested: true, AudioForwarded: true,
		Video: DownTrackStats{Bitrate: int(bobSub.video.stats.bitrate.Load()), LossPct: 25},
		Audio: DownTrackStats{Bitrate: int(bobSub.audio.stats.bitrate.Load())},
	}
	if st := bob.Stats(); len(st.PCs) != 0 || len(st.Shares) != 0 || len(st.Subscriptions) != 1 ||
		!reflect.DeepEqual(st.Subscriptions[0], wantSub) || st.DownlinkEstimate != 0 {
		t.Errorf("bob's Stats = %+v, want only the subscription %+v", st, wantSub)
	}
	if wantSub.Video.Bitrate == 0 || wantSub.Audio.Bitrate == 0 {
		t.Errorf("bob's DownTracks have no bitrate: %+v", wantSub)
	}
	if st := carol.Stats(); len(st.Subscriptions) != 1 || st.Subscriptions[0].Layer != "q" ||
		st.Subscriptions[0].Forwarded != QualityLow || st.Subscriptions[0].AudioForwarded {
		t.Errorf("carol's Stats = %+v, want low on q without audio", st)
	}
	st := alice.Stats()
	info, _ := sh.info()
	audio := rig.a.info(monoNow())
	if len(st.Shares) != 1 || !reflect.DeepEqual(st.Shares[0].Layers, info.Layers) || st.Shares[0].State != ShareLive ||
		!st.Shares[0].Audio || st.Shares[0].AudioBitrate != audio.Bitrate || audio.Bitrate < 39_000 ||
		len(st.Subscriptions) != 0 {
		t.Errorf("alice's Stats = %+v, want her share with its layers and the audio layer's %d bit/s", st, audio.Bitrate)
	}
	st = dave.Stats()
	wantDave := SubscriptionStats{
		Share: "s_den", Requested: QualityHigh, Reason: SubReasonNoLayer, AudioRequested: true,
	}
	if len(st.PCs) != 1 || st.PCs[0] != (PCStats{Kind: PCSub, Gen: 1, State: "new"}) || len(st.Subscriptions) != 1 ||
		st.Subscriptions[0] != wantDave {
		t.Errorf("dave's Stats = %+v, want his new sub PC and %+v", st, wantDave)
	}

	// A state sent again (the resume of 02 §6.5) is no new downgrade.
	events := func() int {
		daveSig.mu.Lock()
		defer daveSig.mu.Unlock()
		return len(daveSig.events)
	}
	before := events()
	dave.Resync()
	eventuallyTrue(t, func() bool { return events() > before }, "the resync's SubscriptionStateEvent arrives")
	if n := s.Metrics().Downgrades["no_layer"]; n != 1 {
		t.Errorf("no_layer downgrades after a resync = %d, want still 1", n)
	}
}

// TestTotalsOutliveTheirObjects: the counters of Metrics never go back (02 §13): what a Layer and a DownTrack counted
// stays counted after the share has ended and they are gone, while the gauges drop to zero. It also checks the
// counters the golden test leaves at zero: queue drops, the repair path, and layer switches, which count a change of
// layer, not a resume in the same one.
func TestTotalsOutliveTheirObjects(t *testing.T) {
	s, _ := newTicklessSFU(t)
	alice, _ := join(t, s, "lounge", "alice", "c-alice", RoleFull)
	bob, _ := join(t, s, "lounge", "bob", "c-bob", RoleViewer)
	startShare(t, alice, "s_1")
	sh := s.lookupShare("s_1")
	rig := newMediaRig(t, sh)
	sub := testSubscription(t, bob, sh, QualityHigh, false)
	d := sub.video
	w := &rtpWriteState{}
	rig.writers[d] = w
	now := t0

	// f, then q, then f again: two switches. A pause and a resume on f: none.
	rig.second(now, d)
	for _, want := range []Quality{QualityLow, QualityHigh, QualityOff, QualityHigh} {
		d.setWant(want)
		now += oneSecond
		// A keyframe on both layers, which the switch waits for, and a second of what follows them.
		for _, l := range []*Layer{rig.f, rig.q} {
			rig.ts[l] += 6000
			l.handleRTP(rtpPkt(1000+rig.seq[l], rig.ts[l], true, rig.key), ProfileHigh, now)
			rig.seq[l]++
		}
		rig.second(now, d)
	}
	if got := s.Metrics().LayerSwitches; got != 2 {
		t.Errorf("LayerSwitches = %d after f → q → f → off → f, want 2", got)
	}
	if d.stats.switches.Load() != 4 {
		t.Errorf("the DownTrack started %d epochs, want 4 (f, q, f, f)", d.stats.switches.Load())
	}

	// A NACK for the last packet bob got: served from the cache, as RTX is not negotiated, as the packet again.
	last := w.packets // the writer's count of what left on the media SSRC
	if last == 0 {
		t.Fatal("bob got nothing")
	}
	d.mu.Lock()
	lastSeq := d.m.lastS
	d.mu.Unlock()
	d.handleRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: []rtcp.NackPair{{PacketID: lastSeq}}}})
	select {
	case it := <-d.rtxQueue:
		d.retransmit(w, it)
	default:
		t.Fatal("the NACK queued no retransmission")
	}
	// A writer that has stopped: its queue fills, and three more packets are dropped.
	for i := range videoQueueLen + 3 {
		d.enqueue(&packet{layer: rig.f, seq: uint16(i), payload: []byte{0x41}})
	}

	m := s.Metrics()
	if m.NACKReceived != 1 || m.NACKServed != 1 || m.NACKMissed != 0 || m.RTXSkipped != 0 || m.EgressPackets["rtx"] != 1 ||
		m.QueueDrops["video"] != 3 || m.QueueDrops["rtx"] != 0 {
		t.Errorf("Metrics after a NACK and a full queue = %+v", m)
	}
	if st := bob.Stats(); len(st.Subscriptions) != 1 || st.Subscriptions[0].Video.NACKsServed != 1 ||
		st.Subscriptions[0].Video.Drops != 3 {
		t.Errorf("bob's Stats = %+v, want 1 NACK served and 3 drops", st)
	}

	// The share ends: its Layers and DownTracks are gone, what they counted isn't.
	if err := s.StopShare("s_1", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	after := s.Metrics()
	for name, pair := range map[string][2]any{
		"IngressPackets":    {after.IngressPackets, m.IngressPackets},
		"IngressBytes":      {after.IngressBytes, m.IngressBytes},
		"EgressPackets":     {after.EgressPackets, m.EgressPackets},
		"EgressBytes":       {after.EgressBytes, m.EgressBytes},
		"QueueDrops":        {after.QueueDrops, m.QueueDrops},
		"NACKServed":        {after.NACKServed, m.NACKServed},
		"PLISent":           {after.PLISent, m.PLISent},
		"KeyframesReceived": {after.KeyframesReceived, m.KeyframesReceived},
		"LayerSwitches":     {after.LayerSwitches, m.LayerSwitches},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("Metrics.%s after the share ended = %v, want %v as before", name, pair[0], pair[1])
		}
	}
	if after.IngressPackets["video"] == 0 || after.PLISent == 0 || after.KeyframesReceived < 6 {
		t.Errorf("Metrics = %+v, want the packets, PLIs and keyframes of the test", after)
	}
	if after.Shares["live"] != 0 || after.DownTracks["video"]["high"] != 0 || after.DownTracks["video"]["off"] != 0 ||
		after.PacketCacheBytes != 0 {
		t.Errorf("gauges after the share ended: shares %v, DownTracks %v, cache %d; want all 0", after.Shares,
			after.DownTracks, after.PacketCacheBytes)
	}
	if snap := s.Snapshot(); snap.Totals.DownTracks != 0 || snap.Totals.IngressBps != 0 || len(snap.Rooms[0].Shares) != 0 {
		t.Errorf("Snapshot after the share ended = %+v", snap)
	}
}

// TestDownTrackTick: a DownTrack's bitrate is a 2 s moving average of what it sends, retransmissions included, like a
// Layer's: nothing on the first tick, the first second as it was, then the average.
func TestDownTrackTick(t *testing.T) {
	d := newDownTrack(&Subscription{share: &Share{id: "s_1"}}, webrtc.RTPCodecTypeVideo)
	d.tick(t0)
	d.stats.bytes.Add(100_000) // 800 kbit in the first second
	d.tick(t0 + oneSecond)
	if got := d.stats.bitrate.Load(); got != 800_000 {
		t.Errorf("bitrate after the first second = %d, want 800000", got)
	}
	d.stats.rtxBytes.Add(25_000) // then only 200 kbit of retransmissions
	d.tick(t0 + 2*oneSecond)
	want := int64(math.Round(800_000 + (1-math.Exp(-0.5))*(200_000-800_000)))
	if got := d.stats.bitrate.Load(); got != want {
		t.Errorf("bitrate after the second second = %d, want %d", got, want)
	}
	d.tick(t0 + 2*oneSecond) // no time passed: nothing changes
	if got := d.stats.bitrate.Load(); got != want {
		t.Errorf("bitrate after a tick without time = %d, want %d", got, want)
	}
}

// TestPionLogs: what the SFU makes of Pion's lines (pionlog.go, 02 §13): info lines are debug lines, and so are
// warnings and errors that hold an address; the others keep their level. Below debug, logx's bridge drops Pion's
// debug lines, so at info nothing of the first kinds is left.
func TestPionLogs(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
		var buf strings.Builder
		lf := pionLogs{logx.NewPionLoggerFactory(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))}
		l := lf.NewLogger("ice")
		l.Info("peer connection state changed: connected")
		l.Infof("ICE connection state changed: %s", "checking")
		l.Warnf("Discard request with wrong username from (%s), %v", "192.0.2.7:51000", errors.New("bad"))
		l.Warn("Failed to accept RTP stream is already closed")
		l.Errorf("Failed to handle packet from %s", "[2001:db8::1]:5000")
		l.Error("Failed to start the manager")
		l.Debugf("debug stays %s", "debug")

		var got []string
		sc := bufio.NewScanner(strings.NewReader(buf.String()))
		for sc.Scan() {
			var rec struct{ Level, Msg, Scope string }
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Scope != "ice" {
				t.Errorf("a Pion line without its scope: %s", sc.Text())
			}
			got = append(got, rec.Level+" "+rec.Msg)
		}
		want := []string{
			"DEBUG peer connection state changed: connected",
			"DEBUG ICE connection state changed: checking",
			"DEBUG Discard request with wrong username from (192.0.2.7:51000), bad",
			"WARN Failed to accept RTP stream is already closed",
			"DEBUG Failed to handle packet from [2001:db8::1]:5000",
			"ERROR Failed to start the manager",
			"DEBUG debug stays debug",
		}
		if level == slog.LevelInfo {
			want = []string{"WARN Failed to accept RTP stream is already closed", "ERROR Failed to start the manager"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("at %s the log is\n%s\nwant\n%s", level, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// TestHoldsAddress: the address test of the Pion lines finds IPv4 and IPv6 addresses, with or without a port, a
// zone or brackets, wherever they stand in a line, and nothing else that has digits, colons and dots.
func TestHoldsAddress(t *testing.T) {
	for _, s := range []string{
		"Discard request with wrong username from (192.0.2.7:51000), bad",
		"Failed to handle packet from [2001:db8::1]:5000",
		"Listening TCP on 127.0.0.1:43123",
		"remote:198.51.100.2",
		"to ::1.",
		"on fe80::1%eth0",
		"on fe80::1%lo0 now",
		"::ffff:10.0.0.1",
	} {
		if !holdsAddress(s) {
			t.Errorf("holdsAddress(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"Incoming unhandled RTCP ssrc(305419896), OnTrack will not be fired",
		"peer connection state changed: connected",
		"at 12:34:56.789",
		"pion/webrtc v4.2.23",
		"fingerprint AB:CD:EF:01:23:45:67",
		"deadbeef cafe",
		"took 1.5s, 3 of 4",
		"",
	} {
		if holdsAddress(s) {
			t.Errorf("holdsAddress(%q) = true, want false", s)
		}
	}
}

// TestDowngradeReasons: every reason of isshoni_sfu_downgrades_total has its counter, and a reason that isn't one
// (none) counts nothing.
func TestDowngradeReasons(t *testing.T) {
	var c counters
	for i, r := range downgradeReasons {
		for range i + 1 {
			c.downgrade(r)
		}
	}
	c.downgrade(SubReasonNone)
	for i, r := range downgradeReasons {
		if got := c.downgrades[i].Load(); got != uint64(i+1) {
			t.Errorf("downgrades of %s = %d, want %d", r, got, i+1)
		}
	}
	if fmt.Sprint(downgradeReasons) != "[bandwidth server_limit codec_mismatch decoder_unavailable decoder_failed "+
		"no_preview_layer no_layer]" {
		t.Errorf("downgradeReasons = %v, want the order of 02 §13", downgradeReasons)
	}
}
