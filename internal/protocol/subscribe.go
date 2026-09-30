package protocol

// VideoLayer is a simulcast layer, or off. Clients treat unknown values in statuses and watchers as high.
type VideoLayer string

const (
	VideoLayerHigh VideoLayer = "high" // rid "f": full (source, <=1080p60 by default)
	VideoLayerLow  VideoLayer = "low"  // rid "q": preview (360p15)
	VideoLayerOff  VideoLayer = "off"
	// later (M5, feature layer.mid): VideoLayerMid = "mid", rid "h"
)

// Valid reports whether l is a known layer.
func (l VideoLayer) Valid() bool {
	return l == VideoLayerHigh || l == VideoLayerLow || l == VideoLayerOff
}

// AudioState says whether a subscription forwards a share's audio.
type AudioState string

const (
	AudioStateOn  AudioState = "on"
	AudioStateOff AudioState = "off"
)

// Valid reports whether a is a known state.
func (a AudioState) Valid() bool { return a == AudioStateOn || a == AudioStateOff }

// RIDs used in simulcast for each layer. Fixed for all senders (browser and native).
const (
	RIDHigh = "f"
	RIDLow  = "q"
)

// SubscribeUpdate is desired state per share, merged into the connection's existing wants (01 §8.9). All changes of
// one user action go in one update, so the server applies them atomically (audio follows focus).
type SubscribeUpdate struct {
	Subs []SubscriptionWant `json:"subs"` // 1..64 items, unique shareIds
}

// SubscriptionWant is the desired video layer and audio of one share. {off, off} pauses a subscription but keeps its
// transceivers.
type SubscriptionWant struct {
	ShareID string     `json:"shareId"`
	Video   VideoLayer `json:"video"`
	Audio   AudioState `json:"audio"`
}

// SubscribeResult is the ok payload of subscribe.update.
type SubscribeResult struct {
	Ignored []string `json:"ignored"` // shareIds that don't exist (any more) in the connection's room
}

// SubscribeStatus reports forwarding that differs from the request, or changes back.
type SubscribeStatus struct {
	Subs []SubscriptionStatus `json:"subs"` // only the subscriptions whose status changed
}

// SubscriptionStatus is what the server forwards for one subscription now.
type SubscriptionStatus struct {
	ShareID        string       `json:"shareId"`
	Video          VideoLayer   `json:"video"` // being forwarded now
	Audio          AudioState   `json:"audio"`
	RequestedVideo VideoLayer   `json:"requestedVideo"`
	Reason         StatusReason `json:"reason,omitempty"` // absent: video == requestedVideo
}

// StatusReason says why forwarding differs from the request. 02's finer reasons are mapped to these four (§15.4).
// Clients show unknown reasons without a specific text.
type StatusReason string

const (
	StatusReasonBandwidth   StatusReason = "bandwidth"   // server downgraded (REMB/loss estimate, 02)
	StatusReasonUnavailable StatusReason = "unavailable" // publisher doesn't send that layer (now)
	StatusReasonCodec       StatusReason = "codec"       // this client can't decode the share's codec (§11.7)
	StatusReasonWaiting     StatusReason = "waiting"     // sub PC not connected yet, or waiting for a keyframe
)

// Valid reports whether r is a known reason.
func (r StatusReason) Valid() bool {
	switch r {
	case StatusReasonBandwidth, StatusReasonUnavailable, StatusReasonCodec, StatusReasonWaiting:
		return true
	}
	return false
}
