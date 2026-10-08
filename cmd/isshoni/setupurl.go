package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/MoonWX/isshoni/internal/server/ops"
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

// How setup-url --wait behaves while it waits.
const (
	setupRetryEvery  = 500 * time.Millisecond // between two tries while no server answers yet
	setupNoticeAfter = 2 * time.Second        // of silence, before it says that it is waiting
)

// runSetupURL asks the server for a setup link through the admin socket and prints it (04 §3.3, §12.5).
func runSetupURL(ctx context.Context, inv *invocation, o setupURLOptions, _ []string) error {
	if o.qr && o.noQR {
		return usageErrorf("--qr and --no-qr can't be combined")
	}
	if o.wait < 0 {
		return usageErrorf("--wait %s: want a duration of zero or more", o.wait)
	}
	c := o.admin(inv)
	res, err := setupURLWait(ctx, inv, c, o.wait)
	if err != nil {
		return adminFailure(inv, o.clientOptions, c, err)
	}

	const certNote = "The certificate isn't ready yet. The link works once it is; check with `isshoni doctor`."
	if o.json {
		// One JSON document on stdout; what is for people goes to stderr (04 §3.1).
		if err := printJSON(inv.stdout, res); err != nil {
			return err
		}
		if !res.TLSReady {
			_, _ = fmt.Fprintln(inv.stderr, certNote)
		}
		if o.qr {
			if code, err := qrText(res.URL); err == nil {
				_, _ = fmt.Fprint(inv.stderr, code)
			}
		}
		return nil
	}
	out := linkOutput{
		heading: fmt.Sprintf("Open this link in your browser to create the admin account (valid %s, works once):",
			validFor(time.Time(res.ExpiresAt))),
		url: res.URL,
		qr:  o.qr || (!o.noQR && inv.terminal(inv.stdout)),
	}
	if !res.TLSReady {
		out.notes = append(out.notes, certNote)
	}
	out.notes = append(out.notes, "Running `isshoni setup-url` again makes a new link and cancels this one.")
	return printLink(inv, out)
}

// setupURLWait asks for the setup link. With wait > 0 it keeps trying while no server listens yet (a container that
// has just started), and the server itself then waits for readiness before it mints the link; both together take
// at most wait. Without it there is one try.
func setupURLWait(ctx context.Context, inv *invocation, c *ops.AdminClient, wait time.Duration) (ops.AdminSetupURL, error) {
	type result struct {
		res ops.AdminSetupURL
		err error
	}
	start := time.Now()
	deadline := start.Add(wait)
	// A wait that takes a while says so once, on stderr; one that is over at once stays quiet.
	notice := time.NewTimer(setupNoticeAfter)
	defer notice.Stop()
	announced := wait <= 0
	announce := func() {
		if !announced {
			announced = true
			_, _ = fmt.Fprintf(inv.stderr, "Waiting for the server to be ready (up to %s)…\n", shortDuration(wait))
		}
	}
	for {
		left := max(time.Until(deadline), 0)
		callCtx, cancel := context.WithTimeout(ctx, left+adminCallTimeout)
		done := make(chan result, 1)
		go func() {
			res, err := c.SetupURL(callCtx, left)
			done <- result{res, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-notice.C:
			announce()
			r = <-done
		}
		cancel()
		if r.err == nil || !errors.Is(r.err, ops.ErrAdminNotRunning) || time.Until(deadline) <= 0 {
			return r.res, r.err
		}
		if time.Since(start) >= setupNoticeAfter {
			announce()
		}
		if !sleep(ctx, min(setupRetryEvery, time.Until(deadline))) {
			return r.res, ctx.Err()
		}
	}
}
