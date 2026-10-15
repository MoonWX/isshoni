// What the debug overlay (DebugOverlay.tsx, 05 §10.7) says about a stats sample and the server's view: one line of
// text per row, the parts that the browser or the server reported, joined with " · ". Pure functions, with every
// message key written out so check:i18n sees it; numbers go through Intl with the page's language (05 §16.5).
//
// Only what a sample and ServerStats hold is shown, and neither has an IP address: of the selected candidate pair a
// sample keeps the local candidate's type and transport (lib/stats/summarize.ts), and ServerStats is rates and loss
// (01 §8.11).
import type { TFunction } from 'i18next';

import type { AudioInSample, OutboundSample, PCSample, VideoInSample } from '../lib/stats/summarize';
import type { ServerLayerStats, ServerSubStats } from '../protocol/types.gen';

/** The number formats of the overlay, for the page's language. */
export interface DebugNumbers {
  /** Counts, kbit/s and ms: whole numbers with grouping. */
  readonly whole: Intl.NumberFormat;
  /** Pixels of a frame size: whole numbers without grouping (1920×1080). */
  readonly pixels: Intl.NumberFormat;
  /** fps, seconds, levels and percentages: up to one decimal. */
  readonly decimal: Intl.NumberFormat;
  /** An audio level, 0–1: two decimals. */
  readonly level: Intl.NumberFormat;
}

export function debugNumbers(language: string): DebugNumbers {
  return {
    whole: new Intl.NumberFormat(language, { maximumFractionDigits: 0 }),
    pixels: new Intl.NumberFormat(language, { maximumFractionDigits: 0, useGrouping: false }),
    decimal: new Intl.NumberFormat(language, { maximumFractionDigits: 1 }),
    level: new Intl.NumberFormat(language, { minimumFractionDigits: 2, maximumFractionDigits: 2 }),
  };
}

/** The parts that are there, as one line. */
function line(parts: readonly (string | null | undefined | false)[]): string | null {
  const shown = parts.filter((p): p is string => typeof p === 'string' && p !== '');
  return shown.length === 0 ? null : shown.join(' · ');
}

const kbps = (t: TFunction, n: DebugNumbers, value: number): string =>
  t('viewer.debug.value.kbps', { kbps: n.whole.format(value) });

const size = (t: TFunction, n: DebugNumbers, width?: number, height?: number): string | null =>
  width === undefined || height === undefined
    ? null
    : t('viewer.debug.value.size', { width: n.pixels.format(width), height: n.pixels.format(height) });

const fps = (t: TFunction, n: DebugNumbers, value?: number): string | null =>
  value === undefined ? null : t('viewer.debug.value.fps', { fps: n.decimal.format(value) });

/** "ExternalDecoder (hardware)": a coder's name, and whether the browser says it is power-efficient. */
function coder(t: TFunction, name: string | undefined, hw: boolean | undefined): string | null {
  if (name === undefined) return null;
  if (hw === undefined) return name;
  return hw ? t('viewer.debug.value.hardware', { name }) : t('viewer.debug.value.software', { name });
}

/** "sub PC 1": a PC of the page, by its kind and generation (01 §9). */
export function pcName(t: TFunction, pc: PCSample): string {
  return t('viewer.debug.pc', { pc: pc.pc, gen: pc.gen });
}

/** "connected · host udp · RTT 3 ms · out 2,400 kbit/s". */
export function pcLine(t: TFunction, n: DebugNumbers, pc: PCSample): string {
  // "host udp": the selected pair's local candidate, its type and transport, never its address.
  const candidate = [pc.candidateType, pc.transport].filter((p) => p !== undefined).join(' ');
  return (
    line([
      pc.state,
      candidate,
      pc.rttMs !== undefined && t('viewer.debug.value.rtt', { ms: n.whole.format(pc.rttMs) }),
      pc.availableOutgoingBitrate !== undefined &&
        t('viewer.debug.value.out', { kbps: n.whole.format(pc.availableOutgoingBitrate / 1000) }),
    ]) ?? pc.state
  );
}

/** A tile's received video: "1920×1080 · 59.8 fps · 7,400 kbit/s · h264/6400". */
export function videoLine(t: TFunction, n: DebugNumbers, v: VideoInSample): string {
  return (
    line([size(t, n, v.frameWidth, v.frameHeight), fps(t, n, v.framesPerSecond), kbps(t, n, v.kbps), v.codec]) ??
    kbps(t, n, v.kbps)
  );
}

/** The decoder: "ExternalDecoder (hardware)"; null where the browser doesn't say (Chrome: only while capturing). */
export function decoderLine(t: TFunction, v: VideoInSample): string | null {
  return coder(t, v.decoderImplementation, v.powerEfficientDecoder);
}

