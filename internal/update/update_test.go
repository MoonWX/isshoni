package update

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"
)

// fakeInstallerMain is the fake installer of apply_windows_test.go; TestMain runs it instead of the tests when this
// binary is started as one (fakeInstallerEnv set).
var fakeInstallerMain func() int

const fakeInstallerEnv = "UPDATE_TEST_FAKE_INSTALLER"

// TestMain fails the run when a goroutine outlives the tests (README "Testing"): Run, the checks and the HTTP
// connections start some. Most tests also run in testing/synctest bubbles, which fail a test whose goroutines are
// still blocked when it returns.
func TestMain(m *testing.M) {
	if fakeInstallerMain != nil && os.Getenv(fakeInstallerEnv) != "" {
		os.Exit(fakeInstallerMain())
	}
	goleak.VerifyTestMain(m)
}

// The schedule (06 §8.6): the first check 60–300 s after start and not before the wizard.
func TestScheduleFirstCheck(t *testing.T) {
	for _, tt := range []struct {
		name string
		rand float64
		at   time.Duration
	}{
		{"earliest", 0, 60 * time.Second},
		{"middle", 0.5, 180 * time.Second},
		{"latest", 0.9999, 300 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
				h.setRands(tt.rand, 0.5)
				stop := h.run()
				defer stop()
				time.Sleep(tt.at - time.Second)
				synctest.Wait()
				if n := h.checks(); n != 0 {
					t.Fatalf("%d checks before %v", n, tt.at)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if n := h.checks(); n != 1 {
					t.Fatalf("%d checks at %v, want 1", n, tt.at)
				}
				st := h.u.Status()
				if at := st.CheckedAt.Sub(testEpoch); st.State != StateUpToDate || at <= tt.at-time.Second || at > tt.at {
					t.Errorf("status %+v, want up_to_date checked within a second before %v", st, tt.at)
				}
			})
		})
	}
}

func TestScheduleWaitsForWizard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		h.wizard.Store(false)
		h.setRands(0)
		stop := h.run()
		defer stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := h.checks(); n != 0 {
			t.Fatalf("%d checks before the wizard is done", n)
		}
		h.wizard.Store(true)
		time.Sleep(wizardPoll)
		synctest.Wait()
		if n := h.checks(); n != 1 {
			t.Fatalf("%d checks right after the wizard, want 1", n)
		}
	})
}

// Then every 24 h ± 1 h, and 6 h ± 30 min after an error.
func TestScheduleDailyAndRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		// The first check at 60 s (rand 0); then 23 h (rand 0); 24 h 30 min (rand 0.75); 24 h (rand 0.5), which fails;
		// then 5 h 30 min (rand 0), which fails again; 6 h 15 min (rand 0.75), which works; then daily again, 24 h.
		h.setRands(0, 0, 0.75, 0.5, 0, 0.75, 0.5)
		stop := h.run()
		defer stop()
		at := func(d time.Duration, want int) {
			t.Helper()
			time.Sleep(d - time.Second)
			synctest.Wait()
			if n := h.checks(); n != want-1 {
				t.Fatalf("%d checks a second before the next one, want %d", n, want-1)
			}
			time.Sleep(time.Second)
			synctest.Wait()
			if n := h.checks(); n != want {
				t.Fatalf("%d checks, want %d", n, want)
			}
		}
		at(60*time.Second, 1)
		at(23*time.Hour, 2)
		at(24*time.Hour+30*time.Minute, 3)
		if st := h.u.Status(); st.State != StateUpToDate {
			t.Fatalf("status %+v", st)
		}
		h.gh.with(func(fg *fakeGitHub) { fg.latest = "" }) // 404: a network error
		at(24*time.Hour, 4)
		if st := h.u.Status(); st.State != StateError || st.ErrorCode != CodeNetwork {
			t.Fatalf("status %+v, want error/network", st)
		}
		at(5*time.Hour+30*time.Minute, 5)
		h.gh.with(func(fg *fakeGitHub) { fg.latest = testCurrent })
		at(6*time.Hour+15*time.Minute, 6)
		if st := h.u.Status(); st.State != StateUpToDate {
			t.Fatalf("status %+v, want up_to_date", st)
		}
		at(24*time.Hour, 7)
	})
}

// CheckNow never waits for the wizard, and honours Enabled.
func TestCheckNowBeforeWizard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		h.wizard.Store(false)
		h.checkNow(StateUpToDate, "")
		if n := h.checks(); n != 1 {
			t.Fatalf("%d checks, want 1", n)
		}
		n := len(h.gh.requests())
		h.enabled.Store(false)
		time.Sleep(checkNowInterval)
		h.checkNow(StateOff, "")
		if got := len(h.gh.requests()); got != n {
			t.Errorf("%d requests while off", got-n)
		}
	})
}

