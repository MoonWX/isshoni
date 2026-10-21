package deviceauth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The method of 01-client-core's core.Auth that *deviceauth.Manager provides itself. core.Auth's other method,
// SessionEnded(ctx, code) without a result, is 03's session adapter's: Manager.SessionEnded returns the State the
// SPA's binding needs (02 §11.3), so the adapter wraps it.
var _ interface {
	AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)
} = (*deviceauth.Manager)(nil)

// The error values answer what the core reads from an Auth error's chain (02 §11.3 "Errors").
func TestErrorMethods(t *testing.T) {
	if !relinkRequired(deviceauth.ErrNotLinked) || updateRequired(deviceauth.ErrNotLinked) {
		t.Error("ErrNotLinked: want RelinkRequired only")
	}
	if !updateRequired(deviceauth.ErrIncompatible) || relinkRequired(deviceauth.ErrIncompatible) {
		t.Error("ErrIncompatible: want UpdateRequired only")
	}
	cause := errors.New("cause")
	off := errors.Join(deviceauth.ErrOffline, cause)
	if relinkRequired(off) || updateRequired(off) {
		t.Error("ErrOffline must be transient")
	}
	wrapped := errors.New("x")
	if !errors.Is(errors.Join(wrapped, deviceauth.ErrNotLinked), deviceauth.ErrNotLinked) {
		t.Error("ErrNotLinked isn't comparable")
	}
}

// No credential: AccessToken answers ErrNotLinked with RelinkRequired, without a request.
func TestAccessTokenNotLinked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		_, err := e.m.AccessToken(t.Context(), false)
		if !errors.Is(err, deviceauth.ErrNotLinked) || !relinkRequired(err) {
			t.Fatalf("AccessToken = %s, want ErrNotLinked", describe(err))
		}
		if n := len(e.fs.seen("")); n != 0 {
			t.Fatalf("%d requests", n)
		}
	})
}

// Start: /info, then one refresh; the rotated token is stored before the new access token is handed out.
func TestStartRefreshes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		var returned atomic.Bool
		e.store.onPut = func(credstore.Credential) {
			if returned.Load() {
				t.Error("the token was returned before the credential was written")
			}
		}
		must(t, e.m.Start(t.Context()))
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		if got := storedToken(t, e.store, testOrigin); got != refreshToken(1) {
			t.Fatalf("stored %q, want the rotated token", got)
		}
		refs := e.fs.refreshes()
		if len(refs) != 1 || refs[0].RefreshToken != seedToken || len(refs[0].ExchangeID) != 22 {
			t.Fatalf("refreshes %+v", refs)
		}
		tok, err := e.m.AccessToken(t.Context(), false)
		returned.Store(true)
		must(t, err)
		if tok.Reveal() != accessToken(1) || len(e.fs.refreshes()) != 1 {
			t.Fatalf("AccessToken = %q after %d refreshes", tok.Reveal(), len(e.fs.refreshes()))
		}
		if s := e.m.State(); s.User == nil || s.Device == nil || s.ServerName != "Alex's server" {
			t.Fatalf("state %+v", s)
		}
	})
}

// Without Start, AccessToken loads the credential and refreshes; the credential is written before the token is
// returned (an ordering store), and the token has at least 120 s left.
func TestAccessTokenWritesTheCredentialFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		var returned atomic.Bool
		e.store.onPut = func(c credstore.Credential) {
			if returned.Load() {
				t.Error("AccessToken returned before the credential was written")
			}
			if c.RefreshToken != refreshToken(1) {
				t.Errorf("Put %q, want the rotated token", c.RefreshToken)
			}
		}
		tok, exp, err := e.m.AccessTokenWithExpiry(t.Context(), false)
		returned.Store(true)
		must(t, err)
		if tok.Reveal() != accessToken(1) || time.Until(exp) < 120*time.Second {
			t.Fatalf("AccessTokenWithExpiry = %q, %v", tok.Reveal(), time.Until(exp))
		}
		if e.store.puts.Load() != 1 {
			t.Fatalf("%d Puts", e.store.puts.Load())
		}
		// Below 120 s left, the next call refreshes.
		e.store.onPut = nil
		time.Sleep(781 * time.Second)
		tok, err = e.m.AccessToken(t.Context(), false)
		must(t, err)
		if tok.Reveal() != accessToken(2) {
			t.Fatalf("AccessToken = %q, want a refreshed token", tok.Reveal())
		}
	})
}

