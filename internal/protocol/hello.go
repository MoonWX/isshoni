package protocol

import "time"

// Hello is the first message of every connection (01 §8.2). hello, welcome, error and the envelope are frozen
// across all protocol versions (additive changes only), so any client can read the server's answer.
type Hello struct {
	Protocol    int        `json:"protocol"`    // the client's newest protocol version
	MinProtocol int        `json:"minProtocol"` // the client's oldest protocol version
	Features    []Feature  `json:"features"`
	Client      ClientInfo `json:"client"`
	Role        Role       `json:"role"`
	ResumeToken Secret     `json:"resumeToken,omitempty"` // from the last welcome of the connection to resume (§10.3)
	Auth        *HelloAuth `json:"auth,omitempty"`        // only when the upgrade carried no valid cookie
	Caps        Caps       `json:"caps"`
}

// HelloAuth carries a bearer token (native apps, M2+). M1 servers always answer unauthenticated.
type HelloAuth struct {
	Scheme AuthScheme `json:"scheme"`
	Token  Secret     `json:"token"`
}

// AuthScheme is the credential scheme in HelloAuth.
type AuthScheme string

const AuthSchemeBearer AuthScheme = "bearer" // the only scheme in v1 (TS: plain string)

// Valid reports whether s is a known scheme.
func (s AuthScheme) Valid() bool { return s == AuthSchemeBearer }

// ClientInfo describes the client build. OS and version are never shown to other users (01 §17).
type ClientInfo struct {
	Kind    ClientKind `json:"kind"`
	Version string     `json:"version"` // SemVer of the client build; web = SPA build = server release
	OS      ClientOS   `json:"os"`
	Browser string     `json:"browser,omitempty"` // chrome|edge|firefox|safari|other; diagnostics only
}

// ClientKind is the kind of client. The server accepts unknown kinds ([a-z]{1,16}) and treats them like web.
type ClientKind string

const (
	ClientKindWeb     ClientKind = "web"
	ClientKindDesktop ClientKind = "desktop" // later (M2/M3/M4)
	ClientKindMobile  ClientKind = "mobile"  // pending native apps
	ClientKindTool    ClientKind = "tool"    // isshoni-loadtest, tests
)

// Valid reports whether k is a known kind.
func (k ClientKind) Valid() bool {
	switch k {
	case ClientKindWeb, ClientKindDesktop, ClientKindMobile, ClientKindTool:
		return true
	}
	return false
}

// ClientOS is the client's operating system (diagnostics only).
type ClientOS string

const (
	ClientOSWindows  ClientOS = "windows"
	ClientOSMacOS    ClientOS = "macos"
	ClientOSLinux    ClientOS = "linux"
	ClientOSIOS      ClientOS = "ios"
	ClientOSAndroid  ClientOS = "android"
	ClientOSChromeOS ClientOS = "chromeos"
	ClientOSOther    ClientOS = "other"
)

// Valid reports whether o is a known OS.
func (o ClientOS) Valid() bool {
	switch o {
	case ClientOSWindows, ClientOSMacOS, ClientOSLinux, ClientOSIOS, ClientOSAndroid, ClientOSChromeOS, ClientOSOther:
		return true
	}
	return false
}

// Role decides what a connection may do (01 §6.3).
type Role string

const (
	RoleFull      Role = "full"
	RoleViewer    Role = "viewer"
	RolePublisher Role = "publisher" // later (M2)
	RoleAgent     Role = "agent"     // later (M4)
)

// Valid reports whether r is a known role. Unknown roles in hello are rejected with bad_request.
func (r Role) Valid() bool {
	switch r {
	case RoleFull, RoleViewer, RolePublisher, RoleAgent:
		return true
	}
	return false
}

// CanPublish reports whether r may share: share.* and pc.* for the pub PC (full, publisher, agent).
func (r Role) CanPublish() bool { return r == RoleFull || r == RolePublisher || r == RoleAgent }

// CanSubscribe reports whether r may watch: subscribe.update and pc.* for the sub PC (full, viewer).
func (r Role) CanSubscribe() bool { return r == RoleFull || r == RoleViewer }

// AllowsPC reports whether r may send pc.* messages about the given PeerConnection (01 §6.3).
func (r Role) AllowsPC(pc PCKind) bool {
	switch pc {
	case PCKindPub:
		return r.CanPublish()
	case PCKindSub:
		return r.CanSubscribe()
	}
	return false
}

