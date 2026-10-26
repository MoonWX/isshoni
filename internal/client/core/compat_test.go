package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/version"
)

// TestCompatServer is the client core against a running server (docs/m2/01-client-core.md §14 "Compatibility"),
// for 06's nightly compat-servers job (06 §10.3), which starts each of the last two server releases and runs
// `go test ./internal/client/... -run '^TestCompatServer'` with:
//
//   - ISSHONI_COMPAT_SERVER_URL: the server's origin (http://127.0.0.1:<port>), the test's only source of a server
//     (README §3 item 24). Without it the test skips.
//   - ISSHONI_COMPAT_ADMIN_FILE: a JSON file {"username": "…", "password": "…"} of the server's admin, which the job
//     writes after creating the admin.
//
// Against a server whose /api/v1/info lists no deviceFlow (every 0.1.x server) it asserts only compat.FromInfo's
// verdict update_server/no_device_flow: no M2 app can link to or use such a server, so it never joins or publishes
// there (06 §10.3). Against any other server:
//
//   - cookie: it signs in with POST /api/v1/auth/login and passes the session cookie as Options.Header, joins the
//     default room (M1's lounge), shares fake.Engine's picked source, and checks with an sfutest.Viewer on a second
//     cookie connection that both layers and the audio arrive, continuous over 5 s;
//   - bearer: it links a deviceauth.Manager (memory store) by the device flow, approving the code with the admin's
//     cookie as the /link page does (02 §16.2 case 1's lookup and decide), runs the same share with Options.Auth
//     (the manager, through an adapter as 03's session adapter), forces one refresh (AccessToken(ctx, true)), drops
//     the core's socket through its HTTPClient's dialer, and checks that the connection comes back with the
//     refreshed token and the share stays live.
func TestCompatServer(t *testing.T) {
	origin := strings.TrimSuffix(os.Getenv(envCompatServer), "/")
	if origin == "" {
		t.Skip(envCompatServer + " is not set: no server to test against (06 §10.3's compat-servers job sets it)")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		t.Fatalf("%s = %q is not an origin", envCompatServer, origin)
	}
	cs := &compatServer{t: t, origin: origin, loopback: loopbackHost(u.Hostname()), http: newCompatHTTP(t)}
	info := cs.info()
	check := compat.FromInfo(info, compat.Default())
	t.Logf("server %s, protocol %d–%d, features %v: %s %s", info.Server.Version, info.Protocol.Min,
		info.Protocol.Current, info.Features, check.Verdict, check.Reason)
	if !slices.Contains(info.Features, api.InfoFeatureDeviceFlow) {
		if check.Verdict != compat.UpdateServer || check.Reason != compat.ReasonNoDeviceFlow {
			t.Fatalf("compat.FromInfo for a server without deviceFlow = %+v, want update_server/no_device_flow", check)
		}
		return
	}
	if check.Verdict != compat.OK {
		t.Fatalf("compat.FromInfo = %+v, want ok: this build can't use the server", check)
	}
	cs.cookie = cs.login(readAdminFile(t))

	t.Run("cookie", func(t *testing.T) {
		app := cs.app(t, Options{Header: http.Header{"Cookie": {cs.cookie}}})
		v := cs.viewer(t)
		sh := app.share(v.room)
		v.watchBoth(t, sh.Snapshot().ID)
	})

	t.Run("bearer", func(t *testing.T) {
		mgr := cs.linkedManager(t)
		auth := &compatAuth{m: mgr}
		app := cs.app(t, Options{Auth: auth})
		v := cs.viewer(t)
		sh := app.share(v.room)
		id, connID := sh.Snapshot().ID, app.c.Status().ConnectionID
		v.watchBoth(t, id)

		used := auth.last()
		fresh, err := mgr.AccessToken(ctxFor(t), true)
		if err != nil {
			t.Fatalf("the forced refresh: %v", err)
		}
		if fresh == used {
			t.Fatal("the forced refresh gave the access token the core already had")
		}
		dials, issued, mark := app.dials.count(), auth.count(), len(app.connStates())
		app.dials.dropAll()
		// Back online after the drop: an EventStatus that isn't online, then one that is.
		back := func() bool {
			seen := app.connStates()[mark:]
			down := slices.IndexFunc(seen, func(c string) bool { return c != ConnOnline })
			return down >= 0 && slices.Contains(seen[down:], ConnOnline)
		}
		eventually(t, func() bool { return back() && app.dials.count() > dials && auth.count() > issued },
			func() string {
				return fmt.Sprintf("after the dropped socket: states %v, %d dials (had %d), %d tokens (had %d)",
					app.connStates()[mark:], app.dials.count(), dials, auth.count(), issued)
			})
		if auth.last() != fresh {
			t.Error("the connection came back with another token than the refreshed one")
		}
		if st := app.c.Status(); st.ConnectionID != connID {
			t.Logf("the connection was not resumed: %s, then %s (the share is published again)", connID,
				st.ConnectionID)
		}
		app.waitLive(sh)
		select {
		case <-sh.Done():
			t.Fatalf("the share ended: %+v", sh.Snapshot().End)
		default:
		}
		v.watchBoth(t, sh.Snapshot().ID)
	})
}

