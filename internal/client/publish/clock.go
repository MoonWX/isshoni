package publish

import "time"

// Clock is implemented by a Source whose CaptureNS is not "now" when Next returns it: a real engine hands out a frame
// 10–30 ms after its capture (docs/m2/01-client-core.md D6, §8.2). The core's media pump implements it with
// media.Session.Now. A Source without it (M1's fake.Source) hands out each packet when it is due, so its capture
// instant is taken to be the moment Next returned it.
type Clock interface {
	// Now returns the current time on the clock of media.Packet.CaptureNS.
	Now() int64
}

// anchorWall returns the wall-clock instant of a capture time, taken when a packet captured at capture is in hand:
// now − (clock.Now() − capture) with a Clock, else now (M1's rule: the packet is due). It is the anchor of a
// Publisher's sender reports (§8.2), which then carry NTP = anchor + (capture − cap0) for every later frame.
func anchorWall(now time.Time, clock Clock, capture int64) time.Time {
	if clock == nil {
		return now
	}
	return now.Add(-time.Duration(clock.Now() - capture))
}
