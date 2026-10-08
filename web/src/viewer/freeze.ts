// Tile freeze detection (05 §12.6): a tile whose video is asked for and whose share is live, but whose
// `framesDecoded` has not moved for 3 s, says "Waiting for video…" over its last frame. It consumes the stats
// collector's samples (lib/stats, every 2 s; 05 §10.7) and writes viewerStore.frozen, which the tiles and the stage
// read through shareView(). No React here (05 §3).
//
// Wiring, for whoever owns the collector (rooms/connectStats.ts):
//   const off = attachFreezeWatch(viewer, collector);     // once the collector exists; off() with the runtime
// Without it nothing is ever frozen: the other tile states need no stats.
//
// What is measured, and what is left alone:
// - Only a share that the layer policy asks video for (layerPolicy.ts), that is live (a stalled share has its own
//   notice) and that the server forwards (subscribe.status says neither `off` nor `waiting`): those have their own
//   overlays, and their clock starts again when video is due.
// - Only while the page is visible: a hidden tab may stop decoding on its own.
// - Only a share whose stream is in the sample with a frame count. A tile that the stats can't see is never called
//   frozen: an overlay over a picture that plays would be worse than a missing notice.
import type { StatsCollector } from '../lib/stats/collector';
import type { StatsSample } from '../lib/stats/summarize';
import { ShareStatusStalled, StatusReasonWaiting, VideoLayerOff } from '../protocol/types.gen';
import type { ViewerServices } from './services';
import { viewerWants } from './subscriptions';
import type { ViewerData } from './viewerStore';

/** A tile is frozen after this long without a decoded frame (05 §18). */
export const FREEZE_AFTER_MS = 3_000;

export interface FreezeDetector {
  /**
   * Takes one stats sample and the shares whose video should be moving, and returns those among them that have
   * decoded nothing for FREEZE_AFTER_MS. A share that is not watched in a sample starts over when it is again.
   */
  observe(sample: Pick<StatsSample, 'at' | 'shares'>, watched: Iterable<string>): string[];
  /** Forgets every share: the next sample starts all clocks. */
  reset(): void;
}

export function createFreezeDetector(): FreezeDetector {
  /** Per measured share: the frame count, and the time of the first sample that had it. */
  const seen = new Map<string, { frames: number; since: number }>();
  return {
    observe(sample, watched) {
      const frozen: string[] = [];
      const measured = new Set<string>();
      for (const shareId of watched) {
        const frames = sample.shares[shareId]?.video?.framesDecoded;
        if (frames === undefined) continue;
        measured.add(shareId);
        const was = seen.get(shareId);
        if (was?.frames !== frames) {
          seen.set(shareId, { frames, since: sample.at });
        } else if (sample.at - was.since >= FREEZE_AFTER_MS) {
          frozen.push(shareId);
        }
      }
      for (const shareId of [...seen.keys()]) {
        if (!measured.has(shareId)) seen.delete(shareId);
      }
      return frozen;
    },
    reset() {
      seen.clear();
    },
  };
}

/** The shares whose video should be moving at `now` (Date.now()): the ones a freeze can be told for. */
export function freezeWatched(s: ViewerData, now: number): string[] {
  if (s.pageHiddenSince !== null) return [];
  const live = new Set(s.shares.filter((share) => share.info.status !== ShareStatusStalled).map((share) => share.id));
  return viewerWants(s, now)
    .filter((want) => {
      if (want.video === VideoLayerOff || !live.has(want.shareId)) return false;
      const status = s.status[want.shareId];
      return status?.video !== VideoLayerOff && status?.reason !== StatusReasonWaiting;
    })
    .map((want) => want.shareId);
}

/**
 * Keeps viewerStore.frozen in step with the collector's samples. Returns the function that stops it, which also
 * clears the store's flags.
 */
export function attachFreezeWatch(
  viewer: Pick<ViewerServices, 'store'>,
  stats: Pick<StatsCollector, 'subscribe'>,
): () => void {
  const detector = createFreezeDetector();
  const off = stats.subscribe((sample) => {
    const s = viewer.store.getState();
    s.setFrozen(detector.observe(sample, freezeWatched(s, sample.at)));
  });
  return () => {
    off();
    detector.reset();
    viewer.store.getState().setFrozen([]);
  };
}
