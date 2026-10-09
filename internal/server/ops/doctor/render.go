package doctor

import (
	"fmt"
	"io"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// The CLI's text output (04 §13.2):
//
//	isshoni doctor · 0.3.0 · server running · 2026-09-29 20:15 UTC
//	[ ok ] config        /etc/isshoni/isshoni.toml
//	[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608
//	       fix: printf '…' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system
//	       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-udp_buffers
//	[info] firewall_hint Hetzner: Cloud Console → Firewalls → allow TCP 80, 443, 7882 and UDP 7882
//	       https://moonwx.github.io/isshoni/install/vps#hetzner
//	12 ok · 1 warn · 0 fail
//
// Nothing is wrapped: a fix is often a command, and it must stay copyable.

const (
	idWidth    = 13                        // the longest check id, firewall_hint
	indent     = "       "                 // under the text of a result line: the width of "[warn] "
	fixIndent  = indent + "     "          // under the text of a "fix: " line
	troubleURL = "troubleshooting#doctor-" // + the check's id (06 §10.4)
	vpsURL     = "install/vps#"            // + the provider's id
)

// The ANSI colors of the status tags.
var statusColor = map[api.DoctorStatus]string{
	api.DoctorStatusOK:   "\x1b[32m", // green
	api.DoctorStatusWarn: "\x1b[33m", // yellow
	api.DoctorStatusFail: "\x1b[31m", // red
	api.DoctorStatusSkip: "\x1b[2m",  // dim
	api.DoctorStatusInfo: "\x1b[36m", // cyan
}

const colorReset = "\x1b[0m"

// statusTag is the bracketed status at the start of a line, always four characters wide inside the brackets.
func statusTag(s api.DoctorStatus, color bool) string {
	tag := "[" + string(s) + "]"
	if s == api.DoctorStatusOK {
		tag = "[ ok ]"
	}
	if c, ok := statusColor[s]; ok && color {
		return c + tag + colorReset
	}
	return tag
}

// RenderText writes the report for a terminal: one line per check, with its fix and a link to the project site's
// troubleshooting section after every warning and failure, a link to the provider's install notes where a result
// is about the hosting provider, and the counts at the end. color adds ANSI colors to the status tags. An error of
// w is dropped: the caller has nowhere better to report it.
func RenderText(w io.Writer, r api.DoctorReport, color bool) {
	var b strings.Builder
	server := "server running"
	if r.Mode == api.DoctorModeCLIOffline {
		server = "server not running"
	}
	fmt.Fprintf(&b, "isshoni doctor · %s · %s · %s\n", r.Version, server, r.RanAt.UTC().Format("2006-01-02 15:04")+" UTC")

	for _, c := range r.Checks {
		lines := strings.Split(c.Message, "\n")
		fmt.Fprintf(&b, "%s %-*s %s\n", statusTag(c.Status, color), idWidth, c.ID, lines[0])
		for _, line := range lines[1:] {
			b.WriteString(indent + line + "\n")
		}
		if c.Fix != "" {
			fix := strings.Split(c.Fix, "\n")
			b.WriteString(indent + "fix: " + fix[0] + "\n")
			for _, line := range fix[1:] {
				b.WriteString(fixIndent + line + "\n")
			}
		}
		if c.Status == api.DoctorStatusWarn || c.Status == api.DoctorStatusFail {
			b.WriteString(indent + "more: " + version.DocsURL + troubleURL + c.ID + "\n")
		}
		if provider := r.Env.Provider; provider != "" && provider != api.CloudProviderUnknown &&
			(c.ID == idFirewallHint || c.Code == codePublicIPEphemeral) {
			b.WriteString(indent + version.DocsURL + vpsURL + string(provider) + "\n")
		}
	}

	fmt.Fprintf(&b, "%d ok · %d warn · %d fail", r.Summary.OK, r.Summary.Warn, r.Summary.Fail)
	if r.Summary.Skip > 0 {
		fmt.Fprintf(&b, " · %d skipped", r.Summary.Skip)
	}
	b.WriteString("\n")
	_, _ = io.WriteString(w, b.String())
}
