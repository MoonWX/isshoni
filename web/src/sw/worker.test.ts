// @vitest-environment node
import { describe, expect, it } from 'vitest';

import en from '../i18n/en.json';
import { OPEN_URL, SKIP_WAITING } from './contract';
import {
  FakeExtendableEvent,
  FakeFetchEvent,
  fakeRequest,
  FakeScope,
  ORIGIN,
  type FakeWindowClient,
} from './testing/fakes';
import { PUSH_SUBSCRIPTIONS_PATH, startWorker, type WorkerConfig } from './worker';

const CONFIG: WorkerConfig = {
  shell: ['/', '/assets/index-abc123.css', '/assets/index-def456.js', '/boot-check.js'],
  shellVersion: 'aaaaaaaaaaaa',
  pushStrings: en.push,
};
const CACHE = 'isshoni-shell-aaaaaaaaaaaa';

function start(config: WorkerConfig = CONFIG): FakeScope {
  const scope = new FakeScope();
  startWorker(scope.asScope(), config);
  return scope;
}

async function fire(scope: FakeScope, type: string, extra: object = {}): Promise<FakeExtendableEvent> {
  const event = Object.assign(new FakeExtendableEvent(), extra);
  scope.dispatch(type, event);
  await event.settled();
  return event;
}

function windowClient(visibilityState: DocumentVisibilityState, focus?: () => Promise<unknown>) {
  const messages: unknown[] = [];
  let focused = 0;
  const client: FakeWindowClient = {
    visibilityState,
    focus:
      focus ??
      (() => {
        focused++;
        return Promise.resolve(client);
      }),
    postMessage: (message) => {
      messages.push(message);
    },
  };
  return { client, messages, focused: () => focused };
}

function notification(data: unknown) {
  let closed = 0;
  return {
    notification: {
      data,
      close: () => {
        closed++;
      },
    },
    closed: () => closed,
  };
}

function pushSubscription(endpoint: string, key: ArrayBuffer | null = null): PushSubscription {
  return {
    endpoint,
    options: { applicationServerKey: key, userVisibleOnly: true },
    toJSON: () => ({ endpoint, keys: { p256dh: 'p', auth: 'a' } }),
  } as unknown as PushSubscription;
}

describe('startWorker', () => {
  it('registers one handler per event', () => {
    expect([...start().handlers.keys()].sort()).toEqual([
      'activate',
      'fetch',
      'install',
      'message',
      'notificationclick',
      'push',
      'pushsubscriptionchange',
    ]);
  });
});

describe('install and activate (05 §16.2)', () => {
  it('install precaches the shell into the cache of its version, and does not skip waiting', async () => {
    const scope = start();
    await fire(scope, 'install');
    expect([...scope.cacheStorage.caches.keys()]).toEqual([CACHE]);
    expect(scope.cacheStorage.get(CACHE).added.map((r) => new URL(r.url).pathname)).toEqual(CONFIG.shell);
    expect(scope.skipWaitingCalls).toBe(0);
  });

  it('a worker of another version fills its own cache', async () => {
    const scope = start({ ...CONFIG, shell: ['/', '/assets/index-new.js'], shellVersion: 'bbbbbbbbbbbb' });
    await fire(scope, 'install');
    expect(scope.cacheStorage.get('isshoni-shell-bbbbbbbbbbbb').added.map((r) => new URL(r.url).pathname)).toEqual([
      '/',
      '/assets/index-new.js',
    ]);
  });

  it('the install fails when the shell cannot be cached', async () => {
    const scope = start();
    await scope.caches.open(CACHE);
    scope.cacheStorage.get(CACHE).failWrites = new TypeError('Failed to fetch');
    await expect(fire(scope, 'install')).rejects.toThrow('Failed to fetch');
  });

  it('activate deletes the other shell caches, then claims the open pages', async () => {
    const scope = start();
    await fire(scope, 'install');
    await scope.caches.open('isshoni-shell-000000000000');
    await scope.caches.open('unrelated');
    expect(scope.claimCalls).toBe(0);
    await fire(scope, 'activate');
    expect([...scope.cacheStorage.caches.keys()]).toEqual([CACHE, 'unrelated']);
    expect(scope.claimCalls).toBe(1);
  });
});

