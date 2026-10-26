package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

const (
	originA = "https://a.example.com"
	originB = "https://b.example.com"
)

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("%T %v is not a bridge error", err, err)
	}
	return be.Code
}

// TestAccessTokenOriginBound is 03 §5.8 / §22.1: a token only for the active server's own origin, no manager token
// call for any other origin, null when not linked, and force reaching AccessTokenWithExpiry.
func TestAccessTokenOriginBound(t *testing.T) {
	var buf bytes.Buffer
	a := newFakeManager(originA, deviceauth.StatusLinked)
	srv := &fakeServers{active: a}
	s := newTestServices(&fakeCore{}, srv, nil, &buf)
	ctx := context.Background()

	tok, err := s.Auth.AccessToken(ctx, originB, false)
	if tok != nil || err != nil || a.tokenCallCount() != 0 {
		t.Fatalf("another origin: %v, %v, %d token calls", tok, err, a.tokenCallCount())
	}
	if !strings.Contains(buf.String(), "token request for a server that isn't active") ||
		strings.Contains(buf.String(), originB) || strings.Contains(buf.String(), originA) {
		t.Fatalf("the debug line names an origin or is missing: %s", buf.String())
	}
	if tok, err := s.Auth.AccessToken(ctx, "", false); tok != nil || err != nil || a.tokenCallCount() != 0 {
		t.Fatalf("empty origin: %v %v", tok, err)
	}

	tok, err = s.Auth.AccessToken(ctx, originA, true)
	if err != nil || tok == nil || tok.Token.Reveal() != "isa_"+originA || tok.ExpiresAt != 1762110845123 {
		t.Fatalf("active origin: %+v, %v", tok, err)
	}
	if a.tokenCalls[0] != true {
		t.Fatal("force didn't reach AccessTokenWithExpiry")
	}
	b, _ := json.Marshal(tok)
	if string(b) != `{"token":"isa_https://a.example.com","expiresAt":1762110845123}` {
		t.Fatalf("JSON %s", b)
	}

	a.tokenErr = deviceauth.ErrNotLinked
	if tok, err := s.Auth.AccessToken(ctx, originA, false); tok != nil || err != nil {
		t.Fatalf("not linked: %v %v", tok, err)
	}
	a.tokenErr = deviceauth.ErrOffline
	if _, err := s.Auth.AccessToken(ctx, originA, false); codeOf(t, err) != codes.CodeServerUnreachable {
		t.Fatalf("offline: %v", err)
	}
	a.tokenErr = deviceauth.ErrIncompatible
	a.state.Compat = compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonProtocolTooNew}
	_, err = s.Auth.AccessToken(ctx, originA, false)
	if codeOf(t, err) != codes.CodeIncompatibleServer ||
		!strings.Contains(err.Error(), `"verdict":"update_app"`) || !strings.Contains(err.Error(), "protocol_too_new") {
		t.Fatalf("incompatible: %v", err)
	}

	srv.setActive(nil)
	if tok, err := s.Auth.AccessToken(ctx, originA, false); tok != nil || err != nil {
		t.Fatalf("no active server: %v %v", tok, err)
	}
}

// TestAccessTokenRacingASwitch: while the active server changes from A to B, a call for A returns either null or
// A's own token, never B's (03 §22.1).
func TestAccessTokenRacingASwitch(t *testing.T) {
	var buf bytes.Buffer
	a := newFakeManager(originA, deviceauth.StatusLinked)
	b := newFakeManager(originB, deviceauth.StatusLinked)
	srv := &fakeServers{active: a}
	s := newTestServices(&fakeCore{}, srv, nil, &buf)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				srv.setActive(b)
			} else {
				srv.setActive(a)
			}
		}
	})
	for range 2000 {
		tok, err := s.Auth.AccessToken(context.Background(), originA, false)
		if err != nil {
			t.Fatal(err)
		}
		if tok != nil && tok.Token.Reveal() != "isa_"+originA {
			t.Fatalf("a call for A got %q", tok.Token.Reveal())
		}
	}
	close(stop)
	wg.Wait()
	if b.tokenCallCount() != 0 {
		t.Fatalf("B's manager was asked for a token %d times by calls for A", b.tokenCallCount())
	}
}

