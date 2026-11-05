package config

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
	want = append(want, desktopEnvNames[2:]...) // M2 06 §7.3 after M1's; its first two names are M1's already
	for _, d := range devEnvNames {
		want = append(want, d.name) // then WF7's
	}
	got := ReservedEnvNames()
	if !slices.Equal(got, want) {
		t.Errorf("ReservedEnvNames = %v, want 04 §4.2's list %v", got, want)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("%s is listed twice", n)
		}
		seen[n] = true
		if !strings.HasPrefix(n, "ISSHONI_") || strings.ToUpper(n) != n {
			t.Errorf("%s is not an upper-case ISSHONI_* name", n)
		}
	}
}

// desktopEnvNames is M2 06 §7.3's list, in its order.
var desktopEnvNames = []string{
	"ISSHONI_VERSION", "ISSHONI_DOWNLOAD_BASE",
	"ISSHONI_NO_UPDATE_CHECKS", "ISSHONI_NO_LAUNCH", "ISSHONI_ALLOW_DOWNGRADE",
	"ISSHONI_UPDATE_TEST_BASE", "ISSHONI_UPDATE_TEST_SIGNERS",
	"ISSHONI_TEST_ENGINE_DLL", "ISSHONI_COMPAT_SERVER_URL", "ISSHONI_COMPAT_ADMIN_FILE",
	"ISSHONI_DESKTOP_MIN_VERSION", "ISSHONI_FFMPEG_VERSION",
}

// The server never warns about M2 06 §7.3's names (README WA66): a desktop or release task that sets them in the
// shell the server runs in leaves the server's log alone.
func TestDesktopEnvNamesNotWarned(t *testing.T) {
	env := make([]string, 0, len(desktopEnvNames))
	for _, n := range desktopEnvNames {
		if !slices.Contains(ReservedEnvNames(), n) {
			t.Errorf("%s is not a reserved name", n)
		}
		env = append(env, n+"=1")
	}
	if ws := mustLoad(t, "", env).Warnings(); len(ws) != 0 {
		t.Errorf("warnings:%s\nwant none", problemList(ws))
	}
}

// devEnvNames are the ISSHONI_* names that M2's desktop dev and e2e builds, tasks and tests read and that 06 §7.3
// doesn't list (WF7), in ReservedEnvNames' order, each with where the repository reads it. They were found by
// grepping every tracked file outside docs/ for ISSHONI_ names; header guards, window globals (__ISSHONI_…__) and the
// deliberately unknown names of tests are not env names. The M0 spikes' signing variables are not listed.
// TestRepoEnvNamesReserved repeats that grep on every run, so a name that a later change starts to read lands here.
var devEnvNames = []struct{ name, readBy string }{
	{"ISSHONI_DESKTOP_DEV_URL", "cmd/isshoni-desktop/flavor_dev.go, set by task desktop:dev (M2 03 §5.3)"},
	{"ISSHONI_FAKE_AUDIO", "cmd/isshoni-desktop/engine_fake.go, internal/desktop/e2ehost, web/e2e (M2 05 §13.3)"},
	{"ISSHONI_E2E_CDP_PORT", "web/e2e/cdp, and cmd/isshoni-desktop/flavor_e2e.go from WA72 on (M2 03 §22.4)"},
	{"ISSHONI_E2E_DATA_DIR", "web/e2e/cdp, and cmd/isshoni-desktop/flavor_e2e.go from WA72 on (M2 03 §22.4)"},
	{"ISSHONI_DESKTOP_EXE", "web/e2e/cdp: the e2e build of isshoni.exe the CDP spec starts"},
	{"ISSHONI_DESKTOP_PROD_EXE", "web/e2e/cdp: the production build W19 runs outside CI (M2 03 §22.4)"},
	{"ISSHONI_CDP_ENDPOINT", "web/e2e/cdp: a running app the CDP spec attaches to"},
	{"ISSHONI_NATIVE_PRESETS", "Taskfile.yml native:build"},
	{"ISSHONI_NATIVE_TEST", "Taskfile.yml native:build"},
	{"ISSHONI_NATIVE_BUILD_PS1", "Taskfile.yml native:build"},
	{"ISSHONI_FFMPEG_BUILD_PS1", "Taskfile.yml ffmpeg:build"},
	{"ISSHONI_CHECKLIST_OUT", "cmd/isshoni-desktop/checklist_test.go (M2 03 §3.2)"},
	{"ISSHONI_CHECKLIST_W19_PORT", "cmd/isshoni-desktop/checklist_test.go (M2 03 §3.2)"},
	{"ISSHONI_CHECKLIST_W19_PIPE", "cmd/isshoni-desktop/checklist_test.go (M2 03 §3.2)"},
	{"ISSHONI_DESKTOP_TEST_CHILD", "cmd/isshoni-desktop/main_windows_test.go"},
	{"ISSHONI_DESKTOP_TEST_ID", "cmd/isshoni-desktop/main_windows_test.go"},
	{"ISSHONI_DESKTOP_TEST_RUNNING", "cmd/isshoni-desktop/main_windows_test.go"},
	{"ISSHONI_DESKTOP_TEST_OUT", "cmd/isshoni-desktop/main_windows_test.go"},
	{"ISSHONI_DESKTOP_TEST_ROOT", "cmd/isshoni-desktop/main_windows_test.go"},
	{"ISSHONI_WINOS_TEST_CHILD", "internal/desktop/winos/winos_windows_test.go"},
	{"ISSHONI_WINOS_TEST_ID", "internal/desktop/winos/winos_windows_test.go"},
	{"ISSHONI_WINOS_TEST_OUT", "internal/desktop/winos/winos_windows_test.go"},
	{"ISSHONI_WINOS_TEST_ROOT", "internal/desktop/winos/winos_windows_test.go"},
	{"ISSHONI_APPLOG_CRASH_CHILD", "internal/desktop/applog/applog_test.go"},
	{"ISSHONI_MEDIATEST_MUTANT", "internal/media/mediatest/mutant_test.go"},
	{"ISSHONI_ENGINETEST_TEST_CHILD", "cmd/isshoni-enginetest/main_windows_test.go"},
	{"ISSHONI_REQUIRE_AUTOSTART_E2E", "cmd/isshoni-desktop/autostart_windows_test.go, set by ci.yml windows-desktop"},
	{"ISSHONI_TEST_FFMPEG_DIR", "native/windows/tests/ffmpeg_load_test.cpp, mf_sw_test.cpp, set by ci.yml windows-native"},
}