describe('fetch', () => {
  const fetchEvent = (scope: FakeScope, request: Request): FakeFetchEvent => {
    const event = new FakeFetchEvent(request);
    scope.dispatch('fetch', event);
    return event;
  };

  it('answers a navigation from the network', async () => {
    const scope = start();
    const request = fakeRequest('/r/lounge', { mode: 'navigate' });
    const event = fetchEvent(scope, request);
    await expect((await event.response)?.text()).resolves.toBe('from the network');
    expect(scope.fetched).toEqual([{ input: request, init: undefined }]);
  });

  it('answers a navigation with the cached shell when the server is unreachable', async () => {
    const scope = start();
    await fire(scope, 'install');
    scope.network = () => Promise.reject(new TypeError('Failed to fetch'));
    const event = fetchEvent(scope, fakeRequest('/r/lounge', { mode: 'navigate' }));
    await expect((await event.response)?.text()).resolves.toBe('precached /');
  });

  it('answers a precached asset from the cache', async () => {
    const scope = start();
    await fire(scope, 'install');
    const event = fetchEvent(scope, fakeRequest('/assets/index-def456.js'));
    await expect((await event.response)?.text()).resolves.toBe('precached /assets/index-def456.js');
    expect(scope.fetched).toEqual([]);
  });

  it('caches a lazy chunk in the background', async () => {
    const scope = start();
    await fire(scope, 'install');
    const event = fetchEvent(scope, fakeRequest('/assets/admin-0a1b2c.js'));
    await event.response;
    await event.settled();
    expect(scope.cacheStorage.get(CACHE).entries.has(`${ORIGIN}/assets/admin-0a1b2c.js`)).toBe(true);
  });

  it.each([
    ['/api/v1/me', { mode: 'cors' }],
    ['/api/v1/push/subscriptions', { mode: 'cors', method: 'POST' }],
    ['/ws', { mode: 'websocket' }],
    ['/version.json', { mode: 'cors' }],
    ['/sw.js', { mode: 'same-origin' }],
  ])('does not answer %s: the browser fetches it as if there were no worker', (path, init) => {
    const scope = start();
    const event = fetchEvent(scope, fakeRequest(path, init));
    expect(event.response).toBeNull();
    expect(event.tasks).toEqual([]);
    expect(scope.fetched).toEqual([]);
  });
});

describe('message', () => {
  it('SKIP_WAITING activates the waiting worker', async () => {
    const scope = start();
    await fire(scope, 'message', { data: { type: SKIP_WAITING } });
    expect(scope.skipWaitingCalls).toBe(1);
  });

  it.each([[{ type: 'OTHER' }], [{ type: OPEN_URL, url: '/' }], ['SKIP_WAITING'], [null], [undefined], [7]])(
    'ignores %j',
    async (data) => {
      const scope = start();
      await fire(scope, 'message', { data });
      expect(scope.skipWaitingCalls).toBe(0);
    },
  );
});

describe('push: every message shows a notification (05 §16.3)', () => {
  const data = (json: () => unknown) => ({ data: { json } });

  it('shows the notification of the payload', async () => {
    const scope = start();
    const payload = { v: 1, type: 'push.test', ts: 1790712000000, tag: 'test', url: '/account/notifications' };
    await fire(
      scope,
      'push',
      data(() => payload),
    );
    expect(scope.notifications).toHaveLength(1);
    expect(scope.notifications[0]?.title).not.toBe('');
    expect(scope.notifications[0]?.options).toMatchObject({
      icon: '/icons/icon-192.png',
      badge: '/icons/badge-72.png',
      tag: 'test',
      timestamp: 1790712000000,
      data: { url: '/account/notifications' },
    });
  });

  it.each([
    ['a body that is not JSON', data(() => JSON.parse('<html>') as unknown)],
    ['a body that is JSON null', data(() => null)],
    ['a body of the wrong shape', data(() => [1, 2, 3])],
    ['no body at all', { data: null }],
  ])('shows the generic notification for %s', async (_name, event) => {
    const scope = start();
    await fire(scope, 'push', event);
    expect(scope.notifications).toEqual([
      {
        title: en.push.generic.title,
        options: {
          body: en.push.generic.body,
          icon: '/icons/icon-192.png',
          badge: '/icons/badge-72.png',
          data: { url: '/' },
        },
      },
    ]);
  });

  it('still shows one when the catalog was built without push texts', async () => {
    const scope = start({ ...CONFIG, pushStrings: {} });
    await fire(scope, 'push', { data: null });
    expect(scope.notifications).toHaveLength(1);
    expect(scope.notifications[0]?.title).toBe('isshoni');
  });
});

