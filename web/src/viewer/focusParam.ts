// `?focus=<shareId>` (05 §12.2; the links of push notifications, 04 §14.3: /r/lounge?focus=<shareId>). It acts as
// a manual focus once that share is live: the reducer picks it when it is there, or keeps it as pendingFocusParam
// until room.state lists it (autoFocus.ts). This module is the wait around that: up to 5 s (05 §18) for the share
// to appear, and then, picked or not, the caller drops the parameter from the URL, so a reload or a shared link
// doesn't pick again. No React here (05 §3); FocusFromUrl.tsx reads the URL.
//
// The 5 s count from the room's first snapshot (viewerStore.inRoom), not from the link. A push link often opens
// the app cold: the layout is there while the page still connects and joins, and until room.state came nobody
// knows which shares the room has. Out of a room the clock doesn't run, and the next room gets the whole 5 s.
// While it doesn't run the id just stays pending: there is no stage to move, and the wait ends with the page
// (FocusFromUrl gives it up when the layout goes or the link changes).
//
// One case is still timed from the link: a page that was in the room already and is reconnecting, as a phone does
// when a notification's tap brings its tab back. The store keeps the snapshot of before (the session keeps
// syncing after a welcome, for re-published shares: index.ts), so `inRoom` is true and the 5 s start at once. A
// share that began while the page was away is then picked only if the fresh room.state comes in time; after that
// it is a new share like any other: auto-focus shows it when it is the newest, and a pick the user holds stays.
//
// The wait survives what happens to the store meanwhile:
// - a link that names another room makes the session switch rooms, which empties the store (reset()) and with it
//   the pending id: it is asked for again, and the clock starts over with the new room's first snapshot;
// - a pick by the user, or a newer request for another share (the toast's "Watch"), ends the wait: what the user
//   chose last wins.
import type { ViewerState, ViewerStore } from './viewerStore';

/** The query parameter's name. */
export const FOCUS_PARAM = 'focus';
/** How long a `?focus=` share is waited for once the room's shares are known (05 §18). */
export const FOCUS_PARAM_WAIT_MS = 5_000;

/**
 * Asks the store to focus shareId as soon as it is live, for at most waitMs after the room's first snapshot (see
 * above). `done` is called once when the wait is over, whatever the outcome: the share got the stage, the time ran
 * out, or the user chose something else. Returns the function that gives the wait up without calling `done` (the
 * page is going away, or the link changed).
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
  /** Runs while the store has a room's snapshot; null out of a room. */
  let timer: ReturnType<typeof setTimeout> | null = null;
  /** Ends the wait; the pending id goes unless the store has moved on from it already. */
  const stop = (): void => {
    if (over) return;
    over = true;
    if (timer !== null) clearTimeout(timer);
    timer = null;
    off();
    if (store.getState().pendingFocusParam === shareId)
      store.getState().dispatch({ type: 'focusParam', shareId: null });
  };
  const finish = (): void => {
    if (over) return;
    stop();
    done();
  };
  /** Starts the clock with a room's first snapshot and stops it when the page is out of the room. */
  const clock = (inRoom: boolean): void => {
    if (inRoom === (timer !== null)) return;
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    } else {
      timer = setTimeout(finish, waitMs);
    }
  };
  const off = store.subscribe((s) => {
    if (over) return;
    clock(s.inRoom);
    if (s.pendingFocusParam === shareId) return;
    if (picked(s)) finish();
    else if (s.pendingFocusParam === null && s.focusMode === 'auto') ask();
    else finish();
  });
  clock(store.getState().inRoom);
  return stop;
}
