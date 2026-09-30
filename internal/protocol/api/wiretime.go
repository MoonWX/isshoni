package api

import (
	"errors"
	"time"
)

// WireTime is a timestamp in the one JSON form of the API (03 §3.3): an RFC 3339 UTC string with exactly three
// fractional digits and a Z suffix, "2026-09-30T12:00:00.000Z", whatever the location and precision of the value
// (the fraction is truncated to the millisecond, not rounded). It is the fixed-width form of 01's signaling
// timestamps (01 §5, internal/protocol) and of JavaScript's Date.toISOString, so the strings also sort as text.
//
// DTO fields stay time.Time, which tygo maps to string: the MarshalJSON methods in encode.go encode each of them as a
// WireTime, so every encoder sends this form. Code that writes a timestamp into JSON outside these DTOs (04's admin
// socket, for example) declares the field as WireTime. Decoding accepts any RFC 3339 form, as time.Time does, and
// keeps the value as sent.
type WireTime time.Time

// wireTimeLayout is WireTime's form. internal/protocol encodes its timestamps with the same layout; this package
// imports only the standard library, so it has its own copy, and TestWireTimeMatchesProtocolFixtures checks that the
// two agree on 01's fixtures.
const wireTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// errTimeRange is returned for a timestamp that RFC 3339 cannot hold, as time.Time.MarshalJSON does.
var errTimeRange = errors.New("api: time year outside of range [0,9999]")

// MarshalJSON returns the quoted wire form, or an error when the year (in UTC) is outside 0–9999.
func (t WireTime) MarshalJSON() ([]byte, error) {
	tt := time.Time(t).UTC()
	if y := tt.Year(); y < 0 || y > 9999 {
		return nil, errTimeRange
	}
	b := make([]byte, 0, len(wireTimeLayout)+2)
	b = append(b, '"')
	b = tt.AppendFormat(b, wireTimeLayout)
	return append(b, '"'), nil
}

// UnmarshalJSON accepts any RFC 3339 string, like time.Time: readers never require the fixed form. JSON null leaves
// the value unchanged.
func (t *WireTime) UnmarshalJSON(b []byte) error {
	return (*time.Time)(t).UnmarshalJSON(b)
}

// IsZero makes omitzero fields behave as they do with time.Time: a zero time in any location is absent.
func (t WireTime) IsZero() bool { return time.Time(t).IsZero() }

// String returns the wire form without quotes, for logs and CLI output. A year outside 0–9999 prints in Go's
// default time format instead.
func (t WireTime) String() string {
	tt := time.Time(t).UTC()
	if y := tt.Year(); y < 0 || y > 9999 {
		return tt.String()
	}
	return tt.Format(wireTimeLayout)
}
