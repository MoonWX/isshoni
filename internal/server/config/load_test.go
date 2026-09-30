package config

import (
	"errors"
	"flag"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// layerValues are three distinct values of a kind, for the file, the env and a flag, in each syntax, and the Go
// values they decode to.
type layerValues struct {
	toml [3]string // TOML syntax
	text [3]string // env and flag syntax
	want [3]any
}

func valuesFor(k Key) layerValues {
	switch k.Kind {
	case KindString:
		return layerValues{
			[3]string{`"from-file"`, "", ""}, [3]string{"", "from-env", "from-flag"},
			[3]any{"from-file", "from-env", "from-flag"},
		}
	case KindBool:
		d := k.Default.(bool)
		return layerValues{
			[3]string{formatTOML(!d), "", ""}, [3]string{"", formatTOML(d), formatTOML(!d)},
			[3]any{!d, d, !d},
		}
	case KindInt:
		return layerValues{[3]string{"11", "", ""}, [3]string{"", "22", "33"}, [3]any{11, 22, 33}}
	case KindDuration:
		return layerValues{
			[3]string{`"11s"`, "", ""}, [3]string{"", "22s", "33s"},
			[3]any{11 * time.Second, 22 * time.Second, 33 * time.Second},
		}
	case KindStringList:
		return layerValues{
			[3]string{`["f1", "f2"]`, "", ""}, [3]string{"", "e1, e2,,e3", "g1"},
			[3]any{[]string{"f1", "f2"}, []string{"e1", "e2", "e3"}, []string{"g1"}},
		}
	case KindCIDRList:
		p := netip.MustParsePrefix
		return layerValues{
			[3]string{`["10.0.0.0/8"]`, "", ""}, [3]string{"", "172.16.0.0/12,fd00::/8", "192.168.0.0/16"},
			[3]any{[]netip.Prefix{p("10.0.0.0/8")}, []netip.Prefix{p("172.16.0.0/12"), p("fd00::/8")}, []netip.Prefix{p("192.168.0.0/16")}},
		}
	}
	panic("kind " + k.Kind)
}

// fileFor is a TOML file that sets only k to value; line is the line of k's value.
func fileFor(k Key, value string) (file string, line int) {
	if k.Section() == "" {
		return k.Name() + " = " + value + "\n", 1
	}
	return "# comment\n[" + k.Section() + "]\n" + k.Name() + " = " + value + "\n", 3
}

// Precedence flag > env > file > default, for every key (so every kind), with the source of each value.
func TestPrecedenceEveryKey(t *testing.T) {
	for _, k := range Keys() {
		t.Run(k.Path, func(t *testing.T) {
			v := valuesFor(k)
			file, line := fileFor(k, v.toml[0])
			env := []string{k.EnvName() + "=" + v.text[1]}
			flagArg := "--" + k.FlagName() + "=" + v.text[2]
			key := &registry[keyByPath[k.Path]]

			check := func(c *Config, want any, src Source) {
				t.Helper()
				if got := key.get(c); formatTOML(got) != formatTOML(want) {
					t.Errorf("value %s, want %s", formatTOML(got), formatTOML(want))
				}
				got := c.Source(k.Path)
				if src.Kind == SourceFile {
					src.File = got.File // a temp path
				}
				if got != src {
					t.Errorf("source %+v, want %+v", got, src)
				}
				if c.IsSet(k.Path) != (src.Kind != SourceDefault) {
					t.Errorf("IsSet = %v with source %s", c.IsSet(k.Path), src.Kind)
				}
			}
			c, _ := testLoad(t, "", nil)
			check(c, k.Default, Source{Kind: SourceDefault})
			c, _ = testLoad(t, file, nil)
			check(c, v.want[0], Source{Kind: SourceFile, Line: line})
			if !strings.HasSuffix(c.Source(k.Path).File, "isshoni.toml") {
				t.Errorf("file source %+v", c.Source(k.Path))
			}
			c, _ = testLoad(t, file, env)
			check(c, v.want[1], Source{Kind: SourceEnv, Name: k.EnvName()})
			c, _ = testLoad(t, file, env, flagArg)
			check(c, v.want[2], Source{Kind: SourceFlag, Name: "--" + k.FlagName()})
			c, _ = testLoad(t, "", nil, flagArg)
			check(c, v.want[2], Source{Kind: SourceFlag, Name: "--" + k.FlagName()})
		})
	}
}

// Policy keys: IsSet only when a flag, env or the file sets them (the wiring pins exactly those, §4.6).
func TestPolicyIsSet(t *testing.T) {
	c := mustLoad(t, "[registration]\nmode = \"closed\"\n", []string{"ISSHONI_LIMITS_MAX_BITRATE_KBPS=4000"},
		"--updates.release-check=false")
	for _, k := range Keys() {
		if !k.Policy {
			continue
		}
		want := k.Path == "registration.mode" || k.Path == "limits.max_bitrate_kbps" || k.Path == "updates.release_check"
		if c.IsSet(k.Path) != want {
			t.Errorf("IsSet(%s) = %v, want %v", k.Path, c.IsSet(k.Path), want)
		}
	}
	if c.Registration.Mode != "closed" || c.Limits.MaxBitrateKbps != 4000 || c.Updates.ReleaseCheck {
		t.Errorf("policy values: %q %d %v", c.Registration.Mode, c.Limits.MaxBitrateKbps, c.Updates.ReleaseCheck)
	}
	// A value equal to the default still counts as set.
	c = mustLoad(t, "", []string{"ISSHONI_REGISTRATION_MODE=invite"})
	if !c.IsSet("registration.mode") {
		t.Error("an env value equal to the default is not IsSet")
	}
}

// An env variable set to "" counts as unset (§4.2): compose's ${VAR:-}.
func TestEmptyEnvIsUnset(t *testing.T) {
	c := mustLoad(t, "", []string{"ISSHONI_PUBLIC_IP=", "ISSHONI_DOMAIN="})
	if c.PublicIP != "auto" || c.Domain != "" || c.EffectiveTLSMode() != TLSIP {
		t.Errorf("public_ip %q, domain %q, mode %q", c.PublicIP, c.Domain, c.EffectiveTLSMode())
	}
	if c.IsSet("public_ip") || c.IsSet("domain") || c.Source("public_ip").Kind != SourceDefault {
		t.Errorf("empty env counted as set: %+v %+v", c.Source("public_ip"), c.Source("domain"))
	}
	if len(c.Problems()) != 0 {
		t.Errorf("problems:%s", problemList(c.Problems()))
	}
	// An empty env doesn't hide the file either.
	c = mustLoad(t, "public_ip = \"auto\"\n", []string{"ISSHONI_PUBLIC_IP="})
	if c.Source("public_ip").Kind != SourceFile {
		t.Errorf("source %+v, want the file", c.Source("public_ip"))
	}
	// A flag may set an empty string.
	c = mustLoad(t, "", []string{"ISSHONI_LISTEN_ICE_UDP=:9000"}, "--listen.ice-udp=")
	if c.Listen.ICEUDP != "" || c.Source("listen.ice_udp").Kind != SourceFlag {
		t.Errorf("listen.ice_udp = %q from %+v", c.Listen.ICEUDP, c.Source("listen.ice_udp"))
	}
}

// Reserved names and env-only switches are ignored without a warning; other unknown ISSHONI_* names warn with a
// suggestion; non-ISSHONI variables are not looked at.
func TestEnvNames(t *testing.T) {
	var env []string
	for _, n := range ReservedEnvNames() {
		if n != EnvConfig {
			env = append(env, n+"=x")
		}
	}
	env = append(env, EnvInContainer+"=1", EnvAllowEphemeralData+"=1", "PATH=/bin", "isshoni_tls_mode=bogus",
		"ISSHONI_TLS_MOD=ip", "ISSHONI_TOTALLY_UNKNOWN=1", "ISSHONI_EMPTY_UNKNOWN=")
	c := mustLoad(t, "", env)
	ws := c.Warnings()
	if len(ws) != 2 {
		t.Fatalf("warnings:%s\nwant two", problemList(ws))
	}
	w := ws[0]
	if w.Source != (Source{Kind: SourceEnv, Name: "ISSHONI_TLS_MOD"}) || w.Severity != SeverityWarning ||
		w.Fix != "did you mean ISSHONI_TLS_MODE?" || w.Key != "" {
		t.Errorf("warning %+v", w)
	}
	if got := w.String(); got != "config warning: ISSHONI_TLS_MOD (env) is not a config key; it is ignored.\n  fix: did you mean ISSHONI_TLS_MODE?" {
		t.Errorf("warning text %q", got)
	}
	if ws[1].Source.Name != "ISSHONI_TOTALLY_UNKNOWN" || !strings.Contains(ws[1].Fix, "config example") {
		t.Errorf("warning %+v", ws[1])
	}
	if c.TLS.Mode != "" {
		t.Errorf("tls.mode = %q from an unknown or lower-case variable", c.TLS.Mode)
	}
}

// The last of several assignments of one variable wins, as in a shell.
func TestEnvLastWins(t *testing.T) {
	c := mustLoad(t, "", []string{"ISSHONI_LOG_LEVEL=debug", "ISSHONI_LOG_LEVEL=warn"})
	if c.Log.Level != "warn" {
		t.Errorf("log.level = %q", c.Log.Level)
	}
}

// Unknown file keys are errors with the line and column and a "did you mean" suggestion (§4.2).
func TestUnknownFileKeys(t *testing.T) {
	file := strings.Join([]string{
		`domian = "watch.example.com"`, // 1: typo of a top-level key
		`Log.Level = "info"`,           // 2: case
		`[tls]`,                        // 3
		`mdoe = "ip"`,                  // 4: typo inside a section
		`[listen]`,                     // 5
		`  admin-socket = "/x.sock"`,   // 6: dash for underscore, indented
		`[tsl]`,                        // 7: unknown section
		`mode = "ip"`,                  // 8
		`[network]`,                    // 9
		`stun = {servers = ["a:1"]}`,   // 10: inline table
		`completely_unrelated = 1`,     // 11
		`[empty]`,                      // 12: an empty unknown table
		`[log]`,                        // 13
		`public_ip = "8.8.8.8"`,        // 14: a top-level key below a section header
		`admin_socket = "/x.sock"`,     // 15: a key of another section
		`mode = "ip"`,                  // 16: a name two sections share
		`[push]`,                       // 17
		`domain = "watch.example.com"`, // 18: a top-level key below a section header
		``,
	}, "\n")
	_, ve := testLoad(t, file, nil)
	if ve == nil {
		t.Fatal("no error")
	}
	want := []struct {
		key       string
		line, col int
		fix       string
	}{
		{"domian", 1, 1, "did you mean domain?"},
		{"Log.Level", 2, 5, "did you mean log.level? (level = … under [log])"},
		{"tls.mdoe", 4, 1, "did you mean tls.mode? (mode = … under [tls])"},
		{"listen.admin-socket", 6, 3, "did you mean listen.admin_socket? (admin_socket = … under [listen])"},
		{"tsl.mode", 8, 1, "did you mean tls.mode? (mode = … under [tls])"},
		{"network.stun.servers", 10, 9, "did you mean network.stun_servers? (stun_servers = … under [network])"},
		{"network.completely_unrelated", 11, 1, "remove it; 'isshoni config example' prints every key"},
		{"empty", 12, 2, "remove it; 'isshoni config example' prints every key"},
		{"log.public_ip", 14, 1, "public_ip is a top-level key: move the line above the first [section] header"},
		{"log.admin_socket", 15, 1, "did you mean listen.admin_socket? (admin_socket = … under [listen])"},
		{"log.mode", 16, 1, "remove it; 'isshoni config example' prints every key"},
		{"push.domain", 18, 1, "domain is a top-level key: move the line above the first [section] header"},
	}
	for _, w := range want {
		p, ok := findProblem(ve.Problems, w.key, "is not a config key")
		if !ok {
			t.Errorf("no problem for %s:%s", w.key, problemList(ve.Problems))
			continue
		}
		if p.Source.Line != w.line || p.Source.Column != w.col || p.Fix != w.fix || p.Severity != SeverityError {
			t.Errorf("%s: %+v, want line %d col %d fix %q", w.key, p, w.line, w.col, w.fix)
		}
	}
	if len(ve.Problems) != len(want) {
		t.Errorf("%d problems, want %d:%s", len(ve.Problems), len(want), problemList(ve.Problems))
	}
	// The common case of the example file: a key that belongs above [tls] written below it.
	_, ve2 := testLoad(t, "[tls]\nmode = \"auto\"\ndomain = \"watch.example.com\"\n", nil)
	if ve2 == nil {
		t.Fatal("tls.domain: no error")
	}
	if p, ok := findProblem(ve2.Problems, "tls.domain", "is not a config key"); !ok || p.Source.Line != 3 ||
		p.Fix != "domain is a top-level key: move the line above the first [section] header" {
		t.Errorf("tls.domain:%s", problemList(ve2.Problems))
	}
	p, _ := findProblem(ve.Problems, "tls.mdoe", "")
	if !strings.HasPrefix(p.String(), "config error: tls.mdoe (file ") || !strings.Contains(p.String(), "isshoni.toml:4:1) is not a config key.\n  fix: did you mean tls.mode?") {
		t.Errorf("text %q", p.String())
	}
}

// A section written as a value, or as an array of tables, is an error at its line.
func TestSectionAsValue(t *testing.T) {
	for _, file := range []string{"tls = \"ip\"\n", "[[tls]]\nmode = \"ip\"\n"} {
		_, ve := testLoad(t, file, nil)
		if ve == nil {
			t.Fatalf("%q: no error", file)
		}
		p, ok := findProblem(ve.Problems, "tls", "is a section")
		if !ok || p.Source.Line != 1 || !strings.Contains(p.Fix, "[tls]") {
			t.Errorf("%q:%s", file, problemList(ve.Problems))
		}
	}
}

// A file that Windows Notepad saved, with a UTF-8 byte order mark and CRLF line ends, loads, and its line numbers
// are the editor's.
func TestFileBOMAndCRLF(t *testing.T) {
	file := "\xef\xbb\xbf# isshoni\r\ndomain = \"watch.example.com\"\r\n\r\n[log]\r\nlevel = \"debug\"\r\n"
	c := mustLoad(t, file, nil)
	if c.Domain != "watch.example.com" || c.Log.Level != "debug" {
		t.Errorf("values %q %q", c.Domain, c.Log.Level)
	}
	if s := c.Source("domain"); s.Kind != SourceFile || s.Line != 2 {
		t.Errorf("domain source %+v", s)
	}
	if s := c.Source("log.level"); s.Line != 5 {
		t.Errorf("log.level source %+v", s)
	}
	// A key on the first line keeps column 1 after the mark.
	_, ve := testLoad(t, "\xef\xbb\xbfdomian = \"a\"\r\n", nil)
	if ve == nil {
		t.Fatal("no error")
	}
	if p, ok := findProblem(ve.Problems, "domian", "is not a config key"); !ok || p.Source.Line != 1 || p.Source.Column != 1 {
		t.Errorf("problems:%s", problemList(ve.Problems))
	}
}

// Dotted keys and inline tables are ordinary TOML and work like sections.
func TestDottedAndInlineKeys(t *testing.T) {
	c := mustLoad(t, "tls.mode = \"off\"\nlisten = {http = \"127.0.0.1:9000\"}\n\n[log]\nlevel = \"debug\"\n", nil)
	if c.TLS.Mode != TLSOff || c.Listen.HTTP != "127.0.0.1:9000" || c.Log.Level != "debug" {
		t.Errorf("values %q %q %q", c.TLS.Mode, c.Listen.HTTP, c.Log.Level)
	}
	if s := c.Source("listen.http"); s.Line != 2 || s.Kind != SourceFile {
		t.Errorf("listen.http source %+v", s)
	}
	if s := c.Source("log.level"); s.Line != 5 {
		t.Errorf("log.level source %+v", s)
	}
}

// Values of the wrong type are errors naming the key, the source and a fix; every source is checked.
func TestValueParsing(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		env   []string
		args  []string
		key   string
		src   SourceKind
		value string
		fix   string
	}{
		{"file int as string", "[network]\nudp_buffer_bytes = \"big\"\n", nil, nil, "network.udp_buffer_bytes", SourceFile, `"big"`, "without quotes"},
		{"file float", "[limits]\nconns_per_ip = 1.5\n", nil, nil, "limits.conns_per_ip", SourceFile, "1.5", "whole number"},
		{"file bool as string", "[tls]\nhsts = \"yes\"\n", nil, nil, "tls.hsts", SourceFile, `"yes"`, "true or false"},
		{"file string as int", "domain = 7\n", nil, nil, "domain", SourceFile, "7", "double quotes"},
		{"file duration as int", "shutdown_timeout = 10\n", nil, nil, "shutdown_timeout", SourceFile, "10", `"10s"`},
		{"file bad duration", "shutdown_timeout = \"10 seconds\"\n", nil, nil, "shutdown_timeout", SourceFile, `"10 seconds"`, "Go syntax"},
		{"file list as string", "[network]\nstun_servers = \"a:1\"\n", nil, nil, "network.stun_servers", SourceFile, `"a:1"`, "list of strings"},
		{"file list of ints", "[network]\nstun_servers = [1]\n", nil, nil, "network.stun_servers", SourceFile, "[1]", "list of strings"},
		{"file bad cidr", "[network]\ntrusted_proxies = [\"10.0.0.1\"]\n", nil, nil, "network.trusted_proxies", SourceFile, `["10.0.0.1"]`, "10.0.0.1/32"},
		{"file huge int", "[limits]\nconns_per_ip = 99999999999\n", nil, nil, "limits.conns_per_ip", SourceFile, "99999999999", "smaller"},
		{"file table for a key", "domain = {a = 1}\n", nil, nil, "domain", SourceFile, "{…}", "double quotes"},
		{"env bool", "", []string{"ISSHONI_TLS_HSTS=yes"}, nil, "tls.hsts", SourceEnv, `"yes"`, "true, false, 1 or 0"},
		{"env int", "", []string{"ISSHONI_LIMITS_CONNS_PER_IP=many"}, nil, "limits.conns_per_ip", SourceEnv, `"many"`, "whole number"},
		{"env duration", "", []string{"ISSHONI_SHUTDOWN_TIMEOUT=10"}, nil, "shutdown_timeout", SourceEnv, `"10"`, "Go syntax"},
		{"env cidr", "", []string{"ISSHONI_NETWORK_TRUSTED_PROXIES=10.0.0.0/8,nope"}, nil, "network.trusted_proxies", SourceEnv, `"10.0.0.0/8,nope"`, "prefix length"},
		{"flag bool", "", nil, []string{"--metrics.enabled=maybe"}, "metrics.enabled", SourceFlag, `"maybe"`, "true, false"},
		{"flag int", "", nil, []string{"--network.udp-buffer-bytes", "8M"}, "network.udp_buffer_bytes", SourceFlag, `"8M"`, "whole number"},
		{"flag duration", "", nil, []string{"--shutdown-timeout", "soon"}, "shutdown_timeout", SourceFlag, `"soon"`, "Go syntax"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ve := testLoad(t, tt.file, tt.env, tt.args...)
			if ve == nil {
				t.Fatal("no error")
			}
			p, ok := findProblem(ve.Problems, tt.key, "")
			if !ok {
				t.Fatalf("no problem for %s:%s", tt.key, problemList(ve.Problems))
			}
			if p.Source.Kind != tt.src || p.Value != tt.value || !strings.Contains(p.Fix, tt.fix) || p.Severity != SeverityError {
				t.Errorf("%+v; want source %s, value %s, fix containing %q", p, tt.src, tt.value, tt.fix)
			}
			// The value that could not be used stays at its default.
			k := registry[keyByPath[tt.key]]
			if formatTOML(k.get(c)) != formatTOML(k.Default) {
				t.Errorf("%s = %s after a bad value, want the default", tt.key, formatTOML(k.get(c)))
			}
		})
	}
}

