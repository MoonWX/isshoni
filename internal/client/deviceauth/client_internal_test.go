package deviceauth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The verification-URI check of 02 §11.2 against an https origin: every refused shape of 02 §16.1, and the
// accepted spellings (upper-case host, explicit :443), which are replaced by Go's canonical strings.
func TestCheckCodeResponse(t *testing.T) {
	c, err := NewClient("https://watch.example.com", AppInfo{}, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const code = "WDJB-MJHT"
	base := api.DeviceCodeResponse{
		DeviceCode:              strings.Repeat("A", 43),
		UserCode:                code,
		VerificationURI:         "https://watch.example.com/link",
		VerificationURIComplete: "https://watch.example.com/link?code=" + code,
		ExpiresIn:               600,
		Interval:                5,
	}
	refused := []string{
		"file:///C:/x",
		"ms-settings:display",
		"search-ms:query=x",
		"javascript:alert(1)",
		`\\host\share\x`,
		"file://host/share/x",
		"https://evil.example/link?code=" + code,
		"https://watch.example.com.evil.example/link?code=" + code,
		"https://watch.example.com:8443/link?code=" + code,
		"http://watch.example.com/link?code=" + code,
		"http://localhost/link?code=" + code,
		"https://user@watch.example.com/link?code=" + code,
		"https://user:pw@watch.example.com/link?code=" + code,
		"https://watch.example.com/link?code=" + code + "#frag",
		"https://watch.example.com/link?code=" + code + "#",
		"https://watch.example.com/link/?code=" + code,
		"https://watch.example.com/Link?code=" + code,
		"https://watch.example.com//link?code=" + code,
		"https://watch.example.com/link?code=" + code + "&next=x",
		"https://watch.example.com/link?next=x&code=" + code,
		"https://watch.example.com/link?code=BCDF-GHJK",
		"https://watch.example.com/link?code=wdjb-mjht",
		"https://watch.example.com/link",
		"https:///link?code=" + code,
		"https://xn--wtch-6na.example.com/link?code=" + code,
		"",
	}
	for _, u := range refused {
		r := base
		r.VerificationURIComplete = u
		if _, err := c.checkCodeResponse(r); !errors.Is(err, ErrBadResponse) {
			t.Errorf("verificationUriComplete %q accepted", u)
		}
		if u == base.VerificationURI {
			continue // the right verificationUri
		}
		r = base
		r.VerificationURI = u
		if _, err := c.checkCodeResponse(r); !errors.Is(err, ErrBadResponse) {
			t.Errorf("verificationUri %q accepted", u)
		}
	}
	for _, u := range []string{"https://watch.example.com/link?", "https://watch.example.com/link?code=" + code} {
		r := base
		r.VerificationURI = u
		if _, err := c.checkCodeResponse(r); !errors.Is(err, ErrBadResponse) {
			t.Errorf("verificationUri %q (with a query) accepted", u)
		}
	}
	for _, tc := range []struct{ uri, complete string }{
		{"https://WATCH.Example.COM/link", "HTTPS://watch.EXAMPLE.com/link?code=" + code},
		{"https://watch.example.com:443/link", "https://watch.example.com:443/link?code=" + code},
	} {
		r := base
		r.VerificationURI, r.VerificationURIComplete = tc.uri, tc.complete
		got, err := c.checkCodeResponse(r)
		if err != nil {
			t.Errorf("%q, %q refused: %v", tc.uri, tc.complete, err)
			continue
		}
		if got.VerificationURI != "https://watch.example.com/link" ||
			got.VerificationURIComplete != "https://watch.example.com/link?code="+code || got.UserCode != code {
			t.Errorf("canonical strings = %+v", got)
		}
	}
	// A user code in another spelling is formatted; the complete URI must carry the formatted one.
	r := base
	r.UserCode = "wdjb mjht"
	if got, err := c.checkCodeResponse(r); err != nil || got.UserCode != code {
		t.Errorf("user code %q: %+v, %v", r.UserCode, got, err)
	}
}

// 02 §3's shape checks: canonical base64url of exactly 32 bytes (16 for an exchange id).
func TestShapes(t *testing.T) {
	good := strings.Repeat("A", 43)
	for s, want := range map[string]bool{
		good:                                 true,
		strings.Repeat("A", 42) + "E":        true,  // its two trailing bits are zero
		strings.Repeat("A", 42) + "B":        false, // non-zero trailing bits: not canonical
		strings.Repeat("A", 42):              false,
		strings.Repeat("A", 44):              false,
		strings.Repeat("A", 42) + "=":        false,
		strings.Repeat("A", 42) + "+":        false,
		strings.Repeat("A", 41) + "\n" + "A": false,
	} {
		if validDeviceCode(s) != want {
			t.Errorf("validDeviceCode(%q) = %v", s, !want)
		}
	}
	if !validToken("isa_"+good, accessPrefix) || validToken("isr_"+good, accessPrefix) || validToken(good, accessPrefix) {
		t.Error("validToken")
	}
	id, err := newExchangeID(strings.NewReader(strings.Repeat("x", 16)))
	if err != nil || len(id) != 22 || !validB64(id, 16) {
		t.Errorf("newExchangeID = %q, %v", id, err)
	}
	if _, err := newExchangeID(strings.NewReader("short")); err == nil {
		t.Error("newExchangeID from a short reader succeeded")
	}
}

func TestValidAppVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"0.2.0":                        true,
		"0.0.0-dev+1a2b3c4d5e6f":       true,
		"0.0.0-dev+1a2b3c4d5e6f-dirty": true,
		"":                             false,
		strings.Repeat("1", 33):        false,
		"0.2.0 beta":                   false,
		"0.2.0_1":                      false,
	} {
		if validAppVersion(v) != want {
			t.Errorf("validAppVersion(%q) = %v", v, !want)
		}
	}
}

