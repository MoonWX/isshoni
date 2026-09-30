package config

import (
	"bytes"
	"strings"

	"github.com/MoonWX/isshoni/internal/version"
)

// Example returns the commented isshoni.toml of `isshoni config example` and `isshoni config init` (§3.1, §4.3):
// every key of the registry in order, with its help. Keys that c sets (IsSet) are written as values; every other key
// is a commented-out line with its default or an example. In off mode with a loopback listen.http and no
// network.trusted_proxies, the derived loopback value is written too (§4.4), so the operator sees it. Hidden keys
// appear only when set. Loading the output gives back c's values.
func Example(c *Config) []byte {
	var b bytes.Buffer
	b.WriteString("# isshoni server configuration. Every key also has an ISSHONI_* env variable and a --flag.\n")
	b.WriteString("# Reference: " + version.DocsURL + "reference/config\n")
	b.WriteString("#\n")
	b.WriteString("# A line that starts with # shows the default (or an example) and changes nothing. Remove the # to set\n")
	b.WriteString("# a value. Changes need a restart (sudo systemctl restart isshoni).\n")

	derivedProxies := c.derivedTrustedProxies()
	section := ""
	for i := range registry {
		k := &registry[i]
		set := c.IsSet(k.Path)
		writeDerived := k.Path == "network.trusted_proxies" && derivedProxies
		if k.Hidden && !set {
			continue
		}
		if s := k.Section(); s != section {
			section = s
			b.WriteString("\n[" + s + "]\n")
		}
		b.WriteString("\n")
		writeComment(&b, k.Help)
		if note := defaultComment(k); note != "" {
			writeComment(&b, note)
		}
		if k.Policy {
			writeComment(&b, "Also editable in the admin page; a value set here locks it there.")
		}
		switch {
		case set:
			b.WriteString(k.Name() + " = " + formatTOML(k.get(c)) + "\n")
		case writeDerived:
			writeComment(&b, `Written by "isshoni config init" for tls.mode = "off" with a loopback listen.http (a proxy on this host).`)
			b.WriteString(k.Name() + " = " + formatTOML(k.get(c)) + "\n")
		default:
			b.WriteString("# " + k.Name() + " = " + exampleValue(k) + "\n")
		}
	}
	return b.Bytes()
}

// exampleValue is the value a commented-out line shows: the key's example, else its default.
func exampleValue(k *Key) string {
	if k.Example != "" {
		return k.Example
	}
	return formatTOML(k.Default)
}

// defaultComment names the default when the commented-out line shows an example instead, or when the default is
// derived.
func defaultComment(k *Key) string {
	empty := k.Default == ""
	switch {
	case empty && k.DefaultNote != "":
		return "Default: " + k.DefaultNote + "."
	case empty && k.Example != "":
		return "Empty by default."
	case k.DefaultNote != "":
		return "Default: " + formatTOML(k.Default) + "; " + k.DefaultNote + "."
	case k.Example != "":
		return "Default: " + formatTOML(k.Default) + "."
	default:
		return ""
	}
}

// writeComment writes text as "# " lines of at most 100 columns.
func writeComment(b *bytes.Buffer, text string) {
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case 2+len(line)+1+len(word) > 100:
			b.WriteString("# " + line + "\n")
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		b.WriteString("# " + line + "\n")
	}
}
