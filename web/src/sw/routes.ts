// The service worker's fetch strategy (05 §16.2): a pure function from a request's URL, mode and method to what the
// worker does with it. shell.ts carries the strategies out; this file only decides.

/**
 * What the worker does with a request:
 * - `network`: not handled. The worker doesn't call respondWith, so the browser fetches as if there were no worker.
 * - `navigate`: network first; after NAVIGATION_TIMEOUT_MS, or when the network fails, the cached `/`.
 * - `cache-first`: the cache, else the network (and the answer is cached).
 * - `stale-while-revalidate`: the cache at once, refreshed from the network in the background.
 */
export type Strategy = 'network' | 'navigate' | 'cache-first' | 'stale-while-revalidate';

/** How long a navigation waits for the network before the cached shell answers (05 §18). */
export const NAVIGATION_TIMEOUT_MS = 4000;

/**
 * Never handled, whatever the request mode: the API and the WebSocket (nothing user-specific is ever cached, 05 §20),
 * the health endpoints, and the server's downloads and install script (04).
 */
function isNetworkOnly(path: string): boolean {
  return (
    path === '/api' ||
    path.startsWith('/api/') ||
    path === '/ws' ||
    path === '/healthz' ||
    path === '/readyz' ||
    path.startsWith('/download') ||
    path.startsWith('/install')
  );
}

/**
 * A path whose last segment has a dot is a file: no SPA route has one, and the server never answers one with the
 * SPA (04 §9.5).
 */
function isFilePath(path: string): boolean {
  return path.slice(path.lastIndexOf('/') + 1).includes('.');
}

/**
 * The strategy for one request (05 §16.2).
 *
 * | Request | Strategy |
 * |---|---|
 * | not GET, another origin, `/api/*`, `/ws`, `/healthz`, `/readyz`, `/download*`, `/install*` | `network` |
 * | a navigation to an SPA route (no dot in the last segment) | `navigate` |
 * | `/assets/*` (hashed names), `/boot-check.js` | `cache-first` |
 * | `/icons/*`, `/manifest.webmanifest` | `stale-while-revalidate` |
 * | anything else (`/sw.js`, `/version.json`, `/licenses.txt`, a navigation to a file, …) | `network` |
 *
 * @param url the request's URL (absolute, or relative to origin)
 * @param mode Request.mode; `navigate` for a page load
 * @param method Request.method
 * @param origin the worker's own origin
 */
export function route(url: string | URL, mode: string, method: string, origin: string): Strategy {
  if (method !== 'GET') return 'network';
  let target: URL;
  try {
    target = new URL(url, origin);
  } catch {
    return 'network';
  }
  if (target.origin !== origin) return 'network';
  const path = target.pathname;
  if (isNetworkOnly(path)) return 'network';
  if (mode === 'navigate') {
    // A file opened as a page (/licenses.txt from the About page) is never answered with the cached shell.
    return isFilePath(path) ? 'network' : 'navigate';
  }
  if (path.startsWith('/assets/') || path === '/boot-check.js') return 'cache-first';
  if (path.startsWith('/icons/') || path === '/manifest.webmanifest') return 'stale-while-revalidate';
  return 'network';
}
