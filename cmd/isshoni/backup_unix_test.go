//go:build unix

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/version"
)

// Where a backup may write when something is there already (04 §12.3, review R5): never at the default name, and
// at an --out that the operator named only into a pipe or a character device of the command's own user or root's.

// fifoTap is a FIFO with a reader that waits on it, as the program of another local user would (review R5): its
// read end is open, and a write end of the tap's own keeps the reader from seeing the end of the pipe before the
// command had its chance to write.
type fifoTap struct {
	path string
	w    *os.File
	data chan []byte

	once sync.Once
	got  []byte
}

// newFIFOTap makes a FIFO at path and starts reading it.
func newFIFOTap(t *testing.T, path string) *fifoTap {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// The read end is opened non-blocking, so that the open does not wait for a writer, and then made blocking, so
	// that a read waits for data (Go does not poll a FIFO on every system: on macOS a read would just fail with
	// EAGAIN).
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	r := os.NewFile(uintptr(fd), path)
	w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	tap := &fifoTap{path: path, w: w, data: make(chan []byte, 1)}
	go func() {
		data, _ := io.ReadAll(r)
		_ = r.Close()
		tap.data <- data
	}()
	t.Cleanup(func() { tap.read() })
	return tap
}

// read closes the tap's own write end and returns every byte that came through the pipe.
func (p *fifoTap) read() []byte {
	p.once.Do(func() {
		_ = p.w.Close()
		p.got = <-p.data
	})
	return p.got
}

// setBackupOutOwner replaces the owner check of an existing --out for the rest of the test.
func setBackupOutOwner(t *testing.T, owner func(fs.FileInfo) (int, bool)) {
	t.Helper()
	prev := backupOutOwner
	backupOutOwner = owner
	t.Cleanup(func() { backupOutOwner = prev })
}

// commandUID is the effective uid of a command that runCLIWith runs, unless the test says root.
const commandUID = 1000

// asTheCommandsUser is the real owner, except that what belongs to the user who runs the tests belongs to the
// command's user: runCLIWith runs every command as uid 1000, whoever runs the tests.
func asTheCommandsUser(fi fs.FileInfo) (int, bool) {
	uid, ok := fileOwner(fi)
	if ok && uid == os.Geteuid() {
		uid = commandUID
	}
	return uid, ok
}

