package protocol

// QualityHint tells a sharer to change how it encodes one share. Policy (when and what): 02.
type QualityHint struct {
	ShareID    string     `json:"shareId"`
	Reason     HintReason `json:"reason"`
	Codec      CodecKey   `json:"codec,omitempty"`      // set: switch profile (the client re-offers the pub PC with it first)
	Encodings  []Encoding `json:"encodings,omitempty"`  // set: the complete list; apply every entry with setParameters (active, maxBitrate, ...)
	MaxBitrate int64      `json:"maxBitrate,omitempty"` // native publishers (M2+): total cap, applied with hysteresis
}

// HintReason says why a quality.hint was sent. Clients show unknown reasons without a specific text.
type HintReason string

const (
	HintReasonViewers    HintReason = "viewers"    // e.g. nobody watches high: deactivate "f"
	HintReasonCongestion HintReason = "congestion" // server uplink estimate
	HintReasonCodec      HintReason = "codec"      // room codec safe set changed
	HintReasonPreset     HintReason = "preset"
	HintReasonAdmin      HintReason = "admin"
)

// Valid reports whether r is a known reason.
func (r HintReason) Valid() bool {
	switch r {
	case HintReasonViewers, HintReasonCongestion, HintReasonCodec, HintReasonPreset, HintReasonAdmin:
		return true
	}
	return false
}

// CapsUpdate reports changed capabilities (web: polled every 5 s while a subscription has reason codec). It is the
// client's only part in codec recovery; all rebuild retries are server-driven (02 §8.5).
type CapsUpdate struct {
	Caps Caps `json:"caps"`
}
