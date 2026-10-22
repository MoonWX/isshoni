package itest

import (
	"context"
	"fmt"
	"log/slog"
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
// connection; and a server that stays away ends the share after the 30 s hold. The viewers are M1's harness clients,
// which recover by themselves (client_test.go); D17 (no leaks) is every case's.
//
// The core's own view comes from what it reports (Status, the share's snapshot, its events) and, for its pub offers,
// from its debug log: the "pub offer" lines name each offer's gen and neg, and whether it restarts ICE.

// recoveryApp is a desktop app for these cases: the core of desktopApp, whose WebSocket connections are recorded so
// that a test can cut the one in use, with the Pion settings the case gives its pub PC.
type recoveryApp struct {
	*desktopApp
	sock *sockets

	mu       sync.Mutex
	statuses []timedStatus // every EventStatus, with when it arrived
}

// timedStatus is an EventStatus and when the test received it.
type timedStatus struct {
	at time.Time
	st core.Status
}

// recoveringDesktop starts a desktop app for m, as desktop does, with settings for its pub PC (nil: loopback) and
// its sockets recorded. It waits until the connection is online; the app is closed and checked when the test ends.
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
	sock := &sockets{}
	c, err := core.New(core.Options{
		ServerURL:  w.srv.URL,
		Header:     http.Header{"Cookie": {strings.Join(cookies, "; ")}},
		Engine:     eng,
		Settings:   settings,
		HTTPClient: &http.Client{Transport: sock.transport(t, w.srv.Client.Transport)},
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &recoveryApp{
		desktopApp: &desktopApp{t: t, name: m.name + "'s app", core: c, engine: eng, runErr: make(chan error, 1), logs: logs},
		sock:       sock,
	}
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
	videoTo := newestSeq(video)
	time.Sleep(sfutest.FinalLossAfter + 200*time.Millisecond)
	vp := seqRange(video, resumed[0].Seq, videoTo)
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckStartsOnSPS} {
		if err := check(sfutest.WithoutPartialTail(vp)); err != nil {
			t.Errorf("the video since the network came back: %v", err)
		}
	}
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
