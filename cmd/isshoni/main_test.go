package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"go.uber.org/goleak"
)

var update = flag.Bool("update", false, "rewrite the expected output in testdata/script")

func TestMain(m *testing.M) {
	testscript.Main(leakCheck{m}, map[string]func(){"isshoni": scriptMain})
}

// leakCheck fails a run that passed when a goroutine outlives the tests (goleak): the CLI's signal watcher, the
// call that setup-url --wait runs on the side, and the fake admin servers of the tests must all have stopped.
type leakCheck struct{ m *testing.M }

func (l leakCheck) Run() int {
	code := l.m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "goleak: goroutines are still running after the tests:\n%v\n", err)
			return 1
		}
	}
	return code
}

// scriptMain is the CLI as the test scripts and serve_test.go run it: this test binary under the name isshoni. It
// is main, except that it honors strangerEnv.
func scriptMain() {
	inv := processInvocation()
	if os.Getenv(strangerEnv) == "1" {
		inv.dialAdmin = dialAsStranger
	}
	os.Exit(runMain(inv))
}

// TestScripts runs testdata/script/*.txtar against the real binary (this test binary, re-executed as isshoni).
func TestScripts(t *testing.T) {
	t.Parallel()
	coverDir := os.Getenv("GOCOVERDIR")
	if coverDir != "" {
		t.Cleanup(func() { mergeScriptCoverage(t, coverDir) })
	}
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"exitcode":   cmdExitCode,
			"fakedb":     cmdFakeDB,
			"fakeserver": cmdFakeServer,
			"holdport":   cmdHoldPort,
		},
		Condition: func(cond string) (bool, error) {
			switch cond {
			case "root": // file modes don't keep root out
				return os.Geteuid() == 0, nil
			case "peercred": // the admin socket checks peer credentials on this platform
				return peerCreds, nil
			default:
				return false, fmt.Errorf("unknown condition %q", cond)
			}
		},
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		UpdateScripts:       *update,
		Setup: func(env *testscript.Env) error {
			// Under -race a process that exits 0 first sleeps atexit_sleep_ms (1 s by default) to flush reports.
			env.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			// Never read the machine's /etc/isshoni/isshoni.toml: an empty config file unless a script sets another.
			empty := filepath.Join(env.WorkDir, ".empty.toml")
			env.Setenv("ISSHONI_CONFIG", empty)
			if err := os.WriteFile(empty, nil, 0o600); err != nil {
				return err
			}
			// Under go test -cover, each script's programs write their coverage into a GOCOVERDIR of the script's
			// own (mergeScriptCoverage).
			if coverDir != "" {
				own, err := os.MkdirTemp(coverDir, scriptCoverPrefix)
				if err != nil {
					return err
				}
				env.Setenv("GOCOVERDIR", own)
			}
			// Never talk to the machine's /run/isshoni/admin.sock either: the script gets an admin socket path of
			// its own, where "fakeserver start" serves a fake (fake_test.go).
			return setupScriptFake(env)
		},
	})
}

// scriptCoverPrefix starts the names of the scripts' own coverage directories inside GOCOVERDIR.
const scriptCoverPrefix = "testscript-"

// mergeScriptCoverage moves the coverage files of the scripts' own directories into GOCOVERDIR once every script
// has ended, so that the scripts' coverage counts as before. A script's programs don't write into GOCOVERDIR
// itself: the coverage runtime names the temporary copy of its meta-data file by the clock alone, so two programs
// of parallel scripts that exited within one clock tick wrote the same file, and the loser printed "error: coverage
// meta-data emit failed: … rename … no such file or directory" to the stderr that scripts check is empty. The
// meta-data file is the same in every directory (one binary), so one that GOCOVERDIR already has stays behind.
// go test reads only the files directly in GOCOVERDIR, so a directory left behind on an error changes nothing.
func mergeScriptCoverage(t *testing.T, coverDir string) {
	root, err := os.OpenRoot(coverDir)
	if err != nil {
		t.Errorf("coverage of the scripts: %v", err)
		return
	}
	defer func() { _ = root.Close() }()
	dirs, err := fs.Glob(root.FS(), scriptCoverPrefix+"*")
	if err != nil {
		t.Errorf("coverage of the scripts: %v", err)
		return
	}
	for _, dir := range dirs {
		files, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			t.Errorf("coverage of the scripts: %v", err)
			continue
		}
		for _, f := range files {
			name := f.Name()
			meta := strings.HasPrefix(name, "covmeta.")
			if !meta && !strings.HasPrefix(name, "covcounters.") {
				continue
			}
			if _, err := root.Stat(name); meta && err == nil {
				continue
			}
			if err := root.Rename(filepath.Join(dir, name), name); err != nil {
				t.Errorf("coverage of the scripts: %v", err)
			}
		}
		if err := root.RemoveAll(dir); err != nil {
			t.Errorf("coverage of the scripts: %v", err)
		}
	}
}

