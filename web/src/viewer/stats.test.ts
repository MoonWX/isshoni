// The viewer's stats (05 §10.7): the session's real sub PC (on the fake RTCPeerConnection and the fake signaling
// server) as a source of the collector, and window.__isshoni: stats() returns per-tile stats, state() what the
// viewer shows.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createLogger } from '../lib/log';
import { createStatsCollector, installDebugHandle, STATS_INTERVAL_MS, type StatsCollector } from '../lib/stats';
import { createMemoryStorage } from '../platform/browser/storage';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer } from '../protocol/testing';
import type { PCOffer } from '../protocol/types.gen';
import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { buildSdp, FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { viewerDebugState, viewerStatsSources } from './stats';
import { SubscriberPC } from './SubscriberPC';
import { room, SELF, shareInfo, status } from './testing';

interface Section {
  mid: string;
  kind: 'audio' | 'video';
  /** The share this m-section carries; none = inactive. */
  share?: string;
}

/** A server's sub offer: one m-section per entry, `tracks` for the ones that carry a share. */
function offer(gen: number, neg: number, sections: Section[]): PCOffer {
  return {
    pc: 'sub',
    gen,
    neg,
    sdp: buildSdp({
      ufrag: 'srv1',
      sections: sections.map((s) => ({
        kind: s.kind,
        mid: s.mid,
        direction: s.share ? 'sendonly' : 'inactive',
        ...(s.share ? { msid: { stream: s.share, track: `${s.kind.charAt(0)}-${s.share}` } } : {}),
      })),
    }),
    tracks: sections.flatMap((s) => (s.share ? [{ mid: s.mid, shareId: s.share, kind: s.kind }] : [])),
  };
}

/** Bea's share on the stage (video and audio), Cy's as a tile (video only), and an idle m-section. */
const SECTIONS: Section[] = [
  { mid: '0', kind: 'video', share: 's_bea' },
  { mid: '1', kind: 'audio', share: 's_bea' },
  { mid: '2', kind: 'video', share: 's_cy' },
  { mid: '3', kind: 'audio' },
];

const H264 = {
  id: 'C102',
  type: 'codec',
  mimeType: 'video/H264',
  sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
};

/** What Chrome's getStats() reports for the sub PC after `seconds` of watching. */
function report(seconds: number): Map<string, Record<string, unknown>> {
  const timestamp = seconds * 1000;
  const stats: Record<string, unknown>[] = [
    H264,
    { id: 'T1', type: 'transport', selectedCandidatePairId: 'CP1' },
    { id: 'CP1', type: 'candidate-pair', localCandidateId: 'L1', state: 'succeeded', currentRoundTripTime: 0.021 },
    { id: 'L1', type: 'local-candidate', candidateType: 'host', protocol: 'udp', address: '192.0.2.44', port: 51000 },
    {
      id: 'IV0',
      type: 'inbound-rtp',
      kind: 'video',
      mid: '0',
      codecId: 'C102',
      timestamp,
      bytesReceived: seconds * 1_000_000,
      packetsLost: 0,
      frameWidth: 1920,
      frameHeight: 1080,
      framesPerSecond: 60,
      framesDecoded: seconds * 60,
    },
    {
      id: 'IA1',
      type: 'inbound-rtp',
      kind: 'audio',
      mid: '1',
      timestamp,
      bytesReceived: seconds * 16_000,
      packetsLost: 0,
      audioLevel: 0.2,
      totalAudioEnergy: seconds * 0.5,
    },
    {
      id: 'IV2',
      type: 'inbound-rtp',
      kind: 'video',
      mid: '2',
      codecId: 'C102',
      timestamp,
      bytesReceived: seconds * 50_000,
      packetsLost: 0,
      frameWidth: 640,
      frameHeight: 360,
      framesPerSecond: 30,
      framesDecoded: seconds * 30,
    },
    // An m-section that carries no share now: it is nobody's tile.
    { id: 'IA3', type: 'inbound-rtp', kind: 'audio', mid: '3', timestamp, bytesReceived: 9_999, packetsLost: 0 },
  ];
  return new Map(stats.map((s) => [s['id'] as string, s]));
}

let media: FakeMediaElementControl;
let server: FakeSignalServer;
let signal: SignalClient;
let viewer: ViewerServices;
let sub: SubscriberPC;
let collector: StatsCollector;
let uninstall: () => void;

const lastPc = (): FakeRTCPeerConnection => {
  const pc = FakeRTCPeerConnection.last;
  if (!pc) throw new Error('no RTCPeerConnection was created');
  return pc;
};

beforeEach(async () => {
  vi.useFakeTimers();
  vi.setSystemTime(2_000_000);
  installFakeMedia();
  media = installFakeMediaElement();
  server = FakeSignalServer.install();
  const platform = createTestPlatform();
  signal = new SignalClient({
    url: platform.signaling().url,
    client: platform.client,
    role: 'viewer',
    caps: () => platform.capsNow(),
  });
  viewer = createViewer();
  // As the room's wiring does it (viewer/index.ts, lib/stats/index.ts).
  sub = viewer.createSubscriber({ platform, signal, log: createLogger('sub-pc') }, SubscriberPC);
  collector = createStatsCollector({ sources: () => viewerStatsSources(viewer) });
  uninstall = installDebugHandle({
    storage: createMemoryStorage({ 'isshoni.debug': '1' }),
    stats: () => collector.snapshot(),
    state: () => ({ viewer: viewerDebugState(viewer) }),
  });
  signal.start();
  await vi.advanceTimersByTimeAsync(0);
});

afterEach(() => {
  uninstall();
  collector.stop();
  sub.close();
  signal.stop();
  server.uninstall();
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** Both shares live, the sub PC negotiated and connected, one sample stored after 10 s of watching. */
async function watching(): Promise<void> {
  syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 2), shareInfo('s_cy', 'u_cy', 1)), SELF);
  await sub.handleOffer(offer(1, 1, SECTIONS));
  lastPc().setConnectionState('connected');
  lastPc().stats = report(10);
  collector.start();
  await vi.advanceTimersByTimeAsync(0);
}

