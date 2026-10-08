// lazySubscriber: the sub PC controller that is made when its code has loaded (05 §10.1). Messages keep their
// order, close() before the load makes no controller at all, and a load that failed is tried again.
import { describe, expect, it, vi } from 'vitest';

import type { PCICE, PCOffer } from '../protocol/types.gen';
import { lazySubscriber } from './lazySubscriber';
import type { SubscriberLike } from './RoomSession';

const offer = (neg: number): PCOffer => ({ pc: 'sub', gen: 1, neg, sdp: `offer ${String(neg)}`, tracks: [] });
const ice = (n: number): PCICE => ({ pc: 'sub', gen: 1, candidate: { candidate: `candidate:${String(n)}` } });

/** A controller that records what it got, in order. */
function recorder(): SubscriberLike & { readonly got: string[] } {
  const got: string[] = [];
  return {
    got,
    handleOffer: (o) => {
      got.push(`offer ${String(o.neg)}`);
      return Promise.resolve();
    },
    handleIce: (i) => {
      got.push(i.candidate?.candidate ?? 'end');
      return Promise.resolve();
    },
    close: () => {
      got.push('close');
    },
  };
}

/** A load that the test settles. */
function deferredLoad() {
  let resolve!: (make: () => SubscriberLike) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<() => SubscriberLike>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { load: vi.fn(() => promise), resolve, reject };
}

describe('lazySubscriber', () => {
  it('loads with the first message, not before', async () => {
    const sub = recorder();
    const load = vi.fn(() => Promise.resolve(() => sub));
    const lazy = lazySubscriber(load);
    expect(load).not.toHaveBeenCalled();
    await lazy.handleOffer(offer(1));
    expect(load).toHaveBeenCalledOnce();
    expect(sub.got).toEqual(['offer 1']);
  });

  it('keeps the order of what came while the code was loading, and after', async () => {
    const sub = recorder();
    const { load, resolve } = deferredLoad();
    const lazy = lazySubscriber(load);
    const waiting = [lazy.handleOffer(offer(1)), lazy.handleIce(ice(1)), lazy.handleIce(ice(2))];
    expect(sub.got).toEqual([]);
    resolve(() => sub);
    await Promise.all(waiting);
    await lazy.handleOffer(offer(2));
    expect(sub.got).toEqual(['offer 1', 'candidate:1', 'candidate:2', 'offer 2']);
    // One load and one controller for all of them.
    expect(load).toHaveBeenCalledOnce();
  });

  it('makes no controller when close() came before the code', async () => {
    const make = vi.fn(recorder);
    const { load, resolve } = deferredLoad();
    const lazy = lazySubscriber(load);
    const waiting = lazy.handleOffer(offer(1));
    lazy.close();
    resolve(make);
    await waiting;
    expect(make).not.toHaveBeenCalled();
  });

  it('close() closes the controller, and what comes later is dropped', async () => {
    const sub = recorder();
    const lazy = lazySubscriber(() => Promise.resolve(() => sub));
    await lazy.handleOffer(offer(1));
    lazy.close();
    lazy.close();
    await lazy.handleIce(ice(1));
    expect(sub.got).toEqual(['offer 1', 'close']);
  });

  it('rejects the message whose load failed, and loads again for the next one', async () => {
    const sub = recorder();
    const load = vi
      .fn<() => Promise<() => SubscriberLike>>()
      .mockRejectedValueOnce(new Error('chunk failed to load'))
      .mockResolvedValue(() => sub);
    const lazy = lazySubscriber(load);
    await expect(lazy.handleOffer(offer(1))).rejects.toThrow('chunk failed to load');
    await lazy.handleOffer(offer(2));
    expect(load).toHaveBeenCalledTimes(2);
    expect(sub.got).toEqual(['offer 2']);
  });

  it('passes on a controller’s own failure to the caller', async () => {
    const sub = recorder();
    sub.handleOffer = () => Promise.reject(new Error('bad offer'));
    const lazy = lazySubscriber(() => Promise.resolve(() => sub));
    await expect(lazy.handleOffer(offer(1))).rejects.toThrow('bad offer');
  });
});
