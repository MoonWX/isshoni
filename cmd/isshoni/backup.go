package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/version"
)

// backup, restore and rotate-secrets (04 §5.3, §12.3, §12.4, §12.6). The three talk to the running server over its
// admin socket, which does the work as the service user; backup and restore also work with --offline, directly on
// the data directory of a stopped server, for when the server can't start at all.

// How long the commands of this file wait.
const (
	// adminStreamTimeout bounds a backup's download and a restore's upload with its validation on the server: a
	// large database on a slow disk takes minutes, not the seconds of the other calls.
	adminStreamTimeout = 30 * time.Minute
	// restartPollEvery is how often restore and rotate-secrets ask whether the restarted server answers.
	restartPollEvery = 250 * time.Millisecond
)

// restartWait is how long restore and rotate-secrets wait for the restarted server to answer (04 §12.4): 60 s. It
// is a variable only so that a test of a server that does not come back need not take a minute.
var restartWait = 60 * time.Second

// keepPrivate is what every backup command says about its file (04 §12.3).
const keepPrivate = "This file contains your server's keys. Keep it private."

// inContainer reports whether the command runs in a container (04 §5.1): the commands then print the Docker form
// of what they suggest.
func (inv *invocation) inContainer() bool {
	return config.Host{Environ: inv.environ, Root: inv.hostRoot}.Container() != config.ContainerNone
}

// euid is the effective uid of the command.
func (inv *invocation) euid() int {
	if inv.geteuid != nil {
		return inv.geteuid()
	}
	return os.Geteuid()
}

// stdinTerminal reports whether stdin is a terminal that someone can answer a question on (isTerminal).
func (inv *invocation) stdinTerminal() bool {
	if inv.stdinTTY != nil {
		return inv.stdinTTY()
	}
	return isTerminal(inv.stdin)
}

// ---- backup ----

// backupDockerForm is the one documented way to take a backup out of a container (04 §12.3, 06 §6.4): the
// subshell's umask makes the file on the host 0600, which a plain redirect would not.
const backupDockerForm = "(umask 077; docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz)"

// runAdminBackup writes a backup archive (04 §12.3): to --out, to stdout with --out -, or to
// ./isshoni-backup-<version>-<time>.tar.gz. The archive comes from the running server, or with --offline from the
// data directory of a stopped one.
func runAdminBackup(ctx context.Context, inv *invocation, o backupOptions, _ []string) error {
	switch {
	case o.out == "" && inv.inContainer():
		// A container has no working directory worth writing to: the file would be lost with it.
		return exitWith(exitUsage, errors.New("--out is required in a container: a file in the container's working "+
			"directory is lost with the container. From the host, run:\n  "+backupDockerForm))
	case o.out == "-" && inv.terminal(inv.stdout):
		// Also what `docker compose exec` without -T gives: a pseudo-terminal, which would corrupt the archive.
		form := "(umask 077; sudo isshoni admin backup --out - > isshoni-backup.tar.gz)"
		if inv.inContainer() {
			form = backupDockerForm
		}
		return exitWith(exitUsage, errors.New("stdout is a terminal; redirect it to a file:\n  "+form))
	}
	if o.offline {
		return backupOffline(ctx, inv, o)
	}
	c := o.admin(inv)
	ctx, cancel := context.WithTimeout(ctx, adminStreamTimeout)
	defer cancel()
	b, err := c.Backup(ctx, !o.noCerts)
	if err != nil {
		return adminFailure(inv, o.clientOptions, c, err)
	}
	defer func() { _ = b.Close() }()
	name := b.Filename
	if name == "" {
		name = ops.BackupFileName(version.Version(), time.Now())
	}
	return writeBackup(inv, o.out, name, func(w io.Writer) error {
		if _, err := io.Copy(w, b); err != nil {
			return fmt.Errorf("the server's answer broke off: %w", err)
		}
		return nil
	})
}

