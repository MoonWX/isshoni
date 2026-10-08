// getStats() → one StatsSample (05 §10.7): what the tiles, the sharer hints, the debug overlay and the e2e tests
// read, and the source of the `stats` notification (01 §8.11). Pure functions: the collector (collector.ts) calls
// getStats() and keeps the samples.
//
// A sample holds, with NO IP ADDRESSES (05 §20; of the selected candidate pair only the local candidate's type and
// transport are kept):
// - per subscribed share (a tile), the received video and audio;
// - per sent track or simulcast layer (rid), what the encoder does;
// - per PC, its state and the selected candidate pair.
// Rates (kbps, and fps where the browser reports none) are differences against the counters of the previous sample.
import { h264Key } from '../../protocol/codecs';
import {
  TrackKindAudio,
  TrackKindVideo,
  type ClientStats,
  type InboundStats,
  type OutboundStats,
  type PCKind,
  type PCStats,
  type TrackKind,
} from '../../protocol/types.gen';

/** The part of RTCStatsReport that is read; a Map of plain objects is one too (tests). */
export interface StatsReportLike {
  forEach(fn: (value: unknown, key: string) => void): void;
}

/** How a report names a track: the m-section's mid and the MediaStreamTrack's id. Browsers report one or both. */
export interface StatsTrackRef {
  readonly kind: TrackKind;
  readonly mid?: string;
  readonly trackId?: string;
}

/** One PC's getStats() result with what its owner knows about it. */
export interface PCReport {
  readonly pc: PCKind;
  readonly gen: number;
  /** RTCPeerConnection.connectionState. */
  readonly state: string;
  /** null: getStats() failed; the PC is still listed, without numbers. */
  readonly report: StatsReportLike | null;
  /** The share a track belongs to (the offer's `tracks`, 05 §9); undefined for a track that carries none. */
  shareOf(track: StatsTrackRef): string | undefined;
}

export interface PCSample {
  readonly pc: PCKind;
  readonly gen: number;
  /** connectionState. */
  readonly state: string;
  /** The selected pair's local candidate: host, srflx, prflx or relay. */
  readonly candidateType?: string;
  /** udp or tcp. */
  readonly transport?: string;
  /** currentRoundTripTime of the selected pair. */
  readonly rttMs?: number;
  /** availableOutgoingBitrate, bits/s. */
  readonly availableOutgoingBitrate?: number;
}

/** What both kinds of received track have. */
interface InboundSample {
  readonly shareId: string;
  readonly kbps: number;
  /** Cumulative, so two samples show whether media is flowing whatever the sampling interval. */
  readonly bytesReceived: number;
  readonly packetsLost: number;
  readonly jitterBufferMs?: number;
}

export interface VideoInSample extends InboundSample {
  readonly kind: typeof TrackKindVideo;
  readonly frameWidth?: number;
  readonly frameHeight?: number;
  readonly framesPerSecond?: number;
  readonly framesDecoded?: number;
  readonly framesDropped?: number;
  readonly keyFramesDecoded?: number;
  readonly freezeCount?: number;
  /** Seconds, as getStats reports it. */
  readonly totalFreezesDuration?: number;
  readonly pliCount?: number;
  readonly nackCount?: number;
  /** The H.264 CodecKey of the inbound codec ("h264/6400"); another codec's mime subtype in lowercase. */
  readonly codec?: string;
  /** Chrome exposes this and powerEfficientDecoder only to pages with an active capture. */
  readonly decoderImplementation?: string;
  readonly powerEfficientDecoder?: boolean;
}

export interface AudioInSample extends InboundSample {
  readonly kind: typeof TrackKindAudio;
  readonly audioLevel?: number;
  readonly totalAudioEnergy?: number;
  readonly concealedSamples?: number;
  readonly totalSamplesReceived?: number;
  /** concealedSamples / totalSamplesReceived, 0–1. */
  readonly concealedRatio?: number;
  readonly codec?: string;
}

/** What one tile receives. */
export interface ShareSample {
  readonly video?: VideoInSample;
  readonly audio?: AudioInSample;
}

