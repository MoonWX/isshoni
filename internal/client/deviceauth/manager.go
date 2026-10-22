package deviceauth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The manager's timing (02 §11.3, §11.4, §15).
const (
	minTokenLife       = 120 * time.Second // AccessToken refreshes below this
	retryTokenLife     = 10 * time.Second  // during refresh retries, a token with more than this left is still handed out
	forcedRefreshEvery = 10 * time.Second  // at most one forced refresh per this
	dailyRefresh       = 24 * time.Hour    // the refresh that keeps the 90-day window sliding
	recheckEvery       = 10 * time.Minute  // Recheck while incompatible
	backgroundTimeout  = time.Minute       // bounds a Recheck the manager runs on its own
	minPollInterval    = 5 * time.Second
	slowDownStep       = 5 * time.Second
	unlinkTimeout      = 10 * time.Second
)

// retryDelays is the retry series of a refresh, and of a poll after a network error (02 §11.4): 1, 2, 4, 8, 16,
// 30 s, then every 30 s, each with ±20 % jitter.
var retryDelays = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 30 * time.Second}

// ManagerOptions configures NewManager.
type ManagerOptions struct {
	Client  *Client          // required: the server this manager links to
	Store   credstore.Store  // required: where the refresh token lives
	OnState func(State)      // called on every change, never while holding a lock; nil = no callback
	Clock   func() time.Time // monotonic-safe; tests inject. nil = time.Now
	Rand    io.Reader        // exchange ids; nil = crypto/rand
	Logger  *slog.Logger     // nil = slog.Default()
}

// Manager is one server's link (02 §11.3): the device-flow and password links, the credential, the access token
// and its refresh, the compat state. Safe for concurrent use.
//
// NewManager starts nothing. Start loads the credential and refreshes; the methods that need the credential load
// it on first use too, so a manager also works without Start. A credential found in the store makes the state
// linked right away (offline, unlinked or incompatible follow from what the server answers), so a linked app never
// shows unlinked while it starts; Servers loads it when it makes the manager. From the first change that needs one,
// the manager runs one goroutine of its own for the daily refresh and the 10-minute Recheck while incompatible; Link
// adds the polling goroutine and a refresh its retry goroutine. Close stops them all and waits for them.
//
// OnState is called after every change with a copy of the new State, in order, on the goroutine that made the
// change or on one that is delivering an earlier change at that moment; never while the manager holds a lock, so
// the callback may call the manager's methods.
type Manager struct {
	c         *Client
	origin    string
	store     credstore.Store
	onState   func(State)
	clock     func() time.Time
	rand      io.Reader
	log       *slog.Logger
	compatApp compat.App

	// entry is set by Servers: it updates the server's entry in servers.json. It is called with commitMu held
	// and mu not held.
	entry func(entryChange)

	ctx    context.Context // the manager's own: polling, refreshes and rechecks run on it; Close cancels it
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// commitMu serializes the changes of the credential (a link, a refresh's result, invalid_grant, Unlink,
	// ForgetAll) with the state they lead to, so the order "credential written, then the state" holds. It is
	// taken before mu, and held only across local work (the credential store, servers.json).
	commitMu sync.Mutex

	mu          sync.Mutex
	closed      bool
	state       State
	loaded      bool                  // cred reflects the store (or a newer commit)
	cred        *credstore.Credential // nil: not linked
	access      string                // the cached access token
	accessAt    time.Time             // when it was issued
	accessExp   time.Time             // when it expires
	epoch       uint64                // bumped when the credential is replaced or removed other than by a refresh
	flight      *flight               // the running refresh, or nil
	lastForced  time.Time             // when the last forced refresh started
	nextDaily   time.Time             // when the loop refreshes next; zero = not scheduled
	nextRecheck time.Time             // when the loop rechecks next while incompatible
	link        *linkRun              // the running device-flow link, or nil
	loopOn      bool
	wake        chan struct{}
	queue       []State // changes not delivered to OnState yet
	lastQueued  State
	delivering  bool
}

// flight is one logical refresh (02 §11.4): one exchange id, attempts until it succeeds, gets invalid_grant or is
// cancelled, with the retry series between them.
type flight struct {
	token string // the refresh token it presents
	xid   string // its exchange id, the same for every attempt
	epoch uint64 // the manager's epoch when it started; a different one at commit time commits nothing

	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{} // cap 1: try again now instead of waiting out the delay

	// Guarded by Manager.mu.
	attempt   chan struct{}      // closed (and replaced) when an attempt fails, and at the end
	done      chan struct{}      // closed at the end
	err       error              // the end's result: nil, an error wrapping ErrNotLinked, errStale, errCancelled, …
	lastErr   error              // the last attempt's retryable error
	failed    bool               // an attempt failed: the retry series runs
	trying    bool               // an attempt is in progress
	endedCode protocol.ErrorCode // SessionEnded's code, for the notice on invalid_grant
}

// linkRun is one running device-flow link.
type linkRun struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// entryChange is what a manager tells Servers about its entry.
type entryChange struct {
	name    string         // the server's name from /info; "" = unchanged
	setLink bool           // set User and Device (nil clears them)
	user    *api.User      // with setLink
	device  *api.DeviceRef // with setLink
}

// NewManager returns a manager for o.Client's server. It panics when o.Client or o.Store is nil.
func NewManager(o ManagerOptions) *Manager {
	if o.Client == nil || o.Store == nil {
		panic("deviceauth: NewManager needs a Client and a Store")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		c:         o.Client,
		origin:    o.Client.origin,
		store:     o.Store,
		onState:   o.OnState,
		clock:     o.Clock,
		rand:      o.Rand,
		log:       o.Logger.With("component", "deviceauth"),
		compatApp: compatApp(o.Client.app.Version),
		ctx:       ctx,
		cancel:    cancel,
		wake:      make(chan struct{}, 1),
	}
	m.state = State{Origin: m.origin, Status: StatusUnlinked}
	m.lastQueued = m.state.clone()
	return m
}

