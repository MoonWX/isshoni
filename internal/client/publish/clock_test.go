package publish

import (
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// ntpDiff returns a − b of two NTP times.
func ntpDiff(a, b uint64) time.Duration {
	return time.Duration((int64(a) - int64(b)) * int64(time.Second) >> 32)
}

// TestSRAnchorClock: with a Source that is a Clock and hands out packets 25 ms after their capture, the sender
// reports carry NTP = wall(now) − (Now − capture) within 1 ms, for the first frame and, through the fixed anchor,
// for every later frame and the audio (§8.2, D6). Without a Clock the first packet's capture is "now" (M1).
func TestSRAnchorClock(t *testing.T) {
	const age = 25 * time.Millisecond
	src := newScriptSource(nil)
	b := newBench(src, []string{"f"}, true, false)
	send := func(pkt media.Packet, old time.Duration) (wall time.Time) {
		pkt.CaptureNS = src.Now() - int64(old)
		wall = time.Now().Add(-old)
		b.p.send(pkt)
		return wall
	}
	sr := func(tr *track) uint64 {
		t.Helper()
		r, ok := tr.senderReport()
		if !ok {
			t.Fatal("no sender report")
		}
		return r.NTPTime
	}
	within := func(what string, ntp uint64, wall time.Time) {
		t.Helper()
		if d := ntpDiff(ntp, ntpTime(wall)); d < -time.Millisecond || d > time.Millisecond {
			t.Errorf("%s: SR NTP is %v off the wall clock at capture", what, d)
		}
	}
	first := send(videoFrame("f", 3000, true), age)
	within("the first frame", sr(b.p.video[0]), first)
	ts0, _ := b.p.video[0].senderReport()

	time.Sleep(30 * time.Millisecond)
	later := send(videoFrame("f", 3000, false), age)
	within("a frame 30 ms later", sr(b.p.video[0]), later)
	ts1, _ := b.p.video[0].senderReport()
	// The anchor is fixed: NTP and RTP time advance by the capture difference, whatever the packets' age.
	dCap := time.Duration(int64(ts1.RTPTime-ts0.RTPTime) * int64(time.Second) / videoClock)
	if d := ntpDiff(ts1.NTPTime, ts0.NTPTime) - dCap; d < -50*time.Microsecond || d > 50*time.Microsecond {
		t.Errorf("between two frames NTP moved %v more than the RTP time", d)
	}
	older := send(audioPacket(), 60*time.Millisecond) // audio is older when it comes out (§12.1)
	within("audio 60 ms old", sr(b.p.audio), older)

	// Without a Clock the first packet's capture is the moment it is sent, whatever its CaptureNS says.
	plain := newBench(plainSource{newScriptSource(nil)}, []string{"f"}, false, false)
	pkt := videoFrame("f", 3000, true)
	pkt.CaptureNS = 123_456_789
	now := time.Now()
	plain.p.send(pkt)
	within("without a Clock", sr(plain.p.video[0]), now)
}

func TestAnchorWall(t *testing.T) {
	now := time.Unix(1000, 0)
	src := newScriptSource(nil)
	capture := src.Now() - int64(25*time.Millisecond)
	if got := anchorWall(now, src, capture); now.Sub(got) < 25*time.Millisecond || now.Sub(got) > 26*time.Millisecond {
		t.Errorf("a packet 25 ms old is anchored %v before now", now.Sub(got))
	}
	if got := anchorWall(now, nil, capture); !got.Equal(now) {
		t.Errorf("without a Clock the anchor is %v, want now", got)
	}
}
