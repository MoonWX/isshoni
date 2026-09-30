package protocol

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth bounds the nesting of objects and arrays in a message (envelope included). Real messages nest at most 6
// levels (room.state → shares → watchers); relay payloads are opaque but small. It also bounds the parse stack of
// encoding/json, which would otherwise grow with the input.
const maxDepth = 32

var errTooDeep = errors.New("protocol: message nested too deeply")

// limitNode describes the array limits of one payload type as a tree of JSON keys. Keys match the way
// encoding/json matches them: exactly or case-insensitively (Unicode simple folding, as bytes.EqualFold).
type limitNode struct {
	key      []byte       // JSON key
	path     string       // full path for FieldError, e.g. "caps.decode"
	max      int          // > 0: the value is an array with at most max elements
	children []*limitNode // the value is an object with these limited keys
}

// child returns the node of key (the raw contents of a JSON string, escaped reports a backslash in it), or nil.
func (n *limitNode) child(key []byte, escaped bool) *limitNode {
	if n == nil {
		return nil
	}
	if escaped {
		// Rare: a key with escapes. Unescape it the way encoding/json does, into a buffer on the stack; a key
		// that doesn't fit is longer than any limited key, even folded (a rune folds to one of at most 3 bytes).
		var buf [64]byte
		var ok bool
		if key, ok = unescapeKey(key, buf[:]); !ok {
			return nil
		}
	}
	for _, c := range n.children {
		if bytes.EqualFold(key, c.key) {
			return c
		}
	}
	return nil
}

// unescapeKey decodes the contents of a JSON string into buf without allocating, as encoding/json's unquote does:
// escapes, surrogate pairs, and U+FFFD for invalid surrogates and invalid UTF-8. ok is false when the result does
// not fit in buf or raw is not a valid string body (encoding/json then rejects the message anyway).
func unescapeKey(raw, buf []byte) (out []byte, ok bool) {
	out = buf[:0]
	for i := 0; i < len(raw); {
		if len(out)+utf8.UTFMax > cap(out) {
			return nil, false
		}
		c := raw[i]
		switch {
		case c == '\\':
			if i+1 >= len(raw) {
				return nil, false
			}
			switch e := raw[i+1]; e {
			case '"', '\\', '/', '\'':
				out = append(out, e)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r := getu4(raw[i:])
				if r < 0 {
					return nil, false
				}
				if utf16.IsSurrogate(r) {
					if dec := utf16.DecodeRune(r, getu4(raw[i+6:])); dec != utf8.RuneError {
						r = dec
						i += 6
					} else {
						r = utf8.RuneError
					}
				}
				out = utf8.AppendRune(out, r)
				i += 6
				continue
			default:
				return nil, false
			}
			i += 2
		case c < ' ':
			return nil, false
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(raw[i:])
			out = utf8.AppendRune(out, r)
			i += size
		}
	}
	return out, true
}

// getu4 decodes the \uXXXX escape at the start of s, or returns -1.
func getu4(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return -1
	}
	var r rune
	for _, c := range s[2:6] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}

// arrayLimit is one entry for newLimits: a dotted path from the payload root and the maximum element count.
type arrayLimit struct {
	path string
	max  int
}

// newLimits builds the limit tree of a payload type, e.g. newLimits(arrayLimit{"caps.decode", MaxCodecs}).
func newLimits(ls ...arrayLimit) *limitNode {
	root := &limitNode{}
	for _, l := range ls {
		n := root
		for i, name := range strings.Split(l.path, ".") {
			var next *limitNode
			for _, c := range n.children {
				if string(c.key) == name {
					next = c
				}
			}
			if next == nil {
				next = &limitNode{key: []byte(name), path: strings.Join(strings.Split(l.path, ".")[:i+1], ".")}
				n.children = append(n.children, next)
			}
			n = next
		}
		n.max = l.max
	}
	return root
}

type scanFrame struct {
	obj       bool
	expectKey bool       // object: the next string is a key
	node      *limitNode // object: limits of its keys (nil = untracked)
	value     *limitNode // object: limits of the current key's value
	limit     *limitNode // array: its limit (nil = untracked)
	count     int        // array: elements seen so far
}

// scanJSON walks b once and fails when containers nest deeper than maxDepth or when an array named in root has more
// elements than allowed (a *FieldError with reason too_many). It allocates nothing (escaped keys are unescaped into
// a stack buffer). It is not a validator: on malformed JSON it returns nil and leaves the syntax error to
// encoding/json, which the caller runs next.
func scanJSON(b []byte, root *limitNode) error {
	var stack [maxDepth]scanFrame
	depth := 0
	// valueStart records that a value begins in the innermost container and returns the limit node that describes
	// it (nil = untracked).
	valueStart := func() (*limitNode, error) {
		if depth == 0 {
			return root, nil
		}
		f := &stack[depth-1]
		if f.obj {
			v := f.value
			f.value = nil
			return v, nil
		}
		if f.count == 0 {
			f.count = 1
			if f.limit != nil && f.count > f.limit.max {
				return nil, &FieldError{Field: f.limit.path, Reason: FieldTooMany}
			}
		}
		return nil, nil
	}
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case ' ', '\t', '\n', '\r', ':':
		case '"':
			start, escaped := i+1, false
			for i++; i < len(b) && b[i] != '"'; i++ {
				if b[i] == '\\' {
					escaped = true
					i++
				}
			}
			end := min(i, len(b))
			if depth > 0 && stack[depth-1].obj && stack[depth-1].expectKey {
				f := &stack[depth-1]
				f.expectKey = false
				f.value = f.node.child(b[start:end], escaped)
				continue
			}
			if _, err := valueStart(); err != nil {
				return err
			}
		case '{', '[':
			n, err := valueStart()
			if err != nil {
				return err
			}
			if depth == maxDepth {
				return errTooDeep
			}
			f := scanFrame{obj: c == '{'}
			if f.obj {
				f.expectKey = true
				if n != nil && len(n.children) > 0 {
					f.node = n
				}
			} else if n != nil && n.max > 0 {
				f.limit = n
			}
			stack[depth] = f
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				continue
			}
			f := &stack[depth-1]
			if f.obj {
				f.expectKey, f.value = true, nil
				continue
			}
			f.count++
			if f.limit != nil && f.count > f.limit.max {
				return &FieldError{Field: f.limit.path, Reason: FieldTooMany}
			}
		default: // a number or a literal (or garbage, which encoding/json reports)
			if _, err := valueStart(); err != nil {
				return err
			}
		}
	}
	return nil
}
