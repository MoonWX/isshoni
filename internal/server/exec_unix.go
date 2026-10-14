//go:build unix

package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"syscall"
)

// Reexec replaces the running process by a new run of the same program, with the same arguments and environment
// (04 §6.5): what cmd/isshoni does when Run returns ErrRestartRequested. The PID stays the same, so systemd and
// Docker (where isshoni is PID 1) see no exit, and the new process starts from scratch: it reads the config, takes
// the data-directory lock that the shutdown released, and finds whatever the restart was for (the restored files,
// the rotated keys, the public address). On success it does not return; its error means the process is still the
// old one, with its server shut down, and the caller exits 75 so that the service manager starts a new one.
//
// The program is the one os.Executable names, not os.Args[0]: under the systemd unit argv[0] is the bare word
// "isshoni", which names a file only by way of PATH. After an upgrade that replaced the binary on disk, that path
// holds the new version, and the restart runs it, as a restart by the service manager would. Where the path is
// gone altogether, Linux still has the running program itself, /proc/self/exe.
func Reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("server: re-exec: %w", err)
	}
	// The program, its arguments and its environment are this process's own: nothing here comes from a request.
	err = syscall.Exec(exe, os.Args, os.Environ()) //nolint:gosec // G702, G204: see above
	if errors.Is(err, fs.ErrNotExist) && runtime.GOOS == "linux" {
		err = syscall.Exec("/proc/self/exe", os.Args, os.Environ()) //nolint:gosec // G204: see above
	}
	return fmt.Errorf("server: re-exec %s: %w", exe, err)
}