// backupOffline is `admin backup --offline` (04 §12.3, §12.6): the snapshot is 03's store.BackupFile on the stopped
// server's database, which a newer schema does not stop; only when that fails are the raw files copied.
func backupOffline(ctx context.Context, inv *invocation, o backupOptions) error {
	data, release, err := offlineData(ctx, inv, o.clientOptions)
	if err != nil {
		return err
	}
	defer release()
	b, err := data.PrepareBackup(ctx, ops.BackupOptions{NoCerts: o.noCerts})
	if err != nil {
		return dataFailure(err)
	}
	defer b.Close()
	if b.Empty() {
		return fmt.Errorf("there is nothing to back up in %s: no database, no secrets.json and no certificates", data.Paths.DataDir)
	}
	if b.Manifest.Raw {
		_, _ = fmt.Fprintln(inv.stderr, "The database could not be read as a database, so this backup holds its files as they are. "+
			"`isshoni admin restore` takes it only if the database in it passes its checks.")
	}
	return writeBackup(inv, o.out, b.Filename(), func(w io.Writer) error { return dataFailure(b.WriteTo(ctx, w)) })
}

// writeBackup writes an archive where --out says: to stdout for "-", else to a new file with mode 0600, out or
// name in the working directory. A file that exists is never overwritten (exit 7); a file that could not be
// written to its end is removed again. Afterwards it says where the archive is, and that it holds the keys.
func writeBackup(inv *invocation, out, name string, write func(io.Writer) error) error {
	if out == "-" {
		if err := write(inv.stdout); err != nil {
			return fmt.Errorf("the backup failed, and what stdout got is not a whole archive: %w", err)
		}
		_, _ = fmt.Fprintln(inv.stderr, keepPrivate)
		return nil
	}
	if out == "" {
		out = name
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's --out
	created := err == nil
	if errors.Is(err, fs.ErrExist) {
		fi, statErr := os.Stat(out)
		if statErr != nil || fi.Mode().IsRegular() || fi.IsDir() {
			return exitWith(exitRefused, fmt.Errorf("%s exists, and a backup never overwrites a file: remove it, or name another --out", out))
		}
		f, err = os.OpenFile(out, os.O_WRONLY, 0) //nolint:gosec // G304: a pipe or a device that the operator named
	}
	if err != nil {
		return fmt.Errorf("the backup can't be written: %w", err)
	}
	err = write(f)
	if err == nil && created {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if created {
			_ = os.Remove(out)
		}
		return fmt.Errorf("the backup failed: %w", err)
	}
	size := ""
	if fi, err := os.Stat(out); err == nil && created {
		size = " (" + byteSize(fi.Size()) + ")"
	}
	_, err = fmt.Fprintf(inv.stdout, "Wrote %s%s.\n%s\n", out, size, keepPrivate)
	return err
}

// byteSize prints a file size for people: 512 B, 3.4 MB.
func byteSize(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
}

// ---- restore ----

// runAdminRestore restores a backup archive, or only the database from a database file such as 03's
// backups/pre-<schema>-<ts>.db (04 §12.4). It says what the file is, asks for confirmation on a terminal (--yes
// otherwise), hands the file to the server, and waits for the server to be back. With --offline it does the
// restore itself, on the data directory of a stopped server.
func runAdminRestore(ctx context.Context, inv *invocation, o restoreOptions, args []string) error {
	path, fromStdin := args[0], args[0] == "-"
	ask := confirmation{yes: o.yes, stdinIsData: fromStdin, offline: o.offline, words: 2}
	if err := canConfirm(inv, ask); err != nil {
		return err
	}
	var (
		in   io.Reader
		size int64 = -1
	)
	if fromStdin {
		if in = inv.stdin; in == nil {
			in = strings.NewReader("")
		}
	} else {
		f, err := os.Open(path) //nolint:gosec // G304: the operator's file
		if err != nil {
			return fmt.Errorf("the backup can't be read: %w", err)
		}
		defer func() { _ = f.Close() }()
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			size = fi.Size()
		}
		in = f
	}
	// The first bytes say what it is; the server looks again, at the whole of it.
	br := bufio.NewReaderSize(in, 64<<10)
	head, _ := br.Peek(16)
	kind, err := ops.KindOf(head)
	if err != nil {
		return dataFailure(err)
	}
	what := []string{path + " is a database file: only the database is replaced; secrets.json and the certificates stay."}
	contentType := ops.AdminContentSQLite
	if kind == ops.RestoreArchive {
		contentType = ops.AdminContentArchive
		what = []string{"The backup replaces the server's database, its secrets.json and, when the backup has them, its certificates."}
		if !fromStdin { // stdin can be read only once, and --yes is required there anyway
			m, err := manifestOf(path)
			if err != nil {
				return dataFailure(err)
			}
			what = append([]string{describeManifest(path, m)}, what...)
		}
	}
	const saved = "The current state is saved first, as backups/pre-restore-<time>.tar.gz in the data directory."
	if o.offline {
		ask.what = append(what, saved)
		data, release, err := offlineData(ctx, inv, o.clientOptions)
		if err != nil {
			return err
		}
		defer release()
		if err := confirm(inv, ask); err != nil {
			return err
		}
		return restoreOffline(ctx, inv, data, br, size)
	}

	c := o.admin(inv)
	before, err := serverStart(ctx, inv, o.clientOptions, c)
	if err != nil {
		return err
	}
	ask.what = append(what, "The server restarts. "+saved)
	if err := confirm(inv, ask); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, adminStreamTimeout)
	res, err := c.Restore(callCtx, br, contentType)
	cancel()
	if err != nil {
		return adminFailure(inv, o.clientOptions, c, err)
	}
	_, _ = fmt.Fprintln(inv.stderr, "The server accepted the backup and is restarting…")
	st, err := waitRestarted(ctx, c, before)
	if err != nil {
		return restartNotSeen(inv, "The restore is in place", err)
	}
	_, err = fmt.Fprintf(inv.stdout, "Restored. The server is back (isshoni %s).\n%s", st.Version, preRestoreNote(res.PreRestoreBackup))
	return err
}

