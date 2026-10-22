// The boot sequence (05 §4) against MSW: one test per branch (NeedsHttps, Unsupported, Offline, NotSetUp, Fatal)
// and the way into the app.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createMemoryRouter, useLocation, type RouteObject } from 'react-router';

import type { Platform } from '../platform/types';
import type { Info } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { installFakeRTC } from '../test/FakeRTCPeerConnection';
import { apiError, apiPath, infoFixture, meFixture, server, signedIn } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { desktopStartPath, fragmentTokenKey, loadInfo, stashFragmentToken, startApp, type StartedApp } from './boot';
import { createAppRoutes } from './router';
import { CHANNEL_NAME } from './session';
import { useApp } from './context';
import { useInfo } from './info';
import { createDesktopTestPlatform } from './testing/desktop';

/** Calls of the router's preloadRoute, which still does its work: what boot asks for when nothing else is given. */
const preloadSpy = vi.hoisted(() => vi.fn<(pathname: string) => void>());
/** The RouteTarget of each of those calls (M2). */
const preloadTargets = vi.hoisted((): (string | undefined)[] => []);
vi.mock('./router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./router')>();
  return {
    ...actual,
    preloadRoute: (...args: Parameters<typeof actual.preloadRoute>) => {
      preloadSpy(args[0]);
      preloadTargets.push(args[2]);
      actual.preloadRoute(...args);
    },
  };
});

/** A tiny app for boot tests: shows the path and the seeded server name. */
function AppReady() {
  const { queryClient } = useApp();
  const info = useInfo();
  return (
    <main>
      <h1>app ready</h1>
      <p>server: {info.data?.server.name ?? '?'}</p>
      <p>cached: {queryClient.getQueryData<Info>(queryKeys.info)?.server.name}</p>
    </main>
  );
}
const testRoutes = (): RouteObject[] => [{ path: '*', Component: AppReady }];

interface MountOptions {
  path?: string;
  hash?: string;
  routes?: () => RouteObject[];
  platform?: Platform;
  preload?: (pathname: string) => void;
}

let started: StartedApp | undefined;
let uninstallRTC: (() => void) | undefined;

function mount({
  path = '/',
  hash = '',
  routes = testRoutes,
  platform = createTestPlatform(),
  preload,
}: MountOptions = {}) {
  const container = document.createElement('div');
  document.body.append(container);
  const history = { state: null, replaceState: vi.fn() };
  act(() => {
    started = startApp(container, {
      platform,
      location: { pathname: path, search: '', hash },
      history,
      retryDelaysMs: [0, 0, 0],
      offlineRetryMs: 60_000,
      createRouter: (r) => createMemoryRouter(r, { initialEntries: [path] }),
      routes,
      ...(preload ? { preload } : {}),
    });
  });
  return { container, history, platform, started: started as StartedApp };
}

beforeEach(() => {
  uninstallRTC = installFakeRTC();
  preloadSpy.mockClear();
  preloadTargets.length = 0;
});

afterEach(() => {
  act(() => {
    started?.stop();
  });
  started = undefined;
  uninstallRTC?.();
  document.body.innerHTML = '';
});

