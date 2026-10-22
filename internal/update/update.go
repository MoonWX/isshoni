package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/version"
)

// The updater (06 §8.4–§8.9): states, the schedule, CheckNow, the start-time rules and Apply. fetch.go holds the HTTP
// side (the client, its redirect rule, the limits and the download), state.go the updates folder's state.json, and
// apply_windows.go, policy_windows.go and apply_other.go what differs per OS.
//
// A check is one pass of 06 §8.5's diagram: fetch the manifest and its signature, verify, compare, and, when a newer
// version applies to this copy, download and verify its installer. The download is part of the check, and checks
// never overlap (CheckNow joins a running one), so there is never more than one download; a check that finds a
// version newer than the pending one discards the pending installer first (06 §8.6, "one download at a time").

// State is the updater's state (06 §8.5). 03's UpdateStatus mirrors it in camelCase.
type State string

const (
	StateDisabled    State = "disabled" // not a release build, or no embedded keys
	StateOff         State = "off"      // turned off by the user or by policy
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateUpToDate    State = "up_to_date"
	StateDownloading State = "downloading"
	StateReady       State = "ready" // a verified installer waits
	StateError       State = "error"
)

// Status is what the app shows about updates (06 §8.9, 03 §16.2).
type Status struct {
	State     State
	Current   string    // running version
	Available string    // newer version found, "" otherwise
	Required  bool      // Current < minVersion
	NotesURL  string    // the release page of Available
	Progress  float64   // 0..1 while downloading
	CheckedAt time.Time // zero before the first check
	ErrorCode string    // §8.10, "" unless StateError (or StateDisabled: "no_keys", "not_release")
	PolicyOff bool      // StateOff because of the machine policy (06 §8.8)
}

// Options configures New (06 §8.9).
type Options struct {
	CurrentVersion string           // internal/version.Version(); "" means that
	Channel        func() Channel   // 03's setting, asked at each check; nil means ChannelStable
	Enabled        func() bool      // 03's setting AND !PolicyDisabled(); asked before every request; nil means !PolicyDisabled()
	WizardDone     func() bool      // only the scheduled first check waits for it; CheckNow never does; nil means done
	DataDir        string           // %LOCALAPPDATA%\isshoni (03); the updater uses DataDir\updates
	HTTP           *http.Client     // nil: the updater's own client; either way with the redirect rule of 06 §8.1
	Now            func() time.Time // the wall clock for expiry, CheckedAt and lastCheckAt; nil: time.Now (waits use timers)
	Rand           func() float64   // [0, 1) for the schedule's jitter; nil: math/rand/v2
	Logger         *slog.Logger
}

// Pending is a verified installer that waits to be run (06 §8.7).
type Pending struct {
	Version  string
	Path     string // the installer in DataDir\updates
	Required bool
}

var (
	// ErrNotInstalled is Apply's answer for a copy whose exe is not in HKCU\Software\isshoni\InstallDir (06 §8.7).
	ErrNotInstalled = errors.New("update: not running from the installed folder")
	// ErrUnsupported is Apply's answer on an OS without an apply method (apply_other.go).
	ErrUnsupported = errors.New("update: no apply method on this OS")
)

// The schedule and limits of 06 §8.6 and §8.7.
const (
	firstCheckMin    = 60 * time.Second // the first check: 60–300 s after start
	firstCheckSpread = 240 * time.Second
	wizardPoll       = 5 * time.Second // how often the first scheduled check re-asks WizardDone
	dailyMin         = 23 * time.Hour  // then every 24 h ± 1 h
	dailySpread      = 2 * time.Hour
	retryMin         = 5*time.Hour + 30*time.Minute // after an error: 6 h ± 30 min
	retrySpread      = time.Hour
	checkNowInterval = 30 * time.Second // CheckNow at most once per 30 s

	maxAttempts = 2 // the loop guard: after 2 failed attempts a version is applied no more

	installingPoll = 50 * time.Millisecond // Apply probes the installer's mutex every 50 ms
	installingWait = 10 * time.Second      // for up to 10 s
)