export interface OutboundSample {
  readonly shareId: string;
  readonly kind: TrackKind;
  /** The simulcast layer; absent for audio and for a single encoding. */
  readonly rid?: string;
  readonly kbps: number;
  readonly bytesSent: number;
  readonly frameWidth?: number;
  readonly frameHeight?: number;
  readonly framesPerSecond?: number;
  /** none, cpu, bandwidth or other. */
  readonly qualityLimitationReason?: string;
  readonly encoderImplementation?: string;
  readonly powerEfficientEncoder?: boolean;
}

export interface StatsSample {
  /** Date.now() when it was taken. */
  readonly at: number;
  /** The time since the sample its rates are measured against; 0 for a first sample (its rates are 0). */
  readonly intervalMs: number;
  readonly pcs: readonly PCSample[];
  /** Per subscribed share: the tile's stats. */
  readonly shares: Readonly<Record<string, ShareSample>>;
  /** Per sent track or simulcast layer. */
  readonly outbound: readonly OutboundSample[];
}

/** The cumulative counters of one RTP stream at one sample. */
interface Counters {
  /** The stat's own timestamp (ms); the sample's time where a browser gives none. */
  readonly at: number;
  readonly bytes: number;
  readonly frames?: number;
  readonly jitterBufferDelay?: number;
  readonly jitterBufferEmitted?: number;
}

/** A sample's counters by stream, for the rates of the next one. Opaque to callers. */
export interface StatsCounters {
  readonly at: number;
  readonly streams: ReadonlyMap<string, Counters>;
}

export interface Summary {
  readonly sample: StatsSample;
  /** Give these to the next summarize() call. */
  readonly counters: StatsCounters;
}

type Stat = Readonly<Record<string, unknown>>;

const num = (s: Stat, name: string): number | undefined => {
  const v = s[name];
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
};
const str = (s: Stat, name: string): string | undefined => {
  const v = s[name];
  return typeof v === 'string' && v !== '' ? v : undefined;
};
const bool = (s: Stat, name: string): boolean | undefined => {
  const v = s[name];
  return typeof v === 'boolean' ? v : undefined;
};

const round1 = (v: number): number => Math.round(v * 10) / 10;

/** Drops the undefined members, so samples compare and serialize without them. */
function compact<T extends object>(o: T): T {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(o)) {
    if (v !== undefined) out[k] = v;
  }
  return out as T;
}

function trackKind(s: Stat): TrackKind | undefined {
  // `kind` everywhere today; `mediaType` is its old name.
  const kind = str(s, 'kind') ?? str(s, 'mediaType');
  return kind === TrackKindVideo || kind === TrackKindAudio ? kind : undefined;
}

/** kbit/s between two counter readings; 0 without an earlier reading or when the counter went back (a new stream). */
function kbps(bytes: number, now: number, prev: Counters | undefined): number {
  if (!prev || now <= prev.at || bytes < prev.bytes) return 0;
  return round1(((bytes - prev.bytes) * 8) / (now - prev.at));
}

function perSecond(count: number | undefined, now: number, prev: Counters | undefined): number | undefined {
  if (count === undefined || prev?.frames === undefined || now <= prev.at || count < prev.frames) return undefined;
  return round1(((count - prev.frames) * 1000) / (now - prev.at));
}

/**
 * The jitter buffer's delay in ms: over the interval when there is an earlier reading, else the average since the
 * track started (jitterBufferDelay / jitterBufferEmittedCount are cumulative).
 */
function jitterBufferMs(delay: number | undefined, emitted: number | undefined, prev: Counters | undefined) {
  if (delay === undefined || emitted === undefined) return undefined;
  if (prev?.jitterBufferDelay !== undefined && prev.jitterBufferEmitted !== undefined) {
    const d = delay - prev.jitterBufferDelay;
    const n = emitted - prev.jitterBufferEmitted;
    if (n > 0 && d >= 0) return Math.round((d / n) * 1000);
  }
  return emitted > 0 ? Math.round((delay / emitted) * 1000) : undefined;
}

