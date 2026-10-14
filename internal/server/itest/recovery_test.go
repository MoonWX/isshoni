package itest

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases 4 to 8 of 01 §19 "Integration" (README S74, 01 slice P9): recovery end to end. A socket that dies, a
// network that goes away for a while, a server that restarts, a viewer without a decoder, and a pub PeerConnection
// that dies, each with the clients doing what the web client does about it (client_test.go) and the server doing
// the rest.

// ---- helpers ----

// longestGap returns the longest time between two packets that arrived in [from, to], counting from the last
// packet before from, and when it began.
func longestGap(pkts []sfutest.Packet, from, to time.Time) (gap time.Duration, at time.Time) {
	var prev time.Time
	for _, p := range pkts {
		if p.Arrival.After(to) {
			break
		}
		if !prev.IsZero() && !p.Arrival.Before(from) && p.Arrival.Sub(prev) > gap {
			gap, at = p.Arrival.Sub(prev), prev
		}
		prev = p.Arrival
	}
	if !prev.IsZero() && to.Sub(prev) > gap { // nothing since the last packet
		gap, at = to.Sub(prev), prev
	}
	return gap, at
}

// waitResumed waits until a video Recorder's stream has started again after its first skip packets: a packet that
// starts a keyframe, and n whole frames of layer rid from there on. It returns the packets from that keyframe on,
// in sequence-number order, and how many arrived after the mark before it: what was still on its way when the
// stream broke off.
//
// "From there on" is the stream's order, not the order of arrival: a packet that arrives after the keyframe's start
// with a sequence number before it is no part of the stream that resumed. Those are packets that the outage
// swallowed and NACKs brought back; a decoder that starts on the keyframe has no use for them.
func waitResumed(t *testing.T, r *sfutest.Recorder, skip int, rid string, n int) (resumed []sfutest.Packet, before int) {
	t.Helper()
	since := func(pkts []sfutest.Packet) ([]sfutest.Packet, int) {
		before := slices.IndexFunc(pkts, func(p sfutest.Packet) bool { return p.KeyStart })
		if before < 0 {
			return nil, len(pkts)
		}
		first := pkts[before].Seq
		var out []sfutest.Packet
		for _, p := range sfutest.InSeqOrder(pkts[before:]) {
			if p.Seq-first < 1<<15 { // at or after first, with wraparound
				out = append(out, p)
			}
		}
		return out, before
	}
	err := r.Wait(waitCtx(t), func(r *sfutest.Recorder) bool {
		pkts := r.Packets()
		if len(pkts) < skip {
			return false
		}
		resumed, _ := since(pkts[skip:])
		return len(resumed) > 0 && layerFrames(resumed, rid) >= n
	})
	if err != nil {
		t.Fatalf("the %s of %s didn't start again with a keyframe and %d frames of layer %s: %v (%d packets since)",
			r.Kind(), r.StreamID(), n, rid, err, len(r.Packets())-skip)
	}
	return since(r.Packets()[skip:])
}

// newestSeq returns the highest sequence number a Recorder has received so far.
func newestSeq(r *sfutest.Recorder) uint16 {
	pkts := sfutest.InSeqOrder(r.Packets())
	return pkts[len(pkts)-1].Seq
}

// seqRange returns a Recorder's packets with sequence numbers from from to upTo, in sequence-number order. A test
// that takes upTo, waits sfutest.FinalLossAfter and then takes the range sees the stream as a jitter buffer would
// have it: a packet that the machine's load made late or lost on the way has been asked for again by then, and is
// in its place unless it is lost for good.
func seqRange(r *sfutest.Recorder, from, upTo uint16) []sfutest.Packet {
	var out []sfutest.Packet
	for _, p := range sfutest.InSeqOrder(r.Packets()) {
		if p.Seq-from < 1<<15 && upTo-p.Seq < 1<<15 { // from ≤ seq ≤ upTo, with wraparound
			out = append(out, p)
		}
	}
	return out
}

// tracksOf counts a Viewer's tracks of a kind in a share's stream: one per sub PC that carried it.
func tracksOf(v *sfutest.Viewer, shareID string, kind webrtc.RTPCodecType) int {
	n := 0
	for _, r := range v.Recorders() {
		if r.Kind() == kind && r.StreamID() == shareID {
			n++
		}
	}
	return n
}

