package applog

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
)

// redactor writes the user's profile folder as %USERPROFILE% in a record's message and in every string value,
// error and other value whose text holds it (03 §17). The handler it wraps applies logx's rules.
type redactor struct {
	h        slog.Handler
	profiles []string // the profile folder, with \ and with / as separators; none when empty
}

func newRedactor(h slog.Handler, profile string) slog.Handler {
	r := &redactor{h: h}
	profile = strings.TrimRight(profile, `\/`)
	if len(profile) < 3 { // never "", a bare drive or a root
		return r
	}
	back := strings.ReplaceAll(profile, "/", `\`)
	// Longest first, so the JSON-escaped spelling (C:\\Users\\x, inside a struct written as JSON) goes before the
	// plain one it contains parts of.
	for _, p := range []string{strings.ReplaceAll(back, `\`, `\\`), back, strings.ReplaceAll(profile, `\`, "/")} {
		if !slices.Contains(r.profiles, p) {
			r.profiles = append(r.profiles, p)
		}
	}
	return r
}

func (r *redactor) Enabled(ctx context.Context, l slog.Level) bool { return r.h.Enabled(ctx, l) }

func (r *redactor) Handle(ctx context.Context, rec slog.Record) error {
	if len(r.profiles) == 0 {
		return r.h.Handle(ctx, rec)
	}
	out := slog.NewRecord(rec.Time, rec.Level, r.scrub(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(r.attr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r *redactor) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = r.attr(a)
	}
	return &redactor{h: r.h.WithAttrs(out), profiles: r.profiles}
}

func (r *redactor) WithGroup(name string) slog.Handler {
	return &redactor{h: r.h.WithGroup(name), profiles: r.profiles}
}

func (r *redactor) attr(a slog.Attr) slog.Attr {
	if len(r.profiles) == 0 {
		return a
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, r.scrub(v.String()))
	case slog.KindGroup:
		group := v.Group()
		out := make([]slog.Attr, len(group))
		for i, g := range group {
			out[i] = r.attr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			if s := x.Error(); r.has(s) {
				return slog.String(a.Key, r.scrub(s))
			}
		default:
			// A struct, map or slice: the JSON handler would write it as JSON; when that text holds the profile
			// folder, the redacted text is written instead.
			if data, err := json.Marshal(x); err == nil && r.has(string(data)) {
				return slog.String(a.Key, r.scrub(string(data)))
			}
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

func (r *redactor) has(s string) bool {
	for _, p := range r.profiles {
		if indexFold(s, p) >= 0 {
			return true
		}
	}
	return false
}

// scrub replaces every profile spelling, compared without regard to case (Windows paths), with ProfileToken.
func (r *redactor) scrub(s string) string {
	for _, p := range r.profiles {
		from := 0
		for {
			i := indexFold(s[from:], p)
			if i < 0 {
				break
			}
			i += from
			s = s[:i] + ProfileToken + s[i+len(p):]
			from = i + len(ProfileToken)
		}
	}
	return s
}

// indexFold is strings.Index without regard to case, for whole folder names only: a match must end the string or be
// followed by a separator or a character that can't continue a folder name, so C:\Users\Al never matches inside
// C:\Users\Alexander or C:\Users\Al Smith.
func indexFold(s, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], sub) && (i+n == len(s) || endsName(s[i+n])) {
			return i
		}
	}
	return -1
}

// endsName reports whether c can't be part of a folder name, so a path ends before it.
func endsName(c byte) bool {
	return c < ' ' || strings.IndexByte(`\/"'*:;,<>|?)]}`, c) >= 0
}