// Enabled false: the server counts zero requests, whatever is asked.
func TestEnabledFalseMakesNoRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.stage(rel, 0)
		h.enabled.Store(false)
		h.policy.Store(true)
		h.u = h.newUpdater()
		if p, err := h.u.PendingAtStartup(false); p != nil || err != nil {
			t.Errorf("PendingAtStartup = %+v, %v; want nil while off", p, err)
		}
		stop := h.run()
		time.Sleep(72 * time.Hour)
		synctest.Wait()
		st := h.checkNow(StateOff, "")
		if !st.PolicyOff {
			t.Errorf("status %+v, want PolicyOff", st)
		}
		stop()
		if reqs := h.gh.requests(); len(reqs) != 0 {
			t.Errorf("requests while off: %v", reqs)
		}
		if h.u.Ready() != nil {
			t.Error("Ready() non-nil while off")
		}
	})
}

// Enabled is asked before every request: turned off after the manifest, the signature is never asked for.
func TestEnabledTurnedOffMidCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/"+ManifestName) && r.Host == assetHost {
				h.enabled.Store(false)
			}
			return false
		})
		h.checkNow(StateOff, "")
		if n := h.gh.count(SignatureName); n != 0 {
			t.Errorf("%d signature requests after Enabled turned false", n)
		}
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == repoPath+"/latest/download/"+ManifestName {
				h.enabled.Store(false) // before the redirect is followed
			}
			return false
		})
		h.enabled.Store(true)
		time.Sleep(checkNowInterval)
		n := len(h.gh.requests())
		h.checkNow(StateOff, "")
		if got := len(h.gh.requests()) - n; got != 1 {
			t.Errorf("%d requests, want 1: the redirect is a request too", got)
		}
	})
}

// CheckNow: at most once per 30 s, and a second call returns the running check's result.
func TestCheckNowDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		h.checkNow(StateUpToDate, "")
		time.Sleep(checkNowInterval - time.Second)
		h.checkNow(StateUpToDate, "")
		if n := h.checks(); n != 1 {
			t.Fatalf("%d checks within 30 s, want 1", n)
		}
		time.Sleep(time.Second)
		h.checkNow(StateUpToDate, "")
		if n := h.checks(); n != 2 {
			t.Fatalf("%d checks after 30 s, want 2", n)
		}

		// While a check runs, other calls join it.
		release := make(chan struct{})
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == repoPath+"/latest/download/"+ManifestName {
				<-release
			}
			return false
		})
		time.Sleep(checkNowInterval)
		results := make(chan Status, 3)
		for range 3 {
			go func() {
				st, _ := h.u.CheckNow(t.Context())
				results <- st
			}()
		}
		synctest.Wait()
		if st := h.u.Status(); st.State != StateChecking {
			t.Errorf("status %+v while a check runs, want checking", st)
		}
		close(release)
		for range 3 {
			if st := <-results; st.State != StateUpToDate {
				t.Errorf("a joined CheckNow = %+v", st)
			}
		}
		if n := h.checks(); n != 3 {
			t.Errorf("%d checks, want 3: the three calls share one", n)
		}
	})
}

// The 30 s limit counts checks: a CheckNow that found the updater off made none, so once it is turned on the next
// CheckNow checks at once.
func TestCheckNowAfterTurnedOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		h.enabled.Store(false)
		h.checkNow(StateOff, "")
		h.enabled.Store(true)
		time.Sleep(time.Second)
		h.checkNow(StateUpToDate, "")
		if n := h.checks(); n != 1 {
			t.Errorf("%d manifest requests, want 1", n)
		}
	})
}

// hangDownload makes the fake GitHub send half of rel's installer and then hang until the client gives up, while
// hang is true.
func hangDownload(h *harness, rel release, hang *atomic.Bool) {
	h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Host != assetHost || !strings.HasSuffix(r.URL.Path, rel.name) || !hang.Load() {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(rel.installer)))
		_, _ = w.Write(rel.installer[:len(rel.installer)/2])
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-h.gh.quit:
		}
		return true
	})
}

// A CheckNow cancelled mid-download (its binding call cancelled) is no check for the 30 s limit: the next CheckNow
// with a live context checks, and downloads, at once.
func TestCheckNowCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(1000, 2))
		var hang atomic.Bool
		hang.Store(true)
		hangDownload(h, rel, &hang)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := h.u.CheckNow(ctx)
			done <- err
		}()
		synctest.Wait()
		if st := h.u.Status(); st.State != StateDownloading {
			t.Fatalf("status %+v, want downloading", st)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled CheckNow = %v, want context.Canceled", err)
		}
		hang.Store(false)
		time.Sleep(time.Second)
		h.checkNow(StateReady, "")
		if n := h.checks(); n != 2 {
			t.Errorf("%d manifest requests, want 2", n)
		}
	})
}

// A check that joined a CheckNow whose context then ended runs its own check rather than taking context.Canceled:
// the schedule would otherwise wait a whole day.
func TestJoinedCheckOutlivesCancelledCheckNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(1000, 2))
		var hang atomic.Bool
		hang.Store(true)
		hangDownload(h, rel, &hang)
		ctx, cancel := context.WithCancel(t.Context())
		user := make(chan error, 1)
		go func() {
			_, err := h.u.CheckNow(ctx)
			user <- err
		}()
		synctest.Wait()
		type result struct {
			st  Status
			err error
		}
		scheduled := make(chan result, 1)
		go func() {
			st, err := h.u.runCheck(t.Context(), false)
			scheduled <- result{st, err}
		}()
		synctest.Wait()
		if n := h.checks(); n != 1 {
			t.Fatalf("%d manifest requests, want 1: the scheduled check joins the running one", n)
		}
		hang.Store(false)
		cancel()
		if err := <-user; !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled CheckNow = %v, want context.Canceled", err)
		}
		if r := <-scheduled; r.st.State != StateReady || r.err != nil {
			t.Errorf("the joined check = %+v, %v; want its own check, ready", r.st, r.err)
		}
		if n := h.checks(); n != 2 {
			t.Errorf("%d manifest requests, want 2", n)
		}
	})
}

