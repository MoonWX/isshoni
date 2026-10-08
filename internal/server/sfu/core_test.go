package sfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// The core slice's unit tests (README S29): the object model, the Conn actor and the API surface of 02 §6, without
// media. The tests that negotiate real PeerConnections through sfutest.Harness are in negotiate_test.go.

// recEvents is a RoomEvents that records every call.
type recEvents struct {
	mu      sync.Mutex
	ended   []endedEvent
	updated []ShareInfo
	policy  []ProfileKey
}

type endedEvent struct {
	room   RoomID
	share  ShareInfo
	reason EndReason
}

func (e *recEvents) ShareUpdated(_ RoomID, s ShareInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.updated = append(e.updated, s)
}

func (e *recEvents) ShareEnded(room RoomID, s ShareInfo, r EndReason) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = append(e.ended, endedEvent{room, s, r})
}

func (e *recEvents) CodecPolicyChanged(_ RoomID, p ProfileKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policy = append(e.policy, p)
}

// endedReasons returns "share:reason" for every ShareEnded so far, in order.
func (e *recEvents) endedReasons() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.ended))
	for _, ev := range e.ended {
		out = append(out, string(ev.share.ID)+":"+string(ev.reason))
	}
	return out
}

// recSignaler is a Signaler that records every call.
type recSignaler struct {
	mu     sync.Mutex
	offers []subOffer
	gens   []uint32
	events []Event
}

func (r *recSignaler) SendOffer(_ PCKind, gen, neg uint32, sdp string, tracks []TrackBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offers = append(r.offers, subOffer{neg: neg, sdp: sdp, tracks: slices.Clone(tracks)})
	r.gens = append(r.gens, gen)
}

func (r *recSignaler) SendEvent(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recSignaler) sent() []subOffer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.offers)
}

// pcStates returns the PCStateEvents the Signaler got, in order, as "sub/1:connected".
func (r *recSignaler) pcStates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		if st, ok := ev.(PCStateEvent); ok {
			out = append(out, fmt.Sprintf("%s/%d:%s", st.PC, st.Gen, st.State))
		}
	}
	return out
}

