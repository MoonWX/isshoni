package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// The checks of what is on disk: config, data_dir, secrets and schema (04 §13.2). They read and never write:
// doctor creates no file in the data directory, in either mode (04 §13.1).

// lowDiskBytes is the free space below which data_dir warns: 1 GB.
const lowDiskBytes = 1_000_000_000

// checkConfig reports the problems the config loader found (04 §4.5): errors fail, warnings warn.
func checkConfig(_ context.Context, r *run) result {
	file, read := r.cfg.File()
	p := params{"file": file, "fileRead": read}
	var errs, warns []config.Problem
	for _, pr := range r.cfg.Problems() {
		if pr.Severity == config.SeverityError {
			errs = append(errs, pr)
		} else {
			warns = append(warns, pr)
		}
	}
	first := func(pr config.Problem) {
		p["errors"], p["warnings"] = len(errs), len(warns)
		p["first"] = problemLine(pr)
		if pr.Fix != "" {
			p["firstFix"] = pr.Fix
		}
	}
	switch {
	case len(errs) > 0:
		first(errs[0])
		return failResult(codeConfigInvalid, p, fixConfigCheck)
	case len(warns) > 0:
		first(warns[0])
		return warnResult(codeConfigWarnings, p, fixConfigCheck)
	}
	return okResult(codeConfigOK, p)
}

// problemLine is a config problem on one line, as `isshoni config check` prints its first line, without the
// "config error: " in front: `tls.mode = "ip" (file /etc/isshoni/isshoni.toml:7) needs a public address, …`.
func problemLine(pr config.Problem) string {
	line, _, _ := strings.Cut(pr.String(), "\n")
	line = strings.TrimPrefix(line, "config "+pr.Severity+": ")
	return strings.TrimSuffix(line, ".")
}

// serviceUID returns the uid the server runs as, when doctor knows it: its own uid inside the server, and offline
// the caller's uid, who is taken for the service's user. Offline root outside a container is the exception: the
// service has a user of its own there (04 §5.1) and root reads everything, so the owner checks are left out.
func (r *run) serviceUID() (uid int, known bool) {
	if !unixPerms || r.env.UID < 0 {
		return 0, false
	}
	if r.offline() && r.env.UID == 0 && !r.inContainer() {
		return 0, false
	}
	return r.env.UID, true
}

// deniedOffline reports that err is "permission denied" for a caller who could use sudo: the offline mode as an
// ordinary user.
func (r *run) deniedOffline(err error) bool {
	return errors.Is(err, fs.ErrPermission) && r.offline() && r.env.UID > 0
}

// errText is the cause of an error for a message: the system's own words without the path in front, which the
// message already names.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// modeText prints permission bits as chmod takes them: "0700".
func modeText(m fs.FileMode) string { return fmt.Sprintf("%04o", m.Perm()) }

// checkDataDir looks at data_dir (04 §5.1): it exists, is a directory, in a container a mounted volume, owned by
// and writable for the service's user, private, and has room.
func checkDataDir(_ context.Context, r *run) result {
	dir := r.cfg.DataDir
	p := params{"path": dir, "offline": r.offline()}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist) && r.offline():
		return infoResult(codeDataDirNotCreated, p)
	case errors.Is(err, fs.ErrNotExist):
		return failResult(codeDataDirMissing, p, "")
	case err != nil:
		p["error"] = errText(err)
		if r.deniedOffline(err) {
			return failResult(codeDataDirUnreadable, p, fixDataDirSudo)
		}
		return failResult(codeDataDirUnreadable, p, fixDataDirOwner)
	case !fi.IsDir():
		return failResult(codeDataDirNotDir, p, fixDataDirNotDir)
	}
	p["mode"] = modeText(fi.Mode())

	// The container check comes before the owner and writable checks, as at startup (04 §5.1): a container without
	// a volume is told to mount one, not to chown a directory that is lost with the container.
	if r.inContainer() {
		mounted, merr := r.env.Host.DataDirMounted(dir)
		if !mounted {
			if merr != nil {
				p["error"] = merr.Error()
			}
			if r.env.Host.AllowEphemeralData() {
				return warnResult(codeDataDirEphemeralAllowed, p, fixDataDirMount)
			}
			return failResult(codeDataDirNotMounted, p, fixDataDirMount)
		}
	}

	// The owner and the uid go into the params only where they are the finding: a result that is fine reads the
	// same on every machine.
	if service, known := r.serviceUID(); known {
		if owner, ok := fileOwner(fi); ok && owner != service {
			p["owner"], p["uid"] = owner, service
			return failResult(codeDataDirWrongOwner, p, fixDataDirOwner)
		}
		if err := writable(dir); err != nil {
			p["error"], p["uid"] = err.Error(), service
			return failResult(codeDataDirNotWritable, p, fixDataDirOwner)
		}
	}

	free, freeErr := r.env.DiskFree(dir)
	if freeErr == nil {
		p["freeBytes"] = int64(min(free, 1<<62)) //nolint:gosec // G115: bounded just here
	}
	switch {
	case unixPerms && fi.Mode().Perm()&0o077 != 0:
		return warnResult(codeDataDirModeWide, p, fixDataDirMode)
	case freeErr == nil && free < lowDiskBytes:
		return warnResult(codeDataDirLowSpace, p, fixDataDirSpace)
	}
	return okResult(codeDataDirOK, p)
}

