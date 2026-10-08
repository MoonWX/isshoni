import { describe, expect, it, vi } from 'vitest';

import { NotImplementedError } from '../lib/errors';
import { createLogger } from '../lib/log';
import type { PickedSource, SignalClientLike } from '../platform/types';
import { createTestPlatform } from '../test/platform';
import { BrowserSharing, type BrowserSharingDeps } from './BrowserSharing';
import { PublisherPC } from './PublisherPC';
import { fakePick } from './testing/fakeCapture';

function deps(capture: BrowserSharingDeps['capture']): BrowserSharingDeps {
  return { capture, platform: createTestPlatform() };
}

describe('BrowserSharing (05 §8)', () => {
  it('is the in-page provider', () => {
    expect(new BrowserSharing(deps(() => Promise.resolve(null))).mode).toBe('in-page');
  });

  it('pick() is the capture it was given: called at once, same result', async () => {
    const src = fakePick('window', true);
    const capture = vi.fn<BrowserSharingDeps['capture']>(() => Promise.resolve(src));
    const sharing = new BrowserSharing(deps(capture));
    const picking = sharing.pick({ preset: 'text' });
    expect(capture).toHaveBeenCalledExactlyOnceWith({ preset: 'text' });
    await expect(picking).resolves.toBe(src);
  });

  it('pick() passes a cancel (null) and a failed capture through', async () => {
    await expect(new BrowserSharing(deps(() => Promise.resolve(null))).pick({ preset: 'auto' })).resolves.toBeNull();
    const failure = new Error('capture failed');
    await expect(new BrowserSharing(deps(() => Promise.reject(failure))).pick({ preset: 'auto' })).rejects.toBe(
      failure,
    );
  });

  it('start() is not implemented until S46, and says so', async () => {
    const sharing = new BrowserSharing(deps(() => Promise.resolve(null)));
    const src: PickedSource = fakePick('window', true);
    const err: unknown = await sharing
      .start(src, { preset: 'auto', withAudio: true }, { signal: {} as SignalClientLike, roomId: 'lounge' })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(NotImplementedError);
    expect(err).toMatchObject({ member: 'sharing.start', slice: 'S46' });
  });
});

describe('PublisherPC (05 §13.6): declared, S46 writes it', () => {
  const pc = new PublisherPC({
    platform: createTestPlatform(),
    signal: {} as SignalClientLike,
    log: createLogger('test'),
  });

  it('starts at generation 1', () => {
    expect(pc.gen).toBe(1);
  });

  it('rejects every method with NotImplementedError naming the member and S46', async () => {
    const stream = fakePick('window', true).preview;
    const params = { shareId: 's1', codec: 'h264/6400', encodings: [], audioBitrate: 128_000 };
    const calls: Record<string, Promise<void>> = {
      addShare: pc.addShare('s1', stream, params, 'auto'),
      removeShare: pc.removeShare('s1'),
      applyParams: pc.applyParams('s1', { audioBitrate: 256_000 }),
      handleAnswer: pc.handleAnswer({ pc: 'pub', gen: 1, neg: 1, sdp: '' }),
      handleIce: pc.handleIce({ pc: 'pub', gen: 1 }),
      handleRestart: pc.handleRestart({ pc: 'pub', gen: 1, mode: 'ice', reason: 'disconnected' }),
      rebuild: pc.rebuild(),
    };
    for (const [member, call] of Object.entries(calls)) {
      await expect(call).rejects.toMatchObject({
        name: 'NotImplementedError',
        member: `PublisherPC.${member}`,
        slice: 'S46',
      });
    }
    expect(() => {
      pc.close();
    }).toThrow(NotImplementedError);
  });
});
