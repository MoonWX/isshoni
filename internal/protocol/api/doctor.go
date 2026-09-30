package api

import "time"

// This file holds 04's doctor report and bandwidth calculator types (04 §13.4–§13.5), used by doctor --json,
// POST /api/v1/admin/doctor, the admin socket's POST /v1/doctor and GET /api/v1/admin/bandwidth.

// DoctorReportSchema is DoctorReport.Schema. It increases only on breaking changes; within a schema fields are only
// added.
const DoctorReportSchema = 1

// DoctorStatus is the result of one check (04 §13.2).
type DoctorStatus string

const (
	DoctorStatusOK   DoctorStatus = "ok"
	DoctorStatusWarn DoctorStatus = "warn"
	DoctorStatusFail DoctorStatus = "fail"
	DoctorStatusSkip DoctorStatus = "skip"
	DoctorStatusInfo DoctorStatus = "info"
)

// DoctorMode says where the checks ran (04 §13.1).
type DoctorMode string

const (
	DoctorModeServer        DoctorMode = "server"          // inside the server (dashboard, POST /api/v1/admin/doctor)
	DoctorModeCLIWithServer DoctorMode = "cli-with-server" // the CLI asked the running server over the admin socket
	DoctorModeCLIOffline    DoctorMode = "cli-offline"     // no server: the CLI ran the checks itself
)

// DoctorReport is one doctor run (04 §13.5). Message and Fix in each check are English for the CLI; 05 renders
// Code, Params and FixCode from its catalog.
type DoctorReport struct {
	Schema    int                `json:"schema"` // DoctorReportSchema
	Version   string             `json:"version"`
	RanAt     time.Time          `json:"ranAt"`
	Mode      DoctorMode         `json:"mode"`
	Summary   DoctorCounts       `json:"summary"`
	Env       DoctorEnv          `json:"env"`
	Checks    []DoctorCheck      `json:"checks"`
	Bandwidth *BandwidthEstimate `json:"bandwidth,omitempty"`
}

// DoctorCounts is DoctorReport.Summary: the number of checks per status.
type DoctorCounts struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
	Info int `json:"info"`
}

// DoctorEnv describes the host doctor ran on.
type DoctorEnv struct {
	OS        string        `json:"os"`
	Arch      string        `json:"arch"`
	Kernel    string        `json:"kernel"`
	Container ContainerKind `json:"container"`
	Systemd   bool          `json:"systemd"`
	Provider  CloudProvider `json:"provider"`
	User      string        `json:"user"`
}

// DoctorCheck is one check's result. ID is one of the check ids of 04 §13.2 (doctor.CheckIDs).
type DoctorCheck struct {
	ID         string         `json:"id"`
	Status     DoctorStatus   `json:"status"`
	Code       string         `json:"code"` // message template key, e.g. "udp_buffers.low"
	Params     map[string]any `json:"params,omitempty"`
	Message    string         `json:"message"` // English, for the CLI
	FixCode    string         `json:"fixCode,omitempty"`
	Fix        string         `json:"fix,omitempty"` // English, for the CLI
	LocalOnly  bool           `json:"localOnly"`
	DurationMs int64          `json:"durationMs"`
}

// BandwidthQuality is the full-layer quality the calculator assumes (04 §13.4).
type BandwidthQuality string

const (
	BandwidthQuality1080p60 BandwidthQuality = "1080p60" // 8 Mbps
	BandwidthQuality1440p60 BandwidthQuality = "1440p60" // 16 Mbps
	BandwidthQuality2160p60 BandwidthQuality = "2160p60" // 32 Mbps
)

// BandwidthPreset is the sharing preset the calculator assumes: it sets the audio bitrate.
type BandwidthPreset string

const (
	BandwidthPresetAuto  BandwidthPreset = "auto"  // 128 kbps audio
	BandwidthPresetMovie BandwidthPreset = "movie" // 256 kbps audio
)

// BandwidthInput is the calculator's input: doctor's flags and the query of GET /api/v1/admin/bandwidth.
type BandwidthInput struct {
	People     int              `json:"people"`
	Sharing    int              `json:"sharing"`
	Thumbnails int              `json:"thumbnails"`
	Quality    BandwidthQuality `json:"quality"`
	Preset     BandwidthPreset  `json:"preset"`
	Hours      float64          `json:"hours"`
}

// BandwidthEstimate is the calculator's output (04 §13.4): GET /api/v1/admin/bandwidth and DoctorReport.Bandwidth.
type BandwidthEstimate struct {
	Input                BandwidthInput     `json:"input"`
	PerViewerMbps        BandwidthPerViewer `json:"perViewerMbps"`
	EgressMediaMbps      float64            `json:"egressMediaMbps"`
	EgressWireMbps       float64            `json:"egressWireMbps"`
	IngressMediaMbps     float64            `json:"ingressMediaMbps"`
	TransferPerSessionGB float64            `json:"transferPerSessionGb"` // 1 GB = 10⁹ bytes, on the wire
}

// BandwidthPerViewer is the egress to one person, depending on whether that person shares too.
type BandwidthPerViewer struct {
	Sharer float64 `json:"sharer"`
	Viewer float64 `json:"viewer"`
}
