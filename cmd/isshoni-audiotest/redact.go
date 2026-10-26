package main

import (
	"os"
	"os/user"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file is the report's redaction (docs/m2/05-windows-audio.md §18.7): the report is committed to a public
// repository, so it carries no user, host or device friendly names, no absolute paths (paths become relative to the
// work folder) and no names of apps that aren't fixtures ("other"). It is the default; `run -no-redact` skips it
// for local debugging only, and such a report is written under a name ending in "-unredacted".
//
// Most of the report is redacted by construction: fixtures are "fixture:<role>", endpoints {formFactor, bus}, the
// checks are numbers. What can carry a name is free text (a case's problems, an INFO case's info, an error from the
// engine or the OS) and the events' app ids. The redactor rewrites every string field of the report: free text has
// the work folders made relative, the harness folder and the home folders replaced, any other absolute path (and
// any path in a home folder) replaced by a placeholder that keeps at most a file name of the harness's kinds, and
// then every name the run collected (the engine's app and exe names, the local user, host and domain names) replaced
// by "<name>"; token fields (codes, versions, verdicts) are kept only when they look like tokens. It is portable and
// tested on every OS (redact_test.go).

// redactInfo is what a driver collected for the redaction during a run: the work folders its fixtures ran in, the
// folder of the harness itself, and the app and exe names the engine's reports showed (05 §18.7: "names collected
// at runtime").
type redactInfo struct {
	workDirs   []string
	harnessDir string
	names      []string
}

// localIDs are the machine's own identifiers: the user, host and domain names, and the home folders.
type localIDs struct {
	names []string
	homes []string
}

// currentLocalIDs reads the local user's and host's identifiers from the OS and the environment.
func currentLocalIDs() localIDs {
	var ids localIDs
	addName := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			ids.names = append(ids.names, n)
		}
	}
	if h, err := os.Hostname(); err == nil {
		addName(h)
		if i := strings.IndexByte(h, '.'); i > 0 {
			addName(h[:i])
		}
	}
	if u, err := user.Current(); err == nil {
		addName(u.Username)
		if i := strings.LastIndexAny(u.Username, `\/`); i >= 0 {
			addName(u.Username[:i]) // the domain of DOMAIN\user
			addName(u.Username[i+1:])
		}
		addName(u.Name)
		if u.HomeDir != "" {
			ids.homes = append(ids.homes, u.HomeDir)
		}
	}
	for _, k := range []string{"USERNAME", "USER", "LOGNAME", "COMPUTERNAME", "HOSTNAME", "USERDOMAIN",
		"USERDNSDOMAIN"} {
		addName(os.Getenv(k))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		ids.homes = append(ids.homes, h)
	}
	for _, k := range []string{"USERPROFILE", "HOME"} {
		if h := os.Getenv(k); h != "" {
			ids.homes = append(ids.homes, h)
		}
	}
	return ids
}

// redactor rewrites a report's strings (05 §18.7).
type redactor struct {
	workDirs   []string // longest first: paths below them become relative
	harnessDir string
	homes      []string // longest first: replaced by "<home>"
	names      []*regexp.Regexp
}

// The placeholders the redactor writes.
const (
	redactedName    = "<name>"
	redactedPath    = "<path>"
	redactedHome    = "<home>"
	redactedHarness = "<harness>"
)

// minNameRunes is the shortest name the redactor scrubs from free text: a shorter one would hit ordinary words.
const minNameRunes = 3

// newRedactor builds the redactor for a run's collected names and folders and the machine's own identifiers. Names
// that are the harness's own vocabulary (fixture exe names and roles, result and reason tokens) are kept: they are
// public and appear in every report.
func newRedactor(info redactInfo, local localIDs) *redactor {
	rd := &redactor{harnessDir: strings.TrimRight(info.harnessDir, `\/`)}
	for _, d := range info.workDirs {
		if d = strings.TrimRight(d, `\/`); d != "" && !slices.Contains(rd.workDirs, d) {
			rd.workDirs = append(rd.workDirs, d)
		}
	}
	for _, h := range local.homes {
		if h = strings.TrimRight(h, `\/`); len(h) > 1 && !slices.Contains(rd.homes, h) {
			rd.homes = append(rd.homes, h)
		}
	}
	byLen := func(a, b string) int { return len(b) - len(a) }
	slices.SortFunc(rd.workDirs, byLen)
	slices.SortFunc(rd.homes, byLen)

	keep := harnessVocabulary()
	var names []string
	for _, n := range append(append([]string(nil), info.names...), local.names...) {
		n = strings.TrimSpace(n)
		if utf8.RuneCountInString(n) < minNameRunes || keep[strings.ToLower(n)] || slices.Contains(names, n) {
			continue
		}
		names = append(names, n)
	}
	slices.SortFunc(names, byLen)
	for _, n := range names {
		// A name counts only as a whole word: not inside a longer word ("Ann" in "Announce").
		rd.names = append(rd.names, regexp.MustCompile(`(?i)(^|[^\pL\pN])(`+regexp.QuoteMeta(n)+`)($|[^\pL\pN])`))
	}
	return rd
}