// compatApp is compat.Default() with the app's own version (AppInfo.Version is version.Version() in the app).
func compatApp(v string) compat.App {
	a := compat.Default()
	if v != "" {
		a.Version = v
	}
	return a
}

func (m *Manager) now() time.Time { return m.clock() }

// Start loads the credential and refreshes once: linked, unlinked, offline or incompatible (02 §11.3, §13.1). It
// asks /info first: a verdict that isn't ok enters StatusIncompatible and keeps the credential. It never blocks
// longer than ctx: when ctx ends first, the refresh goes on in the background and ctx's error is returned. A network
// failure at /info, or ctx ending before /info answers, leaves StatusOffline when no token is usable (unlinked
// without a credential) and the refresh retrying in the background (02 §11.4); Start then returns nil, or ctx's
// error. A credential store that can't be read leaves the state unlinked and returns its error.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.loadAndFlush(); err != nil {
		return err
	}
	if m.isClosed() {
		return errClosed
	}
	info, err := m.c.Info(ctx)
	if err != nil {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			m.log.Info("deviceauth: the server didn't answer before start's ctx ended", "origin", m.origin)
		} else {
			m.log.Info("deviceauth: the server can't be reached at start", "origin", m.origin, "err", err)
		}
		m.mu.Lock()
		if m.cred != nil && m.state.Status != StatusIncompatible && m.state.Status != StatusLinking {
			if m.flight == nil {
				m.startFlightLocked()
			}
			if !m.usableLocked(m.now()) {
				m.state.Status = StatusOffline
			}
		}
		m.changedLocked()
		m.mu.Unlock()
		m.flush()
		return ctxErr
	}
	check := compat.FromInfo(info, m.compatApp)
	m.noteServerName(info.Server.Name)
	m.mu.Lock()
	m.state.ServerName = info.Server.Name
	if check.Verdict != compat.OK {
		m.enterIncompatibleLocked(check)
		m.mu.Unlock()
		m.flush()
		m.wakeLoop()
		return nil
	}
	m.state.Compat = check
	if m.state.Status == StatusIncompatible {
		m.state.Status = m.restingStatusLocked()
	}
	if m.cred == nil {
		if m.state.Status != StatusLinking {
			m.state.Status = StatusUnlinked
		}
		m.changedLocked()
		m.mu.Unlock()
		m.flush()
		return nil
	}
	f := m.joinFlightLocked()
	ch := f.attempt
	m.changedLocked()
	m.mu.Unlock()
	m.flush()
	m.wakeLoop()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Link checks compat (/info) and starts a device-flow link (02 §11.3 "Linking"). It returns once StatusLinking is
// reached (with Go's canonical verification URIs in State().Link), or with an error: ErrIncompatible (and
// StatusIncompatible), StartLink's ErrBadResponse, an *api.Error such as rate_limited, ErrNetwork, ctx's error; the
// state is unchanged on those. ctx bounds only the /info and /device/code requests. Polling then runs on the
// manager's own context (not the caller's: a Wails binding's ctx ends with the JS call) until linked, denied or
// expired, CancelLink, or Close. A Link while a code is shown returns nil and keeps that code.
func (m *Manager) Link(ctx context.Context) error {
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		return errClosed
	case m.link != nil:
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := m.checkCompat(ctx); err != nil {
		return err
	}
	resp, err := m.c.StartLink(ctx)
	if err != nil {
		return err
	}
	m.commitMu.Lock()
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		m.commitMu.Unlock()
		return errClosed
	case m.state.Status == StatusIncompatible:
		m.mu.Unlock()
		m.commitMu.Unlock()
		return ErrIncompatible
	case m.link != nil:
		m.mu.Unlock()
		m.commitMu.Unlock()
		return nil
	}
	now := m.now()
	ctxRun, cancel := context.WithCancel(m.ctx)
	run := &linkRun{ctx: ctxRun, cancel: cancel}
	m.link = run
	m.state.Status = StatusLinking
	m.state.Notice = NoticeNone
	m.state.Link = &LinkState{
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		ExpiresAt:               now.Add(time.Duration(resp.ExpiresIn) * time.Second),
	}
	m.changedLocked()
	m.wg.Add(1)
	go m.poll(run, resp, now)
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	return nil
}

// CancelLink ends a running device-flow link: unlinked (or linked, when an earlier link's credential exists), no
// notice. Without one it does nothing.
func (m *Manager) CancelLink() {
	m.commitMu.Lock()
	m.mu.Lock()
	m.stopLinkLocked(NoticeNone)
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
}

// LinkWithPassword is the fallback of 02 §14: /info → compat.FromInfo (not ok → StatusIncompatible,
// ErrIncompatible), then POST /device/password; a 200 takes the steps of a poll's success (the credential first,
// then servers.json, then StatusLinked). It returns when linked or with the error: *api.Error (invalid_credentials,
// account_pending, account_disabled, rate_limited with RetryAfter, server_busy, validation_failed), ErrNetwork or
// ErrIncompatible; the state stays unlinked on an error, and a running device-flow link is cancelled first. The
// password is passed on once, never stored, retried or logged.
func (m *Manager) LinkWithPassword(ctx context.Context, username string, password protocol.Secret) error {
	m.commitMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.commitMu.Unlock()
		return errClosed
	}
	m.stopLinkLocked(NoticeNone)
	if m.state.Notice != NoticeNone && m.state.Notice != NoticeSaveFailed {
		m.state.Notice = NoticeNone
		m.changedLocked()
	}
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	if err := m.checkCompat(ctx); err != nil {
		return err
	}
	resp, err := m.c.PasswordLink(ctx, username, password)
	if err != nil {
		return err
	}
	if !m.finishLink(nil, resp, "password") {
		return errClosed
	}
	return nil
}

