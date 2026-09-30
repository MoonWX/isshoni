package config

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHost is a Host with env as its environment and a fresh temp dir as its root.
func fakeHost(t *testing.T, env ...string) Host {
	t.Helper()
	return Host{Environ: append([]string{}, env...), Root: t.TempDir()}
}

// writeHostFile writes a machine file (p like "/proc/self/mountinfo") under h's root.
func writeHostFile(t *testing.T, h Host, p, content string) {
	t.Helper()
	path := h.file(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // G703: under the test's temp root
		t.Fatal(err)
	}
}

// mountLine is a mountinfo line with mount point mp (escaped as the kernel does).
func mountLine(id int, mp string) string {
	mp = strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(filepath.ToSlash(mp))
	return fmt.Sprintf("%d 1 8:1 /volumes/v%d %s rw,relatime - ext4 /dev/sda1 rw\n", id, id, mp)
}

// realDir creates dir under a temp dir and returns it with symlinks resolved (macOS temp dirs are under a symlink).
func realDir(t *testing.T, elem ...string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(append([]string{base}, elem...)...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// captureLog returns a logger that writes text lines to the returned buffer.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

func TestContainerDetection(t *testing.T) {
	tests := []struct {
		name  string
		env   []string
		files []string
		want  ContainerKind
	}{
		{"nothing", nil, nil, ContainerNone},
		{"unrelated env", []string{"PATH=/bin", "ISSHONI_IN_CONTAINER="}, nil, ContainerNone},
		{"switch off", []string{EnvInContainer + "=0"}, nil, ContainerNone},
		{"switch 1", []string{EnvInContainer + "=1"}, nil, ContainerOther},
		{"switch true", []string{EnvInContainer + "=true"}, nil, ContainerOther},
		{"switch set twice, last wins", []string{EnvInContainer + "=1", EnvInContainer + "=0"}, nil, ContainerNone},
		{"dockerenv", nil, []string{"/.dockerenv"}, ContainerDocker},
		{"dockerenv and switch", []string{EnvInContainer + "=1"}, []string{"/.dockerenv"}, ContainerDocker},
		{"containerenv", nil, []string{"/run/.containerenv"}, ContainerPodman},
		{"containerenv wins", nil, []string{"/run/.containerenv", "/.dockerenv"}, ContainerPodman},
		{"container=podman", []string{"container=podman"}, nil, ContainerPodman},
		{"container=docker", []string{"container=docker"}, nil, ContainerDocker},
		{"container=systemd-nspawn", []string{"container=systemd-nspawn"}, nil, ContainerOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := fakeHost(t, tt.env...)
			for _, f := range tt.files {
				writeHostFile(t, h, f, "")
			}
			if got := h.Container(); got != tt.want {
				t.Errorf("Container() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllowEphemeralData(t *testing.T) {
	for env, want := range map[string]bool{"": false, "0": false, "1": true, "true": true, "yes": false} {
		h := Host{Environ: []string{EnvAllowEphemeralData + "=" + env}}
		if got := h.AllowEphemeralData(); got != want {
			t.Errorf("%s=%q: AllowEphemeralData() = %v, want %v", EnvAllowEphemeralData, env, got, want)
		}
	}
}

func TestDataDirMounted(t *testing.T) {
	skipOnWindows(t, "mount tables are Linux's")
	dir := realDir(t, "var", "lib", "isshoni")
	parent := filepath.Dir(dir)
	tests := []struct {
		name      string
		mountinfo string
		want      bool
	}{
		{"root only", mountLine(1, "/"), false},
		{"exact", mountLine(1, "/") + mountLine(2, dir), true},
		{"parent", mountLine(1, "/") + mountLine(2, parent), true},
		{"sibling with the same prefix", mountLine(1, "/") + mountLine(2, dir+"-old"), false},
		{"child mount only", mountLine(1, "/") + mountLine(2, filepath.Join(dir, "certmagic")), false},
		{"trailing slash", mountLine(1, "/") + mountLine(2, dir+"/"), true},
		{"malformed lines around", "garbage\n\n1 2 3\n" + mountLine(2, dir) + "4 5 6 7 relative - x\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := fakeHost(t)
			writeHostFile(t, h, "/proc/self/mountinfo", tt.mountinfo)
			got, err := h.DataDirMounted(dir)
			if err != nil || got != tt.want {
				t.Errorf("DataDirMounted = %v, %v; want %v", got, err, tt.want)
			}
		})
	}

	t.Run("escaped mount point", func(t *testing.T) {
		spaced := realDir(t, "isshoni data")
		h := fakeHost(t)
		writeHostFile(t, h, "/proc/self/mountinfo", mountLine(1, "/")+mountLine(2, spaced))
		if got, err := h.DataDirMounted(spaced); !got || err != nil {
			t.Errorf("DataDirMounted(%q) = %v, %v", spaced, got, err)
		}
	})
	t.Run("symlink to a mounted dir", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Skip("no symlinks here:", err)
		}
		h := fakeHost(t)
		writeHostFile(t, h, "/proc/self/mountinfo", mountLine(1, "/")+mountLine(2, dir))
		if got, err := h.DataDirMounted(link); !got || err != nil {
			t.Errorf("DataDirMounted(link) = %v, %v", got, err)
		}
	})
	t.Run("no mount table", func(t *testing.T) {
		if _, err := fakeHost(t).DataDirMounted(dir); err == nil {
			t.Error("no error without a mountinfo file")
		}
	})
}

// Realistic mount tables: a container without a volume, with a named volume at /var/lib/isshoni, and with a bind
// mount on a parent (whose host path has an escaped space).
func TestDataDirMountedFixtures(t *testing.T) {
	skipOnWindows(t, "mount tables are Linux's")
	for file, want := range map[string]bool{"docker-no-volume": false, "docker-named-volume": true, "podman-parent-bind": true} {
		data, err := os.ReadFile(filepath.Join("testdata", "mountinfo", file))
		if err != nil {
			t.Fatal(err)
		}
		h := fakeHost(t)
		writeHostFile(t, h, "/proc/self/mountinfo", string(data))
		if got, err := h.DataDirMounted("/var/lib/isshoni"); got != want || err != nil {
			t.Errorf("%s: DataDirMounted = %v, %v; want %v", file, got, err, want)
		}
	}
}

func TestUnescapeMountPath(t *testing.T) {
	for in, want := range map[string]string{
		`/plain`:           "/plain",
		`/a\040b`:          "/a b",
		`/tab\011nl\012`:   "/tab\tnl\n",
		`/back\134slash`:   `/back\slash`,
		`/short\04`:        `/short\04`,
		`/not\08x`:         `/not\08x`,
		`/end\`:            `/end\`,
		`\040\040`:         "  ",
		`/mixed\040\134\x`: `/mixed \\x`,
	} {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// containerHost is a fake Docker host (/.dockerenv, ISSHONI_IN_CONTAINER=1) whose mount table has only mounts.
func containerHost(t *testing.T, env []string, mounts ...string) Host {
	t.Helper()
	h := fakeHost(t, append([]string{EnvInContainer + "=1"}, env...)...)
	writeHostFile(t, h, "/.dockerenv", "")
	info := mountLine(1, "/") + mountLine(2, "/etc/hosts") + mountLine(3, "/run/isshoni")
	for i, m := range mounts {
		info += mountLine(10+i, m)
	}
	writeHostFile(t, h, "/proc/self/mountinfo", info)
	return h
}

// dataDirConfig is a config whose data_dir is dir and whose admin socket is under a temp dir.
func dataDirConfig(t *testing.T, dir string) *Config {
	t.Helper()
	return &Config{DataDir: dir, Listen: Listen{AdminSocket: filepath.Join(t.TempDir(), "run", "admin.sock")}}
}

// The acceptance check of S25: in a container, a data_dir that is not a mount fails with ErrNeedsOperator (serve
// exits 78) and the fix text, even when PrepareDataDir just created it; ISSHONI_ALLOW_EPHEMERAL_DATA=1 lets it pass.
func TestPrepareDataDirContainerVolume(t *testing.T) {
	skipOnWindows(t, "containers are Linux's")
	base := realDir(t)
	dir := filepath.Join(base, "var", "lib", "isshoni")

	t.Run("no volume", func(t *testing.T) {
		d := filepath.Join(dir, "a")
		err := PrepareDataDir(t.Context(), dataDirConfig(t, d), containerHost(t, nil), nil)
		if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonDataNotMounted {
			t.Fatalf("PrepareDataDir = %v, want %s", err, ReasonDataNotMounted)
		}
		for _, s := range []string{
			d + " is not a mounted volume. Your data would be lost when the container is removed.\n",
			"fix: Mount a volume at " + d + " (see compose.yaml), e.g. -v isshoni-data:" + d,
		} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("error lacks %q:\n%v", s, err)
			}
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("data_dir was not created before the check: %v", err)
		}
	})
	t.Run("no volume, ephemeral allowed", func(t *testing.T) {
		d := filepath.Join(dir, "b")
		log, buf := captureLog()
		h := containerHost(t, []string{EnvAllowEphemeralData + "=1"})
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, d), h, log); err != nil {
			t.Fatalf("PrepareDataDir = %v", err)
		}
		if !strings.Contains(buf.String(), "data-volume check is off") {
			t.Errorf("no warning logged:\n%s", buf.String())
		}
	})
	t.Run("volume at data_dir", func(t *testing.T) {
		d := filepath.Join(dir, "c")
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, d), containerHost(t, nil, d), nil); err != nil {
			t.Fatalf("PrepareDataDir = %v", err)
		}
	})
	t.Run("volume on a parent", func(t *testing.T) {
		d := filepath.Join(dir, "d")
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, d), containerHost(t, nil, filepath.Dir(d)), nil); err != nil {
			t.Fatalf("PrepareDataDir = %v", err)
		}
	})
	t.Run("no mount table", func(t *testing.T) {
		d := filepath.Join(dir, "e")
		h := fakeHost(t, EnvInContainer+"=1")
		err := PrepareDataDir(t.Context(), dataDirConfig(t, d), h, nil)
		if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonDataNotMounted ||
			!strings.Contains(err.Error(), "can't check") {
			t.Fatalf("PrepareDataDir = %v", err)
		}
	})
	t.Run("not in a container: no check", func(t *testing.T) {
		d := filepath.Join(dir, "f")
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, d), fakeHost(t), nil); err != nil {
			t.Fatalf("PrepareDataDir = %v", err)
		}
	})
}

func TestPrepareDataDirLayout(t *testing.T) {
	skipOnWindows(t, "Unix modes")
	old := SetPrivateUmask()
	defer setUmask(old)

	t.Run("creates", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b", "data")
		c := dataDirConfig(t, dir)
		log, buf := captureLog()
		if err := PrepareDataDir(t.Context(), c, fakeHost(t), log); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Dir(c.Listen.AdminSocket), 0o700)
		if strings.Contains(buf.String(), "level=WARN") && geteuid() != 0 {
			t.Errorf("unexpected warning:\n%s", buf.String())
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("the write check left %v", entries)
		}
	})
	t.Run("tightens", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the point of the test
			t.Fatal(err)
		}
		log, buf := captureLog()
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, dir), fakeHost(t), log); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dir, 0o700)
		if !strings.Contains(buf.String(), "changed its mode to 0700") || !strings.Contains(buf.String(), "was=0755") {
			t.Errorf("no warning:\n%s", buf.String())
		}
	})
	t.Run("keeps an existing socket directory", func(t *testing.T) {
		c := dataDirConfig(t, filepath.Join(t.TempDir(), "data"))
		sockDir := filepath.Dir(c.Listen.AdminSocket)
		if err := os.Mkdir(sockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sockDir, 0o755); err != nil { //nolint:gosec // like systemd's RuntimeDirectory
			t.Fatal(err)
		}
		if err := PrepareDataDir(t.Context(), c, fakeHost(t), nil); err != nil {
			t.Fatal(err)
		}
		assertMode(t, sockDir, 0o755)
	})
	t.Run("not a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "data")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := PrepareDataDir(t.Context(), dataDirConfig(t, path), fakeHost(t), nil)
		if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonDataDirNotWritable ||
			!strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("PrepareDataDir = %v", err)
		}
	})
	t.Run("empty data_dir", func(t *testing.T) {
		err := PrepareDataDir(t.Context(), &Config{}, fakeHost(t), nil)
		if !errors.Is(err, ErrNeedsOperator) {
			t.Errorf("PrepareDataDir = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := PrepareDataDir(ctx, dataDirConfig(t, t.TempDir()), fakeHost(t), nil); !errors.Is(err, context.Canceled) {
			t.Errorf("PrepareDataDir = %v", err)
		}
	})
}

func TestPrepareDataDirNotWritable(t *testing.T) {
	skipOnWindows(t, "Unix modes")
	if geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	t.Run("existing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // so TempDir can clean up
		err := PrepareDataDir(t.Context(), dataDirConfig(t, dir), fakeHost(t), nil)
		assertNotWritable(t, err, dir, "is not writable by isshoni")
	})
	t.Run("can't be created", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "lib")
		if err := os.Mkdir(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // so TempDir can clean up
		dir := filepath.Join(parent, "isshoni")
		err := PrepareDataDir(t.Context(), dataDirConfig(t, dir), fakeHost(t), nil)
		assertNotWritable(t, err, dir, "can't be created")
	})
}

func assertNotWritable(t *testing.T, err error, dir, what string) {
	t.Helper()
	if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonDataDirNotWritable {
		t.Fatalf("PrepareDataDir = %v, want %s", err, ReasonDataDirNotWritable)
	}
	msg := err.Error()
	ids := fmt.Sprintf("uid %d, gid %d", os.Getuid(), os.Getgid())
	for _, s := range []string{dir + " (data_dir) " + what, "permission denied", ids,
		"fix: run: sudo chown -R " + ownerNames(os.Getuid(), os.Getgid()) + " " + dir} {
		if !strings.Contains(msg, s) {
			t.Errorf("error lacks %q:\n%s", s, msg)
		}
	}
}

func TestPrepareDataDirRootWarning(t *testing.T) {
	skipOnWindows(t, "Unix uids")
	defer func(f func() int) { geteuid = f }(geteuid)
	geteuid = func() int { return 0 }
	for _, inContainer := range []bool{false, true} {
		var h Host
		if inContainer {
			h = containerHost(t, []string{EnvAllowEphemeralData + "=1"})
		} else {
			h = fakeHost(t)
		}
		log, buf := captureLog()
		if err := PrepareDataDir(t.Context(), dataDirConfig(t, t.TempDir()), h, log); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(buf.String(), "running as root"); got == inContainer {
			t.Errorf("in a container %v: root warning logged = %v:\n%s", inContainer, got, buf.String())
		}
	}
}

func TestOwnerFix(t *testing.T) {
	defer func(u, g func() int) { getuid, getgid = u, g }(getuid, getgid)
	getuid = func() int { return 65532 }
	getgid = func() int { return 65532 }
	docker := fakeHost(t)
	writeHostFile(t, docker, "/.dockerenv", "")
	if got, want := ownerFix(docker, "/var/lib/isshoni", true),
		"on the container's host, run: sudo chown -R 65532:65532 <the host path of /var/lib/isshoni>"; got != want {
		t.Errorf("container fix = %q, want %q", got, want)
	}
	if got, want := ownerFix(fakeHost(t), "/var/lib/isshoni/secrets.json", false),
		"run: sudo chown "+ownerNames(65532, 65532)+" /var/lib/isshoni/secrets.json"; got != want {
		t.Errorf("fix = %q, want %q", got, want)
	}
	getuid = func() int { return -1 } // Windows
	if got := ownerFix(fakeHost(t), `C:\data`, true); strings.Contains(got, "chown") {
		t.Errorf("Windows fix = %q", got)
	}
}

func TestOwnerNames(t *testing.T) {
	// An id that no account has stays a number.
	if got := ownerNames(3999999, 3999998); got != "3999999:3999998" {
		t.Errorf("ownerNames = %q", got)
	}
}

func TestSetPrivateUmask(t *testing.T) {
	skipOnWindows(t, "no umask")
	old := SetPrivateUmask()
	defer setUmask(old)
	if again := SetPrivateUmask(); again != 0o077 {
		t.Errorf("previous umask = %#o, want 0077", again)
	}
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "f"), os.O_CREATE|os.O_WRONLY, 0o666) //nolint:gosec // the umask decides
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	assertMode(t, filepath.Join(dir, "f"), 0o600)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o777); err != nil { //nolint:gosec // the umask decides
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(dir, "d"), 0o700)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if !unixPerms {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s: mode %#o, want %#o", filepath.Base(path), got, want)
	}
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	return (&Config{DataDir: t.TempDir()}).Paths()
}

func TestLockDataDir(t *testing.T) {
	p := testPaths(t)
	l1, err := LockDataDir(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, p.Lock, 0o600)
	_, err = LockDataDir(t.Context(), p)
	if !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("second LockDataDir = %v, want ErrDataDirLocked", err)
	}
	if want := "another isshoni process is using " + p.DataDir; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	l2, err := LockDataDir(t.Context(), p)
	if err != nil {
		t.Fatalf("LockDataDir after Close = %v", err)
	}
	_ = l2.Close()
	if _, err := os.Stat(p.Lock); err != nil {
		t.Errorf("the lock file was removed: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LockDataDir(ctx, p); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled LockDataDir = %v", err)
	}
	missing := (&Config{DataDir: filepath.Join(t.TempDir(), "missing")}).Paths()
	if _, err := LockDataDir(t.Context(), missing); err == nil || errors.Is(err, ErrDataDirLocked) {
		t.Errorf("LockDataDir without a data dir = %v", err)
	}
}

const lockHelperEnv = "CONFIG_TEST_LOCK_HELPER_DIR"

// TestLockHelperProcess is not a test: TestLockDataDirOtherProcess runs the test binary with it to hold the lock in
// another process until its stdin closes.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	l, err := LockDataDir(context.Background(), (&Config{DataDir: dir}).Paths())
	if err != nil {
		fmt.Println("error:", err) //nolint:forbidigo // the helper's report to its parent
		os.Exit(1)
	}
	fmt.Println("locked") //nolint:forbidigo // as above
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = l.Close()
	os.Exit(0)
}

func TestLockDataDirOtherProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	p := testPaths(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockHelperProcess$") //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+p.DataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("helper: %q", line)
	}
	if _, err := LockDataDir(t.Context(), p); !errors.Is(err, ErrDataDirLocked) {
		t.Errorf("LockDataDir while another process holds it = %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	l, err := LockDataDir(t.Context(), p)
	if err != nil {
		t.Fatalf("LockDataDir after the other process exited = %v", err)
	}
	_ = l.Close()
}

func TestOperatorError(t *testing.T) {
	cause := errors.New("cause")
	e := &OperatorError{Reason: ReasonSecretsCorrupt, Path: "/x", Message: "/x is broken", Fix: "run: fix /x", Err: cause}
	if got, want := e.Error(), "/x is broken.\n  fix: run: fix /x"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	wrapped := fmt.Errorf("serve: %w", e)
	if !errors.Is(wrapped, ErrNeedsOperator) || !errors.Is(wrapped, cause) || ReasonOf(wrapped) != ReasonSecretsCorrupt {
		t.Errorf("wrapping lost something: %v", wrapped)
	}
	if ReasonOf(cause) != "" || ReasonOf(nil) != "" {
		t.Error("ReasonOf of a plain error is not empty")
	}
	if !errors.Is(&OperatorError{Message: "m"}, ErrNeedsOperator) {
		t.Error("an OperatorError without Err does not wrap ErrNeedsOperator")
	}
	if got := (&OperatorError{Message: "done."}).Error(); got != "done." {
		t.Errorf("Error() without a fix = %q", got)
	}
}
