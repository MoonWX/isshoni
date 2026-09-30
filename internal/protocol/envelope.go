package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MessageType is the envelope's type: dotted lowercase noun.verb (room.join), or a bare word for protocol-level
// messages. One constant per row of the message catalog (01 §7).
type MessageType string

const (
	MessageTypeHello           MessageType = "hello"
	MessageTypeWelcome         MessageType = "welcome"
	MessageTypeOK              MessageType = "ok"
	MessageTypeError           MessageType = "error"
	MessageTypePing            MessageType = "ping"
	MessageTypePong            MessageType = "pong"
	MessageTypeRoomJoin        MessageType = "room.join"
	MessageTypeRoomLeave       MessageType = "room.leave"
	MessageTypeRoomState       MessageType = "room.state"
	MessageTypeRoomEvent       MessageType = "room.event"
	MessageTypeShareStart      MessageType = "share.start"
	MessageTypeShareUpdate     MessageType = "share.update"
	MessageTypeShareStop       MessageType = "share.stop"
	MessageTypePCOffer         MessageType = "pc.offer"
	MessageTypePCAnswer        MessageType = "pc.answer"
	MessageTypePCICE           MessageType = "pc.ice"
	MessageTypePCRestart       MessageType = "pc.restart"
	MessageTypePCClose         MessageType = "pc.close"
	MessageTypeSubscribeUpdate MessageType = "subscribe.update"
	MessageTypeSubscribeStatus MessageType = "subscribe.status"
	MessageTypeQualityHint     MessageType = "quality.hint"
	MessageTypeCapsUpdate      MessageType = "caps.update"
	MessageTypeStats           MessageType = "stats" // ClientStats from clients, ServerStats from the server
	MessageTypeStatsWatch      MessageType = "stats.watch"
	MessageTypeInvalidate      MessageType = "invalidate"
	MessageTypeServerShutdown  MessageType = "server.shutdown"
	MessageTypeAgentSend       MessageType = "agent.send"
	MessageTypeAgentRecv       MessageType = "agent.recv"
	MessageTypeUserConnections MessageType = "user.connections" // later (M2, feature user.connections)
)

// Envelope is every WebSocket message (01 §5).
type Envelope struct {
	Type MessageType     `json:"type"`
	ID   string          `json:"id,omitempty"`   // requests only: 1-32 chars [A-Za-z0-9_-], chosen by the requester
	Re   string          `json:"re,omitempty"`   // replies only: the id of the request this answers
	Data json.RawMessage `json:"data,omitempty"` // payload for Type; absent means {}
}

// Empty is the payload of requests and replies without fields.
type Empty struct{}

// ErrBadMessage is wrapped by every ParseEnvelope error; the hub answers it with bad_message and closes with 4400.
var ErrBadMessage = errors.New("protocol: bad message")

// maxTypeLen bounds Envelope.Type. The longest real type is 16 bytes; anything much longer is garbage, not a type from
// a newer peer.
const maxTypeLen = 64

// ParseEnvelope parses one frame. Errors wrap ErrBadMessage (-> bad_message): not valid UTF-8 (JSON and WebSocket
// text frames must be), not a JSON object, nested deeper than 32 levels, missing or empty type (or one longer than
// 64 bytes), id/re longer than 32 bytes or with characters outside [A-Za-z0-9_-], or data that is neither an object
// nor null. A data of null is treated as absent.
func ParseEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if !utf8.Valid(b) {
		// Checked first: encoding/json would replace each invalid byte with a 3-byte U+FFFD, which lets a message
		// allocate several times its size.
		return Envelope{}, fmt.Errorf("%w: not valid UTF-8", ErrBadMessage)
	}
	if err := scanJSON(b, nil); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	switch {
	case e.Type == "":
		return Envelope{}, fmt.Errorf("%w: missing type", ErrBadMessage)
	case len(e.Type) > maxTypeLen:
		return Envelope{}, fmt.Errorf("%w: type too long", ErrBadMessage)
	case e.ID != "" && !validToken(e.ID, MaxIDLen):
		return Envelope{}, fmt.Errorf("%w: invalid id", ErrBadMessage)
	case e.Re != "" && !validToken(e.Re, MaxIDLen):
		return Envelope{}, fmt.Errorf("%w: invalid re", ErrBadMessage)
	}
	switch {
	case len(e.Data) == 0:
	case bytes.Equal(e.Data, jsonNull):
		e.Data = nil
	case e.Data[0] != '{':
		return Envelope{}, fmt.Errorf("%w: data is not an object", ErrBadMessage)
	}
	return e, nil
}

// Decode unmarshals e.Data into T (unknown fields ignored; absent data decodes as the zero value) and runs T's
// Validate method if it has one.
//
// Errors are a *FieldError (the hub answers bad_request with params {field, reason}) or, where the spec prescribes
// another code, a *Error (RoomJoin: room_not_found; AgentSend: message_too_large). Before unmarshaling, Decode
// rejects data that is not valid UTF-8 or nested deeper than 32 levels, and arrays longer than the payload's limits
// (for example subs > 64), so a client->server message cannot make the decoder allocate much more than its own size.
func Decode[T any](e Envelope) (T, error) {
	var v T
	err := DecodeInto(e, &v)
	return v, err
}

// DecodeInto is Decode for a caller-supplied pointer, for code that picks the payload type at run time (the Go
// client's Request, or a Registry-driven dispatcher). v must be a non-nil pointer.
func DecodeInto(e Envelope, v any) error {
	if err := unmarshalData(e.Data, v); err != nil {
		return err
	}
	if val, ok := v.(validator); ok {
		return val.Validate()
	}
	return nil
}

// validator is implemented (on the pointer) by every client->server payload.
type validator interface {
	Validate() error
}

// arrayLimiter is implemented by payloads whose arrays have a count limit; scanJSON enforces the limits before
// encoding/json allocates the elements.
type arrayLimiter interface {
	arrayLimits() *limitNode
}

var jsonNull = []byte("null")

func unmarshalData(data json.RawMessage, v any) error {
	if len(data) == 0 || bytes.Equal(data, jsonNull) {
		return nil
	}
	if !utf8.Valid(data) {
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	var limits *limitNode
	if l, ok := v.(arrayLimiter); ok {
		limits = l.arrayLimits()
	}
	if err := scanJSON(data, limits); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return fe
		}
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	if err := json.Unmarshal(data, v); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return &FieldError{Field: ute.Field, Reason: FieldInvalid}
		}
		var ie *json.InvalidUnmarshalError
		if errors.As(err, &ie) {
			return fmt.Errorf("protocol: decode: %w", err) // a programming error, not the peer's
		}
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	return nil
}

// Marshal encodes one message; data may be nil (no data field). The payload types' MarshalJSON methods encode nil
// slices as [] and timestamps as RFC 3339 UTC with milliseconds, so the output never contains a null array.
func Marshal(t MessageType, id, re string, data any) ([]byte, error) {
	if t == "" {
		return nil, errors.New("protocol: marshal: empty message type")
	}
	e := Envelope{Type: t, ID: id, Re: re}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("protocol: marshal %s: %w", t, err)
		}
		if !bytes.Equal(b, jsonNull) {
			e.Data = b
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal %s: %w", t, err)
	}
	return b, nil
}
