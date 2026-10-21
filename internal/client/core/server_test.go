package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	hellos []protocol.Hello    // every hello, in order
	got    []protocol.Envelope // every other message, in order
	conns  []*fakeConn
	// hello answers the n-th hello (0-based); nil answers each with a welcome, resumed when it carries a resume
	// token of this server.
	hello func(n int, h protocol.Hello) fakeAnswer
	// silent lists request types the server never answers; reject answers request types with an error.
	silent []protocol.MessageType
	reject map[protocol.MessageType]protocol.Error
	nextID int
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
	fs.nextID++
	c := &fakeConn{ws: ws, connID: "c_" + strconv.Itoa(fs.nextID)}
	fs.conns = append(fs.conns, c)
	fs.mu.Unlock()
	fs.write(ws, protocol.MessageTypeWelcome, "", env.ID, protocol.Welcome{
		Protocol: protocol.Version, ServerVersion: "0.2.0-test",
		Features:      []protocol.Feature{protocol.FeatureAgentRelay},
		Limits:        protocol.Limits{PingIntervalMs: 15000, GraceMs: 30000},
		ICEServers:    []protocol.ICEServer{},
		ConnectionID:  c.connID,
		ResumeToken:   protocol.Secret("rt-" + c.connID),
		Resumed:       ans.resumed,
		DefaultRoomID: "lounge",
		User:          protocol.UserInfo{ID: "u_alice", Name: "alice"},
		ServerTime:    time.Now(),
	})
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
		c.room = j.RoomID
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.RoomJoinResult{Room: protocol.RoomInfo{ID: j.RoomID, Name: j.RoomID}})
		fs.write(c.ws, protocol.MessageTypeRoomState, "", "", protocol.RoomState{RoomID: j.RoomID, Rev: 1,
			Participants: []protocol.ParticipantInfo{}, Shares: []protocol.ShareInfo{}})
	case protocol.MessageTypeRoomLeave:
		c.room = ""
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.Empty{})
	case protocol.MessageTypeShareStart:
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, fakeParams("s_1"))
	case protocol.MessageTypeShareStop, protocol.MessageTypeShareUpdate, protocol.MessageTypeStatsWatch:
		fs.write(c.ws, protocol.MessageTypeOK, "", env.ID, protocol.Empty{})
	}
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
	delay := a.endedDelay
	a.mu.Unlock()
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
