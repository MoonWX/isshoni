package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// fakeServer is a scripted signaling server for the core's unit tests (docs/m2/01-client-core.md §14: "a
// signaltest-style fake server where cheap"): it answers each hello as the test says (a welcome, or an error and
// the close code that follows it), answers requests (room.join, room.leave, share.start, share.stop) like the hub,
// and records every message it receives. It answers no pub offer, so a share's pub PC never connects.
//
// Like the hub, a resumed welcome keeps the connection's id and room, and is followed by a room.state that lists
// the connection's shares (as live); share.start gives each share an id of its own (s_1, s_2, …), and the same one
// again for a ref it has seen on the connection. Like the SFU (M1 01 §9 rules 2 and 3), it keeps the gen and neg of
// the connection's pub offers while the connection is in a room, resumes included, and answers an offer of an older
// gen, or of an older neg in the current gen, with error{stale_negotiation, scope pc}.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	hellos []protocol.Hello    // every hello, in order
	got    []protocol.Envelope // every other message, in order
	conns  []*fakeConn
	shares []fakeShare // the shares started and not stopped
	// hello answers the n-th hello (0-based); nil answers each with a welcome, resumed when it carries a resume
	// token of this server.
	hello func(n int, h protocol.Hello) fakeAnswer
	// silent lists request types the server never answers; reject answers request types with an error, and
	// rejectOnce the next request of a type (then it answers that type again).
	silent     []protocol.MessageType
	reject     map[protocol.MessageType]protocol.Error
	rejectOnce map[protocol.MessageType]protocol.Error
	// stale counts the pub offers answered with stale_negotiation.
	stale int
	// iceServers go into every welcome (none when nil, as M1's server sends).
	iceServers []protocol.ICEServer
	nextID     int
	nextShare  int
}

// fakeShare is a share the fake server started.
type fakeShare struct {
	id, connID, room, ref string
}

// setHello replaces the hello answers while the server runs.
func (fs *fakeServer) setHello(f func(n int, h protocol.Hello) fakeAnswer) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.hello = f
}

// fakeAnswer is the server's answer to a hello: an error (with the close code its catalog entry names), or a
// welcome.
type fakeAnswer struct {
	err     *protocol.Error
	resumed bool
}

// fakeConn is one socket of the server.
type fakeConn struct {
	ws     *websocket.Conn
	connID string
	room   string
	// pubGen and pubNeg are the newest pub offer's, as the SFU keeps them for the connection while it is in a room
	// (the SFU's Conn of a room membership); 0 before the first.
	pubGen, pubNeg uint32
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.close)
	return fs
}

// url is the server's origin, for Options.ServerURL.
func (fs *fakeServer) url() string { return fs.srv.URL }

func (fs *fakeServer) close() {
	fs.mu.Lock()
	conns := slices.Clone(fs.conns)
	fs.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.CloseNow()
	}
	fs.srv.Close()
}

// welcomeErr is a hello answer that refuses the hello with code in scope.
func welcomeErr(code protocol.ErrorCode, scope protocol.ErrorScope, params map[string]any) fakeAnswer {
	e := protocol.NewError(code, scope)
	e.Params = params
	return fakeAnswer{err: &e}
}