describe('boot branches (05 §4)', () => {
  it('NeedsHttps: not a secure context, before any request', () => {
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false });
    try {
      const platform = createTestPlatform();
      const fetchSpy = vi.spyOn(platform, 'apiFetch');
      const { started: app } = mount({ platform });
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('isshoni needs HTTPS');
      expect(screen.getByText(/Ask your admin to check the TLS setup/)).toBeInTheDocument();
      expect(app.screen).toEqual({ kind: 'needsHttps' });
      expect(fetchSpy).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
    }
  });

  it('Unsupported: no WebRTC, before any request', () => {
    uninstallRTC?.();
    uninstallRTC = undefined;
    const platform = createTestPlatform();
    const fetchSpy = vi.spyOn(platform, 'apiFetch');
    const { started: app } = mount({ platform });
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent("This browser can't run isshoni");
    expect(app.screen).toEqual({ kind: 'unsupported' });
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('Offline: /info unreachable after 3 retries; "Try now" and the online event get back in', async () => {
    let calls = 0;
    let up = false;
    server.use(
      http.get(apiPath('/api/v1/info'), () => {
        calls++;
        return up
          ? HttpResponse.json(infoFixture({ server: { name: 'Back', version: '1', publicUrl: '' } }))
          : HttpResponse.error();
      }),
    );
    mount();
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
    expect(calls).toBe(4); // the first try and 3 retries

    // "Try now" while still down: one more request, still offline.
    await userEvent.click(screen.getByRole('button', { name: 'Try now' }));
    await waitFor(() => {
      expect(calls).toBe(5);
    });
    expect(await screen.findByRole('button', { name: 'Try now' })).toBeEnabled();

    // The network comes back: the online event retries.
    up = true;
    act(() => {
      window.dispatchEvent(new Event('online'));
    });
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(screen.getByText('server: Back')).toBeInTheDocument();
    expect(calls).toBe(6);
  });

  it('Offline: also retries by itself every offlineRetryMs', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let up = false;
      server.use(
        http.get(apiPath('/api/v1/info'), () => (up ? HttpResponse.json(infoFixture()) : HttpResponse.error())),
      );
      mount();
      expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
      up = true;
      await act(() => vi.advanceTimersByTimeAsync(60_000));
      expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('Offline: a server that keeps failing with 5xx (a proxy in front of a stopped server)', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => new HttpResponse('Bad Gateway', { status: 502 })));
    mount();
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
  });

  it('NotSetUp: setupRequired on any path but /setup, with the setup-url commands', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture({ setupRequired: true }))));
    mount({ path: '/r/lounge' });
    expect(await screen.findByRole('heading', { name: "This server isn't set up yet" })).toBeInTheDocument();
    expect(screen.getByText('sudo isshoni setup-url')).toBeInTheDocument();
    expect(screen.getByText('docker compose exec isshoni isshoni setup-url')).toBeInTheDocument();
  });

  it('NotSetUp does not apply to /setup itself', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture({ setupRequired: true }))));
    mount({ path: '/setup', hash: '#setup-token' });
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
  });

  it.each([
    ['a 404 envelope (not an isshoni server)', () => apiError(404, { code: 'not_found' }), 'Error: not_found'],
    ['a 403 envelope', () => apiError(403, { code: 'forbidden' }), 'Error: forbidden'],
    ['a plain-text 421 (Host check)', () => new HttpResponse('Misdirected Request', { status: 421 }), 'Error: unknown'],
    ['a 200 that is not Info', () => HttpResponse.json({ hello: 'world' }), 'Error: unknown'],
    [
      'a 200 HTML page',
      () => new HttpResponse('<html></html>', { headers: { 'Content-Type': 'text/html' } }),
      'Error: unknown',
    ],
  ])('Fatal: %s', async (_name, respond, footnote) => {
    server.use(http.get(apiPath('/api/v1/info'), respond));
    const platform = createTestPlatform();
    // Count our requests, not the server's: fetch itself repeats a request once after a 421.
    const fetchSpy = vi.spyOn(platform, 'apiFetch');
    mount({ platform });
    expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByText(footnote)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(fetchSpy).toHaveBeenCalledOnce(); // not retried
  });

  it('Fatal: Reload uses the platform', async () => {
    const reload = vi.fn();
    server.use(http.get(apiPath('/api/v1/info'), () => apiError(404, { code: 'not_found' })));
    mount({ platform: createTestPlatform({ versionActions: () => ({ reload }) }) });
    await userEvent.click(await screen.findByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledOnce();
  });

  it('App: renders the router once /info answers, with info seeded in the query cache', async () => {
    const { started: app } = mount();
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(screen.getByText('server: Test server')).toBeInTheDocument();
    expect(app.services?.queryClient.getQueryData(queryKeys.info)).toEqual(infoFixture());
  });

  it('App: retries a failing /info before giving up (1, 2, 4 s in production)', async () => {
    let calls = 0;
    server.use(
      http.get(apiPath('/api/v1/info'), () => {
        calls++;
        return calls < 3 ? apiError(503, { code: 'server_busy', retryAfter: 1 }) : HttpResponse.json(infoFixture());
      }),
    );
    mount();
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(calls).toBe(3);
  });

  it('App: the real routes start at the path (a guarded route without a session goes to /login)', async () => {
    server.use(signedIn(null));
    const { started: app } = mount({ path: '/account', routes: createAppRoutes });
    // The login page of the auth/ folder (S33), with the server name that boot seeded from /info.
    expect(await screen.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
    expect(app.services).not.toBeNull();
  });
});

