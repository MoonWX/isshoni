package sfu

import (
	"time"

	"github.com/pion/webrtc/v4"
)

// This file is the pacer of 02 §9.8: one token bucket per video DownTrack, which spreads a keyframe burst over a few
// tens of milliseconds instead of handing it to the socket at once for every viewer.
//
// The rate follows the layer the packet belongs to: max(2.5 × the layer's MaxBitrate after the admin cap, 3 × its
// measured bitrate, 2 Mbit/s), and the bucket holds max(20 ms of that rate, 16 KiB). The configured maximum counts
// because screen video idles, then bursts: a still screen runs at about 1 fps and well under 0.5 Mbit/s, and the first
// change arrives at up to the cap, often starting with a frame of 150–400 KB. A rate from the measured average alone
// would be 2 Mbit/s at that moment, and add about a second of video latency (and of A/V drift, since audio isn't
// paced) at every motion onset. For an 8 Mbit/s full layer the rate is 20 Mbit/s: a 150 KB keyframe leaves over
// about 60 ms, 50 KB of it (the bucket) at once.
//
// What goes through it (decided in README S84, the open question of S63): every video packet the writer sends, media
// and retransmissions alike, in the writer's order, which sends retransmissions first. A repair takes its tokens
// before new media does, so it never waits behind a frame; and a viewer whose NACKs ask for a large frame again gets
// it spread like the frame itself, not in one burst on top of the burst that was lost. The retransmission budget of
// 02 §9.5 bounds how much of the rate repair may use on average; the pacer bounds how fast any of it leaves. Audio is
// never paced, its retransmissions neither: it has its own writer, so video pacing never delays it.
//
// Queue drops feed the downlink allocator (02 §10.2); at these rates they mean that the writer is starved of CPU, not
// that the pacer is slow. Time spent in the pacer is not an allocator input.

// Pacing (02 §9.8).
const (
	// The rate: the largest of pacerMaxShare × the layer's MaxBitrate (pacerMaxShare/pacerShareDiv: 2.5),
	// pacerMeasured × its measured bitrate, and pacerMinRate.
	pacerMaxShare = 5
	pacerShareDiv = 2
	pacerMeasured = 3
	pacerMinRate  = 2_000_000
	// The bucket holds pacerDepthTime of the rate, at least pacerMinDepth bits (16 KiB).
	pacerDepthTime = 20 * time.Millisecond
	pacerMinDepth  = 16 * 1024 * 8
	// pacerMaxFill bounds the time one fill counts: the bucket is full long before, and a long idle time would
	// overflow the arithmetic.
	pacerMaxFill = time.Second
)

// pacer is a video DownTrack's token bucket (02 §9.8). Its writer is the only goroutine that touches it, so it has no
// lock. It holds bits: a packet takes its payload's bits, and leaves when the bucket has refilled them. The tokens go
// below zero while a packet waits for its turn: the packet leaves when they are back at zero.
type pacer struct {
	started bool
	tokens  int64 // bits
	at      int64 // monoNow of the last fill
}

// pacerRate returns the pacer's rate for a packet of layer l, in bit/s: max(2.5 × the layer's MaxBitrate after the
// admin cap, 3 × its measured bitrate, 2 Mbit/s). Every number is an atomic or a constant (Share.maxBitrate), so a
// writer reads it for every packet without a lock.
func pacerRate(l *Layer) int64 {
	if l == nil {
		return pacerMinRate
	}
	most := l.share.maxBitrate(l.slot) * pacerMaxShare / pacerShareDiv
	return max(most, pacerMeasured*l.stats.bitrate.Load(), pacerMinRate)
}

// pacerDepth returns how many bits the bucket holds at rate: 20 ms of it, at least 16 KiB.
func pacerDepth(rate int64) int64 {
	return max(rate*int64(pacerDepthTime)/int64(time.Second), pacerMinDepth)
}

// delay takes a packet of bits at rate (bit/s) at now (monoNow), and returns how long it must wait before it may
// leave: 0 while the bucket holds its bits. The bucket starts full, fills at rate up to its depth, and a lower rate
// than before (the DownTrack went to a smaller layer) also lowers what it holds. The time a writer oversleeps is
// not lost: the next fill counts it, so the rate holds over a burst however coarse the sleeps are.
func (p *pacer) delay(bits, rate, now int64) time.Duration {
	depth := pacerDepth(rate)
	switch {
	case !p.started:
		p.started, p.tokens, p.at = true, depth, now
	case now > p.at:
		elapsed := min(now-p.at, int64(pacerMaxFill))
		p.tokens += elapsed * rate / int64(time.Second)
		p.at = now
	}
	p.tokens = min(p.tokens, depth)
	p.tokens -= bits
	if p.tokens >= 0 {
		return 0
	}
	// Rounded up: a packet that left a nanosecond early would leave the next one a nanosecond less.
	return time.Duration((-p.tokens*int64(time.Second) + rate - 1) / rate)
}

// pace holds the writer until a video packet of layer l with n payload bytes may leave (02 §9.8). It reports false
// when the DownTrack or its Conn stopped meanwhile: the packet is not sent then. Audio is never paced.
func (d *DownTrack) pace(w *rtpWriteState, l *Layer, n int) bool {
	if d.kind != webrtc.RTPCodecTypeVideo {
		return true
	}
	wait := d.pacer.delay(int64(n)*8, pacerRate(l), monoNow())
	if wait <= 0 {
		return true
	}
	return w.sleep(wait)
}

// sleep waits d, unless the DownTrack's writer is told to stop first (stop, the Conn's close): then it reports false.
// The timer is the writer's own and is reused, so pacing allocates nothing per packet.
func (w *rtpWriteState) sleep(d time.Duration) bool {
	if w.timer == nil {
		w.timer = time.NewTimer(d)
	} else {
		w.timer.Reset(d)
	}
	select {
	case <-w.timer.C:
		return true
	case <-w.stop:
	case <-w.connClosed:
	}
	w.timer.Stop()
	return false
}
