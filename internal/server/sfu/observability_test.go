package sfu_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of the observability slice (README S79, 02 §13) with real PeerConnections and media, through
// sfutest.Harness: every metric after integration 2, Snapshot and ConnStats of the same session, and a capture of
// the SFU's log, Pion's lines included, in which nothing at info or above holds SDP or an address. The numbers on a
// hand-fed media path, and Snapshot's golden file, are stats_test.go's.

// TestMetricsAfterLayerSwitches runs integration 2 of 02 §17 (high → low → off → high, TestLayerSwitchOnKeyframe) and
// then reads every metric of 02 §13: each label value is there, the gauges count what the session has, and the
// counters that its media and its switches drive aren't zero. Snapshot and both Conns' Stats say the same.
func TestMetricsAfterLayerSwitches(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_metrics")
	pubConn, _ := publishShare(t, h, "alice", "c-pub", share, 79)
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	state := func(requested, forwarded sfu.Quality, audio bool) sfu.SubscriptionStateEvent {
		return sfu.SubscriptionStateEvent{Share: share, Requested: requested, Forwarded: forwarded, Audio: audio}
	}

	// ---- integration 2 ----
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 15)
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 25 }); err != nil {
		t.Fatal(err)
	}
	waitSubState(t, viewSig, state(sfu.QualityHigh, sfu.QualityHigh, true))
	mark := len(video.Packets())
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, video, mark, "q", 8)
	waitSubState(t, viewSig, state(sfu.QualityLow, sfu.QualityLow, true))
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityOff})
	waitSubState(t, viewSig, state(sfu.QualityOff, sfu.QualityOff, false))
	mark = len(video.Packets())
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	waitFrames(t, video, mark, "f", 15)
	waitSubState(t, viewSig, state(sfu.QualityHigh, sfu.QualityHigh, true))
	if runs := layerRuns(sfutest.WithoutPartialTail(video.Packets())); runs != "f q f" {
		t.Fatalf("the viewer got the layers %q, want f, then q, then f again", runs)
	}

	// ---- every metric ----
	m := h.SFU.Metrics()
	t.Logf("Metrics after integration 2: %+v", m)
	exact := func(name string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) { // maps print sorted by key
			t.Errorf("Metrics.%s = %v, want %v", name, got, want)
		}
	}
	nonZero := func(name string, got map[string]uint64, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if got[k] == 0 {
				t.Errorf("Metrics.%s[%s] = 0, want the session's", name, k)
			}
		}
	}
	keys := func(name string, got any, want ...string) {
		t.Helper()
		var have []string
		switch g := got.(type) {
		case map[string]uint64:
			for k := range g {
				have = append(have, k)
			}
		case map[string]int64:
			for k := range g {
				have = append(have, k)
			}
		}
		slices.Sort(have)
		slices.Sort(want)
		if !slices.Equal(have, want) {
			t.Errorf("Metrics.%s has the labels %v, want %v", name, have, want)
		}
	}
	states := "map[closed:0 connected:1 connecting:0 disconnected:0 failed:0 new:0]"
	// The gauges: the session as it is.
	exact("Conns", m.Conns, "map[agent:0 full:1 publisher:0 viewer:1]")
	exact("PeerConnections", m.PeerConnections, "map[pub:"+states+" sub:"+states+"]")
	exact("SelectedTransport", m.SelectedTransport, "map[tcp443:0 tcp7882:0 udp:2]")
	exact("Shares", m.Shares, "map[live:1 pending:0 stalled:0]")
	exact("DownTracks", m.DownTracks, "map[audio:map[high:1 low:0 off:0] video:map[high:1 low:0 off:0]]")
	if m.PacketCacheBytes <= 0 {
		t.Errorf("Metrics.PacketCacheBytes = %d, want the cached packets' bytes", m.PacketCacheBytes)
	}
	// The counters its media and its switches drive.
	nonZero("IngressBytes", m.IngressBytes, "video", "audio")
	nonZero("IngressPackets", m.IngressPackets, "video", "audio")
	nonZero("EgressBytes", m.EgressBytes, "video", "audio")
	nonZero("EgressPackets", m.EgressPackets, "video", "audio")
	keys("IngressBytes", m.IngressBytes, "video", "audio")
	keys("EgressPackets", m.EgressPackets, "video", "audio", "rtx")
	keys("QueueDrops", m.QueueDrops, "video", "audio", "rtx")
	if m.IngressPackets["video"] < m.EgressPackets["video"]/2 || m.IngressBytes["audio"] < m.EgressBytes["audio"] {
		t.Errorf("Metrics: ingress %v / %v, egress %v / %v; one viewer of one share gets at most what came in",
			m.IngressPackets, m.IngressBytes, m.EgressPackets, m.EgressBytes)
	}
	// A keyframe for the viewer's start and one for each switch, asked for with a PLI each.
	if m.PLISent < 2 || m.KeyframesReceived < 4 {
		t.Errorf("Metrics: %d PLIs sent, %d keyframes received; want the switches' requests and their keyframes",
			m.PLISent, m.KeyframesReceived)
	}
	exact("LayerSwitches", m.LayerSwitches, 2) // f → q, and q → off → f; the start is no switch
	// Integration 2 gives no reason (it checks that), and nothing timed out.
	exact("Downgrades", m.Downgrades, "map[bandwidth:0 codec_mismatch:0 decoder_failed:0 decoder_unavailable:0 "+
		"no_layer:0 no_preview_layer:0 server_limit:0]")
	exact("HandshakeTimeouts", m.HandshakeTimeouts, "map[pub:0 sub:0]")
	// The repair counters belong to integration 4 (TestNACKFromCache), the duplicates to 19 (TestRTXDuplicates): on
	// loopback they are what the network did, so here they are only read.
	t.Logf("repair on loopback: duplicates %d, NACKs %d received, %d served, %d missed, %d skipped; PLIs throttled %d",
		m.IngressDuplicates, m.NACKReceived, m.NACKServed, m.NACKMissed, m.RTXSkipped, m.PLIThrottled)

	// ---- Snapshot: the same session for the dashboard ----
	var snap sfu.Snapshot
	eventually(t, func() bool {
		snap = h.SFU.Snapshot()
		if len(snap.Rooms) != 1 || len(snap.Rooms[0].Shares) != 1 || len(snap.Rooms[0].Conns) != 2 {
			return false
		}
		ss, view := snap.Rooms[0].Shares[0], snap.Rooms[0].Conns[1]
		return ss.IngressBps > 0 && ss.EgressBps > 0 && view.RTT > 0
	}, func() string { return fmt.Sprintf("Snapshot = %+v", snap) })
	room := snap.Rooms[0]
	ss := room.Shares[0]
	if room.Room != "lounge" || ss.Info.ID != share || ss.Info.State != sfu.ShareLive || len(ss.Info.Layers) != 2 ||
		!ss.Info.Audio || ss.Viewers != (sfu.ViewerCounts{High: 1, Audio: 1}) {
		t.Errorf("Snapshot's share = %+v, want the live share with f, q and audio and one high viewer with audio", ss)
	}
	for _, l := range ss.Info.Layers {
		if l.Bitrate <= 0 || l.FPS <= 0 || l.Width == 0 || !l.Active {
			t.Errorf("Snapshot's layer %+v, want its ingress measured", l)
		}
	}
	if ss.EgressBps > ss.IngressBps*2 {
		t.Errorf("Snapshot: the share's egress %d bit/s for one viewer, its ingress %d", ss.EgressBps, ss.IngressBps)
	}
	pubSum, viewSum := room.Conns[0], room.Conns[1]
	if pubSum.Conn != "c-pub" || pubSum.User != "alice" || pubSum.Transport != "udp" ||
		viewSum.Conn != "c-view" || viewSum.User != "bob" || viewSum.Transport != "udp" || viewSum.RTT > time.Second {
		t.Errorf("Snapshot's Conns = %+v, want both on udp, and the viewer's RTT from its sub PC", room.Conns)
	}
	tot := snap.Totals
	if tot.DownTracks != 2 || tot.IngressBps != ss.IngressBps || tot.EgressBps != ss.EgressBps ||
		fmt.Sprint(tot.PCsByState) != "map[closed:0 connected:2 connecting:0 disconnected:0 failed:0 new:0]" ||
		fmt.Sprint(tot.PCsByTransport) != "map[tcp443:0 tcp7882:0 udp:2]" {
		t.Errorf("Snapshot.Totals = %+v", tot)
	}
	t.Logf("Snapshot: ingress %d bit/s, egress %d bit/s, RTT publisher %v, viewer %v", ss.IngressBps, ss.EgressBps,
		pubSum.RTT, viewSum.RTT)

	// ---- ConnStats of both Conns ----
	var vs sfu.ConnStats
	eventually(t, func() bool {
		vs = viewConn.Stats()
		return len(vs.Subscriptions) == 1 && vs.Subscriptions[0].Video.Bitrate > 0 && vs.Subscriptions[0].Audio.Bitrate > 0
	}, func() string { return fmt.Sprintf("the viewer's Stats = %+v", vs) })
	sub := vs.Subscriptions[0]
	if len(vs.PCs) != 1 || vs.PCs[0].Kind != sfu.PCSub || vs.PCs[0].Gen != 1 || vs.PCs[0].State != "connected" ||
		vs.PCs[0].Transport != "udp" || vs.PCs[0].RTT <= 0 {
		t.Errorf("the viewer's PCs = %+v, want its sub PC, connected on udp, with an RTT", vs.PCs)
	}
	if sub.Share != share || sub.Requested != sfu.QualityHigh || sub.Forwarded != sfu.QualityHigh || sub.Reason != "" ||
		sub.Layer != "f" || sub.Profile == "" || !sub.AudioRequested || !sub.AudioForwarded {
		t.Errorf("the viewer's subscription = %+v, want high on f with audio", sub)
	}
	// The Viewer sends no REMB here, and nothing was capped: the downlink allocator has no estimate.
	if len(vs.Shares) != 0 || vs.DownlinkEstimate != 0 {
		t.Errorf("the viewer's Stats = %+v, want no own share and no downlink estimate without a REMB", vs)
	}
	var ps sfu.ConnStats
	eventually(t, func() bool {
		ps = pubConn.Stats()
		return len(ps.Shares) == 1 && ps.Shares[0].AudioBitrate > 0
	}, func() string { return fmt.Sprintf("the publisher's Stats = %+v", ps) })
	if len(ps.PCs) != 1 || ps.PCs[0].Kind != sfu.PCPub || ps.PCs[0].State != "connected" || ps.PCs[0].Transport != "udp" ||
		len(ps.Subscriptions) != 0 {
		t.Errorf("the publisher's Stats = %+v, want its pub PC on udp and no subscription", ps)
	}
	if own := ps.Shares[0]; own.Share != share || own.State != sfu.ShareLive || len(own.Layers) != 2 || !own.Audio {
		t.Errorf("the publisher's share = %+v", own)
	}
}