describe('viewerStatsSources', () => {
  it('has no source before the first sub offer and after the PC is closed', async () => {
    expect(viewerStatsSources(viewer)).toEqual([]);
    expect(viewerStatsSources(createViewer())).toEqual([]);
    await sub.handleOffer(offer(1, 1, SECTIONS));
    expect(viewerStatsSources(viewer)).toHaveLength(1);
    sub.close();
    expect(viewerStatsSources(viewer)).toEqual([]);
  });

  it('is the sub PC: its gen, its connectionState, its getStats()', async () => {
    await sub.handleOffer(offer(1, 1, SECTIONS));
    lastPc().stats = report(1);
    const [source] = viewerStatsSources(viewer);
    expect(source).toMatchObject({ pc: 'sub', gen: 1, state: 'new' });
    lastPc().setConnectionState('connected');
    expect(viewerStatsSources(viewer)[0]?.state).toBe('connected');
    expect(await source?.getStats()).toBeInstanceOf(Map);
  });

  it('maps a received stream to its share by the offer’s `tracks`, else by the track in the registry', async () => {
    await sub.handleOffer(offer(1, 1, SECTIONS));
    const [source] = viewerStatsSources(viewer);
    if (!source) throw new Error('no source');
    expect(source.shareOf({ kind: 'video', mid: '0' })).toBe('s_bea');
    expect(source.shareOf({ kind: 'audio', mid: '1' })).toBe('s_bea');
    expect(source.shareOf({ kind: 'video', mid: '2' })).toBe('s_cy');
    expect(source.shareOf({ kind: 'audio', mid: '3' })).toBeUndefined(); // carries no share
    expect(source.shareOf({ kind: 'audio', mid: '0' })).toBeUndefined(); // not that m-section's kind
    expect(source.shareOf({ kind: 'video', mid: '9' })).toBeUndefined();

    // A browser whose report has no mid: the stream's track id is the registry's track.
    const cyVideo = viewer.registry.get('s_cy').video;
    expect(cyVideo).toBeDefined();
    expect(source.shareOf({ kind: 'video', trackId: cyVideo?.id ?? '' })).toBe('s_cy');
    expect(source.shareOf({ kind: 'audio', trackId: cyVideo?.id ?? '' })).toBeUndefined();
    expect(source.shareOf({ kind: 'video', trackId: 'someone-else' })).toBeUndefined();
    expect(source.shareOf({ kind: 'video' })).toBeUndefined();

    // The server moved Cy's share to another m-section: the newest offer counts.
    await sub.handleOffer(
      offer(1, 2, [
        { mid: '0', kind: 'video', share: 's_cy' },
        { mid: '1', kind: 'audio' },
        { mid: '2', kind: 'video' },
        { mid: '3', kind: 'audio' },
      ]),
    );
    const [next] = viewerStatsSources(viewer);
    expect(next?.shareOf({ kind: 'video', mid: '0' })).toBe('s_cy');
    expect(next?.shareOf({ kind: 'video', mid: '2' })).toBeUndefined();
  });
});

