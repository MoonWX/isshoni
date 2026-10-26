// Package i18n gives the desktop app's Go code its texts (docs/m2/03-desktop-app.md §18.1, "03" below): the tray,
// the Sharing pill, toasts and native message boxes. They come from the SPA's own catalog file,
// web/src/i18n/lazy/desktop.en.json (web.DesktopCatalog(), which main passes to Load), so the SPA and Go share one
// file and one review.
//
// Go reads only the desktop.native.* keys, with i18next's forms: {{name}} interpolation, and the _one/_other plural
// suffixes chosen by a count variable. A key that is missing returns the key itself and logs one WARN per key.
// i18n_test.go fails when Go code under internal/desktop or cmd/isshoni-desktop names a desktop.native key the file
// doesn't have.
package i18n

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Prefix is the part of the catalog Go reads.
const Prefix = "desktop.native."

// Catalog holds the desktop.native.* messages. A nil *Catalog is valid: T returns the key. Safe for concurrent use.
type Catalog struct {
	msgs map[string]string // full keys ("desktop.native.tray.open") → text
	log  *slog.Logger

	mu     sync.Mutex
	warned map[string]bool // missing keys already logged
}

// Load reads a catalog file, `{"desktop": {"native": {…}, …}}` as desktop.en.json is, and keeps its
// desktop.native.* messages. A file that isn't such JSON, or whose messages aren't strings, is an error. log nil
// means slog.Default().
func Load(data []byte, log *slog.Logger) (*Catalog, error) {
	if log == nil {
		log = slog.Default()
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("i18n: the catalog is not JSON: %w", err)
	}
	msgs := make(map[string]string)
	if err := flatten("", root, msgs); err != nil {
		return nil, err
	}
	for k := range msgs {
		if !strings.HasPrefix(k, Prefix) {
			delete(msgs, k)
		}
	}
	if len(msgs) == 0 {
		return nil, errors.New("i18n: the catalog has no desktop.native messages")
	}
	return &Catalog{msgs: msgs, log: log.With("component", "i18n"), warned: make(map[string]bool)}, nil
}

// flatten turns nested objects into dotted keys. Every leaf must be a string, as in the SPA's catalog.
func flatten(prefix string, node map[string]any, out map[string]string) error {
	for k, v := range node {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch v := v.(type) {
		case string:
			out[key] = v
		case map[string]any:
			if err := flatten(key, v, out); err != nil {
				return err
			}
		default:
			return fmt.Errorf("i18n: %s is neither a message nor a group of messages", key)
		}
	}
	return nil
}

// T returns the message for key with args interpolated. args are name, value pairs, as slog takes them:
// T("desktop.native.pill.live", "count", 3). A count argument picks the plural form key_one (count 1) or key_other,
// when the catalog has them (English plural rules, the only language in M2); otherwise key itself. {{name}} becomes
// the value of name (fmt's %v); a placeholder without an argument stays as it is, as i18next leaves it. A missing
// key returns the key and logs one WARN.
func (c *Catalog) T(key string, args ...any) string {
	if c == nil {
		return key
	}
	vars := pairs(args)
	msg, ok := c.lookup(key, vars)
	if !ok {
		c.warnOnce(key)
		return key
	}
	return interpolate(msg, vars)
}

// Has reports whether key, or its plural forms, is in the catalog.
func (c *Catalog) Has(key string) bool {
	if c == nil {
		return false
	}
	if _, ok := c.msgs[key]; ok {
		return true
	}
	_, one := c.msgs[key+"_one"]
	_, other := c.msgs[key+"_other"]
	return one && other
}

// Keys returns the number of messages the catalog holds.
func (c *Catalog) Keys() int {
	if c == nil {
		return 0
	}
	return len(c.msgs)
}

func (c *Catalog) lookup(key string, vars map[string]any) (string, bool) {
	if n, ok := count(vars); ok {
		suffix := "_other"
		if n == 1 {
			suffix = "_one"
		}
		if msg, ok := c.msgs[key+suffix]; ok {
			return msg, true
		}
	}
	msg, ok := c.msgs[key]
	return msg, ok
}

func (c *Catalog) warnOnce(key string) {
	c.mu.Lock()
	seen := c.warned[key]
	c.warned[key] = true
	c.mu.Unlock()
	if !seen {
		c.log.Warn("i18n: missing message", "key", key)
	}
}

// pairs reads name, value pairs; a name that isn't a string, and a name without a value, are skipped.
func pairs(args []any) map[string]any {
	if len(args) < 2 {
		return nil
	}
	vars := make(map[string]any, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		if name, ok := args[i].(string); ok {
			vars[name] = args[i+1]
		}
	}
	return vars
}

// count reads the count variable as a whole number.
func count(vars map[string]any) (int64, bool) {
	v, ok := vars["count"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(min(uint64(n), math.MaxInt64)), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(min(n, math.MaxInt64)), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int64(n), true
		}
	}
	return 0, false
}

// interpolate replaces {{name}} (spaces inside the braces allowed) with the value of name.
func interpolate(msg string, vars map[string]any) string {
	if !strings.Contains(msg, "{{") {
		return msg
	}
	var b strings.Builder
	rest := msg
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := strings.Index(rest[start+2:], "}}")
		if end < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end += start + 2
		name := strings.TrimSpace(rest[start+2 : end])
		b.WriteString(rest[:start])
		if v, ok := vars[name]; ok {
			b.WriteString(format(v))
		} else {
			b.WriteString(rest[start : end+2])
		}
		rest = rest[end+2:]
	}
}

func format(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}
