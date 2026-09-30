package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// config example without flags re-parses to the defaults, with nothing set (§17).
func TestExampleRoundTripDefaults(t *testing.T) {
	c, ve := loadFlags(t)
	if ve != nil {
		t.Fatal(ve)
	}
	out := string(Example(c))
	back := mustLoad(t, out, nil)
	for _, k := range Keys() {
		if back.IsSet(k.Path) {
			t.Errorf("%s is set by the default example", k.Path)
		}
		if got := formatTOML(registry[keyByPath[k.Path]].get(back)); got != k.DefaultTOML() {
			t.Errorf("%s = %s, want the default %s", k.Path, got, k.DefaultTOML())
		}
	}
	// Every key that is not hidden is in the file, commented out, under its section.
	for _, k := range Keys() {
		line := "# " + k.Name() + " = "
		if k.Hidden == strings.Contains(out, "\n"+line) {
			t.Errorf("%s: hidden %v, but line %q present %v", k.Path, k.Hidden, line, !k.Hidden)
		}
	}
	if !strings.HasPrefix(out, "# isshoni server configuration. Every key also has an ISSHONI_* env variable and a --flag.\n"+
		"# Reference: https://moonwx.github.io/isshoni/reference/config\n") {
		t.Errorf("header:\n%s", out[:200])
	}
	for _, s := range []string{"\n[listen]\n", "\n[tls]\n", "\n[registration]\n\n# Who can create an account", "\n# domain = \"watch.example.com\"\n", "\n# mode = \"auto\"\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("example lacks %q", s)
		}
	}
}

// config example with a value for every key (so every kind, and a string that needs escaping) re-parses to the
// same values, each set from the file.
func TestExampleRoundTripValues(t *testing.T) {
	args := []string{
		"--domain=watch.example.com", "--public-ip=8.8.8.8", "--public-ipv6=off", "--public-url=https://watch.example.com",
		"--data-dir=/srv/\"is\\shoni\"\tü\x01", "--shutdown-timeout=1m30s",
		"--listen.https=:8443", "--listen.http=:8080", "--listen.ice-udp=:9000", "--listen.ice-tcp=",
		"--listen.admin-socket=/tmp/a.sock",
		"--tls.mode=auto", "--tls.acme-email=you@example.com", "--tls.acme-ca=https://ca.example/dir",
		"--tls.acme-staging=false", "--tls.acme-ca-root=", "--tls.cert-file=/c.pem", "--tls.key-file=/k.pem", "--tls.hsts=false",
		"--network.stun-servers=a.example:3478,[2001:db8::1]:3478", "--network.ipv6=false",
		"--network.exclude-interfaces=eth*,wg0", "--network.include-loopback", "--network.udp-buffer-bytes=2097152",
		"--network.trusted-proxies=10.0.0.0/8,fd00::/8",
		"--registration.mode=closed", "--clients.min-version=0.3.0",
		"--limits.max-participants-per-room=12", "--limits.max-shares-per-room=3", "--limits.max-bitrate-kbps=8000",
		"--limits.transfer-alert-gb=500", "--limits.ws-handshakes-per-ip-per-minute=60", "--limits.conns-per-ip=64",
		"--updates.release-check=false", "--updates.release-url=https://releases.example/api",
		"--sfu.pause-unwatched-layers=false", "--push.enabled=false", "--push.subject=mailto:you@example.com",
		"--metrics.enabled", "--metrics.listen=127.0.0.1:9470", "--metrics.pprof", "--log.level=debug", "--log.format=json",
	}
	c, ve := loadFlags(t, args...)
	if ve != nil {
		t.Fatal(ve)
	}
	for _, k := range Keys() {
		if !c.IsSet(k.Path) {
			t.Fatalf("the test sets no value for %s", k.Path)
		}
	}
	out := string(Example(c))
	if err := toml.Unmarshal([]byte(out), new(map[string]any)); err != nil {
		t.Fatalf("example is not TOML: %v\n%s", err, out)
	}
	back := mustLoad(t, out, nil)
	for _, k := range Keys() {
		key := &registry[keyByPath[k.Path]]
		if got, want := formatTOML(key.get(back)), formatTOML(key.get(c)); got != want {
			t.Errorf("%s = %s after the round trip, want %s", k.Path, got, want)
		}
		if src := back.Source(k.Path); src.Kind != SourceFile || src.Line == 0 {
			t.Errorf("%s: source %+v, want a file line", k.Path, src)
		}
	}
	if back.DataDir != "/srv/\"is\\shoni\"\tü\x01" {
		t.Errorf("data_dir = %q", back.DataDir)
	}
}

// config init --tls.mode off with a loopback listen.http writes network.trusted_proxies into the file (§4.4).
func TestExampleOffModeWritesTrustedProxies(t *testing.T) {
	c, ve := loadFlags(t, "--tls.mode", "off")
	if ve != nil {
		t.Fatal(ve)
	}
	out := string(Example(c))
	if !strings.Contains(out, "\ntrusted_proxies = [\"127.0.0.0/8\", \"::1/128\"]\n") {
		t.Errorf("example lacks the trusted_proxies line:\n%s", out)
	}
	if !strings.Contains(out, "\nmode = \"off\"\n") || strings.Contains(out, "\nhttp = ") {
		t.Errorf("example should set tls.mode and leave listen.http commented out:\n%s", out)
	}
	back := mustLoad(t, out, nil)
	want := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	if !back.IsSet("network.trusted_proxies") || !slices.Equal(back.Network.TrustedProxies, want) || back.Listen.HTTP != "127.0.0.1:8080" {
		t.Errorf("round trip: set %v, %v, listen.http %q", back.IsSet("network.trusted_proxies"), back.Network.TrustedProxies, back.Listen.HTTP)
	}

	// Not with a non-loopback listen.http, nor with an explicit value, nor in other modes.
	for _, args := range [][]string{
		{"--tls.mode=off", "--listen.http=0.0.0.0:8080", "--public-url=https://watch.example.com"},
		{"--tls.mode=off", "--network.trusted-proxies=10.0.0.0/8"},
		{"--domain=watch.example.com"},
	} {
		c, ve := loadFlags(t, args...)
		if ve != nil {
			t.Fatal(ve)
		}
		out := string(Example(c))
		if strings.Contains(out, `trusted_proxies = ["127.0.0.0/8", "::1/128"]`) && !strings.Contains(out, "# trusted_proxies") {
			t.Errorf("%v: wrote the loopback default:\n%s", args, out)
		}
	}
}

// The comments name the default where the line shows an example, or the default is derived.
func TestDefaultComment(t *testing.T) {
	for path, want := range map[string]string{
		"domain":                  "Empty by default.",
		"tls.mode":                "Default: auto when domain is set, else ip.",
		"listen.http":             `Default: ":80"; "127.0.0.1:8080" when tls.mode = "off".`,
		"network.trusted_proxies": `Default: []; ["127.0.0.0/8", "::1/128"] when tls.mode = "off" and listen.http is a loopback address.`,
		"data_dir":                "",
	} {
		if got := defaultComment(&registry[keyByPath[path]]); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     `"plain"`,
		`a"b\c`:     `"a\"b\\c"`,
		"tab\tnl\n": `"tab\tnl\n"`,
		"\x00\x7f":  `"\u0000\u007F"`,
		"ü😀":        `"ü😀"`,
		"\xff":      `"` + "�" + `"`,
	} {
		got := tomlString(in)
		if got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
		var m map[string]string
		if err := toml.Unmarshal([]byte("v = "+got), &m); err != nil {
			t.Errorf("tomlString(%q) = %s is not TOML: %v", in, got, err)
		}
	}
}
