// @vitest-environment node
import { afterEach, describe, expect, it, vi } from 'vitest';

import { NAVIGATION_TIMEOUT_MS } from './routes';
import {
  CACHE_PREFIX,
  dropOldCaches,
  handleFetch,
  precache,
  shellCacheName,
  shellRequest,
  type ShellConfig,
} from './shell';
import { fakeRequest, FakeCacheStorage, ORIGIN } from './testing/fakes';

const VERSION = '0123456789ab';
const SHELL = [
  '/',
  '/assets/index-abc123.css',
  '/assets/index-def456.js',
  '/boot-check.js',
  '/icons/icon-192.png',
  '/manifest.webmanifest',
];

function setup(network: (request: Request) => Promise<Response> = () => Promise.resolve(new Response('network'))) {
  const storage = new FakeCacheStorage();
  const fetch = vi.fn(network);
  const tasks: Promise<unknown>[] = [];
  const cfg: ShellConfig = { version: VERSION, urls: SHELL, origin: ORIGIN, caches: storage.asCacheStorage(), fetch };
  const waitUntil = (task: Promise<unknown>): void => {
    tasks.push(task);
  };
  const cache = () => storage.get(shellCacheName(VERSION));
  return { storage, fetch, tasks, cfg, waitUntil, cache };
}

/** The handled response of a request; fails the test when the worker would not handle it. */
function respond(s: ReturnType<typeof setup>, request: Request): Promise<Response> {
  const response = handleFetch(s.cfg, request, s.waitUntil);
  if (!response) throw new Error(`${request.url} was not handled`);
  return response;
}

afterEach(() => {
  vi.useRealTimers();
});

describe('the versioned shell cache (05 §16.2)', () => {
  it('names the cache after the shell version', () => {
    expect(CACHE_PREFIX).toBe('isshoni-shell-');
    expect(shellCacheName(VERSION)).toBe('isshoni-shell-0123456789ab');
  });

  it('install precaches exactly the shell list into this version’s cache', async () => {
    const s = setup();
    await precache(s.cfg);
    expect([...s.storage.caches.keys()]).toEqual(['isshoni-shell-0123456789ab']);
    expect(s.cache().added.map((r) => r.url)).toEqual(SHELL.map((u) => ORIGIN + u));
    expect(s.fetch).not.toHaveBeenCalled(); // addAll fetches by itself
  });

  it('revalidates the files that keep their names; hashed assets may come from the HTTP cache', () => {
    expect(shellRequest('/', ORIGIN).cache).toBe('no-cache');
    expect(shellRequest('/boot-check.js', ORIGIN).cache).toBe('no-cache');
    expect(shellRequest('/manifest.webmanifest', ORIGIN).cache).toBe('no-cache');
    expect(shellRequest('/icons/icon-192.png', ORIGIN).cache).toBe('no-cache');
    expect(shellRequest('/assets/index-def456.js', ORIGIN).cache).toBe('default');
    expect(shellRequest('/assets/index-def456.js', ORIGIN).url).toBe(`${ORIGIN}/assets/index-def456.js`);
  });

  it('install fails when a shell file cannot be cached, so the previous worker stays', async () => {
    const s = setup();
    await s.cfg.caches.open(shellCacheName(VERSION));
    s.cache().failWrites = new TypeError('Failed to fetch');
    await expect(precache(s.cfg)).rejects.toThrow('Failed to fetch');
  });

  it('activate deletes the shell caches of other versions and nothing else', async () => {
    const s = setup();
    await precache(s.cfg);
    await s.cfg.caches.open('isshoni-shell-aaaaaaaaaaaa');
    await s.cfg.caches.open('isshoni-shell-bbbbbbbbbbbb');
    await s.cfg.caches.open('someone-elses-cache');
    await expect(dropOldCaches(s.cfg)).resolves.toEqual(['isshoni-shell-aaaaaaaaaaaa', 'isshoni-shell-bbbbbbbbbbbb']);
    expect([...s.storage.caches.keys()]).toEqual(['isshoni-shell-0123456789ab', 'someone-elses-cache']);
    await expect(dropOldCaches(s.cfg)).resolves.toEqual([]);
  });
});

