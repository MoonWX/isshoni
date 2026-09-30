// platform.requestWakeLock (05 §8, §12.5): keeps the screen on while watching. The Screen Wake Lock API needs a
// visible page and may refuse (battery saver, Low Power Mode); every failure is "no lock", never an error.

import type { WakeLockHandle } from '../types';

/** Requests a screen wake lock; null when the API is missing or the browser refused. */
export async function requestScreenWakeLock(): Promise<WakeLockHandle | null> {
  // Older Safari and Firefox have no navigator.wakeLock, whatever lib.dom says.
  const wakeLock = (globalThis.navigator as { wakeLock?: WakeLock }).wakeLock;
  if (!wakeLock) return null;
  try {
    const sentinel = await wakeLock.request('screen');
    return {
      async release() {
        if (sentinel.released) return;
        try {
          await sentinel.release();
        } catch {
          // already released by the browser (page hidden)
        }
      },
    };
  } catch {
    return null;
  }
}
