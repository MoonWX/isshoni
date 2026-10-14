// Package publish is the Go RTP publisher (docs/m1/02-sfu.md §15.1): the M1 subset that the SFU tests and
// isshoni-loadtest use, and the start of M2's native client core.
//
// A Publisher owns one Pion PeerConnection that it offers, like a browser's pub PC. It sends one video track per
// simulcast rid (one transceiver, one encoding per layer) and optionally one audio track, pulled from a Source
// (fake.Source in M1, the native engine in M2):
//   - its own TrackLocal stamps the sdes:mid and rtp-stream-id header extensions, which Pion's built-in tracks don't
//     do for simulcast senders (S4);
//   - H.264 access units are packetized with pion/rtp's H264Payloader (payload MTU 1200, SPS and PPS aggregated into
//     a STAP-A, the RTP marker on the last packet of each frame); Opus packets go out as they are;
//   - rtp_ts = base + (capture − t0) × clock, one t0 for all tracks, so audio and video share one capture clock;
//   - it sends its own sender reports every second per SSRC: NTP = wall clock at the capture of the last frame sent,
//     RTP = that frame's timestamp (the plan's native path; no report.SenderInterceptor);
//   - PLI and FIR ask the Source for a keyframe of that layer; NACKs are answered by Pion's NACK responder; the TWCC
//     header-extension interceptor numbers every packet, so the SFU's feedback generator does real work;
//   - it can offer again on the same PeerConnection (the next neg of its gen), and every offer it hands out lists
//     its simulcast rids once, as the ones it sends: Pion's own re-offer repeats the rids of the last answer as
//     receive rids, which the SFU refuses like any rid listed twice (sendOnlySimulcast);
//   - it offers every H.264 profile it can send (Options.Profile first, then Options.Profiles) and sends the first
//     one the answer lists, as a browser does, so the room's codec policy (02 §8.3–8.4) chooses. On a codec hint
//     the caller does what a browser sharer does (01 §8.10): SetProfile puts the hinted profile first, and the next
//     offer's answer has the Publisher switch, each layer with its next keyframe, which it asks the Source for. A
//     ProfileSource re-encodes in the new profile; another Source keeps its SPS and only the payload type changes.
//
// There is no rate control in M1: the SFU's REMBs (02 §11: the admin's cap) are only counted in Stats. SetActive
// turns a video layer off and on again, which is what a browser sharer does with the active flags of a quality hint's
// encodings when the SFU pauses a full layer that nobody watches (02 §11); a layer turned on again starts with a
// keyframe.
//
// Two options exist for the SFU's tests (02 §17), to send what browsers send and fake media alone doesn't.
// Options.RedundantRTX negotiates RTX for the video and makes the Publisher answer NACKs itself, on the RTX stream,
// on which it also sends second copies of packets it already delivered: Chrome's padding and probes (repair.go).
// Options.Padding sends padding-only packets on the video's media streams, which take sequence numbers and carry
// nothing: a browser's padding without RTX.
package publish