// slow_down's params.interval as JSON decodes it.
func TestPositiveSeconds(t *testing.T) {
	for v, want := range map[any]time.Duration{
		float64(10):      10 * time.Second,
		12:               12 * time.Second,
		json.Number("7"): 7 * time.Second,
	} {
		if got, ok := positiveSeconds(v); !ok || got != want {
			t.Errorf("positiveSeconds(%v) = %v, %v", v, got, ok)
		}
	}
	for _, v := range []any{nil, float64(0), -5, 1.5, "10", float64(1e9), json.Number("x")} {
		if _, ok := positiveSeconds(v); ok {
			t.Errorf("positiveSeconds(%v) accepted", v)
		}
	}
}

// The retry series: 1, 2, 4, 8, 16, 30 s, then 30 s, each ±20 %; a 429 waits its retryAfter.
func TestRetryDelay(t *testing.T) {
	m := &Manager{}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for n, w := range want {
		w *= time.Second
		for range 50 {
			d := m.retryDelay(n, ErrNetwork)
			if d < w*8/10 || d > w*12/10 {
				t.Fatalf("retry %d: %v, want %v ± 20 %%", n, d, w)
			}
		}
	}
	if d := m.retryDelay(0, &api.Error{Code: api.CodeRateLimited, RetryAfter: 42}); d != 42*time.Second {
		t.Errorf("429 delay = %v", d)
	}
}

func TestRetryable(t *testing.T) {
	for err, want := range map[error]bool{
		ErrNetwork:                                   true,
		badResponse(502, "x"):                        true,
		&api.Error{Code: api.CodeServerBusy}:         true,
		&api.Error{Code: api.CodeInternal}:           true,
		&api.Error{Code: api.CodeRateLimited}:        true,
		&api.Error{Code: "some_future_code"}:         true, // unknown codes count as 500
		&api.Error{Code: api.CodeInvalidGrant}:       false,
		&api.Error{Code: api.CodeValidationFailed}:   false,
		&api.Error{Code: api.CodeInvalidCredentials}: false,
		errors.New("anything else"):                  false,
	} {
		if retryable(err) != want {
			t.Errorf("retryable(%v) = %v", err, !want)
		}
	}
}
