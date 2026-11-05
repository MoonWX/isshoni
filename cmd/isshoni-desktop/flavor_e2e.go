//go:build (windows || (darwin && cgo)) && e2e

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The e2e build's own variables (03 §22.4 B, README WA72), which web/e2e/cdp's fixture sets. Only this file reads
// them, and only -tags e2e compiles it. The file also gives the build its own credential prefix (e2eCredPrefix).
const (
	// e2eCDPPortEnv is the WebView2 remote debugging port. It can't come through
	// WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: the pinned Wails overwrites that variable in every build (03 §5.4).
	e2eCDPPortEnv = "ISSHONI_E2E_CDP_PORT"
	// e2eDataDirEnv is an absolute folder that stands in for %LOCALAPPDATA%, so the data root is <dir>\isshoni. It
	// can't come through LOCALAPPDATA: the app reads FOLDERID_LocalAppData, never the environment (03 §14.1).
	e2eDataDirEnv = "ISSHONI_E2E_DATA_DIR"
)

func init() {
	flavor.e2e = true
	flavor.browserArgs, flavor.localAppData, flavor.err = e2eSettings(os.Getenv)
	flavor.credPrefix = e2eCredPrefix()
}

// e2eCredPrefix is a fresh "isshoni-test:<16 hex>:" credential prefix (03 §22.4 B): every run of the e2e build keeps
// its credentials apart from the user's isshoni:v1: entries and from other runs, and deletes them at exit.
func e2eCredPrefix() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return "isshoni-test:" + hex.EncodeToString(b[:]) + ":"
}

// e2eSettings reads the e2e build's variables through getenv: the extra WebView2 arguments (the debugging port) and
// the folder that replaces FOLDERID_LocalAppData ("" keeps the known folder; the folder is used on Windows only). An
// unset variable keeps the default; a malformed one is an error, which the start reports and exits on, rather than
// falling back to the user's real data root or to a webview without the port the spec waits for.
func e2eSettings(getenv func(string) string) (args []string, localAppData string, err error) {
	if v := strings.TrimSpace(getenv(e2eCDPPortEnv)); v != "" {
		port, perr := strconv.ParseUint(v, 10, 16)
		if perr != nil || port == 0 {
			return nil, "", fmt.Errorf("%s=%q is not a port (1-65535)", e2eCDPPortEnv, v)
		}
		args = append(args, "--remote-debugging-port="+strconv.FormatUint(port, 10))
	}
	if v := getenv(e2eDataDirEnv); v != "" {
		if !filepath.IsAbs(v) {
			return nil, "", fmt.Errorf("%s=%q is not an absolute path", e2eDataDirEnv, v)
		}
		localAppData = filepath.Clean(v)
	}
	return args, localAppData, nil
}
