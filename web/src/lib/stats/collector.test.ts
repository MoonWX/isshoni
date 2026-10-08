// @vitest-environment node
// The stats collector (05 §10.7, §18): a sample every 2 s (1 s with the debug overlay), rates from the sample
// before, a ring of 150 samples, and snapshot() for the debug handle.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  createStatsCollector,
  STATS_FAST_INTERVAL_MS,
  STATS_HISTORY,
  STATS_INTERVAL_MS,
  type StatsCollector,
  type StatsSource,
} from './collector';
import type { StatsSample } from './summarize';

/** A sub PC that received `bytes` so far for share s_a on mid 0. */
class FakePC implements StatsSource {
  readonly pc = 'sub' as const;
  gen = 1;
  state = 'connected';
  bytes = 0;
  calls = 0;
  fail = false;

  getStats(): Promise<Map<string, Record<string, unknown>>> {
    this.calls++;
    if (this.fail) return Promise.reject(new Error('InvalidStateError'));
    return Promise.resolve(
      new Map([
        [
          'IT1',
          { id: 'IT1', type: 'inbound-rtp', kind: 'video', mid: '0', bytesReceived: this.bytes, timestamp: Date.now() },
        ],
      ]),
    );
  }

  shareOf(track: { mid?: string }): string | undefined {
    return track.mid === '0' ? 's_a' : undefined;
  }
}

let pc: FakePC;
let sources: StatsSource[];
let collector: StatsCollector;

const kbps = (s: StatsSample | null): number | undefined => s?.shares['s_a']?.video?.kbps;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(1_000_000);
  pc = new FakePC();
  sources = [pc];
  collector = createStatsCollector({ sources: () => sources });
});

afterEach(() => {
  collector.stop();
  vi.useRealTimers();
});

