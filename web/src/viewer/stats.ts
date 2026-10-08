// The viewer's side of the stats (05 §10.7): the sub PC as a source of the collector (lib/stats/collector.ts), and
// what window.__isshoni.state() says about the viewer (lib/stats/debugHandle.ts). No React here.
//
//   const stats = createStatsCollector({ sources: () => viewerStatsSources(viewer) });
//   installDebugHandle({ storage: platform.storage.session, stats: () => stats.snapshot(),
//                        state: () => ({ viewer: viewerDebugState(viewer) }) });
import type { StatsSource } from '../lib/stats';
import { PCKindSub } from '../protocol/types.gen';
import type { ViewerServices } from './services';

/**
 * The sub PC, while the viewer has one, as the collector reads it. A received stream's share is the one the
 * newest offer's `tracks` give its mid (05 §9); for a browser whose report has no mid, the share whose track in
 * the media registry has the stream's track id.
 */
export function viewerStatsSources(viewer: Pick<ViewerServices, 'subscriber' | 'registry'>): StatsSource[] {
  const sub = viewer.subscriber;
  if (!sub || sub.gen === 0) return [];
  const { registry } = viewer;
  return [
    {
      pc: PCKindSub,
      gen: sub.gen,
      state: sub.connectionState,
      getStats: () => sub.getStats(),
      shareOf(track) {
        if (track.mid !== undefined) {
          const ref = sub.trackOf(track.mid);
          if (ref?.kind === track.kind) return ref.shareId;
        }
        if (track.trackId !== undefined) {
          return registry.shareIds().find((id) => registry.get(id)[track.kind]?.id === track.trackId);
        }
        return undefined;
      },
    },
  ];
}

/**
 * The viewer's part of window.__isshoni.state(): what is on the stage and what is heard, the tiles, and the state
 * of the playback. Plain JSON, and nothing the page doesn't show (05 §10.7): ids and states, no names.
 */
export function viewerDebugState(viewer: Pick<ViewerServices, 'store' | 'audio' | 'subscriber'>) {
  const s = viewer.store.getState();
  const el = viewer.audio.element;
  return {
    focusedShareId: s.focusedShareId,
    focusMode: s.focusMode,
    audibleShareId: s.audibleShareId,
    audio: s.audio,
    /** The <audio> element plays (it exists and is not paused). */
    audioPlaying: el !== null && !el.paused,
    videoBlocked: s.videoBlocked,
    volume: s.volume,
    fullscreen: s.fullscreen,
    pipShareId: s.pipShareId,
    media: s.media,
    subGen: viewer.subscriber?.gen ?? 0,
    shares: s.shares.map((share) => ({
      id: share.id,
      userId: share.userId,
      own: share.own,
      local: share.local,
      kind: share.info.kind,
      status: share.info.status,
      layers: [...share.info.layers],
      watchers: share.info.watchers.length,
      visible: s.visible[share.id] === true,
      subscription: s.status[share.id] ?? null,
    })),
  };
}

export type ViewerDebugState = ReturnType<typeof viewerDebugState>;