// 100 concurrent callers share one refresh.
func TestAccessTokenSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		var wg sync.WaitGroup
		tokens := make([]string, 100)
		errs := make([]error, 100)
		for i := range 100 {
			wg.Go(func() {
				tok, err := e.m.AccessToken(t.Context(), false)
				tokens[i], errs[i] = tok.Reveal(), err
			})
		}
		wg.Wait()
		for i := range 100 {
			if errs[i] != nil || tokens[i] != accessToken(1) {
				t.Fatalf("caller %d: %q, %v", i, tokens[i], errs[i])
			}
		}
		if n := len(e.fs.refreshes()); n != 1 {
			t.Fatalf("%d refreshes for 100 callers, want 1", n)
		}
	})
}

// Retries: network errors, 5xx, server_busy and a 429 are retried after 1, 2, 4, … s (±20 %; a 429 after its
// retryAfter) with the same exchange id; a new logical refresh draws a new one.
func TestRefreshRetriesKeepTheExchangeID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		e.fs.with(func(fs *fakeServer) {
			fs.refresh = func(_ api.DeviceTokenRequest, n int) reply {
				switch n {
				case 0:
					return apiErr(api.CodeServerBusy, nil)
				case 1:
					return reply{status: http.StatusBadGateway, body: []byte("<html>proxy</html>")}
				case 2:
					r := apiErr(api.CodeRateLimited, nil)
					r.body = api.ErrorResponse{Error: api.Error{Code: api.CodeRateLimited, RetryAfter: 9}}
					return r
				case 3:
					return apiErr(api.CodeInternal, nil)
				}
				return ok(fs.pairLocked())
			}
		})
		start := time.Now()
		_, err := e.m.AccessToken(t.Context(), false)
		if !errors.Is(err, deviceauth.ErrOffline) || !isAPI(err, api.CodeServerBusy) || relinkRequired(err) {
			t.Fatalf("AccessToken = %s, want ErrOffline wrapping server_busy", describe(err))
		}
		wantStatus(t, e.m, deviceauth.StatusOffline, deviceauth.NoticeNone)
		// During the retry series a caller gets the answer at once, without a request of its own.
		_, err = e.m.AccessToken(t.Context(), false)
		if !errors.Is(err, deviceauth.ErrOffline) {
			t.Fatalf("second AccessToken = %s", describe(err))
		}
		time.Sleep(time.Minute)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		refs := e.fs.refreshes()
		if len(refs) != 5 {
			t.Fatalf("%d refresh attempts, want 5", len(refs))
		}
		for _, r := range refs {
			if r.ExchangeID != refs[0].ExchangeID || r.RefreshToken != seedToken {
				t.Fatalf("attempt %+v differs from the first one", r)
			}
		}
		at := e.fs.seen("/api/v1/device/token")
		gaps := []struct{ lo, hi time.Duration }{
			{800 * time.Millisecond, 1200 * time.Millisecond},  // 1 s ± 20 %
			{1600 * time.Millisecond, 2400 * time.Millisecond}, // 2 s ± 20 %
			{9 * time.Second, 9 * time.Second},                 // retryAfter
			{6400 * time.Millisecond, 9600 * time.Millisecond}, // 8 s ± 20 %
		}
		if len(at) != len(gaps)+1 || at[0].At != start {
			t.Fatalf("%d attempts, the first after %v", len(at), at[0].At.Sub(start))
		}
		for i, g := range gaps {
			if d := at[i+1].At.Sub(at[i].At); d < g.lo || d > g.hi {
				t.Errorf("retry %d after %v, want %v–%v", i+1, d, g.lo, g.hi)
			}
		}
		tok, err := e.m.AccessToken(t.Context(), false)
		must(t, err)
		if tok.Reveal() != accessToken(1) {
			t.Fatalf("AccessToken = %q", tok.Reveal())
		}
		_, err = e.m.AccessToken(t.Context(), true)
		must(t, err)
		refs = e.fs.refreshes()
		if last := refs[len(refs)-1]; last.ExchangeID == refs[0].ExchangeID || last.RefreshToken != refreshToken(1) {
			t.Fatalf("the next logical refresh reused exchange id or token: %+v", last)
		}
	})
}

