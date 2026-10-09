// useVisibility (05 §12.4): viewerStore.visible says which shares the page shows: the ones whose tile, or the
// stage, is mounted and not known to be out of view (less than 10% inside the viewport, by IntersectionObserver).
// Without an observer, the ones whose element is mounted.
import { act, render } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ViewerContext } from './context';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { installFakeIntersectionObserver, room, SELF, shareInfo } from './testing';
import { markShown, showWhileVisible, useVisibility, VISIBLE_RATIO, watchVisible } from './useVisibility';

function Shows({ shareId }: { shareId: string | null }) {
  const ref = useVisibility(shareId);
  return <div ref={ref} data-shows={shareId ?? ''} />;
}

const inViewer = (viewer: ViewerServices, ...shareIds: (string | null)[]) => (
  <ViewerContext.Provider value={viewer}>
    {shareIds.map((id, i) => (
      <Shows key={String(i)} shareId={id} />
    ))}
  </ViewerContext.Provider>
);

/** The element that shows a share. */
const el = (shareId: string): Element => {
  const found = document.querySelector(`[data-shows="${shareId}"]`);
  if (!found) throw new Error(`nothing shows ${shareId}`);
  return found;
};

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('markShown', () => {
  it('marks a share while at least one element shows it', () => {
    const { store } = createViewer();
    const visible = () => store.getState().visible;
    const tile = markShown(store, 's_a');
    expect(visible()).toEqual({ s_a: true });
    // The share moves to the stage: for a moment both show it.
    const stage = markShown(store, 's_a');
    tile();
    expect(visible()).toEqual({ s_a: true });
    tile(); // taking it back twice counts once
    expect(visible()).toEqual({ s_a: true });
    stage();
    expect(visible()).toEqual({ s_a: false });

    markShown(store, 's_a');
    expect(visible()).toEqual({ s_a: true });
  });

  it('keeps the stores apart', () => {
    const a = createViewer().store;
    const b = createViewer().store;
    const off = markShown(a, 's_x');
    markShown(b, 's_x');
    off();
    expect(a.getState().visible).toEqual({ s_x: false });
    expect(b.getState().visible).toEqual({ s_x: true });
  });
});

