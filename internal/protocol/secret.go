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
// slog resolves a LogValuer only for an attribute's own value, and its JSON handler encodes any other struct with
// encoding/json, which keeps a nested Secret or SDP verbatim. So every payload struct that holds a Secret, an SDP or
// a candidate address (Hello, HelloAuth, Welcome, ICEServer, PCOffer, PCAnswer, PCICE) has its own LogValue that
// logs a redacted summary. A slice of them ([]ICEServer) is still encoded field by field: log such values one by one.
// (fmt and slog's text handler call Format on struct fields and redact them.)
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

// LogValue implements slog.LogValuer: the negotiation fields, the client's diagnostics and the role; the resume
// token and the bearer token are redacted.
func (h Hello) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Int("protocol", h.Protocol),
		slog.Int("minProtocol", h.MinProtocol),
		slog.Int("features", len(h.Features)),
		slog.Group("client", slog.String("kind", string(h.Client.Kind)), slog.String("os", string(h.Client.OS)),
			slog.String("version", h.Client.Version)),
		slog.String("role", string(h.Role)),
	}
	if h.ResumeToken != "" {
		attrs = append(attrs, slog.String("resumeToken", redacted))
	}
	if h.Auth != nil {
		attrs = append(attrs, slog.Group("auth", slog.String("scheme", string(h.Auth.Scheme))))
	}
	return slog.GroupValue(attrs...)
}

// LogValue implements slog.LogValuer: the connection, the room and the number of ICE servers; never the resume token,
// the ICE credentials or the user.
func (w Welcome) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("connectionId", w.ConnectionID),
		slog.Bool("resumed", w.Resumed),
		slog.String("roomId", w.RoomID),
		slog.Int("protocol", w.Protocol),
		slog.Int("iceServers", len(w.ICEServers)),
	)
}

// LogValue implements slog.LogValuer: the URLs only, never the username or the credential.
func (s ICEServer) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("urls", s.URLs))
}

// LogValue implements slog.LogValuer: the negotiation numbers, the SDP's length and the number of tracks.
func (o PCOffer) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("pc", string(o.PC)),
		slog.Uint64("gen", uint64(o.Gen)),
		slog.Uint64("neg", uint64(o.Neg)),
		slog.String("sdp", o.SDP.String()),
		slog.Int("tracks", len(o.Tracks)),
	)
}

// LogValue implements slog.LogValuer: the negotiation numbers and the SDP's length.
func (a PCAnswer) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("pc", string(a.PC)),
		slog.Uint64("gen", uint64(a.Gen)),
		slog.Uint64("neg", uint64(a.Neg)),
		slog.String("sdp", a.SDP.String()),
	)
}

// LogValue implements slog.LogValuer: pc, gen and whether a candidate is present, never its addresses.
func (c PCICE) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("pc", string(c.PC)),
		slog.Uint64("gen", uint64(c.Gen)),
		slog.Bool("candidate", c.Candidate != nil),
	)
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
