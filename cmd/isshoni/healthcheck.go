package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/server/ops"
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

// healthPollEvery is how often healthcheck --wait asks again (04 §11.1).
const healthPollEvery = time.Second

// runHealthcheck asks the admin socket's /v1/health, or /v1/ready with --ready (04 §11.1, §12.2). It asks the
// socket and not HTTPS: inside a container 127.0.0.1 would fail certificate verification, and the image has no
// curl. Every failure exits 1 (the command is zeroOrOne).
func runHealthcheck(ctx context.Context, inv *invocation, o healthcheckOptions, _ []string) error {
	if o.wait < 0 {
		return usageErrorf("--wait %s: want a duration of zero or more", o.wait)
	}
	c := o.admin(inv)
	deadline := time.Now().Add(o.wait)
	for {
		err := healthOnce(ctx, c, o.ready)
		if err == nil {
			word := ops.StatusOK
			if o.ready {
				word = ops.StatusReady
			}
			_, err := fmt.Fprintln(inv.stdout, word)
			return err
		}
		// Waiting does not help against a refusal, and not after an interrupt either. It does help while no server
		// answers, also when one hung up on a caller it would serve: it is on its way down, or up. The last try is
		// at the end of the wait.
		left := time.Until(deadline)
		giveUp := errors.Is(err, ops.ErrAdminPermission) || ctx.Err() != nil || left <= 0
		if giveUp || !sleep(ctx, min(healthPollEvery, left)) {
			return adminFailure(inv, o.clientOptions, c, err)
		}
	}
}

// healthOnce asks once and returns nil when the server is alive (ready, with ready).
func healthOnce(ctx context.Context, c *ops.AdminClient, ready bool) error {
	ctx, cancel := context.WithTimeout(ctx, healthCallTimeout)
	defer cancel()
	if !ready {
		h, err := c.Health(ctx)
		switch {
		case err != nil:
			return err
		case h.Status != ops.StatusOK:
			return errors.New(healthText(h))
		}
		return nil
	}
	h, err := c.Ready(ctx)
	switch {
	case err != nil:
		return err
	case h.Status != ops.StatusReady:
		return errors.New(healthText(h))
	}
	return nil
}

// healthText says why a health document is not the good one: "not ready: tls: waiting: obtaining certificate for
// 203.0.113.7".
func healthText(h ops.AdminHealth) string {
	if h.Status == ops.StatusShuttingDown {
		return "the server is shutting down"
	}
	var failing []string
	for name, state := range h.Checks {
		if state != ops.StatusOK { // a passing check shows as "ok"
			failing = append(failing, name+": "+state)
		}
	}
	slices.Sort(failing)
	text := strings.ReplaceAll(h.Status, "_", " ")
	if len(failing) > 0 {
		text += ": " + strings.Join(failing, "; ")
	}
	return text
}
