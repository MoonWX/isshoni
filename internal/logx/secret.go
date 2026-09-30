package logx

import (
	"fmt"
	"io"
	"log/slog"
)

// redacted replaces every secret in logs, fmt output and serialized forms.
const redacted = "[redacted]"

// Secret holds a value that must never be logged or serialized by accident: tokens, passwords, keys, push endpoints.
// fmt (every verb that reaches a Formatter), slog (text and JSON handlers), encoding/json and every
// encoding.TextMarshaler user print "[redacted]". Use Reveal to send or store the real value.
//
// Gaps that come from Go itself, because a Secret is a string underneath:
//   - fmt prints the raw operand for %p and %w, which it never passes to a Formatter for a string. go vet's printf
//     check (part of go test and CI) rejects both verbs for a Secret.
//   - fmt can't call methods on unexported struct fields, so a Secret in an unexported field prints raw. Keep
//     Secret fields exported, or don't print the struct.
//   - encoding/json writes a string-kind map key as it is, so never use a Secret as a map key.
type Secret string

// Reveal returns the real value.
func (s Secret) Reveal() string { return string(s) }

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]", for %#v.
func (Secret) GoString() string { return redacted }

// Format writes "[redacted]" for every verb.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue returns "[redacted]".
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns the JSON string "[redacted]"; use Reveal to send a real value.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText returns "[redacted]".
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// SecretBytes is the []byte form of Secret, for keys and other binary secrets. It has the same methods. Of Secret's
// gaps only %w and unexported fields apply: %p prints the slice's address, and a slice can't be a map key.
type SecretBytes []byte

// Reveal returns the real value. The slice is shared, not copied.
func (s SecretBytes) Reveal() []byte { return []byte(s) }

// String returns "[redacted]".
func (SecretBytes) String() string { return redacted }

// GoString returns "[redacted]", for %#v.
func (SecretBytes) GoString() string { return redacted }

// Format writes "[redacted]" for every verb.
func (SecretBytes) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue returns "[redacted]".
func (SecretBytes) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns the JSON string "[redacted]"; use Reveal to send a real value.
func (SecretBytes) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText returns "[redacted]".
func (SecretBytes) MarshalText() ([]byte, error) { return []byte(redacted), nil }
