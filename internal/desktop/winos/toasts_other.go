//go:build !windows

package winos

import "github.com/MoonWX/isshoni/internal/desktop/app"

// DeliveredToasts answers app.ErrNotSupported off Windows: there is no Action Center.
func DeliveredToasts(string) ([]DeliveredToast, error) { return nil, app.ErrNotSupported }

// RemoveDeliveredToasts answers app.ErrNotSupported off Windows.
func RemoveDeliveredToasts(string, func(DeliveredToast) bool) (int, error) {
	return 0, app.ErrNotSupported
}

// ShowToast answers app.ErrNotSupported off Windows.
func ShowToast(string, Toast) error { return app.ErrNotSupported }

// removeToasts does nothing off Windows: the app's toasts are Windows' (03 §15).
func (*Hooks) removeToasts(string, func(DeliveredToast) bool) (int, error) { return 0, nil }
