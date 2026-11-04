package main

// This file has no build constraint: the one-shot modes' logic (03 §13.4 step 1) and step 2a's link-to-Pending rule
// work on interfaces and build and test on every OS, so their unit tests run on ubuntu too, where the rest of the
// package is the stub of main_other.go. oneshot_os.go wires them to the real engine, bundle, credential store and
// data root.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
	"github.com/MoonWX/isshoni/internal/version"
)

// The exit codes of the app's modes (03 §13.4; README §4 "Errors").
const (
	exitOK     = 0
	exitFailed = 1 // a one-shot mode failed, or the app couldn't start
	// exitUnavailable (2: a mode this build doesn't have yet) is in start.go.
)

// The limits of the one-shot modes (03 §13.4 step 1).
const (
	// selfCheckProbeTimeout bounds --self-check's Probe(ctx, false).
	selfCheckProbeTimeout = 20 * time.Second
	// purgeRetryBudget is how long --purge-data retries a data root that still has locked files (WebView2's
	// children may still be exiting).
	purgeRetryBudget = 10 * time.Second
	// purgeRetryEvery is the spacing of those retries.
	purgeRetryEvery = 250 * time.Millisecond
)

// ffmpegLicense is the only FFmpeg license --self-check accepts: avcodec_license() of 06 §4's LGPL build.
const ffmpegLicense = "LGPL version 2.1 or later"

// selfCheckEngine is what --self-check reads of the engine: native.Engine's Info, Probe, BuildInfo and Close.
type selfCheckEngine interface {
	Info() media.Info
	Probe(ctx context.Context, refresh bool) (media.Probe, error)
	BuildInfo() native.BuildInfo
	Close() error
}

// selfCheckReport is --self-check's JSON (06 §2.4, README §3 item 4): 06's engine fields plus 03's spa.ok.
type selfCheckReport struct {
	OK      bool              `json:"ok"`
	Version string            `json:"version"`
	Engine  selfCheckEngineOK `json:"engine"`
	SPA     selfCheckSPA      `json:"spa"`
}

type selfCheckEngineOK struct {
	Loaded        bool   `json:"loaded"`
	ABI           int    `json:"abi"` // the DLL's ABI major, Info().ABI (01 §5.1)
	FFmpegLoaded  bool   `json:"ffmpegLoaded"`
	FFmpegLicense string `json:"ffmpegLicense"`
	TestHooks     bool   `json:"testHooks"`
}

type selfCheckSPA struct {
	OK bool `json:"ok"`
}

// ok is the report's verdict: the engine loaded with ABI major media.ABIVersion, FFmpeg loaded under the LGPL
// license, no test hooks, and the bundled SPA served (03 §13.4, 06 §2.4). No encoder is required, so it passes on
// a GPU-less VM.
func (r selfCheckReport) ok() bool {
	e := r.Engine
	return e.Loaded && e.ABI == media.ABIVersion && e.FFmpegLoaded && e.FFmpegLicense == ffmpegLicense &&
		!e.TestHooks && r.SPA.OK
}

// runSelfCheck builds --self-check's report (03 §13.4 step 1): openEngine (native.Open from the exe's folder), its
// Info().ABI, Probe(ctx, false) under selfCheckProbeTimeout (which loads FFmpeg, 04 §5.3), BuildInfo(), then
// checkSPA (the asset handler over the bundled SPA). A failed step leaves its fields false and is logged; the
// report is always complete.
func runSelfCheck(ctx context.Context, log *slog.Logger, openEngine func() (selfCheckEngine, error),
	checkSPA func() error) selfCheckReport {
	r := selfCheckReport{Version: version.Version()}
	if e, err := openEngine(); err != nil {
		log.Error("desktop: self-check: the engine didn't load", "err", err)
	} else {
		r.Engine.Loaded = true
		r.Engine.ABI = e.Info().ABI
		pctx, cancel := context.WithTimeout(ctx, selfCheckProbeTimeout)
		if _, err := e.Probe(pctx, false); err != nil {
			log.Warn("desktop: self-check: the probe failed", "err", err)
		}
		cancel()
		b := e.BuildInfo()
		r.Engine.FFmpegLoaded = b.FFmpeg.Loaded
		r.Engine.FFmpegLicense = b.FFmpeg.License
		r.Engine.TestHooks = b.TestHooks
		if err := e.Close(); err != nil {
			log.Warn("desktop: self-check: the engine didn't close", "err", err)
		}
	}
	if err := checkSPA(); err != nil {
		log.Error("desktop: self-check: the bundled web app is unusable", "err", err)
	} else {
		r.SPA.OK = true
	}
	r.OK = r.ok()
	return r
}

