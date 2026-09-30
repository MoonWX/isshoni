package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Never read the machine's /etc/isshoni/isshoni.toml.
	dir, err := os.MkdirTemp("", "isshoni-config-test")
	if err != nil {
		panic(err)
	}
	defaultPath = filepath.Join(dir, "missing", "isshoni.toml")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testLoad loads a config with file as the TOML file ("" for none), env in os.Environ form and args as flags.
// It fails the test on a usage error; a *ValidationError is returned.
func testLoad(t *testing.T, file string, env []string, args ...string) (*Config, *ValidationError) {
	t.Helper()
	if file != "" {
		path := writeFile(t, file)
		env = append([]string{EnvConfig + "=" + path}, env...)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c, err := Load(fs, args, env)
	var ve *ValidationError
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("Load: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned a nil config")
	}
	return c, ve
}

// mustLoad is testLoad for a config that must have no errors.
func mustLoad(t *testing.T, file string, env []string, args ...string) *Config {
	t.Helper()
	c, ve := testLoad(t, file, env, args...)
	if ve != nil {
		t.Fatalf("unexpected config errors:\n%v", ve)
	}
	return c
}

// loadFlags is LoadFlags for tests.
func loadFlags(t *testing.T, args ...string) (*Config, *ValidationError) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c, err := LoadFlags(fs, args)
	var ve *ValidationError
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("LoadFlags: %v", err)
	}
	return c, ve
}

// writeFile writes content to a fresh isshoni.toml and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "isshoni.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// findProblem returns the first problem for key (and, when not "", whose message contains msg).
func findProblem(ps []Problem, key, msg string) (Problem, bool) {
	for _, p := range ps {
		if p.Key == key && strings.Contains(p.Message, msg) {
			return p, true
		}
	}
	return Problem{}, false
}

func problemList(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("\n" + p.String())
	}
	return b.String()
}
