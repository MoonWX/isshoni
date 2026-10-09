package doctor_test

import (
	"slices"
	"testing"

	"github.com/caddyserver/certmagic"

	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
)

// doctor may not import tlsmgr (04 §2), so it has its own copy of the ten codes of tlsmgr.Status.LastErrorCode
// (04 §8.7), which its tls check passes on as fix codes. Each of them must have a fix text here, or a failing
// certificate would be reported without its cause.
func TestEveryCertificateCodeHasAFix(t *testing.T) {
	fixes := doctor.FixCodes()
	for _, code := range []string{
		tlsmgr.CodeACMEUnreachable, tlsmgr.CodeDNSMissing, tlsmgr.CodeDNSWrong, tlsmgr.CodeRateLimited, tlsmgr.CodeIPRejected,
		tlsmgr.CodeCAAForbids, tlsmgr.CodeACMEFailed, tlsmgr.CodeCertUnreadable, tlsmgr.CodeCertInvalid, tlsmgr.CodeCertExpired,
	} {
		if !slices.Contains(fixes, code) {
			t.Errorf("tlsmgr's code %q has no fix text in doctor", code)
		}
	}
}

// The clock check asks the ACME directory the certificate comes from. Its two Let's Encrypt URLs are certmagic's,
// which tlsmgr uses.
func TestLetsEncryptDirectories(t *testing.T) {
	production, staging := doctor.LetsEncryptDirectories()
	if production != certmagic.LetsEncryptProductionCA || staging != certmagic.LetsEncryptStagingCA {
		t.Errorf("doctor's directories %q and %q, certmagic's %q and %q", production, staging,
			certmagic.LetsEncryptProductionCA, certmagic.LetsEncryptStagingCA)
	}
}
