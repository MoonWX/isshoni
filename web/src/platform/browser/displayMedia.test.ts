// createBrowserSharing (05 §8, §17.3): the provider BrowserPlatform hands out opens the picker itself and loads the
// publisher, share/'s BrowserSharing, when it is first needed. The picker's own tests (the getDisplayMedia options,
// its fallbacks, the fake display) are in share/displayMedia.test.ts; that the publisher is a chunk of its own in a
// build is app/chunks.node.test.ts's.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { LocalError } from '../../lib/errors';
import { clearLog, logLines } from '../../lib/log';
import { deferred, fakePick } from '../../share/testing/fakeCapture';
import { FakeRTCPeerConnection } from '../../test/FakeRTCPeerConnection';
import type { ActiveShare, PickedSource, ShareContext } from '../types';
import { createBrowserSharing, type Publisher } from './displayMedia';

const ctx = { signal: {}, roomId: 'lounge' } as unknown as ShareContext;
const startOpts = { preset: 'auto', withAudio: true } as const;

/** A publisher whose start() resolves with a share named after the call. */
function fakePublisher() {
  const start = vi.fn<Publisher['start']>((src) => Promise.resolve({ shareId: `s_${src.kind}` } as ActiveShare));
  return { start } satisfies Publisher;
}

/** Lets pending promise callbacks run. */
const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe('createBrowserSharing', () => {
  it('is the in-page provider, and loads nothing by being made', () => {
    const capture = vi.fn();
    const load = vi.fn();
    const sharing = createBrowserSharing({ capture, load });
    expect(sharing.mode).toBe('in-page');
    expect(capture).not.toHaveBeenCalled();
    expect(load).not.toHaveBeenCalled();
  });

  it('pick() opens the picker first, in the same turn, and then asks for the publisher', () => {
    const order: string[] = [];
    const capture = vi.fn(() => {
      order.push('capture');
      return new Promise<PickedSource | null>(() => undefined);
    });
    const load = vi.fn(() => {
      order.push('load');
      return new Promise<Publisher>(() => undefined);
    });
    const sharing = createBrowserSharing({ capture, load });
    void sharing.pick({ preset: 'movie' });
    // Both before pick() returned: nothing was awaited between the click and getDisplayMedia.
    expect(order).toEqual(['capture', 'load']);
    expect(capture).toHaveBeenCalledWith({ preset: 'movie' });
  });

  it('pick() does not wait for the publisher', async () => {
    const src = fakePick('window', true);
    const load = vi.fn(() => new Promise<Publisher>(() => undefined));
    const sharing = createBrowserSharing({ capture: () => Promise.resolve(src), load });
    await expect(sharing.pick({ preset: 'auto' })).resolves.toBe(src);
    await expect(
      createBrowserSharing({ capture: () => Promise.resolve(null), load }).pick({ preset: 'auto' }),
    ).resolves.toBeNull();
  });

  it("pick() passes the capture's failure on as it is", async () => {
    const failure = new LocalError('capture_failed');
    const sharing = createBrowserSharing({
      capture: () => Promise.reject(failure),
      load: () => Promise.resolve(fakePublisher()),
    });
    await expect(sharing.pick({ preset: 'auto' })).rejects.toBe(failure);
  });

  it('start() hands over to the publisher that the pick loaded: one publisher for every share', async () => {
    const publisher = fakePublisher();
    const load = vi.fn(() => Promise.resolve(publisher));
    const sharing = createBrowserSharing({ capture: () => Promise.resolve(fakePick('window', true)), load });

    const first = await sharing.pick({ preset: 'auto' });
    expect(first).not.toBeNull();
    if (!first) return;
    await expect(sharing.start(first, startOpts, ctx)).resolves.toMatchObject({ shareId: 's_window' });
    expect(publisher.start).toHaveBeenCalledWith(first, startOpts, ctx);

    const second = fakePick('browser', false);
    await sharing.pick({ preset: 'auto' });
    await expect(sharing.start(second, { preset: 'movie', withAudio: false }, ctx)).resolves.toMatchObject({
      shareId: 's_tab',
    });
    expect(publisher.start).toHaveBeenLastCalledWith(second, { preset: 'movie', withAudio: false }, ctx);
    // The pub PC lives in the publisher, one per signaling client (05 §13.6): it must be the same one each time.
    expect(load).toHaveBeenCalledOnce();
  });

  it('start() waits for a publisher that is still loading', async () => {
    const publisher = fakePublisher();
    const loading = deferred<Publisher>();
    const sharing = createBrowserSharing({ capture: () => Promise.resolve(null), load: () => loading.promise });
    const src = fakePick('monitor', true);
    await sharing.pick({ preset: 'auto' });
    const starting = sharing.start(src, startOpts, ctx);
    await settle();
    expect(publisher.start).not.toHaveBeenCalled();
    loading.resolve(publisher);
    await expect(starting).resolves.toMatchObject({ shareId: 's_screen' });
  });

  it('start() loads the publisher itself when no pick did, once for starts side by side', async () => {
    const publisher = fakePublisher();
    const load = vi.fn(() => Promise.resolve(publisher));
    const sharing = createBrowserSharing({ capture: vi.fn(), load });
    const [a, b] = await Promise.all([
      sharing.start(fakePick('window', true), startOpts, ctx),
      sharing.start(fakePick('browser', true), startOpts, ctx),
    ]);
    expect([a.shareId, b.shareId]).toEqual(['s_window', 's_tab']);
    expect(load).toHaveBeenCalledOnce();
  });

  it("start() passes the publisher's own failure on as it is", async () => {
    const failure = new LocalError('h264_unavailable');
    const sharing = createBrowserSharing({
      capture: vi.fn(),
      load: () => Promise.resolve({ start: () => Promise.reject(failure) }),
    });
    await expect(sharing.start(fakePick('window', true), startOpts, ctx)).rejects.toBe(failure);
  });

  describe('when the publisher cannot be loaded', () => {
    afterEach(() => {
      clearLog();
    });

    it('the pick still resolves, start() says `offline`, and the next start loads again', async () => {
      const publisher = fakePublisher();
      const gone = new TypeError('Failed to fetch dynamically imported module');
      const load = vi
        .fn<() => Promise<Publisher>>()
        .mockRejectedValueOnce(gone)
        .mockRejectedValueOnce(gone)
        .mockResolvedValue(publisher);
      const src = fakePick('window', true);
      const sharing = createBrowserSharing({ capture: () => Promise.resolve(src), load });

      // The load that the pick started fails: nobody hears of it yet.
      await expect(sharing.pick({ preset: 'auto' })).resolves.toBe(src);
      await settle();
      expect(load).toHaveBeenCalledTimes(1);

      // start() loads again, and that fails too.
      const err: unknown = await sharing.start(src, startOpts, ctx).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(LocalError);
      expect(err).toMatchObject({ code: 'offline', cause: gone });
      expect(load).toHaveBeenCalledTimes(2);
      expect(publisher.start).not.toHaveBeenCalled();
      // The source is the caller's to release, as on every rejection of start().
      expect(src.preview.getTracks().every((t) => t.readyState === 'live')).toBe(true);
      expect(logLines().filter((l) => l.component === 'capture' && l.level === 'warn')).toHaveLength(1);

      // The chunk arrives the next time.
      await expect(sharing.start(src, startOpts, ctx)).resolves.toMatchObject({ shareId: 's_window' });
      expect(load).toHaveBeenCalledTimes(3);
    });

    it('a loader that throws is a failed load like any other, and the picker is open by then', async () => {
      const src = fakePick('window', true);
      const capture = vi.fn(() => Promise.resolve<PickedSource | null>(src));
      const sharing = createBrowserSharing({
        capture,
        load: () => {
          throw new Error('no loader');
        },
      });
      await expect(sharing.pick({ preset: 'auto' })).resolves.toBe(src);
      expect(capture).toHaveBeenCalledOnce();
      await expect(sharing.start(src, startOpts, ctx)).rejects.toMatchObject({ name: 'LocalError', code: 'offline' });
    });
  });
});

