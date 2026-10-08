// The screen wake lock (05 §12.5): the screen stays on while a share is being watched and the page is visible.
// The lock itself is the platform's (platform.requestWakeLock(), 05 §8), which answers null where the browser has
// no Screen Wake Lock API or refuses (battery saver, iOS Low Power Mode); this controller only decides when to
// hold one. The caller asks for it only where PlatformCapabilities.wakeLock says the API exists.
//
// The browser drops the lock by itself when the page is hidden. So `wanted` goes false with the page's visibility
// (the lock is released, which is a no-op by then) and true again on `visibilitychange → visible`, which requests
// a new one. A request that was refused is not repeated until then. No React here (05 §3).
import type { Logger } from '../lib/log';
import type { WakeLockHandle } from '../platform/types';
import type { ViewerData } from './viewerStore';

export interface WakeLockDeps {
  /** platform.requestWakeLock. */
  request: () => Promise<WakeLockHandle | null>;
  log?: Logger;
}

export interface WakeLockController {
  /** Whether the screen should stay on now. Asking again for what is already so does nothing. */
  set(wanted: boolean): void;
  /** Releases the lock for good: a request still on its way is released when it arrives. */
  dispose(): void;
}

/**
 * Whether the page is showing someone's share right now: it is visible, and a share that it receives (not its own
 * capture's preview) is on the stage or in a tile that is in view.
 */
export function isWatching(s: Pick<ViewerData, 'shares' | 'visible' | 'pageHiddenSince'>): boolean {
  return s.pageHiddenSince === null && s.shares.some((share) => !share.local && s.visible[share.id] === true);
}

export function createWakeLock(deps: WakeLockDeps): WakeLockController {
  const { log } = deps;
  let wanted = false;
  let handle: WakeLockHandle | null = null;
  /** A request the platform has not answered yet: at most one at a time. */
  let pending = false;

  const release = (h: WakeLockHandle): void => {
    h.release().catch((err: unknown) => {
      log?.debug('wake lock release failed', { error: err });
    });
  };

  const sync = (): void => {
    if (pending) return;
    if (!wanted) {
      if (handle === null) return;
      const h = handle;
      handle = null;
      release(h);
      return;
    }
    if (handle !== null) return;
    pending = true;
    void deps
      .request()
      .catch((err: unknown) => {
        log?.debug('wake lock request failed', { error: err });
        return null;
      })
      .then((h) => {
        pending = false;
        if (h === null) return;
        // What is wanted may have changed while the platform was asked.
        if (wanted) handle = h;
        else release(h);
      });
  };

  return {
    set(next) {
      if (wanted === next) return;
      wanted = next;
      sync();
    },
    dispose() {
      wanted = false;
      sync();
    },
  };
}
