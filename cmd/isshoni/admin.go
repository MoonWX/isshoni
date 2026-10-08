package main

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

// The admin commands talk to the running server over its admin socket (04 §12). backup and restore also work
// offline, on the data directory of a stopped server (04 §12.6). backup, restore and rotate-secrets come with a
// later slice of the M1 plan (README S65).

func adminCmd() *command {
	return &command{
		name:    "admin",
		summary: "Manage a running server: status, users, invites, backups",
		help: "Manages the running server through its admin socket (/run/isshoni/admin.sock). Only root and the " +
			"server's own user may use it: run these commands with sudo, or in Docker with 'docker compose exec " +
			"isshoni isshoni admin …'. Exit 4 when the server is not running or the socket is not accessible.",
		subs: []*command{
			adminStatusCmd(), adminBackupCmd(), adminRestoreCmd(), adminUsersCmd(), adminInviteCmd(),
			adminRotateSecretsCmd(), adminLogLevelCmd(),
		},
	}
}

// jsonOptions are the options of a client command whose only flag is --json.
type jsonOptions struct {
	clientOptions
	json bool
}

func jsonFlags(what string) func(fs *flag.FlagSet, o *jsonOptions) {
	return func(fs *flag.FlagSet, o *jsonOptions) {
		fs.BoolVar(&o.json, "json", false, "print "+what+" as JSON")
		o.register(fs)
	}
}

// clientFlags registers only --socket.
func clientFlags(fs *flag.FlagSet, o *clientOptions) { o.register(fs) }

func adminStatusCmd() *command {
	return &command{
		name:    "status",
		summary: "Show version, uptime, TLS, rooms and connections",
		config:  true,
		setup:   leaf(jsonFlags("the status"), runAdminStatus),
	}
}

// call runs one call to the admin socket under the CLI's timeout and turns its error into the command's
// (adminFailure).
func (o clientOptions) call(ctx context.Context, inv *invocation, fn func(ctx context.Context, c *ops.AdminClient) error) error {
	c := o.admin(inv)
	ctx, cancel := context.WithTimeout(ctx, adminCallTimeout)
	defer cancel()
	if err := fn(ctx, c); err != nil {
		return adminFailure(inv, o, c, err)
	}
	return nil
}

// runAdminStatus prints GET /v1/status (04 §12.2): the document itself with --json, a summary otherwise.
func runAdminStatus(ctx context.Context, inv *invocation, o jsonOptions, _ []string) error {
	var st api.ServerStatus
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) (err error) {
		st, err = c.Status(ctx)
		return err
	})
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(inv.stdout, st)
	}
	_, err = inv.stdout.Write([]byte(statusText(st)))
	return err
}