// TestSessionCallsOriginBound: SessionEnded and HelloRefused for an origin that isn't active reject with not_found
// and make no manager call; a code outside their lists is invalid_argument, also with no call.
func TestSessionCallsOriginBound(t *testing.T) {
	var buf bytes.Buffer
	a := newFakeManager(originA, deviceauth.StatusLinked)
	s := newTestServices(&fakeCore{}, &fakeServers{active: a}, nil, &buf)
	ctx := context.Background()

	if _, err := s.Auth.SessionEnded(ctx, originB, "session_revoked"); codeOf(t, err) != codes.CodeNotFound {
		t.Fatalf("SessionEnded, other origin: %v", err)
	}
	if _, err := s.Auth.SessionEnded(ctx, originA, "replaced"); codeOf(t, err) != codes.CodeInvalidArgument {
		t.Fatalf("SessionEnded, bad code: %v", err)
	}
	if _, err := s.Auth.HelloRefused(originB, HelloRefusal{Code: "client_outdated"}); codeOf(t, err) !=
		codes.CodeNotFound {
		t.Fatalf("HelloRefused, other origin: %v", err)
	}
	if _, err := s.Auth.HelloRefused(originA, HelloRefusal{Code: "unauthenticated"}); codeOf(t, err) !=
		codes.CodeInvalidArgument {
		t.Fatalf("HelloRefused, bad code: %v", err)
	}
	if len(a.sessions) != 0 || len(a.hellos) != 0 {
		t.Fatalf("manager calls: %v %v", a.sessions, a.hellos)
	}

	for _, code := range []string{"unauthenticated", "session_revoked", "account_disabled"} {
		st, err := s.Auth.SessionEnded(ctx, originA, code)
		if err != nil || st.Status != "linked" || st.Origin != originA {
			t.Fatalf("SessionEnded %s: %+v %v", code, st, err)
		}
	}
	if len(a.sessions) != 3 || a.sessions[1] != protocol.ErrorCodeSessionRevoked {
		t.Fatalf("sessions %v", a.sessions)
	}
	params := map[string]any{"serverMax": 1.0}
	if _, err := s.Auth.HelloRefused(originA, HelloRefusal{Code: "protocol_unsupported", Params: params}); err != nil {
		t.Fatal(err)
	}
	if len(a.hellos) != 1 || a.hellos[0].Code != protocol.ErrorCodeProtocolUnsupported ||
		a.hellos[0].Params["serverMax"] != 1.0 {
		t.Fatalf("hellos %+v", a.hellos)
	}
}

// TestAuthStateJSON: the state's JSON is 02 §11.7's (expiresAt in ms), and a server never checked shows verdict ok.
func TestAuthStateJSON(t *testing.T) {
	var buf bytes.Buffer
	m := newFakeManager("https://watch.example.com", deviceauth.StatusLinking)
	m.state.ServerName = "Alex's server"
	m.state.Link = &deviceauth.LinkState{UserCode: "WDJB-MJHT", VerificationURI: "https://watch.example.com/link",
		VerificationURIComplete: "https://watch.example.com/link?code=WDJB-MJHT",
		ExpiresAt:               time.UnixMilli(1762110845123)}
	m.state.Compat = compat.Check{Verdict: compat.OK, ServerVersion: "0.2.0"}
	s := newTestServices(&fakeCore{}, &fakeServers{active: m}, nil, &buf)
	b, _ := json.Marshal(s.Auth.State())
	want := `{"origin":"https://watch.example.com","serverName":"Alex's server","status":"linking",` +
		`"link":{"userCode":"WDJB-MJHT","verificationUri":"https://watch.example.com/link",` +
		`"verificationUriComplete":"https://watch.example.com/link?code=WDJB-MJHT","expiresAt":1762110845123},` +
		`"user":null,"device":null,"notice":"",` +
		`"compat":{"verdict":"ok","reason":"","serverVersion":"0.2.0","minClientVersion":""}}`
	if string(b) != want {
		t.Fatalf("JSON\n%s\nwant\n%s", b, want)
	}

	m.state = deviceauth.State{Origin: originA, Status: deviceauth.StatusLinked,
		User: &api.User{ID: "u_1", Username: "alex", Role: api.RoleUser}, Device: &api.DeviceRef{ID: "d_1",
			Name: "PC"}}
	st := s.Auth.State()
	if st.Compat.Verdict != "ok" || st.User == nil || st.User.Role != "user" || st.Device.Name != "PC" {
		t.Fatalf("linked state %+v", st)
	}

	none := newTestServices(&fakeCore{}, &fakeServers{}, nil, &buf).Auth.State()
	if none.Status != "unlinked" || none.Origin != "" || none.Compat.Verdict != "ok" {
		t.Fatalf("no server: %+v", none)
	}
}

