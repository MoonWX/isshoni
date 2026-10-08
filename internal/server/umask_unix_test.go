//go:build unix

package server_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// TestStartSetsPrivateUmask: the real server (Deps.InProcess false) sets umask 0077 before it creates anything
// (04 §5.1), and a server inside another process leaves the umask alone. No test of this package runs in parallel,
// so changing the process umask for a moment is safe here.
func TestStartSetsPrivateUmask(t *testing.T) {
	const open = 0o022
	old := syscall.Umask(open)
	defer syscall.Umask(old)

	// A machine without cgroups and with GOMEMLIMIT set, so the real path changes nothing else in this process.
	host := config.Host{Root: t.TempDir(), Environ: []string{"GOMEMLIMIT=1GiB"}}

	for _, tc := range []struct {
		name      string
		inProcess bool
		want      int
	}{
		{"in process", true, open},
		{"real server", false, 0o077},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syscall.Umask(open)
			cfg := testConfig(t)
			s, err := server.New(cfg, discardLog(), server.Deps{InProcess: tc.inProcess, Host: host})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := syscall.Umask(open) // reads the umask; the next subtest sets it again anyway
			if err := s.Shutdown(context.Background(), server.ShutdownStop); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			if got != tc.want {
				t.Errorf("umask after Start = %#o, want %#o", got, tc.want)
			}
			// Either way the data directory and the secrets are private: the server asks for those modes itself.
			for name, want := range map[string]fs.FileMode{"": 0o700, "secrets.json": 0o600, "isshoni.lock": 0o600} {
				fi, err := os.Stat(filepath.Join(cfg.DataDir, name))
				if err != nil {
					t.Errorf("%q: %v", name, err)
					continue
				}
				if fi.Mode().Perm() != want {
					t.Errorf("%q has mode %v, want %v", name, fi.Mode().Perm(), want)
				}
			}
		})
	}
}
