package itest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases D3 to D6 of docs/m2/01-client-core.md §14: the client core's recovery (§11.6, §11.7) against the test's
// server. A socket that dies mid-share is resumed and the media never stops; a UDP outage of the pub PC is an ICE
// restart, and a rebuild when the restart can't connect; a server restart is a republish with replaces on a new
// connection; and a server that stays away ends the share after the 30 s hold. Next to D3, two resumes that have
// more to do: a pub PC that went disconnected while the socket was away restarts ICE on the welcome (M1 01 §10.4),
// and a share that the server ended meanwhile is published again on the same connection, at the pub PC's next gen
// (M1 01 §9 rule 2, §10.5). The viewers are M1's harness clients, which recover by themselves (client_test.go); D17
// (no leaks) is every case's.
//
// The core's own view comes from what it reports (Status, the share's snapshot, its events) and, for its pub offers,
// from its debug log: the "pub offer" lines name each offer's gen and neg, and whether it restarts ICE.

// recoveryApp is a desktop app for these cases: the core of desktopApp, whose WebSocket connections are recorded so
// that a test can cut the one in use and hold back the next, with the Pion settings the case gives its pub PC.
type recoveryApp struct {
	*desktopApp
	sock *sockets
	gate redialGate

	mu       sync.Mutex
	statuses []timedStatus // every EventStatus, with when it arrived
}

// timedStatus is an EventStatus and when the test received it.
type timedStatus struct {
	at time.Time
	st core.Status
}

// recoveringDesktop starts a desktop app for m, as desktop does, with settings for its pub PC (nil: loopback) and
// its sockets recorded and their redials gated (redialGate). It waits until the connection is online; the app is
// closed and checked when the test ends.
//
// Its authentication is desktop()'s (desktop_helpers_test.go), the member's session cookie in Options.Header. README
// WA73's switch of that file to a linked device's bearer token has to switch this function too, so that D3 runs on
// bearer auth as WA73's acceptance asks.
func (w *world) recoveringDesktop(m *member, engine fake.EngineConfig, settings *webrtc.SettingEngine) *recoveryApp {
	w.t.Helper()
	t := w.t
	site, err := url.Parse(w.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []string
	for _, ck := range m.http.Jar.Cookies(site) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	if len(cookies) == 0 {
		t.Fatalf("%s has no session cookie", m.name)
	}
	if settings == nil {
		se := sfutest.LoopbackSettings()
		settings = &se
	}
	logs := &lockedBuffer{}
	eng := fake.NewEngine(engine)
	a := &recoveryApp{sock: &sockets{}}
	tr, ok := a.sock.transport(t, w.srv.Client.Transport).(*http.Transport)
	if !ok {
		t.Fatal("the recorded transport isn't an *http.Transport")
	}
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := a.gate.wait(ctx); err != nil {
			return nil, err
		}
		return dial(ctx, network, addr)
	}
	t.Cleanup(a.gate.release) // registered before the core's close, so it runs after it: nothing waits by then
	c, err := core.New(core.Options{
		ServerURL:  w.srv.URL,
		Header:     http.Header{"Cookie": {strings.Join(cookies, "; ")}},
		Engine:     eng,
		Settings:   settings,
		HTTPClient: &http.Client{Transport: tr},
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.desktopApp = &desktopApp{t: t, name: m.name + "'s app", core: c, engine: eng, runErr: make(chan error, 1),
		logs: logs}
	d := a.desktopApp
	events, unsub := c.Subscribe()
	d.unsub = unsub
	d.rec.Go(func() {
		for ev := range events {
			at := time.Now()
			d.mu.Lock()
			d.events = append(d.events, ev)
			d.mu.Unlock()
			if ev.Kind == core.EventStatus && ev.Status != nil {
				a.mu.Lock()
				a.statuses = append(a.statuses, timedStatus{at: at, st: *ev.Status})
				a.mu.Unlock()
			}
		}
	})
	go func() { d.runErr <- c.Run(context.Background()) }()
	t.Cleanup(d.close)
	eventually(t, func() bool { return c.Status().Conn == core.ConnOnline },
		func() string { return fmt.Sprintf("%s: status %+v, want online", d.name, c.Status()) })
	return a
}

// statusMark returns how many EventStatus the app has had.
func (a *recoveryApp) statusMark() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.statuses)
}

// statusesSince returns the EventStatus from the mark on.
func (a *recoveryApp) statusesSince(mark int) []timedStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.statuses[mark:])
}

// wentAway returns when the first EventStatus since mark said the connection wasn't online, false before one did.
func (a *recoveryApp) wentAway(mark int) (time.Time, bool) {
	for _, s := range a.statusesSince(mark) {
		if s.st.Conn != core.ConnOnline {
			return s.at, true
		}
	}
	return time.Time{}, false
}

// cutSocket closes the TCP connection under the app's WebSocket, as killSocket does for a harness client.
func (a *recoveryApp) cutSocket() {
	a.t.Helper()
	a.sock.mu.Lock()
	conn := a.sock.last
	a.sock.mu.Unlock()
	if conn == nil {
		a.t.Fatalf("%s has no socket to cut", a.name)
	}
	if err := conn.Close(); err != nil {
		a.t.Fatalf("%s: closing its socket: %v", a.name, err)
	}
}

// dialed returns how many TCP connections the app's WebSocket has dialed.
func (a *recoveryApp) dialed() int {
	a.sock.mu.Lock()
	defer a.sock.mu.Unlock()
	return a.sock.n
}

