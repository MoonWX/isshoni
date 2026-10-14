package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The log canary of 04 §17 (README S90): the full flow of an evening runs against a server that logs at debug, and
// its log is searched for what must never be in it (04 §10, README §4 "Logging"): a secret of any kind (the setup,
// invite and reset tokens, passwords, session cookies, resume tokens, the keys of secrets.json, a push
// subscription's keys), SDP (its attribute lines, an ICE password), an ICE candidate, a push endpoint beyond its
// host, a username or a share label, at any level; and a client's address, IP or ICE transport address, in any
// line at info or above, except as the remote_ip of a security event at warn (01 §17, 03 §7.3: a refused WebSocket
// Origin, throttled pre-auth upgrades, a throttled auth attempt), of which the flow makes one.
//
// What the canary allows: Pion's own warnings (they carry the attribute scope), which the SFU keeps at warn when they
// name no address (sfu/pionlog.go). Two of them come with every PeerConnection in a normal evening, at log.level = info
// as well: "Failed to ping without candidate pairs. Connection is not possible yet." (scope ice) at its start, when its
// ICE agent begins the checks before it has a candidate pair (the server learns a client's address from the client's
// checks), and "Failed to accept RTP stream is already closed" with its RTCP twin (scope pc) when it closes: every
// share, every viewer and every probe. "Incoming unhandled RTCP ssrc(…), OnTrack will not be fired" (scope pc) comes
// now and then besides. How many appear depends on the moment and on logx's limit per scope (20 lines a minute, which
// Pion's debug lines use up at this test's level), so the canary does not count them; it holds them to the same rules
// as every other line, and S79's and netx's Pion loggers have moved every line of Pion's that names an address to
// debug. The machine's own warnings (hostWarning) are allowed too. Client addresses at debug level are allowed: debug
// is for the operator who is looking for a problem, for a while (`isshoni admin log-level debug --for 30m`), and Pion's
// ICE lines name the pairs it checks.

// The canary's people and what they type. The names are unusual on purpose: a common word could be in the log for
// another reason.
const (
	canaryAdmin         = "Quillwren"
	canaryMember        = "Junoberry"
	canaryAdminIP       = "203.0.113.77"  // as the operator's proxy forwards it (off mode, 04 §8.5)
	canaryMemberIP      = "198.51.100.77" // as the operator's proxy forwards it
	canaryLabel         = "movie night canary"
	canaryAdminPassword = "canary admin passphrase one"
)

// proxied is the transport of a browser behind the operator's proxy: every request carries the client's
// address in X-Forwarded-For, which an off-mode server on loopback takes from its loopback proxy.
type proxied struct {
	base http.RoundTripper
	ip   string
}

func (f proxied) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", f.ip)
	return f.base.RoundTrip(r)
}

// canary collects what the log must not hold.
type canary struct {
	secrets    map[string]string // value → what it is
	clientAddr map[string]string // value → whose address
}

func (c *canary) secret(what, value string) {
	if len(value) >= 6 { // shorter values would match by chance
		c.secrets[value] = what
	}
}

func (c *canary) addr(what, value string) { c.clientAddr[value] = what }

// sdp keeps the ICE passwords of a session description; its attribute lines are searched for by their names.
func (c *canary) sdp(what, sdp string) {
	for _, m := range icePwd.FindAllStringSubmatch(sdp, -1) {
		c.secret(what+"'s ICE password", m[1])
	}
}

var icePwd = regexp.MustCompile(`a=ice-pwd:(\S+)`)

// securityEvents are the messages of the security events, logged at warn with the client's IP as remote_ip:
// signal's refused upgrades (01 §17) and auth's blocked attempts (03 §7.3).
var securityEvents = map[string]bool{
	"websocket origin refused":                         true,
	"websocket origin refused for a cookie connection": true,
	"websocket pre-auth upgrades throttled":            true,
	"auth attempt throttled":                           true,
}

// sdpMarkers are what SDP and ICE candidates look like inside a JSON log line.
var sdpMarkers = []string{"a=ice-ufrag", "a=ice-pwd", "a=fingerprint", "a=candidate", "a=rtpmap", "a=mid:", "a=ssrc",
	"a=group:BUNDLE", "v=0\\r\\n", "candidate:", "typ host", "typ srflx"}

