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
			"A configuration or data problem that a restart can't fix exits 78.",
		config:          true,
		listConfigFlags: true,
		setup: func(*flag.FlagSet) runFunc {
			return runServe
		},
	}
}

// runServe runs the server until SIGTERM or SIGINT (04 §6.1–§6.5). Step 1 of the startup sequence is here: a config
// with errors exits 78 with every problem printed, and the logger is built (04 §10); server.New and Run do the
// rest, and serveExit maps how the server ended to the exit code. The config's warnings are not printed here:
// the server logs them, so they reach the journal in the log's format.
func runServe(ctx context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	// The level is a variable: `isshoni admin log-level` changes it at runtime through the admin socket, once the
	// wiring hands it to ops.NewLogLevel (README S54 starts the admin socket in the server).
	level := new(slog.LevelVar)
	if l, ok := ops.ParseLogLevel(inv.cfg.Log.Level); ok { // validated by config; anything else stays at info
		level.Set(l)
	}
	log := logx.New(logx.Options{Level: level, Format: inv.cfg.Log.Format, Out: inv.stderr})

	srv, err := server.New(inv.cfg, log, server.Deps{})
	if err != nil {
		return serveExit(inv, err)
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

	return serveExit(inv, srv.Run(ctx))
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
//   - a restart the server asked for after a restore or rotate-secrets: 75, "start it again". README S65 replaces
//     this by the re-exec of 04 §6.5 where the platform has one;
//   - anything else (a busy port, a data directory another isshoni holds, a listener that failed): 1.
func serveExit(inv *invocation, err error) error {
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
		return exitWith(exitRestart, errors.New("the server stopped for a restart (after a restore or rotate-secrets): start it again"))
	default:
		return exitWith(exitRuntime, err)
	}
}