// While a refresh is retried, a cached token with more than 10 s left is still handed out; after that ErrOffline
// (wrapping the network error) and offline. The retries stop at invalid_grant.
func TestRefreshRetriesWithAUsableToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		tok, err := e.m.AccessToken(t.Context(), false)
		must(t, err)
		time.Sleep(800 * time.Second) // 100 s left: below 120 s
		e.fs.ln.down.Store(true)
		got, err := e.m.AccessToken(t.Context(), false)
		if err != nil || got != tok {
			t.Fatalf("AccessToken during retries = %q, %s; want the cached token", got.Reveal(), describe(err))
		}
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		time.Sleep(91 * time.Second) // 9 s left
		_, err = e.m.AccessToken(t.Context(), false)
		if !errors.Is(err, deviceauth.ErrOffline) || !errors.Is(err, deviceauth.ErrNetwork) {
			t.Fatalf("AccessToken = %s, want ErrOffline wrapping ErrNetwork", describe(err))
		}
		wantStatus(t, e.m, deviceauth.StatusOffline, deviceauth.NoticeNone)

		e.fs.with(func(fs *fakeServer) {
			fs.refresh = func(api.DeviceTokenRequest, int) reply { return apiErr(api.CodeInvalidGrant, nil) }
		})
		e.fs.ln.down.Store(false)
		time.Sleep(time.Minute)
		wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeSignedOut)
		n := len(e.fs.refreshes())
		time.Sleep(10 * time.Minute)
		if len(e.fs.refreshes()) != n {
			t.Fatal("retries went on after invalid_grant")
		}
	})
}

// invalid_grant at a refresh: the credential is deleted before the callers (all of the single flight's) get an
// error wrapping ErrNotLinked; the notice says why.
func TestRefreshInvalidGrant(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		notice deviceauth.Notice
	}{
		"signed out": {nil, deviceauth.NoticeSignedOut},
		"reused":     {map[string]any{api.ParamReason: api.InvalidGrantReused}, deviceauth.NoticeRefreshReused},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, true)
				e.fs.with(func(fs *fakeServer) {
					fs.refresh = func(api.DeviceTokenRequest, int) reply { return apiErr(api.CodeInvalidGrant, tc.params) }
				})
				var wg sync.WaitGroup
				for range 5 {
					wg.Go(func() {
						_, err := e.m.AccessToken(t.Context(), false)
						if !errors.Is(err, deviceauth.ErrNotLinked) || !relinkRequired(err) {
							t.Errorf("AccessToken = %s, want ErrNotLinked", describe(err))
						}
						if got := storedToken(t, e.store, testOrigin); got != "" {
							t.Error("the credential was still stored when AccessToken returned")
						}
					})
				}
				wg.Wait()
				wantStatus(t, e.m, deviceauth.StatusUnlinked, tc.notice)
				if s := e.m.State(); s.User != nil || s.Device != nil {
					t.Fatalf("user or device kept: %+v", s)
				}
				if n := len(e.fs.refreshes()); n != 1 {
					t.Fatalf("%d refreshes", n)
				}
				if _, err := e.m.AccessToken(t.Context(), false); !errors.Is(err, deviceauth.ErrNotLinked) {
					t.Fatalf("later AccessToken = %s", describe(err))
				}
			})
		})
	}
}

