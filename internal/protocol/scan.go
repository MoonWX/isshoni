package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth bounds the nesting of objects and arrays in a message (envelope included). Real messages nest at most 6
// levels (room.state → shares → watchers); relay payloads are opaque but small. It also bounds the parse stack of
// encoding/json, which would otherwise grow with the input.
const maxDepth = 32

var errTooDeep = errors.New("protocol: message nested too deeply")

// maxKeyLen bounds the name of an object member in a payload that Decode reads, in bytes as the message spells it
// (between the quotes, escapes not decoded). The names of this protocol are under 32 bytes.
//
// It is there for the names that a payload's type does not know. encoding/json looks such a name up through a
// case-folded copy of it, in a buffer that it grows step by step and allocates anew at each step. With Go 1.27 that
// adds up to at most 3.7 times a name within this limit (and 3.8 times one of up to 2048 bytes), but to more than 4
// times some longer ones, 5.2 times at worst: a message of such names costs more than the 4 times its size that
// Decode may (01 §19). ParseEnvelope has no such limit and needs none: it copies no name.
const maxKeyLen = 1024

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
// validPayload, which Decode runs next, before encoding/json reads the payload. The envelope around the payload has
// a strict scanner of its own (scanEnvelope, below) and is never read by encoding/json.
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

// The envelope scanner: ParseEnvelope's own strict JSON reader. A frame comes from an untrusted peer, and an
// envelope has four keys: everything else in the frame is only checked and skipped. The scanner therefore keeps
// nothing but positions in the frame, so a key or a value that the envelope does not keep costs no memory, however
// long it is and wherever the frame breaks off. (encoding/json keeps the names on the way to a value, for its error
// positions, and compares every name with the struct's fields through copies of it: several times the length of a
// long key, most of all in a frame that ends right after one.)
//
// It reads what encoding/json reads into an Envelope, with the same result (the tests and FuzzParseEnvelope compare
// the two):
//   - the frame is one JSON value by RFC 8259 with nothing but whitespace around it, and that value is an object;
//   - a key names a field when it equals the field's name under Unicode case folding (bytes.EqualFold), after
//     unescaping: TYPE is type, and so is a key that spells one of its letters as a \u escape;
//   - when a key occurs more than once, the last value counts;
//   - type, id and re take a string; null leaves the field as it is; any other value is an error;
//   - data takes any value, null included, as the bytes of the frame. ParseEnvelope then accepts an object or null.

// The ParseEnvelope errors. They are built once: rejecting a frame allocates nothing.
var (
	errEnvelopeUTF8      = fmt.Errorf("%w: not valid UTF-8", ErrBadMessage)
	errEnvelopeSyntax    = fmt.Errorf("%w: not a JSON object", ErrBadMessage)
	errEnvelopeDepth     = fmt.Errorf("%w: %w", ErrBadMessage, errTooDeep)
	errEnvelopeNotString = fmt.Errorf("%w: type, id or re is not a string", ErrBadMessage)
	errEnvelopeNoType    = fmt.Errorf("%w: missing type", ErrBadMessage)
	errEnvelopeTypeLen   = fmt.Errorf("%w: type too long", ErrBadMessage)
	errEnvelopeID        = fmt.Errorf("%w: invalid id", ErrBadMessage)
	errEnvelopeRe        = fmt.Errorf("%w: invalid re", ErrBadMessage)
	errEnvelopeData      = fmt.Errorf("%w: data is not an object", ErrBadMessage)
)

// The JSON names of the Envelope fields (TestEnvelopeScanKnowsEveryField holds them to the struct).
var (
	envelopeNameType = []byte("type")
	envelopeNameID   = []byte("id")
	envelopeNameRe   = []byte("re")
	envelopeNameData = []byte("data")
)

// envelopeScan is what scanEnvelope found in a frame: the values that count, as parts of the frame.
type envelopeScan struct {
	// typ, id and re are the contents of the field's last string value: without the quotes, still escaped. A field
	// without a string value is empty, which reads as "".
	typ, id, re []byte
	// data is the last value of the data key, nil when the frame has none.
	data []byte
}

