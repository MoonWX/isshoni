// SDP text helpers for the PeerConnection controllers (05 §9–§10). Only line-level edits: SDP is never parsed into
// objects or logged (05 §10.7).

/** An SDP's line break: browsers produce CRLF; tests and hand-written SDP may use LF. */
function lineBreak(sdp: string): string {
  return sdp.includes('\r\n') ? '\r\n' : '\n';
}

const OPUS_RTPMAP = /^a=rtpmap:(\d+) opus\/48000(?:\/\d+)?$/i;
const FMTP = /^a=fmtp:(\d+) (.*)$/;

/**
 * Sets stereo=1 and sprop-stereo=1 on the fmtp parameters, keeping every other parameter and its order. A parameter
 * already present with another value is changed in place; a missing one is appended.
 */
function withStereo(params: string): string {
  const parts = params.split(';').filter((p) => p.trim() !== '');
  const want = new Map([
    ['stereo', '1'],
    ['sprop-stereo', '1'],
  ]);
  const out = parts.map((p) => {
    const eq = p.indexOf('=');
    const name = (eq < 0 ? p : p.slice(0, eq)).trim().toLowerCase();
    const value = want.get(name);
    if (value === undefined) return p;
    want.delete(name);
    return `${name}=${value}`;
  });
  for (const [name, value] of want) out.push(`${name}=${value}`);
  return out.join(';');
}

/**
 * The Opus stereo munge of 01 §9 rule 6 (05 §10.1): a receiver must ask for stereo itself, so the sub PC's answer
 * gets stereo=1;sprop-stereo=1 on the fmtp of every Opus payload type, in every audio m-section. An Opus payload
 * type without an fmtp line gets one after its rtpmap. Idempotent; other fmtp lines and parameters are untouched.
 */
export function forceOpusStereo(sdp: string): string {
  const eol = lineBreak(sdp);
  const trailing = sdp.endsWith(eol);
  const lines = (trailing ? sdp.slice(0, -eol.length) : sdp).split(eol);

  // Payload types are per m-section, so work section by section (the session part comes first, with none).
  const sections: string[][] = [[]];
  for (const line of lines) {
    if (line.startsWith('m=')) sections.push([]);
    sections[sections.length - 1]?.push(line);
  }
  return sections.map(mungeSection).flat().join(eol) + (trailing ? eol : '');
}

function mungeSection(lines: string[]): string[] {
  const opus = new Set<string>();
  const hasFmtp = new Set<string>();
  for (const line of lines) {
    const rtpmap = OPUS_RTPMAP.exec(line);
    if (rtpmap?.[1] !== undefined) opus.add(rtpmap[1]);
    const fmtp = FMTP.exec(line);
    if (fmtp?.[1] !== undefined) hasFmtp.add(fmtp[1]);
  }
  if (opus.size === 0) return lines;
  const out: string[] = [];
  for (const line of lines) {
    const fmtp = FMTP.exec(line);
    if (fmtp?.[1] !== undefined && fmtp[2] !== undefined && opus.has(fmtp[1])) {
      out.push(`a=fmtp:${fmtp[1]} ${withStereo(fmtp[2])}`);
      continue;
    }
    out.push(line);
    const rtpmap = OPUS_RTPMAP.exec(line);
    if (rtpmap?.[1] !== undefined && !hasFmtp.has(rtpmap[1])) {
      out.push(`a=fmtp:${rtpmap[1]} stereo=1;sprop-stereo=1`);
    }
  }
  return out;
}

/**
 * The first a=ice-ufrag value, or null. With max-bundle every m-section has the same one; a new value in a sub offer
 * means the server restarted ICE (05 §9).
 */
export function iceUfrag(sdp: string): string | null {
  const m = /^a=ice-ufrag:(\S+)\s*$/m.exec(sdp);
  return m?.[1] ?? null;
}
