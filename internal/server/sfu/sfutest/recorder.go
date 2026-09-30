package sfutest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// Packet is one received RTP packet as a Recorder keeps it (no payload).
type Packet struct {
	Seq     uint16
	TS      uint32
	PT      uint8
	Marker  bool // the RTP marker bit
	Size    int  // payload bytes
	Arrival time.Time
	RTX     bool // Pion unwrapped it from the RTX stream
	Dup     bool // a copy of a sequence number already received
	// Video only.
	NAL      uint8 // the NAL unit type of the payload's first byte: 1 or 5 (slice), 7 (SPS), 24 (STAP-A), 28 (FU-A)…
	KeyStart bool  // the packet carries an SPS (single NAL unit or STAP-A), where a decoder can start (02 §9.1)
	// Mark is the fake media's marker: in a video frame's slice start (single NAL unit or first FU-A fragment), or in
	// every synthetic audio packet.
	Mark    fake.Marker
	HasMark bool
}

// SenderReport is one received sender report.
type SenderReport struct {
	Arrival         time.Time
	NTP             uint64 // 32.32 fixed point since 1900
	RTP             uint32
	Packets, Octets uint32
}

// Time returns the SR's NTP time as wall-clock time.
func (s SenderReport) Time() time.Time {
	return time.Unix(0, ntpNanos(s.NTP)-ntpEpochOffset*int64(time.Second))
}

// RecorderStats are a Recorder's counters over every packet, also those it no longer keeps.
type RecorderStats struct {
	Packets    int // RTP packets received, duplicates included
	Bytes      int // their payload bytes
	Duplicates int // packets whose sequence number was already received
	RTX        int // packets unwrapped from RTX
	Expected   int // highest − first sequence number + 1
	// Gaps counts sequence numbers found missing when a later one arrived; Recovered those that arrived afterwards
	// (RecoveredRTX of them through RTX); Lost those still missing after FinalLossAfter (the final loss).
	Gaps, Recovered, RecoveredRTX, Lost int
	Keyframes                           int // video: packets that start a keyframe
	SRs                                 int // sender reports received
	PLIs                                int // PLIs the Viewer sent for this track
	First, Last                         time.Time
}

// Recorder records one received track.
type Recorder struct {
	kind   webrtc.RTPCodecType
	mid    string
	rid    string
	stream string
	id     string
	ssrc   uint32
	codec  webrtc.RTPCodecParameters
	keep   int

	mu    sync.Mutex // guards the fields below
	pkts  []Packet
	srs   []SenderReport
	stats RecorderStats
	seq   seqTracker
}

func newRecorder(tr *webrtc.TrackRemote, mid string, keep int) *Recorder {
	return &Recorder{
		kind: tr.Kind(), mid: mid, rid: tr.RID(), stream: tr.StreamID(), id: tr.ID(), ssrc: uint32(tr.SSRC()),
		codec: tr.Codec(), keep: keep,
	}
}

// Kind, MID, RID, StreamID, TrackID, SSRC and Codec describe the track. StreamID and TrackID are the sender's msid
// (the SFU sends ShareID and "v-"/"a-" + ShareID); RID is set for a simulcast layer received from a publisher.
func (r *Recorder) Kind() webrtc.RTPCodecType        { return r.kind }
func (r *Recorder) MID() string                      { return r.mid }
func (r *Recorder) RID() string                      { return r.rid }
func (r *Recorder) StreamID() string                 { return r.stream }
func (r *Recorder) TrackID() string                  { return r.id }
func (r *Recorder) SSRC() uint32                     { return r.ssrc }
func (r *Recorder) Codec() webrtc.RTPCodecParameters { return r.codec }

// Packets returns a copy of the kept packets, in arrival order.
func (r *Recorder) Packets() []Packet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pkts)
}

// SRs returns a copy of the received sender reports, in arrival order.
func (r *Recorder) SRs() []SenderReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.srs)
}

// Stats returns the counters. Sequence numbers missing for FinalLossAfter count as lost by now.
func (r *Recorder) Stats() RecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq.expire(time.Now(), &r.stats)
	s := r.stats
	s.Expected = r.seq.expected()
	return s
}

