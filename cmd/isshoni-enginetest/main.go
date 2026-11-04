// Command isshoni-enginetest drives the Windows media engine (isshoni-media.dll) through its Go binding
// (internal/media/native), so it tests the binding too (docs/m2/04-windows-engine.md §14, "04" below). It is not part
// of any installer: the windows-native job puts it into the isshoni-native-tools zip for the owner's PC and the
// hwcheck suite (06 §5.1).
//
//	isshoni-enginetest [-dll DIR] [-v] [-json] <command> [flags]
//
// Global flags: -dll is the folder of isshoni-media.dll (default: the executable's folder; the FFmpeg DLLs are loaded
// from the same folder), -v logs the engine at debug level to stderr, -json prints the command's result as JSON.
// Exit codes: 0 pass, 1 an assertion failed, 2 environment (unsupported OS, no encoder of the asked kind, no GPU, the
// DLL doesn't load), 64 usage.
//
// At startup, before the DLL is loaded, the process default of 04 §13.3 layer 2 applies
// (SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32), SetDllDirectoryW("")), and the process becomes per-monitor
// DPI aware (v2), which Desktop Duplication needs (04 §7.4).
//
// README WA44 wrote the tool: the flags, the dispatch, probe, capture and bench. Each later command is one file that
// registers itself in init() (pick.go README WA50, openh264.go WA51, sources.go WA60, sessions.go WA61,
// protect.go WA70, soak.go and avsync.go WA76).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// The exit codes of 04 §14 (README §4: isshoni-enginetest's are 0, 1, 2, 64).
const (
	exitPass  = 0
	exitFail  = 1
	exitEnv   = 2
	exitUsage = 64
)

// command is one subcommand. run gets the command's own arguments (after its name) and returns the exit code.
type command struct {
	name    string
	summary string // one line for the usage text
	run     func(ctx context.Context, g *globals, args []string) int
}

var commands = map[string]command{}

// register adds a subcommand; each command's file calls it from init().
func register(c command) {
	if _, dup := commands[c.name]; dup {
		panic("isshoni-enginetest: command " + c.name + " registered twice")
	}
	commands[c.name] = c
}

// globals are the global flags and the process's streams.
type globals struct {
	dll     string
	verbose bool
	json    bool
	stdout  io.Writer
	stderr  io.Writer
	// open loads the engine (tests replace it).
	open func(native.Options) (*native.Engine, error)
}

func main() {
	// 04 §13.3 layer 2 and §7.4: before anything else, the DLL above all.
	if err := processSetup(); err != nil {
		fmt.Fprintln(os.Stderr, "isshoni-enginetest: process setup:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses the global flags and dispatches.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	g := &globals{stdout: stdout, stderr: stderr, open: native.Open}
	fs := flag.NewFlagSet("isshoni-enginetest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&g.dll, "dll", "", "`DIR` of isshoni-media.dll (default: the executable's folder)")
	fs.BoolVar(&g.verbose, "v", false, "log the engine at debug level to stderr")
	fs.BoolVar(&g.json, "json", false, "print the result as JSON")
	fs.Usage = func() { usage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitPass
		}
		return exitUsage
	}
	if fs.NArg() == 0 {
		usage(stderr, fs)
		return exitUsage
	}
	c, ok := commands[fs.Arg(0)]
	if !ok {
		fmt.Fprintf(stderr, "isshoni-enginetest: unknown command %q\n", fs.Arg(0))
		usage(stderr, fs)
		return exitUsage
	}
	return c.run(ctx, g, fs.Args()[1:])
}

func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "usage: isshoni-enginetest [-dll DIR] [-v] [-json] <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-10s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(w, "\nglobal flags:")
	fs.PrintDefaults()
	fmt.Fprintln(w, "\nexit codes: 0 pass, 1 an assertion failed, 2 environment, 64 usage")
}

// openEngine loads the engine with the global flags. noAudio is IM_INIT_NO_AUDIO (video-only runs).
func (g *globals) openEngine(noAudio bool) (*native.Engine, int, error) {
	level := slog.LevelWarn
	if g.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(g.stderr, &slog.HandlerOptions{Level: level}))
	e, err := g.open(native.Options{Dir: g.dll, Logger: logger, LogLevel: level, NoAudio: noAudio})
	if err != nil {
		return nil, exitEnv, err
	}
	return e, exitPass, nil
}

// envCode is the exit code for a failed engine call: environment problems (no encoder of the asked kind, an OS or a
// dependency the PC lacks, a source that is gone) are 2, anything else 1.
func envCode(err error) int {
	var me *media.Error
	if errors.As(err, &me) {
		switch me.Code {
		case media.CodeUnsupportedOS, media.CodeNoEncoder, media.CodeDependency, media.CodeSourceGone,
			media.CodePermissionDenied, media.CodeCancelled:
			return exitEnv
		}
	}
	return exitFail
}

// newFlags is a subcommand's flag set: errors go to stderr and end the command with exitUsage.
func newFlags(g *globals, name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	fs.Usage = func() {
		fmt.Fprintf(g.stderr, "usage: isshoni-enginetest %s %s\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses a subcommand's flags; ok false means the command returns code.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitPass, false
		}
		return exitUsage, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected arguments %s\n", fs.Name(), strings.Join(fs.Args(), " "))
		return exitUsage, false
	}
	return exitPass, true
}