// AccessToken(ctx, true) refreshes even with a fresh token; a second forced call within 10 s gets that refresh's
// token without a request; one after 10 s refreshes again.
func TestForcedRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		first, err := e.m.AccessToken(t.Context(), false)
		must(t, err)
		forced, err := e.m.AccessToken(t.Context(), true)
		must(t, err)
		if forced == first || len(e.fs.refreshes()) != 2 {
			t.Fatalf("forced = %q after %d refreshes; want a new token", forced.Reveal(), len(e.fs.refreshes()))
		}
		time.Sleep(9 * time.Second)
		again, err := e.m.AccessToken(t.Context(), true)
		must(t, err)
		if again != forced || len(e.fs.refreshes()) != 2 {
			t.Fatalf("a forced call within 10 s = %q after %d refreshes; want the same token, no request",
				again.Reveal(), len(e.fs.refreshes()))
		}
		time.Sleep(2 * time.Second)
		later, err := e.m.AccessToken(t.Context(), true)
		must(t, err)
		if later == forced || len(e.fs.refreshes()) != 3 {
			t.Fatalf("a forced call after 10 s = %q after %d refreshes; want a new token", later.Reveal(),
				len(e.fs.refreshes()))
		}
		// Concurrent forced calls share one refresh.
		time.Sleep(11 * time.Second)
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				if _, err := e.m.AccessToken(t.Context(), true); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if n := len(e.fs.refreshes()); n != 4 {
			t.Fatalf("%d refreshes after 10 concurrent forced calls, want 4", n)
		}
	})
}

// At least one refresh a day while the manager runs, without any caller (02 §11.4).
func TestDailyRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		must(t, e.m.Start(t.Context()))
		time.Sleep(24*time.Hour - time.Second)
		if n := len(e.fs.refreshes()); n != 1 {
			t.Fatalf("%d refreshes before a day passed, want 1", n)
		}
		time.Sleep(2 * time.Second)
		if n := len(e.fs.refreshes()); n != 2 {
			t.Fatalf("%d refreshes after a day, want 2", n)
		}
		time.Sleep(24 * time.Hour)
		if n := len(e.fs.refreshes()); n != 3 {
			t.Fatalf("%d refreshes after two days, want 3", n)
		}
		if got := storedToken(t, e.store, testOrigin); got != refreshToken(3) {
			t.Fatalf("stored %q, want the third rotation", got)
		}
	})
}

// Start's other outcomes: unreachable → offline, then linked once a background retry gets through; incompatible
// keeps the credential; no credential → unlinked without a refresh; invalid_grant → unlinked, signed_out.
func TestStartOutcomes(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			e.fs.ln.down.Store(true)
			must(t, e.m.Start(t.Context()))
			wantStatus(t, e.m, deviceauth.StatusOffline, deviceauth.NoticeNone)
			if _, err := e.m.AccessToken(t.Context(), false); !errors.Is(err, deviceauth.ErrOffline) {
				t.Fatalf("AccessToken = %s", describe(err))
			}
			e.fs.ln.down.Store(false)
			time.Sleep(time.Minute)
			wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		})
	})
	t.Run("incompatible", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			e.fs.with(func(fs *fakeServer) { fs.info.Protocol = api.InfoProtocol{Current: 9, Min: 9} })
			must(t, e.m.Start(t.Context()))
			s := e.m.State()
			if s.Status != deviceauth.StatusIncompatible || s.Compat.Verdict != compat.UpdateApp ||
				s.Compat.Reason != compat.ReasonProtocolTooNew {
				t.Fatalf("state %s %+v", s.Status, s.Compat)
			}
			if got := storedToken(t, e.store, testOrigin); got != seedToken {
				t.Fatal("the credential wasn't kept")
			}
			if _, err := e.m.AccessToken(t.Context(), false); !errors.Is(err, deviceauth.ErrIncompatible) {
				t.Fatalf("AccessToken = %s", describe(err))
			}
			if n := len(e.fs.refreshes()); n != 0 {
				t.Fatalf("%d refreshes while incompatible", n)
			}
		})
	})
	t.Run("unlinked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			must(t, e.m.Start(t.Context()))
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
			if n := len(e.fs.seen("")); n != 1 {
				t.Fatalf("%d requests, want only /info", n)
			}
		})
	})
	t.Run("signed out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			e.fs.with(func(fs *fakeServer) {
				fs.refresh = func(api.DeviceTokenRequest, int) reply { return apiErr(api.CodeInvalidGrant, nil) }
			})
			must(t, e.m.Start(t.Context()))
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeSignedOut)
			if got := storedToken(t, e.store, testOrigin); got != "" {
				t.Fatal("the credential is still stored")
			}
		})
	})
	t.Run("ctx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			e.fs.with(func(fs *fakeServer) {
				fs.hang = func(r *http.Request) bool { return r.URL.Path == "/api/v1/device/token" }
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := e.m.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Start = %s, want ctx's error", describe(err))
			}
			e.fs.with(func(fs *fakeServer) { fs.hang = nil })
			time.Sleep(time.Minute) // the refresh times out after 15 s and is retried in the background
			wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		})
	})
}

