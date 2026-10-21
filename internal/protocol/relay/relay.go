// Package relay is the registry of the same-user relay's kinds and the payload of each kind that has one (M2 01
// §11.11). The relay itself is protocol's agent.send and agent.recv (01 §8.14): the server delivers a payload only to
// other connections of the sender's user and never parses it, so the clients that send and receive a kind agree on
// its payload here.
//
// The TypeScript types in web/src/protocol/relay.gen.ts are generated from this package by tygo (task gen). That file
// is the one TS source of the kinds and payloads: the desktop SPA's playback.stop sender, rooms/handoff.ts and the web
// tabs' playback.state sender import it (03 §12.2).
//
// A receiver decodes a payload with its kind's Decode function and ignores a payload that doesn't decode or breaks
// the kind's rules, and every kind it doesn't serve, unknown ones included. Relay payloads are never logged (01
// §11.12): a receiver's debug line names the kind only, and the errors of this package name fields, never values.
//
// The package imports only the standard library and internal/protocol.
package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Relay kinds: [a-z.]{1,32} (protocol.MaxAgentKindLen), dotted lower case. A kind's name is never reused for
// another payload. In M2 the Windows app's Go connection serves no kind: it only consumes playback.state (M2 01 D16).
// tygo turns this group into the TS union type Kind.
const (
	KindPlaybackStop  = "playback.stop"  // M2: desktop SPA (C1) → the user's full connections (toRole "full"), 03 §12.2
	KindPlaybackState = "playback.state" // M2: web tab → desktop app (to: the app's publisher connectionId from user.connections), 05 §5.9
	KindShareRequest  = "share.request"  // later (M4): web-initiated desktop or agent shares; no payload declared in M2
	KindShareStatus   = "share.status"   // later (M4)
	KindExclusionsGet = "exclusions.get" // reserved (M4): payload declared by M4's agent spec (D16)
	KindExclusionsSet = "exclusions.set" // reserved (M4)
)

// PlaybackStopVersion is PlaybackStop.V in M2. A receiver ignores a playback.stop with any other v.
const PlaybackStopVersion = 1

// PlaybackStop is the payload of playback.stop: "Now watching in the isshoni app" (03 §12.2). Sender: the desktop
// SPA (C1), with toRole "full", once each time the user brings the app into a room. Receiver rules
// (rooms/handoff.ts): v === 1, roomId equals the session's room, from is a desktop connection of the user in
// room.state; anything else is ignored. The core never sends or handles it (01 §11.11 rule 2): the type is here for
// relay.gen.ts.
type PlaybackStop struct {
	V      int    `json:"v"`      // PlaybackStopVersion
	RoomID string `json:"roomId"` // the room the desktop app joined
}

// Browser families of PlaybackState.Browser: 05 §5.4's browser families, plus other. A web tab maps its M1
// client.browser to one of them: chrome, edge and firefox as they are, anything else (safari included) to other.
// tygo turns this group into the TS union type Browser, which is PlaybackState.browser's type in relay.gen.ts.
const (
	BrowserChrome  = "chrome"
	BrowserEdge    = "edge"
	BrowserFirefox = "firefox"
	BrowserOther   = "other"
)

// PlaybackState is the payload of playback.state: a web tab reports whether it plays isshoni audio, so 05 can keep
// that browser out of the share (05 §5.9). Sender: rooms/handoff.ts in every browser, addressed with to (never
// toRole) to the desktop connections in the tab's user.connections, whenever the tab's audible playback starts or
// stops (03 §12.2). Receiver: the core on the app's publisher connection, which emits EventPlaybackState{From,
// Playing, Browser, OS} for a payload that DecodePlaybackState accepts and ignores any other (01 §11.11 rule 1); the
// desktop SPA's own connection ignores the kind.
type PlaybackState struct {
	Playing bool              `json:"playing"`
	Browser string            `json:"browser" tstype:"Browser"` // "chrome" | "edge" | "firefox" | "other" (05 §5.4 families)
	OS      protocol.ClientOS `json:"os"`
}

