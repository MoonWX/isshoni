// SubscriptionSync (05 §12.4, 01 §8.9) against a stand-in for the SignalClient's request(): batching, the two
// delays, chunks of 64, the full re-send after a welcome, ignored shares, and the retry rules of 05 §6.3.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createLogger } from '../lib/log';
import { ProtocolError } from '../protocol/errors';
import type { SignalState } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import type { SubscribeResult, SubscriptionWant } from '../protocol/types.gen';
import { SUBSCRIBE_DEBOUNCE_MS, SUBSCRIBE_OFF_DELAY_MS, SubscriptionSync } from './subscriptionSync';
import { tick } from './testing/harness';

type Reply = SubscribeResult | ProtocolError;

/** The SignalClient's request(), recorded. Replies come from `answer` (default: ok), or by hand with `hold`. */
class FakeSignal {
  state: SignalState = 'ready';
  /** The subs of every subscribe.update, oldest first. */
  readonly updates: SubscriptionWant[][] = [];
  answer: (subs: SubscriptionWant[]) => Reply = () => ({ ignored: [] });
  /** Requests wait for release() instead of being answered at once. */
  hold = false;
  readonly #held: (() => void)[] = [];

  request = (type: string, data: { subs: SubscriptionWant[] }): Promise<SubscribeResult> => {
    expect(type).toBe('subscribe.update');
    this.updates.push(data.subs);
    return new Promise<SubscribeResult>((resolve, reject) => {
      const settle = (): void => {
        const reply = this.answer(data.subs);
        if (reply instanceof ProtocolError) reject(reply);
        else resolve(reply);
      };
      if (this.hold) this.#held.push(settle);
      else settle();
    });
  };

  /** Answers the oldest held request. */
  release(): void {
    this.#held.shift()?.();
  }
}

const want = (shareId: string, video: 'high' | 'low' | 'off', audio: 'on' | 'off' = 'off'): SubscriptionWant => ({
  shareId,
  video,
  audio,
});

const wireError = (code: string, extra = {}): ProtocolError =>
  ProtocolError.fromWire(makeError(code, 'request', extra));

let signal: FakeSignal;
let sync: SubscriptionSync;
let rejoin: ReturnType<typeof vi.fn<() => Promise<boolean>>>;

beforeEach(() => {
  vi.useFakeTimers();
  signal = new FakeSignal();
  rejoin = vi.fn(() => Promise.resolve(true));
  sync = new SubscriptionSync({ signal, log: createLogger('test'), rejoin });
});

afterEach(() => {
  sync.dispose();
  vi.useRealTimers();
});

