package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// The data directory (§5.1): serve's startup checks, container detection, the umask and the data-directory lock.
// Paths (paths.go) is the layout.

// ContainerKind is the container runtime the server runs in (§5.1). The values are those of doctor's env.container
// and the connection test's server.container (api.ContainerKind, §7.7, §13.5); the wiring converts.
type ContainerKind string

const (
	ContainerNone   ContainerKind = "none"
	ContainerDocker ContainerKind = "docker"
	ContainerPodman ContainerKind = "podman"
	ContainerOther  ContainerKind = "other" // detected by env only (ISSHONI_IN_CONTAINER, container=…), e.g. containerd
)

// Host is the machine as the data-directory checks see it. The zero value is the running process: its environment
// and the real filesystem. Tests point Environ and Root at fakes.
type Host struct {
	// Environ is the environment in os.Environ form; nil means os.Environ(). Read: ISSHONI_IN_CONTAINER,
	// ISSHONI_ALLOW_EPHEMERAL_DATA, container (set by Podman and systemd-nspawn) and GOMEMLIMIT.
	Environ []string
	// Root is the directory the machine's files are looked up under: /.dockerenv, /run/.containerenv,
	// /proc/self/mountinfo, /proc/self/cgroup and /sys/fs/cgroup. "" means "/".
	Root string
}

// getenv returns the value of the environment variable name ("" when unset; the last occurrence wins).
func (h Host) getenv(name string) string {
	env := h.Environ
	if env == nil {
		env = os.Environ()
	}
	value := ""
	for _, kv := range env {
		if n, v, ok := strings.Cut(kv, "="); ok && n == name {
			value = v
		}
	}
	return value
}

// file returns the path of the machine file p ("/.dockerenv") under Root.
func (h Host) file(p string) string {
	root := h.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// Container detects the container runtime (§5.1): /run/.containerenv (Podman), /.dockerenv (Docker), the
// container variable (Podman and systemd-nspawn set it), or ISSHONI_IN_CONTAINER=1 (06's image sets it). The files
// name the runtime; the variables alone give ContainerOther unless container names docker or podman.
func (h Host) Container() ContainerKind {
	switch {
	case exists(h.file("/run/.containerenv")):
		return ContainerPodman
	case exists(h.file("/.dockerenv")):
		return ContainerDocker
	}
	switch c := h.getenv("container"); c {
	case "":
	case "docker":
		return ContainerDocker
	case "podman":
		return ContainerPodman
	default:
		return ContainerOther
	}
	if envTrue(h.getenv(EnvInContainer)) {
		return ContainerOther
	}
	return ContainerNone
}

// AllowEphemeralData reports whether ISSHONI_ALLOW_EPHEMERAL_DATA=1 turns off the container data-volume check (tests
// only, §4.3).
func (h Host) AllowEphemeralData() bool { return envTrue(h.getenv(EnvAllowEphemeralData)) }

// DataDirMounted reports whether dir is on a mounted volume: the mount point of one of the process's mounts
// (/proc/self/mountinfo) is dir itself or a parent other than / (§5.1). dir is made absolute, and its symlinks are
// resolved when it exists; either form counts. The error is about reading the mount table.
func (h Host) DataDirMounted(dir string) (bool, error) {
	data, err := os.ReadFile(h.file("/proc/self/mountinfo"))
	if err != nil {
		return false, fmt.Errorf("config: reading the mount table: %w", err)
	}
	forms := dirForms(dir)
	for _, mp := range parseMountPoints(data) {
		if mp == "/" {
			continue
		}
		for _, d := range forms {
			if d == mp || strings.HasPrefix(d, mp+"/") {
				return true, nil
			}
		}
	}
	return false, nil
}

// dirForms returns dir made absolute and, when it exists, with its symlinks resolved, both with forward slashes.
func dirForms(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	forms := []string{filepath.ToSlash(abs)}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		forms = append(forms, filepath.ToSlash(real))
	}
	return forms
}