/** "h264/6400" for H.264 (01's CodecKey), else the mime subtype: "opus", "vp8". */
function codecName(codec: Stat | undefined): string | undefined {
  if (!codec) return undefined;
  const subtype = str(codec, 'mimeType')?.split('/')[1]?.toLowerCase();
  if (subtype === 'h264') {
    const key = h264Key(str(codec, 'sdpFmtpLine') ?? '');
    return key !== '' ? key : subtype;
  }
  return subtype;
}

/**
 * The selected candidate pair: the transport's selectedCandidatePairId; Firefox has no such field and marks the
 * pair `selected`. Only the local candidate's type and protocol are read: no addresses (05 §20).
 */
function selectedPair(byId: ReadonlyMap<string, Stat>, byType: (type: string) => Stat[]): Stat | undefined {
  for (const transport of byType('transport')) {
    const id = str(transport, 'selectedCandidatePairId');
    const pair = id === undefined ? undefined : byId.get(id);
    if (pair) return pair;
  }
  const pairs = byType('candidate-pair');
  return (
    pairs.find((p) => bool(p, 'selected') === true) ??
    pairs.find((p) => bool(p, 'nominated') === true && str(p, 'state') === 'succeeded')
  );
}

function pcSample(r: PCReport, byId: ReadonlyMap<string, Stat>, byType: (type: string) => Stat[]): PCSample {
  const pair = selectedPair(byId, byType);
  const localId = pair ? str(pair, 'localCandidateId') : undefined;
  const local = localId === undefined ? undefined : byId.get(localId);
  const rtt = pair ? num(pair, 'currentRoundTripTime') : undefined;
  const outgoing = pair ? num(pair, 'availableOutgoingBitrate') : undefined;
  return compact({
    pc: r.pc,
    gen: r.gen,
    state: r.state,
    candidateType: local ? str(local, 'candidateType') : undefined,
    transport: local ? str(local, 'protocol')?.toLowerCase() : undefined,
    rttMs: rtt === undefined ? undefined : Math.round(rtt * 1000),
    availableOutgoingBitrate: outgoing === undefined ? undefined : Math.round(outgoing),
  });
}

/** When the stream last got a packet, to tell the live one from a leftover of the same m-section. */
const lastPacketAt = (s: Stat): number => num(s, 'lastPacketReceivedTimestamp') ?? num(s, 'timestamp') ?? 0;

/**
 * Summarizes the PCs' reports taken at `at` (Date.now()). `prev` is the `counters` of the previous call: without it
 * every rate is 0.
 */