// SessionEnded: each of its three codes makes one refresh; invalid_grant → unlinked with signed_out (or
// account_disabled); success → linked; concurrent calls share one refresh; another code makes no request.
func TestSessionEnded(t *testing.T) {
	for _, tc := range []struct {
		code   protocol.ErrorCode
		notice deviceauth.Notice
	}{
		{protocol.ErrorCodeUnauthenticated, deviceauth.NoticeSignedOut},
		{protocol.ErrorCodeSessionRevoked, deviceauth.NoticeSignedOut},
		{protocol.ErrorCodeAccountDisabled, deviceauth.NoticeAccountDisabled},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, true)
				must(t, e.m.Start(t.Context()))
				s := e.m.SessionEnded(t.Context(), tc.code)
				if s.Status != deviceauth.StatusLinked || len(e.fs.refreshes()) != 2 {
					t.Fatalf("SessionEnded = %s after %d refreshes; want linked after one more", s.Status,
						len(e.fs.refreshes()))
				}
				e.fs.with(func(fs *fakeServer) {
					fs.refresh = func(api.DeviceTokenRequest, int) reply { return apiErr(api.CodeInvalidGrant, nil) }
				})
				s = e.m.SessionEnded(t.Context(), tc.code)
				if s.Status != deviceauth.StatusUnlinked || s.Notice != tc.notice {
					t.Fatalf("SessionEnded = %s/%q, want unlinked/%q", s.Status, s.Notice, tc.notice)
				}
				if got := storedToken(t, e.store, testOrigin); got != "" {
					t.Fatal("the credential is still stored")
				}
				n := len(e.fs.seen(""))
				if s := e.m.SessionEnded(t.Context(), tc.code); s.Status != deviceauth.StatusUnlinked || len(e.fs.seen("")) != n {
					t.Fatal("SessionEnded while unlinked made a request")
				}
				_, err := e.m.AccessToken(t.Context(), false)
				if !relinkRequired(err) {
					t.Fatalf("AccessToken = %s, want RelinkRequired", describe(err))
				}
			})
		})
	}
	t.Run("concurrent and other codes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			n := len(e.fs.seen(""))
			if s := e.m.SessionEnded(t.Context(), protocol.ErrorCodeTooManyConnections); s.Status != deviceauth.StatusLinked ||
				len(e.fs.seen("")) != n {
				t.Fatal("SessionEnded with another code made a request")
			}
			var wg sync.WaitGroup
			for _, code := range []protocol.ErrorCode{protocol.ErrorCodeSessionRevoked, protocol.ErrorCodeUnauthenticated} {
				wg.Go(func() {
					if s := e.m.SessionEnded(t.Context(), code); s.Status != deviceauth.StatusLinked {
						t.Errorf("SessionEnded(%s) = %s", code, s.Status)
					}
				})
			}
			wg.Wait()
			if got := len(e.fs.refreshes()); got != 2 {
				t.Fatalf("%d refreshes, want 2 (Start's and one shared)", got)
			}
		})
	})
	t.Run("offline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			e.fs.ln.down.Store(true)
			if s := e.m.SessionEnded(t.Context(), protocol.ErrorCodeSessionRevoked); s.Status != deviceauth.StatusOffline {
				t.Fatalf("SessionEnded = %s, want offline", s.Status)
			}
		})
	})
}