// redialGate holds back the TCP dials of an app's WebSocket while a test says so: the core's signaling stays down,
// for as long as the test needs to set up what the resume is to find, and its next attempt goes out once released.
type redialGate struct {
	mu   sync.Mutex
	held chan struct{} // closed when released; nil while not held
}

// hold holds back the dials from now on.
func (g *redialGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held == nil {
		g.held = make(chan struct{})
	}
}

// release lets the dials go, the waiting one included.
func (g *redialGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held != nil {
		close(g.held)
		g.held = nil
	}
}

// wait waits while the gate holds, until ctx ends.
func (g *redialGate) wait(ctx context.Context) error {
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	if held == nil {
		return nil
	}
	select {
	case <-held:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pubOffer is one of the core's pub offers, as its log names it.
type pubOffer struct {
	gen, neg int
	restart  bool
}

func (o pubOffer) String() string {
	s := strconv.Itoa(o.gen) + "/" + strconv.Itoa(o.neg)
	if o.restart {
		s += "r"
	}
	return s
}

var pubOfferLine = regexp.MustCompile(`msg="pub offer" gen=(\d+) neg=(\d+) ice_restart=(true|false)`)

// offers returns the core's pub offers so far, in order.
func (a *recoveryApp) offers() []pubOffer {
	var out []pubOffer
	for _, m := range pubOfferLine.FindAllStringSubmatch(a.logs.String(), -1) {
		gen, _ := strconv.Atoi(m[1])
		neg, _ := strconv.Atoi(m[2])
		out = append(out, pubOffer{gen: gen, neg: neg, restart: m[3] == "true"})
	}
	return out
}

// pubOfferList renders the core's pub offers as "1/1 1/2r 2/1" ("r": an ICE restart).
func pubOfferList(offers []pubOffer) string {
	var out []string
	for _, o := range offers {
		out = append(out, o.String())
	}
	return strings.Join(out, " ")
}

// shareStates returns the states of the app's EventShare snapshots from the mark on, each run once: "live
// reconnecting live".
func (a *recoveryApp) shareStates(mark int) string {
	var runs []string
	for _, s := range a.shareEvents()[mark:] {
		if len(runs) == 0 || runs[len(runs)-1] != string(s.State) {
			runs = append(runs, string(s.State))
		}
	}
	return strings.Join(runs, " ")
}

// watched starts a share of the app in the default room and has bob watch it, the full layer in focus with its
// audio, until both arrive.
func watched(t *testing.T, app *recoveryApp, bob *client, viewer *sfutest.Viewer) (
	sh *core.Share, snap core.ShareSnapshot, video, audio *sfutest.Recorder) {
	t.Helper()
	sh = app.start(defaultRoom(bob))
	snap = app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, true))
	video, audio = track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo), track(t, viewer, snap.ID, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(snap.ID, true))
	return sh, snap, video, audio
}

// resumedSpan is how much of the viewer's video after the keyframe it came back with checkResumedVideo looks at
// after an ICE restart: enough to take in the layer switch that the SFU usually makes after the outage, about 0.75 s
// after the keyframe on an idle machine (later on a loaded one).
const resumedSpan = 2500 * time.Millisecond

// checkResumedVideo checks the viewer's video after a UDP outage of the core, from the first keyframe after the
// outage on (resumed, waitResumed's), once the Viewer has had the time to ask for what it missed
// (sfutest.FinalLossAfter, slept here). back is when the network came back. samePC says whether the pub PC that came
// back is the one the outage cut, its ICE restarted, rather than a rebuilt one.
//
// Either way, as D4 asks, the viewer resumes on a keyframe: one arrived whole within 5 s of the network
// (resumedOn). And every layer switch from the first keyframe on starts on an SPS.
//
// A rebuilt PC must also deliver the stream from the keyframe on without a gap or a second copy, as M1's
// TestUDPBlackHole asks of its share. It has nothing of the outage to repair: the closed PC's NACK responder is gone,
// the SFU's NACK generator sees no gap on the new tracks, and the SFU continues the viewer's sequence numbers right
// after the old track's last (a new epoch).
//
// After an ICE restart of the same PC, the stream after the keyframe needn't be continuous, unlike M1's
// TestUDPBlackHole, which checks its share's: D4 asks for no more than the keyframe there. The end of the outage
// brings a storm of repairs at the core's bitrate, about 4 Mbit/s here where M1's share sends 0.5: the SFU asks for
// every packet of the outage that its NACK window holds (2048), the core's NACK responder sends all of them that its
// 4096 packets hold at once, unpaced (over a thousand here), and the SFU forwards them to the viewer, which asks for
// what is still missing of the gap for a second; the SFU serves that from a budget of a quarter of the layer's rate
// (M1 02 §9.5). On a loaded machine (CI's runners, under -race) packets after the keyframe are then lost, or come
// back late, twice when the original was only queued, for as long as the load makes it last: in loaded runs here the
// last such loss came from under a second to over ten seconds after the keyframe, around when the SFU moved the
// viewer down a layer for it. Whether the core should send the outage again at all is an open question
// (internal/client/publish's responder), not something this test settles.
func checkResumedVideo(t *testing.T, video *sfutest.Recorder, resumed []sfutest.Packet, back time.Time, samePC bool) {
	t.Helper()
	first := resumed[0]
	if samePC {
		time.Sleep(time.Until(first.Arrival.Add(resumedSpan)))
	}
	videoTo := newestSeq(video)
	time.Sleep(sfutest.FinalLossAfter + 200*time.Millisecond)

	key, whole, skipped, err := resumedOn(seqRange(video, first.Seq, videoTo))
	switch {
	case whole.IsZero():
		t.Errorf("no keyframe arrived whole since the network came back: %v", err)
	case whole.Sub(back) > 5*time.Second:
		t.Errorf("the keyframe the video resumed on (seq %d) was whole %v after the network, want within 5 s",
			key.Seq, whole.Sub(back).Round(time.Millisecond))
	case skipped > 0:
		t.Logf("the video resumed on the keyframe at seq %d, whole %v after the network; %d keyframes before it "+
			"arrived in part (the first: %v)", key.Seq, whole.Sub(back).Round(time.Millisecond), skipped, err)
	}

	checks := []func([]sfutest.Packet) error{sfutest.CheckStartsOnSPS}
	if !samePC {
		checks = append(checks, sfutest.CheckContinuous)
	}
	since := sfutest.WithoutPartialTail(seqRange(video, first.Seq, videoTo))
	for _, check := range checks {
		if err := check(since); err != nil {
			t.Errorf("the video since the network came back (seq %d–%d): %v", first.Seq, videoTo, err)
		}
	}
}

