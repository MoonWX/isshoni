// @vitest-environment node
// summarize (05 §10.7): a getStats() report → per-tile, per-rid and per-PC numbers, with rates from the previous
// sample and no IP addresses; toClientStats: the table that maps a sample to 01's ClientStats.
import { describe, expect, it } from 'vitest';

import { summarize, toClientStats, type PCReport, type StatsTrackRef } from './summarize';

type Stat = Record<string, unknown>;

const report = (...stats: Stat[]): Map<string, Stat> => new Map(stats.map((s) => [s['id'] as string, s]));

/** mid → share, like the sub offer's `tracks`. */
const byMid =
  (map: Record<string, string>) =>
  (track: StatsTrackRef): string | undefined =>
    track.mid === undefined ? undefined : map[track.mid];

const H264_HIGH = {
  id: 'CIT01_102',
  type: 'codec',
  mimeType: 'video/H264',
  sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
};
const OPUS = { id: 'CIT01_111', type: 'codec', mimeType: 'audio/opus', sdpFmtpLine: 'minptime=10;useinbandfec=1' };

/** The transport, its selected pair and both candidates, addresses included, as a browser reports them. */
const transport = (protocol = 'udp', extra: Stat = {}): Stat[] => [
  { id: 'T01', type: 'transport', selectedCandidatePairId: 'CP1', dtlsState: 'connected' },
  {
    id: 'CP1',
    type: 'candidate-pair',
    localCandidateId: 'L1',
    remoteCandidateId: 'R1',
    state: 'succeeded',
    nominated: true,
    currentRoundTripTime: 0.0384,
    availableOutgoingBitrate: 2_400_000.4,
    ...extra,
  },
  { id: 'CP2', type: 'candidate-pair', localCandidateId: 'L2', remoteCandidateId: 'R1', state: 'waiting' },
  { id: 'L1', type: 'local-candidate', candidateType: 'prflx', protocol, address: '192.0.2.17', port: 50123 },
  { id: 'L2', type: 'local-candidate', candidateType: 'host', protocol: 'udp', address: '10.1.2.3', port: 50124 },
  { id: 'R1', type: 'remote-candidate', candidateType: 'host', protocol, address: '203.0.113.9', port: 7882 },
];

const videoIn = (over: Stat = {}): Stat => ({
  id: 'IT01V1',
  type: 'inbound-rtp',
  kind: 'video',
  mid: '0',
  trackIdentifier: 'track-v',
  codecId: 'CIT01_102',
  timestamp: 10_000,
  bytesReceived: 1_000_000,
  packetsLost: 12,
  frameWidth: 1920,
  frameHeight: 1080,
  framesPerSecond: 59.8,
  framesDecoded: 600,
  framesDropped: 3,
  keyFramesDecoded: 2,
  freezeCount: 1,
  totalFreezesDuration: 0.42,
  pliCount: 1,
  nackCount: 7,
  jitterBufferDelay: 12,
  jitterBufferEmittedCount: 300,
  decoderImplementation: 'ExternalDecoder',
  powerEfficientDecoder: true,
  ...over,
});

const audioIn = (over: Stat = {}): Stat => ({
  id: 'IT01A1',
  type: 'inbound-rtp',
  kind: 'audio',
  mid: '1',
  codecId: 'CIT01_111',
  timestamp: 10_000,
  bytesReceived: 50_000,
  packetsLost: 0,
  audioLevel: 0.31,
  totalAudioEnergy: 4.5,
  concealedSamples: 480,
  totalSamplesReceived: 96_000,
  jitterBufferDelay: 3,
  jitterBufferEmittedCount: 100,
  ...over,
});

const sub = (stats: Stat[], over: Partial<PCReport> = {}): PCReport => ({
  pc: 'sub',
  gen: 1,
  state: 'connected',
  report: report(...stats),
  shareOf: byMid({ '0': 's_a', '1': 's_a' }),
  ...over,
});

