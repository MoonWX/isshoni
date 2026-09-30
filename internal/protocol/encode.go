package protocol

import (
	"encoding/json"
	"errors"
	"time"
)

// The MarshalJSON methods below enforce two wire rules (01 §5) for every encoder, not just Marshal:
//   - arrays in snapshots and replies are always present: a nil slice encodes as [], never null;
//   - timestamps are RFC 3339 UTC strings with millisecond precision ("2026-10-12T19:04:05.123Z", like JavaScript's
//     Date.toISOString), not Go's default RFC 3339 with nanoseconds and the local offset.
//
// Each method converts to a local type without methods (same fields and tags) and fills in the values, so encoding
// stays with encoding/json. Every payload type with a non-omitempty slice or a time.Time field needs one; the
// no-null test walks the Registry and fails on a type that is missing it. Types that are embedded in other structs
// (ConnectionInfo) must not get one.

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// msTime encodes a time.Time as RFC 3339 UTC with exactly three fractional digits.
type msTime time.Time

const msTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// errTimeRange is returned for a timestamp that RFC 3339 cannot hold, as time.Time.MarshalJSON does.
var errTimeRange = errors.New("protocol: time year outside of range [0,9999]")

func (t msTime) MarshalJSON() ([]byte, error) {
	tt := time.Time(t).UTC()
	if y := tt.Year(); y < 0 || y > 9999 {
		return nil, errTimeRange
	}
	b := make([]byte, 0, len(msTimeLayout)+2)
	b = append(b, '"')
	b = tt.AppendFormat(b, msTimeLayout)
	return append(b, '"'), nil
}

func (h Hello) MarshalJSON() ([]byte, error) {
	type plain Hello
	p := plain(h)
	p.Features = nonNil(p.Features)
	return json.Marshal(p)
}

func (c Caps) MarshalJSON() ([]byte, error) {
	type plain Caps
	p := plain(c)
	p.Decode = nonNil(p.Decode)
	return json.Marshal(p)
}

func (w Welcome) MarshalJSON() ([]byte, error) {
	type plain Welcome
	p := plain(w)
	p.Features = nonNil(p.Features)
	p.ICEServers = nonNil(p.ICEServers)
	return json.Marshal(struct {
		plain
		ServerTime msTime `json:"serverTime"`
	}{p, msTime(w.ServerTime)})
}

func (s ICEServer) MarshalJSON() ([]byte, error) {
	type plain ICEServer
	p := plain(s)
	p.URLs = nonNil(p.URLs)
	return json.Marshal(p)
}

func (s RoomState) MarshalJSON() ([]byte, error) {
	type plain RoomState
	p := plain(s)
	p.Participants = nonNil(p.Participants)
	p.Shares = nonNil(p.Shares)
	return json.Marshal(p)
}

func (pi ParticipantInfo) MarshalJSON() ([]byte, error) {
	type plain ParticipantInfo
	p := plain(pi)
	p.Connections = nonNil(p.Connections)
	return json.Marshal(struct {
		plain
		JoinedAt msTime `json:"joinedAt"`
	}{p, msTime(pi.JoinedAt)})
}

func (s ShareInfo) MarshalJSON() ([]byte, error) {
	type plain ShareInfo
	p := plain(s)
	p.Layers = nonNil(p.Layers)
	p.Watchers = nonNil(p.Watchers)
	return json.Marshal(struct {
		plain
		StartedAt msTime `json:"startedAt"`
	}{p, msTime(s.StartedAt)})
}

func (e RoomEvent) MarshalJSON() ([]byte, error) {
	type plain RoomEvent
	return json.Marshal(struct {
		plain
		At msTime `json:"at"`
	}{plain(e), msTime(e.At)})
}

func (s ShareParams) MarshalJSON() ([]byte, error) {
	type plain ShareParams
	p := plain(s)
	p.Encodings = nonNil(p.Encodings)
	return json.Marshal(p)
}

func (o PCOffer) MarshalJSON() ([]byte, error) {
	type plain PCOffer
	p := plain(o)
	p.Tracks = nonNil(p.Tracks)
	return json.Marshal(p)
}

func (u SubscribeUpdate) MarshalJSON() ([]byte, error) {
	type plain SubscribeUpdate
	p := plain(u)
	p.Subs = nonNil(p.Subs)
	return json.Marshal(p)
}

func (r SubscribeResult) MarshalJSON() ([]byte, error) {
	type plain SubscribeResult
	p := plain(r)
	p.Ignored = nonNil(p.Ignored)
	return json.Marshal(p)
}

func (s SubscribeStatus) MarshalJSON() ([]byte, error) {
	type plain SubscribeStatus
	p := plain(s)
	p.Subs = nonNil(p.Subs)
	return json.Marshal(p)
}

func (s ClientStats) MarshalJSON() ([]byte, error) {
	type plain ClientStats
	p := plain(s)
	p.PCs = nonNil(p.PCs)
	return json.Marshal(p)
}

func (s ServerStats) MarshalJSON() ([]byte, error) {
	type plain ServerStats
	p := plain(s)
	p.Subs = nonNil(p.Subs)
	p.Layers = nonNil(p.Layers)
	return json.Marshal(p)
}

func (i Invalidate) MarshalJSON() ([]byte, error) {
	type plain Invalidate
	p := plain(i)
	p.Topics = nonNil(p.Topics)
	return json.Marshal(p)
}

// emptyObject is what a nil relay payload encodes as: "absent means {}", as for the envelope's data.
var emptyObject = json.RawMessage("{}")

func (a AgentSend) MarshalJSON() ([]byte, error) {
	type plain AgentSend
	p := plain(a)
	if p.Payload == nil {
		p.Payload = emptyObject
	}
	return json.Marshal(p)
}

func (a AgentRecv) MarshalJSON() ([]byte, error) {
	type plain AgentRecv
	p := plain(a)
	if p.Payload == nil {
		p.Payload = emptyObject
	}
	return json.Marshal(p)
}

func (u UserConnections) MarshalJSON() ([]byte, error) {
	type plain UserConnections
	p := plain(u)
	p.Connections = nonNil(p.Connections)
	return json.Marshal(p)
}
