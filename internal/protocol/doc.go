// Package protocol is isshoni's signaling protocol, version 1 (docs/m1/01-protocol.md): the WebSocket envelope,
// every message payload and enum, the error codes and close codes, the message registry, and payload validation.
//
// It is the single source of truth for the wire format. The TypeScript types in web/src/protocol are generated from
// it (tygo for the types, internal/protocol/gen/tsregistry for the registry; 01 §14.4), and the golden fixtures in
// testdata/v1 pin the JSON shape of every message (01 §14.3).
//
// The package imports only the standard library, so the server, the Go clients and the generator can all use it.
//
// # Wire conventions (01 §5)
//
// Every WebSocket text frame is one [Envelope] {type, id, re, data}. Keys are camelCase, unknown keys are ignored,
// IDs are strings, units live in the field name (…Bitrate in bit/s, …Ms in milliseconds), timestamps are RFC 3339
// UTC strings with millisecond precision, and arrays are always present ([] rather than null). The types' MarshalJSON
// methods enforce the last two rules, so callers may leave slices nil.
//
// # Compatibility (01 §14)
//
// Within a protocol version every change is additive: new optional fields whose zero value means the old behavior,
// new message types behind a feature, new server→client enum values with a client fallback, new error codes, new
// features. Renaming or removing a field, changing its type, unit or meaning, or reusing an error code is forbidden.
// The fixtures are copied to testdata/compat/<version> before each release, and the compat test decodes them with
// the current code.
//
// # Adding to this package
//
// Enum constants use the <Type><Value> naming in one const group per type, so tygo turns them into TS union types;
// CodecKey and AuthScheme use single-line constants on purpose so they stay plain strings in TS. A new message type
// needs a Registry entry and a fixture; a new payload type with a slice field needs a MarshalJSON method in
// encode.go (the no-null test finds a missing one); a new client→server payload needs a Validate method.
package protocol

// Protocol versions this build speaks (01 §6.1). The server accepts [MinVersion, Version]; once version 2 exists,
// MinVersion stays at Version-1.
const (
	Version    = 1 // newest protocol version this build speaks
	MinVersion = 1 // oldest version this build still speaks (Version-1 once Version >= 2)
)

// Negotiate picks the protocol version for a connection from the client's hello (01 §6.1): chosen =
// min(clientMax, Version). ok is false when chosen < max(clientMin, MinVersion); the server then answers
// protocol_unsupported and closes with 4426.
func Negotiate(clientMax, clientMin int) (chosen int, ok bool) {
	chosen = min(clientMax, Version)
	return chosen, chosen >= max(clientMin, MinVersion)
}
