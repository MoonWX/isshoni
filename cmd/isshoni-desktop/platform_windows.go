//go:build windows

package main

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/assets"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// assetOrigin is the origin of the SPA's pages: Wails v3's asset server in WebView2 (03 §5.7, W1).
const assetOrigin = protocol.DesktopAssetOriginWindows

// assetGuard is the /wails/* guard with 03 §5.6's rule as written: WebView2 sends Origin or Sec-Fetch-Site.
func assetGuard() func(http.Handler) http.Handler { return assets.Guard(assetOrigin) }

// clientOS is BridgeConfig.App.OS.
const clientOS = protocol.ClientOSWindows

// defaultDeviceName is 02's device name when the computer has no name (AppInfo.DeviceName).
const defaultDeviceName = "Windows PC"

// dataRoot returns %LOCALAPPDATA%\isshoni (03 §14.1) from the shell's known folder, never from the environment
// (README §4: release builds read no environment variable for paths). The e2e build alone takes the folder from
// ISSHONI_E2E_DATA_DIR when it is set (flavor.localAppData, 03 §22.4 B), so a CDP run never touches the user's data.
func dataRoot() (string, error) {
	if flavor.err != nil {
		return "", flavor.err
	}
	local := flavor.localAppData
	if local == "" {
		var err error
		local, err = windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(local) {
		return "", errors.New("the LocalAppData folder is not an absolute path")
	}
	return filepath.Join(local, "isshoni"), nil
}

// profileDir is the user's profile folder (FOLDERID_Profile), which the log writes as %USERPROFILE% (03 §17).
func profileDir() string {
	p, err := windows.KnownFolderPath(windows.FOLDERID_Profile, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return ""
	}
	return p
}

// webviewDataDir is WebView2's user data folder, %LOCALAPPDATA%\isshoni\WebView2, absolute and without a trailing
// separator (03 §5.4; the same string is audio.SelfInfo.WebViewDataDir, main.go's newAudioService).
func webviewDataDir() (string, error) {
	root, err := dataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(root, "WebView2")), nil
}

// osBuild is the Windows build number, such as 26100 (BridgeConfig.App.OSBuild).
func osBuild() int {
	return int(windows.RtlGetVersion().BuildNumber)
}

// mutexOpens is 03 §13.4 step 2a's probe: OpenMutexW(SYNCHRONIZE, FALSE, name). It succeeds while the mutex exists
// (true); ERROR_FILE_NOT_FOUND means it doesn't (false); any other error (access denied included) is returned, and
// the start logs it and goes on. winos.MutexExists counts access denied as existing, which step 2a must not.
func mutexOpens(name string) (bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, p)
	switch {
	case err == nil:
		_ = windows.CloseHandle(h)
		return true, nil
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
		return false, nil
	default:
		return false, fmt.Errorf("OpenMutexW: %w", err)
	}
}