// A full update: the manifest, the signature and the installer through GitHub's redirects, then ready.
func TestCheckDownloadsAndReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(300<<10, 2))
		var duringDownload *Pending
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == repoPath+"/latest/download/"+ManifestName {
				if st := h.u.Status(); st.State != StateChecking {
					t.Errorf("status during the manifest request %+v", st)
				}
			}
			if r.Host == assetHost && strings.HasSuffix(r.URL.Path, rel.name) {
				duringDownload = h.u.Ready()
				if st := h.u.Status(); st.State != StateDownloading || st.Available != "0.2.1" {
					t.Errorf("status during the download %+v", st)
				}
			}
			return false
		})
		st := h.checkNow(StateReady, "")
		if st.Available != "0.2.1" || st.Required || st.NotesURL != ReleasesURL+"/tag/v0.2.1" || st.Progress != 0 ||
			st.Current != testCurrent {
			t.Errorf("status %+v", st)
		}
		if duringDownload != nil {
			t.Errorf("Ready() during the download = %+v, want nil", duringDownload)
		}
		p := h.u.Ready()
		if want := (Pending{Version: "0.2.1", Path: filepath.Join(h.dir, rel.name)}); p == nil || *p != want {
			t.Fatalf("Ready() = %+v, want %+v", p, want)
		}
		h.wantFiles(stateName, rel.name)
		data, err := os.ReadFile(p.Path)
		if err != nil || string(data) != string(rel.installer) {
			t.Fatalf("the installer on disk differs: %v", err)
		}
		pending := h.diskState().Pending
		if pending == nil || pending.Version != "0.2.1" || pending.File != rel.name || pending.Attempts != 0 ||
			pending.Size != int64(len(rel.installer)) {
			t.Errorf("state.json pending %+v", pending)
		}
		// The requests: GitHub's redirects followed, the updater's User-Agent, no cookie and no Referer.
		for _, r := range h.gh.requests() {
			if ua := r.header.Get("User-Agent"); ua != "isshoni-desktop/"+testCurrent {
				t.Errorf("%s: User-Agent %q", r, ua)
			}
			if c := r.header.Get("Cookie"); c != "" {
				t.Errorf("%s: Cookie %q", r, c)
			}
			if ref := r.header.Get("Referer"); ref != "" {
				t.Errorf("%s: Referer %q", r, ref)
			}
		}
		if n := h.gh.count(assetHost); n != 3 {
			t.Errorf("%d requests to %s, want 3 (manifest, signature, installer)", n, assetHost)
		}
		for _, l := range h.logs.lines() {
			if strings.Contains(l.text, "X-Amz") || strings.Contains(l.text, "?") {
				t.Errorf("a log line names a signed URL: %s", l.text)
			}
		}

		// A later check of the same version keeps it, without a second download, and without leaving StateReady.
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if st := h.u.Status(); st.State != StateReady {
				t.Errorf("status during a later check %+v, want ready", st)
			}
			return false
		})
		time.Sleep(checkNowInterval)
		h.checkNow(StateReady, "")
		if n := h.gh.count(rel.name); n != 2 {
			t.Errorf("%d installer requests (redirect and asset), want the first download's 2", n)
		}
	})
}

// Required: the running version is below the manifest's minVersion.
func TestCheckRequired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, "0.3.0", installerBytes(100, 3), func(f map[string]any) { f["minVersion"] = "0.2.1" })
		st := h.checkNow(StateReady, "")
		if !st.Required || !h.u.Ready().Required || !h.diskState().Pending.Required {
			t.Errorf("status %+v, Ready %+v: want required", st, h.u.Ready())
		}
	})
}

func TestCheckUpToDate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		// An older version, and an expired one that is not newer: both are just up to date (06 §8.5).
		h.gh.publish(h.active, "0.1.9", installerBytes(100, 1))
		h.checkNow(StateUpToDate, "")
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1), func(f map[string]any) {
			f["publishedAt"], f["expiresAt"] = "2026-01-01T00:00:00.000Z", "2026-06-30T00:00:00.000Z"
		})
		time.Sleep(checkNowInterval)
		st := h.checkNow(StateUpToDate, "")
		if st.Available != "" || h.u.Ready() != nil {
			t.Errorf("status %+v", st)
		}
		if got := h.diskState().LastCheckAt; !got.Equal(h.now()) {
			t.Errorf("lastCheckAt %v, want %v", got, h.now())
		}
		if n := h.gh.count("_setup.exe"); n != 0 {
			t.Errorf("%d installer requests", n)
		}
	})
}

