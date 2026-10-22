//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	"github.com/MoonWX/isshoni/internal/version"
)

// fakeInstance is a single-instance lock that records what start asked of it.
type fakeInstance struct {
	rec        *[]string
	first      bool
	acquireErr error
	forwardErr error
	forwarded  []string
}

func (f *fakeInstance) Acquire() (bool, error) {
	*f.rec = append(*f.rec, "acquire")
	return f.first, f.acquireErr
}

func (f *fakeInstance) Serve(func([]string)) error {
	*f.rec = append(*f.rec, "serve")
	return nil
}

func (f *fakeInstance) Forward(args []string) error {
	*f.rec = append(*f.rec, "forward")
	f.forwarded = slices.Clone(args)
	return f.forwardErr
}

func (f *fakeInstance) Close() error { return nil }

type startRun struct {
	rec     []string
	out     bytes.Buffer
	inst    *fakeInstance
	pending app.Pending
	gotArgs app.Args
	gotPend app.Pending
	logs    bytes.Buffer
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
		singleInstance: func() (instance, error) {
			r.rec = append(r.rec, "single-instance")
			return r.inst, nil
		},
		holdRunning: func() error { r.rec = append(r.rec, "running-mutex"); return nil },
		takePending: func() (app.Pending, error) { r.rec = append(r.rec, "take-pending"); return r.pending, nil },
		runApp: func(p app.Args, _ []string, pending app.Pending, inst instance) int {
			r.rec = append(r.rec, "app")
			r.gotArgs, r.gotPend = p, pending
			return 0
		},
	}
}

// TestStartOrder is 03 §13.4's order for a first instance: single instance, the running mutex, the Pending values,
// the app.
func TestStartOrder(t *testing.T) {
	r := &startRun{pending: app.Pending{Server: "https://watch.example.com", Room: "lounge"}}
	if code := newStarter(r).start([]string{"--updated-from=0.2.0"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := []string{"single-instance", "acquire", "running-mutex", "take-pending", "app"}
	if !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	if r.gotPend != r.pending || r.gotArgs.UpdatedFrom != "0.2.0" {
		t.Errorf("the app got %+v and %+v", r.gotArgs, r.gotPend)
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
		if want := []string{"single-instance", "acquire", "forward"}; !slices.Equal(r.rec, want) {
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
	if !slices.Equal(r.rec, []string{"single-instance", "acquire", "running-mutex", "take-pending", "app"}) {
		t.Errorf("steps %v", r.rec)
	}
	s := newStarter(&startRun{})
	s.singleInstance = func() (instance, error) { return nil, errors.New("no SID") }
	ran := false
	s.runApp = func(app.Args, []string, app.Pending, instance) int { ran = true; return 0 }
	if code := s.start(nil); code != 0 || !ran {
		t.Errorf("exit %d, app ran %v", code, ran)
	}
}

// TestOneShotModesNotYetHere: the modes of later slices exit without a window, single instance or mutex; -Embedding
// exits 0 (its actions belong to a run that is gone); --sound-check-helper is step 0, before anything.
func TestOneShotModesNotYetHere(t *testing.T) {
	for args, want := range map[string]int{
		"--self-check=out.json":                 exitUnavailable,
		"--forget-credentials":                  exitUnavailable,
		"--purge-data":                          exitUnavailable,
		"--update-now":                          exitUnavailable,
		"-Embedding":                            exitOK,
		"--sound-check-helper":                  exitUnavailable,
		"--sound-check-helper --version":        exitUnavailable,
		"--forget-credentials --version":        exitUnavailable, // the first mode wins
		"--version --purge-data --open-url-ish": exitOK,
	} {
		r := &startRun{}
		if code := newStarter(r).start(strings.Fields(args)); code != want {
			t.Errorf("%s: exit %d, want %d", args, code, want)
		}
		for _, step := range r.rec {
			if step != "stdout" {
				t.Errorf("%s: step %s", args, step)
			}
		}
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
// maxQueuedForwards of them; afterwards each goes to the app at once.
func TestForwardedQueue(t *testing.T) {
	var got []string
	f := &forwarded{handle: func(args []string) { got = append(got, strings.Join(args, " ")) }}
	for i := range maxQueuedForwards + 2 {
		f.receive([]string{"--open-url", "isshoni://x/r/" + string(rune('a'+i))})
	}
	if len(got) != 0 {
		t.Fatalf("handled before the app booted: %v", got)
	}
	f.start()
	if len(got) != maxQueuedForwards || got[0] != "--open-url isshoni://x/r/a" || got[7] != "--open-url isshoni://x/r/h" {
		t.Fatalf("queued: %v", got)
	}
	f.receive([]string{"--autostart"})
	if got[len(got)-1] != "--autostart" {
		t.Errorf("after the boot: %v", got)
	}
}
