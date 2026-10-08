import { describe, expect, it, vi, type Mock } from 'vitest';

import type { PickedSource } from '../platform/types';
import {
  createShareStore,
  isSharingPhase,
  ShareCancelledError,
  shareStore,
  type ShareFields,
  type ShareStore,
  type StartShare,
} from './shareStore';
import { deferred, fakePick } from './testing/fakeCapture';
import { shareParams } from './testing/publish';

/** The machine's state without the host and the panels, which are about the page and have their own tests. */
type Fields = Omit<ShareFields, 'hostId' | 'panels'>;

const fields = (store: ShareStore): Fields => {
  const { phase, preset, withAudio, picked, soundOn, params, hint, unreachable, error, notice } = store.getState();
  return { phase, preset, withAudio, picked, soundOn, params, hint, unreachable, error, notice };
};

const IDLE: Fields = {
  phase: 'idle',
  preset: 'auto',
  withAudio: false,
  picked: null,
  soundOn: true,
  params: null,
  hint: null,
  unreachable: false,
  error: null,
  notice: null,
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
    // The publisher moves it on from here.
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

describe('shareStore: hosts (the mounted Share buttons)', () => {
  it('names the host attached longest; the next in line takes over; null when none is left', () => {
    const store = createShareStore();
    expect(store.getState().hostId).toBeNull();
    const detachA = store.getState().attach('a');
    const detachB = store.getState().attach('b');
    const detachC = store.getState().attach('c');
    expect(store.getState().hostId).toBe('a');
    detachB();
    expect(store.getState().hostId).toBe('a');
    detachA();
    expect(store.getState().hostId).toBe('c');
    detachC();
    expect(store.getState().hostId).toBeNull();
    store.getState().attach('d');
    expect(store.getState().hostId).toBe('d');
  });

  it('a detach counts once, and removes only its own host', () => {
    const store = createShareStore();
    const detachA = store.getState().attach('a');
    store.getState().attach('b');
    detachA();
    detachA();
    expect(store.getState().hostId).toBe('b');
  });

  it('the host stays through a pick, a cancel and a failed start', async () => {
    const store = createShareStore();
    store.getState().attach('a');
    await store.getState().pick(Promise.resolve(fakePick('monitor', true)), { preset: 'auto', start: started() });
    expect(store.getState()).toMatchObject({ phase: 'confirming', hostId: 'a' });
    store.getState().cancel();
    expect(store.getState()).toMatchObject({ phase: 'idle', hostId: 'a' });
    await store.getState().pick(Promise.resolve(fakePick('window', true)), {
      preset: 'auto',
      start: () => Promise.reject(new Error('nope')),
    });
    expect(store.getState()).toMatchObject({ phase: 'failed', hostId: 'a' });
    store.getState().dismiss();
    expect(store.getState()).toMatchObject({ phase: 'idle', hostId: 'a' });
  });

  it('a host detaching while the picker is open leaves the pick alone as long as another is attached', async () => {
    const store = createShareStore();
    const start = started();
    store.getState().attach('header');
    const detachEmptyState = store.getState().attach('empty-state');
    const picking = deferred<PickedSource | null>();
    const flow = store.getState().pick(picking.promise, { preset: 'auto', start });
    detachEmptyState();
    expect(store.getState()).toMatchObject({ phase: 'picking', hostId: 'header' });
    const src = fakePick('window', true);
    picking.resolve(src);
    await expect(flow).resolves.toMatchObject({ step: 'started' });
    expect(start).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: true });
    expect(src.release).not.toHaveBeenCalled();
  });

  it('a host detaching under the warning leaves it up as long as another is attached', async () => {
    const store = createShareStore();
    const start = started();
    const detachHeader = store.getState().attach('header');
    store.getState().attach('empty-state');
    const src = fakePick('monitor', true);
    await store.getState().pick(Promise.resolve(src), { preset: 'auto', start });
    detachHeader();
    expect(store.getState()).toMatchObject({ phase: 'confirming', hostId: 'empty-state' });
    expect(src.release).not.toHaveBeenCalled();
    await expect(store.getState().confirm(false)).resolves.toMatchObject({ step: 'started', withAudio: false });
    expect(start).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
  });

  it('the last host detaching while the picker is open gives the pick up: what arrives later is released', async () => {
    const store = createShareStore();
    const start = started();
    const detachA = store.getState().attach('a');
    const detachB = store.getState().attach('b');
    const picking = deferred<PickedSource | null>();
    const flow = store.getState().pick(picking.promise, { preset: 'auto', start });
    detachA();
    expect(store.getState().phase).toBe('picking');
    detachB();
    expect(store.getState()).toMatchObject({ phase: 'idle', hostId: null });
    const src = fakePick('window', true);
    picking.resolve(src);
    await expect(flow).resolves.toEqual({ step: 'cancelled' });
    expect(src.release).toHaveBeenCalledOnce();
    expect(start).not.toHaveBeenCalled();
  });

  it('the last host detaching under the warning releases the capture', async () => {
    const store = createShareStore();
    const detach = store.getState().attach('a');
    const src = fakePick('monitor', true);
    await store.getState().pick(Promise.resolve(src), { preset: 'auto', start: started() });
    detach();
    expect(store.getState()).toMatchObject({ phase: 'idle', hostId: null });
    expect(src.release).toHaveBeenCalledOnce();
  });

  it.each(['starting', 'live', 'reconnecting', 'stopping', 'failed'] as const)(
    'the last host detaching while %s leaves the share to the publisher',
    (phase) => {
      const store = createShareStore();
      const detach = store.getState().attach('a');
      store.setState({ phase });
      detach();
      expect(store.getState()).toMatchObject({ phase, hostId: null });
    },
  );
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

describe('shareStore: the publisher’s half (05 §13.1)', () => {
  const PARAMS = shareParams('s_1');
  const WINDOW = { kind: 'window', audioScope: 'window', warning: null } as const;
  const publishing = (store: ShareStore, withAudio = true): void => {
    store.getState().publishing({ picked: WINDOW, preset: 'movie', withAudio, params: PARAMS });
  };

  it('isSharingPhase: a share is under way from starting to stopping', () => {
    expect(
      (['idle', 'picking', 'confirming', 'starting', 'live', 'reconnecting', 'stopping', 'failed'] as const).filter(
        isSharingPhase,
      ),
    ).toEqual(['starting', 'live', 'reconnecting', 'stopping']);
  });

  it('publishing fills in the share that a pick started', async () => {
    const store = createShareStore();
    const start = vi.fn<StartShare>(() => {
      publishing(store);
      return Promise.resolve();
    });
    await store.getState().pick(Promise.resolve(fakePick('window', true)), { preset: 'movie', start });
    expect(fields(store)).toEqual({
      ...IDLE,
      phase: 'starting',
      preset: 'movie',
      withAudio: true,
      picked: WINDOW,
      params: PARAMS,
    });
  });

  it('publishing without a pick through the store goes to starting, and gives up what was being picked here', async () => {
    const store = createShareStore();
    const waiting = fakePick('monitor', true);
    await store.getState().pick(Promise.resolve(waiting), { preset: 'auto', start: started() });
    expect(store.getState().phase).toBe('confirming');
    publishing(store, false);
    expect(waiting.release).toHaveBeenCalledOnce();
    expect(fields(store)).toEqual({
      ...IDLE,
      phase: 'starting',
      preset: 'movie',
      withAudio: false,
      soundOn: false,
      picked: WINDOW,
      params: PARAMS,
    });
    // The warning's buttons have nothing to confirm any more.
    await expect(store.getState().confirm(true)).resolves.toEqual({ step: 'cancelled' });
  });

  it('publishing does not take over a share that is already further along', () => {
    const store = createShareStore();
    publishing(store);
    store.getState().advance('live');
    expect(
      store.getState().publishing({ picked: WINDOW, preset: 'text', withAudio: false, params: shareParams('s_2') }),
    ).toBe(false);
    expect(store.getState()).toMatchObject({ phase: 'live', preset: 'movie', params: PARAMS });
    // Once that share is over the machine is free again.
    store.getState().finish();
    expect(
      store.getState().publishing({ picked: WINDOW, preset: 'text', withAudio: false, params: shareParams('s_2') }),
    ).toBe(true);
  });

  it('advance: starting → live ⇄ reconnecting → stopping, and nothing leaves stopping', () => {
    const store = createShareStore();
    publishing(store);
    store.getState().advance('reconnecting'); // only a share that was live can be reconnecting
    expect(store.getState().phase).toBe('starting');
    store.getState().advance('live');
    expect(store.getState().phase).toBe('live');
    store.getState().advance('reconnecting');
    expect(store.getState().phase).toBe('reconnecting');
    store.getState().advance('live');
    expect(store.getState().phase).toBe('live');
    store.getState().advance('stopping');
    store.getState().advance('live');
    store.getState().advance('reconnecting');
    expect(store.getState().phase).toBe('stopping');
  });

  it.each(['idle', 'picking', 'confirming', 'failed'] as const)(
    'advance, report and finish do nothing while no share is under way (%s)',
    (phase) => {
      const store = createShareStore();
      store.setState({ phase });
      const before = fields(store);
      store.getState().advance('live');
      store.getState().report({ hint: { kind: 'cpu-limited' }, soundOn: false });
      store.getState().finish({ error: new Error('late') });
      expect(fields(store)).toEqual(before);
    },
  );

  it('report: what changes while the share runs', () => {
    const store = createShareStore();
    publishing(store);
    store.getState().advance('live');
    const next = shareParams('s_1', { audioBitrate: 256_000 });
    store.getState().report({ params: next, preset: 'game', hint: { kind: 'upload-limited', approxHeight: 720 } });
    store.getState().report({ soundOn: false, unreachable: true });
    expect(store.getState()).toMatchObject({
      phase: 'live',
      params: next,
      preset: 'game',
      hint: { kind: 'upload-limited', approxHeight: 720 },
      soundOn: false,
      unreachable: true,
    });
  });

  it('finish(): → idle, keeping the preset for the next share', () => {
    const store = createShareStore();
    publishing(store);
    store.getState().advance('live');
    store.getState().report({ hint: { kind: 'cpu-limited' }, unreachable: true, soundOn: false });
    store.getState().finish();
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie' });
  });

  it('finish({error}): → failed with the error and what was shared; dismiss → idle', () => {
    const store = createShareStore();
    publishing(store);
    const error = new Error('no video reached the server');
    store.getState().finish({ error });
    expect(fields(store)).toEqual({ ...IDLE, phase: 'failed', preset: 'movie', picked: WINDOW, error });
    store.getState().dismiss();
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie' });
  });

  it('finish({notice}): → idle with the notice, a new object each time; the next pick clears it', async () => {
    const store = createShareStore();
    publishing(store);
    store.getState().finish({ notice: 'elsewhere' });
    const first = store.getState().notice;
    expect(fields(store)).toEqual({ ...IDLE, preset: 'movie', notice: { kind: 'elsewhere' } });
    publishing(store);
    expect(store.getState().notice).toBe(first);
    store.getState().finish({ notice: 'elsewhere' });
    expect(store.getState().notice).toEqual({ kind: 'elsewhere' });
    expect(store.getState().notice).not.toBe(first);

    await store.getState().pick(Promise.resolve(null), { preset: 'auto', start: started() });
    expect(store.getState().notice).toBeNull();
  });

  it('a start that rejects after the publisher already reported the failure keeps the publisher’s word', async () => {
    const store = createShareStore();
    const failure = new Error('the tracks could not be offered');
    const src = fakePick('window', true);
    const start = vi.fn<StartShare>(() => {
      publishing(store);
      store.getState().finish({ error: failure });
      return Promise.reject(failure);
    });
    await expect(store.getState().pick(Promise.resolve(src), { preset: 'auto', start })).resolves.toEqual({
      step: 'failed',
      error: failure,
    });
    expect(store.getState()).toMatchObject({ phase: 'failed', error: failure });
    expect(src.release).toHaveBeenCalledOnce();
  });

  it('a start that was given up (ShareCancelledError) goes back to idle without a failure', async () => {
    const store = createShareStore();
    const src = fakePick('window', true);
    const start = vi.fn<StartShare>(() => Promise.reject(new ShareCancelledError()));
    await expect(store.getState().pick(Promise.resolve(src), { preset: 'game', start })).resolves.toEqual({
      step: 'cancelled',
    });
    expect(fields(store)).toEqual({ ...IDLE, preset: 'game' });
    expect(src.release).toHaveBeenCalledOnce();
  });

  it('a share that ended while its start was still running is not put back to failed by a late rejection', async () => {
    const store = createShareStore();
    const start = vi.fn<StartShare>(() => {
      publishing(store);
      store.getState().finish(); // "Stop sharing" in the browser's bar, before start() settled
      return Promise.reject(new Error('capture_failed'));
    });
    await store.getState().pick(Promise.resolve(fakePick('window', true)), { preset: 'auto', start });
    expect(store.getState().phase).toBe('idle');
  });
});

describe('shareStore: panels', () => {
  it('counts the mounted SharePanels; a detach counts once', () => {
    const store = createShareStore();
    expect(store.getState().panels).toBe(0);
    const first = store.getState().attachPanel();
    const second = store.getState().attachPanel();
    expect(store.getState().panels).toBe(2);
    first();
    first();
    expect(store.getState().panels).toBe(1);
    second();
    expect(store.getState().panels).toBe(0);
  });

  it('survives a share coming and going', () => {
    const store = createShareStore();
    store.getState().attachPanel();
    store.getState().publishing({
      picked: { kind: 'tab', audioScope: 'tab', warning: null },
      preset: 'auto',
      withAudio: true,
      params: shareParams('s_1'),
    });
    store.getState().finish();
    expect(store.getState().panels).toBe(1);
  });
});
