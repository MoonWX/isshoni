//go:build !isshoni_hw

package main

// These tests have no OS constraint, like oneshot.go: they run on the Mac, CI ubuntu (where the rest of the package
// is main_other.go's stub), macos and windows.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/version"
)

// fakeSelfCheckEngine is an engine for --self-check: what it reports, and what was asked of it.
type fakeSelfCheckEngine struct {
	abi      int
	build    native.BuildInfo
	probeErr error

	probed    bool
	probeCtx  bool // the probe's context had a deadline
	refreshed bool
	closed    bool
}

func (e *fakeSelfCheckEngine) Info() media.Info { return media.Info{Name: "windows", ABI: e.abi} }

func (e *fakeSelfCheckEngine) Probe(ctx context.Context, refresh bool) (media.Probe, error) {
	e.probed, e.refreshed = true, refresh
	_, e.probeCtx = ctx.Deadline()
	return media.Probe{}, e.probeErr
}

func (e *fakeSelfCheckEngine) BuildInfo() native.BuildInfo { return e.build }
func (e *fakeSelfCheckEngine) Close() error                { e.closed = true; return nil }

// goodEngine reports what a release's engine on a GPU-less VM reports: ABI major 1, FFmpeg loaded under the LGPL,
// no test hooks.
func goodEngine() *fakeSelfCheckEngine {
	return &fakeSelfCheckEngine{abi: media.ABIVersion, build: native.BuildInfo{
		Engine: "0.2.0", ABI: 1 << 16,
		FFmpeg: native.FFmpegBuildInfo{Loaded: true, License: "LGPL version 2.1 or later"},
	}}
}

func openerOf(e *fakeSelfCheckEngine) func() (selfCheckEngine, error) {
	return func() (selfCheckEngine, error) { return e, nil }
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// readReport reads --self-check's file and checks it has exactly the fields of README §3 item 4 (06 §2.4's engine
// fields plus 03's spa.ok).
func readReport(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %s: %v", data, err)
	}
	check := func(what string, m map[string]any, want ...string) {
		t.Helper()
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
			t.Errorf("%s fields %v, want %v", what, got, want)
		}
	}
	check("report", v, "engine", "ok", "spa", "version")
	engine, _ := v["engine"].(map[string]any)
	check("engine", engine, "abi", "ffmpegLicense", "ffmpegLoaded", "loaded", "testHooks")
	spa, _ := v["spa"].(map[string]any)
	check("spa", spa, "ok")
	if v["version"] != version.Version() {
		t.Errorf("version %v", v["version"])
	}
	return v
}

// TestSelfCheckOK (03 §13.4 step 1, 06 §2.4): a good engine and bundle write ok true with every field, and exit 0;
// the probe is the quick one (refresh false) under a deadline, and the engine is closed afterwards.
func TestSelfCheckOK(t *testing.T) {
	e := goodEngine()
	path := filepath.Join(t.TempDir(), "self-check.json")
	if code := selfCheckMode(t.Context(), discardLog(), path, openerOf(e), func() error { return nil }); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	v := readReport(t, path)
	engine := v["engine"].(map[string]any)
	if v["ok"] != true || engine["loaded"] != true || engine["abi"] != json.Number("1") ||
		engine["ffmpegLoaded"] != true || engine["ffmpegLicense"] != "LGPL version 2.1 or later" ||
		engine["testHooks"] != false || v["spa"].(map[string]any)["ok"] != true {
		t.Errorf("report %v", v)
	}
	if !e.probed || e.refreshed || !e.probeCtx || !e.closed {
		t.Errorf("probed %v refresh %v deadline %v closed %v", e.probed, e.refreshed, e.probeCtx, e.closed)
	}
}

// TestSelfCheckNotOK: each requirement of ok, broken alone, makes ok false and the exit 1, and the report is still
// written in full.
func TestSelfCheckNotOK(t *testing.T) {
	for name, tc := range map[string]struct {
		engine  func(*fakeSelfCheckEngine)
		noLoad  bool
		spaErr  error
		checkFn func(v map[string]any) bool
	}{
		"engine not loaded": {noLoad: true, checkFn: func(v map[string]any) bool {
			e := v["engine"].(map[string]any)
			return e["loaded"] == false && e["ffmpegLoaded"] == false && e["abi"] == json.Number("0")
		}},
		"abi 2":             {engine: func(e *fakeSelfCheckEngine) { e.abi = 2 }},
		"ffmpeg not loaded": {engine: func(e *fakeSelfCheckEngine) { e.build.FFmpeg.Loaded = false }},
		"gpl ffmpeg":        {engine: func(e *fakeSelfCheckEngine) { e.build.FFmpeg.License = "GPL version 2 or later" }},
		"no license":        {engine: func(e *fakeSelfCheckEngine) { e.build.FFmpeg.License = "" }},
		"test hooks":        {engine: func(e *fakeSelfCheckEngine) { e.build.TestHooks = true }},
		"broken bundle": {spaErr: assets.ErrNoInjectionPoint, checkFn: func(v map[string]any) bool {
			return v["spa"].(map[string]any)["ok"] == false
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e := goodEngine()
			if tc.engine != nil {
				tc.engine(e)
			}
			open := openerOf(e)
			if tc.noLoad {
				open = func() (selfCheckEngine, error) { return nil, media.ErrUnsupportedOS }
			}
			path := filepath.Join(t.TempDir(), "self-check.json")
			if code := selfCheckMode(t.Context(), discardLog(), path, open, func() error { return tc.spaErr }); code != exitFailed {
				t.Errorf("exit %d", code)
			}
			v := readReport(t, path)
			if v["ok"] != false {
				t.Errorf("ok %v", v["ok"])
			}
			if tc.checkFn != nil && !tc.checkFn(v) {
				t.Errorf("report %v", v)
			}
		})
	}
}

