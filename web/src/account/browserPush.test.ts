// The browser's NotificationsProvider (platform/browser/push.ts, 05 §16.3) against fakes of the service worker
// container, the Push API, the Notification API and REST: the support states, the start-up re-post,
// enable/disable/test, and notification clicks. The test lives in account/, the folder of the slice that wrote the
// provider (S77), as S35's tests of its platform/browser files live in share/ (05 §3).
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { Info } from '../protocol/api.gen';
import { api as restApi, ApiError, NetworkError } from '../protocol/rest';
import type { DeviceEnv } from '../platform/browser/device';
import {
  base64UrlBytes,
  BrowserNotifications,
  INFO_MAX_AGE_MS,
  INFO_WAIT_MS,
  onNotificationOpen,
  openRequest,
  PUSH_SUBSCRIPTIONS_PATH,
  PUSH_TEST_PATH,
  PUSH_UNSUBSCRIBE_PATH,
  PushSetupError,
  subscribeRequest,
  type BrowserNotificationsOptions,
  type NotificationApi,
} from '../platform/browser/push';

const ORIGIN = 'https://isshoni.test';
/** A VAPID public key: 65 bytes, 0x04 first, base64url without padding (as 04 writes it). */
const KEY_BYTES = Uint8Array.from({ length: 65 }, (_, i) => (i === 0 ? 4 : i));
const KEY = toBase64Url(KEY_BYTES);
const OTHER_KEY_BYTES = Uint8Array.from({ length: 65 }, (_, i) => (i === 0 ? 4 : 200 - i));
const OTHER_KEY = toBase64Url(OTHER_KEY_BYTES);

function toBase64Url(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');
}

const UA = {
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  safariIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  safariIPad:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
};

function env(ua: string, { standalone = false, touch = 0 } = {}): DeviceEnv {
  return {
    ua,
    platform: ua.includes('iPhone') ? 'iPhone' : 'MacIntel',
    maxTouchPoints: touch,
    matches: (q) => standalone && q === '(display-mode: standalone)',
  };
}

// ---- fakes ----

class FakeSubscription {
  unsubscribed = 0;
  constructor(
    readonly endpoint: string,
    readonly key: Uint8Array | null,
  ) {}
  get options(): PushSubscriptionOptions {
    return { applicationServerKey: this.key ? this.key.slice().buffer : null, userVisibleOnly: true };
  }
  toJSON(): PushSubscriptionJSON {
    return { endpoint: this.endpoint, expirationTime: null, keys: { p256dh: 'BPd', auth: 'au' } };
  }
  unsubscribe(): Promise<boolean> {
    this.unsubscribed++;
    registration.current = registration.current === this ? null : registration.current;
    return Promise.resolve(true);
  }
}

/** The registration's push subscription, shared by the fakes (a browser keeps one per worker). */
const registration: { current: FakeSubscription | null; subscribes: PushSubscriptionOptionsInit[]; fail?: Error } = {
  current: null,
  subscribes: [],
};

function fakeRegistration(): ServiceWorkerRegistration {
  let n = 0;
  return {
    pushManager: {
      getSubscription: () => Promise.resolve(registration.current as unknown as PushSubscription | null),
      subscribe: (opts: PushSubscriptionOptionsInit) => {
        registration.subscribes.push(opts);
        if (registration.fail) return Promise.reject(registration.fail);
        const key: unknown = opts.applicationServerKey;
        const bytes = key instanceof Uint8Array ? new Uint8Array(key) : null;
        const sub = new FakeSubscription(`https://push.example/sub/${String(++n)}`, bytes);
        registration.current = sub;
        return Promise.resolve(sub as unknown as PushSubscription);
      },
    },
  } as unknown as ServiceWorkerRegistration;
}

class FakeContainer extends EventTarget {
  registration: ServiceWorkerRegistration | null = fakeRegistration();
  /** Resolves when a worker is active; never, for a container whose worker never activates. */
  ready: Promise<ServiceWorkerRegistration>;
  constructor({ active = true } = {}) {
    super();
    this.ready = active && this.registration ? Promise.resolve(this.registration) : new Promise(() => undefined);
  }
  getRegistration(): Promise<ServiceWorkerRegistration | undefined> {
    return Promise.resolve(this.registration ?? undefined);
  }
  /** A message from the worker (a notification click). */
  post(data: unknown): void {
    this.dispatchEvent(new MessageEvent('message', { data }));
  }
}

