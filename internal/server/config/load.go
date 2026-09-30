package config

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// Load registers the config flags on set (--config and one flag per key), parses args, reads the file and the
// environment, applies the defaults and validates (§4.1).
//
// Precedence is flag > env > file > default. The file is --config, else ISSHONI_CONFIG, else DefaultPath; a
// missing file at DefaultPath is fine, a missing file at an explicit path is an error. An env variable set to ""
// counts as unset. environ is in os.Environ form; only ISSHONI_* names are looked at.
//
// Errors:
//   - A *ValidationError when the config has errors (all problems of every source, with a fix). The returned
//     *Config is still filled in, with the default in place of each value that could not be used, so that doctor can
//     report on it and the offline admin commands can find data_dir.
//   - Any other error comes from parsing args (flag.ErrHelp for -h, an unknown flag, a missing flag argument): a
//     usage error, and the *Config is nil.
//
// Warnings are in cfg.Warnings(). Flag values are never rejected while parsing: a malformed value becomes a
// Problem with its flag as the source, like a malformed env or file value.
func Load(set *flag.FlagSet, args []string, environ []string) (*Config, error) {
	return load(set, args, loadOptions{environ: environ, sources: true})
}

// LoadFlags is Load for `isshoni config example` and `isshoni config init` (§3.1): the values come only from the
// flags. No file is read (there is no --config flag) and the environment is ignored, so the output holds exactly
// what the caller passed.
func LoadFlags(set *flag.FlagSet, args []string) (*Config, error) {
	return load(set, args, loadOptions{})
}

// defaultPath is DefaultPath; tests point it elsewhere so they never read the machine's config.
var defaultPath = DefaultPath

type loadOptions struct {
	environ []string
	sources bool // read the file and the environment
}

// flagValue records a config flag's raw text; load parses it with the key's kind.
type flagValue struct {
	kind Kind
	text string
	set  bool
}

func (v *flagValue) String() string {
	if v == nil {
		return ""
	}
	return v.text
}

func (v *flagValue) Set(s string) error {
	v.text, v.set = s, true
	return nil
}

// IsBoolFlag lets a bool key be given as a bare --tls.hsts (= true).
func (v *flagValue) IsBoolFlag() bool { return v.kind == KindBool }

// registerFlags defines --config (when withConfig) and one flag per key on set.
func registerFlags(set *flag.FlagSet, withConfig bool) (configPath *string, values []*flagValue) {
	if withConfig {
		configPath = set.String("config", "", "the config file `PATH` (default "+DefaultPath+", env "+EnvConfig+")")
	}
	values = make([]*flagValue, len(registry))
	for i := range registry {
		k := &registry[i]
		v := &flagValue{kind: k.Kind}
		values[i] = v
		set.Var(v, k.FlagName(), k.Help)
	}
	return configPath, values
}

func load(set *flag.FlagSet, args []string, o loadOptions) (*Config, error) {
	configPath, flagValues := registerFlags(set, o.sources)
	if err := set.Parse(args); err != nil {
		return nil, err
	}

	c := newConfig()
	var problems []Problem

	// The file and the environment.
	var fileValues map[string]fileValue
	env := map[string]string{}
	if o.sources {
		env, problems = readEnv(o.environ)
		var fileProblems []Problem
		fileValues, fileProblems = c.readFile(*configPath, set.Lookup("config"), env[EnvConfig])
		problems = append(fileProblems, problems...)
	}

	// Every key: flag > env > file > default.
	for i := range registry {
		k := &registry[i]
		var (
			src Source
			v   any
			err error
		)
		switch fv, ev, file := flagValues[i], env[k.EnvName()], fileValues[k.Path]; {
		case fv.set:
			src = Source{Kind: SourceFlag, Name: "--" + k.FlagName()}
			v, err = parseText(k.Kind, fv.text)
			if err != nil {
				problems = append(problems, valueProblem(k, src, tomlString(fv.text), err))
			}
		case ev != "":
			src = Source{Kind: SourceEnv, Name: k.EnvName()}
			v, err = parseText(k.Kind, ev)
			if err != nil {
				problems = append(problems, valueProblem(k, src, tomlString(ev), err))
			}
		case file.src.Kind == SourceFile:
			src = file.src
			v, err = fromTOML(k.Kind, file.v)
			if err != nil {
				problems = append(problems, valueProblem(k, src, rawTOML(file.v), err))
			}
		default:
			continue
		}
		c.sources[k.Path] = src
		if err == nil {
			k.set(c, v)
		}
	}

	c.applyDerived()
	problems = append(problems, c.Validate()...)
	c.problems = sortProblems(problems)
	if slices.ContainsFunc(c.problems, func(p Problem) bool { return p.Severity == SeverityError }) {
		return c, &ValidationError{Problems: c.Problems()}
	}
	return c, nil
}

// newConfig returns a Config with every key at its default.
func newConfig() *Config {
	c := &Config{sources: map[string]Source{}}
	for i := range registry {
		registry[i].set(c, cloneValue(registry[i].Default))
	}
	return c
}

