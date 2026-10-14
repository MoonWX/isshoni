# Command line

`isshoni` is one program: the server, and the commands that manage it. `isshoni help <command>` shows each command's
flags. On a server installed with the script, run the commands with `sudo`; in Docker, run them with
`docker compose exec isshoni isshoni <command>`.

Most commands talk to the running server through its admin socket (`/run/isshoni/admin.sock`), which only root and
the server's user `isshoni` may use. Commands that read the configuration accept `--config PATH` (default
`/etc/isshoni/isshoni.toml`, or `ISSHONI_CONFIG`) and one flag per [configuration key](/reference/config). `--json`
prints exactly one JSON document.

## Server {#server}

| Command | What it does |
|---|---|
| `isshoni serve` | Runs the server until it gets SIGTERM or SIGINT, then shuts down gracefully (friends' apps reconnect by themselves). systemd and Docker start it this way. After a restore or `rotate-secrets` it restarts in place |
| `isshoni setup-url [--qr\|--no-qr] [--wait DUR] [--json]` | Prints a one-time link that creates the admin account (valid 24 hours, works once), with a QR code on a terminal. Running it again cancels the previous link. Only while no admin exists |
| `isshoni doctor [--json] [--strict] [--only IDS] [--list-checks]` | Checks the server and this machine and prints a fix for each problem ([Troubleshooting](/troubleshooting#doctor)). Works with or without a running server. Bandwidth estimate: `--people N --sharing N --thumbnails N --quality 1080p60\|1440p60\|2160p60 --preset auto\|movie --hours H` |
| `isshoni healthcheck [--ready] [--wait DUR]` | Exits 0 when the server is alive (with `--ready`: ready to serve), else 1. Docker's health check uses it |
| `isshoni version [--short\|--json]` | Version, commit, Go version, platform, protocol and database schema versions. `--short` prints only the version |

## Admin {#admin}

| Command | What it does |
|---|---|
| `isshoni admin status [--json]` | Version, uptime, certificate, rooms and connections |
| `isshoni admin users list [--json]` | Lists the accounts |
| `isshoni admin users reset-password NAME` | Prints a one-time password-reset link and signs the user out everywhere. Also how an admin who forgot their password gets back in |
| `isshoni admin users set-role NAME admin\|user` | Makes a user an admin or a regular user. The last active admin can't be demoted |
| `isshoni admin users disable NAME` · `enable NAME` | Blocks sign-in (and signs the user out everywhere), or allows it again |
| `isshoni admin invite create [--uses N] [--ttl DUR]` | Prints an invite link. Without flags the server's invite settings apply; `--ttl` is whole hours from `1h` to `720h` |
| `isshoni admin backup [--out PATH\|-] [--no-certs] [--offline]` | Writes a backup archive: the database, `secrets.json` and the certificates. It holds the server's keys: keep it private. Never overwrites a file |
| `isshoni admin restore PATH\|- [--yes] [--offline]` | Restores a backup archive, or only the database from a `backups/pre-*.db` file, and restarts the server. The state before the restore is kept as `backups/pre-restore-<time>.tar.gz` |
| `isshoni admin rotate-secrets [--yes]` | Replaces all generated keys and restarts the server: everyone is signed out, open invite links stop working, notifications must be turned on again |
| `isshoni admin log-level LEVEL [--for DUR]` | Changes the log level (`debug`, `info`, `warn`, `error`) for a while, 30 minutes by default |

`restore` and `rotate-secrets` ask before they act; without a terminal they need `--yes`.

**When the server can't start** (for example after a downgrade), `admin backup --offline` and
`admin restore --offline PATH` work on the data directory directly. Run them as the server's user while it is
stopped: `sudo -u isshoni isshoni admin restore --offline PATH`, or in Docker `docker compose run --rm isshoni admin
restore --offline PATH` after `docker compose stop`. Then start the server.

## Configuration {#config}

| Command | What it does |
|---|---|
| `isshoni config check` | Loads and validates the configuration and prints every problem with its fix |
| `isshoni config print [--json]` | Prints the effective configuration and where each value comes from |
| `isshoni config example [config flags]` | Prints a commented `isshoni.toml` with the given values set |
| `isshoni config init --path PATH [config flags]` | Writes that file to PATH (mode `0640`). Never overwrites a file; ignores the `ISSHONI_*` environment |

## Exit codes {#exit-codes}

| Code | Meaning |
|---|---|
| 0 | Success (`doctor`: no check failed) |
| 1 | An error at run time, or a cancelled confirmation |
| 2 | Wrong usage: an unknown command or flag, a missing argument, or a destructive command without a terminal and without `--yes` |
| 4 | The server is not reachable on the admin socket: not running, or no permission (run it with `sudo`) |
| 5 | `doctor`: at least one check failed (with `--strict`, also a warning) |
| 7 | Refused: an admin already exists (`setup-url`), the last admin can't lose the role, the file exists, the backup is newer than this isshoni, `--offline` while the server runs, and the like |
| 75 | The server must be restarted by its supervisor (systemd and Docker do it) |
| 78 | A configuration or data problem that a restart can't fix; the message says what to do. systemd stops restarting the server |
| 130 | Interrupted (Ctrl-C) |

`isshoni healthcheck` exits only 0 or 1.
