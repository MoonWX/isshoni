// The stats collector (05 §10.7): while a room session exists it samples getStats() of the page's PCs (sub and
// pub) every 2 s, every 1 s while the debug overlay is open, summarizes each (summarize.ts) and keeps the last
// 150 samples (5 minutes). It never imports React: consumers read latest() and history(), or subscribe.
//
// Consumers (05 §10.7): tile freeze detection (05 §12.6) and the sharer hints (05 §13.7), the debug overlay, the
// `stats` notification every 10 s (toClientStats(latest()), 01 §8.11) and the e2e tests through window.__isshoni
// (debugHandle.ts). No sample holds an IP address.
import type { Logger } from '../log';
import type { PCKind } from '../../protocol/types.gen';
import {
  summarize,
  type PCReport,
  type StatsCounters,
  type StatsReportLike,
  type StatsSample,
  type StatsTrackRef,
} from './summarize';

/** The sampling interval (05 §18) … */
export const STATS_INTERVAL_MS = 2_000;
/** … and the one while the debug overlay is open. */
export const STATS_FAST_INTERVAL_MS = 1_000;
/** The ring buffer's size: 5 minutes at 2 s. */
export const STATS_HISTORY = 150;

/** One PC as the collector sees it. viewer/ gives the sub PC (viewerStatsSources), share/ the pub PC. */
export interface StatsSource {
  readonly pc: PCKind;
  readonly gen: number;
  /** RTCPeerConnection.connectionState. */
  readonly state: string;
  /** null when there is no PC to ask anymore. */
  getStats(): Promise<StatsReportLike | null>;
  /** The share a track belongs to (the offer's `tracks`); undefined for a track that carries none. */
  shareOf(track: StatsTrackRef): string | undefined;
}

export interface StatsCollectorDeps {
  /** The PCs that exist now. Asked at every sample: PCs come and go with the room and its shares. */
  sources: () => readonly StatsSource[];
  log?: Logger;
  /** Default Date.now. */
  now?: () => number;
}

export interface StatsCollector {
  /** Starts sampling; the first sample is taken at once. A no-op while it runs. */
  start(): void;
  /** Stops sampling and forgets the history: the next start() begins a new session. */
  stop(): void;
  readonly running: boolean;
  /** The debug overlay is open: sample every second. */
  setFast(fast: boolean): void;
  /** The newest stored sample; null before the first. */
  latest(): StatsSample | null;
  /** The stored samples, oldest first, at most STATS_HISTORY. */
  history(): readonly StatsSample[];
  /**
   * A sample taken now, for the debug handle and the tests. It is not stored and moves nothing: its rates are
   * measured against the last stored sample (0 when there is none), so asking often never shortens the history.
   */
  snapshot(): Promise<StatsSample>;
  /** Calls fn with every stored sample. Returns the function that stops it. */
  subscribe(fn: (sample: StatsSample) => void): () => void;
}

type Timer = ReturnType<typeof setTimeout>;

export function createStatsCollector(deps: StatsCollectorDeps): StatsCollector {
  const now = deps.now ?? Date.now;
  const listeners = new Set<(sample: StatsSample) => void>();
  let samples: StatsSample[] = [];
  let counters: StatsCounters | undefined;
  let timer: Timer | undefined;
  let running = false;
  let fast = false;
  /** Grows on stop(): a sample that was in flight belongs to the session before. */
  let epoch = 0;

  const read = async (): Promise<PCReport[]> =>
    Promise.all(
      deps.sources().map(async (src): Promise<PCReport> => {
        // Read before the await: a PC that is replaced meanwhile must not lend its gen to the old one's numbers.
        const { pc, gen, state } = src;
        let report: StatsReportLike | null = null;
        try {
          report = await src.getStats();
        } catch (err) {
          // A PC that closed under the call. The sample lists it without numbers.
          deps.log?.debug('getStats failed', { pc, gen, error: err });
        }
        return { pc, gen, state, report, shareOf: (track) => src.shareOf(track) };
      }),
    );

  const interval = (): number => (fast ? STATS_FAST_INTERVAL_MS : STATS_INTERVAL_MS);

  const schedule = (): void => {
    clearTimeout(timer);
    timer = running ? setTimeout(tick, interval()) : undefined;
  };

  const tick = (): void => {
    timer = undefined;
    const started = epoch;
    read()
      .then((reports) => {
        if (started !== epoch) return;
        const summary = summarize(reports, now(), counters);
        counters = summary.counters;
        // A new array per sample: history() hands out a list that never changes under its reader.
        samples = [...samples, summary.sample].slice(-STATS_HISTORY);
        for (const fn of [...listeners]) {
          try {
            fn(summary.sample);
          } catch (err) {
            deps.log?.error('a stats listener failed', { error: err });
          }
        }
      })
      .catch((err: unknown) => {
        deps.log?.warn('a stats sample failed', { error: err });
      })
      .finally(() => {
        // The next one only after this one is done: a slow getStats() never piles samples up.
        if (started === epoch) schedule();
      });
  };

  return {
    start() {
      if (running) return;
      running = true;
      tick();
    },
    stop() {
      running = false;
      epoch++;
      clearTimeout(timer);
      timer = undefined;
      samples = [];
      counters = undefined;
    },
    get running() {
      return running;
    },
    setFast(next) {
      if (fast === next) return;
      fast = next;
      // A sample that is in flight schedules the next one itself, with the new interval.
      if (timer !== undefined) schedule();
    },
    latest: () => samples.at(-1) ?? null,
    history: () => samples,
    async snapshot() {
      const base = counters;
      return summarize(await read(), now(), base).sample;
    },
    subscribe(fn) {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    },
  };
}
