// Package deploy embeds the files the server serves from deploy/ (M2: the desktop install script).
//
// Only the server's wiring (internal/server) imports it and passes the embedded bytes on (docs/m2/06-windows-release.md
// §7.4; the depguard rule deploy-importers in .golangci.yml): httpapi gets them through httpapi.DesktopScripts.
package deploy

import _ "embed" // for the go:embed directive below

// InstallDesktopPS1 is deploy/install-desktop.ps1 as committed (docs/m2/06-windows-release.md §7.1): ASCII with LF
// line ends (.gitattributes keeps LF in a Git for Windows checkout, `task lint:ps` refuses CRLF), its prefill block
// unwritten and its key block as `task release:keys` wrote it. The server serves it at GET /install-desktop.ps1 with
// the prefill block rewritten for its own origin and version (httpapi.RenderInstallPS1). Read it, never change it.
//
//go:embed install-desktop.ps1
var InstallDesktopPS1 []byte