// resumedOn returns the keyframe that a decoder resumes on after an outage: the first keyframe among a video's
// packets pkts, in sequence-number order (seqRange's), that arrived whole, and when its last packet arrived. Whole is
// one run of sequence numbers from the SPS to the RTP marker bit, with the fake marker of a keyframe; a second copy of
// a packet is no fault (a retransmission that overtook the original, which a jitter buffer drops). skipped counts the
// keyframes before it that arrived in part, and err says what was wrong with the first of them.
//
// It is mostly the first keyframe after the outage (waitResumed's). It isn't when that one is a keyframe of the
// outage that the core's NACK responder sent again after an ICE restart (checkResumedVideo): such a keyframe can
// arrive after the resumed stream's first packets but before its keyframe, and in part, when a loaded SFU drops some
// of the burst it came in. A decoder skips it.
func resumedOn(pkts []sfutest.Packet) (key sfutest.Packet, whole time.Time, skipped int, err error) {
	var frames [][]sfutest.Packet // in sequence-number order, without second copies
	for _, p := range pkts {
		switch {
		case p.Dup:
		case len(frames) == 0 || frames[len(frames)-1][0].TS != p.TS:
			frames = append(frames, []sfutest.Packet{p})
		default:
			frames[len(frames)-1] = append(frames[len(frames)-1], p)
		}
	}
	for _, f := range frames {
		keyMark := func(p sfutest.Packet) bool { return p.HasMark && p.Mark.Keyframe }
		if !f[0].KeyStart && !slices.ContainsFunc(f, keyMark) {
			continue
		}
		var bad error
		for _, check := range []func([]sfutest.Packet) error{
			sfutest.CheckStartsOnSPS, sfutest.CheckContinuous, sfutest.CheckVideoMarkers,
		} {
			if bad = check(f); bad != nil {
				break
			}
		}
		if bad != nil {
			if err == nil {
				err = fmt.Errorf("the keyframe at seq %d (%d packets): %w", f[0].Seq, len(f), bad)
			}
			skipped++
			continue
		}
		for _, p := range f {
			if p.Arrival.After(whole) {
				whole = p.Arrival
			}
		}
		return f[0], whole, skipped, err
	}
	switch {
	case err != nil:
	case len(pkts) == 0:
		err = errors.New("no packets")
	default:
		err = fmt.Errorf("no keyframe among seq %d–%d", pkts[0].Seq, pkts[len(pkts)-1].Seq)
	}
	return sfutest.Packet{}, time.Time{}, skipped, err
}

