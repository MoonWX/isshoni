//go:build !windows

package winos

import "github.com/MoonWX/isshoni/internal/desktop/app"

// Registry has no values off Windows: no installer writes them.
type Registry struct{}

// NewRegistry returns the stub.
func NewRegistry(string) *Registry { return &Registry{} }

// TakePending has nothing to read.
func (*Registry) TakePending() (app.Pending, error) { return app.Pending{}, nil }

// WritePending isn't possible off Windows.
func (*Registry) WritePending(app.Pending) error { return app.ErrNotSupported }

// UpdateChecks is absent off Windows.
func (*Registry) UpdateChecks() (uint32, bool) { return 0, false }
