//go:build !windows

package winos

import (
	"errors"
	"log/slog"
	"os"
)

// SingleInstance is a stub on darwin and linux: every process is the first instance (the macOS dev build may run
// twice; M3 adds the macOS lock).
type SingleInstance struct{}

// NewSingleInstance returns the stub; it reads no secret from the data root.
func NewSingleInstance(string, string, *slog.Logger) (*SingleInstance, error) {
	return &SingleInstance{}, nil
}

// Acquire always reports the first instance.
func (*SingleInstance) Acquire() (bool, error) { return true, nil }

// Serve does nothing: no second instance ever forwards here.
func (*SingleInstance) Serve(func([]string)) error { return nil }

// Forward is never needed.
func (*SingleInstance) Forward([]string) error {
	return errors.New("winos: no single instance on this OS")
}

// Close does nothing.
func (*SingleInstance) Close() error { return nil }

// osSecretFiles are the secret's files outside Windows, where only the tests use them: mode 0600, and a hard link
// that fails on an existing name.
func osSecretFiles() secretFiles {
	return secretFiles{
		createNew: func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the data root
		},
		publish: func(tmp, path string) error {
			if err := os.Link(tmp, path); err != nil {
				return err
			}
			_ = os.Remove(tmp) // published: a temp name left behind is only clutter
			return nil
		},
		replace: os.Rename,
	}
}