export function summarize(reports: readonly PCReport[], at: number, prev?: StatsCounters): Summary {
  const streams = new Map<string, Counters>();
  const pcs: PCSample[] = [];
  const shares: Record<string, { video?: VideoInSample; audio?: AudioInSample }> = {};
  /** The stat each share's entry came from, to keep the live stream when a mid has two. */
  const chosen = new Map<string, Stat>();
  const outbound: OutboundSample[] = [];

  for (const r of reports) {
    const byId = new Map<string, Stat>();
    r.report?.forEach((value, key) => {
      if (typeof value === 'object' && value !== null) byId.set(key, value as Stat);
    });
    const byType = (type: string): Stat[] => [...byId.values()].filter((s) => s['type'] === type);
    pcs.push(pcSample(r, byId, byType));

    /** The counters of one stream now, stored for the next sample, with the ones before. */
    const counters = (
      s: Stat,
      id: string,
      bytes: number,
      frames: number | undefined,
    ): [Counters, Counters | undefined] => {
      const key = `${r.pc}:${String(r.gen)}:${id}`;
      const now: Counters = compact({
        at: num(s, 'timestamp') ?? at,
        bytes,
        frames,
        jitterBufferDelay: num(s, 'jitterBufferDelay'),
        jitterBufferEmitted: num(s, 'jitterBufferEmittedCount'),
      });
      streams.set(key, now);
      return [now, prev?.streams.get(key)];
    };
    const refOf = (s: Stat, kind: TrackKind): StatsTrackRef =>
      compact({ kind, mid: str(s, 'mid'), trackId: str(s, 'trackIdentifier') });
    const codecOf = (s: Stat): string | undefined => {
      const id = str(s, 'codecId');
      return codecName(id === undefined ? undefined : byId.get(id));
    };

    for (const [id, s] of byId) {
      if (s['type'] === 'inbound-rtp') {
        const kind = trackKind(s);
        const shareId = kind && r.shareOf(refOf(s, kind));
        if (!kind || shareId === undefined) continue;
        const bytes = num(s, 'bytesReceived') ?? 0;
        const framesDecoded = num(s, 'framesDecoded');
        const [now, before] = counters(s, id, bytes, framesDecoded);
        const slot = `${shareId} ${kind}`;
        const other = chosen.get(slot);
        if (other && lastPacketAt(other) > lastPacketAt(s)) continue;
        chosen.set(slot, s);
        const common = {
          shareId,
          kbps: kbps(bytes, now.at, before),
          bytesReceived: bytes,
          packetsLost: num(s, 'packetsLost') ?? 0,
          jitterBufferMs: jitterBufferMs(now.jitterBufferDelay, now.jitterBufferEmitted, before),
        };
        const entry = (shares[shareId] ??= {});
        if (kind === TrackKindVideo) {
          entry.video = compact({
            ...common,
            kind,
            frameWidth: num(s, 'frameWidth'),
            frameHeight: num(s, 'frameHeight'),
            framesPerSecond: num(s, 'framesPerSecond') ?? perSecond(framesDecoded, now.at, before),
            framesDecoded,
            framesDropped: num(s, 'framesDropped'),
            keyFramesDecoded: num(s, 'keyFramesDecoded'),
            freezeCount: num(s, 'freezeCount'),
            totalFreezesDuration: num(s, 'totalFreezesDuration'),
            pliCount: num(s, 'pliCount'),
            nackCount: num(s, 'nackCount'),
            codec: codecOf(s),
            decoderImplementation: str(s, 'decoderImplementation'),
            powerEfficientDecoder: bool(s, 'powerEfficientDecoder'),
          });
        } else {
          const concealed = num(s, 'concealedSamples');
          const total = num(s, 'totalSamplesReceived');
          entry.audio = compact({
            ...common,
            kind,
            audioLevel: num(s, 'audioLevel'),
            totalAudioEnergy: num(s, 'totalAudioEnergy'),
            concealedSamples: concealed,
            totalSamplesReceived: total,
            concealedRatio: concealed !== undefined && total !== undefined && total > 0 ? concealed / total : undefined,
            codec: codecOf(s),
          });
        }
      } else if (s['type'] === 'outbound-rtp') {
        const kind = trackKind(s);
        const shareId = kind && r.shareOf(refOf(s, kind));
        if (!kind || shareId === undefined) continue;
        const bytes = num(s, 'bytesSent') ?? 0;
        const framesEncoded = num(s, 'framesEncoded');
        const [now, before] = counters(s, id, bytes, framesEncoded);
        outbound.push(
          compact({
            shareId,
            kind,
            rid: str(s, 'rid'),
            kbps: kbps(bytes, now.at, before),
            bytesSent: bytes,
            frameWidth: num(s, 'frameWidth'),
            frameHeight: num(s, 'frameHeight'),
            framesPerSecond: num(s, 'framesPerSecond') ?? perSecond(framesEncoded, now.at, before),
            qualityLimitationReason: str(s, 'qualityLimitationReason'),
            encoderImplementation: str(s, 'encoderImplementation'),
            powerEfficientEncoder: bool(s, 'powerEfficientEncoder'),
          }),
        );
      }
    }
  }

  // A stable order: by share, video before audio, the largest layer first (01 lists encodings high first).
  outbound.sort(
    (a, b) =>
      a.shareId.localeCompare(b.shareId) ||
      (a.kind === b.kind ? 0 : a.kind === TrackKindVideo ? -1 : 1) ||
      (b.frameHeight ?? 0) - (a.frameHeight ?? 0) ||
      (a.rid ?? '').localeCompare(b.rid ?? ''),
  );

  return {
    sample: { at, intervalMs: prev ? Math.max(0, at - prev.at) : 0, pcs, shares, outbound },
    counters: { at, streams },
  };
}

/** How often the `stats` notification goes out (05 §18, 01 §8.11). */
export const STATS_REPORT_INTERVAL_MS = 10_000;