// AccessToken returns a token with at least 120 s left, refreshing when needed (single flight). It satisfies
// 01-client-core's core.Auth.AccessToken. forceRefresh=true means the server refused the token the caller had: the
// cached token is dropped and refreshed (single flight). At most one forced refresh per 10 s: a forced call within
// 10 s of the last forced refresh gets the token that refresh issued, without a new request.
//
// While a refresh is being retried (02 §11.4), the cached token is returned as long as it has more than 10 s left;
// after that the state is offline and the answer ErrOffline.
//
// Errors: ErrNotLinked (RelinkRequired), ErrIncompatible (UpdateRequired), ErrOffline (transient; wraps the network
// error), ctx's error.
func (m *Manager) AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error) {
	tok, _, err := m.AccessTokenWithExpiry(ctx, forceRefresh)
	return tok, err
}

// AccessTokenWithExpiry is AccessToken plus its expiry, for the desktop SPA (02 §11.7). Same forceRefresh rules.
func (m *Manager) AccessTokenWithExpiry(ctx context.Context, forceRefresh bool) (protocol.Secret, time.Time, error) {
	if err := m.loadAndFlush(); err != nil {
		return "", time.Time{}, err
	}
	tok, exp, err := m.accessToken(ctx, forceRefresh)
	m.flush() // an ErrOffline answer may have changed the state
	return tok, exp, err
}

func (m *Manager) accessToken(ctx context.Context, force bool) (protocol.Secret, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		switch {
		case m.closed:
			return "", time.Time{}, errClosed
		case m.state.Status == StatusIncompatible:
			return "", time.Time{}, ErrIncompatible
		case m.cred == nil:
			return "", time.Time{}, ErrNotLinked
		}
		now := m.now()
		f := m.flight
		switch {
		case force && !m.lastForced.IsZero() && now.Sub(m.lastForced) < forcedRefreshEvery:
			// Within 10 s of the last forced refresh: its result, without a new request.
			switch {
			case f != nil && f.failed && !f.trying:
				return m.retryAnswerLocked(f)
			case f != nil:
				// wait for it below
			case m.access != "" && !m.accessAt.Before(m.lastForced) && m.accessExp.After(now):
				return protocol.Secret(m.access), m.accessExp, nil
			default:
				force = false // it issued nothing: answer as an ordinary call
				continue
			}
		case force:
			m.lastForced = now
			m.access = "" // the server refused it
			if f == nil {
				f = m.startFlightLocked()
			} else {
				m.kickLocked(f)
			}
		case m.access != "" && m.accessExp.Sub(now) >= minTokenLife:
			return protocol.Secret(m.access), m.accessExp, nil
		case f == nil:
			f = m.startFlightLocked()
		case f.failed && !f.trying:
			// The retry series runs: no waiting for its next attempt.
			return m.retryAnswerLocked(f)
		}
		ch := f.attempt
		m.mu.Unlock()
		select {
		case <-ch:
			m.mu.Lock()
		case <-ctx.Done():
			m.mu.Lock()
			return "", time.Time{}, ctx.Err()
		}
		select {
		case <-f.done:
		default:
			// An attempt failed and the retry series goes on.
			return m.retryAnswerLocked(f)
		}
		switch {
		case f.err == nil:
			// This refresh's token, even when the server made it live less than 120 s (no refresh loop).
			if m.access != "" && m.accessExp.After(m.now()) && !m.closed && m.state.Status != StatusIncompatible {
				return protocol.Secret(m.access), m.accessExp, nil
			}
		case errors.Is(f.err, errStale), errors.Is(f.err, errCancelled):
			// The credential was replaced or removed meanwhile: look again.
		default:
			return "", time.Time{}, f.err // wraps ErrNotLinked after invalid_grant
		}
		force = false
	}
}

// retryAnswerLocked is AccessToken's answer while f's retry series runs: the cached token while it has more than
// 10 s left, else ErrOffline, and the state is offline.
func (m *Manager) retryAnswerLocked(f *flight) (protocol.Secret, time.Time, error) {
	if m.usableLocked(m.now()) {
		return protocol.Secret(m.access), m.accessExp, nil
	}
	if m.state.Status == StatusLinked || m.state.Status == StatusUnlinked {
		m.state.Status = StatusOffline
		m.changedLocked()
	}
	return "", time.Time{}, fmt.Errorf("%w: %w", ErrOffline, f.lastErr)
}

// SessionEnded reports that a connection was stopped with unauthenticated (a second one, after the core's forced
// refresh), session_revoked or account_disabled; any other code is ignored and State() returned. It is synchronous:
// it makes one refresh attempt, bounded by ctx, and returns the resulting state. invalid_grant → unlinked with the
// notice of 02 §11.4 (account_disabled when code says so). Success → the manager stays linked. Concurrent calls
// share one attempt (single flight). A network failure → offline, as in §11.4. Already unlinked: no request, the
// unlinked state is returned.
func (m *Manager) SessionEnded(ctx context.Context, code protocol.ErrorCode) State {
	switch code {
	case protocol.ErrorCodeUnauthenticated, protocol.ErrorCodeSessionRevoked, protocol.ErrorCodeAccountDisabled:
	default:
		return m.State()
	}
	if err := m.loadAndFlush(); err != nil {
		return m.State()
	}
	m.mu.Lock()
	if m.closed || m.cred == nil || m.state.Status == StatusIncompatible {
		s := m.state.clone()
		m.mu.Unlock()
		return s
	}
	m.access = "" // the server ended the session that token opened
	f := m.flight
	if f == nil {
		f = m.startFlightLocked()
	} else {
		m.kickLocked(f)
	}
	if code == protocol.ErrorCodeAccountDisabled {
		f.endedCode = code
	}
	ch := f.attempt
	m.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return m.State()
}