// shareRuns returns the statuses a share had in a client's room.state snapshots from the from-th on, each run of
// one status once: "live stalled live". A snapshot without the share adds "gone".
func shareRuns(c *client, shareID string, from int) string {
	var runs []string
	for _, st := range c.roomStates()[from:] {
		status := "gone"
		for _, s := range st.Shares {
			if s.ID == shareID {
				status = string(s.Status)
			}
		}
		if len(runs) == 0 || runs[len(runs)-1] != status {
			runs = append(runs, status)
		}
	}
	return strings.Join(runs, " ")
}

// participantRuns is shareRuns for a participant: "present reconnecting present".
func participantRuns(c *client, userID string, from int) string {
	var runs []string
	for _, st := range c.roomStates()[from:] {
		status := "gone"
		for _, p := range st.Participants {
			if p.UserID == userID {
				status = string(p.Status)
			}
		}
		if len(runs) == 0 || runs[len(runs)-1] != status {
			runs = append(runs, status)
		}
	}
	return strings.Join(runs, " ")
}

// offerList renders offers as "gen/neg" in order: "1/1 1/2 2/1".
func offerList(offers []protocol.PCOffer) string {
	var out []string
	for _, o := range offers {
		out = append(out, fmt.Sprintf("%d/%d", o.Gen, o.Neg))
	}
	return strings.Join(out, " ")
}

// freePort returns a port that is free on 127.0.0.1 for network ("udp" or "tcp") now.
func freePort(t *testing.T, network string) int {
	t.Helper()
	var lc net.ListenConfig
	if network == "udp" {
		pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- the cases ----

// TestSocketKilled is case 4: the WebSockets of a sharer and of its viewer die mid-share, their TCP connections
// closed under them (01 §11.5 A). The media doesn't notice: RTP keeps flowing through the outage, with no gap of
// more than 200 ms. Both clients resume on a new socket each, with the connection ids they had, and the room sees
// them reconnecting and then present again, without anybody leaving or joining. Nothing is negotiated again: the
// sub offer and the pub offer of before are all there are.
func TestSocketKilled(t *testing.T) {
	const maxGap = 200 * time.Millisecond
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(41, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	// Carol is in the room and watches nothing: she sees what the room sees.
	carol := w.connect(w.member("carol"), protocol.RoleViewer)
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))
	carol.waitLive(sh.id)
	ids := map[*client]string{alice: alice.connID(), bob: bob.connID()}
	dialed := map[*client]int{alice: alice.dialed(), bob: bob.dialed()}
	roomMark := len(carol.roomStates()) - 1 // the newest snapshot: the room as it was
	eventMarks := map[*client]int{alice: len(alice.roomEvents()), bob: len(bob.roomEvents()), carol: len(carol.roomEvents())}

	// ---- both sockets die ----
	cut := time.Now()
	alice.killSocket()
	bob.killSocket()
	for _, c := range []*client{alice, bob} {
		if resumed := c.waitWelcomes(1); !resumed[0] {
			t.Fatalf("%s's welcome after the cut did not resume the connection (states %+v)", c.name, c.stateLog())
		}
	}
	back := time.Since(cut)
	// The media of the next second too: whatever the resume does must not disturb it either.
	waitPackets(t, audio, 50)
	until := time.Now()

	// ---- the media flowed through it ----
	for name, r := range map[string]*sfutest.Recorder{"video": video, "audio": audio} {
		gap, at := longestGap(r.Packets(), cut, until)
		t.Logf("%s: the longest gap between two packets from the cut to %v after it is %v, %v after the cut", name,
			until.Sub(cut).Round(time.Millisecond), gap.Round(time.Millisecond), at.Sub(cut).Round(time.Millisecond))
		if gap > maxGap {
			t.Errorf("%s: no packet for %v, %v after the cut; want no gap over %v", name, gap.Round(time.Millisecond),
				at.Sub(cut).Round(time.Millisecond), maxGap)
		}
		if st := r.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
			t.Errorf("%s: stats %+v, want no gap: nothing happened to the media", name, st)
		}
	}
	vp := checkVideo(t, "video", video.Packets())
	if runs := layerRuns(vp); runs != "f" {
		t.Errorf("the viewer got the layers %q, want the full layer alone", runs)
	}
	if err := sfutest.CheckContinuous(audio.Packets()); err != nil {
		t.Errorf("audio: %v", err)
	}

	// ---- the signaling resumed ----
	t.Logf("both clients resumed %v after the cut", back.Round(time.Millisecond))
	for _, c := range []*client{alice, bob} {
		if id := c.connID(); id != ids[c] {
			t.Errorf("%s's connection is %s after the resume, want %s as before", c.name, id, ids[c])
		}
		if n := c.dialed() - dialed[c]; n != 1 {
			t.Errorf("%s dialed %d sockets to resume, want one", c.name, n)
		}
	}
	// The room saw both reconnecting, then present again: nobody left, nobody joined.
	for _, c := range []*client{alice, bob} {
		eventually(t, func() bool { return participantRuns(carol, c.userID, roomMark) == "present reconnecting present" },
			func() string {
				return fmt.Sprintf("%s in carol's room.state went %q since the cut, want present, reconnecting, present again",
					c.name, participantRuns(carol, c.userID, roomMark))
			})
	}
	if runs := shareRuns(carol, sh.id, roomMark); runs != "live" {
		t.Errorf("the share went %q in carol's room.state since the cut, want live all along", runs)
	}
	for c, mark := range eventMarks {
		if evs := c.roomEvents()[mark:]; len(evs) != 0 {
			t.Errorf("%s got the room events %+v since the cut, want none: a resume is no leave and no join", c.name, evs)
		}
	}

	// ---- nothing was negotiated again ----
	if offers := bob.subOffers(); len(offers) != 1 {
		t.Errorf("the viewer got the sub offers %s, want its first alone", offerList(offers))
	}
	if offers := alice.sentOffers(); offerList(offers) != "1/1" {
		t.Errorf("the sharer sent the pub offers %s, want its first alone", offerList(offers))
	}
	for _, c := range []*client{alice, bob} {
		if r, a := c.sentRestarts(), c.askedRestarts(); len(r) != 0 || len(a) != 0 {
			t.Errorf("%s sent pc.restart %+v and was asked for %+v, want neither", c.name, r, a)
		}
	}
	if did := sh.actions(); len(did) != 0 {
		t.Errorf("the sharer's pub PC: %v, want nothing done to it", did)
	}
	// The subscription is as it was: the viewer was told again what it gets (the resume's Resync), and it is what
	// it asked for.
	bob.waitStatus(want(sh.id, true))
	if n := tracksOf(viewer, sh.id, webrtc.RTPCodecTypeVideo); n != 1 {
		t.Errorf("the viewer has %d video tracks of the share, want the one it had", n)
	}
	for _, c := range []*client{alice, bob, carol} {
		c.check()
	}
}

