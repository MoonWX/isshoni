// lazyBridge (docs/m2/03 §7.2): calls and subscriptions made before the transport has loaded wait for it; a failed
// load rejects every call; fire-and-forget methods report their failures instead of rejecting.
import { describe, expect, it, vi } from 'vitest';

import type { NativeBridge } from './bridge';
import { BridgeError } from './errors';
import { lazyBridge } from './lazyBridge';
import { authState, createFakeBridge } from './testing/fakeBridge';

const ORIGIN = 'https://watch.example.com';

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('lazyBridge', () => {
  it('queues calls made before the load and answers them after it', async () => {
    const load = deferred<NativeBridge>();
    const fake = createFakeBridge({ active: ORIGIN });
    const b = lazyBridge(load.promise, vi.fn());
    const token = b.auth.accessToken(ORIGIN, false);
    expect(fake.calls).toEqual([]);
    load.resolve(fake.bridge);
    expect(await token).toMatchObject({ token: 'isa_fake0001' });
    expect(await b.auth.accessToken(ORIGIN, true)).toMatchObject({ token: 'isa_fake0002' });
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [ORIGIN, false],
      [ORIGIN, true],
    ]);
  });

  it('subscribes on() after the load, and an unsubscribe before it cancels the subscription', async () => {
    const load = deferred<NativeBridge>();
    const fake = createFakeBridge({ active: ORIGIN });
    const b = lazyBridge(load.promise, vi.fn());
    const kept = vi.fn();
    const dropped = vi.fn();
    b.on('auth.state', kept);
    const off = b.on('auth.state', dropped);
    off();
    load.resolve(fake.bridge);
    await load.promise;
    await Promise.resolve();
    fake.emit('auth.state', authState(ORIGIN));
    expect(kept).toHaveBeenCalledTimes(1);
    expect(dropped).not.toHaveBeenCalled();
    const late = vi.fn();
    const offLate = b.on('auth.state', late);
    fake.emit('auth.state', authState(ORIGIN));
    offLate();
    fake.emit('auth.state', authState(ORIGIN));
    expect(late).toHaveBeenCalledTimes(1);
  });

  it('rejects every call with the load error, and reports the failed load once', async () => {
    const report = vi.fn();
    const b = lazyBridge(Promise.reject(new BridgeError('not_implemented')), report);
    await expect(b.auth.state()).rejects.toMatchObject({ code: 'not_implemented' });
    await expect(b.updates.status()).rejects.toMatchObject({ code: 'not_implemented' });
    expect(report).toHaveBeenCalledTimes(1);
    expect(report).toHaveBeenCalledWith('desktop: the bridge failed to load', expect.any(BridgeError));
  });

  it('returns nothing from fire-and-forget methods and reports their failures', async () => {
    const report = vi.fn();
    const fake = createFakeBridge();
    fake.answers['app.log'] = () => Promise.reject(new BridgeError('rate_limited'));
    const b = lazyBridge(Promise.resolve(fake.bridge), report);
    type Untyped = Record<string, ((...a: unknown[]) => unknown) | undefined>;
    expect((b.app as unknown as Untyped)['log']?.('info', 'hello')).toBeUndefined();
    expect((b.share as unknown as Untyped)['release']?.('p1')).toBeUndefined();
    await vi.waitFor(() => {
      expect(report).toHaveBeenCalledWith('desktop: app.log failed', expect.any(BridgeError));
    });
    expect(fake.callsOf('share.release')).toEqual([['p1']]);
  });

  it('has the method groups and on, nothing else, and no group looks like a promise', () => {
    const b = lazyBridge(new Promise<NativeBridge>(() => undefined), vi.fn());
    expect(typeof b.auth.accessToken).toBe('function');
    expect(typeof b.on).toBe('function');
    expect((b as unknown as Record<string, unknown>)['then']).toBeUndefined();
    expect((b as unknown as Record<string, unknown>)['other']).toBeUndefined();
    expect((b.auth as unknown as Record<string, unknown>)['then']).toBeUndefined();
  });

  it('rejects a method the transport lacks', async () => {
    const b = lazyBridge(Promise.resolve({ on: () => () => undefined } as unknown as NativeBridge), vi.fn());
    await expect(b.auth.state()).rejects.toThrow('no bridge method auth.state');
  });
});
