// audioOut with a fake media element (05 §19.1): one <audio> element per page, and the unlock state machine of
// 05 §10.3: locked → blocked on NotAllowedError → playing after the tap; a swap keeps playing; mute and unmute; an
// interruption that the browser doesn't let it recover from.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { AUTO_RESUME_SPACING_MS, canSetVolume, createAudioOut, type AudioOut } from './audioOut';
import { createViewerStore, type ViewerStore } from './viewerStore';

const track = () => new FakeMediaStreamTrack('audio') as unknown as MediaStreamTrack;
/** Lets the promise of a play() settle. */
const settle = () => Promise.resolve().then(() => Promise.resolve());

let media: FakeMediaElementControl;
let store: ViewerStore;
let out: AudioOut;

const state = () => store.getState().audio;
const elements = () => [...document.querySelectorAll('audio')];
const tracksOf = (el: HTMLAudioElement | null) => (el?.srcObject as unknown as FakeMediaStream | null)?.getTracks();

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  store = createViewerStore();
  out = createAudioOut({ store });
});

afterEach(() => {
  out.dispose();
  media.restore();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('audioOut: the one element', () => {
  it('has no element until there is something to play, then one in document.body for good', async () => {
    expect(state()).toBe('locked');
    expect(out.element).toBeNull();
    expect(elements()).toHaveLength(0);

    const a = track();
    out.setTrack(a);
    await settle();
    const el = out.element;
    expect(elements()).toEqual([el]);
    expect(el?.parentElement).toBe(document.body);
    expect(el).toHaveAttribute('data-isshoni-audio');
    expect(el).not.toHaveAttribute('controls');
    expect(tracksOf(el)).toEqual([a]);

    out.setTrack(null);
    out.setTrack(track());
    await settle();
    expect(elements()).toEqual([el]);
  });

  it('goes where it is told', async () => {
    out.dispose();
    const parent = document.createElement('div');
    out = createAudioOut({ store, parent });
    out.setTrack(track());
    await settle();
    expect(out.element?.parentElement).toBe(parent);
  });

  it('dispose() removes the element and ends everything', async () => {
    out.setTrack(track());
    await settle();
    const el = out.element;
    out.dispose();
    expect(elements()).toHaveLength(0);
    expect(el?.srcObject).toBeNull();
    expect(out.element).toBeNull();
    out.setTrack(track());
    expect(elements()).toHaveLength(0);
  });

  it('stays silent, without failing, where there is no MediaStream (a DOM without media)', async () => {
    vi.stubGlobal('MediaStream', undefined);
    out.setTrack(track());
    await settle();
    expect(out.element).toBeNull();
    expect(state()).toBe('locked');
    await expect(out.unlock()).resolves.toBeUndefined();
  });
});

describe('audioOut: the unlock state machine (05 §10.3)', () => {
  it('locked → playing when the browser lets the assigned track play', async () => {
    out.setTrack(track());
    expect(state()).toBe('locked'); // until play() settles
    await settle();
    expect(state()).toBe('playing');
    expect(out.element?.paused).toBe(false);
    expect(out.element?.muted).toBe(false);
  });

  it('locked → blocked on NotAllowedError → playing after the tap', async () => {
    media.policy = 'block';
    out.setTrack(track());
    await settle();
    expect(state()).toBe('blocked');
    expect(out.element?.paused).toBe(true);

    // The tap: the browser allows play() inside a user gesture, and unlock() calls it synchronously.
    media.policy = 'allow';
    const before = media.played.length;
    const unlocked = out.unlock();
    expect(media.played.length).toBe(before + 1);
    expect(media.played.at(-1)).toBe(out.element);
    await unlocked;
    expect(state()).toBe('playing');
    expect(out.element?.paused).toBe(false);
  });

  it('stays blocked, and says so, when the browser still refuses the tap', async () => {
    media.policy = 'block';
    out.setTrack(track());
    await settle();
    await expect(out.unlock()).rejects.toMatchObject({ name: 'NotAllowedError' });
    expect(state()).toBe('blocked');
  });

  it('keeps playing through a swap of the track (audio follows focus)', async () => {
    const a = track();
    const b = track();
    out.setTrack(a);
    await settle();
    const el = out.element;
    const first = el?.srcObject;
    const states: string[] = [];
    const off = store.subscribe((s) => states.push(s.audio));

    out.setTrack(b);
    expect(state()).toBe('playing');
    await settle();
    off();
    expect(states).toEqual([]); // never left `playing`
    expect(out.element).toBe(el);
    expect(el?.srcObject).not.toBe(first);
    expect(tracksOf(el ?? null)).toEqual([b]);
    expect(el?.paused).toBe(false);
    expect(media.played.filter((p) => p === el)).toHaveLength(2);

    // The same track again is not a swap.
    out.setTrack(b);
    expect(media.played.filter((p) => p === el)).toHaveLength(2);
  });

  it('is blocked when the browser refuses a later track', async () => {
    out.setTrack(track());
    await settle();
    media.policy = 'block';
    out.setTrack(track());
    await settle();
    expect(state()).toBe('blocked');
  });

  it('ignores the result of a play() that a newer track replaced', async () => {
    media.policy = 'block';
    out.setTrack(track()); // this play() is refused …
    media.policy = 'allow';
    out.setTrack(track()); // … but this one decides
    await settle();
    expect(state()).toBe('playing');
  });

  it('goes silent without a track: nothing is left to unmute', async () => {
    media.policy = 'block';
    out.setTrack(track());
    await settle();
    expect(state()).toBe('blocked');
    out.setTrack(null);
    expect(state()).toBe('locked');
    expect(out.element?.srcObject).toBeNull();
    await expect(out.unlock()).resolves.toBeUndefined();
    expect(state()).toBe('locked');

    // While it plays, silence keeps the unlock.
    media.policy = 'allow';
    out.setTrack(track());
    await settle();
    out.setTrack(null);
    expect(state()).toBe('playing');
  });

  it('playing ⇄ muted by the user; the muted element keeps playing', async () => {
    out.setTrack(track());
    await settle();
    out.setMuted(true);
    expect(state()).toBe('muted');
    expect(out.element?.muted).toBe(true);
    expect(out.element?.paused).toBe(false);

    // A swap while muted stays muted.
    out.setTrack(track());
    await settle();
    expect(state()).toBe('muted');

    const plays = media.played.length;
    out.setMuted(false);
    expect(state()).toBe('playing');
    expect(out.element?.muted).toBe(false);
    expect(media.played.length).toBe(plays); // it never stopped
  });

  it('lets the user mute instead of tapping, and starts on unmute (a user gesture)', async () => {
    media.policy = 'block';
    out.setTrack(track());
    await settle();
    expect(state()).toBe('blocked');
    out.setMuted(true);
    expect(state()).toBe('muted');

    media.policy = 'allow';
    out.setMuted(false);
    await settle();
    expect(state()).toBe('playing');
    expect(out.element?.paused).toBe(false);
  });

  it('is blocked when unmuting can’t start it either', async () => {
    media.policy = 'block';
    out.setMuted(true);
    out.setTrack(track());
    await settle();
    expect(state()).toBe('muted'); // refused, but nobody hears a muted element anyway
    out.setMuted(false);
    expect(state()).toBe('locked');
    await settle();
    expect(state()).toBe('blocked');
  });

  it('the tap leaves a user’s mute alone', async () => {
    out.setTrack(track());
    await settle();
    out.setMuted(true);
    await out.unlock(); // "Tap to start video" runs the same handler
    expect(state()).toBe('muted');
    expect(out.element?.muted).toBe(true);
  });

  it('takes its first state from the store: a muted page stays muted', async () => {
    out.dispose();
    store.getState().setAudio('muted');
    out = createAudioOut({ store });
    out.setTrack(track());
    await settle();
    expect(out.element?.muted).toBe(true);
    expect(state()).toBe('muted');
  });
});

describe('audioOut: interruptions (05 §10.3, §12.8)', () => {
  it('plays again by itself when the browser paused it and lets it', async () => {
    out.setTrack(track());
    await settle();
    out.element?.pause(); // the browser, not this module
    await settle();
    expect(out.element?.paused).toBe(false);
    expect(state()).toBe('playing');
  });

  it('playing → blocked when a later play() is rejected (an iOS interruption)', async () => {
    out.setTrack(track());
    await settle();
    media.policy = 'block';
    out.element?.pause();
    await settle();
    expect(state()).toBe('blocked');

    media.policy = 'allow';
    await out.unlock();
    expect(state()).toBe('playing');
  });

  it('doesn’t fight something that keeps pausing it: the second pause within a second waits for a tap', async () => {
    vi.useFakeTimers();
    out.setTrack(track());
    await settle();
    out.element?.pause();
    await settle();
    expect(state()).toBe('playing');
    const plays = media.played.length;

    out.element?.pause();
    await settle();
    expect(media.played.length).toBe(plays);
    expect(state()).toBe('blocked');

    await out.unlock();
    vi.advanceTimersByTime(AUTO_RESUME_SPACING_MS);
    out.element?.pause();
    await settle();
    expect(media.played.length).toBe(plays + 2);
    expect(state()).toBe('playing');
  });

  it('resume() plays a paused element again and does nothing while it plays', async () => {
    out.resume(); // nothing to resume yet
    expect(out.element).toBeNull();

    media.policy = 'block';
    out.setTrack(track());
    await settle();
    const plays = media.played.length;
    media.policy = 'allow';
    out.resume();
    await settle();
    expect(media.played.length).toBe(plays + 1);
    expect(state()).toBe('playing');

    out.resume();
    expect(media.played.length).toBe(plays + 1);
  });

  it('a pause without a track is not an interruption', async () => {
    out.setTrack(track());
    await settle();
    out.setTrack(null);
    const plays = media.played.length;
    out.element?.pause();
    await settle();
    expect(media.played.length).toBe(plays);
  });
});

describe('audioOut: volume (05 §10.3)', () => {
  it('sets the element’s volume and returns what the element reports back', async () => {
    expect(out.setVolume(0.4)).toBe(0.4); // remembered until there is an element
    out.setTrack(track());
    await settle();
    expect(out.element?.volume).toBe(0.4);
    expect(out.setVolume(0.75)).toBe(0.75);
    expect(out.element?.volume).toBe(0.75);
    expect(out.setVolume(7)).toBe(1);
    expect(out.setVolume(-1)).toBe(0);
    expect(out.setVolume(Number.NaN)).toBe(1);
  });

  it('starts with the store’s volume', async () => {
    out.dispose();
    store = createViewerStore({ volume: 0.25 });
    out = createAudioOut({ store });
    out.setTrack(track());
    await settle();
    expect(out.element?.volume).toBe(0.25);
  });

  it('reports the volume a browser forces (iOS always reads 1)', async () => {
    out.setTrack(track());
    await settle();
    const el = out.element;
    if (!el) throw new Error('no element');
    Object.defineProperty(el, 'volume', { configurable: true, get: () => 1, set: () => undefined });
    expect(out.setVolume(0.3)).toBe(1);
  });

  it('canSetVolume() tells whether a page can set an element’s volume at all', () => {
    expect(canSetVolume()).toBe(true);
    const fixed = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'volume');
    Object.defineProperty(HTMLMediaElement.prototype, 'volume', {
      configurable: true,
      get: () => 1,
      set: () => undefined,
    });
    try {
      expect(canSetVolume()).toBe(false);
    } finally {
      if (fixed) Object.defineProperty(HTMLMediaElement.prototype, 'volume', fixed);
    }
  });
});