describe('notificationclick', () => {
  it('closes the notification, focuses the visible window and tells it where to go', async () => {
    const scope = start();
    const hidden = windowClient('hidden');
    const visible = windowClient('visible');
    scope.windows = [hidden.client, visible.client];
    const n = notification({ url: '/r/lounge?focus=s_q7m2x9c4v8b1n5k3' });
    await fire(scope, 'notificationclick', n);
    expect(n.closed()).toBe(1);
    expect(visible.focused()).toBe(1);
    expect(visible.messages).toEqual([{ type: 'open', url: '/r/lounge?focus=s_q7m2x9c4v8b1n5k3' }]);
    expect(hidden.focused()).toBe(0);
    expect(hidden.messages).toEqual([]);
    expect(scope.opened).toEqual([]);
  });

  it('takes any window when none is visible', async () => {
    const scope = start();
    const hidden = windowClient('hidden');
    scope.windows = [hidden.client];
    await fire(scope, 'notificationclick', notification({ url: '/admin/approvals' }));
    expect(hidden.focused()).toBe(1);
    expect(hidden.messages).toEqual([{ type: OPEN_URL, url: '/admin/approvals' }]);
  });

  it('opens a window when the app is not open', async () => {
    const scope = start();
    await fire(scope, 'notificationclick', notification({ url: '/r/lounge' }));
    expect(scope.opened).toEqual(['/r/lounge']);
  });

  it('opens a window when the browser refuses to focus the open one', async () => {
    const scope = start();
    const stubborn = windowClient('visible', () => Promise.reject(new Error('not allowed to focus')));
    scope.windows = [stubborn.client];
    await fire(scope, 'notificationclick', notification({ url: '/r/lounge' }));
    expect(stubborn.messages).toEqual([]);
    expect(scope.opened).toEqual(['/r/lounge']);
  });

  it.each([
    ['another site', { url: 'https://evil.example/login' }],
    ['a protocol-relative URL', { url: '//evil.example/login' }],
    ['javascript:', { url: 'javascript:alert(1)' }],
    ['no URL', {}],
    ['no data', null],
    ['data that is not an object', 'https://evil.example/'],
  ])('opens / instead of %s', async (_name, data) => {
    const scope = start();
    await fire(scope, 'notificationclick', notification(data));
    expect(scope.opened).toEqual(['/']);
  });

  it('never sends an open window to another site either', async () => {
    const scope = start();
    const visible = windowClient('visible');
    scope.windows = [visible.client];
    await fire(scope, 'notificationclick', notification({ url: 'https://evil.example/login' }));
    expect(visible.messages).toEqual([{ type: 'open', url: '/' }]);
  });
});

describe('pushsubscriptionchange', () => {
  const posted = (scope: FakeScope) =>
    scope.fetched.map(({ input, init }) => ({
      input,
      method: init?.method,
      credentials: init?.credentials,
      headers: init?.headers,
      body: typeof init?.body === 'string' ? (JSON.parse(init.body) as unknown) : init?.body,
    }));

  it('posts the new subscription the browser already made', async () => {
    const scope = start();
    await fire(scope, 'pushsubscriptionchange', {
      oldSubscription: pushSubscription('https://push.example/old'),
      newSubscription: pushSubscription('https://push.example/new'),
    });
    expect(scope.subscribeCalls).toEqual([]);
    expect(posted(scope)).toEqual([
      {
        input: PUSH_SUBSCRIPTIONS_PATH,
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: { endpoint: 'https://push.example/new', keys: { p256dh: 'p', auth: 'a' } },
      },
    ]);
    expect(PUSH_SUBSCRIPTIONS_PATH).toBe('/api/v1/push/subscriptions');
  });

  it('subscribes again with the old key when the browser only dropped the subscription', async () => {
    const scope = start();
    const key = new Uint8Array([4, 1, 2, 3]).buffer;
    scope.subscribeResult = pushSubscription('https://push.example/renewed');
    await fire(scope, 'pushsubscriptionchange', {
      oldSubscription: pushSubscription('https://push.example/old', key),
      newSubscription: null,
    });
    expect(scope.subscribeCalls).toEqual([{ userVisibleOnly: true, applicationServerKey: key }]);
    expect(posted(scope).map((p) => p.body)).toEqual([
      { endpoint: 'https://push.example/renewed', keys: { p256dh: 'p', auth: 'a' } },
    ]);
  });

  it('does nothing without a key to subscribe with: the page subscribes again at its next start', async () => {
    const scope = start();
    await fire(scope, 'pushsubscriptionchange', { oldSubscription: null, newSubscription: null });
    await fire(scope, 'pushsubscriptionchange', {
      oldSubscription: pushSubscription('https://push.example/old', null),
      newSubscription: null,
    });
    expect(scope.subscribeCalls).toEqual([]);
    expect(scope.fetched).toEqual([]);
  });

  it('ignores a 401 (logged out), a failed subscribe and an unreachable server', async () => {
    const loggedOut = start();
    loggedOut.network = () => Promise.resolve(new Response('{"error":{"code":"unauthenticated"}}', { status: 401 }));
    await fire(loggedOut, 'pushsubscriptionchange', { newSubscription: pushSubscription('https://push.example/n') });

    const denied = start(); // subscribeResult is null: subscribe() rejects
    await fire(denied, 'pushsubscriptionchange', {
      oldSubscription: pushSubscription('https://push.example/old', new ArrayBuffer(8)),
      newSubscription: null,
    });
    expect(denied.fetched).toEqual([]);

    const offline = start();
    offline.network = () => Promise.reject(new TypeError('Failed to fetch'));
    await fire(offline, 'pushsubscriptionchange', { newSubscription: pushSubscription('https://push.example/n') });
    expect(offline.fetched).toHaveLength(1);
  });
});
