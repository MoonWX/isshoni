import { describe, expect, it } from 'vitest';

import type { Encoding } from '../protocol/types.gen';
import {
  applyEncodings,
  buildSendEncodings,
  buildSingleEncoding,
  fullLayer,
  layerScales,
  needsRescale,
  scaleDownBy,
  sourceSize,
} from './encodings';

/** ShareParams.encodings for the Auto preset, in wire order: high first (01 §8.7). */
const WIRE: Encoding[] = [
  { rid: 'f', layer: 'high', active: true, maxBitrate: 8_000_000, maxFramerate: 60, maxPixels: 2_073_600 },
  { rid: 'q', layer: 'low', active: true, maxBitrate: 300_000, maxFramerate: 15, maxPixels: 230_400 },
];

const size = (width: number, height: number) => ({ width, height });

describe('scaleResolutionDownBy = max(1, sqrt(w·h / maxPixels)) (01 §8.7)', () => {
  it.each([
    ['1080p', 1920, 1080, 1, 3],
    ['720p (smaller than the full budget: never scaled up)', 1280, 720, 1, 2],
    ['1440p', 2560, 1440, 4 / 3, 4],
    ['2160p', 3840, 2160, 2, 6],
    ['ultrawide 3440×1440', 3440, 1440, Math.sqrt(4_953_600 / 2_073_600), Math.sqrt(4_953_600 / 230_400)],
    ['a small window, under the preview budget too', 480, 360, 1, 1],
  ])('%s', (_name, w, h, full, preview) => {
    expect(scaleDownBy(size(w, h), 2_073_600)).toBeCloseTo(full, 10);
    expect(scaleDownBy(size(w, h), 230_400)).toBeCloseTo(preview, 10);
  });

  it('keeps the aspect ratio: an ultrawide source lands on its pixel budget, not on 16:9', () => {
    const scale = scaleDownBy(size(3440, 1440), 2_073_600);
    expect((3440 / scale) * (1440 / scale)).toBeCloseTo(2_073_600, 3);
    expect(3440 / scale / (1440 / scale)).toBeCloseTo(3440 / 1440, 10);
  });

  it('is 1 without a pixel budget', () => {
    expect(scaleDownBy(size(3840, 2160), 0)).toBe(1);
  });
});

describe('sourceSize', () => {
  it('reads the track settings, and is null while the browser reports no size', () => {
    expect(sourceSize({ getSettings: () => ({ width: 1920, height: 1080 }) })).toEqual({ width: 1920, height: 1080 });
    expect(sourceSize({ getSettings: () => ({}) })).toBeNull();
    expect(sourceSize({ getSettings: () => ({ width: 0, height: 1080 }) })).toBeNull();
  });
});