// cmdExitCode is the script command "exitcode N PROG [ARGS…]": it runs PROG like exec and fails unless the exit code
// is N. stdout and stderr are kept for the next commands, as after exec.
func cmdExitCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: exitcode N PROG [ARGS…]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("exitcode: bad code %q", args[0])
	}
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			ts.Fatalf("exitcode: %v", err)
		}
		got = ee.ExitCode()
	}
	if got != want {
		ts.Fatalf("exit code %d, want %d", got, want)
	}
}

// cmdFakeDB is the script command "fakedb FILE SCHEMA APP": it writes a database file as the fake server knows
// them (fakeDatabase), of the schema version SCHEMA, last used by isshoni APP. A script can't hold one itself: the
// file starts with SQLite's header, which has a NUL byte.
func cmdFakeDB(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: fakedb FILE SCHEMA APP")
	}
	schema, err := strconv.Atoi(args[1])
	if err != nil {
		ts.Fatalf("fakedb: bad schema %q", args[1])
	}
	ts.Check(os.WriteFile(ts.MkAbs(args[0]), fakeDatabase(schema, args[2]), 0o600))
}

// cmdHoldPort is the script command "holdport": it listens on a TCP port of the loopback interface until the script
// ends and puts the port's number into $HELDPORT, so that a script has a port that another program holds.
func cmdHoldPort(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 0 {
		ts.Fatalf("usage: holdport")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	ts.Check(err)
	ts.Defer(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	ts.Check(err)
	ts.Setenv("HELDPORT", port)
}

// runCLI runs the CLI in-process with an empty config file (never the machine's /etc/isshoni/isshoni.toml).
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLIEnv(t, nil, args...)
}

// runCLIEnv is runCLI with extra environment variables (os.Environ form). ISSHONI_CONFIG defaults to an empty file
// and ISSHONI_LISTEN_ADMIN_SOCKET to a path where nothing listens, so a test never reads the machine's config or
// talks to a server that runs on it. strangerEnv=1 is the test's own switch, as in the scripts.
func runCLIEnv(t *testing.T, environ []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLITTY(t, false, environ, args...)
}

// runCLITTY is runCLIEnv with stdout as a terminal (tty), or as a pipe.
func runCLITTY(t *testing.T, tty bool, environ []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLIWith(t, cliRun{environ: environ, stdoutTTY: tty}, args...)
}

// cliRun is what one in-process run of the CLI finds around it, beyond its arguments.
type cliRun struct {
	environ   []string // extra environment variables, as for runCLIEnv
	stdin     string   // what stdin holds
	stdinTTY  bool     // stdin is a terminal: stdin is then what the operator types
	stdoutTTY bool     // stdout is a terminal
	root      bool     // the command runs as root
}

// runCLIWith runs the CLI in-process as runCLIEnv does, in the surroundings r. The machine is never a container
// by its files, wherever the tests run (hostRoot); a test makes it one with ISSHONI_IN_CONTAINER=1 in r.environ.
func runCLIWith(t *testing.T, r cliRun, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	inv := &invocation{
		stdin:    strings.NewReader(r.stdin),
		stdout:   &out,
		stderr:   &errOut,
		environ:  append([]string{"ISSHONI_CONFIG=" + empty, "ISSHONI_LISTEN_ADMIN_SOCKET=" + socketPath(t)}, r.environ...),
		isTTY:    func(w io.Writer) bool { return r.stdoutTTY && w == io.Writer(&out) },
		stdinTTY: func() bool { return r.stdinTTY },
		hostRoot: t.TempDir(),
		// Whoever runs the tests, the command runs as an ordinary user unless the test says root.
		geteuid: func() int {
			if r.root {
				return 0
			}
			return 1000
		},
	}
	if slices.Contains(r.environ, strangerEnv+"=1") {
		inv.dialAdmin = dialAsStranger
	}
	code = run(t.Context(), inv, args)
	return code, out.String(), errOut.String()
}

// leaves returns every leaf command's path below c, e.g. ["admin", "users", "list"].
func leaves(c *command, prefix []string) [][]string {
	var out [][]string
	for _, s := range c.subs {
		p := append(append([]string(nil), prefix...), s.name)
		if s.setup != nil {
			out = append(out, p)
		} else {
			out = append(out, leaves(s, p)...)
		}
	}
	return out
}

// groups returns every group's path, the root's included (empty).
func groups(c *command, prefix []string) [][]string {
	out := [][]string{prefix}
	for _, s := range c.subs {
		if len(s.subs) > 0 {
			out = append(out, groups(s, append(append([]string(nil), prefix...), s.name))...)
		}
	}
	return out
}

// The command tree of 04 §3.1.
func TestCommandTree(t *testing.T) {
	var got []string
	for _, p := range leaves(root(), nil) {
		got = append(got, strings.Join(p, " "))
	}
	want := []string{
		"serve", "setup-url", "doctor", "healthcheck",
		"admin status", "admin backup", "admin restore",
		"admin users list", "admin users reset-password", "admin users set-role", "admin users disable", "admin users enable",
		"admin invite create", "admin rotate-secrets", "admin log-level",
		"config check", "config print", "config example", "config init",
		"version", "help",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("leaves:\n got %v\nwant %v", got, want)
	}
}

// Every command answers -h, --help and "isshoni help …" with the same usage on stdout.
func TestEveryCommandHasHelp(t *testing.T) {
	paths := append(groups(root(), nil), leaves(root(), nil)...)
	for _, p := range paths {
		name := strings.TrimSpace("isshoni " + strings.Join(p, " "))
		t.Run(name, func(t *testing.T) {
			code, want, stderr := runCLI(t, append([]string{"help"}, p...)...)
			if code != exitOK || stderr != "" {
				t.Fatalf("help: exit %d, stderr %q", code, stderr)
			}
			if !strings.HasPrefix(want, "Usage: "+name+" ") {
				t.Errorf("help starts with %q, want the usage line of %s", strings.SplitN(want, "\n", 2)[0], name)
			}
			if !strings.Contains(want, "\nFlags:\n") || !strings.Contains(want, "-h, --help") {
				t.Errorf("help has no flags section:\n%s", want)
			}
			for _, h := range []string{"-h", "--help", "-help"} {
				code, got, stderr := runCLI(t, append(append([]string(nil), p...), h)...)
				if code != exitOK || stderr != "" || got != want {
					t.Errorf("%s %s: exit %d, stderr %q, stdout differs from help: %v", name, h, code, stderr, got != want)
				}
			}
		})
	}
}

// validArgs returns arguments that the leaf name accepts, so that it gets past its own checks and as far as the
// admin socket. The destructive commands need --yes for that (a test has no terminal to confirm on), restore a
// file that is a backup by its first bytes, and backup a place for its archive.
func validArgs(t *testing.T, name string) []string {
	t.Helper()
	switch name {
	case "admin backup":
		return []string{"--out", filepath.Join(t.TempDir(), "backup.tar.gz")}
	case "admin restore":
		file := filepath.Join(t.TempDir(), "pre-7-20261014T021500Z.db")
		if err := os.WriteFile(file, fakeDatabase(fakeSchema, "0.3.0"), 0o600); err != nil {
			t.Fatal(err)
		}
		return []string{"--yes", file}
	case "admin rotate-secrets":
		return []string{"--yes"}
	case "admin users reset-password", "admin users disable", "admin users enable":
		return []string{"alice"}
	case "admin users set-role":
		return []string{"alice", "admin"}
	case "admin log-level":
		return []string{"debug"}
	default:
		return nil
	}
}

// clientCommands are the leaves that talk to the running server over its admin socket.
var clientCommands = map[string]bool{
	"setup-url": true, "healthcheck": true, "admin status": true, "admin users list": true,
	"admin users reset-password": true, "admin users set-role": true, "admin users disable": true,
	"admin users enable": true, "admin invite create": true, "admin log-level": true,
	"admin backup": true, "admin restore": true, "admin rotate-secrets": true,
}

// Every leaf is accounted for: a client command, or one that works on its own. doctor is of the last kind: it
// asks a running server, and without one it runs its checks itself (doctor_test.go).
func TestLeavesAreClassified(t *testing.T) {
	standalone := map[string]bool{
		"serve": true, "version": true, "help": true, "config check": true, "config print": true, "config example": true, "config init": true,
		"doctor": true,
	}
	for _, p := range leaves(root(), nil) {
		name := strings.Join(p, " ")
		n := 0
		for _, set := range []map[string]bool{clientCommands, standalone} {
			if set[name] {
				n++
			}
		}
		if n != 1 {
			t.Errorf("isshoni %s is in %d of the test's command lists, want exactly 1", name, n)
		}
	}
}

// Without a server every client command exits 4 with the message of 04 §12.1 (healthcheck: 1), and prints nothing
// on stdout.
func TestClientCommandsWithoutServer(t *testing.T) {
	for _, p := range leaves(root(), nil) {
		name := strings.Join(p, " ")
		if !clientCommands[name] {
			continue
		}
		sock := socketPath(t)
		valid := validArgs(t, name)
		code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_LISTEN_ADMIN_SOCKET=" + sock}, append(p, valid...)...)
		wantCode := exitUnreachable
		if name == "healthcheck" {
			wantCode = exitRuntime
		}
		if want := "isshoni is not running (no server on " + sock + ")\n"; code != wantCode || stdout != "" || stderr != want {
			t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit %d and %q", name, code, stdout, stderr, wantCode, want)
		}
		// --socket wins over listen.admin_socket.
		other := filepath.Join(filepath.Dir(sock), "other.sock")
		args := append(append(append([]string(nil), p...), "--socket", other), valid...)
		if _, _, stderr := runCLIEnv(t, []string{"ISSHONI_LISTEN_ADMIN_SOCKET=" + sock}, args...); !strings.Contains(stderr, "(no server on "+other+")") {
			t.Errorf("isshoni %s --socket: stderr %q, want the --socket path", name, stderr)
		}
	}
}

// Values at the edges of what the CLI accepts get past its checks: to the admin socket, where no server answers
// here (exit 4). TestRestoreArguments has restore's "-" and "-- -file".
func TestAcceptedValues(t *testing.T) {
	for _, args := range [][]string{
		{"admin", "invite", "create", "--uses", "1", "--ttl", "1h"},
		{"admin", "invite", "create", "--uses", "1000", "--ttl", "720h"},
		{"admin", "invite", "create", "--ttl", "168h"},
		{"admin", "invite", "create", "--uses", "0", "--ttl", "0"}, // 0 is the "not sent" value
		{"admin", "log-level", "--for", "1s", "warn"},
		{"admin", "users", "set-role", "--socket", "/x", "alice", "user"}, // flags before the arguments
		{"setup-url", "--wait", "0s", "--qr"},
		{"healthcheck", "--ready", "--wait", "0"},
	} {
		code, stdout, stderr := runCLI(t, args...)
		name := strings.Join(args, " ")
		leaf := strings.Join(leafPath(args), " ")
		switch leaf {
		case "healthcheck":
			if code != exitRuntime || stdout != "" || !strings.HasPrefix(stderr, "isshoni is not running (no server on ") {
				t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit 1 and \"isshoni is not running\"", name, code, stdout, stderr)
			}
		default:
			if code != exitUnreachable || stdout != "" || !strings.HasPrefix(stderr, "isshoni is not running (no server on ") {
				t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit 4 and \"isshoni is not running\"", name, code, stdout, stderr)
			}
		}
	}
}

