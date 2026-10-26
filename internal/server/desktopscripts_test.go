package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/deploy"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/version"
)

// TestCheckDesktopScripts: the startup check of the embedded install script (M2 06 §7.4) passes the committed file
// and refuses one without a prefill block as an operator error (exit 78) that names the block and carries a fix line.
func TestCheckDesktopScripts(t *testing.T) {
	ok := &httpapi.DesktopScripts{PS1: deploy.InstallDesktopPS1, Origin: "https://watch.example.com", Version: "0.2.0"}
	if err := checkDesktopScripts(ok); err != nil {
		t.Errorf("checkDesktopScripts(embedded script) = %v, want nil", err)
	}

	err := checkDesktopScripts(&httpapi.DesktopScripts{PS1: []byte("Write-Host 'x'\n")})
	if err == nil {
		t.Fatal("checkDesktopScripts(a script without a prefill block) = nil, want an error")
	}
	if !NeedsOperator(err) {
		t.Errorf("NeedsOperator(%v) = false, want true: serve must exit 78 on a broken embedded script", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "isshoni-prefill") || !strings.Contains(msg, "\n  fix: ") {
		t.Errorf("error = %q, want it to name isshoni-prefill and carry a fix line", msg)
	}
}

// TestRouterOptionsDesktop: the server passes the embedded script, its site's origin and this binary's version to
// the router (RouterOptions.Desktop), which serves GET /install-desktop.ps1 from them (M2 06 §7.4).
func TestRouterOptionsDesktop(t *testing.T) {
	s, err := New(loadFlags(t), discardLog(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	s.site = config.Site{Origin: "https://watch.example.com", Host: "watch.example.com", Hostname: "watch.example.com", TLSMode: config.TLSOff}
	d := s.routerOptions().Desktop
	if d == nil {
		t.Fatal("RouterOptions.Desktop is nil: GET /install-desktop.ps1 would answer 404")
	}
	if !bytes.Equal(d.PS1, deploy.InstallDesktopPS1) {
		t.Error("RouterOptions.Desktop.PS1 is not deploy.InstallDesktopPS1")
	}
	if d.Origin != s.site.Origin {
		t.Errorf("RouterOptions.Desktop.Origin = %q, want the site's %q", d.Origin, s.site.Origin)
	}
	if d.Version != version.Version() {
		t.Errorf("RouterOptions.Desktop.Version = %q, want %q", d.Version, version.Version())
	}
}