describe('createBrowserSharing with the page’s own publisher', () => {
  afterEach(() => {
    vi.doUnmock('../../share/BrowserSharing');
    vi.resetModules();
  });

  it("imports share/'s BrowserSharing with the first pick, not before, and makes it once", async () => {
    const made: unknown[] = [];
    const evaluated = vi.fn();
    const start = vi.fn<Publisher['start']>(() => Promise.resolve({ shareId: 's_1' } as ActiveShare));
    vi.doMock('../../share/BrowserSharing', () => {
      evaluated();
      return {
        BrowserSharing: class {
          readonly start = start;
          constructor(deps: unknown) {
            made.push(deps);
          }
        },
      };
    });
    const src = fakePick('window', true);
    const capture = vi.fn(() => Promise.resolve<PickedSource | null>(src));
    const sharing = createBrowserSharing({ capture });
    await settle();
    expect(evaluated).not.toHaveBeenCalled();

    await sharing.pick({ preset: 'auto' });
    await expect(sharing.start(src, startOpts, ctx)).resolves.toMatchObject({ shareId: 's_1' });
    await sharing.pick({ preset: 'auto' });
    await sharing.start(src, startOpts, ctx);
    expect(evaluated).toHaveBeenCalledOnce();
    expect(made).toHaveLength(1);
    expect(start).toHaveBeenCalledTimes(2);

    // What only platform/ may touch: the picker and the page's RTCPeerConnection.
    const deps = made[0] as {
      capture: (opts: { preset: 'auto' }) => Promise<PickedSource | null>;
      platform: { createPeerConnection: (config: RTCConfiguration) => RTCPeerConnection };
    };
    await expect(deps.capture({ preset: 'auto' })).resolves.toBe(src);
    const before = FakeRTCPeerConnection.instances.length;
    const original = globalThis.RTCPeerConnection;
    globalThis.RTCPeerConnection = FakeRTCPeerConnection as unknown as typeof RTCPeerConnection;
    try {
      const pc = deps.platform.createPeerConnection({ bundlePolicy: 'max-bundle' });
      expect(pc).toBeInstanceOf(FakeRTCPeerConnection);
      expect(FakeRTCPeerConnection.instances).toHaveLength(before + 1);
    } finally {
      globalThis.RTCPeerConnection = original;
    }
  });
});
