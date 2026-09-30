package logx

import (
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secretRaw = "hunter2"

// verbs is every fmt verb that reaches a Formatter: all letters except %T (the type name, printed by fmt itself),
// and %p and %w, which fmt prints raw for these types and go vet's printf check rejects (see Secret).
func verbs() []string {
	var out []string
	for _, r := range "abcdefghijklmnoqrstuvxyzABCDEFGHIJKLMNOPQRSUVWXYZ" {
		out = append(out, string(r))
	}
	return out
}

// flagForms are flag, width and precision combinations to try with each verb.
var flagForms = []string{"", "+", "#", " ", "-", "0", "10", "-10", ".3", "+#10.3"}

func assertRedacted(t *testing.T, what, got string) {
	t.Helper()
	if strings.Contains(got, secretRaw) || strings.Contains(got, "68756e74657232") || strings.Contains(got, "104 117") {
		t.Errorf("%s leaked: %q", what, got)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("%s = %q, want %s", what, got, redacted)
	}
}

func TestSecretEveryVerb(t *testing.T) {
	values := map[string]any{
		"Secret":       Secret(secretRaw),
		"*Secret":      new(Secret(secretRaw)),
		"SecretBytes":  SecretBytes(secretRaw),
		"*SecretBytes": new(SecretBytes(secretRaw)),
	}
	for name, v := range values {
		for _, verb := range verbs() {
			for _, flags := range flagForms {
				format := "%" + flags + verb
				assertRedacted(t, fmt.Sprintf("%s with %s", name, format), fmt.Sprintf(format, v))
			}
		}
		if got := fmt.Sprintf("%T", v); strings.Contains(got, secretRaw) || !strings.Contains(got, "logx.Secret") {
			t.Errorf("%%T of %s = %q", name, got)
		}
	}
}

func TestSecretPrintFunctions(t *testing.T) {
	s := Secret(secretRaw)
	b := SecretBytes(secretRaw)
	type exported struct {
		Name  string
		Token Secret
		Key   SecretBytes
	}
	noVerbs := []string{"x"}[0] // not a constant, so vet and staticcheck don't flag the extra argument this case is about
	checks := map[string]string{
		"Sprint":            fmt.Sprint(s, b),
		"Sprintln":          fmt.Sprintln(s, b),
		"Errorf":            fmt.Errorf("login with %v failed", s).Error(),
		"extra argument":    fmt.Sprintf(noVerbs, s), // fmt's %!(EXTRA …) path
		"slice":             fmt.Sprintf("%v %+v %#v", []Secret{s}, []Secret{s}, []Secret{s}),
		"map value":         fmt.Sprintf("%v", map[string]Secret{"k": s}),
		"map key":           fmt.Sprintf("%v", map[Secret]int{s: 1}),
		"struct %v":         fmt.Sprintf("%v", exported{"n", s, b}),
		"struct %+v":        fmt.Sprintf("%+v", exported{"n", s, b}),
		"struct %#v":        fmt.Sprintf("%#v", exported{"n", s, b}),
		"pointer to struct": fmt.Sprintf("%+v", &exported{"n", s, b}),
		"String":            s.String() + b.String(),
		"GoString":          s.GoString() + b.GoString(),
	}
	for what, got := range checks {
		assertRedacted(t, what, got)
	}
}

func TestSecretMarshal(t *testing.T) {
	s := Secret(secretRaw)
	b := SecretBytes(secretRaw)
	type body struct {
		Token Secret      `json:"token"`
		Key   SecretBytes `json:"key"`
		Ptr   *Secret     `json:"ptr"`
	}
	got, err := json.Marshal(body{Token: s, Key: b, Ptr: &s})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"token":"[redacted]","key":"[redacted]","ptr":"[redacted]"}`; string(got) != want {
		t.Errorf("json.Marshal = %s, want %s", got, want)
	}
	// Not checked: a Secret used as a JSON map key, which encoding/json writes raw (see Secret).

	for name, m := range map[string]encoding.TextMarshaler{"Secret": s, "SecretBytes": b} {
		text, err := m.MarshalText()
		if err != nil || string(text) != redacted {
			t.Errorf("%s.MarshalText() = %q, %v", name, text, err)
		}
	}
	for name, v := range map[string]slog.LogValuer{"Secret": s, "SecretBytes": b} {
		if got := v.LogValue(); got.Kind() != slog.KindString || got.String() != redacted {
			t.Errorf("%s.LogValue() = %v", name, got)
		}
	}
}

func TestSecretReveal(t *testing.T) {
	if got := Secret(secretRaw).Reveal(); got != secretRaw {
		t.Errorf("Secret.Reveal() = %q", got)
	}
	if got := SecretBytes(secretRaw).Reveal(); string(got) != secretRaw {
		t.Errorf("SecretBytes.Reveal() = %q", got)
	}
	// A Secret decodes from JSON like a string, so request bodies can use it directly.
	var in struct{ Token Secret }
	if err := json.Unmarshal([]byte(`{"Token":"abc"}`), &in); err != nil || in.Token.Reveal() != "abc" {
		t.Errorf("json.Unmarshal: %q, %v", in.Token.Reveal(), err)
	}
}