describe('buildSendEncodings (05 §13.4 step 2)', () => {
  it('wire order (f, q) → complete sendEncodings in ascending order (q, f), every field set', () => {
    expect(buildSendEncodings(WIRE, size(1920, 1080))).toEqual([
      { rid: 'q', active: true, maxBitrate: 300_000, maxFramerate: 15, scaleResolutionDownBy: 3 },
      { rid: 'f', active: true, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
    ]);
  });

  it('is ascending whatever order the server lists them in', () => {
    expect(buildSendEncodings([...WIRE].reverse(), size(1920, 1080)).map((e) => e.rid)).toEqual(['q', 'f']);
  });

  it.each([
    ['1080p', 1920, 1080, 3, 1],
    ['1440p', 2560, 1440, 4, 4 / 3],
    ['2160p', 3840, 2160, 6, 2],
    ['3440×1440', 3440, 1440, Math.sqrt(4_953_600 / 230_400), Math.sqrt(4_953_600 / 2_073_600)],
  ])('scales a %s source to each layer’s budget', (_name, w, h, q, f) => {
    const [low, high] = buildSendEncodings(WIRE, size(w, h));
    expect(low?.scaleResolutionDownBy).toBeCloseTo(q, 10);
    expect(high?.scaleResolutionDownBy).toBeCloseTo(f, 10);
  });

  it('carries the server’s active flags and caps (a layer that starts paused)', () => {
    const paused = WIRE.map((e) => (e.rid === 'f' ? { ...e, active: false, maxBitrate: 4_000_000 } : e));
    expect(buildSendEncodings(paused, size(1920, 1080))[1]).toMatchObject({
      rid: 'f',
      active: false,
      maxBitrate: 4_000_000,
    });
  });

  it('an unknown source size still gives every layer a scale: the proportions of the budgets', () => {
    expect(buildSendEncodings(WIRE, null).map((e) => [e.rid, e.scaleResolutionDownBy])).toEqual([
      ['q', 3],
      ['f', 1],
    ]);
    expect(layerScales(WIRE, null)).toEqual(
      new Map([
        ['f', 1],
        ['q', 3],
      ]),
    );
  });

  it('the single-layer fallback is the full layer without a rid', () => {
    expect(fullLayer(WIRE)?.rid).toBe('f');
    expect(buildSingleEncoding(WIRE, size(2560, 1440))).toEqual([
      { active: true, maxBitrate: 8_000_000, maxFramerate: 60, scaleResolutionDownBy: 4 / 3 },
    ]);
    expect(buildSingleEncoding([], size(1920, 1080))).toEqual([]);
  });
});

describe('applyEncodings: later setParameters edits match by rid, never by index', () => {
  const sender = (order: string[]): Pick<RTCRtpSendParameters, 'encodings'> => ({
    encodings: buildSendEncodings(WIRE, size(1920, 1080)).sort(
      (a, b) => order.indexOf(a.rid ?? '') - order.indexOf(b.rid ?? ''),
    ),
  });

  it('a hint’s active flags are applied per rid', () => {
    const params = sender(['q', 'f']);
    const hint = WIRE.map((e) => (e.rid === 'f' ? { ...e, active: false } : e));
    expect(applyEncodings(params, hint, size(1920, 1080))).toBe(true);
    expect(params.encodings.map((e) => [e.rid, e.active])).toEqual([
      ['q', true],
      ['f', false],
    ]);
  });

  it('a reversed getParameters() order still updates the right layer', () => {
    const params = sender(['f', 'q']);
    const hint = WIRE.map((e) =>
      e.rid === 'f' ? { ...e, active: false, maxBitrate: 5_000_000 } : { ...e, maxFramerate: 10 },
    );
    expect(applyEncodings(params, hint, size(1920, 1080))).toBe(true);
    expect(params.encodings).toEqual([
      { rid: 'f', active: false, maxBitrate: 5_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
      { rid: 'q', active: true, maxBitrate: 300_000, maxFramerate: 10, scaleResolutionDownBy: 3 },
    ]);
  });

  it('sets scaleResolutionDownBy from maxPixels and the source size now', () => {
    const params = sender(['q', 'f']);
    expect(applyEncodings(params, WIRE, size(3840, 2160))).toBe(true);
    expect(params.encodings.map((e) => [e.rid, e.scaleResolutionDownBy])).toEqual([
      ['q', 6],
      ['f', 2],
    ]);
  });

  it('says when nothing changed, so no setParameters is needed', () => {
    expect(applyEncodings(sender(['q', 'f']), WIRE, size(1920, 1080))).toBe(false);
  });

  it('with an unknown source size, the scales the sender has stay', () => {
    const params = { encodings: buildSendEncodings(WIRE, size(3840, 2160)) };
    const hint = WIRE.map((e) => (e.rid === 'q' ? { ...e, maxBitrate: 200_000 } : e));
    expect(applyEncodings(params, hint, null)).toBe(true);
    expect(params.encodings.map((e) => [e.rid, e.scaleResolutionDownBy, e.maxBitrate])).toEqual([
      ['q', 6, 200_000],
      ['f', 2, 8_000_000],
    ]);
  });

  it('leaves alone what the list doesn’t name, and ignores a layer the sender doesn’t have', () => {
    const params = sender(['q', 'f']);
    const onlyMid: Encoding[] = [
      { rid: 'h', layer: 'high', active: false, maxBitrate: 1, maxFramerate: 1, maxPixels: 1 },
    ];
    expect(applyEncodings(params, onlyMid, size(1920, 1080))).toBe(false);
    expect(params).toEqual(sender(['q', 'f']));
  });

  it('the single encoding without a rid is the f layer', () => {
    const params = { encodings: buildSingleEncoding(WIRE, size(1920, 1080)) };
    const hint = WIRE.map((e) => (e.rid === 'f' ? { ...e, maxBitrate: 2_000_000 } : { ...e, active: false }));
    expect(applyEncodings(params, hint, size(1920, 1080))).toBe(true);
    expect(params.encodings).toEqual([
      { active: true, maxBitrate: 2_000_000, maxFramerate: 60, scaleResolutionDownBy: 1 },
    ]);
  });
});

describe('needsRescale: a resized source (05 §13.4 step 7)', () => {
  const params = { encodings: buildSendEncodings(WIRE, size(1920, 1080)) };

  it('is false for the same size and for a change of 10 % or less', () => {
    expect(needsRescale(params, WIRE, size(1920, 1080))).toBe(false);
    // q: sqrt(2000·1080 / 230 400) = 3.06, 2 % more than 3.
    expect(needsRescale(params, WIRE, size(2000, 1080))).toBe(false);
  });

  it('is true once a layer’s scale moves by more than 10 %', () => {
    expect(needsRescale(params, WIRE, size(2560, 1440))).toBe(true);
    // Smaller: f stays 1, but q goes from 3 to 2.
    expect(needsRescale(params, WIRE, size(1280, 720))).toBe(true);
  });

  it('matches by rid, whatever the order', () => {
    const reversed = { encodings: [...params.encodings].reverse() };
    expect(needsRescale(reversed, WIRE, size(1920, 1080))).toBe(false);
    expect(needsRescale(reversed, WIRE, size(3840, 2160))).toBe(true);
  });
});