// HelloRefused: client_outdated → incompatible (update_app); protocol_unsupported without serverMax → a /info
// request (Recheck) decides; other codes are ignored.
func TestHelloRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		must(t, e.m.Start(t.Context()))
		e.m.HelloRefused(nil)
		e.m.HelloRefused(&protocol.Error{Code: protocol.ErrorCodeUnauthenticated})
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)

		e.m.HelloRefused(&protocol.Error{Code: protocol.ErrorCodeClientOutdated})
		s := e.m.State()
		if s.Status != deviceauth.StatusIncompatible || s.Compat.Verdict != compat.UpdateApp ||
			s.Compat.Reason != compat.ReasonMinClientVersion {
			t.Fatalf("state %s %+v", s.Status, s.Compat)
		}
		_, err := e.m.AccessToken(t.Context(), false)
		if !errors.Is(err, deviceauth.ErrIncompatible) || !updateRequired(err) {
			t.Fatalf("AccessToken = %s, want UpdateRequired", describe(err))
		}

		// Undecided: /info says the server is fine again → linked after a refresh.
		infos := len(e.fs.seen("/api/v1/info"))
		e.m.HelloRefused(&protocol.Error{Code: protocol.ErrorCodeProtocolUnsupported})
		synctest.Wait()
		if len(e.fs.seen("/api/v1/info")) != infos+1 {
			t.Fatal("protocol_unsupported without serverMax didn't recheck /info")
		}
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)

		// Undecided, and /info says the protocol is too new.
		e.fs.with(func(fs *fakeServer) { fs.info.Protocol = api.InfoProtocol{Current: 9, Min: 9} })
		e.m.HelloRefused(&protocol.Error{Code: protocol.ErrorCodeProtocolUnsupported, Params: map[string]any{"serverMax": 0}})
		synctest.Wait()
		if s := e.m.State(); s.Status != deviceauth.StatusIncompatible || s.Compat.Reason != compat.ReasonProtocolTooNew {
			t.Fatalf("state %s %+v", s.Status, s.Compat)
		}
	})
}

// Recheck back to linked, and the manager's own Recheck every 10 minutes while incompatible.
func TestRecheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, true)
		must(t, e.m.Start(t.Context()))
		e.m.SetIncompatible(compat.Check{Verdict: compat.OK}) // ignored
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		e.m.SetIncompatible(compat.Check{Verdict: compat.UpdateServer, Reason: compat.ReasonProtocolTooOld})
		wantStatus(t, e.m, deviceauth.StatusIncompatible, deviceauth.NoticeNone)
		s, err := e.m.Recheck(t.Context())
		must(t, err)
		if s.Status != deviceauth.StatusLinked || s.Compat.Verdict != compat.OK {
			t.Fatalf("Recheck = %s %+v", s.Status, s.Compat)
		}

		// A network failure leaves the state as it was.
		e.m.SetIncompatible(compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonMinClientVersion})
		e.fs.ln.down.Store(true)
		if _, err := e.m.Recheck(t.Context()); !errors.Is(err, deviceauth.ErrNetwork) {
			t.Fatalf("Recheck = %s, want ErrNetwork", describe(err))
		}
		wantStatus(t, e.m, deviceauth.StatusIncompatible, deviceauth.NoticeNone)

		// Every 10 minutes while incompatible: the server stays too old once, then is fixed.
		e.fs.ln.down.Store(false)
		e.fs.with(func(fs *fakeServer) { fs.info.MinClientVersion = "9.0.0" })
		infos := len(e.fs.seen("/api/v1/info"))
		time.Sleep(10*time.Minute + time.Second)
		if len(e.fs.seen("/api/v1/info")) != infos+1 {
			t.Fatalf("no recheck after 10 minutes")
		}
		wantStatus(t, e.m, deviceauth.StatusIncompatible, deviceauth.NoticeNone)
		e.fs.with(func(fs *fakeServer) { fs.info.MinClientVersion = "" })
		time.Sleep(10 * time.Minute)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		infos = len(e.fs.seen("/api/v1/info"))
		time.Sleep(time.Hour)
		if len(e.fs.seen("/api/v1/info")) != infos {
			t.Fatal("rechecks went on while compatible")
		}
	})
}