// applyDerived fills in the derived defaults of §4.3 and §4.4 for keys that were not set: listen.http in off mode,
// and network.trusted_proxies in off mode with a loopback listen.http.
func (c *Config) applyDerived() {
	if c.EffectiveTLSMode() != TLSOff {
		return
	}
	if !c.IsSet("listen.http") {
		c.Listen.HTTP = offModeHTTP
	}
	if !c.IsSet("network.trusted_proxies") && isLoopbackListen(c.Listen.HTTP) {
		c.Network.TrustedProxies = slices.Clone(loopbackProxies)
	}
}

// derivedTrustedProxies reports whether network.trusted_proxies holds the loopback default of §4.4 (which config
// init writes into the file explicitly).
func (c *Config) derivedTrustedProxies() bool {
	return c.EffectiveTLSMode() == TLSOff && !c.IsSet("network.trusted_proxies") && isLoopbackListen(c.Listen.HTTP)
}

// readFile reads and decodes the config file. flagPath is --config ("" when not given).
func (c *Config) readFile(flagPath string, configFlag *flag.Flag, envPath string) (map[string]fileValue, []Problem) {
	path, explicit, pathSrc := defaultPath, false, Source{Kind: SourceDefault}
	switch {
	case flagPath != "":
		path, explicit, pathSrc = flagPath, true, Source{Kind: SourceFlag, Name: "--" + configFlag.Name}
	case envPath != "":
		path, explicit, pathSrc = envPath, true, Source{Kind: SourceEnv, Name: EnvConfig}
	}
	c.file = path
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		c.fileRead = true
		return parseFile(path, data)
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		return nil, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, []Problem{{
			Source: pathSrc, Severity: SeverityError,
			Message: "names the config file " + path + ", which does not exist",
			Fix:     "check the path, or create the file with 'isshoni config init --path " + path + "'",
		}}
	case errors.Is(err, fs.ErrPermission):
		return nil, []Problem{{
			Source: Source{Kind: SourceFile, File: path}, Severity: SeverityError,
			Message: "can't be read: permission denied",
			Fix:     "run the command with sudo, or make the file readable by the isshoni group (root:isshoni, mode 0640)",
		}}
	default:
		return nil, []Problem{{
			Source: Source{Kind: SourceFile, File: path}, Severity: SeverityError,
			Message: "can't be read: " + err.Error(),
			Fix:     "check that the path is a readable file",
		}}
	}
}

// readEnv returns the non-empty ISSHONI_* variables of environ that are keys or env-only switches (the last
// occurrence wins), and a warning for every other one that is not a reserved name (§4.2).
func readEnv(environ []string) (map[string]string, []Problem) {
	env := map[string]string{}
	var names []string
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "ISSHONI_") {
			continue
		}
		if _, seen := env[name]; !seen {
			names = append(names, name)
		}
		env[name] = value
	}
	known := map[string]bool{}
	var envNames []string
	for _, k := range registry {
		known[k.EnvName()] = true
		envNames = append(envNames, k.EnvName())
	}
	for _, n := range envOnly {
		known[n] = true
	}
	var problems []Problem
	for _, name := range names {
		switch {
		case env[name] == "": // empty means unset (§4.2), also for unknown names
			delete(env, name)
		case known[name]:
		case slices.Contains(reservedEnv, name):
			delete(env, name)
		default:
			delete(env, name)
			fix := "unset it; the env names are ISSHONI_ plus a key path in upper case with . → _ ('isshoni config example' lists the keys)"
			if s := suggest(name, envNames); s != "" {
				fix = "did you mean " + s + "?"
			}
			problems = append(problems, Problem{
				Source: Source{Kind: SourceEnv, Name: name}, Severity: SeverityWarning,
				Message: "is not a config key; it is ignored",
				Fix:     fix,
			})
		}
	}
	return env, problems
}

// valueProblem is the problem for a value that doesn't fit its key's kind.
func valueProblem(k *Key, src Source, value string, err error) Problem {
	p := Problem{Key: k.Path, Value: value, Source: src, Severity: SeverityError, Message: err.Error()}
	var ve *valueError
	if errors.As(err, &ve) {
		p.Fix = ve.fix
	}
	return p
}

// rawTOML renders a value decoded from the file (which may be of the wrong type) in TOML syntax for a Problem.
func rawTOML(v any) string {
	switch v := v.(type) {
	case string:
		return tomlString(v)
	case int64:
		return formatTOML(int(v))
	case bool:
		return formatTOML(v)
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = rawTOML(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return "{…}"
	default:
		return formatTOML(v)
	}
}

// sortProblems puts errors before warnings and keeps the order otherwise.
func sortProblems(ps []Problem) []Problem {
	rank := func(p Problem) int {
		if p.Severity == SeverityError {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(ps, func(a, b Problem) int { return rank(a) - rank(b) })
	return ps
}