// writeSelfCheck writes the report to path as one JSON document with a final newline.
func writeSelfCheck(path string, r selfCheckReport) error {
	if path == "" {
		return errors.New("--self-check needs a file: --self-check=<path>")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	//nolint:gosec // G306, G703: the caller's own output file (06's smoke and stage read it as the same user)
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// selfCheckMode is --self-check=<path>: the report written to path; exitOK only when it is ok.
func selfCheckMode(ctx context.Context, log *slog.Logger, path string, openEngine func() (selfCheckEngine, error),
	checkSPA func() error) int {
	if path == "" { // nothing to write the verdict to: no point loading the engine
		log.Error("desktop: --self-check needs a file: --self-check=<path>")
		return exitFailed
	}
	r := runSelfCheck(ctx, log, openEngine, checkSPA)
	if err := writeSelfCheck(path, r); err != nil {
		log.Error("desktop: self-check: the report wasn't written", "err", err)
		return exitFailed
	}
	log.Info("desktop: self-check done", "ok", r.OK, "engine_loaded", r.Engine.Loaded, "abi", r.Engine.ABI,
		"ffmpeg_loaded", r.Engine.FFmpegLoaded, "test_hooks", r.Engine.TestHooks, "spa_ok", r.SPA.OK)
	if !r.OK {
		return exitFailed
	}
	return exitOK
}

// bridgeTag is what the asset handler puts into index.html (03 §5.1): a bundle it serves without it is broken.
const bridgeTag = `<script src="/desktop/bridge.js">`

// serveIndex checks an asset handler the way the window first uses it: GET / at origin answers 200 with the bridge
// tag in the page.
func serveIndex(h http.Handler, origin string) error {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, origin+"/", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET / answered %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), bridgeTag) {
		return errors.New("GET / has no bridge tag")
	}
	return nil
}

// credentialForgetter is what --forget-credentials and --purge-data use of 02's servers: deviceauth.Servers.
type credentialForgetter interface {
	ForgetAll(ctx context.Context) error
	Close() error
}

// forgetCredentials is --forget-credentials' work, and --purge-data's first part (03 §13.4 step 1, 06 §2.4):
// 02's Servers.ForgetAll (every linked server revoked in parallel under one 5 s budget, then every credential under
// the store's prefix deleted, whatever the revokes answered). When servers.json can't be opened, every credential
// under the prefix is still deleted, without revokes. It returns the local errors, joined; the modes exit 0 anyway
// (offline included): the uninstaller goes on regardless (06 §6.8).
func forgetCredentials(ctx context.Context, log *slog.Logger, openStore func() (credstore.Store, error),
	openServers func(credstore.Store) (credentialForgetter, error)) error {
	store, err := openStore()
	if err != nil {
		return fmt.Errorf("the credential store: %w", err)
	}
	srv, err := openServers(store)
	if err != nil {
		log.Warn("desktop: servers.json is unusable; deleting the credentials without revoking them", "err", err)
		return errors.Join(fmt.Errorf("servers.json: %w", err), deleteAllCredentials(store))
	}
	err = srv.ForgetAll(ctx)
	if cerr := srv.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	return err
}

