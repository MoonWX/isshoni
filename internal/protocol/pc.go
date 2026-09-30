package protocol

// TrackKind is the media kind of a track.
type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

// Valid reports whether k is a known kind.
func (k TrackKind) Valid() bool { return k == TrackKindVideo || k == TrackKindAudio }

// PCKind names one of a connection's two PeerConnections. Each has a fixed offerer (01 §9 rule 1).
type PCKind string

const (
	PCKindPub PCKind = "pub" // publish PC: the client offers
	PCKindSub PCKind = "sub" // subscribe PC: the server offers
)

// Valid reports whether k is a known PC kind.
func (k PCKind) Valid() bool { return k == PCKindPub || k == PCKindSub }

// PCOffer is an SDP offer: from the client for pub, from the server for sub (01 §8.8, rules in §9).
type PCOffer struct {
	PC     PCKind     `json:"pc"`
	Gen    uint32     `json:"gen"`    // PC generation, starts at 1; the side that creates PCs increments it
	Neg    uint32     `json:"neg"`    // offer number within gen, starts at 1; the offerer increments it
	SDP    SDP        `json:"sdp"`    // an ICE restart shows only here (new ICE credentials); there is no flag
	Tracks []TrackRef `json:"tracks"` // every m-section currently carrying a share
}

// PCAnswer answers a PCOffer: from the server for pub, from the client for sub.
type PCAnswer struct {
	PC  PCKind `json:"pc"`
	Gen uint32 `json:"gen"`
	Neg uint32 `json:"neg"` // echoes the offer
	SDP SDP    `json:"sdp"`
}

// TrackRef maps one m-section of an offer to a share. Both sides use it, not msid.
type TrackRef struct {
	MID     string    `json:"mid"`
	ShareID string    `json:"shareId"`
	Kind    TrackKind `json:"kind"`
}

// PCICE carries one trickled ICE candidate. Clients trickle; the server puts all its candidates in its SDP.
type PCICE struct {
	PC        PCKind        `json:"pc"`
	Gen       uint32        `json:"gen"`
	Candidate *ICECandidate `json:"candidate,omitempty"` // absent = end of candidates (optional; the server ignores it)
}

// ICECandidate has the JSON shape of RTCIceCandidateInit and pion's webrtc.ICECandidateInit.
type ICECandidate struct {
	Candidate        string  `json:"candidate"` // <= 512 bytes
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// PCRestart asks the offerer of a PC to restart ICE or rebuild the PC (§10.4).
// Client -> server: sub only (mode ice or rebuild). Server -> client: pub only, mode rebuild (reason failed).
type PCRestart struct {
	PC     PCKind        `json:"pc"`
	Gen    uint32        `json:"gen"` // generation the requester has now; stale requests are ignored
	Mode   RestartMode   `json:"mode"`
	Reason RestartReason `json:"reason"`
}

// RestartMode is how a PC recovers.
type RestartMode string

const (
	RestartModeICE     RestartMode = "ice"
	RestartModeRebuild RestartMode = "rebuild"
)

// Valid reports whether m is a known mode.
func (m RestartMode) Valid() bool { return m == RestartModeICE || m == RestartModeRebuild }

// RestartReason says why a restart is requested.
type RestartReason string

const (
	RestartReasonDisconnected RestartReason = "disconnected"
	RestartReasonFailed       RestartReason = "failed"
	// No "codec" reason: codec-blocked subscriptions are retried by the server (02 §8.5), never by the client.
)

// Valid reports whether r is a known reason.
func (r RestartReason) Valid() bool {
	return r == RestartReasonDisconnected || r == RestartReasonFailed
}

// PCClose tells the server the client closed its pub PC on purpose (no shares left, or leaving).
// M1 clients send it only for pub; the server ignores pc: sub.
type PCClose struct {
	PC  PCKind `json:"pc"`
	Gen uint32 `json:"gen"`
}
