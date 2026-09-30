package sfu

import "github.com/pion/webrtc/v4"

// Identifiers (02 §5.2). They are opaque strings assigned by signal and the store; the SFU only compares them.
type (
	RoomID        string
	ParticipantID string
	ConnID        string
	UserID        string
	// ShareID is assigned by the signal hub: "s_" plus 16 lowercase base32 characters (01 §4.1).
	ShareID string
)

// TrackBinding is 01's TrackRef: one m-section (by mid) carrying one share's video or audio. Every pub offer lists
// one for each sending m-section, and every sub offer returns one for each m-section that carries a share.
type TrackBinding struct {
	MID   string
	Share ShareID
	Kind  webrtc.RTPCodecType
}
