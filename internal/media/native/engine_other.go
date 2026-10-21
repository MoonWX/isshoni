//go:build !windows

package native

import (
	"runtime"

	"github.com/MoonWX/isshoni/internal/media"
)

// Open returns an error wrapping media.ErrUnsupportedOS: in M2 the native engine exists on Windows only (04 §12.1).
// M3 adds engine_darwin.go (cgo, a static engine) and M4 engine_linux.go; the app on macOS and Linux runs the fake.
func Open(o Options) (*Engine, error) {
	return nil, &media.Error{Code: media.CodeUnsupportedOS, Op: "open",
		Detail: "no native media engine for " + runtime.GOOS + " yet"}
}