// field returns where the value of the member called name goes, or nil for a name that the envelope ignores. name
// is the contents of a JSON string that scanString accepted; escaped reports a backslash in it.
func (s *envelopeScan) field(name []byte, escaped bool) *[]byte {
	if escaped {
		// Unescaped into a buffer on the stack. A name that doesn't fit is longer than any envelope key, even
		// folded (a rune folds to one of at most 3 bytes).
		var buf [64]byte
		var ok bool
		if name, ok = unescapeKey(name, buf[:]); !ok {
			return nil
		}
	}
	switch {
	case bytes.EqualFold(name, envelopeNameType):
		return &s.typ
	case bytes.EqualFold(name, envelopeNameID):
		return &s.id
	case bytes.EqualFold(name, envelopeNameRe):
		return &s.re
	case bytes.EqualFold(name, envelopeNameData):
		return &s.data
	}
	return nil
}

// scanEnvelope checks that b (valid UTF-8) is one JSON object, nested at most maxDepth levels deep, and finds the
// envelope's fields in it. It allocates nothing.
func scanEnvelope(b []byte) (envelopeScan, error) {
	var s envelopeScan
	end, err := scanObject(b, skipSpace(b, 0), func(name []byte, escaped bool, start int) (int, error) {
		end, err := skipValue(b, start, 1, 0)
		if err != nil {
			return 0, err
		}
		switch field := s.field(name, escaped); {
		case field == nil: // not a key of the envelope: checked and skipped
		case field == &s.data:
			s.data = b[start:end]
		case b[start] == '"':
			*field = b[start+1 : end-1]
		case b[start] != 'n': // null leaves the field as it is
			return 0, errEnvelopeNotString
		}
		return end, nil
	})
	if err != nil {
		return s, err
	}
	if skipSpace(b, end) != len(b) {
		return s, errEnvelopeSyntax // something after the object
	}
	return s, nil
}

// scanObject checks the JSON object that starts at b[i] and returns the index after it. For each member it calls
// value with the member's name (the contents of a JSON string that scanString accepted; escaped reports a backslash
// in it) and the index where the member's value starts; value checks that value and returns the index after it.
// scanObject fails with errEnvelopeSyntax when b[i] does not start an object or the object is malformed, and with
// value's error. It allocates nothing, and it does not keep value: a closure costs its caller no memory either.
func scanObject(b []byte, i int, value func(name []byte, escaped bool, start int) (end int, err error)) (int, error) {
	if i >= len(b) || b[i] != '{' {
		return 0, errEnvelopeSyntax
	}
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return i + 1, nil
	}
	for {
		nameEnd, escaped, ok := scanString(b, i)
		if !ok {
			return 0, errEnvelopeSyntax
		}
		start, ok := skipColon(b, nameEnd)
		if !ok {
			return 0, errEnvelopeSyntax
		}
		end, err := value(b[i+1:nameEnd-1], escaped, start)
		if err != nil {
			return 0, err
		}
		// A comma and the next member, or the end of the object.
		i = skipSpace(b, end)
		if i >= len(b) {
			return 0, errEnvelopeSyntax
		}
		if b[i] == '}' {
			return i + 1, nil
		}
		if b[i] != ',' {
			return 0, errEnvelopeSyntax
		}
		i = skipSpace(b, i+1)
	}
}

// envelopeStringBuf holds the longest string that the envelope accepts, a type, while unescapeKey decodes it:
// unescapeKey wants room for one more rune before each step, so it fails only for a longer string.
type envelopeStringBuf [maxTypeLen + utf8.UTFMax]byte

