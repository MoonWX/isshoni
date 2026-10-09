// The page's visibility (05 §12.4 rule 3, §12.8): the store knows since when the page is hidden, and when it comes
// back every video and the <audio> element are played again (iOS paused them).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { FakeMediaStreamTrack, installFakeMedia, installFakeMediaElement } from '../test/fakeMedia';
import { attachPage } from './page';
import { createViewer, syncRoom } from './services';
import { viewerWants } from './subscriptions';
import { room, SELF, shareInfo } from './testing';
import { markShown } from './useVisibility';
import { createViewerStore, type ViewerStore } from './viewerStore';

let hidden = false;

/** Hides or shows the page as the browser does: the state first, then the event. */
function setHidden(next: boolean): void {
  hidden = next;
  document.dispatchEvent(new Event('visibilitychange'));
}

beforeEach(() => {
  hidden = false;
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => (hidden ? 'hidden' : 'visible'),
  });
});

afterEach(() => {
  Reflect.deleteProperty(document, 'visibilityState');
  vi.unstubAllGlobals();
});

describe('attachPage', () => {
  let store: ViewerStore;
  let now: number;
  const videos = { resume: vi.fn() };
  const audio = { resume: vi.fn() };
  const attach = (): (() => void) => attachPage({ store, videos, audio, now: () => now });

  beforeEach(() => {
    store = createViewerStore();
    now = 10_000;
    videos.resume.mockClear();
    audio.resume.mockClear();
  });

  it('remembers when the page was hidden and forgets it when the page is back', () => {
    const off = attach();
    expect(store.getState().pageHiddenSince).toBeNull();
    setHidden(true);
    expect(store.getState().pageHiddenSince).toBe(10_000);
    now = 25_000;
    setHidden(false);
    expect(store.getState().pageHiddenSince).toBeNull();
    off();
  });

  it('keeps the first time when the browser says "hidden" twice', () => {
    const off = attach();
    setHidden(true);
    now = 12_000;
    setHidden(true);
    expect(store.getState().pageHiddenSince).toBe(10_000);
    off();
  });

  it('plays every video and the audio again when the page comes back (iOS suspended them)', () => {
    const off = attach();
    setHidden(true);
    expect(videos.resume).not.toHaveBeenCalled();
    setHidden(false);
    expect(videos.resume).toHaveBeenCalledOnce();
    expect(audio.resume).toHaveBeenCalledOnce();
    // Not for a "visible" that follows no "hidden".
    setHidden(false);
    expect(videos.resume).toHaveBeenCalledOnce();
    off();
  });

  it('knows a page that starts in the background', () => {
    hidden = true;
    const off = attach();
    expect(store.getState().pageHiddenSince).toBe(10_000);
    off();
  });

  it('stops with the detach', () => {
    const off = attach();
    off();
    setHidden(true);
    expect(store.getState().pageHiddenSince).toBeNull();
  });

  it('writes the store once per change, so nothing that follows it runs twice', () => {
    const off = attach();
    const changes = vi.fn();
    const unsubscribe = store.subscribe(changes);
    setHidden(false);
    expect(changes).not.toHaveBeenCalled();
    setHidden(true);
    setHidden(true);
    expect(changes).toHaveBeenCalledTimes(1);
    unsubscribe();
    off();
  });
});

describe('createViewer follows the page', () => {
  it('turns every video off once the page was hidden for 10 s, and keeps the sound (rule 3, rule 1)', () => {
    vi.useFakeTimers({ now: 1_000_000 });
    const viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1), shareInfo('s_cy', 'u_cy', 2)), SELF);
    markShown(viewer.store, 's_cy');
    markShown(viewer.store, 's_bea');
    const wants = () => viewerWants(viewer.store.getState()).map((w) => `${w.shareId} ${w.video}/${w.audio}`);
    expect(wants()).toEqual(['s_cy high/on', 's_bea low/off']);

    setHidden(true);
    vi.advanceTimersByTime(9_999);
    expect(wants()).toEqual(['s_cy high/on', 's_bea low/off']);
    vi.advanceTimersByTime(1);
    expect(wants()).toEqual(['s_cy off/on', 's_bea off/off']);

    setHidden(false);
    expect(wants()).toEqual(['s_cy high/on', 's_bea low/off']);
    viewer.dispose();
    vi.useRealTimers();
  });

  it('plays a paused tile video and the audio again when the page comes back', async () => {
    installFakeMedia();
    const media = installFakeMediaElement();
    const viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1)), SELF);
    viewer.registry.set('s_bea', 'audio', new FakeMediaStreamTrack('audio') as unknown as MediaStreamTrack);
    const tile = document.createElement('video');
    viewer.videos.play(tile);
    await Promise.resolve();
    expect(viewer.store.getState().audio).toBe('playing');

    // iOS in the background: both elements are paused, and a play() there is refused.
    setHidden(true);
    media.policy = 'block';
    tile.pause();
    viewer.audio.element?.pause();
    await Promise.resolve();
    await Promise.resolve();
    expect(viewer.store.getState().audio).toBe('blocked');
    media.played.length = 0;

    media.policy = 'allow';
    setHidden(false);
    await Promise.resolve();
    await Promise.resolve();
    expect(media.played).toContain(tile);
    expect(tile.paused).toBe(false);
    expect(viewer.store.getState().audio).toBe('playing');

    viewer.dispose();
    media.restore();
  });

  it('asks for a tap when the audio is still refused after the return (TapToStart, 05 §10.3)', async () => {
    installFakeMedia();
    const media = installFakeMediaElement();
    const viewer = createViewer();
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1)), SELF);
    viewer.registry.set('s_bea', 'audio', new FakeMediaStreamTrack('audio') as unknown as MediaStreamTrack);
    await Promise.resolve();

    setHidden(true);
    media.policy = 'block';
    viewer.audio.element?.pause();
    await Promise.resolve();
    await Promise.resolve();
    setHidden(false);
    await Promise.resolve();
    await Promise.resolve();
    expect(viewer.store.getState().audio).toBe('blocked');

    viewer.dispose();
    media.restore();
  });

  it('stops following when the viewer is disposed', () => {
    const viewer = createViewer();
    viewer.dispose();
    setHidden(true);
    expect(viewer.store.getState().pageHiddenSince).toBeNull();
  });
});
