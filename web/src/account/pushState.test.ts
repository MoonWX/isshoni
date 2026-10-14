// The push state that the notifications page and the room's prompts share (05 §16.3): one store per platform,
// what an enable() click came to, and the messages of a failure.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import { ApiError, NetworkError } from '../protocol/rest';
import { createTestPlatform } from '../test/platform';
import { pushErrorMessage, pushStore } from './pushState';
import { fakePush } from './testing/push';

const t = i18n.t.bind(i18n);

describe('pushStore', () => {
  it('is one store per platform', () => {
    const a = createTestPlatform();
    expect(pushStore(a)).toBe(pushStore(a));
    expect(pushStore(createTestPlatform())).not.toBe(pushStore(a));
  });

  it('asks status() only where push is supported, and shares one read between callers', async () => {
    const supported = fakePush('supported', 'on');
    const store = pushStore(createTestPlatform({ notifications: supported }));
    await Promise.all([store.getState().refresh(), store.getState().refresh()]);
    expect(supported.support).toHaveBeenCalledTimes(1);
    expect(supported.status).toHaveBeenCalledTimes(1);
    expect(store.getState()).toMatchObject({ support: 'supported', status: 'on' });

    const ios = fakePush('needs-install');
    const other = pushStore(createTestPlatform({ notifications: ios }));
    await other.getState().refresh();
    expect(ios.status).not.toHaveBeenCalled();
    expect(other.getState()).toMatchObject({ support: 'needs-install', status: 'off' });
  });

  it('takes a provider that throws as one that can’t push', async () => {
    const push = fakePush('supported');
    push.support.mockRejectedValue(new Error('boom'));
    const store = pushStore(createTestPlatform({ notifications: push }));
    await store.getState().refresh();
    expect(store.getState()).toMatchObject({ support: 'unsupported', status: 'off' });
  });

  it.each([
    ['on', () => undefined, 'on'],
    ['denied', (p: ReturnType<typeof fakePush>) => (p.state.support = 'denied'), 'denied'],
    ['still off (the prompt was dismissed)', () => undefined, 'off'],
  ] as const)('enable() comes to %s', async (name, after, want) => {
    const push = fakePush('supported');
    push.enable.mockImplementation(() => {
      if (name !== 'on') push.state.status = 'off';
      else push.state.status = 'on';
      after(push);
      return Promise.resolve();
    });
    const store = pushStore(createTestPlatform({ notifications: push }));
    await expect(store.getState().enable()).resolves.toBe(want);
    expect(store.getState().busy).toBe(false);
    expect(store.getState().error).toBeNull();
  });

  it('keeps the failure of an action, and clears it with the next one', async () => {
    const push = fakePush('supported');
    const failure = new ApiError({ status: 503, code: 'push_unavailable' });
    push.enable.mockRejectedValueOnce(failure);
    const store = pushStore(createTestPlatform({ notifications: push }));
    await expect(store.getState().enable()).resolves.toBe('failed');
    expect(store.getState().error).toBe(failure);
    await expect(store.getState().enable()).resolves.toBe('on');
    expect(store.getState().error).toBeNull();
  });

  it('turns a provider that throws in the click into a failure, not an exception', async () => {
    const push = fakePush('supported');
    push.enable.mockImplementation(() => {
      throw new Error('sync');
    });
    const store = pushStore(createTestPlatform({ notifications: push }));
    await expect(store.getState().enable()).resolves.toBe('failed');
  });

  it('disable() and test() say whether they worked', async () => {
    const push = fakePush('supported', 'on');
    const store = pushStore(createTestPlatform({ notifications: push }));
    await expect(store.getState().disable()).resolves.toBe(true);
    expect(store.getState().status).toBe('off');
    push.test.mockRejectedValueOnce(new NetworkError());
    await expect(store.getState().test()).resolves.toBe(false);
    expect(store.getState().error).toBeInstanceOf(NetworkError);
  });
});

describe('pushErrorMessage', () => {
  it('says the server’s and the network’s reasons as everywhere', () => {
    expect(pushErrorMessage(new ApiError({ status: 503, code: 'push_unavailable' }), t)).toBe(
      'Notifications are turned off on this server.',
    );
    expect(pushErrorMessage(new NetworkError(), t)).toBe("You're offline. Check your connection.");
  });

  it('has one text for what failed in the browser', () => {
    const text =
      "This browser couldn't turn on notifications. Check that they aren't turned off in its settings, then try again.";
    expect(pushErrorMessage(new Error('push: subscribe_failed'), t)).toBe(text);
    expect(pushErrorMessage(new DOMException('no', 'AbortError'), t)).toBe(text);
    expect(pushErrorMessage('a string', t)).toBe(text);
  });
});