// HelloRefused reports a hello refused with client_outdated or protocol_unsupported (either connection: 03's C2
// adapter, and the SPA through the binding); other codes and nil are ignored. It runs compat.FromError: a decided
// check that is not OK → SetIncompatible; an undecided one (protocol_unsupported without params.serverMax, 02 §13.2)
// → Recheck on the manager's own context.
func (m *Manager) HelloRefused(e *protocol.Error) {
	if e == nil {
		return
	}
	if c, decided := compat.FromError(e, m.compatApp); decided {
		m.SetIncompatible(c)
		return
	}
	if e.Code != protocol.ErrorCodeProtocolUnsupported {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, backgroundTimeout)
		defer cancel()
		if _, err := m.Recheck(ctx); err != nil {
			m.log.Debug("deviceauth: recheck after a refused hello failed", "origin", m.origin, "err", err)
		}
	}()
}

// SetIncompatible enters StatusIncompatible with c and keeps the credential; a check whose Verdict is OK is ignored.
// A running device-flow link is cancelled.
func (m *Manager) SetIncompatible(c compat.Check) {
	if c.Verdict == compat.OK {
		return
	}
	m.commitMu.Lock()
	m.mu.Lock()
	if !m.closed {
		m.enterIncompatibleLocked(c)
	}
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	m.wakeLoop()
}

// Recheck re-runs /info → compat.FromInfo. Verdict ok: the state Start would reach (linked after one refresh when a
// credential exists, else unlinked). Otherwise StatusIncompatible with the new check. A network failure returns the
// ErrNetwork error and leaves the state as it was. The manager calls it every 10 minutes while incompatible.
func (m *Manager) Recheck(ctx context.Context) (State, error) {
	if err := m.loadAndFlush(); err != nil {
		return m.State(), err
	}
	info, err := m.c.Info(ctx)
	if err != nil {
		return m.State(), err
	}
	check := compat.FromInfo(info, m.compatApp)
	m.noteServerName(info.Server.Name)
	m.commitMu.Lock()
	m.mu.Lock()
	if m.closed {
		s := m.state.clone()
		m.mu.Unlock()
		m.commitMu.Unlock()
		return s, errClosed
	}
	m.state.ServerName = info.Server.Name
	if check.Verdict != compat.OK {
		m.enterIncompatibleLocked(check)
		s := m.state.clone()
		m.mu.Unlock()
		m.commitMu.Unlock()
		m.flush()
		m.wakeLoop()
		return s, nil
	}
	m.state.Compat = check
	if m.state.Status == StatusIncompatible {
		m.state.Status = m.restingStatusLocked()
	}
	if m.cred == nil {
		m.changedLocked()
		s := m.state.clone()
		m.mu.Unlock()
		m.commitMu.Unlock()
		m.flush()
		return s, nil
	}
	f := m.joinFlightLocked()
	ch := f.attempt
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	m.wakeLoop()
	select {
	case <-ch:
		return m.State(), nil
	case <-ctx.Done():
		return m.State(), ctx.Err()
	}
}

// Unlink signs this app out: POST /device/revoke (10 s, best effort; with a fresh access token when the cached one
// is about to expire), then deletes the credential and clears the server's user and device in servers.json, whatever
// the revoke's result: unlinked, no notice. It returns ErrRevokeNotConfirmed (still unlinked locally) when the
// server couldn't be reached or answered with an error other than "this device is already gone"; the UI then says
// "Remove {device} on your Devices page to finish". A failed deletion of the credential is returned too.
func (m *Manager) Unlink(ctx context.Context) error {
	loadErr := m.loadAndFlush()
	t, ok := m.takeCredential()
	if !ok {
		return errClosed
	}
	confirmed := true
	if t.cred != nil {
		rctx, cancel := context.WithTimeout(ctx, unlinkTimeout)
		confirmed = revokeDevice(rctx, m.c, m.rand, *t.cred, t.access, t.exp, m.now())
		cancel()
	}

	var delErr error
	m.commitMu.Lock()
	m.mu.Lock()
	same := m.epoch == t.epoch
	m.mu.Unlock()
	if same { // no link stored a new credential while the revoke ran
		if err := m.store.Delete(m.origin); err != nil {
			delErr = fmt.Errorf("deviceauth: delete the credential: %w", err)
			m.log.Warn("deviceauth: couldn't delete the credential at unlink", "origin", m.origin, "err", err)
		}
		m.setEntry(entryChange{setLink: true})
	}
	m.commitMu.Unlock()
	m.log.Info("deviceauth: unlinked", "origin", m.origin, "confirmed", confirmed)
	cred := t.cred
	switch {
	case delErr != nil:
		return delErr
	case loadErr != nil && cred == nil:
		return loadErr
	case !confirmed:
		return ErrRevokeNotConfirmed
	}
	return nil
}

// State returns the current state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.clone()
}

// Close stops polling, refreshes and timers and waits for the manager's goroutines. When the Client made its own
// HTTP client (ClientOptions.HTTP nil), its idle connections are closed too, so their goroutines end with Close. The
// state stays readable; the other methods answer an error afterwards. Close is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	if m.link != nil {
		m.link.cancel()
	}
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	if m.c.ownHTTP {
		m.c.http.CloseIdleConnections()
	}
	return nil
}