// TestSelfCheckProbeError: a failed probe is logged; the verdict follows what BuildInfo says afterwards (no encoder
// is required: a GPU-less VM's probe finds none).
func TestSelfCheckProbeError(t *testing.T) {
	e := goodEngine()
	e.probeErr = errors.New("no adapter")
	var logs bytes.Buffer
	r := runSelfCheck(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), openerOf(e), func() error { return nil })
	if !r.OK || !strings.Contains(logs.String(), "probe failed") {
		t.Errorf("ok %v, logs %s", r.OK, logs.String())
	}
}

// TestSelfCheckPath: without a path nothing is loaded and the exit is 1; an unwritable path is exit 1 too.
func TestSelfCheckPath(t *testing.T) {
	e := goodEngine()
	if code := selfCheckMode(t.Context(), discardLog(), "", openerOf(e), func() error { return nil }); code != exitFailed {
		t.Errorf("no path: exit %d", code)
	}
	if e.probed {
		t.Error("no path: the engine was used")
	}
	bad := filepath.Join(t.TempDir(), "missing", "self-check.json")
	if code := selfCheckMode(t.Context(), discardLog(), bad, openerOf(goodEngine()), func() error { return nil }); code != exitFailed {
		t.Errorf("unwritable path: exit %d", code)
	}
}