// ErrInvalidPayload is wrapped by every error of the Decode functions, together with a *protocol.FieldError that
// names the field at fault ("payload" when the payload as a whole is not an object of the kind's shape).
var ErrInvalidPayload = errors.New("relay: invalid payload")

// Validate checks what every receiver checks of the payload alone (03 §12.2): v is PlaybackStopVersion and roomId is
// a room id (1-64 characters of [A-Za-z0-9_-]). That roomId is the receiver's room and the sender a desktop
// connection of the user are the receiver's checks. Errors are a *protocol.FieldError.
func (p PlaybackStop) Validate() error {
	switch {
	case p.V == 0:
		return &protocol.FieldError{Field: "v", Reason: protocol.FieldRequired}
	case p.V != PlaybackStopVersion:
		return &protocol.FieldError{Field: "v", Reason: protocol.FieldInvalid}
	case p.RoomID == "":
		return &protocol.FieldError{Field: "roomId", Reason: protocol.FieldRequired}
	case len(p.RoomID) > protocol.MaxOpaqueIDLen:
		return &protocol.FieldError{Field: "roomId", Reason: protocol.FieldTooLong}
	case !validID(p.RoomID):
		return &protocol.FieldError{Field: "roomId", Reason: protocol.FieldInvalid}
	}
	return nil
}

// Validate checks 01 §11.11 rule 1: browser is one of the four families. Playing has no wrong value, and os is not
// checked: like hello's client.os it may hold a value this build does not know, and 05 acts only on windows. Errors
// are a *protocol.FieldError.
func (p PlaybackState) Validate() error {
	switch p.Browser {
	case BrowserChrome, BrowserEdge, BrowserFirefox, BrowserOther:
		return nil
	case "":
		return &protocol.FieldError{Field: "browser", Reason: protocol.FieldRequired}
	}
	return &protocol.FieldError{Field: "browser", Reason: protocol.FieldInvalid}
}

// DecodePlaybackStop decodes the payload of an agent.recv of kind playback.stop and checks it with Validate.
func DecodePlaybackStop(payload json.RawMessage) (PlaybackStop, error) {
	return decode[PlaybackStop](KindPlaybackStop, payload)
}

// DecodePlaybackState decodes the payload of an agent.recv of kind playback.state and checks it with Validate: the
// decoding of 01 §11.11 rule 1. A payload it refuses is ignored by the receiver.
func DecodePlaybackState(payload json.RawMessage) (PlaybackState, error) {
	return decode[PlaybackState](KindPlaybackState, payload)
}

// decode reads a payload as protocol.Decode reads a message's data: a JSON object, keys matched as encoding/json
// matches them, unknown keys ignored, a value of the wrong JSON type refused, with protocol's bounds on depth and
// key length; then T's Validate method. The server bounds a payload at 16 KiB before it relays it.
func decode[T any](kind string, payload json.RawMessage) (T, error) {
	var v T
	if p := bytes.TrimLeft(payload, " \t\r\n"); len(p) == 0 || p[0] != '{' {
		// null, an array, a string, a number or a boolean: protocol.DecodeInto would read null as absent.
		return v, invalid(kind, &protocol.FieldError{Field: "payload", Reason: protocol.FieldInvalid})
	}
	if err := protocol.DecodeInto(protocol.Envelope{Data: payload}, &v); err != nil {
		var fe *protocol.FieldError
		if errors.As(err, &fe) {
			fe.Field = fieldName(reflect.TypeOf(v), fe.Field)
		}
		return v, invalid(kind, err)
	}
	return v, nil
}

// fieldName returns the JSON name of the field of struct type t that name matches the way encoding/json matches a
// key with a field (case folded), or "payload" for anything else: protocol's "data", the name of the whole payload.
// encoding/json may report a field under the key as the sender wrote it ("V" for v), and an error names only the
// fields of the kind's type, never something of the payload.
func fieldName(t reflect.Type, name string) string {
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag != "" && strings.EqualFold(tag, name) {
			return tag
		}
	}
	return "payload"
}

func invalid(kind string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalidPayload, kind, err)
}

// validID reports whether s is made of [A-Za-z0-9_-] only (protocol's opaque ids).
func validID(s string) bool {
	for i := range len(s) {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