// TestUDPBlackHole is case 5 (01 §11.5 B): the sharer's UDP is gone for a while, its WebSocket is fine. The share
// goes stalled, the sharer restarts ICE (an offer of its own on the same PC: the same gen, the next neg, new ICE
// credentials), and when the network is back the media is too, within 3 s. The share keeps its id, and its viewer,
// whose network is fine, keeps its subscription, its sub PC and its tracks: it is offered nothing new and asks for
// nothing.
//
// The outage lasts 5 s, and on until the hub has said that the share is stalled. Within 5 s that can't happen: the
// SFU sees a pub PC not connected only from Pion's 5 s without a packet (02 §7.3) or from the client's ICE restart,
// which comes 3 s after the client's own ICE has noticed the silence (01 §10.4, after 2 s here, faultySettings),
// and it says the share is stalled 2 s after that, with its once-a-second ticker (02 §5.3). So the outage lasts
// about 7 to 8 s.
//
// The viewer's network stays: a viewer whose UDP is gone too asks for its sub ICE restart while it is still gone
// (pc.restart{sub, ice} 3 s after its ICE noticed), and the SFU's ICE agent, which controls a sub PC, gives up on
// each candidate pair after 7 unanswered checks, about 1.5 s, and never checks it again. That restart does not
// connect when the network is back, and the viewer recovers with the 15 s rebuild of 01 §10.4 instead. The SFU's own
// TestSubICERestart (02 §17 integration 7) times the request with the network's return for the same reason.
func TestUDPBlackHole(t *testing.T) {
	const (
		outage = 5 * time.Second
		within = 3 * time.Second
	)
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	pubNet, wire := faultySettings(t)
	sh := alice.publish(51, pubNet)
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))
	roomMark := len(bob.roomStates()) - 1
	statusMark := len(bob.statuses(sh.id))

	// ---- the sharer's UDP is gone ----
	blackHole(wire, true)
	gone := time.Now()
	eventually(t, func() bool {
		s, _ := bob.shareIn(sh.id)
		return time.Since(gone) >= outage && s.Status == protocol.ShareStatusStalled
	}, func() string {
		s, _ := bob.shareIn(sh.id)
		return fmt.Sprintf("%v into the outage the share is %s, want stalled", time.Since(gone), s.Status)
	})
	videoMark, audioMark := len(video.Packets()), len(audio.Packets())
	blackHole(wire, false)
	back := time.Now()
	t.Logf("the sharer's UDP was gone for %v", back.Sub(gone).Round(time.Millisecond))

	// ---- the media is back within 3 s ----
	resumed, stragglers := waitResumed(t, video, videoMark, "f", 15)
	took := resumed[0].Arrival.Sub(back)
	t.Logf("the video came back %v after the network (%d packets of the old stream came first)", took.Round(time.Millisecond), stragglers)
	if took > within {
		t.Errorf("the video came back %v after the network, want within %v", took.Round(time.Millisecond), within)
	}
	if err := audio.Wait(waitCtx(t), func(r *sfutest.Recorder) bool {
		return slices.ContainsFunc(r.Packets()[audioMark:], func(p sfutest.Packet) bool { return p.Arrival.After(back) })
	}); err != nil {
		t.Fatalf("the audio didn't come back: %v", err)
	}
	waitPackets(t, audio, 25)
	pkts := audio.Packets()[audioMark:]
	again := pkts[slices.IndexFunc(pkts, func(p sfutest.Packet) bool { return p.Arrival.After(back) })]
	if d := again.Arrival.Sub(back); d > within {
		t.Errorf("the audio came back %v after the network, want within %v", d.Round(time.Millisecond), within)
	}
	// One stream each from where it came back on, once what was lost on the way has been repaired. Across the
	// outage the viewer's sequence numbers skip what the sharer sent into the void: the same pub PC and the same
	// layers, so the SFU's mapping goes on as it was (02 §9.4), and the sharer's own numbers went on meanwhile.
	videoTo, audioTo := newestSeq(video), newestSeq(audio)
	time.Sleep(sfutest.FinalLossAfter + 200*time.Millisecond)
	vp := seqRange(video, resumed[0].Seq, videoTo)
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckStartsOnSPS} {
		if err := check(sfutest.WithoutPartialTail(vp)); err != nil {
			t.Errorf("the video since the network came back: %v", err)
		}
	}
	if err := sfutest.CheckContinuous(seqRange(audio, again.Seq, audioTo)); err != nil {
		t.Errorf("the audio since the network came back: %v", err)
	}
	bob.waitLive(sh.id)
	if runs := shareRuns(bob, sh.id, roomMark); runs != "live stalled live" {
		t.Errorf("the share went %q in the viewer's room.state since the outage, want live, stalled, live again", runs)
	}

	// ---- one ICE restart, and nothing more ----
	if did := sh.actions(); !slices.Equal(did, []string{"ice restart"}) {
		t.Errorf("the sharer's pub PC: %v, want one ICE restart", did)
	}
	offers := alice.sentOffers()
	if offerList(offers) != "1/1 1/2" {
		t.Fatalf("the sharer sent the pub offers %s, want its first and the ICE restart's, 1/1 1/2", offerList(offers))
	}
	if a, b := iceUfrag(offers[0].SDP.Reveal()), iceUfrag(offers[1].SDP.Reveal()); a == b || !slices.Equal(offers[0].Tracks, offers[1].Tracks) {
		t.Errorf("the second pub offer has the ICE user name %q (the first %q) and the tracks %+v (the first %+v), "+
			"want new ICE credentials for the same share", b, a, offers[1].Tracks, offers[0].Tracks)
	}
	if pair := selectedPair(t, "the sharer", sh.publisher().PC()); pair.Local.Protocol != webrtc.ICEProtocolUDP {
		t.Errorf("the sharer's PC is on a %s pair after the ICE restart, want UDP again", pair.Local.Protocol)
	}
	for _, c := range []*client{alice, bob} {
		if r, a := c.sentRestarts(), c.askedRestarts(); len(r) != 0 || len(a) != 0 {
			t.Errorf("%s sent pc.restart %+v and was asked for %+v: no PC failed", c.name, r, a)
		}
	}

	// ---- the share and the subscription are the ones they were ----
	if ids := sh.allIDs(); len(ids) != 1 {
		t.Errorf("the share had the ids %v, want one", ids)
	}
	bob.waitStatus(want(sh.id, true))
	for _, st := range bob.statuses(sh.id)[statusMark:] {
		if st.RequestedVideo != protocol.VideoLayerHigh {
			t.Errorf("subscribe.status %+v during the outage: the subscription changed", st)
		}
	}
	if offers := bob.subOffers(); len(offers) != 1 {
		t.Errorf("the viewer got the sub offers %s, want its first alone", offerList(offers))
	}
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if n := tracksOf(viewer, sh.id, kind); n != 1 {
			t.Errorf("the viewer has %d %s tracks of the share, want the one it had", n, kind)
		}
	}
	if d := bob.desired(); !slices.Equal(d, []protocol.SubscriptionWant{want(sh.id, true)}) {
		t.Errorf("the viewer wants %+v, want what it wanted", d)
	}
	alice.check()
	bob.check()
}