// logRecord is one line of the JSON log the capture test reads.
type logRecord map[string]any

func (r logRecord) str(key string) string {
	s, _ := r[key].(string)
	return s
}

// strings returns every string value of the record but its time, keys included (sorted).
func (r logRecord) strings() []string {
	var out []string
	for k, v := range r {
		if k == "time" {
			continue
		}
		out = append(out, k)
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// sdpRE matches what SDP and ICE candidates are made of: the lines of a session description, a candidate attribute
// or its type, and ICE credentials.
var sdpRE = regexp.MustCompile(`(?:^|[\s"\\])(?:v=0|o=-|s=-|t=0 0|c=IN|m=(?:audio|video|application)|a=[a-z-]+)|` +
	`candidate:|typ (?:host|srflx|prflx|relay)|ice-ufrag|ice-pwd`)

// quadRE matches a dotted quad, the form of an IPv4 address.
var quadRE = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)

// addressIn returns an IP address that s holds, "" when it holds none. It is the test's own reading, apart from the
// SFU's (sfu.holdsAddress): dotted quads that parse as IPv4, and words with two colons or more that parse as IPv6,
// with or without brackets, a port, a zone and the punctuation around them.
func addressIn(s string) string {
	for _, q := range quadRE.FindAllString(s, -1) {
		if _, err := netip.ParseAddr(q); err == nil {
			return q
		}
	}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(" \t\r\n,;()'\"<>=", r) }) {
		if strings.Count(w, ":") < 2 {
			continue
		}
		w = strings.TrimRight(w, ".:")
		if _, err := netip.ParseAddrPort(w); err == nil {
			return w
		}
		if i := strings.IndexByte(w, '%'); i > 0 {
			w = w[:i]
		}
		if _, err := netip.ParseAddr(strings.Trim(w, "[]")); err == nil {
			return w
		}
	}
	return ""
}

