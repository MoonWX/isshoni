package config

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// PrintText writes the effective config of `isshoni config print` (§3.1): one line per key in registry order,
// "key = value  # source". The output is valid TOML (dotted keys). A policy key that is not set shows as a comment,
// "not set (admin UI value applies)" (§4.6), because its effective value lives in the database.
func (c *Config) PrintText(w io.Writer) error {
	var b strings.Builder
	if c.file != "" {
		if c.fileRead {
			b.WriteString("# config file: " + c.file + "\n")
		} else {
			b.WriteString("# config file: " + c.file + " (not found: defaults, environment and flags only)\n")
		}
	}
	fmt.Fprintf(&b, "# effective tls.mode: %s\n", c.EffectiveTLSMode())
	width := 0 // the source comments line up, except after the few long lines
	for _, k := range registry {
		if n := len(k.Path) + 3 + len(formatTOML(k.get(c))); n <= 64 {
			width = max(width, n)
		}
	}
	for i := range registry {
		k := &registry[i]
		value := formatTOML(k.get(c))
		if k.Policy && !c.IsSet(k.Path) {
			fmt.Fprintf(&b, "# %s: not set (admin UI value applies; default %s)\n", k.Path, value)
			continue
		}
		line := k.Path + " = " + value
		fmt.Fprintf(&b, "%-*s  # %s%s\n", width, line, c.Source(k.Path), c.derivedNote(k))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// derivedNote explains a default that depends on other keys.
func (c *Config) derivedNote(k *Key) string {
	if c.IsSet(k.Path) {
		return ""
	}
	switch {
	case k.Path == "tls.mode":
		if c.Domain != "" {
			return " (derived: auto, because domain is set)"
		}
		return " (derived: ip, because domain is empty)"
	case k.Path == "listen.http" && c.EffectiveTLSMode() == TLSOff:
		return ` (tls.mode = "off")`
	case k.Path == "network.trusted_proxies" && c.derivedTrustedProxies():
		return ` (tls.mode = "off" with a loopback listen.http)`
	}
	return ""
}

// PrintedConfig is the JSON document of `isshoni config print --json`.
type PrintedConfig struct {
	File     string       `json:"file"`     // the config file looked at
	FileRead bool         `json:"fileRead"` // whether it existed and was read
	TLSMode  TLSMode      `json:"tlsMode"`  // the effective TLS mode (§4.4)
	Keys     []PrintedKey `json:"keys"`     // every key in registry order
	Problems []Problem    `json:"problems"` // errors first, then warnings; never null
}

// PrintedKey is one key of PrintedConfig.
type PrintedKey struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`  // null for a policy key that is not set (the admin UI value applies)
	Source Source `json:"source"` // kind "default", "file" (with file and line), "env" or "flag" (with name)
	Policy bool   `json:"policy,omitempty"`
	Env    string `json:"env"`  // the key's environment variable
	Flag   string `json:"flag"` // the key's flag, with dashes
}

// Printed returns the effective config as `isshoni config print --json` prints it.
func (c *Config) Printed() PrintedConfig {
	p := PrintedConfig{File: c.file, FileRead: c.fileRead, TLSMode: c.EffectiveTLSMode(), Problems: c.Problems()}
	if p.Problems == nil {
		p.Problems = []Problem{}
	}
	for i := range registry {
		k := &registry[i]
		pk := PrintedKey{
			Key: k.Path, Value: jsonValue(k.get(c)), Source: c.Source(k.Path), Policy: k.Policy,
			Env: k.EnvName(), Flag: "--" + k.FlagName(),
		}
		if k.Policy && !c.IsSet(k.Path) {
			pk.Value = nil
		}
		p.Keys = append(p.Keys, pk)
	}
	return p
}

// PrintJSON writes Printed as one JSON document on one line.
func (c *Config) PrintJSON(w io.Writer) error {
	return json.NewEncoder(w).Encode(c.Printed())
}