// The server never warns about the names M2's desktop dev and e2e builds, tasks and tests read (WF7, after README
// WA66's note): running task desktop:dev, an e2e run or a native build in the shell the server runs in leaves the
// server's log alone. The names are matched exactly, so an unlisted name of the same families still warns.
func TestDevEnvNamesNotWarned(t *testing.T) {
	env := make([]string, 0, len(devEnvNames))
	for _, d := range devEnvNames {
		if !slices.Contains(ReservedEnvNames(), d.name) {
			t.Errorf("%s (read by %s) is not a reserved name", d.name, d.readBy)
		}
		env = append(env, d.name+"=1")
	}
	if ws := mustLoad(t, "", env).Warnings(); len(ws) != 0 {
		t.Errorf("warnings:%s\nwant none", problemList(ws))
	}

	for _, n := range devUnknownNames {
		env = append(env, n+"=1")
	}
	ws := mustLoad(t, "", env).Warnings()
	var names []string
	for _, w := range ws {
		if w.Severity != SeverityWarning || w.Source.Kind != SourceEnv || w.Message != "is not a config key; it is ignored" {
			t.Errorf("warning %+v, want an unknown env warning", w)
		}
		names = append(names, w.Source.Name)
	}
	if !slices.Equal(names, devUnknownNames) {
		t.Errorf("warned about %v, want %v", names, devUnknownNames)
	}
}

// devUnknownNames are unlisted names of devEnvNames' families, which TestDevEnvNamesNotWarned expects a warning for.
var devUnknownNames = []string{
	"ISSHONI_E2E_NOT_A_NAME", "ISSHONI_NATIVE_NOT_A_NAME", "ISSHONI_FAKE_AUDIOS", "ISSHONI_DESKTOP_DEV",
}

