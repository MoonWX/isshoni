//go:build !windows

package native

import (
	"errors"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestOpenUnsupportedOS: off Windows, Open returns an error wrapping media.ErrUnsupportedOS (04 §12.1, §18.3), a
// *media.Error like every error of the package.
func TestOpenUnsupportedOS(t *testing.T) {
	e, err := Open(Options{})
	if e != nil {
		t.Error("Open returned an engine")
	}
	if !errors.Is(err, media.ErrUnsupportedOS) {
		t.Fatalf("Open: %v, want media.ErrUnsupportedOS", err)
	}
	wantError(t, err, media.CodeUnsupportedOS, "", "open")
}
