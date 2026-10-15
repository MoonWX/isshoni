package server_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
)

// reexecStageEnv tells the helper process of TestReexec which half of the re-exec it is. Its name is not an
// ISSHONI_* one: the config would warn about those.
const reexecStageEnv = "TEST_REEXEC_STAGE"

// TestReexec runs the re-exec of 04 §6.5 for real, in a process of its own: this test binary, started as the
// helper below. The helper calls server.Reexec, and the process that answers afterwards has the same PID, the same
// arguments and the environment as the first one left it.
//
// The second run starts the helper under the bare name "isshoni", which names no file: that is argv[0] under the
// systemd unit, and a re-exec by os.Args[0] would fail there. Reexec goes by os.Executable.
func TestReexec(t *testing.T) {
	if runtime.GOOS == "windows" {
		if err := server.Reexec(); !errors.Is(err, server.ErrNoReexec) {
			t.Fatalf("Reexec on Windows = %v, want ErrNoReexec", err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^reexec (first|second) pid=(\d+) arg0=(\S+) args=(\d+)$`)
	for _, arg0 := range []string{exe, "isshoni"} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestReexecHelper$", "-test.count=1")
		cmd.Args[0] = arg0
		cmd.Dir = t.TempDir() // nothing here is named isshoni
		cmd.Env = append(os.Environ(), reexecStageEnv+"=first",
			"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("the helper started as %q: %v\n%s", arg0, err, out)
		}
		stages := line.FindAllStringSubmatch(string(out), -1)
		if len(stages) != 2 || stages[0][1] != "first" || stages[1][1] != "second" {
			t.Fatalf("the helper started as %q printed:\n%s\nwant one line of the first run and one of the second", arg0, out)
		}
		first, second := stages[0], stages[1]
		if first[2] != second[2] || first[2] != strconv.Itoa(cmd.Process.Pid) {
			t.Errorf("started as %q: PIDs %s and %s, want both to be the process's %d", arg0, first[2], second[2], cmd.Process.Pid)
		}
		if first[3] != arg0 || second[3] != arg0 || first[4] != second[4] {
			t.Errorf("started as %q: argv[0] %q then %q, %s then %s arguments; want the same command line", arg0, first[3], second[3], first[4], second[4])
		}
	}
}

// TestReexecDeletedBinary: on Linux the re-exec finds the program even when the file it was started from is gone,
// as after an upgrade that removed the binary instead of replacing it: os.Executable then names a path that no
// longer exists, and Reexec runs /proc/self/exe, the running program itself (04 §6.5). The helper is a copy of this
// test binary that deletes itself before it re-execs.
func TestReexecDeletedBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/exe is Linux's")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copied := filepath.Join(dir, "isshoni-upgraded-away")
	if err := os.WriteFile(copied, data, 0o700); err != nil { //nolint:gosec // G306: an executable, for the test's own user
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, copied, "-test.run=^TestReexecHelper$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), reexecStageEnv+"=first", reexecDeleteEnv+"=1",
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper: %v\n%s", err, out)
	}
	stages := regexp.MustCompile(`(?m)^reexec (first|second) pid=(\d+) `).FindAllStringSubmatch(string(out), -1)
	if len(stages) != 2 || stages[0][1] != "first" || stages[1][1] != "second" || stages[0][2] != stages[1][2] {
		t.Fatalf("the helper printed:\n%s\nwant a first and a second run in one process", out)
	}
	if _, err := os.Stat(copied); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the helper's binary is still there (%v): the test did not test the fallback", err)
	}
}

// reexecDeleteEnv makes the helper of TestReexecDeletedBinary delete its own executable before it re-execs.
const reexecDeleteEnv = "TEST_REEXEC_DELETE"

// TestReexecHelper is the process that TestReexec starts; in an ordinary test run it does nothing.
func TestReexecHelper(t *testing.T) {
	stage := os.Getenv(reexecStageEnv)
	if stage == "" {
		t.Skip("the helper process of TestReexec")
	}
	_, _ = os.Stdout.WriteString("reexec " + stage + " pid=" + strconv.Itoa(os.Getpid()) + " arg0=" + os.Args[0] +
		" args=" + strconv.Itoa(len(os.Args)) + "\n")
	if stage != "first" {
		return
	}
	if os.Getenv(reexecDeleteEnv) != "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(exe); err != nil {
			t.Fatal(err)
		}
	}
	// The new process must find the environment as this one leaves it.
	if err := os.Setenv(reexecStageEnv, "second"); err != nil {
		t.Fatal(err)
	}
	err := server.Reexec()
	t.Fatalf("Reexec came back: %v", err)
}