// The environment of TestCompatServer (06 §10.3).
const (
	envCompatServer = "ISSHONI_COMPAT_SERVER_URL"
	envCompatAdmin  = "ISSHONI_COMPAT_ADMIN_FILE"
)

// compatAdmin is ISSHONI_COMPAT_ADMIN_FILE's content.
type compatAdmin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// readAdminFile reads ISSHONI_COMPAT_ADMIN_FILE. A server with deviceFlow needs it: the device's code is approved
// with the admin's cookie.
func readAdminFile(t *testing.T) compatAdmin {
	t.Helper()
	path := os.Getenv(envCompatAdmin)
	if path == "" {
		t.Fatalf("%s is set, %s isn't: the test signs in as the server's admin", envCompatServer, envCompatAdmin)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the file the job names, read in a test
	if err != nil {
		t.Fatalf("%s: %v", envCompatAdmin, err)
	}
	var a compatAdmin
	if err := json.Unmarshal(b, &a); err != nil || a.Username == "" || a.Password == "" {
		t.Fatalf("%s: want {\"username\", \"password\"} (%v)", envCompatAdmin, err)
	}
	return a
}

// compatServer is the server under test, with the admin's session cookie once signed in.
type compatServer struct {
	t        *testing.T
	origin   string
	loopback bool // the server is on a loopback address: the PeerConnections use loopback candidates
	http     *http.Client
	cookie   string // "name=value" of the admin's session
}

// newCompatHTTP returns an HTTP client for the test's REST calls; its idle connections close when the test ends.
func newCompatHTTP(t *testing.T) *http.Client {
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// do sends a JSON request to the server, with the admin's cookie when it has one, decodes the answer into out (nil
// drops it) and returns the cookies it set. Any status but want fails the test. No Origin header: a request without
// browser headers passes the server's CSRF check, as the app's own requests do (02 §11.2).
func (cs *compatServer) do(method, path string, body any, want int, out any) []*http.Cookie {
	cs.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			cs.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctxFor(cs.t), method, cs.origin+path, rd)
	if err != nil {
		cs.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cs.cookie != "" {
		req.Header.Set("Cookie", cs.cookie)
	}
	res, err := cs.http.Do(req)
	if err != nil {
		cs.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != want {
		cs.t.Fatalf("%s %s = %d %s, want %d", method, path, res.StatusCode, b, want)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			cs.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return res.Cookies()
}

// info is GET /api/v1/info.
func (cs *compatServer) info() api.Info {
	cs.t.Helper()
	var info api.Info
	cs.do(http.MethodGet, "/api/v1/info", nil, http.StatusOK, &info)
	return info
}

// login signs the admin in (POST /api/v1/auth/login) and returns the session cookie as a Cookie header's value.
func (cs *compatServer) login(a compatAdmin) string {
	cs.t.Helper()
	set := cs.do(http.MethodPost, "/api/v1/auth/login", api.LoginRequest{Username: a.Username, Password: a.Password},
		http.StatusOK, nil)
	var cookies []string
	for _, c := range set {
		if c.Value != "" {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
	}
	if len(cookies) == 0 {
		cs.t.Fatal("the login set no cookie")
	}
	return strings.Join(cookies, "; ")
}

// settings are the Pion settings of the test's PeerConnections: loopback candidates for a server on a loopback
// address (06's job runs it on 127.0.0.1), Pion's defaults otherwise.
func (cs *compatServer) settings() webrtc.SettingEngine {
	if cs.loopback {
		return sfutest.LoopbackSettings()
	}
	return webrtc.SettingEngine{}
}

// linkedManager links a deviceauth.Manager with a memory store to the admin's account by the device flow: Link, then
// the /link page's lookup and decide with the admin's cookie. It closes when the test ends; the device is unlinked
// (revoked) first.
func (cs *compatServer) linkedManager(t *testing.T) *deviceauth.Manager {
	t.Helper()
	dc, err := deviceauth.NewClient(cs.origin, deviceauth.AppInfo{Version: version.Version(),
		ClientKind: protocol.ClientKindDesktop, OS: clientOS(), DeviceName: "Compat test"},
		deviceauth.ClientOptions{AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	mgr := deviceauth.NewManager(deviceauth.ManagerOptions{Client: dc, Store: credstore.NewMemory(),
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := mgr.Unlink(ctx); err != nil {
			t.Logf("unlinking the test's device: %v", err)
		}
		_ = mgr.Close()
		if t.Failed() {
			t.Logf("the device manager's log:\n%s", logs.String())
		}
	})
	if err := mgr.Start(ctxFor(t)); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	if err := mgr.Link(ctxFor(t)); err != nil {
		t.Fatalf("Manager.Link: %v", err)
	}
	st := mgr.State()
	if st.Status != deviceauth.StatusLinking || st.Link == nil {
		t.Fatalf("after Link the manager is %s, want linking with a code", st.Status)
	}
	var lookup api.DeviceLookup
	cs.do(http.MethodPost, "/api/v1/device/lookup", api.DeviceLookupRequest{UserCode: st.Link.UserCode},
		http.StatusOK, &lookup)
	if lookup.UserCode != st.Link.UserCode || lookup.ClientKind != string(protocol.ClientKindDesktop) {
		t.Fatalf("the lookup shows code %q of a %q client, want the manager's code of a desktop app", lookup.UserCode,
			lookup.ClientKind)
	}
	name := "Compat test"
	cs.do(http.MethodPost, "/api/v1/device/decide",
		api.DeviceDecideRequest{UserCode: st.Link.UserCode, Approve: true, DeviceName: &name}, http.StatusNoContent, nil)
	eventually(t, func() bool { return mgr.State().Status == deviceauth.StatusLinked },
		func() string { return fmt.Sprintf("the manager is %s, want linked", mgr.State().Status) })
	return mgr
}

// compatAuth is core.Auth over a deviceauth.Manager, as 03's session adapter makes it (the Manager's SessionEnded
// returns its State). It remembers how many tokens it handed out and the last one, never logging them.
type compatAuth struct {
	m      *deviceauth.Manager
	mu     sync.Mutex
	n      int
	latest protocol.Secret
}

func (a *compatAuth) AccessToken(ctx context.Context, force bool) (protocol.Secret, error) {
	tok, err := a.m.AccessToken(ctx, force)
	if err == nil {
		a.mu.Lock()
		a.n, a.latest = a.n+1, tok
		a.mu.Unlock()
	}
	return tok, err
}

func (a *compatAuth) SessionEnded(ctx context.Context, code protocol.ErrorCode) {
	a.m.SessionEnded(ctx, code)
}

func (a *compatAuth) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

func (a *compatAuth) last() protocol.Secret {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latest
}

// dialRecorder keeps the TCP connections an HTTP client dials, so a test can drop them.
type dialRecorder struct {
	mu    sync.Mutex
	conns []net.Conn
}

// transport returns an HTTP transport that dials through the recorder.
func (d *dialRecorder) transport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err == nil {
			d.mu.Lock()
			d.conns = append(d.conns, c)
			d.mu.Unlock()
		}
		return c, err
	}}
}

func (d *dialRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

// dropAll closes every connection dialed so far, without a close frame: the network went away under it.
func (d *dialRecorder) dropAll() {
	d.mu.Lock()
	conns := slices.Clone(d.conns)
	d.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// compatApp is the core under test on a fake engine, running until the test ends.
type compatApp struct {
	t      *testing.T
	c      *Client
	engine *fake.Engine
	dials  *dialRecorder

	mu    sync.Mutex
	conns []string // Status.Conn of every EventStatus, in order
}

// connStates returns Status.Conn of the EventStatus events so far.
func (a *compatApp) connStates() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.conns)
}

// app starts the core with o's credentials (Header or Auth) against the server, its upgrade requests dialed
// through a dialRecorder, and waits until it is online. When the test ends the core closes (Run returns nil) and
// its engine's session has stopped.
func (cs *compatServer) app(t *testing.T, o Options) *compatApp {
	t.Helper()
	logs := &logBuffer{}
	dials := &dialRecorder{}
	tr := dials.transport()
	se := cs.settings()
	engine := fake.NewEngine(fake.EngineConfig{SourceSize: [2]int{1280, 720}})
	o.ServerURL, o.Settings, o.HTTPClient, o.Engine = cs.origin, &se, &http.Client{Transport: tr}, engine
	o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	a := &compatApp{t: t, c: c, engine: engine, dials: dials}
	events, unsub := c.Subscribe()
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		for ev := range events {
			if ev.Kind == EventStatus && ev.Status != nil {
				a.mu.Lock()
				a.conns = append(a.conns, ev.Status.Conn)
				a.mu.Unlock()
			}
		}
	}()
	ran := make(chan error, 1)
	go func() { ran <- c.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case err := <-ran:
			if err != nil {
				t.Errorf("Run = %v, want nil after Close", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Run did not return after Close")
		}
		unsub()
		<-recorded
		if a.engine.Session() != nil {
			t.Error("the engine's session still runs after Close")
		}
		_ = a.engine.Close()
		tr.CloseIdleConnections()
		if t.Failed() {
			t.Logf("the core's log:\n%s", logs.String())
		}
	})
	eventually(t, func() bool { return c.Status().Conn == ConnOnline || c.Status().Conn == ConnStopped },
		func() string { return fmt.Sprintf("the core is %+v, want online", c.Status()) })
	if st := c.Status(); st.Conn != ConnOnline {
		t.Fatalf("the core stopped: %v", st.Stop)
	}
	return a
}

// share picks the fake engine's window and shares it in room with audio, and waits until it is live.
func (a *compatApp) share(room string) *Share {
	a.t.Helper()
	ctx := ctxFor(a.t)
	src, err := a.engine.Pick(ctx, media.PickRequest{})
	if err != nil {
		a.t.Fatal(err)
	}
	sh, err := a.c.StartShare(ctx, ShareRequest{RoomID: room, Source: src, Preset: protocol.PresetAuto, Audio: true})
	if err != nil {
		a.t.Fatalf("StartShare in %s: %v", room, err)
	}
	a.waitLive(sh)
	return sh
}

// waitLive waits until the share is live.
func (a *compatApp) waitLive(sh *Share) {
	a.t.Helper()
	eventually(a.t, func() bool { return sh.Snapshot().State == StateLive },
		func() string { return fmt.Sprintf("the share is %+v, want it live", sh.Snapshot()) })
}

// compatViewer is a second connection of the admin's: role viewer, in the default room, whose sub PC is an
// sfutest.Viewer that answers the server's offers.
type compatViewer struct {
	t      *testing.T
	sig    *signal.Client
	v      *sfutest.Viewer
	room   string
	pumped chan struct{} // closed when pump has returned

	mu       sync.Mutex
	gen, neg uint32 // of the last sub offer answered
	answer   string // its answer
	failed   error  // what answering the offers ran into
}

// viewer opens the viewer connection with the admin's cookie and joins the default room.
func (cs *compatServer) viewer(t *testing.T) *compatViewer {
	t.Helper()
	tr := &http.Transport{}
	sig, err := signal.Dial(ctxFor(t), signal.Options{
		URL:        cs.origin + "/ws",
		Header:     http.Header{"Cookie": {cs.cookie}},
		HTTPClient: &http.Client{Transport: tr},
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: version.Version(), OS: clientOS()},
		Role:       protocol.RoleViewer,
		Caps: protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline,
			protocol.CodecH264High, protocol.CodecOpus}, Encode: []protocol.CodecKey{}},
	})
	if err != nil {
		t.Fatalf("the viewer's dial: %v", err)
	}
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: cs.settings()})
	if err != nil {
		_ = sig.Close()
		t.Fatal(err)
	}
	cv := &compatViewer{t: t, sig: sig, v: v, pumped: make(chan struct{})}
	go cv.pump()
	t.Cleanup(func() {
		_ = sig.Close()
		<-cv.pumped
		if err := v.Close(); err != nil {
			t.Errorf("Viewer.Close: %v", err)
		}
		tr.CloseIdleConnections()
	})
	var joined protocol.RoomJoinResult
	room := sig.Welcome().DefaultRoomID
	if err := sig.Request(ctxFor(t), protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room}, &joined); err != nil {
		t.Fatalf("the viewer's room.join of %s: %v", room, err)
	}
	cv.room = room
	return cv
}

