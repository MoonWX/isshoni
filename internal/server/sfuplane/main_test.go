package sfuplane

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// TestMain fails the run when goroutines outlive the tests. The tests with a real SFU use Pion, whose goroutines get
// 2 s to settle after Close (02 §17); goleak's own retries stop after about 0.5 s.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		err := goleak.Find()
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			err = goleak.Find()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// ---- a fake *sfu.Conn ----

// call is one recorded call of the fake Conn: the method and its arguments, without the context.
type call struct {
	Method string
	Args   []any
}

// fakeConn stands in for an *sfu.Conn (the conn interface): it records every call, checks that each one that takes
// a context carries the 5 s deadline, and returns what the test set.
type fakeConn struct {
	t *testing.T

	mu          sync.Mutex
	calls       []call
	errs        map[string]error // by method: what it returns
	itemErrs    []error          // UpdateSubscriptions' per-item errors
	shareParams sfu.ShareParams  // StartShare and UpdateShare
	answer      string           // HandleOffer
	stats       sfu.ConnStats
}

var _ conn = (*fakeConn)(nil)

// record notes a call that takes a context, which must carry the deadline of callTimeout, and returns the error set
// for its method.
func (f *fakeConn) record(ctx context.Context, method string, args ...any) error {
	f.t.Helper()
	deadline, ok := ctx.Deadline()
	if left := time.Until(deadline); !ok || left <= 0 || left > callTimeout {
		f.t.Errorf("%s: the context's deadline is %v away (set: %v), want within %v", method, left, ok, callTimeout)
	}
	return f.note(method, args...)
}

// note records a call and returns the error set for its method.
func (f *fakeConn) note(method string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Method: method, Args: args})
	return f.errs[method]
}

func (f *fakeConn) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string]error{}
	}
	f.errs[method] = err
}

// take returns the calls recorded since the last take.
func (f *fakeConn) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeConn) HandleOffer(ctx context.Context, pc sfu.PCKind, gen, neg uint32, sdp string,
	tracks []sfu.TrackBinding,
) (string, error) {
	if err := f.record(ctx, "HandleOffer", pc, gen, neg, sdp, tracks); err != nil {
		return "", err
	}
	return f.answer, nil
}

func (f *fakeConn) HandleAnswer(ctx context.Context, pc sfu.PCKind, gen, neg uint32, sdp string) error {
	return f.record(ctx, "HandleAnswer", pc, gen, neg, sdp)
}

func (f *fakeConn) AddICECandidate(ctx context.Context, pc sfu.PCKind, gen uint32, cand webrtc.ICECandidateInit) error {
	return f.record(ctx, "AddICECandidate", pc, gen, cand)
}

func (f *fakeConn) RestartICE(ctx context.Context, pc sfu.PCKind, gen uint32) error {
	return f.record(ctx, "RestartICE", pc, gen)
}

func (f *fakeConn) ResetPC(ctx context.Context, pc sfu.PCKind, gen uint32) error {
	return f.record(ctx, "ResetPC", pc, gen)
}

func (f *fakeConn) ClosePC(ctx context.Context, pc sfu.PCKind, gen uint32) error {
	return f.record(ctx, "ClosePC", pc, gen)
}

func (f *fakeConn) StartShare(ctx context.Context, p sfu.StartShareParams) (sfu.ShareParams, error) {
	if err := f.record(ctx, "StartShare", p); err != nil {
		return sfu.ShareParams{}, err
	}
	return f.shareParams, nil
}

func (f *fakeConn) UpdateShare(ctx context.Context, id sfu.ShareID, u sfu.ShareUpdate) (sfu.ShareParams, error) {
	if err := f.record(ctx, "UpdateShare", id, u); err != nil {
		return sfu.ShareParams{}, err
	}
	return f.shareParams, nil
}

func (f *fakeConn) StopShare(ctx context.Context, id sfu.ShareID, r sfu.EndReason) error {
	return f.record(ctx, "StopShare", id, r)
}

func (f *fakeConn) UpdateSubscriptions(ctx context.Context, items []sfu.SubscriptionUpdate) ([]error, error) {
	if err := f.record(ctx, "UpdateSubscriptions", items); err != nil {
		return nil, err
	}
	errs := make([]error, len(items))
	copy(errs, f.itemErrs)
	return errs, nil
}

func (f *fakeConn) SetDecodeCaps(ctx context.Context, caps sfu.DecodeCaps) error {
	return f.record(ctx, "SetDecodeCaps", caps)
}

func (f *fakeConn) Resync() { _ = f.note("Resync") }

func (f *fakeConn) Stats() sfu.ConnStats {
	_ = f.note("Stats")
	return f.stats
}

func (f *fakeConn) Close(r sfu.EndReason) { _ = f.note("Close", r) }

// ---- a log recorder ----

// logLine is one recorded log record.
type logLine struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// logRecorder is a slog.Handler that keeps every record with its attributes, those of With included.
type logRecorder struct {
	mu    *sync.Mutex
	lines *[]logLine
	attrs []slog.Attr
}