describe('window.__isshoni (05 §10.7)', () => {
  it('stats() returns per-tile stats', async () => {
    await watching();
    lastPc().stats = report(12);
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);

    const stats = await window.__isshoni?.stats();
    expect(Object.keys(stats?.shares ?? {}).sort()).toEqual(['s_bea', 's_cy']);
    // The focused tile: the high layer, decoding, with its audio.
    expect(stats?.shares['s_bea']?.video).toMatchObject({
      frameWidth: 1920,
      frameHeight: 1080,
      framesDecoded: 720,
      framesPerSecond: 60,
      bytesReceived: 12_000_000,
      codec: 'h264/6400',
    });
    expect(stats?.shares['s_bea']?.audio).toMatchObject({
      audioLevel: 0.2,
      totalAudioEnergy: 6,
      bytesReceived: 192_000,
    });
    // The other tile: the low layer, no audio (audio only for the focused share).
    expect(stats?.shares['s_cy']?.video).toMatchObject({ frameHeight: 360, framesDecoded: 360 });
    expect(stats?.shares['s_cy']?.audio).toBeUndefined();
    expect(stats?.pcs).toEqual([
      { pc: 'sub', gen: 1, state: 'connected', candidateType: 'host', transport: 'udp', rttMs: 21 },
    ]);
    expect(stats?.outbound).toEqual([]);
    expect(JSON.stringify(stats)).not.toContain('192.0.2.44');
  });

  it('stats() is fresh at every call, with rates since the collector’s last sample', async () => {
    await watching(); // the stored sample: 10 s
    lastPc().stats = report(11);
    await vi.advanceTimersByTimeAsync(1_000);
    const first = await window.__isshoni?.stats();
    expect(first?.shares['s_bea']?.video).toMatchObject({ framesDecoded: 660, kbps: 8_000 });
    expect(first?.shares['s_bea']?.audio?.kbps).toBe(128);
    expect(first?.shares['s_cy']?.video?.kbps).toBe(400);

    lastPc().stats = report(11.5);
    const second = await window.__isshoni?.stats();
    expect(second?.shares['s_bea']?.video?.framesDecoded).toBe(690);
    expect(collector.history()).toHaveLength(1); // asking stores nothing
  });

  it('stats() lists no tile for a share that ended, and nothing without a PC', async () => {
    await watching();
    await sub.handleOffer(
      offer(1, 2, [
        { mid: '0', kind: 'video', share: 's_bea' },
        { mid: '1', kind: 'audio', share: 's_bea' },
        { mid: '2', kind: 'video' },
        { mid: '3', kind: 'audio' },
      ]),
    );
    expect(Object.keys((await window.__isshoni?.stats())?.shares ?? {})).toEqual(['s_bea']);

    sub.close();
    expect(await window.__isshoni?.stats()).toMatchObject({ pcs: [], shares: {}, outbound: [] });
  });

  it('state() says what the viewer shows and plays, as plain JSON', async () => {
    await watching();
    viewer.store.getState().applyStatus([status('s_cy', { video: 'low', requestedVideo: 'low', audio: 'off' })]);
    await vi.advanceTimersByTimeAsync(0);

    const state = window.__isshoni?.state();
    expect(JSON.parse(JSON.stringify(state))).toEqual(state);
    expect(state).toEqual({
      viewer: {
        focusedShareId: 's_bea',
        focusMode: 'auto',
        audibleShareId: 's_bea',
        audio: 'playing',
        audioPlaying: true,
        videoBlocked: false,
        volume: 1,
        fullscreen: false,
        pipShareId: null,
        pageHidden: false,
        media: 'connected',
        subGen: 1,
        shares: [
          {
            id: 's_bea',
            userId: 'u_bea',
            own: false,
            local: false,
            kind: 'window',
            status: 'live',
            layers: ['high', 'low'],
            watchers: 0,
            visible: false,
            frozen: false,
            subscription: null,
          },
          {
            id: 's_cy',
            userId: 'u_cy',
            own: false,
            local: false,
            kind: 'window',
            status: 'live',
            layers: ['high', 'low'],
            watchers: 0,
            visible: false,
            frozen: false,
            subscription: { shareId: 's_cy', video: 'low', audio: 'off', requestedVideo: 'low' },
          },
        ],
      },
    });
  });

  it('state() follows the focus, and the audio element when the browser refuses it', async () => {
    media.policy = 'block';
    await watching();
    await vi.advanceTimersByTimeAsync(0);
    expect(viewerDebugState(viewer)).toMatchObject({ audio: 'blocked', audioPlaying: false });

    viewer.store.getState().focusShare('s_cy');
    expect(viewerDebugState(viewer)).toMatchObject({
      focusedShareId: 's_cy',
      focusMode: 'manual',
      audibleShareId: 's_cy',
    });
  });

  it('is absent without the debug flag', () => {
    uninstall();
    expect(window.__isshoni).toBeUndefined();
    uninstall = installDebugHandle({
      storage: createMemoryStorage(),
      stats: () => collector.snapshot(),
      state: () => ({ viewer: viewerDebugState(viewer) }),
    });
    expect(window.__isshoni).toBeUndefined();
  });
});
