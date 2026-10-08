// useVisibility (05 §12.4): viewerStore.visible says which shares the page shows. With this slice a share is shown
// while an element with it is mounted.
import { act, render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ViewerContext } from './context';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { markShown, useVisibility } from './useVisibility';

function Shows({ shareId }: { shareId: string | null }) {
  useVisibility(shareId);
  return null;
}

const inViewer = (viewer: ViewerServices, ...shareIds: (string | null)[]) => (
  <ViewerContext.Provider value={viewer}>
    {shareIds.map((id, i) => (
      <Shows key={String(i)} shareId={id} />
    ))}
  </ViewerContext.Provider>
);

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);

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

describe('useVisibility', () => {
  it('marks its share while the component is mounted, and follows a change of share', () => {
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

  it('marks nothing for null', () => {
    const viewer = createViewer();
    render(inViewer(viewer, null));
    expect(viewer.store.getState().visible).toEqual({});
  });

  it('marks its share again when the store forgot it while the component stayed (reset(), then the same room)', () => {
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

  it('leaves a share that ended alone', () => {
    const viewer = createViewer();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(inViewer(viewer, 's_bea'));
    act(() => {
      syncRoom(viewer, room(CY), SELF);
    });
    // The store dropped its per-share state; the component, still mounted for a moment, doesn't bring it back.
    expect(viewer.store.getState().visible).toEqual({});
  });
});