// ---- credential and state helpers ----

// loadAndFlush is load for the methods: a state change it makes goes to OnState right away.
func (m *Manager) loadAndFlush() error {
	err := m.load()
	m.flush()
	return err
}

// load reads the credential from the store once. ErrNotFound means not linked; any other error is returned and
// load tries again on the next call. A credential it finds while the state is unlinked makes it linked: unlinked
// means "no credential" (02 §11.3), and the app shows its relink screen for it. The change is queued for OnState
// and delivered by the caller's flush; Servers loads when it makes a manager and doesn't flush (OpenServers hasn't
// returned yet), so the change reaches OnState with the manager's first flush, Start's usually.
func (m *Manager) load() error {
	m.mu.Lock()
	loaded := m.loaded
	m.mu.Unlock()
	if loaded {
		return nil
	}
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	m.mu.Lock()
	loaded = m.loaded
	m.mu.Unlock()
	if loaded {
		return nil
	}
	c, err := m.store.Get(m.origin)
	switch {
	case errors.Is(err, credstore.ErrNotFound):
		m.mu.Lock()
		m.loaded = true
		m.mu.Unlock()
		return nil
	case err != nil:
		m.log.Warn("deviceauth: couldn't read the credential", "origin", m.origin, "err", err)
		return fmt.Errorf("deviceauth: read the credential: %w", err)
	}
	m.mu.Lock()
	m.cred = &c
	m.loaded = true
	if m.state.Status == StatusUnlinked {
		m.state.Status = StatusLinked
		m.changedLocked()
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// usableLocked reports whether the cached token may be handed out during refresh retries (more than 10 s left).
func (m *Manager) usableLocked(now time.Time) bool {
	return m.access != "" && m.accessExp.Sub(now) > retryTokenLife
}

// restingStatusLocked is the status without a link in progress: unlinked without a credential; linked, or offline
// while a refresh is being retried without a usable token.
func (m *Manager) restingStatusLocked() Status {
	switch {
	case m.cred == nil:
		return StatusUnlinked
	case m.flight != nil && m.flight.failed && !m.usableLocked(m.now()):
		return StatusOffline
	}
	return StatusLinked
}

// enterIncompatibleLocked enters StatusIncompatible with c; a running link is cancelled.
func (m *Manager) enterIncompatibleLocked(c compat.Check) {
	if m.link != nil {
		m.link.cancel()
		m.link = nil
		m.state.Link = nil
	}
	m.state.Status = StatusIncompatible
	m.state.Compat = c
	m.nextRecheck = m.now().Add(recheckEvery)
	m.changedLocked()
}

// stopLinkLocked ends the running device-flow link with notice; the caller holds commitMu.
func (m *Manager) stopLinkLocked(n Notice) {
	if m.link == nil {
		return
	}
	m.link.cancel()
	m.link = nil
	m.state.Link = nil
	if m.state.Status == StatusLinking {
		m.state.Status = m.restingStatusLocked()
	}
	m.state.Notice = n
	m.changedLocked()
}

// stopFlightLocked cancels the running refresh; its waiters look at the manager again.
func (m *Manager) stopFlightLocked() {
	if m.flight != nil {
		m.flight.cancel()
		m.flight = nil
	}
}

// taken is what takeCredential took.
type taken struct {
	cred   *credstore.Credential // nil: there was none
	access string
	exp    time.Time
	epoch  uint64 // the manager's epoch afterwards; a different one later means a link stored a new credential
}

// takeCredential is the first step of Unlink and ForgetAll: it ends a running link and refresh and forgets the
// credential and the tokens in memory (unlinked, no notice), so nothing refreshes while the revoke runs, and returns
// them. The store still holds the credential; the caller deletes it. ok is false when the manager is closed.
func (m *Manager) takeCredential() (t taken, ok bool) {
	m.commitMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.commitMu.Unlock()
		return taken{}, false
	}
	m.stopLinkLocked(NoticeNone)
	t = taken{cred: m.cred, access: m.access, exp: m.accessExp}
	m.dropCredentialLocked(NoticeNone)
	t.epoch = m.epoch
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	return t, true
}

// dropCredentialLocked forgets the credential and the tokens: unlinked (unless incompatible) with notice.
func (m *Manager) dropCredentialLocked(n Notice) {
	m.stopFlightLocked()
	m.cred = nil
	m.loaded = true
	m.access, m.accessAt, m.accessExp = "", time.Time{}, time.Time{}
	m.lastForced, m.nextDaily = time.Time{}, time.Time{}
	m.epoch++
	if m.state.Status != StatusIncompatible && m.state.Status != StatusLinking {
		m.state.Status = StatusUnlinked
	}
	m.state.User, m.state.Device = nil, nil
	m.state.Notice = n
}

// checkCompat is the pre-link check of 02 §13.1: /info → compat.FromInfo; not ok → StatusIncompatible and
// ErrIncompatible. A request error is returned as it is, the state unchanged.
func (m *Manager) checkCompat(ctx context.Context) error {
	info, err := m.c.Info(ctx)
	if err != nil {
		return err
	}
	check := compat.FromInfo(info, m.compatApp)
	m.noteServerName(info.Server.Name)
	m.commitMu.Lock()
	m.mu.Lock()
	m.state.ServerName = info.Server.Name
	if check.Verdict != compat.OK {
		if !m.closed {
			m.enterIncompatibleLocked(check)
		}
		m.mu.Unlock()
		m.commitMu.Unlock()
		m.flush()
		m.wakeLoop()
		return ErrIncompatible
	}
	m.state.Compat = check
	if m.state.Status == StatusIncompatible {
		m.state.Status = m.restingStatusLocked()
	}
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	return nil
}

// saveCredential writes c to the store, trying a second time when the first write fails (02 §11.3 step 4). When
// both fail it deletes what the store holds for the server (best effort: an older refresh token there is spent or
// belongs to another device, and presenting it after a restart would trip reuse detection) and reports false.
func (m *Manager) saveCredential(c credstore.Credential) bool {
	err := m.store.Put(m.origin, c)
	if err == nil {
		return true
	}
	m.log.Warn("deviceauth: couldn't save the credential; trying again", "origin", m.origin, "err", err)
	if err = m.store.Put(m.origin, c); err == nil {
		return true
	}
	m.log.Warn("deviceauth: couldn't save the credential; linked for this run only", "origin", m.origin, "err", err)
	if err := m.store.Delete(m.origin); err != nil {
		m.log.Debug("deviceauth: couldn't delete the old credential either", "origin", m.origin, "err", err)
	}
	return false
}

// setEntry tells Servers about the entry; the caller holds commitMu (and not mu).
func (m *Manager) setEntry(ch entryChange) {
	if m.entry != nil {
		m.entry(ch)
	}
}

// noteServerName passes a name from /info on to servers.json.
func (m *Manager) noteServerName(name string) {
	if name == "" || m.entry == nil {
		return
	}
	m.commitMu.Lock()
	m.entry(entryChange{name: name})
	m.commitMu.Unlock()
}

// ---- linking ----

// poll is the device-flow loop of 02 §11.3 step 3, on the manager's own context.
func (m *Manager) poll(run *linkRun, code api.DeviceCodeResponse, started time.Time) {
	defer m.wg.Done()
	defer run.cancel()
	interval := max(time.Duration(code.Interval)*time.Second, minPollInterval)
	expires := started.Add(time.Duration(code.ExpiresIn) * time.Second)
	delay := interval
	failures := 0
	for {
		wait := delay
		if left := expires.Sub(m.now()); wait > left {
			wait = max(left, 0)
		}
		t := time.NewTimer(wait)
		select {
		case <-run.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !m.now().Before(expires) {
			m.endLink(run, NoticeCodeExpired)
			return
		}
		resp, err := m.c.Poll(run.ctx, code.DeviceCode)
		if run.ctx.Err() != nil {
			return
		}
		e := apiError(err)
		switch {
		case err == nil:
			m.finishLink(run, resp, "device_flow")
			return
		case e != nil && e.Code == api.CodeAuthorizationPending:
			failures, delay = 0, interval
		case e != nil && e.Code == api.CodeSlowDown:
			if s, ok := positiveSeconds(e.Params[api.ParamInterval]); ok {
				interval = max(s, minPollInterval)
			} else {
				interval += slowDownStep
			}
			failures, delay = 0, interval
		case e != nil && e.Code == api.CodeAccessDenied:
			m.endLink(run, NoticeDenied)
			return
		case e != nil && (e.Code == api.CodeExpiredToken || e.Code == api.CodeInvalidGrant):
			m.endLink(run, NoticeCodeExpired)
			return
		case retryable(err):
			// Keep polling with the retry series, never faster than the interval (a lost answer still counted as a
			// poll on the server, and a quicker one would earn a slow_down).
			delay = max(interval, m.retryDelay(failures, err))
			failures++
			m.log.Debug("deviceauth: poll failed; polling on", "origin", m.origin, "err", err)
		default:
			m.log.Warn("deviceauth: unexpected answer while linking", "origin", m.origin, "err", err)
			m.endLink(run, NoticeCodeExpired)
			return
		}
	}
}

// endLink ends run with notice, unless it was already ended.
func (m *Manager) endLink(run *linkRun, n Notice) {
	m.commitMu.Lock()
	m.mu.Lock()
	if m.link == run {
		m.stopLinkLocked(n)
	}
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
}

// finishLink takes a link's token pair (02 §11.3 step 4): the credential first, then servers.json, then
// StatusLinked. run is nil for a password link. It reports false when run was ended meanwhile or the manager closed;
// nothing is stored then. Any other running link ends with this one: a device-flow link that Link started while a
// password link's request ran would otherwise keep its code shown and polling, and an approval of that code would
// make a second device.
func (m *Manager) finishLink(run *linkRun, resp api.DeviceTokenResponse, via string) bool {
	m.commitMu.Lock()
	m.mu.Lock()
	if m.closed || run != nil && m.link != run {
		m.mu.Unlock()
		m.commitMu.Unlock()
		return false
	}
	m.mu.Unlock()
	now := m.now()
	cred := credstore.Credential{
		RefreshToken: resp.RefreshToken,
		DeviceID:     resp.Device.ID,
		UserName:     resp.User.Username,
		SavedAt:      now,
	}
	saved := m.saveCredential(cred)
	user, device := resp.User, resp.Device
	m.setEntry(entryChange{setLink: true, user: &user, device: &device})
	m.mu.Lock()
	if m.link != nil {
		m.link.cancel()
		m.link = nil
		m.state.Link = nil
	}
	m.stopFlightLocked() // a refresh of an earlier credential commits nothing now
	m.epoch++
	m.cred = &cred
	m.loaded = true
	m.access, m.accessAt = resp.AccessToken, now
	m.accessExp = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
	m.lastForced = time.Time{}
	m.nextDaily = now.Add(dailyRefresh)
	if m.state.Status != StatusIncompatible {
		m.state.Status = StatusLinked
	}
	m.state.User, m.state.Device = &user, &device
	m.state.Notice = NoticeNone
	if !saved {
		m.state.Notice = NoticeSaveFailed
	}
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	m.wakeLoop()
	m.log.Info("deviceauth: linked", "origin", m.origin, "via", via, "saved", saved)
	return true
}

// ---- refresh (02 §11.4) ----

// startFlightLocked starts a logical refresh of the current credential. On a closed manager it returns a refresh
// that has already ended with errClosed.
func (m *Manager) startFlightLocked() *flight {
	ctx, cancel := context.WithCancel(m.ctx)
	f := &flight{
		token:   m.cred.RefreshToken,
		epoch:   m.epoch,
		ctx:     ctx,
		cancel:  cancel,
		kick:    make(chan struct{}, 1),
		attempt: make(chan struct{}),
		done:    make(chan struct{}),
		trying:  true,
	}
	if m.closed {
		cancel()
		f.err, f.trying = errClosed, false
		close(f.attempt)
		close(f.done)
		return f
	}
	m.flight = f
	m.wg.Add(1)
	go m.runFlight(f)
	return f
}

// joinFlightLocked returns the running refresh, made to try now, or a new one: Start, Recheck and SessionEnded
// wait for one attempt.
func (m *Manager) joinFlightLocked() *flight {
	if f := m.flight; f != nil {
		m.kickLocked(f)
		return f
	}
	return m.startFlightLocked()
}

// kickLocked makes f try again now when it is waiting out a retry delay.
func (m *Manager) kickLocked(f *flight) {
	if f.trying {
		return
	}
	f.trying = true
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// runFlight runs f's attempts until it ends.
func (m *Manager) runFlight(f *flight) {
	defer m.wg.Done()
	defer f.cancel()
	xid, err := newExchangeID(m.rand)
	if err != nil {
		m.endFlight(f, err)
		return
	}
	f.xid = xid
	for n := 0; ; n++ {
		resp, err := m.c.Refresh(f.ctx, f.token, f.xid)
		switch {
		case f.ctx.Err() != nil:
			m.endFlight(f, errCancelled)
			return
		case err == nil:
			m.commitRefresh(f, resp)
			return
		case isCode(err, api.CodeInvalidGrant):
			m.commitInvalidGrant(f, apiError(err))
			return
		case !retryable(err):
			m.log.Warn("deviceauth: refresh refused", "origin", m.origin, "err", err)
			m.endFlight(f, fmt.Errorf("deviceauth: refresh: %w", err))
			return
		}
		delay := m.retryDelay(n, err)
		m.log.Debug("deviceauth: refresh failed; retrying", "origin", m.origin, "attempt", n+1, "delay", delay,
			"err", err)
		m.attemptFailed(f, err)
		t := time.NewTimer(delay)
		select {
		case <-f.ctx.Done():
			t.Stop()
			m.endFlight(f, errCancelled)
			return
		case <-f.kick:
			t.Stop()
		case <-t.C:
		}
		m.mu.Lock()
		f.trying = true
		m.mu.Unlock()
	}
}

// retryDelay is the delay before retry n+1 (02 §11.4): the series with ±20 % jitter, or a 429's retryAfter.
func (m *Manager) retryDelay(n int, err error) time.Duration {
	if e := apiError(err); e != nil && e.Code == api.CodeRateLimited && e.RetryAfter > 0 {
		return time.Duration(e.RetryAfter) * time.Second
	}
	base := retryDelays[min(n, len(retryDelays)-1)]
	return jitter(base)
}

// attemptFailed records a failed attempt of f: its waiters are woken, and without a usable token the state is
// offline.
func (m *Manager) attemptFailed(f *flight, err error) {
	m.mu.Lock()
	f.lastErr, f.failed, f.trying = err, true, false
	close(f.attempt)
	f.attempt = make(chan struct{})
	if m.flight == f && m.cred != nil && !m.usableLocked(m.now()) {
		switch m.state.Status {
		case StatusLinked, StatusUnlinked:
			m.state.Status = StatusOffline
			m.changedLocked()
		}
	}
	m.mu.Unlock()
	m.flush()
}

// endFlight ends f with err and wakes its waiters.
func (m *Manager) endFlight(f *flight, err error) {
	m.mu.Lock()
	m.endFlightLocked(f, err)
	m.mu.Unlock()
	m.wakeLoop()
}

func (m *Manager) endFlightLocked(f *flight, err error) {
	if m.flight == f {
		m.flight = nil
	}
	select {
	case <-f.done:
		return
	default:
	}
	f.err, f.trying = err, false
	close(f.attempt)
	close(f.done)
}

// commitRefresh takes a refresh's new pair in 02 §11.4's order: the new refresh token into the credential store,
// then servers.json, then the cached access token, and only then do the waiters get it.
func (m *Manager) commitRefresh(f *flight, resp api.DeviceTokenResponse) {
	m.commitMu.Lock()
	m.mu.Lock()
	stale := m.closed || m.epoch != f.epoch || m.flight != f
	m.mu.Unlock()
	if stale {
		m.commitMu.Unlock()
		m.endFlight(f, errStale)
		return
	}
	now := m.now()
	cred := credstore.Credential{
		RefreshToken: resp.RefreshToken,
		DeviceID:     resp.Device.ID,
		UserName:     resp.User.Username,
		SavedAt:      now,
	}
	saved := m.saveCredential(cred)
	user, device := resp.User, resp.Device
	m.setEntry(entryChange{setLink: true, user: &user, device: &device})
	m.mu.Lock()
	m.cred = &cred
	m.access, m.accessAt = resp.AccessToken, now
	m.accessExp = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
	m.nextDaily = now.Add(dailyRefresh)
	switch m.state.Status {
	case StatusUnlinked, StatusOffline, StatusLinked:
		m.state.Status = StatusLinked
		m.state.Notice = NoticeNone
	}
	m.state.User, m.state.Device = &user, &device
	switch {
	case !saved:
		m.state.Notice = NoticeSaveFailed
	case m.state.Notice == NoticeSaveFailed:
		m.state.Notice = NoticeNone
	}
	m.endFlightLocked(f, nil)
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	m.wakeLoop()
}

// commitInvalidGrant ends the link after a refresh got invalid_grant (02 §11.4): the credential is deleted, the
// entry's user and device cleared, the tokens dropped, and the waiters get an error wrapping ErrNotLinked.
func (m *Manager) commitInvalidGrant(f *flight, e *api.Error) {
	m.commitMu.Lock()
	m.mu.Lock()
	stale := m.closed || m.epoch != f.epoch || m.flight != f
	m.mu.Unlock()
	if stale {
		m.commitMu.Unlock()
		m.endFlight(f, errStale)
		return
	}
	if err := m.store.Delete(m.origin); err != nil {
		m.log.Warn("deviceauth: couldn't delete the credential after invalid_grant", "origin", m.origin, "err", err)
	}
	m.setEntry(entryChange{setLink: true})
	m.mu.Lock()
	n := NoticeSignedOut
	switch {
	case grantReused(e):
		n = NoticeRefreshReused
	case f.endedCode == protocol.ErrorCodeAccountDisabled:
		n = NoticeAccountDisabled
	}
	m.flight = nil // so dropCredentialLocked doesn't cancel f before it ends
	m.dropCredentialLocked(n)
	m.endFlightLocked(f, fmt.Errorf("%w: %w", ErrNotLinked, e))
	m.changedLocked()
	m.mu.Unlock()
	m.commitMu.Unlock()
	m.flush()
	m.log.Info("deviceauth: the server ended this device's sign-in", "origin", m.origin, "notice", string(n))
}

// revokeDevice is the revoke of Unlink and ForgetAll: POST /device/revoke with the cached access token, or with a
// fresh one (one refresh, its own exchange id) when the cached one has 10 s or less left. It reports whether the
// server confirmed that the device is gone: a 204, or an answer saying the token or the grant is already dead.
func revokeDevice(ctx context.Context, c *Client, r io.Reader, cred credstore.Credential, access string, exp,
	now time.Time) bool {
	if access == "" || exp.Sub(now) <= retryTokenLife {
		xid, err := newExchangeID(r)
		if err != nil {
			return false
		}
		resp, err := c.Refresh(ctx, cred.RefreshToken, xid)
		if err != nil {
			return isCode(err, api.CodeInvalidGrant) // the device is gone already
		}
		access = resp.AccessToken
	}
	err := c.Revoke(ctx, access)
	return err == nil || isCode(err, api.CodeInvalidToken) || isCode(err, api.CodeUnauthenticated)
}

// ---- the background loop: the daily refresh and the 10-minute recheck ----

// wakeLoop starts the loop, or makes it look at the state again.
func (m *Manager) wakeLoop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if !m.loopOn {
		m.loopOn = true
		m.wg.Add(1)
		go m.loop()
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

type loopAction int

const (
	actNone loopAction = iota
	actRecheck
	actRefresh
)

func (m *Manager) loop() {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		act, at := actNone, time.Time{}
		switch {
		case m.state.Status == StatusIncompatible:
			act, at = actRecheck, m.nextRecheck
		case m.cred != nil && m.flight == nil && !m.nextDaily.IsZero():
			act, at = actRefresh, m.nextDaily
		}
		now := m.now()
		m.mu.Unlock()
		var tc <-chan time.Time
		var t *time.Timer
		if act != actNone {
			t = time.NewTimer(max(at.Sub(now), 0))
			tc = t.C
		}
		select {
		case <-m.ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-m.wake:
			if t != nil {
				t.Stop()
			}
		case <-tc:
			m.runLoopAction(act)
		}
	}
}

func (m *Manager) runLoopAction(act loopAction) {
	switch act {
	case actRecheck:
		m.mu.Lock()
		m.nextRecheck = m.now().Add(recheckEvery)
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(m.ctx, backgroundTimeout)
		defer cancel()
		if _, err := m.Recheck(ctx); err != nil {
			m.log.Debug("deviceauth: recheck failed", "origin", m.origin, "err", err)
		}
	case actRefresh:
		m.mu.Lock()
		m.nextDaily = m.now().Add(dailyRefresh) // a failed refresh retries on its own; the next daily one is a day away
		if m.cred != nil && m.flight == nil && m.state.Status != StatusIncompatible {
			m.startFlightLocked()
		}
		m.mu.Unlock()
	}
}

// ---- OnState delivery ----

// changedLocked queues the current state for OnState when it differs from the last one queued.
func (m *Manager) changedLocked() {
	if sameState(m.state, m.lastQueued) {
		return
	}
	s := m.state.clone()
	m.lastQueued = s
	if m.onState != nil {
		m.queue = append(m.queue, s)
	}
}

// flush delivers the queued states in order, without holding a lock. When another goroutine is delivering, it
// delivers these too.
func (m *Manager) flush() {
	m.mu.Lock()
	if m.delivering || len(m.queue) == 0 {
		m.mu.Unlock()
		return
	}
	m.delivering = true
	for len(m.queue) > 0 {
		q := m.queue
		m.queue = nil
		m.mu.Unlock()
		for _, s := range q {
			m.deliver(s)
		}
		m.mu.Lock()
	}
	m.delivering = false
	m.mu.Unlock()
}

// deliver calls OnState; a panic in it doesn't leave the queue stuck.
func (m *Manager) deliver(s State) {
	defer func() {
		if r := recover(); r != nil {
			m.mu.Lock()
			m.delivering = false
			m.mu.Unlock()
			panic(r)
		}
	}()
	m.onState(s)
}
