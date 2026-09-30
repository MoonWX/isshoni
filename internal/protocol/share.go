package protocol

// ShareStart starts a share (01 §8.7). The reply is its ShareParams; the client then offers the share's tracks on
// the pub PC.
type ShareStart struct {
	Kind     ShareKind `json:"kind"`
	Label    string    `json:"label,omitempty"` // user-typed only; <= 40 code points
	Preset   Preset    `json:"preset"`
	Audio    bool      `json:"audio"`              // an audio track will be sent
	Ref      string    `json:"ref"`                // client idempotency key, 1-32 chars, unique per connection
	Replaces string    `json:"replaces,omitempty"` // previous shareId of the same local capture (§10.6)
}

// ShareParams tells the sharer how to encode. The server computes it from the preset, the room's codec safe set and
// admin limits (policy and numbers: 02). Encodings is in wire order, high first (f, then q); that is not the order
// a web client gives the browser. Web clients (05 §13.4) pass complete sendEncodings to addTransceiver in ascending
// order (q, then f), each with rid, active, maxBitrate, maxFramerate and scaleResolutionDownBy =
// max(1, sqrt(width*height/maxPixels)). Later changes go through RTCRtpSender.setParameters, matched by rid.
type ShareParams struct {
	ShareID      string     `json:"shareId"`
	Codec        CodecKey   `json:"codec"`        // H.264 profile to put first in setCodecPreferences
	Encodings    []Encoding `json:"encodings"`    // wire order, high first (f, q); not the sendEncodings order
	AudioBitrate int        `json:"audioBitrate"` // Opus target in bit/s (the pub answer's maxaveragebitrate)
}

// Encoding is one simulcast layer's encoder settings.
type Encoding struct {
	RID          string     `json:"rid"` // RIDHigh or RIDLow
	Layer        VideoLayer `json:"layer"`
	Active       bool       `json:"active"`
	MaxBitrate   int64      `json:"maxBitrate"`
	MaxFramerate int        `json:"maxFramerate"`
	MaxPixels    int        `json:"maxPixels"` // pixel budget, aspect-ratio neutral (ultrawide screens)
}

// ShareUpdate changes the label or preset of a share of the same user. The reply carries the new ShareParams.
type ShareUpdate struct {
	ShareID string  `json:"shareId"`
	Label   *string `json:"label,omitempty"` // "" clears the label
	Preset  Preset  `json:"preset,omitempty"`
	// later (M2, feature share.pause): Paused *bool `json:"paused,omitempty"`
}

// ShareStop ends a share. It is idempotent: an unknown or already ended share replies ok.
type ShareStop struct {
	ShareID string `json:"shareId"`
}

// ShareKind is what is being shared. Clients treat unknown values as screen.
type ShareKind string

const (
	ShareKindScreen ShareKind = "screen" // getDisplayMedia displaySurface "monitor"
	ShareKindWindow ShareKind = "window" // "window"
	ShareKindTab    ShareKind = "tab"    // "browser"
)

// Valid reports whether k is a known kind.
func (k ShareKind) Valid() bool {
	return k == ShareKindScreen || k == ShareKindWindow || k == ShareKindTab
}

// Preset is the sharer's quality preset (numbers: 02). Clients treat unknown values as auto.
type Preset string

const (
	PresetAuto  Preset = "auto"
	PresetGame  Preset = "game"
	PresetMovie Preset = "movie"
	PresetText  Preset = "text"
)

// Valid reports whether p is a known preset.
func (p Preset) Valid() bool {
	switch p {
	case PresetAuto, PresetGame, PresetMovie, PresetText:
		return true
	}
	return false
}

// ShareStatus is a share's state (01 §4.4). Clients treat unknown values as live.
type ShareStatus string

const (
	ShareStatusStarting ShareStatus = "starting"
	ShareStatusLive     ShareStatus = "live"
	ShareStatusStalled  ShareStatus = "stalled"
	// later (M2, feature share.pause): ShareStatusPaused = "paused"
)

// Valid reports whether s is a known status.
func (s ShareStatus) Valid() bool {
	return s == ShareStatusStarting || s == ShareStatusLive || s == ShareStatusStalled
}
