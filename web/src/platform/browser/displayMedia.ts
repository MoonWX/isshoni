// STUB until S35 (05 W6 capture) replaces this file; S46 (publish) completes start().
//
// Contract for the replacement: BrowserPlatform imports createBrowserSharing() from here and calls it once, only on a
// device that may share (05 §8: getDisplayMedia exists, the sender encodes H.264, not a phone or tablet), so the
// export and its signature stay. S35 implements pick() (the getDisplayMedia options and fallbacks of 05 §13.2,
// classify.ts of §13.3, the loopback-only fake-display seam of §19.3); start() is share/'s BrowserSharing (S46).
import { NotImplementedError } from '../../lib/errors';
import type { SharingProvider } from '../types';

/** The in-page SharingProvider of BrowserPlatform. The stub rejects every call with NotImplementedError. */
export function createBrowserSharing(): SharingProvider {
  return {
    mode: 'in-page',
    pick: () => Promise.reject(new NotImplementedError('sharing.pick', 'S35')),
    start: () => Promise.reject(new NotImplementedError('sharing.start', 'S46')),
  };
}
