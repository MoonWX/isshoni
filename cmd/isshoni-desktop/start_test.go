//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/update"
	"github.com/MoonWX/isshoni/internal/version"
)

// fakeInstance is a single-instance lock that records what start asked of it.
type fakeInstance struct {
	rec        *[]string
	first      bool
	acquireErr error
	forwardErr error
	forwarded  []string
	onArgs     func([]string) // what Serve was given: a test calls it as a second launch would
	closed     int
}

func (f *fakeInstance) Acquire() (bool, error) {
	*f.rec = append(*f.rec, "acquire")
	return f.first, f.acquireErr
}

func (f *fakeInstance) Serve(onArgs func([]string)) error {
	*f.rec = append(*f.rec, "serve")
	f.onArgs = onArgs
	return nil
}

func (f *fakeInstance) Forward(args []string) error {
	*f.rec = append(*f.rec, "forward")
	f.forwarded = slices.Clone(args)
	return f.forwardErr
}

func (f *fakeInstance) Close() error { f.closed++; return nil }

type startRun struct {
	rec     []string
	out     bytes.Buffer
	inst    *fakeInstance
	pending app.Pending
	gotArgs app.Args
	gotPend app.Pending
	logs    bytes.Buffer

	// Step 2a: whether the installer's mutex exists (or the probe's error), and what was written to the Pending
	// values: a map like the registry key's, PendingRoom deleted when a write has no room (winos.WritePending).
	installer    bool
	installerErr error
	registry     map[string]string
	// The one-shot modes: the engine and SPA check of --self-check, the store, servers.json's error and the data
	// root of --forget-credentials and --purge-data.
	engine     *fakeSelfCheckEngine
	spaErr     error
	store      credstore.Store
	serversErr error
	root       string
}

// newStarter returns a starter whose OS parts are fakes that record the order of the steps.
func newStarter(r *startRun) *starter {
	if r.inst == nil {
		r.inst = &fakeInstance{first: true}
	}
	r.inst.rec = &r.rec
	return &starter{
		log:    slog.New(slog.NewTextHandler(&r.logs, nil)),
		stdout: func() io.Writer { r.rec = append(r.rec, "stdout"); return &r.out },
		installerRunning: func() (bool, error) {
			r.rec = append(r.rec, "installer-probe")
			return r.installer, r.installerErr
		},
		writePending: func(p app.Pending) error {
			r.rec = append(r.rec, "write-pending")
			if r.registry == nil {
				r.registry = map[string]string{}
			}
			r.registry["PendingServer"] = p.Server
			if p.Room == "" {
				delete(r.registry, "PendingRoom")
			} else {
				r.registry["PendingRoom"] = p.Room
			}
			return nil
		},
		openEngine: func() (selfCheckEngine, error) {
			r.rec = append(r.rec, "engine")
			if r.engine == nil {
				r.engine = goodEngine()
			}
			return r.engine, nil
		},
		checkSPA: func() error { r.rec = append(r.rec, "spa"); return r.spaErr },
		openStore: func() (credstore.Store, error) {
			r.rec = append(r.rec, "store")
			if r.store == nil {
				r.store = credstore.NewMemory()
			}
			return r.store, nil
		},
		openServers: func(store credstore.Store) (credentialForgetter, error) {
			r.rec = append(r.rec, "servers")
			if r.serversErr != nil {
				return nil, r.serversErr
			}
			return &fakeForgetter{store: store, rec: &r.rec}, nil
		},
		dataRoot: func() (string, error) { r.rec = append(r.rec, "data-root"); return r.root, nil },
		removeAll: func(p string) error {
			r.rec = append(r.rec, "remove")
			return os.RemoveAll(p)
		},
		singleInstance: func() (instance, error) {
			r.rec = append(r.rec, "single-instance")
			return r.inst, nil
		},
		holdRunning: func() error { r.rec = append(r.rec, "running-mutex"); return nil },
		// Step 6 reads and deletes the Pending values: those a step wrote (registry), else r.pending.
		takePending: func() (app.Pending, error) {
			r.rec = append(r.rec, "take-pending")
			if len(r.registry) == 0 {
				return r.pending, nil
			}
			p := app.Pending{Server: r.registry["PendingServer"], Room: r.registry["PendingRoom"]}
			clear(r.registry)
			return p, nil
		},
		runApp: func(p app.Args, _ []string, pending app.Pending, _ *forwarded, _ instance) int {
			r.rec = append(r.rec, "app")
			r.gotArgs, r.gotPend = p, pending
			return 0
		},
	}
}

