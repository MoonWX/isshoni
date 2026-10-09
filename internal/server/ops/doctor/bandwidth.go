package doctor

import (
	"math"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The bandwidth calculator (04 §13.4): one implementation for `isshoni doctor`'s bandwidth flags, the doctor
// report and the admin page (GET /api/v1/admin/bandwidth).
//
// The model is the plan's "egress ≈ Σ viewers × (focus + thumbnails × preview)". Of N people, S share. Each person
// sees the shares of the others: one in focus at full quality, up to T more as thumbnails at preview quality, and
// the audio of the focused share:
//
//	for each person v:
//	    visible = S − (1 if v shares else 0)
//	    focus   = full    if visible ≥ 1 else 0
//	    thumbs  = min(T, visible − 1) × preview      (0 if visible ≤ 1)
//	    audio   = audio   if visible ≥ 1 else 0
//	egress_media  = Σ_v (focus + thumbs + audio)
//	egress_wire   = egress_media × 1.05              (RTP, SRTP, UDP and IP headers at ~1200-byte packets)
//	ingress_media = S × (full + preview + audio)
//	transfer_per_session_bytes = egress_wire × hours × 3600 / 8
//
// The rates are whole bits per second and are added as integers, so the results carry no floating-point noise;
// megabits are rounded to two places at the end, gigabytes (10⁹ bytes) to one.

// Bitrates of the model, in bits per second.
const (
	previewBps      = 300_000 // a thumbnail: the preview layer
	audioBps        = 128_000 // the preset "auto"
	movieAudioBps   = 256_000 // the preset "movie"
	wirePercent     = 105     // egress on the wire, in percent of the media bitrate
	bitsPerMegabit  = 1_000_000
	bytesPerGB      = 1_000_000_000
	secondsPerHour  = 3600
	defaultFullBps  = 8_000_000
	centiMegabitBps = bitsPerMegabit / 100
)

// Limits of BandwidthInput: what the calculator takes (BandwidthInputProblem).
const (
	MaxBandwidthPeople     = 1000
	MaxBandwidthThumbnails = 100
	MaxBandwidthHours      = 24 * 366
)

// DefaultBandwidthInput is the session doctor estimates when it is given none: the M1 exit scenario of 04 §13.4,
// five people of whom two share at 1080p60, for two hours.
func DefaultBandwidthInput() api.BandwidthInput {
	return api.BandwidthInput{
		People: 5, Sharing: 2, Thumbnails: 8,
		Quality: api.BandwidthQuality1080p60, Preset: api.BandwidthPresetAuto, Hours: 2,
	}
}

// fullBps is the bitrate of a share's full layer at a quality; ok is false for a quality the model does not know.
func fullBps(q api.BandwidthQuality) (bps int64, ok bool) {
	switch q {
	case api.BandwidthQuality1080p60:
		return 8_000_000, true
	case api.BandwidthQuality1440p60:
		return 16_000_000, true
	case api.BandwidthQuality2160p60:
		return 32_000_000, true
	}
	return 0, false
}

// presetAudioBps is the audio bitrate of a preset.
func presetAudioBps(p api.BandwidthPreset) (bps int64, ok bool) {
	switch p {
	case api.BandwidthPresetAuto:
		return audioBps, true
	case api.BandwidthPresetMovie:
		return movieAudioBps, true
	}
	return 0, false
}

// BandwidthInputProblem names the first field of in that the calculator does not take, by its JSON name, with the
// reason as one of api's field codes; ok is true when every field is fine. The ranges: people 1 to
// MaxBandwidthPeople, sharing 0 to people, thumbnails 0 to MaxBandwidthThumbnails, a known quality and preset, and
// hours above 0 up to MaxBandwidthHours.
func BandwidthInputProblem(in api.BandwidthInput) (field, code string, ok bool) {
	_, qualityOK := fullBps(in.Quality)
	_, presetOK := presetAudioBps(in.Preset)
	switch {
	case in.People < 1 || in.People > MaxBandwidthPeople:
		return "people", api.FieldOutOfRange, false
	case in.Sharing < 0 || in.Sharing > in.People:
		return "sharing", api.FieldOutOfRange, false
	case in.Thumbnails < 0 || in.Thumbnails > MaxBandwidthThumbnails:
		return "thumbnails", api.FieldOutOfRange, false
	case !qualityOK:
		return "quality", api.FieldInvalid, false
	case !presetOK:
		return "preset", api.FieldInvalid, false
	case math.IsNaN(in.Hours) || in.Hours <= 0 || in.Hours > MaxBandwidthHours:
		return "hours", api.FieldOutOfRange, false
	}
	return "", "", true
}

// Bandwidth estimates the server's bandwidth for a session (04 §13.4). It answers every input: a value outside
// the ranges of BandwidthInputProblem is brought into them first (an unknown quality counts as 1080p60, an unknown
// preset as auto), and the estimate's Input shows what was computed. Callers that take the input from a person
// check it with BandwidthInputProblem before, so that a typo is an error and not a silent correction.
func Bandwidth(in api.BandwidthInput) api.BandwidthEstimate {
	in.People = min(max(in.People, 0), MaxBandwidthPeople)
	in.Sharing = min(max(in.Sharing, 0), in.People)
	in.Thumbnails = min(max(in.Thumbnails, 0), MaxBandwidthThumbnails)
	if math.IsNaN(in.Hours) || in.Hours < 0 {
		in.Hours = 0
	}
	in.Hours = min(in.Hours, MaxBandwidthHours)
	full, ok := fullBps(in.Quality)
	if !ok {
		in.Quality, full = api.BandwidthQuality1080p60, defaultFullBps
	}
	audio, ok := presetAudioBps(in.Preset)
	if !ok {
		in.Preset, audio = api.BandwidthPresetAuto, audioBps
	}

	// perPerson is the egress to one person who sees `visible` shares.
	perPerson := func(visible int) int64 {
		if visible < 1 {
			return 0
		}
		thumbs := int64(min(in.Thumbnails, visible-1))
		return full + thumbs*previewBps + audio
	}
	n, s := int64(in.People), int64(in.Sharing)
	sharer, viewer := perPerson(in.Sharing-1), perPerson(in.Sharing) // a sharer does not receive its own share
	egress := s*sharer + (n-s)*viewer
	wire := egress * wirePercent / 100
	ingress := s * (full + previewBps + audio)
	transferBytes := float64(wire) * in.Hours * secondsPerHour / 8

	return api.BandwidthEstimate{
		Input:                in,
		PerViewerMbps:        api.BandwidthPerViewer{Sharer: mbps(sharer), Viewer: mbps(viewer)},
		EgressMediaMbps:      mbps(egress),
		EgressWireMbps:       mbps(wire),
		IngressMediaMbps:     mbps(ingress),
		TransferPerSessionGB: math.Round(transferBytes/(bytesPerGB/10)) / 10,
	}
}

// mbps converts bits per second to megabits per second, rounded to two places.
func mbps(bps int64) float64 {
	return float64((bps+centiMegabitBps/2)/centiMegabitBps) / 100
}
