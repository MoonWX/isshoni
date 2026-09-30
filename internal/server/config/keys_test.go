package config

import (
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every M1 key of 04 §4.3, including 01's guard, 02's sfu.pause_unwatched_layers and the policy keys, in the
// documentation order.
var m1Keys = []string{
	"domain", "public_ip", "public_ipv6", "public_url", "data_dir", "shutdown_timeout",
	"listen.https", "listen.http", "listen.ice_udp", "listen.ice_tcp", "listen.admin_socket",
	"tls.mode", "tls.acme_email", "tls.acme_ca", "tls.acme_staging", "tls.acme_ca_root", "tls.cert_file",
	"tls.key_file", "tls.hsts",
	"network.stun_servers", "network.ipv6", "network.exclude_interfaces", "network.include_loopback",
	"network.udp_buffer_bytes", "network.trusted_proxies",
	"registration.mode", "clients.min_version",
	"limits.max_participants_per_room", "limits.max_shares_per_room", "limits.max_bitrate_kbps",
	"limits.transfer_alert_gb", "limits.ws_handshakes_per_ip_per_minute", "limits.conns_per_ip",
	"updates.release_check", "updates.release_url",
	"sfu.pause_unwatched_layers",
	"push.enabled", "push.subject",
	"metrics.enabled", "metrics.listen", "metrics.pprof",
	"log.level", "log.format",
}

func TestRegistryHasEveryM1Key(t *testing.T) {
	var got []string
	for _, k := range Keys() {
		got = append(got, k.Path)
	}
	if !slices.Equal(got, m1Keys) {
		t.Errorf("registry keys:\n got %v\nwant %v", got, m1Keys)
	}
}

// The policy keys of §4.3 and the 03 settings fields they pin (§4.6), with 03 §9's defaults.
func TestPolicyKeys(t *testing.T) {
	want := map[string]struct {
		setting string
		def     any
	}{
		"registration.mode":                {"registrationMode", "invite"},
		"clients.min_version":              {"minClientVersion", ""},
		"limits.max_participants_per_room": {"maxParticipantsPerRoom", 0},
		"limits.max_shares_per_room":       {"maxSharesPerRoom", 0},
		"limits.max_bitrate_kbps":          {"maxShareBitrateKbps", 0},
		"limits.transfer_alert_gb":         {"transferAlertGb", 0},
		"updates.release_check":            {"updateCheck", true},
	}
	for _, k := range Keys() {
		w, ok := want[k.Path]
		if k.Policy != ok {
			t.Errorf("%s: Policy = %v, want %v", k.Path, k.Policy, ok)
			continue
		}
		if !ok {
			if k.Setting != "" {
				t.Errorf("%s: Setting %q on a non-policy key", k.Path, k.Setting)
			}
			continue
		}
		if k.Setting != w.setting || k.Default != w.def {
			t.Errorf("%s: Setting %q, Default %v; want %q, %v", k.Path, k.Setting, k.Default, w.setting, w.def)
		}
	}
}

// The guards and 02's key keep their documented defaults (01 §3, 02 §11).
func TestGuardDefaults(t *testing.T) {
	for path, want := range map[string]any{
		"limits.ws_handshakes_per_ip_per_minute": 20,
		"limits.conns_per_ip":                    256,
		"sfu.pause_unwatched_layers":             true,
	} {
		k, ok := Lookup(path)
		if !ok || k.Default != want || k.Policy {
			t.Errorf("%s: %+v, want default %v and not a policy key", path, k, want)
		}
	}
}

// No two keys share an env or flag name, and none is a reserved or env-only name.
func TestRegistryNamesUnique(t *testing.T) {
	envs := map[string]string{}
	flags := map[string]string{"config": "--config"}
	for _, k := range Keys() {
		env := k.EnvName()
		if slices.Contains(ReservedEnvNames(), env) || slices.Contains(envOnly, env) {
			t.Errorf("%s: env name %s is reserved", k.Path, env)
		}
		if other, dup := envs[env]; dup {
			t.Errorf("%s: env name %s is also %s's", k.Path, env, other)
		}
		envs[env] = k.Path
		if other, dup := flags[k.FlagName()]; dup {
			t.Errorf("%s: flag --%s is also %s's", k.Path, k.FlagName(), other)
		}
		flags[k.FlagName()] = k.Path
	}
}

func TestNaming(t *testing.T) {
	tests := []struct{ path, env, flag string }{
		{"tls.mode", "ISSHONI_TLS_MODE", "tls.mode"},
		{"public_ip", "ISSHONI_PUBLIC_IP", "public-ip"},
		{"listen.admin_socket", "ISSHONI_LISTEN_ADMIN_SOCKET", "listen.admin-socket"},
		{"network.trusted_proxies", "ISSHONI_NETWORK_TRUSTED_PROXIES", "network.trusted-proxies"},
		{"limits.max_bitrate_kbps", "ISSHONI_LIMITS_MAX_BITRATE_KBPS", "limits.max-bitrate-kbps"},
		{"sfu.pause_unwatched_layers", "ISSHONI_SFU_PAUSE_UNWATCHED_LAYERS", "sfu.pause-unwatched-layers"},
	}
	for _, tt := range tests {
		k, ok := Lookup(tt.path)
		if !ok {
			t.Fatalf("no key %s", tt.path)
		}
		if k.EnvName() != tt.env || k.FlagName() != tt.flag {
			t.Errorf("%s: env %s flag %s, want %s %s", tt.path, k.EnvName(), k.FlagName(), tt.env, tt.flag)
		}
	}
	if k, _ := Lookup("tls.mode"); k.Section() != "tls" || k.Name() != "mode" {
		t.Errorf("tls.mode: section %q name %q", k.Section(), k.Name())
	}
	if k, _ := Lookup("domain"); k.Section() != "" || k.Name() != "domain" {
		t.Errorf("domain: section %q name %q", k.Section(), k.Name())
	}
}

// Each key's path matches the toml tags of the Config field it points to, its Kind matches the field's type, and
// its Default has the Go type of its Kind. The registry and Config can't drift apart.
func TestRegistryMatchesConfig(t *testing.T) {
	var c Config
	root := reflect.ValueOf(&c).Elem()
	seen := map[uintptr]string{}
	for i := range registry {
		k := &registry[i]
		ptr := reflect.ValueOf(k.field(&c))
		if prev, dup := seen[ptr.Pointer()]; dup {
			t.Errorf("%s and %s point to the same field", k.Path, prev)
		}
		seen[ptr.Pointer()] = k.Path
		if got := tomlPathOf(root, ptr.Pointer()); got != k.Path {
			t.Errorf("%s points to the field with toml path %q", k.Path, got)
		}
		wantKind := map[reflect.Type]Kind{
			reflect.TypeFor[*string]():         KindString,
			reflect.TypeFor[*TLSMode]():        KindString,
			reflect.TypeFor[*bool]():           KindBool,
			reflect.TypeFor[*int]():            KindInt,
			reflect.TypeFor[*Duration]():       KindDuration,
			reflect.TypeFor[*[]string]():       KindStringList,
			reflect.TypeFor[*[]netip.Prefix](): KindCIDRList,
		}[ptr.Type()]
		if k.Kind != wantKind {
			t.Errorf("%s: Kind %q, but its field is %s", k.Path, k.Kind, ptr.Type())
		}
		defType := map[Kind]reflect.Type{
			KindString: reflect.TypeFor[string](), KindBool: reflect.TypeFor[bool](), KindInt: reflect.TypeFor[int](),
			KindDuration: reflect.TypeFor[time.Duration](), KindStringList: reflect.TypeFor[[]string](),
			KindCIDRList: reflect.TypeFor[[]netip.Prefix](),
		}[k.Kind]
		if reflect.TypeOf(k.Default) != defType {
			t.Errorf("%s: Default is %T, want %s", k.Path, k.Default, defType)
		}
		if strings.TrimSpace(k.Help) == "" || k.Consumer == "" {
			t.Errorf("%s: missing Help or Consumer", k.Path)
		}
		if k.Enum != nil && !slices.Contains(k.Enum, k.Default.(string)) && k.Default != "" {
			t.Errorf("%s: default %v is not in its enum", k.Path, k.Default)
		}
	}
	if n := countLeaves(root.Type()); n != len(registry) {
		t.Errorf("Config has %d exported leaf fields, the registry %d keys", n, len(registry))
	}
}

// tomlPathOf finds the exported field of root (sections one level deep) at address p and returns its toml path.
func tomlPathOf(root reflect.Value, p uintptr) string {
	for i := range root.NumField() {
		f, fv := root.Type().Field(i), root.Field(i)
		switch {
		case !f.IsExported():
		case isSection(f.Type):
			for j := range fv.NumField() {
				if fv.Field(j).Addr().Pointer() == p {
					return f.Tag.Get("toml") + "." + f.Type.Field(j).Tag.Get("toml")
				}
			}
		case fv.Addr().Pointer() == p:
			return f.Tag.Get("toml")
		}
	}
	return ""
}

func isSection(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t != reflect.TypeFor[Duration]()
}

func countLeaves(t reflect.Type) int {
	n := 0
	for i := range t.NumField() {
		f := t.Field(i)
		switch {
		case !f.IsExported():
		case isSection(f.Type):
			n += f.Type.NumField()
		default:
			n++
		}
	}
	return n
}

// The defaults alone are a valid config without warnings (the Docker case: no file, no env).
func TestDefaultsAreValid(t *testing.T) {
	c := mustLoad(t, "", nil)
	if len(c.Problems()) != 0 {
		t.Errorf("defaults have problems:%s", problemList(c.Problems()))
	}
	for _, k := range Keys() {
		if c.IsSet(k.Path) {
			t.Errorf("%s is set without any source", k.Path)
		}
		if got := formatTOML(registry[keyByPath[k.Path]].get(c)); got != k.DefaultTOML() {
			t.Errorf("%s = %s, want the default %s", k.Path, got, k.DefaultTOML())
		}
	}
	if c.EffectiveTLSMode() != TLSIP {
		t.Errorf("effective tls mode %q, want ip", c.EffectiveTLSMode())
	}
}

// Keys returns copies: changing them doesn't change the registry.
func TestKeysReturnsCopies(t *testing.T) {
	ks := Keys()
	for i := range ks {
		if l, ok := ks[i].Default.([]string); ok && len(l) > 0 {
			l[0] = "changed"
		}
		ks[i].Enum = append(ks[i].Enum[:0:0], "x")
	}
	if Keys()[keyByPath["network.stun_servers"]].Default.([]string)[0] == "changed" {
		t.Error("Keys shares the default slices with the registry")
	}
	if k, _ := Lookup("tls.mode"); !slices.Equal(k.Enum, []string{"auto", "ip", "manual", "off"}) {
		t.Errorf("tls.mode enum = %v", k.Enum)
	}
	if _, ok := Lookup("tls.nope"); ok {
		t.Error("Lookup found an unknown key")
	}
}

func TestReservedNames(t *testing.T) {
	want := []string{
		"ISSHONI_CONFIG", "ISSHONI_VERSION", "ISSHONI_YES", "ISSHONI_NO_FIREWALL", "ISSHONI_DOWNLOAD_BASE",
		"ISSHONI_INSTALL_SOURCED", "ISSHONI_INSTALLER_VERSION", "ISSHONI_SNAPSHOT_VERSION", "ISSHONI_BIN",
		"ISSHONI_DEV_SERVER", "ISSHONI_LOADTEST_PASSWORD",
	}
	if got := ReservedEnvNames(); !slices.Equal(got, want) {
		t.Errorf("ReservedEnvNames = %v, want 04 §4.2's list %v", got, want)
	}
}
