package config

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// fileValue is one key's value from the TOML file.
type fileValue struct {
	src Source
	v   any // as go-toml decodes into map[string]any
}

// position is a 1-based line and column in the file.
type position struct{ line, col int }

// parseFile decodes a config file. It returns the values of registered keys and one problem per syntax error,
// unknown key (with its line, column and a suggestion) or misplaced section.
//
// go-toml/v2 decodes the document into a map first, which catches syntax errors and duplicate keys with their
// position. The registry is then walked against that map, so every unknown key is reported in one run (a strict
// decode into Config would stop at the first type error), and go-toml's parser supplies the line of every key.
// A leading UTF-8 byte order mark, as Windows Notepad writes it, is dropped first.
func parseFile(path string, data []byte) (map[string]fileValue, []Problem) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		src := Source{Kind: SourceFile, File: path}
		msg := strings.TrimPrefix(err.Error(), "toml: ")
		if de := (*toml.DecodeError)(nil); errors.As(err, &de) {
			src.Line, src.Column = de.Position()
		}
		return nil, []Problem{{
			Source: src, Severity: SeverityError,
			Message: "is not valid TOML: " + msg,
			Fix:     `check the syntax at that position: strings need double quotes (domain = "watch.example.com"), each key appears once, and sections look like [tls]`,
		}}
	}
	pos := keyPositions(data)
	w := &fileWalker{path: path, pos: pos, values: map[string]fileValue{}}
	w.walk("", doc)
	sort.SliceStable(w.problems, func(i, j int) bool { return w.problems[i].Source.Line < w.problems[j].Source.Line })
	return w.values, w.problems
}

type fileWalker struct {
	path     string
	pos      map[string]position
	values   map[string]fileValue
	problems []Problem
}

func (w *fileWalker) source(path string) Source {
	p := w.pos[path]
	return Source{Kind: SourceFile, File: w.path, Line: p.line}
}

func (w *fileWalker) walk(prefix string, table map[string]any) {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := table[name]
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if _, ok := keyByPath[path]; ok {
			w.values[path] = fileValue{src: w.source(path), v: v}
			continue
		}
		if prefix == "" && sections[path] {
			if m, ok := v.(map[string]any); ok {
				w.walk(path, m)
				continue
			}
			src := w.source(path)
			src.Column = w.pos[path].col
			w.problems = append(w.problems, Problem{
				Key: path, Source: src, Severity: SeverityError,
				Message: "is a section, not a value",
				Fix:     fmt.Sprintf("put its keys under a [%s] line, e.g. [%s] then %s", path, path, exampleLine(path)),
			})
			continue
		}
		w.unknown(path, v)
	}
}

// unknown reports an unknown key; for an unknown table, each key inside it, so that [tsl] with mode = "ip" in it
// suggests tls.mode.
func (w *fileWalker) unknown(path string, v any) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		w.problems = append(w.problems, unknownKey(path, w.path, w.pos[path]))
		return
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w.unknown(path+"."+name, m[name])
	}
}

// exampleLine is the first key line of a section in config example form, e.g. `mode = "auto"`.
func exampleLine(section string) string {
	for _, k := range registry {
		if k.Section() == section && !k.Hidden {
			return k.Name() + " = " + exampleValue(&k)
		}
	}
	return ""
}

// unknownKey is the problem for a key the registry doesn't know, with a "did you mean" suggestion when a key is
// within an edit distance of 2 (04 §4.2). A key that landed in the wrong table, typically a top-level key written
// below a [section] header (log.public_ip), is pointed at its right place instead of being called unknown.
func unknownKey(path, file string, p position) Problem {
	var names []string
	for _, k := range registry {
		names = append(names, k.Path)
	}
	fix := "remove it; 'isshoni config example' prints every key"
	if s := suggest(strings.ToLower(path), names); s != "" {
		fix = didYouMean(s)
	} else if s := misplacedKey(strings.ToLower(path)); s != "" {
		if k := registry[keyByPath[s]]; k.Section() == "" {
			fix = s + " is a top-level key: move the line above the first [section] header"
		} else {
			fix = didYouMean(s)
		}
	}
	return Problem{
		Key:      path,
		Source:   Source{Kind: SourceFile, File: file, Line: p.line, Column: p.col},
		Severity: SeverityError,
		Message:  "is not a config key",
		Fix:      fix,
	}
}

// didYouMean is the fix that suggests the registered key path, with its section form for a key in a section.
func didYouMean(path string) string {
	fix := "did you mean " + path + "?"
	if k := registry[keyByPath[path]]; k.Section() != "" {
		fix += fmt.Sprintf(" (%s = … under [%s])", k.Name(), k.Section())
	}
	return fix
}

// misplacedKey returns the registered key that an unknown path most likely meant to set from the wrong table, or "":
// the path without its first element when that is a key (tls.domain → domain, log.listen.http → listen.http), else
// the only key whose name is the path's last element (log.admin_socket → listen.admin_socket).
func misplacedKey(path string) string {
	_, rest, ok := strings.Cut(path, ".")
	if !ok {
		return ""
	}
	if _, ok := keyByPath[rest]; ok {
		return rest
	}
	name := path[strings.LastIndexByte(path, '.')+1:]
	found := ""
	for _, k := range registry {
		if k.Name() == name {
			if found != "" {
				return "" // ambiguous, like mode (tls.mode, registration.mode)
			}
			found = k.Path
		}
	}
	return found
}

// keyPositions returns the line and column of the first appearance of every key path in the document, table
// headers and dotted-key prefixes included. The document must already have decoded without error.
func keyPositions(data []byte) map[string]position {
	pos := map[string]position{}
	var p unstable.Parser
	p.Reset(data)
	var table []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = recordKey(&p, pos, nil, e.Key())
		case unstable.KeyValue:
			walkKeyValue(&p, pos, table, e)
		}
	}
	return pos
}

// walkKeyValue records a key-value's key under prefix and, for an inline table, the keys inside it.
func walkKeyValue(p *unstable.Parser, pos map[string]position, prefix []string, kv *unstable.Node) {
	path := recordKey(p, pos, prefix, kv.Key())
	if v := kv.Value(); v.Kind == unstable.InlineTable {
		for it := v.Children(); it.Next(); {
			if n := it.Node(); n.Kind == unstable.KeyValue {
				walkKeyValue(p, pos, path, n)
			}
		}
	}
}

// recordKey records every prefix of the dotted key under prefix at the position of its part, and returns the
// full path.
func recordKey(p *unstable.Parser, pos map[string]position, prefix []string, it unstable.Iterator) []string {
	path := append([]string(nil), prefix...)
	for it.Next() {
		n := it.Node()
		path = append(path, string(n.Data))
		joined := strings.Join(path, ".")
		if _, seen := pos[joined]; !seen {
			start := p.Shape(n.Raw).Start
			pos[joined] = position{line: start.Line, col: start.Column}
		}
	}
	return path
}

// suggest returns the candidate closest to name within an edit distance of 2, or "".
func suggest(name string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b (bytes).
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