// Booleans take true/false/1/0 in env and flags; a bare bool flag means true; lists are comma-separated with
// spaces and empty elements dropped; durations use Go syntax.
func TestTextSyntax(t *testing.T) {
	c := mustLoad(t, "", []string{"ISSHONI_TLS_HSTS=0", "ISSHONI_NETWORK_IPV6=FALSE", "ISSHONI_PUSH_ENABLED=1",
		"ISSHONI_SHUTDOWN_TIMEOUT=1m30s", "ISSHONI_NETWORK_EXCLUDE_INTERFACES= eth9 ,, wg* "},
		"--metrics.pprof", "--network.include-loopback=true", "--network.stun-servers=")
	if c.TLS.HSTS || c.Network.IPv6 || !c.Push.Enabled || !c.Metrics.PProf || !c.Network.IncludeLoopback {
		t.Errorf("bools: hsts %v ipv6 %v push %v pprof %v loopback %v", c.TLS.HSTS, c.Network.IPv6, c.Push.Enabled,
			c.Metrics.PProf, c.Network.IncludeLoopback)
	}
	if c.ShutdownTimeout.Duration != 90*time.Second {
		t.Errorf("shutdown_timeout = %v", c.ShutdownTimeout)
	}
	if !slices.Equal(c.Network.ExcludeInterfaces, []string{"eth9", "wg*"}) {
		t.Errorf("exclude_interfaces = %q", c.Network.ExcludeInterfaces)
	}
	if c.Network.STUNServers == nil || len(c.Network.STUNServers) != 0 || !c.IsSet("network.stun_servers") {
		t.Errorf("stun_servers = %#v (set %v), want an explicit empty list", c.Network.STUNServers, c.IsSet("network.stun_servers"))
	}
	// An empty TOML list works too (06's dev config: stun_servers = []).
	c = mustLoad(t, "[network]\nstun_servers = []\n", nil)
	if len(c.Network.STUNServers) != 0 || !c.IsSet("network.stun_servers") {
		t.Errorf("stun_servers = %q", c.Network.STUNServers)
	}
}

