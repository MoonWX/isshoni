// Package sfu is isshoni's selective forwarding unit on Pion (docs/m1/02-sfu.md): rooms, participants and
// connections, shares with two simulcast layers, keyframe-aligned layer switches, NACK/RTX from a shared packet
// cache, forwarded sender reports, downlink adaptation and a per-room H.264 profile policy.
//
// The package is built slice by slice (docs/m1/README.md §5). What exists so far:
//   - codec.go: ProfileKey, the H.264 compatibility matrix, bestPT, the static payload-type table and the publish and
//     subscribe MediaEngines with their interceptors (02 §8.1–8.2), and the codec policy helpers (02 §8.3–8.4);
//   - h264.go: keyframe-start detection and the SPS parser (02 §9.1);
//   - sdpcheck.go: publish-offer validation, the subscribe-answer H.264 check, and the answer edits: the Opus
//     maxaveragebitrate, and a=inactive for sending m-sections that tracks doesn't bind (02 §8.4–8.5);
//   - errors.go and types.go: Error with the sfu.* codes (02 §6.3), the identifier types and TrackBinding (02 §6.1);
//   - layer.go: Slot, packet, srInfo and the part of Layer that the munger and the packet cache use (02 §9.1);
//   - munger.go: the per-DownTrack seq/ts rewrite with epochs (the sequence map NACKs go through), keyframe-aligned
//     switches, SR-aligned timestamps and closed padding gaps (02 §9.4);
//   - packetcache.go: the per-Layer ring of recent packets that serves every viewer's NACKs (02 §9.2).
//
// The SFU never imports signal or protocol: 01's sfuplane adapter is the only translator between this package and
// the wire.
package sfu