describe('the first page’s code (05 §5: every page is in a lazy chunk)', () => {
  /** Boot's requests and its preload, in the order they were made. */
  function recorded(platform: Platform) {
    const order: string[] = [];
    const original = platform.apiFetch.bind(platform);
    vi.spyOn(platform, 'apiFetch').mockImplementation((p, init) => {
      order.push(`fetch ${p}`);
      return original(p, init);
    });
    const preload = vi.fn((pathname: string) => {
      order.push(`preload ${pathname}`);
    });
    return { order, preload };
  }

  it.each(['/', '/r/lounge', '/login'])('is asked for before /info, with the path: %s', async (path) => {
    const platform = createTestPlatform();
    const { order, preload } = recorded(platform);
    mount({ path, platform, preload });
    // Asked for by startApp itself: before the request, not after its answer.
    expect(order).toEqual([`preload ${path}`, 'fetch /api/v1/info']);
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(preload).toHaveBeenCalledOnce();
  });

  it('is asked for once, however often /info has to be retried', async () => {
    let calls = 0;
    server.use(
      http.get(apiPath('/api/v1/info'), () => {
        calls++;
        return calls < 3 ? apiError(503, { code: 'server_busy', retryAfter: 1 }) : HttpResponse.json(infoFixture());
      }),
    );
    const platform = createTestPlatform();
    const { preload } = recorded(platform);
    mount({ path: '/r/lounge', platform, preload });
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(calls).toBe(3);
    expect(preload).toHaveBeenCalledOnce();
  });

  it('is not asked for while the browser says it is offline: a failed import() may not be tried again', async () => {
    const onLine = vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(false);
    try {
      const platform = createTestPlatform();
      const { order, preload } = recorded(platform);
      mount({ path: '/r/lounge', platform, preload });
      // Boot itself goes on: /info decides whether the server can be reached.
      expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
      expect(preload).not.toHaveBeenCalled();
      expect(order[0]).toBe('fetch /api/v1/info');
    } finally {
      onLine.mockRestore();
    }
  });

  it('is not asked for on NeedsHttps: that screen leads to no page', () => {
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false });
    try {
      const preload = vi.fn();
      mount({ path: '/r/lounge', preload });
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('isshoni needs HTTPS');
      expect(preload).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
    }
  });

  it('with the default routes it is preloadRoute of the router; a test’s own routes preload nothing', async () => {
    // A test's own routes (every other test here): the router's folders are not touched.
    mount({ path: '/pending' });
    expect(await screen.findByRole('heading', { name: 'app ready' })).toBeInTheDocument();
    expect(preloadSpy).not.toHaveBeenCalled();
    act(() => {
      started?.stop();
    });
    document.body.innerHTML = '';

    // The defaults, as main.tsx starts the app: /pending is a page of auth/.
    server.use(signedIn(null));
    const container = document.createElement('div');
    document.body.append(container);
    act(() => {
      started = startApp(container, {
        platform: createTestPlatform(),
        location: { pathname: '/pending', search: '', hash: '' },
        history: { state: null, replaceState: vi.fn() },
        retryDelaysMs: [],
        createRouter: (r) => createMemoryRouter(r, { initialEntries: ['/pending'] }),
      });
    });
    expect(preloadSpy).toHaveBeenCalledExactlyOnceWith('/pending');
    expect(await screen.findByRole('heading', { level: 1, name: 'Request sent' })).toBeInTheDocument();
  });
});

describe('step 0: the fragment token', () => {
  it.each([
    ['/setup', 'setup'],
    ['/invite', 'invite'],
    ['/reset', 'reset'],
  ] as const)('%s#token is stashed and the fragment dropped before the first request', async (path, kind) => {
    const order: string[] = [];
    const platform = createTestPlatform();
    const original = platform.apiFetch.bind(platform);
    vi.spyOn(platform, 'apiFetch').mockImplementation((p, init) => {
      order.push(`fetch ${p}`);
      return original(p, init);
    });
    const container = document.createElement('div');
    const history = {
      state: { idx: 0 },
      replaceState: vi.fn((_state: unknown, _unused: string, url: string) => {
        order.push(`replaceState ${url}`);
      }),
    };
    act(() => {
      started = startApp(container, {
        platform,
        location: { pathname: path, search: '?x=1', hash: '#tok%2Fen' },
        history,
        retryDelaysMs: [],
        createRouter: (r) => createMemoryRouter(r, { initialEntries: [path] }),
        routes: testRoutes,
      });
    });
    await waitFor(() => {
      expect(order).toContain('fetch /api/v1/info');
    });
    expect(order[0]).toBe(`replaceState ${path}?x=1`);
    expect(history.replaceState).toHaveBeenCalledWith({ idx: 0 }, '', `${path}?x=1`);
    expect(platform.storage.session.get(fragmentTokenKey(kind))).toBe('tok/en');
  });

  it('leaves other paths and empty fragments alone', () => {
    const session = createTestPlatform().storage.session;
    const history = { state: null, replaceState: vi.fn() };
    expect(stashFragmentToken({ pathname: '/login', search: '', hash: '#abc' }, history, session)).toBeNull();
    expect(stashFragmentToken({ pathname: '/invite', search: '', hash: '' }, history, session)).toBeNull();
    expect(stashFragmentToken({ pathname: '/invite', search: '', hash: '#' }, history, session)).toBeNull();
    expect(stashFragmentToken({ pathname: '/invite/x', search: '', hash: '#abc' }, history, session)).toBeNull();
    expect(history.replaceState).not.toHaveBeenCalled();
    expect(session.get('isshoni.invite')).toBeNull();
  });

  it('keeps a token that is not valid percent-encoding as it came', () => {
    const session = createTestPlatform().storage.session;
    stashFragmentToken(
      { pathname: '/reset', search: '', hash: '#bad%E0%A4%A' },
      { state: null, replaceState: vi.fn() },
      session,
    );
    expect(session.get('isshoni.reset')).toBe('bad%E0%A4%A');
  });
});

