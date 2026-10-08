import { describe, expect, it, vi, type Mock } from 'vitest';

import type { PickedSource } from '../platform/types';
import { createShareStore, shareStore, type ShareFields, type ShareStore, type StartShare } from './shareStore';
import { deferred, fakePick } from './testing/fakeCapture';

/** The machine's state without the pick counter, which has its own tests. */
type Fields = Omit<ShareFields, 'pickId'>;

const fields = (store: ShareStore): Fields => {
  const { phase, preset, withAudio, picked, params, hint, error } = store.getState();
  return { phase, preset, withAudio, picked, params, hint, error };
};

const IDLE: Fields = {
  phase: 'idle',
  preset: 'auto',
  withAudio: false,
  picked: null,
  params: null,
  hint: null,
  error: null,
};

const started = (): Mock<StartShare> => vi.fn<StartShare>(() => Promise.resolve());

describe('shareStore: picking (05 §13.1)', () => {
  it('starts idle', () => {
    expect(fields(createShareStore())).toEqual(IDLE);
    expect(fields(shareStore)).toEqual(IDLE);
  });

  it('idle → picking at once, with the preset of the click', () => {
    const store = createShareStore();
    const picking = deferred<PickedSource | null>();
    void store.getState().pick(picking.promise, { preset: 'movie', start: started() });
    expect(fields(store)).toEqual({ ...IDLE, phase: 'picking', preset: 'movie' });
  });

  it('picking → idle when the picker is cancelled', async () => {
    const store = createShareStore();
    const start = started();
    const outcome = await store.getState().pick(Promise.resolve(null), { preset: 'game', start });
    expect(outcome).toEqual({ step: 'cancelled' });
    expect(fields(store)).toEqual({ ...IDLE, preset: 'game' });
    expect(start).not.toHaveBeenCalled();
  });

  it('picking → idle when the capture fails, and the error goes to the caller', async () => {
    const store = createShareStore();
    const error = new Error('capture failed');
    const outcome = await store.getState().pick(Promise.reject(error), { preset: 'auto', start: started() });
    expect(outcome).toEqual({ step: 'failed', error });
    expect(fields(store)).toEqual(IDLE);
  });

  it.each([
    ['window', true, true],
    ['window', false, false],
    ['browser', true, true],
    ['browser', false, false],
    ['monitor', false, false],
  ] as const)('a %s pick (audio %s) goes straight to starting', async (surface, audio, withAudio) => {
    const store = createShareStore();
    const src = fakePick(surface, audio);
    const starting = deferred<undefined>();
    const start = vi.fn<StartShare>(() => starting.promise);
    const flow = store.getState().pick(Promise.resolve(src), { preset: 'text', start });
    await vi.waitFor(() => {
      expect(store.getState().phase).toBe('starting');
    });
    const picked = { kind: src.kind, audioScope: src.audioScope, warning: src.warning };
    expect(fields(store)).toEqual({ ...IDLE, phase: 'starting', preset: 'text', picked, withAudio });
    expect(start).toHaveBeenCalledExactlyOnceWith(src, { preset: 'text', withAudio });

    starting.resolve(undefined);
    await expect(flow).resolves.toEqual({ step: 'started', picked, withAudio });
    // The publisher moves it on from here (S46).
    expect(store.getState().phase).toBe('starting');
    expect(src.release).not.toHaveBeenCalled();
  });

  it('starting → failed when start rejects; the source is released', async () => {
    const store = createShareStore();
    const src = fakePick('window', true);
    const error = new Error('share.start failed');
    const outcome = await store.getState().pick(Promise.resolve(src), {
      preset: 'auto',
      start: () => Promise.reject(error),
    });
    expect(outcome).toEqual({ step: 'failed', error });
    expect(fields(store)).toMatchObject({ phase: 'failed', error, picked: { kind: 'window' } });
    expect(src.release).toHaveBeenCalledOnce();
  });

  it('failed → idle on dismiss, and a new pick may start from failed', async () => {
    const store = createShareStore();
    const failing = { preset: 'movie', start: () => Promise.reject(new Error('nope')) } as const;
    await store.getState().pick(Promise.resolve(fakePick('window', true)), failing);
    expect(store.getState().phase).toBe('failed');
    store.getState().dismiss();
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie' });

    await store.getState().pick(Promise.resolve(fakePick('window', true)), failing);
    expect(store.getState().phase).toBe('failed');
    const picking = deferred<PickedSource | null>();
    void store.getState().pick(picking.promise, { preset: 'auto', start: started() });
    expect(fields(store)).toEqual({ ...IDLE, phase: 'picking' });
  });

  it('dismiss does nothing outside failed', () => {
    const store = createShareStore();
    store.setState({ phase: 'live' });
    store.getState().dismiss();
    expect(store.getState().phase).toBe('live');
  });

  it('a failed start does not undo what the publisher did meanwhile', async () => {
    const store = createShareStore();
    const starting = deferred<undefined>();
    const flow = store.getState().pick(Promise.resolve(fakePick('window', true)), {
      preset: 'auto',
      start: () => starting.promise,
    });
    await vi.waitFor(() => {
      expect(store.getState().phase).toBe('starting');
    });
    store.setState({ phase: 'idle' });
    starting.reject(new Error('late'));
    await expect(flow).resolves.toMatchObject({ step: 'failed' });
    expect(store.getState().phase).toBe('idle');
  });
});

