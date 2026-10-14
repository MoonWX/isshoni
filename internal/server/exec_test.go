package server_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	// The new process must find the environment as this one leaves it.
	if err := os.Setenv(reexecStageEnv, "second"); err != nil {
		t.Fatal(err)
	}
	err := server.Reexec()
	t.Fatalf("Reexec came back: %v", err)
}
