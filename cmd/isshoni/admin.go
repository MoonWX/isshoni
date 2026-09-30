package main

import (
	"context"
	"flag"
	"slices"
	"time"
)

// The admin commands talk to the running server over its admin socket (04 §12). backup and restore also work
// offline, on the data directory of a stopped server (04 §12.6). Later slices fill in the run functions.

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

func runAdminStatus(context.Context, *invocation, jsonOptions, []string) error {
	return errNotImplemented
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

func runUsersList(context.Context, *invocation, jsonOptions, []string) error {
	return errNotImplemented
}

func runUsersResetPassword(context.Context, *invocation, clientOptions, []string) error {
	return errNotImplemented
}

func runUsersSetRole(_ context.Context, _ *invocation, _ clientOptions, args []string) error {
	if role := args[1]; role != "admin" && role != "user" {
		return usageErrorf("role %q: want admin or user", role)
	}
	return errNotImplemented
}

func runUsersDisable(context.Context, *invocation, clientOptions, []string) error {
	return errNotImplemented
}

func runUsersEnable(context.Context, *invocation, clientOptions, []string) error {
	return errNotImplemented
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

// Invite limits (04 §3.1, 03 §7.9). 0 is the flag's "not sent" value, so the server's setting applies.
const (
	inviteMaxUses = 1000
	inviteMaxTTL  = 720 * time.Hour
)

func runInviteCreate(_ context.Context, _ *invocation, o inviteCreateOptions, _ []string) error {
	if o.uses < 0 || o.uses > inviteMaxUses {
		return usageErrorf("--uses %d: want 1 to %d", o.uses, inviteMaxUses)
	}
	if o.ttl != 0 && (o.ttl < time.Hour || o.ttl > inviteMaxTTL || o.ttl%time.Hour != 0) {
		return usageErrorf("--ttl %s: want whole hours from 1h to 720h", o.ttl)
	}
	return errNotImplemented
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

var logLevels = []string{"debug", "info", "warn", "error"}

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

func runLogLevel(_ context.Context, _ *invocation, o logLevelOptions, args []string) error {
	if !slices.Contains(logLevels, args[0]) {
		return usageErrorf("log level %q: want debug, info, warn or error", args[0])
	}
	if o.forDur <= 0 {
		return usageErrorf("--for %s: want a positive duration", o.forDur)
	}
	return errNotImplemented
}