// canaryBrowser is a browser behind the proxy, with the client address ip.
func canaryBrowser(t *testing.T, srv *servertest.Server, ip string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Transport: proxied{base: srv.Client.Transport, ip: ip}}
}

// localAddr returns the local transport address ("ip:port") of a PeerConnection's selected candidate pair, once
// there is one.
func localAddr(t *testing.T, what string, transport func() *webrtc.ICETransport) string {
	t.Helper()
	var addr string
	waitFor(t, what+"'s selected candidate pair", func() bool {
		tr := transport()
		if tr == nil {
			return false
		}
		pair, err := tr.GetSelectedCandidatePair()
		if err != nil || pair == nil {
			return false
		}
		addr = fmt.Sprintf("%s:%d", pair.Local.Address, pair.Local.Port)
		return true
	})
	return addr
}

// senderICE and receiverICE return a media PeerConnection's ICE transport.
func senderICE(pc *webrtc.PeerConnection) func() *webrtc.ICETransport {
	return func() *webrtc.ICETransport {
		for _, s := range pc.GetSenders() {
			if tr := s.Transport(); tr != nil {
				return tr.ICETransport()
			}
		}
		return nil
	}
}

func receiverICE(pc *webrtc.PeerConnection) func() *webrtc.ICETransport {
	return func() *webrtc.ICETransport {
		for _, r := range pc.GetReceivers() {
			if tr := r.Transport(); tr != nil {
				return tr.ICETransport()
			}
		}
		return nil
	}
}

// canaryShare starts a share with a label on c and publishes fake media on it, as goLive does, and returns the
// share's id and the publisher.
func canaryShare(t *testing.T, c *signaltest.Client, k *canary) (string, *publish.Publisher) {
	t.Helper()
	params := request[protocol.ShareParams](t, c, protocol.MessageTypeShareStart, protocol.ShareStart{
		Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Audio: true, Ref: "canary", Label: canaryLabel,
	})
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Minute, Audio: true, Seed: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := publish.New(publish.Options{Source: src, Settings: sfutest.LoopbackSettings(), Audio: true})
	if err != nil {
		_ = src.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = pub.Close()
		_ = src.Close()
	})
	offerCtx, cancelOffer := context.WithTimeout(ctx, 10*time.Second)
	defer cancelOffer()
	offer, err := pub.Offer(offerCtx)
	if err != nil {
		t.Fatalf("pub offer: %v", err)
	}
	k.sdp("the sharer's offer", offer.SDP)
	var tracks []protocol.TrackRef
	for _, tr := range pub.Tracks() {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: params.ShareID, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(tracks, ref) {
			tracks = append(tracks, ref)
		}
	}
	if err := c.Send(protocol.MessageTypePCOffer, "", protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: protocol.SDP(offer.SDP), Tracks: tracks,
	}); err != nil {
		t.Fatal(err)
	}
	env := nextOf(t, c, protocol.MessageTypePCAnswer, protocol.MessageTypeError)
	answer, err := protocol.Decode[protocol.PCAnswer](env)
	if env.Type != protocol.MessageTypePCAnswer || err != nil {
		t.Fatalf("the pub offer was answered with %s %s", env.Type, env.Data)
	}
	k.sdp("the server's pub answer", answer.SDP.Reveal())
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP.Reveal()}); err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return params.ShareID, pub
}