describe('shareStore: the whole-screen warning (05 §13.3)', () => {
  async function confirming(start: StartShare = started()) {
    const store = createShareStore();
    const src = fakePick('monitor', true);
    const outcome = await store.getState().pick(Promise.resolve(src), { preset: 'movie', start });
    expect(outcome).toEqual({ step: 'confirming' });
    return { store, src, start };
  }

  it('a whole screen with system audio waits in confirming; nothing starts', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    expect(fields(store)).toEqual({
      ...IDLE,
      phase: 'confirming',
      preset: 'movie',
      withAudio: true,
      picked: { kind: 'screen', audioScope: 'system', warning: 'screen-with-system-audio' },
    });
    expect(start).not.toHaveBeenCalled();
    expect(src.stream.active).toBe(true);
  });

  it('"Share without sound": the audio track is stopped and removed at the click, then it starts', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    const [audio] = src.stream.getAudioTracks();
    const flow = store.getState().confirm(false);
    // Before start resolves: the sound is already gone.
    expect(audio?.readyState).toBe('ended');
    expect(src.stream.getAudioTracks()).toEqual([]);
    expect(src.stream.getVideoTracks()[0]?.readyState).toBe('live');
    expect(store.getState()).toMatchObject({ phase: 'starting', withAudio: false });
    expect(start).toHaveBeenCalledExactlyOnceWith(src, { preset: 'movie', withAudio: false });
    await expect(flow).resolves.toMatchObject({ step: 'started', withAudio: false });
    expect(src.release).not.toHaveBeenCalled();
  });

  it('"Share with sound anyway": starts with the audio track', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    await expect(store.getState().confirm(true)).resolves.toMatchObject({ step: 'started', withAudio: true });
    expect(src.stream.getAudioTracks()).toHaveLength(1);
    expect(src.stream.getAudioTracks()[0]?.readyState).toBe('live');
    expect(start).toHaveBeenCalledExactlyOnceWith(src, { preset: 'movie', withAudio: true });
    expect(store.getState()).toMatchObject({ phase: 'starting', withAudio: true });
  });

  it('"Pick something else": releases the stream and follows the new pick', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    const next = deferred<PickedSource | null>();
    const flow = store.getState().pick(next.promise, { preset: 'movie', start });
    expect(src.release).toHaveBeenCalledOnce();
    expect(fields(store)).toEqual({ ...IDLE, phase: 'picking', preset: 'movie' });

    const window = fakePick('window', true);
    next.resolve(window);
    await expect(flow).resolves.toMatchObject({ step: 'started', picked: { kind: 'window' } });
    expect(start).toHaveBeenCalledExactlyOnceWith(window, { preset: 'movie', withAudio: true });
  });

  it('picking a whole screen with sound again shows the warning again', async () => {
    const { store, src, start } = await confirming();
    const again = fakePick('monitor', true);
    await expect(store.getState().pick(Promise.resolve(again), { preset: 'movie', start })).resolves.toEqual({
      step: 'confirming',
    });
    expect(src.release).toHaveBeenCalledOnce();
    expect(again.release).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('confirming');
  });

  it('cancel: confirming → idle, the stream is released, and confirm no longer starts anything', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    store.getState().cancel();
    expect(src.release).toHaveBeenCalledOnce();
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie' });
    await expect(store.getState().confirm(true)).resolves.toEqual({ step: 'cancelled' });
    expect(start).not.toHaveBeenCalled();
  });

  it('confirm outside confirming does nothing', async () => {
    const store = createShareStore();
    await expect(store.getState().confirm(false)).resolves.toEqual({ step: 'cancelled' });
    expect(fields(store)).toEqual(IDLE);
  });

  it('the capture ending under the warning (the browser\'s "Stop sharing") cancels the flow', async () => {
    const start = started();
    const { store, src } = await confirming(start);
    src.stream.getVideoTracks()[0]?.end();
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie' });
    expect(src.release).toHaveBeenCalledOnce();
    await expect(store.getState().confirm(true)).resolves.toEqual({ step: 'cancelled' });
    expect(start).not.toHaveBeenCalled();
  });

  it("stops watching the capture once the share starts: its end is then the publisher's business", async () => {
    const { store, src } = await confirming();
    await store.getState().confirm(true);
    src.stream.getVideoTracks()[0]?.end();
    expect(store.getState().phase).toBe('starting');
    expect(src.release).not.toHaveBeenCalled();
  });

  it('stops watching a capture that was given up', async () => {
    const { store, src, start } = await confirming();
    const next = fakePick('monitor', true);
    await store.getState().pick(Promise.resolve(next), { preset: 'movie', start });
    // The first capture was released by "Pick something else"; a late `ended` from it must not cancel the second.
    src.stream.getVideoTracks()[0]?.end();
    expect(store.getState().phase).toBe('confirming');
    expect(next.release).not.toHaveBeenCalled();
  });
});