// TestServers lists servers.json with the derived id, the active mark and linked.
func TestServers(t *testing.T) {
	var buf bytes.Buffer
	added := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	srv := &fakeServers{
		entries: []deviceauth.ServerEntry{
			{Origin: originA, Name: "A", AddedAt: added, User: &api.User{ID: "u", Username: "alex", Role: api.RoleAdmin}},
			{Origin: originB, Name: "B"},
		},
		active: newFakeManager(originB, deviceauth.StatusUnlinked),
	}
	list := newTestServices(&fakeCore{}, srv, nil, &buf).Auth.Servers()
	if len(list) != 2 {
		t.Fatalf("%d servers", len(list))
	}
	if list[0].ID != servers.LocalID(originA) || list[0].Active || !list[0].Linked ||
		list[0].AddedAt != "2026-10-21T12:00:00Z" || list[0].User.Role != "admin" {
		t.Fatalf("A: %+v", list[0])
	}
	if !list[1].Active || list[1].Linked || list[1].User != nil || list[1].AddedAt != "" {
		t.Fatalf("B: %+v", list[1])
	}
	if got := newTestServices(&fakeCore{}, &fakeServers{}, nil, &buf).Auth.Servers(); got == nil || len(got) != 0 {
		t.Fatalf("no servers: %#v (want an empty list, not null)", got)
	}
}