// envelopeString decodes raw, the contents of a JSON string that scanString accepted, and returns the text: raw
// itself, or a part of buf when raw has escapes. ok is false when the text is longer than maxLen bytes (at most
// maxTypeLen). It allocates nothing.
func envelopeString(raw []byte, buf *envelopeStringBuf, maxLen int) (text []byte, ok bool) {
	if bytes.IndexByte(raw, '\\') >= 0 {
		if raw, ok = unescapeKey(raw, buf[:]); !ok {
			return nil, false
		}
	}
	return raw, len(raw) <= maxLen
}

// validTokenBytes is validToken for the text of an id that is not empty and at most MaxIDLen bytes long.
func validTokenBytes(text []byte) bool {
	for _, c := range text {
		if !isTokenByte(c) {
			return false
		}
	}
	return true
}

// skipValue checks the JSON value that starts at b[i], with depth containers open around it, and returns the index
// after it. It fails when the value is not valid JSON (errEnvelopeSyntax) or nests deeper than maxDepth levels in
// all (errEnvelopeDepth). With maxName > 0 it also fails (errEnvelopeSyntax) when an object in the value has a
// member name of more than maxName bytes between its quotes.
func skipValue(b []byte, i, depth, maxName int) (int, error) {
	var isObject [maxDepth]bool // by depth: the open container is an object, not an array
	outer := depth
	for {
		// A value starts at b[i].
		if i >= len(b) {
			return 0, errEnvelopeSyntax
		}
		ok := true
		switch c := b[i]; c {
		case '{', '[':
			if depth == maxDepth {
				return 0, errEnvelopeDepth
			}
			closing := byte(']')
			if c == '{' {
				closing = '}'
			}
			isObject[depth] = c == '{'
			depth++
			i = skipSpace(b, i+1)
			if i < len(b) && b[i] == closing { // an empty container is a complete value
				depth--
				i++
				break
			}
			if c == '{' {
				if i, ok = skipName(b, i, maxName); !ok {
					return 0, errEnvelopeSyntax
				}
			}
			continue // the container's first value starts at b[i]
		case '"':
			i, _, ok = scanString(b, i)
		case 't':
			i, ok = skipLiteral(b, i, "true")
		case 'f':
			i, ok = skipLiteral(b, i, "false")
		case 'n':
			i, ok = skipLiteral(b, i, "null")
		default:
			i, ok = skipNumber(b, i)
		}
		if !ok {
			return 0, errEnvelopeSyntax
		}
		// A value ended before b[i]. Close the containers that end with it, up to the next value or the end of the
		// value this call checks.
		for more := false; !more; {
			if depth == outer {
				return i, nil
			}
			i = skipSpace(b, i)
			if i >= len(b) {
				return 0, errEnvelopeSyntax
			}
			object := isObject[depth-1]
			switch c := b[i]; {
			case c == ',':
				i = skipSpace(b, i+1)
				if object {
					if i, ok = skipName(b, i, maxName); !ok {
						return 0, errEnvelopeSyntax
					}
				}
				more = true
			case c == '}' && object, c == ']' && !object:
				depth--
				i++
			default:
				return 0, errEnvelopeSyntax
			}
		}
	}
}

// validPayload is what Decode asks of a payload b (valid UTF-8) before encoding/json reads it, found out without
// allocating:
//   - b is one JSON value, nested at most maxDepth levels deep, with nothing but whitespace around it: what
//     encoding/json accepts;
//   - no object in it has a member name of more than maxKeyLen bytes.
//
// raw, when not nil, names the member of the payload object that the payload's type keeps as the bytes of the
// message (rawMember). The second rule does not hold inside the value of that member: encoding/json reads no name
// in there. A key names the member the way encoding/json matches it with the field (keyIs).
func validPayload(b, raw []byte) bool {
	i := skipSpace(b, 0)
	var end int
	var err error
	if raw != nil && i < len(b) && b[i] == '{' {
		end, err = scanObject(b, i, func(name []byte, escaped bool, start int) (int, error) {
			switch {
			case len(name) > maxKeyLen:
				return 0, errEnvelopeSyntax
			case keyIs(name, escaped, raw):
				return skipValue(b, start, 1, 0)
			}
			return skipValue(b, start, 1, maxKeyLen)
		})
	} else {
		end, err = skipValue(b, i, 0, maxKeyLen)
	}
	return err == nil && skipSpace(b, end) == len(b)
}