// The error states of a newer manifest (06 §8.5).
func TestCheckErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version string
		edit    func(f map[string]any)
		setup   func(h *harness)
		code    string
	}{
		{name: "expired", version: "0.2.1", code: CodeManifestExpired, edit: func(f map[string]any) {
			f["publishedAt"], f["expiresAt"] = "2026-01-01T00:00:00.000Z", "2027-01-15T11:59:59.999Z"
		}},
		{name: "no artifact for this OS", version: "0.2.1", code: CodeNoArtifact,
			setup: func(h *harness) { h.u.env.sys.goarch = "arm64" }},
		{name: "not installed", version: "0.2.1", code: CodeNotInstalled,
			setup: func(h *harness) { h.installed.Store(false) }},
		{name: "unsupported schema", version: "0.2.1", code: CodeManifestUnsupported,
			edit: func(f map[string]any) { f["schema"] = 2 }},
		{name: "bad manifest", version: "0.2.1", code: CodeBadManifest,
			edit: func(f map[string]any) { f["product"] = "something-else" }},
		{name: "wrong channel", version: "0.2.1-rc.1", code: CodeWrongChannel, setup: func(h *harness) {
			h.gh.with(func(fg *fakeGitHub) { fg.latest = "0.2.1-rc.1" })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				var edits []func(map[string]any)
				if tt.edit != nil {
					edits = append(edits, tt.edit)
				}
				h.gh.publish(h.active, tt.version, installerBytes(100, 2), edits...)
				if tt.setup != nil {
					tt.setup(h)
				}
				st := h.checkNow(StateError, tt.code)
				if h.u.Ready() != nil {
					t.Error("Ready() non-nil in an error state")
				}
				if n := h.gh.count("_setup.exe"); n != 0 {
					t.Errorf("%d installer requests", n)
				}
				newer := tt.code == CodeManifestExpired || tt.code == CodeNoArtifact || tt.code == CodeNotInstalled
				if newer && (st.Available != tt.version || st.NotesURL != ReleasesURL+"/tag/v"+tt.version) {
					t.Errorf("status %+v, want Available %s with its NotesURL", st, tt.version)
				}
				level := slog.LevelWarn
				if tt.code == CodeNotInstalled { // expected for a copy run from a folder
					level = slog.LevelInfo
				}
				if h.logs.count(level, "code="+tt.code) != 1 || h.logs.count(slog.LevelWarn) != btoi(level == slog.LevelWarn) {
					t.Errorf("want one %s line with code=%s; logs %v", level, tt.code, h.logs.lines())
				}
			})
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Signatures: a manifest signed by an untrusted key, or a signature of other bytes, is bad_signature.
func TestCheckBadSignature(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(newTestSigner(t, "isshoni-update-9"), "0.2.1", installerBytes(100, 2))
		h.checkNow(StateError, CodeBadSignature)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.gh.with(func(fg *fakeGitHub) { fg.assets["v0.2.1/"+SignatureName] = h.active.sign(rel.installer) })
		time.Sleep(checkNowInterval)
		h.checkNow(StateError, CodeBadSignature)
	})
}

// A backup-signed manifest's revocation is persisted even when it offers nothing newer, and the revoked key then
// signs nothing (06 §8.1).
func TestCheckPersistsRevocations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		// An active-key manifest's revocations are ignored, with one INFO line, and nothing is persisted.
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1), func(f map[string]any) {
			f["revokedKeys"] = []string{h.backup.Fingerprint()}
		})
		h.checkNow(StateUpToDate, "")
		if got := h.diskState().RevokedKeys; len(got) != 0 {
			t.Fatalf("revokedKeys %q after an active-key manifest", got)
		}
		if h.logs.count(slog.LevelInfo, "ignored", "count=1") != 1 {
			t.Errorf("want one INFO line with the count; logs %v", h.logs.lines())
		}
		// A backup-key manifest's are honoured, but never for the backup itself.
		h.gh.publish(h.backup, testCurrent, installerBytes(100, 1), func(f map[string]any) {
			f["revokedKeys"] = []string{h.active.Fingerprint(), h.backup.Fingerprint()}
		})
		time.Sleep(checkNowInterval)
		h.checkNow(StateUpToDate, "")
		if got := h.diskState().RevokedKeys; !slices.Equal(got, []string{h.active.Fingerprint()}) {
			t.Fatalf("revokedKeys %q, want the active key only", got)
		}
		if h.logs.count(slog.LevelWarn, "revoked") != 1 {
			t.Errorf("want one WARN about the revocation; logs %v", h.logs.lines())
		}
		// A new start reads the revocation back.
		h.u = h.newUpdater()
		h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateError, CodeBadSignature)
		// The backup key still verifies, and its next revocation adds to the list.
		h.gh.publish(h.backup, "0.2.1", installerBytes(100, 2), func(f map[string]any) {
			f["revokedKeys"] = []string{newTestSigner(t, "isshoni-update-7").Fingerprint()}
		})
		h.u = h.newUpdater()
		h.checkNow(StateReady, "")
		if got := h.diskState().RevokedKeys; len(got) != 2 {
			t.Errorf("revokedKeys %q, want 2", got)
		}
	})
}

