//go:build windows

package native

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// The Windows-only part of the session API (04 §12.2, 01 §5.1): media.Source.Exe of a window pick, the base name of
// the image of the window's process. Everything else of Pick and the session is portable (bridge.go).
func init() { exeOfPID = processImageName }

// processImageName is the base name of pid's image (OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION, then
// QueryFullProcessImageNameW), or "" when the process is gone or not readable (a protected or elevated process). The
// name is local data (the UI, 05's audio rules): never logged.
func processImageName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about a failed close of a query handle
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}