func newLogRecorder() *logRecorder {
	return &logRecorder{mu: &sync.Mutex{}, lines: &[]logLine{}}
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	line := logLine{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	for _, a := range l.attrs {
		line.Attrs[a.Key] = a.Value.Resolve().String()
	}
	r.Attrs(func(a slog.Attr) bool {
		line.Attrs[a.Key] = a.Value.Resolve().String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.lines = append(*l.lines, line)
	return nil
}

func (l *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logRecorder{mu: l.mu, lines: l.lines, attrs: append(slices.Clone(l.attrs), attrs...)}
}

func (l *logRecorder) WithGroup(string) slog.Handler { return l }

// take returns the records since the last take.
func (l *logRecorder) take() []logLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := *l.lines
	*l.lines = nil
	return out
}

// atLeast returns the records at or above a level.
func atLeast(lines []logLine, level slog.Level) []logLine {
	var out []logLine
	for _, l := range lines {
		if l.Level >= level {
			out = append(out, l)
		}
	}
	return out
}

// ---- a Plane on fake Conns ----

// testPlane is a Plane whose Join returns a new fakeConn for each peer, in place of a bound SFU.
type testPlane struct {
	t      *testing.T
	plane  *Plane
	events sfu.RoomEvents
	logs   *logRecorder

	mu      sync.Mutex
	joins   []sfu.JoinParams
	conns   []*fakeConn
	joinErr error
}

func newTestPlane(t *testing.T) *testPlane {
	t.Helper()
	tp := &testPlane{t: t, logs: newLogRecorder()}
	tp.plane, tp.events = New(slog.New(tp.logs))
	tp.plane.join = func(jp sfu.JoinParams) (conn, error) {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		if tp.joinErr != nil {
			return nil, tp.joinErr
		}
		fc := &fakeConn{t: t, answer: "v=0\r\n", shareParams: sfu.ShareParams{
			Profile:      sfu.ProfileHigh,
			Encodings:    []sfu.EncodingParams{{RID: "f", Active: true}, {RID: "q", Active: true}},
			AudioBitrate: 128_000,
		}}
		tp.joins = append(tp.joins, jp)
		tp.conns = append(tp.conns, fc)
		return fc, nil
	}
	return tp
}

// testPeer is one peer of a testPlane with everything around it.
type testPeer struct {
	signal.MediaPeer
	peer *peer // the same peer, as the SFU's Signaler
	conn *fakeConn
	sink *signaltest.Sink
	join sfu.JoinParams
}

// newPeer joins a full web connection of a user as connection connID in the lounge.
func (tp *testPlane) newPeer(connID, userID string) *testPeer {
	tp.t.Helper()
	return tp.newPeerWith(signal.PeerParams{
		ConnectionID: connID, UserID: userID, RoomID: "lounge", Role: protocol.RoleFull,
		Client: protocol.ClientInfo{Kind: protocol.ClientKindWeb},
	})
}

func (tp *testPlane) newPeerWith(pp signal.PeerParams) *testPeer {
	tp.t.Helper()
	sink := &signaltest.Sink{}
	mp, err := tp.plane.NewPeer(pp, sink)
	if err != nil {
		tp.t.Fatalf("NewPeer: %v", err)
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return &testPeer{
		MediaPeer: mp, peer: mp.(*peer), sink: sink,
		conn: tp.conns[len(tp.conns)-1], join: tp.joins[len(tp.joins)-1],
	}
}

// ---- the SFU's declarations, read from its source ----

// sfuConst is one string constant of package sfu.
type sfuConst struct {
	Name  string
	Type  string // "" for an untyped constant
	Value string
}

// sfuFile parses a file of package sfu. The mapping tables of 01 §15.4 are complete only if they cover everything
// the SFU declares, and Go has no way to list a package's constants at run time: so the tests read the declarations
// and fail when one has no row here.
func sfuFile(t *testing.T, name string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "sfu", name), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("read the SFU's declarations: %v", err)
	}
	return f
}

// sfuConsts returns the string constants that a file of package sfu declares.
func sfuConsts(t *testing.T, file string) []sfuConst {
	t.Helper()
	var out []sfuConst
	for _, decl := range sfuFile(t, file).Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			typ := ""
			if id, ok := vs.Type.(*ast.Ident); ok {
				typ = id.Name
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: constant %s: %v", file, name.Name, err)
				}
				out = append(out, sfuConst{Name: name.Name, Type: typ, Value: v})
			}
		}
	}
	return out
}

// sfuConstsOf returns the values of the constants of one type that a file of package sfu declares.
func sfuConstsOf(t *testing.T, file, typ string) []string {
	t.Helper()
	var out []string
	for _, c := range sfuConsts(t, file) {
		if c.Type == typ {
			out = append(out, c.Value)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no constant of type %s: has it moved?", file, typ)
	}
	return out
}