// checkSecrets looks at secrets.json (04 §5.2): present, owned by the service's user, parseable, and private.
func checkSecrets(_ context.Context, r *run) result {
	paths := r.cfg.Paths()
	path := paths.Secrets
	p := params{"path": path, "offline": r.offline()}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Before the first start there is neither a database nor a secrets file, and the server makes both. A
		// database without its keys is another matter: new keys sign everyone out.
		if _, dbErr := os.Stat(paths.DB); r.offline() && errors.Is(dbErr, fs.ErrNotExist) {
			return infoResult(codeSecretsNotCreated, p)
		}
		return failResult(codeSecretsMissing, p, fixSecretsRestore)
	case err != nil:
		return r.secretsUnreadable(p, err)
	}
	p["mode"] = modeText(fi.Mode())
	if service, known := r.serviceUID(); known {
		if owner, ok := fileOwner(fi); ok && owner != service {
			p["owner"], p["uid"] = owner, service
			return failResult(codeSecretsWrongOwner, p, fixSecretsOwner)
		}
	}
	if err := config.InspectSecrets(path); err != nil {
		var oe *config.OperatorError
		if errors.As(err, &oe) && oe.Reason == config.ReasonSecretsCorrupt {
			p["error"] = "not a regular file"
			if oe.Err != nil {
				p["error"] = oe.Err.Error()
			}
			return failResult(codeSecretsCorrupt, p, fixSecretsRestore)
		}
		return r.secretsUnreadable(p, err)
	}
	if unixPerms && fi.Mode().Perm()&^0o600 != 0 {
		return warnResult(codeSecretsModeWide, p, fixSecretsMode)
	}
	return okResult(codeSecretsOK, p)
}

func (r *run) secretsUnreadable(p params, err error) result {
	p["error"] = errText(err)
	if r.deniedOffline(err) {
		return failResult(codeSecretsUnreadable, p, fixSecretsSudo)
	}
	return failResult(codeSecretsUnreadable, p, fixSecretsOwner)
}

// checkSchema compares the database's schema with the one this binary knows. A running server has migrated its
// database at startup (04 §6.1 step 4), so there the check only reports the version. Offline it reads the file
// through DBFiles, read-only and immutable, which never creates -wal or -shm files (04 §13.1), and explains a
// refusal to start: a schema newer than the binary, a history that is not this build's, a damaged file.
func checkSchema(ctx context.Context, r *run) result {
	binary, binaryKnown := 0, false
	if f := r.env.DB.LatestSchemaVersion; f != nil {
		binary, binaryKnown = f(), true
	}
	if !r.offline() {
		if r.live.SchemaVersion == 0 {
			return skipResult(codeSkipNotReported, nil)
		}
		p := params{"version": r.live.SchemaVersion}
		if binaryKnown {
			p["binary"] = binary
		}
		return okResult(codeSchemaOK, p)
	}

	path := r.cfg.Paths().DB
	p := params{"path": path}
	if r.env.DB.Inspect == nil {
		return skipResult(codeSkipNotReported, nil)
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return infoResult(codeSchemaNoDB, p)
	}
	info, err := r.env.DB.Inspect(ctx, path)
	if err != nil {
		p["error"] = errText(err)
		if r.deniedOffline(err) {
			return failResult(codeSchemaUnreadable, p, fixSchemaSudo)
		}
		return failResult(codeSchemaUnreadable, p, fixSchemaRestoreBackup)
	}
	p["version"] = info.SchemaVersion
	if binaryKnown {
		p["binary"] = binary
	}
	if info.LastAppVersion != "" {
		p["lastAppVersion"] = info.LastAppVersion
	}
	switch {
	case info.Integrity != nil:
		p["error"] = info.Integrity.Error()
		return failResult(codeSchemaCorrupt, p, fixSchemaRestoreBackup)
	case info.TooNewMessage != "" || (binaryKnown && info.SchemaVersion > binary):
		if info.TooNewMessage != "" {
			p["storeMessage"] = info.TooNewMessage
		}
		if info.TooNewBackup != "" {
			p["backup"] = info.TooNewBackup
			return failResult(codeSchemaNewer, p, fixSchemaRestore)
		}
		return failResult(codeSchemaNewer, p, fixSchemaUpgrade)
	case !info.HistoryOK:
		return failResult(codeSchemaHistoryMismatch, p, fixSchemaSameBuild)
	case binaryKnown && info.SchemaVersion < binary:
		return okResult(codeSchemaOlder, p)
	}
	return okResult(codeSchemaOK, p)
}

// RestoreCommand is the command that restores a database file while the server is stopped (04 §6.1 step 4, §6.3,
// §12.6): the systemd form, or the Docker form in a container, where the stopped service's container is gone and a
// one-off container does the restore. The server prints the same command when it refuses to start on a newer
// schema.
func RestoreCommand(backup string, container bool) string {
	arg := shellQuote(backup)
	if container {
		return "docker compose stop && docker compose run --rm isshoni admin restore --offline " + arg +
			" && docker compose up -d"
	}
	return "sudo -u isshoni isshoni admin restore --offline " + arg + " && sudo systemctl start isshoni"
}

// shellQuote returns s as one word of a shell command: as it is when it has only plain characters, in single quotes
// otherwise.
func shellQuote(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@%+", r)
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !plain(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