// repoNonEnvNames are the ISSHONI_* names in the repository that are not env names or are unknown on purpose, with
// where they are; TestRepoEnvNamesReserved accepts them, and devUnknownNames.
var repoNonEnvNames = map[string]string{
	"ISSHONI_MEDIA_H":                             "native/include/isshoni_media.h's include guard",
	"ISSHONI_COMMON_NSH":                          "deploy/windows/nsis/common.nsh's include guard (an NSIS !define)",
	"ISSHONI_UNINSTALL_NSH":                       "deploy/windows/nsis/uninstall.nsh's include guard (an NSIS !define)",
	"ISSHONI_UNINSTALLER_ONLY":                    "deploy/windows/nsis/uninst-gen.nsi's NSIS !define for common.nsh",
	"ISSHONI_UPDATE_TEST":                         "internal/update's update_test: a string release builds lack",
	"ISSHONI_TEST_PADDING":                        "tools/desktoppkg's ffmpeg_test: the filler of a long header",
	"ISSHONI_TEST_PADDING_VALUE_OF_A_LONG_HEADER": "tools/desktoppkg's ffmpeg_test: the filler of a long header",
	"ISSHONI_TLS_MOD":                             "a misspelled key that config tests expect a warning for",
	"ISSHONI_TOTALLY_UNKNOWN":                     "load_test's unknown name",
	"ISSHONI_EMPTY_UNKNOWN":                       "load_test's unknown name",
	"ISSHONI_SOMETHING_ELSE":                      "deploy/test/install_unit.sh's unknown name",
}

// envNameRE matches an ISSHONI_* name as a whole word. A window global (__ISSHONI_…__) has a word character on each
// side, and a family written as ISSHONI_E2E_* or a prefix as "ISSHONI_" ends in an underscore, so neither matches.
var envNameRE = regexp.MustCompile(`\bISSHONI_[A-Z0-9_]*[A-Z0-9]\b`)

// Every ISSHONI_* name in a tracked file outside docs/ and spikes/ is a key's env name, a reserved name, an env-only
// switch or one of repoNonEnvNames (WF7's review). The reserved list matches names exactly (04 §4.2), so it holds
// only while each new variable that a task, test or build reads is added to it by hand; this test fails on one that
// isn't, where otherwise the only sign would be the server's unknown-env warning in the shell that runs both. It
// scans git's list of tracked files, so build output, node_modules, agent worktrees and other ignored files never
// count; outside a git checkout of the repository (a module-cache build, a source archive) it is skipped.
func TestRepoEnvNamesReserved(t *testing.T) {
	root := repoRoot(t)
	known := map[string]bool{}
	for _, k := range Keys() {
		known[k.EnvName()] = true
	}
	for _, n := range slices.Concat(ReservedEnvNames(), envOnly) {
		known[n] = true
	}
	accepted := map[string]bool{}
	for n := range repoNonEnvNames {
		accepted[n] = true
	}
	for _, n := range devUnknownNames {
		accepted[n] = true
	}
	for n := range accepted {
		if known[n] {
			t.Errorf("%s is a key's env name, a reserved name or an env-only switch, so it is not a non-env name", n)
		}
	}

	found := map[string][]string{} // name → the files it is in
	files := trackedFiles(t, root)
	for _, f := range files {
		if strings.HasPrefix(f, "docs/") || strings.HasPrefix(f, "spikes/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the working tree
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			continue // binary, by git's own test
		}
		for _, m := range envNameRE.FindAll(b, -1) {
			if n := string(m); !slices.Contains(found[n], f) {
				found[n] = append(found[n], f)
			}
		}
	}
	// keys.go holds every reserved and env-only name, so a scan that misses one of them read nothing.
	for _, n := range slices.Concat(ReservedEnvNames(), envOnly) {
		if found[n] == nil {
			t.Fatalf("the scan of %d tracked files found no %s, which keys.go holds: it is broken", len(files), n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(found)) {
		if !known[n] && !accepted[n] {
			t.Errorf("%s (in %s) is not a config key's env name or a reserved name, so the server warns about it: "+
				"add it to reservedEnv in internal/server/config/keys.go and to devEnvNames with what reads it, or to "+
				"repoNonEnvNames if it is not an env name; that edit belongs to the change that adds the name, even "+
				"when internal/server/config is outside its Touches", n, strings.Join(found[n], ", "))
		}
	}
}

// repoRoot is the root of the repository this package is in: the closest directory above it that holds go.mod and
// Taskfile.yml. Without one (the package was built from the module cache) the test is skipped.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, errMod := os.Stat(filepath.Join(dir, "go.mod"))
		_, errTask := os.Stat(filepath.Join(dir, "Taskfile.yml"))
		if errMod == nil && errTask == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("not in a checkout of the repository: no directory above holds go.mod and Taskfile.yml")
		}
		dir = parent
	}
}

// trackedFiles lists the files git tracks under root, slash-separated and relative to it. A root that is not a git
// work tree (a source archive), or a machine without git, skips the test; a git that fails in a work tree fails it.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skipf("%s is not a git work tree: %v", filepath.Base(root), err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("no git: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-z")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v\n%s", err, stderr.Bytes())
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}
