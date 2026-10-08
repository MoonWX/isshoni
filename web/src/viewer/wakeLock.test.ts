// The screen wake lock (05 §12.5): held while a share is watched and the page is visible, released when not, and
// requested anew when the page comes back.
import { describe, expect, it, vi } from 'vitest';

import type { WakeLockHandle } from '../platform/types';
import { createViewer, syncRoom } from './services';
import { room, SELF, shareInfo } from './testing';
import { markShown } from './useVisibility';
import { createWakeLock, isWatching } from './wakeLock';

/** A platform whose wake lock requests the test answers. */
function platform() {
  const handles: { release: ReturnType<typeof vi.fn<() => Promise<void>>> }[] = [];
  const pending: ((granted: boolean) => void)[] = [];
  const request = vi.fn(
    () =>
      new Promise<WakeLockHandle | null>((resolve) => {
        pending.push((granted) => {
          if (!granted) {
            resolve(null);
            return;
          }
          const handle = { release: vi.fn(() => Promise.resolve()) };
          handles.push(handle);
          resolve(handle);
        });
      }),
  );
  return {
    request,
    handles,
    /** Answers the oldest open request. */
    async answer(granted = true): Promise<void> {
      pending.shift()?.(granted);
      await Promise.resolve();
      await Promise.resolve();
    },
  };
}

describe('createWakeLock', () => {
  it('requests a lock when one is wanted and releases it when not', async () => {
    const p = platform();
    const lock = createWakeLock({ request: p.request });
    expect(p.request).not.toHaveBeenCalled();

    lock.set(true);
    lock.set(true); // asking again changes nothing
    expect(p.request).toHaveBeenCalledTimes(1);
    await p.answer();
    expect(p.handles[0]?.release).not.toHaveBeenCalled();

    lock.set(false);
    expect(p.handles[0]?.release).toHaveBeenCalledOnce();
    lock.set(false);
    expect(p.handles[0]?.release).toHaveBeenCalledOnce();
  });

  it('requests a new one when the page comes back: the browser dropped the old one while it was hidden', async () => {
    const p = platform();
    const lock = createWakeLock({ request: p.request });
    lock.set(true);
    await p.answer();
    lock.set(false); // visibilitychange → hidden
    lock.set(true); // → visible
    expect(p.request).toHaveBeenCalledTimes(2);
    await p.answer();
    expect(p.handles).toHaveLength(2);
    expect(p.handles[1]?.release).not.toHaveBeenCalled();
  });

  it('releases a lock that arrives after it stopped being wanted', async () => {
    const p = platform();
    const lock = createWakeLock({ request: p.request });
    lock.set(true);
    lock.set(false);
    await p.answer();
    expect(p.handles[0]?.release).toHaveBeenCalledOnce();
  });

  it('keeps the one request that is on its way when the wish flips back meanwhile', async () => {
    const p = platform();
    const lock = createWakeLock({ request: p.request });
    lock.set(true);
    lock.set(false);
    lock.set(true);
    expect(p.request).toHaveBeenCalledTimes(1);
    await p.answer();
    expect(p.handles[0]?.release).not.toHaveBeenCalled();
    lock.set(false);
    expect(p.handles[0]?.release).toHaveBeenCalledOnce();
  });

  it('takes a refusal as "no lock" and asks again only with the next wish', async () => {
    const p = platform();
    const lock = createWakeLock({ request: p.request });
    lock.set(true);
    await p.answer(false); // battery saver, Low Power Mode
    expect(p.request).toHaveBeenCalledTimes(1);
    lock.set(false);
    lock.set(true);
    expect(p.request).toHaveBeenCalledTimes(2);
  });

  it('survives a platform that rejects, and a release that fails', async () => {
    const lock = createWakeLock({ request: () => Promise.reject(new Error('no')) });
    lock.set(true);
    await Promise.resolve();
    await Promise.resolve();
    lock.set(false);

    const release = vi.fn(() => Promise.reject(new Error('gone')));
    const second = createWakeLock({ request: () => Promise.resolve({ release }) });
    second.set(true);
    await Promise.resolve();
    await Promise.resolve();
    second.set(false);
    await Promise.resolve();
    expect(release).toHaveBeenCalledOnce();
  });

  it('releases for good when disposed, also a lock that is still on its way', async () => {
    const p = platform();
    const held = createWakeLock({ request: p.request });
    held.set(true);
    await p.answer();
    held.dispose();
    expect(p.handles[0]?.release).toHaveBeenCalledOnce();

    const late = createWakeLock({ request: p.request });
    late.set(true);
    late.dispose();
    await p.answer();
    expect(p.handles[1]?.release).toHaveBeenCalledOnce();
  });
});

describe('isWatching', () => {
  it('is true while a received share is in view on a visible page', () => {
    const viewer = createViewer();
    const state = () => viewer.store.getState();
    expect(isWatching(state())).toBe(false);
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1)), SELF);
    expect(isWatching(state())).toBe(false); // the room page is not mounted
    const hide = markShown(viewer.store, 's_bea');
    expect(isWatching(state())).toBe(true);

    state().setPageHidden(5);
    expect(isWatching(state())).toBe(false);
    state().setPageHidden(null);
    expect(isWatching(state())).toBe(true);
    hide();
    expect(isWatching(state())).toBe(false);
    viewer.dispose();
  });

  it('doesn’t count the preview of the page’s own capture, and counts the user’s share from another device', () => {
    const viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me' })), SELF);
    markShown(viewer.store, 's_mine');
    expect(isWatching(viewer.store.getState())).toBe(false);

    syncRoom(viewer, room(shareInfo('s_phone', 'u_alex', 2, { connectionId: 'c_phone' })), SELF);
    markShown(viewer.store, 's_phone');
    expect(isWatching(viewer.store.getState())).toBe(true);
    viewer.dispose();
  });
});
