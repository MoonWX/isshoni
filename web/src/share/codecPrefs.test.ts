import { describe, expect, it } from 'vitest';

import { LocalError } from '../lib/errors';
import { h264Key } from '../protocol/codecs';
import { audioCodecPreferences, canSendH264, H264_PROFILE_ORDER, videoCodecPreferences } from './codecPrefs';

const h264 = (profileLevelId: string, mode = 1): RTCRtpCodec => ({
  mimeType: 'video/H264',
  clockRate: 90000,
  sdpFmtpLine: `level-asymmetry-allowed=1;packetization-mode=${String(mode)};profile-level-id=${profileLevelId}`,
});
const other = (mimeType: string): RTCRtpCodec => ({ mimeType, clockRate: 90000 });
const RTX = other('video/rtx');

/** Like desktop Chrome's sender capabilities: every profile with both packetization modes, between other codecs. */
const CHROME: RTCRtpCodec[] = [
  other('video/VP8'),
  RTX,
  other('video/VP9'),
  h264('42001f'),
  h264('42001f', 0),
  h264('42e01f'),
  h264('42e01f', 0),
  h264('4d001f'),
  h264('4d001f', 0),
  h264('640034'),
  h264('640034', 0),
  other('video/AV1'),
  other('video/red'),
  other('video/ulpfec'),
];

/** Like Safari's: Constrained High and Constrained Baseline. */
const SAFARI: RTCRtpCodec[] = [h264('640c1f'), RTX, h264('42e01f'), other('video/VP8')];

/** The profile keys of a preference list, RTX as "rtx". */
const keys = (codecs: RTCRtpCodec[]): string[] =>
  codecs.map((c) => (c.mimeType === 'video/rtx' ? 'rtx' : h264Key(c.sdpFmtpLine ?? '')));

describe('videoCodecPreferences (05 §13.4 step 3)', () => {
  it('puts ShareParams.codec first, matched by its key: Chrome’s 640034 is h264/6400', () => {
    const prefs = videoCodecPreferences(CHROME, 'h264/6400');
    expect(prefs[0]?.sdpFmtpLine).toContain('profile-level-id=640034');
    expect(keys(prefs)).toEqual(['h264/6400', 'h264/42e0', 'h264/4200', 'h264/4d00', 'rtx']);
  });

  it('matches Safari’s 640c1f for h264/640c', () => {
    const prefs = videoCodecPreferences(SAFARI, 'h264/640c');
    expect(prefs[0]?.sdpFmtpLine).toContain('profile-level-id=640c1f');
    expect(keys(prefs)).toEqual(['h264/640c', 'h264/42e0', 'rtx']);
  });

  it('then the other profiles in the fixed order: 6400, 640c, 42e0, 4200, 4d00', () => {
    expect(H264_PROFILE_ORDER).toEqual(['h264/6400', 'h264/640c', 'h264/42e0', 'h264/4200', 'h264/4d00']);
    expect(keys(videoCodecPreferences(CHROME, 'h264/42e0'))).toEqual([
      'h264/42e0',
      'h264/6400',
      'h264/4200',
      'h264/4d00',
      'rtx',
    ]);
    const all = [h264('4d001f'), h264('42001f'), h264('42e01f'), h264('640c1f'), h264('640034')];
    expect(keys(videoCodecPreferences(all, 'h264/4d00'))).toEqual([
      'h264/4d00',
      'h264/6400',
      'h264/640c',
      'h264/42e0',
      'h264/4200',
    ]);
  });

  it('keeps RTX (every entry) and nothing else: no VP8, VP9, AV1, RED, FEC, no packetization-mode 0', () => {
    const prefs = videoCodecPreferences([...CHROME, RTX], 'h264/6400');
    expect(prefs.map((c) => c.mimeType)).toEqual([
      'video/H264',
      'video/H264',
      'video/H264',
      'video/H264',
      'video/rtx',
      'video/rtx',
    ]);
    expect(prefs.every((c) => !c.sdpFmtpLine?.includes('packetization-mode=0'))).toBe(true);
  });

  it('a preferred profile this browser can’t encode just isn’t first: the fixed order', () => {
    // Firefox: Baseline profiles only; the room asks for High.
    const firefox = [h264('42001f'), h264('42e01f'), RTX];
    expect(keys(videoCodecPreferences(firefox, 'h264/6400'))).toEqual(['h264/42e0', 'h264/4200', 'rtx']);
  });

  it('keeps several levels of one profile together, in the browser’s order', () => {
    const prefs = videoCodecPreferences([h264('42e01f'), h264('640034'), h264('64001f')], 'h264/6400');
    expect(prefs.map((c) => /profile-level-id=(\w+)/.exec(c.sdpFmtpLine ?? '')?.[1])).toEqual([
      '640034',
      '64001f',
      '42e01f',
    ]);
  });

  it('a profile outside the fixed order comes after the known ones', () => {
    // High 4:4:4 Predictive: the server's answer never picks it (02 §8.4).
    expect(keys(videoCodecPreferences([h264('f4001f'), h264('42e01f'), RTX], 'h264/6400'))).toEqual([
      'h264/42e0',
      'h264/f400',
      'rtx',
    ]);
  });

  it('is case-insensitive about the mime type', () => {
    const lower = [
      { ...h264('640034'), mimeType: 'video/h264' },
      { ...RTX, mimeType: 'video/RTX' },
    ];
    expect(videoCodecPreferences(lower, 'h264/6400')).toHaveLength(2);
  });

  it('no H.264 → LocalError h264_unavailable (05 §13.4 step 8)', () => {
    for (const caps of [[], [other('video/VP8'), RTX], [h264('42e01f', 0)]]) {
      let err: unknown;
      try {
        videoCodecPreferences(caps, 'h264/6400');
      } catch (e) {
        err = e;
      }
      expect(err).toBeInstanceOf(LocalError);
      expect(err).toMatchObject({ code: 'h264_unavailable' });
      expect(canSendH264(caps)).toBe(false);
    }
    expect(canSendH264(CHROME)).toBe(true);
  });
});

describe('audioCodecPreferences (05 §13.4 step 4)', () => {
  it('keeps only audio/opus', () => {
    const opus: RTCRtpCodec = { mimeType: 'audio/opus', clockRate: 48000, channels: 2, sdpFmtpLine: 'minptime=10' };
    const caps: RTCRtpCodec[] = [
      opus,
      { mimeType: 'audio/red', clockRate: 48000, channels: 2 },
      { mimeType: 'audio/G722', clockRate: 8000 },
      { mimeType: 'audio/telephone-event', clockRate: 48000 },
    ];
    expect(audioCodecPreferences(caps)).toEqual([opus]);
    expect(audioCodecPreferences([])).toEqual([]);
  });
});