// canaryWatch subscribes c to the share and answers the server's sub offer with a Viewer, which it returns once
// media arrives.
func canaryWatch(t *testing.T, c *signaltest.Client, shareID string, k *canary) *sfutest.Viewer {
	t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: sfutest.LoopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	request[protocol.SubscribeResult](t, c, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
		Subs: []protocol.SubscriptionWant{{ShareID: shareID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		offer, err := protocol.Decode[protocol.PCOffer](nextOf(t, c, protocol.MessageTypePCOffer))
		if err != nil {
			t.Fatal(err)
		}
		if offer.PC != protocol.PCKindSub {
			continue
		}
		k.sdp("the server's sub offer", offer.SDP.Reveal())
		answer, err := v.Answer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP.Reveal()})
		if err != nil {
			t.Fatal(err)
		}
		k.sdp("the viewer's answer", answer.SDP)
		if err := c.Send(protocol.MessageTypePCAnswer, "", protocol.PCAnswer{
			PC: offer.PC, Gen: offer.Gen, Neg: offer.Neg, SDP: protocol.SDP(answer.SDP),
		}); err != nil {
			t.Fatal(err)
		}
		break
	}
	if _, err := v.WaitRecorder(ctx, func(r *sfutest.Recorder) bool {
		return r.Kind() == webrtc.RTPCodecTypeVideo && r.Stats().Packets > 30
	}); err != nil {
		t.Fatalf("the viewer got no video: %v", err)
	}
	return v
}

// cookies keeps the values of the cookies a browser holds for the site.
func (c *canary) cookies(srv *servertest.Server, who string, b *http.Client) {
	u, _ := url.Parse(srv.URL)
	for _, ck := range b.Jar.Cookies(u) {
		c.secret(who+"'s cookie "+ck.Name, ck.Value)
	}
}

// fragmentToken is the token of a link of the form <origin>/<page>#<token>.
func fragmentToken(t *testing.T, link string) string {
	t.Helper()
	_, token, ok := strings.Cut(link, "#")
	if !ok || token == "" {
		t.Fatalf("link %q has no token", link)
	}
	return token
}