func (fs *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := context.Background()
	_, b, err := ws.Read(ctx)
	if err != nil {
		_ = ws.CloseNow()
		return
	}
	env, err := protocol.ParseEnvelope(b)
	if err != nil || env.Type != protocol.MessageTypeHello {
		_ = ws.CloseNow()
		return
	}
	hello, err := protocol.Decode[protocol.Hello](env)
	if err != nil {
		_ = ws.CloseNow()
		return
	}
	fs.mu.Lock()
	n := len(fs.hellos)
	fs.hellos = append(fs.hellos, hello)
	answerFn := fs.hello
	fs.mu.Unlock()
	ans := fakeAnswer{resumed: hello.ResumeToken != ""}
	if answerFn != nil {
		ans = answerFn(n, hello)
	}
	if ans.err != nil {
		fs.write(ws, protocol.MessageTypeError, "", env.ID, ans.err)
		code := protocol.CloseCodeFor(ans.err.Code, ans.err.Scope)
		if code == 0 {
			code = protocol.CloseCodeProtocolViolation
		}
		_ = ws.Close(websocket.StatusCode(code), "")
		return
	}
	fs.mu.Lock()
	c := &fakeConn{ws: ws}
	var old *fakeConn
	if ans.resumed {
		id := strings.TrimPrefix(hello.ResumeToken.Reveal(), "rt-")
		if i := slices.IndexFunc(fs.conns, func(c *fakeConn) bool { return c.connID == id }); i >= 0 {
			old = fs.conns[i]
		}
	}
	if old != nil {
		c.connID, c.room, c.pubGen, c.pubNeg = old.connID, old.room, old.pubGen, old.pubNeg
	} else {
		ans.resumed = false
		fs.nextID++
		c.connID = "c_" + strconv.Itoa(fs.nextID)
	}
	fs.conns = append(fs.conns, c)
	ice := append([]protocol.ICEServer{}, fs.iceServers...)
	var shares []protocol.ShareInfo
	for _, s := range fs.shares {
		if s.connID == c.connID && s.room == c.room {
			shares = append(shares, protocol.ShareInfo{ID: s.id, UserID: "u_alice", ConnectionID: s.connID,
				Kind: protocol.ShareKindWindow, Preset: protocol.PresetAuto, Status: protocol.ShareStatusLive})
		}
	}
	fs.mu.Unlock()
	fs.write(ws, protocol.MessageTypeWelcome, "", env.ID, protocol.Welcome{
		Protocol: protocol.Version, ServerVersion: "0.2.0-test",
		Features:      []protocol.Feature{protocol.FeatureAgentRelay},
		Limits:        protocol.Limits{PingIntervalMs: 15000, GraceMs: 30000},
		ICEServers:    ice,
		ConnectionID:  c.connID,
		ResumeToken:   protocol.Secret("rt-" + c.connID),
		Resumed:       ans.resumed,
		RoomID:        c.room,
		DefaultRoomID: "lounge",
		User:          protocol.UserInfo{ID: "u_alice", Name: "alice"},
		ServerTime:    time.Now(),
	})
	if c.room != "" {
		fs.write(ws, protocol.MessageTypeRoomState, "", "", protocol.RoomState{RoomID: c.room, Rev: 2,
			Participants: []protocol.ParticipantInfo{}, Shares: append([]protocol.ShareInfo{}, shares...)})
	}
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		env, err := protocol.ParseEnvelope(b)
		if err != nil {
			continue
		}
		fs.mu.Lock()
		fs.got = append(fs.got, env)
		silent := slices.Contains(fs.silent, env.Type)
		rej, rejected := fs.reject[env.Type]
		if once, ok := fs.rejectOnce[env.Type]; ok && !rejected {
			rej, rejected = once, true
			delete(fs.rejectOnce, env.Type)
		}
		fs.mu.Unlock()
		switch {
		case silent:
		case rejected:
			fs.write(ws, protocol.MessageTypeError, "", env.ID, rej)
		default:
			fs.answer(c, env)
		}
	}
}