function fakeNotification(permission: NotificationPermission, answer: NotificationPermission = 'granted') {
  const n = {
    permission,
    requestPermission: vi.fn(() => {
      n.permission = answer;
      return Promise.resolve(answer);
    }),
  };
  return n satisfies NotificationApi;
}

interface Call {
  method: string;
  path: string;
  body: unknown;
}

/**
 * REST answered from a table; every call is recorded. `info: 'never'` is a server that never answers /info; `fail`
 * may be changed between calls.
 */
function fakeApi(answers: { info?: Partial<Info> | Error | 'never'; fail?: Partial<Record<string, Error>> } = {}) {
  const calls: Call[] = [];
  const fn = (method: string, path: string, body?: unknown): Promise<unknown> => {
    calls.push({ method, path, body });
    const failure = answers.fail?.[path];
    if (failure) return Promise.reject(failure);
    if (path === '/api/v1/info') {
      if (answers.info === 'never') return new Promise(() => undefined);
      if (answers.info instanceof Error) return Promise.reject(answers.info);
      return Promise.resolve({ setupRequired: false, push: { vapidPublicKey: KEY }, ...answers.info });
    }
    return Promise.resolve(undefined);
  };
  return { api: fn as unknown as typeof restApi, calls, posts: () => calls.filter((c) => c.method !== 'GET') };
}

interface Setup extends BrowserNotificationsOptions {
  api?: typeof restApi;
}

const providers: BrowserNotifications[] = [];

function provider(opts: Setup = {}): BrowserNotifications {
  const p = new BrowserNotifications({
    env: env(UA.chromeMac),
    serviceWorker: new FakeContainer() as unknown as ServiceWorkerContainer,
    notification: fakeNotification('default'),
    pushManager: true,
    origin: ORIGIN,
    assign: () => undefined,
    ...opts,
  });
  providers.push(p);
  return p;
}

afterEach(() => {
  for (const p of providers.splice(0)) p.dispose();
  registration.current = null;
  registration.subscribes = [];
  delete registration.fail;
  vi.useRealTimers();
});

// ---- tests ----