// A newer version found while one is ready replaces it; a check that finds nothing newer drops it.
func TestCheckReplacesPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		old := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		h.wantFiles(stateName, old.name)
		rel := h.gh.publish(h.active, "0.2.2", installerBytes(200, 3))
		time.Sleep(checkNowInterval)
		if st := h.checkNow(StateReady, ""); st.Available != "0.2.2" {
			t.Fatalf("status %+v", st)
		}
		h.wantFiles(stateName, rel.name)
		if p := h.u.Ready(); p == nil || p.Version != "0.2.2" {
			t.Fatalf("Ready() = %+v", p)
		}
		// GitHub's latest moves back (the release was withdrawn): the pending update goes.
		h.gh.with(func(fg *fakeGitHub) { fg.latest = testCurrent })
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		time.Sleep(checkNowInterval)
		h.checkNow(StateUpToDate, "")
		h.wantFiles(stateName)
		if h.diskState().Pending != nil || h.u.Ready() != nil {
			t.Error("the pending update survived a check that found nothing newer")
		}
	})
}

// A ready update stays offered when a later check fails before it finds anything newer.
func TestCheckFailureKeepsReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		h.gh.with(func(fg *fakeGitHub) { fg.latest = "" })
		time.Sleep(checkNowInterval)
		st, err := h.u.CheckNow(t.Context())
		if st.State != StateReady || CodeOf(err) != CodeNetwork || h.u.Ready() == nil {
			t.Errorf("CheckNow = %+v, %v; want ready with a network error", st, err)
		}
	})
}

// The loop guard (06 §8.7): after 2 failed attempts for a version, error/install_failed with Available until a newer
// version appears, which downloads.
func TestCheckInstallFailedGuard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		old := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.stage(old, 2)
		h.u = h.newUpdater()
		if p, err := h.u.PendingAtStartup(false); p != nil || err != nil {
			t.Fatalf("PendingAtStartup = %+v, %v; want nil after 2 attempts", p, err)
		}
		if st := h.u.Status(); st.State != StateError || st.ErrorCode != CodeInstallFailed || st.Available != "0.2.1" {
			t.Fatalf("status at start %+v", st)
		}
		st := h.checkNow(StateError, CodeInstallFailed)
		if st.Available != "0.2.1" || h.gh.count(old.name) != 0 {
			t.Errorf("status %+v; %d installer requests", st, h.gh.count(old.name))
		}
		rel := h.gh.publish(h.active, "0.2.2", installerBytes(100, 3))
		time.Sleep(checkNowInterval)
		if st := h.checkNow(StateReady, ""); st.Available != "0.2.2" {
			t.Errorf("status %+v", st)
		}
		h.wantFiles(stateName, rel.name)
		if p := h.diskState().Pending; p == nil || p.Version != "0.2.2" || p.Attempts != 0 {
			t.Errorf("pending %+v", p)
		}
	})
}

// PendingAtStartup (06 §8.7 (b)): the pending update when it is newer, not expired, verifies and has had fewer than
// 2 attempts; nil at an --autostart start, where it stays ready.
func TestPendingAtStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.stage(rel, 1)
		for _, junk := range []string{"isshoni-desktop_0.2.0_windows_amd64_setup.exe", rel.name + ".123.partial",
			stateName + ".456.tmp"} {
			if err := os.WriteFile(filepath.Join(h.dir, junk), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(h.dir, "keep-dir"), 0o700); err != nil {
			t.Fatal(err)
		}
		h.u = h.newUpdater()
		if p, err := h.u.PendingAtStartup(true); p != nil || err != nil {
			t.Fatalf("PendingAtStartup(true) = %+v, %v; want nil", p, err)
		}
		h.wantFiles(stateName, rel.name, "keep-dir")
		if st := h.u.Status(); st.State != StateReady || st.Available != "0.2.1" {
			t.Errorf("status after an autostart start %+v, want ready", st)
		}
		want := Pending{Version: "0.2.1", Path: filepath.Join(h.dir, rel.name)}
		if p := h.u.Ready(); p == nil || *p != want {
			t.Errorf("Ready() = %+v, want %+v", p, want)
		}
		p, err := h.u.PendingAtStartup(false)
		if err != nil || p == nil || *p != want {
			t.Fatalf("PendingAtStartup(false) = %+v, %v; want %+v", p, err, want)
		}
		if reqs := h.gh.requests(); len(reqs) != 0 {
			t.Errorf("requests at startup: %v", reqs)
		}
	})
}

