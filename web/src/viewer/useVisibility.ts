// Which shares the page shows (05 §12.4): viewerStore.visible, the layer policy's `visible`. A share that nothing
// shows gets no video, so the tiles of a room page the user navigated away from cost no bandwidth (05 §11.1).
//
// With this slice (S47) "shown" means mounted: the stage and each tile mark their share while they are in the
// document. S56 (05 W8) refines it to "at least 10% inside the viewport" with an IntersectionObserver on the
// element, which also covers tiles scrolled out of the strip; the store's field and this hook's callers stay.
import { useEffect } from 'react';

import { useViewer, useViewerServices } from './context';
import type { ViewerStore } from './viewerStore';

/** Per store: how many mounted elements show each share. The stage and a tile can show the same one for a moment. */
const shownBy = new WeakMap<ViewerStore, Map<string, number>>();

/** Counts one more element that shows the share. Returns the function that takes it back. */
export function markShown(store: ViewerStore, shareId: string): () => void {
  let counts = shownBy.get(store);
  if (!counts) {
    counts = new Map();
    shownBy.set(store, counts);
  }
  const shown = counts;
  shown.set(shareId, (shown.get(shareId) ?? 0) + 1);
  store.getState().setVisible(shareId, true);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const left = (shown.get(shareId) ?? 1) - 1;
    if (left > 0) {
      shown.set(shareId, left);
      return;
    }
    shown.delete(shareId);
    store.getState().setVisible(shareId, false);
  };
}

/** Marks the share as shown while the calling component is mounted with it; null marks nothing. */
export function useVisibility(shareId: string | null): void {
  const { store } = useViewerServices();
  // The store forgets its per-share state when the page leaves the room (reset()). A share that is back before
  // this component unmounted is marked again.
  const forgotten = useViewer(
    (s) => shareId !== null && s.visible[shareId] !== true && s.shares.some((share) => share.id === shareId),
  );

  useEffect(() => {
    if (shareId === null) return undefined;
    return markShown(store, shareId);
  }, [store, shareId]);

  useEffect(() => {
    if (forgotten && shareId !== null) store.getState().setVisible(shareId, true);
  }, [forgotten, store, shareId]);
}
