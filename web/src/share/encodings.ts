// Video encodings for the pub PC (05 §13.4 step 2, 01 §8.7). Pure functions.
//
// ShareParams.encodings comes in wire order, high first (`f`, then `q`). The browser gets the layers in ascending
// order (`q`, then `f`), as the S4 spike measured them, and complete: every entry carries rid, active, maxBitrate,
// maxFramerate and scaleResolutionDownBy, so nothing is left to browser defaults. (Without scaleResolutionDownBy the
// WebRTC default is 2^(n−1−i): the first entry at half size and the last at full size, uncapped, until the first
// setParameters.)
//
//   scaleResolutionDownBy = max(1, sqrt(width × height / maxPixels))
//
// maxPixels is a pixel budget, not a size: an ultrawide 3440×1440 source keeps its aspect ratio and is scaled until
// its area fits. After the transceiver exists, every read or write finds an encoding by rid, never by index:
// applyEncodings() edits what getParameters() returned, in whatever order the browser lists it.
import { RIDHigh, type Encoding } from '../protocol/types.gen';

/** The size of the captured source, from MediaStreamTrack.getSettings(). */
export interface SourceSize {
  readonly width: number;
  readonly height: number;
}

/** The source size of a video track; null while the browser doesn't report one (before the first frame). */
export function sourceSize(track: Pick<MediaStreamTrack, 'getSettings'>): SourceSize | null {
  const { width, height } = track.getSettings();
  if (width === undefined || height === undefined || !(width > 0) || !(height > 0)) return null;
  return { width, height };
}

/** max(1, sqrt(width × height / maxPixels)); 1 without a pixel budget. */
export function scaleDownBy(size: SourceSize, maxPixels: number): number {
  if (!(maxPixels > 0)) return 1;
  return Math.max(1, Math.sqrt((size.width * size.height) / maxPixels));
}

/**
 * scaleResolutionDownBy of each layer, by rid. With an unknown source size the source is taken to fill the largest
 * budget exactly, so the layers still get their proportions (f 1, q 3 for 1080p and 360p); the size re-check
 * corrects them once the track reports its size.
 */
export function layerScales(encodings: readonly Encoding[], size: SourceSize | null): Map<string, number> {
  const largest = Math.max(0, ...encodings.map((e) => e.maxPixels));
  const scales = new Map<string, number>();
  for (const e of encodings) {
    const scale =
      size !== null ? scaleDownBy(size, e.maxPixels) : scaleDownBy({ width: largest, height: 1 }, e.maxPixels);
    scales.set(e.rid, scale);
  }
  return scales;
}

function toSendEncoding(e: Encoding, scale: number, withRid: boolean): RTCRtpEncodingParameters {
  return {
    ...(withRid ? { rid: e.rid } : {}),
    active: e.active,
    maxBitrate: e.maxBitrate,
    maxFramerate: e.maxFramerate,
    scaleResolutionDownBy: scale,
  };
}

/** Ascending: the smallest pixel budget first, then the lowest bitrate; equal layers keep the reverse wire order. */
function ascending(encodings: readonly Encoding[]): Encoding[] {
  return [...encodings].reverse().sort((a, b) => a.maxPixels - b.maxPixels || a.maxBitrate - b.maxBitrate);
}

/**
 * The complete sendEncodings for addTransceiver: ShareParams.encodings (wire order, high first) in ascending order
 * (`q`, then `f`), each with rid, active, maxBitrate, maxFramerate and scaleResolutionDownBy.
 */
export function buildSendEncodings(
  encodings: readonly Encoding[],
  size: SourceSize | null,
): RTCRtpEncodingParameters[] {
  const scales = layerScales(encodings, size);
  return ascending(encodings).map((e) => toSendEncoding(e, scales.get(e.rid) ?? 1, true));
}

/** The full layer of a list: rid `f`, else the one with the largest budget. */
export function fullLayer(encodings: readonly Encoding[]): Encoding | undefined {
  return encodings.find((e) => e.rid === RIDHigh) ?? ascending(encodings).at(-1);
}

/**
 * The fallback when the browser rejects simulcast (05 §13.4 step 2): one encoding, the full layer, without a rid
 * (01 §9 rule 4: no rids = a single `f` layer). [] when the params list no encoding at all.
 */
export function buildSingleEncoding(
  encodings: readonly Encoding[],
  size: SourceSize | null,
): RTCRtpEncodingParameters[] {
  const full = fullLayer(encodings);
  if (full === undefined) return [];
  return [toSendEncoding(full, layerScales(encodings, size).get(full.rid) ?? 1, false)];
}

/** The rid an encoding of getParameters() stands for: its own, or `f` for the single layer that has none. */
function ridOf(e: RTCRtpEncodingParameters): string {
  return e.rid ?? RIDHigh;
}

/**
 * Edits the encodings of a sender's getParameters() result in place, each matched by rid (never by index): active,
 * maxBitrate, maxFramerate, and scaleResolutionDownBy from maxPixels and the source size. An encoding the list
 * doesn't name is left alone, and so is a list entry the sender doesn't have (a layer the browser refused). Returns
 * whether anything changed, so the caller can skip a setParameters() that would change nothing.
 */
export function applyEncodings(
  params: Pick<RTCRtpSendParameters, 'encodings'>,
  encodings: readonly Encoding[],
  size: SourceSize | null,
): boolean {
  const scales = layerScales(encodings, size);
  let changed = false;
  for (const current of params.encodings) {
    const want = encodings.find((e) => e.rid === ridOf(current));
    if (want === undefined) continue;
    const next = toSendEncoding(want, scales.get(want.rid) ?? 1, false);
    // With an unknown source size, a scale the sender already has (from the known size of before) stays.
    if (size === null && current.scaleResolutionDownBy !== undefined) delete next.scaleResolutionDownBy;
    for (const key of ['active', 'maxBitrate', 'maxFramerate', 'scaleResolutionDownBy'] as const) {
      const value = next[key];
      if (value === undefined || current[key] === value) continue;
      Object.assign(current, { [key]: value });
      changed = true;
    }
  }
  return changed;
}

/** A source resize counts when a layer's scale moves by more than this fraction (05 §18: 10 %). */
export const RESIZE_THRESHOLD = 0.1;

/**
 * Whether the source size changed enough to set scaleResolutionDownBy again (05 §13.4 step 7): some layer's scale
 * for the new size differs by more than 10 % from the one the sender has now. Encodings are matched by rid.
 */
export function needsRescale(
  params: Pick<RTCRtpSendParameters, 'encodings'>,
  encodings: readonly Encoding[],
  size: SourceSize,
): boolean {
  const scales = layerScales(encodings, size);
  return params.encodings.some((current) => {
    const want = scales.get(ridOf(current));
    if (want === undefined) return false;
    const have = current.scaleResolutionDownBy ?? 1;
    return Math.abs(want - have) > RESIZE_THRESHOLD * have;
  });
}
