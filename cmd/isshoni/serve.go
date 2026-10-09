package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
			"the server restarts in place, with the same process ID. A reload (SIGHUP) is not supported yet: restart " +
			"the server to apply a changed configuration.",
		config:          true,
		listConfigFlags: true,
		setup: func(*flag.FlagSet) runFunc {
			return runServe
		},
	}
}

// reloadNotSupported is what serve logs for a SIGHUP.
const reloadNotSupported = "reload is not supported yet; restart to apply changes"

// runServe runs the server until SIGTERM or SIGINT (04 §6.1–§6.5). Step 1 of the startup sequence is here: a config
// with errors exits 78 with every problem printed, and the logger is built (04 §10); server.New and Run do the
// rest, and serveExit maps how the server ended to the exit code, or to the re-exec that restarts it in place. The
// config's warnings are not printed here: the server logs them, so they reach the journal in the log's format.
func runServe(ctx context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	// The level is a variable: `isshoni admin log-level` changes it at runtime through the admin socket. The server
	// gets the variable as Deps.LogLevel and hands it to ops.NewLogLevel.
	level := new(slog.LevelVar)
	if l, ok := ops.ParseLogLevel(inv.cfg.Log.Level); ok { // validated by config; anything else stays at info
		level.Set(l)
	}
	log := logx.New(logx.Options{Level: level, Format: inv.cfg.Log.Format, Out: inv.stderr})

	// SIGHUP is the reload of 04 §6.5 (06's unit: ExecReload=/bin/kill -HUP $MAINPID), which comes with the server
	// hardening (README S90). Until then the signal must not do what it does to a process that doesn't handle it:
	// end the server, which systemd would count as a clean stop. serve takes it, says so, and goes on. It is
	// watched from here on, before the server starts, so a reload during a slow start is harmless too.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	defer logSignals(hup, func() { log.Info(reloadNotSupported) })()

	srv, err := server.New(inv.cfg, log, server.Deps{LogLevel: level})
	if err != nil {
		return serveExit(ctx, inv, log, err)
	}

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

	return serveExit(ctx, inv, log, srv.Run(ctx))
}

// logSignals calls say for every signal on sigs until the returned function is called, which waits for the
// watching goroutine. serve uses it for SIGHUP.
func logSignals(sigs <-chan os.Signal, say func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-sigs:
				say()
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

// stopOnSignal returns a context that ends with the first signal on sigs (or with ctx), and calls again for a
// second signal: the operator does not want to wait for the graceful shutdown. stop ends the watching and waits
// for its goroutine.
//
// SIGINT arrives twice, here and through main's context, which ends ctx by itself; the watcher still takes its
// copy from sigs first, so that the first Ctrl-C never counts as the second.
func stopOnSignal(ctx context.Context, sigs <-chan os.Signal, again func()) (context.Context, func()) {
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
	return ctx, func() {
		close(done)
		wg.Wait()
		cancel()
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
// ctx is the one that SIGTERM and SIGINT end.
func serveExit(ctx context.Context, inv *invocation, log *slog.Logger, err error) error {
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
		return restartInPlace(ctx, inv, log)
	default:
		return exitWith(exitRuntime, err)
	}
}

// restartInPlace is what serve does once the server has shut down for a restart (04 §6.5): it replaces the process
// by a new run of the same command line (inv.reexec, server.Reexec in the real program). The PID stays, so systemd
// and Docker see no exit, and the new process finds what the restart was for: the restored files, the rotated
// keys, or the public address that the old one had started without.
//
// It returns only when there is no new process:
//   - the operator's stop came while the server was already shutting down for the restart: the server is stopped,
//     which is what a stop asks for, so serve exits 0 and nothing starts;
//   - the platform has no re-exec (Windows), or the exec failed: exit 75, "restart required". systemd
//     (Restart=on-failure) and Docker (restart: unless-stopped) start a new process on it.
func restartInPlace(ctx context.Context, inv *invocation, log *slog.Logger) error {
	const startAgain = "the server stopped for a restart (after a restore or rotate-secrets, or because it found the " +
		"public address it was waiting for)"
	if ctx.Err() != nil {
		log.Info("stopped instead of restarting: a stop signal came during the shutdown")
		return nil //nolint:nilerr // the stop is what was asked for, not a failure
	}
	if inv.reexec == nil {
		return exitWith(exitRestart, errors.New(startAgain+": start it again"))
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
