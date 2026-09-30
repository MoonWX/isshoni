// Package api holds the JSON types of the REST API under /api/v1 and of the ops routes and Web Push payloads, plus
// the one REST error envelope and its code table.
//
// Owners: 03 (docs/m1/03-accounts-and-store.md §12) owns the REST DTOs in types.go and the envelope and code table
// in errors.go; 04 (docs/m1/04-server-platform.md §2, §18) adds the dashboard (ops.go), doctor and bandwidth
// (doctor.go), connection test (conntest.go) and push payload (push.go) types and its rows of the code table.
//
// Rules:
//   - The package imports only the standard library. store, auth, httpapi, netx, ops, doctor and push import it, and
//     tygo generates web/src/protocol/api.gen.ts from it (01 §14.4).
//   - JSON names are camelCase; enum values are lowercase strings; error codes are snake_case and never carry English
//     text (the SPA maps a code to errors.<code> and a field code to fieldErrors.<field>.<code>).
//   - Timestamps are time.Time (tygo maps them to string) and go on the wire as RFC 3339 UTC strings with exactly
//     three fractional digits and a Z suffix, "2026-10-01T12:00:00.000Z" (03 §3.3), the same form as 01's signaling
//     timestamps: fixed width, so they also sort as text. WireTime (wiretime.go) is that form; the MarshalJSON
//     methods in encode.go encode every DTO timestamp through it, converting any location and truncating any
//     precision for every encoder, and other packages use it for timestamps in their own JSON (04's admin socket).
//     Readers accept any RFC 3339 form. Durations are integer seconds, with the unit in the name (expiresIn,
//     retryAfter, expiresInS). PushPayload.TS is the one timestamp sent as unix milliseconds (04 §14.3).
//   - Optional fields carry omitempty or omitzero, and their zero value means "absent". Lists are always present
//     ([] rather than null, and AuditEntry.Detail is {} when empty), also when a producer leaves a slice nil: the
//     same MarshalJSON methods replace nil collections. The only null on the wire is AuditPage.NextBefore on the last
//     page.
//   - Passwords, tokens, SDP, push endpoints and token links are redacted in fmt and log/slog output (secret.go);
//     JSON carries them unchanged.
//   - Within v1 every change is additive (03 §12.1): a field is never renamed, removed or retyped, and an error code is
//     never reused with another meaning. Clients ignore unknown fields and the server ignores unknown request fields.
//   - Enum constants are declared in const groups named <Type><Value>, which tygo turns into TypeScript unions.
//     Open sets that clients must pass through unchanged (Info.Features) use untyped constants instead.
//
// Golden JSON for every DTO lives in testdata/; go test -run TestGolden -update rewrites it from the samples in
// golden_test.go.
package api
