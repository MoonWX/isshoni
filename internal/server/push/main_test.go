package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// TestMain fails the run when goroutines outlive the tests (the HTTP client's and the test servers' get 2 s to
// settle after Close).
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		err := goleak.Find()
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
			err = goleak.Find()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// newVAPID returns a fresh VAPID pair in the form of config.SecretStore.VAPID.
func newVAPID(t testing.TB) config.VAPIDKeys {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return config.VAPIDKeys{Public: b64(k.PublicKey().Bytes()), Private: logx.Secret(b64(k.Bytes()))}
}

// fakeStore is a Store in memory. It records every call.
type fakeStore struct {
	mu sync.Mutex

	subs       []Subscription                         // what Recipients returns ...
	recipients func(f RecipientFilter) []Subscription // ... unless this is set
	meta       map[string]string                      // the meta table
	errs       map[string]error                       // by method name: the error it returns
	deleteAllN int                                    // DeleteAll's count
	pruneN     int                                    // Prune's count

	calls    []string // method names, in call order
	filters  []RecipientFilter
	results  []recordedResult
	deleted  []string
	prunes   []pruneCall
	purges   int // DeleteAll calls
	metaSets []string
}

type recordedResult struct {
	ID string
	OK bool
	At time.Time
}

type pruneCall struct {
	MinFailures    int
	NoSuccessSince time.Time
}

func newFakeStore(subs ...Subscription) *fakeStore {
	return &fakeStore{subs: subs, meta: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeStore) call(name string) error {
	f.calls = append(f.calls, name)
	return f.errs[name]
}

func (f *fakeStore) Recipients(_ context.Context, flt RecipientFilter) ([]Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Recipients"); err != nil {
		return nil, err
	}
	flt.ExcludeUserIDs = slices.Clone(flt.ExcludeUserIDs)
	f.filters = append(f.filters, flt)
	if f.recipients != nil {
		return f.recipients(flt), nil
	}
	return slices.Clone(f.subs), nil
}

func (f *fakeStore) RecordResult(_ context.Context, id string, ok bool, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("RecordResult"); err != nil {
		return err
	}
	f.results = append(f.results, recordedResult{ID: id, OK: ok, At: at})
	return nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Delete"); err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeStore) DeleteAll(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteAll"); err != nil {
		return 0, err
	}
	f.purges++
	return f.deleteAllN, nil
}

func (f *fakeStore) Prune(_ context.Context, minFailures int, noSuccessSince time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Prune"); err != nil {
		return 0, err
	}
	f.prunes = append(f.prunes, pruneCall{minFailures, noSuccessSince})
	return f.pruneN, nil
}

func (f *fakeStore) Meta(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Meta"); err != nil {
		return "", err
	}
	return f.meta[key], nil
}

func (f *fakeStore) SetMeta(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SetMeta"); err != nil {
		return err
	}
	f.meta[key] = value
	f.metaSets = append(f.metaSets, key+"="+value)
	return nil
}

// snapshot returns a copy of what the store recorded.
func (f *fakeStore) snapshot() fakeStore {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeStore{
		calls:    slices.Clone(f.calls),
		filters:  slices.Clone(f.filters),
		results:  slices.Clone(f.results),
		deleted:  slices.Clone(f.deleted),
		prunes:   slices.Clone(f.prunes),
		purges:   f.purges,
		metaSets: slices.Clone(f.metaSets),
	}
}

// sendCall is one call of fakeSender.Send.
type sendCall struct {
	Sub     Subscription
	Payload string
	Opts    SendOptions
	At      time.Time
}

// fakeSender is a Sender that answers from respond (nil: 201 Created) and records every call.
type fakeSender struct {
	mu      sync.Mutex
	calls   []sendCall
	respond func(n int, c sendCall) (SendResult, error) // n counts this subscription's calls from 0
	hang    bool                                        // Send blocks until its context ends
	perSub  map[string]int
}

func (f *fakeSender) Send(ctx context.Context, sub Subscription, payload []byte, o SendOptions) (SendResult, error) {
	f.mu.Lock()
	c := sendCall{Sub: sub, Payload: string(payload), Opts: o, At: time.Now()}
	f.calls = append(f.calls, c)
	if f.perSub == nil {
		f.perSub = map[string]int{}
	}
	n := f.perSub[sub.ID]
	f.perSub[sub.ID]++
	respond, hang := f.respond, f.hang
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return SendResult{}, ctx.Err()
	}
	if respond == nil {
		return SendResult{Status: 201}, nil
	}
	return respond(n, c)
}

func (f *fakeSender) sent() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// status makes a respond function that always answers with one status.
func status(code int) func(int, sendCall) (SendResult, error) {
	return func(int, sendCall) (SendResult, error) { return SendResult{Status: code}, nil }
}

// fakeResolver resolves the names it knows (lower case) and fails for the others. It records the lookups.
type fakeResolver struct {
	mu      sync.Mutex
	hosts   map[string][]string
	lookups []string
	hang    bool // LookupNetIP blocks until its context ends
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.lookups = append(r.lookups, host)
	list, ok := r.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
	hang := r.hang
	r.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if network != "ip" {
		return nil, fmt.Errorf("fakeResolver: network %q, want ip", network)
	}
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]netip.Addr, len(list))
	for i, s := range list {
		out[i] = netip.MustParseAddr(s)
	}
	return out, nil
}

func (r *fakeResolver) lookedUp() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lookups)
}

// lockedBuffer is a bytes.Buffer for a logger that several goroutines write to.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// testSubject is the VAPID subject of the test services.
const testSubject = "mailto:admin@example.com"

// fixture is a Service on fakes.
type fixture struct {
	*Service
	store  *fakeStore
	sender *fakeSender
	logs   *lockedBuffer
	keys   config.VAPIDKeys
}

// newFixture builds a Service on a fakeStore and a fakeSender; mod may change the options before New.
func newFixture(t testing.TB, mod func(*Options), subs ...Subscription) *fixture {
	t.Helper()
	f := &fixture{store: newFakeStore(subs...), sender: &fakeSender{}, logs: &lockedBuffer{}, keys: newVAPID(t)}
	opts := Options{
		Enabled: true,
		VAPID:   func() config.VAPIDKeys { return f.keys },
		Subject: testSubject,
		Store:   f.store,
		Sender:  f.sender,
		Logger:  slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mod != nil {
		mod(&opts)
	}
	s, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.Service = s
	return f
}

// start runs the Service in a goroutine and returns the function that stops it and waits for Run.
func (f *fixture) start(t testing.TB) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// sent returns the counter of one result.
func (s *Service) sent(r result) uint64 { return s.counts[r].Load() }

// sub builds a subscription of a user at push.example.com. The token stands for the secret part of the endpoint.
func sub(id, userID string) Subscription {
	return Subscription{
		ID: id, UserID: userID, SessionID: "sess-" + id,
		Endpoint: logx.Secret("https://push.example.com/send/TOKEN-" + id),
		P256dh:   "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
		Auth:     "tBHItJI5svbpez7KI4CCXg",
	}
}
