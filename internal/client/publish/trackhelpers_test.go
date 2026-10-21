package publish

import "github.com/MoonWX/isshoni/internal/media"

// M1's track-level test helpers. The send loop packetizes (track.packetize) and then writes the frame itself or hands
// it to the pacer, and applyProfile sets switches with a profile_idc to wait for (track.switchWhen); these two are
// what M1's tests drive a track with.

// switchTo has the video track go on in another of the Publisher's profiles from its next keyframe (write), and
// reports whether it will switch: false when it sends that profile already, or isn't bound.
func (t *track) switchTo(profile string) bool { return t.switchWhen(profile, 0) }

// write packetizes and writes one Source packet with RTP timestamp tsBase + ticks and the sender-report NTP time of
// its capture, without a Clock. It returns false when the track isn't bound.
func (t *track) write(pkt media.Packet, ticks uint32, ntp uint64) bool {
	f := t.packetize(pkt, ticks, ntp)
	if f == nil {
		return false
	}
	t.writeFrame(f, nil)
	return true
}
