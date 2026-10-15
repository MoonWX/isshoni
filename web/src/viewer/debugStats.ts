// The debug overlay's state (05 §10.7, "Stats for nerds"): whether the overlay is on screen, and what it shows.
// A vanilla Zustand store in the viewer's services (createViewer), so the two sides never meet (05 §3):
// - DebugOverlay.tsx says when it is open (setOpen) and renders `sample` and `server`;
// - attachViewerStats (statsLink.ts), which the owner of the stats collector runs, follows `open`: it makes the
//   collector sample every second, turns 01's stats.watch on for the server's view, and fills `sample` with each new
//   sample and `server` with each server `stats` message while the overlay is open.
// Nothing in it is ever an IP address: a sample keeps only the selected pair's candidate type and transport
// (lib/stats/summarize.ts), and ServerStats has none (01 §8.11).
import { createStore, type StoreApi } from 'zustand/vanilla';

import type { StatsSample } from '../lib/stats/summarize';
import type { ServerStats } from '../protocol/types.gen';

export interface DebugStatsState {
  /** The overlay is on screen. */
  readonly open: boolean;
  /** The collector's newest sample while the overlay is open; null before the first one, and while it is closed. */
  readonly sample: StatsSample | null;
  /** The server's newest `stats` message (stats.watch) while the overlay is open; null until one came. */
  readonly server: ServerStats | null;
  /** The overlay opened or closed. Closing forgets what it showed. */
  setOpen(open: boolean): void;
  setSample(sample: StatsSample | null): void;
  setServer(server: ServerStats | null): void;
}

export type DebugStatsStore = StoreApi<DebugStatsState>;

export function createDebugStats(): DebugStatsStore {
  return createStore<DebugStatsState>()((set, get) => ({
    open: false,
    sample: null,
    server: null,
    setOpen(open) {
      if (open === get().open) return;
      set(open ? { open } : { open, sample: null, server: null });
    },
    setSample(sample) {
      set({ sample });
    },
    setServer(server) {
      set({ server });
    },
  }));
}
