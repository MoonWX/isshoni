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

// The enums below are this package's own (02 §6.1): 01's sfuplane maps them to the wire by name, so their numbers may
// change. Role, ClientKind and PCKind have no valid zero value, so a value the adapter forgot to map is refused
// instead of meaning "full", "web" or "pub".

// Role says what a Conn may do (01 §6.3).
type Role uint8

// The roles: full publishes and subscribes (the web app with getDisplayMedia), viewer only subscribes (phones),
// publisher only publishes (the M2 desktop core) and agent is a publisher that also takes relayed commands (M4).
const (
	RoleFull Role = iota + 1
	RoleViewer
	RolePublisher
	RoleAgent
)

// String returns the role's name, which is also its Metrics label value (02 §13).
func (r Role) String() string {
	switch r {
	case RoleFull:
		return "full"
	case RoleViewer:
		return "viewer"
	case RolePublisher:
		return "publisher"
	case RoleAgent:
		return "agent"
	}
	return "role?"
}

func (r Role) valid() bool { return r >= RoleFull && r <= RoleAgent }

// canPublish reports whether the role may start shares and offer a pub PC.
func (r Role) canPublish() bool { return r == RoleFull || r == RolePublisher || r == RoleAgent }

// canSubscribe reports whether the role may subscribe to shares and get a sub PC.
func (r Role) canSubscribe() bool { return r == RoleFull || r == RoleViewer }

// ClientKind is the kind of client behind a Conn.
type ClientKind uint8

// The client kinds. ClientMobile is for later native apps.
const (
	ClientWeb ClientKind = iota + 1
	ClientDesktop
	ClientMobile
)

// String returns the kind's name.
func (k ClientKind) String() string {
	switch k {
	case ClientWeb:
		return "web"
	case ClientDesktop:
		return "desktop"
	case ClientMobile:
		return "mobile"
	}
	return "client?"
}

func (k ClientKind) valid() bool { return k >= ClientWeb && k <= ClientMobile }

// PCKind names one of a Conn's two PeerConnections: the client offers on pub, the server on sub, so there is never
// glare (01 §9 rule 1).
type PCKind uint8

// The PC kinds.
const (
	PCPub PCKind = iota + 1
	PCSub
)

// String returns "pub" or "sub", the Metrics label values (02 §13).
func (k PCKind) String() string {
	switch k {
	case PCPub:
		return "pub"
	case PCSub:
		return "sub"
	}
	return "pc?"
}

// Quality is a video quality a subscriber asks for or gets, ordered so that a smaller value is a cheaper one.
// Later (M5): QualityMedium for the "h" layer.
type Quality uint8

// The qualities: off sends no video, low is the preview layer (a thumbnail), high the full layer (the focused share).
const (
	QualityOff Quality = iota
	QualityLow
	QualityHigh
)

// String returns "off", "low" or "high", the Metrics label values (02 §13).
func (q Quality) String() string {
	switch q {
	case QualityOff:
		return "off"
	case QualityLow:
		return "low"
	case QualityHigh:
		return "high"
	}
	return "quality?"
}

func (q Quality) valid() bool { return q <= QualityHigh }

// Preset is what the sharer said the share shows; it picks the encodings and the audio bitrate (02 §8.6).
type Preset uint8

// The presets.
const (
	PresetAuto Preset = iota
	PresetGame
	PresetMovie
	PresetText
)

// String returns the preset's name.
func (p Preset) String() string {
	switch p {
	case PresetAuto:
		return "auto"
	case PresetGame:
		return "game"
	case PresetMovie:
		return "movie"
	case PresetText:
		return "text"
	}
	return "preset?"
}

func (p Preset) valid() bool { return p <= PresetText }

// SourceKind is what a share captures. It is informational and copied into ShareInfo.
type SourceKind uint8

// The source kinds.
const (
	SourceUnknown SourceKind = iota
	SourceScreen
	SourceWindow
	SourceTab
)

// String returns the kind's name.
func (k SourceKind) String() string {
	switch k {
	case SourceUnknown:
		return "unknown"
	case SourceScreen:
		return "screen"
	case SourceWindow:
		return "window"
	case SourceTab:
		return "tab"
	}
	return "source?"
}

// ShareState is a share's media state (02 §5.3). These are media facts only: the hub owns the lifecycle that clients
// see, its timeouts and the end reasons, and an ended share is never listed.
type ShareState uint8