// Updater checks for, downloads and applies updates (06 §8). New starts nothing; Run runs the schedule until its
// context ends. All methods may be called from any goroutine.
type Updater struct {
	o            Options
	log          *slog.Logger
	env          env
	keys         []TrustedKey
	disabledCode string // CodeNotRelease or CodeNoKeys when StateDisabled
	dir          string // DataDir\updates
	client       *http.Client
	ownClient    bool // the client's transport is the updater's own: its idle connections close after each check
	rules        rules

	running   atomic.Bool
	startOnce sync.Once
	startErr  error
	applyMu   sync.Mutex // one Apply at a time

	stateMu sync.Mutex
	st      *stateFile // state.json as last read or written; nil until loaded

	mu       sync.Mutex
	status   Status
	ready    *Pending // non-nil exactly in StateReady
	subs     map[uint64]chan Status
	nextSub  uint64
	inflight *checkCall // the running check
	lastUser *checkCall // the last CheckNow's check, for the 30 s limit
}

// checkCall is one check, which later callers may join.
type checkCall struct {
	done    chan struct{}
	started time.Time // monotonic
	st      Status
	err     error
	cut     bool // it failed because its starter's context ended
}

// env is what the updatetest hooks (06 §8.11) and the tests change: the keys, the URLs and the OS seams.
type env struct {
	signers   func() ([]TrustedKey, error) // EmbeddedSigners
	ep        endpoints
	testBuild bool // updateTestBuild
	sys       system
}

// system is what differs per OS (apply_windows.go, apply_other.go), and the tests' seams.
type system struct {
	goos, goarch string                                                                        // the artifact to select
	installed    func() (bool, error)                                                          // the exe's folder is InstallDir
	freeSpace    func(dir string) (uint64, error)                                              // bytes free for this user; errors.ErrUnsupported: unknown
	launch       func(ctx context.Context, path string, args []string, log *slog.Logger) error // start the installer, wait for its mutex
	policy       func() bool                                                                   // PolicyDisabled
	rename       func(oldpath, newpath string) error                                           // os.Rename
}

// testHooks is set by hooks_updatetest.go's init in builds with the updatetest tag (06 §8.11); release builds have no
// code that sets it.
var testHooks func(*env) error

func defaultEnv() env {
	sys := defaultSystem()
	sys.rename = os.Rename
	return env{signers: EmbeddedSigners, ep: defaultEndpoints(), testBuild: updateTestBuild, sys: sys}
}

// New returns an updater for o (06 §8.9). It starts nothing and makes no request. A build that is not a release
// (Eligible false) or embeds no update key gives an updater in StateDisabled that never makes a request. An error
// means bad options or a hand-edited embedded key block.
func New(o Options) (*Updater, error) {
	e := defaultEnv()
	if testHooks != nil {
		if err := testHooks(&e); err != nil {
			return nil, err
		}
	}
	return newUpdater(o, e)
}

