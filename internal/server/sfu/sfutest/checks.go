package sfutest

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// Frame is one video frame: the packets that share an RTP timestamp.
type Frame struct {
	TS       uint32
	Mark     fake.Marker // the frame's marker, when a packet has one
	HasMark  bool
	Marks    int  // packets with a marker: 1 when the frame is intact
	KeyStart bool // the frame's first packet (lowest sequence number) starts a keyframe
	// FirstSeq and LastSeq are the lowest and highest sequence numbers (int16 order).
	FirstSeq, LastSeq uint16
	Packets, Bytes    int
	MarkerBits        int  // packets with the RTP marker bit
	MarkerOnLast      bool // the packet with LastSeq has the marker bit
	First, Last       time.Time
}

// Frames groups video packets into frames in the order the frames began to arrive. Duplicates are skipped.
func Frames(pkts []Packet) []Frame {
	var frames []Frame
	index := map[uint32]int{}
	firstKey := map[uint32]bool{}
	for _, p := range pkts {
		if p.Dup {
			continue
		}
		i, ok := index[p.TS]
		if !ok {
			i = len(frames)
			index[p.TS] = i
			frames = append(frames, Frame{TS: p.TS, FirstSeq: p.Seq, LastSeq: p.Seq, First: p.Arrival})
			firstKey[p.TS] = p.KeyStart
		}
		f := &frames[i]
		if int16(p.Seq-f.FirstSeq) < 0 {
			f.FirstSeq = p.Seq
			firstKey[p.TS] = p.KeyStart
		}
		if int16(p.Seq-f.LastSeq) > 0 {
			f.LastSeq = p.Seq
			f.MarkerOnLast = p.Marker
		} else if p.Seq == f.LastSeq {
			f.MarkerOnLast = p.Marker
		}
		if p.HasMark {
			f.Marks++
			f.Mark, f.HasMark = p.Mark, true
		}
		if p.Marker {
			f.MarkerBits++
		}
		f.Packets++
		f.Bytes += p.Size
		f.Last = p.Arrival
	}
	for i := range frames {
		frames[i].KeyStart = firstKey[frames[i].TS]
	}
	return frames
}

// WithoutPartialTail drops the packets of the newest video frame when its last packet (the one with the RTP marker
// bit) hasn't arrived yet: a test that takes its snapshot while the stream still runs can land inside a frame.
func WithoutPartialTail(pkts []Packet) []Packet {
	frames := Frames(pkts)
	if len(frames) == 0 {
		return pkts
	}
	last := frames[len(frames)-1]
	if last.MarkerBits > 0 && last.MarkerOnLast {
		return pkts
	}
	out := make([]Packet, 0, len(pkts))
	for _, p := range pkts {
		if p.TS != last.TS {
			out = append(out, p)
		}
	}
	return out
}

// WithoutCutFrames drops the packets of every video frame that a layer switch cut short: a frame whose last packet
// (the one with the RTP marker bit) didn't arrive, right before a frame of another layer. An SFU switches on the
// first packet of the new layer's keyframe (02 §9.4). The two layers are read by different goroutines, so that
// packet can reach a viewer's queue between two packets of the old layer's current frame; the rest of that frame is
// then no longer forwarded, and a decoder drops what it got of it. The sequence numbers stay continuous
// (CheckContinuous sees the cut frame's packets too); only the checks of whole frames must leave it out.
func WithoutCutFrames(pkts []Packet) []Packet {
	frames := Frames(pkts)
	cut := map[uint32]bool{}
	rid, known := "", false // the layer of the newest frame with a marker
	for i, f := range frames {
		if f.HasMark {
			rid, known = f.Mark.RID, true
		}
		whole := f.MarkerBits > 0 && f.MarkerOnLast
		if whole || !known || i+1 == len(frames) {
			continue
		}
		if next := frames[i+1]; next.HasMark && next.Mark.RID != rid {
			cut[f.TS] = true
		}
	}
	if len(cut) == 0 {
		return pkts
	}
	out := make([]Packet, 0, len(pkts))
	for _, p := range pkts {
		if !cut[p.TS] {
			out = append(out, p)
		}
	}
	return out
}

