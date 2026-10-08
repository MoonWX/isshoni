// window.__isshoni (05 §10.7): the debug handle the e2e tests read (05 §19.3). It exists only when
// sessionStorage['isshoni.debug'] === '1', which e2e sets with addInitScript, and it exposes nothing the user can't
// see in the debug overlay (05 §20):
// - stats():      a fresh stats sample, per tile (lib/stats/summarize.ts); no IP addresses;
// - state():      what the page shows: the viewer's focus and audio, the room, the connection;
// - dropSocket(): closes the signaling socket with code 3000, which the server treats as a network drop (the
//                 reconnect test).
// The handle is frozen and the property can't be assigned: tests read it, nothing else writes it.
import { NotImplementedError } from '../errors';
import type { StatsSample } from './summarize';

/** The per-tab flag (platform.storage.session, 05 §6.1). */
export const DEBUG_FLAG_KEY = 'isshoni.debug';

export interface DebugHandle {
  /** A sample taken now: `shares[shareId]` holds a tile's video and audio stats. */
  stats(): Promise<StatsSample>;
  /** A JSON-safe snapshot of the page's state. */
  state(): unknown;
  /** Drops the signaling socket as a network failure would. */
  dropSocket(): void;
}

declare global {
  interface Window {
    /** The debug handle; present only with the debug flag (05 §10.7). */
    __isshoni?: DebugHandle;
  }
}

export interface DebugHandleDeps {
  /** platform.storage.session: holds the flag. */
  storage: { get(key: string): string | null };
  /** StatsCollector.snapshot. */
  stats: () => Promise<StatsSample>;
  /** What state() returns; the caller gathers it from its stores. */
  state: () => unknown;
  /**
   * Closes the signaling socket with code 3000. Without it dropSocket() throws NotImplementedError: the reconnect
   * test's slice (S81) passes the SignalClient's hook.
   */
  dropSocket?: () => void;
  /** Where the handle goes; default window. */
  target?: { __isshoni?: DebugHandle };
}

/** Whether this tab asked for the debug handle. */
export function debugEnabled(storage: DebugHandleDeps['storage']): boolean {
  return storage.get(DEBUG_FLAG_KEY) === '1';
}

/**
 * Puts the handle on window when the tab's debug flag is set; otherwise it does nothing. Returns the function that
 * removes it (tests; the page keeps its handle).
 */
export function installDebugHandle(deps: DebugHandleDeps): () => void {
  if (!debugEnabled(deps.storage)) return () => undefined;
  const target = deps.target ?? window;
  const handle: DebugHandle = Object.freeze({
    stats: () => deps.stats(),
    state: () => deps.state(),
    dropSocket: () => {
      if (!deps.dropSocket) throw new NotImplementedError('__isshoni.dropSocket', 'S81');
      deps.dropSocket();
    },
  });
  Object.defineProperty(target, '__isshoni', { configurable: true, enumerable: false, writable: false, value: handle });
  return () => {
    if (target.__isshoni === handle) Reflect.deleteProperty(target, '__isshoni');
  };
}