/** Frames: "decoded 1,234 · dropped 2 · key frames 3 · freezes 0 (0 s)". */
export function framesLine(t: TFunction, n: DebugNumbers, v: VideoInSample): string | null {
  return line([
    v.framesDecoded !== undefined && t('viewer.debug.value.decoded', { count: n.whole.format(v.framesDecoded) }),
    v.framesDropped !== undefined && t('viewer.debug.value.dropped', { count: n.whole.format(v.framesDropped) }),
    v.keyFramesDecoded !== undefined &&
      t('viewer.debug.value.keyFrames', { count: n.whole.format(v.keyFramesDecoded) }),
    v.freezeCount !== undefined &&
      t('viewer.debug.value.freezes', {
        count: n.whole.format(v.freezeCount),
        seconds: n.decimal.format(v.totalFreezesDuration ?? 0),
      }),
  ]);
}

/** The network as the video saw it: "lost 12 · jitter buffer 41 ms · PLI 1 · NACK 4". */
export function videoNetworkLine(t: TFunction, n: DebugNumbers, v: VideoInSample): string {
  return (
    line([
      t('viewer.debug.value.lost', { count: n.whole.format(v.packetsLost) }),
      v.jitterBufferMs !== undefined && t('viewer.debug.value.jitter', { ms: n.whole.format(v.jitterBufferMs) }),
      v.pliCount !== undefined && t('viewer.debug.value.pli', { count: n.whole.format(v.pliCount) }),
      v.nackCount !== undefined && t('viewer.debug.value.nack', { count: n.whole.format(v.nackCount) }),
    ]) ?? ''
  );
}

/** A tile's received sound: "32 kbit/s · level 0.21 · concealed 0.4 % · lost 0 · opus". */
export function audioLine(t: TFunction, n: DebugNumbers, a: AudioInSample): string {
  return (
    line([
      kbps(t, n, a.kbps),
      a.audioLevel !== undefined && t('viewer.debug.value.level', { level: n.level.format(a.audioLevel) }),
      a.concealedRatio !== undefined &&
        t('viewer.debug.value.concealed', { percent: n.decimal.format(a.concealedRatio * 100) }),
      t('viewer.debug.value.lost', { count: n.whole.format(a.packetsLost) }),
      a.codec,
    ]) ?? kbps(t, n, a.kbps)
  );
}

/** What the server forwards of a subscription (stats.watch): "high · 7,600 kbit/s · loss 0.2 % · dropped 0". */
export function serverSubLine(t: TFunction, n: DebugNumbers, s: ServerSubStats): string {
  return (
    line([
      s.layer,
      kbps(t, n, s.bitrate / 1000),
      t('viewer.debug.value.loss', { percent: n.decimal.format(s.lossPct) }),
      t('viewer.debug.value.dropped', { count: n.whole.format(s.dropped) }),
    ]) ?? ''
  );
}

/** "video f" or "audio": one sent track or simulcast layer. */
export function outboundName(t: TFunction, o: Pick<OutboundSample, 'kind' | 'rid'>): string {
  return o.rid === undefined ? o.kind : t('viewer.debug.layer', { kind: o.kind, rid: o.rid });
}

/** "1280×720 · 30 fps · 2,500 kbit/s · limited by bandwidth · OpenH264 (software)". */
export function outboundLine(t: TFunction, n: DebugNumbers, o: OutboundSample): string {
  const limited = o.qualityLimitationReason !== undefined && o.qualityLimitationReason !== 'none';
  return (
    line([
      size(t, n, o.frameWidth, o.frameHeight),
      fps(t, n, o.framesPerSecond),
      kbps(t, n, o.kbps),
      limited && t('viewer.debug.value.limited', { reason: o.qualityLimitationReason }),
      coder(t, o.encoderImplementation, o.powerEfficientEncoder),
    ]) ?? kbps(t, n, o.kbps)
  );
}

/** What the server receives of one of this page's layers (stats.watch): "2,400 kbit/s · loss 0 %". */
export function serverLayerLine(t: TFunction, n: DebugNumbers, l: ServerLayerStats): string {
  return (
    line([kbps(t, n, l.bitrate / 1000), t('viewer.debug.value.loss', { percent: n.decimal.format(l.lossPct) })]) ?? ''
  );
}

/** The server's estimate of this connection's downlink: "24,000 kbit/s". */
export function downlinkLine(t: TFunction, n: DebugNumbers, bitsPerSecond: number): string {
  return kbps(t, n, bitsPerSecond / 1000);
}