// Duration marshals as Go syntax.
func TestDurationText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("168h")); err != nil || d.Duration != 168*time.Hour {
		t.Fatalf("UnmarshalText: %v %v", d, err)
	}
	if b, _ := d.MarshalText(); string(b) != "168h0m0s" {
		t.Errorf("MarshalText = %s", b)
	}
	if err := d.UnmarshalText([]byte("x")); err == nil {
		t.Error("UnmarshalText accepted x")
	}
}

func TestDerivedTLSMode(t *testing.T) {
	tests := []struct {
		env  []string
		want TLSMode
	}{
		{nil, TLSIP},
		{[]string{"ISSHONI_DOMAIN=watch.example.com"}, TLSAuto},
		{[]string{"ISSHONI_DOMAIN=watch.example.com", "ISSHONI_TLS_MODE=ip"}, TLSIP},
		{[]string{"ISSHONI_TLS_MODE=off"}, TLSOff},
		{[]string{"ISSHONI_TLS_MODE=manual", "ISSHONI_TLS_CERT_FILE=c", "ISSHONI_TLS_KEY_FILE=k"}, TLSManual},
	}
	for _, tt := range tests {
		c := mustLoad(t, "", tt.env)
		if got := c.EffectiveTLSMode(); got != tt.want {
			t.Errorf("%v: effective mode %q, want %q", tt.env, got, tt.want)
		}
	}
}

