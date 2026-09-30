package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/MoonWX/isshoni/internal/version"
)

// flagSet returns cmd's flag set with its flags registered (the root's --version, a leaf's own flags) and, for a
// leaf, the function that runs it.
func (c *command) flagSet() (*flag.FlagSet, runFunc) {
	fs := flag.NewFlagSet(c.path(), flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors and usage are ours to print
	fs.Usage = func() {}
	switch {
	case c.setup != nil:
		return fs, c.setup(fs)
	case c.parent == nil:
		fs.Bool("version", false, "print build information and exit")
	}
	return fs, nil
}

// writeHelp writes cmd's usage to w: the usage line, the description (full only), subcommands or flags, and hints.
func writeHelp(w io.Writer, c *command, full bool) error {
	var b bytes.Buffer
	fs, _ := c.flagSet()

	b.WriteString("Usage: " + c.usageLine() + "\n")
	if full {
		desc := c.help
		if desc == "" {
			desc = c.summary + "."
		}
		b.WriteString("\n" + wrap(desc, 100) + "\n")
	}

	if len(c.subs) > 0 {
		b.WriteString("\nCommands:\n")
		var rows [][2]string
		for _, s := range c.subs {
			rows = append(rows, [2]string{s.name, s.summary})
		}
		writeRows(&b, rows)
	}

	var rows [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		left := "--" + f.Name
		if name != "" {
			left += " " + name
		}
		if def := defaultText(f); def != "" {
			usage += " (default " + def + ")"
		}
		rows = append(rows, [2]string{left, usage})
	})
	rows = append(rows, [2]string{"-h, --help", "show this help"})
	b.WriteString("\nFlags:\n")
	writeRows(&b, rows)

	if c.config {
		// The config package registers these through config.Load (04 §4.2, §4.7).
		b.WriteString("\nConfig flags: --config PATH (default /etc/isshoni/isshoni.toml, env ISSHONI_CONFIG) and one\n" +
			"flag per config key, such as --tls.mode or --public-ip. See 'isshoni config example'.\n")
	}

	switch {
	case c.parent == nil:
		b.WriteString("\nRun 'isshoni help <command>' for more about a command.\nDocumentation: " + version.DocsURL + "\n")
	case len(c.subs) > 0:
		b.WriteString("\nRun 'isshoni help " + strings.TrimPrefix(c.path(), "isshoni ") + " <command>' for more about a command.\n")
	case !full:
		b.WriteString("\nRun 'isshoni help " + strings.TrimPrefix(c.path(), "isshoni ") + "' for more.\n")
	}
	_, err := w.Write(b.Bytes())
	return err
}

func (c *command) usageLine() string {
	if len(c.subs) > 0 {
		return c.path() + " <command> [flags] [arguments]"
	}
	line := c.path() + " [flags]"
	if c.config {
		line += " [config flags]"
	}
	if c.args != "" {
		line += " " + c.args
	}
	return line
}

// defaultText is a flag's default for the help, or "" for a zero value.
func defaultText(f *flag.Flag) string {
	switch d := f.DefValue; d {
	case "", "0", "false", "0s":
		return ""
	default:
		// time.Duration prints 30m as 30m0s and 2h as 2h0m0s.
		if strings.HasSuffix(d, "m0s") {
			d = strings.TrimSuffix(d, "0s")
		}
		if strings.HasSuffix(d, "h0m") {
			d = strings.TrimSuffix(d, "0m")
		}
		return d
	}
}

func writeRows(b *bytes.Buffer, rows [][2]string) {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	for _, r := range rows {
		fmt.Fprintf(b, "  %-*s  %s\n", width, r[0], r[1])
	}
}

// wrap breaks text into lines of at most width characters (words longer than width stay whole). Blank lines separate
// paragraphs.
func wrap(text string, width int) string {
	var out []string
	for _, para := range strings.Split(text, "\n\n") {
		var lines []string
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) > width:
				lines = append(lines, line)
				line = word
			default:
				line += " " + word
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n\n")
}

// helpCmd is "isshoni help [COMMAND…]".
func helpCmd() *command {
	return &command{
		name:    "help",
		args:    "[COMMAND…]",
		summary: "Show help for a command",
		maxArgs: -1,
		setup: func(*flag.FlagSet) runFunc {
			return func(_ context.Context, inv *invocation, args []string) error {
				c := root()
				for _, name := range args {
					next := c.sub(name)
					if next == nil {
						return unknownCommand(c, name)
					}
					c = next
				}
				return writeHelp(inv.stdout, c, true)
			}
		},
	}
}