// TestResumedOn checks resumedOn on made-up packets: a keyframe that arrived in part, or without its SPS, is
// skipped for the next whole one, a second copy of a packet is no fault, and without a whole keyframe it says why.
func TestResumedOn(t *testing.T) {
	t0 := time.Now()
	// frame makes the packets of one fake video frame of n packets from seq on, the i-th arriving at t0+arrive[i] ms
	// (or t0): a keyframe's first packet carries the SPS, the next one the fake marker; a delta frame's first packet
	// carries the marker. The last one has the RTP marker bit.
	frame := func(seq uint16, ts uint32, n int, key bool, arrive ...int) []sfutest.Packet {
		out := make([]sfutest.Packet, n)
		for i := range out {
			p := sfutest.Packet{Seq: seq + uint16(i), TS: ts, Marker: i == n-1, Arrival: t0}
			if i < len(arrive) {
				p.Arrival = t0.Add(time.Duration(arrive[i]) * time.Millisecond)
			}
			switch {
			case key && i == 0:
				p.KeyStart = true
			case key && i == 1, !key && i == 0:
				p.Mark, p.HasMark = fake.Marker{RID: "f", Keyframe: key, Frame: ts / 3000}, true
			}
			out[i] = p
		}
		return out
	}
	join := func(parts ...[]sfutest.Packet) []sfutest.Packet { return slices.Concat(parts...) }
	without := func(pkts []sfutest.Packet, seqs ...uint16) []sfutest.Packet {
		return slices.DeleteFunc(slices.Clone(pkts), func(p sfutest.Packet) bool { return slices.Contains(seqs, p.Seq) })
	}
	dup := func(p sfutest.Packet) []sfutest.Packet { p.Dup, p.RTX = true, true; return []sfutest.Packet{p} }

	key := frame(100, 3000, 5, true, 0, 10, 40, 20, 30)
	inPart := without(frame(50, 1500, 5, true), 52, 53)
	noSPS := without(frame(50, 1500, 4, true), 50)
	tests := []struct {
		name        string
		pkts        []sfutest.Packet
		wantSeq     uint16
		wantWhole   time.Duration // 0: no keyframe arrived whole
		wantSkipped int
		wantErr     string
	}{
		{name: "the first keyframe whole", pkts: join(key, frame(105, 6000, 3, false)), wantSeq: 100,
			wantWhole: 40 * time.Millisecond},
		{name: "a second copy in it", pkts: join(key[:3], dup(key[1]), key[3:]), wantSeq: 100,
			wantWhole: 40 * time.Millisecond},
		{name: "the first keyframe in part", pkts: join(inPart, frame(55, 1800, 2, false), frame(57, 2100, 3, true, 7)),
			wantSeq: 57, wantWhole: 7 * time.Millisecond, wantSkipped: 1, wantErr: "seq 52–53 missing"},
		{name: "the first keyframe without its SPS", pkts: join(noSPS, frame(54, 2100, 3, true, 9)),
			wantSeq: 54, wantWhole: 9 * time.Millisecond, wantSkipped: 1, wantErr: "without an SPS"},
		{name: "no keyframe whole", pkts: join(without(key, 104), frame(105, 6000, 3, false)), wantSkipped: 1,
			wantErr: "0 RTP marker bits"},
		{name: "no keyframe", pkts: frame(105, 6000, 3, false), wantErr: "no keyframe among seq 105–107"},
		{name: "no packets", wantErr: "no packets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, whole, skipped, err := resumedOn(tc.pkts)
			switch {
			case tc.wantWhole == 0 && !whole.IsZero():
				t.Errorf("resumedOn found the keyframe at seq %d, want none", k.Seq)
			case tc.wantWhole != 0 && (whole.IsZero() || k.Seq != tc.wantSeq || whole.Sub(t0) != tc.wantWhole):
				t.Errorf("resumedOn found the keyframe at seq %d, whole at %v, want seq %d at %v (%v)", k.Seq,
					whole.Sub(t0), tc.wantSeq, tc.wantWhole, err)
			}
			if skipped != tc.wantSkipped {
				t.Errorf("resumedOn skipped %d keyframes, want %d", skipped, tc.wantSkipped)
			}
			if got := fmt.Sprint(err); (tc.wantErr == "" && err != nil) || !strings.Contains(got, tc.wantErr) {
				t.Errorf("resumedOn's error is %v, want one with %q", err, tc.wantErr)
			}
		})
	}
}

// TestDesktopSocketKilled is D3: the core's WebSocket dies mid-share, its TCP connection closed under it. The core
// resumes on one new socket, with its connection id, and nothing is published or negotiated again: one pub offer in
// all, the share's id unchanged, no room event about it. The share is held meanwhile, and its pub PC, still
// connected, goes on sending: the viewer's media has no gap of 500 ms or more.
func TestDesktopSocketKilled(t *testing.T) {
	const maxGap = 500 * time.Millisecond
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	app := w.recoveringDesktop(w.member("alice"), appEngine(31).engine, nil)
	sh, snap, video, audio := watched(t, app, bob, viewer)
	connID := app.core.Status().ConnectionID
	dialed := app.dialed()
	statusMark, shareMark := app.statusMark(), len(app.shareEvents())
	eventMark := len(bob.roomEvents())
	roomMark := len(bob.roomStates()) - 1

	// ---- the socket dies ----
	cut := time.Now()
	app.cutSocket()
	eventually(t, func() bool {
		_, away := app.wentAway(statusMark)
		return away && app.core.Status().Conn == core.ConnOnline
	}, func() string {
		return fmt.Sprintf("the core's statuses since the cut %+v, want it away and back online", app.statusesSince(statusMark))
	})
	back := time.Since(cut)
	// The media of the next second too: whatever the resume does must not disturb it.
	waitPackets(t, audio, 50)
	until := time.Now()
	t.Logf("the core resumed %v after the cut", back.Round(time.Millisecond))

	// ---- the media flowed through it ----
	for name, r := range map[string]*sfutest.Recorder{"video": video, "audio": audio} {
		gap, at := longestGap(r.Packets(), cut, until)
		t.Logf("%s: the longest gap from the cut to %v after it is %v, %v after the cut", name,
			until.Sub(cut).Round(time.Millisecond), gap.Round(time.Millisecond), at.Sub(cut).Round(time.Millisecond))
		if gap >= maxGap {
			t.Errorf("%s: no packet for %v, %v after the cut; want every gap under %v", name, gap.Round(time.Millisecond),
				at.Sub(cut).Round(time.Millisecond), maxGap)
		}
	}
	checkVideo(t, "video", video.Packets())
	if err := sfutest.CheckContinuous(audio.Packets()); err != nil {
		t.Errorf("audio: %v", err)
	}

	// ---- resumed, not republished ----
	if st := app.core.Status(); st.ConnectionID != connID {
		t.Errorf("the core's connection is %s after the resume, want %s as before", st.ConnectionID, connID)
	}
	if n := app.dialed() - dialed; n != 1 {
		t.Errorf("the core dialed %d sockets to resume, want one", n)
	}
	app.waitState(sh, core.StateLive)
	if id := sh.Snapshot().ID; id != snap.ID {
		t.Errorf("the share's id is %s after the resume, want %s: it was published again", id, snap.ID)
	}
	if offers := app.offers(); pubOfferList(offers) != "1/1" {
		t.Errorf("the core's pub offers are %s, want its first alone", pubOfferList(offers))
	}
	if evs := bob.roomEvents()[eventMark:]; len(evs) != 0 {
		t.Errorf("the viewer got the room events %+v since the cut, want none: a resume is no leave and no republish", evs)
	}
	if runs := shareRuns(bob, snap.ID, roomMark); runs != "live" {
		t.Errorf("the share went %q in room.state since the cut, want live all along", runs)
	}
	if strings.Contains(app.logs.String(), "publishing the share again") {
		t.Error("the core published the share again after a resume")
	}
	t.Logf("the share's states since the cut: %s", app.shareStates(shareMark))
	bob.check()
}

