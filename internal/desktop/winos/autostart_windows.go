//go:build windows

package winos

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// runEntry is one value of a Run key under HKEY_CURRENT_USER: the app's (RunKey, RunValue), or a test's own.
type runEntry struct {
	key, name string
}

// appRun is the app's Run value.
var appRun = runEntry{key: RunKey, name: RunValue}

// set writes the value (REG_SZ) with exe's RunCommand when on, and deletes it when off (a value or key that isn't
// there is no error).
func (r runEntry) set(on bool, exe string) error {
	if !on {
		k, err := registry.OpenKey(registry.CURRENT_USER, r.key, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("winos: the Run key: %w", err)
		}
		defer func() { _ = k.Close() }()
		if err := k.DeleteValue(r.name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("winos: deleting the Run value: %w", err)
		}
		return nil
	}
	cmd, err := RunCommand(exe)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, r.key, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("winos: the Run key: %w", err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringValue(r.name, cmd); err != nil {
		return fmt.Errorf("winos: writing the Run value: %w", err)
	}
	return nil
}

// get reports whether the value is there as a string (REG_SZ or REG_EXPAND_SZ), with its data.
func (r runEntry) get() (data string, ok bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, r.key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("winos: the Run key: %w", err)
	}
	defer func() { _ = k.Close() }()
	s, typ, err := k.GetStringValue(r.name)
	switch {
	case errors.Is(err, registry.ErrNotExist), errors.Is(err, registry.ErrUnexpectedType):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("winos: reading the Run value: %w", err)
	case typ != registry.SZ && typ != registry.EXPAND_SZ:
		return "", false, nil
	}
	return s, true, nil
}

// SetAutostart writes HKCU\...\Run\isshoni = "<exe>" --autostart when on and deletes it when off (03 §16.1).
func (*Hooks) SetAutostart(on bool, exe string) error { return appRun.set(on, exe) }

// Autostart reports whether the Run value is there.
func (*Hooks) Autostart() (bool, error) {
	_, ok, err := appRun.get()
	return ok, err
}