// listen.http defaults to 127.0.0.1:8080 in off mode (§4.3), unless it is set.
func TestOffModeHTTPDefault(t *testing.T) {
	if c := mustLoad(t, "", nil); c.Listen.HTTP != ":80" {
		t.Errorf("listen.http = %q, want :80", c.Listen.HTTP)
	}
	c := mustLoad(t, "", nil, "--tls.mode", "off")
	if c.Listen.HTTP != "127.0.0.1:8080" || c.IsSet("listen.http") {
		t.Errorf("off mode: listen.http = %q (set %v)", c.Listen.HTTP, c.IsSet("listen.http"))
	}
	c = mustLoad(t, "", nil, "--tls.mode", "off", "--listen.http", "127.0.0.1:9999")
	if c.Listen.HTTP != "127.0.0.1:9999" {
		t.Errorf("off mode with listen.http set: %q", c.Listen.HTTP)
	}
}

// network.trusted_proxies in off mode (§4.4): loopback default with a loopback listen.http; empty plus a warning
// with a non-loopback one; an explicit value, even [], wins.
func TestOffModeTrustedProxies(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

	c := mustLoad(t, "", nil, "--tls.mode=off")
	if !slices.Equal(c.Network.TrustedProxies, loopback) || c.IsSet("network.trusted_proxies") || len(c.Warnings()) != 0 {
		t.Errorf("loopback listen.http: %v (set %v), warnings:%s", c.Network.TrustedProxies, c.IsSet("network.trusted_proxies"), problemList(c.Warnings()))
	}
	c = mustLoad(t, "", nil, "--tls.mode=off", "--listen.http=[::1]:8080")
	if !slices.Equal(c.Network.TrustedProxies, loopback) {
		t.Errorf("[::1] listen.http: %v", c.Network.TrustedProxies)
	}

	c = mustLoad(t, "", nil, "--tls.mode=off", "--listen.http=0.0.0.0:8080", "--public-url=https://watch.example.com")
	if len(c.Network.TrustedProxies) != 0 {
		t.Errorf("non-loopback listen.http: %v", c.Network.TrustedProxies)
	}
	w, ok := findProblem(c.Warnings(), "network.trusted_proxies", "every client IP will be the proxy's")
	if !ok || w.Source.Kind != SourceDefault || !strings.Contains(w.Fix, "172.30.89.0/24") {
		t.Errorf("warning:%s", problemList(c.Warnings()))
	}

	for _, args := range [][]string{
		{"--tls.mode=off", "--network.trusted-proxies="},
		{"--tls.mode=off", "--listen.http=0.0.0.0:8080", "--public-url=https://watch.example.com", "--network.trusted-proxies="},
	} {
		c = mustLoad(t, "", nil, args...)
		if len(c.Network.TrustedProxies) != 0 || !c.IsSet("network.trusted_proxies") || len(c.Warnings()) != 0 {
			t.Errorf("%v: %v, warnings:%s", args, c.Network.TrustedProxies, problemList(c.Warnings()))
		}
	}
	// The file form of an explicit [].
	c = mustLoad(t, "[tls]\nmode = \"off\"\n[network]\ntrusted_proxies = []\n", nil)
	if len(c.Network.TrustedProxies) != 0 {
		t.Errorf("file []: %v", c.Network.TrustedProxies)
	}
	// Other modes never get the loopback default.
	if c := mustLoad(t, "", nil); len(c.Network.TrustedProxies) != 0 {
		t.Errorf("ip mode: %v", c.Network.TrustedProxies)
	}
}