// TestDesktopResumeICERestart is D3 after a network switch: the core's UDP and its WebSocket go at once, and the
// socket can come back only once the pub PC's ICE is disconnected. On the resumed welcome the core restarts ICE at
// once (01 §11.7; M1 01 §10.4, the pub column's last row): its first pub offer after the welcome is of the same gen
// with the next neg and new ICE credentials, for the same share. Nothing is published again, and when the UDP is
// back the viewer's video starts again on a keyframe on the track it had.
//
// The UDP comes back only after that offer went, so the PC can't reconnect by itself before the welcome; its ICE
// checks go on until the network returns (faultySettings).
func TestDesktopResumeICERestart(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	pubNet, pubWire := faultySettings(t)
	app := w.recoveringDesktop(w.member("alice"), appEngine(32).engine, &pubNet)
	sh, snap, video, audio := watched(t, app, bob, viewer)
	connID := app.core.Status().ConnectionID
	dialed := app.dialed()
	statusMark, shareMark := app.statusMark(), len(app.shareEvents())
	eventMark := len(bob.roomEvents())

	// ---- the network goes: the UDP, and the socket, whose redial waits ----
	app.gate.hold()
	blackHole(pubWire, true)
	gone := time.Now()
	app.cutSocket()
	eventually(t, func() bool {
		_, away := app.wentAway(statusMark)
		return away && strings.Contains(app.logs.String(), `msg="pub PC" connected=false`)
	}, func() string {
		return fmt.Sprintf("%v into the outage the core's statuses are %+v, want it away with its pub PC disconnected",
			time.Since(gone).Round(time.Millisecond), app.statusesSince(statusMark))
	})
	t.Logf("the pub PC went disconnected %v into the outage", time.Since(gone).Round(time.Millisecond))
	if offers := pubOfferList(app.offers()); offers != "1/1" {
		t.Errorf("the core's pub offers while signaling was down are %s, want its first alone: it only records", offers)
	}
	videoMark, audioMark := len(video.Packets()), len(audio.Packets())

	// ---- the socket comes back: an ICE restart on the welcome ----
	// At once on the welcome, not by the 3 s rule of a disconnected PC, which would wait 3 s from when ICE went
	// disconnected, just before the redial.
	released := time.Now()
	app.gate.release()
	eventually(t, func() bool { return app.core.Status().Conn == core.ConnOnline && len(app.offers()) >= 2 },
		func() string {
			return fmt.Sprintf("the core is %s with the pub offers %s, want it online and an offer after the welcome",
				app.core.Status().Conn, pubOfferList(app.offers()))
		})
	if took := time.Since(released); took > 1500*time.Millisecond {
		t.Errorf("the core's first pub offer after the welcome came %v after its redial was let through, want it at "+
			"once on the welcome", took.Round(time.Millisecond))
	}
	blackHole(pubWire, false)
	back := time.Now()
	t.Logf("the UDP was gone for %v", back.Sub(gone).Round(time.Millisecond))

	// ---- the media is back, from a keyframe ----
	resumed, stragglers := waitResumed(t, video, videoMark, "f", 15)
	took := resumed[0].Arrival.Sub(back)
	t.Logf("the video came back %v after the network (%d packets of the old stream came first)",
		took.Round(time.Millisecond), stragglers)
	if took > 5*time.Second {
		t.Errorf("the video came back %v after the network, want within 5 s", took.Round(time.Millisecond))
	}
	if err := audio.Wait(waitCtx(t), func(r *sfutest.Recorder) bool {
		return slices.ContainsFunc(r.Packets()[audioMark:], func(p sfutest.Packet) bool { return p.Arrival.After(back) })
	}); err != nil {
		t.Fatalf("the audio didn't come back: %v", err)
	}
	checkResumedVideo(t, video, resumed, back, true)
	bob.waitLive(snap.ID)
	app.waitState(sh, core.StateLive)
	t.Logf("the share's states since the outage: %s", app.shareStates(shareMark))

	// ---- resumed with an ICE restart, not republished ----
	if got := pubOfferList(app.offers()); got != "1/1 1/2r" {
		t.Errorf("the core's pub offers are %s, want 1/1 1/2r: an ICE restart of the same gen on the welcome", got)
	}
	if st := app.core.Status(); st.ConnectionID != connID {
		t.Errorf("the core's connection is %s after the resume, want %s as before", st.ConnectionID, connID)
	}
	if n := app.dialed() - dialed; n != 1 {
		t.Errorf("the core dialed %d sockets to resume, want one", n)
	}
	if id := sh.Snapshot().ID; id != snap.ID {
		t.Errorf("the share's id is %s after the resume, want %s: it was published again", id, snap.ID)
	}
	if strings.Contains(app.logs.String(), "publishing the share again") {
		t.Error("the core published the share again after a resume")
	}
	if evs := bob.roomEvents()[eventMark:]; len(evs) != 0 {
		t.Errorf("the viewer got the room events %+v since the outage, want none: a resume is no leave and no republish",
			evs)
	}
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if n := tracksOf(viewer, snap.ID, kind); n != 1 {
			t.Errorf("the viewer has %d %s tracks of the share, want the one it had", n, kind)
		}
	}
	bob.check()
}

