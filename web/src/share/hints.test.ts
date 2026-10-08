import { describe, expect, it } from 'vitest';

import type { ShareStatsSample } from '../platform/types';
import {
  createHintDetector,
  HINT_OFF_MS,
  HINT_ON_SAMPLES,
  hintSample,
  roundHintHeight,
  sameHint,
  type HintSample,
} from './hints';

/** A sample every 2 s, like the share takes them (05 §18). */
const at = (n: number): number => n * 2000;
const limited = (n: number, height = 720, targetHeight = 1080): HintSample => ({
  at: at(n),
  limit: 'bandwidth',
  height,
  targetHeight,
});
const cpu = (n: number): HintSample => ({ at: at(n), limit: 'cpu', height: 1080, targetHeight: 1080 });
const fine = (n: number): HintSample => ({ at: at(n), limit: 'none', height: 1080, targetHeight: 1080 });

describe('the upload hint (05 §13.7)', () => {
  it('appears after 3 consecutive bandwidth-limited samples below the target size', () => {
    const d = createHintDetector();
    expect(HINT_ON_SAMPLES).toBe(3);
    expect(d.push(limited(0))).toBeNull();
    expect(d.push(limited(1))).toBeNull();
    expect(d.push(limited(2))).toEqual({ kind: 'upload-limited', approxHeight: 720 });
  });

  it('needs them in a row', () => {
    const d = createHintDetector();
    d.push(limited(0));
    d.push(limited(1));
    expect(d.push(fine(2))).toBeNull();
    d.push(limited(3));
    expect(d.push(limited(4))).toBeNull();
    expect(d.push(limited(5))).not.toBeNull();
  });

  it('not while the full layer still encodes its target size: the limit costs bitrate, not resolution', () => {
    const d = createHintDetector();
    for (let n = 0; n < 6; n++) expect(d.push(limited(n, 1080, 1080))).toBeNull();
    // Encoders round sizes a little: 1072 of 1080 is still the target.
    for (let n = 6; n < 12; n++) expect(d.push(limited(n, 1072, 1080))).toBeNull();
  });

  it('not without a height or a target (the layer sends nothing yet)', () => {
    const d = createHintDetector();
    for (let n = 0; n < 4; n++) expect(d.push({ at: at(n), limit: 'bandwidth' })).toBeNull();
    for (let n = 4; n < 8; n++) expect(d.push({ at: at(n) })).toBeNull();
  });

  it('rounds the height to 360, 480, 540, 720, 900 or 1080', () => {
    expect([200, 360, 400, 470, 500, 530, 620, 700, 800, 850, 1000, 1080, 1440].map(roundHintHeight)).toEqual([
      360, 360, 360, 480, 480, 540, 540, 720, 720, 900, 1080, 1080, 1080,
    ]);
    const d = createHintDetector();
    d.push(limited(0, 810));
    d.push(limited(1, 810));
    expect(d.push(limited(2, 810))).toEqual({ kind: 'upload-limited', approxHeight: 720 });
  });

  it('follows the height the encoder manages while it stays on', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(limited(n, 720));
    expect(d.push(limited(3, 540))).toEqual({ kind: 'upload-limited', approxHeight: 540 });
  });

  it('clears after 10 s without a bandwidth limit, not before', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(limited(n));
    // The last limited sample was at 4 s.
    expect(HINT_OFF_MS).toBe(10_000);
    expect(d.push(fine(3))).not.toBeNull();
    expect(d.push(fine(4))).not.toBeNull();
    expect(d.push(fine(5))).not.toBeNull();
    expect(d.push(fine(6))).not.toBeNull(); // 12 s: 8 s after it
    expect(d.push(fine(7))).toBeNull(); // 14 s: 10 s after it
  });

  it('a limited sample in between keeps it on and restarts the 10 s', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(limited(n));
    d.push(fine(3));
    d.push(fine(4));
    expect(d.push(limited(5))).not.toBeNull();
    for (let n = 6; n < 10; n++) expect(d.push(fine(n))).not.toBeNull();
    expect(d.push(fine(10))).toBeNull();
  });

  it('a bandwidth limit at the target size keeps a shown hint from clearing early, without re-raising it', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(limited(n));
    for (let n = 3; n < 9; n++) expect(d.push(limited(n, 1080, 1080))).not.toBeNull();
    // 10 s after the last bandwidth-limited sample (at 16 s).
    for (let n = 9; n < 13; n++) expect(d.push(fine(n))).not.toBeNull();
    expect(d.push(fine(13))).toBeNull();
  });
});

