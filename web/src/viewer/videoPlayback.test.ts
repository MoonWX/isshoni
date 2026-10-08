// VideoPlayback (05 §10.3): which tile videos the browser refused to play (iOS Low Power Mode blocks even muted
// autoplay), for TapToStart's "Tap to start video" and its retry.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { createVideoPlayback, isNotAllowed, type VideoPlayback } from './videoPlayback';
import { createViewerStore, type ViewerStore } from './viewerStore';

const settle = () => Promise.resolve().then(() => Promise.resolve());
const video = () => document.createElement('video');

let media: FakeMediaElementControl;
let store: ViewerStore;
let videos: VideoPlayback;

const blocked = () => store.getState().videoBlocked;

beforeEach(() => {
  media = installFakeMediaElement();
  store = createViewerStore();
  videos = createVideoPlayback({ store });
});

afterEach(() => {
  media.restore();
});

describe('createVideoPlayback', () => {
  it('plays each video it is given; nothing is blocked while the browser allows it', async () => {
    const a = video();
    const b = video();
    videos.play(a);
    videos.play(b);
    await settle();
    expect(media.played).toEqual([a, b]);
    expect(a.paused).toBe(false);
    expect(blocked()).toBe(false);
  });

  it('marks the page blocked when a video is refused, and retry() plays the refused ones again', async () => {
    const fine = video();
    videos.play(fine);
    media.policy = 'block';
    const a = video();
    const b = video();
    videos.play(a);
    videos.play(b);
    await settle();
    expect(blocked()).toBe(true);

    // Still refused: it stays blocked.
    videos.retry();
    await settle();
    expect(blocked()).toBe(true);

    media.policy = 'allow';
    const before = media.played.length;
    videos.retry();
    expect(media.played.slice(before)).toEqual([a, b]); // synchronously, and only the refused ones
    await settle();
    expect(blocked()).toBe(false);
    expect([a.paused, b.paused]).toEqual([false, false]);

    videos.retry(); // nothing left
    expect(media.played.length).toBe(before + 2);
  });

  it('stays blocked until the last refused video plays or is gone', async () => {
    media.policy = 'block';
    const a = video();
    const b = video();
    videos.play(a);
    videos.play(b);
    await settle();
    videos.forget(a); // its tile unmounted
    expect(blocked()).toBe(true);
    videos.forget(b);
    expect(blocked()).toBe(false);
    videos.forget(b); // harmless twice
  });

  it('clears the mark of a video whose new source plays', async () => {
    media.policy = 'block';
    const a = video();
    videos.play(a);
    await settle();
    expect(blocked()).toBe(true);
    media.policy = 'allow';
    videos.play(a); // a new track for the same element
    await settle();
    expect(blocked()).toBe(false);
  });

  it('ignores the refusal of a video that is gone by then', async () => {
    media.policy = 'block';
    const a = video();
    videos.play(a);
    videos.forget(a);
    await settle();
    expect(blocked()).toBe(false);
  });

  it('ignores a play() that ended for another reason (a newer source replaced it)', async () => {
    const a = video();
    a.play = () => Promise.reject(new DOMException('interrupted by a new load request', 'AbortError'));
    videos.play(a);
    await settle();
    expect(blocked()).toBe(false);
  });

  it('copes with a DOM whose play() returns nothing or throws', async () => {
    const silent = video();
    silent.play = (() => undefined) as unknown as HTMLVideoElement['play'];
    const broken = video();
    broken.play = () => {
      throw new Error('not implemented');
    };
    videos.play(silent);
    videos.play(broken);
    await settle();
    expect(blocked()).toBe(false);
  });

  it('resume() plays the videos that are paused: the page came back (05 §12.8)', async () => {
    const a = video();
    const b = video();
    videos.play(a);
    videos.play(b);
    await settle();
    a.pause(); // iOS suspended it in the background
    const before = media.played.length;
    videos.resume();
    expect(media.played.slice(before)).toEqual([a]);

    // What the browser refuses on the way back needs the tap.
    b.pause();
    media.policy = 'block';
    videos.resume();
    await settle();
    expect(blocked()).toBe(true);
  });
});

describe('isNotAllowed', () => {
  it('goes by the name, whatever realm the error comes from', () => {
    expect(isNotAllowed(new DOMException('needs a gesture', 'NotAllowedError'))).toBe(true);
    expect(isNotAllowed(Object.assign(new Error('x'), { name: 'NotAllowedError' }))).toBe(true);
    expect(isNotAllowed({ name: 'NotAllowedError' })).toBe(true);
    expect(isNotAllowed(new DOMException('aborted', 'AbortError'))).toBe(false);
    expect(isNotAllowed(new Error('NotAllowedError'))).toBe(false);
    expect(isNotAllowed('NotAllowedError')).toBe(false);
    expect(isNotAllowed(null)).toBe(false);
    expect(isNotAllowed(undefined)).toBe(false);
  });
});
