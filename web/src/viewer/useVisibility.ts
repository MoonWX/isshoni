// Which shares the page shows (05 §12.4): viewerStore.visible, the layer policy's `visible`. A share that nothing
// shows gets no video, so a tile scrolled out of the strip costs no bandwidth, and neither do the tiles of a room
// page the user navigated away from (05 §11.1).
//
// "Shown" means that at least 10% of the element with the share's video, a tile or the stage, is inside the
// viewport: an IntersectionObserver on that element, which also sees what the strip's own scrolling clips. An
// element counts as shown from the moment it is mounted, until the observer says that it is out of view: the
// browser measures a frame later, and a stage that changes its share must not look empty to the policy meanwhile
// (a tile that mounts out of view is corrected within that frame, well inside the 150 ms that subscription
// changes are batched for). Where there is no IntersectionObserver (an old browser, a test DOM), nothing ever
// says otherwise, and an element is shown while it is in the document.
//
// What the observer can't see is handled elsewhere: tiles under a fullscreen stage (the layer policy's rule 5),
// and a hidden page (rule 3: the browser stops reporting intersections then, so the marks stay as they were).
import { useCallback, type RefCallback } from 'react';

import { useViewerServices } from './context';
import type { ViewerStore } from './viewerStore';

/** How much of a tile must be inside the viewport for its share to count as shown (05 §12.4). */
export const VISIBLE_RATIO = 0.1;

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

/**
 * Calls onChange with whether at least VISIBLE_RATIO of el is inside the viewport: once the browser has measured
 * it, and whenever that changes. Without IntersectionObserver it never calls. Returns the function that stops it.
 */
export function watchVisible(el: Element, onChange: (visible: boolean) => void): () => void {
  if (typeof IntersectionObserver !== 'function') return () => undefined;
  const observer = new IntersectionObserver(
    (entries) => {
      // Several entries in one call are a history: the last one is how it is now.
      const entry = entries.at(-1);
      if (entry) onChange(entry.isIntersecting && entry.intersectionRatio >= VISIBLE_RATIO);
    },
    { threshold: VISIBLE_RATIO },
  );
  observer.observe(el);
  return () => {
    observer.disconnect();
  };
}

/**
 * Marks the share as shown in the store from now on, except while el is known to be out of view. Returns the
 * function that stops it and takes the mark back.
 */
export function showWhileVisible(store: ViewerStore, shareId: string, el: Element): () => void {
  let release: (() => void) | null = markShown(store, shareId);
  /** Takes this element's mark back. The mark is gone before the store hears of it (the subscription below). */
  const unmark = (): void => {
    const off = release;
    release = null;
    off?.();
  };
  const stopWatching = watchVisible(el, (visible) => {
    if (visible) release ??= markShown(store, shareId);
    else unmark();
  });
  // The store forgets its per-share state when the page leaves the room (reset()). A share that is back while
  // this element still shows it is marked again. One that ended is left alone.
  const offStore = store.subscribe((s) => {
    if (release !== null && s.visible[shareId] !== true && s.shares.some((share) => share.id === shareId)) {
      s.setVisible(shareId, true);
    }
  });
  return () => {
    stopWatching();
    offStore();
    unmark();
  };
}

/**
 * The ref of the element that shows a share's video (a tile's <li>, the stage's <section>): while that element is
 * mounted and in view, the share counts as shown. null marks nothing.
 */
export function useVisibility(shareId: string | null): RefCallback<Element> {
  const { store } = useViewerServices();
  return useCallback(
    (el: Element | null) => {
      if (el === null || shareId === null) return undefined;
      return showWhileVisible(store, shareId, el);
    },
    [store, shareId],
  );
}
