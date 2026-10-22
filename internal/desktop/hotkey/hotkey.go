// Package hotkey parses and formats the desktop app's global hotkey combinations (docs/m2/03-desktop-app.md §11.2,
// "03" below): the Pause and Stop hotkeys of settings.json, the Settings page's hotkey field and what
// internal/desktop/winos registers with RegisterHotKey. It is pure Go with no OS code, so it builds and tests on
// every OS.
//
// The text form is the modifiers Ctrl, Alt and Shift, in that order, then the key, joined by "+":
// "Ctrl+Alt+Shift+F9". Keys are A–Z, 0–9, F1–F24, Pause, ScrollLock, Insert, Home, End, PageUp, PageDown and
// Num0–Num9. A combination needs two modifiers, or an F-key with at least one; the Windows key is never allowed
// (Windows owns most Win combinations). Parse reads names in any case and modifiers in any order, with spaces around
// the "+" signs; String always writes the canonical form, and the two round-trip.
package hotkey

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Combo is one combination: up to three modifiers and one key.
type Combo struct {
	Ctrl, Alt, Shift bool
	// Key is the key's canonical name, as String writes it: "F9", "A", "7", "PageUp", "Num0".
	Key string
}

// The default combinations (03 D9): F-keys, because no keyboard layout types a character on them, unlike a letter
// with Ctrl+Alt, which Windows also delivers for AltGr.
var (
	DefaultPause = Combo{Ctrl: true, Alt: true, Shift: true, Key: "F9"}  // Pause/Resume
	DefaultStop  = Combo{Ctrl: true, Alt: true, Shift: true, Key: "F10"} // Stop, no confirmation
)

// ErrInvalid is wrapped by every error of Parse and Validate; the wrapping error says which rule failed. The SPA's
// hotkey field turns it into invalid_argument (03 §6.2).
var ErrInvalid = errors.New("hotkey: invalid combination")

// maxLen bounds the text Parse reads: the longest canonical combination ("Ctrl+Alt+Shift+ScrollLock") is 25 bytes,
// so this leaves room for spaces and the long modifier spelling "Control".
const maxLen = 64

// Windows modifier flags of RegisterHotKey (winuser.h).
const (
	modAlt     = 0x0001 // MOD_ALT
	modControl = 0x0002 // MOD_CONTROL
	modShift   = 0x0004 // MOD_SHIFT
)

// namedKeys are the keys with a name and their virtual-key codes (winuser.h VK_*). The letters, digits, F-keys and
// number-pad digits are computed in keyByName.
var namedKeys = map[string]uint16{
	"Pause":      0x13, // VK_PAUSE
	"ScrollLock": 0x91, // VK_SCROLL
	"Insert":     0x2D, // VK_INSERT
	"Home":       0x24, // VK_HOME
	"End":        0x23, // VK_END
	"PageUp":     0x21, // VK_PRIOR
	"PageDown":   0x22, // VK_NEXT
}

// winNames are the spellings of the Windows key that Parse refuses with a message of its own.
var winNames = []string{"win", "windows", "meta", "super", "cmd", "command"}

// Parse reads a combination. Names are compared without regard to case; spaces around each part are ignored;
// modifiers may come in any order but each only once, and the key comes last. The result is valid (Validate).
func Parse(s string) (Combo, error) {
	if len(s) > maxLen {
		return Combo{}, invalid("longer than %d bytes", maxLen)
	}
	parts := strings.Split(s, "+")
	var c Combo
	for i, raw := range parts {
		p := strings.Trim(raw, " ")
		if p == "" {
			return Combo{}, invalid("an empty part")
		}
		last := i == len(parts)-1
		switch lower := strings.ToLower(p); {
		case lower == "ctrl" || lower == "control":
			if c.Ctrl {
				return Combo{}, invalid("Ctrl twice")
			}
			c.Ctrl = true
		case lower == "alt":
			if c.Alt {
				return Combo{}, invalid("Alt twice")
			}
			c.Alt = true
		case lower == "shift":
			if c.Shift {
				return Combo{}, invalid("Shift twice")
			}
			c.Shift = true
		case isWin(lower):
			return Combo{}, invalid("the Windows key is not allowed")
		case !last:
			return Combo{}, invalid("the key must come last")
		default:
			name, _, ok := keyByName(p)
			if !ok {
				return Combo{}, invalid("not a key this app can register")
			}
			c.Key = name
		}
		if last && c.Key == "" {
			return Combo{}, invalid("no key after the modifiers")
		}
	}
	if err := c.Validate(); err != nil {
		return Combo{}, err
	}
	return c, nil
}

