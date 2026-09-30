package main

import (
	"context"
	"flag"
	"time"
)

type setupURLOptions struct {
	clientOptions
	qr, noQR bool
	wait     time.Duration
	json     bool
}

func setupURLCmd() *command {
	return &command{
		name:    "setup-url",
		summary: "Print a one-time link that creates the admin account",
		help: "Mints a one-time setup link (valid 24 hours, works once) and prints it, with a QR code on a terminal. " +
			"Running it again makes a new link and cancels the previous one. It works only while no admin account " +
			"exists (exit 7 after that: use 'isshoni admin users reset-password NAME'). Exit 4 when the server is " +
			"not running or the admin socket is not accessible.",
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *setupURLOptions) {
			fs.BoolVar(&o.qr, "qr", false, "print the QR code even when stdout is not a terminal")
			fs.BoolVar(&o.noQR, "no-qr", false, "never print the QR code")
			fs.DurationVar(&o.wait, "wait", 0, "first wait up to `DUR` for the server to be ready (Docker and manual use)")
			fs.BoolVar(&o.json, "json", false, "print {url, expiresAt, tlsReady} as JSON")
			o.register(fs)
		}, runSetupURL),
	}
}

// runSetupURL asks the server for a setup link through the admin socket (04 §12.5). A later slice fills it in.
func runSetupURL(_ context.Context, _ *invocation, o setupURLOptions, _ []string) error {
	if o.qr && o.noQR {
		return usageErrorf("--qr and --no-qr can't be combined")
	}
	return errNotImplemented
}
