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
	"sync"
	"syscall"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

func serveCmd() *command {
	return &command{
		name:    "serve",
		summary: "Run the server",
		help: "Runs the server in the foreground until it receives SIGTERM or SIGINT, then shuts down gracefully " +
			"(clients see \"Server restarting…\" and reconnect on their own). systemd and Docker start it this way. " +
			"A configuration or data problem that a restart can't fix exits 78. After a restore or rotate-secrets " +
			"the server restarts in place, with the same process ID. SIGHUP (systemctl reload isshoni) reads the " +
			"manual TLS certificate files and log.level again; every other change takes a restart, and the log " +
			"names the keys that wait for one.",
		config:          true,
		listConfigFlags: true,
		setup: func(*flag.FlagSet) runFunc {
			return runServe
		},
	}
}

// runServe runs the server until SIGTERM or SIGINT (04 §6.1–§6.5). Step 1 of the startup sequence is here: a config
// with errors exits 78 with every problem printed, and the logger is built (04 §10); server.New and Run do the
// rest, and serveExit maps how the server ended to the exit code, or to the re-exec that restarts it in place. The
// config's warnings are not printed here: the server logs them, so they reach the journal in the log's format.
func runServe(ctx context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	// The level is a variable: `isshoni admin log-level` changes it at runtime through the admin socket, and a
	// reload sets the configured one. The server gets the variable as Deps.LogLevel and hands it to ops.NewLogLevel.
	level := new(slog.LevelVar)
	if l, ok := ops.ParseLogLevel(inv.cfg.Log.Level); ok { // validated by config; anything else stays at info
		level.Set(l)
	}
	log := logx.New(logx.Options{Level: level, Format: inv.cfg.Log.Format, Out: inv.stderr})
	// What the libraries write to the standard log package (certmagic does) goes into the same log.
	defer routeStdLog(log)()

	// SIGHUP is the reload of 04 §6.5 (06's unit: ExecReload=/bin/kill -HUP $MAINPID). The signal must never do
	// what it does to a process that doesn't handle it: end the server, which systemd would count as a clean stop.
	// It is taken from here on, before the server is built, and a reload that comes during a slow start waits for
	// the start (server.Reload).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	srv, err := server.New(inv.cfg, log, server.Deps{LogLevel: level})
	if err != nil {
		// No stop signal is taken yet: SIGTERM still ends the process by itself, so there is nothing to release.
		return serveExit(ctx, inv, log, err, nil)
	}
	defer onSignals(hup, func() { reload(ctx, inv, log, srv) })()

	// SIGTERM joins SIGINT (main's context): systemd and Docker stop the server with it. The first signal starts
	// the graceful shutdown; a second one exits at once with code 1 (04 §6.4).
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigs)
	ctx, stop := stopOnSignal(ctx, sigs, func() {
		_, _ = fmt.Fprintln(inv.stderr, "isshoni serve: second signal, exiting at once")
		os.Exit(exitRuntime)
	})
	defer stop()

	// Before a re-exec the stop signals go back to their default, so that one that comes on the way to the exec
	// ends the process, as the operator asked, instead of a context that nobody reads any more (restartInPlace).
	// signal.Stop hands a signal that is already under way to sigs, and stop waits for the watcher to have seen it.
	// SIGINT is main's too: the Reset takes it from main's context as well. One that arrives in the moment between
	// the two calls is caught by main's context alone, and may be missed; a terminal's operator presses Ctrl-C again.
	release := func() bool {
		signal.Stop(sigs)
		signal.Reset(os.Interrupt)
		return stop()
	}
	return serveExit(ctx, inv, log, srv.Run(ctx), release)
}

// onSignals calls fn for every signal on sigs, one call after the other, until the returned function is called,
// which waits for the watching goroutine (and a call of fn that is under way). serve uses it for SIGHUP.
func onSignals(sigs <-chan os.Signal, fn func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-sigs:
				fn()
			case <-done:
				return
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// reload is serve's answer to SIGHUP (04 §6.5): it loads the config again, from serve's own flags and the
// environment it started with and the file as it is now, so that a flag or an ISSHONI_* variable still wins over
// the file (04 §4.1), and hands it to the server, which reads the manual TLS files again, applies log.level and
// logs the keys that changed and take a restart. A config that can't be loaded at all is reported, and only the
// certificate files are read again.
func reload(ctx context.Context, inv *invocation, log *slog.Logger, srv *server.Server) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg, err := config.Load(fs, inv.configArgs, inv.environ)
	var ve *config.ValidationError
	if err != nil && !errors.As(err, &ve) {
		// The same arguments parsed at the start: this is no usage error of the operator's.
		log.Error("reload: the config could not be loaded again", logx.Err(err))
		cfg = nil
	}
	// The server logs what it did; an error is the TLS manager's, which has logged it with the fix.
	_ = srv.Reload(ctx, cfg)
}

