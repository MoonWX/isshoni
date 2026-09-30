// Command isshoni is the isshoni server and its operator CLI (04 §3): serve, setup-url, doctor, healthcheck, admin,
// config and version.
//
// Exit codes (04 §3.2): 0 success, 1 runtime error, 2 usage error, 4 server not reachable on the admin socket,
// 5 doctor found a failure, 7 refused (a precondition is not met), 75 restart required on a platform without re-exec,
// 78 a configuration or data problem that a restart can't fix (EX_CONFIG: systemd stops retrying), 130 interrupted.
// healthcheck exits only 0 or 1, because Docker's HEALTHCHECK reserves 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
)

// Process exit codes (04 §3.2).
const (
	exitOK          = 0
	exitRuntime     = 1  // cannot bind a port, data-directory lock held, backup failed, unexpected server error
	exitUsage       = 2  // unknown command or flag, missing argument, destructive command without a TTY or --yes
	exitUnreachable = 4  // no server on the admin socket, or permission denied on it
	exitDoctorFail  = 5  // doctor: at least one fail (with --strict, also a warn)
	exitRefused     = 7  // precondition not met: setup_unavailable, backup newer than this binary, …
	exitRestart     = 75 // serve on a platform without re-exec (Windows): restart required after a restore
	exitConfig      = 78 // EX_CONFIG: invalid config, schema newer than the binary, corrupt secrets, no data volume
	exitInterrupted = 130
)

func main() {
	// SIGINT cancels ctx. Once it has, the default handling is back, so a second Ctrl-C ends the process at once.
	// serve adds its own SIGTERM and SIGHUP handling (04 §6.4, §6.5).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	context.AfterFunc(ctx, stop)
	code := run(ctx, &invocation{stdout: os.Stdout, stderr: os.Stderr}, os.Args[1:])
	stop()
	os.Exit(code)
}

// invocation carries the process's streams to the commands, so tests can run them in-process.
type invocation struct {
	stdout io.Writer
	stderr io.Writer
}

// runFunc runs a leaf command with its positional arguments, after its flags are parsed.
type runFunc func(ctx context.Context, inv *invocation, args []string) error

// A command is one node of the CLI tree: a group with subcommands, or a leaf with flags and a runFunc.
type command struct {
	name    string
	args    string // positional arguments for the usage line, e.g. "PATH|-"
	summary string // one line for the parent's command list
	help    string // paragraphs for "isshoni help CMD"; summary when empty

	// minArgs and maxArgs bound a leaf's positional arguments; maxArgs < 0 means no limit.
	minArgs, maxArgs int

	// config marks commands that read the server config (04 §3.1): they take --config PATH (env ISSHONI_CONFIG)
	// and one flag per config key. The config package registers those flags through config.Load (04 §4.7), which
	// also parses the command line; its help shows them as [config flags].
	config bool

	// zeroOrOne makes every failure, a usage error included, exit 1 (healthcheck, for Docker's HEALTHCHECK).
	zeroOrOne bool

	// setup registers a leaf's flags on fs and returns the function that runs it.
	setup func(fs *flag.FlagSet) runFunc

	subs   []*command
	parent *command
}

// leaf builds a leaf's setup from an options struct O: flags binds the flags on fs to the fields of o, and run gets
// the parsed options and the positional arguments.
func leaf[O any](flags func(fs *flag.FlagSet, o *O), run func(ctx context.Context, inv *invocation, o O, args []string) error) func(*flag.FlagSet) runFunc {
	return func(fs *flag.FlagSet) runFunc {
		var o O
		flags(fs, &o)
		return func(ctx context.Context, inv *invocation, args []string) error { return run(ctx, inv, o, args) }
	}
}

// clientOptions are the options of every command that talks to the server over its admin socket.
type clientOptions struct {
	socket string // --socket; "" means listen.admin_socket from the config
}

func (o *clientOptions) register(fs *flag.FlagSet) {
	fs.StringVar(&o.socket, "socket", "", "admin socket `PATH` (default: listen.admin_socket from the config, /run/isshoni/admin.sock)")
}

// errNotImplemented is returned by commands whose behavior later slices of the M1 plan add (docs/m1/README.md).
var errNotImplemented = errors.New("not implemented in this build yet")

// root returns the command tree.
func root() *command {
	c := &command{
		name:    "isshoni",
		summary: "Share screens with friends from your own server",
		help: "isshoni runs a self-hosted server for sharing screens (and system audio) with friends and watching " +
			"them together in the browser.",
		subs: []*command{
			serveCmd(), setupURLCmd(), doctorCmd(), healthcheckCmd(), adminCmd(), configCmd(), versionCmd(), helpCmd(),
		},
	}
	link(c)
	return c
}

func link(c *command) {
	for _, s := range c.subs {
		s.parent = c
		link(s)
	}
}