// MustParse is Parse for combinations written in code; it panics on an invalid one.
func MustParse(s string) Combo {
	c, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return c
}

// String returns the canonical text form, "Ctrl+Alt+Shift+F9". The zero Combo gives "".
func (c Combo) String() string {
	var b strings.Builder
	for _, m := range []struct {
		on   bool
		name string
	}{{c.Ctrl, "Ctrl"}, {c.Alt, "Alt"}, {c.Shift, "Shift"}} {
		if m.on {
			b.WriteString(m.name)
			b.WriteByte('+')
		}
	}
	b.WriteString(c.Key)
	if c.Key == "" {
		return strings.TrimSuffix(b.String(), "+")
	}
	return b.String()
}

// MarshalText writes String (settings.json keeps combinations as text).
func (c Combo) MarshalText() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return []byte(c.String()), nil
}

// UnmarshalText reads a combination with Parse.
func (c *Combo) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// Validate reports whether c can be registered: a known key in its canonical spelling, and two modifiers or an
// F-key with at least one (03 §11.2).
func (c Combo) Validate() error {
	name, _, ok := keyByName(c.Key)
	if !ok || name != c.Key {
		return invalid("not a key this app can register")
	}
	mods := 0
	for _, on := range []bool{c.Ctrl, c.Alt, c.Shift} {
		if on {
			mods++
		}
	}
	switch {
	case mods >= 2:
	case mods == 1 && isFKey(c.Key):
	default:
		return invalid("needs two modifiers, or an F-key with one")
	}
	return nil
}

// Modifiers returns RegisterHotKey's fsModifiers for c: MOD_CONTROL, MOD_ALT and MOD_SHIFT. The caller adds
// MOD_NOREPEAT.
func (c Combo) Modifiers() uint32 {
	var m uint32
	if c.Ctrl {
		m |= modControl
	}
	if c.Alt {
		m |= modAlt
	}
	if c.Shift {
		m |= modShift
	}
	return m
}

// VirtualKey returns the key's Windows virtual-key code (VK_*), or 0 for a key c doesn't have.
func (c Combo) VirtualKey() uint16 {
	_, vk, ok := keyByName(c.Key)
	if !ok {
		return 0
	}
	return vk
}

// keyByName finds a key by name, in any case, and returns its canonical name and virtual-key code.
func keyByName(s string) (name string, vk uint16, ok bool) {
	switch {
	case len(s) == 1 && s[0] >= 'a' && s[0] <= 'z':
		return strings.ToUpper(s), uint16(s[0]-'a') + 'A', true
	case len(s) == 1 && s[0] >= 'A' && s[0] <= 'Z':
		return s, uint16(s[0]), true
	case len(s) == 1 && s[0] >= '0' && s[0] <= '9':
		return s, uint16(s[0]), true // VK_0–VK_9 are the ASCII digits
	}
	lower := strings.ToLower(s)
	if n, ok := numberAfter(lower, "f", 1, 24); ok {
		return "F" + strconv.Itoa(n), uint16(0x70 + n - 1), true //nolint:gosec // G115: n is 1–24; VK_F1 = 0x70 … VK_F24 = 0x87
	}
	if n, ok := numberAfter(lower, "num", 0, 9); ok {
		return "Num" + strconv.Itoa(n), uint16(0x60 + n), true //nolint:gosec // G115: n is 0–9; VK_NUMPAD0 = 0x60 …
	}
	for k, v := range namedKeys {
		if strings.EqualFold(k, s) {
			return k, v, true
		}
	}
	return "", 0, false
}

// numberAfter reads prefix followed by a decimal number in [lo, hi] without a leading zero or sign.
func numberAfter(s, prefix string, lo, hi int) (int, bool) {
	digits, ok := strings.CutPrefix(s, prefix)
	if !ok || digits == "" || len(digits) > 2 || (len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

// isFKey reports whether a canonical key name is F1–F24.
func isFKey(key string) bool {
	_, ok := numberAfter(key, "F", 1, 24)
	return ok
}

func isWin(lower string) bool {
	for _, w := range winNames {
		if lower == w {
			return true
		}
	}
	return false
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