describe('requests the worker leaves alone', () => {
  it.each([
    ['/api/v1/me', { mode: 'cors' }],
    ['/api/v1/auth/login', { mode: 'cors', method: 'POST' }],
    ['/ws', { mode: 'websocket' }],
    ['/version.json', { mode: 'cors' }],
    ['/sw.js', { mode: 'same-origin' }],
    ['/licenses.txt', { mode: 'navigate' }],
    ['/download', { mode: 'navigate' }],
    ['https://other.example/assets/x.js', { mode: 'no-cors' }],
  ])('%s %o is not handled, fetched or cached', async (path, init) => {
    const s = setup();
    await precache(s.cfg);
    expect(handleFetch(s.cfg, fakeRequest(path, init), s.waitUntil)).toBeNull();
    expect(s.fetch).not.toHaveBeenCalled();
    expect(s.tasks).toEqual([]);
    expect(s.cache().entries.size).toBe(SHELL.length);
  });
});

describe('navigations: network first, then the cached shell', () => {
  const nav = (path = '/r/lounge') => fakeRequest(path, { mode: 'navigate' });

  it('answers from the network when it answers', async () => {
    const s = setup(() => Promise.resolve(new Response('fresh page')));
    await precache(s.cfg);
    const request = nav();
    await expect((await respond(s, request)).text()).resolves.toBe('fresh page');
    expect(s.fetch).toHaveBeenCalledExactlyOnceWith(request);
  });

  it('passes a 404 through: the server answers /setup that way once an admin exists', async () => {
    const s = setup(() => Promise.resolve(new Response('index.html', { status: 404 })));
    await precache(s.cfg);
    expect((await respond(s, nav('/setup'))).status).toBe(404);
  });

  it('answers with the cached / when the network fails, whatever the route', async () => {
    const s = setup(() => Promise.reject(new TypeError('Failed to fetch')));
    await precache(s.cfg);
    await expect((await respond(s, nav('/r/lounge?focus=s_1'))).text()).resolves.toBe('precached /');
    await expect((await respond(s, nav('/admin/users'))).text()).resolves.toBe('precached /');
  });

  it('fails like the network when there is no cached shell', async () => {
    const s = setup(() => Promise.reject(new TypeError('Failed to fetch')));
    await expect(respond(s, nav())).rejects.toThrow('Failed to fetch');
  });

  it('answers with the cached / after 4 s without an answer, and a late failure goes unnoticed', async () => {
    vi.useFakeTimers();
    let fail: (err: Error) => void = () => undefined;
    const s = setup(
      () =>
        new Promise<Response>((_resolve, reject) => {
          fail = reject;
        }),
    );
    await precache(s.cfg);
    let answer: string | undefined;
    void respond(s, nav())
      .then((r) => r.text())
      .then((text) => {
        answer = text;
      });
    await vi.advanceTimersByTimeAsync(NAVIGATION_TIMEOUT_MS - 1);
    expect(answer).toBeUndefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(answer).toBe('precached /');
    fail(new TypeError('Failed to fetch'));
    await vi.advanceTimersByTimeAsync(0); // an unhandled rejection would fail the run
  });

  it('keeps waiting for the network after the timeout when there is no cached shell', async () => {
    vi.useFakeTimers();
    let answerNetwork: (r: Response) => void = () => undefined;
    const s = setup(
      () =>
        new Promise<Response>((resolve) => {
          answerNetwork = resolve;
        }),
    );
    let answer: string | undefined;
    void respond(s, nav())
      .then((r) => r.text())
      .then((text) => {
        answer = text;
      });
    await vi.advanceTimersByTimeAsync(NAVIGATION_TIMEOUT_MS * 3);
    expect(answer).toBeUndefined();
    answerNetwork(new Response('slow page'));
    await vi.advanceTimersByTimeAsync(0);
    expect(answer).toBe('slow page');
  });

  it('takes its timeout from the config', async () => {
    vi.useFakeTimers();
    const s = setup(() => new Promise<Response>(() => undefined));
    await precache(s.cfg);
    let answered = false;
    void handleFetch({ ...s.cfg, navigationTimeoutMs: 50 }, nav(), s.waitUntil)?.then(() => {
      answered = true;
    });
    await vi.advanceTimersByTimeAsync(50);
    expect(answered).toBe(true);
  });

  it.each([502, 503, 504])(
    'answers with the cached / when a proxy says %i for a server that is down',
    async (status) => {
      const s = setup(() => Promise.resolve(new Response('Bad Gateway', { status })));
      await precache(s.cfg);
      await expect((await respond(s, nav())).text()).resolves.toBe('precached /');
    },
  );

  it('passes a 503 through when there is no cached shell, and a 500 always', async () => {
    const down = setup(() => Promise.resolve(new Response('Service Unavailable', { status: 503 })));
    expect((await respond(down, nav())).status).toBe(503);
    const broken = setup(() => Promise.resolve(new Response('Internal Server Error', { status: 500 })));
    await precache(broken.cfg);
    expect((await respond(broken, nav())).status).toBe(500);
  });
});