// Feature is a named optional capability within a protocol version (01 §6.2). A feature is active only if it is in
// welcome.features.
type Feature string

const (
	FeatureAgentRelay      Feature = "agent.relay"
	FeatureUserConnections Feature = "user.connections" // later (M2)
	FeatureSharePause      Feature = "share.pause"      // later (M2)
	FeatureLayerMid        Feature = "layer.mid"        // later (M5)
	FeatureICERefresh      Feature = "ice.refresh"      // later (TURN)
)

// Valid reports whether f is a known feature. Clients may list features this build does not know; the server
// replies with the intersection.
func (f Feature) Valid() bool {
	switch f {
	case FeatureAgentRelay, FeatureUserConnections, FeatureSharePause, FeatureLayerMid, FeatureICERefresh:
		return true
	}
	return false
}

// Caps are the client's media capabilities. Web: from RTCRtpReceiver/RTCRtpSender.getCapabilities, H.264 entries with
// packetization-mode=1 only, mapped to CodecKey (web/src/protocol/codecs.ts).
type Caps struct {
	Decode         []CodecKey `json:"decode"`
	Encode         []CodecKey `json:"encode,omitempty"`
	Simulcast      bool       `json:"simulcast,omitempty"`      // can send simulcast
	DisplayCapture bool       `json:"displayCapture,omitempty"` // can share (getDisplayMedia or native capture)
}

// Welcome is the reply to hello.
type Welcome struct {
	Protocol         int         `json:"protocol"`
	ServerVersion    string      `json:"serverVersion"`
	MinClientVersion string      `json:"minClientVersion"` // "" = no minimum
	Features         []Feature   `json:"features"`
	Limits           Limits      `json:"limits"`
	ICEServers       []ICEServer `json:"iceServers"` // M1: always [] (no STUN/TURN needed: the server has a public address)
	ConnectionID     string      `json:"connectionId"`
	ResumeToken      Secret      `json:"resumeToken"`
	Resumed          bool        `json:"resumed"`
	RoomID           string      `json:"roomId,omitempty"` // set when resumed into a room
	DefaultRoomID    string      `json:"defaultRoomId"`    // "lounge" (03 §8; changing the default room is later)
	User             UserInfo    `json:"user"`
	ServerTime       time.Time   `json:"serverTime"`
}

// UserInfo is the connection's own user.
type UserInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"` // what the UI shows: the username in M1 (03 has no display names yet)
	Admin bool   `json:"admin,omitempty"`
}

// Limits are the server's limits for this connection, sent in welcome.
type Limits struct {
	MaxMessageBytes int `json:"maxMessageBytes"` // 65536
	// MaxSDPBytes: max pc.answer (sub) message; pub offers are limited to 65536 bytes and 8 m-lines by the SFU.
	MaxSDPBytes         int   `json:"maxSdpBytes"`                   // 262144
	MaxSharesPerUser    int   `json:"maxSharesPerUser"`              // 4
	MaxRoomParticipants int   `json:"maxRoomParticipants,omitempty"` // admin soft limit (03 maxParticipantsPerRoom); 0 = none
	MaxRoomShares       int   `json:"maxRoomShares,omitempty"`       // admin soft limit (03 maxSharesPerRoom); 0 = none
	MaxVideoBitrate     int64 `json:"maxVideoBitrate,omitempty"`     // admin cap per share in bit/s (03 maxShareBitrateKbps × 1000); 0 = preset default
	MessagesPerSecond   int   `json:"messagesPerSecond"`             // 20
	MessageBurst        int   `json:"messageBurst"`                  // 100
	PingIntervalMs      int   `json:"pingIntervalMs"`                // 15000
	IdleTimeoutMs       int   `json:"idleTimeoutMs"`                 // 45000
	GraceMs             int   `json:"graceMs"`                       // 30000
}

// ICEServer is one STUN/TURN server (RTCIceServer shape). M1 sends none.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential Secret   `json:"credential,omitempty"`
}

// Ping is the client's application-level heartbeat (01 §3.4).
type Ping struct {
	T int64 `json:"t"` // sender's clock in ms, echoed back
}

// Pong answers ping.
type Pong struct {
	T            int64 `json:"t"`
	ServerTimeMs int64 `json:"serverTimeMs"`
}
