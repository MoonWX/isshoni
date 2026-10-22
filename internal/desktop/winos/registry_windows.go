//go:build windows

package winos

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// The values of HKCU\Software\isshoni that the app reads (06 §2.3).
const (
	valuePendingServer = "PendingServer"
	valuePendingRoom   = "PendingRoom"
	valueUpdateChecks  = "UpdateChecks"
	maxPendingBytes    = 2048 // deeplink.MaxBytes: a longer value is no server or room
)

// Registry is the app's key under HKEY_CURRENT_USER: RegistryKey, or a test's own key.
type Registry struct {
	path string
}

// NewRegistry returns the key at path under HKEY_CURRENT_USER.
func NewRegistry(path string) *Registry { return &Registry{path: path} }

// TakePending reads PendingServer and PendingRoom (REG_SZ) and deletes both, also when they hold something else or
// can't be read (03 §13.2, §13.4 step 6). A value of another type or longer than 2048 bytes reads as "".
func (r *Registry) TakePending() (app.Pending, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, r.path, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return app.Pending{}, nil
	}
	if err != nil {
		return app.Pending{}, fmt.Errorf("winos: the app's registry key: %w", err)
	}
	defer func() { _ = k.Close() }()
	p := app.Pending{Server: readString(k, valuePendingServer), Room: readString(k, valuePendingRoom)}
	var errs []error
	for _, name := range []string{valuePendingServer, valuePendingRoom} {
		if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			errs = append(errs, fmt.Errorf("winos: deleting %s: %w", name, err))
		}
	}
	return p, errors.Join(errs...)
}

// WritePending writes both values (an empty Room deletes PendingRoom): a start-time update with a link (03 §13.4
// step 5, README WA84) and a link start that finds the installer running (step 2a, WA59).
func (r *Registry) WritePending(p app.Pending) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, r.path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("winos: the app's registry key: %w", err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringValue(valuePendingServer, p.Server); err != nil {
		return fmt.Errorf("winos: writing %s: %w", valuePendingServer, err)
	}
	if p.Room == "" {
		if err := k.DeleteValue(valuePendingRoom); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("winos: deleting %s: %w", valuePendingRoom, err)
		}
		return nil
	}
	if err := k.SetStringValue(valuePendingRoom, p.Room); err != nil {
		return fmt.Errorf("winos: writing %s: %w", valuePendingRoom, err)
	}
	return nil
}

// UpdateChecks reads the installer's UpdateChecks DWORD (06 §2.3): ok false when it is absent or of another type.
// settings.Options.UpdateChecks.
func (r *Registry) UpdateChecks() (value uint32, ok bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, r.path, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer func() { _ = k.Close() }()
	v, typ, err := k.GetIntegerValue(valueUpdateChecks)
	if err != nil || typ != registry.DWORD {
		return 0, false
	}
	return uint32(v), true //nolint:gosec // G115: a REG_DWORD (checked above) is 32 bits
}

// readString reads a REG_SZ value; anything else reads as "".
func readString(k registry.Key, name string) string {
	s, typ, err := k.GetStringValue(name)
	if err != nil || typ != registry.SZ || len(s) > maxPendingBytes || !utf8.ValidString(s) {
		return ""
	}
	return s
}