describe('summarize: a tile', () => {
  it('gives each subscribed share its video and audio numbers', () => {
    const { sample } = summarize([sub([H264_HIGH, OPUS, ...transport(), videoIn(), audioIn()])], 5_000);
    expect(sample.at).toBe(5_000);
    expect(sample.intervalMs).toBe(0);
    expect(Object.keys(sample.shares)).toEqual(['s_a']);
    expect(sample.shares['s_a']?.video).toEqual({
      shareId: 's_a',
      kind: 'video',
      kbps: 0, // no earlier sample
      bytesReceived: 1_000_000,
      packetsLost: 12,
      jitterBufferMs: 40, // 12 s over 300 frames
      frameWidth: 1920,
      frameHeight: 1080,
      framesPerSecond: 59.8,
      framesDecoded: 600,
      framesDropped: 3,
      keyFramesDecoded: 2,
      freezeCount: 1,
      totalFreezesDuration: 0.42,
      pliCount: 1,
      nackCount: 7,
      codec: 'h264/6400',
      decoderImplementation: 'ExternalDecoder',
      powerEfficientDecoder: true,
    });
    expect(sample.shares['s_a']?.audio).toEqual({
      shareId: 's_a',
      kind: 'audio',
      kbps: 0,
      bytesReceived: 50_000,
      packetsLost: 0,
      jitterBufferMs: 30,
      audioLevel: 0.31,
      totalAudioEnergy: 4.5,
      concealedSamples: 480,
      totalSamplesReceived: 96_000,
      concealedRatio: 0.005,
      codec: 'opus',
    });
  });

  it('computes the rates from the previous sample', () => {
    const first = summarize([sub([H264_HIGH, OPUS, videoIn(), audioIn()])], 5_000);
    const second = summarize(
      [
        sub([
          H264_HIGH,
          OPUS,
          // 2 s later: 1.85 MB and 120 frames more; the jitter buffer held 1.5 s over 60 frames.
          videoIn({
            timestamp: 12_000,
            bytesReceived: 2_850_000,
            framesDecoded: 720,
            framesPerSecond: undefined,
            jitterBufferDelay: 13.5,
            jitterBufferEmittedCount: 360,
          }),
          audioIn({ timestamp: 12_000, bytesReceived: 82_000 }),
        ]),
      ],
      7_000,
      first.counters,
    );
    expect(second.sample.intervalMs).toBe(2_000);
    const video = second.sample.shares['s_a']?.video;
    expect(video?.kbps).toBe(7_400);
    expect(video?.framesPerSecond).toBe(60); // no framesPerSecond in the report: from framesDecoded
    expect(video?.jitterBufferMs).toBe(25); // over the interval, not since the start
    expect(second.sample.shares['s_a']?.audio?.kbps).toBe(128);
  });

  it('reports no rate for a counter that went back (a new stream under the same id) or without time passing', () => {
    const first = summarize([sub([videoIn()])], 5_000);
    const back = summarize([sub([videoIn({ timestamp: 12_000, bytesReceived: 10 })])], 7_000, first.counters);
    expect(back.sample.shares['s_a']?.video?.kbps).toBe(0);
    const same = summarize([sub([videoIn({ bytesReceived: 2_000_000 })])], 7_000, first.counters);
    expect(same.sample.shares['s_a']?.video?.kbps).toBe(0);
  });

  it('keeps the counters of each PC and gen apart', () => {
    const first = summarize([sub([videoIn()])], 5_000);
    // The sub PC was rebuilt: the same stat id means another stream.
    const rebuilt = summarize(
      [sub([videoIn({ timestamp: 12_000, bytesReceived: 3_000_000 })], { gen: 2 })],
      7_000,
      first.counters,
    );
    expect(rebuilt.sample.shares['s_a']?.video?.kbps).toBe(0);
    expect(rebuilt.sample.pcs).toEqual([{ pc: 'sub', gen: 2, state: 'connected' }]);
  });

  it('leaves out a stream that carries no share now, and reads `mediaType` where `kind` is missing', () => {
    const { sample } = summarize(
      [
        sub([
          videoIn({ id: 'idle', mid: '7' }), // an m-section the offer maps to nothing
          videoIn({ id: 'old', kind: undefined, mediaType: 'video' }),
          { id: 'other', type: 'inbound-rtp', kind: 'data', mid: '0' },
        ]),
      ],
      5_000,
    );
    expect(Object.keys(sample.shares)).toEqual(['s_a']);
    expect(sample.shares['s_a']?.video?.framesDecoded).toBe(600);
  });

  it('takes the stream that got a packet last when one share has two', () => {
    const { sample } = summarize(
      [
        sub([
          videoIn({ id: 'stale', framesDecoded: 5, lastPacketReceivedTimestamp: 1_000 }),
          videoIn({ id: 'live', framesDecoded: 900, lastPacketReceivedTimestamp: 9_000 }),
          videoIn({ id: 'older', framesDecoded: 7, lastPacketReceivedTimestamp: 2_000 }),
        ]),
      ],
      5_000,
    );
    expect(sample.shares['s_a']?.video?.framesDecoded).toBe(900);
  });

  it('asks the PC for the share by mid and by track id', () => {
    const asked: StatsTrackRef[] = [];
    summarize(
      [
        sub([videoIn(), audioIn()], {
          shareOf: (track) => {
            asked.push(track);
            return undefined;
          },
        }),
      ],
      5_000,
    );
    expect(asked).toEqual([
      { kind: 'video', mid: '0', trackId: 'track-v' },
      { kind: 'audio', mid: '1' },
    ]);
  });

  it('names another codec by its mime subtype, and H.264 it cannot key as plain h264', () => {
    const vp8 = { id: 'C_VP8', type: 'codec', mimeType: 'video/VP8' };
    const odd = { id: 'C_ODD', type: 'codec', mimeType: 'video/H264', sdpFmtpLine: 'packetization-mode=0' };
    const { sample } = summarize(
      [
        sub([vp8, odd, videoIn({ codecId: 'C_VP8' }), videoIn({ id: 'b', mid: '2', codecId: 'C_ODD' })], {
          shareOf: byMid({ '0': 's_a', '2': 's_b' }),
        }),
      ],
      5_000,
    );
    expect(sample.shares['s_a']?.video?.codec).toBe('vp8');
    expect(sample.shares['s_b']?.video?.codec).toBe('h264');
  });
});