// TestProbe maps the probe's codes and bounds its time; it never adds the server.
func TestProbe(t *testing.T) {
	var buf bytes.Buffer
	core := &fakeCore{}
	check := compat.Check{Verdict: compat.UpdateServer, Reason: compat.ReasonNoDeviceFlow, ServerVersion: "0.1.4"}
	var deadline time.Time
	s, err := New(Deps{App: core, Servers: &fakeServers{}, Log: testLog(&buf),
		Probe: func(ctx context.Context, address string) (servers.ServerProbe, error) {
			deadline, _ = ctx.Deadline()
			switch address {
			case "old.example.com":
				return servers.ServerProbe{}, &servers.Error{Code: servers.CodeIncompatibleServer, Compat: &check}
			case "down.example.com":
				return servers.ServerProbe{}, &servers.Error{Code: servers.CodeServerUnreachable}
			}
			return servers.ServerProbe{Origin: "https://" + address, Name: "Friends", Version: "0.2.0"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Auth.Probe(context.Background(), "watch.example.com")
	if err != nil || p.Origin != "https://watch.example.com" || time.Until(deadline) > probeTimeout {
		t.Fatalf("probe %+v %v (deadline in %v)", p, err, time.Until(deadline))
	}
	_, err = s.Auth.Probe(context.Background(), "old.example.com")
	if codeOf(t, err) != codes.CodeIncompatibleServer ||
		err.Error() != `{"code":"incompatible_server","params":{"minClientVersion":"","reason":"no_device_flow",`+
			`"serverVersion":"0.1.4","verdict":"update_server"}}` {
		t.Fatalf("incompatible: %v", err)
	}
	if _, err := s.Auth.Probe(context.Background(), "down.example.com"); codeOf(t, err) != codes.CodeServerUnreachable {
		t.Fatalf("down: %v", err)
	}
	if _, err := s.Auth.Probe(context.Background(), ""); codeOf(t, err) != codes.CodeInvalidAddress {
		t.Fatalf("empty: %v", err)
	}
}

// TestLinking: Link, CancelLink, LinkWithPassword and Recheck on the active server, their errors, and not_found
// without one. The password never reaches an error or the log.
func TestLinking(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	m := newFakeManager(originA, deviceauth.StatusUnlinked)
	srv := &fakeServers{active: m}
	s := newTestServices(&fakeCore{}, srv, nil, &buf)

	if err := s.Auth.Link(ctx); err != nil || m.linkCalls != 1 {
		t.Fatalf("Link: %v (%d calls)", err, m.linkCalls)
	}
	m.linkErr = &api.Error{Code: "rate_limited", RetryAfter: 5}
	if err := s.Auth.Link(ctx); codeOf(t, err) != "rate_limited" || !strings.Contains(err.Error(), `"retryAfter":5`) {
		t.Fatalf("Link rate limited: %v", err)
	}
	m.linkErr = deviceauth.ErrNetwork
	if err := s.Auth.Link(ctx); codeOf(t, err) != codes.CodeServerUnreachable {
		t.Fatalf("Link offline: %v", err)
	}
	s.Auth.CancelLink()
	if m.cancels != 1 {
		t.Fatal("CancelLink didn't reach the manager")
	}

	secret := protocol.Secret("correct horse battery staple")
	m.pwErr = &api.Error{Code: "invalid_credentials"}
	_, err := s.Auth.LinkWithPassword(ctx, "alex", secret)
	if codeOf(t, err) != "invalid_credentials" || strings.Contains(err.Error(), "horse") {
		t.Fatalf("wrong password: %v", err)
	}
	m.pwErr = nil
	st, err := s.Auth.LinkWithPassword(ctx, "alex", secret)
	if err != nil || st.Status != "linked" {
		t.Fatalf("right password: %+v %v", st, err)
	}
	if _, err := s.Auth.LinkWithPassword(ctx, strings.Repeat("a", 300), secret); codeOf(t, err) !=
		codes.CodeInvalidArgument {
		t.Fatalf("long username: %v", err)
	}
	if _, err := s.Auth.LinkWithPassword(ctx, "alex", "a\x00b"); codeOf(t, err) != codes.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `"field":"password"`) {
		t.Fatalf("NUL in the password: %v", err)
	}
	if strings.Contains(buf.String(), "horse") {
		t.Fatalf("the password reached the log: %s", buf.String())
	}

	m.recheckErr = deviceauth.ErrIncompatible
	m.state.Compat = compat.Check{Verdict: compat.UpdateServer}
	if _, err := s.Auth.Recheck(ctx); codeOf(t, err) != codes.CodeIncompatibleServer {
		t.Fatalf("Recheck: %v", err)
	}

	srv.setActive(nil)
	if err := s.Auth.Link(ctx); codeOf(t, err) != codes.CodeNotFound {
		t.Fatalf("Link without a server: %v", err)
	}
	if _, err := s.Auth.LinkWithPassword(ctx, "alex", secret); codeOf(t, err) != codes.CodeNotFound {
		t.Fatalf("LinkWithPassword without a server: %v", err)
	}
	if _, err := s.Auth.Recheck(ctx); codeOf(t, err) != codes.CodeNotFound {
		t.Fatalf("Recheck without a server: %v", err)
	}
	s.Auth.CancelLink() // nothing to cancel: no panic
}

// TestLinkHonorsItsContext is W11 from the bridge's side: Link's context is the caller's (Wails cancels it when the
// page cancels the call), bounded by Link's own 15 s.
func TestLinkHonorsItsContext(t *testing.T) {
	var buf bytes.Buffer
	m := newFakeManager(originA, deviceauth.StatusUnlinked)
	m.linkBlock = true
	m.linkCtxErr = make(chan error, 1)
	s := newTestServices(&fakeCore{}, &fakeServers{active: m}, nil, &buf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Auth.Link(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(<-m.linkCtxErr, context.Canceled) || codeOf(t, err) != codes.CodeInternal {
			t.Fatalf("cancelled Link: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Link didn't return after its context was cancelled")
	}
}