func TestPendingAtStartupRules(t *testing.T) {
	for _, tt := range []struct {
		name     string
		current  string
		attempts int
		edit     func(h *harness, rel release)
		want     bool   // returned
		keep     bool   // the record and its file stay
		code     string // PendingAtStartup's error
	}{
		{name: "newer", current: "0.2.0", want: true, keep: true},
		{name: "rc below its final", current: "0.2.1-rc.3", want: true, keep: true},
		{name: "installed now", current: "0.2.1"},
		{name: "running a newer version", current: "0.2.2"},
		{name: "one attempt", current: "0.2.0", attempts: 1, want: true, keep: true},
		{name: "two attempts", current: "0.2.0", attempts: 2, keep: true},
		{name: "expired", current: "0.2.0", edit: func(h *harness, rel release) {
			st := h.diskState()
			st.Pending.ExpiresAt = wireTime{testEpoch.Add(-time.Millisecond)}
			writeState(h, st)
		}},
		{name: "changed file", current: "0.2.0", code: CodeHashMismatch, edit: func(h *harness, rel release) {
			data := installerBytes(100, 2)
			data[50] ^= 1
			if err := os.WriteFile(filepath.Join(h.dir, rel.name), data, 0o600); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "truncated file", current: "0.2.0", code: CodeHashMismatch, edit: func(h *harness, rel release) {
			if err := os.WriteFile(filepath.Join(h.dir, rel.name), installerBytes(99, 2), 0o600); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "missing file", current: "0.2.0", code: CodeHashMismatch, edit: func(h *harness, rel release) {
			if err := os.Remove(filepath.Join(h.dir, rel.name)); err != nil {
				h.t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				h.stage(rel, tt.attempts)
				if tt.edit != nil {
					tt.edit(h, rel)
				}
				h.current = tt.current
				h.u = h.newUpdater()
				p, err := h.u.PendingAtStartup(false)
				if (p != nil) != tt.want || CodeOf(err) != tt.code {
					t.Fatalf("PendingAtStartup = %+v, %v; want returned %t, code %q", p, err, tt.want, tt.code)
				}
				if kept := h.diskState().Pending != nil; kept != tt.keep {
					t.Errorf("pending kept %t, want %t", kept, tt.keep)
				}
				if tt.keep {
					h.wantFiles(stateName, rel.name)
				} else {
					h.wantFiles(stateName)
				}
			})
		})
	}
}

func writeState(h *harness, st *stateFile) {
	h.t.Helper()
	data, err := marshalState(st)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, stateName), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// Apply (06 §8.7): re-verify, the installed check, the attempt counted before the launch, the exact arguments.
func TestApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		p := h.u.Ready()

		h.installed.Store(false)
		if err := h.u.Apply(t.Context(), p, true); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("Apply outside InstallDir = %v, want ErrNotInstalled", err)
		}
		if n := len(h.launched()); n != 0 || h.diskState().Pending.Attempts != 0 {
			t.Fatalf("%d launches, attempts %d after ErrNotInstalled", n, h.diskState().Pending.Attempts)
		}
		h.installed.Store(true)

		if err := h.u.Apply(t.Context(), p, true); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if err := h.u.Apply(t.Context(), p, false); err != nil {
			t.Fatalf("Apply without relaunch: %v", err)
		}
		want := []launch{
			{path: filepath.Join(h.dir, rel.name), args: []string{"/S", "/UPDATE", "/RELAUNCH"}, attempts: 1},
			{path: filepath.Join(h.dir, rel.name), args: []string{"/S", "/UPDATE"}, attempts: 2},
		}
		if got := h.launched(); !slices.EqualFunc(got, want, func(a, b launch) bool {
			return a.path == b.path && slices.Equal(a.args, b.args) && a.attempts == b.attempts
		}) {
			t.Errorf("launches %+v, want %+v", got, want)
		}
		// Two attempts: no third, and the next start doesn't apply it.
		if err := h.u.Apply(t.Context(), p, true); CodeOf(err) != CodeInstallFailed {
			t.Errorf("a third Apply = %v, want install_failed", err)
		}
		h.u = h.newUpdater()
		if p, _ := h.u.PendingAtStartup(false); p != nil {
			t.Errorf("PendingAtStartup after 2 attempts = %+v", p)
		}
	})
}

func TestApplyReverifies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		p := h.u.Ready()
		data := installerBytes(100, 2)
		data[0] ^= 0xff
		if err := os.WriteFile(p.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.u.Apply(t.Context(), p, true); CodeOf(err) != CodeHashMismatch {
			t.Fatalf("Apply of a changed file = %v, want hash_mismatch", err)
		}
		if len(h.launched()) != 0 {
			t.Error("the changed file was started")
		}
		h.wantFiles(stateName)
		if st := h.u.Status(); st.State != StateError || st.ErrorCode != CodeHashMismatch || h.u.Ready() != nil {
			t.Errorf("status %+v", st)
		}
		if err := h.u.Apply(t.Context(), &Pending{Version: "0.2.1", Path: filepath.Join(h.dir, rel.name)}, true); err == nil {
			t.Error("Apply without a pending update succeeded")
		}
		// The next check downloads it again.
		time.Sleep(checkNowInterval)
		h.checkNow(StateReady, "")
		if err := h.u.Apply(t.Context(), nil, true); err == nil {
			t.Error("Apply(nil) succeeded")
		}
		if err := h.u.Apply(t.Context(), &Pending{Version: "0.2.1", Path: filepath.Join(h.data, rel.name)}, true); err == nil {
			t.Error("Apply of a path outside the updates folder succeeded")
		}
	})
}