// CheckContinuous checks that the packets form one continuous stream (the S4 check): in sequence-number order, no
// sequence number is missing or received twice, and timestamps never go back.
func CheckContinuous(pkts []Packet) error {
	if len(pkts) == 0 {
		return errors.New("no packets")
	}
	type ext struct {
		n  int64
		ts uint32
	}
	all := make([]ext, 0, len(pkts))
	last := int64(extOrigin) + int64(pkts[0].Seq)
	for _, p := range pkts {
		if p.Dup {
			return fmt.Errorf("seq %d received twice", p.Seq)
		}
		n := last + int64(int16(p.Seq-uint16(last)))
		all = append(all, ext{n, p.TS})
		last = max(last, n)
	}
	slices.SortStableFunc(all, func(a, b ext) int { return int(a.n - b.n) })
	for i := 1; i < len(all); i++ {
		switch d := all[i].n - all[i-1].n; {
		case d == 0:
			return fmt.Errorf("seq %d received twice", uint16(all[i].n))
		case d > 1:
			return fmt.Errorf("seq %d–%d missing", uint16(all[i-1].n+1), uint16(all[i].n-1))
		}
		if int32(all[i].ts-all[i-1].ts) < 0 {
			return fmt.Errorf("timestamp goes back at seq %d: %d → %d", uint16(all[i].n), all[i-1].ts, all[i].ts)
		}
	}
	return nil
}

// CheckStartsOnSPS checks that the video stream starts on an SPS and that every layer switch (a change of the
// marker's layer between two frames) starts on one (S4, 02 §9.4).
func CheckStartsOnSPS(pkts []Packet) error {
	frames := Frames(pkts)
	if len(frames) == 0 {
		return errors.New("no frames")
	}
	if !frames[0].KeyStart {
		return fmt.Errorf("the stream starts on seq %d without an SPS", frames[0].FirstSeq)
	}
	for i := 1; i < len(frames); i++ {
		prev, f := frames[i-1], frames[i]
		if prev.HasMark && f.HasMark && prev.Mark.RID != f.Mark.RID && !f.KeyStart {
			return fmt.Errorf("switch %s → %s at seq %d without an SPS", prev.Mark.RID, f.Mark.RID, f.FirstSeq)
		}
	}
	return nil
}

// CheckVideoMarkers checks that every frame of fake video arrived intact: one marker per frame, whose keyframe flag
// matches an SPS at the frame's start, the RTP marker bit exactly on its last packet, and, within each run of one
// layer, consecutive frame indices and timestamps that follow the capture times at 90 kHz (±1 tick of rounding).
func CheckVideoMarkers(pkts []Packet) error {
	frames := Frames(pkts)
	if len(frames) == 0 {
		return errors.New("no frames")
	}
	for i, f := range frames {
		switch {
		case f.Marks != 1:
			return fmt.Errorf("frame at seq %d has %d markers, want 1", f.FirstSeq, f.Marks)
		case f.Mark.Keyframe != f.KeyStart:
			return fmt.Errorf("frame %s/%d: keyframe flag %v but SPS at start %v", f.Mark.RID, f.Mark.Frame,
				f.Mark.Keyframe, f.KeyStart)
		case f.MarkerBits != 1 || !f.MarkerOnLast:
			return fmt.Errorf("frame %s/%d: %d RTP marker bits, on the last packet %v", f.Mark.RID, f.Mark.Frame,
				f.MarkerBits, f.MarkerOnLast)
		}
		if i == 0 || frames[i-1].Mark.RID != f.Mark.RID {
			continue
		}
		if err := checkStep(frames[i-1].Mark, f.Mark, frames[i-1].TS, f.TS, 90000); err != nil {
			return err
		}
	}
	return nil
}

// CheckAudioMarkers checks that every packet of fake audio carries its marker, with consecutive packet indices and
// timestamps that follow the capture times at 48 kHz (±1 tick).
func CheckAudioMarkers(pkts []Packet) error {
	if len(pkts) == 0 {
		return errors.New("no packets")
	}
	var prev *Packet
	for i := range pkts {
		p := &pkts[i]
		if p.Dup {
			continue
		}
		if !p.HasMark {
			return fmt.Errorf("audio seq %d has no marker", p.Seq)
		}
		if prev != nil {
			if err := checkStep(prev.Mark, p.Mark, prev.TS, p.TS, 48000); err != nil {
				return err
			}
		}
		prev = p
	}
	return nil
}

