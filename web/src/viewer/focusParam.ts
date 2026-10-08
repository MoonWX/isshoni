// `?focus=<shareId>` (05 §12.2; the links of push notifications, 04 §14.3: /r/lounge?focus=<shareId>). It acts as
// a manual focus once that share is live: the reducer picks it when it is there, or keeps it as pendingFocusParam
// until room.state lists it (autoFocus.ts). This module is the wait around that: up to 5 s (05 §18) for the share
// to appear, and then, picked or not, the caller drops the parameter from the URL, so a reload or a shared link
// doesn't pick again. No React here (05 §3); FocusFromUrl.tsx reads the URL.
//
// The wait survives what happens to the store meanwhile:
// - a link that names another room makes the session switch rooms, which empties the store (reset()) and with it
//   the pending id: it is asked for again;
// - a pick by the user, or a newer request for another share (the toast's "Watch"), ends the wait: what the user
//   chose last wins.
import type { ViewerState, ViewerStore } from './viewerStore';

/** The query parameter's name. */
export const FOCUS_PARAM = 'focus';
/** How long a `?focus=` share is waited for (05 §18). */
export const FOCUS_PARAM_WAIT_MS = 5_000;

/**
 * Asks the store to focus shareId as soon as it is live, for at most waitMs. `done` is called once when the wait
 * is over, whatever the outcome: the share got the stage, the time ran out, or the user chose something else.
 * Returns the function that gives the wait up without calling `done` (the page is going away, or the link changed).
 */
export function followFocusParam(
  store: ViewerStore,
  shareId: string,
  done: () => void,
  waitMs: number = FOCUS_PARAM_WAIT_MS,
): () => void {
  const picked = (s: ViewerState): boolean => s.focusedShareId === shareId && s.focusMode === 'manual';
  const ask = (): void => {
    store.getState().dispatch({ type: 'focusParam', shareId });
  };

  ask();
  if (picked(store.getState())) {
    // The share is live already: nothing to wait for.
    done();
    return () => undefined;
  }

  let over = false;
  /** Ends the wait; the pending id goes unless the store has moved on from it already. */
  const stop = (): void => {
    if (over) return;
    over = true;
    clearTimeout(timer);
    off();
    if (store.getState().pendingFocusParam === shareId)
      store.getState().dispatch({ type: 'focusParam', shareId: null });
  };
  const finish = (): void => {
    if (over) return;
    stop();
    done();
  };
  const off = store.subscribe((s) => {
    if (over || s.pendingFocusParam === shareId) return;
    if (picked(s)) finish();
    else if (s.pendingFocusParam === null && s.focusMode === 'auto') ask();
    else finish();
  });
  const timer = setTimeout(finish, waitMs);
  return stop;
}
