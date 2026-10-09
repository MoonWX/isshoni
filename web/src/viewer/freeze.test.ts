// Tile freeze detection (05 §12.6): `framesDecoded` unchanged for 3 s while video is asked for and the share is
// live. The detector by itself, which shares it watches, and the two together on the stats collector's samples.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { STATS_INTERVAL_MS } from '../lib/stats/collector';
import type { StatsSample } from '../lib/stats/summarize';
import { attachFreezeWatch, createFreezeDetector, FREEZE_AFTER_MS, freezeWatched } from './freeze';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { shareView } from './shareView';
import { room, SELF, shareInfo, status } from './testing';
import { markShown } from './useVisibility';

/** A sample at `at` in which each listed share has decoded that many frames so far. */
function sample(at: number, frames: Record<string, number | undefined>): StatsSample {
  return {
    at,
    intervalMs: STATS_INTERVAL_MS,
    pcs: [],
    outbound: [],
    shares: Object.fromEntries(
      Object.entries(frames).map(([shareId, framesDecoded]) => [
        shareId,
        {
          video: {
            kind: 'video' as const,
            shareId,
            kbps: 0,
            bytesReceived: 0,
            packetsLost: 0,
            ...(framesDecoded !== undefined ? { framesDecoded } : {}),
          },
        },
      ]),
    ),
  };
}

describe('createFreezeDetector', () => {
  it('calls a share frozen once its frame count has not moved for 3 s', () => {
    expect(FREEZE_AFTER_MS).toBe(3_000);
    const d = createFreezeDetector();
    const watched = ['s_a'];
    expect(d.observe(sample(0, { s_a: 100 }), watched)).toEqual([]);
    expect(d.observe(sample(2_000, { s_a: 100 }), watched)).toEqual([]); // 2 s: not yet
    expect(d.observe(sample(2_999, { s_a: 100 }), watched)).toEqual([]);
    expect(d.observe(sample(3_000, { s_a: 100 }), watched)).toEqual(['s_a']);
    expect(d.observe(sample(4_000, { s_a: 100 }), watched)).toEqual(['s_a']);
  });

  it('needs two samples at the collector’s pace: 4 s after the last frame', () => {
    const d = createFreezeDetector();
    const frozenAt = [0, 1, 2, 3].map((i) => d.observe(sample(i * STATS_INTERVAL_MS, { s_a: 7 }), ['s_a']).length);
    expect(frozenAt).toEqual([0, 0, 1, 1]);
  });

  it('thaws with the first new frame, and starts the clock again from there', () => {
    const d = createFreezeDetector();
    d.observe(sample(0, { s_a: 100 }), ['s_a']);
    expect(d.observe(sample(4_000, { s_a: 100 }), ['s_a'])).toEqual(['s_a']);
    expect(d.observe(sample(6_000, { s_a: 101 }), ['s_a'])).toEqual([]);
    expect(d.observe(sample(8_000, { s_a: 101 }), ['s_a'])).toEqual([]);
    expect(d.observe(sample(9_000, { s_a: 101 }), ['s_a'])).toEqual(['s_a']);
  });

  it('judges each share by itself', () => {
    const d = createFreezeDetector();
    d.observe(sample(0, { s_a: 1, s_b: 1 }), ['s_a', 's_b']);
    expect(d.observe(sample(4_000, { s_a: 1, s_b: 60 }), ['s_a', 's_b'])).toEqual(['s_a']);
  });

  it('starts over for a share that was not watched in between (its video was off)', () => {
    const d = createFreezeDetector();
    d.observe(sample(0, { s_a: 100 }), ['s_a']);
    expect(d.observe(sample(4_000, { s_a: 100 }), [])).toEqual([]); // scrolled away: off
    // Back in view: the count is the old one, and that is not 4 s without frames.
    expect(d.observe(sample(6_000, { s_a: 100 }), ['s_a'])).toEqual([]);
    expect(d.observe(sample(8_000, { s_a: 100 }), ['s_a'])).toEqual([]);
    expect(d.observe(sample(10_000, { s_a: 100 }), ['s_a'])).toEqual(['s_a']);
  });

  it('never calls a share frozen that the sample doesn’t measure', () => {
    const d = createFreezeDetector();
    // No stream of it in the stats, or one without a frame count.
    for (const at of [0, 4_000, 8_000]) {
      expect(d.observe(sample(at, {}), ['s_a'])).toEqual([]);
      expect(d.observe(sample(at + 1, { s_a: undefined }), ['s_a'])).toEqual([]);
    }
    // A share that is only measured, not watched, is none of its business either.
    expect(d.observe(sample(20_000, { s_b: 1 }), [])).toEqual([]);
    expect(d.observe(sample(30_000, { s_b: 1 }), [])).toEqual([]);
  });

  it('forgets everything on reset()', () => {
    const d = createFreezeDetector();
    d.observe(sample(0, { s_a: 100 }), ['s_a']);
    d.reset();
    expect(d.observe(sample(4_000, { s_a: 100 }), ['s_a'])).toEqual([]);
  });
});