// TestDesktopResumeLostShare is D3 with a share that the server ended while the core's socket was away: alice stops
// it from her web tab, and the socket comes back. The room.state after the resumed welcome lacks the share, and the
// core publishes it again on the same connection (01 §11.7, M1 01 §10.5): share.start{replaces: the old id}, no
// room.join, and a new pub PC at the next gen with neg 1. The server keeps the connection's pub gen (M1 01 §9
// rule 2), so the new PC's offer is taken as it is: no refusal, no rebuild. The new share goes live on the same
// connection, and the viewer follows it by replaces.
func TestDesktopResumeLostShare(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	alice := w.member("alice")
	app := w.recoveringDesktop(alice, appEngine(33).engine, nil)
	tab := w.connect(alice, protocol.RolePublisher) // alice's web tab, in the room
	sh, snap, _, _ := watched(t, app, bob, viewer)
	connID := app.core.Status().ConnectionID
	statusMark := app.statusMark()

	// ---- the socket dies, and the share ends meanwhile ----
	app.gate.hold()
	app.cutSocket()
	eventually(t, func() bool {
		_, away := app.wentAway(statusMark)
		return away
	}, func() string {
		return fmt.Sprintf("the core's statuses since the cut %+v, want it away", app.statusesSince(statusMark))
	})
	tab.request(protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: snap.ID}, nil)
	eventually(t, func() bool {
		_, in := bob.shareIn(snap.ID)
		return !in
	}, func() string {
		return fmt.Sprintf("the viewer's room.state %+v still has %s", bob.roomState(), snap.ID)
	})

	// ---- the socket comes back: the share is published again ----
	app.gate.release()
	var next string
	eventually(t, func() bool {
		next = sh.Snapshot().ID
		return next != snap.ID && next != ""
	}, func() string { return fmt.Sprintf("the share is %+v, want a new id", sh.Snapshot()) })
	info := bob.waitLive(next)
	if info.Replaces != snap.ID || info.ConnectionID != connID {
		t.Errorf("the new share is %+v, want the core's on its connection %s, replacing %s", info, connID, snap.ID)
	}
	video := track(t, viewer, next, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 30)
	if err := sfutest.CheckStartsOnSPS(video.Packets()); err != nil {
		t.Errorf("the new share's video: %v", err)
	}
	waitPackets(t, track(t, viewer, next, webrtc.RTPCodecTypeAudio), 50)
	bob.waitStatus(want(next, true))
	app.waitState(sh, core.StateLive)

	// ---- on the same connection, at the next gen, at the first try ----
	if st := app.core.Status(); st.ConnectionID != connID {
		t.Errorf("the core's connection is %s after the resume, want %s as before", st.ConnectionID, connID)
	}
	if got := pubOfferList(app.offers()); got != "1/1 2/1" {
		t.Errorf("the core's pub offers are %s, want 1/1 2/1: the new PC at the next gen", got)
	}
	for _, line := range []string{"pub offer refused", "pub answer refused", "pub PC rebuild", "pub offer failed"} {
		if strings.Contains(app.logs.String(), line) {
			t.Errorf("the core's log has %q: the republished PC's offer should be taken at once", line)
		}
	}
	if shares := bob.roomState().Shares; len(shares) != 1 {
		t.Errorf("the room has the shares %+v, want the new one alone", shares)
	}
	select {
	case <-sh.Done():
		t.Errorf("the share ended: %+v", sh.Snapshot().End)
	default:
	}
	bob.check()
	tab.check()
}

// TestDesktopUDPBlackHole is D4: the core's UDP is gone for a while, its WebSocket is fine. The pub PC's ICE goes
// disconnected, and 3 s later the core restarts ICE: an offer of the same gen with the next neg. When that can't
// connect (the network is still gone 15 s after its offer) the core rebuilds the PC: gen 2, the same share. Either
// way the share is back to live under its id when the network is, and the viewer's video starts again on a keyframe
// on the track it had.
//
// The viewer's own network is fine (unlike M1's TestUDPBlackHoleViewer, which flaked once in group 3, the viewer
// here never has to recover), and the waits are on events, not on times: the outage lasts until the core has done
// what the case is about.
func TestDesktopUDPBlackHole(t *testing.T) {
	t.Run("ICE restart", func(t *testing.T) { desktopBlackHole(t, false) })
	t.Run("rebuild", func(t *testing.T) { desktopBlackHole(t, true) })
}

