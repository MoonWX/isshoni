package config

import (
	"strings"
)

// Problem severities.
const (
	SeverityError   = "error"   // serve exits 78
	SeverityWarning = "warning" // printed or logged; serving goes on
)

// Problem is one finding of loading or validating the config (§4.5): the key, where its value came from, what is
// wrong and how to fix it.
type Problem struct {
	Key      string `json:"key,omitempty"`   // dotted key path; "" for a file-level problem or an unknown env variable
	Value    string `json:"value,omitempty"` // the offending value in TOML syntax, e.g. "\"ip\""; "" when none applies
	Source   Source `json:"source"`
	Severity string `json:"severity"` // SeverityError or SeverityWarning
	Message  string `json:"message"`  // follows the key, value and source: "needs a public address, …"
	Fix      string `json:"fix"`
}

// String renders the problem as in §4.5:
//
//	config error: tls.mode = "ip" (file /etc/isshoni/isshoni.toml:7) needs a public address, but …
//	  fix: Let's Encrypt only issues IP certificates for public addresses. …
func (p Problem) String() string {
	var b strings.Builder
	b.WriteString("config " + p.Severity + ": ")
	switch {
	case p.Key != "":
		b.WriteString(p.Key)
		if p.Value != "" {
			b.WriteString(" = " + p.Value)
		}
		b.WriteString(" (" + p.Source.String() + ") ")
	case p.Source.Kind == SourceEnv:
		b.WriteString(p.Source.Name + " (env) ")
	default:
		b.WriteString(p.Source.String() + ": ")
	}
	b.WriteString(p.Message)
	if !strings.HasSuffix(p.Message, ".") && !strings.HasSuffix(p.Message, "?") {
		b.WriteByte('.')
	}
	if p.Fix != "" {
		fix := p.Fix
		if !strings.HasSuffix(fix, ".") && !strings.HasSuffix(fix, "?") && !strings.HasSuffix(fix, ")") {
			fix += "."
		}
		b.WriteString("\n  fix: " + wrapIndent(fix, 110, "       "))
	}
	return b.String()
}

// ValidationError is returned by Load when the config has at least one error. Problems holds every problem, errors
// first, then warnings; Error prints them one after another as in §4.5.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	return strings.Join(parts, "\n")
}

// wrapIndent wraps text at width columns (the first line is shorter by the "  fix: " prefix, which has the same
// length as indent) and indents the continuation lines.
func wrapIndent(text string, width int, indent string) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(indent)+len(line)+1+len(word) > width:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"+indent)
}
