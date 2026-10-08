// The sharer's upload and CPU hints (05 §13.7), from the full layer's outbound stats. Pure: the share samples its
// sender every 2 s (05 §18) and feeds each sample to a HintDetector, which says what the panel should show.
//
// - Upload: the full layer reports qualityLimitationReason `bandwidth` in 3 consecutive samples (6 s) while its
//   height is below the size it is meant to send → "Your upload allows about 720p", the height rounded to 360, 480,
//   540, 720, 900 or 1080. It clears after 10 s without a bandwidth limit.
// - CPU: `cpu` in 3 consecutive samples (6 s) → "Your computer is struggling to encode. …"; it clears after 10 s
//   without a CPU limit.
//
// Browsers run their own congestion control (plan "Rate control"), so a hint only explains what the sharer sees;
// nothing is changed because of it.
import type { ShareHint, ShareStatsSample } from '../platform/types';
import { RIDHigh } from '../protocol/types.gen';

/** This many consecutive limited samples turn a hint on (3 samples of 2 s: 6 s). */
export const HINT_ON_SAMPLES = 3;
/** A hint goes away after this long without its limit. */
export const HINT_OFF_MS = 10_000;
/** The heights an upload hint names. */
export const HINT_HEIGHTS: readonly number[] = [360, 480, 540, 720, 900, 1080];
/** An encoded height counts as "below the target" under this fraction of it (encoders round sizes a little). */
const BELOW_TARGET = 0.95;

/** The nearest of HINT_HEIGHTS; the lower one on a tie. */
export function roundHintHeight(height: number): number {
  let best = HINT_HEIGHTS[0] ?? height;
  for (const h of HINT_HEIGHTS) {
    if (Math.abs(h - height) < Math.abs(best - height)) best = h;
  }
  return best;
}

/** What a detector reads from one stats sample: the full layer only. */
export interface HintSample {
  /** When it was taken, in ms (performance.now()). */
  readonly at: number;
  /** The full layer's qualityLimitationReason; undefined while it sends nothing (paused, or no stats yet). */
  readonly limit?: 'none' | 'bandwidth' | 'cpu' | 'other' | undefined;
  /** The height the full layer encodes now. */
  readonly height?: number | undefined;
  /** The height it is meant to encode: the source's, scaled by its scaleResolutionDownBy. */
  readonly targetHeight?: number | undefined;
}

/** The full layer's part of a ShareStatsSample (rid `f`, or the only layer when there is no simulcast). */
export function hintSample(sample: ShareStatsSample, targetHeight: number | undefined): HintSample {
  const full =
    sample.layers.find((l) => l.rid === RIDHigh) ?? (sample.layers.length === 1 ? sample.layers[0] : undefined);
  return { at: sample.at, limit: full?.limit, height: full?.height, targetHeight };
}

export interface HintDetector {
  /** Takes the next sample and returns the hint now in effect (null: none). */
  push(sample: HintSample): ShareHint | null;
  /** Forgets everything (the share ended, or was rebuilt). */
  reset(): void;
}

export function createHintDetector(): HintDetector {
  let uploadRun = 0;
  let cpuRun = 0;
  let lastUploadAt = Number.NEGATIVE_INFINITY;
  let lastCpuAt = Number.NEGATIVE_INFINITY;
  let hint: ShareHint | null = null;

  return {
    push(s) {
      const belowTarget =
        s.height !== undefined && s.targetHeight !== undefined && s.height < s.targetHeight * BELOW_TARGET;
      const uploadLimited = s.limit === 'bandwidth' && belowTarget;

      if (s.limit === 'bandwidth') lastUploadAt = s.at;
      if (s.limit === 'cpu') lastCpuAt = s.at;
      uploadRun = uploadLimited ? uploadRun + 1 : 0;
      cpuRun = s.limit === 'cpu' ? cpuRun + 1 : 0;

      if (cpuRun >= HINT_ON_SAMPLES) {
        hint = { kind: 'cpu-limited' };
      } else if (uploadLimited && (uploadRun >= HINT_ON_SAMPLES || hint?.kind === 'upload-limited')) {
        // On, or still on: the height follows what the encoder manages now.
        hint = { kind: 'upload-limited', approxHeight: roundHintHeight(s.height ?? 0) };
      } else if (hint?.kind === 'upload-limited' && s.at - lastUploadAt >= HINT_OFF_MS) {
        hint = null;
      } else if (hint?.kind === 'cpu-limited' && s.at - lastCpuAt >= HINT_OFF_MS) {
        hint = null;
      }
      return hint;
    },
    reset() {
      uploadRun = 0;
      cpuRun = 0;
      lastUploadAt = Number.NEGATIVE_INFINITY;
      lastCpuAt = Number.NEGATIVE_INFINITY;
      hint = null;
    },
  };
}

/** Whether two hints say the same thing, so a listener hears only changes. */
export function sameHint(a: ShareHint | null, b: ShareHint | null): boolean {
  if (a === null || b === null) return a === b;
  if (a.kind !== b.kind) return false;
  return a.kind !== 'upload-limited' || (b.kind === 'upload-limited' && a.approxHeight === b.approxHeight);
}
