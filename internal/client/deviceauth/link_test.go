package deviceauth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

func pending() reply { return apiErr(api.CodeAuthorizationPending, nil) }

func slowDown(interval int) reply {
	if interval == 0 {
		return apiErr(api.CodeSlowDown, nil)
	}
	return apiErr(api.CodeSlowDown, map[string]any{api.ParamInterval: interval})
}

// script answers polls from a list; the last answer repeats.
func script(answers ...func(fs *fakeServer) reply) func(fs *fakeServer) {
	return func(fs *fakeServer) {
		fs.poll = func(n int) reply { return answers[min(n, len(answers)-1)](fs) }
	}
}

func is(r reply) func(*fakeServer) reply { return func(*fakeServer) reply { return r } }
func tokens(fs *fakeServer) reply        { return ok(fs.pairLocked()) }

// The poll loop of 02 §11.3 step 3: every interval (5 s); slow_down with params.interval takes it, without it adds
// 5 s; access_denied, expired_token and invalid_grant end the link with their notices.
func TestLinkPollOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		script func(*fakeServer)
		status deviceauth.Status
		notice deviceauth.Notice
		at     []time.Duration // when the polls happen, from Link
	}{
		{"pending then tokens", script(is(pending()), is(pending()), tokens),
			deviceauth.StatusLinked, deviceauth.NoticeNone, []time.Duration{5, 10, 15}},
		{"slow_down with interval", script(is(slowDown(12)), is(pending()), tokens),
			deviceauth.StatusLinked, deviceauth.NoticeNone, []time.Duration{5, 17, 29}},
		{"slow_down without interval", script(is(slowDown(0)), is(slowDown(0)), tokens),
			deviceauth.StatusLinked, deviceauth.NoticeNone, []time.Duration{5, 15, 30}},
		{"denied", script(is(pending()), is(apiErr(api.CodeAccessDenied, nil))),
			deviceauth.StatusUnlinked, deviceauth.NoticeDenied, []time.Duration{5, 10}},
		{"expired_token", script(is(apiErr(api.CodeExpiredToken, nil))),
			deviceauth.StatusUnlinked, deviceauth.NoticeCodeExpired, []time.Duration{5}},
		{"invalid_grant", script(is(pending()), is(apiErr(api.CodeInvalidGrant, nil))),
			deviceauth.StatusUnlinked, deviceauth.NoticeCodeExpired, []time.Duration{5, 10}},
		{"5xx and 429 keep polling", script(is(apiErr(api.CodeServerBusy, nil)), is(apiErr(api.CodeRateLimited, nil)),
			tokens), deviceauth.StatusLinked, deviceauth.NoticeNone, []time.Duration{5, 10, 15}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, false)
				e.fs.with(tc.script)
				start := time.Now()
				must(t, e.m.Link(t.Context()))
				wantStatus(t, e.m, deviceauth.StatusLinking, deviceauth.NoticeNone)
				time.Sleep(2 * time.Minute)
				wantStatus(t, e.m, tc.status, tc.notice)
				polls := e.fs.polls()
				var at []time.Duration
				for _, p := range polls {
					at = append(at, p.At.Sub(start)/time.Second)
				}
				if !slices.Equal(at, tc.at) {
					t.Fatalf("polls at %v s, want %v s", at, tc.at)
				}
				if s := e.m.State(); s.Link != nil {
					t.Errorf("Link still set: %+v", s.Link)
				}
				if tc.status == deviceauth.StatusLinked {
					if got := storedToken(t, e.store, testOrigin); got != refreshToken(1) {
						t.Errorf("stored refresh token %q, want the issued one", got)
					}
				} else if got := storedToken(t, e.store, testOrigin); got != "" {
					t.Errorf("a credential was stored after %s", tc.name)
				}
			})
		})
	}
}

// Network errors keep the loop polling with the retry series (never faster than the interval) until the code
// expires: then code_expired, with no automatic restart.
func TestLinkPollsThroughNetworkErrorsUntilExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		must(t, e.m.Link(t.Context()))
		e.fs.ln.down.Store(true)
		dials := e.fs.ln.dials.Load()
		time.Sleep(599 * time.Second)
		wantStatus(t, e.m, deviceauth.StatusLinking, deviceauth.NoticeNone)
		if n := e.fs.ln.dials.Load() - dials; n < 10 {
			t.Fatalf("%d poll attempts in 599 s, want polling to go on", n)
		}
		time.Sleep(2 * time.Second)
		wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeCodeExpired)
		after := e.fs.ln.dials.Load()
		time.Sleep(10 * time.Minute)
		if e.fs.ln.dials.Load() != after {
			t.Fatal("polling went on after the code expired")
		}
	})
}

