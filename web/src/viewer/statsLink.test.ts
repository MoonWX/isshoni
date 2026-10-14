// attachViewerStats (05 §10.7, 01 §8.11), against the real SignalClient on the fake WebSocket server: the `stats`
// notification every 10 s in 01's ClientStats shape, the debug overlay's feed (the collector's fast sampling, the
// samples, and stats.watch for the server's view), and the freeze watch it starts.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { clearLog, createLogger, logLines } from '../lib/log';
import type { StatsCollector } from '../lib/stats/collector';
import { STATS_REPORT_INTERVAL_MS, toClientStats, type StatsSample } from '../lib/stats/summarize';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError } from '../protocol/testing';
import type { ClientStats, ServerStats, StatsWatch } from '../protocol/types.gen';
import { createTestPlatform } from '../test/platform';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { attachViewerStats } from './statsLink';
import { room, SELF, shareInfo } from './testing';
import { markShown } from './useVisibility';

/** A collector that the test feeds: push() stores a sample and tells the listeners, as a tick does. */
interface FakeCollector extends Pick<StatsCollector, 'subscribe' | 'latest' | 'setFast'> {
  push(sample: StatsSample): void;
  /** The collector stopped (no room): its samples are gone. */
  stop(): void;
  /** Every setFast() call, in order. */
  readonly fast: boolean[];
  listeners(): number;
}

function fakeCollector(): FakeCollector {
  const fns = new Set<(s: StatsSample) => void>();
  let latest: StatsSample | null = null;
  const fast: boolean[] = [];
  return {
    subscribe(fn) {
      fns.add(fn);
      return () => {
        fns.delete(fn);
      };
    },
    latest: () => latest,
    setFast(f) {
      fast.push(f);
    },
    push(sample) {
      latest = sample;
      for (const fn of [...fns]) fn(sample);
    },
    stop() {
      latest = null;
    },
    fast,
    listeners: () => fns.size,
  };
}

/** A sample at `at`: the sub PC, and Bea's window decoding `frames` frames so far, with its sound. */
function sample(at: number, frames = 600, pcs: StatsSample['pcs'] = SUB_PC): StatsSample {
  return {
    at,
    intervalMs: 2_000,
    pcs,
    outbound: [],
    shares: {
      s_bea: {
        video: {
          kind: 'video',
          shareId: 's_bea',
          kbps: 2_500,
          bytesReceived: 1_000_000,
          packetsLost: 2,
          frameWidth: 1280,
          frameHeight: 720,
          framesPerSecond: 30,
          framesDecoded: frames,
          framesDropped: 1,
          freezeCount: 0,
          totalFreezesDuration: 0,
          codec: 'h264/42e0',
        },
        audio: {
          kind: 'audio',
          shareId: 's_bea',
          kbps: 32,
          bytesReceived: 40_000,
          packetsLost: 0,
          concealedSamples: 10,
          totalSamplesReceived: 48_000,
        },
      },
    },
  };
}

const SUB_PC: StatsSample['pcs'] = [{ pc: 'sub', gen: 1, state: 'connected', candidateType: 'host', transport: 'udp' }];

const SERVER: ServerStats = {
  downlinkEstimate: 24_000_000,
  subs: [{ shareId: 's_bea', kind: 'video', layer: 'high', bitrate: 2_600_000, lossPct: 0.2, dropped: 0 }],
  layers: [],
};

let server: FakeSignalServer;
let signal: SignalClient;
let viewer: ViewerServices;
let collector: FakeCollector;
let detach: (() => void) | null;

const tick = async (ms = 0): Promise<void> => {
  await vi.advanceTimersByTimeAsync(ms);
};
const reports = (): ClientStats[] => server.messages('stats').map((m) => m.data as ClientStats);
const watches = (): boolean[] => server.messages('stats.watch').map((m) => (m.data as StatsWatch).on);

function attach(): void {
  detach = attachViewerStats({ viewer, collector, signal, log: createLogger('stats') });
}

beforeEach(async () => {
  vi.useFakeTimers();
  clearLog();
  server = FakeSignalServer.install();
  server.handle('stats.watch', () => ({ ok: {} }));
  const platform = createTestPlatform();
  signal = new SignalClient({
    url: platform.signaling().url,
    client: platform.client,
    role: 'viewer',
    caps: () => platform.capsNow(),
  });
  signal.start();
  await tick();
  expect(signal.state).toBe('ready');
  viewer = createViewer();
  syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1)), SELF);
  collector = fakeCollector();
  detach = null;
});

afterEach(() => {
  detach?.();
  viewer.dispose();
  signal.stop();
  server.uninstall();
  vi.useRealTimers();
});

