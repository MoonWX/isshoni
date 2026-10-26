//go:build !windows

package winos

import (
	"errors"
	"log/slog"
)

// SingleInstance is a stub on darwin and linux: every process is the first instance (the macOS dev build may run
// twice; M3 adds the macOS lock).
type SingleInstance struct{}

// NewSingleInstance returns the stub.
func NewSingleInstance(string, *slog.Logger) (*SingleInstance, error) { return &SingleInstance{}, nil }

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