// answer answers a request like the hub would, as far as the tests need it.
func (fs *fakeServer) answer(c *fakeConn, env protocol.Envelope) {
	switch env.Type {
	case protocol.MessageTypePing:
		fs.write(c.ws, protocol.MessageTypePong, "", "", protocol.Pong{})
	case protocol.MessageTypeRoomJoin:
		j, _ := protocol.Decode[protocol.RoomJoin](env)
		fs.mu.Lock()
		c.room, c.pubGen, c.pubNeg = j.RoomID, 0, 0
		fs.mu.Unlock()
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.RoomJoinResult{Room: protocol.RoomInfo{ID: j.RoomID, Name: j.RoomID}})
		fs.write(c.ws, protocol.MessageTypeRoomState, "", "", protocol.RoomState{RoomID: j.RoomID, Rev: 1,
			Participants: []protocol.ParticipantInfo{}, Shares: []protocol.ShareInfo{}})
	case protocol.MessageTypeRoomLeave:
		fs.mu.Lock()
		c.room, c.pubGen, c.pubNeg = "", 0, 0
		fs.shares = slices.DeleteFunc(fs.shares, func(s fakeShare) bool { return s.connID == c.connID })
		fs.mu.Unlock()
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.Empty{})
	case protocol.MessageTypeShareStart:
		st, _ := protocol.Decode[protocol.ShareStart](env)
		fs.mu.Lock()
		i := slices.IndexFunc(fs.shares, func(s fakeShare) bool { return s.connID == c.connID && s.ref == st.Ref })
		if i < 0 {
			fs.nextShare++
			fs.shares = append(fs.shares, fakeShare{id: "s_" + strconv.Itoa(fs.nextShare), connID: c.connID, room: c.room,
				ref: st.Ref})
			i = len(fs.shares) - 1
		}
		id := fs.shares[i].id
		fs.mu.Unlock()
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, fakeParams(id))
	case protocol.MessageTypeShareStop:
		st, _ := protocol.Decode[protocol.ShareStop](env)
		fs.mu.Lock()
		fs.shares = slices.DeleteFunc(fs.shares, func(s fakeShare) bool { return s.id == st.ShareID })
		fs.mu.Unlock()
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.Empty{})
	case protocol.MessageTypeShareUpdate, protocol.MessageTypeStatsWatch:
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.Empty{})
	case protocol.MessageTypePCOffer:
		o, err := protocol.Decode[protocol.PCOffer](env)
		if err != nil || o.PC != protocol.PCKindPub {
			return
		}
		fs.mu.Lock()
		stale := o.Gen < c.pubGen || (o.Gen == c.pubGen && o.Neg < c.pubNeg)
		if stale {
			fs.stale++
		} else {
			c.pubGen, c.pubNeg = o.Gen, o.Neg
		}
		fs.mu.Unlock()
		if stale {
			e := protocol.NewError(protocol.ErrorCodeStaleNegotiation, protocol.ErrorScopePC)
			e.PC, e.Gen, e.Neg = protocol.PCKindPub, o.Gen, o.Neg
			fs.write(c.ws, protocol.MessageTypeError, "", "", e)
		}
	}
}

// staleOffers returns how many pub offers the server answered with stale_negotiation.
func (fs *fakeServer) staleOffers() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.stale
}

// send writes a notification to the newest socket, as the hub sends one.
func (fs *fakeServer) send(t protocol.MessageType, data any) {
	fs.write(fs.last().ws, t, "", "", data)
}

// received decodes the messages of type t that the server received, in order.
func received[T any](fs *fakeServer, t protocol.MessageType) []T {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []T
	for _, env := range fs.got {
		if env.Type != t {
			continue
		}
		if v, err := protocol.Decode[T](env); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// fakeParams are the server's ShareParams: High, f at 1080p30 and q at 360p15 (small for the race detector), Opus.
func fakeParams(id string) protocol.ShareParams {
	return protocol.ShareParams{
		ShareID: id, Codec: protocol.CodecH264High, AudioBitrate: 128_000,
		Encodings: []protocol.Encoding{
			{RID: protocol.RIDHigh, Layer: protocol.VideoLayerHigh, Active: true, MaxBitrate: 8_000_000, MaxFramerate: 30,
				MaxPixels: 1920 * 1080},
			{RID: protocol.RIDLow, Layer: protocol.VideoLayerLow, Active: true, MaxBitrate: 300_000, MaxFramerate: 15,
				MaxPixels: 640 * 360},
		},
	}
}

func (fs *fakeServer) write(ws *websocket.Conn, t protocol.MessageType, id, re string, data any) {
	b, err := protocol.Marshal(t, id, re, data)
	if err != nil {
		fs.t.Errorf("fake server: marshal %s: %v", t, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageText, b)
}

// last returns the server's newest socket.
func (fs *fakeServer) last() *fakeConn {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.conns) == 0 {
		fs.t.Fatal("fake server: no connection")
	}
	return fs.conns[len(fs.conns)-1]
}

// endSession sends an error in scope session or connection on the newest socket and closes it with the code that
// follows it, as the hub does when it revokes a session.
func (fs *fakeServer) endSession(code protocol.ErrorCode, scope protocol.ErrorScope) {
	c := fs.last()
	e := protocol.NewError(code, scope)
	fs.write(c.ws, protocol.MessageTypeError, "", "", e)
	_ = c.ws.Close(websocket.StatusCode(protocol.CloseCodeFor(code, scope)), "")
}

// kill drops the newest socket without a close frame: the client resumes on a new one.
func (fs *fakeServer) kill() {
	_ = fs.last().ws.CloseNow()
}

// helloCount returns the hellos so far.
func (fs *fakeServer) helloCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.hellos)
}