// Wait polls cond (called without the Recorder's lock) until it is true or ctx ends.
func (r *Recorder) Wait(ctx context.Context, cond func(*Recorder) bool) error {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for !cond(r) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sfutest: %s track %s/%s: %w", r.kind, r.mid, r.rid, ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// record adds one received packet.
func (r *Recorder) record(pkt *rtp.Packet, at time.Time, rtx bool) {
	p := Packet{
		Seq: pkt.SequenceNumber, TS: pkt.Timestamp, PT: pkt.PayloadType, Marker: pkt.Marker, Size: len(pkt.Payload),
		Arrival: at, RTX: rtx,
	}
	switch r.kind {
	case webrtc.RTPCodecTypeVideo:
		classifyH264(&p, pkt.Payload)
	case webrtc.RTPCodecTypeAudio:
		p.Mark, p.HasMark = fake.ParseAudioMarker(pkt.Payload)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p.Dup = r.seq.add(p.Seq, at, rtx, &r.stats)
	r.stats.Packets++
	r.stats.Bytes += p.Size
	if p.Dup {
		r.stats.Duplicates++
	}
	if rtx {
		r.stats.RTX++
	}
	if p.KeyStart {
		r.stats.Keyframes++
	}
	if r.stats.First.IsZero() {
		r.stats.First = at
	}
	r.stats.Last = at
	if r.keep > 0 {
		if len(r.pkts) >= r.keep {
			r.pkts = slices.Delete(r.pkts, 0, len(r.pkts)-r.keep/2)
		}
		r.pkts = append(r.pkts, p)
	}
}

// recordSR adds one sender report.
func (r *Recorder) recordSR(sr *rtcp.SenderReport, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srs = append(r.srs, SenderReport{
		Arrival: at, NTP: sr.NTPTime, RTP: sr.RTPTime, Packets: sr.PacketCount, Octets: sr.OctetCount,
	})
	r.stats.SRs++
}

// H.264 RTP payload structure (RFC 6184).
const (
	nalSlice  = 1
	nalIDR    = 5
	nalSPS    = 7
	nalSTAPA  = 24
	nalFUA    = 28
	nalMask   = 0x1f
	fuStart   = 0x80
	stapAHead = 1
	fuAHead   = 2
)

// classifyH264 fills a video Packet's NAL, KeyStart and Mark from its payload.
func classifyH264(p *Packet, payload []byte) {
	if len(payload) == 0 {
		return
	}
	p.NAL = payload[0] & nalMask
	switch p.NAL {
	case nalSlice, nalIDR:
		p.Mark, p.HasMark = fake.ParseVideoMarker(payload[1:])
	case nalSPS:
		p.KeyStart = true
	case nalSTAPA:
		p.KeyStart = stapAHasSPS(payload)
	case nalFUA:
		if len(payload) > fuAHead && payload[1]&fuStart != 0 {
			if t := payload[1] & nalMask; t == nalSlice || t == nalIDR {
				p.Mark, p.HasMark = fake.ParseVideoMarker(payload[fuAHead:])
			}
		}
	}
}

// stapAHasSPS walks a STAP-A's aggregation units (bounds-checked) looking for an SPS, like the SFU's h264.go.
func stapAHasSPS(payload []byte) bool {
	for i := stapAHead; i+2 < len(payload); {
		size := int(payload[i])<<8 | int(payload[i+1])
		i += 2
		if size == 0 || size > len(payload)-i {
			return false
		}
		if payload[i]&nalMask == nalSPS {
			return true
		}
		i += size
	}
	return false
}

// seqTracker unwraps sequence numbers and follows missing ones until they arrive or count as lost.
type seqTracker struct {
	started    bool
	first, max int64               // extended sequence numbers
	seen       [seenWindow]int64   // ext+1 of the packet last seen in each slot; 0 = empty
	missing    map[int64]time.Time // ext → when it went missing
	queue      []int64             // missing sequence numbers in the order they went missing
}

const (
	seenWindow = 1 << 13
	// maxMissing bounds how many missing sequence numbers one jump may add; the rest count as lost at once.
	maxMissing = 4096
	extOrigin  = 1 << 32
)

// add records a sequence number and reports whether it is a duplicate.
func (s *seqTracker) add(seq uint16, at time.Time, rtx bool, st *RecorderStats) (dup bool) {
	if !s.started {
		s.started = true
		s.first = extOrigin + int64(seq)
		s.max = s.first
		s.missing = map[int64]time.Time{}
		s.seen[s.first%seenWindow] = s.first + 1
		return false
	}
	ext := s.max + int64(int16(seq-uint16(s.max)))
	slot := ext % seenWindow
	if s.seen[slot] == ext+1 {
		return true
	}
	s.seen[slot] = ext + 1
	switch {
	case ext > s.max:
		s.markMissing(s.max+1, ext, at, st)
		s.max = ext
	case ext < s.first:
		s.markMissing(ext+1, s.first, at, st)
		s.first = ext
	default:
		if _, ok := s.missing[ext]; ok {
			delete(s.missing, ext)
			st.Recovered++
			if rtx {
				st.RecoveredRTX++
			}
		}
	}
	s.expire(at, st)
	return false
}

// markMissing records [from, to) as missing.
func (s *seqTracker) markMissing(from, to int64, at time.Time, st *RecorderStats) {
	if to <= from {
		return
	}
	st.Gaps += int(to - from)
	if to-from > maxMissing {
		st.Lost += int(to - from - maxMissing)
		from = to - maxMissing
	}
	for m := from; m < to; m++ {
		s.missing[m] = at
		s.queue = append(s.queue, m)
	}
}

// expire counts sequence numbers missing for FinalLossAfter as lost.
func (s *seqTracker) expire(now time.Time, st *RecorderStats) {
	for len(s.queue) > 0 {
		m := s.queue[0]
		when, ok := s.missing[m]
		if ok && now.Sub(when) < FinalLossAfter {
			return
		}
		if ok {
			delete(s.missing, m)
			st.Lost++
		}
		s.queue = s.queue[1:]
	}
}

func (s *seqTracker) expected() int {
	if !s.started {
		return 0
	}
	return int(s.max - s.first + 1)
}