// TestServerRestart is case 6 (01 §11.6): the server stops, and a new one starts on the same ports and data. The
// clients reconnect to it and are not resumed: the sharer joins the room again and publishes its share again with
// replaces, the viewer joins again and moves its focus, with the audio, to the share that replaces the one it
// watched, and the media flows within 5 s of the new server being up.
func TestServerRestart(t *testing.T) {
	const within = 5 * time.Second
	udp, tcp := freePort(t, "udp"), freePort(t, "tcp")
	w := newWorld(t, servertest.Options{Flags: []string{
		"--listen.ice-udp=127.0.0.1:" + strconv.Itoa(udp), "--listen.ice-tcp=127.0.0.1:" + strconv.Itoa(tcp),
	}})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(61, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	waitFrames(t, track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), 0, "f", 15)
	waitPackets(t, track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio), 25)
	before := map[*client]string{alice: alice.connID(), bob: bob.connID()}

	// ---- the restart ----
	stopping := time.Now()
	w.srv.Restart(t)
	up := time.Now()
	t.Logf("the new server was up %v after the old one began to stop", up.Sub(stopping).Round(time.Millisecond))
	if w.srv.UDPPort != udp || w.srv.TCPPort != tcp {
		t.Fatalf("the new server's ICE ports are %d/udp and %d/tcp, want %d and %d as before", w.srv.UDPPort, w.srv.TCPPort, udp, tcp)
	}
	for _, c := range []*client{alice, bob} {
		c.mu.Lock()
		bye := slices.Clone(c.goodbye)
		c.mu.Unlock()
		if len(bye) != 1 || bye[0].Reason != protocol.ShutdownReasonRestart {
			t.Fatalf("%s: server.shutdown = %+v, want one that announces a restart", c.name, bye)
		}
		t.Logf("%s was told to come back in %d ms", c.name, bye[0].ReconnectInMs)
		// A new connection: the new server knows nothing of the old one.
		if resumed := c.waitWelcomes(1); resumed[0] {
			t.Fatalf("%s's welcome from the new server resumed a connection (states %+v)", c.name, c.stateLog())
		}
		eventually(t, func() bool { id := c.connID(); return id != "" && id != before[c] },
			func() string { return c.name + " kept its connection id" })
	}

	// ---- the sharer published again, and the viewer moved its focus to it ----
	eventually(t, func() bool { return len(sh.allIDs()) == 2 }, func() string {
		return fmt.Sprintf("the sharer's share had the ids %v, want a second one", sh.allIDs())
	})
	next := sh.current()
	info := bob.waitLive(next)
	if info.Replaces != sh.id || info.UserID != alice.userID || info.ConnectionID != alice.connID() {
		t.Errorf("the new share is %+v, want the sharer's on its new connection, replacing %s", info, sh.id)
	}
	if st := bob.roomState(); len(st.Shares) != 1 {
		t.Errorf("the room has the shares %+v, want the new one alone", st.Shares)
	}
	video, audio := track(t, viewer, next, webrtc.RTPCodecTypeVideo), track(t, viewer, next, webrtc.RTPCodecTypeAudio)
	flowing := video.Stats().First.Sub(up)
	t.Logf("the viewer got the new share's video %v after the new server was up", flowing.Round(time.Millisecond))
	if flowing > within {
		t.Errorf("the viewer got the new share's video %v after the new server was up, want within %v", flowing.Round(time.Millisecond), within)
	}
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(next, true))
	vp := checkVideo(t, "the new share's video", video.Packets())
	if runs := layerRuns(vp); runs != "f" {
		t.Errorf("the viewer got the layers %q of the new share, want the full layer: the focus moved with it", runs)
	}
	if d := bob.desired(); !slices.Equal(d, []protocol.SubscriptionWant{want(next, true)}) {
		t.Errorf("the viewer wants %+v, want the new share in focus with its audio", d)
	}
	// Who was in the room when the new share went live was told that it started, and what it replaces: the web
	// client shows no toast for it. The sharer was: it joins before it publishes again. The viewer was only if it
	// joined first, which is the order in which the two reconnected.
	eventually(t, func() bool {
		return slices.ContainsFunc(alice.roomEvents(), func(ev protocol.RoomEvent) bool {
			return ev.Kind == protocol.RoomEventKindShareStarted && ev.ShareID == next && ev.Replaces == sh.id
		})
	}, func() string {
		return fmt.Sprintf("the sharer's room events %+v have no share.started with replaces", alice.roomEvents())
	})
	for _, ev := range bob.roomEvents() {
		if ev.Kind == protocol.RoomEventKindShareStarted && ev.ShareID == next && ev.Replaces != sh.id {
			t.Errorf("the viewer was told %+v, want replaces %s", ev, sh.id)
		}
	}
	// On the new connection: the share's pub PC of gen 1, and the viewer's sub PC of gen 1, offered once.
	if offers := alice.sentOffers(); offerList(offers) != "1/1 1/1" || offers[1].Tracks[0].ShareID != next {
		t.Errorf("the sharer sent the pub offers %s (the last for %+v), want one on each server", offerList(offers), offers[len(offers)-1].Tracks)
	}
	if offers := bob.subOffers(); offerList(offers) != "1/1 1/1" {
		t.Errorf("the viewer got the sub offers %s, want one from each server", offerList(offers))
	}
	if did := sh.actions(); !slices.Equal(did, []string{"republish"}) {
		t.Errorf("the sharer's pub PC: %v, want it published again once", did)
	}
	for _, c := range []*client{alice, bob} {
		if r := c.sentRestarts(); len(r) != 0 {
			t.Errorf("%s sent pc.restart %+v", c.name, r)
		}
		c.check()
	}
}

