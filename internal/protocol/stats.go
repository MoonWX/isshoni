package protocol

// ClientStats is the client's stats report, sent every 10 s while it has a PC (01 §8.11). No IP addresses: the
// candidate type only.
type ClientStats struct {
	IntervalMs int             `json:"intervalMs"`
	PCs        []PCStats       `json:"pcs"`
	Inbound    []InboundStats  `json:"inbound,omitempty"`
	Outbound   []OutboundStats `json:"outbound,omitempty"`
}

// PCStats describes one of the client's PCs.
type PCStats struct {
	PC              PCKind `json:"pc"`
	Gen             uint32 `json:"gen"`
	State           string `json:"state"` // RTCPeerConnectionState
	RTTMs           int    `json:"rttMs,omitempty"`
	OutgoingBitrate int64  `json:"outgoingBitrate,omitempty"` // availableOutgoingBitrate
	CandidateType   string `json:"candidateType,omitempty"`   // local side of the selected pair: host|srflx|prflx|relay
	Transport       string `json:"transport,omitempty"`       // udp|tcp
}

// InboundStats describes one received track.
type InboundStats struct {
	ShareID        string    `json:"shareId"`
	Kind           TrackKind `json:"kind"`
	Bitrate        int64     `json:"bitrate"`
	PacketsLost    int64     `json:"packetsLost"` // may be negative (RTCP cumulative loss with duplicates)
	JitterBufferMs int       `json:"jitterBufferMs,omitempty"`
	FPS            float64   `json:"fps,omitempty"`
	Width          int       `json:"width,omitempty"`
	Height         int       `json:"height,omitempty"`
	FreezeCount    int       `json:"freezeCount,omitempty"`
	Decoder        string    `json:"decoder,omitempty"`
	HWDecoder      bool      `json:"hwDecoder,omitempty"`
	Codec          CodecKey  `json:"codec,omitempty"`
	// Cumulative counters since the track started (getStats values); the hub turns deltas into the
	// isshoni_client_* metrics of §18 for the M1 exit test (06 §12.3).
	FramesDecoded    int64 `json:"framesDecoded,omitempty"`
	FramesDropped    int64 `json:"framesDropped,omitempty"`
	FreezeDurationMs int64 `json:"freezeDurationMs,omitempty"` // totalFreezesDuration
	ConcealedSamples int64 `json:"concealedSamples,omitempty"` // audio
	TotalSamples     int64 `json:"totalSamples,omitempty"`     // audio: totalSamplesReceived
}

// OutboundStats describes one sent track or simulcast layer.
type OutboundStats struct {
	ShareID           string    `json:"shareId"`
	Kind              TrackKind `json:"kind"`
	RID               string    `json:"rid,omitempty"`
	Bitrate           int64     `json:"bitrate"`
	FPS               float64   `json:"fps,omitempty"`
	Width             int       `json:"width,omitempty"`
	Height            int       `json:"height,omitempty"`
	Encoder           string    `json:"encoder,omitempty"`
	HWEncoder         bool      `json:"hwEncoder,omitempty"`
	QualityLimitation string    `json:"qualityLimitation,omitempty"` // none|cpu|bandwidth|other
}

// StatsWatch turns the server's stats stream for this connection on or off ("Stats for nerds").
type StatsWatch struct {
	On bool `json:"on"`
}

// ServerStats is sent every 2 s while the connection watches stats; values come from MediaPeer.Stats() (02).
type ServerStats struct {
	DownlinkEstimate int64              `json:"downlinkEstimate,omitempty"` // bit/s for this connection's sub PC
	Subs             []ServerSubStats   `json:"subs"`
	Layers           []ServerLayerStats `json:"layers"` // this connection's own published layers
}

// ServerSubStats describes one forwarded track of this connection's subscriptions.
type ServerSubStats struct {
	ShareID string     `json:"shareId"`
	Kind    TrackKind  `json:"kind"`
	Layer   VideoLayer `json:"layer,omitempty"` // video only
	Bitrate int64      `json:"bitrate"`
	LossPct float64    `json:"lossPct"`
	Dropped int64      `json:"dropped"` // packets dropped by the DownTrack queue
}

// ServerLayerStats describes one received layer of this connection's own shares.
type ServerLayerStats struct {
	ShareID string    `json:"shareId"`
	Kind    TrackKind `json:"kind"`
	RID     string    `json:"rid,omitempty"`
	Bitrate int64     `json:"bitrate"`
	LossPct float64   `json:"lossPct"`
}