// parseMountPoints returns the mount points (the fifth field) of a mountinfo file, unescaped. Malformed lines are
// skipped.
func parseMountPoints(data []byte) []string {
	var out []string
	for line := range bytes.Lines(data) {
		fields := strings.Fields(string(line))
		if len(fields) < 5 || !strings.HasPrefix(fields[4], "/") {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Clean(unescapeMountPath(fields[4]))))
	}
	return out
}

// unescapeMountPath decodes the octal escapes the kernel writes for space, tab, newline and backslash (\040, \011,
// \012, \134).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// SetPrivateUmask sets the process umask to 0077, so that every file and directory the server creates is private to
// its user (§5.1), and returns the previous umask. serve calls it first, before it creates any file. It does nothing
// on Windows.
func SetPrivateUmask() (previous int) { return setUmask(0o077) }

// PrepareDataDir runs serve's data-directory checks (§5.1, §6.1 step 2), after SetPrivateUmask and before the
// data-directory lock, secrets.json and the store:
//   - data_dir is created (0700, with its parents) if missing;
//   - in a container (Host.Container), data_dir must be on a mounted volume (Host.DataDirMounted), unless
//     ISSHONI_ALLOW_EPHEMERAL_DATA=1 (tests only). The check runs after the directory is created, so a directory
//     that serve just created in a container still fails;
//   - data_dir must be a directory. If the process owns it and it is wider than 0700, it is chmod-ed to 0700 with a
//     warning;
//   - the process must be able to create files in it;
//   - the admin socket's parent directory is created (0700) if missing (ReasonAdminSocketDir when it can't be);
//   - running as root outside a container logs a warning.
//
// A failed check returns an *OperatorError (it wraps ErrNeedsOperator: serve exits 78, restarting can't help) whose
// fix names the process's uid:gid in the form for the environment. Only a cancelled ctx returns another error.
func PrepareDataDir(ctx context.Context, c *Config, h Host, log *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log = componentLogger(log)
	dir := c.DataDir
	if dir == "" {
		return &OperatorError{Reason: ReasonDataDirNotWritable, Message: "data_dir is empty", Fix: "set data_dir"}
	}
	container := h.Container()
	if container == ContainerNone && unixPerms && geteuid() == 0 {
		log.Warn("isshoni is running as root; run it as its own user (install.sh creates the isshoni user)")
	}

	mkErr := os.MkdirAll(dir, 0o700)
	if container != ContainerNone {
		if h.AllowEphemeralData() {
			log.Warn("the container data-volume check is off ("+EnvAllowEphemeralData+
				"=1): the data is lost when the container is removed", "data_dir", dir)
		} else if err := h.checkMounted(dir); err != nil {
			return err
		}
	}
	fi, err := os.Stat(dir)
	switch {
	case err == nil && !fi.IsDir():
		// MkdirAll failed with ENOTDIR or EEXIST; say what is in the way.
		return &OperatorError{
			Reason: ReasonDataDirNotWritable, Path: dir,
			Message: dir + " (data_dir) is not a directory",
			Fix:     "move that file away, or set data_dir to a directory",
		}
	case mkErr != nil:
		return dataDirError(h, dir, "can't be created", mkErr)
	case err != nil:
		return dataDirError(h, dir, "can't be read", err)
	}
	if unixPerms {
		if uid, ok := fileOwner(fi); ok && uid == geteuid() && fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its x bit; 0700 is private
				return dataDirError(h, dir, "can't be made private (chmod 0700)", err)
			}
			log.Warn("data_dir was open to other users; changed its mode to 0700", "data_dir", dir,
				"was", fmt.Sprintf("%#o", fi.Mode().Perm()))
		}
	}
	if err := checkWritable(dir); err != nil {
		return dataDirError(h, dir, "is not writable by isshoni", err)
	}
	if sock := c.Listen.AdminSocket; sock != "" {
		sockDir := filepath.Dir(sock)
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			return adminSocketDirError(h, sockDir, err)
		}
	}
	return nil
}

