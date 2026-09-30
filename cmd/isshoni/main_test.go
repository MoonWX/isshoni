package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

var update = flag.Bool("update", false, "rewrite the expected output in testdata/script")

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"isshoni": main})
}

// TestScripts runs testdata/script/*.txtar against the real binary (this test binary, re-executed as isshoni).
func TestScripts(t *testing.T) {
	t.Parallel()
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		Cmds:                map[string]func(*testscript.TestScript, bool, []string){"exitcode": cmdExitCode},
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		UpdateScripts:       *update,
		Setup: func(env *testscript.Env) error {
			// Under -race a process that exits 0 first sleeps atexit_sleep_ms (1 s by default) to flush reports.
			env.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			return nil
		},
	})
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

// runCLI runs the CLI in-process.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), &invocation{stdout: &out, stderr: &errOut}, args)
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

// validArgs are arguments each leaf accepts, so it gets past parsing.
var validArgs = map[string][]string{
	"admin restore":              {"backup.tar.gz"},
	"admin users reset-password": {"alice"},
	"admin users set-role":       {"alice", "admin"},
	"admin users disable":        {"alice"},
	"admin users enable":         {"alice"},
	"admin log-level":            {"debug"},
	"config init":                {"--path", "isshoni.toml"},
}

// Each leaf a later slice fills in says so and exits 1.
func TestNotImplemented(t *testing.T) {
	for _, p := range leaves(root(), nil) {
		name := strings.Join(p, " ")
		if name == "version" || name == "help" {
			continue
		}
		code, stdout, stderr := runCLI(t, append(p, validArgs[name]...)...)
		want := "isshoni " + name + ": not implemented in this build yet\n"
		if code != exitRuntime || stdout != "" || stderr != want {
			t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit 1 and %q", name, code, stdout, stderr, want)
		}
	}
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
		{[]string{"version", "--bogus"}, exitUsage, "isshoni version: unknown flag --bogus"},
		{[]string{"version", "--short", "--json"}, exitUsage, "isshoni version: --short and --json can't be combined"},
		{[]string{"version", "extra"}, exitUsage, `isshoni version: unexpected argument "extra"`},
		{[]string{"version", "--short=maybe"}, exitUsage, `isshoni version: invalid boolean value "maybe" for --short: parse error`},
		{[]string{"serve", "--tls.mode"}, exitUsage, "isshoni serve: unknown flag --tls.mode"},
		{[]string{"admin"}, exitUsage, "isshoni admin: missing command"},
		{[]string{"admin", "--json"}, exitUsage, "isshoni admin: unknown flag --json"},
		{[]string{"admin", "users", "lst"}, exitUsage, `isshoni admin users: unknown command "lst" (did you mean "list"?)`},
		{[]string{"admin", "restore"}, exitUsage, "isshoni admin restore: missing argument PATH|-"},
		{[]string{"admin", "restore", "a.tar.gz", "--yes"}, exitUsage, "isshoni admin restore: flag --yes after an argument: flags go before arguments"},
		{[]string{"admin", "restore", "a.tar.gz", "b.tar.gz"}, exitUsage, `isshoni admin restore: unexpected argument "b.tar.gz"`},
		{[]string{"admin", "users", "set-role", "alice"}, exitUsage, "isshoni admin users set-role: missing arguments: want NAME admin|user"},
		{[]string{"admin", "users", "set-role", "alice", "root"}, exitUsage, `isshoni admin users set-role: role "root": want admin or user`},
		{[]string{"admin", "log-level", "loud"}, exitUsage, `isshoni admin log-level: log level "loud": want debug, info, warn or error`},
		{[]string{"admin", "log-level", "--for"}, exitUsage, "isshoni admin log-level: flag needs an argument: --for"},
		{[]string{"admin", "log-level", "--for", "soon", "debug"}, exitUsage, `isshoni admin log-level: invalid value "soon" for flag --for: parse error`},
		{[]string{"admin", "invite", "create", "--uses", "ten"}, exitUsage, `isshoni admin invite create: invalid value "ten" for flag --uses: parse error`},
		{[]string{"setup-url", "--qr", "--no-qr"}, exitUsage, "isshoni setup-url: --qr and --no-qr can't be combined"},
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
	if code := run(ctx, &invocation{stdout: &out, stderr: &errOut}, []string{"serve"}); code != exitInterrupted {
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
