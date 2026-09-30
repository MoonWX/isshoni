package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoadFile feeds arbitrary files to Load. It must never panic; every problem must name a source, a message and
// a fix; and an accepted file must survive config example: the example of its values loads back to the same values.
func FuzzLoadFile(f *testing.F) {
	for _, seed := range []string{
		"",
		"domain = \"watch.example.com\"\n[tls]\nmode = \"auto\"\n",
		"public_ip = \"8.8.8.8\"\n[network]\nstun_servers = []\ntrusted_proxies = [\"10.0.0.0/8\"]\n",
		"tls.mode = \"off\"\nlisten = {http = \"127.0.0.1:9000\"}\n",
		"[tsl]\nmode = 1\n[[listen]]\n",
		"shutdown_timeout = \"1h30m\"\n[limits]\nconns_per_ip = 99999999999\n",
		"a = \n",
		"[network]\nexclude_interfaces = [\"eth[\", 1]\n",
		"domain = {a = {b = [1, {c = 2}]}}\n",
	} {
		f.Add([]byte(seed))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "fuzz.toml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		c, ve := fuzzLoad(t, path)
		for _, p := range c.Problems() {
			if p.Source.Kind == "" || p.Message == "" || p.Fix == "" || (p.Severity != SeverityError && p.Severity != SeverityWarning) {
				t.Fatalf("incomplete problem %+v", p)
			}
			_ = p.String()
		}
		if ve != nil {
			return
		}
		if err := os.WriteFile(path, Example(c), 0o600); err != nil {
			t.Fatal(err)
		}
		back, ve := fuzzLoad(t, path)
		if ve != nil {
			t.Fatalf("the example of an accepted file has errors:\n%v\nfile:\n%s\nexample:\n%s", ve, data, Example(c))
		}
		for i := range registry {
			k := &registry[i]
			if got, want := formatTOML(k.get(back)), formatTOML(k.get(c)); got != want {
				t.Fatalf("%s = %s after the example, want %s\nfile:\n%s", k.Path, got, want, data)
			}
		}
	})
}

func fuzzLoad(t *testing.T, path string) (*Config, *ValidationError) {
	fs := flag.NewFlagSet("fuzz", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c, err := Load(fs, []string{"--config", path}, nil)
	var ve *ValidationError
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("Load: %v", err)
	}
	return c, ve
}