// TestLogsHoldNoSDPOrAddresses is the log capture test of 02 §13 (README S79): a session with every kind of SFU line
// at info, on the ICE-TCP transports too and with a connection-test probe, read back as JSON. Every line at info and
// above, Pion's included, holds no SDP, no candidate and no IP address in its message or any attribute; Pion's info
// lines are debug lines (pionlog.go). The SFU's own info lines say what 02 §13 lists, with the transport of each
// connected PC.
//
// The session runs twice. Logged at debug, Pion produces all of its lines, and none of its info lines is left at
// info. Logged at info, which is how a server runs, Pion's warnings are all there: at debug, logx's limit of 20 lines
// a minute per Pion scope (04 §10) is spent on Pion's chatter, and it suppresses the warnings that come later.
//
// The lines of other components are the Transport's (netx, 04 §7.3), its Pion ICE scope's among them. They hold no
// address at info and above either: netx sends that scope's info lines ("Listening TCP on <ip>:<port>") to debug, as
// pionlog.go does for the SFU's.
func TestLogsHoldNoSDPOrAddresses(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
		t.Run(level.String(), func(t *testing.T) {
			checkLogs(t, level, logSession(t, level))
		})
	}
}

// logSession runs the session of TestLogsHoldNoSDPOrAddresses with a JSON log at level, and returns the log once
// every Conn and the probe have closed.
func logSession(t *testing.T, level slog.Level) string {
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: level}))
	h := newHarness(t, sfutest.HarnessOptions{TCP: true, TCP443: true, Logger: logger})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_logs")

	// The viewer subscribes while the share is pending: no_layer, a downgrade.
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Reason: sfu.SubReasonNoLayer})

	// Then it is published, watched, switched to low.
	pub := newPublisher(t, newSource(t), sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 10)
	mark := len(video.Packets())
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, video, mark, "q", 4)

	// A connection test on ICE-TCP via 443, by a third user.
	probe, _, res := runProbe(t, h, "carol", sfu.ProbeTCP443)
	select {
	case r := <-res:
		if !r.Connected {
			t.Fatalf("the probe did not connect: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no probe result")
	}
	if err := probe.CloseChannel(); err != nil {
		t.Fatal(err)
	}

	// Everybody leaves, and the PCs close: Pion's warnings about closing streams come now.
	viewConn.Close(sfu.EndReasonLeft)
	pubConn.Close(sfu.EndReasonLeft)
	for _, c := range []*sfu.Conn{viewConn, pubConn} {
		select {
		case <-c.Done():
		case <-ctx.Done():
			t.Fatal("a Conn did not finish closing")
		}
	}
	eventually(t, func() bool { slots, running := h.SFU.Probes(); return slots == 0 && running == 0 },
		func() string { return "the probe is still running" })
	time.Sleep(200 * time.Millisecond) // Pion's last goroutines log as they end
	return logs.String()
}

