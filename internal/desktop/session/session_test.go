package session

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// 03 §22.1's session row: the core.Auth adapter (bound to its origin's Manager, force passed on, ErrNotLinked as
// RelinkRequired, ErrIncompatible closing the core, SessionEnded and HelloRefused handed to 02) and when the session
// builds a new core client (a cured stop, a relink, a server switch).

// relinkRequired and updateRequired walk err's chain as 01's core does (authStopFor).
func relinkRequired(err error) bool {
	var r interface{ RelinkRequired() bool }
	return errors.As(err, &r) && r.RelinkRequired()
}

func updateRequired(err error) bool {
	var u interface{ UpdateRequired() bool }
	return errors.As(err, &u) && u.UpdateRequired()
}

// TestBuildsForActiveLinkedServer: a core client for the active linked server, with the app's hello client, the
// engine and the adapter; Ready returns it once online, and ConnectionID is its connection id.
func TestBuildsForActiveLinkedServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		e := newEnv(t, m, nil)
		c, origin, err := e.sess.Ready(context.Background())
		if err != nil || origin != originA {
			t.Fatalf("Ready = %v, %q, %v", c, origin, err)
		}
		if e.fac.count() != 1 {
			t.Fatalf("%d clients", e.fac.count())
		}
		o := e.fac.get(0).opts
		if o.ServerURL != originA || o.Engine != e.eng || o.Client.Kind != protocol.ClientKindDesktop ||
			o.Client.Version != "0.2.0" || o.Auth == nil || o.Header != nil {
			t.Errorf("core options %+v", o)
		}
		if got := e.sess.ConnectionID(); got != "c_a" {
			t.Errorf("ConnectionID %q", got)
		}
	})
}

// TestNotLinked: no server, an unlinked one or an incompatible one: no core client, and Ready fails at once.
func TestNotLinked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, nil, nil)
		start := time.Now()
		if _, _, err := e.sess.Ready(context.Background()); !errors.Is(err, deviceauth.ErrNotLinked) {
			t.Errorf("Ready without a server: %v", err)
		}
		m := newManager(originA, deviceauth.StatusUnlinked)
		e.srv.setActive(m)
		e.sess.Refresh()
		synctest.Wait()
		if _, _, err := e.sess.Ready(context.Background()); !errors.Is(err, deviceauth.ErrNotLinked) {
			t.Errorf("Ready unlinked: %v", err)
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusIncompatible })
		if _, _, err := e.sess.Ready(context.Background()); !errors.Is(err, deviceauth.ErrIncompatible) {
			t.Errorf("Ready incompatible: %v", err)
		}
		if time.Since(start) != 0 || e.fac.count() != 0 || e.sess.ConnectionID() != "" {
			t.Errorf("waited %v, %d clients", time.Since(start), e.fac.count())
		}
	})
}

// TestReadyWaitsForOnline: Ready waits for the core client's online status, and ends with its ctx.
func TestReadyWaitsForOnline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		online := make(chan struct{})
		e := newEnv(t, newManager(originA, deviceauth.StatusLinked), func(int) func(context.Context, *fakeClient) error {
			return func(ctx context.Context, c *fakeClient) error {
				select {
				case <-online:
					c.goOnline()
				case <-ctx.Done():
				}
				c.waitClosed(ctx)
				return nil
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, _, err := e.sess.Ready(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Ready while connecting: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := e.sess.Ready(context.Background())
			done <- err
		}()
		time.Sleep(3 * time.Second)
		close(online)
		if err := <-done; err != nil {
			t.Fatalf("Ready: %v", err)
		}
	})
}

// TestAdapterBoundToItsOrigin: after the active server changes, the old core is closed and a new one built for the
// new server; the old core's adapter still asks only the old server's Manager, never returning the new server's
// token.
func TestAdapterBoundToItsOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newManager(originA, deviceauth.StatusLinked)
		b := newManager(originB, deviceauth.StatusLinked)
		e := newEnv(t, a, nil)
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.srv.setActive(b)
		e.sess.Refresh()
		synctest.Wait()
		time.Sleep(RebuildEvery)
		c, origin, err := e.sess.Ready(context.Background())
		if err != nil || origin != originB || e.fac.count() != 2 {
			t.Fatalf("Ready after the switch = %v, %q, %v (%d clients)", c, origin, err, e.fac.count())
		}
		old, cur := e.fac.get(0), e.fac.get(1)
		if old.closeCount() == 0 || cur.opts.ServerURL != originB {
			t.Fatalf("old closed %d, new for %s", old.closeCount(), cur.opts.ServerURL)
		}
		tok, err := old.opts.Auth.AccessToken(context.Background(), false)
		if err != nil || tok != protocol.Secret("isa_"+originA) {
			t.Errorf("the old core's token %q, %v", tok, err)
		}
		tok, err = cur.opts.Auth.AccessToken(context.Background(), false)
		if err != nil || tok != protocol.Secret("isa_"+originB) {
			t.Errorf("the new core's token %q, %v", tok, err)
		}
		aCalls, _, _ := a.calls()
		bCalls, _, _ := b.calls()
		if len(aCalls) != 1 || len(bCalls) != 1 {
			t.Errorf("A asked %d times, B %d times", len(aCalls), len(bCalls))
		}
	})
}