describe('freezeWatched', () => {
  let viewer: ViewerServices;
  const state = () => viewer.store.getState();

  beforeEach(() => {
    viewer = createViewer();
    // Cy's share on the stage, Bea's a tile in view, Dee's a tile scrolled away, and this page's own.
    syncRoom(
      viewer,
      room(
        shareInfo('s_bea', 'u_bea', 1),
        shareInfo('s_dee', 'u_dee', 2),
        shareInfo('s_cy', 'u_cy', 3),
        shareInfo('s_mine', 'u_alex', 0, { connectionId: 'c_me' }),
      ),
      SELF,
    );
    markShown(viewer.store, 's_cy');
    markShown(viewer.store, 's_bea');
    markShown(viewer.store, 's_mine');
  });

  afterEach(() => {
    viewer.dispose();
  });

  it('is the shares whose video is asked for: the stage and the tiles in view, never the own preview', () => {
    expect(freezeWatched(state(), 0)).toEqual(['s_cy', 's_bea']);
  });

  it('leaves out a stalled share: it has its own notice', () => {
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1, { status: 'stalled' }), shareInfo('s_cy', 'u_cy', 3)), SELF);
    expect(freezeWatched(state(), 0)).toEqual(['s_cy']);
  });

  it('leaves out a share the server doesn’t forward video for, or is still starting', () => {
    state().applyStatus([status('s_cy', { video: 'off', reason: 'codec' })]);
    expect(freezeWatched(state(), 0)).toEqual(['s_bea']);
    state().applyStatus([status('s_cy', { video: 'off', reason: 'waiting' }), status('s_bea', { reason: 'waiting' })]);
    expect(freezeWatched(state(), 0)).toEqual([]);
    // A lower layer than asked for still moves.
    state().applyStatus([status('s_cy', { video: 'low', reason: 'bandwidth' }), status('s_bea', { video: 'low' })]);
    expect(freezeWatched(state(), 0)).toEqual(['s_cy', 's_bea']);
  });

  it('watches nothing while the page is hidden, or once the layer policy turned the video off', () => {
    state().setPageHidden(1_000);
    expect(freezeWatched(state(), 2_000)).toEqual([]);
    state().setPageHidden(null);
    state().setFullscreen(true);
    expect(freezeWatched(state(), 2_000)).toEqual(['s_cy']);
  });
});

describe('attachFreezeWatch', () => {
  let viewer: ViewerServices;
  let emit: (s: StatsSample) => void;
  let listeners: number;
  let detach: () => void;
  const frozen = () => Object.keys(viewer.store.getState().frozen);

  beforeEach(() => {
    viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1), shareInfo('s_cy', 'u_cy', 2)), SELF);
    markShown(viewer.store, 's_cy');
    markShown(viewer.store, 's_bea');
    const fns = new Set<(s: StatsSample) => void>();
    listeners = 0;
    emit = (s) => {
      for (const fn of fns) fn(s);
    };
    detach = attachFreezeWatch(viewer, {
      subscribe(fn) {
        fns.add(fn);
        listeners = fns.size;
        return () => {
          fns.delete(fn);
          listeners = fns.size;
        };
      },
    });
  });

  afterEach(() => {
    detach();
    viewer.dispose();
  });

  it('marks a tile whose frames stopped, and unmarks it when they come again', () => {
    emit(sample(0, { s_cy: 10, s_bea: 10 }));
    emit(sample(2_000, { s_cy: 70, s_bea: 10 }));
    expect(frozen()).toEqual([]);
    emit(sample(4_000, { s_cy: 130, s_bea: 10 }));
    expect(frozen()).toEqual(['s_bea']);
    emit(sample(6_000, { s_cy: 190, s_bea: 12 }));
    expect(frozen()).toEqual([]);
  });

  it('shows "Waiting for video…" on the frozen tile and nothing on the others', () => {
    emit(sample(0, { s_cy: 10, s_bea: 10 }));
    emit(sample(4_000, { s_cy: 130, s_bea: 10 }));
    const s = viewer.store.getState();
    const view = (id: string) => {
      const share = s.shares.find((x) => x.id === id);
      if (!share) throw new Error(`no ${id}`);
      return shareView(share, s.status[id], true, s.frozen[id] === true);
    };
    expect(view('s_bea')).toEqual({ overlay: 'frozen', badge: null });
    expect(view('s_cy')).toEqual({ overlay: null, badge: null });
  });

  it('writes the store only when the set changes', () => {
    const changes = vi.fn();
    const off = viewer.store.subscribe(changes);
    emit(sample(0, { s_cy: 10 }));
    emit(sample(2_000, { s_cy: 20 }));
    expect(changes).not.toHaveBeenCalled();
    emit(sample(4_000, { s_cy: 20 }));
    expect(changes).not.toHaveBeenCalled(); // 2 s since the last frame
    emit(sample(6_000, { s_cy: 20 }));
    expect(changes).toHaveBeenCalledTimes(1);
    emit(sample(8_000, { s_cy: 20 }));
    emit(sample(10_000, { s_cy: 20 }));
    expect(changes).toHaveBeenCalledTimes(1); // still frozen: nothing new to say
    off();
  });

  it('drops the mark with the tile: a share that ended, a page that left its room', () => {
    emit(sample(0, { s_cy: 10, s_bea: 10 }));
    emit(sample(4_000, { s_cy: 10, s_bea: 10 }));
    expect(frozen().sort()).toEqual(['s_bea', 's_cy']);
    syncRoom(viewer, room(shareInfo('s_cy', 'u_cy', 2)), SELF);
    expect(frozen()).toEqual(['s_cy']);
    viewer.store.getState().reset();
    expect(frozen()).toEqual([]);
  });

  it('clears the marks and stops listening with the detach', () => {
    emit(sample(0, { s_cy: 10 }));
    emit(sample(4_000, { s_cy: 10 }));
    expect(frozen()).toEqual(['s_cy']);
    detach();
    expect(frozen()).toEqual([]);
    expect(listeners).toBe(0);
  });
});