// harnessVocabulary is every word the harness itself writes that a runtime name could equal: the fixture exe names
// (with and without ".exe") and folders, the roles, and the report's tokens.
func harnessVocabulary() map[string]bool {
	keep := map[string]bool{}
	add := func(w string) {
		if w != "" {
			keep[strings.ToLower(w)] = true
		}
	}
	for _, cs := range cases() {
		add(cs.name)
		for _, role := range fixtureRoles(cs) {
			add(role)
		}
		for _, exe := range append(copyList(cs), exeBursts) {
			for _, part := range strings.Split(exe, `\`) {
				add(part)
				add(strings.TrimSuffix(part, ".exe"))
			}
		}
	}
	for _, w := range []string{"isshoni", "isshoni-audiotest", "isshoni-audiotest.exe", "isshoni-media.dll", "fixture",
		otherRedacted, "heard", "kept_out", "not_captured", "pending", "builtin", "user", "mic", "viewer", "launcher",
		"tree", "pid_reuse", "limit", "capture_failed", "include-set", "exclude-self", "endpoint", "none",
		"explorer.exe", "windows", "microsoft"} {
		add(w)
	}
	return keep
}

// text redacts free text: the work folders become relative (a bare work folder "."), the harness folder and the
// home folders become placeholders, every other absolute path becomes "<path>" (one in a home folder "<home>"),
// keeping at most a plain file name at its end ("<path>\isshoni-media.dll"), and then every collected name becomes
// "<name>". Paths go before names: a name replaced inside a path would end the path there and leave the folders
// after it in the text.
func (rd *redactor) text(s string) string {
	if s == "" {
		return s
	}
	for _, d := range rd.workDirs {
		s = replacePathPrefix(s, d, "")
	}
	if rd.harnessDir != "" {
		s = replacePathPrefix(s, rd.harnessDir, redactedHarness)
	}
	for _, h := range rd.homes {
		s = replacePathPrefix(s, h, redactedHome)
	}
	s = redactPaths(s)
	for _, re := range rd.names {
		for range 4 { // adjacent matches share a separator: repeat until none is left
			next := re.ReplaceAllString(s, "${1}"+redactedName+"${3}")
			if next == s {
				break
			}
			s = next
		}
	}
	return s
}

// absPathStart finds where an absolute path starts in free text: a Windows drive path, a UNC path, or a POSIX path
// of two or more segments (a diagnostic run on the Mac or Linux), which the match takes with the delimiter before
// it. A redacted text has none left (unredactedProblem relies on it).
var absPathStart = regexp.MustCompile(`(?i)\b[a-z]:[\\/]|\\\\[^\s"'<>|*?\\/:]+[\\/]|(?:^|[\s"'(=])/[^\s"'<>|*?/:]+/`)

// pathStart is absPathStart plus a path in a home folder, which the home replacement has made "<home>\…".
var pathStart = regexp.MustCompile(absPathStart.String() + `|` + regexp.QuoteMeta(redactedHome) + `[\\/]`)

// redactPaths replaces every absolute path in s, and every path in a home folder, by its placeholder (redactPath).
func redactPaths(s string) string {
	var b strings.Builder
	for {
		loc := pathStart.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		start := loc[0]
		if strings.IndexByte(" \t\n\f\r\"'(=", s[start]) >= 0 {
			start++ // the delimiter before a POSIX path stays
		}
		end := pathEnd(s, start, loc[1])
		b.WriteString(s[:start])
		b.WriteString(redactPath(s[start:end]))
		s = s[end:]
	}
}

// pathEnd is where the path that starts at start (its start match ending at i) ends in s: at the first byte no
// Windows path segment can hold (a quote, '<', '>', '|', '*', '?', ':', a line end; Go's PathError puts ": " after
// the path) or at the end of s. Spaces don't end it, since folder names hold them, so words after a path may go with
// it: over-redaction, never a folder left behind. A ':' that is the next path's drive (" D:\") ends the path before
// that drive's letter, so the next path is redacted on its own; any other ":\" or ":/" (a URL) is taken along.
func pathEnd(s string, start, i int) int {
	for ; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '<', '>', '|', '*', '?', '\r', '\n':
			return i
		case ':':
			if i+1 >= len(s) || (s[i+1] != '\\' && s[i+1] != '/') {
				return i
			}
			if i-2 > start && isASCIILetter(s[i-1]) && !isWordByte(s[i-2]) {
				return i - 1
			}
		}
	}
	return len(s)
}

// isASCIILetter and isWordByte are regexp's ASCII letter and \w.
func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isWordByte(c byte) bool    { return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '_' }

// safeFileLead is a file name the redactor keeps at the end of a redacted path, the harness's own kinds of file,
// when it is the whole last segment or is followed by a space or punctuation (the words a greedy match took along).
var safeFileLead = regexp.MustCompile(`(?i)^(?:[a-z0-9._-]{1,64}\.(?:exe|dll|json|wav)|isshoni-audiotest)` +
	`(?:$|[\s,;)\]}]|\.(?:$|\s))`)

// redactPath replaces one path with its placeholder: "<path>", or for a path in a home folder "<home>" (in its temp
// folder "<home>\AppData\Local\Temp"). The placeholder stands for every folder of the path, however deep; only a
// safe file name in its last segment is kept (with the words after it), and trailing punctuation.
func redactPath(m string) string {
	core := strings.TrimRight(m, " \t.,;)]}")
	tail := m[len(core):]
	placeholder := redactedPath
	if rest, ok := strings.CutPrefix(core, redactedHome); ok {
		placeholder = redactedHome
		const temp = `appdata\local\temp`
		if f := foldPath(rest); len(f) > len(temp) && strings.HasPrefix(f[1:], temp) &&
			(len(f) == len(temp)+1 || f[len(temp)+1] == '\\') {
			sep := rest[:1]
			placeholder = redactedHome + sep + "AppData" + sep + "Local" + sep + "Temp"
		}
	}
	i := strings.LastIndexAny(core, `\/`)
	if i >= 0 && safeFileLead.MatchString(core[i+1:]) {
		return placeholder + core[i:] + tail
	}
	return placeholder + tail
}

// replacePathPrefix replaces every occurrence of the folder dir in s, compared case-insensitively (ASCII) with / and
// \ the same, by placeholder: "dir\rest" becomes placeholder + "\rest" (with the separator found), a bare "dir" the
// placeholder alone; with an empty placeholder "dir\rest" becomes the relative "rest" and a bare "dir" ".". An
// occurrence that is the start of a longer name ("dir2\x") is left alone.
func replacePathPrefix(s, dir, placeholder string) string {
	fd := foldPath(dir)
	if fd == "" {
		return s
	}
	var b strings.Builder
	for {
		fs := foldPath(s)
		i := strings.Index(fs, fd)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(fd)
		switch {
		case end < len(s) && (s[end] == '\\' || s[end] == '/'):
			sep := s[end]
			for end < len(s) && (s[end] == '\\' || s[end] == '/') {
				end++
			}
			b.WriteString(s[:i])
			if placeholder != "" {
				b.WriteString(placeholder)
				b.WriteByte(sep)
			}
		case end < len(s) && isPathByte(s[end]):
			b.WriteString(s[:end]) // a longer name that starts with dir
		default:
			b.WriteString(s[:i])
			if placeholder == "" {
				b.WriteString(".")
			} else {
				b.WriteString(placeholder)
			}
		}
		s = s[end:]
	}
}

// foldPath lower-cases ASCII letters and turns / into \, keeping the byte length (so indexes carry over).
func foldPath(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c == '/':
			b[i] = '\\'
		}
	}
	return string(b)
}

// isPathByte reports whether c continues a file or folder name.
func isPathByte(c byte) bool {
	return c >= 0x80 || c == '_' || c == '-' || c == '.' || c == '~' || c == '$' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// The token shapes of the report's non-free-text strings: anything else is replaced.
var (
	versionToken = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	shaToken     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	wordToken    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`)
	roleToken    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`) // a fixture role: "game", "gameChild"
	editionToken = regexp.MustCompile(`^[A-Za-z]{1,40}$`)
)

// token keeps s when it matches re (or is empty), else returns fallback.
func token(s string, re *regexp.Regexp, fallback string) string {
	if s == "" || re.MatchString(s) {
		return s
	}
	return fallback
}

// The endpoint form factors (EndpointFormFactor) and buses the report may name (05 §18.7: endpoint names become
// {formFactor, bus}); any other value is reported as "unknown" or "other".
var (
	knownFormFactors = []string{"RemoteNetworkDevice", "Speakers", "LineLevel", "Headphones", "Microphone", "Headset",
		"Handset", "UnknownDigitalPassthrough", "SPDIF", "DigitalAudioDisplayDevice", "UnknownFormFactor", "unknown"}
	knownBuses = []string{"usb", "hdaudio", "bluetooth", "pci", "virtual", "other", "unknown"}
)

// report redacts every string field of r in place (05 §18.7).
func (rd *redactor) report(r *report) {
	r.Harness = token(r.Harness, versionToken, "unknown")
	r.DLL.Version = token(r.DLL.Version, versionToken, "unknown")
	r.DLL.SHA256 = token(r.DLL.SHA256, shaToken, "")
	r.OS.Product = token(r.OS.Product, wordToken, "unknown")
	r.OS.Edition = token(r.OS.Edition, editionToken, "")
	r.Machine.CPU = token(rd.text(r.Machine.CPU), versionToken, "other")
	for i, e := range r.Machine.RenderEndpoints {
		if !slices.Contains(knownFormFactors, e.FormFactor) {
			r.Machine.RenderEndpoints[i].FormFactor = "unknown"
		}
		if !slices.Contains(knownBuses, e.Bus) {
			r.Machine.RenderEndpoints[i].Bus = "other"
		}
	}
	for i := range r.Cases {
		rd.caseReport(&r.Cases[i])
	}
	for i := range r.Manual {
		r.Manual[i].Label = token(r.Manual[i].Label, labelToken, "unlabelled")
	}
}

// caseReport redacts one case's strings: its problems and info as free text, its engines and event codes as tokens,
// and its events' apps as "fixture:<role>", "" (engine-wide) or "other".
func (rd *redactor) caseReport(cr *caseReport) {
	cr.Name = token(cr.Name, wordToken, "unknown")
	for i, p := range cr.Problems {
		cr.Problems[i] = rd.text(p)
	}
	for i, e := range cr.Engine {
		cr.Engine[i] = token(e, wordToken, otherRedacted)
	}
	for i, e := range cr.Events {
		cr.Events[i].Code = token(e.Code, wordToken, otherRedacted)
		cr.Events[i].App = redactEventApp(e.App)
	}
	if cr.Info != nil {
		keys := make([]string, 0, len(cr.Info))
		for k := range cr.Info {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cr.Info[k] = rd.value(cr.Info[k])
		}
	}
}

// redactEventApp is an event's app as the report shows it: a fixture's "fixture:<role>" (a role is a plain word),
// "" for an engine-wide event, and "other" for every other app.
func redactEventApp(app string) string {
	switch {
	case app == "":
		return ""
	case strings.HasPrefix(app, fixturePrefix) && roleToken.MatchString(strings.TrimPrefix(app, fixturePrefix)):
		return app
	default:
		return otherRedacted
	}
}

// value redacts an INFO case's value: strings as free text, lists and objects element by element.
func (rd *redactor) value(v any) any {
	switch x := v.(type) {
	case string:
		return rd.text(x)
	case []any:
		for i := range x {
			x[i] = rd.value(x[i])
		}
		return x
	case []string:
		for i := range x {
			x[i] = rd.text(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = rd.value(x[k])
		}
		return x
	default:
		return v
	}
}

// redactAppID maps an app id to its redacted form (05 §18.7): a fixture id stays "fixture:<role>", every other app
// is "other".
func redactAppID(id string) string {
	if strings.HasPrefix(id, fixturePrefix) {
		return id
	}
	return otherRedacted
}

// unredactedProblem is "" when r looks redacted, else why not: merge refuses a report written with -no-redact, which
// must never be committed. A redacted report has every event app a fixture's, "other" or "", and no absolute path
// left in its free text (the problems and the INFO values): a quiet room's -no-redact report may have no other app
// in its events but still name folders in its problems.
func unredactedProblem(r report) string {
	const why = ": it was written with -no-redact and must not be committed"
	for _, c := range r.Cases {
		for _, e := range c.Events {
			if e.App != redactEventApp(e.App) {
				return "the report names a non-fixture app in case " + c.Name + "'s events" + why
			}
		}
		for _, p := range c.Problems {
			if absPathStart.MatchString(p) {
				return "the report has an absolute path in case " + c.Name + "'s problems" + why
			}
		}
		if c.Info != nil && valueHasAbsPath(c.Info) {
			return "the report has an absolute path in case " + c.Name + "'s info" + why
		}
	}
	return ""
}

// valueHasAbsPath reports whether an INFO value holds a string with an absolute path, at any depth.
func valueHasAbsPath(v any) bool {
	switch x := v.(type) {
	case string:
		return absPathStart.MatchString(x)
	case []any:
		return slices.ContainsFunc(x, valueHasAbsPath)
	case []string:
		return slices.ContainsFunc(x, absPathStart.MatchString)
	case map[string]any:
		for _, e := range x {
			if valueHasAbsPath(e) {
				return true
			}
		}
	}
	return false
}
