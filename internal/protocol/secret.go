package protocol

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// Secret is a string that never appears in logs or fmt output; JSON encodes it verbatim (it is a plain string on
// the wire). String, GoString, Format (every verb) and LogValue all return "[redacted]"; Reveal returns the value.
//
// Log a Secret as its own attribute, never inside a payload struct: slog's JSON handler encodes structs with
// encoding/json, which keeps the value. (fmt and slog's text handler call Format on struct fields and redact it.)
type Secret string

const redacted = "[redacted]"

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format redacts the value for every verb, including %v, %+v, %#v, %s, %q and %x.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Format prints HelloAuth without its token, for every verb. HelloAuth is reached through a pointer (Hello.Auth), and
// fmt prints a pointee without calling its fields' methods when the verb does not suit a pointer (%s of a Hello
// prints "%!s(*protocol.HelloAuth=&{…})"), so the Secret alone could not redact it there.
func (a HelloAuth) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "{"+string(a.Scheme)+" "+redacted+"}")
}

// LogValue implements slog.LogValuer.
func (a HelloAuth) LogValue() slog.Value {
	return slog.GroupValue(slog.String("scheme", string(a.Scheme)), slog.String("token", redacted))
}

// SDP is a session description. In logs and fmt output it shows only its length ("[sdp 5123 B]"); JSON encodes it
// verbatim.
type SDP string

// Reveal returns the session description.
func (s SDP) Reveal() string { return string(s) }

func (s SDP) String() string   { return "[sdp " + strconv.Itoa(len(s)) + " B]" }
func (s SDP) GoString() string { return s.String() }

// Format prints only the length for every verb.
func (s SDP) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue implements slog.LogValuer.
func (s SDP) LogValue() slog.Value { return slog.StringValue(s.String()) }