// restoreOffline is `admin restore --offline` (04 §12.6): both halves of the restore here, with no server between
// them. Nothing is re-exec'd; the operator starts the service afterwards.
func restoreOffline(ctx context.Context, inv *invocation, data *ops.DataDir, body io.Reader, size int64) error {
	info, err := data.PrepareRestore(ctx, body, size)
	if err != nil {
		return dataFailure(err)
	}
	if _, err := ops.CompleteRestore(data.Paths, data.Logger); err != nil {
		return fmt.Errorf("the restore was interrupted while it swapped the files (%w). Run the command again, or start "+
			"the server: either completes it", err)
	}
	start := "sudo systemctl start isshoni"
	if inv.inContainer() {
		start = "docker compose up -d"
	}
	_, err = fmt.Fprintf(inv.stdout, "Restored into %s. Start the server: %s\n%s", data.Paths.DataDir, start, preRestoreNote(info.PreRestoreBackup))
	return err
}

// preRestoreNote says where the state from before a restore is, or nothing when there was none to save.
func preRestoreNote(rel string) string {
	if rel == "" {
		return ""
	}
	return "The state from before the restore is in the data directory: " + rel + "\n"
}

// manifestOf reads the manifest of the archive at path, with a file handle of its own.
func manifestOf(path string) (ops.Manifest, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's file
	if err != nil {
		return ops.Manifest{}, fmt.Errorf("the backup can't be read: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ops.ReadManifest(f)
}

// describeManifest is one line about an archive: "backup.tar.gz: a backup of isshoni 0.3.0 from 2026-09-29 10:15
// UTC (database schema 7), with database, secrets and certificates".
func describeManifest(path string, m ops.Manifest) string {
	names := map[string]string{"isshoni.db": "database", "secrets.json": "secrets", "certmagic/": "certificates"}
	var parts []string
	for _, c := range m.Contents {
		if n, ok := names[c]; ok {
			parts = append(parts, n)
		}
	}
	holds := "nothing this isshoni knows"
	switch len(parts) {
	case 0:
	case 1:
		holds = parts[0]
	default:
		holds = strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	raw := ""
	if m.Raw {
		raw = ", a raw copy of the database files"
	}
	return fmt.Sprintf("%s: a backup of isshoni %s from %s (database schema %d%s), with %s.",
		path, m.IsshoniVersion, clock(time.Time(m.CreatedAt)), m.SchemaVersion, raw, holds)
}

// ---- rotate-secrets ----

// runRotateSecrets replaces every generated secret, the session, invite and resume keys and the VAPID pair, and
// waits for the server to be back (04 §5.3). There is no flag to pick keys: rotation is for a suspected
// compromise, and then all of them go.
func runRotateSecrets(ctx context.Context, inv *invocation, o confirmOptions, _ []string) error {
	ask := confirmation{yes: o.yes, words: 2, what: []string{
		"This replaces the session, invite, resume and push (VAPID) keys and restarts the server.",
		"Everyone is signed out, open invite, setup and password-reset links stop working, and push notifications " +
			"must be switched on again.",
	}}
	if err := canConfirm(inv, ask); err != nil {
		return err
	}
	c := o.admin(inv)
	before, err := serverStart(ctx, inv, o.clientOptions, c)
	if err != nil {
		return err
	}
	if err := confirm(inv, ask); err != nil {
		return err
	}
	var keys []string
	for _, k := range config.KeyNames() {
		keys = append(keys, string(k))
	}
	var res ops.AdminRotateResult
	err = o.call(ctx, inv, func(ctx context.Context, c *ops.AdminClient) (err error) {
		res, err = c.RotateSecrets(ctx, ops.AdminRotateRequest{Keys: keys, VAPID: true})
		return err
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(inv.stderr, "The server has new keys and is restarting…")
	st, err := waitRestarted(ctx, c, before)
	if err != nil {
		return restartNotSeen(inv, "The new keys are in place", err)
	}
	_, err = fmt.Fprintf(inv.stdout, "Rotated: %s. The server is back (isshoni %s); everyone has to sign in again.\n",
		strings.Join(res.Rotated, ", "), st.Version)
	return err
}

// ---- what the commands share ----

// serverStart asks the running server when it started, so that waitRestarted can tell the restarted server from
// this one. It is also the commands' first look at the socket: no server, or no permission, ends them here,
// before anything is changed.
func serverStart(ctx context.Context, inv *invocation, o clientOptions, c *ops.AdminClient) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, adminCallTimeout)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		return time.Time{}, adminFailure(inv, o, c, err)
	}
	return st.StartedAt, nil
}

// waitRestarted waits until a server that started after before answers on the socket and is alive (04 §12.4), for
// at most restartWait. While the old server shuts down and the new one starts, nothing answers, or the old one
// still does: both are asked again.
func waitRestarted(ctx context.Context, c *ops.AdminClient, before time.Time) (api.ServerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, restartWait)
	defer cancel()
	for {
		callCtx, cancelCall := context.WithTimeout(ctx, healthCallTimeout)
		st, err := c.Status(callCtx)
		if err == nil && !st.StartedAt.Equal(before) {
			var h ops.AdminHealth
			if h, err = c.Health(callCtx); err == nil && h.Status == ops.StatusOK {
				cancelCall()
				return st, nil
			}
		}
		cancelCall()
		if !sleep(ctx, restartPollEvery) {
			if err == nil {
				err = errors.New("the server that answers is still the one from before")
			}
			return api.ServerStatus{}, err
		}
	}
}

// restartNotSeen is the error of a restore or a rotation whose server did not come back in time: the change is
// made, and the server is what to look at.
func restartNotSeen(inv *invocation, done string, err error) error {
	logs := "journalctl -u isshoni"
	if inv.inContainer() {
		logs = "docker compose logs isshoni"
	}
	return fmt.Errorf("%s, but the server has not answered within %s (%w). Look at its log (%s) and at `isshoni doctor`",
		strings.ToLower(done[:1])+done[1:], shortDuration(restartWait), err, logs)
}

// confirmation is what a destructive command asks before it acts (04 §3.1).
type confirmation struct {
	yes         bool     // --yes: don't ask
	stdinIsData bool     // stdin carries the archive (restore -): there is nobody to ask
	offline     bool     // --offline: the command to print runs as the service user
	words       int      // how many words of the command line name the command ("admin restore": 2)
	what        []string // what will happen, one line each
}

// A destructive command is confirmed in two steps, with whatever the command checks without changing anything
// between them (its file, the server on the socket), so that nobody is asked about something that could not
// happen anyway:
//
//   - canConfirm, first: with --yes, or with a terminal to ask on, the command may go on. Without either it
//     refuses (exit 2) and prints the command with --yes added, in the form of this environment (04 §3.1).
//   - confirm, last: without --yes it says what will happen and asks `Continue? [y/N]`. Anything but y or yes is a
//     no (exit 1), and nothing was changed.

// canConfirm is the first step.
func canConfirm(inv *invocation, c confirmation) error {
	if c.yes || (!c.stdinIsData && inv.stdinTerminal()) {
		return nil
	}
	return needsYes(inv, c)
}

// needsYes is the refusal of a destructive command that nobody can confirm.
func needsYes(inv *invocation, c confirmation) error {
	return exitWith(exitUsage, errors.New("it needs a confirmation, and there is no terminal to ask on. Add --yes:\n  "+
		yesCommand(inv, c)))
}

// confirm is the last step.
func confirm(inv *invocation, c confirmation) error {
	if c.yes {
		return nil
	}
	for _, line := range c.what {
		_, _ = fmt.Fprintln(inv.stderr, line)
	}
	_, _ = fmt.Fprint(inv.stderr, "Continue? [y/N] ")
	answer, err := bufio.NewReader(inv.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	if err != nil && strings.TrimSpace(answer) == "" {
		// The input ended without an answer: that was no terminal after all.
		_, _ = fmt.Fprintln(inv.stderr)
		return needsYes(inv, c)
	}
	return exitWith(exitRuntime, errors.New("cancelled; nothing was changed"))
}

// yesCommand is the command line as typed with --yes added, in the environment's form (04 §3.1):
//
//	sudo isshoni admin restore --yes /root/backup.tar.gz
//	docker compose exec -T isshoni isshoni admin restore --yes - < backup.tar.gz
//
// An offline command runs as the service user, and in Docker in a container of its own, because the service's
// container is stopped (04 §12.6).
func yesCommand(inv *invocation, c confirmation) string {
	args := slices.Insert(slices.Clone(inv.args), min(c.words, len(inv.args)), "--yes")
	line := strings.Join(quoteArgs(args), " ")
	var prefix string
	switch container := inv.inContainer(); {
	case container && c.offline && c.stdinIsData:
		prefix = "docker compose run --rm -T isshoni "
	case container && c.offline:
		prefix = "docker compose run --rm isshoni "
	case container:
		prefix = "docker compose exec -T isshoni isshoni "
	case c.offline:
		prefix = "sudo -u isshoni isshoni "
	default:
		prefix = "sudo isshoni "
	}
	if c.stdinIsData {
		line += " < backup.tar.gz"
	}
	return prefix + line
}

// dataFailure turns an error of ops's backup and restore into the command's: a refusal (*ops.DataError) prints
// its message and fix and exits like the same answer of the server would, 7 for backup_newer and 1 otherwise
// (04 §3.2). Any other error is returned as it is.
func dataFailure(err error) error {
	var de *ops.DataError
	if !errors.As(err, &de) {
		return err
	}
	text := de.Message
	if de.Fix != "" {
		text += "\n  fix: " + de.Fix
	}
	return exitWith(refusalExit(de.Code), errors.New(text))
}

// offlineData prepares an offline command (04 §12.6) and returns the data directory to act on, with a function
// that releases it. In this order:
//
//  1. the admin socket must not answer: "the server is running; drop --offline" (exit 7). This check is first
//     because its message is the friendliest;
//  2. not as root outside a container (exit 7): root would create files that the service user can't open;
//  3. the data-directory lock, without waiting (exit 7 when another process holds it, which also sees a server in
//     another container on the same volume);
//  4. a restore that was interrupted is completed, as the server would at its start, so that the command sees a
//     data directory in one piece.
//
// The config may have errors: that is one reason a server does not start, and the commands need only data_dir
// and the socket's path from it.
func offlineData(ctx context.Context, inv *invocation, o clientOptions) (*ops.DataDir, func(), error) {
	const running = "the server is running; drop --offline"
	c := o.admin(inv)
	callCtx, cancel := context.WithTimeout(ctx, healthCallTimeout)
	_, err := c.Health(callCtx)
	cancel()
	switch {
	case ctx.Err() != nil:
		return nil, nil, ctx.Err()
	case errors.Is(err, ops.ErrAdminNotRunning):
	case errors.Is(err, ops.ErrAdminPermission):
		return nil, nil, adminFailure(inv, o, c, err)
	default: // an answer, also one that is no health document, or a socket on which something listens and is silent
		return nil, nil, exitWith(exitRefused, errors.New(running))
	}
	if inv.cfgErr != nil {
		_, _ = fmt.Fprintf(inv.stderr, "The config has errors (isshoni config check); using data_dir = %s\n", inv.cfg.DataDir)
	}
	paths := inv.cfg.Paths()
	if inv.euid() == 0 && !inv.inContainer() {
		return nil, nil, exitWith(exitRefused, errors.New("run as the service user, so that the files stay the "+
			"server's:\n  sudo -u isshoni isshoni "+strings.Join(quoteArgs(inv.args), " ")))
	}
	if fi, err := os.Stat(paths.DataDir); err != nil || !fi.IsDir() {
		return nil, nil, fmt.Errorf("there is no data directory at %s (data_dir). Pass the server's --config or "+
			"--data-dir, or start the server once so that it creates one", paths.DataDir)
	}
	lock, err := config.LockDataDir(ctx, paths)
	switch {
	case errors.Is(err, config.ErrDataDirLocked):
		return nil, nil, exitWith(exitRefused, fmt.Errorf("%s (another isshoni process is using %s)", running, paths.DataDir))
	case err != nil:
		return nil, nil, err
	}
	log := slog.New(cliLog{w: inv.stderr})
	if err := ops.RecoverRestore(paths, log); err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	data := &ops.DataDir{Paths: paths, DB: dbFiles(paths), Logger: log}
	return data, func() { _ = lock.Close() }, nil
}

// dbFiles are 03's file-level store functions for the offline commands and offline doctor (04 §6.6).
func dbFiles(paths config.Paths) doctor.DBFiles { return server.DBFiles(paths.Backups) }

// cliLog is the log of the offline commands: what backup and restore would write to the server's log goes to
// stderr as plain lines, "isshoni: <message> (key=value …)". Debug lines are left out.
type cliLog struct {
	w     io.Writer
	attrs []slog.Attr
}

func (h cliLog) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h cliLog) Handle(_ context.Context, r slog.Record) error {
	var parts []string
	add := func(a slog.Attr) bool {
		if a.Key != "component" && !a.Equal(slog.Attr{}) {
			parts = append(parts, a.Key+"="+a.Value.String())
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	line := "isshoni: " + r.Message
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, " ") + ")"
	}
	_, err := fmt.Fprintln(h.w, line)
	return err
}

func (h cliLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return cliLog{w: h.w, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h cliLog) WithGroup(string) slog.Handler { return h }