// forgetCredentialsMode is --forget-credentials (03 §13.4 step 1): forgetCredentials, leaving the disk as it found
// it. ForgetAll always saves servers.json, which creates the data root, so on a PC where the app never ran (or whose
// data root is gone) a keep-data uninstall would leave a data root the app never made: when the root didn't exist
// before, removeDataRoot deletes it again afterwards. Errors are logged; the mode exits 0 regardless (06 §6.8).
func forgetCredentialsMode(ctx context.Context, log *slog.Logger, openStore func() (credstore.Store, error),
	openServers func(credstore.Store) (credentialForgetter, error), dataRoot func() (string, error),
	remove func(string) error) {
	root, rootErr := dataRoot()
	absent := false
	if rootErr == nil {
		_, err := os.Stat(root)
		absent = errors.Is(err, fs.ErrNotExist)
	}
	if err := forgetCredentials(ctx, log, openStore, openServers); err != nil {
		log.Warn("desktop: not every credential was forgotten", "err", err)
	}
	if absent {
		if err := removeDataRoot(ctx, root, purgeRetryBudget, purgeRetryEvery, remove); err != nil {
			log.Warn("desktop: the data folder this mode created wasn't deleted", "err", err)
		}
	}
}

// deleteAllCredentials deletes every credential under the store's prefix (02's DeleteAll without the revokes).
func deleteAllCredentials(store credstore.Store) error {
	origins, err := store.List()
	if err != nil {
		return fmt.Errorf("list the credentials: %w", err)
	}
	var errs []error
	for _, o := range origins {
		if err := store.Delete(o); err != nil {
			errs = append(errs, fmt.Errorf("delete a credential: %w", err))
		}
	}
	return errors.Join(errs...)
}

// dataRootNames are the last elements a data root may have: %LOCALAPPDATA%\isshoni (03 §14.1) and the macOS dev
// build's isshoni-dev. --purge-data refuses to delete any other folder.
var dataRootNames = map[string]bool{"isshoni": true, "isshoni-dev": true}

// removeDataRoot deletes root and everything in it, retrying for up to budget, every every, while something is
// still locked (03 §13.4: WebView2's children may still be exiting). remove is os.RemoveAll. A root that doesn't
// exist is no error; a root that isn't an absolute path named like a data root is refused.
func removeDataRoot(ctx context.Context, root string, budget, every time.Duration, remove func(string) error) error {
	if !filepath.IsAbs(root) || !dataRootNames[filepath.Base(root)] || filepath.Dir(root) == root {
		return fmt.Errorf("refusing to delete %q: not a data root", filepath.Base(root))
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for attempt := 1; ; attempt++ {
		err := remove(root)
		if err == nil {
			return nil
		}
		t := time.NewTimer(every)
		select {
		case <-t.C:
		case <-deadline.C:
			t.Stop()
			return fmt.Errorf("the data root is still there after %d attempts: %w", attempt, err)
		case <-ctx.Done():
			t.Stop()
			return errors.Join(ctx.Err(), err)
		}
	}
}

// purgeData is --purge-data's work: forgetCredentials exactly as --forget-credentials runs it, then the data root
// (06 §2.2) deleted by removeDataRoot. The credentials' errors are logged; the data root's is returned.
func purgeData(ctx context.Context, log *slog.Logger, openStore func() (credstore.Store, error),
	openServers func(credstore.Store) (credentialForgetter, error), dataRoot func() (string, error),
	remove func(string) error) error {
	if err := forgetCredentials(ctx, log, openStore, openServers); err != nil {
		log.Warn("desktop: not every credential was forgotten", "err", err)
	}
	root, err := dataRoot()
	if err != nil {
		return fmt.Errorf("no data folder: %w", err)
	}
	return removeDataRoot(ctx, root, purgeRetryBudget, purgeRetryEvery, remove)
}

// pendingFromLink is what a start from --open-url writes to PendingServer and PendingRoom when it finds an installer
// running (03 §13.4 step 2a): the link's origin and room id ("" for a link to the server's own last room, which
// deletes an older PendingRoom: the newest link wins). A link that doesn't parse gives an error and nothing to write.
func pendingFromLink(link string) (app.Pending, error) {
	l, err := deeplink.Parse(link)
	if err != nil {
		return app.Pending{}, err
	}
	return app.Pending{Server: l.Origin, Room: l.RoomID}, nil
}
