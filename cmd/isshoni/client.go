package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

// What the client commands share: finding the admin socket, turning its errors into the messages and exit codes of
// 04 §3.2 and §12.1, and printing links, JSON documents and times.

// How long a call to the admin socket may take before the CLI gives up.
const (
	adminCallTimeout  = 30 * time.Second
	healthCallTimeout = 5 * time.Second // one healthcheck attempt: Docker's HEALTHCHECK must not hang
)

// admin returns the client for the admin socket: --socket, else listen.admin_socket from the config. A config with
// errors still gives a path (the default in place of a value that could not be used); adminFailure says so when
// nothing answers there.
func (o clientOptions) admin(inv *invocation) *ops.AdminClient {
	path := o.socket
	if path == "" {
		path = inv.cfg.Listen.AdminSocket
	}
	if inv.dialAdmin != nil {
		return inv.dialAdmin(path)
	}
	return ops.DialAdmin(path)
}

// adminFailure turns the error of an admin socket call into the command's error (04 §3.2, §12.1):
//   - no server on the socket, or one that went away before it answered: its message on stderr, exit 4;
//   - permission denied: "Permission denied on PATH: run it with sudo (sudo isshoni <command as typed>)", exit 4;
//   - an error answer of the server: its message and fix; exit 7 when the server refused because a precondition is
//     not met (setup_unavailable, last_admin, …), 1 otherwise;
//   - anything else (a timeout, an interrupt) unchanged.
//
// healthcheck prints the same messages and exits 1 (04 §3.2).
func adminFailure(inv *invocation, o clientOptions, c *ops.AdminClient, err error) error {
	var ae *ops.AdminError
	switch {
	case errors.Is(err, ops.ErrAdminPermission):
		_, _ = fmt.Fprintf(inv.stderr, "Permission denied on %s: run it with sudo (sudo isshoni %s)\n",
			c.Path(), strings.Join(quoteArgs(inv.args), " "))
		return exitWith(exitUnreachable, nil)
	case errors.Is(err, ops.ErrAdminNotRunning):
		_, _ = fmt.Fprintf(inv.stderr, "isshoni is not running (no server on %s)\n", c.Path())
		if inv.cfgErr != nil && o.socket == "" {
			// The path may be the default instead of the one the broken config meant.
			_, _ = fmt.Fprintln(inv.stderr, "The config has errors, so that path may not be the configured one: "+
				"run `isshoni config check`, or pass --socket PATH.")
		}
		return exitWith(exitUnreachable, nil)
	case errors.As(err, &ae):
		text := ae.Error()
		if ae.Fix != "" {
			text += "\n  fix: " + ae.Fix
		}
		return exitWith(refusalExit(ae.API.Code), errors.New(text))
	default:
		return err
	}
}

// refusalExit is the exit code for an error code of the server (04 §3.2, §12.2): 7 for "refused: a precondition is
// not met", 1 for everything else. The CLI branches on the code, never on the HTTP status.
func refusalExit(code string) int {
	switch code {
	case api.CodeSetupUnavailable, api.CodeLastAdmin, api.CodeRegistrationClosed, api.CodeLimitReached,
		api.CodeBackupNewer, api.CodeRestoreInProgress:
		return exitRefused
	default:
		return exitRuntime
	}
}

// quoteArgs returns args as a shell would take them back: an argument with anything but plain characters is put in
// single quotes.
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, needsQuoting) < 0 {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return out
}

// needsQuoting reports whether r is a character a shell may give a meaning to.
func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("-_./=:,@%+", r):
		return false
	default:
		return true
	}
}

// terminal reports whether w is a terminal: the QR code and the roomier layout are for people, not for pipes.
func (inv *invocation) terminal(w io.Writer) bool {
	if inv.isTTY != nil {
		return inv.isTTY(w)
	}
	s, ok := w.(interface{ Stat() (fs.FileInfo, error) })
	if !ok {
		return false
	}
	fi, err := s.Stat()
	return err == nil && fi.Mode()&fs.ModeCharDevice != 0
}

// color reports whether output to w may carry colors: only on a terminal, and only when NO_COLOR is not set
// (04 §3.1; https://no-color.org: any value but the empty one switches colors off).
func (inv *invocation) color(w io.Writer) bool {
	for _, kv := range inv.environ {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok && v != "" {
			return false
		}
	}
	return inv.terminal(w)
}

// printJSON writes v as one JSON document on one line, as every --json does (04 §3.1). HTML escaping is off, so a
// URL prints as it is.
func printJSON(w io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
}

// linkOutput is a one-time link with the text around it.
type linkOutput struct {
	heading string   // one line that ends with a colon
	url     string   //
	qr      bool     // draw the link as a QR code too
	notes   []string // lines after the link
}

// printLink prints a one-time link (04 §3.3).
//
// On a terminal everything goes to stdout: the heading, the indented link, the QR code, the notes. Otherwise
// stdout gets the link alone on its first line, followed by the QR code when it was asked for, so that a script
// can take it (`url=$(isshoni setup-url)`, install.sh, `docker compose exec -T`); the heading and the notes go to
// stderr.
func printLink(inv *invocation, out linkOutput) error {
	code := ""
	if out.qr {
		var err error
		if code, err = qrText(out.url); err != nil {
			// A link too long for a QR code is still a link.
			_, _ = fmt.Fprintf(inv.stderr, "isshoni: no QR code: %v\n", err)
		}
	}
	var b strings.Builder
	if !inv.terminal(inv.stdout) {
		_, _ = fmt.Fprintln(inv.stderr, out.heading)
		b.WriteString(out.url + "\n" + code)
		if _, err := io.WriteString(inv.stdout, b.String()); err != nil {
			return err
		}
		for _, n := range out.notes {
			_, _ = fmt.Fprintln(inv.stderr, n)
		}
		return nil
	}
	b.WriteString(out.heading + "\n\n  " + out.url + "\n")
	if code != "" {
		b.WriteString("\n  " + strings.ReplaceAll(strings.TrimSuffix(code, "\n"), "\n", "\n  ") + "\n")
	}
	if len(out.notes) > 0 {
		b.WriteString("\n" + strings.Join(out.notes, "\n") + "\n")
	}
	_, err := io.WriteString(inv.stdout, b.String())
	return err
}

// validFor says how long a link works from now on, for "(valid 24 hours, works once)": in days from three days on
// ("7 days"), in hours below that ("24 hours", "48 hours"), in minutes below an hour. The links live for whole
// hours, so rounding loses nothing.
func validFor(expires time.Time) string {
	d := time.Until(expires)
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return fmt.Sprintf("%d %ss", n, name)
	}
	switch {
	case d <= 0:
		return "until " + clock(expires)
	case d >= 72*time.Hour-30*time.Minute:
		return unit(int(d.Round(24*time.Hour)/(24*time.Hour)), "day")
	case d >= time.Hour-30*time.Second:
		return unit(int(d.Round(time.Hour)/time.Hour), "hour")
	default:
		return unit(max(int(d.Round(time.Minute)/time.Minute), 1), "minute")
	}
}

// clock prints a time for people: UTC to the minute.
func clock(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// shortDuration prints a duration without the zero units time.Duration adds: 30m, not 30m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// sleep waits for d, or less when ctx ends; it reports whether the whole time passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