// A launch that fails (Smart App Control or an antivirus blocks the installer) leaves the app running: one failed
// attempt stays ready, and the second trips the loop guard at once (06 §8.5, §8.7), not at the next start or check.
func TestApplyLaunchFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		h.launchErr = &Error{Code: CodeInstallFailed, Err: errors.New("start: access denied")}
		p := h.u.Ready()
		if err := h.u.Apply(t.Context(), p, true); CodeOf(err) != CodeInstallFailed {
			t.Errorf("Apply = %v", err)
		}
		if a := h.diskState().Pending.Attempts; a != 1 {
			t.Errorf("attempts %d, want 1: counted before the launch", a)
		}
		if st := h.u.Status(); st.State != StateReady || h.u.Ready() == nil {
			t.Fatalf("status %+v, Ready() %+v after one failed launch; want ready", st, h.u.Ready())
		}
		if n := h.logs.count(slog.LevelWarn); n != 0 {
			t.Errorf("%d WARN lines after one failed launch: %v", n, h.logs.lines())
		}

		if err := h.u.Apply(t.Context(), p, true); CodeOf(err) != CodeInstallFailed {
			t.Errorf("a second Apply = %v", err)
		}
		st := h.u.Status()
		if st.State != StateError || st.ErrorCode != CodeInstallFailed || st.Available != "0.2.1" ||
			st.NotesURL != ReleasesURL+"/tag/v0.2.1" || st.CheckedAt.IsZero() || h.u.Ready() != nil {
			t.Errorf("status %+v, Ready() %+v after two failed launches; want error/install_failed with 0.2.1", st,
				h.u.Ready())
		}
		if h.logs.count(slog.LevelWarn, "version=0.2.1", "attempts=2") != 1 {
			t.Errorf("want one WARN naming the version and the attempts; logs %v", h.logs.lines())
		}
		// A third Apply, with the Pending from before, starts nothing.
		if err := h.u.Apply(t.Context(), p, true); CodeOf(err) != CodeInstallFailed {
			t.Errorf("a third Apply = %v", err)
		}
		if n := len(h.launched()); n != 2 {
			t.Errorf("%d launches, want 2", n)
		}
		// CheckNow within 30 s of the last one gives the status as it is now; a check after that finds the same
		// version, which stays install_failed and is not downloaded again.
		if st, err := h.u.CheckNow(t.Context()); st.State != StateError || st.ErrorCode != CodeInstallFailed || err != nil {
			t.Errorf("CheckNow within 30 s = %+v, %v; want the current error/install_failed", st, err)
		}
		time.Sleep(checkNowInterval)
		if st := h.checkNow(StateError, CodeInstallFailed); st.Available != "0.2.1" || h.u.Ready() != nil {
			t.Errorf("status %+v, Ready() %+v", st, h.u.Ready())
		}
		if n := h.gh.count(rel.name); n != 2 {
			t.Errorf("%d installer requests (redirect and asset), want the first download's 2", n)
		}
	})
}

// A cancelled context while Apply waits for the installer's mutex is not a failed launch: the guard doesn't trip.
func TestApplyCancelledIsNoFailedLaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
		h.checkNow(StateReady, "")
		h.launchErr = &Error{Code: CodeInstallFailed, Err: errors.New("start: access denied")}
		if err := h.u.Apply(t.Context(), h.u.Ready(), true); CodeOf(err) != CodeInstallFailed {
			t.Errorf("Apply = %v", err)
		}
		h.launchErr = context.Canceled
		if err := h.u.Apply(t.Context(), h.u.Ready(), true); !errors.Is(err, context.Canceled) {
			t.Errorf("a second Apply = %v", err)
		}
		if st := h.u.Status(); st.State != StateReady || h.u.Ready() == nil || h.diskState().Pending.Attempts != 2 {
			t.Errorf("status %+v, Ready() %+v, attempts %d; want ready", st, h.u.Ready(), h.diskState().Pending.Attempts)
		}
	})
}

// The installer's mutex: Apply returns once it exists, and nil with one WARN after 10 s when it never does.
func TestWaitInstalling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &logRecorder{}
		start := time.Now()
		appears := start.Add(300 * time.Millisecond)
		probes := 0
		err := waitInstalling(t.Context(), func() (bool, error) {
			probes++
			return !time.Now().Before(appears), nil
		}, logs.logger())
		if err != nil || time.Since(start) != 300*time.Millisecond || probes != 7 {
			t.Errorf("waitInstalling = %v after %v and %d probes, want nil after 300 ms and 7 probes", err,
				time.Since(start), probes)
		}
		start = time.Now()
		err = waitInstalling(t.Context(), func() (bool, error) { return false, errors.New("probe failed") }, logs.logger())
		if err != nil || time.Since(start) != installingWait {
			t.Errorf("waitInstalling = %v after %v, want nil after 10 s", err, time.Since(start))
		}
		if n := logs.count(slog.LevelWarn, ""); n != 1 {
			t.Errorf("%d WARN lines, want 1: %v", n, logs.lines())
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := waitInstalling(ctx, func() (bool, error) { return false, nil }, logs.logger()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waitInstalling with an ending context = %v", err)
		}
	})
}