func newUpdater(o Options, e env) (*Updater, error) {
	if o.DataDir == "" || !filepath.IsAbs(o.DataDir) {
		return nil, fmt.Errorf("update: Options.DataDir %q is not an absolute path", o.DataDir)
	}
	if o.CurrentVersion == "" {
		o.CurrentVersion = version.Version()
	}
	if o.Channel == nil {
		o.Channel = func() Channel { return ChannelStable }
	}
	if o.Enabled == nil {
		policy := e.sys.policy
		o.Enabled = func() bool { return !policy() }
	}
	if o.WizardDone == nil {
		o.WizardDone = func() bool { return true }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	keys, err := e.signers()
	if err != nil {
		return nil, err
	}
	u := &Updater{
		o:     o,
		log:   o.Logger.With("component", "update"),
		env:   e,
		keys:  keys,
		dir:   filepath.Join(o.DataDir, "updates"),
		rules: rules{releases: e.ep.releases, testBuild: e.testBuild},
		subs:  make(map[uint64]chan Status),
	}
	u.client = newClient(o.HTTP, e.ep, u.enabled)
	u.ownClient = o.HTTP == nil || o.HTTP.Transport == nil
	switch {
	case !eligible(o.CurrentVersion, e.testBuild):
		u.disabledCode = CodeNotRelease
	case len(keys) == 0:
		u.disabledCode = CodeNoKeys
	}
	u.status = Status{State: StateIdle, Current: o.CurrentVersion}
	if u.disabledCode != "" {
		u.status.State, u.status.ErrorCode = StateDisabled, u.disabledCode
	}
	return u, nil
}

// Status returns the current status.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// Subscribe returns a channel that holds the latest status, for 03's binding: it has the current status at once and
// then each change; a reader that falls behind gets only the latest. cancel removes the subscription and closes the
// channel; it may be called more than once.
func (u *Updater) Subscribe() (<-chan Status, func()) {
	ch := make(chan Status, 1)
	u.mu.Lock()
	id := u.nextSub
	u.nextSub++
	u.subs[id] = ch
	ch <- u.status
	u.mu.Unlock()
	return ch, sync.OnceFunc(func() {
		u.mu.Lock()
		delete(u.subs, id)
		close(ch)
		u.mu.Unlock()
	})
}

// Ready returns the verified installer that waits, the source of "Restart and update" (03 §16.2); it is non-nil only
// in StateReady.
func (u *Updater) Ready() *Pending {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ready == nil {
		return nil
	}
	p := *u.ready
	return &p
}

// set publishes st (and the installer that waits, non-nil only with StateReady) to Status and the subscribers.
func (u *Updater) set(st Status, ready *Pending) {
	st.Current = u.o.CurrentVersion
	if st.State != StateReady {
		ready = nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.ready = st, ready
	for _, ch := range u.subs {
		select { // keep only the latest value: drain, then send; only set sends, under mu, so the send can't block
		case <-ch:
		default:
		}
		ch <- st
	}
}

// enabled is Options.Enabled, asked before every request (06 §8.8).
func (u *Updater) enabled() bool { return u.o.Enabled() }

// channel returns the app's channel; an unknown value counts as stable (06 §8.4).
func (u *Updater) channel() Channel {
	if c := u.o.Channel(); c == ChannelBeta {
		return c
	}
	return ChannelStable
}

// Run runs the schedule of 06 §8.6 until ctx ends, and then returns nil: the first check 60–300 s after it starts
// and not before WizardDone, then every 24 h ± 1 h, or 6 h ± 30 min after an error. At its start it does the
// start-time cleanup of 06 §8.7 (as PendingAtStartup does, once per Updater). A disabled updater only waits for ctx.
func (u *Updater) Run(ctx context.Context) error {
	if !u.running.CompareAndSwap(false, true) {
		return errors.New("update: Run is already running")
	}
	defer u.running.Store(false)
	if u.disabledCode != "" {
		<-ctx.Done()
		return nil
	}
	if u.enabled() {
		_ = u.startup() // logged there; the checks go on
	} else {
		u.turnOff()
	}
	if !sleep(ctx, firstCheckMin+u.jitter(firstCheckSpread)) {
		return nil
	}
	for !u.o.WizardDone() {
		if !sleep(ctx, wizardPoll) {
			return nil
		}
	}
	for {
		st, _ := u.runCheck(ctx, false)
		if ctx.Err() != nil {
			return nil
		}
		var next time.Duration
		if st.State == StateError {
			next = retryMin + u.jitter(retrySpread)
		} else {
			next = dailyMin + u.jitter(dailySpread)
		}
		if !sleep(ctx, next) {
			return nil
		}
	}
}

// jitter returns a uniform random duration in [0, spread).
func (u *Updater) jitter(spread time.Duration) time.Duration {
	r := u.o.Rand()
	if r < 0 || r >= 1 {
		r = 0.5
	}
	return time.Duration(r * float64(spread))
}

// sleep waits for d or the end of ctx, and reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// CheckNow runs a check now, as a user action (06 §8.6): it never waits for WizardDone, and with Enabled false it
// makes no request and returns StateOff. At most one check starts per 30 s: a call while any check runs returns that
// check's result, and a call within 30 s of the last CheckNow that checked returns the current status (with that
// check's error while the status is the one it left). The check includes the download, so a newer version that
// applies to this copy returns StateReady (or an error). The error is the check's (an *Error with its code), or
// ctx's.
func (u *Updater) CheckNow(ctx context.Context) (Status, error) { return u.runCheck(ctx, true) }

// runCheck runs one check, or joins the one that runs (and, for CheckNow, the last CheckNow's within 30 s).
//
// A check runs on the context of the caller that started it. When that context ends the check (a binding call
// cancelled mid-download, say), a joined caller whose own context is live doesn't take that caller's error: it runs
// (or joins) a check of its own. Only a CheckNow that checked counts for the 30 s limit: one that ended with its
// context, or found the updater off or disabled, made no complete check, so the next CheckNow checks at once.
func (u *Updater) runCheck(ctx context.Context, user bool) (Status, error) {
	for {
		u.mu.Lock()
		c, finished := u.inflight, false
		if c == nil && user && u.lastUser != nil && time.Since(u.lastUser.started) < checkNowInterval {
			c, finished = u.lastUser, true
		}
		if c == nil {
			break // start one, below, with mu held
		}
		u.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return u.Status(), ctx.Err()
		}
		if c.cut && ctx.Err() == nil {
			continue // the check ended with its starter's context, not with ours
		}
		if st := u.Status(); finished && st != c.st {
			return st, nil // changed since the check (Apply, a scheduled check): the current status, not its error
		}
		return c.st, c.err
	}
	c := &checkCall{done: make(chan struct{}), started: time.Now()}
	u.inflight = c
	if user {
		u.lastUser = c
	}
	u.mu.Unlock()

	st, err := u.check(ctx)
	if u.ownClient { // the next check is hours away: keep no connection, nor its goroutines, open until then
		u.client.CloseIdleConnections()
	}

	u.mu.Lock()
	c.st, c.err = st, err
	c.cut = err != nil && ctx.Err() != nil
	if u.lastUser == c && (c.cut || st.State == StateOff || st.State == StateDisabled) {
		u.lastUser = nil
	}
	u.inflight = nil
	u.mu.Unlock()
	close(c.done)
	return st, err
}

// check is one pass of 06 §8.5's diagram.
func (u *Updater) check(ctx context.Context) (Status, error) {
	if u.disabledCode != "" {
		return u.Status(), nil
	}
	if !u.enabled() {
		return u.turnOff(), nil
	}
	prev := u.Status()
	wasReady := prev.State == StateReady
	if !wasReady { // a ready installer stays offered while a later check runs
		u.set(Status{State: StateChecking}, nil)
	}
	ch := u.channel()
	v, err := u.fetchVerified(ctx, ch)
	if err != nil {
		return u.checkFailed(ctx, prev, err)
	}
	now := u.o.Now()
	cur := u.o.CurrentVersion
	newer, err := v.Newer(cur)
	if err != nil {
		return u.checkFailed(ctx, prev, &Error{Code: CodeBadManifest, Err: err})
	}
	if !newer {
		u.dropPending("the release is not newer than this version")
		u.noteCheck(now)
		u.log.Debug("up to date", "version", cur, "latest", v.Version)
		st := Status{State: StateUpToDate, CheckedAt: now}
		u.set(st, nil)
		return u.Status(), nil
	}
	required, _ := v.Required(cur) // cur parsed in Newer
	found := Status{Available: v.Version, Required: required, NotesURL: v.NotesURL, CheckedAt: now}
	u.noteCheck(now)
	if v.Expired(now) {
		return u.fail(found, &Error{Code: CodeManifestExpired,
			Err: fmt.Errorf("the manifest of %s expired at %s (check the clock)", v.Version, wireTime{v.ExpiresAt})})
	}
	a, ok := v.Artifact(u.env.sys.goos, u.env.sys.goarch)
	if !ok {
		return u.fail(found, &Error{Code: CodeNoArtifact,
			Err: fmt.Errorf("%s has no installer for %s/%s", v.Version, u.env.sys.goos, u.env.sys.goarch)})
	}
	if ok, err := u.env.sys.installed(); !ok {
		if err != nil {
			u.log.Debug("can't tell whether this copy is installed", logx.Err(err))
		}
		return u.fail(found, &Error{Code: CodeNotInstalled, Err: errors.New("this copy is not in the installed folder")})
	}
	if rec := u.state().Pending; rec != nil && rec.Version == v.Version {
		if rec.Attempts >= maxAttempts {
			return u.fail(found, &Error{Code: CodeInstallFailed,
				Err: fmt.Errorf("%s failed to install %d times", v.Version, rec.Attempts)})
		}
		if rec.File == a.Name && rec.Size == a.Size && rec.SHA256 == a.SHA256 {
			if err := verifyFile(filepath.Join(u.dir, rec.File), rec.Size, rec.SHA256); err == nil {
				return u.becomeReady(found, rec)
			}
		}
	}
	u.dropPending("the latest release is downloaded instead")
	return u.download(ctx, v, a, found)
}

// checkFailed ends a check that failed before it found a newer version: a context that ended restores the status
// from before the check, a ready installer stays offered (the error is only logged), and anything else is StateError
// with the error's code.
func (u *Updater) checkFailed(ctx context.Context, prev Status, err error) (Status, error) {
	if errors.Is(err, errTurnedOff) {
		return u.turnOff(), nil
	}
	if ctx.Err() != nil {
		u.set(prev, u.Ready())
		return u.Status(), ctx.Err()
	}
	code := CodeOf(err)
	if code == "" {
		code, err = CodeNetwork, &Error{Code: CodeNetwork, Err: err}
	}
	if prev.State == StateReady {
		u.log.Warn("update check failed; the downloaded update stays ready", "code", code, logx.Err(err))
		u.set(prev, u.Ready())
		return prev, err
	}
	return u.fail(Status{CheckedAt: u.o.Now()}, err)
}

// fail publishes StateError with err's code and the fields of st, and returns them.
func (u *Updater) fail(st Status, err error) (Status, error) {
	code := CodeOf(err)
	if code == "" {
		code = CodeNetwork
	}
	st.State, st.ErrorCode, st.Progress = StateError, code, 0
	if code == CodeNotInstalled { // a copy run from a folder: expected, and seen at every check
		u.log.Info("update available, but this copy is not installed", "code", code, "available", st.Available)
	} else {
		u.log.Warn("update check failed", "code", code, "available", st.Available, logx.Err(err))
	}
	u.set(st, nil)
	return u.Status(), err
}

// turnOff publishes StateOff (06 §8.8).
func (u *Updater) turnOff() Status {
	u.set(Status{State: StateOff, PolicyOff: u.env.sys.policy()}, nil)
	return u.Status()
}

// becomeReady publishes StateReady for the pending installer rec.
func (u *Updater) becomeReady(st Status, rec *pendingRec) (Status, error) {
	st.State, st.Available, st.Required, st.Progress, st.ErrorCode = StateReady, rec.Version, rec.Required, 0, ""
	if st.NotesURL == "" {
		st.NotesURL = rec.NotesURL
	}
	u.set(st, &Pending{Version: rec.Version, Path: filepath.Join(u.dir, rec.File), Required: rec.Required})
	return u.Status(), nil
}

// fetchVerified fetches the manifest and its signature of ch, verifies them (06 §8.3) and checks the channel. The
// revocations a backup-signed manifest honours are persisted here, whether or not the manifest is newer (06 §8.1).
func (u *Updater) fetchVerified(ctx context.Context, ch Channel) (*Verified, error) {
	data, sig, err := u.fetchManifest(ctx, ch)
	if err != nil {
		return nil, err
	}
	v, err := verifyManifest(data, sig, u.keys, u.state().RevokedKeys, u.rules)
	if err != nil {
		return nil, err
	}
	if v.IgnoredRevocations > 0 {
		u.log.Info("revocations in the update manifest ignored", "count", v.IgnoredRevocations, "backup", v.Signer.Backup)
	}
	if v.Revoked != nil {
		if err := u.updateState(func(st *stateFile) { st.RevokedKeys = v.Revoked }); err != nil {
			return nil, &Error{Code: CodeDisk, Err: fmt.Errorf("persist the revoked update keys: %w", err)}
		}
		u.log.Warn("update keys revoked by the backup key", "revoked", len(v.Revoked))
	}
	if err := v.checkChannel(ch, u.env.testBuild); err != nil {
		return nil, err
	}
	return v, nil
}

// noteCheck records the time of a check that verified a manifest (state.json's lastCheckAt).
func (u *Updater) noteCheck(now time.Time) {
	if err := u.updateState(func(st *stateFile) { st.LastCheckAt = wireTime{now} }); err != nil {
		u.log.Warn("can't write the update state", logx.Err(err))
	}
}

// PendingAtStartup returns the update to apply at this start, before the app's window opens (06 §8.7 (b)): a pending
// installer that is newer than the running version, not expired, verifies and has had fewer than 2 attempts. It is
// nil, nil when nothing should be applied now, and always nil when autostart is true (a start with --autostart,
// 03 §16.1; the update stays ready). The first call (or Run) also does the start-time cleanup: files in the updates
// folder that are not the pending version's are deleted, and pending is cleared once the running version is at
// least its version. A disabled or turned-off updater does nothing. An error (state.json, or a pending installer
// that no longer verifies, which is deleted) is for the log; the start goes on.
func (u *Updater) PendingAtStartup(autostart bool) (*Pending, error) {
	if u.disabledCode != "" {
		return nil, nil
	}
	if !u.enabled() {
		u.turnOff()
		return nil, nil
	}
	err := u.startup()
	if autostart {
		return nil, err
	}
	return u.Ready(), err
}

// startup runs the start-time cleanup once.
func (u *Updater) startup() error {
	u.startOnce.Do(func() { u.startErr = u.doStartup() })
	return u.startErr
}

func (u *Updater) doStartup() error {
	var verr error
	u.stateMu.Lock()
	st := u.loadLocked()
	rec := st.Pending
	cur := u.o.CurrentVersion
	if rec != nil {
		c, err := version.Compare(rec.Version, cur)
		switch {
		case err != nil || c <= 0:
			if err == nil && c == 0 {
				u.log.Info("update installed", "version", cur)
			}
			rec = nil
		case u.o.Now().After(rec.ExpiresAt.Time):
			u.log.Info("the pending update expired", "version", rec.Version)
			rec = nil
		case rec.Attempts >= maxAttempts:
			u.log.Warn("the pending update failed to install; not applied again", "version", rec.Version,
				"attempts", rec.Attempts)
		default:
			if err := verifyFile(filepath.Join(u.dir, rec.File), rec.Size, rec.SHA256); err != nil {
				verr = &Error{Code: CodeHashMismatch, Err: err}
				u.log.Warn("the pending update no longer verifies; deleted", "version", rec.Version, logx.Err(err))
				rec = nil
			}
		}
	}
	var werr error
	if rec != st.Pending {
		next := st.clone()
		next.Pending = rec
		werr = u.saveLocked(next)
	}
	keep := ""
	if rec != nil {
		keep = rec.File
	}
	u.cleanLocked(keep)
	u.stateMu.Unlock()

	switch {
	case rec != nil && rec.Attempts >= maxAttempts:
		u.installFailed(rec)
	case rec != nil:
		_, _ = u.becomeReady(Status{}, rec)
	case verr != nil:
		u.set(Status{State: StateError, ErrorCode: CodeHashMismatch}, nil)
	}
	if werr != nil {
		u.log.Warn("can't write the update state", logx.Err(werr))
	}
	return errors.Join(verr, werr)
}

// Apply runs the installer of p (06 §8.7): it verifies the file again (size and SHA-256 against state.json; a
// mismatch deletes it and returns CodeHashMismatch), returns ErrNotInstalled when the exe is not in InstallDir,
// counts the attempt in state.json, starts the installer with /S /UPDATE (and /RELAUNCH when relaunch is true), waits
// (≤ 10 s) until the installer's mutex Local\io.isshoni.desktop.installing exists, and returns. The app then quits
// within 5 s (03). After 10 s without the mutex it logs a WARN and returns nil anyway. The loop guard: once 2
// attempts failed to start, or for a pending update that has had 2, it publishes error/install_failed (Ready() is then
// nil) and returns CodeInstallFailed. Where there is no apply method it returns ErrUnsupported.
func (u *Updater) Apply(ctx context.Context, p *Pending, relaunch bool) error {
	if p == nil {
		return errors.New("update: Apply without a pending update")
	}
	if u.disabledCode != "" {
		return &Error{Code: u.disabledCode, Err: errors.New("the updater is disabled")}
	}
	u.applyMu.Lock()
	defer u.applyMu.Unlock()
	rec := u.state().Pending
	if rec == nil || rec.Version != p.Version || filepath.Join(u.dir, rec.File) != p.Path {
		return fmt.Errorf("update: no pending update %s", p.Version)
	}
	path := filepath.Join(u.dir, rec.File)
	if err := verifyFile(path, rec.Size, rec.SHA256); err != nil {
		u.dropPending("it no longer verifies")
		u.set(Status{State: StateError, ErrorCode: CodeHashMismatch, Available: rec.Version, Required: rec.Required,
			NotesURL: rec.NotesURL}, nil)
		u.log.Warn("the update no longer verifies; deleted", "version", rec.Version, logx.Err(err))
		return &Error{Code: CodeHashMismatch, Err: err}
	}
	switch c, err := version.Compare(rec.Version, u.o.CurrentVersion); {
	case err != nil || c <= 0:
		return fmt.Errorf("update: %s is not newer than %s", rec.Version, u.o.CurrentVersion)
	case u.o.Now().After(rec.ExpiresAt.Time):
		return &Error{Code: CodeManifestExpired, Err: fmt.Errorf("the update %s expired", rec.Version)}
	case rec.Attempts >= maxAttempts:
		u.installFailed(rec)
		return &Error{Code: CodeInstallFailed, Err: fmt.Errorf("%s failed to install %d times", rec.Version, rec.Attempts)}
	}
	installed, err := u.env.sys.installed()
	switch {
	case errors.Is(err, ErrUnsupported):
		return ErrUnsupported
	case err != nil:
		u.log.Warn("can't tell whether this copy is installed", logx.Err(err))
		return ErrNotInstalled
	case !installed:
		return ErrNotInstalled
	}
	attempts := rec.Attempts + 1
	if err := u.updateState(func(st *stateFile) {
		if st.Pending != nil && st.Pending.Version == rec.Version {
			st.Pending.Attempts = attempts
		}
	}); err != nil { // without the count the loop guard can't work: don't start
		return &Error{Code: CodeDisk, Err: fmt.Errorf("count the attempt: %w", err)}
	}
	args := []string{"/S", "/UPDATE"}
	if relaunch {
		args = append(args, "/RELAUNCH")
	}
	u.log.Info("starting the installer", "version", rec.Version, "attempt", attempts)
	err = u.env.sys.launch(ctx, path, args, u.log)
	// A launch that failed (Smart App Control or an antivirus blocked the installer, say) leaves the app running. The
	// last allowed attempt then trips the loop guard now, not at the next start or check: "Restart and update" goes
	// and 03 shows "The update didn't install" (06 §8.5, §8.7). A context that ended while Apply waited for the
	// installer's mutex is not a failed launch: the installer was started.
	if err != nil && attempts >= maxAttempts && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		u.log.Warn("the installer failed to start; the update is not applied again", "version", rec.Version,
			"attempts", attempts, logx.Err(err))
		u.installFailed(rec)
	}
	return err
}