describe('summarize: the PCs', () => {
  it('describes the selected candidate pair by type and transport only', () => {
    const { sample } = summarize([sub([...transport('tcp'), videoIn()])], 5_000);
    expect(sample.pcs).toEqual([
      {
        pc: 'sub',
        gen: 1,
        state: 'connected',
        candidateType: 'prflx',
        transport: 'tcp',
        rttMs: 38,
        availableOutgoingBitrate: 2_400_000,
      },
    ]);
  });

  it('stores no IP address (05 §20)', () => {
    const { sample, counters } = summarize([sub([H264_HIGH, OPUS, ...transport(), videoIn(), audioIn()])], 5_000);
    const text = JSON.stringify(sample) + JSON.stringify([...counters.streams]);
    for (const address of ['192.0.2.17', '10.1.2.3', '203.0.113.9', '50123', '7882']) {
      expect(text).not.toContain(address);
    }
  });

  it('finds the pair Firefox marks `selected`, and a nominated one that succeeded', () => {
    const firefox = summarize(
      [
        sub([
          { id: 'CP9', type: 'candidate-pair', localCandidateId: 'L9', selected: true, currentRoundTripTime: 0.012 },
          { id: 'L9', type: 'local-candidate', candidateType: 'host', protocol: 'UDP' },
        ]),
      ],
      5_000,
    );
    expect(firefox.sample.pcs[0]).toMatchObject({ candidateType: 'host', transport: 'udp', rttMs: 12 });

    const nominated = summarize(
      [
        sub([
          { id: 'CP8', type: 'candidate-pair', localCandidateId: 'L8', nominated: true, state: 'succeeded' },
          { id: 'L8', type: 'local-candidate', candidateType: 'relay', protocol: 'tcp' },
        ]),
      ],
      5_000,
    );
    expect(nominated.sample.pcs[0]).toMatchObject({ candidateType: 'relay', transport: 'tcp' });
  });

  it('lists a PC whose getStats() gave nothing, without numbers', () => {
    const { sample } = summarize([sub([], { report: null, state: 'failed' })], 5_000);
    expect(sample).toEqual({
      at: 5_000,
      intervalMs: 0,
      pcs: [{ pc: 'sub', gen: 1, state: 'failed' }],
      shares: {},
      outbound: [],
    });
  });
});