// TestLogCanary runs the flow and searches the log (see the top of the file).
func TestLogCanary(t *testing.T) {
	if testing.Short() {
		t.Skip("the full flow with media")
	}
	pushes := servertest.NewPushService()
	deps := wiredDeps()
	deps.PushSender, deps.Resolver = pushes, pushes
	srv := servertest.Start(t, servertest.Options{Deps: deps})
	k := &canary{secrets: map[string]string{}, clientAddr: map[string]string{
		canaryAdminIP: "the admin's IP", canaryMemberIP: "the member's IP",
	}}
	k.secret("the admin's password", canaryAdminPassword)
	k.secret("the admin's username", canaryAdmin)
	k.secret("the member's username", canaryMember)
	k.secret("the share's label", canaryLabel)
	admin, member := canaryBrowser(t, srv, canaryAdminIP), canaryBrowser(t, srv, canaryMemberIP)
	sock, ctx := adminSocket(t, srv)
	// want fails the test unless res has the status; ok is any 2xx.
	const ok = 0
	want := func(what string, res response, status int) {
		t.Helper()
		if status == ok && res.status/100 == 2 || res.status == status {
			return
		}
		t.Fatalf("%s = %d %s, want %d (0: any 2xx)", what, res.status, res.body, status)
	}

	// The operator's setup link, the admin's account, a login with the password.
	link, err := sock.SetupURL(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	setupToken := fragmentToken(t, link.URL)
	k.secret("the setup token", setupToken)
	want("setup/check", callAs(t, admin, srv, http.MethodPost, "/api/v1/auth/setup/check", api.TokenRequest{Token: setupToken}), ok)
	want("setup/complete", callAs(t, admin, srv, http.MethodPost, "/api/v1/auth/setup/complete", api.SetupCompleteRequest{
		Token: setupToken, Username: canaryAdmin, Password: canaryAdminPassword,
	}), ok)
	k.cookies(srv, "the admin", admin)
	want("logout", callAs(t, admin, srv, http.MethodPost, "/api/v1/auth/logout", nil), ok)
	want("a login with a wrong password", callAs(t, admin, srv, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: canaryAdmin, Password: "not the canary password"}), http.StatusUnauthorized)
	want("login", callAs(t, admin, srv, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: canaryAdmin, Password: canaryAdminPassword}), ok)
	k.cookies(srv, "the admin", admin)

	// An invite, the member's sign-up, a password reset that the admin hands out, and a password change.
	res := callAs(t, admin, srv, http.MethodPost, "/api/v1/invites", api.CreateInviteRequest{Note: "canary note", MaxUses: 1})
	want("invite", res, ok)
	var invite api.CreateInviteResponse
	if err := json.Unmarshal([]byte(res.body), &invite); err != nil {
		t.Fatal(err)
	}
	inviteToken := fragmentToken(t, invite.URL)
	k.secret("the invite token", inviteToken)
	want("invite/check", callAs(t, member, srv, http.MethodPost, "/api/v1/auth/invite/check", api.TokenRequest{Token: inviteToken}), ok)
	const memberPassword, memberReset = "canary member passphrase", "canary member second passphrase"
	k.secret("the member's password", memberPassword)
	k.secret("the member's new password", memberReset)
	want("register", callAs(t, member, srv, http.MethodPost, "/api/v1/auth/register", api.RegisterRequest{
		InviteToken: inviteToken, Username: canaryMember, Password: memberPassword,
	}), ok)
	k.cookies(srv, "the member", member)
	reset, err := sock.ResetLink(ctx, canaryMember)
	if err != nil {
		t.Fatal(err)
	}
	resetToken := fragmentToken(t, reset.URL)
	k.secret("the reset token", resetToken)
	want("reset/complete", callAs(t, member, srv, http.MethodPost, "/api/v1/auth/reset/complete",
		api.ResetCompleteRequest{Token: resetToken, Password: memberReset}), ok)
	k.cookies(srv, "the member", member)
	const adminNew = "canary admin passphrase two"
	k.secret("the admin's new password", adminNew)
	want("me/password", callAs(t, admin, srv, http.MethodPost, "/api/v1/me/password",
		api.ChangePasswordRequest{CurrentPassword: canaryAdminPassword, NewPassword: adminNew}), ok)
	k.cookies(srv, "the admin", admin)

	// Web Push: both subscribe, and the admin sends a test notification.
	for name, b := range map[string]*http.Client{"canary-admin-laptop": admin, "canary-member-phone": member} {
		sub := pushes.Subscription(name)
		k.secret("a push endpoint", strings.TrimPrefix(sub.Endpoint, "https://"+servertest.PushHost))
		k.secret("a push p256dh key", sub.Keys.P256dh)
		k.secret("a push auth secret", sub.Keys.Auth)
		want("push subscribe "+name, callAs(t, b, srv, http.MethodPost, "/api/v1/push/subscriptions", sub), ok)
	}
	want("push test", callAs(t, admin, srv, http.MethodPost, "/api/v1/push/test", nil), ok)

	// Signaling and media: the admin shares with a label, the member watches.
	dial := func(b *http.Client) *signaltest.Client {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := signaltest.Dial(dctx, b, srv.WSURL, signaltest.DialOptions{Header: http.Header{"Origin": {srv.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	sharer := dial(admin)
	welcome, _ := joinLounge(t, sharer, signaltest.DefaultHello())
	k.secret("the sharer's resume token", welcome.ResumeToken.Reveal())
	shareID, pub := canaryShare(t, sharer, k)
	watcher := dial(member)
	welcome, _ = joinLounge(t, watcher, signaltest.DefaultHello())
	k.secret("the watcher's resume token", welcome.ResumeToken.Reveal())
	viewer := canaryWatch(t, watcher, shareID, k)
	k.addr("the sharer's ICE address", localAddr(t, "the sharer", senderICE(pub.PC())))
	k.addr("the viewer's ICE address", localAddr(t, "the viewer", receiverICE(viewer.PC())))

	// The connection test.
	probe := newProbeClient(t, false)
	offer, err := probe.Offer()
	if err != nil {
		t.Fatal(err)
	}
	k.sdp("the probe's offer", offer)
	res = callAs(t, member, srv, http.MethodPost, "/api/v1/conntest", api.ConnTestRequest{Transport: api.TransportUDP, Offer: offer})
	want("conntest", res, ok)
	var probed api.ConnTestResponse
	if err := json.Unmarshal([]byte(res.body), &probed); err != nil {
		t.Fatal(err)
	}
	k.sdp("the probe's answer", probed.Answer)
	if err := probe.Accept(probed.Answer); err != nil {
		t.Fatal(err)
	}
	wantConnected(t, "the probe", probe, webrtc.ICEProtocolUDP, srv.UDPPort)
	k.addr("the probe's ICE address", localAddr(t, "the probe", func() *webrtc.ICETransport {
		return probe.PC().SCTP().Transport().ICETransport()
	}))

	// A second connection of the member that goes away, and one that resumes it with its token.
	gone := dial(member)
	hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := gone.Hello(hctx, signaltest.DefaultHello())
	if err != nil {
		t.Fatal(err)
	}
	k.secret("a resume token", first.ResumeToken.Reveal())
	_ = gone.CloseWith(4000) // the web client's close that keeps the grace (05 §10.7)
	back := dial(member)
	hello := signaltest.DefaultHello()
	hello.ResumeToken = first.ResumeToken
	again, err := back.Hello(hctx, hello)
	if err != nil {
		t.Fatal(err)
	}
	k.secret("a resume token", again.ResumeToken.Reveal())

	// The operator's commands, and the member signs out.
	if _, err := sock.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sock.Users(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sock.Doctor(ctx, ops.AdminDoctorRequest{}); err != nil {
		t.Fatal(err)
	}
	// A security event: the member's browser on another site's page opens the WebSocket, and the Origin check
	// refuses it (01 §17). Its line carries the member's IP as remote_ip, and nothing else of the member's.
	dctx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	_, err = signaltest.Dial(dctx, member, srv.WSURL, signaltest.DialOptions{Header: http.Header{"Origin": {"https://evil.example"}}})
	var refused *signaltest.RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("a WebSocket from another site's page: %v, want 403", err)
	}

	want("logout", callAs(t, member, srv, http.MethodPost, "/api/v1/auth/logout", nil), ok)
	waitPushes(t, pushes, 1)
	srv.Stop(t)
	if recs := records(t, srv, "websocket origin refused"); len(recs) != 1 || recs[0]["level"] != "WARN" ||
		recs[0]["remote_ip"] != canaryMemberIP {
		t.Errorf("the refused Origin's line = %v, want one warning with remote_ip %s", recs, canaryMemberIP)
	}

	// The keys of secrets.json.
	data, err := os.ReadFile(srv.Cfg.Paths().Secrets)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Keys  map[string]struct{ Key string } `json:"keys"`
		VAPID struct {
			PrivateKey string `json:"private_key"`
		} `json:"vapid"`
	}
	if err := json.Unmarshal(data, &file); err != nil || len(file.Keys) == 0 || file.VAPID.PrivateKey == "" {
		t.Fatalf("secrets.json: %v", err)
	}
	for name, key := range file.Keys {
		k.secret("the "+name+" key of secrets.json", key.Key)
	}
	k.secret("the VAPID private key", file.VAPID.PrivateKey)

	k.search(t, srv.Logs())
}

// search fails the test for every log line that holds what it must not.
func (c *canary) search(t *testing.T, logs string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) < 50 {
		t.Fatalf("the canary's log has %d lines; the flow did not run", len(lines))
	}
	var found int
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		var why []string
		for value, what := range c.secrets {
			if strings.Contains(line, value) || strings.Contains(line, jsonEscaped(value)) {
				why = append(why, what)
			}
		}
		for _, m := range sdpMarkers {
			if strings.Contains(line, m) {
				why = append(why, "SDP or a candidate ("+m+")")
			}
		}
		if level := rec["level"]; level != "DEBUG" {
			// A security event at warn carries the client's IP as remote_ip, and nowhere else.
			rest := line
			if msg, _ := rec["msg"].(string); level == "WARN" && securityEvents[msg] {
				delete(rec, "remote_ip")
				b, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				rest = string(b)
			}
			for value, what := range c.clientAddr {
				if strings.Contains(rest, value) {
					why = append(why, what+" at "+fmt.Sprint(level))
				}
			}
		}
		if len(why) > 0 {
			found++
			slices.Sort(why)
			t.Errorf("the log holds %s:\n%s", strings.Join(slices.Compact(why), ", "), line)
		}
	}
	t.Logf("searched %d log lines for %d secrets, %d client addresses and %d SDP markers; %d lines hold one",
		len(lines), len(c.secrets), len(c.clientAddr), len(sdpMarkers), found)
}

// jsonEscaped is value as it appears inside a JSON string.
func jsonEscaped(value string) string {
	b, _ := json.Marshal(value)
	return strings.Trim(string(b), `"`)
}