describe('attachViewerStats: the stats notification (01 §8.11)', () => {
  it('sends the newest sample every 10 s in the ClientStats shape, each sample once', async () => {
    attach();
    collector.push(sample(1_000));
    await tick(STATS_REPORT_INTERVAL_MS - 1);
    expect(reports()).toEqual([]);
    await tick(1);
    expect(reports()).toEqual([toClientStats(sample(1_000))]);
    expect(reports()[0]).toMatchObject({
      intervalMs: STATS_REPORT_INTERVAL_MS,
      pcs: [{ pc: 'sub', gen: 1, state: 'connected', candidateType: 'host', transport: 'udp' }],
      inbound: [
        { shareId: 's_bea', kind: 'video', framesDecoded: 600, framesDropped: 1, width: 1280, height: 720 },
        { shareId: 's_bea', kind: 'audio', concealedSamples: 10, totalSamples: 48_000 },
      ],
    });

    // No new sample in the next 10 s (a collector that is slow): nothing again.
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(reports()).toHaveLength(1);

    // The next one covers the time since the sample of the report before.
    collector.push(sample(13_000, 900));
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(reports()).toHaveLength(2);
    expect(reports()[1]).toMatchObject({ intervalMs: 12_000, inbound: [{ framesDecoded: 900 }, {}] });
  });

  it('sends nothing without a PC, without a sample, or while signaling is down', async () => {
    attach();
    collector.push(sample(1_000, 600, []));
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(reports()).toEqual([]);

    collector.push(sample(2_000));
    collector.stop(); // the room was left: no samples
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(reports()).toEqual([]);

    collector.push(sample(3_000));
    server.autoOpen = false;
    server.drop();
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(signal.state).not.toBe('ready');
    expect(reports()).toEqual([]);
    server.autoOpen = true;
    for (const socket of server.sockets) if (socket.readyState === WebSocket.CONNECTING) server.accept(socket);
    await tick(STATS_REPORT_INTERVAL_MS);
    expect(signal.state).toBe('ready');
    expect(reports()).toHaveLength(1);
  });

  it('stops with the function it returns', async () => {
    attach();
    detach?.();
    detach = null;
    collector.push(sample(1_000));
    await tick(STATS_REPORT_INTERVAL_MS * 2);
    expect(reports()).toEqual([]);
    expect(collector.listeners()).toBe(0);
  });
});

describe('attachViewerStats: the debug overlay (05 §10.7)', () => {
  it('while the overlay is open: fast samples into viewer.debug, and stats.watch for the server’s view', async () => {
    attach();
    collector.push(sample(1_000));
    server.send('stats', SERVER); // nobody watches: ignored
    expect(viewer.debug.getState()).toMatchObject({ sample: null, server: null });
    expect(watches()).toEqual([]);

    viewer.debug.getState().setOpen(true);
    expect(collector.fast).toEqual([true]);
    // The newest sample at once, then each new one.
    expect(viewer.debug.getState().sample).toEqual(sample(1_000));
    collector.push(sample(2_000, 630));
    expect(viewer.debug.getState().sample?.at).toBe(2_000);
    await tick();
    expect(watches()).toEqual([true]);
    server.send('stats', SERVER);
    expect(viewer.debug.getState().server).toEqual(SERVER);

    viewer.debug.getState().setOpen(false);
    expect(collector.fast).toEqual([true, false]);
    expect(viewer.debug.getState()).toMatchObject({ sample: null, server: null });
    await tick();
    expect(watches()).toEqual([true, false]);
    collector.push(sample(3_000));
    server.send('stats', SERVER);
    expect(viewer.debug.getState()).toMatchObject({ sample: null, server: null });
  });

  it('asks a new connection again, a resumed one not, and one that was down when it opened once it is ready', async () => {
    attach();
    viewer.debug.getState().setOpen(true);
    await tick();
    expect(watches()).toEqual([true]);

    // A resumed connection keeps its watch.
    server.drop();
    await tick(1_000);
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    expect(watches()).toEqual([true]);

    // A new one (the server restarted) has none.
    server.restart();
    server.drop();
    await tick(1_000);
    expect(server.welcomes.at(-1)?.resumed).toBe(false);
    expect(watches()).toEqual([true, true]);

    // Closed and opened while the socket is down: the next ready says what the overlay wants then.
    server.autoOpen = false;
    server.drop();
    await tick(1);
    viewer.debug.getState().setOpen(false);
    viewer.debug.getState().setOpen(true);
    viewer.debug.getState().setOpen(false);
    expect(watches()).toEqual([true, true]);
    server.autoOpen = true;
    // The backoff after a third drop in a row is a few seconds (01 §10.2).
    await tick(10_000);
    expect(signal.state).toBe('ready');
    expect(watches()).toEqual([true, true, false]);
  });

  it('a stats.watch the server refuses is logged and asked again on the next connection', async () => {
    server.handle('stats.watch', () => ({ error: makeError('internal', 'request') }));
    attach();
    viewer.debug.getState().setOpen(true);
    await tick();
    expect(watches()).toEqual([true]);
    expect(logLines().some((l) => l.msg === 'stats.watch failed' && l.level === 'debug')).toBe(true);

    server.handle('stats.watch', () => ({ ok: {} }));
    server.drop();
    await tick(1_000);
    expect(watches()).toEqual([true, true]);
  });

  it('opens with the overlay that is open already, and turns everything off when stopped', async () => {
    viewer.debug.getState().setOpen(true);
    collector.push(sample(1_000));
    attach();
    expect(collector.fast).toEqual([true]);
    expect(viewer.debug.getState().sample?.at).toBe(1_000);
    await tick();
    expect(watches()).toEqual([true]);

    detach?.();
    detach = null;
    expect(collector.fast).toEqual([true, false]);
    expect(viewer.debug.getState().sample).toBeNull();
    await tick();
    expect(watches()).toEqual([true, false]);
    expect(collector.listeners()).toBe(0);
  });
});

describe('attachViewerStats: the freeze watch (05 §12.6)', () => {
  it('runs on the same samples, and its marks go with it', () => {
    markShown(viewer.store, 's_bea');
    attach();
    collector.push(sample(0, 100));
    collector.push(sample(2_000, 100));
    collector.push(sample(4_000, 100));
    expect(Object.keys(viewer.store.getState().frozen)).toEqual(['s_bea']);
    detach?.();
    detach = null;
    expect(viewer.store.getState().frozen).toEqual({});
  });
});