// statusText is `isshoni admin status` for people: version, uptime, TLS, addresses, rooms and connections. A part
// the server did not report is left out.
func statusText(st api.ServerStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "isshoni %s, up %s (since %s)\n", st.Version,
		shortDuration((time.Duration(st.UptimeS) * time.Second).Round(time.Second)), clock(st.StartedAt))
	row := func(name, format string, args ...any) {
		fmt.Fprintf(&b, "  %-11s %s\n", name, fmt.Sprintf(format, args...))
	}
	if st.Origin != "" {
		row("site", "%s", st.Origin)
	}
	if st.TLS.Mode != "" {
		cert := "certificate not ready yet"
		switch {
		case st.TLS.Mode == api.TLSModeOff:
			cert = "the proxy in front has the certificate"
		case st.TLS.Ready && !st.TLS.NotAfter.IsZero():
			cert = "certificate ready, expires " + st.TLS.NotAfter.UTC().Format(time.DateOnly)
		case st.TLS.Ready:
			cert = "certificate ready"
		case st.TLS.LastErrorCode != "":
			cert += " (" + st.TLS.LastErrorCode + ")"
		}
		row("tls", "%s: %s", st.TLS.Mode, cert)
	}
	var ips []string
	if st.PublicIPv4 != "" {
		ips = append(ips, withMethod(st.PublicIPv4, st.PublicIPv4Method))
	}
	if st.PublicIPv6 != "" {
		ips = append(ips, withMethod(st.PublicIPv6, st.PublicIPv6Method))
	}
	if len(ips) > 0 {
		nat := ""
		if st.NAT != "" {
			nat = "; NAT: " + string(st.NAT)
		}
		row("public IP", "%s%s", strings.Join(ips, ", "), nat)
	}
	if len(st.Listeners) > 0 {
		parts := make([]string, len(st.Listeners))
		for i, l := range st.Listeners {
			parts[i] = l.Network + " " + l.Addr
		}
		row("listening", "%s", strings.Join(parts, ", "))
	}
	if len(st.Advertised) > 0 {
		parts := make([]string, len(st.Advertised))
		for i, a := range st.Advertised {
			parts[i] = a.Proto + " " + a.Addr
		}
		row("media", "%s", strings.Join(parts, ", "))
	}
	row("live", "%s, %s, %s", count(st.Rooms, "room"), count(st.Participants, "participant"), count(st.Shares, "share"))
	if st.SchemaVersion != 0 {
		row("database", "schema %d", st.SchemaVersion)
	}
	if t := st.Transfer; t != nil {
		row("transfer", "%s: %.1f GB out, %.1f GB in", t.Month, float64(t.EgressBytes)/1e9, float64(t.IngressBytes)/1e9)
	}
	if u := st.Update; u != nil && u.Latest != "" && u.Latest != st.Version {
		security := ""
		if u.Security {
			security = " (security release)"
		}
		row("update", "%s is available%s: %s", u.Latest, security, u.URL)
	}
	return b.String()
}

func withMethod(ip, method string) string {
	if method == "" {
		return ip
	}
	return ip + " (" + method + ")"
}

// count is "1 room", "3 rooms".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

type backupOptions struct {
	clientOptions
	out              string
	noCerts, offline bool
}

func adminBackupCmd() *command {
	return &command{
		name:    "backup",
		summary: "Write a backup archive: database, secrets and certificates",
		help: "Writes a backup archive with the database, secrets.json and the TLS certificates. The file contains " +
			"your server's keys: keep it private. In a container --out is required; --out - writes to stdout, " +
			"which must not be a terminal.",
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *backupOptions) {
			fs.StringVar(&o.out, "out", "", "write the archive to `PATH`, or - for stdout (default ./isshoni-backup-<version>-<time>.tar.gz)")
			fs.BoolVar(&o.noCerts, "no-certs", false, "leave out the TLS certificates")
			fs.BoolVar(&o.offline, "offline", false, "read the data directory directly while the server is stopped")
			o.register(fs)
		}, runAdminBackup),
	}
}

func runAdminBackup(context.Context, *invocation, backupOptions, []string) error {
	return errNotImplemented
}

type restoreOptions struct {
	clientOptions
	yes, offline bool
}

func adminRestoreCmd() *command {
	return &command{
		name:    "restore",
		args:    "PATH|-",
		summary: "Restore a backup archive or a pre-migration database, then restart the server",
		help: "Restores a backup archive (.tar.gz), or only the database from a pre-migration backups/pre-*.db file, " +
			"and restarts the server. It asks for confirmation on a terminal; otherwise --yes is required. With - " +
			"the archive comes from stdin, so --yes is always required.",
		minArgs: 1,
		maxArgs: 1,
		config:  true,
		setup: leaf(func(fs *flag.FlagSet, o *restoreOptions) {
			fs.BoolVar(&o.yes, "yes", false, "don't ask for confirmation")
			fs.BoolVar(&o.offline, "offline", false, "restore into the data directory directly while the server is stopped")
			o.register(fs)
		}, runAdminRestore),
	}
}

func runAdminRestore(context.Context, *invocation, restoreOptions, []string) error {
	return errNotImplemented
}