// adminSocketDirError is the *OperatorError for an admin socket directory that MkdirAll can't create. MkdirAll
// succeeds on an existing directory, so dir (or one of its parents) is missing, and chown alone can't fix it.
func adminSocketDirError(h Host, dir string, err error) error {
	cause := err
	var pe *fs.PathError
	if errors.As(err, &pe) {
		cause = pe.Err // "read-only file system", "permission denied", "not a directory"
	}
	elsewhere := "set listen.admin_socket to a path in a directory isshoni can write"
	fix := elsewhere
	uid, gid := getuid(), getgid()
	switch {
	case uid < 0:
	case h.Container() != ContainerNone && (errors.Is(err, fs.ErrPermission) || isReadOnlyFS(err)):
		// 06's compose files mount a tmpfs at /run/isshoni; a read-only root filesystem without it lands here.
		fix = fmt.Sprintf("mount a tmpfs at %s writable by %d:%d (see compose.yaml), e.g. --tmpfs %s:uid=%d,gid=%d,mode=0750",
			dir, uid, gid, dir, uid, gid)
	case errors.Is(err, fs.ErrPermission):
		fix = fmt.Sprintf("run: sudo mkdir -p %s && sudo chown %s %s, or %s", dir, ownerNames(uid, gid), dir, elsewhere)
	}
	return &OperatorError{
		Reason: ReasonAdminSocketDir, Path: dir, Err: err,
		Message: fmt.Sprintf("%s (the directory of listen.admin_socket) can't be created (%v); isshoni runs as %s",
			dir, cause, processIDs()),
		Fix: fix,
	}
}

// checkMounted is PrepareDataDir's container check: an *OperatorError with ReasonDataNotMounted unless dir is on a
// mounted volume.
func (h Host) checkMounted(dir string) error {
	mounted, err := h.DataDirMounted(dir)
	if mounted {
		return nil
	}
	// 04 §5.1 and 06 §6 quote this text.
	fix := "Mount a volume at " + dir + " (see compose.yaml), e.g. -v isshoni-data:" + dir
	if err != nil {
		return &OperatorError{
			Reason: ReasonDataNotMounted, Path: dir, Err: err,
			Message: "isshoni runs in a container but can't check that " + dir + " is a mounted volume: " + err.Error(),
			Fix:     fix,
		}
	}
	return &OperatorError{
		Reason: ReasonDataNotMounted, Path: dir,
		Message: dir + " is not a mounted volume. Your data would be lost when the container is removed",
		Fix:     fix,
	}
}

// dataDirError is the *OperatorError for a data directory the process can't use; what follows the path.
func dataDirError(h Host, dir, what string, err error) error {
	fix := "check that " + dir + " is a writable directory with free space"
	switch {
	case errors.Is(err, fs.ErrPermission):
		fix = ownerFix(h, dir, true)
		what += " (permission denied)"
	case isReadOnlyFS(err):
		fix = "put data_dir on a writable filesystem"
		if h.Container() != ContainerNone {
			fix = "mount a writable volume at " + dir + " (see compose.yaml), e.g. -v isshoni-data:" + dir
		}
		what += " (read-only filesystem)"
	}
	return &OperatorError{
		Reason: ReasonDataDirNotWritable, Path: dir, Err: err,
		Message: fmt.Sprintf("%s (data_dir) %s; isshoni runs as %s", dir, what, processIDs()),
		Fix:     fix,
	}
}

// checkWritable creates and removes a file in dir.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".isshoni-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	return errors.Join(f.Close(), os.Remove(name))
}