describe('createStatsCollector', () => {
  it('samples at once on start, then every 2 s, with the rates between samples', async () => {
    expect(collector.latest()).toBeNull();
    expect(collector.running).toBe(false);
    collector.start();
    expect(collector.running).toBe(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(collector.history()).toHaveLength(1);
    expect(collector.latest()).toMatchObject({ at: 1_000_000, intervalMs: 0, pcs: [{ pc: 'sub', gen: 1 }] });
    expect(kbps(collector.latest())).toBe(0);

    pc.bytes = 500_000; // 2 Mbit/s over 2 s
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS - 1);
    expect(collector.history()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(collector.history()).toHaveLength(2);
    expect(collector.latest()?.intervalMs).toBe(2_000);
    expect(kbps(collector.latest())).toBe(2_000);

    collector.start(); // a no-op while it runs
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.history()).toHaveLength(3);
    expect(kbps(collector.latest())).toBe(0);
  });

  it('samples every second while the debug overlay is open', async () => {
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    collector.setFast(true);
    await vi.advanceTimersByTimeAsync(STATS_FAST_INTERVAL_MS);
    expect(collector.history()).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(3 * STATS_FAST_INTERVAL_MS);
    expect(collector.history()).toHaveLength(5);

    collector.setFast(false);
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS - 1);
    expect(collector.history()).toHaveLength(5);
    await vi.advanceTimersByTimeAsync(1);
    expect(collector.history()).toHaveLength(6);
  });

  it('keeps the last 150 samples (5 minutes), oldest first', async () => {
    collector.start();
    await vi.advanceTimersByTimeAsync((STATS_HISTORY + 9) * STATS_INTERVAL_MS);
    const history = collector.history();
    expect(history).toHaveLength(STATS_HISTORY);
    expect(history[0]?.at).toBe(1_000_000 + 10 * STATS_INTERVAL_MS);
    expect(history.at(-1)).toBe(collector.latest());
    expect(history.at(-1)?.at).toBe(1_000_000 + (STATS_HISTORY + 9) * STATS_INTERVAL_MS);
  });

  it('hands out a history that does not change under its reader', async () => {
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    const before = collector.history();
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(before).toHaveLength(1);
    expect(collector.history()).toHaveLength(2);
  });

  it('asks for the PCs at every sample: they come and go with the room', async () => {
    sources = [];
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(collector.latest()).toMatchObject({ pcs: [], shares: {}, outbound: [] });

    sources = [pc];
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.latest()?.pcs).toHaveLength(1);

    // Rebuilt: a new gen, whose counters start over.
    pc.gen = 2;
    pc.bytes = 9_000_000;
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.latest()?.pcs[0]?.gen).toBe(2);
    expect(kbps(collector.latest())).toBe(0);
  });

  it('lists a PC whose getStats() fails without numbers, and goes on sampling', async () => {
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    pc.fail = true;
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.latest()).toMatchObject({ pcs: [{ pc: 'sub', gen: 1, state: 'connected' }], shares: {} });
    pc.fail = false;
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.latest()?.shares['s_a']?.video).toBeDefined();
  });

  it('goes on after a sample that could not be taken at all', async () => {
    let broken = true;
    collector = createStatsCollector({
      sources: () => {
        if (broken) throw new Error('no session');
        return [pc];
      },
    });
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(collector.history()).toHaveLength(0);
    broken = false;
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.history()).toHaveLength(1);
  });

  it('tells its subscribers every stored sample, and survives one that throws', async () => {
    const seen: StatsSample[] = [];
    collector.subscribe(() => {
      throw new Error('listener bug');
    });
    const off = collector.subscribe((s) => seen.push(s));
    collector.start();
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(seen).toEqual(collector.history());
    expect(seen).toHaveLength(2);
    off();
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(seen).toHaveLength(2);
  });

  it('stop() ends the sampling and forgets the history; start() begins anew', async () => {
    collector.start();
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    collector.stop();
    expect(collector.running).toBe(false);
    expect(collector.latest()).toBeNull();
    expect(collector.history()).toEqual([]);
    const calls = pc.calls;
    await vi.advanceTimersByTimeAsync(5 * STATS_INTERVAL_MS);
    expect(pc.calls).toBe(calls);

    pc.bytes = 4_000_000;
    collector.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(collector.history()).toHaveLength(1);
    expect(kbps(collector.latest())).toBe(0); // nothing from before the stop is compared
  });

  it('drops a sample that was in flight when it stopped', async () => {
    let release: (() => void) | undefined;
    const slow: StatsSource = {
      pc: 'sub',
      gen: 1,
      state: 'connected',
      getStats: () =>
        new Promise((resolve) => {
          release = () => {
            resolve(new Map());
          };
        }),
      shareOf: () => undefined,
    };
    sources = [slow];
    collector.start();
    collector.stop();
    release?.();
    await vi.advanceTimersByTimeAsync(STATS_INTERVAL_MS);
    expect(collector.history()).toEqual([]);
  });

  describe('snapshot()', () => {
    it('takes a fresh sample without storing it, measured against the last stored one', async () => {
      collector.start();
      await vi.advanceTimersByTimeAsync(0);
      pc.bytes = 125_000;
      await vi.advanceTimersByTimeAsync(500);

      const fresh = await collector.snapshot();
      expect(fresh.at).toBe(1_000_500);
      expect(fresh.intervalMs).toBe(500);
      expect(fresh.shares['s_a']?.video).toMatchObject({ bytesReceived: 125_000, kbps: 2_000 });
      expect(collector.history()).toHaveLength(1);

      // Asking again changes nothing for the next stored sample: it still spans the whole 2 s.
      await collector.snapshot();
      pc.bytes = 500_000;
      await vi.advanceTimersByTimeAsync(1_500);
      expect(collector.history()).toHaveLength(2);
      expect(collector.latest()?.intervalMs).toBe(2_000);
      expect(kbps(collector.latest())).toBe(2_000);
    });

    it('works before start(): the numbers are there, the rates are 0', async () => {
      pc.bytes = 42;
      const fresh = await collector.snapshot();
      expect(fresh.shares['s_a']?.video).toMatchObject({ bytesReceived: 42, kbps: 0 });
      expect(collector.latest()).toBeNull();
    });
  });
});
