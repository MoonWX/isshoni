// The app-shell cache (05 §16.2): one cache per shell version, filled at install with the shell list, and the three
// fetch strategies that routes.ts chooses between. Everything the worker touches comes in through ShellConfig, so
// the tests run it against a fake cache and a fake network.
//
// What ends up in the cache: the shell (`/`, the entry chunk with its static imports and CSS, /boot-check.js, the
// manifest, one icon) and, on first use, lazy route chunks and the other icons. Never an API response and nothing
// user-specific, so logging out needs no cleanup (05 §20).
import { CACHE_PREFIX, shellCacheName } from './contract';
import { NAVIGATION_TIMEOUT_MS, route } from './routes';

export { CACHE_PREFIX, shellCacheName };

export interface ShellConfig {
  /** `__SHELL_VERSION__`: the first 12 hex digits of SHA-256 over the shell files (build/shell.ts). */
  readonly version: string;
  /** `__SHELL__`: the URL paths to precache, `/` first. */
  readonly urls: readonly string[];
  /** The worker's own origin. */
  readonly origin: string;
  readonly caches: CacheStorage;
  /** The network (the worker's own fetch). */
  readonly fetch: (request: Request) => Promise<Response>;
  /** Default: NAVIGATION_TIMEOUT_MS. */
  readonly navigationTimeoutMs?: number;
}

/** Keeps the worker alive until a background task settles (ExtendableEvent.waitUntil). */
export type WaitUntil = (task: Promise<unknown>) => void;

// The same URL is the same file whatever the request headers were (the server only varies the encoding).
const MATCH: CacheQueryOptions = { ignoreVary: true };

const noop = (): void => undefined;

function openShellCache(cfg: ShellConfig): Promise<Cache> {
  return cfg.caches.open(shellCacheName(cfg.version));
}

/**
 * The request that precaches one shell URL. Hashed files under /assets/ never change, so the HTTP cache may answer
 * (the page has usually just loaded them). The others (`/`, /boot-check.js, the manifest, the icon) keep their names
 * across builds: `no-cache` makes the browser revalidate them, so a new worker never stores an old copy.
 */
export function shellRequest(url: string, origin: string): Request {
  const absolute = new URL(url, origin).href;
  return url.startsWith('/assets/') ? new Request(absolute) : new Request(absolute, { cache: 'no-cache' });
}

/**
 * install: fills this version's cache with the whole shell. It rejects when one file can't be fetched (addAll is
 * all-or-nothing), so the install fails and the previous worker, with its complete cache, stays in charge.
 */
export async function precache(cfg: ShellConfig): Promise<void> {
  const cache = await openShellCache(cfg);
  await cache.addAll(cfg.urls.map((url) => shellRequest(url, cfg.origin)));
}

/** activate: deletes the shell caches of other versions and returns their names. Other caches are not ours. */
export async function dropOldCaches(cfg: ShellConfig): Promise<string[]> {
  const keep = shellCacheName(cfg.version);
  const old = (await cfg.caches.keys()).filter((name) => name.startsWith(CACHE_PREFIX) && name !== keep);
  await Promise.all(old.map((name) => cfg.caches.delete(name)));
  return old;
}

/**
 * fetch: the response for a request the worker handles, or null when it doesn't (routes.ts says `network`); the
 * caller then must not call respondWith. Decided synchronously, as respondWith requires.
 */
export function handleFetch(cfg: ShellConfig, request: Request, waitUntil: WaitUntil): Promise<Response> | null {
  switch (route(request.url, request.mode, request.method, cfg.origin)) {
    case 'network':
      return null;
    case 'navigate':
      return navigate(cfg, request);
    case 'cache-first':
      return cacheFirst(cfg, request, waitUntil);
    case 'stale-while-revalidate':
      return staleWhileRevalidate(cfg, request, waitUntil);
  }
}

/** Only complete same-origin answers are stored: no errors, no partial content, no opaque responses. */
function isCacheable(response: Response): boolean {
  return response.status === 200 && (response.type === 'basic' || response.type === 'default');
}

/** A proxy in front of a server that is down or restarting: the server itself didn't answer. */
function isGatewayError(response: Response): boolean {
  return response.status === 502 || response.status === 503 || response.status === 504;
}

/** Stores a copy; a full disk or a blocked cache only costs the next request a network round trip. */
function store(cache: Cache, request: Request, response: Response): Promise<void> {
  return cache.put(request, response).catch(noop);
}

/**
 * A page load: the network first, because the server always serves the SPA that matches it. The cached `/` answers
 * instead when the network fails, when it hasn't answered after the timeout, or when a proxy answers for a server
 * that is down (502, 503, 504); the SPA then starts from the cache and shows its Offline screen, which keeps
 * retrying. Without a cached shell the network's answer (or its failure) stands.
 */
async function navigate(cfg: ShellConfig, request: Request): Promise<Response> {
  // Promise.race below subscribes to the network's answer for good, so a failure that arrives after the cached
  // shell has answered is not an unhandled rejection.
  const network = cfg.fetch(request);
  const cachedShell = async (): Promise<Response | undefined> =>
    (await openShellCache(cfg)).match(new URL('/', cfg.origin).href, MATCH);

  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<null>((resolve) => {
    timer = setTimeout(() => {
      resolve(null);
    }, cfg.navigationTimeoutMs ?? NAVIGATION_TIMEOUT_MS);
  });
  let response: Response | null;
  try {
    response = await Promise.race([network, timeout]);
  } catch (err) {
    const cached = await cachedShell();
    if (cached) return cached;
    throw err;
  } finally {
    clearTimeout(timer);
  }
  if (response && !isGatewayError(response)) return response;
  return (await cachedShell()) ?? response ?? network;
}

/** Hashed assets and /boot-check.js: the cache, else the network, whose answer is cached for the next time. */
async function cacheFirst(cfg: ShellConfig, request: Request, waitUntil: WaitUntil): Promise<Response> {
  const cache = await openShellCache(cfg);
  const cached = await cache.match(request, MATCH);
  if (cached) return cached;
  const response = await cfg.fetch(request);
  if (isCacheable(response)) waitUntil(store(cache, request, response.clone()));
  return response;
}

/** Icons and the manifest: the cached copy at once, refreshed in the background; the network when there is none. */
async function staleWhileRevalidate(cfg: ShellConfig, request: Request, waitUntil: WaitUntil): Promise<Response> {
  const cache = await openShellCache(cfg);
  const cached = await cache.match(request, MATCH);
  const fresh = cfg.fetch(request).then(async (response) => {
    if (isCacheable(response)) await store(cache, request, response.clone());
    return response;
  });
  if (!cached) return fresh;
  waitUntil(fresh.catch(noop));
  return cached;
}