// TestCodecBlockedViewer is case 7 (01 §11.7): a viewer without an H.264 decoder (a fresh Firefox, caps.decode =
// ["opus"]) gets the audio and subscribe.status{reason: codec}. Its decoder arrives, and the client says so with
// caps.update: the room's codec policy drops to Constrained Baseline, the sharer gets quality.hint{codec:
// h264/42e0} and switches, the server rebuilds the viewer's sub PC (gen 2), and the video decodes. The viewer
// never sends pc.restart: the client's part in codec recovery is caps.update alone.
func TestCodecBlockedViewer(t *testing.T) {
	cb := protocol.CodecH264ConstrainedBaseline
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publishWith(71, sfutest.LoopbackSettings(), shareOptions{profileSource: true})
	alice.waitLive(sh.id)
	fresh := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecOpus}}
	dan := w.connectWith(w.member("dan"), protocol.RoleViewer, connOptions{caps: &fresh})
	viewer := dan.watchWith(sfutest.LoopbackSettings(), viewerOptions{noH264: true})
	dan.waitLive(sh.id)
	dan.subscribe(want(sh.id, true))

	// ---- no decoder: the audio, and the reason ----
	heard := track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitPackets(t, heard, 50)
	blocked := protocol.SubscriptionStatus{
		ShareID: sh.id, Video: protocol.VideoLayerOff, Audio: protocol.AudioStateOn,
		RequestedVideo: protocol.VideoLayerHigh, Reason: protocol.StatusReasonCodec,
	}
	eventually(t, func() bool { st, _ := dan.lastStatus(sh.id); return st == blocked },
		func() string {
			return fmt.Sprintf("the viewer's statuses are %+v, want the last %+v", dan.statuses(sh.id), blocked)
		})
	offers := dan.subOffers()
	if len(offers) != 1 || len(offers[0].Tracks) != 1 || offers[0].Tracks[0].Kind != protocol.TrackKindAudio {
		t.Fatalf("the sub offers to a viewer without H.264 are %+v, want one with the audio alone", offers)
	}
	if hasTrack(viewer, sh.id, webrtc.RTPCodecTypeVideo) {
		t.Error("the viewer without H.264 got video")
	}
	// A viewer without H.264 doesn't count for the room's codec policy: the share stays High.
	if info, _ := dan.shareIn(sh.id); info.Codec != protocol.CodecH264High {
		t.Errorf("the share is in %s, want High while nobody needs less", info.Codec)
	}

	// ---- the decoder arrives ----
	// The browser's new PeerConnections decode Constrained Baseline and Baseline (OpenH264), and the client says so.
	if err := viewer.SetH264([]string{"42e01f", "42001f"}); err != nil {
		t.Fatal(err)
	}
	gained := protocol.Caps{Decode: []protocol.CodecKey{cb, protocol.CodecH264Baseline, protocol.CodecOpus}}
	if err := dan.send(protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: gained}); err != nil {
		t.Fatal(err)
	}
	// The sharer is told to switch to Constrained Baseline.
	hint := protocol.QualityHint{ShareID: sh.id, Reason: protocol.HintReasonCodec, Codec: cb}
	eventually(t, func() bool {
		return slices.ContainsFunc(alice.qualityHints(), func(h protocol.QualityHint) bool {
			return h.ShareID == hint.ShareID && h.Reason == hint.Reason && h.Codec == hint.Codec && len(h.Encodings) == 0
		})
	}, func() string { return fmt.Sprintf("the sharer's hints are %+v, want %+v", alice.qualityHints(), hint) })
	// The viewer's sub PC is rebuilt by the server, with the video.
	eventually(t, func() bool { return len(dan.subOffers()) >= 2 }, func() string {
		return fmt.Sprintf("the viewer got the sub offers %s, want a new sub PC", offerList(dan.subOffers()))
	})
	if rebuilt := dan.subOffers()[1]; rebuilt.Gen != 2 || rebuilt.Neg != 1 || len(rebuilt.Tracks) != 2 {
		t.Errorf("the second sub offer is gen %d, neg %d with %+v; want gen 2, neg 1 with the video and the audio",
			rebuilt.Gen, rebuilt.Neg, rebuilt.Tracks)
	}

	// ---- the video decodes ----
	seen := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo)
	waitFrames(t, seen, 0, "f", 30)
	if fmtp := seen.Codec().SDPFmtpLine; !strings.Contains(fmtp, "profile-level-id=42e01f") {
		t.Errorf("the viewer's video is %q, want Constrained Baseline", fmtp)
	}
	vp := checkVideo(t, "the video after the decoder came", seen.Packets())
	if runs := layerRuns(vp); runs != "f" {
		t.Errorf("the viewer got the layers %q, want the full layer", runs)
	}
	dan.waitStatus(want(sh.id, true))
	alice.waitLiveIn(sh.id, cb)
	if profile, switches := sh.src.(*sfutest.ProfileSource).Profile(); profile != "42e0" || switches != 1 {
		t.Errorf("the sharer's encoder is in %s after %d switches, want Constrained Baseline after one", profile, switches)
	}
	// The sharer offered again with Constrained Baseline first, on its PC of before.
	if did := sh.actions(); !slices.Equal(did, []string{"codec " + string(cb)}) {
		t.Errorf("the sharer's pub PC: %v, want one re-offer for the codec", did)
	}
	if offers := alice.sentOffers(); offerList(offers) != "1/1 1/2" {
		t.Errorf("the sharer sent the pub offers %s, want 1/1 1/2", offerList(offers))
	}
	// The audio went on, on the new sub PC.
	now, err := viewer.WaitRecorder(waitCtx(t), func(r *sfutest.Recorder) bool {
		return r.Kind() == webrtc.RTPCodecTypeAudio && r.StreamID() == sh.id && r != heard
	})
	if err != nil {
		t.Fatalf("no audio on the new sub PC: %v", err)
	}
	waitPackets(t, now, 25)
	if n := tracksOf(viewer, sh.id, webrtc.RTPCodecTypeAudio); n != 2 {
		t.Errorf("the viewer has %d audio tracks of the share, want one on each sub PC", n)
	}

	// ---- the client never asked for a restart ----
	for _, c := range []*client{alice, dan} {
		if r := c.sentRestarts(); len(r) != 0 {
			t.Errorf("%s sent pc.restart %+v: codec recovery is the server's", c.name, r)
		}
		c.check()
	}
}