// fakeForgetter is 02's servers for the one-shot modes: ForgetAll deletes every credential of the store.
type fakeForgetter struct {
	store credstore.Store
	rec   *[]string
}

func (f *fakeForgetter) ForgetAll(context.Context) error {
	*f.rec = append(*f.rec, "forget-all")
	return deleteAllCredentials(f.store)
}

func (f *fakeForgetter) Close() error { *f.rec = append(*f.rec, "servers-closed"); return nil }

// TestStartOrder is 03 §13.4's order for a first instance: single instance (served at once), the running mutex,
// the Pending values, the app; the lock is released when the start returns.
func TestStartOrder(t *testing.T) {
	r := &startRun{pending: app.Pending{Server: "https://watch.example.com", Room: "lounge"}}
	if code := newStarter(r).start([]string{"--updated-from=0.2.0"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := []string{"installer-probe", "single-instance", "acquire", "serve", "running-mutex", "take-pending", "app"}
	if !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	if r.gotPend != r.pending || r.gotArgs.UpdatedFrom != "0.2.0" {
		t.Errorf("the app got %+v and %+v", r.gotArgs, r.gotPend)
	}
	if r.inst.closed == 0 {
		t.Error("the single-instance lock wasn't released")
	}
}

// TestForwardBeforeTheApp (03 §13.3, DK3): a second launch that comes while the first instance is still in its
// slow steps (the running mutex, the Pending values, step 7's engine, WebView2 check and Credential Manager) is
// taken at once and handed to the app after its own command line and the Pending values (Boot), in order.
func TestForwardBeforeTheApp(t *testing.T) {
	r := &startRun{}
	s := newStarter(r)
	early := []string{"--open-url", "isshoni://watch.example.com/r/early"}
	s.takePending = func() (app.Pending, error) {
		r.inst.onArgs(early) // during step 6
		return app.Pending{Server: "https://watch.example.com"}, nil
	}
	var handled []string
	s.runApp = func(_ app.Args, args []string, pending app.Pending, fwd *forwarded, inst instance) int {
		r.inst.onArgs([]string{"--open-url", "isshoni://watch.example.com/r/later"}) // while step 7 loads
		if len(handled) != 0 {
			t.Errorf("handed over before the app booted: %v", handled)
		}
		// Boot: the start's own command line and the Pending values, then what second launches forwarded.
		handled = append(handled, "boot "+strings.Join(args, " ")+" "+pending.Server)
		fwd.start(func(args []string) { handled = append(handled, strings.Join(args, " ")) })
		r.inst.onArgs([]string{"--autostart"}) // after the boot: at once
		return exitOK
	}
	if code := s.start([]string{"--server", "https://a.example.com"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want := []string{"boot --server https://a.example.com https://watch.example.com",
		"--open-url isshoni://watch.example.com/r/early", "--open-url isshoni://watch.example.com/r/later",
		"--autostart"}
	if !slices.Equal(handled, want) {
		t.Errorf("handled %q, want %q", handled, want)
	}
}

// TestVersion: --version prints exactly one JSON line and exits 0, with no window, single instance, mutex or
// registry read (03 §13.4 step 1, 06 §2.4); engineAbi is the ABI major Go was built for, without the DLL.
func TestVersion(t *testing.T) {
	r := &startRun{}
	if code := newStarter(r).start([]string{"--version"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !slices.Equal(r.rec, []string{"stdout"}) {
		t.Errorf("steps %v: a one-shot mode touched more than its output", r.rec)
	}
	checkVersionLine(t, r.out.Bytes())
}

// checkVersionLine checks --version's output: one line of JSON with exactly 06 §2.4's fields.
func checkVersionLine(t *testing.T, out []byte) {
	t.Helper()
	text := strings.TrimRight(string(out), "\r\n")
	if strings.Count(string(out), "\n") != 1 || strings.Contains(text, "\n") {
		t.Fatalf("not exactly one line: %q", out)
	}
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %q: %v", text, err)
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"commit", "date", "engineAbi", "protocol", "version"}) {
		t.Errorf("fields %v", keys)
	}
	if v["engineAbi"] != json.Number("1") || media.ABIVersion != 1 {
		t.Errorf("engineAbi %v, want the ABI major 1", v["engineAbi"])
	}
	if v["version"] != version.Version() || v["commit"] != version.Commit() {
		t.Errorf("version %v commit %v", v["version"], v["commit"])
	}
	proto, _ := v["protocol"].(map[string]any)
	if proto["min"] != json.Number("1") || proto["max"] != json.Number("1") ||
		protocol.MinVersion != 1 || protocol.Version != 1 {
		t.Errorf("protocol %v", v["protocol"])
	}
}

// TestOpenURLStartsNormally: a launch with --open-url honors no one-shot mode (03 §13.1): --version and
// --purge-data are ignored and the start is a normal one that hands the link to the app.
func TestOpenURLStartsNormally(t *testing.T) {
	r := &startRun{}
	args := []string{"--open-url", "isshoni://watch.example.com/r/lounge", "--purge-data", "--version"}
	if code := newStarter(r).start(args); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !slices.Contains(r.rec, "app") || slices.Contains(r.rec, "stdout") {
		t.Errorf("steps %v", r.rec)
	}
	if r.gotArgs.Link != "isshoni://watch.example.com/r/lounge" || r.gotArgs.OneShot != "" {
		t.Errorf("the app got %+v", r.gotArgs)
	}
}

// TestSecondInstanceForwards: a second instance forwards os.Args[1:] unchanged and exits 0, before the running
// mutex and without reading the Pending values (03 §13.3, §13.4 step 3), also when the forward fails.
func TestSecondInstanceForwards(t *testing.T) {
	for _, forwardErr := range []error{nil, errors.New("pipe busy")} {
		r := &startRun{inst: &fakeInstance{first: false, forwardErr: forwardErr}}
		args := []string{"--open-url", "isshoni://watch.example.com/r/lounge", "--autostart"}
		if code := newStarter(r).start(args); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if want := []string{"installer-probe", "single-instance", "acquire", "forward"}; !slices.Equal(r.rec, want) {
			t.Errorf("steps %v, want %v", r.rec, want)
		}
		if !slices.Equal(r.inst.forwarded, args) {
			t.Errorf("forwarded %q, want the arguments unchanged", r.inst.forwarded)
		}
	}
}

// TestSingleInstanceUnavailable: a lock that can't be taken doesn't stop the start.
func TestSingleInstanceUnavailable(t *testing.T) {
	r := &startRun{inst: &fakeInstance{acquireErr: errors.New("access denied")}}
	if code := newStarter(r).start(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !slices.Equal(r.rec, []string{"installer-probe", "single-instance", "acquire", "running-mutex", "take-pending",
		"app"}) {
		t.Errorf("steps %v", r.rec)
	}
	s := newStarter(&startRun{})
	s.singleInstance = func() (instance, error) { return nil, errors.New("no SID") }
	ran := false
	s.runApp = func(app.Args, []string, app.Pending, *forwarded, instance) int { ran = true; return 0 }
	if code := s.start(nil); code != 0 || !ran {
		t.Errorf("exit %d, app ran %v", code, ran)
	}
}

// startSteps are the steps of a normal start: a one-shot mode takes none of them (no installer probe, single
// instance, running mutex, Pending values or window).
var startSteps = []string{"installer-probe", "write-pending", "single-instance", "acquire", "serve", "forward",
	"running-mutex", "take-pending", "app"}

func checkNoStartStep(t *testing.T, what string, rec []string) {
	t.Helper()
	for _, step := range rec {
		if slices.Contains(startSteps, step) {
			t.Errorf("%s: step %s (steps %v)", what, step, rec)
		}
	}
}

// TestOneShotModesSkipTheStart (03 §13.4 step 1): every one-shot mode runs before anything else, without a window,
// single instance or mutex, and the first mode on the command line wins; --update-now needs the updatetest tag;
// -Embedding exits 0 (its actions belong to a run that is gone); --sound-check-helper is step 0, before anything.
func TestOneShotModesSkipTheStart(t *testing.T) {
	out := filepath.Join(t.TempDir(), "self-check.json")
	for args, want := range map[string]int{
		"--self-check=" + out:                   exitOK,
		"--forget-credentials":                  exitOK,
		"--purge-data":                          exitOK,
		"--update-now":                          exitUnavailable,
		"-Embedding":                            exitOK,
		"--sound-check-helper":                  exitUnavailable,
		"--sound-check-helper --version":        exitUnavailable,
		"--forget-credentials --version":        exitOK, // the first mode wins
		"--version --purge-data --open-url-ish": exitOK,
	} {
		r := &startRun{installer: true, root: filepath.Join(t.TempDir(), "isshoni")}
		if code := newStarter(r).start(strings.Fields(args)); code != want {
			t.Errorf("%s: exit %d, want %d", args, code, want)
		}
		checkNoStartStep(t, args, r.rec)
		if strings.HasPrefix(args, "--forget-credentials --version") && slices.Contains(r.rec, "stdout") {
			t.Errorf("%s: --version ran too", args)
		}
		if strings.HasPrefix(args, "--version") && slices.Contains(r.rec, "remove") {
			t.Errorf("%s: --purge-data ran too", args)
		}
	}
}

// TestSelfCheckStart: --self-check=<path> loads the engine and checks the SPA, writes the report and exits 0 only
// when it is ok (03 §13.4, 06 §2.4).
func TestSelfCheckStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "self-check.json")
	r := &startRun{}
	if code := newStarter(r).start([]string{"--self-check=" + path}); code != exitOK {
		t.Fatalf("exit %d\n%s", code, r.logs.String())
	}
	if want := []string{"engine", "spa"}; !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	if v := readReport(t, path); v["ok"] != true {
		t.Errorf("report %v", v)
	}

	hooks := goodEngine()
	hooks.build.TestHooks = true
	for name, r := range map[string]*startRun{
		"test hooks":    {engine: hooks},
		"broken bundle": {spaErr: errors.New("no bridge tag")},
	} {
		if code := newStarter(r).start([]string{"--self-check=" + path}); code != exitFailed {
			t.Errorf("%s: exit %d", name, code)
		}
		if v := readReport(t, path); v["ok"] != false {
			t.Errorf("%s: report %v", name, v)
		}
	}
	r = &startRun{}
	if code := newStarter(r).start([]string{"--self-check"}); code != exitFailed || len(r.rec) != 0 {
		t.Errorf("no path: exit %d, steps %v", code, r.rec)
	}
}

// TestForgetCredentialsStart: --forget-credentials runs 02's ForgetAll and leaves no credential under the prefix;
// it exits 0, also when servers.json can't be opened (the credentials still go) and when the revokes failed.
func TestForgetCredentialsStart(t *testing.T) {
	for _, serversErr := range []error{nil, errors.New("servers.json unreadable")} {
		store := credstore.NewMemory()
		for _, o := range []string{"https://a.example.com", "https://b.example.com"} {
			if err := store.Put(o, testCredential()); err != nil {
				t.Fatal(err)
			}
		}
		root := filepath.Join(t.TempDir(), "isshoni")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		r := &startRun{store: store, serversErr: serversErr, root: root}
		if code := newStarter(r).start([]string{"--forget-credentials"}); code != exitOK {
			t.Errorf("exit %d", code)
		}
		want := []string{"data-root", "store", "servers", "forget-all", "servers-closed"}
		if serversErr != nil {
			want = []string{"data-root", "store", "servers"}
		}
		if _, err := os.Stat(root); err != nil {
			t.Errorf("--forget-credentials touched an existing data root: %v", err)
		}
		if !slices.Equal(r.rec, want) {
			t.Errorf("steps %v, want %v", r.rec, want)
		}
		if left, _ := store.List(); len(left) != 0 {
			t.Errorf("credentials left: %v", left)
		}
	}
}

// TestForgetCredentialsNoDataRoot: on a PC where the app never ran (no data root), --forget-credentials still
// forgets every credential with 02's real servers (whose ForgetAll saves servers.json, creating the folder), and
// leaves no data root behind.
func TestForgetCredentialsNoDataRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	store := credstore.NewMemory()
	if err := store.Put("https://127.0.0.1:2", testCredential()); err != nil {
		t.Fatal(err)
	}
	r := &startRun{store: store, root: root}
	s := newStarter(r)
	s.openServers = realServers(root)
	if code := s.start([]string{"--forget-credentials"}); code != exitOK {
		t.Fatalf("exit %d\n%s", code, r.logs.String())
	}
	if want := []string{"data-root", "store", "remove"}; !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	if left, err := store.List(); err != nil || len(left) != 0 {
		t.Errorf("credentials left: %v %v", left, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the mode left a data root behind: %v\n%s", err, r.logs.String())
	}
}

// TestPurgeDataStart: --purge-data forgets the credentials as --forget-credentials does, then deletes the data root,
// and exits 0; a data root it may not delete is logged at ERROR and still exits 0 (03 §13.4 step 1, 06 §2.4).
func TestPurgeDataStart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := credstore.NewMemory()
	if err := store.Put("https://a.example.com", testCredential()); err != nil {
		t.Fatal(err)
	}
	r := &startRun{store: store, root: root}
	if code := newStarter(r).start([]string{"--purge-data"}); code != exitOK {
		t.Fatalf("exit %d\n%s", code, r.logs.String())
	}
	if want := []string{"store", "servers", "forget-all", "servers-closed", "data-root", "remove"}; !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Errorf("credentials left: %v", left)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data root is still there: %v", err)
	}
	other := t.TempDir()
	r = &startRun{root: other}
	if code := newStarter(r).start([]string{"--purge-data"}); code != exitOK {
		t.Errorf("a folder that isn't a data root: exit %d", code)
	}
	if !strings.Contains(r.logs.String(), "level=ERROR") {
		t.Errorf("the refusal wasn't logged at ERROR:\n%s", r.logs.String())
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a folder that isn't a data root was touched: %v", err)
	}
}

// TestInstallerHandover is 03 §22.1's start-order row for step 2a: with the installer's mutex present, a start from
// --open-url writes PendingServer and PendingRoom (replacing older values) and exits 0, a plain start writes
// nothing and exits 0, and neither takes single instance, the running mutex, the Pending values or the app (the
// updater and Apply of step 5 come after step 3 too). With the mutex absent, or when the probe fails, the start
// takes the normal order.
func TestInstallerHandover(t *testing.T) {
	old := map[string]string{"PendingServer": "https://old.example.com", "PendingRoom": "old"}
	for _, tc := range []struct {
		args []string
		want map[string]string // the registry afterwards; nil: nothing written
	}{
		{[]string{"--open-url", "isshoni://watch.example.com/r/lounge"},
			map[string]string{"PendingServer": "https://watch.example.com", "PendingRoom": "lounge"}},
		{[]string{"--open-url", "isshoni://watch.example.com/"}, // the newest link wins: no older room stays
			map[string]string{"PendingServer": "https://watch.example.com"}},
		{[]string{"--relaunched", "--open-url", "isshoni://watch.example.com/r/lounge", "--purge-data"},
			map[string]string{"PendingServer": "https://watch.example.com", "PendingRoom": "lounge"}},
		{[]string{"--open-url", "isshoni://watch.example.com/r/bad room"}, nil},
		{[]string{"--open-url", "https://watch.example.com/r/lounge"}, nil},
		{nil, nil},
		{[]string{"--autostart"}, nil},
		{[]string{"--server", "https://watch.example.com"}, nil},
		{[]string{"--updated-from=0.2.0"}, nil},
	} {
		name := strings.Join(tc.args, " ")
		r := &startRun{installer: true, registry: maps.Clone(old), root: filepath.Join(t.TempDir(), "isshoni")}
		// A pending update waits: with the installer's mutex present no start builds the updater or applies it
		// (03 §22.1's start order), so neither open-updater nor apply may appear among the steps.
		s := withUpdater(r, &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}})
		if code := s.start(tc.args); code != exitOK {
			t.Errorf("%q: exit %d", name, code)
		}
		want := []string{"installer-probe"}
		wantReg := old
		if tc.want != nil {
			want = append(want, "write-pending")
			wantReg = tc.want
		}
		if !slices.Equal(r.rec, want) {
			t.Errorf("%q: steps %v, want %v", name, r.rec, want)
		}
		if !maps.Equal(r.registry, wantReg) {
			t.Errorf("%q: registry %v, want %v", name, r.registry, wantReg)
		}
		if strings.Contains(r.logs.String(), "lounge") {
			t.Errorf("%q: the link's room was logged:\n%s", name, r.logs.String())
		}
	}

	// Absent, or the probe failed: the normal order (step 5 builds the updater; nothing pending), nothing written.
	for _, r := range []*startRun{{}, {installerErr: errors.New("access denied")}} {
		args := []string{"--open-url", "isshoni://watch.example.com/r/lounge"}
		if code := withUpdater(r, &fakeStartUpdater{}).start(args); code != exitOK {
			t.Errorf("exit %d", code)
		}
		want := []string{"installer-probe", "single-instance", "acquire", "serve", "running-mutex", "open-updater",
			"pending-at-startup", "take-pending", "app"}
		if !slices.Equal(r.rec, want) || r.registry != nil {
			t.Errorf("steps %v (want %v), registry %v", r.rec, want, r.registry)
		}
		if r.gotArgs.Link != args[1] {
			t.Errorf("the app got %+v", r.gotArgs)
		}
	}
}

