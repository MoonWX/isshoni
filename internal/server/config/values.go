package config

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// valueError is a value that doesn't fit its key's kind. msg follows the key and its source in a Problem.
type valueError struct {
	msg string
	fix string
}

func (e *valueError) Error() string { return e.msg }

// kindHint is the fix for a value of the wrong type, per kind and source syntax.
func kindHint(k Kind, env bool) string {
	switch k {
	case KindString:
		return `write it in double quotes, e.g. key = "value"`
	case KindBool:
		if env {
			return "use true, false, 1 or 0"
		}
		return "use true or false, without quotes"
	case KindInt:
		if env {
			return "use a whole number like 20"
		}
		return "use a whole number like 20, without quotes"
	case KindDuration:
		if env {
			return "use Go syntax like 10s, 5m or 168h"
		}
		return `use Go syntax in quotes, like "10s", "5m" or "168h"`
	case KindStringList:
		if env {
			return "separate the values with commas: a:3478,b:3478"
		}
		return `use a list of strings: ["a", "b"]`
	case KindCIDRList:
		if env {
			return "separate the CIDRs with commas: 10.0.0.0/8,172.16.0.0/12"
		}
		return `use a list of CIDRs: ["10.0.0.0/8", "172.16.0.0/12"]`
	default:
		return ""
	}
}

// parseText parses an env or flag value. Lists are comma-separated; empty elements are dropped, so "" is an empty
// list.
func parseText(k Kind, s string) (any, error) {
	switch k {
	case KindString:
		return s, nil
	case KindBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
		return nil, &valueError{"is not a boolean", kindHint(k, true)}
	case KindInt:
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil, &valueError{"is not a whole number", kindHint(k, true)}
		}
		return n, nil
	case KindDuration:
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return nil, &valueError{"is not a duration", kindHint(k, true)}
		}
		return d, nil
	case KindStringList:
		return splitList(s), nil
	case KindCIDRList:
		return parseCIDRs(splitList(s))
	default:
		return nil, fmt.Errorf("config: unknown kind %q", k)
	}
}

func splitList(s string) []string {
	out := []string{}
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func parseCIDRs(list []string) (any, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, e := range list {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			fix := "write each entry as an address and a prefix length, e.g. 10.0.0.0/8"
			if a, err := netip.ParseAddr(e); err == nil {
				fix = fmt.Sprintf("write a single address as %s/%d", a, a.BitLen())
			}
			return nil, &valueError{fmt.Sprintf("has %s, which is not a CIDR", tomlString(e)), fix}
		}
		out = append(out, p)
	}
	return out, nil
}

// fromTOML converts a value decoded from the TOML file (string, int64, float64, bool, []any, map[string]any or a
// date/time type) to the Go type of kind k.
func fromTOML(k Kind, v any) (any, error) {
	wrong := func() error { return &valueError{"must be " + kindNoun(k), kindHint(k, false)} }
	switch k {
	case KindString:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, wrong()
	case KindBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, wrong()
	case KindInt:
		if n, ok := v.(int64); ok {
			if n < math.MinInt32 || n > math.MaxInt32 {
				return nil, &valueError{"is out of range", "use a smaller number"}
			}
			return int(n), nil
		}
		return nil, wrong()
	case KindDuration:
		s, ok := v.(string)
		if !ok {
			return nil, wrong()
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, &valueError{"is not a duration", kindHint(k, false)}
		}
		return d, nil
	case KindStringList, KindCIDRList:
		arr, ok := v.([]any)
		if !ok {
			return nil, wrong()
		}
		list := make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, wrong()
			}
			list = append(list, s)
		}
		if k == KindCIDRList {
			return parseCIDRs(list)
		}
		return list, nil
	default:
		return nil, fmt.Errorf("config: unknown kind %q", k)
	}
}

func kindNoun(k Kind) string {
	switch k {
	case KindString:
		return "a string"
	case KindBool:
		return "true or false"
	case KindInt:
		return "a whole number"
	case KindDuration:
		return "a duration string"
	case KindStringList:
		return "a list of strings"
	case KindCIDRList:
		return "a list of CIDR strings"
	default:
		return string(k)
	}
}

// formatTOML renders a value of a key's Go type as a TOML value.
func formatTOML(v any) string {
	switch v := v.(type) {
	case string:
		return tomlString(v)
	case TLSMode:
		return tomlString(string(v))
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case time.Duration:
		return tomlString(formatDuration(v))
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = tomlString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []netip.Prefix:
		parts := make([]string, len(v))
		for i, p := range v {
			parts[i] = tomlString(p.String())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// formatText renders a value the way env and flags take it: lists comma-separated, strings bare.
func formatText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case time.Duration:
		return formatDuration(v)
	case []string:
		return strings.Join(v, ",")
	case []netip.Prefix:
		parts := make([]string, len(v))
		for i, p := range v {
			parts[i] = p.String()
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsonValue renders a value for config print --json: durations and prefixes as strings, lists never null.
func jsonValue(v any) any {
	switch v := v.(type) {
	case time.Duration:
		return formatDuration(v)
	case []string:
		if v == nil {
			return []string{}
		}
		return v
	case []netip.Prefix:
		out := make([]string, len(v))
		for i, p := range v {
			out[i] = p.String()
		}
		return out
	default:
		return v
	}
}

// formatDuration is time.Duration.String without the trailing zero units: 10s, 5m, 168h, 1h30m.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// tomlString quotes s as a TOML basic string: backslash, quote and control characters escaped, everything else
// literal (TOML files are UTF-8). Invalid UTF-8 bytes become U+FFFD.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || (unicode.IsControl(r) && r < 0xa0) {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