// helloAt returns the n-th hello.
func (fs *fakeServer) helloAt(n int) protocol.Hello {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hellos[n]
}

// types returns the types of the messages received after the hellos, in order.
func (fs *fakeServer) types() []protocol.MessageType {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []protocol.MessageType
	for _, env := range fs.got {
		if env.Type != protocol.MessageTypePing {
			out = append(out, env.Type)
		}
	}
	return out
}

// settledTypes is types once the server has read nothing new for 100 ms (within waitTimeout). A call such as Stop
// returns when its last message is queued on the connection, before the server has read it, so a check of the whole
// sequence waits for it to settle.
func (fs *fakeServer) settledTypes() []protocol.MessageType {
	const quiet = 100 * time.Millisecond
	deadline := time.Now().Add(waitTimeout)
	last, since := fs.types(), time.Now()
	for time.Since(since) < quiet && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		if now := fs.types(); len(now) != len(last) {
			last, since = now, time.Now()
		}
	}
	return last
}

// fakeAuth is a scripted core.Auth: AccessToken answers from tokens (forceRefresh false) and forced (true), the last
// answer repeating; SessionEnded is recorded.
type fakeAuth struct {
	mu          sync.Mutex
	tokens      []tokenAnswer
	forced      []tokenAnswer
	calls       int
	forcedCalls int
	ended       []protocol.ErrorCode
	endedLeft   time.Duration // the time SessionEnded's ctx had left
	endedAt     time.Time
	endedDelay  time.Duration // SessionEnded takes this long
	endedHook   func()        // runs when SessionEnded is called, before its delay
}

type tokenAnswer struct {
	tok protocol.Secret
	err error
}

func (a *fakeAuth) AccessToken(_ context.Context, force bool) (protocol.Secret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	list, n := a.tokens, a.calls
	if force {
		list, n = a.forced, a.forcedCalls
		a.forcedCalls++
	} else {
		a.calls++
	}
	if len(list) == 0 {
		return "token", nil
	}
	ans := list[min(n, len(list)-1)]
	return ans.tok, ans.err
}

func (a *fakeAuth) SessionEnded(ctx context.Context, code protocol.ErrorCode) {
	a.mu.Lock()
	delay, hook := a.endedDelay, a.endedHook
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, code)
	a.endedAt = time.Now()
	if d, ok := ctx.Deadline(); ok {
		a.endedLeft = time.Until(d)
	}
}

// setEndedHook sets the function SessionEnded runs first.
func (a *fakeAuth) setEndedHook(f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.endedHook = f
}

func (a *fakeAuth) snapshot() (calls, forced int, ended []protocol.ErrorCode, left time.Duration, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, a.forcedCalls, slices.Clone(a.ended), a.endedLeft, a.endedAt
}

// relinkError is an Auth error that says the device is no longer linked (02's deviceauth errors have the method).
type relinkError struct{}

func (relinkError) Error() string        { return "device unlinked" }
func (relinkError) RelinkRequired() bool { return true }

// updateError is an Auth error that says the app must be updated.
type updateError struct{}

func (updateError) Error() string        { return "app outdated" }
func (updateError) UpdateRequired() bool { return true }

var errTransient = errors.New("token endpoint unreachable")