// pump answers the server's sub offers until the connection closes; it drops every other notification.
func (cv *compatViewer) pump() {
	defer close(cv.pumped)
	for env := range cv.sig.Events() {
		if env.Type != protocol.MessageTypePCOffer {
			continue
		}
		o, err := protocol.Decode[protocol.PCOffer](env)
		if err == nil {
			err = cv.answerOffer(o)
		}
		if err != nil {
			cv.mu.Lock()
			cv.failed = errors.Join(cv.failed, err)
			cv.mu.Unlock()
		}
	}
}

// answerOffer answers a sub offer as 01 §9 says: a new gen gets a new PeerConnection, the repeat of the offer last
// answered gets that answer again, an older one is dropped.
func (cv *compatViewer) answerOffer(o protocol.PCOffer) error {
	if o.PC != protocol.PCKindSub {
		return fmt.Errorf("an offer for the %s PC", o.PC)
	}
	cv.mu.Lock()
	gen, neg, prev := cv.gen, cv.neg, cv.answer
	cv.mu.Unlock()
	switch {
	case o.Gen < gen, o.Gen == gen && o.Neg < neg:
		return nil
	case o.Gen == gen && o.Neg == neg:
		return cv.sig.Send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg,
			SDP: protocol.SDP(prev)})
	case o.Gen > gen && gen != 0:
		if err := cv.v.Rebuild(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	answer, err := cv.v.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP.Reveal()})
	if err != nil {
		return err
	}
	cv.mu.Lock()
	cv.gen, cv.neg, cv.answer = o.Gen, o.Neg, answer.SDP
	cv.mu.Unlock()
	return cv.sig.Send(protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg,
		SDP: protocol.SDP(answer.SDP)})
}