describe('support() (05 §16.3)', () => {
  it('is supported where the browser and the server can push', async () => {
    const rest = fakeApi();
    await expect(provider({ api: rest.api }).support()).resolves.toBe('supported');
    expect(rest.calls).toEqual([{ method: 'GET', path: '/api/v1/info', body: undefined }]);
  });

  it.each([
    ['an iPhone in a Safari tab', env(UA.safariIPhone)],
    ['an iPad in a Safari tab (MacIntel with touch)', env(UA.safariIPad, { touch: 5 })],
  ])('needs the Home Screen app on %s, whatever the tab has', async (_name, device) => {
    const rest = fakeApi();
    const p = provider({ env: device, api: rest.api, serviceWorker: null, pushManager: false, notification: null });
    await expect(p.support()).resolves.toBe('needs-install');
  });

  it('is unsupported on iOS when the server has no push: the Home Screen app would not help', async () => {
    const rest = fakeApi({ info: { push: undefined } });
    await expect(provider({ env: env(UA.safariIPhone), api: rest.api }).support()).resolves.toBe('unsupported');
  });

  it('is supported in the iOS Home Screen app with the Push API, unsupported without it (before iOS 16.4)', async () => {
    const app = env(UA.safariIPhone, { standalone: true });
    await expect(provider({ env: app, api: fakeApi().api }).support()).resolves.toBe('supported');
    await expect(provider({ env: app, api: fakeApi().api, pushManager: false }).support()).resolves.toBe('unsupported');
  });

  it.each([
    ['no service worker (a dev build, an old browser)', { serviceWorker: null }],
    ['no Push API', { pushManager: false }],
    ['no Notification API', { notification: null }],
  ])('is unsupported with %s, without asking the server', async (_name, missing) => {
    const rest = fakeApi();
    await expect(provider({ api: rest.api, ...missing }).support()).resolves.toBe('unsupported');
    expect(rest.calls).toEqual([]);
  });

  it('is unsupported when the server has no push (/info without push)', async () => {
    const rest = fakeApi({ info: { push: undefined } });
    await expect(provider({ api: rest.api }).support()).resolves.toBe('unsupported');
  });

  it('is denied when the user blocked notifications', async () => {
    const p = provider({ api: fakeApi().api, notification: fakeNotification('denied') });
    await expect(p.support()).resolves.toBe('denied');
  });

  it('counts a server it could not ask as one with push, and never rejects', async () => {
    const rest = fakeApi({ info: new NetworkError() });
    await expect(provider({ api: rest.api }).support()).resolves.toBe('supported');
  });

  it(`waits at most ${String(INFO_WAIT_MS / 1000)} s for /info, then counts the server as one with push`, async () => {
    vi.useFakeTimers();
    const rest = fakeApi({ info: 'never' });
    const p = provider({ api: rest.api });
    let answer: string | undefined;
    void p.support().then((s) => {
      answer = s;
    });
    await vi.advanceTimersByTimeAsync(INFO_WAIT_MS - 1);
    expect(answer).toBeUndefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(answer).toBe('supported');
    // The read that is still out is shared, not asked again.
    void p.support();
    await vi.advanceTimersByTimeAsync(INFO_WAIT_MS);
    expect(rest.calls).toHaveLength(1);
  });

  it(`keeps the answer of /info for ${String(INFO_MAX_AGE_MS / 60_000)} minutes`, async () => {
    let now = 1_000_000;
    const rest = fakeApi();
    const p = provider({ api: rest.api, now: () => now });
    await Promise.all([p.support(), p.support()]);
    await p.support();
    expect(rest.calls).toHaveLength(1);
    now += INFO_MAX_AGE_MS;
    await p.support();
    expect(rest.calls).toHaveLength(2);
  });
});

