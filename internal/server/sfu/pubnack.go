package sfu

import (
	"maps"
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

// This file is the publish side of repair (02 §9.5): what the SFU asks a publisher to send again. Pion's NACK
// generator (02 §8.1) logs the sequence numbers that arrive on each incoming stream and every 50 ms asks for those
// that are missing. It never learns of a packet that came back on the RTX stream: Pion's receiver reads that stream
// through an interceptor chain of its own and hands the packets to the track past the generator. Left to itself, the
// generator would ask for such a packet again at each tick, up to pubNACKTries times, and a browser sends a copy for
// every request that comes a round trip or more after its last one: some ten copies of each packet the publisher's
// link lost, all dropped as duplicates at the cache.
//
// So each pub PC has a NACK filter of its own, the first interceptor of its chain, and the generator, registered
// after it, writes its NACKs through it (pubInterceptors). The filter takes out every sequence number whose packet
// the cache of the track's Layer holds by then, and drops a NACK with nothing left. The cache gets every packet of
// the track, however it came (the track's reader hands both kinds to the Layer, 02 §9.1), so the requests for a
// packet stop once it is there, and pubNACKTries only bounds those for a packet that never comes.

// nackFilter is the NACK filter of one pub PC. Its table maps the media SSRC of each of the PC's tracks to the
// track. The Conn's actor replaces the table whenever a track comes or goes (copy on write); the filter reads it on
// the generator's goroutine without a lock. A NACK about a track that feeds no Layer, or about an SSRC the table
// doesn't have yet (a track whose arrival the actor hasn't handled), and every other RTCP packet, pass unchanged.
//
// It is its own interceptor.Factory: a pub PC's registry is the PC's own (apis.pubAPI), and Pion builds the PC's
// interceptors from it once, while it creates the PC.
type nackFilter struct {
	interceptor.NoOp
	tracks atomic.Pointer[map[uint32]*pubTrack]
}

// NewInterceptor returns f itself, for the PC whose registry f is in.
func (f *nackFilter) NewInterceptor(string) (interceptor.Interceptor, error) { return f, nil }

// BindRTCPWriter puts the filter in front of the PC's RTCP writer: what the interceptors registered after it write
// (the NACK generator, receiver reports, transport-wide CC feedback) goes through it, and so does what the SFU writes
// itself (PLI). Only NACKs are changed. A batch of which nothing is left is not written; the generator, the one
// writer of NACKs, ignores the count it gets back.
func (f *nackFilter) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, a interceptor.Attributes) (int, error) {
		out := f.filter(pkts, monoNow())
		if len(out) == 0 && len(pkts) > 0 {
			return 0, nil
		}
		return w.Write(out, a)
	})
}

// filter returns pkts without what the caches hold at now: pkts itself when that is nothing, else a new slice in
// which each NACK that asked for something cached has been replaced by a smaller one, or left out when it asked for
// nothing else. pkts is not changed.
func (f *nackFilter) filter(pkts []rtcp.Packet, now int64) []rtcp.Packet {
	var (
		out     []rtcp.Packet
		changed bool
	)
	for i, p := range pkts {
		nack, ok := p.(*rtcp.TransportLayerNack)
		var trimmed *rtcp.TransportLayerNack
		if ok {
			trimmed = f.trim(nack, now)
		}
		switch {
		case !ok || trimmed == nack:
			if changed {
				out = append(out, p)
			}
			continue
		case !changed:
			changed, out = true, append(make([]rtcp.Packet, 0, len(pkts)), pkts[:i]...)
		}
		if trimmed != nil {
			out = append(out, trimmed)
		}
	}
	if !changed {
		return pkts
	}
	return out
}

// trim returns nack without the sequence numbers whose packets are in the cache of the Layer that its media SSRC's
// track feeds: nack itself when that is none of them, nil when it is all of them.
func (f *nackFilter) trim(nack *rtcp.TransportLayerNack, now int64) *rtcp.TransportLayerNack {
	tracks := f.tracks.Load()
	if tracks == nil {
		return nack
	}
	t := (*tracks)[nack.MediaSSRC]
	if t == nil {
		return nack
	}
	l := t.layer.Load()
	if l == nil {
		return nack
	}
	var (
		scratch [64]uint16
		missing = scratch[:0]
		arrived bool
	)
	for _, pair := range nack.Nacks {
		for bit := -1; bit < 16; bit++ {
			if bit >= 0 && uint16(pair.LostPackets)&(1<<bit) == 0 {
				continue
			}
			if seq := pair.PacketID + uint16(bit+1); l.cache.get(seq, now) == nil {
				missing = append(missing, seq)
			} else {
				arrived = true
			}
		}
	}
	switch {
	case !arrived:
		return nack
	case len(missing) == 0:
		return nil
	}
	return &rtcp.TransportLayerNack{
		SenderSSRC: nack.SenderSSRC, MediaSSRC: nack.MediaSSRC, Nacks: rtcp.NackPairsFromSequenceNumbers(missing),
	}
}

// add enters track t of the PC under its SSRC, in place of any track that had it. The actor calls it when Pion
// delivers t.
func (f *nackFilter) add(t *pubTrack) {
	next := map[uint32]*pubTrack{}
	if cur := f.tracks.Load(); cur != nil {
		maps.Copy(next, *cur)
	}
	next[t.ssrc] = t
	f.tracks.Store(&next)
}

// remove takes track t out of the table, unless another track has its SSRC by now. The actor calls it when t has
// ended.
func (f *nackFilter) remove(t *pubTrack) {
	cur := f.tracks.Load()
	if cur == nil || (*cur)[t.ssrc] != t {
		return
	}
	next := maps.Clone(*cur)
	delete(next, t.ssrc)
	f.tracks.Store(&next)
}
