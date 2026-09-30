package main

import (
	"context"
	"flag"
	"time"
)

type healthcheckOptions struct {
	clientOptions
	ready bool
	wait  time.Duration
}

func healthcheckCmd() *command {
	return &command{
		name:    "healthcheck",
		summary: "Check that the server is alive, or ready with --ready",
		help: "Exits 0 when the server answers on its admin socket (with --ready: when it is also ready to serve) and " +
			"1 otherwise. It never exits with another code, not even for a usage error, because Docker's HEALTHCHECK " +
			"reserves exit code 2.",
		config:    true,
		zeroOrOne: true,
		setup: leaf(func(fs *flag.FlagSet, o *healthcheckOptions) {
			fs.BoolVar(&o.ready, "ready", false, "check readiness (database, certificate, media, signaling) instead of liveness")
			fs.DurationVar(&o.wait, "wait", 0, "keep trying for up to `DUR`")
			o.register(fs)
		}, runHealthcheck),
	}
}

// runHealthcheck asks the admin socket's /v1/health or /v1/ready (04 §12.2). A later slice fills it in.
func runHealthcheck(context.Context, *invocation, healthcheckOptions, []string) error {
	return errNotImplemented
}