describe('status() and the start-up re-post (05 §16.3, 03 §12.4.6)', () => {
  it('is off without the permission, and asks nothing', async () => {
    const rest = fakeApi();
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    await expect(provider({ api: rest.api }).status()).resolves.toBe('off');
    expect(rest.calls).toEqual([]);
  });

  it('is off with the permission but no subscription (turned off on this device)', async () => {
    const rest = fakeApi();
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('off');
    expect(rest.posts()).toEqual([]);
  });

  it('is off where no worker is registered', async () => {
    const container = new FakeContainer();
    container.registration = null;
    const p = provider({
      serviceWorker: container as unknown as ServiceWorkerContainer,
      notification: fakeNotification('granted'),
    });
    await expect(p.status()).resolves.toBe('off');
  });

  it('posts the subscription again at every call: a later one may be a new session of the same page', async () => {
    const rest = fakeApi();
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    await expect(p.status()).resolves.toBe('on');
    const post = {
      method: 'POST',
      path: PUSH_SUBSCRIPTIONS_PATH,
      body: { endpoint: 'https://push.example/a', keys: { p256dh: 'BPd', auth: 'au' } },
    };
    expect(rest.posts()).toEqual([post, post]);
    expect(registration.subscribes).toEqual([]);
  });

  it('takes the key from support()’s answer of a moment ago: one /info for a room’s start', async () => {
    const rest = fakeApi();
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.support()).resolves.toBe('supported');
    await expect(p.status()).resolves.toBe('on');
    expect(rest.calls.map((c) => `${c.method} ${c.path}`)).toEqual([
      'GET /api/v1/info',
      `POST ${PUSH_SUBSCRIPTIONS_PATH}`,
    ]);
  });

  it('subscribes again with the new key when /info has another one (rotate-secrets --include-vapid)', async () => {
    const rest = fakeApi({ info: { push: { vapidPublicKey: OTHER_KEY } } });
    const old = new FakeSubscription('https://push.example/old', KEY_BYTES);
    registration.current = old;
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    expect(old.unsubscribed).toBe(1);
    expect(registration.subscribes).toHaveLength(1);
    expect(new Uint8Array(registration.subscribes[0]?.applicationServerKey as Uint8Array)).toEqual(OTHER_KEY_BYTES);
    expect(rest.posts().map((c) => (c.body as { endpoint: string }).endpoint)).toEqual(['https://push.example/sub/1']);
  });

  it('keeps a subscription whose key the browser does not report', async () => {
    const rest = fakeApi({ info: { push: { vapidPublicKey: OTHER_KEY } } });
    registration.current = new FakeSubscription('https://push.example/a', null);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    expect(registration.subscribes).toEqual([]);
    expect(rest.posts()).toHaveLength(1);
  });

  it('keeps the subscription and posts nothing while the server has push off', async () => {
    const rest = fakeApi({ info: { push: undefined } });
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts()).toEqual([]);
    expect(registration.current.unsubscribed).toBe(0);
  });

  it('tries again at the next call after a failed post', async () => {
    const fail: Partial<Record<string, Error>> = {
      [PUSH_SUBSCRIPTIONS_PATH]: new ApiError({ status: 503, code: 'server_busy' }),
    };
    const rest = fakeApi({ fail });
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    fail[PUSH_SUBSCRIPTIONS_PATH] = undefined;
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts().map((c) => c.path)).toEqual([PUSH_SUBSCRIPTIONS_PATH, PUSH_SUBSCRIPTIONS_PATH]);
  });

  it('tries again at the next call after a failed /info, asking /info again', async () => {
    const answers: { info?: Error } = { info: new NetworkError() };
    const rest = fakeApi(answers);
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts()).toEqual([]);
    delete answers.info;
    await expect(p.status()).resolves.toBe('on');
    expect(rest.calls.filter((c) => c.path === '/api/v1/info')).toHaveLength(2);
    expect(rest.posts().map((c) => c.path)).toEqual([PUSH_SUBSCRIPTIONS_PATH]);
  });

  it('takes a 401 calmly, and posts again after the next sign-in in the same page', async () => {
    const fail: Partial<Record<string, Error>> = {
      [PUSH_SUBSCRIPTIONS_PATH]: new ApiError({ status: 401, code: 'unauthenticated' }),
    };
    const rest = fakeApi({ fail });
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    // Signed in again (no reload): the next signed-in page's status() posts the subscription for the new session.
    fail[PUSH_SUBSCRIPTIONS_PATH] = undefined;
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts().map((c) => c.path)).toEqual([PUSH_SUBSCRIPTIONS_PATH, PUSH_SUBSCRIPTIONS_PATH]);
    expect(registration.current.unsubscribed).toBe(0);
  });

  it('posts the endpoint the worker made after a pushsubscriptionchange whose own post got a 401', async () => {
    const rest = fakeApi();
    registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
    const p = provider({ api: rest.api, notification: fakeNotification('granted') });
    await expect(p.status()).resolves.toBe('on');
    // The browser changed the subscription while signed out; the worker's post was refused (05 §16.3).
    registration.current = new FakeSubscription('https://push.example/b', KEY_BYTES);
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts().map((c) => (c.body as { endpoint: string }).endpoint)).toEqual([
      'https://push.example/a',
      'https://push.example/b',
    ]);
  });
});