// keyIs reports whether key, the contents of a JSON string that scanString accepted (escaped reports a backslash in
// it), names the member name the way encoding/json matches a key with a struct's field: equal under Unicode case
// folding (bytes.EqualFold), after unescaping.
func keyIs(key []byte, escaped bool, name []byte) bool {
	if escaped {
		// Unescaped into a buffer on the stack, as in limitNode.child: a key that doesn't fit is longer than a
		// member's name, even folded.
		var buf [64]byte
		var ok bool
		if key, ok = unescapeKey(key, buf[:]); !ok {
			return false
		}
	}
	return bytes.EqualFold(key, name)
}

// skipSpace returns the index of the first byte of b at or after i that is not JSON whitespace.
func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipName checks an object member's name at b[i] and the colon after it, and returns the index where the member's
// value starts. With maxName > 0, a name of more than maxName bytes between its quotes fails the check.
func skipName(b []byte, i, maxName int) (int, bool) {
	end, _, ok := scanString(b, i)
	if !ok || maxName > 0 && end-i-2 > maxName {
		return 0, false
	}
	return skipColon(b, end)
}

// skipColon skips the colon between a member's name, which ended before b[i], and its value, with the whitespace
// around it.
func skipColon(b []byte, i int) (int, bool) {
	i = skipSpace(b, i)
	if i >= len(b) || b[i] != ':' {
		return 0, false
	}
	return skipSpace(b, i+1), true
}

// scanString checks the JSON string that starts at b[i] and returns the index after its closing quote; escaped
// reports a backslash in it. The contents are b[i+1 : end-1]. b is valid UTF-8, so the string is too. As in
// encoding/json, a \u escape may be half of a surrogate pair on its own (it decodes to U+FFFD).
func scanString(b []byte, i int) (end int, escaped, ok bool) {
	if i >= len(b) || b[i] != '"' {
		return 0, false, false
	}
	for i++; i < len(b); i++ {
		switch c := b[i]; {
		case c == '"':
			return i + 1, escaped, true
		case c < ' ':
			return 0, false, false
		case c == '\\':
			escaped = true
			i++
			if i >= len(b) {
				return 0, false, false
			}
			switch b[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if getu4(b[i-1:]) < 0 {
					return 0, false, false
				}
				i += 4
			default:
				return 0, false, false
			}
		}
	}
	return 0, false, false // no closing quote
}

// skipLiteral checks that the literal lit (true, false or null) is at b[i] and returns the index after it.
func skipLiteral(b []byte, i int, lit string) (int, bool) {
	if len(b)-i < len(lit) || string(b[i:i+len(lit)]) != lit {
		return 0, false
	}
	return i + len(lit), true
}

// skipNumber checks the JSON number that starts at b[i] and returns the index after it. What follows the number is
// the caller's to check: 01 is the number 0 and then a 1 where a comma or the container's end must be.
func skipNumber(b []byte, i int) (int, bool) {
	if i < len(b) && b[i] == '-' {
		i++
	}
	switch {
	case i >= len(b):
		return 0, false
	case b[i] == '0':
		i++
	case '1' <= b[i] && b[i] <= '9':
		i = skipDigits(b, i)
	default:
		return 0, false
	}
	if i < len(b) && b[i] == '.' {
		end := skipDigits(b, i+1)
		if end == i+1 {
			return 0, false
		}
		i = end
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		end := skipDigits(b, i)
		if end == i {
			return 0, false
		}
		i = end
	}
	return i, true
}

// skipDigits returns the index of the first byte of b at or after i that is not a decimal digit.
func skipDigits(b []byte, i int) int {
	for i < len(b) && '0' <= b[i] && b[i] <= '9' {
		i++
	}
	return i
}