// TestServeIndex: the SPA check passes a bundle the handler serves with its bridge tag and fails one without it.
func TestServeIndex(t *testing.T) {
	const origin = "http://wails.localhost"
	dist := fstest.MapFS{"index.html": {Data: []byte(
		`<html><head><meta charset="utf-8"></head><body><script src="/boot-check.js"></script></body></html>`)}}
	h, err := assets.New(assets.Options{Dist: dist, Origin: origin,
		Bridge: func() assets.BridgeConfig { return assets.BridgeConfig{V: assets.BridgeConfigVersion} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := serveIndex(h, origin); err != nil {
		t.Errorf("a good bundle: %v", err)
	}
	if err := serveIndex(http.NotFoundHandler(), origin); err == nil {
		t.Error("a 404 passed")
	}
	plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html></html>")) })
	if err := serveIndex(plain, origin); err == nil {
		t.Error("a page without the bridge tag passed")
	}
}

// offlineServers writes a servers.json naming an unreachable linked server into dir and returns its origin. Port 1
// on the loopback refuses at once: the revoke fails like an offline PC's.
func offlineServers(t *testing.T, dir string) string {
	t.Helper()
	const origin = "https://127.0.0.1:1"
	doc := `{"version":1,"active":"` + origin + `","servers":[
	  {"origin":"` + origin + `","name":"Offline","addedAt":"2026-10-01T00:00:00Z",
	   "user":{"id":"u1","username":"friend","role":"user"},"device":{"id":"d1","name":"PC"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return origin
}

func testCredential() credstore.Credential {
	return credstore.Credential{RefreshToken: "isr_" + strings.Repeat("a", 43), DeviceID: "d1", UserName: "friend",
		SavedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

// realServers opens 02's servers over store from dir, as openAppServers does.
func realServers(dir string) func(credstore.Store) (credentialForgetter, error) {
	return func(store credstore.Store) (credentialForgetter, error) {
		srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: dir, Store: store,
			App: deviceauth.AppInfo{Version: "0.2.0", ClientKind: protocol.ClientKindDesktop,
				OS: protocol.ClientOSWindows, DeviceName: "PC"}})
		if err != nil {
			return nil, err
		}
		return srv, nil
	}
}

// TestForgetCredentialsOffline (03 §13.4 step 1, 06 §2.4): with 02's real servers offline, every credential under
// the prefix is gone (the listed server's and one that servers.json doesn't name) within ForgetAll's 5 s budget.
func TestForgetCredentialsOffline(t *testing.T) {
	dir := t.TempDir()
	origin := offlineServers(t, dir)
	store := credstore.NewMemory()
	for _, o := range []string{origin, "https://stray.example.com"} {
		if err := store.Put(o, testCredential()); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := forgetCredentials(t.Context(), discardLog(), func() (credstore.Store, error) { return store, nil },
		realServers(dir)); err != nil {
		t.Errorf("local errors: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	if left, err := store.List(); err != nil || len(left) != 0 {
		t.Errorf("credentials left: %v %v", left, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "servers.json"))
	if err != nil || strings.Contains(string(data), `"user"`) || strings.Contains(string(data), `"device"`) {
		t.Errorf("servers.json still names the user or device: %s %v", data, err)
	}
}

// TestForgetCredentialsWithoutServers: a servers.json that can't be opened still leaves no credential (no revokes);
// a store that can't be opened is reported.
func TestForgetCredentialsWithoutServers(t *testing.T) {
	store := credstore.NewMemory()
	if err := store.Put("https://a.example.com", testCredential()); err != nil {
		t.Fatal(err)
	}
	broken := func(credstore.Store) (credentialForgetter, error) { return nil, errors.New("unreadable") }
	if err := forgetCredentials(t.Context(), discardLog(), func() (credstore.Store, error) { return store, nil },
		broken); err == nil {
		t.Error("the servers.json error wasn't reported")
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Errorf("credentials left: %v", left)
	}
	noStore := func() (credstore.Store, error) { return nil, credstore.ErrUnavailable }
	if err := forgetCredentials(t.Context(), discardLog(), noStore, broken); !errors.Is(err, credstore.ErrUnavailable) {
		t.Errorf("err %v", err)
	}
}

// TestRemoveDataRoot: the data root goes with everything in it; a locked file is retried until it lets go, within
// the budget; a root that stays locked is an error after the budget; a missing root is fine; and a folder that
// isn't named like a data root is never touched.
func TestRemoveDataRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	if err := os.MkdirAll(filepath.Join(root, "WebView2", "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := 3
	calls := 0
	remove := func(p string) error {
		calls++
		if calls <= locked {
			return &fs.PathError{Op: "remove", Path: p, Err: errors.New("in use")}
		}
		return os.RemoveAll(p)
	}
	if err := removeDataRoot(t.Context(), root, 5*time.Second, time.Millisecond, remove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) || calls != locked+1 {
		t.Errorf("root still there (%v) after %d calls", err, calls)
	}
	if err := removeDataRoot(t.Context(), root, time.Second, time.Millisecond, os.RemoveAll); err != nil {
		t.Errorf("a missing root: %v", err)
	}

	start := time.Now()
	stuck := func(string) error { return errors.New("in use") }
	if err := removeDataRoot(t.Context(), root, 50*time.Millisecond, 5*time.Millisecond, stuck); err == nil {
		t.Error("a root that stays locked passed")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the budget didn't hold: %v", d)
	}

	other := t.TempDir()
	for _, p := range []string{other, "isshoni", filepath.Join(other, "isshoni-other"), ""} {
		touched := false
		if err := removeDataRoot(t.Context(), p, time.Second, time.Millisecond,
			func(string) error { touched = true; return nil }); err == nil || touched {
			t.Errorf("%q: err %v, touched %v", p, err, touched)
		}
	}
}

// TestPurgeData: --purge-data forgets every credential offline (as --forget-credentials does), then deletes the data
// root, servers.json and all.
func TestPurgeData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	origin := offlineServers(t, root)
	store := credstore.NewMemory()
	if err := store.Put(origin, testCredential()); err != nil {
		t.Fatal(err)
	}
	if err := purgeData(t.Context(), discardLog(), func() (credstore.Store, error) { return store, nil },
		realServers(root), func() (string, error) { return root, nil }, os.RemoveAll); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Errorf("credentials left: %v", left)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the data root is still there: %v", err)
	}
	// No data root: the credentials still go, and the error is the data root's.
	if err := store.Put(origin, testCredential()); err != nil {
		t.Fatal(err)
	}
	noRoot := func() (string, error) { return "", errors.New("no known folder") }
	if err := purgeData(t.Context(), discardLog(), func() (credstore.Store, error) { return store, nil },
		func(credstore.Store) (credentialForgetter, error) { return nil, errors.New("none") }, noRoot,
		os.RemoveAll); err == nil {
		t.Error("no data root passed")
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Errorf("credentials left: %v", left)
	}
}

// TestPendingFromLink: step 2a writes the link's origin and room id; a link to the server's own last room writes no
// room (which deletes an older PendingRoom); a link that doesn't parse writes nothing.
func TestPendingFromLink(t *testing.T) {
	for link, want := range map[string]app.Pending{
		"isshoni://watch.example.com/r/lounge":       {Server: "https://watch.example.com", Room: "lounge"},
		"ISSHONI://Watch.Example.com:8443/r/a_b-c":   {Server: "https://watch.example.com:8443", Room: "a_b-c"},
		"isshoni://watch.example.com/":               {Server: "https://watch.example.com"},
		"isshoni://watch.example.com/r/lounge?x=1#y": {Server: "https://watch.example.com", Room: "lounge"},
	} {
		got, err := pendingFromLink(link)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", link, got, err, want)
		}
	}
	for _, link := range []string{"isshoni:", "isshoni://", "isshoni://watch.example.com/r/bad room",
		"isshoni://user:pw@watch.example.com/", "https://watch.example.com/r/lounge"} {
		if got, err := pendingFromLink(link); err == nil {
			t.Errorf("%s: %+v", link, got)
		}
	}
}