describe('enable() (05 §16.3)', () => {
  it('asks for the permission in the click’s own turn, before anything is awaited', () => {
    const notification = fakeNotification('default');
    const p = provider({ api: fakeApi().api, notification });
    void p.enable();
    expect(notification.requestPermission).toHaveBeenCalledTimes(1);
  });

  it('subscribes with the key from a fresh /info and posts the subscription', async () => {
    let now = 0;
    const rest = fakeApi();
    const p = provider({ api: rest.api, now: () => now, notification: fakeNotification('default') });
    await p.support(); // an answer of /info in the cache
    now = 1;
    await p.enable();
    expect(rest.calls.filter((c) => c.path === '/api/v1/info')).toHaveLength(2);
    expect(registration.subscribes).toHaveLength(1);
    expect(registration.subscribes[0]?.userVisibleOnly).toBe(true);
    expect(new Uint8Array(registration.subscribes[0]?.applicationServerKey as Uint8Array)).toEqual(KEY_BYTES);
    expect(rest.posts()).toEqual([
      {
        method: 'POST',
        path: PUSH_SUBSCRIPTIONS_PATH,
        body: { endpoint: 'https://push.example/sub/1', keys: { p256dh: 'BPd', auth: 'au' } },
      },
    ]);
    // On now. The read that follows posts the same subscription again (the server upserts it), and subscribes
    // nothing new.
    await expect(p.status()).resolves.toBe('on');
    expect(rest.posts()).toHaveLength(2);
    expect(rest.posts()[1]?.body).toEqual(rest.posts()[0]?.body);
    expect(registration.subscribes).toHaveLength(1);
  });

  it.each([['denied'], ['default']] as const)('resolves without subscribing when the answer is %s', async (answer) => {
    const rest = fakeApi();
    const p = provider({ api: rest.api, notification: fakeNotification('default', answer) });
    await expect(p.enable()).resolves.toBeUndefined();
    expect(registration.subscribes).toEqual([]);
    expect(rest.calls).toEqual([]);
    await expect(p.status()).resolves.toBe('off');
    await expect(p.support()).resolves.toBe(answer === 'denied' ? 'denied' : 'supported');
  });

  it('fails with push_unavailable when the server has no push', async () => {
    const p = provider({ api: fakeApi({ info: { push: undefined } }).api });
    await expect(p.enable()).rejects.toMatchObject({ name: 'ApiError', code: 'push_unavailable' });
    expect(registration.subscribes).toEqual([]);
  });

  it('fails as /info failed when the server can’t be asked', async () => {
    const p = provider({ api: fakeApi({ info: new NetworkError() }).api });
    await expect(p.enable()).rejects.toBeInstanceOf(NetworkError);
  });

  it('reuses a subscription made with the same key, and replaces one made with another', async () => {
    const same = new FakeSubscription('https://push.example/same', KEY_BYTES);
    registration.current = same;
    const rest = fakeApi();
    await provider({ api: rest.api }).enable();
    expect(registration.subscribes).toEqual([]);
    expect(rest.posts()[0]?.body).toMatchObject({ endpoint: 'https://push.example/same' });

    const other = new FakeSubscription('https://push.example/other', OTHER_KEY_BYTES);
    registration.current = other;
    await provider({ api: fakeApi().api }).enable();
    expect(other.unsubscribed).toBe(1);
    expect(registration.subscribes).toHaveLength(1);
  });

  it('takes the subscription back when the server refuses it', async () => {
    const refused = new ApiError({ status: 422, code: 'push_endpoint_rejected', params: { reason: 'not_https' } });
    const p = provider({ api: fakeApi({ fail: { [PUSH_SUBSCRIPTIONS_PATH]: refused } }).api });
    await expect(p.enable()).rejects.toBe(refused);
    expect(registration.current).toBeNull();
    await expect(p.status()).resolves.toBe('off');
  });

  it('fails with subscribe_failed when the browser’s push service refuses', async () => {
    registration.fail = new DOMException('Registration failed - push service not available', 'AbortError');
    const rest = fakeApi();
    await expect(provider({ api: rest.api }).enable()).rejects.toMatchObject({
      name: 'PushSetupError',
      reason: 'subscribe_failed',
    });
    expect(rest.posts()).toEqual([]);
  });

  it('fails with no_worker when no service worker becomes active in time', async () => {
    vi.useFakeTimers();
    const container = new FakeContainer({ active: false });
    const p = provider({ api: fakeApi().api, serviceWorker: container as unknown as ServiceWorkerContainer });
    const result = p.enable();
    const assertion = expect(result).rejects.toMatchObject({ reason: 'no_worker' });
    await vi.advanceTimersByTimeAsync(10_000);
    await assertion;
  });

  it('fails with bad_key for a key that is not base64url', async () => {
    const p = provider({ api: fakeApi({ info: { push: { vapidPublicKey: 'not a key!' } } }).api });
    await expect(p.enable()).rejects.toMatchObject({ reason: 'bad_key' });
  });

  it('fails at once where the browser can’t push, without asking for the permission', async () => {
    const notification = fakeNotification('default');
    const p = provider({ notification, pushManager: false });
    await expect(p.enable()).rejects.toBeInstanceOf(PushSetupError);
    expect(notification.requestPermission).not.toHaveBeenCalled();
  });
});