describe('loadInfo', () => {
  it('stops at a non-retryable error without waiting', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => apiError(400, { code: 'bad_request' })));
    await expect(loadInfo([60_000])).resolves.toEqual({ kind: 'fatal', code: 'bad_request' });
  });

  it('is offline after the last delay', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.error()));
    await expect(loadInfo([0])).resolves.toEqual({ kind: 'offline' });
  });

  it('can be aborted while waiting', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.error()));
    const ctl = new AbortController();
    const p = loadInfo([60_000], ctl.signal);
    await new Promise((r) => setTimeout(r, 20));
    ctl.abort();
    await expect(p).rejects.toMatchObject({ name: 'AbortError' });
  });
});

describe('M2: boot in the desktop app (docs/m2/03 §7.2)', () => {
  /** Shows where the router is: path, query and fragment. */
  function Where() {
    const { pathname, search, hash } = useLocation();
    return (
      <main>
        <h1>at {pathname + search + hash}</h1>
      </main>
    );
  }
  const whereRoutes = (): RouteObject[] => [{ path: '*', Component: Where }];

  /**
   * Starts the app like main.tsx in the desktop app: a history whose replaceState moves the location that the
   * router then starts at, as createBrowserRouter reads the page's location.
   */
  function mountDesktop({
    path = '/',
    search = '',
    platform = createDesktopTestPlatform().platform,
    routes = whereRoutes,
    preload,
  }: {
    path?: string;
    search?: string;
    platform?: Platform;
    /** 'app': boot's own default (the app's routes for the platform). */
    routes?: (() => RouteObject[]) | 'app';
    preload?: (pathname: string) => void;
  } = {}) {
    const container = document.createElement('div');
    document.body.append(container);
    const urls: string[] = [];
    const history = {
      state: null,
      replaceState: vi.fn((_s: unknown, _u: string, url: string) => {
        urls.push(url);
      }),
    };
    const fetchSpy = vi.spyOn(platform, 'apiFetch');
    act(() => {
      started = startApp(container, {
        platform,
        location: { pathname: path, search, hash: '' },
        history,
        retryDelaysMs: [0, 0, 0],
        offlineRetryMs: 60_000,
        createRouter: (r) => createMemoryRouter(r, { initialEntries: [urls.at(-1) ?? path + search] }),
        ...(routes !== 'app' ? { routes } : {}),
        ...(preload ? { preload } : {}),
      });
    });
    return { urls, fetchSpy, started: started as StartedApp };
  }

  it('desktopStartPath: the desktop app’s own pages stay, every other path starts the wizard', () => {
    expect(desktopStartPath({ pathname: '/' })).toBe('/desktop/setup');
    expect(desktopStartPath({ pathname: '/r/lounge' })).toBe('/desktop/setup');
    expect(desktopStartPath({ pathname: '/login' })).toBe('/desktop/setup');
    expect(desktopStartPath({ pathname: '/desktop' })).toBe('/desktop/setup');
    expect(desktopStartPath({ pathname: '/desktop/setup' })).toBeNull();
    expect(desktopStartPath({ pathname: '/desktop/settings' })).toBeNull();
  });

  it('no server yet: no /info, and the router starts at /desktop/setup', async () => {
    const platform = createDesktopTestPlatform({ server: null }).platform;
    const preload = vi.fn();
    const { urls, fetchSpy } = mountDesktop({ path: '/r/lounge', platform, preload });
    expect(await screen.findByRole('heading', { name: 'at /desktop/setup' })).toBeInTheDocument();
    expect(urls).toEqual(['/desktop/setup']);
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(preload).toHaveBeenCalledExactlyOnceWith('/desktop/setup');
  });

  it('no server yet: a desktop page stays where the app opened it, with its query (?add=)', async () => {
    const platform = createDesktopTestPlatform({ server: null }).platform;
    const { urls, fetchSpy } = mountDesktop({
      path: '/desktop/setup',
      search: '?add=https%3A%2F%2Fwatch.example.com',
      platform,
    });
    expect(
      await screen.findByRole('heading', { name: 'at /desktop/setup?add=https%3A%2F%2Fwatch.example.com' }),
    ).toBeInTheDocument();
    expect(urls).toEqual([]);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('with a server: GET /info through apiFetch, then the app, at the path it opened', async () => {
    const { urls, fetchSpy } = mountDesktop({ path: '/r/lounge' });
    expect(await screen.findByRole('heading', { name: 'at /r/lounge' })).toBeInTheDocument();
    expect(fetchSpy).toHaveBeenCalledWith('/api/v1/info', expect.anything());
    expect(urls).toEqual([]);
  });

  it('has no secure-context check: the app’s pages come from its own asset handler', async () => {
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false });
    try {
      const { started: app } = mountDesktop({ path: '/r/lounge' });
      expect(app.screen).toBeNull();
      expect(await screen.findByRole('heading', { name: 'at /r/lounge' })).toBeInTheDocument();
    } finally {
      Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
    }
  });

  it('still needs WebRTC', () => {
    uninstallRTC?.();
    uninstallRTC = undefined;
    const { started: app } = mountDesktop();
    expect(app.screen).toEqual({ kind: 'unsupported' });
  });

  it('Offline gains "Switch server": the router at /desktop/settings#servers, without /info', async () => {
    let calls = 0;
    server.use(
      http.get(apiPath('/api/v1/info'), () => {
        calls++;
        return HttpResponse.error();
      }),
    );
    const { urls } = mountDesktop({ path: '/r/lounge' });
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
    expect(calls).toBe(4);
    await userEvent.click(screen.getByRole('button', { name: 'Switch server' }));
    expect(await screen.findByRole('heading', { name: 'at /desktop/settings#servers' })).toBeInTheDocument();
    expect(urls).toEqual(['/desktop/settings#servers']);
    // The Offline screen's own retries are over.
    act(() => {
      window.dispatchEvent(new Event('online'));
    });
    await act(() => Promise.resolve());
    expect(calls).toBe(4);
  });

  it('the browser’s Offline screen has no "Switch server"', async () => {
    server.use(http.get(apiPath('/api/v1/info'), () => HttpResponse.error()));
    mount();
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Switch server' })).not.toBeInTheDocument();
  });

  it('the default routes and preload are the desktop table’s: /desktop/settings has its page', async () => {
    const { started: app } = mountDesktop({ path: '/desktop/settings', routes: 'app' });
    // desktop/'s stub (WA42) until WA82 fills it.
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
    expect(app.services?.platform.kind).toBe('desktop');
    expect(preloadSpy).toHaveBeenCalledExactlyOnceWith('/desktop/settings');
    expect(preloadTargets).toEqual(['desktop']);
  });

  it('…and the browser’s default table has none: NotFound', async () => {
    const container = document.createElement('div');
    document.body.append(container);
    act(() => {
      started = startApp(container, {
        platform: createTestPlatform(),
        location: { pathname: '/desktop/settings', search: '', hash: '' },
        history: { state: null, replaceState: vi.fn() },
        retryDelaysMs: [],
        createRouter: (r) => createMemoryRouter(r, { initialEntries: ['/desktop/settings'] }),
      });
    });
    expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
    expect(preloadTargets).toEqual(['browser']);
  });
});

describe('cross-tab logout after boot', () => {
  it('a logout message from another tab clears ["me"]', async () => {
    server.use(signedIn(meFixture()));
    const { started: app } = mount();
    await screen.findByRole('heading', { name: 'app ready' });
    const qc = app.services?.queryClient;
    qc?.setQueryData(queryKeys.me, meFixture());
    const other = new BroadcastChannel(CHANNEL_NAME);
    other.postMessage({ type: 'logout' });
    other.close();
    await waitFor(() => {
      expect(qc?.getQueryData(queryKeys.me)).toBeNull();
    });
  });

  it('stop() unmounts the app', async () => {
    const { container, started: app } = mount();
    await within(container).findByRole('heading', { name: 'app ready' });
    act(() => {
      app.stop();
    });
    started = undefined;
    expect(container).toBeEmptyDOMElement();
  });
});