// TestPubPCRebuild is case 8: the sharer's pub PeerConnection dies (closed under the client, with no pc.close), and
// the client rebuilds it: a new PeerConnection, offered as gen 2 with the same share id in its tracks. The share is
// stalled and then live again under its id, and its viewers get video again on the tracks they had, starting with a
// keyframe, without a new sub offer.
func TestPubPCRebuild(t *testing.T) {
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(81, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	// A viewer with the share in focus, and one with it as a thumbnail.
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	carol := w.connect(w.member("carol"), protocol.RoleViewer)
	focus, thumb := bob.watch(sfutest.LoopbackSettings(), 0), carol.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	carol.waitLive(sh.id)
	bob.subscribe(want(sh.id, true))
	carol.subscribe(want(sh.id, false))
	full, audio := track(t, focus, sh.id, webrtc.RTPCodecTypeVideo), track(t, focus, sh.id, webrtc.RTPCodecTypeAudio)
	small := track(t, thumb, sh.id, webrtc.RTPCodecTypeVideo)
	waitFrames(t, full, 0, "f", 30)
	waitFrames(t, small, 0, "q", 15)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))
	carol.waitStatus(want(sh.id, false))
	roomMark := len(bob.roomStates()) - 1
	eventMark := len(bob.roomEvents())
	fullMark, smallMark, audioMark := len(full.Packets()), len(small.Packets()), audio.Stats().Packets

	// ---- the pub PC dies ----
	if err := sh.publisher().PC().Close(); err != nil {
		t.Fatal(err)
	}
	died := time.Now()
	// The client rebuilds it: gen 2, with the same share.
	eventually(t, func() bool { return offerList(alice.sentOffers()) == "1/1 2/1" }, func() string {
		return fmt.Sprintf("the sharer sent the pub offers %s, want 1/1 2/1", offerList(alice.sentOffers()))
	})
	if offers := alice.sentOffers(); len(offers[1].Tracks) != 2 || offers[1].Tracks[0].ShareID != sh.id || offers[1].Tracks[1].ShareID != sh.id {
		t.Errorf("the rebuilt pub PC's offer binds %+v, want the video and the audio of %s", offers[1].Tracks, sh.id)
	}
	bob.waitLive(sh.id)
	eventually(t, func() bool { return shareRuns(bob, sh.id, roomMark) == "live stalled live" }, func() string {
		return fmt.Sprintf("the share went %q in room.state since its pub PC died, want live, stalled, live again",
			shareRuns(bob, sh.id, roomMark))
	})

	// ---- the viewers: the same tracks, on with a keyframe, no new offer ----
	for _, v := range []struct {
		c    *client
		r    *sfutest.Recorder
		mark int
		rid  string
		view *sfutest.Viewer
	}{{bob, full, fullMark, "f", focus}, {carol, small, smallMark, "q", thumb}} {
		resumed, old := waitResumed(t, v.r, v.mark, v.rid, 15)
		t.Logf("%s got the video again %v after the pub PC died (%d packets of the old PC came first)", v.c.name,
			resumed[0].Arrival.Sub(died).Round(time.Millisecond), old)
		// The new PC's layers attach in the order their first packets arrive: a viewer of f gets q for as long as
		// that is all the share has (02 §10.1), and f from its first frame on.
		frames := sfutest.Frames(sfutest.WithoutPartialTail(resumed))
		first := slices.IndexFunc(frames, func(f sfutest.Frame) bool { return f.HasMark && f.Mark.RID == v.rid })
		for i, f := range frames {
			if !f.HasMark || (f.Mark.RID != v.rid && (i > first || f.Mark.RID != "q")) {
				t.Errorf("%s got a frame of layer %q after the rebuild (frame %d, the first of %s is %d)", v.c.name,
					f.Mark.RID, i, v.rid, first)
				break
			}
		}
		if n := tracksOf(v.view, sh.id, webrtc.RTPCodecTypeVideo); n != 1 {
			t.Errorf("%s has %d video tracks of the share, want the one it had", v.c.name, n)
		}
		if offers := v.c.subOffers(); len(offers) != 1 {
			t.Errorf("%s got the sub offers %s, want its first alone: a pub PC rebuild needs no renegotiation", v.c.name, offerList(offers))
		}
	}
	if err := audio.Wait(waitCtx(t), func(r *sfutest.Recorder) bool { return r.Stats().Packets >= audioMark+50 }); err != nil {
		t.Errorf("the audio didn't come back: %v", err)
	}
	// One stream each through the rebuild, once what was lost on the way has had its repair: the viewers'
	// sequence numbers have no gap, and the timestamps go on.
	streams := map[string]*sfutest.Recorder{"bob's video": full, "carol's video": small, "bob's audio": audio}
	ends := map[*sfutest.Recorder]uint16{}
	for _, r := range streams {
		ends[r] = newestSeq(r)
	}
	time.Sleep(sfutest.FinalLossAfter + 200*time.Millisecond)
	for name, r := range streams {
		pkts := seqRange(r, sfutest.InSeqOrder(r.Packets())[0].Seq, ends[r])
		if r.Kind() == webrtc.RTPCodecTypeVideo {
			pkts = sfutest.WithoutPartialTail(pkts)
		}
		if err := sfutest.CheckContinuous(pkts); err != nil {
			t.Errorf("%s through the rebuild: %v", name, err)
		}
	}
	bob.waitStatus(want(sh.id, true))
	carol.waitStatus(want(sh.id, false))

	// ---- the share is the one it was ----
	if ids := sh.allIDs(); len(ids) != 1 {
		t.Errorf("the share had the ids %v, want one", ids)
	}
	if evs := bob.roomEvents()[eventMark:]; len(evs) != 0 {
		t.Errorf("room events since the pub PC died: %+v, want none: the share never ended", evs)
	}
	if did := sh.actions(); !slices.Equal(did, []string{"rebuild"}) {
		t.Errorf("the sharer's pub PC: %v, want one rebuild", did)
	}
	for _, c := range []*client{alice, bob, carol} {
		c.check()
	}
}
