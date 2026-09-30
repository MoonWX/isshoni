package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrintText(t *testing.T) {
	c := mustLoad(t, "[registration]\nmode = \"closed\"\n", []string{"ISSHONI_PUBLIC_IP=8.8.8.8"}, "--log.level=debug")
	var b bytes.Buffer
	if err := c.PrintText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, re := range []string{
		"# config file: ",
		"# effective tls.mode: ip\n",
		`public_ip = "8.8.8.8"`, "# env ISSHONI_PUBLIC_IP\n",
		`log.level = "debug"`, "# flag --log.level\n",
		`registration.mode = "closed"`, "isshoni.toml:2\n",
		"# default (derived: ip, because domain is empty)\n",
		"# limits.max_bitrate_kbps: not set (admin UI value applies; default 0)\n",
		"# updates.release_check: not set (admin UI value applies; default true)\n",
	} {
		if !strings.Contains(out, re) {
			t.Errorf("print lacks %q:\n%s", re, out)
		}
	}
}

// An explicitly empty tls.mode is still derived, and print says so.
func TestPrintTextEmptyTLSMode(t *testing.T) {
	for _, domain := range []string{"", "watch.example.com"} {
		c := mustLoad(t, "[tls]\nmode = \"\"\n", []string{"ISSHONI_DOMAIN=" + domain, "ISSHONI_PUBLIC_IP=8.8.8.8"})
		var b bytes.Buffer
		if err := c.PrintText(&b); err != nil {
			t.Fatal(err)
		}
		want := "isshoni.toml:2 (derived: ip, because domain is empty)\n"
		if domain != "" {
			want = "isshoni.toml:2 (derived: auto, because domain is set)\n"
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("domain %q: print lacks %q:\n%s", domain, want, b.String())
		}
	}
	// A mode that is set has no note.
	c := mustLoad(t, "", nil, "--tls.mode=off")
	var b bytes.Buffer
	if err := c.PrintText(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "derived:") {
		t.Errorf("print of a set mode has a derived note:\n%s", b.String())
	}
}

// The text form is TOML: the effective values load back.
func TestPrintTextIsTOML(t *testing.T) {
	c := mustLoad(t, "", nil, "--tls.mode=off", "--log.format=json")
	var b bytes.Buffer
	if err := c.PrintText(&b); err != nil {
		t.Fatal(err)
	}
	back := mustLoad(t, b.String(), nil)
	if back.TLS.Mode != TLSOff || back.Log.Format != "json" || back.Listen.HTTP != "127.0.0.1:8080" {
		t.Errorf("printed config loads back as %q %q %q", back.TLS.Mode, back.Log.Format, back.Listen.HTTP)
	}
}

// The JSON form: 06's Docker check reads domain and public_ip with source default for empty env, and the
// derived TLS mode.
func TestPrintJSON(t *testing.T) {
	c := mustLoad(t, "", []string{"ISSHONI_DOMAIN=", "ISSHONI_PUBLIC_IP=", "ISSHONI_LOG_LEVEL=warn", "ISSHONI_TLS_MOD=x"})
	var b bytes.Buffer
	if err := c.PrintJSON(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Errorf("not one line: %q", b.String())
	}
	var doc struct {
		File     string `json:"file"`
		FileRead bool   `json:"fileRead"`
		TLSMode  string `json:"tlsMode"`
		Keys     []struct {
			Key    string          `json:"key"`
			Value  json.RawMessage `json:"value"`
			Source struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"source"`
			Policy bool   `json:"policy"`
			Env    string `json:"env"`
			Flag   string `json:"flag"`
		} `json:"keys"`
		Problems []Problem `json:"problems"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.TLSMode != "ip" || doc.File == "" || doc.FileRead || len(doc.Keys) != len(registry) {
		t.Errorf("doc: %+v", doc)
	}
	byKey := map[string]int{}
	for i, k := range doc.Keys {
		byKey[k.Key] = i
	}
	for key, want := range map[string]struct{ value, kind, name string }{
		"domain":                  {`""`, "default", ""},
		"public_ip":               {`"auto"`, "default", ""},
		"log.level":               {`"warn"`, "env", "ISSHONI_LOG_LEVEL"},
		"shutdown_timeout":        {`"10s"`, "default", ""},
		"network.trusted_proxies": {`[]`, "default", ""},
		"network.stun_servers":    {`["stun.cloudflare.com:3478","stun.l.google.com:19302"]`, "default", ""},
		"registration.mode":       {`null`, "default", ""},
		"limits.conns_per_ip":     {`256`, "default", ""},
	} {
		k := doc.Keys[byKey[key]]
		if string(k.Value) != want.value || k.Source.Kind != want.kind || k.Source.Name != want.name {
			t.Errorf("%s: %s %+v, want %s %s %s", key, k.Value, k.Source, want.value, want.kind, want.name)
		}
	}
	if k := doc.Keys[byKey["registration.mode"]]; !k.Policy || k.Env != "ISSHONI_REGISTRATION_MODE" || k.Flag != "--registration.mode" {
		t.Errorf("registration.mode: %+v", k)
	}
	if len(doc.Problems) != 1 || doc.Problems[0].Source.Name != "ISSHONI_TLS_MOD" {
		t.Errorf("problems: %+v", doc.Problems)
	}

	// Problems is never null.
	b.Reset()
	c = mustLoad(t, "", nil)
	if err := c.PrintJSON(&b); err != nil || !strings.Contains(b.String(), `"problems":[]`) {
		t.Errorf("%s %v", b.String(), err)
	}
}