// TestAccessTokenForce: force reaches the Manager's AccessToken (02's own forced-refresh rule).
func TestAccessTokenForce(t *testing.T) {
	m := newManager(originA, deviceauth.StatusLinked)
	auth := NewAuth(m)
	if _, err := auth.AccessToken(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AccessToken(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls, _, _ := m.calls(); !slices.Equal(calls, []bool{true, false}) {
		t.Errorf("force %v", calls)
	}
}

// TestAccessTokenErrors: ErrNotLinked comes back with RelinkRequired, ErrIncompatible with UpdateRequired, and any
// other error (ErrOffline) is transient: neither method.
func TestAccessTokenErrors(t *testing.T) {
	m := newManager(originA, deviceauth.StatusLinked)
	auth := NewAuth(m)
	for _, c := range []struct {
		err            error
		relink, update bool
	}{
		{deviceauth.ErrNotLinked, true, false},
		{deviceauth.ErrIncompatible, false, true},
		{deviceauth.ErrOffline, false, false},
		{context.DeadlineExceeded, false, false},
	} {
		m.set(func(m *fakeManager) { m.tokenErr = c.err })
		_, err := auth.AccessToken(context.Background(), false)
		if !errors.Is(err, c.err) || relinkRequired(err) != c.relink || updateRequired(err) != c.update {
			t.Errorf("%v → %v (relink %v, update %v)", c.err, err, relinkRequired(err), updateRequired(err))
		}
	}
}

// TestIncompatibleClosesCore: an ErrIncompatible from the Manager closes the core at once; no new core is built
// while the server stays incompatible; once 02's Recheck says linked again, a new one is.
func TestIncompatibleClosesCore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		ask := make(chan struct{})
		got := make(chan error, 1)
		e := newEnv(t, m, func(n int) func(context.Context, *fakeClient) error {
			return func(ctx context.Context, c *fakeClient) error {
				c.goOnline()
				if n == 0 {
					select {
					case <-ask: // the core's next hello asks for a token
						_, err := c.opts.Auth.AccessToken(ctx, false)
						got <- err
					case <-ctx.Done():
					}
				}
				c.waitClosed(ctx)
				return nil
			}
		})
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.set(func(m *fakeManager) {
			m.state.Status = deviceauth.StatusIncompatible
			m.tokenErr = deviceauth.ErrIncompatible
		})
		close(ask)
		if err := <-got; !updateRequired(err) {
			t.Fatalf("AccessToken: %v", err)
		}
		synctest.Wait()
		if e.fac.get(0).closeCount() == 0 {
			t.Fatal("the core wasn't closed")
		}
		e.sess.OnAuthState(m.State())
		time.Sleep(time.Minute)
		if e.fac.count() != 1 {
			t.Fatalf("%d clients while incompatible", e.fac.count())
		}
		m.set(func(m *fakeManager) {
			m.state.Status = deviceauth.StatusLinked
			m.tokenErr = nil
		})
		e.sess.OnAuthState(m.State())
		if _, _, err := e.sess.Ready(context.Background()); err != nil || e.fac.count() != 2 {
			t.Fatalf("after the recheck: %v, %d clients", err, e.fac.count())
		}
	})
}

// sessionStop is a client script: online, then at stop's signal SessionEnded with code (as the core's terminate
// does) and the stop as Run's error. Later clients run the default.
func sessionStop(stop <-chan struct{}, code protocol.ErrorCode, se *core.StopError) func(int) func(context.Context,
	*fakeClient) error {
	return func(n int) func(context.Context, *fakeClient) error {
		if n > 0 {
			return nil
		}
		return func(ctx context.Context, c *fakeClient) error {
			c.goOnline()
			select {
			case <-stop:
			case <-ctx.Done():
				return nil
			}
			c.setStatus(core.Status{Conn: core.ConnStopped, Stop: se})
			if code != "" {
				c.opts.Auth.SessionEnded(ctx, code)
			}
			return se
		}
	}
}

