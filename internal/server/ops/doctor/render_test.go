package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// sampleReport is a report with one check of every status, as the CLI gets it from the admin socket.
func sampleReport() api.DoctorReport {
	return api.DoctorReport{
		Schema: api.DoctorReportSchema, Version: "0.3.0", RanAt: time.Date(2026, 9, 29, 22, 15, 30, 0, time.FixedZone("CEST", 7200)),
		Mode:    api.DoctorModeCLIWithServer,
		Summary: api.DoctorCounts{OK: 1, Warn: 1, Fail: 1, Skip: 1, Info: 2},
		Env:     api.DoctorEnv{OS: "linux", Provider: api.CloudProviderHetzner},
		Checks: []api.DoctorCheck{
			{ID: "config", Status: api.DoctorStatusOK, Code: codeConfigOK, Message: "/etc/isshoni/isshoni.toml"},
			{ID: "public_ip", Status: api.DoctorStatusFail, Code: codePublicIPNone,
				Message: "no public IP address found\nisshoni is running but not ready",
				FixCode: fixPublicIPSet, Fix: "set public_ip\n(or restart)"},
			{ID: "udp_buffers", Status: api.DoctorStatusWarn, Code: codeUDPBuffersLow, Message: "net.core.rmem_max is 212992, isshoni wants 8388608",
				FixCode: fixUDPBuffersSysctlReload, Fix: "sudo sysctl --system"},
			{ID: "ports", Status: api.DoctorStatusInfo, Code: codePortsListening, Message: "listening on 443/tcp (local check only)", LocalOnly: true},
			{ID: "firewall_hint", Status: api.DoctorStatusInfo, Code: codeFirewallHintOpenPorts, Message: "Hetzner: Cloud Console → Firewalls → allow TCP 80"},
			{ID: "transfer", Status: api.DoctorStatusSkip, Code: codeSkipNotReported, Message: "not checked: the server does not report it"},
		},
	}
}

// The layout of 04 §13.2: the header, one line per check with its id in a column, continuation lines under the
// text, "fix:" and "more:" after warnings and failures, the provider's page after firewall_hint, and the counts.
func TestRenderText(t *testing.T) {
	var b strings.Builder
	RenderText(&b, sampleReport(), false)
	want := `isshoni doctor · 0.3.0 · server running · 2026-09-29 20:15 UTC
[ ok ] config        /etc/isshoni/isshoni.toml
[fail] public_ip     no public IP address found
       isshoni is running but not ready
       fix: set public_ip
            (or restart)
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-public_ip
[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608
       fix: sudo sysctl --system
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-udp_buffers
[info] ports         listening on 443/tcp (local check only)
[info] firewall_hint Hetzner: Cloud Console → Firewalls → allow TCP 80
       https://moonwx.github.io/isshoni/install/vps#hetzner
[skip] transfer      not checked: the server does not report it
1 ok · 1 warn · 1 fail · 1 skipped
`
	if b.String() != want {
		t.Errorf("text:\n%s\nwant:\n%s", b.String(), want)
	}

	// Offline, an unknown provider (no link), and nothing skipped.
	rep := sampleReport()
	rep.Mode, rep.Env.Provider, rep.Summary.Skip = api.DoctorModeCLIOffline, api.CloudProviderUnknown, 0
	b.Reset()
	RenderText(&b, rep, false)
	contains(t, b.String(), "isshoni doctor · 0.3.0 · server not running · ", "\n1 ok · 1 warn · 1 fail\n")
	if strings.Contains(b.String(), "install/vps#") {
		t.Errorf("an unknown provider got a link:\n%s", b.String())
	}
}

// Colors go on the status tags only, and only when asked for.
func TestRenderTextColor(t *testing.T) {
	var plain, colored strings.Builder
	RenderText(&plain, sampleReport(), false)
	RenderText(&colored, sampleReport(), true)
	if strings.Contains(plain.String(), "\x1b[") {
		t.Error("the plain output has escape sequences")
	}
	for _, tag := range []string{
		"\x1b[32m[ ok ]\x1b[0m config", "\x1b[31m[fail]\x1b[0m public_ip", "\x1b[33m[warn]\x1b[0m udp_buffers",
		"\x1b[36m[info]\x1b[0m ports", "\x1b[2m[skip]\x1b[0m transfer",
	} {
		contains(t, colored.String(), tag)
	}
	// Without the colors the two are the same text.
	stripped := colored.String()
	for _, seq := range []string{"\x1b[32m", "\x1b[31m", "\x1b[33m", "\x1b[36m", "\x1b[2m", "\x1b[0m"} {
		stripped = strings.ReplaceAll(stripped, seq, "")
	}
	if stripped != plain.String() {
		t.Errorf("colored output differs in more than its colors:\n%s", colored.String())
	}
}

// failingWriter refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A writer that fails does not make RenderText panic, and an empty report still has its two lines.
func TestRenderTextEdges(t *testing.T) {
	RenderText(failingWriter{}, sampleReport(), true)
	var b strings.Builder
	RenderText(&b, api.DoctorReport{Version: "0.3.0", Mode: api.DoctorModeCLIOffline, RanAt: testNow}, false)
	if want := "isshoni doctor · 0.3.0 · server not running · 2026-09-29 20:15 UTC\n0 ok · 0 warn · 0 fail\n"; b.String() != want {
		t.Errorf("empty report: %q", b.String())
	}
}