// path is the command as typed, e.g. "isshoni admin users list".
func (c *command) path() string {
	if c.parent == nil {
		return c.name
	}
	return c.parent.path() + " " + c.name
}

func (c *command) sub(name string) *command {
	for _, s := range c.subs {
		if s.name == name {
			return s
		}
	}
	return nil
}

// run executes the command line args (without the program name) and returns the exit code.
func run(ctx context.Context, inv *invocation, args []string) int {
	cmd, err := dispatch(ctx, inv, root(), args)
	code := exitCodeOf(ctx, err)
	if err != nil {
		report(inv, cmd, err)
	}
	if cmd.zeroOrOne && code != exitOK {
		code = exitRuntime
	}
	return code
}

// dispatch walks the command tree along args and runs the command it finds. It returns that command (for error
// reporting and exit codes) and its error. A request for help prints it and returns a nil error.
func dispatch(ctx context.Context, inv *invocation, cmd *command, args []string) (*command, error) {
	for {
		fs, runCmd := cmd.flagSet()
		if err := fs.Parse(args); err != nil {
			return cmd, parseError(cmd, inv, err)
		}
		args = fs.Args()
		if runCmd != nil { // a leaf
			if err := checkArgs(cmd, args); err != nil {
				return cmd, err
			}
			return cmd, runCmd(ctx, inv, args)
		}
		// A group: the root, or admin, admin users, …
		if f := fs.Lookup("version"); f != nil && f.Value.String() == "true" { // isshoni --version
			return cmd, printVersion(inv.stdout, buildInfo(), false, false)
		}
		if len(args) == 0 {
			return cmd, usageErrorf("missing command")
		}
		next := cmd.sub(args[0])
		if next == nil {
			return cmd, unknownCommand(cmd, args[0])
		}
		cmd, args = next, args[1:]
	}
}

// parseError turns a flag parsing error into a usage error, or prints the help for -h/--help.
func parseError(cmd *command, inv *invocation, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return writeHelp(inv.stdout, cmd, true)
	}
	return usageErrorf("%s", flagMessage(err))
}

// flagMessage rewrites the flag package's messages to the double-dash form the docs use: "flag provided but not
// defined: -bogus" becomes "unknown flag --bogus".
func flagMessage(err error) string {
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		return "unknown flag --" + name
	}
	if strings.HasPrefix(msg, "bad flag syntax: ") {
		return msg
	}
	return strings.NewReplacer(": -", ": --", "flag -", "flag --", "for -", "for --").Replace(msg)
}

func checkArgs(cmd *command, args []string) error {
	switch {
	case len(args) < cmd.minArgs:
		if cmd.minArgs == 1 {
			return usageErrorf("missing argument %s", cmd.args)
		}
		return usageErrorf("missing arguments: want %s", cmd.args)
	case cmd.maxArgs >= 0 && len(args) > cmd.maxArgs:
		extra := args[cmd.maxArgs:]
		for _, a := range extra {
			if len(a) > 1 && strings.HasPrefix(a, "-") {
				return usageErrorf("flag %s after an argument: flags go before arguments", a)
			}
		}
		return usageErrorf("unexpected argument %q", extra[0])
	}
	return nil
}

func unknownCommand(cmd *command, name string) error {
	if strings.HasPrefix(name, "-") { // after "--"
		return usageErrorf("unknown flag %s", name)
	}
	var names []string
	for _, s := range cmd.subs {
		names = append(names, s.name)
	}
	if s := suggest(name, names); s != "" {
		return usageErrorf("unknown command %q (did you mean %q?)", name, s)
	}
	return usageErrorf("unknown command %q", name)
}

// usageError is a command line the CLI can't run: exit 2, and the command's usage on stderr.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exitError makes a command exit with code. Its error is printed unless it is nil (the command already printed its
// own report, like doctor's).
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// exitWith returns an error that makes the process exit with code, e.g. exitWith(exitConfig, err) for a config
// error. A nil err exits silently.
func exitWith(code int, err error) error { return &exitError{code: code, err: err} }

func exitCodeOf(ctx context.Context, err error) int {
	var ue *usageError
	var ee *exitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue):
		return exitUsage
	case errors.As(err, &ee):
		return ee.code
	case ctx.Err() != nil:
		return exitInterrupted
	default:
		return exitRuntime
	}
}

// report prints err for cmd on stderr: a usage error with the command's usage, anything else on one line.
func report(inv *invocation, cmd *command, err error) {
	var ue *usageError
	if errors.As(err, &ue) {
		_, _ = fmt.Fprintf(inv.stderr, "%s: %s\n", cmd.path(), ue.msg)
		_ = writeHelp(inv.stderr, cmd, false)
		return
	}
	var ee *exitError
	if errors.As(err, &ee) && ee.err == nil {
		return
	}
	_, _ = fmt.Fprintf(inv.stderr, "%s: %v\n", cmd.path(), err)
}

// suggest returns the candidate closest to name within an edit distance of 2, or "".
func suggest(name string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
