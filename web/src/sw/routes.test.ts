// @vitest-environment node
import { describe, expect, it } from 'vitest';

import { NAVIGATION_TIMEOUT_MS, route, type Strategy } from './routes';

const ORIGIN = 'https://isshoni.test';

describe('route (05 §16.2)', () => {
  it.each<[string, string, string, Strategy]>([
    // Not handled: other methods, other origins, the API, the WebSocket, health, downloads, the install script.
    ['/r/lounge', 'navigate', 'POST', 'network'],
    ['/assets/index-abc.js', 'cors', 'HEAD', 'network'],
    ['/api/v1/auth/login', 'cors', 'POST', 'network'],
    ['https://push.example/send', 'cors', 'GET', 'network'],
    ['https://other.example/assets/index-abc.js', 'no-cors', 'GET', 'network'],
    ['http://isshoni.test/assets/index-abc.js', 'no-cors', 'GET', 'network'],
    ['https://isshoni.test:8443/r/lounge', 'navigate', 'GET', 'network'],
    ['/api', 'navigate', 'GET', 'network'],
    ['/api/v1/info', 'cors', 'GET', 'network'],
    ['/api/v1/me', 'navigate', 'GET', 'network'],
    ['/ws', 'websocket', 'GET', 'network'],
    ['/ws', 'navigate', 'GET', 'network'],
    ['/healthz', 'navigate', 'GET', 'network'],
    ['/healthz', 'cors', 'GET', 'network'],
    ['/readyz', 'navigate', 'GET', 'network'],
    ['/download', 'navigate', 'GET', 'network'],
    ['/download/isshoni_linux_amd64.tar.gz', 'navigate', 'GET', 'network'],
    ['/downloads', 'cors', 'GET', 'network'],
    ['/install', 'navigate', 'GET', 'network'],
    ['/install.sh', 'navigate', 'GET', 'network'],
    ['/install.sh', 'cors', 'GET', 'network'],

    // Navigations to SPA routes: network first, then the cached shell.
    ['/', 'navigate', 'GET', 'navigate'],
    ['/r/lounge', 'navigate', 'GET', 'navigate'],
    ['/r/lounge?focus=s_q7m2x9c4v8b1n5k3', 'navigate', 'GET', 'navigate'],
    ['/login?next=%2Fr%2Flounge', 'navigate', 'GET', 'navigate'],
    ['/invite', 'navigate', 'GET', 'navigate'],
    ['/admin/users', 'navigate', 'GET', 'navigate'],
    ['/account/notifications', 'navigate', 'GET', 'navigate'],
    ['/apiary', 'navigate', 'GET', 'navigate'],
    ['/wsx', 'navigate', 'GET', 'navigate'],
    ['/no/such/page', 'navigate', 'GET', 'navigate'],
    ['https://isshoni.test/about', 'navigate', 'GET', 'navigate'],

    // A file opened as a page is a file: never the shell.
    ['/licenses.txt', 'navigate', 'GET', 'network'],
    ['/version.json', 'navigate', 'GET', 'network'],
    ['/index.html', 'navigate', 'GET', 'network'],
    ['/assets/index-abc.js', 'navigate', 'GET', 'network'],
    ['/manifest.webmanifest', 'navigate', 'GET', 'network'],
    ['/icons/icon.svg', 'navigate', 'GET', 'network'],

    // Hashed assets and the boot check: cache first.
    ['/assets/index-abc123.js', 'cors', 'GET', 'cache-first'],
    ['/assets/index-abc123.css', 'no-cors', 'GET', 'cache-first'],
    ['/assets/auth-9f8e7d.js', 'cors', 'GET', 'cache-first'],
    ['/boot-check.js', 'no-cors', 'GET', 'cache-first'],

    // Icons and the manifest: stale while revalidate.
    ['/icons/icon-192.png', 'no-cors', 'GET', 'stale-while-revalidate'],
    ['/icons/apple-touch-icon.png', 'no-cors', 'GET', 'stale-while-revalidate'],
    ['/manifest.webmanifest', 'cors', 'GET', 'stale-while-revalidate'],

    // Everything else is left to the network: the worker itself, the version file, unknown files.
    ['/sw.js', 'same-origin', 'GET', 'network'],
    ['/version.json', 'cors', 'GET', 'network'],
    ['/licenses.txt', 'cors', 'GET', 'network'],
    ['/robots.txt', 'no-cors', 'GET', 'network'],
    ['/boot-check.js.map', 'cors', 'GET', 'network'],
    ['/assets', 'cors', 'GET', 'network'],
    ['/r/lounge', 'cors', 'GET', 'network'],
  ])('%s (%s, %s) → %s', (url, mode, method, want) => {
    expect(route(url, mode, method, ORIGIN)).toBe(want);
  });

  it('takes a URL object as well as a string', () => {
    expect(route(new URL('/r/lounge', ORIGIN), 'navigate', 'GET', ORIGIN)).toBe('navigate');
    expect(route(new URL('https://other.example/'), 'navigate', 'GET', ORIGIN)).toBe('network');
  });

  it('leaves a URL it cannot parse to the network', () => {
    expect(route('http://[bad', 'navigate', 'GET', ORIGIN)).toBe('network');
  });

  it('gives a navigation 4 s before the cached shell answers (05 §18)', () => {
    expect(NAVIGATION_TIMEOUT_MS).toBe(4000);
  });
});