// checkLogs checks the log of logSession at level.
func checkLogs(t *testing.T, level slog.Level, text string) {
	var (
		sfuInfo, pionDebug int
		pionAbove, msgs    []string
	)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec logRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("a log line that isn't JSON: %v: %s", err, sc.Text())
		}
		lv, pion := rec.str("level"), rec["scope"] != nil
		above := lv == slog.LevelInfo.String() || lv == slog.LevelWarn.String() || lv == slog.LevelError.String()
		if !above && lv != slog.LevelDebug.String() {
			t.Errorf("a log line at %q: %s", lv, sc.Text())
		}
		other := rec.str("component") != "sfu"
		switch {
		case other:
		case pion && lv == slog.LevelDebug.String():
			pionDebug++
		case pion && lv == slog.LevelInfo.String():
			t.Errorf("a Pion line at info: %s", sc.Text())
		case pion && above:
			pionAbove = append(pionAbove, fmt.Sprintf("%s %s %q", rec.str("scope"), lv, rec.str("msg")))
		case lv == slog.LevelInfo.String():
			sfuInfo++
			msgs = append(msgs, rec.str("msg"))
		}
		if !above {
			continue
		}
		for _, s := range rec.strings() {
			if sdpRE.MatchString(s) {
				t.Errorf("a log line at %s holds SDP or a candidate (%q): %s", lv, sdpRE.FindString(s), sc.Text())
			}
			if a := addressIn(s); a != "" {
				t.Errorf("a log line at %s holds the address %s: %s", lv, a, sc.Text())
			}
		}
	}
	t.Logf("component=sfu: %d info lines of the SFU's own, %d Pion lines at debug, %d at warn or error: %v", sfuInfo,
		pionDebug, len(pionAbove), pionAbove)
	if level == slog.LevelDebug && pionDebug == 0 {
		t.Error("no Pion line at debug: the capture saw nothing of Pion's")
	}

	// What 02 §13 has the SFU say at info, each with ids, never more.
	for _, want := range []string{
		`"msg":"connection joined"`, `"msg":"share started"`, `"msg":"share live"`, `"msg":"share ended"`,
		`"msg":"connection left"`, `"msg":"subscription downgraded"`, `"reason":"no_layer"`,
		`"pc":"pub","gen":1,"state":"connected","transport":"udp"`,
		`"pc":"sub","gen":1,"state":"connected","transport":"udp"`,
		`"msg":"connection test probe connected"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %s; the SFU's info lines are %q", want, msgs)
		}
	}
}