func TestConfigFile(t *testing.T) {
	// A missing file at the default path is fine.
	c := mustLoad(t, "", nil)
	if path, read := c.File(); path != defaultPath || read {
		t.Errorf("File() = %q, %v", path, read)
	}

	// A missing file at an explicit path is an error, from --config or ISSHONI_CONFIG; --config wins.
	missing := filepath.Join(t.TempDir(), "nope.toml")
	for _, tt := range []struct {
		env  []string
		args []string
		src  Source
	}{
		{nil, []string{"--config", missing}, Source{Kind: SourceFlag, Name: "--config"}},
		{[]string{EnvConfig + "=" + missing}, nil, Source{Kind: SourceEnv, Name: EnvConfig}},
		{[]string{EnvConfig + "=" + writeFile(t, "")}, []string{"--config=" + missing}, Source{Kind: SourceFlag, Name: "--config"}},
	} {
		_, ve := testLoad(t, "", tt.env, tt.args...)
		if ve == nil || len(ve.Problems) != 1 {
			t.Fatalf("%v %v: %v", tt.env, tt.args, ve)
		}
		p := ve.Problems[0]
		if p.Source != tt.src || !strings.Contains(p.Message, missing) || !strings.Contains(p.Fix, "config init") {
			t.Errorf("%+v", p)
		}
	}

	// ISSHONI_CONFIG= (empty) is unset.
	c = mustLoad(t, "", []string{EnvConfig + "="})
	if path, _ := c.File(); path != defaultPath {
		t.Errorf("empty ISSHONI_CONFIG: file %q", path)
	}

	// A file that is read reports it.
	path := writeFile(t, "# nothing\n")
	c = mustLoad(t, "", nil, "--config", path)
	if p, read := c.File(); p != path || !read {
		t.Errorf("File() = %q, %v", p, read)
	}
}