describe('disable() and test()', () => {
  it('tells the server first, then drops the browser’s subscription', async () => {
    const sub = new FakeSubscription('https://push.example/a', KEY_BYTES);
    registration.current = sub;
    const rest = fakeApi();
    await provider({ api: rest.api }).disable();
    expect(rest.posts()).toEqual([
      { method: 'POST', path: PUSH_UNSUBSCRIBE_PATH, body: { endpoint: 'https://push.example/a' } },
    ]);
    expect(sub.unsubscribed).toBe(1);
  });

  it('does nothing without a subscription', async () => {
    const rest = fakeApi();
    await provider({ api: rest.api }).disable();
    expect(rest.calls).toEqual([]);
  });

  it('drops the subscription after a 401: the server deleted it with the session', async () => {
    const sub = new FakeSubscription('https://push.example/a', KEY_BYTES);
    registration.current = sub;
    const rest = fakeApi({
      fail: { [PUSH_UNSUBSCRIBE_PATH]: new ApiError({ status: 401, code: 'unauthenticated' }) },
    });
    await provider({ api: rest.api }).disable();
    expect(sub.unsubscribed).toBe(1);
  });

  it('keeps the subscription when the server could not be told', async () => {
    const sub = new FakeSubscription('https://push.example/a', KEY_BYTES);
    registration.current = sub;
    const rest = fakeApi({ fail: { [PUSH_UNSUBSCRIBE_PATH]: new NetworkError() } });
    await expect(provider({ api: rest.api }).disable()).rejects.toBeInstanceOf(NetworkError);
    expect(sub.unsubscribed).toBe(0);
  });

  it('test() posts to /api/v1/push/test and passes the server’s errors on', async () => {
    const rest = fakeApi();
    await provider({ api: rest.api }).test();
    expect(rest.posts()).toEqual([{ method: 'POST', path: PUSH_TEST_PATH, body: undefined }]);
    const limited = new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 7 });
    await expect(provider({ api: fakeApi({ fail: { [PUSH_TEST_PATH]: limited } }).api }).test()).rejects.toBe(limited);
  });

  describe('test() when the server has no subscription of this session (404 not_found)', () => {
    /** A server whose answer to a test is 404 until this session posts a subscription (03 §12.4.6). */
    function serverWithoutRow() {
      const notFound = new ApiError({ status: 404, code: 'not_found' });
      const fail: Partial<Record<string, Error>> = { [PUSH_TEST_PATH]: notFound };
      const rest = fakeApi({ fail });
      const api = (async (...args: Parameters<typeof restApi>) => {
        const out: unknown = await rest.api(...args);
        const [, path] = args;
        if (path === PUSH_SUBSCRIPTIONS_PATH) fail[PUSH_TEST_PATH] = undefined;
        return out;
      }) as typeof restApi;
      return { notFound, fail, rest, api };
    }

    it('posts the subscription again and tries once more', async () => {
      const server = serverWithoutRow();
      registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
      await provider({ api: server.api, notification: fakeNotification('granted') }).test();
      expect(server.rest.posts().map((c) => c.path)).toEqual([PUSH_TEST_PATH, PUSH_SUBSCRIPTIONS_PATH, PUSH_TEST_PATH]);
    });

    it.each([
      ['there is no subscription to post', null, 'granted'],
      ['the permission is gone', 'https://push.example/a', 'default'],
    ] as const)('passes the 404 on when %s', async (_name, endpoint, permission) => {
      const server = serverWithoutRow();
      registration.current = endpoint === null ? null : new FakeSubscription(endpoint, KEY_BYTES);
      const p = provider({ api: server.api, notification: fakeNotification(permission) });
      await expect(p.test()).rejects.toBe(server.notFound);
      expect(server.rest.posts().map((c) => c.path)).toEqual([PUSH_TEST_PATH]);
    });

    it('passes the second 404 on when the post did not help', async () => {
      const server = serverWithoutRow();
      server.fail[PUSH_SUBSCRIPTIONS_PATH] = new ApiError({ status: 503, code: 'server_busy' });
      registration.current = new FakeSubscription('https://push.example/a', KEY_BYTES);
      const p = provider({ api: server.api, notification: fakeNotification('granted') });
      await expect(p.test()).rejects.toBe(server.notFound);
      expect(server.rest.posts().map((c) => c.path)).toEqual([PUSH_TEST_PATH, PUSH_SUBSCRIPTIONS_PATH, PUSH_TEST_PATH]);
    });
  });
});