// Link returns once linking is reached; polling continues after Link's ctx is cancelled, until CancelLink (unlinked,
// no notice) or Close.
func TestLinkPollsOnTheManagersContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		ctx, cancel := context.WithCancel(t.Context())
		must(t, e.m.Link(ctx))
		cancel()
		time.Sleep(11 * time.Second)
		if n := len(e.fs.polls()); n != 2 {
			t.Fatalf("%d polls after Link's ctx ended, want 2", n)
		}
		must(t, e.m.Link(t.Context())) // a second Link keeps the shown code
		if n := len(e.fs.seen("/api/v1/device/code")); n != 1 {
			t.Fatalf("%d /device/code requests, want 1", n)
		}
		e.m.CancelLink()
		wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
		time.Sleep(time.Minute)
		if n := len(e.fs.polls()); n != 2 {
			t.Fatalf("%d polls after CancelLink, want 2", n)
		}

		must(t, e.m.Link(t.Context()))
		time.Sleep(6 * time.Second)
		must(t, e.m.Close())
		n := len(e.fs.polls())
		time.Sleep(time.Minute)
		if len(e.fs.polls()) != n {
			t.Fatal("polling went on after Close")
		}
		if err := e.m.Link(t.Context()); err == nil {
			t.Fatal("Link after Close succeeded")
		}
	})
}

// 02 §11.3 step 4: the credential is written first, then the state becomes linked; the access token of the pair
// is used without a refresh.
func TestLinkWritesTheCredentialBeforeLinked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		e.fs.with(script(is(pending()), tokens))
		e.store.onPut = func(credstore.Credential) {
			if s := e.m.State(); s.Status == deviceauth.StatusLinked {
				t.Error("the state was linked before the credential was written")
			}
		}
		must(t, e.m.Link(t.Context()))
		time.Sleep(11 * time.Second)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		put, linked := e.log.index("put"), e.log.index("state:linked")
		if put < 0 || linked < 0 || put > linked {
			t.Fatalf("events %v: want put before state:linked", e.log.list())
		}
		c, err := e.store.Get(testOrigin)
		must(t, err)
		if c.RefreshToken != refreshToken(1) || c.DeviceID != testDevice || c.UserName != "Alex" {
			t.Fatalf("stored credential %+v", c)
		}
		s := e.m.State()
		if s.User == nil || s.User.ID != testUserID || s.Device == nil || s.Device.ID != testDevice || s.ServerName != "Alex's server" {
			t.Fatalf("state %+v", s)
		}
		tok, err := e.m.AccessToken(t.Context(), false)
		must(t, err)
		if tok.Reveal() != accessToken(1) || len(e.fs.refreshes()) != 0 {
			t.Fatalf("AccessToken = %q after %d refreshes; want the linked pair's token, no refresh", tok.Reveal(),
				len(e.fs.refreshes()))
		}
	})
}

// A failed credential write is retried once; when it fails again the app stays linked for this run with save_failed.
func TestLinkSaveFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		e.fs.with(script(tokens))
		e.store.failAll.Store(true)
		must(t, e.m.Link(t.Context()))
		time.Sleep(6 * time.Second)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeSaveFailed)
		if n := e.store.puts.Load(); n != 2 {
			t.Fatalf("%d Put calls, want 2 (one retry)", n)
		}
		tok, err := e.m.AccessToken(t.Context(), false)
		must(t, err)
		if tok.Reveal() != accessToken(1) {
			t.Fatalf("AccessToken = %q, want the run's token", tok.Reveal())
		}
		if got := storedToken(t, e.store, testOrigin); got != "" {
			t.Fatal("a credential is stored after save_failed")
		}

		// The run-only credential refreshes too; a later successful save clears the notice.
		e.store.failAll.Store(false)
		_, err = e.m.AccessToken(t.Context(), true)
		must(t, err)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		if got := storedToken(t, e.store, testOrigin); got != refreshToken(2) {
			t.Fatalf("stored %q after the refresh, want the rotated token", got)
		}
	})
}

// One failed write followed by a good one is no save_failed.
func TestLinkSaveRetriedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		e.fs.with(script(tokens))
		e.store.failPuts.Store(1)
		must(t, e.m.Link(t.Context()))
		time.Sleep(6 * time.Second)
		wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
		if got := storedToken(t, e.store, testOrigin); got != refreshToken(1) {
			t.Fatalf("stored %q, want the issued token", got)
		}
	})
}

// Link checks compat first: a server without the device flow is update_server, and nothing else is asked.
func TestLinkIncompatible(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, false)
		e.fs.with(func(fs *fakeServer) { fs.info.Features = []string{api.InfoFeaturePush} })
		err := e.m.Link(t.Context())
		if !errors.Is(err, deviceauth.ErrIncompatible) || !updateRequired(err) {
			t.Fatalf("Link = %s, want ErrIncompatible", describe(err))
		}
		s := e.m.State()
		if s.Status != deviceauth.StatusIncompatible || s.Compat.Verdict != compat.UpdateServer ||
			s.Compat.Reason != compat.ReasonNoDeviceFlow {
			t.Fatalf("state %s %+v", s.Status, s.Compat)
		}
		if n := len(e.fs.seen("/api/v1/device/code")); n != 0 {
			t.Fatalf("%d /device/code requests", n)
		}
	})
}