func TestFileSyntaxError(t *testing.T) {
	_, ve := testLoad(t, "domain = \"a\"\n[tls]\nmode = \n", nil)
	if ve == nil || len(ve.Problems) != 1 {
		t.Fatalf("problems: %v", ve)
	}
	p := ve.Problems[0]
	if p.Source.Kind != SourceFile || p.Source.Line != 3 || p.Source.Column == 0 || !strings.Contains(p.Message, "is not valid TOML") || p.Fix == "" {
		t.Errorf("%+v", p)
	}
	if !strings.HasPrefix(p.String(), "config error: file ") || !strings.Contains(p.String(), "isshoni.toml:3:") {
		t.Errorf("text %q", p.String())
	}
	// A duplicate key is a syntax error too, at its line.
	_, ve = testLoad(t, "[log]\nlevel = \"info\"\nlevel = \"debug\"\n", nil)
	if ve == nil || ve.Problems[0].Source.Line != 3 {
		t.Errorf("duplicate key: %v", ve)
	}
}

func TestFilePermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root unix user")
	}
	path := writeFile(t, "")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, ve := testLoad(t, "", nil, "--config", path)
	if ve == nil || !strings.Contains(ve.Problems[0].Message, "permission denied") || !strings.Contains(ve.Problems[0].Fix, "sudo") {
		t.Errorf("%v", ve)
	}
}