describe('a click on a notification (05 §16.2: the worker posts {type: "open", url})', () => {
  it('navigates with the latest handler, the one before it after that one leaves, else the whole page', () => {
    const container = new FakeContainer();
    const assign = vi.fn();
    provider({ serviceWorker: container as unknown as ServiceWorkerContainer, assign });
    const first = vi.fn();
    const second = vi.fn();
    const offFirst = onNotificationOpen(first);
    const offSecond = onNotificationOpen(second);
    container.post({ type: 'open', url: '/r/lounge?focus=s_1' });
    expect(second).toHaveBeenCalledWith('/r/lounge?focus=s_1');
    expect(first).not.toHaveBeenCalled();
    offSecond();
    container.post({ type: 'open', url: `${ORIGIN}/admin/approvals` });
    expect(first).toHaveBeenCalledWith('/admin/approvals');
    offFirst();
    container.post({ type: 'open', url: '/account/notifications' });
    expect(assign).toHaveBeenCalledWith('/account/notifications');
  });

  it.each([
    ['another site', { type: 'open', url: 'https://evil.example/r/lounge' }],
    ['a protocol-relative URL', { type: 'open', url: '//evil.example/' }],
    ['a path that resolves to another site', { type: 'open', url: '/.//evil.example/' }],
    ['javascript:', { type: 'open', url: 'javascript:alert(1)' }],
    ['another message', { type: 'SKIP_WAITING' }],
    ['no URL', { type: 'open' }],
    ['not an object', 'open'],
  ])('ignores %s', (_name, data) => {
    const container = new FakeContainer();
    const assign = vi.fn();
    provider({ serviceWorker: container as unknown as ServiceWorkerContainer, assign });
    const handler = vi.fn();
    const off = onNotificationOpen(handler);
    container.post(data);
    off();
    container.post(data);
    expect(handler).not.toHaveBeenCalled();
    expect(assign).not.toHaveBeenCalled();
  });

  it('stops listening when disposed', () => {
    const container = new FakeContainer();
    const assign = vi.fn();
    provider({ serviceWorker: container as unknown as ServiceWorkerContainer, assign }).dispose();
    container.post({ type: 'open', url: '/' });
    expect(assign).not.toHaveBeenCalled();
  });

  it('openRequest keeps path, query and fragment of this origin only', () => {
    expect(openRequest({ type: 'open', url: '/r/a?focus=s#x' }, ORIGIN)).toBe('/r/a?focus=s#x');
    expect(openRequest({ type: 'open', url: 'http://isshoni.test/r/a' }, ORIGIN)).toBeNull();
  });
});

describe('helpers', () => {
  it('base64UrlBytes decodes with and without padding, and refuses what isn’t base64url', () => {
    expect(base64UrlBytes(KEY)).toEqual(KEY_BYTES);
    expect(base64UrlBytes('AQID')).toEqual(Uint8Array.of(1, 2, 3));
    expect(base64UrlBytes('AQI=')).toEqual(Uint8Array.of(1, 2));
    expect(base64UrlBytes('AQI')).toEqual(Uint8Array.of(1, 2));
    expect(base64UrlBytes('-_8')).toEqual(Uint8Array.of(251, 255));
    for (const bad of ['a+b/', 'not a key', 'A', 'AQ===']) expect(base64UrlBytes(bad), bad).toBeNull();
  });

  it('subscribeRequest trims toJSON() to 03’s body, and refuses one without keys', () => {
    const sub = new FakeSubscription('https://push.example/a', KEY_BYTES) as unknown as PushSubscription;
    expect(subscribeRequest(sub)).toEqual({ endpoint: 'https://push.example/a', keys: { p256dh: 'BPd', auth: 'au' } });
    const bare = { toJSON: () => ({ endpoint: 'https://push.example/a' }) } as unknown as PushSubscription;
    expect(subscribeRequest(bare)).toBeNull();
  });
});
