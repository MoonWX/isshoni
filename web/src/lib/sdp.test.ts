import { describe, expect, it } from 'vitest';

import { forceOpusStereo, iceUfrag } from './sdp';

const crlf = (lines: string[]) => lines.join('\r\n') + '\r\n';

const ANSWER = crlf([
  'v=0',
  'o=- 1 2 IN IP4 127.0.0.1',
  's=-',
  't=0 0',
  'a=group:BUNDLE 0 1 2',
  'm=video 9 UDP/TLS/RTP/SAVPF 96 97',
  'a=ice-ufrag:abcd',
  'a=mid:0',
  'a=rtpmap:96 H264/90000',
  'a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f',
  'a=rtpmap:97 rtx/90000',
  'a=fmtp:97 apt=96',
  'm=audio 9 UDP/TLS/RTP/SAVPF 111 63',
  'a=ice-ufrag:abcd',
  'a=mid:1',
  'a=rtpmap:111 opus/48000/2',
  'a=rtcp-fb:111 transport-cc',
  'a=fmtp:111 minptime=10;useinbandfec=1',
  'a=rtpmap:63 red/48000/2',
  'a=fmtp:63 111/111',
  'm=audio 9 UDP/TLS/RTP/SAVPF 109',
  'a=mid:2',
  'a=rtpmap:109 opus/48000/2',
  'a=fmtp:109 minptime=10',
]);

describe('forceOpusStereo (05 §10.1, §19.1)', () => {
  it('adds stereo=1;sprop-stereo=1 to the Opus fmtp of every audio m-section', () => {
    const out = forceOpusStereo(ANSWER);
    expect(out).toContain('a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1\r\n');
    expect(out).toContain('a=fmtp:109 minptime=10;stereo=1;sprop-stereo=1\r\n');
  });

  it('leaves other fmtp lines intact', () => {
    const out = forceOpusStereo(ANSWER);
    expect(out).toContain('a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f\r\n');
    expect(out).toContain('a=fmtp:97 apt=96\r\n');
    expect(out).toContain('a=fmtp:63 111/111\r\n');
    // Only the two Opus lines changed.
    const before = ANSWER.split('\r\n');
    const after = out.split('\r\n');
    expect(after).toHaveLength(before.length);
    expect(after.filter((l, i) => l !== before[i])).toHaveLength(2);
  });

  it('is idempotent and adds each parameter once', () => {
    const once = forceOpusStereo(ANSWER);
    expect(forceOpusStereo(once)).toBe(once);
    expect(once.match(/stereo=1/g)).toHaveLength(4); // stereo + sprop-stereo, twice
  });

  it('sets a parameter that is present with another value, in place', () => {
    const sdp = crlf([
      'm=audio 9 UDP/TLS/RTP/SAVPF 111',
      'a=rtpmap:111 opus/48000/2',
      'a=fmtp:111 stereo=0;minptime=10',
    ]);
    expect(forceOpusStereo(sdp)).toContain('a=fmtp:111 stereo=1;minptime=10;sprop-stereo=1\r\n');
  });

  it('adds an fmtp line for an Opus payload type without one', () => {
    const sdp = crlf(['m=audio 9 UDP/TLS/RTP/SAVPF 111', 'a=mid:0', 'a=rtpmap:111 opus/48000/2', 'a=rtcp-mux']);
    expect(forceOpusStereo(sdp)).toBe(
      crlf([
        'm=audio 9 UDP/TLS/RTP/SAVPF 111',
        'a=mid:0',
        'a=rtpmap:111 opus/48000/2',
        'a=fmtp:111 stereo=1;sprop-stereo=1',
        'a=rtcp-mux',
      ]),
    );
  });

  it('treats payload types per m-section (111 is Opus only where its rtpmap says so)', () => {
    const sdp = crlf([
      'm=video 9 UDP/TLS/RTP/SAVPF 111',
      'a=rtpmap:111 H264/90000',
      'a=fmtp:111 packetization-mode=1',
      'm=audio 9 UDP/TLS/RTP/SAVPF 111',
      'a=rtpmap:111 opus/48000/2',
      'a=fmtp:111 minptime=10',
    ]);
    const out = forceOpusStereo(sdp);
    expect(out).toContain('a=fmtp:111 packetization-mode=1\r\n');
    expect(out).toContain('a=fmtp:111 minptime=10;stereo=1;sprop-stereo=1\r\n');
  });

  it('keeps LF line ends and a missing final newline', () => {
    const sdp = 'm=audio 9 UDP/TLS/RTP/SAVPF 111\na=rtpmap:111 opus/48000/2\na=fmtp:111 minptime=10';
    expect(forceOpusStereo(sdp)).toBe(
      'm=audio 9 UDP/TLS/RTP/SAVPF 111\na=rtpmap:111 opus/48000/2\na=fmtp:111 minptime=10;stereo=1;sprop-stereo=1',
    );
  });

  it('returns SDP without Opus unchanged', () => {
    const sdp = crlf(['v=0', 'm=video 9 UDP/TLS/RTP/SAVPF 96', 'a=rtpmap:96 H264/90000']);
    expect(forceOpusStereo(sdp)).toBe(sdp);
  });
});

describe('iceUfrag', () => {
  it('returns the first a=ice-ufrag, or null', () => {
    expect(iceUfrag(ANSWER)).toBe('abcd');
    expect(iceUfrag('v=0\r\n')).toBeNull();
  });
});