// ownerFix is the fix for a data file or directory the process can't use: give it to the process's real uid:gid
// (§5.1), in the form for the environment: the user and group names outside a container ("run: sudo chown -R
// isshoni:isshoni /var/lib/isshoni"), the numbers on the container's host ("sudo chown -R 65532:65532 <host path>",
// or 0:0 with compose.host.yaml). recursive adds -R.
func ownerFix(h Host, path string, recursive bool) string {
	uid, gid := getuid(), getgid()
	if uid < 0 {
		return "let the user that runs isshoni write to " + path
	}
	r := ""
	if recursive {
		r = "-R "
	}
	if h.Container() != ContainerNone {
		return fmt.Sprintf("on the container's host, run: sudo chown %s%d:%d <the host path of %s>", r, uid, gid, path)
	}
	return fmt.Sprintf("run: sudo chown %s%s %s", r, ownerNames(uid, gid), path)
}

// ownerNames is "user:group" for uid and gid, with numbers for names that don't resolve.
func ownerNames(uid, gid int) string {
	u, g := strconv.Itoa(uid), strconv.Itoa(gid)
	if usr, err := user.LookupId(u); err == nil && usr.Username != "" {
		u = usr.Username
	}
	if grp, err := user.LookupGroupId(g); err == nil && grp.Name != "" {
		g = grp.Name
	}
	return u + ":" + g
}

// processIDs describes the process's real uid and gid for messages: "uid 998, gid 998".
func processIDs() string {
	if getuid() < 0 {
		return "the current user"
	}
	return fmt.Sprintf("uid %d, gid %d", getuid(), getgid())
}

// Test seams for the process's ids.
var (
	geteuid = os.Geteuid
	getuid  = os.Getuid
	getgid  = os.Getgid
)

// ErrDataDirLocked is wrapped by LockDataDir's error when another process holds the data-directory lock. serve
// exits 1 on it, the offline admin commands 7 (§3.2, §12.6). The error reads "another isshoni process is using
// <data_dir>".
var ErrDataDirLocked = errors.New("config: the data directory is locked by another process")

// errLockHeld is lockFile's error when the lock is taken.
var errLockHeld = errors.New("lock held")

type lockedError struct{ dir string }

func (e *lockedError) Error() string { return "another isshoni process is using " + e.dir }
func (e *lockedError) Unwrap() error { return ErrDataDirLocked }

// DataDirLock is the data-directory lock (§5.1): an exclusive flock (LockFileEx on Windows) on data_dir/isshoni.lock.
// serve holds it for its whole life; the offline admin commands take it too (§12.6). It works across containers that
// share the volume, because they share the kernel. It is released by Close, by the process's exit and by exec.
type DataDirLock struct {
	mu sync.Mutex
	f  *os.File
}

// LockDataDir takes the data-directory lock p.Lock without waiting. The lock file is created (0600) if missing and
// never removed. When another process, or another DataDirLock of this process, holds the lock, the error wraps
// ErrDataDirLocked. A lock file the process may not open (a root run created it) is an *OperatorError with
// ReasonDataDirNotWritable (serve exits 78, §3.2): restarting can't fix it.
func LockDataDir(ctx context.Context, p Paths) (*DataDirLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.Lock, os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, fs.ErrPermission) {
		return nil, &OperatorError{
			Reason: ReasonDataDirNotWritable, Path: p.Lock, Err: err,
			Message: p.Lock + " can't be opened (permission denied); isshoni runs as " + processIDs(),
			Fix:     ownerFix(Host{}, p.DataDir, true),
		}
	}
	if err != nil {
		return nil, fmt.Errorf("config: opening the data-directory lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, &lockedError{dir: p.DataDir}
		}
		return nil, fmt.Errorf("config: locking %s: %w", p.Lock, err)
	}
	return &DataDirLock{f: f}, nil
}

// Close releases the lock. Calling it again does nothing.
func (l *DataDirLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("config: releasing the data-directory lock: %w", err)
	}
	return nil
}

// exists reports whether path exists (any type).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// envTrue reports whether an env-only switch is on: true or 1 (§4.2's booleans).
func envTrue(v string) bool {
	b, err := parseText(KindBool, v)
	return err == nil && b.(bool)
}

// componentLogger returns log (a discarding logger when nil) with component=config.
func componentLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return log.With("component", "config")
}