const int = (v: number | undefined): number | undefined => (v === undefined ? undefined : Math.round(v));

/**
 * The longest diagnostic string the server takes, in bytes of UTF-8 (maxStatsStringLen in
 * internal/protocol/validate.go). One longer string fails the whole report, and the hub drops it without a word.
 */
const MAX_STATS_STRING_BYTES = 64;

/**
 * A browser's string cut to what the server takes. Decoder and encoder names do get longer: Chrome's software
 * fallback is "FFmpeg (fallback from: ExternalDecoder (VideoToolboxVideoDecoder))", and a simulcast encoder lists
 * the encoder of every layer. The names are ASCII in practice; anything else is cut between characters.
 */
function clip(v: string): string;
function clip(v: string | undefined): string | undefined;
function clip(v: string | undefined): string | undefined {
  if (v === undefined) return undefined;
  let bytes = 0;
  let out = '';
  for (const ch of v) {
    const code = ch.codePointAt(0) ?? 0;
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
    if (bytes > MAX_STATS_STRING_BYTES) break;
    out += ch;
  }
  return out;
}

/**
 * A sample in 01's ClientStats shape, for the `stats` notification (05 §10.7's table). Go decodes most of these
 * fields into integers, so they are rounded here; `fps` stays a fraction. The strings are cut to the server's
 * limit: the sample itself (the overlay, __isshoni.stats()) keeps them whole.
 */
export function toClientStats(sample: StatsSample, intervalMs: number = STATS_REPORT_INTERVAL_MS): ClientStats {
  const pcs = sample.pcs.map((p): PCStats =>
    compact({
      pc: p.pc,
      gen: p.gen,
      state: clip(p.state),
      rttMs: int(p.rttMs),
      outgoingBitrate: int(p.availableOutgoingBitrate),
      candidateType: clip(p.candidateType),
      transport: clip(p.transport),
    }),
  );

  const inbound: InboundStats[] = [];
  for (const share of Object.values(sample.shares)) {
    const v = share.video;
    if (v) {
      inbound.push(
        compact({
          shareId: v.shareId,
          kind: v.kind,
          bitrate: Math.round(v.kbps * 1000),
          packetsLost: Math.round(v.packetsLost),
          jitterBufferMs: int(v.jitterBufferMs),
          fps: v.framesPerSecond,
          width: int(v.frameWidth),
          height: int(v.frameHeight),
          freezeCount: int(v.freezeCount),
          decoder: clip(v.decoderImplementation),
          hwDecoder: v.powerEfficientDecoder,
          // 01's CodecKey names H.264 profiles only.
          codec: v.codec?.startsWith('h264/') === true ? v.codec : undefined,
          framesDecoded: int(v.framesDecoded),
          framesDropped: int(v.framesDropped),
          freezeDurationMs:
            v.totalFreezesDuration === undefined ? undefined : Math.round(v.totalFreezesDuration * 1000),
        }),
      );
    }
    const a = share.audio;
    if (a) {
      inbound.push(
        compact({
          shareId: a.shareId,
          kind: a.kind,
          bitrate: Math.round(a.kbps * 1000),
          packetsLost: Math.round(a.packetsLost),
          jitterBufferMs: int(a.jitterBufferMs),
          concealedSamples: int(a.concealedSamples),
          totalSamples: int(a.totalSamplesReceived),
        }),
      );
    }
  }

  const outbound = sample.outbound.map((o): OutboundStats =>
    compact({
      shareId: o.shareId,
      kind: o.kind,
      rid: clip(o.rid),
      bitrate: Math.round(o.kbps * 1000),
      fps: o.framesPerSecond,
      width: int(o.frameWidth),
      height: int(o.frameHeight),
      encoder: clip(o.encoderImplementation),
      hwEncoder: o.powerEfficientEncoder,
      qualityLimitation: clip(o.qualityLimitationReason),
    }),
  );

  return {
    intervalMs: Math.round(intervalMs),
    pcs,
    ...(inbound.length > 0 ? { inbound } : {}),
    ...(outbound.length > 0 ? { outbound } : {}),
  };
}
