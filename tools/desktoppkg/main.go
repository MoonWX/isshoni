// Command desktoppkg holds the packaging steps of the isshoni Windows app that are not CMake, NSIS or a workflow
// (docs/m2/06-windows-release.md, "06" below). It runs on the Mac, on Linux CI and on Windows:
//
//	go -C tools run ./desktoppkg [-root DIR] <command> [flags]
//
// -root is the repository root; by default the nearest directory above the working directory whose go.mod is the
// module github.com/MoonWX/isshoni (so `go -C tools run` finds it too).
//
// # Commands
//
//	ffmpeg-inputs [-check]          print lock.json's inputsSha256 for the current native/ffmpeg files (06 §4.1);
//	                                -check fails when native/ffmpeg/lock.json does not match them (stale)
//	ffmpeg-fetch [-out DIR] [-zip FILE] [-source] [-base URL]
//	                                download the locked FFmpeg build from the deps release, verify it against
//	                                lock.json and unpack it; prints the folder to pass as IM_FFMPEG_ROOT (06 §4.5)
//	dll-check -dll FILE -version V -winver W [-ffmpeg DIR] [-load-config]
//	                                the checks of isshoni-media.dll (exports, imports, VERSIONINFO, load config) and
//	                                of the FFmpeg DLLs, from the PE images themselves (06 §5.1)
//	winver <semver>                 the four-part Windows version of a SemVer (06 §5.3; slice WA67)
//	stage [-release]                copy the install tree into dist-desktop/stage/ (06 §6.2; slices WA67, WA78)
//	nsis                            the defines passed to makensis (06 §6.2; slice WA67)
//	check                           the stage and installer source rules (06 §6.10, §9.3; slice WA67)
//	manifest -version V -installer FILE [-date T] [-out FILE] [-releases URL]
//	                                write the update manifest isshoni-desktop-update.json from the installer, the
//	                                version and deploy/desktop/min-version, deploy/keys/update_revoked (06 §8.2);
//	                                -releases only for the updater e2e's ci manifests (06 §8.11)
//
// A command whose slice has not landed yet fails with "not implemented yet".
//
// Exit status: 0 on success, 1 when a command fails, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/mod/modfile"
)

// modulePath identifies the repository root: the directory whose go.mod declares it.
const modulePath = "github.com/MoonWX/isshoni"

// errUsage marks an error that is the caller's mistake; run prints the usage and exits 2.
var errUsage = errors.New("usage error")

// errNotImplemented is returned by the commands whose slices have not landed yet.
var errNotImplemented = errors.New("not implemented yet")

// env is what a command runs with.
type env struct {
	root   string // repository root
	stdout io.Writer
	stderr io.Writer
	client *http.Client                                     // ffmpeg-fetch's HTTP client
	sleep  func(ctx context.Context, d time.Duration) error // waits between download attempts
}

// command is one subcommand.
type command struct {
	name     string
	synopsis string
	run      func(ctx context.Context, e *env, args []string) error
}

// commands is the whole command set of 06 §6.1 ("winver, stage, nsis defines, check, manifest, ffmpeg-inputs,
// ffmpeg-fetch, dll-check"). Each later slice fills its own command in its own file.
var commands = []command{
	{"ffmpeg-inputs", "[-check]", runFFmpegInputs},
	{"ffmpeg-fetch", "[-out DIR] [-zip FILE] [-source] [-base URL]", runFFmpegFetch},
	{"dll-check", "-dll FILE -version V -winver W [-ffmpeg DIR] [-load-config]", runDLLCheck},
	{"winver", "<semver> (slice WA67)", runWinver},
	{"stage", "[-release] (slices WA67, WA78)", runStage},
	{"nsis", "(slice WA67)", runNSIS},
	{"check", "(slice WA67)", runCheck},
	{"manifest", "-version V -installer FILE [-date T] [-out FILE] [-releases URL]", runManifest},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("desktoppkg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "repository root (default: found from the working directory)")
	fs.Usage = func() { usage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	var cmd *command
	for i := range commands {
		if commands[i].name == name {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Fprintf(stderr, "desktoppkg: unknown command %q\n", name)
		fs.Usage()
		return 2
	}

	e := &env{stdout: stdout, stderr: stderr, client: newHTTPClient(), sleep: sleepContext}
	dir, err := findRoot(*root)
	if err != nil {
		fmt.Fprintf(stderr, "desktoppkg %s: %v\n", name, err)
		return 1
	}
	e.root = dir

	if err := cmd.run(ctx, e, fs.Args()[1:]); err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			return 0
		case errors.Is(err, errUsage):
			fmt.Fprintf(stderr, "desktoppkg %s: %v\n", name, err)
			return 2
		default:
			fmt.Fprintf(stderr, "desktoppkg %s: %v\n", name, err)
			return 1
		}
	}
	return 0
}

func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "usage: desktoppkg [-root DIR] <command> [flags]")
	fs.PrintDefaults()
	fmt.Fprintln(w, "commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.name, c.synopsis)
	}
}

// findRoot returns dir when it is set, else the nearest directory at or above the working directory whose go.mod
// declares modulePath.
func findRoot(dir string) (string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; {
		data, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil && modfile.ModulePath(data) == modulePath {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod of %s at or above %s: pass -root", modulePath, wd)
		}
		d = parent
	}
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notImplemented is the error of a command whose slice has not landed yet.
func notImplemented(slice string) error {
	return fmt.Errorf("%w (slice %s)", errNotImplemented, slice)
}
