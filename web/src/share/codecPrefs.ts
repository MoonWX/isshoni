// Codec preferences for the pub PC's transceivers (05 §13.4 steps 3 and 4, 01 §9 rule 7). Pure functions over
// RTCRtpSender.getCapabilities(kind).codecs.
//
// Video: H.264 everywhere in v1. The offer lists every H.264 profile the browser can encode with packetization-mode 1
// (the server's answer keeps the ones the room's codec policy allows, 02 §8.4), plus RTX, and nothing else: no VP8,
// VP9 or AV1, no RED or FEC. The order is the preference: the profile of ShareParams.codec (or of the latest
// quality.hint) first, matched by its CodecKey, so the level doesn't matter (Chrome's High is 640034, Safari's
// Constrained High 640c1f). S4 finding 4: Chrome on macOS hardware-encodes both simulcast layers only with High,
// which is why the server asks for High whenever the room allows it.
//
// Audio: Opus only.
import { LocalError } from '../lib/errors';
import { h264Key } from '../protocol/codecs';
import {
  CodecH264Baseline,
  CodecH264ConstrainedBaseline,
  CodecH264ConstrainedHigh,
  CodecH264High,
  CodecH264Main,
  type CodecKey,
} from '../protocol/types.gen';

/** The order of the profiles after the preferred one: High, Constrained High, Constrained Baseline, Baseline, Main. */
export const H264_PROFILE_ORDER: readonly CodecKey[] = [
  CodecH264High,
  CodecH264ConstrainedHigh,
  CodecH264ConstrainedBaseline,
  CodecH264Baseline,
  CodecH264Main,
];

const mime = (c: RTCRtpCodec): string => c.mimeType.toLowerCase();

/** The CodecKey of an H.264 capability with packetization-mode 1; '' for anything else. */
function videoKey(c: RTCRtpCodec): CodecKey {
  return mime(c) === 'video/h264' ? h264Key(c.sdpFmtpLine ?? '') : '';
}

/** Whether the sender can encode H.264 at all (packetization-mode 1): 05 §13.4 step 8. */
export function canSendH264(capabilities: readonly RTCRtpCodec[]): boolean {
  return capabilities.some((c) => videoKey(c) !== '');
}

/**
 * The list for the video transceiver's setCodecPreferences: the H.264 entries with packetization-mode 1, the
 * preferred profile first, then the others in H264_PROFILE_ORDER (a profile outside that list comes after them),
 * then every video/rtx entry. Entries of one profile (several levels) keep the browser's order.
 *
 * Throws LocalError `h264_unavailable` when the browser has no H.264 encoder ("This browser can't send H.264 video.
 * Use Chrome or Edge."). A preferred profile the browser can't encode just isn't first: the answer decides.
 */
export function videoCodecPreferences(capabilities: readonly RTCRtpCodec[], preferred: CodecKey): RTCRtpCodec[] {
  const h264 = capabilities.filter((c) => videoKey(c) !== '');
  if (h264.length === 0) throw new LocalError('h264_unavailable');
  const rank = (c: RTCRtpCodec): number => {
    const key = videoKey(c);
    if (key === preferred) return -1;
    const at = H264_PROFILE_ORDER.indexOf(key);
    return at < 0 ? H264_PROFILE_ORDER.length : at;
  };
  // Array.prototype.sort is stable: equal ranks keep the browser's order.
  const ordered = [...h264].sort((a, b) => rank(a) - rank(b));
  return [...ordered, ...capabilities.filter((c) => mime(c) === 'video/rtx')];
}

/** The list for the audio transceiver's setCodecPreferences: only audio/opus. [] when the browser lists none. */
export function audioCodecPreferences(capabilities: readonly RTCRtpCodec[]): RTCRtpCodec[] {
  return capabilities.filter((c) => mime(c) === 'audio/opus');
}
