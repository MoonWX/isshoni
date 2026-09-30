package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// writeConfig writes a config file for a test and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "isshoni.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// config init writes the example file with mode 0640, holding exactly the flags' values; the environment is
// ignored (04 §3.1, 06 §4.8).
func TestConfigInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "isshoni.toml")
	code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_DOMAIN=from-env.example.com", "ISSHONI_LOG_LEVEL=debug"},
		"config", "init", "--path", path, "--tls.mode", "ip", "--public-ip", "8.8.8.8")
	if code != exitOK || stdout != "" || stderr != "Wrote "+path+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
			t.Errorf("mode %v, want 0640", fi.Mode().Perm())
		}
	}
	file := string(data)
	for _, want := range []string{"\npublic_ip = \"8.8.8.8\"\n", "\n[tls]\n", "\nmode = \"ip\"\n", "\n# domain = \"watch.example.com\"\n", "\n# level = \"info\"\n"} {
		if !strings.Contains(file, want) {
			t.Errorf("file lacks %q", want)
		}
	}
	if strings.Contains(file, "from-env") || strings.Contains(file, "\nlevel = ") || strings.Contains(file, "\ndomain = ") {
		t.Errorf("file holds env values:\n%s", file)
	}
	if file != string(config.Example(mustLoadFlags(t, "--tls.mode", "ip", "--public-ip", "8.8.8.8"))) {
		t.Error("config init wrote something else than config example prints")
	}

	// The file is what config check reads back.
	code, stdout, stderr = runCLI(t, "config", "check", "--config", path)
	if code != exitOK || stdout != "config ok (file "+path+")\n" || stderr != "" {
		t.Errorf("config check: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// It never overwrites: exit 7, file unchanged.
	code, stdout, stderr = runCLI(t, "config", "init", "--path", path, "--domain", "watch.example.com")
	if code != exitRefused || stdout != "" || !strings.Contains(stderr, path+" already exists, and config init never overwrites a file") {
		t.Errorf("second init: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if again, _ := os.ReadFile(path); string(again) != file {
		t.Error("the existing file changed")
	}
}

func mustLoadFlags(t *testing.T, args ...string) *config.Config {
	t.Helper()
	fs := root().sub("config").sub("example")
	set, _ := fs.flagSet()
	c, err := config.LoadFlags(set, args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A symlink (even a dangling one) counts as an existing file.
func TestConfigInitSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "isshoni.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "config", "init", "--path", link); code != exitRefused {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("the symlink's target was created: %v", err)
	}
}

// Invalid values exit 78 with every problem and a fix, and write nothing.
func TestConfigInitInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "isshoni.toml")
	code, stdout, stderr := runCLI(t, "config", "init", "--path", path, "--tls.mode", "ip", "--public-ip", "192.168.1.20", "--log.level", "loud")
	if code != exitConfig || stdout != "" {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
	for _, want := range []string{
		`config error: tls.mode = "ip" (flag --tls.mode) needs a public address, but public_ip = "192.168.1.20" is private.`,
		"\n  fix: Let's Encrypt only issues IP certificates for public addresses.",
		`config error: log.level = "loud" (flag --log.level) is not one of debug, info, warn, error.`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file was written: %v", err)
	}
	// off mode with a loopback listen.http writes trusted_proxies (04 §4.4).
	code, _, stderr = runCLI(t, "config", "init", "--path", path, "--tls.mode", "off", "--public-url", "https://share.example.com")
	data, _ := os.ReadFile(path)
	if code != exitOK || !strings.Contains(string(data), "\ntrusted_proxies = [\"127.0.0.0/8\", \"::1/128\"]\n") {
		t.Errorf("off mode: exit %d, stderr %q, file:\n%s", code, stderr, data)
	}
}

// config example prints the same file on stdout; invalid values exit 78 with nothing on stdout; the environment is
// ignored.
func TestConfigExample(t *testing.T) {
	code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_PUBLIC_IP=192.168.1.20"}, "config", "example", "--domain", "watch.example.com")
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "\ndomain = \"watch.example.com\"\n") || !strings.Contains(stdout, "\n# public_ip = \"auto\"\n") {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, stderr, stdout)
	}
	code, stdout, stderr = runCLI(t, "config", "example", "--tls.mode", "auto")
	if code != exitConfig || stdout != "" || !strings.Contains(stderr, `config error: tls.mode = "auto" (flag --tls.mode) needs a domain.`) {
		t.Errorf("invalid: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Warnings go to stderr, the file still to stdout.
	code, stdout, stderr = runCLI(t, "config", "example", "--tls.acme-staging")
	if code != exitOK || !strings.HasPrefix(stderr, "config warning: tls.acme_staging = true (flag --tls.acme-staging)") || !strings.Contains(stdout, "\nacme_staging = true\n") {
		t.Errorf("warning: exit %d, stderr %q", code, stderr)
	}
}

// config check: exit 0 with the file named, 78 with every problem showing file:line and a fix.
func TestConfigCheck(t *testing.T) {
	code, stdout, stderr := runCLI(t, "config", "check")
	if code != exitOK || !strings.HasPrefix(stdout, "config ok (file ") || stderr != "" {
		t.Errorf("empty file: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	missing := filepath.Join(t.TempDir(), "none.toml")
	code, _, stderr = runCLIEnv(t, []string{"ISSHONI_CONFIG="}, "config", "check", "--config", missing)
	if code != exitConfig || !strings.Contains(stderr, "config error: flag --config: names the config file "+missing+", which does not exist.") {
		t.Errorf("missing file: exit %d, stderr %q", code, stderr)
	}

	path := writeConfig(t, "public_ip = \"8.8.8.8\"\n\n[tls]\nmode = \"ip\"\nmdoe = \"auto\"\n\n[metrics]\nenabled = true\nlisten = \"0.0.0.0:9469\"\n")
	code, stdout, stderr = runCLI(t, "config", "check", "--config", path)
	want := "config error: tls.mdoe (file " + path + ":5:1) is not a config key.\n" +
		"  fix: did you mean tls.mode? (mode = … under [tls])\n" +
		"config warning: metrics.listen = \"0.0.0.0:9469\" (file " + path + ":9) exposes unauthenticated metrics.\n" +
		"  fix: use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent.\n"
	if code != exitConfig || stdout != "" || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant:\n%s", code, stdout, stderr, want)
	}

	// Warnings alone exit 0; the env's warning names the variable.
	path = writeConfig(t, "[metrics]\nenabled = true\nlisten = \"0.0.0.0:9469\"\n")
	code, stdout, stderr = runCLIEnv(t, []string{"ISSHONI_TLS_MOD=ip"}, "config", "check", "--config", path)
	if code != exitOK || stdout != "config ok with 2 warnings (file "+path+")\n" || !strings.Contains(stderr, "config warning: ISSHONI_TLS_MOD (env) is not a config key; it is ignored.\n  fix: did you mean ISSHONI_TLS_MODE?\n") {
		t.Errorf("warnings: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// config print --json is one document; empty env values count as unset (06's Docker check).
func TestConfigPrintJSON(t *testing.T) {
	code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_DOMAIN=", "ISSHONI_PUBLIC_IP=", "ISSHONI_LOG_FORMAT=json"}, "config", "print", "--json")
	if code != exitOK || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	var doc config.PrintedConfig
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.TLSMode != config.TLSIP || !doc.FileRead {
		t.Errorf("tlsMode %q, fileRead %v", doc.TLSMode, doc.FileRead)
	}
	for _, k := range doc.Keys {
		switch k.Key {
		case "domain", "public_ip":
			if k.Source.Kind != config.SourceDefault {
				t.Errorf("%s: source %+v, want default", k.Key, k.Source)
			}
		case "log.format":
			if k.Value != "json" || k.Source.Kind != config.SourceEnv || k.Source.Name != "ISSHONI_LOG_FORMAT" {
				t.Errorf("log.format: %+v", k)
			}
		}
	}

	// An invalid config still prints, and exits 78 with the problems on stderr.
	code, stdout, stderr = runCLI(t, "config", "print", "--json", "--tls.mode", "auto")
	if code != exitConfig || !strings.Contains(stdout, `"severity":"error"`) || !strings.Contains(stderr, "needs a domain") {
		t.Errorf("invalid: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, _ = runCLI(t, "config", "print")
	if code != exitOK || !strings.Contains(stdout, "# registration.mode: not set (admin UI value applies; default \"invite\")\n") {
		t.Errorf("text: exit %d, stdout:\n%s", code, stdout)
	}
}

// serve step 1: a config with errors exits 78 before anything else.
func TestServeConfigErrors(t *testing.T) {
	code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_TLS_MODE=auto"}, "serve")
	if code != exitConfig || stdout != "" || !strings.HasPrefix(stderr, `config error: tls.mode = "auto" (env ISSHONI_TLS_MODE) needs a domain.`) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = runCLI(t, "serve", "--config", filepath.Join(t.TempDir(), "missing.toml"))
	if code != exitConfig || !strings.Contains(stderr, "which does not exist") {
		t.Errorf("missing explicit file: exit %d, stderr %q", code, stderr)
	}
	// A bad value in a flag is a config error too, not a usage error.
	if code, _, _ = runCLI(t, "serve", "--shutdown-timeout", "soon"); code != exitConfig {
		t.Errorf("bad flag value: exit %d, want 78", code)
	}
	// Commands that only talk to the server don't stop at config errors (the offline admin commands are the way out
	// of a config the server refuses).
	if code, _, stderr = runCLIEnv(t, []string{"ISSHONI_TLS_MODE=auto"}, "admin", "status"); code != exitRuntime || !strings.Contains(stderr, "not implemented") {
		t.Errorf("admin status with a bad config: exit %d, stderr %q", code, stderr)
	}
}

// serve --help lists every config flag of the registry (04 §4.2); config init lists them without --config; the
// client commands point to serve's help.
func TestConfigFlagsHelp(t *testing.T) {
	_, serve, _ := runCLI(t, "help", "serve")
	_, initHelp, _ := runCLI(t, "help", "config", "init")
	for _, k := range config.Keys() {
		flag := "\n  --" + k.FlagName() + " "
		if k.Hidden {
			if strings.Contains(serve, flag) {
				t.Errorf("serve help lists the hidden --%s", k.FlagName())
			}
			continue
		}
		if !strings.Contains(serve, flag) || !strings.Contains(serve, k.EnvName()) {
			t.Errorf("serve help lacks --%s or %s", k.FlagName(), k.EnvName())
		}
		if !strings.Contains(initHelp, flag) {
			t.Errorf("config init help lacks --%s", k.FlagName())
		}
	}
	if !strings.Contains(serve, "\n  --config PATH ") || strings.Contains(initHelp, "--config") {
		t.Error("--config belongs to serve's help only")
	}
	_, status, _ := runCLI(t, "help", "admin", "status")
	if !strings.Contains(status, "See 'isshoni help serve'.") || strings.Contains(status, "--tls.mode VALUE") {
		t.Errorf("admin status help:\n%s", status)
	}
}
