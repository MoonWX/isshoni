// Media capabilities as the protocol names them (docs/m1/01-protocol.md §8.2, §16): CodecKeys from the browser's
// RTCRtpReceiver/RTCRtpSender.getCapabilities, for hello.caps and caps.update. h264Key is the TypeScript twin of
// Go's protocol.ParseH264CodecKey (internal/protocol/codec.go); codecs.test.ts runs Go's own test table against it.
import { CodecOpus, MaxCodecs, type Caps, type CodecKey } from './types.gen';

const h264Prefix = 'h264/';
const hex6 = /^[0-9a-f]{6}$/i;

/**
 * Maps an H.264 fmtp line to its CodecKey: "h264/" and the first 4 hex digits of profile-level-id in lowercase
 * (profile_idc and the constraint byte; the level is ignored, so Chrome's High 640034 is "h264/6400"). It returns ""
 * unless packetization-mode is 1 and profile-level-id is 6 hex digits. It takes the parameter list as browsers report
 * it in RTCRtpCodec.sdpFmtpLine ("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"), with or
 * without an "a=fmtp:<pt> " prefix; names and hex digits are case-insensitive, and the last duplicate wins.
 */
export function h264Key(fmtp: string): CodecKey {
  let params = fmtp;
  if (params.startsWith('a=fmtp:')) {
    const space = params.indexOf(' ');
    params = space < 0 ? '' : params.slice(space + 1);
  }
  let mode1 = false;
  let profile = '';
  for (const param of params.split(';')) {
    const eq = param.indexOf('=');
    const name = (eq < 0 ? param : param.slice(0, eq)).trim().toLowerCase();
    const value = eq < 0 ? '' : param.slice(eq + 1).trim();
    if (name === 'packetization-mode') {
      mode1 = value === '1';
    } else if (name === 'profile-level-id') {
      profile = value;
    }
  }
  if (!mode1 || !hex6.test(profile)) {
    return '';
  }
  return h264Prefix + profile.slice(0, 4).toLowerCase();
}

/** The static side of RTCRtpReceiver and RTCRtpSender, loosely typed: older browsers and jsdom lack them. */
interface CapabilitiesSource {
  getCapabilities?: (kind: string) => RTCRtpCapabilities | null;
}

/** The globals detectCaps reads, each of which may be missing. */
interface MediaGlobals {
  RTCRtpReceiver?: CapabilitiesSource;
  RTCRtpSender?: CapabilitiesSource;
  RTCRtpTransceiver?: unknown;
  navigator?: { mediaDevices?: { getDisplayMedia?: unknown } };
}

function codecsOf(source: CapabilitiesSource | undefined, kind: 'audio' | 'video'): RTCRtpCodec[] {
  try {
    return source?.getCapabilities?.(kind)?.codecs ?? [];
  } catch {
    // A browser may throw instead of returning null for a kind it cannot handle.
    return [];
  }
}

/**
 * The CodecKeys of one side (receiver: decode, sender: encode): every H.264 profile with packetization-mode=1 in the
 * browser's order, each once, then "opus" when the browser has Opus. At most MaxCodecs entries (the server rejects
 * longer lists).
 */
function codecKeys(source: CapabilitiesSource | undefined): CodecKey[] {
  const keys = new Set<CodecKey>();
  for (const c of codecsOf(source, 'video')) {
    const key = c.mimeType.toLowerCase() === 'video/h264' ? h264Key(c.sdpFmtpLine ?? '') : '';
    if (key !== '') {
      keys.add(key);
    }
  }
  if (codecsOf(source, 'audio').some((c) => c.mimeType.toLowerCase() === 'audio/opus')) {
    keys.add(CodecOpus);
  }
  return [...keys].slice(0, MaxCodecs);
}

/**
 * Reports this browser's media capabilities for hello.caps and caps.update (01 §8.2, §8.10, §11.7). It is
 * synchronous and cheap, so callers re-read it on every (re)connect and poll it while a subscription waits for a
 * decoder (Firefox fetches OpenH264 in the background).
 * - decode: the receiver's H.264 profiles (packetization-mode=1 only) and opus; what the server's codec policy uses.
 * - encode: the same for the sender; omitted when empty.
 * - simulcast: the sender can encode H.264 and has RTCRtpTransceiver (rid simulcast through sendEncodings).
 * - displayCapture: the sender can encode H.264 and getDisplayMedia exists. Whether this device may share (phones
 *   and tablets may not) is the platform's decision (05 §8), not a capability.
 * Missing APIs give empty lists and false flags, never an exception.
 */
export function detectCaps(): Caps {
  const g = globalThis as MediaGlobals;
  const decode = codecKeys(g.RTCRtpReceiver);
  const encode = codecKeys(g.RTCRtpSender);
  const canEncodeH264 = encode.some((k) => k.startsWith(h264Prefix));
  const caps: Caps = { decode };
  if (encode.length > 0) {
    caps.encode = encode;
  }
  if (canEncodeH264 && typeof g.RTCRtpTransceiver === 'function') {
    caps.simulcast = true;
  }
  if (canEncodeH264 && typeof g.navigator?.mediaDevices?.getDisplayMedia === 'function') {
    caps.displayCapture = true;
  }
  return caps;
}
