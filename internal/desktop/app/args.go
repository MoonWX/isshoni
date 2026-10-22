package app

import (
	"log/slog"
	"regexp"
	"strings"
)

// OneShot is a one-shot mode of 03 §13.4 step 1: it runs first, opens no window, skips single instance and creates
// no mutex. main dispatches it; HandleArgs never honors one (forwarded arguments are as untrusted as any command
// line, 03 §13.3).
type OneShot string

const (
	ModeVersion           OneShot = "--version"
	ModeSelfCheck         OneShot = "--self-check"         // --self-check=<path>
	ModeForgetCredentials OneShot = "--forget-credentials" //nolint:gosec // G101: a flag's name, no credential
	ModePurgeData         OneShot = "--purge-data"
	ModeUpdateNow         OneShot = "--update-now" // only builds with the updatetest tag honor it (06 §8.11)
	ModeEmbedding         OneShot = "-Embedding"   // COM activation of a toast button (03 §15)
)

// The flags of 03 §13.1 and §13.4.
const (
	flagOpenURL          = "--open-url"
	flagServer           = "--server"
	flagAutostart        = "--autostart"
	flagLogLevel         = "--log-level"
	flagUpdatedFrom      = "--updated-from"
	flagRelaunched       = "--relaunched"
	flagSoundCheckHelper = "--sound-check-helper"
)

// Args is a parsed command line (03 §13.1).
type Args struct {
	// OpenURL is set when the arguments contain --open-url. Then only Link, and Relaunched as the first argument,
	// are honored: every other argument, one-shot modes included, is ignored and logged by name (02 §12.2's
	// injection rule, 03 §13.1).
	OpenURL bool
	// Link is the one value after --open-url when it starts with "isshoni:" (the scheme in any case); "" otherwise.
	// It is not parsed yet: deeplink.Parse does that.
	Link string
	// Relaunched is --relaunched, which only turns the shell relaunch off; the relaunch puts it first (03 §13.4
	// step 2). With --open-url it counts only as the first argument.
	Relaunched bool
	// SoundCheckHelper is --sound-check-helper as the first argument (03 §13.4 step 0, 05 §11.3); main runs it
	// before anything else. Anywhere else it is ignored.
	SoundCheckHelper bool
	// OneShot is the first one-shot mode on the command line; "" when there is none or OpenURL is set.
	OneShot OneShot
	// SelfCheckPath is --self-check=<path>'s path.
	SelfCheckPath string
	// Server is --server <origin> (or --server=<origin>), as given: HandleArgs normalizes it like the address field.
	Server string
	// Autostart is --autostart: the start at sign-in, which opens no window (03 §16.1).
	Autostart bool
	// Debug is --log-level debug (or --log-level=debug).
	Debug bool
	// UpdatedFrom is --updated-from=<v> (the installer's relaunch after an update), when v looks like a version.
	UpdatedFrom string
	// Ignored names what was ignored, for the log: flags by name without their values, "(positional)" for a bare
	// argument, "(flag)" for something whose name isn't one. Never a value: a bare argument may be a URL with a path.
	Ignored []string
}

// flagName matches what may be logged as a flag's name.
var flagName = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9-]{0,40}$`)

// versionValue matches what may be logged as --updated-from's value.
var versionValue = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,64}$`)