// leafPath returns the command words at the start of args: those before the first flag or argument.
func leafPath(args []string) []string {
	c := root()
	var p []string
	for _, a := range args {
		s := c.sub(a)
		if s == nil {
			break
		}
		c, p = s, append(p, a)
	}
	return p
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		args     []string
		wantCode int
		wantErr  string // first line of stderr
	}{
		{nil, exitUsage, "isshoni: missing command"},
		{[]string{"--bogus"}, exitUsage, "isshoni: unknown flag --bogus"},
		{[]string{"-bogus"}, exitUsage, "isshoni: unknown flag --bogus"},
		{[]string{"bogus"}, exitUsage, `isshoni: unknown command "bogus"`},
		{[]string{"sreve"}, exitUsage, `isshoni: unknown command "sreve" (did you mean "serve"?)`},
		{[]string{"--", "--version"}, exitUsage, "isshoni: unknown flag --version"},
		{[]string{"--version", "serve"}, exitUsage, "isshoni: --version takes no arguments; use 'isshoni version'"},
		{[]string{"--version", "serve", "--bogus"}, exitUsage, "isshoni: --version takes no arguments; use 'isshoni version'"},
		{[]string{"version", "--bogus"}, exitUsage, "isshoni version: unknown flag --bogus"},
		{[]string{"version", "--short", "--json"}, exitUsage, "isshoni version: --short and --json can't be combined"},
		{[]string{"version", "extra"}, exitUsage, `isshoni version: unexpected argument "extra"`},
		{[]string{"version", "--short=maybe"}, exitUsage, `isshoni version: invalid boolean value "maybe" for --short: parse error`},
		{[]string{"serve", "--tls.mod", "ip"}, exitUsage, "isshoni serve: unknown flag --tls.mod"},
		{[]string{"serve", "--tls.mode"}, exitUsage, "isshoni serve: flag needs an argument: --tls.mode"},
		{[]string{"serve", "extra"}, exitUsage, `isshoni serve: unexpected argument "extra"`},
		{[]string{"config", "example", "--config", "x.toml"}, exitUsage, "isshoni config example: unknown flag --config"},
		{[]string{"config", "init", "--path", "x.toml", "--config", "y.toml"}, exitUsage, "isshoni config init: unknown flag --config"},
		{[]string{"config", "check", "--json"}, exitUsage, "isshoni config check: unknown flag --json"},
		{[]string{"admin"}, exitUsage, "isshoni admin: missing command"},
		{[]string{"admin", "--json"}, exitUsage, "isshoni admin: unknown flag --json"},
		{[]string{"admin", "users", "lst"}, exitUsage, `isshoni admin users: unknown command "lst" (did you mean "list"?)`},
		{[]string{"admin", "restore"}, exitUsage, "isshoni admin restore: missing argument PATH|-"},
		{[]string{"admin", "restore", "a.tar.gz", "--yes"}, exitUsage, "isshoni admin restore: flag --yes after an argument: flags go before arguments"},
		{[]string{"admin", "restore", "a.tar.gz", "b.tar.gz"}, exitUsage, `isshoni admin restore: unexpected argument "b.tar.gz"`},
		{[]string{"admin", "users", "set-role", "alice", "--socket", "/x"}, exitUsage, "isshoni admin users set-role: flag --socket after an argument: flags go before arguments"},
		{[]string{"admin", "users", "set-role", "alice", "--yes"}, exitUsage, "isshoni admin users set-role: flag --yes after an argument: flags go before arguments"},
		{[]string{"admin", "users", "set-role", "alice", "-json", "admin"}, exitUsage, "isshoni admin users set-role: flag -json after an argument: flags go before arguments"},
		{[]string{"version", "extra", "--json"}, exitUsage, "isshoni version: flag --json after an argument: flags go before arguments"},
		{[]string{"admin", "users", "set-role", "alice"}, exitUsage, "isshoni admin users set-role: missing arguments: want NAME admin|user"},
		{[]string{"admin", "users", "set-role", "alice", "root"}, exitUsage, `isshoni admin users set-role: role "root": want admin or user`},
		{[]string{"admin", "log-level", "loud"}, exitUsage, `isshoni admin log-level: log level "loud": want debug, info, warn or error`},
		{[]string{"admin", "log-level", "--for"}, exitUsage, "isshoni admin log-level: flag needs an argument: --for"},
		{[]string{"admin", "log-level", "--for", "soon", "debug"}, exitUsage, `isshoni admin log-level: invalid value "soon" for flag --for: parse error`},
		{[]string{"admin", "log-level", "--for", "-5m", "debug"}, exitUsage, "isshoni admin log-level: --for -5m0s: want a positive duration"},
		{[]string{"admin", "log-level", "--for", "0s", "info"}, exitUsage, "isshoni admin log-level: --for 0s: want a positive duration"},
		{[]string{"admin", "invite", "create", "--uses", "ten"}, exitUsage, `isshoni admin invite create: invalid value "ten" for flag --uses: parse error`},
		{[]string{"admin", "invite", "create", "--uses", "-3"}, exitUsage, "isshoni admin invite create: --uses -3: want 1 to 1000"},
		{[]string{"admin", "invite", "create", "--uses", "1001"}, exitUsage, "isshoni admin invite create: --uses 1001: want 1 to 1000"},
		{[]string{"admin", "invite", "create", "--ttl", "90m"}, exitUsage, "isshoni admin invite create: --ttl 1h30m0s: want whole hours from 1h to 720h"},
		{[]string{"admin", "invite", "create", "--ttl", "30m"}, exitUsage, "isshoni admin invite create: --ttl 30m0s: want whole hours from 1h to 720h"},
		{[]string{"admin", "invite", "create", "--ttl", "1000h"}, exitUsage, "isshoni admin invite create: --ttl 1000h0m0s: want whole hours from 1h to 720h"},
		{[]string{"admin", "invite", "create", "--ttl", "-24h"}, exitUsage, "isshoni admin invite create: --ttl -24h0m0s: want whole hours from 1h to 720h"},
		{[]string{"setup-url", "--qr", "--no-qr"}, exitUsage, "isshoni setup-url: --qr and --no-qr can't be combined"},
		{[]string{"setup-url", "--wait", "-1s"}, exitUsage, "isshoni setup-url: --wait -1s: want a duration of zero or more"},
		{[]string{"setup-url", "--wait", "2h"}, exitUsage, "isshoni setup-url: --wait 2h0m0s: want at most 1h"},
		{[]string{"setup-url", "--json", "--wait", "1h0m1s"}, exitUsage, "isshoni setup-url: --wait 1h0m1s: want at most 1h"},
		{[]string{"healthcheck", "--wait", "-1s"}, exitRuntime, "isshoni healthcheck: --wait -1s: want a duration of zero or more"},
		{[]string{"doctor", "--quality", "720p30"}, exitUsage, `isshoni doctor: --quality "720p30": want 1080p60, 1440p60 or 2160p60`},
		{[]string{"doctor", "--preset", "game"}, exitUsage, `isshoni doctor: --preset "game": want auto or movie`},
		{[]string{"config", "init"}, exitUsage, "isshoni config init: --path is required"},
		{[]string{"help", "bogus"}, exitUsage, `isshoni help: unknown command "bogus"`},
		{[]string{"help", "admin", "users", "lst"}, exitUsage, `isshoni help: unknown command "lst" (did you mean "list"?)`},
		// healthcheck exits only 0 or 1 (Docker reserves 2 in HEALTHCHECK).
		{[]string{"healthcheck", "--bogus"}, exitRuntime, "isshoni healthcheck: unknown flag --bogus"},
		{[]string{"healthcheck", "extra"}, exitRuntime, `isshoni healthcheck: unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d", code, tt.wantCode)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			first, rest, _ := strings.Cut(stderr, "\n")
			if first != tt.wantErr {
				t.Errorf("stderr first line = %q\n                     want %q", first, tt.wantErr)
			}
			if !strings.HasPrefix(rest, "Usage: isshoni") {
				t.Errorf("stderr has no usage after the error:\n%s", stderr)
			}
		})
	}
}

// The values of 04 §3.2; install.sh, systemd (RestartPreventExitStatus=78) and Docker depend on them.
func TestExitCodes(t *testing.T) {
	codes := map[string][2]int{
		"ok": {exitOK, 0}, "runtime": {exitRuntime, 1}, "usage": {exitUsage, 2}, "unreachable": {exitUnreachable, 4},
		"doctor": {exitDoctorFail, 5}, "refused": {exitRefused, 7}, "restart": {exitRestart, 75},
		"config": {exitConfig, 78}, "interrupted": {exitInterrupted, 130},
	}
	for name, c := range codes {
		if c[0] != c[1] {
			t.Errorf("exit code %s = %d, want %d", name, c[0], c[1])
		}
	}
}

func TestExitCodeOf(t *testing.T) {
	ctx := t.Context()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	cause := errors.New("config error: tls.mode = \"auto\" needs a domain")
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"success", ctx, nil, exitOK},
		{"runtime error", ctx, errors.New("port 443 is in use"), exitRuntime},
		{"usage error", ctx, usageErrorf("missing command"), exitUsage},
		{"config error", ctx, exitWith(exitConfig, cause), exitConfig},
		{"wrapped exit error", ctx, fmt.Errorf("serve: %w", exitWith(exitConfig, cause)), exitConfig},
		{"silent exit", ctx, exitWith(exitDoctorFail, nil), exitDoctorFail},
		{"refused", ctx, exitWith(exitRefused, errors.New("setup_unavailable")), exitRefused},
		{"unreachable", ctx, exitWith(exitUnreachable, errors.New("isshoni is not running")), exitUnreachable},
		{"restart", ctx, exitWith(exitRestart, errors.New("restart required")), exitRestart},
		{"interrupted", canceled, context.Canceled, exitInterrupted},
		{"exit code wins over the interrupt", canceled, exitWith(exitConfig, cause), exitConfig},
	}
	for _, tt := range tests {
		if got := exitCodeOf(tt.ctx, tt.err); got != tt.want {
			t.Errorf("%s: exitCodeOf = %d, want %d", tt.name, got, tt.want)
		}
	}
	if !errors.Is(exitWith(exitConfig, cause), cause) {
		t.Error("exitWith does not wrap its error")
	}
}

func TestReport(t *testing.T) {
	cmd := root().sub("doctor")
	var stderr bytes.Buffer
	report(&invocation{stderr: &stderr}, cmd, exitWith(exitDoctorFail, nil))
	if stderr.Len() != 0 {
		t.Errorf("silent exit printed %q", stderr.String())
	}
	report(&invocation{stderr: &stderr}, cmd, exitWith(exitConfig, errors.New("bad config")))
	if got := stderr.String(); got != "isshoni doctor: bad config\n" {
		t.Errorf("report = %q", got)
	}
}

func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, errOut bytes.Buffer
	// The call to the admin socket fails at once: its context is done.
	empty := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"ISSHONI_CONFIG=" + empty, "ISSHONI_LISTEN_ADMIN_SOCKET=" + socketPath(t)}
	if code := run(ctx, &invocation{stdout: &out, stderr: &errOut, environ: env}, []string{"admin", "status"}); code != exitInterrupted {
		t.Errorf("a failing command after SIGINT: exit %d, want %d", code, exitInterrupted)
	}
	if code := run(ctx, &invocation{stdout: &out, stderr: &errOut}, []string{"version"}); code != exitOK {
		t.Errorf("a succeeding command after SIGINT: exit %d, want 0", code)
	}
}

func TestFlagMessage(t *testing.T) {
	tests := map[string]string{
		"flag provided but not defined: -bogus":            "unknown flag --bogus",
		"flag needs an argument: -wait":                    "flag needs an argument: --wait",
		`invalid value "x" for flag -wait: parse error`:    `invalid value "x" for flag --wait: parse error`,
		`invalid boolean value "x" for -json: parse error`: `invalid boolean value "x" for --json: parse error`,
		"bad flag syntax: ---x":                            "bad flag syntax: ---x",
		`invalid value "-5" for flag -uses: out of range`:  `invalid value "-5" for flag --uses: out of range`,
	}
	for in, want := range tests {
		if got := flagMessage(errors.New(in)); got != want {
			t.Errorf("flagMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggest(t *testing.T) {
	names := []string{"serve", "setup-url", "doctor", "healthcheck", "admin", "config", "version", "help"}
	tests := map[string]string{
		"sreve": "serve", "serv": "serve", "docter": "doctor", "versio": "version", "hlep": "help",
		"setupurl": "setup-url", "xyz": "", "completely-different": "",
	}
	for in, want := range tests {
		if got := suggest(in, names); got != want {
			t.Errorf("suggest(%q) = %q, want %q", in, got, want)
		}
	}
	if d := editDistance("kitten", "sitting"); d != 3 {
		t.Errorf("editDistance(kitten, sitting) = %d, want 3", d)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("one two three four\n\nfive six", 9)
	if want := "one two\nthree\nfour\n\nfive six"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}

func TestDefaultText(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Duration("a", 0, "")
	fs.Duration("b", 30*60e9, "")
	fs.Duration("c", 2*3600e9, "")
	fs.Duration("d", 90e9, "")
	fs.Bool("e", false, "")
	fs.String("f", "auto", "")
	fs.Int("g", 0, "")
	want := map[string]string{"a": "", "b": "30m", "c": "2h", "d": "1m30s", "e": "", "f": "auto", "g": ""}
	for name, w := range want {
		if got := defaultText(fs.Lookup(name)); got != w {
			t.Errorf("defaultText(%s) = %q, want %q", name, got, w)
		}
	}
}
