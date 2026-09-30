package api

import "time"

// This file holds 04's admin dashboard types (04 §11.4) and the admin socket's server status (04 §12.2).

// TLSMode is the effective tls.mode (04 §8.1).
type TLSMode string

const (
	TLSModeAuto   TLSMode = "auto"
	TLSModeIP     TLSMode = "ip"
	TLSModeManual TLSMode = "manual"
	TLSModeOff    TLSMode = "off"
)

// AlertCode names a dashboard alert (04 §11.4); 05 renders it from its catalog with Alert.Params.
type AlertCode string

const (
	AlertCodeTransfer80            AlertCode = "transfer.80"
	AlertCodeTransfer100           AlertCode = "transfer.100"
	AlertCodeReleaseUpdate         AlertCode = "release.update"
	AlertCodeReleaseSecurityUpdate AlertCode = "release.security_update"
	AlertCodePublicIPChanged       AlertCode = "public_ip.changed"
	AlertCodeTLSRenewalFailing     AlertCode = "tls.renewal_failing"
	AlertCodeDoctorFail            AlertCode = "doctor.fail"
	AlertCodeDashboardSourceFailed AlertCode = "dashboard.source_failed"
)

// AlertSeverity is how prominently the dashboard shows an alert.
type AlertSeverity string

const (
	AlertSeverityInfo  AlertSeverity = "info"
	AlertSeverityWarn  AlertSeverity = "warn"
	AlertSeverityError AlertSeverity = "error"
)

// OpsDashboard is GET /api/v1/admin/dashboard (04 §11.4). A failing source yields an empty section (an empty list,
// zero totals, or an absent pointer) plus an alert dashboard.source_failed, never a failed response.
type OpsDashboard struct {
	GeneratedAt time.Time            `json:"generatedAt"`
	Server      ServerInfo           `json:"server"`
	Transfer    TransferInfo         `json:"transfer"`
	Media       MediaTotals          `json:"media"`
	Rooms       []RoomLive           `json:"rooms"`
	Clients     []ClientVersionCount `json:"clients"`
	Doctor      *DoctorSummary       `json:"doctor,omitempty"` // absent until the first doctor run
	Alerts      []Alert              `json:"alerts"`
	Accounts    *DashboardAccounts   `json:"accounts,omitempty"` // 03's part; absent when its source failed
}

// ServerInfo is OpsDashboard.Server.
type ServerInfo struct {
	Version    string           `json:"version"`
	StartedAt  time.Time        `json:"startedAt"`
	Origin     string           `json:"origin"`
	Process    ProcessInfo      `json:"process"`
	TLS        TLSInfo          `json:"tls"`
	PublicIPv4 string           `json:"publicIpv4"` // "" when none is known
	PublicIPv6 string           `json:"publicIpv6"`
	NAT        NATKind          `json:"nat"`
	Advertised []AdvertisedAddr `json:"advertised"`
	Update     *UpdateInfo      `json:"update,omitempty"` // absent before the first release check or when it is off
}

// ProcessInfo is ServerInfo.Process.
type ProcessInfo struct {
	CPUSeconds float64 `json:"cpuSeconds"`
	RSSBytes   int64   `json:"rssBytes"`
	NumCPU     int     `json:"numCpu"`
	Goroutines int     `json:"goroutines"`
}

// TLSInfo is the certificate state (04 §8.2 tlsmgr.Status without the English error text).
type TLSInfo struct {
	Mode          TLSMode   `json:"mode"`
	Names         []string  `json:"names"`
	Ready         bool      `json:"ready"`
	Issuer        string    `json:"issuer,omitempty"`
	NotBefore     time.Time `json:"notBefore,omitzero"`
	NotAfter      time.Time `json:"notAfter,omitzero"`
	NextRenewal   time.Time `json:"nextRenewal,omitzero"`
	LastErrorCode string    `json:"lastErrorCode,omitempty"` // an ACME hint code of 04 §8.7, e.g. "tls.dns_wrong"
	LastErrorAt   time.Time `json:"lastErrorAt,omitzero"`
}

// AdvertisedAddr is one address the server advertises in ICE candidates.
type AdvertisedAddr struct {
	Proto string    `json:"proto"` // "udp" | "tcp"
	Addr  string    `json:"addr"`  // host:port
	Via   Transport `json:"via"`
}

// UpdateInfo is the result of the daily release check (04 §11.5).
type UpdateInfo struct {
	Latest    string    `json:"latest"`
	URL       string    `json:"url"`
	Security  bool      `json:"security"` // a newer release is marked as a security release
	CheckedAt time.Time `json:"checkedAt"`
}

// TransferInfo is month-to-date transfer, counted at the socket layer, per calendar month in UTC (04 §11.3).
type TransferInfo struct {
	Month                string `json:"month"` // "2026-09"
	EgressBytes          int64  `json:"egressBytes"`
	IngressBytes         int64  `json:"ingressBytes"`
	AlertGB              int    `json:"alertGb"`                        // the setting transferAlertGb; 0 = off
	ProjectedEgressBytes int64  `json:"projectedEgressBytes,omitempty"` // shown after day 3 of the month
	EgressBps            int64  `json:"egressBps"`
	IngressBps           int64  `json:"ingressBps"`
}