// TestSessionRevokedStaysLinked: a session_revoked stop reaches 02's SessionEnded; when 02 stays linked, a new core
// client is built at once.
func TestSessionRevokedStaysLinked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		stop := make(chan struct{})
		e := newEnv(t, m, sessionStop(stop, protocol.ErrorCodeSessionRevoked,
			&core.StopError{Reason: core.StopUnlinked, Code: protocol.ErrorCodeSessionRevoked}))
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		close(stop)
		synctest.Wait()
		if _, sessions, _ := m.calls(); !slices.Equal(sessions, []protocol.ErrorCode{protocol.ErrorCodeSessionRevoked}) {
			t.Fatalf("SessionEnded %v", sessions)
		}
		if e.fac.count() != 2 {
			t.Fatalf("%d clients after a cured stop", e.fac.count())
		}
		if _, origin, err := e.sess.Ready(context.Background()); err != nil || origin != originA {
			t.Fatalf("Ready = %q, %v", origin, err)
		}
	})
}

// TestSessionRevokedRelink: when 02's refresh got invalid_grant (unlinked), no core is built until the app is
// linked again; then a new core client is.
func TestSessionRevokedRelink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		m.afterEnd = &deviceauth.State{Origin: originA, Status: deviceauth.StatusUnlinked,
			Notice: deviceauth.NoticeSignedOut}
		stop := make(chan struct{})
		e := newEnv(t, m, sessionStop(stop, protocol.ErrorCodeAccountDisabled,
			&core.StopError{Reason: core.StopDisabled, Code: protocol.ErrorCodeAccountDisabled}))
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(stop)
		synctest.Wait()
		e.sess.OnAuthState(m.State()) // 02 tells the app it is unlinked
		time.Sleep(time.Minute)
		if e.fac.count() != 1 {
			t.Fatalf("%d clients while unlinked", e.fac.count())
		}
		if _, _, err := e.sess.Ready(context.Background()); !errors.Is(err, deviceauth.ErrNotLinked) {
			t.Errorf("Ready while unlinked: %v", err)
		}
		m.set(func(m *fakeManager) { m.state = deviceauth.State{Origin: originA, Status: deviceauth.StatusLinked} })
		e.sess.OnAuthState(m.State()) // the relink
		if _, _, err := e.sess.Ready(context.Background()); err != nil || e.fac.count() != 2 {
			t.Fatalf("after the relink: %v, %d clients", err, e.fac.count())
		}
		if e.fac.get(1).opts.Auth == e.fac.get(0).opts.Auth {
			t.Error("the new core shares the old adapter")
		}
	})
}

// TestUnlinkedByAuthError: a stop the core made from the adapter's RelinkRequired (no server code) waits for the
// relink too, without a SessionEnded.
func TestUnlinkedByAuthError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		stop := make(chan struct{})
		e := newEnv(t, m, sessionStop(stop, "", &core.StopError{Reason: core.StopUnlinked}))
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusUnlinked })
		close(stop)
		synctest.Wait()
		time.Sleep(time.Minute)
		if _, sessions, _ := m.calls(); e.fac.count() != 1 || len(sessions) != 0 {
			t.Fatalf("%d clients, SessionEnded %v", e.fac.count(), sessions)
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusLinked })
		e.sess.OnAuthState(m.State())
		synctest.Wait()
		if e.fac.count() != 2 {
			t.Fatalf("%d clients after the relink", e.fac.count())
		}
	})
}

// TestHelloRefused: an update_required stop hands the server's refusal to 02's HelloRefused (compat.FromError, then
// the version screen) and builds nothing until 02's state changes.
func TestHelloRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		refusal := &protocol.Error{Code: protocol.ErrorCodeClientOutdated,
			Params: map[string]any{"minClientVersion": "0.3.0"}}
		stop := make(chan struct{})
		e := newEnv(t, m, sessionStop(stop, "", &core.StopError{Reason: core.StopUpdateRequired,
			Code: protocol.ErrorCodeClientOutdated, Err: refusal}))
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(stop)
		synctest.Wait()
		if _, _, hellos := m.calls(); len(hellos) != 1 || hellos[0] != refusal {
			t.Fatalf("HelloRefused %v", hellos)
		}
		time.Sleep(time.Minute)
		if e.fac.count() != 1 {
			t.Fatalf("%d clients after the refusal", e.fac.count())
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusIncompatible })
		e.sess.OnAuthState(m.State())
		synctest.Wait()
		if e.fac.count() != 1 {
			t.Fatalf("%d clients while incompatible", e.fac.count())
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusLinked }) // a later recheck says ok
		e.sess.OnAuthState(m.State())
		synctest.Wait()
		if e.fac.count() != 2 {
			t.Fatalf("%d clients after the recheck", e.fac.count())
		}
	})
}