describe('watchVisible', () => {
  it('says "visible" from 10% of the element inside the viewport, and follows every change', () => {
    expect(VISIBLE_RATIO).toBe(0.1);
    const io = installFakeIntersectionObserver(null);
    const tile = document.createElement('li');
    const onChange = vi.fn<(visible: boolean) => void>();
    const stop = watchVisible(tile, onChange);
    // Nothing until the browser has measured.
    expect(onChange).not.toHaveBeenCalled();
    expect(io.observed()).toEqual([tile]);

    io.show(tile, 1);
    io.show(tile, 0.1);
    io.show(tile, 0.09); // a sliver at the strip's edge
    io.show(tile, 0);
    io.show(tile, 0.5);
    expect(onChange.mock.calls.map(([visible]) => visible)).toEqual([true, true, false, false, true]);

    stop();
    expect(io.observed()).toEqual([]);
    io.show(tile, 0);
    expect(onChange).toHaveBeenCalledTimes(5);
  });

  it('observes with the 10% threshold, so the browser reports the crossings that matter', () => {
    const seen: IntersectionObserverInit[] = [];
    class Recording {
      constructor(_cb: IntersectionObserverCallback, init: IntersectionObserverInit = {}) {
        seen.push(init);
      }
      observe(): void {}
      disconnect(): void {}
    }
    vi.stubGlobal('IntersectionObserver', Recording);
    watchVisible(document.createElement('li'), () => undefined)();
    expect(seen).toEqual([{ threshold: VISIBLE_RATIO }]);
  });

  it('has nothing to say where there is no IntersectionObserver', () => {
    expect(typeof IntersectionObserver).toBe('undefined'); // jsdom
    const onChange = vi.fn();
    watchVisible(document.createElement('li'), onChange)();
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe('showWhileVisible', () => {
  it('marks the share in the store from the start, and then while its element is in view', () => {
    const io = installFakeIntersectionObserver(null);
    const { store } = createViewer();
    const tile = document.createElement('li');
    const stop = showWhileVisible(store, 's_a', tile);
    // Shown until the browser says otherwise: it measures a frame later.
    expect(store.getState().visible).toEqual({ s_a: true });
    io.show(tile, 0.6);
    expect(store.getState().visible).toEqual({ s_a: true });
    io.show(tile, 0.6); // the same again: still one mark
    io.show(tile, 0);
    expect(store.getState().visible).toEqual({ s_a: false });
    io.show(tile, 0.2);
    stop();
    expect(store.getState().visible).toEqual({ s_a: false });
    expect(io.observed()).toEqual([]);
  });

  it('keeps a share shown that the stage shows while its tile scrolls away', () => {
    const io = installFakeIntersectionObserver();
    const { store } = createViewer();
    const stage = document.createElement('section');
    const tile = document.createElement('li');
    showWhileVisible(store, 's_a', stage);
    showWhileVisible(store, 's_a', tile);
    io.show(tile, 0);
    expect(store.getState().visible).toEqual({ s_a: true });
    io.show(stage, 0);
    expect(store.getState().visible).toEqual({ s_a: false });
  });
});

describe('useVisibility', () => {
  it('marks its share while its element is mounted, and follows a change of share (no observer: jsdom)', () => {
    const viewer = createViewer();
    const visible = () => viewer.store.getState().visible;
    const { rerender, unmount } = render(inViewer(viewer, 's_bea', 's_cy'));
    expect(visible()).toEqual({ s_bea: true, s_cy: true });

    // A click on Cy's tile: the stage and the tile swap their shares in one render.
    rerender(inViewer(viewer, 's_cy', 's_bea'));
    expect(visible()).toEqual({ s_bea: true, s_cy: true });

    rerender(inViewer(viewer, 's_cy', null));
    expect(visible()).toEqual({ s_bea: false, s_cy: true });
    unmount();
    expect(visible()).toEqual({ s_bea: false, s_cy: false });
  });

  it('marks only what is in view: a tile scrolled out of the strip is not shown', () => {
    const io = installFakeIntersectionObserver();
    const viewer = createViewer();
    const visible = () => viewer.store.getState().visible;
    const { unmount } = render(inViewer(viewer, 's_bea', 's_cy'));
    expect(visible()).toEqual({ s_bea: true, s_cy: true });

    act(() => {
      io.show(el('s_bea'), 0);
    });
    expect(visible()).toEqual({ s_bea: false, s_cy: true });
    act(() => {
      io.show(el('s_bea'), 0.3);
    });
    expect(visible()).toEqual({ s_bea: true, s_cy: true });

    unmount();
    expect(visible()).toEqual({ s_bea: false, s_cy: false });
    expect(io.observed()).toEqual([]);
  });

  it('counts a new element as shown until the browser has measured it: a tile that mounts out of view', () => {
    const io = installFakeIntersectionObserver(null);
    const viewer = createViewer();
    render(inViewer(viewer, 's_bea'));
    expect(viewer.store.getState().visible).toEqual({ s_bea: true });
    act(() => {
      io.show(el('s_bea'), 0);
    });
    expect(viewer.store.getState().visible).toEqual({ s_bea: false });
  });

  it('keeps the stage shown through a change of its share: no moment in which the policy sees it empty', () => {
    installFakeIntersectionObserver(null);
    const viewer = createViewer();
    const seen: Record<string, boolean>[] = [];
    const { rerender } = render(inViewer(viewer, 's_bea'));
    const off = viewer.store.subscribe((s) => seen.push({ ...s.visible }));
    rerender(inViewer(viewer, 's_cy'));
    off();
    expect(seen.at(-1)).toEqual({ s_bea: false, s_cy: true });
    // The new share is marked in the same commit that unmarks the old one.
    expect(seen).toHaveLength(2);
  });

  it('observes the element of the new share when its share changes', () => {
    const io = installFakeIntersectionObserver();
    const viewer = createViewer();
    const { rerender } = render(inViewer(viewer, 's_bea'));
    rerender(inViewer(viewer, 's_cy'));
    expect(viewer.store.getState().visible).toEqual({ s_bea: false, s_cy: true });
    expect(io.observed()).toEqual([el('s_cy')]);
  });

  it('marks nothing for null', () => {
    const io = installFakeIntersectionObserver();
    const viewer = createViewer();
    render(inViewer(viewer, null));
    expect(viewer.store.getState().visible).toEqual({});
    expect(io.observed()).toEqual([]);
  });

  it('marks its share again when the store forgot it while the element stayed (reset(), then the same room)', () => {
    const viewer = createViewer();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(inViewer(viewer, 's_bea'));
    expect(viewer.store.getState().visible).toEqual({ s_bea: true });

    act(() => {
      viewer.store.getState().reset();
      syncRoom(viewer, room(BEA, CY), SELF);
    });
    expect(viewer.store.getState().visible).toEqual({ s_bea: true });
  });

  it('lets a tile of a live share go out of view: the store’s own forgetting is not mistaken for it', () => {
    const io = installFakeIntersectionObserver();
    const viewer = createViewer();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(inViewer(viewer, 's_bea', 's_cy'));
    act(() => {
      io.show(el('s_bea'), 0);
    });
    expect(viewer.store.getState().visible).toEqual({ s_bea: false, s_cy: true });
  });

  it('doesn’t bring back a share that the store forgot while it was out of view', () => {
    const io = installFakeIntersectionObserver();
    const viewer = createViewer();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(inViewer(viewer, 's_bea'));
    act(() => {
      io.show(el('s_bea'), 0);
      viewer.store.getState().reset();
      syncRoom(viewer, room(BEA, CY), SELF);
    });
    expect(viewer.store.getState().visible).toEqual({});
  });

  it('leaves a share that ended alone', () => {
    const viewer = createViewer();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(inViewer(viewer, 's_bea'));
    act(() => {
      syncRoom(viewer, room(CY), SELF);
    });
    // The store dropped its per-share state; the element, still mounted for a moment, doesn't bring it back.
    expect(viewer.store.getState().visible).toEqual({});
  });
});