describe('summarize: what this page sends', () => {
  const layer = (rid: string, over: Stat = {}): Stat => ({
    id: `OT01V${rid}`,
    type: 'outbound-rtp',
    kind: 'video',
    mid: '0',
    rid,
    timestamp: 10_000,
    bytesSent: 500_000,
    frameWidth: 1920,
    frameHeight: 1080,
    framesPerSecond: 60,
    qualityLimitationReason: 'none',
    encoderImplementation: 'VideoToolbox',
    powerEfficientEncoder: true,
    ...over,
  });
  const pub = (stats: Stat[]): PCReport => ({
    pc: 'pub',
    gen: 3,
    state: 'connected',
    report: report(...stats),
    shareOf: byMid({ '0': 's_mine', '1': 's_mine' }),
  });

  it('lists every simulcast layer by rid, the largest first, then the audio', () => {
    const stats = [
      layer('q', { frameWidth: 640, frameHeight: 360, framesPerSecond: 30, qualityLimitationReason: 'bandwidth' }),
      { id: 'OT01A', type: 'outbound-rtp', kind: 'audio', mid: '1', timestamp: 10_000, bytesSent: 20_000 },
      layer('f'),
    ];
    const first = summarize([pub(stats)], 5_000);
    expect(first.sample.outbound.map((o) => [o.kind, o.rid])).toEqual([
      ['video', 'f'],
      ['video', 'q'],
      ['audio', undefined],
    ]);
    expect(first.sample.outbound[1]).toEqual({
      shareId: 's_mine',
      kind: 'video',
      rid: 'q',
      kbps: 0,
      bytesSent: 500_000,
      frameWidth: 640,
      frameHeight: 360,
      framesPerSecond: 30,
      qualityLimitationReason: 'bandwidth',
      encoderImplementation: 'VideoToolbox',
      powerEfficientEncoder: true,
    });

    const second = summarize(
      [pub([layer('f', { timestamp: 12_000, bytesSent: 2_500_000 }), layer('q', { timestamp: 12_000 })])],
      7_000,
      first.counters,
    );
    expect(second.sample.outbound.map((o) => o.kbps)).toEqual([8_000, 0]);
  });

  it('puts both PCs into one sample', () => {
    const { sample } = summarize([sub([videoIn()]), pub([layer('f')])], 5_000);
    expect(sample.pcs.map((p) => [p.pc, p.gen])).toEqual([
      ['sub', 1],
      ['pub', 3],
    ]);
    expect(Object.keys(sample.shares)).toEqual(['s_a']);
    expect(sample.outbound).toHaveLength(1);
  });
});

describe('toClientStats (05 §10.7, 01 §8.11)', () => {
  it('maps a sample to ClientStats, with whole numbers where Go has integers', () => {
    const first = summarize([sub([H264_HIGH, OPUS, ...transport(), videoIn(), audioIn()])], 5_000);
    const { sample } = summarize(
      [
        sub([
          H264_HIGH,
          OPUS,
          ...transport(),
          videoIn({ timestamp: 12_000, bytesReceived: 2_850_100 }),
          audioIn({ timestamp: 12_000, bytesReceived: 82_000 }),
        ]),
      ],
      7_000,
      first.counters,
    );
    expect(toClientStats(sample)).toEqual({
      intervalMs: 10_000,
      pcs: [
        {
          pc: 'sub',
          gen: 1,
          state: 'connected',
          rttMs: 38,
          outgoingBitrate: 2_400_000,
          candidateType: 'prflx',
          transport: 'udp',
        },
      ],
      inbound: [
        {
          shareId: 's_a',
          kind: 'video',
          bitrate: 7_400_400,
          packetsLost: 12,
          jitterBufferMs: 40,
          fps: 59.8,
          width: 1920,
          height: 1080,
          freezeCount: 1,
          decoder: 'ExternalDecoder',
          hwDecoder: true,
          codec: 'h264/6400',
          framesDecoded: 600,
          framesDropped: 3,
          freezeDurationMs: 420,
        },
        {
          shareId: 's_a',
          kind: 'audio',
          bitrate: 128_000,
          packetsLost: 0,
          jitterBufferMs: 30,
          concealedSamples: 480,
          totalSamples: 96_000,
        },
      ],
    });
  });

  it('maps the sent layers, leaves a non-H.264 codec out, and omits empty lists', () => {
    const vp8 = { id: 'C_VP8', type: 'codec', mimeType: 'video/VP8' };
    const { sample } = summarize(
      [
        sub([vp8, videoIn({ codecId: 'C_VP8' })]),
        {
          pc: 'pub',
          gen: 2,
          state: 'connecting',
          report: report({
            id: 'O1',
            type: 'outbound-rtp',
            kind: 'video',
            mid: '0',
            rid: 'f',
            bytesSent: 1,
            frameWidth: 2560,
            frameHeight: 1440,
            framesPerSecond: 29.97,
            qualityLimitationReason: 'cpu',
            encoderImplementation: 'OpenH264',
            powerEfficientEncoder: false,
          }),
          shareOf: () => 's_mine',
        },
      ],
      5_000,
    );
    const stats = toClientStats(sample, 2_000.4);
    expect(stats.intervalMs).toBe(2_000);
    expect(stats.inbound?.[0]).not.toHaveProperty('codec');
    expect(stats.outbound).toEqual([
      {
        shareId: 's_mine',
        kind: 'video',
        rid: 'f',
        bitrate: 0,
        fps: 29.97,
        width: 2560,
        height: 1440,
        encoder: 'OpenH264',
        hwEncoder: false,
        qualityLimitation: 'cpu',
      },
    ]);

    const empty = toClientStats(summarize([sub([])], 5_000).sample);
    expect(empty).toEqual({ intervalMs: 10_000, pcs: [{ pc: 'sub', gen: 1, state: 'connected' }] });
  });
});