// LinkWithPassword (02 §14): 200 → the credential, then linked; 401 → stays unlinked with the *api.Error; a compat
// refusal → incompatible; a running device-flow link is cancelled first.
func TestLinkWithPassword(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			must(t, e.m.Link(t.Context())) // a device-flow link in progress
			must(t, e.m.LinkWithPassword(t.Context(), "Alex", "correct horse"))
			wantStatus(t, e.m, deviceauth.StatusLinked, deviceauth.NoticeNone)
			put, linked := e.log.index("put"), e.log.index("state:linked")
			if put < 0 || put > linked {
				t.Fatalf("events %v: want put before state:linked", e.log.list())
			}
			if got := storedToken(t, e.store, testOrigin); got != refreshToken(1) {
				t.Fatalf("stored %q", got)
			}
			time.Sleep(time.Minute)
			if n := len(e.fs.polls()); n != 0 {
				t.Fatalf("the device-flow link polled %d times after the password link", n)
			}
			tok, err := e.m.AccessToken(t.Context(), false)
			must(t, err)
			if tok.Reveal() != accessToken(1) {
				t.Fatalf("AccessToken = %q", tok.Reveal())
			}
		})
	})
	t.Run("wrong password", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			e.fs.with(func(fs *fakeServer) {
				fs.password = func(api.DevicePasswordRequest) reply { return apiErr(api.CodeInvalidCredentials, nil) }
			})
			err := e.m.LinkWithPassword(t.Context(), "Alex", "wrong")
			if !isAPI(err, api.CodeInvalidCredentials) {
				t.Fatalf("LinkWithPassword = %s, want invalid_credentials", describe(err))
			}
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
			if got := storedToken(t, e.store, testOrigin); got != "" {
				t.Fatal("a credential was stored")
			}
			if n := len(e.fs.seen("/api/v1/device/password")); n != 1 {
				t.Fatalf("%d password requests, want 1 (never retried)", n)
			}
		})
	})
	t.Run("rate limited", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			e.fs.with(func(fs *fakeServer) {
				fs.password = func(api.DevicePasswordRequest) reply {
					r := apiErr(api.CodeRateLimited, nil)
					r.body = api.ErrorResponse{Error: api.Error{Code: api.CodeRateLimited, RetryAfter: 42}}
					return r
				}
			})
			var e2 *api.Error
			if err := e.m.LinkWithPassword(t.Context(), "Alex", "x"); !errors.As(err, &e2) || e2.RetryAfter != 42 {
				t.Fatalf("LinkWithPassword = %s, want rate_limited with retryAfter 42", describe(err))
			}
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
		})
	})
	t.Run("incompatible", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			e.fs.with(func(fs *fakeServer) { fs.info.MinClientVersion = "9.0.0" })
			err := e.m.LinkWithPassword(t.Context(), "Alex", "correct horse")
			if !errors.Is(err, deviceauth.ErrIncompatible) {
				t.Fatalf("LinkWithPassword = %s, want ErrIncompatible", describe(err))
			}
			s := e.m.State()
			if s.Status != deviceauth.StatusIncompatible || s.Compat.Verdict != compat.UpdateApp ||
				s.Compat.Reason != compat.ReasonMinClientVersion {
				t.Fatalf("state %s %+v", s.Status, s.Compat)
			}
			if n := len(e.fs.seen("/api/v1/device/password")); n != 0 {
				t.Fatal("the password was sent to an incompatible server")
			}
		})
	})
	t.Run("offline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, false)
			e.fs.ln.down.Store(true)
			if err := e.m.LinkWithPassword(t.Context(), "Alex", "x"); !errors.Is(err, deviceauth.ErrNetwork) {
				t.Fatalf("LinkWithPassword = %s, want ErrNetwork", describe(err))
			}
			wantStatus(t, e.m, deviceauth.StatusUnlinked, deviceauth.NoticeNone)
		})
	})
}

// OnState gets every change in order, with copies, and may call the manager from inside the callback.
func TestOnStateMayCallTheManager(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakeServer(t)
		var m *deviceauth.Manager
		var seen []deviceauth.Status
		m = deviceauth.NewManager(deviceauth.ManagerOptions{
			Client: fs.client(""),
			Store:  newMemoryStore(),
			Logger: quiet,
			OnState: func(s deviceauth.State) {
				seen = append(seen, s.Status)
				_ = m.State()
				if s.Status == deviceauth.StatusLinking {
					m.CancelLink()
				}
				if s.Link != nil {
					s.Link.UserCode = "changed by the callback"
				}
			},
		})
		defer func() { _ = m.Close() }()
		must(t, m.Link(t.Context()))
		// unlinked first: /info brought the server's name.
		want := []deviceauth.Status{deviceauth.StatusUnlinked, deviceauth.StatusLinking, deviceauth.StatusUnlinked}
		if !slices.Equal(seen, want) {
			t.Fatalf("OnState saw %v, want %v", seen, want)
		}
	})
}
