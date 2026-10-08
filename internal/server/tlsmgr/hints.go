package tlsmgr

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"

	"github.com/mholt/acmez/v3/acme"
)

// The codes of Status.LastErrorCode. They are stable: logs, the dashboard (api.TLSInfo) and doctor's tls check use
// them, and the web client translates them.
//
// The first seven are the ACME hints of 04 §8.7, one per row of its table. The last three are manual mode's load
// errors (04 §8.4), which that table does not have.
const (
	CodeACMEUnreachable = "tls.acme_unreachable" // the CA could not connect to this server on port 80 or 443
	CodeDNSMissing      = "tls.dns_missing"      // the domain does not resolve
	CodeDNSWrong        = "tls.dns_wrong"        // the domain points to another machine
	CodeRateLimited     = "tls.rate_limited"     // the CA's rate limit
	CodeIPRejected      = "tls.ip_rejected"      // the CA refuses the IP address
	CodeCAAForbids      = "tls.caa_forbids"      // a CAA record keeps the CA out
	CodeACMEFailed      = "tls.acme_failed"      // anything else; the text is the CA's own

	CodeCertUnreadable = "tls.cert_unreadable" // tls.cert_file or tls.key_file can't be read
	CodeCertInvalid    = "tls.cert_invalid"    // the two files are not a certificate with its key
	CodeCertExpired    = "tls.cert_expired"    // the certificate is past its end date, or not valid yet
)

// hint is a certificate problem as the operator sees it: a stable code and the English fix text that goes into
// Status.LastError, the log and doctor.
type hint struct {
	code string
	fix  string
}

// hintEnv is what the fix texts name.
type hintEnv struct {
	name     string     // the certificate's subject: the domain, or the IP address as text
	isIP     bool       // name is an IP address (ip mode)
	publicIP netip.Addr // this server's public address, when one is known
}

// maxDetail bounds the CA's own text in a tls.acme_failed fix.
const maxDetail = 300

// retryAfterRE finds the time in a rateLimited problem's detail, as Let's Encrypt writes it: "… retry after
// 2026-10-09 12:30:00 UTC: see https://letsencrypt.org/docs/rate-limits/…".
var retryAfterRE = regexp.MustCompile(`(?i)retry after (\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?: ?UTC|Z)?)`)

// hintFor maps an error of an ACME attempt to its hint (04 §8.7). An error that carries an acme.Problem (the CA's
// answer, RFC 8555 §6.7) is mapped by the problem's type; any other error, such as a CA that can't be reached from
// here, is tls.acme_failed with the error's text.
//
// Two rows of the table are about one kind of name only, so the other kind falls through to tls.acme_failed with the
// CA's detail: "unauthorized" is tls.dns_wrong for a domain (an IP address has no DNS record to fix), and
// "rejectedIdentifier" is tls.ip_rejected for an IP address. "unauthorized" is tls.acme_failed too when the CA says
// it did reach this server's address: then DNS is right and something else on this machine answered.
func hintFor(err error, env hintEnv) hint {
	var p acme.Problem
	if !errors.As(err, &p) {
		return hint{CodeACMEFailed, clip(err.Error())}
	}
	ip := ""
	if env.publicIP.IsValid() {
		ip = env.publicIP.Unmap().String()
	}
	failed := hint{CodeACMEFailed, clip(firstNonEmpty(p.Detail, p.Title, p.Type))}

	switch strings.TrimPrefix(p.Type, acme.ProblemTypeNamespace) {
	case "connection":
		return hint{CodeACMEUnreachable, "Let's Encrypt couldn't connect to " + env.name +
			" on port 80 or 443. Open TCP 80 and 443 in your cloud firewall."}
	case "dns":
		if env.isIP {
			return failed
		}
		if ip == "" {
			return hint{CodeDNSMissing, env.name + " doesn't resolve. Add an A record pointing to this server."}
		}
		return hint{CodeDNSMissing, env.name + " doesn't resolve. Add an A record pointing to " + ip + "."}
	case "unauthorized":
		other := reachedAddr(p.Detail)
		if env.isIP || (other.IsValid() && other == env.publicIP.Unmap()) {
			return failed
		}
		switch {
		case other.IsValid() && ip != "":
			return hint{CodeDNSWrong, env.name + " points to " + other.String() + ", but this server is " + ip + "."}
		case other.IsValid():
			return hint{CodeDNSWrong, env.name + " points to " + other.String() + ", which is not this server."}
		case ip != "":
			return hint{CodeDNSWrong, env.name + " doesn't point to this server (" + ip + ")."}
		default:
			return hint{CodeDNSWrong, env.name + " doesn't point to this server."}
		}
	case "rateLimited":
		const restore = " Restore certmagic/ from a backup if you reinstalled."
		if m := retryAfterRE.FindStringSubmatch(p.Detail); m != nil {
			return hint{CodeRateLimited, "Let's Encrypt rate limit until " + m[1] + "." + restore}
		}
		return hint{CodeRateLimited, "Let's Encrypt rate limit reached." + restore}
	case "rejectedIdentifier":
		if !env.isIP {
			return failed
		}
		return hint{CodeIPRejected, "Let's Encrypt refused " + env.name + " (not a public address?)."}
	case "caa":
		return hint{CodeCAAForbids, "A CAA record on " + env.name + " doesn't allow letsencrypt.org."}
	default:
		return failed
	}
}

// reachedAddr returns the address the CA connected to, which Let's Encrypt puts in front of a validation
// problem's detail ("203.0.113.9: Invalid response from http://…"), or the zero Addr.
func reachedAddr(detail string) netip.Addr {
	head, _, ok := strings.Cut(detail, ": ")
	if !ok {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(strings.TrimSpace(head))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// clip cuts a text from outside to one line of at most maxDetail bytes.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxDetail {
		return s
	}
	cut := maxDetail
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