// ParseArgs reads a command line (os.Args[1:], or the arguments a second instance forwarded) by 03 §13.1's rules.
// It never fails: what it can't use is ignored and named in Ignored.
func ParseArgs(args []string) Args {
	for i, a := range args {
		if a == flagOpenURL || strings.HasPrefix(a, flagOpenURL+"=") {
			return parseOpenURL(args, i)
		}
	}
	var p Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		// next takes --name <v> as well as --name=<v>; a flag is never taken as a value.
		next := func() (string, bool) {
			if hasValue {
				return value, true
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == string(ModeVersion), a == string(ModeForgetCredentials), a == string(ModePurgeData),
			a == string(ModeUpdateNow):
			p.oneShot(OneShot(a), a)
		case strings.EqualFold(a, string(ModeEmbedding)):
			p.oneShot(ModeEmbedding, a)
		case name == string(ModeSelfCheck):
			if p.oneShot(ModeSelfCheck, name) {
				p.SelfCheckPath = value
			}
		case a == flagSoundCheckHelper && i == 0:
			p.SoundCheckHelper = true
		case a == flagRelaunched:
			p.Relaunched = true
		case a == flagAutostart:
			p.Autostart = true
		case name == flagServer:
			if v, ok := next(); ok && v != "" {
				p.Server = v
			} else {
				p.Ignored = append(p.Ignored, flagServer)
			}
		case name == flagLogLevel:
			if v, ok := next(); ok && v == "debug" {
				p.Debug = true
			} else if v != "info" {
				p.Ignored = append(p.Ignored, flagLogLevel)
			}
		case name == flagUpdatedFrom:
			if v, ok := next(); ok && versionValue.MatchString(v) {
				p.UpdatedFrom = v
			} else {
				p.Ignored = append(p.Ignored, flagUpdatedFrom)
			}
		default:
			p.Ignored = append(p.Ignored, ignoredName(a))
		}
	}
	return p
}

// parseOpenURL is ParseArgs for a launch that contains --open-url at index i: exactly the next value is the link.
func parseOpenURL(args []string, i int) Args {
	p := Args{OpenURL: true}
	value, inline := strings.CutPrefix(args[i], flagOpenURL+"=")
	consumed := i
	if !inline {
		if i+1 < len(args) {
			value, consumed = args[i+1], i+1
		} else {
			value = ""
		}
	}
	if hasIsshoniScheme(value) {
		p.Link = value
	} else if value != "" {
		p.Ignored = append(p.Ignored, flagOpenURL+" (not an isshoni: link)")
	}
	for j, a := range args {
		switch {
		case j == i || j == consumed:
		case j == 0 && a == flagRelaunched:
			p.Relaunched = true
		default:
			p.Ignored = append(p.Ignored, ignoredName(a))
		}
	}
	return p
}

// oneShot records the first one-shot mode; later ones are ignored. It reports whether m was recorded.
func (p *Args) oneShot(m OneShot, arg string) bool {
	if p.OneShot != "" {
		p.Ignored = append(p.Ignored, ignoredName(arg))
		return false
	}
	p.OneShot = m
	return true
}

// ignoredName is what the log may say about an ignored argument.
func ignoredName(a string) string {
	if !strings.HasPrefix(a, "-") {
		return "(positional)"
	}
	name, _, _ := strings.Cut(a, "=")
	if flagName.MatchString(name) {
		return name
	}
	return "(flag)"
}

// hasIsshoniScheme reports a value that starts with "isshoni:", the scheme compared without regard to the case of
// its ASCII letters.
func hasIsshoniScheme(v string) bool {
	const scheme = "isshoni:"
	if len(v) < len(scheme) {
		return false
	}
	for i := range len(scheme) {
		c := v[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != scheme[i] {
			return false
		}
	}
	return true
}

// LogValue logs what a command line asked for, never a link, a path or a server address.
func (p Args) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Bool("open_url", p.OpenURL),
		slog.Bool("link", p.Link != ""),
		slog.Bool("server", p.Server != ""),
		slog.Bool("autostart", p.Autostart),
		slog.Bool("relaunched", p.Relaunched),
	}
	if p.OneShot != "" {
		attrs = append(attrs, slog.String("mode", string(p.OneShot)))
	}
	if p.UpdatedFrom != "" {
		attrs = append(attrs, slog.String("updated_from", p.UpdatedFrom))
	}
	if len(p.Ignored) > 0 {
		attrs = append(attrs, slog.Any("ignored", p.Ignored))
	}
	return slog.GroupValue(attrs...)
}