// checkStep checks that b is the frame or packet after a and that its timestamp advanced by the capture time.
func checkStep(a, b fake.Marker, tsA, tsB uint32, clock int64) error {
	if b.Frame != a.Frame+1 {
		return fmt.Errorf("%q: frame %d follows frame %d", b.RID, b.Frame, a.Frame)
	}
	want := ((b.CaptureNS-a.CaptureNS)*clock + int64(time.Second)/2) / int64(time.Second)
	if got := int64(int32(tsB - tsA)); got < want-1 || got > want+1 {
		return fmt.Errorf("%q frame %d: timestamp +%d, capture time says +%d", b.RID, b.Frame, got, want)
	}
	return nil
}

// GOPBitrate returns the payload bitrate of fake video over whole GOPs: the bytes of the frames from the first
// keyframe up to (not including) the last one, over their capture-time distance. It needs markers and two keyframes.
// RTP payloads differ from the Source's access units only by the FU-A and STAP-A headers and the missing start
// codes, well under 1% at any realistic rate.
func GOPBitrate(frames []Frame) (float64, error) {
	first, last := -1, -1
	for i, f := range frames {
		if f.HasMark && f.Mark.Keyframe {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last == first {
		return 0, errors.New("fewer than two keyframes")
	}
	bytes := 0
	for _, f := range frames[first:last] {
		bytes += f.Bytes
	}
	d := frames[last].Mark.CaptureNS - frames[first].Mark.CaptureNS
	if d <= 0 {
		return 0, errors.New("keyframes out of capture order")
	}
	return float64(bytes) * 8 / time.Duration(d).Seconds(), nil
}

// AVSample is one flash/beep pair.
type AVSample struct {
	Layer  string        // the video layer
	Frame  uint32        // the flash frame's index
	Offset time.Duration // flash − beep, both mapped to NTP through the sender reports
}

// AVOffset pairs every flash frame of a stream's video tracks with the beep of the same capture instant on its audio
// track and returns flash − beep for each pair. Each packet's RTP timestamp is mapped to NTP through the last sender
// report its track received before the packet (or the first one after, early on). stream is the tracks' msid stream
// (the SFU's ShareID).
func (v *Viewer) AVOffset(stream string) ([]AVSample, error) {
	var video []*Recorder
	var audio *Recorder
	for _, r := range v.Recorders() {
		if r.stream != stream {
			continue
		}
		switch r.kind {
		case webrtc.RTPCodecTypeVideo:
			video = append(video, r)
		case webrtc.RTPCodecTypeAudio:
			if audio != nil {
				return nil, fmt.Errorf("stream %q has two audio tracks", stream)
			}
			audio = r
		}
	}
	if audio == nil || len(video) == 0 {
		return nil, fmt.Errorf("stream %q needs a video and an audio track", stream)
	}
	aSRs := audio.SRs()
	beeps := map[int64]int64{} // capture ns → NTP ns
	for _, p := range audio.Packets() {
		if !p.HasMark || !p.Mark.Beep || p.Dup {
			continue
		}
		if ntp, ok := mapNTP(aSRs, p.TS, p.Arrival, 48000); ok {
			beeps[p.Mark.CaptureNS] = ntp
		}
	}
	var out []AVSample
	for _, r := range video {
		srs := r.SRs()
		for _, f := range Frames(r.Packets()) {
			if !f.HasMark || !f.Mark.Flash {
				continue
			}
			beep, ok := beeps[f.Mark.CaptureNS]
			if !ok {
				continue
			}
			if flash, ok := mapNTP(srs, f.TS, f.First, 90000); ok {
				out = append(out, AVSample{Layer: f.Mark.RID, Frame: f.Mark.Frame, Offset: time.Duration(flash - beep)})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("stream %q: no flash and beep pair with sender reports", stream)
	}
	return out, nil
}

// mapNTP maps an RTP timestamp that arrived at `at` to NTP nanoseconds through the last SR before it (or the first
// SR, when none came before).
func mapNTP(srs []SenderReport, ts uint32, at time.Time, clock int64) (int64, bool) {
	if len(srs) == 0 {
		return 0, false
	}
	sr := srs[0]
	for _, s := range srs {
		if s.Arrival.After(at) {
			break
		}
		sr = s
	}
	return ntpNanos(sr.NTP) + int64(int32(ts-sr.RTP))*int64(time.Second)/clock, true
}

// ntpEpochOffset is the number of seconds from 1900-01-01 (NTP) to 1970-01-01 (Unix).
const ntpEpochOffset = 2208988800

// ntpNanos converts a 32.32 NTP time to nanoseconds since 1900.
func ntpNanos(ntp uint64) int64 {
	return int64(ntp>>32)*int64(time.Second) + int64((ntp&0xffffffff)*uint64(time.Second)>>32)
}