// subscribe sends one subscription.
func (cv *compatViewer) subscribe(w protocol.SubscriptionWant) {
	cv.t.Helper()
	var res protocol.SubscribeResult
	err := cv.sig.Request(ctxFor(cv.t), protocol.MessageTypeSubscribeUpdate,
		protocol.SubscribeUpdate{Subs: []protocol.SubscriptionWant{w}}, &res)
	if err != nil || len(res.Ignored) != 0 {
		cv.t.Fatalf("subscribe.update %+v: ignored %v, %v", w, res.Ignored, err)
	}
}

// recorder waits for the Viewer's track of a kind in the share's stream (the SFU names the stream after the share).
func (cv *compatViewer) recorder(shareID string, kind webrtc.RTPCodecType) *sfutest.Recorder {
	cv.t.Helper()
	r, err := cv.v.WaitRecorder(ctxFor(cv.t), func(r *sfutest.Recorder) bool {
		return r.Kind() == kind && r.StreamID() == shareID
	})
	if err != nil {
		cv.t.Fatalf("the viewer got no %s of %s: %v", kind, shareID, err)
	}
	return r
}

// framesOf counts the whole frames of layer rid among pkts (fake.Engine marks each frame with its layer).
func framesOf(pkts []sfutest.Packet, rid string) int {
	n := 0
	for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(pkts)) {
		if f.HasMark && f.Mark.RID == rid {
			n++
		}
	}
	return n
}