// installFailed publishes the tripped loop guard for rec (06 §8.5): StateError with install_failed and Available,
// until a check finds a newer version.
func (u *Updater) installFailed(rec *pendingRec) {
	u.set(Status{State: StateError, ErrorCode: CodeInstallFailed, Available: rec.Version, Required: rec.Required,
		NotesURL: rec.NotesURL, CheckedAt: u.Status().CheckedAt}, nil)
}

// waitInstalling polls exists, the probe of the installer's mutex Local\io.isshoni.desktop.installing, every 50 ms
// until it reports the mutex, for up to 10 s (06 §8.7). Then it returns nil anyway after one WARN: the installer was
// started, and the app quits as it would have. Probe errors are logged once at DEBUG and count as "not yet".
func waitInstalling(ctx context.Context, exists func() (bool, error), log *slog.Logger) error {
	deadline := time.NewTimer(installingWait)
	defer deadline.Stop()
	tick := time.NewTicker(installingPoll)
	defer tick.Stop()
	logged := false
	for {
		ok, err := exists()
		if ok {
			return nil
		}
		if err != nil && !logged {
			log.Debug("can't probe the installer's mutex", logx.Err(err))
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			log.Warn("the installer did not show up within 10 s; quitting anyway", "wait", installingWait)
			return nil
		case <-tick.C:
		}
	}
}
