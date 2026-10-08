package tlsmgr

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/mholt/acmez/v3/acme"
)

func problem(kind, detail string) error {
	// As certmagic and acmez hand it on: the CA's problem document, wrapped several times.
	p := acme.Problem{Type: acme.ProblemTypeNamespace + kind, Status: 403, Detail: detail}
	return fmt.Errorf("[watch.example.com] Obtain: %w", fmt.Errorf("solving challenges: %w (order=https://ca.example/order/1)", p))
}

// TestHintFor is the table of 04 §8.7: ACME problem → code and fix text.
func TestHintFor(t *testing.T) {
	domain := hintEnv{name: "watch.example.com", publicIP: netip.MustParseAddr("203.0.113.7")}
	domainNoIP := hintEnv{name: "watch.example.com"}
	dualStack := hintEnv{name: "watch.example.com", publicIP: netip.MustParseAddr("203.0.113.7"), publicIPv6: netip.MustParseAddr("2001:db8::7")}
	ip := hintEnv{name: "203.0.113.7", isIP: true, publicIP: netip.MustParseAddr("203.0.113.7")}

	tests := []struct {
		name string
		err  error
		env  hintEnv
		code string
		fix  string
	}{
		{"connection, domain", problem("connection", "203.0.113.7: Fetching http://watch.example.com/.well-known/acme-challenge/x: Timeout during connect (likely firewall problem)"), domain,
			CodeACMEUnreachable, "Let's Encrypt couldn't connect to watch.example.com on port 80 or 443. Open TCP 80 and 443 in your cloud firewall."},
		{"connection, ip", problem("connection", "203.0.113.7: Fetching http://203.0.113.7/.well-known/acme-challenge/x: Connection refused"), ip,
			CodeACMEUnreachable, "Let's Encrypt couldn't connect to 203.0.113.7 on port 80 or 443. Open TCP 80 and 443 in your cloud firewall."},
		{"dns NXDOMAIN", problem("dns", "DNS problem: NXDOMAIN looking up A for watch.example.com - check that a DNS record exists for this domain"), domain,
			CodeDNSMissing, "watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7."},
		{"dns, own address unknown", problem("dns", "DNS problem: NXDOMAIN looking up A for watch.example.com"), domainNoIP,
			CodeDNSMissing, "watch.example.com doesn't resolve. Add an A record pointing to this server."},
		{"dns, a name without an address record", problem("dns", "no valid A records found for watch.example.com; no valid AAAA records found for watch.example.com"), domain,
			CodeDNSMissing, "watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7."},
		{"dns, no address found", problem("dns", "No valid IP addresses found for watch.example.com"), domain,
			CodeDNSMissing, "watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7."},
		// The record may be there: the nameserver failed, so "add an A record" would be wrong advice.
		{"dns SERVFAIL", problem("dns", "DNS problem: SERVFAIL looking up A for watch.example.com - the domain's nameservers may be malfunctioning"), domain,
			CodeACMEFailed, "DNS problem: SERVFAIL looking up A for watch.example.com - the domain's nameservers may be malfunctioning"},
		{"dns timeout", problem("dns", "DNS problem: query timed out looking up A for watch.example.com"), domain,
			CodeACMEFailed, "DNS problem: query timed out looking up A for watch.example.com"},
		{"dns, a failed CAA lookup", problem("dns", "DNS problem: SERVFAIL looking up CAA for example.com - the domain's nameservers may be malfunctioning"), domainNoIP,
			CodeACMEFailed, "DNS problem: SERVFAIL looking up CAA for example.com - the domain's nameservers may be malfunctioning"},
		{"unauthorized, another address", problem("unauthorized", "198.51.100.4: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), domain,
			CodeDNSWrong, "watch.example.com points to 198.51.100.4, but this server is 203.0.113.7."},
		{"unauthorized, another IPv6 address", problem("unauthorized", "2001:db8::4: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), domain,
			CodeDNSWrong, "watch.example.com points to 2001:db8::4, but this server is 203.0.113.7."},
		{"unauthorized, own address unknown", problem("unauthorized", "198.51.100.4: Invalid response from http://watch.example.com/x: 404"), domainNoIP,
			CodeDNSWrong, "watch.example.com points to 198.51.100.4, which is not this server."},
		{"unauthorized, no address in the detail", problem("unauthorized", "Incorrect validation certificate for tls-alpn-01 challenge"), domain,
			CodeDNSWrong, "watch.example.com doesn't point to this server (203.0.113.7)."},
		{"unauthorized, nothing known", problem("unauthorized", "Incorrect validation certificate"), domainNoIP,
			CodeDNSWrong, "watch.example.com doesn't point to this server."},
		{"unauthorized, but the CA reached this server", problem("unauthorized", "203.0.113.7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), domain,
			CodeACMEFailed, "203.0.113.7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"},
		// Let's Encrypt prefers IPv6 when the domain has an AAAA record.
		{"unauthorized, but the CA reached this server's IPv6 address", problem("unauthorized", "2001:db8::7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), dualStack,
			CodeACMEFailed, "2001:db8::7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"},
		{"unauthorized, but the CA reached this server's IPv4 address, dual stack", problem("unauthorized", "203.0.113.7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), dualStack,
			CodeACMEFailed, "203.0.113.7: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"},
		{"unauthorized, another IPv6 address, dual stack", problem("unauthorized", "2001:db8::4: Invalid response from http://watch.example.com/.well-known/acme-challenge/x: 404"), dualStack,
			CodeDNSWrong, "watch.example.com points to 2001:db8::4, but this server is 203.0.113.7."},
		{"unauthorized, ip mode", problem("unauthorized", "203.0.113.7: Invalid response from http://203.0.113.7/.well-known/acme-challenge/x: 404"), ip,
			CodeACMEFailed, "203.0.113.7: Invalid response from http://203.0.113.7/.well-known/acme-challenge/x: 404"},
		{"rateLimited with a time", problem("rateLimited", "too many certificates (5) already issued for this exact set of identifiers in the last 168h0m0s, retry after 2026-10-09 12:30:00 UTC: see https://letsencrypt.org/docs/rate-limits/"), domain,
			CodeRateLimited, "Let's Encrypt rate limit until 2026-10-09 12:30:00 UTC. Restore certmagic/ from a backup if you reinstalled."},
		{"rateLimited without a time", problem("rateLimited", "Service busy; retry later."), ip,
			CodeRateLimited, "Let's Encrypt rate limit reached. Restore certmagic/ from a backup if you reinstalled."},
		{"rejectedIdentifier, ip", problem("rejectedIdentifier", "Cannot issue for \"10.0.0.5\": IP address is in a reserved address block"), hintEnv{name: "10.0.0.5", isIP: true},
			CodeIPRejected, "Let's Encrypt refused 10.0.0.5 (not a public address?)."},
		{"rejectedIdentifier, domain", problem("rejectedIdentifier", "Cannot issue for \"watch.example\": Domain name does not end with a valid public suffix (TLD)"), domain,
			CodeACMEFailed, "Cannot issue for \"watch.example\": Domain name does not end with a valid public suffix (TLD)"},
		{"caa", problem("caa", "CAA record for watch.example.com prevents issuance"), domain,
			CodeCAAForbids, "A CAA record on watch.example.com doesn't allow letsencrypt.org."},
		{"another problem", problem("serverInternal", "Error finalizing order :: The service is down for maintenance"), domain,
			CodeACMEFailed, "Error finalizing order :: The service is down for maintenance"},
		{"a problem without detail", fmt.Errorf("x: %w", acme.Problem{Type: acme.ProblemTypeNamespace + "malformed", Title: "Malformed request"}), domain,
			CodeACMEFailed, "Malformed request"},
		{"a problem of an unknown namespace", fmt.Errorf("x: %w", acme.Problem{Type: "about:blank"}), domain,
			CodeACMEFailed, "about:blank"},
		{"dns, ip mode", problem("dns", "DNS problem: SERVFAIL looking up CAA for 7.113.0.203.in-addr.arpa"), ip,
			CodeACMEFailed, "DNS problem: SERVFAIL looking up CAA for 7.113.0.203.in-addr.arpa"},
		{"not a problem document", errors.New("Get \"https://acme-v02.api.letsencrypt.org/directory\":\n dial tcp: lookup acme-v02.api.letsencrypt.org: no such host"), domain,
			CodeACMEFailed, "Get \"https://acme-v02.api.letsencrypt.org/directory\": dial tcp: lookup acme-v02.api.letsencrypt.org: no such host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hintFor(tc.err, tc.env)
			if got.code != tc.code || got.fix != tc.fix {
				t.Errorf("hintFor = %s %q\n            want %s %q", got.code, got.fix, tc.code, tc.fix)
			}
		})
	}
}

// TestHintCodes: the codes are stable strings that the dashboard and doctor carry. (Two constants with one value
// would not compile as keys of this map.)
func TestHintCodes(t *testing.T) {
	want := map[string]string{
		CodeACMEUnreachable: "tls.acme_unreachable",
		CodeDNSMissing:      "tls.dns_missing",
		CodeDNSWrong:        "tls.dns_wrong",
		CodeRateLimited:     "tls.rate_limited",
		CodeIPRejected:      "tls.ip_rejected",
		CodeCAAForbids:      "tls.caa_forbids",
		CodeACMEFailed:      "tls.acme_failed",
		CodeCertUnreadable:  "tls.cert_unreadable",
		CodeCertInvalid:     "tls.cert_invalid",
		CodeCertExpired:     "tls.cert_expired",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("code %q, want %q", got, w)
		}
	}
}

// TestHintClipsLongDetail: a CA's text is cut to one bounded line, on a rune boundary.
func TestHintClipsLongDetail(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	got := hintFor(problem("serverInternal", "line one\nline two "+long), hintEnv{name: "watch.example.com"})
	if got.code != CodeACMEFailed || strings.Contains(got.fix, "\n") || !strings.HasPrefix(got.fix, "line one line two é") {
		t.Fatalf("hint = %s %q", got.code, got.fix)
	}
	if len(got.fix) > maxDetail+len("…") || !strings.HasSuffix(got.fix, "é…") {
		t.Errorf("fix has %d bytes and ends %q", len(got.fix), got.fix[len(got.fix)-8:])
	}
}