// Unlink: POST /device/revoke with the cached token, then the credential is deleted. Offline: ErrRevokeNotConfirmed,
// and the credential is deleted anyway. With an expired access token, one refresh first.
func TestUnlink(t *testing.T) {
	t.Run("confirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			tok, err := e.m.AccessToken(t.Context(), false)
			must(t, err)
			must(t, e.m.Unlink(t.Context()))
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
			revokes := e.fs.seen("/api/v1/device/revoke")
			if len(revokes) != 1 || revokes[0].Header.Get("Authorization") != "Bearer "+tok.Reveal() {
				t.Fatalf("revokes %+v", revokes)
			}
			if got := storedToken(t, e.store, testOrigin); got != "" {
				t.Fatal("the credential is still stored")
			}
			if _, err := e.m.AccessToken(t.Context(), false); !errors.Is(err, deviceauth.ErrNotLinked) {
				t.Fatalf("AccessToken after Unlink = %s", describe(err))
			}
		})
	})
	t.Run("offline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			e.fs.ln.down.Store(true)
			if err := e.m.Unlink(t.Context()); !errors.Is(err, deviceauth.ErrRevokeNotConfirmed) {
				t.Fatalf("Unlink = %s, want ErrRevokeNotConfirmed", describe(err))
			}
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
			if got := storedToken(t, e.store, testOrigin); got != "" {
				t.Fatal("the credential is still stored")
			}
		})
	})
	t.Run("hanging server", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			e.fs.with(func(fs *fakeServer) { fs.hang = func(*http.Request) bool { return true } })
			start := time.Now()
			if err := e.m.Unlink(t.Context()); !errors.Is(err, deviceauth.ErrRevokeNotConfirmed) {
				t.Fatalf("Unlink = %s, want ErrRevokeNotConfirmed", describe(err))
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("Unlink took %v, want at most 10 s", d)
			}
		})
	})
	t.Run("expired token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			time.Sleep(time.Hour)
			must(t, e.m.Unlink(t.Context()))
			revokes := e.fs.seen("/api/v1/device/revoke")
			if len(revokes) != 1 || revokes[0].Header.Get("Authorization") != "Bearer "+accessToken(2) {
				t.Fatalf("revoke with %q, want the refreshed token", revokes[0].Header.Get("Authorization"))
			}
		})
	})
	t.Run("already revoked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, true)
			must(t, e.m.Start(t.Context()))
			e.fs.with(func(fs *fakeServer) {
				fs.revoke = func(*http.Request) reply { return apiErr(api.CodeInvalidToken, nil) }
			})
			must(t, e.m.Unlink(t.Context()))
		})
	})
	t.Run("never linked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			must(t, e.m.Unlink(t.Context()))
			if n := len(e.fs.seen("")); n != 0 {
				t.Fatalf("%d requests", n)
			}
		})
	})
}

// No log line, at any level, names a token, a code, an exchange id, the password, the user or the device.
func TestLogsHoldNoSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		e.fs.with(script(is(pending()), is(apiErr(api.CodeServerBusy, nil)), tokens))
		must(t, e.m.Link(t.Context()))
		time.Sleep(time.Minute)
		e.fs.with(func(fs *fakeServer) {
			fs.refresh = func(_ api.DeviceTokenRequest, n int) reply {
				if n == 0 {
					return apiErr(api.CodeServerBusy, nil)
				}
				return ok(fs.pairLocked())
			}
		})
		_, _ = e.m.AccessToken(t.Context(), true)
		time.Sleep(time.Minute)
		e.store.failAll.Store(true)
		_ = e.m.LinkWithPassword(t.Context(), "Alex", "correct horse battery")
		_ = e.m.SessionEnded(t.Context(), protocol.ErrorCodeSessionRevoked)
		_ = e.m.Unlink(t.Context())
		logs := e.logs.String()
		if logs == "" {
			t.Fatal("no log lines")
		}
		var secrets []string
		for i := range 5 {
			secrets = append(secrets, accessToken(i), refreshToken(i))
		}
		for _, r := range e.fs.refreshes() {
			secrets = append(secrets, r.ExchangeID)
		}
		secrets = append(secrets, seedToken, b64("device-code"), "WDJB", "MJHT", "correct horse", "Alex\"", "Alex ",
			"Test PC", testApp.DeviceName)
		for _, s := range secrets {
			if s != "" && strings.Contains(logs, s) {
				t.Errorf("a log line holds %q:\n%s", s, logs)
			}
		}
		if strings.Contains(logs, "username") || strings.Contains(logs, "Alex=") {
			t.Errorf("a log line names the user:\n%s", logs)
		}
	})
}