func adminUsersCmd() *command {
	return &command{
		name:    "users",
		summary: "Manage accounts: list, reset-password, set-role, disable, enable",
		subs: []*command{
			{
				name:    "list",
				summary: "List accounts",
				config:  true,
				setup:   leaf(jsonFlags("the accounts"), runUsersList),
			},
			{
				name:    "reset-password",
				args:    "NAME",
				summary: "Print a one-time password-reset link; signs the user out everywhere",
				help: "Prints a one-time password-reset link (valid 24 hours, works once). Issuing it clears the " +
					"password and signs the user out everywhere: sessions, devices and push subscriptions. This is " +
					"also how an admin who forgot their password gets back in.",
				minArgs: 1, maxArgs: 1,
				config: true,
				setup:  leaf(clientFlags, runUsersResetPassword),
			},
			{
				name:    "set-role",
				args:    "NAME admin|user",
				summary: "Change a user's role",
				help:    "Makes a user an admin or a regular user. The last active admin can't be demoted.",
				minArgs: 2, maxArgs: 2,
				config: true,
				setup:  leaf(clientFlags, runUsersSetRole),
			},
			{
				name:    "disable",
				args:    "NAME",
				summary: "Block sign-in and sign the user out everywhere",
				help:    "Blocks sign-in and revokes the user's sessions and devices. The last active admin can't be disabled.",
				minArgs: 1, maxArgs: 1,
				config: true,
				setup:  leaf(clientFlags, runUsersDisable),
			},
			{
				name:    "enable",
				args:    "NAME",
				summary: "Allow sign-in again",
				minArgs: 1, maxArgs: 1,
				config: true,
				setup:  leaf(clientFlags, runUsersEnable),
			},
		},
	}
}

// runUsersList prints GET /v1/users: the document itself with --json, a table otherwise.
func runUsersList(ctx context.Context, inv *invocation, o jsonOptions, _ []string) error {
	var users ops.AdminUsers
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) (err error) {
		users, err = c.Users(ctx)
		return err
	})
	if err != nil {
		return err
	}
	if o.json {
		if users.Users == nil {
			users.Users = []ops.AdminUser{} // "users": [], never null
		}
		return printJSON(inv.stdout, users)
	}
	if len(users.Users) == 0 {
		_, err := fmt.Fprintln(inv.stdout, "No accounts yet. `isshoni setup-url` prints the link that creates the admin account.")
		return err
	}
	tw := tabwriter.NewWriter(inv.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tCREATED\tLAST SEEN")
	for _, u := range users.Users {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Username, u.Role, u.Status,
			clock(time.Time(u.CreatedAt)), clock(time.Time(u.LastSeenAt)))
	}
	return tw.Flush()
}

// runUsersResetPassword prints a one-time password-reset link (04 §12.5). Issuing it has already cleared the
// password and signed the user out everywhere, and the command says so: it is the plan's "a password reset revokes
// everything".
func runUsersResetPassword(ctx context.Context, inv *invocation, o clientOptions, args []string) error {
	name := args[0]
	var link ops.AdminLink
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) (err error) {
		link, err = c.ResetLink(ctx, name)
		return err
	})
	if err != nil {
		return err
	}
	return printLink(inv, linkOutput{
		heading: fmt.Sprintf("Password-reset link for %s (valid %s, works once):", name, validFor(time.Time(link.ExpiresAt))),
		url:     link.URL,
		notes: []string{
			fmt.Sprintf("A password reset revokes everything: %s's old password no longer works, and %s is signed out", name, name),
			"everywhere (browsers, devices and push notifications).",
		},
	})
}

func runUsersSetRole(ctx context.Context, inv *invocation, o clientOptions, args []string) error {
	name, role := args[0], api.Role(args[1])
	if role != api.RoleAdmin && role != api.RoleUser {
		return usageErrorf("role %q: want admin or user", args[1])
	}
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) error { return c.SetRole(ctx, name, role) })
	if err != nil {
		return err
	}
	what := "a regular user"
	if role == api.RoleAdmin {
		what = "an admin"
	}
	_, err = fmt.Fprintf(inv.stdout, "%s is now %s.\n", name, what)
	return err
}

func runUsersDisable(ctx context.Context, inv *invocation, o clientOptions, args []string) error {
	name := args[0]
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) error { return c.SetDisabled(ctx, name, true) })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(inv.stdout, "%s can no longer sign in and is signed out everywhere.\n", name)
	return err
}

func runUsersEnable(ctx context.Context, inv *invocation, o clientOptions, args []string) error {
	name := args[0]
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) error { return c.SetDisabled(ctx, name, false) })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(inv.stdout, "%s can sign in again.\n", name)
	return err
}