describe('the CPU hint (05 §13.7)', () => {
  it('appears after cpu for 3 samples (6 s) and clears 10 s after the last one', () => {
    const d = createHintDetector();
    expect(d.push(cpu(0))).toBeNull();
    expect(d.push(cpu(1))).toBeNull();
    expect(d.push(cpu(2))).toEqual({ kind: 'cpu-limited' });
    for (let n = 3; n < 7; n++) expect(d.push(fine(n))).toEqual({ kind: 'cpu-limited' });
    expect(d.push(fine(7))).toBeNull();
  });

  it('replaces an upload hint, and the other way round', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(limited(n));
    d.push(cpu(3));
    expect(d.push(cpu(4))).toEqual({ kind: 'upload-limited', approxHeight: 720 });
    expect(d.push(cpu(5))).toEqual({ kind: 'cpu-limited' });
    d.push(limited(6));
    d.push(limited(7));
    expect(d.push(limited(8))).toEqual({ kind: 'upload-limited', approxHeight: 720 });
  });

  it('reset() forgets the run and the hint', () => {
    const d = createHintDetector();
    for (let n = 0; n < 3; n++) d.push(cpu(n));
    d.reset();
    expect(d.push(cpu(3))).toBeNull();
    expect(d.push(cpu(4))).toBeNull();
    expect(d.push(cpu(5))).toEqual({ kind: 'cpu-limited' });
  });
});

describe('hintSample: the full layer of a stats sample', () => {
  const sample = (layers: ShareStatsSample['layers']): ShareStatsSample => ({ at: 4000, layers, audioKbps: 0 });

  it('reads rid f', () => {
    const s = sample([
      { rid: 'q', kbps: 250, height: 360, limit: 'none' },
      { rid: 'f', kbps: 2500, height: 720, limit: 'bandwidth' },
    ]);
    expect(hintSample(s, 1080)).toEqual({ at: 4000, limit: 'bandwidth', height: 720, targetHeight: 1080 });
  });

  it('takes the only layer when there is no simulcast, and nothing when f is missing', () => {
    expect(hintSample(sample([{ rid: 'x', kbps: 1, height: 540, limit: 'cpu' }]), 1080)).toMatchObject({
      limit: 'cpu',
      height: 540,
    });
    const none = hintSample(sample([]), undefined);
    expect(none.limit).toBeUndefined();
    expect(none.height).toBeUndefined();
  });
});

describe('sameHint', () => {
  it('compares kind and height', () => {
    expect(sameHint(null, null)).toBe(true);
    expect(sameHint(null, { kind: 'cpu-limited' })).toBe(false);
    expect(sameHint({ kind: 'cpu-limited' }, { kind: 'cpu-limited' })).toBe(true);
    expect(sameHint({ kind: 'upload-limited', approxHeight: 720 }, { kind: 'upload-limited', approxHeight: 720 })).toBe(
      true,
    );
    expect(sameHint({ kind: 'upload-limited', approxHeight: 720 }, { kind: 'upload-limited', approxHeight: 540 })).toBe(
      false,
    );
    expect(sameHint({ kind: 'upload-limited', approxHeight: 720 }, { kind: 'cpu-limited' })).toBe(false);
  });
});
