//go:build windows

package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/media"
)

// dllName is the engine's file name (docs/m2/README.md §4, Naming).
const dllName = "isshoni-media.dll"

// dllSearch restricts the DLL's own dependencies (the FFmpeg DLLs, delay-loaded system DLLs) to its folder and
// System32, never PATH or the working directory (04 §13.3 layer 1, 01 §7.1).
const dllSearch = windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | windows.LOAD_LIBRARY_SEARCH_SYSTEM32

// Open loads the engine (04 §12.2): LoadLibraryEx(<Dir>\isshoni-media.dll, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR |
// LOAD_LIBRARY_SEARCH_SYSTEM32) by full path; im_version (major 1, 04 §5.5); every export resolved; im_init; the clock
// cross-check (04 §12.3); the build info. A version or export problem is a *media.Error with Code dependency and
// Reason "abi" (01 §5.2); a DLL that is not there or that a code-integrity policy blocked is dependency with Reason
// "missing" or "blocked_by_policy". The DLL is never unloaded: a failed Open leaves it loaded, and a later Open reuses
// it.
func Open(o Options) (*Engine, error) {
	dir, err := engineDir(o.Dir, os.Executable)
	if err != nil {
		return nil, err
	}
	h, err := loadDLL(filepath.Join(dir, dllName))
	if err != nil {
		return nil, err
	}
	lookup := func(name string) (uintptr, bool) {
		a, err := windows.GetProcAddress(h, name)
		return a, err == nil && a != 0
	}
	// The version first, so that a DLL of another major fails as one rather than as a list of missing exports.
	if a, ok := lookup(procVersion.String()); ok {
		r, _, _ := syscall.SyscallN(a)
		if err := checkVersion(uint32(r)); err != nil {
			return nil, err
		}
	}
	addrs, err := resolveProcs(lookup)
	if err != nil {
		return nil, err
	}
	return openEngine(&dllCaller{addr: addrs}, o)
}

// loadDLL is the one LoadLibraryEx of the engine, by full path with dllSearch. The error names the file, not its
// folder (the path holds the user's name; logs carry no paths of this PC).
func loadDLL(path string) (windows.Handle, error) {
	h, err := windows.LoadLibraryEx(path, 0, dllSearch)
	if err == nil {
		return h, nil
	}
	var errno syscall.Errno
	code := uint32(0)
	if errors.As(err, &errno) {
		code = uint32(errno)
	}
	return 0, &media.Error{Code: media.CodeDependency, Op: "open", Reason: loadErrorReason(code),
		Detail: fmt.Sprintf("LoadLibraryEx %s: %v (0x%X)", dllName, err, code)}
}