describe('sending changes', () => {
  it('merges the changes of 150 ms into one subscribe.update', async () => {
    sync.set([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS - 1);
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    // The debounce starts again with each change.
    await tick(SUBSCRIBE_DEBOUNCE_MS - 1);
    expect(signal.updates).toEqual([]);
    await tick(1);
    expect(signal.updates).toEqual([[want('a', 'high', 'on'), want('b', 'low')]]);
    // Nothing more to say.
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
  });

  it('audio follows focus in one message: the old share and the new one together', async () => {
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'low'), want('b', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[1]).toEqual([want('a', 'low'), want('b', 'high', 'on')]);
  });

  it('a share that is no longer listed is turned off', async () => {
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    // Its audio changes too, so it doesn't wait like a tile that scrolled away.
    expect(signal.updates[1]).toEqual([want('a', 'off', 'off')]);
  });

  it('sends nothing for a set equal to the last one, or for {off, off} of a share the server never had', async () => {
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'low'), want('b', 'off', 'off')]);
    sync.set([want('a', 'low')]);
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
    expect(sync.desired).toEqual([want('a', 'low')]);
  });

  it('a tile that left the viewport waits 1 s, and nothing is sent when it comes back in time', async () => {
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'high', 'on')]); // b scrolled away
    await tick(SUBSCRIBE_OFF_DELAY_MS - 1);
    expect(signal.updates).toHaveLength(1);
    sync.set([want('a', 'high', 'on'), want('b', 'low')]); // and back
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);

    sync.set([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_OFF_DELAY_MS - 1);
    expect(signal.updates).toHaveLength(1);
    await tick(1);
    expect(signal.updates[1]).toEqual([want('b', 'off')]);
  });

  it('a raise does not wait for a pending off, and does not take it along early', async () => {
    sync.set([want('a', 'low'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'low')]); // b scrolled away
    await tick(400);
    sync.set([want('a', 'high', 'on')]); // a is focused
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[1]).toEqual([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_OFF_DELAY_MS - 400 - SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[2]).toEqual([want('b', 'off')]);
  });

  it('sends at most 64 shares per message', async () => {
    const wants = Array.from({ length: 130 }, (_, i) => want(`s${String(i)}`, 'low'));
    sync.set(wants);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates.map((u) => u.length)).toEqual([64, 64, 2]);
    expect(signal.updates.flat()).toEqual(wants);
  });

  it('drops the shares the server ignored from the local state', async () => {
    signal.answer = (subs) => ({ ignored: subs.filter((s) => s.shareId === 'gone').map((s) => s.shareId) });
    sync.set([want('a', 'low'), want('gone', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(sync.desired).toEqual([want('a', 'low')]);
    // Nothing is sent about it again, not even an off.
    sync.set([want('a', 'low')]);
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
  });

  it('corrects a want that went back while its request was out', async () => {
    signal.hold = true;
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    // The tile scrolled away before the ok: for the server (which holds nothing yet) that is no change.
    sync.set([]);
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
    // The ok makes it one: the server now sends a's video, and nobody wants it.
    signal.hold = false;
    signal.release();
    await tick(SUBSCRIBE_OFF_DELAY_MS);
    expect(signal.updates[1]).toEqual([want('a', 'off')]);
  });

  it('sends one request at a time, then what changed meanwhile', async () => {
    signal.hold = true;
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    signal.hold = false;
    signal.release();
    await tick();
    expect(signal.updates[1]).toEqual([want('a', 'high', 'on')]);
  });
});

describe('after a welcome', () => {
  it('sends nothing while signaling is not ready, then the full desired set', async () => {
    signal.state = 'backoff';
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(5_000);
    expect(signal.updates).toEqual([]);
    signal.state = 'ready';
    sync.resend(false);
    await tick();
    expect(signal.updates).toEqual([[want('a', 'high', 'on'), want('b', 'low')]]);
  });

  it('sends nothing when nothing is wanted', async () => {
    sync.resend(false);
    sync.resend(true);
    await tick(5_000);
    expect(signal.updates).toEqual([]);
  });

  it('re-sends the full set after a resumed welcome, with an off for what the server still holds', async () => {
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    signal.state = 'backoff';
    sync.set([want('a', 'high', 'on')]); // b ended up unwanted while offline
    await tick(5_000);
    signal.state = 'ready';
    sync.resend(true);
    await tick();
    expect(signal.updates[1]).toEqual([want('a', 'high', 'on'), want('b', 'off', 'off')]);
  });

  it('after a welcome that was not resumed the server holds nothing: only what is wanted goes out', async () => {
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'high', 'on')]);
    sync.resend(false);
    await tick();
    expect(signal.updates[1]).toEqual([want('a', 'high', 'on')]);
    // And the old ids of a restarted server are pruned by its ignored list.
    signal.answer = () => ({ ignored: ['a'] });
    sync.resend(false);
    await tick();
    expect(sync.desired).toEqual([]);
  });

  it('re-sends in chunks of at most 64', async () => {
    signal.state = 'backoff';
    sync.set(Array.from({ length: 100 }, (_, i) => want(`s${String(i)}`, 'low')));
    signal.state = 'ready';
    sync.resend(true);
    await tick();
    expect(signal.updates.map((u) => u.length)).toEqual([64, 36]);
  });

  it('does not apply the reply of a request from before the welcome', async () => {
    signal.hold = true;
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    // The socket dropped and came back as a new connection; the old request's ok arrives anyway.
    sync.resend(false);
    signal.hold = false;
    signal.release();
    await tick();
    // The full set went out again: the old ok says nothing about the new connection.
    expect(signal.updates).toEqual([[want('a', 'low')], [want('a', 'low')]]);
  });
});