// stopOnSignal returns a context that ends with the first signal on sigs (or with ctx), and calls again for a
// second signal: the operator does not want to wait for the graceful shutdown. stop ends the watching, waits for
// its goroutine, ends the context, and reports whether a stop came: the context had ended, or a signal still waits
// in sigs (the goroutine was told to end before it took it). Calling stop again changes nothing and reports the
// same.
//
// SIGINT arrives twice, here and through main's context, which ends ctx by itself; the watcher still takes its
// copy from sigs first, so that the first Ctrl-C never counts as the second.
func stopOnSignal(ctx context.Context, sigs <-chan os.Signal, again func()) (context.Context, func() (stopped bool)) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		select {
		case <-sigs:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sigs:
			again()
		case <-done:
		}
	})
	var (
		once    sync.Once
		stopped bool
	)
	return ctx, func() bool {
		once.Do(func() {
			close(done)
			wg.Wait()
			stopped = ctx.Err() != nil || len(sigs) > 0
			cancel()
		})
		return stopped
	}
}

// serveExit maps the result of server.New or Run to serve's exit (04 §6, §3.2):
//   - nil, and a stop that had to close something by force (the server logged the warning): 0;
//   - a refusal that a restart can't fix (server.NeedsOperator): 78, with the one message that says what to do.
//     The server returns it without logging it;
//   - a restart the server asked for, after a restore or rotate-secrets or because it found the public address it
//     had started without (04 §6.2): the re-exec of 04 §6.5 (restartInPlace), which does not come back; 75 where
//     there is none;
//   - anything else (a busy port, a data directory another isshoni holds, a listener that failed): 1.
//
// ctx is the one that SIGTERM and SIGINT end, and release gives those signals back to their default (nil when
// serve has not taken them); restartInPlace has the details.
func serveExit(ctx context.Context, inv *invocation, log *slog.Logger, err error, release func() (stopped bool)) error {
	var ve *config.ValidationError
	switch {
	case err == nil, errors.Is(err, server.ErrShutdownForced):
		return nil
	case errors.As(err, &ve): // values that server.New checks again; Load's own errors never get this far
		printProblems(inv.stderr, ve.Problems)
		return exitWith(exitConfig, nil)
	case server.NeedsOperator(err):
		return exitWith(exitConfig, err)
	case errors.Is(err, server.ErrRestartRequested):
		return restartInPlace(ctx, inv, log, release)
	default:
		return exitWith(exitRuntime, err)
	}
}

// restartInPlace is what serve does once the server has shut down for a restart (04 §6.5): it replaces the process
// by a new run of the same command line (inv.reexec, server.Reexec in the real program). The PID stays, so systemd
// and Docker see no exit, and the new process finds what the restart was for: the restored files, the rotated
// keys, or the public address that the old one had started without.
//
// A stop signal must not get lost on the way: serve's handling of it ends with the exec, and the new process takes
// a while to start (a migration, a certificate order), during which systemd would wait for it in vain and then
// kill it. So before the exec, release gives SIGTERM and SIGINT back to their default, which ends the process,
// and reports whether one came before; the new process keeps the default until it takes them itself.
//
// It returns only when there is no new process:
//   - the operator's stop came while the server was shutting down for the restart, or before the signals were
//     released: the server is stopped, which is what a stop asks for, so serve exits 0 and nothing starts;
//   - the platform has no re-exec (Windows), or the exec failed: exit 75, "restart required". systemd
//     (Restart=on-failure) and Docker (restart: unless-stopped) start a new process on it.
func restartInPlace(ctx context.Context, inv *invocation, log *slog.Logger, release func() (stopped bool)) error {
	const startAgain = "the server stopped for a restart (after a restore or rotate-secrets, or because it found the " +
		"public address it was waiting for)"
	const stopped = "stopped instead of restarting: a stop signal came during the shutdown"
	if ctx.Err() != nil {
		log.Info(stopped)
		return nil //nolint:nilerr // the stop is what was asked for, not a failure
	}
	if inv.reexec == nil {
		return exitWith(exitRestart, errors.New(startAgain+": start it again"))
	}
	if release != nil && release() { // the last look for a stop: one that comes later ends the process
		log.Info(stopped)
		return nil
	}
	log.Info("restarting in place")
	err := inv.reexec() // comes back only when it failed
	if errors.Is(err, server.ErrNoReexec) {
		return exitWith(exitRestart, errors.New(startAgain+": start it again"))
	}
	return exitWith(exitRestart, fmt.Errorf("%s and could not start itself again (%w): start it again", startAgain, err))
}

// reexecServe is the invocation's reexec in the real program: server.Reexec, which replaces the process.
//
// A handled signal goes back to its default at an exec, and SIGHUP's default ends the process. Between the exec
// and the moment the new process watches SIGHUP itself, a reload would therefore kill the server that is just
// restarting. An ignored signal stays ignored across an exec, so SIGHUP is ignored from here until runServe's
// signal.Notify in the new process takes it again.
func reexecServe() error {
	signal.Ignore(syscall.SIGHUP)
	return server.Reexec()
}
