package main

import (
	"context"
	"flag"
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

// runServe loads the config, builds the logger (logx.New), runs internal/server until ctx ends and re-execs after a
// restore or rotate-secrets (04 §6.1–6.5). Step 1 is here: a config with errors exits 78 with every problem printed.
// Later slices of the M1 plan fill in the rest (and log the warnings through the logger).
func runServe(_ context.Context, inv *invocation, _ []string) error {
	if err := configInvalid(inv); err != nil {
		return err
	}
	printProblems(inv.stderr, inv.cfg.Warnings())
	return errNotImplemented
}