// desktopBlackHole runs D4. With rebuild the outage lasts until the core has rebuilt its pub PC, else until it has
// restarted ICE and the hub has said that the share is stalled (and 5 s at least).
func desktopBlackHole(t *testing.T, rebuild bool) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	pubNet, pubWire := faultySettings(t)
	app := w.recoveringDesktop(w.member("alice"), appEngine(41).engine, &pubNet)
	sh, snap, video, audio := watched(t, app, bob, viewer)
	roomMark := len(bob.roomStates()) - 1
	shareMark := len(app.shareEvents())

	// ---- the UDP is gone ----
	blackHole(pubWire, true)
	gone := time.Now()
	restarted := func() bool { return len(app.offers()) >= 2 }
	if rebuild {
		eventually(t, func() bool { return len(app.offers()) >= 3 }, func() string {
			return fmt.Sprintf("%v into the outage the core's pub offers are %s, want an ICE restart and a rebuild",
				time.Since(gone).Round(time.Millisecond), pubOfferList(app.offers()))
		})
	} else {
		eventually(t, func() bool {
			s, _ := bob.shareIn(snap.ID)
			return time.Since(gone) >= 5*time.Second && restarted() && s.Status == protocol.ShareStatusStalled
		}, func() string {
			s, _ := bob.shareIn(snap.ID)
			return fmt.Sprintf("%v into the outage the share is %s and the core's pub offers are %s, want it stalled "+
				"and an ICE restart", time.Since(gone).Round(time.Millisecond), s.Status, pubOfferList(app.offers()))
		})
	}
	if st := sh.Snapshot().State; st != core.StateReconnecting {
		t.Errorf("the share is %s during the outage, want reconnecting", st)
	}
	videoMark, audioMark := len(video.Packets()), len(audio.Packets())
	blackHole(pubWire, false)
	back := time.Now()
	t.Logf("the UDP was gone for %v", back.Sub(gone).Round(time.Millisecond))

	// ---- the media is back, from a keyframe ----
	resumed, stragglers := waitResumed(t, video, videoMark, "f", 15)
	took := resumed[0].Arrival.Sub(back)
	t.Logf("the video came back %v after the network (%d packets of the old stream came first)",
		took.Round(time.Millisecond), stragglers)
	if took > 5*time.Second {
		t.Errorf("the video came back %v after the network, want within 5 s", took.Round(time.Millisecond))
	}
	if err := audio.Wait(waitCtx(t), func(r *sfutest.Recorder) bool {
		return slices.ContainsFunc(r.Packets()[audioMark:], func(p sfutest.Packet) bool { return p.Arrival.After(back) })
	}); err != nil {
		t.Fatalf("the audio didn't come back: %v", err)
	}
	checkResumedVideo(t, video, resumed, back, !rebuild)
	bob.waitLive(snap.ID)
	if runs := shareRuns(bob, snap.ID, roomMark); runs != "live stalled live" {
		t.Errorf("the share went %q in room.state since the outage, want live, stalled, live again", runs)
	}
	app.waitState(sh, core.StateLive)
	t.Logf("the share's states since the outage: %s", app.shareStates(shareMark))

	// ---- what the core did: an ICE restart, then a rebuild if that couldn't connect ----
	offers := app.offers()
	t.Logf("the core's pub offers: %s", pubOfferList(offers))
	want := "1/1 1/2r"
	if rebuild {
		want = "1/1 1/2r 2/1"
	}
	if got := pubOfferList(offers); got != want {
		t.Errorf("the core's pub offers are %s, want %s", got, want)
	}
	if id := sh.Snapshot().ID; id != snap.ID {
		t.Errorf("the share's id is %s after the outage, want %s", id, snap.ID)
	}
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if n := tracksOf(viewer, snap.ID, kind); n != 1 {
			t.Errorf("the viewer has %d %s tracks of the share, want the one it had", n, kind)
		}
	}
	if offers := bob.subOffers(); len(offers) != 1 {
		t.Errorf("the viewer got the sub offers %s, want its first alone", offerList(offers))
	}
	bob.check()
}

// TestDesktopServerRestart is D5: the server restarts, on the same ports and data. The core's welcome from the new
// server is not resumed: a new connection id (Status.ConnectionID, with its EventStatus), and the share published
// again within 5 s, share.start{replaces: the old id} on a new pub PC of gen 1, while the engine's session goes on.
// The viewer moves its focus to the new share by replaces and gets its video within 5 s of the new server being up.
func TestDesktopServerRestart(t *testing.T) {
	const within = 5 * time.Second
	noLeaks(t)
	udp, tcp := freePort(t, "udp"), freePort(t, "tcp")
	w := newWorld(t, servertest.Options{Flags: []string{
		"--listen.ice-udp=127.0.0.1:" + strconv.Itoa(udp), "--listen.ice-tcp=127.0.0.1:" + strconv.Itoa(tcp),
	}})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	app := w.recoveringDesktop(w.member("alice"), appEngine(51).engine, nil)
	sh, snap, _, _ := watched(t, app, bob, viewer)
	before := app.core.Status().ConnectionID
	old, _ := bob.shareIn(snap.ID)
	sess := app.engine.Session()
	statusMark := app.statusMark()

	// ---- the restart ----
	stopping := time.Now()
	w.srv.Restart(t)
	up := time.Now()
	t.Logf("the new server was up %v after the old one began to stop", up.Sub(stopping).Round(time.Millisecond))

	// ---- the core published the share again on a new connection ----
	var next string
	eventually(t, func() bool {
		next = sh.Snapshot().ID
		return next != snap.ID && next != ""
	}, func() string { return fmt.Sprintf("the share is %+v, want a new id", sh.Snapshot()) })
	republished := time.Since(up)
	t.Logf("the core published the share again %v after the new server was up", republished.Round(time.Millisecond))
	if republished > within {
		t.Errorf("the core published the share again %v after the new server was up, want within %v",
			republished.Round(time.Millisecond), within)
	}
	st := app.core.Status()
	if st.ConnectionID == before || st.ConnectionID == "" || st.Conn != core.ConnOnline {
		t.Errorf("the core's status after the restart is %+v, want online with a new connection id (it was %s)", st, before)
	}
	if !slices.ContainsFunc(app.statusesSince(statusMark), func(s timedStatus) bool {
		return s.st.Conn == core.ConnOnline && s.st.ConnectionID == st.ConnectionID
	}) {
		t.Errorf("EventStatus since the restart %+v, want one online as %s", app.statusesSince(statusMark), st.ConnectionID)
	}
	info := bob.waitLive(next)
	if info.Replaces != snap.ID || info.ConnectionID != st.ConnectionID || info.UserID != old.UserID {
		t.Errorf("the new share is %+v, want the core's on its new connection %s, replacing %s", info, st.ConnectionID, snap.ID)
	}
	if shares := bob.roomState().Shares; len(shares) != 1 {
		t.Errorf("the room has the shares %+v, want the new one alone", shares)
	}

	// ---- the viewer followed it by replaces ----
	video := track(t, viewer, next, webrtc.RTPCodecTypeVideo)
	flowing := video.Stats().First.Sub(up)
	t.Logf("the viewer got the new share's video %v after the new server was up", flowing.Round(time.Millisecond))
	if flowing > within {
		t.Errorf("the viewer got the new share's video %v after the new server was up, want within %v",
			flowing.Round(time.Millisecond), within)
	}
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, track(t, viewer, next, webrtc.RTPCodecTypeAudio), 50)
	bob.waitStatus(want(next, true))
	if runs := layerRuns(checkVideo(t, "the new share's video", video.Packets())); runs != "f" {
		t.Errorf("the viewer got the layers %q of the new share, want the full layer: the focus moved with it", runs)
	}
	if d := bob.desired(); !slices.Equal(d, []protocol.SubscriptionWant{want(next, true)}) {
		t.Errorf("the viewer wants %+v, want the new share in focus with its audio", d)
	}

	// ---- the same local share, the same engine session, a new pub PC ----
	app.waitState(sh, core.StateLive)
	select {
	case <-sh.Done():
		t.Errorf("the share ended: %+v", sh.Snapshot().End)
	default:
	}
	if app.core.ActiveShare() != sh {
		t.Error("the active share isn't the one that was published again")
	}
	if app.engine.Session() != sess {
		t.Error("the engine session changed: a republish keeps it")
	}
	if got := pubOfferList(app.offers()); got != "1/1 1/1" {
		t.Errorf("the core's pub offers are %s, want one on each server", got)
	}
	bob.check()
}