// TestOtherStopsWait: too_many_connections, replaced and bad_message aren't 02's to cure: nothing is built until
// something changes.
func TestOtherStopsWait(t *testing.T) {
	for _, r := range []core.StopReason{core.StopTooMany, core.StopReplaced, core.StopProtocol} {
		t.Run(string(r), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := newManager(originA, deviceauth.StatusLinked)
				stop := make(chan struct{})
				e := newEnv(t, m, sessionStop(stop, "", &core.StopError{Reason: r}))
				if _, _, err := e.sess.Ready(context.Background()); err != nil {
					t.Fatal(err)
				}
				close(stop)
				synctest.Wait()
				time.Sleep(time.Minute)
				if e.fac.count() != 1 {
					t.Fatalf("%d clients", e.fac.count())
				}
				if _, _, hellos := m.calls(); len(hellos) != 0 {
					t.Errorf("HelloRefused %v", hellos)
				}
				e.sess.Refresh()
				synctest.Wait()
				if e.fac.count() != 2 {
					t.Fatalf("%d clients after Refresh", e.fac.count())
				}
			})
		})
	}
}

// TestUnlinkClosesCore: the active server unlinked (signed out, removed) closes its core.
func TestUnlinkClosesCore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		e := newEnv(t, m, nil)
		if _, _, err := e.sess.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.set(func(m *fakeManager) { m.state.Status = deviceauth.StatusUnlinked })
		e.sess.OnAuthState(m.State())
		synctest.Wait()
		if e.fac.get(0).closeCount() == 0 || e.sess.ConnectionID() != "" {
			t.Fatal("the unlinked server's core runs on")
		}
		e.srv.setActive(nil) // removed
		e.sess.Refresh()
		synctest.Wait()
		if e.fac.count() != 1 {
			t.Errorf("%d clients", e.fac.count())
		}
	})
}

// TestRebuildRate: a core that stops at once with a cured stop is rebuilt at most once per RebuildEvery.
func TestRebuildRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(originA, deviceauth.StatusLinked)
		e := newEnv(t, m, func(int) func(context.Context, *fakeClient) error {
			return func(ctx context.Context, c *fakeClient) error {
				c.opts.Auth.SessionEnded(ctx, protocol.ErrorCodeSessionRevoked)
				return &core.StopError{Reason: core.StopUnlinked, Code: protocol.ErrorCodeSessionRevoked}
			}
		})
		time.Sleep(5*RebuildEvery + RebuildEvery/2)
		if n := e.fac.count(); n < 5 || n > 6 {
			t.Fatalf("%d clients in 5.5 s", n)
		}
	})
}

// TestNewAndRun: New's required options; Run runs once.
func TestNewAndRun(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without Servers")
	}
	if _, err := New(Options{Servers: &fakeServers{}}); err == nil {
		t.Error("New without an engine")
	}
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, nil, nil)
		synctest.Wait()
		if err := e.sess.Run(context.Background()); err == nil {
			t.Error("a second Run")
		}
	})
}

// TestRealCore: without Options.NewClient the session runs 01's real core client: here against a loopback port
// nothing listens on, so it stays offline (Ready ends with its ctx), and Run returns once ctx ends, leaving nothing
// running (TestMain's goleak).
func TestRealCore(t *testing.T) {
	m := newManager("http://127.0.0.1:1", deviceauth.StatusLinked)
	eng := fake.NewEngine(fake.EngineConfig{})
	defer func() { _ = eng.Close() }()
	s, err := New(Options{Servers: &fakeServers{active: m}, Engine: eng, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	rctx, rcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer rcancel()
	if _, _, err := s.Ready(rctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ready: %v", err)
	}
	if s.ConnectionID() != "" {
		t.Error("a connection id while offline")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
	if calls, _, _ := m.calls(); len(calls) == 0 {
		t.Error("the core never asked for a token")
	}
}

// TestServersOf: no listed server is a nil Manager, not a typed nil.
func TestServersOf(t *testing.T) {
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: t.TempDir(), Store: memStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if m := ServersOf(srv).Active(); m != nil {
		t.Errorf("Active = %v", m)
	}
}
