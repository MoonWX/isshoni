// The stats' side of the room runtime (05 §10.7): lib/stats' collector samples the page's PCs while the session is
// in a room, and window.__isshoni, the debug handle the e2e tests read (05 §19.3), shows a fresh sample and the
// page's state. No React here (05 §3).
//
// - The collector's code (with the summary of a getStats() report) is in the room's media chunk (rooms/media.ts):
//   it is loaded with the first room.state, or with the handle's first stats() call. It starts when the session has
//   a room's snapshot and stops when it has none anymore (leave(), a room switch, a room the server closed), which
//   also forgets its samples.
// - Its sources are the viewer's sub PC (viewer/stats.ts). The pub PC joins them when share/ has its source.
// - The viewer's freeze watch (viewer/freeze.ts, 05 §12.6) reads the collector's samples from the moment the
//   collector exists until the runtime goes: it is what puts "Waiting for video…" on a tile whose picture stands
//   still. It comes with the same chunk.
// - The handle exists only in a tab with sessionStorage['isshoni.debug'] === '1' (lib/stats/debugHandle.ts). Its
//   state() has ids and states and no names: nothing the page doesn't show. dropSocket() arrives with the web
//   recovery slice, which gives the SignalClient its hook.
import type { Logger } from '../lib/log';
import type { StatsCollector } from '../lib/stats/collector';
import { installDebugHandle } from '../lib/stats/debugHandle';
import type { KeyValueStore } from '../platform/types';
import type { ViewerServices } from '../viewer/services';
import { viewerDebugState, viewerStatsSources } from '../viewer/stats';
import type { ConnectionStore } from './connection';
import { loadRoomMedia } from './loadMedia';
import type { RoomStore } from './roomStore';

export interface ConnectStatsDeps {
  viewer: ViewerServices;
  room: RoomStore;
  connection: ConnectionStore;
  /** platform.storage.session: holds the debug flag. */
  storage: Pick<KeyValueStore, 'get'>;
  log: Logger;
  /** Where the debug handle goes; default window (tests). */
  target?: Parameters<typeof installDebugHandle>[0]['target'];
}

/** What window.__isshoni.state() returns. */
export function debugState({ viewer, room, connection }: Pick<ConnectStatsDeps, 'viewer' | 'room' | 'connection'>) {
  const r = room.getState();
  const c = connection.getState();
  return {
    connection: { state: c.state, resumed: c.resumed, connectionId: r.connectionId },
    room: {
      roomId: r.roomId,
      joinState: r.joinState,
      rev: r.state?.rev ?? null,
      participants: r.state?.participants.length ?? 0,
    },
    viewer: viewerDebugState(viewer),
  };
}

export type RoomDebugState = ReturnType<typeof debugState>;

/** Starts the stats for a runtime. Returns the function that stops them and removes the debug handle. */
export function connectStats(deps: ConnectStatsDeps): () => void {
  const { viewer, room, log } = deps;
  let collector: StatsCollector | null = null;
  let loading: Promise<StatsCollector> | null = null;
  let offFreeze: (() => void) | null = null;
  let disconnected = false;

  const get = (): Promise<StatsCollector> => {
    loading ??= loadRoomMedia().then(
      ({ createStatsCollector, attachFreezeWatch }) => {
        collector = createStatsCollector({ sources: () => viewerStatsSources(viewer), log });
        // A runtime that is gone by now never starts the collector, so there is nothing to watch either.
        if (!disconnected) offFreeze = attachFreezeWatch(viewer, collector);
        return collector;
      },
      (err: unknown) => {
        // A chunk that didn't arrive once may arrive the next time.
        loading = null;
        throw err;
      },
    );
    return loading;
  };

  const inRoom = (): boolean => room.getState().state !== null;

  const follow = (): void => {
    if (!inRoom()) {
      collector?.stop();
      return;
    }
    get().then(
      (c) => {
        // The room may be gone, or the runtime, by the time the code is there.
        if (!disconnected && inRoom()) c.start();
      },
      (err: unknown) => {
        log.warn('the stats collector did not load', { error: err });
      },
    );
  };

  const offRoom = room.subscribe((s, prev) => {
    if ((s.state === null) !== (prev.state === null)) follow();
  });
  follow();

  const uninstall = installDebugHandle({
    storage: deps.storage,
    stats: () => get().then((c) => c.snapshot()),
    state: () => debugState(deps),
    ...(deps.target ? { target: deps.target } : {}),
  });

  return () => {
    disconnected = true;
    offRoom();
    uninstall();
    offFreeze?.();
    offFreeze = null;
    collector?.stop();
  };
}