// watchBoth watches the share for 5 s: the full layer with the audio for 2.5 s, then the preview with the audio for
// 2.5 s. Both layers and the audio must arrive, each stream continuous (sfutest.CheckContinuous).
func (cv *compatViewer) watchBoth(t *testing.T, shareID string) {
	t.Helper()
	const half = 2500 * time.Millisecond
	cv.subscribe(protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn})
	video, audio := cv.recorder(shareID, webrtc.RTPCodecTypeVideo), cv.recorder(shareID, webrtc.RTPCodecTypeAudio)
	vmark, amark := len(video.Packets()), len(audio.Packets())
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		if err := video.Wait(ctx, func(*sfutest.Recorder) bool { return cond() }); err != nil {
			t.Fatalf("%s: %v (%d video packets since the subscription)", what, err, len(video.Packets())-vmark)
		}
	}
	waitFor("frames of the full layer", func() bool { return framesOf(video.Packets()[vmark:], "f") >= 10 })
	time.Sleep(half)
	cv.subscribe(protocol.SubscriptionWant{ShareID: shareID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn})
	waitFor("frames of the preview", func() bool { return framesOf(video.Packets()[vmark:], "q") >= 5 })
	time.Sleep(half)

	vp, ap := sfutest.WithoutPartialTail(video.Packets()[vmark:]), audio.Packets()[amark:]
	if err := sfutest.CheckContinuous(vp); err != nil {
		t.Errorf("video: %v", err)
	}
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if f, q := framesOf(vp, "f"), framesOf(vp, "q"); f < 30 || q < 15 {
		t.Errorf("the viewer got %d frames of the full layer and %d of the preview in 5 s, want both", f, q)
	}
	if len(ap) < 150 { // 50 packets a second
		t.Errorf("the viewer got %d audio packets in 5 s, want the audio", len(ap))
	}
	cv.mu.Lock()
	failed := cv.failed
	cv.mu.Unlock()
	if failed != nil {
		t.Errorf("the viewer's sub PC: %v", failed)
	}
}
