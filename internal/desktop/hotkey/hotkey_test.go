package hotkey

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDefaults(t *testing.T) {
	if got := DefaultPause.String(); got != "Ctrl+Alt+Shift+F9" {
		t.Errorf("DefaultPause = %q", got)
	}
	if got := DefaultStop.String(); got != "Ctrl+Alt+Shift+F10" {
		t.Errorf("DefaultStop = %q", got)
	}
	for _, c := range []Combo{DefaultPause, DefaultStop} {
		if err := c.Validate(); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	if got := DefaultPause.Modifiers(); got != modControl|modAlt|modShift {
		t.Errorf("Modifiers = %#x", got)
	}
	if got := DefaultPause.VirtualKey(); got != 0x78 { // VK_F9
		t.Errorf("VirtualKey(F9) = %#x", got)
	}
	if got := DefaultStop.VirtualKey(); got != 0x79 { // VK_F10
		t.Errorf("VirtualKey(F10) = %#x", got)
	}
}

func TestParseCanonical(t *testing.T) {
	for in, want := range map[string]Combo{
		"Ctrl+Alt+Shift+F9":            DefaultPause,
		"ctrl+alt+shift+f10":           DefaultStop,
		"Shift + Alt + Ctrl + F9":      DefaultPause, // any order, spaces around the signs
		"Control+Alt+P":                {Ctrl: true, Alt: true, Key: "P"},
		"Ctrl+Shift+7":                 {Ctrl: true, Shift: true, Key: "7"},
		"Alt+F4":                       {Alt: true, Key: "F4"}, // an F-key with one modifier
		"Ctrl+F24":                     {Ctrl: true, Key: "F24"},
		"Shift+F1":                     {Shift: true, Key: "F1"},
		"Ctrl+Alt+pageup":              {Ctrl: true, Alt: true, Key: "PageUp"},
		"Ctrl+Alt+PageDown":            {Ctrl: true, Alt: true, Key: "PageDown"},
		"Ctrl+Shift+SCROLLLOCK":        {Ctrl: true, Shift: true, Key: "ScrollLock"},
		"Alt+Shift+Pause":              {Alt: true, Shift: true, Key: "Pause"},
		"Ctrl+Alt+Insert":              {Ctrl: true, Alt: true, Key: "Insert"},
		"Ctrl+Alt+Home":                {Ctrl: true, Alt: true, Key: "Home"},
		"Ctrl+Alt+End":                 {Ctrl: true, Alt: true, Key: "End"},
		"Ctrl+Alt+num0":                {Ctrl: true, Alt: true, Key: "Num0"},
		"Ctrl+Alt+Num9":                {Ctrl: true, Alt: true, Key: "Num9"},
		"Ctrl+Alt+z":                   {Ctrl: true, Alt: true, Key: "Z"},
		" Ctrl + Alt + Shift + F9 ":    DefaultPause,
		"Ctrl+Alt+Shift+ScrollLock":    {Ctrl: true, Alt: true, Shift: true, Key: "ScrollLock"},
		"CONTROL+ALT+SHIFT+F12":        {Ctrl: true, Alt: true, Shift: true, Key: "F12"},
		"Shift+Ctrl+Alt+Num5":          {Ctrl: true, Alt: true, Shift: true, Key: "Num5"},
		"Alt+Shift+0":                  {Alt: true, Shift: true, Key: "0"},
		"ctrl+shift+a":                 {Ctrl: true, Shift: true, Key: "A"},
		"Alt+Ctrl+End":                 {Ctrl: true, Alt: true, Key: "End"},
		"Ctrl + Alt + Shift + PageUp ": {Ctrl: true, Alt: true, Shift: true, Key: "PageUp"},
	} {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", in, got, want)
		}
		again, err := Parse(got.String())
		if err != nil || again != got {
			t.Errorf("round trip of %q: %+v, %v", got.String(), again, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"F9",              // no modifier
		"Ctrl+A",          // one modifier with a key that isn't an F-key
		"Shift+7",         // the same
		"Ctrl+Alt",        // no key
		"Ctrl+Alt+",       // empty part
		"+Ctrl+Alt+F9",    // empty part
		"Ctrl++F9",        // empty part
		"Win+F9",          // the Windows key
		"Ctrl+Win+F9",     // the Windows key
		"Ctrl+Meta+F9",    // the Windows key by another name
		"Ctrl+Super+A",    // the same
		"Ctrl+Ctrl+F9",    // a modifier twice
		"Ctrl+Alt+Alt+F9", // a modifier twice
		"Ctrl+F9+Alt",     // the key not last
		"Ctrl+Alt+F25",    // no such F-key
		"Ctrl+Alt+F0",     // no such F-key
		"Ctrl+Alt+F09",    // a leading zero
		"Ctrl+Alt+Num10",  // no such number-pad key
		"Ctrl+Alt+Esc",    // not in the list
		"Ctrl+Alt+Space",  // not in the list
		"Ctrl+Alt+Tab",    // not in the list
		"Ctrl+Alt+ß",      // not ASCII
		"Ctrl+Alt+AB",     // two letters
		"Ctrl+Alt+F-1",    // a sign
		"Ctrl+Alt+F+9",    // the key not last
		"Ctrl\tAlt+F9",    // a tab is not a separator
		"Ctrl+Alt+Shift+F9 0123456789012345678901234567890123456789012345678",
	} {
		c, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", in, c)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q): %v does not wrap ErrInvalid", in, err)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []Combo{
		{},
		{Ctrl: true, Alt: true},                 // no key
		{Ctrl: true, Alt: true, Key: "f9"},      // not the canonical spelling
		{Ctrl: true, Alt: true, Key: "Escape"},  // unknown key
		{Ctrl: true, Key: "A"},                  // one modifier, not an F-key
		{Key: "F9"},                             // no modifier
		{Ctrl: true, Alt: true, Key: "pageup"},  // not the canonical spelling
		{Ctrl: true, Shift: true, Key: "Num 1"}, // unknown key
		{Alt: true, Shift: true, Key: "F25"},    // no such F-key
		{Ctrl: true, Alt: true, Key: "F9+Ctrl"}, // garbage
		{Ctrl: true, Alt: true, Shift: true, Key: "+"},
	} {
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", c, err)
		}
	}
}

func TestVirtualKeys(t *testing.T) {
	for in, want := range map[string]uint16{
		"Ctrl+Alt+A":          0x41,
		"Ctrl+Alt+Z":          0x5A,
		"Ctrl+Alt+0":          0x30,
		"Ctrl+Alt+9":          0x39,
		"Ctrl+F1":             0x70,
		"Ctrl+F24":            0x87,
		"Ctrl+Alt+Num0":       0x60,
		"Ctrl+Alt+Num9":       0x69,
		"Ctrl+Alt+Pause":      0x13,
		"Ctrl+Alt+ScrollLock": 0x91,
		"Ctrl+Alt+Insert":     0x2D,
		"Ctrl+Alt+Home":       0x24,
		"Ctrl+Alt+End":        0x23,
		"Ctrl+Alt+PageUp":     0x21,
		"Ctrl+Alt+PageDown":   0x22,
	} {
		if got := MustParse(in).VirtualKey(); got != want {
			t.Errorf("%s: VirtualKey = %#x, want %#x", in, got, want)
		}
	}
	if got := (Combo{Ctrl: true, Alt: true, Key: "nope"}).VirtualKey(); got != 0 {
		t.Errorf("unknown key: VirtualKey = %#x", got)
	}
	if got := (Combo{Alt: true, Key: "F4"}).Modifiers(); got != modAlt {
		t.Errorf("Alt: Modifiers = %#x", got)
	}
	if got := (Combo{Ctrl: true, Shift: true, Key: "F4"}).Modifiers(); got != modControl|modShift {
		t.Errorf("Ctrl+Shift: Modifiers = %#x", got)
	}
}

func TestText(t *testing.T) {
	var v struct {
		Pause Combo `json:"pause"`
	}
	if err := json.Unmarshal([]byte(`{"pause":"shift+ctrl+alt+f9"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Pause != DefaultPause {
		t.Errorf("unmarshal = %+v", v.Pause)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"pause":"Ctrl+Alt+Shift+F9"}` {
		t.Errorf("marshal = %s", out)
	}
	if err := json.Unmarshal([]byte(`{"pause":"Win+F9"}`), &v); !errors.Is(err, ErrInvalid) {
		t.Errorf("unmarshal Win+F9: %v", err)
	}
	if _, err := json.Marshal(struct{ C Combo }{Combo{Key: "F9"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("marshal an invalid combination: %v", err)
	}
	if got := (Combo{}).String(); got != "" {
		t.Errorf("zero String = %q", got)
	}
	if got := (Combo{Ctrl: true}).String(); got != "Ctrl" {
		t.Errorf("Ctrl alone String = %q", got)
	}
}

func TestMustParsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParse of an invalid combination did not panic")
		}
	}()
	MustParse("F9")
}

// FuzzParse: Parse never panics; what it accepts is valid, formats canonically and parses back to itself; what it
// refuses wraps ErrInvalid.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"Ctrl+Alt+Shift+F9", "Ctrl+Alt+Shift+F10", "alt+f4", "Ctrl+Win+A", "Ctrl++F9",
		"Shift + Ctrl + Num7", "Ctrl+Alt+PageUp", "Ctrl+Alt+F25", "", "+", "Control+Shift+ScrollLock"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Parse(s)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q): %v does not wrap ErrInvalid", s, err)
			}
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Parse(%q) = %+v, which is invalid: %v", s, c, err)
		}
		text := c.String()
		again, err := Parse(text)
		if err != nil || again != c {
			t.Fatalf("Parse(%q) = %+v; Parse(%q) = %+v, %v", s, c, text, again, err)
		}
		if again.String() != text {
			t.Fatalf("String is not canonical: %q then %q", text, again.String())
		}
		if c.VirtualKey() == 0 || c.Modifiers() == 0 {
			t.Fatalf("Parse(%q) = %+v without a key code or modifiers", s, c)
		}
	})
}