// Without --out, nothing that exists at the default name is written into: a FIFO that another user placed at the
// name of the coming seconds is exit 7, and its reader gets nothing.
func TestBackupDefaultNameFIFO(t *testing.T) {
	_, env := startTestServer(t, false)
	dir := t.TempDir()
	t.Chdir(dir)
	// Every name of the seconds around the backup, as the attack of review R5 lays them out.
	now := time.Now()
	var taps []*fifoTap
	for s := -2; s <= 10; s++ {
		name := ops.BackupFileName(version.Version(), now.Add(time.Duration(s)*time.Second))
		taps = append(taps, newFIFOTap(t, filepath.Join(dir, name)))
	}
	code, stdout, stderr := runCLIEnv(t, env, "admin", "backup")
	refused := slices.ContainsFunc(taps, func(tap *fifoTap) bool {
		return stderr == "isshoni admin backup: "+filepath.Base(tap.path)+
			" exists, and a backup never overwrites a file: remove it, or name another --out\n"
	})
	if code != exitRefused || stdout != "" || !refused {
		t.Errorf("admin backup with a FIFO at the default name: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, tap := range taps {
		if got := tap.read(); len(got) != 0 {
			t.Errorf("the reader of %s got %d bytes", filepath.Base(tap.path), len(got))
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != len(taps) {
		t.Errorf("the refused backup left files: %v", entries)
	}

	// A link at the default name is refused as well, even one that leads to a FIFO of the command's own user:
	// only an --out that the operator named may exist. Named, the same link is written through (--out /dev/fd/3
	// is a link too).
	setBackupOutOwner(t, asTheCommandsUser)
	inv := &invocation{stdout: io.Discard, stderr: io.Discard, geteuid: func() int { return commandUID }}
	write := func(w io.Writer) error {
		_, err := io.WriteString(w, "the archive")
		return err
	}
	for _, named := range []bool{false, true} {
		mine := newFIFOTap(t, filepath.Join(t.TempDir(), "mine"))
		link := filepath.Join(t.TempDir(), ops.BackupFileName(version.Version(), now))
		if err := os.Symlink(mine.path, link); err != nil {
			t.Fatal(err)
		}
		var err error
		if named {
			err = writeBackup(inv, link, "unused", write)
		} else {
			err = writeBackup(inv, "", link, write)
		}
		var ee *exitError
		switch got := string(mine.read()); {
		case named && (err != nil || got != "the archive"):
			t.Errorf("--out <a link to the command's user's FIFO>: %v, the reader got %q", err, got)
		case !named && (!errors.As(err, &ee) || ee.code != exitRefused || got != ""):
			t.Errorf("a link at the default name: %v, the reader got %q; want exit 7 and nothing", err, got)
		}
	}
}

// An --out that the operator named and that exists is written into when it is a FIFO or a character device of
// the command's user or root's, as before; any other owner, or one that can't be told, is exit 7 with nothing
// written.
func TestBackupNamedFIFO(t *testing.T) {
	_, env := startTestServer(t, false)
	backup := func(t *testing.T, owner func(fs.FileInfo) (int, bool)) (tap *fifoTap, code int, stdout, stderr string) {
		t.Helper()
		setBackupOutOwner(t, owner)
		tap = newFIFOTap(t, filepath.Join(t.TempDir(), "backup.pipe"))
		code, stdout, stderr = runCLIEnv(t, env, "admin", "backup", "--out", tap.path)
		return tap, code, stdout, stderr
	}
	streamed := func(t *testing.T, owner func(fs.FileInfo) (int, bool)) {
		t.Helper()
		tap, code, stdout, stderr := backup(t, owner)
		if code != exitOK || stderr != "" || stdout != "Wrote "+tap.path+".\n"+keepPrivate+"\n" {
			t.Fatalf("admin backup --out <FIFO>: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		m, names := archiveNames(t, tap.read())
		wantNames := []string{"isshoni.db", "secrets.json", "certmagic/certificates/", "certmagic/certificates/site.crt"}
		if m.Format != ops.BackupFormat || !slices.Equal(names, wantNames) {
			t.Errorf("the archive in the FIFO: manifest %+v, entries %v", m, names)
		}
	}
	refused := func(t *testing.T, owner func(fs.FileInfo) (int, bool), want string) {
		t.Helper()
		tap, code, stdout, stderr := backup(t, owner)
		if code != exitRefused || stdout != "" || stderr != "isshoni admin backup: "+tap.path+want+"\n" {
			t.Errorf("admin backup --out <FIFO>: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if got := tap.read(); len(got) != 0 {
			t.Errorf("the FIFO's reader got %d bytes", len(got))
		}
	}

	t.Run("the command's user's", func(t *testing.T) { streamed(t, asTheCommandsUser) })
	t.Run("root's", func(t *testing.T) { streamed(t, func(fs.FileInfo) (int, bool) { return 0, true }) })
	t.Run("another user's", func(t *testing.T) {
		refused(t, func(fs.FileInfo) (int, bool) { return 4242, true }, " is a pipe or a device of another user (uid 4242), "+
			"and a backup writes only into one of yours or root's: remove it, or name another --out (for stdout, --out -)")
	})
	t.Run("an owner the system can't tell", func(t *testing.T) {
		refused(t, func(fs.FileInfo) (int, bool) { return 0, false }, " exists, and a backup writes into a pipe or a device "+
			"only when it belongs to you or to root, which this system can't tell: name a new file with --out, or use --out -")
	})
	t.Run("another user's once it is open", func(t *testing.T) {
		// What the open reached is checked again: a FIFO swapped in after the first look gets nothing either.
		var calls int
		refused(t, func(fi fs.FileInfo) (int, bool) {
			if calls++; calls == 1 {
				return asTheCommandsUser(fi)
			}
			return 4242, true
		}, " is a pipe or a device of another user (uid 4242), and a backup writes only into one of yours or root's: "+
			"remove it, or name another --out (for stdout, --out -)")
		if calls != 2 {
			t.Errorf("the owner was asked %d times, want before and after the open", calls)
		}
	})

	t.Run("a character device of root's", func(t *testing.T) {
		// The real owner check: /dev/null belongs to root, except in a user namespace that doesn't map the host's
		// root (a rootless container), where it shows the overflow uid and the refusal is right.
		fi, err := os.Stat(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		if uid, ok := fileOwner(fi); !ok || uid != 0 {
			t.Skipf("%s belongs to uid %d here, not to root", os.DevNull, uid)
		}
		code, stdout, stderr := runCLIEnv(t, env, "admin", "backup", "--out", os.DevNull)
		if code != exitOK || stderr != "" || stdout != "Wrote "+os.DevNull+".\n"+keepPrivate+"\n" {
			t.Errorf("admin backup --out %s: exit %d, stdout %q, stderr %q", os.DevNull, code, stdout, stderr)
		}
	})
	t.Run("not a pipe or a device", func(t *testing.T) {
		// A directory, or a link that leads nowhere, exists too.
		setBackupOutOwner(t, asTheCommandsUser)
		dangling := filepath.Join(t.TempDir(), "dangling")
		if err := os.Symlink(filepath.Join(t.TempDir(), "nothing"), dangling); err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{t.TempDir(), dangling} {
			code, stdout, stderr := runCLIEnv(t, env, "admin", "backup", "--out", out)
			if code != exitRefused || stdout != "" || !strings.HasSuffix(stderr, out+" exists, and a backup never overwrites a file: remove it, or name another --out\n") {
				t.Errorf("admin backup --out %s: exit %d, stdout %q, stderr %q", out, code, stdout, stderr)
			}
		}
	})
	t.Run("--out -", func(t *testing.T) {
		// stdout is no file that exists at a path: the owner check has no part in it.
		setBackupOutOwner(t, func(fs.FileInfo) (int, bool) {
			t.Error("--out - asked for an owner")
			return 4242, true
		})
		code, stdout, stderr := runCLIEnv(t, env, "admin", "backup", "--out", "-")
		if code != exitOK || stderr != keepPrivate+"\n" || !strings.HasPrefix(stdout, "\x1f\x8b") {
			t.Errorf("admin backup --out -: exit %d, stderr %q", code, stderr)
		}
	})
}

// fileOwner reads the owner that the system records.
func TestFileOwner(t *testing.T) {
	fi, err := os.Stat(writeTemp(t, "mine", nil))
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := fileOwner(fi); !ok || uid != os.Geteuid() {
		t.Errorf("fileOwner of a file the test made = %d, %v; want %d", uid, ok, os.Geteuid())
	}
}