describe('errors (05 §6.3, scope request)', () => {
  it('rate_limited: one retry after retryAfterMs', async () => {
    let calls = 0;
    signal.answer = () => (calls++ === 0 ? wireError('rate_limited', { retryAfterMs: 2_000 }) : { ignored: [] });
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(1);
    await tick(1_999);
    expect(signal.updates).toHaveLength(1);
    await tick(1);
    expect(signal.updates).toHaveLength(2);
    // It was applied: nothing left to send.
    sync.resend(true);
    await tick();
    expect(signal.updates[2]).toEqual([want('a', 'low')]);
  });

  it('drops the retry when the session left the room during the wait', async () => {
    signal.answer = () => wireError('rate_limited', { retryAfterMs: 2_000 });
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.clear();
    await tick(5_000);
    // The old room's shares are not sent into the next room.
    expect(signal.updates).toHaveLength(1);
  });

  it('internal: one retry at once, then it gives up until the next change', async () => {
    signal.answer = () => wireError('internal', { params: { ref: 'a1b2c3d4' } });
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates).toHaveLength(2);
    // No loop: a failed update waits for the next trigger.
    await tick(60_000);
    expect(signal.updates).toHaveLength(2);
    signal.answer = () => ({ ignored: [] });
    sync.set([want('a', 'low'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[2]).toEqual([want('a', 'low'), want('b', 'low')]);
  });

  it('not_in_room: rejoins, then sends the full set (the server dropped the subscriptions)', async () => {
    sync.set([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    let calls = 0;
    signal.answer = () => (calls++ === 0 ? wireError('not_in_room') : { ignored: [] });
    sync.set([want('a', 'high', 'on'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(rejoin).toHaveBeenCalledOnce();
    expect(signal.updates.slice(1)).toEqual([[want('b', 'low')], [want('a', 'high', 'on'), want('b', 'low')]]);
  });

  it('not_in_room again after the rejoin: gives up until the next trigger, which may rejoin once more', async () => {
    // A server that answers ok to room.join and not_in_room to every subscribe.update (a bug there, or a kick).
    signal.answer = () => wireError('not_in_room');
    sync.set([want('a', 'low')]);
    await tick(60_000);
    // The update, one rejoin, the full set: no loop of rejoins and full sets.
    expect(rejoin).toHaveBeenCalledOnce();
    expect(signal.updates).toEqual([[want('a', 'low')], [want('a', 'low')]]);

    sync.set([want('a', 'low'), want('b', 'low')]);
    await tick(60_000);
    expect(rejoin).toHaveBeenCalledTimes(2);
    expect(signal.updates).toHaveLength(4);
    // Once the server has the room again, the full set goes through.
    signal.answer = () => ({ ignored: [] });
    sync.resend(true);
    await tick();
    expect(signal.updates[4]).toEqual([want('a', 'low'), want('b', 'low')]);
    await tick(60_000);
    expect(signal.updates).toHaveLength(5);
    expect(rejoin).toHaveBeenCalledTimes(2);
  });

  it('not_in_room with a rejoin that fails: gives up', async () => {
    rejoin.mockResolvedValue(false);
    signal.answer = () => wireError('not_in_room');
    sync.set([want('a', 'low')]);
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
  });

  it('a lost connection is left to the next welcome', async () => {
    signal.answer = () => ProtocolError.local('connection_lost');
    sync.set([want('a', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    await tick(60_000);
    expect(signal.updates).toHaveLength(1);
    signal.answer = () => ({ ignored: [] });
    sync.resend(true);
    await tick();
    expect(signal.updates[1]).toEqual([want('a', 'low')]);
  });

  it('a pending off that was not due when a batch failed is still sent at its time', async () => {
    sync.set([want('a', 'low'), want('b', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.set([want('a', 'low')]); // b scrolled away: due in 1 s
    await tick(100);
    signal.answer = (subs) => (subs.some((s) => s.shareId === 'c') ? wireError('forbidden') : { ignored: [] });
    sync.set([want('a', 'low'), want('c', 'high')]); // the raise fails
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[1]).toEqual([want('c', 'high')]);
    await tick(SUBSCRIBE_OFF_DELAY_MS - 100 - SUBSCRIBE_DEBOUNCE_MS);
    // b's off went out at its time, although the batch before it failed.
    expect(signal.updates[2]).toEqual([want('b', 'off')]);
    // That success is a new trigger for the failed raise: one more try, then it rests again.
    await tick(60_000);
    expect(signal.updates.slice(3)).toEqual([[want('c', 'high')]]);
  });
});

describe('clear and dispose', () => {
  it('clear() forgets what is wanted and held, without telling the server (it left the room)', async () => {
    sync.set([want('a', 'high', 'on')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    sync.clear();
    expect(sync.desired).toEqual([]);
    await tick(5_000);
    expect(signal.updates).toHaveLength(1);
    // A new room starts from nothing.
    sync.set([want('x', 'low')]);
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(signal.updates[1]).toEqual([want('x', 'low')]);
  });

  it('dispose() stops the timers', async () => {
    sync.set([want('a', 'low')]);
    sync.dispose();
    sync.set([want('b', 'low')]);
    sync.resend(false);
    await tick(5_000);
    expect(signal.updates).toEqual([]);
  });
});