type inviteCreateOptions struct {
	clientOptions
	uses int           // 0 = not sent: the server's setting applies
	ttl  time.Duration // 0 = not sent
}

func adminInviteCmd() *command {
	return &command{
		name:    "invite",
		summary: "Create invite links",
		subs: []*command{{
			name:    "create",
			summary: "Print an invite link",
			help: "Prints an invite link. A flag left out is not sent, so the server's invite settings apply " +
				"(Admin → Settings).",
			config: true,
			setup: leaf(func(fs *flag.FlagSet, o *inviteCreateOptions) {
				fs.IntVar(&o.uses, "uses", 0, "how many accounts the link may create, 1 to 1000 (default: the server's setting)")
				fs.DurationVar(&o.ttl, "ttl", 0, "how long the link works, in whole hours from 1h to 720h (default: the server's setting)")
				o.register(fs)
			}, runInviteCreate),
		}},
	}
}

// runInviteCreate prints an invite link (04 §12.5). 0 is a flag's "not sent" value: the server's invite setting
// applies then (03 §9). The limits are the socket's (04 §3.1, 03 §7.9).
func runInviteCreate(ctx context.Context, inv *invocation, o inviteCreateOptions, _ []string) error {
	if o.uses < 0 || o.uses > ops.AdminInviteMaxUses {
		return usageErrorf("--uses %d: want 1 to %d", o.uses, ops.AdminInviteMaxUses)
	}
	if o.ttl != 0 && (o.ttl < time.Hour || o.ttl > ops.AdminInviteMaxTTL || o.ttl%time.Hour != 0) {
		return usageErrorf("--ttl %s: want whole hours from 1h to 720h", o.ttl)
	}
	var link ops.AdminLink
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) (err error) {
		link, err = c.CreateInvite(ctx, o.uses, o.ttl)
		return err
	})
	if err != nil {
		return err
	}
	return printLink(inv, linkOutput{
		heading: fmt.Sprintf("Invite link (valid %s):", validFor(time.Time(link.ExpiresAt))),
		url:     link.URL,
	})
}

type confirmOptions struct {
	clientOptions
	yes bool
}

func adminRotateSecretsCmd() *command {
	return &command{
		name:    "rotate-secrets",
		summary: "Replace all generated secrets and restart the server",
		help: "Replaces the session, invite, resume and push (VAPID) keys and restarts the server. Everyone is " +
			"signed out, open invite links stop working and push notifications must be switched on again. It asks " +
			"for confirmation on a terminal; otherwise --yes is required.",
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *confirmOptions) {
			fs.BoolVar(&o.yes, "yes", false, "don't ask for confirmation")
			o.register(fs)
		}, runRotateSecrets),
	}
}

func runRotateSecrets(context.Context, *invocation, confirmOptions, []string) error {
	return errNotImplemented
}

type logLevelOptions struct {
	clientOptions
	forDur time.Duration
}

func adminLogLevelCmd() *command {
	return &command{
		name:    "log-level",
		args:    "debug|info|warn|error",
		summary: "Change the server's log level for a while",
		help:    "Changes the running server's log level and switches it back after --for.",
		minArgs: 1, maxArgs: 1,
		config: true,
		setup: leaf(func(fs *flag.FlagSet, o *logLevelOptions) {
			fs.DurationVar(&o.forDur, "for", 30*time.Minute, "switch back after `DUR`")
			o.register(fs)
		}, runLogLevel),
	}
}

// runLogLevel changes the running server's log level for a while (04 §3.1, §12.2).
func runLogLevel(ctx context.Context, inv *invocation, o logLevelOptions, args []string) error {
	level := args[0]
	if !slices.Contains(ops.LogLevelNames, level) {
		return usageErrorf("log level %q: want debug, info, warn or error", level)
	}
	if o.forDur <= 0 {
		return usageErrorf("--for %s: want a positive duration", o.forDur)
	}
	err := o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) error { return c.SetLogLevel(ctx, level, o.forDur) })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(inv.stdout, "The log level is %s for %s, then back to the configured level.\n", level, shortDuration(o.forDur))
	return err
}
