//go:build windows

package main

import (
	"errors"
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

// dataRoot returns %LOCALAPPDATA%\isshoni (03 §14.1) from the shell's known folder, never from the environment
// (README §4: release builds read no environment variable for paths).
func dataRoot() (string, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(local) {
		return "", errors.New("the LocalAppData folder is not an absolute path")
	}
	return filepath.Join(local, "isshoni"), nil
}

// webviewDataDir is WebView2's user data folder, %LOCALAPPDATA%\isshoni\WebView2, absolute and without a trailing
// separator (03 §5.4; the same string becomes audio.SelfInfo.WebViewDataDir in WA41's wiring).
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