// errorEvents returns the ErrorEvents the Signaler got, in order.
func (r *recSignaler) errorEvents() []ErrorEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ErrorEvent
	for _, ev := range r.events {
		if e, ok := ev.(ErrorEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

// waitPCStates waits until the Signaler has got exactly these PCStateEvents (see pcStates).
func (r *recSignaler) waitPCStates(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(r.pcStates(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("PC states = %v, want %v", r.pcStates(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitOffers waits until the Signaler has got n offers and returns them.
func (r *recSignaler) waitOffers(t *testing.T, n int) []subOffer {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if offers := r.sent(); len(offers) >= n {
			return offers
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sub offers after 10 s, want %d", len(r.sent()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newTestSFU builds an SFU on a loopback Transport (UDP and TCP 7882) and closes it when the test ends, before the
// Transport.
func newTestSFU(t *testing.T) (*SFU, *recEvents) {
	t.Helper()
	tr := loopbackTransport(t, true)
	ev := &recEvents{}
	s, err := New(Config{Transport: tr}, Deps{Events: ev, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("SFU.Close: %v", err)
		}
	})
	return s, ev
}

// join joins a Conn; the participant id is the user id, as 01's sfuplane passes it.
func join(t *testing.T, s *SFU, room RoomID, user UserID, conn ConnID, role Role) (*Conn, *recSignaler) {
	t.Helper()
	sig := &recSignaler{}
	c, err := s.Join(JoinParams{
		Room: room, Participant: ParticipantID(user), User: user, Conn: conn, Role: role, Client: ClientWeb,
		Decode: DecodeCaps{H264: []ProfileKey{ProfileHigh, ProfileConstrainedBaseline}}, Signaler: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sig
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// wantErr fails unless err is an *Error with the code.
func wantErr(t *testing.T, err error, code string) {
	t.Helper()
	_ = errOf(t, err, code)
}

// errOf fails unless err is an *Error with the code, and returns it.
func errOf(t *testing.T, err error, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	if e.Retryable != retryableCode(code) && !errors.Is(err, ErrNotImplemented) && code != CodeInternal {
		t.Errorf("%s: Retryable = %v, want the flag of 02 §6.3", code, e.Retryable)
	}
	return e
}

func waitDone(t *testing.T, c *Conn) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("Conn %s did not finish closing", c.ID())
	}
}

func startShare(t *testing.T, c *Conn, id ShareID) ShareParams {
	t.Helper()
	p, err := c.StartShare(testCtx(t), StartShareParams{ID: id, Preset: PresetAuto, Audio: true, Source: SourceScreen})
	if err != nil {
		t.Fatalf("StartShare(%s): %v", id, err)
	}
	return p
}

func TestNew(t *testing.T) {
	if _, err := New(Config{}, Deps{}); err == nil {
		t.Error("New without a Transport succeeded")
	}
	tr := loopbackTransport(t, false)
	if _, err := New(Config{Transport: tr, Limits: Limits{MaxShareKbps: -1}}, Deps{}); err == nil {
		t.Error("New with a negative limit succeeded")
	}
	s, err := New(Config{Transport: tr}, Deps{}) // no Events, no Logger
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); err != nil {
		t.Errorf("Ready = %v, want nil once New returned", err)
	}
	c, _ := join(t, s, "lounge", "u1", "c1", RoleFull)
	startShare(t, c, "s_1") // ends without a RoomEvents: nothing to call

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if d := time.Since(start); d > closeTimeout {
		t.Errorf("Close took %v, want at most %v", d, closeTimeout)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	wantErr(t, s.Ready(), CodeClosed)
	_, err = s.Join(JoinParams{
		Room: "lounge", Participant: "u2", User: "u2", Conn: "c2", Role: RoleViewer, Client: ClientWeb,
		Signaler: &recSignaler{},
	})
	wantErr(t, err, CodeClosed)
	waitDone(t, c)
	_, err = c.StartShare(testCtx(t), StartShareParams{ID: "s_2"})
	wantErr(t, err, CodeClosed)
}

func TestJoinValidation(t *testing.T) {
	s, _ := newTestSFU(t)
	good := JoinParams{
		Room: "lounge", Participant: "u1", User: "u1", Conn: "c1", Role: RoleFull, Client: ClientWeb,
		Signaler: &recSignaler{},
	}
	for name, change := range map[string]func(*JoinParams){
		"no room":        func(p *JoinParams) { p.Room = "" },
		"no participant": func(p *JoinParams) { p.Participant = "" },
		"no user":        func(p *JoinParams) { p.User = "" },
		"no conn":        func(p *JoinParams) { p.Conn = "" },
		"no role":        func(p *JoinParams) { p.Role = 0 },
		"unknown role":   func(p *JoinParams) { p.Role = RoleAgent + 1 },
		"no client":      func(p *JoinParams) { p.Client = 0 },
		"no signaler":    func(p *JoinParams) { p.Signaler = nil },
	} {
		p := good
		change(&p)
		if _, err := s.Join(p); err == nil {
			t.Errorf("%s: Join succeeded", name)
		} else if e := errOf(t, err, CodeInternal); e.Retryable {
			t.Errorf("%s: a caller bug is retryable", name)
		}
	}
	c, err := s.Join(good)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID() != "c1" {
		t.Errorf("ID = %q", c.ID())
	}
	if _, err := s.Join(good); err == nil {
		t.Error("joining the same connection id twice succeeded")
	}
	other := good
	other.Conn, other.User = "c2", "u2" // participant u1 again, but another user
	if _, err := s.Join(other); err == nil {
		t.Error("a participant with two users was accepted")
	}

	// A closed Conn's id is free at once: the hub rejoins a connection right after it left a room.
	c.Close(EndReasonLeft)
	c2, err := s.Join(good)
	if err != nil {
		t.Fatalf("rejoin after Close: %v", err)
	}
	if c2 == c {
		t.Error("rejoin returned the closed Conn")
	}
	waitDone(t, c)
}

// TestRooms: rooms and participants come and go with their Conns, and Snapshot and Metrics follow.
func TestRooms(t *testing.T) {
	s, _ := newTestSFU(t)
	a1, _ := join(t, s, "lounge", "alice", "c-a1", RoleFull)
	a2, _ := join(t, s, "lounge", "alice", "c-a2", RoleViewer) // the same participant on a second connection
	b, _ := join(t, s, "den", "bob", "c-b", RolePublisher)
	startShare(t, a1, "s_a")
	startShare(t, b, "s_b")

	snap := s.Snapshot()
	if len(snap.Rooms) != 2 || snap.Rooms[0].Room != "den" || snap.Rooms[1].Room != "lounge" {
		t.Fatalf("Snapshot rooms = %+v, want den and lounge", snap.Rooms)
	}
	lounge := snap.Rooms[1]
	if len(lounge.Conns) != 2 || lounge.Conns[0].Conn != "c-a1" || lounge.Conns[1].Conn != "c-a2" ||
		lounge.Conns[0].User != "alice" {
		t.Errorf("lounge Conns = %+v", lounge.Conns)
	}
	if len(lounge.Shares) != 1 || lounge.Shares[0].Info.ID != "s_a" {
		t.Errorf("lounge Shares = %+v", lounge.Shares)
	}
	if snap.At.IsZero() || snap.Totals.PCsByState == nil || snap.Totals.PCsByTransport == nil {
		t.Errorf("Snapshot = %+v, want a time and non-nil total maps", snap)
	}
	m := s.Metrics()
	if m.Conns["full"] != 1 || m.Conns["viewer"] != 1 || m.Conns["publisher"] != 1 || m.Conns["agent"] != 0 {
		t.Errorf("Metrics.Conns = %v", m.Conns)
	}
	if m.Shares["pending"] != 2 || m.Shares["live"] != 0 {
		t.Errorf("Metrics.Shares = %v", m.Shares)
	}

	a1.Close(EndReasonLeft)
	if got := s.Shares("lounge"); len(got) != 0 {
		t.Errorf("shares after their Conn closed: %+v", got)
	}
	if n := len(s.Snapshot().Rooms); n != 2 {
		t.Errorf("%d rooms while alice's second connection is in the lounge, want 2", n)
	}
	a2.Close(EndReasonLeft)
	if rooms := s.Snapshot().Rooms; len(rooms) != 1 || rooms[0].Room != "den" {
		t.Errorf("rooms after the lounge emptied: %+v", rooms)
	}
	b.Close(EndReasonLeft)
	if n := len(s.Snapshot().Rooms); n != 0 {
		t.Errorf("%d rooms left", n)
	}
	if m := s.Metrics(); m.Conns["full"]+m.Conns["viewer"]+m.Conns["publisher"] != 0 || m.Shares["pending"] != 0 {
		t.Errorf("Metrics after everyone left: conns %v, shares %v", m.Conns, m.Shares)
	}
}

// TestMetricsLabels: every label value of 02 §13 is present, zero or not.
func TestMetricsLabels(t *testing.T) {
	s, _ := newTestSFU(t)
	m := s.Metrics()
	keys := func(m any) []string {
		var out []string
		for _, k := range reflect.ValueOf(m).MapKeys() {
			out = append(out, k.String())
		}
		slices.Sort(out)
		return out
	}
	states := []string{"closed", "connected", "connecting", "disconnected", "failed", "new"}
	for name, tc := range map[string]struct {
		got  any
		want []string
	}{
		"Conns":                {m.Conns, []string{"agent", "full", "publisher", "viewer"}},
		"PeerConnections":      {m.PeerConnections, []string{"pub", "sub"}},
		"PeerConnections[pub]": {m.PeerConnections["pub"], states},
		"PeerConnections[sub]": {m.PeerConnections["sub"], states},
		"SelectedTransport":    {m.SelectedTransport, []string{"tcp443", "tcp7882", "udp"}},
		"Shares":               {m.Shares, []string{"live", "pending", "stalled"}},
		"DownTracks":           {m.DownTracks, []string{"audio", "video"}},
		"DownTracks[video]":    {m.DownTracks["video"], []string{"high", "low", "off"}},
		"DownTracks[audio]":    {m.DownTracks["audio"], []string{"high", "low", "off"}},
		"IngressBytes":         {m.IngressBytes, []string{"audio", "video"}},
		"IngressPackets":       {m.IngressPackets, []string{"audio", "video"}},
		"EgressBytes":          {m.EgressBytes, []string{"audio", "rtx", "video"}},
		"EgressPackets":        {m.EgressPackets, []string{"audio", "rtx", "video"}},
		"Downgrades": {m.Downgrades, []string{
			"bandwidth", "codec_mismatch", "decoder_failed", "decoder_unavailable", "no_layer", "no_preview_layer",
			"server_limit",
		}},
		"QueueDrops":        {m.QueueDrops, []string{"audio", "rtx", "video"}},
		"HandshakeTimeouts": {m.HandshakeTimeouts, []string{"pub", "sub"}},
	} {
		if got := keys(tc.got); !slices.Equal(got, tc.want) {
			t.Errorf("Metrics.%s keys = %v, want %v", name, got, tc.want)
		}
	}
}

// TestShareLifecycle: StartShare makes a pending share that the room lists; the guards and the owner rules of 02
// §6.3 hold; every way a share ends fires ShareEnded once, with the caller's reason.
func TestShareLifecycle(t *testing.T) {
	s, ev := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	other, _ := join(t, s, "lounge", "bob", "c-b", RoleFull)
	viewer, _ := join(t, s, "lounge", "carol", "c-c", RoleViewer)

	_, err := viewer.StartShare(ctx, StartShareParams{ID: "s_v"})
	wantErr(t, err, CodeRoleForbidden)
	_, err = pub.StartShare(ctx, StartShareParams{})
	wantErr(t, err, CodeInternal)
	_, err = pub.StartShare(ctx, StartShareParams{ID: "s_x", Preset: PresetText + 1})
	wantErr(t, err, CodeInternal)

	params := startShare(t, pub, "s_1")
	if params.Profile != ProfileHigh || len(params.Encodings) != 2 {
		t.Errorf("ShareParams = %+v", params)
	}
	info, ok := s.Share("s_1")
	want := ShareInfo{
		ID: "s_1", Room: "lounge", Participant: "alice", User: "alice", Conn: "c-a", Source: SourceScreen,
		Preset: PresetAuto, State: SharePending, StartedAt: info.StartedAt,
	}
	if !ok || !reflect.DeepEqual(info, want) || info.StartedAt.IsZero() {
		t.Errorf("Share(s_1) = %+v, %v; want %+v", info, ok, want)
	}
	if _, err := pub.StartShare(ctx, StartShareParams{ID: "s_1"}); err == nil {
		t.Error("a second share with the same id was accepted")
	}
	for i := 2; i <= maxSharesPerParticipant; i++ {
		startShare(t, pub, ShareID(fmt.Sprintf("s_%d", i)))
	}
	_, err = pub.StartShare(ctx, StartShareParams{ID: "s_5"})
	wantErr(t, err, CodeTooManyShares)
	if _, ok := s.Share("s_5"); ok {
		t.Error("the refused fifth share exists")
	}
	startShare(t, other, "s_o") // the limit is per participant
	shares := s.Shares("lounge")
	var ids []ShareID
	for _, sh := range shares {
		ids = append(ids, sh.ID)
	}
	if !slices.Equal(ids, []ShareID{"s_1", "s_2", "s_3", "s_4", "s_o"}) {
		t.Errorf("Shares(lounge) = %v, want them sorted by StartedAt", ids)
	}
	if got := s.Shares("nowhere"); got != nil {
		t.Errorf("Shares of an unknown room = %v", got)
	}
	if st := pub.Stats(); len(st.Shares) != 4 || st.Shares[0].Share != "s_1" || st.Shares[0].State != SharePending {
		t.Errorf("Stats().Shares = %+v, want the Conn's own four", st.Shares)
	}

	// Ownership: only the publishing Conn stops a share through Conn.StopShare.
	wantErr(t, other.StopShare(ctx, "s_1", EndReasonStopped), CodeNotOwner)
	wantErr(t, pub.StopShare(ctx, "s_nope", EndReasonStopped), CodeShareNotFound)
	_, err = other.UpdateShare(ctx, "s_1", ShareUpdate{})
	wantErr(t, err, CodeNotOwner)
	_, err = pub.UpdateShare(ctx, "s_nope", ShareUpdate{})
	wantErr(t, err, CodeShareNotFound)

	if err := pub.StopShare(ctx, "s_1", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	wantErr(t, pub.StopShare(ctx, "s_1", EndReasonStopped), CodeShareNotFound)
	if _, ok := s.Share("s_1"); ok {
		t.Error("the stopped share is still listed")
	}
	startShare(t, pub, "s_5") // a slot is free again
	if err := s.StopShare("s_2", EndReasonMediaTimeout); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.StopShare("s_2", EndReasonMediaTimeout), CodeShareNotFound)

	pub.Close(EndReasonDisconnected) // ends s_3, s_4 and s_5, in StartedAt order
	s.CloseRoom("lounge", EndReasonRoomClosed)
	s.CloseRoom("lounge", EndReasonRoomClosed) // gone already: nothing happens
	wantReasons := []string{
		"s_1:stopped", "s_2:media_timeout", "s_3:disconnected", "s_4:disconnected", "s_5:disconnected",
		"s_o:room_closed",
	}
	if got := ev.endedReasons(); !slices.Equal(got, wantReasons) {
		t.Errorf("ShareEnded calls = %v, want %v", got, wantReasons)
	}
	if ev.ended[0].room != "lounge" || ev.ended[0].share.Conn != "c-a" || ev.ended[0].share.State != SharePending {
		t.Errorf("ShareEnded info = %+v", ev.ended[0])
	}
	for _, c := range []*Conn{pub, other, viewer} {
		waitDone(t, c) // CloseRoom closed the two that were left
	}
	if len(ev.updated) != 0 {
		t.Errorf("%d ShareUpdated calls for shares that never went live", len(ev.updated))
	}
}

// TestCloseEndsShares: SFU.Close ends every share with server_shutdown and closes every Conn.
func TestCloseEndsShares(t *testing.T) {
	s, ev := newTestSFU(t)
	a, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	b, _ := join(t, s, "den", "bob", "c-b", RoleFull)
	startShare(t, a, "s_a")
	startShare(t, b, "s_b")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ev.endedReasons(); !slices.Equal(got, []string{"s_a:server_shutdown", "s_b:server_shutdown"}) {
		t.Errorf("ShareEnded calls = %v", got)
	}
	for _, c := range []*Conn{a, b} {
		select {
		case <-c.Done():
		default:
			t.Errorf("Conn %s is not done after SFU.Close", c.ID())
		}
	}
	wantErr(t, s.StopShare("s_a", EndReasonStopped), CodeShareNotFound)
}

// syncBuffer is a log sink that several goroutines write to.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// TestCloseAbandonsSlowConns: SFU.Close returns within about 1 s even when a Conn can't close in time, says so in a
// warning, and the Conn still finishes on its own. The logs carry component=sfu and the ids, never more.
func TestCloseAbandonsSlowConns(t *testing.T) {
	tr := loopbackTransport(t, false)
	logs := &syncBuffer{}
	s, err := New(Config{Transport: tr}, Deps{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	quick, _ := join(t, s, "lounge", "alice", "c-quick", RoleFull)
	slow, _ := join(t, s, "lounge", "bob", "c-slow", RoleFull)
	startShare(t, slow, "s_1")
	entered, release := make(chan struct{}), make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- slow.do(context.Background(), func(context.Context) error {
			close(entered)
			<-release // the actor is stuck, as it would be inside a Pion call that hangs
			return nil
		})
	}()
	<-entered

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if d := time.Since(start); d < closeTimeout || d > closeTimeout+time.Second {
		t.Errorf("Close took %v with a stuck Conn, want about %v", d, closeTimeout)
	}
	waitDone(t, quick)
	select {
	case <-slow.Done():
		t.Error("the stuck Conn is done")
	default:
	}
	if _, ok := s.Share("s_1"); ok {
		t.Error("the stuck Conn's share outlived SFU.Close")
	}
	close(release)
	if err := <-blocked; err != nil {
		t.Errorf("the command that was running returned %v", err)
	}
	waitDone(t, slow)

	out := logs.String()
	for _, want := range []string{
		"level=WARN", "connections were still closing", "count=1", "component=sfu",
		`msg="connection joined"`, "conn_id=c-slow", "room_id=lounge", "user_id=bob", "role=full",
		`msg="share started"`, `msg="share ended"`, "share_id=s_1", "reason=server_shutdown",
		`msg="connection left"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the logs lack %q:\n%s", want, out)
		}
	}
}

// TestShareParams is the table of 02 §8.6.
func TestShareParams(t *testing.T) {
	full := EncodingParams{RID: "f", Active: true, MaxBitrate: 8_000_000, MaxFramerate: 60, MaxPixels: 2_073_600}
	preview := EncodingParams{RID: "q", Active: true, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 230_400}
	with := func(e EncodingParams, change func(*EncodingParams)) EncodingParams {
		change(&e)
		return e
	}
	for _, tc := range []struct {
		name   string
		preset Preset
		limits Limits
		policy ProfileKey
		want   ShareParams
	}{
		{"auto", PresetAuto, Limits{}, ProfileHigh, ShareParams{ProfileHigh, []EncodingParams{full, preview}, 128_000}},
		{"game", PresetGame, Limits{}, ProfileHigh, ShareParams{ProfileHigh, []EncodingParams{full, preview}, 128_000}},
		{"movie", PresetMovie, Limits{}, ProfileHigh, ShareParams{ProfileHigh, []EncodingParams{full, preview}, 256_000}},
		{"text", PresetText, Limits{}, ProfileHigh, ShareParams{
			ProfileHigh, []EncodingParams{with(full, func(e *EncodingParams) { e.MaxFramerate = 30 }), preview}, 128_000,
		}},
		{"cb policy", PresetAuto, Limits{}, ProfileConstrainedBaseline, ShareParams{
			ProfileConstrainedBaseline, []EncodingParams{full, preview}, 128_000,
		}},
		{"admin cap", PresetAuto, Limits{MaxShareKbps: 2500}, ProfileHigh, ShareParams{
			ProfileHigh, []EncodingParams{with(full, func(e *EncodingParams) { e.MaxBitrate = 2_500_000 }), preview}, 128_000,
		}},
		{"admin cap above the preset", PresetAuto, Limits{MaxShareKbps: 20_000}, ProfileHigh, ShareParams{
			ProfileHigh, []EncodingParams{full, preview}, 128_000,
		}},
		{"admin cap below the preview", PresetAuto, Limits{MaxShareKbps: 100}, ProfileHigh, ShareParams{
			ProfileHigh, []EncodingParams{with(full, func(e *EncodingParams) { e.MaxBitrate = 100_000 }), preview}, 128_000,
		}},
	} {
		if got := shareParams(tc.preset, tc.limits, tc.policy); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: shareParams = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestUpdateShareAndLimits: a preset change and the admin cap reach ShareParams and ShareInfo at once.
func TestUpdateShareAndLimits(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	if p := startShare(t, c, "s_1"); p.AudioBitrate != 128_000 || p.Encodings[0].MaxBitrate != 8_000_000 {
		t.Fatalf("ShareParams = %+v", p)
	}
	movie := PresetMovie
	p, err := c.UpdateShare(ctx, "s_1", ShareUpdate{Preset: &movie})
	if err != nil {
		t.Fatal(err)
	}
	if p.AudioBitrate != 256_000 {
		t.Errorf("AudioBitrate after the Movie preset = %d", p.AudioBitrate)
	}
	if info, _ := s.Share("s_1"); info.Preset != PresetMovie {
		t.Errorf("ShareInfo.Preset = %v", info.Preset)
	}
	bad := PresetText + 1
	_, err = c.UpdateShare(ctx, "s_1", ShareUpdate{Preset: &bad})
	wantErr(t, err, CodeInternal)

	s.SetLimits(Limits{MaxShareKbps: 3000})
	s.SetLimits(Limits{MaxShareKbps: -5}) // ignored
	p, err = c.UpdateShare(ctx, "s_1", ShareUpdate{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Encodings[0].MaxBitrate != 3_000_000 || p.Encodings[1].MaxBitrate != 300_000 || p.AudioBitrate != 256_000 {
		t.Errorf("ShareParams under a 3 Mbps cap = %+v", p)
	}
	if got := s.CodecPolicy("lounge"); got != ProfileHigh {
		t.Errorf("CodecPolicy = %q, want the default %q", got, ProfileHigh)
	}
	if got := s.CodecPolicy("nowhere"); got != ProfileHigh {
		t.Errorf("CodecPolicy of an unknown room = %q", got)
	}
}

// TestNotImplemented: the methods that later slices fill in exist and say so (README §4 "Interfaces first").
func TestNotImplemented(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	_, _, probeErr := s.Probe(ctx, "alice", ProbeUDP, "v=0\r\n")
	for name, err := range map[string]error{
		"SFU.Probe":          probeErr,
		"Conn.RestartICE":    c.RestartICE(ctx, PCSub, 1),
		"Conn.ResetPC":       c.ResetPC(ctx, PCSub, 1),
		"Conn.ClosePC":       c.ClosePC(ctx, PCPub, 1),
		"Conn.SetDecodeCaps": c.SetDecodeCaps(ctx, DecodeCaps{H264: []ProfileKey{ProfileHigh}}),
	} {
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s = %v, want an error that wraps ErrNotImplemented", name, err)
			continue
		}
		if e := errOf(t, err, CodeInternal); e.Retryable {
			t.Errorf("%s: a missing method is retryable", name)
		}
	}
	c.Resync() // no error to return; it must not disturb the Conn
	if _, err := c.StartShare(ctx, StartShareParams{ID: "s_1"}); err != nil {
		t.Errorf("StartShare after Resync: %v", err)
	}
}

// TestCommandQueue: signal calls are serialized by the actor; a full queue is sfu.busy (retryable, 1 s), a call whose
// context ends while it is queued never runs, and calls on a closed Conn are sfu.closed.
func TestCommandQueue(t *testing.T) {
	s, _ := newTestSFU(t)
	if s.commandWait != 5*time.Second {
		t.Errorf("commandWait = %v, want the 5 s of 02 §5.4", s.commandWait)
	}
	s.commandWait = 50 * time.Millisecond
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)

	// Block the actor inside a command.
	entered, release := make(chan struct{}), make(chan struct{})
	blockerDone := make(chan error, 1)
	go func() {
		blockerDone <- c.do(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	// A call whose context ends while it waits in the queue is busy and never runs.
	ran := make(chan struct{}, commandQueueLen+2)
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := c.do(short, func(context.Context) error { ran <- struct{}{}; return nil })
	cancel()
	if e := errOf(t, err, CodeBusy); e.RetryAfter != busyRetryAfter {
		t.Errorf("busy RetryAfter = %v, want %v", e.RetryAfter, busyRetryAfter)
	}

	// Fill the queue (the canceled command above still holds one slot), then one more call finds it full.
	var wg sync.WaitGroup
	results := make(chan error, commandQueueLen)
	for range commandQueueLen - 1 {
		wg.Go(func() {
			results <- c.do(context.Background(), func(context.Context) error { ran <- struct{}{}; return nil })
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(c.cmds) < commandQueueLen {
		if time.Now().After(deadline) {
			t.Fatalf("the command queue holds %d of %d", len(c.cmds), commandQueueLen)
		}
		time.Sleep(time.Millisecond)
	}
	full, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = c.do(full, func(context.Context) error { ran <- struct{}{}; return nil })
	cancel()
	wantErr(t, err, CodeBusy)
	// Without a deadline of its own, a call gives up after the SFU's wait (5 s; 50 ms in this test).
	start := time.Now()
	err = c.do(context.Background(), func(context.Context) error { ran <- struct{}{}; return nil })
	wantErr(t, err, CodeBusy)
	if d := time.Since(start); d < s.commandWait || d > s.commandWait+2*time.Second {
		t.Errorf("a call on a full queue waited %v, want about %v", d, s.commandWait)
	}

	close(release)
	if err := <-blockerDone; err != nil {
		t.Errorf("the blocking command returned %v", err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("a queued command returned %v", err)
		}
	}
	if n := len(ran); n != commandQueueLen-1 {
		t.Errorf("%d commands ran, want %d: the two busy ones must not run", n, commandQueueLen-1)
	}

	// Close while a command runs and another waits: the running one finishes, the waiting one is sfu.closed.
	entered, release = make(chan struct{}), make(chan struct{})
	go func() {
		blockerDone <- c.do(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return errors.New("finished")
		})
	}()
	<-entered
	waiting := make(chan error, 1)
	go func() { waiting <- c.do(context.Background(), func(context.Context) error { return nil }) }()
	for len(c.cmds) == 0 {
		time.Sleep(time.Millisecond)
	}
	c.Close(EndReasonLeft)
	c.Close(EndReasonLeft) // idempotent
	close(release)
	if err := <-blockerDone; err == nil || err.Error() != "finished" {
		t.Errorf("the running command returned %v, want its own result", err)
	}
	wantErr(t, <-waiting, CodeClosed)
	waitDone(t, c)
	wantErr(t, c.do(context.Background(), func(context.Context) error { return nil }), CodeClosed)
	wantErr(t, c.StopShare(context.Background(), "s_1", EndReasonStopped), CodeClosed)
	wantErr(t, c.AddICECandidate(context.Background(), PCPub, 1, webrtc.ICECandidateInit{}), CodeClosed)
	_, err = c.HandleOffer(context.Background(), PCPub, 1, 1, "", nil)
	wantErr(t, err, CodeClosed)
	_, err = c.UpdateSubscriptions(context.Background(), nil)
	wantErr(t, err, CodeClosed)
	if c.events.post(func() { t.Error("an event ran on a closed Conn") }) {
		t.Error("a closed Conn took an internal event")
	}
}

// TestEventQueue: internal events never wait, run in order on the actor and interleave with commands.
func TestEventQueue(t *testing.T) {
	s, _ := newTestSFU(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	const n = 5000
	var got []int // written by the actor only
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = c.do(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	for i := range n { // far more than the command queue holds, while the actor is busy
		c.post(func() { got = append(got, i) })
	}
	close(release)
	var seen []int
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < n && time.Now().Before(deadline) {
		if err := c.do(context.Background(), func(context.Context) error {
			seen = slices.Clone(got)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != n || !slices.IsSorted(seen) {
		t.Errorf("%d of %d events ran, in order: %v", len(seen), n, slices.IsSorted(seen))
	}
}

// pubOfferFrom returns a complete offer of a Pion client with one sendonly H.264 track and one Opus track, and its
// tracks binding to share.
func pubOfferFrom(t *testing.T, share ShareID) (*webrtc.PeerConnection, string, []TrackBinding) {
	t.Helper()
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	addSendonly(t, client, h264Track(t, "pub"), opusTrack(t, "a-pub"))
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := completeDescription(t, client, offer)
	var tracks []TrackBinding
	for _, tr := range client.GetTransceivers() {
		tracks = append(tracks, TrackBinding{MID: tr.Mid(), Share: share, Kind: tr.Kind()})
	}
	return client, raw, tracks
}

// pubState returns the Conn's pub gen and whether it has a pub PC.
func pubState(t *testing.T, c *Conn) (gen uint32, exists bool) {
	t.Helper()
	if err := c.do(testCtx(t), func(context.Context) error {
		gen, exists = c.pubGen, c.pub != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return gen, exists
}

// TestHandleOfferRefusals: an offer the SFU refuses changes nothing (02 §8.4 step 1): no PC, no gen.
func TestHandleOfferRefusals(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, c, "s_1")
	_, raw, tracks := pubOfferFrom(t, "s_1")

	_, err := c.HandleOffer(ctx, PCSub, 1, 1, raw, tracks)
	wantErr(t, err, CodeBadPC)
	_, err = c.HandleOffer(ctx, 0, 1, 1, raw, tracks)
	wantErr(t, err, CodeBadPC)
	_, err = viewer.HandleOffer(ctx, PCPub, 1, 1, raw, tracks)
	wantErr(t, err, CodeRoleForbidden)
	_, err = c.HandleOffer(ctx, PCPub, 0, 1, raw, tracks)
	wantErr(t, err, CodeStaleOffer)
	_, err = c.HandleOffer(ctx, PCPub, 1, 0, raw, tracks)
	wantErr(t, err, CodeStaleOffer)
	_, err = c.HandleOffer(ctx, PCPub, 1, 1, "not an SDP", tracks)
	wantErr(t, err, CodeBadSDP)
	_, err = c.HandleOffer(ctx, PCPub, 1, 1, "", tracks)
	wantErr(t, err, CodeBadSDP)
	// A malformed binding: the video m-section bound as audio.
	swapped := slices.Clone(tracks)
	swapped[0].Kind = webrtc.RTPCodecTypeAudio
	_, err = c.HandleOffer(ctx, PCPub, 1, 1, raw, swapped)
	if e := errOf(t, err, CodeUnknownTrack); e.Share != "s_1" {
		t.Errorf("unknown_track Share = %q", e.Share)
	}
	// An offer that passes the SFU's checks but that Pion can't apply: the m-sections lose their ICE credentials.
	desc, err := parseSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(attrs []sdp.Attribute) []sdp.Attribute {
		return slices.DeleteFunc(attrs, func(a sdp.Attribute) bool { return a.Key == "ice-ufrag" || a.Key == "ice-pwd" })
	}
	desc.Attributes = strip(desc.Attributes)
	for _, md := range desc.MediaDescriptions {
		md.Attributes = strip(md.Attributes)
	}
	broken, err := desc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.HandleOffer(ctx, PCPub, 1, 1, string(broken), tracks)
	wantErr(t, err, CodeBadSDP)

	if gen, exists := pubState(t, c); gen != 0 || exists {
		t.Errorf("after refused offers: pub gen %d, PC exists %v; want nothing applied", gen, exists)
	}
	wantErr(t, c.HandleAnswer(ctx, PCPub, 1, 1, raw), CodeBadPC)
	wantErr(t, c.HandleAnswer(ctx, PCSub, 1, 1, raw), CodeBadPC) // no sub PC yet

	// The same offer is fine once nothing is wrong with it.
	if _, err := c.HandleOffer(ctx, PCPub, 1, 1, raw, tracks); err != nil {
		t.Fatalf("the good offer: %v", err)
	}
	if gen, exists := pubState(t, c); gen != 1 || !exists {
		t.Errorf("after the good offer: pub gen %d, PC exists %v", gen, exists)
	}
}

// candInit is a trickled host candidate.
func candInit(addr string, port int) webrtc.ICECandidateInit {
	mid, idx := "0", uint16(0)
	return webrtc.ICECandidateInit{
		Candidate: fmt.Sprintf("candidate:1 1 udp 2130706431 %s %d typ host", addr, port),
		SDPMid:    &mid, SDPMLineIndex: &idx,
	}
}

// candState returns the pub candidate buffer of a Conn.
func candState(t *testing.T, c *Conn, kind PCKind) (st remoteCandidates) {
	t.Helper()
	if err := c.do(testCtx(t), func(context.Context) error {
		st = c.cands[kind]
		st.pending = slices.Clone(st.pending)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestAddICECandidate: trickled candidates are filtered (02 §7.3), buffered until their PC has its remote
// description, capped at 64 per PC and gen, and ignored for an older gen.
func TestAddICECandidate(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	startShare(t, c, "s_1")

	wantErr(t, c.AddICECandidate(ctx, 0, 1, candInit("127.0.0.1", 5000)), CodeBadPC)
	// Dropped by the filter (the loopback Transport keeps loopback, and nothing else private or odd), by its gen, or
	// as the end-of-candidates marker: no error, nothing kept.
	for name, tc := range map[string]struct {
		kind PCKind
		gen  uint32
		cand webrtc.ICECandidateInit
	}{
		"multicast":         {PCPub, 1, candInit("224.0.0.1", 5000)},
		"link-local":        {PCPub, 1, candInit("169.254.1.1", 5000)},
		"private":           {PCPub, 1, candInit("10.0.0.1", 5000)},
		"mDNS name":         {PCPub, 1, candInit("abc.local", 5000)},
		"garbage":           {PCPub, 1, webrtc.ICECandidateInit{Candidate: "candidate:nonsense"}},
		"gen 0":             {PCPub, 0, candInit("127.0.0.1", 5000)},
		"end of candidates": {PCPub, 1, webrtc.ICECandidateInit{}},
		"no sub PC":         {PCSub, 1, candInit("127.0.0.1", 5000)},
	} {
		if err := c.AddICECandidate(ctx, tc.kind, tc.gen, tc.cand); err != nil {
			t.Errorf("%s: %v, want it ignored", name, err)
		}
	}
	if st := candState(t, c, PCPub); st.count != 0 || len(st.pending) != 0 {
		t.Errorf("after dropped candidates: %+v, want nothing kept", st)
	}

	// Candidates that overtake their offer wait for it; the first 64 are kept.
	for i := range maxRemoteCandidates + 6 {
		if err := c.AddICECandidate(ctx, PCPub, 1, candInit("127.0.0.1", 6000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if st := candState(t, c, PCPub); st.gen != 1 || st.count != maxRemoteCandidates || len(st.pending) != maxRemoteCandidates {
		t.Errorf("buffered: gen %d, count %d, pending %d; want gen 1 and %d kept", st.gen, st.count, len(st.pending),
			maxRemoteCandidates)
	}
	_, raw, tracks := pubOfferFrom(t, "s_1")
	if _, err := c.HandleOffer(ctx, PCPub, 1, 1, raw, tracks); err != nil {
		t.Fatal(err)
	}
	if st := candState(t, c, PCPub); st.gen != 1 || st.count != maxRemoteCandidates || len(st.pending) != 0 {
		t.Errorf("after the offer: %+v, want the buffer flushed and the count kept", st)
	}
	if err := c.AddICECandidate(ctx, PCPub, 1, candInit("127.0.0.1", 7000)); err != nil {
		t.Fatal(err)
	}
	if st := candState(t, c, PCPub); st.count != maxRemoteCandidates {
		t.Errorf("count = %d after the 65th candidate, want it dropped", st.count)
	}

	// A candidate of a newer gen starts that gen's buffer; the old gen's candidates are over.
	if err := c.AddICECandidate(ctx, PCPub, 2, candInit("127.0.0.1", 7001)); err != nil {
		t.Fatal(err)
	}
	if err := c.AddICECandidate(ctx, PCPub, 1, candInit("127.0.0.1", 7002)); err != nil {
		t.Fatal(err)
	}
	if st := candState(t, c, PCPub); st.gen != 2 || st.count != 1 || len(st.pending) != 1 {
		t.Errorf("after a gen 2 candidate: %+v, want one buffered for gen 2", st)
	}
}

// TestSubCandidates: candidates the viewer trickles for the sub PC wait for its answer, then go to Pion; those of
// another gen are ignored.
func TestSubCandidates(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, pub, "s_1")
	if _, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityLow}}); err != nil {
		t.Fatal(err)
	}
	offer := sig.waitOffers(t, 1)[0]

	for i := range 2 {
		if err := viewer.AddICECandidate(ctx, PCSub, 1, candInit("127.0.0.1", 6000+i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, gen := range []uint32{0, 2} {
		if err := viewer.AddICECandidate(ctx, PCSub, gen, candInit("127.0.0.1", 6100)); err != nil {
			t.Fatal(err)
		}
	}
	if st := candState(t, viewer, PCSub); st.gen != 1 || st.count != 2 || len(st.pending) != 2 {
		t.Errorf("before the answer: %+v, want two candidates of gen 1 waiting", st)
	}

	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.sdp}); err != nil {
		t.Fatal(err)
	}
	answer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.HandleAnswer(ctx, PCSub, 1, 1, completeDescription(t, client, answer)); err != nil {
		t.Fatal(err)
	}
	if st := candState(t, viewer, PCSub); st.count != 2 || len(st.pending) != 0 {
		t.Errorf("after the answer: %+v, want the waiting candidates applied", st)
	}
	if err := viewer.AddICECandidate(ctx, PCSub, 1, candInit("127.0.0.1", 6002)); err != nil {
		t.Fatal(err)
	}
	if st := candState(t, viewer, PCSub); st.count != 3 || len(st.pending) != 0 {
		t.Errorf("a candidate after the answer: %+v, want it applied at once", st)
	}
}

// TestPCStateReadOnActor: Pion runs every OnConnectionStateChange callback in a goroutine of its own, so two changes
// close together can reach the actor in either order. A callback therefore only says that something changed: the
// actor reads the state, reports each one once, and what it reports last is the PC's real state (02 §5.3).
func TestPCStateReadOnActor(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, pub, "s_1")
	if _, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityLow}}); err != nil {
		t.Fatal(err)
	}
	offer := sig.waitOffers(t, 1)[0]
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.sdp}); err != nil {
		t.Fatal(err)
	}
	answer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.HandleAnswer(ctx, PCSub, 1, 1, completeDescription(t, client, answer)); err != nil {
		t.Fatal(err)
	}
	sig.waitPCStates(t, "sub/1:connected")

	// lateCallbacks is what the actor gets from callbacks that Pion started earlier, for whatever state.
	lateCallbacks := func() {
		t.Helper()
		if err := viewer.do(ctx, func(context.Context) error {
			for range 3 {
				viewer.onPCState(PCSub, 1, viewer.sub.pc)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	lateCallbacks()
	sig.waitPCStates(t, "sub/1:connected") // nothing new: the state is the one already reported

	// The PC closes while the actor is busy, so no callback for it has been handled yet. The next one to be handled
	// reports closed, even though Pion started it for an older state.
	if err := viewer.do(ctx, func(context.Context) error {
		sub := viewer.sub
		if !sub.ready.Load() {
			return errors.New("the DTLS-ready gate of the sub PC was not set when it connected")
		}
		if err := sub.pc.Close(); err != nil {
			return err
		}
		viewer.onPCState(PCSub, 1, sub.pc)
		if !sub.closed || sub.lastState != webrtc.PeerConnectionStateClosed {
			return fmt.Errorf("after a callback on the closed PC: closed %v, last state %s", sub.closed, sub.lastState)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Pion's own callbacks for the close, and any older one that arrives after them, change nothing: connected is
	// not reported after closed, and closed not twice.
	time.Sleep(100 * time.Millisecond)
	lateCallbacks()
	sig.waitPCStates(t, "sub/1:connected", "sub/1:closed")
}

// TestSubOfferFailure: a sub offer that Pion can't make is fatal for the sub PC (02 §5.3, the closed row). The SFU
// closes the PC at once, so nothing is left idle with a change that was never offered, and tells the client with an
// ErrorEvent (sfu.internal). The subscriptions stay for the sub PC that README S57 builds from closed.
func TestSubOfferFailure(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, pub, "s_1")

	// One actor turn: subscribe (which starts the debounce), break the PeerConnection so that CreateOffer fails, and do
	// what the debounce does. Pion's closed state can't arrive in between, so offerSub meets a PC it thinks is fine.
	if err := viewer.do(ctx, func(context.Context) error {
		errs, err := viewer.updateSubscriptions([]SubscriptionUpdate{{Share: "s_1", Video: QualityLow}})
		if err != nil {
			return err
		}
		if errs[0] != nil {
			return errs[0]
		}
		sub := viewer.sub
		if sub.debounce == nil || sub.closed {
			return fmt.Errorf("before the offer: debounce pending %v, closed %v", sub.debounce != nil, sub.closed)
		}
		if err := sub.pc.Close(); err != nil {
			return err
		}
		viewer.offerSub(sub)
		if !sub.closed || sub.offering || sub.dirty || sub.debounce != nil || sub.neg != 0 {
			return fmt.Errorf("after the failed offer: closed %v, offering %v, dirty %v, debounce pending %v, neg %d; "+
				"want a closed PC with nothing outstanding", sub.closed, sub.offering, sub.dirty, sub.debounce != nil, sub.neg)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sig.waitPCStates(t, "sub/1:closed")
	wantErrorEvent := func() {
		t.Helper()
		evs := sig.errorEvents()
		if len(evs) != 1 || evs[0].Scope != ScopePCSub {
			t.Fatalf("error events = %+v, want one for the sub PC", evs)
		}
		if e := errOf(t, evs[0].Err, CodeInternal); !e.Retryable {
			t.Error("the sub offer's sfu.internal is not retryable")
		}
	}
	wantErrorEvent()

	// The Conn goes on with a closed sub PC: the subscription is kept, answers have nothing to apply to, and a later
	// change (the share ends) neither offers nor fails again.
	if subs, err := viewer.Subscriptions(ctx); err != nil || len(subs) != 1 || subs[0].Share != "s_1" {
		t.Errorf("subscriptions after the failed offer = %+v, %v; want the one to s_1 kept for the rebuild", subs, err)
	}
	wantErr(t, viewer.HandleAnswer(ctx, PCSub, 1, 1, "v=0"), CodeBadPC)
	if err := pub.StopShare(ctx, "s_1", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		subs, err := viewer.Subscriptions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the subscription to the ended share is still there: %+v", subs)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(4 * subOfferDebounce)
	if n := len(sig.sent()); n != 0 {
		t.Errorf("%d sub offers on a PC that never made one, want none", n)
	}
	wantErrorEvent()
	sig.waitPCStates(t, "sub/1:closed")
}

// TestUpdateSubscriptions: the role, the guards and the per-item errors of 02 §6.1 and §12; a batch that creates
// subscriptions makes one debounced sub offer whose tracks bind every m-section; a quality change makes none.
func TestUpdateSubscriptions(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	elsewhere, _ := join(t, s, "den", "dan", "c-d", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	publisher, _ := join(t, s, "lounge", "carol", "c-c", RolePublisher)
	startShare(t, pub, "s_1")
	startShare(t, pub, "s_2")
	startShare(t, elsewhere, "s_den")

	_, err := publisher.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityLow}})
	wantErr(t, err, CodeRoleForbidden)
	_, err = viewer.UpdateSubscriptions(ctx, make([]SubscriptionUpdate, maxSubscriptionsPerCall+1))
	wantErr(t, err, CodeTooManySubscriptions)

	errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{
		{Share: "s_1", Video: QualityHigh, Audio: true},
		{Share: "s_gone", Video: QualityLow},
		{Share: "s_den", Video: QualityLow}, // a share of another room
		{Share: "s_2", Video: QualityHigh + 1},
		{Share: "s_2", Video: QualityLow},
	})
	if err != nil || len(errs) != 5 {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	if errs[0] != nil || errs[4] != nil {
		t.Errorf("the good items failed: %v, %v", errs[0], errs[4])
	}
	wantErr(t, errs[1], CodeShareNotFound)
	wantErr(t, errs[2], CodeShareNotFound)
	wantErr(t, errs[3], CodeInternal)

	subs, err := viewer.Subscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantSubs := []SubscriptionState{{Share: "s_1", Video: QualityHigh, Audio: true}, {Share: "s_2", Video: QualityLow}}
	if !reflect.DeepEqual(subs, wantSubs) {
		t.Errorf("subscriptions = %+v, want %+v", subs, wantSubs)
	}
	for _, id := range []ShareID{"s_1", "s_2"} {
		if v, a, ok := s.FanOut(id); !ok || v != 1 || a != 1 {
			t.Errorf("FanOut(%s) = %d video, %d audio, %v; want one DownTrack of each kind", id, v, a, ok)
		}
	}

	// One offer for the whole batch: gen 1, neg 1, four m-sections, each bound to its share.
	offers := sig.waitOffers(t, 1)
	offer := offers[0]
	wantTracks := []TrackBinding{
		{MID: "0", Share: "s_1", Kind: webrtc.RTPCodecTypeVideo}, {MID: "1", Share: "s_1", Kind: webrtc.RTPCodecTypeAudio},
		{MID: "2", Share: "s_2", Kind: webrtc.RTPCodecTypeVideo}, {MID: "3", Share: "s_2", Kind: webrtc.RTPCodecTypeAudio},
	}
	if sig.gens[0] != 1 || offer.neg != 1 || !slices.Equal(offer.tracks, wantTracks) {
		t.Errorf("sub offer: gen %d, neg %d, tracks %+v; want gen 1, neg 1, %+v", sig.gens[0], offer.neg, offer.tracks,
			wantTracks)
	}
	desc, err := parseSDP(offer.sdp)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.MediaDescriptions) != 4 {
		t.Fatalf("%d m-sections in the sub offer, want 4", len(desc.MediaDescriptions))
	}
	for i, md := range desc.MediaDescriptions {
		share := wantTracks[i].Share
		wantMSID := string(share) + " v-" + string(share)
		if mediaKind(md) == webrtc.RTPCodecTypeAudio {
			wantMSID = string(share) + " a-" + string(share)
		}
		if msid, _ := md.Attribute("msid"); msid != wantMSID {
			t.Errorf("m-section %d: msid %q, want %q", i, msid, wantMSID)
		}
		if direction(md.Attributes) != sdp.AttrKeySendOnly {
			t.Errorf("m-section %d is %s, want sendonly: viewers never send on the sub PC", i, direction(md.Attributes))
		}
	}

	// Changing quality never renegotiates, and neither does an item for a share that is already subscribed.
	errs, err = viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityOff}})
	if err != nil || errs[0] != nil {
		t.Fatalf("quality change: %v, %v", errs, err)
	}
	time.Sleep(4 * subOfferDebounce)
	if n := len(sig.sent()); n != 1 {
		t.Errorf("%d sub offers after a quality change, want still 1", n)
	}
	if subs, _ := viewer.Subscriptions(ctx); len(subs) != 2 || subs[0].Video != QualityOff || subs[0].Audio {
		t.Errorf("subscriptions after the change = %+v", subs)
	}

	// The 256-subscription guard, with the Conn's table filled by hand (256 real ones would mean 512 m-sections).
	if err := viewer.do(ctx, func(context.Context) error {
		sh := s.lookupShare("s_1")
		for i := len(viewer.subs); i < maxSubscriptions; i++ {
			sub := &Subscription{conn: viewer, share: sh}
			sub.video = newDownTrack(sub, webrtc.RTPCodecTypeVideo)
			sub.audio = newDownTrack(sub, webrtc.RTPCodecTypeAudio)
			viewer.subs[ShareID(fmt.Sprintf("s_fill%d", i))] = sub
		}
		delete(viewer.subs, "s_2")
		viewer.subs["s_fill_last"] = viewer.subs["s_fill2"]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	errs, err = viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityLow}, {Share: "s_2"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("with a full table: %v, %v", errs, err)
	}
	wantErr(t, errs[1], CodeTooManySubscriptions)
}

// fakeTrackContext is the webrtc.TrackLocalContext Pion hands to Bind, with a chosen negotiation result.
type fakeTrackContext struct {
	codecs []webrtc.RTPCodecParameters
	exts   []webrtc.RTPHeaderExtensionParameter
}

func (f fakeTrackContext) CodecParameters() []webrtc.RTPCodecParameters { return f.codecs }
func (f fakeTrackContext) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return f.exts
}
func (fakeTrackContext) SSRC() webrtc.SSRC                       { return 1111 }
func (fakeTrackContext) SSRCRetransmission() webrtc.SSRC         { return 2222 }
func (fakeTrackContext) SSRCForwardErrorCorrection() webrtc.SSRC { return 0 }
func (fakeTrackContext) WriteStream() webrtc.TrackLocalWriter    { return nil }
func (fakeTrackContext) ID() string                              { return "fake" }
func (fakeTrackContext) RTCPReader() interceptor.RTCPReader      { return nil }

func h264Param(pt webrtc.PayloadType, plid string) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpPrefix + plid,
		},
		PayloadType: pt,
	}
}

func rtxParam(pt, apt webrtc.PayloadType) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", apt),
		},
		PayloadType: pt,
	}
}

// TestDownTrackBind: Bind maps each stream profile to the viewer's payload type for it, finds RTX and
// abs-send-time, and never fails while a codec of its kind is negotiated (02 §8.5, §9.3).
func TestDownTrackBind(t *testing.T) {
	sub := &Subscription{share: &Share{id: "s_1"}}
	video := func() *DownTrack { return newDownTrack(sub, webrtc.RTPCodecTypeVideo) }
	if d := video(); d.ID() != "v-s_1" || d.StreamID() != "s_1" || d.RID() != "" || d.Kind() != webrtc.RTPCodecTypeVideo {
		t.Errorf("video DownTrack ids: %q %q %q %v", d.ID(), d.StreamID(), d.RID(), d.Kind())
	}
	if d := newDownTrack(sub, webrtc.RTPCodecTypeAudio); d.ID() != "a-s_1" || d.StreamID() != "s_1" {
		t.Errorf("audio DownTrack ids: %q %q", d.ID(), d.StreamID())
	}

	// A viewer like Chrome: all five profiles with RTX, and abs-send-time.
	chrome := fakeTrackContext{exts: []webrtc.RTPHeaderExtensionParameter{{URI: sdp.ABSSendTimeURI, ID: 3}}}
	for _, c := range h264Codecs {
		chrome.codecs = append(chrome.codecs, h264Param(c.pt, c.profileLevelID), rtxParam(c.rtx, c.pt))
	}
	d := video()
	codec, err := d.Bind(chrome)
	if err != nil {
		t.Fatal(err)
	}
	b := d.binding.Load()
	wantPTs := map[ProfileKey]uint8{"42e0": 96, "4200": 98, "4d00": 100, "640c": 102, "6400": 104}
	wantRTX := map[uint8]uint8{96: 97, 98: 99, 100: 101, 102: 103, 104: 105}
	if b == nil || !reflect.DeepEqual(b.ptFor, wantPTs) || !reflect.DeepEqual(b.rtxPTFor, wantRTX) ||
		b.ssrc != 1111 || b.rtxSSRC != 2222 || b.absSendTimeID != 3 || b.unsupported {
		t.Errorf("binding = %+v", b)
	}
	if codec.PayloadType != 104 {
		t.Errorf("Bind returned PT %d, want 104: the sender starts with the best profile the viewer takes", codec.PayloadType)
	}
	sub.share.profile = ProfileConstrainedBaseline // the share's current profile, once the media path knows it
	if codec, err = video().Bind(chrome); err != nil || codec.PayloadType != 96 {
		t.Errorf("Bind with a CB share returned PT %d, %v; want 96", codec.PayloadType, err)
	}
	sub.share.profile = ""
	if err := d.Unbind(chrome); err != nil || d.binding.Load() != nil {
		t.Errorf("Unbind: %v, binding %v", err, d.binding.Load())
	}

	// A viewer that decodes only Constrained Baseline (Firefox): High streams have no PT there.
	firefox := fakeTrackContext{codecs: []webrtc.RTPCodecParameters{h264Param(126, "42e01f"), rtxParam(127, 126)}}
	d = video()
	if codec, err = d.Bind(firefox); err != nil || codec.PayloadType != 126 {
		t.Fatalf("CB-only Bind = PT %d, %v", codec.PayloadType, err)
	}
	if b := d.binding.Load(); !reflect.DeepEqual(b.ptFor, map[ProfileKey]uint8{"42e0": 126, "4200": 126}) ||
		b.absSendTimeID != 0 || b.unsupported {
		t.Errorf("CB-only binding = %+v", b)
	}

	// Video negotiated without any H.264 the SFU forwards: Bind still succeeds, marked unsupported.
	vp8 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, PayloadType: 120,
	}
	d = video()
	if codec, err = d.Bind(fakeTrackContext{codecs: []webrtc.RTPCodecParameters{rtxParam(121, 120), vp8}}); err != nil ||
		codec.PayloadType != 120 {
		t.Fatalf("Bind without H.264 = PT %d, %v; want the first negotiated codec", codec.PayloadType, err)
	}
	if b := d.binding.Load(); b == nil || !b.unsupported || len(b.ptFor) != 0 {
		t.Errorf("binding without H.264 = %+v, want unsupported", b)
	}
	// No codec at all: the only case Bind fails.
	d = video()
	if _, err := d.Bind(fakeTrackContext{}); err == nil || d.binding.Load() != nil {
		t.Errorf("Bind without codecs: %v, binding %v", err, d.binding.Load())
	}

	// Audio: Opus.
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}
	a := newDownTrack(sub, webrtc.RTPCodecTypeAudio)
	if codec, err = a.Bind(fakeTrackContext{codecs: []webrtc.RTPCodecParameters{opus}}); err != nil || codec.PayloadType != 111 {
		t.Fatalf("audio Bind = PT %d, %v", codec.PayloadType, err)
	}
	if b := a.binding.Load(); !reflect.DeepEqual(b.ptFor, map[ProfileKey]uint8{"": 111}) || b.unsupported {
		t.Errorf("audio binding = %+v", b)
	}
}

// TestEnumStrings: the names that are also Metrics label values (02 §13).
func TestEnumStrings(t *testing.T) {
	for got, want := range map[string]string{
		RoleFull.String(): "full", RoleViewer.String(): "viewer", RolePublisher.String(): "publisher",
		RoleAgent.String(): "agent", Role(0).String(): "role?",
		ClientWeb.String(): "web", ClientDesktop.String(): "desktop", ClientMobile.String(): "mobile",
		ClientKind(0).String(): "client?",
		PCPub.String():         "pub", PCSub.String(): "sub", PCKind(0).String(): "pc?",
		QualityOff.String(): "off", QualityLow.String(): "low", QualityHigh.String(): "high",
		Quality(9).String(): "quality?",
		PresetAuto.String(): "auto", PresetGame.String(): "game", PresetMovie.String(): "movie",
		PresetText.String(): "text", Preset(9).String(): "preset?",
		SourceUnknown.String(): "unknown", SourceScreen.String(): "screen", SourceWindow.String(): "window",
		SourceTab.String(): "tab", SourceKind(9).String(): "source?",
		SharePending.String(): "pending", ShareLive.String(): "live", ShareStalled.String(): "stalled",
		ShareState(9).String(): "state?",
	} {
		if got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
	if !QualityOff.valid() || Quality(3).valid() || Role(0).valid() || ClientKind(0).valid() || !RoleAgent.valid() {
		t.Error("valid() is wrong for an enum boundary")
	}
	if RoleViewer.canPublish() || !RoleAgent.canPublish() || !RolePublisher.canPublish() || !RoleFull.canPublish() {
		t.Error("canPublish doesn't follow 01 §6.3")
	}
	if !RoleViewer.canSubscribe() || RoleAgent.canSubscribe() || RolePublisher.canSubscribe() || !RoleFull.canSubscribe() {
		t.Error("canSubscribe doesn't follow 01 §6.3")
	}
	if got, ok := slotFor(webrtc.RTPCodecTypeVideo, ""); got != SlotF || !ok {
		t.Errorf("slotFor(video, no rid) = %v, %v", got, ok)
	}
	if got, ok := slotFor(webrtc.RTPCodecTypeVideo, "q"); got != SlotQ || !ok {
		t.Errorf("slotFor(video, q) = %v, %v", got, ok)
	}
	if _, ok := slotFor(webrtc.RTPCodecTypeVideo, "h"); ok {
		t.Error("slotFor accepts rid h, which M1 rejects")
	}
	if got, ok := slotFor(webrtc.RTPCodecTypeAudio, ""); got != SlotAudio || !ok {
		t.Errorf("slotFor(audio) = %v, %v", got, ok)
	}
}
