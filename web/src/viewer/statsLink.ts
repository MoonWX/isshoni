// What the page does with the stats collector's samples (05 §10.7), besides the e2e handle: everything that needs
// the collector and the tab's SignalClient together. No React here (05 §3).
//
// Wiring, for whoever owns the collector (rooms/connectStats.ts), once it exists and for the runtime's life:
//   const off = attachViewerStats({ viewer, collector, signal, log });   // off() when the runtime goes
//
// - The tiles' freeze watch (freeze.ts, 05 §12.6) reads the samples.
// - The `stats` notification (01 §8.11): every STATS_REPORT_INTERVAL_MS (10 s) the newest sample goes to the server
//   in 01's ClientStats shape (toClientStats), while the client has a PC (the sample lists one) and signaling is
//   ready. A sample is sent once: a collector that stopped (no room) or a socket that is down sends nothing. The hub
//   keeps the last report for the admin's live view and turns the growth of its inbound counters into the
//   isshoni_client_* metrics (01 §18), which the M1 exit test reads.
// - The debug overlay (DebugOverlay.tsx, `Shift+D` or `?debug=1`), while it is open (viewer.debug.open): the
//   collector samples every second instead of every 2 (setFast), and each sample goes to viewer.debug; stats.watch
//   {on: true} asks the server for its view of this connection every 2 s (01 §8.11), which goes there too. Closing
//   the overlay sends stats.watch {on: false} and forgets both. A connection that was not resumed starts without
//   the watch, so a new one is asked again while the overlay is open; a resumed one keeps it.
// Nothing here holds an IP address: the samples have none (lib/stats/summarize.ts), and neither has ServerStats.
import type { Logger } from '../lib/log';
import type { StatsCollector } from '../lib/stats/collector';
import { STATS_REPORT_INTERVAL_MS, toClientStats, type StatsSample } from '../lib/stats/summarize';
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeStats, MessageTypeStatsWatch } from '../protocol/types.gen';
import { attachFreezeWatch } from './freeze';
import type { ViewerServices } from './services';

/** What attachViewerStats needs from signaling: 01's SignalClient, by its structural type. */
export type StatsLinkSignal = Pick<SignalClient, 'state' | 'on' | 'onState' | 'notify' | 'request'>;

export interface ViewerStatsDeps {
  /** The viewer: its store for the freeze watch, and the debug overlay's state. */
  viewer: Pick<ViewerServices, 'store' | 'debug'>;
  /** The page's stats collector (lib/stats/collector.ts), sampling the sub and pub PCs. */
  collector: Pick<StatsCollector, 'subscribe' | 'latest' | 'setFast'>;
  /** The tab's SignalClient. */
  signal: StatsLinkSignal;
  log?: Logger;
}

/** Starts the freeze watch, the `stats` reports and the debug overlay's feed. Returns the function that stops them. */
export function attachViewerStats({ viewer, collector, signal, log }: ViewerStatsDeps): () => void {
  const offFreeze = attachFreezeWatch(viewer, collector);
  const debug = viewer.debug;

  // ---- The stats notification (01 §8.11) ----

  /** The sample that the last report carried. */
  let reported: StatsSample | null = null;
  const report = (): void => {
    const sample = collector.latest();
    if (sample === null || sample === reported || sample.pcs.length === 0 || signal.state !== 'ready') return;
    // The time the report covers: since the sample of the report before, when there was one.
    const intervalMs =
      reported !== null && sample.at > reported.at ? sample.at - reported.at : STATS_REPORT_INTERVAL_MS;
    if (signal.notify(MessageTypeStats, toClientStats(sample, intervalMs))) reported = sample;
  };
  const timer = setInterval(report, STATS_REPORT_INTERVAL_MS);

  // ---- The debug overlay ----

  /**
   * What this connection's stats.watch is on the server, as far as this page knows: true or false once asked
   * (optimistically, from the request on), null when that is unknown (a request failed).
   */
  let watching: boolean | null = false;
  const watch = (on: boolean): void => {
    if (signal.state !== 'ready' || watching === on) return;
    watching = on;
    signal.request(MessageTypeStatsWatch, { on }).catch((err: unknown) => {
      // A socket that went away under it: the next ready asks again while the overlay is open.
      watching = null;
      log?.debug('stats.watch failed', { on, error: err });
    });
  };

  const follow = (open: boolean): void => {
    collector.setFast(open);
    if (open) debug.getState().setSample(collector.latest());
    watch(open);
  };

  const offSamples = collector.subscribe((sample) => {
    if (debug.getState().open) debug.getState().setSample(sample);
  });
  const offServer = signal.on(MessageTypeStats, (stats) => {
    if (debug.getState().open) debug.getState().setServer(stats);
  });
  const offState = signal.onState((state, info) => {
    if (state !== 'ready') return;
    // A new connection has no watch; a resumed one keeps what it had (01 §8.11: until {on: false} or the close).
    if (info.resumed !== true) watching = false;
    // What the overlay wants now, also when it opened or closed while the socket was down.
    watch(debug.getState().open);
  });
  const offDebug = debug.subscribe((s, prev) => {
    if (s.open !== prev.open) follow(s.open);
  });
  if (debug.getState().open) follow(true);

  return () => {
    clearInterval(timer);
    offDebug();
    offState();
    offServer();
    offSamples();
    offFreeze();
    collector.setFast(false);
    if (watching !== false) watch(false);
    debug.getState().setSample(null);
    debug.getState().setServer(null);
  };
}