// The states: pending until the first keyframe, live while media flows, stalled while the pub PC is gone or not
// connected.
const (
	SharePending ShareState = iota
	ShareLive
	ShareStalled
)

// String returns "pending", "live" or "stalled", the Metrics label values (02 §13).
func (s ShareState) String() string {
	switch s {
	case SharePending:
		return "pending"
	case ShareLive:
		return "live"
	case ShareStalled:
		return "stalled"
	}
	return "state?"
}

// EndReason says why a share ended or a Conn closed. The values are 01's protocol.EndReason strings, passed through
// unchanged; the SFU chooses only EndReasonServerShutdown itself (SFU.Close).
type EndReason string

// The end reasons of 01 §8.6.
const (
	EndReasonStopped        EndReason = "stopped"
	EndReasonLeft           EndReason = "left"
	EndReasonDisconnected   EndReason = "disconnected"
	EndReasonMediaTimeout   EndReason = "media_timeout"
	EndReasonKicked         EndReason = "kicked" // reserved
	EndReasonRoomClosed     EndReason = "room_closed"
	EndReasonServerShutdown EndReason = "server_shutdown"
)

// SubReason says why a subscription gets less video than it asked for (02 §6.2). 01 §15.4 maps the values to the
// wire's four status reasons.
type SubReason string

// The reasons. SubReasonNone with Forwarded below Requested means the sub PC isn't connected yet or the target
// layer's keyframe hasn't arrived (01's "waiting"). SubReasonServerLimit is reserved (M5).
const (
	SubReasonNone               SubReason = ""
	SubReasonBandwidth          SubReason = "bandwidth"
	SubReasonServerLimit        SubReason = "server_limit"
	SubReasonCodecMismatch      SubReason = "codec_mismatch"
	SubReasonDecoderUnavailable SubReason = "decoder_unavailable"
	SubReasonDecoderFailed      SubReason = "decoder_failed"
	SubReasonNoPreviewLayer     SubReason = "no_preview_layer"
	SubReasonNoLayer            SubReason = "no_layer"
)

// DecodeCaps are a viewer's decode capabilities (01's hello.caps.decode without the "h264/" prefix).
type DecodeCaps struct {
	H264 []ProfileKey // what the viewer can decode; empty = no H.264 decoder (yet)
}

// TrackBinding is 01's TrackRef: one m-section (by mid) carrying one share's video or audio. Every pub offer lists
// one for each sending m-section, and every sub offer returns one for each m-section that carries a share.
type TrackBinding struct {
	MID   string
	Share ShareID
	Kind  webrtc.RTPCodecType
}

// JoinParams describe the connection that joins a room (02 §6.1).
type JoinParams struct {
	Room        RoomID
	Participant ParticipantID
	User        UserID
	Conn        ConnID
	Role        Role
	Client      ClientKind
	Decode      DecodeCaps
	Signaler    Signaler // stable for the Conn's life (survives WebSocket resumes)
}

// StartShareParams describe a new share.
type StartShareParams struct {
	ID     ShareID    // from the hub (01)
	Preset Preset     // sets the encodings and the Opus maxaveragebitrate in the publish answer (02 §8.6)
	Audio  bool       // the publisher will send one audio track
	Source SourceKind // informational, copied into ShareInfo
}

// ShareUpdate changes a share: new encodings at once, the audio bitrate on the next publish offer.
type ShareUpdate struct{ Preset *Preset }

// ShareParams tells the sharer how to encode (01's protocol.ShareParams without the wire types). Numbers: 02 §8.6.
type ShareParams struct {
	Profile      ProfileKey       // the room's codec policy: put first in the publisher's codec preferences
	Encodings    []EncodingParams // f first, then q
	AudioBitrate int              // Opus target in bit/s (the pub answer's maxaveragebitrate)
}

// EncodingParams is one simulcast encoding of ShareParams.
type EncodingParams struct {
	RID          string // "f" | "q"
	Active       bool
	MaxBitrate   int // bit/s, after the admin cap (Limits.MaxShareKbps)
	MaxFramerate int
	MaxPixels    int // pixel budget, aspect-ratio neutral
}

// SubscriptionUpdate is the desired state of one subscription (01's subscribe.update item).
type SubscriptionUpdate struct {
	Share ShareID
	Video Quality // high = focused, low = thumbnail, off = hidden
	Audio bool    // audio follows focus: normally true only for the focused share
}