// TestDesktopHoldExpiry is D6: the server stops and stays away for 35 s. The core holds the share for 30 s from
// the moment its connection went away, then ends it: server-unreachable, the engine session stopped. When the
// server is back the core connects again, with no share and in no room.
func TestDesktopHoldExpiry(t *testing.T) {
	const (
		hold    = 30 * time.Second
		slack   = time.Second
		outage  = 35 * time.Second
		endWait = hold + 10*time.Second
	)
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	app := w.recoveringDesktop(w.member("alice"), appEngine(61).engine, nil)
	sh, _, _, _ := watched(t, app, bob, viewer)
	before := app.core.Status().ConnectionID
	statusMark, shareMark := app.statusMark(), len(app.shareEvents())

	// ---- the server stops ----
	stopping := time.Now()
	w.srv.Stop(t)
	var away time.Time
	eventually(t, func() bool {
		var ok bool
		away, ok = app.wentAway(statusMark)
		return ok
	}, func() string { return "the core never went offline" })
	t.Logf("the core went offline %v after the server began to stop", away.Sub(stopping).Round(time.Millisecond))
	select {
	case <-sh.Done():
	case <-time.After(time.Until(away.Add(endWait))):
		t.Fatalf("the share didn't end %v after the core went offline: %+v", endWait, sh.Snapshot())
	}
	held := time.Since(away)
	t.Logf("the share ended %v after the core went offline", held.Round(time.Millisecond))
	if held < hold-slack || held > hold+slack {
		t.Errorf("the share ended %v after the core went offline, want %v ± %v", held.Round(time.Millisecond), hold, slack)
	}
	if end := sh.Snapshot().End; end == nil || *end != (core.EndInfo{Reason: core.EndServerUnreachable}) {
		t.Errorf("the share ended with %+v, want server-unreachable", end)
	}
	if app.engine.Session() != nil || app.core.ActiveShare() != nil {
		t.Errorf("after the end: engine session %v, active share %v; want neither", app.engine.Session(),
			app.core.ActiveShare())
	}
	if runs := app.shareStates(shareMark); runs != "reconnecting ended" {
		t.Errorf("the share's states since the stop are %q, want reconnecting, then ended", runs)
	}

	// ---- the server is back after 35 s: a new connection, no share, no room ----
	time.Sleep(time.Until(stopping.Add(outage)))
	w.srv.Restart(t)
	eventually(t, func() bool {
		st := app.core.Status()
		return st.Conn == core.ConnOnline && st.ConnectionID != before
	}, func() string {
		return fmt.Sprintf("the core's status is %+v, want online on a new connection", app.core.Status())
	})
	coreID := app.core.Status().ConnectionID
	eventually(t, func() bool {
		ids := slices.Concat(connIDsOf(bob.roomState())...)
		return len(bob.roomState().Participants) > 0 && !slices.Contains(ids, coreID) && !slices.Contains(ids, before) &&
			len(bob.roomState().Shares) == 0
	}, func() string {
		return fmt.Sprintf("the viewer's room.state %+v, want no share and no connection of the core", bob.roomState())
	})
	if st := app.core.Status(); st.Share != nil {
		t.Errorf("the core's status has the share %+v after the server came back", st.Share)
	}
}

// connIDsOf returns the connection ids of each participant of a room.state.
func connIDsOf(st protocol.RoomState) [][]string {
	var out [][]string
	for _, p := range st.Participants {
		out = append(out, connIDs(p))
	}
	return out
}