// Subscribe: the current status at once, then the latest; cancel closes the channel.
func TestSubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, testCurrent, installerBytes(100, 1))
		ch, cancel := h.u.Subscribe()
		if st := <-ch; st.State != StateIdle || st.Current != testCurrent {
			t.Errorf("first status %+v", st)
		}
		h.checkNow(StateUpToDate, "")
		if st := <-ch; st.State != StateUpToDate {
			t.Errorf("latest status %+v, want up_to_date (checking was replaced)", st)
		}
		cancel()
		cancel()
		if _, ok := <-ch; ok {
			t.Error("the channel is open after cancel")
		}
	})
}

// A disabled updater (06 §8.5): not a release build, or no keys; it never makes a request.
func TestDisabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		current string
		keys    bool
		code    string
	}{
		{"dev build", "0.0.0-dev+1a2b3c4d5e6f", true, CodeNotRelease},
		{"ci build", "0.0.1-ci.7", true, CodeNotRelease},
		{"dry run", "0.0.0-dryrun.3", true, CodeNotRelease},
		{"no keys", "0.2.0", false, CodeNoKeys},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				h.stage(rel, 0)
				u, err := newUpdater(Options{CurrentVersion: tt.current, DataDir: h.data,
					HTTP: &http.Client{Transport: h.gh.tr}},
					env{signers: func() ([]TrustedKey, error) {
						if tt.keys {
							return keys(h.active, h.backup), nil
						}
						return nil, nil
					}, ep: defaultEndpoints(), sys: system{policy: func() bool { return false }}})
				if err != nil {
					t.Fatal(err)
				}
				want := Status{State: StateDisabled, Current: tt.current, ErrorCode: tt.code}
				if st := u.Status(); st != want {
					t.Errorf("Status = %+v, want %+v", st, want)
				}
				if st, err := u.CheckNow(t.Context()); st != want || err != nil {
					t.Errorf("CheckNow = %+v, %v", st, err)
				}
				if p, err := u.PendingAtStartup(false); p != nil || err != nil {
					t.Errorf("PendingAtStartup = %+v, %v", p, err)
				}
				if err := u.Apply(t.Context(), &Pending{Version: "0.2.1", Path: filepath.Join(h.dir, rel.name)}, true); CodeOf(err) != tt.code {
					t.Errorf("Apply = %v", err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 72*time.Hour)
				defer cancel()
				if err := u.Run(ctx); err != nil {
					t.Errorf("Run = %v", err)
				}
				if reqs := h.gh.requests(); len(reqs) != 0 {
					t.Errorf("requests: %v", reqs)
				}
				h.wantFiles(stateName, rel.name) // untouched
			})
		})
	}
}

func TestNewOptions(t *testing.T) {
	if _, err := New(Options{CurrentVersion: "0.2.0"}); err == nil {
		t.Error("New without DataDir succeeded")
	}
	if _, err := New(Options{CurrentVersion: "0.2.0", DataDir: "relative"}); err == nil {
		t.Error("New with a relative DataDir succeeded")
	}
	u, err := New(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// This test binary is a dev build without embedded keys.
	if st := u.Status(); st.State != StateDisabled || st.Current == "" || st.ErrorCode != CodeNotRelease {
		t.Errorf("Status = %+v, want disabled/not_release", st)
	}
	if _, err := newUpdater(Options{DataDir: t.TempDir()}, env{signers: func() ([]TrustedKey, error) {
		return nil, errors.New("broken block")
	}, sys: system{policy: func() bool { return false }}}); err == nil {
		t.Error("newUpdater with broken signers succeeded")
	}
}

// A release build contains no test hooks (06 §5.2 step 3, §8.11): testdata/hookcheck, which links this package the
// way the app does, built for Windows with the release's tags holds neither variable name, while an updatetest build
// holds both. It builds twice with the go tool, so it takes a few seconds: not under -short.
func TestReleaseBuildHasNoTestHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("builds testdata/hookcheck twice")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool: %v", err)
	}
	build := func(tags string) []byte {
		t.Helper()
		bin := filepath.Join(t.TempDir(), "hookcheck.exe")
		//nolint:gosec // G204: the go tool from PATH with fixed arguments
		cmd := exec.CommandContext(t.Context(), goTool, "build", "-trimpath", "-tags", tags, "-ldflags", "-s -w",
			"-o", bin, "./testdata/hookcheck")
		cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build -tags %q: %v\n%s", tags, err, out)
		}
		return mustRead(t, bin)
	}
	names := []string{"ISSHONI_UPDATE_TEST_BASE", "ISSHONI_UPDATE_TEST_SIGNERS"}
	release := build("production")
	for _, name := range append(names, "ISSHONI_UPDATE_TEST", fakeInstallerEnv) {
		if bytes.Contains(release, []byte(name)) {
			t.Errorf("the release build contains %q", name)
		}
	}
	test := build("production updatetest")
	for _, name := range names {
		if !bytes.Contains(test, []byte(name)) {
			t.Errorf("the updatetest build lacks %q, so the check above proves nothing", name)
		}
	}
}

func TestRunTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		stop := h.run()
		synctest.Wait()
		if err := h.u.Run(t.Context()); err == nil {
			t.Error("a second Run succeeded")
		}
		stop()
	})
}