// MediaTotals is OpsDashboard.Media, from the SFU's totals (probe PCs excluded).
type MediaTotals struct {
	IngressBps      int64           `json:"ingressBps"`
	EgressBps       int64           `json:"egressBps"`
	DownTracks      int             `json:"downTracks"`
	PeerConnections TransportCounts `json:"peerConnections"`
}

// TransportCounts counts peer connections by selected transport.
type TransportCounts struct {
	UDP     int `json:"udp"`
	TCP443  int `json:"tcp443"`
	TCP7882 int `json:"tcp7882"`
}

// RoomLive is one room with its live participants and shares.
type RoomLive struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Participants []ParticipantLive `json:"participants"`
	Shares       []ShareLive       `json:"shares"`
}

// ParticipantLive is one user in a room.
type ParticipantLive struct {
	UserID      string           `json:"userId"`
	Username    string           `json:"username"`
	Connections []ConnectionLive `json:"connections"`
	Watching    []string         `json:"watching"` // share IDs this user subscribes to
}

// ConnectionLive is one signaling connection. Transport and RTTMs come from the SFU, the rest from the hub.
type ConnectionLive struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`    // 01's ClientKind
	Role        string    `json:"role"`    // 01's Role
	Version     string    `json:"version"` // client version
	OS          string    `json:"os"`
	Transport   Transport `json:"transport,omitempty"` // absent until a PC has a selected pair
	RTTMs       int       `json:"rttMs"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// ShareLive is one share as the SFU sees it. Share labels are not shown.
type ShareLive struct {
	ShareID     string       `json:"shareId"`
	OwnerUserID string       `json:"ownerUserId"`
	Kind        string       `json:"kind"` // 01's share source kind, e.g. "window"
	StartedAt   time.Time    `json:"startedAt"`
	Codec       string       `json:"codec"` // "h264/" + profile, e.g. "h264/640c"
	Layers      []LayerLive  `json:"layers"`
	Viewers     ViewerCounts `json:"viewers"`
	IngressBps  int64        `json:"ingressBps"`
	EgressBps   int64        `json:"egressBps"`
}

// LayerLive is one simulcast layer of a share.
type LayerLive struct {
	RID     string  `json:"rid"` // "f" | "q"
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	FPS     float64 `json:"fps"`
	Bitrate int64   `json:"bitrate"` // bits/s
	LossPct float64 `json:"lossPct"`
}

// ViewerCounts counts a share's viewers by forwarded quality.
type ViewerCounts struct {
	High  int `json:"high"`
	Low   int `json:"low"`
	Audio int `json:"audio"`
}

// ClientVersionCount counts connected clients per kind and version.
type ClientVersionCount struct {
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	Count    int    `json:"count"`
	Outdated bool   `json:"outdated"` // below minClientVersion or older than this server
}

// DoctorSummary is OpsDashboard.Doctor: the counts of the last doctor run.
type DoctorSummary struct {
	RanAt time.Time `json:"ranAt"`
	OK    int       `json:"ok"`
	Warn  int       `json:"warn"`
	Fail  int       `json:"fail"`
}

// Alert is one dashboard alert.
type Alert struct {
	Code     AlertCode      `json:"code"`
	Severity AlertSeverity  `json:"severity"`
	Params   map[string]any `json:"params,omitempty"`
}

// ServerStatus is the admin socket's GET /v1/status (04 §12.2, isshoni admin status --json) and doctor's view of a
// running server (doctor.Env.Live): version, uptime, site, TLS, public addresses, listeners, live counts, schema
// version, and the transfer and release state that doctor's transfer and release checks need.
type ServerStatus struct {
	Version       string           `json:"version"`
	StartedAt     time.Time        `json:"startedAt"`
	UptimeS       int64            `json:"uptimeS"`
	Origin        string           `json:"origin"`
	TLS           TLSInfo          `json:"tls"`
	PublicIPv4    string           `json:"publicIpv4"`
	PublicIPv6    string           `json:"publicIpv6"`
	NAT           NATKind          `json:"nat"`
	Advertised    []AdvertisedAddr `json:"advertised"`
	Listeners     []ListenerInfo   `json:"listeners"`
	Rooms         int              `json:"rooms"`
	Participants  int              `json:"participants"`
	Shares        int              `json:"shares"`
	SchemaVersion int              `json:"schemaVersion"`
	Transfer      *TransferInfo    `json:"transfer,omitempty"`
	Update        *UpdateInfo      `json:"update,omitempty"`
}

// ListenerInfo is one bound listener of a running server.
type ListenerInfo struct {
	Key     string `json:"key"`     // its config key: listen.https, listen.http, listen.ice_udp, listen.ice_tcp, metrics.listen, listen.admin_socket
	Network string `json:"network"` // "tcp" | "udp" | "unix"
	Addr    string `json:"addr"`    // the bound address or socket path
}
