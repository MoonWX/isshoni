package publish

import (
	"context"
	"sync"
	"time"
)

// The pacer's numbers (docs/m2/01-client-core.md D8, §8.5, §13).
const (
	// PacingFactor times the sum of the active video layers' targets (Publisher.SetPacingTargets) is the pacing
	// rate, as Chrome paces at 2.5 times its target: an 8 Mbit/s full layer paces at 24 Mbit/s.
	PacingFactor = 3
	// MinPacingRate is the lowest pacing rate, in bit/s: what a pacing Publisher without targets uses.
	MinPacingRate = 6_000_000
	// PacingDepth is the token bucket's depth, in time at the pacing rate. The bucket starts full.
	PacingDepth = 40 * time.Millisecond
	// PacingBacklog is how much queued video, in time at the pacing rate, makes the send loop wait before it takes
	// the next packet from its Source.
	PacingBacklog = 300 * time.Millisecond
)

// pacingRate returns the pacing rate in bit/s for the targets of the active video layers: PacingFactor times their
// sum, and at least MinPacingRate. Negative targets count 0.
func pacingRate(targets map[string]int, active []string) int64 {
	var sum int64
	for _, l := range active {
		if v := targets[l]; v > 0 {
			sum += int64(v)
		}
	}
	return max(PacingFactor*sum, MinPacingRate)
}

// pacer writes a Publisher's video (Options.Pacing, D8): the send loop queues each frame's packets (push), and the
// pacer's goroutine (run) writes them in order through one token bucket for all layers, so that a keyframe doesn't
// hit the user's router as a line-rate burst. Audio never goes through it: the send loop writes audio itself, so it
// never waits behind video. The pacer drops nothing: the send loop waits (waitRoom) while the queue holds more than
// PacingBacklog at the pacing rate, and dropping whole frames is the core's pump's job (§11.5).
type pacer struct {
	clock Clock // the Publisher's Source as a Clock, or nil: for CaptureToSend

	mu     sync.Mutex // guards the fields below
	rate   int64      // bit/s
	tokens float64    // bits in the bucket, at most rate × PacingDepth
	at     time.Time  // when tokens was last brought up to date
	used   bool       // a packet has taken tokens: until then the bucket stays full, whatever the rate
	queue  []*outFrame
	queued int64 // bytes of queued packets not written yet, the frame being written included

	wake chan struct{} // a frame came, or the rate changed (one signal at most is kept)
	room chan struct{} // the queue got shorter, or the rate changed
}

func newPacer(clock Clock) *pacer {
	pc := &pacer{
		clock: clock, rate: MinPacingRate, at: time.Now(),
		wake: make(chan struct{}, 1), room: make(chan struct{}, 1),
	}
	pc.tokens = pc.depth()
	return pc
}

// depth returns the bucket's depth in bits (pc.mu held).
func (pc *pacer) depth() float64 { return float64(pc.rate) * PacingDepth.Seconds() }

// refill brings the bucket up to now (pc.mu held).
func (pc *pacer) refill(now time.Time) {
	if d := now.Sub(pc.at); d > 0 {
		pc.tokens = min(pc.depth(), pc.tokens+float64(pc.rate)*d.Seconds())
		pc.at = now
	}
}

// setRate sets the pacing rate in bit/s. The bucket keeps its tokens, up to the new depth; before the first packet
// it is full at the new rate (it starts full at the rate the core sets before media flows).
func (pc *pacer) setRate(bps int64) {
	pc.mu.Lock()
	pc.refill(time.Now())
	pc.rate = bps
	pc.tokens = min(pc.tokens, pc.depth())
	if !pc.used {
		pc.tokens = pc.depth()
	}
	pc.mu.Unlock()
	signal(pc.wake)
	signal(pc.room)
}

// push queues a video frame's packets. Only the send loop calls it.
func (pc *pacer) push(f *outFrame) {
	var n int64
	for _, p := range f.pkts {
		n += int64(p.size)
	}
	pc.mu.Lock()
	pc.queue = append(pc.queue, f)
	pc.queued += n
	pc.mu.Unlock()
	signal(pc.wake)
}

// backlogged reports whether the queue holds more than PacingBacklog at the pacing rate (pc.mu held).
func (pc *pacer) backlogged() bool {
	return float64(pc.queued*8) > float64(pc.rate)*PacingBacklog.Seconds()
}

// waitRoom returns once the queue holds at most PacingBacklog at the pacing rate, or with ctx's error.
func (pc *pacer) waitRoom(ctx context.Context) error {
	for {
		pc.mu.Lock()
		full := pc.backlogged()
		pc.mu.Unlock()
		if !full {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pc.room:
		}
	}
}

// run writes the queued frames until ctx ends: each packet once the bucket holds its size, then the frame is counted
// (finish). What is still queued when ctx ends is never written.
func (pc *pacer) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		f := pc.pop(ctx)
		if f == nil {
			return
		}
		for i := range f.pkts {
			if !pc.take(ctx, timer, f.pkts[i].size) {
				return
			}
			f.t.writePacket(f, i)
			signal(pc.room)
		}
		f.t.finish(f, time.Now(), pc.clock)
	}
}

// pop returns the oldest queued frame, waiting for one; nil when ctx ends.
func (pc *pacer) pop(ctx context.Context) *outFrame {
	for {
		if ctx.Err() != nil {
			return nil
		}
		pc.mu.Lock()
		if len(pc.queue) > 0 {
			f := pc.queue[0]
			pc.queue[0] = nil
			pc.queue = pc.queue[1:]
			pc.mu.Unlock()
			return f
		}
		pc.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-pc.wake:
		}
	}
}

// take waits until the bucket holds size bytes' worth of tokens and takes them; false when ctx ends first. A packet
// larger than the bucket (never at these rates) takes a full bucket.
func (pc *pacer) take(ctx context.Context, timer *time.Timer, size int) bool {
	for {
		pc.mu.Lock()
		pc.refill(time.Now())
		need := min(float64(size*8), pc.depth())
		// Half a bit of slack, so that float rounding never costs one more wait of a few ns.
		if pc.tokens+0.5 >= need {
			pc.tokens -= need
			pc.used = true
			pc.queued -= int64(size)
			pc.mu.Unlock()
			return true
		}
		wait := time.Duration((need - pc.tokens) / float64(pc.rate) * float64(time.Second))
		pc.mu.Unlock()
		timer.Reset(max(wait, time.Microsecond))
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		case <-pc.wake: // the rate changed, or a frame came: count again
			timer.Stop()
		}
	}
}

// signal sends on a channel of capacity 1 without blocking: a signal already pending stands for both.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