// TestSoundCheckHelperFirst: --sound-check-helper is step 0, after main's first action only: not even the
// installer probe runs before it.
func TestSoundCheckHelperFirst(t *testing.T) {
	r := &startRun{installer: true}
	newStarter(r).start([]string{"--sound-check-helper"})
	if len(r.rec) != 0 {
		t.Errorf("steps %v", r.rec)
	}
}

func TestDeviceName(t *testing.T) {
	long := strings.Repeat("ä", 70)
	for in, want := range map[string]string{
		"":               defaultDeviceName,
		"   ":            defaultDeviceName,
		"\xff\xfe":       defaultDeviceName,
		" DESKTOP-1A2B ": "DESKTOP-1A2B",
		long:             strings.Repeat("ä", 64),
	} {
		if got := deviceName(in); got != want {
			t.Errorf("deviceName(%q) = %q, want %q", in, got, want)
		}
	}
	if a := appInfo(); a.ClientKind != protocol.ClientKindDesktop || a.OS != clientOS || a.DeviceName == "" ||
		a.Version != version.Version() {
		t.Errorf("appInfo %+v", a)
	}
}

// TestBridgeConfigServers: bridge.js names the active server and the derived id of every listed one (03 §5.2,
// §8.1, §14.3).
func TestBridgeConfigServers(t *testing.T) {
	dir := t.TempDir()
	doc := `{"version":1,"active":"https://b.example.com","servers":[
	  {"origin":"https://a.example.com","name":"A","addedAt":"2026-10-01T00:00:00Z"},
	  {"origin":"https://b.example.com","name":"B","addedAt":"2026-10-02T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: dir, Store: credstore.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	c := bridgeConfig(appFacts(), srv)()
	if !slices.Equal(c.ServerIDs, []string{servers.LocalID("https://a.example.com"), servers.LocalID("https://b.example.com")}) {
		t.Errorf("serverIds %v", c.ServerIDs)
	}
	if c.Server == nil || c.Server.Origin != "https://b.example.com" || c.Server.Name != "B" ||
		c.Server.ID != servers.LocalID("https://b.example.com") {
		t.Errorf("server %+v", c.Server)
	}
	// After a switch the next bridge.js follows (every switch reloads the window, 03 §8.1).
	if err := srv.SetActive("https://a.example.com"); err != nil {
		t.Fatal(err)
	}
	if c := bridgeConfig(appFacts(), srv)(); c.Server == nil || c.Server.Origin != "https://a.example.com" {
		t.Errorf("after the switch: %+v", c.Server)
	}
	empty, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: t.TempDir(), Store: credstore.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = empty.Close() }()
	if c := bridgeConfig(appFacts(), empty)(); c.Server != nil || c.ServerIDs == nil || len(c.ServerIDs) != 0 {
		t.Errorf("no servers: %+v", c)
	}
}

func TestWriteVersionError(t *testing.T) {
	if err := writeVersion(failingWriter{}); err == nil {
		t.Error("a failed write wasn't reported")
	}
	r := &startRun{}
	s := newStarter(r)
	s.stdout = func() io.Writer { return failingWriter{} }
	if code := s.start([]string{"--version"}); code != exitFailed {
		t.Errorf("exit %d", code)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// TestForwardedQueue: command lines that second launches forward before the app booted wait, in order, and at most
// maxQueuedForwards of them; afterwards each goes to the app at once; once Wails shuts down (stop) they are dropped.
func TestForwardedQueue(t *testing.T) {
	var got []string
	handle := func(args []string) { got = append(got, strings.Join(args, " ")) }
	var logs bytes.Buffer
	f := newForwarded(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for i := range maxQueuedForwards + 2 {
		f.receive([]string{"--open-url", "isshoni://x/r/" + string(rune('a'+i))})
	}
	if len(got) != 0 {
		t.Fatalf("handled before the app booted: %v", got)
	}
	f.start(handle)
	if len(got) != maxQueuedForwards || got[0] != "--open-url isshoni://x/r/a" || got[7] != "--open-url isshoni://x/r/h" {
		t.Fatalf("queued: %v", got)
	}
	f.receive([]string{"--autostart"})
	if got[len(got)-1] != "--autostart" {
		t.Errorf("after the boot: %v", got)
	}
	f.stop()
	n := len(got)
	f.receive([]string{"--open-url", "isshoni://x/r/late"})
	if len(got) != n || !strings.Contains(logs.String(), "dropped") {
		t.Errorf("handled after stop: %v\n%s", got, logs.String())
	}

	// Stopped before the boot (Wails quit before it started): nothing queued is handed over.
	got = nil
	g := newForwarded(slog.New(slog.DiscardHandler))
	g.receive([]string{"--autostart"})
	g.stop()
	g.start(handle)
	g.receive([]string{"--autostart"})
	if len(got) != 0 {
		t.Errorf("handled after stop: %v", got)
	}
}