describe('shareStore: pickId', () => {
  it('counts the picks it follows, so a component can tell its own pick from a newer one', async () => {
    const store = createShareStore();
    expect(store.getState().pickId).toBe(0);
    const first = store.getState().pick(Promise.resolve(null), { preset: 'auto', start: started() });
    expect(store.getState().pickId).toBe(1);
    await first;
    expect(store.getState().pickId).toBe(1);
    await store.getState().pick(Promise.resolve(fakePick('monitor', true)), { preset: 'auto', start: started() });
    expect(store.getState()).toMatchObject({ pickId: 2, phase: 'confirming' });
    store.getState().cancel();
    expect(store.getState().pickId).toBe(2);
  });

  it('does not count a pick it refuses', async () => {
    const store = createShareStore();
    store.setState({ phase: 'live' });
    await store.getState().pick(Promise.resolve(null), { preset: 'auto', start: started() });
    expect(store.getState().pickId).toBe(0);
  });
});

describe('shareStore: picks that arrive late', () => {
  it('cancel while the picker is open: the source it returns later is released, nothing starts', async () => {
    const store = createShareStore();
    const start = started();
    const picking = deferred<PickedSource | null>();
    const flow = store.getState().pick(picking.promise, { preset: 'auto', start });
    store.getState().cancel();
    expect(store.getState().phase).toBe('idle');
    const src = fakePick('monitor', true);
    picking.resolve(src);
    await expect(flow).resolves.toEqual({ step: 'cancelled' });
    expect(src.release).toHaveBeenCalledOnce();
    expect(start).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('idle');
  });

  it('a capture error after a cancel stays quiet', async () => {
    const store = createShareStore();
    const picking = deferred<PickedSource | null>();
    const flow = store.getState().pick(picking.promise, { preset: 'auto', start: started() });
    store.getState().cancel();
    picking.reject(new Error('late failure'));
    await expect(flow).resolves.toEqual({ step: 'cancelled' });
  });

  it('a newer pick wins: the older one is released when it arrives', async () => {
    const store = createShareStore();
    const start = started();
    const first = deferred<PickedSource | null>();
    const second = deferred<PickedSource | null>();
    const firstFlow = store.getState().pick(first.promise, { preset: 'auto', start });
    const secondFlow = store.getState().pick(second.promise, { preset: 'game', start });
    const old = fakePick('window', true);
    first.resolve(old);
    await expect(firstFlow).resolves.toEqual({ step: 'cancelled' });
    expect(old.release).toHaveBeenCalledOnce();
    expect(store.getState()).toMatchObject({ phase: 'picking', preset: 'game' });
    const fresh = fakePick('browser', true);
    second.resolve(fresh);
    await expect(secondFlow).resolves.toMatchObject({ step: 'started' });
    expect(start).toHaveBeenCalledExactlyOnceWith(fresh, { preset: 'game', withAudio: true });
  });

  it.each(['starting', 'live', 'reconnecting', 'stopping'] as const)(
    'while %s, a pick is not wanted: its source is released and the share is left alone',
    async (phase) => {
      const store = createShareStore();
      store.setState({ phase, preset: 'movie' });
      const start = started();
      const src = fakePick('window', true);
      await expect(store.getState().pick(Promise.resolve(src), { preset: 'auto', start })).resolves.toEqual({
        step: 'cancelled',
      });
      await vi.waitFor(() => {
        expect(src.release).toHaveBeenCalledOnce();
      });
      expect(store.getState()).toMatchObject({ phase, preset: 'movie' });
      expect(start).not.toHaveBeenCalled();
      // cancel is for the pick, never for a share that is under way.
      store.getState().cancel();
      expect(store.getState().phase).toBe(phase);
    },
  );
});