// A usage error (unknown flag, missing argument, -h) is returned as is, with a nil config.
func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"--tls.mode"}, {"-h"}} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		c, err := Load(fs, args, nil)
		var ve *ValidationError
		if c != nil || err == nil || errors.As(err, &ve) {
			t.Errorf("%v: %v, %v", args, c, err)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := Load(fs, []string{"-h"}, nil); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
	// Positional arguments are left for the caller.
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	if _, err := Load(fs, []string{"--log.level", "debug", "rest", "--x"}, nil); err != nil || !slices.Equal(fs.Args(), []string{"rest", "--x"}) {
		t.Errorf("args %v, %v", fs.Args(), err)
	}
}

// LoadFlags reads neither the file nor the environment and has no --config flag (config example and init).
func TestLoadFlagsIgnoresFileAndEnv(t *testing.T) {
	t.Setenv("ISSHONI_DOMAIN", "watch.example.com") // Load gets environ explicitly; this proves LoadFlags ignores os.Environ too
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c, err := LoadFlags(fs, []string{"--log.level=debug"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "" || c.Log.Level != "debug" || fs.Lookup("config") != nil {
		t.Errorf("domain %q, level %q, --config %v", c.Domain, c.Log.Level, fs.Lookup("config"))
	}
	if path, read := c.File(); path != "" || read {
		t.Errorf("File() = %q, %v", path, read)
	}
}

// The repository's dev and e2e configs (06 §7.3) load without problems, with the per-server flags of the e2e
// fixture, and give the dev site.
func TestDevConfigs(t *testing.T) {
	for _, tt := range []struct {
		file string
		args []string
	}{
		{"isshoni.dev.toml", nil},
		{"isshoni.e2e.toml", []string{
			"--listen.http", "127.0.0.1:40001", "--listen.ice-udp", ":40002", "--listen.ice-tcp", ":40003",
			"--public-url", "http://127.0.0.1:40001", "--listen.admin-socket", filepath.Join(os.TempDir(), "isshoni-e2e-1-1.sock"),
		}},
	} {
		path := filepath.Join("..", "..", "..", "deploy", "dev", tt.file)
		c := mustLoad(t, "", []string{"ISSHONI_DATA_DIR=" + t.TempDir()}, append([]string{"--config", path}, tt.args...)...)
		if len(c.Problems()) != 0 {
			t.Errorf("%s:%s", tt.file, problemList(c.Problems()))
		}
		site, err := NewSite(c, netip.Addr{}, netip.Addr{})
		if err != nil || !site.Dev || site.TLSMode != TLSOff {
			t.Errorf("%s: site %+v, %v", tt.file, site, err)
		}
	}
}

// Problems come back errors first, and a config with errors is still filled in.
func TestValidationErrorKeepsConfig(t *testing.T) {
	c, ve := testLoad(t, "", nil, "--tls.mode=auto", "--metrics.enabled", "--metrics.listen=0.0.0.0:9469", "--log.level=debug")
	if ve == nil {
		t.Fatal("no error")
	}
	if ve.Problems[0].Severity != SeverityError || ve.Problems[len(ve.Problems)-1].Severity != SeverityWarning {
		t.Errorf("order:%s", problemList(ve.Problems))
	}
	if c.Log.Level != "debug" || len(c.Warnings()) != 1 || len(c.Problems()) != len(ve.Problems) {
		t.Errorf("config not filled in: %q, %d warnings", c.Log.Level, len(c.Warnings()))
	}
	if !strings.Contains(ve.Error(), "config error: tls.mode = \"auto\" (flag --tls.mode) needs a domain.\n  fix: ") ||
		!strings.Contains(ve.Error(), "\nconfig warning: metrics.listen") {
		t.Errorf("Error():\n%s", ve.Error())
	}
}
