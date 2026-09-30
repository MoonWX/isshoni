package auth

import (
	"strings"
	"testing"
)

func TestDescribeUserAgent(t *testing.T) {
	for _, tc := range []struct{ ua, want string }{
		// Desktop.
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
			"Chrome on Windows"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.2792.79",
			"Edge on Windows"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.2792.65",
			"Edge on macOS"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
			"Chrome on macOS"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
			"Safari on macOS"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on macOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on Linux"},
		{"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox on Linux"},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
			"Chrome on Linux"},
		{"Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
			"Chrome on ChromeOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 OPR/114.0.0.0",
			"Opera on Windows"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Vivaldi/6.9.3447.54",
			"Vivaldi on Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/129.0.6668.29 Safari/537.36",
			"Chrome on Linux"},

		// iPhone and iPad.
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
			"Safari on iPhone"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
			"Safari on iPhone"}, // Home Screen web app
		{"Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
			"Safari on iPad"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0.6668.69 Mobile/15E148 Safari/604.1",
			"Chrome on iPhone"},
		{"Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0.6668.69 Mobile/15E148 Safari/604.1",
			"Chrome on iPad"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/131.0 Mobile/15E148 Safari/605.1.15",
			"Firefox on iPhone"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 EdgiOS/129.2792.84 Mobile/15E148 Safari/605.1.15",
			"Edge on iPhone"},

		// Android.
		{"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36",
			"Chrome on Android"},
		{"Mozilla/5.0 (Linux; Android 14; SAMSUNG SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/26.0 Chrome/122.0.0.0 Mobile Safari/537.36",
			"Samsung Internet on Android"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8 Build/AP2A.240805.005; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/129.0.6668.81 Mobile Safari/537.36",
			"WebView on Android"},
		{"Mozilla/5.0 (Android 14; Mobile; rv:131.0) Gecko/131.0 Firefox/131.0", "Firefox on Android"},
		{"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36 EdgA/129.0.0.0",
			"Edge on Android"},
		{"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36 OPR/84.0.0.0",
			"Opera on Android"},
		{"Mozilla/5.0 (Linux; U; Android 4.0.3; en-us) AppleWebKit/534.30 (KHTML, like Gecko) Version/4.0 Mobile Safari/534.30",
			"Browser on Android"}, // the old stock browser is not Safari

		// Fallbacks.
		{"curl/8.7.1", "Browser"},
		{"", "Browser"},
		{"Mozilla/5.0 (X11; Linux x86_64) SomethingNew/1.0", "Browser on Linux"},
		{"Mozilla/5.0 (Unknown) Firefox/131.0", "Firefox"},
		{"<script>alert(1)</script> Chrome/1", "Chrome"},
	} {
		if got := DescribeUserAgent(tc.ua); got != tc.want {
			t.Errorf("DescribeUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

func TestDescribeUserAgentLongHeader(t *testing.T) {
	// Only the start of a huge header is read.
	ua := "Mozilla/5.0 (Windows NT 10.0) " + strings.Repeat("x", 10_000) + " Firefox/131.0"
	if got := DescribeUserAgent(ua); got != "Browser on Windows" {
		t.Fatalf("DescribeUserAgent(long) = %q", got)
	}
}

// uaLabels is every label DescribeUserAgent can return.
func uaLabels() map[string]bool {
	browsers := []string{"Samsung Internet", "Edge", "Opera", "Vivaldi", "Firefox", "Chrome", "WebView", "Chromium", "Safari"}
	systems := []string{"iPhone", "iPad", "iPod", "Android", "ChromeOS", "Windows", "macOS", "Linux"}
	labels := map[string]bool{"Browser": true}
	for _, b := range browsers {
		labels[b] = true
		for _, s := range systems {
			labels[b+" on "+s] = true
		}
	}
	for _, s := range systems {
		labels["Browser on "+s] = true
	}
	return labels
}

func FuzzDescribeUserAgent(f *testing.F) {
	for _, s := range []string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
		"Mozilla/5.0 (Linux; Android 14; wv) Chrome/129", "curl/8", "", "\xff\xfe",
	} {
		f.Add(s)
	}
	labels := uaLabels()
	f.Fuzz(func(t *testing.T, ua string) {
		if got := DescribeUserAgent(ua); !labels[got] {
			t.Fatalf("DescribeUserAgent(%q) = %q, not one of the fixed labels", ua, got)
		}
	})
}