describe('hashed assets and /boot-check.js: cache first', () => {
  it('answers a precached file without the network', async () => {
    const s = setup();
    await precache(s.cfg);
    await expect((await respond(s, fakeRequest('/assets/index-def456.js'))).text()).resolves.toBe(
      'precached /assets/index-def456.js',
    );
    await expect((await respond(s, fakeRequest('/boot-check.js'))).text()).resolves.toBe('precached /boot-check.js');
    expect(s.fetch).not.toHaveBeenCalled();
  });

  it('fetches a lazy chunk once and caches it on first use', async () => {
    const s = setup(() => Promise.resolve(new Response('lazy chunk')));
    await precache(s.cfg);
    const request = fakeRequest('/assets/admin-0a1b2c.js', { mode: 'cors' });
    await expect((await respond(s, request)).text()).resolves.toBe('lazy chunk');
    await Promise.all(s.tasks);
    expect(s.tasks).toHaveLength(1);
    await expect((await respond(s, request)).text()).resolves.toBe('lazy chunk');
    expect(s.fetch).toHaveBeenCalledOnce();
  });

  it.each([
    ['a 404 (a chunk of an older build)', new Response('not found', { status: 404 })],
    ['partial content', new Response('part', { status: 206 })],
    ['an opaque response', Object.defineProperty(new Response('?'), 'type', { value: 'opaque' })],
    ['a network error response', Response.error()],
  ])('does not cache %s', async (_name, response) => {
    const s = setup(() => Promise.resolve(response));
    await precache(s.cfg);
    await respond(s, fakeRequest('/assets/old-999999.js'));
    await Promise.all(s.tasks);
    expect(s.cache().entries.has(`${ORIGIN}/assets/old-999999.js`)).toBe(false);
  });

  it('still answers when the cache cannot store the file', async () => {
    const s = setup(() => Promise.resolve(new Response('lazy chunk')));
    await precache(s.cfg);
    s.cache().failWrites = new DOMException('full', 'QuotaExceededError');
    await expect((await respond(s, fakeRequest('/assets/admin-0a1b2c.js'))).text()).resolves.toBe('lazy chunk');
    await expect(Promise.all(s.tasks)).resolves.toBeDefined();
  });

  it('fails like the network when the file is neither cached nor reachable', async () => {
    const s = setup(() => Promise.reject(new TypeError('Failed to fetch')));
    await expect(respond(s, fakeRequest('/assets/admin-0a1b2c.js'))).rejects.toThrow('Failed to fetch');
  });
});

describe('icons and the manifest: stale while revalidate', () => {
  it('answers from the cache at once and refreshes it in the background', async () => {
    const s = setup(() => Promise.resolve(new Response('new manifest')));
    await precache(s.cfg);
    const request = fakeRequest('/manifest.webmanifest', { mode: 'cors' });
    await expect((await respond(s, request)).text()).resolves.toBe('precached /manifest.webmanifest');
    expect(s.tasks).toHaveLength(1);
    await Promise.all(s.tasks);
    await expect((await respond(s, request)).text()).resolves.toBe('new manifest');
  });

  it('waits for the network when nothing is cached, and caches the answer', async () => {
    const s = setup(() => Promise.resolve(new Response('icon bytes')));
    await precache(s.cfg);
    const request = fakeRequest('/icons/icon-512.png');
    await expect((await respond(s, request)).text()).resolves.toBe('icon bytes');
    expect(s.cache().entries.has(`${ORIGIN}/icons/icon-512.png`)).toBe(true);
  });

  it('keeps the cached copy when the refresh fails or is not a 200', async () => {
    const offline = setup(() => Promise.reject(new TypeError('Failed to fetch')));
    await precache(offline.cfg);
    const request = fakeRequest('/icons/icon-192.png');
    await expect((await respond(offline, request)).text()).resolves.toBe('precached /icons/icon-192.png');
    await expect(Promise.all(offline.tasks)).resolves.toBeDefined();

    const gone = setup(() => Promise.resolve(new Response('not found', { status: 404 })));
    await precache(gone.cfg);
    await respond(gone, request);
    await Promise.all(gone.tasks);
    await expect((await respond(gone, request)).text()).resolves.toBe('precached /icons/icon-192.png');
  });

  it('fails like the network when the file is neither cached nor reachable', async () => {
    const s = setup(() => Promise.reject(new TypeError('Failed to fetch')));
    await expect(respond(s, fakeRequest('/icons/icon-512.png'))).rejects.toThrow('Failed to fetch');
  });
});
